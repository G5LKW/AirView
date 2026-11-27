package linalg

import "math"

// QR factors the m×n matrix a (m ≥ n, row-major) in place with Householder
// reflections, LAPACK style: on return the upper triangle of the first n
// rows holds R, the entries below the diagonal hold the Householder
// vectors (with an implicit leading 1) and tau[0..n) their scalar factors,
// so that Q = H₀·H₁·…·Hₙ₋₁ with Hⱼ = I - tauⱼ·vⱼ·vⱼᵀ. work needs n
// elements (nil allocates).
//
// QR is the numerically preferred path for small least-squares problems
// whose normal equations would square the condition number (linear TOA
// initialisation, Bancroft-style systems, survey fits).
func QR(a []float64, m, n int, tau, work []float64) {
	checkLen("a", a, m*n)
	checkLen("tau", tau, n)
	if m < n {
		panic("linalg: QR needs m >= n")
	}
	if work == nil {
		work = make([]float64, n)
	}
	checkLen("work", work, n)
	for j := 0; j < n; j++ {
		// Norm of column j below the diagonal.
		alpha := a[j*n+j]
		var scale, ssq float64 = 0, 1
		for i := j + 1; i < m; i++ {
			v := a[i*n+j]
			if v == 0 {
				continue
			}
			av := math.Abs(v)
			if scale < av {
				r := scale / av
				ssq = 1 + ssq*r*r
				scale = av
			} else {
				r := av / scale
				ssq += r * r
			}
		}
		xnorm := scale * math.Sqrt(ssq)
		if xnorm == 0 {
			tau[j] = 0 // already upper triangular in this column
			continue
		}
		beta := -math.Copysign(math.Hypot(alpha, xnorm), alpha)
		tau[j] = (beta - alpha) / beta
		inv := 1 / (alpha - beta)
		for i := j + 1; i < m; i++ {
			a[i*n+j] *= inv
		}
		a[j*n+j] = beta
		// Apply Hⱼ to the trailing columns: w = vᵀ·A[j:, j+1:],
		// A[j:, j+1:] -= tau·v·w. Rows are contiguous, so accumulate w
		// row by row.
		nc := n - j - 1
		if nc == 0 {
			continue
		}
		w := work[:nc]
		copy(w, a[j*n+j+1:j*n+n]) // v_j = 1
		for i := j + 1; i < m; i++ {
			if v := a[i*n+j]; v != 0 {
				Axpy(v, a[i*n+j+1:i*n+n], w)
			}
		}
		t := tau[j]
		Axpy(-t, w, a[j*n+j+1:j*n+n])
		for i := j + 1; i < m; i++ {
			if v := a[i*n+j]; v != 0 {
				Axpy(-t*v, w, a[i*n+j+1:i*n+n])
			}
		}
	}
}

// QRApplyQT overwrites the m-vector b with Qᵀ·b using a factorisation
// from QR.
func QRApplyQT(a []float64, m, n int, tau, b []float64) {
	checkLen("b", b, m)
	for j := 0; j < n; j++ {
		t := tau[j]
		if t == 0 {
			continue
		}
		s := b[j]
		for i := j + 1; i < m; i++ {
			s += a[i*n+j] * b[i]
		}
		s *= t
		b[j] -= s
		for i := j + 1; i < m; i++ {
			b[i] -= s * a[i*n+j]
		}
	}
}

// QRRank returns the numerical rank of R: the number of diagonal elements
// larger than rcond·max|Rⱼⱼ| (rcond <= 0 selects n·1e-13). Note that
// without column pivoting a rank deficiency shows as a small diagonal but
// not necessarily in the last position.
func QRRank(a []float64, m, n int, rcond float64) int {
	if rcond <= 0 {
		rcond = float64(n) * 1e-13
	}
	var dmax float64
	for j := 0; j < n; j++ {
		dmax = math.Max(dmax, math.Abs(a[j*n+j]))
	}
	r := 0
	for j := 0; j < n; j++ {
		if math.Abs(a[j*n+j]) > rcond*dmax {
			r++
		}
	}
	return r
}

// QRSolve solves the least-squares problem min ‖A·x - b‖₂ from a
// factorisation produced by QR. b (m elements) is overwritten with Qᵀ·b,
// x (n elements) receives the solution, and the residual norm ‖A·x - b‖
// is returned. ErrRankDeficient is returned when R is numerically
// singular (see QRRank).
func QRSolve(a []float64, m, n int, tau, b, x []float64) (float64, error) {
	checkLen("x", x, n)
	if QRRank(a, m, n, 0) < n {
		return 0, ErrRankDeficient
	}
	QRApplyQT(a, m, n, tau, b)
	for i := n - 1; i >= 0; i-- {
		ri := a[i*n : i*n+n]
		x[i] = (b[i] - Dot(ri[i+1:], x[i+1:n])) / ri[i]
	}
	return Norm2(b[n:m]), nil
}

// QRCovariance writes (AᵀA)⁻¹ = R⁻¹·R⁻ᵀ (n×n, full symmetric) into out,
// from a factorisation produced by QR. Scale it by the residual variance
// (or use whitened rows) to obtain a parameter covariance. out must not
// alias a. work needs n elements (nil allocates).
func QRCovariance(a []float64, m, n int, out, work []float64) error {
	checkLen("out", out, n*n)
	if QRRank(a, m, n, 0) < n {
		return ErrRankDeficient
	}
	// Build Rᵀ (lower) in out, invert it in place (giving R⁻ᵀ, lower), then
	// form R⁻¹·R⁻ᵀ = (R⁻ᵀ)ᵀ·R⁻ᵀ, which is exactly what CholToInverse does
	// for a lower factor.
	Zero(out[:n*n])
	for i := 0; i < n; i++ {
		for j := i; j < n; j++ {
			out[j*n+i] = a[i*n+j]
		}
	}
	// AᵀA = RᵀR = L·Lᵀ with L = Rᵀ: CholToInverse(L) gives (L·Lᵀ)⁻¹.
	return CholToInverse(out, n, work)
}
