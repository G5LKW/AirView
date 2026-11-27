package quality

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/solve"
	"airview/internal/sim/simnet"
)

// katmResult is one k_atm simnet run.
type katmResult struct {
	proj, dem KAtmReport
	// keff is the model-equivalent truth: the same projected regression
	// fed with the noise-free model mismatch at the true positions,
	// L_model − (|p − rᵢ| + simnet excess). simnet's straight-chord
	// four-node excess and geo's ray-traced excess with the geoid differ
	// by a few per cent, so the k that makes geo's model match simnet's
	// world is not exactly simnet's KAtm.
	keff  float64
	fixes int
	rmsH  float64
}

// katmRun simulates a well-timed (GPS + Beast) network whose true
// refractivity scale is kTrue, solves every 6th transmission heard by >= 6
// stations with the model k = modelK, and feeds both k_atm estimators.
func katmRun(tb testing.TB, seed uint64, kTrue, modelK float64, dur time.Duration) katmResult {
	tb.Helper()
	atm := geo.Atmosphere{N0: 315, H: 7000, K: modelK}
	sv := solve.NewSolver(solve.Options{Atmosphere: atm})
	a1 := atm
	a1.K = 1
	sa := simnet.Atmosphere{N0: 315, ScaleHeightM: 7000, KAtm: kTrue, KFactor: 4.0 / 3}
	var kp, kd, ke KAtm
	rng := rand.New(rand.NewPCG(seed, 9))
	var f solve.Fix
	var res katmResult
	var se float64
	var h *harnessQ
	var st, stT [solve.MaxStations]geo.Vec3
	var rs, sg, ex1, rt [solve.MaxStations]float64
	h = newHarnessQ(tb, hOpts{seed: seed, receivers: 40, aircraft: 250, kAtm: kTrue, minRows: 6,
		mix: map[simnet.Device]float64{simnet.DeviceRadarcapeGPS: 0.6, simnet.DeviceBeast: 0.4}}, func(t *hTx) {
		if t.seq%6 != 0 {
			return
		}
		if _, ok := solveTx(sv, t, rng, 30, &f); !ok || f.DOF < 2 || f.Flags&solve.FlagAmbiguous != 0 {
			return
		}
		e, n, _ := enuErr(f.ECEF, t.pos)
		se += e*e + n*n
		res.fixes++
		m := 0
		for i := 0; i < len(t.rows) && i < solve.MaxStations; i++ {
			if f.Used&(1<<i) == 0 {
				continue
			}
			st[m] = t.rows[i].pos
			rs[m] = float64(f.Resid[i])
			sg[m] = t.rows[i].sigmaS * geo.C
			ex1[m] = a1.Excess(st[m], f.ECEF)
			stT[m] = st[m]
			rt[m] = atm.Range(st[m], t.pos) - (t.pos.Dist(st[m]) + h.simExcess(sa, t.rows[i].rx, t.pos))
			m++
		}
		now := h.unixNs(t.txS)
		kd.AddFix(now, ex1[:m], rs[:m], sg[:m])
		alt := 0.0
		if f.AltUsed {
			alt = 30
		}
		kp.AddSolved(now, &KAtmFix{Target: f.ECEF, Stations: st[:m], ResidM: rs[:m], SigmaM: sg[:m], AltSigmaM: alt, AltResidM: f.AltResid, ModelK: modelK}, atm)
		ke.AddSolved(now, &KAtmFix{Target: t.pos, Stations: stT[:m], ResidM: rt[:m], SigmaM: sg[:m], AltSigmaM: alt, ModelK: modelK}, atm)
	})
	h.run(tb, dur)
	kp.Flush()
	kd.Flush()
	ke.Flush()
	if res.fixes > 0 {
		res.rmsH = math.Sqrt(se / float64(res.fixes))
	}
	res.proj, res.dem = kp.Report(), kd.Report()
	res.keff = ke.Report().K
	return res
}

// TestKAtmSimnet validates the k_atm regression against simnet's true
// refractivity scale. The projected estimator must recover the
// model-equivalent truth k_eff within 0.01 and simnet's KAtm within 0.05
// (simnet's straight-chord excess is ≈ 3.5 % above geo's ray trace),
// for a model that starts at 1 and for one that starts on the other side,
// and must beat the demeaned estimator whenever the model is off.
func TestKAtmSimnet(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	type tc struct{ kTrue, model float64 }
	cases := []tc{{1.0, 1.0}, {1.15, 1.0}, {0.88, 1.0}, {1.1, 1.2}}
	for _, c := range cases {
		t.Run(fmt.Sprintf("k%.2f_model%.2f", c.kTrue, c.model), func(t *testing.T) {
			t.Parallel()
			r := katmRun(t, 3, c.kTrue, c.model, 4*time.Minute)
			demK := c.model - (1 - r.dem.K) // the demeaned report assumes model 1
			t.Logf("k*=%.3f k_eff=%.4f model=%.2f fixes=%d rmsH=%.1fm | projected k=%.4f ±%.4f (rows %d) err_eff=%+.4f err=%+.4f | demeaned k=%.4f err_eff=%+.4f",
				c.kTrue, r.keff, c.model, r.fixes, r.rmsH, r.proj.K, r.proj.Sigma, r.proj.Rows, r.proj.K-r.keff, r.proj.K-c.kTrue, demK, demK-r.keff)
			if r.proj.Method != "projected" {
				t.Fatalf("no projected estimate: %+v", r.proj)
			}
			if math.Abs(r.proj.K-r.keff) > 0.01 {
				t.Errorf("projected k = %.4f, model-equivalent truth %.4f", r.proj.K, r.keff)
			}
			if math.Abs(r.proj.K-c.kTrue) > 0.05 {
				t.Errorf("projected k = %.4f, simnet truth %.3f", r.proj.K, c.kTrue)
			}
			if math.Abs(r.keff-c.model) > 0.03 && math.Abs(r.proj.K-r.keff) > math.Abs(demK-r.keff) {
				t.Errorf("projected (%.4f) not better than demeaned (%.4f) for k_eff = %.4f", r.proj.K, demK, r.keff)
			}
		})
	}
}

// TestKAtmProjectedSynthetic checks the projected regression on exact
// synthetic fixes (no solver): residuals are built as P·(b·E + n) for a
// known geometry, so the estimate must be unbiased and its σ honest.
func TestKAtmProjectedSynthetic(t *testing.T) {
	atm := geo.DefaultAtmosphere()
	a1 := atm
	sv := solve.NewSolver(solve.Options{Atmosphere: atm})
	rng := rand.New(rand.NewPCG(5, 5))
	var k KAtm
	kTrue := 1.12
	tru := atm
	tru.K = kTrue
	var f solve.Fix
	for i := 0; i < 3000; i++ {
		// 8 stations around (50, 9), a target within 250 km at 3-11 km
		tlat, tlon := 50+rng.Float64()*3-1.5, 9+rng.Float64()*4-2
		tgt := geo.ToECEF(tlat, tlon, 3000+rng.Float64()*8000)
		var pr solve.Problem
		var sts [8]solve.Station
		for j := range sts {
			p := geo.ToECEF(50+rng.Float64()*4-2, 9+rng.Float64()*6-3, 100+rng.Float64()*300)
			toa := tru.Range(p, tgt)/geo.C + rng.NormFloat64()*10e-9
			sts[j] = solve.Station{Pos: p, TOA: toa, Sigma: 10e-9}
		}
		pr.Stations = sts[:]
		h := geo.FromECEF(tgt).H
		pr.Alt = solve.Altitude{Valid: true, HAE: h + rng.NormFloat64()*20, Sigma: 20}
		if err := sv.Solve(&pr, &f); err != nil || !f.OK() {
			continue
		}
		var ps []geo.Vec3
		var rs, sg []float64
		for j := range sts {
			if f.Used&(1<<j) == 0 {
				continue
			}
			ps = append(ps, sts[j].Pos)
			rs = append(rs, float64(f.Resid[j]))
			sg = append(sg, 10e-9*geo.C)
		}
		k.AddSolved(int64(i)*1e9, &KAtmFix{Target: f.ECEF, Stations: ps, ResidM: rs, SigmaM: sg, AltSigmaM: 20, AltResidM: f.AltResid, ModelK: 1}, a1)
	}
	k.Flush()
	r := k.Report()
	t.Logf("synthetic: k* %.3f → %.4f ± %.4f (%d fixes, %d rows)", kTrue, r.K, r.Sigma, r.Fixes, r.Rows)
	if math.Abs(r.K-kTrue) > 4*r.Sigma+0.003 {
		t.Errorf("k = %.4f ± %.4f, truth %.3f", r.K, r.Sigma, kTrue)
	}
	if r.Sigma <= 0 || r.Sigma > 0.02 {
		t.Errorf("σ = %.4f", r.Sigma)
	}
	if kk, _, nh, ok := k.Estimate(1); !ok || nh != 1 || math.Abs(kk-r.K) > 1e-12 {
		t.Errorf("Estimate = %v %v %v", kk, nh, ok)
	}
}

// TestKAtmCombineAndReady checks the hourly combination and the apply gate.
func TestKAtmCombineAndReady(t *testing.T) {
	var k KAtm
	if _, _, _, ok := k.Estimate(1); ok {
		t.Fatal("estimate before data")
	}
	rng := rand.New(rand.NewPCG(1, 2))
	add := func(hour int64, kk float64) {
		for i := 0; i < 400; i++ {
			x := rng.Float64()*20 - 10
			// rows with z = k·x + noise; ModelK 1 → resid = x − z
			z := kk*x + rng.NormFloat64()*0.5
			k.mu.Lock()
			k.rollIfLocked(hour * hourNs)
			k.prow = append(k.prow, katmPRow{x: x, z: z, w: 4, fix: int32(i)})
			k.nfix++
			k.mu.Unlock()
		}
	}
	add(1, 1.10)
	add(2, 1.10)
	add(3, 1.10)
	k.Flush()
	r := k.Report()
	if r.Hours != 3 || math.Abs(r.K-1.10) > 0.01 || !r.Ready {
		t.Fatalf("report %+v", r)
	}
	// a jump: χ² per hour grows, gate closes
	add(4, 1.0)
	add(5, 1.0)
	k.Flush()
	r = k.Report()
	if r.Ready || r.Chi2PerHour < 4 {
		t.Fatalf("after jump: %+v", r)
	}
	if len(r.Recent) != 5 || r.Recent[0].K > 1.05 {
		t.Fatalf("recent %+v", r.Recent)
	}
}
