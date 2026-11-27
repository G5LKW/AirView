package linalg

import "math"

// Congruence computes dst = A·P·Aᵀ for A k×n and symmetric P n×n
// (covariance propagation). work needs k·n elements (nil allocates). dst
// (k×k) must not alias A, P or work.
func Congruence(dst, A, P []float64, k, n int, work []float64) {
	checkLen("A", A, k*n)
	checkLen("P", P, n*n)
	checkLen("dst", dst, k*k)
	if work == nil {
		work = make([]float64, k*n)
	}
	checkLen("work", work, k*n)
	MatMul(work, A, P, k, n, n) // T = A·P (k×n)
	for i := 0; i < k; i++ {
		ti := work[i*n : i*n+n]
		for j := 0; j <= i; j++ {
			v := Dot(ti, A[j*n:j*n+n])
			dst[i*k+j] = v
			dst[j*k+i] = v
		}
	}
}

// Symmetrize replaces the n×n matrix a by (a + aᵀ)/2.
func Symmetrize(a []float64, n int) {
	checkLen("a", a, n*n)
	for i := 0; i < n; i++ {
		for j := 0; j < i; j++ {
			v := (a[i*n+j] + a[j*n+i]) / 2
			a[i*n+j], a[j*n+i] = v, v
		}
	}
}

// AddDiag adds v to every diagonal element of the n×n matrix a.
func AddDiag(a []float64, n int, v float64) {
	for i := 0; i < n; i++ {
		a[i*n+i] += v
	}
}

// Submatrix extracts the block a[rows, cols] of the n-column matrix a into
// dst (len(rows)×len(cols), row-major). With rows == cols on a covariance
// this is the marginal covariance of those states.
func Submatrix(dst, a []float64, n int, rows, cols []int) {
	nc := len(cols)
	checkLen("dst", dst, len(rows)*nc)
	for i, r := range rows {
		for j, c := range cols {
			dst[i*nc+j] = a[r*n+c]
		}
	}
}

// MahalanobisSq returns vᵀ·Σ⁻¹·v given the lower Cholesky factor L of Σ
// (n×n). work needs n elements (nil allocates); v is not modified.
func MahalanobisSq(l []float64, n int, v, work []float64) float64 {
	if work == nil {
		work = make([]float64, n)
	}
	checkLen("work", work, n)
	w := work[:n]
	copy(w, v[:n])
	SolveLower(l, n, w)
	return Dot(w, w)
}

// Ellipse describes a 2-D error ellipse in an east-north plane.
type Ellipse struct {
	SemiMajor float64 // 1σ semi-major axis (same unit as the covariance's sqrt)
	SemiMinor float64 // 1σ semi-minor axis
	// AzimuthDeg is the direction of the major axis in degrees clockwise
	// from north, in [0, 180).
	AzimuthDeg float64
}

// ErrorEllipse returns the 1σ error ellipse of a 2×2 east-north covariance
// [[ee, en], [en, nn]]. Multiply the axes by sqrt(χ²₂(p)) (2.4477 for 95 %)
// for a confidence region. Negative eigenvalues from rounding are clamped
// to 0.
func ErrorEllipse(ee, en, nn float64) Ellipse {
	tr := ee + nn
	d := math.Hypot((ee-nn)/2, en)
	l1 := tr/2 + d
	l2 := tr/2 - d
	if l2 < 0 {
		l2 = 0
	}
	if l1 < 0 {
		l1 = 0
	}
	// Major-axis direction: angle from east θ = ½·atan2(2en, ee-nn);
	// azimuth from north = 90° - θ.
	theta := 0.5 * math.Atan2(2*en, ee-nn)
	az := 90 - theta*180/math.Pi
	az = math.Mod(az, 180)
	if az < 0 {
		az += 180
	}
	return Ellipse{SemiMajor: math.Sqrt(l1), SemiMinor: math.Sqrt(l2), AzimuthDeg: az}
}

// HorizontalSigma returns the 1σ "DRMS" horizontal error sqrt(ee + nn).
func HorizontalSigma(ee, nn float64) float64 { return math.Sqrt(math.Max(ee+nn, 0)) }

// SymEigen2 returns the eigenvalues (descending) of a symmetric 2×2.
func SymEigen2(a, b, c float64) (l1, l2 float64) {
	tr := a + c
	d := math.Hypot((a-c)/2, b)
	return tr/2 + d, tr/2 - d
}
