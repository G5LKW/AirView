package geo

import (
	"math"
	"strings"
	"testing"
)

func TestParseLatLon(t *testing.T) {
	const london, londonLon = 51.5074, -0.1278
	cases := []struct {
		in       string
		lat, lon float64
		tol      float64
		format   string // substring expected in the format description
	}{
		{"51.5074, -0.1278", london, londonLon, 1e-9, FmtDecimal},
		{"51.5074 -0.1278", london, londonLon, 1e-9, FmtDecimal},
		{"  51.5074,-0.1278  ", london, londonLon, 1e-9, FmtDecimal},
		{"(51.5074, -0.1278)", london, londonLon, 1e-9, FmtDecimal},
		{"51.5074; -0.1278", london, londonLon, 1e-9, FmtDecimal},
		{"51,5074 -0,1278", london, londonLon, 1e-9, FmtDecimal},
		{"51,5074; -0,1278", london, londonLon, 1e-9, FmtDecimal},
		{"+51.5074 +0.1278", london, 0.1278, 1e-9, FmtDecimal},
		{"51.5074, −0.1278", london, londonLon, 1e-9, FmtDecimal}, // unicode minus
		{"-33.8688, 151.2093", -33.8688, 151.2093, 1e-9, FmtDecimal},
		{"51, 0", 51, 0, 1e-9, FmtDecimal},
		{".5 -.25", 0.5, -0.25, 1e-9, FmtDecimal},

		{"51.5074N 0.1278W", london, londonLon, 1e-9, FmtDecimalHemi},
		{"51.5074° N, 0.1278° W", london, londonLon, 1e-9, FmtDecimalHemi},
		{"N51.5074 W0.1278", london, londonLon, 1e-9, FmtDecimalHemi},
		{"N 51.5074, W 0.1278", london, londonLon, 1e-9, FmtDecimalHemi},
		{"0.1278W 51.5074N", london, londonLon, 1e-9, FmtDecimalHemi}, // lon first, resolved by letters
		{"51.5074 north 0.1278 west", london, londonLon, 1e-9, FmtDecimalHemi},
		{"40.7128° N, 74.0060° W", 40.7128, -74.0060, 1e-9, FmtDecimalHemi},
		{"33.8688s 151.2093e", -33.8688, 151.2093, 1e-9, FmtDecimalHemi},
		{"51.5074W 0.1278N", 0.1278, -51.5074, 1e-9, FmtDecimalHemi}, // letters decide, not order

		{"51 30 26 N 0 7 39 W", dms(51, 30, 26), -dms(0, 7, 39), 1e-9, FmtDMS},
		{`51°30'26.6"N 0°07'39.9"W`, dms(51, 30, 26.6), -dms(0, 7, 39.9), 1e-9, FmtDMS},
		{"51°30′26.6″N 0°07′39.9″W", dms(51, 30, 26.6), -dms(0, 7, 39.9), 1e-9, FmtDMS},
		{"51° 30' 26'' N, 0° 7' 39'' W", dms(51, 30, 26), -dms(0, 7, 39), 1e-9, FmtDMS},
		{`S 33° 51' 54.5" E 151° 12' 35.6"`, -dms(33, 51, 54.5), dms(151, 12, 35.6), 1e-9, FmtDMS},
		{"51-30-26N 000-07-39W", dms(51, 30, 26), -dms(0, 7, 39), 1e-9, FmtDMS},
		{"51:30:26N 0:07:39W", dms(51, 30, 26), -dms(0, 7, 39), 1e-9, FmtDMS},
		{"51 30 26, -0 7 39", dms(51, 30, 26), -dms(0, 7, 39), 1e-9, FmtDMS}, // sign on a zero degree field
		{"51 30 26 -0 7 39", dms(51, 30, 26), -dms(0, 7, 39), 1e-9, FmtDMS},
		{"51 deg 30 min 26 sec N 0 deg 7 min 39 sec W", dms(51, 30, 26), -dms(0, 7, 39), 1e-9, FmtDMS},

		{"N51 30.433 W000 07.650", 51 + 30.433/60, -(7.650 / 60), 1e-9, FmtDDM},
		{"51° 30.433' N 0° 7.650' W", 51 + 30.433/60, -(7.650 / 60), 1e-9, FmtDDM},
		{"S33 52.128 E151 12.558", -(33 + 52.128/60), 151 + 12.558/60, 1e-9, FmtDDM},
		{"51 30.433, -0 7.650", 51 + 30.433/60, -(7.650 / 60), 1e-9, FmtDDM},

		{"5130.433N 00007.650W", 51 + 30.433/60, -(7.650 / 60), 1e-9, FmtCompactDM},
		{"5130N 00007W", 51.5, -(7.0 / 60), 1e-9, FmtCompactDM},
		{"513026N 0000739W", dms(51, 30, 26), -dms(0, 7, 39), 1e-9, FmtCompactDMS},
		{"513026N0000739W", dms(51, 30, 26), -dms(0, 7, 39), 1e-9, FmtCompactDMS},

		{"lat: 51.5074 lon: -0.1278", london, londonLon, 1e-9, FmtDecimal},
		{"Latitude: 51.5074, Longitude: -0.1278", london, londonLon, 1e-9, FmtDecimal},
		{"lon=-0.1278, lat=51.5074", london, londonLon, 1e-9, FmtDecimal},
		{"Lat 51.5074° N, Long 0.1278° W", london, londonLon, 1e-9, FmtDecimalHemi},

		{"IO91", 51.5, -1, 1e-9, FmtMaidenhead},
		{"IO91wm", 51.5 + 1.0/48, -0.125, 1e-9, FmtMaidenhead},
		{"io91WM", 51.5 + 1.0/48, -0.125, 1e-9, FmtMaidenhead},
		{"IO91 wm", 51.5 + 1.0/48, -0.125, 1e-9, FmtMaidenhead},
		{"IO91wm42", 51.5 + 2.0/240 + 1.0/480, -2 + 22.0/12 + 4.0/120 + 1.0/240, 1e-9, "8 characters"},
		{"JN58td", 48 + 3.0/24 + 1.0/48, 10 + 19.0/12 + 1.0/24, 1e-9, FmtMaidenhead}, // Munich
		{"AA00", -89.5, -179, 1e-9, FmtMaidenhead},

		{"8FVC9G8F+6X", 47.3655625, 8.5249375, 1e-9, FmtPlusCode},
		{"8fvc9g8f+6x Zürich", 47.3655625, 8.5249375, 1e-9, FmtPlusCode},

		{"geo:51.5074,-0.1278", london, londonLon, 1e-9, FmtGeoURI},
		{"geo:51.5074,-0.1278,35;u=20", london, londonLon, 1e-9, FmtGeoURI},
		{"https://www.google.com/maps/@51.5074,-0.1278,15z", london, londonLon, 1e-9, FmtGoogleMapsView},
		{"https://www.google.com/maps/place/London/@51.5,-0.12,10z/data=!3m1!4b1!4m6!3m5!1s0x0:0x0!8m2!3d51.5072178!4d-0.1275862",
			51.5072178, -0.1275862, 1e-9, FmtGoogleMapsPin},
		{"https://maps.google.com/?q=51.5074,-0.1278", london, londonLon, 1e-9, FmtMapURL},
		{"https://www.google.com/maps/search/?api=1&query=51.5074%2C-0.1278", london, londonLon, 1e-9, FmtMapURL},
		{"https://www.openstreetmap.org/#map=15/51.5074/-0.1278", london, londonLon, 1e-9, FmtOSMURL},
		{"https://www.openstreetmap.org/?mlat=51.5074&mlon=-0.1278#map=15/51.5/-0.12", london, londonLon, 1e-9, FmtOSMURL},
		{"https://www.bing.com/maps?cp=51.5074~-0.1278&lvl=12", london, londonLon, 1e-9, FmtMapURL},
		{"https://example.org/view?lat=51.5074&lng=-0.1278", london, londonLon, 1e-9, FmtMapURL},
	}
	for _, c := range cases {
		lat, lon, format, err := ParseLatLon(c.in)
		if err != nil {
			t.Errorf("ParseLatLon(%q) error: %v", c.in, err)
			continue
		}
		if math.Abs(lat-c.lat) > c.tol || math.Abs(lon-c.lon) > c.tol {
			t.Errorf("ParseLatLon(%q) = %.7f, %.7f; want %.7f, %.7f", c.in, lat, lon, c.lat, c.lon)
		}
		if !strings.Contains(format, c.format) {
			t.Errorf("ParseLatLon(%q) format %q, want it to contain %q", c.in, format, c.format)
		}
	}
}

func TestParseLatLonRejects(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"hello",
		"51.5",                           // one number
		"1 2 3",                          // cannot split
		"1 2 3 4 5",                      // odd count
		"91, 0",                          // lat out of range
		"-90.01, 0",                      //
		"0, 181",                         // lon out of range
		"0, -180.5",                      //
		"51 60 0 N 0 0 0 E",              // minutes = 60
		"51 30 60 N 0 0 0 E",             // seconds = 60
		"51.5 30 N 0 7 W",                // fractional degrees with minutes
		"51.5N 0.1N",                     // both latitude
		"51.5E 0.1W",                     // both longitude
		"-51.5S 0.1W",                    // sign and hemisphere
		"51.5N",                          // missing longitude
		"51.5N 0.12",                     // only one hemisphere letter
		"N 51.5, 0.12 W",                 // mixed prefix/suffix
		"51.5 - 0.12",                    // ambiguous dash
		"51.5074, -0.1278, 35",           // three groups
		"GV4C+XV London",                 // short plus code
		"https://maps.app.goo.gl/abc123", // shortened link
		"geo:somewhere",
		"IO91w",                   // odd-length locator -> not a locator, not numbers
		"SZ91",                    // S and Z outside A-R
		"51.5074 X 0.1278",        // unknown letter
		"9160N 00000E",            // compact minutes >= 60
		strings.Repeat("1", 5000), // too long
	}
	for _, in := range bad {
		if lat, lon, format, err := ParseLatLon(in); err == nil {
			t.Errorf("ParseLatLon(%q) = %v, %v (%s); want an error", in, lat, lon, format)
		}
	}
}

func TestFormat(t *testing.T) {
	if got := FormatDecimal(51.5074, -0.1278, 5); got != "51.50740, -0.12780" {
		t.Errorf("FormatDecimal = %q", got)
	}
	if got := FormatDMS(51.5074, -0.1278, 1); got != `51°30'26.6"N 0°07'40.1"W` {
		t.Errorf("FormatDMS = %q", got)
	}
	if got := FormatDMS(-33.8688, 151.2093, 0); got != `33°52'08"S 151°12'33"E` {
		t.Errorf("FormatDMS = %q", got)
	}
	// Rounding must carry, never print 60 seconds or 60 minutes.
	if got := FormatDMS(59.9999999, 9.99999999, 1); got != `60°00'00.0"N 10°00'00.0"E` {
		t.Errorf("FormatDMS carry = %q", got)
	}
	if got := FormatDDM(51.5074, -0.1278, 3); got != "N51 30.444 W000 07.668" {
		t.Errorf("FormatDDM = %q", got)
	}
	if got := FormatDDM(-0.99999999, 179.99999999, 3); got != "S01 00.000 E180 00.000" {
		t.Errorf("FormatDDM carry = %q", got)
	}

	loc := []struct {
		lat, lon float64
		chars    int
		want     string
	}{
		{51.5074, -0.1278, 6, "IO91wm"},
		{51.5074, -0.1278, 4, "IO91"},
		{51.5074, -0.1278, 2, "IO"},
		{48.1466, 11.6083, 6, "JN58td"}, // Munich
		{-34.91, -56.21, 6, "GF15vc"},   // Montevideo
		{38.92, -77.065, 6, "FM18lw"},   // Washington DC
		{90, 180, 4, "AR09"},            // lon wraps to -180, lat clamps into the top row
		{-90, -180, 6, "AA00aa"},
	}
	for _, c := range loc {
		if got := Maidenhead(c.lat, c.lon, c.chars); got != c.want {
			t.Errorf("Maidenhead(%v,%v,%d) = %q, want %q", c.lat, c.lon, c.chars, got, c.want)
		}
	}

	// Non-finite input never produces garbage digits.
	if got := FormatDMS(math.NaN(), math.Inf(1), 1); got != "? ?" {
		t.Errorf("FormatDMS(NaN, Inf) = %q", got)
	}
	if got := FormatDDM(math.Inf(-1), math.NaN(), 3); got != "? ?" {
		t.Errorf("FormatDDM(-Inf, NaN) = %q", got)
	}
	if got := Maidenhead(math.NaN(), 0, 6); got != "" {
		t.Errorf("Maidenhead(NaN) = %q", got)
	}

	// Every formatter round-trips through the parser.
	pts := [][2]float64{{51.5074, -0.1278}, {-33.8688, 151.2093}, {0.0001, -0.0001}, {-89.5, 179.5}, {64.1466, -21.9426}}
	for _, p := range pts {
		for name, s := range map[string]string{
			"decimal": FormatDecimal(p[0], p[1], 6),
			"dms":     FormatDMS(p[0], p[1], 3),
			"ddm":     FormatDDM(p[0], p[1], 5),
		} {
			lat, lon, _, err := ParseLatLon(s)
			if err != nil || math.Abs(lat-p[0]) > 2e-6 || math.Abs(lon-p[1]) > 2e-6 {
				t.Errorf("%s round trip %q -> %v, %v, %v (want %v)", name, s, lat, lon, err, p)
			}
		}
		for _, n := range []int{4, 6, 8, 10} {
			loc := Maidenhead(p[0], p[1], n)
			lat, lon, err := ParseMaidenhead(loc)
			if err != nil {
				t.Errorf("ParseMaidenhead(%q): %v", loc, err)
				continue
			}
			if Maidenhead(lat, lon, n) != loc {
				t.Errorf("locator %q centre %v,%v re-encodes to %q", loc, lat, lon, Maidenhead(lat, lon, n))
			}
		}
	}
}
