package stat

import "math"

// errSums accumulates forecast error statistics over complete pairs. The
// moments needed for the correlation are kept as running means and centred
// (co-)moments (Welford), not as raw sums of squares: with raw sums a series
// far from zero (pressure in Pa, a Unix time) loses its variance to
// cancellation and the correlation would be reported as 0.
type errSums struct {
	n, nPct       int
	abs, sqr, sum float64
	pct, smape    float64
	ma, mp        float64 // running means of actual and forecast
	caa, cpp, cxy float64 // centred sums of squares and cross products
}

func (s *errSums) add(actual, pred float64) {
	if !IsFinite(actual) || !IsFinite(pred) {
		return
	}
	e := pred - actual
	s.n++
	s.abs += math.Abs(e)
	s.sqr += e * e
	s.sum += e
	if actual != 0 {
		s.pct += math.Abs(e / actual)
		s.nPct++
	}
	if d := math.Abs(actual) + math.Abs(pred); d > 0 {
		s.smape += 2 * math.Abs(e) / d
	}
	fn := float64(s.n)
	da, dp := actual-s.ma, pred-s.mp
	s.ma += da / fn
	s.mp += dp / fn
	s.caa += da * (actual - s.ma)
	s.cpp += dp * (pred - s.mp)
	s.cxy += da * (pred - s.mp)
}

func (s *errSums) addAll(actual, pred []float64) {
	n := min(len(actual), len(pred))
	for i := 0; i < n; i++ {
		s.add(actual[i], pred[i])
	}
}

// Metrics is a JSON-safe bundle of forecast accuracy measures over the
// pairwise-complete (actual, forecast) points. Every field is 0 when N is 0.
type Metrics struct {
	N     int     `json:"n"`     // points scored
	MAE   float64 `json:"mae"`   // mean absolute error
	RMSE  float64 `json:"rmse"`  // root mean squared error
	Bias  float64 `json:"bias"`  // mean of forecast - actual (positive: over-forecast)
	MAPE  float64 `json:"mape"`  // mean |error/actual| in percent over points with actual != 0
	NPct  int     `json:"nPct"`  // points entering MAPE
	SMAPE float64 `json:"smape"` // symmetric MAPE in percent, 0..200
	Corr  float64 `json:"corr"`  // Pearson correlation of forecast and actual (0 if undefined)
}

func (s *errSums) metrics() Metrics {
	if s.n == 0 {
		return Metrics{}
	}
	fn := float64(s.n)
	m := Metrics{N: s.n, MAE: s.abs / fn, RMSE: math.Sqrt(s.sqr / fn), Bias: s.sum / fn, NPct: s.nPct}
	if s.nPct > 0 {
		m.MAPE = 100 * s.pct / float64(s.nPct)
	}
	m.SMAPE = 100 * s.smape / fn
	// A spread below 1e-12 of the mean is rounding noise, not variance.
	if s.caa > fn*sq(1e-12*s.ma) && s.cpp > fn*sq(1e-12*s.mp) {
		if c := s.cxy / math.Sqrt(s.caa*s.cpp); IsFinite(c) {
			m.Corr = clamp(c, -1, 1)
		}
	}
	return m
}

// Evaluate scores forecasts against actual values over the indices where
// both are finite. If the slices differ in length the common prefix is used.
func Evaluate(actual, pred []float64) Metrics {
	var s errSums
	s.addAll(actual, pred)
	return s.metrics()
}

// MAE returns the mean absolute error over pairwise-complete points, NaN if
// none.
func MAE(actual, pred []float64) float64 {
	var s errSums
	s.addAll(actual, pred)
	if s.n == 0 {
		return math.NaN()
	}
	return s.abs / float64(s.n)
}

// MSE returns the mean squared error over pairwise-complete points, NaN if
// none.
func MSE(actual, pred []float64) float64 {
	var s errSums
	s.addAll(actual, pred)
	if s.n == 0 {
		return math.NaN()
	}
	return s.sqr / float64(s.n)
}

// RMSE returns the root mean squared error, NaN if no complete pair exists.
func RMSE(actual, pred []float64) float64 { return math.Sqrt(MSE(actual, pred)) }

// Bias returns the mean error forecast - actual (positive: the forecast is
// too high), NaN if no complete pair exists.
func Bias(actual, pred []float64) float64 {
	var s errSums
	s.addAll(actual, pred)
	if s.n == 0 {
		return math.NaN()
	}
	return s.sum / float64(s.n)
}

// MAPE returns the mean absolute percentage error in percent. Points whose
// actual value is 0 are skipped; NaN if nothing remains.
func MAPE(actual, pred []float64) float64 {
	var s errSums
	s.addAll(actual, pred)
	if s.nPct == 0 {
		return math.NaN()
	}
	return 100 * s.pct / float64(s.nPct)
}

// SMAPE returns the symmetric MAPE in percent: mean of 2|f-a|/(|a|+|f|),
// range 0..200. Points where both values are 0 count as zero error. NaN if
// no complete pair exists.
func SMAPE(actual, pred []float64) float64 {
	var s errSums
	s.addAll(actual, pred)
	if s.n == 0 {
		return math.NaN()
	}
	return 100 * s.smape / float64(s.n)
}

// SkillScore returns 1 - score/baseline for error measures where smaller is
// better (MAE, RMSE, MSE): 1 is a perfect forecast, 0 matches the baseline,
// negative is worse than the baseline. NaN when either input is NaN or the
// baseline error is 0 while the score is not; 0 when both are 0.
func SkillScore(score, baseline float64) float64 {
	if score != score || baseline != baseline {
		return math.NaN()
	}
	if baseline == 0 {
		if score == 0 {
			return 0
		}
		return math.NaN()
	}
	return 1 - score/baseline
}

// Forecaster produces h forecasts following history (oldest first). It may
// return fewer than h values; NaN forecasts are not scored. history aliases
// the caller's series and must be treated as read-only.
type Forecaster func(history []float64, h int) ([]float64, error)

// BacktestOptions configures Backtest. The zero value (or nil) is usable.
type BacktestOptions struct {
	Horizon    int // forecast steps per origin; default 1
	MinTrain   int // history length at the first origin; default max(len(y)/2, 2)
	Step       int // distance between origins; default Horizon
	Window     int // > 0: pass only the last Window samples as history (rolling); 0: expanding
	MaxOrigins int // > 0: evaluate only the most recent MaxOrigins origins
}

// BacktestResult is the outcome of a rolling-origin evaluation.
type BacktestResult struct {
	Origins   []int     `json:"origins"`   // history lengths (index of the first forecast sample) evaluated
	Failures  int       `json:"failures"`  // origins where the forecaster returned an error
	Overall   Metrics   `json:"overall"`   // all horizons pooled
	ByHorizon []Metrics `json:"byHorizon"` // index h-1
}

// Backtest evaluates a forecasting method by rolling-origin validation: for
// origins o = MinTrain, MinTrain+Step, ... it calls f(y[:o], Horizon) (or the
// last Window samples) and scores the forecasts against y[o:o+Horizon].
// Missing actuals and NaN forecasts are skipped; a truncated horizon at the
// end of the series is scored as far as data exists. Errors:
// ErrInvalidArgument (nil f), ErrInsufficientData (no origin fits).
func Backtest(y []float64, f Forecaster, opt *BacktestOptions) (*BacktestResult, error) {
	if f == nil {
		return nil, ErrInvalidArgument
	}
	res, _, err := backtestRun(y, []Forecaster{f}, opt)
	if err != nil {
		return nil, err
	}
	return res[0], nil
}

// backtestRun evaluates several forecasters on the same rolling origins. For
// exactly two forecasters it also returns, per forecaster, the error sums
// over the points that BOTH scored (index 0: all horizons pooled, index h:
// horizon h), the only fair basis for comparing them.
func backtestRun(y []float64, fs []Forecaster, opt *BacktestOptions) (res []*BacktestResult, paired [][]errSums, err error) {
	o := BacktestOptions{}
	if opt != nil {
		o = *opt
	}
	if o.Horizon < 1 {
		o.Horizon = 1
	}
	if o.Step < 1 {
		o.Step = o.Horizon
	}
	if o.MinTrain < 1 {
		o.MinTrain = max(len(y)/2, 2)
	}
	var origins []int
	for org := o.MinTrain; org < len(y); org += o.Step {
		origins = append(origins, org)
	}
	if o.MaxOrigins > 0 && len(origins) > o.MaxOrigins {
		origins = origins[len(origins)-o.MaxOrigins:]
	}
	if len(origins) == 0 {
		return nil, nil, ErrInsufficientData
	}
	res = make([]*BacktestResult, len(fs))
	all := make([]errSums, len(fs))
	byH := make([][]errSums, len(fs))
	for j := range fs {
		res[j] = &BacktestResult{Origins: origins, ByHorizon: make([]Metrics, o.Horizon)}
		byH[j] = make([]errSums, o.Horizon)
	}
	if len(fs) == 2 {
		paired = [][]errSums{make([]errSums, o.Horizon+1), make([]errSums, o.Horizon+1)}
	}
	fcs := make([][]float64, len(fs))
	ok := make([]bool, len(fs))
	for _, org := range origins {
		start := 0
		if o.Window > 0 && org > o.Window {
			start = org - o.Window
		}
		for j, f := range fs {
			// The capacity is capped at the origin as well, so a forecaster
			// cannot reach (or overwrite) the future by re-slicing.
			fc, ferr := f(y[start:org:org], o.Horizon)
			fcs[j], ok[j] = fc, ferr == nil
			if ferr != nil {
				res[j].Failures++
			}
		}
		for k := 0; k < o.Horizon && org+k < len(y); k++ {
			actual := y[org+k]
			for j := range fs {
				if ok[j] && k < len(fcs[j]) {
					all[j].add(actual, fcs[j][k])
					byH[j][k].add(actual, fcs[j][k])
				}
			}
			if paired != nil && ok[0] && ok[1] && k < len(fcs[0]) && k < len(fcs[1]) &&
				IsFinite(fcs[0][k]) && IsFinite(fcs[1][k]) {
				for j := range fs {
					paired[j][0].add(actual, fcs[j][k])
					paired[j][k+1].add(actual, fcs[j][k])
				}
			}
		}
	}
	for j := range fs {
		res[j].Overall = all[j].metrics()
		for k := range byH[j] {
			res[j].ByHorizon[k] = byH[j][k].metrics()
		}
	}
	return res, paired, nil
}

// BacktestComparison is a model backtest next to a baseline backtest.
//
// Model and Baseline are the two individual backtests. The skill scores are
// computed over the N points for which both forecasters delivered a forecast:
// a model that fails (or returns NaN) at the difficult origins must not look
// better than the baseline merely because it skipped them.
type BacktestComparison struct {
	Model     *BacktestResult `json:"model"`
	Baseline  *BacktestResult `json:"baseline"`
	N         int             `json:"n"`         // points scored by both forecasters, the basis of the skills
	Skill     float64         `json:"skill"`     // 1 - RMSE(model)/RMSE(baseline), all horizons pooled (0 if undefined)
	SkillMAE  float64         `json:"skillMae"`  // same for MAE
	ByHorizon []float64       `json:"byHorizon"` // RMSE skill per horizon (0 if undefined)
}

// CompareBacktest backtests a model and a baseline (e.g. a seasonal-naive
// forecaster) on the same origins and reports the skill of the model
// relative to the baseline, measured on the points both of them forecast.
// Undefined skills are reported as 0 so the result stays JSON-safe.
func CompareBacktest(y []float64, model, baseline Forecaster, opt *BacktestOptions) (*BacktestComparison, error) {
	if model == nil || baseline == nil {
		return nil, ErrInvalidArgument
	}
	res, paired, err := backtestRun(y, []Forecaster{model, baseline}, opt)
	if err != nil {
		return nil, err
	}
	skill := func(m, b *errSums, rmse bool) float64 {
		if m.n == 0 || b.n == 0 {
			return 0
		}
		fm, fb := float64(m.n), float64(b.n)
		sm, sb := m.abs/fm, b.abs/fb
		if rmse {
			sm, sb = math.Sqrt(m.sqr/fm), math.Sqrt(b.sqr/fb)
		}
		if s := SkillScore(sm, sb); IsFinite(s) {
			return s
		}
		return 0
	}
	pm, pb := paired[0], paired[1]
	c := &BacktestComparison{Model: res[0], Baseline: res[1], N: pm[0].n, ByHorizon: make([]float64, len(res[0].ByHorizon))}
	c.Skill = skill(&pm[0], &pb[0], true)
	c.SkillMAE = skill(&pm[0], &pb[0], false)
	for k := range c.ByHorizon {
		c.ByHorizon[k] = skill(&pm[k+1], &pb[k+1], true)
	}
	return c, nil
}

// SeasonalNaiveForecaster returns a Forecaster that repeats the last
// observed cycle (see SeasonalNaive); the customary baseline.
func SeasonalNaiveForecaster(period int) Forecaster {
	return func(history []float64, h int) ([]float64, error) {
		return SeasonalNaive(history, period, h)
	}
}
