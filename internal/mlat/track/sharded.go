package track

import (
	"runtime"
	"sync"
)

// Sharded spreads ICAOs over independent Trackers, each behind its own
// mutex (§9.5 concurrency: tracks are sharded by ICAO). All methods are
// safe for concurrent use; calls for different shards run in parallel.
type Sharded struct {
	shards []shard
	mask   uint32
}

type shard struct {
	mu sync.Mutex
	t  *Tracker
	_  [40]byte // keep neighbouring shard locks off one cache line
}

// NewSharded returns a sharded tracker with n shards (rounded up to a power
// of two; n <= 0 selects 4×GOMAXPROCS).
func NewSharded(p Params, n int) *Sharded {
	if n <= 0 {
		n = 4 * runtime.GOMAXPROCS(0)
	}
	k := 1
	for k < n {
		k <<= 1
	}
	s := &Sharded{shards: make([]shard, k), mask: uint32(k - 1)}
	for i := range s.shards {
		s.shards[i].t = New(p)
	}
	return s
}

func (s *Sharded) shard(icao uint32) *shard {
	h := icao * 0x9E3779B1 // Fibonacci hashing spreads adjacent ICAOs
	return &s.shards[(h>>16^h)&s.mask]
}

// Cluster applies a TOA cluster (see Tracker.Cluster).
func (s *Sharded) Cluster(c *Cluster, out *Estimate) Result {
	sh := s.shard(c.ICAO)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.t.Cluster(c, out)
}

// Fix applies a solver fix (see Tracker.Fix).
func (s *Sharded) Fix(f *Fix, out *Estimate) Result {
	sh := s.shard(f.ICAO)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.t.Fix(f, out)
}

// Velocity applies a velocity measurement (see Tracker.Velocity).
func (s *Sharded) Velocity(v *Velocity, out *Estimate) Result {
	sh := s.shard(v.ICAO)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.t.Velocity(v, out)
}

// Altitude applies an altitude measurement (see Tracker.Altitude).
func (s *Sharded) Altitude(a *Altitude, out *Estimate) Result {
	sh := s.shard(a.ICAO)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.t.Altitude(a, out)
}

// Get returns the current estimate of icao.
func (s *Sharded) Get(icao uint32, out *Estimate) bool {
	sh := s.shard(icao)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.t.Get(icao, out)
}

// Predict returns the estimate of icao predicted to tNs.
func (s *Sharded) Predict(icao uint32, tNs int64, out *Estimate) bool {
	sh := s.shard(icao)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.t.Predict(icao, tNs, out)
}

// Delete drops the track of icao.
func (s *Sharded) Delete(icao uint32) {
	sh := s.shard(icao)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.t.Delete(icao)
}

// Sweep runs Tracker.Sweep on every shard, one shard at a time (emit is
// called with that shard locked: it must not call back into s).
func (s *Sharded) Sweep(nowNs int64, emit func(*Estimate)) {
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		sh.t.Sweep(nowNs, emit)
		sh.mu.Unlock()
	}
}

// Len returns the number of tracked ICAOs.
func (s *Sharded) Len() int {
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		n += sh.t.Len()
		sh.mu.Unlock()
	}
	return n
}

// Stats sums the shard counters.
func (s *Sharded) Stats() Stats {
	var a Stats
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		b := sh.t.Stats()
		sh.mu.Unlock()
		a.Tracks += b.Tracks
		a.Confirmed += b.Confirmed
		a.Hypotheses += b.Hypotheses
		a.Clusters += b.Clusters
		a.Fixes += b.Fixes
		a.Applied += b.Applied
		a.Gated += b.Gated
		a.Excluded += b.Excluded
		a.OOSM += b.OOSM
		a.TooLate += b.TooLate
		a.Created += b.Created
		a.Confirmations += b.Confirmations
		a.Pruned += b.Pruned
		a.Deleted += b.Deleted
		a.Respawns += b.Respawns
	}
	return a
}
