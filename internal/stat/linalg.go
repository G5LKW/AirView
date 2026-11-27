package stat

import "math"

// Matrix is a dense row-major matrix: element (i, j) is Data[i*Cols+j].
type Matrix struct {
	Rows, Cols int
	Data       []float64
}

// NewMatrix returns a zero matrix. Negative sizes are treated as 0.
func NewMatrix(rows, cols int) *Matrix {
	if rows < 0 {
		rows = 0
	}
	if cols < 0 {
		cols = 0
	}
	return &Matrix{Rows: rows, Cols: cols, Data: make([]float64, rows*cols)}
}

// MatrixFromRows copies a slice of equally long rows into a Matrix.
func MatrixFromRows(rows [][]float64) (*Matrix, error) {
	if len(rows) == 0 {
		return NewMatrix(0, 0), nil
	}
	c := len(rows[0])
	m := NewMatrix(len(rows), c)
	for i, r := range rows {
		if len(r) != c {
			return nil, ErrDimension
		}
		copy(m.Data[i*c:(i+1)*c], r)
	}
	return m, nil
}

// At returns element (i, j). Indices are not range checked beyond Go's own
// slice bounds check.
func (m *Matrix) At(i, j int) float64 { return m.Data[i*m.Cols+j] }

// Set stores v at (i, j).
func (m *Matrix) Set(i, j int, v float64) { m.Data[i*m.Cols+j] = v }

// Row returns row i as a slice sharing the matrix storage.
func (m *Matrix) Row(i int) []float64 { return m.Data[i*m.Cols : (i+1)*m.Cols] }

// Clone returns a deep copy.
func (m *Matrix) Clone() *Matrix {
	c := &Matrix{Rows: m.Rows, Cols: m.Cols, Data: make([]float64, len(m.Data))}
	copy(c.Data, m.Data)
	return c
}

// T returns the transpose as a new matrix.
func (m *Matrix) T() *Matrix {
	t := NewMatrix(m.Cols, m.Rows)
	for i := 0; i < m.Rows; i++ {
		for j := 0; j < m.Cols; j++ {
			t.Data[j*t.Cols+i] = m.Data[i*m.Cols+j]
		}
	}
	return t
}

// Mul returns the matrix product m*b.
func (m *Matrix) Mul(b *Matrix) (*Matrix, error) {
	if b == nil || m.Cols != b.Rows {
		return nil, ErrDimension
	}
	out := NewMatrix(m.Rows, b.Cols)
	for i := 0; i < m.Rows; i++ {
		orow := out.Data[i*out.Cols : (i+1)*out.Cols]
		for k := 0; k < m.Cols; k++ {
			a := m.Data[i*m.Cols+k]
			if a == 0 {
				continue
			}
			brow := b.Data[k*b.Cols : (k+1)*b.Cols]
			for j, bv := range brow {
				orow[j] += a * bv
			}
		}
	}
	return out, nil
}

// MulVec returns m*x.
func (m *Matrix) MulVec(x []float64) ([]float64, error) {
	if len(x) != m.Cols {
		return nil, ErrDimension
	}
	out := make([]float64, m.Rows)
	for i := 0; i < m.Rows; i++ {
		out[i] = dot(m.Data[i*m.Cols:(i+1)*m.Cols], x)
	}
	return out, nil
}

func dot(a, b []float64) float64 {
	s := 0.0
	for i, v := range a {
		s += v * b[i]
	}
	return s
}

// Cholesky factors a symmetric positive definite matrix a = L*L' and returns
// the lower triangular L. Only the lower triangle of a is read. ErrSingular
// is returned when a is not (numerically) positive definite.
func Cholesky(a *Matrix) (*Matrix, error) {
	if a == nil || a.Rows != a.Cols {
		return nil, ErrDimension
	}
	n := a.Rows
	l := NewMatrix(n, n)
	for j := 0; j < n; j++ {
		ajj := a.Data[j*n+j]
		d := ajj
		lj := l.Data[j*n : j*n+j]
		for _, v := range lj {
			d -= v * v
		}
		if !(d > 1e-13*math.Abs(ajj)) || math.IsInf(d, 0) {
			return nil, ErrSingular
		}
		d = math.Sqrt(d)
		l.Data[j*n+j] = d
		for i := j + 1; i < n; i++ {
			s := a.Data[i*n+j]
			li := l.Data[i*n : i*n+j]
			for k, v := range lj {
				s -= li[k] * v
			}
			l.Data[i*n+j] = s / d
		}
	}
	return l, nil
}

// CholeskySolve solves (L*L') x = b for x given the factor from Cholesky.
func CholeskySolve(l *Matrix, b []float64) ([]float64, error) {
	if l == nil || l.Rows != l.Cols || len(b) != l.Rows {
		return nil, ErrDimension
	}
	n := l.Rows
	x := make([]float64, n)
	copy(x, b)
	for i := 0; i < n; i++ {
		s := x[i]
		row := l.Data[i*n : i*n+i]
		for k, v := range row {
			s -= v * x[k]
		}
		x[i] = s / l.Data[i*n+i]
	}
	for i := n - 1; i >= 0; i-- {
		s := x[i]
		for k := i + 1; k < n; k++ {
			s -= l.Data[k*n+i] * x[k]
		}
		x[i] = s / l.Data[i*n+i]
	}
	return x, nil
}

// CholeskyInverse returns (L*L')^-1 given the factor from Cholesky.
func CholeskyInverse(l *Matrix) (*Matrix, error) {
	if l == nil || l.Rows != l.Cols {
		return nil, ErrDimension
	}
	n := l.Rows
	inv := NewMatrix(n, n)
	e := make([]float64, n)
	for j := 0; j < n; j++ {
		for i := range e {
			e[i] = 0
		}
		e[j] = 1
		col, err := CholeskySolve(l, e)
		if err != nil {
			return nil, err
		}
		for i := 0; i < n; i++ {
			inv.Data[i*n+j] = col[i]
		}
	}
	return inv, nil
}

// SolveSPD solves a x = b for a symmetric positive definite a.
func SolveSPD(a *Matrix, b []float64) ([]float64, error) {
	l, err := Cholesky(a)
	if err != nil {
		return nil, err
	}
	return CholeskySolve(l, b)
}

// Solve solves the square system a x = b by Gaussian elimination with partial
// pivoting. a and b are not modified.
//
// The equations are first scaled so that the largest coefficient of every
// row is 1, and a pivot counts as zero when it is below 1e-14 of the largest
// coefficient of its column. Both the pivot choice and the singularity test
// are therefore independent of the units of the individual equations and
// unknowns: diag(1e20, 1) is solved, not rejected as singular. ErrSingular is
// returned for a singular or numerically singular matrix and for a matrix
// containing NaN or Inf.
func Solve(a *Matrix, b []float64) ([]float64, error) {
	if a == nil || a.Rows != a.Cols || len(b) != a.Rows {
		return nil, ErrDimension
	}
	n := a.Rows
	if n == 0 {
		return nil, ErrSingular
	}
	m := a.Clone().Data
	x := make([]float64, n)
	copy(x, b)
	for i := 0; i < n; i++ {
		row := m[i*n : (i+1)*n]
		s := 0.0
		for _, v := range row {
			if !IsFinite(v) {
				return nil, ErrSingular
			}
			if av := math.Abs(v); av > s {
				s = av
			}
		}
		if s == 0 {
			return nil, ErrSingular
		}
		for j := range row {
			row[j] /= s
		}
		x[i] /= s
	}
	colMax := make([]float64, n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if av := math.Abs(m[i*n+j]); av > colMax[j] {
				colMax[j] = av
			}
		}
	}
	for k := 0; k < n; k++ {
		p := k
		pv := math.Abs(m[k*n+k])
		for i := k + 1; i < n; i++ {
			if av := math.Abs(m[i*n+k]); av > pv {
				p, pv = i, av
			}
		}
		if !(pv > 1e-14*colMax[k]) {
			return nil, ErrSingular
		}
		if p != k {
			for j := 0; j < n; j++ {
				m[k*n+j], m[p*n+j] = m[p*n+j], m[k*n+j]
			}
			x[k], x[p] = x[p], x[k]
		}
		piv := m[k*n+k]
		for i := k + 1; i < n; i++ {
			f := m[i*n+k] / piv
			if f == 0 {
				continue
			}
			for j := k + 1; j < n; j++ {
				m[i*n+j] -= f * m[k*n+j]
			}
			x[i] -= f * x[k]
		}
	}
	for i := n - 1; i >= 0; i-- {
		s := x[i]
		for j := i + 1; j < n; j++ {
			s -= m[i*n+j] * x[j]
		}
		x[i] = s / m[i*n+i]
	}
	return x, nil
}

// QR is a Householder QR decomposition of an m x n matrix with m >= n.
type QR struct {
	m, n    int
	qr      []float64 // Householder vectors below the diagonal, R above it
	rdiag   []float64 // diagonal of R
	colNorm []float64 // Euclidean norm of every column of the input
}

// NewQR decomposes a (not modified). It requires Rows >= Cols >= 1.
func NewQR(a *Matrix) (*QR, error) {
	if a == nil || a.Cols < 1 || a.Rows < a.Cols {
		return nil, ErrDimension
	}
	m, n := a.Rows, a.Cols
	q := &QR{m: m, n: n, qr: make([]float64, len(a.Data)), rdiag: make([]float64, n), colNorm: make([]float64, n)}
	copy(q.qr, a.Data)
	d := q.qr
	for k := 0; k < n; k++ {
		nrm := 0.0
		for i := 0; i < m; i++ {
			nrm = math.Hypot(nrm, d[i*n+k])
		}
		q.colNorm[k] = nrm
	}
	for k := 0; k < n; k++ {
		nrm := 0.0
		for i := k; i < m; i++ {
			nrm = math.Hypot(nrm, d[i*n+k])
		}
		if nrm != 0 {
			if d[k*n+k] < 0 {
				nrm = -nrm
			}
			for i := k; i < m; i++ {
				d[i*n+k] /= nrm
			}
			d[k*n+k]++
			for j := k + 1; j < n; j++ {
				s := 0.0
				for i := k; i < m; i++ {
					s += d[i*n+k] * d[i*n+j]
				}
				s = -s / d[k*n+k]
				for i := k; i < m; i++ {
					d[i*n+j] += s * d[i*n+k]
				}
			}
		}
		q.rdiag[k] = -nrm
	}
	return q, nil
}

// FullRank reports whether the columns are (numerically) linearly
// independent. The test is invariant to the scale of the individual columns:
// column k counts as dependent when the part of it that is orthogonal to the
// preceding columns, |R[k][k]|, is below 1e-12 of the column's own norm. (A
// test against the largest diagonal element would wrongly reject a well
// conditioned problem whose features merely have very different units.)
func (q *QR) FullRank() bool {
	for k, v := range q.rdiag {
		c := q.colNorm[k]
		if !(c > 0) || !IsFinite(c) || !(math.Abs(v) > 1e-12*c) {
			return false
		}
	}
	return true
}

// Solve returns the least-squares solution x minimising |a x - b|.
func (q *QR) Solve(b []float64) ([]float64, error) {
	if len(b) != q.m {
		return nil, ErrDimension
	}
	if !q.FullRank() {
		return nil, ErrSingular
	}
	m, n, d := q.m, q.n, q.qr
	y := make([]float64, m)
	copy(y, b)
	for k := 0; k < n; k++ {
		s := 0.0
		for i := k; i < m; i++ {
			s += d[i*n+k] * y[i]
		}
		s = -s / d[k*n+k]
		for i := k; i < m; i++ {
			y[i] += s * d[i*n+k]
		}
	}
	for k := n - 1; k >= 0; k-- {
		y[k] /= q.rdiag[k]
		for i := 0; i < k; i++ {
			y[i] -= y[k] * d[i*n+k]
		}
	}
	return y[:n:n], nil
}

// NormalInverse returns (A'A)^-1 = R^-1 R^-T, the unscaled covariance of the
// least-squares coefficients.
func (q *QR) NormalInverse() (*Matrix, error) {
	if !q.FullRank() {
		return nil, ErrSingular
	}
	n, d := q.n, q.qr
	// Invert the upper triangular R column by column.
	ri := NewMatrix(n, n)
	for j := 0; j < n; j++ {
		ri.Data[j*n+j] = 1 / q.rdiag[j]
		for i := j - 1; i >= 0; i-- {
			s := 0.0
			for k := i + 1; k <= j; k++ {
				s += d[i*n+k] * ri.Data[k*n+j]
			}
			ri.Data[i*n+j] = -s / q.rdiag[i]
		}
	}
	out := NewMatrix(n, n)
	for i := 0; i < n; i++ {
		for j := i; j < n; j++ {
			s := 0.0
			for k := j; k < n; k++ {
				s += ri.Data[i*n+k] * ri.Data[j*n+k]
			}
			out.Data[i*n+j] = s
			out.Data[j*n+i] = s
		}
	}
	return out, nil
}

// LeastSquares solves min |a x - b| via Householder QR (Rows >= Cols).
func LeastSquares(a *Matrix, b []float64) ([]float64, error) {
	q, err := NewQR(a)
	if err != nil {
		return nil, err
	}
	return q.Solve(b)
}

// quadForm returns v' M v for a square row-major matrix m of order len(v).
func quadForm(m []float64, v []float64) float64 {
	n := len(v)
	s := 0.0
	for i := 0; i < n; i++ {
		if v[i] == 0 {
			continue
		}
		s += v[i] * dot(m[i*n:(i+1)*n], v)
	}
	return s
}
