package server

// Route product surface (docs/ROUTES-DESIGN.md §6.5-6.6, §7): airport boards,
// airport references, route shorts for the list, leg statistics, leg search
// and provider status. Boards and batches are built from the caller's own
// projection of the live picture, so a public viewer of an aggregator never
// sees restricted-class (online provider) routes.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
	rtsvc "airview/internal/routes"
)

func init() {
	routes = append(routes, routeRoutesMore...)
}

var routeRoutesMore = []Route{
	{Method: "GET", Path: "/api/routes/board/{airport}", Group: grpRoutes, Access: AccessViewer, API: apiRouteBoard, Sample: "/api/routes/board/EGLL", NoAuto: true,
		Summary: "Arrivals, departures and recently landed flights of an airport, built from the live picture and the routes resolved for it (not a schedule).",
		Notes:   "Arrivals: route ends here with confidence >= 50 %, sorted by ETA. Departures: route starts here, climbing, within 300 km. Landed: legs this station saw end here in the last 60 minutes (station users only on an aggregator). Public viewers of an aggregator see the public picture (no online-provider routes).",
		Returns: "`{airport, arrivals[], departures[], landed[], generated, note}`; `404` for an unknown airport"},
	{Method: "GET", Path: "/api/routes/airports", Group: grpRoutes, Access: AccessPublic, API: apiRouteAirports, Sample: "/api/routes/airports?codes=EGLL,JFK",
		Summary: "Airport references (ICAO / IATA code, name, position, elevation, size) for client-side route drawing and the IATA/ICAO alias map.",
		Params:  []Param{{"codes", "comma-separated ICAO, IATA or ident codes (at most 200); unknown codes are left out"}}},
	{Method: "GET", Path: "/api/routes/batch", Group: grpRoutes, Access: AccessViewer, API: apiRouteBatch, Sample: "/api/routes/batch?callsigns=BAW123,EZY45",
		Summary: "Route shorts of the aircraft in the caller's live picture with the given callsigns (for lists and filters). Never triggers an online look-up.",
		Params:  []Param{{"callsigns", "comma-separated callsigns (at most 200)"}},
		Returns: "`{\"routes\":{\"BAW123\":{route short},...}}`; callsigns not in the live picture or without a route are left out"},
	{Method: "GET", Path: "/api/routes/stats", Group: grpRoutes, Access: AccessViewer, API: apiRouteStats, Sample: "/api/routes/stats?kind=routes",
		Summary: "Statistics of the legs this station observed: busiest `routes` (observed departure-arrival pairs), `airports` (departures + arrivals), `airlines` (callsign prefix), or `coverage` (legs by the source of the route shown, and the share with both ends observed).",
		Params: []Param{{"kind", "routes | airports | airlines | coverage"},
			{"from, to", "time range of the leg start, Unix s (default: the last 7 days)"},
			{"limit", "1..1000 (default 50)"}},
		Notes: "Only ends observed with confidence >= 50 % count for routes and airports. Online-provider answers are never stored in legs. On an aggregator only owners and admins may read them."},
	{Method: "GET", Path: "/api/routes/legs", Group: grpRoutes, Access: AccessOwner, API: apiRouteLegs, Sample: "/api/routes/legs?limit=20",
		Summary: "Search the observed legs, newest first.",
		Params: []Param{{"from, to", "time range of the leg start, Unix s"}, {"callsign", "exact callsign"}, {"airline", "3-letter ICAO airline prefix"},
			{"dep, arr", "observed departure / arrival airport (as stored, normally ICAO)"}, {"route", "route shown when the flight ended, e.g. `EGLL-KJFK`"},
			{"limit", "1..5000 (default 500)"}}},
	{Method: "GET", Path: "/api/routes/providers", Group: grpRoutes, Access: AccessAdmin, API: apiRouteProviders,
		Summary: "Online route providers: enabled, licence class, budget used today, daily cap, back-off and what is sent."},
}

// liveAircraft returns the caller's projection of the live picture as
// model.Aircraft (public viewers of an aggregator: the public picture).
func (s *Server) liveAircraft(r *http.Request) []model.Aircraft {
	switch v := s.viewSnapshot(r).(type) {
	case model.LiveSnapshot:
		return v.Aircraft
	case *model.LiveSnapshot:
		return v.Aircraft
	default:
		p := s.publicSnapshot()
		out := make([]model.Aircraft, 0, len(p.Aircraft))
		for _, a := range p.Aircraft {
			out = append(out, model.Aircraft{ICAO: a.ICAO, Callsign: a.Callsign, TypeCode: a.TypeCode, Operator: a.Operator,
				Lat: a.Lat, Lon: a.Lon, OnGround: a.OnGround, AltBaro: a.AltBaro, VertRate: a.VertRate, Route: a.Route})
		}
		return out
	}
}

func apiRouteBoard(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, mapOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ap, ok, err := rt.Airport(ctx, r.PathValue("airport"))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("unknown airport %q", r.PathValue("airport"))
	}
	now := time.Now()
	var landed []rtsvc.Leg
	if s.routesRestrictedOK(r) { // the station's own users: its observed legs
		codes := []string{ap.Code}
		if ap.IATA != "" {
			codes = append(codes, ap.IATA)
		}
		if landed, err = rt.LandedAt(ctx, codes, now.Add(-rtsvc.BoardWindow).Unix()); err != nil {
			return nil, err
		}
	}
	return rtsvc.BuildBoard(ap, s.liveAircraft(r), landed, now), nil
}

func apiRouteAirports(s *Server, r *http.Request) (any, error) {
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	codes := strings.Split(qStr(r, "codes"), ",")
	if len(codes) > rtsvc.MaxAirportCodes {
		return nil, badRequest("at most %d codes", rtsvc.MaxAirportCodes)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	return rt.Airports(ctx, codes)
}

// maxBatchCallsigns bounds GET /api/routes/batch.
const maxBatchCallsigns = 200

func apiRouteBatch(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, mapOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	list := strings.Split(qStr(r, "callsigns"), ",")
	if len(list) > maxBatchCallsigns {
		return nil, badRequest("at most %d callsigns", maxBatchCallsigns)
	}
	want := make(map[string]bool, len(list))
	for _, c := range list {
		if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
			want[c] = true
		}
	}
	out := map[string]*model.RouteShort{}
	for _, a := range s.liveAircraft(r) {
		if cs := strings.ToUpper(strings.TrimSpace(a.Callsign)); a.Route != nil && want[cs] {
			out[cs] = a.Route
		}
	}
	return map[string]any{"routes": out}, nil
}

func routeRange(r *http.Request) (from, to int64, err error) {
	if from, err = qInt64(r, "from", 0); err != nil {
		return
	}
	to, err = qInt64(r, "to", 0)
	return
}

func apiRouteStats(s *Server, r *http.Request) (any, error) {
	if !s.routesRestrictedOK(r) {
		return nil, errStatus(http.StatusForbidden, "route statistics are for the station's own users")
	}
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	from, to, err := routeRange(r)
	if err != nil {
		return nil, err
	}
	if from == 0 && to == 0 {
		from = time.Now().Add(-7 * 24 * time.Hour).Unix()
	}
	limit, err := qInt(r, "limit", 50, 1, 1000)
	if err != nil {
		return nil, err
	}
	kind := qStr(r, "kind")
	if kind == "" {
		kind = rtsvc.StatRoutes
	}
	rows, err := rt.LegStats(r.Context(), kind, from, to, limit)
	if err != nil {
		if err == rtsvc.ErrNotFound {
			return nil, badRequest("unknown kind %q (routes, airports, airlines, coverage)", kind)
		}
		return nil, err
	}
	return map[string]any{"kind": kind, "from": from, "to": to, "rows": rows}, nil
}

func apiRouteLegs(s *Server, r *http.Request) (any, error) {
	rt, err := s.routeSvc()
	if err != nil {
		return nil, err
	}
	from, to, err := routeRange(r)
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 500, 1, 5000)
	if err != nil {
		return nil, err
	}
	return rt.Legs(r.Context(), rtsvc.LegQuery{From: from, To: to, Callsign: qStr(r, "callsign"), Airline: qStr(r, "airline"),
		Dep: qStr(r, "dep"), Arr: qStr(r, "arr"), Route: qStr(r, "route"), Limit: limit})
}

type routeProviderDTO struct {
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
	Class        string `json:"lic"`
	Used         int    `json:"used"`
	Daily        int    `json:"daily"`
	BackoffUntil int64  `json:"backoffUntil,omitempty"`
	Sends        string `json:"sends"`
	Homepage     string `json:"homepage"`
}

func apiRouteProviders(s *Server, r *http.Request) (any, error) {
	cfg := s.eng.EffectiveConfig().NavData.Routes
	used, daily, until := s.eng.NavData().OnlineBudget()
	p := routeProviderDTO{Name: "adsbdb", Enabled: cfg.Enabled && cfg.Online, Class: "restricted", Used: used, Daily: daily,
		Sends:    "the callsign of an aircraft with no offline route, after it was received for " + strconv.Itoa(cfg.StableSec) + " s with a position; never the receiver location",
		Homepage: "https://www.adsbdb.com/"}
	if !until.IsZero() {
		p.BackoffUntil = until.Unix()
	}
	return []routeProviderDTO{p}, nil
}
