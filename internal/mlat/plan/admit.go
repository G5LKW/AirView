package plan

import "sync"

// Solve admission (implements group.Selector).
//
// Two token buckets decide whether a solvable cluster is solved now:
//
//   - per aircraft: rate by class (AcquireHz 4/s while acquiring, TrackedHz
//     2/s once tracked, HoldOutHz 1/s, SuspectHz 0.5/s), burst Burst. A
//     tracked aircraft does not need every one of its 10-50 replies per
//     second solved: the tracker uses the clusters it is not given as fixes
//     as TDOA updates.
//   - global: GlobalSolveHz with capacity one second's worth. Class p is
//     admitted only while the global level is above Reserve[p] of capacity,
//     so under overload suspects back off first, then hold-out, then tracked
//     aircraft, and acquisitions of Mode S-only aircraft go on longest.
//
// A refusal downgrades the cluster to a TDOA-only update; nothing is lost.

type globalBucket struct {
	mu     sync.Mutex
	level  float64
	at     int64
	rate   float64
	cap    float64
	denied [NumPrio]uint64
	ok     [NumPrio]uint64
}

func (b *globalBucket) refill(nowNs int64) {
	if b.at == 0 {
		b.level, b.at = b.cap, nowNs
		return
	}
	if dt := nowNs - b.at; dt > 0 {
		b.level = min(b.cap, b.level+float64(dt)*1e-9*b.rate)
		b.at = nowNs
	}
}

func (p *Planner) rateFor(pr Prio) float64 {
	switch pr {
	case PrioAcquire:
		return p.cfg.AcquireHz
	case PrioTracked:
		return p.cfg.TrackedHz
	case PrioHoldOut:
		return p.cfg.HoldOutHz
	}
	return p.cfg.SuspectHz
}

// Candidate implements group.Selector: frames of aircraft without a good
// ADS-B position in the last ADSBGoodAge, of hold-out aircraft and of
// suspects are MLAT candidates (DESIGN §9.3). Unknown aircraft are
// candidates.
func (p *Planner) Candidate(key uint32, _ uint8, nowNs int64) bool {
	s := &p.t.sh[shardOf(key)]
	s.mu.Lock()
	e := s.lookup(key)
	pr := PrioAcquire
	if e != nil {
		pr = p.prio(e, nowNs)
	}
	s.mu.Unlock()
	return pr != PrioNone
}

// Admit implements group.Selector: whether a solvable cluster of aircraft key
// is solved now (see the rules above).
func (p *Planner) Admit(key uint32, sites int, nowNs int64) bool {
	s := &p.t.sh[shardOf(key)]
	s.mu.Lock()
	e := s.get(key)
	pr := p.prio(e, nowNs)
	if pr == PrioNone {
		pr = PrioSuspect // explicitly asked: lowest class
	}
	rate := p.rateFor(pr)
	if e.tokAt == 0 {
		e.tok, e.tokAt = float32(p.cfg.Burst), nowNs
	} else if dt := nowNs - e.tokAt; dt > 0 {
		e.tok = float32(min(p.cfg.Burst, float64(e.tok)+float64(dt)*1e-9*rate))
		e.tokAt = nowNs
	}
	if e.tok < 1 {
		s.mu.Unlock()
		p.g.mu.Lock()
		p.g.denied[pr]++
		p.g.mu.Unlock()
		return false
	}
	g := &p.g
	g.mu.Lock()
	g.refill(nowNs)
	if g.level < 1 || g.level < p.cfg.Reserve[pr]*g.cap {
		g.denied[pr]++
		g.mu.Unlock()
		s.mu.Unlock()
		return false
	}
	g.level--
	g.ok[pr]++
	g.mu.Unlock()
	e.tok--
	s.mu.Unlock()
	return true
}
