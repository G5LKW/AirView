// Code generated from one template for N = 3, 4, 6 and 9 (the solver,
// tracker and IMM state sizes). Keep the four copies identical.

package linalg

// Vec3 is a 3-vector.
type Vec3 [3]float64

// Mat3 is a row-major 3×3 matrix. It is a value type: methods return
// new values and never allocate.
type Mat3 [9]float64

// Identity3 returns the 3×3 identity.
func Identity3() Mat3 {
	var m Mat3
	for i := 0; i < 3; i++ {
		m[i*3+i] = 1
	}
	return m
}

// At returns element (i, j).
func (m *Mat3) At(i, j int) float64 { return m[i*3+j] }

// Set sets element (i, j).
func (m *Mat3) Set(i, j int, v float64) { m[i*3+j] = v }

// T returns the transpose.
func (m Mat3) T() Mat3 {
	var t Mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			t[j*3+i] = m[i*3+j]
		}
	}
	return t
}

// Add returns m + b.
func (m Mat3) Add(b Mat3) Mat3 {
	for i := range m {
		m[i] += b[i]
	}
	return m
}

// Sub returns m - b.
func (m Mat3) Sub(b Mat3) Mat3 {
	for i := range m {
		m[i] -= b[i]
	}
	return m
}

// Scale returns s·m.
func (m Mat3) Scale(s float64) Mat3 {
	for i := range m {
		m[i] *= s
	}
	return m
}

// Mul returns m·b.
func (m Mat3) Mul(b Mat3) Mat3 {
	var r Mat3
	for i := 0; i < 3; i++ {
		for k := 0; k < 3; k++ {
			v := m[i*3+k]
			if v == 0 {
				continue
			}
			for j := 0; j < 3; j++ {
				r[i*3+j] += v * b[k*3+j]
			}
		}
	}
	return r
}

// MulVec returns m·v.
func (m Mat3) MulVec(v Vec3) Vec3 {
	var r Vec3
	for i := 0; i < 3; i++ {
		var s float64
		for j := 0; j < 3; j++ {
			s += m[i*3+j] * v[j]
		}
		r[i] = s
	}
	return r
}

// Quad returns the quadratic form vᵀ·m·v.
func (m Mat3) Quad(v Vec3) float64 {
	var s float64
	for i := 0; i < 3; i++ {
		var r float64
		for j := 0; j < 3; j++ {
			r += m[i*3+j] * v[j]
		}
		s += v[i] * r
	}
	return s
}

// Congruence returns m·p·mᵀ (covariance propagation through m).
func (m Mat3) Congruence(p Mat3) Mat3 { return m.Mul(p).Mul(m.T()) }

// Symmetrize returns (m + mᵀ)/2.
func (m Mat3) Symmetrize() Mat3 {
	for i := 0; i < 3; i++ {
		for j := 0; j < i; j++ {
			v := (m[i*3+j] + m[j*3+i]) / 2
			m[i*3+j], m[j*3+i] = v, v
		}
	}
	return m
}

// Chol returns the lower Cholesky factor of the SPD matrix m (only the
// lower triangle of m is read).
func (m Mat3) Chol() (Mat3, error) {
	err := Cholesky(m[:], 3)
	return m, err
}

// InvSPD returns the inverse of the SPD matrix m (lower triangle read).
func (m Mat3) InvSPD() (Mat3, error) {
	var w [3]float64
	err := SPDInverse(m[:], 3, w[:])
	return m, err
}

// SolveSPD solves m·x = b for SPD m (lower triangle read).
func (m Mat3) SolveSPD(b Vec3) (Vec3, error) {
	if err := Cholesky(m[:], 3); err != nil {
		return b, err
	}
	CholSolve(m[:], 3, b[:])
	return b, nil
}

// Solve solves the general system m·x = b by LU with partial pivoting.
func (m Mat3) Solve(b Vec3) (Vec3, error) {
	var piv [3]int
	err := LUSolve(m[:], 3, b[:], piv[:])
	return b, err
}

// Dot returns v·w.
func (v Vec3) Dot(w Vec3) float64 {
	var s float64
	for i := range v {
		s += v[i] * w[i]
	}
	return s
}

// Outer returns v·wᵀ.
func (v Vec3) Outer(w Vec3) Mat3 {
	var m Mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			m[i*3+j] = v[i] * w[j]
		}
	}
	return m
}

// Vec4 is a 4-vector.
type Vec4 [4]float64

// Mat4 is a row-major 4×4 matrix. It is a value type: methods return
// new values and never allocate.
type Mat4 [16]float64

// Identity4 returns the 4×4 identity.
func Identity4() Mat4 {
	var m Mat4
	for i := 0; i < 4; i++ {
		m[i*4+i] = 1
	}
	return m
}

// At returns element (i, j).
func (m *Mat4) At(i, j int) float64 { return m[i*4+j] }

// Set sets element (i, j).
func (m *Mat4) Set(i, j int, v float64) { m[i*4+j] = v }

// T returns the transpose.
func (m Mat4) T() Mat4 {
	var t Mat4
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			t[j*4+i] = m[i*4+j]
		}
	}
	return t
}

// Add returns m + b.
func (m Mat4) Add(b Mat4) Mat4 {
	for i := range m {
		m[i] += b[i]
	}
	return m
}

// Sub returns m - b.
func (m Mat4) Sub(b Mat4) Mat4 {
	for i := range m {
		m[i] -= b[i]
	}
	return m
}

// Scale returns s·m.
func (m Mat4) Scale(s float64) Mat4 {
	for i := range m {
		m[i] *= s
	}
	return m
}

// Mul returns m·b.
func (m Mat4) Mul(b Mat4) Mat4 {
	var r Mat4
	for i := 0; i < 4; i++ {
		for k := 0; k < 4; k++ {
			v := m[i*4+k]
			if v == 0 {
				continue
			}
			for j := 0; j < 4; j++ {
				r[i*4+j] += v * b[k*4+j]
			}
		}
	}
	return r
}

// MulVec returns m·v.
func (m Mat4) MulVec(v Vec4) Vec4 {
	var r Vec4
	for i := 0; i < 4; i++ {
		var s float64
		for j := 0; j < 4; j++ {
			s += m[i*4+j] * v[j]
		}
		r[i] = s
	}
	return r
}

// Quad returns the quadratic form vᵀ·m·v.
func (m Mat4) Quad(v Vec4) float64 {
	var s float64
	for i := 0; i < 4; i++ {
		var r float64
		for j := 0; j < 4; j++ {
			r += m[i*4+j] * v[j]
		}
		s += v[i] * r
	}
	return s
}

// Congruence returns m·p·mᵀ (covariance propagation through m).
func (m Mat4) Congruence(p Mat4) Mat4 { return m.Mul(p).Mul(m.T()) }

// Symmetrize returns (m + mᵀ)/2.
func (m Mat4) Symmetrize() Mat4 {
	for i := 0; i < 4; i++ {
		for j := 0; j < i; j++ {
			v := (m[i*4+j] + m[j*4+i]) / 2
			m[i*4+j], m[j*4+i] = v, v
		}
	}
	return m
}

// Chol returns the lower Cholesky factor of the SPD matrix m (only the
// lower triangle of m is read).
func (m Mat4) Chol() (Mat4, error) {
	err := Cholesky(m[:], 4)
	return m, err
}

// InvSPD returns the inverse of the SPD matrix m (lower triangle read).
func (m Mat4) InvSPD() (Mat4, error) {
	var w [4]float64
	err := SPDInverse(m[:], 4, w[:])
	return m, err
}

// SolveSPD solves m·x = b for SPD m (lower triangle read).
func (m Mat4) SolveSPD(b Vec4) (Vec4, error) {
	if err := Cholesky(m[:], 4); err != nil {
		return b, err
	}
	CholSolve(m[:], 4, b[:])
	return b, nil
}

// Solve solves the general system m·x = b by LU with partial pivoting.
func (m Mat4) Solve(b Vec4) (Vec4, error) {
	var piv [4]int
	err := LUSolve(m[:], 4, b[:], piv[:])
	return b, err
}

// Dot returns v·w.
func (v Vec4) Dot(w Vec4) float64 {
	var s float64
	for i := range v {
		s += v[i] * w[i]
	}
	return s
}

// Outer returns v·wᵀ.
func (v Vec4) Outer(w Vec4) Mat4 {
	var m Mat4
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			m[i*4+j] = v[i] * w[j]
		}
	}
	return m
}

// Vec6 is a 6-vector.
type Vec6 [6]float64

// Mat6 is a row-major 6×6 matrix. It is a value type: methods return
// new values and never allocate.
type Mat6 [36]float64

// Identity6 returns the 6×6 identity.
func Identity6() Mat6 {
	var m Mat6
	for i := 0; i < 6; i++ {
		m[i*6+i] = 1
	}
	return m
}

// At returns element (i, j).
func (m *Mat6) At(i, j int) float64 { return m[i*6+j] }

// Set sets element (i, j).
func (m *Mat6) Set(i, j int, v float64) { m[i*6+j] = v }

// T returns the transpose.
func (m Mat6) T() Mat6 {
	var t Mat6
	for i := 0; i < 6; i++ {
		for j := 0; j < 6; j++ {
			t[j*6+i] = m[i*6+j]
		}
	}
	return t
}

// Add returns m + b.
func (m Mat6) Add(b Mat6) Mat6 {
	for i := range m {
		m[i] += b[i]
	}
	return m
}

// Sub returns m - b.
func (m Mat6) Sub(b Mat6) Mat6 {
	for i := range m {
		m[i] -= b[i]
	}
	return m
}

// Scale returns s·m.
func (m Mat6) Scale(s float64) Mat6 {
	for i := range m {
		m[i] *= s
	}
	return m
}

// Mul returns m·b.
func (m Mat6) Mul(b Mat6) Mat6 {
	var r Mat6
	for i := 0; i < 6; i++ {
		for k := 0; k < 6; k++ {
			v := m[i*6+k]
			if v == 0 {
				continue
			}
			for j := 0; j < 6; j++ {
				r[i*6+j] += v * b[k*6+j]
			}
		}
	}
	return r
}

// MulVec returns m·v.
func (m Mat6) MulVec(v Vec6) Vec6 {
	var r Vec6
	for i := 0; i < 6; i++ {
		var s float64
		for j := 0; j < 6; j++ {
			s += m[i*6+j] * v[j]
		}
		r[i] = s
	}
	return r
}

// Quad returns the quadratic form vᵀ·m·v.
func (m Mat6) Quad(v Vec6) float64 {
	var s float64
	for i := 0; i < 6; i++ {
		var r float64
		for j := 0; j < 6; j++ {
			r += m[i*6+j] * v[j]
		}
		s += v[i] * r
	}
	return s
}

// Congruence returns m·p·mᵀ (covariance propagation through m).
func (m Mat6) Congruence(p Mat6) Mat6 { return m.Mul(p).Mul(m.T()) }

// Symmetrize returns (m + mᵀ)/2.
func (m Mat6) Symmetrize() Mat6 {
	for i := 0; i < 6; i++ {
		for j := 0; j < i; j++ {
			v := (m[i*6+j] + m[j*6+i]) / 2
			m[i*6+j], m[j*6+i] = v, v
		}
	}
	return m
}

// Chol returns the lower Cholesky factor of the SPD matrix m (only the
// lower triangle of m is read).
func (m Mat6) Chol() (Mat6, error) {
	err := Cholesky(m[:], 6)
	return m, err
}

// InvSPD returns the inverse of the SPD matrix m (lower triangle read).
func (m Mat6) InvSPD() (Mat6, error) {
	var w [6]float64
	err := SPDInverse(m[:], 6, w[:])
	return m, err
}

// SolveSPD solves m·x = b for SPD m (lower triangle read).
func (m Mat6) SolveSPD(b Vec6) (Vec6, error) {
	if err := Cholesky(m[:], 6); err != nil {
		return b, err
	}
	CholSolve(m[:], 6, b[:])
	return b, nil
}

// Solve solves the general system m·x = b by LU with partial pivoting.
func (m Mat6) Solve(b Vec6) (Vec6, error) {
	var piv [6]int
	err := LUSolve(m[:], 6, b[:], piv[:])
	return b, err
}

// Dot returns v·w.
func (v Vec6) Dot(w Vec6) float64 {
	var s float64
	for i := range v {
		s += v[i] * w[i]
	}
	return s
}

// Outer returns v·wᵀ.
func (v Vec6) Outer(w Vec6) Mat6 {
	var m Mat6
	for i := 0; i < 6; i++ {
		for j := 0; j < 6; j++ {
			m[i*6+j] = v[i] * w[j]
		}
	}
	return m
}

// Vec9 is a 9-vector.
type Vec9 [9]float64

// Mat9 is a row-major 9×9 matrix. It is a value type: methods return
// new values and never allocate.
type Mat9 [81]float64

// Identity9 returns the 9×9 identity.
func Identity9() Mat9 {
	var m Mat9
	for i := 0; i < 9; i++ {
		m[i*9+i] = 1
	}
	return m
}

// At returns element (i, j).
func (m *Mat9) At(i, j int) float64 { return m[i*9+j] }

// Set sets element (i, j).
func (m *Mat9) Set(i, j int, v float64) { m[i*9+j] = v }

// T returns the transpose.
func (m Mat9) T() Mat9 {
	var t Mat9
	for i := 0; i < 9; i++ {
		for j := 0; j < 9; j++ {
			t[j*9+i] = m[i*9+j]
		}
	}
	return t
}

// Add returns m + b.
func (m Mat9) Add(b Mat9) Mat9 {
	for i := range m {
		m[i] += b[i]
	}
	return m
}

// Sub returns m - b.
func (m Mat9) Sub(b Mat9) Mat9 {
	for i := range m {
		m[i] -= b[i]
	}
	return m
}

// Scale returns s·m.
func (m Mat9) Scale(s float64) Mat9 {
	for i := range m {
		m[i] *= s
	}
	return m
}

// Mul returns m·b.
func (m Mat9) Mul(b Mat9) Mat9 {
	var r Mat9
	for i := 0; i < 9; i++ {
		for k := 0; k < 9; k++ {
			v := m[i*9+k]
			if v == 0 {
				continue
			}
			for j := 0; j < 9; j++ {
				r[i*9+j] += v * b[k*9+j]
			}
		}
	}
	return r
}

// MulVec returns m·v.
func (m Mat9) MulVec(v Vec9) Vec9 {
	var r Vec9
	for i := 0; i < 9; i++ {
		var s float64
		for j := 0; j < 9; j++ {
			s += m[i*9+j] * v[j]
		}
		r[i] = s
	}
	return r
}

// Quad returns the quadratic form vᵀ·m·v.
func (m Mat9) Quad(v Vec9) float64 {
	var s float64
	for i := 0; i < 9; i++ {
		var r float64
		for j := 0; j < 9; j++ {
			r += m[i*9+j] * v[j]
		}
		s += v[i] * r
	}
	return s
}

// Congruence returns m·p·mᵀ (covariance propagation through m).
func (m Mat9) Congruence(p Mat9) Mat9 { return m.Mul(p).Mul(m.T()) }

// Symmetrize returns (m + mᵀ)/2.
func (m Mat9) Symmetrize() Mat9 {
	for i := 0; i < 9; i++ {
		for j := 0; j < i; j++ {
			v := (m[i*9+j] + m[j*9+i]) / 2
			m[i*9+j], m[j*9+i] = v, v
		}
	}
	return m
}

// Chol returns the lower Cholesky factor of the SPD matrix m (only the
// lower triangle of m is read).
func (m Mat9) Chol() (Mat9, error) {
	err := Cholesky(m[:], 9)
	return m, err
}

// InvSPD returns the inverse of the SPD matrix m (lower triangle read).
func (m Mat9) InvSPD() (Mat9, error) {
	var w [9]float64
	err := SPDInverse(m[:], 9, w[:])
	return m, err
}

// SolveSPD solves m·x = b for SPD m (lower triangle read).
func (m Mat9) SolveSPD(b Vec9) (Vec9, error) {
	if err := Cholesky(m[:], 9); err != nil {
		return b, err
	}
	CholSolve(m[:], 9, b[:])
	return b, nil
}

// Solve solves the general system m·x = b by LU with partial pivoting.
func (m Mat9) Solve(b Vec9) (Vec9, error) {
	var piv [9]int
	err := LUSolve(m[:], 9, b[:], piv[:])
	return b, err
}

// Dot returns v·w.
func (v Vec9) Dot(w Vec9) float64 {
	var s float64
	for i := range v {
		s += v[i] * w[i]
	}
	return s
}

// Outer returns v·wᵀ.
func (v Vec9) Outer(w Vec9) Mat9 {
	var m Mat9
	for i := 0; i < 9; i++ {
		for j := 0; j < 9; j++ {
			m[i*9+j] = v[i] * w[j]
		}
	}
	return m
}
