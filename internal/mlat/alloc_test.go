package mlat

import (
	"testing"
	"time"

	"airview/internal/sim/simnet"
)

// TestHotPathAllocs measures the engine's per-group cost and allocations
// on real traffic: after 3 simulated minutes (clocks synced, tracks
// established) two seconds of ingest groups are captured and then
// processed in order (decode, reference store, planner, clock sync,
// assembler, solver, tracker, publication). Steady state allocates
// nothing per group; the remaining allocations come from new aircraft
// and stations entering the per-aircraft maps.
func TestHotPathAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet warm-up")
	}
	h := newHarness(t, harnessCfg{seed: 11, devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 8, simnet.DeviceSDR: 8},
		radiusKm: 150, modeSFrac: 0.3, holdout: 0.1, evalFromS: 1e9})
	h.run(3 * time.Minute)
	h.buffer = true
	h.run(2 * time.Second)
	h.buffer = false
	gs := h.buffered
	if len(gs) < 1000 {
		t.Fatalf("captured %d groups", len(gs))
	}
	st0 := h.e.Stats()
	start := time.Now()
	allocs := testing.AllocsPerRun(1, func() {
		for _, g := range gs {
			h.e.Observe(g)
		}
	})
	el := time.Since(start)
	st1 := h.e.Stats()
	per := allocs / float64(len(gs))
	t.Logf("%d groups (%d clusters, %d solves): %.2f allocs/group, %.1f µs/group (incl. AllocsPerRun warm-up pass)",
		len(gs), st1.Clusters-st0.Clusters, st1.Solves-st0.Solves, per, float64(el.Microseconds())/float64(2*len(gs)))
	if per > 0.05 {
		t.Errorf("hot path allocates %.3f per group", per)
	}
}
