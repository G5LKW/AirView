package clock

import "math"

// chi2Table999 holds the 0.999 quantiles of χ²(k), k = 1..10.
var chi2Table999 = [...]float64{10.828, 13.816, 16.266, 18.467, 20.515, 22.458, 24.322, 26.124, 27.877, 29.588}

// chi2Quantile returns the p-quantile of the χ² distribution with k degrees
// of freedom: exact table values for p = 0.999 and k ≤ 10, otherwise the
// Wilson–Hilferty cube-root normal approximation (relative error < 1 % for
// k ≥ 3 in the upper tail).
func chi2Quantile(p float64, k int) float64 {
	if k < 1 {
		return 0
	}
	if p == 0.999 && k <= len(chi2Table999) {
		return chi2Table999[k-1]
	}
	z := normQuantile(p)
	kf := float64(k)
	a := 2 / (9 * kf)
	v := 1 - a + z*math.Sqrt(a)
	return kf * v * v * v
}

// normQuantile returns the standard normal p-quantile (Acklam's rational
// approximation, relative error < 1.2e-9).
func normQuantile(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	a := [...]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [...]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := [...]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [...]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const lo = 0.02425
	switch {
	case p < lo:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > 1-lo:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	q := p - 0.5
	r := q * q
	return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q / (((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
}
