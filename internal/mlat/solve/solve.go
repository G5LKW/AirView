package solve

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
	"airview/internal/model"
)

// Solve solves one cluster and writes the result into out (which is fully
// overwritten). It returns an error when no fix could be computed at all;
// otherwise check out.OK() / out.Reject. Solve does not allocate.
func (s *Solver) Solve(pr *Problem, out *Fix) error {
	*out = Fix{}
	if err := s.setup(pr); err != nil {
		return err
	}
	if err := s.buildRows(); err != nil {
		return ErrBadInput
	}
	h, ok := s.multiStart()
	if !ok {
		out.Starts, out.Iter = s.ns, s.iters
		return ErrNoSolution
	}
	out.Starts = s.ns
	out.Init = h.init
	s.restore(h)

	// alternates (before the robust stage changes the row set)
	s.collectAlternates(h, out)

	exclusions, raimFailed, ransac := s.robust()
	if exclusions < 0 {
		out.Iter = s.iters
		return ErrNoSolution
	}
	out.Exclusions = exclusions
	if ransac {
		out.Flags |= FlagRANSAC
		out.Init = InitRANSAC
	}
	if raimFailed {
		out.Flags |= FlagRAIMFailed
	}
	s.finish(pr, out)
	return nil
}

// restore loads a hypothesis as the current estimate.
func (s *Solver) restore(h *hyp) {
	s.frame, s.p, s.conv = h.frame, h.p, h.conv
	if s.linCoarse || s.position(&s.p).Dist(s.lin) > s.opt.RelinM {
		s.linearize(s.position(&s.p))
	}
}

// runSeed runs LM from a seed and records the hypothesis (deduplicated).
func (s *Solver) runSeed(sd seed) *hyp {
	s.startAt(sd.x)
	s.coarse = sd.init == InitGrid || sd.init == InitCentroid
	it := s.opt.MaxIter
	if s.coarse {
		it *= 2 // kilometres away: allow the extra damped iterations
	}
	cost := s.lm(it)
	if !s.conv && s.plausible(s.position(&s.p)) {
		// far-out targets: the vertical/transmit-time valley is long and
		// flat; one more round finishes what a closed-form seed started.
		// It runs on the exact excess model: where the coarse one did not
		// converge (long, low paths), its valley is the wrong one to follow.
		s.exactLin()
		cost = s.lm(s.opt.MaxIter)
	}
	s.coarse = false
	x := s.position(&s.p)
	h := hyp{x: x, frame: s.frame, p: s.p, chi2: 2 * cost, conv: s.conv, init: sd.init}
	h.ok = s.plausible(x)
	h.vis = h.ok && s.visible(x)
	for k := 0; k < s.nh; k++ {
		if s.hyps[k].x.Dist(x) < 20 {
			if h.chi2 < s.hyps[k].chi2 {
				s.hyps[k] = h
			}
			return &s.hyps[k]
		}
	}
	if s.nh < len(s.hyps) {
		s.hyps[s.nh] = h
		s.nh++
		return &s.hyps[s.nh-1]
	}
	s.spare = h
	return &s.spare
}

// good reports whether a hypothesis is a consistent, plausible solution.
func (s *Solver) good(h *hyp) bool {
	if !h.ok || !h.vis || !h.conv {
		return false
	}
	d := s.dof()
	if d <= 0 {
		return true
	}
	return h.chi2 <= s.chi2T[d]
}

// nearFound reports whether x is within dist of an already found
// converged hypothesis (a seed there would converge to the same point).
func (s *Solver) nearFound(x geo.Vec3, dist float64) bool {
	for k := 0; k < s.nh; k++ {
		if s.hyps[k].conv && s.hyps[k].x.Dist(x) < dist {
			return true
		}
	}
	return false
}

// multiStart runs the initialisation cascade of DESIGN §9.4 and returns the
// best hypothesis.
func (s *Solver) multiStart() (*hyp, bool) {
	s.nh, s.ns = 0, 0
	idx := s.activeToa()
	n := len(idx)
	d := s.dof()
	altH := 10000.0
	if s.hasAlt {
		altH = s.altH
	} else if s.hasPrior {
		altH = geo.FromECEF(s.priorPos).H
	}
	needAll := d <= 1 || s.opt.AlwaysAlternates
	s.coarseOK = !needAll && !s.opt.NoRefraction && !s.exactOnly
	defer func() { s.coarseOK = false }()

	try := func(sd seed) (*hyp, bool) {
		// A closed-form seed below the ground is the up/down mirror image
		// of the solution (nearly coplanar stations; the linear TOA solution
		// of far-out targets lands tens of km below ground in about one
		// case in eight): start from its reflection straight away instead of
		// converging LM to the implausible side first and mirroring after
		// (the second run below). Saves one full LM run in those cases.
		if sd.init != InitPrior && !needAll {
			if ll := geo.FromECEF(sd.x); ll.H < s.opt.MinHAE && !math.IsNaN(ll.H) {
				mh := math.Min(2*s.meanStationH()-ll.H, s.opt.MaxHAE)
				sd.x = geo.ToECEF(ll.Lat, ll.Lon, mh)
			}
		}
		if s.nearFound(sd.x, 1000) {
			return nil, false
		}
		s.ns++
		h := s.runSeed(sd)
		if g := s.good(h); g || s.ns > 6 {
			return h, g
		}
		// Up/down mirror: the stations are nearly coplanar, so the
		// reflection of the solution through their plane fits almost as
		// well. A start that ended below ground is retried from its
		// mirror image (same latitude/longitude, height reflected about
		// the mean station height).
		if ll := geo.FromECEF(h.x); ll.H < s.opt.MinHAE && !math.IsNaN(ll.H) {
			mh := 2*s.meanStationH() - ll.H
			if mh > s.opt.MaxHAE {
				mh = s.opt.MaxHAE
			}
			mx := geo.ToECEF(ll.Lat, ll.Lon, mh)
			if !s.nearFound(mx, 1000) {
				s.ns++
				m := s.runSeed(seed{mx, sd.init})
				if s.good(m) {
					return m, true
				}
			}
		}
		return h, false
	}

	// 1. track prediction
	if s.hasPrior && s.priorSH < s.opt.PriorSeedMaxSigma {
		if h, g := try(seed{s.priorPos, InitPrior}); g && (!needAll || s.fuse) {
			return h, true
		}
	}
	// 2. linear TOA (N ≥ 5)
	if n >= 5 {
		if x, ok := s.linearToa(idx); ok {
			x = s.withExcess(x, func(out []geo.Vec3) int {
				y, ok := s.linearToa(idx)
				if !ok {
					return 0
				}
				out[0] = y
				return 1
			})
			if h, g := try(seed{x, InitLinear}); g && !needAll {
				return h, true
			}
		}
	}
	// 3. Bancroft (N ≥ 4); both roots when the problem is not
	// over-determined
	if n >= 4 {
		var xs [2]geo.Vec3
		k := s.bancroft(idx, xs[:])
		for j := 0; j < k; j++ {
			if s.nearFound(xs[j], 1000) {
				continue
			}
			x := s.withExcess(xs[j], func(out []geo.Vec3) int { return s.bancroft(idx, out) })
			if h, g := try(seed{x, InitBancroft}); g && !needAll {
				return h, true
			}
		}
	}
	// 4. 3 + altitude: closed form, then the grid when that found nothing
	// consistent
	if n == 3 && s.hasAlt && !s.altOff {
		var xs [4]geo.Vec3
		k := s.quartic(idx, s.altH, xs[:])
		for j := 0; j < k; j++ {
			if s.nearFound(xs[j], 1000) {
				continue
			}
			x := s.withExcess(xs[j], func(out []geo.Vec3) int { return s.quartic(idx, s.altH, out) })
			try(seed{x, InitQuartic})
		}
		if g := s.countGood(); g == 0 || (g < 2 && (s.opt.AlwaysAlternates || s.loneFar())) {
			var gs [3]geo.Vec3
			k = s.gridSeeds(idx, s.altH, s.opt.GridStepM, gs[:])
			for j := 0; j < k; j++ {
				try(seed{gs[j], InitGrid})
			}
		}
	}
	// 4b. 4 + altitude (DOF 1) has the same two-branch structure; when the
	// only solution found is far out, look for the near branch
	if n == 4 && s.hasAlt && !s.altOff && !s.fuse && s.countGood() == 1 && s.loneFar() {
		var gs [3]geo.Vec3
		k := s.gridSeeds(idx, s.altH, 2*s.opt.GridStepM, gs[:])
		for j := 0; j < k; j++ {
			try(seed{gs[j], InitGrid})
		}
	}
	// 5. centroid at the altitude
	if s.countUsable() == 0 {
		try(seed{s.centroidSeed(altH), InitCentroid})
	}
	// still nothing: the grid also for other problems (gross outliers or
	// near-tangent geometry can defeat the closed forms); without an
	// altitude at two typical heights, the LM then frees the height. A
	// plausible, visible start that fits but did not formally converge
	// (the long flat vertical/transmit-time valley of far-out targets
	// without an altitude row) is not "nothing": the grid would only find
	// the same valley again, at ~0.4 ms per call (H-track, solver budget).
	if s.countUsable() == 0 && !s.fitsUnconverged() && n >= 3 && !(n == 3 && s.hasAlt && !s.altOff) {
		var gs [2]geo.Vec3
		hs := [2]float64{9000, 3000}
		nh := 2
		if s.hasAlt && !s.altOff {
			hs[0], nh = s.altH, 1
		}
		for j := 0; j < nh; j++ {
			k := s.gridSeeds(idx, hs[j], 2*s.opt.GridStepM, gs[:])
			for q := 0; q < k; q++ {
				try(seed{gs[q], InitGrid})
			}
		}
	}
	return s.pickBest()
}

// countUsable counts plausible, visible hypotheses whatever their χ²
// (an inconsistent one is the robust stage's business, not a reason for
// more expensive starts).
func (s *Solver) countUsable() int {
	n := 0
	for k := 0; k < s.nh; k++ {
		if h := &s.hyps[k]; h.ok && h.vis && h.conv {
			n++
		}
	}
	return n
}

// fitsUnconverged reports whether some plausible, visible hypothesis is
// consistent (χ² within the RAIM threshold) although LM did not declare
// convergence.
func (s *Solver) fitsUnconverged() bool {
	d := s.dof()
	for k := 0; k < s.nh; k++ {
		h := &s.hyps[k]
		if h.ok && h.vis && !h.conv && (d <= 0 || h.chi2 <= s.chi2T[d]) {
			return true
		}
	}
	return false
}

// loneFarM: a single consistent solution whose farthest station is
// further than this triggers the search for a second, nearer branch
// (3 + altitude, 4 + altitude). Most 3-station clusters are near their
// stations, so the common case pays nothing.
const loneFarM = 250e3

// loneFar reports whether the (single) good hypothesis is far out and no
// prior can arbitrate.
func (s *Solver) loneFar() bool {
	if s.hasPrior && s.priorSH < s.opt.PriorSeedMaxSigma {
		return false
	}
	for k := 0; k < s.nh; k++ {
		if h := &s.hyps[k]; s.good(h) {
			return s.maxRange(h.x) > loneFarM
		}
	}
	return false
}

func (s *Solver) countGood() int {
	n := 0
	for k := 0; k < s.nh; k++ {
		if s.good(&s.hyps[k]) {
			n++
		}
	}
	return n
}

// priorMaha returns the Mahalanobis distance² of x from the prior.
func (s *Solver) priorMaha(x geo.Vec3) float64 {
	m := linalg.Mat3(s.priorCov)
	inv, err := m.InvSPD()
	if err != nil {
		return 0
	}
	d := x.Sub(s.priorPos)
	v := linalg.Vec3{d.X, d.Y, d.Z}
	return inv.Quad(v)
}

// score orders hypotheses: plausible and converged first, then χ² (plus
// the prior's Mahalanobis distance when a prior is known but not fused).
func (s *Solver) score(h *hyp) float64 {
	sc := h.chi2
	if s.hasPrior && !s.fuse {
		sc += s.priorMaha(h.x)
	}
	// Reception prior: an aircraft heard by every station of the cluster
	// is far more likely near them than at the edge of their horizons.
	// (max range / 300 km)² is a tie-breaker between hypotheses that fit
	// equally well (3 + altitude and 4-station mirror solutions), small
	// against any real χ² difference.
	r := s.maxRange(h.x) / 300e3
	sc += r * r
	if !s.good(h) {
		sc += 1e12
	}
	if !h.vis {
		sc += 1e14
	}
	if !h.ok {
		sc += 1e15
	}
	return sc
}

// meanStationH returns the mean height of the active stations.
func (s *Solver) meanStationH() float64 {
	var sum float64
	n := 0
	for k := 0; k < s.nr; k++ {
		if i := s.rows[k]; i < MaxStations {
			sum += geo.GeocentricHeight(s.pos[i])
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// plausibleLoose accepts anything within 50 km of the height band and
// 2·MaxRangeM of a station: a candidate for the robust stage.
func (s *Solver) plausibleLoose(x geo.Vec3) bool {
	h := geo.GeocentricHeight(x)
	return h > -50000 && h < s.opt.MaxHAE+50000 && s.maxRange(x) < 2*s.opt.MaxRangeM
}

// maxRange returns the largest distance from x to an active station.
func (s *Solver) maxRange(x geo.Vec3) float64 {
	var m float64
	for k := 0; k < s.nr; k++ {
		if i := s.rows[k]; i < MaxStations {
			m = math.Max(m, x.Dist(s.pos[i]))
		}
	}
	return m
}

func (s *Solver) pickBest() (*hyp, bool) {
	var best *hyp
	bs := math.Inf(1)
	for k := 0; k < s.nh; k++ {
		h := &s.hyps[k]
		// Implausible hypotheses stay candidates (heavily penalised by
		// score): a single gross fault can push every start below ground
		// or behind the horizon, and only the robust stage, after
		// excluding it, brings the solution back. The final rejection
		// tests decide.
		if !finiteVec(h.x) || math.IsNaN(h.chi2) || !s.plausibleLoose(h.x) {
			continue
		}
		if sc := s.score(h); sc < bs {
			best, bs = h, sc
		}
	}
	return best, best != nil
}

// collectAlternates records other consistent hypotheses more than 1 km
// from the chosen one.
func (s *Solver) collectAlternates(best *hyp, out *Fix) {
	for k := 0; k < s.nh && out.NAlt < len(out.Alt); k++ {
		h := &s.hyps[k]
		if h == best || !s.good(h) || h.x.Dist(best.x) < 1000 {
			continue
		}
		sh, cov := s.sigmaHAt(h)
		out.Alt[out.NAlt] = Hypothesis{ECEF: h.x, Chi2: h.chi2, Init: h.init, SigmaH: sh, CovECEF: cov}
		out.NAlt++
	}
	if out.NAlt > 0 {
		out.Flags |= FlagAmbiguous
	}
}

// sigmaHAt returns the horizontal CRLB σ (m, unscaled) and the ECEF
// position CRLB of a hypothesis under the current rows and nominal
// covariance.
func (s *Solver) sigmaHAt(h *hyp) (float64, [9]float64) {
	p0, f0 := s.p, s.frame
	s.p, s.frame = h.p, h.frame
	n := s.nr
	r, J := s.rb[1][:n], s.jb[1][:n*nParam]
	s.eval(&s.p, r, J)
	jw := s.jwb[1][:n*nParam]
	copy(jw, J)
	linalg.Whiten(s.lnom[:n*n], n, nil, jw, nParam)
	s.p, s.frame = p0, f0
	var N linalg.Mat4
	for i := 0; i < n; i++ {
		row := jw[i*nParam : i*nParam+nParam]
		for a := 0; a < nParam; a++ {
			for b := 0; b < nParam; b++ {
				N[a*4+b] += row[a] * row[b]
			}
		}
	}
	inv, err := N.InvSPD()
	if err != nil {
		return math.Inf(1), [9]float64{}
	}
	var pc [9]float64
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			pc[a*3+b] = inv[a*4+b]
		}
	}
	ecef := h.frame.CovToECEF(pc)
	ff := geo.NewFrameECEF(h.x)
	enu := ff.CovToENU(ecef)
	return linalg.HorizontalSigma(enu[0], enu[4]), ecef
}

// ---------------------------------------------------------------------
// robust stage

// wtests computes, at the current estimate and under the nominal
// covariance, the weighted SSE χ² = ‖L⁻¹r‖² and Baarda's w-test of every
// active row:
//
//	wᵢ = cᵢᵀΣ⁻¹r / sqrt(cᵢᵀΣ⁻¹Q_rΣ⁻¹cᵢ),  Q_r = Σ − J(JᵀΣ⁻¹J)⁻¹Jᵀ
//
// computed in whitened form with mᵢ = L⁻¹cᵢ: wᵢ = mᵢᵀẽ / sqrt(‖mᵢ‖² −
// (J̃ᵀmᵢ)ᵀN⁻¹(J̃ᵀmᵢ)). For a diagonal Σ this is the studentised residual
// rᵢ/(σᵢ·sqrt(1 − hᵢᵢ)). It returns false when the normal matrix is
// singular.
func (s *Solver) wtests() bool {
	// The robust stage and finish ask for the w-tests of the same estimate
	// up to three times in a row (Huber pass without a weight change, the
	// RAIM test at the GLS solution, the output); the result depends only
	// on the estimate, the active rows and the linearisation, so it is
	// reused while none of them changed (wtMemo).
	if s.wt.valid && s.wt.gen == s.gen && s.wt.p == s.p && s.wt.frame == s.frame {
		s.chi2, s.w, s.den = s.wt.chi2, s.wt.w, s.wt.den
		return s.wt.ok
	}
	ok := s.wtestsCompute()
	s.wt = wtMemo{valid: true, ok: ok, gen: s.gen, p: s.p, frame: s.frame, chi2: s.chi2, w: s.w, den: s.den}
	return ok
}

// wtMemo is the memo of the last w-test computation (see wtests).
type wtMemo struct {
	valid, ok bool
	gen       uint64
	p         [nParam]float64
	frame     geo.Frame
	chi2      float64
	w, den    [maxRows]float64
}

func (s *Solver) wtestsCompute() bool {
	n := s.nr
	k := 0
	r, J := s.rb[k][:n], s.jb[k][:n*nParam]
	s.eval(&s.p, r, J)
	e, jw := s.rwb[k][:n], s.jwb[k][:n*nParam]
	copy(e, r)
	copy(jw, J)
	L := s.lnom[:n*n]
	linalg.Whiten(L, n, e, jw, nParam)
	s.chi2 = linalg.Dot(e, e)
	var N linalg.Mat4
	for i := 0; i < n; i++ {
		row := jw[i*nParam : i*nParam+nParam]
		for a := 0; a < nParam; a++ {
			for b := 0; b < nParam; b++ {
				N[a*4+b] += row[a] * row[b]
			}
		}
	}
	Ninv, err := N.InvSPD()
	if err != nil {
		for i := 0; i < n; i++ {
			s.w[i] = 0
		}
		return false
	}
	if !s.linvOK {
		li := s.linv[:n*n]
		copy(li, L)
		if linalg.InvertLower(li, n, s.invWork[:n]) != nil {
			return false
		}
		s.linvOK = true
	}
	li := s.linv[:n*n]
	for a := 0; a < n; a++ {
		var num, mm float64
		var v linalg.Vec4
		for i := a; i < n; i++ {
			mi := li[i*n+a]
			if mi == 0 {
				continue
			}
			num += mi * e[i]
			mm += mi * mi
			row := jw[i*nParam : i*nParam+nParam]
			v[0] += mi * row[0]
			v[1] += mi * row[1]
			v[2] += mi * row[2]
			v[3] += mi * row[3]
		}
		den := mm - Ninv.Quad(v)
		if den > 1e-10*mm {
			s.w[a] = num / math.Sqrt(den)
			s.den[a] = den
		} else {
			s.w[a] = 0 // no redundancy for this row
			s.den[a] = 0
		}
	}
	return true
}

// huberIRLS iterates Huber weights ω = min(1, k/|w|) on the w-tests and
// re-solves with Σ inflated row-wise (Σ' = SΣS, S = diag(1/√ω)) until the
// weights settle (at most 5 passes). Only with DOF ≥ 2: below that a
// single residual cannot be attributed.
func (s *Solver) huberIRLS() {
	if s.dof() < 2 {
		return
	}
	k := s.opt.HuberK
	for pass := 0; pass < 5; pass++ {
		if !s.wtests() {
			return
		}
		var change float64
		for a := 0; a < s.nr; a++ {
			nw := 1.0
			if s.rows[a] < rowPrior {
				if aw := math.Abs(s.w[a]); aw > k {
					nw = k / aw
				}
			}
			change = math.Max(change, math.Abs(nw-s.hw[a]))
			s.hw[a] = nw
		}
		if change < 0.01 {
			return
		}
		s.applyHuber()
		s.lm(s.opt.MaxIter)
	}
}

// testGLS evaluates the RAIM statistics at the nominal-weight (GLS)
// solution. χ² is χ²(DOF)-distributed only at the minimiser of the nominal
// cost; at the Huber estimate it is biased upwards (badly so along the
// flat vertical/transmit-time direction of far-out targets). When Huber
// weights are active, the M-estimate is saved, the weights are reset and
// LM finishes the GLS solution, whose χ², w-tests and redundancies are
// kept for the test and the output (useRobust restores the M-estimate).
func (s *Solver) testGLS() {
	s.haveRobust = false
	if s.huber {
		s.robP, s.robF, s.robConv = s.p, s.frame, s.conv
		s.robHW = s.hw
		s.haveRobust = true
		for a := 0; a < s.nr; a++ {
			s.hw[a] = 1
		}
		s.applyHuber()
		s.lm(s.opt.MaxIter)
	}
	s.wtests()
	s.tChi2, s.tW, s.tDen = s.chi2, s.w, s.den
	s.tValid = true
}

// useRobust returns to the Huber M-estimate saved by testGLS (if any).
func (s *Solver) useRobust() {
	if !s.haveRobust {
		return
	}
	s.p, s.frame, s.conv = s.robP, s.robF, s.robConv
	s.hw = s.robHW
	s.applyHuber()
	s.haveRobust = false
}

// robust runs the Huber stage and RAIM fault detection and exclusion
// (DESIGN §9.4): the weighted SSE is tested against χ²(DOF) at PFA; on
// failure the row with the largest |w| is excluded and the problem
// re-solved while DOF ≥ 1 remains; RANSAC is tried when RAIM has failed
// twice and there are at least RANSACMinStations stations. It returns the
// number of exclusions (−1 on a numerical failure), whether the final test
// still fails, and whether RANSAC chose the inliers.
func (s *Solver) robust() (excl int, failed, ransac bool) {
	fails := 0
	ransacTried := false
	for {
		s.huberIRLS()
		s.testGLS()
		d := s.dof()
		if d < 1 || s.chi2 <= s.chi2T[d] {
			s.useRobust()
			return excl, false, ransac
		}
		fails++
		if fails >= 2 && !ransacTried && s.m >= s.opt.RANSACMinStations {
			ransacTried = true
			if s.ransac() {
				ransac = true
				excl = 0
				for i := 0; i < s.m; i++ {
					if s.excl[i] {
						excl++
					}
				}
				continue
			}
		}
		if d-1 < 1 {
			return excl, true, ransac
		}
		if !s.excludeBest() {
			return excl, true, ransac
		}
		excl++
	}
}

// excludable reports whether active row a may be excluded: TOA rows and
// the altitude row, keeping at least 2 TOA rows with a fused prior and 3
// otherwise (the DOF ≥ 1 rule is checked by the caller).
func (s *Solver) excludable(a int) bool {
	code := s.rows[a]
	switch {
	case code >= rowPrior:
		return false
	case code < MaxStations:
		minToa := 3
		if s.fuse {
			minToa = 2
		}
		return s.nToa-1 >= minToa
	}
	return true
}

// setExcluded marks active row a as excluded (or included again).
func (s *Solver) setExcluded(code int, v bool) {
	if code == rowAlt {
		s.altOff = v
	} else {
		s.excl[code] = v
	}
}

// excludeBest performs one RAIM exclusion. The w-test identifies a single
// fault reliably only when the rows' test statistics are weakly
// correlated; with little redundancy the largest |w| can point at an
// innocent row. So the (up to) three rows with the largest |w| are each
// tried: excluded, re-solved from the current estimate, and the one whose
// remaining measurements fit best (smallest χ²) is excluded for good. The
// solver is left on that solution. It returns false when no row can be
// excluded.
func (s *Solver) excludeBest() bool {
	var cand [3]int
	var cw [3]float64
	nc := 0
	for a := 0; a < s.nr; a++ {
		if !s.excludable(a) {
			continue
		}
		aw := math.Abs(s.w[a])
		pos := nc
		for pos > 0 && cw[pos-1] < aw {
			pos--
		}
		if pos >= len(cand) {
			continue
		}
		if nc < len(cand) {
			nc++
		}
		for k := nc - 1; k > pos; k-- {
			cand[k], cw[k] = cand[k-1], cw[k-1]
		}
		cand[pos], cw[pos] = s.rows[a], aw
	}
	if nc == 0 {
		return false
	}
	p0, f0 := s.p, s.frame
	best, bestChi := -1, math.Inf(1)
	var bestP [nParam]float64
	var bestF geo.Frame
	for k := 0; k < nc; k++ {
		// a candidate far below the leader cannot be the fault
		if k > 0 && cw[k] < 0.5*cw[0] {
			break
		}
		s.setExcluded(cand[k], true)
		if s.buildRows() == nil {
			s.p, s.frame = p0, f0
			s.lm(s.opt.MaxIter)
			s.wtests()
			if s.chi2 < bestChi {
				best, bestChi, bestP, bestF = cand[k], s.chi2, s.p, s.frame
			}
		}
		s.setExcluded(cand[k], false)
		if nc == 1 {
			break
		}
	}
	if best < 0 {
		_ = s.buildRows()
		s.p, s.frame = p0, f0
		return false
	}
	s.setExcluded(best, true)
	if s.buildRows() != nil {
		return false
	}
	s.p, s.frame = bestP, bestF
	if s.linCoarse || s.position(&s.p).Dist(s.lin) > s.opt.RelinM {
		s.linearize(s.position(&s.p))
	}
	s.conv = true
	s.lm(s.opt.MaxIter)
	return true
}

// ransac tries minimal subsets (4 TOA, plus the altitude row when present)
// and keeps the solution with the largest consensus set (|residual| ≤ 3σ).
// It leaves the solver on the re-solved inlier set and returns true, or
// restores the previous state and returns false.
func (s *Solver) ransac() bool {
	m := s.m
	var saveExcl [MaxStations]bool
	saveExcl = s.excl
	saveAlt := s.altOff
	saveP, saveF, saveConv := s.p, s.frame, s.conv

	bestCnt, bestCost := 0, math.Inf(1)
	var bestX geo.Vec3
	var bestIn [MaxStations]bool

	// subset enumeration: lexicographic when C(m,4) is small, else a
	// deterministic pseudo-random sample
	total := m * (m - 1) * (m - 2) * (m - 3) / 24
	maxSub := s.opt.RANSACMaxSubsets
	var comb [4]int
	for k := range comb {
		comb[k] = k
	}
	rng := uint64(0x9E3779B97F4A7C15) ^ uint64(m)
	next := func() bool {
		if total <= maxSub {
			// next combination
			k := 3
			for k >= 0 && comb[k] == m-4+k {
				k--
			}
			if k < 0 {
				return false
			}
			comb[k]++
			for j := k + 1; j < 4; j++ {
				comb[j] = comb[j-1] + 1
			}
			return true
		}
		for {
			for k := 0; k < 4; k++ {
				rng ^= rng << 13
				rng ^= rng >> 7
				rng ^= rng << 17
				comb[k] = int(rng % uint64(m))
			}
			if comb[0] != comb[1] && comb[0] != comb[2] && comb[0] != comb[3] &&
				comb[1] != comb[2] && comb[1] != comb[3] && comb[2] != comb[3] {
				return true
			}
		}
	}
	for sub := 0; sub < maxSub; sub++ {
		if sub > 0 && !next() {
			break
		}
		for i := 0; i < m; i++ {
			s.excl[i] = true
		}
		for _, i := range comb {
			s.excl[i] = false
		}
		s.altOff = false
		if s.buildRows() != nil {
			continue
		}
		idx := s.activeToa()
		var xs [2]geo.Vec3
		k := s.bancroft(idx, xs[:])
		for j := 0; j < k; j++ {
			s.startAt(xs[j])
			s.lm(s.opt.MaxIter)
			x := s.position(&s.p)
			if !s.conv || !s.plausible(x) {
				continue
			}
			// consensus over all stations
			cnt := 0
			var cost float64
			var in [MaxStations]bool
			for i := 0; i < m; i++ {
				rho, _ := s.rangeTo(i, x)
				res := (rho + s.p[3] - s.z[i]) / s.sd[i]
				if math.Abs(res) <= 3 {
					in[i] = true
					cnt++
					cost += res * res
				} else {
					cost += 9
				}
			}
			if s.hasAlt {
				if dh := (geo.FromECEF(x).H - s.altH) / s.altS; math.Abs(dh) > 3 {
					cost += 9
				}
			}
			if cnt > bestCnt || (cnt == bestCnt && cost < bestCost) {
				bestCnt, bestCost, bestX, bestIn = cnt, cost, x, in
			}
		}
	}
	if bestCnt < 5 {
		s.excl, s.altOff = saveExcl, saveAlt
		_ = s.buildRows()
		s.p, s.frame, s.conv = saveP, saveF, saveConv
		return false
	}
	for i := 0; i < m; i++ {
		s.excl[i] = !bestIn[i]
	}
	s.altOff = false
	if s.buildRows() != nil {
		s.excl, s.altOff = saveExcl, saveAlt
		_ = s.buildRows()
		s.p, s.frame, s.conv = saveP, saveF, saveConv
		return false
	}
	s.startAt(bestX)
	s.lm(s.opt.MaxIter)
	return true
}

// ---------------------------------------------------------------------
// output

// finish computes the covariance, DOP, residuals, rejection tests and the
// grade of the current estimate.
func (s *Solver) finish(pr *Problem, out *Fix) {
	if s.exactLin() {
		// an estimate that never converged on the coarse excess model
		// (RANSAC's last start): finish it on the exact one
		s.lm(s.opt.MaxIter)
	}
	n := s.nr
	x := s.position(&s.p)
	llh := geo.FromECEF(x)
	out.ECEF, out.LLH = x, llh
	out.TxS = s.tref + s.p[3]/c
	out.Iter = s.iters
	if s.conv {
		out.Flags |= FlagConverged
	}

	// w-tests and χ² under the nominal covariance (also leaves the whitened
	// Jacobian of the nominal model in jwb[0])
	s.wtests() // Jacobian at the final estimate (jwb[0])
	if s.tValid {
		s.chi2, s.w, s.den = s.tChi2, s.tW, s.tDen
	}
	out.Chi2 = s.chi2
	out.DOF = s.dof()
	if out.DOF == 0 {
		out.Flags |= FlagUnderdetermined
	}
	jw := s.jwb[0][:n*nParam]
	var N linalg.Mat4
	for i := 0; i < n; i++ {
		row := jw[i*nParam : i*nParam+nParam]
		for a := 0; a < nParam; a++ {
			for b := 0; b < nParam; b++ {
				N[a*4+b] += row[a] * row[b]
			}
		}
	}
	Ninv, err := N.InvSPD()
	if err != nil {
		for i := range Ninv {
			Ninv[i] = math.Inf(1)
		}
	}
	scale := 1.0
	if out.DOF > 0 {
		scale = math.Max(1, out.Chi2/float64(out.DOF))
	}
	out.Scale = scale
	var pc [9]float64 // position block in the start frame
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			pc[a*3+b] = Ninv[a*4+b]
		}
	}
	ecef := s.frame.CovToECEF(pc)
	ff := geo.NewFrame(llh)
	enu := ff.CovToENU(ecef)
	out.CRLBENU = enu
	for i := range ecef {
		out.CovECEF[i] = ecef[i] * scale
		out.CovENU[i] = enu[i] * scale
	}
	out.VarTx = Ninv[15] * scale / (c * c)
	out.SigmaH = linalg.HorizontalSigma(out.CovENU[0], out.CovENU[4])
	out.SigmaV = math.Sqrt(math.Max(out.CovENU[8], 0))
	out.Ellipse = linalg.ErrorEllipse(out.CovENU[0], out.CovENU[1], out.CovENU[4])
	if err != nil {
		out.SigmaH, out.SigmaV = math.Inf(1), math.Inf(1)
	}

	// per-station outputs
	for a := 0; a < n; a++ {
		code := s.rows[a]
		switch {
		case code < MaxStations:
			out.Used |= 1 << uint(code)
			out.NStations++
			out.WTest[code] = float32(s.w[a])
			if s.den[a] > 0 {
				out.MDB[code] = float32(mdbDelta / math.Sqrt(s.den[a]))
			} else {
				out.MDB[code] = float32(math.Inf(1))
			}
			out.Weight[code] = float32(s.hw[a])
		case code == rowAlt:
			out.AltUsed = true
			out.Flags |= FlagAltConstrained
		default:
			out.PriorUsed = true
		}
	}
	if out.PriorUsed {
		out.Flags |= FlagPriorFused
	}
	if s.huber {
		out.Flags |= FlagHuber
	}
	for i := 0; i < s.m; i++ {
		rho, _ := s.rangeTo(i, x)
		out.Resid[i] = float32(rho + s.p[3] - s.z[i])
		if s.excl[i] {
			out.Excluded |= 1 << uint(i)
		}
	}
	if s.hasAlt {
		out.AltResid = llh.H - s.altH
		out.AltExcluded = s.altOff
	}
	if out.Excluded != 0 || out.AltExcluded {
		out.Flags |= FlagRAIMExcluded
	}
	out.DOP = s.dop(x, &ff)

	// rejection tests (DESIGN §9.4)
	o := &s.opt
	if !s.conv {
		out.Reject |= RejectNotConverged
	}
	if !(out.SigmaH <= o.MaxSigmaH) {
		out.Reject |= RejectSigma
	}
	if llh.H < o.MinHAE || llh.H > o.MaxHAE || !s.plausible(x) {
		out.Reject |= RejectImplausible
	}
	if s.hasAlt {
		if out.AltUsed {
			for a := 0; a < n; a++ {
				if s.rows[a] == rowAlt && math.Abs(s.w[a]) > o.AltRejectSigma {
					out.Reject |= RejectAltitude
				}
			}
		} else if sg := math.Sqrt(s.altS*s.altS + out.CovENU[8]); math.Abs(out.AltResid) > o.AltRejectSigma*sg {
			out.Reject |= RejectAltitude
		}
	}
	if !s.visible(x) {
		out.Reject |= RejectHorizon
	}
	if pr.Last.Valid && pr.Last.DtS > 0 {
		allow := 3 * math.Hypot(out.SigmaH, pr.Last.SigmaM)
		v := math.Max(0, x.Dist(pr.Last.Pos)-allow) / pr.Last.DtS
		lim := o.MaxSpeedMps
		if pr.Last.Mach > 0 {
			lim = math.Max(lim, o.MachFactor*pr.Last.Mach*SpeedOfSound(llh.H))
		}
		if v > lim {
			out.Reject |= RejectSpeed
		}
	}
	if pr.HostValid {
		first := math.Inf(1)
		for a := 0; a < n; a++ {
			if i := s.rows[a]; i < MaxStations {
				rho, _ := s.rangeTo(i, x)
				first = math.Min(first, rho)
			}
		}
		if d := pr.HostFirstS - (out.TxS + first/c); math.Abs(d) > o.HostTolS {
			out.Reject |= RejectHostTime
		}
	}
	if out.Flags&FlagRAIMFailed != 0 && out.DOF > 0 && Chi2Survival(out.Chi2, out.DOF) < 1e-9 {
		out.Reject |= RejectRAIM
	}
	if out.Reject == 0 {
		out.Grade = model.GradeFor(out.SigmaH, out.DOF)
	}
}

// visible checks the 4/3-earth line of sight from every used station.
func (s *Solver) visible(x geo.Vec3) bool {
	hx := geo.GeocentricHeight(x)
	for a := 0; a < s.nr; a++ {
		i := s.rows[a]
		if i >= MaxStations {
			continue
		}
		hs := geo.GeocentricHeight(s.pos[i])
		// cheap accept well inside the smooth-earth radio horizon
		if x.Dist(s.pos[i]) < 0.9*geo.RadioHorizon(hs, hx, geo.DefaultKFactor) {
			continue
		}
		if geo.Clearance(s.pos[i], x, geo.DefaultKFactor) < -s.opt.HorizonSlackM {
			return false
		}
	}
	return true
}

// dop returns the unit-variance DOPs of the used TOA rows at x in frame f.
func (s *Solver) dop(x geo.Vec3, f *geo.Frame) DOP {
	var G linalg.Mat4
	cnt := 0
	for a := 0; a < s.nr; a++ {
		i := s.rows[a]
		if i >= MaxStations {
			continue
		}
		u := f.Rotate(x.Sub(s.pos[i]).Unit())
		row := [4]float64{u.X, u.Y, u.Z, 1}
		for p := 0; p < 4; p++ {
			for q := 0; q < 4; q++ {
				G[p*4+q] += row[p] * row[q]
			}
		}
		cnt++
	}
	nan := math.NaN()
	if cnt < 4 {
		return DOP{nan, nan, nan, nan, nan}
	}
	inv, err := G.InvSPD()
	if err != nil {
		return DOP{nan, nan, nan, nan, nan}
	}
	e, nn, u, t := inv[0], inv[5], inv[10], inv[15]
	return DOP{
		G: math.Sqrt(e + nn + u + t), P: math.Sqrt(e + nn + u),
		H: math.Sqrt(e + nn), V: math.Sqrt(u), T: math.Sqrt(t),
	}
}

// mdbDelta is the non-centrality of Baarda's minimal detectable bias for a
// two-sided w-test at α = 1e-3 with 80 % power: 3.29 + 0.84.
const mdbDelta = 4.13

// SpeedOfSound returns the ISA speed of sound (m/s) at a height in metres
// (geometric height is used for the geopotential altitude; the difference
// is irrelevant for a speed gate).
func SpeedOfSound(h float64) float64 {
	t := 216.65
	if h < 11000 {
		t = 288.15 - 0.0065*h
	}
	return math.Sqrt(1.4 * 287.053 * t)
}
