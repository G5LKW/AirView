package trajectory

import "math"

const (
	deg2rad = math.Pi / 180
	rad2deg = 180 / math.Pi
	ftPerM  = 1 / 0.3048
	mPerFt  = 0.3048
	ktToMS  = 1852.0 / 3600.0
	msToKt  = 3600.0 / 1852.0
	// G0 is standard gravity (CODATA, exact), m/s^2.
	G0 = 9.80665

	wgs84A  = 6378137.0
	wgs84F  = 1 / 298.257223563
	wgs84E2 = wgs84F * (2 - wgs84F)
)

// frame is a local tangent plane (x east, y north, metres) around an origin.
//
// It is an azimuthal-equidistant projection on an auxiliary sphere of radius
// N (prime-vertical radius of curvature at the origin) on which latitude
// offsets are stretched by M/N (M = meridional radius). The local scale is
// therefore exact in both axes at the origin (WGS84), the mapping is exactly
// invertible, and distances / azimuths from the origin follow great circles.
// Error versus Vincenty is below 1e-4 relative within 100 km.
type frame struct {
	lat0, lon0 float64 // deg
	phi0, lam0 float64 // rad
	sin0, cos0 float64
	k          float64 // M/N
	r          float64 // N, metres
}

func newFrame(lat, lon float64) frame {
	var f frame
	f.lat0, f.lon0 = lat, lon
	f.phi0, f.lam0 = lat*deg2rad, lon*deg2rad
	f.sin0, f.cos0 = math.Sincos(f.phi0)
	w := 1 - wgs84E2*f.sin0*f.sin0
	f.r = wgs84A / math.Sqrt(w)
	f.k = (1 - wgs84E2) / w
	return f
}

// toXY projects lat/lon (deg) into the plane.
func (f *frame) toXY(lat, lon float64) (x, y float64) {
	phi := f.phi0 + (lat*deg2rad-f.phi0)*f.k
	dl := lon*deg2rad - f.lam0
	if dl > math.Pi {
		dl -= 2 * math.Pi
	} else if dl < -math.Pi {
		dl += 2 * math.Pi
	}
	sp, cp := math.Sincos(phi)
	sdl, cdl := math.Sincos(dl)
	a := cp * sdl                  // east
	b := f.cos0*sp - f.sin0*cp*cdl // north
	c := f.sin0*sp + f.cos0*cp*cdl // cos of central angle
	h := math.Hypot(a, b)          // sin of central angle
	if h < 1e-15 {
		return 0, 0
	}
	ang := math.Atan2(h, c)
	s := f.r * ang / h
	return s * a, s * b
}

// toLatLon is the exact inverse of toXY.
func (f *frame) toLatLon(x, y float64) (lat, lon float64) {
	rho := math.Hypot(x, y)
	if rho < 1e-9 {
		return f.lat0, f.lon0
	}
	c := rho / f.r
	sc, cc := math.Sincos(c)
	sp := cc*f.sin0 + y*sc*f.cos0/rho
	if sp > 1 {
		sp = 1
	} else if sp < -1 {
		sp = -1
	}
	phi := math.Asin(sp)
	lam := f.lam0 + math.Atan2(x*sc, rho*f.cos0*cc-y*f.sin0*sc)
	lat = (f.phi0 + (phi-f.phi0)/f.k) * rad2deg
	if lat > 90 {
		lat = 90
	} else if lat < -90 {
		lat = -90
	}
	lon = normLon(lam * rad2deg)
	return lat, lon
}

// gridConvergence returns gamma (rad): the angle, clockwise, from the grid
// north of this frame to true north at (lat, lon). A grid vector (gx, gy) has
// true components (cos g*gx - sin g*gy, sin g*gx + cos g*gy).
func (f *frame) gridConvergence(lat, lon float64) float64 {
	x0, y0 := f.toXY(lat, lon)
	d := 0.001
	if lat > 89.9 {
		d = -d
	}
	x1, y1 := f.toXY(lat+d, lon)
	if d < 0 {
		x1, y1 = 2*x0-x1, 2*y0-y1
	}
	return math.Atan2(x1-x0, y1-y0)
}

func normLon(l float64) float64 {
	if l >= -180 && l < 180 {
		return l
	}
	l = math.Mod(l+180, 360)
	if l < 0 {
		l += 360
	}
	return l - 180
}

func normDeg(d float64) float64 {
	if d >= 0 && d < 360 {
		return d
	}
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	if d >= 360 {
		d = 0
	}
	return d
}

// angDiff returns a-b wrapped to (-180,180].
func angDiff(a, b float64) float64 {
	d := math.Mod(a-b, 360)
	if d > 180 {
		d -= 360
	} else if d <= -180 {
		d += 360
	}
	return d
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func fin(v float64) float64 {
	if finite(v) {
		return v
	}
	return 0
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// trackOf returns the true track (deg) of a grid velocity.
func trackOf(vx, vy float64) float64 {
	if vx == 0 && vy == 0 {
		return 0
	}
	return normDeg(math.Atan2(vx, vy) * rad2deg)
}

// haversineM is the spherical great-circle distance in metres (mean radius).
func haversineM(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := lat1*deg2rad, lat2*deg2rad
	dp := p2 - p1
	dl := (lon2 - lon1) * deg2rad
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * 6371008.8 * math.Asin(math.Sqrt(clamp(a, 0, 1)))
}

func bearingDeg(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := lat1*deg2rad, lat2*deg2rad
	dl := (lon2 - lon1) * deg2rad
	y := math.Sin(dl) * math.Cos(p2)
	x := math.Cos(p1)*math.Sin(p2) - math.Sin(p1)*math.Cos(p2)*math.Cos(dl)
	return normDeg(math.Atan2(y, x) * rad2deg)
}
