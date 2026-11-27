package stat

import "math"

// HWParams are the smoothing parameters of a Holt-Winters model.
type HWParams struct {
	Alpha float64 `json:"alpha"` // level, (0, 1)
	Beta  float64 `json:"beta"`  // trend, relative to Alpha, [0, 1)
	Gamma float64 `json:"gamma"` // season, relative to 1-Alpha, [0, 1]
	Phi   float64 `json:"phi"`   // trend damping, (0, 1]; 1 = undamped
}

// HoltWinters is the state of an additive exponential smoothing model with
// optional (damped) linear trend and optional additive season. It covers
// simple exponential smoothing (no trend, Period <= 1), Holt's linear trend
// (Period <= 1) and Holt-Winters triple smoothing (Period >= 2).
//
// One-step recursion with e = y - (Level + Phi*Trend + Season[phase]):
//
//	Level  <- Level + Phi*Trend + Alpha*e
//	Trend  <- Phi*Trend + Alpha*Beta*e
//	Season[phase] <- Season[phase] + Gamma*(1-Alpha)*e
//
// A missing observation (NaN) uses e = 0: the state is propagated without
// correction. The struct is JSON-safe and can be persisted and advanced
// later with Update.
type HoltWinters struct {
	HWParams
	Period   int       `json:"period"`   // season length; <= 1 means no season
	HasTrend bool      `json:"hasTrend"` // false: Trend stays 0
	Level    float64   `json:"level"`
	Trend    float64   `json:"trend"`
	Season   []float64 `json:"season"` // len Period; Season[i] applies to samples with index = i (mod Period), index 0 being the first fitted sample
	N        int       `json:"n"`      // samples consumed, including missing ones
	NObs     int       `json:"nObs"`   // non-missing samples consumed
	Sigma    float64   `json:"sigma"`  // std of the one-step-ahead residuals from the fit
}

func (m *HoltWinters) seasonal() bool { return m.Period >= 2 && len(m.Season) == m.Period }

// Phase returns the index into Season of the next sample to be consumed.
func (m *HoltWinters) Phase() int {
	if !m.seasonal() {
		return 0
	}
	return m.phase(0)
}

// phase returns the seasonal index of the sample k steps after the next one.
// Only valid when seasonal(); a negative N (corrupt state) cannot index out
// of range.
func (m *HoltWinters) phase(k int) int {
	p := (m.N + k) % m.Period
	if p < 0 {
		p += m.Period
	}
	return p
}

// PredictNext returns the one-step-ahead forecast.
func (m *HoltWinters) PredictNext() float64 {
	p := m.Level + m.Phi*m.Trend
	if m.seasonal() {
		p += m.Season[m.phase(0)]
	}
	return p
}

// Update consumes the next observation and returns the forecast that was
// made for it and the residual y - pred (NaN for a missing y, in which case
// the state is only propagated).
func (m *HoltWinters) Update(y float64) (pred, resid float64) {
	pred = m.PredictNext()
	e := 0.0
	resid = math.NaN()
	if IsFinite(y) {
		e = y - pred
		resid = e
		m.NObs++
	}
	m.Level += m.Phi*m.Trend + m.Alpha*e
	if m.HasTrend {
		m.Trend = m.Phi*m.Trend + m.Alpha*m.Beta*e
	}
	if m.seasonal() {
		m.Season[m.phase(0)] += m.Gamma * (1 - m.Alpha) * e
	}
	m.N++
	return pred, resid
}

// Forecast returns the point forecasts for the next h steps (nil for h < 1).
func (m *HoltWinters) Forecast(h int) []float64 {
	if h < 1 {
		return nil
	}
	out := make([]float64, h)
	damp := 0.0
	pw := 1.0
	for k := 1; k <= h; k++ {
		pw *= m.Phi
		damp += pw // phi + phi^2 + ... + phi^k
		v := m.Level + damp*m.Trend
		if m.seasonal() {
			v += m.Season[m.phase(k-1)]
		}
		out[k-1] = v
	}
	return out
}

// ForecastStd returns the standard deviation of the forecast error for
// horizons 1..h, derived from the one-step residual variance Sigma^2 and the
// model's error-propagation weights (the standard formula for additive
// exponential smoothing): var_k = Sigma^2 * (1 + sum_{j=1}^{k-1} c_j^2) with
// c_j = Alpha*(1 + Beta*(Phi+...+Phi^j)) + Gamma*(1-Alpha)*[j mod Period == 0].
func (m *HoltWinters) ForecastStd(h int) []float64 {
	if h < 1 {
		return nil
	}
	out := make([]float64, h)
	acc := 1.0
	damp, pw := 0.0, 1.0
	for k := 1; k <= h; k++ {
		out[k-1] = m.Sigma * math.Sqrt(acc)
		// c_k contributes to horizons > k.
		pw *= m.Phi
		damp += pw
		c := m.Alpha
		if m.HasTrend {
			c += m.Alpha * m.Beta * damp
		}
		if m.seasonal() && k%m.Period == 0 {
			c += m.Gamma * (1 - m.Alpha)
		}
		acc += c * c
	}
	return out
}

// ForecastInterval returns point forecasts for the next h steps with
// two-sided normal prediction intervals at coverage level (invalid: 0.95).
func (m *HoltWinters) ForecastInterval(h int, level float64) (mean, lo, hi []float64) {
	mean = m.Forecast(h)
	if mean == nil {
		return nil, nil, nil
	}
	sd := m.ForecastStd(h)
	z := intervalZ(level)
	lo = make([]float64, h)
	hi = make([]float64, h)
	for i := range mean {
		lo[i] = mean[i] - z*sd[i]
		hi[i] = mean[i] + z*sd[i]
	}
	return
}

// HWOptions configures FitHoltWinters. The zero value (or nil) fits level,
// undamped trend and (for period >= 2) season, optimising all parameters.
type HWOptions struct {
	NoTrend bool      // drop the trend component
	Damped  bool      // fit a damped trend (Phi in [0.8, 0.995]); ignored with NoTrend
	Params  *HWParams // fixed parameters: skip the optimisation entirely
	MaxIter int       // Nelder-Mead iteration budget; default 300
}

// HWFit is the result of FitHoltWinters.
type HWFit struct {
	Model     *HoltWinters // state after the last sample, ready to Forecast/Update
	Fitted    []float64    // one-step-ahead forecasts for every sample (in-sample: the initial state and the parameters were estimated from the same series; use Backtest for honest errors)
	Residuals []float64    // y - Fitted; NaN where y is missing
	SSE       float64      // sum of squared one-step residuals
	NObs      int          // residuals in SSE
	Opt       OptResult    // optimiser report (zero when Params were fixed)
}

// hwInit derives the initial level (one step before the first sample),
// trend and seasonal indices from the head of the series.
func hwInit(y []float64, period int, trend bool) (level, slope float64, season []float64, err error) {
	if period < 2 {
		// Line through the first (up to 10) valid points.
		var ts, vs []float64
		for i, v := range y {
			if IsFinite(v) {
				ts = append(ts, float64(i))
				vs = append(vs, v)
				if len(vs) == 10 {
					break
				}
			}
		}
		if len(vs) < 2 {
			return 0, 0, nil, ErrInsufficientData
		}
		if !trend {
			k := min(len(vs), 5)
			return Mean(vs[:k]), 0, nil, nil
		}
		mt, mv := Mean(ts), Mean(vs)
		var stt, stv float64
		for i := range ts {
			stt += sq(ts[i] - mt)
			stv += (ts[i] - mt) * (vs[i] - mv)
		}
		slope = stv / stt
		return mv - slope*(mt+1), slope, nil, nil
	}

	cycles := len(y) / period
	if cycles < 2 {
		return 0, 0, nil, ErrInsufficientData
	}
	cycles = min(cycles, 8)
	mid := float64(period-1) / 2

	// Decompose the head of the series as
	//
	//	y[j*period+i] = lvl[j] + slope*(i-mid) + season[i]
	//
	// (lvl[j] = level at the centre of cycle j, season with zero mean). The
	// starting point is the plain mean of every cycle. With gaps that mean
	// is biased towards the phases that happen to be present (a cycle that
	// lost its night hours looks too high), which would leak into the
	// seasonal indices and the trend. The levels and the seasonal indices
	// are therefore re-estimated in turn (backfitting) until they agree.
	// Without missing values the first pass is already the fixed point.
	lvl := make([]float64, cycles) // NaN for a cycle without data
	nValid := 0
	for j := range lvl {
		sum, c := 0.0, 0
		for _, v := range y[j*period : (j+1)*period] {
			if IsFinite(v) {
				sum += v
				c++
			}
		}
		if c == 0 {
			lvl[j] = math.NaN()
			continue
		}
		lvl[j] = sum / float64(c)
		nValid++
	}
	if nValid < 2 {
		return 0, 0, nil, ErrInsufficientData
	}
	// Per-sample slope from a line through the cycle levels.
	fitSlope := func() float64 {
		if !trend {
			return 0
		}
		var mj, mm float64
		for j, v := range lvl {
			if v == v {
				mj += float64(j)
				mm += v
			}
		}
		mj /= float64(nValid)
		mm /= float64(nValid)
		var sjj, sjm float64
		for j, v := range lvl {
			if v == v {
				sjj += sq(float64(j) - mj)
				sjm += (float64(j) - mj) * (v - mm)
			}
		}
		return sjm / sjj / float64(period)
	}
	season = make([]float64, period)
	cnt := make([]int, period)
	const maxPasses = 60
	for pass := 0; ; pass++ {
		slope = fitSlope()
		// Seasonal indices given the levels.
		for i := range season {
			season[i], cnt[i] = 0, 0
		}
		for j, l := range lvl {
			if l != l {
				continue
			}
			for i := 0; i < period; i++ {
				if v := y[j*period+i]; IsFinite(v) {
					season[i] += v - (l + slope*(float64(i)-mid))
					cnt[i]++
				}
			}
		}
		sum, defined := 0.0, 0
		for i := range season {
			if cnt[i] > 0 {
				season[i] /= float64(cnt[i])
				sum += season[i]
				defined++
			}
		}
		if defined == 0 {
			return 0, 0, nil, ErrInsufficientData
		}
		// Normalise the indices to zero mean and move the offset into the levels.
		off := sum / float64(defined)
		for i := range season {
			if cnt[i] > 0 {
				season[i] -= off
			}
		}
		scale := 0.0
		for j := range lvl {
			lvl[j] += off // NaN stays NaN
			if a := math.Abs(lvl[j]); a > scale {
				scale = a
			}
		}
		if pass == maxPasses-1 {
			break
		}
		// Levels given the seasonal indices.
		change := 0.0
		for j, l := range lvl {
			if l != l {
				continue
			}
			s, c := 0.0, 0
			for i := 0; i < period; i++ {
				if v := y[j*period+i]; IsFinite(v) && cnt[i] > 0 {
					s += v - season[i] - slope*(float64(i)-mid)
					c++
				}
			}
			if c == 0 {
				continue
			}
			nl := s / float64(c)
			change = math.Max(change, math.Abs(nl-l))
			lvl[j] = nl
		}
		if !(change > 1e-13*scale) {
			break
		}
	}
	// Level one step before sample 0: the first valid cycle level, moved
	// back along the trend from that cycle's centre.
	for j, l := range lvl {
		if l == l {
			level = l - slope*(float64(j)*float64(period)+mid+1)
			break
		}
	}
	return level, slope, season, nil
}

// hwSSE runs the recursion over y from the given initial state and returns
// the one-step-ahead SSE. season is used as scratch state (overwritten).
// fitted/resid may be nil.
func hwSSE(y []float64, m *HoltWinters, fitted, resid []float64) (sse float64) {
	for i, v := range y {
		pred, e := m.Update(v)
		if fitted != nil {
			fitted[i] = pred
			resid[i] = e
		}
		if e == e {
			sse += e * e
		}
	}
	return sse
}

// FitHoltWinters fits additive exponential smoothing to a regularly sampled
// series. period is the season length (24 for a daily cycle of hourly data,
// 168 for a weekly one); period <= 1 fits a non-seasonal model (Holt's
// linear trend, or simple exponential smoothing with NoTrend).
//
// Missing values (NaN) are allowed anywhere; the model state is carried
// through them. The initial state comes from a decomposition of the first
// cycles (up to 8) into cycle levels, trend and seasonal indices, estimated
// jointly so that gaps in those cycles do not bias it. The smoothing parameters minimise the one-step-ahead
// sum of squared errors: the best point of a coarse grid seeds a bounded
// Nelder-Mead search. Model.Sigma is the residual standard deviation used
// for prediction intervals.
//
// Errors: ErrInsufficientData (fewer than two full seasons, or fewer than 3
// valid samples), ErrInvalidArgument (fixed Params out of range).
func FitHoltWinters(y []float64, period int, opt *HWOptions) (*HWFit, error) {
	o := HWOptions{}
	if opt != nil {
		o = *opt
	}
	if period < 2 {
		period = 0
	}
	finite := 0
	for _, v := range y {
		if IsFinite(v) {
			finite++
		}
	}
	if finite < 3 { // infinities are missing values, like NaN
		return nil, ErrInsufficientData
	}
	trend := !o.NoTrend
	damped := trend && o.Damped
	level0, slope0, season0, err := hwInit(y, period, trend)
	if err != nil {
		return nil, err
	}
	if !IsFinite(level0) || !IsFinite(slope0) {
		return nil, ErrDegenerate
	}

	scratch := &HoltWinters{Period: period, HasTrend: trend, Season: make([]float64, len(season0))}
	reset := func(p HWParams) {
		scratch.HWParams = p
		scratch.Level, scratch.Trend = level0, slope0
		scratch.N, scratch.NObs = 0, 0
		copy(scratch.Season, season0)
	}
	// Parameter vector layout: alpha, [beta], [phi], [gamma].
	unpack := func(x []float64) HWParams {
		p := HWParams{Alpha: x[0], Phi: 1}
		i := 1
		if trend {
			p.Beta = x[i]
			i++
		}
		if damped {
			p.Phi = x[i]
			i++
		}
		if period >= 2 {
			p.Gamma = x[i]
		}
		return p
	}
	objective := func(x []float64) float64 {
		reset(unpack(x))
		return hwSSE(y, scratch, nil, nil)
	}

	fit := &HWFit{}
	var params HWParams
	nParams := 0
	if o.Params != nil {
		params = *o.Params
		if !trend {
			params.Beta = 0
		}
		if !damped {
			params.Phi = 1
		}
		if period < 2 {
			params.Gamma = 0
		}
		if !(params.Alpha > 0 && params.Alpha <= 1) || !(params.Beta >= 0 && params.Beta <= 1) ||
			!(params.Gamma >= 0 && params.Gamma <= 1) || !(params.Phi > 0 && params.Phi <= 1) {
			return nil, ErrInvalidArgument
		}
	} else {
		cand := [][]float64{{0.05, 0.2, 0.5, 0.8}}
		lower := []float64{1e-4}
		upper := []float64{0.9999}
		if trend {
			cand = append(cand, []float64{0.01, 0.1, 0.3})
			lower = append(lower, 0)
			upper = append(upper, 0.9999)
		}
		if damped {
			cand = append(cand, []float64{0.9, 0.98})
			lower = append(lower, 0.8)
			upper = append(upper, 0.995)
		}
		if period >= 2 {
			cand = append(cand, []float64{0.05, 0.2, 0.5})
			lower = append(lower, 0)
			upper = append(upper, 1)
		}
		nParams = len(cand)
		// Coarse grid over the candidate lists.
		x := make([]float64, nParams)
		best := make([]float64, nParams)
		bestF := math.Inf(1)
		idx := make([]int, nParams)
		evals := 0
		for {
			for i := range x {
				x[i] = cand[i][idx[i]]
			}
			evals++
			if f := objective(x); f < bestF {
				bestF = f
				copy(best, x)
			}
			d := 0
			for d < nParams {
				idx[d]++
				if idx[d] < len(cand[d]) {
					break
				}
				idx[d] = 0
				d++
			}
			if d == nParams {
				break
			}
		}
		if math.IsInf(bestF, 1) {
			return nil, ErrDegenerate
		}
		maxIter := o.MaxIter
		if maxIter <= 0 {
			maxIter = 300
		}
		res, err := NelderMead(objective, best, &NMOptions{
			Lower: lower, Upper: upper, MaxIter: maxIter, TolF: 1e-9, TolX: 1e-5, Restarts: -1,
		})
		if err != nil {
			return nil, err
		}
		res.Evals += evals
		fit.Opt = res
		params = unpack(res.X)
	}

	model := &HoltWinters{
		HWParams: params, Period: period, HasTrend: trend,
		Level: level0, Trend: slope0, Season: append([]float64(nil), season0...),
	}
	fit.Fitted = make([]float64, len(y))
	fit.Residuals = make([]float64, len(y))
	fit.SSE = hwSSE(y, model, fit.Fitted, fit.Residuals)
	fit.NObs = model.NObs
	if !IsFinite(fit.SSE) || !IsFinite(model.Level) || !IsFinite(model.Trend) {
		return nil, ErrDegenerate
	}
	dof := fit.NObs - nParams
	if dof < 1 {
		dof = 1
	}
	model.Sigma = math.Sqrt(fit.SSE / float64(dof))
	fit.Model = model
	return fit, nil
}

// FitHolt fits Holt's linear-trend exponential smoothing (no season). It is
// FitHoltWinters with period 0.
func FitHolt(y []float64, opt *HWOptions) (*HWFit, error) {
	return FitHoltWinters(y, 0, opt)
}
