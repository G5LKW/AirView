package ssr_test

import (
	"math"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/ssr"
	"airview/internal/mlat/ssr/ssrsim"
)

// modeSScenario: Mode S radars that only send all-calls (DF11 with their
// interrogator code), half of the aircraft locked out of each, no Mode A/C:
// the environment where the pair method of SSR-RESEARCH.md finds nothing.
func modeSScenario(seed int64) ssrsim.Scenario {
	rx := geo.LLH{Lat: 52, Lon: -1, H: 30}
	stag := []float64{0.93, 1.05, 0.98, 1.07, 0.95, 1.02}
	allCall := []ssrsim.Mode{ssrsim.ModeSAllCall}
	return ssrsim.Scenario{
		Seed:          seed,
		Receiver:      rx,
		ClockOffsetS:  1234.5,
		ClockDriftPPM: 25,
		Radars: []ssrsim.Radar{
			{Name: "A", Pos: ssrsim.Offset(rx, 40, 60e3, 80), PeriodS: 4.8, Az0Deg: 10, PRFHz: 120, Stagger: stag, Modes: allCall, IID: 3, LockoutFrac: 0.5},
			{Name: "B", Pos: ssrsim.Offset(rx, 210, 140e3, 150), PeriodS: -8, Az0Deg: 200, PRFHz: 90, Stagger: stag, Modes: allCall, IID: 16 + 12, LockoutFrac: 0.5},
			// C shares II 3 with A, far away (codes are reused).
			{Name: "C", Pos: ssrsim.Offset(rx, 290, 230e3, 60), PeriodS: 10, Az0Deg: 99, PRFHz: 100, Stagger: stag, Modes: allCall, IID: 3, LockoutFrac: 0.5},
		},
		Aircraft: 150,
		DurS:     900,
	}
}

func runSweep(t *testing.T, sc ssrsim.Scenario, cfg ssr.Config, h ssr.Hint) ([]ssr.SweepRadar, ssr.Stats) {
	t.Helper()
	e := ssr.New(cfg)
	next := math.Inf(-1)
	ssrsim.Run(sc, func(ev ssrsim.Event) {
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
	return e.LocateRadars(h), e.Stats()
}

func TestLocateBySweepModeS(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	sc := modeSScenario(7)
	cfg := ssr.DefaultConfig()
	cfg.Sites = []ssr.Site{{Name: "ref-A", Lat: sc.Radars[0].Pos.Lat, Lon: sc.Radars[0].Pos.Lon}}
	h := ssr.Hint{Valid: true, Pos: sc.Receiver, RadiusM: 50e3}
	got, st := runSweep(t, sc, cfg, h)
	t.Logf("stats: dwells %d, sweep bursts %d, pair matches %d", st.Dwells, st.SweepObs, st.PairMatches)
	if st.SweepObs < 200 {
		t.Fatalf("only %d sweep bursts", st.SweepObs)
	}
	for _, r := range got {
		t.Logf("%s ok=%v %q at %.4f %.4f σ %.0f/%.0f m P %.4f±%.4f coh %.2f rms %.2f° used %d/%d ac %d spread %.0f° ref %q %.0f m",
			r.Code, r.OK, r.Reason, r.LLH.Lat, r.LLH.Lon, r.SemiMajorM, r.SemiMinorM, r.PeriodS, r.PeriodSigmaS,
			r.Coherence, r.RMSDeg, r.NUsed, r.NObs, r.NAircraft, r.SpreadDeg, r.Ref, r.RefDistM)
	}
	for _, rd := range sc.Radars {
		want := rd.Pos.ECEF()
		found := false
		for _, r := range got {
			if !r.OK || r.IID != rd.IID {
				continue
			}
			f := geo.NewFrameECEF(want)
			d := f.ToENU(r.LLH.ECEF())
			e := math.Hypot(d.X, d.Y)
			if e > 3000 {
				continue
			}
			found = true
			if e > 5*r.SigmaHM+300 {
				t.Errorf("%s: error %.0f m but σ %.0f m", rd.Name, e, r.SigmaHM)
			}
			if math.Abs(r.PeriodS-rd.PeriodS) > 0.01*math.Abs(rd.PeriodS) {
				t.Errorf("%s: period %.3f, want %.3f", rd.Name, r.PeriodS, rd.PeriodS)
			}
			t.Logf("%s located %.0f m from the truth (σ_h %.0f m)", rd.Name, e, r.SigmaHM)
		}
		if !found {
			t.Errorf("radar %s (%s) not located", rd.Name, ssr.CodeName(rd.IID))
		}
	}
	for _, r := range got {
		if r.OK && r.IID == 3 && r.Ref == "ref-A" && r.RefDistM < 3000 {
			return
		}
	}
	t.Error("radar A not matched to its reference site")
}

// Full lockout: no repeat bursts, so no period and no site, with a reason.
func TestLocateBySweepLockout(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	sc := modeSScenario(3)
	sc.DurS = 300
	for i := range sc.Radars {
		sc.Radars[i].LockoutFrac = 1
	}
	got, _ := runSweep(t, sc, ssr.DefaultConfig(), ssr.Hint{})
	for _, r := range got {
		if r.OK {
			t.Errorf("%s located with every aircraft locked out", r.Code)
		}
	}
}

func TestCodeName(t *testing.T) {
	for iid, want := range map[uint8]string{1: "II 1", 15: "II 15", 16: "SI 0", 28: "SI 12", 79: "SI 63"} {
		if got := ssr.CodeName(iid); got != want {
			t.Errorf("CodeName(%d) = %q, want %q", iid, got, want)
		}
	}
}

func TestLocateBySweepEmpty(t *testing.T) {
	if got := ssr.LocateBySweep(nil, ssr.Hint{}, ssr.DefaultConfig()); got != nil {
		t.Fatalf("got %v", got)
	}
	// Too few bursts: one result with a reason, not OK.
	obs := []ssr.SweepObs{{T: 1, ICAO: 1, IID: 2, P: geo.ToECEF(52, -1, 10000)}}
	got := ssr.LocateBySweep(obs, ssr.Hint{}, ssr.DefaultConfig())
	if len(got) != 1 || got[0].OK || got[0].Reason == "" || got[0].Code != "II 2" {
		t.Fatalf("got %+v", got)
	}
}

// BenchmarkLocateBySweep: the whole locator on 15 minutes of the
// three-radar Mode S scenario (two codes, three fits).
func BenchmarkLocateBySweep(b *testing.B) {
	sc := modeSScenario(7)
	e := ssr.New(ssr.DefaultConfig())
	next := math.Inf(-1)
	ssrsim.Run(sc, func(ev ssrsim.Event) {
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
	obs := e.SweepObservations()
	cfg := ssr.DefaultConfig()
	h := ssr.Hint{Valid: true, Pos: sc.Receiver}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ssr.LocateBySweep(obs, h, cfg)
	}
}
