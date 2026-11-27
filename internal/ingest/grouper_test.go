package ingest

import (
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"airview/internal/feed"
	"airview/internal/ingest/beastin"
	"airview/internal/model"
	"airview/internal/tracker"
)

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC).UnixNano()

// df11 returns a DF11 payload for addr (CRC not computed: the fake handler
// does not decode).
func df11(addr uint32) []byte {
	return []byte{0x5D, byte(addr >> 16), byte(addr >> 8), byte(addr), 0x11, 0x22, 0x33}
}

// df17pos returns an airborne-position DF17 payload (TC 11) varying with n.
func df17pos(addr uint32, n int) []byte {
	return []byte{0x8D, byte(addr >> 16), byte(addr >> 8), byte(addr), 0x58, byte(n), byte(n >> 8), 1, 2, 3, 4, 5, 6, 7}
}

// tframe is a timed 12 MHz frame of station st heard at host time h (ns)
// whose ticks follow host time exactly.
func tframe(st model.StationIdx, ch int, h int64, data []byte) model.RawFrame {
	kind := model.FrameModeSShort
	if len(data) == 14 {
		kind = model.FrameModeSLong
	}
	return model.RawFrame{Station: st, Channel: ch, HostNs: h, Kind: kind, Data: data, Signal: -20,
		ClockCls: model.ClockHW12, Epoch: 1, Ticks: uint64((h - t0 + int64(time.Hour)) * 12 / 1000), GPSNs: -1}
}

type tester struct {
	t    *testing.T
	clk  *vclock
	h    *countHandler
	ing  *Ingest
	sink *groupSink
}

func newTester(t *testing.T, mut func(*Options)) *tester {
	clk := &vclock{}
	clk.set(t0)
	h := newCountHandler()
	sink := &groupSink{}
	ing := newTestIngest(t, h, clk, func(o *Options) {
		o.MLAT = sink
		if mut != nil {
			mut(o)
		}
	})
	return &tester{t: t, clk: clk, h: h, ing: ing, sink: sink}
}

// send delivers f at server time f.HostNs + 20 ms.
func (x *tester) send(f model.RawFrame, mapOnly bool) {
	at := f.HostNs + int64(20*time.Millisecond)
	if at > x.clk.ns.Load() {
		x.clk.set(at)
	}
	x.ing.frame(&f, mapOnly, x.clk.ns.Load())
}

// warm sends unique position squitters from every station at the same
// instants so the stations calibrate (n rounds, 100 ms apart).
func (x *tester) warm(sts []model.StationIdx, rounds int) {
	for i := 0; i < rounds; i++ {
		h := t0 + int64(i)*int64(100*time.Millisecond)
		for _, st := range sts {
			x.send(tframe(st, 0, h, df17pos(0xABCDEF, i)), false)
		}
		x.sweepTo(h)
	}
}

func (x *tester) sweepTo(h int64) {
	if h > x.clk.ns.Load() {
		x.clk.set(h)
	}
	x.ing.Sweep(time.Unix(0, x.clk.ns.Load()))
}

func (x *tester) finish() {
	x.sweepTo(x.clk.ns.Load() + int64(10*time.Second))
	x.ing.Flush()
}

func TestRepeatRule(t *testing.T) {
	x := newTester(t, nil)
	x.warm([]model.StationIdx{2, 3, 4}, 30)
	base := x.h.n.Load()
	h := t0 + int64(10*time.Second)
	d := df11(0x4840D6)
	for k := 0; k < 5; k++ { // identical DF11 every 0.8 s
		hk := h + int64(k)*int64(800*time.Millisecond)
		x.send(tframe(2, 0, hk, d), false)
		x.send(tframe(3, 0, hk+int64(time.Millisecond), d), false)
		if k%2 == 0 { // station 4 hears only every other one
			x.send(tframe(4, 0, hk+int64(2*time.Millisecond), d), false)
		}
		x.sweepTo(hk + int64(100*time.Millisecond))
	}
	x.finish()
	if got := x.h.n.Load() - base; got != 5 {
		t.Fatalf("5 identical DF11 0.8 s apart gave %d unique transmissions", got)
	}
	st := x.ing.Stats()
	if st.Dups != 0 || st.Same != 0 {
		t.Errorf("dups %d same %d", st.Dups, st.Same)
	}
}

func TestOtherChannelAndDuplicates(t *testing.T) {
	x := newTester(t, nil)
	x.warm([]model.StationIdx{2, 3}, 30)
	base := x.h.n.Load()
	h := t0 + int64(10*time.Second)
	d := df11(0x4840D6)
	f := tframe(2, 0, h, d)
	x.send(f, false)
	f1 := f
	f1.Channel = 1
	f1.Ticks += 10 // < 1 µs later on the other antenna
	x.send(f1, false)
	x.send(f, false) // exact resend
	x.send(tframe(3, 0, h+int64(time.Millisecond), d), false)
	x.finish()
	if got := x.h.n.Load() - base; got != 1 {
		t.Fatalf("got %d unique transmissions, want 1", got)
	}
	st := x.ing.Stats()
	if st.Same != 1 || st.Dups != 1 {
		t.Errorf("same %d dups %d, want 1 and 1", st.Same, st.Dups)
	}
	var g *Group
	for _, gg := range x.sink.groups {
		if gg.DF == 11 {
			g = gg
		}
	}
	if g == nil || len(g.Copies) != 3 || g.Timed != 2 {
		t.Fatalf("group %+v", g)
	}
	for _, c := range g.Copies {
		if c.SameStation && (c.Timed || c.Channel != 1) {
			t.Errorf("second-channel copy %+v", c)
		}
	}
}

func TestUntimedRepeatByChannel(t *testing.T) {
	x := newTester(t, nil)
	h := t0
	d := df11(0x112233)
	f := tframe(2, -1, h, d)
	f.ClockCls, f.Ticks = model.ClockNone, 0
	x.send(f, false)
	f2 := f
	f2.HostNs += int64(400 * time.Microsecond) // same channel: a new transmission
	x.send(f2, false)
	f3 := f
	f3.Channel = 1 // other channel within 1 ms: the same transmission
	x.send(f3, false)
	x.finish()
	if got := x.h.n.Load(); got != 2 {
		t.Fatalf("got %d unique transmissions, want 2", got)
	}
}

func TestMLATEligibility(t *testing.T) {
	res := newFakeRes()
	res.set(5, model.AdmPending)
	res.set(6, model.AdmRevoked)
	x := newTester(t, func(o *Options) { o.Resolver = res })
	x.warm([]model.StationIdx{2, 3, 4}, 30)
	x.ing.SetTimestamps(4, false)
	x.sink.groups = nil
	h := t0 + int64(10*time.Second)
	// A: station 2 timed + station 3 Beast map-only -> 1 timed: not delivered
	a := df11(0xA00001)
	x.send(tframe(2, 0, h, a), false)
	x.send(tframe(3, 0, h, a), true)
	// B: station 2 + station 4 (timestamps off) -> not delivered
	b := df11(0xA00002)
	x.send(tframe(2, 0, h+1e7, b), false)
	x.send(tframe(4, 0, h+1e7, b), false)
	// C: stations 2 and 3 timed -> delivered
	c := df11(0xA00003)
	x.send(tframe(2, 0, h+2e7, c), false)
	x.send(tframe(3, 0, h+2e7, c), false)
	// pending and revoked stations
	n0 := x.h.n.Load()
	x.send(tframe(5, 0, h+3e7, df11(0xA00004)), false)
	x.send(tframe(6, 0, h+3e7, df11(0xA00005)), false)
	if x.h.n.Load() != n0 {
		t.Error("pending/revoked frames reached the tracker")
	}
	x.finish()
	keys := map[uint32]bool{}
	for _, g := range x.sink.groups {
		keys[g.Key] = true
	}
	if keys[0xA00001] || keys[0xA00002] || !keys[0xA00003] {
		t.Errorf("delivered keys %v", keys)
	}
	st := x.ing.Stats()
	if st.Pending != 1 || st.Refused != 1 {
		t.Errorf("pending %d refused %d", st.Pending, st.Refused)
	}
}

type fakeClock struct {
	off  map[model.StationIdx]float64 // network time = ticks/12 MHz in ns + off
	cand bool
}

func (c *fakeClock) NetTime(st model.StationIdx, _ uint32, ticks uint64) (float64, float64, bool) {
	o, ok := c.off[st]
	return float64(ticks)*1000/12 + o, 50, ok
}
func (c *fakeClock) BaselineNs(a, b model.StationIdx) (float64, bool) { return 100e3, true }
func (c *fakeClock) SyncCandidate(uint32, uint8) bool                 { return c.cand }

func TestClockViewRules(t *testing.T) {
	cv := &fakeClock{off: map[model.StationIdx]float64{2: 0, 3: 0}, cand: true}
	x := newTester(t, func(o *Options) { o.Clock = cv })
	x.warm([]model.StationIdx{2, 3}, 30)
	x.sink.groups = nil
	base := x.h.n.Load()
	h := t0 + int64(10*time.Second)
	// one timed copy of a sync candidate heard by a synced station: delivered
	x.send(tframe(2, 0, h, df11(0xB00001)), false)
	// station 3's copy is 2 ms off in network time (> 100 µs + 5σ): not the
	// same transmission
	f := tframe(3, 0, h, df11(0xB00002))
	x.send(tframe(2, 0, h, df11(0xB00002)), false)
	f.Ticks += 24000 // 2 ms
	x.send(f, false)
	x.finish()
	if got := x.h.n.Load() - base; got != 3 {
		t.Errorf("unique %d, want 3 (the 2 ms copy must open its own group)", got)
	}
	found := false
	for _, g := range x.sink.groups {
		if g.Key == 0xB00001 && g.Timed == 1 {
			found = true
		}
	}
	if !found {
		t.Error("single-copy sync candidate not delivered")
	}
}

func TestLateTooOldAndBypass(t *testing.T) {
	x := newTester(t, nil)
	x.clk.set(t0 + int64(time.Minute))
	live := tframe(2, 0, t0+int64(time.Minute)-int64(20*time.Millisecond), df17pos(9, 9))
	x.ing.frame(&live, false, x.clk.ns.Load()) // establishes the station's host offset
	f := tframe(2, 0, t0+int64(time.Minute)-int64(10*time.Second), df11(1))
	x.ing.frame(&f, false, x.clk.ns.Load()) // too old: lanes only
	g := tframe(2, 0, t0+int64(time.Minute), df11(2))
	g.Late = true
	x.ing.frame(&g, false, x.clk.ns.Load())
	m := tframe(2, 0, t0+int64(time.Minute), df11(3))
	m.MLAT = true
	x.ing.frame(&m, false, x.clk.ns.Load())
	st := x.ing.Stats()
	if st.TooOld != 1 || st.Late != 1 || st.Bypass != 1 || x.h.late.Load() != 2 || x.h.n.Load() != 2 {
		t.Errorf("stats %+v handler late %d n %d", st, x.h.late.Load(), x.h.n.Load())
	}
}

func TestLateDeliveryAndUncalibratedStation(t *testing.T) {
	x := newTester(t, nil)
	// stations 2 and 3 deliver after 20 ms, station 4 after 600 ms: it
	// learns its latency from shared position squitters
	slow := int64(600 * time.Millisecond)
	for i := 0; i < 40; i++ {
		h := t0 + int64(i)*int64(100*time.Millisecond)
		p := df17pos(0x654321, i)
		x.send(tframe(2, 0, h, p), false)
		x.send(tframe(3, 0, h, p), false)
		x.sweepTo(h + int64(25*time.Millisecond))
		f := tframe(4, 0, h, p)
		x.ing.frame(&f, false, max(x.clk.ns.Load(), h+slow))
	}
	x.sweepTo(t0 + int64(8*time.Second))
	x.sink.groups, x.sink.late = nil, nil
	h := t0 + int64(10*time.Second)
	d := df11(0xC00001)
	x.send(tframe(2, 0, h, d), false)
	x.send(tframe(3, 0, h, d), false)
	x.sweepTo(h + int64(700*time.Millisecond)) // flushed at +400 ms (corrected)
	// station 4 is slow: its copy arrives after the flush, inside the window
	f := tframe(4, 0, h, d)
	x.ing.frame(&f, false, h+slow)
	if ti, _ := x.ing.Timing(4); ti.LatencyNs < int64(500*time.Millisecond) {
		t.Errorf("slow station's latency %v not learned", time.Duration(ti.LatencyNs))
	}
	x.finish()
	if len(x.sink.late) != 1 || len(x.sink.late[0].Copies) != 1 || x.sink.late[0].Earlier != 2 {
		t.Fatalf("late deliveries %+v", x.sink.late)
	}
}

func TestUniquePayloadJoinsFarOffStation(t *testing.T) {
	x := newTester(t, nil)
	// station 9's host clock is 2 s off and it has no history: its position
	// squitters still join (unique payload), and it calibrates.
	for i := 0; i < 40; i++ {
		h := t0 + int64(i)*int64(100*time.Millisecond)
		p := df17pos(0x123456, i)
		x.send(tframe(2, 0, h, p), false)
		x.send(tframe(3, 0, h, p), false)
		f := tframe(9, 0, h+int64(2*time.Second), p)
		f.Ticks = uint64(h-t0) * 12 / 1000
		x.ing.frame(&f, false, h+int64(30*time.Millisecond)) // arrives on time: only its clock is off
		x.sweepTo(h)
	}
	x.finish()
	if got := x.h.n.Load(); got != 40 {
		t.Errorf("unique %d, want 40", got)
	}
}

func TestHindexRandom(t *testing.T) {
	var h hindex
	h.init(16)
	ref := map[uint64]int32{}
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200000; i++ {
		k := r.Uint64() % 5000
		if k%7 == 0 {
			k |= 0xFFFF << 48 // clustered high bits, low bits collide
		}
		switch r.IntN(3) {
		case 0, 1:
			v := int32(r.IntN(1 << 20))
			h.set(k, v)
			ref[k] = v
		default:
			h.del(k)
			delete(ref, k)
		}
		if i%1000 == 0 {
			for rk, rv := range ref {
				if got := h.get(rk); got != rv {
					t.Fatalf("step %d: get(%x)=%d want %d", i, rk, got, rv)
				}
			}
			if h.n != len(ref) {
				t.Fatalf("n %d want %d", h.n, len(ref))
			}
		}
	}
}

func TestEnvelopeSDRBlocks(t *testing.T) {
	var e envelope
	r := rand.New(rand.NewPCG(3, 4))
	const block = 54.6e6
	maxErr := 0.0
	ppm := 80e-6
	for i := 0; i < 20000; i++ {
		tt := float64(i) * 1e6 * (1 + r.Float64()) // true time, ns
		host := int64(math.Ceil(tt/block)*block + 5e6 + r.ExpFloat64()*3e6)
		ticks := uint64(tt * (1 + ppm) * 12 / 1000)
		m, ok := e.update(1, model.ClockSDR12, ticks, int64(t0)+host)
		if !ok || i < 200 {
			continue
		}
		// the mapped time must equal the true time + the minimum delay
		err := math.Abs(float64(m-int64(t0)) - (tt + 5e6))
		maxErr = math.Max(maxErr, err)
	}
	if maxErr > 3e6 {
		t.Errorf("mapping error %.2f ms > 3 ms", maxErr/1e6)
	}
	// a forward step of the ticks resets the mapping
	if _, ok := e.update(1, model.ClockSDR12, e.baseTicks+1<<40, e.baseHost+1e9); ok {
		t.Error("a 25 h tick step kept the mapping")
	}
	// a new epoch restarts
	if _, ok := e.update(2, model.ClockSDR12, 5, int64(t0)); ok {
		t.Error("new epoch kept the mapping")
	}
}

func TestBudgetPriorities(t *testing.T) {
	clk := &vclock{}
	clk.set(t0)
	b := NewBudget(1000, clk.Now)
	if !b.TakePrio(600, PrioSheddable) || b.TakePrio(200, PrioSheddable) {
		t.Fatal("sheddable must stop at 70 %")
	}
	if !b.TakePrio(250, PrioNormal) || b.TakePrio(100, PrioNormal) {
		t.Fatal("normal must stop at 90 %")
	}
	if !b.TakePrio(100, PrioEssential) || b.TakePrio(100, PrioEssential) {
		t.Fatal("essential must stop at 100 %")
	}
	clk.set(t0 + int64(time.Second))
	if !b.Take(500) {
		t.Fatal("budget did not refill")
	}
	s := b.Stats()
	if s.Shed != [3]uint64{200, 100, 100} {
		t.Errorf("shed %v", s.Shed)
	}
	var u Budget
	if !u.TakePrio(1e9, PrioSheddable) {
		t.Error("zero budget must be unlimited")
	}
}

type fakeObs struct {
	mu     sync.Mutex
	events []ClockEvent
	pairs  int
	status int
}

func (o *fakeObs) ClockEvent(st model.StationIdx, ev ClockEvent) {
	o.mu.Lock()
	o.events = append(o.events, ev)
	o.mu.Unlock()
}
func (o *fakeObs) ClockPair(model.StationIdx, ClockPair)     { o.mu.Lock(); o.pairs++; o.mu.Unlock() }
func (o *fakeObs) ClockStatus(model.StationIdx, ClockStatus) { o.mu.Lock(); o.status++; o.mu.Unlock() }

func TestHandleBeast(t *testing.T) {
	obs := &fakeObs{}
	x := newTester(t, func(o *Options) { o.ClockObserver = obs })
	x.clk.set(t0)
	b := &beastin.Batch{MapStatsOnly: true}
	for i := 0; i < 3; i++ {
		f := tframe(7, -1, t0+int64(i)*1e6, df11(uint32(0xD00000+i)))
		f.Recv = time.Unix(0, t0)
		b.Frames = append(b.Frames, f)
	}
	b.Events = []beastin.ClockEvent{{At: 1, Station: 7, Event: feed.ClockEvent{OldEpoch: 1, NewEpoch: 2, Reason: model.ClockEvJump}}}
	x.ing.HandleBeast(b)
	x.ing.ClockPair(7, ClockPair{})
	x.ing.ClockStatus(7, ClockStatus{})
	if x.h.n.Load() != 3 || len(obs.events) != 1 || !obs.events[0].MapOnly || obs.events[0].NewEpoch != 2 || obs.pairs != 1 || obs.status != 1 {
		t.Fatalf("handled %d, events %+v pairs %d status %d", x.h.n.Load(), obs.events, obs.pairs, obs.status)
	}
	x.finish()
	for _, g := range x.sink.groups {
		t.Errorf("map-only frames reached MLAT: %+v", g)
	}
}

func TestUpdatesRouting(t *testing.T) {
	res := newFakeRes()
	res.set(5, model.AdmPending)
	x := newTester(t, func(o *Options) { o.Resolver = res })
	x.ing.HandleUpdate(model.DecodedUpdate{ICAO: 1, Recv: time.Unix(0, t0)})                       // host station
	x.ing.HandleUpdate(model.DecodedUpdate{ICAO: 1, Recv: time.Unix(0, t0), Src: model.SrcMLATAV}) // own MLAT
	x.ing.HandleUpdate(model.DecodedUpdate{ICAO: 1, Recv: time.Unix(0, t0), Station: 5})           // pending
	if x.h.updates.Load() != 2 || x.ing.Stats().Pending != 1 {
		t.Errorf("updates %d pending %d", x.h.updates.Load(), x.ing.Stats().Pending)
	}
}

// C1: the grouper credits a same-station copy to its channel only: the
// station's own counters come from the first copy (Tracker.Handle).
func TestSameStationCopyChannelCredit(t *testing.T) {
	x := newTester(t, nil)
	x.h.lanes = tracker.NewLanes(tracker.LaneOptions{})
	h := t0 + int64(10*time.Second)
	for i := range 10 {
		d := df11(0x4840D0 + uint32(i))
		f := tframe(2, 0, h+int64(i)*int64(time.Second), d)
		x.send(f, false)
		f1 := f
		f1.Channel = 1
		f1.Ticks += 10
		x.send(f1, false)
	}
	x.finish()
	if st := x.ing.Stats(); st.Same != 10 {
		t.Fatalf("same %d", st.Same)
	}
	// countHandler.Handle credits nothing itself, so any station frames
	// here would be double counts from the grouper.
	if info, ok := x.h.lanes.Info(2); ok && info.Frames != 0 {
		t.Fatalf("diversity copies counted as station frames: %+v", info)
	}
}

// C2: a two-station group completed after the flush but inside the window
// goes to MLAT on close.
func TestSecondCopyAfterFlushDelivered(t *testing.T) {
	x := newTester(t, nil)
	slow := int64(600 * time.Millisecond)
	for i := 0; i < 40; i++ { // station 4 learns its 600 ms latency
		h := t0 + int64(i)*int64(100*time.Millisecond)
		p := df17pos(0x654321, i)
		x.send(tframe(2, 0, h, p), false)
		x.sweepTo(h + int64(25*time.Millisecond))
		f := tframe(4, 0, h, p)
		x.ing.frame(&f, false, max(x.clk.ns.Load(), h+slow))
	}
	x.sweepTo(t0 + int64(8*time.Second))
	x.sink.groups, x.sink.late = nil, nil
	h := t0 + int64(10*time.Second)
	d := df11(0xC00002)
	x.send(tframe(2, 0, h, d), false)
	x.sweepTo(h + int64(500*time.Millisecond)) // flushed with one timed station
	f := tframe(4, 0, h, d)
	x.ing.frame(&f, false, h+int64(620*time.Millisecond))
	x.finish()
	n := 0
	for _, g := range x.sink.groups {
		if g.DF == 11 {
			n++
			if len(g.Copies) != 2 || g.Timed != 2 {
				t.Errorf("group %+v", g)
			}
		}
	}
	if n != 1 {
		t.Fatalf("DF11 group delivered %d times, want once (late: %d)", n, len(x.sink.late))
	}
}

// blockHandler holds Handle until release is closed (the result of a
// group stays pending meanwhile).
type blockHandler struct {
	*countHandler
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockHandler) Handle(f model.RawFrame) tracker.Result {
	h.once.Do(func() { close(h.entered) })
	<-h.release
	return h.countHandler.Handle(f)
}

// SO-10: copies beyond MaxCopies that arrive while the result is pending
// are still credited to their stations.
func TestOverflowCopiesCredited(t *testing.T) {
	clk := &vclock{}
	clk.set(t0 + int64(10*time.Second))
	ch := newCountHandler()
	ch.lanes = tracker.NewLanes(tracker.LaneOptions{})
	h := &blockHandler{countHandler: ch, entered: make(chan struct{}), release: make(chan struct{})}
	ing := newTestIngest(t, h, clk, nil)
	hk := t0 + int64(10*time.Second)
	d := df17pos(0x4840D6, 1)
	const k = 100
	first := tframe(2, 0, hk, d)
	done := make(chan struct{})
	go func() { ing.frame(&first, false, hk+int64(20*time.Millisecond)); close(done) }()
	<-h.entered
	for s := 1; s < k; s++ {
		f := tframe(model.StationIdx(2+s), 0, hk, d)
		ing.frame(&f, false, hk+int64(20*time.Millisecond))
	}
	close(h.release)
	<-done
	credited := 0
	for s := 1; s < k; s++ {
		if info, ok := ch.lanes.Info(model.StationIdx(2 + s)); ok && info.Frames == 1 {
			credited++
		}
	}
	if credited != k-1 {
		t.Fatalf("%d of %d joined copies credited", credited, k-1)
	}
}
