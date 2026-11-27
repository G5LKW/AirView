package predict

import (
	"math"

	"airview/internal/weather"
)

// Num is a float64 that encodes NaN / Inf as JSON null (weather.Num). Every
// optional number in the result types is a Num, so a result can always be
// handed to encoding/json.
type Num = weather.Num

// Confidence levels used by Meta.Confidence.
const (
	ConfLow    = "low"
	ConfMedium = "medium"
	ConfHigh   = "high"
)

// Model levels: how much of a product is learned from the station's own
// history (Meta.Level).
const (
	LevelNone    = "none"    // no usable data, nothing to show
	LevelPhysics = "physics" // physics / climatology only
	LevelBlend   = "blend"   // physics blended with a young learned model
	LevelLearned = "learned" // dominated by the learned model
)

// Est is one estimate: the expected value, an 80 % interval and a
// confidence between 0 and 1.
type Est struct {
	Value      Num     `json:"value"`
	Lo         Num     `json:"lo"`
	Hi         Num     `json:"hi"`
	Confidence float64 `json:"confidence"`
}

// Point is an Est at a time (Unix ms).
type Point struct {
	T int64 `json:"t"`
	Est
}

// Skill is the outcome of a rolling-origin backtest of one model against
// the naive baselines. Errors are RMSE unless Metric says otherwise; the
// Vs* scores are 1 - error(model)/error(baseline): positive = better than
// the baseline, 0 = no better, negative = worse.
type Skill struct {
	Metric          string `json:"metric"`          // "rmse" | "brier"
	Unit            string `json:"unit"`            // unit of the errors
	Model           Num    `json:"model"`           // error of the model
	Persistence     Num    `json:"persistence"`     // error of the persistence forecast
	SeasonalNaive   Num    `json:"seasonalNaive"`   // error of the seasonal-naive forecast
	VsPersistence   Num    `json:"vsPersistence"`   // skill score against persistence
	VsSeasonalNaive Num    `json:"vsSeasonalNaive"` // skill score against seasonal-naive
	N               int    `json:"n"`               // forecast points scored
	Origins         int    `json:"origins"`         // forecast origins
	HorizonH        int    `json:"horizonH"`        // forecast horizon of the backtest, hours
	Note            string `json:"note,omitempty"`
}

// Meta is the header shared by all products.
type Meta struct {
	Product   string `json:"product"`
	Generated int64  `json:"generated"` // Unix ms
	// Method is a short description of how the numbers were produced.
	Method string `json:"method"`
	// Level is LevelNone | LevelPhysics | LevelBlend | LevelLearned.
	Level string `json:"level"`
	// Confidence is ConfLow | ConfMedium | ConfHigh; ConfidenceValue the
	// underlying 0..1 figure.
	Confidence      string  `json:"confidence"`
	ConfidenceValue float64 `json:"confidenceValue"`
	// Skill is the backtest result, nil while there is too little history.
	Skill *Skill `json:"skill,omitempty"`
	// DataHours is the number of history hours the product could use.
	DataHours int      `json:"dataHours"`
	Notes     []string `json:"notes,omitempty"`
}

func (m *Meta) setConfidence(v float64) {
	v = clamp01(v)
	m.ConfidenceValue = round(v, 2)
	switch {
	case v >= 0.66:
		m.Confidence = ConfHigh
	case v >= 0.4:
		m.Confidence = ConfMedium
	default:
		m.Confidence = ConfLow
	}
}

// ---------------------------------------------------------------- helpers

const z80 = 1.2816 // half-width of an 80 % normal interval in sigmas

var nan = math.NaN()

func fin(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clamp01 clamps to [0, 1]; NaN gives 0.
func clamp01(v float64) float64 {
	if !(v > 0) {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// round rounds v to d decimals (NaN / Inf pass through).
func round(v float64, d int) float64 {
	if !fin(v) {
		return v
	}
	p := math.Pow(10, float64(d))
	return math.Round(v*p) / p
}

func rn(v float64, d int) Num { return Num(round(v, d)) }

// or returns v, or def when v is not finite.
func or(v, def float64) float64 {
	if fin(v) {
		return v
	}
	return def
}

func est(v, sd, conf float64, d int, lo0 bool) Est {
	e := Est{Value: rn(v, d), Lo: Num(nan), Hi: Num(nan), Confidence: round(clamp01(conf), 2)}
	if fin(v) && fin(sd) {
		lo, hi := v-z80*sd, v+z80*sd
		if lo0 && lo < 0 {
			lo = 0
		}
		e.Lo, e.Hi = rn(lo, d), rn(hi, d)
	}
	if !fin(v) {
		e.Confidence = 0
	}
	return e
}

// finiteVals returns the finite values of x.
func finiteVals(x []float64) []float64 {
	out := make([]float64, 0, len(x))
	for _, v := range x {
		if fin(v) {
			out = append(out, v)
		}
	}
	return out
}

func lastFinite(x []float64) float64 {
	for i := len(x) - 1; i >= 0; i-- {
		if fin(x[i]) {
			return x[i]
		}
	}
	return nan
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return itoa(n) + " " + unit + "s"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
