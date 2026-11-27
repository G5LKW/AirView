package solve

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// Synthetic geometry for unit tests: stations on rings around a centre,
// TOAs generated with the same refraction model the solver uses.

var testCentre = geo.LLH{Lat: 51.0, Lon: 9.0, H: 0}

// ring returns n stations evenly spread on a circle of radius rKm around
// the test centre (plus a little jitter from rng), heights 80-400 m HAE.
func ring(rng *rand.Rand, n int, rKm float64) []geo.Vec3 {
	f := geo.NewFrame(testCentre)
	out := make([]geo.Vec3, n)
	for i := range out {
		a := 2*math.Pi*float64(i)/float64(n) + 0.3*rng.Float64()
		r := rKm * 1000 * (0.7 + 0.3*rng.Float64())
		e, nn := r*math.Sin(a), r*math.Cos(a)
		p := f.FromENU(geo.Vec3{X: e, Y: nn, Z: 0})
		ll := geo.FromECEF(p)
		out[i] = geo.ToECEF(ll.Lat, ll.Lon, 80+320*rng.Float64())
	}
	return out
}

// target returns an ECEF point at (east, north) km from the centre at HAE h.
func target(eKm, nKm, h float64) geo.Vec3 {
	f := geo.NewFrame(testCentre)
	p := f.FromENU(geo.Vec3{X: eKm * 1000, Y: nKm * 1000, Z: 0})
	ll := geo.FromECEF(p)
	return geo.ToECEF(ll.Lat, ll.Lon, h)
}

// seenBy reports whether every station has a 4/3-earth line of sight to x
// (the station heights themselves are the minimum clearance).
func seenBy(st []geo.Vec3, x geo.Vec3) bool {
	for _, p := range st {
		if !geo.LineOfSight(p, x, 0) {
			return false
		}
	}
	return true
}

// visTarget draws targets with gen until every station sees one.
func visTarget(st []geo.Vec3, gen func() geo.Vec3) geo.Vec3 {
	for {
		if x := gen(); seenBy(st, x) {
			return x
		}
	}
}

// synth builds a problem: transmit at tau (s), station i hears at
// tau + Range/c + noise, noise ~ N(0, cov) when cov != nil (s², full)
// else N(0, sigma_i²).
func synth(rng *rand.Rand, st []geo.Vec3, x geo.Vec3, sigma []float64, cov []float64, refr bool) Problem {
	m := len(st)
	atm := geo.DefaultAtmosphere()
	pr := Problem{Stations: make([]Station, m)}
	noise := make([]float64, m)
	if cov != nil {
		L := make([]float64, m*m)
		copy(L, cov)
		if err := linalg.Cholesky(L, m); err != nil {
			panic(err)
		}
		g := make([]float64, m)
		for i := range g {
			g[i] = rng.NormFloat64()
		}
		for i := 0; i < m; i++ {
			for k := 0; k <= i; k++ {
				noise[i] += L[i*m+k] * g[k]
			}
		}
		pr.Cov = cov
	} else if sigma != nil {
		for i := range noise {
			noise[i] = sigma[i] * rng.NormFloat64()
		}
	}
	const tau = 0.123456
	for i, p := range st {
		var rho float64
		if refr {
			rho = atm.Range(p, x)
		} else {
			rho = p.Dist(x)
		}
		sg := 50e-9
		if sigma != nil {
			sg = sigma[i]
		}
		pr.Stations[i] = Station{Pos: p, TOA: tau + rho/c + noise[i], Sigma: sg, ID: int32(i)}
	}
	return pr
}

func fill(n int, v float64) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = v
	}
	return s
}

// enuErr returns the error of x against truth in the ENU frame at truth.
func enuErr(x, truth geo.Vec3) geo.Vec3 {
	f := geo.NewFrameECEF(truth)
	return f.Rotate(x.Sub(truth))
}

func TestChi2Quantile(t *testing.T) {
	cases := []struct {
		dof  int
		p, x float64
	}{
		{1, 1e-3, 10.828}, {2, 1e-3, 13.816}, {3, 1e-3, 16.266}, {5, 1e-3, 20.515},
		{10, 1e-3, 29.588}, {30, 1e-3, 59.703}, {1, 0.05, 3.841}, {4, 0.01, 13.277},
		{60, 1e-3, 99.607},
	}
	for _, cs := range cases {
		got := Chi2Quantile(cs.p, cs.dof)
		if math.Abs(got-cs.x) > 2e-3*cs.x {
			t.Errorf("χ²(%d) at %g: got %.4f want %.3f", cs.dof, cs.p, got, cs.x)
		}
		if s := Chi2Survival(got, cs.dof); math.Abs(s-cs.p) > 1e-6*math.Max(1, cs.p*1e3) {
			t.Errorf("survival round trip dof %d: %g", cs.dof, s)
		}
	}
	if !math.IsInf(Chi2Quantile(1e-3, 0), 1) {
		t.Error("dof 0 must give +Inf")
	}
}

func TestExactRecovery(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, refr := range []bool{false, true} {
		s := NewSolver(Options{NoRefraction: !refr})
		miss4 := 0
		defer func() {
			// 4 stations without altitude: near-tangent far-out geometries
			// may hide the true root; require it in ≥ 90 %
			if miss4 > 2 {
				t.Errorf("4 stations: truth not found in %d/20 trials", miss4)
			}
		}()
		for _, n := range []int{4, 5, 6, 8, 12} {
			for trial := 0; trial < 20; trial++ {
				st := ring(rng, n, 120)
				x := visTarget(st, func() geo.Vec3 {
					return target(rng.Float64()*400-200, rng.Float64()*400-200, 1000+11000*rng.Float64())
				})
				pr := synth(rng, st, x, nil, nil, refr)
				for i := range pr.Stations {
					pr.Stations[i].Sigma = 30e-9
				}
				var f Fix
				if err := s.Solve(&pr, &f); err != nil {
					t.Fatalf("n=%d refr=%v: %v", n, refr, err)
				}
				// 4 stations without altitude can be ambiguous: accept the
				// truth as the fix or as an alternate
				d := f.ECEF.Dist(x)
				if n == 4 && d > 0.05 {
					for k := 0; k < f.NAlt; k++ {
						d = math.Min(d, f.Alt[k].ECEF.Dist(x))
					}
				}
				if n == 4 && d > 0.05 {
					miss4++
					continue
				}
				if d > 0.05 {
					t.Errorf("n=%d refr=%v trial %d: error %.3f m (init %v, flags %b, reject %v, chi2 %.3g)",
						n, refr, trial, d, f.Init, f.Flags, f.Reject, f.Chi2)
				}
				if math.Abs(f.TxS-0.123456) > 1e-9 && n > 4 {
					t.Errorf("n=%d: transmit time off by %g s", n, f.TxS-0.123456)
				}
			}
		}
	}
}

func TestClosedFormSeeds(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	s := NewSolver(Options{NoRefraction: true})
	for trial := 0; trial < 50; trial++ {
		st := ring(rng, 6, 150)
		x := visTarget(st, func() geo.Vec3 {
			return target(rng.Float64()*600-300, rng.Float64()*600-300, 500+12000*rng.Float64())
		})
		pr := synth(rng, st, x, nil, nil, false)
		for i := range pr.Stations {
			pr.Stations[i].Sigma = 1e-9
		}
		if err := s.setup(&pr); err != nil {
			t.Fatal(err)
		}
		if err := s.buildRows(); err != nil {
			t.Fatal(err)
		}
		idx := s.activeToa()
		lx, ok := s.linearToa(idx)
		if !ok || lx.Dist(x) > 1e-3 {
			t.Errorf("linear TOA: ok=%v err %.3g m", ok, lx.Dist(x))
		}
		var xs [2]geo.Vec3
		k := s.bancroft(idx[:4], xs[:])
		best := math.Inf(1)
		for j := 0; j < k; j++ {
			best = math.Min(best, xs[j].Dist(x))
		}
		if best > 0.2 {
			t.Errorf("Bancroft (4): best root %.3g m from truth", best)
		}
		k = s.bancroft(idx, xs[:])
		best = math.Inf(1)
		for j := 0; j < k; j++ {
			best = math.Min(best, xs[j].Dist(x))
		}
		if best > 0.2 {
			t.Errorf("Bancroft (6): best root %.3g m from truth", best)
		}
		// 3 + altitude closed form
		h := geo.FromECEF(x).H
		var qs [4]geo.Vec3
		k = s.quartic(idx[:3], h, qs[:])
		best = math.Inf(1)
		for j := 0; j < k; j++ {
			best = math.Min(best, qs[j].Dist(x))
		}
		// near-tangent geometries leave the spherical-shell closed form a
		// few km off along the flat direction; LM finishes the job
		if best > 25000 {
			t.Errorf("quartic: %d roots, best %.3g m from truth", k, best)
		}
	}
}

func TestGridSeeds(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	s := NewSolver(Options{NoRefraction: true})
	hit := 0
	const trials = 30
	for trial := 0; trial < trials; trial++ {
		st := ring(rng, 3, 100)
		x := visTarget(st, func() geo.Vec3 {
			return target(rng.Float64()*300-150, rng.Float64()*300-150, 3000+8000*rng.Float64())
		})
		pr := synth(rng, st, x, nil, nil, false)
		pr.Alt = Altitude{Valid: true, HAE: geo.FromECEF(x).H, Sigma: 20}
		if err := s.setup(&pr); err != nil {
			t.Fatal(err)
		}
		if err := s.buildRows(); err != nil {
			t.Fatal(err)
		}
		var gs [3]geo.Vec3
		k := s.gridSeeds(s.activeToa(), geo.FromECEF(x).H, s.opt.GridStepM, gs[:])
		for j := 0; j < k; j++ {
			if gs[j].Dist(x) < 7500 {
				hit++
				break
			}
		}
	}
	if hit < trials*9/10 {
		t.Errorf("grid: truth within one cell diagonal in %d/%d trials", hit, trials)
	}
}
