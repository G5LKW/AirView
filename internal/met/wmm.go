package met

import (
	"math"
	"time"
)

// Geomagnetic reference radius of the WMM (km) and the WGS84 ellipsoid.
const (
	wmmRefRadiusKm = 6371.2
	wgs84AKm       = 6378.137
	wgs84F         = 1 / 298.257223563
	wgs84E2        = wgs84F * (2 - wgs84F)

	// The model is published for 2025.0 - 2030.0. Outside that range the
	// linear secular variation is extrapolated, limited to this many years
	// from the epoch so a wrong clock can never produce a wild field.
	wmmMaxExtrapolationYears = 15.0
)

// Recursion constants of the Schmidt semi-normalised associated Legendre
// functions, computed once:
//
//	P(n,n) = wmmDiag[n] * cos(phi) * P(n-1,n-1)
//	P(n,m) = (wmmK1[n][m] * sin(phi) * P(n-1,m) - wmmK2[n][m] * P(n-2,m))
var (
	wmmDiag [wmmMaxDeg + 1]float64
	wmmK1   [wmmMaxDeg + 1][wmmMaxDeg + 1]float64
	wmmK2   [wmmMaxDeg + 1][wmmMaxDeg + 1]float64
	// Coefficients indexed [n][m] for the synthesis loop.
	wmmG, wmmH, wmmGDot, wmmHDot [wmmMaxDeg + 1][wmmMaxDeg + 1]float64
)

func init() {
	for n := 1; n <= wmmMaxDeg; n++ {
		if n == 1 {
			wmmDiag[n] = 1
		} else {
			wmmDiag[n] = math.Sqrt(1 - 1/(2*float64(n)))
		}
		for m := 0; m < n; m++ {
			d := math.Sqrt(float64(n*n - m*m))
			wmmK1[n][m] = float64(2*n-1) / d
			wmmK2[n][m] = math.Sqrt(float64((n-1)*(n-1)-m*m)) / d
		}
	}
	for _, c := range wmm2025 {
		wmmG[c.n][c.m], wmmH[c.n][c.m] = c.g, c.h
		wmmGDot[c.n][c.m], wmmHDot[c.n][c.m] = c.gDot, c.hDot
	}
}

// Field is the geomagnetic main field at one point, in the local geodetic
// north-east-down frame.
type Field struct {
	X float64 `json:"x"` // north component, nT
	Y float64 `json:"y"` // east component, nT
	Z float64 `json:"z"` // down component, nT
	H float64 `json:"h"` // horizontal intensity, nT
	F float64 `json:"f"` // total intensity, nT
	D float64 `json:"d"` // declination, degrees east of true north
	I float64 `json:"i"` // inclination, degrees below the horizontal
}

// DecimalYear converts a time to a decimal year (2025.5 = middle of 2025).
func DecimalYear(t time.Time) float64 {
	t = t.UTC()
	y := t.Year()
	start := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC)
	return float64(y) + float64(t.Sub(start))/float64(end.Sub(start))
}

// MagField evaluates the World Magnetic Model 2025 (degree and order 12,
// with linear secular variation) at a geodetic latitude / longitude in
// degrees, a height above the WGS84 ellipsoid in km and a decimal year.
// ok is false for non-finite or out-of-range coordinates.
//
// Accuracy: the function reproduces the official WMM2025 test values to
// better than 0.01 degrees in declination and 0.1 nT in the components. The
// model itself describes only the core field: the real declination differs
// by typically 0.3 - 0.5 degrees (crustal and external fields).
func MagField(latDeg, lonDeg, altKm, year float64) (f Field, ok bool) {
	if !finite(latDeg) || !finite(lonDeg) || latDeg < -90 || latDeg > 90 || !finite(year) {
		return Field{}, false
	}
	if !finite(altKm) {
		altKm = 0
	}
	// The horizontal components are undefined exactly at the poles.
	const poleLimit = 89.9999
	if latDeg > poleLimit {
		latDeg = poleLimit
	} else if latDeg < -poleLimit {
		latDeg = -poleLimit
	}
	dt := year - wmmEpoch
	if dt > wmmMaxExtrapolationYears {
		dt = wmmMaxExtrapolationYears
	} else if dt < -wmmMaxExtrapolationYears {
		dt = -wmmMaxExtrapolationYears
	}

	// Geodetic -> geocentric spherical coordinates.
	phi := latDeg * math.Pi / 180
	lam := lonDeg * math.Pi / 180
	sphi, cphi := math.Sincos(phi)
	rc := wgs84AKm / math.Sqrt(1-wgs84E2*sphi*sphi)
	p := (rc + altKm) * cphi
	z := (rc*(1-wgs84E2) + altKm) * sphi
	r := math.Hypot(p, z)
	phiC := math.Asin(z / r)
	st, ct := math.Sincos(phiC) // sin / cos of the geocentric latitude

	var sinM, cosM [wmmMaxDeg + 1]float64
	sl, cl := math.Sincos(lam)
	cosM[0] = 1
	for m := 1; m <= wmmMaxDeg; m++ {
		sinM[m] = sinM[m-1]*cl + cosM[m-1]*sl
		cosM[m] = cosM[m-1]*cl - sinM[m-1]*sl
	}

	// Legendre functions P(n,m)(sin phi') and their derivatives with
	// respect to phi', column by column (fixed m), two rows of history.
	var pnm, dpnm [wmmMaxDeg + 1][wmmMaxDeg + 1]float64
	pnm[0][0] = 1
	for n := 1; n <= wmmMaxDeg; n++ {
		pnm[n][n] = wmmDiag[n] * ct * pnm[n-1][n-1]
		dpnm[n][n] = wmmDiag[n] * (ct*dpnm[n-1][n-1] - st*pnm[n-1][n-1])
		for m := 0; m < n; m++ {
			var p2, dp2 float64
			if n >= 2 {
				p2, dp2 = pnm[n-2][m], dpnm[n-2][m]
			}
			pnm[n][m] = wmmK1[n][m]*st*pnm[n-1][m] - wmmK2[n][m]*p2
			dpnm[n][m] = wmmK1[n][m]*(st*dpnm[n-1][m]+ct*pnm[n-1][m]) - wmmK2[n][m]*dp2
		}
	}

	var bx, by, bz float64 // geocentric north, east, down
	ar := wmmRefRadiusKm / r
	rn := ar * ar // (a/r)^(n+2), starting at n = 1 below
	for n := 1; n <= wmmMaxDeg; n++ {
		rn *= ar
		var sx, sy, sz float64
		for m := 0; m <= n; m++ {
			g := wmmG[n][m] + dt*wmmGDot[n][m]
			h := wmmH[n][m] + dt*wmmHDot[n][m]
			a := g*cosM[m] + h*sinM[m]
			sx += a * dpnm[n][m]
			sy += float64(m) * (g*sinM[m] - h*cosM[m]) * pnm[n][m]
			sz += a * pnm[n][m]
		}
		bx -= rn * sx
		by += rn * sy
		bz -= float64(n+1) * rn * sz
	}
	by /= ct

	// Rotate from the geocentric to the geodetic frame.
	sd, cd := math.Sincos(phiC - phi)
	f.X = bx*cd - bz*sd
	f.Y = by
	f.Z = bx*sd + bz*cd
	f.H = math.Hypot(f.X, f.Y)
	f.F = math.Hypot(f.H, f.Z)
	f.D = math.Atan2(f.Y, f.X) * 180 / math.Pi
	f.I = math.Atan2(f.Z, f.H) * 180 / math.Pi
	return f, true
}

// MagVar returns the magnetic declination (variation) in degrees, positive
// east: true bearing = magnetic bearing + MagVar. Latitude and longitude are
// geodetic degrees, altKm the height above the ellipsoid in km (the
// difference to mean sea level is irrelevant for declination) and t the
// date. It returns NaN - never a silent zero - when the position is unknown
// or invalid. A zero time means "now".
func MagVar(lat, lon, altKm float64, t time.Time) float64 {
	if t.IsZero() {
		t = time.Now()
	}
	f, ok := MagField(lat, lon, altKm, DecimalYear(t))
	if !ok {
		return math.NaN()
	}
	return f.D
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
