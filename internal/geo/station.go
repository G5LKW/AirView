package geo

import "math"

// Receiver-to-aircraft geometry over an "effective earth".
//
// Radio rays in a standard atmosphere bend gently towards the ground. The
// classical way to keep straight-ray geometry is to replace the earth radius
// R by an effective radius Re = k·R (k = 4/3 for the standard atmosphere,
// ITU-R P.453 / P.834). All elevation and visibility functions here work on
// that model with a smooth spherical earth and heights above mean sea level:
//
//	θ      = d / Re                       central angle for ground distance d
//	elev   = atan2( (Re+h2)·cosθ − (Re+h1), (Re+h2)·sinθ )
//	slant² = (h2−h1)² + 4·(R+h1)·(R+h2)·sin²(d/2R)      (true earth radius)
//
// The slant range is a real length, so it is always computed with the true
// earth radius; only angles/visibility use k.

// EffectiveK sanitises an effective-earth-radius factor: NaN or 0 selects
// DefaultK (4/3); negative values (ray curvature exceeding the earth's, i.e.
// trapping/ducting) and very large values are treated as a flat earth.
func EffectiveK(k float64) float64 {
	switch {
	case k != k || k == 0:
		return DefaultK
	case k < 0 || k > maxK:
		return maxK
	default:
		return k
	}
}

// SlantRangeKm is the straight-line distance in km between an antenna at
// antennaM metres AMSL and an aircraft at altM metres AMSL separated by a
// great-circle ground distance groundKm.
func SlantRangeKm(groundKm, antennaM, altM float64) float64 {
	r1 := EarthRadiusKm + antennaM/1000
	r2 := EarthRadiusKm + altM/1000
	s := math.Sin(groundKm / (2 * EarthRadiusKm))
	dh := (altM - antennaM) / 1000
	return math.Sqrt(dh*dh + 4*r1*r2*s*s)
}

// ElevationDeg is the apparent elevation angle in degrees above the local
// horizontal at which an antenna at antennaM metres AMSL sees an aircraft at
// altM metres AMSL at ground distance groundKm, for effective-earth factor k
// (see EffectiveK; pass 0 for the standard 4/3). Negative values are below
// the horizontal (possible for elevated antennas and distant targets).
func ElevationDeg(groundKm, antennaM, altM, k float64) float64 {
	re := EffectiveK(k) * EarthRadiusKm
	r2 := re + altM/1000
	theta := groundKm / re
	sh := math.Sin(theta / 2)
	// (Re+h2)·cosθ − (Re+h1) written as Δh − 2·(Re+h2)·sin²(θ/2) to avoid
	// cancellation between two ~8500 km terms.
	num := (altM-antennaM)/1000 - 2*r2*sh*sh
	den := r2 * math.Sin(theta)
	if num == 0 && den == 0 {
		return 0
	}
	return math.Atan2(num, den) * radToDeg
}

// HorizonKm is the ground distance in km from a point hM metres above a
// smooth earth to its horizon for effective-earth factor k:
// d = Re·acos(Re/(Re+h)) ≈ sqrt(2·k·R·h). Non-positive heights give 0.
func HorizonKm(hM, k float64) float64 {
	if !(hM > 0) {
		return 0
	}
	re := EffectiveK(k) * EarthRadiusKm
	return re * math.Acos(re/(re+hM/1000))
}

// MinVisibleAltitudeM is the lowest altitude (metres AMSL) at which an
// aircraft at ground distance groundKm is above the smooth-earth horizon of
// an antenna at antennaM metres AMSL, for effective-earth factor k.
// Within the antenna's own horizon distance the result is 0.
func MinVisibleAltitudeM(groundKm, antennaM, k float64) float64 {
	re := EffectiveK(k) * EarthRadiusKm
	var theta1 float64
	if antennaM > 0 {
		theta1 = math.Acos(re / (re + antennaM/1000))
	}
	theta := groundKm/re - theta1
	if theta <= 0 {
		return 0
	}
	if theta >= math.Pi/2 {
		return math.Inf(1)
	}
	return (re/math.Cos(theta) - re) * 1000
}

// AltitudeAtElevationM returns the altitude (metres AMSL) of the point at
// ground distance groundKm along a ray leaving an antenna at antennaM metres
// AMSL at elevationDeg above the horizontal, for effective-earth factor k.
// ok is false when the ray has hit the smooth earth before that distance or
// never reaches it. It is the inverse of ElevationDeg and is what draws
// "coverage floor" curves for a given horizon mask angle.
func AltitudeAtElevationM(groundKm, antennaM, elevationDeg, k float64) (altM float64, ok bool) {
	re := EffectiveK(k) * EarthRadiusKm
	r1 := re + antennaM/1000
	eps := elevationDeg * degToRad
	theta := groundKm / re
	if eps+theta >= math.Pi/2 || eps <= -math.Pi/2 {
		return 0, false
	}
	// Law of sines in the triangle earth-centre / antenna / target:
	// r2 = r1·cos(ε) / cos(ε+θ).
	r2 := r1 * math.Cos(eps) / math.Cos(eps+theta)
	if eps < 0 {
		// The ray descends until θ = −ε, where it is closest to the earth's
		// centre (rMin). If that is below the surface the ray struck the
		// ground at θhit = −ε − acos(rMin/Re) and nothing beyond is visible.
		if rMin := r1 * math.Cos(eps); rMin < re && theta > -eps-math.Acos(rMin/re) {
			return 0, false
		}
	}
	if r2 < re {
		r2 = re // rounding at the grazing point
	}
	return (r2 - re) * 1000, true
}

// Look is the geometry of one aircraft as seen from a Station.
type Look struct {
	GroundKm     float64 // WGS84 ground (great-circle) distance
	BearingDeg   float64 // degrees true from the station to the aircraft
	SlantKm      float64 // straight-line distance antenna → aircraft
	ElevationDeg float64 // apparent elevation above the station's horizontal
}

// Station is a fixed observation point with everything that can be
// precomputed for fast per-position geometry. It is immutable after
// NewStation and safe for concurrent use; build a new one when the receiver
// position, antenna height or k-factor changes.
type Station struct {
	Lat, Lon     float64 // WGS84 degrees
	AntennaAMSLM float64 // antenna height above mean sea level, metres
	K            float64 // effective-earth factor actually used (sanitised)

	sinU, cosU float64 // reduced latitude
	lonRad     float64
}

// NewStation prepares a Station. antennaAMSLM is the antenna feed-point
// height above mean sea level (site ground elevation + mast height); k is the
// effective-earth factor (0 = standard 4/3).
func NewStation(lat, lon, antennaAMSLM, k float64) *Station {
	s := &Station{Lat: lat, Lon: lon, AntennaAMSLM: antennaAMSLM, K: EffectiveK(k)}
	s.sinU, s.cosU = reducedLat(lat)
	s.lonRad = lon * degToRad
	return s
}

// DistanceBearing returns the WGS84 ground distance in km (Andoyer-Lambert,
// metre-level accuracy at ADS-B ranges) and the bearing in degrees true from
// the station to (lat,lon). Cost: two Sincos, two Atan2, two Sqrt.
func (s *Station) DistanceBearing(lat, lon float64) (km, bearingDeg float64) {
	sinU2, cosU2 := reducedLat(lat)
	d, az := lambert(s.sinU, s.cosU, sinU2, cosU2, lon*degToRad-s.lonRad)
	az *= radToDeg
	if az < 0 {
		az += 360
	}
	if az >= 360 {
		az = 0
	}
	return d / 1000, az
}

// DistanceKm returns only the ground distance in km to (lat,lon).
func (s *Station) DistanceKm(lat, lon float64) float64 {
	km, _ := s.DistanceBearing(lat, lon)
	return km
}

// Look returns the full geometry of an aircraft at (lat,lon) and altM metres
// AMSL. When the altitude is unknown pass NaN: distance and bearing are
// still valid and SlantKm/ElevationDeg are NaN.
func (s *Station) Look(lat, lon, altM float64) Look {
	km, brg := s.DistanceBearing(lat, lon)
	l := Look{GroundKm: km, BearingDeg: brg, SlantKm: math.NaN(), ElevationDeg: math.NaN()}
	if altM == altM {
		l.SlantKm = SlantRangeKm(km, s.AntennaAMSLM, altM)
		l.ElevationDeg = ElevationDeg(km, s.AntennaAMSLM, altM, s.K)
	}
	return l
}

// LookFt is Look with the aircraft altitude in feet (the API unit).
func (s *Station) LookFt(lat, lon, altFt float64) Look {
	return s.Look(lat, lon, altFt*MetresPerFoot)
}

// Destination returns the WGS84 point at distKm on bearingDeg from the
// station (Vincenty direct), e.g. for range rings and sector outlines.
func (s *Station) Destination(bearingDeg, distKm float64) (lat, lon float64) {
	lat, lon, _ = VincentyDirect(s.Lat, s.Lon, bearingDeg, distKm*1000)
	return lat, lon
}

// MinVisibleAltitudeM is the smooth-earth coverage floor (metres AMSL) at
// ground distance groundKm from this station.
func (s *Station) MinVisibleAltitudeM(groundKm float64) float64 {
	return MinVisibleAltitudeM(groundKm, s.AntennaAMSLM, s.K)
}
