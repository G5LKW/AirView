package trust_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/ingest/mlatc/mlatcsim"
	"airview/internal/model"
	"airview/internal/registry/trust"
	"airview/internal/sim/simnet"
)

// table is a Stations source for the simulation: receiver positions from
// the simulator, states set by the test.
type table struct {
	mu     sync.Mutex
	n      *simnet.Network
	state  map[model.StationIdx]trust.State
	proven map[model.StationIdx]trust.Report
}

func (t *table) TrustInfo(st model.StationIdx) (trust.StationInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rx := int(st) - 2
	if t.n == nil || rx < 0 || rx >= t.n.NumReceivers() {
		return trust.StationInfo{}, false
	}
	r := t.n.Receiver(rx)
	s, ok := t.state[st]
	if !ok {
		s = trust.Trusted
	}
	return trust.StationInfo{State: s, Located: true, Lat: r.Lat, Lon: r.Lon, HAE: r.HAE}, true
}

func (t *table) promote(st model.StationIdx, rep trust.Report) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state[st] = trust.Trusted
	t.proven[st] = rep
}

// gateRef lets the mlatc server ask a gate created later (inside Wrap).
type gateRef struct{ g **trust.Gate }

func (r gateRef) Quarantined(st model.StationIdx) bool {
	if *r.g == nil {
		return false
	}
	return (*r.g).Quarantined(st)
}

// The world: 3 GPS, 3 Beast, 6 SDR honest receivers; receivers 9-11 are
// new honest stations (two SDR, one... see devices), 12-14 are hostile.
var qDevices = []simnet.Device{
	simnet.DeviceRadarcapeGPS, simnet.DeviceRadarcapeGPS, simnet.DeviceRadarcapeGPS,
	simnet.DeviceBeast, simnet.DeviceBeast, simnet.DeviceBeast,
	simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR,
	simnet.DeviceBeast, simnet.DeviceSDR, simnet.DeviceSDR, // new, honest
	simnet.DeviceSDR, simnet.DeviceBeast, simnet.DeviceSDR, // new, hostile
}

var (
	qNewHonest = []int{9, 10, 11}
	qHostile   = map[int]mlatcsim.Hostile{
		12: {NoiseNs: 50_000},             // random timing (like mlatc-load's synthetic clients)
		13: {NoiseNs: 50_000},             // the same over the mlat-client protocol
		14: {NoiseNs: 4_000, DriftPPM: 1}, // subtle: a few µs of noise and a wandering rate
	}
)

func qVia() []mlatcsim.Transport {
	v := make([]mlatcsim.Transport, len(qDevices))
	v[13], v[11] = mlatcsim.MLATC, mlatcsim.MLATC
	return v
}

type qScenario struct {
	name string
	// state of each station at start
	initial func(rx int) trust.State
	gateOff bool
}

func runQ(t *testing.T, sc qScenario, seed uint64, dur time.Duration) (mlatcsim.Result, *table, trust.Stats) {
	res, tab, g := runQGate(t, sc, seed, dur)
	return res, tab, g.Stats()
}

func runQGate(t *testing.T, sc qScenario, seed uint64, dur time.Duration) (mlatcsim.Result, *table, *trust.Gate) {
	t.Helper()
	tab := &table{state: map[model.StationIdx]trust.State{}, proven: map[model.StationIdx]trust.Report{}}
	for rx := range qDevices {
		tab.state[mlatcsim.Station(rx)] = sc.initial(rx)
	}
	var gate *trust.Gate
	cfg := mlatcsim.Config{Seed: seed, Devices: qDevices, Via: qVia(), Hostile: qHostile, Duration: dur,
		EvalFrom: dur / 2, SplitSync: 0, HonourSplit: true, Trust: gateRef{&gate},
		Wrap: func(next ingest.MLATSink) ingest.MLATSink {
			gate = trust.New(next, tab, trust.Config{Disabled: sc.gateOff}, tab.promote)
			return gate
		}}
	r, err := mlatcsim.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tab.mu.Lock()
	tab.n = r.Net
	tab.mu.Unlock()
	res, err := r.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res, tab, gate
}

func isHostile(rx int) bool { _, ok := qHostile[rx]; return ok }

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestQuarantineHostileStations is the H-mlatc quarantine acceptance test:
// hostile stations (random timing, ±50 µs, over AVF and over the
// mlat-client protocol; and a subtle one with ±4 µs and a wandering rate)
// are admitted next to three new honest stations.
//
//   - reference: the hostile stations are excluded (as if never admitted);
//   - open admission without the gate: everything joins MLAT at once;
//   - with the gate: every new station starts quarantined.
//
// With the gate the hostile stations must stay quarantined, the new honest
// ones must be proven, and the hold-out error must match the reference.
func TestQuarantineHostileStations(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet MLAT run")
	}
	dur := 12 * time.Minute
	if d, err := time.ParseDuration(os.Getenv("TRUST_DUR")); err == nil {
		dur = d
	}
	const seed = 41
	ref, _, _ := runQ(t, qScenario{name: "reference", initial: func(rx int) trust.State {
		if isHostile(rx) {
			return trust.Excluded
		}
		return trust.Trusted
	}}, seed, dur)
	open, _, _ := runQ(t, qScenario{name: "open", gateOff: true, initial: func(int) trust.State { return trust.Trusted }}, seed, dur)
	gated, tab, gs := runQ(t, qScenario{name: "quarantine", initial: func(rx int) trust.State {
		if isHostile(rx) || contains(qNewHonest, rx) {
			return trust.Quarantined
		}
		return trust.Trusted
	}}, seed, dur)
	t.Logf("reference (hostile excluded): hold-out %v, published %v", ref.Holdout, ref.Published)
	t.Logf("open admission, no gate:      hold-out %v, published %v", open.Holdout, open.Published)
	t.Logf("quarantine gate:              hold-out %v, published %v; gate %+v", gated.Holdout, gated.Published, gs)
	for st, rep := range tab.proven {
		t.Logf("proven: rx %d after %d windows (%d good, %d refs, σ %.0f ns)", int(st)-2, rep.Windows, rep.Good, rep.Refs, rep.LastSigmaNs)
	}
	for rx := range qHostile {
		if _, ok := tab.proven[mlatcsim.Station(rx)]; ok {
			t.Errorf("hostile rx %d was admitted to MLAT", rx)
		}
	}
	for _, rx := range qNewHonest {
		if _, ok := tab.proven[mlatcsim.Station(rx)]; !ok {
			t.Errorf("honest new rx %d was not proven within %v", rx, dur)
		}
	}
	if ref.Holdout.N < 100 || gated.Holdout.N < 100 {
		t.Fatalf("too few hold-out samples: %d / %d", ref.Holdout.N, gated.Holdout.N)
	}
	if gated.Holdout.P95 > 1.25*ref.Holdout.P95+10 {
		t.Errorf("quarantine: hold-out P95 %.1f m vs reference %.1f m", gated.Holdout.P95, ref.Holdout.P95)
	}
	if open.Holdout.P95 < 1.5*ref.Holdout.P95 && open.Holdout.N > 0 {
		t.Logf("note: the hostile stations did not degrade the open run much (P95 %.1f vs %.1f m)", open.Holdout.P95, ref.Holdout.P95)
	}
}

// TestQuarantineBootstrap: a new network where every station is new. With
// no trusted reference the gate judges quarantined stations against each
// other; the honest ones must be proven, the hostile ones not.
func TestQuarantineBootstrap(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet MLAT run")
	}
	dur := 10 * time.Minute
	if d, err := time.ParseDuration(os.Getenv("TRUST_DUR")); err == nil {
		dur = d
	}
	res, tab, gate := runQGate(t, qScenario{name: "bootstrap", initial: func(int) trust.State { return trust.Quarantined }}, 43, dur)
	gs := gate.Stats()
	for rx := range qDevices {
		if rep, ok := gate.Status(mlatcsim.Station(rx)); ok {
			t.Logf("still quarantined: rx %d (hostile %v): %+v", rx, isHostile(rx), rep)
		}
	}
	t.Logf("bootstrap: hold-out %v, gate %+v, proven %d", res.Holdout, gs, len(tab.proven))
	for rx := range qHostile {
		if _, ok := tab.proven[mlatcsim.Station(rx)]; ok {
			t.Errorf("hostile rx %d was admitted to MLAT", rx)
		}
	}
	honest := 0
	for rx := range qDevices {
		if _, ok := tab.proven[mlatcsim.Station(rx)]; ok && !isHostile(rx) {
			honest++
		}
	}
	if honest < len(qDevices)-len(qHostile)-1 {
		t.Errorf("only %d of %d honest stations proven", honest, len(qDevices)-len(qHostile))
	}
	if res.Holdout.N == 0 {
		t.Errorf("no hold-out fixes after the bootstrap")
	}
}
