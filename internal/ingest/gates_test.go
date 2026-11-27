package ingest

import (
	"math"
	"testing"
	"time"

	"airview/internal/sim/simnet"
)

// gateRun runs a simnet network through the ingest and checks the DESIGN
// §13.2 grouper gate: unique count = truth ± 0.1 % (overall and for DF11,
// which every aircraft repeats identically every 0.8-1.2 s), and that MLAT
// groups never mix transmissions.
func gateRun(t *testing.T, name string, rx, aircraft int, dur time.Duration, maxErr float64, mix map[simnet.Device]float64, setup func(*netDriver)) {
	t.Helper()
	n := newSimNetMix(t, 11, rx, aircraft, mix)
	clk := &vclock{}
	clk.set(n.UnixNs(0))
	h := newCountHandler()
	sink := &groupSink{}
	ing := newTestIngest(t, h, clk, func(o *Options) { o.MLAT = sink })
	d := newNetDriver(t, n, ing, clk)
	if setup != nil {
		setup(d)
	}
	n.AddSink(d)
	t0 := time.Now()
	d.run(dur)
	el := time.Since(t0)

	st := ing.Stats()
	truth := d.truthCount(-1)
	truth11 := d.truthCount(11)
	got := int(h.n.Load())
	got11 := h.byDF[11]
	rel := func(a, b int) float64 { return math.Abs(float64(a-b)) / float64(b) }
	t.Logf("%s: %d stations, %d frames in %v (%.0f copies/s wall); truth %d unique (%d DF11), grouper %d (%d DF11); "+
		"joined %d (unique-rule %d), same %d, dups %d, tooOld %d, late %d, flushed %d, lateMLAT %d, live %d",
		name, rx, d.frames, el.Round(time.Millisecond), float64(d.frames)/el.Seconds(), truth, truth11, got, got11,
		st.Joined, st.Unique, st.Same, st.Dups, st.TooOld, st.Late, st.Flushed, st.LateMLAT, st.Live)
	if truth < 1000 || truth11 < 100 {
		t.Fatalf("too little traffic: %d transmissions, %d DF11", truth, truth11)
	}
	if e := rel(got, truth); e > maxErr {
		t.Errorf("%s: unique count %d vs truth %d: error %.3f %% > %.2f %%", name, got, truth, 100*e, 100*maxErr)
	}
	if e := rel(got11, truth11); e > 0.001 {
		t.Errorf("%s: DF11 unique count %d vs truth %d: error %.3f %% > 0.1 %%", name, got11, truth11, 100*e)
	}
	if st.Copies+st.TooOld+st.Late != d.frames {
		t.Errorf("%s: %d copies + %d too old + %d late != %d frames", name, st.Copies, st.TooOld, st.Late, d.frames)
	}
	if st.Live != 0 {
		t.Errorf("%s: %d groups left after Flush", name, st.Live)
	}
	// purity of the MLAT groups: all timed copies of a group are one truth
	// transmission
	mixed, timed := 0, 0
	for _, g := range sink.groups {
		var seq uint64
		ok := true
		for _, c := range g.Copies {
			if !c.Timed {
				continue
			}
			s, found := d.byTick[tickKey{c.Station, c.Epoch, c.Ticks}]
			if !found {
				continue
			}
			if seq == 0 {
				seq = s
			} else if s != seq {
				ok = false
			}
		}
		timed++
		if !ok {
			mixed++
		}
	}
	if timed == 0 && setup == nil {
		t.Errorf("%s: no MLAT groups", name)
	}
	// Identical replies 0.74 ms apart (interrogation bursts) cannot be told
	// apart across stations by host-derived time; the MLAT clustering
	// (DESIGN §9.3) splits them with network time. Bound the share anyway.
	if timed > 0 && float64(mixed)/float64(timed) > 0.01 {
		t.Errorf("%s: %d of %d MLAT groups mix transmissions", name, mixed, timed)
	}
	t.Logf("%s: %d MLAT groups (%d mixed), %d late deliveries", name, timed, mixed, len(sink.late))
}

// TestGateDF11Repeat100 is the DESIGN §13.2 gate "DF11 repeat every 0.8 s,
// 100 stations: grouper unique count = truth ± 0.1 %".
func TestGateDF11Repeat100(t *testing.T) {
	dur := 60 * time.Second
	if testing.Short() {
		dur = 20 * time.Second
	}
	gateRun(t, "df11-100", 100, 250, dur, 0.001, nil, nil)
}

// TestGate100StationsHostTiming repeats the dedup gate with hostile host
// timing: a fifth of the stations deliver no timestamps (host time only),
// a fifth have host clocks off by up to ±3 s (no PING: the arrival
// envelope and the latency estimator must absorb it), and every tenth is a
// Beast 30004 map-only link.
func TestGate100StationsHostTiming(t *testing.T) {
	dur := 60 * time.Second
	if testing.Short() {
		dur = 20 * time.Second
	}
	gateRun(t, "hostile-100", 100, 250, dur, 0.003, nil, func(d *netDriver) {
		for i := range d.untimed {
			switch i % 5 {
			case 1:
				d.untimed[i] = true
			case 2:
				d.hostErrNs[i] = int64((float64(i%13)/6 - 1) * 3e9)
			}
			if i%10 == 3 {
				d.mapOnly[i] = true
			}
		}
	})
}

// TestGateSDRHeavy uses an all-SDR network (54.6 ms sample blocks, tens of
// ppm): the tick envelope must remove the block jitter.
func TestGateSDRHeavy(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	gateRun(t, "sdr-60", 60, 200, 40*time.Second, 0.001, map[simnet.Device]float64{simnet.DeviceSDR: 0.9, simnet.DeviceAirspy: 0.1}, nil)
}
