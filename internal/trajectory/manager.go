package trajectory

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const nShards = 32

type shard struct {
	mu   sync.Mutex
	m    map[uint32]*track
	free []*track
}

type rxInfo struct {
	lat, lon float64
}

type rangeHolder struct{ f RangeFunc }
type runwayHolder struct{ p RunwayProvider }

// Manager estimates and predicts the trajectories of all tracked aircraft.
// All methods are safe for concurrent use. Observe does not allocate once an
// aircraft is known.
type Manager struct {
	cfg     Config
	env     env
	cnt     counters
	shards  [nShards]shard
	n       atomic.Int64
	rx      atomic.Pointer[rxInfo]
	rangeFn atomic.Pointer[rangeHolder]
	runways atomic.Pointer[runwayHolder]

	proxMu sync.Mutex
	prox   proxScratch
}

// NewManager creates a Manager. Zero Config fields take DefaultConfig values.
func NewManager(cfg Config) *Manager {
	cfg = cfg.withDefaults()
	m := &Manager{cfg: cfg}
	m.env.p = newParams(cfg)
	m.env.cnt = &m.cnt
	m.env.sc = newScorer(cfg.StatsWindow)
	for i := range m.shards {
		m.shards[i].m = make(map[uint32]*track, 64)
	}
	return m
}

// Config returns the effective configuration.
func (m *Manager) Config() Config { return m.cfg }

func unixSec(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())*1e-9
}

func msOf(t float64) int64 { return int64(math.Round(t * 1000)) }

func (m *Manager) shard(icao uint32) *shard {
	h := icao * 2654435761
	return &m.shards[h>>27]
}

// Observe feeds one observation of aircraft icao (24-bit address; bit 24 set
// = non-ICAO). Call it for every decoded position, velocity, altitude and
// Comm-B (BDS 4,0 / 5,0 / 6,0) update. Zero allocations in steady state.
func (m *Manager) Observe(icao uint32, o *Obs) {
	if o == nil || o.T.IsZero() || o.Has == 0 {
		return
	}
	m.cnt.obs.Add(1)
	t := unixSec(o.T)
	sh := m.shard(icao)
	sh.mu.Lock()
	tr := sh.m[icao]
	if tr == nil {
		if o.Has&(HasPos|HasVel|HasAlt) == 0 {
			sh.mu.Unlock()
			return
		}
		if m.n.Load() >= int64(m.cfg.MaxTracks) {
			sh.mu.Unlock()
			m.cnt.overflow.Add(1)
			return
		}
		if k := len(sh.free); k > 0 {
			tr = sh.free[k-1]
			sh.free = sh.free[:k-1]
		} else {
			tr = new(track)
		}
		tr.reset(icao)
		sh.m[icao] = tr
		m.n.Add(1)
	}
	tr.observe(t, o, &m.env)
	sh.mu.Unlock()
}

// Len returns the number of tracked aircraft.
func (m *Manager) Len() int { return int(m.n.Load()) }

func (m *Manager) remove(sh *shard, icao uint32, tr *track) {
	delete(sh.m, icao)
	m.n.Add(-1)
	if len(sh.free) < 32 {
		sh.free = append(sh.free, tr)
	}
}

// Drop forgets an aircraft.
func (m *Manager) Drop(icao uint32) {
	sh := m.shard(icao)
	sh.mu.Lock()
	if tr := sh.m[icao]; tr != nil {
		m.remove(sh, icao, tr)
	}
	sh.mu.Unlock()
}

// GC removes aircraft not observed for StaleAfter; returns how many.
func (m *Manager) GC(now time.Time) int {
	t := unixSec(now)
	lim := m.cfg.StaleAfter.Seconds()
	n := 0
	for i := range m.shards {
		sh := &m.shards[i]
		sh.mu.Lock()
		for icao, tr := range sh.m {
			if t-tr.lastSeen > lim {
				m.remove(sh, icao, tr)
				n++
			}
		}
		sh.mu.Unlock()
	}
	return n
}

// Run garbage-collects stale aircraft every 10 s until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	tk := time.NewTicker(10 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
			if n := m.GC(m.cfg.Now()); n > 0 {
				slog.Debug("trajectory: dropped stale aircraft", "n", n, "tracks", m.Len())
			}
		}
	}
}

// SetReceiver sets the receiver location used for CPA / exit predictions.
func (m *Manager) SetReceiver(lat, lon float64) {
	if !finite(lat) || !finite(lon) || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		m.rx.Store(nil)
		return
	}
	m.rx.Store(&rxInfo{lat, lon})
}

// ClearReceiver forgets the receiver location.
func (m *Manager) ClearReceiver() { m.rx.Store(nil) }

// SetRangeFunc sets the coverage function used for exit-time prediction (nil clears).
func (m *Manager) SetRangeFunc(f RangeFunc) {
	if f == nil {
		m.rangeFn.Store(nil)
		return
	}
	m.rangeFn.Store(&rangeHolder{f})
}

// SetRunways sets the optional runway provider for approach detection (nil clears).
func (m *Manager) SetRunways(p RunwayProvider) {
	if p == nil {
		m.runways.Store(nil)
		return
	}
	m.runways.Store(&runwayHolder{p})
}

// SetWind sets an optional wind provider, queried at most once a minute per
// aircraft when observations carry no wind (nil clears).
func (m *Manager) SetWind(w WindProvider) {
	if w == nil {
		m.env.wind.Store(nil)
		return
	}
	m.env.wind.Store(&windHolder{w})
}

func confidence(age, sigH float64, src Source) float64 {
	c := math.Exp(-math.Max(age, 0)/30) / (1 + sigH/2000)
	if src == SrcMLAT || src == SrcTISB {
		c *= 0.9
	}
	return clamp(fin(c), 0, 1)
}

// predictLocked builds a prediction at time t; ok=false if the aircraft has
// no usable estimate or t is beyond MaxCoast after the last position.
func (m *Manager) predictLocked(tr *track, t float64) (pstate, float64, bool) {
	p := &m.env.p
	if !tr.ready() {
		return pstate{}, 0, false
	}
	age := t - tr.lastAccPosT
	if age > p.maxCoast {
		return pstate{}, 0, false
	}
	ps := tr.pstate(p)
	return ps, age, true
}

func (m *Manager) fillPrediction(tr *track, ps *pstate, t, age float64) Prediction {
	p := &m.env.p
	ps.propagate(t-ps.t, p, true)
	o := ps.output(p)
	pr := Prediction{
		ICAO: tr.hex, T: msOf(t), Lat: o.lat, Lon: o.lon, AltOK: ps.vOK, AltFt: o.alt,
		TrackDeg: o.trk, GSKt: o.gsKt, VRateFpm: o.vrFpm, TurnDps: o.turnDps,
		SigmaHM: o.sigH, SigmaVFt: o.sigV, Ellipse: o.ell, AgeSec: fin(math.Max(age, 0)),
		Coasting: age > p.coastAfter, PTurn: fin(ps.muCT), OnGround: tr.ground, Source: tr.src.String(),
	}
	pr.Model = "cv"
	if ps.muCT > 0.5 {
		pr.Model = "ct"
	}
	pr.Confidence = confidence(age, o.sigF, tr.src)
	return pr
}

// Predict returns the predicted state of one aircraft at time at.
func (m *Manager) Predict(icao uint32, at time.Time) (Prediction, bool) {
	t := unixSec(at)
	sh := m.shard(icao)
	sh.mu.Lock()
	tr := sh.m[icao]
	if tr == nil {
		sh.mu.Unlock()
		return Prediction{}, false
	}
	ps, age, ok := m.predictLocked(tr, t)
	if !ok {
		sh.mu.Unlock()
		return Prediction{}, false
	}
	pr := m.fillPrediction(tr, &ps, t, age)
	sh.mu.Unlock()
	return pr, true
}

// Predicted appends the predicted position at now of every aircraft whose last
// position is at most MaxCoast old (Coasting marks those older than
// CoastAfter) and returns dst. Pass dst[:0] of a reused slice to avoid allocation.
func (m *Manager) Predicted(now time.Time, dst []Predicted) []Predicted {
	t := unixSec(now)
	p := &m.env.p
	for i := range m.shards {
		sh := &m.shards[i]
		sh.mu.Lock()
		for _, tr := range sh.m {
			ps, age, ok := m.predictLocked(tr, t)
			if !ok {
				continue
			}
			ps.propagate(t-ps.t, p, true)
			o := ps.output(p)
			e := Predicted{ICAO: tr.hex, Lat: o.lat, Lon: o.lon, TrackDeg: o.trk, GSKt: o.gsKt,
				VRateFpm: o.vrFpm, SigmaHM: o.sigH, AgeSec: fin(math.Max(age, 0)), Coasting: age > p.coastAfter}
			if ps.vOK {
				e.AltFt = o.alt
			}
			e.Confidence = confidence(age, o.sigF, tr.src)
			dst = append(dst, e)
		}
		sh.mu.Unlock()
	}
	return dst
}

// PathFor returns the predicted path from now to now+horizon in step
// increments (horizon capped at 900 s, at most 301 points).
func (m *Manager) PathFor(icao uint32, now time.Time, horizon, step time.Duration) (Path, bool) {
	t := unixSec(now)
	p := &m.env.p
	sh := m.shard(icao)
	sh.mu.Lock()
	tr := sh.m[icao]
	if tr == nil {
		sh.mu.Unlock()
		return Path{}, false
	}
	ps, _, ok := m.predictLocked(tr, t)
	hex := tr.hex
	sh.mu.Unlock()
	if !ok {
		return Path{}, false
	}
	hs := clamp(horizon.Seconds(), 0, 900)
	ss := step.Seconds()
	if !(ss > 0) {
		ss = 10
	}
	if hs/ss > 300 {
		ss = hs / 300
	}
	ps.propagate(t-ps.t, p, true)
	ps.recenter()
	path := Path{ICAO: hex, T0: msOf(t), AltOK: ps.vOK, Plan: ps.plan.String()}
	n := int(math.Floor(hs/ss+1e-9)) + 1
	path.Points = make([]PathPoint, 0, n)
	for k := 0; k < n; k++ {
		if k > 0 {
			ps.propagate(ss, p, true)
		}
		o := ps.output(p)
		path.Points = append(path.Points, PathPoint{DT: float64(k) * ss, Lat: o.lat, Lon: o.lon, AltFt: o.alt,
			SigmaHM: o.sigH, SigmaFM: o.sigF, SigmaVFt: o.sigV, Ellipse: o.ell})
	}
	return path, true
}

// Intent returns the intent-aware analysis of one aircraft at now.
func (m *Manager) Intent(icao uint32, now time.Time) (Intent, bool) {
	t := unixSec(now)
	p := &m.env.p
	sh := m.shard(icao)
	sh.mu.Lock()
	tr := sh.m[icao]
	if tr == nil {
		sh.mu.Unlock()
		return Intent{}, false
	}
	ps, _, ok := m.predictLocked(tr, t)
	if !ok {
		sh.mu.Unlock()
		return Intent{}, false
	}
	hold := tr.hold
	holdOK := hold.ok && tr.lastSeen-tr.holdT < 60
	ground := tr.ground
	navApp := tr.nav&NavApproach != 0 && tr.lastSeen-tr.navT < navFresh
	var selTrk *float64
	if tr.selHdgT > 0 && tr.lastSeen-tr.selHdgT < selHdgFresh {
		s := tr.selHdg
		if tr.magOffOK {
			s = normDeg(s + tr.magOff)
		}
		selTrk = &s
	}
	hex := tr.hex
	sh.mu.Unlock()

	ps.propagate(t-ps.t, p, false)
	ps.recenter()
	o := ps.output(p)
	in := Intent{ICAO: hex, T: msOf(t), Plan: ps.plan.String(), SelTrackDeg: selTrk}
	if holdOK {
		d := "right"
		if hold.w < 0 {
			d = "left"
		}
		k := "hold"
		if hold.orbit {
			k = "orbit"
		}
		in.Holding = &HoldInfo{Kind: k, Direction: d, CenterLat: hold.cLat, CenterLon: hold.cLon,
			RadiusKm: fin(hold.radiusKm), LegCourse: fin(hold.legCourse), LegSec: fin(hold.legSec),
			TurnDps: fin(math.Abs(hold.w) * rad2deg), Laps: fin(hold.laps)}
	}
	if rh := m.runways.Load(); rh != nil && rh.p != nil && !ground && ps.vOK {
		var buf [16]Runway
		rws := rh.p.RunwaysNear(o.lat, o.lon, 45, buf[:0])
		in.Approach = detectApproach(o.lat, o.lon, o.trk, o.gsKt*ktToMS, o.alt, o.vrFpm, ps.accel, navApp, rws)
		if in.Approach != nil {
			ps.floorFt = o.alt - in.Approach.HeightAGLFt // threshold elevation
		}
	}
	in.LevelOff = levelOff(&ps, p)
	if ps.vOK && math.Abs(o.vrFpm) < 300 && ps.selOK && !ground {
		if ps.selAlt < o.alt-900 {
			in.DescentPending = true
		} else if ps.selAlt > o.alt+900 {
			in.ClimbPending = true
		}
	}
	if rx := m.rx.Load(); rx != nil {
		tc, d, best := cpaToPoint(ps, p, rx.lat, rx.lon, 1800)
		bo := best.output(p)
		in.CPA = &CPA{InSec: tc, T: msOf(t + tc), DistKm: fin(d / 1000), Lat: bo.lat, Lon: bo.lon, AltFt: bo.alt}
		if rf := m.rangeFn.Load(); rf != nil && rf.f != nil {
			if es, ok := exitTime(ps, p, rx.lat, rx.lon, rf.f, 3600); ok {
				v := fin(es)
				in.ExitSec = &v
			}
		}
	}
	switch {
	case ground:
		in.Phase = "ground"
	case holdOK:
		in.Phase = "holding"
	case in.Approach != nil || (navApp && o.vrFpm < -200):
		in.Phase = "approach"
	case math.Abs(o.turnDps) > 1 && ps.muCT > 0.5:
		in.Phase = "turning"
	case !ps.vOK:
		in.Phase = "unknown"
	case o.vrFpm > 300:
		in.Phase = "climb"
	case o.vrFpm < -300:
		in.Phase = "descent"
	default:
		in.Phase = "cruise"
	}
	return in, true
}

// Stats returns ingest counters and the rolling prediction-error statistics.
func (m *Manager) Stats(now time.Time) Stats {
	t := unixSec(now)
	p := &m.env.p
	st := Stats{Now: msOf(t)}
	for i := range m.shards {
		sh := &m.shards[i]
		sh.mu.Lock()
		for _, tr := range sh.m {
			st.Tracks++
			if tr.ready() {
				st.Ready++
				age := t - tr.lastAccPosT
				if age > p.coastAfter && age <= p.maxCoast {
					st.Coasting++
				}
			}
		}
		sh.mu.Unlock()
	}
	c := &m.cnt
	st.Counters = Counters{
		Observations: c.obs.Load(), Positions: c.positions.Load(), Rejected: c.rejected.Load(),
		VelRejected: c.velRejected.Load(), AltRejected: c.altRejected.Load(), OutOfOrder: c.outOfOrder.Load(),
		OOODropped: c.oooDropped.Load(), Duplicates: c.duplicates.Load(), Resets: c.resets.Load(),
		Inits: c.inits.Load(), Overflow: c.overflow.Load(),
	}
	st.Horizons, st.Skill = m.env.sc.summarize()
	if st.Horizons == nil {
		st.Horizons = []HorizonStats{}
	}
	if st.Skill == nil {
		st.Skill = []Skill{}
	}
	return st
}
