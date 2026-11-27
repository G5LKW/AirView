package analytics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"airview/internal/stat"
	"airview/internal/store"
)

// minWeatherHours is the number of joint range + weather hours below which
// no conclusions are drawn.
const minWeatherHours = 48

type weatherVar struct {
	key, label, unit string
	get              func(w *store.Weather) float64
}

var weatherVars = []weatherVar{
	{"temp_c", "Temperature", "°C", func(w *store.Weather) float64 { return w.TempC }},
	{"dewpoint_c", "Dew point", "°C", func(w *store.Weather) float64 { return w.DewpointC }},
	{"rh", "Relative humidity", "%", func(w *store.Weather) float64 { return w.RH }},
	{"pressure_msl", "Pressure (MSL)", "hPa", func(w *store.Weather) float64 { return w.PressureMSL }},
	{"pressure_tend_3h", "Pressure tendency (3 h)", "hPa/3h", nil}, // derived
	{"wind_kmh", "Wind speed", "km/h", func(w *store.Weather) float64 { return w.WindKmh }},
	{"gust_kmh", "Wind gusts", "km/h", func(w *store.Weather) float64 { return w.GustKmh }},
	{"cloud", "Cloud cover", "%", func(w *store.Weather) float64 { return w.Cloud }},
	{"cloud_low", "Low cloud", "%", func(w *store.Weather) float64 { return w.CloudLow }},
	{"precip_mm", "Precipitation", "mm", func(w *store.Weather) float64 { return w.PrecipMM }},
	{"visibility_km", "Visibility", "km", func(w *store.Weather) float64 { return w.VisibilityM / 1000 }},
	{"cape", "CAPE", "J/kg", func(w *store.Weather) float64 { return w.CAPE }},
	{"n_surface", "Surface refractivity", "N", func(w *store.Weather) float64 { return w.NSurface }},
	{"dn_dh", "Refractivity gradient dN/dh", "N/km", func(w *store.Weather) float64 { return w.DNDH }},
	{"k_factor", "k-factor", "", func(w *store.Weather) float64 { return math.Min(w.KFactor, 5) }}, // trapping (k = 1000) capped
	{"duct", "Duct flag", "", func(w *store.Weather) float64 {
		if !isNum(w.Duct) {
			return math.NaN()
		}
		if w.Duct > 0 {
			return 1
		}
		return 0
	}},
}

var rangeMetrics = []struct {
	key, label string
	get        func(r *store.RangeHour) float64
}{
	{"p95_km", "95th percentile range", func(r *store.RangeHour) float64 { return r.P95Km }},
	{"max_km", "Maximum range", func(r *store.RangeHour) float64 { return r.MaxKm }},
	{"far_count", "Far positions", func(r *store.RangeHour) float64 { return float64(r.FarCount) }},
	{"resid_db", "Signal residual", func(r *store.RangeHour) float64 { return r.ResidDB }},
}

// WeatherVarInfo describes one weather variable of the analysis.
type WeatherVarInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Unit  string `json:"unit"`
	N     int    `json:"n"` // hours with this variable and a range value
	Mean  Num    `json:"mean"`
	Std   Num    `json:"std"`
}

// WeatherCorr is the association of one range metric with one variable.
type WeatherCorr struct {
	Var       string `json:"var"`
	Metric    string `json:"metric"`
	N         int    `json:"n"`
	Pearson   Num    `json:"pearson"`
	PearsonP  Num    `json:"pearsonP"`
	Spearman  Num    `json:"spearman"`
	SpearmanP Num    `json:"spearmanP"`
	// Partial is the correlation with the traffic level (aircraft count)
	// held constant.
	Partial  Num `json:"partial"`
	PartialP Num `json:"partialP"`
	// BestLagH is the lag in hours (-6..+6) with the strongest correlation;
	// positive = the range follows the weather by that many hours.
	BestLagH int `json:"bestLagH"`
	BestLagR Num `json:"bestLagR"`
	// Significant: partial correlation p-value below the Bonferroni
	// threshold for the number of variables tested, with enough hours.
	Significant bool `json:"significant"`
}

// WeatherResponse is a binned response curve of the 95th percentile range.
type WeatherResponse struct {
	Var    string             `json:"var"`
	Metric string             `json:"metric"`
	Bins   []stat.ResponseBin `json:"bins"`
}

// RidgeFactor is one term of the range model.
type RidgeFactor struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Group string `json:"group"` // "weather" | "traffic" | "time"
	// Beta is the standardised coefficient: standard deviations of range
	// per standard deviation of the factor.
	Beta Num `json:"beta"`
	// PerUnit is the coefficient in km per unit of the factor.
	PerUnit Num `json:"perUnit"`
}

// WeatherModel is the ridge regression explaining the hourly 95th
// percentile range from weather, traffic and hour-of-day terms.
type WeatherModel struct {
	OK         bool          `json:"ok"`
	Reason     string        `json:"reason,omitempty"`
	Target     string        `json:"target"`
	N          int           `json:"n"`
	R2         Num           `json:"r2"`         // in sample
	CVR2       Num           `json:"cvR2"`       // blocked 5-fold cross-validation
	BaseR2     Num           `json:"baseR2"`     // traffic + time-of-day terms only
	WeatherR2  Num           `json:"weatherR2"`  // R2 - BaseR2: what weather adds
	Lambda     Num           `json:"lambda"`     //
	ResidStdKm Num           `json:"residStdKm"` //
	Factors    []RidgeFactor `json:"factors"`    // sorted by |Beta|
}

// DuctEpisode is a run of hours with a duct flag or anomalously long range.
type DuctEpisode struct {
	From      int64  `json:"from"`
	To        int64  `json:"to"`
	Hours     int    `json:"hours"`
	PeakP95Km Num    `json:"peakP95Km"`
	PeakMaxKm Num    `json:"peakMaxKm"`
	PeakZ     Num    `json:"peakZ"`    // robust z-score of the range anomaly
	DuctFlag  bool   `json:"ductFlag"` // weather analysis flagged ducting
	Anomalous bool   `json:"anomalous"`
	MinDNDH   Num    `json:"minDnDh"`
	Reason    string `json:"reason"`
}

// WeatherImpact is the result of Analyzer.WeatherImpact.
type WeatherImpact struct {
	From         int64             `json:"from"`
	To           int64             `json:"to"`
	Ant          string            `json:"ant"`
	Hours        int               `json:"hours"` // hours with both range and weather data
	RangeHours   int               `json:"rangeHours"`
	WeatherHours int               `json:"weatherHours"`
	MinHours     int               `json:"minHours"`
	Enough       bool              `json:"enough"`
	Vars         []WeatherVarInfo  `json:"vars"`
	Metrics      []string          `json:"metrics"`
	Correlations []WeatherCorr     `json:"correlations"`
	Responses    []WeatherResponse `json:"responses"`
	Model        *WeatherModel     `json:"model"`
	Episodes     []DuctEpisode     `json:"episodes"`
	Conclusions  []string          `json:"conclusions"`
}

// WeatherImpact joins the hourly range metrics with the weather
// observations: Pearson / Spearman correlations with p-values and the best
// lag, partial correlations controlling for traffic, binned response
// curves, a ridge regression of the 95th percentile range and the list of
// ducting episodes. Default range: the last 60 days.
func (a *Analyzer) WeatherImpact(ctx context.Context, r Range, ant string) (*WeatherImpact, error) {
	now, _ := a.clock()
	from, to := r.resolve(now.Unix(), 60*86400, 3600)
	return cached(ctx, a, a.key("weather", ant, a.cfg(), from, to), ttlWeather, func(ctx context.Context) (*WeatherImpact, error) {
		rh, err := a.RangeHours(ctx, ant, from, to-1)
		if err != nil {
			return nil, err
		}
		wx, err := a.mainSt().WeatherObs(ctx, from-3*3600, to-1)
		if err != nil {
			return nil, err
		}
		return weatherImpact(from, to, ant, rh, wx), nil
	})
}

func weatherImpact(from, to int64, ant string, rh []store.RangeHour, wx []store.Weather) *WeatherImpact {
	res := &WeatherImpact{From: from * 1000, To: to * 1000, Ant: ant, MinHours: minWeatherHours,
		Vars: []WeatherVarInfo{}, Correlations: []WeatherCorr{}, Responses: []WeatherResponse{},
		Episodes: []DuctEpisode{}, Conclusions: []string{}}
	for _, m := range rangeMetrics {
		res.Metrics = append(res.Metrics, m.key)
	}
	n := int((to - from) / 3600)
	if n <= 0 || n > 24*366*20 {
		res.Conclusions = []string{"No data in the selected range."}
		return res
	}
	// Hourly grid.
	ys := make([][]float64, len(rangeMetrics))
	for i := range ys {
		ys[i] = nanSlice(n)
	}
	traffic := nanSlice(n)
	maxKm := nanSlice(n)
	for i := range rh {
		row := &rh[i]
		k := int((row.T - from) / 3600)
		if row.T < from || k >= n || row.Positions <= 0 {
			continue
		}
		for j, m := range rangeMetrics {
			ys[j][k] = m.get(row)
		}
		traffic[k] = math.Log1p(float64(row.Aircraft))
		maxKm[k] = row.MaxKm
		if isNum(row.P95Km) {
			res.RangeHours++
		}
	}
	xs := make([][]float64, len(weatherVars))
	for i := range xs {
		xs[i] = nanSlice(n)
	}
	pressure := map[int64]float64{}
	have := make([]bool, n)
	for i := range wx {
		pressure[wx[i].T] = wx[i].PressureMSL
	}
	for i := range wx {
		w := &wx[i]
		k := int((w.T - from) / 3600)
		if w.T < from || k >= n {
			continue
		}
		got := false
		for j, v := range weatherVars {
			var val float64
			if v.get != nil {
				val = v.get(w)
			} else if p0, ok := pressure[w.T-3*3600]; ok {
				val = w.PressureMSL - p0
			} else {
				val = math.NaN()
			}
			if isNum(val) {
				xs[j][k] = val
				got = true
			}
		}
		if got {
			have[k] = true
			res.WeatherHours++
		}
	}
	p95 := ys[0]
	for k := 0; k < n; k++ {
		if have[k] && isNum(p95[k]) {
			res.Hours++
		}
	}
	res.Enough = res.Hours >= minWeatherHours

	// Variables and correlations.
	bonf := 0.05 / float64(len(weatherVars))
	for j, v := range weatherVars {
		info := WeatherVarInfo{Key: v.key, Label: v.label, Unit: v.unit, Mean: Num(math.NaN()), Std: Num(math.NaN())}
		var vals []float64
		for k := 0; k < n; k++ {
			if isNum(xs[j][k]) && isNum(p95[k]) {
				vals = append(vals, xs[j][k])
			}
		}
		info.N = len(vals)
		if len(vals) > 0 {
			info.Mean = Num(stat.Mean(vals))
		}
		if len(vals) > 1 {
			info.Std = Num(stat.Std(vals))
		}
		res.Vars = append(res.Vars, info)
		if info.N < 12 {
			continue
		}
		for mi, m := range rangeMetrics {
			c := WeatherCorr{Var: v.key, Metric: m.key, Pearson: Num(math.NaN()), PearsonP: Num(math.NaN()),
				Spearman: Num(math.NaN()), SpearmanP: Num(math.NaN()), Partial: Num(math.NaN()), PartialP: Num(math.NaN()),
				BestLagR: Num(math.NaN())}
			pr, err := stat.Pearson(xs[j], ys[mi])
			if err != nil {
				continue
			}
			c.N, c.Pearson, c.PearsonP = pr.N, Num(pr.R), Num(pr.P)
			if sr, err := stat.Spearman(xs[j], ys[mi]); err == nil {
				c.Spearman, c.SpearmanP = Num(sr.R), Num(sr.P)
			}
			if pc, err := stat.PartialCorr(xs[j], ys[mi], traffic); err == nil {
				c.Partial, c.PartialP = Num(pc.R), Num(pc.P)
				c.Significant = pc.P < bonf && pc.N >= minWeatherHours
			}
			if bl, err := stat.BestLag(xs[j], ys[mi], 6, 24); err == nil && isNum(bl.R) {
				c.BestLagH, c.BestLagR = bl.Lag, Num(bl.R)
			}
			res.Correlations = append(res.Correlations, c)
		}
		if bins, err := stat.BinnedResponse(xs[j], p95, &stat.BinOptions{Bins: 8, EqualCount: true, MinCount: 4}); err == nil && len(bins) >= 2 {
			res.Responses = append(res.Responses, WeatherResponse{Var: v.key, Metric: rangeMetrics[0].key, Bins: bins})
		}
	}

	res.Model = weatherModel(from, p95, traffic, xs)
	res.Episodes = ductEpisodes(from, p95, maxKm, traffic, xs)
	res.Conclusions = weatherConclusions(res)
	return res
}

func varIndex(key string) int {
	for i, v := range weatherVars {
		if v.key == key {
			return i
		}
	}
	return -1
}

// weatherModel fits the ridge regression of p95 on weather + traffic +
// hour-of-day harmonics.
func weatherModel(from int64, p95, traffic []float64, xs [][]float64) *WeatherModel {
	nan := Num(math.NaN())
	m := &WeatherModel{Target: "p95_km", R2: nan, CVR2: nan, BaseR2: nan, WeatherR2: nan, Lambda: nan, ResidStdKm: nan, Factors: []RidgeFactor{}}
	n := len(p95)
	valid := 0
	for k := 0; k < n; k++ {
		if isNum(p95[k]) {
			valid++
		}
	}
	// Weather variables that are present for most range hours and vary.
	type feat struct {
		name, label, group string
		col                []float64
	}
	var feats []feat
	for j, v := range weatherVars {
		cnt := 0
		lo, hi := math.Inf(1), math.Inf(-1)
		for k := 0; k < n; k++ {
			if isNum(p95[k]) && isNum(xs[j][k]) {
				cnt++
				lo, hi = math.Min(lo, xs[j][k]), math.Max(hi, xs[j][k])
			}
		}
		if valid > 0 && float64(cnt) >= 0.8*float64(valid) && hi > lo {
			feats = append(feats, feat{v.key, v.label, "weather", xs[j]})
		}
	}
	nWeather := len(feats)
	if nWeather == 0 {
		m.Reason = fmt.Sprintf("not enough data yet: %d hours with range data and no usable weather variables", valid)
		return m
	}
	harm := [4][]float64{make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)}
	for k := 0; k < n; k++ {
		ph := 2 * math.Pi * float64((from+int64(k)*3600)%86400) / 86400
		harm[0][k], harm[1][k] = math.Sin(ph), math.Cos(ph)
		harm[2][k], harm[3][k] = math.Sin(2*ph), math.Cos(2*ph)
	}
	feats = append(feats,
		feat{"traffic", "Traffic (log aircraft)", "traffic", traffic},
		feat{"hod_sin24", "Hour of day (24 h sine)", "time", harm[0]},
		feat{"hod_cos24", "Hour of day (24 h cosine)", "time", harm[1]},
		feat{"hod_sin12", "Hour of day (12 h sine)", "time", harm[2]},
		feat{"hod_cos12", "Hour of day (12 h cosine)", "time", harm[3]})
	var X [][]float64
	var y []float64
	names := make([]string, len(feats))
	for i, f := range feats {
		names[i] = f.name
	}
rows:
	for k := 0; k < n; k++ {
		if !isNum(p95[k]) {
			continue
		}
		row := make([]float64, len(feats))
		for i, f := range feats {
			if !isNum(f.col[k]) {
				continue rows
			}
			row[i] = f.col[k]
		}
		X = append(X, row)
		y = append(y, p95[k])
	}
	m.N = len(y)
	if need := max(minWeatherHours, 4*len(feats)); m.N < need {
		m.Reason = fmt.Sprintf("not enough data yet: %d complete hours, %d needed for the model", m.N, need)
		return m
	}
	sdY := stat.PopStd(y)
	if !(sdY > 0) {
		m.Reason = "the range did not vary in this period"
		return m
	}
	mod, cv, err := stat.RidgeCV(X, y, names, stat.LogSpace(1e-3, 3, 8), stat.BlockFold(len(y), 5))
	if err != nil {
		if mod, err = stat.Ridge(X, y, names, 0.05); err != nil {
			m.Reason = "model fit failed: " + err.Error()
			return m
		}
	} else if cv.BestMSE >= 0 {
		m.CVR2 = Num(1 - cv.BestMSE/(sdY*sdY))
	}
	m.OK = true
	m.R2, m.Lambda, m.ResidStdKm = Num(mod.R2), Num(mod.Lambda), Num(mod.ResidStd)
	raw, _ := mod.RawCoef()
	for i, f := range feats {
		if i >= len(mod.Coef) || i >= len(raw) {
			break
		}
		m.Factors = append(m.Factors, RidgeFactor{Name: f.name, Label: f.label, Group: f.group,
			Beta: Num(mod.Coef[i] / sdY), PerUnit: Num(raw[i])})
	}
	sort.SliceStable(m.Factors, func(i, j int) bool {
		return math.Abs(float64(m.Factors[i].Beta)) > math.Abs(float64(m.Factors[j].Beta))
	})
	// Baseline without the weather terms.
	Xb := make([][]float64, len(X))
	for i := range X {
		Xb[i] = X[i][nWeather:]
	}
	if base, err := stat.Ridge(Xb, y, names[nWeather:], mod.Lambda); err == nil {
		m.BaseR2 = Num(base.R2)
		m.WeatherR2 = Num(mod.R2 - base.R2)
	}
	return m
}

// ductEpisodes lists runs of hours with the duct flag set or with a range
// far above what the traffic level explains.
func ductEpisodes(from int64, p95, maxKm, traffic []float64, xs [][]float64) []DuctEpisode {
	n := len(p95)
	out := []DuctEpisode{}
	// Range anomaly: robust z-score of the residual of p95 on traffic.
	resid := nanSlice(n)
	if fit, err := stat.LinearRegression(traffic, p95); err == nil && fit.N >= minWeatherHours {
		for k := 0; k < n; k++ {
			if isNum(p95[k]) && isNum(traffic[k]) {
				resid[k] = p95[k] - fit.Predict(traffic[k])
			}
		}
	} else if stat.Count(p95) >= minWeatherHours {
		copy(resid, p95)
	}
	z := stat.RobustZScores(resid)
	duct, dndh := xs[varIndex("duct")], xs[varIndex("dn_dh")]
	flagged := func(k int) (d, an bool) {
		return isNum(duct[k]) && duct[k] > 0, k < len(z) && isNum(z[k]) && z[k] > 3
	}
	var cur *DuctEpisode
	gap := 0
	closeEp := func() {
		if cur == nil {
			return
		}
		switch {
		case cur.DuctFlag && cur.Anomalous:
			cur.Reason = "ducting indicated by the weather profile and range far above normal"
		case cur.DuctFlag:
			cur.Reason = "ducting indicated by the weather profile"
		default:
			cur.Reason = "range far above normal for the traffic level"
		}
		out = append(out, *cur)
		cur = nil
	}
	for k := 0; k < n; k++ {
		d, an := flagged(k)
		if !d && !an {
			if gap++; gap > 1 {
				closeEp()
			}
			continue
		}
		gap = 0
		t := (from + int64(k)*3600) * 1000
		if cur == nil {
			cur = &DuctEpisode{From: t, PeakP95Km: Num(math.NaN()), PeakMaxKm: Num(math.NaN()), PeakZ: Num(math.NaN()), MinDNDH: Num(math.NaN())}
		}
		cur.To = t + 3600_000
		cur.Hours = int((cur.To - cur.From) / 3600_000)
		cur.DuctFlag = cur.DuctFlag || d
		cur.Anomalous = cur.Anomalous || an
		cur.PeakP95Km = Num(nanMax(float64(cur.PeakP95Km), p95[k]))
		cur.PeakMaxKm = Num(nanMax(float64(cur.PeakMaxKm), maxKm[k]))
		if k < len(z) {
			cur.PeakZ = Num(nanMax(float64(cur.PeakZ), z[k]))
		}
		cur.MinDNDH = Num(nanMin(float64(cur.MinDNDH), dndh[k]))
	}
	closeEp()
	// Newest first, at most 100.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

func weatherConclusions(w *WeatherImpact) []string {
	if !w.Enough {
		return []string{fmt.Sprintf("Not enough data yet: %d hours with both range and weather data (%d range hours, %d weather hours); at least %d are needed before any conclusion is drawn.",
			w.Hours, w.RangeHours, w.WeatherHours, w.MinHours)}
	}
	out := []string{fmt.Sprintf("Based on %d hours with both range and weather data.", w.Hours)}
	labels := map[string]string{}
	for _, v := range w.Vars {
		labels[v.Key] = v.Label
	}
	var sig []WeatherCorr
	for _, c := range w.Correlations {
		if c.Metric == "p95_km" && c.Significant && math.Abs(float64(c.Partial)) >= 0.15 {
			sig = append(sig, c)
		}
	}
	sort.Slice(sig, func(i, j int) bool { return math.Abs(float64(sig[i].Partial)) > math.Abs(float64(sig[j].Partial)) })
	if len(sig) == 0 {
		out = append(out, "No weather variable shows a statistically reliable link with the 95th percentile range once the traffic level is held constant.")
	}
	for i, c := range sig {
		if i >= 4 {
			break
		}
		dir := "increases"
		if c.Partial < 0 {
			dir = "decreases"
		}
		strength := "weakly"
		switch a := math.Abs(float64(c.Partial)); {
		case a >= 0.5:
			strength = "strongly"
		case a >= 0.3:
			strength = "moderately"
		}
		s := fmt.Sprintf("Range %s %s with %s (partial r = %+.2f at constant traffic, n = %d).", dir, strength, strings.ToLower(labels[c.Var]), float64(c.Partial), c.N)
		if c.BestLagH != 0 && math.Abs(float64(c.BestLagR)) > math.Abs(float64(c.Pearson))+0.05 {
			if c.BestLagH > 0 {
				s += fmt.Sprintf(" The link is strongest %d h after the weather change.", c.BestLagH)
			} else {
				s += fmt.Sprintf(" The range change leads the weather reading by %d h.", -c.BestLagH)
			}
		}
		out = append(out, s)
	}
	if m := w.Model; m != nil && m.OK {
		s := fmt.Sprintf("A ridge regression on weather, traffic and time of day explains %.0f %% of the hourly range variation", 100*float64(m.R2))
		if isNum(float64(m.CVR2)) {
			s += fmt.Sprintf(" (%.0f %% on held-out blocks)", 100*math.Max(0, float64(m.CVR2)))
		}
		if isNum(float64(m.WeatherR2)) {
			s += fmt.Sprintf("; weather adds %.0f percentage points over traffic and time of day alone", 100*math.Max(0, float64(m.WeatherR2)))
		}
		out = append(out, s+".")
		var top []string
		for _, f := range m.Factors {
			if f.Group == "weather" && len(top) < 3 && math.Abs(float64(f.Beta)) >= 0.05 {
				top = append(top, fmt.Sprintf("%s (%+.2f)", strings.ToLower(f.Label), float64(f.Beta)))
			}
		}
		if len(top) > 0 {
			out = append(out, "Weather factors that matter most (standardised coefficients): "+strings.Join(top, ", ")+".")
		}
		if isNum(float64(m.CVR2)) && float64(m.CVR2) < 0.1 {
			out = append(out, "The model barely predicts held-out hours, so treat the individual coefficients with caution.")
		}
	} else if m != nil && m.Reason != "" {
		out = append(out, "Regression model: "+m.Reason+".")
	}
	nd := 0
	for _, e := range w.Episodes {
		if e.DuctFlag && e.Anomalous {
			nd++
		}
	}
	if len(w.Episodes) > 0 {
		out = append(out, fmt.Sprintf("%d episode(s) of ducting or anomalously long range were found; in %d of them the weather profile indicated ducting while the range was far above normal.", len(w.Episodes), nd))
	}
	if w.Hours < 24*14 {
		out = append(out, "Less than two weeks of data: seasonal effects and rare conditions are not covered yet. Hourly values are also correlated in time, which makes p-values optimistic.")
	}
	return out
}
