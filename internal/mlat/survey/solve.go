package survey

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
	"airview/internal/model"
)

var debugSolve = false

// cLight is the speed of light in vacuum (m/s).
const cLight = 299792458.0

// jumpS and maxDriftPPM: a change of the target's local-minus-network
// time between consecutive observations larger than jumpS (beyond the
// largest range difference, ~1.5 ms at 450 km) plus maxDriftPPM of the
// elapsed time starts a new clock segment.
const (
	jumpS       = 2e-3
	maxDriftPPM = 300
)

// maxStepM bounds one Gauss-Newton position step.
const maxStepM = 5000

// minSegObs is the minimum number of observations of a clock segment
// (fewer are dropped: their clock would absorb them).
const minSegObs = 10

// Input is one survey problem.
type Input struct {
	Obs []Obs
	// Seed is the centre of the seed grid (DESIGN §10.2: the declared
	// position, Locate Me v2, or the centroid of the co-observing peers);
	// SearchRadiusM its radius (0 = Config.SearchRadiusM).
	Seed          geo.Vec3
	SearchRadiusM float64
	// HeightHAE / HeightSigmaM is the height prior (DEM + AGL, σ 20 m).
	// HeightSigmaM 0 = free height (a weak 2 km prior only keeps the
	// problem defined).
	HeightHAE    float64
	HeightSigmaM float64
	// Atm is the propagation model (the engine's, so both ends use the
	// same refraction).
	Atm geo.Atmosphere
}

// Solution is a survey result. Positions are exact (owner/admin data:
// never public, never logged).
type Solution struct {
	ECEF geo.Vec3
	LLH  geo.LLH
	// CovENU is the final 3×3 east-north-up covariance (m²): the larger of
	// the formal and the jackknife covariance plus the systematic floor.
	// FormalENU and JackknifeENU are the two ingredients.
	CovENU       [9]float64
	FormalENU    [9]float64
	JackknifeENU [9]float64
	// IndepENU is the formal covariance without the per-aircraft
	// correlation (a lower bound); JackknifeGroups the replicates used.
	IndepENU        [9]float64
	JackknifeGroups int
	// CommonHeightSens is the position change (ENU, m) per metre of a
	// height error common to all reference aircraft, CommonHeightSigmaM the
	// σ of that error assumed (the aircraft's mean vertical σ); their
	// product is part of CovENU.
	CommonHeightSens   [3]float64
	CommonHeightSigmaM float64
	// SigmaHM is the 1σ semi-major axis of the horizontal error ellipse,
	// SigmaVM the vertical 1σ; Ellipse is the 1σ ellipse (scale by 2.4477
	// for 95 %).
	SigmaHM float64
	SigmaVM float64
	Ellipse linalg.Ellipse

	// Counts: observations given, used (weight > 0), rejected as outliers,
	// below the elevation mask, distinct aircraft used, clock segments and
	// detected clock breaks.
	N, Used, Rejected, Masked, Aircraft, Segments, Breaks int
	// JitterNs is the target's timing noise learned from the residuals.
	JitterNs float64
	// AzSpreadDeg is 360° minus the largest empty azimuth sector of the
	// used aircraft (10° bins); ElevMinDeg/ElevMaxDeg their elevation range.
	AzSpreadDeg            float64
	ElevMinDeg, ElevMaxDeg float64
	// DurationS is the time span of the observations used; Anchored the
	// fraction of them in UTC (anchored) time domains.
	DurationS float64
	Anchored  float64
	// HeightResidM is the height minus the prior (0 without a prior).
	HeightResidM float64
	// HalvesM is the horizontal distance between the solutions of the
	// first and second half of the observations, HalvesChi2 its
	// Mahalanobis² (2 dof); HalvesOK = false when there was not enough
	// data for the test.
	HalvesM, HalvesChi2 float64
	HalvesOK            bool
	// OnEdge: the seed grid's best point was on the search boundary.
	OnEdge bool
	// Iterations of the main Gauss-Newton loop.
	Iterations int
}

// Survey-solver errors.
var (
	ErrTooFewObs = errors.New("survey: too few observations")
	ErrNoSolve   = errors.New("survey: no solution (singular geometry)")
)

type segment struct {
	k0, nk  int     // first global knot, knot count
	knotS   float64 // knot spacing (s)
	curvM   float64 // second-difference prior 1σ (m)
	jitM    float64 // prior jitter (m)
	anch    bool
	gps     bool
	tauBase int64
}

// prob is the prepared problem: per-observation geometry and clock
// indexing plus the iteration state. Not safe for concurrent use.
type prob struct {
	cfg *Config
	atm geo.Atmosphere
	in  []Obs

	n     int
	t     []float64 // s since the earliest τ
	rho   []float64 // c·(s_U − τ), detrended per segment (m)
	knot  []int32   // global index of the knot left of the observation
	alpha []float64 // position between the knots
	seg   []int32
	segs  []segment
	K     int
	brk   []bool // brk[i]: the second-difference row (i, i+1, i+2) is dropped

	// iteration state
	b    []float64  // clock values at the knots (m)
	exc  []float64  // refraction excess at the last refresh (m)
	res  []float64  // residual ρ − R − b(t)
	eu   []geo.Vec3 // unit vector target → aircraft (ECEF)
	sinE []float64  // sine of the elevation at the target
	vo   []float64  // variance from τ and the aircraft position (m²)
	w    []float64  // weight (0 = not used)
	act  []bool     // active subset (masks, jackknife, halves)
	cf   []float64  // per-aircraft correlation weight factor (estimateCorr)
	sigU float64    // target jitter 1σ (m)
	rho0 float64    // estimated per-aircraft residual correlation

	// normal equations
	hcc  band
	fac  band
	hpc  [][3]float64
	gc   []float64
	z    []float64
	zc   [3][]float64
	tmpZ []float64
}

func (p *prob) prepare(in []Obs) error {
	p.in = in
	type acc struct {
		minTau int64
		maxTau int64
		ticks0 uint64
		n      int
		cls    model.ClockClass
		anch   bool
	}
	// Segments: one clock per receiver epoch, peer time domain and
	// anchoring (a domain that gains its first anchor changes its time
	// scale), split further where the target's local time jumps against
	// network time by more than any range difference can explain (a day
	// wrap of a GPS counter, a gauge change of the peers' time).
	type gkey struct {
		epoch, dom uint32
		anch       bool
	}
	groups := map[gkey][]int{}
	var gorder []gkey
	for i := range in {
		o := &in[i]
		if o.Class.NominalHz() == 0 {
			continue
		}
		k := gkey{o.Epoch, o.Domain, o.Anchored}
		if _, ok := groups[k]; !ok {
			gorder = append(gorder, k)
		}
		groups[k] = append(groups[k], i)
	}
	segKey := make([]int, len(in))
	for i := range segKey {
		segKey[i] = -1
	}
	var accs []acc
	minTau := int64(math.MaxInt64)
	for _, k := range gorder {
		idx := groups[k]
		slices.SortStableFunc(idx, func(a, b int) int {
			switch {
			case in[a].TauNs < in[b].TauNs:
				return -1
			case in[a].TauNs > in[b].TauNs:
				return 1
			}
			return 0
		})
		cur := -1
		var prevD float64
		var prevTau int64
		for _, i := range idx {
			o := &in[i]
			if cur >= 0 {
				a := &accs[cur]
				d := float64(int64(o.Ticks-a.ticks0))/o.Class.NominalHz() - float64(o.TauNs-a.minTau)/1e9
				dt := float64(o.TauNs-prevTau) / 1e9
				if math.Abs(d-prevD) > jumpS+maxDriftPPM*1e-6*dt {
					cur = -1
				} else {
					prevD = d
				}
			}
			if cur < 0 {
				cur = len(accs)
				accs = append(accs, acc{minTau: o.TauNs, maxTau: o.TauNs, ticks0: o.Ticks, cls: o.Class, anch: o.Anchored})
				prevD = 0
			}
			a := &accs[cur]
			a.maxTau = max(a.maxTau, o.TauNs)
			a.n++
			prevTau = o.TauNs
			segKey[i] = cur
			minTau = min(minTau, o.TauNs)
		}
	}
	p.segs = p.segs[:0]
	segOf := make([]int, len(accs))
	K := 0
	for j := range accs {
		a := &accs[j]
		if a.n < minSegObs {
			segOf[j] = -1
			continue
		}
		cm := ClockModelFor(a.cls, !a.anch)
		span := float64(a.maxTau-a.minTau) / 1e9
		nk := int(math.Floor(span/cm.KnotS)) + 2
		segOf[j] = len(p.segs)
		p.segs = append(p.segs, segment{k0: K, nk: nk, knotS: cm.KnotS, curvM: cm.CurvNs * 1e-9 * cLight,
			jitM: cm.JitterNs * 1e-9 * cLight, anch: a.anch, gps: a.cls.GPS(), tauBase: a.minTau})
		K += nk
	}
	p.K = K
	p.n = 0
	p.t, p.rho, p.knot, p.alpha, p.seg = p.t[:0], p.rho[:0], p.knot[:0], p.alpha[:0], p.seg[:0]
	kept := make([]int, 0, len(in))
	for i := range in {
		o := &in[i]
		if segKey[i] < 0 {
			continue
		}
		j := segOf[segKey[i]]
		if j < 0 {
			continue
		}
		s := &p.segs[j]
		a := &accs[segKey[i]]
		ts := float64(o.TauNs-s.tauBase) / 1e9
		y := float64(int64(o.Ticks-a.ticks0)) / o.Class.NominalHz()
		p.t = append(p.t, float64(o.TauNs-minTau)/1e9)
		p.rho = append(p.rho, cLight*(y-ts))
		x := ts / s.knotS
		ki := min(int(math.Floor(x)), s.nk-2)
		p.knot = append(p.knot, int32(s.k0+ki))
		p.alpha = append(p.alpha, x-float64(ki))
		p.seg = append(p.seg, int32(j))
		kept = append(kept, i)
	}
	if len(kept) < 2*minSegObs {
		return ErrTooFewObs
	}
	// keep only the observations with a segment (re-slice the input copy)
	obs := make([]Obs, len(kept))
	for k, i := range kept {
		obs[k] = in[i]
	}
	p.in = obs
	p.n = len(obs)
	// detrend each segment (keeps the clock values small; the clock model
	// is unchanged by a linear term)
	for j := range p.segs {
		var sw, st, sr, stt, str float64
		for k := 0; k < p.n; k++ {
			if int(p.seg[k]) != j {
				continue
			}
			tt := p.t[k]
			sw++
			st += tt
			sr += p.rho[k]
			stt += tt * tt
			str += tt * p.rho[k]
		}
		den := sw*stt - st*st
		a, b := sr/sw, 0.0
		if den > 1e-9*sw*sw {
			b = (sw*str - st*sr) / den
			a = (sr - b*st) / sw
		}
		for k := 0; k < p.n; k++ {
			if int(p.seg[k]) == j {
				p.rho[k] -= a + b*p.t[k]
			}
		}
	}
	grow := func(s []float64) []float64 {
		if cap(s) < p.n {
			return make([]float64, p.n)
		}
		return s[:p.n]
	}
	p.exc, p.res, p.sinE, p.vo, p.w = grow(p.exc), grow(p.res), grow(p.sinE), grow(p.vo), grow(p.w)
	if cap(p.eu) < p.n {
		p.eu = make([]geo.Vec3, p.n)
	}
	p.eu = p.eu[:p.n]
	if cap(p.act) < p.n {
		p.act = make([]bool, p.n)
	}
	p.act = p.act[:p.n]
	for k := range p.act {
		p.act[k] = true
	}
	p.cf = make([]float64, p.n)
	for k := range p.cf {
		p.cf[k] = 1
	}
	p.b = make([]float64, p.K)
	p.brk = make([]bool, p.K)
	p.hpc = make([][3]float64, p.K)
	p.gc = make([]float64, p.K)
	p.z = make([]float64, p.K)
	for i := range p.zc {
		p.zc[i] = make([]float64, p.K)
	}
	p.sigU = p.segs[0].jitM
	return nil
}

// clockAt is the clock value at observation k.
func (p *prob) clockAt(k int) float64 {
	i := p.knot[k]
	a := p.alpha[k]
	return (1-a)*p.b[i] + a*p.b[i+1]
}

// linearize evaluates the model at u: ranges (with refraction; refresh
// recomputes the excess, otherwise the cached one is reused), unit vectors,
// elevations, residuals and the non-jitter variance.
func (p *prob) linearize(u geo.Vec3, refresh bool) {
	fu := geo.NewFrameECEF(u)
	up := fu.Up()
	for k := 0; k < p.n; k++ {
		o := &p.in[k]
		d := o.P.Sub(u)
		g := d.Norm()
		if refresh {
			p.exc[k] = p.atm.Range(u, o.P) - g
		}
		e := d.Scale(1 / g)
		p.eu[k] = e
		p.sinE[k] = e.Dot(up)
		p.res[k] = p.rho[k] - g - p.exc[k] - p.clockAt(k)
		p.vo[k] = o.VarTauM2 + o.quad(e)
	}
}

// weights sets the IRLS weights: Huber between HuberK and RejectK σ,
// rejection beyond (robust = false: plain 1/variance), the elevation mask
// and the active subset.
func (p *prob) weights(robust bool) (used, rejected, masked int) {
	sm := math.Sin(p.cfg.MaskDeg * math.Pi / 180)
	s2 := p.sigU * p.sigU
	hk, rk := p.cfg.HuberK, p.cfg.RejectK
	for k := 0; k < p.n; k++ {
		p.w[k] = 0
		if !p.act[k] {
			continue
		}
		if p.sinE[k] < sm {
			masked++
			continue
		}
		v := s2 + p.vo[k]
		w := p.cf[k] / v
		if robust {
			z := math.Abs(p.res[k]) / math.Sqrt(v)
			switch {
			case z > rk:
				rejected++
				continue
			case z > hk:
				w *= hk / z
			}
		}
		p.w[k] = w
		used++
	}
	return used, rejected, masked
}

// heightPrior is the soft height constraint.
type heightPrior struct {
	h0, sigma float64
}

// solveStep builds the normal equations of the current linearisation and
// returns the position step (ENU of fr) and its formal covariance (pos =
// false: clock only). The clock step is left in p.z.
func (p *prob) solveStep(u geo.Vec3, fr *geo.Frame, pos bool, hp heightPrior) (dp [3]float64, cov sym3, err error) {
	K := p.K
	p.hcc.reset(K)
	clear(p.gc)
	clear(p.hpc)
	var hpp sym3
	var gp [3]float64
	rot := fr.Rotation()
	for k := 0; k < p.n; k++ {
		w := p.w[k]
		if w == 0 {
			continue
		}
		i := int(p.knot[k])
		a1 := p.alpha[k]
		a0 := 1 - a1
		r := p.res[k]
		p.hcc.add(i, i, w*a0*a0)
		p.hcc.add(i+1, i+1, w*a1*a1)
		p.hcc.add(i+1, i, w*a0*a1)
		p.gc[i] += w * a0 * r
		p.gc[i+1] += w * a1 * r
		if pos {
			e := p.eu[k]
			// J_p = ∂(R + b)/∂u in ENU = −R·e
			j := [3]float64{
				-(rot[0]*e.X + rot[1]*e.Y + rot[2]*e.Z),
				-(rot[3]*e.X + rot[4]*e.Y + rot[5]*e.Z),
				-(rot[6]*e.X + rot[7]*e.Y + rot[8]*e.Z),
			}
			for x := 0; x < 3; x++ {
				gp[x] += w * j[x] * r
				for y := 0; y < 3; y++ {
					hpp[3*x+y] += w * j[x] * j[y]
				}
				p.hpc[i][x] += w * j[x] * a0
				p.hpc[i+1][x] += w * j[x] * a1
			}
		}
	}
	// second-difference priors and a negligible ridge
	for si := range p.segs {
		s := &p.segs[si]
		wp := 1 / (s.curvM * s.curvM)
		for i := s.k0; i+2 < s.k0+s.nk; i++ {
			if p.brk[i] {
				continue
			}
			d := p.b[i] - 2*p.b[i+1] + p.b[i+2]
			p.hcc.add(i, i, wp)
			p.hcc.add(i+1, i+1, 4*wp)
			p.hcc.add(i+2, i+2, wp)
			p.hcc.add(i+1, i, -2*wp)
			p.hcc.add(i+2, i+1, -2*wp)
			p.hcc.add(i+2, i, wp)
			p.gc[i] -= wp * d
			p.gc[i+1] += 2 * wp * d
			p.gc[i+2] -= wp * d
		}
	}
	for i := 0; i < K; i++ {
		p.hcc.add(i, i, 1e-14)
	}
	p.fac.copyFrom(&p.hcc)
	if err := p.fac.cholesky(); err != nil {
		return dp, cov, err
	}
	copy(p.z, p.gc)
	p.fac.solve(p.z)
	if !pos {
		return dp, cov, nil
	}
	if hp.sigma > 0 {
		wh := 1 / (hp.sigma * hp.sigma)
		hpp[8] += wh
		gp[2] += wh * (hp.h0 - geo.FromECEF(u).H)
	}
	// Schur complement onto the position
	for x := 0; x < 3; x++ {
		zc := p.zc[x]
		for i := 0; i < K; i++ {
			zc[i] = p.hpc[i][x]
		}
		p.fac.solve(zc)
	}
	S := hpp
	rhs := gp
	for x := 0; x < 3; x++ {
		for i := 0; i < K; i++ {
			rhs[x] -= p.hpc[i][x] * p.z[i]
		}
		for y := 0; y < 3; y++ {
			s := 0.0
			zc := p.zc[y]
			for i := 0; i < K; i++ {
				s += p.hpc[i][x] * zc[i]
			}
			S[3*x+y] -= s
		}
	}
	// symmetrise
	S[1], S[3] = (S[1]+S[3])/2, (S[1]+S[3])/2
	S[2], S[6] = (S[2]+S[6])/2, (S[2]+S[6])/2
	S[5], S[7] = (S[5]+S[7])/2, (S[5]+S[7])/2
	cov, err = inv3(S)
	if err != nil {
		return dp, cov, err
	}
	dp = cov.mulVec(rhs)
	for i := 0; i < K; i++ {
		p.z[i] -= p.zc[0][i]*dp[0] + p.zc[1][i]*dp[1] + p.zc[2][i]*dp[2]
	}
	return dp, cov, nil
}

// applyClock adds the clock step in p.z.
func (p *prob) applyClock() {
	for i := range p.b {
		p.b[i] += p.z[i]
	}
}

// estimateJitter sets p.sigU so that the median of res²/(σ_U² + v_o) over
// the unmasked active observations is the χ²₁ median (0.4549), corrected
// for the parameters' leverage. It returns false when there is too little
// data.
func (p *prob) estimateJitter() bool {
	sm := math.Sin(p.cfg.MaskDeg * math.Pi / 180)
	stride := max(1, p.n/4000)
	var r2, vo []float64
	for k := 0; k < p.n; k += stride {
		if !p.act[k] || p.sinE[k] < sm {
			continue
		}
		r2 = append(r2, p.res[k]*p.res[k])
		vo = append(vo, p.vo[k])
	}
	m := len(r2)
	if m < 20 {
		return false
	}
	params := float64(p.K + 3)
	lev := math.Max(0.5, 1-params/float64(p.used()+1))
	target := 0.4549 * lev
	q := make([]float64, m)
	med := func(s float64) float64 {
		for i := range q {
			q[i] = r2[i] / (s*s + vo[i])
		}
		slices.Sort(q)
		return q[m/2]
	}
	lo, hi := math.Log(0.05), math.Log(5000.0)
	for it := 0; it < 40; it++ {
		mid := (lo + hi) / 2
		if med(math.Exp(mid)) > target {
			lo = mid
		} else {
			hi = mid
		}
	}
	p.sigU = math.Exp((lo + hi) / 2)
	return true
}

// estimateCorr estimates the correlation rho of the normalised residuals
// of one aircraft (an equicorrelation model: position latency and NACp
// biases are shared by all frames of an aircraft) by the method of moments
// on the aircraft's sum of normalised residuals, E[S_a^2] = n_a + n_a(n_a-1)rho,
// and sets each observation's weight factor to 1/(1 + (n_a - 1)rho), the
// GLS weight of the mean of n_a equicorrelated observations. Without it
// the formal covariance treats thousands of frames of a few dozen
// aircraft as independent and lets them overrule the height prior.
func (p *prob) estimateCorr() {
	sm := math.Sin(p.cfg.MaskDeg * math.Pi / 180)
	s2 := p.sigU * p.sigU
	type agg struct {
		n   int
		sum float64
	}
	m := map[uint32]*agg{}
	for k := 0; k < p.n; k++ {
		if !p.act[k] || p.sinE[k] < sm {
			continue
		}
		z := p.res[k] / math.Sqrt(s2+p.vo[k])
		if math.Abs(z) > p.cfg.RejectK {
			continue
		}
		a := m[p.in[k].Key]
		if a == nil {
			a = &agg{}
			m[p.in[k].Key] = a
		}
		a.n++
		a.sum += z
	}
	var num, den float64
	for _, a := range m {
		n := float64(a.n)
		num += a.sum*a.sum - n
		den += n * (n - 1)
	}
	rho := 0.0
	if den > 0 && len(m) >= 3 {
		rho = math.Min(math.Max(num/den, 0), 0.95)
	}
	p.rho0 = rho
	for k := 0; k < p.n; k++ {
		n := 1.0
		if a := m[p.in[k].Key]; a != nil {
			n = float64(a.n)
		}
		p.cf[k] = 1 / (1 + (n-1)*rho)
	}
}

func (p *prob) used() int {
	n := 0
	for _, w := range p.w {
		if w > 0 {
			n++
		}
	}
	return n
}

// fitClockOnly fits the clock at fixed u with the current weights.
func (p *prob) fitClockOnly(u geo.Vec3, fr *geo.Frame) error {
	_, _, err := p.solveStep(u, fr, false, heightPrior{})
	if err == nil {
		p.applyClock()
	}
	return err
}

// Solve runs the reverse-MLAT survey (DESIGN §10.2): seed grid, robust
// Gauss-Newton with the clock knots, the jackknife covariance, the split
// halves and the summary statistics.
func Solve(in Input, cfg Config) (Solution, error) {
	cfg.Normalize()
	p := &prob{cfg: &cfg, atm: in.Atm}
	if p.atm == (geo.Atmosphere{}) {
		p.atm = geo.DefaultAtmosphere()
	}
	var sol Solution
	sol.N = len(in.Obs)
	if err := p.prepare(in.Obs); err != nil {
		return sol, err
	}
	hp := heightPrior{h0: in.HeightHAE, sigma: in.HeightSigmaM}
	if hp.sigma <= 0 {
		hp.sigma = 2000
	}
	radius := in.SearchRadiusM
	if radius <= 0 {
		radius = cfg.SearchRadiusM
	}
	u, edge := p.grid(in.Seed, radius, in.HeightHAE)
	sol.OnEdge = edge
	fr := geo.NewFrameECEF(u)
	// reset the clock (the grid fitted it on a subsample)
	clear(p.b)
	p.linearize(u, true)
	p.weights(false)
	if err := p.fitClockOnly(u, &fr); err != nil {
		return sol, ErrNoSolve
	}
	// Rounds of iteratively reweighted least squares: each round re-learns
	// the target's jitter, the per-aircraft error correlation and the
	// robust weights at the current solution, then runs Gauss-Newton with
	// those weights held (holding them inside a round avoids the limit
	// cycles of reweighting at every step).
	// Every round is robust, the first one included: the jitter learned at
	// the grid point (within ~50 m) is inflated but robust, and a gross
	// error (a clock step, a bad reference) never steers an unweighted step.
	var cov sym3
	var err error
	for round := 0; round < 12; round++ {
		r0 := u
		p.linearize(u, true)
		p.estimateJitter()
		if round > 0 {
			p.estimateCorr()
		}
		sol.Used, sol.Rejected, sol.Masked = p.weights(true)
		for it := 0; it < 10; it++ {
			sol.Iterations++
			if it > 0 {
				p.linearize(u, true)
			}
			var dp [3]float64
			dp, cov, err = p.solveStep(u, &fr, true, hp)
			if err != nil {
				return sol, ErrNoSolve
			}
			step := math.Sqrt(dp[0]*dp[0] + dp[1]*dp[1] + dp[2]*dp[2])
			if step > maxStepM {
				// a trust region: the linearisation holds over a few km
				f := maxStepM / step
				for x := range dp {
					dp[x] *= f
				}
				for i := range p.z {
					p.z[i] *= f
				}
				step = maxStepM
			}
			p.applyClock()
			u = fr.FromENU(fr.ToENU(u).Add(geo.Vec3{X: dp[0], Y: dp[1], Z: dp[2]}))
			if debugSolve {
				fmt.Printf("round %d it %d step %.3f dp %.3f %.3f %.3f sigU %.2f\n", round, it, step, dp[0], dp[1], dp[2], p.sigU)
			}
			if step < 0.005 {
				break
			}
		}
		if round >= 2 && u.Sub(r0).Norm() < 0.02 {
			// clock breaks: a silent step in the target clock shows as one
			// outsized second difference; drop its prior rows and go on
			if sol.Breaks < 10 && p.detectBreak() {
				sol.Breaks++
				continue
			}
			break
		}
	}
	// final state: the last round's weights at the converged position
	p.linearize(u, true)
	if sol.Used < 2*minSegObs {
		return sol, ErrTooFewObs
	}
	_, cov, err = p.solveStep(u, &fr, true, hp)
	if err != nil {
		return sol, ErrNoSolve
	}
	sol.ECEF = u
	sol.LLH = geo.FromECEF(u)
	sol.JitterNs = p.sigU / cLight * 1e9
	sol.Segments = len(p.segs)
	if in.HeightSigmaM > 0 {
		sol.HeightResidM = sol.LLH.H - in.HeightHAE
	}
	// rotate the formal covariance into the ENU frame at u (fr is the seed
	// frame; the two differ by the seed offset's angle, negligible for the
	// covariance but done properly anyway)
	fu := geo.NewFrameECEF(u)
	formal := rotateCov(cov, &fr, &fu)
	sol.FormalENU = formal
	if debugSolve {
		fmt.Printf("formal %.2f %.2f %.2f\n", math.Sqrt(formal[0]), math.Sqrt(formal[4]), math.Sqrt(formal[8]))
	}

	w := append([]float64(nil), p.w...)
	b := append([]float64(nil), p.b...)
	// the formal covariance as if every observation were independent: a
	// lower bound no resampling may go below
	for k := range p.w {
		if p.cf[k] > 0 {
			p.w[k] = w[k] / p.cf[k]
		}
	}
	indep := formal
	if _, ci, err := p.solveStep(u, &fr, true, hp); err == nil {
		indep = rotateCov(ci, &fr, &fu)
	}
	copy(p.w, w)
	sol.IndepENU = indep
	jk, groups := p.jackknife(u, &fr, &fu, hp, w, b)
	sol.JackknifeENU = jk
	sol.JackknifeGroups = groups
	final := combineCov(formal, indep, jk, groups)
	// a height error shared by the reference aircraft (the GNSS−baro
	// field) is common to every jackknife replicate: add it as a consider
	// term
	if sv, vc, ok := p.commonHeight(u, &fr, hp, b); ok {
		e := fr.Unrotate(geo.Vec3{X: sv[0], Y: sv[1], Z: sv[2]})
		g := fu.Rotate(e)
		v := [3]float64{g.X, g.Y, g.Z}
		for x := 0; x < 3; x++ {
			for y := 0; y < 3; y++ {
				final[3*x+y] += v[x] * v[y] * vc
			}
		}
		sol.CommonHeightSens = v
		sol.CommonHeightSigmaM = math.Sqrt(vc)
	}
	copy(p.b, b)
	copy(p.w, w)
	p.linearize(u, false)
	sol.Anchored = p.anchoredFrac()
	floor := cfg.SysFloorAnchoredM*sol.Anchored + cfg.SysFloorRelativeM*(1-sol.Anchored)
	final[0] += floor * floor
	final[4] += floor * floor
	final[8] += floor * floor
	sol.CovENU = final
	sol.Ellipse = linalg.ErrorEllipse(final[0], final[1], final[4])
	sol.SigmaHM = sol.Ellipse.SemiMajor
	sol.SigmaVM = math.Sqrt(math.Max(final[8], 0))

	// halves, on the restored final state
	copy(p.w, w)
	copy(p.b, b)
	infl := 1.0
	if formal[0]+formal[4] > 0 {
		infl = math.Max(1, (final[0]+final[4])/(formal[0]+formal[4]))
	}
	p.halves(u, &fr, &fu, hp, w, b, infl, &sol)
	copy(p.w, w)
	copy(p.b, b)
	p.linearize(u, false)
	p.summary(u, &fu, &sol)
	return sol, nil
}

// detectBreak finds the largest normalised second difference of the clock
// and, when it exceeds BreakZ, drops the prior rows around it.
func (p *prob) detectBreak() bool {
	best, bi := 0.0, -1
	for si := range p.segs {
		s := &p.segs[si]
		for i := s.k0; i+2 < s.k0+s.nk; i++ {
			if p.brk[i] {
				continue
			}
			z := math.Abs(p.b[i]-2*p.b[i+1]+p.b[i+2]) / s.curvM
			if z > best {
				best, bi = z, i
			}
		}
	}
	if bi < 0 || best <= p.cfg.BreakZ {
		return false
	}
	// a step inside interval (j, j+1) bends the clock at both ends: free
	// the rows centred on both knots
	p.brk[bi] = true
	if bi+1 < len(p.brk) {
		p.brk[bi+1] = true
	}
	if bi > 0 {
		p.brk[bi-1] = true
	}
	return true
}

func (p *prob) anchoredFrac() float64 {
	n, a := 0, 0
	for k := 0; k < p.n; k++ {
		if p.w[k] > 0 {
			n++
			if p.in[k].Anchored {
				a++
			}
		}
	}
	if n == 0 {
		return 0
	}
	return float64(a) / float64(n)
}

// rotateCov moves a covariance from the ENU axes of frame a to those of b.
func rotateCov(c sym3, a, b *geo.Frame) [9]float64 {
	ecef := a.CovToECEF([9]float64(c))
	return b.CovToENU(ecef)
}

// groupOf assigns an aircraft to one of g jackknife groups.
func groupOf(key uint32, g int) int {
	h := uint64(key) * 0x9E3779B97F4A7C15
	return int((h >> 32) % uint64(g))
}

// jackknife returns the delete-one-group (by aircraft) jackknife
// covariance of the position (ENU at u): it carries the errors that are
// correlated within an aircraft (position latency, NACp bias), which the
// formal covariance treats as independent.
func (p *prob) jackknife(u geo.Vec3, fr, fu *geo.Frame, hp heightPrior, w, b []float64) ([9]float64, int) {
	keys := map[uint32]struct{}{}
	for k := 0; k < p.n; k++ {
		if w[k] > 0 {
			keys[p.in[k].Key] = struct{}{}
		}
	}
	G := min(p.cfg.JackknifeGroups, max(10, len(keys)/3))
	var est []geo.Vec3
	for g := 0; g < G; g++ {
		copy(p.b, b)
		n := 0
		for k := 0; k < p.n; k++ {
			p.act[k] = groupOf(p.in[k].Key, G) != g
		}
		for k := 0; k < p.n; k++ {
			if p.act[k] {
				p.w[k] = w[k]
				if w[k] > 0 {
					n++
				}
			} else {
				p.w[k] = 0
			}
		}
		if n < 2*minSegObs {
			continue
		}
		ug, _, err := p.gnFixedW(u, fr, hp, 4)
		if err != nil {
			continue
		}
		est = append(est, fu.ToENU(ug))
		if debugSolve {
			fmt.Printf("jk %d n %d d %+v\n", g, n, fu.ToENU(ug))
		}
	}
	for k := range p.act {
		p.act[k] = true
	}
	var out [9]float64
	m := len(est)
	if m < 3 {
		return out, m
	}
	var mean geo.Vec3
	for _, e := range est {
		mean = mean.Add(e)
	}
	mean = mean.Scale(1 / float64(m))
	for _, e := range est {
		d := [3]float64{e.X - mean.X, e.Y - mean.Y, e.Z - mean.Z}
		for x := 0; x < 3; x++ {
			for y := 0; y < 3; y++ {
				out[3*x+y] += d[x] * d[y]
			}
		}
	}
	f := float64(m-1) / float64(m)
	for i := range out {
		out[i] *= f
	}
	return out, m
}

// commonHeight returns the sensitivity of the position (ENU of fr) to a
// height error δ common to every reference aircraft, and the variance of
// such an error: the mean vertical variance of the used aircraft
// positions (conservative: all of it treated as common). A common shift
// δ·up of every aircraft changes a pseudorange residual by
// (Ebar − e_U)·up·δ (the peers' emission time moves by Ebar·up·δ/c, the
// modelled range by e_U·up·δ); the estimator maps it linearly.
func (p *prob) commonHeight(u geo.Vec3, fr *geo.Frame, hp heightPrior, b []float64) ([3]float64, float64, bool) {
	res := append([]float64(nil), p.res...)
	defer copy(p.res, res)
	clear(p.b) // no prior gradient: a pure sensitivity
	var sw, svar float64
	for k := 0; k < p.n; k++ {
		o := &p.in[k]
		up := o.P.Unit()
		p.res[k] = o.Ebar.Sub(p.eu[k]).Dot(up)
		if p.w[k] > 0 {
			c := &o.CovP
			v := up.X*up.X*c[0] + up.Y*up.Y*c[3] + up.Z*up.Z*c[5] + 2*(up.X*up.Y*c[1]+up.X*up.Z*c[2]+up.Y*up.Z*c[4])
			sw += p.w[k]
			svar += p.w[k] * v
		}
	}
	if !(sw > 0) {
		return [3]float64{}, 0, false
	}
	hp.h0 = geo.FromECEF(u).H // keep the prior's information, not its pull
	dp, _, err := p.solveStep(u, fr, true, hp)
	copy(p.b, b)
	if err != nil {
		return [3]float64{}, 0, false
	}
	return dp, svar / sw, true
}

// minJackknifeGroups is the number of jackknife replicates from which the
// jackknife covariance is trusted on its own (above the independent-
// observation bound); with fewer the GLS formal covariance also bounds it.
const minJackknifeGroups = 8

// combineCov forms the reported covariance (ENU, before the systematic
// floor). The formal covariance with the per-aircraft equicorrelation
// weights (formal) is a model; the delete-one-aircraft-group jackknife (jk)
// measures the scatter that errors shared within an aircraft actually
// cause. With enough replicates the jackknife, inflated by the upper
// confidence factor of a variance estimated from m−1 degrees of freedom
// (1 + 1/√(2(m−1))), is used, bounded below by the covariance of
// independent observations (indep: no resampling can beat it); with too
// few replicates the larger of formal and jk is used. Horizontal and
// vertical blocks are compared separately.
func combineCov(formal, indep, jk [9]float64, m int) [9]float64 {
	pick := func(dst *[9]float64, src [9]float64, horiz bool) {
		if horiz {
			dst[0], dst[1], dst[3], dst[4] = src[0], src[1], src[3], src[4]
			dst[2], dst[5], dst[6], dst[7] = src[2], src[5], src[6], src[7]
		} else {
			dst[8] = src[8]
		}
	}
	if m < minJackknifeGroups {
		out := formal
		if jk[0]+jk[4] > out[0]+out[4] {
			pick(&out, jk, true)
		}
		if jk[8] > out[8] {
			out[8] = jk[8]
		}
		return out
	}
	f := 1 + 1/math.Sqrt(2*float64(m-1))
	f *= f
	var j [9]float64
	for i := range jk {
		j[i] = jk[i] * f
	}
	out := indep
	if j[0]+j[4] > out[0]+out[4] {
		pick(&out, j, true)
	}
	if j[8] > out[8] {
		out[8] = j[8]
	}
	return out
}

// gnFixedW is gn with the weights held (the jackknife and halves: the
// weights of the full solution, zeroed outside the subset).
func (p *prob) gnFixedW(u geo.Vec3, fr *geo.Frame, hp heightPrior, iters int) (geo.Vec3, sym3, error) {
	var cov sym3
	for it := 0; it < iters; it++ {
		p.linearize(u, false)
		dp, c, err := p.solveStep(u, fr, true, hp)
		if err != nil {
			return u, cov, err
		}
		cov = c
		p.applyClock()
		u = fr.FromENU(fr.ToENU(u).Add(geo.Vec3{X: dp[0], Y: dp[1], Z: dp[2]}))
		if math.Sqrt(dp[0]*dp[0]+dp[1]*dp[1]+dp[2]*dp[2]) < 1e-3 {
			break
		}
	}
	return u, cov, nil
}

// halves solves the first and the second half (by time) separately and
// tests their horizontal agreement (DESIGN §10.2 consistency; a station
// that moved, a clock fault or a bad peer set shows here).
func (p *prob) halves(u geo.Vec3, fr, fu *geo.Frame, hp heightPrior, w, b []float64, infl float64, sol *Solution) {
	ts := make([]float64, 0, p.n)
	for k := 0; k < p.n; k++ {
		if w[k] > 0 {
			ts = append(ts, p.t[k])
		}
	}
	if len(ts) < 8*minSegObs {
		return
	}
	slices.Sort(ts)
	tm := ts[len(ts)/2]
	var pos [2]geo.Vec3
	var cv [2][9]float64
	for h := 0; h < 2; h++ {
		copy(p.b, b)
		n := 0
		keys := map[uint32]struct{}{}
		for k := 0; k < p.n; k++ {
			in := (p.t[k] < tm) == (h == 0)
			p.w[k] = 0
			if in && w[k] > 0 {
				p.w[k] = w[k]
				n++
				keys[p.in[k].Key] = struct{}{}
			}
		}
		if n < 4*minSegObs || len(keys) < 3 {
			return
		}
		uh, c, err := p.gnFixedW(u, fr, hp, 6)
		if err != nil {
			return
		}
		pos[h] = fu.ToENU(uh)
		cv[h] = rotateCov(c, fr, fu)
	}
	dE, dN := pos[0].X-pos[1].X, pos[0].Y-pos[1].Y
	ee := infl * (cv[0][0] + cv[1][0])
	en := infl * (cv[0][1] + cv[1][1])
	nn := infl * (cv[0][4] + cv[1][4])
	floor := p.cfg.SysFloorAnchoredM*sol.Anchored + p.cfg.SysFloorRelativeM*(1-sol.Anchored)
	ee += 2 * floor * floor
	nn += 2 * floor * floor
	det := ee*nn - en*en
	sol.HalvesM = math.Hypot(dE, dN)
	if det > 0 {
		sol.HalvesChi2 = (nn*dE*dE - 2*en*dE*dN + ee*dN*dN) / det
		sol.HalvesOK = true
	}
}

// summary fills the geometry statistics of the used observations.
func (p *prob) summary(u geo.Vec3, fu *geo.Frame, sol *Solution) {
	var bins [36]bool
	keys := map[uint32]struct{}{}
	emin, emax := 90.0, -90.0
	tmin, tmax := math.Inf(1), math.Inf(-1)
	for k := 0; k < p.n; k++ {
		if p.w[k] == 0 {
			continue
		}
		az, el, _ := fu.AzElRange(p.in[k].P)
		bins[int(math.Mod(az+360, 360)/10)%36] = true
		emin = math.Min(emin, el)
		emax = math.Max(emax, el)
		keys[p.in[k].Key] = struct{}{}
		tmin = math.Min(tmin, p.t[k])
		tmax = math.Max(tmax, p.t[k])
	}
	sol.Aircraft = len(keys)
	if sol.Aircraft == 0 {
		return
	}
	// largest run of empty bins (circular)
	gap, run := 0, 0
	for i := 0; i < 72; i++ {
		if !bins[i%36] {
			run++
			gap = max(gap, min(run, 36))
		} else {
			run = 0
		}
	}
	sol.AzSpreadDeg = float64(36-gap) * 10
	sol.ElevMinDeg, sol.ElevMaxDeg = emin, emax
	sol.DurationS = tmax - tmin
}
