package ingest

import (
	"sync"
	"testing"
	"time"

	"airview/internal/model"
)

// fakePolicy is a FlushPolicy with fixed delays per aircraft that records
// what the ingest teaches it.
type fakePolicy struct {
	mu      sync.Mutex
	delay   map[uint32]time.Duration
	delays  map[model.StationIdx][]int64
	hearers map[uint32]map[model.StationIdx]bool
}

func newFakePolicy() *fakePolicy {
	return &fakePolicy{delay: map[uint32]time.Duration{}, delays: map[model.StationIdx][]int64{},
		hearers: map[uint32]map[model.StationIdx]bool{}}
}

func (p *fakePolicy) Delay(key uint32) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.delay[key]
}

func (p *fakePolicy) ObserveDelay(st model.StationIdx, d, _ int64) {
	p.mu.Lock()
	p.delays[st] = append(p.delays[st], d)
	p.mu.Unlock()
}

func (p *fakePolicy) NoteHearer(key uint32, st model.StationIdx, _ int64) {
	p.mu.Lock()
	if p.hearers[key] == nil {
		p.hearers[key] = map[model.StationIdx]bool{}
	}
	p.hearers[key][st] = true
	p.mu.Unlock()
}

// groupsOf returns the deliveries of aircraft key.
func (x *tester) groupsOf(key uint32) []*Group {
	var out []*Group
	for _, g := range x.sink.groups {
		if g.Key == key {
			out = append(out, g)
		}
	}
	return out
}

func (x *tester) delivered(key uint32) bool {
	for _, g := range x.sink.groups {
		if g.Key == key {
			return true
		}
	}
	return false
}

// TestFlushPolicyDelays: with a FlushPolicy each aircraft's groups flush
// after its own delay (clamped to 100 ms..Window), the policy learns the
// copies' delivery latency and hearers, and without one the fixed
// FlushAfter applies.
func TestFlushPolicyDelays(t *testing.T) {
	p := newFakePolicy()
	p.delay[0xD00001] = 150 * time.Millisecond
	p.delay[0xD00002] = 600 * time.Millisecond
	p.delay[0xD00003] = 20 * time.Millisecond // clamped to 100 ms
	x := newTester(t, func(o *Options) { o.Flush, o.FlushSampleEvery = p, 1 })
	x.warm([]model.StationIdx{2, 3}, 30)
	x.sink.groups = nil
	h := t0 + int64(10*time.Second)
	for _, k := range []uint32{0xD00001, 0xD00002, 0xD00003, 0xD00004} {
		x.send(tframe(2, 0, h, df11(k)), false)
		x.send(tframe(3, 0, h, df11(k)), false)
	}
	x.sweepTo(h + int64(90*time.Millisecond))
	if x.delivered(0xD00003) {
		t.Fatal("delay below 100 ms not clamped")
	}
	x.sweepTo(h + int64(200*time.Millisecond))
	if !x.delivered(0xD00001) || !x.delivered(0xD00003) || x.delivered(0xD00002) || x.delivered(0xD00004) {
		t.Fatalf("at +200 ms: delivered %v %v %v %v", x.delivered(0xD00001), x.delivered(0xD00002), x.delivered(0xD00003), x.delivered(0xD00004))
	}
	x.sweepTo(h + int64(450*time.Millisecond))
	if !x.delivered(0xD00004) || x.delivered(0xD00002) {
		t.Fatal("unknown aircraft must use FlushAfter (400 ms), the 600 ms one wait")
	}
	x.sweepTo(h + int64(650*time.Millisecond))
	if !x.delivered(0xD00002) {
		t.Fatal("600 ms aircraft not delivered")
	}
	x.finish()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, st := range []model.StationIdx{2, 3} {
		ds := p.delays[st]
		if len(ds) == 0 {
			t.Fatalf("station %d: no delivery latency learned", st)
		}
		for _, d := range ds {
			if d < 0 || d > int64(100*time.Millisecond) {
				t.Fatalf("station %d: delivery latency %v (copies arrive 20 ms after reception)", st, time.Duration(d))
			}
		}
	}
	if h := p.hearers[0xD00002]; !h[2] || !h[3] {
		t.Errorf("hearers of D00002: %v", h)
	}
}

// TestFlushSampling: FlushSampleEvery trains the policy with a fraction of
// the groups.
func TestFlushSampling(t *testing.T) {
	count := func(every int) int {
		p := newFakePolicy()
		x := newTester(t, func(o *Options) { o.Flush, o.FlushSampleEvery = p, every })
		for i := 0; i < 400; i++ {
			h := t0 + int64(i)*int64(10*time.Millisecond)
			x.send(tframe(2, 0, h, df11(uint32(0xE00000+i))), false)
		}
		x.finish()
		return len(p.delays[2])
	}
	all, quarter := count(1), count(4)
	if all != 400 || quarter < 60 || quarter > 140 {
		t.Fatalf("sampled %d of 400 (every 1) and %d (every 4)", all, quarter)
	}
}

// kindClock is a ClockKindView: network time = ticks/12 MHz + off, with a
// per-kind offset and a small σ; it records the kinds it was asked for.
type kindClock struct {
	fakeClock
	mu    sync.Mutex
	kinds map[model.FrameKind]int
}

func (c *kindClock) NetTimeKind(st model.StationIdx, ep uint32, ticks uint64, kind model.FrameKind) (float64, float64, bool) {
	c.mu.Lock()
	c.kinds[kind]++
	c.mu.Unlock()
	t, _, ok := c.fakeClock.NetTime(st, ep, ticks)
	return t, 10, ok
}

// TestClockGateFloorAndKind: the synced-station gate uses the frame kind
// (ClockKindView) and never separates copies closer than d/c +
// ClockGateFloor, so a clock that drifted by tens of µs keeps contributing
// its copies; 2 ms apart is still another transmission.
func TestClockGateFloorAndKind(t *testing.T) {
	run := func(floor time.Duration, offNs float64) (unique uint64, kc *kindClock) {
		kc = &kindClock{fakeClock: fakeClock{off: map[model.StationIdx]float64{2: 0, 3: 0}}, kinds: map[model.FrameKind]int{}}
		x := newTester(t, func(o *Options) { o.Clock, o.ClockGateFloor = kc, floor })
		x.warm([]model.StationIdx{2, 3}, 30)
		base := x.h.n.Load()
		h := t0 + int64(10*time.Second)
		f := tframe(3, 0, h, df11(0xF00001))
		x.send(tframe(2, 0, h, df11(0xF00001)), false)
		kc.off[3] = offNs // station 3's clock is off by offNs in network time
		x.send(f, false)
		x.finish()
		return x.h.n.Load() - base, kc
	}
	// baseline 100 µs, σ 10 ns: 130 µs off is outside d/c + 5σ
	if u, _ := run(-1, 130e3); u != 2 {
		t.Errorf("no floor: 130 µs off gave %d groups, want 2", u)
	}
	u, kc := run(0, 130e3) // default floor 50 µs
	if u != 1 {
		t.Errorf("50 µs floor: 130 µs off gave %d groups, want 1", u)
	}
	if kc.kinds[model.FrameModeSShort] == 0 {
		t.Errorf("gate asked for kinds %v, want the DF11 short kind among them", kc.kinds)
	}
	if u, _ := run(0, 2e6); u != 2 {
		t.Errorf("2 ms off gave %d groups, want 2 (another transmission)", u)
	}
}

// TestRedeliveryOfSingleCopyFlush (HOOKS WP-M-engine -> internal/ingest
// item 2): a sync candidate flushed with one synced copy whose other
// stations' copies arrive later is delivered once more with every copy,
// marked Redelivery, instead of a late delivery of the new copies only.
func TestRedeliveryOfSingleCopyFlush(t *testing.T) {
	cv := &fakeClock{off: map[model.StationIdx]float64{2: 0, 3: 0, 4: 0}, cand: true}
	x := newTester(t, func(o *Options) { o.Clock = cv })
	x.warm([]model.StationIdx{2, 3, 4}, 30)
	x.sink.groups, x.sink.late = nil, nil
	h := t0 + int64(10*time.Second)
	d := df11(0xA10001)
	x.send(tframe(2, 0, h, d), false)
	x.sweepTo(h + int64(450*time.Millisecond)) // flushed with station 2 alone
	if gs := x.groupsOf(0xA10001); len(gs) != 1 || gs[0].Timed != 1 || gs[0].Redelivery {
		t.Fatalf("first delivery %+v", gs)
	}
	for _, st := range []model.StationIdx{3, 4} {
		f := tframe(st, 0, h, d)
		x.ing.frame(&f, false, h+int64(600*time.Millisecond))
	}
	x.finish()
	if len(x.sink.late) != 0 {
		t.Errorf("late deliveries %d, want 0", len(x.sink.late))
	}
	gs := x.groupsOf(0xA10001)
	if len(gs) != 2 {
		t.Fatalf("%d deliveries, want 2", len(gs))
	}
	g := gs[1]
	if !g.Redelivery || g.Timed != 3 || len(g.Copies) != 3 || g.Key != 0xA10001 {
		t.Fatalf("redelivery %+v", g)
	}
	if st := x.ing.Stats(); st.Redelivered != 1 {
		t.Errorf("stats redelivered %d", st.Redelivered)
	}
	// a normally flushed group keeps the late delivery
	x.sink.groups, x.sink.late = nil, nil
	h = x.clk.ns.Load() + int64(time.Second)
	d = df11(0xA10002)
	x.send(tframe(2, 0, h, d), false)
	x.send(tframe(3, 0, h, d), false)
	x.sweepTo(h + int64(450*time.Millisecond))
	f := tframe(4, 0, h, d)
	x.ing.frame(&f, false, h+int64(600*time.Millisecond))
	x.finish()
	if gs := x.groupsOf(0xA10002); len(x.sink.late) != 1 || len(gs) != 1 || gs[0].Redelivery {
		t.Fatalf("two-copy flush: groups %d late %d", len(x.sink.groups), len(x.sink.late))
	}
}
