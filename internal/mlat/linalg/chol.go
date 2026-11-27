package linalg

import "math"

// Cholesky factors the symmetric positive definite n×n matrix a in place
// into its lower-triangular factor L (a = L·Lᵀ). Only the lower triangle
// (including the diagonal) of a is read; on success the strict upper
// triangle is set to zero. On failure a is partly overwritten and
// ErrNotPositiveDefinite is returned.
//
// The row-oriented (Cholesky-Banachiewicz) order makes every inner loop a
// dot product of two contiguous rows; from n = 8 on, rows are processed
// in pairs with 2×2 register blocking. Cost n³/6 multiply-adds.
func Cholesky(a []float64, n int) error {
	checkLen("a", a, n*n)
	if n >= 8 {
		return choleskyBlocked(a, n)
	}
	return choleskySimple(a, n)
}

func choleskySimple(a []float64, n int) error {
	for i := 0; i < n; i++ {
		ri := a[i*n : i*n+n]
		for j := 0; j < i; j++ {
			rj := a[j*n : j*n+j+1]
			ri[j] = (ri[j] - Dot(ri[:j], rj[:j])) / rj[j]
		}
		d := ri[i] - Dot(ri[:i], ri[:i])
		if !(d > 0) || math.IsInf(d, 0) {
			return ErrNotPositiveDefinite
		}
		ri[i] = math.Sqrt(d)
		Zero(ri[i+1:])
	}
	return nil
}

// CholeskyJitter is Cholesky with a fallback for matrices that are positive
// semi-definite up to rounding: on failure it restores a from orig (the
// caller's copy of the input, which must not alias a), adds jitter·mean(diag)
// to the diagonal, and retries with ten times more jitter up to tries times.
// It returns the jitter that was finally added (0 when none was needed).
func CholeskyJitter(a, orig []float64, n int, jitter float64, tries int) (float64, error) {
	checkLen("orig", orig, n*n)
	copy(a[:n*n], orig[:n*n])
	if err := Cholesky(a, n); err == nil {
		return 0, nil
	}
	var md float64
	for i := 0; i < n; i++ {
		md += math.Abs(orig[i*n+i])
	}
	md /= float64(n)
	if md == 0 {
		md = 1
	}
	add := jitter * md
	for t := 0; t < tries; t++ {
		copy(a[:n*n], orig[:n*n])
		for i := 0; i < n; i++ {
			a[i*n+i] += add
		}
		if err := Cholesky(a, n); err == nil {
			return add, nil
		}
		add *= 10
	}
	return 0, ErrNotPositiveDefinite
}

// SolveLower solves L·x = b in place (forward substitution) for the lower
// triangular n×n L (only the lower triangle is read). Applied with the
// Cholesky factor of a covariance Σ it whitens a residual vector:
// x = L⁻¹·r has identity covariance.
func SolveLower(l []float64, n int, b []float64) {
	checkLen("l", l, n*n)
	checkLen("b", b, n)
	for i := 0; i < n; i++ {
		ri := l[i*n : i*n+i+1]
		b[i] = (b[i] - Dot(ri[:i], b[:i])) / ri[i]
	}
}

// SolveLowerT solves Lᵀ·x = b in place (back substitution with the
// transpose of the lower triangular L).
func SolveLowerT(l []float64, n int, b []float64) {
	checkLen("l", l, n*n)
	checkLen("b", b, n)
	for i := n - 1; i >= 0; i-- {
		b[i] /= l[i*n+i]
		// Remove x[i]'s contribution from the earlier rows: row i of L
		// holds column i of Lᵀ.
		Axpy(-b[i], l[i*n:i*n+i], b[:i])
	}
}

// CholSolve solves A·x = b in place given the Cholesky factor L of A.
func CholSolve(l []float64, n int, b []float64) {
	SolveLower(l, n, b)
	SolveLowerT(l, n, b)
}

// CholSolveMulti solves A·X = B in place for the n×k right-hand side B
// (row-major), given the Cholesky factor L of A.
func CholSolveMulti(l []float64, n int, b []float64, k int) {
	checkLen("l", l, n*n)
	checkLen("b", b, n*k)
	// Forward: row i of X = (B_i - Σ_{j<i} L_ij X_j) / L_ii.
	for i := 0; i < n; i++ {
		bi := b[i*k : i*k+k]
		for j := 0; j < i; j++ {
			if v := l[i*n+j]; v != 0 {
				Axpy(-v, b[j*k:j*k+k], bi)
			}
		}
		inv := 1 / l[i*n+i]
		for c := range bi {
			bi[c] *= inv
		}
	}
	// Backward with Lᵀ.
	for i := n - 1; i >= 0; i-- {
		bi := b[i*k : i*k+k]
		inv := 1 / l[i*n+i]
		for c := range bi {
			bi[c] *= inv
		}
		for j := 0; j < i; j++ {
			if v := l[i*n+j]; v != 0 {
				Axpy(-v, bi, b[j*k:j*k+k])
			}
		}
	}
}

// CholLogDet returns log det A = 2·Σ log L_ii from the Cholesky factor.
func CholLogDet(l []float64, n int) float64 {
	var s float64
	for i := 0; i < n; i++ {
		s += math.Log(l[i*n+i])
	}
	return 2 * s
}

// InvertLower replaces the lower triangular n×n L (in a) by L⁻¹, in place.
// work needs n elements (nil allocates). The strict upper triangle is left
// untouched. Cost n³/6 multiply-adds, row-contiguous AXPYs.
func InvertLower(a []float64, n int, work []float64) error {
	checkLen("a", a, n*n)
	if work == nil {
		work = make([]float64, n)
	}
	checkLen("work", work, n)
	for i := 0; i < n; i++ {
		ri := a[i*n : i*n+n]
		d := ri[i]
		if d == 0 {
			return ErrSingular
		}
		t := work[:i]
		Zero(t)
		// Row i of X = L⁻¹: X_i,0..i-1 = -(Σ_{k<i} L_ik X_k,0..k) / L_ii.
		for k := 0; k < i; k++ {
			if v := ri[k]; v != 0 {
				Axpy(v, a[k*n:k*n+k+1], t[:k+1])
			}
		}
		inv := 1 / d
		for j := 0; j < i; j++ {
			ri[j] = -t[j] * inv
		}
		ri[i] = inv
	}
	return nil
}

// SPDInverse replaces the symmetric positive definite n×n matrix a by its
// inverse (full symmetric storage), in place. Only the lower triangle of a
// is read. work needs n elements (nil allocates). Cost about n³/2
// multiply-adds: Cholesky (n³/6), triangular inverse (n³/6) and
// A⁻¹ = L⁻ᵀ·L⁻¹ (n³/6).
func SPDInverse(a []float64, n int, work []float64) error {
	if err := Cholesky(a, n); err != nil {
		return err
	}
	return CholToInverse(a, n, work)
}

// CholToInverse turns a Cholesky factor L (in a) into A⁻¹ = L⁻ᵀ·L⁻¹ in
// place (full symmetric storage). work needs n elements (nil allocates).
// See cholToInverseBlocked for the method.
func CholToInverse(a []float64, n int, work []float64) error {
	checkLen("a", a, n*n)
	if work == nil {
		work = make([]float64, n)
	}
	checkLen("work", work, n)
	return cholToInverseBlocked(a, n, work)
}

// MirrorLower copies the strict lower triangle of the n×n matrix a onto
// its strict upper triangle.
func MirrorLower(a []float64, n int) {
	for i := 0; i < n; i++ {
		for j := 0; j < i; j++ {
			a[j*n+i] = a[i*n+j]
		}
	}
}

// MirrorUpper copies the strict upper triangle onto the strict lower one.
func MirrorUpper(a []float64, n int) {
	for i := 0; i < n; i++ {
		for j := 0; j < i; j++ {
			a[i*n+j] = a[j*n+i]
		}
	}
}

// LUSolve solves the general square system A·x = b with partial pivoting.
// a (n×n) and b are overwritten (a with its LU factors, b with x). piv
// needs n elements (nil allocates). For the rare non-symmetric small
// systems (e.g. Bancroft initialisation); symmetric systems should use
// Cholesky.
func LUSolve(a []float64, n int, b []float64, piv []int) error {
	checkLen("a", a, n*n)
	checkLen("b", b, n)
	if piv == nil {
		piv = make([]int, n)
	}
	var amax float64
	for _, v := range a[:n*n] {
		amax = math.Max(amax, math.Abs(v))
	}
	tol := amax * float64(n) * 1e-15
	for k := 0; k < n; k++ {
		p, pv := k, math.Abs(a[k*n+k])
		for i := k + 1; i < n; i++ {
			if v := math.Abs(a[i*n+k]); v > pv {
				p, pv = i, v
			}
		}
		if !(pv > tol) {
			return ErrSingular
		}
		piv[k] = p
		if p != k {
			rk, rp := a[k*n:k*n+n], a[p*n:p*n+n]
			for j := range rk {
				rk[j], rp[j] = rp[j], rk[j]
			}
			b[k], b[p] = b[p], b[k]
		}
		rk := a[k*n+k : k*n+n]
		inv := 1 / rk[0]
		for i := k + 1; i < n; i++ {
			ri := a[i*n+k : i*n+n]
			f := ri[0] * inv
			ri[0] = f
			if f != 0 {
				Axpy(-f, rk[1:], ri[1:])
				b[i] -= f * b[k]
			}
		}
	}
	for i := n - 1; i >= 0; i-- {
		ri := a[i*n : i*n+n]
		b[i] = (b[i] - Dot(ri[i+1:], b[i+1:n])) / ri[i]
	}
	return nil
}
