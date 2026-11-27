package ingest

import (
	"sync/atomic"
	"time"
)

// Priority is the class of traffic for the global ingest budget
// (DESIGN §4.6: "selective-eligible traffic is shed first").
type Priority uint8

// Priorities, from the first shed to the last.
const (
	// PrioSheddable is traffic that only feeds the map and statistics and is
	// redundant across stations: Beast 30004 (map/stats only) frames. It may
	// use up to 70 % of the budget.
	PrioSheddable Priority = iota
	// PrioNormal is full-mode AVF traffic: the part a selective station would
	// not send is selective-eligible. It may use up to 90 %.
	PrioNormal
	// PrioEssential is traffic that is already reduced or irreplaceable:
	// selective-mode stations, the host station, decoded updates. It may use
	// the whole budget.
	PrioEssential
	numPriorities
)

var prioShare = [numPriorities]float64{0.70, 0.90, 1.0}

// Budget is the global ingest semaphore: at most FPS frames per second
// across all stations (model.AggLimits.IngestFPS). It is safe for concurrent
// use. The zero value refuses nothing until SetFPS is called.
type Budget struct {
	fps   atomic.Int64
	sec   atomic.Int64
	used  atomic.Int64
	shed  [numPriorities]atomic.Uint64
	taken atomic.Uint64
	now   func() time.Time
}

// NewBudget returns a budget of fps frames per second (0 = unlimited).
func NewBudget(fps int, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	b := &Budget{now: now}
	b.fps.Store(int64(fps))
	return b
}

// SetFPS changes the limit (0 = unlimited).
func (b *Budget) SetFPS(fps int) { b.fps.Store(int64(fps)) }

// TakePrio reports whether n more frames of priority p may be accepted now.
// Refused frames must be dropped by the caller (they are counted here).
func (b *Budget) TakePrio(n int, p Priority) bool {
	if n <= 0 {
		return true
	}
	lim := b.fps.Load()
	if lim <= 0 {
		b.taken.Add(uint64(n))
		return true
	}
	if p >= numPriorities {
		p = PrioEssential
	}
	now := time.Now
	if b.now != nil {
		now = b.now
	}
	sec := now().Unix()
	if cur := b.sec.Load(); cur != sec && b.sec.CompareAndSwap(cur, sec) {
		b.used.Store(0)
	}
	used := b.used.Add(int64(n))
	if float64(used) > prioShare[p]*float64(lim) {
		b.used.Add(-int64(n))
		b.shed[p].Add(uint64(n))
		return false
	}
	b.taken.Add(uint64(n))
	return true
}

// Take implements beastin.Budget: Beast 30004 batches are sheddable.
func (b *Budget) Take(n int) bool { return b.TakePrio(n, PrioSheddable) }

// BudgetStats are the budget counters.
type BudgetStats struct {
	FPS   int64  `json:"fps"`   // limit (0 = unlimited)
	Used  int64  `json:"used"`  // frames taken in the current second
	Taken uint64 `json:"taken"` // frames accepted since start
	// Shed counts refused frames by priority (sheddable, normal, essential).
	Shed [3]uint64 `json:"shed"`
}

// Stats returns the counters.
func (b *Budget) Stats() BudgetStats {
	s := BudgetStats{FPS: b.fps.Load(), Used: b.used.Load(), Taken: b.taken.Load()}
	for i := range s.Shed {
		s.Shed[i] = b.shed[i].Load()
	}
	return s
}
