package ingest

import (
	"context"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/tracker"
)

// nopHandler is the cheapest NetHandler: it measures the ingest alone.
type nopHandler struct{ n atomic.Uint64 }

func (h *nopHandler) Handle(f model.RawFrame) tracker.Result {
	h.n.Add(1)
	return tracker.Result{Status: tracker.ResultOK, Key: 1}
}
func (h *nopHandler) HandleUpdate(model.DecodedUpdate) {}
func (h *nopHandler) Lanes() *tracker.Lanes            { return nil }

type benchFrames struct {
	frames []model.RawFrame
	span   int64 // ns covered
}

var (
	benchOnce sync.Once
	benchData benchFrames
)

// loadBench simulates 50 stations for 20 s (about 600k copies) once.
func loadBench(b *testing.B) *benchFrames {
	benchOnce.Do(func() {
		const rx = 50
		n := newSimNet(b, 21, rx, 250, nil)
		col := &collectObs{n: n, frames: make([][]model.RawFrame, rx)}
		n.AddSink(col)
		if err := n.Advance(context.Background(), 20*time.Second); err != nil {
			b.Fatal(err)
		}
		var all []model.RawFrame
		for _, fs := range col.frames {
			all = append(all, fs...)
		}
		sort.SliceStable(all, func(i, j int) bool { return all[i].HostNs < all[j].HostNs })
		benchData = benchFrames{frames: all, span: all[len(all)-1].HostNs - all[0].HostNs + int64(time.Second)}
	})
	return &benchData
}

// shifted returns frame i of pass p, moved p spans later (host and ticks).
func (d *benchFrames) shifted(i, p int) model.RawFrame {
	f := d.frames[i]
	sh := int64(p) * d.span
	f.HostNs += sh
	if f.ClockCls.Timed() {
		f.Ticks += uint64(float64(sh) * f.ClockCls.NominalHz() / 1e9)
	}
	f.Recv = time.Time{}
	return f
}

// BenchmarkGrouperCopy measures the ingest per copy (DESIGN §13.2 budget:
// ≤ 300 ns per copy, 0 allocs): normaliser, host-offset estimator, grouper,
// amortised sweeping and latency learning; the tracker is a no-op.
func BenchmarkGrouperCopy(b *testing.B) {
	d := loadBench(b)
	clk := &vclock{}
	h := &nopHandler{}
	ing := newTestIngest(b, h, clk, nil)
	n := len(d.frames)
	// warm up one pass (allocates the pools)
	for i := 0; i < n; i++ {
		f := d.shifted(i, 0)
		now := f.HostNs + int64(20*time.Millisecond)
		ing.frame(&f, false, now)
		if i%512 == 0 {
			ing.sweep(now)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for k := 0; k < b.N; k++ {
		i, p := k%n, 1+k/n
		f := d.shifted(i, p)
		now := f.HostNs + int64(20*time.Millisecond)
		ing.frame(&f, false, now)
		if k%512 == 0 {
			ing.sweep(now)
		}
	}
}

// BenchmarkGrouperCopyParallel runs the same with one goroutine per station
// group (GOMAXPROCS workers, each owning a disjoint set of stations).
func BenchmarkGrouperCopyParallel(b *testing.B) {
	d := loadBench(b)
	var newest atomic.Int64
	h := &nopHandler{}
	ing, err := New(Options{Handler: h, Logger: quietLogger(), Now: func() time.Time { return time.Unix(0, newest.Load()) }})
	if err != nil {
		b.Fatal(err)
	}
	stop := make(chan struct{})
	var sw sync.WaitGroup
	sw.Add(1)
	go func() {
		defer sw.Done()
		for {
			select {
			case <-stop:
				return
			default:
				ing.sweep(newest.Load())
				time.Sleep(time.Millisecond)
			}
		}
	}()
	var worker atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		w := int(worker.Add(1))
		k := 0
		for pb.Next() {
			i := (k*7919 + w) % len(d.frames)
			f := d.shifted(i, 1+k/len(d.frames))
			now := f.HostNs + int64(20*time.Millisecond)
			if now > newest.Load() {
				newest.Store(now)
			}
			ing.frame(&f, false, now)
			k++
		}
	})
	b.StopTimer()
	close(stop)
	sw.Wait()
}

// BenchmarkPayloadHash measures the hash of a long frame.
func BenchmarkPayloadHash(b *testing.B) {
	d := []byte{0x8D, 0x48, 0x40, 0xD6, 0x20, 0x2C, 0xC3, 0x71, 0xC3, 0x2C, 0xE0, 0x57, 0x60, 0x98}
	var s uint64
	for i := 0; i < b.N; i++ {
		d[13] = byte(i)
		s += payloadHash(d)
	}
	_ = s
}

// TestGrouperBudget re-checks the per-copy budget with a 3x margin for slow
// CI machines (the benchmark is the measurement).
func TestGrouperBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	r := testing.Benchmark(BenchmarkGrouperCopy)
	ns := float64(r.T.Nanoseconds()) / float64(r.N)
	t.Logf("grouper: %.0f ns/copy, %d allocs/op (%d B/op)", ns, r.AllocsPerOp(), r.AllocedBytesPerOp())
	if ns > 900 {
		t.Errorf("%.0f ns per copy > 3 x 300 ns", ns)
	}
	if r.AllocsPerOp() != 0 {
		t.Errorf("%d allocs per copy, want 0", r.AllocsPerOp())
	}
}

// BenchmarkGrouperDense measures the per-copy cost when every transmission
// is heard by many stations (dense areas: 36 copies per transmission were
// measured at 1000 feeders, up to MaxCopies). SO-4: the same-station check
// must not make this quadratic.
func BenchmarkGrouperDense(b *testing.B) {
	for _, k := range []int{6, 50, 100} {
		b.Run(strconv.Itoa(k), func(b *testing.B) { benchDense(b, k) })
	}
}

// benchDense offers b.N copies, k per transmission (one per station).
func benchDense(b *testing.B, k int) {
	clk := &vclock{}
	ing := newTestIngest(b, &nopHandler{}, clk, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; {
		tx := n / k
		h := t0 + int64(tx)*int64(50*time.Microsecond)
		d := df17pos(0x400000+uint32(tx%4000), tx)
		for s := 0; s < k && n < b.N; s, n = s+1, n+1 {
			f := tframe(model.StationIdx(2+s), 0, h, d)
			ing.frame(&f, false, h+int64(20*time.Millisecond))
		}
		if tx%256 == 0 {
			ing.sweep(h)
		}
	}
}

// BenchmarkGrouperDenseParallel is the dense case as a busy aggregator sees
// it (WP-SCALE-tracker): GOMAXPROCS reader goroutines, each owning a set of
// stations, deliver the copies of the same transmissions at the same time,
// k copies per transmission (k above MaxCopies: the copies beyond it are
// credited but not stored). Per copy it measures the station look-up of a
// full group and the contention on the group's shard.
func BenchmarkGrouperDenseParallel(b *testing.B) {
	for _, k := range []int{16, 64, 129} {
		b.Run(strconv.Itoa(k), func(b *testing.B) { benchDenseParallel(b, k) })
	}
}

func benchDenseParallel(b *testing.B, k int) {
	var vnow atomic.Int64
	vnow.Store(t0)
	ing, err := New(Options{Handler: &nopHandler{}, Logger: quietLogger(), Now: func() time.Time { return time.Unix(0, vnow.Load()) }})
	if err != nil {
		b.Fatal(err)
	}
	const txs = 4096
	data := make([][]byte, txs)
	for i := range data {
		data[i] = df17pos(0x400000+uint32(i), i)
	}
	workers := min(runtime.GOMAXPROCS(0), k)
	per := (b.N + workers - 1) / workers
	stop := make(chan struct{})
	swept := make(chan struct{})
	go func() { // the sweeper, as Run does every 10 ms
		defer close(swept)
		for {
			select {
			case <-stop:
				return
			default:
				ing.sweep(vnow.Load())
				time.Sleep(time.Millisecond)
			}
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := 0
			for tx := 0; done < per; tx++ {
				h := t0 + int64(tx)*int64(50*time.Microsecond)
				if w == 0 && tx%256 == 0 {
					vnow.Store(h)
				}
				for s := w; s < k && done < per; s += workers {
					f := tframe(model.StationIdx(2+s), 0, h, data[tx%txs])
					ing.frame(&f, false, h+int64(20*time.Millisecond))
					done++
				}
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	close(stop)
	<-swept
}

// TestGrouperDenseBudget re-checks the 300 ns/copy budget (DESIGN §13.2) at
// 100 copies per transmission, with a 3x margin for slow CI machines.
func TestGrouperDenseBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	r := testing.Benchmark(func(b *testing.B) { benchDense(b, 100) })
	if r.N == 0 {
		t.Skip("benchmark did not run")
	}
	if ns := float64(r.T.Nanoseconds()) / float64(r.N); ns > 900 {
		t.Fatalf("%.0f ns per copy at dense load (budget 300, CI margin 3x)", ns)
	}
}
