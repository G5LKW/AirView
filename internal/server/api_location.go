package server

// Station location flow of phase 3 (DESIGN §10, §11 "Location", §19.3;
// WP-W3-wire): the owner's location notices, the admin survey console and
// the network's location quality.
//
// Privacy (DESIGN §8.2): notices are built from audit rows, which never
// hold coordinates; the admin survey list carries no positions either (the
// per-station survey route has them, admin/owner only). Nothing here is
// public.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"airview/internal/model"
	"airview/internal/registry"
)

func init() {
	routes = append(routes, locationRoutes...)
}

var locationRoutes = []Route{
	{Method: "GET", Path: "/api/me/stations/{sid}/notices", Group: grpMe, Access: AccessOwner, Station: StationPath, NetOnly: true, API: apiStationNotices, NoAuto: true,
		Summary: "Location notices of the station (last 30 days, newest first): finished surveys, declared-position checks, the automatic use of the survey position for MLAT and its owner override, confirmations.",
		Returns: "`{\"notices\":[{\"t\":ms,\"kind\":\"survey|declared|mlat|located|owner\",\"level\":\"info|ok|warn\",\"text\":\"...\"}]}`",
		Notes:   "Built from the audit log, which never holds coordinates."},
	{Method: "GET", Path: "/api/me/notices", Group: grpMe, Access: AccessOwner, NetOnly: true, API: apiMyNotices, NoAuto: true,
		Summary: "Location notices of all the viewer's stations (last 14 days, at most 50, newest first).",
		Returns: "`{\"notices\":[{\"sid\",\"name\",\"t\",\"kind\",\"level\",\"text\"}]}`"},
	{Method: "GET", Path: "/api/admin/mlat/surveys", Group: grpNetAdm, Access: AccessAdmin, NetOnly: true, API: apiAdminSurveys, NoAuto: true,
		Summary: "Survey console: every approved station's location status, MLAT position source, running survey and last result (no coordinates), plus the network's location quality summary.",
		Returns: "`{\"enabled\":bool,\"autoUse\":bool,\"autoUseMaxSigmaM\":n,\"stats\":{...},\"quality\":{...},\"stations\":[...]}`"},
}

// noticeDTO is one location notice.
type noticeDTO struct {
	SID   string `json:"sid,omitempty"`
	Name  string `json:"name,omitempty"`
	T     int64  `json:"t"` // Unix ms
	Kind  string `json:"kind"`
	Level string `json:"level"` // info | ok | warn
	Text  string `json:"text"`
}

// locationNoticeActions are the audit actions that become owner notices.
var locationNoticeActions = []string{"station.survey", "station.mlat.", "station.location."}

func isLocationAction(a string) bool {
	for _, p := range locationNoticeActions {
		if a == p || strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}

// locationNotice turns one audit row into an owner notice (false = not a
// location event). Pure; covered by TestLocationNotice.
func locationNotice(e registry.AuditEntry) (noticeDTO, bool) {
	if !isLocationAction(e.Action) {
		return noticeDTO{}, false
	}
	var d map[string]any
	_ = json.Unmarshal([]byte(e.Detail), &d)
	b := func(k string) bool { v, _ := d[k].(bool); return v }
	f := func(k string) float64 {
		v, ok := d[k].(float64)
		if !ok || math.IsNaN(v) {
			return math.NaN()
		}
		return v
	}
	s := func(k string) string { v, _ := d[k].(string); return v }
	metres := func(v float64) string {
		switch {
		case math.IsNaN(v):
			return "?"
		case v >= 10_000:
			return fmt.Sprintf("%.0f km", v/1000)
		case v >= 1000:
			return fmt.Sprintf("%.1f km", v/1000)
		}
		return fmt.Sprintf("%.0f m", v)
	}
	n := noticeDTO{T: e.T.UnixMilli(), Level: "info"}
	switch e.Action {
	case "station.survey":
		n.Kind = "survey"
		switch {
		case b("suspect"):
			n.Level = "warn"
			n.Text = "Network survey finished, but its result failed the consistency checks and is not used."
		case s("state") == "done" || b("stable"):
			n.Level = "ok"
			n.Text = fmt.Sprintf("Network survey finished: position measured to ±%s.", metres(f("sigmaHM")))
			if b("autoUse") {
				n.Text += " It is precise enough to be used for MLAT automatically; you can reject it in the Location section."
			}
		case s("state") == "nodata":
			n.Text = "Network survey ended without enough data (the station shared too few aircraft with located stations). It will be retried."
		case s("state") == "cancelled":
			n.Text = "Network survey cancelled."
		default:
			n.Text = fmt.Sprintf("Network survey finished without a stable result (±%s); it will be repeated.", metres(f("sigmaHM")))
		}
		switch s("altRef") {
		case model.AltRefMSL:
			n.Text += " Your altitude was found to be above mean sea level (MSL)."
		case model.AltRefHAE:
			n.Text += " Your altitude was found to be above the WGS-84 ellipsoid (HAE)."
		}
	case "station.survey.declared":
		n.Kind = "declared"
		if b("suspect") {
			n.Level = "warn"
			n.Text = fmt.Sprintf("Your declared position is %s from where the network measures your antenna. Until it is corrected the station is not used for MLAT geometry.", metres(f("discrepancyM")))
		} else {
			n.Level = "ok"
			n.Text = "Your declared position agrees with the network's measurement again."
		}
	case "station.survey.start":
		n.Kind = "survey"
		switch s("reason") {
		case "admin":
			n.Text = "The network operator started a survey of this station."
		default:
			n.Text = "You asked for a survey of this station."
		}
	case "station.mlat.position":
		n.Kind = "mlat"
		switch {
		case s("to") == "survey":
			n.Level = "ok"
			// sigmaM of the engine's change is the 3-D σ (dominated by the
			// vertical); the horizontal σ is in the survey notice, so none is
			// quoted here.
			n.Text = "MLAT now uses the network's survey position of this station. You can reject it in the Location section."
		case s("from") == "survey":
			n.Text = "MLAT no longer uses the survey position"
			switch s("to") {
			case "none":
				n.Text += "; the station has no position for MLAT now."
			case "declared":
				n.Text += "; it uses your declared position."
			case "confirmed":
				n.Text += "; it uses the position you confirmed."
			case "gnss":
				n.Text += "; it uses the receiver's GNSS position."
			default:
				n.Text += "."
			}
		default:
			return noticeDTO{}, false
		}
	case "station.mlat.survey":
		n.Kind = "owner"
		if b("reject") {
			n.Text = "The survey position is not used for MLAT (your choice)."
		} else {
			n.Text = "The survey position may be used for MLAT automatically (your choice)."
		}
	case "station.location.survey":
		n.Kind = "located"
		if b("located") {
			n.Level = "ok"
			n.Text = "The network located this station; its measured position is now shown as \"estimated\"."
		} else {
			n.Text = "The network's measured position is no longer used; the station is shown as not located."
		}
	case "station.location.confirm":
		n.Kind = "owner"
		n.Level = "ok"
		n.Text = "You confirmed a measured position as the station's location."
	default:
		return noticeDTO{}, false
	}
	return n, true
}

// stationNotices returns the location notices of sid since since.
func stationNotices(r *http.Request, reg *registry.Registry, sid string, since time.Time, limit int) ([]noticeDTO, error) {
	rows, err := reg.AuditLog(r.Context(), registry.AuditFilter{SID: sid, ActionPrefix: "station.", Since: since, Limit: 500})
	if err != nil {
		return nil, err
	}
	out := []noticeDTO{}
	for _, e := range rows {
		if n, ok := locationNotice(e); ok {
			out = append(out, n)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func apiStationNotices(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	out, err := stationNotices(r, reg, st.SID, time.Now().Add(-30*24*time.Hour), 100)
	if err != nil {
		return nil, regErr(err)
	}
	return map[string]any{"notices": out}, nil
}

func apiMyNotices(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	since := time.Now().Add(-14 * 24 * time.Hour)
	out := []noticeDTO{}
	for _, st := range reg.OwnerStations(s.viewerOf(r).OwnerID) {
		ns, err := stationNotices(r, reg, st.SID, since, 50)
		if err != nil {
			return nil, regErr(err)
		}
		for _, n := range ns {
			n.SID, n.Name = st.SID, st.Name
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T > out[j].T })
	if len(out) > 50 {
		out = out[:50]
	}
	return map[string]any{"notices": out}, nil
}

// surveyRowDTO is one station of the admin survey console (no position).
type surveyRowDTO struct {
	SID            string         `json:"sid"`
	Name           string         `json:"name"`
	Kind           string         `json:"kind"`
	ClockClass     string         `json:"clockClass"`
	LocStatus      string         `json:"locStatus"`
	PositionSource string         `json:"positionSource"`
	SurveyRejected bool           `json:"surveyRejected"`
	Timestamps     bool           `json:"timestamps"`
	Running        bool           `json:"running"`
	Reason         string         `json:"reason,omitempty"`
	Started        int64          `json:"started,omitempty"`
	Observed       int            `json:"observed"`
	NextCheck      int64          `json:"nextCheck,omitempty"`
	Last           *surveyLastDTO `json:"last,omitempty"`
}

// surveyLastDTO is the last survey result of a console row (no position).
type surveyLastDTO struct {
	Finished        int64    `json:"finished"`
	Reason          string   `json:"reason"`
	State           string   `json:"state"`
	Valid           bool     `json:"valid"`
	Stable          bool     `json:"stable"`
	Suspect         bool     `json:"suspect"`
	SuspectWhy      []string `json:"suspectWhy,omitempty"`
	AutoUse         bool     `json:"autoUse"`
	SigmaHM         num      `json:"sigmaHM"`
	Ellipse95A      num      `json:"ellipse95A"`
	Aircraft        int      `json:"aircraft"`
	Observations    int      `json:"observations"`
	DeclaredChecked bool     `json:"declaredChecked"`
	DeclaredDistM   num      `json:"declaredDistM"`
	DeclaredSuspect bool     `json:"declaredSuspect"`
	AltRef          string   `json:"altRef,omitempty"`
}

// locQualityDTO summarises how the network's stations are located.
type locQualityDTO struct {
	Stations int            `json:"stations"`
	ByStatus map[string]int `json:"byStatus"`
	BySource map[string]int `json:"bySource"` // MLAT position source
	// SigmaBins: stations whose last survey has σ_h in [0,10), [10,30),
	// [30,50), [50,200), [200,∞) m.
	SigmaBins [5]int `json:"sigmaBins"`
	// Declared-position checks: checked, agreeing, suspect, and the
	// median / max discrepancy of the checked ones (m).
	Checked      int `json:"checked"`
	Agreeing     int `json:"agreeing"`
	Suspect      int `json:"suspect"`
	MedianDistM  num `json:"medianDistM"`
	MaxDistM     num `json:"maxDistM"`
	AutoUsed     int `json:"autoUsed"`
	Rejected     int `json:"rejected"`
	Unlocated    int `json:"unlocated"`
	SurveyedOK   int `json:"surveyedStable"`
	Running      int `json:"running"`
	NoTimestamps int `json:"noTimestamps"`
}

func sigmaBin(s float64) int {
	switch {
	case s < 10:
		return 0
	case s < 30:
		return 1
	case s < 50:
		return 2
	case s < 200:
		return 3
	}
	return 4
}

func apiAdminSurveys(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	cfg := s.eng.EffectiveConfig().Aggregator.MLAT
	out := map[string]any{"enabled": false, "autoUse": cfg.AutoUseSurvey, "autoUseMaxSigmaM": cfg.AutoUseMaxSigmaM, "at": unixMs(time.Now())}
	sv := s.surveyor()
	eng := s.eng.MLAT()
	q := locQualityDTO{ByStatus: map[string]int{}, BySource: map[string]int{}}
	rows := []surveyRowDTO{}
	var dists []float64
	for _, st := range reg.List(registry.Filter{Admission: model.AdmApproved}) {
		if st.Idx == model.HostStation {
			continue
		}
		row := surveyRowDTO{SID: st.SID, Name: st.Name, Kind: st.Kind, ClockClass: st.ClockCls.String(), LocStatus: st.LocStatus,
			Timestamps:     st.ClockCls.Timed() && !st.Has(registry.FlagBeast) && !st.Has(registry.FlagTimingExcluded),
			SurveyRejected: st.Has(registry.FlagSurveyRejected), PositionSource: "none"}
		if row.LocStatus == "" {
			row.LocStatus = model.LocUnknown
		}
		if eng != nil {
			row.PositionSource = eng.PositionSource(st.Idx).String()
		}
		q.Stations++
		q.ByStatus[row.LocStatus]++
		q.BySource[row.PositionSource]++
		if !st.Located {
			q.Unlocated++
		}
		if !row.Timestamps {
			q.NoTimestamps++
		}
		if row.PositionSource == "survey" {
			q.AutoUsed++
		}
		if row.SurveyRejected {
			q.Rejected++
		}
		if sv != nil {
			if stt, ok := sv.Status(st.Idx); ok {
				row.Running, row.Reason, row.Observed = stt.Running, string(stt.Reason), stt.Observed
				if stt.Running {
					q.Running++
				}
				if !stt.Started.IsZero() {
					row.Started = unixMs(stt.Started)
				}
				if !stt.NextCheck.IsZero() {
					row.NextCheck = unixMs(stt.NextCheck)
				}
				if l := stt.Last; l != nil {
					d := surveyResultTO(l)
					row.Last = &surveyLastDTO{Finished: d.Finished, Reason: d.Reason, State: d.State, Valid: d.Valid, Stable: d.Stable,
						Suspect: d.Suspect, SuspectWhy: d.SuspectWhy, AutoUse: d.AutoUse, SigmaHM: d.SigmaHM, Ellipse95A: d.Ellipse95A,
						Aircraft: d.Aircraft, Observations: d.Observations, DeclaredChecked: d.DeclaredChecked,
						DeclaredDistM: d.DeclaredDistM, DeclaredSuspect: d.DeclaredSuspect, AltRef: d.AltRef}
					if l.Valid && l.Stable && !l.Suspect {
						q.SurveyedOK++
						q.SigmaBins[sigmaBin(l.Solution.SigmaHM)]++
					}
					if l.DeclaredChecked {
						q.Checked++
						if l.DeclaredSuspect {
							q.Suspect++
						} else {
							q.Agreeing++
						}
						if !math.IsNaN(l.DeclaredDistM) {
							dists = append(dists, l.DeclaredDistM)
						}
					}
				}
			}
		}
		rows = append(rows, row)
	}
	q.MedianDistM, q.MaxDistM = num(math.NaN()), num(math.NaN())
	if len(dists) > 0 {
		sort.Float64s(dists)
		q.MedianDistM, q.MaxDistM = rnd(dists[len(dists)/2], 0), rnd(dists[len(dists)-1], 0)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Running != rows[j].Running {
			return rows[i].Running
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})
	out["stations"] = rows
	out["quality"] = q
	if sv != nil {
		out["enabled"] = true
		out["stats"] = sv.Stats()
	}
	return out, nil
}
