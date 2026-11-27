package linalg

import "math"

// Register-blocked kernels for the O(n³) routines. Go does not vectorise,
// so the scalar loops are bound by loads: a plain dot product needs two
// loads per multiply-add. Computing a 2×2 block of dot products (two rows
// against two rows) shares every load between two multiply-adds, which
// roughly doubles throughput for the clock filter's n ≈ 600 inverses.

// dot2x2 returns the four dot products of (a0, a1) with (b0, b1) over
// len(a0) elements.
func dot2x2(a0, a1, b0, b1 []float64) (s00, s01, s10, s11 float64) {
	n := len(a0)
	a1, b0, b1 = a1[:n], b0[:n], b1[:n]
	var t00, t01, t10, t11 float64
	k := 0
	for ; k+2 <= n; k += 2 {
		x0, x1, y0, y1 := a0[k], a1[k], b0[k], b1[k]
		s00 += x0 * y0
		s01 += x0 * y1
		s10 += x1 * y0
		s11 += x1 * y1
		x0, x1, y0, y1 = a0[k+1], a1[k+1], b0[k+1], b1[k+1]
		t00 += x0 * y0
		t01 += x0 * y1
		t10 += x1 * y0
		t11 += x1 * y1
	}
	if k < n {
		x0, x1, y0, y1 := a0[k], a1[k], b0[k], b1[k]
		s00 += x0 * y0
		s01 += x0 * y1
		s10 += x1 * y0
		s11 += x1 * y1
	}
	return s00 + t00, s01 + t01, s10 + t10, s11 + t11
}

// choleskyBlocked is Cholesky with rows processed in pairs and the inner
// products computed as 2×2 blocks. Same contract as Cholesky.
func choleskyBlocked(a []float64, n int) error {
	i := 0
	for ; i+1 < n; i += 2 {
		r0 := a[i*n : i*n+n]
		r1 := a[(i+1)*n : (i+1)*n+n]
		// i is even, so the columns j < i pair up exactly.
		for j := 0; j < i; j += 2 {
			q0 := a[j*n : j*n+j+2]
			q1 := a[(j+1)*n : (j+1)*n+j+2]
			s00, s01, s10, s11 := dot2x2(r0[:j], r1[:j], q0[:j], q1[:j])
			r0[j] = (r0[j] - s00) / q0[j]
			r1[j] = (r1[j] - s10) / q0[j]
			r0[j+1] = (r0[j+1] - s01 - r0[j]*q1[j]) / q1[j+1]
			r1[j+1] = (r1[j+1] - s11 - r1[j]*q1[j]) / q1[j+1]
		}
		d := r0[i] - Dot(r0[:i], r0[:i])
		if !(d > 0) || math.IsInf(d, 0) {
			return ErrNotPositiveDefinite
		}
		r0[i] = math.Sqrt(d)
		r1[i] = (r1[i] - Dot(r1[:i], r0[:i])) / r0[i]
		d = r1[i+1] - Dot(r1[:i+1], r1[:i+1])
		if !(d > 0) || math.IsInf(d, 0) {
			return ErrNotPositiveDefinite
		}
		r1[i+1] = math.Sqrt(d)
		Zero(r0[i+1:])
		Zero(r1[i+2:])
	}
	if i < n { // odd n: last row
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
	}
	return nil
}

// cholToInverseBlocked turns the lower Cholesky factor L in a into
// A⁻¹ = L⁻ᵀ·L⁻¹ (full storage) in place. vd needs n elements.
//
// Phase 1 forms V = L⁻ᵀ (upper triangular) in the strict upper triangle of
// a with its diagonal in vd, by the dot-form recurrence
//
//	V[j][i] = -(Σ_{k=j}^{i-1} L[i][k]·V[j][k]) / L[i][i],   j < i,
//
// where both factors are contiguous rows. Phase 2 forms
//
//	A⁻¹[i][j] = Σ_{k≥i} V[i][k]·V[j][k],   j ≤ i,
//
// into the lower triangle (L is no longer needed), again as row dot
// products, and mirrors it to the upper triangle.
func cholToInverseBlocked(a []float64, n int, vd []float64) error {
	vd = vd[:n]
	for i := 0; i < n; i++ {
		d := a[i*n+i]
		if d == 0 {
			return ErrSingular
		}
		vd[i] = 1 / d
	}
	// v(r, c) for c >= r.
	v := func(r, c int) float64 {
		if r == c {
			return vd[r]
		}
		return a[r*n+c]
	}

	// Phase 1: V. Rows i of L in pairs; columns j < i in pairs.
	i := 0
	for ; i+1 < n; i += 2 {
		Li := a[i*n : i*n+i]
		Li1 := a[(i+1)*n : (i+1)*n+i+1]
		for j := 0; j < i; j += 2 {
			Vj := a[j*n : j*n+n]
			Vj1 := a[(j+1)*n : (j+1)*n+n]
			lo := j + 2
			s00, s01, s10, s11 := dot2x2(Li[lo:i], Li1[lo:i], Vj[lo:i], Vj1[lo:i])
			// Σ over k ∈ [j, i) for row i and target rows j, j+1.
			sj := s00 + Li[j]*vd[j] + Li[j+1]*Vj[j+1]
			sj1 := s01 + Li[j+1]*vd[j+1]
			vji := -sj * vd[i]
			vj1i := -sj1 * vd[i]
			Vj[i], Vj1[i] = vji, vj1i
			// Row i+1 sums over k ∈ [j, i]: includes k = i.
			tj := s10 + Li1[j]*vd[j] + Li1[j+1]*Vj[j+1] + Li1[i]*vji
			tj1 := s11 + Li1[j+1]*vd[j+1] + Li1[i]*vj1i
			Vj[i+1] = -tj * vd[i+1]
			Vj1[i+1] = -tj1 * vd[i+1]
		}
		// V[i][i+1] = -(L[i+1][i]·V[i][i]) / L[i+1][i+1].
		a[i*n+i+1] = -Li1[i] * vd[i] * vd[i+1]
	}
	if i < n { // odd n: last row of L
		Li := a[i*n : i*n+i]
		for j := 0; j < i; j++ {
			s := Li[j]*vd[j] + Dot(Li[j+1:i], a[j*n+j+1:j*n+i])
			a[j*n+i] = -s * vd[i]
		}
	}

	// Phase 2: lower triangle of A⁻¹. Rows i, i+1 in pairs.
	i = 0
	for ; i+1 < n; i += 2 {
		Vi := a[i*n : i*n+n]
		Vi1 := a[(i+1)*n : (i+1)*n+n]
		hi := i + 2
		for j := 0; j+1 < i; j += 2 {
			Vj := a[j*n : j*n+n]
			Vj1 := a[(j+1)*n : (j+1)*n+n]
			s00, s01, s10, s11 := dot2x2(Vi[hi:], Vi1[hi:], Vj[hi:], Vj1[hi:])
			vii, vii1, vi1i1 := vd[i], Vi[i+1], vd[i+1]
			a[i*n+j] = s00 + vii*Vj[i] + vii1*Vj[i+1]
			a[i*n+j+1] = s01 + vii*Vj1[i] + vii1*Vj1[i+1]
			a[(i+1)*n+j] = s10 + vi1i1*Vj[i+1]
			a[(i+1)*n+j+1] = s11 + vi1i1*Vj1[i+1]
		}
		// Diagonal 2×2 block (j = i, i+1). Read V[i][i+1] before the
		// lower triangle is written (it lives in the upper triangle, so it
		// is not overwritten here, but keep the order explicit).
		vii, vii1, vi1i1 := vd[i], Vi[i+1], vd[i+1]
		tail := Dot(Vi[hi:], Vi[hi:])
		cross := Dot(Vi1[hi:], Vi[hi:])
		tail1 := Dot(Vi1[hi:], Vi1[hi:])
		a[i*n+i] = vii*vii + vii1*vii1 + tail
		a[(i+1)*n+i] = vi1i1*vii1 + cross
		a[(i+1)*n+i+1] = vi1i1*vi1i1 + tail1
	}
	if i < n { // odd n: last row
		for j := 0; j <= i; j++ {
			a[i*n+j] = v(i, i) * v(j, i)
		}
	}
	MirrorLower(a, n)
	return nil
}
