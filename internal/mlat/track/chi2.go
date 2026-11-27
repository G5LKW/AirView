package track

import "math"

// Chi-square quantiles for the Mahalanobis gate. P(k/2, x/2) is the
// regularized lower incomplete gamma function; the quantile is found by
// bisection on it. Both are implemented from the textbook series and
// continued-fraction expansions.

// gammaP returns the regularized lower incomplete gamma P(a, x).
func gammaP(a, x float64) float64 {
	if x <= 0 {
		return 0
	}
	lg, _ := math.Lgamma(a)
	if x < a+1 {
		// Series: P = e^(−x) x^a / Γ(a+1) · Σ x^n / ((a+1)…(a+n)).
		sum, term := 1/a, 1/a
		for n := 1; n < 1000; n++ {
			term *= x / (a + float64(n))
			sum += term
			if math.Abs(term) < math.Abs(sum)*1e-16 {
				break
			}
		}
		return sum * math.Exp(-x+a*math.Log(x)-lg)
	}
	// Continued fraction for Q = 1 − P (modified Lentz).
	const tiny = 1e-300
	b := x + 1 - a
	c := 1 / tiny
	d := 1 / b
	h := d
	for i := 1; i < 1000; i++ {
		an := -float64(i) * (float64(i) - a)
		b += 2
		d = an*d + b
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = b + an/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < 1e-16 {
			break
		}
	}
	return 1 - math.Exp(-x+a*math.Log(x)-lg)*h
}

// Chi2CDF returns the chi-square CDF with k degrees of freedom at x.
func Chi2CDF(x float64, k int) float64 { return gammaP(float64(k)/2, x/2) }

// Chi2Quantile returns x with Chi2CDF(x, k) = p (0 < p < 1).
func Chi2Quantile(p float64, k int) float64 {
	if k <= 0 || !(p > 0 && p < 1) {
		return math.NaN()
	}
	lo, hi := 0.0, float64(k)+10*math.Sqrt(2*float64(k))+50
	for Chi2CDF(hi, k) < p {
		hi *= 2
	}
	for range 200 {
		mid := (lo + hi) / 2
		if Chi2CDF(mid, k) < p {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo < 1e-12*hi {
			break
		}
	}
	return (lo + hi) / 2
}
