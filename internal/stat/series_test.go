package stat

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

// seasonSignal is a trend + daily season test signal.
func seasonSignal(t int, period int) float64 {
	ph := 2 * math.Pi * float64(t%period) / float64(period)
	return 50 + 0.2*float64(t) + 10*math.Sin(ph) + 3*math.Cos(2*ph)
}

func rmseTo(pred []float64, truth func(i int) float64) float64 {
	s := 0.0
	for i, p := range pred {
		s += sq(p - truth(i))
	}
	return math.Sqrt(s / float64(len(pred)))
}

// --------------------------------------------------------------- seasonal

func TestHourSlots(t *testing.T) {
	const monday = 1704067200 // 2024-01-01T00:00:00Z, a Monday
	if HourOfWeek(monday, 0) != 0 || HourOfDay(monday, 0) != 0 {
		t.Error("monday midnight")
	}
	if HourOfWeek(monday+3600*30, 0) != 30 || HourOfDay(monday+3600*30, 0) != 6 {
		t.Error("tuesday 06:00")
	}
	if HourOfWeek(monday-1, 0) != 167 || HourOfDay(monday-1, 0) != 23 {
		t.Error("sunday 23:59:59")
	}
	if HourOfWeek(monday, 2*3600) != 2 || HourOfWeek(monday, -5*3600) != 163 {
		t.Error("utc offset")
	}
	// The epoch began on a Thursday; negative times floor correctly.
	if HourOfWeek(0, 0) != 72 || HourOfWeek(-3600, 0) != 71 || HourOfDay(-1, 0) != 23 {
		t.Errorf("epoch: %d %d %d", HourOfWeek(0, 0), HourOfWeek(-3600, 0), HourOfDay(-1, 0))
	}
	for h := int64(0); h < 400; h++ {
		if HourOfWeek(monday+h*3600, 0) != int(h%168) || HourOfDay(monday+h*3600+1799, 0) != int(h%24) {
			t.Fatalf("hour %d", h)
		}
	}
}

func TestSeasonalProfile(t *testing.T) {
	pattern := func(s int) float64 { return float64(s%24) + 10*float64(s/24) }
	const start = 100
	y := make([]float64, 6*HoursPerWeek)
	for i := range y {
		y[i] = pattern((start + i) % HoursPerWeek)
	}
	// One outage-style outlier and one gap in several slots.
	for _, i := range []int{5, 77, 300, 555, 800} {
		y[i] = -9999
		y[i+3] = nan
	}
	p, err := FitSeasonalProfile(y, start, HoursPerWeek, nil)
	if err != nil {
		t.Fatal(err)
	}
	for s := 0; s < HoursPerWeek; s++ {
		v, ok := p.At(s)
		if !ok || math.Abs(v-pattern(s)) > 1e-9 {
			t.Fatalf("slot %d = %v, want %v", s, v, pattern(s))
		}
		if p.Spread[s] != 0 {
			t.Fatalf("spread slot %d = %v", s, p.Spread[s])
		}
	}
	if p.Count[(start+8)%HoursPerWeek] != 5 || p.Count[(start+9)%HoursPerWeek] != 6 {
		t.Errorf("counts: %d %d", p.Count[(start+8)%HoursPerWeek], p.Count[(start+9)%HoursPerWeek])
	}
	if v, ok := p.At(-1); !ok || v != pattern(167) {
		t.Error("At wraps negative slots")
	}
	// The mean is not robust.
	pm, _ := FitSeasonalProfile(y, start, HoursPerWeek, &ProfileOptions{Mean: true})
	s5 := (start + 5) % HoursPerWeek
	if v, _ := pm.At(s5); !(v < pattern(s5)-1000) || !(pm.Spread[s5] > 1000) {
		t.Errorf("mean profile should be pulled by the outlier: %v", v)
	}
	// Forecast / Deseasonalize follow the slots.
	fc := p.Forecast(165, 6)
	for k, v := range fc {
		approx(t, "forecast", v, pattern((165+k)%168), 1e-9)
	}
	ds := p.Deseasonalize(y, start)
	approx(t, "deseasonalized", ds[0], 0, 1e-9)
	approx(t, "deseasonalized outlier", ds[5], -9999-pattern((start+5)%168), 1e-9)
	wantNaN(t, "deseasonalized gap", ds[8])
	if _, err := json.Marshal(p); err != nil {
		t.Error(err)
	}

	// Exponential forgetting: an old regime is forgotten.
	z := make([]float64, 24*40)
	for i := range z {
		if i >= 24*20 {
			z[i] = 10
		}
	}
	for _, mean := range []bool{false, true} {
		old, _ := FitSeasonalProfile(z, 0, 24, &ProfileOptions{Mean: mean})
		rec, _ := FitSeasonalProfile(z, 0, 24, &ProfileOptions{Mean: mean, HalfLife: 48})
		vo, _ := old.At(3)
		vr, _ := rec.At(3)
		approx(t, "no forgetting", vo, 5, 1e-9)
		if !(vr > 9.9) {
			t.Errorf("forgetting (mean=%v): %v", mean, vr)
		}
		if !(rec.Weight[3] < old.Weight[3]) {
			t.Error("effective weight should shrink with forgetting")
		}
	}

	// Gaps are filled around the cycle.
	g, err := FitSeasonalProfileSlots([]float64{0, 30, nan}, []int{0, 3, 4}, 6, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.At(1); ok {
		t.Error("slot 1 should be undefined")
	}
	filled, ok := g.Filled()
	if !ok {
		t.Fatal("filled")
	}
	for i, w := range []float64{0, 10, 20, 30, 20, 10} {
		approx(t, "filled", filled[i], w, 1e-12)
	}
	one, _ := FitSeasonalProfileSlots([]float64{7}, []int{-1}, 4, nil)
	filled, _ = one.Filled()
	for _, v := range filled {
		approx(t, "single slot fills all", v, 7, 0)
	}
	if one.Count[3] != 1 {
		t.Error("negative slot wraps")
	}

	// Online updates.
	on := NewSeasonalProfile(24)
	if _, ok := on.Filled(); ok || on.Forecast(0, 3) != nil || on.Deseasonalize(y, 0) != nil {
		t.Error("empty profile")
	}
	on.Update(5, 10, 0.5)
	on.Update(5, 20, 0.5)
	on.Update(5, nan, 0.5)
	on.Update(29, 20, 0.5) // wraps to slot 5
	on.Update(6, 1, 7)     // invalid alpha ignored
	v, _ := on.At(5)
	approx(t, "online level", v, 17.5, 1e-12)
	if on.Count[5] != 3 || on.Count[6] != 0 {
		t.Errorf("online counts %v %v", on.Count[5], on.Count[6])
	}

	if _, err := FitSeasonalProfile(y, 0, 0, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Error("period 0", err)
	}
	if _, err := FitSeasonalProfile([]float64{nan}, 0, 24, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("no data", err)
	}
	if _, err := FitSeasonalProfileSlots([]float64{1}, nil, 24, nil); !errors.Is(err, ErrDimension) {
		t.Error("dimension", err)
	}
	var nilProfile *SeasonalProfile
	if _, ok := nilProfile.At(0); ok {
		t.Error("nil profile")
	}
	nilProfile.Update(0, 1, 0.5)
}

func TestSeasonalNaive(t *testing.T) {
	check := func(name string, got, want []float64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %v", name, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: %v, want %v", name, got, want)
			}
		}
	}
	f, err := SeasonalNaive([]float64{1, 2, 3, 4, 5, 6}, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	check("basic", f, []float64{4, 5, 6, 4, 5})
	f, _ = SeasonalNaive([]float64{1, 2, 3, 4, 5, nan}, 3, 5)
	check("missing looks one cycle further back", f, []float64{4, 5, 3, 4, 5})
	f, _ = SeasonalNaive([]float64{1, 2, nan, 4, 5, nan}, 3, 3)
	check("never observed phase uses last value", f, []float64{4, 5, 5})
	f, _ = SeasonalNaive([]float64{7, 8}, 24, 3)
	check("short history", f, []float64{8, 8, 8})
	f, _ = SeasonalNaive([]float64{7, 8, nan}, 0, 2)
	check("period<1 is last value", f, []float64{8, 8})
	if _, err := SeasonalNaive([]float64{nan}, 3, 2); !errors.Is(err, ErrInsufficientData) {
		t.Error("no data", err)
	}
	if _, err := SeasonalNaive([]float64{1}, 3, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Error("h<1", err)
	}
}

// ----------------------------------------------------------- Holt-Winters

func TestHoltWintersExact(t *testing.T) {
	for _, period := range []int{24, 168} {
		n := period * 5
		y := make([]float64, n)
		for i := range y {
			y[i] = seasonSignal(i, period)
		}
		fit, err := FitHoltWinters(y, period, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !(fit.SSE < 1e-12) || fit.NObs != n || fit.Model.N != n {
			t.Errorf("period %d: sse %v nobs %d", period, fit.SSE, fit.NObs)
		}
		approx(t, "trend", fit.Model.Trend, 0.2, 1e-9)
		fc := fit.Model.Forecast(2 * period)
		for k, v := range fc {
			approx(t, "forecast", v, seasonSignal(n+k, period), 1e-6)
		}
		for i := range y {
			approx(t, "fitted", fit.Fitted[i], y[i], 1e-6)
		}
		if fit.Model.Phase() != 0 {
			t.Error("phase")
		}
	}
}

func TestHoltWintersNoisy(t *testing.T) {
	const period = 24
	r := newRand(30)
	n := period * 40
	y := make([]float64, n)
	for i := range y {
		y[i] = seasonSignal(i, period) + r.NormFloat64()
	}
	truth := func(k int) float64 { return seasonSignal(n+k, period) }
	fit, err := FitHoltWinters(y, period, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := fit.Model
	if !(m.Alpha > 0 && m.Alpha < 1 && m.Beta >= 0 && m.Gamma >= 0 && m.Gamma <= 1 && m.Phi == 1) {
		t.Errorf("params %+v", m.HWParams)
	}
	approx(t, "sigma", m.Sigma, 1, 0.15)
	approx(t, "trend", m.Trend, 0.2, 0.05)
	mean, lo, hi := m.ForecastInterval(48, 0.9)
	hw := rmseTo(mean, truth)
	naive, _ := SeasonalNaive(y, period, 48)
	nv := rmseTo(naive, truth)
	if !(hw < 1.2) || !(hw < nv) {
		t.Errorf("forecast rmse %v (seasonal naive %v)", hw, nv)
	}
	// Intervals bracket the mean and never narrow with the horizon.
	sd := m.ForecastStd(48)
	for k := range mean {
		if !(lo[k] < mean[k] && mean[k] < hi[k]) {
			t.Fatalf("interval order at %d", k)
		}
		if k > 0 && sd[k] < sd[k-1] {
			t.Fatalf("std shrinks at %d", k)
		}
	}
	approx(t, "h=1 std", sd[0], m.Sigma, 1e-12)
	approx(t, "interval width", hi[0]-lo[0], 2*1.6448536*m.Sigma, 1e-6)
	// The optimiser must not be worse than its own starting grid or than
	// arbitrary fixed parameters.
	fixed, err := FitHoltWinters(y, period, &HWOptions{Params: &HWParams{Alpha: 0.5, Beta: 0.1, Gamma: 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	if !(fit.SSE <= fixed.SSE) || fit.Opt.Evals == 0 || fixed.Opt.Evals != 0 {
		t.Errorf("optimised sse %v vs fixed %v", fit.SSE, fixed.SSE)
	}

	// Online continuation equals batch processing.
	pr := &HWParams{Alpha: 0.3, Beta: 0.05, Gamma: 0.2}
	head, _ := FitHoltWinters(y[:n-100], period, &HWOptions{Params: pr})
	whole, _ := FitHoltWinters(y, period, &HWOptions{Params: pr})
	for i, v := range y[n-100:] {
		pred, resid := head.Model.Update(v)
		approx(t, "online pred", pred, whole.Fitted[n-100+i], 1e-9)
		approx(t, "online resid", resid, whole.Residuals[n-100+i], 1e-9)
	}
	approx(t, "online level", head.Model.Level, whole.Model.Level, 1e-9)
	approx(t, "online trend", head.Model.Trend, whole.Model.Trend, 1e-9)
	if head.Model.N != whole.Model.N || head.Model.Phase() != whole.Model.Phase() {
		t.Error("online counters")
	}

	// JSON round trip keeps the forecast.
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var m2 HoltWinters
	if err := json.Unmarshal(data, &m2); err != nil {
		t.Fatal(err)
	}
	f1, f2 := m.Forecast(30), m2.Forecast(30)
	for k := range f1 {
		if f1[k] != f2[k] {
			t.Fatalf("forecast differs after JSON round trip at %d", k)
		}
	}
}

func TestHoltWintersMissing(t *testing.T) {
	const period = 24
	r := newRand(31)
	n := period * 30
	y := make([]float64, n)
	for i := range y {
		y[i] = seasonSignal(i, period) + 0.5*r.NormFloat64()
		if r.Float64() < 0.1 {
			y[i] = nan
		}
	}
	for i := 200; i < 260; i++ { // a 2.5 day outage
		y[i] = nan
	}
	for i := n - 7; i < n-2; i++ { // and a gap right before the forecast
		y[i] = nan
	}
	fit, err := FitHoltWinters(y, period, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fit.NObs != Count(y) || len(fit.Residuals) != n {
		t.Errorf("nobs %d vs %d", fit.NObs, Count(y))
	}
	for i := range y {
		if math.IsNaN(y[i]) != math.IsNaN(fit.Residuals[i]) || math.IsNaN(fit.Fitted[i]) {
			t.Fatalf("residual/fitted NaN pattern at %d", i)
		}
	}
	fc := fit.Model.Forecast(48)
	if e := rmseTo(fc, func(k int) float64 { return seasonSignal(n+k, period) }); !(e < 1.2) {
		t.Errorf("forecast rmse with gaps = %v", e)
	}
	if _, err := json.Marshal(fit.Model); err != nil {
		t.Error("model with gaps must stay JSON-safe:", err)
	}

	// A whole missing first cycle and a never-observed phase are survivable.
	z := make([]float64, period*6)
	for i := range z {
		z[i] = seasonSignal(i, period)
		if i < period || i%period == 7 {
			z[i] = nan
		}
	}
	fit, err = FitHoltWinters(z, period, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range fit.Model.Forecast(period) {
		if !IsFinite(v) {
			t.Fatal("non-finite forecast")
		}
	}
}

func TestHoltAndSES(t *testing.T) {
	// Exact line.
	y := make([]float64, 40)
	for i := range y {
		y[i] = 3 + 2*float64(i)
	}
	y[10], y[11] = nan, nan
	fit, err := FitHolt(y, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !(fit.SSE < 1e-16) || fit.Model.Period != 0 || fit.Model.Season != nil && len(fit.Model.Season) != 0 {
		t.Errorf("holt exact: sse %v", fit.SSE)
	}
	for k, v := range fit.Model.Forecast(5) {
		approx(t, "holt forecast", v, 3+2*float64(40+k), 1e-8)
	}

	// Noisy line: trend recovered, forecast close.
	r := newRand(32)
	for i := range y {
		y[i] = 3 + 2*float64(i) + r.NormFloat64()
	}
	fit, _ = FitHolt(y, nil)
	approx(t, "holt trend", fit.Model.Trend, 2, 0.5)
	approx(t, "holt h=5", fit.Model.Forecast(5)[4], 3+2*44.0, 4)

	// Damped trend: phi fitted inside its bounds, forecasts flatten.
	fit, err = FitHolt(y, &HWOptions{Damped: true})
	if err != nil {
		t.Fatal(err)
	}
	if !(fit.Model.Phi >= 0.8 && fit.Model.Phi <= 0.995) {
		t.Errorf("phi = %v", fit.Model.Phi)
	}
	fc := fit.Model.Forecast(400)
	// Successive increments decay geometrically with phi.
	approx(t, "damping", (fc[399]-fc[398])/(fc[1]-fc[0]), math.Pow(fit.Model.Phi, 398), 1e-6)
	if !(fc[399]-fc[398] < fc[1]-fc[0]) {
		t.Error("damped forecast should flatten")
	}

	// Simple exponential smoothing with fixed alpha: closed-form recursion
	// and forecast variance sigma^2 * (1 + (h-1) alpha^2).
	s := []float64{10, 12, nan, 11, 15, 9, 10, 13}
	fit, err = FitHoltWinters(s, 1, &HWOptions{NoTrend: true, Params: &HWParams{Alpha: 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	level := Mean([]float64{10, 12, 11, 15, 9}) // initial level: first five valid values
	for _, v := range s {
		if v == v {
			level += 0.5 * (v - level)
		}
	}
	approx(t, "ses level", fit.Model.Level, level, 1e-12)
	if fit.Model.Trend != 0 || fit.Model.HasTrend {
		t.Error("ses has no trend")
	}
	fc = fit.Model.Forecast(3)
	if fc[0] != fc[2] {
		t.Error("ses forecast is flat")
	}
	sd := fit.Model.ForecastStd(4)
	for h := 1; h <= 4; h++ {
		approx(t, "ses std", sd[h-1], fit.Model.Sigma*math.Sqrt(1+float64(h-1)*0.25), 1e-12)
	}
	// Holt: var_h = sigma^2 * (1 + sum_{j<h} (alpha*(1+j*beta))^2).
	hm := &HoltWinters{HWParams: HWParams{Alpha: 0.4, Beta: 0.5, Phi: 1}, HasTrend: true, Sigma: 2}
	approx(t, "holt std h=3", hm.ForecastStd(3)[2], 2*math.Sqrt(1+sq(0.4*1.5)+sq(0.4*2)), 1e-12)
	// Seasonal term enters once per period.
	sm := &HoltWinters{HWParams: HWParams{Alpha: 0.2, Gamma: 0.5, Phi: 1}, Period: 2, Season: []float64{1, -1}, Sigma: 1}
	approx(t, "seasonal std h=3", sm.ForecastStd(3)[2], math.Sqrt(1+sq(0.2)+sq(0.2+0.5*0.8)), 1e-12)
	if sm.Forecast(0) != nil || sm.ForecastStd(0) != nil {
		t.Error("h<1")
	}
	if a, b, c := sm.ForecastInterval(0, 0.9); a != nil || b != nil || c != nil {
		t.Error("interval h<1")
	}
}

func TestHoltWintersErrors(t *testing.T) {
	y := make([]float64, 40)
	for i := range y {
		y[i] = float64(i % 24)
	}
	if _, err := FitHoltWinters(y, 24, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("less than two seasons", err)
	}
	if _, err := FitHoltWinters([]float64{1, nan, 2}, 0, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("too few valid", err)
	}
	if _, err := FitHoltWinters(nil, 24, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("nil", err)
	}
	all := make([]float64, 100)
	for i := range all {
		all[i] = nan
	}
	if _, err := FitHoltWinters(all, 24, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("all nan", err)
	}
	long := append(append([]float64{}, y...), y...)
	if _, err := FitHoltWinters(long, 24, &HWOptions{Params: &HWParams{Alpha: 1.5}}); !errors.Is(err, ErrInvalidArgument) {
		t.Error("alpha out of range", err)
	}
	// Constant series: a perfectly valid, flat forecast.
	c := make([]float64, 96)
	for i := range c {
		c[i] = 42
	}
	fit, err := FitHoltWinters(c, 24, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range fit.Model.Forecast(30) {
		approx(t, "constant", v, 42, 1e-9)
	}
	if fit.Model.Sigma != 0 {
		t.Errorf("sigma = %v", fit.Model.Sigma)
	}
}

// --------------------------------------------------------------------- AR

func TestAR(t *testing.T) {
	r := newRand(33)
	n := 30000
	x := make([]float64, n)
	x[0], x[1] = 5, 5
	for i := 2; i < n; i++ {
		x[i] = 5 + 0.6*(x[i-1]-5) - 0.3*(x[i-2]-5) + r.NormFloat64()
	}
	x[500], x[501], x[9000] = nan, nan, nan
	for name, fitFn := range map[string]func([]float64, int) (*ARModel, error){
		"yule-walker": FitARYuleWalker, "least-squares": FitARLeastSquares,
	} {
		m, err := fitFn(x, 2)
		if err != nil {
			t.Fatal(name, err)
		}
		approx(t, name+" phi1", m.Coef[0], 0.6, 0.03)
		approx(t, name+" phi2", m.Coef[1], -0.3, 0.03)
		approx(t, name+" sigma2", m.Sigma2, 1, 0.05)
		approx(t, name+" mean", m.Mean, 5, 0.1)
		approx(t, name+" const", m.Const, 5*0.7, 0.2)
		// Long-range forecasts revert to the process mean.
		fc := m.Forecast([]float64{9, 8}, 200)
		approx(t, name+" h=1", fc[0], m.Const+m.Coef[0]*8+m.Coef[1]*9, 1e-12)
		approx(t, name+" reversion", fc[199], 5, 0.15)
		// Missing / short history falls back to the mean.
		fc = m.Forecast([]float64{nan}, 1)
		approx(t, name+" imputed", fc[0], m.Const+(m.Coef[0]+m.Coef[1])*m.Mean, 1e-12)
		if _, err := json.Marshal(m); err != nil {
			t.Error(err)
		}
	}
	// Over-fitting the order leaves the extra coefficients near zero.
	m, _ := FitARYuleWalker(x, 5)
	for _, c := range m.Coef[2:] {
		approx(t, "extra coef", c, 0, 0.03)
	}

	// Noise-free AR(1): exact recovery by least squares.
	d := make([]float64, 15)
	d[0] = 10
	for i := 1; i < len(d); i++ {
		d[i] = 1 + 0.5*d[i-1]
	}
	m, err := FitARLeastSquares(d, 1)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "exact phi", m.Coef[0], 0.5, 1e-9)
	approx(t, "exact const", m.Const, 1, 1e-9)
	approx(t, "exact next", m.Forecast(d, 1)[0], 1+0.5*d[14], 1e-9)

	// Forecast error of an AR(1): sigma^2 (1-phi^(2h)) / (1-phi^2).
	ar1 := &ARModel{Coef: []float64{0.8}, Sigma2: 2}
	sd := ar1.ForecastStd(6)
	for h := 1; h <= 6; h++ {
		approx(t, "ar1 std", sd[h-1], math.Sqrt(2*(1-math.Pow(0.8, float64(2*h)))/(1-0.64)), 1e-12)
	}
	if ar1.Forecast(nil, 0) != nil || ar1.ForecastStd(0) != nil {
		t.Error("h<1")
	}

	if _, err := FitARYuleWalker(x, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Error("p<1", err)
	}
	if _, err := FitARLeastSquares(x, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Error("p<1", err)
	}
	if _, err := FitARYuleWalker([]float64{1, 2, 3}, 3); !errors.Is(err, ErrInsufficientData) {
		t.Error("short", err)
	}
	if _, err := FitARLeastSquares([]float64{1, 2, 3}, 3); !errors.Is(err, ErrInsufficientData) {
		t.Error("short ls", err)
	}
	if _, err := FitARYuleWalker([]float64{4, 4, 4, 4, 4, 4}, 1); !errors.Is(err, ErrDegenerate) {
		t.Error("constant", err)
	}
	if _, err := FitARLeastSquares([]float64{4, 4, 4, 4, 4, 4}, 1); !errors.Is(err, ErrSingular) {
		t.Error("constant ls", err)
	}
}

// ---------------------------------------------------------------- anomaly

func TestAnomaly(t *testing.T) {
	r := newRand(34)
	res := make([]float64, 300)
	for i := range res {
		res[i] = 0.5 * r.NormFloat64()
	}
	res[120] = 8
	res[200] = nan
	z := AnomalyScores(res)
	if !(z[120] > 10) {
		t.Errorf("spike score %v", z[120])
	}
	wantNaN(t, "nan stays", z[200])
	flagged := 0
	for i, v := range z {
		if i != 120 && math.Abs(v) > 3.5 {
			flagged++
		}
	}
	if flagged > 2 {
		t.Errorf("%d false alarms", flagged)
	}

	// Rolling scores judge against the preceding window only.
	x := make([]float64, 200)
	for i := range x {
		x[i] = 100 + r.NormFloat64()
		if i >= 150 {
			x[i] += 12
		}
	}
	x[60] = nan
	rz := RollingAnomalyScores(x, 48, 10)
	wantNaN(t, "warm-up", rz[5])
	wantNaN(t, "missing", rz[60])
	if !(rz[150] > 6) || math.Abs(rz[100]) > 4 {
		t.Errorf("rolling: shift %v, quiet %v", rz[150], rz[100])
	}
	// After the window has moved past the shift the scores calm down again.
	if math.Abs(rz[199]) > 4 {
		t.Errorf("adapted score %v", rz[199])
	}
	flat := []float64{5, 5, 5, 5, 5, 5, 9, 5}
	rz = RollingAnomalyScores(flat, 5, 3)
	if rz[5] != 0 || rz[6] != 1e6 {
		t.Errorf("flat baseline: %v", rz)
	}
	for _, v := range RollingAnomalyScores(flat, 0, 3) {
		wantNaN(t, "window 0", v)
	}

	// Change point.
	cpx := make([]float64, 100)
	for i := range cpx {
		cpx[i] = r.NormFloat64()
		if i >= 60 {
			cpx[i] += 5
		}
	}
	cpx[30] = nan
	cp, ok := DetectChangePoint(cpx, 5)
	if !ok || iabs(cp.Index-60) > 1 || !(cp.Score > 10) {
		t.Errorf("change point %+v", cp)
	}
	approx(t, "shift", cp.Shift, 5, 0.6)
	if _, err := json.Marshal(cp); err != nil {
		t.Error(err)
	}
	quiet := make([]float64, 100)
	for i := range quiet {
		quiet[i] = r.NormFloat64()
	}
	cp, ok = DetectChangePoint(quiet, 5)
	if !ok || !(cp.Score < 4.5) {
		t.Errorf("stationary noise scored %+v", cp)
	}
	if _, ok := DetectChangePoint([]float64{1, 2, 3}, 5); ok {
		t.Error("short series")
	}
	if _, ok := DetectChangePoint([]float64{2, 2, 2, 2, 2, 2}, 2); ok {
		t.Error("constant series")
	}
	cp, ok = DetectChangePoint([]float64{2, 2, 2, 7, 7, 7}, 2)
	if !ok || cp.Index != 3 || cp.Shift != 5 || cp.Score != 1e6 {
		t.Errorf("noise-free step: %+v", cp)
	}

	// CUSUM: a +2 sigma shift with k = 0.5 gains 1.5 per step, alarm at the 4th.
	zs := make([]float64, 30)
	for i := 10; i < 30; i++ {
		zs[i] = 2
	}
	zs[5] = nan
	pos, neg, alarms := CUSUM(zs, 0.5, 5)
	if len(alarms) == 0 || alarms[0] != 13 || pos[12] != 4.5 || neg[20] != 0 {
		t.Errorf("cusum alarms %v pos[12]=%v", alarms, pos[12])
	}
	if pos[13] != 6 || pos[14] != 1.5 {
		t.Errorf("cusum reset: %v %v", pos[13], pos[14])
	}
	for i := range zs {
		zs[i] = -zs[i]
	}
	_, neg, alarms = CUSUM(zs, 0.5, 5)
	if alarms[0] != 13 || neg[12] != 4.5 {
		t.Error("cusum negative side")
	}
}

// ----------------------------------------------------------------- Kalman

func TestLocalLevel(t *testing.T) {
	// Steady state: predicted variance Pp solves Pp^2 - Q*Pp - Q*R = 0.
	k := NewLocalLevel(1, 4)
	wantNaN(t, "before first", k.Update(nan))
	for i := 0; i < 200; i++ {
		k.Update(float64(i % 3))
	}
	pp := (1 + math.Sqrt(17)) / 2
	approx(t, "steady P", k.P, pp-1, 1e-9)

	// Q = 0 reduces the filter to the running mean with variance R/n.
	k = NewLocalLevel(0, 2)
	obs := []float64{3, 5, nan, 10, 2}
	var acc Accumulator
	for _, v := range obs {
		k.Update(v)
		acc.Add(v)
		approx(t, "running mean", k.Level, acc.Mean(), 1e-12)
		approx(t, "variance", k.P, 2/float64(acc.N), 1e-12)
	}
	if k.N != 4 {
		t.Errorf("N = %d", k.N)
	}
	// A gap inflates the uncertainty in proportion to its length.
	k = NewLocalLevel(0.5, 1)
	k.Update(10)
	p0 := k.P
	k.Step(nan, 6)
	approx(t, "gap", k.P, p0+3, 1e-12)
	approx(t, "level held", k.Level, 10, 0)
	if NewLocalLevel(-1, nan).Q != 0 {
		t.Error("invalid variances clamp to 0")
	}

	// Smoother: closer to the truth than the raw data and than the filter.
	r := newRand(35)
	n := 400
	truth := make([]float64, n)
	y := make([]float64, n)
	for i := range y {
		truth[i] = 1013 + 6*math.Sin(float64(i)/40)
		y[i] = truth[i] + r.NormFloat64()
	}
	for i := 100; i < 110; i++ {
		y[i] = nan
	}
	y[0] = nan
	sm := KalmanSmoothLevel(y, 0.02, 1)
	wantNaN(t, "leading", sm[0])
	var eRaw, eSm float64
	cnt := 0
	for i := 1; i < n; i++ {
		if !IsFinite(sm[i]) {
			t.Fatalf("smoothed NaN at %d", i)
		}
		eSm += sq(sm[i] - truth[i])
		if y[i] == y[i] {
			eRaw += sq(y[i] - truth[i])
			cnt++
		}
	}
	eSm = math.Sqrt(eSm / float64(n-1))
	eRaw = math.Sqrt(eRaw / float64(cnt))
	if !(eSm < 0.5*eRaw) {
		t.Errorf("smoother rmse %v vs raw %v", eSm, eRaw)
	}
	for _, v := range KalmanSmoothLevel([]float64{nan, nan}, 1, 1) {
		wantNaN(t, "all nan", v)
	}
	if len(KalmanSmoothLevel(nil, 1, 1)) != 0 {
		t.Error("nil")
	}
	// Noise-free data passes through unchanged when R = 0.
	sm = KalmanSmoothLevel([]float64{1, 4, 2}, 1, 0)
	if sm[0] != 1 || sm[1] != 4 || sm[2] != 2 {
		t.Errorf("R=0: %v", sm)
	}
}

func TestLocalTrend(t *testing.T) {
	// Noise-free ramp: the trend is identified after two samples.
	k := NewLocalTrend(1e-4, 1e-6, 0.01)
	if l, b := k.Update(nan); !math.IsNaN(l) || !math.IsNaN(b) {
		t.Error("before first measurement")
	}
	if m, s := k.Forecast(1); !math.IsNaN(m) || !math.IsNaN(s) {
		t.Error("forecast before first measurement")
	}
	for i := 0; i < 50; i++ {
		v := 2 + 0.5*float64(i)
		if i == 20 || i == 21 {
			v = nan
		}
		k.Update(v)
	}
	approx(t, "ramp level", k.Level, 2+0.5*49, 1e-4)
	approx(t, "ramp trend", k.Trend, 0.5, 1e-5)
	mean, sd := k.Forecast(6)
	approx(t, "ramp forecast", mean, 2+0.5*55, 1e-3)
	_, sd1 := k.Forecast(1)
	if !(sd > sd1) || !(sd1 > 0) {
		t.Errorf("forecast std %v %v", sd1, sd)
	}
	if k.N != 48 {
		t.Errorf("N = %d", k.N)
	}
	data, _ := json.Marshal(k)
	var k2 LocalTrend
	if err := json.Unmarshal(data, &k2); err != nil || k2 != *k {
		t.Error("JSON round trip", err)
	}
	// Irregular sampling: a step of dt=3 extrapolates along the trend.
	k.Step(nan, 3)
	approx(t, "dt step", k.Level, 2+0.5*52, 1e-3)

	// Falling pressure with noise: tendency -1 hPa/h is recovered by the
	// filter, and the smoother is closer to the truth than the raw data.
	r := newRand(36)
	n := 240
	y := make([]float64, n)
	truth := make([]float64, n)
	for i := range y {
		truth[i] = 1020 - float64(i) + 15*math.Sin(float64(i)/30)
		y[i] = truth[i] + 0.5*r.NormFloat64()
		if i%23 == 0 || (i > 80 && i < 86) {
			y[i] = nan
		}
	}
	level, trend := KalmanSmoothTrend(y, 1e-3, 1e-3, 0.25)
	wantNaN(t, "leading level", level[0])
	wantNaN(t, "leading trend", trend[0])
	var eSm, eTr float64
	for i := 5; i < n-5; i++ {
		eSm += sq(level[i] - truth[i])
		eTr += sq(trend[i] - (-1 + 0.5*math.Cos(float64(i)/30)))
	}
	eSm = math.Sqrt(eSm / float64(n-10))
	eTr = math.Sqrt(eTr / float64(n-10))
	if !(eSm < 0.3) || !(eTr < 0.1) {
		t.Errorf("smoothed level rmse %v, trend rmse %v", eSm, eTr)
	}
	f := NewLocalTrend(1e-3, 1e-3, 0.25)
	for _, v := range y {
		f.Update(v)
	}
	approx(t, "filtered tendency", f.Trend, -1+0.5*math.Cos(float64(n-1)/30), 0.15)

	lv, tr := KalmanSmoothTrend([]float64{nan}, 1, 1, 1)
	if !math.IsNaN(lv[0]) || !math.IsNaN(tr[0]) {
		t.Error("all nan")
	}
	lv, tr = KalmanSmoothTrend([]float64{5}, 1, 1, 1)
	if lv[0] != 5 || tr[0] != 0 {
		t.Error("single sample")
	}
}

// ------------------------------------------------------------- evaluation

func TestMetrics(t *testing.T) {
	actual := []float64{1, 2, 3, 4, nan}
	pred := []float64{1.5, 2, 2, nan, 5}
	approx(t, "mae", MAE(actual, pred), 0.5, 1e-15)
	approx(t, "mse", MSE(actual, pred), 1.25/3, 1e-15)
	approx(t, "rmse", RMSE(actual, pred), math.Sqrt(1.25/3), 1e-15)
	approx(t, "bias", Bias(actual, pred), -0.5/3, 1e-15)
	approx(t, "mape", MAPE(actual, pred), 100*(0.5+0+1.0/3)/3, 1e-12)
	approx(t, "smape", SMAPE(actual, pred), 100*(0.4+0+0.4)/3, 1e-12)
	m := Evaluate(actual, pred)
	if m.N != 3 || m.NPct != 3 || m.MAE != MAE(actual, pred) || m.RMSE != RMSE(actual, pred) ||
		m.Bias != Bias(actual, pred) || m.MAPE != MAPE(actual, pred) || m.SMAPE != SMAPE(actual, pred) {
		t.Errorf("Evaluate = %+v", m)
	}
	ref, _ := Pearson(actual[:3], pred[:3])
	approx(t, "corr", m.Corr, ref.R, 1e-12)

	// Zeros: MAPE skips them, sMAPE treats 0/0 as no error.
	approx(t, "mape zeros", MAPE([]float64{0, 10}, []float64{3, 12}), 20, 1e-12)
	approx(t, "smape zeros", SMAPE([]float64{0, 0}, []float64{0, 5}), 100, 1e-12)
	wantNaN(t, "mape all zero", MAPE([]float64{0, 0}, []float64{1, 2}))
	z := Evaluate([]float64{0, 0}, []float64{1, 2})
	if z.NPct != 0 || z.MAPE != 0 || z.N != 2 {
		t.Errorf("%+v", z)
	}
	// Empty.
	for _, f := range []func(a, p []float64) float64{MAE, MSE, RMSE, Bias, MAPE, SMAPE} {
		wantNaN(t, "empty", f(nil, nil))
		wantNaN(t, "no overlap", f([]float64{1, nan}, []float64{nan, 1}))
	}
	if e := Evaluate(nil, nil); e != (Metrics{}) {
		t.Errorf("empty Evaluate = %+v", e)
	}
	if _, err := json.Marshal(Evaluate([]float64{1, 1}, []float64{2, 2})); err != nil {
		t.Error(err)
	}
	// Different lengths: common prefix.
	approx(t, "prefix", MAE([]float64{1, 2, 3}, []float64{2}), 1, 0)

	approx(t, "skill", SkillScore(2, 8), 0.75, 1e-15)
	approx(t, "skill equal", SkillScore(3, 3), 0, 0)
	approx(t, "skill worse", SkillScore(6, 3), -1, 0)
	approx(t, "skill both zero", SkillScore(0, 0), 0, 0)
	wantNaN(t, "skill zero baseline", SkillScore(1, 0))
	wantNaN(t, "skill nan", SkillScore(nan, 1))
}

func TestBacktest(t *testing.T) {
	y := make([]float64, 100)
	for i := range y {
		y[i] = 3 * float64(i)
	}
	y[57] = nan
	var histLens []int
	linear := func(h []float64, steps int) ([]float64, error) {
		histLens = append(histLens, len(h))
		n := len(h)
		if n < 2 || h[n-1] != h[n-1] || h[n-2] != h[n-2] {
			return nil, ErrInsufficientData
		}
		out := make([]float64, steps)
		for k := range out {
			out[k] = h[n-1] + float64(k+1)*(h[n-1]-h[n-2])
		}
		return out, nil
	}
	res, err := Backtest(y, linear, &BacktestOptions{Horizon: 4, MinTrain: 20, Step: 7})
	if err != nil {
		t.Fatal(err)
	}
	// Origins 20, 27, ..., 97 (12 of them); the ones with a missing value at
	// the end of their history (58, 59 are not origins; 62 is fine) all work.
	if len(res.Origins) != 12 || res.Origins[0] != 20 || res.Origins[11] != 97 || res.Failures != 0 {
		t.Errorf("origins %v failures %d", res.Origins, res.Failures)
	}
	// 12 origins x 4 steps, minus one missing actual (index 57), minus the
	// truncated tail (origin 97 only has 3 samples left).
	if res.Overall.N != 12*4-1-1 || res.Overall.RMSE > 1e-9 || res.Overall.MAE > 1e-9 {
		t.Errorf("overall %+v", res.Overall)
	}
	if len(res.ByHorizon) != 4 || res.ByHorizon[0].N != 12 || res.ByHorizon[2].N != 11 || res.ByHorizon[3].N != 11 {
		t.Errorf("by horizon %+v", res.ByHorizon)
	}
	for i, l := range histLens {
		if l != res.Origins[i] {
			t.Fatalf("expanding window: history %d at origin %d", l, res.Origins[i])
		}
	}

	// Rolling window, origin limit and failures.
	histLens = nil
	res, err = Backtest(y, linear, &BacktestOptions{Horizon: 1, MinTrain: 50, Window: 10, MaxOrigins: 45})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Origins) != 45 || res.Origins[0] != 55 || res.Failures != 2 {
		t.Errorf("origins %d first %d failures %d", len(res.Origins), res.Origins[0], res.Failures)
	}
	for _, l := range histLens {
		if l != 10 {
			t.Fatalf("rolling window passed %d samples", l)
		}
	}
	// 45 origins - 2 failures (history ends in NaN at origins 58, 59) - 1
	// missing actual (origin 57).
	if res.Overall.N != 42 {
		t.Errorf("scored %d", res.Overall.N)
	}

	// Defaults: horizon 1, first origin at half the series.
	res, _ = Backtest(y, linear, nil)
	if res.Origins[0] != 50 || len(res.Origins) != 50 || len(res.ByHorizon) != 1 {
		t.Errorf("defaults: %v", res.Origins[:2])
	}
	// The forecaster must not be able to see the future through capacity.
	_, _ = Backtest(y, func(h []float64, steps int) ([]float64, error) {
		if cap(h) != len(h) {
			t.Fatal("history slice exposes future samples")
		}
		return make([]float64, steps), nil
	}, nil)

	if _, err := Backtest(y, nil, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Error("nil forecaster", err)
	}
	if _, err := Backtest(y[:3], linear, &BacktestOptions{MinTrain: 10}); !errors.Is(err, ErrInsufficientData) {
		t.Error("too short", err)
	}
	if _, err := Backtest(nil, linear, nil); !errors.Is(err, ErrInsufficientData) {
		t.Error("nil series", err)
	}
	if _, err := json.Marshal(res); err != nil {
		t.Error(err)
	}
}

func TestCompareBacktest(t *testing.T) {
	const period = 24
	r := newRand(37)
	n := period * 16
	y := make([]float64, n)
	for i := range y {
		y[i] = seasonSignal(i, period) + r.NormFloat64()
	}
	hw := func(h []float64, steps int) ([]float64, error) {
		fit, err := FitHoltWinters(h, period, &HWOptions{MaxIter: 120})
		if err != nil {
			return nil, err
		}
		return fit.Model.Forecast(steps), nil
	}
	opt := &BacktestOptions{Horizon: period, MinTrain: period * 8, Step: period}
	cmp, err := CompareBacktest(y, hw, SeasonalNaiveForecaster(period), opt)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Model.Overall.N != period*8 || cmp.Baseline.Overall.N != period*8 {
		t.Errorf("scored %d / %d", cmp.Model.Overall.N, cmp.Baseline.Overall.N)
	}
	// The seasonal-naive baseline ignores the trend (0.2*24 = 4.8 per day)
	// and carries the full noise; Holt-Winters must beat it clearly.
	if !(cmp.Skill > 0.5) || !(cmp.SkillMAE > 0.5) || len(cmp.ByHorizon) != period {
		t.Errorf("skill %v / %v", cmp.Skill, cmp.SkillMAE)
	}
	approx(t, "baseline bias", cmp.Baseline.Overall.Bias, -4.8, 0.3)
	approx(t, "skill formula", cmp.Skill, 1-cmp.Model.Overall.RMSE/cmp.Baseline.Overall.RMSE, 1e-12)
	// A model compared with itself has zero skill.
	same, _ := CompareBacktest(y, SeasonalNaiveForecaster(period), SeasonalNaiveForecaster(period), opt)
	if same.Skill != 0 || same.SkillMAE != 0 {
		t.Errorf("self skill %v", same.Skill)
	}
	if _, err := json.Marshal(cmp); err != nil {
		t.Error(err)
	}
	if _, err := CompareBacktest(y[:5], hw, hw, opt); !errors.Is(err, ErrInsufficientData) {
		t.Error("short", err)
	}
}
