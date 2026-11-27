package server

// Reverse-MLAT survey routes (internal/mlat/survey, DESIGN §10.2-10.3,
// §19.3). Owner- and admin-only: survey results hold the station's exact
// position (DESIGN §8.2); nothing here is public and nothing is logged.

import (
	"airview/internal/analytics"
	"errors"
	"math"
	"net/http"
	"time"

	"airview/internal/mlat/survey"
	"airview/internal/registry"
)

func init() {
	routes = append(routes, surveyRoutes...)
}

// consentC1 is CONSENT.md C1 (timestamps and location), returned when a
// survey is refused because the station sends no timestamps.
const consentC1 = "If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this."

var surveyRoutes = []Route{
	{Method: "GET", Path: "/api/me/stations/{sid}/survey", Group: grpMe, Access: AccessOwner, Station: StationPath, NetOnly: true, API: apiSurveyStatus, NoAuto: true,
		Summary: "The station's reverse-MLAT survey: the running job, the last results (position, σ, 95 % ellipse, stable/suspect, declared-position check, MSL/HAE verdict) and the next scheduled check.",
		Returns: "`{\"enabled\":bool,\"running\":bool,...,\"last\":{...},\"history\":[...],\"nextCheck\":ms}`"},
	{Method: "POST", Path: "/api/me/stations/{sid}/survey", Group: grpMe, Access: AccessOwner, Station: StationPath, NetOnly: true, API: apiMeSurveyStart, NoAuto: true,
		Summary: "Ask for a survey of the station (at most one per hour and station).",
		Returns: "`{\"job\":n,\"queued\":bool}`; `202` when queued behind other surveys, `409` with the consent text when the station sends no timestamps, `429` when a survey ran recently",
		Notes:   "Audited (`station.survey.start`)."},
	{Method: "GET", Path: "/api/admin/stations/{sid}/survey", Group: grpNetAdm, Access: AccessAdmin, Station: StationPath, NetOnly: true, API: apiSurveyStatus, NoAuto: true,
		Summary: "A station's reverse-MLAT survey status (as the owner sees it)."},
	{Method: "POST", Path: "/api/admin/stations/{sid}/survey", Group: grpNetAdm, Access: AccessAdmin, Station: StationPath, NetOnly: true, Admin: true, API: apiAdminSurveyStart, NoAuto: true,
		Summary: "Start a survey of a station (no rate limit).", Returns: "`{\"job\":n,\"queued\":bool}`",
		Notes: "Audited (`station.survey.start`)."},
	{Method: "DELETE", Path: "/api/admin/stations/{sid}/survey", Group: grpNetAdm, Access: AccessAdmin, Station: StationPath, NetOnly: true, Admin: true, API: apiAdminSurveyCancel, NoAuto: true,
		Summary: "Cancel a station's running or queued survey.", Returns: "`{\"cancelled\":bool}`"},
	{Method: "GET", Path: "/api/me/stations/{sid}/locate", Group: grpMe, Access: AccessOwner, Station: StationPath, NetOnly: true, API: apiStationLocate, NoAuto: true, Cache: ttlMed,
		Summary: "Locate Me v2 for a station without a location: the approximate position estimated from what the station receives (radio horizon, skyline, signal level), with its 95 % circle and the error against the last survey.",
		Returns: "Locate2Result (`lat`, `lon`, `r95Km`, `sigmaKm`, `terms[]`, `errorKm`, `refSource`, `notes[]`); `404` without stations.db",
		Notes:   "Owner/admin only (DESIGN §8.2). Uses the station's guess samples of the last 7 days, recorded only while the station has no location (`locate.*`)."},
	{Method: "GET", Path: "/api/admin/mlat/survey", Group: grpNetAdm, Access: AccessAdmin, NetOnly: true, API: apiAdminSurveyStats, NoAuto: true,
		Summary: "Survey counters: stations known, running and queued jobs, finished jobs by outcome, tap counters.",
		Returns: "`{\"enabled\":bool,\"stats\":{...},\"bundle\":{\"stats\":{...},\"last\":{...}}}` (bundle: the network bundle adjustment, when on)"},
}

// surveyResultDTO is one survey result (owner/admin: exact position).
type surveyResultDTO struct {
	Job          uint64   `json:"job"`
	Reason       string   `json:"reason"`
	State        string   `json:"state"`
	Started      int64    `json:"started"`  // Unix ms
	Finished     int64    `json:"finished"` // Unix ms
	Observations int      `json:"observations"`
	Valid        bool     `json:"valid"`
	Error        string   `json:"error,omitempty"`
	Lat          *float64 `json:"lat,omitempty"`
	Lon          *float64 `json:"lon,omitempty"`
	Alt          *float64 `json:"alt,omitempty"` // HAE
	SigmaHM      num      `json:"sigmaHM"`
	SigmaVM      num      `json:"sigmaVM"`
	Ellipse95A   num      `json:"ellipse95A"`
	Ellipse95B   num      `json:"ellipse95B"`
	EllipseAzDeg num      `json:"ellipseAzDeg"`
	Aircraft     int      `json:"aircraft"`
	Stable       bool     `json:"stable"`
	Suspect      bool     `json:"suspect"`
	SuspectWhy   []string `json:"suspectWhy,omitempty"`
	AutoUse      bool     `json:"autoUse"`
	// Declared-position check (DESIGN §10.3).
	DeclaredChecked bool `json:"declaredChecked"`
	DeclaredDistM   num  `json:"declaredDistM"`
	DeclaredLimitM  num  `json:"declaredLimitM"`
	DeclaredSuspect bool `json:"declaredSuspect"`
	// AltRef is the MSL/HAE verdict ("" = not tested / undecided).
	AltRefTested bool   `json:"altRefTested"`
	AltRef       string `json:"altRef,omitempty"`
}

type surveyStatusDTO struct {
	Enabled   bool              `json:"enabled"`
	Running   bool              `json:"running"`
	Job       uint64            `json:"job,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	Started   int64             `json:"started,omitempty"`
	Observed  int               `json:"observed"`
	Last      *surveyResultDTO  `json:"last,omitempty"`
	History   []surveyResultDTO `json:"history"`
	NextCheck int64             `json:"nextCheck,omitempty"`
}

func surveyResultTO(r *survey.Result) surveyResultDTO {
	o := surveyResultDTO{Job: r.Job, Reason: string(r.Reason), State: string(r.State), Started: unixMs(r.Started),
		Finished: unixMs(r.Finished), Observations: r.Observations, Valid: r.Valid, Error: r.Error,
		Stable: r.Stable, Suspect: r.Suspect, SuspectWhy: r.SuspectWhy, AutoUse: r.AutoUse,
		DeclaredChecked: r.DeclaredChecked, DeclaredSuspect: r.DeclaredSuspect,
		DeclaredDistM: rnd(r.DeclaredDistM, 0), DeclaredLimitM: rnd(r.DeclaredLimitM, 0),
		AltRefTested: r.AltRef.Tested, AltRef: r.AltRef.Chosen,
		SigmaHM: num(math.NaN()), SigmaVM: num(math.NaN()), Ellipse95A: num(math.NaN()), Ellipse95B: num(math.NaN()), EllipseAzDeg: num(math.NaN())}
	if r.Valid {
		lat, lon, alt := round6(r.Solution.LLH.Lat), round6(r.Solution.LLH.Lon), round1(r.Solution.LLH.H)
		o.Lat, o.Lon, o.Alt = &lat, &lon, &alt
		o.SigmaHM, o.SigmaVM = rnd(r.Solution.SigmaHM, 1), rnd(r.Solution.SigmaVM, 1)
		o.Ellipse95A, o.Ellipse95B, o.EllipseAzDeg = rnd(r.Ellipse95A, 1), rnd(r.Ellipse95B, 1), rnd(r.Ellipse95AngleDeg, 0)
		o.Aircraft = r.Solution.Aircraft
	}
	return o
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func (s *Server) surveyor() *survey.Surveyor {
	if s.eng == nil {
		return nil
	}
	return s.eng.Survey()
}

func apiSurveyStatus(s *Server, r *http.Request) (any, error) {
	st, _ := selectedStation(r)
	out := surveyStatusDTO{History: []surveyResultDTO{}}
	sv := s.surveyor()
	if sv == nil {
		return out, nil
	}
	out.Enabled = true
	stt, ok := sv.Status(st.Idx)
	if !ok {
		return out, nil
	}
	out.Running, out.Job, out.Reason, out.Observed = stt.Running, stt.Job, string(stt.Reason), stt.Observed
	if !stt.Started.IsZero() {
		out.Started = unixMs(stt.Started)
	}
	if !stt.NextCheck.IsZero() {
		out.NextCheck = unixMs(stt.NextCheck)
	}
	if stt.Last != nil {
		l := surveyResultTO(stt.Last)
		out.Last = &l
	}
	for i := range stt.History {
		out.History = append(out.History, surveyResultTO(&stt.History[i]))
	}
	return out, nil
}

func (s *Server) surveyStart(r *http.Request, reason survey.Reason) (any, error) {
	sv := s.surveyor()
	if sv == nil {
		return nil, notFound("the survey does not run (aggregator.mlat.enabled / aggregator.mlat.survey.disabled)")
	}
	st, _ := selectedStation(r)
	job, err := sv.Start(st.Idx, reason)
	queued := false
	switch {
	case errors.Is(err, survey.ErrNoConsent), errors.Is(err, survey.ErrNotTimed):
		return nil, errStatus(http.StatusConflict, "the station sends no timestamps, so it cannot be surveyed. %s", consentC1)
	case errors.Is(err, survey.ErrRateLimited):
		return nil, errStatus(http.StatusTooManyRequests, "a survey of this station ran within the last hour")
	case errors.Is(err, survey.ErrUnknownStation):
		return nil, notFound("the survey does not know this station yet (it is not approved or not connected)")
	case errors.Is(err, survey.ErrBusy):
		queued = true
	case err != nil:
		return nil, err
	}
	if reg, rerr := s.reg(); rerr == nil {
		_ = reg.Audit(r.Context(), s.actorOf(r), "station.survey.start", st.SID, `{"reason":"`+string(reason)+`"}`)
	}
	res := map[string]any{"job": job, "queued": queued}
	if queued {
		return accepted(res), nil
	}
	return res, nil
}

func apiMeSurveyStart(s *Server, r *http.Request) (any, error) {
	return s.surveyStart(r, survey.ReasonOwner)
}

func apiAdminSurveyStart(s *Server, r *http.Request) (any, error) {
	return s.surveyStart(r, survey.ReasonAdmin)
}

func apiAdminSurveyCancel(s *Server, r *http.Request) (any, error) {
	sv := s.surveyor()
	if sv == nil {
		return nil, notFound("the survey does not run")
	}
	st, _ := selectedStation(r)
	return map[string]any{"cancelled": sv.Cancel(st.Idx)}, nil
}

func apiStationLocate(s *Server, r *http.Request) (any, error) {
	st, _ := selectedStation(r)
	if s.eng == nil {
		return nil, notFound("not available")
	}
	an := s.eng.StationAnalyzer(st.SID)
	if an == nil {
		return nil, notFound("this station has no statistics database")
	}
	req := analytics.LocateRequest{}
	if sv := s.surveyor(); sv != nil {
		if stt, ok := sv.Status(st.Idx); ok && stt.Last != nil && stt.Last.Valid {
			req.Ref = &analytics.LatLon{Lat: stt.Last.Solution.LLH.Lat, Lon: stt.Last.Solution.LLH.Lon}
			req.RefSource = "survey"
		}
	}
	return an.LocateMe(r.Context(), req)
}

func apiAdminSurveyStats(s *Server, r *http.Request) (any, error) {
	sv := s.surveyor()
	if sv == nil {
		return map[string]any{"enabled": false}, nil
	}
	out := map[string]any{"enabled": true, "stats": sv.Stats(), "at": unixMs(time.Now())}
	if bd := s.eng.Bundler(); bd != nil {
		// Network bundle adjustment (admin: the last result holds exact
		// positions).
		b := map[string]any{"stats": bd.Stats()}
		if last := bd.Last(); last != nil {
			b["last"] = bundleResultDTO(last, s.registry())
		}
		out["bundle"] = b
	}
	return out, nil
}

// bundleStationDTO is one station of the last bundle adjustment (admin).
type bundleStationDTO struct {
	Station     int      `json:"station"`
	SID         string   `json:"sid,omitempty"`
	Name        string   `json:"name,omitempty"`
	Prior       string   `json:"prior"`
	Used        bool     `json:"used"`
	Why         string   `json:"why,omitempty"`
	Determined  bool     `json:"determined"`
	Lat         num      `json:"lat"`
	Lon         num      `json:"lon"`
	HAE         num      `json:"hae"`
	SigmaHM     num      `json:"sigmaHM"`
	SigmaVM     num      `json:"sigmaVM"`
	ShiftHM     num      `json:"shiftHM"`
	Receptions  int      `json:"receptions"`
	Rejected    int      `json:"rejected"`
	JitterNs    num      `json:"jitterNs"`
	PriorTested bool     `json:"priorTested"`
	PriorDistHM num      `json:"priorDistHM"`
	Released    bool     `json:"priorReleased"`
	Outlier     bool     `json:"outlier"`
	OutlierWhy  []string `json:"outlierWhy,omitempty"`
	Seeded      bool     `json:"seeded"`
}

func bundleResultDTO(r *survey.BundleResult, reg *registry.Registry) map[string]any {
	sts := make([]bundleStationDTO, 0, len(r.Stations))
	for i := range r.Stations {
		x := &r.Stations[i]
		sts = append(sts, bundleStationDTO{Station: int(x.ID), Prior: x.Prior.String(), Used: x.Used, Why: x.Why, Determined: x.Determined,
			Lat: num(x.LLH.Lat), Lon: num(x.LLH.Lon), HAE: num(x.LLH.H), SigmaHM: num(x.SigmaHM), SigmaVM: num(x.SigmaVM),
			ShiftHM: num(x.ShiftHM), Receptions: x.Receptions, Rejected: x.Rejected, JitterNs: num(x.JitterNs),
			PriorTested: x.PriorTested, PriorDistHM: num(x.PriorDistHM), Released: x.PriorReleased, Outlier: x.Outlier,
			OutlierWhy: x.OutlierWhy, Seeded: x.Seeded})
		if reg != nil {
			if st, ok := reg.Station(x.ID); ok {
				sts[len(sts)-1].SID, sts[len(sts)-1].Name = st.SID, st.Name
			}
		}
	}
	return map[string]any{"stations": sts, "frames": r.Frames, "framesUsed": r.FramesUsed, "receptions": r.Receptions,
		"rejected": r.Rejected, "released": r.Released, "varianceFactor": num(r.VarianceFactor), "durationS": num(r.DurationS),
		"elapsedMs": r.Elapsed.Milliseconds()}
}
