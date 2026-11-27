package linalg

import "math"

// MaxNormal is the largest parameter count Normal supports (the 9-state
// tracker / survey problems; the MLAT fix itself has 4).
const MaxNormal = 9

// Normal accumulates the normal equations of a small weighted
// least-squares problem, cost = ½·Σ wᵢ·rᵢ², without allocating:
//
//	H = Σ wᵢ·jᵢ·jᵢᵀ   (JᵀWJ, Gauss-Newton Hessian)
//	g = Σ wᵢ·rᵢ·jᵢ    (JᵀWr, gradient of the cost)
//
// For correlated measurements (a full covariance Σ, DESIGN §9.4) whiten
// first: with Σ = L·Lᵀ, use rows of L⁻¹·J and entries of L⁻¹·r with unit
// weight (see Whiten).
type Normal struct {
	N    int
	H    [MaxNormal * MaxNormal]float64 // row-major N×N (stride N)
	G    [MaxNormal]float64
	Cost float64 // ½·Σ w·r²
	Rows int
}

// Reset clears the accumulator for n parameters.
func (s *Normal) Reset(n int) {
	if n < 1 || n > MaxNormal {
		panic("linalg: Normal size out of range")
	}
	*s = Normal{N: n}
}

// AddRow adds one measurement row: Jacobian row j (N elements), residual r
// (model minus measurement) and weight w (1/σ²).
func (s *Normal) AddRow(j []float64, r, w float64) {
	n := s.N
	j = j[:n]
	for a := 0; a < n; a++ {
		wa := w * j[a]
		if wa == 0 {
			continue
		}
		row := s.H[a*n : a*n+a+1]
		for b := 0; b <= a; b++ {
			row[b] += wa * j[b]
		}
		s.G[a] += wa * r
	}
	s.Cost += 0.5 * w * r * r
	s.Rows++
}

// hessian returns the full symmetric H (lower triangle mirrored).
func (s *Normal) hessian() [MaxNormal * MaxNormal]float64 {
	h := s.H
	MirrorLower(h[:s.N*s.N], s.N)
	return h
}

// Step solves the damped normal equations
//
//	(H + λ·D)·dx = -g,   D = diag(max(Hᵢᵢ, dmin))
//
// (Marquardt scaling; dmin keeps unobservable directions bounded) and
// writes dx (N elements). λ = 0 gives the Gauss-Newton step. It returns
// the predicted cost reduction ½·dxᵀ(λ·D·dx - g) used by GainRatio.
func (s *Normal) Step(lambda, dmin float64, dx []float64) (pred float64, err error) {
	n := s.N
	dx = dx[:n]
	h := s.hessian()
	for i := 0; i < n; i++ {
		d := math.Max(h[i*n+i], dmin)
		h[i*n+i] += lambda * d
		dx[i] = -s.G[i]
	}
	if err = Cholesky(h[:n*n], n); err != nil {
		return 0, err
	}
	CholSolve(h[:n*n], n, dx)
	for i := 0; i < n; i++ {
		d := math.Max(s.H[i*n+i], dmin)
		pred += dx[i] * (lambda*d*dx[i] - s.G[i])
	}
	return pred / 2, nil
}

// Covariance writes (JᵀWJ)⁻¹ (N×N, full) into out: the Cramér-Rao bound of
// the parameters when the weights are 1/σ². Scale it by max(1, χ²/DOF) for
// the a-posteriori covariance (DESIGN §9.4).
func (s *Normal) Covariance(out []float64) error {
	n := s.N
	checkLen("out", out, n*n)
	h := s.hessian()
	copy(out[:n*n], h[:n*n])
	var w [MaxNormal]float64
	return SPDInverse(out, n, w[:n])
}

// GainRatio returns ρ = (actual reduction)/(predicted reduction) of a
// trial step, the acceptance test of Levenberg-Marquardt.
func GainRatio(costOld, costNew, pred float64) float64 {
	if !(pred > 0) {
		if costNew < costOld {
			return 1
		}
		return -1
	}
	return (costOld - costNew) / pred
}

// LM is the damping-parameter bookkeeping of Levenberg-Marquardt with the
// Nielsen (1999) update: on acceptance λ ← λ·max(1/3, 1-(2ρ-1)³), ν ← 2;
// on rejection λ ← λ·ν, ν ← 2ν. Typical use:
//
//	var lm linalg.LM
//	lm.Init(&normal, 1e-3)
//	for it := 0; it < 10; it++ {
//		pred, err := normal.Step(lm.Lambda, dmin, dx)
//		... evaluate the cost at x+dx ...
//		if lm.Update(linalg.GainRatio(cost, newCost, pred)) { x += dx; rebuild normal }
//		if lm.Converged(dx, x, 1e-6) { break }
//	}
type LM struct {
	Lambda float64
	Nu     float64
}

// Init sets λ = tau·max(Hᵢᵢ) (tau around 1e-3 for a good start, 1 for a
// poor one).
func (l *LM) Init(s *Normal, tau float64) {
	var m float64
	for i := 0; i < s.N; i++ {
		m = math.Max(m, s.H[i*s.N+i])
	}
	if m == 0 {
		m = 1
	}
	l.Lambda = tau * m
	l.Nu = 2
}

// InitLambda sets λ directly.
func (l *LM) InitLambda(lambda float64) { l.Lambda, l.Nu = lambda, 2 }

// Update applies the gain ratio of the last trial step and reports whether
// the step is accepted (ρ > 0).
func (l *LM) Update(rho float64) bool {
	if rho > 0 {
		t := 2*rho - 1
		l.Lambda *= math.Max(1.0/3, 1-t*t*t)
		l.Nu = 2
		if l.Lambda < 1e-300 {
			l.Lambda = 1e-300
		}
		return true
	}
	l.Lambda *= l.Nu
	l.Nu *= 2
	if math.IsInf(l.Lambda, 0) || l.Lambda > 1e300 {
		l.Lambda = 1e300
	}
	return false
}

// Converged reports whether the step is small relative to the parameters:
// ‖dx‖ <= tol·(‖x‖ + tol).
func Converged(dx, x []float64, tol float64) bool {
	return Norm2(dx) <= tol*(Norm2(x)+tol)
}

// Whiten applies L⁻¹ to the residual vector r (m elements) and to each
// column of the m×n Jacobian J (row-major) in place, where L is the lower
// Cholesky factor of the measurement covariance Σ (m×m). Afterwards the
// rows have unit weight: rᵀΣ⁻¹r = ‖L⁻¹r‖². J may be nil.
func Whiten(l []float64, m int, r, J []float64, n int) {
	if r != nil {
		SolveLower(l, m, r)
	}
	if J == nil {
		return
	}
	checkLen("J", J, m*n)
	// Forward substitution on all columns at once: row i of the result =
	// (J_i - Σ_{k<i} L_ik·result_k) / L_ii.
	for i := 0; i < m; i++ {
		ji := J[i*n : i*n+n]
		for k := 0; k < i; k++ {
			if v := l[i*m+k]; v != 0 {
				Axpy(-v, J[k*n:k*n+n], ji)
			}
		}
		inv := 1 / l[i*m+i]
		for c := range ji {
			ji[c] *= inv
		}
	}
}
