package stat

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"testing"
)

var nan = math.NaN()

// approx fails the test unless got is within tol of want (absolute).
func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.12g, want %.12g (tol %g)", name, got, want, tol)
	}
}

func wantNaN(t *testing.T, name string, got float64) {
	t.Helper()
	if !math.IsNaN(got) {
		t.Errorf("%s = %v, want NaN", name, got)
	}
}

func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15)) }

// Anscombe's quartet.
var (
	anscombeX  = []float64{10, 8, 13, 9, 11, 14, 6, 4, 12, 7, 5}
	anscombeY1 = []float64{8.04, 6.95, 7.58, 8.81, 8.33, 9.96, 7.24, 4.26, 10.84, 4.82, 5.68}
	anscombeY2 = []float64{9.14, 8.14, 8.74, 8.77, 9.26, 8.10, 6.13, 3.10, 9.13, 7.26, 4.74}
	anscombeY3 = []float64{7.46, 6.77, 12.74, 7.11, 7.81, 8.84, 6.08, 5.39, 8.15, 6.42, 5.73}
	anscombeX4 = []float64{8, 8, 8, 8, 8, 8, 8, 19, 8, 8, 8}
	anscombeY4 = []float64{6.58, 5.76, 7.71, 8.84, 8.47, 7.04, 5.25, 12.50, 5.56, 7.91, 6.89}
)

// ---------------------------------------------------------------- special

func TestRegIncBeta(t *testing.T) {
	for _, x := range []float64{0.01, 0.2, 0.5, 0.77, 0.99} {
		approx(t, "I_x(1,1)", RegIncBeta(1, 1, x), x, 1e-14)
		approx(t, "I_x(3.5,1)", RegIncBeta(3.5, 1, x), math.Pow(x, 3.5), 1e-13)
		approx(t, "I_x(1,4.2)", RegIncBeta(1, 4.2, x), 1-math.Pow(1-x, 4.2), 1e-13)
		approx(t, "I_x(.5,.5)", RegIncBeta(0.5, 0.5, x), 2/math.Pi*math.Asin(math.Sqrt(x)), 1e-12)
		approx(t, "symmetry", RegIncBeta(2.3, 7.1, x), 1-RegIncBeta(7.1, 2.3, 1-x), 1e-13)
		// Beta(2,3) CDF in closed form.
		approx(t, "I_x(2,3)", RegIncBeta(2, 3, x), x*x*(6-8*x+3*x*x), 1e-13)
	}
	approx(t, "I_.4(2,3)", RegIncBeta(2, 3, 0.4), 0.5248, 1e-13)
	for _, a := range []float64{0.3, 1, 5, 80} {
		approx(t, "I_.5(a,a)", RegIncBeta(a, a, 0.5), 0.5, 1e-12)
	}
	// Large parameters: limited by the cancellation in the log-gamma terms.
	approx(t, "I_.5(5000,5000)", RegIncBeta(5000, 5000, 0.5), 0.5, 1e-10)
	approx(t, "I_.5(3e5,3e5)", RegIncBeta(3e5, 3e5, 0.5), 0.5, 1e-8)
	if RegIncBeta(2, 3, 0) != 0 || RegIncBeta(2, 3, 1) != 1 {
		t.Error("endpoints")
	}
	wantNaN(t, "a<=0", RegIncBeta(0, 1, 0.5))
	wantNaN(t, "x>1", RegIncBeta(1, 1, 1.5))
	wantNaN(t, "nan", RegIncBeta(1, 1, nan))
}

func TestStudentT(t *testing.T) {
	for _, x := range []float64{-7, -1, -0.2, 0, 0.5, 1, 3, 40} {
		// df=1 is the Cauchy distribution, df=2 has a closed form too.
		approx(t, "cdf df1", StudentTCDF(x, 1), 0.5+math.Atan(x)/math.Pi, 1e-13)
		approx(t, "cdf df2", StudentTCDF(x, 2), 0.5+x/(2*math.Sqrt(2+x*x)), 1e-13)
		approx(t, "two-sided", StudentTTwoSidedP(x, 7), 2*StudentTCDF(-math.Abs(x), 7), 1e-13)
	}
	// Table values.
	approx(t, "p(t=2,df=10)", StudentTTwoSidedP(2, 10), 0.07338803, 1e-8)
	approx(t, "p(t=2.228139,df=10)", StudentTTwoSidedP(2.228138852, 10), 0.05, 1e-9)
	approx(t, "p(t=12.7062,df=1)", StudentTTwoSidedP(12.70620474, 1), 0.05, 1e-9)
	approx(t, "p(t=1.96,df=1e6)", StudentTTwoSidedP(1.959963985, 1e6), 0.05, 2e-6)
	approx(t, "q(.975,10)", StudentTQuantile(0.975, 10), 2.228138852, 1e-8)
	approx(t, "q(.975,1)", StudentTQuantile(0.975, 1), 12.70620474, 1e-7)
	approx(t, "q(.95,5)", StudentTQuantile(0.95, 5), 2.015048373, 1e-8)
	approx(t, "q(.995,30)", StudentTQuantile(0.995, 30), 2.749995654, 1e-8)
	approx(t, "q(.025,10)", StudentTQuantile(0.025, 10), -2.228138852, 1e-8)
	if StudentTQuantile(0.5, 3) != 0 || !math.IsInf(StudentTQuantile(1, 3), 1) {
		t.Error("quantile edge cases")
	}
	for _, p := range []float64{0.6, 0.9, 0.999, 0.999999} {
		for _, df := range []float64{1, 2.5, 9, 200} {
			approx(t, "roundtrip", StudentTCDF(StudentTQuantile(p, df), df), p, 1e-11)
		}
	}
	wantNaN(t, "df<=0", StudentTCDF(1, 0))
	wantNaN(t, "q p>1", StudentTQuantile(1.1, 3))
}

func TestNormalAndF(t *testing.T) {
	approx(t, "q(.975)", NormalQuantile(0.975), 1.959963985, 1e-9)
	approx(t, "q(.5)", NormalQuantile(0.5), 0, 1e-15)
	approx(t, "q(.05)", NormalQuantile(0.05), -1.644853627, 1e-9)
	approx(t, "cdf(1.96)", NormalCDF(1.96), 0.9750021049, 1e-9)
	for _, p := range []float64{0.001, 0.3, 0.9, 0.9999} {
		approx(t, "roundtrip", NormalCDF(NormalQuantile(p)), p, 1e-12)
	}
	wantNaN(t, "q(-1)", NormalQuantile(-1))
	// F(1, d) is t^2; F(2, d) has the closed form (1+2f/d)^(-d/2).
	approx(t, "F(1,9)", FDistSF(2.3*2.3, 1, 9), StudentTTwoSidedP(2.3, 9), 1e-13)
	approx(t, "F(2,10)", FDistSF(4, 2, 10), math.Pow(1.8, -5), 1e-13)
	if FDistSF(-1, 2, 3) != 1 || FDistSF(math.Inf(1), 2, 3) != 0 {
		t.Error("F edges")
	}
	approx(t, "sigmoid", Sigmoid(Logit(0.123)), 0.123, 1e-15)
	if Sigmoid(-1000) != 0 || Sigmoid(1000) != 1 {
		t.Error("sigmoid saturation")
	}
}

// ------------------------------------------------------------ descriptive

func TestDescriptive(t *testing.T) {
	x := []float64{2, nan, 4, 4, 4, 5, 5, nan, 7, 9}
	if Count(x) != 8 {
		t.Fatal("count")
	}
	approx(t, "sum", Sum(x), 40, 0)
	approx(t, "mean", Mean(x), 5, 1e-15)
	approx(t, "popvar", PopVariance(x), 4, 1e-14)
	approx(t, "popstd", PopStd(x), 2, 1e-14)
	approx(t, "var", Variance(x), 32.0/7, 1e-14)
	approx(t, "std", Std(x), math.Sqrt(32.0/7), 1e-14)
	lo, hi, ok := MinMax(x)
	if !ok || lo != 2 || hi != 9 || Min(x) != 2 || Max(x) != 9 {
		t.Error("minmax")
	}
	approx(t, "median", Median(x), 4.5, 1e-15)

	// Empty / all-NaN / single value.
	for _, e := range [][]float64{nil, {}, {nan, nan}} {
		wantNaN(t, "mean", Mean(e))
		wantNaN(t, "var", Variance(e))
		wantNaN(t, "popvar", PopVariance(e))
		wantNaN(t, "median", Median(e))
		wantNaN(t, "min", Min(e))
		wantNaN(t, "mad", MAD(e))
		wantNaN(t, "trim", TrimmedMean(e, 0.1))
		if _, _, ok := MinMax(e); ok {
			t.Error("minmax ok on empty")
		}
		if Sum(e) != 0 || Describe(e).N != 0 {
			t.Error("empty sum/describe")
		}
	}
	wantNaN(t, "var n=1", Variance([]float64{3}))
	approx(t, "popvar n=1", PopVariance([]float64{3}), 0, 0)

	// Anscombe: all four y have mean 7.50 and variance 4.12..4.13; x var 11.
	approx(t, "anscombe mean x", Mean(anscombeX), 9, 1e-14)
	approx(t, "anscombe var x", Variance(anscombeX), 11, 1e-13)
	for i, y := range [][]float64{anscombeY1, anscombeY2, anscombeY3, anscombeY4} {
		approx(t, "anscombe mean y", Mean(y), 7.50, 0.002)
		approx(t, "anscombe var y", Variance(y), 4.125, 0.004)
		_ = i
	}
}

func TestQuantiles(t *testing.T) {
	x := []float64{4, 1, nan, 3, 2}
	approx(t, "q0", Quantile(x, 0), 1, 0)
	approx(t, "q1", Quantile(x, 1), 4, 0)
	approx(t, "q.25", Quantile(x, 0.25), 1.75, 1e-15) // type 7
	approx(t, "q.5", Quantile(x, 0.5), 2.5, 1e-15)
	approx(t, "q.9", Quantile(x, 0.9), 3.7, 1e-14)
	wantNaN(t, "p>1", Quantile(x, 1.2))
	wantNaN(t, "p nan", Quantile(x, nan))
	qs := Quantiles(x, 0.25, 0.5, 0.75)
	if len(qs) != 3 || qs[0] != 1.75 || qs[1] != 2.5 || qs[2] != 3.25 {
		t.Errorf("Quantiles = %v", qs)
	}
	if x[0] != 4 || !math.IsNaN(x[2]) {
		t.Error("Quantile modified its input")
	}
	approx(t, "single", Quantile([]float64{7}, 0.3), 7, 0)

	// MAD of {1,1,2,2,4,6,9}: median 2, deviations {1,1,0,0,2,4,7} -> 1.
	m := []float64{1, 1, 2, 2, 4, 6, 9}
	approx(t, "mad", MAD(m), 1, 0)
	approx(t, "robuststd", RobustStd(m), MADScale, 1e-15)

	d := Describe(anscombeX)
	if d.N != 11 || d.Min != 4 || d.Max != 14 || d.Median != 9 || d.Q1 != 6.5 || d.Q3 != 11.5 {
		t.Errorf("Describe = %+v", d)
	}
	approx(t, "describe std", d.Std, math.Sqrt(11), 1e-13)
	if _, err := json.Marshal(Describe(nil)); err != nil {
		t.Error(err)
	}
}

func TestWeightedAndTrimmed(t *testing.T) {
	x := []float64{1, 2, nan, 4, 100}
	w := []float64{1, 2, 5, 1, 0}
	approx(t, "wmean", WeightedMean(x, w), (1+4+4)/4.0, 1e-15)
	wantNaN(t, "wmean len", WeightedMean(x, w[:2]))
	wantNaN(t, "wmean zero", WeightedMean([]float64{1, 2}, []float64{0, 0}))
	wantNaN(t, "wmean negative", WeightedMean([]float64{1}, []float64{-1}))

	// Equal weights reproduce the ordinary median; a dominant weight wins.
	approx(t, "wmedian even", WeightedQuantile([]float64{4, 1, 3, 2}, []float64{1, 1, 1, 1}, 0.5), 2.5, 1e-15)
	approx(t, "wmedian odd", WeightedQuantile([]float64{3, 1, 2}, []float64{2, 2, 2}, 0.5), 2, 1e-15)
	approx(t, "wmedian heavy", WeightedQuantile([]float64{1, 2, 50}, []float64{1, 1, 100}, 0.5), 50, 1e-12)
	approx(t, "wq .25 of 4", WeightedQuantile([]float64{4, 1, 3, 2}, []float64{1, 1, 1, 1}, 0.25), 1.5, 1e-15)
	approx(t, "wq .3 of 4", WeightedQuantile([]float64{4, 1, 3, 2}, []float64{1, 1, 1, 1}, 0.3), 2, 0)
	approx(t, "wq p=0", WeightedQuantile([]float64{3, 1, 2}, []float64{1, 1, 1}, 0), 1, 0)
	approx(t, "wq p=1", WeightedQuantile([]float64{3, 1, 2}, []float64{1, 1, 1}, 1), 3, 0)
	wantNaN(t, "wq empty", WeightedQuantile(nil, nil, 0.5))

	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 1000}
	approx(t, "trim 0", TrimmedMean(v, 0), Mean(v), 1e-12)
	approx(t, "trim .1", TrimmedMean(v, 0.1), 5.5, 1e-14)
	approx(t, "trim .5", TrimmedMean(v, 0.5), 5.5, 1e-14)
	approx(t, "trim >.5", TrimmedMean(v, 3), 5.5, 1e-14)
}

func TestZScores(t *testing.T) {
	x := []float64{2, 4, nan, 6}
	z := ZScores(x)
	approx(t, "z0", z[0], -1, 1e-15)
	approx(t, "z1", z[1], 0, 1e-15)
	approx(t, "z3", z[3], 1, 1e-15)
	wantNaN(t, "z2", z[2])
	for _, v := range ZScores([]float64{5, nan}) {
		wantNaN(t, "z n<2", v)
	}
	c := ZScores([]float64{3, 3, 3})
	if c[0] != 0 || c[2] != 0 {
		t.Error("constant z should be 0")
	}

	r := RobustZScores([]float64{1, 1, 2, 2, 4, 6, 9, nan})
	approx(t, "rz", r[6], (9-2)/MADScale, 1e-12)
	wantNaN(t, "rz nan", r[7])
	// MAD = 0 falls back to the mean absolute deviation instead of Inf.
	r = RobustZScores([]float64{5, 5, 5, 5, 5, 5, 5, 12})
	if r[0] != 0 || !(r[7] > 3) || math.IsInf(r[7], 0) {
		t.Errorf("fallback robust z = %v", r)
	}
	r = RobustZScores([]float64{5, 5, 5})
	if r[0] != 0 || r[2] != 0 {
		t.Error("constant robust z should be 0")
	}
	if len(RobustZScores(nil)) != 0 {
		t.Error("nil")
	}
}

func TestHistQuantile(t *testing.T) {
	// 4 bins of width 5 starting at 10: [10,15) [15,20) [20,25) [25,30).
	c := []uint32{10, 0, 30, 60}
	approx(t, "p0", HistQuantile(c, 10, 5, 0), 10, 0)
	approx(t, "p1", HistQuantile(c, 10, 5, 1), 30, 1e-12)
	approx(t, "p.05", HistQuantile(c, 10, 5, 0.05), 12.5, 1e-12) // half of the first bin
	approx(t, "p.1", HistQuantile(c, 10, 5, 0.1), 15, 1e-12)
	approx(t, "p.25", HistQuantile(c, 10, 5, 0.25), 22.5, 1e-12)
	approx(t, "p.7", HistQuantile(c, 10, 5, 0.7), 27.5, 1e-12)
	approx(t, "edges", HistQuantileEdges(c, []float64{10, 15, 20, 25, 30}, 0.7), 27.5, 1e-12)
	approx(t, "edges uneven", HistQuantileEdges([]int{1, 1}, []float64{0, 1, 11}, 0.75), 6, 1e-12)
	approx(t, "histmean", HistMean(c, 10, 5), (10*12.5+30*22.5+60*27.5)/100, 1e-12)
	wantNaN(t, "empty", HistQuantile([]uint32{0, 0}, 0, 1, 0.5))
	wantNaN(t, "nil", HistQuantile([]float64(nil), 0, 1, 0.5))
	wantNaN(t, "bad p", HistQuantile(c, 10, 5, 1.5))
	wantNaN(t, "bad edges", HistQuantileEdges(c, []float64{1, 2}, 0.5))
	wantNaN(t, "histmean empty", HistMean([]int{0}, 0, 1))
	approx(t, "negative counts ignored", HistQuantile([]float64{-5, 2, nan}, 0, 1, 0.5), 1.5, 1e-12)

	// Binned quantiles of a large uniform sample agree with exact ones to
	// within a fraction of a bin.
	r := newRand(1)
	x := make([]float64, 20000)
	for i := range x {
		x[i] = 600 * r.Float64() * r.Float64()
	}
	counts, under, over := Histogram(x, 0, 5, 120)
	if under != 0 || over != 0 || Sum(counts) != 20000 {
		t.Fatalf("histogram totals: %d %d %v", under, over, Sum(counts))
	}
	for _, p := range []float64{0.5, 0.9, 0.95, 0.99} {
		approx(t, "hist vs exact", HistQuantile(counts, 0, 5, p), Quantile(x, p), 1.0)
	}
	if c, _, _ := Histogram(x, 0, 0, 5); c != nil {
		t.Error("invalid histogram should be nil")
	}
	_, under, over = Histogram([]float64{-1, 0, 4.99, 5, nan}, 0, 1, 5)
	if under != 1 || over != 1 {
		t.Errorf("under/over = %d/%d", under, over)
	}
}

// ---------------------------------------------------------------- rolling

func TestRolling(t *testing.T) {
	r := newRand(2)
	x := make([]float64, 200)
	for i := range x {
		x[i] = r.NormFloat64() * 10
		if r.Float64() < 0.15 {
			x[i] = nan
		}
	}
	x[0], x[1], x[2] = nan, nan, nan
	const w = 7
	mean, sum := RollingMean(x, w), RollingSum(x, w)
	sd, med := RollingStd(x, w), RollingMedian(x, w)
	mn, mx := RollingMin(x, w), RollingMax(x, w)
	for i := range x {
		win := x[max(0, i-w+1) : i+1]
		if Count(win) == 0 {
			wantNaN(t, "mean", mean[i])
			wantNaN(t, "sum", sum[i])
			continue
		}
		approx(t, "rolling mean", mean[i], Mean(win), 1e-10)
		approx(t, "rolling sum", sum[i], Sum(win), 1e-10)
		approx(t, "rolling median", med[i], Median(win), 1e-12)
		approx(t, "rolling min", mn[i], Min(win), 0)
		approx(t, "rolling max", mx[i], Max(win), 0)
		if Count(win) >= 2 {
			approx(t, "rolling std", sd[i], Std(win), 1e-10)
		} else {
			wantNaN(t, "std n<2", sd[i])
		}
	}
	for _, v := range RollingMean(x, 0) {
		wantNaN(t, "window 0", v)
	}
	c := RollingMeanCentered([]float64{1, 2, nan, 4, 5}, 3)
	approx(t, "centered 0", c[0], 1.5, 1e-15)
	approx(t, "centered 2", c[2], 3, 1e-15)
	approx(t, "centered 4", c[4], 4.5, 1e-15)

	d := Diff([]float64{1, 4, 9, nan, 25}, 1)
	wantNaN(t, "diff0", d[0])
	approx(t, "diff1", d[1], 3, 0)
	wantNaN(t, "diff3", d[3])
	wantNaN(t, "diff4", d[4])
	l := Lag([]float64{1, 2, 3}, 1)
	if !math.IsNaN(l[0]) || l[1] != 1 || l[2] != 2 {
		t.Errorf("Lag = %v", l)
	}
	l = Lag([]float64{1, 2, 3}, -2)
	if l[0] != 3 || !math.IsNaN(l[1]) {
		t.Errorf("Lead = %v", l)
	}
	f := FillLinear([]float64{nan, 1, nan, nan, 4, nan, nan, nan, 8, nan}, 2)
	want := []float64{nan, 1, 2, 3, 4, nan, nan, nan, 8, nan}
	for i := range f {
		if math.IsNaN(want[i]) != math.IsNaN(f[i]) || (!math.IsNaN(want[i]) && math.Abs(f[i]-want[i]) > 1e-12) {
			t.Errorf("FillLinear = %v", f)
			break
		}
	}
	approx(t, "fill any gap", FillLinear([]float64{0, nan, nan, nan, 8}, 0)[2], 4, 1e-12)
}

func TestWindow(t *testing.T) {
	w := NewWindow(4)
	wantNaN(t, "empty mean", w.Mean())
	wantNaN(t, "empty min", w.Min())
	in := []float64{1, 2, nan, 4, 5, 6, nan, 8, 9, 10, 11}
	for i, v := range in {
		w.Push(v)
		win := in[max(0, i-3) : i+1]
		if w.Len() != len(win) || w.Valid() != Count(win) {
			t.Fatalf("len/valid at %d", i)
		}
		approx(t, "window mean", w.Mean(), Mean(win), 1e-12)
		approx(t, "window min", w.Min(), Min(win), 0)
		approx(t, "window max", w.Max(), Max(win), 0)
		approx(t, "window median", w.Median(), Median(win), 1e-12)
		vals := w.Values(nil)
		for j := range win {
			if math.IsNaN(win[j]) != math.IsNaN(vals[j]) || (!math.IsNaN(win[j]) && win[j] != vals[j]) {
				t.Fatalf("Values at %d = %v, want %v", i, vals, win)
			}
		}
	}
	if !w.Full() {
		t.Error("should be full")
	}
	approx(t, "window std", w.Std(), Std(in[7:]), 1e-12)
	w.Reset()
	if w.Len() != 0 || w.Valid() != 0 {
		t.Error("reset")
	}
	if NewWindow(0).Full() {
		t.Error("size clamps to 1")
	}
}

func TestEWMA(t *testing.T) {
	approx(t, "alpha(1)", AlphaFromHalfLife(1), 0.5, 1e-15)
	if AlphaFromHalfLife(0) != 1 || AlphaFromHalfLife(nan) != 1 {
		t.Error("invalid half-life")
	}
	// After exactly halfLife samples of a unit step, half the gap is closed.
	e := NewEWMAHalfLife(12)
	wantNaN(t, "initial", e.Value())
	wantNaN(t, "initial std", e.Std())
	e.Add(0)
	for i := 0; i < 12; i++ {
		e.Add(nan) // ignored
		e.Add(1)
	}
	approx(t, "half-life", e.Value(), 0.5, 1e-12)
	if e.N != 13 {
		t.Errorf("N = %d", e.N)
	}
	// Irregular sampling: one step of dt=12 equals twelve steps of dt=1.
	e2 := NewEWMAHalfLife(12)
	e2.Add(0)
	e2.AddDT(1, 12)
	approx(t, "AddDT", e2.Value(), 0.5, 1e-12)
	// Closed form recursion.
	e3 := NewEWMA(0.25)
	s := EWMASeries([]float64{nan, 4, 8, nan, 0}, 0.25)
	wantNaN(t, "series0", s[0])
	approx(t, "series1", s[1], 4, 0)
	approx(t, "series2", s[2], 5, 1e-15)
	approx(t, "series3", s[3], 5, 1e-15)
	approx(t, "series4", s[4], 3.75, 1e-15)
	e3.Add(4)
	e3.Add(8)
	approx(t, "var", e3.Var, 0.75*(0+0.25*16), 1e-15)
	approx(t, "std", e3.Std(), math.Sqrt(3), 1e-15)
	if NewEWMA(-3).Alpha != 1 || NewEWMA(nan).Alpha != 1 {
		t.Error("invalid alpha")
	}
	b, err := json.Marshal(NewEWMA(0.1))
	if err != nil || len(b) == 0 {
		t.Error("EWMA json", err)
	}
}

func TestAccumulator(t *testing.T) {
	r := newRand(3)
	x := make([]float64, 1000)
	for i := range x {
		x[i] = 1013 + r.NormFloat64()*8
		if i%17 == 0 {
			x[i] = nan
		}
	}
	var all, a, b, empty Accumulator
	wantNaN(t, "empty mean", all.Mean())
	wantNaN(t, "empty var", all.Variance())
	wantNaN(t, "empty min", all.Min())
	all.AddAll(x)
	a.AddAll(x[:333])
	b.AddAll(x[333:])
	a.Merge(b)
	a.Merge(empty)
	empty.Merge(a)
	for name, acc := range map[string]Accumulator{"whole": all, "merged": a, "into-empty": empty} {
		if acc.N != int64(Count(x)) {
			t.Errorf("%s N = %d", name, acc.N)
		}
		approx(t, name+" mean", acc.Mean(), Mean(x), 1e-10)
		approx(t, name+" var", acc.Variance(), Variance(x), 1e-9)
		approx(t, name+" popvar", acc.PopVariance(), PopVariance(x), 1e-9)
		approx(t, name+" std", acc.Std(), Std(x), 1e-10)
		approx(t, name+" sum", acc.Sum(), Sum(x), 1e-7)
		approx(t, name+" min", acc.Min(), Min(x), 0)
		approx(t, name+" max", acc.Max(), Max(x), 0)
	}
	if all.Last != x[len(x)-1] {
		t.Error("last")
	}
	var one Accumulator
	one.Add(5)
	wantNaN(t, "var n=1", one.Variance())
	approx(t, "popvar n=1", one.PopVariance(), 0, 0)
	one.Reset()
	if one.N != 0 {
		t.Error("reset")
	}
	if _, err := json.Marshal(all); err != nil {
		t.Error(err)
	}
}

// --------------------------------------------------------------- circular

func TestCircular(t *testing.T) {
	approx(t, "norm -10", NormalizeDeg(-10), 350, 1e-12)
	approx(t, "norm 725", NormalizeDeg(725), 5, 1e-12)
	approx(t, "norm 360", NormalizeDeg(360), 0, 0)
	approx(t, "norm tiny negative", NormalizeDeg(-1e-20), 0, 0)
	wantNaN(t, "norm nan", NormalizeDeg(nan))
	wantNaN(t, "norm inf", NormalizeDeg(math.Inf(1)))

	approx(t, "diff 10-350", AngleDiffDeg(10, 350), 20, 1e-12)
	approx(t, "diff 350-10", AngleDiffDeg(350, 10), -20, 1e-12)
	approx(t, "diff 180", AngleDiffDeg(270, 90), 180, 1e-12)
	approx(t, "diff -180 -> 180", AngleDiffDeg(90, 270), 180, 1e-12)
	approx(t, "diff wraps", AngleDiffDeg(725, -355), 0, 1e-12)
	approx(t, "abs diff", AbsAngleDiffDeg(350, 10), 20, 1e-12)
	wantNaN(t, "diff nan", AngleDiffDeg(nan, 1))

	m, r, ok := CircularMeanDeg([]float64{350, 10, nan})
	if !ok {
		t.Fatal("mean not ok")
	}
	approx(t, "mean 350,10", math.Abs(AngleDiffDeg(m, 0)), 0, 1e-9)
	approx(t, "R 350,10", r, math.Cos(10*math.Pi/180), 1e-12)
	approx(t, "variance", CircularVariance([]float64{350, 10}), 1-math.Cos(10*math.Pi/180), 1e-12)
	approx(t, "resultant", ResultantLength([]float64{90, 90, 90}), 1, 1e-12)
	approx(t, "std identical", CircularStdDeg([]float64{90, 90, 90}), 0, 1e-5)
	// Tightly clustered angles: circular std ~ linear std (population).
	cl := []float64{178, 179, 180, 181, 182}
	approx(t, "std small spread", CircularStdDeg(cl), PopStd(cl), 1e-3)
	m, _, _ = CircularMeanDeg(cl)
	approx(t, "mean cluster", m, 180, 1e-9)

	// Opposite directions cancel: mean undefined.
	m, r, ok = CircularMeanDeg([]float64{0, 180})
	if ok || !math.IsNaN(m) || r > 1e-12 {
		t.Errorf("opposite: %v %v %v", m, r, ok)
	}
	st, valid := CircularSummary([]float64{0, 90, 180, 270}, nil)
	if !valid || st.Defined || st.N != 4 || st.Variance < 0.999999 {
		t.Errorf("uniform: %+v", st)
	}
	if _, err := json.Marshal(st); err != nil {
		t.Error(err)
	}
	if _, _, ok := CircularMeanDeg(nil); ok {
		t.Error("empty ok")
	}
	wantNaN(t, "empty R", ResultantLength([]float64{nan}))
	wantNaN(t, "empty var", CircularVariance(nil))
	wantNaN(t, "empty std", CircularStdDeg(nil))

	// Weighted: heavy weight pulls the mean.
	st, valid = CircularSummary([]float64{0, 90}, []float64{3, 1})
	if !valid {
		t.Fatal("weighted")
	}
	approx(t, "weighted mean", st.MeanDeg, math.Atan2(1, 3)*180/math.Pi, 1e-9)
	if _, ok := CircularSummary([]float64{1, 2}, []float64{1}); ok {
		t.Error("length mismatch should fail")
	}
	st, _ = CircularSummary([]float64{0, 90}, []float64{1, -1})
	if st.N != 1 || st.MeanDeg != 0 {
		t.Errorf("negative weight skipped: %+v", st)
	}
}

func TestWind(t *testing.T) {
	// Wind FROM the north blows towards the south: v negative.
	u, v := WindToUV(10, 0)
	approx(t, "north u", u, 0, 1e-12)
	approx(t, "north v", v, -10, 1e-12)
	// Wind FROM the west blows towards the east: u positive.
	u, v = WindToUV(10, 270)
	approx(t, "west u", u, 10, 1e-12)
	approx(t, "west v", v, 0, 1e-12)
	for _, d := range []float64{0, 33, 90, 179.5, 180, 271, 359.9} {
		s, dir := UVToWind(WindToUV(7.5, d))
		approx(t, "roundtrip speed", s, 7.5, 1e-12)
		approx(t, "roundtrip dir", AbsAngleDiffDeg(dir, d), 0, 1e-9)
	}
	if s, d := UVToWind(0, 0); s != 0 || d != 0 {
		t.Error("calm")
	}

	// Two equal winds 90 degrees apart: mean direction between, speed/sqrt2.
	w, ok := VectorMeanWind([]float64{10, 10, nan}, []float64{0, 90, 45})
	if !ok || w.N != 2 {
		t.Fatalf("%+v", w)
	}
	approx(t, "vec dir", w.DirDeg, 45, 1e-9)
	approx(t, "vec speed", w.Speed, 10/math.Sqrt2, 1e-12)
	approx(t, "scalar speed", w.ScalarSpeed, 10, 1e-12)
	approx(t, "steadiness", w.Steadiness, 1/math.Sqrt2, 1e-12)
	// Across north.
	w, _ = VectorMeanWind([]float64{5, 5}, []float64{350, 10})
	approx(t, "across north", AbsAngleDiffDeg(w.DirDeg, 0), 0, 1e-9)
	// Opposite winds cancel.
	w, ok = VectorMeanWind([]float64{8, 8}, []float64{90, 270})
	if !ok || w.Speed > 1e-12 || w.Steadiness > 1e-12 || w.ScalarSpeed != 8 {
		t.Errorf("opposite winds: %+v", w)
	}
	if _, ok := VectorMeanWind(nil, nil); ok {
		t.Error("empty")
	}
	if _, ok := VectorMeanWind([]float64{1}, []float64{1, 2}); ok {
		t.Error("mismatch")
	}
	if _, err := json.Marshal(w); err != nil {
		t.Error(err)
	}
}

// ------------------------------------------------------------ association

func TestPearson(t *testing.T) {
	// All four Anscombe sets share r = 0.816.
	xs := [][]float64{anscombeX, anscombeX, anscombeX, anscombeX4}
	ys := [][]float64{anscombeY1, anscombeY2, anscombeY3, anscombeY4}
	for i := range xs {
		c, err := Pearson(xs[i], ys[i])
		if err != nil {
			t.Fatal(err)
		}
		approx(t, "anscombe r", c.R, 0.8164, 0.0006)
		if c.N != 11 || c.DF != 9 {
			t.Errorf("n/df = %d/%d", c.N, c.DF)
		}
	}
	c, _ := Pearson(anscombeX, anscombeY1)
	approx(t, "r", c.R, 0.81642052, 1e-7)
	approx(t, "t", c.T, 4.24146, 1e-4)
	approx(t, "p", c.P, 0.00216963, 1e-6)
	approx(t, "cov", Covariance(anscombeX, anscombeY1), 5.501, 1e-12)

	// Missing values: pairwise deletion.
	x := append([]float64{nan, 1}, anscombeX...)
	y := append([]float64{3, nan}, anscombeY1...)
	c2, err := Pearson(x, y)
	if err != nil || c2.N != 11 || math.Abs(c2.R-c.R) > 1e-14 {
		t.Errorf("pairwise deletion: %+v %v", c2, err)
	}
	// Perfect correlation.
	c, err = Pearson([]float64{1, 2, 3, 4}, []float64{-2, -4, -6, -8})
	if err != nil || c.R != -1 || c.P != 0 || !(c.T < -1e200) {
		t.Errorf("perfect: %+v %v", c, err)
	}
	if _, err := json.Marshal(c); err != nil {
		t.Error("corr result must be JSON-safe:", err)
	}
	// Errors.
	if _, err := Pearson([]float64{1, 2}, []float64{1}); !errors.Is(err, ErrDimension) {
		t.Error("dimension", err)
	}
	if _, err := Pearson([]float64{1, 2, nan}, []float64{1, 2, 3}); !errors.Is(err, ErrInsufficientData) {
		t.Error("insufficient", err)
	}
	if _, err := Pearson([]float64{1, 1, 1}, []float64{1, 2, 3}); !errors.Is(err, ErrDegenerate) {
		t.Error("degenerate", err)
	}
	if _, err := Pearson(nil, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("empty", err)
	}
	wantNaN(t, "cov empty", Covariance(nil, nil))
}

func TestSpearman(t *testing.T) {
	// Textbook example (IQ vs. hours of TV): rho = -29/165, p = 0.627188.
	iq := []float64{106, 86, 100, 101, 99, 103, 97, 113, 112, 110}
	tv := []float64{7, 0, 27, 50, 28, 29, 20, 12, 6, 17}
	c, err := Spearman(iq, tv)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "rho", c.R, -29.0/165, 1e-12)
	approx(t, "p", c.P, 0.627188, 1e-6)
	// Monotone but non-linear: rho = 1 while Pearson r < 1.
	x := []float64{1, 2, 3, 4, 5, 6, nan}
	y := []float64{1, 8, 27, 64, 125, 216, 5}
	c, _ = Spearman(x, y)
	approx(t, "monotone", c.R, 1, 1e-15)
	p, _ := Pearson(x, y)
	if !(p.R < 0.99) {
		t.Error("pearson of cubic should be < 1")
	}
	// Ranks with ties.
	r := Ranks([]float64{10, 20, 10, nan, 30, 20, 20})
	want := []float64{1.5, 4, 1.5, nan, 6, 4, 4}
	for i := range r {
		if math.IsNaN(want[i]) != math.IsNaN(r[i]) || (!math.IsNaN(want[i]) && r[i] != want[i]) {
			t.Fatalf("Ranks = %v", r)
		}
	}
	if _, err := Spearman([]float64{1, 2, 3}, []float64{4, 4, 4}); !errors.Is(err, ErrDegenerate) {
		t.Error("degenerate", err)
	}
}

func TestPartialCorr(t *testing.T) {
	r := newRand(4)
	n := 400
	x, y, z := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		z[i] = r.NormFloat64()
		x[i] = 2*z[i] + r.NormFloat64()
		y[i] = -z[i] + 0.5*x[i] + r.NormFloat64()
	}
	x[5], y[9], z[11] = nan, nan, nan
	pc, err := PartialCorr(x, y, z)
	if err != nil {
		t.Fatal(err)
	}
	if pc.N != n-3 || pc.DF != n-6 {
		t.Errorf("n/df = %d/%d", pc.N, pc.DF)
	}
	// Independent definition: correlate the residuals of x~z and y~z.
	var xs, ys, zs []float64
	for i := 0; i < n; i++ {
		if IsFinite(x[i]) && IsFinite(y[i]) && IsFinite(z[i]) {
			xs, ys, zs = append(xs, x[i]), append(ys, y[i]), append(zs, z[i])
		}
	}
	fx, _ := LinearRegression(zs, xs)
	fy, _ := LinearRegression(zs, ys)
	rx, ry := make([]float64, len(xs)), make([]float64, len(xs))
	for i := range xs {
		rx[i] = xs[i] - fx.Predict(zs[i])
		ry[i] = ys[i] - fy.Predict(zs[i])
	}
	ref, _ := Pearson(rx, ry)
	approx(t, "partial vs residual method", pc.R, ref.R, 1e-12)
	// True partial correlation of x and y given z is 0.5/sqrt(1.25) = 0.447.
	approx(t, "partial value", pc.R, 0.4472, 0.08)
	if !(pc.P < 1e-6) {
		t.Errorf("p = %v", pc.P)
	}

	// A pure confounder: marginal correlation is strong, partial vanishes.
	for i := 0; i < n; i++ {
		z[i] = r.NormFloat64()
		x[i] = z[i] + 0.3*r.NormFloat64()
		y[i] = z[i] + 0.3*r.NormFloat64()
	}
	marg, _ := Pearson(x, y)
	pc, _ = PartialCorr(x, y, z)
	if !(marg.R > 0.85) || !(math.Abs(pc.R) < 0.15) {
		t.Errorf("confounder: marginal %v partial %v", marg.R, pc.R)
	}
	if _, err := PartialCorr(x, y, x); !errors.Is(err, ErrDegenerate) {
		t.Error("collinear covariate", err)
	}
	if _, err := PartialCorr(x[:3], y[:3], z[:3]); !errors.Is(err, ErrInsufficientData) {
		t.Error("insufficient", err)
	}
	if _, err := PartialCorr(x, y, z[:5]); !errors.Is(err, ErrDimension) {
		t.Error("dimension", err)
	}
}

func TestCrossCorr(t *testing.T) {
	r := newRand(5)
	n := 500
	x := make([]float64, n)
	for i := range x {
		x[i] = r.NormFloat64()
	}
	// y follows x by 3 samples.
	y := make([]float64, n)
	for i := range y {
		y[i] = 0.2 * r.NormFloat64()
		if i >= 3 {
			y[i] += x[i-3]
		}
	}
	x[40], y[77] = nan, nan
	cc := CrossCorr(x, y, 6)
	if len(cc) != 13 || cc[0].Lag != -6 || cc[12].Lag != 6 {
		t.Fatalf("lags: %+v", cc)
	}
	best, err := BestLag(x, y, 6, 50)
	if err != nil || best.Lag != 3 || !(best.R > 0.95) {
		t.Errorf("best = %+v %v", best, err)
	}
	// Reversed roles give the mirrored lag; negative correlation is found too.
	best, _ = BestLag(y, x, 6, 50)
	if best.Lag != -3 {
		t.Errorf("mirrored lag = %d", best.Lag)
	}
	neg := make([]float64, n)
	for i := range neg {
		neg[i] = -y[i]
	}
	best, _ = BestLag(x, neg, 6, 50)
	if best.Lag != 3 || !(best.R < -0.95) {
		t.Errorf("negative best = %+v", best)
	}
	// Lag 0 equals Pearson.
	p, _ := Pearson(x, y)
	approx(t, "lag0", cc[6].R, p.R, 1e-12)
	if cc[6].N != p.N {
		t.Error("lag0 n")
	}
	// Degenerate inputs.
	if _, err := BestLag([]float64{1, 2}, []float64{1, 2}, 3, 0); !errors.Is(err, ErrInsufficientData) {
		t.Error("short", err)
	}
	if _, err := BestLag(nil, nil, 3, 0); !errors.Is(err, ErrInsufficientData) {
		t.Error("nil", err)
	}
	for _, c := range CrossCorr([]float64{1, 1, 1, 1, 1}, []float64{1, 2, 3, 4, 5}, 1) {
		wantNaN(t, "constant series", c.R)
	}
	if len(CrossCorr(x, y, -4)) != 1 {
		t.Error("negative maxLag")
	}

	// Autocorrelation of an AR(1) process: r[k] ~ phi^k.
	ar := make([]float64, 20000)
	for i := 1; i < len(ar); i++ {
		ar[i] = 0.7*ar[i-1] + r.NormFloat64()
	}
	ar[100] = nan
	ac := Autocorr(ar, 3)
	approx(t, "acf0", ac[0], 1, 1e-12)
	approx(t, "acf1", ac[1], 0.7, 0.03)
	approx(t, "acf2", ac[2], 0.49, 0.04)
	if Autocorr([]float64{2, 2, 2}, 2) != nil || Autocorr(nil, 2) != nil {
		t.Error("degenerate acf should be nil")
	}
}

func TestBinnedResponse(t *testing.T) {
	var x, y []float64
	for i := 0; i < 100; i++ {
		x = append(x, float64(i))
		y = append(y, 2*float64(i)+1)
	}
	x = append(x, nan, 50)
	y = append(y, 3, nan)
	bins, err := BinnedResponse(x, y, &BinOptions{Bins: 4, Lo: 0, Hi: 100})
	if err != nil || len(bins) != 4 {
		t.Fatalf("%v %v", bins, err)
	}
	for b, bin := range bins {
		if bin.N != 25 || bin.Lo != float64(25*b) || bin.Hi != float64(25*b+25) {
			t.Errorf("bin %d: %+v", b, bin)
		}
		approx(t, "xmean", bin.XMean, float64(25*b)+12, 1e-12)
		approx(t, "ymean", bin.YMean, 2*(float64(25*b)+12)+1, 1e-12)
		approx(t, "median", bin.YQ[1], bin.YMean, 1e-12)
		approx(t, "q10", bin.YQ[0], 2*(float64(25*b)+2.4)+1, 1e-12)
		approx(t, "ystd", bin.YStd, 2*math.Sqrt(25.0*26/12), 1e-10)
	}
	// Auto range, default options: x == max belongs to the last bin.
	bins, err = BinnedResponse(x, y, nil)
	if err != nil || len(bins) != 10 {
		t.Fatalf("default: %d bins, %v", len(bins), err)
	}
	total := 0
	for _, b := range bins {
		total += b.N
	}
	if total != 100 {
		t.Errorf("total = %d", total)
	}
	// Equal-count bins.
	bins, err = BinnedResponse(x, y, &BinOptions{Bins: 5, EqualCount: true, Quantiles: []float64{0.5}})
	if err != nil || len(bins) != 5 {
		t.Fatal(err)
	}
	for b, bin := range bins {
		if bin.N != 20 || bin.Lo != float64(20*b) || bin.Hi != float64(20*b+19) || len(bin.YQ) != 1 {
			t.Errorf("equal-count bin %d: %+v", b, bin)
		}
	}
	// Sparse data: empty bins are dropped, MinCount filters.
	bins, _ = BinnedResponse([]float64{0, 0.1, 9.9, 10}, []float64{1, 2, 3, 4}, &BinOptions{Bins: 10})
	if len(bins) != 2 || bins[0].N != 2 || bins[1].N != 2 {
		t.Errorf("sparse: %+v", bins)
	}
	bins, _ = BinnedResponse([]float64{0, 0.1, 9.9, 10, 5}, []float64{1, 2, 3, 4, 5}, &BinOptions{Bins: 10, MinCount: 2})
	if len(bins) != 2 {
		t.Errorf("mincount: %+v", bins)
	}
	// Constant x.
	bins, err = BinnedResponse([]float64{3, 3, 3}, []float64{1, 2, 3}, nil)
	if err != nil || len(bins) != 1 || bins[0].N != 3 || bins[0].YMean != 2 {
		t.Errorf("constant x: %+v %v", bins, err)
	}
	if _, err := json.Marshal(bins); err != nil {
		t.Error(err)
	}
	if _, err := BinnedResponse(nil, nil, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("empty", err)
	}
	if _, err := BinnedResponse([]float64{1}, []float64{1, 2}, nil); !errors.Is(err, ErrDimension) {
		t.Error("dimension", err)
	}
	if _, err := BinnedResponse([]float64{1}, []float64{1}, &BinOptions{Quantiles: []float64{2}}); !errors.Is(err, ErrInvalidArgument) {
		t.Error("bad quantile", err)
	}
}
