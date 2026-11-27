package clock

import (
	"math"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Two clusters 900 km apart form two anchored time domains; aircraft over
// the gap link them into one; when the link is gone they split again.
func TestComponentsSplitMerge(t *testing.T) {
	w := newSynth(8)
	for _, lon := range []float64{4, 13} {
		for i := 0; i < 3; i++ {
			w.addStation(model.ClockGPSUTC, 50+0.6*math.Cos(float64(i)*2), lon+0.9*math.Sin(float64(i)*2))
		}
		for i := 0; i < 4; i++ {
			w.addStation(model.ClockSDR12, 50+(w.rng.Float64()-0.5)*1.5, lon+(w.rng.Float64()-0.5)*2)
		}
	}
	// aircraft near each cluster only
	w.addAircraft(30, 50, 4, 120)
	w.addAircraft(30, 50, 13, 120)
	f, _ := New(Config{OriginUnixNs: synthOrigin, ProbationS: 30, Workers: 2, CoObsWindowS: 20, StationTimeoutS: 30})
	w.register(f)
	w.run(f, 0, 90, 150, 0.2, nil)
	domains := func() map[uint32]int {
		d := map[uint32]int{}
		for _, c := range f.Snapshot().Components {
			d[c.Domain] += len(c.Stations)
		}
		return d
	}
	if d := domains(); len(d) != 2 {
		t.Fatalf("separate clusters: domains %v", d)
	}
	for _, c := range f.Snapshot().Components {
		if c.Relative || c.Anchors != 3 {
			t.Errorf("component %d: relative %v anchors %d", c.ID, c.Relative, c.Anchors)
		}
	}
	// aircraft midway, high: heard by both clusters
	keep := w.acs
	w.acs = nil
	w.addAircraft(30, 50, 8.5, 60)
	for i := range w.acs {
		w.acs[i].p0 = geo.ToECEF(50+(w.rng.Float64()-0.5), 8.5+(w.rng.Float64()-0.5)*2, 12000)
		w.acs[i].v = geo.Vec3{}
	}
	w.posS = 5
	w.run(f, 90, 150, 150, 0.2, nil)
	if d := domains(); len(d) != 1 {
		t.Errorf("linked clusters: domains %v", d)
	}
	synced := 0
	for _, s := range w.sts {
		if sc := f.Snapshot().Station(s.idx); sc != nil && sc.Synced {
			synced++
		}
	}
	if synced < len(w.sts)-2 {
		t.Errorf("after merge %d/%d synced", synced, len(w.sts))
	}
	// back to local traffic: the link expires (CoObsWindowS) and they split
	w.acs = keep
	w.run(f, 150, 220, 150, 0.2, nil)
	if d := domains(); len(d) != 2 {
		t.Errorf("after the link expired: domains %v", d)
	}
	if st := f.Stats(); st.UpdateFailures > 0 || st.Steps > 0 {
		t.Errorf("stats %+v", st)
	}
}

// An aircraft whose reference position is 3 km off is blacklisted from sync
// and the clocks stay correct.
func TestAircraftBlacklist(t *testing.T) {
	w := newSynth(9)
	for i := 0; i < 4; i++ {
		w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*1.6), 9+1.2*math.Sin(float64(i)*1.6))
	}
	for i := 0; i < 6; i++ {
		w.addStation(model.ClockHW12, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
	}
	w.addAircraft(8, 50, 9, 150)
	var suspended []uint32
	f, _ := New(Config{OriginUnixNs: synthOrigin, ProbationS: 30, Workers: 1,
		OnBlacklist: func(key uint32, until int64) { suspended = append(suspended, key) }})
	w.register(f)
	w.run(f, 0, 60, 60, 0, nil)
	bad := w.acs[0].key
	// from now on aircraft 0 reports a position 3 km off
	w.badKey, w.badOff = bad, geo.Vec3{X: 3000}
	w.run(f, 60, 240, 60, 0, nil)
	f.mu.Lock()
	_, black := f.black[bad]
	f.mu.Unlock()
	if !black || len(suspended) == 0 || suspended[0] != bad {
		t.Errorf("aircraft %06X not blacklisted (callback %v); stats %+v", bad, suspended, f.Stats())
	}
	for _, s := range w.sts {
		e, sg, _ := w.errNs(f, s, f.Snapshot().T)
		cm, _, _ := w.errNs(f, w.sts[0], f.Snapshot().T)
		if math.Abs(e-cm) > 4*sg+5 {
			t.Errorf("station %d error %.1f σ %.1f", s.idx, e-cm, sg)
		}
	}
}

// A class-2 (GPS time) station 18 s ahead of the UTC anchors gets the leap
// offset applied, becomes class 1 and is promoted after probation.
func TestLeapOffsetAndProbation(t *testing.T) {
	w := newSynth(10)
	for i := 0; i < 3; i++ {
		w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*2), 9+1.2*math.Sin(float64(i)*2))
	}
	g2 := w.addStation(model.ClockGPSGPST, 50.2, 9.3)
	g2.clk.theta0 += 18
	for i := 0; i < 4; i++ {
		w.addStation(model.ClockSDR12, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
	}
	w.addAircraft(40, 50, 9, 200)
	f, _ := New(Config{OriginUnixNs: synthOrigin, ProbationS: 60, Workers: 1})
	w.register(f)
	w.run(f, 0, 200, 120, 0.2, nil)
	sc := f.Snapshot().Station(g2.idx)
	t.Logf("class %s anchor %s events %+v", sc.Class, sc.AnchorState, sc.Events)
	if sc.Class != model.ClockGPSUTC || !sc.Anchor {
		t.Errorf("GPS-time station: class %s anchor %v", sc.Class, sc.Anchor)
	}
	e, sg, _ := w.errNs(f, g2, f.Snapshot().T)
	cm, _, _ := w.errNs(f, w.sts[0], f.Snapshot().T)
	if math.Abs(e-cm) > 4*sg+5 {
		t.Errorf("after the leap offset: error %.1f ns σ %.1f", e-cm, sg)
	}
}

// An anchor that slowly drifts (360 ns/h, below the step detector) is
// demoted by the drift monitor.
func TestAnchorDriftDemotion(t *testing.T) {
	w := newSynth(12)
	for i := 0; i < 5; i++ {
		w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*1.3), 9+1.2*math.Sin(float64(i)*1.3))
	}
	for i := 0; i < 5; i++ {
		w.addStation(model.ClockHW12, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
	}
	w.addAircraft(40, 50, 9, 200)
	drifter := w.sts[2]
	const t0 = 200.0
	drifter.clk.fault = func(t float64) float64 {
		if t < t0 {
			return 0
		}
		return 1e-10 * (t - t0) // 0.1 ppb = 360 ns/h
	}
	f, _ := New(Config{OriginUnixNs: synthOrigin, ProbationS: 60, Workers: 1})
	w.register(f)
	demoted := -1.0
	w.run(f, 0, t0+1500, 80, 0.2, func(tE float64) {
		sc := f.Snapshot().Station(drifter.idx)
		if demoted < 0 && tE > t0 && sc != nil && sc.AnchorState == AnchorDemoted {
			demoted = tE
		}
	})
	sc := f.Snapshot().Station(drifter.idx)
	t.Logf("drifting anchor demoted %.0f s after the drift started (%s)", demoted-t0, sc.AnchorReason)
	if demoted < 0 || sc.AnchorReason != "drift" {
		t.Errorf("drifting anchor not demoted for drift (state %s, reason %s)", sc.AnchorState, sc.AnchorReason)
	}
	for _, s := range w.sts {
		if s != drifter && !f.Snapshot().Station(s.idx).Anchor && s.clk.cls.GPS() {
			t.Errorf("good anchor %d demoted", s.idx)
		}
	}
}
