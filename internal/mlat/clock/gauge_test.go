package clock

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/model"
)

// TestGaugeChangeReleasesCommonMode checks component.projectGauge against
// the dense definition P' = A·P·Aᵀ + σ²·u·uᵀ, A = I − u·wᵀ: every
// difference of the projected states keeps its covariance, the gauge
// combination wᵀx gets exactly σ², and the mean is unchanged.
func TestGaugeChangeReleasesCommonMode(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for trial := 0; trial < 5; trial++ {
		c, _ := testComponent(r)
		n := c.n
		u := c.commonIndices(kTheta, nil)
		idx := u[:2]
		w := []float64{0.5, 0.5}
		const sigma = 7.0
		P0 := append([]float64(nil), c.P...)
		x0 := append([]float64(nil), c.x...)
		c.projectGauge(u, idx, w, sigma)
		// dense reference
		A := make([]float64, n*n)
		for i := 0; i < n; i++ {
			A[i*n+i] = 1
		}
		for _, i := range u {
			for k, j := range idx {
				A[i*n+j] -= w[k]
			}
		}
		want := make([]float64, n*n)
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				v := 0.0
				for a := 0; a < n; a++ {
					for b := 0; b < n; b++ {
						v += A[i*n+a] * P0[a*n+b] * A[j*n+b]
					}
				}
				want[i*n+j] = v
			}
		}
		for _, i := range u {
			for _, j := range u {
				want[i*n+j] += sigma * sigma
			}
		}
		if e := maxRelDiff(c.P, want); e > 1e-10 {
			t.Fatalf("trial %d: projected P differs from A·P·Aᵀ + σ²uuᵀ by %.2e", trial, e)
		}
		for i := range x0 {
			if c.x[i] != x0[i] {
				t.Fatalf("trial %d: mean changed", trial)
			}
		}
		// differences keep their covariance
		dv := func(P []float64, i, j int) float64 { return P[i*n+i] + P[j*n+j] - 2*P[i*n+j] }
		for _, i := range u {
			for _, j := range u {
				if i < j && math.Abs(dv(c.P, i, j)-dv(P0, i, j)) > 1e-8*math.Max(1, dv(P0, i, j)) {
					t.Fatalf("trial %d: var(x%d − x%d) %.6g → %.6g", trial, i, j, dv(P0, i, j), dv(c.P, i, j))
				}
			}
		}
		// the gauge combination has variance σ²
		v := 0.0
		for a, i := range idx {
			for b, j := range idx {
				v += w[a] * w[b] * c.P[i*n+j]
			}
		}
		if math.Abs(v-sigma*sigma) > 1e-8*sigma*sigma {
			t.Fatalf("trial %d: var(wᵀx) = %.6g, want %.6g", trial, v, sigma*sigma)
		}
	}
}

// TestGaugeSetChangeDoesNotPin reproduces the SDR-only failure of HOOKS
// WP-M-engine -> internal/mlat/clock: in a relative component the gauge
// pseudo-observation accumulates information on the mean of the gauge
// clocks; when a station leaves the gauge set (it stops being "synced" for
// an epoch) and comes back, that information pinned its θ and φ to their
// current estimates. With the projection the station's marginal σ stays
// where the data put it; without it (NoGaugeProjection) it collapses.
func TestGaugeSetChangeDoesNotPin(t *testing.T) {
	run := func(noProj bool) (before, after [2]float64) {
		w := newSynth(21)
		for i := 0; i < 8; i++ {
			w.addStation(model.ClockSDR12, 50+0.6*math.Cos(float64(i)*0.8), 9+0.9*math.Sin(float64(i)*0.8))
		}
		w.addAircraft(30, 50, 9, 150)
		f, err := New(Config{OriginUnixNs: synthOrigin, Workers: 1, NoGaugeProjection: noProj})
		if err != nil {
			t.Fatal(err)
		}
		w.register(f)
		w.run(f, 0, 300, 80, 0, nil)
		if len(f.comps) != 1 || f.comps[0].gauge != gaugeRelative {
			t.Fatalf("expected one relative component, got %d", len(f.comps))
		}
		c := f.comps[0]
		victim := w.sts[3]
		s := f.station(victim.idx, false)
		it, ip := s.off+int(s.layout[kTheta]), s.off+int(s.layout[kPhi])
		sig := func() [2]float64 {
			return [2]float64{math.Sqrt(c.P[it*c.n+it]), math.Sqrt(c.P[ip*c.n+ip])}
		}
		before = sig()
		full := append([]model.StationIdx(nil), c.gSet...)
		var without []model.StationIdx
		for _, x := range full {
			if x != victim.idx {
				without = append(without, x)
			}
		}
		// one epoch with the victim out of the gauge set, one with it back
		for _, set := range [][]model.StationIdx{without, full} {
			f.regaugeTo(c, gaugeRelative, set)
			c.ensure()
			clear(c.J)
			clear(c.g)
			f.addGauges(c)
			if !c.update() {
				t.Fatal("update failed")
			}
			c.symmetrize()
		}
		return before, sig()
	}
	b, a := run(false)
	t.Logf("with projection: σθ %.1f → %.1f ns, σφ %.2f → %.2f ppb", b[0], a[0], b[1], a[1])
	if a[0] < 0.8*b[0] || a[1] < 0.8*b[1] {
		t.Errorf("gauge change pinned the station: σθ %.1f → %.1f ns, σφ %.2f → %.2f ppb", b[0], a[0], b[1], a[1])
	}
	b, a = run(true)
	t.Logf("without projection (old behaviour): σθ %.1f → %.1f ns, σφ %.2f → %.2f ppb", b[0], a[0], b[1], a[1])
	if a[0] > 0.8*b[0] {
		t.Errorf("the reproduction no longer shows the pinning of the old behaviour (σθ %.1f → %.1f ns)", b[0], a[0])
	}
}

// TestStepEstimateSlope: a residual run that grows linearly is reported as
// a frequency error with its slope; a constant run as an offset.
func TestStepEstimateSlope(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 9))
	s := newStation(2)
	for i := 0; i < 8; i++ {
		at := 100 + 0.3*float64(i)
		s.streak(10, 500+2000*(at-100)+r.NormFloat64()*100, at)
	}
	off, slope, se := s.stepEstimate(102.5)
	if math.Abs(slope-2000) > 4*se || se > 200 || math.Abs(off-(500+2000*2.5)) > 300 {
		t.Fatalf("frequency run: offset %.0f slope %.0f ± %.0f", off, slope, se)
	}
	s = newStation(3)
	for i := 0; i < 6; i++ {
		s.streak(-10, -3000+r.NormFloat64()*100, 50+0.4*float64(i))
	}
	off, slope, se = s.stepEstimate(52.5)
	if math.Abs(slope) > 4*se || math.Abs(off+3000) > 300 {
		t.Fatalf("offset run: offset %.0f slope %.0f ± %.0f", off, slope, se)
	}
	// too few or too close members: the mean, slope undetermined
	s = newStation(4)
	for i := 0; i < 5; i++ {
		s.streak(10, 1000, 7+0.01*float64(i))
	}
	if off, _, se := s.stepEstimate(7.5); !math.IsInf(se, 1) || off != 1000 {
		t.Fatalf("short run: offset %.0f se %v", off, se)
	}
}
