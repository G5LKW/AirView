package astro

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f (±%g)", name, got, want, tol)
	}
}

func nearTime(t *testing.T, name string, got, want time.Time, tol time.Duration) {
	t.Helper()
	d := got.Sub(want)
	if d < 0 {
		d = -d
	}
	if got.IsZero() || d > tol {
		t.Errorf("%s = %s, want %s (±%s)", name, got.Format(time.RFC3339), want.Format(time.RFC3339), tol)
	}
}

func utc(y int, m time.Month, d, hh, mm, ss int) time.Time {
	return time.Date(y, m, d, hh, mm, ss, 0, time.UTC)
}

func TestJulianDay(t *testing.T) {
	near(t, "J2000", JulianDay(utc(2000, 1, 1, 12, 0, 0)), 2451545.0, 1e-9)
	// Meeus example 7.a: 1957 October 4.81 = JD 2436116.31.
	near(t, "Sputnik", JulianDay(utc(1957, 10, 4, 19, 26, 24)), 2436116.31, 1e-6)
}

func TestSolarCoordsMeeus(t *testing.T) {
	// Meeus example 25.a: 1992 October 13, 0h TD (JDE 2448908.5):
	// apparent RA = 13h13m31.4s = 198.38083°, declination = −7°47'06" = −7.78507°.
	// Example 28.a (same instant): equation of time = +13m42.6s.
	s := solarCoords(2448908.5)
	near(t, "declination", s.decl, -7.78507, 0.003)
	near(t, "right ascension", s.ra, 198.38083, 0.003)
	near(t, "equation of time", s.eqTime, 13+42.6/60, 0.03)
	near(t, "distance", s.dist, 0.99766, 0.0001)
}

func TestSolsticeEquinox(t *testing.T) {
	// June solstice 2024-06-20 20:51 UTC: declination = obliquity ≈ 23.436°.
	lat, _ := SubsolarPoint(utc(2024, 6, 20, 20, 51, 0))
	near(t, "solstice declination", lat, 23.436, 0.005)
	// March equinox 2024-03-20 03:06 UTC: declination crosses zero.
	lat, _ = SubsolarPoint(utc(2024, 3, 20, 3, 6, 0))
	near(t, "equinox declination", lat, 0, 0.01)
	// December solstice 2024-12-21 09:21 UTC.
	lat, _ = SubsolarPoint(utc(2024, 12, 21, 9, 21, 0))
	near(t, "december declination", lat, -23.436, 0.005)

	// At 12:00 UTC the sub-solar longitude is −eqTime/4 degrees.
	_, lon := SubsolarPoint(utc(2024, 11, 3, 12, 0, 0)) // eq. of time ≈ +16m25s
	near(t, "subsolar lon", lon, -(16+25.0/60)/4, 0.03)
	// The sun is at the zenith at the sub-solar point.
	ts := utc(2024, 8, 9, 15, 30, 0)
	lat, lon = SubsolarPoint(ts)
	near(t, "zenith", SunAt(ts, lat, lon).TrueElevationDeg, 90, 1e-4)
}

func TestSolarNoonEquationOfTime(t *testing.T) {
	// Greenwich meridian: solar noon = 12:00 − equation of time.
	// Well-known extremes: ≈ +16m25s on 3 Nov, ≈ −14m14s on 11 Feb; −7m28s on 20 Mar.
	nearTime(t, "noon 3 Nov", SolarNoon(utc(2024, 11, 3, 9, 0, 0), 0), utc(2024, 11, 3, 11, 43, 35), 20*time.Second)
	nearTime(t, "noon 11 Feb", SolarNoon(utc(2024, 2, 11, 15, 0, 0), 0), utc(2024, 2, 11, 12, 14, 14), 20*time.Second)
	nearTime(t, "noon 20 Mar", SolarNoon(utc(2024, 3, 20, 12, 0, 0), 0), utc(2024, 3, 20, 12, 7, 28), 20*time.Second)
	// 15° of longitude is one hour.
	a := SolarNoon(utc(2024, 5, 5, 12, 0, 0), 0)
	b := SolarNoon(utc(2024, 5, 5, 12, 0, 0), 15)
	near(t, "15° east = 1 h earlier", a.Sub(b).Minutes(), 60, 0.01)
	// Nearest transit is always within 12 h.
	for h := 0; h < 24; h++ {
		ts := utc(2024, 7, 1, h, 30, 0)
		n := SolarNoon(ts, 151.2)
		if d := n.Sub(ts); d > 12*time.Hour+time.Minute || d < -12*time.Hour-time.Minute {
			t.Errorf("SolarNoon(%v) = %v: more than 12 h away", ts, n)
		}
		if ha := SunAt(n, -33.87, 151.2).HourAngleDeg; math.Abs(ha) > 1e-4 {
			t.Errorf("hour angle at solar noon = %v", ha)
		}
	}
}

func TestDayEventsLondon(t *testing.T) {
	const lat, lon = 51.5074, -0.1278
	// Published times (timeanddate.com, London): 21 Jun 2024 sunrise 04:43, sunset 21:21 BST;
	// 21 Dec 2024 sunrise 08:04, sunset 15:53 GMT.
	e := DayEvents(utc(2024, 6, 21, 12, 0, 0), lat, lon)
	nearTime(t, "summer sunrise", e.Sunrise, utc(2024, 6, 21, 3, 43, 0), 90*time.Second)
	nearTime(t, "summer sunset", e.Sunset, utc(2024, 6, 21, 20, 21, 30), 90*time.Second)
	nearTime(t, "summer noon", e.SolarNoon, utc(2024, 6, 21, 12, 2, 20), 60*time.Second)
	near(t, "summer day length h", e.DayLength.Hours(), 16+38.0/60, 0.03)
	near(t, "summer noon elevation", e.NoonElevationDeg, 90-lat+23.44, 0.02)
	near(t, "summer midnight elevation", e.MidnightElevationDeg, -(90 - lat - 23.44), 0.03)
	if e.PolarDay || e.PolarNight {
		t.Error("London is not polar")
	}
	// The sun never gets 18° below the London horizon at midsummer: no
	// astronomical dawn/dusk, but nautical and civil twilight exist.
	if !e.AstroDawn.IsZero() || !e.AstroDusk.IsZero() {
		t.Errorf("midsummer London must have no astronomical night: %v %v", e.AstroDawn, e.AstroDusk)
	}
	if e.NauticalDawn.IsZero() || e.CivilDawn.IsZero() || e.CivilDusk.IsZero() || e.NauticalDusk.IsZero() {
		t.Error("missing twilight events")
	}
	if !(e.NauticalDawn.Before(e.CivilDawn) && e.CivilDawn.Before(e.Sunrise) && e.Sunrise.Before(e.SolarNoon) &&
		e.SolarNoon.Before(e.Sunset) && e.Sunset.Before(e.CivilDusk) && e.CivilDusk.Before(e.NauticalDusk)) {
		t.Errorf("events out of order: %+v", e)
	}
	// Each event sits exactly at its defining altitude.
	near(t, "alt at sunrise", SunAt(e.Sunrise, lat, lon).TrueElevationDeg, SunriseAltitudeDeg, 1e-3)
	near(t, "alt at sunset", SunAt(e.Sunset, lat, lon).TrueElevationDeg, SunriseAltitudeDeg, 1e-3)
	near(t, "alt at civil dawn", SunAt(e.CivilDawn, lat, lon).TrueElevationDeg, -6, 1e-3)
	near(t, "alt at nautical dusk", SunAt(e.NauticalDusk, lat, lon).TrueElevationDeg, -12, 1e-3)
	// Midsummer sunrise in London is in the north-east (~49°), sunset north-west (~311°).
	near(t, "sunrise azimuth", SunAt(e.Sunrise, lat, lon).AzimuthDeg, 49, 1.0)
	near(t, "sunset azimuth", SunAt(e.Sunset, lat, lon).AzimuthDeg, 311, 1.0)
	near(t, "noon azimuth", SunAt(e.SolarNoon, lat, lon).AzimuthDeg, 180, 0.01)

	// Any instant of the same solar day gives the same events.
	for _, h := range []int{1, 6, 18, 23} {
		e2 := DayEvents(utc(2024, 6, 21, h, 0, 0), lat, lon)
		nearTime(t, "same day sunrise", e2.Sunrise, e.Sunrise, time.Second)
	}

	e = DayEvents(utc(2024, 12, 21, 12, 0, 0), lat, lon)
	nearTime(t, "winter sunrise", e.Sunrise, utc(2024, 12, 21, 8, 4, 0), 90*time.Second)
	nearTime(t, "winter sunset", e.Sunset, utc(2024, 12, 21, 15, 53, 30), 90*time.Second)
	if e.AstroDawn.IsZero() || e.AstroDusk.IsZero() {
		t.Error("winter London has astronomical night")
	}
	near(t, "alt at astro dawn", SunAt(e.AstroDawn, lat, lon).TrueElevationDeg, -18, 1e-3)

	m := e.Ms()
	if m.Sunrise != e.Sunrise.UnixMilli() || m.SolarNoon == 0 || m.PolarDay {
		t.Errorf("Ms() = %+v", m)
	}
	if DayEvents(utc(2024, 6, 21, 12, 0, 0), lat, lon).Ms().AstroDawn != 0 {
		t.Error("missing event must be 0 ms")
	}
}

func TestDayEventsOtherPlaces(t *testing.T) {
	// Sydney (east of Greenwich, southern hemisphere): 21 Dec 2024 sunrise 05:41,
	// sunset 20:05 AEDT (UTC+11) per timeanddate.com.
	aedt := time.FixedZone("AEDT", 11*3600)
	e := DayEvents(time.Date(2024, 12, 21, 12, 0, 0, 0, aedt), -33.8688, 151.2093)
	nearTime(t, "Sydney sunrise", e.Sunrise, time.Date(2024, 12, 21, 5, 41, 0, 0, aedt), 2*time.Minute)
	nearTime(t, "Sydney sunset", e.Sunset, time.Date(2024, 12, 21, 20, 5, 0, 0, aedt), 2*time.Minute)

	// Equator at the equinox: ~12 h 07 m of daylight (refraction + disc size).
	e = DayEvents(utc(2024, 3, 20, 12, 0, 0), 0, 0)
	near(t, "equator day length", e.DayLength.Minutes(), 12*60+6.7, 0.7)
}

func TestDayEventsOn(t *testing.T) {
	// The date is the local (mean solar) date, wherever on earth the place is.
	places := []struct {
		name     string
		lat, lon float64
		zoneH    float64 // civil offset used only to check the date of noon
	}{
		{"London", 51.5074, -0.1278, 0},
		{"Sydney", -33.8688, 151.2093, 10},
		{"Honolulu", 21.3069, -157.8583, -10},
		{"Auckland", -36.8485, 174.7633, 12},
		{"Anchorage", 61.2181, -149.9003, -9},
	}
	for _, p := range places {
		e := DayEventsOn(2024, time.September, 10, p.lat, p.lon)
		local := e.SolarNoon.Add(time.Duration(p.zoneH * float64(time.Hour)))
		if local.Day() != 10 || local.Month() != time.September {
			t.Errorf("%s: solar noon %v is not on 10 September local time", p.name, local)
		}
		if h := local.Hour(); h < 10 || h > 14 {
			t.Errorf("%s: solar noon at %v local", p.name, local)
		}
		if e.Sunrise.IsZero() || !e.Sunrise.Before(e.SolarNoon) || !e.SolarNoon.Before(e.Sunset) {
			t.Errorf("%s: %+v", p.name, e)
		}
		// Identical to DayEvents for any instant of that solar day.
		e2 := DayEvents(e.SolarNoon.Add(-5*time.Hour), p.lat, p.lon)
		nearTime(t, p.name+" same day", e2.Sunset, e.Sunset, time.Second)
	}
	// Consecutive dates give consecutive days, ~24 h apart.
	a := DayEventsOn(2024, time.February, 28, 51.5, -0.13)
	b := DayEventsOn(2024, time.February, 29, 51.5, -0.13)
	c := DayEventsOn(2024, time.March, 1, 51.5, -0.13)
	near(t, "leap day", b.SolarNoon.Sub(a.SolarNoon).Hours(), 24, 0.01)
	near(t, "day after leap day", c.SolarNoon.Sub(b.SolarNoon).Hours(), 24, 0.01)
	// Published (timeanddate.com) London 21 Jun 2024: sunrise 04:43 BST.
	nearTime(t, "London sunrise", DayEventsOn(2024, time.June, 21, 51.5074, -0.1278).Sunrise, utc(2024, 6, 21, 3, 43, 0), 90*time.Second)
}

func TestPolar(t *testing.T) {
	const lat, lon = 69.6492, 18.9553 // Tromsø
	e := DayEvents(utc(2024, 6, 21, 11, 0, 0), lat, lon)
	if !e.PolarDay || e.PolarNight || !e.Sunrise.IsZero() || !e.Sunset.IsZero() || e.DayLength != 24*time.Hour {
		t.Errorf("Tromsø midsummer should be polar day: %+v", e)
	}
	if e.MidnightElevationDeg < 2 || e.MidnightElevationDeg > 4 { // midnight sun ~3° up
		t.Errorf("midnight sun elevation %v", e.MidnightElevationDeg)
	}
	if !e.CivilDawn.IsZero() || !e.AstroDusk.IsZero() {
		t.Error("no twilight events during polar day")
	}

	e = DayEvents(utc(2024, 12, 21, 11, 0, 0), lat, lon)
	if !e.PolarNight || e.PolarDay || !e.Sunrise.IsZero() || e.DayLength != 0 {
		t.Errorf("Tromsø midwinter should be polar night: %+v", e)
	}
	// ...but it still has civil twilight around noon (sun ~3° below the horizon).
	if e.CivilDawn.IsZero() || e.CivilDusk.IsZero() || !e.CivilDawn.Before(e.CivilDusk) {
		t.Errorf("Tromsø midwinter civil twilight missing: %+v", e)
	}
	if e.NoonElevationDeg > -2 || e.NoonElevationDeg < -4 {
		t.Errorf("noon elevation %v", e.NoonElevationDeg)
	}

	// Exactly at the poles nothing may blow up.
	for _, la := range []float64{90, -90} {
		e := DayEvents(utc(2024, 6, 21, 12, 0, 0), la, 0)
		if e.PolarDay == e.PolarNight {
			t.Errorf("pole %v: %+v", la, e)
		}
		if p := SunAt(utc(2024, 6, 21, 12, 0, 0), la, 0); math.IsNaN(p.AzimuthDeg) || math.IsNaN(p.ElevationDeg) {
			t.Errorf("pole position NaN: %+v", p)
		}
	}
	if !DayEvents(utc(2024, 6, 21, 12, 0, 0), 90, 0).PolarDay || !DayEvents(utc(2024, 6, 21, 12, 0, 0), -90, 0).PolarNight {
		t.Error("June: north pole day, south pole night")
	}
}

func TestPhases(t *testing.T) {
	const lat, lon = 51.5074, -0.1278
	cases := []struct {
		t    time.Time
		want Phase
	}{
		{utc(2024, 6, 21, 12, 0, 0), Day},
		{utc(2024, 6, 21, 0, 2, 0), AstronomicalTwilight}, // London midsummer midnight: sun only 15° down
		{utc(2024, 12, 21, 0, 0, 0), Night},
		{utc(2024, 12, 21, 12, 0, 0), Day},
		{utc(2024, 12, 21, 16, 10, 0), CivilTwilight},    // 17 min after sunset
		{utc(2024, 12, 21, 16, 55, 0), NauticalTwilight}, // civil dusk is ~16:33
		{utc(2024, 12, 21, 17, 35, 0), AstronomicalTwilight},
		{utc(2024, 12, 21, 19, 0, 0), Night},
	}
	for _, c := range cases {
		if got := PhaseAt(c.t, lat, lon); got != c.want {
			t.Errorf("PhaseAt(%s) = %s (elev %.2f), want %s", c.t.Format(time.RFC3339), got,
				SunAt(c.t, lat, lon).TrueElevationDeg, c.want)
		}
		if IsDay(c.t, lat, lon) != (c.want == Day) {
			t.Errorf("IsDay(%s) wrong", c.t.Format(time.RFC3339))
		}
	}
	bounds := []struct {
		el   float64
		want Phase
	}{{10, Day}, {-0.5, Day}, {-0.9, CivilTwilight}, {-6, CivilTwilight}, {-6.01, NauticalTwilight},
		{-12.01, AstronomicalTwilight}, {-18, AstronomicalTwilight}, {-18.01, Night}, {-60, Night}}
	for _, b := range bounds {
		if got := PhaseOfElevation(b.el); got != b.want {
			t.Errorf("PhaseOfElevation(%v) = %v, want %v", b.el, got, b.want)
		}
	}
	if Day.String() != "day" || Night.String() != "night" || CivilTwilight.String() != "civil" {
		t.Error("Phase.String")
	}
}

func TestRefraction(t *testing.T) {
	near(t, "horizon", Refraction(0), 1735.0/3600, 1e-9) // ~0.48°
	near(t, "10°", Refraction(10), 0.0883, 0.002)        // ~5.3'
	near(t, "45°", Refraction(45), 58.1/3600, 0.0002)    // ~1'
	near(t, "zenith", Refraction(90), 0, 0)
	if r := Refraction(-5); r < 0 || r > 0.1 {
		t.Errorf("Refraction(-5) = %v", r)
	}
	// A sun whose centre is geometrically 0.575° below the horizon appears
	// exactly on it; the two lower branches join continuously there.
	near(t, "apparent horizon", -0.575+Refraction(-0.575), 0, 0.002)
	near(t, "branch continuity", Refraction(-0.5751), Refraction(-0.5749), 0.001)
}

// pointInRing is an even-odd test in plain lon/lat space.
func pointInRing(ring [][2]float64, lon, lat float64) bool {
	in := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		xi, yi := ring[i][0], ring[i][1]
		xj, yj := ring[j][0], ring[j][1]
		if (yi > lat) != (yj > lat) && lon < (xj-xi)*(lat-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

func ringArea(ring [][2]float64) float64 { // signed, > 0 = counter-clockwise
	var a float64
	for i := 0; i+1 < len(ring); i++ {
		a += ring[i][0]*ring[i+1][1] - ring[i+1][0]*ring[i][1]
	}
	return a / 2
}

func inRingAnyWorld(ring [][2]float64, lon, lat float64) bool {
	return pointInRing(ring, lon, lat) || pointInRing(ring, lon+360, lat) || pointInRing(ring, lon-360, lat)
}

func TestNightRing(t *testing.T) {
	times := []time.Time{
		utc(2024, 6, 21, 10, 0, 0),  // south pole dark
		utc(2024, 12, 21, 22, 0, 0), // north pole dark
		utc(2024, 3, 20, 3, 6, 0),   // equinox: boundary runs pole to pole
		utc(2024, 3, 20, 15, 0, 0),
		utc(2024, 9, 22, 12, 44, 0),
		utc(2024, 4, 25, 1, 0, 0), // declination ~13°: -12 cap has the pole, -18 cap does not
		utc(2025, 2, 3, 18, 30, 0),
	}
	alts := []float64{0, SunriseAltitudeDeg, -6, -12, -18}
	for _, ts := range times {
		subLat, subLon := SubsolarPoint(ts)
		antiLat, antiLon := -subLat, norm180(subLon+180)
		for _, alt := range alts {
			ring := NightRing(ts, alt, 3)
			if len(ring) < 100 {
				t.Fatalf("%v alt %v: ring too short (%d)", ts, alt, len(ring))
			}
			if ring[0] != ring[len(ring)-1] {
				t.Errorf("%v alt %v: ring not closed", ts, alt)
			}
			if ringArea(ring) <= 0 {
				t.Errorf("%v alt %v: ring is not counter-clockwise", ts, alt)
			}
			poleClosed := false
			for i, p := range ring {
				if p[1] < -90 || p[1] > 90 || math.IsNaN(p[0]) || math.IsNaN(p[1]) {
					t.Fatalf("%v alt %v: bad vertex %v", ts, alt, p)
				}
				if math.Abs(p[1]) == 90 {
					poleClosed = true
					if math.Abs(p[0]) != 180 {
						t.Errorf("pole closure vertex at lon %v", p[0])
					}
					continue
				}
				// Boundary vertices lie exactly on the altitude contour.
				el := SunAt(ts, p[1], p[0]).TrueElevationDeg
				if math.Abs(el-alt) > 2e-3 {
					t.Fatalf("%v alt %v: vertex %d %v has sun altitude %.5f", ts, alt, i, p, el)
				}
				// No wild jumps between consecutive boundary vertices.
				if i > 0 && math.Abs(ring[i-1][1]) != 90 && math.Abs(p[0]-ring[i-1][0]) > 100 {
					t.Fatalf("%v alt %v: longitude jump %v -> %v", ts, alt, ring[i-1], p)
				}
			}
			if poleClosed {
				for _, p := range ring {
					if p[0] < -180 || p[0] > 180 {
						t.Fatalf("pole-closed ring leaves [-180,180]: %v", p)
					}
				}
			}
			// The anti-solar point is in the dark, the sub-solar point is not,
			// and a scatter of test points agrees with the actual sun altitude.
			if !inRingAnyWorld(ring, antiLon, antiLat) {
				t.Errorf("%v alt %v: anti-solar point not inside the night ring", ts, alt)
			}
			if inRingAnyWorld(ring, subLon, subLat) {
				t.Errorf("%v alt %v: sub-solar point inside the night ring", ts, alt)
			}
			for la := -85.0; la <= 85; la += 17 {
				for lo := -175.0; lo <= 175; lo += 23 {
					el := SunAt(ts, la, lo).TrueElevationDeg
					if math.Abs(el-alt) < 1.5 {
						continue // too close to the (polyline-approximated) boundary
					}
					if got, want := inRingAnyWorld(ring, lo, la), el < alt; got != want {
						t.Fatalf("%v alt %v: point %v,%v sun %.2f° inside=%v want %v", ts, alt, la, lo, el, got, want)
					}
				}
			}
		}
	}

	// The equinox terminator must be closed over a pole, not left as an open band.
	ring := NightRing(utc(2024, 3, 20, 3, 6, 0), 0, 2)
	hasPole := false
	for _, p := range ring {
		if math.Abs(p[1]) == 90 {
			hasPole = true
		}
	}
	if !hasPole {
		t.Error("equinox terminator ring lacks the pole closure")
	}
	// ClampRing limits the pole closure to the Web-Mercator world without
	// touching the caller's ring.
	cl := ClampRing(ring, WebMercatorMaxLat)
	if len(cl) != len(ring) {
		t.Fatalf("ClampRing length %d", len(cl))
	}
	for i, p := range cl {
		if math.Abs(p[1]) > WebMercatorMaxLat || p[0] != ring[i][0] {
			t.Fatalf("ClampRing vertex %v (from %v)", p, ring[i])
		}
		if math.Abs(ring[i][1]) <= WebMercatorMaxLat && p != ring[i] {
			t.Fatalf("ClampRing changed in-range vertex %v -> %v", ring[i], p)
		}
	}
	stillHasPole := false
	for _, p := range ring {
		if math.Abs(p[1]) == 90 {
			stillHasPole = true
		}
	}
	if !stillHasPole {
		t.Error("ClampRing modified the caller's ring")
	}
	near(t, "mercator limit", WebMercatorMaxLat, math.Atan(math.Sinh(math.Pi))*180/math.Pi, 1e-12)

	// Default step and out-of-range arguments.
	if n := len(NightRing(utc(2024, 1, 1, 0, 0, 0), 0, 0)); n != 180+1+2+1 {
		t.Errorf("default step ring length %d", n)
	}
	if len(NightRing(utc(2024, 1, 1, 0, 0, 0), 5, 2)) != len(NightRing(utc(2024, 1, 1, 0, 0, 0), 0, 2)) {
		t.Error("positive altitude should clamp to 0")
	}
}

func TestNightGeoJSON(t *testing.T) {
	n := NightSideAt(utc(2024, 10, 5, 17, 0, 0), 5)
	if len(n.Sunset) == 0 || len(n.Civil) == 0 || len(n.Nautical) == 0 || len(n.Astronomical) == 0 {
		t.Fatal("missing rings")
	}
	if n.TimeMs != utc(2024, 10, 5, 17, 0, 0).UnixMilli() {
		t.Error("time")
	}
	b, err := json.Marshal(n.GeoJSON())
	if err != nil {
		t.Fatal(err)
	}
	var fc struct {
		Type     string `json:"type"`
		Features []struct {
			Type       string `json:"type"`
			Properties struct {
				Kind   string  `json:"kind"`
				SunAlt float64 `json:"sunAlt"`
			} `json:"properties"`
			Geometry struct {
				Type        string         `json:"type"`
				Coordinates [][][2]float64 `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(b, &fc); err != nil {
		t.Fatal(err)
	}
	if fc.Type != "FeatureCollection" || len(fc.Features) != 4 || fc.Features[3].Properties.Kind != "astronomical" ||
		fc.Features[0].Geometry.Type != "Polygon" || len(fc.Features[0].Geometry.Coordinates[0]) != len(n.Sunset) {
		t.Errorf("unexpected GeoJSON: %s", b[:200])
	}
	if _, err := json.Marshal(n); err != nil {
		t.Fatal(err)
	}
}

func TestMoon(t *testing.T) {
	// Meeus example 48.a: 1992 April 12, 0h TD: illuminated fraction 0.6786
	// (0.680 with the low-accuracy method used here), waxing.
	m := MoonAt(utc(1992, 4, 12, 0, 0, 0))
	near(t, "Meeus 48.a illumination", m.Illumination, 0.679, 0.006)
	if !m.Waxing || m.Name != "Waxing Gibbous" {
		t.Errorf("1992-04-12: %+v", m)
	}
	// Full moon 2024-01-25 17:54 UTC, new moon 2024-01-11 11:57 UTC,
	// first quarter 2024-01-18 03:53 UTC, last quarter 2024-02-02 23:18 UTC.
	m = MoonAt(utc(2024, 1, 25, 17, 54, 0))
	near(t, "full illumination", m.Illumination, 1, 0.003)
	near(t, "full phase", m.Phase, 0.5, 0.01)
	if m.Name != "Full Moon" {
		t.Errorf("name %q", m.Name)
	}
	m = MoonAt(utc(2024, 1, 11, 11, 57, 0))
	near(t, "new illumination", m.Illumination, 0, 0.003)
	if m.Name != "New Moon" || (m.Phase > 0.01 && m.Phase < 0.99) {
		t.Errorf("new moon: %+v", m)
	}
	m = MoonAt(utc(2024, 1, 18, 3, 53, 0))
	near(t, "first quarter illumination", m.Illumination, 0.5, 0.02)
	if m.Name != "First Quarter" || !m.Waxing {
		t.Errorf("first quarter: %+v", m)
	}
	m = MoonAt(utc(2024, 2, 2, 23, 18, 0))
	near(t, "last quarter illumination", m.Illumination, 0.5, 0.02)
	if m.Name != "Last Quarter" || m.Waxing {
		t.Errorf("last quarter: %+v", m)
	}
	near(t, "age", MoonAt(utc(2024, 1, 25, 17, 54, 0)).AgeDays, SynodicMonthDays/2, 0.4)
}
