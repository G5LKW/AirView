package server

// Views and actions added by the integrator after the hardening wave
// (H-mlatc, H-track): the timing quarantine of new stations, feeder
// positions held for confirmation, and the owner's control over the
// automatic use of a survey position for MLAT (DESIGN §19.3).

import (
	"context"
	"net/http"
	"sort"
	"time"

	"airview/internal/mlat"
	"airview/internal/mlat/geo"
	"airview/internal/registry"
	"airview/internal/registry/trust"
)

func init() {
	routes = append(routes, hardenRoutes...)
}

var hardenRoutes = []Route{
	{Method: "POST", Path: "/api/me/stations/{sid}/location/pending/confirm", Group: grpMe, Access: AccessOwner, Station: StationPath, NetOnly: true, API: apiMePendingConfirm, NoAuto: true,
		Summary: "Apply the position the station's feeder announced and the aggregator holds (a large move waits for the survey or this confirmation; finding PRIV-1).",
		Returns: "the station (OwnerStation); `404` when no position is pending"},
	{Method: "PUT", Path: "/api/me/stations/{sid}/mlat/survey", Group: grpMe, Access: AccessOwner, Station: StationPath, NetOnly: true, API: apiMeSurveyUse, NoAuto: true,
		Summary: "Allow or reject the automatic use of the station's survey position for MLAT (DESIGN §19.3).",
		Body:    "`{\"reject\":bool}`",
		Returns: "`{\"positionSource\":\"...\",\"surveyRejected\":bool}`",
		Notes:   "The choice is stored with the station and stands until it is changed."},
	{Method: "POST", Path: "/api/admin/stations/{sid}/timing", Group: grpNetAdm, Access: AccessAdmin, Station: StationPath, NetOnly: true, Admin: true, API: apiAdminTiming, NoAuto: true,
		Summary: "Override a station's timing quarantine: `trusted` (joins MLAT now), `excluded` (never), `quarantine` (judge again), `auto` (clear overrides).",
		Body:    "`{\"mode\":\"trusted|excluded|quarantine|auto\"}`", Returns: "`{\"timing\":\"...\",\"override\":\"...\"}`",
		Notes: "Audited (`station.timing`); the MLAT gate applies it at once."},
	{Method: "GET", Path: "/api/admin/mlat/quarantine", Group: grpNetAdm, Access: AccessAdmin, NetOnly: true, API: apiAdminQuarantine, NoAuto: true,
		Summary: "Stations in timing quarantine with the gate's progress per station, and the gate counters.",
		Returns: "`{\"enabled\":bool,\"stats\":{...},\"stations\":[{\"sid\",\"name\",\"since\",\"report\"}]}`"},
}

// timingDTO is a station's timing quarantine state (owner: own station;
// admin: any). No peer identities.
type timingDTO struct {
	State string `json:"state"` // trusted | quarantined | excluded
	// Override is the admin override in force (admin view only).
	Override string        `json:"override,omitempty"`
	Progress *trust.Report `json:"progress,omitempty"`
}

// pendingPosDTO is a feeder-announced position the aggregator holds.
type pendingPosDTO struct {
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Alt    float64 `json:"alt"`
	AltRef string  `json:"altRef,omitempty"`
	GNSS   bool    `json:"gnss"`
	Reason string  `json:"reason"` // rate-limited | held
	Since  int64   `json:"since"`  // Unix ms
	Last   int64   `json:"last"`   // Unix ms
}

// mlatPositionDTO is the station's MLAT position source and the survey.
type mlatPositionDTO struct {
	Source         string            `json:"source"`
	SurveyRejected bool              `json:"surveyRejected"`
	Survey         *surveyDTO        `json:"survey,omitempty"`
	LastChange     *positionNoticeTO `json:"lastChange,omitempty"`
}

type surveyDTO struct {
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Alt     float64 `json:"alt"` // HAE
	SigmaHM float64 `json:"sigmaHM"`
	SigmaVM float64 `json:"sigmaVM"`
	Stable  bool    `json:"stable"`
	Suspect bool    `json:"suspect"`
}

// positionNoticeTO is the owner notice of a change of the MLAT position
// source that involved the survey (DESIGN §19.3).
type positionNoticeTO struct {
	T          int64  `json:"t"` // Unix ms
	From       string `json:"from"`
	To         string `json:"to"`
	AutoSurvey bool   `json:"autoSurvey"`
	Reason     string `json:"reason"`
}

// fillHardenDetail adds the timing, pending-position and MLAT position
// sections to an owner/admin station detail.
func (s *Server) fillHardenDetail(ctx context.Context, reg *registry.Registry, st *registry.Station, admin bool, out *stationDetail) {
	td := &timingDTO{State: st.Timing()}
	if admin {
		td.Override = st.TimingOverride()
	}
	if s.eng != nil {
		if g := s.eng.TrustGate(); g != nil {
			if rep, ok := g.Status(st.Idx); ok {
				td.Progress = &rep
			}
		}
	}
	out.Timing = td
	if p, ok, err := reg.PendingPosition(ctx, st.Idx); err == nil && ok {
		out.PendingPosition = &pendingPosDTO{Lat: p.Lat, Lon: p.Lon, Alt: p.Alt, AltRef: p.AltRef, GNSS: p.GNSS,
			Reason: p.Reason, Since: unixMs(p.Since), Last: unixMs(p.Last)}
	}
	if s.eng == nil {
		return
	}
	eng := s.eng.MLAT()
	if eng == nil {
		return
	}
	mp := &mlatPositionDTO{Source: eng.PositionSource(st.Idx).String()}
	if sv, ok := eng.SurveyOf(st.Idx); ok && sv.Valid {
		g := geo.FromECEF(sv.ECEF)
		mp.Survey = &surveyDTO{Lat: g.Lat, Lon: g.Lon, Alt: round1(g.H), SigmaHM: round1(sv.SigmaHM), SigmaVM: round1(sv.SigmaVM),
			Stable: sv.Stable, Suspect: sv.Suspect}
		mp.SurveyRejected = sv.OwnerRejected
	}
	if n, ok := s.eng.MLATPositionNotice(st.Idx); ok {
		c := n.Change
		mp.LastChange = &positionNoticeTO{T: unixMs(n.At), From: c.Old.String(), To: c.New.String(),
			AutoSurvey: c.AutoSurvey, Reason: c.Reason}
	}
	out.MLATPosition = mp
}

func apiMePendingConfirm(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	next, ok, err := reg.ConfirmPendingPosition(r.Context(), s.actorOf(r), st.Idx)
	if err != nil {
		return nil, regErr(err)
	}
	if !ok {
		return nil, notFound("no position is pending")
	}
	s.cache.invalidate()
	return s.stationDTO(r.Context(), &next, false, time.Now()), nil
}

func apiMeSurveyUse(s *Server, r *http.Request) (any, error) {
	var b struct {
		Reject *bool `json:"reject"`
	}
	if err := decodeBody(r, &b, 1<<10); err != nil {
		return nil, err
	}
	if b.Reject == nil {
		return nil, badRequest("reject is required")
	}
	var eng *mlat.Engine
	if s.eng != nil {
		eng = s.eng.MLAT()
	}
	if eng == nil {
		return nil, notFound("the MLAT engine does not run")
	}
	st, _ := selectedStation(r)
	if !eng.OverrideSurvey(st.Idx, *b.Reject) {
		return nil, notFound("the MLAT engine does not know this station yet")
	}
	if reg, err := s.reg(); err == nil {
		detail := `{"reject":false}`
		if *b.Reject {
			detail = `{"reject":true}`
		}
		// Persisted with the station: the engine re-applies it after a
		// restart (internal/engine syncMLATStations).
		_, _ = reg.Update(r.Context(), st.Idx, func(x *registry.Station) error {
			if *b.Reject {
				x.Flags |= registry.FlagSurveyRejected
			} else {
				x.Flags &^= registry.FlagSurveyRejected
			}
			return nil
		})
		_ = reg.Audit(r.Context(), s.actorOf(r), "station.mlat.survey", st.SID, detail)
	}
	return map[string]any{"positionSource": eng.PositionSource(st.Idx).String(), "surveyRejected": *b.Reject}, nil
}

func apiAdminTiming(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	var b struct {
		Mode string `json:"mode"`
	}
	if err := decodeBody(r, &b, 1<<10); err != nil {
		return nil, err
	}
	switch b.Mode {
	case registry.TimingTrusted, registry.TimingExcluded, "quarantine", "auto":
	default:
		return nil, badRequest("mode: trusted | excluded | quarantine | auto")
	}
	st, _ := selectedStation(r)
	next, err := reg.SetTiming(r.Context(), s.actorOf(r), st.Idx, b.Mode)
	if err != nil {
		return nil, regErr(err)
	}
	if s.eng != nil {
		if g := s.eng.TrustGate(); g != nil {
			g.Refresh()
		}
	}
	s.cache.invalidate()
	return map[string]string{"timing": next.Timing(), "override": next.TimingOverride()}, nil
}

type quarantinedDTO struct {
	SID    string        `json:"sid"`
	Name   string        `json:"name"`
	Kind   string        `json:"kind"`
	Since  int64         `json:"created"` // Unix ms (station creation)
	Report *trust.Report `json:"report,omitempty"`
}

func apiAdminQuarantine(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	out := struct {
		Enabled  bool             `json:"enabled"`
		Stats    *trust.Stats     `json:"stats,omitempty"`
		Stations []quarantinedDTO `json:"stations"`
	}{Enabled: !s.eng.Config().Get().Aggregator.MLAT.Quarantine.Disabled, Stations: []quarantinedDTO{}}
	var g *trust.Gate
	if s.eng != nil {
		g = s.eng.TrustGate()
	}
	if g != nil {
		st := g.Stats()
		out.Stats = &st
	}
	for _, st := range reg.Quarantined() {
		d := quarantinedDTO{SID: st.SID, Name: st.Name, Kind: st.Kind, Since: unixMs(st.Created)}
		if g != nil {
			if rep, ok := g.Status(st.Idx); ok {
				d.Report = &rep
			}
		}
		out.Stations = append(out.Stations, d)
	}
	sort.Slice(out.Stations, func(i, j int) bool { return out.Stations[i].SID < out.Stations[j].SID })
	return out, nil
}
