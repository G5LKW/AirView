package solve

import "math"

// Chi-square distribution helpers for the RAIM test. Implemented from the
// textbook definitions: the χ²(k) upper-tail probability is the regularised
// upper incomplete gamma function Q(k/2, x/2), evaluated with its power
// series below a+1 and with a modified-Lentz continued fraction above.

// gammaQ returns the regularised upper incomplete gamma Q(a, x), a > 0.
func gammaQ(a, x float64) float64 {
	if x <= 0 {
		return 1
	}
	lg, _ := math.Lgamma(a)
	pre := math.Exp(-x + a*math.Log(x) - lg)
	if x < a+1 {
		// P(a,x) = e^-x x^a / Γ(a+1) · Σ x^n / ((a+1)…(a+n))
		sum, term, ap := 1/a, 1/a, a
		for n := 0; n < 1000; n++ {
			ap++
			term *= x / ap
			sum += term
			if math.Abs(term) < math.Abs(sum)*1e-16 {
				break
			}
		}
		return math.Max(0, 1-sum*pre)
	}
	// Q(a,x) = e^-x x^a / Γ(a) · 1/(x+1-a- 1·(1-a)/(x+3-a- 2·(2-a)/(x+5-a- …)))
	const tiny = 1e-300
	b := x + 1 - a
	cc := 1 / tiny
	d := 1 / b
	h := d
	for i := 1; i < 1000; i++ {
		an := -float64(i) * (float64(i) - a)
		b += 2
		d = an*d + b
		if math.Abs(d) < tiny {
			d = tiny
		}
		cc = b + an/cc
		if math.Abs(cc) < tiny {
			cc = tiny
		}
		d = 1 / d
		del := d * cc
		h *= del
		if math.Abs(del-1) < 1e-16 {
			break
		}
	}
	return math.Min(1, pre*h)
}

// Chi2Survival returns P(X > x) for X ~ χ²(dof).
func Chi2Survival(x float64, dof int) float64 {
	if dof <= 0 {
		return 1
	}
	return gammaQ(float64(dof)/2, x/2)
}

// Chi2Quantile returns the x with P(X > x) = p for X ~ χ²(dof) (the RAIM
// threshold for false-alarm probability p). It returns +Inf for dof <= 0.
func Chi2Quantile(p float64, dof int) float64 {
	if dof <= 0 || !(p > 0 && p < 1) {
		return math.Inf(1)
	}
	k := float64(dof)
	lo, hi := 0.0, k+10*math.Sqrt(2*k)+60
	for Chi2Survival(hi, dof) > p {
		hi *= 2
	}
	for i := 0; i < 200 && hi-lo > 1e-12*hi; i++ {
		mid := (lo + hi) / 2
		if Chi2Survival(mid, dof) > p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}
