// Package astro computes solar geometry for AirView: the sun's position,
// sunrise/sunset/twilight times, day/night classification of an instant, the
// night-side (terminator) polygon for the map, and the moon's phase.
//
// The solar algorithm is the one behind the NOAA Solar Calculator
// (https://gml.noaa.gov/grad/solcalc/calcdetails.html), which implements the
// low-accuracy formulae of Jean Meeus, "Astronomical Algorithms" (2nd ed.,
// chapters 25 and 28). NOAA quotes sunrise/sunset "within a minute for
// locations between +/- 72° latitude"; the position is good to ~0.01°.
// UTC is used in place of Terrestrial Time (ΔT ≈ 69 s is far below that).
//
// Conventions: latitude north-positive, longitude east-positive, azimuth in
// degrees true clockwise from north, angles in degrees. Everything is a pure
// function and safe for concurrent use.
package astro

import (
	"math"
	"time"
)

const (
	degToRad = math.Pi / 180
	radToDeg = 180 / math.Pi

	// SunriseAltitudeDeg is the geometric altitude of the sun's centre at
	// sunrise/sunset: 16' semi-diameter + 34' standard refraction (NOAA uses
	// a zenith of 90.833°).
	SunriseAltitudeDeg = -0.833
	// Twilight limits: geometric altitude of the sun's centre.
	CivilAltitudeDeg        = -6.0
	NauticalAltitudeDeg     = -12.0
	AstronomicalAltitudeDeg = -18.0
)

// JulianDay returns the Julian Day number (with fraction) of an instant.
func JulianDay(t time.Time) float64 {
	return float64(t.Unix())/86400 + float64(t.Nanosecond())/86400e9 + 2440587.5
}

func sinD(d float64) float64 { return math.Sin(d * degToRad) }
func cosD(d float64) float64 { return math.Cos(d * degToRad) }
func tanD(d float64) float64 { return math.Tan(d * degToRad) }

func norm360(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}

// norm180 wraps an angle into [-180,180).
func norm180(d float64) float64 {
	d = math.Mod(d+180, 360)
	if d < 0 {
		d += 360
	}
	return d - 180
}

// solar holds the sun's geocentric apparent coordinates for an instant.
type solar struct {
	decl   float64 // apparent declination, degrees
	ra     float64 // apparent right ascension, degrees [0,360)
	eqTime float64 // equation of time, minutes (apparent − mean solar time)
	dist   float64 // earth–sun distance, AU
}

// solarCoords implements Meeus ch. 25 (low accuracy) + ch. 28 exactly as the
// NOAA spreadsheet does.
func solarCoords(jd float64) solar {
	T := (jd - 2451545.0) / 36525.0 // Julian centuries from J2000.0

	L0 := norm360(280.46646 + T*(36000.76983+T*0.0003032)) // geometric mean longitude
	M := 357.52911 + T*(35999.05029-0.0001537*T)           // mean anomaly
	e := 0.016708634 - T*(0.000042037+0.0000001267*T)      // orbital eccentricity
	// Equation of the centre.
	C := sinD(M)*(1.914602-T*(0.004817+0.000014*T)) +
		sinD(2*M)*(0.019993-0.000101*T) +
		sinD(3*M)*0.000289
	trueLong := L0 + C
	trueAnom := M + C
	R := 1.000001018 * (1 - e*e) / (1 + e*cosD(trueAnom))
	// Apparent longitude: nutation + aberration.
	omega := 125.04 - 1934.136*T
	lambda := trueLong - 0.00569 - 0.00478*sinD(omega)
	// Obliquity of the ecliptic, corrected.
	eps0 := 23 + (26+(21.448-T*(46.815+T*(0.00059-T*0.001813)))/60)/60
	eps := eps0 + 0.00256*cosD(omega)

	decl := math.Asin(sinD(eps)*sinD(lambda)) * radToDeg
	ra := norm360(math.Atan2(cosD(eps)*sinD(lambda), cosD(lambda)) * radToDeg)

	y := tanD(eps / 2)
	y *= y
	eq := 4 * radToDeg * (y*sinD(2*L0) - 2*e*sinD(M) + 4*e*y*sinD(M)*cosD(2*L0) -
		0.5*y*y*sinD(4*L0) - 1.25*e*e*sinD(2*M))
	return solar{decl: decl, ra: ra, eqTime: eq, dist: R}
}

// utcMinutes returns minutes since 00:00 UTC of the instant's UTC day.
func utcMinutes(t time.Time) float64 {
	u := t.UTC()
	return float64(u.Hour())*60 + float64(u.Minute()) + float64(u.Second())/60 + float64(u.Nanosecond())/60e9
}

// hourAngle returns the sun's local hour angle in degrees [-180,180):
// negative before solar noon, positive after.
func hourAngle(t time.Time, lon float64, s solar) float64 {
	tst := utcMinutes(t) + s.eqTime + 4*lon // true solar time, minutes
	return norm180(tst/4 - 180)
}

// Refraction returns the atmospheric refraction in degrees to add to a
// geometric (true) elevation to obtain the apparent elevation, using the
// NOAA piecewise approximation (standard atmosphere, sea level).
func Refraction(trueElevationDeg float64) float64 {
	h := trueElevationDeg
	switch {
	case h > 85:
		return 0
	case h > 5:
		te := tanD(h)
		return (58.1/te - 0.07/(te*te*te) + 0.000086/(te*te*te*te*te)) / 3600
	case h > -0.575:
		return (1735 + h*(-518.2+h*(103.4+h*(-12.79+h*0.711)))) / 3600
	default:
		return (-20.774 / tanD(h)) / 3600
	}
}

// SunPosition is the sun's position for an observer.
type SunPosition struct {
	AzimuthDeg       float64 `json:"azimuth"`       // degrees true, clockwise from north
	ElevationDeg     float64 `json:"elevation"`     // apparent elevation (refraction corrected)
	TrueElevationDeg float64 `json:"trueElevation"` // geometric elevation of the sun's centre
	DeclinationDeg   float64 `json:"declination"`   // apparent declination
	HourAngleDeg     float64 `json:"hourAngle"`     // local hour angle, negative before solar noon
	EqOfTimeMin      float64 `json:"eqOfTimeMin"`   // equation of time, minutes
	DistanceAU       float64 `json:"distanceAu"`    // earth–sun distance
}

// SunAt returns the sun's position as seen from (lat,lon) at time t.
func SunAt(t time.Time, lat, lon float64) SunPosition {
	s := solarCoords(JulianDay(t))
	ha := hourAngle(t, lon, s)
	sinLat, cosLat := math.Sincos(lat * degToRad)
	sinDec, cosDec := math.Sincos(s.decl * degToRad)
	sinHA, cosHA := math.Sincos(ha * degToRad)

	sinEl := sinLat*sinDec + cosLat*cosDec*cosHA
	if sinEl > 1 {
		sinEl = 1
	} else if sinEl < -1 {
		sinEl = -1
	}
	el := math.Asin(sinEl) * radToDeg
	// Azimuth from north, clockwise (Meeus 13.5 gives it from south; +180).
	az := norm360(math.Atan2(sinHA, cosHA*sinLat-(sinDec/cosDec)*cosLat)*radToDeg + 180)

	return SunPosition{
		AzimuthDeg:       az,
		ElevationDeg:     el + Refraction(el),
		TrueElevationDeg: el,
		DeclinationDeg:   s.decl,
		HourAngleDeg:     ha,
		EqOfTimeMin:      s.eqTime,
		DistanceAU:       s.dist,
	}
}

// SubsolarPoint returns the point on earth where the sun is at the zenith.
func SubsolarPoint(t time.Time) (lat, lon float64) {
	s := solarCoords(JulianDay(t))
	// True solar time is 12:00 where UTCmin + eqTime + 4·lon = 720.
	return s.decl, norm180((720 - utcMinutes(t) - s.eqTime) / 4)
}

// Phase classifies how dark it is.
type Phase int

const (
	Day                  Phase = iota // sun's upper limb above the horizon
	CivilTwilight                     // sun 0.833°–6° below
	NauticalTwilight                  // 6°–12° below
	AstronomicalTwilight              // 12°–18° below
	Night                             // more than 18° below
)

// String returns "day", "civil", "nautical", "astronomical" or "night".
func (p Phase) String() string {
	switch p {
	case Day:
		return "day"
	case CivilTwilight:
		return "civil"
	case NauticalTwilight:
		return "nautical"
	case AstronomicalTwilight:
		return "astronomical"
	default:
		return "night"
	}
}

// PhaseOfElevation classifies a geometric (true) solar elevation in degrees.
func PhaseOfElevation(trueElevationDeg float64) Phase {
	switch {
	case trueElevationDeg > SunriseAltitudeDeg:
		return Day
	case trueElevationDeg >= CivilAltitudeDeg:
		return CivilTwilight
	case trueElevationDeg >= NauticalAltitudeDeg:
		return NauticalTwilight
	case trueElevationDeg >= AstronomicalAltitudeDeg:
		return AstronomicalTwilight
	default:
		return Night
	}
}

// PhaseAt classifies the instant t at (lat,lon).
func PhaseAt(t time.Time, lat, lon float64) Phase {
	return PhaseOfElevation(SunAt(t, lat, lon).TrueElevationDeg)
}

// IsDay reports whether the sun is up (between sunrise and sunset) at t.
func IsDay(t time.Time, lat, lon float64) bool {
	return SunAt(t, lat, lon).TrueElevationDeg > SunriseAltitudeDeg
}

// Events are the solar events of one solar day at a location. A zero
// time.Time means the event does not happen that day (polar day/night, or
// "white nights" when the sun never gets low enough for that twilight);
// NoonElevationDeg and MidnightElevationDeg tell which.
type Events struct {
	SolarNoon            time.Time // upper transit
	NoonElevationDeg     float64   // geometric elevation at solar noon (highest of the day)
	MidnightElevationDeg float64   // geometric elevation at solar midnight (lowest of the day)

	Sunrise, Sunset            time.Time // sun's upper limb on the horizon (-0.833°)
	CivilDawn, CivilDusk       time.Time // -6°
	NauticalDawn, NauticalDusk time.Time // -12°
	AstroDawn, AstroDusk       time.Time // -18°

	DayLength  time.Duration // sunrise → sunset; 24 h on a polar day, 0 in polar night
	PolarDay   bool          // the sun does not set
	PolarNight bool          // the sun does not rise
}

// EventsMs is Events with Unix-millisecond timestamps (0 = does not occur),
// ready for the JSON API.
type EventsMs struct {
	SolarNoon            int64   `json:"solarNoon"`
	NoonElevationDeg     float64 `json:"noonElevation"`
	MidnightElevationDeg float64 `json:"midnightElevation"`
	Sunrise              int64   `json:"sunrise"`
	Sunset               int64   `json:"sunset"`
	CivilDawn            int64   `json:"civilDawn"`
	CivilDusk            int64   `json:"civilDusk"`
	NauticalDawn         int64   `json:"nauticalDawn"`
	NauticalDusk         int64   `json:"nauticalDusk"`
	AstroDawn            int64   `json:"astroDawn"`
	AstroDusk            int64   `json:"astroDusk"`
	DayLengthSec         float64 `json:"dayLengthSec"`
	PolarDay             bool    `json:"polarDay"`
	PolarNight           bool    `json:"polarNight"`
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// Ms converts the events to Unix-millisecond form.
func (e Events) Ms() EventsMs {
	return EventsMs{
		SolarNoon: ms(e.SolarNoon), NoonElevationDeg: e.NoonElevationDeg, MidnightElevationDeg: e.MidnightElevationDeg,
		Sunrise: ms(e.Sunrise), Sunset: ms(e.Sunset),
		CivilDawn: ms(e.CivilDawn), CivilDusk: ms(e.CivilDusk),
		NauticalDawn: ms(e.NauticalDawn), NauticalDusk: ms(e.NauticalDusk),
		AstroDawn: ms(e.AstroDawn), AstroDusk: ms(e.AstroDusk),
		DayLengthSec: e.DayLength.Seconds(), PolarDay: e.PolarDay, PolarNight: e.PolarNight,
	}
}

// SolarNoon returns the instant of the sun's upper transit nearest to t at
// longitude lon (within ±12 h of t).
func SolarNoon(t time.Time, lon float64) time.Time {
	noon := t
	for i := 0; i < 3; i++ {
		s := solarCoords(JulianDay(noon))
		ha := hourAngle(noon, lon, s) // degrees, 0 at transit
		noon = noon.Add(time.Duration(-ha * 4 * float64(time.Minute)))
	}
	return noon.UTC()
}

// Altitude-crossing states.
const (
	crossNormal      = 0
	crossAlwaysAbove = 1
	crossAlwaysBelow = -1
)

// Crossings returns the instants at which the sun's centre passes the
// geometric altitude altDeg while rising and while setting, on the solar day
// whose noon is nearest to t. state is 0 when both happen, +1 when the sun
// stays above altDeg all day and -1 when it stays below (rise and set are
// then zero).
func Crossings(t time.Time, lat, lon, altDeg float64) (rise, set time.Time, state int) {
	return crossings(SolarNoon(t, lon), lat, lon, altDeg)
}

func crossings(noon time.Time, lat, lon, altDeg float64) (rise, set time.Time, state int) {
	sinLat, cosLat := math.Sincos(lat * degToRad)
	sinAlt := sinD(altDeg)

	// h0 returns the hour angle (degrees, >= 0) at which the sun is at altDeg
	// for declination decl.
	h0 := func(decl float64) (float64, int) {
		sinDec, cosDec := math.Sincos(decl * degToRad)
		den := cosLat * cosDec
		num := sinAlt - sinLat*sinDec
		if math.Abs(den) < 1e-12 { // at a pole the altitude is constant over the day
			if num < 0 {
				return 0, crossAlwaysAbove
			}
			return 0, crossAlwaysBelow
		}
		c := num / den
		if c > 1 {
			return 0, crossAlwaysBelow
		}
		if c < -1 {
			return 0, crossAlwaysAbove
		}
		return math.Acos(c) * radToDeg, crossNormal
	}

	solve := func(sign float64) (time.Time, int) {
		s := solarCoords(JulianDay(noon))
		h, st := h0(s.decl)
		if st != crossNormal {
			return time.Time{}, st
		}
		t := noon.Add(time.Duration(sign * h * 4 * float64(time.Minute)))
		// Refine with the declination and equation of time at the event
		// itself: move by the remaining hour-angle error at 15°/h.
		for i := 0; i < 4; i++ {
			s = solarCoords(JulianDay(t))
			h, st = h0(s.decl)
			if st != crossNormal {
				return time.Time{}, st
			}
			diff := norm180(sign*h - hourAngle(t, lon, s))
			t = t.Add(time.Duration(diff * 4 * float64(time.Minute)))
			if math.Abs(diff) < 1e-5 {
				break
			}
		}
		return t.UTC(), crossNormal
	}

	rise, st1 := solve(-1)
	set, st2 := solve(+1)
	if st1 != crossNormal || st2 != crossNormal {
		st := st1
		if st == crossNormal {
			st = st2
		}
		return time.Time{}, time.Time{}, st
	}
	return rise, set, crossNormal
}

// DayEvents returns sunrise, sunset, the three twilights and solar noon for
// the solar day that contains t at (lat,lon): the one whose solar noon is
// nearest to t (within ±12 h). This is independent of time zones; to get the
// events of a particular calendar date pass local noon of that date.
func DayEvents(t time.Time, lat, lon float64) Events {
	noon := SolarNoon(t, lon)
	e := Events{SolarNoon: noon}
	e.NoonElevationDeg = SunAt(noon, lat, lon).TrueElevationDeg
	// Lowest point: the nearer of the two solar midnights is equivalent to
	// within the daily change of declination.
	e.MidnightElevationDeg = SunAt(noon.Add(12*time.Hour), lat, lon).TrueElevationDeg

	var st int
	e.Sunrise, e.Sunset, st = crossings(noon, lat, lon, SunriseAltitudeDeg)
	switch st {
	case crossAlwaysAbove:
		e.PolarDay = true
		e.DayLength = 24 * time.Hour
	case crossAlwaysBelow:
		e.PolarNight = true
	default:
		e.DayLength = e.Sunset.Sub(e.Sunrise)
	}
	e.CivilDawn, e.CivilDusk, _ = crossings(noon, lat, lon, CivilAltitudeDeg)
	e.NauticalDawn, e.NauticalDusk, _ = crossings(noon, lat, lon, NauticalAltitudeDeg)
	e.AstroDawn, e.AstroDusk, _ = crossings(noon, lat, lon, AstronomicalAltitudeDeg)
	return e
}

// DayEventsOn returns the solar events of a calendar date at (lat,lon): the
// solar day whose noon falls on that date in local mean solar time (UTC +
// lon/15 h), which is the date a resident would call it give or take the
// offset of the civil time zone from the sun. All returned instants are UTC.
// Use DayEvents for "the solar day around this instant".
func DayEventsOn(year int, month time.Month, day int, lat, lon float64) Events {
	meanNoon := time.Date(year, month, day, 12, 0, 0, 0, time.UTC).
		Add(-time.Duration(norm180(lon) * 4 * float64(time.Minute)))
	return DayEvents(meanNoon, lat, lon)
}
