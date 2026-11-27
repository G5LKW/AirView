package mlat

import (
	"os"
	"strconv"
	"testing"
	"time"

	"airview/internal/sim/simnet"
)

// TestThroughput measures the engine's processing cost on simnet networks
// of 100 and 500 stations (DefaultMix device mix: 62 % RTL-SDR, 10 % GPS
// Radarcapes, ...), through the real ingest. The engine's own CPU time is
// the sum of the workers' processing time (Stats.ProcUsPerGroup × groups);
// it is reported per simulated second, i.e. in cores of the dev machine.
// Opt-in (MLAT_THROUGHPUT=1): the 500-station run simulates for minutes.
func TestThroughput(t *testing.T) {
	if os.Getenv("MLAT_THROUGHPUT") == "" {
		t.Skip("set MLAT_THROUGHPUT=1")
	}
	workers, _ := strconv.Atoi(os.Getenv("MLAT_WORKERS"))
	if workers == 0 {
		workers = -1
	}
	for _, tc := range []struct {
		rx, ac int
		radius float64
		dur    time.Duration
	}{{100, 600, 300, 3 * time.Minute}, {500, 1200, 500, 2 * time.Minute}} {
		if v := os.Getenv("MLAT_ONLY"); v != "" && v != strconv.Itoa(tc.rx) {
			continue
		}
		specs := simnet.RandomReceivers(31, tc.rx, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: tc.radius})
		start := time.Now()
		h := newHarnessSpecs(t, harnessCfg{seed: 31, radiusKm: tc.radius, aircraft: tc.ac, modeSFrac: 0.3, holdout: 0.1,
			evalFromS: 1e9, workers: workers}, specs)
		h.run(tc.dur)
		if !h.e.sync {
			if err := h.e.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		wall := time.Since(start)
		st := h.e.Stats()
		simS := tc.dur.Seconds()
		cpu := st.ProcUsPerGroup * float64(st.Groups+st.Late) / 1e6
		t.Logf("%d stations, %d aircraft, %v simulated, workers %d: groups %d (%.0f/s) clusters %d (%.0f/s) solves %d (%.0f/s) published %d; "+
			"engine %.1f µs/group, engine CPU %.3f cores (%.1f s for %.0f s); dropped %d; wall %v (simnet + ingest + engine)",
			tc.rx, tc.ac, tc.dur, len(h.e.workers), st.Groups, float64(st.Groups)/simS, st.Clusters, float64(st.Clusters)/simS,
			st.Solves, float64(st.Solves)/simS, st.Published, st.ProcUsPerGroup, cpu/simS, cpu, simS, st.Dropped, wall.Round(time.Second))
		t.Logf("  clock: epochs %d, last epoch %.1f ms, stations %d, components %d; tracks %d confirmed %d",
			st.Clock.Epochs, float64(st.Clock.LastEpochNs)/1e6, st.Clock.Stations, st.Clock.Components, st.Track.Tracks, st.Track.Confirmed)
	}
}
