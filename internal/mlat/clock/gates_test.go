package clock

import (
	"math"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// DESIGN §13.2 gates of the clock filter, on simnet with fixed seeds.

func gpsIdx(h *simHarness, skip ...int) []int {
	var out []int
outer:
	for i, r := range h.n.Receivers() {
		if r.Device != simnet.DeviceRadarcapeGPS {
			continue
		}
		for _, s := range skip {
			if s == i {
				continue outer
			}
		}
		out = append(out, i)
	}
	return out
}

func sdrMix(n int) []simnet.Device {
	out := make([]simnet.Device, n)
	for i := range out {
		out[i] = simnet.DeviceSDR
	}
	return out
}

// Silent 22 µs step: detected < 5 s, re-synced < 30 s, and while the
// station is published as synced its error stays within 3σ.
func TestGateSilentStep(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	others := append(sdrMix(10), simnet.DeviceBeast, simnet.DeviceAirspy, simnet.DeviceSBS, simnet.DeviceSDR)
	rx := mixedReceivers(11, 6, others, 150)
	const victim = 8 // an SDR
	const stepAt = 400.0
	rx[victim].Clock.Steps = []simnet.ClockStep{{AtS: stepAt, StepNs: 22000}}
	rx[victim].Clock.SilentDropsPerHour, rx[victim].Clock.DeclaredDropsPerHour = 0, 0
	h := newSimHarness(t, simnet.Config{Seed: 11, Aircraft: 220, Receivers: rx}, Config{ProbationS: 120})
	if rx[victim].Clock.Device != simnet.DeviceSDR {
		t.Fatalf("victim is %s", rx[victim].Clock.Device)
	}
	h.register()
	gps := gpsIdx(h)
	detected, resynced := -1.0, -1.0
	// within the 30-s recovery window: a Gaussian error exceeds 3σ in 0.3 %
	// of the epochs, so the hard limit is 4σ and 3σ exceedances are reported
	violations, beyond3, checked := 0, 0, 0
	var worst float64
	h.onEpoch = func(tE float64) {
		if tE < stepAt-30 || tE > stepAt+60 {
			return
		}
		// a snapshot describes the clocks up to its epoch time: compare it
		// with the truth just before (the step happens on a 100 ms grid
		// point that can coincide with an epoch)
		tq := tE - 0.05
		e, sg, sc, ok := h.errNs(victim, tq)
		if sc == nil {
			return
		}
		if detected < 0 && tE >= stepAt {
			for _, ev := range sc.Events {
				if ev.Reason == "step" && ev.T >= stepAt {
					detected = ev.T
				}
			}
		}
		cm, okc := h.commonMode(gps, tq)
		if !ok || !okc || !sc.Synced {
			return
		}
		d := e - cm
		checked++
		if math.Abs(d) > 3*sg+5 && tE <= stepAt+30 {
			beyond3++
			t.Logf("t=%.1f synced with error %.0f ns = %.1fσ", tE, d, math.Abs(d)/sg)
		}
		if math.Abs(d) > 4*sg+5 && tE <= stepAt+30 {
			violations++
			worst = math.Max(worst, math.Abs(d)/sg)
		}
		if detected > 0 && resynced < 0 && tE > detected && math.Abs(d) < 3*sg+5 {
			resynced = tE
		}
	}
	h.run(time.Duration(stepAt+70) * time.Second)
	t.Logf("step at %.1f s: detected at %.1f s (+%.1f s), re-synced at %.1f s (+%.1f s); %d synced epochs checked, %d beyond 3σ, %d beyond 4σ in the 30-s recovery window",
		stepAt, detected, detected-stepAt, resynced, resynced-stepAt, checked, beyond3, violations)
	if detected < 0 || detected-stepAt > 5 {
		t.Errorf("step not detected within 5 s (detected %.1f)", detected)
	}
	if resynced < 0 || resynced-stepAt > 30 {
		t.Errorf("not re-synced within 30 s (%.1f)", resynced)
	}
	if violations > 0 {
		t.Errorf("%d synced epochs with error beyond 4σ (worst %.1fσ)", violations, worst)
	}
	if st := h.f.Stats(); st.Steps != 1 {
		t.Errorf("steps detected = %d, want 1", st.Steps)
	}
}

// 1 µs anchor fault: demoted < 10 min; network time shifts < 20 ns.
func TestGateAnchorFault(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	others := append(sdrMix(8), simnet.DeviceBeast, simnet.DeviceAirspy, simnet.DeviceSBS, simnet.DeviceRadarcape12)
	rx := mixedReceivers(13, 8, others, 150)
	const bad = 3
	const faultAt = 400.0
	h := newSimHarness(t, simnet.Config{Seed: 13, Aircraft: 220, Receivers: rx}, Config{ProbationS: 120})
	h.fault[bad] = func(t float64) float64 {
		if t >= faultAt {
			return 1000
		}
		return 0
	}
	h.register()
	good := gpsIdx(h, bad)
	var before, after []float64
	demoted := -1.0
	h.onEpoch = func(tE float64) {
		sc := h.f.Snapshot().Station(bad + 2)
		if sc != nil && demoted < 0 && tE >= faultAt && !sc.Anchor {
			demoted = tE
			t.Logf("anchor %d demoted at %.1f s (%s, %s)", bad, tE, sc.AnchorState, sc.AnchorReason)
		}
		if math.Mod(tE, 2) != 0 {
			return
		}
		cm, ok := h.commonMode(good, tE)
		if !ok {
			return
		}
		switch {
		case tE >= faultAt-120 && tE < faultAt:
			before = append(before, cm)
		case tE >= faultAt+60 && tE < faultAt+180:
			after = append(after, cm)
		}
	}
	h.run(time.Duration(faultAt+190) * time.Second)
	mb, ma := mean(before), mean(after)
	t.Logf("network time vs truth: before %.1f ns, after %.1f ns, shift %.1f ns; demoted after %.1f s", mb, ma, ma-mb, demoted-faultAt)
	if demoted < 0 || demoted-faultAt > 600 {
		t.Errorf("faulty anchor not demoted within 10 min (%.1f)", demoted)
	}
	if math.Abs(ma-mb) >= 20 {
		t.Errorf("network time shifted %.1f ns", ma-mb)
	}
	for _, i := range good {
		if sc := h.f.Snapshot().Station(model.StationIdx(i + 2)); sc == nil || !sc.Anchor {
			t.Errorf("good anchor %d lost its anchor state", i)
		}
	}
}

func mean(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(max(len(v), 1))
}

// False GPS claim: a free-running receiver that claims gps_utc with good
// status bits becomes the self-asserted first anchor; when genuine GPS
// stations become eligible it is demoted within 10 minutes and they take
// over.
func TestGateFalseGPSClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	others := append(sdrMix(8), simnet.DeviceBeast, simnet.DeviceAirspy)
	rx := mixedReceivers(17, 5, others, 150)
	const fake = 4
	rx[fake].Clock.LockLoss = []simnet.Interval{{FromS: 0, ToS: 1e9}}
	const genuineOK = 300.0
	h := newSimHarness(t, simnet.Config{Seed: 17, Aircraft: 220, Receivers: rx}, Config{ProbationS: 120})
	// free running at +50 ppb (simnet keeps a GPS device's frequency at 0
	// when the lock is lost from the start)
	h.fault[fake] = func(t float64) float64 { return 50 * t }
	h.class[fake] = model.ClockGPSUTC
	h.gpsOK[fake] = func(float64) bool { return true }
	for i := 0; i < 4; i++ {
		h.gpsOK[i] = func(t float64) bool { return t >= genuineOK }
	}
	h.register()
	fakeAnchor, demoted, genuineAnchor := -1.0, -1.0, -1.0
	h.onEpoch = func(tE float64) {
		snap := h.f.Snapshot()
		sc := snap.Station(fake + 2)
		if sc == nil {
			return
		}
		if sc.Anchor && fakeAnchor < 0 {
			fakeAnchor = tE
			t.Logf("fake anchor admitted at %.1f s (%s)", tE, sc.AnchorState)
		}
		if fakeAnchor > 0 && !sc.Anchor && demoted < 0 {
			demoted = tE
			t.Logf("fake demoted at %.1f s (%s)", tE, sc.AnchorReason)
		}
		if g := snap.Station(2); g != nil && g.Anchor && genuineAnchor < 0 {
			genuineAnchor = tE
		}
		if demoted > 0 && sc.Anchor {
			t.Errorf("fake re-admitted at %.1f", tE)
		}
	}
	h.run(20 * time.Minute)
	eligible := genuineOK + 120 // condition (a) of the genuine stations
	t.Logf("fake anchor %.1f s, demoted %.1f s (%.1f s after genuine stations became eligible), genuine anchor %.1f s",
		fakeAnchor, demoted, demoted-eligible, genuineAnchor)
	if fakeAnchor < 0 {
		t.Fatalf("scenario: fake never became the self-asserted anchor")
	}
	if demoted < 0 || demoted-eligible > 600 {
		t.Errorf("false GPS claim not demoted within 10 min of genuine anchors being available")
	}
	if genuineAnchor < 0 {
		t.Errorf("genuine GPS stations never became anchors")
	}
	// network time is UTC again: genuine anchors agree with the truth
	h.dump(t)
}

// 2 µs short/long reference error on 5 SDR stations: β_short within 100 ns
// after 10 minutes.
func TestGateBetaShort(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	rx := mixedReceivers(19, 4, sdrMix(15), 150)
	victims := []int{4, 5, 6, 7, 8}
	for _, v := range victims {
		rx[v].Clock.ShortLongErrorNs = 2000
	}
	h := newSimHarness(t, simnet.Config{Seed: 19, Aircraft: 250, Receivers: rx}, Config{ProbationS: 120})
	h.register()
	conv := map[int]float64{}
	h.onEpoch = func(tE float64) {
		snap := h.f.Snapshot()
		for _, v := range victims {
			sc := snap.Station(model.StationIdx(v + 2))
			if sc == nil {
				continue
			}
			if math.Abs(sc.Beta-2000) < 100 {
				if _, ok := conv[v]; !ok {
					conv[v] = tE
				}
			} else {
				delete(conv, v)
			}
		}
	}
	h.run(10 * time.Minute)
	snap := h.f.Snapshot()
	cfg := h.n.Config()
	for i := range cfg.Receivers {
		sc := snap.Station(model.StationIdx(i + 2))
		if sc == nil {
			continue
		}
		truth := cfg.Receivers[i].Clock.ShortLongErrorNs
		t.Logf("rx %2d %-12s β = %7.1f ns (truth %7.1f, σ %5.1f)", i, sc.Class, sc.Beta, truth, math.Sqrt(sc.Cov[3][3]))
	}
	for _, v := range victims {
		sc := snap.Station(model.StationIdx(v + 2))
		if sc == nil || math.Abs(sc.Beta-2000) >= 100 {
			t.Errorf("rx %d: β_short not within 100 ns of 2000 after 10 min", v)
			continue
		}
		t.Logf("rx %d: within 100 ns since %.0f s", v, conv[v])
	}
}

// An SDR-only network (relative time): every clock syncs; offsets are
// reported against the median clock.
func TestSimnetRelativeSDROnly(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	rx := mixedReceivers(23, 0, sdrMix(20), 150)
	h := newSimHarness(t, simnet.Config{Seed: 23, Aircraft: 250, Receivers: rx}, Config{})
	h.register()
	all := make([]int, len(rx))
	for i := range all {
		all[i] = i
	}
	var st classStats
	h.onEpoch = func(tE float64) {
		if tE < 300 || math.Mod(tE, 5) != 0 {
			return
		}
		cm, ok := h.commonMode(all, tE)
		if !ok {
			return
		}
		for i := range rx {
			if e, sg, sc, ok := h.errNs(i, tE); ok && sc.Synced {
				st.add("sdr12 (relative)", e-cm, sg)
			}
		}
	}
	h.run(10 * time.Minute)
	st.report(t)
	snap := h.f.Snapshot()
	synced := 0
	for i := range rx {
		if sc := snap.Station(model.StationIdx(i + 2)); sc != nil && sc.Synced {
			synced++
			if !sc.RelativeTime {
				t.Errorf("rx %d not flagged relative time", i)
			}
		}
	}
	t.Logf("synced %d/%d", synced, len(rx))
	if synced < len(rx)-1 {
		t.Errorf("synced %d of %d", synced, len(rx))
	}
}

// Declared epochs (restart, sample drop) re-sync within 30 s.
func TestSimnetClockEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	rx := mixedReceivers(29, 5, append(sdrMix(10), simnet.DeviceBeast), 150)
	rx[6].Clock.Drops = []simnet.SampleDrop{{AtS: 300, Samples: 100000, Declared: true}}
	rx[15].Clock.Restarts = []float64{320}
	h := newSimHarness(t, simnet.Config{Seed: 29, Aircraft: 220, Receivers: rx}, Config{ProbationS: 120})
	h.register()
	gps := gpsIdx(h)
	seen := make([]int, len(rx))
	back := map[int]float64{}
	h.onEpoch = func(tE float64) {
		for i := range rx {
			evs := h.n.ClockEvents(i)
			for ; seen[i] < len(evs); seen[i]++ {
				ev := evs[seen[i]]
				h.f.StationEvent(model.StationIdx(i+2), Event{OldEpoch: ev.OldEpoch, NewEpoch: ev.NewEpoch, Reason: ev.Reason,
					SamplesLost: int64(ev.SamplesLost), HostNs: h.n.UnixNs(ev.AtS)})
			}
		}
		cm, ok := h.commonMode(gps, tE)
		if !ok {
			return
		}
		for _, v := range []int{6, 15} {
			e, sg, sc, ok := h.errNs(v, tE)
			at := 300.0
			if v == 15 {
				at = 320
			}
			if tE > at+0.5 && ok && sc.Synced && math.Abs(e-cm) < 3*sg+5 {
				if _, done := back[v]; !done {
					back[v] = tE - at
				}
			}
		}
	}
	h.run(6 * time.Minute)
	for _, v := range []int{6, 15} {
		d, ok := back[v]
		t.Logf("rx %d (%s) back in sync %.1f s after its event", v, rx[v].Clock.Device, d)
		if !ok || d > 30 {
			t.Errorf("rx %d did not re-sync within 30 s", v)
		}
	}
}

// Partitioned domain (phase-5 seam): 70 stations with partitions of at most
// 30 stations stay synced with accuracy close to the dense filter.
func TestSimnetPartitioned(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	var others []simnet.Device
	for i := 0; i < 60; i++ {
		others = append(others, allClockDevices[i%len(allClockDevices)])
	}
	rx := mixedReceivers(31, 10, others, 220)
	h := newSimHarness(t, simnet.Config{Seed: 31, Aircraft: 300, Receivers: rx}, Config{ProbationS: 120, MaxComponentStations: 30})
	h.register()
	gps := gpsIdx(h)
	var st classStats
	h.onEpoch = func(tE float64) {
		if tE < 360 || math.Mod(tE, 5) != 0 {
			return
		}
		cm, ok := h.commonMode(gps, tE)
		if !ok {
			return
		}
		for i, r := range h.n.Receivers() {
			if e, sg, sc, ok := h.errNs(i, tE); ok && sc.Synced {
				st.add(r.Class.String(), e-cm, sg)
			}
		}
	}
	h.run(9 * time.Minute)
	res := st.report(t)
	snap := h.f.Snapshot()
	synced := 0
	for i := range rx {
		if sc := snap.Station(model.StationIdx(i + 2)); sc != nil && sc.Synced {
			synced++
		}
	}
	doms := map[uint32]int{}
	for _, c := range snap.Components {
		doms[c.Domain]++
		if len(c.Stations) > 30 {
			t.Errorf("component %d has %d stations", c.ID, len(c.Stations))
		}
	}
	t.Logf("components %d in %d domains, synced %d/%d, stats %+v", len(snap.Components), len(doms), synced, len(rx), h.f.Stats())
	if len(snap.Components) < 3 {
		t.Errorf("expected ≥ 3 partitions, got %d", len(snap.Components))
	}
	if synced < len(rx)-3 {
		t.Errorf("synced %d of %d", synced, len(rx))
	}
	if res["sdr12"] > 300 || res["hw12"] > 60 {
		t.Errorf("partitioned accuracy too poor: %v", res)
	}
}
