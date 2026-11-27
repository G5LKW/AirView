package predict

import (
	"math"
	"sort"

	"airview/internal/stat"
)

// Data thresholds of the traffic model, in valid history hours.
const (
	trafficDailyHours  = 24      // hour-of-day profile
	trafficWeeklyHours = 8 * 24  // hour-of-week profile
	trafficFullHours   = 28 * 24 // every hour-of-week slot seen four times
	trafficARHours     = 72      // AR correction of the anomaly
	trafficBTHours     = 96      // rolling-origin backtest
)

// seriesModel forecasts one hourly, non-negative series (aircraft count,
// message rate): a seasonal baseline (hour-of-week profile shrunk towards
// the hour-of-day profile, both with exponential forgetting) plus an AR
// forecast of the recent anomaly, which decays back to the baseline.
type seriesModel struct {
	Level string                `json:"level"` // "none" | "mean" | "daily" | "weekly"
	N     int                   `json:"n"`     // valid samples
	Mean  float64               `json:"mean"`
	HOD   *stat.SeasonalProfile `json:"hod,omitempty"`
	HOW   *stat.SeasonalProfile `json:"how,omitempty"`
	AR    *stat.ARModel         `json:"ar,omitempty"`
	// ResidStd is the robust spread of the in-sample anomalies.
	ResidStd float64 `json:"residStd"`
	// HorizonRMSE is the backtested forecast error per lead hour (index
	// lead-1); empty until a backtest could run.
	HorizonRMSE []float64 `json:"horizonRmse,omitempty"`
	Skill       *Skill    `json:"skill,omitempty"`
}

// fitSeries fits a seriesModel to y, whose sample i belongs to
// hour-of-week slot sW[i] and hour-of-day slot sD[i]. Only y[:len(y)] is
// read, so it cannot see the future of a truncated series.
func fitSeries(y []float64, sW, sD []int) *seriesModel {
	n := len(y)
	m := &seriesModel{Level: "none", N: stat.Count(y)}
	if m.N == 0 || len(sW) < n || len(sD) < n {
		return m
	}
	m.Level = "mean"
	m.Mean = stat.Median(y)
	if m.N >= trafficDailyHours {
		if p, err := stat.FitSeasonalProfileSlots(y, sD[:n], stat.HoursPerDay, &stat.ProfileOptions{HalfLife: 14 * 24}); err == nil {
			m.HOD, m.Level = p, "daily"
		}
	}
	if m.N >= trafficWeeklyHours {
		if p, err := stat.FitSeasonalProfileSlots(y, sW[:n], stat.HoursPerWeek, &stat.ProfileOptions{HalfLife: 6 * 7 * 24}); err == nil {
			m.HOW, m.Level = p, "weekly"
		}
	}
	resid := make([]float64, n)
	for i, v := range y {
		resid[i] = v - m.base(sW[i], sD[i]) // NaN stays NaN
	}
	m.ResidStd = stat.RobustStd(resid)
	if !fin(m.ResidStd) || m.ResidStd == 0 {
		m.ResidStd = or(stat.Std(resid), 0)
	}
	if stat.Count(resid) >= trafficARHours {
		if ar, err := stat.FitARYuleWalker(resid, 3); err == nil && arUsable(ar) {
			m.AR = ar
		}
	}
	return m
}

func arUsable(ar *stat.ARModel) bool {
	if ar == nil || !fin(ar.Const) || !fin(ar.Mean) {
		return false
	}
	s := 0.0
	for _, c := range ar.Coef {
		if !fin(c) {
			return false
		}
		s += math.Abs(c)
	}
	return s < 1.6
}

// base is the seasonal expectation for an hour-of-week / hour-of-day slot.
func (m *seriesModel) base(sw, sd int) float64 {
	b := m.Mean
	if m.HOD != nil {
		if v, ok := m.HOD.At(sd); ok {
			b = v
		}
	}
	if m.HOW != nil {
		if v, ok := m.HOW.At(sw); ok {
			// Shrink the weekly slot towards the daily profile while it has
			// been seen only once or twice.
			c := 1.0
			if i := ((sw % stat.HoursPerWeek) + stat.HoursPerWeek) % stat.HoursPerWeek; i < len(m.HOW.Weight) {
				c = m.HOW.Weight[i]
			}
			w := c / (c + 1)
			b = w*v + (1-w)*b
		}
	}
	return b
}

// forecast returns the next h values after y (same slot convention as
// fitSeries; sW / sD must cover len(y)+h). The recent anomaly is continued
// with the AR model, or with a geometric decay while there is none.
func (m *seriesModel) forecast(y []float64, sW, sD []int, h int) []float64 {
	n := len(y)
	out := make([]float64, h)
	if m.Level == "none" || len(sW) < n+h || len(sD) < n+h {
		for i := range out {
			out[i] = nan
		}
		return out
	}
	const tail = 6
	var rt [tail]float64
	k := min(tail, n)
	for j := 0; j < k; j++ {
		i := n - k + j
		rt[j] = y[i] - m.base(sW[i], sD[i])
	}
	var anom []float64
	if m.AR != nil {
		anom = m.AR.Forecast(rt[:k], h)
	} else {
		anom = make([]float64, h)
		last := 0.0
		for j := k - 1; j >= 0 && j >= k-3; j-- {
			if fin(rt[j]) {
				last = rt[j]
				break
			}
		}
		for i := range anom {
			last *= 0.8
			anom[i] = last
		}
	}
	for i := range out {
		v := m.base(sW[n+i], sD[n+i]) + anom[i]
		if v < 0 {
			v = 0
		}
		out[i] = v
	}
	return out
}

// sigma returns the forecast standard error for lead hour k (1-based).
func (m *seriesModel) sigma(k int, value float64) float64 {
	if k >= 1 && k <= len(m.HorizonRMSE) && m.HorizonRMSE[k-1] > 0 {
		return m.HorizonRMSE[k-1]
	}
	if m.ResidStd > 0 && m.N >= trafficDailyHours {
		return m.ResidStd * (1 + 0.5*math.Min(1, float64(k)/24))
	}
	if fin(value) {
		return 0.35*math.Abs(value) + 1
	}
	return nan
}

// dataConfidence is the 0..1 trust in the model from the amount of history.
func (m *seriesModel) dataConfidence() float64 {
	if m.Level == "none" {
		return 0
	}
	c := float64(m.N) / (float64(m.N) + 14*24)
	if m.Skill != nil && m.Skill.VsSeasonalNaive.Valid() && m.Skill.VsSeasonalNaive.Float() < 0 {
		c *= 0.7
	}
	return c
}

func persistenceForecaster(history []float64, h int) ([]float64, error) {
	v := lastFinite(history)
	out := make([]float64, h)
	for i := range out {
		out[i] = v
	}
	return out, nil
}

// VerifyPoint is one backtest forecast next to what really happened. The
// forecast was made LeadH hours ahead: from data before T - (LeadH-1) h only.
type VerifyPoint struct {
	T        int64 `json:"t"` // start of the target hour, Unix ms
	LeadH    int   `json:"leadH"`
	Actual   Num   `json:"actual"`
	Forecast Num   `json:"forecast"`
}

// backtestSeries evaluates the seriesModel on y by rolling-origin
// validation (every origin refits on y[:origin] only) against persistence
// and the seasonal-naive forecast, stores skill and per-lead errors in m and
// returns a day-ahead forecast-versus-actual series for charts.
func backtestSeries(m *seriesModel, y []float64, sW, sD []int, t0 int64, unit string) []VerifyPoint {
	n := len(y)
	if m == nil || m.N < trafficBTHours || n < trafficBTHours {
		return nil
	}
	fc := func(hist []float64, h int) ([]float64, error) {
		// stat.Backtest caps both length and capacity of hist at the origin.
		mm := fitSeries(hist, sW, sD)
		return mm.forecast(hist, sW, sD, min(h, n-len(hist))), nil
	}
	period := stat.HoursPerDay
	if n >= 3*stat.HoursPerWeek {
		period = stat.HoursPerWeek
	}
	opt := &stat.BacktestOptions{Horizon: HorizonHours, Step: 37, MaxOrigins: 24, MinTrain: max(n/2, 72)}
	cs, err1 := stat.CompareBacktest(y, fc, stat.SeasonalNaiveForecaster(period), opt)
	cp, err2 := stat.CompareBacktest(y, fc, persistenceForecaster, opt)
	if err1 == nil && err2 == nil && cs.N > 0 {
		sk := &Skill{
			Metric: "rmse", Unit: unit, HorizonH: HorizonHours, N: cs.N, Origins: len(cs.Model.Origins),
			Model:         rn(cs.Model.Overall.RMSE, 3),
			SeasonalNaive: rn(cs.Baseline.Overall.RMSE, 3), VsSeasonalNaive: rn(cs.Skill, 3),
			Persistence: rn(cp.Baseline.Overall.RMSE, 3), VsPersistence: rn(cp.Skill, 3),
		}
		if period == stat.HoursPerDay {
			sk.Note = "seasonal-naive baseline = same hour yesterday (less than three weeks of history)"
		} else {
			sk.Note = "seasonal-naive baseline = same hour last week"
		}
		m.Skill = sk
		// Per-lead error, smoothed over neighbouring leads (few origins).
		raw := make([]float64, HorizonHours)
		for k := range raw {
			raw[k] = nan
			if k < len(cs.Model.ByHorizon) && cs.Model.ByHorizon[k].N > 0 {
				raw[k] = cs.Model.ByHorizon[k].RMSE
			}
		}
		sm := stat.RollingMeanCentered(raw, 9)
		floor := 0.5 * m.ResidStd
		m.HorizonRMSE = make([]float64, HorizonHours)
		for k := range sm {
			v := sm[k]
			if !fin(v) {
				v = cs.Model.Overall.RMSE
			}
			m.HorizonRMSE[k] = round(math.Max(or(v, 0), floor), 4)
		}
	}

	// Day-ahead forecasts of the last seven days.
	var pts []VerifyPoint
	for d := 7; d >= 1; d-- {
		o := n - 24*d
		if o < 72 {
			continue
		}
		mm := fitSeries(y[:o:o], sW, sD)
		f := mm.forecast(y[:o:o], sW, sD, 24)
		for k, v := range f {
			pts = append(pts, VerifyPoint{T: (t0 + int64(o+k)*hourSec) * 1000, LeadH: k + 1, Actual: rn(y[o+k], 2), Forecast: rn(v, 2)})
		}
	}
	return pts
}

// ---------------------------------------------------------------- product

// TrafficHour is one forecast hour.
type TrafficHour struct {
	T        int64 `json:"t"`        // start of the hour, Unix ms
	Aircraft Est   `json:"aircraft"` // peak number of aircraft tracked at once
	MsgRate  Est   `json:"msgRate"`  // frames per second
}

// BusyHour is one of the busiest upcoming hours.
type BusyHour struct {
	T        int64 `json:"t"`
	Aircraft Num   `json:"aircraft"`
	MsgRate  Num   `json:"msgRate"`
	InHours  int   `json:"inHours"` // hours from now
}

// TrafficActual is one observed hour, for drawing the recent past next to
// the forecast.
type TrafficActual struct {
	T        int64 `json:"t"`
	Aircraft Num   `json:"aircraft"`
	MsgRate  Num   `json:"msgRate"`
}

// TrafficForecast is product 1: expected traffic for the next 72 hours.
type TrafficForecast struct {
	Meta
	// UTCOffsetSec is the local-time offset used for the hour-of-week
	// profile at the first forecast hour.
	UTCOffsetSec int             `json:"utcOffsetSec"`
	Hours        []TrafficHour   `json:"hours"`
	Busiest      []BusyHour      `json:"busiest"`
	Quietest     *BusyHour       `json:"quietest,omitempty"`
	Recent       []TrafficActual `json:"recent"` // last 48 observed hours
	// MsgRateSkill is the backtest of the message-rate model (Meta.Skill is
	// the aircraft-count model).
	MsgRateSkill *Skill `json:"msgRateSkill,omitempty"`
}

func (f *Forecaster) buildTraffic(h *history, m *models, nowMs int64) (*TrafficForecast, []float64) {
	out := &TrafficForecast{
		Meta:    Meta{Product: "traffic", Generated: nowMs, Level: LevelNone},
		Hours:   make([]TrafficHour, 0, HorizonHours),
		Busiest: []BusyHour{}, Recent: []TrafficActual{},
		UTCOffsetSec: f.utcOffset(h.cur),
	}
	am, mm := m.Aircraft, m.MsgRate
	if am == nil {
		am = &seriesModel{Level: "none"}
	}
	if mm == nil {
		mm = &seriesModel{Level: "none"}
	}
	ac := am.forecast(h.aircraft, h.sW, h.sD, HorizonHours)
	mr := mm.forecast(h.msgRate, h.sW, h.sD, HorizonHours)
	ca, cm := am.dataConfidence(), mm.dataConfidence()
	for k := 0; k < HorizonHours; k++ {
		decay := 1 - 0.25*float64(k)/HorizonHours
		out.Hours = append(out.Hours, TrafficHour{
			T:        (h.cur + int64(k)*hourSec) * 1000,
			Aircraft: est(ac[k], am.sigma(k+1, ac[k]), ca*decay, 1, true),
			MsgRate:  est(mr[k], mm.sigma(k+1, mr[k]), cm*decay, 1, true),
		})
	}
	for i := max(h.n-48, 0); i < h.n; i++ {
		out.Recent = append(out.Recent, TrafficActual{T: h.t(i) * 1000, Aircraft: rn(h.aircraft[i], 1), MsgRate: rn(h.msgRate[i], 1)})
	}

	out.DataHours = am.N
	out.Skill, out.MsgRateSkill = am.Skill, mm.Skill
	out.setConfidence(ca)
	switch am.Level {
	case "none":
		out.Method = "No traffic history yet: the forecast starts after the first complete hour of reception."
		out.Notes = append(out.Notes, "needs at least 1 day of reception for a daily profile and 8 days for a weekly one")
		return out, ac
	case "mean":
		out.Level = LevelPhysics
		out.Method = "Typical level of the few hours received so far (less than a day of history), recent anomaly decaying."
		out.Notes = append(out.Notes, needMore(am.N, trafficDailyHours, "a daily traffic profile"))
	case "daily":
		out.Level = LevelBlend
		out.Method = "Hour-of-day profile (weighted median, 14-day half-life) plus the recent anomaly continued by an AR(3) model; intervals from the spread of the anomalies."
		out.Notes = append(out.Notes, needMore(am.N, trafficWeeklyHours, "a weekly (weekday/weekend) profile"))
	default:
		out.Level = LevelLearned
		out.Method = "Hour-of-week seasonal profile with exponential forgetting (6-week half-life, shrunk towards the hour-of-day profile) plus an AR(3) forecast of the recent anomaly; 80 % intervals from rolling-origin backtest errors per lead time."
		if am.N < trafficFullHours {
			out.Level = LevelBlend
			out.Notes = append(out.Notes, needMore(am.N, trafficFullHours, "a settled weekly profile"))
		}
	}

	// Busiest upcoming hours: greedy peaks at least three hours apart.
	order := make([]int, 0, HorizonHours)
	for k, v := range ac {
		if fin(v) {
			order = append(order, k)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return ac[order[i]] > ac[order[j]] })
	for _, k := range order {
		if len(out.Busiest) >= 5 {
			break
		}
		near := false
		for _, b := range out.Busiest {
			if d := b.InHours - k; d > -3 && d < 3 {
				near = true
				break
			}
		}
		if !near {
			out.Busiest = append(out.Busiest, BusyHour{T: out.Hours[k].T, Aircraft: rn(ac[k], 1), MsgRate: rn(mr[k], 1), InHours: k})
		}
	}
	if len(order) > 0 {
		k := order[len(order)-1]
		out.Quietest = &BusyHour{T: out.Hours[k].T, Aircraft: rn(ac[k], 1), MsgRate: rn(mr[k], 1), InHours: k}
	}
	return out, ac
}

// needMore phrases a data-sufficiency note.
func needMore(haveHours, needHours int, what string) string {
	missing := needHours - haveHours
	if missing <= 0 {
		return ""
	}
	if missing < 48 {
		return "needs " + plural(missing, "more hour") + " of data for " + what
	}
	return "needs " + plural((missing+23)/24, "more day") + " of data for " + what
}
