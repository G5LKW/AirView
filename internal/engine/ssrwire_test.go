package engine

import (
	"math"
	"reflect"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/ssr"
	"airview/internal/mlat/ssr/ssrsim"
	"airview/internal/mlat/track"
	"airview/internal/model"
	"airview/internal/modes"
)

// TestSSRConfigFromModel: every scalar of ssr.Config has a file setting of
// the same name and kind (so nothing is silently unconfigurable), and the
// mapping carries values, sites and codes.
func TestSSRConfigFromModel(t *testing.T) {
	ct := reflect.TypeOf(ssr.Config{})
	mt := reflect.TypeOf(model.SSRConfig{})
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		switch f.Type.Kind() {
		case reflect.Bool, reflect.Int, reflect.Float64:
			mf, ok := mt.FieldByName(f.Name)
			if !ok || mf.Type.Kind() != f.Type.Kind() {
				t.Errorf("ssr.Config.%s (%s) has no model.SSRConfig counterpart", f.Name, f.Type)
			}
		}
	}
	m := model.SSRConfig{Enabled: true, MatchTolNs: 300, MinMatches: 6, MaxSigmaHM: 250, NoLatency: true,
		Sites: []model.SSRSite{{Name: "X", Lat: 51, Lon: -1, HeightM: 90}}, ConspicuityCodes: []string{"7000", "2000"}}
	c := ssrConfigFromModel(m)
	if !c.Enabled || c.MatchTolNs != 300 || c.MinMatches != 6 || c.MaxSigmaHM != 250 || !c.NoLatency {
		t.Fatalf("scalars not mapped: %+v", c)
	}
	if len(c.Sites) != 1 || c.Sites[0].Name != "X" || c.Sites[0].HeightM != 90 {
		t.Fatalf("sites %+v", c.Sites)
	}
	if !reflect.DeepEqual(c.ConspicuityCodes, []uint16{0o7000, 0o2000}) {
		t.Fatalf("codes %o", c.ConspicuityCodes)
	}
	if d := ssrConfigFromModel(model.SSRConfig{}); d.MatchTolNs != 400 || d.Enabled {
		t.Fatalf("zero block is not the package default: %+v", d)
	}
}

// --- frame encoders (the inverse of what the wiring decodes) ---

func es17(icao uint32, me uint64) []byte {
	f := make([]byte, 14)
	f[0] = 17<<3 | 5
	f[1], f[2], f[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(me >> (48 - 8*i))
	}
	modes.SetParity(f, 0)
	return f
}

func posFrame(icao uint32, lat, lon, altFt float64, odd bool) []byte {
	ac, _ := modes.EncodeAC12(int32(math.Round(altFt)))
	cl, cn := modes.CPREncode(lat, lon, odd, false)
	var fbit uint64
	if odd {
		fbit = 1
	}
	me := uint64(11)<<51 | uint64(ac)<<36 | fbit<<34 | uint64(cl)<<17 | uint64(cn)
	return es17(icao, me)
}

func df5(icao uint32, squawkOctal uint16) []byte {
	nib := squawkOctal>>9&7<<12 | squawkOctal>>6&7<<8 | squawkOctal>>3&7<<4 | squawkOctal&7
	w := uint32(5)<<27 | uint32(modes.EncodeID13(nib))
	f := []byte{byte(w >> 24), byte(w >> 16), byte(w >> 8), byte(w), 0, 0, 0}
	modes.SetParity(f, icao)
	return f
}

func df11(icao uint32, iid uint8) []byte {
	f := []byte{11<<3 | 5, byte(icao >> 16), byte(icao >> 8), byte(icao), 0, 0, 0}
	modes.SetParity(f, uint32(iid))
	return f
}

// ssrFrames renders an ssrsim run as the Beast frames of one receiver with
// a 12 MHz counter (what the wiring sees from a Beast feed).
func ssrFrames(sc ssrsim.Scenario, emit func(model.RawFrame)) ssrsim.Truth {
	lastSq := map[uint32]float64{}
	odd := map[uint32]bool{}
	mk := func(t float64, kind model.FrameKind, data []byte) model.RawFrame {
		return model.RawFrame{Kind: kind, Data: data, GPSNs: -1, Clock12MHz: uint64(math.Round((t + 1000) * 12e6)),
			FeedID: "rx", Signal: math.NaN(), Channel: -1}
	}
	tr, _ := ssrsim.Run(sc, func(ev ssrsim.Event) {
		if ev.IsPos {
			p := geo.FromECEF(ev.Pos.P)
			o := odd[ev.ICAO]
			odd[ev.ICAO] = !o
			emit(mk(ev.T, model.FrameModeSLong, posFrame(ev.ICAO, p.Lat, p.Lon, ev.Pos.AltBaroFt, o)))
			if ev.Pos.Squawk >= 0 && ev.T-lastSq[ev.ICAO] > 5 {
				lastSq[ev.ICAO] = ev.T
				emit(mk(ev.T+0.001, model.FrameModeSShort, df5(ev.ICAO, uint16(ev.Pos.Squawk))))
			}
			return
		}
		r := ev.Reply
		switch r.Class {
		case ssr.ClassModeAC:
			raw := track.ACFromSquawk(r.Code)
			emit(mk(r.T, model.FrameModeAC, []byte{byte(raw >> 8), byte(raw)}))
		case ssr.ClassModeS:
			emit(mk(r.T, model.FrameModeSShort, df11(r.ICAO, r.IID)))
		}
	})
	return tr
}

// TestSSRWiringBaseline runs the SSR-RESEARCH baseline scenario through the
// engine's frame decoder (Beast frames with a 12 MHz counter: CPR, DF5
// identities, Mode A/C pulses, DF11) and compares the solve with the
// reference evaluation that feeds the estimator directly.
func TestSSRWiringBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	sc := ssrsim.Baseline(21)
	hint := ssr.Hint{Valid: true, Pos: ssrsim.Offset(sc.Receiver, 70, 15e3, 0), RadiusM: 50e3}
	ref := ssrsim.Evaluate(sc, ssr.DefaultConfig(), hint)

	w := &ssrWiring{cfgM: model.SSRConfig{Enabled: true}, est: ssr.New(ssr.DefaultConfig()), ac: map[uint32]*ssrAircraft{}}
	next := math.Inf(-1)
	tr := ssrFrames(sc, func(f model.RawFrame) {
		w.observe(&f)
		if w.now >= next {
			w.tick()
			next = w.now + 1
		}
	})
	w.est.Process(math.Inf(1))
	res := w.est.Solve(hint)
	errH := math.NaN()
	if res.ECEF != (geo.Vec3{}) {
		fr := geo.NewFrameECEF(tr.Receiver)
		d := fr.ToENU(res.ECEF)
		errH = math.Hypot(d.X, d.Y)
	}
	st := w.status()
	t.Logf("reference: ok=%v err_h %.1f m σ_h %.1f m obs %d; wiring: ok=%v %q err_h %.1f m σ_h %.1f m obs %d radars %d; frames %d positions %d replies %d pairMatches %d resolvedAC %d",
		ref.Result.OK, ref.ErrHM, ref.Result.SigmaHM, ref.Result.NObs, res.OK, res.Reason, errH, res.SigmaHM, res.NObs, res.NRadars,
		st.Frames, st.Positions, st.Replies, st.Estimator.PairMatches, st.Estimator.ResolvedAC)
	if !res.OK {
		t.Fatalf("wiring solve failed: %s", res.Reason)
	}
	if errH > 30 || errH > math.Max(3*ref.ErrHM, ref.ErrHM+15) {
		t.Fatalf("wiring err_h %.1f m (reference %.1f m)", errH, ref.ErrHM)
	}
	if float64(res.NObs) < 0.5*float64(ref.Result.NObs) {
		t.Fatalf("wiring produced %d observations, reference %d", res.NObs, ref.Result.NObs)
	}
}

// TestSSRWiringIgnoresOtherFeeds: one receiver clock only; untimed and
// foreign frames are counted as ignored, a time-base jump resets.
func TestSSRWiringIgnoresOtherFeeds(t *testing.T) {
	w := &ssrWiring{cfgM: model.SSRConfig{Enabled: true}, est: ssr.New(ssr.DefaultConfig()), ac: map[uint32]*ssrAircraft{}}
	untimed := model.RawFrame{Kind: model.FrameModeSShort, Data: df11(0x4ca123, 3), GPSNs: -1, FeedID: "sbs"}
	w.observe(&untimed)
	a := model.RawFrame{Kind: model.FrameModeSShort, Data: df11(0x4ca123, 3), GPSNs: -1, Clock12MHz: 12e6 * 100, FeedID: "a"}
	w.observe(&a)
	b := a
	b.FeedID = "b"
	w.observe(&b)
	if w.feedID != "a" || w.ignored != 2 || w.replies != 1 {
		t.Fatalf("feed %q ignored %d replies %d", w.feedID, w.ignored, w.replies)
	}
	j := a
	j.Clock12MHz = 12e6 * 5000 // receiver restarted / clock jumped
	w.observe(&j)
	if w.resets != 1 {
		t.Fatalf("resets %d", w.resets)
	}
	// 48-bit counter wrap keeps the time monotonic.
	var c ssrClock
	f := model.RawFrame{GPSNs: -1, Clock12MHz: 1<<48 - 12e6}
	t0, _ := c.local(&f)
	f.Clock12MHz = 12e6
	t1, _ := c.local(&f)
	if d := t1 - t0; math.Abs(d-2) > 1e-6 {
		t.Fatalf("wrap: %.6f s", d)
	}
	// GPS ns of day across midnight.
	c = ssrClock{}
	g := model.RawFrame{GPSNs: 86399e9}
	t0, _ = c.local(&g)
	g.GPSNs = 1e9
	t1, _ = c.local(&g)
	if d := t1 - t0; math.Abs(d-2) > 1e-6 {
		t.Fatalf("midnight: %.6f s", d)
	}
}

// BenchmarkSSRObserve is the per-frame cost of the SSR decoder on a
// realistic mix (baseline scenario: 25 % ADS-B positions, 75 % replies);
// budget 1 µs per frame on one core.
func BenchmarkSSRObserve(b *testing.B) {
	sc := ssrsim.Baseline(5)
	sc.DurS = 60
	var fs []model.RawFrame
	ssrFrames(sc, func(f model.RawFrame) { fs = append(fs, f) })
	w := &ssrWiring{cfgM: model.SSRConfig{Enabled: true}, est: ssr.New(ssr.DefaultConfig()), ac: map[uint32]*ssrAircraft{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := fs[i%len(fs)]
		if i%len(fs) == 0 && i > 0 {
			b.StopTimer()
			w = &ssrWiring{cfgM: model.SSRConfig{Enabled: true}, est: ssr.New(ssr.DefaultConfig()), ac: map[uint32]*ssrAircraft{}}
			b.StartTimer()
		}
		w.observe(&f)
	}
}

// TestSSRWiringSweep: Mode S radars that only send all-calls (no Mode A/C,
// half the aircraft locked out) are located from their beam sweeps through
// the engine's frame decoder, and the status carries them.
func TestSSRWiringSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	rx := geo.LLH{Lat: 52, Lon: -1, H: 30}
	allCall := []ssrsim.Mode{ssrsim.ModeSAllCall}
	sc := ssrsim.Scenario{
		Seed: 5, Receiver: rx, ClockOffsetS: 50, ClockDriftPPM: 20, Aircraft: 120, DurS: 600,
		Radars: []ssrsim.Radar{
			{Name: "A", Pos: ssrsim.Offset(rx, 130, 80e3, 70), PeriodS: 4.8, PRFHz: 110, Modes: allCall, IID: 7, LockoutFrac: 0.5},
			{Name: "B", Pos: ssrsim.Offset(rx, 300, 150e3, 90), PeriodS: -9, PRFHz: 90, Modes: allCall, IID: 16 + 33, LockoutFrac: 0.5},
		},
	}
	w := &ssrWiring{cfgM: model.SSRConfig{Enabled: true}, est: ssr.New(ssr.DefaultConfig()), ac: map[uint32]*ssrAircraft{},
		hint: func() ssr.Hint { return ssr.Hint{Valid: true, Pos: rx, RadiusM: 50e3} }}
	next := math.Inf(-1)
	ssrFrames(sc, func(f model.RawFrame) {
		w.observe(&f)
		if w.now >= next {
			w.tick()
			next = w.now + 1
		}
	})
	w.est.Process(math.Inf(1))
	w.solve()
	st := w.status()
	if st.Estimator.SweepBursts == 0 {
		t.Fatal("no sweep bursts")
	}
	for _, rd := range sc.Radars {
		code := ssr.CodeName(rd.IID)
		var got *SSRSweepRadarDTO
		for i := range st.Radars {
			if st.Radars[i].Code == code {
				got = &st.Radars[i]
			}
		}
		if got == nil || !got.OK || got.Lat == nil {
			t.Fatalf("%s (%s) not located: %+v", rd.Name, code, got)
		}
		f := geo.NewFrame(rd.Pos)
		d := f.ToENU(geo.ToECEF(*got.Lat, *got.Lon, rd.Pos.H))
		e := math.Hypot(d.X, d.Y)
		t.Logf("%s %s: %.0f m off, σ %.0f/%.0f m, period %.3f s, %d bursts of %d aircraft", rd.Name, code, e, got.SemiMajorM, got.SemiMinorM, got.PeriodS, got.NUsed, got.NAircraft)
		if e > 500 {
			t.Errorf("%s: %.0f m from the truth", rd.Name, e)
		}
		if math.Abs(got.PeriodS-rd.PeriodS) > 0.05 {
			t.Errorf("%s: period %.3f, want %.3f", rd.Name, got.PeriodS, rd.PeriodS)
		}
		if got.RefDistM != nil {
			t.Errorf("%s: reference distance without configured sites", rd.Name)
		}
	}
}
