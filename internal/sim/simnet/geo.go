package simnet

import "math"

// Geometry helpers. simnet keeps its own small WGS-84 implementation so it
// does not depend on the MLAT packages it is meant to test.

const (
	// SpeedOfLight in vacuum, m/s.
	SpeedOfLight = 299_792_458.0

	wgsA  = 6378137.0
	wgsF  = 1 / 298.257223563
	wgsB  = wgsA * (1 - wgsF)
	wgsE2 = wgsF * (2 - wgsF)
	wgsEP = (wgsA*wgsA - wgsB*wgsB) / (wgsB * wgsB)

	// earthMeanR is the mean earth radius used for the radio horizon.
	earthMeanR = 6371000.0

	deg = math.Pi / 180
	ft  = 0.3048
)

// Vec3 is an earth-centred, earth-fixed (ECEF) vector in metres.
type Vec3 [3]float64

// Sub returns a-b.
func (a Vec3) Sub(b Vec3) Vec3 { return Vec3{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }

// Add returns a+b.
func (a Vec3) Add(b Vec3) Vec3 { return Vec3{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }

// Scale returns k*a.
func (a Vec3) Scale(k float64) Vec3 { return Vec3{a[0] * k, a[1] * k, a[2] * k} }

// Dot returns the scalar product.
func (a Vec3) Dot(b Vec3) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

// Norm returns the Euclidean length.
func (a Vec3) Norm() float64 { return math.Sqrt(a.Dot(a)) }

// ECEF converts WGS-84 geodetic coordinates (degrees, metres above the
// ellipsoid) to ECEF.
func ECEF(latDeg, lonDeg, hae float64) Vec3 {
	sl, cl := math.Sincos(latDeg * deg)
	so, co := math.Sincos(lonDeg * deg)
	n := wgsA / math.Sqrt(1-wgsE2*sl*sl)
	return Vec3{(n + hae) * cl * co, (n + hae) * cl * so, (n*(1-wgsE2) + hae) * sl}
}

// Geodetic converts ECEF to WGS-84 latitude, longitude (degrees) and height
// above the ellipsoid (metres), using Bowring's method with one refinement
// (sub-millimetre for points near the earth's surface and aircraft heights).
func Geodetic(p Vec3) (latDeg, lonDeg, hae float64) {
	x, y, z := p[0], p[1], p[2]
	r := math.Hypot(x, y)
	lon := math.Atan2(y, x)
	if r < 1e-6 {
		lat := math.Pi / 2
		if z < 0 {
			lat = -lat
		}
		return lat / deg, lon / deg, math.Abs(z) - wgsB
	}
	beta := math.Atan2(z*wgsA, r*wgsB)
	var lat float64
	for i := 0; i < 3; i++ {
		sb, cb := math.Sincos(beta)
		lat = math.Atan2(z+wgsEP*wgsB*sb*sb*sb, r-wgsE2*wgsA*cb*cb*cb)
		beta = math.Atan2(wgsB*math.Sin(lat), wgsA*math.Cos(lat))
	}
	sl, cl := math.Sincos(lat)
	n := wgsA / math.Sqrt(1-wgsE2*sl*sl)
	var h float64
	if math.Abs(cl) > 1e-9 {
		h = r/cl - n
	} else {
		h = math.Abs(z)/math.Abs(sl) - n*(1-wgsE2)
	}
	return lat / deg, lon / deg, h
}

// Up returns the local vertical unit vector (ellipsoid normal) at a
// geodetic position.
func Up(latDeg, lonDeg float64) Vec3 {
	sl, cl := math.Sincos(latDeg * deg)
	so, co := math.Sincos(lonDeg * deg)
	return Vec3{cl * co, cl * so, sl}
}

// enuBasis returns the east, north and up unit vectors at a position.
func enuBasis(latDeg, lonDeg float64) (e, n, u Vec3) {
	sl, cl := math.Sincos(latDeg * deg)
	so, co := math.Sincos(lonDeg * deg)
	e = Vec3{-so, co, 0}
	n = Vec3{-sl * co, -sl * so, cl}
	u = Vec3{cl * co, cl * so, sl}
	return
}

// offsetLatLon moves a position by east/north metres (small offsets).
func offsetLatLon(latDeg, lonDeg, eastM, northM float64) (float64, float64) {
	sl := math.Sin(latDeg * deg)
	rn := wgsA / math.Sqrt(1-wgsE2*sl*sl)
	rm := rn * (1 - wgsE2) / (1 - wgsE2*sl*sl)
	lat := latDeg + northM/rm/deg
	cl := math.Cos(latDeg * deg)
	if cl < 1e-6 {
		cl = 1e-6
	}
	lon := wrapLon(lonDeg + eastM/(rn*cl)/deg)
	return lat, lon
}

// destination returns the point at distance m and bearing brgDeg from a start
// point on a sphere (adequate for receiver placement).
func destination(latDeg, lonDeg, brgDeg, m float64) (float64, float64) {
	d := m / earthMeanR
	la1, lo1, b := latDeg*deg, lonDeg*deg, brgDeg*deg
	sl1, cl1 := math.Sincos(la1)
	sd, cd := math.Sincos(d)
	la2 := math.Asin(sl1*cd + cl1*sd*math.Cos(b))
	lo2 := lo1 + math.Atan2(math.Sin(b)*sd*cl1, cd-sl1*math.Sin(la2))
	return la2 / deg, wrapLon(lo2 / deg)
}

// groundDistance is the great-circle distance in metres (spherical).
func groundDistance(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := lat1*deg, lat2*deg
	dp, dl := p2-p1, (lon2-lon1)*deg
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * earthMeanR * math.Asin(math.Min(1, math.Sqrt(a)))
}

func wrapLon(lon float64) float64 {
	for lon > 180 {
		lon -= 360
	}
	for lon < -180 {
		lon += 360
	}
	return lon
}

// Atmosphere is the refractivity model used for the propagation delay and
// the radio horizon.
//
// Refractivity follows the exponential reference profile
// N(h) = N0·exp(−h/H) (N-units, 1e-6), so a radio wave on a path through the
// troposphere travels a longer electrical distance than the straight line.
// The excess is the integral of N(h)·1e-6 along the chord, scaled by KAtm
// (the network's bulk correction factor the MLAT engine estimates as k_atm).
// Ray bending (a sub-metre effect at the elevations ADS-B uses) is not
// modelled separately.
type Atmosphere struct {
	N0           float64 // surface refractivity, N-units (default 315)
	ScaleHeightM float64 // H, metres (default 7000)
	KAtm         float64 // multiplier on the excess (default 1; 0.8..1.2 is realistic)
	KFactor      float64 // effective earth radius factor for the radio horizon (default 4/3)
}

func (a *Atmosphere) normalize() {
	if !(a.N0 > 0) {
		a.N0 = 315
	}
	if !(a.ScaleHeightM > 0) {
		a.ScaleHeightM = 7000
	}
	if !(a.KAtm > 0) {
		a.KAtm = 1
	}
	if !(a.KFactor > 0) {
		a.KFactor = 4.0 / 3
	}
}

// 4-point Gauss-Legendre nodes and weights on [0, 1].
var (
	glX = [4]float64{0.0694318442029737, 0.3300094782075719, 0.6699905217924281, 0.9305681557970263}
	glW = [4]float64{0.1739274225687269, 0.3260725774312731, 0.3260725774312731, 0.1739274225687269}
)

// ExcessPathM returns the refraction excess path length in metres between a
// receiver at rx (ECEF, with rxR = |rx| − its height above the ellipsoid, the
// local geocentric radius of the ellipsoid) and a transmitter at tx. Heights
// along the chord are measured as the distance from the earth centre minus
// rxR, which follows the dip of a long, low chord correctly.
func (a *Atmosphere) ExcessPathM(rx, tx Vec3, rxR, slant float64) float64 {
	var s float64
	d := tx.Sub(rx)
	invH := 1 / a.ScaleHeightM
	for i, x := range glX {
		p := rx.Add(d.Scale(x))
		h := p.Norm() - rxR
		if h < 0 {
			h = 0
		}
		s += glW[i] * math.Exp(-h*invH)
	}
	return a.KAtm * a.N0 * 1e-6 * s * slant
}

// horizonM returns sqrt(2·k·R·h): the distance to the radio horizon of an
// antenna h metres above the surface.
func (a *Atmosphere) horizonM(h float64) float64 {
	if h < 1 {
		h = 1
	}
	return math.Sqrt(2 * a.KFactor * earthMeanR * h)
}
