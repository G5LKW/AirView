package track

import (
	"math"
	"slices"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Mode A/C target tracking (DESIGN §9.9, phase 5). Mode A/C replies carry no
// address, so a target is known only by where it is and which codes it
// sends. ACTracker associates Mode A/C measurements (solver fixes of ≥ 4
// stations and TOA clusters of 2-3 stations) with targets that get
// synthetic identities (ACIDFlag | sequence), and runs the ordinary IMM
// Tracker underneath for the kinematics. See doc.go §9.

// ACIDFlag marks a synthetic Mode A/C target identity. It lies above the
// 24-bit ICAO address and the non-ICAO flag (bit 24), so it can never be
// confused with an address key.
const ACIDFlag uint32 = 1 << 25

// IsACID reports whether key is a synthetic Mode A/C identity.
func IsACID(key uint32) bool { return key&ACIDFlag != 0 }

// maxACCodes is the number of distinct codes remembered per target (one
// identity plus the recent Mode C altitude codes of a climbing target).
const maxACCodes = 6

// ACParams configures an ACTracker. The zero value of every field selects
// the documented default; Normalize clamps the rest.
type ACParams struct {
	// Track is the kinematic filter. Zero fields take the track defaults,
	// except the lifecycle, which defaults for the bursty update pattern of
	// SSR replies (one dwell of a few dozen replies per antenna sweep of
	// 4-12 s): ConfirmWindowS 30, ConfirmUpdates 3, CoastS 30.
	Track Params
	// GateChi2 is the association gate on the squared Mahalanobis distance
	// between a fix and a target's prediction (3 degrees of freedom; 25 ≈
	// 1.5·10⁻⁵ miss probability).
	GateChi2 float64
	// CodePenalty is added to the association cost of a target that has not
	// sent the fix's code yet (a target sends at least two codes: identity
	// and altitude).
	CodePenalty float64
	// AltGateM: a new code may join a target that already has two codes
	// only when it decodes as a Gillham altitude within AltGateFt of the
	// target's altitude code or within AltGateM of the target's height
	// (pressure vs geometric height: QNH, temperature and geoid differ by
	// hundreds of metres).
	AltGateM  float64
	AltGateFt float64
	// ConfirmFixes, ConfirmBursts and ConfirmSpanS: a target is shown only
	// once it collected this many fixes, in at least this many separate
	// bursts (replies more than BurstGapS apart), spanning at least
	// ConfirmSpanS. A chance coincidence of replies (FRUIT, shared codes)
	// does not repeat at a consistent position over several sweeps.
	ConfirmFixes  int
	ConfirmBursts int
	ConfirmSpanS  float64
	BurstGapS     float64
	// MaxTargets bounds the number of targets (tentative ones included).
	MaxTargets int
	// CodeTTLS: a code not seen for this long is forgotten (the target
	// keeps at least one).
	CodeTTLS float64
	// CrowdM: a shown target lies within CrowdM of another shown target
	// with the same identity code is graded one class worse (association
	// by position alone).
	CrowdM float64
	// SpawnMinDOF: only an unambiguous fix with at least this many degrees
	// of freedom (stations − 4) starts a target (default 0: any).
	// StrongDOF, WeakConfirmFixes, WeakConfirmBursts: a 4-station fix has
	// no redundancy (and a mirror solution that may move as smoothly as the
	// aircraft); a target that never had a fix with at least StrongDOF
	// degrees of freedom is shown only after WeakConfirmFixes fixes in
	// WeakConfirmBursts bursts.
	SpawnMinDOF       int
	StrongDOF         int
	WeakConfirmFixes  int
	WeakConfirmBursts int
	// OutlierChi2: a fix that gates no target but lies within this squared
	// Mahalanobis distance of a shown target whose codes it fits is taken as
	// that target's outlier (fix errors have heavier tails than their
	// covariance) and does not start a duplicate target.
	OutlierChi2 float64
	// NewCodeWindowS: a target that has sent only one code accepts a
	// second one only from a fix within this time of its last update (the
	// replies of one dwell alternate between Mode A and Mode C 4-8 ms
	// apart). Later, the kinematic gate of a target with an unknown velocity
	// is too wide to tell a FRUIT coincidence from the same aircraft.
	NewCodeWindowS float64
	// Merge: two targets whose positions agree within MergeChi2 (squared
	// Mahalanobis distance, 3 DOF) and velocities within MergeVelMS for
	// MergeSweeps consecutive sweeps are one aircraft (its Mode A and Mode C
	// replies started separate targets, or a target was restarted); the one
	// with fewer fixes is merged into the other.
	MergeChi2   float64
	MergeVelMS  float64
	MergeSweeps int
	// ClusterSlackM is added (as a 1σ) to the predicted position when a
	// 2-3 station cluster is matched against the targets with its code.
	ClusterSlackM float64
}

// DefaultACParams returns the defaults.
func DefaultACParams() ACParams {
	tp := Params{ConfirmWindowS: 30, ConfirmUpdates: 3, CoastS: 30}.Normalize()
	return ACParams{Track: tp, GateChi2: 25, CodePenalty: 6, AltGateM: 1000, AltGateFt: 1000,
		ConfirmFixes: 3, ConfirmBursts: 2, ConfirmSpanS: 2, BurstGapS: 1,
		MaxTargets: 20000, CodeTTLS: 120, CrowdM: 5000, ClusterSlackM: 50,
		SpawnMinDOF: 0, StrongDOF: 1, WeakConfirmFixes: 12, WeakConfirmBursts: 4, OutlierChi2: 400, NewCodeWindowS: 0.5, MergeChi2: 14, MergeVelMS: 25, MergeSweeps: 2}
}

// Normalize fills zero fields with defaults and clamps the others.
func (p ACParams) Normalize() ACParams {
	d := DefaultACParams()
	if p.Track.ConfirmWindowS == 0 {
		p.Track.ConfirmWindowS = d.Track.ConfirmWindowS
	}
	if p.Track.ConfirmUpdates == 0 {
		p.Track.ConfirmUpdates = d.Track.ConfirmUpdates
	}
	if p.Track.CoastS == 0 {
		p.Track.CoastS = d.Track.CoastS
	}
	p.Track = p.Track.Normalize()
	f := func(v *float64, def, lo, hi float64) {
		if !(*v > 0) || math.IsInf(*v, 0) {
			*v = def
		}
		*v = math.Min(math.Max(*v, lo), hi)
	}
	f(&p.GateChi2, d.GateChi2, 4, 1000)
	f(&p.CodePenalty, d.CodePenalty, 0.01, 100)
	f(&p.AltGateM, d.AltGateM, 50, 5000)
	f(&p.AltGateFt, d.AltGateFt, 100, 10000)
	f(&p.ConfirmSpanS, d.ConfirmSpanS, 0.1, 120)
	f(&p.BurstGapS, d.BurstGapS, 0.05, 30)
	f(&p.CodeTTLS, d.CodeTTLS, 5, 3600)
	f(&p.CrowdM, d.CrowdM, 100, 100000)
	f(&p.ClusterSlackM, d.ClusterSlackM, 1, 10000)
	f(&p.NewCodeWindowS, d.NewCodeWindowS, 0.01, 30)
	f(&p.OutlierChi2, d.OutlierChi2, p.GateChi2, 1e6)
	f(&p.MergeChi2, d.MergeChi2, 1, 100)
	f(&p.MergeVelMS, d.MergeVelMS, 1, 200)
	i := func(v *int, def, lo, hi int) {
		if *v <= 0 {
			*v = def
		}
		*v = min(max(*v, lo), hi)
	}
	i(&p.ConfirmFixes, d.ConfirmFixes, 1, 100)
	i(&p.ConfirmBursts, d.ConfirmBursts, 1, 20)
	i(&p.MergeSweeps, d.MergeSweeps, 1, 60)
	p.SpawnMinDOF = min(max(p.SpawnMinDOF, 0), 10)
	i(&p.StrongDOF, d.StrongDOF, 1, 10)
	i(&p.WeakConfirmFixes, d.WeakConfirmFixes, p.ConfirmFixes, 1000)
	i(&p.WeakConfirmBursts, d.WeakConfirmBursts, p.ConfirmBursts, 100)
	i(&p.MaxTargets, d.MaxTargets, 1, 1<<22)
	return p
}

// ACResult tells what a Mode A/C measurement did.
type ACResult struct {
	// ID is the target the measurement was associated with (0: none).
	ID uint32
	// Res is the kinematic tracker's result for that target.
	Res Result
	// New is set when the measurement started a new target.
	New bool
}

// ACEstimate is the output for one shown Mode A/C target.
type ACEstimate struct {
	Estimate // Estimate.ICAO is the synthetic ID (ACIDFlag set)
	// Squawk is the raw identity code (see ACSquawk) when SquawkOK.
	Squawk   uint16
	SquawkOK bool
	// AltFt is the Mode C pressure altitude (ft) when AltOK; AltCode the
	// raw code it was decoded from.
	AltFt   int
	AltOK   bool
	AltCode uint16
	// Codes is the number of distinct codes the target currently has.
	Codes int
	// Fixes and Clusters count the accepted solver fixes and TOA clusters;
	// FirstNs is when the target was started.
	Fixes, Clusters uint32
	FirstNs         int64
	// Crowded is set when another shown target with the same identity code
	// is within CrowdM.
	Crowded bool
	// Grade is the quality grade (model.Grade*): from the horizontal σ and
	// the redundancy like Mode S results, one class worse when Crowded.
	Grade uint8
}

// ACStats are the counters of an ACTracker.
type ACStats struct {
	Fixes, FixApplied, FixGated, FixLate, FixDropped uint64
	FixNoSpawn                                       uint64 // no target, and the fix may not start one
	FixOutlier                                       uint64 // outside every gate but close to a shown target that could have sent it
	Created, Confirmed, Removed, Merged              uint64
	Clusters, ClusterApplied, ClusterNoTarget        uint64
	ClusterGated                                     uint64
	Targets, Shown                                   int
}

type acCode struct {
	raw    uint16
	n      uint32
	lastNs int64
	altFt  int
	altOK  bool
}

type acTarget struct {
	id              uint32
	codes           [maxACCodes]acCode
	nc              int
	firstNs, lastNs int64
	burstNs         int64
	fixes, clusters uint32
	bursts          uint32
	shown           bool
	strong          bool // had a fix with DOF >= StrongDOF
	cell            acCell
	hae, sigV       float64  // height of the last estimate and its 1σ (m)
	pos             geo.Vec3 // position of the last estimate (ECEF)
	mergeWith       uint32   // merge candidate of the last sweep
	mergeN          int      // consecutive sweeps it qualified
}

type acCell struct{ x, y, z int32 }

// acCellM is the size of the spatial index cells. A target moves at most
// MaxSpeedMS·CoastS (≈ 11 km) between updates, so the 27 cells around a
// fix hold every candidate.
const acCellM = 40e3

func cellOf(p geo.Vec3) acCell {
	return acCell{int32(math.Floor(p.X / acCellM)), int32(math.Floor(p.Y / acCellM)), int32(math.Floor(p.Z / acCellM))}
}

// ACTracker tracks Mode A/C targets. It is not safe for concurrent use.
type ACTracker struct {
	p       ACParams
	t       *Tracker
	targets map[uint32]*acTarget
	grid    map[acCell][]uint32
	byCode  map[uint16][]uint32
	seq     uint32
	stats   ACStats

	est, est2 Estimate
	cand      []uint32
	out       []ACEstimate
	mest      map[uint32]*Estimate
	mbuf      []*Estimate
	fx        Fix
	cl        Cluster
}

// NewACTracker returns an empty Mode A/C tracker.
func NewACTracker(p ACParams) *ACTracker {
	p = p.Normalize()
	return &ACTracker{p: p, t: New(p.Track), targets: map[uint32]*acTarget{},
		grid: map[acCell][]uint32{}, byCode: map[uint16][]uint32{}}
}

// Params returns the normalised parameters.
func (a *ACTracker) Params() ACParams { return a.p }

// Len returns the number of targets (tentative ones included).
func (a *ACTracker) Len() int { return len(a.targets) }

// Stats returns the counters.
func (a *ACTracker) Stats() ACStats {
	s := a.stats
	s.Targets = len(a.targets)
	for _, tg := range a.targets {
		if tg.shown {
			s.Shown++
		}
	}
	return s
}

func (a *ACTracker) newID() uint32 {
	for {
		a.seq = (a.seq + 1) & 0xFFFFFF
		if a.seq == 0 {
			continue
		}
		id := ACIDFlag | a.seq
		if a.targets[id] == nil {
			return id
		}
	}
}

func (tg *acTarget) code(raw uint16) int {
	for i := 0; i < tg.nc; i++ {
		if tg.codes[i].raw == raw {
			return i
		}
	}
	return -1
}

// altCode returns the index of the target's altitude code (-1: none): the
// most recent code that decodes as a Gillham altitude within AltGateM of
// the target's height; ties of equal recency go to the closer one.
func (a *ACTracker) altCode(tg *acTarget) int {
	best := -1
	var bestNs int64
	bestD := math.Inf(1)
	for i := 0; i < tg.nc; i++ {
		c := &tg.codes[i]
		if !c.altOK {
			continue
		}
		d := math.Abs(float64(c.altFt)*0.3048 - tg.hae)
		if d > a.altGate(tg) {
			continue
		}
		// recency first (Mode C codes change as the target climbs), with a
		// 2 s tolerance so that interleaved A/C replies do not flip it
		if best < 0 || c.lastNs > bestNs+2e9 || (c.lastNs >= bestNs-2e9 && d < bestD) {
			best, bestNs, bestD = i, c.lastNs, d
		}
	}
	return best
}

// altGate is the height gate of altitude codes: AltGateM, or 3σ of the
// target's MLAT height when that is larger (few-station Mode A/C geometry
// fixes the height poorly).
func (a *ACTracker) altGate(tg *acTarget) float64 { return math.Max(a.p.AltGateM, 3*tg.sigV) }

// squawkCode returns the index of the identity code (-1: none): the most
// frequent code that is not the altitude code.
func (a *ACTracker) squawkCode(tg *acTarget, alt int) int {
	best := -1
	for i := 0; i < tg.nc; i++ {
		if i == alt {
			continue
		}
		if best < 0 || tg.codes[i].n > tg.codes[best].n {
			best = i
		}
	}
	return best
}

// codePenalty returns the association cost of code raw for tg, or +Inf
// when the code cannot belong to the target.
func (a *ACTracker) codePenalty(tg *acTarget, raw uint16, hae float64, tNs int64) float64 {
	if tg.code(raw) >= 0 {
		return 0
	}
	if tg.nc < 2 {
		if math.Abs(float64(tNs-tg.lastNs))/1e9 > a.p.NewCodeWindowS {
			return math.Inf(1)
		}
		return a.p.CodePenalty
	}
	ft, ok := ACAltitudeFt(raw)
	if !ok {
		return math.Inf(1)
	}
	// a Mode C code next to one the target already sent (climb or
	// descent), whichever of its codes is the altitude
	for i := 0; i < tg.nc; i++ {
		if c := &tg.codes[i]; c.altOK && math.Abs(float64(ft-c.altFt)) <= a.p.AltGateFt {
			return a.p.CodePenalty
		}
	}
	if math.Abs(float64(ft)*0.3048-hae) <= a.altGate(tg) {
		return a.p.CodePenalty
	}
	return math.Inf(1)
}

func (a *ACTracker) addCode(tg *acTarget, raw uint16, tNs int64) {
	i := tg.code(raw)
	if i < 0 {
		if tg.nc < maxACCodes {
			i = tg.nc
			tg.nc++
		} else {
			// replace the least recently seen code
			i = 0
			for k := 1; k < tg.nc; k++ {
				if tg.codes[k].lastNs < tg.codes[i].lastNs {
					i = k
				}
			}
			a.unindexCode(tg.codes[i].raw, tg.id)
		}
		ft, ok := ACAltitudeFt(raw)
		tg.codes[i] = acCode{raw: raw, altFt: ft, altOK: ok}
		a.byCode[raw] = append(a.byCode[raw], tg.id)
	}
	c := &tg.codes[i]
	c.n++
	c.lastNs = max(c.lastNs, tNs)
}

func (a *ACTracker) unindexCode(raw uint16, id uint32) {
	l := a.byCode[raw]
	if k := slices.Index(l, id); k >= 0 {
		l[k] = l[len(l)-1]
		l = l[:len(l)-1]
	}
	if len(l) == 0 {
		delete(a.byCode, raw)
	} else {
		a.byCode[raw] = l
	}
}

func (a *ACTracker) setCell(tg *acTarget, c acCell, init bool) {
	if !init && c == tg.cell {
		return
	}
	if !init {
		a.unindexCell(tg)
	}
	tg.cell = c
	a.grid[c] = append(a.grid[c], tg.id)
}

func (a *ACTracker) unindexCell(tg *acTarget) {
	l := a.grid[tg.cell]
	if k := slices.Index(l, tg.id); k >= 0 {
		l[k] = l[len(l)-1]
		l = l[:len(l)-1]
	}
	if len(l) == 0 {
		delete(a.grid, tg.cell)
	} else {
		a.grid[tg.cell] = l
	}
}

func (a *ACTracker) remove(tg *acTarget) {
	a.unindexCell(tg)
	for i := 0; i < tg.nc; i++ {
		a.unindexCode(tg.codes[i].raw, tg.id)
	}
	a.t.Delete(tg.id)
	delete(a.targets, tg.id)
	a.stats.Removed++
}

// mahal3 returns dᵀ(A+B)⁻¹d for 3×3 row-major covariances (ok false when
// the sum is not positive definite).
func mahal3(d geo.Vec3, A, B *[9]float64) (float64, bool) {
	m, _, ok := mahal3Det(d, A, B)
	return m, ok
}

// mahal3Det is mahal3 that also returns ln det(A+B).
func mahal3Det(d geo.Vec3, A, B *[9]float64) (float64, float64, bool) {
	var s [9]float64
	for i := range s {
		s[i] = A[i] + B[i]
	}
	// symmetric 3×3 inverse by cofactors
	c00 := s[4]*s[8] - s[5]*s[7]
	c01 := s[5]*s[6] - s[3]*s[8]
	c02 := s[3]*s[7] - s[4]*s[6]
	det := s[0]*c00 + s[1]*c01 + s[2]*c02
	if !(det > 0) {
		return 0, 0, false
	}
	c11 := s[0]*s[8] - s[2]*s[6]
	c12 := s[2]*s[3] - s[0]*s[5]
	c22 := s[0]*s[4] - s[1]*s[3]
	m := (d.X*d.X*c00 + d.Y*d.Y*c11 + d.Z*d.Z*c22 + 2*(d.X*d.Y*c01+d.X*d.Z*c02+d.Y*d.Z*c12)) / det
	return m, math.Log(det), m >= 0
}

// Fix associates a solver fix of a reply with code raw (any X bit is
// ignored) with the best-gating target, or starts a new target. f.ICAO is
// ignored.
func (a *ACTracker) Fix(raw uint16, f *Fix) ACResult {
	a.stats.Fixes++
	raw &= ACCodeMask
	if f.NHyp < 1 || f.NHyp > 2 {
		a.stats.FixDropped++
		return ACResult{Res: ResIgnored}
	}
	p0 := f.Hyp[0].ECEF
	if math.IsNaN(p0.X+p0.Y+p0.Z) || p0.Norm() < 6e6 {
		a.stats.FixDropped++
		return ACResult{Res: ResIgnored}
	}
	c0 := cellOf(p0)
	a.cand = a.cand[:0]
	for dx := int32(-1); dx <= 1; dx++ {
		for dy := int32(-1); dy <= 1; dy++ {
			for dz := int32(-1); dz <= 1; dz++ {
				a.cand = append(a.cand, a.grid[acCell{c0.x + dx, c0.y + dy, c0.z + dz}]...)
			}
		}
	}
	slices.Sort(a.cand) // reproducible tie-breaking
	// cheap pre-filter before the prediction: the fix must be reachable
	// from the target's last position (speed limit, 5σ of the fix, 3 km)
	fc := &f.Hyp[0].Cov
	reach0 := 3000 + 5*math.Sqrt(math.Max(fc[0]+fc[4]+fc[8], 0))
	if f.NHyp == 2 {
		reach0 += f.Hyp[1].ECEF.Dist(p0)
	}
	var best *acTarget
	bestCost := math.Inf(1)
	near := false // a shown target that could have sent the fix is close
	maxLagNs := int64(a.p.Track.MaxLagS * 1e9)
	for _, id := range a.cand {
		tg := a.targets[id]
		if tg == nil || f.TNs < tg.lastNs-maxLagNs {
			continue
		}
		if dt := math.Abs(float64(f.TNs-tg.lastNs)) / 1e9; tg.pos.Dist(p0) > reach0+1.5*a.p.Track.MaxSpeedMS*dt {
			continue
		}
		if !a.t.Predict(id, f.TNs, &a.est2) {
			continue
		}
		pen := a.codePenalty(tg, raw, a.est2.LLH.H, f.TNs)
		if math.IsInf(pen, 1) {
			continue
		}
		// gate on the Mahalanobis distance, choose by the negative log
		// likelihood m² + ln det S: a tentative target with a wide
		// prediction must not steal the fixes of an established one
		m, ld := math.Inf(1), 0.0
		for b := 0; b < f.NHyp; b++ {
			if v, l, ok := mahal3Det(f.Hyp[b].ECEF.Sub(a.est2.ECEF), &f.Hyp[b].Cov, &a.est2.Cov); ok && v+l < m+ld {
				m, ld = v, l
			}
		}
		if tg.shown && m <= a.p.OutlierChi2 {
			near = true
		}
		if m > a.p.GateChi2 {
			continue
		}
		cost := m + ld + pen
		// a shown target wins ties against a tentative one
		if tg.shown {
			cost -= 1
		}
		if cost < bestCost {
			best, bestCost = tg, cost
		}
	}
	a.fx = *f
	if best == nil {
		if f.DOF < a.p.SpawnMinDOF || f.NHyp != 1 {
			a.stats.FixNoSpawn++
			return ACResult{Res: ResNoTrack}
		}
		if near {
			a.stats.FixOutlier++
			return ACResult{Res: ResGated}
		}
		if len(a.targets) >= a.p.MaxTargets {
			a.stats.FixDropped++
			return ACResult{Res: ResNoTrack}
		}
		id := a.newID()
		tg := &acTarget{id: id, firstNs: f.TNs}
		a.targets[id] = tg
		a.fx.ICAO = id
		a.t.Delete(id)
		res := a.t.Fix(&a.fx, &a.est)
		a.stats.Created++
		a.applied(tg, raw, f.TNs, true, f.DOF)
		a.setCell(tg, cellOf(a.est.ECEF), true)
		return ACResult{ID: id, Res: res, New: true}
	}
	a.fx.ICAO = best.id
	res := a.t.Fix(&a.fx, &a.est)
	switch res {
	case ResApplied, ResCreated:
		a.stats.FixApplied++
		a.applied(best, raw, f.TNs, true, f.DOF)
		a.setCell(best, cellOf(a.est.ECEF), false)
	case ResTooLate:
		a.stats.FixLate++
	default:
		a.stats.FixGated++
	}
	return ACResult{ID: best.id, Res: res}
}

// applied books an accepted measurement of tg.
func (a *ACTracker) applied(tg *acTarget, raw uint16, tNs int64, fix bool, dof int) {
	a.addCode(tg, raw, tNs)
	if fix && dof >= a.p.StrongDOF {
		tg.strong = true
	}
	if a.est.Status != StatusNone {
		tg.hae, tg.sigV, tg.pos = a.est.LLH.H, a.est.SigmaV, a.est.ECEF
	}
	if fix {
		tg.fixes++
		if tg.bursts == 0 || float64(tNs-tg.burstNs)/1e9 > a.p.BurstGapS {
			tg.bursts++
		}
		tg.burstNs = max(tg.burstNs, tNs)
	} else {
		tg.clusters++
	}
	tg.lastNs = max(tg.lastNs, tNs)
	nf, nb := a.p.ConfirmFixes, a.p.ConfirmBursts
	if !tg.strong {
		nf, nb = a.p.WeakConfirmFixes, a.p.WeakConfirmBursts
	}
	if !tg.shown && tg.mergeWith == 0 && a.est.Status == StatusConfirmed && int(tg.fixes) >= nf &&
		int(tg.bursts) >= nb && float64(tg.lastNs-tg.firstNs)/1e9 >= a.p.ConfirmSpanS {
		tg.shown = true
		a.stats.Confirmed++
	}
}

// Cluster associates a TOA cluster of 2-3 stations of a reply with code
// raw with a shown target that has sent that code and whose predicted
// position explains the time differences, and updates it. c.ICAO is
// ignored. A cluster never starts a target.
func (a *ACTracker) Cluster(raw uint16, c *Cluster) ACResult {
	a.stats.Clusters++
	raw &= ACCodeMask
	m := len(c.TOAs)
	if m < 2 {
		return ACResult{Res: ResIgnored}
	}
	ids := a.byCode[raw]
	var best *acTarget
	bestChi := math.Inf(1)
	for _, id := range ids {
		tg := a.targets[id]
		if tg == nil || !tg.shown {
			continue
		}
		if !a.t.Predict(id, c.T0Ns+int64(c.TOAs[0].T*1e9), &a.est2) {
			continue
		}
		p := a.est2.ECEF
		// position uncertainty as an isotropic σ (horizontal dominates)
		ps := math.Hypot(a.est2.SigmaH, a.p.ClusterSlackM)
		r0 := p.Dist(c.TOAs[0].Pos)
		chi := 0.0
		for k := 1; k < m; k++ {
			rk := p.Dist(c.TOAs[k].Pos)
			res := (c.TOAs[k].T-c.TOAs[0].T)*geo.C - (rk - r0)
			s0, sk := c.TOAs[0], c.TOAs[k]
			v := (s0.Sigma*s0.Sigma+sk.Sigma*sk.Sigma+s0.SyncSigma*s0.SyncSigma+sk.SyncSigma*sk.SyncSigma)*geo.C*geo.C +
				2*ps*ps
			chi += res * res / v
		}
		if chi < bestChi {
			best, bestChi = tg, chi
		}
	}
	if best == nil {
		a.stats.ClusterNoTarget++
		return ACResult{Res: ResNoTrack}
	}
	if bestChi > Chi2Quantile(0.999, m-1) {
		a.stats.ClusterGated++
		return ACResult{ID: best.id, Res: ResGated}
	}
	a.cl = *c
	a.cl.ICAO = best.id
	res := a.t.Cluster(&a.cl, &a.est)
	if res == ResApplied {
		a.stats.ClusterApplied++
		a.applied(best, raw, c.T0Ns+int64(c.TOAs[0].T*1e9), false, 0)
		a.setCell(best, cellOf(a.est.ECEF), false)
	} else {
		a.stats.ClusterGated++
	}
	return ACResult{ID: best.id, Res: res}
}

// Predict returns the prediction of target id to tNs (see
// Tracker.Predict).
func (a *ACTracker) Predict(id uint32, tNs int64, out *Estimate) bool {
	return a.t.Predict(id, tNs, out)
}

// Hint returns the prediction to tNs of the shown target that has sent code
// raw and is closest to near (ECEF; the zero vector: any), for seeding the
// solver. ok is false when there is none.
func (a *ACTracker) Hint(raw uint16, tNs int64, near geo.Vec3, out *Estimate) bool {
	raw &= ACCodeMask
	found := false
	bestD := math.Inf(1)
	for _, id := range a.byCode[raw] {
		tg := a.targets[id]
		if tg == nil || !tg.shown || !a.t.Predict(id, tNs, &a.est2) {
			continue
		}
		d := 0.0
		if near != (geo.Vec3{}) {
			d = a.est2.ECEF.Dist(near)
		}
		if d < bestD {
			bestD, found = d, true
			*out = a.est2
		}
	}
	return found
}

// Sweep removes targets the kinematic tracker dropped (coasted longer than
// CoastS, not confirmed in time, σ too large), forgets stale codes, and
// calls emit (optional) with the prediction to nowNs of every shown
// target, in ID order. The *ACEstimate is valid only during the call.
func (a *ACTracker) Sweep(nowNs int64, emit func(*ACEstimate)) {
	a.out = a.out[:0]
	a.t.Sweep(nowNs, func(e *Estimate) {
		tg := a.targets[e.ICAO]
		if tg == nil || !tg.shown {
			return
		}
		a.out = append(a.out, a.describe(tg, e))
	})
	ttl := int64(a.p.CodeTTLS * 1e9)
	for _, tg := range a.targets {
		if !a.t.Get(tg.id, &a.est2) {
			a.remove(tg)
			continue
		}
		tg.hae, tg.sigV = a.est2.LLH.H, a.est2.SigmaV
		for i := tg.nc - 1; i >= 0 && tg.nc > 1; i-- {
			if nowNs-tg.codes[i].lastNs > ttl {
				a.unindexCode(tg.codes[i].raw, tg.id)
				tg.nc--
				tg.codes[i] = tg.codes[tg.nc]
			}
		}
	}
	a.merge(nowNs)
	// crowding: shown targets with the same identity code close together
	for i := range a.out {
		oi := &a.out[i]
		if !oi.SquawkOK {
			continue
		}
		for k := range a.out {
			ok := &a.out[k]
			if k != i && ok.SquawkOK && ok.Squawk == oi.Squawk && oi.ECEF.Dist(ok.ECEF) <= a.p.CrowdM {
				oi.Crowded = true
				break
			}
		}
		if oi.Crowded && oi.Grade != model.GradeNone {
			oi.Grade = min(oi.Grade+1, model.GradeD)
		}
	}
	if emit != nil {
		for i := range a.out {
			emit(&a.out[i])
		}
	}
}

// describe builds the output record of tg from its estimate e.
func (a *ACTracker) describe(tg *acTarget, e *Estimate) ACEstimate {
	o := ACEstimate{Estimate: *e, Codes: tg.nc, Fixes: tg.fixes, Clusters: tg.clusters, FirstNs: tg.firstNs}
	sh, sv := tg.hae, tg.sigV
	tg.hae, tg.sigV = e.LLH.H, e.SigmaV
	ai := a.altCode(tg)
	si := a.squawkCode(tg, ai)
	tg.hae, tg.sigV = sh, sv
	if ai >= 0 {
		o.AltOK, o.AltFt, o.AltCode = true, tg.codes[ai].altFt, tg.codes[ai].raw
	}
	if si >= 0 {
		o.SquawkOK, o.Squawk = true, tg.codes[si].raw
	}
	o.Grade = model.GradeFor(e.SigmaH, e.MaxDOF)
	return o
}

// Get returns the current estimate of target id (any status).
func (a *ACTracker) Get(id uint32, out *ACEstimate) bool {
	tg := a.targets[id]
	if tg == nil || !a.t.Get(id, &a.est2) {
		return false
	}
	*out = a.describe(tg, &a.est2)
	return true
}

// Shown reports whether target id is confirmed for display.
func (a *ACTracker) Shown(id uint32) bool {
	tg := a.targets[id]
	return tg != nil && tg.shown
}

// merge joins targets that are the same aircraft (see ACParams.MergeChi2).
// Called by Sweep after the dropped targets were removed.
func (a *ACTracker) merge(nowNs int64) {
	// candidates: targets with an established velocity (single-fix FRUIT
	// targets never qualify), predicted once
	if a.mest == nil {
		a.mest = map[uint32]*Estimate{}
	}
	clear(a.mest)
	a.cand = a.cand[:0]
	for id, tg := range a.targets {
		e := a.mpool(len(a.cand))
		if !a.t.Predict(id, nowNs, e) {
			tg.mergeWith, tg.mergeN = 0, 0
			continue
		}
		a.mest[id] = e
		a.cand = append(a.cand, id)
	}
	slices.Sort(a.cand)
	for _, ia := range a.cand {
		ta := a.targets[ia]
		ea := a.mest[ia]
		if ta == nil || ea == nil {
			continue
		}
		var partner uint32
		c := ta.cell
		for dx := int32(-1); dx <= 1; dx++ {
			for dy := int32(-1); dy <= 1; dy++ {
				for dz := int32(-1); dz <= 1; dz++ {
					for _, ib := range a.grid[acCell{c.x + dx, c.y + dy, c.z + dz}] {
						eb := a.mest[ib]
						tb := a.targets[ib]
						if ib == ia || eb == nil || tb == nil {
							continue
						}
						if a.sameAircraft(ta, tb, ea, eb) && (partner == 0 || ib < partner) {
							partner = ib
						}
					}
				}
			}
		}
		if partner == 0 || partner != ta.mergeWith {
			ta.mergeWith, ta.mergeN = partner, 0
			if partner == 0 {
				continue
			}
		}
		ta.mergeN++
		if ta.mergeN < a.p.MergeSweeps {
			continue
		}
		tb := a.targets[partner]
		keep, drop := ta, tb
		if tb.fixes > ta.fixes || (tb.fixes == ta.fixes && tb.id < ta.id) {
			keep, drop = tb, ta
		}
		for i := 0; i < drop.nc; i++ {
			c := drop.codes[i]
			a.addCode(keep, c.raw, c.lastNs)
			keep.codes[keep.code(c.raw)].n += c.n - 1
		}
		keep.fixes += drop.fixes
		keep.clusters += drop.clusters
		keep.bursts = max(keep.bursts, drop.bursts)
		keep.firstNs = min(keep.firstNs, drop.firstNs)
		keep.lastNs = max(keep.lastNs, drop.lastNs)
		keep.shown = keep.shown || drop.shown
		keep.strong = keep.strong || drop.strong
		keep.mergeWith, keep.mergeN = 0, 0
		delete(a.mest, drop.id)
		a.remove(drop)
		a.stats.Removed--
		a.stats.Merged++
	}
}

// mpool returns the i-th reusable Estimate of the merge pass.
func (a *ACTracker) mpool(i int) *Estimate {
	for len(a.mbuf) <= i {
		a.mbuf = append(a.mbuf, new(Estimate))
	}
	return a.mbuf[i]
}

// sameAircraft tells whether two targets are one aircraft: positions
// within MergeChi2 and horizontal velocities compatible. Targets that share
// a code need velocities within 3σ of each other (MergeVelMS at least): a
// young target of an established one's code is usually its duplicate, born
// from an outlier fix. Targets without a common code (Mode A and Mode C
// halves of one aircraft) need both velocities known to MergeVelMS and
// within MergeVelMS of each other.
func (a *ACTracker) sameAircraft(ta, tb *acTarget, x, y *Estimate) bool {
	m, ok := mahal3(x.ECEF.Sub(y.ECEF), &x.Cov, &y.Cov)
	if !ok || m > a.p.MergeChi2 {
		return false
	}
	shared := false
	for i := 0; i < ta.nc && !shared; i++ {
		shared = tb.code(ta.codes[i].raw) >= 0
	}
	dv := math.Hypot(x.VelENU[0]-y.VelENU[0], x.VelENU[1]-y.VelENU[1])
	vx := x.VelCov[0] + x.VelCov[4]
	vy := y.VelCov[0] + y.VelCov[4]
	if shared {
		return dv <= math.Max(a.p.MergeVelMS, 3*math.Sqrt(math.Max(vx+vy, 0)))
	}
	if math.Sqrt(math.Max(vx, 0)) > a.p.MergeVelMS || math.Sqrt(math.Max(vy, 0)) > a.p.MergeVelMS {
		return false
	}
	return dv <= a.p.MergeVelMS
}
