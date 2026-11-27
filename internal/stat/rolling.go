package stat

import "math"

// RollingApply evaluates f over a trailing window: out[i] = f(valid values
// of x[i-window+1 .. i]). Windows are truncated at the start of the series.
// out[i] is NaN when the window holds fewer than minValid (at least 1)
// non-NaN values. f receives a scratch slice it may reorder but not retain.
// A window < 1 or nil f yields all NaN.
func RollingApply(x []float64, window, minValid int, f func(valid []float64) float64) []float64 {
	out := make([]float64, len(x))
	if minValid < 1 {
		minValid = 1
	}
	if window < 1 || f == nil {
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
	buf := make([]float64, 0, min(window, len(x)))
	for i := range x {
		start := i - window + 1
		if start < 0 {
			start = 0
		}
		buf = buf[:0]
		for _, v := range x[start : i+1] {
			if v == v {
				buf = append(buf, v)
			}
		}
		if len(buf) < minValid {
			out[i] = math.NaN()
		} else {
			out[i] = f(buf)
		}
	}
	return out
}

// slider is the sum of a sliding window that stays accurate however the
// window moves. A plain running sum (add the newcomer, subtract the leaver)
// is not: once a large value has passed through, the low-order digits it
// swallowed are lost for good, and an infinity turns every later result into
// NaN. The slider therefore
//
//   - accumulates with Neumaier's compensated summation, so adding and later
//     removing a value of any magnitude leaves the remaining sum intact,
//   - keeps infinities out of the sum and only counts them, and
//   - lets the caller rebuild the sum from the window itself once per window
//     length (refresh), which bounds whatever error is left.
type slider struct {
	sum, comp  float64
	n          int // non-NaN values in the window
	pinf, ninf int // of which +Inf / -Inf
}

func (s *slider) acc(t float64) {
	u := s.sum + t
	if math.Abs(s.sum) >= math.Abs(t) {
		s.comp += (s.sum - u) + t
	} else {
		s.comp += (t - u) + s.sum
	}
	s.sum = u
}

func (s *slider) add(v float64) {
	switch {
	case v != v:
		return
	case math.IsInf(v, 1):
		s.pinf++
	case math.IsInf(v, -1):
		s.ninf++
	default:
		s.acc(v)
	}
	s.n++
}

func (s *slider) remove(v float64) {
	switch {
	case v != v:
		return
	case math.IsInf(v, 1):
		s.pinf--
	case math.IsInf(v, -1):
		s.ninf--
	default:
		s.acc(-v)
	}
	s.n--
	if s.n-s.pinf-s.ninf == 0 {
		s.sum, s.comp = 0, 0 // nothing finite left: drop any residue
	}
}

// refresh recomputes the state from the window contents.
func (s *slider) refresh(window []float64) {
	*s = slider{}
	for _, v := range window {
		s.add(v)
	}
}

// total returns the sum of the non-NaN values in the window (0 if none),
// with the usual IEEE result when infinities are present.
func (s *slider) total() float64 {
	switch {
	case s.pinf > 0 && s.ninf > 0:
		return math.NaN()
	case s.pinf > 0:
		return math.Inf(1)
	case s.ninf > 0:
		return math.Inf(-1)
	}
	return s.sum + s.comp
}

// rollingSumCount computes trailing-window sums and valid counts in O(n).
func rollingSumCount(x []float64, window int) (sum []float64, cnt []int) {
	sum = make([]float64, len(x))
	cnt = make([]int, len(x))
	var s slider
	for i, v := range x {
		if i >= window && i%window == 0 {
			s.refresh(x[i-window+1 : i]) // exact restart once per window length
			s.add(v)
		} else {
			s.add(v)
			if j := i - window; j >= 0 {
				s.remove(x[j])
			}
		}
		sum[i], cnt[i] = s.total(), s.n
	}
	return
}

// RollingSum returns the trailing-window sum of non-NaN values (NaN when the
// window holds none). O(n).
func RollingSum(x []float64, window int) []float64 {
	if window < 1 {
		return RollingApply(x, window, 1, nil)
	}
	sum, cnt := rollingSumCount(x, window)
	for i := range sum {
		if cnt[i] == 0 {
			sum[i] = math.NaN()
		}
	}
	return sum
}

// RollingMean returns the trailing-window mean of non-NaN values (NaN when
// the window holds none). O(n).
func RollingMean(x []float64, window int) []float64 {
	if window < 1 {
		return RollingApply(x, window, 1, nil)
	}
	sum, cnt := rollingSumCount(x, window)
	for i := range sum {
		if cnt[i] == 0 {
			sum[i] = math.NaN()
		} else {
			sum[i] /= float64(cnt[i])
		}
	}
	return sum
}

// RollingStd returns the trailing-window sample std (NaN with < 2 values).
func RollingStd(x []float64, window int) []float64 {
	return RollingApply(x, window, 2, Std)
}

// RollingMedian returns the trailing-window median.
func RollingMedian(x []float64, window int) []float64 {
	return RollingApply(x, window, 1, medianInPlace)
}

// RollingMin returns the trailing-window minimum.
func RollingMin(x []float64, window int) []float64 {
	return RollingApply(x, window, 1, Min)
}

// RollingMax returns the trailing-window maximum.
func RollingMax(x []float64, window int) []float64 {
	return RollingApply(x, window, 1, Max)
}

// RollingMeanCentered returns the mean over a window centred on each sample
// (window/2 points on each side, truncated at the ends). NaN where the window
// holds no valid value.
func RollingMeanCentered(x []float64, window int) []float64 {
	out := make([]float64, len(x))
	half := window / 2
	if window < 1 {
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
	// Slide the window [a, b) = [i-half, i+half+1) clipped to the series.
	var s slider
	a, b := 0, 0
	span := 2*half + 1
	for i := range x {
		na, nb := max(i-half, 0), min(i+half+1, len(x))
		if i > 0 && i%span == 0 {
			s.refresh(x[na:nb]) // exact restart once per window length
		} else {
			for ; b < nb; b++ {
				s.add(x[b])
			}
			for ; a < na; a++ {
				s.remove(x[a])
			}
		}
		a, b = na, nb
		if s.n > 0 {
			out[i] = s.total() / float64(s.n)
		} else {
			out[i] = math.NaN()
		}
	}
	return out
}

// Lag shifts a series by k samples: out[i] = x[i-k], NaN where i-k is out of
// range. Negative k shifts the other way (a lead).
func Lag(x []float64, k int) []float64 {
	out := make([]float64, len(x))
	for i := range out {
		j := i - k
		if j < 0 || j >= len(x) {
			out[i] = math.NaN()
		} else {
			out[i] = x[j]
		}
	}
	return out
}

// Diff returns x[i]-x[i-lag] (lag >= 1), NaN for the first lag samples and
// wherever either operand is NaN.
func Diff(x []float64, lag int) []float64 {
	out := make([]float64, len(x))
	for i := range out {
		if lag < 1 || i < lag {
			out[i] = math.NaN()
		} else {
			out[i] = x[i] - x[i-lag]
		}
	}
	return out
}

// FillLinear returns a copy of x in which interior runs of at most maxGap
// consecutive NaN are replaced by linear interpolation between the
// neighbouring valid samples (maxGap <= 0 means any length). Leading and
// trailing NaN are left untouched.
func FillLinear(x []float64, maxGap int) []float64 {
	out := make([]float64, len(x))
	copy(out, x)
	last := -1
	for i, v := range x {
		if v != v {
			continue
		}
		if gap := i - last - 1; last >= 0 && gap > 0 && (maxGap <= 0 || gap <= maxGap) {
			a, b := x[last], v
			for j := last + 1; j < i; j++ {
				out[j] = a + (b-a)*float64(j-last)/float64(i-last)
			}
		}
		last = i
	}
	return out
}

// Window is a fixed-size ring buffer of the most recent samples with cheap
// summary statistics, for streaming use. Create it with NewWindow.
type Window struct {
	buf  []float64
	head int // index of the next write
	n    int // slots filled (<= len(buf))
	s    slider
}

// NewWindow returns a window holding the last size samples (size >= 1).
func NewWindow(size int) *Window {
	if size < 1 {
		size = 1
	}
	return &Window{buf: make([]float64, size)}
}

// Push appends a sample, evicting the oldest when full. NaN is stored as a
// missing slot (it ages out like any other sample).
func (w *Window) Push(v float64) {
	if len(w.buf) == 0 {
		w.buf = make([]float64, 1) // zero value: behave like NewWindow(1)
	}
	if w.n == len(w.buf) {
		w.s.remove(w.buf[w.head])
	} else {
		w.n++
	}
	w.buf[w.head] = v
	w.s.add(v)
	w.head++
	if w.head == len(w.buf) {
		w.head = 0
		// Re-sum once per revolution so rounding errors cannot accumulate.
		w.s.refresh(w.buf[:w.n])
	}
}

// Reset empties the window.
func (w *Window) Reset() { w.head, w.n, w.s = 0, 0, slider{} }

// Len returns the number of samples held, including missing ones.
func (w *Window) Len() int { return w.n }

// Valid returns the number of non-NaN samples held.
func (w *Window) Valid() int { return w.s.n }

// Full reports whether the window has reached its capacity.
func (w *Window) Full() bool { return w.n > 0 && w.n == len(w.buf) }

// Values appends the held samples, oldest first (NaN included), to dst.
func (w *Window) Values(dst []float64) []float64 {
	if w.n < len(w.buf) {
		return append(dst, w.buf[:w.n]...)
	}
	dst = append(dst, w.buf[w.head:]...)
	return append(dst, w.buf[:w.head]...)
}

// Mean returns the mean of the valid samples, NaN if none.
func (w *Window) Mean() float64 {
	if w.s.n == 0 {
		return math.NaN()
	}
	return w.s.total() / float64(w.s.n)
}

// Std returns the sample std of the valid samples (NaN with < 2).
func (w *Window) Std() float64 { return Std(w.buf[:w.n]) }

// Min returns the smallest valid sample, NaN if none.
func (w *Window) Min() float64 { return Min(w.buf[:w.n]) }

// Max returns the largest valid sample, NaN if none.
func (w *Window) Max() float64 { return Max(w.buf[:w.n]) }

// Median returns the median of the valid samples, NaN if none.
func (w *Window) Median() float64 { return Median(w.buf[:w.n]) }

// AlphaFromHalfLife converts a half-life measured in samples into the EWMA
// smoothing factor alpha = 1 - 2^(-1/halfLife). Non-positive or NaN
// half-lives give alpha = 1 (no memory).
func AlphaFromHalfLife(halfLife float64) float64 {
	if !(halfLife > 0) {
		return 1
	}
	return 1 - math.Exp2(-1/halfLife)
}

// EWMA is an exponentially weighted moving average with a matching
// exponentially weighted variance. The zero value is not usable; create it
// with NewEWMA or NewEWMAHalfLife. All fields are exported so the state can
// be persisted as JSON.
type EWMA struct {
	Alpha float64 `json:"alpha"` // weight of the newest sample, (0, 1]
	Mean  float64 `json:"mean"`  // current average (valid when N > 0)
	Var   float64 `json:"var"`   // exponentially weighted variance
	N     int64   `json:"n"`     // samples absorbed
}

// NewEWMA returns an EWMA with the given smoothing factor, clamped to
// (0, 1]; an invalid alpha becomes 1.
func NewEWMA(alpha float64) *EWMA {
	if !(alpha > 0 && alpha <= 1) {
		alpha = 1
	}
	return &EWMA{Alpha: alpha}
}

// NewEWMAHalfLife returns an EWMA whose weights halve every halfLife samples.
func NewEWMAHalfLife(halfLife float64) *EWMA {
	return NewEWMA(AlphaFromHalfLife(halfLife))
}

// Add absorbs one sample and returns the updated average. NaN and +-Inf are
// ignored.
// The first sample initialises the average.
func (e *EWMA) Add(v float64) float64 { return e.AddDT(v, 1) }

// AddDT absorbs a sample that arrived dt sample-intervals after the previous
// one (irregular sampling): the effective weight is 1-(1-Alpha)^dt.
// Non-finite samples (NaN, +-Inf) and non-positive dt are ignored: an
// infinity would otherwise turn the state into NaN for good.
func (e *EWMA) AddDT(v, dt float64) float64 {
	if !IsFinite(v) || !(dt > 0) {
		return e.Value()
	}
	if e.N == 0 {
		e.Mean, e.Var, e.N = v, 0, 1
		return v
	}
	a := e.Alpha
	if dt != 1 {
		a = 1 - math.Pow(1-e.Alpha, dt)
	}
	d := v - e.Mean
	e.Mean += a * d
	e.Var = (1 - a) * (e.Var + a*d*d)
	e.N++
	return e.Mean
}

// Value returns the current average, NaN before the first sample.
func (e *EWMA) Value() float64 {
	if e.N == 0 {
		return math.NaN()
	}
	return e.Mean
}

// Std returns the exponentially weighted standard deviation, NaN before the
// second sample.
func (e *EWMA) Std() float64 {
	if e.N < 2 {
		return math.NaN()
	}
	return math.Sqrt(e.Var)
}

// EWMASeries returns the running EWMA of x. NaN (and +-Inf) inputs do not
// update the average; the output carries the previous average forward (NaN before the
// first valid sample).
func EWMASeries(x []float64, alpha float64) []float64 {
	e := NewEWMA(alpha)
	out := make([]float64, len(x))
	for i, v := range x {
		out[i] = e.Add(v)
	}
	return out
}

// Accumulator is a streaming, mergeable summary (count, mean, variance, min,
// max) using Welford's algorithm. The zero value is ready to use and all
// fields are exported for persistence.
type Accumulator struct {
	N    int64   `json:"n"`
	Avg  float64 `json:"mean"`
	M2   float64 `json:"m2"` // sum of squared deviations from the mean
	Lo   float64 `json:"min"`
	Hi   float64 `json:"max"`
	Last float64 `json:"last"` // most recent sample
}

// Add absorbs a sample; NaN and +-Inf are ignored (an infinity would turn
// the mean and the variance into NaN for good).
func (a *Accumulator) Add(v float64) {
	if !IsFinite(v) {
		return
	}
	if a.N == 0 {
		a.Lo, a.Hi = v, v
	} else {
		if v < a.Lo {
			a.Lo = v
		}
		if v > a.Hi {
			a.Hi = v
		}
	}
	a.N++
	d := v - a.Avg
	a.Avg += d / float64(a.N)
	a.M2 += d * (v - a.Avg)
	a.Last = v
}

// AddAll absorbs every finite element of x.
func (a *Accumulator) AddAll(x []float64) {
	for _, v := range x {
		a.Add(v)
	}
}

// Merge folds another accumulator into a; the result equals having added
// both sample sets to one accumulator (Last is taken from b when b is not
// empty).
func (a *Accumulator) Merge(b Accumulator) {
	if b.N == 0 {
		return
	}
	if a.N == 0 {
		*a = b
		return
	}
	na, nb := float64(a.N), float64(b.N)
	d := b.Avg - a.Avg
	n := na + nb
	a.M2 += b.M2 + d*d*na*nb/n
	a.Avg += d * nb / n
	a.N += b.N
	if b.Lo < a.Lo {
		a.Lo = b.Lo
	}
	if b.Hi > a.Hi {
		a.Hi = b.Hi
	}
	a.Last = b.Last
}

// Reset clears the accumulator.
func (a *Accumulator) Reset() { *a = Accumulator{} }

// Mean returns the mean, NaN when empty.
func (a Accumulator) Mean() float64 {
	if a.N == 0 {
		return math.NaN()
	}
	return a.Avg
}

// Sum returns the sum of the samples (0 when empty).
func (a Accumulator) Sum() float64 { return a.Avg * float64(a.N) }

// Variance returns the sample variance (n-1), NaN for N < 2.
func (a Accumulator) Variance() float64 {
	if a.N < 2 {
		return math.NaN()
	}
	return a.M2 / float64(a.N-1)
}

// PopVariance returns the population variance (n), NaN when empty.
func (a Accumulator) PopVariance() float64 {
	if a.N < 1 {
		return math.NaN()
	}
	return a.M2 / float64(a.N)
}

// Std returns the sample standard deviation, NaN for N < 2.
func (a Accumulator) Std() float64 { return math.Sqrt(a.Variance()) }

// Min returns the smallest sample, NaN when empty.
func (a Accumulator) Min() float64 {
	if a.N == 0 {
		return math.NaN()
	}
	return a.Lo
}

// Max returns the largest sample, NaN when empty.
func (a Accumulator) Max() float64 {
	if a.N == 0 {
		return math.NaN()
	}
	return a.Hi
}
