package track

import (
	"math"
	"math/rand/v2"
	"sort"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// This file holds test-only helpers: a small snapshot solver standing in
// for mlat/solve (fixes for initiation), synthetic trajectories and TOA
// generation, and error statistics.

// solution is one snapshot solve result.
type solution struct {
	p    geo.Vec3
	cov  [9]float64
	chi2 float64
}

// solveTOA solves for (p, transmit time) with Gauss-Newton/LM from several
// seeds and returns the distinct converged minima, best first. alt < -1e4
// means no altitude row.
func solveTOA(toas []TOA, alt, altSig float64, atm *geo.Atmosphere) []solution {
	var cen geo.Vec3
	for _, m := range toas {
		cen = cen.Add(m.Pos)
	}
	cen = cen.Scale(1 / float64(len(toas)))
	cl := geo.FromECEF(cen)
	h0 := 9000.0
	if alt > -1e4 {
		h0 = alt
	}
	fr := geo.NewFrame(geo.LLH{Lat: cl.Lat, Lon: cl.Lon})
	var seeds []geo.Vec3
	seeds = append(seeds, fr.FromENU(geo.Vec3{Z: h0}))
	for _, r := range []float64{60e3, 150e3, 300e3} {
		for k := range 8 {
			a := float64(k) * math.Pi / 4
			seeds = append(seeds, fr.FromENU(geo.Vec3{X: r * math.Cos(a), Y: r * math.Sin(a), Z: h0}))
		}
	}
	var out []solution
	for _, s := range seeds {
		sol, ok := gnSolve(toas, alt, altSig, atm, s)
		if !ok {
			continue
		}
		dup := false
		for i := range out {
			if out[i].p.Dist(sol.p) < 500 {
				dup = true
				if sol.chi2 < out[i].chi2 {
					out[i] = sol
				}
			}
		}
		if !dup {
			out = append(out, sol)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].chi2 < out[j].chi2 })
	return out
}

func gnSolve(toas []TOA, alt, altSig float64, atm *geo.Atmosphere, seed geo.Vec3) (solution, bool) {
	p := seed
	var b float64 // c·τ
	for _, m := range toas {
		b += geo.C*m.T - atm.Range(m.Pos, p)
	}
	b /= float64(len(toas))
	cost := func(p geo.Vec3, b float64) float64 {
		var s float64
		for _, m := range toas {
			r := (geo.C*m.T - b - atm.Range(m.Pos, p)) / (geo.C * tsig(m))
			s += r * r
		}
		if alt > -1e4 {
			r := (geo.FromECEF(p).H - alt) / altSig
			s += r * r
		}
		return s
	}
	lambda := 1e-3
	c0 := cost(p, b)
	var nrm linalg.Normal
	for range 60 {
		nrm.Reset(4)
		for _, m := range toas {
			rho, g := atm.RangeGrad(m.Pos, p)
			w := 1 / (geo.C * tsig(m))
			r := (b + rho - geo.C*m.T) * w // model minus measurement
			nrm.AddRow([]float64{g.X * w, g.Y * w, g.Z * w, w}, r, 1)
		}
		if alt > -1e4 {
			llh := geo.FromECEF(p)
			u := geo.Up(llh.Lat, llh.Lon)
			w := 1 / altSig
			nrm.AddRow([]float64{u.X * w, u.Y * w, u.Z * w, 0}, (llh.H-alt)*w, 1)
		}
		var dx [4]float64
		if _, err := nrm.Step(lambda, 1e-9, dx[:]); err != nil {
			return solution{}, false
		}
		np := p.Add(geo.Vec3{X: dx[0], Y: dx[1], Z: dx[2]})
		nb := b + dx[3]
		c1 := cost(np, nb)
		if c1 < c0 {
			p, b, c0 = np, nb, c1
			lambda = math.Max(lambda/5, 1e-9)
			if math.Abs(dx[0])+math.Abs(dx[1])+math.Abs(dx[2]) < 0.01 {
				break
			}
		} else {
			lambda *= 8
			if lambda > 1e12 {
				break
			}
		}
	}
	n := len(toas)
	if alt > -1e4 {
		n++
	}
	if c0 > float64(n)+6*math.Sqrt(2*float64(n))+10 || geo.FromECEF(p).H < -500 {
		return solution{}, false
	}
	// covariance (JᵀWJ)⁻¹, position block
	nrm.Reset(4)
	for _, m := range toas {
		_, g := atm.RangeGrad(m.Pos, p)
		w := 1 / (geo.C * tsig(m))
		nrm.AddRow([]float64{g.X * w, g.Y * w, g.Z * w, w}, 0, 1)
	}
	if alt > -1e4 {
		llh := geo.FromECEF(p)
		u := geo.Up(llh.Lat, llh.Lon)
		nrm.AddRow([]float64{u.X / altSig, u.Y / altSig, u.Z / altSig, 0}, 0, 1)
	}
	var cv [16]float64
	if nrm.Covariance(cv[:]) != nil {
		return solution{}, false
	}
	var cov [9]float64
	for i := range 3 {
		for j := range 3 {
			cov[i*3+j] = cv[i*4+j]
		}
	}
	dof := max(n-4, 1)
	s := math.Max(1, c0/float64(dof))
	for i := range cov {
		cov[i] *= s
	}
	return solution{p: p, cov: cov, chi2: c0}, true
}

// fixFrom turns solutions into a Fix (at most two hypotheses).
func fixFrom(icao uint32, tNs int64, sols []solution, nst int) Fix {
	f := Fix{ICAO: icao, TNs: tNs, NStations: nst, DOF: nst - 3}
	for i := 0; i < len(sols) && i < 2; i++ {
		f.Hyp[i] = FixHyp{ECEF: sols[i].p, Cov: sols[i].cov}
		f.NHyp++
	}
	return f
}

// seg is a trajectory segment: constant speed, turn rate omega (rad/s),
// vertical rate vz, for dur seconds.
type seg struct{ dur, omega, vz float64 }

// path is a synthetic trajectory in the local frame fr.
type path struct {
	fr    geo.Frame
	start vec // e, n, u, ve, vn, vu, -
	segs  []seg
}

// at returns the ECEF position and the ENU (frame) state at t >= 0.
func (p *path) at(t float64) (geo.Vec3, vec) {
	x := p.start
	for _, s := range p.segs {
		d := math.Min(t, s.dur)
		x[iW] = s.omega
		x[iVU] = s.vz
		x, _ = transition(modeCT, &x, d)
		t -= d
		if t <= 0 {
			break
		}
	}
	if t > 0 {
		x[iW] = 0
		x, _ = transition(modeCT, &x, t)
	}
	return p.fr.FromENU(geo.Vec3{X: x[iE], Y: x[iN], Z: x[iU]}), x
}

// synth generates synthetic TOA clusters.
type synth struct {
	st    []geo.Vec3
	sigma []float64 // TOA 1σ per station, s
	atm   geo.Atmosphere
	rng   *rand.Rand
	base  int64 // network Unix ns of t = 0
}

func newSynth(seed uint64, st []geo.Vec3, sigmaNs float64) *synth {
	s := &synth{st: st, atm: geo.DefaultAtmosphere(), rng: rand.New(rand.NewPCG(seed, 99)), base: 1_780_000_000e9}
	for range st {
		s.sigma = append(s.sigma, sigmaNs*1e-9)
	}
	return s
}

// cluster returns the cluster of a transmission at t (s) from p, heard by
// the stations in use (nil = all).
func (s *synth) cluster(icao uint32, t float64, p geo.Vec3, use []int, altHAE, altSig float64) Cluster {
	c := Cluster{ICAO: icao, T0Ns: s.base + int64(math.Floor(t))*1e9}
	frac := t - math.Floor(t)
	if use == nil {
		use = make([]int, len(s.st))
		for i := range use {
			use[i] = i
		}
	}
	for _, j := range use {
		tt := frac + s.atm.Range(s.st[j], p)/geo.C + s.rng.NormFloat64()*s.sigma[j]
		c.TOAs = append(c.TOAs, TOA{Station: uint32(j), Pos: s.st[j], T: tt, Sigma: s.sigma[j]})
	}
	if altSig > 0 {
		c.AltHAE = altHAE + s.rng.NormFloat64()*altSig
		c.AltSigma = altSig
	}
	return c
}

func (s *synth) tNs(t float64) int64 { return s.base + int64(math.Round(t*1e9)) }

// stationsAround places n stations on a ring of radius r (m) around
// (lat, lon), plus one in the middle when centre is set.
func stationsAround(lat, lon float64, n int, r float64, centre bool) []geo.Vec3 {
	fr := geo.NewFrame(geo.LLH{Lat: lat, Lon: lon})
	var out []geo.Vec3
	if centre {
		out = append(out, fr.FromENU(geo.Vec3{Z: 150}))
	}
	for k := range n {
		a := 2 * math.Pi * float64(k) / float64(n)
		out = append(out, fr.FromENU(geo.Vec3{X: r * math.Cos(a), Y: r * math.Sin(a), Z: 150}))
	}
	return out
}

// errStats collects horizontal errors and normalised errors.
type errStats struct {
	errs   []float64
	nees   []float64 // horizontal NEES (2 dof)
	sigmas []float64
}

// add records the error of estimate e against the true ECEF position.
func (s *errStats) add(e *Estimate, truth geo.Vec3) float64 {
	lf := geo.NewFrame(e.LLH)
	d := lf.Rotate(truth.Sub(e.ECEF))
	pl := lf.CovToENU(e.Cov)
	h := math.Hypot(d.X, d.Y)
	s.errs = append(s.errs, h)
	det := pl[0]*pl[4] - pl[1]*pl[3]
	nees := (pl[4]*d.X*d.X - 2*pl[1]*d.X*d.Y + pl[0]*d.Y*d.Y) / det
	s.nees = append(s.nees, nees)
	s.sigmas = append(s.sigmas, e.SigmaH)
	return h
}

func pct(v []float64, q float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	i := int(math.Ceil(q*float64(len(c)))) - 1
	return c[max(0, min(i, len(c)-1))]
}

// within returns the fraction of NEES values at or below the 2-dof
// quantile of probability p.
func (s *errStats) within(p float64) float64 {
	if len(s.nees) == 0 {
		return math.NaN()
	}
	q := Chi2Quantile(p, 2)
	n := 0
	for _, v := range s.nees {
		if v <= q {
			n++
		}
	}
	return float64(n) / float64(len(s.nees))
}

func (s *errStats) maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

// tsig is the total 1σ of a TOA for a snapshot solve.
func tsig(m TOA) float64 { return math.Hypot(m.Sigma, m.SyncSigma) }
