package stat

// Independent numerical verification of the package.
//
// The expected values in this file do not come from the package itself:
//
//   - refData (reference_data_test.go) was produced by a stand-alone Node.js
//     program that uses deliberately different, brute-force methods: Simpson
//     integration of the t / F / beta / normal densities, Gaussian elimination
//     on the normal equations, plain Newton iterations for the logistic
//     likelihood, the textbook component form of Holt-Winters, penalised
//     least squares for the Kalman smoothers and impulse responses for the
//     forecast variances. The pseudo-random inputs are regenerated here with
//     the same 32-bit LCG (refRand), so both sides see identical data.
//   - NIST StRD certified values (Norris, Longley, NoInt1), Anscombe's
//     quartet and classic t-table critical values are published numbers.
//   - Everything else is checked against naive O(n^2) loops written in the
//     test itself.

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"testing"
)

// ---------------------------------------------------------------- helpers

// refRand is the LCG shared with the Node.js reference program.
type refRand struct{ s uint32 }

func (r *refRand) u() float64 {
	r.s = r.s*1664525 + 1013904223
	return float64(r.s) / 4294967296
}

// norm is the sum of 12 uniforms minus 6 (no transcendental functions, so
// the stream is bit-identical in Go and JavaScript).
func (r *refRand) norm() float64 {
	a := 0.0
	for i := 0; i < 12; i++ {
		a += r.u()
	}
	return a - 6
}

// refNear checks |got-want| <= tol*max(1, |want|).
func refNear(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(want) {
		if !math.IsNaN(got) {
			t.Errorf("%s: got %v, want NaN", name, got)
		}
		return
	}
	if math.IsNaN(got) || math.Abs(got-want) > tol*math.Max(1, math.Abs(want)) {
		t.Errorf("%s: got %.15g, want %.15g (diff %.3g, tol %.1g)", name, got, want, got-want, tol)
	}
}

// refRel checks the relative error (for tiny p-values).
func refRel(t *testing.T, name string, got, want, rel float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > rel*math.Abs(want) {
		t.Errorf("%s: got %.15g, want %.15g (rel %.3g, tol %.1g)", name, got, want, (got-want)/want, rel)
	}
}

func refSlice(t *testing.T, name string, got, want []float64, tol float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: len %d, want %d", name, len(got), len(want))
		return
	}
	for i := range want {
		if math.IsNaN(want[i]) {
			if !math.IsNaN(got[i]) {
				t.Errorf("%s[%d]: got %v, want NaN", name, i, got[i])
			}
			continue
		}
		if math.IsNaN(got[i]) || math.Abs(got[i]-want[i]) > tol*math.Max(1, math.Abs(want[i])) {
			t.Errorf("%s[%d]: got %.15g, want %.15g (diff %.3g)", name, i, got[i], want[i], got[i]-want[i])
		}
	}
}

func ref(t *testing.T, key string) []float64 {
	t.Helper()
	v, ok := refData[key]
	if !ok {
		t.Fatalf("missing reference entry %q", key)
	}
	return v
}

// noPanic runs f and reports a panic as a test failure.
func noPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: panic: %v", name, r)
		}
	}()
	f()
}

// jsonSafe fails when v cannot be marshalled (NaN or Inf inside).
func jsonSafe(t *testing.T, name string, v any) {
	t.Helper()
	if _, err := json.Marshal(v); err != nil {
		t.Errorf("%s: not JSON-safe: %v", name, err)
	}
}

// naiveQuantile7 is the textbook type-7 definition on a sorted copy.
func naiveQuantile7(x []float64, p float64) float64 {
	var s []float64
	for _, v := range x {
		if !math.IsNaN(v) {
			s = append(s, v)
		}
	}
	if len(s) == 0 {
		return math.NaN()
	}
	sort.Float64s(s)
	h := float64(len(s)-1) * p
	lo, hi := int(math.Floor(h)), int(math.Ceil(h))
	if lo == hi {
		return s[lo]
	}
	return s[lo]*(float64(hi)-h) + s[hi]*(h-float64(lo))
}

func naiveMeanStd(v []float64) (mean, std float64, n int) {
	s := 0.0
	for _, x := range v {
		if !math.IsNaN(x) {
			s += x
			n++
		}
	}
	if n == 0 {
		return math.NaN(), math.NaN(), 0
	}
	mean = s / float64(n)
	ss := 0.0
	for _, x := range v {
		if !math.IsNaN(x) {
			ss += (x - mean) * (x - mean)
		}
	}
	if n < 2 {
		return mean, math.NaN(), n
	}
	return mean, math.Sqrt(ss / float64(n-1)), n
}

func naivePearson(a, b []float64) float64 {
	ma, _, _ := naiveMeanStd(a)
	mb, _, _ := naiveMeanStd(b)
	var sab, saa, sbb float64
	for i := range a {
		sab += (a[i] - ma) * (b[i] - mb)
		saa += (a[i] - ma) * (a[i] - ma)
		sbb += (b[i] - mb) * (b[i] - mb)
	}
	return sab / math.Sqrt(saa*sbb)
}

// ------------------------------------------------------ special functions

func TestRefSpecialFunctions(t *testing.T) {
	pts := [][2]float64{{0.5, 1}, {1.3, 2.5}, {-2.1, 4}, {2.228138852, 10}, {0.01, 7}, {3.7, 33.5}, {-0.8, 120}, {5.5, 3}, {1.96, 1000}, {12, 2}}
	cdf, p2 := ref(t, "t_cdf"), ref(t, "t_p2")
	for i, p := range pts {
		refNear(t, "StudentTCDF", StudentTCDF(p[0], p[1]), cdf[i], 1e-9)
		refRel(t, "StudentTTwoSidedP", StudentTTwoSidedP(p[0], p[1]), p2[i], 1e-7)
	}
	qs := [][2]float64{{0.975, 1}, {0.975, 2}, {0.95, 5}, {0.975, 10}, {0.995, 30}, {0.975, 120}, {0.9, 3.5}, {0.999, 8}, {0.75, 57.3}}
	for i, q := range qs {
		refNear(t, "StudentTQuantile", StudentTQuantile(q[0], q[1]), ref(t, "t_q")[i], 2e-8)
		refNear(t, "StudentTQuantile lower", StudentTQuantile(1-q[0], q[1]), -ref(t, "t_q")[i], 2e-8)
	}
	// Published two-sided 5 % / 1 % critical values of Student's t.
	table := []struct{ p, df, want float64 }{
		{0.975, 1, 12.7062047364}, {0.975, 2, 4.30265272991}, {0.975, 3, 3.18244630528},
		{0.975, 5, 2.57058183661}, {0.975, 10, 2.22813885196}, {0.975, 20, 2.08596344727},
		{0.975, 30, 2.04227245630}, {0.975, 60, 2.00029782106}, {0.975, 120, 1.97993040505},
		{0.995, 10, 3.16927267261}, {0.95, 10, 1.81246112281}, {0.995, 5, 4.03214298356},
	}
	for _, c := range table {
		refNear(t, "t table", StudentTQuantile(c.p, c.df), c.want, 1e-9)
	}
	// Very large df converges to the normal quantile.
	refNear(t, "t(1e7) -> z", StudentTQuantile(0.975, 1e7), 1.959963984540054, 1e-6)

	fs := [][3]float64{{4.0, 3, 20}, {1.0, 1, 5}, {2.5, 6, 9}, {0.3, 2, 40}, {10, 4, 12}, {1.7, 10, 100}}
	for i, f := range fs {
		refRel(t, "FDistSF", FDistSF(f[0], f[1], f[2]), ref(t, "f_sf")[i], 1e-7)
	}
	// F critical value F(0.95; 3, 20) = 3.098391 (table).
	refNear(t, "F table", FDistSF(3.0983912121407795, 3, 20), 0.05, 1e-9)
	bs := [][3]float64{{2, 3, 0.4}, {5.5, 1.5, 0.9}, {1, 1, 0.3}, {10, 20, 0.25}, {3, 1.5, 0.01}, {50, 60, 0.5}}
	for i, b := range bs {
		refRel(t, "RegIncBeta", RegIncBeta(b[0], b[1], b[2]), ref(t, "inc_beta")[i], 1e-7)
		// Symmetry I_x(a,b) = 1 - I_{1-x}(b,a).
		refNear(t, "RegIncBeta symmetry", RegIncBeta(b[0], b[1], b[2])+RegIncBeta(b[1], b[0], 1-b[2]), 1, 1e-13)
	}
	for i, x := range []float64{-3, -1.5, 0, 0.7, 1.959963984540054, 4} {
		refNear(t, "NormalCDF", NormalCDF(x), ref(t, "norm_cdf")[i], 1e-10)
	}
	for i, p := range []float64{0.6, 0.9, 0.975, 0.999} {
		refNear(t, "NormalQuantile", NormalQuantile(p), ref(t, "norm_q")[i], 1e-9)
		refNear(t, "NormalQuantile lower", NormalQuantile(1-p), -ref(t, "norm_q")[i], 1e-9)
	}
	refNear(t, "z(.975)", NormalQuantile(0.975), 1.959963984540054, 1e-13)
	refNear(t, "z(.995)", NormalQuantile(0.995), 2.5758293035489004, 1e-13)
	if !math.IsInf(NormalQuantile(0), -1) || !math.IsInf(NormalQuantile(1), 1) {
		t.Error("NormalQuantile(0/1) must be -Inf/+Inf")
	}
	// Extreme arguments stay in range and never produce NaN.
	for _, df := range []float64{0.1, 1, 3, 1e3, 1e8} {
		for _, x := range []float64{0, 1e-300, 1e-8, 1, 1e8, 1e300} {
			p := StudentTTwoSidedP(x, df)
			c := StudentTCDF(x, df)
			if math.IsNaN(p) || p < 0 || p > 1 || math.IsNaN(c) || c < 0.5 || c > 1 {
				t.Errorf("t(%g, df=%g): p=%v cdf=%v", x, df, p, c)
			}
		}
	}
}

// ------------------------------------------------------------- quantiles

func TestRefQuantileType7(t *testing.T) {
	// R: quantile(c(15,20,35,40,50), c(0,.25,.4,.5,.75,.9,1)) (type 7, also numpy default).
	x := []float64{40, 15, 50, 20, 35}
	orig := append([]float64(nil), x...)
	ps := []float64{0, 0.25, 0.4, 0.5, 0.75, 0.9, 1}
	want := []float64{15, 20, 29, 35, 40, 46, 50}
	for i, p := range ps {
		refNear(t, "Quantile", Quantile(x, p), want[i], 1e-14)
	}
	refSlice(t, "Quantiles", Quantiles(x, ps...), want, 1e-14)
	for i := range x {
		if x[i] != orig[i] {
			t.Fatal("Quantile modified its input")
		}
	}
	// R: quantile(c(1,3,4,9,10,20), c(.1,.33,.5,.9)) = 2, 3.65, 6.5, 15.
	y := []float64{20, 1, math.NaN(), 9, 3, 10, 4, math.NaN()}
	refSlice(t, "Quantiles NaN", Quantiles(y, 0.1, 0.33, 0.5, 0.9), []float64{2, 3.65, 6.5, 15}, 1e-14)
	refNear(t, "Median even", Median(y), 6.5, 1e-14)
	// numpy: np.percentile(np.arange(1, 11), [25, 90, 99]) = 3.25, 9.1, 9.91.
	z := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	refSlice(t, "Quantiles 1..10", Quantiles(z, 0.25, 0.9, 0.99), []float64{3.25, 9.1, 9.91}, 1e-14)
	d := Describe(z)
	if d.N != 10 || d.Q1 != 3.25 || d.Median != 5.5 || d.Q3 != 7.75 || d.Min != 1 || d.Max != 10 {
		t.Errorf("Describe: %+v", d)
	}
	refNear(t, "Describe.Std", d.Std, 3.0276503540974917, 1e-14)

	// Random data against the naive definition.
	r := &refRand{11}
	for trial := 0; trial < 50; trial++ {
		n := 1 + int(r.u()*30)
		v := make([]float64, n)
		for i := range v {
			v[i] = math.Floor(r.u()*40) / 4 // ties
			if r.u() < 0.1 {
				v[i] = math.NaN()
			}
		}
		for _, p := range []float64{0, 0.01, 0.1, 0.25, 1.0 / 3, 0.5, 0.9, 0.999, 1} {
			refNear(t, "Quantile vs naive", Quantile(v, p), naiveQuantile7(v, p), 1e-13)
		}
	}
	for _, p := range []float64{-0.01, 1.01, math.NaN(), math.Inf(1)} {
		if !math.IsNaN(Quantile(z, p)) {
			t.Errorf("Quantile(p=%v) must be NaN", p)
		}
	}
	// A value next to an infinity must not turn into NaN (0*Inf).
	inf := []float64{1, 2, math.Inf(1)}
	if got := Quantile(inf, 0.5); got != 2 {
		t.Errorf("median of {1,2,+Inf} = %v, want 2", got)
	}
	if got := Quantile([]float64{math.Inf(-1), 5, 7}, 0.5); got != 5 {
		t.Errorf("median of {-Inf,5,7} = %v, want 5", got)
	}
	if got := Quantile([]float64{math.Inf(1), math.Inf(1)}, 0.3); !math.IsInf(got, 1) {
		t.Errorf("quantile of {+Inf,+Inf} = %v, want +Inf", got)
	}
}

func TestRefRobustAndWeighted(t *testing.T) {
	// MAD of {1,1,2,2,4,6,9}: median 2, |dev| = {1,1,0,0,2,4,7} -> 1.
	x := []float64{1, 1, 2, 2, 4, 6, 9}
	refNear(t, "MAD", MAD(x), 1, 0)
	refNear(t, "RobustStd", RobustStd(x), 1.482602218505602, 1e-15)
	// MADScale is 1/Phi^-1(0.75).
	refNear(t, "MADScale", MADScale, 1/NormalQuantile(0.75), 1e-12)
	z := RobustZScores(x)
	for i, v := range x {
		refNear(t, "RobustZ", z[i], (v-2)/1.482602218505602, 1e-14)
	}
	// Integer weights must behave like repeating the observations
	// (inverse ECDF, averaged at exact steps = R quantile type 2).
	r := &refRand{12}
	for trial := 0; trial < 200; trial++ {
		n := 1 + int(r.u()*8)
		v, w := make([]float64, n), make([]float64, n)
		var expanded []float64
		for i := range v {
			v[i] = math.Floor(r.u() * 10)
			w[i] = math.Floor(r.u() * 4) // may be 0: ignored
			for k := 0; k < int(w[i]); k++ {
				expanded = append(expanded, v[i])
			}
		}
		sort.Float64s(expanded)
		for _, p := range []float64{0, 0.1, 0.25, 0.5, 0.75, 1} {
			got := WeightedQuantile(v, w, p)
			if len(expanded) == 0 {
				if !math.IsNaN(got) {
					t.Fatalf("WeightedQuantile of zero weight = %v", got)
				}
				continue
			}
			np := float64(len(expanded)) * p
			var want float64
			switch {
			case np == math.Floor(np) && np > 0 && int(np) < len(expanded):
				want = (expanded[int(np)-1] + expanded[int(np)]) / 2
			case np == 0:
				want = expanded[0]
			default:
				want = expanded[int(math.Ceil(np))-1]
			}
			if got != want {
				t.Fatalf("WeightedQuantile(%v, %v, %v) = %v, want %v", v, w, p, got, want)
			}
		}
		// Weighted mean against the expansion.
		if len(expanded) > 0 {
			m, _, _ := naiveMeanStd(expanded)
			refNear(t, "WeightedMean", WeightedMean(v, w), m, 1e-13)
		}
	}
	// Trimmed mean: scipy.stats.trim_mean([1..10], 0.2) = 5.5; with an outlier.
	refNear(t, "TrimmedMean", TrimmedMean([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 1000}, 0.1), 5.5, 1e-14)
	refNear(t, "TrimmedMean 0", TrimmedMean([]float64{1, 2, 6}, 0), 3, 1e-14)
	refNear(t, "TrimmedMean .5", TrimmedMean([]float64{1, 2, 60}, 0.5), 2, 1e-14)
	// floor(frac*n) must not be thrown off by rounding: 0.29*100 is
	// 28.999999999999996 in binary, but 29 values go from each end.
	hundred := make([]float64, 100)
	for i := range hundred {
		hundred[i] = float64(i * i)
	}
	s29 := 0.0
	for i := 29; i < 71; i++ {
		s29 += hundred[i]
	}
	refNear(t, "TrimmedMean 0.29", TrimmedMean(hundred, 0.29), s29/42, 1e-14)
	// Ranks with ties (R: rank(c(10,20,10,30,20,10)) = 2 4.5 2 6 4.5 2).
	refSlice(t, "Ranks", Ranks([]float64{10, 20, 10, 30, 20, 10}), []float64{2, 4.5, 2, 6, 4.5, 2}, 0)
}

func TestRefHistQuantile(t *testing.T) {
	// The range_hour layout: uint32 counts, 5 km bins starting at 0.
	counts := []uint32{0, 2, 0, 6, 2}
	cases := []struct{ p, want float64 }{
		{0, 5}, {0.1, 7.5}, {0.2, 10}, {0.5, 17.5}, {0.8, 20}, {0.9, 22.5}, {1, 25},
	}
	edges := []float64{0, 5, 10, 15, 20, 25}
	for _, c := range cases {
		refNear(t, "HistQuantile", HistQuantile(counts, 0, 5, c.p), c.want, 1e-14)
		refNear(t, "HistQuantileEdges", HistQuantileEdges(counts, edges, c.p), c.want, 1e-14)
	}
	refNear(t, "HistMean", HistMean(counts, 0, 5), (2*7.5+6*17.5+2*22.5)/10, 1e-14)
	// Unequal edges.
	refNear(t, "HistQuantileEdges uneven", HistQuantileEdges([]int{1, 1}, []float64{0, 1, 11}, 0.75), 6, 1e-14)
	// Against the exact quantiles of a large uniformly spread sample: the
	// histogram quantile may deviate by at most one bin width.
	r := &refRand{13}
	xs := make([]float64, 20000)
	for i := range xs {
		u := r.u()
		xs[i] = 400 * u * u // skewed, 0..400 km
	}
	h, under, over := Histogram(xs, 0, 5, 120)
	if under != 0 || over != 0 || Sum(h) != float64(len(xs)) {
		t.Fatalf("Histogram: under=%d over=%d sum=%v", under, over, Sum(h))
	}
	u32 := make([]uint32, len(h))
	for i, c := range h {
		u32[i] = uint32(c)
	}
	for _, p := range []float64{0.01, 0.1, 0.5, 0.9, 0.95, 0.99} {
		exact := naiveQuantile7(xs, p)
		if got := HistQuantile(u32, 0, 5, p); math.Abs(got-exact) > 0.5 {
			t.Errorf("HistQuantile(p=%v) = %v, exact %v", p, got, exact)
		}
	}
	m, _, _ := naiveMeanStd(xs)
	if got := HistMean(u32, 0, 5); math.Abs(got-m) > 0.5 {
		t.Errorf("HistMean = %v, exact %v", got, m)
	}
	// Monotone in p, and bounded by the occupied range.
	prev := math.Inf(-1)
	for p := 0.0; p <= 1.0; p += 1.0 / 64 {
		q := HistQuantile(u32, 0, 5, p)
		if q < prev || q < 0 || q > 400 {
			t.Fatalf("HistQuantile not monotone/bounded at p=%v: %v after %v", p, q, prev)
		}
		prev = q
	}
	// Degenerate histograms.
	for _, c := range [][]uint32{nil, {}, {0, 0, 0}} {
		if !math.IsNaN(HistQuantile(c, 0, 5, 0.5)) || !math.IsNaN(HistMean(c, 0, 5)) {
			t.Errorf("empty histogram %v must give NaN", c)
		}
	}
	if !math.IsNaN(HistQuantileEdges(counts, edges[:4], 0.5)) {
		t.Error("edge count mismatch must give NaN")
	}
	if !math.IsNaN(HistQuantile(counts, 0, 5, 1.5)) || !math.IsNaN(HistQuantile(counts, 0, 5, math.NaN())) {
		t.Error("p outside [0,1] must give NaN")
	}
	// A single occupied bin: every quantile lies inside it.
	one := []uint32{0, 0, 7, 0}
	refNear(t, "single bin p=0", HistQuantile(one, 0, 5, 0), 10, 0)
	refNear(t, "single bin p=.5", HistQuantile(one, 0, 5, 0.5), 12.5, 1e-14)
	refNear(t, "single bin p=1", HistQuantile(one, 0, 5, 1), 15, 0)
	// Histogram bin edges: lo inclusive, hi exclusive.
	hc, un, ov := Histogram([]float64{-0.001, 0, 4.999, 5, 9.999, 10, math.NaN(), math.Inf(1), math.Inf(-1)}, 0, 5, 2)
	if hc[0] != 2 || hc[1] != 2 || un != 2 || ov != 2 {
		t.Errorf("Histogram edges: %v under=%d over=%d", hc, un, ov)
	}
}

// ----------------------------------------------------------- correlation

func refCaseA() (x, y, z []float64) {
	r := &refRand{1}
	const n = 40
	x, y, z = make([]float64, n), make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		xi := math.Floor(r.u() * 20)
		e := r.norm()
		zn := r.norm()
		x[i] = xi
		y[i] = float64(0.3*xi) + float64(2*e)
		z[i] = float64(0.5*xi) + float64(3*zn)
	}
	x[5], y[17], z[30] = math.NaN(), math.NaN(), math.NaN()
	return
}

func TestRefCorrelation(t *testing.T) {
	x, y, z := refCaseA()
	want := ref(t, "A_pearson")
	c, err := Pearson(x, y)
	if err != nil {
		t.Fatal(err)
	}
	refNear(t, "Pearson.R", c.R, want[0], 1e-12)
	if c.N != int(want[1]) || c.DF != c.N-2 {
		t.Errorf("Pearson N=%d DF=%d, want N=%v DF=N-2", c.N, c.DF, want[1])
	}
	refNear(t, "Pearson.T", c.T, want[2], 1e-10)
	refRel(t, "Pearson.P", c.P, want[3], 1e-6)

	want = ref(t, "A_spearman")
	s, err := Spearman(x, y)
	if err != nil {
		t.Fatal(err)
	}
	refNear(t, "Spearman.R", s.R, want[0], 1e-12)
	refNear(t, "Spearman.T", s.T, want[1], 1e-10)
	refRel(t, "Spearman.P", s.P, want[2], 1e-6)
	if s.DF != s.N-2 {
		t.Errorf("Spearman DF=%d N=%d", s.DF, s.N)
	}
	refNear(t, "Covariance", Covariance(x, y), ref(t, "A_cov")[0], 1e-12)

	want = ref(t, "A_partial")
	p, err := PartialCorr(x, y, z)
	if err != nil {
		t.Fatal(err)
	}
	refNear(t, "PartialCorr.R", p.R, want[0], 1e-11)
	if p.N != int(want[1]) || p.DF != p.N-3 {
		t.Errorf("PartialCorr N=%d DF=%d, want N=%v DF=N-3", p.N, p.DF, want[1])
	}
	refNear(t, "PartialCorr.T", p.T, want[2], 1e-9)
	refRel(t, "PartialCorr.P", p.P, want[3], 1e-6)

	want = ref(t, "A_linreg")
	f, err := LinearRegression(x, y)
	if err != nil {
		t.Fatal(err)
	}
	got := []float64{f.Slope, f.Intercept, f.R2, f.SlopeSE, f.InterceptSE, f.T, f.P, f.ResidStd}
	refSlice(t, "LinearRegression", got[:6], want[:6], 1e-11)
	refRel(t, "LinearRegression.P", f.P, want[6], 1e-6)
	refNear(t, "LinearRegression.ResidStd", f.ResidStd, want[7], 1e-11)
	// r^2 of the simple regression is Pearson's r squared, and the slope's
	// p-value equals the correlation's p-value.
	refNear(t, "R2 = r^2", f.R2, c.R*c.R, 1e-12)
	refRel(t, "P slope = P corr", f.P, c.P, 1e-9)
	want = ref(t, "A_linreg_int")
	yh, lo, hi := f.PredictInterval(25, 0.9)
	_, clo, chi := f.ConfidenceInterval(25, 0.9)
	refSlice(t, "LinearFit intervals", []float64{yh, lo, hi, clo, chi}, want, 1e-8)
	jsonSafe(t, "LinearFit", f)
	jsonSafe(t, "CorrResult", c)

	// Anscombe's quartet (published): all four sets share r = 0.816,
	// slope 0.500, intercept 3.00.
	ax := []float64{10, 8, 13, 9, 11, 14, 6, 4, 12, 7, 5}
	ay := [][]float64{
		{8.04, 6.95, 7.58, 8.81, 8.33, 9.96, 7.24, 4.26, 10.84, 4.82, 5.68},
		{9.14, 8.14, 8.74, 8.77, 9.26, 8.10, 6.13, 3.10, 9.13, 7.26, 4.74},
		{7.46, 6.77, 12.74, 7.11, 7.81, 8.84, 6.08, 5.39, 8.15, 6.42, 5.73},
	}
	for i, yy := range ay {
		c, err := Pearson(ax, yy)
		if err != nil {
			t.Fatal(err)
		}
		refNear(t, "Anscombe r", c.R, 0.8164, 2e-4)
		refNear(t, "Anscombe p", c.P, 0.00217, 2e-5)
		f, _ := LinearRegression(ax, yy)
		refNear(t, "Anscombe slope", f.Slope, 0.5, 1e-3)
		refNear(t, "Anscombe intercept", f.Intercept, 3.0, 3e-3)
		_ = i
	}
	a4x := []float64{8, 8, 8, 8, 8, 8, 8, 19, 8, 8, 8}
	a4y := []float64{6.58, 5.76, 7.71, 8.84, 8.47, 7.04, 5.25, 12.50, 5.56, 7.91, 6.89}
	c4, _ := Pearson(a4x, a4y)
	refNear(t, "Anscombe IV r", c4.R, 0.8165, 2e-4)

	// Spearman of a monotone but non-linear relation is exactly 1.
	mx := []float64{1, 2, 3, 4, 5, 6}
	my := []float64{1, 8, 27, 64, 125, 216}
	sm, err := Spearman(mx, my)
	if err != nil || sm.R != 1 || sm.P != 0 {
		t.Errorf("Spearman monotone: %+v %v", sm, err)
	}
	// Published example (Wikipedia, IQ vs. hours of TV): rho = -29/165.
	iq := []float64{106, 100, 86, 101, 99, 103, 97, 113, 112, 110}
	tv := []float64{7, 27, 2, 50, 28, 29, 20, 12, 6, 17}
	sw, _ := Spearman(iq, tv)
	refNear(t, "Spearman IQ/TV", sw.R, -29.0/165, 1e-13)
	refNear(t, "Spearman IQ/TV p", sw.P, 0.627188, 1e-5)
}

func TestRefCrossCorr(t *testing.T) {
	r := &refRand{21}
	const n = 120
	x := make([]float64, n)
	for i := range x {
		x[i] = r.norm()
	}
	// y follows x by 3 samples.
	y := make([]float64, n)
	for i := range y {
		if i >= 3 {
			y[i] = x[i-3] + 0.1*r.norm()
		} else {
			y[i] = r.norm()
		}
	}
	x[40], y[77] = math.NaN(), math.NaN()
	cc := CrossCorr(x, y, 6)
	if len(cc) != 13 {
		t.Fatalf("CrossCorr len %d", len(cc))
	}
	for _, c := range cc {
		// Naive pairing: x[t] with y[t+lag].
		var a, b []float64
		for i := 0; i < n; i++ {
			j := i + c.Lag
			if j < 0 || j >= n || math.IsNaN(x[i]) || math.IsNaN(y[j]) {
				continue
			}
			a = append(a, x[i])
			b = append(b, y[j])
		}
		if c.N != len(a) {
			t.Errorf("lag %d: N=%d, want %d", c.Lag, c.N, len(a))
		}
		refNear(t, "CrossCorr", c.R, naivePearson(a, b), 1e-12)
	}
	best, err := BestLag(x, y, 6, 10)
	if err != nil || best.Lag != 3 || best.R < 0.95 {
		t.Errorf("BestLag = %+v, %v; want lag +3 (y follows x)", best, err)
	}
	// Swapping the arguments flips the sign of the lag.
	best, _ = BestLag(y, x, 6, 10)
	if best.Lag != -3 {
		t.Errorf("BestLag swapped = %+v, want lag -3", best)
	}
	// Lag(x, k) is the matching transform: Pearson(Lag(x,3), y) is the lag-3 value.
	p, _ := Pearson(Lag(x, 3), y)
	refNear(t, "Lag consistency", p.R, cc[6+3].R, 1e-12)

	// Autocorrelation against the textbook biased estimator (Node).
	s := refCaseG()
	refSlice(t, "Autocorr", Autocorr(s, 3), ref(t, "G_acf"), 1e-12)
}

// ------------------------------------------------------------ regression

func TestRefNIST(t *testing.T) {
	// NIST StRD "Norris" (lower difficulty), y = B0 + B1*x, 36 observations.
	ny := []float64{0.1, 338.8, 118.1, 888.0, 9.2, 228.1, 668.5, 998.5, 449.1, 778.9, 559.2, 0.3, 0.1, 778.1, 668.8, 339.3, 448.9, 10.8, 557.7, 228.3, 998.0, 888.8, 119.6, 0.3, 0.6, 557.6, 339.3, 888.0, 998.5, 778.9, 10.2, 117.6, 228.9, 668.4, 449.2, 0.2}
	nx := []float64{0.2, 337.4, 118.2, 884.6, 10.1, 226.5, 666.3, 996.3, 448.6, 777.0, 558.2, 0.4, 0.6, 775.5, 666.9, 338.0, 447.5, 11.6, 556.0, 228.1, 995.8, 887.6, 120.2, 0.3, 0.3, 556.8, 339.1, 887.2, 999.0, 779.0, 11.1, 118.3, 229.2, 669.1, 448.9, 0.5}
	f, err := LinearRegression(nx, ny)
	if err != nil {
		t.Fatal(err)
	}
	refRel(t, "Norris B0", f.Intercept, -0.262323073774029, 1e-9)
	refRel(t, "Norris B1", f.Slope, 1.00211681802045, 1e-12)
	refRel(t, "Norris SE(B0)", f.InterceptSE, 0.232818234301152, 1e-9)
	refRel(t, "Norris SE(B1)", f.SlopeSE, 0.429796848199937e-03, 1e-9)
	refRel(t, "Norris resid SD", f.ResidStd, 0.884796396144373, 1e-9)
	refRel(t, "Norris R2", f.R2, 0.999993745883712, 1e-12)
	if f.N != 36 {
		t.Errorf("Norris N = %d", f.N)
	}
	// The same through the matrix path.
	X := make([][]float64, len(nx))
	for i, v := range nx {
		X[i] = []float64{v}
	}
	o, err := OLS(X, ny, true)
	if err != nil {
		t.Fatal(err)
	}
	refRel(t, "Norris OLS B0", o.Coef[0], -0.262323073774029, 1e-9)
	refRel(t, "Norris OLS B1", o.Coef[1], 1.00211681802045, 1e-12)
	refRel(t, "Norris OLS SE0", o.SE[0], 0.232818234301152, 1e-9)
	refRel(t, "Norris OLS SE1", o.SE[1], 0.429796848199937e-03, 1e-9)
	refRel(t, "Norris OLS F", o.F, 5436385.54079785, 1e-8)
	if o.DF != 34 {
		t.Errorf("Norris DF = %d", o.DF)
	}

	// NIST StRD "NoInt1": y = B1*x without intercept, x = 60..70, y = 130..140.
	var x0 [][]float64
	var y0 []float64
	for i := 0; i <= 10; i++ {
		x0 = append(x0, []float64{float64(60 + i)})
		y0 = append(y0, float64(130+i))
	}
	o, err = OLS(x0, y0, false)
	if err != nil {
		t.Fatal(err)
	}
	refRel(t, "NoInt1 B1", o.Coef[0], 2.07438016528926, 1e-13)
	refRel(t, "NoInt1 SE", o.SE[0], 0.165289256198347e-01, 1e-11)
	refRel(t, "NoInt1 resid SD", o.ResidStd, 3.56753034006338, 1e-12)
	refRel(t, "NoInt1 R2", o.R2, 0.999365492298663, 1e-12)
	if o.DF != 10 {
		t.Errorf("NoInt1 DF = %d", o.DF)
	}

	// NIST StRD "Longley" (higher difficulty): 6 strongly collinear
	// regressors; the normal equations lose all significant digits here.
	longley := [][]float64{
		{60323, 83.0, 234289, 2356, 1590, 107608, 1947},
		{61122, 88.5, 259426, 2325, 1456, 108632, 1948},
		{60171, 88.2, 258054, 3682, 1616, 109773, 1949},
		{61187, 89.5, 284599, 3351, 1650, 110929, 1950},
		{63221, 96.2, 328975, 2099, 3099, 112075, 1951},
		{63639, 98.1, 346999, 1932, 3594, 113270, 1952},
		{64989, 99.0, 365385, 1870, 3547, 115094, 1953},
		{63761, 100.0, 363112, 3578, 3350, 116219, 1954},
		{66019, 101.2, 397469, 2904, 3048, 117388, 1955},
		{67857, 104.6, 419180, 2822, 2857, 118734, 1956},
		{68169, 108.4, 442769, 2936, 2798, 120445, 1957},
		{66513, 110.8, 444546, 4681, 2637, 121950, 1958},
		{68655, 112.6, 482704, 3813, 2552, 123366, 1959},
		{69564, 114.2, 502601, 3931, 2514, 125368, 1960},
		{69331, 115.7, 518173, 4806, 2572, 127852, 1961},
		{70551, 116.9, 554894, 4007, 2827, 130081, 1962},
	}
	var lx [][]float64
	var ly []float64
	for _, row := range longley {
		ly = append(ly, row[0])
		// The table above lists unemployment and armed forces in thousands
		// (Longley 1967); the certified coefficients B3 and B4 belong to
		// those two columns scaled by 100 (235600, 159000, ...).
		lx = append(lx, []float64{row[1], row[2], 100 * row[3], 100 * row[4], row[5], row[6]})
	}
	o, err = OLS(lx, ly, true)
	if err != nil {
		t.Fatal(err)
	}
	coef := []float64{-3482258.63459582, 15.0618722713733, -0.358191792925910e-01, -0.202022980381683e-01, -0.103322686717359e-01, -0.511041056535807e-01, 1829.15146461355}
	se := []float64{890420.383607373, 84.9149257747669, 0.334910077722432e-01, 0.488399681651699e-02, 0.214274163161675e-02, 0.226073200069370, 455.478499142212}
	for j := range coef {
		refRel(t, "Longley coef", o.Coef[j], coef[j], 1e-7)
		refRel(t, "Longley SE", o.SE[j], se[j], 1e-7)
	}
	refRel(t, "Longley resid SD", o.ResidStd, 304.854073561965, 1e-8)
	refRel(t, "Longley R2", o.R2, 0.995479004577296, 1e-10)
	refRel(t, "Longley F", o.F, 330.285339234588, 1e-7)
	if o.DF != 9 || o.N != 16 {
		t.Errorf("Longley N=%d DF=%d", o.N, o.DF)
	}
	// Ridge with lambda = 0 must reproduce the certified fit as well.
	m, err := Ridge(lx, ly, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, icpt := m.RawCoef()
	refRel(t, "Longley ridge0 intercept", icpt, coef[0], 1e-6)
	for j := range raw {
		refRel(t, "Longley ridge0 coef", raw[j], coef[j+1], 1e-6)
	}
	refRel(t, "Longley ridge0 resid SD", m.ResidStd, 304.854073561965, 1e-7)
}

func refCaseB() (X [][]float64, y, w []float64) {
	r := &refRand{2}
	const n = 60
	for i := 0; i < n; i++ {
		x0 := r.u() * 10
		x1 := r.u() * 10
		x2 := float64(r.u()*10) + float64(0.5*x0)
		wi := 0.5 + float64(r.u()*2)
		e := r.norm()
		X = append(X, []float64{x0, x1, x2})
		w = append(w, wi)
		y = append(y, 1.5+float64(2*x0)-float64(1*x1)+float64(0.5*x2)+e)
	}
	return
}

func TestRefMultipleRegression(t *testing.T) {
	X, y, w := refCaseB()
	xs := []float64{5, 5, 5}

	o, err := OLS(X, y, true)
	if err != nil {
		t.Fatal(err)
	}
	refSlice(t, "OLS coef", o.Coef, ref(t, "B_ols_coef"), 1e-10)
	refSlice(t, "OLS SE", o.SE, ref(t, "B_ols_se"), 1e-10)
	refSlice(t, "OLS T", o.T, ref(t, "B_ols_t"), 1e-9)
	for j, p := range ref(t, "B_ols_p") {
		refRel(t, "OLS P", o.P[j], p, 1e-6)
	}
	refSlice(t, "OLS scalars", []float64{o.R2, o.AdjR2, o.ResidStd, o.SSE, o.SST}, ref(t, "B_ols_scal"), 1e-10)
	refNear(t, "OLS F", o.F, ref(t, "B_ols_f")[0], 1e-9)
	refRel(t, "OLS FP", o.FP, ref(t, "B_ols_f")[1], 1e-6)
	yh, lo, hi := o.PredictInterval(xs, 0.95)
	refSlice(t, "OLS PredictInterval", []float64{yh, lo, hi}, ref(t, "B_ols_pi"), 1e-8)
	if o.N != 60 || o.DF != 56 || !o.Intercept || o.NumFeatures() != 3 {
		t.Errorf("OLS N=%d DF=%d", o.N, o.DF)
	}
	jsonSafe(t, "OLSResult", o)

	o0, err := OLS(X, y, false)
	if err != nil {
		t.Fatal(err)
	}
	refSlice(t, "OLS0 coef", o0.Coef, ref(t, "B_ols0_coef"), 1e-10)
	refSlice(t, "OLS0 SE", o0.SE, ref(t, "B_ols0_se"), 1e-10)
	refSlice(t, "OLS0 scalars", []float64{o0.R2, o0.AdjR2, o0.ResidStd, o0.SSE, o0.SST}, ref(t, "B_ols0_scal"), 1e-10)
	if o0.DF != 57 {
		t.Errorf("OLS0 DF=%d", o0.DF)
	}

	ow, err := WLS(X, y, w, true)
	if err != nil {
		t.Fatal(err)
	}
	refSlice(t, "WLS coef", ow.Coef, ref(t, "B_wls_coef"), 1e-10)
	refSlice(t, "WLS SE", ow.SE, ref(t, "B_wls_se"), 1e-10)
	refSlice(t, "WLS scalars", []float64{ow.R2, ow.AdjR2, ow.ResidStd, ow.SSE, ow.SST}, ref(t, "B_wls_scal"), 1e-10)
	refNear(t, "WLS F", ow.F, ref(t, "B_wls_f")[0], 1e-9)
	yh, lo, hi = ow.PredictInterval(xs, 0.95)
	refSlice(t, "WLS PredictInterval", []float64{yh, lo, hi}, ref(t, "B_wls_pi"), 1e-8)

	// Rows with NaN/Inf in X, y or w (or w <= 0) are dropped: adding such
	// rows must not change anything.
	X2 := append([][]float64{{math.NaN(), 1, 1}, {1, 1, 1}, {2, math.Inf(1), 3}, {1, 2, 3}, {3, 2, 1}}, X...)
	y2 := append([]float64{1, math.NaN(), 3, 4, 5}, y...)
	w2 := append([]float64{1, 1, 1, 0, -2}, w...)
	ow2, err := WLS(X2, y2, w2, true)
	if err != nil {
		t.Fatal(err)
	}
	refSlice(t, "WLS drop rows", ow2.Coef, ow.Coef, 1e-12)
	if ow2.N != 60 {
		t.Errorf("WLS kept %d rows, want 60", ow2.N)
	}
	// Scaling all weights leaves coefficients and standard errors unchanged.
	w3 := make([]float64, len(w))
	for i := range w {
		w3[i] = 1000 * w[i]
	}
	ow3, _ := WLS(X, y, w3, true)
	refSlice(t, "WLS weight scale coef", ow3.Coef, ow.Coef, 1e-10)
	refSlice(t, "WLS weight scale SE", ow3.SE, ow.SE, 1e-10)
}

func TestRefRidge(t *testing.T) {
	X, y, w := refCaseB()
	xs := []float64{5, 5, 5}
	check := func(key string, m *Model, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		refSlice(t, key+" coef", m.Coef, ref(t, key+"_coef"), 1e-10)
		raw, icpt := m.RawCoef()
		refSlice(t, key+" raw", append([]float64{icpt}, raw...), ref(t, key+"_raw"), 1e-10)
		refSlice(t, key+" scalars", []float64{m.Intercept, m.R2, m.DF, m.ResidStd}, ref(t, key+"_scal"), 1e-10)
		p, lo, hi := m.PredictInterval(xs, 0.95)
		refSlice(t, key+" PredictInterval", []float64{p, lo, hi}, ref(t, key+"_pi"), 1e-8)
		refNear(t, key+" Predict", m.Predict(xs), ref(t, key+"_pi")[0], 1e-10)
		refNear(t, key+" PredictMap", m.PredictMap(map[string]float64{"x0": 5, "x1": 5, "x2": 5, "zz": 9}), ref(t, key+"_pi")[0], 1e-10)
		k := len(m.Coef) + 1
		diag := make([]float64, k)
		for a := 0; a < k; a++ {
			diag[a] = m.Cov[a*k+a]
		}
		refSlice(t, key+" Cov diag", diag, ref(t, key+"_covdiag"), 1e-9)
		jsonSafe(t, key, m)
		b, _ := json.Marshal(m)
		m2, err := ParseModel(b)
		if err != nil {
			t.Fatalf("%s: ParseModel: %v", key, err)
		}
		refNear(t, key+" JSON round trip", m2.Predict(xs), m.Predict(xs), 1e-15)
	}
	m, err := Ridge(X, y, nil, 0.5)
	check("B_ridge", m, err)
	m, err = Ridge(X, y, nil, 0)
	check("B_ridge0", m, err)
	m, err = RidgeWeighted(X, y, w, nil, 0.1)
	check("B_ridgew", m, err)

	// Model interval at lambda = 0 equals the OLS prediction interval.
	m0, _ := Ridge(X, y, nil, 0)
	o, _ := OLS(X, y, true)
	p, lo, hi := m0.PredictInterval(xs, 0.8)
	op, olo, ohi := o.PredictInterval(xs, 0.8)
	refSlice(t, "Ridge0 vs OLS interval", []float64{p, lo, hi}, []float64{op, olo, ohi}, 1e-9)
	// A missing feature is imputed with its training mean.
	nanRow := []float64{5, math.NaN(), 5}
	refNear(t, "impute", m0.Predict(nanRow), m0.Predict([]float64{5, m0.Mean[1], 5}), 1e-12)
	if !math.IsNaN(m0.Predict([]float64{1, 2})) {
		t.Error("wrong row length must give NaN")
	}

	// RidgeCV: no leakage. Recompute every fold by hand, fitting on the
	// training rows only, and compare the reported MSE.
	lambdas := []float64{0, 0.01, 0.3, 3}
	folds := BlockFold(len(X), 5)
	_, cv, err := RidgeCV(X, y, nil, lambdas, folds)
	if err != nil {
		t.Fatal(err)
	}
	for li, lam := range lambdas {
		sse, cnt := 0.0, 0
		for _, test := range folds {
			in := map[int]bool{}
			for _, i := range test {
				in[i] = true
			}
			var trX [][]float64
			var trY []float64
			for i := range X {
				if !in[i] {
					trX = append(trX, X[i])
					trY = append(trY, y[i])
				}
			}
			fm, err := Ridge(trX, trY, nil, lam)
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range test {
				sse += (y[i] - fm.Predict(X[i])) * (y[i] - fm.Predict(X[i]))
				cnt++
			}
		}
		refNear(t, "RidgeCV MSE", cv.MSE[li], sse/float64(cnt), 1e-12)
	}
	// Folds partition the rows.
	for _, fs := range [][][]int{KFold(17, 4), BlockFold(17, 4), BlockFold(5, 9), KFold(2, 2)} {
		seen := map[int]int{}
		total := 0
		for _, f := range fs {
			for _, i := range f {
				seen[i]++
				total++
			}
			if len(f) == 0 {
				t.Errorf("empty fold in %v", fs)
			}
		}
		if len(seen) != total {
			t.Errorf("folds overlap: %v", fs)
		}
	}
	// CrossValidate hands the fit only training indices.
	leak := false
	_, err = CrossValidate(20, BlockFold(20, 4), func(train []int) (func(int) float64, error) {
		tr := map[int]bool{}
		for _, i := range train {
			tr[i] = true
		}
		return func(i int) float64 {
			if tr[i] {
				leak = true
			}
			return 0
		}, nil
	}, func(i int, pred float64) float64 { return 1 })
	if err != nil || leak {
		t.Errorf("CrossValidate: err=%v leak=%v", err, leak)
	}
}

func refCaseC() (X [][]float64, y, w []float64) {
	r := &refRand{3}
	const n = 80
	for i := 0; i < n; i++ {
		x0 := float64(r.u()*4) - 2
		x1 := float64(r.u()*4) - 2
		e := r.norm()
		wi := 0.5 + r.u()
		X = append(X, []float64{x0, x1})
		v := 0.5 + float64(1.2*x0) - float64(0.8*x1) + float64(1.5*e)
		if v > 0 {
			y = append(y, 1)
		} else {
			y = append(y, 0)
		}
		w = append(w, wi)
	}
	return
}

func TestRefLogistic(t *testing.T) {
	X, y, w := refCaseC()
	if got := Sum(y); got != ref(t, "C_scal")[2] {
		t.Fatalf("data generation differs from the reference: %v events, want %v", got, ref(t, "C_scal")[2])
	}
	m, err := Logistic(X, y, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Converged || m.Kind != KindLogistic || m.N != 80 {
		t.Errorf("Logistic: converged=%v kind=%q n=%d", m.Converged, m.Kind, m.N)
	}
	raw, icpt := m.RawCoef()
	refSlice(t, "Logistic raw coef", append([]float64{icpt}, raw...), ref(t, "C_raw"), 1e-8)
	refNear(t, "Logistic LogLoss", m.LogLoss, ref(t, "C_scal")[0], 1e-10)
	refNear(t, "Logistic McFadden R2", m.R2, ref(t, "C_scal")[1], 1e-9)
	// Standard errors of the slopes in raw units from Cov (standardised units).
	k := len(m.Coef) + 1
	for j := range m.Coef {
		se := math.Sqrt(m.Cov[(j+1)*k+j+1]) / m.Scale[j]
		refNear(t, "Logistic slope SE", se, ref(t, "C_se")[j+1], 1e-7)
	}
	// Confidence interval of the probability at a point (invariant under the
	// internal standardisation, so it checks the whole covariance matrix).
	p, lo, hi := m.PredictInterval([]float64{0.5, -1}, 0.95)
	refSlice(t, "Logistic CI", []float64{p, lo, hi}, ref(t, "C_ci"), 1e-7)
	refNear(t, "Logistic Predict", m.Predict([]float64{0.5, -1}), ref(t, "C_ci")[0], 1e-8)
	jsonSafe(t, "logistic model", m)

	mp, err := Logistic(X, y, nil, &LogisticOptions{Lambda: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	refSlice(t, "Logistic penalised", append([]float64{mp.Intercept}, mp.Coef...), ref(t, "C_pen"), 1e-8)
	refNear(t, "Logistic penalised LogLoss", mp.LogLoss, ref(t, "C_pen_ll")[0], 1e-9)

	mw, err := Logistic(X, y, nil, &LogisticOptions{Weights: w})
	if err != nil {
		t.Fatal(err)
	}
	raw, icpt = mw.RawCoef()
	refSlice(t, "Logistic weighted", append([]float64{icpt}, raw...), ref(t, "C_w_raw"), 1e-8)
	for j := range mw.Coef {
		se := math.Sqrt(mw.Cov[(j+1)*k+j+1]) / mw.Scale[j]
		refNear(t, "Logistic weighted SE", se, ref(t, "C_w_se")[j+1], 1e-7)
	}
	// Published example (Wikipedia "Logistic regression", hours studied vs.
	// passing): intercept -4.0777, slope 1.5046, SE 1.7610 / 0.6287.
	hours := []float64{0.50, 0.75, 1.00, 1.25, 1.50, 1.75, 1.75, 2.00, 2.25, 2.50, 2.75, 3.00, 3.25, 3.50, 4.00, 4.25, 4.50, 4.75, 5.00, 5.50}
	pass := []float64{0, 0, 0, 0, 0, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 1, 1, 1, 1, 1}
	hx := make([][]float64, len(hours))
	for i, h := range hours {
		hx[i] = []float64{h}
	}
	hm, err := Logistic(hx, pass, []string{"hours"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	hr, hi0 := hm.RawCoef()
	refNear(t, "exam intercept", hi0, -4.0777, 2e-5)
	refNear(t, "exam slope", hr[0], 1.5046, 5e-5)
	refNear(t, "exam slope SE", math.Sqrt(hm.Cov[3])/hm.Scale[0], 0.6287, 2e-4)
	refNear(t, "exam P(pass | 2h)", hm.Predict([]float64{2}), 0.2557, 2e-4)
}

func TestRefTheilSen(t *testing.T) {
	r := &refRand{4}
	const n = 31
	x, y := make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		xi := r.u() * 50
		e := r.norm()
		x[i] = xi
		y[i] = 3 - float64(0.7*xi) + e
	}
	y[3] += 80
	y[11] -= 60
	y[20] += 45
	y[27] = 500
	l, err := TheilSen(x, y)
	if err != nil {
		t.Fatal(err)
	}
	want := ref(t, "D_theilsen")
	refSlice(t, "TheilSen", []float64{l.Slope, l.Intercept, l.ResidStd}, want, 1e-12)
	// The outliers must not have moved the line far from the truth, while OLS is thrown off.
	if math.Abs(l.Slope+0.7) > 0.05 || math.Abs(l.Intercept-3) > 1.5 {
		t.Errorf("TheilSen not robust: %+v", l)
	}
	// Sampled branch (n > 700): still close to the exact all-pairs answer.
	const big = 1500
	bx, by := make([]float64, big), make([]float64, big)
	for i := range bx {
		bx[i] = r.u() * 100
		by[i] = 2 + 0.25*bx[i] + r.norm()
		if i%10 == 0 {
			by[i] += 500
		}
	}
	bl, err := TheilSen(bx, by)
	if err != nil {
		t.Fatal(err)
	}
	var sl []float64
	for i := 0; i < big; i++ {
		for j := i + 1; j < big; j++ {
			if bx[i] != bx[j] {
				sl = append(sl, (by[j]-by[i])/(bx[j]-bx[i]))
			}
		}
	}
	exact := naiveQuantile7(sl, 0.5)
	if math.Abs(bl.Slope-exact) > 0.004 {
		t.Errorf("TheilSen sampled slope %v, exact %v", bl.Slope, exact)
	}
}

// -------------------------------------------------------------- circular

func TestRefCircular(t *testing.T) {
	// Naive reference via explicit unit vectors.
	naive := func(deg, w []float64) (mean, r float64) {
		var s, c, sw float64
		for i, d := range deg {
			wi := 1.0
			if w != nil {
				wi = w[i]
			}
			s += wi * math.Sin(d*math.Pi/180)
			c += wi * math.Cos(d*math.Pi/180)
			sw += wi
		}
		mean = math.Atan2(s, c) * 180 / math.Pi
		if mean < 0 {
			mean += 360
		}
		return mean, math.Sqrt(s*s+c*c) / sw
	}
	near360 := func(name string, got, want float64) {
		t.Helper()
		d := math.Abs(got - want)
		if d > 180 {
			d = 360 - d
		}
		if d > 1e-9 || got < 0 || got >= 360 || math.IsNaN(got) {
			t.Errorf("%s: got %.12g, want %.12g (must be in [0,360))", name, got, want)
		}
	}
	cases := []struct {
		deg  []float64
		want float64
	}{
		{[]float64{359, 1}, 0},
		{[]float64{1, 359}, 0},
		{[]float64{350, 10}, 0},
		{[]float64{355, 5, 15}, 5},
		{[]float64{345, 355, 5}, 355},
		{[]float64{180, 180}, 180},
		{[]float64{-10, 370}, 0},
		{[]float64{90, 180}, 135},
		{[]float64{270, 0}, 315},
		{[]float64{0, 0, 90}, 26.565051177077986},
		{[]float64{720 + 45, -315}, 45},
		{[]float64{359.9999999, 0.0000001}, 0},
		{[]float64{359.9999999, 0.00000005}, 359.999999975},
	}
	for _, c := range cases {
		m, r, ok := CircularMeanDeg(c.deg)
		if !ok {
			t.Errorf("CircularMeanDeg(%v) not ok", c.deg)
			continue
		}
		near360("CircularMeanDeg", m, c.want)
		nm, nr := naive(c.deg, nil)
		near360("CircularMeanDeg vs naive", m, nm)
		refNear(t, "R", r, nr, 1e-12)
		refNear(t, "ResultantLength", ResultantLength(c.deg), nr, 1e-12)
		refNear(t, "CircularVariance", CircularVariance(c.deg), 1-nr, 1e-12)
		if nr < 1-1e-12 {
			refNear(t, "CircularStdDeg", CircularStdDeg(c.deg), math.Sqrt(-2*math.Log(nr))*180/math.Pi, 1e-9)
		}
	}
	_, r, _ := CircularMeanDeg([]float64{359, 1})
	refNear(t, "R of 359/1", r, math.Cos(math.Pi/180), 1e-14)
	// The arithmetic mean of 359 and 1 would be 180: make sure that is not what we get.
	if m, _, _ := CircularMeanDeg([]float64{359, 1}); math.Abs(m-180) < 90 {
		t.Errorf("circular mean of 359/1 = %v", m)
	}
	// Undefined directions.
	for _, deg := range [][]float64{{0, 180}, {90, 270}, {0, 120, 240}, {10, 100, 190, 280}, nil, {math.NaN()}} {
		m, _, ok := CircularMeanDeg(deg)
		if ok || !math.IsNaN(m) {
			t.Errorf("CircularMeanDeg(%v) = %v, ok=%v; want undefined", deg, m, ok)
		}
	}
	st, ok := CircularSummary([]float64{0, 180}, nil)
	if !ok || st.Defined || st.N != 2 || st.R > 1e-12 || st.MeanDeg != 0 {
		t.Errorf("CircularSummary(0,180) = %+v", st)
	}
	jsonSafe(t, "CircularStats undefined", st)
	// NaN / Inf are skipped.
	m, _, ok := CircularMeanDeg([]float64{math.NaN(), 350, math.Inf(1), 10})
	if !ok {
		t.Fatal("NaN handling")
	}
	near360("NaN skipped", m, 0)
	// Weighted: 3 x north, 1 x east -> atan2(1, 3).
	ws, ok := CircularSummary([]float64{0, 90, 45, 200}, []float64{3, 1, 0, math.NaN()})
	if !ok || ws.N != 2 {
		t.Fatalf("weighted summary %+v", ws)
	}
	near360("weighted mean", ws.MeanDeg, math.Atan2(1, 3)*180/math.Pi)
	refNear(t, "weighted R", ws.R, math.Sqrt(10)/4, 1e-14)
	if _, ok := CircularSummary([]float64{1, 2}, []float64{1}); ok {
		t.Error("length mismatch must fail")
	}

	// Angle differences: a-b wrapped into (-180, 180].
	diffs := []struct{ a, b, want float64 }{
		{1, 359, 2}, {359, 1, -2}, {10, 350, 20}, {350, 10, -20}, {180, 0, 180}, {0, 180, 180},
		{90, 270, 180}, {270, 90, 180}, {725, -360, 5}, {0, 0, 0}, {45, 45 + 3600, 0}, {0, 179.5, -179.5}, {0, 180.5, 179.5},
	}
	for _, d := range diffs {
		got := AngleDiffDeg(d.a, d.b)
		if math.Abs(got-d.want) > 1e-10 || got <= -180 || got > 180 {
			t.Errorf("AngleDiffDeg(%v, %v) = %v, want %v", d.a, d.b, got, d.want)
		}
		refNear(t, "AbsAngleDiffDeg", AbsAngleDiffDeg(d.a, d.b), math.Abs(d.want), 1e-10)
	}
	norms := []struct{ a, want float64 }{{-1, 359}, {360, 0}, {720.5, 0.5}, {-360, 0}, {-1e-20, 0}, {359.999, 359.999}, {-720, 0}, {1e6 + 0.25, 280.25}}
	for _, c := range norms {
		got := NormalizeDeg(c.a)
		if math.Abs(got-c.want) > 1e-9 || got < 0 || got >= 360 || math.Signbit(got) {
			t.Errorf("NormalizeDeg(%v) = %v, want %v", c.a, got, c.want)
		}
	}
	if !math.IsNaN(NormalizeDeg(math.Inf(1))) || !math.IsNaN(AngleDiffDeg(math.NaN(), 1)) {
		t.Error("non-finite angles must give NaN")
	}
}

func TestRefWind(t *testing.T) {
	// Meteorological convention: the direction the wind blows FROM.
	// A westerly (from 270) moves air towards the east: u > 0.
	cases := []struct{ speed, dir, u, v float64 }{
		{10, 270, 10, 0},  // westerly
		{10, 90, -10, 0},  // easterly
		{10, 0, 0, -10},   // northerly: air moves south
		{10, 360, 0, -10}, // same
		{10, 180, 0, 10},  // southerly: air moves north
		{10, 225, 10 * math.Sqrt2 / 2, 10 * math.Sqrt2 / 2}, // south-westerly
		{10, 45, -10 * math.Sqrt2 / 2, -10 * math.Sqrt2 / 2},
		{0, 123, 0, 0},
	}
	for _, c := range cases {
		u, v := WindToUV(c.speed, c.dir)
		if math.Abs(u-c.u) > 1e-12 || math.Abs(v-c.v) > 1e-12 {
			t.Errorf("WindToUV(%v, %v) = (%v, %v), want (%v, %v)", c.speed, c.dir, u, v, c.u, c.v)
		}
		s, d := UVToWind(c.u, c.v)
		refNear(t, "UVToWind speed", s, c.speed, 1e-12)
		if c.speed > 0 && AbsAngleDiffDeg(d, c.dir) > 1e-9 {
			t.Errorf("UVToWind(%v, %v) dir = %v, want %v", c.u, c.v, d, c.dir)
		}
		if d < 0 || d >= 360 {
			t.Errorf("UVToWind dir %v out of [0,360)", d)
		}
	}
	if s, d := UVToWind(0, 0); s != 0 || d != 0 {
		t.Errorf("calm: %v %v", s, d)
	}
	// Round trip.
	r := &refRand{31}
	for i := 0; i < 500; i++ {
		sp, dir := 0.1+r.u()*60, r.u()*360
		u, v := WindToUV(sp, dir)
		s2, d2 := UVToWind(u, v)
		if math.Abs(s2-sp) > 1e-10 || AbsAngleDiffDeg(d2, dir) > 1e-9 {
			t.Fatalf("round trip (%v, %v) -> (%v, %v)", sp, dir, s2, d2)
		}
	}
	// Vector mean across north: 10 kt from 350 and from 10 -> from 0 at 10*cos(10 deg).
	wm, ok := VectorMeanWind([]float64{10, 10, math.NaN(), 5}, []float64{350, 10, 90, math.NaN()})
	if !ok || wm.N != 2 {
		t.Fatalf("VectorMeanWind %+v", wm)
	}
	if AbsAngleDiffDeg(wm.DirDeg, 0) > 1e-9 || wm.DirDeg < 0 || wm.DirDeg >= 360 {
		t.Errorf("mean dir %v", wm.DirDeg)
	}
	refNear(t, "mean speed", wm.Speed, 10*math.Cos(10*math.Pi/180), 1e-12)
	refNear(t, "scalar speed", wm.ScalarSpeed, 10, 1e-12)
	refNear(t, "steadiness", wm.Steadiness, math.Cos(10*math.Pi/180), 1e-12)
	refNear(t, "mean v", wm.V, -10*math.Cos(10*math.Pi/180), 1e-12)
	// Opposing winds cancel.
	wm, _ = VectorMeanWind([]float64{8, 8}, []float64{90, 270})
	if wm.Speed > 1e-12 || wm.Steadiness > 1e-12 || wm.ScalarSpeed != 8 {
		t.Errorf("opposing winds: %+v", wm)
	}
	jsonSafe(t, "WindMean", wm)
	// Unequal speeds: 20 from the west, 10 from the south -> u=10, v=5.
	wm, _ = VectorMeanWind([]float64{20, 10}, []float64{270, 180})
	refNear(t, "u", wm.U, 10, 1e-12)
	refNear(t, "v", wm.V, 5, 1e-12)
	refNear(t, "dir", wm.DirDeg, 180+math.Atan2(10, 5)*180/math.Pi, 1e-12)
	if _, ok := VectorMeanWind(nil, nil); ok {
		t.Error("empty wind mean must not be ok")
	}
}

// ---------------------------------------------------------- Holt-Winters

func refCaseE() []float64 {
	r := &refRand{5}
	seas := []float64{3, -1, -2, 0}
	y := make([]float64, 28)
	for i := range y {
		y[i] = 10 + float64(0.3*float64(i)) + seas[i%4] + float64(0.5*r.norm())
	}
	y[9], y[10], y[19] = math.NaN(), math.NaN(), math.NaN()
	return y
}

func TestRefHoltWintersRecursion(t *testing.T) {
	y := refCaseE()
	for _, c := range []struct {
		key string
		p   HWParams
	}{
		{"E_hw", HWParams{Alpha: 0.3, Beta: 0.2, Gamma: 0.4, Phi: 1}},
		{"E_hwd", HWParams{Alpha: 0.3, Beta: 0.2, Gamma: 0.4, Phi: 0.9}},
		{"E_hwnt", HWParams{Alpha: 0.5, Beta: 0, Gamma: 0.25, Phi: 1}},
	} {
		m := &HoltWinters{HWParams: c.p, Period: 4, HasTrend: true, Level: 9.7, Trend: 0.3, Season: []float64{3, -1, -2, 0}}
		fitted := make([]float64, len(y))
		sse := 0.0
		for i, v := range y {
			if m.Phase() != i%4 {
				t.Fatalf("%s: Phase() = %d at sample %d", c.key, m.Phase(), i)
			}
			if pn := m.PredictNext(); true {
				pred, resid := m.Update(v)
				if pred != pn {
					t.Fatalf("%s: Update pred %v != PredictNext %v", c.key, pred, pn)
				}
				fitted[i] = pred
				if math.IsNaN(v) {
					if !math.IsNaN(resid) {
						t.Fatalf("%s: residual of a missing sample must be NaN", c.key)
					}
				} else {
					refNear(t, c.key+" resid", resid, v-pred, 1e-13)
					sse += resid * resid
				}
			}
		}
		refSlice(t, c.key+" fitted", fitted, ref(t, c.key+"_fitted"), 1e-11)
		refSlice(t, c.key+" forecast", m.Forecast(10), ref(t, c.key+"_fc"), 1e-11)
		st := ref(t, c.key+"_state")
		got := append([]float64{m.Level, m.Trend}, m.Season...)
		got = append(got, sse, float64(m.NObs))
		refSlice(t, c.key+" state", got, st, 1e-11)
		if m.N != len(y) {
			t.Errorf("%s: N = %d", c.key, m.N)
		}
		// Forecast standard deviation grows with the horizon exactly like the
		// impulse response of the recursion says.
		m.Sigma = 2.5
		sd := m.ForecastStd(10)
		wantRatio := ref(t, c.key+"_sdratio")
		for h := range sd {
			refNear(t, c.key+" ForecastStd", sd[h]/2.5, wantRatio[h], 1e-10)
			if h > 0 && sd[h] < sd[h-1] {
				t.Errorf("%s: forecast std shrinks at h=%d", c.key, h+1)
			}
		}
		mean, lo, hi := m.ForecastInterval(10, 0.9)
		z := 1.6448536269514722
		for h := range mean {
			refNear(t, c.key+" interval lo", lo[h], mean[h]-z*sd[h], 1e-10)
			refNear(t, c.key+" interval hi", hi[h], mean[h]+z*sd[h], 1e-10)
		}
		jsonSafe(t, c.key, m)
	}
}

func TestRefHoltWintersFit(t *testing.T) {
	// 1. An exactly periodic series with a linear trend is reproduced and
	// extrapolated without error, whatever the phase the series ends in
	// (length not a multiple of the period: catches off-by-one in the
	// seasonal index of the forecasts).
	for _, period := range []int{2, 7, 24} {
		for _, extra := range []int{0, 1, period - 1} {
			n := 5*period + extra
			truth := func(i int) float64 {
				return 50 + 0.25*float64(i) + 10*math.Sin(2*math.Pi*float64(i%period)/float64(period)) + float64((i%period)%3)
			}
			y := make([]float64, n)
			for i := range y {
				y[i] = truth(i)
			}
			fit, err := FitHoltWinters(y, period, nil)
			if err != nil {
				t.Fatalf("period %d: %v", period, err)
			}
			fc := fit.Model.Forecast(2*period + 3)
			for h, v := range fc {
				if math.Abs(v-truth(n+h)) > 1e-6*math.Abs(truth(n+h)) {
					t.Errorf("period %d extra %d: forecast h=%d is %v, want %v", period, extra, h+1, v, truth(n+h))
					break
				}
			}
			if fit.Model.Phase() != n%period || fit.Model.N != n {
				t.Errorf("period %d extra %d: phase %d N %d", period, extra, fit.Model.Phase(), fit.Model.N)
			}
			jsonSafe(t, "HoltWinters", fit.Model)
		}
	}

	// 2. The optimiser must not be worse than any point of a fixed-parameter grid.
	r := &refRand{41}
	const period, n = 12, 12 * 9
	y := make([]float64, n)
	lev := 20.0
	for i := range y {
		lev += 0.05 + 0.2*r.norm()
		y[i] = lev + 4*math.Cos(2*math.Pi*float64(i)/period) + 0.8*r.norm()
		if i%17 == 5 {
			y[i] = math.NaN()
		}
	}
	fit, err := FitHoltWinters(y, period, nil)
	if err != nil {
		t.Fatal(err)
	}
	grid := []float64{0.02, 0.1, 0.25, 0.45, 0.7, 0.95}
	bestGrid := math.Inf(1)
	for _, a := range grid {
		for _, b := range grid {
			for _, g := range grid {
				f, err := FitHoltWinters(y, period, &HWOptions{Params: &HWParams{Alpha: a, Beta: b, Gamma: g, Phi: 1}})
				if err != nil {
					t.Fatal(err)
				}
				bestGrid = math.Min(bestGrid, f.SSE)
			}
		}
	}
	if fit.SSE > bestGrid*1.0005 {
		t.Errorf("optimised SSE %v worse than grid optimum %v (params %+v)", fit.SSE, bestGrid, fit.Model.HWParams)
	}
	// Residual bookkeeping: SSE is the sum of the squared finite residuals,
	// Residuals = y - Fitted, missing samples have NaN residuals.
	sse, nobs := 0.0, 0
	for i, v := range y {
		if math.IsNaN(v) {
			if !math.IsNaN(fit.Residuals[i]) || math.IsNaN(fit.Fitted[i]) {
				t.Fatalf("missing sample %d: fitted %v resid %v", i, fit.Fitted[i], fit.Residuals[i])
			}
			continue
		}
		refNear(t, "residual", fit.Residuals[i], v-fit.Fitted[i], 1e-12)
		sse += fit.Residuals[i] * fit.Residuals[i]
		nobs++
	}
	refNear(t, "SSE", fit.SSE, sse, 1e-10)
	if fit.NObs != nobs || fit.Model.NObs != nobs {
		t.Errorf("NObs %d / %d, want %d", fit.NObs, fit.Model.NObs, nobs)
	}
	// Sigma uses the residual degrees of freedom (3 fitted parameters).
	refNear(t, "Sigma", fit.Model.Sigma, math.Sqrt(sse/float64(nobs-3)), 1e-10)

	// 3. No look-ahead in the one-step fitted values: with fixed parameters,
	// Fitted[i] may only depend on y[:i] (beyond the initialisation window
	// of 8 cycles).
	par := &HWOptions{Params: &HWParams{Alpha: 0.3, Beta: 0.1, Gamma: 0.2, Phi: 1}}
	base, err := FitHoltWinters(y, period, par)
	if err != nil {
		t.Fatal(err)
	}
	const cut = 8*period + 3
	y2 := append([]float64(nil), y...)
	for i := cut; i < n; i++ {
		y2[i] += 1000
	}
	mod, _ := FitHoltWinters(y2, period, par)
	for i := 0; i <= cut; i++ {
		if base.Fitted[i] != mod.Fitted[i] {
			t.Fatalf("Fitted[%d] depends on later samples", i)
		}
	}
	// The fitted value at i is the forecast of the model that has seen y[:i].
	hw := &HoltWinters{}
	b, _ := json.Marshal(base.Model)
	if err := json.Unmarshal(b, hw); err != nil {
		t.Fatal(err)
	}
	refNear(t, "JSON round trip forecast", hw.Forecast(5)[4], base.Model.Forecast(5)[4], 1e-12)

	// 4. Forecast interval widens with the horizon and matches Sigma at h=1.
	sd := fit.Model.ForecastStd(3 * period)
	refNear(t, "sd[0]", sd[0], fit.Model.Sigma, 1e-12)
	for h := 1; h < len(sd); h++ {
		if !(sd[h] >= sd[h-1]) {
			t.Fatalf("forecast std not monotone at h=%d", h+1)
		}
	}
	if !(sd[len(sd)-1] > 1.2*sd[0]) {
		t.Errorf("forecast std does not grow: %v .. %v", sd[0], sd[len(sd)-1])
	}

	// 5. A gap of several cycles in the middle must not derail the fit.
	y3 := append([]float64(nil), y...)
	for i := 3 * period; i < 5*period; i++ {
		y3[i] = math.NaN()
	}
	gap, err := FitHoltWinters(y3, period, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range gap.Model.Forecast(period) {
		if math.IsNaN(v) || math.Abs(v-lev) > 15 {
			t.Fatalf("forecast after gap: %v (level %v)", v, lev)
		}
	}
	jsonSafe(t, "HoltWinters gap", gap.Model)

	// 5b. Gaps inside the initialisation window must not bias the starting
	// seasonal indices: a series that the model represents exactly is still
	// reproduced and extrapolated exactly when a quarter of every early
	// cycle (always the same phases, e.g. the nights) is missing, or when
	// 30 % of all samples are missing at random.
	for _, period := range []int{24, 168} {
		const nc = 6
		truth := func(i int) float64 {
			ph := float64(i % period)
			return 200 + 0.05*float64(i) + 80*math.Sin(2*math.Pi*ph/float64(period)) + 30*math.Cos(4*math.Pi*ph/float64(period))
		}
		night := make([]float64, nc*period)
		random := make([]float64, nc*period)
		rg := &refRand{uint32(period)}
		for i := range night {
			night[i], random[i] = truth(i), truth(i)
			if i < 3*period && i%period < period/4 {
				night[i] = math.NaN()
			}
			if rg.u() < 0.3 {
				random[i] = math.NaN()
			}
		}
		for name, yy := range map[string][]float64{"nights": night, "random": random} {
			gf, err := FitHoltWinters(yy, period, nil)
			if err != nil {
				t.Fatal(err)
			}
			for h, v := range gf.Model.Forecast(period) {
				if math.Abs(v-truth(nc*period+h)) > 1e-6 {
					t.Errorf("period %d, %s missing: forecast h=%d off by %.3g (sigma %.3g)", period, name, h+1, v-truth(nc*period+h), gf.Model.Sigma)
					break
				}
			}
			for i, v := range gf.Fitted {
				if math.Abs(v-truth(i)) > 1e-6 {
					t.Errorf("period %d, %s missing: fitted[%d] off by %.3g", period, name, i, v-truth(i))
					break
				}
			}
		}
	}

	// 6. Constant series: zero error, constant forecast, no NaN.
	c := make([]float64, 60)
	for i := range c {
		c[i] = 7
	}
	for _, p := range []int{0, 12} {
		for _, o := range []*HWOptions{nil, {NoTrend: true}, {Damped: true}} {
			cf, err := FitHoltWinters(c, p, o)
			if err != nil {
				t.Fatalf("constant series: %v", err)
			}
			for _, v := range cf.Model.Forecast(30) {
				refNear(t, "constant forecast", v, 7, 1e-9)
			}
			if cf.Model.Sigma > 1e-9 || math.IsNaN(cf.Model.Sigma) {
				t.Errorf("constant sigma %v", cf.Model.Sigma)
			}
			jsonSafe(t, "HoltWinters constant", cf.Model)
		}
	}
	// 7. Simple exponential smoothing with fixed alpha is the textbook EWMA
	// forecast: f[t+1] = f[t] + alpha*(y[t]-f[t]).
	ses, err := FitHolt(y, &HWOptions{NoTrend: true, Params: &HWParams{Alpha: 0.35}})
	if err != nil {
		t.Fatal(err)
	}
	f := ses.Fitted[0]
	for i, v := range y {
		refNear(t, "SES fitted", ses.Fitted[i], f, 1e-12)
		if !math.IsNaN(v) {
			f += 0.35 * (v - f)
		}
	}
	for _, v := range ses.Model.Forecast(4) {
		refNear(t, "SES flat forecast", v, f, 1e-12)
	}
	// SES forecast variance: sigma^2 * (1 + (h-1)*alpha^2).
	ses.Model.Sigma = 1
	for h, s := range ses.Model.ForecastStd(6) {
		refNear(t, "SES std", s, math.Sqrt(1+float64(h)*0.35*0.35), 1e-12)
	}
	// Holt (undamped) forecast variance, Hyndman et al. (2008) table 6.1:
	// sigma^2 * (1 + (h-1)*(a^2 + a*b*h + b^2*h*(2h-1)/6)), b = alpha*beta.
	holt := &HoltWinters{HWParams: HWParams{Alpha: 0.4, Beta: 0.5, Phi: 1}, HasTrend: true, Sigma: 1}
	for h1, s := range holt.ForecastStd(8) {
		h := float64(h1 + 1)
		a, b := 0.4, 0.4*0.5
		refNear(t, "Holt std", s, math.Sqrt(1+(h-1)*(a*a+a*b*h+b*b*h*(2*h-1)/6)), 1e-12)
	}
}

// ---------------------------------------------------------------- Kalman

func refCaseF() []float64 {
	r := &refRand{6}
	y := make([]float64, 30)
	lev := 5.0
	for i := range y {
		lev += float64(0.6 * r.norm())
		y[i] = lev + float64(1.2*r.norm())
	}
	y[0], y[7], y[8], y[20] = math.NaN(), math.NaN(), math.NaN(), math.NaN()
	return y
}

func TestRefKalman(t *testing.T) {
	y := refCaseF()
	// The RTS smoother must equal the minimiser of the penalised least
	// squares problem (posterior mode of the same model), solved densely.
	sm := KalmanSmoothLevel(y, 0.3, 1.5)
	if len(sm) != len(y) || !math.IsNaN(sm[0]) {
		t.Fatalf("KalmanSmoothLevel: len %d, sm[0]=%v", len(sm), sm[0])
	}
	refSlice(t, "KalmanSmoothLevel", sm[1:], ref(t, "F_level"), 1e-10)

	lv, tr := KalmanSmoothTrend(y, 0.05, 0.01, 1)
	if !math.IsNaN(lv[0]) || !math.IsNaN(tr[0]) {
		t.Fatal("leading NaN must stay NaN")
	}
	refSlice(t, "KalmanSmoothTrend level", lv[1:], ref(t, "F_trend_level"), 1e-6)
	refSlice(t, "KalmanSmoothTrend slope", tr[1:], ref(t, "F_trend_slope"), 1e-6)

	// The last smoothed value is the filtered value.
	kf := NewLocalLevel(0.3, 1.5)
	var last float64
	for _, v := range y {
		last = kf.Update(v)
	}
	refNear(t, "smoother end = filter end", sm[len(sm)-1], last, 1e-12)
	jsonSafe(t, "LocalLevel", kf)

	// Steady state of the local level filter: P solves P^2 + qP - qr = 0
	// (posterior variance), gain = (P+q)/(P+q+r).
	q, r := 0.3, 1.5
	k := NewLocalLevel(q, r)
	for i := 0; i < 500; i++ {
		k.Update(float64(i % 3))
	}
	prior := (q + math.Sqrt(q*q+4*q*r)) / 2 // predicted variance
	refNear(t, "steady-state P", k.P, prior*r/(prior+r), 1e-12)

	// With zero process noise the local trend smoother is the least-squares
	// line through the data (diffuse prior), and the filtered end state is
	// the fitted line at the last sample.
	n := 40
	xs, ys := make([]float64, n), make([]float64, n)
	rr := &refRand{61}
	for i := range xs {
		xs[i] = float64(i)
		ys[i] = 3 + 0.7*float64(i) + rr.norm()
	}
	fit, _ := LinearRegression(xs, ys)
	l0, t0 := KalmanSmoothTrend(ys, 0, 0, 1)
	for i := range l0 {
		if math.Abs(l0[i]-fit.Predict(xs[i])) > 1e-4 || math.Abs(t0[i]-fit.Slope) > 1e-5 {
			t.Fatalf("zero-noise smoother at %d: level %v slope %v, line %v slope %v", i, l0[i], t0[i], fit.Predict(xs[i]), fit.Slope)
		}
	}
	lt := NewLocalTrend(0, 0, 1)
	for _, v := range ys {
		lt.Update(v)
	}
	if math.Abs(lt.Level-fit.Predict(xs[n-1])) > 1e-4 || math.Abs(lt.Trend-fit.Slope) > 1e-5 {
		t.Errorf("zero-noise filter: %v/%v vs %v/%v", lt.Level, lt.Trend, fit.Predict(xs[n-1]), fit.Slope)
	}
	// Its forecast interval is the regression prediction interval (known
	// sigma = 1): var = 1 + 1/n + (x-xbar)^2/Sxx, growing with the horizon.
	for _, h := range []float64{1, 5, 24} {
		mean, sd := lt.Forecast(h)
		x := xs[n-1] + h
		refNear(t, "trend forecast mean", mean, fit.Predict(x), 1e-4)
		want := math.Sqrt(1 + 1/float64(n) + (x-fit.XMean)*(x-fit.XMean)/fit.SXX)
		refNear(t, "trend forecast std", sd, want, 1e-5)
	}
	jsonSafe(t, "LocalTrend", lt)

	// h-step forecast variance with process noise: brute-force propagation
	// of the covariance, P <- F P F' + Q, h times.
	ltq := NewLocalTrend(0.05, 0.01, 1)
	for _, v := range y {
		ltq.Update(v)
	}
	p00, p01, p11 := ltq.P00, ltq.P01, ltq.P11
	lvl := ltq.Level
	prevSD := 0.0
	for h := 1; h <= 30; h++ {
		p00, p01, p11 = p00+2*p01+p11+0.05, p01+p11, p11+0.01
		lvl += ltq.Trend
		mean, sd := ltq.Forecast(float64(h))
		refNear(t, "LocalTrend.Forecast mean", mean, lvl, 1e-10)
		refNear(t, "LocalTrend.Forecast std", sd, math.Sqrt(p00+1), 1e-10)
		if sd <= prevSD {
			t.Fatalf("forecast std not growing at h=%d", h)
		}
		prevSD = sd
	}
	// Step(NaN, 5) equals five missing unit steps.
	a, b := *ltq, *ltq
	a.Step(math.NaN(), 5)
	for i := 0; i < 5; i++ {
		b.Update(math.NaN())
	}
	refSlice(t, "Step(NaN,5)", []float64{a.Level, a.Trend, a.P00, a.P01, a.P11}, []float64{b.Level, b.Trend, b.P00, b.P01, b.P11}, 1e-12)

	// Degenerate noise settings must not produce NaN.
	for _, qr := range [][3]float64{{0, 0, 0}, {1, 0, 0}, {0, 0, 1}, {0, 1, 0}, {math.NaN(), -1, math.Inf(1)}} {
		for _, v := range KalmanSmoothLevel(y[1:], qr[0], qr[2]) {
			if math.IsNaN(v) {
				t.Fatalf("KalmanSmoothLevel(q=%v r=%v) produced NaN", qr[0], qr[2])
			}
		}
		l, s := KalmanSmoothTrend(y[1:], qr[0], qr[1], qr[2])
		for i := range l {
			if !IsFinite(l[i]) || !IsFinite(s[i]) {
				t.Fatalf("KalmanSmoothTrend(%v) produced %v/%v at %d", qr, l[i], s[i], i)
			}
		}
	}
}

// -------------------------------------------------------------------- AR

func refCaseG() []float64 {
	r := &refRand{7}
	const n = 200
	x := make([]float64, n+2)
	for i := 2; i < n+2; i++ {
		x[i] = float64(0.6*x[i-1]) - float64(0.3*x[i-2]) + r.norm()
	}
	s := make([]float64, n)
	for i := range s {
		s[i] = x[i+2] + 5
	}
	return s
}

func TestRefAR(t *testing.T) {
	s := refCaseG()
	yw, err := FitARYuleWalker(s, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := ref(t, "G_yw")
	refSlice(t, "Yule-Walker", []float64{yw.Coef[0], yw.Coef[1], yw.Const, yw.Sigma2, yw.Mean}, want, 1e-10)
	ls, err := FitARLeastSquares(s, 2)
	if err != nil {
		t.Fatal(err)
	}
	want = ref(t, "G_ls")
	refSlice(t, "AR least squares", []float64{ls.Coef[0], ls.Coef[1], ls.Const, ls.Sigma2, float64(ls.N)}, want, 1e-9)
	jsonSafe(t, "ARModel", yw)

	m := &ARModel{Coef: []float64{0.6, -0.3}, Const: 1, Sigma2: 2, Mean: 1 / 0.7}
	refSlice(t, "AR forecast", m.Forecast([]float64{9, 9, 4, 6}, 8), ref(t, "G_fc"), 1e-13)
	refSlice(t, "AR forecast std", m.ForecastStd(8), ref(t, "G_sd"), 1e-13)
	// Lag order: Coef[0] multiplies the most recent sample.
	one := (&ARModel{Coef: []float64{1, 0}}).Forecast([]float64{4, 6}, 1)[0]
	two := (&ARModel{Coef: []float64{0, 1}}).Forecast([]float64{4, 6}, 1)[0]
	if one != 6 || two != 4 {
		t.Errorf("AR lag order: %v %v", one, two)
	}
	// AR(1) closed form: forecast mean reverts as phi^h, variance sigma^2*(1-phi^2h)/(1-phi^2).
	ar1 := &ARModel{Coef: []float64{0.8}, Const: 2, Sigma2: 1.5, Mean: 10}
	fc, sd := ar1.Forecast([]float64{13}, 6), ar1.ForecastStd(6)
	for h := 1; h <= 6; h++ {
		ph := math.Pow(0.8, float64(h))
		refNear(t, "AR1 forecast", fc[h-1], 10+ph*3, 1e-12)
		refNear(t, "AR1 std", sd[h-1], math.Sqrt(1.5*(1-ph*ph)/(1-0.64)), 1e-12)
	}
	// Infinities are missing values: they must neither end up in the model
	// nor make the fit fail.
	sInf := append([]float64(nil), s...)
	sNaN := append([]float64(nil), s...)
	for _, i := range []int{17, 90, 91} {
		sInf[i] = math.Inf(1 - 2*(i%2))
		sNaN[i] = math.NaN()
	}
	for name, fit := range map[string]func([]float64, int) (*ARModel, error){"Yule-Walker": FitARYuleWalker, "least squares": FitARLeastSquares} {
		a, errA := fit(sInf, 2)
		b, errB := fit(sNaN, 2)
		if errA != nil || errB != nil {
			t.Fatalf("%s with gaps: %v / %v", name, errA, errB)
		}
		jsonSafe(t, name+" with Inf", a)
		refSlice(t, name+" Inf = NaN", []float64{a.Coef[0], a.Coef[1], a.Const, a.Sigma2, a.Mean}, []float64{b.Coef[0], b.Coef[1], b.Const, b.Sigma2, b.Mean}, 1e-13)
		refNear(t, name+" gap tolerant", a.Coef[0], 0.55, 0.1)
	}
	refSlice(t, "Autocorr Inf = NaN", Autocorr(sInf, 3), Autocorr(sNaN, 3), 1e-13)
	// Missing history is imputed with the mean: forecast stays at the mean.
	refNear(t, "AR1 no history", ar1.Forecast(nil, 3)[2], 10, 1e-12)
	refNear(t, "AR1 NaN history", ar1.Forecast([]float64{math.NaN()}, 3)[2], 10, 1e-12)
}

// ------------------------------------------------------------- optimiser

func TestRefNelderMead(t *testing.T) {
	rosen := func(x []float64) float64 {
		return 100*(x[1]-x[0]*x[0])*(x[1]-x[0]*x[0]) + (1-x[0])*(1-x[0])
	}
	res, err := NelderMead(rosen, []float64{-1.2, 1}, nil)
	if err != nil || !res.Converged {
		t.Fatalf("Rosenbrock: %+v %v", res, err)
	}
	if math.Abs(res.X[0]-1) > 1e-4 || math.Abs(res.X[1]-1) > 1e-4 || res.F > 1e-8 {
		t.Errorf("Rosenbrock minimum: %+v", res)
	}
	refNear(t, "F is f(X)", res.F, rosen(res.X), 1e-15)
	// Sum of squares in 5 dimensions with different scales.
	target := []float64{3, -200, 0.001, 40000, -7}
	quad := func(x []float64) float64 {
		s := 0.0
		for i, v := range x {
			d := (v - target[i]) / (1 + math.Abs(target[i]))
			s += d * d
		}
		return s
	}
	res, err = NelderMead(quad, make([]float64, 5), &NMOptions{MaxIter: 20000, Restarts: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range res.X {
		if math.Abs(v-target[i]) > 1e-3*(1+math.Abs(target[i])) {
			t.Errorf("quadratic[%d] = %v, want %v (F=%v, iter=%d)", i, v, target[i], res.F, res.Iter)
		}
	}
	// Active bounds: the minimum of (x-3)^2 + (y+1)^2 in [0,2]x[0,5] is (2, 0).
	evalOutside := false
	bounded := func(x []float64) float64 {
		if x[0] < 0 || x[0] > 2 || x[1] < 0 || x[1] > 5 {
			evalOutside = true
		}
		return (x[0]-3)*(x[0]-3) + (x[1]+1)*(x[1]+1)
	}
	res, err = NelderMead(bounded, []float64{1, 3}, &NMOptions{Lower: []float64{0, 0}, Upper: []float64{2, 5}})
	if err != nil || evalOutside {
		t.Fatalf("bounded: %v outside=%v", err, evalOutside)
	}
	if math.Abs(res.X[0]-2) > 1e-5 || math.Abs(res.X[1]) > 1e-5 {
		t.Errorf("bounded minimum: %+v", res)
	}
	// A start outside the box is projected into it.
	res, _ = NelderMead(bounded, []float64{-50, 80}, &NMOptions{Lower: []float64{0, 0}, Upper: []float64{2, 5}})
	if evalOutside || math.Abs(res.X[0]-2) > 1e-5 || math.Abs(res.X[1]) > 1e-5 {
		t.Errorf("bounded from outside: %+v outside=%v", res, evalOutside)
	}
	// NaN regions are treated as +Inf, never returned as the optimum.
	nanf := func(x []float64) float64 {
		if x[0] < 0 {
			return math.NaN()
		}
		return (x[0]-0.5)*(x[0]-0.5) + 2
	}
	res, err = NelderMead(nanf, []float64{4}, nil)
	if err != nil || math.Abs(res.X[0]-0.5) > 1e-5 || math.Abs(res.F-2) > 1e-9 {
		t.Errorf("NaN objective: %+v %v", res, err)
	}
	// Everything NaN: a result without NaN in F that is not "converged to a value".
	res, err = NelderMead(func([]float64) float64 { return math.NaN() }, []float64{1, 2}, nil)
	if err != nil || math.IsNaN(res.F) || !math.IsInf(res.F, 1) || res.Converged {
		t.Errorf("all-NaN objective: %+v %v", res, err)
	}
	if g, err := GoldenSection(func(float64) float64 { return math.NaN() }, 0, 1, 0, 0); err != nil || g.Converged {
		t.Errorf("all-NaN golden section: %+v %v", g, err)
	}
	// One-dimensional, and the iteration budget.
	res, _ = NelderMead(func(x []float64) float64 { return math.Cosh(x[0] - 2) }, []float64{10}, nil)
	if math.Abs(res.X[0]-2) > 1e-4 {
		t.Errorf("1-D: %+v", res)
	}
	res, _ = NelderMead(rosen, []float64{-1.2, 1}, &NMOptions{MaxIter: 5})
	if res.Converged || res.Iter > 5 || res.F > rosen([]float64{-1.2, 1}) {
		t.Errorf("budget: %+v", res)
	}
	// Golden section and grid search on known minima.
	g, err := GoldenSection(func(x float64) float64 { return (x - math.Pi) * (x - math.Pi) }, 10, -10, 1e-10, 0)
	if err != nil || math.Abs(g.X[0]-math.Pi) > 1e-7 || !g.Converged {
		t.Errorf("GoldenSection: %+v %v", g, err)
	}
	gs, err := GridSearch(rosen, []float64{-2, -2}, []float64{2, 2}, 9, 12)
	if err != nil || math.Abs(gs.X[0]-1) > 1e-3 || math.Abs(gs.X[1]-1) > 1e-3 {
		t.Errorf("GridSearch: %+v %v", gs, err)
	}
	for _, r := range []OptResult{res, g, gs} {
		jsonSafe(t, "OptResult", r)
	}
}

// -------------------------------------------------------------- backtest

func TestRefBacktestNoLeak(t *testing.T) {
	r := &refRand{51}
	const n = 60
	y := make([]float64, n)
	for i := range y {
		y[i] = 100 + 3*float64(i%7) + r.norm()
	}
	y[33], y[41] = math.NaN(), math.NaN()
	orig := append([]float64(nil), y...)

	type call struct {
		n    int
		last float64
	}
	var calls []call
	// "Last value" forecaster that also tries to peek beyond its history.
	naive := func(history []float64, h int) ([]float64, error) {
		if cap(history) != len(history) {
			t.Errorf("history has spare capacity (%d > %d): the future is reachable", cap(history), len(history))
		}
		if len(history) > 0 && &history[0] != &y[0] {
			t.Errorf("expanding window must start at y[0]")
		}
		last := math.NaN()
		for i := len(history) - 1; i >= 0; i-- {
			if !math.IsNaN(history[i]) {
				last = history[i]
				break
			}
		}
		calls = append(calls, call{len(history), last})
		out := make([]float64, h)
		for i := range out {
			out[i] = last
		}
		return out, nil
	}
	opt := &BacktestOptions{Horizon: 5, MinTrain: 20, Step: 3}
	res, err := Backtest(y, naive, opt)
	if err != nil {
		t.Fatal(err)
	}
	// Expected origins and scores, by hand.
	var origins []int
	for o := 20; o < n; o += 3 {
		origins = append(origins, o)
	}
	if len(res.Origins) != len(origins) || len(calls) != len(origins) {
		t.Fatalf("origins %v, calls %d, want %v", res.Origins, len(calls), origins)
	}
	var absSum, sqSum, biasSum float64
	cnt := 0
	hAbs := make([]float64, 5)
	hCnt := make([]int, 5)
	for i, o := range origins {
		if res.Origins[i] != o || calls[i].n != o {
			t.Fatalf("origin %d: got %d, history len %d, want %d", i, res.Origins[i], calls[i].n, o)
		}
		// The forecaster saw exactly y[:o].
		last := math.NaN()
		for j := o - 1; j >= 0; j-- {
			if !math.IsNaN(orig[j]) {
				last = orig[j]
				break
			}
		}
		if calls[i].last != last {
			t.Fatalf("origin %d: forecaster's last value %v, want %v", o, calls[i].last, last)
		}
		for k := 0; k < 5 && o+k < n; k++ {
			a := orig[o+k]
			if math.IsNaN(a) {
				continue
			}
			e := last - a
			absSum += math.Abs(e)
			sqSum += e * e
			biasSum += e
			cnt++
			hAbs[k] += math.Abs(e)
			hCnt[k]++
		}
	}
	if res.Overall.N != cnt {
		t.Fatalf("scored %d points, want %d", res.Overall.N, cnt)
	}
	refNear(t, "Backtest MAE", res.Overall.MAE, absSum/float64(cnt), 1e-12)
	refNear(t, "Backtest RMSE", res.Overall.RMSE, math.Sqrt(sqSum/float64(cnt)), 1e-12)
	refNear(t, "Backtest Bias", res.Overall.Bias, biasSum/float64(cnt), 1e-12)
	for k := range hAbs {
		if res.ByHorizon[k].N != hCnt[k] {
			t.Errorf("horizon %d: N=%d want %d", k+1, res.ByHorizon[k].N, hCnt[k])
		}
		refNear(t, "Backtest MAE by horizon", res.ByHorizon[k].MAE, hAbs[k]/float64(hCnt[k]), 1e-12)
	}
	for i := range y {
		if y[i] != orig[i] && !(math.IsNaN(y[i]) && math.IsNaN(orig[i])) {
			t.Fatal("Backtest modified the series")
		}
	}
	jsonSafe(t, "BacktestResult", res)

	// A forecaster that tries to cheat by re-slicing cannot see the future,
	// and one that writes into its history cannot corrupt the actuals: the
	// scores of an honest forecaster stay the same when the data after the
	// last origin changes only in the future part.
	cheat := func(history []float64, h int) ([]float64, error) {
		defer func() { _ = recover() }()
		ext := history[:cap(history)]
		out := make([]float64, h)
		for i := range out {
			if len(history)+i < len(ext) {
				out[i] = ext[len(history)+i] // would be a perfect forecast
			} else {
				out[i] = history[len(history)-1]
			}
		}
		return out, nil
	}
	cres, err := Backtest(orig, cheat, opt)
	if err != nil {
		t.Fatal(err)
	}
	if cres.Overall.MAE < 0.5*res.Overall.MAE {
		t.Errorf("cheating forecaster scored MAE %v (honest %v): look-ahead leak", cres.Overall.MAE, res.Overall.MAE)
	}

	// Rolling window: exactly the last Window samples before the origin.
	calls = nil
	winF := func(history []float64, h int) ([]float64, error) {
		calls = append(calls, call{len(history), history[len(history)-1]})
		if cap(history) != len(history) {
			t.Errorf("window history has spare capacity")
		}
		return make([]float64, h), nil
	}
	clean := append([]float64(nil), orig...)
	clean[33], clean[41] = 0, 0
	wres, err := Backtest(clean, winF, &BacktestOptions{Horizon: 2, MinTrain: 10, Window: 8, MaxOrigins: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(wres.Origins) != 4 || wres.Origins[3] != 58 || wres.Origins[0] != 52 {
		t.Fatalf("MaxOrigins: %v", wres.Origins)
	}
	for i, o := range wres.Origins {
		if calls[i].n != 8 || calls[i].last != clean[o-1] {
			t.Fatalf("window call %d: len %d last %v, want 8 / %v", i, calls[i].n, calls[i].last, clean[o-1])
		}
	}
	// Defaults: horizon 1, first origin at len/2, every sample after it once.
	calls = nil
	dres, err := Backtest(clean, winF, nil)
	if err != nil || len(dres.Origins) != n-n/2 || dres.Origins[0] != n/2 || dres.Overall.N != n-n/2 {
		t.Fatalf("defaults: %v %+v", err, dres)
	}

	// Seasonal-naive baseline: brute-force "value one period ago".
	sn := SeasonalNaiveForecaster(7)
	sres, err := Backtest(clean, sn, &BacktestOptions{Horizon: 7, MinTrain: 14, Step: 7})
	if err != nil {
		t.Fatal(err)
	}
	abs, c := 0.0, 0
	for o := 14; o < n; o += 7 {
		for k := 0; k < 7 && o+k < n; k++ {
			abs += math.Abs(clean[o+k-7] - clean[o+k])
			c++
		}
	}
	refNear(t, "seasonal naive MAE", sres.Overall.MAE, abs/float64(c), 1e-12)
	// Horizon beyond one period repeats the last observed cycle.
	fc, err := SeasonalNaive([]float64{1, 2, 3, 4, 5, 6, 7, 8}, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	refSlice(t, "SeasonalNaive", fc, []float64{6, 7, 8, 6, 7, 8, 6}, 0)
	fc, _ = SeasonalNaive([]float64{1, 2, math.NaN(), 4, math.NaN(), math.NaN()}, 3, 4)
	refSlice(t, "SeasonalNaive NaN", fc, []float64{4, 2, 4, 4}, 0)

	// CompareBacktest: a perfect model has skill 1, the baseline against itself 0.
	perfect := func(history []float64, h int) ([]float64, error) {
		out := make([]float64, h)
		for i := range out {
			if len(history)+i < n {
				out[i] = clean[len(history)+i]
			}
		}
		return out, nil
	}
	cmp, err := CompareBacktest(clean, perfect, sn, &BacktestOptions{Horizon: 3, MinTrain: 21})
	if err != nil || cmp.Skill != 1 || cmp.SkillMAE != 1 {
		t.Errorf("perfect skill: %+v %v", cmp, err)
	}
	cmp, _ = CompareBacktest(clean, sn, sn, &BacktestOptions{Horizon: 3, MinTrain: 21})
	if cmp.Skill != 0 || cmp.SkillMAE != 0 {
		t.Errorf("self skill: %+v", cmp)
	}
	jsonSafe(t, "BacktestComparison", cmp)
}

func TestRefMetrics(t *testing.T) {
	actual := []float64{10, 0, 20, math.NaN(), -5, 8}
	pred := []float64{12, 1, 15, 3, -5, math.NaN()}
	// Complete pairs: (10,12) (0,1) (20,15) (-5,-5): errors +2 +1 -5 0.
	refNear(t, "MAE", MAE(actual, pred), 8.0/4, 1e-15)
	refNear(t, "MSE", MSE(actual, pred), 30.0/4, 1e-15)
	refNear(t, "RMSE", RMSE(actual, pred), math.Sqrt(7.5), 1e-15)
	refNear(t, "Bias", Bias(actual, pred), -2.0/4, 1e-15)
	// MAPE skips actual == 0: (20% + 25% + 0%)/3.
	refNear(t, "MAPE", MAPE(actual, pred), 15, 1e-13)
	// SMAPE: 2*2/22, 2*1/1, 2*5/35, 0 -> mean * 100.
	refNear(t, "SMAPE", SMAPE(actual, pred), 100*(4.0/22+2+10.0/35+0)/4, 1e-13)
	m := Evaluate(actual, pred)
	if m.N != 4 || m.NPct != 3 {
		t.Errorf("Evaluate N=%d NPct=%d", m.N, m.NPct)
	}
	refNear(t, "Corr", m.Corr, naivePearson([]float64{10, 0, 20, -5}, []float64{12, 1, 15, -5}), 1e-13)
	refNear(t, "SkillScore", SkillScore(2, 8), 0.75, 1e-15)
	refNear(t, "SkillScore worse", SkillScore(12, 8), -0.5, 1e-15)
	// Correlation of a series far from zero (hPa-sized offsets) keeps its precision.
	a := []float64{101325.1, 101325.4, 101324.9, 101326.2, 101325.8}
	p := []float64{101325.0, 101325.5, 101325.1, 101326.0, 101326.1}
	refNear(t, "Corr offset", Evaluate(a, p).Corr, naivePearson(a, p), 1e-9)
	jsonSafe(t, "Metrics", m)
	jsonSafe(t, "Metrics empty", Evaluate(nil, nil))
	jsonSafe(t, "Metrics constant", Evaluate([]float64{1, 1, 1}, []float64{2, 2, 2}))
}

// --------------------------------------------------- rolling / streaming

func TestRefRollingBruteForce(t *testing.T) {
	r := &refRand{71}
	const n = 300
	x := make([]float64, n)
	for i := range x {
		x[i] = 1e6 + 50*r.norm()
		if r.u() < 0.15 {
			x[i] = math.NaN()
		}
	}
	for i := 100; i < 112; i++ {
		x[i] = math.NaN() // a gap longer than the short windows
	}
	for _, w := range []int{1, 2, 5, 24, 400} {
		mean, sum, std := RollingMean(x, w), RollingSum(x, w), RollingStd(x, w)
		med, mn, mx := RollingMedian(x, w), RollingMin(x, w), RollingMax(x, w)
		cen := RollingMeanCentered(x, w)
		for i := range x {
			lo := max(i-w+1, 0)
			win := x[lo : i+1]
			m, s, c := naiveMeanStd(win)
			refNear(t, "RollingMean", mean[i], m, 1e-12)
			refNear(t, "RollingStd", std[i], s, 1e-9)
			if c == 0 {
				if !math.IsNaN(sum[i]) || !math.IsNaN(med[i]) || !math.IsNaN(mn[i]) || !math.IsNaN(mx[i]) {
					t.Fatalf("w=%d i=%d: empty window must be NaN", w, i)
				}
			} else {
				refNear(t, "RollingSum", sum[i], m*float64(c), 1e-12)
				refNear(t, "RollingMedian", med[i], naiveQuantile7(win, 0.5), 1e-12)
				refNear(t, "RollingMin", mn[i], naiveQuantile7(win, 0), 0)
				refNear(t, "RollingMax", mx[i], naiveQuantile7(win, 1), 0)
			}
			half := w / 2
			cm, _, _ := naiveMeanStd(x[max(i-half, 0):min(i+half+1, n)])
			refNear(t, "RollingMeanCentered", cen[i], cm, 1e-12)
		}
		// Streaming window agrees with the batch functions.
		win := NewWindow(w)
		for i, v := range x {
			win.Push(v)
			refNear(t, "Window.Mean", win.Mean(), mean[i], 1e-12)
			refNear(t, "Window.Std", win.Std(), std[i], 1e-9)
			refNear(t, "Window.Median", win.Median(), med[i], 1e-12)
			refNear(t, "Window.Min", win.Min(), mn[i], 0)
			refNear(t, "Window.Max", win.Max(), mx[i], 0)
			if win.Len() != min(i+1, w) || win.Full() != (i+1 >= w) {
				t.Fatalf("Window len/full at %d", i)
			}
		}
	}
	// Causality: a trailing statistic at i never depends on x[i+1:].
	x2 := append([]float64(nil), x...)
	for i := 150; i < n; i++ {
		x2[i] = -x2[i] + 7
	}
	for name, f := range map[string]func([]float64) []float64{
		"RollingMean":          func(v []float64) []float64 { return RollingMean(v, 24) },
		"RollingMedian":        func(v []float64) []float64 { return RollingMedian(v, 24) },
		"RollingStd":           func(v []float64) []float64 { return RollingStd(v, 24) },
		"EWMASeries":           func(v []float64) []float64 { return EWMASeries(v, 0.2) },
		"Diff":                 func(v []float64) []float64 { return Diff(v, 3) },
		"Lag":                  func(v []float64) []float64 { return Lag(v, 2) },
		"RollingAnomalyScores": func(v []float64) []float64 { return RollingAnomalyScores(v, 48, 5) },
	} {
		a, b := f(x), f(x2)
		for i := 0; i < 150; i++ {
			if a[i] != b[i] && !(math.IsNaN(a[i]) && math.IsNaN(b[i])) {
				t.Errorf("%s[%d] depends on later samples", name, i)
				break
			}
		}
	}
	// The rolling anomaly baseline excludes the sample itself: a spike does
	// not dilute its own score.
	flat := make([]float64, 60)
	for i := range flat {
		flat[i] = 10 + float64(i%5)
	}
	flat[50] = 1000
	sc := RollingAnomalyScores(flat, 30, 5)
	base := flat[20:50]
	medB := naiveQuantile7(base, 0.5)
	dev := make([]float64, len(base))
	for i, v := range base {
		dev[i] = math.Abs(v - medB)
	}
	refNear(t, "RollingAnomalyScores", sc[50], (1000-medB)/(MADScale*naiveQuantile7(dev, 0.5)), 1e-12)

	// Lag / Diff conventions.
	v := []float64{1, 4, 9, 16, 25}
	refSlice(t, "Lag 2", Lag(v, 2), []float64{math.NaN(), math.NaN(), 1, 4, 9}, 0)
	refSlice(t, "Lag -1", Lag(v, -1), []float64{4, 9, 16, 25, math.NaN()}, 0)
	refSlice(t, "Diff 1", Diff(v, 1), []float64{math.NaN(), 3, 5, 7, 9}, 0)
	refSlice(t, "Diff 2", Diff(v, 2), []float64{math.NaN(), math.NaN(), 8, 12, 16}, 0)
	refSlice(t, "FillLinear", FillLinear([]float64{math.NaN(), 1, math.NaN(), math.NaN(), 4, math.NaN(), math.NaN(), math.NaN(), 8, math.NaN()}, 2),
		[]float64{math.NaN(), 1, 2, 3, 4, math.NaN(), math.NaN(), math.NaN(), 8, math.NaN()}, 1e-15)

	// EWMA against the explicit weighted sum
	// m_t = (1-a)^t x_0 + a * sum_k (1-a)^k x_{t-k}.
	const a = 0.3
	clean := DropNaN(x)[:40]
	es := EWMASeries(clean, a)
	for ti := range clean {
		s := math.Pow(1-a, float64(ti)) * clean[0]
		for k := 0; k < ti; k++ {
			s += a * math.Pow(1-a, float64(k)) * clean[ti-k]
		}
		refNear(t, "EWMASeries", es[ti], s, 1e-12)
	}
	refNear(t, "AlphaFromHalfLife", math.Pow(1-AlphaFromHalfLife(6), 6), 0.5, 1e-14)
	// Irregular sampling: one step of dt=3 weighs like three steps of the same value.
	e1, e2 := NewEWMA(a), NewEWMA(a)
	e1.Add(5)
	e2.Add(5)
	e1.AddDT(9, 3)
	e2.Add(9)
	e2.Add(9)
	e2.Add(9)
	refNear(t, "AddDT", e1.Value(), e2.Value(), 1e-13)

	// Accumulator: merge equals pooling; matches the naive moments.
	var acc, a1, a2 Accumulator
	acc.AddAll(x)
	a1.AddAll(x[:117])
	a2.AddAll(x[117:])
	a1.Merge(a2)
	m, s, c := naiveMeanStd(x)
	if acc.N != int64(c) || a1.N != int64(c) {
		t.Fatalf("Accumulator N %d/%d want %d", acc.N, a1.N, c)
	}
	refNear(t, "Accumulator mean", acc.Mean(), m, 1e-13)
	refNear(t, "Accumulator std", acc.Std(), s, 1e-10)
	refNear(t, "merged mean", a1.Mean(), m, 1e-13)
	refNear(t, "merged std", a1.Std(), s, 1e-10)
	refNear(t, "merged min", a1.Min(), Min(x), 0)
	refNear(t, "merged max", a1.Max(), Max(x), 0)
	refNear(t, "Std offset", Std(x), s, 1e-10)
	refNear(t, "Variance vs PopVariance", Variance(x)*float64(c-1), PopVariance(x)*float64(c), 1e-12)
}

func TestRefSeasonal(t *testing.T) {
	// 2024-01-01 00:00:00 UTC was a Monday (Unix 1704067200).
	const mon = 1704067200
	if HourOfWeek(mon, 0) != 0 || HourOfDay(mon, 0) != 0 {
		t.Errorf("Monday 00:00 UTC: how=%d hod=%d", HourOfWeek(mon, 0), HourOfDay(mon, 0))
	}
	if HourOfWeek(mon-1, 0) != 167 || HourOfDay(mon-1, 0) != 23 {
		t.Errorf("Sunday 23:59:59: %d %d", HourOfWeek(mon-1, 0), HourOfDay(mon-1, 0))
	}
	// Sunday 2024-01-07 15:30 UTC = slot 6*24+15.
	if got := HourOfWeek(mon+6*86400+15*3600+1800, 0); got != 159 {
		t.Errorf("Sunday 15:30 = %d", got)
	}
	// UTC+2: Monday 00:00 UTC is Monday 02:00 local; UTC-5: Sunday 19:00.
	if HourOfWeek(mon, 7200) != 2 || HourOfWeek(mon, -5*3600) != 6*24+19 || HourOfDay(mon, -5*3600) != 19 {
		t.Error("UTC offsets")
	}
	// Before the epoch: 1969-12-31 23:00 UTC was a Wednesday.
	if HourOfWeek(-3600, 0) != 2*24+23 || HourOfDay(-3600, 0) != 23 || HourOfDay(-1, 0) != 23 {
		t.Errorf("negative times: %d %d", HourOfWeek(-3600, 0), HourOfDay(-3600, 0))
	}

	// Profile: slot s of period 5 holds values {s, s+10, s+20, 1000 (outlier in slot 2)}.
	const period = 5
	var y []float64
	for c := 0; c < 3; c++ {
		for s := 0; s < period; s++ {
			y = append(y, float64(s+10*c))
		}
	}
	y = append(y, math.NaN(), math.NaN(), 1000) // slots 0,1 missing, slot 2 outlier
	p, err := FitSeasonalProfile(y, 3, period, nil)
	if err != nil {
		t.Fatal(err)
	}
	// y[i] is in slot (3+i) mod 5.
	for s := 0; s < period; s++ {
		var vals []float64
		for i, v := range y {
			if (3+i)%period == s && !math.IsNaN(v) {
				vals = append(vals, v)
			}
		}
		lv, ok := p.At(s)
		if !ok || p.Count[s] != len(vals) {
			t.Fatalf("slot %d: ok=%v count=%d want %d", s, ok, p.Count[s], len(vals))
		}
		refNear(t, "profile median", lv, naiveQuantile7(vals, 0.5), 1e-13)
		lw, _ := p.At(s + 3*period) // wrapped
		lneg, _ := p.At(s - period)
		if lw != lv || lneg != lv {
			t.Errorf("slot wrapping")
		}
	}
	pm, _ := FitSeasonalProfile(y, 3, period, &ProfileOptions{Mean: true})
	for s := 0; s < period; s++ {
		var vals []float64
		for i, v := range y {
			if (3+i)%period == s && !math.IsNaN(v) {
				vals = append(vals, v)
			}
		}
		m, _, _ := naiveMeanStd(vals)
		refNear(t, "profile mean", pm.Level[s], m, 1e-13)
	}
	// Forgetting: weights 0.5^(age/halfLife), age counted from the end.
	ph, _ := FitSeasonalProfile(y, 3, period, &ProfileOptions{Mean: true, HalfLife: 4})
	for s := 0; s < period; s++ {
		sw, swv := 0.0, 0.0
		for i, v := range y {
			if (3+i)%period == s && !math.IsNaN(v) {
				w := math.Pow(0.5, float64(len(y)-1-i)/4)
				sw += w
				swv += w * v
			}
		}
		refNear(t, "profile weighted mean", ph.Level[s], swv/sw, 1e-12)
		refNear(t, "profile weight", ph.Weight[s], sw, 1e-12)
	}
	// Forecast / Deseasonalize use the same slot alignment.
	fc := p.Forecast(3+len(y), 7)
	for k, v := range fc {
		lv, _ := p.At(3 + len(y) + k)
		if v != lv {
			t.Errorf("Forecast slot alignment at %d", k)
		}
	}
	ds := p.Deseasonalize(y, 3)
	for i, v := range y {
		lv, _ := p.At(3 + i)
		refNear(t, "Deseasonalize", ds[i], v-lv, 1e-13)
	}
	// Gap filling around the cycle: slots 1..3 of 6 undefined, linear from slot 0 (=6) to slot 4 (=14); slot 5 between 4 and 0.
	g := NewSeasonalProfile(6)
	g.Update(0, 6, 0.5)
	g.Update(4, 14, 0.5)
	filled, ok := g.Filled()
	if !ok {
		t.Fatal("Filled")
	}
	refSlice(t, "Filled", filled, []float64{6, 8, 10, 12, 14, 10}, 1e-13)
	jsonSafe(t, "SeasonalProfile", p)
	jsonSafe(t, "SeasonalProfile sparse", g)
}

func TestRefBinnedResponseAndChange(t *testing.T) {
	r := &refRand{81}
	const n = 400
	x, y := make([]float64, n), make([]float64, n)
	for i := range x {
		x[i] = r.u() * 100
		y[i] = 0.02*x[i]*x[i] + r.norm()
	}
	x[7], y[9] = math.NaN(), math.NaN()
	bins, err := BinnedResponse(x, y, &BinOptions{Bins: 8, Lo: 0, Hi: 100, Quantiles: []float64{0.25, 0.75}})
	if err != nil || len(bins) != 8 {
		t.Fatalf("BinnedResponse: %d bins, %v", len(bins), err)
	}
	total := 0
	for b, bin := range bins {
		var bx, by []float64
		for i := range x {
			if math.IsNaN(x[i]) || math.IsNaN(y[i]) {
				continue
			}
			lo, hi := 12.5*float64(b), 12.5*float64(b+1)
			if x[i] >= lo && (x[i] < hi || (b == 7 && x[i] == hi)) {
				bx = append(bx, x[i])
				by = append(by, y[i])
			}
		}
		total += bin.N
		if bin.N != len(by) || bin.Lo != 12.5*float64(b) || bin.Hi != 12.5*float64(b+1) {
			t.Fatalf("bin %d: N=%d want %d, edges %v..%v", b, bin.N, len(by), bin.Lo, bin.Hi)
		}
		mx, _, _ := naiveMeanStd(bx)
		my, sy, _ := naiveMeanStd(by)
		refNear(t, "bin XMean", bin.XMean, mx, 1e-12)
		refNear(t, "bin YMean", bin.YMean, my, 1e-12)
		refNear(t, "bin YStd", bin.YStd, sy, 1e-11)
		refNear(t, "bin Q25", bin.YQ[0], naiveQuantile7(by, 0.25), 1e-12)
		refNear(t, "bin Q75", bin.YQ[1], naiveQuantile7(by, 0.75), 1e-12)
	}
	if total != n-2 {
		t.Errorf("binned %d points, want %d", total, n-2)
	}
	eq, err := BinnedResponse(x, y, &BinOptions{Bins: 6, EqualCount: true})
	if err != nil || len(eq) != 6 {
		t.Fatal("equal-count bins")
	}
	total = 0
	for i, b := range eq {
		total += b.N
		if b.N != 66 && b.N != 67 {
			t.Errorf("equal-count bin %d holds %d", i, b.N)
		}
		if i > 0 && b.Lo < eq[i-1].Hi {
			t.Errorf("equal-count bins overlap")
		}
	}
	if total != n-2 {
		t.Errorf("equal-count total %d", total)
	}
	jsonSafe(t, "ResponseBin", bins)

	// Change point at a known index; CUSUM alarm shortly after it.
	z := make([]float64, 200)
	for i := range z {
		z[i] = r.norm()
		if i >= 120 {
			z[i] += 3
		}
	}
	z[60] = math.NaN()
	cp, ok := DetectChangePoint(z, 10)
	if !ok || cp.Index < 117 || cp.Index > 123 || math.Abs(cp.Shift-3) > 0.6 || cp.Score < 8 {
		t.Errorf("DetectChangePoint: %+v ok=%v", cp, ok)
	}
	pos, neg, alarms := CUSUM(z, 0.5, 5)
	if len(alarms) == 0 || alarms[0] < 120 || alarms[0] > 128 {
		t.Errorf("CUSUM alarms %v", alarms)
	}
	// Textbook recursion.
	var sp, sn float64
	for i, v := range z {
		if !math.IsNaN(v) {
			sp = math.Max(0, sp+v-0.5)
			sn = math.Max(0, sn-v-0.5)
		}
		if pos[i] != sp || neg[i] != sn {
			t.Fatalf("CUSUM recursion at %d", i)
		}
		if sp > 5 || sn > 5 {
			sp, sn = 0, 0
		}
	}
	jsonSafe(t, "ChangePoint", cp)
}

// ---------------------------------------------------------- linear algebra

func TestRefLinalg(t *testing.T) {
	// Hilbert matrix of order 6 (cond ~ 1.5e7): exact inverse is integral,
	// first row 36, -630, 3360, -7560, 7560, -2772.
	const n = 6
	h := NewMatrix(n, n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			h.Set(i, j, 1/float64(i+j+1))
		}
	}
	l, err := Cholesky(h)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := CholeskyInverse(l)
	if err != nil {
		t.Fatal(err)
	}
	refSlice(t, "Hilbert inverse", inv.Row(0), []float64{36, -630, 3360, -7560, 7560, -2772}, 1e-8)
	b := make([]float64, n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			b[i] += h.At(i, j) // solution: all ones
		}
	}
	ones := []float64{1, 1, 1, 1, 1, 1}
	for name, f := range map[string]func() ([]float64, error){
		"Solve":        func() ([]float64, error) { return Solve(h, b) },
		"SolveSPD":     func() ([]float64, error) { return SolveSPD(h, b) },
		"LeastSquares": func() ([]float64, error) { return LeastSquares(h, b) },
	} {
		x, err := f()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		refSlice(t, name, x, ones, 1e-8)
	}
	// QR least squares on an overdetermined system against the exact
	// solution of a constructed problem: b = A*x0 + r with A'r = 0.
	a, _ := MatrixFromRows([][]float64{{1, 1}, {1, 2}, {1, 3}, {1, 4}})
	x, err := LeastSquares(a, []float64{6, 5, 7, 10}) // classic: y = 3.5 + 1.4 x
	if err != nil {
		t.Fatal(err)
	}
	refSlice(t, "LeastSquares line", x, []float64{3.5, 1.4}, 1e-13)
	qr, _ := NewQR(a)
	ni, _ := qr.NormalInverse()
	// (A'A)^-1 = [[30,-10],[-10,4]]/20.
	refSlice(t, "NormalInverse", ni.Data, []float64{1.5, -0.5, -0.5, 0.2}, 1e-13)
	at := a.T()
	ata, _ := at.Mul(a)
	refSlice(t, "A'A", ata.Data, []float64{4, 10, 10, 30}, 0)
	v, _ := a.MulVec([]float64{2, -1})
	refSlice(t, "MulVec", v, []float64{1, 0, -1, -2}, 0)
	// The units of individual equations / unknowns must not matter: these
	// systems are perfectly conditioned after scaling.
	scaled := []struct {
		a    [][]float64
		b, x []float64
	}{
		{[][]float64{{1e20, 0}, {0, 1}}, []float64{2e20, 3}, []float64{2, 3}},
		{[][]float64{{1e-20, 2e-20}, {3, 4}}, []float64{5e-20, 11}, []float64{1, 2}},
		{[][]float64{{1, 2e12}, {3, 4e12}}, []float64{5, 11}, []float64{1, 2e-12}},
		// diag(1e15, 1, 1e-12) * [[4,1,2],[1,5,1],[2,1,6]] * diag(1e-8, 1, 1e10)
		{[][]float64{{4e7, 1e15, 2e25}, {1e-8, 5, 1e10}, {2e-20, 1e-12, 6e-2}}, []float64{1.2e16, 14, 2.2e-11}, []float64{1e8, 2, 3e-10}},
	}
	for _, c := range scaled {
		am, _ := MatrixFromRows(c.a)
		got, err := Solve(am, c.b)
		if err != nil {
			t.Errorf("Solve(%v): %v", c.a, err)
			continue
		}
		for i := range got {
			if math.Abs(got[i]-c.x[i]) > 1e-12*math.Abs(c.x[i]) {
				t.Errorf("Solve(%v) = %v, want %v", c.a, got, c.x)
				break
			}
		}
	}
	// A scaled singular system is still singular.
	ss, _ := MatrixFromRows([][]float64{{1e20, 2e20}, {1, 2}})
	if _, err := Solve(ss, []float64{1, 2}); !errors.Is(err, ErrSingular) {
		t.Errorf("Solve scaled singular: %v", err)
	}
	nn, _ := MatrixFromRows([][]float64{{1, math.NaN()}, {0, 1}})
	if x, err := Solve(nn, []float64{1, 2}); err == nil {
		t.Errorf("Solve with NaN returned %v", x)
	}
	// Singular systems are reported, not solved.
	sing, _ := MatrixFromRows([][]float64{{1, 2}, {2, 4}})
	if _, err := Solve(sing, []float64{1, 2}); !errors.Is(err, ErrSingular) {
		t.Errorf("Solve singular: %v", err)
	}
	if _, err := Cholesky(sing); !errors.Is(err, ErrSingular) {
		t.Errorf("Cholesky singular: %v", err)
	}
	col, _ := MatrixFromRows([][]float64{{1, 2}, {2, 4}, {3, 6}})
	if _, err := LeastSquares(col, []float64{1, 2, 3}); !errors.Is(err, ErrSingular) {
		t.Errorf("LeastSquares collinear: %v", err)
	}
}

// ------------------------------------------- short and degenerate inputs

func TestRefShortInputs(t *testing.T) {
	nan := math.NaN()
	inputs := [][]float64{nil, {}, {3}, {nan}, {3, 3}, {3, nan}, {1, 2}, {nan, nan, nan}, {5, 5, 5, 5, 5, 5}, {math.Inf(1), 1, 2}, {math.Inf(1), math.Inf(-1)}}
	for _, x := range inputs {
		x := x
		y := make([]float64, len(x))
		for i := range y {
			y[i] = float64(i)
		}
		X := make([][]float64, len(x))
		for i, v := range x {
			X[i] = []float64{v}
		}
		noPanic(t, "descriptive", func() {
			Count(x)
			DropNaN(x)
			Sum(x)
			Mean(x)
			Variance(x)
			PopVariance(x)
			Std(x)
			PopStd(x)
			Min(x)
			Max(x)
			MinMax(x)
			Median(x)
			Quantile(x, 0.3)
			Quantiles(x, 0, 0.5, 1)
			MAD(x)
			RobustStd(x)
			WeightedMean(x, y)
			WeightedQuantile(x, y, 0.5)
			TrimmedMean(x, 0.2)
			ZScores(x)
			RobustZScores(x)
			jsonSafe(t, "Describe", Describe(x))
			Describe(dropInf(x))
			Histogram(x, 0, 1, 4)
			Ranks(x)
		})
		noPanic(t, "rolling", func() {
			for _, w := range []int{-1, 0, 1, 2, 5} {
				RollingMean(x, w)
				RollingSum(x, w)
				RollingStd(x, w)
				RollingMedian(x, w)
				RollingMin(x, w)
				RollingMax(x, w)
				RollingMeanCentered(x, w)
				RollingApply(x, w, 0, nil)
				RollingAnomalyScores(x, w, 0)
				Lag(x, w)
				Lag(x, -w)
				Diff(x, w)
				FillLinear(x, w)
				win := NewWindow(w)
				for _, v := range x {
					win.Push(v)
				}
				win.Mean()
				win.Std()
				win.Min()
				win.Max()
				win.Median()
				win.Values(nil)
				win.Reset()
			}
			EWMASeries(x, 0.3)
			EWMASeries(x, nan)
			var acc Accumulator
			acc.AddAll(x)
			acc.Merge(Accumulator{})
			acc.Std()
		})
		noPanic(t, "circular", func() {
			CircularSummary(x, nil)
			CircularSummary(x, y)
			CircularMeanDeg(x)
			ResultantLength(x)
			CircularVariance(x)
			CircularStdDeg(x)
			VectorMeanWind(x, y)
		})
		noPanic(t, "assoc", func() {
			Pearson(x, y)
			Spearman(x, y)
			Covariance(x, y)
			PartialCorr(x, y, y)
			CrossCorr(x, y, 3)
			CrossCorr(x, y, -1)
			BestLag(x, y, 3, 0)
			Autocorr(x, 3)
			Autocorr(x, -2)
			BinnedResponse(x, y, nil)
			BinnedResponse(x, y, &BinOptions{EqualCount: true, Bins: 3})
		})
		noPanic(t, "regress", func() {
			LinearRegression(x, y)
			LinearRegression(y, x)
			TheilSen(x, y)
			TheilSen(y, x)
			OLS(X, y, true)
			OLS(X, y, false)
			WLS(X, y, y, true)
			Ridge(X, y, nil, 0)
			Ridge(X, y, nil, 1)
			RidgeWeighted(X, y, y, nil, 1)
			RidgeCV(X, y, nil, []float64{0, 1}, KFold(len(x), 2))
			Logistic(X, y, nil, nil)
			Logistic(X, x, nil, &LogisticOptions{Lambda: 1})
		})
		noPanic(t, "series", func() {
			for _, p := range []int{-1, 0, 1, 2, 3} {
				FitHoltWinters(x, p, nil)
				FitHoltWinters(x, p, &HWOptions{NoTrend: true})
				FitHoltWinters(x, p, &HWOptions{Damped: true, Params: &HWParams{Alpha: 0.5, Beta: 0.1, Gamma: 0.1, Phi: 0.9}})
				FitSeasonalProfile(x, 1, p, nil)
				FitSeasonalProfile(x, -7, p, &ProfileOptions{Mean: true, HalfLife: 2})
				SeasonalNaive(x, p, 3)
				SeasonalNaive(x, p, 0)
				FitARYuleWalker(x, p)
				FitARLeastSquares(x, p)
			}
			FitHolt(x, nil)
			AnomalyScores(x)
			DetectChangePoint(x, 0)
			CUSUM(x, 0.5, 5)
			KalmanSmoothLevel(x, 0.1, 1)
			KalmanSmoothTrend(x, 0.1, 0.01, 1)
			(&ARModel{}).Forecast(x, 2)
			(&ARModel{Coef: []float64{0.5, 0.2}}).Forecast(x, 2)
			(&ARModel{}).ForecastStd(2)
		})
		noPanic(t, "eval", func() {
			MAE(x, y)
			MSE(x, y)
			RMSE(x, y)
			Bias(x, y)
			MAPE(x, y)
			SMAPE(x, y)
			jsonSafe(t, "Evaluate", Evaluate(x, y))
			Backtest(x, SeasonalNaiveForecaster(2), nil)
			Backtest(x, SeasonalNaiveForecaster(2), &BacktestOptions{Horizon: 3, MinTrain: 1, Window: 1})
			CompareBacktest(x, SeasonalNaiveForecaster(1), SeasonalNaiveForecaster(2), nil)
			Backtest(x, nil, nil)
		})
	}

	// Documented results for the smallest inputs.
	if !math.IsNaN(Mean(nil)) || !math.IsNaN(Std([]float64{1})) || !math.IsNaN(Median(nil)) || Sum(nil) != 0 {
		t.Error("empty summaries")
	}
	if Median([]float64{4}) != 4 || Quantile([]float64{4}, 0.9) != 4 || PopStd([]float64{4}) != 0 {
		t.Error("single value summaries")
	}
	if _, err := Pearson([]float64{1, 2}, []float64{1, 2}); !errors.Is(err, ErrInsufficientData) {
		t.Errorf("Pearson n=2: %v", err)
	}
	if _, err := Pearson([]float64{1, 2, 3}, []float64{1, 2}); !errors.Is(err, ErrDimension) {
		t.Errorf("Pearson dimension: %v", err)
	}
	if _, err := LinearRegression([]float64{1, 2}, []float64{1, 2}); !errors.Is(err, ErrInsufficientData) {
		t.Errorf("LinearRegression n=2: %v", err)
	}
	if _, err := OLS([][]float64{{1}, {2}}, []float64{1, 2}, true); !errors.Is(err, ErrInsufficientData) {
		t.Errorf("OLS n=p: %v", err)
	}
	if _, err := OLS(nil, nil, true); !errors.Is(err, ErrInsufficientData) {
		t.Errorf("OLS empty: %v", err)
	}
	if _, err := FitHoltWinters([]float64{1, 2}, 0, nil); !errors.Is(err, ErrInsufficientData) {
		t.Errorf("FitHoltWinters n=2: %v", err)
	}
	if _, err := FitHoltWinters([]float64{1, 2, 3, 4, 5}, 3, nil); !errors.Is(err, ErrInsufficientData) {
		t.Errorf("FitHoltWinters < 2 seasons: %v", err)
	}
	if _, err := Backtest([]float64{1}, SeasonalNaiveForecaster(1), nil); !errors.Is(err, ErrInsufficientData) {
		t.Errorf("Backtest n=1: %v", err)
	}
	if _, err := NelderMead(func([]float64) float64 { return 0 }, nil, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("NelderMead empty x0: %v", err)
	}
	// Matrix helpers on empty shapes.
	noPanic(t, "linalg empty", func() {
		e := NewMatrix(0, 0)
		e.T()
		e.Clone()
		e.Mul(e)
		e.MulVec(nil)
		Cholesky(e)
		Solve(e, nil)
		NewQR(e)
		LeastSquares(e, nil)
		MatrixFromRows(nil)
		MatrixFromRows([][]float64{{}})
		NewMatrix(-1, 3)
	})
	// Zero-value and inconsistent (e.g. badly deserialised) structs.
	noPanic(t, "zero values", func() {
		var w Window
		w.Push(1)
		w.Push(2)
		if w.Len() != 1 || w.Mean() != 2 || !w.Full() {
			t.Errorf("zero-value Window: len %d mean %v", w.Len(), w.Mean())
		}
		var e EWMA
		e.Add(1)
		e.Add(2)
		var hw HoltWinters
		hw.Update(1)
		hw.Forecast(3)
		hw.ForecastStd(3)
		hw.ForecastInterval(3, 0.9)
		bad := HoltWinters{Period: 4, Season: []float64{1, 2, 3, 4}, N: -3}
		bad.Update(1)
		bad.Forecast(6)
		short := HoltWinters{Period: 4, Season: []float64{1, 2}}
		short.Update(1)
		short.Forecast(6)
		var o OLSResult
		o.Predict(nil)
		o.PredictInterval(nil, 0.9)
		o2 := OLSResult{Coef: []float64{1, 2}, Intercept: true, DF: 5, ResidStd: 1}
		if _, lo, _ := o2.PredictInterval([]float64{3}, 0.9); !math.IsNaN(lo) {
			t.Errorf("OLSResult without Cov: interval %v", lo)
		}
		var m Model
		m.Predict(nil)
		m.PredictInterval(nil, 0.9)
		m.RawCoef()
		m.PredictMap(nil)
		_ = m.Validate()
		m2 := Model{Kind: KindLinear, Coef: []float64{1, 2}, Mean: []float64{0}, Scale: []float64{1, 1}, Features: []string{"a"}}
		m2.Predict([]float64{1, 2})
		m2.PredictInterval([]float64{1, 2}, 0.9)
		m2.RawCoef()
		m2.PredictMap(map[string]float64{"a": 1})
		sp := SeasonalProfile{Period: 3, Level: []float64{1, 2, 3}}
		sp.Update(1, 5, 0.5)
		sp.At(1)
		sp.Filled()
		sp.Forecast(0, 4)
		sp.Deseasonalize([]float64{1, 2}, 0)
		var sp0 *SeasonalProfile
		sp0.At(0)
		sp0.Update(0, 1, 0.5)
		sp0.Filled()
		var ar ARModel
		ar.Forecast(nil, 2)
		ar.ForecastStd(2)
		var ll LocalLevel
		ll.Update(1)
		ll.Update(2)
		var lt LocalTrend
		lt.Update(1)
		lt.Update(2)
		lt.Forecast(math.Inf(1))
		var lf LinearFit
		lf.PredictInterval(1, 0.9)
		lf.ConfidenceInterval(1, 0.9)
		var acc Accumulator
		acc.Merge(acc)
	})
	if m, s := (&LocalTrend{N: 3, Trend: 0, Level: 1}).Forecast(math.Inf(1)); !math.IsNaN(m) || !math.IsNaN(s) {
		t.Errorf("Forecast(+Inf) = %v, %v", m, s)
	}
	// Streaming structs before the first sample.
	if !math.IsNaN(NewEWMA(0.5).Value()) || !math.IsNaN((Accumulator{}).Mean()) || !math.IsNaN(NewWindow(3).Mean()) {
		t.Error("empty streaming state must report NaN")
	}
	k := NewLocalTrend(0.1, 0.1, 1)
	if m, s := k.Forecast(3); !math.IsNaN(m) || !math.IsNaN(s) {
		t.Error("LocalTrend.Forecast before data")
	}
	jsonSafe(t, "empty LocalTrend", k)
	jsonSafe(t, "empty EWMA", NewEWMA(0.5))
	jsonSafe(t, "empty Accumulator", Accumulator{})
}

// dropInf removes infinities (test helper).
func dropInf(x []float64) []float64 {
	var out []float64
	for _, v := range x {
		if !math.IsInf(v, 0) {
			out = append(out, v)
		}
	}
	return out
}

func TestRefConstantSeries(t *testing.T) {
	c := make([]float64, 50)
	ramp := make([]float64, 50)
	for i := range c {
		c[i] = 42.5
		ramp[i] = float64(i)
	}
	// Correlations with a constant are undefined, not NaN-valued results.
	if r, err := Pearson(c, ramp); !errors.Is(err, ErrDegenerate) || math.IsNaN(r.R) {
		t.Errorf("Pearson constant: %+v %v", r, err)
	}
	if _, err := Spearman(ramp, c); !errors.Is(err, ErrDegenerate) {
		t.Errorf("Spearman constant: %v", err)
	}
	if _, err := PartialCorr(ramp, ramp, c); !errors.Is(err, ErrDegenerate) {
		t.Errorf("PartialCorr constant: %v", err)
	}
	if _, err := LinearRegression(c, ramp); !errors.Is(err, ErrSingular) {
		t.Errorf("LinearRegression constant x: %v", err)
	}
	f, err := LinearRegression(ramp, c)
	if err != nil || f.Slope != 0 || f.R2 != 0 || f.P != 1 || f.ResidStd != 0 {
		t.Errorf("LinearRegression constant y: %+v %v", f, err)
	}
	jsonSafe(t, "LinearFit constant y", f)
	for _, cc := range CrossCorr(c, ramp, 2) {
		if !math.IsNaN(cc.R) {
			t.Errorf("CrossCorr with a constant must be NaN, got %v", cc.R)
		}
	}
	if _, err := BestLag(c, ramp, 2, 3); !errors.Is(err, ErrInsufficientData) {
		t.Errorf("BestLag constant: %v", err)
	}
	if Autocorr(c, 3) != nil {
		t.Error("Autocorr constant must be nil")
	}
	for _, z := range ZScores(c) {
		if z != 0 {
			t.Fatalf("ZScores constant: %v", z)
		}
	}
	for _, z := range RobustZScores(c) {
		if z != 0 {
			t.Fatalf("RobustZScores constant: %v", z)
		}
	}
	for _, z := range RollingAnomalyScores(c, 10, 3)[10:] {
		if z != 0 {
			t.Fatalf("RollingAnomalyScores constant: %v", z)
		}
	}
	if Std(c) != 0 || MAD(c) != 0 || Variance(c) != 0 {
		t.Errorf("constant spread: %v %v", Std(c), MAD(c))
	}
	if _, err := FitARYuleWalker(c, 2); !errors.Is(err, ErrDegenerate) {
		t.Errorf("Yule-Walker constant: %v", err)
	}
	if _, err := FitARLeastSquares(c, 2); err == nil {
		t.Error("AR least squares on a constant must fail")
	}
	if _, ok := DetectChangePoint(c, 5); ok {
		t.Error("DetectChangePoint constant")
	}
	// Regression on a constant feature.
	X := make([][]float64, len(c))
	for i := range X {
		X[i] = []float64{c[i], ramp[i]}
	}
	if _, err := OLS(X, ramp, true); !errors.Is(err, ErrSingular) {
		t.Errorf("OLS constant column + intercept: %v", err)
	}
	m, err := Ridge(X, ramp, nil, 0.1)
	if err != nil || m.Coef[0] != 0 {
		t.Errorf("Ridge constant feature: %+v %v", m, err)
	}
	jsonSafe(t, "Ridge constant feature", m)
	// Constant target: perfect fit with zero variance everywhere.
	Xr := make([][]float64, len(c))
	for i := range Xr {
		Xr[i] = []float64{ramp[i]}
	}
	oc, err := OLS(Xr, c, true)
	if err != nil {
		t.Fatal(err)
	}
	jsonSafe(t, "OLS constant y", oc)
	if oc.R2 != 0 || math.Abs(oc.Coef[0]-42.5) > 1e-9 || math.Abs(oc.Coef[1]) > 1e-12 {
		t.Errorf("OLS constant y: %+v", oc)
	}
	_, lo, hi := oc.PredictInterval([]float64{3}, 0.95)
	if math.IsNaN(lo) || math.IsNaN(hi) {
		t.Error("OLS interval on constant y is NaN")
	}
	mc, err := Ridge(Xr, c, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	jsonSafe(t, "Ridge constant y", mc)
	if _, err := Logistic(Xr, make([]float64, len(c)), nil, nil); !errors.Is(err, ErrDegenerate) {
		t.Errorf("Logistic single class: %v", err)
	}
	// Metrics and backtests on constants stay JSON-safe.
	jsonSafe(t, "Evaluate constant", Evaluate(c, c))
	cmp, err := CompareBacktest(c, SeasonalNaiveForecaster(1), SeasonalNaiveForecaster(7), nil)
	if err != nil {
		t.Fatal(err)
	}
	jsonSafe(t, "CompareBacktest constant", cmp)
	// Smoothers.
	for _, v := range KalmanSmoothLevel(c, 0.1, 1) {
		refNear(t, "Kalman constant", v, 42.5, 1e-12)
	}
	lv, tr := KalmanSmoothTrend(c, 0.1, 0.01, 1)
	for i := range lv {
		refNear(t, "Kalman trend constant level", lv[i], 42.5, 1e-9)
		refNear(t, "Kalman trend constant slope", tr[i], 0, 1e-9)
	}
	p, err := FitSeasonalProfile(c, 0, 24, nil)
	if err != nil {
		t.Fatal(err)
	}
	jsonSafe(t, "profile constant", p)
}

func TestRefNonFiniteStreaming(t *testing.T) {
	// Infinities must not poison persisted state (JSON cannot hold them and
	// every later value would turn into NaN).
	e := NewEWMA(0.3)
	for _, v := range []float64{1, 2, math.Inf(1), math.NaN(), 3, math.Inf(-1), 4} {
		e.Add(v)
	}
	if !IsFinite(e.Mean) || !IsFinite(e.Var) {
		t.Errorf("EWMA after Inf: %+v", e)
	}
	jsonSafe(t, "EWMA", e)
	want := NewEWMA(0.3)
	for _, v := range []float64{1, 2, 3, 4} {
		want.Add(v)
	}
	refNear(t, "EWMA skips Inf", e.Mean, want.Mean, 1e-15)

	var a Accumulator
	a.AddAll([]float64{1, math.Inf(1), 2, math.NaN(), 3, math.Inf(-1)})
	jsonSafe(t, "Accumulator", a)
	if a.N != 3 || a.Mean() != 2 || a.Min() != 1 || a.Max() != 3 {
		t.Errorf("Accumulator after Inf: %+v", a)
	}
	p := NewSeasonalProfile(4)
	p.Update(1, math.Inf(1), 0.5)
	p.Update(1, 5, 0.5)
	jsonSafe(t, "SeasonalProfile", p)
	hw := &HoltWinters{HWParams: HWParams{Alpha: 0.5, Phi: 1}, Level: 1}
	hw.Update(math.Inf(1))
	hw.Update(math.NaN())
	hw.Update(2)
	jsonSafe(t, "HoltWinters", hw)
	ll := NewLocalLevel(0.1, 1)
	lt := NewLocalTrend(0.1, 0.1, 1)
	for _, v := range []float64{math.Inf(1), 1, math.Inf(-1), math.NaN(), 2} {
		ll.Update(v)
		lt.Update(v)
	}
	jsonSafe(t, "LocalLevel", ll)
	jsonSafe(t, "LocalTrend", lt)
	// Describe is documented as JSON-safe: non-finite values are skipped.
	d := Describe([]float64{1, math.Inf(1), 3, math.NaN(), math.Inf(-1)})
	jsonSafe(t, "Describe", d)
	if d.N != 2 || d.Mean != 2 || d.Min != 1 || d.Max != 3 {
		t.Errorf("Describe with Inf: %+v", d)
	}
	// Batch summaries: NaN is missing everywhere.
	x := []float64{math.NaN(), 4, math.NaN(), 8}
	if Mean(x) != 6 || Sum(x) != 12 || Count(x) != 2 || Median(x) != 6 || Min(x) != 4 || Max(x) != 8 {
		t.Error("NaN must be skipped by summaries")
	}
	z := ZScores(x)
	if !math.IsNaN(z[0]) || !math.IsNaN(z[2]) || math.IsNaN(z[1]) || math.IsNaN(z[3]) {
		t.Errorf("ZScores NaN positions: %v", z)
	}
	// One infinity must not wipe out the z-scores of all other samples.
	zi := ZScores([]float64{1, 2, math.Inf(1), 3})
	if !math.IsNaN(zi[2]) || zi[0] != -1 || zi[1] != 0 || zi[3] != 1 {
		t.Errorf("ZScores with Inf: %v", zi)
	}
	// Baseline forecasts never repeat an infinity.
	if fc, err := SeasonalNaive([]float64{1, 2, 3, math.Inf(1)}, 2, 3); err != nil || fc[0] != 3 || fc[1] != 2 || fc[2] != 3 {
		t.Errorf("SeasonalNaive with Inf: %v %v", fc, err)
	}
}

// ------------------------------------------------- randomised cross-checks

// First-order optimality is a property of the answer, not of the algorithm:
// at the least-squares / ridge / maximum-likelihood solution the gradient of
// the documented objective vanishes. The checks below evaluate that gradient
// naively from the returned model on many random problems (random sizes,
// feature scales, offsets, weights, missing rows, constant columns).
func TestRefOptimalityConditions(t *testing.T) {
	r := &refRand{101}
	var nOLS, nRidge, nLogit, nKnownConst int
	for trial := 0; trial < 300; trial++ {
		n := 8 + int(r.u()*60)
		k := 1 + int(r.u()*4)
		scale := make([]float64, k)
		offset := make([]float64, k)
		for j := range scale {
			scale[j] = math.Pow(10, math.Floor(r.u()*9)-4) // 1e-4 .. 1e4
			offset[j] = scale[j] * math.Floor(r.u()*2000)  // features far from zero
		}
		constCol := -1
		if k > 1 && r.u() < 0.3 {
			constCol = int(r.u() * float64(k))
		}
		X := make([][]float64, n)
		y := make([]float64, n)
		yb := make([]float64, n)
		w := make([]float64, n)
		for i := range X {
			X[i] = make([]float64, k)
			eta := 0.3
			for j := range X[i] {
				z := r.norm()
				if j == constCol {
					z = 0
				}
				X[i][j] = offset[j] + scale[j]*z
				eta += z * (0.8 - 0.5*float64(j))
			}
			y[i] = 100 + 5*eta + 2*r.norm()
			if eta+1.5*r.norm() > 0 {
				yb[i] = 1
			}
			w[i] = 0.2 + 3*r.u()
			if r.u() < 0.05 {
				X[i][0] = math.NaN()
			}
			if r.u() < 0.05 {
				y[i], yb[i] = math.NaN(), math.NaN()
			}
			if r.u() < 0.05 {
				w[i] = 0
			}
		}
		used := func(i int, weighted bool) bool {
			if math.IsNaN(y[i]) || math.IsNaN(X[i][0]) {
				return false
			}
			return !weighted || w[i] > 0
		}

		// OLS / WLS: the weighted residuals are orthogonal to every column
		// of the design (and sum to zero with an intercept).
		if constCol < 0 {
			for _, weighted := range []bool{false, true} {
				for _, icpt := range []bool{true, false} {
					var o *OLSResult
					var err error
					if weighted {
						o, err = WLS(X, y, w, icpt)
					} else {
						o, err = OLS(X, y, icpt)
					}
					if err != nil {
						if errors.Is(err, ErrInsufficientData) {
							continue
						}
						t.Fatalf("trial %d: OLS weighted=%v icpt=%v: %v", trial, weighted, icpt, err)
					}
					g := make([]float64, k+1)
					norm := make([]float64, k+1)
					sse, rows := 0.0, 0
					for i := range X {
						if !used(i, weighted) {
							continue
						}
						wi := 1.0
						if weighted {
							wi = w[i]
						}
						e := y[i] - o.Predict(X[i])
						sse += wi * e * e
						rows++
						g[0] += wi * e
						norm[0] += wi * math.Abs(y[i])
						for j, v := range X[i] {
							g[j+1] += wi * e * v
							norm[j+1] += wi * math.Abs(y[i]) * math.Abs(v)
						}
					}
					if rows != o.N || o.DF != rows-len(o.Coef) {
						t.Fatalf("trial %d: OLS used %d rows / DF %d, want %d / %d", trial, o.N, o.DF, rows, rows-len(o.Coef))
					}
					for j := range g {
						if j == 0 && !icpt {
							continue
						}
						if math.Abs(g[j]) > 1e-8*norm[j] {
							t.Fatalf("trial %d: OLS weighted=%v icpt=%v: normal equation %d violated: %g (scale %g)", trial, weighted, icpt, j, g[j], norm[j])
						}
					}
					nOLS++
					refNear(t, "OLS SSE", o.SSE, sse, 1e-8)
					refNear(t, "OLS ResidStd", o.ResidStd, math.Sqrt(sse/float64(o.DF)), 1e-8)
					jsonSafe(t, "OLS random", o)
				}
			}
		}

		// Ridge: with z the standardised features and weights normalised to
		// sum n, the gradient of (1/n) sum w e^2 + lambda |b|^2 is
		// -2/n sum w e z + 2 lambda b.
		for _, lam := range []float64{0, 0.05, 2} {
			for _, weighted := range []bool{false, true} {
				var m *Model
				var err error
				if weighted {
					m, err = RidgeWeighted(X, y, w, nil, lam)
				} else {
					m, err = Ridge(X, y, nil, lam)
				}
				if err != nil {
					if errors.Is(err, ErrSingular) && lam == 0 {
						continue
					}
					t.Fatalf("trial %d: Ridge lam=%v weighted=%v: %v", trial, lam, weighted, err)
				}
				sw, rows := 0.0, 0
				for i := range X {
					if used(i, weighted) {
						rows++
						if weighted {
							sw += w[i]
						} else {
							sw++
						}
					}
				}
				g := make([]float64, k+1)
				for i := range X {
					if !used(i, weighted) {
						continue
					}
					wi := float64(rows) / sw
					if weighted {
						wi *= w[i]
					}
					e := y[i] - m.Predict(X[i])
					g[0] += wi * e / float64(rows)
					for j, v := range X[i] {
						g[j+1] += wi * e * (v - m.Mean[j]) / m.Scale[j] / float64(rows)
					}
				}
				if m.N != rows {
					t.Fatalf("trial %d: Ridge used %d rows, want %d", trial, m.N, rows)
				}
				for j := range g {
					want := 0.0
					if j > 0 {
						want = lam * m.Coef[j-1]
					}
					if j > 0 && j-1 == constCol {
						if m.Coef[j-1] != 0 {
							t.Fatalf("trial %d: constant feature has coefficient %v", trial, m.Coef[j-1])
						}
						continue
					}
					if math.Abs(g[j]-want) > 1e-7 {
						t.Fatalf("trial %d: Ridge lam=%v weighted=%v: gradient %d = %g, want %g", trial, lam, weighted, j, g[j], want)
					}
				}
				nRidge++
				if constCol >= 0 {
					nKnownConst++
				}
				jsonSafe(t, "Ridge random", m)
				if _, lo, hi := m.PredictInterval(X[n-1], 0.9); !(lo <= hi) {
					t.Fatalf("trial %d: Ridge interval %v..%v", trial, lo, hi)
				}
			}
		}

		// Logistic: gradient of mean log-loss + lambda/2 |b|^2.
		for _, lam := range []float64{0, 0.02, 1} {
			for _, weighted := range []bool{false, true} {
				opt := &LogisticOptions{Lambda: lam}
				if weighted {
					opt.Weights = w
				}
				m, err := Logistic(X, yb, nil, opt)
				if err != nil {
					if errors.Is(err, ErrDegenerate) || (lam == 0 && errors.Is(err, ErrSingular)) {
						continue
					}
					t.Fatalf("trial %d: Logistic lam=%v weighted=%v: %v", trial, lam, weighted, err)
				}
				jsonSafe(t, "Logistic random", m)
				if !m.Converged {
					if lam > 0 {
						t.Fatalf("trial %d: penalised logistic did not converge (iter %d)", trial, m.Iter)
					}
					continue // separable sample without penalty: no finite optimum
				}
				sw, rows := 0.0, 0
				for i := range X {
					if used(i, weighted) {
						rows++
						if weighted {
							sw += w[i]
						} else {
							sw++
						}
					}
				}
				g := make([]float64, k+1)
				ll := 0.0
				for i := range X {
					if !used(i, weighted) {
						continue
					}
					wi := float64(rows) / sw
					if weighted {
						wi *= w[i]
					}
					p := m.Predict(X[i])
					ll -= wi * (yb[i]*math.Log(p) + (1-yb[i])*math.Log(1-p)) / float64(rows)
					g[0] += wi * (p - yb[i]) / float64(rows)
					for j, v := range X[i] {
						g[j+1] += wi * (p - yb[i]) * (v - m.Mean[j]) / m.Scale[j] / float64(rows)
					}
				}
				for j := range g {
					if j > 0 {
						g[j] += lam * m.Coef[j-1]
					}
					if j > 0 && j-1 == constCol {
						continue
					}
					if math.Abs(g[j]) > 2e-6 {
						t.Fatalf("trial %d: Logistic lam=%v weighted=%v: gradient %d = %g (iter %d)", trial, lam, weighted, j, g[j], m.Iter)
					}
				}
				if !math.IsInf(ll, 0) && !math.IsNaN(ll) {
					refNear(t, "Logistic LogLoss", m.LogLoss, ll, 1e-7)
				}
				nLogit++
			}
		}
	}
	// Guard against a vacuous test: most of the random problems must have
	// produced a fit that was actually checked.
	t.Logf("checked %d OLS/WLS fits, %d ridge fits (%d with a constant column), %d logistic fits", nOLS, nRidge, nKnownConst, nLogit)
	if nOLS < 600 || nRidge < 1500 || nKnownConst < 200 || nLogit < 1200 {
		t.Errorf("too few fits checked: OLS %d, ridge %d (const %d), logistic %d", nOLS, nRidge, nKnownConst, nLogit)
	}
}

// The Kalman smoothers against a dense solve of the equivalent penalised
// least-squares problem, for random series with leading, interior and
// trailing gaps and random noise variances.
func TestRefKalmanDense(t *testing.T) {
	r := &refRand{111}
	for trial := 0; trial < 200; trial++ {
		n := 1 + int(r.u()*40)
		y := make([]float64, n)
		lev := 10 * r.norm()
		for i := range y {
			lev += r.norm()
			y[i] = lev + 2*r.norm()
			if r.u() < 0.25 {
				y[i] = math.NaN()
			}
		}
		if r.u() < 0.3 { // a long gap
			for i := n / 3; i < 2*n/3; i++ {
				y[i] = math.NaN()
			}
		}
		if r.u() < 0.3 && n > 2 {
			y[0], y[n-1] = math.NaN(), math.NaN()
		}
		first := -1
		for i, v := range y {
			if !math.IsNaN(v) {
				first = i
				break
			}
		}
		q := math.Pow(10, 3*r.u()-2)
		rr := math.Pow(10, 3*r.u()-2)
		sm := KalmanSmoothLevel(y, q, rr)
		lv, tr := KalmanSmoothTrend(y, q, q/10, rr)
		if len(sm) != n || len(lv) != n || len(tr) != n {
			t.Fatalf("trial %d: lengths", trial)
		}
		for i := 0; i < n; i++ {
			if first < 0 || i < first {
				if !math.IsNaN(sm[i]) || !math.IsNaN(lv[i]) || !math.IsNaN(tr[i]) {
					t.Fatalf("trial %d: output before the first sample must be NaN", trial)
				}
			}
		}
		if first < 0 {
			continue
		}
		m := n - first
		// Local level: minimise sum (y-x)^2/r + sum (x[i+1]-x[i])^2/q.
		a := NewMatrix(m, m)
		g := make([]float64, m)
		for i := 0; i < m; i++ {
			if v := y[first+i]; !math.IsNaN(v) {
				a.Data[i*m+i] += 1 / rr
				g[i] += v / rr
			}
			if i+1 < m {
				a.Data[i*m+i] += 1 / q
				a.Data[(i+1)*m+i+1] += 1 / q
				a.Data[i*m+i+1] -= 1 / q
				a.Data[(i+1)*m+i] -= 1 / q
			}
		}
		want, err := Solve(a, g)
		if err != nil {
			t.Fatalf("trial %d: dense level solve: %v", trial, err)
		}
		refSlice(t, "KalmanSmoothLevel dense", sm[first:], want, 1e-8)

		// Local linear trend: unknowns l[0..m), b[0..m).
		ql, qt := q, q/10
		v0 := 1e6 * math.Max(rr, math.Max(ql, qt))
		a = NewMatrix(2*m, 2*m)
		g = make([]float64, 2*m)
		add := func(idx []int, coef []float64, rhs, wt float64) {
			for p, ip := range idx {
				g[ip] += wt * coef[p] * rhs
				for s, is := range idx {
					a.Data[ip*2*m+is] += wt * coef[p] * coef[s]
				}
			}
		}
		for i := 0; i < m; i++ {
			if v := y[first+i]; !math.IsNaN(v) {
				add([]int{i}, []float64{1}, v, 1/rr)
			}
			if i+1 < m {
				add([]int{i + 1, i, m + i}, []float64{1, -1, -1}, 0, 1/ql)
				add([]int{m + i + 1, m + i}, []float64{1, -1}, 0, 1/qt)
			}
		}
		add([]int{m}, []float64{1}, 0, 1/v0)
		sol, err := Solve(a, g)
		if err != nil {
			t.Fatalf("trial %d: dense trend solve: %v", trial, err)
		}
		// The dense system is badly scaled (prior weight 1e-6): compare
		// relative to the size of the data.
		tol := 2e-5 * (1 + math.Abs(lev))
		for i := 0; i < m; i++ {
			if math.Abs(lv[first+i]-sol[i]) > tol || math.Abs(tr[first+i]-sol[m+i]) > tol {
				t.Fatalf("trial %d (n=%d q=%g r=%g): trend smoother at %d: level %g slope %g, dense %g %g\ny=%v",
					trial, n, q, rr, first+i, lv[first+i], tr[first+i], sol[i], sol[m+i], y)
			}
		}
	}
}
