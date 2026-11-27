package geo

import (
	"math"

	"airview/internal/geoid"
)

// Reference-atmosphere defaults (ITU-R P.453 exponential model; DESIGN §9.1).
const (
	// DefaultN0 is the surface refractivity in N-units used when no METAR
	// derived value is available.
	DefaultN0 = 315.0
	// DefaultScaleHeight is the refractivity scale height H in metres.
	DefaultScaleHeight = 7000.0
	// KAtmMin and KAtmMax bound the network scale factor k_atm (§9.2.6).
	KAtmMin = 0.8
	KAtmMax = 1.2
	// DefaultKFactor is the standard effective-earth-radius factor (4/3)
	// used by the line-of-sight check.
	DefaultKFactor = 4.0 / 3.0
)

// Atmosphere is the exponential refractivity profile
//
//	N(h) = K · N0 · exp(-h / H)       (N-units, n = 1 + N·1e-6)
//
// with h the height above mean sea level (the geoid): N0 is a sea-level
// refractivity (ITU-R P.453, METAR-derived). Ray paths take the EGM96
// undulation at the chord midpoint (one value per path; the geoid varies
// slowly) and evaluate the profile at h_HAE − N_geoid; without a geoid
// grid, and in the sphere test mode, the height above the ellipsoid
// (sphere) is used. K is the slowly estimated network
// scale factor k_atm. The zero value is not useful; use DefaultAtmosphere or
// call Normalize. Atmosphere is a small value type; its methods never
// modify it and are safe for concurrent use.
type Atmosphere struct {
	N0 float64 // surface refractivity, N-units (METAR-derived or DefaultN0)
	H  float64 // scale height, m
	K  float64 // k_atm scale factor, clamped to [KAtmMin, KAtmMax]

	// sphereR, when non-zero, replaces the ellipsoid by a sphere of this
	// radius for heights (tests and the distance-only legacy entry point).
	sphereR float64
}

// DefaultAtmosphere returns N0 = 315, H = 7 km, k_atm = 1.
func DefaultAtmosphere() Atmosphere {
	return Atmosphere{N0: DefaultN0, H: DefaultScaleHeight, K: 1}
}

// Normalize replaces missing or implausible parameters with defaults and
// clamps K to [KAtmMin, KAtmMax]. Plausible N0 is 200..450 and H 4..12 km.
func (a Atmosphere) Normalize() Atmosphere {
	if !(a.N0 >= 200 && a.N0 <= 450) {
		a.N0 = DefaultN0
	}
	if !(a.H >= 4000 && a.H <= 12000) {
		a.H = DefaultScaleHeight
	}
	switch {
	case !(a.K > 0): // includes NaN
		a.K = 1
	case a.K < KAtmMin:
		a.K = KAtmMin
	case a.K > KAtmMax:
		a.K = KAtmMax
	}
	return a
}

// N returns the refractivity (N-units) at height h (m above mean sea level).
// Heights below -1 km are treated as -1 km.
func (a *Atmosphere) N(h float64) float64 {
	if h < -1000 {
		h = -1000
	}
	return a.K * a.N0 * math.Exp(-h/a.H)
}

// 8-point Gauss-Legendre nodes and weights on [0, 1], plus the
// 6u(1-u) weights used to average the ray curvature (they sum to 1).
var gl8U, gl8W, gl8C [8]float64

func init() {
	// Nodes/weights on [-1, 1] (Abramowitz & Stegun table 25.4).
	x := [4]float64{0.1834346424956498, 0.5255324099163290, 0.7966664774136267, 0.9602898564975363}
	w := [4]float64{0.3626837833783620, 0.3137066458778873, 0.2223810344533745, 0.1012285362903763}
	for i := 0; i < 4; i++ {
		gl8U[3-i], gl8W[3-i] = (1-x[i])/2, w[i]/2
		gl8U[4+i], gl8W[4+i] = (1+x[i])/2, w[i]/2
	}
	var s float64
	for i := range gl8U {
		gl8C[i] = gl8W[i] * 6 * gl8U[i] * (1 - gl8U[i])
		s += gl8C[i]
	}
	for i := range gl8C {
		gl8C[i] /= s
	}
}

func (a *Atmosphere) height(v Vec3) float64 {
	if a.sphereR != 0 {
		return v.Norm() - a.sphereR
	}
	return GeocentricHeight(v)
}

// undulation returns the geoid undulation N (m, geoid above ellipsoid) at
// the midpoint of the chord p1-p2, 0 in the sphere mode or without a grid.
// The latitude is geocentric: its error (≤ 0.2°) is negligible against the
// smoothness of the geoid.
func (a *Atmosphere) undulation(p1, p2 Vec3) float64 {
	if a.sphereR != 0 {
		return 0
	}
	m := p1.Add(p2)
	if m.X == 0 && m.Y == 0 && m.Z == 0 {
		return 0
	}
	lat := math.Atan2(m.Z, math.Hypot(m.X, m.Y)) * (180 / math.Pi)
	lon := math.Atan2(m.Y, m.X) * (180 / math.Pi)
	n, ok := geoid.Undulation(lat, lon)
	if !ok || math.IsNaN(n) {
		return 0
	}
	return n
}

// path evaluates the optical path between two ECEF points.
//
// Model. In a horizontally stratified atmosphere the ray between the end
// points bulges upwards from the chord with curvature κ = |dn/dh|·cos(e)
// (e = local elevation of the ray). By Fermat's principle the optical
// length ∫n ds is stationary - in fact minimal - on the true ray, so the
// optical length of any nearby trial path overestimates it by a term of
// second order in the shape error. The trial paths here are the chord
// displaced perpendicularly (in the vertical plane) by
//
//	y(u) = D²·(A·u(1-u) + B·u(1-u)(1-2u)),   u ∈ [0, 1],
//
// i.e. a parabola plus an antisymmetric cubic that lets the bend
// concentrate at the low end. The geometric length is
// D + D³(A²/3 + B²/5)/2 and the refractivity is integrated with 8-point
// Gauss-Legendre quadrature at the displaced heights. (A, B) start from the
// curvature averaged along the chord (A = κ̄/2, B = 0) and take two Newton
// steps on the optical length, which is convex in (A, B). Against a numeric
// ray trace (raytrace_test.go) the result is within a few centimetres at
// 400 km.
//
// It returns the optical length L (m) and the chord D (m). When grad is not
// nil it also stores dL/dp2 there: the exact gradient of this model with
// respect to the second end point (envelope theorem: (A, B) are at the
// optimum, so only the explicit dependence on p2 remains; the small
// dependence of vzᵢ on p2 is neglected, < 1e-6).
func (a *Atmosphere) path(p1, p2 Vec3, grad *Vec3) (L, D float64) {
	d := p2.Sub(p1)
	D = d.Norm()
	if !(D > 1e-6) {
		if grad != nil {
			*grad = Vec3{}
		}
		return 0, 0
	}
	dh := d.Scale(1 / D)
	nf := a.K * a.N0
	invH := 1 / a.H
	gk := nf * 1e-6 * invH // |dn/dh| at h = 0

	var hc, vz [8]float64 // chord height (above MSL); vertical component of the displacement direction
	var ex, hx [8]float64 // e^(-h/H) and h of the latest evaluation per node (see the Newton loop)
	var kbar float64
	und := a.undulation(p1, p2)
	for i, u := range gl8U {
		p := p1.Add(d.Scale(u))
		hc[i] = a.height(p) - und
		r := p.Norm()
		se := 0.0
		if r > 0 {
			se = dh.Dot(p) / r
		}
		c2 := 1 - se*se
		if c2 < 0 {
			c2 = 0
		}
		vz[i] = math.Sqrt(c2)
		h := hc[i]
		if h < -1000 {
			h = -1000
		}
		ex[i], hx[i] = math.Exp(-h*invH), h
		kbar += gl8C[i] * gk * ex[i] * vz[i]
	}

	// Optical length F(A, B) = D + D³(A²/3 + B²/5)/2 + 1e-6·nf·D·Σ wᵢ e^(-hᵢ/H)
	// with hᵢ = hcᵢ + D²·vzᵢ·(A·b1ᵢ + B·b2ᵢ).
	D2, D3 := D*D, D*D*D
	cn := 1e-6 * nf * D
	A, B := kbar/2, 0.0
	var nInt float64
	var eN [8]float64 // wᵢ e^(-hᵢ/H) at the final (A, B), 0 where clamped
	// ex/hx: e^(-h/H) and h of the previous Newton pass. Once the iteration
	// has converged to a few per cent, the next pass's exponential is the
	// previous one times e^(-Δh/H) with a small argument, evaluated by its
	// Taylor series (relative error < δ⁶/720 < 2e-11 for |δ| < 0.05): this
	// saves most of the math.Exp calls of the later passes (H-track request).
	for it := 0; ; it++ {
		var g1, g2, h11, h12, h22 float64
		nInt = 0
		for i, u := range gl8U {
			b1 := u * (1 - u)
			b2 := b1 * (1 - 2*u)
			s := D2 * vz[i]
			h := hc[i] + s*(A*b1+B*b2)
			clamped := h < -1000
			if clamped {
				h = -1000
			}
			var x float64
			if dl := (h - hx[i]) * invH; math.Abs(dl) < 0.05 {
				x = ex[i] * (1 - dl*(1-dl*(1.0/2-dl*(1.0/6-dl*(1.0/24-dl*(1.0/120))))))
			} else {
				x = math.Exp(-h * invH)
			}
			ex[i], hx[i] = x, h
			e := gl8W[i] * x
			nInt += e
			if clamped {
				eN[i] = 0
			} else {
				eN[i] = e
			}
			// ∂/∂A e^(-h/H) = -(s·b1/H)·e, likewise for B.
			t1, t2 := s*b1*invH, s*b2*invH
			g1 -= t1 * e
			g2 -= t2 * e
			h11 += t1 * t1 * e
			h12 += t1 * t2 * e
			h22 += t2 * t2 * e
		}
		if it == 2 {
			break
		}
		g1 = cn*g1 + D3*A/3
		g2 = cn*g2 + D3*B/5
		h11 = cn*h11 + D3/3
		h12 = cn * h12
		h22 = cn*h22 + D3/5
		det := h11*h22 - h12*h12
		if !(det > 0) {
			break
		}
		A -= (h22*g1 - h12*g2) / det
		B -= (h11*g2 - h12*g1) / det
	}
	geo := (A*A/3 + B*B/5) / 2
	L = D + D3*geo + cn*nInt
	if grad == nil {
		return
	}
	// ∂L/∂p2 = d̂·(1 + 3D²·geo + 1e-6·nf·Σe) - (cn/H)·Σ eᵢ·∂hᵢ/∂p2,
	// ∂hᵢ/∂p2 = uᵢ·∇h(Pᵢ) + 2·D·vzᵢ·(A·b1ᵢ + B·b2ᵢ)·d̂.
	coefD := 1 + 3*D2*geo + 1e-6*nf*nInt
	var gh Vec3
	for i, u := range gl8U {
		if eN[i] == 0 {
			continue
		}
		b1 := u * (1 - u)
		b2 := b1 * (1 - 2*u)
		f := -cn * invH * eN[i]
		coefD += f * 2 * D * vz[i] * (A*b1 + B*b2)
		gh = gh.Add(a.heightGrad(p1.Add(d.Scale(u))).Scale(f * u))
	}
	*grad = dh.Scale(coefD).Add(gh)
	return
}

// heightGrad returns the gradient of height() at v.
func (a *Atmosphere) heightGrad(v Vec3) Vec3 {
	r := v.Norm()
	if r == 0 {
		return Vec3{}
	}
	rh := v.Scale(1 / r)
	if a.sphereR != 0 {
		return rh
	}
	// h = r - r/√q, q = (x²+y²)/a² + z²/b².
	q := (v.X*v.X+v.Y*v.Y)*invA2 + v.Z*v.Z*invB2
	sq := math.Sqrt(q)
	c := r / (q * sq) // r·q^(-3/2); ∇q = 2(x/a², y/a², z/b²)
	return rh.Scale(1 - 1/sq).Add(Vec3{v.X * invA2 * c, v.Y * invA2 * c, v.Z * invB2 * c})
}

// Range returns the refraction-corrected one-way optical path length in
// metres between two ECEF points: the radio propagation time multiplied by
// c. Symmetric in its arguments.
func (a *Atmosphere) Range(p1, p2 Vec3) float64 {
	L, _ := a.path(p1, p2, nil)
	return L
}

// Excess returns Range - |p2 - p1|: the tropospheric excess path (m),
// including the extra geometric length of the bent ray.
func (a *Atmosphere) Excess(p1, p2 Vec3) float64 {
	L, D := a.path(p1, p2, nil)
	return L - D
}

// PropagationNs returns the one-way radio propagation time between two ECEF
// points in nanoseconds.
func (a *Atmosphere) PropagationNs(p1, p2 Vec3) float64 {
	return a.Range(p1, p2) / C * 1e9
}

// RangeGrad returns the optical range from rx to tgt and its gradient with
// respect to the target position (ECEF), for analytic Jacobians. The
// gradient is that of the model itself (it matches a numeric ray trace's
// to ~1e-7). Its norm is n(tgt) ≈ 1.0003 and it points along the ray's
// arrival tangent, which is the chord direction tilted by the bending.
// The gradient with respect to rx is RangeGrad(tgt, rx).
func (a *Atmosphere) RangeGrad(rx, tgt Vec3) (L float64, grad Vec3) {
	L, _ = a.path(rx, tgt, &grad)
	return L, grad
}

// PropagationNsDist is the distance-only form for single-station code that
// knows the straight-line distance and the two end heights (above mean sea
// level, the refractivity datum) but not the
// positions (internal/latency). The geometry is rebuilt on a sphere of the
// mean earth radius; against the full ellipsoid this changes the result by
// well under a nanosecond.
func (a *Atmosphere) PropagationNsDist(distM, h1, h2 float64) float64 {
	if !(distM > 0) {
		return 0
	}
	b := *a
	b.sphereR = MeanEarthRadius
	r1, r2 := MeanEarthRadius+h1, MeanEarthRadius+h2
	ct := (r1*r1 + r2*r2 - distM*distM) / (2 * r1 * r2)
	ct = math.Max(-1, math.Min(1, ct))
	st := math.Sqrt(1 - ct*ct)
	p1 := Vec3{0, r1, 0}
	p2 := Vec3{r2 * st, r2 * ct, 0}
	return b.PropagationNs(p1, p2)
}

// StraightExcess is the closed-form excess (m) of a straight path of length
// D between heights h1 and h2 (above mean sea level) through the
// exponential profile (no ray
// bending, no earth curvature of the path):
//
//	1e-6·D·K·N0·H·(e^(-h1/H) - e^(-h2/H)) / (h2 - h1)
//
// It is cheap and adequate for steep or short paths; for long, low paths it
// underestimates by metres to tens of metres because it ignores the dip of
// the chord towards the ground. Use Excess there.
func (a *Atmosphere) StraightExcess(D, h1, h2 float64) float64 {
	if !(D > 0) {
		return 0
	}
	if math.Abs(h2-h1) < 1 {
		return 1e-6 * D * a.N((h1+h2)/2)
	}
	return 1e-6 * D * a.K * a.N0 * a.H * (math.Exp(-h1/a.H) - math.Exp(-h2/a.H)) / (h2 - h1)
}

// Clearance returns the minimum height (m above the ellipsoid) of the
// effective-earth ray between two ECEF points: the chord raised by the ray
// bulge of curvature (1 - 1/k)/R. A negative value means the earth blocks
// the path. k is the effective-earth-radius factor (k <= 0 selects 4/3).
// The minimum is found by golden-section search (the clearance profile is
// convex along the chord for k >= 1).
func Clearance(p1, p2 Vec3, k float64) float64 {
	if !(k > 0) {
		k = DefaultKFactor
	}
	d := p2.Sub(p1)
	D := d.Norm()
	h1, h2 := GeocentricHeight(p1), GeocentricHeight(p2)
	if D < 1 {
		return math.Min(h1, h2)
	}
	curv := (1 - 1/k) / MeanEarthRadius
	f := func(u float64) float64 {
		return GeocentricHeight(p1.Add(d.Scale(u))) + 0.5*curv*D*D*u*(1-u)
	}
	const g = 0.6180339887498949
	lo, hi := 0.0, 1.0
	x1, x2 := hi-g*(hi-lo), lo+g*(hi-lo)
	f1, f2 := f(x1), f(x2)
	for i := 0; i < 48 && hi-lo > 1e-7; i++ {
		if f1 < f2 {
			hi, x2, f2 = x2, x1, f1
			x1 = hi - g*(hi-lo)
			f1 = f(x1)
		} else {
			lo, x1, f1 = x1, x2, f2
			x2 = lo + g*(hi-lo)
			f2 = f(x2)
		}
	}
	return math.Min(math.Min(h1, h2), math.Min(f1, f2))
}

// LineOfSight reports whether the 4/3-earth ray between two ECEF points
// clears the ellipsoid by at least marginM metres.
func LineOfSight(p1, p2 Vec3, marginM float64) bool {
	return Clearance(p1, p2, DefaultKFactor) >= marginM
}

// RadioHorizon returns the maximum ground range (m) at which two points at
// heights h1 and h2 above a smooth earth can see each other over an
// effective earth of factor k (k <= 0 selects 4/3).
func RadioHorizon(h1, h2, k float64) float64 {
	if !(k > 0) {
		k = DefaultKFactor
	}
	re := k * MeanEarthRadius
	return math.Sqrt(2*re*math.Max(h1, 0)) + math.Sqrt(2*re*math.Max(h2, 0))
}
