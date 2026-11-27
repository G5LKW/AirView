package plan

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// bigWorld is a DESIGN §20 scale network: nst stations over a disc of
// radiusKm, nac aircraft, hearing within 350 km (line of sight) with a
// range-dependent probability. A third of the aircraft are Mode S-only.
func bigWorld(b testing.TB, nst, nac int, radiusKm float64) (*Planner, [][]model.StationIdx, []uint32) {
	o := geo.ToECEF(50, 10, 0)
	f := geo.NewFrameECEF(o)
	rng := rand.New(rand.NewPCG(5, 5))
	sts := stMap{}
	pos := make([]geo.Vec3, nst)
	for i := 0; i < nst; i++ {
		r := radiusKm * 1e3 * math.Sqrt(rng.Float64())
		a := rng.Float64() * 2 * math.Pi
		pos[i] = f.FromENU(geo.Vec3{X: r * math.Cos(a), Y: r * math.Sin(a), Z: 50})
		cls := model.ClockSDR12
		if i%10 == 0 {
			cls = model.ClockGPSUTC
		}
		sts[model.StationIdx(i+2)] = StationInfo{Pos: pos[i], Located: true, Synced: rng.Float64() < 0.9, Anchor: cls.AnchorCapable(), Class: cls}
	}
	p, err := New(Options{Config: DefaultConfig(), Stations: sts, Now: func() time.Time { return time.Unix(0, t0) }})
	if err != nil {
		b.Fatal(err)
	}
	heard := make([][]model.StationIdx, nac)
	keys := make([]uint32, nac)
	for k := 0; k < nac; k++ {
		r := (radiusKm + 100) * 1e3 * math.Sqrt(rng.Float64())
		a := rng.Float64() * 2 * math.Pi
		ap := f.FromENU(geo.Vec3{X: r * math.Cos(a), Y: r * math.Sin(a), Z: 1000 + rng.Float64()*11000})
		keys[k] = uint32(0x400000 + k)
		for i := 0; i < nst; i++ {
			if d := ap.Dist(pos[i]); d < 350e3 && rng.Float64() < 1-d/400e3 {
				heard[k] = append(heard[k], model.StationIdx(i+2))
			}
		}
	}
	return p, heard, keys
}

func feedBig(p *Planner, heard [][]model.StationIdx, keys []uint32, now int64) {
	g := &ingest.Group{DF: 4}
	for k, key := range keys {
		g.Key, g.FirstNs = key, now
		g.Copies = g.Copies[:0]
		for _, st := range heard[k] {
			if len(g.Copies) == 64 {
				break
			}
			g.Copies = append(g.Copies, ingest.Copy{Station: st, Timed: true, Signal: -20})
		}
		p.ObserveGroup(g)
		// the rest of the hearers (beyond the 64 copies of one group)
		for i := 64; i < len(heard[k]); i++ {
			p.NoteHearer(key, heard[k][i], now)
		}
		if k%3 != 0 {
			p.NoteADSB(key, now, true)
		}
	}
}

// BenchmarkReplan1000x10k measures one replan of 1,000 stations and 10,000
// aircraft (DESIGN §20.2 scale).
func BenchmarkReplan1000x10k(b *testing.B) {
	p, heard, keys := bigWorld(b, 1000, 10000, 1500)
	tot := 0
	for _, h := range heard {
		tot += len(h)
	}
	now := t0 + 1e9
	feedBig(p, heard, keys, now)
	p.Replan(now)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now += 5e9
		feedBig(p, heard, keys, now)
		b.StartTimer()
		p.Replan(now)
		b.StopTimer()
	}
	b.StopTimer()
	s := p.Summary()
	b.ReportMetric(float64(tot)/float64(len(keys)), "hearers/aircraft")
	b.ReportMetric(float64(s.SyncMembers)/float64(s.Stations), "sync/station")
	b.ReportMetric(float64(s.MLATMembers)/float64(s.Stations), "mlat/station")
}

func BenchmarkObserveGroup(b *testing.B) {
	p, heard, keys := bigWorld(b, 300, 2000, 600)
	gs := make([]*ingest.Group, len(keys))
	for k, key := range keys {
		g := &ingest.Group{Key: key, DF: 4}
		for _, st := range heard[k] {
			if len(g.Copies) < 64 {
				g.Copies = append(g.Copies, ingest.Copy{Station: st, Timed: true, Signal: -20})
			}
		}
		gs[k] = g
	}
	b.ReportAllocs()
	b.ResetTimer()
	now := t0
	for i := 0; i < b.N; i++ {
		g := gs[i%len(gs)]
		now += 1e5 // 10k groups/s: every aircraft is re-noted every 0.2 s ... about once a second at 2000 aircraft
		g.FirstNs = now
		p.ObserveGroup(g)
	}
}

func BenchmarkAdmit(b *testing.B) {
	p := newPlanner(b, stMap{}, nil)
	b.ReportAllocs()
	now := t0
	for i := 0; i < b.N; i++ {
		now += 1e4
		p.Admit(uint32(i&1023), 5, now)
	}
}

func BenchmarkCandidate(b *testing.B) {
	p := newPlanner(b, stMap{}, nil)
	for k := uint32(0); k < 1024; k++ {
		p.NoteADSB(k, t0, k%2 == 0)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.Candidate(uint32(i&1023), 4, t0)
	}
}
