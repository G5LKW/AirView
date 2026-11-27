package stat

import "math"

// OptResult reports the outcome of a minimisation.
//
// F is +Inf, and Converged false, when the objective was NaN or +Inf at
// every point tried; that is the only case in which the struct cannot be
// passed to encoding/json.
type OptResult struct {
	X         []float64 `json:"x"`         // best point found
	F         float64   `json:"f"`         // objective at X
	Iter      int       `json:"iter"`      // iterations performed
	Evals     int       `json:"evals"`     // objective evaluations
	Restarts  int       `json:"restarts"`  // restarts performed (Nelder-Mead)
	Converged bool      `json:"converged"` // tolerances met before the iteration limit
}

// NMOptions configures NelderMead. The zero value (or nil) selects defaults.
type NMOptions struct {
	MaxIter  int       // total iteration budget over all restarts; default 400*n
	TolF     float64   // spread of objective values over the simplex; default 1e-10 (absolute and relative)
	TolX     float64   // size of the simplex; default 1e-8 (absolute and relative)
	Step     []float64 // initial simplex edge per dimension; default 10 % of the bound range, else max(0.1*|x0|, 0.1)
	Lower    []float64 // optional lower bounds (nil or len n); trial points are clamped
	Upper    []float64 // optional upper bounds (nil or len n)
	Restarts int       // extra runs restarted from the best point with a fresh simplex; default 1, negative = none
}

// NelderMead minimises f with the downhill simplex method (reflection 1,
// expansion 2, contraction 0.5, shrink 0.5). Bounds are enforced by clamping
// every trial point before evaluation. After convergence the search is
// restarted from the best point with a fresh, smaller simplex up to
// Restarts times and stops early when a restart no longer improves the
// objective. NaN objective values are treated as +Inf.
//
// The result is returned even if the iteration budget ran out (Converged =
// false). Errors: ErrInvalidArgument (nil f, empty x0, inconsistent option
// lengths, lower > upper, non-finite x0).
func NelderMead(f func(x []float64) float64, x0 []float64, opt *NMOptions) (OptResult, error) {
	n := len(x0)
	o := NMOptions{}
	if opt != nil {
		o = *opt
	}
	if f == nil || n == 0 ||
		(o.Lower != nil && len(o.Lower) != n) || (o.Upper != nil && len(o.Upper) != n) ||
		(o.Step != nil && len(o.Step) != n) {
		return OptResult{}, ErrInvalidArgument
	}
	for i, v := range x0 {
		if !IsFinite(v) {
			return OptResult{}, ErrInvalidArgument
		}
		if o.Lower != nil && o.Upper != nil && o.Lower[i] > o.Upper[i] {
			return OptResult{}, ErrInvalidArgument
		}
	}
	if o.MaxIter <= 0 {
		o.MaxIter = 400 * n
	}
	if !(o.TolF > 0) {
		o.TolF = 1e-10
	}
	if !(o.TolX > 0) {
		o.TolX = 1e-8
	}
	switch {
	case opt == nil || o.Restarts == 0:
		o.Restarts = 1
	case o.Restarts < 0:
		o.Restarts = 0
	}
	project := func(x []float64) {
		for i := range x {
			if o.Lower != nil && x[i] < o.Lower[i] {
				x[i] = o.Lower[i]
			}
			if o.Upper != nil && x[i] > o.Upper[i] {
				x[i] = o.Upper[i]
			}
		}
	}
	res := OptResult{X: make([]float64, n)}
	eval := func(x []float64) float64 {
		project(x)
		res.Evals++
		v := f(x)
		if v != v {
			return math.Inf(1)
		}
		return v
	}
	step := make([]float64, n)
	for i := range step {
		switch {
		case o.Step != nil && o.Step[i] != 0 && IsFinite(o.Step[i]):
			step[i] = o.Step[i]
		case o.Lower != nil && o.Upper != nil && IsFinite(o.Upper[i]-o.Lower[i]) && o.Upper[i] > o.Lower[i]:
			step[i] = 0.1 * (o.Upper[i] - o.Lower[i])
		default:
			step[i] = math.Max(0.1*math.Abs(x0[i]), 0.1)
		}
	}

	copy(res.X, x0)
	res.F = eval(res.X)
	simplex := make([]float64, (n+1)*n)
	fv := make([]float64, n+1)
	cen := make([]float64, n)
	xr := make([]float64, n)
	xe := make([]float64, n)
	scale := 1.0
	for run := 0; run <= o.Restarts && res.Iter < o.MaxIter; run++ {
		// Build the simplex around the current best point.
		copy(simplex[:n], res.X)
		fv[0] = res.F
		for i := 0; i < n; i++ {
			v := simplex[(i+1)*n : (i+2)*n]
			copy(v, res.X)
			s := step[i] * scale
			if o.Upper != nil && v[i]+s > o.Upper[i] {
				s = -s
			}
			v[i] += s
			fv[i+1] = eval(v)
		}
		prevBest := res.F
		conv := false
		for res.Iter < o.MaxIter {
			// Locate best, worst and second worst vertices.
			lo, hi, nh := 0, 0, 0
			for i := 1; i <= n; i++ {
				if fv[i] < fv[lo] {
					lo = i
				}
				if fv[i] > fv[hi] {
					hi = i
				}
			}
			nh = lo
			for i := 0; i <= n; i++ {
				if i != hi && fv[i] > fv[nh] {
					nh = i
				}
			}
			xlo := simplex[lo*n : (lo+1)*n]
			xhi := simplex[hi*n : (hi+1)*n]
			// Convergence: flat objective and a collapsed simplex.
			size := 0.0
			for i := 0; i <= n; i++ {
				for j := 0; j < n; j++ {
					d := math.Abs(simplex[i*n+j]-xlo[j]) / (1 + math.Abs(xlo[j]))
					if d > size {
						size = d
					}
				}
			}
			spread := fv[hi] - fv[lo]
			if (spread <= o.TolF*(1+math.Abs(fv[lo])) || math.IsInf(fv[lo], 1)) && size <= o.TolX {
				conv = true
				break
			}
			res.Iter++
			for j := 0; j < n; j++ {
				s := 0.0
				for i := 0; i <= n; i++ {
					if i != hi {
						s += simplex[i*n+j]
					}
				}
				cen[j] = s / float64(n)
			}
			for j := 0; j < n; j++ {
				xr[j] = cen[j] + (cen[j] - xhi[j])
			}
			fr := eval(xr)
			switch {
			case fr < fv[lo]:
				for j := 0; j < n; j++ {
					xe[j] = cen[j] + 2*(cen[j]-xhi[j])
				}
				if fe := eval(xe); fe < fr {
					copy(xhi, xe)
					fv[hi] = fe
				} else {
					copy(xhi, xr)
					fv[hi] = fr
				}
			case fr < fv[nh]:
				copy(xhi, xr)
				fv[hi] = fr
			default:
				accepted := false
				if fr < fv[hi] { // outside contraction
					for j := 0; j < n; j++ {
						xe[j] = cen[j] + 0.5*(xr[j]-cen[j])
					}
					if fc := eval(xe); fc <= fr {
						copy(xhi, xe)
						fv[hi] = fc
						accepted = true
					}
				} else { // inside contraction
					for j := 0; j < n; j++ {
						xe[j] = cen[j] - 0.5*(cen[j]-xhi[j])
					}
					if fc := eval(xe); fc < fv[hi] {
						copy(xhi, xe)
						fv[hi] = fc
						accepted = true
					}
				}
				if !accepted { // shrink towards the best vertex
					for i := 0; i <= n; i++ {
						if i == lo {
							continue
						}
						v := simplex[i*n : (i+1)*n]
						for j := 0; j < n; j++ {
							v[j] = xlo[j] + 0.5*(v[j]-xlo[j])
						}
						fv[i] = eval(v)
					}
				}
			}
		}
		lo := 0
		for i := 1; i <= n; i++ {
			if fv[i] < fv[lo] {
				lo = i
			}
		}
		if fv[lo] <= res.F {
			res.F = fv[lo]
			copy(res.X, simplex[lo*n:(lo+1)*n])
		}
		// "Converged" on a plateau of +Inf (objective NaN or infinite
		// everywhere it was tried) is not a minimum.
		res.Converged = conv && !math.IsInf(res.F, 1)
		if run > 0 {
			res.Restarts = run
			if !(prevBest-res.F > o.TolF*(1+math.Abs(res.F))) {
				break // the restart did not help: accept
			}
		}
		if !conv {
			break // out of iterations
		}
		scale *= 0.5
	}
	return res, nil
}

// GoldenSection minimises a unimodal function of one variable on [a, b] by
// golden-section search. tol is the absolute width of the final bracket
// (default 1e-8 scaled by the interval), maxIter defaults to 200. The
// bounds may be given in either order. Errors: ErrInvalidArgument (nil f or
// non-finite bounds). NaN objective values are treated as +Inf.
func GoldenSection(f func(x float64) float64, a, b, tol float64, maxIter int) (OptResult, error) {
	if f == nil || !IsFinite(a) || !IsFinite(b) {
		return OptResult{}, ErrInvalidArgument
	}
	if a > b {
		a, b = b, a
	}
	if !(tol > 0) {
		tol = 1e-8 * math.Max(b-a, 1e-300)
	}
	if maxIter <= 0 {
		maxIter = 200
	}
	res := OptResult{}
	eval := func(x float64) float64 {
		res.Evals++
		v := f(x)
		if v != v {
			return math.Inf(1)
		}
		return v
	}
	const invPhi = 0.6180339887498949
	c := b - invPhi*(b-a)
	d := a + invPhi*(b-a)
	fc, fd := eval(c), eval(d)
	for b-a > tol && res.Iter < maxIter {
		res.Iter++
		if fc < fd {
			b, d, fd = d, c, fc
			c = b - invPhi*(b-a)
			fc = eval(c)
		} else {
			a, c, fc = c, d, fd
			d = a + invPhi*(b-a)
			fd = eval(d)
		}
	}
	if fc < fd {
		res.X, res.F = []float64{c}, fc
	} else {
		res.X, res.F = []float64{d}, fd
	}
	res.Converged = b-a <= tol && !math.IsInf(res.F, 1)
	return res, nil
}

// GridSearch minimises f over the box [lo, hi] by a coarse-to-fine grid: it
// evaluates a regular grid with steps points per dimension (>= 2, default
// 5), then shrinks the box to one grid cell on either side of the best
// point and repeats, refine times (default 0 = a single pass). Dimensions
// with lo == hi are held fixed. It is derivative-free and robust for
// rough objectives but costs steps^dims evaluations per pass; more than
// 2,000,000 evaluations per pass is rejected.
//
// Errors: ErrInvalidArgument (nil f, empty or inconsistent bounds, lo > hi,
// non-finite bounds, grid too large). Converged is true when a finite
// minimum was found.
func GridSearch(f func(x []float64) float64, lo, hi []float64, steps, refine int) (OptResult, error) {
	n := len(lo)
	if f == nil || n == 0 || len(hi) != n {
		return OptResult{}, ErrInvalidArgument
	}
	if steps < 2 {
		steps = 5
	}
	if refine < 0 {
		refine = 0
	}
	a := make([]float64, n)
	b := make([]float64, n)
	total := 1.0
	for i := range lo {
		if !IsFinite(lo[i]) || !IsFinite(hi[i]) || lo[i] > hi[i] {
			return OptResult{}, ErrInvalidArgument
		}
		a[i], b[i] = lo[i], hi[i]
		if hi[i] > lo[i] {
			total *= float64(steps)
		}
	}
	if total > 2e6 {
		return OptResult{}, ErrInvalidArgument
	}
	res := OptResult{X: make([]float64, n), F: math.Inf(1)}
	copy(res.X, a)
	x := make([]float64, n)
	idx := make([]int, n)
	for pass := 0; pass <= refine; pass++ {
		res.Iter++
		for i := range idx {
			idx[i] = 0
		}
		for {
			for i := range x {
				if b[i] > a[i] {
					x[i] = a[i] + (b[i]-a[i])*float64(idx[i])/float64(steps-1)
				} else {
					x[i] = a[i]
				}
			}
			res.Evals++
			if v := f(x); v < res.F {
				res.F = v
				copy(res.X, x)
			}
			// Advance the odometer.
			d := 0
			for d < n {
				if b[d] > a[d] {
					idx[d]++
					if idx[d] < steps {
						break
					}
					idx[d] = 0
				}
				d++
			}
			if d == n {
				break
			}
		}
		for i := range a {
			if !(b[i] > a[i]) {
				continue
			}
			cell := (b[i] - a[i]) / float64(steps-1)
			a[i] = math.Max(res.X[i]-cell, lo[i])
			b[i] = math.Min(res.X[i]+cell, hi[i])
		}
	}
	res.Converged = !math.IsInf(res.F, 1)
	return res, nil
}
