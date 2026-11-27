package mlat

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"airview/internal/mlat/quality"
)

// TestEstimateFromCross: hold-out samples whose reference lags by an
// airframe latency of 0-300 ms (along-track offsets up to 75 m) on top of
// an anisotropic MLAT error (σ 40 m / 15 m, axes fixed to the ground, i.e.
// to the station geometry): the full error against the reference
// overstates the MLAT error, the cross-track estimate recovers its
// percentiles within 15 %.
func TestEstimateFromCross(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 3))
	var es quality.ErrorStats
	var truth []float64
	t0 := int64(1_780_000_000e9)
	for i := 0; i < 20000; i++ {
		hdg := rng.Float64() * 2 * math.Pi
		v := 120 + rng.Float64()*130
		lat := rng.Float64() * 0.3
		ex, ny := 40*rng.NormFloat64(), 15*rng.NormFloat64() // MLAT error, ground-fixed axes
		truth = append(truth, math.Hypot(ex, ny))
		ve, vn := v*math.Sin(hdg), v*math.Cos(hdg)
		// the reference lags: MLAT − reference = MLAT error + v·latency
		s := quality.Sample{TNs: t0 + int64(i)*1e6, EastM: ex + ve*lat, NorthM: ny + vn*lat, VelE: ve, VelN: vn,
			CovEE: 1600, CovNN: 225}
		es.Add(&s)
	}
	sum := es.Total(t0 + 20e9)
	est := EstimateFromCross(sum)
	slices.Sort(truth)
	tp50, tp95 := truth[len(truth)/2], truth[len(truth)*95/100]
	t.Logf("MLAT error P50 %.1f P95 %.1f; against the lagging reference P50 %.1f P95 %.1f; cross-track estimate P50 %.1f P95 %.1f",
		tp50, tp95, sum.P50, sum.P95, est.P50, est.P95)
	if math.Abs(est.P50/tp50-1) > 0.15 || math.Abs(est.P95/tp95-1) > 0.15 {
		t.Fatalf("estimate P50 %.1f / P95 %.1f vs truth %.1f / %.1f", est.P50, est.P95, tp50, tp95)
	}
	if sum.P50 < 1.2*tp50 {
		t.Fatalf("the full reference error should overstate (P50 %.1f vs %.1f)", sum.P50, tp50)
	}
	if (EstimateFromCross(quality.Summary{CrossP50: math.NaN()}) != ErrorEstimate{}) {
		t.Fatal("no cross-track data must give a zero estimate")
	}
}
