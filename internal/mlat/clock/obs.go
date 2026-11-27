package clock

import (
	"math"
	"slices"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
	"airview/internal/model"
)

// c in metres per nanosecond.
const cMPerNs = 0.299792458

// workObs is one admitted sync observation of the epoch, validated against
// the station table.
type workObs struct {
	key    uint32
	short  bool
	hostS  float64
	pos    geo.Vec3
	cov    [6]float64
	m0, m1 int32 // range in Filter.wm
}

// workMember is one validated member: the station, its local time y (s
// since the origin, frame reference removed) and the propagation time ρ/c
// from the transmitter (ns).
type workMember struct {
	st  *station
	y   float64
	rho float64
	u   geo.Vec3 // unit vector transmitter -> station
}

// memRec is one member of an accepted group, as needed for the
// information accumulation.
type memRec struct {
	st   *station
	off  int32 // block offset in the component, -1 = external member
	dim  int8
	h    [4]float64 // H row over the block (layout order)
	dinv float64    // 1/D_ii
	e    float64    // innovation (ns)
	z    [4]float64 // low-rank factors: Wp = D⁻¹ − Σ_k z_k z_kᵀ
	ext  int32      // external slot (c.xs), -1 = none
}

type grpRec struct {
	m0, m1 int32
	ze     [4]float64 // z_kᵀ e
	scale  float64
}

type arena struct {
	grp []grpRec
	mem []memRec
}

func (a *arena) reset() { a.grp, a.mem = a.grp[:0], a.mem[:0] }

// groupWork is the per-component scratch space of processGroup.
type groupWork struct {
	mm      []gm
	act     []int
	healthy []bool
	S       []float64
	Si      []float64
	u, w    []float64
	spe     []float64
	touch   []touched
}

// gm is one member during processing.
type gm struct {
	st    *station
	ext   *StationClock
	off   int // block offset (internal) or -1
	y     float64
	rho   float64
	ut    geo.Vec3
	varTh float64 // var θ at the epoch (ns²)
	t     float64 // arrival time estimate (s since origin)
	dl    float64 // t − tE (s)
	h     [4]float64
	dim   int
	e     float64
	d     float64 // D_ii (ns²)
	a     float64 // τ coefficient 1 + φ̂
	v     [3]float64
	tStat float64 // studentised innovation
	loo   float64 // leave-one-out residual (ns)
	vext  float64 // external member: inflated prediction variance at the epoch (ns²)
}

type touched struct {
	idx int
	q   [4]float64
}

func (w *groupWork) ensure(m int) {
	if cap(w.mm) < m {
		w.mm = make([]gm, m)
		w.act = make([]int, m)
		w.S = make([]float64, m*m)
		w.Si = make([]float64, m*m)
		w.u = make([]float64, m)
		w.w = make([]float64, m)
		w.spe = make([]float64, m)
	}
	w.mm = w.mm[:m]
}

// thetaInternal returns θ(t_e + dl) + β·short and its h row for a block.
func thetaInternal(c *component, s *station, dl float64, short bool, h *[4]float64) (pred float64) {
	x := c.x[s.off : s.off+s.dim]
	for i := range h {
		h[i] = 0
	}
	h[s.layout[kTheta]] = 1
	pred = x[s.layout[kTheta]]
	if s.mode == modeFree {
		h[s.layout[kPhi]] = dl
		pred += x[s.layout[kPhi]] * dl
		if s.psi {
			h[s.layout[kPsi]] = dl * dl / 2
			pred += x[s.layout[kPsi]] * dl * dl / 2
		}
	}
	if short {
		h[s.layout[kBeta]] = 1
		pred += x[s.layout[kBeta]]
	}
	return pred
}

// processGroup gates one sync observation for component c and, when
// accepted, records it in c.ar. prev is the previous snapshot (external
// members of other partitions of the same time domain).
func (f *Filter) processGroup(c *component, wo *workObs, prev *Snapshot) {
	cfg := &f.cfg
	w := &c.gw
	all := f.wm[wo.m0:wo.m1]
	w.ensure(len(all))
	m := 0
	internal := 0
	tE := c.tE
	for i := range all {
		wm := &all[i]
		s := wm.st
		g := gm{st: s, y: wm.y, rho: wm.rho, ut: wm.u, off: -1}
		switch {
		case s.comp == c:
			g.off = s.off
			g.dim = s.dim
			it := s.off + int(s.layout[kTheta])
			g.varTh = c.P[it*c.n+it]
			internal++
		case s.comp != nil && s.comp.domain == c.domain && prev != nil:
			sc := prev.Station(s.idx)
			if sc == nil || !sc.Synced || sc.Epoch != s.epoch || sc.Domain != c.domain {
				continue
			}
			g.ext = sc
			g.varTh = sc.Cov[0][0] * cfg.ExternalInflate
		default:
			continue
		}
		w.mm[m] = g
		m++
	}
	if internal == 0 || m < 2 {
		return
	}
	mm := w.mm[:m]
	if m > cfg.MaxMembers {
		slices.SortFunc(mm, func(a, b gm) int {
			// internal unsynced members are worth keeping only when few
			switch {
			case a.varTh < b.varTh:
				return -1
			case a.varTh > b.varTh:
				return 1
			}
			return 0
		})
		mm = mm[:cfg.MaxMembers]
		m = len(mm)
		internal = 0
		for i := range mm {
			if mm[i].off >= 0 {
				internal++
			}
		}
		if internal == 0 {
			return
		}
	}
	// Reference member: the most precisely known clock defines τ̂.
	best := 0
	for i := range mm {
		if mm[i].varTh < mm[best].varTh {
			best = i
		}
	}
	short := wo.short
	thetaAt := func(g *gm, t float64) (pred, vpred float64, h [4]float64) {
		if g.off >= 0 {
			dl := t - tE
			pred = thetaInternal(c, g.st, dl, short, &h)
			return pred, 0, h
		}
		pred, vpred = g.ext.offsetAt(t, short)
		return pred, vpred * cfg.ExternalInflate, h
	}
	b := &mm[best]
	// t_b = y_b − θ_b(t_b): two fixed-point iterations (|φ| ≪ 1).
	tb := b.y
	for it := 0; it < 2; it++ {
		p, _, _ := thetaAt(b, tb)
		tb = b.y - p*1e-9
	}
	tau := tb - b.rho*1e-9
	varTau := b.varTh
	// Position covariance factor (ECEF, m): L_p with Σ_pos = L_p·L_pᵀ.
	var lp [9]float64
	hasPos := false
	if cv := wo.cov; cv[0] > 0 || cv[3] > 0 || cv[5] > 0 {
		m3 := linalg.Mat3{cv[0], cv[1], cv[2], cv[1], cv[3], cv[4], cv[2], cv[4], cv[5]}
		if l, err := m3.Chol(); err == nil {
			lp = l
			hasPos = true
		}
	}
	for i := range mm {
		g := &mm[i]
		g.t = tau + g.rho*1e-9
		g.dl = g.t - tE
		pred, vpred, h := thetaAt(g, g.t)
		g.h = h
		g.e = (g.y-g.t)*1e9 - pred
		s := g.st
		j := s.jitter()
		g.d = j*j + vpred
		if ps := s.info.PosSigmaM - f.cfg.PosSigmaFreeM; ps > 0 {
			// doubtful station position (MLAT-R4): its range error is
			// common to the station's frames, not white, but at this size
			// the station must not be trusted by its peers
			g.d += (ps / cMPerNs) * (ps / cMPerNs)
		}
		phiHat, varPhi := 0.0, 0.0
		if g.off >= 0 {
			// influence cap (DESIGN §9.2.4): a station that contributed more
			// than InfluenceCap × the component median last epoch has its
			// measurement variance inflated by the excess
			g.d *= s.capInfl
			g.d += s.procVar(g.dl)
			if s.cls == model.ClockGPSUTC || s.cls == model.ClockGPSGPST {
				// 1PPS error: common to the station's frames of one second
				pc := f.cfg.PPSCorrelatedNs
				g.d += pc * pc * math.Max(1, s.rate)
			}
			if s.layout[kPhi] >= 0 {
				ip := s.off + int(s.layout[kPhi])
				phiHat, varPhi = c.x[ip], c.P[ip*c.n+ip]
			}
		} else {
			phiHat, varPhi = g.ext.Phi, g.ext.Cov[1][1]*cfg.ExternalInflate
			g.vext = 0
			if c.xs.kcap > 0 {
				// the part of vpred shared by the epoch's groups (external.go)
				g.vext = math.Min(extShared(g, tE, short, cfg.ExternalInflate), vpred)
			}
		}
		if i != best {
			// bilinear term δτ·δφ_i of the time argument
			g.d += varTau * varPhi * 1e-18
		}
		g.a = 1 + phiHat*1e-9
		if hasPos {
			// d(ρ/c)/dp = u/c; V row = uᵀ·L_p / c (ns)
			u := g.ut
			for k := 0; k < 3; k++ {
				g.v[k] = (u.X*lp[0*3+k] + u.Y*lp[1*3+k] + u.Z*lp[2*3+k]) / cMPerNs
			}
		} else {
			g.v = [3]float64{}
		}
	}
	// Failure attribution (finding ROB-2): members of stations that are
	// themselves outlier-prone do not count towards the aircraft's
	// failure, so a few noisy (or hostile) stations cannot blacklist every
	// aircraft they hear. m0 counts the healthy members.
	m0 := 0
	healthy := w.healthy[:0]
	for i := range mm {
		h := mm[i].st == nil || mm[i].st.outEWMA < f.cfg.StationBadOutlier
		healthy = append(healthy, h)
		if h {
			m0++
		}
	}
	w.healthy = healthy
	act := w.act[:0]
	for i := range mm {
		act = append(act, i)
	}
	dropped := 0
	accepted := false
	for len(act) >= 2 {
		ok, worst := f.gate(c, w, mm, act)
		if ok {
			accepted = true
			break
		}
		if len(act) == 2 {
			break
		}
		// drop the member with the largest studentised innovation
		g := &mm[act[worst]]
		if g.off >= 0 {
			f.noteOutlier(g.st, g.tStat, g.loo, g.t)
		}
		if healthy[act[worst]] {
			dropped++
		}
		act = slices.Delete(act, worst, worst+1)
	}
	if f.acftTrack {
		c.acRes = append(c.acRes, acftResult{key: wo.key, fail: m0 >= 4 && (!accepted || dropped >= 2)})
	}
	if !accepted {
		c.nRej++
		return
	}
	c.nAcc++
	f.recordAccepted(c, w, mm, act, best, wo.short)
}

// gate computes the innovation covariance S = H·P⁻·Hᵀ + D + V·Vᵀ of the
// active members, eliminates the transmit time (τ column a) and tests the
// normalised innovation squared and the studentised innovations. It
// returns whether the set passes and the position (in act) of the worst
// member. Each member's tStat and loo are filled.
func (f *Filter) gate(c *component, w *groupWork, mm []gm, act []int) (bool, int) {
	cfg := &f.cfg
	m := len(act)
	S := w.S[:m*m]
	n := c.n
	for a := 0; a < m; a++ {
		ga := &mm[act[a]]
		for b := 0; b <= a; b++ {
			gb := &mm[act[b]]
			v := ga.v[0]*gb.v[0] + ga.v[1]*gb.v[1] + ga.v[2]*gb.v[2]
			if ga.off >= 0 && gb.off >= 0 {
				for p := 0; p < ga.dim; p++ {
					hp := ga.h[p]
					if hp == 0 {
						continue
					}
					row := c.P[(ga.off+p)*n+gb.off:]
					for q := 0; q < gb.dim; q++ {
						v += hp * row[q] * gb.h[q]
					}
				}
			}
			if a == b {
				v += ga.d
			}
			S[a*m+b] = v
			S[b*m+a] = v
		}
	}
	if err := linalg.Cholesky(S, m); err != nil {
		return false, 0
	}
	u, we := w.u[:m], w.w[:m]
	for a := 0; a < m; a++ {
		u[a] = mm[act[a]].a
		we[a] = mm[act[a]].e
	}
	linalg.CholSolve(S, m, u)  // u = S⁻¹a
	linalg.CholSolve(S, m, we) // w = S⁻¹e
	ata, atw, etw := 0.0, 0.0, 0.0
	for a := 0; a < m; a++ {
		ga := &mm[act[a]]
		ata += ga.a * u[a]
		atw += ga.a * we[a]
		etw += ga.e * we[a]
	}
	if !(ata > 0) {
		return false, 0
	}
	nis := etw - atw*atw/ata
	// diag(Sp) needs S⁻¹: invert the factor (m ≤ MaxMembers)
	Si := w.Si[:m*m]
	copy(Si, S)
	if err := linalg.CholToInverse(Si, m, w.spe[:m]); err != nil {
		return false, 0
	}
	worst, wmax := 0, -1.0
	for a := 0; a < m; a++ {
		ga := &mm[act[a]]
		spe := we[a] - u[a]*atw/ata
		spii := Si[a*m+a] - u[a]*u[a]/ata
		if !(spii > 0) {
			spii = 1e-300
		}
		ga.tStat = spe / math.Sqrt(spii)
		ga.loo = spe / spii
		if t := math.Abs(ga.tStat); t > wmax {
			wmax, worst = t, a
		}
	}
	thr := chi2Quantile(cfg.GateProb, m-1)
	return nis <= thr && wmax <= cfg.WTestMax, worst
}

// noteOutlier updates a station's step streak with a rejected member that
// arrived at network time at.
func (f *Filter) noteOutlier(s *station, t, loo, at float64) {
	s.nOut++
	s.outEWMA += 0.02 * (1 - s.outEWMA)
	if math.Abs(t) <= f.cfg.StepSigma {
		return
	}
	s.streak(t, loo, at)
	s.suspect++
}

// streak extends (or restarts) the run of same-sign large innovations; at
// is the member's arrival time (network seconds).
func (s *station) streak(t, loo, at float64) {
	sg := int8(1)
	if t < 0 {
		sg = -1
	}
	if s.streakSign == sg {
		s.streakN++
		s.streakSum += loo
	} else {
		s.streakSign, s.streakN, s.streakSum = sg, 1, loo
	}
	k := (s.streakN - 1) % streakCap
	s.streakT[k], s.streakR[k] = at, loo
}

// stepEstimate splits the residual run that declared a step into an
// offset and a frequency part: the least-squares line r = a + b·(t − t̄)
// through the run's leave-one-out residuals. It returns the offset at tE
// (ns), the slope b (ppb = ns/s) and its standard error. With fewer than
// four members, or members spread over less than half a second, the slope
// is not determined (se = +Inf) and the offset is the mean residual.
func (s *station) stepEstimate(tE float64) (off, slope, se float64) {
	n := min(s.streakN, streakCap)
	mean := s.streakSum / float64(max(s.streakN, 1))
	if n < 4 {
		return mean, 0, math.Inf(1)
	}
	var mt, mr float64
	lo, hi := math.Inf(1), math.Inf(-1)
	for i := 0; i < n; i++ {
		mt += s.streakT[i]
		mr += s.streakR[i]
		lo, hi = math.Min(lo, s.streakT[i]), math.Max(hi, s.streakT[i])
	}
	if hi-lo < 0.5 {
		return mean, 0, math.Inf(1)
	}
	mt /= float64(n)
	mr /= float64(n)
	var stt, str float64
	for i := 0; i < n; i++ {
		dt := s.streakT[i] - mt
		stt += dt * dt
		str += dt * (s.streakR[i] - mr)
	}
	slope = str / stt
	var rss float64
	for i := 0; i < n; i++ {
		r := s.streakR[i] - mr - slope*(s.streakT[i]-mt)
		rss += r * r
	}
	se = math.Sqrt(rss / float64(n-2) / stt)
	return mr + slope*(tE-mt), slope, se
}

// recordAccepted computes the measurement-only weight of an accepted group,
// Wp = R⁻¹ − R⁻¹a·aᵀR⁻¹/(aᵀR⁻¹a) with R = D + V·Vᵀ (Woodbury), stores it
// in low-rank form for the accumulation and updates the member statistics.
func (f *Filter) recordAccepted(c *component, w *groupWork, mm []gm, act []int, best int, short bool) {
	m := len(act)
	// External members with a slot: their shared prediction error is a
	// state of the epoch (external.go), so D keeps only the white part.
	var slots [64]int32
	for a := 0; a < m; a++ {
		g := &mm[act[a]]
		sl := int32(-1)
		if g.off < 0 && g.vext > 0 && a < len(slots) {
			if sl = c.xs.slotFor(g.st, g.ext, short, g.vext); sl >= 0 {
				g.d = math.Max(g.d-g.vext, 1e-3*g.d)
			}
		}
		if a < len(slots) {
			slots[a] = sl
		}
	}
	// K = (I + VᵀD⁻¹V)⁻¹, Y = D⁻¹V
	var vtdv [9]float64
	for a := 0; a < m; a++ {
		g := &mm[act[a]]
		di := 1 / g.d
		for p := 0; p < 3; p++ {
			for q := 0; q < 3; q++ {
				vtdv[p*3+q] += g.v[p] * g.v[q] * di
			}
		}
	}
	km := linalg.Mat3{1 + vtdv[0], vtdv[1], vtdv[2], vtdv[3], 1 + vtdv[4], vtdv[5], vtdv[6], vtdv[7], 1 + vtdv[8]}
	kinv, err := km.InvSPD()
	if err != nil {
		kinv = linalg.Identity3()
	}
	lk, err := kinv.Chol()
	if err != nil {
		lk = linalg.Mat3{}
	}
	g0 := int32(len(c.ar.mem))
	// Yᵀa (3), then Wa = D⁻¹a − Y·K·Yᵀa
	var yta [3]float64
	for a := 0; a < m; a++ {
		g := &mm[act[a]]
		di := 1 / g.d
		for p := 0; p < 3; p++ {
			yta[p] += g.v[p] * di * g.a
		}
	}
	kyta := kinv.MulVec(linalg.Vec3(yta))
	ata := 0.0
	for a := 0; a < m; a++ {
		g := &mm[act[a]]
		di := 1 / g.d
		ata += g.a * g.a * di
	}
	ata -= yta[0]*kyta[0] + yta[1]*kyta[1] + yta[2]*kyta[2]
	if !(ata > 0) {
		return
	}
	sq := 1 / math.Sqrt(ata)
	var ze [4]float64
	for a := 0; a < m; a++ {
		g := &mm[act[a]]
		di := 1 / g.d
		r := memRec{st: g.st, off: int32(g.off), dim: int8(g.dim), h: g.h, dinv: di, e: g.e, ext: -1}
		if g.off < 0 && a < len(slots) {
			r.ext = slots[a]
		}
		// y_i = D⁻¹ v_i (row of Y); z_k = (Y·L_K)_k
		for k := 0; k < 3; k++ {
			r.z[k] = di * (g.v[0]*lk[0*3+k] + g.v[1]*lk[1*3+k] + g.v[2]*lk[2*3+k])
		}
		wa := di*g.a - di*(g.v[0]*kyta[0]+g.v[1]*kyta[1]+g.v[2]*kyta[2])
		r.z[3] = wa * sq
		for k := 0; k < 4; k++ {
			ze[k] += r.z[k] * g.e
		}
		if g.off < 0 {
			r.off = -1
		} else {
			s := g.st
			// effective observation count: weight relative to the
			// member's own noise (≈ 1 − 1/m per member)
			wpii := di - r.z[0]*r.z[0] - r.z[1]*r.z[1] - r.z[2]*r.z[2] - r.z[3]*r.z[3]
			if wpii > 0 {
				s.influ += wpii * g.d
			}
			s.epAcc++
			s.nAcc++
			s.outEWMA -= 0.02 * s.outEWMA
			s.adW2 += g.tStat * g.tStat
			s.adWN++
			s.looSum += g.loo
			s.looN++
			if math.Abs(g.tStat) > f.cfg.StepSigma {
				s.streak(g.tStat, g.loo, g.t)
			} else {
				s.streakN, s.streakSign, s.streakSum, s.suspect = 0, 0, 0, 0
			}
			if act[a] != best && mm[best].st != nil {
				c.peerRes = append(c.peerRes, peerResult{a: s.idx, b: mm[best].st.idx, r: g.loo})
			}
		}
		c.ar.mem = append(c.ar.mem, r)
	}
	c.ar.grp = append(c.ar.grp, grpRec{m0: g0, m1: int32(len(c.ar.mem)), ze: ze, scale: 1})
}

// accumulate adds the accepted groups to J and g.
func (f *Filter) accumulate(c *component) {
	n := c.n
	J, g := c.J, c.g
	xs := &c.xs
	for gi := range c.ar.grp {
		gr := &c.ar.grp[gi]
		mem := c.ar.mem[gr.m0:gr.m1]
		sc := gr.scale
		tch := c.gw.touch[:0]
		for i := range mem {
			r := &mem[i]
			if r.off < 0 {
				if r.ext < 0 {
					continue
				}
				// external state (H = 1): index n + slot
				e := int(r.ext)
				xs.jee[e*xs.kcap+e] += sc * r.dinv
				xs.ge[e] += sc * r.dinv * r.e
				tch = append(tch, touched{idx: n + e, q: r.z})
				continue
			}
			off := int(r.off)
			for p := 0; p < int(r.dim); p++ {
				hp := r.h[p]
				if hp == 0 {
					continue
				}
				// diagonal part D⁻¹
				row := J[(off+p)*n+off:]
				for q := 0; q < int(r.dim); q++ {
					row[q] += sc * r.dinv * hp * r.h[q]
				}
				g[off+p] += sc * r.dinv * hp * r.e
				tch = append(tch, touched{idx: off + p, q: [4]float64{r.z[0] * hp, r.z[1] * hp, r.z[2] * hp, r.z[3] * hp}})
			}
		}
		// low-rank part −Σ_k (Hᵀz_k)(Hᵀz_k)ᵀ
		for a := range tch {
			ta := &tch[a]
			gq := sc * (ta.q[0]*gr.ze[0] + ta.q[1]*gr.ze[1] + ta.q[2]*gr.ze[2] + ta.q[3]*gr.ze[3])
			if ta.idx >= n {
				// external row: J_ee (J_ex is the transpose of J_xe)
				e := ta.idx - n
				for b := range tch {
					tb := &tch[b]
					if tb.idx >= n {
						xs.jee[e*xs.kcap+tb.idx-n] -= sc * (ta.q[0]*tb.q[0] + ta.q[1]*tb.q[1] + ta.q[2]*tb.q[2] + ta.q[3]*tb.q[3])
					}
				}
				xs.ge[e] -= gq
				continue
			}
			row := J[ta.idx*n:]
			for b := range tch {
				tb := &tch[b]
				v := sc * (ta.q[0]*tb.q[0] + ta.q[1]*tb.q[1] + ta.q[2]*tb.q[2] + ta.q[3]*tb.q[3])
				if tb.idx >= n {
					xs.addXE(ta.idx, tb.idx-n, -v)
					continue
				}
				row[tb.idx] -= v
			}
			g[ta.idx] -= gq
		}
		c.gw.touch = tch
	}
}

// frameRef returns the frame reference (ns) of a station for a frame kind.
func (f *Filter) frameRef(cls model.ClockClass, short bool) float64 {
	if short {
		return f.cfg.FrameRefNs(cls, model.FrameModeSShort)
	}
	return f.cfg.FrameRefNs(cls, model.FrameModeSLong)
}
