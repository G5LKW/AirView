package tracker

import (
	"math"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
)

// Aggregator-mode budgets (docs/aggregator/DESIGN.md §6.4, WP-T acceptance):
//   - Tracker.Handle: <= 700 ns per unique transmission, including the
//     credit of the hearing station's lane;
//   - Lanes.Credit: <= 120 ns per later copy;
//
// both with 0 allocs/op in steady state. TestNetworkBudgets enforces looser
// bounds (CI machines vary); the benchmarks report the real numbers.

const benchStations = 100

// benchNet builds a network tracker with benchStations located stations
// around the simulator's receiver and returns it with recorded unique frames
// (span of simulated time).
func benchNet(b testing.TB, aircraft int) (*Tracker, *Lanes, []model.RawFrame, time.Duration) {
	const span = 20 * time.Second
	frames, cfg := uniqueFrames(b, aircraft, span)
	res := newFakeRes()
	for st := model.StationIdx(1); st <= benchStations; st++ {
		res.set(st, "sid", cfg.Receiver.Lat+float64(st%10)*0.05, cfg.Receiver.Lon+float64(st/10)*0.05, true)
	}
	ls := NewLanes(LaneOptions{Resolver: res})
	// One shard: these benchmarks track the single-tracker path (and its
	// history in WIRING.md); the ICAO-sharded paths have their own
	// benchmarks in shard_bench_test.go.
	tr := New(Options{Config: &cfg, Shards: 1})
	tr.SetNetwork(res, ls)
	return tr, ls, frames, span
}

// BenchmarkNetworkHandle measures Tracker.Handle per unique transmission
// (decode, aircraft state, the hearing station's lane credit).
func BenchmarkNetworkHandle(b *testing.B) {
	tr, _, frames, span := benchNet(b, 150)
	for pass := 0; pass < 3; pass++ {
		for i, f := range frames {
			f.Recv = f.Recv.Add(time.Duration(pass) * span)
			f.Station = model.StationIdx(1 + i%benchStations)
			tr.Handle(f)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	pass, i := 3, 0
	shift := time.Duration(pass) * span
	for n := 0; n < b.N; n++ {
		f := frames[i]
		f.Recv = f.Recv.Add(shift)
		f.Station = model.StationIdx(1 + n%benchStations)
		tr.Handle(f)
		if i++; i == len(frames) {
			i, pass = 0, pass+1
			shift = time.Duration(pass) * span
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/frame")
}

// creditSet returns Results of recorded frames for the credit benchmark.
func creditSet(b testing.TB) ([]Result, []float64, *Lanes, time.Duration) {
	tr, ls, frames, span := benchNet(b, 150)
	rs := make([]Result, 0, len(frames))
	sig := make([]float64, 0, len(frames))
	for i, f := range frames {
		f.Station = model.StationIdx(1 + i%benchStations)
		r := tr.Handle(f)
		if r.Status != ResultNone {
			rs = append(rs, r)
			sig = append(sig, f.Signal)
		}
	}
	return rs, sig, ls, span
}

// BenchmarkLaneCredit measures Lanes.Credit per later copy, spread over
// benchStations stations (the realistic mix of statuses and positions of a
// simulated sky). The lanes' 1 Hz Tick runs inside the timed loop whenever
// data time crosses a second, so the deferred work (position fold, minute
// rows, flushes) is part of the per-credit cost.
func BenchmarkLaneCredit(b *testing.B) {
	rs, sig, ls, span := creditSet(b)
	shiftMs := span.Milliseconds()
	var lastSec int64
	credit := func(r *Result, st model.StationIdx, sig float64) {
		ls.Credit(st, -1, sig, r)
		if s := r.TMs / 1000; s != lastSec {
			lastSec = s
			ls.Tick(r.TMs)
		}
	}
	for pass := int64(1); pass <= 3; pass++ { // warm up lanes and their tables
		for i := range rs {
			r := rs[i]
			r.TMs += pass * shiftMs
			credit(&r, model.StationIdx(1+(i+int(pass))%benchStations), sig[i])
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	pass, i := int64(4), 0
	for n := 0; n < b.N; n++ {
		r := rs[i]
		r.TMs += pass * shiftMs
		credit(&r, model.StationIdx(1+n%benchStations), sig[i])
		if i++; i == len(rs) {
			i, pass = 0, pass+1
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/credit")
}

// BenchmarkLaneCreditParallel is BenchmarkLaneCredit from GOMAXPROCS
// goroutines (shard lock contention).
func BenchmarkLaneCreditParallel(b *testing.B) {
	rs, sig, ls, _ := creditSet(b)
	var ctr atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int(ctr.Add(1)) * 7919
		for pb.Next() {
			k := i % len(rs)
			r := rs[k]
			ls.Credit(model.StationIdx(1+i%benchStations), -1, sig[k], &r)
			i++
		}
	})
}

// BenchmarkNetworkHandleParallel calls Handle from GOMAXPROCS goroutines:
// it measures the tracker mutex under contention (the ICAO-sharding gate,
// see WIRING.md).
func BenchmarkNetworkHandleParallel(b *testing.B) {
	tr, _, frames, _ := benchNet(b, 150)
	for i, f := range frames {
		f.Station = model.StationIdx(1 + i%benchStations)
		tr.Handle(f)
	}
	var ctr atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int(ctr.Add(1)) * 7919
		for pb.Next() {
			f := frames[i%len(frames)]
			f.Station = model.StationIdx(1 + i%benchStations)
			tr.Handle(f)
			i++
		}
	})
}

// TestNetworkBudgets checks the aggregator budgets with a margin for slow
// or shared CI machines, and that both paths stay allocation-free.
func TestNetworkBudgets(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark-based")
	}
	// The wall-clock budget is checked on the best of up to five measurements:
	// under "go test ./..." the long MLAT suites load every core, and a single
	// measurement then reads up to 3x slow. Allocations are checked on every
	// measurement.
	var h, c testing.BenchmarkResult
	hNs, cNs := math.Inf(1), math.Inf(1)
	for try := 0; try < 5 && (try == 0 || hNs > 3*700 || cNs > 3*120); try++ {
		h = testing.Benchmark(BenchmarkNetworkHandle)
		c = testing.Benchmark(BenchmarkLaneCredit)
		if h.AllocsPerOp() != 0 || c.AllocsPerOp() != 0 {
			break
		}
		hNs = min(hNs, float64(h.T.Nanoseconds())/float64(h.N))
		cNs = min(cNs, float64(c.T.Nanoseconds())/float64(c.N))
	}
	t.Logf("Handle %.0f ns/frame %d allocs/op; Credit %.0f ns/credit %d allocs/op", hNs, h.AllocsPerOp(), cNs, c.AllocsPerOp())
	if h.AllocsPerOp() != 0 || c.AllocsPerOp() != 0 {
		t.Errorf("allocations on the hot path: handle %d, credit %d", h.AllocsPerOp(), c.AllocsPerOp())
	}
	if hNs > 3*700 || cNs > 3*120 {
		t.Errorf("budget exceeded by more than 3x: handle %.0f ns, credit %.0f ns", hNs, cNs)
	}
}
