package survey

import (
	"fmt"
	"os"
	"testing"
	"time"

	"airview/internal/mlat"
	"airview/internal/sim/simnet"
)

// Acceptance gates of DESIGN §13.2 on simnet (fixed seeds). The numbers
// of the last runs are recorded in docs/aggregator/MLAT-RESULTS.md.

// skipShort: the gates take 1-6 minutes each (simulated 15-45 minutes);
// they run with SURVEY_GATES=1 (go test -timeout 60m). TestSimnetSmoke
// runs by default.
func skipShort(t *testing.T) {
	if os.Getenv("SURVEY_GATES") == "" {
		t.Skip("simnet gate: set SURVEY_GATES=1 (and -timeout 60m)")
	}
}

// TestSimnetSmoke: the end-to-end path (simnet → ingest → Tee → engine
// clock filter and references → survey) on a short run: an unlocated
// GPS station and a 12 MHz station declared 300 m off.
func TestSimnetSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	h := newSim(t, simCfg{seed: 21, gps: 6, sdr: 8, aircraft: 200, targets: []target{
		{dev: simnet.DeviceRadarcapeGPS, eKm: 10, nKm: 25},
		{dev: simnet.DeviceBeast, eKm: -25, nKm: -10, declared: true, declErrM: 300, altRef: "hae"},
	}, warmup: 11 * time.Minute, cfg: Config{MaxCheckFrac: 1}})
	h.run(4 * time.Minute)
	for k := range 2 {
		r, ok := h.resultOf(k)
		if !ok || !r.Valid {
			t.Fatalf("target %d: no result (stats %+v)", k, h.s.Stats())
		}
		h.logResult(k, fmt.Sprintf("smoke %d", k), r)
		eh, _ := h.truthErr(k, &r)
		if eh > 30 || eh > 4*r.Solution.SigmaHM+5 {
			t.Errorf("target %d: error %.1f m σ_h %.1f m", k, eh, r.Solution.SigmaHM)
		}
	}
	if r, _ := h.resultOf(1); !r.DeclaredSuspect {
		t.Error("declaration 300 m off not flagged")
	}
	if sv, ok := h.sunk[h.station(0)]; !ok || !sv.Valid {
		t.Error("survey not published to the engine")
	} else if src := h.e.PositionSource(h.station(0)); src != mlat.PosSurvey {
		t.Errorf("unlocated GPS station: engine position source %v, want survey (AutoUseSurvey)", src)
	}
}

// TestGateSurveyGPS: an unlocated GPS-timed station (Radarcape GPS) is
// surveyed to σ_h ≤ 25 m and an error < 25 m within 5 minutes of
// collection (§13.2 "GPS target < 25 m in 5 minutes").
func TestGateSurveyGPS(t *testing.T) {
	skipShort(t)
	h := newSim(t, simCfg{seed: 11, gps: 6, sdr: 10, targets: []target{{dev: simnet.DeviceRadarcapeGPS, eKm: 23, nKm: -31}},
		warmup: 12 * time.Minute, cfg: Config{MinDuration: 3 * time.Minute, MaxDuration: 5 * time.Minute}})
	h.run(6 * time.Minute)
	r, ok := h.resultOf(0)
	if !ok || !r.Valid {
		t.Fatalf("no result (stats %+v)", h.s.Stats())
	}
	h.logResult(0, "GPS", r)
	eh, _ := h.truthErr(0, &r)
	if eh > 25 || r.Solution.SigmaHM > 25 {
		t.Fatalf("GPS survey: error %.1f m, σ_h %.1f m (gate 25 m)", eh, r.Solution.SigmaHM)
	}
	if eh > 4*r.Solution.SigmaHM {
		t.Fatalf("GPS survey σ not honest: error %.1f m vs σ_h %.1f m", eh, r.Solution.SigmaHM)
	}
}

// TestGateSurvey12MHz: unlocated 12 MHz stations, a hardware counter
// (Beast) and an RTL-SDR (dump1090 2.4 MS/s in 12 MHz units), surveyed for
// up to 30 minutes (§13.2 "SDR target < 300 m in 30 minutes"; WP gate
// σ_h ≤ 500 m).
func TestGateSurvey12MHz(t *testing.T) {
	skipShort(t)
	h := newSim(t, simCfg{seed: 12, gps: 6, sdr: 10, targets: []target{
		{dev: simnet.DeviceBeast, eKm: -40, nKm: 18},
		{dev: simnet.DeviceSDR, eKm: 35, nKm: 30, hint: true},
	}, warmup: 12 * time.Minute, cfg: Config{MaxDuration: 30 * time.Minute}})
	h.run(31 * time.Minute)
	for k, name := range []string{"Beast hw12", "RTL-SDR sdr12"} {
		r, ok := h.resultOf(k)
		if !ok || !r.Valid {
			t.Fatalf("%s: no result (stats %+v)", name, h.s.Stats())
		}
		h.logResult(k, name, r)
		eh, _ := h.truthErr(k, &r)
		if eh > 300 || r.Solution.SigmaHM > 500 {
			t.Errorf("%s: error %.1f m, σ_h %.1f m (gates 300 m / 500 m)", name, eh, r.Solution.SigmaHM)
		}
		if eh > 4*r.Solution.SigmaHM+10 {
			t.Errorf("%s: σ not honest: error %.1f m vs σ_h %.1f m", name, eh, r.Solution.SigmaHM)
		}
	}
}

// TestGateDeclared300m: declared positions 300 m off are flagged suspect
// within 15 minutes (§13.2), for a GPS and a 12 MHz station; a correct
// declaration is not.
func TestGateDeclared300m(t *testing.T) {
	skipShort(t)
	h := newSim(t, simCfg{seed: 13, gps: 6, sdr: 10, targets: []target{
		{dev: simnet.DeviceRadarcapeGPS, eKm: 20, nKm: 25, declared: true, declErrM: 300, altRef: "hae"},
		{dev: simnet.DeviceBeast, eKm: -30, nKm: -20, declared: true, declErrM: 300, altRef: "hae"},
		{dev: simnet.DeviceRadarcapeGPS, eKm: -10, nKm: 40, declared: true, declErrM: 0, altRef: "hae"},
	}, warmup: 12 * time.Minute, cfg: Config{MaxCheckFrac: 1}})
	h.run(15 * time.Minute)
	for k, name := range []string{"GPS 300 m off", "Beast 300 m off", "GPS correct"} {
		r, ok := h.resultOf(k)
		if !ok {
			t.Fatalf("%s: no result within 15 minutes (stats %+v)", name, h.s.Stats())
		}
		h.logResult(k, name, r)
		_, flagged := h.flags[h.station(k)]
		want := k < 2
		if flagged != want || r.DeclaredSuspect != want {
			t.Errorf("%s: suspect %v (flag %v), want %v", name, r.DeclaredSuspect, flagged, want)
		}
	}
}

// TestGateAltRef: a declared MSL altitude with the reference ambiguous
// (mlat-client) is resolved to MSL within 30 minutes (§13.2), on three
// seeds; the verdict may stay undecided ("") but must never be wrong.
func TestGateAltRef(t *testing.T) {
	skipShort(t)
	right := 0
	for _, seed := range []uint64{14, 15, 16} {
		h := newSim(t, simCfg{seed: seed, gps: 6, sdr: 10, targets: []target{
			{dev: simnet.DeviceRadarcapeGPS, eKm: 15, nKm: -20, declared: true, altRef: "ambiguous"},
		}, warmup: 12 * time.Minute, cfg: Config{MaxDuration: 30 * time.Minute}})
		h.run(31 * time.Minute)
		r, ok := h.resultOf(0)
		if !ok {
			t.Fatalf("seed %d: no result (stats %+v)", seed, h.s.Stats())
		}
		h.logResult(0, fmt.Sprintf("altRef seed %d", seed), r)
		switch r.AltRef.Chosen {
		case "msl":
			right++
		case "":
		default:
			t.Errorf("seed %d: wrong altRef verdict %+v", seed, r.AltRef)
		}
	}
	t.Logf("altRef: %d/3 decided correctly within 30 minutes", right)
	if right < 2 {
		t.Errorf("altRef decided correctly in %d/3 runs (gate: 2/3)", right)
	}
}
