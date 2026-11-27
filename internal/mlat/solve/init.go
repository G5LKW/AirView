package solve

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// Closed-form and search initialisations (DESIGN §9.4). All of them ignore
// the refraction excess (tens of metres at most) and the clock covariance:
// they only have to land in the basin of the LM solution.
//
// Notation (see doc.go): station i at rᵢ (ECEF) receives at zᵢ = c·tᵢ
// (metres, relative to a common reference); the unknowns are the position x
// and b = c·τ. The noise-free model |x − rᵢ| = zᵢ − b squares to
//
//	⟨aᵢ, y⟩ = αᵢ + Λ,  aᵢ = (rᵢ, zᵢ), y = (x, b), αᵢ = ½⟨aᵢ, aᵢ⟩, Λ = ½⟨y, y⟩
//
// with the Lorentz product ⟨a, b⟩ = a₁b₁ + a₂b₂ + a₃b₃ − a₄b₄.

// lorentz returns the Lorentz inner product of two 4-vectors.
func lorentz(a, b *[4]float64) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] - a[3]*b[3] }

// activeToa writes the active TOA station indices into s.idx and returns
// their count.
func (s *Solver) activeToa() []int {
	n := 0
	for k := 0; k < s.nr; k++ {
		if i := s.rows[k]; i < MaxStations {
			s.idx[n] = i
			n++
		}
	}
	return s.idx[:n]
}

// zMean returns the mean of z over idx (subtracted to keep the Lorentz
// system well scaled; a common time shift is absorbed by b).
func (s *Solver) zMean(idx []int) float64 {
	var zm float64
	for _, i := range idx {
		zm += s.zc[i]
	}
	return zm / float64(len(idx))
}

// bancroft returns up to two candidate positions from the TOAs of idx
// (len ≥ 4) by Bancroft's method: with B the matrix of rows
// (rᵢ, −zᵢ), By = α + Λ·1 gives y = q + Λ·p (q = B⁺α, p = B⁺1 in the
// least-squares sense for more than 4 rows), and Λ = ½⟨y, y⟩ becomes
//
//	⟨p,p⟩Λ² + 2(⟨p,q⟩ − 1)Λ + ⟨q,q⟩ = 0.
//
// Each real root gives a candidate. ECEF coordinates are used on purpose: a
// station-centred origin makes the rows linearly dependent for 4 stations.
func (s *Solver) bancroft(idx []int, out []geo.Vec3) int {
	n := len(idx)
	if n < 4 {
		return 0
	}
	zm := s.zMean(idx)
	a := s.tmpB[:n*4]
	al := s.tmpV[:n]
	one := s.tmpV2[:n]
	for k, i := range idx {
		r := s.pos[i]
		z := s.zc[i] - zm
		a[k*4+0], a[k*4+1], a[k*4+2], a[k*4+3] = r.X, r.Y, r.Z, -z
		al[k] = 0.5 * (r.Dot(r) - z*z)
		one[k] = 1
	}
	linalg.QR(a, n, 4, s.tau[:], s.qrw[:])
	var q, p [4]float64
	if _, err := linalg.QRSolve(a, n, 4, s.tau[:], al, q[:]); err != nil {
		return 0
	}
	if _, err := linalg.QRSolve(a, n, 4, s.tau[:], one, p[:]); err != nil {
		return 0
	}
	A := lorentz(&p, &p)
	B := 2 * (lorentz(&p, &q) - 1)
	C := lorentz(&q, &q)
	var roots [2]float64
	nr := 0
	switch {
	case A == 0:
		if B != 0 {
			roots[0] = -C / B
			nr = 1
		}
	default:
		d := B*B - 4*A*C
		if d < 0 {
			// Noise pushed the discriminant below zero: take the double
			// root (closest real approximation).
			d = 0
		}
		sq := math.Sqrt(d)
		qq := -0.5 * (B + math.Copysign(sq, B))
		if qq != 0 {
			roots[0], roots[1] = qq/A, C/qq
			nr = 2
		} else {
			roots[0] = -B / (2 * A)
			nr = 1
		}
	}
	cnt := 0
	for k := 0; k < nr && cnt < len(out); k++ {
		L := roots[k]
		x := geo.Vec3{X: q[0] + L*p[0], Y: q[1] + L*p[1], Z: q[2] + L*p[2]}
		if finiteVec(x) {
			out[cnt] = x
			cnt++
		}
	}
	return cnt
}

// linearToa is the linear TOA least-squares initialisation (N ≥ 5):
// differencing the squared equations against a reference station 0
// eliminates Λ and leaves N−1 linear equations
//
//	(rᵢ − r₀)·x − (zᵢ − z₀)·b = αᵢ − α₀
//
// in the four unknowns, solved by Householder QR.
func (s *Solver) linearToa(idx []int) (geo.Vec3, bool) {
	n := len(idx)
	if n < 5 {
		return geo.Vec3{}, false
	}
	// reference: the best-timed station
	ref := idx[0]
	for _, i := range idx {
		if s.sd[i] < s.sd[ref] {
			ref = i
		}
	}
	zm := s.zMean(idx)
	r0, z0 := s.pos[ref], s.zc[ref]-zm
	a := s.tmpB[:(n-1)*4]
	rhs := s.tmpV[:n-1]
	k := 0
	for _, i := range idx {
		if i == ref {
			continue
		}
		ri, zi := s.pos[i], s.zc[i]-zm
		d := ri.Sub(r0)
		a[k*4+0], a[k*4+1], a[k*4+2], a[k*4+3] = d.X, d.Y, d.Z, -(zi - z0)
		// αᵢ − α₀ without cancellation: ½((rᵢ−r₀)·(rᵢ+r₀) − (zᵢ−z₀)(zᵢ+z₀))
		rhs[k] = 0.5 * (d.Dot(ri.Add(r0)) - (zi-z0)*(zi+z0))
		k++
	}
	var y [4]float64
	linalg.QR(a, n-1, 4, s.tau[:], s.qrw[:])
	if _, err := linalg.QRSolve(a, n-1, 4, s.tau[:], rhs, y[:]); err != nil {
		return geo.Vec3{}, false
	}
	x := geo.Vec3{X: y[0], Y: y[1], Z: y[2]}
	return x, finiteVec(x)
}

// quartic is the closed-form 3 + altitude initialisation. The altitude
// surface is approximated by a sphere |x| = R about the earth's centre
// (R = geocentric radius of the ellipsoid at the candidate latitude plus
// the height). With A the 3×3 matrix of rows rᵢ, Ax = α + Λ·1 + b·z gives
//
//	x(b) = u + Λv + bw,  u = A⁻¹α, v = A⁻¹1, w = A⁻¹z,  Λ = ½(R² − b²),
//
// so x(b) = P + Qb + Sb² with P = u + ½R²v, Q = w, S = −½v, and |x(b)|² = R²
// is a quartic in b. Its real roots on the physical interval
// b ∈ [max zᵢ − MaxRangeM, min zᵢ] (ranges zᵢ − b between 0 and MaxRangeM)
// are found by a sign-change scan plus bisection; near-tangent minima of
// |f| (noise can lift a double root off the axis) are kept as well. Each
// root is refined twice with the radius at its own latitude.
func (s *Solver) quartic(idx []int, h float64, out []geo.Vec3) int {
	if len(idx) != 3 {
		return 0
	}
	zm := s.zMean(idx)
	var A linalg.Mat3
	var al, one, zz linalg.Vec3
	for k, i := range idx {
		r := s.pos[i]
		z := s.zc[i] - zm
		A[k*3+0], A[k*3+1], A[k*3+2] = r.X, r.Y, r.Z
		al[k] = 0.5 * (r.Dot(r) - z*z)
		one[k] = 1
		zz[k] = z
	}
	u, err1 := A.Solve(al)
	v, err2 := A.Solve(one)
	w, err3 := A.Solve(zz)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0
	}
	zmin, zmax := math.Inf(1), math.Inf(-1)
	for _, i := range idx {
		zmin = math.Min(zmin, s.zc[i]-zm)
		zmax = math.Max(zmax, s.zc[i]-zm)
	}
	lo, hi := zmax-s.opt.MaxRangeM, zmin
	if !(hi > lo) {
		return 0
	}
	cl := geo.FromECEF(s.centroid)
	R0 := geo.ToECEF(cl.Lat, cl.Lon, h).Norm()

	xAt := func(b, R float64) geo.Vec3 {
		L := 0.5 * (R*R - b*b)
		return geo.Vec3{X: u[0] + L*v[0] + b*w[0], Y: u[1] + L*v[1] + b*w[1], Z: u[2] + L*v[2] + b*w[2]}
	}
	f := func(b, R float64) float64 {
		x := xAt(b, R)
		return x.Dot(x) - R*R
	}
	const ns = 2000
	step := (hi - lo) / ns
	cnt := 0
	add := func(b float64) {
		R := R0
		var x geo.Vec3
		for it := 0; it < 6; it++ {
			x = xAt(b, R)
			ll := geo.FromECEF(x)
			R = geo.ToECEF(ll.Lat, ll.Lon, h).Norm()
			// one Newton step on f(b) = 0 for the updated radius
			const db = 1.0
			f0 := f(b, R)
			d := (f(b+db, R) - f(b-db, R)) / (2 * db)
			if d != 0 && math.Abs(f0/d) < 50000 {
				b -= f0 / d
			}
		}
		x = xAt(b, R)
		if !finiteVec(x) || cnt >= len(out) {
			return
		}
		for k := 0; k < cnt; k++ {
			if out[k].Dist(x) < 500 {
				return
			}
		}
		out[cnt] = x
		cnt++
	}
	bisect := func(a0, a1 float64) float64 {
		fa := f(a0, R0)
		for it := 0; it < 60; it++ {
			mid := 0.5 * (a0 + a1)
			fm := f(mid, R0)
			if (fm < 0) == (fa < 0) {
				a0, fa = mid, fm
			} else {
				a1 = mid
			}
		}
		return 0.5 * (a0 + a1)
	}
	// golden-section search for the extremum of f on [a0, a1]; sgn = +1
	// for a minimum, -1 for a maximum
	extremum := func(a0, a1, sgn float64) float64 {
		const g = 0.6180339887498949
		x1, x2 := a1-g*(a1-a0), a0+g*(a1-a0)
		f1, f2 := sgn*f(x1, R0), sgn*f(x2, R0)
		for it := 0; it < 50; it++ {
			if f1 < f2 {
				a1, x2, f2 = x2, x1, f1
				x1 = a1 - g*(a1-a0)
				f1 = sgn * f(x1, R0)
			} else {
				a0, x1, f1 = x1, x2, f2
				x2 = a0 + g*(a1-a0)
				f2 = sgn * f(x2, R0)
			}
		}
		return 0.5 * (a0 + a1)
	}
	fPrev2, fPrev := math.NaN(), f(lo, R0)
	for k := 1; k <= ns; k++ {
		b := lo + float64(k)*step
		fb := f(b, R0)
		if (fPrev < 0) != (fb < 0) {
			add(bisect(b-step, b))
		} else if !math.IsNaN(fPrev2) && (fPrev-fPrev2)*(fb-fPrev) < 0 {
			// sampled extremum at b−step: two close roots may hide inside
			// [b−2·step, b]
			sgn := 1.0
			if fPrev > fPrev2 {
				sgn = -1
			}
			e := extremum(b-2*step, b, sgn)
			fe := f(e, R0)
			switch {
			case (fe < 0) != (fPrev < 0):
				add(bisect(b-2*step, e))
				add(bisect(e, b))
			case math.Abs(fe) < 2*R0*2000:
				// near-tangent: noise lifted a double root off the axis
				add(e)
			}
		}
		fPrev2, fPrev = fPrev, fb
	}
	return cnt
}

// gridSeeds is the 3 + altitude grid search of DESIGN §9.4: the TOA misfit
// (with b eliminated in closed form under the full covariance) is evaluated
// on a grid of the given step over ±GridHalfM around the station centroid at height
// h; the best (up to len(out)) local minima are returned.
func (s *Solver) gridSeeds(idx []int, h, step float64, out []geo.Vec3) int {
	n := len(idx)
	if n < 3 {
		return 0
	}
	// A = Σ⁻¹ of the active TOA rows
	const MS = MaxStations
	A := s.tmpA[:n*n]
	for a, i := range idx {
		for b, j := range idx {
			A[a*n+b] = s.sig[i*MS+j]
		}
	}
	if err := linalg.SPDInverse(A, n, s.invWork[:n]); err != nil {
		return 0
	}
	var a1 float64 // 1ᵀA1
	for i := 0; i < n*n; i++ {
		a1 += A[i]
	}
	cll := geo.FromECEF(s.centroid)
	f := geo.NewFrame(geo.LLH{Lat: cll.Lat, Lon: cll.Lon, H: 0})
	Rg := geo.GaussianRadius(cll.Lat)
	half := s.opt.GridHalfM
	g := int(2*half/step) + 1
	grid := s.grid[:g*g]
	rv := s.tmpV[:n]
	ar := s.tmpV2[:n]
	for iy := 0; iy < g; iy++ {
		yn := -half + float64(iy)*step
		for ix := 0; ix < g; ix++ {
			xe := -half + float64(ix)*step
			up := h - (xe*xe+yn*yn)/(2*Rg)
			p := f.FromENU(geo.Vec3{X: xe, Y: yn, Z: up})
			for k, i := range idx {
				rv[k] = s.z[i] - p.Dist(s.pos[i])
			}
			// misfit min_b (r−b1)ᵀA(r−b1) = rᵀAr − (1ᵀAr)²/(1ᵀA1)
			var rar, oar float64
			for a := 0; a < n; a++ {
				ar[a] = linalg.Dot(A[a*n:a*n+n], rv)
				rar += rv[a] * ar[a]
				oar += ar[a]
			}
			grid[iy*g+ix] = rar - oar*oar/a1
		}
	}
	// local minima, best first
	cnt := 0
	var best [4]float64
	var bestXY [4][2]int
	for iy := 0; iy < g; iy++ {
		for ix := 0; ix < g; ix++ {
			v := grid[iy*g+ix]
			isMin := true
			for dy := -1; dy <= 1 && isMin; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					jy, jx := iy+dy, ix+dx
					if jy < 0 || jx < 0 || jy >= g || jx >= g {
						continue
					}
					if grid[jy*g+jx] < v {
						isMin = false
						break
					}
				}
			}
			if !isMin {
				continue
			}
			lim := len(out)
			if lim > len(best) {
				lim = len(best)
			}
			// insertion into the sorted best list
			pos := cnt
			for pos > 0 && best[pos-1] > v {
				pos--
			}
			if pos >= lim {
				continue
			}
			if cnt < lim {
				cnt++
			}
			for k := cnt - 1; k > pos; k-- {
				best[k], bestXY[k] = best[k-1], bestXY[k-1]
			}
			best[pos], bestXY[pos] = v, [2]int{ix, iy}
		}
	}
	for k := 0; k < cnt; k++ {
		xe := -half + float64(bestXY[k][0])*step
		yn := -half + float64(bestXY[k][1])*step
		up := h - (xe*xe+yn*yn)/(2*Rg)
		out[k] = f.FromENU(geo.Vec3{X: xe, Y: yn, Z: up})
	}
	return cnt
}

// centroidSeed returns the station centroid lifted to height h.
func (s *Solver) centroidSeed(h float64) geo.Vec3 {
	ll := geo.FromECEF(s.centroid)
	return geo.ToECEF(ll.Lat, ll.Lon, h)
}

// withExcess re-runs a closed-form initialisation with the TOAs corrected
// by the refraction excess evaluated at its first answer x0 (the closed
// forms assume straight-line ranges; the excess is tens of metres and the
// DOP can amplify the resulting seed error to kilometres). The cheap
// straight-path closed form of the excess is good enough here: the exact
// model is linearised at the seed afterwards. It returns the corrected
// root nearest to x0, or x0 when the second pass fails.
func (s *Solver) withExcess(x0 geo.Vec3, solve func(out []geo.Vec3) int) geo.Vec3 {
	if s.opt.NoRefraction {
		return x0
	}
	h0 := geo.GeocentricHeight(x0)
	for i := 0; i < s.m; i++ {
		d := x0.Dist(s.pos[i])
		s.zc[i] = s.z[i] - s.atm.StraightExcess(d, geo.GeocentricHeight(s.pos[i]), h0)
	}
	xs := s.exs[:]
	k := solve(xs)
	copy(s.zc[:s.m], s.z[:s.m])
	best, bd := x0, math.Inf(1)
	for j := 0; j < k; j++ {
		if d := xs[j].Dist(x0); d < bd {
			best, bd = xs[j], d
		}
	}
	if bd > 50000 {
		return x0
	}
	return best
}
