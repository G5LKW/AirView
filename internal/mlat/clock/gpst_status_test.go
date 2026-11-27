package clock

import (
	"math"
	"testing"

	"airview/internal/model"
)

// TestGPSTimeWithStatusBecomesAnchor is the regression test of finding
// MLAT-R1: a GPS-time (class 2) receiver keeps reporting UTC=false in its
// status frames after the filter applied its 18 s leap. The status used to
// be judged by the mutable class (now UTC), so the station was demoted a
// second after its promotion and could never return. It must be an anchor
// after probation, survive its own status frames, and recover after a short
// lock loss.
func TestGPSTimeWithStatusBecomesAnchor(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic 1500 s run")
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
				lock := !(s == g && tt >= 800 && tt < 815)
				f.GPS(s.idx, GPSStatus{HostNs: synthOrigin + int64(tt*1e9), Locked: lock, UTC: s != g})
			}
		}
	}
	tick(0)
	last := 0.0
	anchorBefore, lostDuring := false, false
	w.run(f, 0, 1500, 150, 0, func(tE float64) {
		if tE-last >= 1 {
			tick(tE)
			last = tE
		}
		if tE == 790 {
			if sc := f.Snapshot().Station(g.idx); sc != nil && sc.Anchor {
				anchorBefore = true
			}
		}
		if tE == 810 {
			if sc := f.Snapshot().Station(g.idx); sc != nil && !sc.Anchor {
				lostDuring = true
			}
		}
	})
	if !anchorBefore {
		dumpEvents(t, f, g.idx)
		t.Fatal("the GPS-time station with status frames was not an anchor before its lock loss")
	}
	if !lostDuring {
		t.Fatal("the lock loss did not demote the station (test premise)")
	}
	sc := f.Snapshot().Station(g.idx)
	if sc == nil || !sc.Anchor {
		dumpEvents(t, f, g.idx)
		t.Fatal("the GPS-time station is not an anchor again after its lock returned")
	}
	if sc.Class != model.ClockGPSUTC {
		t.Errorf("class %s after recovery, want gps_utc (leap kept)", sc.Class)
	}
}
