package stat

import (
	"math"
	"slices"
	"sort"
)

// MADScale converts a median absolute deviation into a consistent estimate
// of the standard deviation for normally distributed data.
const MADScale = 1.482602218505602

// Count returns the number of non-NaN values.
func Count(x []float64) int {
	n := 0
	for _, v := range x {
		if v == v {
			n++
		}
	}
	return n
}

// DropNaN returns a new slice with the non-NaN values of x in order.
func DropNaN(x []float64) []float64 {
	out := make([]float64, 0, len(x))
	for _, v := range x {
		if v == v {
			out = append(out, v)
		}
	}
	return out
}

// Sum returns the sum of the non-NaN values (0 for none).
func Sum(x []float64) float64 {
	s := 0.0
	for _, v := range x {
		if v == v {
			s += v
		}
	}
	return s
}

// Mean returns the arithmetic mean of the non-NaN values, NaN if none.
func Mean(x []float64) float64 {
	s, n := 0.0, 0
	for _, v := range x {
		if v == v {
			s += v
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return s / float64(n)
}

// moments returns count, mean and the sum of squared deviations (Welford).
func moments(x []float64) (n int, mean, m2 float64) {
	for _, v := range x {
		if v != v {
			continue
		}
		n++
		d := v - mean
		mean += d / float64(n)
		m2 += d * (v - mean)
	}
	return
}

// Variance returns the sample variance (n-1) of the non-NaN values; NaN when
// fewer than two are present.
func Variance(x []float64) float64 {
	n, _, m2 := moments(x)
	if n < 2 {
		return math.NaN()
	}
	return m2 / float64(n-1)
}

// PopVariance returns the population variance (n); NaN for no data.
func PopVariance(x []float64) float64 {
	n, _, m2 := moments(x)
	if n < 1 {
		return math.NaN()
	}
	return m2 / float64(n)
}

// Std returns the sample standard deviation (n-1); NaN for n < 2.
func Std(x []float64) float64 { return math.Sqrt(Variance(x)) }

// PopStd returns the population standard deviation (n); NaN for no data.
func PopStd(x []float64) float64 { return math.Sqrt(PopVariance(x)) }

// MinMax returns the smallest and largest non-NaN value; ok is false when
// there is none.
func MinMax(x []float64) (lo, hi float64, ok bool) {
	for _, v := range x {
		if v != v {
			continue
		}
		if !ok {
			lo, hi, ok = v, v, true
			continue
		}
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return
}

// Min returns the smallest non-NaN value, NaN if none.
func Min(x []float64) float64 {
	lo, _, ok := MinMax(x)
	if !ok {
		return math.NaN()
	}
	return lo
}

// Max returns the largest non-NaN value, NaN if none.
func Max(x []float64) float64 {
	_, hi, ok := MinMax(x)
	if !ok {
		return math.NaN()
	}
	return hi
}

// sortedCopy returns the non-NaN values of x in ascending order.
func sortedCopy(x []float64) []float64 {
	s := DropNaN(x)
	slices.Sort(s)
	return s
}

// QuantileSorted returns the p-quantile (0 <= p <= 1) of an ascending slice
// that contains no NaN, using linear interpolation between order statistics
// (type 7). It returns NaN for an empty slice or p outside [0, 1].
func QuantileSorted(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 || !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	h := float64(n-1) * p
	i := int(h)
	if i >= n-1 {
		return sorted[n-1]
	}
	f := h - float64(i)
	a, b := sorted[i], sorted[i+1]
	if f == 0 || a == b {
		// Exactly on an order statistic (or between two equal ones): no
		// interpolation, so a neighbouring infinity cannot turn the result
		// into NaN through 0*Inf or Inf-Inf.
		return a
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return (1-f)*a + f*b
	}
	return a + f*(b-a)
}

// Quantile returns the p-quantile of the non-NaN values of x (x is not
// modified). NaN for no data or p outside [0, 1].
func Quantile(x []float64, p float64) float64 {
	return QuantileSorted(sortedCopy(x), p)
}

// Quantiles returns several quantiles with a single sort.
func Quantiles(x []float64, ps ...float64) []float64 {
	s := sortedCopy(x)
	out := make([]float64, len(ps))
	for i, p := range ps {
		out[i] = QuantileSorted(s, p)
	}
	return out
}

// Median returns the median of the non-NaN values, NaN if none.
func Median(x []float64) float64 { return Quantile(x, 0.5) }

// medianInPlace sorts v (which must not contain NaN) and returns its median.
func medianInPlace(v []float64) float64 {
	slices.Sort(v)
	return QuantileSorted(v, 0.5)
}

// MAD returns the raw median absolute deviation from the median of the
// non-NaN values (multiply by MADScale for a robust sigma). NaN if no data.
func MAD(x []float64) float64 {
	s := sortedCopy(x)
	if len(s) == 0 {
		return math.NaN()
	}
	med := QuantileSorted(s, 0.5)
	for i, v := range s {
		s[i] = math.Abs(v - med)
	}
	return medianInPlace(s)
}

// RobustStd returns MADScale*MAD(x), a robust estimate of sigma.
func RobustStd(x []float64) float64 { return MADScale * MAD(x) }

// WeightedMean returns sum(w*x)/sum(w) over indices where x and w are finite
// and w > 0. NaN when the lengths differ or no weight remains.
func WeightedMean(x, w []float64) float64 {
	if len(x) != len(w) {
		return math.NaN()
	}
	sw, s := 0.0, 0.0
	for i, v := range x {
		wi := w[i]
		if v != v || !(wi > 0) || math.IsInf(wi, 0) {
			continue
		}
		sw += wi
		s += wi * v
	}
	if !(sw > 0) {
		return math.NaN()
	}
	return s / sw
}

// WeightedQuantile returns the weighted p-quantile over indices where x and
// w are finite and w > 0: the smallest value whose cumulative weight reaches
// p of the total weight (the inverse of the weighted empirical distribution
// function). When the cumulative weight meets the target exactly, the value
// is averaged with the next larger one, so equal weights reproduce the
// ordinary median at p = 0.5. A value holding more than half of the weight
// is always the weighted median. NaN when undefined.
func WeightedQuantile(x, w []float64, p float64) float64 {
	if len(x) != len(w) || !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	type xw struct{ x, w float64 }
	pts := make([]xw, 0, len(x))
	total := 0.0
	for i, v := range x {
		wi := w[i]
		if v != v || !(wi > 0) || math.IsInf(wi, 0) {
			continue
		}
		pts = append(pts, xw{v, wi})
		total += wi
	}
	if len(pts) == 0 || !(total > 0) || math.IsInf(total, 0) {
		return math.NaN()
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].x < pts[j].x })
	target := p * total
	tol := 1e-12 * total
	cum := 0.0
	for i, q := range pts {
		cum += q.w
		if cum < target-tol {
			continue
		}
		if cum <= target+tol && i+1 < len(pts) {
			return (q.x + pts[i+1].x) / 2
		}
		return q.x
	}
	return pts[len(pts)-1].x
}

// TrimmedMean returns the mean after discarding floor(frac*n) values at each
// end of the sorted non-NaN data. frac is clamped to [0, 0.5]; at 0.5 (or
// when everything would be trimmed) the median is returned. NaN if no data.
func TrimmedMean(x []float64, frac float64) float64 {
	s := sortedCopy(x)
	n := len(s)
	if n == 0 || frac != frac {
		return math.NaN()
	}
	frac = clamp(frac, 0, 0.5)
	// The small offset keeps products such as 0.29*100 = 28.999999999999996
	// from losing a whole observation to binary rounding.
	k := int(frac*float64(n) + 1e-9)
	if 2*k >= n {
		return QuantileSorted(s, 0.5)
	}
	sum := 0.0
	for _, v := range s[k : n-k] {
		sum += v
	}
	return sum / float64(n-2*k)
}

// ZScores returns (x-mean)/std (sample std). NaN inputs stay NaN; infinities
// are treated as missing too (their score is NaN and they do not enter the
// mean and std, which they would turn into NaN for every sample). If fewer
// than two valid values exist every output is NaN; if the std is zero the
// valid outputs are 0.
func ZScores(x []float64) []float64 {
	out := make([]float64, len(x))
	x = infToNaN(x)
	n, mean, m2 := moments(x)
	if n < 2 {
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
	sd := math.Sqrt(m2 / float64(n-1))
	for i, v := range x {
		switch {
		case v != v:
			out[i] = math.NaN()
		case sd == 0:
			out[i] = 0
		default:
			out[i] = (v - mean) / sd
		}
	}
	return out
}

// robustCenterScale returns the median and a robust sigma: MADScale*MAD, or,
// when the MAD is zero (more than half the values identical), the mean
// absolute deviation scaled for normal data, or 0 if that vanishes too.
func robustCenterScale(x []float64) (center, scale float64, ok bool) {
	s := sortedCopy(x)
	if len(s) == 0 {
		return 0, 0, false
	}
	center = QuantileSorted(s, 0.5)
	sumAbs := 0.0
	for i, v := range s {
		s[i] = math.Abs(v - center)
		sumAbs += s[i]
	}
	scale = MADScale * medianInPlace(s)
	if scale == 0 {
		scale = 1.2533141373155 * sumAbs / float64(len(s))
	}
	return center, scale, true
}

// RobustZScores returns (x-median)/(MADScale*MAD). NaN inputs stay NaN. When
// the MAD is zero the scaled mean absolute deviation is used instead; if the
// data is constant the valid outputs are 0.
func RobustZScores(x []float64) []float64 {
	out := make([]float64, len(x))
	c, s, ok := robustCenterScale(x)
	for i, v := range x {
		switch {
		case !ok || v != v:
			out[i] = math.NaN()
		case s == 0:
			out[i] = 0
		default:
			out[i] = (v - c) / s
		}
	}
	return out
}

// Summary is a five-number style description of a sample. All fields are 0
// when N is 0; Std is 0 when N < 2.
type Summary struct {
	N      int     `json:"n"`
	Mean   float64 `json:"mean"`
	Std    float64 `json:"std"`
	Min    float64 `json:"min"`
	Q1     float64 `json:"q1"`
	Median float64 `json:"median"`
	Q3     float64 `json:"q3"`
	Max    float64 `json:"max"`
}

// Describe summarises the finite values of x (NaN and infinities are
// skipped, so the result is always JSON-safe).
func Describe(x []float64) Summary {
	s := make([]float64, 0, len(x))
	for _, v := range x {
		if IsFinite(v) {
			s = append(s, v)
		}
	}
	slices.Sort(s)
	n := len(s)
	if n == 0 {
		return Summary{}
	}
	_, mean, m2 := moments(s)
	out := Summary{
		N: n, Mean: mean, Min: s[0], Max: s[n-1],
		Q1: QuantileSorted(s, 0.25), Median: QuantileSorted(s, 0.5), Q3: QuantileSorted(s, 0.75),
	}
	if n > 1 {
		out.Std = math.Sqrt(m2 / float64(n-1))
	}
	return out
}

// Number is the set of element types accepted for histogram counts.
type Number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// histLocate finds the bin and the fractional position inside it at which the
// cumulative count reaches p*total. Negative and NaN counts count as 0.
func histLocate[T Number](counts []T, p float64) (bin int, frac float64, ok bool) {
	if !(p >= 0 && p <= 1) {
		return 0, 0, false
	}
	total := 0.0
	last := -1
	for i, c := range counts {
		if f := float64(c); f > 0 {
			total += f
			last = i
		}
	}
	if last < 0 || math.IsInf(total, 0) {
		return 0, 0, false
	}
	target := p * total
	cum := 0.0
	for i, c := range counts {
		f := float64(c)
		if !(f > 0) {
			continue
		}
		if cum+f >= target {
			return i, clamp((target-cum)/f, 0, 1), true
		}
		cum += f
	}
	return last, 1, true
}

// HistQuantile returns the p-quantile of data that was binned into equal
// width bins, bin i covering [lo+i*width, lo+(i+1)*width). Values are assumed
// uniformly spread inside a bin (linear interpolation). p = 0 yields the
// lower edge of the first occupied bin and p = 1 the upper edge of the last.
// NaN for an empty histogram or p outside [0, 1].
func HistQuantile[T Number](counts []T, lo, width, p float64) float64 {
	bin, frac, ok := histLocate(counts, p)
	if !ok {
		return math.NaN()
	}
	return lo + (float64(bin)+frac)*width
}

// HistQuantileEdges is HistQuantile for arbitrary bin edges; bin i covers
// [edges[i], edges[i+1]) and len(edges) must be len(counts)+1.
func HistQuantileEdges[T Number](counts []T, edges []float64, p float64) float64 {
	if len(edges) != len(counts)+1 {
		return math.NaN()
	}
	bin, frac, ok := histLocate(counts, p)
	if !ok {
		return math.NaN()
	}
	return edges[bin] + frac*(edges[bin+1]-edges[bin])
}

// HistMean returns the mean of equal-width binned data using bin centres.
// NaN for an empty histogram.
func HistMean[T Number](counts []T, lo, width float64) float64 {
	total, s := 0.0, 0.0
	for i, c := range counts {
		if f := float64(c); f > 0 {
			total += f
			s += f * (lo + (float64(i)+0.5)*width)
		}
	}
	if !(total > 0) {
		return math.NaN()
	}
	return s / total
}

// Histogram counts the non-NaN values of x into bins equal-width bins that
// start at lo. Values below lo or at/above lo+bins*width are tallied in under
// and over instead. A nil slice is returned for bins < 1 or width <= 0.
func Histogram(x []float64, lo, width float64, bins int) (counts []float64, under, over int) {
	if bins < 1 || !(width > 0) {
		return nil, 0, 0
	}
	counts = make([]float64, bins)
	for _, v := range x {
		if v != v {
			continue
		}
		f := math.Floor((v - lo) / width)
		switch {
		case f < 0:
			under++
		case f >= float64(bins):
			over++
		default:
			counts[int(f)]++
		}
	}
	return
}
