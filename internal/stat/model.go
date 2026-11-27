package stat

import (
	"encoding/json"
	"fmt"
	"math"
)

// Model kinds.
const (
	KindLinear   = "linear"   // Predict returns the linear predictor
	KindLogistic = "logistic" // Predict returns a probability
)

// Model is a fitted (generalised) linear model on standardised features:
//
//	eta = Intercept + sum_j Coef[j] * (x[j]-Mean[j]) / Scale[j]
//
// Predict returns eta for KindLinear and Sigmoid(eta) for KindLogistic. The
// struct holds everything needed for prediction, contains no NaN after a
// successful fit and round-trips through encoding/json, so it can be stored
// in the database.
type Model struct {
	Kind      string    `json:"kind"`
	Features  []string  `json:"features"`  // feature names, in column order
	Mean      []float64 `json:"mean"`      // per-feature centre
	Scale     []float64 `json:"scale"`     // per-feature scale (1 for constant features)
	Coef      []float64 `json:"coef"`      // coefficients of the standardised features
	Intercept float64   `json:"intercept"` // value at the feature means
	Lambda    float64   `json:"lambda"`    // L2 penalty used for the fit
	N         int       `json:"n"`         // rows used
	DF        float64   `json:"df"`        // residual degrees of freedom (linear)
	ResidStd  float64   `json:"residStd"`  // residual standard deviation (linear)
	R2        float64   `json:"r2"`        // R2 (linear) or McFadden pseudo-R2 (logistic)
	LogLoss   float64   `json:"logLoss"`   // mean negative log-likelihood (logistic)
	Iter      int       `json:"iter"`      // IRLS iterations (logistic)
	Converged bool      `json:"converged"` // false if IRLS hit its iteration limit
	// Cov is the covariance matrix of (Intercept, Coef...) in row-major
	// order, (p+1) x (p+1); optional, enables leverage-aware intervals.
	Cov []float64 `json:"cov,omitempty"`
}

// NumFeatures returns the number of features the model expects.
func (m *Model) NumFeatures() int { return len(m.Coef) }

// Validate checks the internal consistency of a (possibly deserialised)
// model.
func (m *Model) Validate() error {
	if m == nil {
		return ErrInvalidArgument
	}
	p := len(m.Coef)
	if m.Kind != KindLinear && m.Kind != KindLogistic {
		return fmt.Errorf("%w: unknown model kind %q", ErrInvalidArgument, m.Kind)
	}
	if len(m.Mean) != p || len(m.Scale) != p || len(m.Features) != p {
		return ErrDimension
	}
	if m.Cov != nil && len(m.Cov) != (p+1)*(p+1) {
		return ErrDimension
	}
	if !IsFinite(m.Intercept) {
		return ErrInvalidArgument
	}
	for j := 0; j < p; j++ {
		if !IsFinite(m.Coef[j]) || !IsFinite(m.Mean[j]) || !IsFinite(m.Scale[j]) || m.Scale[j] == 0 {
			return ErrInvalidArgument
		}
	}
	return nil
}

// ParseModel decodes a JSON model and validates it.
func ParseModel(data []byte) (*Model, error) {
	m := new(Model)
	if err := json.Unmarshal(data, m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// standardize fills z with the standardised feature row. Non-finite features
// are imputed with the training mean (z = 0). ok is false for a wrong length.
func (m *Model) standardize(x, z []float64) bool {
	if len(x) != len(m.Coef) || len(m.Mean) != len(x) || len(m.Scale) != len(x) {
		return false
	}
	for j, v := range x {
		if !IsFinite(v) || m.Scale[j] == 0 {
			z[j] = 0
		} else {
			z[j] = (v - m.Mean[j]) / m.Scale[j]
		}
	}
	return true
}

// Linear returns the linear predictor eta for a feature row. Missing
// (non-finite) features are imputed with their training mean. NaN is
// returned when the row has the wrong length.
func (m *Model) Linear(x []float64) float64 {
	if len(x) != len(m.Coef) || len(m.Mean) != len(x) || len(m.Scale) != len(x) {
		return math.NaN()
	}
	eta := m.Intercept
	for j, v := range x {
		if IsFinite(v) && m.Scale[j] != 0 {
			eta += m.Coef[j] * (v - m.Mean[j]) / m.Scale[j]
		}
	}
	return eta
}

// Predict returns the model output for a feature row: the linear predictor
// for KindLinear, the event probability for KindLogistic. Missing features
// are imputed with their training mean; a wrong row length yields NaN.
func (m *Model) Predict(x []float64) float64 {
	eta := m.Linear(x)
	if m.Kind == KindLogistic {
		return Sigmoid(eta)
	}
	return eta
}

// PredictMap predicts from named feature values. Names that the model does
// not know are ignored; features absent from the map are imputed with their
// training mean.
func (m *Model) PredictMap(values map[string]float64) float64 {
	x := make([]float64, len(m.Features))
	for j, name := range m.Features {
		if v, ok := values[name]; ok {
			x[j] = v
		} else {
			x[j] = math.NaN()
		}
	}
	return m.Predict(x)
}

// PredictInterval returns the prediction and a two-sided interval at
// coverage level (invalid: 0.95).
//
// KindLinear: a prediction interval for one new observation, from ResidStd
// and, if Cov is present, the uncertainty of the coefficients (t quantile
// with DF degrees of freedom when DF >= 1, else normal).
//
// KindLogistic: a confidence interval for the probability, obtained by
// transforming eta +- z*se(eta); without Cov the interval collapses to the
// point estimate.
func (m *Model) PredictInterval(x []float64, level float64) (pred, lo, hi float64) {
	p := len(m.Coef)
	v := make([]float64, p+1)
	v[0] = 1
	if !m.standardize(x, v[1:]) {
		return math.NaN(), math.NaN(), math.NaN()
	}
	eta := m.Intercept + dot(m.Coef, v[1:])
	varEta := 0.0
	if len(m.Cov) == (p+1)*(p+1) {
		varEta = math.Max(quadForm(m.Cov, v), 0)
	}
	level = fixLevel(level)
	if m.Kind == KindLogistic {
		h := NormalQuantile(0.5+level/2) * math.Sqrt(varEta)
		return Sigmoid(eta), Sigmoid(eta - h), Sigmoid(eta + h)
	}
	q := NormalQuantile(0.5 + level/2)
	if m.DF >= 1 {
		q = StudentTQuantile(0.5+level/2, m.DF)
	}
	h := q * math.Sqrt(m.ResidStd*m.ResidStd+varEta)
	return eta, eta - h, eta + h
}

// RawCoef converts the model to original (unstandardised) feature units:
// eta = intercept + sum_j coef[j]*x[j].
func (m *Model) RawCoef() (coef []float64, intercept float64) {
	coef = make([]float64, len(m.Coef))
	intercept = m.Intercept
	for j, c := range m.Coef {
		if j >= len(m.Scale) || j >= len(m.Mean) || m.Scale[j] == 0 {
			continue
		}
		coef[j] = c / m.Scale[j]
		intercept -= coef[j] * m.Mean[j]
	}
	return
}

func featureNames(names []string, k int) ([]string, error) {
	if names == nil {
		names = make([]string, k)
		for j := range names {
			names[j] = fmt.Sprintf("x%d", j)
		}
		return names, nil
	}
	if len(names) != k {
		return nil, ErrDimension
	}
	out := make([]string, k)
	copy(out, names)
	return out, nil
}

// design is a cleaned, standardised regression problem.
type design struct {
	n, k   int
	z      []float64 // n x ka standardised active columns, row-major
	y      []float64
	w      []float64 // weights normalised to sum n
	mean   []float64 // k
	scale  []float64 // k (1 for inactive)
	active []int     // indices of non-constant features
}

func newDesign(X [][]float64, y, w []float64) (*design, error) {
	rows, ys, ws, k, err := cleanRows(X, y, w)
	if err != nil {
		return nil, err
	}
	n := len(rows)
	if n < 2 {
		return nil, ErrInsufficientData
	}
	d := &design{n: n, k: k, y: ys, w: make([]float64, n), mean: make([]float64, k), scale: make([]float64, k)}
	if ws == nil {
		for i := range d.w {
			d.w[i] = 1
		}
	} else {
		sw := 0.0
		for _, v := range ws {
			sw += v
		}
		for i, v := range ws {
			d.w[i] = v * float64(n) / sw
		}
	}
	fn := float64(n)
	for i, r := range rows {
		for j, v := range r {
			d.mean[j] += d.w[i] * v
		}
	}
	for j := range d.mean {
		d.mean[j] /= fn
	}
	for i, r := range rows {
		for j, v := range r {
			d.scale[j] += d.w[i] * sq(v-d.mean[j])
		}
	}
	for j := range d.scale {
		s := math.Sqrt(d.scale[j] / fn)
		if s > 1e-12*math.Max(math.Abs(d.mean[j]), 1e-300) && IsFinite(s) {
			d.scale[j] = s
			d.active = append(d.active, j)
		} else {
			d.scale[j] = 1
		}
	}
	ka := len(d.active)
	d.z = make([]float64, n*ka)
	for i, r := range rows {
		zr := d.z[i*ka : (i+1)*ka]
		for a, j := range d.active {
			zr[a] = (r[j] - d.mean[j]) / d.scale[j]
		}
	}
	return d, nil
}

// Ridge fits a linear model with an L2 penalty on internally standardised
// features and an unpenalised intercept:
//
//	minimise  (1/n) * sum_i (y_i - b0 - z_i*b)^2  +  lambda * |b|^2
//
// where z are the features scaled to zero mean and unit variance. In this
// parametrisation lambda is dimensionless and independent of the sample
// size: 0 is ordinary least squares, 1 shrinks an uncorrelated feature's
// coefficient by half. The features are standardised with the (weighted)
// population standard deviation. The problem is solved by Householder QR of
// the augmented design, not through the normal equations, so nearly
// collinear features keep their accuracy.
//
// Rows with non-finite values are dropped. Constant features get a zero
// coefficient. names may be nil ("x0", "x1", ...). Errors: ErrDimension,
// ErrInvalidArgument (lambda < 0 or NaN), ErrInsufficientData (< 2 rows),
// ErrSingular (collinear features with lambda = 0, or more active features
// than rows-1 with lambda = 0).
func Ridge(X [][]float64, y []float64, names []string, lambda float64) (*Model, error) {
	return RidgeWeighted(X, y, nil, names, lambda)
}

// RidgeWeighted is Ridge with per-row weights (e.g. exponential forgetting).
// Weights are relative: they are rescaled to sum to the number of rows; rows
// with non-positive weight are dropped. w == nil means equal weights.
//
// ResidStd is the weighted residual standard deviation and Cov is the
// model-based covariance for precision weights (a row of weight w counts as
// w observations). With strongly unequal forgetting weights the coefficient
// uncertainty in Cov is therefore optimistic by roughly n/n_eff, n_eff =
// (sum w)^2 / sum w^2; the prediction interval is dominated by ResidStd and
// is hardly affected.
func RidgeWeighted(X [][]float64, y, w []float64, names []string, lambda float64) (*Model, error) {
	if !(lambda >= 0) || math.IsInf(lambda, 0) {
		return nil, ErrInvalidArgument
	}
	d, err := newDesign(X, y, w)
	if err != nil {
		return nil, err
	}
	names, err = featureNames(names, d.k)
	if err != nil {
		return nil, err
	}
	n, ka := d.n, len(d.active)
	fn := float64(n)
	if lambda == 0 && ka > n-1 {
		return nil, ErrSingular
	}
	ybar := 0.0
	for i, v := range d.y {
		ybar += d.w[i] * v
	}
	ybar /= fn

	m := &Model{
		Kind: KindLinear, Features: names, Mean: d.mean, Scale: d.scale,
		Coef: make([]float64, d.k), Intercept: ybar, Lambda: lambda, N: n, Converged: true,
	}
	var ainv *Matrix
	coefA := make([]float64, ka)
	if ka > 0 {
		// Solve the penalised problem as the augmented least-squares system
		//
		//	[ sqrt(w_i/n) * z_i ]       [ sqrt(w_i/n) * (y_i - ybar) ]
		//	[ sqrt(lambda) * I  ] b  =  [ 0                          ]
		//
		// by Householder QR. Its normal equations are the ridge equations
		// (Z'WZ/n + lambda*I) b = Z'W(y-ybar)/n, but working on the design
		// itself loses cond(Z) digits instead of cond(Z)^2, which matters
		// for strongly correlated features with a small penalty.
		rowsA := n
		if lambda > 0 {
			rowsA += ka
		}
		a := NewMatrix(rowsA, ka)
		rhs := make([]float64, rowsA)
		for i := 0; i < n; i++ {
			s := math.Sqrt(d.w[i] / fn)
			ar := a.Data[i*ka : (i+1)*ka]
			for p, zp := range d.z[i*ka : (i+1)*ka] {
				ar[p] = s * zp
			}
			rhs[i] = s * (d.y[i] - ybar)
		}
		if lambda > 0 {
			sl := math.Sqrt(lambda)
			for p := 0; p < ka; p++ {
				a.Data[(n+p)*ka+p] = sl
			}
		}
		qr, err := NewQR(a)
		if err != nil {
			return nil, err
		}
		if coefA, err = qr.Solve(rhs); err != nil {
			return nil, err
		}
		if ainv, err = qr.NormalInverse(); err != nil {
			return nil, err
		}
		for p, j := range d.active {
			if !IsFinite(coefA[p]) {
				return nil, ErrSingular
			}
			m.Coef[j] = coefA[p]
		}
	}

	sse, sst := 0.0, 0.0
	for i := 0; i < n; i++ {
		e := d.y[i] - ybar - dot(coefA, d.z[i*ka:(i+1)*ka])
		sse += d.w[i] * e * e
		sst += d.w[i] * sq(d.y[i]-ybar)
	}
	if sst > 0 {
		m.R2 = clamp(1-sse/sst, 0, 1)
	}
	// Effective degrees of freedom of the fit: trace(A^-1 * C), C = A - lambda*I.
	edf := float64(ka)
	for p := 0; p < ka; p++ {
		edf -= lambda * ainv.Data[p*ka+p]
	}
	m.DF = fn - 1 - edf
	if m.DF >= 1 {
		m.ResidStd = math.Sqrt(sse / m.DF)
	} else {
		m.DF = 0
		m.ResidStd = math.Sqrt(sse / fn)
	}
	// Covariance of (intercept, coef): sigma^2/n * blockdiag(1, A^-1 C A^-1)
	// with A^-1 C A^-1 = A^-1 - lambda*A^-1*A^-1.
	p1 := d.k + 1
	s2n := m.ResidStd * m.ResidStd / fn
	m.Cov = make([]float64, p1*p1)
	m.Cov[0] = s2n
	for p, jp := range d.active {
		for q, jq := range d.active {
			v := ainv.Data[p*ka+q]
			if lambda > 0 {
				v -= lambda * dot(ainv.Data[p*ka:(p+1)*ka], ainv.Data[q*ka:(q+1)*ka])
			}
			m.Cov[(jp+1)*p1+jq+1] = s2n * v
		}
	}
	return m, nil
}

// KFold splits the indices 0..n-1 into k interleaved test sets (index i
// belongs to fold i mod k). Suitable for data without serial dependence.
// k is clamped to [2, n]; nil is returned for n < 2.
func KFold(n, k int) [][]int {
	if n < 2 {
		return nil
	}
	k = min(max(k, 2), n)
	folds := make([][]int, k)
	for i := 0; i < n; i++ {
		folds[i%k] = append(folds[i%k], i)
	}
	return folds
}

// BlockFold splits the indices 0..n-1 into k contiguous blocks
// (leave-one-block-out), the appropriate scheme for autocorrelated time
// series: each block of consecutive rows is predicted from all the others.
// k is clamped to [2, n]; nil is returned for n < 2.
func BlockFold(n, k int) [][]int {
	if n < 2 {
		return nil
	}
	k = min(max(k, 2), n)
	folds := make([][]int, k)
	for b := 0; b < k; b++ {
		lo, hi := b*n/k, (b+1)*n/k
		for i := lo; i < hi; i++ {
			folds[b] = append(folds[b], i)
		}
	}
	return folds
}

// LogSpace returns n values spaced evenly on a log scale from lo to hi
// (both > 0). n < 2 returns {lo}.
func LogSpace(lo, hi float64, n int) []float64 {
	if n < 2 || !(lo > 0) || !(hi > 0) {
		return []float64{lo}
	}
	out := make([]float64, n)
	a, b := math.Log(lo), math.Log(hi)
	for i := range out {
		out[i] = math.Exp(a + (b-a)*float64(i)/float64(n-1))
	}
	out[0], out[n-1] = lo, hi
	return out
}

// CVResult reports a cross-validated search over ridge penalties.
type CVResult struct {
	Lambdas []float64 `json:"lambdas"` // candidates, as given
	MSE     []float64 `json:"mse"`     // mean squared out-of-fold error per candidate (-1: no valid fit)
	Best    float64   `json:"best"`    // candidate with the smallest MSE
	BestMSE float64   `json:"bestMse"`
	N       int       `json:"n"` // out-of-fold predictions scored per candidate
}

// CrossValidate runs fit on the complement of each fold and returns the
// mean of loss over all test indices. fit receives the training indices and
// returns a predictor for single indices; loss scores one prediction for
// index i. Folds whose fit fails are skipped; ErrInsufficientData is
// returned when no prediction could be scored. Non-finite losses are
// ignored.
func CrossValidate(n int, folds [][]int, fit func(train []int) (predict func(i int) float64, err error), loss func(i int, pred float64) float64) (float64, error) {
	if n < 2 || len(folds) < 2 || fit == nil || loss == nil {
		return 0, ErrInvalidArgument
	}
	total, cnt := 0.0, 0
	inTest := make([]bool, n)
	train := make([]int, 0, n)
	for _, test := range folds {
		for i := range inTest {
			inTest[i] = false
		}
		for _, i := range test {
			if i >= 0 && i < n {
				inTest[i] = true
			}
		}
		train = train[:0]
		for i := 0; i < n; i++ {
			if !inTest[i] {
				train = append(train, i)
			}
		}
		pred, err := fit(train)
		if err != nil || pred == nil {
			continue
		}
		for _, i := range test {
			if i < 0 || i >= n {
				continue
			}
			if l := loss(i, pred(i)); IsFinite(l) {
				total += l
				cnt++
			}
		}
	}
	if cnt == 0 {
		return 0, ErrInsufficientData
	}
	return total / float64(cnt), nil
}

// RidgeCV chooses the ridge penalty by cross-validation and refits on all
// rows. folds lists the test rows of each fold (use BlockFold for time
// series, KFold otherwise; indices refer to rows of X). For every candidate
// lambda the model is fitted on the complement of each fold and scored by
// the squared error on the fold; the candidate with the smallest mean
// squared error wins (ties: the larger penalty). Candidates for which no
// fold could be fitted get MSE -1. Errors: ErrInvalidArgument (no
// candidates, fewer than two folds), plus the errors of Ridge.
func RidgeCV(X [][]float64, y []float64, names []string, lambdas []float64, folds [][]int) (*Model, CVResult, error) {
	cv := CVResult{Lambdas: append([]float64(nil), lambdas...), MSE: make([]float64, len(lambdas))}
	if len(lambdas) == 0 || len(folds) < 2 {
		return nil, cv, ErrInvalidArgument
	}
	if len(X) != len(y) {
		return nil, cv, ErrDimension
	}
	n := len(X)
	sums := make([]float64, len(lambdas))
	counts := make([]int, len(lambdas))
	inTest := make([]bool, n)
	for _, test := range folds {
		for i := range inTest {
			inTest[i] = false
		}
		for _, i := range test {
			if i >= 0 && i < n {
				inTest[i] = true
			}
		}
		trX := make([][]float64, 0, n)
		trY := make([]float64, 0, n)
		for i := 0; i < n; i++ {
			if !inTest[i] {
				trX = append(trX, X[i])
				trY = append(trY, y[i])
			}
		}
		for li, lam := range lambdas {
			m, err := Ridge(trX, trY, nil, lam)
			if err != nil {
				continue
			}
		rows:
			for _, i := range test {
				if i < 0 || i >= n || !IsFinite(y[i]) || len(X[i]) != len(m.Coef) {
					continue
				}
				for _, v := range X[i] {
					if !IsFinite(v) {
						continue rows
					}
				}
				sums[li] += sq(y[i] - m.Predict(X[i]))
				counts[li]++
			}
		}
	}
	best := -1
	for li := range lambdas {
		if counts[li] == 0 {
			cv.MSE[li] = -1
			continue
		}
		cv.MSE[li] = sums[li] / float64(counts[li])
		if best < 0 || cv.MSE[li] < cv.MSE[best] ||
			(cv.MSE[li] == cv.MSE[best] && lambdas[li] > lambdas[best]) {
			best = li
		}
	}
	if best < 0 {
		return nil, cv, ErrInsufficientData
	}
	cv.Best, cv.BestMSE, cv.N = lambdas[best], cv.MSE[best], counts[best]
	m, err := Ridge(X, y, names, cv.Best)
	return m, cv, err
}

// LogisticOptions configures Logistic. The zero value is usable.
type LogisticOptions struct {
	// Lambda is the L2 penalty on the standardised coefficients: the fit
	// minimises mean log-loss + Lambda/2 * |b|^2 (intercept unpenalised).
	// A small positive value (e.g. 1e-3) keeps separable data finite.
	Lambda  float64
	MaxIter int       // IRLS iterations; default 50
	Tol     float64   // convergence threshold on the largest coefficient step; default 1e-8
	Weights []float64 // optional per-row weights (relative)
}

// Logistic fits a logistic regression for event probabilities by iteratively
// reweighted least squares (Newton steps with step halving) on internally
// standardised features. y holds the outcomes in [0, 1] (0/1 events, or
// observed fractions). Rows with non-finite values are dropped; constant
// features get a zero coefficient.
//
// With Lambda = 0 and perfectly separable classes the maximum likelihood
// estimate does not exist; the iteration then stops when the log-loss
// becomes negligible or MaxIter is reached and Model.Converged is false.
//
// Errors: ErrDimension, ErrInvalidArgument (y outside [0, 1], Lambda < 0),
// ErrInsufficientData (< 2 rows), ErrDegenerate (all outcomes 0 or all 1),
// ErrSingular (collinear features with Lambda = 0).
func Logistic(X [][]float64, y []float64, names []string, opt *LogisticOptions) (*Model, error) {
	o := LogisticOptions{}
	if opt != nil {
		o = *opt
	}
	if !(o.Lambda >= 0) || math.IsInf(o.Lambda, 0) {
		return nil, ErrInvalidArgument
	}
	if o.MaxIter <= 0 {
		o.MaxIter = 50
	}
	if !(o.Tol > 0) {
		o.Tol = 1e-8
	}
	d, err := newDesign(X, y, o.Weights)
	if err != nil {
		return nil, err
	}
	names, err = featureNames(names, d.k)
	if err != nil {
		return nil, err
	}
	n, ka := d.n, len(d.active)
	fn := float64(n)
	ybar := 0.0
	for i, v := range d.y {
		if v < 0 || v > 1 {
			return nil, ErrInvalidArgument
		}
		ybar += d.w[i] * v
	}
	ybar /= fn
	if !(ybar > 0 && ybar < 1) {
		return nil, ErrDegenerate
	}

	p := ka + 1
	theta := make([]float64, p) // intercept, active coefficients
	theta[0] = Logit(ybar)
	eta := func(th []float64, i int) float64 {
		return th[0] + dot(th[1:], d.z[i*ka:(i+1)*ka])
	}
	objective := func(th []float64) float64 {
		s := 0.0
		for i := 0; i < n; i++ {
			e := eta(th, i)
			// log(1+exp(e)) - y*e, computed stably.
			s += d.w[i] * (math.Max(e, 0) + math.Log1p(math.Exp(-math.Abs(e))) - d.y[i]*e)
		}
		pen := 0.0
		for _, b := range th[1:] {
			pen += b * b
		}
		return s/fn + 0.5*o.Lambda*pen
	}
	nullLoss := -(ybar*math.Log(ybar) + (1-ybar)*math.Log(1-ybar))

	h := NewMatrix(p, p)
	g := make([]float64, p)
	trial := make([]float64, p)
	row := make([]float64, p)
	build := func(th []float64) {
		for i := range h.Data {
			h.Data[i] = 0
		}
		for i := range g {
			g[i] = 0
		}
		row[0] = 1
		for i := 0; i < n; i++ {
			copy(row[1:], d.z[i*ka:(i+1)*ka])
			pr := Sigmoid(eta(th, i))
			wi := d.w[i] / fn
			r := wi * (pr - d.y[i])
			v := wi * pr * (1 - pr)
			for a := 0; a < p; a++ {
				g[a] += r * row[a]
				va := v * row[a]
				hr := h.Data[a*p : a*p+a+1]
				for b := range hr {
					hr[b] += va * row[b]
				}
			}
		}
		for a := 1; a < p; a++ {
			g[a] += o.Lambda * th[a]
			h.Data[a*p+a] += o.Lambda
		}
		for a := 0; a < p; a++ {
			for b := 0; b < a; b++ {
				h.Data[b*p+a] = h.Data[a*p+b]
			}
		}
	}

	m := &Model{
		Kind: KindLogistic, Features: names, Mean: d.mean, Scale: d.scale,
		Coef: make([]float64, d.k), Lambda: o.Lambda, N: n, DF: fn - float64(p),
	}
	obj := objective(theta)
	for it := 1; it <= o.MaxIter; it++ {
		m.Iter = it
		build(theta)
		l, err := Cholesky(h)
		if err != nil {
			if o.Lambda == 0 && obj < 1e-6 {
				break // separable data: likelihood is flat, stop quietly
			}
			return nil, ErrSingular
		}
		step, err := CholeskySolve(l, g)
		if err != nil {
			return nil, err
		}
		// Step halving until the objective does not increase.
		s := 1.0
		newObj := math.Inf(1)
		for k := 0; k < 30; k++ {
			for a := range trial {
				trial[a] = theta[a] - s*step[a]
			}
			newObj = objective(trial)
			if newObj <= obj+1e-14 {
				break
			}
			s /= 2
		}
		if !(newObj <= obj+1e-14) {
			m.Converged = true // no descent possible: at the optimum to rounding
			break
		}
		maxStep := 0.0
		for a := range theta {
			maxStep = math.Max(maxStep, math.Abs(theta[a]-trial[a]))
			theta[a] = trial[a]
		}
		decrease := obj - newObj
		obj = newObj
		if maxStep < o.Tol || decrease <= 1e-13*(math.Abs(obj)+1e-13) && maxStep < 1e-4 {
			m.Converged = true
			break
		}
		if o.Lambda == 0 && obj < 1e-9 {
			break // separable: coefficients diverge
		}
	}
	for a := range theta {
		if !IsFinite(theta[a]) {
			return nil, ErrSingular
		}
	}
	m.Intercept = theta[0]
	for a, j := range d.active {
		m.Coef[j] = theta[a+1]
	}
	// Report the unpenalised mean log-loss.
	pen := 0.0
	for _, b := range theta[1:] {
		pen += b * b
	}
	m.LogLoss = math.Max(obj-0.5*o.Lambda*pen, 0)
	if nullLoss > 0 {
		m.R2 = clamp(1-m.LogLoss/nullLoss, 0, 1)
	}
	// Covariance of the estimates: inverse of n*H at the solution.
	build(theta)
	if l, err := Cholesky(h); err == nil {
		if inv, err := CholeskyInverse(l); err == nil {
			p1 := d.k + 1
			cov := make([]float64, p1*p1)
			idx := func(a int) int {
				if a == 0 {
					return 0
				}
				return d.active[a-1] + 1
			}
			okCov := true
			for a := 0; a < p; a++ {
				for b := 0; b < p; b++ {
					v := inv.Data[a*p+b] / fn
					if !IsFinite(v) {
						okCov = false
					}
					cov[idx(a)*p1+idx(b)] = v
				}
			}
			if okCov {
				m.Cov = cov
			}
		}
	}
	return m, nil
}
