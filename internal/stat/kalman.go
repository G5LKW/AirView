package stat

import "math"

// LocalLevel is a one-dimensional Kalman filter for the local level
// (random walk plus noise) model:
//
//	level[t] = level[t-1] + w,  var(w) = Q per unit time
//	z[t]     = level[t] + v,    var(v) = R
//
// The ratio Q/R sets the smoothing: small values smooth heavily. The first
// measurement initialises the state. The struct is JSON-safe.
type LocalLevel struct {
	Q     float64 `json:"q"`
	R     float64 `json:"r"`
	Level float64 `json:"level"`
	P     float64 `json:"p"` // variance of Level
	N     int     `json:"n"` // measurements absorbed
}

// NewLocalLevel returns a filter with process variance q and measurement
// variance r (negative or NaN values are treated as 0).
func NewLocalLevel(q, r float64) *LocalLevel {
	return &LocalLevel{Q: nonNeg(q), R: nonNeg(r)}
}

func nonNeg(v float64) float64 {
	if !(v > 0) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// Step advances the filter by dt time units and absorbs measurement z. A NaN
// z (or non-positive dt handled as 0) only propagates the uncertainty. It
// returns the filtered level (NaN before the first measurement).
func (k *LocalLevel) Step(z, dt float64) float64 {
	if !(dt > 0) || math.IsInf(dt, 0) {
		dt = 0
	}
	if k.N == 0 {
		if IsFinite(z) {
			k.Level, k.P, k.N = z, k.R, 1
			return z
		}
		return math.NaN()
	}
	k.P += k.Q * dt
	if IsFinite(z) {
		s := k.P + k.R
		if s > 0 {
			g := k.P / s
			k.Level += g * (z - k.Level)
			k.P *= 1 - g
		}
		k.N++
	}
	return k.Level
}

// Update is Step with dt = 1.
func (k *LocalLevel) Update(z float64) float64 { return k.Step(z, 1) }

// LocalTrend is a Kalman filter for the local linear trend model with state
// (level, trend):
//
//	level[t] = level[t-1] + dt*trend[t-1] + w1,  var(w1) = QLevel*dt
//	trend[t] = trend[t-1] + w2,                  var(w2) = QTrend*dt
//	z[t]     = level[t] + v,                     var(v)  = R
//
// It smooths a noisy series such as barometric pressure and yields its rate
// of change (the tendency) directly as Trend, in units per time step. The
// first measurement initialises the level; the trend starts at 0 with a
// large variance. The struct is JSON-safe.
type LocalTrend struct {
	QLevel float64 `json:"qLevel"`
	QTrend float64 `json:"qTrend"`
	R      float64 `json:"r"`
	Level  float64 `json:"level"`
	Trend  float64 `json:"trend"`
	P00    float64 `json:"p00"` // var(level)
	P01    float64 `json:"p01"` // cov(level, trend)
	P11    float64 `json:"p11"` // var(trend)
	N      int     `json:"n"`   // measurements absorbed
}

// NewLocalTrend returns a local linear trend filter.
func NewLocalTrend(qLevel, qTrend, r float64) *LocalTrend {
	return &LocalTrend{QLevel: nonNeg(qLevel), QTrend: nonNeg(qTrend), R: nonNeg(r)}
}

// trendInitVar is the initial variance of the trend state, relative to the
// measurement scale: large enough to be uninformative.
func (k *LocalTrend) trendInitVar() float64 {
	v := 1e6 * math.Max(k.R, math.Max(k.QLevel, k.QTrend))
	if !(v > 0) {
		v = 1e6
	}
	return v
}

// predict propagates the state by dt time units. An interval longer than one
// time step is treated as floor(dt) unit steps followed by one fractional
// step, so that Step(NaN, 5) or Forecast(5) is exactly the same as five
// missing unit steps. (A single stride of length dt would leave out the
// trend noise that accumulates into the level over the interval and
// understate the forecast variance, the more so the longer the horizon.)
func (k *LocalTrend) predict(dt float64) {
	if n := math.Floor(dt); n >= 1 {
		// Closed form of n unit steps:
		//   level_n = level + n*trend + sum_{i<=n} w1_i + sum_{i<n} (n-i)*w2_i
		k.Level += n * k.Trend
		k.P00 += n*(2*k.P01+n*k.P11) + n*k.QLevel + k.QTrend*(n-1)*n*(2*n-1)/6
		k.P01 += n*k.P11 + k.QTrend*n*(n-1)/2
		k.P11 += n * k.QTrend
		dt -= n
	}
	if dt > 0 {
		k.Level += dt * k.Trend
		k.P00 += dt*(2*k.P01+dt*k.P11) + k.QLevel*dt
		k.P01 += dt * k.P11
		k.P11 += k.QTrend * dt
	}
}

// Step advances the filter by dt time units and absorbs measurement z; NaN
// only predicts. It returns the filtered level and trend (NaN, NaN before
// the first measurement).
func (k *LocalTrend) Step(z, dt float64) (level, trend float64) {
	if !(dt > 0) || math.IsInf(dt, 0) {
		dt = 0
	}
	if k.N == 0 {
		if !IsFinite(z) {
			return math.NaN(), math.NaN()
		}
		k.Level, k.Trend = z, 0
		k.P00, k.P01, k.P11 = k.R, 0, k.trendInitVar()
		k.N = 1
		return k.Level, k.Trend
	}
	k.predict(dt)
	if IsFinite(z) {
		s := k.P00 + k.R
		if s > 0 {
			g0, g1 := k.P00/s, k.P01/s
			e := z - k.Level
			k.Level += g0 * e
			k.Trend += g1 * e
			p00, p01 := k.P00, k.P01
			k.P00 = (1 - g0) * p00
			k.P01 = (1 - g0) * p01
			k.P11 -= g1 * p01
			if k.P11 < 0 {
				k.P11 = 0
			}
		}
		k.N++
	}
	return k.Level, k.Trend
}

// Update is Step with dt = 1.
func (k *LocalTrend) Update(z float64) (level, trend float64) { return k.Step(z, 1) }

// Forecast extrapolates the level h time units ahead and returns the
// standard deviation of a new measurement at that time (state uncertainty
// plus the process noise accumulated over the horizon plus measurement
// noise). For whole h it equals what h missing unit steps would give, so the
// interval widens properly with the horizon. NaN, NaN before the first
// measurement.
func (k *LocalTrend) Forecast(h float64) (mean, std float64) {
	if k.N == 0 || !(h >= 0) || math.IsInf(h, 0) {
		return math.NaN(), math.NaN()
	}
	c := *k
	c.predict(h)
	return c.Level, math.Sqrt(math.Max(c.P00+c.R, 0))
}

// KalmanSmoothLevel runs the local level filter over a regularly sampled
// series and then the Rauch-Tung-Striebel smoother, so every output uses
// the data before and after it. NaN samples are interpolated; outputs
// before the first valid sample are NaN. q and r as in LocalLevel.
func KalmanSmoothLevel(y []float64, q, r float64) []float64 {
	n := len(y)
	out := make([]float64, n)
	xf := make([]float64, n) // filtered state
	pf := make([]float64, n) // filtered variance
	pp := make([]float64, n) // predicted variance for step i (prior)
	k := NewLocalLevel(q, r)
	first := -1
	for i, z := range y {
		if k.N == 0 {
			out[i] = math.NaN()
			if k.Step(z, 1); k.N == 0 {
				continue
			}
			first = i
			xf[i], pf[i], pp[i] = k.Level, k.P, k.P
			continue
		}
		pp[i] = k.P + k.Q
		k.Step(z, 1)
		xf[i], pf[i] = k.Level, k.P
	}
	if first < 0 {
		return out
	}
	out[n-1] = xf[n-1]
	for i := n - 2; i >= first; i-- {
		c := 0.0
		if pp[i+1] > 0 {
			c = pf[i] / pp[i+1]
		}
		// The predicted state for i+1 equals the filtered state at i.
		out[i] = xf[i] + c*(out[i+1]-xf[i])
	}
	return out
}

// KalmanSmoothTrend runs the local linear trend filter and the
// Rauch-Tung-Striebel smoother over a regularly sampled series and returns
// the smoothed level and trend (rate of change per sample). NaN samples are
// bridged; outputs before the first valid sample are NaN.
func KalmanSmoothTrend(y []float64, qLevel, qTrend, r float64) (level, trend []float64) {
	n := len(y)
	level = make([]float64, n)
	trend = make([]float64, n)
	type st struct{ l, b, p00, p01, p11 float64 }
	filt := make([]st, n)
	pred := make([]st, n) // prior for step i
	k := NewLocalTrend(qLevel, qTrend, r)
	first := -1
	for i, z := range y {
		if k.N == 0 {
			level[i], trend[i] = math.NaN(), math.NaN()
			if k.Step(z, 1); k.N == 0 {
				continue
			}
			first = i
			filt[i] = st{k.Level, k.Trend, k.P00, k.P01, k.P11}
			pred[i] = filt[i]
			continue
		}
		c := *k
		c.predict(1)
		pred[i] = st{c.Level, c.Trend, c.P00, c.P01, c.P11}
		k.Step(z, 1)
		filt[i] = st{k.Level, k.Trend, k.P00, k.P01, k.P11}
	}
	if first < 0 {
		return level, trend
	}
	level[n-1], trend[n-1] = filt[n-1].l, filt[n-1].b
	for i := n - 2; i >= first; i-- {
		f, p := filt[i], pred[i+1]
		// C = P_f * F' * inv(P_pred), F = [[1,1],[0,1]].
		a00, a01 := f.p00+f.p01, f.p01 // P_f * F' row 0
		a10, a11 := f.p01+f.p11, f.p11 // row 1
		det := p.p00*p.p11 - p.p01*p.p01
		if !(det > 0) || !IsFinite(det) {
			level[i], trend[i] = f.l, f.b
			continue
		}
		i00, i01, i11 := p.p11/det, -p.p01/det, p.p00/det
		c00 := a00*i00 + a01*i01
		c01 := a00*i01 + a01*i11
		c10 := a10*i00 + a11*i01
		c11 := a10*i01 + a11*i11
		dl, db := level[i+1]-p.l, trend[i+1]-p.b
		level[i] = f.l + c00*dl + c01*db
		trend[i] = f.b + c10*dl + c11*db
	}
	return level, trend
}
