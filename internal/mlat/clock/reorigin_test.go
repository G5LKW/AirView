package clock

import (
	"math"
	"testing"

	"airview/internal/model"
)

// TestReoriginContinuity: moving the filter-internal time origin (Config.
// ReoriginS) changes nothing a caller can see. The same synthetic world is
// run with the origin moved every ~30 s and never moved; the published
// network times, snapshot times, events and statistics agree.
func TestReoriginContinuity(t *testing.T) {
	run := func(reorigin float64) (*Filter, *synthWorld) {
		w := newSynth(11)
		for i := 0; i < 3; i++ {
			w.addStation(model.ClockGPSUTC, 50+0.7*math.Cos(float64(i)*2.1), 9+1.0*math.Sin(float64(i)*2.1))
		}
		for i := 0; i < 7; i++ {
			cls := []model.ClockClass{model.ClockSDR12, model.ClockHW12, model.ClockSDR12HiRes}[i%3]
			w.addStation(cls, 50+(w.rng.Float64()-0.5)*1.6, 9+(w.rng.Float64()-0.5)*2.4)
		}
		w.addAircraft(40, 50, 9, 180)
		f, err := New(Config{OriginUnixNs: synthOrigin, ProbationS: 60, Workers: 1, ReoriginS: reorigin})
		if err != nil {
			t.Fatal(err)
		}
		w.register(f)
		w.run(f, 0, 240, 120, 0.3, nil)
		return f, w
	}
	fa, wa := run(30)
	fb, _ := run(1e9)
	if fa.shiftS == 0 || fb.shiftS != 0 {
		t.Fatalf("shift: moved %d s, unmoved %d s", fa.shiftS, fb.shiftS)
	}
	sa, sb := fa.Snapshot(), fb.Snapshot()
	if sa.T != sb.T {
		t.Fatalf("snapshot time %v vs %v", sa.T, sb.T)
	}
	stA, stB := fa.Stats(), fb.Stats()
	if stA.Accepted != stB.Accepted || stA.Steps != stB.Steps {
		t.Errorf("stats differ: %+v vs %+v", stA, stB)
	}
	worst := 0.0
	for _, s := range wa.sts {
		a, b := sa.Station(s.idx), sb.Station(s.idx)
		if a == nil || b == nil || a.Synced != b.Synced || a.AnchorState != b.AnchorState {
			t.Fatalf("station %d differs: %+v / %+v", s.idx, a, b)
		}
		if math.Abs(a.BaseS-b.BaseS) > 1e-6 {
			t.Errorf("station %d BaseS %v vs %v", s.idx, a.BaseS, b.BaseS)
		}
		if len(a.Events) != len(b.Events) {
			t.Errorf("station %d: %d vs %d events", s.idx, len(a.Events), len(b.Events))
		} else {
			for i := range a.Events {
				if math.Abs(a.Events[i].T-b.Events[i].T) > 1e-6 || a.Events[i].Reason != b.Events[i].Reason {
					t.Errorf("station %d event %d: %+v vs %+v", s.idx, i, a.Events[i], b.Events[i])
				}
			}
		}
		// one timestamp at the snapshot time, both forms of the conversion
		q := Query{Station: s.idx, Epoch: a.Epoch, Kind: model.FrameModeSLong}
		q.Ticks = s.clk.ticks(sa.T + s.clk.theta(sa.T) + DefaultFrameRefNs(s.clk.cls, q.Kind)*1e-9)
		ta, _, oka := sa.NetTime(q)
		tb, _, okb := sb.NetTime(q)
		na, fra, _, _, _ := sa.NetTimeUnix(q)
		nb, frb, _, _, _ := sb.NetTimeUnix(q)
		if oka != okb {
			t.Fatalf("station %d: ok %v vs %v", s.idx, oka, okb)
		}
		d := math.Abs(float64(na-nb) + fra - frb)
		worst = math.Max(worst, d)
		if math.Abs(ta-tb)*1e9 > 0.05 || d > 0.05 {
			t.Errorf("station %d: network time differs by %.4f ns (float) / %.4f ns (split)", s.idx, (ta-tb)*1e9, d)
		}
	}
	t.Logf("origin moved by %d s; worst network-time difference %.5f ns", fa.shiftS, worst)
}
