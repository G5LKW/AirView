package simnet

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/feed"
	"airview/internal/model"
)

const testStartNs = int64(1_780_000_000) * 1e9

func mkClock(t *testing.T, s ClockSpec) *clockModel {
	t.Helper()
	if err := s.normalize(); err != nil {
		t.Fatal(err)
	}
	return newClockModel(0, s, testStartNs, 42)
}

func noRand() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }

func TestClockSDRDrift(t *testing.T) {
	c := mkClock(t, ClockSpec{Device: DeviceSDR, InitialPPM: 50})
	a, _ := st(c, 10, noRand())
	ct, ok := tr(c, 100)
	if !ok {
		t.Fatal("no history")
	}
	if math.Abs(ct.OffsetNs-5e6) > 1 || math.Abs(ct.FreqPPM-50) > 1e-9 {
		t.Fatalf("offset %v ns freq %v ppm", ct.OffsetNs, ct.FreqPPM)
	}
	b, _ := st(c, 110, noRand())
	if a.class != model.ClockSDR12 || a.ticks%5 != b.ticks%5 {
		t.Fatalf("2.4 MS/s quantisation not visible: %d %d", a.ticks, b.ticks)
	}
	want := 100 * (1 + 50e-6) * 12e6
	if d := float64(b.ticks-a.ticks) - want; math.Abs(d) > 5 {
		t.Fatalf("tick delta off by %v", d)
	}
	if a.beastTS != a.ticks&(1<<48-1) {
		t.Fatal("beast timestamp is not the 48-bit counter")
	}
}

func TestClockThermalBand(t *testing.T) {
	c := mkClock(t, ClockSpec{Device: DeviceSDR, InitialPPM: -40, ThermalRampPPMPerS: 0.01, ThermalBandPPM: 2, ThermalSegmentS: 300})
	lo, hi := 0.0, -100.0
	for s := 1.0; s < 4*3600; s += 10 {
		c.advanceTo(s)
		ct, _ := tr(c, s)
		lo, hi = math.Min(lo, ct.FreqPPM), math.Max(hi, ct.FreqPPM)
	}
	if lo < -42.2 || hi > -37.8 || hi-lo < 1 {
		t.Fatalf("thermal wander %v..%v ppm (band -42..-38)", lo, hi)
	}
}

func TestClockSampleDrops(t *testing.T) {
	c := mkClock(t, ClockSpec{Device: DeviceSDR, Drops: []SampleDrop{{AtS: 10, Samples: 24000, Declared: true}, {AtS: 20, Samples: 2400}}})
	before, _ := tr(c, 9.95)
	if _, ok := st(c, 10.005, noRand()); ok {
		t.Fatal("frame received during a sample drop")
	}
	after, ok := st(c, 10.02, noRand())
	if !ok || after.epoch != before.Epoch+1 {
		t.Fatalf("declared drop: epoch %d -> %d", before.Epoch, after.epoch)
	}
	if d := after.offsetNs - before.OffsetNs; math.Abs(d+10e6) > 1 {
		t.Fatalf("declared drop moved the clock by %v ns, want -10 ms", d)
	}
	ev := c.events
	if len(ev) != 1 || ev[0].Reason != model.ClockEvSampleDrop || ev[0].SamplesLost != 24000 || ev[0].AtS != 10 {
		t.Fatalf("events %+v", ev)
	}
	s2, _ := tr(c, 20.05)
	if math.Abs(s2.OffsetNs-(after.offsetNs-1e6)) > 1 || s2.Epoch != after.epoch {
		t.Fatalf("silent drop: offset %v epoch %d", s2.OffsetNs, s2.Epoch)
	}
}

func TestClockStepsAndRestart(t *testing.T) {
	c := mkClock(t, ClockSpec{Device: DeviceBeast, Steps: []ClockStep{{AtS: 5, StepNs: 22000}}, Restarts: []float64{12}})
	a, _ := tr(c, 4.9)
	b, _ := tr(c, 5.1)
	if d := b.OffsetNs - a.OffsetNs; math.Abs(d-22000) > 1e-3 || a.Epoch != b.Epoch {
		t.Fatalf("silent step %v ns, epochs %d %d", d, a.Epoch, b.Epoch)
	}
	pre, _ := st(c, 11.9, noRand())
	post, _ := st(c, 12.1, noRand())
	if post.epoch != pre.epoch+1 || post.ticks >= pre.ticks || post.ticks > 1e9+12e6 {
		t.Fatalf("restart: ticks %d -> %d, epoch %d -> %d", pre.ticks, post.ticks, pre.epoch, post.epoch)
	}
	if len(c.events) != 1 || c.events[0].Reason != model.ClockEvRestart {
		t.Fatalf("events %+v", c.events)
	}
}

func TestClockGPS(t *testing.T) {
	c := mkClock(t, ClockSpec{Device: DeviceRadarcapeGPS, CableBiasNs: 150, PPSJitterNs: 30, BootGPSTimeS: 30,
		LockLoss: []Interval{{50, 80}}, FreeRunPPB: 100})
	// boot: GPS time
	b, _ := tr(c, 10)
	if b.Class != model.ClockGPSGPST || !b.GPSTime || math.Abs(b.OffsetNs-18e9-150) > 200 {
		t.Fatalf("boot state %+v", b)
	}
	bs := statusAt(c, 10)
	if s, _ := feed.ParseBeastStatus(bs[:]); s.UTC || !s.GPSLocked() || !s.GPSTimestamps {
		t.Fatalf("boot status %+v", s)
	}
	// locked UTC
	u, _ := tr(c, 40)
	if u.Class != model.ClockGPSUTC || math.Abs(u.OffsetNs-150) > 200 || u.FreqPPM != 0 {
		t.Fatalf("locked state %+v", u)
	}
	ts, _ := st(c, 40, noRand())
	unix := testStartNs + int64(40e9) + int64(math.Round(u.OffsetNs))
	if d := int64(ts.ticks) - unix; d < -60 || d > 60 {
		t.Fatalf("GPS ticks off by %d ns", d)
	}
	day := int64(ts.ticks) % model.NsPerDay
	if ts.beastTS != feed.GPSTimestamp(int(day/1e9), int(day%1e9)) {
		t.Fatal("Radarcape GPS timestamp format")
	}
	if s, _ := feed.ParseBeastStatus(statusSlice(c, 40)); !s.UTC || !s.GPSLocked() {
		t.Fatalf("locked status %+v", s)
	}
	// lock lost: free running, emulated class
	l1, _ := tr(c, 55)
	l2, _ := tr(c, 75)
	if l1.Class != model.ClockGPSEmul || l1.Locked {
		t.Fatalf("lock loss state %+v", l1)
	}
	if d := l2.OffsetNs - l1.OffsetNs; math.Abs(d-2000) > 1 {
		t.Fatalf("free run drift %v ns over 20 s at 100 ppb", d)
	}
	if s, _ := feed.ParseBeastStatus(statusSlice(c, 60)); s.GPSLocked() {
		t.Fatal("status still locked during lock loss")
	}
	r, _ := tr(c, 85)
	if r.Class != model.ClockGPSUTC || !r.Locked || math.Abs(r.OffsetNs-150) > 200 {
		t.Fatalf("relock %+v", r)
	}
	var reasons []model.ClockEventReason
	for _, e := range c.events {
		reasons = append(reasons, e.Reason)
	}
	if len(c.events) != 3 || c.events[0].AtS != 30 || c.events[1].AtS != 50 || c.events[2].AtS != 80 {
		t.Fatalf("events %+v", c.events)
	}
	for _, r := range reasons {
		if r != model.ClockEvModeChange {
			t.Fatalf("reasons %v", reasons)
		}
	}
}

func TestClockSBSWrap(t *testing.T) {
	c := mkClock(t, ClockSpec{Device: DeviceSBS, InitialPPM: 20})
	c.advanceTo(5)
	for _, s := range []float64{0.5, 1.7, 3.3} {
		ts, _ := st(c, s, noRand())
		if ts.beastTS >= 1<<24 || ts.beastTS != ts.ticks&0xFFFFFF || ts.class != model.ClockSBS20 {
			t.Fatalf("SBS timestamp %x ticks %x", ts.beastTS, ts.ticks)
		}
	}
}

// The clock evolution must not depend on which frames were stamped.
func TestClockIndependentOfObservations(t *testing.T) {
	spec := DefaultClockSpec(DeviceSDR, rand.New(rand.NewPCG(3, 4)))
	spec.SilentStepsPerHour, spec.SilentStepMaxNs = 360, 1000
	a, b := mkClock(t, spec), mkClock(t, spec)
	r := noRand()
	for s := 0.05; s < 60; s += 0.0137 {
		a.advanceTo(s + 0.2)
		a.stamp(s, r)
	}
	b.advanceTo(60.2)
	for _, s := range []float64{50, 55.5, 59.9} {
		x, _ := a.truth(s)
		y, _ := b.truth(s)
		if x != y {
			t.Fatalf("clock truth depends on observations: %+v vs %+v", x, y)
		}
	}
}

func TestDefaultClockSpecs(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for _, d := range Devices {
		s := DefaultClockSpec(d, r)
		if err := s.normalize(); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if s.Class != d.Class() || s.Class.NominalHz() == 0 {
			t.Fatalf("%s: class %v", d, s.Class)
		}
		rs := DefaultReception(d, r)
		if err := rs.normalize(10); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
	}
}

func statusSlice(c *clockModel, t float64) []byte {
	s := c.status(t)
	return s[:]
}

// tr / st / statusAt advance the clock just past t first (queries must be
// made in time order, within the kept history).
func tr(c *clockModel, t float64) (ClockTruth, bool) {
	c.advanceTo(t + 0.1)
	return c.truth(t)
}

func st(c *clockModel, t float64, r *rand.Rand) (timestamp, bool) {
	c.advanceTo(t + 0.1)
	return c.stamp(t, r)
}

func statusAt(c *clockModel, t float64) [14]byte {
	c.advanceTo(t + 0.1)
	return c.status(t)
}
