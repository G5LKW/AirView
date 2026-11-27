package mlat

import (
	"math"
	"sync"
	"sync/atomic"

	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/group"
	"airview/internal/mlat/plan"
	"airview/internal/mlat/track"
	"airview/internal/model"
)

// clockAdapter implements group.Clock, group.CrossCov and group.Domains on
// one pinned clock snapshot. Each worker owns one and pins the latest
// snapshot at the start of every group, so the assembler and the solver of
// that group see the same clock states.
type clockAdapter struct {
	snap *clock.Snapshot
}

var (
	_ group.Clock    = (*clockAdapter)(nil)
	_ group.CrossCov = (*clockAdapter)(nil)
	_ group.Domains  = (*clockAdapter)(nil)
)

// NetTime implements group.Clock. The snapshot's float64 network seconds
// are split into whole Unix ns plus a fraction (clock.NetTimeUnix), so no
// precision is lost however long the server runs.
func (a *clockAdapter) NetTime(st model.StationIdx, ep uint32, ticks uint64, kind model.FrameKind) (group.Time, float64, float64, bool) {
	ns, frac, cv, jv, ok := a.snap.NetTimeUnix(clock.Query{Station: st, Epoch: ep, Ticks: ticks, Kind: kind})
	if !ok {
		return group.Time{}, 0, 0, false
	}
	return group.MakeTime(ns, frac), cv, jv, true
}

// CrossCov implements group.CrossCov.
func (a *clockAdapter) CrossCov(x, y model.StationIdx) float64 { return a.snap.CrossCov(x, y) }

// Domain implements group.Domains. A station without a component gets a
// domain of its own (it is unsynced and never converted anyway).
func (a *clockAdapter) Domain(st model.StationIdx) uint64 {
	if d, ok := a.snap.Domain(st); ok {
		return d
	}
	return 1<<63 | uint64(st)
}

// sitesAdapter implements group.Sites on a station-table snapshot.
type sitesAdapter struct {
	tab *stSnap
}

// Site implements group.Sites.
func (s *sitesAdapter) Site(st model.StationIdx) (geo.Vec3, float64, bool) {
	e, ok := s.tab.get(st)
	if !ok || !e.located {
		return geo.Vec3{}, 0, false
	}
	return e.pos, e.sigmaM, true
}

// altRec is the latest Mode S altitude of an aircraft (worker-local).
type altRec struct {
	ft      float64
	tNs     int64
	gillham bool
}

// hintsAdapter implements group.Hints from the worker's tracker partition
// and altitude store. Hints come only from the MLAT tracker (never from the
// ADS-B reference), so hold-out aircraft are clustered as if they were
// Mode S-only.
type hintsAdapter struct {
	w   *worker
	est track.Estimate
}

// maxAltAgeNs: an altitude older than this is not used as a hint.
const maxAltAgeNs = int64(30e9)

// Predict implements group.Hints.
func (h *hintsAdapter) Predict(key uint32, at group.Time) (geo.Vec3, float64, bool) {
	if a := h.w.ac[key]; a == nil || !a.verified || !h.w.tr.Predict(key, at.UnixNs(), &h.est) {
		return geo.Vec3{}, 0, false
	}
	c := &h.est.Cov
	return h.est.ECEF, math.Sqrt(math.Max(c[0]+c[4]+c[8], 0)), true
}

// AltitudeFt implements group.Hints.
func (h *hintsAdapter) AltitudeFt(key uint32, at group.Time) (float64, bool) {
	a, ok := h.w.alt[key]
	if !ok || at.UnixNs()-a.tNs > maxAltAgeNs {
		return 0, false
	}
	return a.ft, true
}

// planStations implements plan.Stations from the station table and the
// clock snapshot.
type planStations struct{ e *Engine }

// Station implements plan.Stations.
func (p planStations) Station(st model.StationIdx) (plan.StationInfo, bool) {
	ef, ok := p.e.st.load().get(st)
	if !ok {
		return plan.StationInfo{}, false
	}
	in := plan.StationInfo{Pos: ef.pos, Located: ef.located, Class: ef.class}
	if sc := p.e.filter.Snapshot().Station(st); sc != nil {
		in.Synced, in.Anchor = sc.Synced, sc.Anchor
		in.Class = sc.Class
		in.SigmaNs = math.Hypot(sc.SigmaThetaNs, sc.JitterNs)
	}
	return in, true
}

// posCache is the planner's view of the aircraft positions (plan.Positions):
// workers write the latest track estimate of each aircraft, the planner
// reads it from its own goroutines. Sharded by address.
type posCache struct {
	sh [64]posShard
}

type posShard struct {
	mu sync.RWMutex
	m  map[uint32]posRec
}

type posRec struct {
	pos   geo.Vec3
	sigma float64
	tNs   int64
}

// posTTL: positions older than this are not offered.
const posTTL = int64(30e9)

func newPosCache() *posCache {
	c := &posCache{}
	for i := range c.sh {
		c.sh[i].m = map[uint32]posRec{}
	}
	return c
}

func (c *posCache) put(key uint32, p geo.Vec3, sigma float64, tNs int64) {
	s := &c.sh[key&63]
	s.mu.Lock()
	s.m[key] = posRec{pos: p, sigma: sigma, tNs: tNs}
	s.mu.Unlock()
}

func (c *posCache) del(key uint32) {
	s := &c.sh[key&63]
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

// approx implements plan.Positions (bound to the engine's clock in
// planPositions).
func (c *posCache) approx(key uint32, nowNs int64) (geo.Vec3, float64, bool) {
	s := &c.sh[key&63]
	s.mu.RLock()
	r, ok := s.m[key]
	s.mu.RUnlock()
	if !ok || nowNs-r.tNs > posTTL {
		return geo.Vec3{}, 0, false
	}
	return r.pos, r.sigma, true
}

func (c *posCache) expire(beforeNs int64) {
	for i := range c.sh {
		s := &c.sh[i]
		s.mu.Lock()
		for k, r := range s.m {
			if r.tNs < beforeNs {
				delete(s.m, k)
			}
		}
		s.mu.Unlock()
	}
}

type planPositions struct{ e *Engine }

// Approx implements plan.Positions.
func (p planPositions) Approx(key uint32) (geo.Vec3, float64, bool) {
	return p.e.pos.approx(key, p.e.nowNs.Load())
}

// holdRef wraps the reference store as the clock filter's RefSource and
// hides hold-out aircraft from it (DESIGN §9.6: they are excluded from
// sync).
type holdRef struct {
	e   *Engine
	now *atomic.Int64
}

var _ clock.RefSource = holdRef{}

// RefPosition implements clock.RefSource.
func (h holdRef) RefPosition(key uint32, df uint8, tUnixNs int64) (geo.Vec3, [6]float64, bool) {
	if h.e.hold.In(key, tUnixNs) {
		return geo.Vec3{}, [6]float64{}, false
	}
	return h.e.refs.RefPosition(key, df, tUnixNs)
}

// Usable implements clock.RefSource.
func (h holdRef) Usable(key uint32) bool {
	if h.e.hold.In(key, h.now.Load()) {
		return false
	}
	return h.e.refs.Usable(key)
}
