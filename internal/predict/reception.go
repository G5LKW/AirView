package predict

import (
	"fmt"
	"math"
	"sort"

	"airview/internal/geo"
	"airview/internal/radio"
	"airview/internal/stat"
	"airview/internal/store"
)

// Feature columns of the reception models.
const (
	fDNDH    = iota // refractivity gradient of the lowest km, N/km
	fMinEx          // how much steeper the steepest layer below 3 km is, N/km (<= 0)
	fDuct           // strength of the strongest duct, M-units
	fNSurf          // surface refractivity, N-units
	fSpread         // dew-point spread, K
	fWind           // 10 m wind, km/h
	fPmsl           // sea-level pressure, hPa
	fHodSin         // local solar time of day
	fHodCos         //
	fTraffic        // log(1 + aircraft tracked)
	nFeat
)

var featNames = []string{"dndh", "minGradExcess", "duct", "nSurface", "dewSpread", "windKmh", "pressureMsl", "hodSin", "hodCos", "traffic"}

// Data thresholds of the reception model, in training hours (hours with
// both a range measurement and a weather observation).
const (
	recMinHours  = 72      // learned model starts to contribute
	recFullHours = 30 * 24 // learned model dominates
	recBTHours   = 120     // rolling-origin backtest
	farMinHours  = 150     // logistic far-reception model
	farMinEvents = 10
	recBlendN0   = 250 // blend weight = n / (n + recBlendN0)
)

// Reception index labels.
const (
	LabelPoor     = "poor"
	LabelNormal   = "normal"
	LabelEnhanced = "enhanced"
	LabelDucting  = "ducting likely"
)

func features(x []float64, a *Atmos, w *store.Weather, sh, aircraft float64) {
	x[fDNDH] = clamp(a.DNDH, -400, 150)
	x[fMinEx] = clamp(math.Min(0, a.MinGradient-a.DNDH), -500, 0)
	x[fDuct] = clamp(a.DuctStrengthM, 0, 60)
	x[fNSurf] = or(a.NSurface, 315)
	spread, wind, pmsl := 5.0, 10.0, 1013.0
	if w != nil {
		if fin(w.TempC) && fin(w.DewpointC) {
			spread = w.TempC - w.DewpointC
		} else if fin(w.RH) {
			spread = (100 - clamp(w.RH, 0, 100)) / 5
		}
		wind = or(w.WindKmh, wind)
		pmsl = or(w.PressureMSL, pmsl)
	}
	x[fSpread] = clamp(spread, 0, 40)
	x[fWind] = clamp(wind, 0, 150)
	x[fPmsl] = clamp(pmsl, 900, 1080)
	ang := 2 * math.Pi * sh / 24
	x[fHodSin], x[fHodCos] = math.Sin(ang), math.Cos(ang)
	if fin(aircraft) {
		x[fTraffic] = math.Log1p(math.Max(aircraft, 0))
	} else {
		x[fTraffic] = nan // imputed with the training mean by stat.Model
	}
}

// ---------------------------------------------------------------- physics

// physics is the smooth-earth baseline: line-of-sight range for the
// effective earth radius of the hour, scaled by how much of the geometric
// horizon this station really reaches.
type physics struct {
	antM  float64 // highest antenna, m AMSL
	los35 float64 // standard line of sight to 35 000 ft
	los40 float64 // standard line of sight to 40 000 ft
}

func newPhysics(antM float64) physics {
	if !fin(antM) || antM < 2 {
		antM = 2
	}
	return physics{
		antM:  antM,
		los35: radio.LineOfSightFtKm(antM, 35000, radio.KStandard),
		los40: radio.LineOfSightFtKm(antM, 40000, radio.KStandard),
	}
}

// bandK is the effective k for a path to an aircraft whose ray spends only
// the fraction s of its bending inside the surface layer the gradient was
// measured in (1 for low aircraft, ~0.2 for the flight levels).
func bandK(k, s float64) float64 {
	k = clamp(or(k, radio.KStandard), 0.6, 6)
	return radio.KStandard + (k-radio.KStandard)*s
}

// bandShare is the share s of the low-level gradient that applies to each of
// the seven altitude bands (geo.AltBand).
var bandShare = [geo.AltBands]float64{1, 1, 0.8, 0.5, 0.35, 0.25, 0.2}

func trafficFactor(ac, depth, scale float64) float64 {
	if !fin(ac) {
		return 1
	}
	return 1 - depth*math.Exp(-math.Max(ac, 0)/scale)
}

func (p physics) range95(eta float64, a *Atmos, ac float64) float64 {
	r := eta * radio.LineOfSightFtKm(p.antM, 35000, bandK(a.K, 0.2))
	if a.Ducting1090 {
		r *= 1 + math.Min(0.12, a.DuctStrengthM/100)
	}
	r = math.Min(r, 1.5*eta*p.los35)
	return r * trafficFactor(ac, 0.5, 4)
}

func (p physics) rangeMax(eta float64, a *Atmos, ac float64) float64 {
	r := eta * radio.LineOfSightFtKm(p.antM, 40000, bandK(a.K, 0.3))
	if a.Ducting1090 {
		r *= 1 + math.Min(0.3, a.DuctStrengthM/50)
	}
	r = math.Min(r, 1.6*eta*p.los40)
	r = math.Min(r, float64(store.DistHistBins*store.DistHistBinKm))
	return r * trafficFactor(ac, 0.6, 5)
}

// physFar is the physics-only probability of an anomalous long-range hour.
func physFar(a *Atmos, ac float64) float64 {
	tf := 0.8
	if fin(ac) {
		tf = 1 - math.Exp(-math.Max(ac, 0)/8)
	}
	return stat.Sigmoid((float64(a.Index)-78)/6) * tf
}

// ---------------------------------------------------------------- model

// receptionModel is the learned part of the reception forecast plus the
// station calibration of the physics baseline. Zero means "unknown" for the
// calibration numbers, so the struct is always JSON-encodable.
type receptionModel struct {
	N       int         `json:"n"`       // training hours
	NEvents int         `json:"nEvents"` // far-reception hours among them
	P95     *stat.Model `json:"p95,omitempty"`
	Max     *stat.Model `json:"max,omitempty"`
	Far     *stat.Model `json:"far,omitempty"`

	TypP95     float64 `json:"typP95"`     // median hourly p95 range, km
	SdP95      float64 `json:"sdP95"`      // robust spread of it
	TypMax     float64 `json:"typMax"`     // median hourly maximum range, km
	FarKm      float64 `json:"farKm"`      // an hour reaching this far is a "far reception event"
	CapKm      float64 `json:"capKm"`      // upper bound for learned predictions
	Eta95      float64 `json:"eta95"`      // p95 range / standard line of sight to 35 000 ft
	EtaMax     float64 `json:"etaMax"`     // max range / standard line of sight to 40 000 ft
	RefTraffic float64 `json:"refTraffic"` // median log(1+aircraft) of the training hours

	Skill    *Skill `json:"skill,omitempty"`
	FarSkill *Skill `json:"farSkill,omitempty"`
}

func (rm *receptionModel) weight() float64 {
	if rm == nil || rm.P95 == nil || rm.N < recMinHours {
		return 0
	}
	return float64(rm.N) / float64(rm.N+recBlendN0)
}

func (rm *receptionModel) farWeight() float64 {
	if rm == nil || rm.Far == nil {
		return 0
	}
	return float64(rm.NEvents) / float64(rm.NEvents+20)
}

// recRow is one training hour.
type recRow struct {
	t          int64
	x          [nFeat]float64
	p95, maxKm float64
	a          *Atmos
	ac         float64 // aircraft
	sw, sd     int
}

func trainRows(h *history, lon float64) []recRow {
	rows := make([]recRow, 0, h.n/2)
	for i := 0; i < h.n; i++ {
		if !fin(h.p95[i]) || !h.has[i] || !h.atm[i].Valid || !fin(h.aircraft[i]) {
			continue
		}
		r := recRow{t: h.t(i), p95: h.p95[i], maxKm: h.maxKm[i], a: &h.atm[i], ac: h.aircraft[i], sw: h.sW[i], sd: h.sD[i]}
		features(r.x[:], r.a, &h.wx[i], solarHour(r.t, lon), r.ac)
		rows = append(rows, r)
	}
	return rows
}

var ridgeLambdas = []float64{0.003, 0.03, 0.3, 3}

// fitReception fits the calibration and the learned models on rows (already
// restricted to what may be known). p95All / maxAll are all range
// measurements of the same period, with or without weather. withMax also
// fits the max-range model.
func fitReception(rows []recRow, p95All, maxAll []float64, ph physics, withMax bool) *receptionModel {
	rm := &receptionModel{N: len(rows), Eta95: 0.78, EtaMax: 0.95}
	pv, mv := finiteVals(p95All), finiteVals(maxAll)
	if len(pv) >= 12 {
		rm.TypP95 = stat.Median(pv)
		rm.SdP95 = or(stat.RobustStd(pv), 0)
		if rm.SdP95 < 0.03*rm.TypP95 {
			rm.SdP95 = math.Max(or(stat.Std(pv), 0), 0.03*rm.TypP95)
		}
		rm.Eta95 = clamp(rm.TypP95/ph.los35, 0.15, 1.3)
	}
	if len(mv) >= 12 {
		rm.TypMax = stat.Median(mv)
		rm.EtaMax = clamp(rm.TypMax/ph.los40, 0.2, 1.4)
		rm.CapKm = math.Min(1.3*stat.Max(mv), float64(store.DistHistBins*store.DistHistBinKm))
	}
	// Far-reception threshold: the station's own top decile, at least 10 %
	// beyond its typical hourly maximum; the smooth-earth horizon to
	// 40 000 ft while there is no history.
	rm.FarKm = ph.los40
	if len(mv) >= 100 {
		rm.FarKm = math.Max(stat.Quantile(mv, 0.9), 1.1*rm.TypMax)
	} else if rm.TypMax > 0 {
		rm.FarKm = math.Max(ph.los40*rm.EtaMax*1.15, 1.15*rm.TypMax)
	}
	if rm.CapKm == 0 {
		rm.CapKm = float64(store.DistHistBins * store.DistHistBinKm)
	}
	n := len(rows)
	if n == 0 {
		return rm
	}
	X := make([][]float64, n)
	yP := make([]float64, n)
	yM := make([]float64, n)
	yF := make([]float64, n)
	tr := make([]float64, n)
	for i := range rows {
		X[i] = rows[i].x[:]
		yP[i], yM[i] = rows[i].p95, rows[i].maxKm
		tr[i] = rows[i].x[fTraffic]
		if rows[i].maxKm >= rm.FarKm {
			yF[i] = 1
			rm.NEvents++
		}
	}
	rm.RefTraffic = or(stat.Median(tr), 0)
	if n < recMinHours {
		return rm
	}
	folds := stat.BlockFold(n, 4)
	if m, _, err := stat.RidgeCV(X, yP, featNames, ridgeLambdas, folds); err == nil && m.Validate() == nil {
		m.Cov = nil // not needed; keeps the stored model small
		rm.P95 = m
	}
	if withMax {
		if m, _, err := stat.RidgeCV(X, yM, featNames, ridgeLambdas, folds); err == nil && m.Validate() == nil {
			m.Cov = nil
			rm.Max = m
		}
	}
	if n >= farMinHours && rm.NEvents >= farMinEvents && n-rm.NEvents >= farMinEvents {
		if m, err := stat.Logistic(X, yF, featNames, &stat.LogisticOptions{Lambda: 0.02}); err == nil && m.Validate() == nil {
			m.Cov = nil
			rm.Far = m
		}
	}
	return rm
}

// recPred is the blended prediction for one hour.
type recPred struct {
	p95, sd95   float64
	maxKm, sdMx float64
	pFar        float64
	index       int
	w           float64
	physP95     float64
	learnedP95  float64 // NaN without a learned model
}

func (rm *receptionModel) predict(ph physics, x []float64, a *Atmos, ac float64, lead int) recPred {
	w := rm.weight()
	out := recPred{w: w, learnedP95: nan}
	out.physP95 = ph.range95(rm.Eta95, a, ac)
	out.p95 = out.physP95
	physMax := ph.rangeMax(rm.EtaMax, a, ac)
	out.maxKm = physMax
	grow := 1 + 0.4*float64(lead)/HorizonHours
	resid := 0.0
	if w > 0 {
		l := clamp(rm.P95.Predict(x), 0, rm.CapKm)
		if fin(l) {
			out.learnedP95 = l
			out.p95 = w*l + (1-w)*out.physP95
			resid = rm.P95.ResidStd
		} else {
			w, out.w = 0, 0
		}
	}
	out.sd95 = math.Sqrt(w*resid*resid+(1-w)*sq(0.18*out.physP95)) * grow
	residM := 0.0
	wm := 0.0
	if w > 0 && rm.Max != nil {
		if l := clamp(rm.Max.Predict(x), 0, rm.CapKm); fin(l) {
			wm = w
			out.maxKm = wm*l + (1-wm)*physMax
			residM = rm.Max.ResidStd
		}
	}
	if out.maxKm < out.p95 {
		out.maxKm = out.p95
	}
	out.sdMx = math.Sqrt(wm*residM*residM+(1-wm)*sq(0.2*physMax)) * grow

	// Probability of a far-reception hour.
	out.pFar = physFar(a, ac)
	if wl := rm.farWeight(); wl > 0 {
		if p := rm.Far.Predict(x); fin(p) {
			out.pFar = wl*p + (1-wl)*out.pFar
		}
	}
	out.pFar = clamp01(out.pFar)

	// Reception index: the propagation index of the profile, pulled towards
	// what this station's history says the same atmosphere does to its
	// range at a reference traffic level (so quiet night hours do not read
	// as bad propagation).
	idx := float64(a.Index)
	if w > 0 && rm.SdP95 > 0 && rm.TypP95 > 0 {
		var xr [nFeat]float64
		copy(xr[:], x)
		xr[fTraffic] = rm.RefTraffic
		if l := rm.P95.Predict(xr[:]); fin(l) {
			li := clamp(50+15*(l-rm.TypP95)/rm.SdP95, 0, 100)
			wi := 0.6 * w
			idx = (1-wi)*idx + wi*li
		}
	}
	out.index = int(math.Round(clamp(idx, 0, 100)))
	return out
}

func sq(v float64) float64 { return v * v }

func receptionLabel(index int, a *Atmos, pFar float64) string {
	switch {
	case index >= 85 || (a.Ducting1090 && index >= 70) || (pFar >= 0.5 && index >= 65):
		return LabelDucting
	case index >= 62:
		return LabelEnhanced
	case index >= 35:
		return LabelNormal
	default:
		return LabelPoor
	}
}

// ---------------------------------------------------------------- backtest

// backtestReception evaluates the blended reception forecast by
// rolling-origin validation with perfect-prognosis weather: for every
// origin the calibration and the models are refitted on the hours before
// the origin only, the traffic feature of the test hours is replaced by the
// hour-of-week expectation learned before the origin, and the following
// 72 hours are scored against persistence (last value before the origin)
// and seasonal-naive (same hour of the last day before the origin).
func backtestReception(rows []recRow, h *history, ph physics) (sk, farSk *Skill, pts []VerifyPoint) {
	n := len(rows)
	if n < recBTHours {
		return nil, nil, nil
	}
	span := rows[n-1].t - rows[0].t
	if span < 6*daySec {
		return nil, nil, nil
	}
	first := rows[0].t + max(span*6/10, 3*daySec)
	var origins []int64
	for o := floorHour(first); o <= rows[n-1].t; o += HorizonHours * hourSec {
		origins = append(origins, o)
	}
	if len(origins) > 16 {
		origins = origins[len(origins)-16:]
	}
	var seM, seP, seS, brM, brB float64
	var cnt, cntF, used int
	for _, o := range origins {
		k := sort.Search(n, func(i int) bool { return rows[i].t >= o })
		e := sort.Search(n, func(i int) bool { return rows[i].t >= o+HorizonHours*hourSec })
		if k < recMinHours || e <= k {
			continue
		}
		train := rows[:k:k]
		oi := h.idx(o)
		if oi < 0 {
			oi = h.n
		}
		rm := fitReception(train, h.p95[:oi:oi], h.maxKm[:oi:oi], ph, false)
		if rm.P95 == nil {
			continue
		}
		used++
		// Traffic expectation from the training hours only.
		var sumW [stat.HoursPerWeek]float64
		var cntW [stat.HoursPerWeek]int
		var sumD [stat.HoursPerDay]float64
		var cntD [stat.HoursPerDay]int
		for i := range train {
			v := train[i].x[fTraffic]
			sumW[train[i].sw] += v
			cntW[train[i].sw]++
			sumD[train[i].sd] += v
			cntD[train[i].sd]++
		}
		persist := train[k-1].p95
		var lastByHour [stat.HoursPerDay]float64
		for i := range lastByHour {
			lastByHour[i] = nan
		}
		for i := k - 1; i >= 0 && train[i].t >= o-48*hourSec; i-- {
			if hd := train[i].sd; !fin(lastByHour[hd]) {
				lastByHour[hd] = train[i].p95
			}
		}
		base := float64(rm.NEvents) / float64(rm.N)
		for i := k; i < e; i++ {
			r := &rows[i]
			x := r.x
			switch {
			case cntW[r.sw] >= 2:
				x[fTraffic] = sumW[r.sw] / float64(cntW[r.sw])
			case cntD[r.sd] >= 1:
				x[fTraffic] = sumD[r.sd] / float64(cntD[r.sd])
			default:
				x[fTraffic] = rm.RefTraffic
			}
			p := rm.predict(ph, x[:], r.a, math.Expm1(x[fTraffic]), int((r.t-o)/hourSec))
			seM += sq(p.p95 - r.p95)
			seP += sq(persist - r.p95)
			seS += sq(or(lastByHour[r.sd], persist) - r.p95)
			cnt++
			if fin(r.maxKm) {
				y := 0.0
				if r.maxKm >= rm.FarKm {
					y = 1
				}
				brM += sq(p.pFar - y)
				brB += sq(base - y)
				cntF++
			}
			pts = append(pts, VerifyPoint{T: r.t * 1000, LeadH: int((r.t-o)/hourSec) + 1, Actual: rn(r.p95, 1), Forecast: rn(p.p95, 1)})
		}
	}
	if cnt == 0 {
		return nil, nil, nil
	}
	c := float64(cnt)
	rm, rp, rs := math.Sqrt(seM/c), math.Sqrt(seP/c), math.Sqrt(seS/c)
	sk = &Skill{
		Metric: "rmse", Unit: "km", N: cnt, Origins: used, HorizonH: HorizonHours,
		Model: rn(rm, 2), Persistence: rn(rp, 2), SeasonalNaive: rn(rs, 2),
		VsPersistence: rn(stat.SkillScore(rm, rp), 3), VsSeasonalNaive: rn(stat.SkillScore(rm, rs), 3),
		Note: "p95 range; perfect-prognosis weather (observed weather as predictor), models refitted before every origin",
	}
	if cntF > 0 {
		bm, bb := brM/float64(cntF), brB/float64(cntF)
		farSk = &Skill{
			Metric: "brier", Unit: "probability^2", N: cntF, Origins: used, HorizonH: HorizonHours,
			Model: rn(bm, 4), Persistence: Num(nan), SeasonalNaive: rn(bb, 4),
			VsPersistence: Num(nan), VsSeasonalNaive: rn(stat.SkillScore(bm, bb), 3),
			Note: "far-reception probability; baseline = climatological event frequency before the origin",
		}
	}
	// Keep the chart series to the last 14 days.
	if len(pts) > 0 {
		cut := pts[len(pts)-1].T - 14*daySec*1000
		i := sort.Search(len(pts), func(i int) bool { return pts[i].T >= cut })
		pts = pts[i:]
	}
	return sk, farSk, pts
}

// ---------------------------------------------------------------- product

// Factor is one contribution to an hour's expected range.
type Factor struct {
	Key      string `json:"key"`      // refraction | duct | traffic | timeOfDay | humidity | wind | pressure
	Text     string `json:"text"`     // plain-language explanation
	EffectKm Num    `json:"effectKm"` // contribution to the expected p95 range relative to a typical hour
}

// ReceptionHour is one hour of the reception-conditions forecast.
type ReceptionHour struct {
	T     int64 `json:"t"` // start of the hour, Unix ms
	LeadH int   `json:"leadH"`
	// Source is "forecast" (Open-Meteo forecast for the hour), "observation"
	// (current analysis) or "climatology" (no weather data: standard
	// atmosphere).
	Source string `json:"source"`
	P95Km  Est    `json:"p95Km"` // expected 95th-percentile range
	MaxKm  Est    `json:"maxKm"` // expected maximum range
	// Index is the 0-100 reception index (50 = standard atmosphere) and
	// Label its class: poor | normal | enhanced | ducting likely.
	Index int    `json:"index"`
	Label string `json:"label"`
	// FarProbability is the probability (0..1) of anomalous long-range
	// reception: an hourly maximum beyond ReceptionForecast.FarKm.
	FarProbability Num      `json:"farProbability"`
	Confidence     float64  `json:"confidence"`
	Aircraft       Num      `json:"aircraft"` // expected traffic level used
	Atmosphere     Atmos    `json:"atmosphere"`
	Factors        []Factor `json:"factors"`
	Summary        string   `json:"summary"`
}

// BandOutlook is the smooth-earth line-of-sight range of one altitude band
// under the standard atmosphere and under the current refraction.
type BandOutlook struct {
	Band       int     `json:"band"`
	Label      string  `json:"label"`
	AltFt      float64 `json:"altFt"`
	StandardKm float64 `json:"standardKm"`
	NowKm      float64 `json:"nowKm"`
	Factor     float64 `json:"factor"` // NowKm / StandardKm (capped)
}

// Coefficient describes one feature of the learned range model.
type Coefficient struct {
	Feature string `json:"feature"`
	// PerUnit is the change of the p95 range in km per unit of the feature,
	// PerSD per standard deviation of the feature in the training data.
	PerUnit Num `json:"perUnit"`
	PerSD   Num `json:"perSd"`
}

// ReceptionModelInfo documents the learned part of the forecast.
type ReceptionModelInfo struct {
	TrainingHours int     `json:"trainingHours"`
	FarEvents     int     `json:"farEvents"`
	Weight        float64 `json:"weight"`    // share of the learned model in the blend, 0..1
	FarWeight     float64 `json:"farWeight"` // share of the logistic model in the far probability
	R2            Num     `json:"r2"`
	ResidStdKm    Num     `json:"residStdKm"`
	Lambda        Num     `json:"lambda"`
	// GradientEffectKm is the learned change of the p95 range when the
	// low-level refractivity gradient steepens from -40 to -120 N/km
	// (super-refraction), all else equal. Positive = the station's range
	// grows with refraction, as physics expects.
	GradientEffectKm Num           `json:"gradientEffectKm"`
	Coefficients     []Coefficient `json:"coefficients"`
}

// ReceptionActual is one observed hour for the chart.
type ReceptionActual struct {
	T     int64 `json:"t"`
	P95Km Num   `json:"p95Km"`
	MaxKm Num   `json:"maxKm"`
	Index *int  `json:"index,omitempty"` // propagation index of the observed atmosphere
}

// ReceptionForecast is product 2: reception conditions for the next 72 hours.
type ReceptionForecast struct {
	Meta
	Hours        []ReceptionHour     `json:"hours"`
	FarKm        Num                 `json:"farKm"` // threshold of a far-reception hour
	TypicalP95Km Num                 `json:"typicalP95Km"`
	TypicalMaxKm Num                 `json:"typicalMaxKm"`
	AntennaAMSLM float64             `json:"antennaAmslM"`
	Bands        []BandOutlook       `json:"bands"`
	Best         *ReceptionHour      `json:"best,omitempty"` // the most promising hour
	Model        *ReceptionModelInfo `json:"model,omitempty"`
	FarSkill     *Skill              `json:"farSkill,omitempty"`
	Recent       []ReceptionActual   `json:"recent"`
	WeatherHours int                 `json:"weatherHours"` // forecast hours backed by an Open-Meteo forecast
}

func modelInfo(rm *receptionModel) *ReceptionModelInfo {
	if rm == nil || rm.P95 == nil {
		return nil
	}
	m := rm.P95
	info := &ReceptionModelInfo{
		TrainingHours: rm.N, FarEvents: rm.NEvents, Weight: round(rm.weight(), 2), FarWeight: round(rm.farWeight(), 2),
		R2: rn(m.R2, 3), ResidStdKm: rn(m.ResidStd, 1), Lambda: Num(m.Lambda),
	}
	raw, _ := m.RawCoef()
	for j, name := range m.Features {
		c := Coefficient{Feature: name, PerUnit: Num(nan), PerSD: Num(nan)}
		if j < len(raw) {
			c.PerUnit = rn(raw[j], 4)
		}
		if j < len(m.Coef) {
			c.PerSD = rn(m.Coef[j], 2)
		}
		info.Coefficients = append(info.Coefficients, c)
	}
	info.GradientEffectKm = rn(gradientEffect(m), 1)
	return info
}

// gradientEffect is the model's p95 change for dN/dh -40 -> -120 N/km.
func gradientEffect(m *stat.Model) float64 {
	if m == nil || m.NumFeatures() != nFeat {
		return nan
	}
	var a, b [nFeat]float64
	for i := range a {
		a[i], b[i] = nan, nan // imputed with the training means
	}
	a[fDNDH], b[fDNDH] = -40, -120
	return m.Predict(b[:]) - m.Predict(a[:])
}

// explain lists the largest contributions to the hour's expected range.
func explain(rm *receptionModel, ph physics, x []float64, a *Atmos, ac float64, p recPred) []Factor {
	eff := map[string]float64{}
	// Physics part.
	std := standardAtmos()
	noDuct := *a
	noDuct.Ducting1090 = false
	wp := 1 - p.w
	full := ph.range95(rm.Eta95, a, ac)
	eff["refraction"] += wp * (ph.range95(rm.Eta95, &noDuct, ac) - ph.range95(rm.Eta95, &std, ac))
	eff["duct"] += wp * (full - ph.range95(rm.Eta95, &noDuct, ac))
	if fin(ac) {
		ref := math.Expm1(rm.RefTraffic)
		if rm.N == 0 {
			ref = 30
		}
		eff["traffic"] += wp * (full - ph.range95(rm.Eta95, a, ref))
	}
	// Learned part.
	if p.w > 0 && rm.P95 != nil && len(rm.P95.Coef) == nFeat {
		m := rm.P95
		c := func(j int) float64 {
			if !fin(x[j]) || m.Scale[j] == 0 {
				return 0
			}
			return p.w * m.Coef[j] * (x[j] - m.Mean[j]) / m.Scale[j]
		}
		eff["refraction"] += c(fDNDH) + c(fMinEx) + c(fNSurf)
		eff["duct"] += c(fDuct)
		eff["traffic"] += c(fTraffic)
		eff["timeOfDay"] += c(fHodSin) + c(fHodCos)
		eff["humidity"] += c(fSpread)
		eff["wind"] += c(fWind)
		eff["pressure"] += c(fPmsl)
	}
	keys := make([]string, 0, len(eff))
	for k, v := range eff {
		if math.Abs(v) >= 1 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		ai, aj := math.Abs(eff[keys[i]]), math.Abs(eff[keys[j]])
		if ai != aj {
			return ai > aj
		}
		return keys[i] < keys[j]
	})
	if len(keys) > 3 {
		keys = keys[:3]
	}
	out := make([]Factor, 0, len(keys))
	for _, k := range keys {
		v := eff[k]
		up := v > 0
		var text string
		switch k {
		case "refraction":
			switch {
			case a.Estimated && !a.HasProfile:
				text = fmt.Sprintf("surface refractivity %.0f N-units (no profile available)", a.NSurface)
			case up:
				text = fmt.Sprintf("stronger downward bending of radio waves than usual (dN/dh %.0f N/km, k = %s)", a.DNDH, fmtK(a.K))
			default:
				text = fmt.Sprintf("less refraction than usual (dN/dh %.0f N/km, k = %s)", a.DNDH, fmtK(a.K))
			}
		case "duct":
			if a.DuctKind != "" {
				text = fmt.Sprintf("%s duct %.0f-%.0f m (strength %.0f M-units)", a.DuctKind, a.DuctBaseM, a.DuctTopM, a.DuctStrengthM)
			} else {
				text = "no duct in the profile"
			}
		case "traffic":
			if up {
				text = fmt.Sprintf("plenty of traffic expected (about %.0f aircraft)", ac)
			} else {
				text = fmt.Sprintf("little traffic expected (about %.0f aircraft): fewer distant targets", ac)
			}
		case "timeOfDay":
			if up {
				text = "time of day that historically reaches further at this station"
			} else {
				text = "time of day that historically reaches less far at this station"
			}
		case "humidity":
			text = fmt.Sprintf("dew-point spread %.1f K", x[fSpread])
		case "wind":
			text = fmt.Sprintf("wind %.0f km/h", x[fWind])
		case "pressure":
			text = fmt.Sprintf("sea-level pressure %.0f hPa", x[fPmsl])
		}
		out = append(out, Factor{Key: k, Text: text, EffectKm: rn(v, 1)})
	}
	return out
}

func fmtK(k float64) string {
	if k >= radio.KMax {
		return "∞ (trapping)"
	}
	return fmt.Sprintf("%.2f", k)
}

func hourSummary(label string, a *Atmos, p recPred, fs []Factor, source string) string {
	var lead string
	switch label {
	case LabelDucting:
		lead = "Ducting likely"
	case LabelEnhanced:
		lead = "Enhanced range"
	case LabelPoor:
		lead = "Reduced range"
	default:
		lead = "Normal conditions"
	}
	s := fmt.Sprintf("%s: p95 range about %.0f km", lead, p.p95)
	if len(fs) > 0 {
		s += "; " + fs[0].Text
	}
	if source == "climatology" {
		s += " (no weather forecast, standard atmosphere assumed)"
	}
	return s + "."
}

// predictHour assembles one ReceptionHour.
func (f *Forecaster) predictHour(rm *receptionModel, ph physics, t int64, lead int, w *store.Weather, a Atmos, ac float64, source string, lon float64) (ReceptionHour, recPred) {
	if !a.Valid {
		a = standardAtmos()
		source = "climatology"
	}
	var x [nFeat]float64
	features(x[:], &a, w, solarHour(t, lon), ac)
	p := rm.predict(ph, x[:], &a, ac, lead)
	conf := (0.3 + 0.6*p.w) * (1 - 0.3*float64(lead)/HorizonHours)
	switch {
	case source == "climatology":
		conf *= 0.5
	case !a.HasProfile:
		conf *= 0.8
	}
	label := receptionLabel(p.index, &a, p.pFar)
	fs := explain(rm, ph, x[:], &a, ac, p)
	a.Summary = ""
	a.NSurface, a.DNDH, a.MinGradient = round(a.NSurface, 1), round(a.DNDH, 1), round(a.MinGradient, 1)
	a.MinGradientM, a.K, a.HorizonFactor = round(a.MinGradientM, 0), round(a.K, 3), round(a.HorizonFactor, 3)
	a.DuctStrengthM, a.DuctBaseM, a.DuctTopM = round(a.DuctStrengthM, 1), round(a.DuctBaseM, 0), round(a.DuctTopM, 0)
	return ReceptionHour{
		T: t * 1000, LeadH: lead, Source: source,
		P95Km: est(p.p95, p.sd95, conf, 0, true), MaxKm: est(p.maxKm, p.sdMx, conf, 0, true),
		Index: p.index, Label: label, FarProbability: rn(p.pFar, 3), Confidence: round(clamp01(conf), 2),
		Aircraft: rn(ac, 1), Atmosphere: a, Factors: fs, Summary: hourSummary(label, &a, p, fs, source),
	}, p
}

func (f *Forecaster) buildReception(h *history, m *models, fc map[int64]*store.Weather, traffic []float64, ph physics, elevM, lon float64, nowMs int64) *ReceptionForecast {
	rm := m.Reception
	if rm == nil {
		rm = fitReception(nil, nil, nil, ph, false)
	}
	out := &ReceptionForecast{
		Meta:  Meta{Product: "reception", Generated: nowMs, Level: LevelPhysics, DataHours: rm.N, Skill: rm.Skill},
		Hours: make([]ReceptionHour, 0, HorizonHours), Recent: []ReceptionActual{},
		FarKm: rn(rm.FarKm, 0), TypicalP95Km: Num(nan), TypicalMaxKm: Num(nan),
		AntennaAMSLM: round(ph.antM, 1), Model: modelInfo(rm), FarSkill: rm.FarSkill,
	}
	if rm.TypP95 > 0 {
		out.TypicalP95Km = rn(rm.TypP95, 0)
	}
	if rm.TypMax > 0 {
		out.TypicalMaxKm = rn(rm.TypMax, 0)
	}
	var nowAtmos Atmos
	bestScore := -1.0
	for k := 0; k < HorizonHours; k++ {
		t := h.cur + int64(k)*hourSec
		var w *store.Weather
		var a Atmos
		source := "climatology"
		if fw := fc[t]; fw != nil {
			if a = AnalyzeWeather(fw, elevM); a.Valid {
				w, source = fw, "forecast"
			}
		}
		if k == 0 && source == "climatology" && h.wxNow != nil && h.atNow.Valid {
			w, a, source = h.wxNow, h.atNow, "observation"
		}
		if source != "climatology" {
			out.WeatherHours++
		}
		ac := nan
		if k < len(traffic) {
			ac = traffic[k]
		}
		rh, p := f.predictHour(rm, ph, t, k, w, a, ac, source, lon)
		out.Hours = append(out.Hours, rh)
		if k == 0 {
			nowAtmos = rh.Atmosphere
		}
		if score := float64(p.index) + 30*p.pFar; score > bestScore {
			bestScore = score
			b := rh
			out.Best = &b
		}
	}

	// Line of sight per altitude band, standard versus now.
	for b := 0; b < geo.AltBands; b++ {
		alt := geo.AltBandTypicalFt(b)
		s := radio.LineOfSightFtKm(ph.antM, alt, radio.KStandard)
		nowKm := math.Min(radio.LineOfSightFtKm(ph.antM, alt, bandK(nowAtmos.K, bandShare[b])), 2*s)
		out.Bands = append(out.Bands, BandOutlook{
			Band: b, Label: geo.AltBandLabel(b), AltFt: alt,
			StandardKm: round(s, 1), NowKm: round(nowKm, 1), Factor: round(nowKm/s, 3),
		})
	}
	for i := max(h.n-48, 0); i < h.n; i++ {
		ra := ReceptionActual{T: h.t(i) * 1000, P95Km: rn(h.p95[i], 1), MaxKm: rn(h.maxKm[i], 1)}
		if h.atm[i].Valid {
			idx := h.atm[i].Index
			ra.Index = &idx
		}
		out.Recent = append(out.Recent, ra)
	}

	w := rm.weight()
	switch {
	case w == 0:
		out.Level = LevelPhysics
		out.Method = "Physics only: Open-Meteo forecast profile -> refractivity N, gradient dN/dh, k-factor and ducts (ITU-R P.453/P.834); range = line of sight for the effective earth radius per altitude band, capped, scaled by expected traffic. No learned correction yet."
		out.Notes = append(out.Notes, needMore(rm.N, recMinHours, "a learned range model (hours with both range and weather data)"))
	case rm.N < recFullHours:
		out.Level = LevelBlend
		out.Method = fmt.Sprintf("Physics baseline (line of sight for the forecast k-factor and ducts) blended %.0f %% with a ridge regression of this station's hourly range on refractivity gradient, duct strength, humidity, wind, pressure, traffic level and time of day; logistic model for far-reception hours.", 100*w)
		out.Notes = append(out.Notes, needMore(rm.N, recFullHours, "a settled learned model"))
	default:
		out.Level = LevelLearned
		out.Method = fmt.Sprintf("Ridge regression (%.0f %% weight) of this station's hourly range on forecast refractivity gradient, duct strength, humidity, wind, pressure, traffic level and time of day, blended with the line-of-sight physics baseline; logistic model for far-reception hours.", 100*w)
	}
	if out.WeatherHours == 0 {
		out.Notes = append(out.Notes, "no weather forecast available: standard atmosphere assumed, only traffic and time-of-day effects are forecast")
	} else if out.WeatherHours < HorizonHours {
		out.Notes = append(out.Notes, fmt.Sprintf("weather forecast covers %d of %d hours; the rest assumes a standard atmosphere", out.WeatherHours, HorizonHours))
	}
	if rm.Far == nil {
		out.Notes = append(out.Notes, "far-reception probability is physics-only until enough far-reception hours have been logged")
	}
	conf := 0.3 + 0.6*w
	if out.WeatherHours == 0 {
		conf *= 0.5
	}
	if rm.Skill != nil && rm.Skill.VsPersistence.Valid() && rm.Skill.VsPersistence.Float() < 0 {
		conf *= 0.8
	}
	out.setConfidence(conf)
	return out
}
