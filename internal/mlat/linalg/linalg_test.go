package linalg

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func randSPD(rng *rand.Rand, n int) []float64 {
	b := make([]float64, n*n)
	for i := range b {
		b[i] = rng.NormFloat64()
	}
	a := make([]float64, n*n)
	MatMulAtB(a, b, b, n, n, n)
	AddDiag(a, n, float64(n)*0.1+0.1)
	return a
}

func randMat(rng *rand.Rand, m, n int) []float64 {
	a := make([]float64, m*n)
	for i := range a {
		a[i] = rng.NormFloat64()
	}
	return a
}

func maxAbsDiff(a, b []float64) float64 {
	var d float64
	for i := range a {
		d = math.Max(d, math.Abs(a[i]-b[i]))
	}
	return d
}

func clone(a []float64) []float64 { return append([]float64(nil), a...) }

func TestDotAxpyNorm(t *testing.T) {
	for n := 0; n < 11; n++ {
		a, b := make([]float64, n), make([]float64, n)
		var want float64
		for i := range a {
			a[i], b[i] = float64(i+1), float64(2*i-3)
			want += a[i] * b[i]
		}
		if got := Dot(a, b); got != want {
			t.Errorf("Dot n=%d: %v want %v", n, got, want)
		}
		y := clone(b)
		Axpy(2, a, y)
		for i := range y {
			if y[i] != b[i]+2*a[i] {
				t.Fatalf("Axpy n=%d", n)
			}
		}
	}
	if v := Norm2([]float64{3, 4}); v != 5 {
		t.Errorf("Norm2 = %v", v)
	}
	if v := Norm2([]float64{3e200, 4e200}); math.Abs(v-5e200)/5e200 > 1e-15 {
		t.Errorf("Norm2 overflow guard = %v", v)
	}
	if Norm2(nil) != 0 {
		t.Error("Norm2(nil)")
	}
}

func TestMatMulFamily(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	a := randMat(rng, 3, 4)
	b := randMat(rng, 4, 5)
	c := make([]float64, 15)
	MatMul(c, a, b, 3, 4, 5)
	for i := 0; i < 3; i++ {
		for j := 0; j < 5; j++ {
			var s float64
			for k := 0; k < 4; k++ {
				s += a[i*4+k] * b[k*5+j]
			}
			if math.Abs(c[i*5+j]-s) > 1e-12 {
				t.Fatal("MatMul")
			}
		}
	}
	bt := make([]float64, 20)
	Transpose(bt, b, 4, 5)
	c2 := make([]float64, 15)
	MatMulABt(c2, a, bt, 3, 4, 5)
	if maxAbsDiff(c, c2) > 1e-12 {
		t.Error("MatMulABt")
	}
	at := make([]float64, 12)
	Transpose(at, a, 3, 4)
	MatMulAtB(c2, at, b, 4, 3, 5)
	if maxAbsDiff(c, c2) > 1e-12 {
		t.Error("MatMulAtB")
	}
	x := []float64{1, -2, 3, 0.5}
	y := make([]float64, 3)
	MatVec(y, a, x, 3, 4)
	y2 := make([]float64, 3)
	MatTVec(y2, at, x, 4, 3)
	if maxAbsDiff(y, y2) > 1e-12 {
		t.Error("MatVec/MatTVec")
	}
	id := make([]float64, 9)
	Identity(id, 3)
	if id[0] != 1 || id[4] != 1 || id[1] != 0 {
		t.Error("Identity")
	}
}

func TestCholesky(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for _, n := range []int{1, 2, 3, 4, 7, 9, 33, 100} {
		a := randSPD(rng, n)
		l := clone(a)
		if err := Cholesky(l, n); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if l[i*n+j] != 0 {
					t.Fatalf("upper not zero")
				}
			}
		}
		lt := make([]float64, n*n)
		Transpose(lt, l, n, n)
		r := make([]float64, n*n)
		MatMul(r, l, lt, n, n, n)
		if d := maxAbsDiff(r, a); d > 1e-10*float64(n) {
			t.Errorf("n=%d: |LLᵀ-A| = %g", n, d)
		}
		// Solve.
		x := make([]float64, n)
		for i := range x {
			x[i] = rng.NormFloat64()
		}
		b := make([]float64, n)
		MatVec(b, a, x, n, n)
		CholSolve(l, n, b)
		if d := maxAbsDiff(b, x); d > 1e-9 {
			t.Errorf("n=%d: solve error %g", n, d)
		}
		// Multi solve.
		k := 3
		X := randMat(rng, n, k)
		B := make([]float64, n*k)
		MatMul(B, a, X, n, n, k)
		CholSolveMulti(l, n, B, k)
		if d := maxAbsDiff(B, X); d > 1e-9 {
			t.Errorf("n=%d: multi solve error %g", n, d)
		}
		// Log det vs LU-free product of pivots squared.
		ld := CholLogDet(l, n)
		var s float64
		for i := 0; i < n; i++ {
			s += 2 * math.Log(l[i*n+i])
		}
		if math.Abs(ld-s) > 1e-12 {
			t.Error("logdet")
		}
	}
	// Known 3×3.
	a := []float64{4, 12, -16, 12, 37, -43, -16, -43, 98}
	if err := Cholesky(a, 3); err != nil {
		t.Fatal(err)
	}
	want := []float64{2, 0, 0, 6, 1, 0, -8, 5, 3}
	if maxAbsDiff(a, want) > 1e-12 {
		t.Errorf("known factor %v", a)
	}
	if math.Abs(CholLogDet(a, 3)-math.Log(36)) > 1e-12 {
		t.Error("known logdet")
	}
}

// TestBlockedKernels checks the register-blocked Cholesky and inverse
// against the simple reference forms for every small size (odd and even).
func TestBlockedKernels(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	for n := 1; n <= 41; n++ {
		a := randSPD(rng, n)
		l1, l2 := clone(a), clone(a)
		if err := choleskySimple(l1, n); err != nil {
			t.Fatal(err)
		}
		if err := choleskyBlocked(l2, n); err != nil {
			t.Fatal(err)
		}
		if d := maxAbsDiff(l1, l2); d > 1e-12 {
			t.Errorf("n=%d: blocked Cholesky differs by %g", n, d)
		}
		// Reference inverse: InvertLower then XᵀX.
		x := clone(l1)
		if err := InvertLower(x, n, nil); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				x[i*n+j] = 0
			}
		}
		ref := make([]float64, n*n)
		MatMulAtB(ref, x, x, n, n, n)
		inv := clone(l1)
		if err := CholToInverse(inv, n, nil); err != nil {
			t.Fatal(err)
		}
		if d := maxAbsDiff(inv, ref); d > 1e-10 {
			t.Errorf("n=%d: blocked inverse differs by %g", n, d)
		}
		// Failure detection in the blocked path.
		bad := clone(a)
		bad[(n-1)*n+n-1] = -1
		if n >= 2 && choleskyBlocked(bad, n) == nil {
			t.Errorf("n=%d: blocked Cholesky accepted an indefinite matrix", n)
		}
	}
}

func TestCholeskyFailures(t *testing.T) {
	for _, a := range [][]float64{
		{1, 2, 2, 1},          // indefinite
		{0, 0, 0, 0},          // zero
		{1, 0, 0, math.NaN()}, // NaN
		{-1, 0, 0, 1},         // negative
	} {
		if err := Cholesky(clone(a), 2); !errors.Is(err, ErrNotPositiveDefinite) {
			t.Errorf("%v: err = %v", a, err)
		}
	}
	// Semi-definite (rank 1) rescued by jitter.
	orig := []float64{1, 1, 1, 1}
	a := make([]float64, 4)
	j, err := CholeskyJitter(a, orig, 2, 1e-12, 6)
	if err != nil || j <= 0 {
		t.Errorf("jitter: %v %v", j, err)
	}
	j, err = CholeskyJitter(a, []float64{2, 0, 0, 3}, 2, 1e-12, 6)
	if err != nil || j != 0 {
		t.Errorf("no jitter needed: %v %v", j, err)
	}
	if _, err = CholeskyJitter(a, []float64{-5, 0, 0, -5}, 2, 1e-12, 3); err == nil {
		t.Error("negative definite should fail")
	}
}

func TestSPDInverse(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, n := range []int{1, 2, 4, 6, 9, 50, 200} {
		a := randSPD(rng, n)
		inv := clone(a)
		if err := SPDInverse(inv, n, nil); err != nil {
			t.Fatal(err)
		}
		p := make([]float64, n*n)
		MatMul(p, a, inv, n, n, n)
		id := make([]float64, n*n)
		Identity(id, n)
		if d := maxAbsDiff(p, id); d > 1e-9 {
			t.Errorf("n=%d: |A·A⁻¹ - I| = %g", n, d)
		}
		for i := 0; i < n; i++ {
			for j := 0; j < i; j++ {
				if inv[i*n+j] != inv[j*n+i] {
					t.Fatal("inverse not symmetric")
				}
			}
		}
	}
	// Only the lower triangle is read.
	a := []float64{4, 999, 2, 3}
	if err := SPDInverse(a, 2, nil); err != nil {
		t.Fatal(err)
	}
	want := []float64{3.0 / 8, -2.0 / 8, -2.0 / 8, 4.0 / 8}
	if maxAbsDiff(a, want) > 1e-12 {
		t.Errorf("lower-only inverse %v", a)
	}
	if err := SPDInverse([]float64{1, 2, 2, 1}, 2, nil); err == nil {
		t.Error("indefinite inverse should fail")
	}
}

func TestInvertLowerAndTriSolves(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	n := 12
	l := randSPD(rng, n)
	if err := Cholesky(l, n); err != nil {
		t.Fatal(err)
	}
	x := clone(l)
	if err := InvertLower(x, n, nil); err != nil {
		t.Fatal(err)
	}
	p := make([]float64, n*n)
	MatMul(p, l, x, n, n, n)
	id := make([]float64, n*n)
	Identity(id, n)
	if d := maxAbsDiff(p, id); d > 1e-12 {
		t.Errorf("L·L⁻¹ - I = %g", d)
	}
	b := randMat(rng, n, 1)
	y := clone(b)
	SolveLower(l, n, y)
	chk := make([]float64, n)
	MatVec(chk, l, y, n, n)
	if maxAbsDiff(chk, b) > 1e-12 {
		t.Error("SolveLower")
	}
	y = clone(b)
	SolveLowerT(l, n, y)
	lt := make([]float64, n*n)
	Transpose(lt, l, n, n)
	MatVec(chk, lt, y, n, n)
	if maxAbsDiff(chk, b) > 1e-12 {
		t.Error("SolveLowerT")
	}
	if InvertLower([]float64{0, 0, 1, 1}, 2, nil) == nil {
		t.Error("singular lower")
	}
}

func TestLUSolve(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for _, n := range []int{1, 3, 4, 10} {
		a := randMat(rng, n, n)
		x := randMat(rng, n, 1)
		b := make([]float64, n)
		MatVec(b, a, x, n, n)
		if err := LUSolve(clone(a), n, b, nil); err != nil {
			t.Fatal(err)
		}
		if d := maxAbsDiff(b, x); d > 1e-9 {
			t.Errorf("n=%d LU error %g", n, d)
		}
	}
	// Needs pivoting.
	a := []float64{0, 1, 1, 0}
	b := []float64{2, 3}
	if err := LUSolve(a, 2, b, nil); err != nil || b[0] != 3 || b[1] != 2 {
		t.Errorf("pivot: %v %v", b, err)
	}
	if LUSolve([]float64{1, 2, 2, 4}, 2, []float64{1, 1}, nil) == nil {
		t.Error("singular LU")
	}
}

func TestQR(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	for _, sz := range [][2]int{{1, 1}, {4, 4}, {5, 3}, {30, 4}, {200, 9}} {
		m, n := sz[0], sz[1]
		a := randMat(rng, m, n)
		b := randMat(rng, m, 1)
		// Reference: normal equations with Cholesky.
		ata := make([]float64, n*n)
		MatMulAtB(ata, a, a, m, n, n)
		atb := make([]float64, n)
		MatTVec(atb, a, b, m, n)
		ref := clone(atb)
		l := clone(ata)
		if err := Cholesky(l, n); err != nil {
			t.Fatal(err)
		}
		CholSolve(l, n, ref)

		qr := clone(a)
		tau := make([]float64, n)
		QR(qr, m, n, tau, nil)
		x := make([]float64, n)
		bb := clone(b)
		res, err := QRSolve(qr, m, n, tau, bb, x)
		if err != nil {
			t.Fatal(err)
		}
		if d := maxAbsDiff(x, ref); d > 1e-9 {
			t.Errorf("%dx%d: QR vs normal equations %g", m, n, d)
		}
		ax := make([]float64, m)
		MatVec(ax, a, x, m, n)
		for i := range ax {
			ax[i] -= b[i]
		}
		if math.Abs(Norm2(ax)-res) > 1e-9 {
			t.Errorf("%dx%d: residual %v vs %v", m, n, res, Norm2(ax))
		}
		// Qᵀ preserves norms.
		v := randMat(rng, m, 1)
		nv := Norm2(v)
		QRApplyQT(qr, m, n, tau, v)
		if math.Abs(Norm2(v)-nv) > 1e-10 {
			t.Error("Qᵀ not orthogonal")
		}
		// Covariance = (AᵀA)⁻¹.
		cov := make([]float64, n*n)
		if err := QRCovariance(qr, m, n, cov, nil); err != nil {
			t.Fatal(err)
		}
		inv := clone(ata)
		if err := SPDInverse(inv, n, nil); err != nil {
			t.Fatal(err)
		}
		if d := maxAbsDiff(cov, inv); d > 1e-9 {
			t.Errorf("%dx%d: QR covariance %g", m, n, d)
		}
	}
	// Rank deficient: two equal columns.
	a := []float64{1, 1, 2, 2, 3, 3, 4, 4}
	tau := make([]float64, 2)
	QR(a, 4, 2, tau, nil)
	if QRRank(a, 4, 2, 0) != 1 {
		t.Error("rank")
	}
	if _, err := QRSolve(a, 4, 2, tau, []float64{1, 2, 3, 4}, make([]float64, 2)); !errors.Is(err, ErrRankDeficient) {
		t.Errorf("rank deficient solve err = %v", err)
	}
	// Exact fit: zero residual.
	a = []float64{1, 0, 0, 1, 1, 1}
	tau = make([]float64, 2)
	QR(a, 3, 2, tau, nil)
	x := make([]float64, 2)
	res, err := QRSolve(a, 3, 2, tau, []float64{2, 3, 5}, x)
	if err != nil || math.Abs(res) > 1e-12 || math.Abs(x[0]-2) > 1e-12 || math.Abs(x[1]-3) > 1e-12 {
		t.Errorf("exact fit x=%v res=%v err=%v", x, res, err)
	}
}

func TestSmallTypes(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var a Mat4
	copy(a[:], randSPD(rng, 4))
	inv, err := a.InvSPD()
	if err != nil {
		t.Fatal(err)
	}
	p := a.Mul(inv)
	id := Identity4()
	if maxAbsDiff(p[:], id[:]) > 1e-12 {
		t.Error("Mat4 inverse")
	}
	b := Vec4{1, 2, 3, 4}
	x, err := a.SolveSPD(b)
	if err != nil {
		t.Fatal(err)
	}
	ax := a.MulVec(x)
	if maxAbsDiff(ax[:], b[:]) > 1e-12 {
		t.Error("Mat4 SolveSPD")
	}
	x2, err := a.Solve(b)
	if err != nil || maxAbsDiff(x2[:], x[:]) > 1e-12 {
		t.Error("Mat4 Solve")
	}
	if q := a.Quad(x); math.Abs(q-x.Dot(b)) > 1e-10 {
		t.Error("Quad")
	}
	l, err := a.Chol()
	if err != nil {
		t.Fatal(err)
	}
	llt := l.Mul(l.T())
	if maxAbsDiff(llt[:], a[:]) > 1e-12 {
		t.Error("Mat4 Chol")
	}
	// Congruence equals the generic routine.
	var r Mat4
	copy(r[:], randMat(rng, 4, 4))
	c := r.Congruence(a)
	c2 := make([]float64, 16)
	Congruence(c2, r[:], a[:], 4, 4, nil)
	if maxAbsDiff(c[:], c2) > 1e-12 {
		t.Error("Congruence")
	}
	s := a.Add(a).Sub(a).Scale(2)
	a2 := a.Scale(2)
	if maxAbsDiff(s[:], a2[:]) > 1e-12 {
		t.Error("Add/Sub/Scale")
	}
	asym := r.Symmetrize()
	if asym.At(0, 1) != asym.At(1, 0) {
		t.Error("Symmetrize")
	}
	o := Vec4{1, 2, 0, 0}.Outer(Vec4{0, 1, 0, 0})
	if o.At(1, 1) != 2 || o.At(0, 0) != 0 {
		t.Error("Outer")
	}
	var m9 Mat9
	copy(m9[:], randSPD(rng, 9))
	i9, err := m9.InvSPD()
	if err != nil {
		t.Fatal(err)
	}
	p9 := m9.Mul(i9)
	id9 := Identity9()
	if maxAbsDiff(p9[:], id9[:]) > 1e-10 {
		t.Error("Mat9 inverse")
	}
	var m6 Mat6
	copy(m6[:], randSPD(rng, 6))
	if _, err := m6.InvSPD(); err != nil {
		t.Error(err)
	}
	m3 := Identity3()
	m3.Set(0, 0, -1)
	if _, err := m3.Chol(); err == nil {
		t.Error("Mat3 indefinite Chol should fail")
	}
}

// TestLevenbergMarquardt solves a small nonlinear problem (2-D position
// and clock offset from pseudoranges, the shape of the MLAT fix) and checks
// convergence and the covariance.
func TestLevenbergMarquardt(t *testing.T) {
	stations := [][2]float64{{0, 0}, {10000, 0}, {0, 12000}, {9000, 11000}, {-5000, 4000}}
	truth := [3]float64{3000, 4000, 250} // x, y, offset (m)
	rng := rand.New(rand.NewSource(8))
	meas := make([]float64, len(stations))
	const sigma = 1.0
	for i, s := range stations {
		meas[i] = math.Hypot(truth[0]-s[0], truth[1]-s[1]) + truth[2] + rng.NormFloat64()*sigma
	}
	x := []float64{-20000, 30000, 0} // poor start
	var nrm Normal
	build := func(x []float64) {
		nrm.Reset(3)
		for i, s := range stations {
			dx, dy := x[0]-s[0], x[1]-s[1]
			r := math.Hypot(dx, dy)
			res := r + x[2] - meas[i]
			nrm.AddRow([]float64{dx / r, dy / r, 1}, res, 1/(sigma*sigma))
		}
	}
	cost := func(x []float64) float64 {
		var c float64
		for i, s := range stations {
			res := math.Hypot(x[0]-s[0], x[1]-s[1]) + x[2] - meas[i]
			c += 0.5 * res * res / (sigma * sigma)
		}
		return c
	}
	build(x)
	var lm LM
	lm.Init(&nrm, 1)
	dx := make([]float64, 3)
	trial := make([]float64, 3)
	it := 0
	for ; it < 100; it++ {
		pred, err := nrm.Step(lm.Lambda, 1e-9, dx)
		if err != nil {
			t.Fatal(err)
		}
		for i := range x {
			trial[i] = x[i] + dx[i]
		}
		if lm.Update(GainRatio(nrm.Cost, cost(trial), pred)) {
			copy(x, trial)
			build(x)
		}
		if Converged(dx, x, 1e-10) {
			break
		}
	}
	if math.Hypot(x[0]-truth[0], x[1]-truth[1]) > 10 {
		t.Fatalf("LM did not converge: %v after %d iterations", x, it)
	}
	cov := make([]float64, 9)
	if err := nrm.Covariance(cov); err != nil {
		t.Fatal(err)
	}
	if !(cov[0] > 0 && cov[4] > 0 && cov[8] > 0) || cov[1] != cov[3] {
		t.Errorf("covariance %v", cov)
	}
	if nrm.Rows != len(stations) {
		t.Error("rows")
	}
	// Gauss-Newton step at the optimum is ~zero.
	if _, err := nrm.Step(0, 0, dx); err != nil || Norm2(dx) > 1e-6 {
		t.Errorf("GN step at optimum %v", dx)
	}
	// LM bookkeeping.
	l := LM{}
	l.InitLambda(1)
	l.Update(-1)
	if l.Lambda != 2 || l.Nu != 4 {
		t.Errorf("reject update %+v", l)
	}
	l.Update(1)
	if l.Lambda != 2.0/3 || l.Nu != 2 {
		t.Errorf("accept update %+v", l)
	}
	if GainRatio(10, 5, 5) != 1 || GainRatio(10, 11, 0) != -1 {
		t.Error("GainRatio")
	}
}

func TestWhiten(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	m, n := 6, 4
	sigma := randSPD(rng, m)
	l := clone(sigma)
	if err := Cholesky(l, m); err != nil {
		t.Fatal(err)
	}
	r := randMat(rng, m, 1)
	J := randMat(rng, m, n)
	// Reference rᵀΣ⁻¹r and JᵀΣ⁻¹J.
	si := clone(sigma)
	if err := SPDInverse(si, m, nil); err != nil {
		t.Fatal(err)
	}
	tmp := make([]float64, m)
	MatVec(tmp, si, r, m, m)
	want := Dot(r, tmp)
	if got := MahalanobisSq(l, m, r, nil); math.Abs(got-want) > 1e-9 {
		t.Errorf("Mahalanobis %v want %v", got, want)
	}
	siJ := make([]float64, m*n)
	MatMul(siJ, si, J, m, m, n)
	wantH := make([]float64, n*n)
	MatMulAtB(wantH, J, siJ, m, n, n)

	rw, Jw := clone(r), clone(J)
	Whiten(l, m, rw, Jw, n)
	if math.Abs(Dot(rw, rw)-want) > 1e-9 {
		t.Error("whitened residual norm")
	}
	gotH := make([]float64, n*n)
	MatMulAtB(gotH, Jw, Jw, m, n, n)
	if d := maxAbsDiff(gotH, wantH); d > 1e-9 {
		t.Errorf("whitened JᵀJ differs by %g", d)
	}
	// Normal from whitened rows equals the same H.
	var nrm Normal
	nrm.Reset(n)
	for i := 0; i < m; i++ {
		nrm.AddRow(Jw[i*n:i*n+n], rw[i], 1)
	}
	h := nrm.hessian()
	if d := maxAbsDiff(h[:n*n], wantH); d > 1e-9 {
		t.Errorf("Normal H differs by %g", d)
	}
}

func TestCovOps(t *testing.T) {
	e := ErrorEllipse(100, 0, 25)
	if e.SemiMajor != 10 || e.SemiMinor != 5 || e.AzimuthDeg != 90 {
		t.Errorf("east-west ellipse %+v", e)
	}
	e = ErrorEllipse(25, 0, 100)
	if e.SemiMajor != 10 || e.AzimuthDeg != 0 {
		t.Errorf("north-south ellipse %+v", e)
	}
	e = ErrorEllipse(50, 40, 50) // major axis along NE
	if math.Abs(e.AzimuthDeg-45) > 1e-9 || math.Abs(e.SemiMajor-math.Sqrt(90)) > 1e-12 || math.Abs(e.SemiMinor-math.Sqrt(10)) > 1e-12 {
		t.Errorf("NE ellipse %+v", e)
	}
	e = ErrorEllipse(50, -40, 50) // along NW = 135°
	if math.Abs(e.AzimuthDeg-135) > 1e-9 {
		t.Errorf("NW ellipse %+v", e)
	}
	if HorizontalSigma(9, 16) != 5 {
		t.Error("HorizontalSigma")
	}
	l1, l2 := SymEigen2(2, 0, 1)
	if l1 != 2 || l2 != 1 {
		t.Error("SymEigen2")
	}
	a := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9}
	Symmetrize(a, 3)
	if a[1] != 3 || a[3] != 3 || a[2] != 5 {
		t.Errorf("Symmetrize %v", a)
	}
	sub := make([]float64, 4)
	Submatrix(sub, a, 3, []int{0, 2}, []int{0, 2})
	if sub[0] != 1 || sub[1] != 5 || sub[3] != 9 {
		t.Errorf("Submatrix %v", sub)
	}
}

func TestAccum(t *testing.T) {
	rng := rand.New(rand.NewSource(10))
	const N = 12
	acc := NewAccum(N)
	J := make([]float64, N*N)
	g := make([]float64, N)
	for obs := 0; obs < 50; obs++ {
		k := 1 + rng.Intn(4)
		m := 1 + rng.Intn(3)
		idx := rng.Perm(N)[:k]
		if obs%7 == 0 && k > 1 {
			idx[1] = idx[0] // duplicate index
		}
		h := randMat(rng, m, k)
		var rinv []float64
		if obs%3 != 0 {
			rinv = randSPD(rng, m)
		}
		nu := randMat(rng, m, 1)
		s := 1.0
		if obs%5 == 0 {
			s = 0.3
		}
		acc.AddScaled(s, idx, h, m, rinv, nu)
		// Dense reference: embed H into m×N.
		H := make([]float64, m*N)
		for r := 0; r < m; r++ {
			for p := 0; p < k; p++ {
				H[r*N+idx[p]] += h[r*k+p]
			}
		}
		R := make([]float64, m*m)
		if rinv != nil {
			copy(R, rinv)
		} else {
			Identity(R, m)
		}
		for i := range R {
			R[i] *= s
		}
		RH := make([]float64, m*N)
		MatMul(RH, R, H, m, m, N)
		dJ := make([]float64, N*N)
		MatMulAtB(dJ, H, RH, m, N, N)
		for i := range J {
			J[i] += dJ[i]
		}
		dg := make([]float64, N)
		MatTVec(dg, RH, nu, m, N)
		for i := range g {
			g[i] += dg[i]
		}
	}
	if d := maxAbsDiff(acc.J, J); d > 1e-9 {
		t.Errorf("accumulated J differs by %g", d)
	}
	if d := maxAbsDiff(acc.G, g); d > 1e-9 {
		t.Errorf("accumulated g differs by %g", d)
	}
	if acc.Count != 50 {
		t.Error("count")
	}
	acc.AddDiagPrior(3, 4, 2)
	if math.Abs(acc.J[3*N+3]-J[3*N+3]-4) > 1e-9 || math.Abs(acc.G[3]-g[3]-8) > 1e-9 {
		t.Error("AddDiagPrior")
	}
	acc.Reset()
	if Norm2(acc.J) != 0 || Norm2(acc.G) != 0 || acc.Count != 0 {
		t.Error("Reset")
	}
}

func TestNoAllocs(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	n := 20
	a := randSPD(rng, n)
	w := make([]float64, n*n)
	work := make([]float64, n)
	b := make([]float64, n)
	check := func(name string, f func()) {
		t.Helper()
		if allocs := testing.AllocsPerRun(20, f); allocs != 0 {
			t.Errorf("%s allocates %v times", name, allocs)
		}
	}
	check("Cholesky+solve", func() {
		copy(w, a)
		_ = Cholesky(w, n)
		CholSolve(w, n, b)
	})
	check("SPDInverse", func() {
		copy(w, a)
		_ = SPDInverse(w, n, work)
	})
	qa := randMat(rng, 30, 4)
	q := make([]float64, len(qa))
	tau, qw, x, rhs, cov := make([]float64, 4), make([]float64, 4), make([]float64, 4), make([]float64, 30), make([]float64, 16)
	check("QR", func() {
		copy(q, qa)
		QR(q, 30, 4, tau, qw)
		_, _ = QRSolve(q, 30, 4, tau, rhs, x)
		_ = QRCovariance(q, 30, 4, cov, qw)
	})
	var m4 Mat4
	copy(m4[:], randSPD(rng, 4))
	var m9 Mat9
	copy(m9[:], randSPD(rng, 9))
	check("Mat4/Mat9", func() {
		i4, _ := m4.InvSPD()
		_, _ = i4.SolveSPD(Vec4{1, 2, 3, 4})
		_, _ = m4.Solve(Vec4{1, 2, 3, 4})
		_ = m4.Congruence(i4)
		_, _ = m9.InvSPD()
	})
	var nrm Normal
	row := []float64{1, 2, 3, 4}
	dx := make([]float64, 4)
	cv := make([]float64, 16)
	check("Normal", func() {
		nrm.Reset(4)
		for i := 0; i < 8; i++ {
			row[i%4] += 1
			nrm.AddRow(row, 0.5, 2)
		}
		_, _ = nrm.Step(1e-3, 1e-9, dx)
		_ = nrm.Covariance(cv)
	})
	acc := NewAccum(30)
	idx := []int{3, 7, 11}
	h := randMat(rng, 2, 3)
	rinv := []float64{2, 0.1, 0.1, 3}
	nu := []float64{1, -1}
	acc.Add(idx, h, 2, rinv, nu) // warm the scratch buffer
	check("Accum", func() {
		acc.Add(idx, h, 2, rinv, nu)
		acc.AddScaled(0.5, idx, h, 2, nil, nu)
	})
	l := clone(a[:16])
	copy(l, randSPD(rng, 4))
	_ = Cholesky(l, 4)
	r4 := []float64{1, 2, 3, 4}
	J4 := randMat(rng, 4, 3)
	check("Whiten/Mahalanobis", func() {
		Whiten(l, 4, r4, J4, 3)
		_ = MahalanobisSq(l, 4, r4, work)
	})
}

func benchSPD(b *testing.B, n int, f func(a []float64, n int, work []float64)) {
	rng := rand.New(rand.NewSource(1))
	a := randSPD(rng, n)
	w := make([]float64, n*n)
	work := make([]float64, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(w, a)
		f(w, n, work)
	}
}

func BenchmarkCholesky(b *testing.B) {
	for _, n := range []int{4, 9, 100, 600} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			benchSPD(b, n, func(a []float64, n int, _ []float64) { _ = Cholesky(a, n) })
		})
	}
}

// BenchmarkSPDInverse covers the clock-filter sizes: DESIGN §9.2.3 has
// n ≤ 700 states (200 stations) per component.
func BenchmarkSPDInverse(b *testing.B) {
	for _, n := range []int{4, 9, 200, 600, 700} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			benchSPD(b, n, func(a []float64, n int, work []float64) { _ = SPDInverse(a, n, work) })
		})
	}
}

func BenchmarkCholSolve(b *testing.B) {
	for _, n := range []int{9, 600} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			rng := rand.New(rand.NewSource(1))
			l := randSPD(rng, n)
			_ = Cholesky(l, n)
			x := randMat(rng, n, 1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				CholSolve(l, n, x)
			}
		})
	}
}

func BenchmarkQR(b *testing.B) {
	for _, sz := range [][2]int{{12, 4}, {60, 4}, {600, 9}} {
		m, n := sz[0], sz[1]
		b.Run(fmt.Sprintf("%dx%d", m, n), func(b *testing.B) {
			rng := rand.New(rand.NewSource(1))
			a := randMat(rng, m, n)
			rhs0 := randMat(rng, m, 1)
			q := make([]float64, m*n)
			rhs := make([]float64, m)
			tau, work, x := make([]float64, n), make([]float64, n), make([]float64, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				copy(q, a)
				copy(rhs, rhs0)
				QR(q, m, n, tau, work)
				_, _ = QRSolve(q, m, n, tau, rhs, x)
			}
		})
	}
}

func BenchmarkMat4InvSPD(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	var m Mat4
	copy(m[:], randSPD(rng, 4))
	b.ReportAllocs()
	var s float64
	for i := 0; i < b.N; i++ {
		inv, _ := m.InvSPD()
		s += inv[0]
	}
	_ = s
}

func BenchmarkMat9InvSPD(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	var m Mat9
	copy(m[:], randSPD(rng, 9))
	b.ReportAllocs()
	var s float64
	for i := 0; i < b.N; i++ {
		inv, _ := m.InvSPD()
		s += inv[0]
	}
	_ = s
}

// BenchmarkLMIteration is one LM iteration of a 10-station fix: build the
// 4-parameter normal equations and solve the damped step.
func BenchmarkLMIteration(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	rows := randMat(rng, 10, 4)
	res := randMat(rng, 10, 1)
	var nrm Normal
	dx := make([]float64, 4)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		nrm.Reset(4)
		for r := 0; r < 10; r++ {
			nrm.AddRow(rows[r*4:r*4+4], res[r], 1)
		}
		_, _ = nrm.Step(1e-3, 1e-12, dx)
	}
}

func BenchmarkAccumAdd(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	acc := NewAccum(700)
	h := randMat(rng, 3, 8)
	rinv := randSPD(rng, 3)
	nu := randMat(rng, 3, 1)
	idx := []int{0, 1, 90, 91, 300, 301, 650, 651}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		acc.Add(idx, h, 3, rinv, nu)
	}
}
