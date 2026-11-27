package clock

import (
	"math"
	"testing"

	"airview/internal/model"
)

// Robustness regression tests for the anchor and blacklist rules
// (findings ROB-1, ROB-2, ROB-5). Each uses the synthetic world of
// synth_test.go: genuine GPS Radarcapes, RTL-SDR clocks and one or more
// misbehaving stations.

// fakeAnchorWorld: 4 genuine GPS stations, 8 SDR, and one GPS-claiming
// station whose clock is UTC + offS.
func fakeAnchorWorld(seed uint64, offS float64) (*synthWorld, *synthStation) {
	w := newSynth(seed)
	for i := 0; i < 4; i++ {
		w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*1.6), 9+1.2*math.Sin(float64(i)*1.6))
	}
	for i := 0; i < 8; i++ {
		w.addStation(model.ClockSDR12, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
	}
	bad := w.addStation(model.ClockGPSUTC, 50.1, 9.1)
	bad.clk.fault = func(float64) float64 { return offS }
	w.addAircraft(60, 50, 9, 200)
	return w, bad
}

// checkTimeBase fails when network time is off true time (seen through
// genuine station 0), when a genuine GPS station was leap-corrected, or
// when the offset station anchors without having been corrected to server
// time first (a k·1 s leap that agrees with server time is the legacy-
// firmware rule and harmless: its stamps are then true time).
func checkTimeBase(t *testing.T, f *Filter, w *synthWorld, bad *synthStation, tE float64, label string) {
	t.Helper()
	snap := f.Snapshot()
	if sb := snap.Station(bad.idx); sb != nil && sb.Anchor {
		corrected := false
		for _, e := range sb.Events {
			if e.Reason == "leap" {
				corrected = true
			}
		}
		if !corrected {
			t.Errorf("%s t=%.0f: the fake-GPS station is an anchor (%s) in its own time base", label, tE, sb.AnchorState)
		}
	}
	for _, s := range w.sts[:4] {
		if sc := snap.Station(s.idx); sc != nil {
			for _, e := range sc.Events {
				if e.Reason == "leap" {
					t.Errorf("%s t=%.0f: genuine station %d was leap-corrected (%s)", label, tE, s.idx, e.Detail)
				}
			}
		}
	}
	e, _, ok := w.errNs(f, w.sts[0], tE)
	s0 := snap.Station(w.sts[0].idx)
	if ok && s0 != nil && !s0.RelativeTime && math.Abs(e) > 1e5 {
		t.Errorf("%s t=%.0f: network time - UTC = %.4g ns", label, tE, e)
	}
}

// ROB-1, AVF variant: the attacker's status bits claim lock from t = 0,
// the genuine stations gain lock 5 minutes later. Before the fix the
// attacker became the self-asserted first anchor at t = 600 and the
// genuine stations were "leap-corrected" by -3 s into its time base.
func TestFakeGPSFirstAnchorAVF(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic 1300 s run")
	}
	w, bad := fakeAnchorWorld(3, 3)
	f, _ := New(Config{OriginUnixNs: synthOrigin, Workers: 1})
	for _, s := range w.sts {
		f.SetStation(s.idx, StationInfo{Class: s.clk.cls, Pos: s.pos, Located: true})
	}
	tick := func(tt float64) {
		for _, s := range w.sts[:4] {
			f.GPS(s.idx, GPSStatus{HostNs: synthOrigin + int64(tt*1e9), Locked: tt >= 300, UTC: true})
		}
		f.GPS(bad.idx, GPSStatus{HostNs: synthOrigin + int64(tt*1e9), Locked: true, UTC: true})
	}
	tick(0)
	last := 0.0
	w.run(f, 0, 1300, 150, 0, func(tE float64) {
		if tE-last >= 1 {
			tick(tE)
			last = tE
		}
		if math.Mod(tE, 100) == 0 {
			checkTimeBase(t, f, w, bad, tE, "avf")
		}
	})
	anchors := 0
	for _, s := range w.sts[:4] {
		if sc := f.Snapshot().Station(s.idx); sc != nil && sc.Anchor {
			anchors++
		}
	}
	if anchors < 3 {
		t.Errorf("only %d of 4 genuine GPS stations are anchors at the end", anchors)
	}
}

// ROB-1, mlat-client variant: a NoStatus radarcape_gps station skips the
// status check; offsets of +1, +10 and -2 s must all be refused.
func TestFakeGPSFirstAnchorNoStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic runs")
	}
	for _, off := range []float64{1, 10, -2} {
		w, bad := fakeAnchorWorld(3, off)
		f, _ := New(Config{OriginUnixNs: synthOrigin, Workers: 1})
		for _, s := range w.sts {
			f.SetStation(s.idx, StationInfo{Class: s.clk.cls, Pos: s.pos, Located: true, NoStatus: s == bad})
		}
		tick := func(tt float64) {
			for _, s := range w.sts[:4] {
				f.GPS(s.idx, GPSStatus{HostNs: synthOrigin + int64(tt*1e9), Locked: true, UTC: true})
			}
		}
		tick(0)
		last := 0.0
		w.run(f, 0, 900, 150, 0, func(tE float64) {
			if tE-last >= 1 {
				tick(tE)
				last = tE
			}
			if math.Mod(tE, 100) == 0 {
				checkTimeBase(t, f, w, bad, tE, "nostatus")
			}
		})
		if sc := f.Snapshot().Station(w.sts[0].idx); sc == nil || !sc.Anchor {
			t.Errorf("offset %+.0f s: genuine station 0 is not an anchor at the end", off)
		}
	}
}

// ROB-1: a genuine station of the GPS time base (GPS time = UTC + 18 s,
// class gps_gpst) is still leap-corrected against validated anchors: the
// leap brings it to server time.
func TestGPSTimeLeapStillApplied(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic run")
	}
	w := newSynth(4)
	for i := 0; i < 4; i++ {
		w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*1.6), 9+1.2*math.Sin(float64(i)*1.6))
	}
	for i := 0; i < 6; i++ {
		w.addStation(model.ClockSDR12, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
	}
	g := w.addStation(model.ClockGPSUTC, 50.2, 9.2)
	g.clk.fault = func(float64) float64 { return 18 }
	w.addAircraft(60, 50, 9, 200)
	f, _ := New(Config{OriginUnixNs: synthOrigin, Workers: 1, ProbationS: 120})
	for _, s := range w.sts {
		cls := s.clk.cls
		if s == g {
			cls = model.ClockGPSGPST
		}
		f.SetStation(s.idx, StationInfo{Class: cls, Pos: s.pos, Located: true})
	}
	tick := func(tt float64) {
		for _, s := range w.sts {
			if s.clk.gps {
				f.GPS(s.idx, GPSStatus{HostNs: synthOrigin + int64(tt*1e9), Locked: true, UTC: s != g})
			}
		}
	}
	tick(0)
	last := 0.0
	w.run(f, 0, 600, 150, 0, func(tE float64) {
		if tE-last >= 1 {
			tick(tE)
			last = tE
		}
	})
	leap := false
	if sc := f.Snapshot().Station(g.idx); sc != nil {
		for _, e := range sc.Events {
			if e.Reason == "leap" && e.Detail == "18 s" {
				leap = true
			}
		}
	}
	if !leap {
		dumpEvents(t, f, g.idx)
		t.Fatal("the GPS-time station was not leap-corrected by 18 s")
	}
}

// ROB-5: a short GPS lock loss of all anchors must not strip them for
// good: once condition (a) holds again they re-enter probation and are
// promoted again.
func TestAnchorsRecoverAfterLockLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic 1500 s run")
	}
	w := newSynth(5)
	for i := 0; i < 3; i++ {
		w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*1.6), 9+1.2*math.Sin(float64(i)*1.6))
	}
	for i := 0; i < 8; i++ {
		w.addStation(model.ClockSDR12, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
	}
	w.addAircraft(60, 50, 9, 200)
	f, _ := New(Config{OriginUnixNs: synthOrigin, Workers: 1})
	w.register(f)
	tick := func(tt float64) {
		lock := !(tt >= 700 && tt < 720)
		for _, s := range w.sts[:3] {
			f.GPS(s.idx, GPSStatus{HostNs: synthOrigin + int64(tt*1e9), Locked: lock, UTC: true})
		}
	}
	tick(0)
	last := 0.0
	demoted := false
	w.run(f, 0, 1500, 150, 0, func(tE float64) {
		if tE-last >= 1 {
			tick(tE)
			last = tE
		}
		if tE == 710 {
			if sc := f.Snapshot().Station(w.sts[0].idx); sc != nil && !sc.Anchor {
				demoted = true
			}
		}
	})
	if !demoted {
		t.Fatal("the lock loss did not demote the anchor (test premise)")
	}
	for _, s := range w.sts[:3] {
		if sc := f.Snapshot().Station(s.idx); sc == nil || !sc.Anchor {
			dumpEvents(t, f, s.idx)
			t.Errorf("station %d is not an anchor again 780 s after its lock returned", s.idx)
		}
	}
}

// ROB-2: two stations with 20 us timestamp noise used to blacklist every
// aircraft from sync within three minutes, and with them all SDR sync.
func TestNoisyStationsDoNotBlacklistNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic 600 s run")
	}
	for _, nBad := range []int{2, 3} {
		w := newSynth(9)
		for i := 0; i < 4; i++ {
			w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*1.6), 9+1.2*math.Sin(float64(i)*1.6))
		}
		for i := 0; i < 10; i++ {
			w.addStation(model.ClockSDR12, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
		}
		nGood := len(w.sts)
		for i := 0; i < nBad; i++ {
			b := w.addStation(model.ClockSDR12, 50.0+0.01*float64(i), 9.0)
			b.clk.jitter = 20e-6
		}
		w.addAircraft(60, 50, 9, 200)
		f, _ := New(Config{OriginUnixNs: synthOrigin, ProbationS: 60, Workers: 1})
		w.register(f)
		maxBlk, minSyn := 0, 10
		w.run(f, 0, 600, 150, 0, func(tE float64) {
			if math.Mod(tE, 60) != 0 || tE < 120 {
				return
			}
			syn := 0
			for _, s := range w.sts[4:nGood] {
				if sc := f.Snapshot().Station(s.idx); sc != nil && sc.Synced {
					syn++
				}
			}
			maxBlk = max(maxBlk, f.Stats().BlacklistedAc)
			minSyn = min(minSyn, syn)
		})
		t.Logf("%d noisy stations: max blacklisted %d/60, min genuine SDR synced %d/10, capped %d",
			nBad, maxBlk, minSyn, f.Stats().BlacklistCapped)
		if maxBlk > 12 {
			t.Errorf("%d noisy stations: up to %d of 60 aircraft blacklisted (cap is 20 %%)", nBad, maxBlk)
		}
		if minSyn < 9 {
			t.Errorf("%d noisy stations: only %d of 10 genuine SDR stations stayed synced", nBad, minSyn)
		}
	}
}

// ROB-2: the blacklist cap holds even when every aircraft fails.
func TestBlacklistCap(t *testing.T) {
	f, _ := New(Config{OriginUnixNs: synthOrigin, Workers: 1})
	c := &component{}
	for k := uint32(1); k <= 50; k++ {
		for i := 0; i < 12; i++ {
			c.acRes = append(c.acRes, acftResult{key: k, fail: true})
		}
	}
	f.comps = []*component{c}
	f.blacklist(1)
	if n := f.Stats().BlacklistedAc; n != 10 {
		t.Fatalf("%d of 50 aircraft blacklisted, want the cap of 10", n)
	}
	if f.Stats().BlacklistCapped != 40 {
		t.Fatalf("capped %d, want 40", f.Stats().BlacklistCapped)
	}
}
