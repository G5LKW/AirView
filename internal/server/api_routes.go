package server

// Route look-ups (internal/routes, docs/ROUTES-DESIGN.md §7). Public and
// anonymous callers never trigger an online look-up; online (restricted
// class) answers are shown only to the station's own users.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"airview/internal/model"
	rtsvc "airview/internal/routes"
)

const grpRoutes = "Routes"

func init() {
	routes = append(routes, routeRoutes...)
}

var routeRoutes = []Route{
	{Method: "GET", Path: "/api/routes/{callsign}", Group: grpRoutes, Access: AccessPublic, API: apiRouteDetail, Sample: "/api/routes/BAW123",
		Summary: "Full route view of a callsign: every hypothesis with its sources, fused probability and live score, the shown route (active leg, state, confidence, progress, ETA), the great-circle path and the learned history.",
		Params: []Param{{"icao", "ICAO address of the aircraft: scores the hypotheses against its current position"},
			{"online", "`1`: look the callsign up online (adsbdb) when nothing offline is known (admin only; subject to the daily budget)"}},
		Notes: "Reads the offline data (override, learned, standing data) and the online cache. Answers from online providers (`lic: restricted`) are only shown to the station's own users; public viewers of an aggregator get `hidden` instead."},
	{Method: "GET", Path: "/api/routes/status", Group: grpRoutes, Access: AccessAdmin, API: apiRouteStatus,
		Summary: "Resolver counters, the online budget (used / daily cap / back-off), invalid rows of the overrides file and the attribution lines."},
	{Method: "GET", Path: "/api/routes/overrides", Group: grpRoutes, Access: AccessAdmin, API: apiRouteOverrides,
		Summary: "Every route override (`locked` rows come from `routes.overrides.csv` in the data directory and can only be changed there)."},
	{Method: "PUT", Path: "/api/routes/overrides/{callsign}", Group: grpRoutes, Access: AccessAdmin, API: apiRouteOverridePut, Admin: true, NoAuto: true,
		Summary: "Create or replace the override of a callsign.",
		Body:    "`{\"airports\":\"EGLL-KJFK\",\"mode\":\"fixed|verify|block\",\"note\":\"...\",\"expires\":0}`",
		Returns: "the stored override; `400` when invalid, `409` for a locked (file) row"},
	{Method: "DELETE", Path: "/api/routes/overrides/{callsign}", Group: grpRoutes, Access: AccessAdmin, API: apiRouteOverrideDelete, Admin: true, NoAuto: true,
		Summary: "Delete the override of a callsign.", Returns: "`{\"ok\":true}`; `404` when none, `409` for a locked (file) row"},
	{Method: "GET", Path: "/api/routes/overrides-log", Group: grpRoutes, Access: AccessAdmin, API: apiRouteOverrideLog,
		Summary: "Audit log of override changes, newest first.", Params: []Param{{"limit", "1..1000 (default 200)"}}},
	{Method: "POST", Path: "/api/routes/refresh", Group: grpRoutes, Access: AccessAdmin, API: apiRouteRefresh, Admin: true, NoAuto: true,
		Summary: "Check the reference data (airports, routes, ...) for updates now (conditional GET: unchanged files are not downloaded).",
		Returns: "`{\"started\":true}`; `409` when an import is running"},
	{Method: "GET", Path: "/api/routes/export/{file}", Group: grpRoutes, Access: AccessAdmin, Raw: rawRouteExport, ContentType: "text/csv",
		Sample:  "/api/routes/export/learned.csv",
		Summary: "Exports: `learned.csv` (learned routes with P >= 0.9 seen on >= 3 days, in the VRS standing-data format, released CC0 for submission to the VRS standing data), `overrides.csv` (round-trips with `routes.overrides.csv`), `legs.csv` (observed legs; `from`/`to` Unix s).",
		Params:  []Param{{"from, to", "legs.csv only: time range in Unix s"}}},
}

// routesRestrictedOK reports whether the caller may see restricted-class
// (online provider) answers: everyone in standalone mode (the station's own
// web interface), owners and admins in aggregator mode.
func (s *Server) routesRestrictedOK(r *http.Request) bool {
	if !s.aggregatorMode() {
		return true
	}
	return s.viewerOf(r).Scope.Allows(model.ScopeOwner)
}

func (s *Server) routeSvc() (*rtsvc.Service, error) {
	rt := s.eng.Routes()
	if rt == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "route service not running")
	}
	return rt, nil
}

func routeErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, rtsvc.ErrBadOverride):
		return badRequest("%s", err.Error())
	case errors.Is(err, rtsvc.ErrLocked):
		return errStatus(http.StatusConflict, "%s", err.Error())
	case errors.Is(err, rtsvc.ErrNotFound):
		return notFound("%s", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return err
}

func apiRouteDetail(s *Server, r *http.Request) (any, error) {
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	o := rtsvc.DetailOptions{IncludeRestricted: s.routesRestrictedOK(r), AllowOnline: qStr(r, "online") == "1" && s.isAdmin(r)}
	if v := qStr(r, "icao"); v != "" {
		key, err := parseICAOParam(v)
		if err != nil {
			return nil, err
		}
		if ac, ok := s.eng.Tracker().Aircraft(key); ok {
			o.Aircraft = &ac
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	d, err := rt.Detail(ctx, r.PathValue("callsign"), o)
	if err != nil {
		return nil, routeErr(err)
	}
	return d, nil
}

type routeStatusDTO struct {
	Stats        rtsvc.Stats        `json:"stats"`
	Online       bool               `json:"online"`
	Used         int                `json:"onlineUsed"`  // online look-ups in the current 24-h window
	Daily        int                `json:"onlineDaily"` // daily cap
	BackoffUntil int64              `json:"backoffUntil,omitempty"`
	FileProblems []string           `json:"fileProblems,omitempty"`
	Attribution  []string           `json:"attribution"`
	Config       model.RoutesConfig `json:"config"`
}

func apiRouteStatus(s *Server, r *http.Request) (any, error) {
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	cfg := s.eng.EffectiveConfig().NavData.Routes
	used, daily, until := s.eng.NavData().OnlineBudget()
	out := routeStatusDTO{Stats: rt.Stats(), Online: cfg.Enabled && cfg.Online, Used: used, Daily: daily,
		FileProblems: rt.FileProblems(), Attribution: rt.Attribution(), Config: cfg}
	if !until.IsZero() {
		out.BackoffUntil = until.Unix()
	}
	return out, nil
}

func apiRouteOverrides(s *Server, r *http.Request) (any, error) {
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	return rt.Overrides(), nil
}

// routeAuthor names the person changing an override for the audit log.
func (s *Server) routeAuthor(r *http.Request) string {
	if a := s.actorOf(r); a.Name != "" {
		return a.Name
	}
	return "admin"
}

func apiRouteOverridePut(s *Server, r *http.Request) (any, error) {
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	var in rtsvc.Override
	if err := readJSON(nil, r, &in, 8<<10); err != nil {
		return nil, badRequest("%s", err.Error())
	}
	in.Callsign = r.PathValue("callsign")
	o, err := rt.PutOverride(r.Context(), in, s.routeAuthor(r))
	if err != nil {
		return nil, routeErr(err)
	}
	return o, nil
}

func apiRouteOverrideDelete(s *Server, r *http.Request) (any, error) {
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	if err := rt.DeleteOverride(r.Context(), r.PathValue("callsign"), s.routeAuthor(r)); err != nil {
		return nil, routeErr(err)
	}
	return map[string]any{"ok": true}, nil
}

func apiRouteOverrideLog(s *Server, r *http.Request) (any, error) {
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	n, err := qInt(r, "limit", 200, 1, 1000)
	if err != nil {
		return nil, err
	}
	l, err := rt.OverrideLog(r.Context(), n)
	if err != nil {
		return nil, err
	}
	if l == nil {
		l = []rtsvc.OverrideLog{}
	}
	return l, nil
}

func apiRouteRefresh(s *Server, r *http.Request) (any, error) {
	if err := s.eng.RefreshNavData(); err != nil {
		return nil, navErr(err)
	}
	return map[string]any{"started": true}, nil
}

func rawRouteExport(s *Server, w http.ResponseWriter, r *http.Request) {
	rt := s.eng.Routes()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, "route service not running")
		return
	}
	file := r.PathValue("file")
	switch file {
	case rtsvc.ExportLearned, rtsvc.ExportOverrides, rtsvc.ExportLegs:
	default:
		writeError(w, http.StatusNotFound, "unknown export (learned.csv, overrides.csv, legs.csv)")
		return
	}
	var from, to int64
	fmt.Sscan(qStr(r, "from"), &from)
	fmt.Sscan(qStr(r, "to"), &to)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if file == rtsvc.ExportLearned {
		w.Header().Set("X-License", rtsvc.LearnedLicence)
	}
	w.Header().Set("Content-Disposition", `attachment; filename="airview-routes-`+strings.TrimSuffix(file, ".csv")+`.csv"`)
	if err := rt.Export(r.Context(), w, file, from, to); err != nil {
		s.log.Debug("routes export", "file", file, "err", err)
	}
}
