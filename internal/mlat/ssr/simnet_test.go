package ssr_test

import (
	"math"
	"sync"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/ssr"
	"airview/internal/mlat/ssr/ssrsim"
)

func sitesOf(sc ssrsim.Scenario) []ssr.Site {
	var out []ssr.Site
	for _, r := range sc.Radars {
		out = append(out, ssr.Site{Name: r.Name, Lat: r.Pos.Lat, Lon: r.Pos.Lon, HeightM: r.Pos.H})
	}
	return out
}

// End to end against simulated truth (SSR-RESEARCH.md §6).
func TestSimnetIdealSites(t *testing.T) {
	sc := ssrsim.Baseline(11)
	sc.Ideal = true
	sc.DurS = 300
	cfg := ssr.DefaultConfig()
	cfg.Sites = sitesOf(sc)
	o := ssrsim.Evaluate(sc, cfg, ssr.Hint{})
	if !o.Result.OK || o.ErrHM > 5 {
		t.Fatalf("ideal: ok=%v %q err_h %.1f m σ_h %.1f", o.Result.OK, o.Result.Reason, o.ErrHM, o.Result.SigmaHM)
	}
}

func TestSimnetBaselineLearned(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet")
	}
	for seed := int64(21); seed < 23; seed++ {
		sc := ssrsim.Baseline(seed)
		cfg := ssr.DefaultConfig()
		o := ssrsim.Evaluate(sc, cfg, ssr.Hint{})
		t.Logf("seed %d: err_h %.1f m err_u %.1f m σ_h %.1f m used %d/%d radars %d radarErr %v", seed, o.ErrHM, o.ErrUM,
			o.Result.SigmaHM, o.Result.NUsed, o.Result.NObs, o.Result.NRadars, o.RadarErrM)
		if !o.Result.OK {
			t.Fatalf("seed %d: %s", seed, o.Result.Reason)
		}
		// Budget: 25 m horizontal after 10 min, consistent with σ_h.
		if o.ErrHM > 25 || o.ErrHM > 4*o.Result.SigmaHM {
			t.Fatalf("seed %d: err_h %.1f m, σ_h %.1f m", seed, o.ErrHM, o.Result.SigmaHM)
		}
		for _, ri := range o.Result.Radars {
			best := math.Inf(1)
			for _, tp := range o.Truth.Radars {
				best = math.Min(best, tp.Dist(ri.ECEF))
			}
			t.Logf("  interrogator obs %d lines %d period %.3f coh %.2f nearest truth %.0f m", ri.Obs, ri.Lines, ri.Rotation.PeriodS, ri.Rotation.Coherence, best)
		}
		if o.Result.NRadars != 3 {
			t.Fatalf("seed %d: %d interrogators", seed, o.Result.NRadars)
		}
		for i, e := range o.RadarErrM {
			if !(e < 3000) {
				t.Fatalf("seed %d: radar %d learned %.0f m off", seed, i, e)
			}
		}
	}
}

func TestSimnetLatencyAndRTL(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet")
	}
	sc := ssrsim.Baseline(31)
	sc.LatencyFrac, sc.LatencyMaxS = 0.4, 0.6
	sc.RxJitterACs = 150
	cfg := ssr.DefaultConfig()
	cfg.TimingSigmaACNs = 180
	o := ssrsim.Evaluate(sc, cfg, ssr.Hint{})
	t.Logf("err_h %.1f σ_h %.1f", o.ErrHM, o.Result.SigmaHM)
	if !o.Result.OK || o.ErrHM > 30 {
		t.Fatalf("ok=%v %q err_h %.1f", o.Result.OK, o.Result.Reason, o.ErrHM)
	}
}

// The negative result: Mode S interrogators with II lockout give (almost)
// no common-interrogation pairs.
func TestSimnetModeSLockoutNegative(t *testing.T) {
	sc := ssrsim.Baseline(41)
	sc.DurS = 300
	for i := range sc.Radars {
		sc.Radars[i].Modes = []ssrsim.Mode{ssrsim.ModeSAllCall}
		sc.Radars[i].PRFHz = 60
		sc.Radars[i].LockoutFrac = 0.9
		sc.Radars[i].IID = uint8(i + 1)
	}
	o := ssrsim.Evaluate(sc, ssr.DefaultConfig(), ssr.Hint{})
	if o.Result.OK {
		t.Fatalf("lockout scenario produced a fix (%d obs)", o.Result.NObs)
	}
	if o.Est.Observations > 20 {
		t.Fatalf("lockout scenario: %d observations", o.Est.Observations)
	}
}

func TestSimnetModeSNoLockout(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet")
	}
	sc := ssrsim.Baseline(51)
	for i := range sc.Radars {
		sc.Radars[i].Modes = []ssrsim.Mode{ssrsim.ModeSAllCall}
		sc.Radars[i].PRFHz = 100
		sc.Radars[i].IID = uint8(i + 1)
	}
	o := ssrsim.Evaluate(sc, ssr.DefaultConfig(), ssr.Hint{})
	t.Logf("err_h %.1f σ_h %.1f obs %d", o.ErrHM, o.Result.SigmaHM, o.Result.NUsed)
	if !o.Result.OK || o.ErrHM > 40 {
		t.Fatalf("ok=%v %q err_h %.1f", o.Result.OK, o.Result.Reason, o.ErrHM)
	}
}

func TestSimDeterministic(t *testing.T) {
	sc := ssrsim.Baseline(7)
	sc.DurS = 20
	var n1, n2 int
	_, s1 := ssrsim.Run(sc, func(ssrsim.Event) { n1++ })
	_, s2 := ssrsim.Run(sc, func(ssrsim.Event) { n2++ })
	if n1 != n2 || s1 != s2 || s1.Emitted == 0 || s1.Positions == 0 {
		t.Fatalf("not deterministic or empty: %d %d %+v %+v", n1, n2, s1, s2)
	}
}

func TestEstimatorConcurrentAndBounded(t *testing.T) {
	sc := ssrsim.Baseline(8)
	sc.DurS = 120
	cfg := ssr.DefaultConfig()
	cfg.MaxObs = 1000 // minimum: forces thinning below
	e := ssr.New(cfg)
	var evs []ssrsim.Event
	ssrsim.Run(sc, func(ev ssrsim.Event) { evs = append(evs, ev) })
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for _, ev := range evs {
			if ev.IsPos {
				e.AddPosition(ev.ICAO, ev.Pos)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for _, ev := range evs {
			if !ev.IsPos {
				e.AddReply(ev.Reply)
			}
		}
	}()
	wg.Wait()
	e.Process(math.Inf(1))
	st := e.Stats()
	if st.Replies == 0 || st.Observations == 0 || st.Observations > 1000 {
		t.Fatalf("stats %+v", st)
	}
	_ = e.Solve(ssr.Hint{})
	e.Reset()
	if e.Stats().Observations != 0 {
		t.Fatal("Reset")
	}
}

var benchObs []ssr.PairObs

func benchObservations(b *testing.B) []ssr.PairObs {
	if benchObs == nil {
		e := ssr.New(ssr.DefaultConfig())
		next := math.Inf(-1)
		ssrsim.Run(ssrsim.Baseline(99), func(ev ssrsim.Event) {
			if ev.IsPos {
				e.AddPosition(ev.ICAO, ev.Pos)
			} else {
				e.AddReply(ev.Reply)
			}
			if ev.T >= next {
				e.Process(ev.T)
				next = ev.T + 1
			}
		})
		e.Process(math.Inf(1))
		benchObs = e.Observations()
	}
	return benchObs
}

// BenchmarkSolve: a 10-minute baseline (~1700 observations, ~110 airframes,
// 3 learned interrogators). Budget (SSR-RESEARCH.md §7): 3 s per solve.
func BenchmarkSolve(b *testing.B) {
	obs := benchObservations(b)
	cfg := ssr.DefaultConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := ssr.SolveObs(obs, ssr.Hint{}, cfg, geo.DefaultAtmosphere())
		if !r.OK {
			b.Fatal(r.Reason)
		}
	}
	b.ReportMetric(float64(len(obs)), "obs")
}

// BenchmarkIngest: replies/s through AddReply + Process (budget 200k/s,
// 50x a busy receiver's Mode A/C rate).
func BenchmarkIngest(b *testing.B) {
	sc := ssrsim.Baseline(98)
	sc.DurS = 60
	var evs []ssrsim.Event
	ssrsim.Run(sc, func(ev ssrsim.Event) { evs = append(evs, ev) })
	nrep := 0
	for _, ev := range evs {
		if !ev.IsPos {
			nrep++
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := ssr.New(ssr.DefaultConfig())
		next := math.Inf(-1)
		for _, ev := range evs {
			if ev.IsPos {
				e.AddPosition(ev.ICAO, ev.Pos)
			} else {
				e.AddReply(ev.Reply)
			}
			if ev.T >= next {
				e.Process(ev.T)
				next = ev.T + 1
			}
		}
		e.Process(math.Inf(1))
	}
	b.ReportMetric(float64(nrep)*float64(b.N)/b.Elapsed().Seconds(), "replies/s")
}
