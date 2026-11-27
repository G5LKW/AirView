package solve

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// Row codes of the measurement vector: 0..MaxStations-1 are TOA rows
// (Problem.Stations index), rowAlt the altitude row and rowPrior+k the
// k-th ECEF component of the prior.
const (
	rowAlt   = 100
	rowPrior = 101
)

// dmin is the floor of the Marquardt scaling diag(JᵀJ) (whitened units).
const dmin = 1e-9

// Solver solves MLAT clusters. It owns all workspace, so Solve does not
// allocate; a Solver is not safe for concurrent use (use one per worker
// goroutine, they are ~0.4 MB each).
type Solver struct {
	opt   Options
	atm   geo.Atmosphere
	chi2T [maxRows + 1]float64 // RAIM thresholds by DOF

	// problem
	m        int
	pos      [MaxStations]geo.Vec3
	z        [MaxStations]float64 // c·(TOA − tref), m
	zc       [MaxStations]float64 // z as seen by the closed-form initialisations
	tref     float64
	sig      [MaxStations * MaxStations]float64 // TOA covariance, m², stride MaxStations
	sd       [MaxStations]float64               // sqrt(diag sig), m
	hasAlt   bool
	altH     float64
	altS     float64
	hasPrior bool
	fuse     bool
	priorPos geo.Vec3
	priorCov [9]float64
	priorSH  float64 // horizontal σ of the prior
	centroid geo.Vec3

	// refraction excess linearisation: excess_i(x) ≈ e0_i + ge_i·(x − lin)
	lin   geo.Vec3
	linOK bool
	// linCoarse: e0/ge come from the cheap straight-path model
	// (linearizeCoarse) and must be replaced by the exact one before the
	// estimate is final (relinAtSolution, restore, finish).
	linCoarse bool
	// coarseOK allows the coarse phase (multiStart of over-determined
	// problems only: with DOF <= 1 the branches are close in cost and the
	// coarse path can end in the other one).
	coarseOK bool
	// exactOnly disables the coarse phase (tests compare both paths).
	exactOnly bool
	e0        [MaxStations]float64
	ge        [MaxStations]geo.Vec3

	// active rows
	excl   [MaxStations]bool
	altOff bool
	rows   [maxRows]int
	nr     int
	nToa   int
	hw     [maxRows]float64 // Huber weights per active row
	huber  bool             // some hw < 1

	lnom    [maxRows * maxRows]float64 // Cholesky factor of the nominal row covariance (stride nr)
	lw      [maxRows * maxRows]float64 // Huber-weighted factor
	linv    [maxRows * maxRows]float64 // lnom⁻¹
	linvOK  bool
	covTmp  [maxRows * maxRows]float64
	invWork [maxRows]float64

	// estimate: x = frame.FromENU(p[0:3]), b = p[3]
	frame  geo.Frame
	p      [nParam]float64
	conv   bool
	iters  int
	coarse bool // current start is a coarse seed (grid, centroid)

	// LM buffers (two sets: current and trial)
	rb  [2][maxRows]float64
	jb  [2][maxRows * nParam]float64
	rwb [2][maxRows]float64
	jwb [2][maxRows * nParam]float64
	nb  [2]linalg.Normal
	cur int

	// robust stage
	w   [maxRows]float64 // w-test per active row
	den [maxRows]float64 // w-test variance factor (redundancy) per row

	// RAIM statistics at the GLS solution (testGLS) and the saved Huber
	// M-estimate
	tChi2      float64
	tW, tDen   [maxRows]float64
	tValid     bool
	haveRobust bool
	robP       [nParam]float64
	robF       geo.Frame
	robConv    bool
	robHW      [maxRows]float64
	chi2       float64

	// starts
	hyps  [12]hyp
	nh    int
	seeds [16]seed
	ns    int

	// grid / misc workspace
	grid []float64
	// gen counts changes of the active rows, the linearisation and the k=0
	// LM buffers; wt memoises the w-tests (solve.go wtests).
	gen uint64
	wt  wtMemo

	tmpA  [MaxStations * MaxStations]float64
	tmpB  [MaxStations * 4]float64
	tmpV  [MaxStations]float64
	tmpV2 [MaxStations]float64
	tau   [4]float64
	qrw   [4]float64
	idx   [MaxStations]int
	exs   [4]geo.Vec3
	spare hyp
}

type seed struct {
	x    geo.Vec3
	init InitMethod
}

type hyp struct {
	x     geo.Vec3
	frame geo.Frame
	p     [nParam]float64
	chi2  float64
	conv  bool
	ok    bool // plausible (height and range)
	vis   bool // inside the radio horizon of every active station
	init  InitMethod
}

// NewSolver returns a solver with the given options (zero fields take
// their defaults).
func NewSolver(opt Options) *Solver {
	s := &Solver{opt: opt.Defaults()}
	s.atm = s.opt.Atmosphere
	for d := 1; d <= maxRows; d++ {
		s.chi2T[d] = Chi2Quantile(s.opt.PFA, d)
	}
	s.chi2T[0] = math.Inf(1)
	n := int(2*s.opt.GridHalfM/s.opt.GridStepM) + 1
	s.grid = make([]float64, n*n)
	return s
}

// Options returns the effective options.
func (s *Solver) Options() Options { return s.opt }

// SetAtmosphere replaces the refraction model (e.g. after a k_atm update).
func (s *Solver) SetAtmosphere(a geo.Atmosphere) {
	s.atm = a.Normalize()
	s.opt.Atmosphere = s.atm
}

// ---------------------------------------------------------------------
// problem setup

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func finiteVec(v geo.Vec3) bool { return finite(v.X) && finite(v.Y) && finite(v.Z) }

func (s *Solver) setup(pr *Problem) error {
	m := len(pr.Stations)
	if m > MaxStations {
		return ErrTooMany
	}
	s.m = m
	s.hasAlt = pr.Alt.Valid
	if s.hasAlt {
		if !finite(pr.Alt.HAE) || !(pr.Alt.Sigma > 0) || !finite(pr.Alt.Sigma) {
			return ErrBadInput
		}
		s.altH, s.altS = pr.Alt.HAE, pr.Alt.Sigma
	}
	s.hasPrior = pr.Prior.Valid
	if s.hasPrior {
		if !finiteVec(pr.Prior.Pos) {
			return ErrBadInput
		}
		s.priorPos, s.priorCov = pr.Prior.Pos, pr.Prior.Cov
		linalg.Symmetrize(s.priorCov[:], 3)
		for i := 0; i < 9; i++ {
			if !finite(s.priorCov[i]) {
				return ErrBadInput
			}
		}
		if !(s.priorCov[0] > 0 && s.priorCov[4] > 0 && s.priorCov[8] > 0) {
			return ErrBadInput
		}
		f := geo.NewFrameECEF(s.priorPos)
		ce := f.CovToENU(s.priorCov)
		s.priorSH = math.Sqrt(math.Max(ce[0]+ce[4], 0))
	}
	nAlt := 0
	if s.hasAlt {
		nAlt = 1
	}
	s.fuse = s.hasPrior && (pr.Prior.Fuse || m+nAlt < 4)
	nPrior := 0
	if s.fuse {
		nPrior = 3
	}
	if m < 2 || m+nAlt+nPrior < nParam || (m == 2 && !s.fuse) {
		return ErrTooFew
	}

	// TOAs relative to their mean, in metres.
	var tm float64
	for i := range pr.Stations {
		st := &pr.Stations[i]
		if !finite(st.TOA) || !finiteVec(st.Pos) {
			return ErrBadInput
		}
		tm += st.TOA
	}
	tm /= float64(m)
	s.tref = tm
	var cen geo.Vec3
	for i := range pr.Stations {
		st := &pr.Stations[i]
		s.pos[i] = st.Pos
		s.z[i] = (st.TOA - tm) * c
		s.zc[i] = s.z[i]
		cen = cen.Add(st.Pos)
		s.excl[i] = false
	}
	s.centroid = cen.Scale(1 / float64(m))

	const MS = MaxStations
	c2 := c * c
	if pr.Cov != nil {
		if len(pr.Cov) < m*m {
			return ErrBadInput
		}
		for i := 0; i < m; i++ {
			for j := 0; j <= i; j++ {
				v := pr.Cov[i*m+j] * c2
				if !finite(v) {
					return ErrBadInput
				}
				s.sig[i*MS+j], s.sig[j*MS+i] = v, v
			}
			if !(s.sig[i*MS+i] > 0) {
				return ErrBadInput
			}
		}
	} else {
		for i := 0; i < m; i++ {
			sg := pr.Stations[i].Sigma
			if !(sg > 0) || !finite(sg) {
				return ErrBadInput
			}
			for j := 0; j < m; j++ {
				s.sig[i*MS+j] = 0
			}
			s.sig[i*MS+i] = sg * sg * c2
		}
	}
	for i := 0; i < m; i++ {
		s.sd[i] = math.Sqrt(s.sig[i*MS+i])
	}
	s.altOff = false
	s.linOK = false
	s.iters = 0
	s.huber = false
	s.tValid = false
	s.haveRobust = false
	s.gen++
	s.wt.valid = false
	return nil
}

// buildRows rebuilds the active row list from excl/altOff/fuse and factors
// the nominal covariance of the active rows. Huber weights are reset.
func (s *Solver) buildRows() error {
	n := 0
	s.nToa = 0
	for i := 0; i < s.m; i++ {
		if !s.excl[i] {
			s.rows[n] = i
			n++
			s.nToa++
		}
	}
	if s.hasAlt && !s.altOff {
		s.rows[n] = rowAlt
		n++
	}
	if s.fuse {
		for k := 0; k < 3; k++ {
			s.rows[n] = rowPrior + k
			n++
		}
	}
	s.nr = n
	s.gen++
	const MS = MaxStations
	a := s.covTmp[:n*n]
	for i := 0; i < n; i++ {
		ri := s.rows[i]
		for j := 0; j <= i; j++ {
			rj := s.rows[j]
			var v float64
			switch {
			case ri < MS && rj < MS:
				v = s.sig[ri*MS+rj]
			case ri == rowAlt && rj == rowAlt:
				v = s.altS * s.altS
			case ri >= rowPrior && rj >= rowPrior:
				v = s.priorCov[(ri-rowPrior)*3+(rj-rowPrior)]
			}
			a[i*n+j], a[j*n+i] = v, v
		}
		s.hw[i] = 1
	}
	s.huber = false
	l := s.lnom[:n*n]
	if _, err := linalg.CholeskyJitter(l, a, n, 1e-12, 6); err != nil {
		return err
	}
	copy(s.lw[:n*n], l)
	s.linvOK = false
	return nil
}

// applyHuber sets lw = S·lnom with S = diag(1/√hw): the covariance of row i
// is inflated by 1/hw_i while its correlations are kept.
func (s *Solver) applyHuber() {
	n := s.nr
	s.huber = false
	s.gen++
	for i := 0; i < n; i++ {
		f := 1 / math.Sqrt(s.hw[i])
		if s.hw[i] < 1 {
			s.huber = true
		}
		src := s.lnom[i*n : i*n+i+1]
		dst := s.lw[i*n : i*n+i+1]
		for j := range src {
			dst[j] = src[j] * f
		}
	}
}

// ---------------------------------------------------------------------
// measurement model

// linearize computes the refraction excess and its gradient at x for every
// station (also excluded ones, for residual reporting).
func (s *Solver) linearize(x geo.Vec3) {
	s.lin = x
	s.linOK = true
	s.linCoarse = false
	s.gen++
	for i := 0; i < s.m; i++ {
		if s.opt.NoRefraction {
			s.e0[i], s.ge[i] = 0, geo.Vec3{}
			continue
		}
		d := x.Sub(s.pos[i])
		dist := d.Norm()
		L, g := s.atm.RangeGrad(s.pos[i], x)
		if !(dist > 0) {
			s.e0[i], s.ge[i] = 0, geo.Vec3{}
			continue
		}
		s.e0[i] = L - dist
		s.ge[i] = g.Sub(d.Scale(1 / dist))
	}
}

// linearizeCoarse linearises the excess at x with the closed-form
// straight-path model (geo.Atmosphere.StraightExcess and its analytic
// gradient: two exponentials per station instead of the ~0.4 µs bent-ray
// quadrature of RangeGrad). It is used while LM travels from a seed to the
// solution: the exact model is linearised once the iterations have
// converged on the coarse one (relinAtSolution), so the result is the
// minimiser of the exact model; the coarse phase only saves the exact
// linearisations at the seed and on the way (H-track, §13.2 solver budget).
func (s *Solver) linearizeCoarse(x geo.Vec3) {
	s.lin = x
	s.linOK = true
	s.linCoarse = true
	s.gen++
	a := &s.atm
	h2 := geo.GeocentricHeight(x)
	r := x.Norm()
	var up geo.Vec3
	if r > 0 {
		up = x.Scale(1 / r)
	}
	for i := 0; i < s.m; i++ {
		d := x.Sub(s.pos[i])
		D := d.Norm()
		if !(D > 0) {
			s.e0[i], s.ge[i] = 0, geo.Vec3{}
			continue
		}
		h1 := geo.GeocentricHeight(s.pos[i])
		var e, dEdD, dEdh float64
		if dh := h2 - h1; math.Abs(dh) < 1 {
			nm := a.N((h1 + h2) / 2)
			e = 1e-6 * D * nm
			dEdD = 1e-6 * nm
			dEdh = -0.5 * e / a.H
		} else {
			k := 1e-6 * a.K * a.N0 * a.H
			e1, e2 := math.Exp(-h1/a.H), math.Exp(-h2/a.H)
			f := (e1 - e2) / dh
			df := (e2/a.H - f) / dh // d f/d h2
			e = k * D * f
			dEdD = k * f
			dEdh = k * D * df
		}
		s.e0[i] = e
		s.ge[i] = d.Scale(dEdD / D).Add(up.Scale(dEdh))
	}
}

// exactLin replaces a coarse linearisation by the exact one at the current
// estimate. It reports whether it did.
func (s *Solver) exactLin() bool {
	if !s.linCoarse {
		return false
	}
	s.linearize(s.position(&s.p))
	return true
}

// rangeTo returns the modelled optical range from station i to x and its
// ECEF gradient.
func (s *Solver) rangeTo(i int, x geo.Vec3) (float64, geo.Vec3) {
	d := x.Sub(s.pos[i])
	rho := d.Norm()
	var u geo.Vec3
	if rho > 0 {
		u = d.Scale(1 / rho)
	}
	if s.opt.NoRefraction {
		return rho, u
	}
	rho += s.e0[i] + s.ge[i].Dot(x.Sub(s.lin))
	return rho, u.Add(s.ge[i])
}

// position returns the ECEF position of a parameter vector.
func (s *Solver) position(p *[nParam]float64) geo.Vec3 {
	return s.frame.FromENU(geo.Vec3{X: p[0], Y: p[1], Z: p[2]})
}

// eval fills the residuals (model − measurement, m) and the Jacobian
// (nr×4, d/d(e, n, u, b)) of the active rows at p.
func (s *Solver) eval(p *[nParam]float64, r, J []float64) {
	x := s.position(p)
	for k := 0; k < s.nr; k++ {
		code := s.rows[k]
		jr := J[k*nParam : k*nParam+nParam]
		switch {
		case code < MaxStations:
			rho, g := s.rangeTo(code, x)
			r[k] = rho + p[3] - s.z[code]
			e := s.frame.Rotate(g)
			jr[0], jr[1], jr[2], jr[3] = e.X, e.Y, e.Z, 1
		case code == rowAlt:
			llh := geo.FromECEF(x)
			r[k] = llh.H - s.altH
			e := s.frame.Rotate(geo.Up(llh.Lat, llh.Lon))
			jr[0], jr[1], jr[2], jr[3] = e.X, e.Y, e.Z, 0
		default:
			comp := code - rowPrior
			var unit geo.Vec3
			switch comp {
			case 0:
				r[k] = x.X - s.priorPos.X
				unit.X = 1
			case 1:
				r[k] = x.Y - s.priorPos.Y
				unit.Y = 1
			default:
				r[k] = x.Z - s.priorPos.Z
				unit.Z = 1
			}
			e := s.frame.Rotate(unit)
			jr[0], jr[1], jr[2], jr[3] = e.X, e.Y, e.Z, 0
		}
	}
}

// evalSet evaluates buffer set k at p, whitens it with L and accumulates
// the normal equations; it returns the cost ½‖L⁻¹r‖².
func (s *Solver) evalSet(k int, p *[nParam]float64, L []float64) float64 {
	n := s.nr
	if k == 0 {
		s.gen++ // overwrites the buffers the w-tests left
	}
	r, J := s.rb[k][:n], s.jb[k][:n*nParam]
	s.eval(p, r, J)
	rw, jw := s.rwb[k][:n], s.jwb[k][:n*nParam]
	copy(rw, r)
	copy(jw, J)
	linalg.Whiten(L, n, rw, jw, nParam)
	nrm := &s.nb[k]
	nrm.Reset(nParam)
	for i := 0; i < n; i++ {
		nrm.AddRow(jw[i*nParam:i*nParam+nParam], rw[i], 1)
	}
	return nrm.Cost
}

// initB sets p[3] to the weighted mean of z − ρ over the active TOA rows.
func (s *Solver) initB() {
	x := s.position(&s.p)
	var sw, sb float64
	for k := 0; k < s.nr; k++ {
		i := s.rows[k]
		if i >= MaxStations {
			continue
		}
		rho, _ := s.rangeTo(i, x)
		w := 1 / s.sig[i*MaxStations+i]
		sw += w
		sb += w * (s.z[i] - rho)
	}
	if sw > 0 {
		s.p[3] = sb / sw
	}
}

// startAt re-centres the frame at x, linearises the excess if needed and
// initialises b.
func (s *Solver) startAt(x geo.Vec3) {
	s.frame = geo.NewFrameECEF(x)
	s.p = [nParam]float64{}
	if !s.linOK || x.Dist(s.lin) > s.opt.RelinM {
		if s.coarseOK {
			s.linearizeCoarse(x)
		} else {
			s.linearize(x)
		}
	}
	s.initB()
}

// lm runs Levenberg-Marquardt from the current estimate with the
// Huber-weighted covariance lw. It returns the final cost and records
// convergence in s.conv.
//
// Damping: (N + λ·diag N)·dx = −g. The vertical position and the
// transmit-time term are strongly coupled for targets outside the station
// polygon (condition numbers of 1e8 are normal), so λ must be able to fall
// far below diag N: a good step (gain ratio > 0.75) divides λ by 10, a
// poor one (< 0.25) doubles it, a rejected one multiplies it by ν (ν
// doubling). Convergence is declared on the Newton decrement gᵀN⁻¹g (the
// cost reduction still available to an undamped Gauss-Newton step)
// falling below 1e-10, i.e. statistically nothing is left, or on a step
// below 0.1 mm.
func (s *Solver) lm(maxIter int) float64 {
	n := s.nr
	L := s.lw[:n*n]
	s.cur = 0
	cost := s.evalSet(0, &s.p, L)
	lambda := s.lmTau() * maxDiag(&s.nb[0])
	nu := 2.0
	s.conv = false
	var dx, gn [nParam]float64
	relins := 0
	for it := 0; it < maxIter; it++ {
		s.iters++
		nc := &s.nb[s.cur]
		// Newton decrement at the current point
		if dec, err := nc.Step(0, dmin, gn[:]); err == nil && dec < 1e-10 {
			if !s.relinAtSolution(&relins, &maxIter, it) {
				s.conv = true
				break
			}
			s.cur = 0
			cost = s.evalSet(0, &s.p, L)
			lambda = 1e-9 * maxDiag(&s.nb[0])
			continue
		}
		pred, err := nc.Step(lambda, dmin, dx[:])
		if err != nil {
			lambda = math.Max(lambda*nu, 1e-12)
			nu *= 2
			continue
		}
		var pt [nParam]float64
		for i := range pt {
			pt[i] = s.p[i] + dx[i]
		}
		t := 1 - s.cur
		newCost := s.evalSet(t, &pt, L)
		rho := linalg.GainRatio(cost, newCost, pred)
		if rho > 0 {
			s.p = pt
			s.cur = t
			cost = newCost
			switch {
			case rho > 0.75:
				lambda *= 0.1
			case rho < 0.25:
				lambda *= 2
			}
			nu = 2
			if lambda < 1e-15*maxDiag(&s.nb[t]) {
				lambda = 1e-15 * maxDiag(&s.nb[t])
			}
			step := math.Sqrt(dx[0]*dx[0] + dx[1]*dx[1] + dx[2]*dx[2] + dx[3]*dx[3])
			if !s.opt.NoRefraction && relins < 4 && s.position(&s.p).Dist(s.lin) > s.opt.RelinM {
				// The excess model changed: re-linearise and continue on
				// the new cost surface.
				relins++
				if s.linCoarse {
					s.linearizeCoarse(s.position(&s.p))
				} else {
					s.linearize(s.position(&s.p))
				}
				s.cur = 0
				cost = s.evalSet(0, &s.p, L)
				continue
			}
			if step < 1e-4 {
				if !s.relinAtSolution(&relins, &maxIter, it) {
					s.conv = true
					break
				}
				s.cur = 0
				cost = s.evalSet(0, &s.p, L)
				lambda = 1e-9 * maxDiag(&s.nb[0])
			}
		} else {
			lambda *= nu
			nu *= 2
			if lambda > 1e20*maxDiag(nc) || math.IsInf(lambda, 0) {
				break
			}
		}
	}
	if !s.conv {
		if dec, err := s.nb[s.cur].Step(0, dmin, gn[:]); err == nil && dec < 1e-8 {
			s.conv = true
		}
	}
	return cost
}

// relinFinalM: the excess is re-linearised at the solution when the
// estimate ended further than this from the linearisation point. The
// excess curvature is ~1e-7 /m, so 100 m leaves < 1 mm of model error.
const relinFinalM = 100

// relinAtSolution re-linearises the refraction excess at the current
// estimate when it has moved more than relinFinalM from the linearisation point
// (the second-order excess term, amplified by the DOP, would otherwise
// bias the solution). It reports whether it did, and extends the LM
// iteration budget: by 3 for a re-linearisation of the exact model, by a
// full MaxIter when the coarse phase ends (the exact optimum can lie
// hundreds of metres along the flat valley of far-out targets).
func (s *Solver) relinAtSolution(relins *int, maxIter *int, it int) bool {
	if s.exactLin() {
		*maxIter = max(*maxIter, it+1+s.opt.MaxIter)
		return true // coarse phase over: continue on the exact model
	}
	if s.opt.NoRefraction || *relins >= 4 {
		return false
	}
	x := s.position(&s.p)
	if x.Dist(s.lin) <= relinFinalM {
		return false
	}
	*relins++
	s.linearize(x)
	*maxIter += 3
	return true
}

func maxDiag(n *linalg.Normal) float64 {
	var m float64
	for i := 0; i < n.N; i++ {
		m = math.Max(m, n.H[i*n.N+i])
	}
	if m == 0 {
		m = 1
	}
	return m
}

// lmTau returns the initial LM damping factor: small after a closed-form
// or prior seed (Gauss-Newton is nearly right), larger after a coarse one.
func (s *Solver) lmTau() float64 {
	if s.coarse {
		return 1e-2
	}
	return 1e-6
}

// plausible checks height and range bounds of a solution.
func (s *Solver) plausible(x geo.Vec3) bool {
	if !finiteVec(x) {
		return false
	}
	h := geo.GeocentricHeight(x)
	if h < s.opt.MinHAE-100 || h > s.opt.MaxHAE+100 {
		return false
	}
	near := math.Inf(1)
	for k := 0; k < s.nr; k++ {
		if i := s.rows[k]; i < MaxStations {
			near = math.Min(near, x.Dist(s.pos[i]))
		}
	}
	return near <= s.opt.MaxRangeM
}

// dof returns the degrees of freedom of the active rows.
func (s *Solver) dof() int { return s.nr - nParam }
