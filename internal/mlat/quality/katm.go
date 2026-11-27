package quality

import (
	"math"
	"sync"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// KAtm estimates the refractivity scale k_atm (DESIGN §9.2.6) from solver
// residuals of precisely timed fixes. The propagation model uses the excess
// path k·E(p, rᵢ), E the excess at k = 1. If the true scale is k*, a row's
// residual (model − measurement) contains (k − k*)·Eᵢ before the solution
// absorbs part of it.
//
// Two estimators share the type.
//
// Projected (AddSolved, the phase-4 estimator). The solution absorbs
// exactly the part of the signal that lies in the column space of the
// fix's design matrix H (one row per TOA: the range gradient and 1 for the
// transmit time; plus the altitude row). Linearised at the solution the
// residual vector is r = P·((k − k*)·E + n) with the W-orthogonal residual
// maker P = I − H(HᵀWH)⁻¹HᵀW, so regressing r on x = P·E is unbiased:
// E[rᵢ] = (k − k*)·xᵢ row by row. Each row is stored as
// zᵢ = k·xᵢ − rᵢ = k*·xᵢ − (Pn)ᵢ, so the slope of z on x is k* itself and
// fixes solved with different model k (the engine may apply the estimate
// mid-hour) mix correctly. The slope is fitted per hour by Huber IRLS
// (1.345) with a cluster-robust (per-fix sandwich) standard error; the
// estimate combines the hours of the last CombineHours by inverse variance.
//
// Demeaned (AddFix, the phase-2 estimator, kept for callers without the
// geometry): the regressor is the per-fix weighted-demeaned excess, which
// removes only the transmit-time column. The position also absorbs part of
// the signal (the excess grows toward low elevations, which partly projects
// onto the height), so its slope is attenuated toward the model value: the
// simnet validation (katm_simnet_test.go) measures how much.
//
// Estimate prefers the projected result when one exists.
type KAtm struct {
	// CombineHours is the number of hourly fits combined (default 6).
	CombineHours int
	// ReadySigma is the largest combined 1σ of k* for Report.Ready
	// (default 0.02); ReadyHours the smallest number of hourly fits
	// (default 2).
	ReadySigma float64
	ReadyHours int

	mu   sync.Mutex
	hour int64

	// demeaned (legacy)
	rows       []katmRow
	est, sigma float64
	n          int
	ok         bool

	// projected
	prow   []katmPRow
	nfix   int32
	hours  [katmHourRing]katmHourFit
	nHours int
	rowsN  uint64
	fixesN uint64
}

type katmRow struct {
	x, y, w float64
}

type katmPRow struct {
	x, z, w float64
	fix     int32
}

// katmHourFit is one hour's projected fit.
type katmHourFit struct {
	hour       int64
	k, sigma   float64
	rows, fixs int
}

const (
	// maxKAtmRows bounds the rows kept per hour (each estimator).
	maxKAtmRows = 200000
	// katmMinRows is the smallest number of rows an hour needs for a fit.
	katmMinRows = 100
	// katmHourRing keeps this many hourly fits for the report.
	katmHourRing = 24
	// katmMaxRows is the largest row count of one projected fix.
	katmMaxRows = 64
)

// AddFix adds one fix's rows to the demeaned estimator: excess1[i] is the
// excess path at k = 1 (m), resid[i] the residual (m, model − measurement)
// and sigma[i] its 1σ (m). The fix is demeaned internally. Rows with
// sigma <= 0 are skipped.
func (k *KAtm) AddFix(nowNs int64, excess1, resid, sigma []float64) {
	var sw, sx, sy float64
	for i := range excess1 {
		if !(sigma[i] > 0) {
			continue
		}
		w := 1 / (sigma[i] * sigma[i])
		sw += w
		sx += w * excess1[i]
		sy += w * resid[i]
	}
	if sw == 0 {
		return
	}
	xm, ym := sx/sw, sy/sw
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rollIfLocked(nowNs)
	for i := range excess1 {
		if !(sigma[i] > 0) || len(k.rows) >= maxKAtmRows {
			continue
		}
		k.rows = append(k.rows, katmRow{x: excess1[i] - xm, y: resid[i] - ym, w: 1 / (sigma[i] * sigma[i])})
	}
}

// KAtmFix is one solved fix for the projected estimator. Only the TOA rows
// the solver used belong in it (excluded rows carry no signal of the
// solution's projection).
type KAtmFix struct {
	// Target is the solution (ECEF, m).
	Target geo.Vec3
	// Stations are the antenna positions of the used TOA rows, ResidM
	// their residuals (model − measurement, m) and SigmaM their 1σ (m).
	Stations []geo.Vec3
	ResidM   []float64
	SigmaM   []float64
	// AltSigmaM > 0 says the fix had an altitude row of this 1σ (m), and
	// AltResidM is its residual (solution height − measured altitude, m;
	// solve.Fix.AltResid).
	AltSigmaM float64
	AltResidM float64
	// ModelK is the k_atm of the propagation model that produced the
	// residuals (0 = 1).
	ModelK float64
}

// katmDK is the half step of the numerical derivative ∂L/∂k.
const katmDK = 0.05

// AddSolved adds one fix to the projected estimator. atm is the model
// atmosphere (N0, H; its K is replaced by ModelK). The regressor is the
// derivative of the optical range with respect to k at the model k — the
// excess is not exactly linear in k (the ray bending adds a k² term worth
// ≈ 0.005 in k* at k* − k = 0.12) — taken as a central difference of two
// ray traces. A fix needs at least two redundant rows (TOA rows + altitude
// row − 4 ≥ 2); others are ignored. It costs three ray traces per row
// (~1.2 µs) and does not allocate.
func (k *KAtm) AddSolved(nowNs int64, f *KAtmFix, atm geo.Atmosphere) {
	m := len(f.Stations)
	if m > katmMaxRows || len(f.ResidM) < m || len(f.SigmaM) < m {
		return
	}
	nRows := m
	if f.AltSigmaM > 0 {
		nRows++
	}
	if nRows-4 < 2 {
		return
	}
	mk := f.ModelK
	if !(mk > 0) {
		mk = 1
	}
	// Atmosphere.Normalize clamps K to KAtmMin…KAtmMax; the difference
	// quotient needs the unclamped neighbours, so the model k is moved
	// inside by the step when it sits on a bound.
	kc := min(max(mk, geo.KAtmMin+katmDK), geo.KAtmMax-katmDK)
	am := atm
	am.K = kc
	am = am.Normalize()
	am.K = kc
	alo, ahi := am, am
	alo.K, ahi.K = kc-katmDK, kc+katmDK
	frame := geo.NewFrameECEF(f.Target)
	var hrow [katmMaxRows + 1][4]float64
	var e, w, r [katmMaxRows + 1]float64
	for i := 0; i < m; i++ {
		if !(f.SigmaM[i] > 0) {
			return
		}
		// The design-matrix row uses the straight-line unit vector: the
		// ray's bending (< 1 mrad above 1° elevation) changes the
		// projection by far less than the regression's noise, and it
		// saves a third ray trace per row.
		d := f.Target.Sub(f.Stations[i])
		g := d.Scale(1 / d.Norm())
		e[i] = (ahi.Range(f.Stations[i], f.Target) - alo.Range(f.Stations[i], f.Target)) / (2 * katmDK)
		u := frame.Rotate(g)
		hrow[i] = [4]float64{u.X, u.Y, u.Z, 1}
		w[i] = 1 / (f.SigmaM[i] * f.SigmaM[i])
		r[i] = f.ResidM[i]
	}
	if f.AltSigmaM > 0 {
		hrow[m] = [4]float64{0, 0, 1, 0}
		w[m] = 1 / (f.AltSigmaM * f.AltSigmaM)
		e[m] = 0
		r[m] = f.AltResidM
	}
	// N = HᵀWH, v = HᵀWE, vr = HᵀWr
	var N linalg.Mat4
	var v, vr linalg.Vec4
	for i := 0; i < nRows; i++ {
		for a := 0; a < 4; a++ {
			v[a] += w[i] * hrow[i][a] * e[i]
			vr[a] += w[i] * hrow[i][a] * r[i]
			for b := 0; b < 4; b++ {
				N[a*4+b] += w[i] * hrow[i][a] * hrow[i][b]
			}
		}
	}
	beta, err := N.SolveSPD(v)
	if err != nil {
		return
	}
	betaR, err := N.SolveSPD(vr)
	if err != nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rollIfLocked(nowNs)
	if len(k.prow)+m > maxKAtmRows {
		return
	}
	fix := k.nfix
	k.nfix++
	added := false
	for i := 0; i < m; i++ {
		x := e[i] - (hrow[i][0]*beta[0] + hrow[i][1]*beta[1] + hrow[i][2]*beta[2] + hrow[i][3]*beta[3])
		y := r[i] - (hrow[i][0]*betaR[0] + hrow[i][1]*betaR[1] + hrow[i][2]*betaR[2] + hrow[i][3]*betaR[3])
		if !(math.Abs(x) > 1e-6) || math.IsNaN(y) {
			continue
		}
		k.prow = append(k.prow, katmPRow{x: x, z: mk*x - y, w: w[i], fix: fix})
		k.rowsN++
		added = true
	}
	if added {
		k.fixesN++
	}
}

// rollIfLocked fits the finished hour when nowNs is in a new one.
func (k *KAtm) rollIfLocked(nowNs int64) {
	h := nowNs / hourNs
	if k.hour != h {
		k.rollLocked()
		k.hour = h
	}
}

// rollLocked fits the rows of the current hour and starts a new one.
func (k *KAtm) rollLocked() {
	if len(k.rows) >= katmMinRows {
		if b, sb, ok := huberSlope(k.rows); ok {
			k.est, k.sigma, k.ok = b, sb, true
			k.n++
		}
	}
	k.rows = k.rows[:0]
	if len(k.prow) >= katmMinRows {
		if ks, sg, ok := huberSlopeClustered(k.prow); ok {
			k.hours[k.nHours%katmHourRing] = katmHourFit{hour: k.hour, k: ks, sigma: sg, rows: len(k.prow), fixs: int(k.nfix)}
			k.nHours++
		}
	}
	k.prow = k.prow[:0]
	k.nfix = 0
}

// huberSlope fits y = b·x through the origin with Huber IRLS.
func huberSlope(rows []katmRow) (b, sigma float64, ok bool) {
	const hk = 1.345
	for it := 0; it < 10; it++ {
		var sxx, sxy float64
		for _, r := range rows {
			w := r.w
			if it > 0 {
				e := math.Abs(r.y-b*r.x) * math.Sqrt(r.w)
				if e > hk {
					w *= hk / e
				}
			}
			sxx += w * r.x * r.x
			sxy += w * r.x * r.y
		}
		if !(sxx > 0) {
			return 0, 0, false
		}
		nb := sxy / sxx
		sigma = 1 / math.Sqrt(sxx)
		if math.Abs(nb-b) < 1e-6 {
			b = nb
			break
		}
		b = nb
	}
	return b, sigma, true
}

// huberSlopeClustered fits z = k·x through the origin with Huber IRLS
// (scale: the robust scale of the whitened residuals, so a misstated row σ
// does not change which rows count as outliers) and returns the
// cluster-robust 1σ of the slope: rows of one fix share the projection and
// are correlated, so the score is summed per fix before squaring.
func huberSlopeClustered(rows []katmPRow) (kk, sigma float64, ok bool) {
	const hk = 1.345
	scale := 1.0
	wt := func(r *katmPRow, k, s float64) float64 {
		e := math.Abs(r.z-k*r.x) * math.Sqrt(r.w) / s
		if e > hk {
			return r.w * hk / e
		}
		return r.w
	}
	// start: weighted LS
	var sxx, sxz float64
	for i := range rows {
		r := &rows[i]
		sxx += r.w * r.x * r.x
		sxz += r.w * r.x * r.z
	}
	if !(sxx > 0) {
		return 0, 0, false
	}
	kk = sxz / sxx
	abs := make([]float64, len(rows))
	for it := 0; it < 15; it++ {
		for i := range rows {
			r := &rows[i]
			abs[i] = math.Abs(r.z-kk*r.x) * math.Sqrt(r.w)
		}
		if s := medianInPlace(abs) / 0.6745; s > 0 {
			scale = s
		}
		sxx, sxz = 0, 0
		for i := range rows {
			r := &rows[i]
			w := wt(r, kk, scale)
			sxx += w * r.x * r.x
			sxz += w * r.x * r.z
		}
		if !(sxx > 0) {
			return 0, 0, false
		}
		nk := sxz / sxx
		done := math.Abs(nk-kk) < 1e-7
		kk = nk
		if done {
			break
		}
	}
	// cluster-robust sandwich: Var = Σ_f (Σ_{i∈f} wᵢxᵢeᵢ)² / (Σ wᵢxᵢ²)²
	var meat, g float64
	cur := int32(-1)
	for i := range rows {
		r := &rows[i]
		if r.fix != cur {
			meat += g * g
			g, cur = 0, r.fix
		}
		g += wt(r, kk, scale) * r.x * (r.z - kk*r.x)
	}
	meat += g * g
	sigma = math.Sqrt(meat) / sxx
	if !(sigma > 0) {
		sigma = scale / math.Sqrt(sxx)
	}
	return kk, sigma, true
}

// medianInPlace returns the median of v (reordering it); 0 when empty.
func medianInPlace(v []float64) float64 {
	n := len(v)
	if n == 0 {
		return 0
	}
	return quickSelect(v, n/2)
}

// quickSelect returns the k-th smallest element of v (reordering it).
func quickSelect(v []float64, k int) float64 {
	lo, hi := 0, len(v)-1
	for lo < hi {
		p := v[(lo+hi)/2]
		i, j := lo, hi
		for i <= j {
			for v[i] < p {
				i++
			}
			for v[j] > p {
				j--
			}
			if i <= j {
				v[i], v[j] = v[j], v[i]
				i++
				j--
			}
		}
		switch {
		case k <= j:
			hi = j
		case k >= i:
			lo = i
		default:
			return v[k]
		}
	}
	return v[k]
}

// combinedLocked returns the inverse-variance combination of the last
// CombineHours projected fits (σ floor 0.002 per hour: the hours are not
// independent of slow weather changes).
func (k *KAtm) combinedLocked() (kk, sigma float64, hours int, ok bool) {
	nh := k.CombineHours
	if nh <= 0 {
		nh = 6
	}
	nh = min(nh, katmHourRing, k.nHours)
	var sw, swk float64
	for i := 0; i < nh; i++ {
		h := &k.hours[(k.nHours-1-i)%katmHourRing]
		s := math.Max(h.sigma, 0.002)
		w := 1 / (s * s)
		sw += w
		swk += w * h.k
	}
	if sw == 0 {
		return 0, 0, 0, false
	}
	return swk / sw, 1 / math.Sqrt(sw), nh, true
}

// Estimate returns the k_atm estimate bounded to geo.KAtmMin…KAtmMax with
// its 1σ and the number of hourly fits behind it; ok is false before the
// first hour with data. With projected fits it is their combination
// (independent of model); otherwise the demeaned estimator's model − b of
// the last finished hour.
func (k *KAtm) Estimate(model float64) (kAtm, sigma float64, hours int, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if kk, sg, nh, okp := k.combinedLocked(); okp {
		return min(max(kk, geo.KAtmMin), geo.KAtmMax), sg, nh, true
	}
	if !k.ok {
		return model, 0, 0, false
	}
	return min(max(model-k.est, geo.KAtmMin), geo.KAtmMax), k.sigma, k.n, true
}

// KAtmHour is one hourly projected fit.
type KAtmHour struct {
	StartUnixMs int64   `json:"startUnixMs"`
	K           float64 `json:"k"`
	Sigma       float64 `json:"sigma"`
	Rows        int     `json:"rows"`
	Fixes       int     `json:"fixes"`
}

// KAtmReport describes the estimator state (admin).
type KAtmReport struct {
	// Method is "projected", "demeaned" or "" (no estimate yet).
	Method string `json:"method"`
	// K and Sigma: the bounded estimate and its 1σ; Hours the hourly fits
	// combined.
	K     float64 `json:"k"`
	Sigma float64 `json:"sigma"`
	Hours int     `json:"hours"`
	// Chi2PerHour is the χ² per degree of freedom of the combined hours
	// around K (≈ 1 when the hours agree; large = k* is changing or the
	// errors are understated).
	Chi2PerHour float64 `json:"chi2PerHour"`
	// Ready: Sigma <= ReadySigma, Hours >= ReadyHours and Chi2PerHour <= 4
	// — the estimate is precise and stable enough to apply.
	Ready bool `json:"ready"`
	// Rows and Fixes counted by the projected estimator since start.
	Rows  uint64 `json:"rows"`
	Fixes uint64 `json:"fixes"`
	// Recent hourly fits, newest first.
	Recent []KAtmHour `json:"recent,omitempty"`
}

// Report returns the estimator state.
func (k *KAtm) Report() KAtmReport {
	k.mu.Lock()
	defer k.mu.Unlock()
	r := KAtmReport{Rows: k.rowsN, Fixes: k.fixesN}
	kk, sg, nh, ok := k.combinedLocked()
	switch {
	case ok:
		r.Method, r.K, r.Sigma, r.Hours = "projected", min(max(kk, geo.KAtmMin), geo.KAtmMax), sg, nh
		var chi float64
		for i := 0; i < nh; i++ {
			h := &k.hours[(k.nHours-1-i)%katmHourRing]
			s := math.Max(h.sigma, 0.002)
			chi += (h.k - kk) * (h.k - kk) / (s * s)
		}
		if nh > 1 {
			r.Chi2PerHour = chi / float64(nh-1)
		}
		rs, rh := k.ReadySigma, k.ReadyHours
		if rs <= 0 {
			rs = 0.02
		}
		if rh <= 0 {
			rh = 2
		}
		r.Ready = sg <= rs && nh >= rh && r.Chi2PerHour <= 4
	case k.ok:
		r.Method, r.K, r.Sigma, r.Hours = "demeaned", min(max(1-k.est, geo.KAtmMin), geo.KAtmMax), k.sigma, k.n
	}
	for i := 0; i < min(k.nHours, katmHourRing); i++ {
		h := &k.hours[(k.nHours-1-i)%katmHourRing]
		r.Recent = append(r.Recent, KAtmHour{StartUnixMs: h.hour * hourNs / 1e6, K: h.k, Sigma: h.sigma, Rows: h.rows, Fixes: h.fixs})
	}
	return r
}

// Flush fits the rows of the current hour now (tests, shutdown).
func (k *KAtm) Flush() {
	k.mu.Lock()
	k.rollLocked()
	k.mu.Unlock()
}
