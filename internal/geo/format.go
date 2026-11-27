package geo

import (
	"fmt"
	"math"
	"strings"
)

// FormatDecimal formats a position as signed decimal degrees,
// e.g. FormatDecimal(51.5074, -0.1278, 5) = "51.50740, -0.12780".
func FormatDecimal(lat, lon float64, decimals int) string {
	if decimals < 0 {
		decimals = 0
	}
	return fmt.Sprintf("%.*f, %.*f", decimals, lat, decimals, lon)
}

// FormatDMS formats a position as degrees, minutes and seconds with
// hemisphere letters, e.g. FormatDMS(51.5074, -0.1278, 1) =
// `51°30'26.6"N 0°07'40.1"W`. secDecimals is the number of decimals on the
// seconds (0..6). The output parses back with ParseLatLon. A non-finite
// coordinate is printed as "?".
func FormatDMS(lat, lon float64, secDecimals int) string {
	return dmsOne(lat, 'N', 'S', secDecimals) + " " + dmsOne(lon, 'E', 'W', secDecimals)
}

func dmsOne(v float64, pos, neg byte, secDecimals int) string {
	if v-v != 0 { // NaN or ±Inf
		return "?"
	}
	if secDecimals < 0 {
		secDecimals = 0
	} else if secDecimals > 6 {
		secDecimals = 6
	}
	h := pos
	if v < 0 || (v == 0 && math.Signbit(v)) {
		h = neg
		v = -v
	}
	scale := int64(math.Pow10(secDecimals))
	units := int64(math.Round(v * 3600 * float64(scale))) // carries rounding into minutes/degrees
	perDeg := 3600 * scale
	deg := units / perDeg
	rem := units % perDeg
	min := rem / (60 * scale)
	sec := float64(rem%(60*scale)) / float64(scale)
	width := 2
	if secDecimals > 0 {
		width = 3 + secDecimals
	}
	return fmt.Sprintf("%d°%02d'%0*.*f\"%c", deg, min, width, secDecimals, sec, h)
}

// FormatDDM formats a position as degrees and decimal minutes in the
// GPS/aviation style, e.g. FormatDDM(51.5074, -0.1278, 3) =
// "N51 30.444 W000 07.668". The output parses back with ParseLatLon.
func FormatDDM(lat, lon float64, minDecimals int) string {
	return ddmOne(lat, 'N', 'S', 2, minDecimals) + " " + ddmOne(lon, 'E', 'W', 3, minDecimals)
}

func ddmOne(v float64, pos, neg byte, degWidth, minDecimals int) string {
	if v-v != 0 { // NaN or ±Inf
		return "?"
	}
	if minDecimals < 0 {
		minDecimals = 0
	} else if minDecimals > 8 {
		minDecimals = 8
	}
	h := pos
	if v < 0 || (v == 0 && math.Signbit(v)) {
		h = neg
		v = -v
	}
	scale := int64(math.Pow10(minDecimals))
	units := int64(math.Round(v * 60 * float64(scale)))
	deg := units / (60 * scale)
	min := float64(units%(60*scale)) / float64(scale)
	width := 2
	if minDecimals > 0 {
		width = 3 + minDecimals
	}
	return fmt.Sprintf("%c%0*d %0*.*f", h, degWidth, deg, width, minDecimals, min)
}

// Maidenhead returns the Maidenhead (QTH) locator of a position with the
// given number of characters: 2, 4, 6, 8 or 10 (other values are rounded
// down to the nearest valid length, minimum 2). Following convention the
// field is upper case, sub-squares lower case: Maidenhead(51.5074, -0.1278, 6)
// = "IO91wm". Non-finite coordinates give "".
func Maidenhead(lat, lon float64, chars int) string {
	if lat-lat != 0 || lon-lon != 0 { // NaN or ±Inf
		return ""
	}
	pairs := chars / 2
	if pairs < 1 {
		pairs = 1
	} else if pairs > 5 {
		pairs = 5
	}
	// Work in fractions of the whole grid to keep the arithmetic simple.
	x := (NormalizeLon(lon) + 180) / 360
	y := (ClampLat(lat) + 90) / 180
	if y >= 1 {
		y = math.Nextafter(1, 0)
	}
	if x >= 1 {
		x = math.Nextafter(1, 0)
	}
	var sb strings.Builder
	sb.Grow(pairs * 2)
	for p := 0; p < pairs; p++ {
		var div float64
		var base byte
		switch p {
		case 0:
			div, base = 18, 'A'
		case 1, 3:
			div, base = 10, '0'
		default:
			div, base = 24, 'a'
		}
		x *= div
		y *= div
		xi, yi := math.Floor(x), math.Floor(y)
		if xi >= div {
			xi = div - 1
		}
		if yi >= div {
			yi = div - 1
		}
		x -= xi
		y -= yi
		sb.WriteByte(base + byte(xi))
		sb.WriteByte(base + byte(yi))
	}
	return sb.String()
}
