package track

import (
	"math"
	"slices"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

const (
	maxHyp           = 4
	excCacheN        = MaxClusterMembers
	violationPenalty = 20.0 // log-likelihood penalty of a physically impossible hypothesis
	stepLR           = 4.0  // largest score loss of one measurement against the best hypothesis (settle)
	dofWindowNs      = 10e9
)

type modelState struct {
	x vec
	P mat
}

// hypState is the IMM state of one hypothesis at time tNs.
type hypState struct {
	tNs int64
	m   [nModes]modelState
	mu  [nModes]float64
}

type excEntry struct {
	st        uint32
	spos, p0  geo.Vec3
	exc       float64
	g         geo.Vec3
	populated bool
	usedNs    int64 // last accepted TOA update that used this station (0 = never)
	touchNs   int64 // last lookup (LRU eviction)
}

// hyp is one track hypothesis of an ICAO.
type hyp struct {
	hypState
	frame     geo.Frame
	score     float64 // accumulated log-likelihood relative to the entry's best
	prior     float64 // kinematic log-prior of the current state (speedPrior; only while competing)
	createdNs int64
	lastNs    int64 // last accepted update (measurement time)
	posNs     int64 // last position-determining update (fix, or cluster with >= 3 rows)
	posUpd    int
	confirmed bool
	updates   uint32
	lastN     int
	lastDOF   int
	altUsed   bool
	maxDOF    int
	dofNs     int64
	cache     [excCacheN]excEntry
}

type entry struct {
	icao uint32
	hyps [maxHyp]*hyp
	n    int
	miss int // consecutive fixes that gated against no hypothesis
}

// Tracker is the IMM raw-TOA tracker for a set of ICAOs. It is not safe for
// concurrent use; Sharded spreads ICAOs over independently locked Trackers.
type Tracker struct {
	p       Params
	gate    [maxRows + 1]float64
	entries map[uint32]*entry
	freeH   []*hyp
	freeE   []*entry
	w       work
	ms      meas
	stats   Stats
	// sweepEst is the Estimate Sweep hands to emit (reused).
	sweepEst Estimate
	keys     []uint32 // Sweep's sorted ICAO list (reused)
}

// New returns a tracker with normalised parameters.
func New(p Params) *Tracker {
	p = p.Normalize()
	t := &Tracker{p: p, entries: make(map[uint32]*entry)}
	for k := 1; k <= maxRows; k++ {
		t.gate[k] = Chi2Quantile(p.GateProb, k)
	}
	return t
}

// Params returns the normalised parameters.
func (t *Tracker) Params() Params { return t.p }

// Len returns the number of ICAOs with a track.
func (t *Tracker) Len() int { return len(t.entries) }

// Stats returns the counters (Tracks/Confirmed/Hypotheses are current).
func (t *Tracker) Stats() Stats {
	s := t.stats
	s.Tracks = len(t.entries)
	for _, e := range t.entries {
		s.Hypotheses += e.n
		if b := e.best(); b != nil && b.confirmed {
			s.Confirmed++
		}
	}
	return s
}

func (t *Tracker) newHyp() *hyp {
	if n := len(t.freeH); n > 0 {
		h := t.freeH[n-1]
		t.freeH = t.freeH[:n-1]
		*h = hyp{}
		return h
	}
	return &hyp{}
}

func (t *Tracker) newEntry(icao uint32) *entry {
	var e *entry
	if n := len(t.freeE); n > 0 {
		e = t.freeE[n-1]
		t.freeE = t.freeE[:n-1]
		*e = entry{}
	} else {
		e = &entry{}
	}
	e.icao = icao
	t.entries[icao] = e
	return e
}

// remove drops hypothesis i of e (and the entry when it was the last).
func (t *Tracker) remove(e *entry, i int) {
	t.freeH = append(t.freeH, e.hyps[i])
	e.n--
	e.hyps[i] = e.hyps[e.n]
	e.hyps[e.n] = nil
	if e.n == 0 {
		delete(t.entries, e.icao)
		t.freeE = append(t.freeE, e)
		t.stats.Deleted++
	}
}

// best returns the hypothesis to output: the highest-scoring confirmed one,
// else the highest-scoring tentative one.
func (e *entry) best() *hyp {
	var b *hyp
	for i := 0; i < e.n; i++ {
		h := e.hyps[i]
		if b == nil || (h.confirmed && !b.confirmed) || (h.confirmed == b.confirmed && h.score+h.prior > b.score+b.prior) {
			b = h
		}
	}
	return b
}

// combined returns the IMM-combined mean and covariance of s.
func combined(s *hypState) (x vec, P mat) {
	for j := range nModes {
		for c := range nx {
			x[c] += s.mu[j] * s.m[j].x[c]
		}
	}
	for j := range nModes {
		var d vec
		for c := range nx {
			d[c] = s.m[j].x[c] - x[c]
		}
		mu := s.mu[j]
		for a := range nx {
			for b := range nx {
				P[a*nx+b] += mu * (s.m[j].P[a*nx+b] + d[a]*d[b])
			}
		}
	}
	return
}

// mixPredict performs the IMM interaction step and predicts both models by
// dt >= 0 seconds. cbar receives the predicted mode probabilities.
func (t *Tracker) mixPredict(s *hypState, dt, gq float64, out *hypState) (cbar [nModes]float64) {
	pi := switchProb(dt, &t.p)
	for j := range nModes {
		for i := range nModes {
			cbar[j] += pi[i][j] * s.mu[i]
		}
	}
	for j := range nModes {
		var w [nModes]float64
		for i := range nModes {
			if cbar[j] > 0 {
				w[i] = pi[i][j] * s.mu[i] / cbar[j]
			}
		}
		var x vec
		for i := range nModes {
			for c := range nx {
				x[c] += w[i] * s.m[i].x[c]
			}
		}
		var P mat
		for i := range nModes {
			if w[i] == 0 {
				continue
			}
			var d vec
			for c := range nx {
				d[c] = s.m[i].x[c] - x[c]
			}
			for a := range nx {
				for b := range nx {
					P[a*nx+b] += w[i] * (s.m[i].P[a*nx+b] + d[a]*d[b])
				}
			}
		}
		predict(j, &x, &P, dt, &t.p, gq)
		out.m[j] = modelState{x: x, P: P}
	}
	out.mu = cbar
	out.tNs = s.tNs + int64(math.Round(dt*1e9))
	return
}

// immResult is the outcome of one IMM update attempt.
type immResult struct {
	pass   bool
	ll     float64 // mixture log-likelihood (or the gate-boundary penalty)
	nisMin float64
	cand   hypState
}

// imm runs one IMM cycle of hypothesis h with measurement ms at tNs into r
// without modifying h.
func (t *Tracker) imm(h *hyp, ms *meas, tNs int64, r *immResult) {
	dt := float64(tNs-h.tNs) / 1e9
	var pred hypState
	var cbar [nModes]float64
	back := 0.0
	if dt >= 0 {
		cbar = t.mixPredict(&h.hypState, dt, t.gapQ(h, tNs), &pred)
	} else {
		pred = h.hypState
		cbar = h.mu
		back = dt
	}
	var outs [nModes]modelOut
	r.pass = false
	r.nisMin = math.Inf(1)
	bestDet := math.NaN()
	for j := range nModes {
		t.w.updateModel(ms, &h.frame, j, &pred.m[j].x, &pred.m[j].P, back, &t.p, &outs[j])
		if outs[j].ok && outs[j].nis < r.nisMin {
			r.nisMin = outs[j].nis
			bestDet = outs[j].logdetS
		}
	}
	gamma := t.gate[ms.rows]
	if math.IsNaN(bestDet) {
		r.ll = -0.5*gamma - violationPenalty
		return
	}
	if r.nisMin > gamma {
		r.ll = -0.5 * (gamma + bestDet + float64(ms.rows)*log2Pi)
		return
	}
	// μ_j ∝ c̄_j Λ_j (log-sum-exp)
	var a [nModes]float64
	amax := math.Inf(-1)
	for j := range nModes {
		a[j] = math.Inf(-1)
		if outs[j].ok && cbar[j] > 0 {
			a[j] = math.Log(cbar[j]) + outs[j].ll
		}
		amax = math.Max(amax, a[j])
	}
	var sum float64
	for j := range nModes {
		sum += math.Exp(a[j] - amax)
	}
	r.ll = amax + math.Log(sum)
	r.cand = pred
	if dt < 0 {
		r.cand.tNs = h.tNs
	}
	var musum float64
	for j := range nModes {
		mu := math.Exp(a[j] - r.ll)
		mu = math.Max(mu, 1e-4) // never lock a mode out completely
		r.cand.mu[j] = mu
		musum += mu
		if outs[j].ok {
			r.cand.m[j] = modelState{x: outs[j].x, P: outs[j].P}
		}
	}
	for j := range nModes {
		r.cand.mu[j] /= musum
	}
	r.pass = true
}

// commit stores an accepted update in h.
func (t *Tracker) commit(h *hyp, r *immResult, tNs int64) {
	h.hypState = r.cand
	if tNs > h.lastNs {
		h.lastNs = tNs
	}
	h.updates++
	x, _ := combined(&h.hypState)
	if math.Hypot(x[iE], x[iN]) > t.p.RecentreM {
		t.recentre(h)
	}
}

// notePos records a position-determining update for confirmation and the
// output DOF.
func (t *Tracker) notePos(h *hyp, tNs int64, nst, dof int, alt bool) {
	h.posUpd++
	if tNs > h.posNs {
		h.posNs = tNs
	}
	h.lastN, h.lastDOF, h.altUsed = nst, dof, alt
	if tNs-h.dofNs > dofWindowNs || dof >= h.maxDOF {
		h.maxDOF, h.dofNs = dof, tNs
	}
	if !h.confirmed && h.posUpd >= t.p.ConfirmUpdates &&
		float64(tNs-h.createdNs)/1e9 <= t.p.ConfirmWindowS {
		h.confirmed = true
		t.stats.Confirmations++
	}
}

// recentre moves h's frame under its current position (doc.go §2).
func (t *Tracker) recentre(h *hyp) {
	x, _ := combined(&h.hypState)
	p := h.frame.FromENU(geo.Vec3{X: x[iE], Y: x[iN], Z: x[iU]})
	llh := geo.FromECEF(p)
	nf := geo.NewFrame(geo.LLH{Lat: llh.Lat, Lon: llh.Lon})
	// M = R₂R₁ᵀ, d = R₂(O₁ − O₂)
	var M [3]geo.Vec3 // columns
	for c, u := range [3]geo.Vec3{{X: 1}, {Y: 1}, {Z: 1}} {
		M[c] = nf.Rotate(h.frame.Unrotate(u))
	}
	d := nf.ToENU(h.frame.Origin)
	T := identity()
	for blk := range 2 {
		o := 3 * blk
		for c := range 3 {
			T[(o+0)*nx+o+c] = M[c].X
			T[(o+1)*nx+o+c] = M[c].Y
			T[(o+2)*nx+o+c] = M[c].Z
		}
	}
	for j := range nModes {
		s := &h.m[j]
		var xn vec
		for a := range nx {
			for b := range nx {
				xn[a] += T[a*nx+b] * s.x[b]
			}
		}
		xn[iE] += d.X
		xn[iN] += d.Y
		xn[iU] += d.Z
		s.x = xn
		s.P = congruence(&T, &s.P)
	}
	h.frame = nf
}

// excess fills the linearised refraction excess of m for an aircraft near
// p from h's cache.
func (t *Tracker) excess(h *hyp, m *member, p geo.Vec3) {
	ci := -1
	for i := range h.cache {
		c := &h.cache[i]
		if c.populated && c.st == m.st && c.spos == m.pos {
			ci = i
			break
		}
	}
	if ci < 0 { // evict the least recently used entry
		ci = 0
		for i := 1; i < excCacheN; i++ {
			if h.cache[i].touchNs < h.cache[ci].touchNs {
				ci = i
			}
		}
		h.cache[ci] = excEntry{}
	}
	e := &h.cache[ci]
	if !e.populated || e.p0.Dist(p) > t.p.ExcessRefreshM {
		L, grad := t.p.Atmosphere.RangeGrad(m.pos, p)
		d := p.Sub(m.pos)
		r := d.Norm()
		*e = excEntry{st: m.st, spos: m.pos, p0: p, exc: L - r, g: grad.Sub(d.Scale(1 / r)), populated: true, usedNs: e.usedNs}
	}
	e.touchNs = h.lastNs + 1
	m.p0, m.exc, m.g, m.ci = e.p0, e.exc, e.g, ci
}

// addSync adds the correlated clock-sync part of the TOA variances to the
// TDOA covariance of ms (doc.go §5.3). A Gauss-Markov error of variance s²
// and correlation time τ carries, per sample Δt apart, the information of a
// white error of variance s²·2τ/Δt; Δt is the time since this hypothesis
// last used the station (from the cache), so repeating a station many times
// per second cannot average its clock error away.
func (t *Tracker) addSync(h *hyp, ms *meas, tNs int64) {
	rows := ms.rows
	nT := ms.nst - 1
	var v [MaxClusterMembers]float64
	any := false
	for j := 0; j < ms.nst; j++ {
		m := &ms.st[j]
		if m.syncVar <= 0 {
			continue
		}
		any = true
		f := 1.0
		if u := h.cache[m.ci].usedNs; u != 0 {
			dt := math.Max(float64(tNs-u)/1e9, 1e-3)
			f = math.Max(1, 2*t.p.SyncTauS/dt)
		}
		v[j] = geo.C * geo.C * m.syncVar * f
	}
	if !any {
		return
	}
	for k := range nT {
		for l := range nT {
			ms.R[k*rows+l] += v[0]
		}
		ms.R[k*rows+k] += v[k+1]
	}
}

// markUsed records that h's accepted update at tNs used ms's stations.
func markUsed(h *hyp, ms *meas, tNs int64) {
	for j := 0; j < ms.nst; j++ {
		if c := &h.cache[ms.st[j].ci]; c.st == ms.st[j].st && tNs > c.usedNs {
			c.usedNs = tNs
		}
	}
}

// predictedPos returns h's position extrapolated (CV) to tNs, ECEF.
func predictedPos(h *hyp, tNs int64) geo.Vec3 {
	x, _ := combined(&h.hypState)
	dt := float64(tNs-h.tNs) / 1e9
	return h.frame.FromENU(geo.Vec3{X: x[iE] + dt*x[iVE], Y: x[iN] + dt*x[iVN], Z: x[iU] + dt*x[iVU]})
}

// plausible checks a hypothesis after an update: horizontal speed within
// MaxSpeedMS (3σ), not deep underground, and (for clusters) above the radio
// horizon of every station that heard it.
func (t *Tracker) plausible(h *hyp, ms *meas) bool {
	x, P := combined(&h.hypState)
	v := math.Hypot(x[iVE], x[iVN])
	sv := math.Sqrt(math.Max(P[iVE*nx+iVE]+P[iVN*nx+iVN], 0))
	if v-3*sv > t.p.MaxSpeedMS {
		return false
	}
	p := h.frame.FromENU(geo.Vec3{X: x[iE], Y: x[iN], Z: x[iU]})
	if geo.GeocentricHeight(p) < -1000 {
		return false
	}
	if ms != nil && ms.kind == kTOA {
		for j := 0; j < ms.nst; j++ {
			if !geo.LineOfSight(ms.st[j].pos, p, -t.p.LOSMarginM) {
				return false
			}
		}
	}
	return true
}

// settle applies the per-measurement scores of an entry, prunes and
// normalises. lls[i] is hypothesis i's log-likelihood (accepted, or the
// gate-boundary value when rejected), pen[i] its physical-violation
// penalty; scored tells whether every hypothesis processed the
// measurement (only then are the scores comparable).
//
// One measurement may move a hypothesis at most stepLR nats behind the
// best one (plus its violation penalty): with tens of updates per
// ambiguous track, a single 99.9 %-gate miss of the true branch (one in a
// thousand updates, a multipath copy, a clock glitch) would otherwise cost
// it 6-8 nats against a ghost that happened to fit, enough for the
// likelihood-ratio prune (seen on the same-three-station ambiguity test:
// 4 of 130 true branches dropped that way within 30 s). Consistent
// evidence still accumulates step by step.
func (t *Tracker) settle(e *entry, lls, pen *[maxHyp]float64, scored bool) {
	if scored && e.n > 1 {
		top := math.Inf(-1)
		for i := 0; i < e.n; i++ {
			top = math.Max(top, lls[i])
		}
		for i := 0; i < e.n; i++ {
			e.hyps[i].score += lls[i] - pen[i]
		}
	}
	if e.n <= 1 {
		if e.n == 1 {
			e.hyps[0].score, e.hyps[0].prior = 0, 0
		}
		return
	}
	// Competing hypotheses are compared on likelihood × kinematic prior
	// (speedPrior): the far branch of a 3+alt pair often implies a ground
	// speed no aircraft at that height flies.
	best := math.Inf(-1)
	for i := 0; i < e.n; i++ {
		h := e.hyps[i]
		h.prior = t.speedPrior(&h.hypState)
		best = math.Max(best, h.score+h.prior)
	}
	lim := best - math.Log(t.p.HypLR)
	for i := e.n - 1; i >= 0; i-- {
		if e.hyps[i].score+e.hyps[i].prior < lim {
			t.stats.Pruned++
			t.remove(e, i)
		}
	}
	for i := 0; i < e.n; i++ {
		e.hyps[i].score -= best
	}
}

// Manoeuvre allowance of unobservable stretches (doc.go §8 "2-station
// maintenance"). While a track is maintained by 2-station clusters (one
// TDOA row plus the altitude) its position along the hyperbola and a turn
// are not observed, and the low white-noise acceleration of steady flight
// (QCV 0.3 m²/s³) let manoeuvring GA and helicopters leave their σ (simnet:
// 92-94 % of the errors inside the 99.9 % region, single errors of several
// km). From gapStartS after the last position-determining update on, the
// horizontal PSD of both models is raised by a²·τ (ramped in over
// gapRampS): a lateral acceleration a the aircraft may pull without being
// seen - gapAccelSlow (2.5 m/s², about a 15° bank) at or below
// gapSlowMS, gapAccelFast (1 m/s²) at or above gapFastMS, linear between,
// since slow traffic manoeuvres far more often - with correlation time
// gapTauS. A position-determining update ends the stretch.
const (
	gapStartS    = 3.0
	gapRampS     = 5.0
	gapTauS      = 10.0
	gapAccelSlow = 1.2
	gapAccelFast = 0.0
	gapSlowMS    = 80.0
	gapFastMS    = 160.0
)

// gapQ returns the extra horizontal acceleration PSD (m²/s³) for
// predicting h to tNs.
func (t *Tracker) gapQ(h *hyp, tNs int64) float64 {
	gap := float64(tNs-h.posNs)/1e9 - gapStartS
	if gap <= 0 {
		return 0
	}
	var ve, vn float64
	for j := range nModes {
		ve += h.mu[j] * h.m[j].x[iVE]
		vn += h.mu[j] * h.m[j].x[iVN]
	}
	f := math.Min(math.Max((math.Hypot(ve, vn)-gapSlowMS)/(gapFastMS-gapSlowMS), 0), 1)
	a := gapAccelSlow + f*(gapAccelFast-gapAccelSlow)
	return a * a * gapTauS * math.Min(gap/gapRampS, 1)
}

// Kinematic prior of a hypothesis (doc.go §8). Aircraft above a few
// thousand metres fly faster than a minimum ground speed (fixed-wing
// traffic: well above 60 m/s at cruise heights even into a jet-stream
// headwind; rotorcraft and gliders stay low and slow), and civil ground
// speeds stay below about 300 m/s. The far branch of a 3+alt pair seen by
// the same three stations often moves at 10-60 m/s at FL150-FL350 or above
// 350 m/s, while its likelihood alone stays within 1-4 nats of the true
// branch for many seconds. speedPrior returns
//
//	−½·z², z = max(0, vMin(h) − v, v − speedPriorMaxMS) / hypot(σᵥ, 10 m/s)
//
// capped at −speedPriorCap, with vMin rising linearly from 0 at 1500 m to
// speedPriorMinMS (70 m/s) at 4500 m HAE and σᵥ the largest horizontal
// velocity σ (the square root of the larger eigenvalue). It
// only enters comparisons between competing hypotheses of one ICAO (settle,
// best): a lone track is never penalised for flying slowly.
const (
	speedPriorMinMS = 70.0
	speedPriorMaxMS = 300.0
	speedPriorCap   = 12.0
	speedPriorLowM  = 1500.0
	speedPriorHighM = 4500.0
)

func (t *Tracker) speedPrior(s *hypState) float64 {
	x, P := combined(s)
	v := math.Hypot(x[iVE], x[iVN])
	// the largest horizontal velocity variance: a young track's speed is
	// uncertain in some direction even when its estimate is small, and must
	// not be called implausible for it
	a, b, d := P[iVE*nx+iVE], P[iVE*nx+iVN], P[iVN*nx+iVN]
	sv2 := 0.5*(a+d) + math.Sqrt(0.25*(a-d)*(a-d)+b*b)
	// height: the frame's up coordinate is the height above the frame
	// origin on the ellipsoid (recentred within RecentreM), good to tens of
	// metres here
	h := x[iU]
	vmin := speedPriorMinMS * math.Min(math.Max((h-speedPriorLowM)/(speedPriorHighM-speedPriorLowM), 0), 1)
	ex := math.Max(math.Max(vmin-v, v-speedPriorMaxMS), 0)
	if ex == 0 {
		return 0
	}
	z := ex / math.Sqrt(math.Max(sv2, 0)+10*10)
	return -math.Min(0.5*z*z, speedPriorCap)
}

// expire drops hypotheses of e that coasted too long or were not
// confirmed in time, as of nowNs. It reports whether the entry survives.
func (t *Tracker) expire(e *entry, nowNs int64) bool {
	for i := e.n - 1; i >= 0; i-- {
		h := e.hyps[i]
		if float64(nowNs-h.lastNs)/1e9 > t.p.CoastS ||
			(!h.confirmed && float64(nowNs-h.createdNs)/1e9 > t.p.ConfirmWindowS) {
			if e.n == 1 {
				t.remove(e, i)
				return false
			}
			t.remove(e, i)
		}
	}
	return e.n > 0
}

// buildTOA prepares the TDOA measurement of cluster c, skipping the member
// indices in excl. It reports false when fewer than two usable members
// remain. idx receives the cluster indices of ms.st.
func (t *Tracker) buildTOA(c *Cluster, excl []int, ms *meas, idx *[MaxClusterMembers]int) bool {
	m := len(c.TOAs)
	hasCov := len(c.Cov) == m*m && m > 0
	sig := func(i int) float64 {
		if hasCov {
			return math.Sqrt(math.Max(c.Cov[i*m+i], 0))
		}
		return c.TOAs[i].Sigma
	}
	n := 0
outer:
	for i := range m {
		for _, x := range excl {
			if x == i {
				continue outer
			}
		}
		s := sig(i)
		if !(s > 0) || math.IsInf(s, 0) || math.IsNaN(c.TOAs[i].T) {
			continue
		}
		// insertion into the n best (smallest σ) so far
		if n == t.p.MaxMembers && s >= sig(idx[n-1]) {
			continue
		}
		k := min(n, t.p.MaxMembers-1)
		for k > 0 && sig(idx[k-1]) > s {
			idx[k] = idx[k-1]
			k--
		}
		idx[k] = i
		if n < t.p.MaxMembers {
			n++
		}
	}
	if n < 2 {
		return false
	}
	ms.kind = kTOA
	ms.nst = n
	ms.alt = c.AltSigma > 0 && !math.IsNaN(c.AltHAE)
	ms.rows = n - 1
	if ms.alt {
		ms.rows++
	}
	rows := ms.rows
	cov := func(a, b int) float64 {
		if hasCov {
			return c.Cov[a*m+b]
		}
		if a == b {
			s := c.TOAs[a].Sigma
			return s * s
		}
		return 0
	}
	const c2 = geo.C * geo.C
	r0 := idx[0]
	t0 := c.TOAs[r0].T
	for j := range n {
		ss := c.TOAs[idx[j]].SyncSigma
		ms.st[j] = member{st: c.TOAs[idx[j]].Station, pos: c.TOAs[idx[j]].Pos, syncVar: ss * ss}
	}
	clear(ms.R[:rows*rows])
	s00 := cov(r0, r0)
	for k := 1; k < n; k++ {
		a := idx[k]
		ms.z[k-1] = geo.C * (c.TOAs[a].T - t0)
		for l := 1; l <= k; l++ {
			b := idx[l]
			v := c2 * (cov(a, b) - cov(a, r0) - cov(r0, b) + s00)
			ms.R[(k-1)*rows+(l-1)] = v
			ms.R[(l-1)*rows+(k-1)] = v
		}
	}
	if ms.alt {
		ms.z[rows-1] = c.AltHAE
		ms.R[(rows-1)*rows+rows-1] = c.AltSigma * c.AltSigma
	}
	return true
}

// Cluster applies a raw TOA cluster (§9.5). out, when non-nil, receives the
// ICAO's current estimate (Status none when it has no track).
func (t *Tracker) Cluster(c *Cluster, out *Estimate) Result {
	t.stats.Clusters++
	res := t.cluster(c)
	t.fill(c.ICAO, out)
	return res
}

func (t *Tracker) cluster(c *Cluster) Result {
	e := t.entries[c.ICAO]
	if e == nil || !t.expire(e, c.T0Ns) {
		return ResNoTrack
	}
	var idx [MaxClusterMembers]int
	ms := &t.ms
	if !t.buildTOA(c, nil, ms, &idx) {
		return ResIgnored
	}
	refT := c.TOAs[idx[0]].T
	refPos := c.TOAs[idx[0]].Pos
	rowsAll := ms.rows
	var lls, pen [maxHyp]float64
	scored := true
	accepted, notConf, late := 0, 0, 0
	var r immResult
	for i := 0; i < e.n; i++ {
		h := e.hyps[i]
		if rowsAll < 3 && !h.confirmed {
			notConf++
			scored = false
			continue
		}
		// transmit instant from the hypothesis' predicted position
		pp := predictedPos(h, c.T0Ns+int64(refT*1e9))
		tNs := c.T0Ns + int64(math.Round((refT-pp.Dist(refPos)/geo.C)*1e9))
		if float64(h.tNs-tNs)/1e9 > t.p.MaxLagS {
			late++
			scored = false
			continue
		}
		var excl [4]int
		nex := 0
		if !t.buildTOA(c, nil, ms, &idx) {
			return ResIgnored
		}
		for {
			for j := 0; j < ms.nst; j++ {
				t.excess(h, &ms.st[j], pp)
			}
			t.addSync(h, ms, tNs)
			t.imm(h, ms, tNs, &r)
			// Outlier exclusion only for a lone confirmed track: competing
			// hypotheses must be scored on the same rows, and dropping the
			// member that contradicts a wrong hypothesis would rescue it.
			if r.pass || nex >= t.p.MaxExclude || ms.nst < 4 || e.n > 1 || !h.confirmed {
				break
			}
			// exclude the member that disagrees most with the prediction
			worst := t.worstMember(c, ms, &idx, pp)
			if worst < 0 {
				break
			}
			excl[nex] = worst
			nex++
			if !t.buildTOA(c, excl[:nex], ms, &idx) {
				break
			}
		}
		lls[i] = r.ll
		if !r.pass {
			continue
		}
		t.stats.Excluded += uint64(nex)
		if tNs < h.tNs {
			t.stats.OOSM++
		}
		markUsed(h, ms, tNs)
		t.commit(h, &r, tNs)
		accepted++
		if ms.rows >= 3 {
			dof := ms.rows - 3
			t.notePos(h, tNs, ms.nst, dof, ms.alt)
		}
		if (e.n > 1 || !h.confirmed) && !t.plausible(h, ms) {
			pen[i] += violationPenalty
			if !h.confirmed && e.n == 1 {
				t.remove(e, i)
				return ResGated
			}
		}
	}
	t.settle(e, &lls, &pen, scored)
	switch {
	case accepted > 0:
		t.stats.Applied++
		return ResApplied
	case notConf > 0 && late == 0 && notConf == e.n:
		return ResNotConfirmed
	case late > 0 && accepted == 0 && late+notConf == e.n:
		t.stats.TooLate++
		return ResTooLate
	}
	t.stats.Gated++
	return ResGated
}

// worstMember returns the cluster index of the member whose TOA disagrees
// most with the predicted position (residual against the weighted median
// transmit time, in σ), or -1.
func (t *Tracker) worstMember(c *Cluster, ms *meas, idx *[MaxClusterMembers]int, pp geo.Vec3) int {
	var res [MaxClusterMembers]float64
	var srt [MaxClusterMembers]float64
	n := ms.nst
	for j := range n {
		m := &ms.st[j]
		rho := m.pos.Dist(pp) + m.exc
		res[j] = geo.C*c.TOAs[idx[j]].T - rho
		srt[j] = res[j]
	}
	s := srt[:n]
	for i := 1; i < n; i++ { // insertion sort (n <= 32)
		for k := i; k > 0 && s[k-1] > s[k]; k-- {
			s[k-1], s[k] = s[k], s[k-1]
		}
	}
	med := s[n/2]
	if n%2 == 0 {
		med = 0.5 * (s[n/2-1] + s[n/2])
	}
	worst, wv := -1, 0.0
	for j := range n {
		sg := c.TOAs[idx[j]].Sigma
		if len(c.Cov) == len(c.TOAs)*len(c.TOAs) {
			sg = math.Sqrt(c.Cov[idx[j]*len(c.TOAs)+idx[j]])
		}
		v := math.Abs(res[j]-med) / (geo.C*sg + 1)
		if v > wv {
			worst, wv = idx[j], v
		}
	}
	return worst
}

// validCov reports whether c is a usable 3×3 covariance.
func validCov(c *[9]float64) bool {
	m := linalg.Mat3(*c)
	_, err := m.Chol()
	return err == nil
}

// buildFix prepares a fix hypothesis in frame fr.
func buildFix(fh *FixHyp, fr *geo.Frame, ms *meas) {
	ms.kind, ms.rows, ms.nst, ms.alt = kFix, 3, 0, false
	e := fr.ToENU(fh.ECEF)
	ms.z[0], ms.z[1], ms.z[2] = e.X, e.Y, e.Z
	cov := fh.Cov
	if !validCov(&cov) {
		cov = [9]float64{500 * 500, 0, 0, 0, 500 * 500, 0, 0, 0, 500 * 500}
	}
	R := fr.CovToENU(cov)
	copy(ms.R[:9], R[:])
}

// Fix applies a solver fix (§9.5): it starts tentative hypotheses for an
// unknown ICAO (one per branch of an ambiguous 3+alt fix), confirms them,
// and updates existing tracks with the branch that gates best.
func (t *Tracker) Fix(f *Fix, out *Estimate) Result {
	t.stats.Fixes++
	res := t.fix(f)
	t.fill(f.ICAO, out)
	return res
}

func (t *Tracker) fix(f *Fix) Result {
	nh := f.NHyp
	if nh <= 0 || nh > 2 {
		return ResIgnored
	}
	for b := range nh {
		p := f.Hyp[b].ECEF
		if math.IsNaN(p.X+p.Y+p.Z) || p.Norm() < 6e6 {
			return ResIgnored
		}
	}
	e := t.entries[f.ICAO]
	if e != nil && !t.expire(e, f.TNs) {
		e = nil
	}
	if e == nil {
		e = t.newEntry(f.ICAO)
		for b := range nh {
			t.spawn(e, f, b)
		}
		return ResCreated
	}
	var lls, pen [maxHyp]float64
	scored := true
	accepted, late := 0, 0
	var r, best immResult
	ms := &t.ms
	for i := 0; i < e.n; i++ {
		h := e.hyps[i]
		if float64(h.tNs-f.TNs)/1e9 > t.p.MaxLagS {
			late++
			scored = false
			continue
		}
		found := false
		bestLL := math.Inf(-1)
		for b := range nh {
			buildFix(&f.Hyp[b], &h.frame, ms)
			t.imm(h, ms, f.TNs, &r)
			if r.pass && (!found || r.ll > best.ll) {
				best, found = r, true
			}
			bestLL = math.Max(bestLL, r.ll)
		}
		if !found {
			lls[i] = bestLL
			continue
		}
		lls[i] = best.ll
		if f.TNs < h.tNs {
			t.stats.OOSM++
		}
		t.commit(h, &best, f.TNs)
		accepted++
		t.notePos(h, f.TNs, f.NStations, f.DOF, false)
		if (e.n > 1 || !h.confirmed) && !t.plausible(h, nil) {
			pen[i] += violationPenalty
		}
	}
	t.settle(e, &lls, &pen, scored)
	if accepted > 0 {
		e.miss = 0
		t.stats.Applied++
		return ResApplied
	}
	if late > 0 && late == e.n {
		t.stats.TooLate++
		return ResTooLate
	}
	t.stats.Gated++
	e.miss++
	if e.miss >= t.p.RespawnMisses {
		// The track no longer explains the fixes: start competing
		// hypotheses from this one (they must out-score the old track).
		e.miss = 0
		for b := range nh {
			t.spawn(e, f, b)
			t.stats.Respawns++
		}
	}
	return ResGated
}

// spawn adds a tentative hypothesis from branch b of fix f, replacing the
// worst-scoring one when the entry is full.
func (t *Tracker) spawn(e *entry, f *Fix, b int) {
	if e.n >= t.p.MaxHyp {
		wi := 0
		for i := 1; i < e.n; i++ {
			if e.hyps[i].score < e.hyps[wi].score {
				wi = i
			}
		}
		t.freeH = append(t.freeH, e.hyps[wi])
		e.n--
		e.hyps[wi] = e.hyps[e.n]
		e.hyps[e.n] = nil
	}
	h := t.newHyp()
	fh := &f.Hyp[b]
	llh := geo.FromECEF(fh.ECEF)
	h.frame = geo.NewFrame(geo.LLH{Lat: llh.Lat, Lon: llh.Lon})
	cov := fh.Cov
	if !validCov(&cov) {
		cov = [9]float64{500 * 500, 0, 0, 0, 500 * 500, 0, 0, 0, 500 * 500}
	}
	R := h.frame.CovToENU(cov)
	pe := h.frame.ToENU(fh.ECEF)
	var x vec
	x[iE], x[iN], x[iU] = pe.X, pe.Y, pe.Z
	var P mat
	for a := range 3 {
		for c := range 3 {
			P[a*nx+c] = R[a*3+c]
		}
	}
	P[iVE*nx+iVE] = t.p.InitSigmaVelH * t.p.InitSigmaVelH
	P[iVN*nx+iVN] = t.p.InitSigmaVelH * t.p.InitSigmaVelH
	P[iVU*nx+iVU] = t.p.InitSigmaVelU * t.p.InitSigmaVelU
	P[iW*nx+iW] = t.p.InitSigmaOmega * t.p.InitSigmaOmega
	for j := range nModes {
		h.m[j] = modelState{x: x, P: P}
	}
	h.mu = [nModes]float64{1 - t.p.InitPCT, t.p.InitPCT}
	h.tNs, h.createdNs, h.lastNs, h.posNs = f.TNs, f.TNs, f.TNs, f.TNs
	h.posUpd = 1
	h.updates = 1
	h.lastN, h.lastDOF, h.maxDOF, h.dofNs = f.NStations, f.DOF, f.DOF, f.TNs
	best := math.Inf(-1)
	for i := 0; i < e.n; i++ {
		best = math.Max(best, e.hyps[i].score)
	}
	if e.n == 0 {
		best = 0
	}
	h.score = best
	if t.p.ConfirmUpdates <= 1 {
		h.confirmed = true
		t.stats.Confirmations++
	}
	e.hyps[e.n] = h
	e.n++
	t.stats.Created++
}

// Velocity applies a velocity measurement to every hypothesis of the ICAO.
func (t *Tracker) Velocity(v *Velocity, out *Estimate) Result {
	res := ResNoTrack
	if e := t.entries[v.ICAO]; e != nil && v.SigmaH > 0 {
		ms := &t.ms
		ms.kind, ms.nst, ms.alt = kVel, 0, false
		ms.velUp = v.SigmaUp > 0
		ms.rows = 2
		if ms.velUp {
			ms.rows = 3
		}
		rows := ms.rows
		clear(ms.R[:rows*rows])
		ms.z[0], ms.z[1] = v.East, v.North
		ms.R[0], ms.R[rows+1] = v.SigmaH*v.SigmaH, v.SigmaH*v.SigmaH
		if ms.velUp {
			ms.z[2] = v.Up
			ms.R[8] = v.SigmaUp * v.SigmaUp
		}
		res = t.simple(e, ms, v.TNs)
	}
	t.fill(v.ICAO, out)
	return res
}

// Altitude applies a Mode S altitude (HAE) to every hypothesis of the ICAO.
func (t *Tracker) Altitude(a *Altitude, out *Estimate) Result {
	res := ResNoTrack
	if e := t.entries[a.ICAO]; e != nil && a.Sigma > 0 && !math.IsNaN(a.HAE) {
		ms := &t.ms
		ms.kind, ms.nst, ms.alt, ms.rows = kAlt, 0, true, 1
		ms.z[0] = a.HAE
		ms.R[0] = a.Sigma * a.Sigma
		res = t.simple(e, ms, a.TNs)
	}
	t.fill(a.ICAO, out)
	return res
}

// simple applies a frame-independent measurement to every hypothesis.
func (t *Tracker) simple(e *entry, ms *meas, tNs int64) Result {
	var lls, pen [maxHyp]float64
	scored := true
	accepted := 0
	var r immResult
	for i := 0; i < e.n; i++ {
		h := e.hyps[i]
		if float64(h.tNs-tNs)/1e9 > t.p.MaxLagS {
			scored = false
			continue
		}
		t.imm(h, ms, tNs, &r)
		lls[i] = r.ll
		if r.pass {
			if tNs < h.tNs {
				t.stats.OOSM++
			}
			t.commit(h, &r, tNs)
			accepted++
			if (e.n > 1 || !h.confirmed) && !t.plausible(h, nil) {
				pen[i] += violationPenalty
			}
		}
	}
	t.settle(e, &lls, &pen, scored)
	if accepted > 0 {
		t.stats.Applied++
		return ResApplied
	}
	t.stats.Gated++
	return ResGated
}

// fill writes the current estimate of icao into out (if non-nil).
func (t *Tracker) fill(icao uint32, out *Estimate) {
	if out == nil {
		return
	}
	e := t.entries[icao]
	if e == nil || e.n == 0 {
		*out = Estimate{ICAO: icao}
		return
	}
	h := e.best()
	t.estimate(e, h, &h.hypState, out)
}

// Get returns the current estimate of icao at its last update time.
func (t *Tracker) Get(icao uint32, out *Estimate) bool {
	t.fill(icao, out)
	return out.Status != StatusNone
}

// Predict returns the estimate of icao predicted to tNs (not before the
// track's own time). The track is not modified.
func (t *Tracker) Predict(icao uint32, tNs int64, out *Estimate) bool {
	e := t.entries[icao]
	if e == nil || e.n == 0 {
		*out = Estimate{ICAO: icao}
		return false
	}
	h := e.best()
	var s hypState
	if dt := float64(tNs-h.tNs) / 1e9; dt > 0 {
		t.mixPredict(&h.hypState, dt, t.gapQ(h, tNs), &s)
	} else {
		s = h.hypState
	}
	t.estimate(e, h, &s, out)
	return true
}

// Sweep deletes tracks that coasted longer than CoastS, tentative
// hypotheses past the confirmation window, and confirmed hypotheses whose
// predicted horizontal σ at nowNs exceeds MaxSigmaHM. Entries are visited
// in ICAO order. emit (optional) receives the
// prediction to nowNs of every remaining confirmed track (the 1 Hz output
// of §9.5); the Estimate is reused between calls.
func (t *Tracker) Sweep(nowNs int64, emit func(*Estimate)) {
	est := &t.sweepEst
	var preds [maxHyp]hypState
	var owner [maxHyp]*hyp
	// ICAO order, not map order: emit sees the tracks in a reproducible
	// order (bit-reproducible runs, H-track). The key slice is reused.
	t.keys = t.keys[:0]
	for k := range t.entries {
		t.keys = append(t.keys, k)
	}
	slices.Sort(t.keys)
	for _, k := range t.keys {
		e := t.entries[k]
		if !t.expire(e, nowNs) {
			continue
		}
		for i := e.n - 1; i >= 0; i-- {
			h := e.hyps[i]
			s := &preds[i]
			if dt := float64(nowNs-h.tNs) / 1e9; dt > 0 {
				t.mixPredict(&h.hypState, dt, t.gapQ(h, nowNs), s)
			} else {
				*s = h.hypState
			}
			owner[i] = h
			_, P := combined(s)
			// Only confirmed hypotheses are dropped for their σ: a tentative one
			// is bounded by the confirmation window, and the far branch of a
			// 3+alt pair (σh often several km) must keep competing until the
			// evidence decides - dropped for its σ, it left the near, wrong
			// branch to be confirmed by default (H-track, live-like netsim).
			if h.confirmed && math.Sqrt(math.Max(P[iE*nx+iE]+P[iN*nx+iN], 0)) > t.p.MaxSigmaHM {
				owner[i] = nil
				t.remove(e, i)
			}
		}
		if e.n == 0 || emit == nil {
			continue
		}
		b := e.best()
		if !b.confirmed {
			continue
		}
		for i := range maxHyp {
			if owner[i] == b {
				t.estimate(e, b, &preds[i], est)
				emit(est)
				break
			}
		}
	}
}

// Delete drops the track of icao (e.g. when ADS-B positions take over).
func (t *Tracker) Delete(icao uint32) {
	if e := t.entries[icao]; e != nil {
		for e.n > 0 {
			t.remove(e, e.n-1)
		}
	}
}

// estimate converts hypothesis state s of h into out.
func (t *Tracker) estimate(e *entry, h *hyp, s *hypState, out *Estimate) {
	x, P := combined(s)
	fr := &h.frame
	p := fr.FromENU(geo.Vec3{X: x[iE], Y: x[iN], Z: x[iU]})
	llh := geo.FromECEF(p)
	var pc [9]float64
	for a := range 3 {
		for b := range 3 {
			pc[a*3+b] = P[a*nx+b]
		}
	}
	*out = Estimate{ICAO: e.icao, TNs: s.tNs, ECEF: p, LLH: llh, Cov: fr.CovToECEF(pc)}
	// local ENU at the aircraft: M = R_loc·R_frameᵀ
	lf := geo.NewFrame(llh)
	var M [9]float64
	for c, u := range [3]geo.Vec3{{X: 1}, {Y: 1}, {Z: 1}} {
		v := lf.Rotate(fr.Unrotate(u))
		M[0*3+c], M[1*3+c], M[2*3+c] = v.X, v.Y, v.Z
	}
	var pv [9]float64
	for a := range 3 {
		out.VelENU[a] = M[a*3]*x[iVE] + M[a*3+1]*x[iVN] + M[a*3+2]*x[iVU]
		for b := range 3 {
			pv[a*3+b] = P[(iVE+a)*nx+iVE+b]
		}
	}
	out.VelCov = cong3(&M, &pv)
	pl := cong3(&M, &pc)
	out.SigmaH = linalg.HorizontalSigma(pl[0], pl[4])
	out.SigmaV = math.Sqrt(math.Max(pl[8], 0))
	out.Ellipse = linalg.ErrorEllipse(pl[0], pl[1], pl[4])
	out.TurnRate = x[iW]
	out.PTurn = s.mu[modeCT]
	out.Status = StatusTentative
	if h.confirmed {
		out.Status = StatusConfirmed
	}
	out.Hypotheses = e.n
	out.Ambiguous = e.n > 1
	out.Updates = h.updates
	out.LastStations, out.LastDOF, out.AltUsed = h.lastN, h.lastDOF, h.altUsed
	out.MaxDOF = h.lastDOF
	if s.tNs-h.dofNs <= dofWindowNs {
		out.MaxDOF = max(h.maxDOF, h.lastDOF)
	}
	out.LastUpdateNs = h.lastNs
}

// cong3 returns M·P·Mᵀ for 3×3 row-major matrices.
func cong3(M, P *[9]float64) (c [9]float64) {
	var t [9]float64
	for i := range 3 {
		for j := range 3 {
			t[i*3+j] = M[i*3]*P[j] + M[i*3+1]*P[3+j] + M[i*3+2]*P[6+j]
		}
	}
	for i := range 3 {
		for j := range 3 {
			c[i*3+j] = t[i*3]*M[j*3] + t[i*3+1]*M[j*3+1] + t[i*3+2]*M[j*3+2]
		}
	}
	return
}
