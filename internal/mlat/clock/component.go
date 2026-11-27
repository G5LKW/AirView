package clock

import (
	"math"
	"slices"

	"airview/internal/mlat/linalg"
	"airview/internal/model"
)

// gaugeKind is how a component fixes the unobservable common mode.
type gaugeKind uint8

const (
	gaugeNone     gaugeKind = iota // pinned by external members (partition of an anchored or primary domain)
	gaugeAnchor                    // mean(δ_anchor) = g
	gaugeRelative                  // mean θ, φ and β_short = const over the gauge clocks
)

// component is one jointly estimated set of stations (a connected
// component of the co-observation graph, or one partition of it). Its
// state x and dense covariance P are in filter units (ns, ppb, ppb/s).
type component struct {
	id     uint32
	domain uint32 // time domain (connected component); partitions share it
	sts    []*station
	n      int
	x, P   []float64
	tE     float64

	gauge     gaugeKind
	gSet      []model.StationIdx // gauge members (sorted)
	gTheta    float64            // target of mean δ (anchor) or mean θ (relative)
	gPhi      float64            // target of mean φ (relative)
	gBeta     float64            // target of mean β_short (relative)
	betaGauge bool               // the relative gauge includes β_short (no GPS-class clock in the set)
	relative  bool               // no anchors in the time domain
	anchors   int

	// Per-epoch work (owned by the goroutine that updates the component).
	jobs     []int32 // indices into Filter.work
	J, g     []float64
	pinv, a2 []float64
	scale    []float64
	vec      []float64
	ar       arena
	gw       groupWork
	acRes    []acftResult
	peerRes  []peerResult
	nAcc     int
	nRej     int
	updated  bool
	failures int
	xs       extState // external members of the epoch (external.go)
	xsFailed bool     // the external block was not positive definite this epoch
	cmIdx    []int    // scratch: common-mode indices (addCommonDrift)
}

// acftResult is one group's outcome for the per-aircraft blacklist.
type acftResult struct {
	key  uint32
	fail bool
}

// peerResult is one accepted pair residual (member vs reference member).
type peerResult struct {
	a, b model.StationIdx
	r    float64 // ns
}

func (c *component) ensure() {
	n := c.n
	if cap(c.J) < n*n {
		c.J = make([]float64, n*n)
		c.pinv = make([]float64, n*n)
	}
	c.J = c.J[:n*n]
	c.pinv = c.pinv[:n*n]
	if cap(c.g) < n {
		c.g = make([]float64, n)
		c.scale = make([]float64, n)
		c.vec = make([]float64, n)
		c.a2 = make([]float64, n)
	}
	c.g, c.scale, c.vec, c.a2 = c.g[:n], c.scale[:n], c.vec[:n], c.a2[:n]
}

// blockF returns the transition matrix entries of a free block over dt:
// θ' = θ + φ·dt + ψ·dt²/2, φ' = φ + ψ·dt.
func predictBlock(c *component, s *station, dt float64) {
	if s.mode != modeFree {
		return
	}
	n := c.n
	P, x := c.P, c.x
	it := s.off + int(s.layout[kTheta])
	ip := s.off + int(s.layout[kPhi])
	is := -1
	if s.psi {
		is = s.off + int(s.layout[kPsi])
	}
	h2 := dt * dt / 2
	// rows (θ first: it uses the old φ and ψ rows)
	rt, rp := P[it*n:it*n+n], P[ip*n:ip*n+n]
	if is >= 0 {
		rs := P[is*n : is*n+n]
		for j := 0; j < n; j++ {
			rt[j] += dt*rp[j] + h2*rs[j]
			rp[j] += dt * rs[j]
		}
		x[it] += dt*x[ip] + h2*x[is]
		x[ip] += dt * x[is]
	} else {
		for j := 0; j < n; j++ {
			rt[j] += dt * rp[j]
		}
		x[it] += dt * x[ip]
	}
	// columns
	for i := 0; i < n; i++ {
		r := P[i*n : i*n+n]
		if is >= 0 {
			r[it] += dt*r[ip] + h2*r[is]
			r[ip] += dt * r[is]
		} else {
			r[it] += dt * r[ip]
		}
	}
}

// addQ adds the discrete process noise of one block over dt.
func addQ(c *component, s *station, dt float64) {
	n := c.n
	P := c.P
	q := s.qScale
	it := s.off + int(s.layout[kTheta])
	ib := s.off + int(s.layout[kBeta])
	P[ib*n+ib] += qBeta * dt
	if s.mode == modeAnchor {
		P[it*n+it] += q * qDelta * dt
		return
	}
	ip := s.off + int(s.layout[kPhi])
	pr := &s.prior
	d2, d3 := dt*dt, dt*dt*dt
	P[it*n+it] += q * (pr.qTheta*dt + pr.qPhi*d3/3)
	P[it*n+ip] += q * pr.qPhi * d2 / 2
	P[ip*n+it] += q * pr.qPhi * d2 / 2
	P[ip*n+ip] += q * pr.qPhi * dt
	if s.psi {
		is := s.off + int(s.layout[kPsi])
		d4, d5 := d2*d2, d2*d3
		qs := q * pr.qPsi
		P[it*n+it] += qs * d5 / 20
		P[it*n+ip] += qs * d4 / 8
		P[ip*n+it] += qs * d4 / 8
		P[it*n+is] += qs * d3 / 6
		P[is*n+it] += qs * d3 / 6
		P[ip*n+ip] += qs * d3 / 3
		P[ip*n+is] += qs * d2 / 2
		P[is*n+ip] += qs * d2 / 2
		P[is*n+is] += qs * dt
	}
}

// predict propagates the component to t (block-diagonal F and Q, O(n²)).
func (c *component) predict(t float64) {
	dt := t - c.tE
	if dt > 0 {
		for _, s := range c.sts {
			predictBlock(c, s, dt)
		}
		for _, s := range c.sts {
			addQ(c, s, dt)
		}
	}
	c.tE = t
}

// resetState sets the variance of one state to v (ns² etc.) and removes its
// correlations (mean unchanged).
func (c *component) resetState(i int, v float64) {
	n := c.n
	for j := 0; j < n; j++ {
		c.P[i*n+j] = 0
		c.P[j*n+i] = 0
	}
	c.P[i*n+i] = v
}

// condition applies the hard constraint x_i = v (covariance-form update with
// a noise-free scalar measurement).
func (c *component) condition(i int, v float64) {
	n := c.n
	pii := c.P[i*n+i]
	if !(pii > 0) {
		c.x[i] = v
		return
	}
	col := c.vec
	for j := 0; j < n; j++ {
		col[j] = c.P[j*n+i]
	}
	d := v - c.x[i]
	for j := 0; j < n; j++ {
		c.x[j] += col[j] * d / pii
	}
	for j := 0; j < n; j++ {
		f := col[j] / pii
		if f == 0 {
			continue
		}
		r := c.P[j*n : j*n+n]
		for k := 0; k < n; k++ {
			r[k] -= f * col[k]
		}
	}
	c.x[i] = v
	for j := 0; j < n; j++ {
		c.P[i*n+j], c.P[j*n+i] = 0, 0
	}
}

// regauge changes the time scale of the component at its epoch time: the
// new network time is u = t + Δs + r·(t − t_e) (r in ppb). Every free
// block transforms exactly (to first order in r):
//
//	θ' = θ − Δs·φ + ½Δs²·ψ − Δs·1e9 + r·Δs
//	φ' = (φ − Δs·ψ − r) / (1 + r·1e-9)
//	ψ' = ψ / (1 + r·1e-9)²
//
// (θ ns, φ ppb, ψ ppb/s, Δs s). Anchor blocks shift by −Δs·1e9 only, so
// r must be 0 when the component has anchors. P' = A·P·Aᵀ.
func (c *component) regauge(ds, r float64) {
	n := c.n
	P, x := c.P, c.x
	k := 1 / (1 + r*1e-9)
	for _, s := range c.sts {
		if s.layout[kTheta] < 0 {
			continue // no block yet (a fresh station)
		}
		it := s.off + int(s.layout[kTheta])
		x[it] += -ds*1e9 + r*ds
		// A station demoted earlier in this pass is free but keeps its
		// anchor block (no φ) until the next restructure: it shifts like
		// an anchor block.
		if s.mode != modeFree || s.layout[kPhi] < 0 {
			continue
		}
		ip := s.off + int(s.layout[kPhi])
		is := -1
		if s.psi {
			is = s.off + int(s.layout[kPsi])
		}
		// mean
		if is >= 0 {
			x[it] += -ds*x[ip] + 0.5*ds*ds*x[is]
			x[ip] = (x[ip] - ds*x[is] - r) * k
			x[is] *= k * k
		} else {
			x[it] += -ds * x[ip]
			x[ip] = (x[ip] - r) * k
		}
		// rows then columns of A·P·Aᵀ
		rt, rp := P[it*n:it*n+n], P[ip*n:ip*n+n]
		if is >= 0 {
			rs := P[is*n : is*n+n]
			for j := 0; j < n; j++ {
				rt[j] += -ds*rp[j] + 0.5*ds*ds*rs[j]
				rp[j] = (rp[j] - ds*rs[j]) * k
				rs[j] *= k * k
			}
		} else {
			for j := 0; j < n; j++ {
				rt[j] += -ds * rp[j]
				rp[j] *= k
			}
		}
		for i := 0; i < n; i++ {
			row := P[i*n : i*n+n]
			if is >= 0 {
				row[it] += -ds*row[ip] + 0.5*ds*ds*row[is]
				row[ip] = (row[ip] - ds*row[is]) * k
				row[is] *= k * k
			} else {
				row[it] += -ds * row[ip]
				row[ip] *= k
			}
		}
	}
}

// layoutSource says where a state of the new layout comes from.
type layoutSource struct {
	src  *component // nil = prior
	idx  int
	mean float64
	vr   float64
}

// buildComponent creates a component for sts, copying each station's
// existing block (and the cross-covariances between blocks that come from
// the same old component) and initialising fresh states from their priors.
func buildComponent(id, domain uint32, sts []*station, t float64, hostVar float64) *component {
	c := &component{id: id, domain: domain, sts: sts, tE: t}
	var srcs []layoutSource
	for _, s := range sts {
		lay, dim := s.wantLayout()
		old := s.comp
		oldLay := s.layout
		for k := stateKind(0); k < numKinds; k++ {
			if lay[k] < 0 {
				continue
			}
			ls := layoutSource{}
			if old != nil && oldLay[k] >= 0 && !s.fresh {
				oi := s.off + int(oldLay[k])
				ls.src, ls.idx = old, oi
				ls.mean = old.x[oi]
				ls.vr = old.P[oi*old.n+oi]
			}
			if ls.src == nil || s.rejoin {
				m, v := initialState(s, k, ls.src != nil, ls.mean, ls.vr, hostVar)
				ls = layoutSource{mean: m, vr: v}
			}
			srcs = append(srcs, ls)
		}
		s.off = len(srcs) - dim
		s.layout, s.dim = lay, dim
	}
	n := len(srcs)
	c.n = n
	c.x = make([]float64, n)
	c.P = make([]float64, n*n)
	for i, a := range srcs {
		c.x[i] = a.mean
		if a.src == nil {
			c.P[i*n+i] = a.vr
			continue
		}
		for j, b := range srcs {
			if b.src == a.src {
				c.P[i*n+j] = a.src.P[a.idx*a.src.n+b.idx]
			}
		}
	}
	for _, s := range sts {
		s.comp = c
		s.fresh, s.rejoin = false, false
	}
	c.ensure()
	return c
}

// initialState returns the prior of one state of a fresh block, or of a
// block that rejoins from another time domain (had: its old mean and
// variance are known, but not in this time scale).
func initialState(s *station, k stateKind, had bool, mean, vr, hostVar float64) (float64, float64) {
	pr := &s.prior
	switch k {
	case kTheta:
		if had {
			return mean, math.Max(hostVar, vr)
		}
		return s.initTh, hostVar
	case kPhi:
		v := pr.phiSig * pr.phiSig
		if had {
			// another time scale: a relative component's rate can differ
			// by up to ~100 ppm
			return mean, math.Max(v, 1e10)
		}
		return 0, v
	case kPsi:
		v := pr.psiSig * pr.psiSig
		if had {
			return mean, math.Max(vr, v)
		}
		return 0, v
	case kBeta:
		if had {
			return mean, vr
		}
		return 0, pr.betSig * pr.betSig
	}
	return 0, 1
}

// update applies the accumulated information J, g of the epoch:
//
//	P⁺ = (P⁻⁻¹ + J)⁻¹,  x⁺ = x⁻ + P⁺·g,
//
// with symmetric diagonal scaling (Ps = S⁻¹PS⁻¹, S = diag √P_ii) so the two
// SPD inversions work on unit-diagonal matrices. It returns false (and
// leaves x, P unchanged) when a matrix is not positive definite.
func (c *component) update() bool {
	n := c.n
	P, J := c.P, c.J
	s := c.scale
	for i := 0; i < n; i++ {
		d := P[i*n+i]
		if !(d > 0) {
			d = 1e-30
		}
		s[i] = math.Sqrt(d)
	}
	A := c.pinv
	// Rounding can make a nearly singular correlation matrix indefinite;
	// the retries regularise it by inflating every variance by (1+ε)
	// (conservative) before giving up for this epoch.
	ok := false
	for _, eps := range [...]float64{0, 1e-12, 1e-9, 1e-6} {
		for i := 0; i < n; i++ {
			ri := P[i*n : i*n+n]
			ai := A[i*n : i*n+n]
			si := s[i]
			for j := 0; j <= i; j++ {
				ai[j] = ri[j] / (si * s[j])
			}
			ai[i] += eps
		}
		if linalg.SPDInverse(A, n, c.vec) != nil {
			continue
		}
		for i := 0; i < n; i++ {
			ai := A[i*n : i*n+n]
			ji := J[i*n : i*n+n]
			si := s[i]
			for j := 0; j <= i; j++ {
				ai[j] += ji[j] * si * s[j]
			}
		}
		if linalg.SPDInverse(A, n, c.vec) == nil {
			ok = true
			break
		}
	}
	if !ok {
		c.failures++
		return false
	}
	// P⁺ = S·A⁻¹·S
	for i := 0; i < n; i++ {
		ai := A[i*n : i*n+n]
		ri := P[i*n : i*n+n]
		si := s[i]
		for j := 0; j < n; j++ {
			ri[j] = ai[j] * si * s[j]
		}
	}
	// dx = P⁺ g
	dx := c.a2
	linalg.MatVec(dx, P, c.g, n, n)
	for i := 0; i < n; i++ {
		c.x[i] += dx[i]
	}
	return true
}

// addConstraint adds the soft linear constraint Σ w_i·x_idx[i] = target with
// standard deviation sigma to the epoch information.
func (c *component) addConstraint(idx []int, w []float64, target, sigma float64) {
	if len(idx) == 0 {
		return
	}
	n := c.n
	info := 1 / (sigma * sigma)
	cur := 0.0
	for k, i := range idx {
		cur += w[k] * c.x[i]
	}
	res := target - cur
	for a, i := range idx {
		c.g[i] += w[a] * info * res
		for b, j := range idx {
			c.J[i*n+j] += w[a] * w[b] * info
		}
	}
}

// gaugeIndices returns the state indices and weights of the gauge
// constraints for the current gauge set (theta, then phi).
func (c *component) gaugeIndices(kind stateKind, idx []int, w []float64) ([]int, []float64) {
	idx, w = idx[:0], w[:0]
	for _, s := range c.sts {
		if !slices.Contains(c.gSet, s.idx) {
			continue
		}
		if s.layout[kind] < 0 {
			continue
		}
		idx = append(idx, s.off+int(s.layout[kind]))
		w = append(w, 1)
	}
	for i := range w {
		w[i] = 1 / float64(len(idx))
	}
	return idx, w
}

// commonIndices returns the indices of one state kind of every station of
// the component that has it (free clocks only for φ): the direction u of a
// change of the time scale (θ: offset, φ: rate, β: short-frame offset).
func (c *component) commonIndices(kind stateKind, idx []int) []int {
	idx = idx[:0]
	for _, s := range c.sts {
		l := s.layout[kind]
		if l < 0 || (kind == kPhi && s.mode != modeFree) {
			continue
		}
		idx = append(idx, s.off+int(l))
	}
	return idx
}

// projectGauge re-defines the unobservable common mode of one state kind
// when the gauge changes (DESIGN §9.2.1 gauge; doc.go "Gauge"). The gauge
// pseudo-observation accumulates information on wᵀx (the old gauge
// combination) every epoch. That information is a definition, not a
// measurement: if the gauge set then changes (a station joins or leaves the
// set of synced clocks), the old, tightly known combination and the new
// constraint together pin the joining or leaving station's own state to its
// current estimate with about the gauge σ. On simnet SDR-only networks this
// pinned a wrong φ to 1-4 ppb, which then drove runs of false steps.
//
// The fix is the exact change of time scale x' = x − u·(wᵀx − g) with g =
// wᵀx̂ (the mean is unchanged): A = I − u·wᵀ with u the indicator of the
// kind's states of every station and wᵀu = 1. Every difference x_i − x_j is
// invariant under A, so all relative information is kept, while the
// accumulated common-mode information is released. The common mode then
// gets the gauge variance sigma² back (the time scale is defined to that
// accuracy; the next gauge constraint holds it):
//
//	P' = A·P·Aᵀ + σ²·u·uᵀ = P − u·pᵀ − p·uᵀ + (s + σ²)·u·uᵀ,  p = P·w, s = wᵀP·w
//
// O(n·|u|). TestGaugeChangeReleasesCommonMode checks it.
func (c *component) projectGauge(u, idx []int, w []float64, sigma float64) {
	if len(u) == 0 || len(idx) == 0 {
		return
	}
	n := c.n
	P := c.P
	p := c.vec[:n]
	for j := 0; j < n; j++ {
		v := 0.0
		row := P[j*n:]
		for k, i := range idx {
			v += w[k] * row[i]
		}
		p[j] = v
	}
	s := 0.0
	for k, i := range idx {
		s += w[k] * p[i]
	}
	for _, i := range u {
		row := P[i*n : i*n+n]
		for j := 0; j < n; j++ {
			row[j] -= p[j]
		}
	}
	for j := 0; j < n; j++ {
		row := P[j*n : j*n+n]
		pj := p[j]
		for _, i := range u {
			row[i] -= pj
		}
	}
	add := s + sigma*sigma
	for _, i := range u {
		row := P[i*n : i*n+n]
		for _, j := range u {
			row[j] += add
		}
	}
}

// sumState returns Σ w·x over the indices.
func (c *component) sumState(idx []int, w []float64) float64 {
	v := 0.0
	for k, i := range idx {
		v += w[k] * c.x[i]
	}
	return v
}

// symmetrize averages P with its transpose.
func (c *component) symmetrize() {
	n := c.n
	for i := 0; i < n; i++ {
		for j := 0; j < i; j++ {
			v := 0.5 * (c.P[i*n+j] + c.P[j*n+i])
			c.P[i*n+j], c.P[j*n+i] = v, v
		}
	}
}

// gpsInSet reports whether a GPS-class clock is in the gauge set. Their
// β_short prior (±50 ns) already fixes the common mode of β; a β gauge on
// top of it pinned the GPS stations' β and, through it, made the published
// σ of an all-GPS network over-confident (row 1: within 1σ 51 % instead of
// 64 %).
func (c *component) gpsInSet() bool {
	for _, s := range c.sts {
		if s.cls.GPS() && slices.Contains(c.gSet, s.idx) {
			return true
		}
	}
	return false
}
