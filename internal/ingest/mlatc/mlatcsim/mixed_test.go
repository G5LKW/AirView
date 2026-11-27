package mlatcsim

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"airview/internal/ingest/mlatc"
	"airview/internal/sim/simnet"
)

// mixedDevices is the H-mlatc acceptance world: 3 GPS Radarcapes, 3 Beasts
// and 6 RTL-SDRs over a 150 km disc; receivers 1, 4, 7 and 8 (one GPS, one
// Beast, two SDR) are the ones that may connect as mlat-clients.
var mixedDevices = []simnet.Device{
	simnet.DeviceRadarcapeGPS, simnet.DeviceRadarcapeGPS, simnet.DeviceRadarcapeGPS,
	simnet.DeviceBeast, simnet.DeviceBeast, simnet.DeviceBeast,
	simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR,
}

var mixedMLATC = []int{1, 4, 7, 8}

func envDur(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return d
	}
	return def
}

// TestMixedHoldout is the H-mlatc acceptance measurement: the same simnet
// world, once with every receiver on AVF/1, then with four receivers as
// mlat-clients on 31090 under the old protocol default (paired sync; both
// pairing readings of the reference client) and under the new default
// (split_sync offered, the client sends single syncs). The mlat-client
// stations must no longer degrade the hold-out: the split-sync P95 must
// stay within 25 % (+10 m) of the all-AVF run.
//
// Simulated time: MLATCSIM_DUR (default 8 min, evaluation from minute 3);
// seeds: MLATCSIM_SEEDS (default "31").
func TestMixedHoldout(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet MLAT run")
	}
	dur := envDur("MLATCSIM_DUR", 8*time.Minute)
	seeds := []uint64{31}
	if s := os.Getenv("MLATCSIM_SEEDS"); s != "" {
		seeds = nil
		for _, f := range splitFields(s) {
			if v, err := strconv.ParseUint(f, 10, 64); err == nil {
				seeds = append(seeds, v)
			}
		}
	}
	type variant struct {
		name    string
		mlatc   bool
		split   mlatc.SplitSyncMode
		honour  bool
		pairing Pairing
	}
	variants := []variant{
		{name: "all-AVF"},
		{name: "mlatc pairs (disjoint), split off", mlatc: true, split: mlatc.SplitSyncOff, pairing: PairsDisjoint},
		{name: "mlatc pairs (overlap), split off", mlatc: true, split: mlatc.SplitSyncOff, pairing: PairsOverlap},
		{name: "mlatc, split default (auto)", mlatc: true, split: mlatc.SplitSyncAuto, honour: true},
		{name: "mlatc, split offered but ignored", mlatc: true, split: mlatc.SplitSyncAuto, honour: false, pairing: PairsOverlap},
	}
	devs, sel := mixedDevices, mixedMLATC
	if os.Getenv("MLATCSIM_WORLD") == "sdr" {
		// The WP-E2 diagnostic world: RTL-SDRs and one GPS station.
		devs = []simnet.Device{simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR,
			simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceRadarcapeGPS}
		sel = []int{2, 3, 4}
	}
	via := func(on bool) []Transport {
		v := make([]Transport, len(devs))
		if on {
			for _, i := range sel {
				v[i] = MLATC
			}
		}
		return v
	}
	for _, seed := range seeds {
		res := map[string]Result{}
		for _, v := range variants {
			if only := os.Getenv("MLATCSIM_ONLY"); only != "" && !slices.ContainsFunc(strings.Split(only, "|"),
				func(o string) bool { return strings.Contains(v.name, o) }) {
				continue
			}
			r, err := New(Config{PosSigmaM: envFloat("MLATCSIM_SIGMA_AVF", 0), MLATCPosSigmaM: envFloat("MLATCSIM_SIGMA_MLATC", 0), Seed: seed, Devices: devs, Via: via(v.mlatc), Aircraft: 250,
				Duration: dur, EvalFrom: envDur("MLATCSIM_EVAL", 3*time.Minute), Logf: logIf(t),
				SeenEvery: envDur("MLATCSIM_SEEN", 0), SplitSync: v.split, HonourSplit: v.honour, Pairing: v.pairing})
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.Execute(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			res[v.name] = out
			t.Logf("seed %d %-36s hold-out %v | published %v | clusters %d solves ok %d | syncs %d ssyncs %d | results to clients %d | %v",
				seed, v.name, out.Holdout, out.Published, out.Stats.Clusters, out.Stats.SolveOK, out.MLATC.Syncs, out.MLATC.SSyncs,
				sum(out.Results), out.Wall.Round(time.Second))
		}
		base, after := res["all-AVF"], res["mlatc, split default (auto)"]
		if base.Holdout.N == 0 || after.Holdout.N == 0 {
			continue // a filtered diagnostic run
		}
		if base.Holdout.N < 100 || after.Holdout.N < 100 {
			t.Fatalf("seed %d: too few hold-out samples (%d / %d)", seed, base.Holdout.N, after.Holdout.N)
		}
		if after.Holdout.P95 > 1.25*base.Holdout.P95+10 {
			t.Errorf("seed %d: mlat-client stations still degrade the hold-out: P95 %.1f m vs all-AVF %.1f m",
				seed, after.Holdout.P95, base.Holdout.P95)
		}
		if after.Holdout.N < base.Holdout.N*8/10 {
			t.Errorf("seed %d: hold-out samples %d vs all-AVF %d", seed, after.Holdout.N, base.Holdout.N)
		}
		if after.MLATC.SSyncs == 0 || after.MLATC.Syncs != 0 {
			t.Errorf("seed %d: split sync not in use (ssync %d, sync %d)", seed, after.MLATC.SSyncs, after.MLATC.Syncs)
		}
	}
}

func logIf(t *testing.T) func(string, ...any) {
	if os.Getenv("MLATCSIM_VERBOSE") == "" {
		return nil
	}
	return t.Logf
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil {
		return v
	}
	return def
}

func sum(m map[int]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == ' ' || c == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
