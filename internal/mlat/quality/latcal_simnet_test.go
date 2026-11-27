package quality

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/solve"
)

// latcalResult is one simnet run of the airframe-latency calibration.
type latcalResult struct {
	first, final ABReport
	enabledAtS   float64 // first "enable" decision, s (NaN = never)
	// learned − true latency of calibrated airframes (s)
	latErr []float64
	// along-track reference error against the TRUTH (m) of the hold-out
	// samples, per arm (0 control, 1 treated)
	truthAlong [2][]float64
	// non-hold-out samples of calibrated airframes after the run: the
	// reference's along-track error against the truth with the prior and
	// with the calibrated latency (counterfactual, m)
	netPrior, netCal []float64
	fixes            int
}

// latcalRun simulates a mixed network (simnet default devices) whose ADS-B
// airframes report positions latMaxMs old at most (simnet: uniform per
// airframe; < 0 = none). Every airborne position squitter heard by >= 5
// stations is solved as a snapshot MLAT fix (independent of the ADS-B
// position) and compared along track with the reference the engine would
// build: the reported position moved forward by the applied latency. The
// hold-out (25 %, NACp >= 8, rotated every 3 min) scores the A/B arms; the
// others teach. Decide runs once a simulated minute.
func latcalRun(tb testing.TB, seed uint64, latMaxMs float64, dur time.Duration, p LatencyCalParams) latcalResult {
	tb.Helper()
	atm := geo.DefaultAtmosphere()
	sv := solve.NewSolver(solve.Options{Atmosphere: atm})
	rng := rand.New(rand.NewPCG(seed, 51))
	ho := NewHoldOut(0.25, 3*time.Minute, seed, 8)
	cal := NewLatencyCal(p)
	res := latcalResult{enabledAtS: math.NaN()}
	type afTruth struct {
		lat  float64
		nacp uint8
	}
	truth := map[uint32]afTruth{}
	var f solve.Fix
	var h *harnessQ
	nextDecide := 60.0
	var lastNow int64
	h = newHarnessQ(tb, hOpts{seed: seed, receivers: 40, radiusKm: 250, aircraft: 400, posLatMs: latMaxMs, minRows: 5}, func(tx *hTx) {
		if !tx.pos17 {
			return
		}
		now := h.unixNs(tx.txS)
		lastNow = now
		if tx.txS >= nextDecide {
			r := cal.Decide(now)
			if r.Decision == "enable" && math.IsNaN(res.enabledAtS) {
				res.enabledAtS = tx.txS
				res.first = r
			}
			nextDecide += 60
		}
		ho.NoteNACp(tx.icao, tx.nacp, now)
		truth[tx.icao] = afTruth{lat: tx.latS, nacp: tx.nacp}
		if _, ok := solveTx(sv, tx, rng, 30, &f); !ok || !MLATComparable(&f) {
			return
		}
		res.fixes++
		// local frame at the reported position; horizontal velocity
		rep := geo.ToECEF(tx.repLa, tx.repLo, tx.hae)
		fr := geo.NewFrameECEF(rep)
		ve, vn := velENU(tx.vel, rep)
		speed := math.Hypot(ve, vn)
		if speed < 1 {
			return
		}
		ue, un := ve/speed, vn/speed
		mE := fr.ToENU(f.ECEF)
		tE := fr.ToENU(tx.pos)
		alongMLAT := mE.X*ue + mE.Y*un // MLAT − reported, along track
		alongTrue := tE.X*ue + tE.Y*un // truth − reported
		cm := fr.CovToENU(f.CovECEF)
		sigMLAT := math.Sqrt(math.Max(ue*ue*cm[0]+2*ue*un*cm[1]+un*un*cm[4], 0))
		nacpSig := 30 / 2.4477
		if tx.nacp > 0 && tx.nacp < 12 {
			nacpSig = nacpEPU[tx.nacp] / 2.4477
		}
		sigAlong := math.Hypot(sigMLAT, nacpSig)
		in := ho.In(tx.icao, now)
		latS, _, _ := cal.ForReference(tx.icao, in)
		// reference = reported + v·latS: along errors subtract speed·latS
		if in {
			smp := LatencySample{Key: tx.icao, NowNs: now, AlongM: alongMLAT - speed*latS, SigmaAlongM: sigAlong, SpeedMps: speed, AppliedS: latS, NACp: tx.nacp}
			if cal.Score(smp) {
				arm := 0
				if cal.Treated(tx.icao) {
					arm = 1
				}
				res.truthAlong[arm] = append(res.truthAlong[arm], speed*latS-alongTrue)
			}
			return
		}
		cal.Learn(LatencySample{Key: tx.icao, NowNs: now, AlongM: alongMLAT - speed*latS, SigmaAlongM: sigAlong, SpeedMps: speed, AppliedS: latS, NACp: tx.nacp})
		if l, _, _, ok := cal.Latency(tx.icao); ok && tx.txS > dur.Seconds()-120 {
			res.netPrior = append(res.netPrior, speed*p.PriorS-alongTrue)
			res.netCal = append(res.netCal, speed*l-alongTrue)
		}
	})
	h.run(tb, dur)
	res.final = cal.Decide(lastNow)
	for k, tr := range truth {
		if l, _, _, ok := cal.Latency(k); ok {
			res.latErr = append(res.latErr, l-tr.lat)
		}
	}
	return res
}

func rmsOf(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s / float64(max(len(v), 1)))
}

func absQ(v []float64, q float64) float64 {
	a := make([]float64, len(v))
	for i, x := range v {
		a[i] = math.Abs(x)
	}
	slices.Sort(a)
	if len(a) == 0 {
		return math.NaN()
	}
	return a[min(len(a)-1, int(q*float64(len(a))))]
}

func (r *latcalResult) log(tb testing.TB, name string) {
	tb.Helper()
	tb.Logf("%s: fixes %d; enabled at %.0f s; final %+v", name, r.fixes, r.enabledAtS, r.final)
	tb.Logf("%s: calibrated airframes %d, |learned − true latency| P50 %.1f ms P95 %.1f ms", name, len(r.latErr), 1e3*absQ(r.latErr, 0.5), 1e3*absQ(r.latErr, 0.95))
	tb.Logf("%s: hold-out reference along-track error vs truth: control RMS %.1f m (n %d), treated RMS %.1f m (n %d)",
		name, rmsOf(r.truthAlong[0]), len(r.truthAlong[0]), rmsOf(r.truthAlong[1]), len(r.truthAlong[1]))
	tb.Logf("%s: network references (last 2 min, calibrated airframes) along-track vs truth: prior RMS %.1f m, calibrated RMS %.1f m (n %d)",
		name, rmsOf(r.netPrior), rmsOf(r.netCal), len(r.netCal))
}

// TestLatencyCalSimnet validates the airframe-latency calibration and its
// hold-out A/B gate on simnet.
//
// With simnet's airframe latencies (uniform 0-300 ms per airframe) the A/B
// test must enable the calibration within the run, the learned latencies
// must match the truth (P50 error <= 30 ms), and the truth must confirm
// what the MLAT-scored A/B saw: the treated arm's references are closer
// to the true positions than the control arm's (RMS along-track), and the
// calibrated network references beat the prior by >= 30 %.
//
// Negative control: without airframe latency there is nothing to gain;
// the calibration must stay disabled.
func TestLatencyCalSimnet(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	t.Run("latency0-300ms", func(t *testing.T) {
		t.Parallel()
		r := latcalRun(t, 41, 0, 15*time.Minute, LatencyCalParams{Salt: 7})
		r.log(t, "latency 0-300 ms")
		if math.IsNaN(r.enabledAtS) || !r.final.Enabled {
			t.Errorf("calibration not enabled: %+v", r.final)
		}
		if len(r.latErr) < 50 {
			t.Fatalf("only %d calibrated airframes", len(r.latErr))
		}
		if e := absQ(r.latErr, 0.5); e > 0.05 {
			t.Errorf("median |learned − true| %.1f ms", 1e3*e)
		}
		if c, tr := rmsOf(r.truthAlong[0]), rmsOf(r.truthAlong[1]); !(tr < 0.7*c) {
			t.Errorf("truth: treated RMS %.1f m not below 70 %% of control %.1f m", tr, c)
		}
		if pr, ca := rmsOf(r.netPrior), rmsOf(r.netCal); !(ca < 0.7*pr) {
			t.Errorf("network references: calibrated RMS %.1f m vs prior %.1f m", ca, pr)
		}
	})
	t.Run("no-latency", func(t *testing.T) {
		t.Parallel()
		r := latcalRun(t, 43, -1, 15*time.Minute, LatencyCalParams{Salt: 7})
		r.log(t, "no latency")
		if !math.IsNaN(r.enabledAtS) || r.final.Enabled {
			t.Errorf("calibration enabled without latency: %+v", r.final)
		}
		if r.final.Decision == "insufficient" {
			t.Errorf("A/B never had enough data: %+v", r.final)
		}
	})
}
