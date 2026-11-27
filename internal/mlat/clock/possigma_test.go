package clock

import (
	"math"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// TestSuspectPositionDoesNotBiasAnchors is the regression test of finding
// MLAT-R4: a GPS station whose registered position is kilometres off (a
// declared position the survey contradicted) used to enter the filter as
// Located with no position σ and pulled the honest stations by 10-45 ns
// (9.8σ at 3 km, 45σ at 10 km). With the engine's inflated σ
// (StationInfo.PosSigmaM, as a suspect declared position gets it) its
// observations are down-weighted and the honest clocks stay within a few σ
// of the truth.
func TestSuspectPositionDoesNotBiasAnchors(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic 900 s runs")
	}
	for _, offKm := range []float64{3, 10} {
		w := newSynth(21)
		for i := 0; i < 4; i++ {
			w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*1.6), 9+1.2*math.Sin(float64(i)*1.6))
		}
		for i := 0; i < 8; i++ {
			w.addStation(model.ClockSDR12, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
		}
		w.addAircraft(60, 50, 9, 200)
		bad := w.sts[1]
		f, _ := New(Config{OriginUnixNs: synthOrigin, ProbationS: 60, Workers: 1})
		for _, s := range w.sts {
			in := StationInfo{Class: s.clk.cls, Pos: s.pos, Located: true, NoStatus: true, PosSigmaM: 30}
			if s == bad {
				fr := geo.NewFrameECEF(s.pos)
				in.Pos = fr.FromENU(geo.Vec3{X: offKm * 1000})
				// stations.go: hypot(declared σ, max(discrepancy, 100 m))
				in.PosSigmaM = math.Hypot(30, offKm*1000)
			}
			f.SetStation(s.idx, in)
		}
		w.run(f, 0, 900, 150, 0.2, nil)
		snap := f.Snapshot()
		var cm float64
		var na int
		for _, s := range w.sts[:4] {
			if s == bad {
				continue
			}
			if e, _, ok := w.errNs(f, s, snap.T); ok {
				cm += e
				na++
			}
		}
		if na == 0 {
			t.Fatalf("off %.0f km: no honest anchor synced", offKm)
		}
		cm /= float64(na)
		worst, worstAbs, worstSig := 0.0, 0.0, 0.0
		for _, s := range w.sts {
			if s == bad {
				continue
			}
			if e, sg, ok := w.errNs(f, s, snap.T); ok {
				if r := math.Abs(e-cm) / math.Max(sg, 1); r > worst {
					worst, worstAbs, worstSig = r, e-cm, sg
				}
			}
		}
		t.Logf("off %.0f km: worst honest |err|/σ = %.1f (%.1f ns, σ %.1f)", offKm, worst, worstAbs, worstSig)
		if worst > 4.5 {
			t.Errorf("off %.0f km: an honest station is %.1fσ (%.1f ns) off", offKm, worst, worstAbs)
		}
	}
}
