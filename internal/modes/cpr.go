package modes

import "math"

// Compact Position Reporting. All functions take the raw 17-bit latitude and
// longitude values of position messages (Message.CPRLat, Message.CPRLon) and
// return WGS84 degrees with longitude in [-180, 180).

const cprScale = 131072.0 // 2^17

// nlLimit[n] is the latitude (degrees) at and above which the number of
// longitude zones drops below n, for n = 2..59.
var nlLimit = buildNLLimits()

func buildNLLimits() (t [60]float64) {
	const nz = 15
	a := 1 - math.Cos(math.Pi/(2*nz))
	for n := 2; n <= 59; n++ {
		t[n] = math.Acos(math.Sqrt(a/(1-math.Cos(2*math.Pi/float64(n))))) * (180 / math.Pi)
	}
	t[2] = 87 // by definition
	return t
}

// NL returns the number of longitude zones (1..59) at the given latitude:
// 59 at the equator, 2 up to and including 87 degrees, 1 beyond.
func NL(lat float64) int {
	a := math.Abs(lat)
	if !(a <= nlLimit[2]) { // also catches NaN
		return 1
	}
	// nlLimit decreases with n: find the largest n with a < nlLimit[n].
	lo, hi := 2, 59
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if a < nlLimit[mid] {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func modInt(a, b int) int {
	r := a % b
	if r < 0 {
		r += b
	}
	return r
}

// normLon wraps a longitude into [-180, 180).
func normLon(lon float64) float64 {
	return lon - math.Floor((lon+180)/360)*360
}

// lonDiff returns the absolute angular difference of two longitudes.
func lonDiff(a, b float64) float64 {
	return math.Abs(normLon(a - b))
}

// cprMax is the largest raw CPR value (17 bits).
const cprMax = 1<<17 - 1

// validRef reports whether a reference position is usable: a real latitude
// and a finite longitude (any multiple of 360 degrees is accepted).
func validRef(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && !math.IsNaN(lon) && !math.IsInf(lon, 0)
}

// CPRAirborneGlobal resolves an airborne even/odd position pair without a
// reference (globally unambiguous decoding).
//
// The two frames must come from the same aircraft and be recent enough that
// it cannot have moved more than 3 NM between them (10 s is the customary
// limit). latestOdd selects which frame's position is returned: pass true
// when the odd frame is the newer one.
//
// ok is false when the pair is inconsistent: the two latitudes fall into
// different longitude zone counts (the aircraft crossed a zone boundary
// between the frames, or the frames do not belong together) or the latitude
// is out of range. Retry with the next pair.
func CPRAirborneGlobal(evenLat, evenLon, oddLat, oddLon uint32, latestOdd bool) (lat, lon float64, ok bool) {
	const (
		dLat0 = 360.0 / 60
		dLat1 = 360.0 / 59
	)
	if evenLat > cprMax || evenLon > cprMax || oddLat > cprMax || oddLon > cprMax {
		return 0, 0, false
	}
	lat0, lat1 := float64(evenLat), float64(oddLat)
	lon0, lon1 := float64(evenLon), float64(oddLon)

	j := int(math.Floor((59*lat0-60*lat1)/cprScale + 0.5))
	rlat0 := dLat0 * (float64(modInt(j, 60)) + lat0/cprScale)
	rlat1 := dLat1 * (float64(modInt(j, 59)) + lat1/cprScale)
	if rlat0 >= 270 {
		rlat0 -= 360
	}
	if rlat1 >= 270 {
		rlat1 -= 360
	}
	if rlat0 < -90 || rlat0 > 90 || rlat1 < -90 || rlat1 > 90 {
		return 0, 0, false
	}
	nl := NL(rlat0)
	if nl != NL(rlat1) {
		return 0, 0, false
	}
	lat, lon, _ = cprGlobalLon(rlat0, rlat1, lon0, lon1, nl, latestOdd, 360)
	return lat, normLon(lon), true
}

// cprGlobalLon finishes a global decode once both latitudes are known.
// The longitude is returned in [0, span).
func cprGlobalLon(rlat0, rlat1, lon0, lon1 float64, nl int, latestOdd bool, span float64) (lat, lon float64, ok bool) {
	m := int(math.Floor((lon0*float64(nl-1)-lon1*float64(nl))/cprScale + 0.5))
	if latestOdd {
		ni := nl - 1
		if ni < 1 {
			ni = 1
		}
		lat = rlat1
		lon = span / float64(ni) * (float64(modInt(m, ni)) + lon1/cprScale)
	} else {
		ni := nl
		lat = rlat0
		lon = span / float64(ni) * (float64(modInt(m, ni)) + lon0/cprScale)
	}
	return lat, lon, true
}

// CPRSurfaceGlobal resolves a surface even/odd position pair. Surface
// positions repeat every 90 degrees in latitude and longitude, so a reference
// position (normally the receiver) is needed to choose among the solutions:
// the one closest to the reference is returned. The reference must be within
// 45 degrees of the target, which is always the case for off-air reception.
//
// The pair must be recent (the aircraft may not have moved more than 0.75 NM
// between the frames; 25 s at taxi speed, 50 s is the absolute limit).
// latestOdd and ok are as for CPRAirborneGlobal; ok is also false when the
// reference is not a real position.
func CPRSurfaceGlobal(refLat, refLon float64, evenLat, evenLon, oddLat, oddLon uint32, latestOdd bool) (lat, lon float64, ok bool) {
	const (
		dLat0 = 90.0 / 60
		dLat1 = 90.0 / 59
	)
	if !validRef(refLat, refLon) || evenLat > cprMax || evenLon > cprMax || oddLat > cprMax || oddLon > cprMax {
		return 0, 0, false
	}
	lat0, lat1 := float64(evenLat), float64(oddLat)
	lon0, lon1 := float64(evenLon), float64(oddLon)

	j := int(math.Floor((59*lat0-60*lat1)/cprScale + 0.5))
	rlat0 := dLat0 * (float64(modInt(j, 60)) + lat0/cprScale)
	rlat1 := dLat1 * (float64(modInt(j, 59)) + lat1/cprScale)

	// Both values are in [0, 90): the true latitude is either that or 90
	// degrees further south (and -90, 0 and +90 all encode as 0). Decide for
	// the frame that is returned, then put the other latitude next to it so
	// both stay in the same hemisphere.
	if latestOdd {
		rlat1 = nearestSurfaceLat(rlat1, refLat)
		rlat0 = nearestSurfaceLat(rlat0, rlat1)
	} else {
		rlat0 = nearestSurfaceLat(rlat0, refLat)
		rlat1 = nearestSurfaceLat(rlat1, rlat0)
	}
	nl := NL(rlat0)
	if nl != NL(rlat1) {
		return 0, 0, false
	}
	lat, lon, _ = cprGlobalLon(rlat0, rlat1, lon0, lon1, nl, latestOdd, 90)

	// lon is in [0, 90); the candidates are 90 degrees apart. Take the one
	// nearest to the reference longitude.
	lon += 90 * math.Floor((normLon(refLon-lon)+45)/90)
	return lat, normLon(lon), true
}

// nearestSurfaceLat picks, among the latitudes a surface CPR value in
// [0, 90) can stand for (rlat, rlat-90 and, for 0 only, +90), the one nearest
// to near. On a tie the northern solution wins.
func nearestSurfaceLat(rlat, near float64) float64 {
	best := rlat
	if s := rlat - 90; math.Abs(s-near) < math.Abs(best-near) {
		best = s
	}
	if rlat == 0 && math.Abs(90-near) < math.Abs(best-near) {
		best = 90
	}
	return best
}

// CPRAirborneLocal resolves a single airborne position relative to a
// reference position (locally unambiguous decoding). The reference must be
// within 180 NM of the true position: use the aircraft's last known position,
// or the receiver position when its maximum range is below 180 NM.
//
// ok is false when the result would be more than half a zone away from the
// reference, which means the reference is unusable for this message.
func CPRAirborneLocal(refLat, refLon float64, cprLat, cprLon uint32, odd bool) (lat, lon float64, ok bool) {
	return cprLocal(refLat, refLon, cprLat, cprLon, odd, 360)
}

// CPRSurfaceLocal resolves a single surface position relative to a reference
// position. The reference must be within 45 NM of the true position: use the
// aircraft's last known position or the receiver position.
//
// ok is false when the result would be more than half a zone away from the
// reference.
func CPRSurfaceLocal(refLat, refLon float64, cprLat, cprLon uint32, odd bool) (lat, lon float64, ok bool) {
	return cprLocal(refLat, refLon, cprLat, cprLon, odd, 90)
}

func cprLocal(refLat, refLon float64, cprLat, cprLon uint32, odd bool, span float64) (lat, lon float64, ok bool) {
	if !validRef(refLat, refLon) || cprLat > cprMax || cprLon > cprMax {
		return 0, 0, false
	}
	nz := 60.0
	i := 0
	if odd {
		nz = 59
		i = 1
	}
	fLat := float64(cprLat) / cprScale
	fLon := float64(cprLon) / cprScale

	dLat := span / nz
	lat = dLat * (zoneIndex(refLat, dLat, fLat) + fLat)
	if lat < -90 || lat > 90 || math.Abs(lat-refLat) > dLat/2 {
		return 0, 0, false
	}

	ni := NL(lat) - i
	if ni < 1 {
		ni = 1
	}
	dLon := span / float64(ni)
	lon = normLon(dLon * (zoneIndex(refLon, dLon, fLon) + fLon))
	if lonDiff(lon, refLon) > dLon/2 {
		return 0, 0, false
	}
	return lat, lon, true
}

// zoneIndex returns the index of the zone of width d in which a position
// with zone fraction f lies nearest to ref:
//
//	floor(ref/d) + floor(1/2 + mod(ref, d)/d - f)
//
// The zone number and the fraction of the reference are taken from the same
// quotient. Computing them separately (floor(ref/d) and fmod(ref, d)) can
// disagree by a whole zone when ref sits within rounding error of a zone
// boundary, which the last decoded position of an aircraft can do.
func zoneIndex(ref, d, f float64) float64 {
	q := ref / d
	z := math.Floor(q)
	return z + math.Floor(0.5+(q-z)-f)
}

// CPREncode produces the 17-bit CPR latitude and longitude values of a
// position, for the even (odd=false) or odd format and the airborne or
// surface encoding. It is the inverse of the decoders and intended for
// simulators and tests.
func CPREncode(lat, lon float64, odd, surface bool) (cprLat, cprLon uint32) {
	nz := 60.0
	i := 0
	if odd {
		nz = 59
		i = 1
	}
	scale := cprScale
	if surface {
		scale = 524288 // 2^19; the 17 low bits are transmitted
	}
	if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
		return 0, 0
	}
	dLat := 360 / nz
	// Zone number and fraction come from one quotient (see zoneIndex).
	q := lat / dLat
	z := math.Floor(q)
	yz := math.Floor(scale*(q-z) + 0.5)
	rlat := dLat * (yz/scale + z)
	ni := NL(rlat) - i
	if ni < 1 {
		ni = 1
	}
	q = lon / (360 / float64(ni))
	xz := math.Floor(scale*(q-math.Floor(q)) + 0.5)
	return uint32(int64(yz) & cprMax), uint32(int64(xz) & cprMax)
}
