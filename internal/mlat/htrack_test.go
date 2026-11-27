package mlat

import (
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"airview/internal/sim/simnet"
)

// netsimSpecs reproduces the receiver layout of `airview-netsim -seed 7
// -beast-feeders 3 -avf-feeders 12 -radius 150` (the live run of HOOKS.md
// "run-away hold-out tracks"): receiver 0 is the host's GPS Radarcape, the
// others keep simnet's community device mix.
func netsimSpecs(seed uint64, n int, radiusKm float64) []simnet.ReceiverSpec {
	specs := simnet.RandomReceivers(seed, n, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: radiusKm})
	rr := rand.New(rand.NewPCG(seed, 9))
	specs[0].Clock = simnet.DefaultClockSpec(simnet.DeviceRadarcapeGPS, rr)
	specs[0].Reception = simnet.DefaultReception(simnet.DeviceRadarcapeGPS, rr)
	return specs
}

// TestHoldoutTrackNetsim is the H-track reproduction of the live run-away
// hold-out tracks (opt-in: MLAT_HTRACK=1; MLAT_HTRACK_MIN sets the minutes).
func TestHoldoutTrackNetsim(t *testing.T) {
	if os.Getenv("MLAT_HTRACK") == "" {
		t.Skip("set MLAT_HTRACK=1")
	}
	mins := 12
	if v := os.Getenv("MLAT_HTRACK_MIN"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			mins = int(d.Minutes())
		}
	}
	hc := harnessCfg{seed: 7, radiusKm: 150, aircraft: 250, holdout: 0.1, evalFromS: float64(mins*60) - 240}
	if os.Getenv("MLAT_HTRACK_LIVE") != "" {
		// as the live run: no host station, the Beast compat links map-only,
		// no GPS status over AVF/1 (relative time throughout)
		hc.absent = map[int]bool{0: true}
		hc.untimed = map[int]bool{1: true, 2: true, 3: true}
		hc.noGPSStatus = true
	}
	if v, err := strconv.Atoi(os.Getenv("MLAT_HTRACK_WORKERS")); err == nil {
		hc.workers = v
	}
	h := newHarnessSpecs(t, hc, netsimSpecs(7, 16, 150))
	h.run(time.Duration(mins) * time.Minute)
	reportHoldout(t, h)
	// acceptance (H-track): hold-out track P95 within 2x the fix P95, against
	// the truth and in the engine's own live statistic
	hold, snap := summarize(h.hold, false), summarize(h.holdSnap, false)
	q := h.e.QualityReport(60, false)
	if hold.p95 > 2*snap.p95 || q.Track.P95 > 2*q.Fix.P95 {
		t.Errorf("hold-out track P95 %.0f m (live %.0f m) vs fix P95 %.0f m (live %.0f m): more than 2x", hold.p95, q.Track.P95, snap.p95, q.Fix.P95)
	}
}

func reportHoldout(t *testing.T, h *harness) {
	pub := summarize(h.pub, false)
	hold := summarize(h.hold, false)
	snap := summarize(h.holdSnap, false)
	t.Logf("published n %d P50 %.1f P95 %.1f P99 %.1f 1σ %.1f%% gross %d", pub.n, pub.p50, pub.p95, pub.p99, 100*pub.cal1, pub.gross)
	t.Logf("hold-out TRACK vs truth n %d P50 %.1f P95 %.1f P99 %.1f 1σ %.1f%%", hold.n, hold.p50, hold.p95, hold.p99, 100*hold.cal1)
	t.Logf("hold-out FIX vs truth   n %d P50 %.1f P95 %.1f P99 %.1f", snap.n, snap.p50, snap.p95, snap.p99)
	q := h.e.QualityReport(60, false)
	t.Logf("live hold-out (vs ADS-B ref): fix n %d P50 %.1f P95 %.1f cross P95 %.1f; track n %d P50 %.1f P95 %.1f cross P50 %.1f P95 %.1f",
		q.Fix.N, q.Fix.P50, q.Fix.P95, q.Fix.CrossP95, q.Track.N, q.Track.P50, q.Track.P95, q.Track.CrossP50, q.Track.CrossP95)
	worst := slices.Clone(h.hold)
	slices.SortFunc(worst, func(a, b errSample) int {
		switch {
		case a.errH > b.errH:
			return -1
		case a.errH < b.errH:
			return 1
		}
		return 0
	})
	for i := 0; i < len(worst) && i < 15; i++ {
		w := worst[i]
		t.Logf("  worst hold-out %06x t %.1f err %.0f σ %.0f grade %d", w.icao, w.tS, w.errH, w.sigmaH, w.grade)
	}
	byAC := map[uint32][2]float64{} // count of > 1 km published samples, max error
	for _, p := range h.pub {
		if p.errH > 1000 {
			r := byAC[p.icao]
			r[0]++
			r[1] = math.Max(r[1], p.errH)
			byAC[p.icao] = r
		}
	}
	for k, r := range byAC {
		if r[0] >= 20 {
			t.Logf("  published > 1 km: %06x %0.f samples, max %.0f m", k, r[0], r[1])
		}
	}
	t.Logf("engine stats: %+v", h.e.Stats())
}
