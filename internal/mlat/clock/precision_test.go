package clock

import (
	"math"
	"math/big"
	"testing"

	"airview/internal/model"
)

// Finding M6: network time is float64 seconds since the origin, so after
// months of uptime its ulp reaches nanoseconds and every TOA conversion
// picked that up. NetTimeUnix now converts in split form; its error against
// an exact (big.Float) evaluation stays far below a nanosecond at any
// uptime.
func TestNetTimeUnixPrecisionLongUptime(t *testing.T) {
	const origin = int64(1_780_000_000_000_000_000)
	for _, days := range []float64{0.01, 50, 200, 365, 3 * 365} {
		up := days * 86400
		sc := &StationClock{Station: 2, Class: model.ClockSDR12, Epoch: 7, Hz: 12e6, Synced: true,
			BaseTicks: 987_654_321, BaseS: up - 1234.0 + 0.123456789123, baseS: up - 1234.0 + 0.123456789123,
			Theta: 3_456_789.25, Phi: 48_123.5, t: up - 0.4, mode: modeFree, qScale: 1, prior: priorFor(model.ClockSDR12)}
		snap := &Snapshot{OriginUnixNs: origin, stations: []*StationClock{nil, nil, sc}, frameRef: DefaultFrameRefNs}
		worst := 0.0
		for i := 0; i < 50; i++ {
			ticks := sc.BaseTicks + uint64(1234*12e6) + uint64(i)*1_234_567
			ns, frac, _, _, ok := snap.NetTimeUnix(Query{Station: 2, Epoch: 7, Ticks: ticks, Kind: model.FrameModeSLong})
			if !ok {
				t.Fatal("not converted")
			}
			// exact: local = (ticks−B)/hz + BaseS; t + θ + φ·(t − t0) = local
			prec := uint(256)
			bf := func(x float64) *big.Float { return new(big.Float).SetPrec(prec).SetFloat64(x) }
			local := new(big.Float).SetPrec(prec).SetInt64(int64(ticks - sc.BaseTicks))
			local.Quo(local, bf(sc.Hz))
			local.Add(local, bf(sc.BaseS))
			// t·(1 + φ·1e-9) = local − θ·1e-9 + φ·1e-9·t0
			num := new(big.Float).SetPrec(prec).Sub(local, bf(sc.Theta*1e-9))
			pt := new(big.Float).SetPrec(prec).Mul(bf(sc.Phi), bf(1e-9))
			num.Add(num, new(big.Float).SetPrec(prec).Mul(pt, bf(sc.t)))
			den := new(big.Float).SetPrec(prec).Add(bf(1), pt)
			tt := new(big.Float).SetPrec(prec).Quo(num, den)
			tt.Mul(tt, bf(1e9)) // ns since the origin
			got := new(big.Float).SetPrec(prec).SetInt64(ns - origin)
			got.Add(got, bf(frac))
			d, _ := new(big.Float).Sub(got, tt).Float64()
			worst = math.Max(worst, math.Abs(d))
		}
		if worst > 0.01 {
			t.Errorf("uptime %.0f days: worst conversion error %.4f ns", days, worst)
		}
	}
}

// With the internal origin moved (Config.ReoriginS) the snapshot holds the
// base and the state time relative to OriginUnixNs + shiftS; the split
// conversion adds the whole seconds back exactly.
func TestNetTimeUnixShiftedOrigin(t *testing.T) {
	const origin = int64(1_780_000_000_000_000_000)
	up := 3 * 365 * 86400.0
	shift := int64(up) - 100
	mk := func(sh int64) *Snapshot {
		sc := &StationClock{Station: 2, Class: model.ClockSDR12, Epoch: 7, Hz: 12e6, Synced: true,
			BaseTicks: 987_654_321, BaseS: up - 1234.0 + 0.125, baseS: up - float64(sh) - 1234.0 + 0.125, shiftS: sh,
			Theta: 3_456_789.25, Phi: 48_123.5, t: up - float64(sh) - 0.5, mode: modeFree, qScale: 1, prior: priorFor(model.ClockSDR12)}
		return &Snapshot{OriginUnixNs: origin, shiftS: sh, stations: []*StationClock{nil, nil, sc}, frameRef: DefaultFrameRefNs}
	}
	a, b := mk(0), mk(shift)
	for i := 0; i < 20; i++ {
		q := Query{Station: 2, Epoch: 7, Ticks: 987_654_321 + uint64(1234*12e6) + uint64(i)*1_234_567, Kind: model.FrameModeSLong}
		na, fa, _, _, _ := a.NetTimeUnix(q)
		nb, fb, _, _, _ := b.NetTimeUnix(q)
		if d := math.Abs(float64(na-nb) + fa - fb); d > 0.01 {
			t.Fatalf("query %d: shifted conversion differs by %.4f ns", i, d)
		}
		ta, _, _ := a.NetTime(q)
		tb, _, _ := b.NetTime(q)
		if math.Abs(ta-tb) > 1e-6 {
			t.Fatalf("query %d: NetTime %v vs %v", i, ta, tb)
		}
	}
}
