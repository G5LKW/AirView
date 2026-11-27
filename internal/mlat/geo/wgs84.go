// Package geo is the geometry layer of the AirView MLAT engine (DESIGN §9.1):
//
//   - WGS-84 geodetic <-> Earth-centred Earth-fixed (ECEF) conversion, with a
//     closed-form inverse (no iteration on the hot path);
//   - local East-North-Up (ENU) frames for the solver and tracker;
//   - the refraction-corrected radio propagation model: geometric range plus
//     the tropospheric excess path of an exponential refractivity profile
//     N(h) = k_atm·N0·exp(-h/H), integrated along an approximation of the bent
//     ray (validated against a numeric ray trace in the tests);
//   - an effective-earth (4/3) line-of-sight check.
//
// Conventions: positions are ECEF metres (Vec3); latitudes/longitudes are
// WGS-84 degrees, north/east positive; heights are metres above the
// ellipsoid (HAE). Everything is a pure function or an immutable value, safe
// for concurrent use, and allocation-free.
//
// This package deliberately has no dependency on the older internal/geo
// (which works in kilometres and great circles for the UI and statistics).
package geo

import "math"

// WGS-84 ellipsoid (NIMA TR8350.2) and physical constants.
const (
	WGS84A  = 6378137.0             // semi-major axis, m
	WGS84F  = 1.0 / 298.257223563   // flattening
	WGS84B  = WGS84A * (1 - WGS84F) // semi-minor axis, m
	WGS84E2 = WGS84F * (2 - WGS84F) // first eccentricity squared
	wgs84E4 = WGS84E2 * WGS84E2     // e^4
	invA2   = 1 / (WGS84A * WGS84A) // 1/a²
	invB2   = 1 / (WGS84B * WGS84B) // 1/b²
	oneMe2  = 1 - WGS84E2           // 1 - e²
	oneMe2A = (1 - WGS84E2) / (WGS84A * WGS84A)

	// C is the speed of light in vacuum, m/s.
	C = 299792458.0

	// MeanEarthRadius is the IUGG mean radius R1 = (2a+b)/3, m.
	MeanEarthRadius = (2*WGS84A + WGS84B) / 3

	deg = math.Pi / 180
)

// Vec3 is a 3-vector, normally an ECEF position or displacement in metres.
type Vec3 struct{ X, Y, Z float64 }

// Add returns v+w.
func (v Vec3) Add(w Vec3) Vec3 { return Vec3{v.X + w.X, v.Y + w.Y, v.Z + w.Z} }

// Sub returns v-w.
func (v Vec3) Sub(w Vec3) Vec3 { return Vec3{v.X - w.X, v.Y - w.Y, v.Z - w.Z} }

// Scale returns s·v.
func (v Vec3) Scale(s float64) Vec3 { return Vec3{s * v.X, s * v.Y, s * v.Z} }

// Dot returns v·w.
func (v Vec3) Dot(w Vec3) float64 { return v.X*w.X + v.Y*w.Y + v.Z*w.Z }

// Cross returns v×w.
func (v Vec3) Cross(w Vec3) Vec3 {
	return Vec3{v.Y*w.Z - v.Z*w.Y, v.Z*w.X - v.X*w.Z, v.X*w.Y - v.Y*w.X}
}

// Norm returns |v|.
func (v Vec3) Norm() float64 { return math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z) }

// Unit returns v/|v| (the zero vector for v = 0).
func (v Vec3) Unit() Vec3 {
	n := v.Norm()
	if n == 0 {
		return Vec3{}
	}
	return v.Scale(1 / n)
}

// Dist returns |v-w|.
func (v Vec3) Dist(w Vec3) float64 { return v.Sub(w).Norm() }

// LLH is a WGS-84 geodetic position: latitude and longitude in degrees and
// height above the ellipsoid in metres.
type LLH struct{ Lat, Lon, H float64 }

// ECEF converts the geodetic position to ECEF metres.
func (p LLH) ECEF() Vec3 { return ToECEF(p.Lat, p.Lon, p.H) }

// ToECEF converts WGS-84 latitude/longitude (degrees) and height above the
// ellipsoid (m) to ECEF metres.
func ToECEF(latDeg, lonDeg, h float64) Vec3 {
	sl, cl := math.Sincos(latDeg * deg)
	so, co := math.Sincos(lonDeg * deg)
	n := WGS84A / math.Sqrt(1-WGS84E2*sl*sl) // prime vertical radius of curvature
	return Vec3{(n + h) * cl * co, (n + h) * cl * so, (n*oneMe2 + h) * sl}
}

// FromECEF converts ECEF metres to WGS-84 geodetic coordinates. It uses
// Vermeille's closed-form solution (J. Geodesy 76, 2002), which is exact to
// rounding for every point further than ~45 km from the earth's centre; the
// remaining region (never met in practice) falls back to a converging
// Newton iteration. Longitude is in (-180, 180]; at the poles it is 0.
func FromECEF(v Vec3) LLH {
	x, y, z := v.X, v.Y, v.Z
	rho2 := x*x + y*y
	rho := math.Sqrt(rho2)
	p := rho2 * invA2
	q := oneMe2A * z * z
	r := (p + q - wgs84E4) / 6
	if r > 0 {
		s := wgs84E4 * p * q / (4 * r * r * r)
		t := math.Cbrt(1 + s + math.Sqrt(s*(2+s)))
		u := r * (1 + t + 1/t)
		vv := math.Sqrt(u*u + wgs84E4*q)
		w := WGS84E2 * (u + vv - q) / (2 * vv)
		k := math.Sqrt(u+vv+w*w) - w
		d := k * rho / (k + WGS84E2)
		dz := math.Hypot(d, z)
		lat := 2 * math.Atan2(z, d+dz)
		h := (k + WGS84E2 - 1) / k * dz
		return LLH{lat / deg, lon(x, y), h}
	}
	return fromECEFIter(x, y, z, rho)
}

func lon(x, y float64) float64 {
	if x == 0 && y == 0 {
		return 0
	}
	return math.Atan2(y, x) / deg
}

// fromECEFIter handles points deep inside the earth (r <= 0 above), where
// the closed form breaks down and a point may have several normals. It
// finds a foot point (a·cos β, b·sin β) on the meridian ellipse whose normal
// passes through the point, by bisection on the parametric latitude β of
//
//	f(β) = a·ρ·sin β - b·|z|·cos β - (a² - b²)·sin β·cos β = 0,
//
// which has f(0) <= 0 <= f(π/2), so a root always exists. Only reached on
// test or garbage input; any root gives a position that round-trips.
func fromECEFIter(x, y, z, rho float64) LLH {
	if rho == 0 && z == 0 {
		return LLH{0, 0, -WGS84B}
	}
	az := math.Abs(z)
	const a, b = WGS84A, WGS84B
	f := func(beta float64) float64 {
		s, c := math.Sincos(beta)
		return a*rho*s - b*az*c - (a*a-b*b)*s*c
	}
	lo, hi := 0.0, math.Pi/2
	for i := 0; i < 200 && hi-lo > 1e-16; i++ {
		mid := (lo + hi) / 2
		if f(mid) <= 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	beta := (lo + hi) / 2
	sb, cb := math.Sincos(beta)
	lat := math.Atan2(a*sb, b*cb)
	sl, cl := math.Sincos(lat)
	h := (rho-a*cb)*cl + (az-b*sb)*sl
	if z < 0 {
		lat = -lat
	}
	return LLH{lat / deg, lon(x, y), h}
}

// GeocentricHeight is a cheap approximation of the height above the
// ellipsoid: |v| minus the ellipsoid radius along the same geocentric
// direction. It differs from the geodetic height by less than
// h·(1-cos δ) with δ ≤ 0.2° the angle between the normal and the radius
// (< 0.1 m at 15 km), which is far below what the refraction model needs.
func GeocentricHeight(v Vec3) float64 {
	r2 := v.X*v.X + v.Y*v.Y + v.Z*v.Z
	if r2 == 0 {
		return -WGS84A
	}
	q := (v.X*v.X+v.Y*v.Y)*invA2 + v.Z*v.Z*invB2
	r := math.Sqrt(r2)
	return r - r/math.Sqrt(q)
}

// Up returns the ellipsoid normal (geodetic vertical) at a latitude and
// longitude in degrees.
func Up(latDeg, lonDeg float64) Vec3 {
	sl, cl := math.Sincos(latDeg * deg)
	so, co := math.Sincos(lonDeg * deg)
	return Vec3{cl * co, cl * so, sl}
}

// SurfaceRadius returns the geocentric radius of the ellipsoid at a
// geodetic latitude in degrees.
func SurfaceRadius(latDeg float64) float64 {
	return ToECEF(latDeg, 0, 0).Norm()
}

// GaussianRadius returns the Gaussian mean radius of curvature sqrt(M·N)
// at a geodetic latitude (degrees): the radius of the best-fitting local
// sphere, used by the refraction and line-of-sight models.
func GaussianRadius(latDeg float64) float64 {
	sl := math.Sin(latDeg * deg)
	w := 1 - WGS84E2*sl*sl
	return WGS84A * math.Sqrt(oneMe2) / w
}
