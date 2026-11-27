package mlat

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"airview/internal/model"
)

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

// router batches the results for the feeders: workers add (station, fix)
// pairs that passed the minimum-contribution rule; Advance flushes them
// once a second, one SendResults call per station. Fixes are copied
// (including the velocity) so the workers may change their state at once.
type router struct {
	e     *Engine
	mu    sync.Mutex
	batch map[model.StationIdx][]routed
	spare map[model.StationIdx][]routed
	out   []model.MLATFix
	sts   []model.StationIdx // flush order (sorted, reproducible)
}

type routed struct {
	f      model.MLATFix
	vel    [3]float32
	hasVel bool
}

func newRouter(e *Engine) *router {
	return &router{e: e, batch: map[model.StationIdx][]routed{}, spare: map[model.StationIdx][]routed{}}
}

func (r *router) add(st model.StationIdx, f *model.MLATFix) {
	x := routed{f: *f}
	if f.Vel != nil {
		x.vel, x.hasVel = *f.Vel, true
	}
	x.f.Vel = nil
	r.mu.Lock()
	r.batch[st] = append(r.batch[st], x)
	r.mu.Unlock()
}

// flush sends the batches (called from Advance only).
func (r *router) flush() {
	r.mu.Lock()
	b := r.batch
	r.batch, r.spare = r.spare, b
	r.mu.Unlock()
	r.sts = r.sts[:0]
	for st, xs := range b {
		if len(xs) > 0 {
			r.sts = append(r.sts, st)
		}
	}
	slices.Sort(r.sts)
	for _, st := range r.sts {
		xs := b[st]
		r.out = r.out[:0]
		for i := range xs {
			f := xs[i].f
			if xs[i].hasVel {
				f.Vel = &xs[i].vel
			}
			r.out = append(r.out, f)
		}
		sent := false
		for _, s := range r.e.o.Results {
			if s.SendResults(st, r.out) {
				sent = true
				break
			}
		}
		if sent {
			r.e.ctr.resultsSent.Add(uint64(len(r.out)))
		} else {
			r.e.ctr.resultsRefused.Add(uint64(len(r.out)))
		}
		b[st] = xs[:0]
	}
	if len(b) > 4096 { // forget stations that went away
		clear(b)
	}
}

// rateTable counts each station's timed frames delivered while synced, per
// minute (the second half of the minimum-contribution rule). Workers add
// their counts once a second; Advance rotates the minute.
type rateTable struct {
	mu sync.RWMutex
	m  map[model.StationIdx]*rateCell
}

type rateCell struct {
	cur, prev atomic.Int64
}

func newRateTable() *rateTable { return &rateTable{m: map[model.StationIdx]*rateCell{}} }

func (t *rateTable) cell(st model.StationIdx) *rateCell {
	t.mu.RLock()
	c := t.m[st]
	t.mu.RUnlock()
	if c != nil {
		return c
	}
	t.mu.Lock()
	if c = t.m[st]; c == nil {
		c = &rateCell{}
		t.m[st] = c
	}
	t.mu.Unlock()
	return c
}

func (t *rateTable) add(st model.StationIdx, n int64) { t.cell(st).cur.Add(n) }

// rotate closes the minute.
func (t *rateTable) rotate() {
	t.mu.Lock()
	for k, c := range t.m {
		p := c.cur.Swap(0)
		c.prev.Store(p)
		if p == 0 {
			delete(t.m, k)
		}
	}
	t.mu.Unlock()
}

// perMin returns the station's timed frames in the last full minute (or
// the current one when it already has more).
func (t *rateTable) perMin(st model.StationIdx) int64 {
	t.mu.RLock()
	c := t.m[st]
	t.mu.RUnlock()
	if c == nil {
		return 0
	}
	return max(c.prev.Load(), c.cur.Load())
}

func (t *rateTable) eligible(st model.StationIdx, minPerMin int64) bool {
	return t.perMin(st) >= minPerMin
}
