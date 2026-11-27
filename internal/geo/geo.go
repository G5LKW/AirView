// Package geo provides the geodesy used throughout AirView: great-circle and
// WGS84 geodesic maths, receiver-to-aircraft geometry (ground distance, slant
// range, elevation angle over an effective-earth-radius model), coordinate
// parsing/formatting (decimal, DMS, degrees + decimal minutes, Maidenhead,
// map URLs, Plus Codes) and the binning helpers that define the database
// grid conventions (heat cells, bearing bins, altitude bands, ...).
//
// Conventions
//   - latitude/longitude: WGS84 degrees, north and east positive
//   - bearings/azimuths: degrees true, clockwise from north, [0,360)
//   - distances: kilometres unless the name says otherwise (…M = metres)
//   - heights/altitudes in this package: metres (use FeetToMetres for the
//     feet used by the HTTP API)
//
// Everything here is a pure function or an immutable value and is safe for
// concurrent use. Nothing allocates on the per-position hot path.
package geo

import "math"

// Earth and unit constants.
const (
	// EarthRadiusKm is the IUGG mean earth radius R1 = (2a+b)/3.
	EarthRadiusKm = 6371.0088

	// WGS84 ellipsoid (NIMA TR8350.2).
	WGS84A = 6378137.0             // semi-major axis, metres
	WGS84F = 1.0 / 298.257223563   // flattening
	WGS84B = WGS84A * (1 - WGS84F) // semi-minor axis, metres (6356752.314245)

	// DefaultK is the standard-atmosphere effective-earth-radius factor (4/3).
	DefaultK = 4.0 / 3.0

	MetresPerFoot = 0.3048
	FeetPerMetre  = 1 / MetresPerFoot
	KmPerNM       = 1.852
	KmPerMile     = 1.609344

	degToRad = math.Pi / 180
	radToDeg = 180 / math.Pi

	// maxK is the largest effective-earth factor used for geometry; beyond it
	// the earth is treated as flat (ray curvature ~ earth curvature, i.e.
	// ducting conditions).
	maxK = 1000.0
)

// Rad converts degrees to radians.
func Rad(deg float64) float64 { return deg * degToRad }

// Deg converts radians to degrees.
func Deg(rad float64) float64 { return rad * radToDeg }

// FeetToMetres converts feet to metres.
func FeetToMetres(ft float64) float64 { return ft * MetresPerFoot }

// MetresToFeet converts metres to feet.
func MetresToFeet(m float64) float64 { return m * FeetPerMetre }

// KmToNM converts kilometres to nautical miles.
func KmToNM(km float64) float64 { return km / KmPerNM }

// NMToKm converts nautical miles to kilometres.
func NMToKm(nm float64) float64 { return nm * KmPerNM }

// NormalizeDeg wraps an angle into [0,360).
func NormalizeDeg(deg float64) float64 {
	if deg >= 0 && deg < 360 {
		return deg
	}
	deg = math.Mod(deg, 360)
	if deg < 0 {
		deg += 360
	}
	if deg >= 360 { // -1e-17 + 360 rounds to 360
		deg = 0
	}
	return deg
}

// NormalizeLon wraps a longitude into [-180,180).
func NormalizeLon(lon float64) float64 {
	if lon >= -180 && lon < 180 {
		return lon
	}
	lon = math.Mod(lon+180, 360)
	if lon < 0 {
		lon += 360
	}
	return lon - 180
}

// ClampLat limits a latitude to [-90,90].
func ClampLat(lat float64) float64 {
	if lat > 90 {
		return 90
	}
	if lat < -90 {
		return -90
	}
	return lat
}

// AngleDiff returns the signed smallest difference a-b in degrees, in
// (-180,180]. Positive means a is clockwise of b.
func AngleDiff(a, b float64) float64 {
	d := math.Mod(a-b, 360)
	if d > 180 {
		d -= 360
	} else if d <= -180 {
		d += 360
	}
	return d
}

// ValidLatLon reports whether lat/lon are finite and within range
// (lat in [-90,90], lon in [-180,180]).
func ValidLatLon(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 // false for NaN
}

// HaversineKm is the great-circle distance in km on the mean-radius sphere.
// Error versus the WGS84 geodesic is up to ~0.5 % (use DistanceKm or
// VincentyInverse when that matters).
func HaversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := lat1*degToRad, lat2*degToRad
	sdp := math.Sin((p2 - p1) / 2)
	sdl := math.Sin((lon2 - lon1) * degToRad / 2)
	h := sdp*sdp + math.Cos(p1)*math.Cos(p2)*sdl*sdl
	if h > 1 {
		h = 1
	}
	return 2 * EarthRadiusKm * math.Asin(math.Sqrt(h))
}

// FastDistanceKm is an equirectangular approximation of the distance in km.
// It needs a single cosine and is good to ~0.5 % below ~300 km at mid
// latitudes; use it only for coarse filtering/sorting.
func FastDistanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	dLon := lon2 - lon1
	if dLon > 180 {
		dLon -= 360
	} else if dLon < -180 {
		dLon += 360
	}
	x := dLon * degToRad * math.Cos((lat1+lat2)*0.5*degToRad)
	y := (lat2 - lat1) * degToRad
	return EarthRadiusKm * math.Sqrt(x*x+y*y)
}

// InitialBearing is the initial great-circle bearing (forward azimuth) from
// point 1 to point 2, degrees true in [0,360). Coincident points give 0.
func InitialBearing(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := lat1*degToRad, lat2*degToRad
	sinP1, cosP1 := math.Sincos(p1)
	sinP2, cosP2 := math.Sincos(p2)
	sinDL, cosDL := math.Sincos((lon2 - lon1) * degToRad)
	y := sinDL * cosP2
	x := cosP1*sinP2 - sinP1*cosP2*cosDL
	if x == 0 && y == 0 {
		return 0
	}
	return NormalizeDeg(math.Atan2(y, x) * radToDeg)
}

// FinalBearing is the bearing at which a great-circle path from point 1
// arrives at point 2 (the direction of travel on arrival), degrees true.
func FinalBearing(lat1, lon1, lat2, lon2 float64) float64 {
	return NormalizeDeg(InitialBearing(lat2, lon2, lat1, lon1) + 180)
}

// Destination returns the point reached by travelling distKm along the great
// circle that leaves (lat,lon) on bearingDeg (spherical earth).
func Destination(lat, lon, bearingDeg, distKm float64) (lat2, lon2 float64) {
	return destinationAngular(lat, lon, bearingDeg, distKm/EarthRadiusKm)
}

// destinationAngular is Destination with the distance as a central angle (rad).
func destinationAngular(lat, lon, bearingDeg, delta float64) (lat2, lon2 float64) {
	sinP1, cosP1 := math.Sincos(lat * degToRad)
	sinD, cosD := math.Sincos(delta)
	sinB, cosB := math.Sincos(bearingDeg * degToRad)
	sinP2 := sinP1*cosD + cosP1*sinD*cosB
	if sinP2 > 1 {
		sinP2 = 1
	} else if sinP2 < -1 {
		sinP2 = -1
	}
	p2 := math.Asin(sinP2)
	l2 := lon*degToRad + math.Atan2(sinB*sinD*cosP1, cosD-sinP1*sinP2)
	return p2 * radToDeg, NormalizeLon(l2 * radToDeg)
}

// Midpoint returns the great-circle midpoint of two points.
func Midpoint(lat1, lon1, lat2, lon2 float64) (lat, lon float64) {
	return Intermediate(lat1, lon1, lat2, lon2, 0.5)
}

// Intermediate returns the point at fraction f (0 = point 1, 1 = point 2)
// along the great circle between two points. Antipodal points are undefined
// and return point 1.
func Intermediate(lat1, lon1, lat2, lon2, f float64) (lat, lon float64) {
	p1, l1 := lat1*degToRad, lon1*degToRad
	p2, l2 := lat2*degToRad, lon2*degToRad
	sinP1, cosP1 := math.Sincos(p1)
	sinP2, cosP2 := math.Sincos(p2)
	sdp := math.Sin((p2 - p1) / 2)
	sdl := math.Sin((l2 - l1) / 2)
	h := sdp*sdp + cosP1*cosP2*sdl*sdl
	if h > 1 {
		h = 1
	}
	delta := 2 * math.Asin(math.Sqrt(h))
	sinDelta := math.Sin(delta)
	if sinDelta < 1e-15 {
		return lat1, lon1
	}
	a := math.Sin((1-f)*delta) / sinDelta
	b := math.Sin(f*delta) / sinDelta
	x := a*cosP1*math.Cos(l1) + b*cosP2*math.Cos(l2)
	y := a*cosP1*math.Sin(l1) + b*cosP2*math.Sin(l2)
	z := a*sinP1 + b*sinP2
	return math.Atan2(z, math.Hypot(x, y)) * radToDeg, NormalizeLon(math.Atan2(y, x) * radToDeg)
}

// CrossTrackKm is the signed distance in km of point P from the great-circle
// path that starts at point 1 and passes through point 2. Positive values are
// to the right of the direction of travel, negative to the left.
func CrossTrackKm(latP, lonP, lat1, lon1, lat2, lon2 float64) float64 {
	d13 := HaversineKm(lat1, lon1, latP, lonP) / EarthRadiusKm
	t13 := InitialBearing(lat1, lon1, latP, lonP) * degToRad
	t12 := InitialBearing(lat1, lon1, lat2, lon2) * degToRad
	return math.Asin(math.Sin(d13)*math.Sin(t13-t12)) * EarthRadiusKm
}

// AlongTrackKm is the distance in km from point 1 to the point on the great
// circle through points 1 and 2 that is closest to P (negative when that
// point lies behind point 1).
func AlongTrackKm(latP, lonP, lat1, lon1, lat2, lon2 float64) float64 {
	d13 := HaversineKm(lat1, lon1, latP, lonP) / EarthRadiusKm
	t13 := InitialBearing(lat1, lon1, latP, lonP) * degToRad
	t12 := InitialBearing(lat1, lon1, lat2, lon2) * degToRad
	dxt := math.Asin(math.Sin(d13) * math.Sin(t13-t12))
	cosXT := math.Cos(dxt)
	if cosXT == 0 {
		return 0
	}
	c := math.Cos(d13) / cosXT
	if c > 1 {
		c = 1
	} else if c < -1 {
		c = -1
	}
	dat := math.Acos(c)
	if math.Cos(t13-t12) < 0 {
		dat = -dat
	}
	return dat * EarthRadiusKm
}

// reducedLat returns sin and cos of the reduced (parametric) latitude
// U = atan((1-f)·tan(lat)) without calling atan.
func reducedLat(latDeg float64) (sinU, cosU float64) {
	sinP, cosP := math.Sincos(latDeg * degToRad)
	if cosP < 1e-300 && cosP > -1e-300 { // pole
		if sinP < 0 {
			return -1, 0
		}
		return 1, 0
	}
	tanU := (1 - WGS84F) * sinP / cosP
	cosU = 1 / math.Sqrt(1+tanU*tanU)
	return tanU * cosU, cosU
}

// lambert computes the WGS84 distance (metres) and the initial azimuth (rad,
// unnormalised) between two points given the sin/cos of their reduced
// latitudes and the longitude difference in radians, using the
// Andoyer-Lambert first-order flattening correction of the auxiliary-sphere
// arc. Accuracy is ~f²·d: a few metres at 500 km, and a few thousandths of
// a degree in azimuth (tested against Vincenty).
func lambert(sinU1, cosU1, sinU2, cosU2, dLonRad float64) (distM, az float64) {
	sinL, cosL := math.Sincos(dLonRad)
	y := cosU2 * sinL
	x := cosU1*sinU2 - sinU1*cosU2*cosL
	sinS := math.Sqrt(y*y + x*x)
	cosS := sinU1*sinU2 + cosU1*cosU2*cosL
	if sinS < 1e-13 && cosS > 0 {
		return 0, 0
	}
	sigma := math.Atan2(sinS, cosS)
	// Azimuth: one Vincenty refinement of the auxiliary-sphere longitude
	// difference, λ = L + f·sinα·σ (the C terms are O(f²)). Without it the
	// azimuth is off by up to ~0.05° at any range.
	sinAlpha := cosU1 * cosU2 * sinL / sinS
	sinL2, cosL2 := math.Sincos(dLonRad + WGS84F*sinAlpha*sigma)
	az = math.Atan2(cosU2*sinL2, cosU1*sinU2-sinU1*cosU2*cosL2)
	// X = (σ - sinσ)·sin²P·cos²Q / cos²(σ/2),  Y = (σ + sinσ)·cos²P·sin²Q / sin²(σ/2)
	// with P = (U1+U2)/2, Q = (U2-U1)/2, so that
	// sinP·cosQ = (sinU1+sinU2)/2 and cosP·sinQ = (sinU2-sinU1)/2.
	sp := sinU1 + sinU2
	sm := sinU2 - sinU1
	var xx, yy float64
	if d := 2 * (1 + cosS); d > 1e-12 {
		xx = (sigma - sinS) * sp * sp / d
	}
	if d := 2 * (1 - cosS); d > 1e-18 {
		yy = (sigma + sinS) * sm * sm / d
	}
	return WGS84A * (sigma - WGS84F/2*(xx+yy)), az
}

// DistanceKm is the WGS84 geodesic distance in km using the Andoyer-Lambert
// approximation: a single-pass formula accurate to a few metres at ADS-B
// ranges (versus up to ~0.5 % error for HaversineKm). This is the
// recommended general-purpose distance.
func DistanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	sinU1, cosU1 := reducedLat(lat1)
	sinU2, cosU2 := reducedLat(lat2)
	d, _ := lambert(sinU1, cosU1, sinU2, cosU2, (lon2-lon1)*degToRad)
	return d / 1000
}

// DistanceBearing returns the WGS84 distance in km (Andoyer-Lambert) and the
// initial bearing in degrees true from point 1 to point 2 in one pass.
// For repeated queries from a fixed point use a Station instead.
func DistanceBearing(lat1, lon1, lat2, lon2 float64) (km, bearingDeg float64) {
	sinU1, cosU1 := reducedLat(lat1)
	sinU2, cosU2 := reducedLat(lat2)
	d, az := lambert(sinU1, cosU1, sinU2, cosU2, (lon2-lon1)*degToRad)
	return d / 1000, NormalizeDeg(az * radToDeg)
}
