package mlat

import (
	"testing"
	"time"

	"airview/internal/sim/simnet"
)

// DESIGN §13.2 accuracy gates, rows 1-3, end to end on simnet with fixed
// seeds. The engine runs the real clock filter (GPS anchors need their
// 10-minute probation), so every run lasts 16 simulated minutes and is
// evaluated over the last 4 (MLAT_GATE_MIN / MLAT_GATE_EVAL override).
// Errors are horizontal, of the positions published to the map (tracker
// output, displayable grades), against the simulator's true position at
// the published transmit time, for aircraft inside the network radius.

func gateDur() (time.Duration, float64) {
	return envDur("MLAT_GATE_MIN", 16*time.Minute), envDur("MLAT_GATE_EVAL", 12*time.Minute).Seconds()
}

func TestGateRow1GPS(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet gate (≈30 s)")
	}
	d, ev := gateDur()
	_, pub := gateRun(t, "row1 8 GPS 150 km", harnessCfg{seed: 11, devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 8},
		radiusKm: 150, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev}, d)
	if pub.n < 2000 || pub.p50 >= 20 || pub.p95 >= 60 || pub.cal1 < 0.60 || pub.cal1 > 0.75 {
		t.Errorf("gate: n %d P50 %.1f (< 20) P95 %.1f (< 60) 1σ %.1f %% (60-75)", pub.n, pub.p50, pub.p95, 100*pub.cal1)
	}
}

func TestGateRow2Mixed(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet gate (≈60 s)")
	}
	d, ev := gateDur()
	h, pub := gateRun(t, "row2 4 GPS + 20 SDR", harnessCfg{seed: 12, devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 4, simnet.DeviceSDR: 20},
		radiusKm: 150, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev}, d)
	n4, f4 := h.footprint(20, false)
	na, fa := h.footprint(20, true)
	t.Logf("row2: Mode S-only aircraft heard by >= 4 stations (>= 20 transmissions): %d, with fixes %d (%.1f %%); by >= 4 anchors: %d, with fixes %d",
		n4, f4, 100*float64(f4)/float64(max(n4, 1)), na, fa)
	if pub.n < 2000 || pub.p50 >= 80 || pub.p95 >= 250 {
		t.Errorf("gate: n %d P50 %.1f (< 80) P95 %.1f (< 250)", pub.n, pub.p50, pub.p95)
	}
	if n4 < 10 || float64(f4) < 0.9*float64(n4) {
		t.Errorf("gate: Mode S-only aircraft in the 4-station footprint with fixes %d of %d (>= 90 %%)", f4, n4)
	}
}

func TestGateRow3SDR(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet gate (≈60 s)")
	}
	d, ev := gateDur()
	_, pub := gateRun(t, "row3 20 SDR", harnessCfg{seed: 13, devs: map[simnet.Device]int{simnet.DeviceSDR: 20},
		radiusKm: 150, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev}, d)
	if pub.n < 2000 || pub.p50 >= 200 {
		t.Errorf("gate: n %d P50 %.1f (< 200)", pub.n, pub.p50)
	}
}
