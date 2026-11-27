package server

import (
	"context"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"airview/internal/mlat/plan"
	"airview/internal/model"
)

// ADS-B position verification alerts (phase 4, WP-Q2 item 3; DESIGN §9.2
// "an aircraft biased across ≥ 3 station pairs ... catches bad NACp and
// spoofing", §9.6). The MLAT engine checks ADS-B positions against the
// TDOAs of synced stations; an aircraft whose positions keep contradicting
// them becomes "suspect" for 10 minutes and the planner multilaterates it.
//
// This file turns that into an operator view: the counters, the aircraft
// that are suspect now (with the position they broadcast and, when the
// network has one, the MLAT position) and a log of the last
// verifyEventsMax transitions (became suspect / cleared). Admin only: a
// suspicion is an internal quality signal, not a public accusation, and
// the view names stations nowhere.
//
// The watcher samples every verifyEvery while the MLAT engine runs (and on
// demand when the route is called), reading the planner's priority class
// of each live ADS-B aircraft (plan.PrioSuspect). An aircraft that is also
// in the hold-out set shows as hold-out there and is not listed.

const (
	verifyEvery     = 10 * time.Second
	verifyMinScan   = 3 * time.Second
	verifyEventsMax = 200
)

// suspectSource is the optional MLAT view of the suspect state (the real
// engine; test fakes may implement it).
type suspectSource interface {
	Suspect(key uint32, nowNs int64) bool
}

func (e engineMLAT) Suspect(key uint32, nowNs int64) bool {
	p := e.Planner()
	return p != nil && p.Prio(key, nowNs) == plan.PrioSuspect
}

// verifyAircraft is one currently suspect aircraft.
type verifyAircraft struct {
	ICAO     string   `json:"icao"`
	Callsign string   `json:"callsign,omitempty"`
	Since    int64    `json:"since"` // Unix ms, first seen suspect by the watcher
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	AltFt    *float64 `json:"altFt,omitempty"`
	Source   string   `json:"source,omitempty"`
	NACp     *int     `json:"nacp,omitempty"`
	// MLAT: the aircraft currently has an MLAT position (the picture then
	// shows the network's position next to the broadcast one).
	MLAT bool `json:"mlat"`
}

// verifyEvent is one transition.
type verifyEvent struct {
	T        int64    `json:"t"`
	Kind     string   `json:"kind"` // suspect | cleared
	ICAO     string   `json:"icao"`
	Callsign string   `json:"callsign,omitempty"`
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	AltFt    *float64 `json:"altFt,omitempty"`
	// DurationS: how long it was suspect (cleared events).
	DurationS int64 `json:"durationS,omitempty"`
}

type verifyWatch struct {
	mu       sync.Mutex
	key      any
	scanned  time.Time
	current  map[string]*verifyAircraft
	events   []verifyEvent // oldest first, at most verifyEventsMax
	scans    uint64
	newCount uint64 // transitions to suspect since the server started
}

func round4p(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := math.Round(*p*1e4) / 1e4
	return &v
}

// scanVerify samples the suspect state now (rate-limited to verifyMinScan
// unless force).
func (s *Server) scanVerify(now time.Time, force bool) {
	m := s.mlatSrc()
	w := &s.verify
	w.mu.Lock()
	defer w.mu.Unlock()
	if !force && now.Sub(w.scanned) < verifyMinScan {
		return
	}
	w.scanned = now
	sp, ok := m.(suspectSource)
	if m == nil || !ok || s.eng == nil {
		// Engine gone: everything ends without a "cleared" story.
		w.current = nil
		return
	}
	if w.key != m.Key() {
		w.key, w.current = m.Key(), nil
	}
	if w.current == nil {
		w.current = map[string]*verifyAircraft{}
	}
	w.scans++
	nowNs := now.UnixNano()
	seen := map[string]bool{}
	snap := s.eng.Tracker().Snapshot()
	for i := range snap.Aircraft {
		a := &snap.Aircraft[i]
		if strings.HasPrefix(a.ICAO, "~") || strings.HasPrefix(a.ICAO, "#") {
			continue
		}
		key, err := parseICAOParam(a.ICAO)
		if err != nil || !sp.Suspect(key, nowNs) {
			continue
		}
		seen[a.ICAO] = true
		alt := a.AltBaro
		if alt == nil {
			alt = a.AltGeom
		}
		cur := w.current[a.ICAO]
		if cur == nil {
			cur = &verifyAircraft{ICAO: a.ICAO, Since: now.UnixMilli()}
			w.current[a.ICAO] = cur
			w.newCount++
			w.push(verifyEvent{T: now.UnixMilli(), Kind: "suspect", ICAO: a.ICAO, Callsign: strings.TrimSpace(a.Callsign),
				Lat: round4p(a.Lat), Lon: round4p(a.Lon), AltFt: alt})
		}
		cur.Callsign = strings.TrimSpace(a.Callsign)
		cur.Lat, cur.Lon, cur.AltFt = round4p(a.Lat), round4p(a.Lon), alt
		cur.Source = string(a.Source)
		cur.NACp = a.NACp
		cur.MLAT = strings.HasPrefix(string(a.Source), model.SrcMLAT)
	}
	for icao, cur := range w.current {
		if seen[icao] {
			continue
		}
		delete(w.current, icao)
		w.push(verifyEvent{T: now.UnixMilli(), Kind: "cleared", ICAO: icao, Callsign: cur.Callsign, Lat: cur.Lat, Lon: cur.Lon,
			AltFt: cur.AltFt, DurationS: (now.UnixMilli() - cur.Since) / 1000})
	}
}

func (w *verifyWatch) push(e verifyEvent) {
	if len(w.events) >= verifyEventsMax {
		copy(w.events, w.events[1:])
		w.events = w.events[:len(w.events)-1]
	}
	w.events = append(w.events, e)
}

// runVerify samples while the server runs (aggregator mode with MLAT).
func (s *Server) runVerify(ctx context.Context) {
	t := time.NewTicker(verifyEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if s.aggregatorMode() && s.mlatSrc() != nil {
				s.scanVerify(now, false)
			}
		}
	}
}

func init() {
	routes = append(routes, Route{Method: "GET", Path: "/api/admin/mlat/verification", Group: grpNetAdm, Access: AccessAdmin, NetOnly: true,
		API: apiAdminVerification, NoAuto: true,
		Summary: "ADS-B position verification: counters (confirmed, contradicted, suspects, contradictions charged to a station, contradictions muted by the network-wide breaker, recovered clock-filter and engine panics), the aircraft whose broadcast positions are suspect now, and the last 200 transitions.",
		Returns: "`{\"running\":bool,\"counters\":{...},\"current\":[{icao,callsign,since,lat,lon,altFt,source,nacp,mlat}],\"events\":[{t,kind,icao,...}],\"newSinceStart\":n}`",
		Notes:   "Admin only (an internal quality signal, never public). The list follows the planner's `suspect` class (10 minutes after the last contradiction)."})
}

func apiAdminVerification(s *Server, r *http.Request) (any, error) {
	m := s.mlatSrc()
	if m == nil {
		return mlatOff, nil
	}
	now := time.Now()
	s.scanVerify(now, false)
	st := m.Stats()
	w := &s.verify
	w.mu.Lock()
	cur := make([]verifyAircraft, 0, len(w.current))
	for _, a := range w.current {
		cur = append(cur, *a)
	}
	ev := slices.Clone(w.events)
	newCount := w.newCount
	w.mu.Unlock()
	slices.SortFunc(cur, func(a, b verifyAircraft) int {
		if a.Since != b.Since {
			if a.Since > b.Since {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ICAO, b.ICAO)
	})
	slices.Reverse(ev) // newest first
	total := st.ADSBConfirmed + st.ADSBContradicted
	rate := 0.0
	if total > 0 {
		rate = float64(st.ADSBContradicted) / float64(total)
	}
	return map[string]any{
		"running": true,
		"at":      unixMs(now),
		"counters": map[string]any{
			"confirmed": st.ADSBConfirmed, "contradicted": st.ADSBContradicted, "suspects": st.ADSBSuspects,
			"blamed": st.ADSBBlamed, "culprit": st.ADSBCulprit, "contradictedShare": rnd(rate, 5),
			"muted": st.ADSBMuted, "clockPanicResets": st.Clock.PanicResets, "advancePanics": st.AdvancePanics,
		},
		"current":       cur,
		"events":        ev,
		"newSinceStart": newCount,
	}, nil
}
