package survey

import (
	"errors"
	"math"
)

// errNotPD is returned when a banded or 3×3 system is not positive
// definite.
var errNotPD = errors.New("survey: matrix not positive definite")

// bw is the half bandwidth of the clock block: an observation couples two
// adjacent knots and a second-difference prior three.
const bw = 2

// band is a symmetric positive definite K×K matrix with half bandwidth 2,
// stored by rows of its lower band: a[i][d] = A(i, i−d), d = 0..2.
type band struct {
	a [][bw + 1]float64
}

func (b *band) reset(k int) {
	if cap(b.a) < k {
		b.a = make([][bw + 1]float64, k)
	}
	b.a = b.a[:k]
	clear(b.a)
}

func (b *band) n() int { return len(b.a) }

// add adds v to A(i, j) for |i − j| <= 2 (and, symmetrically, A(j, i)).
func (b *band) add(i, j int, v float64) {
	if i < j {
		i, j = j, i
	}
	b.a[i][i-j] += v
}

// copyFrom makes b a copy of o.
func (b *band) copyFrom(o *band) {
	b.reset(o.n())
	copy(b.a, o.a)
}

// cholesky factors A = L·Lᵀ in place (L in the same band layout).
func (b *band) cholesky() error {
	a := b.a
	for i := range a {
		for d := bw; d >= 1; d-- {
			j := i - d
			if j < 0 {
				continue
			}
			s := a[i][d]
			// k runs over the columns both rows i and j have: k >= i−bw, k < j
			for k := max(0, i-bw); k < j; k++ {
				s -= a[i][i-k] * a[j][j-k]
			}
			a[i][d] = s / a[j][0]
		}
		s := a[i][0]
		for k := max(0, i-bw); k < i; k++ {
			s -= a[i][i-k] * a[i][i-k]
		}
		if !(s > 0) || math.IsNaN(s) {
			return errNotPD
		}
		a[i][0] = math.Sqrt(s)
	}
	return nil
}

// solve overwrites x (the right-hand side) with A⁻¹x, given the factor.
func (b *band) solve(x []float64) {
	a := b.a
	n := len(a)
	for i := 0; i < n; i++ {
		s := x[i]
		for k := max(0, i-bw); k < i; k++ {
			s -= a[i][i-k] * x[k]
		}
		x[i] = s / a[i][0]
	}
	for i := n - 1; i >= 0; i-- {
		s := x[i]
		for k := i + 1; k <= min(n-1, i+bw); k++ {
			s -= a[k][k-i] * x[k]
		}
		x[i] = s / a[i][0]
	}
}

// sym3 is a symmetric 3×3 matrix (row-major full storage).
type sym3 [9]float64

// inv3 inverts a symmetric positive definite 3×3 matrix.
func inv3(m sym3) (sym3, error) {
	a, b, c := m[0], m[1], m[2]
	d, e, f := m[4], m[5], m[8]
	c00 := d*f - e*e
	c01 := c*e - b*f
	c02 := b*e - c*d
	det := a*c00 + b*c01 + c*c02
	if !(det > 0) || !(a > 0) || !(a*d-b*b > 0) {
		return sym3{}, errNotPD
	}
	c11 := a*f - c*c
	c12 := b*c - a*e
	c22 := a*d - b*b
	id := 1 / det
	return sym3{c00 * id, c01 * id, c02 * id, c01 * id, c11 * id, c12 * id, c02 * id, c12 * id, c22 * id}, nil
}

func (m *sym3) mulVec(v [3]float64) [3]float64 {
	return [3]float64{
		m[0]*v[0] + m[1]*v[1] + m[2]*v[2],
		m[3]*v[0] + m[4]*v[1] + m[5]*v[2],
		m[6]*v[0] + m[7]*v[1] + m[8]*v[2],
	}
}
