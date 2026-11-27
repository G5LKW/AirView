package group

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// benchGroups builds groups of n copies (single transmissions, or bursts of
// two replies 0.74 ms apart) over a synthetic network of 200 stations.
func benchSetup(b *testing.B, n int, burst, hinted bool) (*Assembler, []*ingest.Group) {
	o := geo.ToECEF(52, 0, 0)
	f := geo.NewFrameECEF(o)
	rng := rand.New(rand.NewPCG(1, 2))
	sites := siteMap{}
	for i := 0; i < 200; i++ {
		sites[model.StationIdx(i+2)] = f.FromENU(geo.Vec3{X: (rng.Float64()*2 - 1) * 250e3, Y: (rng.Float64()*2 - 1) * 250e3, Z: rng.Float64() * 200})
	}
	clk := &tickClock{base: 1_763_000_000_000_000_000, cv: map[model.StationIdx]float64{}, jv: map[model.StationIdx]float64{}}
	hint := &fixedHint{pos: map[uint32]geo.Vec3{}, sigma: 1000}
	opt := Options{Config: DefaultConfig(), Clock: clk, Sites: sites, Sink: SinkFunc(func(*Cluster) {})}
	if hinted {
		opt.Hints = hint
	}
	a, err := New(opt)
	if err != nil {
		b.Fatal(err)
	}
	var gs []*ingest.Group
	for k := 0; k < 256; k++ {
		p := f.FromENU(geo.Vec3{X: (rng.Float64()*2 - 1) * 150e3, Y: (rng.Float64()*2 - 1) * 150e3, Z: 3000 + rng.Float64()*9000})
		key := uint32(k + 1)
		hint.pos[key] = p
		t0 := Time{Ns: clk.base + int64(k)*1e7}
		g := &ingest.Group{Hash: uint64(k), Data: df4(25000), Key: key, DF: 4, Kind: model.FrameModeSShort, FirstNs: t0.Ns}
		perm := rng.Perm(200)
		for i := 0; i < n; i++ {
			st := model.StationIdx(perm[i] + 2)
			at := t0
			if burst && i%2 == 1 {
				at = t0.Add(0.74e6)
			}
			ta := at.Add(sites[st].Dist(p)/mPerNs + rng.NormFloat64()*100)
			clk.jv[st] = 100 * 100
			g.Copies = append(g.Copies, copyOf(clk, st, ta))
		}
		gs = append(gs, g)
	}
	return a, gs
}

// BenchmarkObserve measures the assembler per copy (the DESIGN §13.2 grouper
// budget is 300 ns per copy for the ingest grouper; this stage runs after it
// on the flushed groups only).
func BenchmarkObserve(b *testing.B) {
	for _, n := range []int{4, 6, 12, 30, 64} {
		for _, mode := range []struct {
			name          string
			burst, hinted bool
		}{{"single", false, false}, {"single-hint", false, true}, {"burst", true, false}, {"burst-hint", true, true}} {
			b.Run(fmt.Sprintf("%s/%d", mode.name, n), func(b *testing.B) {
				a, gs := benchSetup(b, n, mode.burst, mode.hinted)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					a.Observe(gs[i&255])
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/copy")
			})
		}
	}
}

func BenchmarkModeAC(b *testing.B) {
	o := geo.ToECEF(52, 0, 0)
	f := geo.NewFrameECEF(o)
	rng := rand.New(rand.NewPCG(1, 2))
	sites := siteMap{}
	for i := 0; i < 20; i++ {
		sites[model.StationIdx(i+2)] = f.FromENU(geo.Vec3{X: (rng.Float64()*2 - 1) * 60e3, Y: (rng.Float64()*2 - 1) * 60e3})
	}
	m, _ := NewModeAC(DefaultConfig(), sites, nil)
	var obs []ACObs
	t0 := Time{Ns: 1_763_000_000_000_000_000}
	for k := 0; k < 50; k++ { // 50 replies of 10 codes in 10 ms, all heard by 20 stations
		p := f.FromENU(geo.Vec3{X: (rng.Float64()*2 - 1) * 100e3, Y: (rng.Float64()*2 - 1) * 100e3, Z: 5000})
		at := t0.Add(rng.Float64() * 10e6)
		for st, r := range sites {
			obs = append(obs, ACObs{Station: st, Code: uint16(k % 10), T: at.Add(r.Dist(p) / mPerNs), Var: 900})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Cluster(obs, func(*ACCluster) {})
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(obs)), "ns/reply")
}

func BenchmarkFlushDelay(b *testing.B) {
	fe := NewFlushEstimator()
	for st := model.StationIdx(2); st < 40; st++ {
		for i := 0; i < 200; i++ {
			fe.ObserveDelay(st, int64(30e6+float64(i)*1e6), 0)
		}
	}
	for k := uint32(0); k < 1000; k++ {
		for j := uint32(0); j < 8; j++ {
			fe.NoteHearer(k, model.StationIdx(2+(k+j)%38), 0)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fe.Delay(uint32(i % 1000))
	}
}
