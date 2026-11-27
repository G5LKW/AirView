// Package linalg is the small dense linear algebra used by the MLAT engine
// (DESIGN §9.1): Cholesky factorisation, solves and SPD inverses up to a few
// hundred unknowns (the clock filter), Householder QR for least squares,
// fixed-size 3×3/4×4/6×6/9×9 helpers for the solver and tracker,
// Levenberg-Marquardt bookkeeping, covariance operations and a sparse block
// accumulator for information-form updates.
//
// Conventions
//
//   - Matrices are row-major []float64 with explicit dimensions; element
//     (i, j) of an m×n matrix is a[i*n+j]. Vectors are []float64.
//   - Nothing allocates unless the documentation says so: callers pass
//     destination and workspace slices (nil workspace means "allocate").
//     The fixed-size types are values and never allocate.
//   - Symmetric matrices are stored in full. Routines that only read one
//     triangle say which.
//   - Functions panic on dimension mismatches (programming errors) and
//     return errors for numerical failure (not positive definite, rank
//     deficient).
//
// The code is plain Go (no assembly, no cgo). Hot loops are 4-way unrolled
// dot products and AXPYs over contiguous rows.
package linalg

import (
	"errors"
	"fmt"
	"math"
)

// Numerical failure errors.
var (
	// ErrNotPositiveDefinite is returned by Cholesky-based routines when a
	// pivot is not strictly positive.
	ErrNotPositiveDefinite = errors.New("linalg: matrix is not positive definite")
	// ErrRankDeficient is returned by QR solves when R has a (relatively)
	// zero diagonal element.
	ErrRankDeficient = errors.New("linalg: matrix is rank deficient")
	// ErrSingular is returned for singular general matrices.
	ErrSingular = errors.New("linalg: matrix is singular")
)

func checkLen(name string, s []float64, need int) {
	if len(s) < need {
		panic(fmt.Sprintf("linalg: %s has length %d, need %d", name, len(s), need))
	}
}

// Dot returns Σ a[i]·b[i] over len(a) elements (b must be at least as long).
func Dot(a, b []float64) float64 {
	b = b[:len(a)]
	var s0, s1, s2, s3 float64
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}

// Axpy computes y += alpha·x over len(x) elements.
func Axpy(alpha float64, x, y []float64) {
	y = y[:len(x)]
	i := 0
	for ; i+4 <= len(x); i += 4 {
		y[i] += alpha * x[i]
		y[i+1] += alpha * x[i+1]
		y[i+2] += alpha * x[i+2]
		y[i+3] += alpha * x[i+3]
	}
	for ; i < len(x); i++ {
		y[i] += alpha * x[i]
	}
}

// Norm2 returns the Euclidean norm of x, guarded against overflow.
func Norm2(x []float64) float64 {
	var scale, ssq float64 = 0, 1
	for _, v := range x {
		if v == 0 {
			continue
		}
		a := math.Abs(v)
		if scale < a {
			r := scale / a
			ssq = 1 + ssq*r*r
			scale = a
		} else {
			r := a / scale
			ssq += r * r
		}
	}
	return scale * math.Sqrt(ssq)
}

// Zero sets every element of a to 0.
func Zero(a []float64) {
	for i := range a {
		a[i] = 0
	}
}

// Identity writes the n×n identity into a.
func Identity(a []float64, n int) {
	checkLen("a", a, n*n)
	Zero(a[:n*n])
	for i := 0; i < n; i++ {
		a[i*n+i] = 1
	}
}

// Transpose writes the n×m transpose of the m×n matrix a into dst (dst
// must not alias a).
func Transpose(dst, a []float64, m, n int) {
	checkLen("a", a, m*n)
	checkLen("dst", dst, m*n)
	for i := 0; i < m; i++ {
		for j := 0; j < n; j++ {
			dst[j*m+i] = a[i*n+j]
		}
	}
}

// MatMul computes dst = a·b with a m×k and b k×n (dst m×n, not aliasing a
// or b).
func MatMul(dst, a, b []float64, m, k, n int) {
	checkLen("a", a, m*k)
	checkLen("b", b, k*n)
	checkLen("dst", dst, m*n)
	for i := 0; i < m; i++ {
		row := dst[i*n : i*n+n]
		Zero(row)
		for p := 0; p < k; p++ {
			if v := a[i*k+p]; v != 0 {
				Axpy(v, b[p*n:p*n+n], row)
			}
		}
	}
}

// MatMulABt computes dst = a·bᵀ with a m×k and b n×k (dst m×n).
func MatMulABt(dst, a, b []float64, m, k, n int) {
	checkLen("a", a, m*k)
	checkLen("b", b, n*k)
	checkLen("dst", dst, m*n)
	for i := 0; i < m; i++ {
		ai := a[i*k : i*k+k]
		for j := 0; j < n; j++ {
			dst[i*n+j] = Dot(ai, b[j*k:j*k+k])
		}
	}
}

// MatMulAtB computes dst = aᵀ·b with a k×m and b k×n (dst m×n).
func MatMulAtB(dst, a, b []float64, k, m, n int) {
	checkLen("a", a, k*m)
	checkLen("b", b, k*n)
	checkLen("dst", dst, m*n)
	Zero(dst[:m*n])
	for p := 0; p < k; p++ {
		bp := b[p*n : p*n+n]
		for i := 0; i < m; i++ {
			if v := a[p*m+i]; v != 0 {
				Axpy(v, bp, dst[i*n:i*n+n])
			}
		}
	}
}

// MatVec computes y = a·x with a m×n.
func MatVec(y, a, x []float64, m, n int) {
	checkLen("a", a, m*n)
	checkLen("x", x, n)
	checkLen("y", y, m)
	for i := 0; i < m; i++ {
		y[i] = Dot(a[i*n:i*n+n], x)
	}
}

// MatTVec computes y = aᵀ·x with a m×n (y has n elements).
func MatTVec(y, a, x []float64, m, n int) {
	checkLen("a", a, m*n)
	checkLen("x", x, m)
	checkLen("y", y, n)
	Zero(y[:n])
	for i := 0; i < m; i++ {
		if v := x[i]; v != 0 {
			Axpy(v, a[i*n:i*n+n], y)
		}
	}
}
