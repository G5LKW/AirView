package rfstat

import (
	"math"
	"strconv"
)

// Num is a float64 that marshals NaN and +-Inf as JSON null, so result types
// can carry "unknown" without ever producing invalid JSON. It unmarshals null
// back to NaN.
type Num float64

// MarshalJSON implements json.Marshaler.
func (n Num) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	// Round to 4 significant decimals after the point to keep payloads small.
	return strconv.AppendFloat(nil, round4(f), 'f', -1, 64), nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (n *Num) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*n = Num(math.NaN())
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return err
	}
	*n = Num(f)
	return nil
}

// Valid reports whether n is a finite number.
func (n Num) Valid() bool { return !math.IsNaN(float64(n)) && !math.IsInf(float64(n), 0) }

func round4(f float64) float64 {
	if math.Abs(f) >= 1e12 {
		return f
	}
	return math.Round(f*1e4) / 1e4
}

var nan = math.NaN()

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// ratio returns a/b or NaN when b <= 0.
func ratio(a, b float64) float64 {
	if b <= 0 {
		return nan
	}
	return a / b
}

// histQuantile returns the p-quantile (0..1) of counts h whose bin i covers
// [lo+i*w, lo+(i+1)*w), linearly interpolated inside the bin. NaN if empty.
func histQuantile(h []float64, lo, w, p float64) float64 {
	var tot float64
	for _, c := range h {
		tot += c
	}
	if tot <= 0 {
		return nan
	}
	target := p * tot
	var cum float64
	for i, c := range h {
		if c <= 0 {
			continue
		}
		if cum+c >= target {
			frac := (target - cum) / c
			if frac < 0 {
				frac = 0
			}
			return lo + (float64(i)+frac)*w
		}
		cum += c
	}
	return lo + float64(len(h))*w
}

// histMean returns the mean of a binned histogram using bin centres.
func histMean(h []float64, lo, w float64) float64 {
	var s, n float64
	for i, c := range h {
		s += c * (lo + (float64(i)+0.5)*w)
		n += c
	}
	return ratio(s, n)
}

func sum(h []float64) float64 {
	var s float64
	for _, c := range h {
		s += c
	}
	return s
}

// nums converts a float slice into a Num slice.
func nums(f []float64) []Num {
	out := make([]Num, len(f))
	for i, v := range f {
		out[i] = Num(v)
	}
	return out
}
