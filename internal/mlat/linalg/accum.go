package linalg

// Accum is the sparse block accumulator of an information-form update
// (DESIGN §9.2.3): over an epoch it collects
//
//	J += Hᵀ·R⁻¹·H,   g += Hᵀ·R⁻¹·ν
//
// for observations whose Jacobian H touches only a few of the N states
// (the clock states of the stations in one group). Each observation is
// given in compact form: the k state indices it touches and the m×k
// dense block of H over those columns. Only the touched k×k entries of J
// are updated, so an observation costs O(m·k² + m²·k) regardless of N.
//
// J is kept in full symmetric storage, ready for Cholesky. The
// accumulator allocates only in NewAccum; the per-observation methods use
// the internal scratch buffer, so an Accum must not be shared between
// goroutines.
type Accum struct {
	N int
	J []float64 // N×N information matrix
	G []float64 // N information vector
	// Count is the number of observations added since Reset.
	Count int

	scratch []float64
}

// NewAccum allocates an accumulator for n states.
func NewAccum(n int) *Accum {
	return &Accum{N: n, J: make([]float64, n*n), G: make([]float64, n)}
}

// Reset zeroes J and g.
func (a *Accum) Reset() {
	Zero(a.J)
	Zero(a.G)
	a.Count = 0
}

func (a *Accum) buf(n int) []float64 {
	if cap(a.scratch) < n {
		a.scratch = make([]float64, n)
	}
	return a.scratch[:n]
}

// Add adds one observation: idx (k state indices), h (m×k row-major block
// of H over those states), rinv (m×m symmetric R⁻¹; nil means identity,
// i.e. pre-whitened rows) and the innovation nu (m elements; nil adds
// information only). Duplicate indices are allowed and add up correctly.
func (a *Accum) Add(idx []int, h []float64, m int, rinv, nu []float64) {
	a.AddScaled(1, idx, h, m, rinv, nu)
}

// AddScaled is Add with the observation's information multiplied by s
// (s < 1 de-weights it, e.g. the per-station influence cap of §9.2.4).
func (a *Accum) AddScaled(s float64, idx []int, h []float64, m int, rinv, nu []float64) {
	k := len(idx)
	checkLen("h", h, m*k)
	n := a.N
	// T = s·R⁻¹·H (m×k).
	var t []float64
	switch {
	case rinv != nil:
		checkLen("rinv", rinv, m*m)
		t = a.buf(m * k)
		MatMul(t, rinv, h, m, m, k)
		if s != 1 {
			for i := range t {
				t[i] *= s
			}
		}
	case s != 1:
		t = a.buf(m * k)
		for i := range t {
			t[i] = s * h[i]
		}
	default:
		t = h
	}
	// J[idx, idx] += Hᵀ·T and g[idx] += Tᵀ·ν (= s·Hᵀ·R⁻¹·ν since R⁻¹ is
	// symmetric).
	for r := 0; r < m; r++ {
		hr := h[r*k : r*k+k]
		tr := t[r*k : r*k+k]
		for p := 0; p < k; p++ {
			if hp := hr[p]; hp != 0 {
				row := a.J[idx[p]*n : idx[p]*n+n]
				for q := 0; q < k; q++ {
					row[idx[q]] += hp * tr[q]
				}
			}
		}
		if nu != nil {
			if v := nu[r]; v != 0 {
				for q := 0; q < k; q++ {
					a.G[idx[q]] += tr[q] * v
				}
			}
		}
	}
	a.Count++
}

// AddDiagPrior adds a scalar prior information to state i (J_ii += info,
// g_i += info·offset): soft constraints and gauges.
func (a *Accum) AddDiagPrior(i int, info, offset float64) {
	a.J[i*a.N+i] += info
	a.G[i] += info * offset
}
