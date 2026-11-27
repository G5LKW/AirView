package quality

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
)

// Benchmarks of the phase-4 per-fix work against the budgets of a
// 1,000-feeder / 10,000-aircraft aggregator (WIRING.md "Budgets"):
//
//	KAtm.AddSolved, 8 rows        <= 10 µs  (one fix in 4 sampled: ~2.5 µs per fix)
//	CompareMLAT                    <= 1 µs   (per ADS-B position with a fix)
//	Verify, 8 stations             <= 25 µs  (per aircraft every VerifyEvery = 5 s)
//	LatencyCal.Learn / Score       <= 0.3 µs
//	LatencyCal.ForReference        <= 0.1 µs
//
// BenchmarkCRLBMap (crlb_simnet_test.go) covers the admin map.

func benchGeometry(m int) (tgt geo.Vec3, sts []geo.Vec3) {
	tgt = geo.ToECEF(50.2, 9.1, 9000)
	for i := 0; i < m; i++ {
		a := float64(i) / float64(m) * 2 * math.Pi
		sts = append(sts, geo.ToECEF(50+1.3*math.Cos(a), 9+2*math.Sin(a), 150+float64(i)*20))
	}
	return
}

func BenchmarkKAtmAddSolved(b *testing.B) {
	atm := geo.DefaultAtmosphere()
	tgt, sts := benchGeometry(8)
	rs := make([]float64, 8)
	sg := make([]float64, 8)
	rng := rand.New(rand.NewPCG(1, 1))
	for i := range rs {
		rs[i] = rng.NormFloat64() * 5
		sg[i] = 5
	}
	k := &KAtm{}
	f := &KAtmFix{Target: tgt, Stations: sts, ResidM: rs, SigmaM: sg, AltSigmaM: 30, ModelK: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// stay inside one hour; the row store is bounded (maxKAtmRows)
		k.AddSolved(int64(i%1000)*1e6, f, atm)
		if i%20000 == 19999 {
			k.mu.Lock()
			k.prow = k.prow[:0]
			k.mu.Unlock()
		}
	}
}

func BenchmarkCompareMLAT(b *testing.B) {
	rep := geo.ToECEF(50, 9, 10000)
	fr := geo.NewFrameECEF(rep)
	cov := RefCovECEF(rep, fr.Unrotate(geo.Vec3{X: 200}), 9, 0.15, 30)
	ml := fr.FromENU(geo.Vec3{X: 30, Y: -20})
	cm := fr.CovToECEF([9]float64{400, 0, 0, 0, 400, 0, 0, 0, 900})
	p := DefaultCompareParams()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CompareMLAT(rep, cov, ml, cm, p)
	}
}

func BenchmarkVerify8(b *testing.B) {
	atm := geo.DefaultAtmosphere()
	rangeFn := func(rx, ac geo.Vec3) float64 { return atm.Range(rx, ac) }
	tgt, sts := benchGeometry(8)
	toas := make([]TOA, 8)
	sig := make([]float64, 8)
	for i, p := range sts {
		toas[i] = TOA{Pos: p, T: 0.1 + atm.Range(p, tgt)/geo.C}
		sig[i] = 30e-9
	}
	cov := RefCovECEF(tgt, geo.Vec3{}, 9, 0.15, 30)
	vp := DefaultVerifyParams()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Verify(tgt, cov, toas, nil, sig, rangeFn, vp)
	}
}

func BenchmarkLatencyCal(b *testing.B) {
	c := NewLatencyCal(LatencyCalParams{})
	for k := uint32(0); k < 10000; k++ {
		for i := 0; i < 25; i++ {
			c.Learn(LatencySample{Key: k, NowNs: 1, AlongM: 20, SigmaAlongM: 5, SpeedMps: 200, NACp: 9})
		}
	}
	b.Run("Learn", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.Learn(LatencySample{Key: uint32(i % 10000), NowNs: 2, AlongM: 20, SigmaAlongM: 5, SpeedMps: 200, NACp: 9})
		}
	})
	b.Run("Score", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.Score(LatencySample{Key: uint32(i % 10000), NowNs: 2, AlongM: 20, SigmaAlongM: 5, SpeedMps: 200, NACp: 9})
		}
	})
	b.Run("ForReference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.ForReference(uint32(i%10000), i&1 == 0)
		}
	})
	b.Run("Decide", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			c.Decide(3)
		}
	})
}
