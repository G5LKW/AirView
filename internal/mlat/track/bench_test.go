package track

import (
	"fmt"
	"testing"

	"airview/internal/mlat/geo"
)

// confirmedTracker returns a tracker with a confirmed, converged track of
// ICAO 1 on a ring of nst stations, the synthetic world and the next time.
func confirmedTracker(tb testing.TB, nst int) (*Tracker, *synth, *path, float64) {
	tb.Helper()
	st := stationsAround(50, 8, max(nst-1, 3), 80e3, true)
	s := newSynth(1, st, 50)
	pa := straightPath(50, 8, -20e3, 10e3, 9500, 200, 50)
	tr := New(Params{})
	for k := range 2 {
		p, _ := pa.at(float64(k))
		c := s.cluster(1, float64(k), p, nil, geo.FromECEF(p).H, 25)
		f := fixFrom(1, s.tNs(float64(k)), solveTOA(c.TOAs, c.AltHAE, 25, &s.atm), len(c.TOAs))
		tr.Fix(&f, nil)
	}
	for k := 4; k < 40; k++ {
		p, _ := pa.at(float64(k) * 0.5)
		c := s.cluster(1, float64(k)*0.5, p, nil, geo.FromECEF(p).H, 25)
		tr.Cluster(&c, nil)
	}
	var e Estimate
	if !tr.Get(1, &e) || e.Status != StatusConfirmed {
		tb.Fatal("no confirmed track")
	}
	return tr, s, pa, 20
}

// clusters pre-generates n clusters of nst stations from t0 at 0.2 s.
func clusters(s *synth, pa *path, t0 float64, n, nst int) []Cluster {
	use := make([]int, nst)
	for i := range use {
		use[i] = i
	}
	out := make([]Cluster, n)
	for i := range out {
		tt := t0 + float64(i)*0.2
		p, _ := pa.at(tt)
		out[i] = s.cluster(1, tt, p, use, geo.FromECEF(p).H, 25)
	}
	return out
}

func TestNoAllocs(t *testing.T) {
	tr, s, pa, t0 := confirmedTracker(t, 7)
	cs := clusters(s, pa, t0, 400, 7)
	var e Estimate
	i := 0
	if a := testing.AllocsPerRun(200, func() {
		tr.Cluster(&cs[i], &e)
		i++
	}); a != 0 {
		t.Errorf("Cluster: %v allocs", a)
	}
	tt := t0 + float64(i)*0.2
	if a := testing.AllocsPerRun(100, func() {
		tt += 0.2
		p, x := pa.at(tt)
		_ = p
		v := Velocity{ICAO: 1, TNs: s.tNs(tt), East: x[iVE], North: x[iVN], SigmaH: 5}
		tr.Velocity(&v, &e)
		al := Altitude{ICAO: 1, TNs: s.tNs(tt), HAE: geo.FromECEF(p).H, Sigma: 25}
		tr.Altitude(&al, &e)
		tr.Predict(1, s.tNs(tt+1), &e)
		tr.Get(1, &e)
	}); a != 0 {
		t.Errorf("Velocity/Altitude/Predict/Get: %v allocs", a)
	}
	f := func() Fix {
		p, _ := pa.at(tt + 0.1)
		c := s.cluster(1, tt+0.1, p, nil, geo.FromECEF(p).H, 25)
		return fixFrom(1, s.tNs(tt+0.1), solveTOA(c.TOAs, c.AltHAE, 25, &s.atm), 7)
	}()
	if a := testing.AllocsPerRun(50, func() { tr.Fix(&f, &e) }); a != 0 {
		t.Errorf("Fix update: %v allocs", a)
	}
	if a := testing.AllocsPerRun(50, func() { tr.Sweep(s.tNs(tt+0.5), nil) }); a != 0 {
		t.Errorf("Sweep: %v allocs", a)
	}
	sh := NewSharded(Params{}, 4)
	sh.Fix(&f, nil)
	if a := testing.AllocsPerRun(50, func() { sh.Fix(&f, &e) }); a != 0 {
		t.Errorf("Sharded.Fix: %v allocs", a)
	}
}

func BenchmarkCluster(b *testing.B) {
	for _, nst := range []int{2, 4, 8, 16, 24} {
		b.Run(fmt.Sprintf("stations=%d", nst), func(b *testing.B) {
			tr, s, pa, t0 := confirmedTracker(b, max(nst, 7))
			if nst > 7 {
				tr, s, pa, t0 = confirmedTracker(b, nst)
			}
			cs := clusters(s, pa, t0, 2000, nst)
			var e Estimate
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if i > 0 && i%len(cs) == 0 {
					b.StopTimer()
					tr, s, pa, t0 = confirmedTracker(b, max(nst, 7))
					cs = clusters(s, pa, t0, 2000, nst)
					b.StartTimer()
				}
				tr.Cluster(&cs[i%len(cs)], &e)
			}
		})
	}
}

func BenchmarkFixUpdate(b *testing.B) {
	tr, s, pa, t0 := confirmedTracker(b, 7)
	fs := make([]Fix, 500)
	for i := range fs {
		tt := t0 + float64(i)*0.2
		p, _ := pa.at(tt)
		c := s.cluster(1, tt, p, nil, geo.FromECEF(p).H, 25)
		fs[i] = fixFrom(1, s.tNs(tt), solveTOA(c.TOAs, c.AltHAE, 25, &s.atm), 7)
	}
	var e Estimate
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if i > 0 && i%len(fs) == 0 {
			b.StopTimer()
			tr, _, _, _ = confirmedTracker(b, 7)
			b.StartTimer()
		}
		tr.Fix(&fs[i%len(fs)], &e)
	}
}

// BenchmarkSweep10k: the 1 Hz output pass over 10 000 confirmed tracks
// (§20.2 scale target), per call.
func BenchmarkSweep10k(b *testing.B) {
	tr, s, _, t0 := confirmedTracker(b, 7)
	src := tr.entries[1]
	for i := uint32(2); i <= 10000; i++ {
		e := tr.newEntry(i)
		h := tr.newHyp()
		*h = *src.hyps[0]
		e.hyps[0] = h
		e.n = 1
	}
	n := 0
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		tr.Sweep(s.tNs(t0+0.5), func(*Estimate) { n++ })
	}
	b.ReportMetric(float64(tr.Len()), "tracks")
}
