package stat

import (
	"math"
	"slices"
	"sort"
)

// CorrResult is a correlation coefficient with its significance test.
type CorrResult struct {
	R  float64 `json:"r"`  // coefficient in [-1, 1]
	N  int     `json:"n"`  // complete observations used
	DF int     `json:"df"` // degrees of freedom of the t test
	T  float64 `json:"t"`  // t statistic (capped at +-1e300 for |R| = 1)
	P  float64 `json:"p"`  // two-sided p-value
}

func newCorrResult(r float64, n, df int) CorrResult {
	r = clamp(r, -1, 1)
	res := CorrResult{R: r, N: n, DF: df}
	den := 1 - r*r
	if den <= 0 {
		res.T = math.Copysign(1e300, r)
		res.P = 0
		return res
	}
	res.T = r * math.Sqrt(float64(df)/den)
	res.P = StudentTTwoSidedP(res.T, float64(df))
	return res
}

// completePairs returns the pairwise-complete (finite) observations.
func completePairs(x, y []float64) (xs, ys []float64, err error) {
	if len(x) != len(y) {
		return nil, nil, ErrDimension
	}
	xs = make([]float64, 0, len(x))
	ys = make([]float64, 0, len(x))
	for i, v := range x {
		if IsFinite(v) && IsFinite(y[i]) {
			xs = append(xs, v)
			ys = append(ys, y[i])
		}
	}
	return xs, ys, nil
}

// pearsonComplete computes r for NaN-free equally long slices; ok is false
// when either variance is zero or n < 2.
func pearsonComplete(xs, ys []float64) (r float64, ok bool) {
	n := len(xs)
	if n < 2 {
		return 0, false
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(n)
	my /= float64(n)
	var sxx, syy, sxy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxx += dx * dx
		syy += dy * dy
		sxy += dx * dy
	}
	if !(sxx > 0) || !(syy > 0) {
		return 0, false
	}
	return clamp(sxy/math.Sqrt(sxx*syy), -1, 1), true
}

// Covariance returns the sample covariance (n-1) over pairwise-complete
// observations; NaN when fewer than two pairs exist or the lengths differ.
func Covariance(x, y []float64) float64 {
	xs, ys, err := completePairs(x, y)
	if err != nil || len(xs) < 2 {
		return math.NaN()
	}
	mx, my := Mean(xs), Mean(ys)
	s := 0.0
	for i := range xs {
		s += (xs[i] - mx) * (ys[i] - my)
	}
	return s / float64(len(xs)-1)
}

// Pearson returns the product-moment correlation of the pairwise-complete
// observations with a two-sided p-value from the t distribution (df = n-2).
// Errors: ErrDimension (lengths differ), ErrInsufficientData (n < 3),
// ErrDegenerate (a series is constant).
func Pearson(x, y []float64) (CorrResult, error) {
	xs, ys, err := completePairs(x, y)
	if err != nil {
		return CorrResult{}, err
	}
	if len(xs) < 3 {
		return CorrResult{N: len(xs)}, ErrInsufficientData
	}
	r, ok := pearsonComplete(xs, ys)
	if !ok {
		return CorrResult{N: len(xs)}, ErrDegenerate
	}
	return newCorrResult(r, len(xs), len(xs)-2), nil
}

// Ranks returns the 1-based ranks of x with ties sharing their average rank.
// NaN inputs stay NaN and do not take part in the ranking.
func Ranks(x []float64) []float64 {
	out := make([]float64, len(x))
	idx := make([]int, 0, len(x))
	for i, v := range x {
		if v != v {
			out[i] = math.NaN()
		} else {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool { return x[idx[a]] < x[idx[b]] })
	for i := 0; i < len(idx); {
		j := i + 1
		for j < len(idx) && x[idx[j]] == x[idx[i]] {
			j++
		}
		r := float64(i+j+1) / 2 // average of ranks i+1 .. j
		for k := i; k < j; k++ {
			out[idx[k]] = r
		}
		i = j
	}
	return out
}

// Spearman returns the rank correlation (Pearson on average ranks) of the
// pairwise-complete observations, with the usual t approximation for the
// p-value. Errors as for Pearson.
func Spearman(x, y []float64) (CorrResult, error) {
	xs, ys, err := completePairs(x, y)
	if err != nil {
		return CorrResult{}, err
	}
	if len(xs) < 3 {
		return CorrResult{N: len(xs)}, ErrInsufficientData
	}
	r, ok := pearsonComplete(Ranks(xs), Ranks(ys))
	if !ok {
		return CorrResult{N: len(xs)}, ErrDegenerate
	}
	return newCorrResult(r, len(xs), len(xs)-2), nil
}

// PartialCorr returns the correlation of x and y after removing the linear
// effect of the covariate z from both, using observations where all three
// are finite (df = n-3). Errors: ErrDimension, ErrInsufficientData (n < 4),
// ErrDegenerate (a constant series, or x or y collinear with z).
func PartialCorr(x, y, z []float64) (CorrResult, error) {
	if len(x) != len(y) || len(x) != len(z) {
		return CorrResult{}, ErrDimension
	}
	var xs, ys, zs []float64
	for i := range x {
		if IsFinite(x[i]) && IsFinite(y[i]) && IsFinite(z[i]) {
			xs = append(xs, x[i])
			ys = append(ys, y[i])
			zs = append(zs, z[i])
		}
	}
	n := len(xs)
	if n < 4 {
		return CorrResult{N: n}, ErrInsufficientData
	}
	rxy, ok1 := pearsonComplete(xs, ys)
	rxz, ok2 := pearsonComplete(xs, zs)
	ryz, ok3 := pearsonComplete(ys, zs)
	if !ok1 || !ok2 || !ok3 {
		return CorrResult{N: n}, ErrDegenerate
	}
	den := (1 - rxz*rxz) * (1 - ryz*ryz)
	if !(den > 1e-14) {
		return CorrResult{N: n}, ErrDegenerate
	}
	return newCorrResult((rxy-rxz*ryz)/math.Sqrt(den), n, n-3), nil
}

// LagCorr is the correlation at one lag of a cross-correlation function.
type LagCorr struct {
	Lag int     `json:"lag"`
	R   float64 `json:"r"` // NaN (from CrossCorr) when undefined at this lag
	N   int     `json:"n"` // complete pairs at this lag
}

// lagPearson correlates x[t] with y[t+lag] over all t where both are finite.
func lagPearson(x, y []float64, lag int) (r float64, n int) {
	lo, hi := 0, len(x)
	if lag < 0 {
		lo = -lag
	}
	if len(y)-lag < hi {
		hi = len(y) - lag
	}
	var mx, my float64
	for t := lo; t < hi; t++ {
		a, b := x[t], y[t+lag]
		if IsFinite(a) && IsFinite(b) {
			mx += a
			my += b
			n++
		}
	}
	if n < 3 {
		return math.NaN(), n
	}
	mx /= float64(n)
	my /= float64(n)
	var sxx, syy, sxy float64
	for t := lo; t < hi; t++ {
		a, b := x[t], y[t+lag]
		if IsFinite(a) && IsFinite(b) {
			dx, dy := a-mx, b-my
			sxx += dx * dx
			syy += dy * dy
			sxy += dx * dy
		}
	}
	if !(sxx > 0) || !(syy > 0) {
		return math.NaN(), n
	}
	return clamp(sxy/math.Sqrt(sxx*syy), -1, 1), n
}

// CrossCorr returns the lagged Pearson correlation of two equally sampled
// series for every lag in [-maxLag, +maxLag]. At lag k, x[t] is paired with
// y[t+k]: a peak at a positive lag means y follows x by k samples. Each lag
// uses its own pairwise-complete observations; R is NaN where fewer than
// three pairs exist or a series is constant. The series may differ in
// length. A negative maxLag is treated as 0.
func CrossCorr(x, y []float64, maxLag int) []LagCorr {
	if maxLag < 0 {
		maxLag = 0
	}
	out := make([]LagCorr, 0, 2*maxLag+1)
	for k := -maxLag; k <= maxLag; k++ {
		r, n := lagPearson(x, y, k)
		out = append(out, LagCorr{Lag: k, R: r, N: n})
	}
	return out
}

// BestLag returns the lag within +-maxLag with the largest |R| among lags
// having at least minN complete pairs (minN < 3 is raised to 3). Ties are
// resolved towards the smallest |lag|. ErrInsufficientData is returned when
// no lag qualifies.
func BestLag(x, y []float64, maxLag, minN int) (LagCorr, error) {
	if minN < 3 {
		minN = 3
	}
	best := LagCorr{}
	found := false
	for _, c := range CrossCorr(x, y, maxLag) {
		if c.R != c.R || c.N < minN {
			continue
		}
		better := !found || math.Abs(c.R) > math.Abs(best.R)+1e-12 ||
			(math.Abs(math.Abs(c.R)-math.Abs(best.R)) <= 1e-12 && iabs(c.Lag) < iabs(best.Lag))
		if better {
			best, found = c, true
		}
	}
	if !found {
		return LagCorr{}, ErrInsufficientData
	}
	return best, nil
}

func iabs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Autocorr returns the sample autocorrelation function r[0..maxLag] of x
// (r[0] = 1). Missing values are skipped: the lag-k autocovariance is the
// mean product over available pairs, scaled by (1-k/n) like the usual biased
// estimator. nil is returned when fewer than two valid values exist or the
// variance is zero.
func Autocorr(x []float64, maxLag int) []float64 {
	c := autocov(x, maxLag)
	if c == nil {
		return nil
	}
	out := make([]float64, len(c))
	for k, v := range c {
		out[k] = v / c[0]
	}
	return out
}

// autocov returns autocovariances c[0..maxLag] (see Autocorr), nil when the
// series has no variance.
func autocov(x []float64, maxLag int) []float64 {
	x = infToNaN(x) // infinities are missing values, like NaN
	n, mean, m2 := moments(x)
	if n < 2 || !(m2 > 0) {
		return nil
	}
	if maxLag < 0 {
		maxLag = 0
	}
	if maxLag > len(x)-1 {
		maxLag = len(x) - 1
	}
	c := make([]float64, maxLag+1)
	for k := 0; k <= maxLag; k++ {
		s, pairs := 0.0, 0
		for t := 0; t+k < len(x); t++ {
			a, b := x[t], x[t+k]
			if a == a && b == b {
				s += (a - mean) * (b - mean)
				pairs++
			}
		}
		if pairs > 0 {
			c[k] = s / float64(pairs) * (1 - float64(k)/float64(len(x)))
		}
	}
	return c
}

// BinOptions configures BinnedResponse.
type BinOptions struct {
	Bins       int       // number of bins; default 10
	Lo, Hi     float64   // x range of equal-width bins; the data range is used unless Lo < Hi
	EqualCount bool      // equal-population (quantile) bins instead of equal-width bins
	Quantiles  []float64 // y quantiles reported per bin; default {0.1, 0.5, 0.9}
	MinCount   int       // bins with fewer points are omitted; default 1
}

// ResponseBin describes y for the observations whose x fell into one bin.
type ResponseBin struct {
	Lo    float64   `json:"lo"`    // lower x edge (equal-count bins: smallest x)
	Hi    float64   `json:"hi"`    // upper x edge (equal-count bins: largest x)
	N     int       `json:"n"`     // observations in the bin
	XMean float64   `json:"xMean"` // mean x of the bin
	YMean float64   `json:"yMean"` // mean y
	YStd  float64   `json:"yStd"`  // sample std of y (0 when N < 2)
	YQ    []float64 `json:"yq"`    // y quantiles, parallel to BinOptions.Quantiles
}

// BinnedResponse is a simple non-parametric response curve: it bins the
// pairwise-complete observations by x and reports the mean, spread and
// quantiles of y in each bin. With equal-width bins, x outside [Lo, Hi] is
// ignored and x == Hi belongs to the last bin. Bins holding fewer than
// MinCount points are omitted, so the result is always JSON-safe.
// Errors: ErrDimension, ErrInsufficientData (no complete pair),
// ErrInvalidArgument (a quantile outside [0, 1]).
func BinnedResponse(x, y []float64, opt *BinOptions) ([]ResponseBin, error) {
	o := BinOptions{}
	if opt != nil {
		o = *opt
	}
	if o.Bins < 1 {
		o.Bins = 10
	}
	if o.MinCount < 1 {
		o.MinCount = 1
	}
	if o.Quantiles == nil {
		o.Quantiles = []float64{0.1, 0.5, 0.9}
	}
	for _, q := range o.Quantiles {
		if !(q >= 0 && q <= 1) {
			return nil, ErrInvalidArgument
		}
	}
	xs, ys, err := completePairs(x, y)
	if err != nil {
		return nil, err
	}
	if len(xs) == 0 {
		return nil, ErrInsufficientData
	}

	makeBin := func(lo, hi float64, bx, by []float64) ResponseBin {
		b := ResponseBin{Lo: lo, Hi: hi, N: len(by), XMean: Mean(bx), YMean: Mean(by)}
		if len(by) > 1 {
			b.YStd = Std(by)
		}
		slices.Sort(by)
		b.YQ = make([]float64, len(o.Quantiles))
		for i, q := range o.Quantiles {
			b.YQ[i] = QuantileSorted(by, q)
		}
		return b
	}

	var out []ResponseBin
	if o.EqualCount {
		idx := make([]int, len(xs))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return xs[idx[a]] < xs[idx[b]] })
		n := len(idx)
		bins := o.Bins
		if bins > n {
			bins = n
		}
		for b := 0; b < bins; b++ {
			a, e := b*n/bins, (b+1)*n/bins
			if e-a < o.MinCount {
				continue
			}
			bx := make([]float64, 0, e-a)
			by := make([]float64, 0, e-a)
			for _, i := range idx[a:e] {
				bx = append(bx, xs[i])
				by = append(by, ys[i])
			}
			out = append(out, makeBin(bx[0], bx[len(bx)-1], bx, by))
		}
		return out, nil
	}

	lo, hi := o.Lo, o.Hi
	if !(lo < hi) || !IsFinite(lo) || !IsFinite(hi) {
		lo, hi, _ = MinMax(xs)
		if lo == hi { // all x identical: one bin of nominal width
			hi = lo + 1
			o.Bins = 1
		}
	}
	width := (hi - lo) / float64(o.Bins)
	bxs := make([][]float64, o.Bins)
	bys := make([][]float64, o.Bins)
	for i, v := range xs {
		if v < lo || v > hi {
			continue
		}
		b := int((v - lo) / width)
		if b >= o.Bins {
			b = o.Bins - 1
		}
		bxs[b] = append(bxs[b], v)
		bys[b] = append(bys[b], ys[i])
	}
	for b := 0; b < o.Bins; b++ {
		if len(bys[b]) < o.MinCount {
			continue
		}
		out = append(out, makeBin(lo+float64(b)*width, lo+float64(b+1)*width, bxs[b], bys[b]))
	}
	return out, nil
}
