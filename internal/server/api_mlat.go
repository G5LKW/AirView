package server

import (
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"airview/internal/geo"
	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/quality"
	"airview/internal/model"
	"airview/internal/privacy/publicdto"
	"airview/internal/registry"
)

// MLAT engine views (DESIGN §8.2, §11; internal/mlat WIRING.md §2):
//
//   - public: the network's hold-out accuracy and its 1° cells, no
//     identities (GET /api/public/mlat, and the `mlat` member of
//     /api/public/summary);
//   - owner: one own station's timing and MLAT participation; peers only
//     as weekly per-viewer tags (registry.PeerTag) with a distance band
//     (< 50 / 50-150 / > 150 km), never a SID, name, bearing or position;
//     a summary (synced peer count, contributions, results) and the
//     station's own 2-hour history (mlat_history.go);
//   - admin: engine counters, the quality report, the history of the
//     hold-out error and stage rates, the per-station clock / residual
//     table, the sync matrix (peers by SID), the CRLB map with its
//     summary, and any station's report.
//
// Privacy (DESIGN §8.2): the CRLB map, the sync matrix and the station
// table are admin-only; nothing here reaches public routes except
// publicdto.MLATQuality / MLATCoverage. The golden crawls
// (privacy_golden_test.go, mlat_console_test.go) cover every route with a
// populated engine.

// mlatOff is the answer of the admin MLAT routes while the engine does not
// run (standalone mode, aggregator.mlat.enabled off).
var mlatOff = map[string]any{"running": false}

// publicMLATQuality is the summary's `mlat` member (nil until the engine
// has 20 hold-out samples in the last hour).
func (s *Server) publicMLATQuality() *publicdto.MLATQuality {
	m := s.mlatSrc()
	if m == nil {
		return nil
	}
	q := m.PublicQuality()
	if q.Samples < 20 {
		return nil
	}
	return &publicdto.MLATQuality{P50M: q.P50M, P95M: q.P95M, P99M: q.P99M, EstP50M: q.EstP50M, EstP95M: q.EstP95M, FixesPerMin: q.FixesPerMin}
}

func apiPublicMLAT(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, statsOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	out := publicdto.MLATCoverage{Cells: []publicdto.MLATCell{}, Updated: time.Now().UnixMilli()}
	m := s.mlatSrc()
	if m == nil {
		return out, nil
	}
	q := m.PublicQuality()
	out.Samples = q.Samples
	out.Quality = s.publicMLATQuality()
	for _, c := range q.Cells {
		// Defence in depth: the engine already lists only cells with ≥ 5
		// contributing stations (DESIGN §8.2).
		if c.Stations < 5 || c.N < 20 {
			continue
		}
		out.Cells = append(out.Cells, publicdto.MLATCell{Lat: c.Lat, Lon: c.Lon, Band: c.Band, N: c.N, P50M: c.P50, P95M: c.P95})
	}
	return out, nil
}

// --------------------------------------------------------------- station

// mlatPeerDTO is one clock-sync peer. Owners see Tag (unlinkable, weekly,
// per viewer) and Band; admins see SID, Band and the relative frequency.
// The relative frequency is admin-only (finding PRIV-2): together with the
// owner's own drift it gave the peer's crystal offset, a fingerprint that
// joined the weekly tags across weeks and across one owner's stations.
type mlatPeerDTO struct {
	Tag     string   `json:"tag,omitempty"`
	SID     string   `json:"sid,omitempty"`
	Band    string   `json:"band,omitempty"` // "near" < 50 km, "mid" 50-150 km, "far" > 150 km, "" = unknown
	Count   int      `json:"count"`
	RMSNs   float64  `json:"rmsNs"`
	RelPPB  *float64 `json:"relPpb,omitempty"` // admin only
	SameDom bool     `json:"sameDomain"`
}

// ownerDriftPPM coarsens a station's own clock drift for owner views
// (finding PRIV-2): whole ppm, well above the spread of one crystal's
// estimate (≤ 1 ppm) and comparable to the spacing between crystals, so
// it cannot serve as a fingerprint. Admin views keep the full value.
func ownerDriftPPM(v float64) float64 {
	return math.Round(v)
}

// mlatEventDTO is a clock event with its time in Unix ms.
type mlatEventDTO struct {
	T      int64  `json:"t"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// mlatClockDTO is mlat.ClockReport with the peers and event times mapped
// (the outer Peers / Events fields shadow the embedded ones in JSON).
type mlatClockDTO struct {
	mlat.ClockReport
	Peers  []mlatPeerDTO  `json:"peers"`
	Events []mlatEventDTO `json:"events"`
}

// mlatSummaryDTO is the owner's overview of the own station's MLAT
// participation (DESIGN §11 "Timing" / "MLAT").
type mlatSummaryDTO struct {
	Peers           int     `json:"peers"`       // peers with co-observations
	SyncedPeers     int     `json:"syncedPeers"` // of which synced
	ContribPerHour  int     `json:"contribPerHour"`
	TimedPerMin     int64   `json:"timedPerMin"`
	ResultsEligible bool    `json:"resultsEligible"`
	ResultsReceived *uint64 `json:"resultsReceived"` // fixes sent to the station's live links (null = not reported)
}

// mlatStationDTO is GET .../stations/{sid}/mlat.
type mlatStationDTO struct {
	SID             string                 `json:"sid"`
	Running         bool                   `json:"running"` // the MLAT engine runs
	Known           bool                   `json:"known"`
	PositionSource  string                 `json:"positionSource"`
	Clock           *mlatClockDTO          `json:"clock"`
	MLAT            *quality.StationReport `json:"mlat"`
	TimedPerMin     int64                  `json:"timedPerMin"`
	ResultsEligible bool                   `json:"resultsEligible"`
	Summary         mlatSummaryDTO         `json:"summary"`
	History         *mlatStHistDTO         `json:"history"`
}

// distBand classifies a peer distance (DESIGN §8.2 owner sync peers) from
// the viewer's position (lat, lon) to peer b.
func distBand(lat, lon float64, b *registry.Station) string {
	if b == nil || !b.Located {
		return ""
	}
	d := geo.HaversineKm(lat, lon, b.Effective.Lat, b.Effective.Lon)
	switch {
	case math.IsNaN(d):
		return ""
	case d < 50:
		return "near"
	case d <= 150:
		return "mid"
	default:
		return "far"
	}
}

func (s *Server) mlatStation(reg *registry.Registry, st *registry.Station, admin bool) (*mlatStationDTO, error) {
	m := s.mlatSrc()
	if m == nil {
		return &mlatStationDTO{SID: st.SID, PositionSource: mlat.PosNone.String()}, nil
	}
	rep, known := m.Station(st.Idx)
	out := &mlatStationDTO{SID: st.SID, Running: true, Known: known, PositionSource: rep.PositionSource, MLAT: rep.MLAT,
		TimedPerMin: rep.TimedPerMin, ResultsEligible: rep.ResultsEligible}
	out.Summary.TimedPerMin, out.Summary.ResultsEligible = rep.TimedPerMin, rep.ResultsEligible
	if rep.MLAT != nil {
		out.Summary.ContribPerHour = rep.MLAT.ContribPerHour
	}
	if rep.Clock != nil {
		c := &mlatClockDTO{ClockReport: *rep.Clock, Peers: make([]mlatPeerDTO, 0, len(rep.Clock.Peers)),
			Events: make([]mlatEventDTO, 0, len(rep.Clock.Events))}
		c.ClockReport.Peers, c.ClockReport.Events = nil, nil
		if !admin {
			c.ClockReport.DriftPPM = ownerDriftPPM(c.ClockReport.DriftPPM)
		}
		now := time.Now()
		ref, refOK := s.bandRef(st, admin, now)
		peers := mergedPeers(m, st.Idx, rep.Clock.Peers)
		for _, p := range peers {
			c.Peers = append(c.Peers, mlatPeer(reg, st, p, admin, now, ref, refOK))
		}
		sort.Slice(c.Peers, func(i, j int) bool { return c.Peers[i].Count > c.Peers[j].Count })
		origin := m.ClockOriginUnixNs()
		for _, e := range rep.Clock.Events {
			c.Events = append(c.Events, mlatEventDTO{T: (origin + int64(e.T*1e9)) / 1e6, Reason: e.Reason, Detail: e.Detail})
		}
		out.Clock = c
		synced := 0
		for _, p := range peers {
			if pr, ok := m.Station(p.Peer); ok && pr.Clock != nil && pr.Clock.Synced {
				synced++
			}
		}
		if !rep.Clock.Synced {
			synced = 0 // an unsynced clock is synced with nobody
		}
		out.Summary.Peers, out.Summary.SyncedPeers = len(c.Peers), max(rep.Clock.SyncedPeers, synced)
	}
	var res uint64
	reported := false
	for _, l := range s.liveLinks() {
		if l.Station == st.Idx && l.Results != nil {
			res += *l.Results
			reported = true
		}
	}
	if reported {
		out.Summary.ResultsReceived = &res
	}
	if pts := s.mlatHist.station(st.Idx); len(pts) > 0 {
		h := stHistDTO(pts)
		if !admin {
			coarsenDrift(h.DriftPPM)
		}
		out.History = &h
	}
	return out, nil
}

// mergedPeers returns the station's sync peers in both directions. The
// clock filter records a co-observation under the member whose residual
// was taken against a reference peer; a station that mostly serves as the
// reference would otherwise list no peers although others sync against it.
// Edges where the station is the reference are mirrored (relative
// frequency negated); a pair seen in both directions keeps the direction
// with more co-observations.
func mergedPeers(m mlatSource, self model.StationIdx, own []clock.PeerStat) []clock.PeerStat {
	by := map[model.StationIdx]clock.PeerStat{}
	for _, p := range own {
		if p.Count > 0 && p.Peer != self {
			by[p.Peer] = p
		}
	}
	for _, e := range m.SyncMatrix() {
		if e.B != self || e.A == self || e.Count == 0 {
			continue
		}
		if cur, ok := by[e.A]; ok && cur.Count >= e.Count {
			continue
		}
		by[e.A] = clock.PeerStat{Peer: e.A, Count: e.Count, RMSNs: e.RMSNs, RelPPB: -e.RelPPB, SameDom: e.SameDomain}
	}
	out := make([]clock.PeerStat, 0, len(by))
	for _, p := range by {
		out = append(out, p)
	}
	return out
}

// coarsenDrift applies ownerDriftPPM to a drift series in place.
func coarsenDrift(vs []*float64) {
	for _, v := range vs {
		if v != nil {
			*v = ownerDriftPPM(*v)
		}
	}
}

func mlatPeer(reg *registry.Registry, viewer *registry.Station, p clock.PeerStat, admin bool, now time.Time, ref geoPoint, refOK bool) mlatPeerDTO {
	d := mlatPeerDTO{Count: p.Count, RMSNs: rnd1(p.RMSNs), SameDom: p.SameDom}
	sid := "?"
	if ps, ok := reg.Station(p.Peer); ok {
		sid = ps.SID
		if refOK {
			d.Band = distBand(ref.lat, ref.lon, &ps)
		}
	}
	if admin {
		d.SID = sid
		rel := rnd1(p.RelPPB)
		d.RelPPB = &rel
	} else {
		d.Tag = reg.PeerTag(viewer.SID, sid, now)
	}
	return d
}

func apiMeStationMLAT(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	return s.mlatStation(reg, &st, false)
}

func apiAdminStationMLAT(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	return s.mlatStation(reg, &st, true)
}

// ----------------------------------------------------------------- admin

func apiAdminMLAT(s *Server, r *http.Request) (any, error) {
	m := s.mlatSrc()
	if m == nil {
		return mlatOff, nil
	}
	return m.Stats(), nil
}

func apiAdminMLATQuality(s *Server, r *http.Request) (any, error) {
	m := s.mlatSrc()
	if m == nil {
		return mlatOff, nil
	}
	window := 60
	if v := r.URL.Query().Get("window"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10080 {
			return nil, badRequest("window: minutes 1..10080")
		}
		window = n
	}
	return m.QualityReport(window, r.URL.Query().Get("cells") == "1"), nil
}

func apiAdminMLATHistory(s *Server, r *http.Request) (any, error) {
	if s.mlatSrc() == nil {
		return mlatOff, nil
	}
	minutes := 360
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > mlatHistNet {
			return nil, badRequest("minutes: 1..%d", mlatHistNet)
		}
		minutes = n
	}
	return historyDTO(s.mlatHist.points(time.Now(), minutes)), nil
}

// mlatStationRowDTO is one row of the admin station table: the clock state
// and the residual statistics of a station, without position.
type mlatStationRowDTO struct {
	SID            string  `json:"sid"`
	Name           string  `json:"name,omitempty"`
	Kind           string  `json:"kind"`
	Admission      string  `json:"admission"`
	PositionSource string  `json:"positionSource"`
	Class          string  `json:"clockClass,omitempty"`
	AnchorState    string  `json:"anchorState,omitempty"`
	AnchorReason   string  `json:"anchorReason,omitempty"`
	Synced         bool    `json:"synced"`
	RelativeTime   bool    `json:"relativeTime"`
	SigmaThetaNs   float64 `json:"sigmaThetaNs"`
	DriftPPM       float64 `json:"driftPpm"`
	BetaShortNs    float64 `json:"betaShortNs"`
	JitterNs       float64 `json:"jitterNs"`
	ClockOutlier   float64 `json:"clockOutlierPct"`
	Steps          int     `json:"steps"`
	Peers          int     `json:"peers"`
	SyncedPeers    int     `json:"syncedPeers"`
	HasClock       bool    `json:"hasClock"`
	HasMLAT        bool    `json:"hasMlat"`
	ContribPerHour int     `json:"contribPerHour"`
	UsedPct        float64 `json:"usedPct"`
	ExcludedPct    float64 `json:"excludedPct"`
	OutlierPct     float64 `json:"outlierPct"`
	ResidualBiasNs float64 `json:"residualBiasNs"`
	WScale         float64 `json:"wScale"`
	LearnedJitter  float64 `json:"learnedJitterNs"`
	BiasFlag       bool    `json:"biasFlag"`
	TimedPerMin    int64   `json:"timedPerMin"`
	ResultsElig    bool    `json:"resultsEligible"`
}

func apiAdminMLATStations(s *Server, r *http.Request) (any, error) {
	m := s.mlatSrc()
	if m == nil {
		return mlatOff, nil
	}
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	rows := []mlatStationRowDTO{}
	for _, st := range reg.List(registry.Filter{}) {
		rep, ok := m.Station(st.Idx)
		if !ok {
			continue
		}
		row := mlatStationRowDTO{SID: st.SID, Name: st.Name, Kind: st.Kind, Admission: st.Admission,
			PositionSource: rep.PositionSource, TimedPerMin: rep.TimedPerMin, ResultsElig: rep.ResultsEligible}
		if c := rep.Clock; c != nil {
			row.HasClock = true
			row.Class, row.AnchorState, row.AnchorReason = c.Class, c.AnchorState, c.AnchorReason
			row.Synced, row.RelativeTime = c.Synced, c.RelativeTime
			row.SigmaThetaNs, row.DriftPPM, row.BetaShortNs = rnd1(c.SigmaThetaNs), math.Round(fin(c.DriftPPM)*1e4)/1e4, rnd1(c.BetaShortNs)
			row.JitterNs, row.ClockOutlier, row.Steps = rnd1(c.JitterNs), rnd1(c.OutlierPct), c.Steps
			row.SyncedPeers = c.SyncedPeers
			for _, p := range c.Peers {
				if p.Count > 0 {
					row.Peers++
				}
			}
		}
		if q := rep.MLAT; q != nil {
			row.HasMLAT = true
			row.ContribPerHour, row.UsedPct, row.ExcludedPct, row.OutlierPct = q.ContribPerHour, rnd1(q.UsedPct), rnd1(q.ExcludedPct), rnd1(q.OutlierPct)
			row.ResidualBiasNs, row.WScale, row.LearnedJitter, row.BiasFlag = rnd1(q.ResidualBiasNs), round2(q.WScale), rnd1(q.LearnedJitterNs), q.BiasFlag
		}
		rows = append(rows, row)
	}
	return map[string]any{"running": true, "updated": time.Now().UnixMilli(), "stations": rows}, nil
}

// mlatSyncEdgeDTO is one entry of the admin sync matrix (stations by SID).
type mlatSyncEdgeDTO struct {
	A          string  `json:"a"`
	B          string  `json:"b"`
	Count      int     `json:"count"`
	RMSNs      float64 `json:"rmsNs"`
	RelPPB     float64 `json:"relPpb"`
	SameDomain bool    `json:"sameDomain"`
}

// mlatSyncStationDTO names a matrix axis entry.
type mlatSyncStationDTO struct {
	SID    string `json:"sid"`
	Name   string `json:"name,omitempty"`
	Class  string `json:"clockClass,omitempty"`
	Anchor string `json:"anchorState,omitempty"`
	Synced bool   `json:"synced"`
}

func apiAdminMLATSync(s *Server, r *http.Request) (any, error) {
	m := s.mlatSrc()
	if m == nil {
		return mlatOff, nil
	}
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	sids := map[model.StationIdx]string{}
	axis := []mlatSyncStationDTO{}
	sid := func(idx model.StationIdx) string {
		if v, ok := sids[idx]; ok {
			return v
		}
		v := "?"
		d := mlatSyncStationDTO{}
		if st, ok := reg.Station(idx); ok {
			v = st.SID
			d.Name = st.Name
		}
		d.SID = v
		if rep, ok := m.Station(idx); ok && rep.Clock != nil {
			d.Class, d.Anchor, d.Synced = rep.Clock.Class, rep.Clock.AnchorState, rep.Clock.Synced
		}
		sids[idx] = v
		axis = append(axis, d)
		return v
	}
	edges := m.SyncMatrix()
	out := make([]mlatSyncEdgeDTO, 0, len(edges))
	for _, e := range edges {
		out = append(out, mlatSyncEdgeDTO{A: sid(e.A), B: sid(e.B), Count: e.Count, RMSNs: rnd1(e.RMSNs), RelPPB: rnd1(e.RelPPB), SameDomain: e.SameDomain})
	}
	slices.SortFunc(axis, func(a, b mlatSyncStationDTO) int {
		if a.SID < b.SID {
			return -1
		}
		if a.SID > b.SID {
			return 1
		}
		return 0
	})
	return map[string]any{"running": true, "stations": axis, "edges": out}, nil
}

// crlbCache keeps the CPU-heavy CRLB map for crlbTTL.
type crlbCache struct {
	mu      sync.Mutex
	at      time.Time
	key     any
	cells   []quality.CRLBCell
	summary []crlbSummaryDTO
	heights []quality.CRLBSummary // per-height P50/P95 (quality.SummarizeCRLB)
}

const crlbTTL = 10 * time.Minute

// crlbSummaryDTO summarises the CRLB map at one target height: how much
// area the network covers at each accuracy grade (DESIGN §9.4 grades: A
// σ_h ≤ 50 m, B ≤ 150 m, C ≤ 500 m, D ≤ 2 km). A 0.25° cell at latitude φ
// covers (0.25·111.32)² · cos φ km² (spherical; < 0.5 % from the
// ellipsoid at these sizes).
type crlbSummaryDTO struct {
	AltM      float64 `json:"altM"`
	Cells     int     `json:"cells"`
	P50SigmaM float64 `json:"p50SigmaM"`
	P90SigmaM float64 `json:"p90SigmaM"`
	MinSigmaM float64 `json:"minSigmaM"`
	AreaKm2A  float64 `json:"areaKm2A"`
	AreaKm2B  float64 `json:"areaKm2B"`
	AreaKm2C  float64 `json:"areaKm2C"`
	AreaKm2D  float64 `json:"areaKm2D"`
}

// crlbSummary groups the cells by height; step is the grid step in degrees.
func crlbSummary(cells []quality.CRLBCell, step float64) []crlbSummaryDTO {
	by := map[float64][]quality.CRLBCell{}
	for _, c := range cells {
		by[c.AltM] = append(by[c.AltM], c)
	}
	alts := make([]float64, 0, len(by))
	for a := range by {
		alts = append(alts, a)
	}
	sort.Float64s(alts)
	side := step * 111.32
	out := make([]crlbSummaryDTO, 0, len(alts))
	for _, a := range alts {
		cs := by[a]
		sig := make([]float64, len(cs))
		d := crlbSummaryDTO{AltM: a, Cells: len(cs)}
		for i, c := range cs {
			sig[i] = c.SigmaHM
			area := side * side * math.Cos(c.Lat*math.Pi/180)
			switch {
			case c.SigmaHM <= 50:
				d.AreaKm2A += area
			case c.SigmaHM <= 150:
				d.AreaKm2B += area
			case c.SigmaHM <= 500:
				d.AreaKm2C += area
			default:
				d.AreaKm2D += area
			}
		}
		sort.Float64s(sig)
		q := func(p float64) float64 { return rnd1(sig[min(len(sig)-1, int(p*float64(len(sig))))]) }
		d.P50SigmaM, d.P90SigmaM, d.MinSigmaM = q(0.5), q(0.9), rnd1(sig[0])
		d.AreaKm2A, d.AreaKm2B, d.AreaKm2C, d.AreaKm2D = math.Round(d.AreaKm2A), math.Round(d.AreaKm2B), math.Round(d.AreaKm2C), math.Round(d.AreaKm2D)
		out = append(out, d)
	}
	return out
}

func apiAdminMLATCRLB(s *Server, r *http.Request) (any, error) {
	m := s.mlatSrc()
	if m == nil {
		return mlatOff, nil
	}
	c := &s.crlb
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cells == nil || c.key != m.Key() || time.Since(c.at) > crlbTTL || r.URL.Query().Get("refresh") == "1" {
		// Workers: the station choice per target is the planner's greedy
		// D-optimal design (WP-Q2), 1,000 stations in about 0.1 s.
		c.cells = m.CRLBMap(quality.CRLBParams{Workers: 4})
		if c.cells == nil {
			c.cells = []quality.CRLBCell{}
		}
		c.summary = crlbSummary(c.cells, 0.25)
		c.heights = quality.SummarizeCRLB(c.cells)
		c.at, c.key = time.Now(), m.Key()
	}
	return map[string]any{"running": true, "computed": c.at.UnixMilli(), "stepDeg": 0.25, "summary": c.summary,
		"heights": c.heights, "cells": c.cells}, nil
}
