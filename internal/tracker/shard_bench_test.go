package tracker

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
)

// Sharding benchmarks (WP-SCALE-tracker; WIRING.md "Scaling"):
//
//	go test ./internal/tracker -run '^$' -bench 'Sharded' -benchtime 2s
//
// BenchmarkShardedHandle: one goroutine, the routing overhead.
// BenchmarkShardedHandleParallel: GOMAXPROCS goroutines (the mutex gate).
// BenchmarkShardedSnapshot: a fresh merged snapshot of many aircraft.
// BenchmarkShardedTick: the 1 Hz housekeeping of many aircraft (the longest
// mutex hold of the frame path), per shard.

var shardCounts = []int{1, 2, 4, 8, 16}

func benchShardNet(b testing.TB, aircraft, shards int) (*Tracker, []model.RawFrame, time.Duration) {
	frames, cfg, span := cachedFrames(b, aircraft)
	res := newFakeRes()
	for st := model.StationIdx(1); st <= benchStations; st++ {
		res.set(st, "sid", cfg.Receiver.Lat+float64(st%10)*0.05, cfg.Receiver.Lon+float64(st/10)*0.05, true)
	}
	ls := NewLanes(LaneOptions{Resolver: res})
	cfg.Aggregator.Limits.MaxAircraft = 4 * aircraft
	tr := New(Options{Config: &cfg, Shards: shards, MaxAircraft: 4 * aircraft, Logger: quietLog()})
	tr.SetNetwork(res, ls)
	return tr, frames, span
}

var frameCache sync.Map // aircraft -> cachedSet

type cachedSet struct {
	frames []model.RawFrame
	cfg    model.Config
	span   time.Duration
}

// cachedFrames records unique simulator frames once per aircraft count.
func cachedFrames(b testing.TB, aircraft int) ([]model.RawFrame, model.Config, time.Duration) {
	if v, ok := frameCache.Load(aircraft); ok {
		c := v.(cachedSet)
		return c.frames, c.cfg, c.span
	}
	span := 20 * time.Second
	if aircraft > 2000 {
		span = 8 * time.Second
	}
	frames, cfg := uniqueFrames(b, aircraft, span)
	for i := range frames {
		frames[i].Station = model.StationIdx(1 + i%benchStations)
	}
	frameCache.Store(aircraft, cachedSet{frames, cfg, span})
	return frames, cfg, span
}

func BenchmarkShardedHandle(b *testing.B) {
	for _, n := range []int{1, 4} {
		b.Run("shards="+strconv.Itoa(n), func(b *testing.B) {
			tr, frames, span := benchShardNet(b, 150, n)
			for pass := 0; pass < 2; pass++ {
				for _, f := range frames {
					f.Recv = f.Recv.Add(time.Duration(pass) * span)
					tr.Handle(f)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			pass, i := 2, 0
			shift := time.Duration(pass) * span
			for k := 0; k < b.N; k++ {
				f := frames[i]
				f.Recv = f.Recv.Add(shift)
				tr.Handle(f)
				if i++; i == len(frames) {
					i, pass = 0, pass+1
					shift = time.Duration(pass) * span
				}
			}
		})
	}
}

// BenchmarkShardedHandleParallel replays the sky from GOMAXPROCS goroutines
// in time order (each goroutine takes the next frame of a shared cursor),
// as the ingest's reader goroutines deliver it.
func BenchmarkShardedHandleParallel(b *testing.B) {
	for _, ac := range []int{150, 2000} {
		for _, n := range shardCounts {
			b.Run(fmt.Sprintf("aircraft=%d/shards=%d", ac, n), func(b *testing.B) {
				tr, frames, span := benchShardNet(b, ac, n)
				for _, f := range frames {
					tr.Handle(f)
				}
				var cur atomic.Int64
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						k := cur.Add(1)
						pass := 1 + k/int64(len(frames))
						f := frames[k%int64(len(frames))]
						f.Recv = f.Recv.Add(time.Duration(pass) * span)
						tr.Handle(f)
					}
				})
			})
		}
	}
}

// BenchmarkShardedSnapshot measures building a fresh live picture (every
// shard rebuilds; the merge and the copy) - the read that the 1 Hz SSE
// broadcast pays, and that holds the shard mutexes while it builds.
func BenchmarkShardedSnapshot(b *testing.B) {
	ac := envInt("TRACKER_BENCH_AIRCRAFT", 10000)
	for _, n := range []int{1, 4, 8, 16} {
		b.Run(fmt.Sprintf("aircraft=%d/shards=%d", ac, n), func(b *testing.B) {
			tr, frames, _ := benchShardNet(b, ac, n)
			for _, f := range frames {
				tr.Handle(f)
			}
			parts := []*Tracker{tr}
			if s := tr.sh.Load(); s != nil {
				parts = s.parts
			}
			b.ReportAllocs()
			b.ResetTimer()
			var live int
			for k := 0; k < b.N; k++ {
				for _, p := range parts {
					p.mu.Lock()
					p.tickN++ // force a rebuild
					p.mu.Unlock()
				}
				live = tr.Snapshot().Counts.Aircraft
			}
			b.ReportMetric(float64(live), "aircraft")
		})
	}
}

// BenchmarkShardedTick measures the 1 Hz housekeeping (rates, expiry, the
// minute roll's classification) of every shard: per shard it is one mutex
// hold that frames of that shard wait behind.
func BenchmarkShardedTick(b *testing.B) {
	ac := envInt("TRACKER_BENCH_AIRCRAFT", 10000)
	for _, n := range []int{1, 4, 8, 16} {
		b.Run(fmt.Sprintf("aircraft=%d/shards=%d", ac, n), func(b *testing.B) {
			tr, frames, _ := benchShardNet(b, ac, n)
			for _, f := range frames {
				tr.Handle(f)
			}
			parts := []*Tracker{tr}
			if s := tr.sh.Load(); s != nil {
				parts = s.parts
			}
			b.ResetTimer()
			for k := 0; k < b.N; k++ {
				for _, p := range parts {
					p.mu.Lock()
					p.tick(p.nowMs)
					p.mu.Unlock()
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*len(parts)), "µs/shard-hold")
			b.ReportMetric(float64(tr.Snapshot().Counts.Aircraft), "aircraft")
		})
	}
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return d
}
