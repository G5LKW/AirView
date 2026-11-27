package predict

import (
	"math"
	"time"
)

// Pressure trends for Zambretti.
const (
	TrendFalling = "falling"
	TrendSteady  = "steady"
	TrendRising  = "rising"
)

// zambrettiTrendHPa is the 3-hour pressure change that counts as rising /
// falling for the Zambretti forecaster (the conventional 1.6 hPa).
const zambrettiTrendHPa = 1.6

// The 26 forecasts of the Negretti & Zambra forecaster, letters A..Z.
var zambrettiText = [26]string{
	"Settled fine", "Fine weather", "Becoming fine", "Fine, becoming less settled",
	"Fine, possible showers", "Fairly fine, improving", "Fairly fine, possible showers early",
	"Fairly fine, showery later", "Showery early, improving", "Changeable, mending",
	"Fairly fine, showers likely", "Rather unsettled, clearing later", "Unsettled, probably improving",
	"Showery, bright intervals", "Showery, becoming less settled", "Changeable, some rain",
	"Unsettled, short fine intervals", "Unsettled, rain later", "Unsettled, some rain",
	"Mostly very unsettled", "Occasional rain, worsening", "Rain at times, very unsettled",
	"Rain at frequent intervals", "Rain, very unsettled", "Stormy, may improve", "Stormy, much rain",
}

// Forecast number per pressure step (22 steps from 950 to 1050 hPa) for a
// rising, steady and falling barometer: the scales of the instrument.
var (
	zRise   = [22]int{25, 25, 25, 24, 24, 19, 16, 12, 11, 9, 8, 6, 5, 2, 1, 1, 0, 0, 0, 0, 0, 0}
	zSteady = [22]int{25, 25, 25, 25, 25, 25, 23, 23, 22, 18, 15, 13, 10, 4, 1, 1, 0, 0, 0, 0, 0, 0}
	zFall   = [22]int{25, 25, 25, 25, 25, 25, 25, 25, 23, 23, 21, 20, 17, 14, 7, 3, 1, 1, 1, 0, 0, 0}
)

// Wind corrections in percent of the 100 hPa scale for the 16 compass
// points N, NNE, ... NNW (northern hemisphere).
var zWind = [16]float64{6, 5, 5, 2, -0.5, -2, -5, -8.5, -12, -10, -6, -4.5, -3, -0.5, 1.5, 3}

var compass16 = [16]string{"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"}

// CompassPoint returns the 16-point compass name of a direction in degrees.
func CompassPoint(deg float64) string {
	if !fin(deg) {
		return ""
	}
	return compass16[sector16(deg)]
}

func sector16(deg float64) int {
	d := math.Mod(deg, 360)
	if d < 0 {
		d += 360
	}
	return int(math.Floor(d/22.5+0.5)) % 16
}

// ZambrettiInput is the state of the barometer.
type ZambrettiInput struct {
	PressureMSL float64 // hPa, reduced to mean sea level
	// Trend is TrendRising / TrendSteady / TrendFalling; when empty it is
	// derived from Change3h (+-1.6 hPa).
	Trend    string
	Change3h float64 // hPa per 3 h (used when Trend is empty)
	// WindDir is the direction the wind blows FROM in degrees; NaN = calm /
	// unknown (no wind correction).
	WindDir  float64
	Month    time.Month // calendar month, for the season
	Southern bool       // station in the southern hemisphere
}

// ZambrettiResult is the forecast of the Zambretti forecaster.
type ZambrettiResult struct {
	Number      int    `json:"number"` // 0..25
	Letter      string `json:"letter"` // "A".."Z"
	Text        string `json:"text"`
	Trend       string `json:"trend"`
	Exceptional bool   `json:"exceptional"` // adjusted pressure outside the 950-1050 hPa scale
}

// Zambretti implements the Negretti & Zambra "Zambretti forecaster" (1915)
// in its widely used digitised form: the sea-level pressure is corrected
// for wind direction (mirrored in the southern hemisphere) and, in summer,
// for the trend (+7 % of the scale when rising, -7 % when falling), then
// looked up on the rising / steady / falling scale of 22 steps between 950
// and 1050 hPa. Summer is April-September in the northern hemisphere and
// October-March in the southern. ok is false when the pressure is missing.
func Zambretti(in ZambrettiInput) (res ZambrettiResult, ok bool) {
	p := in.PressureMSL
	if !fin(p) || p < 850 || p > 1100 {
		return res, false
	}
	trend := in.Trend
	if trend == "" {
		switch {
		case in.Change3h >= zambrettiTrendHPa:
			trend = TrendRising
		case in.Change3h <= -zambrettiTrendHPa:
			trend = TrendFalling
		default:
			trend = TrendSteady
		}
	}
	const lo, hi = 950.0, 1050.0
	rng := hi - lo
	if fin(in.WindDir) {
		s := sector16(in.WindDir)
		if in.Southern {
			s = (s + 8) % 16
		}
		p += zWind[s] / 100 * rng
	}
	summer := in.Month >= time.April && in.Month <= time.September
	if in.Southern {
		summer = !summer
	}
	if summer {
		switch trend {
		case TrendRising:
			p += 7.0 / 100 * rng
		case TrendFalling:
			p -= 7.0 / 100 * rng
		}
	}
	if p == hi {
		p = hi - 1
	}
	// The step width is rounded to three decimals as in the reference
	// implementation (4.545 hPa), which also keeps exact multiples such as
	// 1000 hPa on the upper step.
	step := math.Round(rng/22*1000) / 1000
	opt := int(math.Floor((p - lo) / step))
	if opt < 0 {
		opt, res.Exceptional = 0, true
	}
	if opt > 21 {
		opt, res.Exceptional = 21, true
	}
	switch trend {
	case TrendRising:
		res.Number = zRise[opt]
	case TrendFalling:
		res.Number = zFall[opt]
	default:
		trend = TrendSteady
		res.Number = zSteady[opt]
	}
	res.Trend = trend
	res.Letter = string(rune('A' + res.Number))
	res.Text = zambrettiText[res.Number]
	return res, true
}

// Pressure tendency classes (per 3 hours).
const (
	TendencySteady          = "steady"
	TendencyRisingSlowly    = "rising slowly"
	TendencyRising          = "rising"
	TendencyRisingRapidly   = "rising rapidly"
	TendencyRisingVRapidly  = "rising very rapidly"
	TendencyFallingSlowly   = "falling slowly"
	TendencyFalling         = "falling"
	TendencyFallingRapidly  = "falling rapidly"
	TendencyFallingVRapidly = "falling very rapidly"
)

// ClassifyTendency names a 3-hour pressure change in hPa using the
// conventional marine thresholds: under 0.5 steady, to 1.6 slowly, to 3.5
// rising/falling, to 6 rapidly, beyond that very rapidly.
func ClassifyTendency(change3h float64) string {
	a := math.Abs(change3h)
	up := change3h > 0
	pick := func(r, f string) string {
		if up {
			return r
		}
		return f
	}
	switch {
	case !fin(change3h) || a < 0.5:
		return TendencySteady
	case a < 1.6:
		return pick(TendencyRisingSlowly, TendencyFallingSlowly)
	case a < 3.5:
		return pick(TendencyRising, TendencyFalling)
	case a < 6:
		return pick(TendencyRisingRapidly, TendencyFallingRapidly)
	default:
		return pick(TendencyRisingVRapidly, TendencyFallingVRapidly)
	}
}
