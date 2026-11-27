package predict

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"airview/internal/astro"
	"airview/internal/stat"
	"airview/internal/store"
	"airview/internal/weather"
)

// ------------------------------------------------------------ bias learning

// Variables with a learned forecast bias.
const (
	bvTemp = iota
	bvDew
	bvRH
	bvPmsl
	bvWind
	bvGust
	bvCloud
	nBiasVar
)

var biasVarNames = [nBiasVar]string{"tempC", "dewPointC", "rh", "pressureMsl", "windKmh", "gustKmh", "cloud"}

// leadBuckets are the lead-time classes (hours, inclusive) of the bias table.
var leadBuckets = [...][2]int{{1, 6}, {7, 12}, {13, 24}, {25, 48}, {49, 72}, {73, 168}}

const (
	biasMinPairs = 24
	biasLogDays  = 45
)

func leadBucket(lead int) int {
	for i, b := range leadBuckets {
		if lead >= b[0] && lead <= b[1] {
			return i
		}
	}
	return -1
}

// BiasRow is the verification of the Open-Meteo forecast against the
// station's observation record for one variable and lead-time class.
type BiasRow struct {
	Variable string `json:"variable"`
	LeadFrom int    `json:"leadFrom"` // hours
	LeadTo   int    `json:"leadTo"`
	N        int    `json:"n"`    // forecast/observation pairs
	Bias     Num    `json:"bias"` // mean of forecast - observation
	MAE      Num    `json:"mae"`  // mean absolute error of the raw forecast
	// MAECorrected is the error after subtracting the bias, measured on the
	// newest 30 % of the pairs with a bias learned from the older 70 %.
	MAECorrected Num `json:"maeCorrected"`
	MAEHoldout   Num `json:"maeHoldout"` // raw forecast on the same 30 %
	// Applied: the correction verifiably helps and is applied to the outlook.
	Applied bool `json:"applied"`
}

type omKey struct {
	t    int64
	lead int
}

// fcLogStats is what one streaming pass over weather_fc_log yields.
type fcLogStats struct {
	bias []BiasRow
	// om holds the logged Open-Meteo forecast (temp, pressure, wind) of the
	// leads the analogue verification compares against.
	om map[omKey][3]float64
}

var analogueLeads = [...]int{6, 12, 24}

// scanForecastLog joins weather_fc_log with weather_obs for the last
// biasLogDays days and accumulates bias statistics without keeping the rows.
func (f *Forecaster) scanForecastLog(ctx context.Context, cur int64) (*fcLogStats, error) {
	from := cur - biasLogDays*daySec
	// Split point of the hold-out: pairs are ordered by target time, the
	// newest 30 % of the period verify the bias learned from the rest.
	var tMin, tMax sql.NullInt64
	if err := f.st.DB().QueryRowContext(ctx,
		`SELECT MIN(t), MAX(t) FROM weather_fc_log WHERE t >= ? AND t <= ? AND lead_h >= 1`, from, cur).Scan(&tMin, &tMax); err != nil {
		return nil, err
	}
	out := &fcLogStats{om: map[omKey][3]float64{}}
	if !tMin.Valid || !tMax.Valid {
		return out, nil
	}
	split := tMin.Int64 + (tMax.Int64-tMin.Int64)*7/10

	type acc struct {
		n, nTrain, nTest          int
		sum, abs                  float64
		sumTrain                  float64
		absTestRaw, absTestCorr   float64
		trainBias                 float64
		trainBiasSet, trainUsable bool
	}
	var a [nBiasVar][len(leadBuckets)]acc

	rows, err := f.st.DB().QueryContext(ctx, `SELECT f.t, f.lead_h,
  f.temp_c, f.dewpoint_c, f.rh, f.pressure_msl, f.wind_kmh, f.gust_kmh, f.cloud,
  o.temp_c, o.dewpoint_c, o.rh, o.pressure_msl, o.wind_kmh, o.gust_kmh, o.cloud
FROM weather_fc_log f JOIN weather_obs o ON o.t = f.t
WHERE f.t >= ? AND f.t <= ? AND f.lead_h >= 1 ORDER BY f.t, f.lead_h`, from, cur)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var t int64
	var lead int
	var fv, ov [nBiasVar]sql.NullFloat64
	dest := []any{&t, &lead}
	for i := range fv {
		dest = append(dest, &fv[i])
	}
	for i := range ov {
		dest = append(dest, &ov[i])
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for _, l := range analogueLeads {
			if lead == l {
				v := [3]float64{nan, nan, nan}
				if fv[bvTemp].Valid {
					v[0] = fv[bvTemp].Float64
				}
				if fv[bvPmsl].Valid {
					v[1] = fv[bvPmsl].Float64
				}
				if fv[bvWind].Valid {
					v[2] = fv[bvWind].Float64
				}
				out.om[omKey{t, lead}] = v
			}
		}
		b := leadBucket(lead)
		if b < 0 {
			continue
		}
		for v := 0; v < nBiasVar; v++ {
			if !fv[v].Valid || !ov[v].Valid {
				continue
			}
			e := fv[v].Float64 - ov[v].Float64
			if !fin(e) {
				continue
			}
			c := &a[v][b]
			c.n++
			c.sum += e
			c.abs += math.Abs(e)
			if t < split {
				c.nTrain++
				c.sumTrain += e
				continue
			}
			if !c.trainBiasSet {
				// Rows arrive ordered by target time: the training part of
				// this cell is complete.
				c.trainBiasSet = true
				if c.nTrain >= biasMinPairs/2 {
					c.trainBias, c.trainUsable = c.sumTrain/float64(c.nTrain), true
				}
			}
			c.nTest++
			c.absTestRaw += math.Abs(e)
			c.absTestCorr += math.Abs(e - c.trainBias)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for v := 0; v < nBiasVar; v++ {
		for b := range leadBuckets {
			c := &a[v][b]
			if c.n == 0 {
				continue
			}
			n := float64(c.n)
			r := BiasRow{
				Variable: biasVarNames[v], LeadFrom: leadBuckets[b][0], LeadTo: leadBuckets[b][1], N: c.n,
				Bias: rn(c.sum/n, 3), MAE: rn(c.abs/n, 3), MAECorrected: Num(nan), MAEHoldout: Num(nan),
			}
			if c.nTest >= 6 && c.trainUsable {
				raw, corr := c.absTestRaw/float64(c.nTest), c.absTestCorr/float64(c.nTest)
				r.MAEHoldout, r.MAECorrected = rn(raw, 3), rn(corr, 3)
				r.Applied = c.n >= biasMinPairs && corr < 0.98*raw
			}
			out.bias = append(out.bias, r)
		}
	}
	return out, nil
}

// biasFor returns the correction to subtract from a forecast value.
func biasFor(rows []BiasRow, v, lead int) float64 {
	b := leadBucket(lead)
	if b < 0 {
		return 0
	}
	for i := range rows {
		r := &rows[i]
		if r.Applied && r.Variable == biasVarNames[v] && r.LeadFrom == leadBuckets[b][0] && r.Bias.Valid() {
			// Shrink towards zero while the sample is small.
			return r.Bias.Float() * float64(r.N) / float64(r.N+12)
		}
	}
	return 0
}

// ---------------------------------------------------------------- types

// WeatherNow is the latest observation with the Kalman-filtered pressure
// tendency of the station's own record.
type WeatherNow struct {
	T           int64  `json:"t"`
	TempC       Num    `json:"tempC"`
	DewPointC   Num    `json:"dewPointC"`
	RH          Num    `json:"rh"`
	PressureMSL Num    `json:"pressureMsl"`
	WindKmh     Num    `json:"windKmh"`
	WindDir     Num    `json:"windDir"`
	Wind        string `json:"wind,omitempty"` // compass point
	Cloud       Num    `json:"cloud"`
	VisibilityM Num    `json:"visibilityM"`
	Text        string `json:"text,omitempty"`
	// Change3h is the smoothed pressure tendency in hPa per 3 hours and
	// Tendency its class (ClassifyTendency).
	Change3h Num    `json:"change3h"`
	Tendency string `json:"tendency"`
	// Pressure6h is the Kalman extrapolation of the pressure six hours ahead.
	Pressure6h Est `json:"pressure6h"`
}

// WeatherHour is one hour of the bias-corrected Open-Meteo forecast.
type WeatherHour struct {
	T           int64  `json:"t"`
	LeadH       int    `json:"leadH"`
	TempC       Num    `json:"tempC"`    // bias-corrected
	TempRawC    Num    `json:"tempRawC"` // as forecast
	DewPointC   Num    `json:"dewPointC"`
	RH          Num    `json:"rh"`
	PressureMSL Num    `json:"pressureMsl"`
	WindKmh     Num    `json:"windKmh"`
	WindDir     Num    `json:"windDir"`
	GustKmh     Num    `json:"gustKmh"`
	Cloud       Num    `json:"cloud"`
	CloudLow    Num    `json:"cloudLow"`
	PrecipMM    Num    `json:"precipMm"`
	PrecipProb  Num    `json:"precipProb"`
	VisibilityM Num    `json:"visibilityM"`
	CAPE        Num    `json:"cape"`
	WeatherCode *int   `json:"weatherCode,omitempty"`
	Text        string `json:"text,omitempty"`
	IsDay       *bool  `json:"isDay,omitempty"`
	// Risks, 0..1.
	FogRisk        float64 `json:"fogRisk"`
	InversionRisk  float64 `json:"inversionRisk"`
	ConvectiveRisk float64 `json:"convectiveRisk"`
	Corrected      bool    `json:"corrected"` // a learned bias was applied to at least one variable
}

// ZambrettiOutlook is the independent barometric forecast.
type ZambrettiOutlook struct {
	ZambrettiResult
	PressureMSL Num     `json:"pressureMsl"`
	Change3h    Num     `json:"change3h"` // Kalman-smoothed, hPa / 3 h
	Tendency    string  `json:"tendency"`
	WindDir     Num     `json:"windDir"`
	Wind        string  `json:"wind,omitempty"`
	Season      string  `json:"season"`     // "summer" | "winter"
	Hemisphere  string  `json:"hemisphere"` // "north" | "south"
	Outlook     string  `json:"outlook"`    // plain-language 12-24 h outlook
	Confidence  float64 `json:"confidence"`
}

// Pattern kinds.
const (
	PatternPressure   = "pressure"
	PatternFrontNear  = "front-approaching"
	PatternFrontPast  = "front-passage"
	PatternFog        = "fog"
	PatternInversion  = "inversion"
	PatternConvective = "convection"
)

// Pattern is one detected or expected weather pattern.
type Pattern struct {
	Kind string `json:"kind"`
	// Severity 0 = information, 1 = low, 2 = moderate, 3 = high.
	Severity    int    `json:"severity"`
	Probability Num    `json:"probability"` // 0..1 where meaningful
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	From        int64  `json:"from,omitempty"` // Unix ms
	To          int64  `json:"to,omitempty"`
}

// WeatherOutlook is product 3: the local weather outlook.
type WeatherOutlook struct {
	Meta
	Attribution    string            `json:"attribution"`
	AttributionURL string            `json:"attributionUrl"`
	Current        *WeatherNow       `json:"current,omitempty"`
	Hours          []WeatherHour     `json:"hours"`
	Bias           []BiasRow         `json:"bias"`
	Zambretti      *ZambrettiOutlook `json:"zambretti,omitempty"`
	Patterns       []Pattern         `json:"patterns"`
	Analogue       *AnalogueForecast `json:"analogue,omitempty"`
}

// ---------------------------------------------------------------- helpers

func pmslOf(w *store.Weather, elevM float64) float64 {
	if fin(w.PressureMSL) {
		return w.PressureMSL
	}
	if fin(w.SurfacePressure) {
		return ReduceToMSL(w.SurfacePressure, elevM, w.TempC)
	}
	return nan
}

// pressureTrend runs a local-linear-trend Kalman filter over the hourly
// sea-level pressure record and returns the filtered level, the tendency in
// hPa per hour and the six-hour extrapolation with its standard error.
func pressureTrend(p []float64) (level, perHour, p6, sd6 float64, ok bool) {
	recent := 0
	for i := max(len(p)-6, 0); i < len(p); i++ {
		if fin(p[i]) {
			recent++
		}
	}
	if recent < 3 || stat.Count(p) < 4 {
		return nan, nan, nan, nan, false
	}
	k := stat.NewLocalTrend(0.01, 0.01, 0.02)
	for _, z := range p {
		k.Step(z, 1)
	}
	if k.N < 4 || !fin(k.Level) || !fin(k.Trend) {
		return nan, nan, nan, nan, false
	}
	p6, sd6 = k.Forecast(6)
	return k.Level, k.Trend, p6, sd6, true
}

func isNight(w *store.Weather, lat, lon float64, located bool) (night, known bool) {
	if fin(w.IsDay) {
		return w.IsDay < 0.5, true
	}
	if located {
		return !astro.IsDay(time.Unix(w.T, 0).UTC(), lat, lon), true
	}
	return false, false
}

func lightWind(kmh float64) float64 {
	if !fin(kmh) {
		return 0.5
	}
	return clamp01((14 - kmh) / 10)
}

// fogRisk scores radiation-fog / low-visibility conditions: small dew-point
// spread, light wind, night, little cloud above.
func fogRisk(w *store.Weather, night, nightKnown bool) float64 {
	if c := w.WeatherCode; c == 45 || c == 48 {
		return 0.95
	}
	risk := 0.0
	if fin(w.TempC) && fin(w.DewpointC) {
		fs := clamp01((3 - (w.TempC - w.DewpointC)) / 2.5)
		fn := 0.35
		if night || !nightKnown {
			fn = 1
		}
		sky := 1.0
		if fin(w.Cloud) {
			sky = 1 - 0.5*clamp01(w.Cloud/100)
		}
		risk = fs * lightWind(w.WindKmh) * fn * sky
	}
	if fin(w.VisibilityM) {
		switch {
		case w.VisibilityM < 1000:
			risk = math.Max(risk, 0.9)
		case w.VisibilityM < 5000:
			risk = math.Max(risk, 0.5)
		}
	}
	return round(clamp01(risk), 2)
}

// inversionRisk scores a surface temperature inversion: measured in the
// profile when available, else inferred from a clear, calm night.
func inversionRisk(w *store.Weather, a *Atmos, night, nightKnown bool) float64 {
	if a != nil && a.InversionC.Valid() && a.InversionC.Float() > 0 {
		return round(clamp01(0.55+a.InversionC.Float()/5), 2)
	}
	fn := 0.1
	if night {
		fn = 1
	} else if !nightKnown {
		fn = 0.5
	}
	sky := 0.6
	if fin(w.Cloud) {
		sky = 1 - clamp01(w.Cloud/100)
	}
	r := 0.8 * fn * sky * lightWind(w.WindKmh)
	if a != nil && a.HasProfile {
		r *= 0.5 // the profile shows no inversion at model resolution
	}
	return round(clamp01(r), 2)
}

// convectiveRisk scores thunderstorm potential from CAPE and the lifted index.
func convectiveRisk(cape float64, li *float64) float64 {
	r := 0.0
	if fin(cape) && cape > 0 {
		r = clamp01(cape / 2000)
	}
	if li != nil && *li < 0 {
		r = math.Max(r, clamp01(-*li/6))
	}
	return round(r, 2)
}

func severityOf(p float64) int {
	switch {
	case p >= 0.7:
		return 3
	case p >= 0.45:
		return 2
	case p >= 0.2:
		return 1
	}
	return 0
}

func hhmm(t int64, off int) string {
	return time.Unix(t+int64(off), 0).UTC().Format("15:04")
}

// ---------------------------------------------------------------- build

func (f *Forecaster) buildWeather(h *history, m *models, fcRows []store.Weather, rx rxInfo, now time.Time) *WeatherOutlook {
	nowMs := now.UnixMilli()
	out := &WeatherOutlook{
		Meta:        Meta{Product: "weather", Generated: nowMs, Level: LevelNone},
		Attribution: weather.Attribution, AttributionURL: weather.AttributionURL,
		Hours: []WeatherHour{}, Bias: m.Bias, Patterns: []Pattern{},
	}
	if out.Bias == nil {
		out.Bias = []BiasRow{}
	}
	off := f.utcOffset(h.cur)

	// --- current conditions and barometric tendency (own record).
	pser := h.obsSeries(72, func(w *store.Weather) float64 { return pmslOf(w, rx.elevM) })
	level, perHour, p6, sd6, trendOK := pressureTrend(pser)
	change3 := nan
	if trendOK {
		change3 = 3 * perHour
	}
	cw, catm, haveObs := h.latestObs(3)
	if haveObs {
		wn := &WeatherNow{
			T: cw.T * 1000, TempC: rn(cw.TempC, 1), DewPointC: rn(cw.DewpointC, 1), RH: rn(cw.RH, 0),
			PressureMSL: rn(pmslOf(cw, rx.elevM), 1), WindKmh: rn(cw.WindKmh, 1), WindDir: rn(cw.WindDir, 0),
			Wind: CompassPoint(cw.WindDir), Cloud: rn(cw.Cloud, 0), VisibilityM: rn(cw.VisibilityM, 0),
			Change3h: rn(change3, 2), Tendency: ClassifyTendency(change3),
			Pressure6h: est(p6, sd6, 0.6, 1, false),
		}
		if fin(cw.WeatherCode) {
			wn.Text = weather.WeatherCodeText(int(cw.WeatherCode))
		}
		out.Current = wn
	}

	// --- (a) Open-Meteo forecast with the learned bias removed.
	applied := 0
	for i := range fcRows {
		w := &fcRows[i]
		if w.T < h.cur || w.T > h.cur+168*hourSec {
			continue
		}
		lead := int((w.T - h.cur) / hourSec)
		bl := max(lead, 1)
		corr := func(v int, x, lo, hi float64) (float64, bool) {
			if !fin(x) {
				return x, false
			}
			b := biasFor(m.Bias, v, bl)
			if b == 0 {
				return x, false
			}
			return clamp(x-b, lo, hi), true
		}
		wh := WeatherHour{
			T: w.T * 1000, LeadH: lead, TempRawC: rn(w.TempC, 1), WindDir: rn(w.WindDir, 0),
			CloudLow: rn(w.CloudLow, 0), PrecipMM: rn(w.PrecipMM, 2), PrecipProb: Num(nan),
			VisibilityM: rn(w.VisibilityM, 0), CAPE: rn(w.CAPE, 0),
		}
		var c [nBiasVar]bool
		var v float64
		v, c[bvTemp] = corr(bvTemp, w.TempC, -90, 60)
		wh.TempC = rn(v, 1)
		tc := v
		v, c[bvDew] = corr(bvDew, w.DewpointC, -90, 40)
		if fin(v) && fin(tc) && v > tc {
			v = tc
		}
		wh.DewPointC = rn(v, 1)
		v, c[bvRH] = corr(bvRH, w.RH, 0, 100)
		wh.RH = rn(v, 0)
		v, c[bvPmsl] = corr(bvPmsl, w.PressureMSL, 850, 1100)
		wh.PressureMSL = rn(v, 1)
		v, c[bvWind] = corr(bvWind, w.WindKmh, 0, 400)
		wh.WindKmh = rn(v, 1)
		v, c[bvGust] = corr(bvGust, w.GustKmh, 0, 400)
		wh.GustKmh = rn(v, 1)
		v, c[bvCloud] = corr(bvCloud, w.Cloud, 0, 100)
		wh.Cloud = rn(v, 0)
		for _, x := range c {
			if x {
				wh.Corrected = true
			}
		}
		if wh.Corrected {
			applied++
		}
		if fin(w.WeatherCode) {
			code := int(w.WeatherCode)
			wh.WeatherCode = &code
			wh.Text = weather.WeatherCodeText(code)
		}
		night, known := isNight(w, rx.lat, rx.lon, rx.located)
		if known {
			d := !night
			wh.IsDay = &d
		}
		a := AnalyzeWeather(w, rx.elevM)
		var li *float64
		if doc, ok := parseProfile(w.ProfileJSON); ok {
			li = doc.LiftedIndex
			if doc.PrecipProb != nil {
				wh.PrecipProb = rn(*doc.PrecipProb, 0)
			}
		}
		// Risks use the corrected temperature / dew point.
		cw2 := *w
		cw2.TempC, cw2.DewpointC, cw2.WindKmh, cw2.Cloud = wh.TempC.Float(), wh.DewPointC.Float(), wh.WindKmh.Float(), wh.Cloud.Float()
		wh.FogRisk = fogRisk(&cw2, night, known)
		wh.InversionRisk = inversionRisk(&cw2, &a, night, known)
		wh.ConvectiveRisk = convectiveRisk(w.CAPE, li)
		out.Hours = append(out.Hours, wh)
	}

	// --- (b) Zambretti with the Kalman-smoothed tendency.
	if haveObs && fin(level) {
		wd := cw.WindDir
		if !fin(cw.WindKmh) || cw.WindKmh < 3 {
			wd = nan // calm: no wind correction
		}
		lt := time.Unix(h.cur+int64(off), 0).UTC()
		in := ZambrettiInput{PressureMSL: level, Change3h: change3, WindDir: wd, Month: lt.Month(), Southern: rx.located && rx.lat < 0}
		if !fin(change3) {
			in.Change3h = 0
		}
		if z, ok := Zambretti(in); ok {
			zo := &ZambrettiOutlook{
				ZambrettiResult: z, PressureMSL: rn(level, 1), Change3h: rn(change3, 2), Tendency: ClassifyTendency(change3),
				WindDir: rn(wd, 0), Wind: CompassPoint(wd), Season: "winter", Hemisphere: "north",
			}
			summer := in.Month >= time.April && in.Month <= time.September
			if in.Southern {
				zo.Hemisphere = "south"
				summer = !summer
			}
			if summer {
				zo.Season = "summer"
			}
			conf := 0.55
			if !trendOK {
				conf = 0.3
			}
			if !rx.located {
				conf *= 0.8
			}
			if z.Exceptional {
				conf *= 0.7
			}
			zo.Confidence = round(conf, 2)
			var sb strings.Builder
			fmt.Fprintf(&sb, "Barometer %.0f hPa, %s", level, zo.Tendency)
			if fin(change3) && math.Abs(change3) >= 0.5 {
				fmt.Fprintf(&sb, " (%+.1f hPa/3 h)", change3)
			}
			if zo.Wind != "" {
				fmt.Fprintf(&sb, ", wind %s", zo.Wind)
			}
			fmt.Fprintf(&sb, ": %s over the next 12-24 hours.", lowerFirst(z.Text))
			switch zo.Tendency {
			case TendencyFallingRapidly, TendencyFallingVRapidly:
				sb.WriteString(" The rapid fall points to an approaching front or deepening low within 6-12 hours, with strengthening wind.")
			case TendencyRisingRapidly, TendencyRisingVRapidly:
				sb.WriteString(" The rapid rise follows a front or trough: clearing but gusty at first.")
			}
			zo.Outlook = sb.String()
			out.Zambretti = zo
		}
	}

	// --- (c) patterns.
	out.Patterns = append(out.Patterns, f.detectPatterns(h, out, catm, change3, trendOK, rx, off)...)

	// --- (d) analogue forecast from the station's own record.
	out.Analogue = analogueForecast(h, m, rx, off)

	// --- meta.
	obsHours := 0
	for _, ok := range h.has {
		if ok {
			obsHours++
		}
	}
	out.DataHours = obsHours
	switch {
	case len(out.Hours) == 0 && !haveObs:
		out.Level = LevelNone
		out.Method = "No weather data yet (weather ingestion disabled or the station is not located)."
		out.setConfidence(0)
	case applied > 0:
		out.Level = LevelLearned
		out.Method = "Open-Meteo forecast with a learned local bias correction per variable and lead time (verified on held-out forecasts), plus an independent Zambretti barometric forecast with a Kalman-filtered pressure tendency, pattern detection and an analogue forecast from the station's own record."
		out.setConfidence(0.75)
	default:
		out.Level = LevelPhysics
		out.Method = "Open-Meteo forecast as issued (no verified local bias yet), plus an independent Zambretti barometric forecast with a Kalman-filtered pressure tendency, pattern detection and, with enough history, an analogue forecast from the station's own record."
		out.Notes = append(out.Notes, "bias correction needs about "+plural(biasMinPairs, "logged forecast")+" per lead-time class that verifiably improve on the raw forecast")
		out.setConfidence(0.55)
	}
	if out.Analogue == nil {
		out.Notes = append(out.Notes, needMore(obsHours, analogueMinHours, "the analogue forecast"))
	}
	return out
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// detectPatterns implements product 3c.
func (f *Forecaster) detectPatterns(h *history, out *WeatherOutlook, catm Atmos, change3 float64, trendOK bool, rx rxInfo, off int) []Pattern {
	var ps []Pattern
	south := rx.located && rx.lat < 0

	// Pressure tendency.
	if trendOK {
		tend := ClassifyTendency(change3)
		p := Pattern{Kind: PatternPressure, Probability: Num(nan), Title: "Pressure " + tend,
			Detail: fmt.Sprintf("Kalman-filtered tendency %+.1f hPa per 3 hours.", change3)}
		switch tend {
		case TendencyFallingRapidly:
			p.Severity = 2
		case TendencyFallingVRapidly:
			p.Severity = 3
		case TendencyFalling, TendencyRisingRapidly, TendencyRisingVRapidly:
			p.Severity = 1
		}
		ps = append(ps, p)
	}

	// Recent record: pressure, temperature, wind direction (oldest first).
	const span = 36
	pr := h.obsSeries(span, func(w *store.Weather) float64 { return pmslOf(w, rx.elevM) })
	tp := h.obsSeries(span, func(w *store.Weather) float64 { return w.TempC })
	wd := h.obsSeries(span, func(w *store.Weather) float64 {
		if !fin(w.WindKmh) || w.WindKmh < 4 {
			return nan
		}
		return w.WindDir
	})
	n := len(pr)
	lastT := h.cur
	if h.wxNow == nil {
		lastT = h.cur - hourSec
	}
	tAt := func(i int) int64 { return lastT - int64(n-1-i)*hourSec }
	// veer > 0: the wind turned in the frontal sense (clockwise in the
	// northern hemisphere, anticlockwise in the southern).
	veer := func(i, j int) float64 {
		if i < 0 || j >= n || !fin(wd[i]) || !fin(wd[j]) {
			return nan
		}
		d := stat.AngleDiffDeg(wd[j], wd[i])
		if south {
			d = -d
		}
		return d
	}

	// Approaching front: marked fall, or a fall with a backing wind.
	if trendOK && n >= 7 {
		back := veer(n-7, n-1)
		switch {
		case change3 <= -3.5:
			ps = append(ps, Pattern{Kind: PatternFrontNear, Severity: 2, Probability: 0.7,
				Title:  "Front or low approaching",
				Detail: fmt.Sprintf("Pressure is falling rapidly (%+.1f hPa/3 h): expect thickening cloud, rain and freshening wind within about 6-12 hours.", change3)})
		case change3 <= -zambrettiTrendHPa && fin(back) && back <= -20:
			ps = append(ps, Pattern{Kind: PatternFrontNear, Severity: 1, Probability: 0.5,
				Title:  "Front possibly approaching",
				Detail: fmt.Sprintf("Falling pressure (%+.1f hPa/3 h) with a wind backing %.0f° in six hours.", change3, -back)})
		}
	}

	// Frontal passage in the last 24 hours: pressure trough + wind veer +
	// temperature step.
	best, bestScore := -1, 0
	var bestVeer, bestStep, bestP float64
	for i := max(3, n-24); i+3 < n; i++ {
		if !fin(pr[i]) || !fin(pr[i-3]) || !fin(pr[i+3]) {
			continue
		}
		trough := pr[i-3]-pr[i] >= 1 && pr[i+3]-pr[i] >= 1
		v := veer(i-2, i+2)
		veered := fin(v) && v >= 30
		step := nan
		if fin(tp[i+3]) && fin(tp[i-3]) {
			step = tp[i+3] - tp[i-3]
			// Remove the diurnal cycle using the same hours one day earlier.
			if i-27 >= 0 && fin(tp[i-21]) && fin(tp[i-27]) {
				step -= tp[i-21] - tp[i-27]
			}
		}
		stepped := fin(step) && math.Abs(step) >= 2.5
		score := 0
		for _, b := range []bool{trough, veered, stepped} {
			if b {
				score++
			}
		}
		if score >= 2 && (trough || (veered && stepped && pr[i+3] > pr[i])) && score >= bestScore {
			best, bestScore, bestVeer, bestStep, bestP = i, score, v, step, pr[i]
		}
	}
	if best >= 0 {
		kind := "Front"
		if fin(bestStep) {
			if bestStep <= -2.5 {
				kind = "Cold front"
			} else if bestStep >= 2.5 {
				kind = "Warm front"
			}
		}
		ago := int((lastT - tAt(best)) / hourSec)
		var sb strings.Builder
		fmt.Fprintf(&sb, "Pressure trough %.1f hPa around %s", bestP, hhmm(tAt(best), off))
		if fin(bestVeer) && bestVeer >= 30 {
			fmt.Fprintf(&sb, ", wind veered %.0f°", bestVeer)
		}
		if fin(bestStep) && math.Abs(bestStep) >= 2.5 {
			fmt.Fprintf(&sb, ", temperature %+.1f K beyond the daily cycle", bestStep)
		}
		sb.WriteString(".")
		ps = append(ps, Pattern{Kind: PatternFrontPast, Severity: 1, Probability: rn(0.35+0.2*float64(bestScore), 2),
			Title: fmt.Sprintf("%s passed about %s ago", kind, plural(ago, "hour")), Detail: sb.String(),
			From: tAt(best) * 1000, To: tAt(best) * 1000})
	}

	// Risks over the next 24 forecast hours.
	type win struct {
		max      float64
		from, to int64
		at       int64
	}
	scan := func(get func(*WeatherHour) float64, thr float64) win {
		w := win{}
		for i := range out.Hours {
			hh := &out.Hours[i]
			if hh.LeadH > 24 {
				break
			}
			r := get(hh)
			if r > w.max {
				w.max, w.at = r, hh.T
			}
			if r >= thr {
				if w.from == 0 {
					w.from = hh.T
				}
				w.to = hh.T + hourSec*1000
			}
		}
		return w
	}
	if w := scan(func(x *WeatherHour) float64 { return x.FogRisk }, 0.4); w.max >= 0.3 {
		ps = append(ps, Pattern{Kind: PatternFog, Severity: severityOf(w.max), Probability: rn(w.max, 2),
			Title:  "Fog / low visibility possible",
			Detail: fmt.Sprintf("Small dew-point spread with light wind, highest risk around %s.", hhmm(w.at/1000, off)),
			From:   w.from, To: w.to})
	}
	if w := scan(func(x *WeatherHour) float64 { return x.InversionRisk }, 0.5); w.max >= 0.4 {
		d := fmt.Sprintf("Clear, calm conditions favour a surface temperature inversion around %s; inversions bend 1090 MHz signals downward and can extend range.", hhmm(w.at/1000, off))
		if catm.Valid && catm.InversionC.Valid() && catm.InversionC.Float() > 0 {
			d = fmt.Sprintf("The current profile shows an inversion of %.1f K in the lowest layer. ", catm.InversionC.Float()) + d
		}
		ps = append(ps, Pattern{Kind: PatternInversion, Severity: severityOf(w.max) - 1, Probability: rn(w.max, 2),
			Title: "Temperature inversion likely", Detail: d, From: w.from, To: w.to})
	}
	if w := scan(func(x *WeatherHour) float64 { return x.ConvectiveRisk }, 0.3); w.max >= 0.15 {
		capeMax := 0.0
		for i := range out.Hours {
			if out.Hours[i].LeadH <= 24 && out.Hours[i].CAPE.Valid() {
				capeMax = math.Max(capeMax, out.Hours[i].CAPE.Float())
			}
		}
		ps = append(ps, Pattern{Kind: PatternConvective, Severity: severityOf(w.max), Probability: rn(w.max, 2),
			Title:  "Convective (shower / thunderstorm) risk",
			Detail: fmt.Sprintf("CAPE up to %.0f J/kg in the next 24 hours, peaking around %s.", capeMax, hhmm(w.at/1000, off)),
			From:   w.from, To: w.to})
	}
	for i := range ps {
		if ps[i].Severity < 0 {
			ps[i].Severity = 0
		}
	}
	return ps
}
