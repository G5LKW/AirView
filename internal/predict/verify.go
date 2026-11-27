package predict

import (
	"fmt"
	"math"
)

// Report statuses.
const (
	StatusOK           = "ok"           // model fitted and backtested
	StatusLearning     = "learning"     // model in use, still short of data for full trust
	StatusInsufficient = "insufficient" // not enough data, fallback in use
)

// LeadError is the backtested error at one lead time.
type LeadError struct {
	LeadH int `json:"leadH"`
	RMSE  Num `json:"rmse"`
}

// ModelReport is the verification of one learned model.
type ModelReport struct {
	Product string `json:"product"` // traffic | reception | weather
	Target  string `json:"target"`  // aircraft | msgRate | p95Km | farProbability | bias | analogue
	Title   string `json:"title"`
	Status  string `json:"status"`
	// DataHours of NeedHours usable history hours; Needs says in words what
	// is missing ("needs 5 more days ..."), empty when nothing is.
	DataHours int    `json:"dataHours"`
	NeedHours int    `json:"needHours"`
	Needs     string `json:"needs,omitempty"`
	Method    string `json:"method"`
	// Skill: rolling-origin backtest against persistence and seasonal-naive.
	Skill  *Skill      `json:"skill,omitempty"`
	ByLead []LeadError `json:"byLead,omitempty"`
	// Series is forecast versus actual for charts.
	Series []VerifyPoint `json:"series,omitempty"`
}

// Verification is product 4: how good the forecasts are.
type Verification struct {
	Generated int64         `json:"generated"`
	Fitted    int64         `json:"fitted"` // Unix ms of the model fit the report belongs to
	Summary   string        `json:"summary"`
	Models    []ModelReport `json:"models"`
	// Bias is the Open-Meteo forecast verification per variable and lead.
	Bias []BiasRow `json:"bias"`
	// Analogue compares the analogue weather forecast with persistence and
	// the logged Open-Meteo forecasts.
	Analogue []AnalogueSkill `json:"analogue"`
	// Live verifies the forecasts that were really issued.
	Live []LiveSkill `json:"live"`
}

func seriesReport(product, target, title string, m *seriesModel, pts []VerifyPoint) ModelReport {
	r := ModelReport{
		Product: product, Target: target, Title: title, Status: StatusInsufficient, NeedHours: trafficFullHours,
		Method: "Hour-of-week profile with exponential forgetting + AR(3) anomaly correction; rolling-origin backtest over 72 h horizons, refitted at every origin on earlier data only.",
		Series: pts,
	}
	if m == nil {
		r.Needs = needMore(0, trafficBTHours, "a first backtest")
		return r
	}
	r.DataHours, r.Skill = m.N, m.Skill
	switch {
	case m.N < trafficBTHours:
		r.Needs = needMore(m.N, trafficBTHours, "a first backtest")
	case m.N < trafficFullHours:
		r.Status = StatusLearning
		r.Needs = needMore(m.N, trafficFullHours, "a settled weekly profile")
	default:
		r.Status = StatusOK
	}
	for _, l := range []int{1, 3, 6, 12, 24, 48, 72} {
		if l <= len(m.HorizonRMSE) {
			r.ByLead = append(r.ByLead, LeadError{LeadH: l, RMSE: rn(m.HorizonRMSE[l-1], 3)})
		}
	}
	return r
}

func buildVerification(h *history, m *models, nowMs int64) *Verification {
	v := &Verification{Generated: nowMs, Fitted: m.Fitted * 1000, Models: []ModelReport{},
		Bias: m.Bias, Analogue: m.AnalogueVerify, Live: m.Live}
	if v.Bias == nil {
		v.Bias = []BiasRow{}
	}
	if v.Analogue == nil {
		v.Analogue = []AnalogueSkill{}
	}
	if v.Live == nil {
		v.Live = []LiveSkill{}
	}
	v.Models = append(v.Models,
		seriesReport("traffic", "aircraft", "Aircraft count", m.Aircraft, m.AircraftSeries),
		seriesReport("traffic", "msgRate", "Message rate", m.MsgRate, m.MsgRateSeries))

	rm := m.Reception
	rep := ModelReport{
		Product: "reception", Target: "p95Km", Title: "Reception range (p95)", Status: StatusInsufficient, NeedHours: recFullHours,
		Method: "Line-of-sight physics blended with a ridge regression on refractivity, ducts, weather, traffic and time of day; rolling-origin backtest over 72 h horizons with observed weather as predictor (perfect prognosis).",
		Series: m.ReceptionSeries,
	}
	far := ModelReport{
		Product: "reception", Target: "farProbability", Title: "Far-reception probability", Status: StatusInsufficient, NeedHours: farMinHours,
		Method: "Logistic regression on the same predictors, blended with a physics prior; Brier score against the climatological event frequency.",
	}
	if rm != nil {
		rep.DataHours, rep.Skill = rm.N, rm.Skill
		switch {
		case rm.P95 == nil:
			rep.Needs = needMore(rm.N, recMinHours, "a learned range model (hours with both range and weather data)")
		case rm.N < recFullHours:
			rep.Status = StatusLearning
			rep.Needs = needMore(rm.N, recFullHours, "a settled learned model")
		default:
			rep.Status = StatusOK
		}
		far.DataHours, far.Skill = rm.N, rm.FarSkill
		switch {
		case rm.Far != nil && rm.NEvents >= 3*farMinEvents:
			far.Status = StatusOK
		case rm.Far != nil:
			far.Status = StatusLearning
			far.Needs = fmt.Sprintf("%d far-reception hours logged so far; the model settles with about %d", rm.NEvents, 3*farMinEvents)
		case rm.N < farMinHours:
			far.Needs = needMore(rm.N, farMinHours, "a far-reception model")
		default:
			far.Needs = fmt.Sprintf("needs at least %d far-reception hours (beyond %.0f km); %d logged so far", farMinEvents, rm.FarKm, rm.NEvents)
		}
	} else {
		rep.Needs = needMore(0, recMinHours, "a learned range model")
		far.Needs = needMore(0, farMinHours, "a far-reception model")
	}
	v.Models = append(v.Models, rep, far)

	// Weather: bias correction and analogue method.
	pairs, appliedRows := 0, 0
	for _, b := range m.Bias {
		pairs = max(pairs, b.N)
		if b.Applied {
			appliedRows++
		}
	}
	bias := ModelReport{
		Product: "weather", Target: "bias", Title: "Forecast bias correction", Status: StatusInsufficient,
		DataHours: pairs, NeedHours: biasMinPairs,
		Method: "Mean error of past Open-Meteo forecasts per variable and lead-time class; applied only where it lowers the error on the newest 30 % of the logged forecasts.",
	}
	switch {
	case appliedRows > 0:
		bias.Status = StatusOK
	case pairs >= biasMinPairs:
		bias.Status = StatusLearning
		bias.Needs = "no systematic bias found that improves on the raw forecast"
	default:
		bias.Needs = "needs " + plural(biasMinPairs-pairs, "more verified forecast hour") + " per lead-time class"
	}
	obsHours := 0
	for _, ok := range h.has {
		if ok {
			obsHours++
		}
	}
	an := ModelReport{
		Product: "weather", Target: "analogue", Title: "Analogue weather forecast", Status: StatusInsufficient,
		DataHours: obsHours, NeedHours: analogueMinHours + 2*24,
		Method: "K nearest past states of the station's own record; verified every 6 h over the last 14 days against persistence and the logged Open-Meteo forecasts.",
	}
	switch {
	case len(m.AnalogueVerify) > 0:
		an.Status = StatusOK
		if obsHours < 60*24 {
			an.Status = StatusLearning
			an.Needs = needMore(obsHours, 60*24, "a well-stocked analogue library")
		}
		// Headline skill: temperature at 24 h.
		for _, s := range m.AnalogueVerify {
			if s.Variable == "tempC" && s.LeadH == 24 {
				an.Skill = &Skill{Metric: "mae", Unit: "K", Model: s.AnalogueMAE, Persistence: s.PersistenceMAE,
					SeasonalNaive: s.OpenMeteoMAE, VsPersistence: s.VsPersistence, VsSeasonalNaive: Num(nan),
					N: s.N, Origins: s.N, HorizonH: 24,
					Note: "temperature 24 h ahead; the seasonalNaive column holds the Open-Meteo forecast error for comparison"}
			}
		}
	default:
		an.Needs = needMore(obsHours, analogueMinHours+2*24, "the analogue forecast and its verification")
	}
	v.Models = append(v.Models, bias, an)

	ok, learning := 0, 0
	for _, r := range v.Models {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusLearning:
			learning++
		}
	}
	v.Summary = fmt.Sprintf("%d of %d models fully trained, %d learning, %d waiting for data.", ok, len(v.Models), learning, len(v.Models)-ok-learning)
	return v
}

// ---------------------------------------------------------------- nowcast

// Trends of the reception index.
const (
	TrendImproving     = "improving"
	TrendStable        = "steady"
	TrendDeteriorating = "deteriorating"
)

// NowcastPoint is the reception index of one of the next hours.
type NowcastPoint struct {
	T     int64  `json:"t"`
	Index int    `json:"index"`
	Label string `json:"label"`
}

// Nowcast is product 5: the compact summary for the top bar / home tiles.
type Nowcast struct {
	Generated int64  `json:"generated"`
	T         int64  `json:"t"`     // hour the index refers to, Unix ms
	Index     int    `json:"index"` // 0-100 reception index now
	Label     string `json:"label"` // poor | normal | enhanced | ducting likely
	// Trend over the next six hours and the index change behind it.
	Trend          string         `json:"trend"`
	TrendDelta     int            `json:"trendDelta"`
	Next6h         []NowcastPoint `json:"next6h"`
	P95Km          Est            `json:"p95Km"`          // expected p95 range this hour
	FarProbability Num            `json:"farProbability"` // highest far-reception probability of the next 6 h
	Aircraft       Num            `json:"aircraft"`       // expected aircraft this hour
	Sentence       string         `json:"sentence"`
	Confidence     string         `json:"confidence"`
	Level          string         `json:"level"`
}

func buildNowcast(r *ReceptionForecast, tr *TrafficForecast, nowMs int64) *Nowcast {
	n := &Nowcast{Generated: nowMs, Index: 50, Label: LabelNormal, Trend: TrendStable, Next6h: []NowcastPoint{},
		P95Km: Est{Value: Num(nan), Lo: Num(nan), Hi: Num(nan)}, FarProbability: Num(nan), Aircraft: Num(nan),
		Confidence: ConfLow, Level: LevelNone, Sentence: "No forecast available yet."}
	if r == nil || len(r.Hours) == 0 {
		return n
	}
	h0 := r.Hours[0]
	n.T, n.Index, n.Label, n.P95Km = h0.T, h0.Index, h0.Label, h0.P95Km
	n.Confidence, n.Level = r.Confidence, r.Level
	if tr != nil && len(tr.Hours) > 0 {
		n.Aircraft = tr.Hours[0].Aircraft.Value
	}
	last := min(6, len(r.Hours)-1)
	pf, peak, peakAt := 0.0, h0.Index, 0
	var late float64
	cnt := 0
	for k := 0; k <= last; k++ {
		hk := r.Hours[k]
		n.Next6h = append(n.Next6h, NowcastPoint{T: hk.T, Index: hk.Index, Label: hk.Label})
		if hk.FarProbability.Valid() {
			pf = math.Max(pf, hk.FarProbability.Float())
		}
		if hk.Index > peak {
			peak, peakAt = hk.Index, k
		}
		if k >= last-2 && k > 0 {
			late += float64(hk.Index)
			cnt++
		}
	}
	n.FarProbability = rn(pf, 3)
	if cnt > 0 {
		n.TrendDelta = int(math.Round(late/float64(cnt))) - h0.Index
	}
	switch {
	case n.TrendDelta >= 5:
		n.Trend = TrendImproving
	case n.TrendDelta <= -5:
		n.Trend = TrendDeteriorating
	}
	s := fmt.Sprintf("Reception %s (index %d)", h0.Label, h0.Index)
	if h0.Label == LabelDucting {
		s = fmt.Sprintf("Ducting likely now (index %d)", h0.Index)
	}
	switch n.Trend {
	case TrendImproving:
		s += fmt.Sprintf(", improving to about %d within 6 h", h0.Index+n.TrendDelta)
	case TrendDeteriorating:
		s += fmt.Sprintf(", easing to about %d within 6 h", h0.Index+n.TrendDelta)
	default:
		s += ", steady for the next 6 h"
	}
	if h0.P95Km.Value.Valid() {
		s += fmt.Sprintf("; expected range about %.0f km", h0.P95Km.Value.Float())
	}
	if peakAt > 0 && r.Hours[peakAt].Label == LabelDucting && h0.Label != LabelDucting {
		s += fmt.Sprintf(", ducting possible in %s", plural(peakAt, "hour"))
	} else if pf >= 0.3 {
		s += fmt.Sprintf(", %.0f %% chance of far reception", 100*pf)
	}
	n.Sentence = s + "."
	return n
}
