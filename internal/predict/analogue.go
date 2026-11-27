package predict

import (
	"fmt"
	"math"
	"sort"

	"airview/internal/stat"
	"airview/internal/store"
)

const (
	analogueMinHours = 10 * 24 // observation hours before analogues are searched
	analogueK        = 8       // analogues used
	analogueH        = 24      // forecast length, hours
	analogueSep      = 12      // minimum spacing of two analogues, hours
	analogueDim      = 11
)

// AnalogueHour is one hour of the analogue forecast.
type AnalogueHour struct {
	T           int64 `json:"t"`
	LeadH       int   `json:"leadH"`
	TempC       Est   `json:"tempC"`
	PressureMSL Est   `json:"pressureMsl"`
	WindKmh     Est   `json:"windKmh"`
	Cloud       Num   `json:"cloud"`      // %
	PrecipProb  Num   `json:"precipProb"` // 0..1: share of the analogues with precipitation in that hour
	// Persistence is the "no change" temperature / pressure for comparison.
	PersistTempC Num `json:"persistTempC"`
}

// AnalogueMatch is one past state that resembled the present.
type AnalogueMatch struct {
	T        int64   `json:"t"`        // Unix ms of the past state
	Distance float64 `json:"distance"` // standardised distance (0 = identical)
	// What followed in the next 24 hours.
	TempChangeC    Num `json:"tempChangeC"`
	PressureChange Num `json:"pressureChange"`
	PrecipMM       Num `json:"precipMm"`
}

// AnalogueSkill verifies the analogue method at one lead time.
type AnalogueSkill struct {
	Variable       string `json:"variable"` // tempC | pressureMsl | windKmh
	LeadH          int    `json:"leadH"`
	N              int    `json:"n"`
	AnalogueMAE    Num    `json:"analogueMae"`
	PersistenceMAE Num    `json:"persistenceMae"`
	VsPersistence  Num    `json:"vsPersistence"` // 1 - MAE(analogue)/MAE(persistence)
	// OpenMeteoMAE is the error of the logged Open-Meteo forecast at the
	// same lead over the NOpenMeteo of those hours that have one.
	OpenMeteoMAE Num `json:"openMeteoMae"`
	NOpenMeteo   int `json:"nOpenMeteo"`
}

// AnalogueForecast is product 3d: what followed the K past states that most
// resembled the present one.
type AnalogueForecast struct {
	Method       string          `json:"method"`
	K            int             `json:"k"`
	Candidates   int             `json:"candidates"` // past states searched
	BaseT        int64           `json:"baseT"`      // Unix ms of the state the forecast starts from
	Hours        []AnalogueHour  `json:"hours"`
	Analogues    []AnalogueMatch `json:"analogues"`
	Summary      string          `json:"summary"`
	Verification []AnalogueSkill `json:"verification"`
}

// analogueIndex holds the standardised state vector of every history hour.
// Fixed physical scales are used (no statistics of the record), so a state
// vector never depends on data after its own hour.
type analogueIndex struct {
	h   *history
	z   []float64 // n * analogueDim
	ok  []bool
	p   []float64 // sea-level pressure per hour (NaN missing)
	lon float64
}

func newAnalogueIndex(h *history, rx rxInfo) *analogueIndex {
	ix := &analogueIndex{h: h, z: make([]float64, h.n*analogueDim), ok: make([]bool, h.n), p: nanSlice(h.n), lon: rx.lon}
	for i := 0; i < h.n; i++ {
		if h.has[i] {
			ix.p[i] = pmslOf(&h.wx[i], rx.elevM)
		}
	}
	for i := 3; i < h.n; i++ {
		if !h.has[i] {
			continue
		}
		w := &h.wx[i]
		if !fin(ix.p[i]) || !fin(ix.p[i-3]) || !fin(w.TempC) || !fin(w.WindKmh) || !fin(w.WindDir) {
			continue
		}
		u, v := stat.WindToUV(w.WindKmh, w.WindDir)
		t := h.t(i)
		ha := 2 * math.Pi * solarHour(t, rx.lon) / 24
		da := 2 * math.Pi * float64((t/daySec)%365) / 365.25
		z := ix.z[i*analogueDim : (i+1)*analogueDim]
		z[0] = (ix.p[i] - 1013) / 8
		z[1] = (ix.p[i] - ix.p[i-3]) / 1.0
		z[2] = u / 12
		z[3] = v / 12
		z[4] = (or(w.RH, 70) - 70) / 15
		z[5] = w.TempC / 6
		z[6], z[7] = 0.7*math.Sin(ha), 0.7*math.Cos(ha)
		z[8], z[9] = 0.7*math.Sin(da), 0.7*math.Cos(da)
		z[10] = (or(w.Cloud, 50) - 50) / 80
		ix.ok[i] = true
	}
	return ix
}

type analogueHit struct {
	j  int
	d2 float64
}

// find returns the K nearest past states of hour i whose following
// analogueH hours end at or before hour limit (pass limit = i for a real
// forecast; in a backtest this is what guarantees that nothing after the
// origin is used), at least analogueSep hours apart. candidates is the
// number of states searched.
func (ix *analogueIndex) find(i, limit, k int) (hits []analogueHit, candidates int) {
	if i < 0 || i >= len(ix.ok) || !ix.ok[i] {
		return nil, 0
	}
	zi := ix.z[i*analogueDim : (i+1)*analogueDim]
	last := min(limit-analogueH, i-analogueH)
	all := make([]analogueHit, 0, max(last, 0))
	for j := 3; j <= last; j++ {
		if !ix.ok[j] {
			continue
		}
		zj := ix.z[j*analogueDim : (j+1)*analogueDim]
		d2 := 0.0
		for c := 0; c < analogueDim; c++ {
			d := zi[c] - zj[c]
			d2 += d * d
		}
		all = append(all, analogueHit{j, d2})
	}
	candidates = len(all)
	sort.Slice(all, func(a, b int) bool {
		if all[a].d2 != all[b].d2 {
			return all[a].d2 < all[b].d2
		}
		return all[a].j > all[b].j
	})
	for _, c := range all {
		if len(hits) >= k {
			break
		}
		near := false
		for _, x := range hits {
			if d := x.j - c.j; d > -analogueSep && d < analogueSep {
				near = true
				break
			}
		}
		if !near {
			hits = append(hits, c)
		}
	}
	return hits, candidates
}

// project forecasts variable get at lead hours after hour i as the value at
// i plus the weighted mean change that followed the analogues; sd is the
// weighted spread of those changes.
func (ix *analogueIndex) project(i int, hits []analogueHit, lead int, get func(w *store.Weather) float64) (v, sd float64) {
	h := ix.h
	base := get(&h.wx[i])
	if !fin(base) {
		return nan, nan
	}
	var sw, sx, sxx float64
	n := 0
	for _, c := range hits {
		j := c.j
		if j+lead >= h.n || !h.has[j+lead] || !h.has[j] {
			continue
		}
		a, b := get(&h.wx[j]), get(&h.wx[j+lead])
		if !fin(a) || !fin(b) {
			continue
		}
		w := 1 / (c.d2 + 0.05)
		d := b - a
		sw += w
		sx += w * d
		sxx += w * d * d
		n++
	}
	if n < 3 {
		return nan, nan
	}
	mean := sx / sw
	return base + mean, math.Sqrt(math.Max(sxx/sw-mean*mean, 0))
}

func getTemp(w *store.Weather) float64 { return w.TempC }
func getWind(w *store.Weather) float64 { return w.WindKmh }

func (ix *analogueIndex) getP(elevM float64) func(w *store.Weather) float64 {
	return func(w *store.Weather) float64 { return pmslOf(w, elevM) }
}

// analogueForecast builds product 3d, or nil while the record is too short.
func analogueForecast(h *history, m *models, rx rxInfo, off int) *AnalogueForecast {
	nObs := 0
	for _, ok := range h.has {
		if ok {
			nObs++
		}
	}
	if nObs < analogueMinHours {
		return nil
	}
	ix := newAnalogueIndex(h, rx)
	i0 := -1
	for i := h.n - 1; i >= 0 && i >= h.n-3; i-- {
		if ix.ok[i] {
			i0 = i
			break
		}
	}
	if i0 < 0 {
		return nil
	}
	hits, cand := ix.find(i0, i0, analogueK)
	if len(hits) < 3 {
		return nil
	}
	getP := ix.getP(rx.elevM)
	out := &AnalogueForecast{
		Method: fmt.Sprintf("Analogue method: the %d past hours of this station's record most similar to now (sea-level pressure, 3 h tendency, wind vector, humidity, temperature, cloud, time of day and season), at least %d h apart; the forecast is the present state plus the weighted mean change that followed them, the interval their spread.", len(hits), analogueSep),
		K:      len(hits), Candidates: cand, BaseT: h.t(i0) * 1000,
		Hours: make([]AnalogueHour, 0, analogueH), Analogues: make([]AnalogueMatch, 0, len(hits)),
		Verification: m.AnalogueVerify,
	}
	if out.Verification == nil {
		out.Verification = []AnalogueSkill{}
	}
	conf := clamp01(0.25 + 0.35*float64(cand)/float64(cand+2000))
	baseT := getTemp(&h.wx[i0])
	wetAny := 0.0
	for lead := 1; lead <= analogueH; lead++ {
		tv, tsd := ix.project(i0, hits, lead, getTemp)
		pv, psd := ix.project(i0, hits, lead, getP)
		wv, wsd := ix.project(i0, hits, lead, getWind)
		if fin(wv) && wv < 0 {
			wv = 0
		}
		var sw, cl, clw, wet float64
		for _, c := range hits {
			j := c.j + lead
			if j >= h.n || !h.has[j] {
				continue
			}
			w := 1 / (c.d2 + 0.05)
			sw += w
			if fin(h.wx[j].Cloud) {
				cl += w * h.wx[j].Cloud
				clw += w
			}
			if h.wx[j].PrecipMM > 0.1 {
				wet += w
			}
		}
		ah := AnalogueHour{
			T: (h.t(i0) + int64(lead)*hourSec) * 1000, LeadH: lead,
			TempC: est(tv, tsd+0.5, conf, 1, false), PressureMSL: est(pv, psd+0.3, conf, 1, false),
			WindKmh: est(wv, wsd+1, conf, 1, true), Cloud: Num(nan), PrecipProb: Num(nan), PersistTempC: rn(baseT, 1),
		}
		if clw > 0 {
			ah.Cloud = rn(cl/clw, 0)
		}
		if sw > 0 {
			ah.PrecipProb = rn(wet/sw, 2)
			wetAny = math.Max(wetAny, wet/sw)
		}
		out.Hours = append(out.Hours, ah)
	}
	for _, c := range hits {
		j := c.j
		am := AnalogueMatch{T: h.t(j) * 1000, Distance: round(math.Sqrt(c.d2), 3),
			TempChangeC: Num(nan), PressureChange: Num(nan), PrecipMM: Num(nan)}
		if e := j + analogueH; e < h.n && h.has[e] {
			am.TempChangeC = rn(getTemp(&h.wx[e])-getTemp(&h.wx[j]), 1)
			am.PressureChange = rn(getP(&h.wx[e])-getP(&h.wx[j]), 1)
			sum, any := 0.0, false
			for q := j + 1; q <= e; q++ {
				if h.has[q] && fin(h.wx[q].PrecipMM) {
					sum += h.wx[q].PrecipMM
					any = true
				}
			}
			if any {
				am.PrecipMM = rn(sum, 1)
			}
		}
		out.Analogues = append(out.Analogues, am)
	}
	// One-sentence summary from the 24 h projection.
	lastH := out.Hours[len(out.Hours)-1]
	var s string
	if lastH.TempC.Value.Valid() && fin(baseT) {
		d := lastH.TempC.Value.Float() - baseT
		switch {
		case d >= 1.5:
			s = fmt.Sprintf("After similar situations it was on average %.1f K warmer a day later", d)
		case d <= -1.5:
			s = fmt.Sprintf("After similar situations it was on average %.1f K colder a day later", -d)
		default:
			s = "After similar situations the temperature a day later was about the same"
		}
		if bp := getP(&h.wx[i0]); lastH.PressureMSL.Value.Valid() && fin(bp) {
			dp := lastH.PressureMSL.Value.Float() - bp
			switch {
			case dp >= 2:
				s += fmt.Sprintf(", with pressure %.0f hPa higher", dp)
			case dp <= -2:
				s += fmt.Sprintf(", with pressure %.0f hPa lower", -dp)
			}
		}
		s += fmt.Sprintf("; precipitation followed in up to %.0f %% of the %d analogues.", 100*wetAny, len(hits))
	}
	out.Summary = s
	return out
}

// verifyAnalogue backtests the analogue method over the last 14 days of the
// record (an origin every 6 hours; analogues are drawn only from states
// whose 24 h sequel ended before the origin) against persistence and, where
// logged, the Open-Meteo forecast for the same hour and lead.
func verifyAnalogue(h *history, rx rxInfo, om map[omKey][3]float64) []AnalogueSkill {
	nObs := 0
	for _, ok := range h.has {
		if ok {
			nObs++
		}
	}
	if nObs < analogueMinHours+2*24 {
		return nil
	}
	ix := newAnalogueIndex(h, rx)
	getP := ix.getP(rx.elevM)
	gets := [3]func(w *store.Weather) float64{getTemp, getP, getWind}
	names := [3]string{"tempC", "pressureMsl", "windKmh"}
	type acc struct {
		n, nOM        int
		an, pe, omAbs float64
	}
	var a [3][len(analogueLeads)]acc
	for o := h.n - 1 - analogueH; o >= analogueMinHours && o >= h.n-1-analogueH-14*24; o -= 6 {
		if !ix.ok[o] {
			continue
		}
		hits, _ := ix.find(o, o, analogueK)
		if len(hits) < 3 {
			continue
		}
		for li, lead := range analogueLeads {
			tgt := o + lead
			if tgt >= h.n || !h.has[tgt] {
				continue
			}
			for v := 0; v < 3; v++ {
				actual, base := gets[v](&h.wx[tgt]), gets[v](&h.wx[o])
				fc, _ := ix.project(o, hits, lead, gets[v])
				if !fin(actual) || !fin(base) || !fin(fc) {
					continue
				}
				c := &a[v][li]
				c.n++
				c.an += math.Abs(fc - actual)
				c.pe += math.Abs(base - actual)
				if x, ok := om[omKey{h.t(tgt), lead}]; ok && fin(x[v]) {
					c.nOM++
					c.omAbs += math.Abs(x[v] - actual)
				}
			}
		}
	}
	var out []AnalogueSkill
	for v := 0; v < 3; v++ {
		for li, lead := range analogueLeads {
			c := &a[v][li]
			if c.n < 8 {
				continue
			}
			n := float64(c.n)
			s := AnalogueSkill{Variable: names[v], LeadH: lead, N: c.n,
				AnalogueMAE: rn(c.an/n, 3), PersistenceMAE: rn(c.pe/n, 3),
				VsPersistence: rn(stat.SkillScore(c.an/n, c.pe/n), 3), OpenMeteoMAE: Num(nan), NOpenMeteo: c.nOM}
			if c.nOM > 0 {
				s.OpenMeteoMAE = rn(c.omAbs/float64(c.nOM), 3)
			}
			out = append(out, s)
		}
	}
	return out
}
