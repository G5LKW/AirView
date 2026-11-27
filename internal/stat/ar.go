package stat

import "math"

// ARModel is an autoregressive model of order len(Coef):
//
//	x[t] = Const + Coef[0]*x[t-1] + ... + Coef[p-1]*x[t-p] + e,  var(e) = Sigma2
//
// It is typically fitted to the residuals of a seasonal model to capture
// their short-term persistence. The struct is JSON-safe.
type ARModel struct {
	Coef   []float64 `json:"coef"`
	Const  float64   `json:"const"`
	Mean   float64   `json:"mean"`   // sample mean of the training data (used to impute missing history)
	Sigma2 float64   `json:"sigma2"` // innovation variance
	N      int       `json:"n"`      // valid observations (Yule-Walker) or regression rows (least squares)
}

// FitARYuleWalker fits an AR(p) model by solving the Yule-Walker equations
// with the Levinson-Durbin recursion. Missing values are skipped when the
// autocovariances are estimated (see Autocorr), so gaps are harmless. The
// result is always stationary. Errors: ErrInvalidArgument (p < 1),
// ErrInsufficientData (fewer than p+2 valid values), ErrDegenerate (zero
// variance).
func FitARYuleWalker(x []float64, p int) (*ARModel, error) {
	if p < 1 {
		return nil, ErrInvalidArgument
	}
	x = infToNaN(x)
	n := Count(x)
	if n < p+2 || len(x) <= p {
		return nil, ErrInsufficientData
	}
	c := autocov(x, p)
	if c == nil || len(c) < p+1 {
		return nil, ErrDegenerate
	}
	phi := make([]float64, p)
	prev := make([]float64, p)
	e := c[0]
	for k := 1; k <= p; k++ {
		acc := c[k]
		for j := 1; j < k; j++ {
			acc -= phi[j-1] * c[k-j]
		}
		refl := acc / e
		copy(prev, phi)
		phi[k-1] = refl
		for j := 0; j < k-1; j++ {
			phi[j] = prev[j] - refl*prev[k-2-j]
		}
		e *= 1 - refl*refl
		if !(e > 0) {
			// Perfectly predictable series: no innovation variance left.
			e = 0
			break
		}
	}
	mean := Mean(x)
	m := &ARModel{Coef: phi, Mean: mean, Sigma2: e, N: n}
	s := 0.0
	for _, v := range phi {
		if !IsFinite(v) {
			return nil, ErrDegenerate
		}
		s += v
	}
	m.Const = mean * (1 - s)
	return m, nil
}

// infToNaN returns x with every infinity replaced by NaN, so that non-finite
// samples are uniformly treated as missing. x itself is returned (not
// copied) when it holds no infinity.
func infToNaN(x []float64) []float64 {
	for i, v := range x {
		if math.IsInf(v, 0) {
			out := make([]float64, len(x))
			copy(out, x)
			for j := i; j < len(out); j++ {
				if math.IsInf(out[j], 0) {
					out[j] = math.NaN()
				}
			}
			return out
		}
	}
	return x
}

// FitARLeastSquares fits an AR(p) model with intercept by ordinary least
// squares on all rows whose target and p lagged values are present. Unlike
// Yule-Walker it is unbiased for strongly persistent series but does not
// guarantee stationarity. Errors: ErrInvalidArgument (p < 1),
// ErrInsufficientData, ErrSingular.
func FitARLeastSquares(x []float64, p int) (*ARModel, error) {
	if p < 1 {
		return nil, ErrInvalidArgument
	}
	var rows [][]float64
	var ys []float64
outer:
	for t := p; t < len(x); t++ {
		if !IsFinite(x[t]) {
			continue
		}
		for j := 1; j <= p; j++ {
			if !IsFinite(x[t-j]) {
				continue outer
			}
		}
		r := make([]float64, p)
		for j := 1; j <= p; j++ {
			r[j-1] = x[t-j]
		}
		rows = append(rows, r)
		ys = append(ys, x[t])
	}
	res, err := OLS(rows, ys, true)
	if err != nil {
		return nil, err
	}
	return &ARModel{
		Coef:   append([]float64(nil), res.Coef[1:]...),
		Const:  res.Coef[0],
		Mean:   Mean(infToNaN(x)),
		Sigma2: res.SSE / float64(res.DF),
		N:      res.N,
	}, nil
}

// Forecast returns the next h values following history (oldest first). Only
// the last p samples are used; missing or absent ones are replaced by the
// training mean. nil for h < 1.
func (m *ARModel) Forecast(history []float64, h int) []float64 {
	if h < 1 {
		return nil
	}
	p := len(m.Coef)
	buf := make([]float64, p+h) // buf[p+k] is forecast k; buf[p-j] is lag j
	for j := 1; j <= p; j++ {
		v := m.Mean
		if i := len(history) - j; i >= 0 && IsFinite(history[i]) {
			v = history[i]
		}
		buf[p-j] = v
	}
	for k := 0; k < h; k++ {
		v := m.Const
		for j := 1; j <= p; j++ {
			v += m.Coef[j-1] * buf[p+k-j]
		}
		buf[p+k] = v
	}
	return buf[p:]
}

// ForecastStd returns the forecast error standard deviation for horizons
// 1..h from the psi-weights of the model: var_k = Sigma2 * sum_{j<k} psi_j^2.
func (m *ARModel) ForecastStd(h int) []float64 {
	if h < 1 {
		return nil
	}
	p := len(m.Coef)
	psi := make([]float64, h)
	out := make([]float64, h)
	acc := 0.0
	for k := 0; k < h; k++ {
		if k == 0 {
			psi[0] = 1
		} else {
			for j := 1; j <= p && j <= k; j++ {
				psi[k] += m.Coef[j-1] * psi[k-j]
			}
		}
		acc += psi[k] * psi[k]
		out[k] = math.Sqrt(m.Sigma2 * acc)
	}
	return out
}

// AnomalyScores returns robust z-scores of residuals: (r - median) /
// (MADScale * MAD). |score| > 3.5 is a common outlier threshold. It is
// RobustZScores under a descriptive name; NaN residuals stay NaN.
func AnomalyScores(resid []float64) []float64 { return RobustZScores(resid) }

// RollingAnomalyScores scores each sample against the robust centre and
// scale of the window preceding it (the sample itself is excluded), so a
// level shift or outlier is judged against recent history only. The score
// is NaN where the sample is missing or the preceding window holds fewer
// than minValid (at least 3) valid samples; it is 0 when the baseline has no
// spread and the sample equals its median, +-Inf is never produced (the
// score is capped at +-1e6).
func RollingAnomalyScores(x []float64, window, minValid int) []float64 {
	out := make([]float64, len(x))
	if minValid < 3 {
		minValid = 3
	}
	for i, v := range x {
		out[i] = math.NaN()
		if v != v || window < 1 {
			continue
		}
		start := max(i-window, 0)
		if Count(x[start:i]) < minValid {
			continue
		}
		c, s, ok := robustCenterScale(x[start:i])
		if !ok {
			continue
		}
		switch {
		case s > 0:
			out[i] = clamp((v-c)/s, -1e6, 1e6)
		case v == c:
			out[i] = 0
		default:
			out[i] = math.Copysign(1e6, v-c)
		}
	}
	return out
}

// ChangePoint describes the most likely single shift in the mean of a series.
type ChangePoint struct {
	Index  int     `json:"index"`  // first sample of the second segment
	Before float64 `json:"before"` // median before the change
	After  float64 `json:"after"`  // median from Index on
	Shift  float64 `json:"shift"`  // After - Before
	Score  float64 `json:"score"`  // |shift| in units of its robust standard error; > ~4 is significant
}

// DetectChangePoint finds the split that maximises the standardised
// difference between the mean of the two sides (a CUSUM-type statistic),
// then describes it robustly with segment medians and a pooled MAD. Each
// segment must hold at least minSeg valid samples (minimum 2). NaN samples
// are skipped. ok is false when the series is too short or has no spread.
func DetectChangePoint(x []float64, minSeg int) (cp ChangePoint, ok bool) {
	if minSeg < 2 {
		minSeg = 2
	}
	idx := make([]int, 0, len(x))
	vals := make([]float64, 0, len(x))
	for i, v := range x {
		if IsFinite(v) {
			idx = append(idx, i)
			vals = append(vals, v)
		}
	}
	n := len(vals)
	if n < 2*minSeg {
		return cp, false
	}
	total := 0.0
	for _, v := range vals {
		total += v
	}
	best, bestK := -1.0, -1
	left := 0.0
	for k := 1; k < n; k++ {
		left += vals[k-1]
		if k < minSeg || n-k < minSeg {
			continue
		}
		n1, n2 := float64(k), float64(n-k)
		d := left/n1 - (total-left)/n2
		if stat := d * d * n1 * n2 / (n1 + n2); stat > best {
			best, bestK = stat, k
		}
	}
	if bestK < 0 {
		return cp, false
	}
	a, b := vals[:bestK], vals[bestK:]
	cp.Index = idx[bestK]
	cp.Before, cp.After = Median(a), Median(b)
	cp.Shift = cp.After - cp.Before
	// Pooled robust sigma of the within-segment deviations.
	dev := make([]float64, 0, n)
	for _, v := range a {
		dev = append(dev, v-cp.Before)
	}
	for _, v := range b {
		dev = append(dev, v-cp.After)
	}
	_, s, _ := robustCenterScale(dev)
	if !(s > 0) {
		if cp.Shift == 0 {
			return cp, false
		}
		cp.Score = 1e6
		return cp, true
	}
	se := s * math.Sqrt(1/float64(len(a))+1/float64(len(b)))
	cp.Score = math.Min(math.Abs(cp.Shift)/se, 1e6)
	return cp, true
}

// CUSUM runs a two-sided tabular CUSUM on standardised values z (e.g.
// AnomalyScores of residuals): pos accumulates max(0, pos + z - k) and neg
// accumulates max(0, neg - z - k). An alarm is recorded, and both sums are
// reset, whenever either exceeds h. Typical values: k = 0.5, h = 5. NaN
// samples leave the sums unchanged.
func CUSUM(z []float64, k, h float64) (pos, neg []float64, alarms []int) {
	pos = make([]float64, len(z))
	neg = make([]float64, len(z))
	var p, q float64
	for i, v := range z {
		if v == v {
			p = math.Max(0, p+v-k)
			q = math.Max(0, q-v-k)
		}
		pos[i], neg[i] = p, q
		if h > 0 && (p > h || q > h) {
			alarms = append(alarms, i)
			p, q = 0, 0
		}
	}
	return
}
