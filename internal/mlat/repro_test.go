package mlat

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"os"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// runDigest runs the synchronous end-to-end harness and returns a digest of
// every published fix (in publication order, all fields that carry results)
// plus the counters.
var orderFree uint64

func runDigest(t *testing.T, seed uint64, d time.Duration) (uint64, int, Stats) {
	orderFree = 0
	h := newHarness(t, harnessCfg{seed: seed, devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 3, simnet.DeviceSDR: 5},
		radiusKm: 120, aircraft: 80, modeSFrac: 0.3, holdout: 0.1, warmup: 20 * time.Second})
	hs := fnv.New64a()
	n := 0
	var b [8]byte
	put := func(v uint64) { binary.LittleEndian.PutUint64(b[:], v); hs.Write(b[:]) }
	h.pubHook = func(f *model.MLATFix, _ simnet.AircraftTruth, _ bool) {
		n++
		put(uint64(f.ICAO))
		put(uint64(f.TUnixNs))
		for _, v := range f.ECEF {
			put(math.Float64bits(v))
		}
		for _, v := range f.Cov {
			put(uint64(math.Float32bits(v)))
		}
		put(uint64(f.Grade)<<8 | uint64(f.Flags))
		g := fnv.New64a()
		var bb [8]byte
		for _, v := range []uint64{uint64(f.ICAO), uint64(f.TUnixNs), math.Float64bits(f.ECEF[0]), math.Float64bits(f.ECEF[1]), math.Float64bits(f.ECEF[2])} {
			binary.LittleEndian.PutUint64(bb[:], v)
			g.Write(bb[:])
		}
		orderFree += g.Sum64()
	}
	h.run(d)
	st := h.e.Stats()
	st.ProcUsPerGroup, st.Clock.LastEpochNs, st.Clock.MeanEpochNs = 0, 0, 0
	st.Plan.Took = 0
	return hs.Sum64(), n, st
}

// TestReproducible: two runs of the same simulated network (same seed) must
// publish bit-identical results in the same order (H-track item 6: no map
// iteration order, wall clock or goroutine schedule may reach the results).
// Synchronous engine (Workers < 0), as every simulation harness uses it.
func TestReproducible(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	d1, n1, s1 := runDigest(t, 31, reproDur())
	o1 := orderFree
	d2, n2, s2 := runDigest(t, 31, reproDur())
	t.Logf("order-free digests %x %x", o1, orderFree)
	t.Logf("run 1: %d fixes, digest %016x; run 2: %d fixes, digest %016x", n1, d1, n2, d2)
	if n1 == 0 {
		t.Fatal("no fixes published")
	}
	if d1 != d2 || n1 != n2 {
		t.Errorf("runs differ: %d vs %d fixes, digest %016x vs %016x", n1, n2, d1, d2)
	}
	if s1.Solves != s2.Solves || s1.SolveOK != s2.SolveOK || s1.Published != s2.Published || s1.Clusters != s2.Clusters {
		t.Errorf("counters differ:\n%+v\n%+v", s1, s2)
	}
}

func reproDur() time.Duration {
	if v, err := time.ParseDuration(os.Getenv("MLAT_REPRO_DUR")); err == nil && v > 0 {
		return v
	}
	return 150 * time.Second
}
