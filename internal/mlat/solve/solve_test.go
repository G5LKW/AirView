package solve

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// mcStats accumulates ENU errors and reported covariances.
type mcStats struct {
	n                      int
	se, sn, su             float64 // Σ error²
	ce, cn, cu             float64 // Σ CRLB variance
	in1                    int     // horizontal error ≤ reported σ_h (scaled)
	chi2                   float64
	errs                   []float64
	sigmaHRep, sigmaHCRLB  float64
	nAccepted, nConverged  int
	sumScaledH, sumRawVarH float64
}

func (m *mcStats) add(f *Fix, truth geo.Vec3) {
	e := enuErr(f.ECEF, truth)
	m.n++
	m.se += e.X * e.X
	m.sn += e.Y * e.Y
	m.su += e.Z * e.Z
	m.ce += f.CRLBENU[0]
	m.cn += f.CRLBENU[4]
	m.cu += f.CRLBENU[8]
	h := math.Hypot(e.X, e.Y)
	if h <= f.SigmaH {
		m.in1++
	}
	m.chi2 += f.Chi2
	m.errs = append(m.errs, h)
}

func (m *mcStats) ratio() (re, rn, ru, rh float64) {
	re = math.Sqrt(m.se / m.ce)
	rn = math.Sqrt(m.sn / m.cn)
	ru = math.Sqrt(m.su / m.cu)
	rh = math.Sqrt((m.se + m.sn) / (m.ce + m.cn))
	return
}

func percentile(v []float64, p float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s) == 0 {
		return math.NaN()
	}
	return s[int(math.Min(float64(len(s)-1), p*float64(len(s))))]
}

// TestCRLBMonteCarlo is the §13.2 gate "CRLB vs Monte Carlo (1000 runs)
// within ±20 %": the empirical error spread of 1000 noisy solves of a
// fixed geometry must match the reported Cramér-Rao bound, for diagonal
// and for fully correlated TOA covariance, with and without altitude.
func TestCRLBMonteCarlo(t *testing.T) {
	type cfg struct {
		name     string
		n        int
		sigmaNs  float64
		corr     bool
		alt      bool
		east, nn float64
		h        float64
	}
	cases := []cfg{
		{"6 GPS inside", 6, 50, false, false, 20, -30, 9000},
		{"6 GPS correlated", 6, 50, true, false, 20, -30, 9000},
		{"8 SDR + alt", 8, 400, false, true, 60, 40, 7000},
		{"5 mixed correlated + alt outside", 5, 150, true, true, 150, 80, 10000},
		{"3 + alt", 3, 50, false, true, 10, 20, 9000},
	}
	for ci, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(10+ci), 7))
			st := ring(rng, cs.n, 100)
			x := target(cs.east, cs.nn, cs.h)
			if !seenBy(st, x) {
				t.Fatal("test geometry not visible")
			}
			m := cs.n
			sig := make([]float64, m)
			for i := range sig {
				sig[i] = cs.sigmaNs * 1e-9 * (0.7 + 0.6*float64(i)/float64(m))
			}
			var cov []float64
			if cs.corr {
				// clock covariance: a common-mode part (absorbed by b),
				// pairwise correlations and per-station marginals
				cov = make([]float64, m*m)
				for i := 0; i < m; i++ {
					for j := 0; j < m; j++ {
						v := 0.3 * sig[i] * sig[j] * math.Exp(-math.Abs(float64(i-j))/2)
						v += 0.5 * sig[i] * sig[j] // common mode
						if i == j {
							v = sig[i]*sig[i] + 0.8*sig[i]*sig[i]
						}
						cov[i*m+j] = v
					}
				}
			}
			s := NewSolver(Options{})
			var st1 mcStats
			const runs = 1000
			hTrue := geo.FromECEF(x).H
			for r := 0; r < runs; r++ {
				pr := synth(rng, st, x, sig, cov, true)
				if cs.alt {
					pr.Alt = Altitude{Valid: true, HAE: hTrue + 20*rng.NormFloat64(), Sigma: 20}
				}
				var f Fix
				if err := s.Solve(&pr, &f); err != nil {
					t.Fatalf("run %d: %v", r, err)
				}
				if cs.n == 3 && f.ECEF.Dist(x) > 5000 {
					// the other 3+alt branch; branch choice is tested
					// separately
					continue
				}
				st1.add(&f, x)
			}
			re, rn, ru, rh := st1.ratio()
			cal := float64(st1.in1) / float64(st1.n)
			t.Logf("%s: n=%d  σ_emp/σ_CRLB  E %.3f  N %.3f  U %.3f  H %.3f; CRLB σ_h %.1f m; P50 %.1f P95 %.1f m; 1σ calibration %.1f %%; mean χ² %.2f",
				cs.name, st1.n, re, rn, ru, rh, math.Sqrt((st1.ce+st1.cn)/float64(st1.n)),
				percentile(st1.errs, 0.5), percentile(st1.errs, 0.95), 100*cal, st1.chi2/float64(st1.n))
			for _, r := range []float64{re, rn, ru, rh} {
				if r < 0.8 || r > 1.2 {
					t.Errorf("Monte Carlo / CRLB ratio %.3f outside ±20 %%", r)
				}
			}
			if st1.n < runs*9/10 {
				t.Errorf("only %d/%d runs on the main branch", st1.n, runs)
			}
		})
	}
}

// TestRAIMExcludesFault: a 2 µs fault on one station must be detected and
// that station excluded (acceptance of WP-SV), for GPS- and SDR-class
// timing.
func TestRAIMExcludesFault(t *testing.T) {
	for _, cs := range []struct {
		name    string
		n       int
		sigmaNs float64
		alt     bool
		minRate float64
		mdbFrac float64 // detectable: MDB < mdbFrac·2 µs
	}{
		{"8 GPS", 8, 30, false, 0.98, 0.6}, // no altitude: rare far-out geometries have a second consistent basin
		{"6 GPS + alt", 6, 30, true, 0.99, 0.6},
		{"5 GPS + alt", 5, 30, true, 0.97, 0.6},
		// SDR timing: 2 µs is only ~5σ; MDB < 2 µs means ≥ 80 % power
		{"10 SDR + alt", 10, 300, true, 0.85, 1},
	} {
		t.Run(cs.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(99, uint64(cs.n)))
			s := NewSolver(Options{})
			ok, falseExcl, rejected, nDet, okDet := 0, 0, 0, 0, 0
			var errs []float64
			const trials = 300
			for k := 0; k < trials; k++ {
				st := ring(rng, cs.n, 120)
				x := visTarget(st, func() geo.Vec3 {
					return target(rng.Float64()*200-100, rng.Float64()*200-100, 2000+9000*rng.Float64())
				})
				sig := fill(cs.n, cs.sigmaNs*1e-9)
				pr := synth(rng, st, x, sig, nil, true)
				if cs.alt {
					pr.Alt = Altitude{Valid: true, HAE: geo.FromECEF(x).H + 20*rng.NormFloat64(), Sigma: 20}
				}
				bad := rng.IntN(cs.n)
				// internal reliability of the fault-free problem: a 2 µs
				// fault is detectable when the station's MDB is below it
				var f0 Fix
				if err := s.Solve(&pr, &f0); err != nil {
					t.Fatalf("trial %d: %v", k, err)
				}
				detectable := float64(f0.MDB[bad]) < cs.mdbFrac*2e-6*c
				if detectable {
					nDet++
				}
				sign := 1.0
				if rng.IntN(2) == 0 {
					sign = -1
				}
				pr.Stations[bad].TOA += sign * 2e-6
				var f Fix
				if err := s.Solve(&pr, &f); err != nil {
					t.Fatalf("trial %d: %v", k, err)
				}
				if f.Excluded&(1<<uint(bad)) != 0 {
					ok++
					if detectable {
						okDet++
					}
				}
				if detectable && f.Excluded&(1<<uint(bad)) == 0 && testing.Verbose() {
					t.Logf("miss trial %d: bad %d MDB %.0f m, excl %b chi2 %.1f dof %d w %v reject %v flags %b init %v dist %.0f", k, bad, f0.MDB[bad], f.Excluded, f.Chi2, f.DOF, f.WTest[:cs.n], f.Reject, f.Flags, f.Init, f.ECEF.Dist(x))
				}
				if f.Excluded&^(1<<uint(bad)) != 0 {
					falseExcl++
				}
				if !f.OK() {
					rejected++
				}
				errs = append(errs, math.Hypot(enuErr(f.ECEF, x).X, enuErr(f.ECEF, x).Y))
			}
			rate := float64(okDet) / float64(nDet)
			t.Logf("%s: fault excluded in %d/%d (%d/%d detectable), other exclusions %d, rejected %d, P50 %.1f m P95 %.1f m",
				cs.name, ok, trials, okDet, nDet, falseExcl, rejected, percentile(errs, 0.5), percentile(errs, 0.95))
			if rate < cs.minRate || nDet < trials/2 {
				t.Errorf("exclusion rate %.3f < %.2f among %d detectable faults", rate, cs.minRate, nDet)
			}
		})
	}
}

// TestRANSACMultipleFaults: two large faults among 10 stations.
func TestRANSACMultipleFaults(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 55))
	s := NewSolver(Options{})
	good := 0
	const trials = 100
	for k := 0; k < trials; k++ {
		st := ring(rng, 10, 120)
		x := visTarget(st, func() geo.Vec3 {
			return target(rng.Float64()*160-80, rng.Float64()*160-80, 3000+8000*rng.Float64())
		})
		pr := synth(rng, st, x, fill(10, 50e-9), nil, true)
		pr.Alt = Altitude{Valid: true, HAE: geo.FromECEF(x).H, Sigma: 25}
		b1 := rng.IntN(10)
		b2 := (b1 + 1 + rng.IntN(9)) % 10
		pr.Stations[b1].TOA += 15e-6
		pr.Stations[b2].TOA -= 8e-6
		var f Fix
		if err := s.Solve(&pr, &f); err != nil {
			t.Fatal(err)
		}
		if f.Excluded == (1<<uint(b1))|(1<<uint(b2)) && f.ECEF.Dist(x) < 200 {
			good++
		}
	}
	t.Logf("two faults: correct exclusion and < 200 m in %d/%d", good, trials)
	if good < trials*95/100 {
		t.Errorf("two-fault recovery %d/%d", good, trials)
	}
}

// TestThreePlusAltBranches: with 3 stations and altitude the true position
// must be among the hypotheses (§13.2 "3+alt ambiguity" at solver level),
// and a track prior must select it.
func TestThreePlusAltBranches(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 41))
	s := NewSolver(Options{})
	found, chosenPrior, ambiguous, solved := 0, 0, 0, 0
	const trials = 300
	for k := 0; k < trials; k++ {
		st := ring(rng, 3, 100)
		x := visTarget(st, func() geo.Vec3 {
			return target(rng.Float64()*300-150, rng.Float64()*300-150, 3000+9000*rng.Float64())
		})
		pr := synth(rng, st, x, fill(3, 50e-9), nil, true)
		pr.Alt = Altitude{Valid: true, HAE: geo.FromECEF(x).H + 20*rng.NormFloat64(), Sigma: 20}
		var f Fix
		if err := s.Solve(&pr, &f); err != nil {
			continue
		}
		solved++
		tol := math.Max(500, 5*f.SigmaH)
		hit := f.ECEF.Dist(x) < tol
		for j := 0; j < f.NAlt; j++ {
			hit = hit || f.Alt[j].ECEF.Dist(x) < tol
		}
		if hit {
			found++
		}
		if f.Flags&FlagAmbiguous != 0 {
			ambiguous++
		}
		// with a track prior (1 km σ around the truth)
		pr.Prior = Prior{Valid: true, Pos: x.Add(geo.Vec3{X: 700 * rng.NormFloat64(), Y: 700 * rng.NormFloat64(), Z: 700 * rng.NormFloat64()}),
			Cov: [9]float64{1e6, 0, 0, 0, 1e6, 0, 0, 0, 1e6}}
		if err := s.Solve(&pr, &f); err == nil && f.ECEF.Dist(x) < tol {
			chosenPrior++
		}
	}
	t.Logf("3+alt: solved %d/%d, truth among hypotheses %d, ambiguous %d, prior picks truth %d",
		solved, trials, found, ambiguous, chosenPrior)
	if float64(found) < 0.95*float64(solved) || solved < trials*95/100 {
		t.Errorf("truth among hypotheses only %d/%d (solved %d)", found, trials, solved)
	}
	if float64(chosenPrior) < 0.95*trials {
		t.Errorf("prior selects the true branch only %d/%d", chosenPrior, trials)
	}
}

// TestTwoStationWithTrack: 2 stations + altitude + a track prior give a
// posterior better than the prior.
func TestTwoStationWithTrack(t *testing.T) {
	rng := rand.New(rand.NewPCG(2, 22))
	s := NewSolver(Options{})
	var sumPost, sumPrior float64
	const trials = 300
	for k := 0; k < trials; k++ {
		st := ring(rng, 2, 80)
		x := visTarget(st, func() geo.Vec3 {
			return target(rng.Float64()*100-50, rng.Float64()*100-50, 4000+6000*rng.Float64())
		})
		pr := synth(rng, st, x, fill(2, 50e-9), nil, true)
		pr.Alt = Altitude{Valid: true, HAE: geo.FromECEF(x).H + 20*rng.NormFloat64(), Sigma: 20}
		const ps = 300.0
		pp := x.Add(geo.Vec3{X: ps * rng.NormFloat64(), Y: ps * rng.NormFloat64(), Z: ps * rng.NormFloat64()})
		pr.Prior = Prior{Valid: true, Pos: pp, Cov: [9]float64{ps * ps, 0, 0, 0, ps * ps, 0, 0, 0, ps * ps}}
		var f Fix
		if err := s.Solve(&pr, &f); err != nil {
			t.Fatal(err)
		}
		if f.Flags&FlagPriorFused == 0 || f.DOF != 2 || f.NStations != 2 {
			t.Fatalf("flags %b dof %d n %d", f.Flags, f.DOF, f.NStations)
		}
		ep, eq := enuErr(f.ECEF, x), enuErr(pp, x)
		sumPost += ep.X*ep.X + ep.Y*ep.Y
		sumPrior += eq.X*eq.X + eq.Y*eq.Y
	}
	post, prior := math.Sqrt(sumPost/trials), math.Sqrt(sumPrior/trials)
	t.Logf("2 stations + alt + prior: horizontal RMS prior %.0f m -> posterior %.0f m", prior, post)
	if post > 0.85*prior {
		t.Errorf("2-station update did not improve the prior: %.0f vs %.0f", post, prior)
	}
	// without a prior 2 stations are refused
	pr := synth(rng, ring(rng, 2, 80), target(0, 0, 8000), fill(2, 50e-9), nil, true)
	pr.Alt = Altitude{Valid: true, HAE: 8000, Sigma: 20}
	var f Fix
	if err := s.Solve(&pr, &f); err != ErrTooFew {
		t.Errorf("2 stations without prior: %v", err)
	}
}

// TestFullCovarianceCommonMode: a clock error common to all stations is
// absorbed by the transmit time when Σ carries it; position unaffected.
func TestFullCovarianceCommonMode(t *testing.T) {
	rng := rand.New(rand.NewPCG(8, 8))
	st := ring(rng, 6, 100)
	x := target(10, 10, 9000)
	m := 6
	cov := make([]float64, m*m)
	for i := 0; i < m; i++ {
		for j := 0; j < m; j++ {
			cov[i*m+j] = (1e-6) * (1e-6) // 1 µs common mode
		}
		cov[i*m+i] += 30e-9 * 30e-9
	}
	s := NewSolver(Options{})
	var sum float64
	falseAlarms := 0
	for r := 0; r < 200; r++ {
		pr := synth(rng, st, x, nil, cov, true)
		var f Fix
		if err := s.Solve(&pr, &f); err != nil {
			t.Fatal(err)
		}
		if f.Exclusions != 0 {
			falseAlarms++ // RAIM false alarms at PFA 1e-3 are allowed, not more
		}

		e := enuErr(f.ECEF, x)
		sum += e.X*e.X + e.Y*e.Y
	}
	if falseAlarms > 2 {
		t.Errorf("common-mode clock error caused %d exclusions in 200 runs", falseAlarms)
	}
	if rms := math.Sqrt(sum / 200); rms > 30 {
		t.Errorf("common-mode clock error leaks into the position: RMS %.1f m", rms)
	}
}

func TestRejections(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 4))
	st := ring(rng, 6, 100)
	x := target(20, 20, 9000)
	s := NewSolver(Options{})
	base := func() Problem { return synth(rng, st, x, fill(6, 50e-9), nil, true) }
	var f Fix

	pr := base()
	if err := s.Solve(&pr, &f); err != nil || !f.OK() || f.Grade != model.GradeA {
		t.Fatalf("baseline: %v reject %v grade %d σh %.1f", err, f.Reject, f.Grade, f.SigmaH)
	}
	// altitude disagreement: needs a geometry with vertical observability
	// (a compact network under the aircraft; far out, TOA cannot see a
	// 1500 m height error at all)
	stc := ring(rng, 6, 20)
	xc := target(2, 3, 9000)
	pr = synth(rng, stc, xc, fill(6, 30e-9), nil, true)
	pr.Alt = Altitude{Valid: true, HAE: geo.FromECEF(xc).H + 1500, Sigma: 20}
	if err := s.Solve(&pr, &f); err != nil || (f.Reject&RejectAltitude == 0 && !f.AltExcluded) {
		t.Errorf("altitude 1500 m off neither excluded nor rejected (err %v reject %v σv %.0f)", err, f.Reject, f.SigmaV)
	} else if f.ECEF.Dist(xc) > 100 {
		t.Errorf("altitude fault pulled the fix %.0f m", f.ECEF.Dist(xc))
	}
	// implied speed
	pr = base()
	pr.Last = Motion{Valid: true, Pos: x.Add(geo.Vec3{X: 5000}), SigmaM: 50, DtS: 2}
	if s.Solve(&pr, &f); f.Reject&RejectSpeed == 0 {
		t.Errorf("5 km in 2 s not rejected")
	}
	pr.Last.Mach = 20 // absurd Mach raises the limit
	if s.Solve(&pr, &f); f.Reject&RejectSpeed != 0 {
		t.Errorf("Mach-aware limit not applied")
	}
	// host time
	pr = base()
	pr.HostValid = true
	pr.HostFirstS = 0.123456 + 0.2
	if s.Solve(&pr, &f); f.Reject&RejectHostTime == 0 {
		t.Errorf("200 ms host-time mismatch not rejected")
	}
	pr.HostFirstS = 0.123456 + 0.0005 + 0.01
	if s.Solve(&pr, &f); f.Reject&RejectHostTime != 0 {
		t.Errorf("10 ms host latency rejected")
	}
	// σ: very far, low target heard by a small cluster with SDR timing
	st2 := ring(rng, 4, 15)
	far := target(180, 0, 9000)
	pr = synth(rng, st2, far, fill(4, 500e-9), nil, true)
	if err := s.Solve(&pr, &f); err == nil && f.Reject&RejectSigma == 0 {
		t.Errorf("far target with σh %.0f m not rejected", f.SigmaH)
	}
	// bad input
	pr = base()
	pr.Stations[2].TOA = math.NaN()
	if err := s.Solve(&pr, &f); err != ErrBadInput {
		t.Errorf("NaN TOA: %v", err)
	}
	pr = base()
	pr.Stations = pr.Stations[:3]
	if err := s.Solve(&pr, &f); err != ErrTooFew {
		t.Errorf("3 stations without altitude: %v", err)
	}
}

func TestAltitudeFromBaro(t *testing.T) {
	// fallback: geoid (embedded EGM96) + ISA; Central Europe N ≈ 47 m
	a := AltitudeFromBaro(AltitudeInput{BaroFt: 30000}, 51, 9, nil)
	if !a.Valid || math.Abs(a.HAE-(30000*0.3048+47)) > 6 || math.Abs(a.Sigma-80) > 1 {
		t.Errorf("fallback: %+v", a)
	}
	f := fieldFunc(func(lat, lon, ft float64) (float64, float64, bool) { return 160, 12, true })
	a = AltitudeFromBaro(AltitudeInput{BaroFt: 30000, QuantFt: 25}, 51, 9, f)
	want := math.Sqrt(15*15 + 7.62*7.62/12)
	if math.Abs(a.HAE-(9144+160)) > 1e-9 || math.Abs(a.Sigma-want) > 1e-9 {
		t.Errorf("field: %+v want σ %.3f", a, want)
	}
	a = AltitudeFromBaro(AltitudeInput{BaroFt: 30000, QuantFt: 100, AgeS: 2, VRateMps: 10}, 51, 9, f)
	want = math.Sqrt(15*15 + 30.48*30.48/12 + 400)
	if math.Abs(a.Sigma-want) > 1e-9 {
		t.Errorf("age/vrate/quant: σ %.3f want %.3f", a.Sigma, want)
	}
	a = AltitudeFromHAE(9300, 40, AltitudeInput{QuantFt: 100, AgeS: 1, VRateMps: 10})
	if want := math.Sqrt(1600 + 30.48*30.48/12 + 100); !a.Valid || a.HAE != 9300 || math.Abs(a.Sigma-want) > 1e-9 {
		t.Errorf("from HAE: %+v", a)
	}
	if a := AltitudeFromBaro(AltitudeInput{BaroFt: math.NaN()}, 51, 9, nil); a.Valid {
		t.Error("NaN altitude valid")
	}
}

type fieldFunc func(lat, lon, ft float64) (float64, float64, bool)

func (f fieldFunc) GNSSMinusBaro(lat, lon, ft float64) (float64, float64, bool) {
	return f(lat, lon, ft)
}

func TestCovFromClock(t *testing.T) {
	m := 4
	dst := make([]float64, m*m)
	jit := fill(m, 100e-9)
	ok := CovFromClock(dst, m, jit, func(i, j int) (float64, bool) {
		if i == j {
			return 50e-9 * 50e-9, true
		}
		return 40e-9 * 40e-9, i+j != 3 // one unknown pair
	})
	if !ok || math.Abs(dst[0]-(50e-9*50e-9+100e-9*100e-9)) > 1e-24 || dst[0*m+1] != 40e-9*40e-9 || dst[0*m+3] != 0 || dst[1*m+2] != 0 {
		t.Errorf("assembly wrong: %v", dst)
	}
	// indefinite cross-covariances are shrunk
	ok = CovFromClock(dst, m, fill(m, 1e-9), func(i, j int) (float64, bool) {
		if i == j {
			return 1e-16, true
		}
		if (i == 1 && j == 0) || (i == 2 && j == 1) {
			return 1e-16, true
		}
		return -1e-16, true
	})
	if ok {
		t.Error("indefinite input not reported")
	}
}

func TestMLATFixAndPredict(t *testing.T) {
	rng := rand.New(rand.NewPCG(6, 6))
	st := ring(rng, 7, 100)
	x := target(-30, 50, 10000)
	sig := fill(7, 50e-9)
	pr := synth(rng, st, x, sig, nil, true)
	pr.EpochNs = 1_700_000_000_000_000_000
	s := NewSolver(Options{})
	var f Fix
	if err := s.Solve(&pr, &f); err != nil {
		t.Fatal(err)
	}
	mf := f.MLATFix(0x3C1234, pr.EpochNs)
	if mf.NBucket != model.NBucket5to8 || mf.Grade != f.Grade || mf.ECEF[0] != f.ECEF.X {
		t.Errorf("MLATFix %+v", mf)
	}
	if d := mf.TUnixNs - (pr.EpochNs + 123456000); d < -1000 || d > 1000 {
		t.Errorf("transmit time %d ns off", d)
	}
	atm := geo.DefaultAtmosphere()
	enu, dop, ok := PredictCRLB(st, sig, f.ECEF, 0, &atm)
	if !ok {
		t.Fatal("predict failed")
	}
	for i := range enu {
		// the solver's Jacobian uses the excess gradient linearised a few
		// metres away: agreement to 1e-4 relative
		if math.Abs(enu[i]-f.CRLBENU[i]) > 1e-4*math.Max(1, math.Abs(f.CRLBENU[i])) {
			t.Errorf("PredictCRLB[%d] %.6g vs solver %.6g", i, enu[i], f.CRLBENU[i])
		}
	}
	if math.Abs(dop.H-f.DOP.H) > 1e-6 || !(dop.H > 0) {
		t.Errorf("DOP %+v vs %+v", dop, f.DOP)
	}
}

func TestNoAllocs(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	s := NewSolver(Options{})
	var f Fix
	probs := map[string]Problem{}
	st := ring(rng, 6, 100)
	x := target(20, 20, 9000)
	probs["6"] = synth(rng, st, x, fill(6, 50e-9), nil, true)
	p := synth(rng, st, x, fill(6, 50e-9), nil, true)
	p.Alt = Altitude{Valid: true, HAE: 9000, Sigma: 20}
	p.Stations[3].TOA += 2e-6
	probs["6+alt+fault"] = p
	st3 := ring(rng, 3, 100)
	p = synth(rng, st3, x, fill(3, 50e-9), nil, true)
	p.Alt = Altitude{Valid: true, HAE: 9000, Sigma: 20}
	probs["3+alt"] = p
	for name, pr := range probs {
		pr := pr
		if n := testing.AllocsPerRun(50, func() { _ = s.Solve(&pr, &f) }); n != 0 {
			t.Errorf("%s: %.1f allocs per solve", name, n)
		}
	}
}
