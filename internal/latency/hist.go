package latency

import (
	"math"
)

// Log-scale signed histogram of durations in nanoseconds.
//
// |x| below hMinNs falls into the centre (zero) bin; above it the bins are
// logarithmic with hPerDecade bins per decade up to hDecades decades
// (100 ns .. 100 s), mirrored for negative values. The relative bin width is
// 10^(1/40) - 1 = 5.9 %, so a percentile read from the histogram is within
// about +-3 % of the true sample value (exact minimum and maximum are kept
// separately and clamp the interpolation). A histogram is a fixed-size value:
// no allocation, ~2.9 KB.
const (
	hMinNs     = 100.0
	hPerDecade = 40
	hDecades   = 9
	hSide      = hPerDecade * hDecades // bins per sign
	hBins      = 2*hSide + 1
	hZero      = hSide // index of the centre bin
)

type hist struct {
	n        int64
	sum      float64 // ns
	min, max int64   // ns, valid when n > 0
	c        [hBins]uint32
}

func (h *hist) reset() { *h = hist{} }

// binOf returns the bin index of x (ns).
func binOf(x int64) int {
	ax := float64(x)
	if ax < 0 {
		ax = -ax
	}
	if ax < hMinNs {
		return hZero
	}
	i := int(math.Log10(ax/hMinNs) * hPerDecade)
	if i >= hSide {
		i = hSide - 1
	}
	if x > 0 {
		return hZero + 1 + i
	}
	return hZero - 1 - i
}

// binEdges returns the lower and upper edge (ns) of bin b in ascending order.
func binEdges(b int) (lo, hi float64) {
	switch {
	case b == hZero:
		return -hMinNs, hMinNs
	case b > hZero:
		i := b - hZero - 1
		return hMinNs * math.Pow(10, float64(i)/hPerDecade), hMinNs * math.Pow(10, float64(i+1)/hPerDecade)
	default:
		i := hZero - 1 - b
		return -hMinNs * math.Pow(10, float64(i+1)/hPerDecade), -hMinNs * math.Pow(10, float64(i)/hPerDecade)
	}
}

func (h *hist) add(x int64) {
	if h.n == 0 || x < h.min {
		h.min = x
	}
	if h.n == 0 || x > h.max {
		h.max = x
	}
	h.n++
	h.sum += float64(x)
	h.c[binOf(x)]++
}

func (h *hist) merge(o *hist) {
	if o.n == 0 {
		return
	}
	if h.n == 0 || o.min < h.min {
		h.min = o.min
	}
	if h.n == 0 || o.max > h.max {
		h.max = o.max
	}
	h.n += o.n
	h.sum += o.sum
	for i := range h.c {
		h.c[i] += o.c[i]
	}
}

// quantile returns the p-quantile (0..1) in ns, NaN when empty. Linear
// interpolation inside the bin, clamped to the exact min / max.
func (h *hist) quantile(p float64) float64 {
	if h.n == 0 {
		return math.NaN()
	}
	if p <= 0 {
		return float64(h.min)
	}
	if p >= 1 {
		return float64(h.max)
	}
	target := p * float64(h.n)
	var cum float64
	for b := range h.c {
		c := float64(h.c[b])
		if c == 0 {
			continue
		}
		if cum+c >= target {
			lo, hi := binEdges(b)
			v := lo + (hi-lo)*(target-cum)/c
			if v < float64(h.min) {
				v = float64(h.min)
			}
			if v > float64(h.max) {
				v = float64(h.max)
			}
			return v
		}
		cum += c
	}
	return float64(h.max)
}

func (h *hist) mean() float64 {
	if h.n == 0 {
		return math.NaN()
	}
	return h.sum / float64(h.n)
}

// welford is a running mean / variance accumulator.
type welford struct {
	n    int64
	mean float64
	m2   float64
}

func (w *welford) add(x float64) {
	w.n++
	d := x - w.mean
	w.mean += d / float64(w.n)
	w.m2 += d * (x - w.mean)
}

func (w *welford) variance() float64 {
	if w.n < 2 {
		return math.NaN()
	}
	return w.m2 / float64(w.n-1)
}

func (w *welford) std() float64 { return math.Sqrt(w.variance()) }
