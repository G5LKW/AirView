package quality

import "math"

// Hist is a log-spaced histogram of positive values (metres, ratios). Bin 0
// holds everything up to HistMin; bin k >= 1 holds (HistMin·r^(k−1),
// HistMin·r^k] with r = HistRatio, so a quantile read from it is within
// ±2.5 % of the exact sample quantile. The zero value is ready to use; a
// Hist is not safe for concurrent use (callers lock).
type Hist struct {
	C [HistBins]uint32
	N uint64
}

// Histogram geometry: 0.5 m · 1.05^255 ≈ 125 km covers every error the
// engine accepts (σ_h ≤ 2 km, gross errors are clipped into the top bin).
const (
	HistBins  = 256
	HistMin   = 0.5
	HistRatio = 1.05
)

var lnRatio = math.Log(HistRatio)

func histBin(v float64) int {
	if !(v > HistMin) {
		return 0
	}
	b := int(math.Log(v/HistMin)/lnRatio) + 1
	if b >= HistBins {
		b = HistBins - 1
	}
	return b
}

// histValue is the representative value of bin b (geometric centre).
func histValue(b int) float64 {
	if b == 0 {
		return HistMin / 2
	}
	return HistMin * math.Pow(HistRatio, float64(b)-0.5)
}

// Add counts one value.
func (h *Hist) Add(v float64) {
	if math.IsNaN(v) {
		return
	}
	h.C[histBin(v)]++
	h.N++
}

// Merge adds o into h.
func (h *Hist) Merge(o *Hist) {
	if o.N == 0 {
		return
	}
	for i := range h.C {
		h.C[i] += o.C[i]
	}
	h.N += o.N
}

// Reset empties h.
func (h *Hist) Reset() { *h = Hist{} }

// Quantile returns the q-quantile (0 < q < 1), NaN when empty.
func (h *Hist) Quantile(q float64) float64 {
	if h.N == 0 {
		return math.NaN()
	}
	target := q * float64(h.N)
	cum := 0.0
	for b, c := range h.C {
		cum += float64(c)
		if cum >= target && c > 0 {
			return histValue(b)
		}
	}
	return histValue(HistBins - 1)
}

// FracBelow returns the fraction of values <= v (bin resolution).
func (h *Hist) FracBelow(v float64) float64 {
	if h.N == 0 {
		return math.NaN()
	}
	b := histBin(v)
	var s uint64
	for i := 0; i <= b; i++ {
		s += uint64(h.C[i])
	}
	return float64(s) / float64(h.N)
}
