package group

import (
	"math"
	"sync"
	"time"

	"airview/internal/model"
)

// FlushEstimator implements the adaptive flush of DESIGN §9.3: a group of an
// aircraft is complete once the p99 delivery latency of the stations that
// usually hear it has elapsed, clamped to 150-600 ms. mlat-server style
// fixed waits of 0.9-2.5 s are the main term of its result latency; here a
// network of nearby, fast feeders flushes after 150 ms and a slow feeder only
// delays the aircraft it actually hears.
//
// Delivery latency is the server receive time minus the copy's corrected
// antenna time (ingest.Copy.CorrNs); the ingest reports it per copy with
// ObserveDelay (see docs/aggregator/HOOKS.md, WP-GR -> internal/ingest).
// Hearers are learned from the groups (NoteGroup or NoteHearer).
//
// The per-station distribution is a log-spaced histogram (8 bins per octave
// from 1 ms, 96 bins up to 4 s; ≤ 9 % quantile resolution) whose counts are
// halved every HalfLife, so it follows network changes within minutes.
// Methods are safe for concurrent use.
type FlushEstimator struct {
	// Quantile (0.99), Min / Max / Default delay and HalfLife; set before
	// first use or keep the NewFlushEstimator defaults.
	Quantile   float64
	Min, Max   time.Duration
	Default    time.Duration
	HalfLife   time.Duration
	MinSamples float64

	mu sync.Mutex
	st map[model.StationIdx]*latHist
	ac map[uint32]*hearerRing
}

const (
	flushBins     = 96
	flushPerOct   = 8
	flushBase     = 1e6 // 1 ms in ns
	hearersPerKey = 8
)

type latHist struct {
	bins    [flushBins]float32
	total   float32
	decayAt int64
	q       float64 // cached quantile value (ns) of qFor, recomputed lazily
	qFor    float64
	dirty   bool
}

type hearerRing struct {
	st   [hearersPerKey]model.StationIdx
	n    uint8
	next uint8
	last int64
}

// NewFlushEstimator returns an estimator with the DESIGN §9.3 bounds.
func NewFlushEstimator() *FlushEstimator {
	return &FlushEstimator{Quantile: 0.99, Min: 150 * time.Millisecond, Max: 600 * time.Millisecond,
		Default: 400 * time.Millisecond, HalfLife: 10 * time.Minute, MinSamples: 50,
		st: map[model.StationIdx]*latHist{}, ac: map[uint32]*hearerRing{}}
}

func delayBin(ns int64) int {
	if ns <= flushBase {
		return 0
	}
	b := int(math.Log2(float64(ns)/flushBase) * flushPerOct)
	return min(b, flushBins-1)
}

// binUpper is the upper edge of bin b in ns.
func binUpper(b int) float64 { return flushBase * math.Exp2(float64(b+1)/flushPerOct) }

// ObserveDelay records one copy's delivery latency for station st at server
// time nowNs.
func (f *FlushEstimator) ObserveDelay(st model.StationIdx, delayNs, nowNs int64) {
	if delayNs < 0 {
		delayNs = 0
	}
	f.mu.Lock()
	h := f.st[st]
	if h == nil {
		h = &latHist{decayAt: nowNs + int64(f.HalfLife)}
		f.st[st] = h
	}
	if nowNs >= h.decayAt {
		for i := range h.bins {
			h.bins[i] *= 0.5
		}
		h.total *= 0.5
		h.decayAt = nowNs + int64(f.HalfLife)
	}
	h.bins[delayBin(delayNs)]++
	h.total++
	h.dirty = true
	f.mu.Unlock()
}

// NoteHearer records that station st heard aircraft key at nowNs.
func (f *FlushEstimator) NoteHearer(key uint32, st model.StationIdx, nowNs int64) {
	f.mu.Lock()
	f.noteLocked(key, st, nowNs)
	f.mu.Unlock()
}

func (f *FlushEstimator) noteLocked(key uint32, st model.StationIdx, nowNs int64) {
	r := f.ac[key]
	if r == nil {
		r = &hearerRing{}
		f.ac[key] = r
	}
	r.last = nowNs
	for i := uint8(0); i < r.n; i++ {
		if r.st[i] == st {
			return
		}
	}
	r.st[r.next] = st
	r.next = (r.next + 1) % hearersPerKey
	r.n = min(r.n+1, hearersPerKey)
}

// NoteGroup records the stations of a cluster as hearers of its aircraft.
func (f *FlushEstimator) NoteGroup(c *Cluster) {
	f.mu.Lock()
	for i := range c.Members {
		f.noteLocked(c.Key, c.Members[i].Station, c.FirstNs)
	}
	f.mu.Unlock()
}

// Delay returns how long after its first copy a group of aircraft key
// should be flushed: the largest p99 latency among the stations that
// usually hear it, clamped to [Min, Max]; Default without enough data.
func (f *FlushEstimator) Delay(key uint32) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.ac[key]
	if r == nil || r.n == 0 {
		return f.Default
	}
	var worst float64
	known := false
	for i := uint8(0); i < r.n; i++ {
		h := f.st[r.st[i]]
		if h == nil || float64(h.total) < f.MinSamples {
			continue
		}
		known = true
		worst = max(worst, h.quantile(f.Quantile))
	}
	if !known {
		return f.Default
	}
	return min(max(time.Duration(worst), f.Min), f.Max)
}

// StationQuantile returns station st's latency quantile q (ns) and whether
// enough samples exist.
func (f *FlushEstimator) StationQuantile(st model.StationIdx, q float64) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.st[st]
	if h == nil || float64(h.total) < f.MinSamples {
		return 0, false
	}
	return h.quantile(q), true
}

func (h *latHist) quantile(q float64) float64 {
	if !h.dirty && h.qFor == q {
		return h.q
	}
	target := float32(q) * h.total
	var cum float32
	b := flushBins - 1
	for i := range h.bins {
		cum += h.bins[i]
		if cum >= target {
			b = i
			break
		}
	}
	h.q, h.qFor, h.dirty = binUpper(b), q, false
	return h.q
}

// Prune forgets aircraft not heard since before cutoffNs and stations with
// (decayed) totals under one sample. Call it about once a minute.
func (f *FlushEstimator) Prune(cutoffNs int64) {
	f.mu.Lock()
	for k, r := range f.ac {
		if r.last < cutoffNs {
			delete(f.ac, k)
		}
	}
	for k, h := range f.st {
		if h.total < 1 {
			delete(f.st, k)
		}
	}
	f.mu.Unlock()
}
