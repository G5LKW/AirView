package stat

import (
	"math"
	"slices"
)

// LinearFit is the result of a simple least-squares line y = Intercept +
// Slope*x.
type LinearFit struct {
	Slope       float64 `json:"slope"`
	Intercept   float64 `json:"intercept"`
	R2          float64 `json:"r2"` // 0 when y is constant
	SlopeSE     float64 `json:"slopeSE"`
	InterceptSE float64 `json:"interceptSE"`
	T           float64 `json:"t"`        // slope / SlopeSE (capped at +-1e300)
	P           float64 `json:"p"`        // two-sided p-value of the slope, df = N-2
	ResidStd    float64 `json:"residStd"` // sqrt(SSE/(N-2))
	N           int     `json:"n"`
	XMean       float64 `json:"xMean"`
	SXX         float64 `json:"sxx"` // sum of squared x deviations
}

// LinearRegression fits y = a + b*x by ordinary least squares on the
// pairwise-complete observations. Errors: ErrDimension, ErrInsufficientData
// (n < 3), ErrSingular (x constant).
func LinearRegression(x, y []float64) (LinearFit, error) {
	xs, ys, err := completePairs(x, y)
	if err != nil {
		return LinearFit{}, err
	}
	n := len(xs)
	if n < 3 {
		return LinearFit{N: n}, ErrInsufficientData
	}
	fn := float64(n)
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx /= fn
	my /= fn
	var sxx, syy, sxy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxx += dx * dx
		syy += dy * dy
		sxy += dx * dy
	}
	if !(sxx > 0) {
		return LinearFit{N: n}, ErrSingular
	}
	f := LinearFit{N: n, XMean: mx, SXX: sxx}
	f.Slope = sxy / sxx
	f.Intercept = my - f.Slope*mx
	sse := 0.0
	for i := range xs {
		e := ys[i] - (f.Intercept + f.Slope*xs[i])
		sse += e * e
	}
	if syy > 0 {
		f.R2 = clamp(1-sse/syy, 0, 1)
	}
	df := fn - 2
	f.ResidStd = math.Sqrt(sse / df)
	f.SlopeSE = f.ResidStd / math.Sqrt(sxx)
	f.InterceptSE = f.ResidStd * math.Sqrt(1/fn+mx*mx/sxx)
	f.T, f.P = tStat(f.Slope, f.SlopeSE, df)
	return f, nil
}

// tStat returns coef/se and its two-sided p-value, staying finite when the
// standard error vanishes (perfect fit).
func tStat(coef, se, df float64) (t, p float64) {
	if se > 0 {
		t = coef / se
		if math.IsInf(t, 0) {
			return math.Copysign(1e300, t), 0
		}
		return t, StudentTTwoSidedP(t, df)
	}
	if coef == 0 {
		return 0, 1
	}
	return math.Copysign(1e300, coef), 0
}

// Predict returns the fitted value at x.
func (f LinearFit) Predict(x float64) float64 { return f.Intercept + f.Slope*x }

// PredictInterval returns the fitted value at x and the two-sided prediction
// interval for a single new observation at coverage level (e.g. 0.95;
// invalid levels mean 0.95), using the t distribution with N-2 df.
func (f LinearFit) PredictInterval(x, level float64) (yhat, lo, hi float64) {
	yhat = f.Predict(x)
	h := f.halfWidth(x, level, 1)
	return yhat, yhat - h, yhat + h
}

// ConfidenceInterval returns the fitted value at x and the confidence
// interval of the regression line (the mean response) at coverage level.
func (f LinearFit) ConfidenceInterval(x, level float64) (yhat, lo, hi float64) {
	yhat = f.Predict(x)
	h := f.halfWidth(x, level, 0)
	return yhat, yhat - h, yhat + h
}

func (f LinearFit) halfWidth(x, level, extra float64) float64 {
	if f.N < 3 || !(f.SXX > 0) {
		return math.NaN()
	}
	t := StudentTQuantile(0.5+fixLevel(level)/2, float64(f.N-2))
	return t * f.ResidStd * math.Sqrt(extra+1/float64(f.N)+sq(x-f.XMean)/f.SXX)
}

// RobustLine is a Theil-Sen line fit.
type RobustLine struct {
	Slope     float64 `json:"slope"`
	Intercept float64 `json:"intercept"`
	N         int     `json:"n"`
	ResidStd  float64 `json:"residStd"` // MADScale * MAD of the residuals
}

// Predict returns the fitted value at x.
func (l RobustLine) Predict(x float64) float64 { return l.Intercept + l.Slope*x }

// theilSenExact is the largest n for which all n(n-1)/2 pair slopes are used.
const theilSenExact = 700

// TheilSen fits a robust line: the slope is the median of the pairwise
// slopes and the intercept the median of y - slope*x. It tolerates up to
// about 29 % arbitrary outliers. Pairs with equal x are ignored. For more
// than 700 complete observations a deterministic pseudo-random subset of
// 250,000 pairs is used instead of all pairs. Errors: ErrDimension,
// ErrInsufficientData (n < 2), ErrSingular (x constant).
func TheilSen(x, y []float64) (RobustLine, error) {
	xs, ys, err := completePairs(x, y)
	if err != nil {
		return RobustLine{}, err
	}
	n := len(xs)
	if n < 2 {
		return RobustLine{N: n}, ErrInsufficientData
	}
	var slopes []float64
	if n <= theilSenExact {
		slopes = make([]float64, 0, n*(n-1)/2)
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if dx := xs[j] - xs[i]; dx != 0 {
					slopes = append(slopes, (ys[j]-ys[i])/dx)
				}
			}
		}
	} else {
		const pairs = 250000
		slopes = make([]float64, 0, pairs)
		state := uint64(0x9E3779B97F4A7C15)
		next := func() uint64 { // xorshift64*
			state ^= state >> 12
			state ^= state << 25
			state ^= state >> 27
			return state * 0x2545F4914F6CDD1D
		}
		un := uint64(n)
		for k := 0; k < pairs; k++ {
			i, j := int((next()>>11)%un), int((next()>>11)%un)
			if dx := xs[j] - xs[i]; dx != 0 {
				slopes = append(slopes, (ys[j]-ys[i])/dx)
			}
		}
	}
	if len(slopes) == 0 {
		return RobustLine{N: n}, ErrSingular
	}
	l := RobustLine{N: n, Slope: medianInPlace(slopes)}
	res := make([]float64, n)
	for i := range xs {
		res[i] = ys[i] - l.Slope*xs[i]
	}
	l.Intercept = medianInPlace(slices.Clone(res))
	for i := range res {
		res[i] = math.Abs(res[i] - l.Intercept)
	}
	l.ResidStd = MADScale * medianInPlace(res)
	return l, nil
}

// OLSResult is a fitted multiple linear regression.
//
// Coef, SE, T and P are indexed like the design matrix columns: when
// Intercept is true, index 0 is the intercept and feature j is at j+1.
type OLSResult struct {
	Coef      []float64 `json:"coef"`
	Intercept bool      `json:"intercept"`
	SE        []float64 `json:"se"`
	T         []float64 `json:"t"`
	P         []float64 `json:"p"`
	R2        float64   `json:"r2"`       // 0 when y has no variance; uncentred without intercept
	AdjR2     float64   `json:"adjR2"`    // adjusted for the number of coefficients
	ResidStd  float64   `json:"residStd"` // sqrt(SSE/DF)
	SSE       float64   `json:"sse"`
	SST       float64   `json:"sst"`
	F         float64   `json:"f"`   // overall F statistic (0 if not applicable)
	FP        float64   `json:"fp"`  // its p-value (1 if not applicable)
	N         int       `json:"n"`   // rows used
	DF        int       `json:"df"`  // residual degrees of freedom N - len(Coef)
	Cov       []float64 `json:"cov"` // unscaled (X'X)^-1, row-major, order len(Coef)
}

// cleanRows validates the design rows and drops every row containing a
// non-finite value in X, y or (if given) w, or a non-positive weight.
func cleanRows(X [][]float64, y, w []float64) (rows [][]float64, ys, ws []float64, k int, err error) {
	if len(X) != len(y) || (w != nil && len(w) != len(y)) {
		return nil, nil, nil, 0, ErrDimension
	}
	if len(X) == 0 {
		return nil, nil, nil, 0, ErrInsufficientData
	}
	k = len(X[0])
	rows = make([][]float64, 0, len(X))
	ys = make([]float64, 0, len(X))
	if w != nil {
		ws = make([]float64, 0, len(X))
	}
outer:
	for i, r := range X {
		if len(r) != k {
			return nil, nil, nil, 0, ErrDimension
		}
		if !IsFinite(y[i]) {
			continue
		}
		if w != nil && (!(w[i] > 0) || math.IsInf(w[i], 0)) {
			continue
		}
		for _, v := range r {
			if !IsFinite(v) {
				continue outer
			}
		}
		rows = append(rows, r)
		ys = append(ys, y[i])
		if w != nil {
			ws = append(ws, w[i])
		}
	}
	return rows, ys, ws, k, nil
}

// OLS fits y = X*b (+ intercept) by Householder QR. X holds one row per
// observation with one column per feature. Rows containing NaN/Inf are
// dropped. Errors: ErrDimension (ragged X or length mismatch),
// ErrInsufficientData (rows <= coefficients), ErrSingular (collinear or
// constant columns).
func OLS(X [][]float64, y []float64, intercept bool) (*OLSResult, error) {
	return fitLS(X, y, nil, intercept)
}

// WLS is weighted least squares: it minimises sum w_i*(y_i - x_i*b)^2. The
// weights are precision weights (var(e_i) = sigma^2 / w_i); rows with a
// non-positive or non-finite weight are dropped. R2, ResidStd and the
// standard errors are the weighted analogues; prediction intervals refer to
// an observation of weight 1.
func WLS(X [][]float64, y, w []float64, intercept bool) (*OLSResult, error) {
	if w == nil {
		return nil, ErrDimension
	}
	return fitLS(X, y, w, intercept)
}

func fitLS(X [][]float64, y, w []float64, intercept bool) (*OLSResult, error) {
	rows, ys, ws, k, err := cleanRows(X, y, w)
	if err != nil {
		return nil, err
	}
	n := len(rows)
	p := k
	off := 0
	if intercept {
		p++
		off = 1
	}
	if p == 0 {
		return nil, ErrInvalidArgument
	}
	if n <= p {
		return nil, ErrInsufficientData
	}
	a := NewMatrix(n, p)
	b := make([]float64, n)
	for i, r := range rows {
		sw := 1.0
		if ws != nil {
			sw = math.Sqrt(ws[i])
		}
		row := a.Data[i*p : (i+1)*p]
		if intercept {
			row[0] = sw
		}
		for j, v := range r {
			row[off+j] = sw * v
		}
		b[i] = sw * ys[i]
	}
	qr, err := NewQR(a)
	if err != nil {
		return nil, err
	}
	coef, err := qr.Solve(b)
	if err != nil {
		return nil, err
	}
	cov, err := qr.NormalInverse()
	if err != nil {
		return nil, err
	}
	res := &OLSResult{Coef: coef, Intercept: intercept, N: n, DF: n - p, Cov: cov.Data}

	// Weighted mean of y for the centred total sum of squares.
	sw, swy := 0.0, 0.0
	for i, v := range ys {
		wi := 1.0
		if ws != nil {
			wi = ws[i]
		}
		sw += wi
		swy += wi * v
	}
	ybar := 0.0
	if intercept {
		ybar = swy / sw
	}
	for i, r := range rows {
		wi := 1.0
		if ws != nil {
			wi = ws[i]
		}
		e := ys[i] - res.Predict(r)
		res.SSE += wi * e * e
		res.SST += wi * sq(ys[i]-ybar)
	}
	df := float64(res.DF)
	res.ResidStd = math.Sqrt(res.SSE / df)
	if res.SST > 0 {
		res.R2 = clamp(1-res.SSE/res.SST, 0, 1)
		num := float64(n)
		if intercept {
			num--
		}
		res.AdjR2 = 1 - (1-res.R2)*num/df
	}
	res.SE = make([]float64, p)
	res.T = make([]float64, p)
	res.P = make([]float64, p)
	for j := 0; j < p; j++ {
		res.SE[j] = res.ResidStd * math.Sqrt(math.Max(cov.Data[j*p+j], 0))
		res.T[j], res.P[j] = tStat(coef[j], res.SE[j], df)
	}
	res.FP = 1
	if intercept && k > 0 && res.SST > 0 {
		if res.SSE > 0 {
			res.F = ((res.SST - res.SSE) / float64(k)) / (res.SSE / df)
			if math.IsInf(res.F, 0) {
				res.F = 1e300
			}
			res.FP = FDistSF(res.F, float64(k), df)
		} else {
			res.F, res.FP = 1e300, 0
		}
	}
	return res, nil
}

// NumFeatures returns the number of feature columns the model expects.
func (r *OLSResult) NumFeatures() int {
	if r.Intercept {
		return len(r.Coef) - 1
	}
	return len(r.Coef)
}

// Predict returns the fitted value for a feature row. NaN is returned when
// the row has the wrong length.
func (r *OLSResult) Predict(x []float64) float64 {
	if len(x) != r.NumFeatures() {
		return math.NaN()
	}
	if r.Intercept {
		return r.Coef[0] + dot(r.Coef[1:], x)
	}
	return dot(r.Coef, x)
}

// PredictInterval returns the fitted value and the two-sided prediction
// interval for one new observation at coverage level (invalid: 0.95), using
// the t distribution with DF degrees of freedom and the leverage of x.
func (r *OLSResult) PredictInterval(x []float64, level float64) (yhat, lo, hi float64) {
	yhat = r.Predict(x)
	if p := len(r.Coef); yhat != yhat || r.DF < 1 || len(r.Cov) != p*p {
		return yhat, math.NaN(), math.NaN()
	}
	v := x
	if r.Intercept {
		v = make([]float64, 0, len(x)+1)
		v = append(append(v, 1), x...)
	}
	lev := math.Max(quadForm(r.Cov, v), 0)
	h := StudentTQuantile(0.5+fixLevel(level)/2, float64(r.DF)) * r.ResidStd * math.Sqrt(1+lev)
	return yhat, yhat - h, yhat + h
}
