package solve

import (
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
)

// benchProblems returns a set of distinct problems of one shape so the
// benchmark does not measure a single lucky geometry.
func benchProblems(n int, alt bool, fault bool, sigmaNs float64) []Problem {
	rng := rand.New(rand.NewPCG(uint64(n), 99))
	out := make([]Problem, 64)
	for k := range out {
		st := ring(rng, n, 120)
		x := visTarget(st, func() geo.Vec3 {
			return target(rng.Float64()*160-80, rng.Float64()*160-80, 3000+8000*rng.Float64())
		})
		pr := synth(rng, st, x, fill(n, sigmaNs*1e-9), nil, true)
		if alt {
			pr.Alt = Altitude{Valid: true, HAE: geo.FromECEF(x).H + 20*rng.NormFloat64(), Sigma: 20}
		}
		if fault {
			pr.Stations[rng.IntN(n)].TOA += 2e-6
		}
		out[k] = pr
	}
	return out
}

func benchSolve(b *testing.B, n int, alt, fault bool, sigmaNs float64) {
	probs := benchProblems(n, alt, fault, sigmaNs)
	s := NewSolver(Options{})
	var f Fix
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Solve(&probs[i%len(probs)], &f)
	}
}

// §13.2 gate: ≤ 20 µs per 6-station cluster.
func BenchmarkSolve6(b *testing.B)          { benchSolve(b, 6, false, false, 50) }
func BenchmarkSolve6Alt(b *testing.B)       { benchSolve(b, 6, true, false, 50) }
func BenchmarkSolve6SDRAlt(b *testing.B)    { benchSolve(b, 6, true, false, 400) }
func BenchmarkSolve4(b *testing.B)          { benchSolve(b, 4, false, false, 50) }
func BenchmarkSolve4Alt(b *testing.B)       { benchSolve(b, 4, true, false, 50) }
func BenchmarkSolve10Alt(b *testing.B)      { benchSolve(b, 10, true, false, 50) }
func BenchmarkSolve20Alt(b *testing.B)      { benchSolve(b, 20, true, false, 300) }
func BenchmarkSolve3Alt(b *testing.B)       { benchSolve(b, 3, true, false, 50) }
func BenchmarkSolve6AltFault(b *testing.B)  { benchSolve(b, 6, true, true, 50) }
func BenchmarkSolve10AltFault(b *testing.B) { benchSolve(b, 10, true, true, 50) }
