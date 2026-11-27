package tracker

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"

	"airview/internal/model"
	"airview/internal/store"
)

// laneOutput collects everything a Lanes writes.
type laneOutput struct {
	min  []store.StatsRow
	hour []store.RangeHour
	fs   []FlightStation
}

func tappedLanes(res model.StationResolver, out *laneOutput) *Lanes {
	ls := NewLanes(LaneOptions{Resolver: res, OnFlightStation: func(rows []FlightStation) {
		out.fs = append(out.fs, rows...)
	}})
	ls.tapRows = func(m []store.StatsRow, h []store.RangeHour) {
		out.min = append(out.min, m...)
		out.hour = append(out.hour, h...)
	}
	return ls
}

// SO-5: a CreditBatch produces exactly the rows Lanes.Credit produces for
// the same copies (minute rows, range hours, flight_station aggregates and
// lane summaries), for chunked and mixed station patterns, with diversity
// copies and flushes at random points.
func TestCreditBatchEquivalent(t *testing.T) {
	rs, sig, _, _ := creditSet(t)
	resolver := func() model.StationResolver {
		res := newFakeRes()
		for st := model.StationIdx(1); st <= benchStations; st++ {
			res.set(st, "sid", 50+float64(st%10)*0.05, 6+float64(st/10)*0.05, st%7 != 0)
		}
		return res
	}
	var a, b laneOutput
	direct := tappedLanes(resolver(), &a)
	batched := tappedLanes(resolver(), &b)
	batch := batched.NewCreditBatch()
	rng := rand.New(rand.NewPCG(3, 4))
	var lastSec int64
	st := model.StationIdx(1)
	for pass := int64(0); pass < 3; pass++ {
		for i := range rs {
			r := rs[i]
			r.TMs += pass * 20_000
			if s := r.TMs / 1000; s != lastSec {
				// Time passes: the worker flushes before the lanes tick.
				batch.Flush()
				direct.Tick(r.TMs)
				batched.Tick(r.TMs)
				lastSec = s
			}
			if rng.IntN(16) == 0 { // a new read: another station (or a spray of them)
				st = model.StationIdx(1 + rng.IntN(benchStations+20))
			}
			cst := st
			if rng.IntN(4) == 0 {
				cst = model.StationIdx(1 + rng.IntN(benchStations+20))
			}
			ch := rng.IntN(3) - 1
			if rng.IntN(10) == 0 && ch >= 0 {
				direct.CreditChannel(cst, ch, sig[i], &r)
				batch.CreditChannel(cst, ch, sig[i], &r)
			} else {
				direct.Credit(cst, ch, sig[i], &r)
				batch.Credit(cst, ch, sig[i], &r)
			}
			if rng.IntN(50) == 0 {
				batch.Flush()
			}
		}
	}
	batch.Flush()
	end := rs[len(rs)-1].TMs + 3*20_000 + 3_600_000
	direct.FlushAll(end)
	batched.FlushAll(end)
	if len(a.min) == 0 || len(a.fs) == 0 {
		t.Fatalf("nothing written: %d minute rows, %d flight rows", len(a.min), len(a.fs))
	}
	if !reflect.DeepEqual(nanSafe(a.min), nanSafe(b.min)) {
		t.Fatalf("minute rows differ (%d vs %d)", len(a.min), len(b.min))
	}
	if !reflect.DeepEqual(nanSafe(a.hour), nanSafe(b.hour)) {
		t.Fatalf("range hours differ (%d vs %d)", len(a.hour), len(b.hour))
	}
	if !reflect.DeepEqual(nanSafe(a.fs), nanSafe(b.fs)) {
		t.Fatalf("flight_station rows differ (%d vs %d)", len(a.fs), len(b.fs))
	}
	if !reflect.DeepEqual(direct.List(), batched.List()) {
		t.Fatal("lane summaries differ")
	}
	t.Logf("%d minute rows, %d range hours, %d flight_station rows identical", len(a.min), len(a.hour), len(a.fs))
}

// nanSafe renders rows with NaN fields comparable (fmt prints NaN as NaN).
func nanSafe(v any) string { return fmt.Sprintf("%+v", v) }

// The batch ignores the copies Lanes.Credit ignores and flushes by itself
// when full; it does not allocate.
func TestCreditBatchBasics(t *testing.T) {
	ls := NewLanes(LaneOptions{})
	b := ls.NewCreditBatch()
	b.Credit(1, -1, -10, nil)
	b.Credit(1, -1, -10, &Result{Status: ResultNone})
	b.Credit(1, -1, -10, &Result{Status: ResultLate})
	b.CreditChannel(1, 9, -10, &Result{Status: ResultBadCRC, TMs: 1000})
	if b.Len() != 0 {
		t.Fatalf("buffered %d ignored copies", b.Len())
	}
	r := Result{Status: ResultBadCRC, TMs: 1_700_000_000_000}
	allocs := testing.AllocsPerRun(10, func() {
		for i := 0; i < 3*CreditBatchCap+5; i++ {
			b.Credit(model.StationIdx(1+i%3), -1, -10, &r)
		}
		b.Flush()
	})
	if allocs > 0 {
		t.Fatalf("%.1f allocations per run", allocs)
	}
	info, ok := ls.Info(2)
	if !ok || info.Frames == 0 {
		t.Fatalf("station 2: %+v %v", info, ok)
	}
}

// creditWorkload runs the parallel credit benchmark with GOMAXPROCS
// ingest workers and `stations` feeders. With affinity every feeder's
// connection is read by one worker (feeders are spread over the workers by
// a hash, so workers share shard mutexes as soon as stations > shards) and
// a tenth of the credits go to other stations (the pending copies a worker
// credits when it resolves a transmission); without affinity every read
// is from a random feeder. A read holds chunk copies of one feeder; the
// batched worker flushes after every read.
func creditWorkload(b *testing.B, batched bool, stations, chunk int, affinity bool) {
	rs, sig, ls, _ := creditSet(b)
	procs := runtime.GOMAXPROCS(0)
	owner := func(st int) int { return int((uint32(st) * 2654435761) >> 7 % uint32(procs)) }
	var ctr atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		g := int(ctr.Add(1)) - 1
		rng := rand.New(rand.NewPCG(uint64(g), 99))
		var mine []model.StationIdx
		for st := 1; st <= stations; st++ {
			if owner(st) == g%procs {
				mine = append(mine, model.StationIdx(st))
			}
		}
		if len(mine) == 0 {
			mine = append(mine, model.StationIdx(1+g%stations))
		}
		var cb *CreditBatch
		if batched {
			cb = ls.NewCreditBatch()
		}
		i := g * 7919
		left := 0
		st := model.StationIdx(1)
		for pb.Next() {
			if left == 0 {
				if affinity {
					st = mine[rng.IntN(len(mine))]
				} else {
					st = model.StationIdx(1 + rng.IntN(stations))
				}
				left = chunk
				if cb != nil {
					cb.Flush() // end of a read
				}
			}
			left--
			cst := st
			if affinity && i%10 == 0 {
				cst = model.StationIdx(1 + rng.IntN(stations))
			}
			k := i % len(rs)
			r := rs[k]
			if cb != nil {
				cb.Credit(cst, -1, sig[k], &r)
			} else {
				ls.Credit(cst, -1, sig[k], &r)
			}
			i++
		}
		if cb != nil {
			cb.Flush()
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/credit")
}

// BenchmarkLaneCreditContention compares Lanes.Credit with CreditBatch
// from GOMAXPROCS workers (SO-5): 100 and 1,000 feeders, reads of 1, 16
// and 64 copies, with and without connection affinity.
func BenchmarkLaneCreditContention(b *testing.B) {
	for _, stations := range []int{100, 1000} {
		for _, aff := range []bool{true, false} {
			for _, chunk := range []int{1, 16, 64} {
				for _, batched := range []bool{false, true} {
					mode, a := "direct", "affinity"
					if batched {
						mode = "batched"
					}
					if !aff {
						a = "random"
					}
					b.Run(fmt.Sprintf("feeders%d/%s/read%d/%s", stations, a, chunk, mode), func(b *testing.B) {
						creditWorkload(b, batched, stations, chunk, aff)
					})
				}
			}
		}
	}
}
