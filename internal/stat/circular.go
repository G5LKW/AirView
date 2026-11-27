package stat

import "math"

const (
	deg2rad = math.Pi / 180
	rad2deg = 180 / math.Pi
)

// NormalizeDeg maps an angle to [0, 360). NaN and infinities yield NaN.
func NormalizeDeg(a float64) float64 {
	if !IsFinite(a) {
		return math.NaN()
	}
	a = math.Mod(a, 360)
	if a < 0 {
		a += 360
	}
	if a >= 360 { // -1e-20 + 360 rounds to 360
		a = 0
	}
	return a + 0 // turns the -0 of e.g. NormalizeDeg(-360) into +0
}

// AngleDiffDeg returns the signed smallest rotation from b to a, i.e. a-b
// wrapped into (-180, 180]. Positive means a is clockwise of b.
func AngleDiffDeg(a, b float64) float64 {
	if !IsFinite(a) || !IsFinite(b) {
		return math.NaN()
	}
	d := math.Mod(a-b, 360)
	if d > 180 {
		d -= 360
	} else if d <= -180 {
		d += 360
	}
	return d
}

// AbsAngleDiffDeg returns the unsigned separation of two directions, [0, 180].
func AbsAngleDiffDeg(a, b float64) float64 { return math.Abs(AngleDiffDeg(a, b)) }

// CircularStats summarises a sample of directions.
type CircularStats struct {
	N        int     `json:"n"`
	MeanDeg  float64 `json:"meanDeg"`  // mean direction [0, 360); 0 when R is ~0 (undefined)
	R        float64 `json:"r"`        // mean resultant length, 0 (uniform) .. 1 (identical)
	Variance float64 `json:"variance"` // circular variance 1-R
	StdDeg   float64 `json:"stdDeg"`   // circular std sqrt(-2 ln R) in degrees; capped at 180*sqrt(2) when R is ~0
	Defined  bool    `json:"defined"`  // false when the mean direction is undefined (R < 1e-9)
}

// CircularSummary computes circular statistics of directions in degrees.
// w may be nil (equal weights) or a slice of non-negative weights of the
// same length. Entries with a non-finite angle or a non-positive / non-finite
// weight are skipped. ok is false when nothing remains or the lengths differ.
func CircularSummary(deg, w []float64) (CircularStats, bool) {
	if w != nil && len(w) != len(deg) {
		return CircularStats{}, false
	}
	var ss, sc, sw float64
	n := 0
	for i, a := range deg {
		if !IsFinite(a) {
			continue
		}
		wi := 1.0
		if w != nil {
			wi = w[i]
			if !(wi > 0) || math.IsInf(wi, 0) {
				continue
			}
		}
		s, c := math.Sincos(a * deg2rad)
		ss += wi * s
		sc += wi * c
		sw += wi
		n++
	}
	if n == 0 || !(sw > 0) {
		return CircularStats{}, false
	}
	r := math.Hypot(ss, sc) / sw
	if r > 1 {
		r = 1
	}
	st := CircularStats{N: n, R: r, Variance: 1 - r}
	if r < 1e-9 {
		st.StdDeg = 180 * math.Sqrt2
		return st, true
	}
	st.Defined = true
	st.MeanDeg = NormalizeDeg(math.Atan2(ss, sc) * rad2deg)
	st.StdDeg = math.Min(math.Sqrt(-2*math.Log(r))*rad2deg, 180*math.Sqrt2)
	return st, true
}

// CircularMeanDeg returns the mean direction of the finite angles in deg and
// the mean resultant length r. ok is false when there is no data or the
// directions cancel out (r ~ 0), in which case mean is NaN.
func CircularMeanDeg(deg []float64) (mean, r float64, ok bool) {
	st, valid := CircularSummary(deg, nil)
	if !valid || !st.Defined {
		return math.NaN(), st.R, false
	}
	return st.MeanDeg, st.R, true
}

// ResultantLength returns the mean resultant length R of the directions,
// NaN if there is no data.
func ResultantLength(deg []float64) float64 {
	st, ok := CircularSummary(deg, nil)
	if !ok {
		return math.NaN()
	}
	return st.R
}

// CircularVariance returns 1-R, NaN if there is no data.
func CircularVariance(deg []float64) float64 {
	st, ok := CircularSummary(deg, nil)
	if !ok {
		return math.NaN()
	}
	return st.Variance
}

// CircularStdDeg returns the circular standard deviation in degrees, NaN if
// there is no data.
func CircularStdDeg(deg []float64) float64 {
	st, ok := CircularSummary(deg, nil)
	if !ok {
		return math.NaN()
	}
	return st.StdDeg
}

// WindToUV converts speed and meteorological direction (degrees the wind
// blows FROM) into eastward (u) and northward (v) components.
func WindToUV(speed, dirDeg float64) (u, v float64) {
	s, c := math.Sincos(dirDeg * deg2rad)
	return -speed * s, -speed * c
}

// UVToWind converts components back to speed and "from" direction in
// [0, 360). Calm (u = v = 0) reports direction 0.
func UVToWind(u, v float64) (speed, dirDeg float64) {
	speed = math.Hypot(u, v)
	if speed == 0 || speed != speed {
		return speed, 0
	}
	return speed, NormalizeDeg(math.Atan2(-u, -v) * rad2deg)
}

// WindMean is the result of vector-averaging wind observations.
type WindMean struct {
	N           int     `json:"n"`
	Speed       float64 `json:"speed"`       // magnitude of the mean vector
	DirDeg      float64 `json:"dirDeg"`      // direction of the mean vector ("from")
	U           float64 `json:"u"`           // mean eastward component
	V           float64 `json:"v"`           // mean northward component
	ScalarSpeed float64 `json:"scalarSpeed"` // arithmetic mean of the speeds
	Steadiness  float64 `json:"steadiness"`  // Speed/ScalarSpeed in [0, 1]; 0 when calm
}

// VectorMeanWind averages wind observations as vectors. Pairs with a
// non-finite speed or direction are skipped; ok is false when none remain or
// the lengths differ.
func VectorMeanWind(speed, dirDeg []float64) (WindMean, bool) {
	if len(speed) != len(dirDeg) {
		return WindMean{}, false
	}
	var su, sv, ssp float64
	n := 0
	for i, s := range speed {
		d := dirDeg[i]
		if !IsFinite(s) || !IsFinite(d) {
			continue
		}
		u, v := WindToUV(s, d)
		su += u
		sv += v
		ssp += s
		n++
	}
	if n == 0 {
		return WindMean{}, false
	}
	fn := float64(n)
	m := WindMean{N: n, U: su / fn, V: sv / fn, ScalarSpeed: ssp / fn}
	m.Speed, m.DirDeg = UVToWind(m.U, m.V)
	if m.ScalarSpeed > 0 {
		m.Steadiness = math.Min(m.Speed/m.ScalarSpeed, 1)
	}
	return m, true
}
