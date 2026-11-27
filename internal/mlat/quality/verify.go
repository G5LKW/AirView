package quality

import (
	"math"
	"sync"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
	"airview/internal/mlat/solve"
)

// ADS-B verification (DESIGN §9.6): an ADS-B position is checked against
// the times of arrival of the same message at synced stations. With the
// transmit time τ unknown, the m arrivals give m−1 independent time
// differences; for station i against the reference member 0
//
//	dᵢ = (tᵢ − t₀) − (ρᵢ(p) − ρ₀(p))/c
//
// where ρ is the (refraction-corrected) propagation range from the reported
// position p. dᵢ is zero-mean Gaussian when p is right, with covariance
//
//	S = D·C·Dᵀ + G·Σp·Gᵀ / c²,   D = [−1 | I],  G rows = uᵢ − u₀
//
// (C the TOA covariance, Σp the position covariance, uᵢ the unit vector
// from station i to p). χ² = dᵀS⁻¹d has m−1 degrees of freedom.
//
// A message is contradicted when χ² exceeds its 1−PFA quantile AND the
// largest |dᵢ|·c exceeds MinBiasM (a few hundred metres: the gross error a
// spoofer or a wrong position source produces; small model errors must not
// trip it). It is confirmed when m ≥ 3 and χ² is below the 99.9 % quantile.
//
// Phase 4 (Verify with VerifyParams.DropOne / MaxConfirmMDOM, validated on
// simnet with synthetic spoofers, see DefaultVerifyParams):
//
//   - Power. A horizontal offset δ of the reported position along the unit
//     direction e shifts d by G_h·e·δ/c (G_h: the horizontal components of
//     G). The test's noncentrality is δ²·eᵀMe with M = G_hᵀS⁻¹G_h/c², so
//     the minimal detectable offset in the worst direction is
//     MDO = sqrt(λ₀ / λmin(M)), λ₀ the noncentrality at which the χ²(m−1)
//     test at PFA 1e-4 reaches 80 % power. Two stations cannot see an
//     offset along their hyperbola at all; a confirmation whose MDO
//     exceeds MaxConfirmMDOM says nothing about a spoofer and is
//     downgraded to consistent.
//   - Leave-one-out. A contradicted message with m ≥ 5 is re-tested
//     without each row in turn; when exactly one removal makes it
//     consistent, the fault is that station's timestamp (multipath, a
//     gross outlier), not the position: the verdict is none and Culprit
//     names the row.

// TOA is one station's arrival for VerifyTDOA.
type TOA struct {
	Pos geo.Vec3 // antenna, ECEF m
	T   float64  // network time of arrival, s relative to a common epoch
}

// Verdict is the outcome of one TDOA check.
type Verdict uint8

// Verdicts.
const (
	VerdictNone         Verdict = iota // too few stations or numerical failure
	VerdictConfirmed                   // consistent, ≥ 3 stations
	VerdictConsistent                  // consistent, 2 stations (no confirmation)
	VerdictContradicted                // gross inconsistency
)

// String returns the verdict name.
func (v Verdict) String() string {
	switch v {
	case VerdictConfirmed:
		return "confirmed"
	case VerdictConsistent:
		return "consistent"
	case VerdictContradicted:
		return "contradicted"
	}
	return "none"
}

// VerifyParams tune VerifyTDOA and Verify. The zero value is the phase-2
// behaviour (no leave-one-out, no power requirement); DefaultVerifyParams
// returns the validated phase-4 settings.
type VerifyParams struct {
	PFA      float64 // false-alarm probability of the contradiction test (default 1e-4)
	MinBiasM float64 // smallest gross error (m) that may contradict (default 300)
	// DropOne enables the leave-one-out attribution of contradictions to a
	// single station (m >= 5).
	DropOne bool
	// MaxConfirmMDOM > 0: a consistent message confirms only when its
	// minimal detectable horizontal offset is at most this (m).
	MaxConfirmMDOM float64
}

// DefaultVerifyParams returns the phase-4 settings validated on simnet with
// synthetic spoofers (spoof_simnet_test.go).
func DefaultVerifyParams() VerifyParams {
	return VerifyParams{PFA: 1e-4, MinBiasM: 300, DropOne: true, MaxConfirmMDOM: 500}
}

// VerifyResult is the full outcome of one check.
type VerifyResult struct {
	Verdict  Verdict
	Chi2     float64
	Dof      int
	MaxBiasM float64 // largest |dᵢ − d₀|·c, m
	// MDOM is the minimal detectable horizontal offset (m, worst
	// direction, PFA 1e-4, 80 % power); +Inf when it was not computed
	// (MaxConfirmMDOM = 0) or some horizontal direction is invisible.
	MDOM float64
	// Culprit is the row the leave-one-out test blamed (−1 = none).
	Culprit int
}

// verifyMax is the largest member count checked (the best-timed ones).
const verifyMax = 16

var (
	chiOnce sync.Once
	chiHi   [verifyMax]float64 // 1-PFA quantile by dof (default PFA)
	chiOK   [verifyMax]float64 // 99.9 % quantile by dof
	lam0    [verifyMax]float64 // noncentrality for 80 % power at the default PFA
)

func chiTables() {
	for k := 1; k < verifyMax; k++ {
		chiHi[k] = solve.Chi2Quantile(1e-4, k)
		chiOK[k] = solve.Chi2Quantile(1e-3, k)
		lam0[k] = ncLambda(chiHi[k], k, 0.8)
	}
}

// ncChi2Survival returns P(X > x) for a noncentral χ²(k, λ) as the Poisson
// mixture Σⱼ Pois(j; λ/2)·P(χ²(k+2j) > x).
func ncChi2Survival(x float64, k int, lambda float64) float64 {
	if lambda <= 0 {
		return solve.Chi2Survival(x, k)
	}
	h := lambda / 2
	jMax := int(h + 12*math.Sqrt(h) + 30)
	var s float64
	lp := -h // log Poisson weight of j = 0
	for j := 0; j <= jMax; j++ {
		if j > 0 {
			lp += math.Log(h) - math.Log(float64(j))
		}
		s += math.Exp(lp) * solve.Chi2Survival(x, k+2*j)
	}
	return s
}

// ncLambda returns the noncentrality at which a χ²(k) test with threshold
// thr has the given power.
func ncLambda(thr float64, k int, power float64) float64 {
	lo, hi := 0.0, 400.0
	for i := 0; i < 60; i++ {
		mid := (lo + hi) / 2
		if ncChi2Survival(thr, k, mid) < power {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// vWork is the per-call state of Verify (on the stack).
type vWork struct {
	d           [verifyMax]float64 // tᵢ − ρᵢ/c, s
	u           [verifyMax]geo.Vec3
	east, north geo.Vec3
	covP        *[9]float64
	cov         []float64
	sig         []float64
	stride      int
}

func (w *vWork) cij(i, j int) float64 {
	if w.cov != nil {
		return w.cov[i*w.stride+j]
	}
	if i == j {
		return w.sig[i] * w.sig[i]
	}
	return 0
}

// eval tests the rows idx (first = reference): χ², dof, the largest
// |dᵢ − d₀|·c and, when wantMDO, the minimal detectable offset.
func (w *vWork) eval(idx []int, wantMDO bool) (chi2 float64, n int, maxBias, mdo float64, ok bool) {
	const c = geo.C
	n = len(idx) - 1
	mdo = math.Inf(1)
	if n < 1 {
		return 0, 0, 0, mdo, false
	}
	var S [verifyMax * verifyMax]float64
	var g [verifyMax]geo.Vec3
	r0 := idx[0]
	for a := 1; a <= n; a++ {
		g[a] = w.u[idx[a]].Sub(w.u[r0])
	}
	cp := w.covP
	for a := 0; a < n; a++ {
		i := idx[a+1]
		for b := 0; b <= a; b++ {
			j := idx[b+1]
			v := w.cij(i, j) - w.cij(i, r0) - w.cij(r0, j) + w.cij(r0, r0)
			gi, gj := g[a+1], g[b+1]
			ga := [3]float64{gi.X, gi.Y, gi.Z}
			gb := [3]float64{gj.X, gj.Y, gj.Z}
			var q float64
			for x := 0; x < 3; x++ {
				for y := 0; y < 3; y++ {
					q += ga[x] * cp[x*3+y] * gb[y]
				}
			}
			v = (v + q/(c*c)) * 1e18 // ns²
			S[a*n+b], S[b*n+a] = v, v
		}
	}
	var dd [verifyMax]float64
	for a := 0; a < n; a++ {
		dd[a] = w.d[idx[a+1]] - w.d[r0]
		maxBias = math.Max(maxBias, math.Abs(dd[a])*c)
		dd[a] *= 1e9
	}
	if err := linalg.Cholesky(S[:n*n], n); err != nil {
		return 0, n, maxBias, mdo, false
	}
	var work [verifyMax]float64
	chi2 = linalg.MahalanobisSq(S[:n*n], n, dd[:n], work[:n])
	if wantMDO {
		// Y = L⁻¹·G_h (ns per m), M = YᵀY
		var ye, yn [verifyMax]float64
		for a := 0; a < n; a++ {
			ye[a] = g[a+1].Dot(w.east) / c * 1e9
			yn[a] = g[a+1].Dot(w.north) / c * 1e9
		}
		linalg.SolveLower(S[:n*n], n, ye[:n])
		linalg.SolveLower(S[:n*n], n, yn[:n])
		var mee, men, mnn float64
		for a := 0; a < n; a++ {
			mee += ye[a] * ye[a]
			men += ye[a] * yn[a]
			mnn += yn[a] * yn[a]
		}
		l1, l2 := linalg.SymEigen2(mee, men, mnn)
		if lmin := math.Min(l1, l2); lmin > 0 {
			mdo = math.Sqrt(lam0[n] / lmin)
		}
	}
	return chi2, n, maxBias, mdo, true
}

// Verify checks position p (covariance covP, row-major 3×3 ECEF m²) against
// the arrivals toas with TOA covariance cov (m×m, s², row-major; nil =
// independent with sigmaS). rangeM returns the propagation range from a
// station to p. At most 16 rows are used (the first 16). It does not
// allocate.
func Verify(p geo.Vec3, covP [9]float64, toas []TOA, cov []float64, sigmaS []float64,
	rangeM func(rx, ac geo.Vec3) float64, par VerifyParams) VerifyResult {
	chiOnce.Do(chiTables)
	res := VerifyResult{Culprit: -1, MDOM: math.Inf(1)}
	m := len(toas)
	if m < 2 {
		return res
	}
	w := vWork{covP: &covP, cov: cov, sig: sigmaS, stride: len(toas)}
	m = min(m, verifyMax)
	for i := 0; i < m; i++ {
		r := rangeM(toas[i].Pos, p)
		w.u[i] = p.Sub(toas[i].Pos).Unit()
		w.d[i] = toas[i].T - r/geo.C // = τ + noise
	}
	wantMDO := par.MaxConfirmMDOM > 0
	if wantMDO {
		fr := geo.NewFrameECEF(p)
		w.east, w.north = fr.East(), fr.North()
	}
	var idx [verifyMax]int
	for i := 0; i < m; i++ {
		idx[i] = i
	}
	chi2, n, maxBias, mdo, ok := w.eval(idx[:m], wantMDO)
	res.Dof, res.MaxBiasM = n, maxBias
	if !ok {
		return res
	}
	res.Chi2, res.MDOM = chi2, mdo
	minBias := par.MinBiasM
	if !(minBias > 0) {
		minBias = 300
	}
	hi := chiHi[n]
	if pfa := par.PFA; pfa > 0 && pfa != 1e-4 {
		hi = solve.Chi2Quantile(pfa, n)
	}
	switch {
	case chi2 > hi && maxBias > minBias:
		res.Verdict = VerdictContradicted
		if par.DropOne && m >= 5 {
			resolved, culprit := 0, -1
			var sub [verifyMax]int
			for skip := 0; skip < m && resolved < 2; skip++ {
				k := 0
				for i := 0; i < m; i++ {
					if i != skip {
						sub[k] = i
						k++
					}
				}
				if c2, n2, _, _, ok2 := w.eval(sub[:k], false); ok2 && c2 <= chiOK[n2] {
					resolved++
					culprit = skip
				}
			}
			if resolved == 1 {
				res.Verdict, res.Culprit = VerdictNone, culprit
			}
		}
	case chi2 <= chiOK[n] && m >= 3:
		res.Verdict = VerdictConfirmed
		if wantMDO && !(mdo <= par.MaxConfirmMDOM) {
			res.Verdict = VerdictConsistent
		}
	case chi2 <= chiOK[n]:
		res.Verdict = VerdictConsistent
	}
	return res
}

// VerifyTDOA is Verify returning the verdict, χ², its degrees of freedom
// and the largest |dᵢ − d₀|·c (m). With a zero VerifyParams it is the
// phase-2 check (the engine's current call).
func VerifyTDOA(p geo.Vec3, covP [9]float64, toas []TOA, cov []float64, sigmaS []float64,
	rangeM func(rx, ac geo.Vec3) float64, par VerifyParams) (v Verdict, chi2 float64, dof int, maxBiasM float64) {
	r := Verify(p, covP, toas, cov, sigmaS, rangeM, par)
	return r.Verdict, r.Chi2, r.Dof, r.MaxBiasM
}

// Verifier turns per-message verdicts into the per-aircraft adsbSuspect and
// verified states. Safe for concurrent use.
//
// Phase 2 (OnlyConfirmedResets false): an aircraft becomes suspect after
// FailsToSuspect contradictions within WindowNs without a confirmation or
// consistent check in between; any of those clears it.
//
// Phase 4 (OnlyConfirmedResets true, with DefaultVerifyParams): verdicts
// are counted in windows of WindowNs. A merely consistent check (no power
// to see an offset) never clears the flag. The aircraft becomes suspect when
// the window holds at least FailsToSuspect contradictions AND they are at
// least ContradictFrac of the window's contradictions + confirmations
// (consistent checks do not count); it
// is cleared when a window holds at least FailsToSuspect confirmations and
// the contradictions are below that fraction. Rare honest contradictions
// (an MLAT fix with an undetected multipath row, 0.3 % of checks on
// simnet) therefore never make a well-observed aircraft suspect, and a
// spoofer's occasional lucky confirmation (3 % at a 300 m offset) neither
// prevents nor clears the flag.
//
// An aircraft is verified while it had a confirmation in the last
// WindowNs.
type Verifier struct {
	FailsToSuspect int   // default 3
	WindowNs       int64 // default 60 s
	// OnlyConfirmedResets selects the phase-4 rule; false is phase 2.
	OnlyConfirmedResets bool
	// ContradictFrac is the phase-4 fraction (0 = 0.5).
	ContradictFrac float64

	mu sync.Mutex
	m  map[uint32]*verState

	confirmed, contradicted, suspects uint64
}

type verState struct {
	fails     int
	firstFail int64 // phase 2: first contradiction; phase 4: window start
	passes    int   // phase 4: confirmations in the window
	checks    int   // phase 4: verdicts (not none) in the window
	lastPass  int64
	lastNs    int64
	suspect   bool
}

// NewVerifier returns a phase-2 verifier with the defaults.
func NewVerifier() *Verifier {
	return &Verifier{FailsToSuspect: 3, WindowNs: 60e9, m: map[uint32]*verState{}}
}

// NewVerifierPhase4 returns a verifier with the phase-4 rule (use with
// DefaultVerifyParams / DefaultCompareParams).
func NewVerifierPhase4() *Verifier {
	v := NewVerifier()
	v.OnlyConfirmedResets = true
	v.ContradictFrac = 0.5
	return v
}

// Record adds a verdict for aircraft key at nowNs and reports whether the
// aircraft just became suspect.
func (v *Verifier) Record(key uint32, nowNs int64, vd Verdict) (becameSuspect bool) {
	if vd == VerdictNone {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.m[key]
	if s == nil {
		s = &verState{}
		v.m[key] = s
	}
	s.lastNs = nowNs
	if v.OnlyConfirmedResets {
		return v.record4(s, nowNs, vd)
	}
	switch vd {
	case VerdictConfirmed, VerdictConsistent:
		if vd == VerdictConfirmed {
			s.lastPass = nowNs
			v.confirmed++
		}
		s.fails = 0
		s.suspect = false
	case VerdictContradicted:
		v.contradicted++
		if s.fails == 0 || nowNs-s.firstFail > v.WindowNs {
			s.fails, s.firstFail = 0, nowNs
		}
		s.fails++
		if !s.suspect && s.fails >= v.FailsToSuspect {
			s.suspect = true
			v.suspects++
			return true
		}
	}
	return false
}

// record4 is the phase-4 rule (v.mu held).
func (v *Verifier) record4(s *verState, nowNs int64, vd Verdict) bool {
	if vd == VerdictConsistent {
		return false // no power: neither evidence for nor against
	}
	if s.checks == 0 || nowNs-s.firstFail > v.WindowNs {
		s.fails, s.passes, s.checks, s.firstFail = 0, 0, 0, nowNs
	}
	s.checks++
	frac := v.ContradictFrac
	if !(frac > 0) {
		frac = 0.5
	}
	switch vd {
	case VerdictConfirmed:
		v.confirmed++
		s.lastPass = nowNs
		s.passes++
		if s.suspect && s.passes >= v.FailsToSuspect && float64(s.fails) < frac*float64(s.checks) {
			s.suspect = false
		}
	case VerdictContradicted:
		v.contradicted++
		s.fails++
		if !s.suspect && s.fails >= v.FailsToSuspect && float64(s.fails) >= frac*float64(s.checks) {
			s.suspect = true
			v.suspects++
			return true
		}
	}
	return false
}

// State returns whether aircraft key is suspect, and verified (confirmed
// within the window) at nowNs.
func (v *Verifier) State(key uint32, nowNs int64) (suspect, verified bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.m[key]
	if s == nil {
		return false, false
	}
	return s.suspect, s.lastPass > 0 && nowNs-s.lastPass <= v.WindowNs
}

// Counts returns the confirmations, contradictions and suspect events.
func (v *Verifier) Counts() (confirmed, contradicted, suspects uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.confirmed, v.contradicted, v.suspects
}

// Expire forgets aircraft not checked since beforeNs.
func (v *Verifier) Expire(beforeNs int64) {
	v.mu.Lock()
	for k, s := range v.m {
		if s.lastNs < beforeNs {
			delete(v.m, k)
		}
	}
	v.mu.Unlock()
}
