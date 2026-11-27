package stat

import "math"

// RegIncBeta returns the regularised incomplete beta function I_x(a, b) for
// a, b > 0 and x in [0, 1]. Out-of-domain arguments yield NaN.
func RegIncBeta(a, b, x float64) float64 {
	if math.IsNaN(a) || math.IsNaN(b) || math.IsNaN(x) || a <= 0 || b <= 0 || x < 0 || x > 1 {
		return math.NaN()
	}
	if x == 0 {
		return 0
	}
	if x == 1 {
		return 1
	}
	lga, _ := math.Lgamma(a)
	lgb, _ := math.Lgamma(b)
	lgab, _ := math.Lgamma(a + b)
	bt := math.Exp(lgab - lga - lgb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return clamp(bt*betaCF(a, b, x)/a, 0, 1)
	}
	return clamp(1-bt*betaCF(b, a, 1-x)/b, 0, 1)
}

// betaCF evaluates the continued fraction of the incomplete beta function by
// the modified Lentz method.
func betaCF(a, b, x float64) float64 {
	const (
		maxIter = 20000
		eps     = 1e-16
		tiny    = 1e-300
	)
	qab, qap, qam := a+b, a+1, a-1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// StudentTCDF returns P(T <= t) for Student's t distribution with df > 0
// degrees of freedom (df need not be an integer).
func StudentTCDF(t, df float64) float64 {
	if math.IsNaN(t) || math.IsNaN(df) || df <= 0 {
		return math.NaN()
	}
	if math.IsInf(t, 1) {
		return 1
	}
	if math.IsInf(t, -1) {
		return 0
	}
	half := 0.5 * RegIncBeta(df/2, 0.5, df/(df+t*t))
	if t > 0 {
		return 1 - half
	}
	return half
}

// StudentTTwoSidedP returns the two-sided p-value P(|T| >= |t|).
func StudentTTwoSidedP(t, df float64) float64 {
	if math.IsNaN(t) || math.IsNaN(df) || df <= 0 {
		return math.NaN()
	}
	if math.IsInf(t, 0) {
		return 0
	}
	return RegIncBeta(df/2, 0.5, df/(df+t*t))
}

// StudentTQuantile returns the t value with P(T <= t) = p for df > 0.
func StudentTQuantile(p, df float64) float64 {
	if math.IsNaN(p) || math.IsNaN(df) || df <= 0 || p < 0 || p > 1 {
		return math.NaN()
	}
	switch {
	case p == 0:
		return math.Inf(-1)
	case p == 1:
		return math.Inf(1)
	case p == 0.5:
		return 0
	case p < 0.5:
		return -StudentTQuantile(1-p, df)
	}
	// Upper tail probability q = 1-p = 0.5*I_{df/(df+t^2)}(df/2, 1/2);
	// bracket and bisect on t (monotone).
	lo, hi := 0.0, 1.0
	for StudentTCDF(hi, df) < p && hi < 1e300 {
		lo = hi
		hi *= 2
	}
	for i := 0; i < 200; i++ {
		mid := 0.5 * (lo + hi)
		if StudentTCDF(mid, df) < p {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= 1e-15*hi {
			break
		}
	}
	return 0.5 * (lo + hi)
}

// FDistSF returns the upper tail probability P(F >= f) of the F distribution
// with (d1, d2) degrees of freedom.
func FDistSF(f, d1, d2 float64) float64 {
	if math.IsNaN(f) || d1 <= 0 || d2 <= 0 || math.IsNaN(d1) || math.IsNaN(d2) {
		return math.NaN()
	}
	if f <= 0 {
		return 1
	}
	if math.IsInf(f, 1) {
		return 0
	}
	return RegIncBeta(d2/2, d1/2, d2/(d2+d1*f))
}

// NormalCDF returns the standard normal distribution function.
func NormalCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// NormalQuantile returns the standard normal quantile for p in [0, 1].
func NormalQuantile(p float64) float64 {
	if math.IsNaN(p) || p < 0 || p > 1 {
		return math.NaN()
	}
	return -math.Sqrt2 * math.Erfcinv(2*p)
}

// Sigmoid returns the logistic function 1/(1+exp(-x)) without overflow.
func Sigmoid(x float64) float64 {
	if x >= 0 {
		return 1 / (1 + math.Exp(-x))
	}
	e := math.Exp(x)
	return e / (1 + e)
}

// Logit returns log(p/(1-p)).
func Logit(p float64) float64 { return math.Log(p) - math.Log1p(-p) }

// intervalZ converts a two-sided coverage level (e.g. 0.9) to a normal
// quantile. Invalid levels fall back to 0.95.
func intervalZ(level float64) float64 {
	return NormalQuantile(0.5 + fixLevel(level)/2)
}

func fixLevel(level float64) float64 {
	if !(level > 0 && level < 1) {
		return 0.95
	}
	return level
}
