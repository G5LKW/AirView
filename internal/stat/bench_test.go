package stat

import (
	"math"
	"testing"
)

func benchSeries(n int, missing float64) []float64 {
	r := newRand(99)
	x := make([]float64, n)
	for i := range x {
		x[i] = seasonSignal(i, 24) + r.NormFloat64()
		if r.Float64() < missing {
			x[i] = math.NaN()
		}
	}
	return x
}

var sink float64

func BenchmarkMeanStd(b *testing.B) {
	x := benchSeries(8760, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = Mean(x) + Std(x)
	}
}

func BenchmarkQuantile(b *testing.B) {
	x := benchSeries(8760, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = Quantile(x, 0.95)
	}
}

func BenchmarkHistQuantile(b *testing.B) {
	counts := make([]uint32, 120)
	r := newRand(1)
	for i := range counts {
		counts[i] = uint32(r.IntN(500))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = HistQuantile(counts, 0, 5, 0.95)
	}
}

func BenchmarkRollingMean(b *testing.B) {
	x := benchSeries(8760, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = RollingMean(x, 24)[100]
	}
}

func BenchmarkRollingMedian(b *testing.B) {
	x := benchSeries(8760, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = RollingMedian(x, 24)[100]
	}
}

func BenchmarkAccumulator(b *testing.B) {
	x := benchSeries(8760, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var a Accumulator
		a.AddAll(x)
		sink = a.Std()
	}
}

func BenchmarkCircularMean(b *testing.B) {
	r := newRand(2)
	d := make([]float64, 8760)
	for i := range d {
		d[i] = 360 * r.Float64()
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m, _, _ := CircularMeanDeg(d)
		sink = m
	}
}

func BenchmarkRegIncBeta(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = RegIncBeta(40, 0.5, 0.93)
	}
}

func BenchmarkPearson(b *testing.B) {
	x, y := benchSeries(8760, 0.05), benchSeries(8760, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c, _ := Pearson(x, y)
		sink = c.R
	}
}

func BenchmarkSpearman(b *testing.B) {
	x, y := benchSeries(8760, 0.05), benchSeries(8760, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c, _ := Spearman(x, y)
		sink = c.R
	}
}

func BenchmarkCrossCorr48(b *testing.B) {
	x, y := benchSeries(8760, 0.05), benchSeries(8760, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c, _ := BestLag(x, y, 48, 100)
		sink = c.R
	}
}

func BenchmarkTheilSen500(b *testing.B) {
	x, y := benchSeries(500, 0), benchSeries(500, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l, _ := TheilSen(x, y)
		sink = l.Slope
	}
}

func BenchmarkOLS(b *testing.B) {
	X, y := planted(5, 2000, 1, []float64{1, 2, 3, 4, 5, 6, 7, 8}, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r, _ := OLS(X, y, true)
		sink = r.R2
	}
}

func BenchmarkRidge(b *testing.B) {
	X, y := planted(5, 2000, 1, []float64{1, 2, 3, 4, 5, 6, 7, 8}, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m, _ := Ridge(X, y, nil, 0.1)
		sink = m.R2
	}
}

func BenchmarkRidgeCV(b *testing.B) {
	X, y := planted(5, 2000, 1, []float64{1, 2, 3, 4, 5, 6, 7, 8}, 1)
	lambdas := LogSpace(1e-3, 10, 9)
	folds := BlockFold(len(y), 5)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, cv, _ := RidgeCV(X, y, nil, lambdas, folds)
		sink = cv.Best
	}
}

func BenchmarkModelPredict(b *testing.B) {
	X, y := planted(5, 500, 1, []float64{1, 2, 3, 4, 5, 6, 7, 8}, 1)
	m, _ := Ridge(X, y, nil, 0.1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = m.Predict(X[i%len(X)])
	}
}

func BenchmarkLogistic(b *testing.B) {
	r := newRand(3)
	var X [][]float64
	var y []float64
	for i := 0; i < 2000; i++ {
		row := []float64{r.NormFloat64(), r.NormFloat64(), r.NormFloat64(), r.NormFloat64()}
		v := 0.0
		if r.Float64() < Sigmoid(row[0]-2*row[1]+0.5) {
			v = 1
		}
		X, y = append(X, row), append(y, v)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m, _ := Logistic(X, y, nil, &LogisticOptions{Lambda: 1e-3})
		sink = m.LogLoss
	}
}

func BenchmarkNelderMeadRosenbrock(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r, _ := NelderMead(rosenbrock, []float64{-1.2, 1}, nil)
		sink = r.F
	}
}

func BenchmarkSeasonalProfile168(b *testing.B) {
	x := benchSeries(8760, 0.05)
	opt := &ProfileOptions{HalfLife: 24 * 60}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p, _ := FitSeasonalProfile(x, 0, 168, opt)
		sink = p.Level[0]
	}
}

func BenchmarkHoltWintersFit24(b *testing.B) {
	x := benchSeries(24*60, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f, _ := FitHoltWinters(x, 24, nil)
		sink = f.SSE
	}
}

func BenchmarkHoltWintersFit168(b *testing.B) {
	x := benchSeries(168*12, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f, _ := FitHoltWinters(x, 168, nil)
		sink = f.SSE
	}
}

func BenchmarkHoltWintersUpdate(b *testing.B) {
	x := benchSeries(24*60, 0.05)
	f, _ := FitHoltWinters(x, 24, &HWOptions{Params: &HWParams{Alpha: 0.2, Beta: 0.05, Gamma: 0.2}})
	m := f.Model
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p, _ := m.Update(x[i%len(x)])
		sink = p
	}
}

func BenchmarkARYuleWalker(b *testing.B) {
	x := benchSeries(8760, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m, _ := FitARYuleWalker(x, 6)
		sink = m.Sigma2
	}
}

func BenchmarkKalmanSmoothTrend(b *testing.B) {
	x := benchSeries(8760, 0.05)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l, _ := KalmanSmoothTrend(x, 1e-3, 1e-4, 1)
		sink = l[100]
	}
}

func BenchmarkLocalTrendStep(b *testing.B) {
	x := benchSeries(8760, 0.05)
	k := NewLocalTrend(1e-3, 1e-4, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l, _ := k.Update(x[i%len(x)])
		sink = l
	}
}
