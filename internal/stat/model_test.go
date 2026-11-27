package stat

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

// ----------------------------------------------------------------- linalg

func TestLinalg(t *testing.T) {
	a, err := MatrixFromRows([][]float64{{4, 12, -16}, {12, 37, -43}, {-16, -43, 98}})
	if err != nil {
		t.Fatal(err)
	}
	l, err := Cholesky(a)
	if err != nil {
		t.Fatal(err)
	}
	// Textbook factor: [[2,0,0],[6,1,0],[-8,5,3]].
	want := []float64{2, 0, 0, 6, 1, 0, -8, 5, 3}
	for i, v := range want {
		approx(t, "chol", l.Data[i], v, 1e-12)
	}
	b := []float64{1, 2, 3}
	x, _ := CholeskySolve(l, b)
	ax, _ := a.MulVec(x)
	for i := range b {
		approx(t, "chol solve", ax[i], b[i], 1e-9)
	}
	inv, _ := CholeskyInverse(l)
	id, _ := a.Mul(inv)
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			w := 0.0
			if i == j {
				w = 1
			}
			approx(t, "inverse", id.At(i, j), w, 1e-9)
		}
	}
	x2, err := SolveSPD(a, b)
	if err != nil || math.Abs(x2[0]-x[0]) > 1e-12 {
		t.Error("SolveSPD")
	}
	if _, err := Cholesky(&Matrix{Rows: 2, Cols: 2, Data: []float64{1, 2, 2, 1}}); !errors.Is(err, ErrSingular) {
		t.Error("indefinite should fail", err)
	}
	if _, err := Cholesky(NewMatrix(2, 3)); !errors.Is(err, ErrDimension) {
		t.Error("non-square", err)
	}

	// General solve with pivoting (zero on the diagonal).
	g, _ := MatrixFromRows([][]float64{{0, 2, 1}, {1, 1, 1}, {2, 0, -1}})
	sol, err := Solve(g, []float64{7, 6, -1})
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range []float64{1, 2, 3} {
		approx(t, "solve", sol[i], w, 1e-12)
	}
	sing, _ := MatrixFromRows([][]float64{{1, 2}, {2, 4}})
	if _, err := Solve(sing, []float64{1, 2}); !errors.Is(err, ErrSingular) {
		t.Error("singular", err)
	}
	if _, err := Solve(NewMatrix(2, 2), []float64{1, 2}); !errors.Is(err, ErrSingular) {
		t.Error("zero matrix", err)
	}

	// QR least squares: overdetermined consistent system.
	m, _ := MatrixFromRows([][]float64{{1, 1}, {1, 2}, {1, 3}, {1, 4}})
	ls, err := LeastSquares(m, []float64{6, 5, 7, 10})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "ls intercept", ls[0], 3.5, 1e-12)
	approx(t, "ls slope", ls[1], 1.4, 1e-12)
	qr, _ := NewQR(m)
	ni, _ := qr.NormalInverse()
	mtm, _ := m.T().Mul(m)
	id, _ = mtm.Mul(ni)
	approx(t, "normal inverse 00", id.At(0, 0), 1, 1e-12)
	approx(t, "normal inverse 01", id.At(0, 1), 0, 1e-12)
	approx(t, "normal inverse 11", id.At(1, 1), 1, 1e-12)
	rd, _ := MatrixFromRows([][]float64{{1, 2}, {2, 4}, {3, 6}})
	if _, err := LeastSquares(rd, []float64{1, 2, 3}); !errors.Is(err, ErrSingular) {
		t.Error("rank deficient", err)
	}
	if _, err := NewQR(NewMatrix(1, 2)); !errors.Is(err, ErrDimension) {
		t.Error("wide matrix", err)
	}
	if _, err := MatrixFromRows([][]float64{{1, 2}, {3}}); !errors.Is(err, ErrDimension) {
		t.Error("ragged", err)
	}
	c := m.Clone()
	c.Set(0, 0, 99)
	if m.At(0, 0) != 1 || c.Row(0)[0] != 99 {
		t.Error("clone")
	}
}

// ------------------------------------------------------------- regression

func TestLinearRegressionAnscombe(t *testing.T) {
	xs := [][]float64{anscombeX, anscombeX, anscombeX, anscombeX4}
	ys := [][]float64{anscombeY1, anscombeY2, anscombeY3, anscombeY4}
	for i := range xs {
		f, err := LinearRegression(xs[i], ys[i])
		if err != nil {
			t.Fatal(err)
		}
		approx(t, "slope", f.Slope, 0.500, 0.001)
		approx(t, "intercept", f.Intercept, 3.00, 0.003)
		approx(t, "r2", f.R2, 0.667, 0.001)
	}
	// Set I against R's lm() output.
	f, _ := LinearRegression(anscombeX, anscombeY1)
	approx(t, "slope", f.Slope, 55.01/110, 1e-13)
	approx(t, "intercept", f.Intercept, 82.51/11-9*55.01/110, 1e-12)
	approx(t, "r2", f.R2, 0.6665425, 1e-6)
	approx(t, "slope se", f.SlopeSE, 0.1179055, 1e-6)
	approx(t, "intercept se", f.InterceptSE, 1.1247468, 1e-6)
	approx(t, "t", f.T, 4.24146, 1e-4)
	approx(t, "p", f.P, 0.00216963, 1e-6)
	approx(t, "resid std", f.ResidStd, 1.236603, 1e-6)
	if f.N != 11 {
		t.Error("n")
	}
	// Intervals: at the mean of x the half widths have closed forms.
	tq := StudentTQuantile(0.975, 9)
	yh, lo, hi := f.PredictInterval(9, 0.95)
	approx(t, "yhat", yh, 82.51/11, 1e-12)
	approx(t, "pi", hi-lo, 2*tq*f.ResidStd*math.Sqrt(1+1.0/11), 1e-10)
	_, clo, chi := f.ConfidenceInterval(9, 0.95)
	approx(t, "ci", chi-clo, 2*tq*f.ResidStd*math.Sqrt(1.0/11), 1e-10)
	_, lo2, hi2 := f.PredictInterval(20, 0.95)
	if !(hi2-lo2 > hi-lo) {
		t.Error("interval should widen away from the mean")
	}
	if _, err := json.Marshal(f); err != nil {
		t.Error(err)
	}
}

func TestLinearRegressionEdges(t *testing.T) {
	// Exact line with a missing point.
	f, err := LinearRegression([]float64{0, 1, 2, nan, 4}, []float64{1, 3, 5, 7, 9})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "slope", f.Slope, 2, 1e-14)
	approx(t, "intercept", f.Intercept, 1, 1e-14)
	approx(t, "r2", f.R2, 1, 1e-14)
	if f.P != 0 || f.N != 4 || math.IsInf(f.T, 0) || math.IsNaN(f.T) {
		t.Errorf("exact fit: %+v", f)
	}
	// Constant y: slope 0, p = 1, R2 = 0.
	f, err = LinearRegression([]float64{1, 2, 3, 4}, []float64{5, 5, 5, 5})
	if err != nil || f.Slope != 0 || f.P != 1 || f.R2 != 0 {
		t.Errorf("constant y: %+v %v", f, err)
	}
	if _, err := LinearRegression([]float64{1, 1, 1}, []float64{1, 2, 3}); !errors.Is(err, ErrSingular) {
		t.Error("constant x", err)
	}
	if _, err := LinearRegression([]float64{1, 2}, []float64{1, 2}); !errors.Is(err, ErrInsufficientData) {
		t.Error("n<3", err)
	}
	if _, err := LinearRegression([]float64{1, 2}, []float64{1}); !errors.Is(err, ErrDimension) {
		t.Error("dimension", err)
	}
	if _, err := LinearRegression(nil, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("nil", err)
	}
	_, lo, _ := LinearFit{}.PredictInterval(1, 0.9)
	wantNaN(t, "zero fit interval", lo)
}

func TestTheilSen(t *testing.T) {
	r := newRand(6)
	var x, y []float64
	for i := 0; i < 200; i++ {
		xi := float64(i) / 10
		yi := 3 - 0.7*xi + 0.05*r.NormFloat64()
		if i%5 == 0 { // 20 % gross outliers
			yi += 50 + 20*r.Float64()
		}
		x, y = append(x, xi), append(y, yi)
	}
	x = append(x, nan)
	y = append(y, 1)
	l, err := TheilSen(x, y)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "slope", l.Slope, -0.7, 0.02)
	approx(t, "intercept", l.Intercept, 3, 0.1)
	approx(t, "predict", l.Predict(10), -4, 0.15)
	if l.N != 200 || !(l.ResidStd < 0.2) {
		t.Errorf("%+v", l)
	}
	// OLS is dragged away by the same outliers.
	f, _ := LinearRegression(x, y)
	if math.Abs(f.Intercept-3) < 5 {
		t.Errorf("expected OLS to be biased, intercept %v", f.Intercept)
	}
	// Exact line.
	l, _ = TheilSen([]float64{1, 2, 3}, []float64{5, 7, 9})
	approx(t, "exact slope", l.Slope, 2, 1e-14)
	approx(t, "exact intercept", l.Intercept, 3, 1e-14)
	// Large n takes the sampled path.
	var bx, by []float64
	for i := 0; i < 3000; i++ {
		bx = append(bx, float64(i))
		by = append(by, 0.25*float64(i)-4+r.NormFloat64())
	}
	l, err = TheilSen(bx, by)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "sampled slope", l.Slope, 0.25, 0.001)
	approx(t, "sampled intercept", l.Intercept, -4, 0.3)
	if _, err := TheilSen([]float64{2, 2, 2}, []float64{1, 2, 3}); !errors.Is(err, ErrSingular) {
		t.Error("constant x", err)
	}
	if _, err := TheilSen([]float64{1}, []float64{1}); !errors.Is(err, ErrInsufficientData) {
		t.Error("n<2", err)
	}
}

// planted builds a design with k features and y = b0 + sum b_j x_j + noise.
func planted(seed uint64, n int, b0 float64, beta []float64, noise float64) (X [][]float64, y []float64) {
	r := newRand(seed)
	for i := 0; i < n; i++ {
		row := make([]float64, len(beta))
		v := b0
		for j := range row {
			row[j] = r.NormFloat64()*float64(j+1) + float64(10*j)
			v += beta[j] * row[j]
		}
		X = append(X, row)
		y = append(y, v+noise*r.NormFloat64())
	}
	return
}

func TestOLSExactRecovery(t *testing.T) {
	beta := []float64{2, -3, 0.5, 0}
	X, y := planted(7, 60, 1.5, beta, 0)
	// Rows with missing values must be ignored.
	X = append(X, []float64{nan, 1, 1, 1}, []float64{1, 1, 1, 1}, []float64{1, 1, math.Inf(1), 1})
	y = append(y, 1, nan, 3)
	res, err := OLS(X, y, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.N != 60 || res.DF != 55 || res.NumFeatures() != 4 {
		t.Errorf("n=%d df=%d", res.N, res.DF)
	}
	approx(t, "intercept", res.Coef[0], 1.5, 1e-9)
	for j, b := range beta {
		approx(t, "coef", res.Coef[j+1], b, 1e-10)
	}
	approx(t, "r2", res.R2, 1, 1e-12)
	approx(t, "adj r2", res.AdjR2, 1, 1e-12)
	approx(t, "resid std", res.ResidStd, 0, 1e-9)
	approx(t, "predict", res.Predict([]float64{1, 2, 3, 4}), 1.5+2-6+1.5, 1e-9)
	wantNaN(t, "predict wrong len", res.Predict([]float64{1}))
	if _, err := json.Marshal(res); err != nil {
		t.Error("OLSResult must be JSON-safe:", err)
	}
	// Without intercept.
	X2, y2 := planted(8, 30, 0, []float64{4, -1}, 0)
	res, err = OLS(X2, y2, false)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "no-intercept coef0", res.Coef[0], 4, 1e-10)
	approx(t, "no-intercept coef1", res.Coef[1], -1, 1e-10)
	if len(res.Coef) != 2 {
		t.Error("coef len")
	}
}

func TestOLSInference(t *testing.T) {
	// A one-feature OLS must agree with the closed-form simple regression.
	X := make([][]float64, len(anscombeX))
	for i, v := range anscombeX {
		X[i] = []float64{v}
	}
	res, err := OLS(X, anscombeY1, true)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := LinearRegression(anscombeX, anscombeY1)
	approx(t, "intercept", res.Coef[0], f.Intercept, 1e-12)
	approx(t, "slope", res.Coef[1], f.Slope, 1e-12)
	approx(t, "se intercept", res.SE[0], f.InterceptSE, 1e-12)
	approx(t, "se slope", res.SE[1], f.SlopeSE, 1e-12)
	approx(t, "t", res.T[1], f.T, 1e-10)
	approx(t, "p", res.P[1], f.P, 1e-12)
	approx(t, "p intercept", res.P[0], 0.02573, 1e-5) // R: 0.02573
	approx(t, "r2", res.R2, f.R2, 1e-12)
	approx(t, "adj r2", res.AdjR2, 0.6295, 1e-4) // R: 0.6295
	approx(t, "resid std", res.ResidStd, f.ResidStd, 1e-12)
	approx(t, "F", res.F, 17.99, 0.005) // R: 17.99 on 1 and 9 DF
	approx(t, "F p", res.FP, f.P, 1e-12)
	approx(t, "sse+ssr", res.SST, 10*Variance(anscombeY1), 1e-10)
	for _, x0 := range []float64{4, 9, 25} {
		y1, lo1, hi1 := res.PredictInterval([]float64{x0}, 0.9)
		y2, lo2, hi2 := f.PredictInterval(x0, 0.9)
		approx(t, "pi yhat", y1, y2, 1e-12)
		approx(t, "pi lo", lo1, lo2, 1e-10)
		approx(t, "pi hi", hi1, hi2, 1e-10)
	}

	// Noisy planted model: estimates within a few standard errors, the null
	// coefficient is insignificant, the real ones are highly significant.
	beta := []float64{2, -3, 0.5, 0}
	Xn, yn := planted(9, 500, 1.5, beta, 2)
	res, err = OLS(Xn, yn, true)
	if err != nil {
		t.Fatal(err)
	}
	for j, b := range beta {
		if math.Abs(res.Coef[j+1]-b) > 4*res.SE[j+1] {
			t.Errorf("coef %d = %v +- %v, want %v", j, res.Coef[j+1], res.SE[j+1], b)
		}
	}
	approx(t, "resid std", res.ResidStd, 2, 0.2)
	if !(res.P[1] < 1e-10) || !(res.P[2] < 1e-10) || !(res.P[4] > 0.01) {
		t.Errorf("p-values %v", res.P)
	}
	if !(res.AdjR2 < res.R2) || !(res.FP < 1e-10) {
		t.Errorf("adjR2 %v r2 %v Fp %v", res.AdjR2, res.R2, res.FP)
	}
	// Empirical coverage of the 90 % prediction interval on fresh data.
	Xt, yt := planted(10, 2000, 1.5, beta, 2)
	inside := 0
	for i := range Xt {
		_, lo, hi := res.PredictInterval(Xt[i], 0.9)
		if yt[i] >= lo && yt[i] <= hi {
			inside++
		}
	}
	approx(t, "coverage", float64(inside)/2000, 0.9, 0.03)
}

func TestOLSErrors(t *testing.T) {
	// Collinear columns.
	X := [][]float64{{1, 2}, {2, 4}, {3, 6}, {4, 8}, {5, 10}}
	y := []float64{1, 2, 3, 4, 5}
	if _, err := OLS(X, y, true); !errors.Is(err, ErrSingular) {
		t.Error("collinear", err)
	}
	// Constant column duplicates the intercept.
	if _, err := OLS([][]float64{{1, 3}, {1, 4}, {1, 9}, {1, 2}}, []float64{1, 2, 3, 4}, true); !errors.Is(err, ErrSingular) {
		t.Error("constant column", err)
	}
	if _, err := OLS(X[:3], y[:3], true); !errors.Is(err, ErrInsufficientData) {
		t.Error("n<=p", err)
	}
	if _, err := OLS(X, y[:2], true); !errors.Is(err, ErrDimension) {
		t.Error("len mismatch", err)
	}
	if _, err := OLS([][]float64{{1, 2}, {1}}, []float64{1, 2}, true); !errors.Is(err, ErrDimension) {
		t.Error("ragged", err)
	}
	if _, err := OLS(nil, nil, true); !errors.Is(err, ErrInsufficientData) {
		t.Error("empty", err)
	}
	if _, err := OLS([][]float64{{}, {}, {}}, []float64{1, 2, 3}, false); !errors.Is(err, ErrInvalidArgument) {
		t.Error("no columns", err)
	}
	// Intercept-only model is the mean.
	res, err := OLS([][]float64{{}, {}, {}}, []float64{1, 2, 6}, true)
	if err != nil || math.Abs(res.Coef[0]-3) > 1e-12 || res.R2 != 0 {
		t.Errorf("intercept only: %+v %v", res, err)
	}
	if _, err := WLS(X, y, nil, true); !errors.Is(err, ErrDimension) {
		t.Error("nil weights", err)
	}
}

func TestWLS(t *testing.T) {
	X, y := planted(11, 40, 2, []float64{1, -2}, 1)
	ones := make([]float64, len(y))
	for i := range ones {
		ones[i] = 1
	}
	o, _ := OLS(X, y, true)
	w, err := WLS(X, y, ones, true)
	if err != nil {
		t.Fatal(err)
	}
	for j := range o.Coef {
		approx(t, "unit weights coef", w.Coef[j], o.Coef[j], 1e-11)
		approx(t, "unit weights se", w.SE[j], o.SE[j], 1e-11)
	}
	approx(t, "unit weights r2", w.R2, o.R2, 1e-12)

	// Integer weights are equivalent to repeating rows (coefficients, SSE).
	wt := make([]float64, len(y))
	var Xr [][]float64
	var yr []float64
	for i := range y {
		wt[i] = float64(1 + i%3)
		for k := 0; k < 1+i%3; k++ {
			Xr = append(Xr, X[i])
			yr = append(yr, y[i])
		}
	}
	wt[3] = 0 // dropped
	wt[4] = nan
	var Xr2 [][]float64
	var yr2 []float64
	for i := range y {
		if i == 3 || i == 4 {
			continue
		}
		for k := 0; k < 1+i%3; k++ {
			Xr2 = append(Xr2, X[i])
			yr2 = append(yr2, y[i])
		}
	}
	w, err = WLS(X, y, wt, true)
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := OLS(Xr2, yr2, true)
	if w.N != 38 {
		t.Errorf("n = %d", w.N)
	}
	for j := range rep.Coef {
		approx(t, "weighted coef", w.Coef[j], rep.Coef[j], 1e-10)
	}
	approx(t, "weighted sse", w.SSE, rep.SSE, 1e-9)
	approx(t, "weighted r2", w.R2, rep.R2, 1e-11)
	_, _ = Xr, yr

	// Heteroscedastic exact recovery: weights do not matter without noise.
	Xe, ye := planted(12, 20, -1, []float64{3}, 0)
	we := make([]float64, 20)
	for i := range we {
		we[i] = float64(i + 1)
	}
	w, _ = WLS(Xe, ye, we, true)
	approx(t, "exact intercept", w.Coef[0], -1, 1e-10)
	approx(t, "exact slope", w.Coef[1], 3, 1e-10)
}

// ------------------------------------------------------------------ ridge

func TestRidgeMatchesOLS(t *testing.T) {
	beta := []float64{2, -3, 0.5}
	X, y := planted(13, 80, 1.5, beta, 0.5)
	o, _ := OLS(X, y, true)
	m, err := Ridge(X, y, []string{"a", "b", "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	coef, b0 := m.RawCoef()
	approx(t, "intercept", b0, o.Coef[0], 1e-9)
	for j := range coef {
		approx(t, "coef", coef[j], o.Coef[j+1], 1e-10)
	}
	approx(t, "r2", m.R2, o.R2, 1e-11)
	approx(t, "resid std", m.ResidStd, o.ResidStd, 1e-10)
	approx(t, "df", m.DF, float64(o.DF), 1e-9)
	for _, x := range [][]float64{{0, 10, 20}, {3, 14, 11}, {-5, 2, 40}} {
		approx(t, "predict", m.Predict(x), o.Predict(x), 1e-9)
		p1, lo1, hi1 := m.PredictInterval(x, 0.9)
		p2, lo2, hi2 := o.PredictInterval(x, 0.9)
		approx(t, "pi mean", p1, p2, 1e-9)
		approx(t, "pi lo", lo1, lo2, 1e-8)
		approx(t, "pi hi", hi1, hi2, 1e-8)
	}

	// Exact recovery of a planted model with noise = 0.
	Xe, ye := planted(14, 50, -4, beta, 0)
	m, _ = Ridge(Xe, ye, nil, 0)
	coef, b0 = m.RawCoef()
	approx(t, "exact intercept", b0, -4, 1e-8)
	for j := range coef {
		approx(t, "exact coef", coef[j], beta[j], 1e-9)
	}
	if m.Features[2] != "x2" || m.Kind != KindLinear || m.NumFeatures() != 3 {
		t.Errorf("%+v", m)
	}
}

func TestRidgeShrinkage(t *testing.T) {
	// One feature: the standardised coefficient is cov(x,y)/sd(x)/(1+lambda)
	// (population moments), and the intercept is the mean of y.
	x, y := anscombeX, anscombeY1
	X := make([][]float64, len(x))
	for i, v := range x {
		X[i] = []float64{v}
	}
	sx := PopStd(x)
	covp := Covariance(x, y) * 10 / 11
	for _, lam := range []float64{0, 0.1, 1, 10} {
		m, err := Ridge(X, y, nil, lam)
		if err != nil {
			t.Fatal(err)
		}
		approx(t, "coef", m.Coef[0], covp/sx/(1+lam), 1e-12)
		approx(t, "intercept", m.Intercept, Mean(y), 1e-12)
		approx(t, "mean", m.Mean[0], 9, 1e-12)
		approx(t, "scale", m.Scale[0], sx, 1e-12)
		approx(t, "predict at mean", m.Predict([]float64{9}), Mean(y), 1e-12)
		// Effective df of the fit is 1/(1+lambda).
		approx(t, "df", m.DF, 11-1-1/(1+lam), 1e-10)
	}
	// Shrinkage is monotone in lambda and R2 falls.
	Xn, yn := planted(15, 100, 0, []float64{1, 1, 1}, 1)
	prevNorm, prevR2 := math.Inf(1), 2.0
	for _, lam := range []float64{0, 0.01, 0.1, 1, 10, 100} {
		m, _ := Ridge(Xn, yn, nil, lam)
		norm := 0.0
		for _, c := range m.Coef {
			norm += c * c
		}
		if !(norm < prevNorm) || !(m.R2 <= prevR2) {
			t.Errorf("lambda %v: norm %v (prev %v) r2 %v (prev %v)", lam, norm, prevNorm, m.R2, prevR2)
		}
		prevNorm, prevR2 = norm, m.R2
	}
}

func TestRidgeDegenerate(t *testing.T) {
	// Collinear features: OLS limit fails, any ridge penalty resolves it and
	// splits the weight evenly between the two copies.
	var X [][]float64
	var y []float64
	r := newRand(16)
	for i := 0; i < 50; i++ {
		v := r.NormFloat64()
		X = append(X, []float64{v, 2 * v, 7})
		y = append(y, 3*v+1)
	}
	if _, err := Ridge(X, y, nil, 0); !errors.Is(err, ErrSingular) {
		t.Error("collinear lambda=0", err)
	}
	m, err := Ridge(X, y, nil, 1e-6)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "split", m.Coef[0], m.Coef[1], 1e-6)
	if m.Coef[2] != 0 || m.Scale[2] != 1 {
		t.Errorf("constant feature: coef %v scale %v", m.Coef[2], m.Scale[2])
	}
	approx(t, "predict", m.Predict([]float64{1, 2, 7}), 4, 1e-4)
	// Missing features are imputed with the training mean.
	approx(t, "impute", m.Predict([]float64{nan, nan, nan}), m.Intercept, 1e-12)
	wantNaN(t, "wrong length", m.Predict([]float64{1}))
	_, lo, _ := m.PredictInterval([]float64{1}, 0.9)
	wantNaN(t, "interval wrong length", lo)

	// Only constant features: the model is the mean.
	m, err = Ridge([][]float64{{1}, {1}, {1}}, []float64{2, 4, 9}, nil, 0)
	if err != nil || m.Intercept != 5 || m.Coef[0] != 0 {
		t.Errorf("constant-only: %+v %v", m, err)
	}
	if _, err := Ridge(X, y, nil, -1); !errors.Is(err, ErrInvalidArgument) {
		t.Error("negative lambda", err)
	}
	if _, err := Ridge(X, y, nil, nan); !errors.Is(err, ErrInvalidArgument) {
		t.Error("nan lambda", err)
	}
	if _, err := Ridge(X, y, []string{"a"}, 1); !errors.Is(err, ErrDimension) {
		t.Error("names", err)
	}
	if _, err := Ridge(X[:1], y[:1], nil, 1); !errors.Is(err, ErrInsufficientData) {
		t.Error("one row", err)
	}
	if _, err := Ridge(nil, nil, nil, 1); !errors.Is(err, ErrInsufficientData) {
		t.Error("empty", err)
	}
	// More features than rows needs a penalty.
	wide := [][]float64{{1, 2, 3, 4}, {2, 1, 4, 3}, {0, 5, 1, 2}}
	if _, err := Ridge(wide, []float64{1, 2, 3}, nil, 0); !errors.Is(err, ErrSingular) {
		t.Error("p>n lambda=0", err)
	}
	if _, err := Ridge(wide, []float64{1, 2, 3}, nil, 0.1); err != nil {
		t.Error("p>n with penalty", err)
	}
}

func TestRidgeWeighted(t *testing.T) {
	X, y := planted(17, 30, 1, []float64{2}, 1)
	var Xr [][]float64
	var yr, w []float64
	for i := range y {
		k := 1 + i%4
		w = append(w, float64(k)*0.37) // weights are relative
		for j := 0; j < k; j++ {
			Xr = append(Xr, X[i])
			yr = append(yr, y[i])
		}
	}
	mw, err := RidgeWeighted(X, y, w, nil, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	mr, _ := Ridge(Xr, yr, nil, 0.3)
	cw, iw := mw.RawCoef()
	cr, ir := mr.RawCoef()
	approx(t, "coef", cw[0], cr[0], 1e-10)
	approx(t, "intercept", iw, ir, 1e-10)
	approx(t, "r2", mw.R2, mr.R2, 1e-10)
}

func TestModelJSON(t *testing.T) {
	X, y := planted(18, 120, 3, []float64{1, -2, 0.25}, 0.7)
	m, err := Ridge(X, y, []string{"temp", "wind", "cloud"}, 0.05)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := ParseModel(data)
	if err != nil {
		t.Fatal(err)
	}
	x := []float64{0.3, 11, 19}
	if m.Predict(x) != m2.Predict(x) {
		t.Errorf("prediction changed after JSON round trip: %v vs %v", m.Predict(x), m2.Predict(x))
	}
	p1, l1, h1 := m.PredictInterval(x, 0.8)
	p2, l2, h2 := m2.PredictInterval(x, 0.8)
	if p1 != p2 || l1 != l2 || h1 != h2 || !(l1 < p1 && p1 < h1) {
		t.Error("interval changed after JSON round trip")
	}
	approx(t, "PredictMap", m2.PredictMap(map[string]float64{"temp": 0.3, "wind": 11, "cloud": 19, "other": 5}), m.Predict(x), 1e-12)
	approx(t, "PredictMap missing", m2.PredictMap(map[string]float64{"temp": 0.3, "wind": 11}), m.Predict([]float64{0.3, 11, nan}), 1e-12)
	// Without the covariance the interval still works (residual spread only).
	m2.Cov = nil
	_, l3, h3 := m2.PredictInterval(x, 0.8)
	if !(h3-l3 > 0) || !(h3-l3 <= h1-l1) {
		t.Errorf("interval without cov: %v vs %v", h3-l3, h1-l1)
	}

	for _, bad := range []string{
		`{`,
		`{"kind":"poisson","features":[],"mean":[],"scale":[],"coef":[]}`,
		`{"kind":"linear","features":["a"],"mean":[],"scale":[1],"coef":[1]}`,
		`{"kind":"linear","features":["a"],"mean":[0],"scale":[0],"coef":[1]}`,
		`{"kind":"linear","features":["a"],"mean":[0],"scale":[1],"coef":[1],"cov":[1,2,3]}`,
	} {
		if _, err := ParseModel([]byte(bad)); err == nil {
			t.Errorf("ParseModel(%s) should fail", bad)
		}
	}
	var nilModel *Model
	if nilModel.Validate() == nil {
		t.Error("nil model")
	}
}

func TestRidgeCV(t *testing.T) {
	// Few rows, many noisy and partly collinear features: some shrinkage
	// must beat the unpenalised fit out of sample.
	r := newRand(19)
	gen := func(n int) (X [][]float64, y []float64) {
		for i := 0; i < n; i++ {
			row := make([]float64, 12)
			base := r.NormFloat64()
			for j := range row {
				row[j] = 0.9*base + 0.45*r.NormFloat64()
			}
			X = append(X, row)
			y = append(y, 2*base+r.NormFloat64())
		}
		return
	}
	X, y := gen(40)
	X[7][3] = nan // a row with a missing feature is skipped everywhere
	lambdas := append([]float64{0}, LogSpace(1e-3, 10, 9)...)
	for _, folds := range [][][]int{BlockFold(len(y), 5), KFold(len(y), 5)} {
		m, cv, err := RidgeCV(X, y, nil, lambdas, folds)
		if err != nil {
			t.Fatal(err)
		}
		if cv.Best <= 0 || m.Lambda != cv.Best || len(cv.MSE) != len(lambdas) || cv.N != 39 {
			t.Errorf("cv = %+v", cv)
		}
		if !(cv.BestMSE < cv.MSE[0]) {
			t.Errorf("penalty should beat OLS out of sample: %v", cv.MSE)
		}
		// The chosen model generalises better than OLS on fresh data.
		Xt, yt := gen(2000)
		ols, _ := Ridge(X, y, nil, 0)
		var seR, seO float64
		for i := range yt {
			seR += sq(yt[i] - m.Predict(Xt[i]))
			seO += sq(yt[i] - ols.Predict(Xt[i]))
		}
		if !(seR < seO) {
			t.Errorf("ridge %v vs ols %v on fresh data", seR/2000, seO/2000)
		}
		if _, err := json.Marshal(cv); err != nil {
			t.Error(err)
		}
	}
	if _, _, err := RidgeCV(X, y, nil, nil, BlockFold(40, 5)); !errors.Is(err, ErrInvalidArgument) {
		t.Error("no lambdas", err)
	}
	if _, _, err := RidgeCV(X, y, nil, lambdas, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Error("no folds", err)
	}
	if _, _, err := RidgeCV(X, y[:5], nil, lambdas, BlockFold(40, 5)); !errors.Is(err, ErrDimension) {
		t.Error("dimension", err)
	}
}

func TestFolds(t *testing.T) {
	for _, mk := range []func(int, int) [][]int{KFold, BlockFold} {
		for _, n := range []int{2, 7, 100, 101} {
			for _, k := range []int{0, 2, 3, 10, 500} {
				folds := mk(n, k)
				seen := make([]int, n)
				for _, f := range folds {
					if len(f) == 0 {
						t.Errorf("empty fold n=%d k=%d", n, k)
					}
					for _, i := range f {
						seen[i]++
					}
				}
				for i, c := range seen {
					if c != 1 {
						t.Fatalf("index %d in %d folds (n=%d k=%d)", i, c, n, k)
					}
				}
				if len(folds) != min(max(k, 2), n) {
					t.Errorf("fold count n=%d k=%d: %d", n, k, len(folds))
				}
			}
		}
		if mk(1, 3) != nil {
			t.Error("n<2 should be nil")
		}
	}
	// Blocks are contiguous.
	for _, f := range BlockFold(50, 4) {
		for i := 1; i < len(f); i++ {
			if f[i] != f[i-1]+1 {
				t.Fatal("block not contiguous")
			}
		}
	}
	ls := LogSpace(0.01, 100, 5)
	for i, w := range []float64{0.01, 0.1, 1, 10, 100} {
		approx(t, "logspace", ls[i], w, 1e-12*w)
	}
	if len(LogSpace(1, 2, 1)) != 1 {
		t.Error("logspace n<2")
	}

	// Generic cross-validation: predicting the training mean of y = i has a
	// known leave-one-block-out error.
	y := []float64{1, 2, 3, 4}
	score, err := CrossValidate(4, BlockFold(4, 2), func(train []int) (func(int) float64, error) {
		s := 0.0
		for _, i := range train {
			s += y[i]
		}
		mean := s / float64(len(train))
		return func(int) float64 { return mean }, nil
	}, func(i int, pred float64) float64 { return sq(y[i] - pred) })
	// Fold {0,1}: mean 3.5 -> 6.25+2.25; fold {2,3}: mean 1.5 -> 2.25+6.25.
	if err != nil || math.Abs(score-17.0/4) > 1e-12 {
		t.Errorf("CrossValidate = %v %v", score, err)
	}
	if _, err := CrossValidate(4, BlockFold(4, 2), func([]int) (func(int) float64, error) {
		return nil, ErrSingular
	}, func(int, float64) float64 { return 0 }); !errors.Is(err, ErrInsufficientData) {
		t.Error("all folds failing", err)
	}
	if _, err := CrossValidate(4, nil, nil, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Error("invalid", err)
	}
}

// --------------------------------------------------------------- logistic

func TestLogisticSaturated(t *testing.T) {
	// One binary feature: the maximum likelihood fit reproduces the observed
	// event rate of each group exactly (20 % and 70 %).
	var X [][]float64
	var y []float64
	add := func(x float64, events, total int) {
		for i := 0; i < total; i++ {
			X = append(X, []float64{x})
			if i < events {
				y = append(y, 1)
			} else {
				y = append(y, 0)
			}
		}
	}
	add(0, 20, 100)
	add(1, 105, 150)
	m, err := Logistic(X, y, []string{"flag"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Converged || m.Kind != KindLogistic || m.N != 250 {
		t.Errorf("%+v", m)
	}
	approx(t, "p(x=0)", m.Predict([]float64{0}), 0.2, 1e-9)
	approx(t, "p(x=1)", m.Predict([]float64{1}), 0.7, 1e-9)
	coef, b0 := m.RawCoef()
	approx(t, "intercept", b0, Logit(0.2), 1e-8)
	approx(t, "coef", coef[0], Logit(0.7)-Logit(0.2), 1e-8)
	// Log-loss of the saturated model in closed form.
	h := func(p float64) float64 { return -(p*math.Log(p) + (1-p)*math.Log(1-p)) }
	approx(t, "logloss", m.LogLoss, (100*h(0.2)+150*h(0.7))/250, 1e-10)
	approx(t, "pseudo r2", m.R2, 1-m.LogLoss/h(0.5), 1e-10)
	// Standard error of the log odds of group 0: sqrt(1/20 + 1/80).
	p, lo, hi := m.PredictInterval([]float64{0}, 0.95)
	se := math.Sqrt(1.0/20 + 1.0/80)
	approx(t, "ci p", p, 0.2, 1e-9)
	approx(t, "ci lo", lo, Sigmoid(Logit(0.2)-1.959963985*se), 1e-6)
	approx(t, "ci hi", hi, Sigmoid(Logit(0.2)+1.959963985*se), 1e-6)

	// Fractional outcomes with weights give the same fit as the raw events.
	mf, err := Logistic([][]float64{{0}, {1}}, []float64{0.2, 0.7}, nil, &LogisticOptions{Weights: []float64{100, 150}})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "fractional p0", mf.Predict([]float64{0}), 0.2, 1e-9)
	approx(t, "fractional p1", mf.Predict([]float64{1}), 0.7, 1e-9)

	// JSON round trip keeps predictions.
	data, _ := json.Marshal(m)
	m2, err := ParseModel(data)
	if err != nil || m2.Predict([]float64{1}) != m.Predict([]float64{1}) {
		t.Error("logistic JSON round trip", err)
	}
}

func TestLogisticRecovery(t *testing.T) {
	r := newRand(20)
	trueB0, trueB := -0.5, []float64{1.5, -2, 0}
	gen := func(n int) (X [][]float64, y []float64) {
		for i := 0; i < n; i++ {
			row := []float64{r.NormFloat64(), 3 + 2*r.NormFloat64(), r.NormFloat64() * 10}
			eta := trueB0 + trueB[0]*row[0] + trueB[1]*(row[1]-3)/2
			v := 0.0
			if r.Float64() < Sigmoid(eta) {
				v = 1
			}
			X = append(X, row)
			y = append(y, v)
		}
		return
	}
	X, y := gen(6000)
	X[0][1], y[1] = nan, nan
	m, err := Logistic(X, y, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Converged || m.N != 5998 || m.Iter > 15 {
		t.Errorf("converged=%v n=%d iter=%d", m.Converged, m.N, m.Iter)
	}
	// Features are standardised internally, so compare on that scale.
	approx(t, "b1", m.Coef[0], 1.5, 0.15)
	approx(t, "b2", m.Coef[1], -2, 0.15)
	approx(t, "b3", m.Coef[2], 0, 0.1)
	approx(t, "b0", m.Linear([]float64{0, 3, 0}), -0.5, 0.12)
	// Calibration and accuracy on fresh data.
	Xt, yt := gen(4000)
	correct, sumP, sumY := 0, 0.0, 0.0
	for i := range yt {
		p := m.Predict(Xt[i])
		if (p > 0.5) == (yt[i] == 1) {
			correct++
		}
		sumP += p
		sumY += yt[i]
	}
	if acc := float64(correct) / 4000; !(acc > 0.78) {
		t.Errorf("accuracy %v", acc)
	}
	approx(t, "calibration", sumP/4000, sumY/4000, 0.02)

	// The penalty shrinks the coefficients.
	mp, _ := Logistic(X, y, nil, &LogisticOptions{Lambda: 0.5})
	if !(math.Abs(mp.Coef[0]) < math.Abs(m.Coef[0])) || !(math.Abs(mp.Coef[1]) < math.Abs(m.Coef[1])) || !mp.Converged {
		t.Errorf("penalised %v vs %v", mp.Coef, m.Coef)
	}
}

func TestLogisticSeparable(t *testing.T) {
	var X [][]float64
	var y []float64
	for i := 0; i < 40; i++ {
		v := float64(i) - 19.5
		X = append(X, []float64{v, 1})
		if v > 0 {
			y = append(y, 1)
		} else {
			y = append(y, 0)
		}
	}
	// With a penalty the fit is finite, converges and classifies perfectly.
	m, err := Logistic(X, y, nil, &LogisticOptions{Lambda: 1e-3})
	if err != nil || !m.Converged {
		t.Fatalf("%+v %v", m, err)
	}
	for i := range y {
		if (m.Predict(X[i]) > 0.5) != (y[i] == 1) {
			t.Fatalf("misclassified row %d", i)
		}
	}
	if m.Coef[1] != 0 {
		t.Error("constant feature must have zero coefficient")
	}
	// Without a penalty the MLE does not exist: no crash, finite output.
	m, err = Logistic(X, y, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !IsFinite(m.Coef[0]) || !IsFinite(m.Intercept) || !(m.LogLoss < 1e-3) {
		t.Errorf("separable unpenalised: %+v", m)
	}
	if _, err := json.Marshal(m); err != nil {
		t.Error(err)
	}

	// Errors.
	if _, err := Logistic(X, make([]float64, 40), nil, nil); !errors.Is(err, ErrDegenerate) {
		t.Error("single class", err)
	}
	bad := append([]float64(nil), y...)
	bad[3] = 2
	if _, err := Logistic(X, bad, nil, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Error("y out of range", err)
	}
	if _, err := Logistic(X, y, nil, &LogisticOptions{Lambda: -1}); !errors.Is(err, ErrInvalidArgument) {
		t.Error("negative lambda", err)
	}
	if _, err := Logistic(X, y[:3], nil, nil); !errors.Is(err, ErrDimension) {
		t.Error("dimension", err)
	}
	if _, err := Logistic(nil, nil, nil, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("empty", err)
	}
	// Collinear features without a penalty.
	var Xc [][]float64
	var yc []float64
	r := newRand(21)
	for i := 0; i < 200; i++ {
		v := r.NormFloat64()
		Xc = append(Xc, []float64{v, -3 * v})
		if r.Float64() < Sigmoid(v) {
			yc = append(yc, 1)
		} else {
			yc = append(yc, 0)
		}
	}
	if _, err := Logistic(Xc, yc, nil, nil); !errors.Is(err, ErrSingular) {
		t.Error("collinear", err)
	}
	if _, err := Logistic(Xc, yc, nil, &LogisticOptions{Lambda: 0.01}); err != nil {
		t.Error("collinear with penalty", err)
	}
}

// ----------------------------------------------------------- optimisation

func rosenbrock(x []float64) float64 {
	return 100*sq(x[1]-x[0]*x[0]) + sq(1-x[0])
}

func himmelblau(x []float64) float64 {
	return sq(x[0]*x[0]+x[1]-11) + sq(x[0]+x[1]*x[1]-7)
}

func TestNelderMead(t *testing.T) {
	res, err := NelderMead(rosenbrock, []float64{-1.2, 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged || !(res.F < 1e-12) {
		t.Errorf("rosenbrock: %+v", res)
	}
	approx(t, "rosenbrock x", res.X[0], 1, 1e-5)
	approx(t, "rosenbrock y", res.X[1], 1, 1e-5)
	if res.Evals < res.Iter || res.Iter == 0 {
		t.Errorf("counters: %+v", res)
	}

	minima := [][2]float64{{3, 2}, {-2.805118, 3.131312}, {-3.779310, -3.283186}, {3.584428, -1.848126}}
	for _, start := range [][]float64{{0, 0}, {-1, 4}, {-4, -4}, {4, -3}, {5, 5}} {
		res, err = NelderMead(himmelblau, start, nil)
		if err != nil || !res.Converged || !(res.F < 1e-10) {
			t.Errorf("himmelblau from %v: %+v %v", start, res, err)
		}
		found := false
		for _, m := range minima {
			if math.Abs(res.X[0]-m[0]) < 1e-4 && math.Abs(res.X[1]-m[1]) < 1e-4 {
				found = true
			}
		}
		if !found {
			t.Errorf("himmelblau from %v ended at %v", start, res.X)
		}
	}

	// Higher dimensional quadratic with a known minimum.
	target := []float64{1, -2, 3, -4, 5}
	quad := func(x []float64) float64 {
		s := 0.0
		for i, v := range x {
			s += float64(i+1) * sq(v-target[i])
		}
		return s
	}
	res, _ = NelderMead(quad, make([]float64, 5), &NMOptions{MaxIter: 5000, Restarts: 3})
	for i, w := range target {
		approx(t, "quad", res.X[i], w, 1e-4)
	}

	// Bounds: the unconstrained minimum (2, -1) lies outside the box.
	box := func(x []float64) float64 { return sq(x[0]-2) + sq(x[1]+1) }
	res, err = NelderMead(box, []float64{0, 2}, &NMOptions{Lower: []float64{-5, 0}, Upper: []float64{1, 5}})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "bounded x", res.X[0], 1, 1e-6)
	approx(t, "bounded y", res.X[1], 0, 1e-6)
	approx(t, "bounded f", res.F, 2, 1e-6)
	// A start outside the box is projected into it.
	res, _ = NelderMead(box, []float64{50, -50}, &NMOptions{Lower: []float64{-5, 0}, Upper: []float64{1, 5}})
	approx(t, "projected f", res.F, 2, 1e-6)

	// Iteration limit is honoured and reported.
	res, _ = NelderMead(rosenbrock, []float64{-1.2, 1}, &NMOptions{MaxIter: 5})
	if res.Converged || res.Iter != 5 || !(res.F <= rosenbrock([]float64{-1.2, 1})) {
		t.Errorf("limited: %+v", res)
	}
	// NaN objective values are survivable.
	res, err = NelderMead(func(x []float64) float64 {
		if x[0] < 0 {
			return nan
		}
		return sq(x[0] - 3)
	}, []float64{1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "nan region", res.X[0], 3, 1e-5)

	if _, err := NelderMead(nil, []float64{1}, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Error("nil f", err)
	}
	if _, err := NelderMead(quad, nil, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Error("empty x0", err)
	}
	if _, err := NelderMead(box, []float64{0, 0}, &NMOptions{Lower: []float64{0}}); !errors.Is(err, ErrInvalidArgument) {
		t.Error("bounds length", err)
	}
	if _, err := NelderMead(box, []float64{0, 0}, &NMOptions{Lower: []float64{2, 2}, Upper: []float64{1, 1}}); !errors.Is(err, ErrInvalidArgument) {
		t.Error("inverted bounds", err)
	}
	if _, err := NelderMead(box, []float64{nan, 0}, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Error("nan start", err)
	}
}

func TestGoldenSection(t *testing.T) {
	res, err := GoldenSection(func(x float64) float64 { return sq(x-1.5) + 2 }, -10, 10, 1e-10, 0)
	if err != nil || !res.Converged {
		t.Fatalf("%+v %v", res, err)
	}
	approx(t, "x", res.X[0], 1.5, 1e-7)
	approx(t, "f", res.F, 2, 1e-12)
	// cos has its minimum at pi on [3, 4]; bounds given in reverse order.
	res, _ = GoldenSection(math.Cos, 4, 3, 0, 0)
	approx(t, "pi", res.X[0], math.Pi, 1e-6)
	// Minimum at the boundary.
	res, _ = GoldenSection(func(x float64) float64 { return x }, 2, 5, 1e-9, 0)
	approx(t, "boundary", res.X[0], 2, 1e-6)
	res, _ = GoldenSection(math.Cos, 3, 4, 1e-12, 3)
	if res.Converged || res.Iter != 3 {
		t.Errorf("limited: %+v", res)
	}
	if _, err := GoldenSection(nil, 0, 1, 0, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Error("nil", err)
	}
	if _, err := GoldenSection(math.Cos, nan, 1, 0, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Error("nan bound", err)
	}
}

func TestGridSearch(t *testing.T) {
	f := func(x []float64) float64 { return sq(x[0]-0.3) + 2*sq(x[1]+1.7) + 5 }
	res, err := GridSearch(f, []float64{-4, -4}, []float64{4, 4}, 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Single pass: nearest grid node (spacing 1).
	if res.X[0] != 0 || res.X[1] != -2 || res.Evals != 81 || !res.Converged {
		t.Errorf("coarse: %+v", res)
	}
	res, _ = GridSearch(f, []float64{-4, -4}, []float64{4, 4}, 9, 12)
	approx(t, "x", res.X[0], 0.3, 1e-4)
	approx(t, "y", res.X[1], -1.7, 1e-4)
	approx(t, "f", res.F, 5, 1e-7)
	// A fixed dimension (lo == hi) is not searched.
	res, _ = GridSearch(f, []float64{-4, 1}, []float64{4, 1}, 5, 10)
	if res.X[1] != 1 || res.Evals != 55 {
		t.Errorf("fixed dim: %+v", res)
	}
	approx(t, "fixed x", res.X[0], 0.3, 2e-3)
	// Multi-modal objective: the grid finds the global basin where a local
	// search from a bad start would not.
	g := func(x []float64) float64 { return math.Sin(3*x[0]) + 0.1*sq(x[0]-4) }
	res, _ = GridSearch(g, []float64{-10}, []float64{10}, 41, 8)
	approx(t, "global", res.X[0], 3.6725, 0.005)
	// All-NaN objective.
	res, err = GridSearch(func([]float64) float64 { return nan }, []float64{0}, []float64{1}, 3, 1)
	if err != nil || res.Converged {
		t.Errorf("nan objective: %+v %v", res, err)
	}
	if _, err := GridSearch(f, []float64{1, 1}, []float64{0, 2}, 3, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Error("inverted", err)
	}
	if _, err := GridSearch(f, nil, nil, 3, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Error("empty", err)
	}
	if _, err := GridSearch(f, make([]float64, 10), []float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, 10, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Error("too large", err)
	}
}
