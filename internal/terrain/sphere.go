package terrain

import (
	"math"

	"airview/internal/geo"
)

// Spherical helpers on the mean earth radius (geo.EarthRadiusKm). The polar
// sweeps, the per-aircraft lookups and the synthetic sources all use the
// same sphere so that their distances agree exactly; the difference to the
// WGS84 ellipsoid (< 0.5 %) is far below the terrain data resolution.

const (
	degToRad = math.Pi / 180
	radToDeg = 180 / math.Pi
)

// origin is a fixed point with precomputed trigonometry.
type origin struct {
	lat, lon       float64 // degrees
	sinLat, cosLat float64
	lonRad         float64
}

func newOrigin(lat, lon float64) origin {
	s, c := math.Sincos(lat * degToRad)
	return origin{lat: lat, lon: lon, sinLat: s, cosLat: c, lonRad: lon * degToRad}
}

// dest returns the point at angular distance (sinD, cosD) along a great
// circle leaving the origin with initial bearing (sinB, cosB).
func (o *origin) dest(sinB, cosB, sinD, cosD float64) (lat, lon float64) {
	sinLat2 := o.sinLat*cosD + o.cosLat*sinD*cosB
	if sinLat2 > 1 {
		sinLat2 = 1
	} else if sinLat2 < -1 {
		sinLat2 = -1
	}
	lat2 := math.Asin(sinLat2)
	lon2 := o.lonRad + math.Atan2(sinB*sinD*o.cosLat, cosD-o.sinLat*sinLat2)
	lon = lon2 * radToDeg
	if lon >= 180 || lon < -180 {
		lon = geo.NormalizeLon(lon)
	}
	return lat2 * radToDeg, lon
}

// destKm returns the point distKm away on bearing brgDeg.
func (o *origin) destKm(brgDeg, distKm float64) (lat, lon float64) {
	sb, cb := math.Sincos(brgDeg * degToRad)
	sd, cd := math.Sincos(distKm / geo.EarthRadiusKm)
	return o.dest(sb, cb, sd, cd)
}

// inverse returns the great-circle distance (km) and initial bearing
// (degrees [0,360)) from the origin to (lat, lon).
func (o *origin) inverse(lat, lon float64) (km, brgDeg float64) {
	s2, c2 := math.Sincos(lat * degToRad)
	sdl, cdl := math.Sincos(lon*degToRad - o.lonRad)
	// Haversine-free form: atan2 of |cross| and dot is accurate at all distances.
	y := c2 * sdl
	x := o.cosLat*s2 - o.sinLat*c2*cdl
	dot := o.sinLat*s2 + o.cosLat*c2*cdl
	ang := math.Atan2(math.Hypot(x, y), dot)
	km = ang * geo.EarthRadiusKm
	if y == 0 && x == 0 {
		return km, 0
	}
	brgDeg = math.Atan2(y, x) * radToDeg
	if brgDeg < 0 {
		brgDeg += 360
	}
	if brgDeg >= 360 {
		brgDeg = 0
	}
	return km, brgDeg
}

// sphereKm is the great-circle distance between two points.
func sphereKm(lat1, lon1, lat2, lon2 float64) float64 {
	o := newOrigin(lat1, lon1)
	km, _ := o.inverse(lat2, lon2)
	return km
}

// interpolate returns the point at fraction f of the great circle from
// (lat1,lon1) to (lat2,lon2).
func interpolateGC(lat1, lon1, lat2, lon2, f float64) (lat, lon float64) {
	if f <= 0 {
		return lat1, lon1
	}
	if f >= 1 {
		return lat2, lon2
	}
	o := newOrigin(lat1, lon1)
	km, brg := o.inverse(lat2, lon2)
	return o.destKm(brg, km*f)
}

// finite replaces NaN and ±Inf with def (JSON cannot encode them).
func finite(v, def float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return def
	}
	return v
}

// round rounds v to the given number of decimals and makes it JSON-safe.
func round(v float64, decimals int) float64 {
	v = finite(v, 0)
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}
