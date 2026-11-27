package geo

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Format descriptions returned by ParseLatLon.
const (
	FmtDecimal        = "decimal degrees"
	FmtDecimalHemi    = "decimal degrees with hemisphere letters"
	FmtDDM            = "degrees and decimal minutes"
	FmtDMS            = "degrees, minutes, seconds"
	FmtCompactDM      = "compact DDMM.m (NMEA / aviation)"
	FmtCompactDMS     = "compact DDMMSS"
	FmtMaidenhead     = "Maidenhead locator" // followed by " (N characters, centre of square)"
	FmtPlusCode       = "Plus Code (centre of cell)"
	FmtGeoURI         = "geo: URI"
	FmtGoogleMapsPin  = "Google Maps URL (place pin)"
	FmtGoogleMapsView = "Google Maps URL (view centre)"
	FmtMapURL         = "map URL"
	FmtOSMURL         = "OpenStreetMap URL"
)

const maxCoordInput = 2048

// ParseLatLon parses a position in practically any form a user may paste and
// returns WGS84 latitude and longitude in degrees plus a human-readable
// description of the detected format. Accepted:
//
//	51.5074, -0.1278            decimal degrees (also "51.5074 -0.1278", "51,5074; -0,1278")
//	51.5074N 0.1278W            decimal degrees with hemisphere letters (prefix or suffix)
//	51 30 26 N 0 7 39 W         degrees minutes seconds, with or without ° ' " symbols
//	51°30'26.6"N 0°07'39.9"W    DMS as copied from Google Maps
//	N51 30.433 W000 07.650      degrees and decimal minutes
//	5130.433N 00007.650W        compact DDMM.mmm / DDDMM.mmm (NMEA), 513026N 0000739W (DDMMSS)
//	lat: 51.5074, lon: -0.1278  labelled values in either order
//	IO91wm                      Maidenhead locator, 4/6/8/10 characters (centre of the square)
//	9C3XGV4C+XV                 full Open Location Code ("Plus Code")
//	geo:51.5074,-0.1278         geo URI
//	https://www.google.com/maps/@51.5074,-0.1278,15z   Google/Bing/Apple/OSM map URLs
//
// Without hemisphere letters or labels the order is latitude, longitude.
// Out-of-range values (|lat| > 90, |lon| > 180, minutes or seconds >= 60)
// are rejected with a descriptive error.
func ParseLatLon(input string) (lat, lon float64, format string, err error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, 0, "", errors.New("no coordinates entered")
	}
	if len(s) > maxCoordInput {
		return 0, 0, "", errors.New("input too long to be a coordinate")
	}

	if la, lo, f, handled, e := parseCoordURL(s); handled {
		if e != nil {
			return 0, 0, "", e
		}
		return checkRange(la, lo, f)
	}

	compact := strings.Join(strings.Fields(s), "")
	if reMaidenhead.MatchString(compact) {
		la, lo, e := ParseMaidenhead(compact)
		if e != nil {
			return 0, 0, "", e
		}
		return la, lo, fmt.Sprintf("%s (%d characters, centre of square)", FmtMaidenhead, len(compact)), nil
	}
	// Plus Codes may be followed by a locality ("9C3XGV4C+XV London").
	if first := strings.Fields(s)[0]; strings.ContainsAny(first, "CFGHJMPQRVWXcfghjmpqrvwx") {
		if rePlusFull.MatchString(first) {
			la, lo, e := ParsePlusCode(first)
			if e != nil {
				return 0, 0, "", e
			}
			return la, lo, FmtPlusCode, nil
		}
		if rePlusShort.MatchString(first) {
			return 0, 0, "", errors.New("short Plus Codes (e.g. \"GV4C+XV London\") need a reference town and cannot be decoded offline; paste the full code or the coordinates")
		}
	}

	return parseGeneral(s)
}

func checkRange(lat, lon float64, format string) (float64, float64, string, error) {
	if !(lat >= -90 && lat <= 90) {
		return 0, 0, "", fmt.Errorf("latitude %.6g is out of range (-90 to 90)", lat)
	}
	if !(lon >= -180 && lon <= 180) {
		return 0, 0, "", fmt.Errorf("longitude %.6g is out of range (-180 to 180)", lon)
	}
	return lat, lon, format, nil
}

// ---------------------------------------------------------------------------
// URLs

const reNum = `([-+]?\d{1,3}(?:\.\d+)?)`

var (
	reGeoURI    = regexp.MustCompile(`(?i)^geo:\s*` + reNum + `\s*,\s*` + reNum)
	reGooglePin = regexp.MustCompile(`!3d` + reNum + `!4d` + reNum)
	reAtView    = regexp.MustCompile(`@` + reNum + `,` + reNum)
	reQueryPair = regexp.MustCompile(`(?i)[?&#/](?:q|ll|sll|query|center|daddr|destination|loc|cp|latlng|coords?|marker|point)=(?:loc:)?` +
		reNum + `\s*(?:,|~|\s)\s*` + reNum)
	reOSMMarker = regexp.MustCompile(`(?i)[?&#]mlat=` + reNum + `&mlon=` + reNum)
	reLatLonArg = regexp.MustCompile(`(?i)[?&#]lat(?:itude)?=` + reNum + `&(?:lon|lng|long|longitude)=` + reNum)
	reOSMMap    = regexp.MustCompile(`(?i)#map=\d+(?:\.\d+)?/` + reNum + `/` + reNum)

	urlUnescape = strings.NewReplacer("%2C", ",", "%2c", ",", "%20", " ", "%7E", "~", "%7e", "~", "%3A", ":", "%3a", ":")
)

// parseCoordURL extracts coordinates from geo: URIs and map URLs. handled is
// false when the input does not look like a URL at all.
func parseCoordURL(s string) (lat, lon float64, format string, handled bool, err error) {
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "geo:") {
		m := reGeoURI.FindStringSubmatch(s)
		if m == nil {
			return 0, 0, "", true, errors.New("geo: URI does not contain \"lat,lon\"")
		}
		return atof(m[1]), atof(m[2]), FmtGeoURI, true, nil
	}
	if !strings.Contains(lower, "://") && !strings.HasPrefix(lower, "www.") &&
		!strings.HasPrefix(lower, "maps.") && !strings.Contains(lower, "/maps") {
		return 0, 0, "", false, nil
	}
	u := urlUnescape.Replace(s)
	tries := []struct {
		re     *regexp.Regexp
		format string
	}{
		{reGooglePin, FmtGoogleMapsPin}, // most precise: the pin itself
		{reOSMMarker, FmtOSMURL},
		{reQueryPair, FmtMapURL},
		{reLatLonArg, FmtMapURL},
		{reAtView, FmtGoogleMapsView},
		{reOSMMap, FmtOSMURL},
	}
	for _, t := range tries {
		if m := t.re.FindStringSubmatch(u); m != nil {
			return atof(m[1]), atof(m[2]), t.format, true, nil
		}
	}
	return 0, 0, "", true, errors.New("no coordinates found in that link (shortened links such as maps.app.goo.gl cannot be resolved offline; open the link and copy the coordinates)")
}

func atof(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// ---------------------------------------------------------------------------
// Maidenhead locator

var reMaidenhead = regexp.MustCompile(`(?i)^[A-R]{2}[0-9]{2}(?:[A-X]{2}(?:[0-9]{2}(?:[A-X]{2})?)?)?$`)

// ParseMaidenhead decodes a Maidenhead (QTH) locator of 4, 6, 8 or 10
// characters (e.g. "IO91", "IO91wm", "IO91wm42") and returns the centre of
// the square. Case is ignored.
func ParseMaidenhead(locator string) (lat, lon float64, err error) {
	loc := strings.ToUpper(strings.TrimSpace(locator))
	if !reMaidenhead.MatchString(loc) {
		return 0, 0, fmt.Errorf("%q is not a valid Maidenhead locator (expected e.g. IO91, IO91wm or IO91wm42)", locator)
	}
	// Pair sizes in degrees: field 20×10, square 2×1, subsquare 2/24×1/24,
	// extended square 2/240×1/240, then another 24× subdivision.
	lonSize, latSize := 20.0, 10.0
	lon, lat = -180, -90
	for i := 0; i < len(loc); i += 2 {
		var a, b float64
		switch i / 2 {
		case 0:
			a, b = float64(loc[i]-'A'), float64(loc[i+1]-'A')
		case 1, 3:
			lonSize /= 10
			latSize /= 10
			a, b = float64(loc[i]-'0'), float64(loc[i+1]-'0')
		default: // 2, 4
			lonSize /= 24
			latSize /= 24
			a, b = float64(loc[i]-'A'), float64(loc[i+1]-'A')
		}
		lon += a * lonSize
		lat += b * latSize
	}
	return lat + latSize/2, lon + lonSize/2, nil
}

// ---------------------------------------------------------------------------
// Open Location Code ("Plus Code")

const olcAlphabet = "23456789CFGHJMPQRVWX"

var (
	rePlusFull  = regexp.MustCompile(`(?i)^[23456789CFGHJMPQRVWX]{8}\+[23456789CFGHJMPQRVWX]{2,7}$`)
	rePlusShort = regexp.MustCompile(`(?i)^[23456789CFGHJMPQRVWX]{2,6}\+[23456789CFGHJMPQRVWX]{2,3}$`)
)

// ParsePlusCode decodes a full Open Location Code such as "9C3XGV4C+XV"
// (10 to 15 digits) and returns the centre of its cell. Short codes that
// rely on a reference locality are not supported.
func ParsePlusCode(code string) (lat, lon float64, err error) {
	c := strings.ToUpper(strings.TrimSpace(code))
	if !rePlusFull.MatchString(c) {
		return 0, 0, fmt.Errorf("%q is not a full Plus Code (expected e.g. 9C3XGV4C+XV)", code)
	}
	digits := strings.Replace(c, "+", "", 1)
	latRes, lonRes := 400.0, 400.0 // multiplied down by 20 per pair → 20, 1, 0.05, …
	lat, lon = -90, -180
	for i := 0; i < 10; i += 2 {
		latRes /= 20
		lonRes /= 20
		lat += float64(strings.IndexByte(olcAlphabet, digits[i])) * latRes
		lon += float64(strings.IndexByte(olcAlphabet, digits[i+1])) * lonRes
	}
	// Grid refinement: each further digit selects a cell in a 4 (lon) × 5 (lat) grid.
	for i := 10; i < len(digits); i++ {
		latRes /= 5
		lonRes /= 4
		idx := strings.IndexByte(olcAlphabet, digits[i])
		lat += float64(idx/4) * latRes
		lon += float64(idx%4) * lonRes
	}
	lat += latRes / 2
	lon += lonRes / 2
	if lat > 90 || lon > 180 {
		return 0, 0, fmt.Errorf("Plus Code %q is outside the valid area", code)
	}
	return lat, lon, nil
}

// ---------------------------------------------------------------------------
// General tokenising parser

type tokKind uint8

const (
	tokNum tokKind = iota
	tokHemi
	tokLabel
	tokSep
)

type token struct {
	kind      tokKind
	val       float64
	neg       bool // explicit minus sign on a number
	signed    bool // explicit + or - on a number
	intDigits int  // digits before the decimal point
	frac      bool // has a fractional part
	axis      int  // 0 = latitude, 1 = longitude (hemisphere letters and labels)
	sign      float64
	text      string
}

var coordWords = map[string]token{
	"N": {kind: tokHemi, axis: 0, sign: 1}, "NORTH": {kind: tokHemi, axis: 0, sign: 1},
	"S": {kind: tokHemi, axis: 0, sign: -1}, "SOUTH": {kind: tokHemi, axis: 0, sign: -1},
	"E": {kind: tokHemi, axis: 1, sign: 1}, "EAST": {kind: tokHemi, axis: 1, sign: 1},
	"W": {kind: tokHemi, axis: 1, sign: -1}, "WEST": {kind: tokHemi, axis: 1, sign: -1},
	"LAT": {kind: tokLabel, axis: 0}, "LATITUDE": {kind: tokLabel, axis: 0},
	"LON": {kind: tokLabel, axis: 1}, "LONG": {kind: tokLabel, axis: 1},
	"LNG": {kind: tokLabel, axis: 1}, "LONGITUDE": {kind: tokLabel, axis: 1},
}

// Words that carry no information and are skipped.
var coordNoise = map[string]bool{
	"DEG": true, "DEGS": true, "DEGREE": true, "DEGREES": true,
	"MIN": true, "MINS": true, "MINUTE": true, "MINUTES": true,
	"SEC": true, "SECS": true, "SECOND": true, "SECONDS": true,
	"AND": true, "POSITION": true, "POS": true, "COORDINATES": true, "COORDS": true, "GPS": true,
}

// Two decimal-comma numbers separated by white space or a semicolon and
// containing no dots: "51,5074 -0,1278", "51,5074° N; 0,1278° W".
var reDecimalCommaPair = regexp.MustCompile(`^[^,.]*\d,\d+[^,.\d]*[\s;][^,.\d]*\d+,\d[^,.]*$`)

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isIgnorable(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '°', 'º', '˚', '\'', '’', '‘', '′', 'ʹ', '"', '”', '“', '″', '`', '´',
		':', '=', '(', ')', '[', ']', '{', '}', '_',
		0x00A0, 0x200B, 0xFEFF: // no-break space, zero-width space, BOM
		return true
	}
	return false
}

func isDash(r rune) bool {
	// hyphen-minus, minus sign U+2212, en dash, em dash, hyphen U+2010
	return r == '-' || r == 0x2212 || r == 0x2013 || r == 0x2014 || r == 0x2010
}

func tokenize(s string) ([]token, error) {
	// Decimal commas: exactly two commas, each between digits, and no dots,
	// e.g. "51,5074 -0,1278" or "51,5074; -0,1278".
	if strings.Count(s, ",") == 2 && reDecimalCommaPair.MatchString(s) {
		s = strings.ReplaceAll(s, ",", ".")
	}
	r := []rune(s)
	toks := make([]token, 0, 12)
	pendingNeg, pendingSigned := false, false
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case isIgnorable(c):
			i++
		case c == ',' || c == ';' || c == '/' || c == '|':
			toks = append(toks, token{kind: tokSep})
			pendingNeg, pendingSigned = false, false
			i++
		case isDash(c) || c == '+':
			j := i + 1
			for j < len(r) && unicode.IsSpace(r[j]) {
				j++
			}
			nextIsNum := j < len(r) && (isDigit(r[j]) || (r[j] == '.' && j+1 < len(r) && isDigit(r[j+1])))
			if !nextIsNum {
				i++ // stray dash: treat as white space
				continue
			}
			prevIsNum := len(toks) > 0 && toks[len(toks)-1].kind == tokNum
			prevAdjacent := i > 0 && (isDigit(r[i-1]) || r[i-1] == '.')
			switch {
			case c == '+':
				pendingSigned = true
			case prevIsNum && prevAdjacent:
				// "51-30-26N": dash used as a field separator
			case prevIsNum && j != i+1:
				return nil, errors.New("ambiguous \" - \" between numbers: write the minus sign directly before the number or use a comma")
			default:
				pendingNeg, pendingSigned = true, true
			}
			i++
		case isDigit(c) || (c == '.' && i+1 < len(r) && isDigit(r[i+1])):
			start := i
			for i < len(r) && isDigit(r[i]) {
				i++
			}
			intDigits := i - start
			frac := false
			if i < len(r) && r[i] == '.' {
				i++
				fs := i
				for i < len(r) && isDigit(r[i]) {
					i++
				}
				frac = i > fs
			}
			text := strings.TrimSuffix(string(r[start:i]), ".")
			v, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, fmt.Errorf("cannot read number %q", text)
			}
			toks = append(toks, token{kind: tokNum, val: v, neg: pendingNeg, signed: pendingSigned,
				intDigits: intDigits, frac: frac, text: text})
			pendingNeg, pendingSigned = false, false
		case unicode.IsLetter(c):
			start := i
			for i < len(r) && unicode.IsLetter(r[i]) {
				i++
			}
			word := strings.ToUpper(string(r[start:i]))
			if coordNoise[word] {
				continue
			}
			t, ok := coordWords[word]
			if !ok {
				return nil, fmt.Errorf("unrecognised text %q in coordinates", string(r[start:i]))
			}
			t.text = word
			toks = append(toks, t)
			pendingNeg, pendingSigned = false, false
		default:
			return nil, fmt.Errorf("unexpected character %q in coordinates", string(c))
		}
	}
	return toks, nil
}

const (
	shapeDecimal = iota + 1
	shapeDM
	shapeDMS
	shapeCompactDM
	shapeCompactDMS
)

func parseGeneral(s string) (lat, lon float64, format string, err error) {
	toks, err := tokenize(s)
	if err != nil {
		return 0, 0, "", err
	}
	var nHemi, nLabel, nNum int
	for _, t := range toks {
		switch t.kind {
		case tokHemi:
			nHemi++
		case tokLabel:
			nLabel++
		case tokNum:
			nNum++
		}
	}
	if nNum == 0 {
		return 0, 0, "", errors.New("no numbers found in coordinates")
	}

	var groups [2][]token // by axis
	hemi := nHemi > 0
	var hemiSign [2]float64

	switch {
	case nHemi > 0 || nLabel > 0:
		markerKind := tokLabel
		what := "\"lat\"/\"lon\" label"
		if hemi {
			markerKind = tokHemi
			what = "hemisphere letter (N/S and E/W)"
		}
		// Keep numbers and the chosen marker kind only.
		seq := make([]token, 0, len(toks))
		for _, t := range toks {
			if t.kind == tokNum || t.kind == markerKind {
				seq = append(seq, t)
			}
		}
		var mi []int
		for i, t := range seq {
			if t.kind == markerKind {
				mi = append(mi, i)
			}
		}
		if len(mi) != 2 {
			return 0, 0, "", fmt.Errorf("expected exactly one %s for each of latitude and longitude", what)
		}
		m0, m1 := seq[mi[0]], seq[mi[1]]
		if m0.axis == m1.axis {
			return 0, 0, "", fmt.Errorf("both values are marked as %s", [2]string{"latitude", "longitude"}[m0.axis])
		}
		var g0, g1 []token
		switch {
		case mi[0] == 0: // prefix form: N 51 30.4 W 0 7.6
			g0, g1 = seq[1:mi[1]], seq[mi[1]+1:]
		case mi[1] == len(seq)-1: // suffix form: 51 30 26 N 0 7 39 W
			g0, g1 = seq[:mi[0]], seq[mi[0]+1:mi[1]]
		default:
			return 0, 0, "", fmt.Errorf("cannot tell which numbers belong to which %s", what)
		}
		groups[m0.axis], groups[m1.axis] = g0, g1
		hemiSign[m0.axis], hemiSign[m1.axis] = m0.sign, m1.sign

	default:
		// No markers: latitude first. Split on separators when they divide
		// the numbers into exactly two groups, otherwise split evenly.
		var parts [][]token
		cur := []token{}
		nSep := 0
		for _, t := range toks {
			if t.kind == tokSep {
				nSep++
				parts = append(parts, cur)
				cur = []token{}
				continue
			}
			cur = append(cur, t)
		}
		parts = append(parts, cur)
		nonEmpty := parts[:0:0]
		for _, p := range parts {
			if len(p) > 0 {
				nonEmpty = append(nonEmpty, p)
			}
		}
		switch {
		case len(nonEmpty) == 2 && nSep >= 1:
			groups[0], groups[1] = nonEmpty[0], nonEmpty[1]
		case len(nonEmpty) == 1 || len(nonEmpty) == nNum:
			flat := make([]token, 0, nNum)
			for _, p := range nonEmpty {
				flat = append(flat, p...)
			}
			if nNum != 2 && nNum != 4 && nNum != 6 {
				return 0, 0, "", fmt.Errorf("found %d numbers; expected latitude and longitude as 2 (decimal), 4 (deg min) or 6 (deg min sec) numbers", nNum)
			}
			groups[0], groups[1] = flat[:nNum/2], flat[nNum/2:]
		default:
			return 0, 0, "", errors.New("cannot split the input into a latitude and a longitude")
		}
	}

	var val [2]float64
	var shape [2]int
	for axis := 0; axis < 2; axis++ {
		v, sh, e := groupValue(groups[axis], axis, hemi)
		if e != nil {
			return 0, 0, "", e
		}
		if hemi {
			v *= hemiSign[axis]
		}
		val[axis], shape[axis] = v, sh
	}

	format = shapeName(shape[0], hemi)
	if shape[1] != shape[0] {
		format += " / " + shapeName(shape[1], hemi)
	}
	return checkRange(val[0], val[1], format)
}

func shapeName(shape int, hemi bool) string {
	switch shape {
	case shapeDM:
		return FmtDDM
	case shapeDMS:
		return FmtDMS
	case shapeCompactDM:
		return FmtCompactDM
	case shapeCompactDMS:
		return FmtCompactDMS
	}
	if hemi {
		return FmtDecimalHemi
	}
	return FmtDecimal
}

// groupValue converts the 1–3 numbers of one coordinate into degrees.
func groupValue(g []token, axis int, hemi bool) (float64, int, error) {
	name := [2]string{"latitude", "longitude"}[axis]
	if len(g) == 0 {
		return 0, 0, fmt.Errorf("%s is missing", name)
	}
	if len(g) > 3 {
		return 0, 0, fmt.Errorf("too many numbers for the %s (at most degrees, minutes, seconds)", name)
	}
	for i, t := range g {
		if t.kind != tokNum {
			return 0, 0, fmt.Errorf("unexpected %q inside the %s", t.text, name)
		}
		if i > 0 && t.signed {
			return 0, 0, fmt.Errorf("unexpected sign inside the %s", name)
		}
	}
	neg := g[0].neg
	if neg && hemi {
		return 0, 0, fmt.Errorf("%s has both a minus sign and a hemisphere letter", name)
	}
	var v float64
	shape := shapeDecimal
	switch len(g) {
	case 1:
		v = g[0].val
		if hemi && !g[0].signed {
			// Compact forms: DDMM[.m] / DDDMM[.m] and DDMMSS[.s] / DDDMMSS[.s].
			switch g[0].intDigits {
			case 4 + axis:
				d := float64(int(v / 100))
				m := v - d*100
				if m >= 60 {
					return 0, 0, fmt.Errorf("%s minutes must be below 60", name)
				}
				v, shape = d+m/60, shapeCompactDM
			case 6 + axis:
				d := float64(int(v / 10000))
				m := float64(int((v - d*10000) / 100))
				sec := v - d*10000 - m*100
				if m >= 60 || sec >= 60 {
					return 0, 0, fmt.Errorf("%s minutes and seconds must be below 60", name)
				}
				v, shape = d+m/60+sec/3600, shapeCompactDMS
			}
		}
	case 2:
		if g[0].frac {
			return 0, 0, fmt.Errorf("%s degrees must be a whole number when minutes are given", name)
		}
		if g[1].val >= 60 {
			return 0, 0, fmt.Errorf("%s minutes must be below 60", name)
		}
		v, shape = g[0].val+g[1].val/60, shapeDM
	case 3:
		if g[0].frac || g[1].frac {
			return 0, 0, fmt.Errorf("%s degrees and minutes must be whole numbers when seconds are given", name)
		}
		if g[1].val >= 60 || g[2].val >= 60 {
			return 0, 0, fmt.Errorf("%s minutes and seconds must be below 60", name)
		}
		v, shape = g[0].val+g[1].val/60+g[2].val/3600, shapeDMS
	}
	if neg {
		v = -v
	}
	return v, shape, nil
}
