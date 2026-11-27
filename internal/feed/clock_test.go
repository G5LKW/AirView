package feed

import (
	"testing"
	"time"

	"airview/internal/model"
)

var clockT0 = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC).UnixNano()

// evLog collects clock events.
type evLog struct{ ev []ClockEvent }

func (l *evLog) add(e ClockEvent) { l.ev = append(l.ev, e) }

func newTracker(cls model.ClockClass, l *evLog) *ClockTracker {
	c := &ClockTracker{OnEvent: l.add}
	c.SetClass(cls, "")
	return c
}

func TestClockTracker12MHzWrap(t *testing.T) {
	var l evLog
	c := newTracker(model.ClockHW12, &l)
	const mod = uint64(1) << 48
	start := mod - 12_000_000 // one second before the 48-bit wrap
	for k := uint64(0); k < 12; k++ {
		raw := (start + k*3_000_000) % mod
		host := clockT0 + int64(k)*250e6
		ticks, ok := c.Stamp(raw, host)
		if !ok || ticks != start+k*3_000_000 {
			t.Fatalf("frame %d: ticks %d ok %v, want %d", k, ticks, ok, start+k*3_000_000)
		}
	}
	if c.LastTicks() <= mod {
		t.Errorf("ticks did not unwrap past 2^48: %d", c.LastTicks())
	}
	if len(l.ev) != 1 || l.ev[0].Reason != model.ClockEvRestart || l.ev[0].Detail != ClockDetailStart ||
		l.ev[0].NewEpoch == 0 || l.ev[0].OldEpoch != 0 || l.ev[0].NewClass != model.ClockHW12 || l.ev[0].NewEpoch != c.Epoch() {
		t.Errorf("events = %+v", l.ev)
	}
}

func TestClockTrackerCounterResetIsBackwards(t *testing.T) {
	var l evLog
	c := newTracker(model.ClockSDR12, &l)
	base := uint64(1e12)
	for k := 0; k < 5; k++ {
		if _, ok := c.Stamp(base+uint64(k)*12e6, clockT0+int64(k)*1e9); !ok {
			t.Fatal("normal frame rejected")
		}
	}
	first := c.Epoch()
	// The receiver restarted its counter.
	var got []bool
	var last uint64
	for k := 0; k < 3; k++ {
		ticks, ok := c.Stamp(5000+uint64(k)*12e6, clockT0+int64(5+k)*1e9)
		got = append(got, ok)
		last = ticks
	}
	if got[0] || got[1] || !got[2] {
		t.Fatalf("confirmation pattern %v, want [false false true]", got)
	}
	if last != 5000+2*12e6 {
		t.Errorf("first ticks of the new epoch = %d, want the raw counter", last)
	}
	if len(l.ev) != 2 {
		t.Fatalf("events = %+v", l.ev)
	}
	ev := l.ev[1]
	if ev.Reason != model.ClockEvBackwards || ev.Detail != ClockDetailBackwards || ev.OldEpoch != first ||
		ev.NewEpoch != first+1 || ev.StepNs >= 0 || ev.HostNs != clockT0+7e9 { // the confirming frame opens the epoch
		t.Errorf("event = %+v", ev)
	}
	if c.Backwards != 1 || c.Steps != 0 || c.Epochs != 2 {
		t.Errorf("counters backwards %d steps %d epochs %d", c.Backwards, c.Steps, c.Epochs)
	}
}

func TestClockTrackerGlitchAndSmallReorder(t *testing.T) {
	var l evLog
	c := newTracker(model.ClockGPSUTC, &l)
	ns := int64(43200e9)
	host := clockT0
	stamp := func(d, h int64) (uint64, bool) { return c.Stamp(uint64(ns+d), host+h) }
	t0, ok := stamp(0, 0)
	if !ok || t0 != uint64(clockT0) { // 12:00:00 UTC on the host's day
		t.Fatalf("first ticks %d ok %v, want %d", t0, ok, clockT0)
	}
	// One corrupt frame 5 hours ahead, then normal traffic again.
	if _, ok := stamp(5*3600e9, 1e6); ok {
		t.Error("lone step accepted")
	}
	if tk, ok := stamp(2e6, 2e6); !ok || tk != t0+2e6 {
		t.Errorf("normal frame after glitch: %d %v", tk, ok)
	}
	// 50 ms out of order: within tolerance, keeps the epoch.
	if tk, ok := stamp(-48e6, 3e6); !ok || tk != t0-48e6 {
		t.Errorf("reordered frame: %d %v", tk, ok)
	}
	if c.LastTicks() != t0+2e6 {
		t.Errorf("reference moved backwards: %d", c.LastTicks())
	}
	// 300 ms back: beyond tolerance, not confirmed (next frame is normal).
	if _, ok := stamp(-300e6, 4e6); ok {
		t.Error("300 ms backwards frame accepted")
	}
	if _, ok := stamp(5e6, 5e6); !ok {
		t.Error("normal frame rejected")
	}
	if len(l.ev) != 1 || c.Glitches != 2 {
		t.Errorf("events %d glitches %d", len(l.ev), c.Glitches)
	}
}

func TestClockTrackerForwardStep(t *testing.T) {
	var l evLog
	c := newTracker(model.ClockGPSUTC, &l)
	c.ConfirmFrames = 2
	ns := uint64(36000e9)
	c.Stamp(ns, clockT0)
	c.Stamp(ns+1e9, clockT0+1e9)
	// Receiver time jumps 120 s ahead of host time.
	if _, ok := c.Stamp(ns+122e9, clockT0+2e9); ok {
		t.Error("first frame of a step accepted")
	}
	tk, ok := c.Stamp(ns+123e9, clockT0+3e9)
	if !ok || tk != uint64(clockT0-7200e9+123e9) { // 10:00 is two hours before clockT0
		t.Fatalf("confirmed step ticks %d ok %v", tk, ok)
	}
	if len(l.ev) != 2 || l.ev[1].Reason != model.ClockEvJump || l.ev[1].Detail != ClockDetailStep ||
		l.ev[1].StepNs < 119e9 || l.ev[1].StepNs > 121e9 {
		t.Errorf("events = %+v", l.ev)
	}
	// 80 s ahead is within the 90 s limit.
	if _, ok := c.Stamp(ns+123e9+81e9, clockT0+4e9); !ok {
		t.Error("80 s forward step started an epoch")
	}
}

func TestClockTrackerSBS20(t *testing.T) {
	var l evLog
	c := newTracker(model.ClockSBS20, &l)
	const mask = 1<<24 - 1
	var k uint64
	for ; k < 60; k++ { // 6 s at 10 frames/s: several 0.84 s wraps
		tk, ok := c.Stamp((k*2_000_000)&mask, clockT0+int64(k)*100e6)
		if !ok || tk != k*2_000_000 {
			t.Fatalf("frame %d: %d %v", k, tk, ok)
		}
	}
	// A 3.3 s gap (several wraps) is bridged by host time.
	k += 33
	tk, ok := c.Stamp((k*2_000_000)&mask, clockT0+int64(k)*100e6+20e6)
	if !ok || tk != k*2_000_000 {
		t.Errorf("after gap: %d %v, want %d", tk, ok, k*2_000_000)
	}
	if len(l.ev) != 1 {
		t.Errorf("events = %+v", l.ev)
	}
}

func TestClockTrackerEpochIDsAndCauses(t *testing.T) {
	var l evLog
	c := &ClockTracker{OnEvent: l.add, ConfirmFrames: 1}
	c.SetEpochBase(^uint32(0)) // previous connection ended at the top of the range
	if _, ok := c.Stamp(1, clockT0); ok {
		t.Error("class none produced ticks")
	}
	c.SetClass(model.ClockHW12, ClockDetailClassChange)
	c.Stamp(100, clockT0)
	if len(l.ev) != 1 || l.ev[0].NewEpoch != 1 || l.ev[0].OldEpoch != ^uint32(0) ||
		l.ev[0].Reason != model.ClockEvRestart || l.ev[0].Detail != ClockDetailReconnect {
		t.Fatalf("reconnect event = %+v", l.ev)
	}
	c.SampleDrop(4096)
	c.Stamp(200, clockT0+1e6)
	if ev := l.ev[len(l.ev)-1]; ev.Reason != model.ClockEvSampleDrop || ev.SamplesLost != 4096 || ev.NewEpoch != 2 {
		t.Errorf("sample drop event = %+v", ev)
	}
	c.SetClass(model.ClockSDR12, ClockDetailOverride)
	c.Stamp(300, clockT0+2e6)
	if ev := l.ev[len(l.ev)-1]; ev.Reason != model.ClockEvModeChange || ev.Detail != ClockDetailOverride ||
		ev.OldClass != model.ClockHW12 || ev.NewClass != model.ClockSDR12 || ev.NewEpoch != 3 {
		t.Errorf("mode change event = %+v", ev)
	}
	c.Restart()
	c.Stamp(400, clockT0+3e6)
	if ev := l.ev[len(l.ev)-1]; ev.Reason != model.ClockEvRestart || ev.NewEpoch != 4 {
		t.Errorf("restart event = %+v", ev)
	}
	for _, ev := range l.ev {
		if !ev.Reason.Valid() || ev.NewEpoch == 0 || ev.NewEpoch == ev.OldEpoch {
			t.Errorf("bad event %+v", ev)
		}
	}
	// Random first epoch without a base.
	var l2 evLog
	c2 := newTracker(model.ClockHW12, &l2)
	c2.Stamp(1, clockT0)
	if l2.ev[0].NewEpoch == 0 {
		t.Error("epoch 0 issued")
	}
	// A zero base means no previous epoch: random start, reported as a start.
	var l3 evLog
	c3 := &ClockTracker{OnEvent: l3.add}
	c3.SetEpochBase(0)
	c3.SetClass(model.ClockHW12, "")
	c3.Stamp(1, clockT0)
	if l3.ev[0].Detail != ClockDetailStart || l3.ev[0].OldEpoch != 0 {
		t.Errorf("zero base event %+v", l3.ev[0])
	}
}

func TestClockTrackerNoAllocs(t *testing.T) {
	c := &ClockTracker{}
	c.SetClass(model.ClockGPSUTC, "")
	var k int64
	if a := testing.AllocsPerRun(1000, func() {
		k++
		c.Stamp(uint64(40000e9+k*1e6), clockT0+k*1e6)
	}); a != 0 {
		t.Errorf("Stamp allocates %v times", a)
	}
}

// --- Timestamper / decoder: GPS time base, day rollover, legacy day --------

// clockSink records frames and clock events in arrival order.
type clockSink struct {
	frames []model.RawFrame
	events []ClockEvent
	order  []byte // 'f' frame, 'e' event
}

func (s *clockSink) HandleFrame(f model.RawFrame) {
	s.frames = append(s.frames, f)
	s.order = append(s.order, 'f')
}
func (s *clockSink) HandleUpdate(model.DecodedUpdate) {}
func (s *clockSink) event(e ClockEvent) {
	s.events = append(s.events, e)
	s.order = append(s.order, 'e')
}

func newClockDecoder() (*BeastDecoder, *clockSink, *BeastParser) {
	out := &clockSink{}
	d := &BeastDecoder{Sink: out, FeedID: "rc"}
	d.TS.OnClockEvent = out.event
	return d, out, NewBeastParser(d.Handle)
}

// feedAt parses one frame received at host time h.
func feedAt(d *BeastDecoder, p *BeastParser, h time.Time, typ byte, ts uint64, payload []byte) {
	d.Recv = h
	p.Write(AppendBeastFrame(nil, typ, ts, 100, payload))
}

const (
	gpsUTCLocked  = 0x80 | 0x10 | 0x08 | 0x07 // true-UTC range, sync, UTC, antenna + sats
	gpsGPSTLocked = 0x80 | 0x10 | 0x07        // UTC-offset bit clear: GPS time base
	gpsBad        = 0x80 | 0x20 | 0x10 | 0x08 | 0x07
	gpsLegacy     = 0x10 | 0x08 | 0x07 // locked, but no "UTC bugfix" flag: day runs 1..86400
)

func TestTimestamperGPSDayRollover(t *testing.T) {
	d, out, p := newClockDecoder()
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	next := day.Add(24 * time.Hour)
	at := func(sec float64) time.Time { return day.Add(time.Duration(sec * 1e9)) }

	feedAt(d, p, at(86399.0), BeastStatusType, GPSTimestamp(86399, 0), statusPayload(0x10, 0, gpsUTCLocked))
	feedAt(d, p, at(86399.6), BeastModeSLong, GPSTimestamp(86399, 600e6), long1)
	feedAt(d, p, at(86400.1), BeastModeSLong, GPSTimestamp(0, 100e6), long1)
	// Host time lagging behind (still the old day): continuity keeps the new day.
	feedAt(d, p, at(86399.9), BeastModeSShort, GPSTimestamp(0, 200e6), short1)

	want := []int64{
		day.UnixNano() + 86399e9,
		day.UnixNano() + 86399e9 + 600e6,
		next.UnixNano() + 100e6,
		next.UnixNano() + 200e6,
	}
	if len(out.frames) != len(want) || len(out.events) != 1 {
		t.Fatalf("%d frames, %d events", len(out.frames), len(out.events))
	}
	for i, w := range want {
		f := out.frames[i]
		if f.ClockCls != model.ClockGPSUTC || f.Ticks != uint64(w) || f.Epoch != out.events[0].NewEpoch || f.HostNs != f.Recv.UnixNano() {
			t.Errorf("frame %d: cls %v ticks %d epoch %d host %d, want ticks %d", i, f.ClockCls, f.Ticks, f.Epoch, f.HostNs, w)
		}
	}
	if string(out.order[:2]) != "ef" {
		t.Errorf("event did not precede the first frame: %q", out.order)
	}
	if d.TS.Class() != model.ClockGPSUTC || d.TS.Epoch() != out.events[0].NewEpoch || d.TS.LastTicks() != uint64(want[3]) {
		t.Errorf("timestamper state: %v %d %d", d.TS.Class(), d.TS.Epoch(), d.TS.LastTicks())
	}
}

func TestTimestamperLegacyDayTicks(t *testing.T) {
	d, out, p := newClockDecoder()
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	at := func(sec float64) time.Time { return day.Add(time.Duration(sec * 1e9)) }

	// Legacy firmware: second-of-day runs 1..86400, i.e. one second ahead.
	feedAt(d, p, at(86398.0), BeastStatusType, GPSTimestamp(86399, 0), statusPayload(0x10, 0, gpsLegacy))
	feedAt(d, p, at(86399.6), BeastModeSLong, GPSTimestamp(86400, 600e6), long1) // true 23:59:59.6
	feedAt(d, p, at(86400.1), BeastModeSLong, GPSTimestamp(1, 100e6), long1)     // true 00:00:00.1 next day
	feedAt(d, p, at(86401.5), BeastModeSLong, GPSTimestamp(2, 500e6), long1)     // true 00:00:01.5

	want := []int64{
		day.UnixNano() + 86398e9,
		day.UnixNano() + 86399e9 + 600e6,
		day.UnixNano() + 86400e9 + 100e6,
		day.UnixNano() + 86401e9 + 500e6,
	}
	wantGPSNs := []int64{86398e9, 86399e9 + 600e6, 100e6, 1e9 + 500e6}
	if len(out.frames) != 4 || len(out.events) != 1 {
		t.Fatalf("%d frames %d events", len(out.frames), len(out.events))
	}
	for i, f := range out.frames {
		if f.Ticks != uint64(want[i]) || f.GPSNs != wantGPSNs[i] || f.ClockCls != model.ClockGPSUTC {
			t.Errorf("frame %d: ticks %d gpsNs %d cls %v, want %d %d", i, f.Ticks, f.GPSNs, f.ClockCls, want[i], wantGPSNs[i])
		}
	}

	// A firmware update to the true-UTC range moves every timestamp by one
	// second: new epoch.
	feedAt(d, p, at(86402.0), BeastStatusType, GPSTimestamp(2, 0), statusPayload(0x10, 0, gpsUTCLocked))
	if len(out.events) != 2 || out.events[1].Reason != model.ClockEvModeChange || out.events[1].Detail != ClockDetailDayRange {
		t.Errorf("events = %+v", out.events)
	}
}

func TestTimestamperGPSTimeBase(t *testing.T) {
	d, out, p := newClockDecoder()
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	next := day.Add(24 * time.Hour)
	host := day.Add(86390 * time.Second) // 23:59:50 UTC

	// GPS time runs 18 s ahead of UTC: 00:00:08 GPS time on the next day.
	feedAt(d, p, host, BeastStatusType, GPSTimestamp(8, 0), statusPayload(0x10, 0, gpsGPSTLocked))
	feedAt(d, p, host.Add(100*time.Millisecond), BeastModeSLong, GPSTimestamp(8, 100e6), long1)
	if len(out.frames) != 2 || out.frames[1].ClockCls != model.ClockGPSGPST || out.frames[1].Ticks != uint64(next.UnixNano()+8e9+100e6) {
		t.Fatalf("GPS-time frame: %+v", out.frames)
	}
	if d.TS.Class() != model.ClockGPSGPST {
		t.Errorf("class %v", d.TS.Class())
	}
	// The UTC offset arrives: the time base switches to UTC.
	feedAt(d, p, host.Add(time.Second), BeastStatusType, GPSTimestamp(86391, 0), statusPayload(0x10, 0, gpsUTCLocked))
	feedAt(d, p, host.Add(1100*time.Millisecond), BeastModeSLong, GPSTimestamp(86391, 100e6), long1)
	if len(out.events) != 2 {
		t.Fatalf("events = %+v", out.events)
	}
	ev := out.events[1]
	if ev.Reason != model.ClockEvModeChange || ev.Detail != ClockDetailTimeBase || ev.OldClass != model.ClockGPSGPST ||
		ev.NewClass != model.ClockGPSUTC || ev.OldEpoch != out.events[0].NewEpoch {
		t.Errorf("event = %+v", ev)
	}
	if f := out.frames[3]; f.ClockCls != model.ClockGPSUTC || f.Ticks != uint64(day.UnixNano()+86391e9+100e6) || f.Epoch != ev.NewEpoch {
		t.Errorf("UTC frame %+v", f)
	}
}

func TestTimestamperGPSLostAndRegained(t *testing.T) {
	d, out, p := newClockDecoder()
	host := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	sec := 36000
	step := func(gps byte) {
		feedAt(d, p, host, BeastStatusType, GPSTimestamp(sec, 0), statusPayload(0x10, 0, gps))
		feedAt(d, p, host.Add(500*time.Millisecond), BeastModeSShort, GPSTimestamp(sec, 500e6), short1)
		host = host.Add(time.Second)
		sec++
	}
	step(gpsUTCLocked)
	step(gpsBad) // lost: immediate
	step(gpsUTCLocked)
	step(gpsUTCLocked) // two good statuses: not yet trusted
	if d.TS.Class() != model.ClockGPSEmul {
		t.Errorf("class after 2 good statuses = %v", d.TS.Class())
	}
	step(gpsUTCLocked) // third: regained
	step(gpsUTCLocked)

	var got []string
	for _, e := range out.events {
		got = append(got, string(e.Reason)+"/"+e.Detail+"/"+e.NewClass.String())
	}
	want := []string{"restart/start/gps_utc", "mode_change/gps_lost/gps_emul", "mode_change/gps_regained/gps_utc"}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %s, want %s", i, got[i], want[i])
		}
	}
	// Every timed frame carries the epoch announced by the latest event before it.
	cur := uint32(0)
	ei, fi := 0, 0
	for _, o := range out.order {
		if o == 'e' {
			cur = out.events[ei].NewEpoch
			ei++
			continue
		}
		f := out.frames[fi]
		fi++
		if f.ClockCls.Timed() && f.Epoch != cur {
			t.Errorf("frame %d epoch %d, want %d", fi-1, f.Epoch, cur)
		}
	}
	// GPS ticks continue across the class changes (same nanosecond scale).
	last := out.frames[len(out.frames)-1]
	if last.Ticks != uint64(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC).UnixNano())+uint64(sec-1)*1e9+500e6 {
		t.Errorf("last ticks %d", last.Ticks)
	}
}

func TestTimestamperOverrideAndUntimed(t *testing.T) {
	host := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

	// Old firmware (no GPS detail): detected gps_emul; the owner vouches for gps_utc.
	d, out, p := newClockDecoder()
	d.TS.SetClassOverride(model.ClockGPSUTC, true)
	feedAt(d, p, host, BeastStatusType, GPSTimestamp(36000, 0), statusPayload(0x10|0x80, 0, 0))
	feedAt(d, p, host, BeastModeSLong, GPSTimestamp(36000, 5), long1)
	if d.TS.DetectedClass() != model.ClockGPSEmul || out.frames[1].ClockCls != model.ClockGPSUTC {
		t.Errorf("override: detected %v frame %v", d.TS.DetectedClass(), out.frames[1].ClockCls)
	}
	if ov, ok := d.TS.ClassOverride(); !ok || ov != model.ClockGPSUTC {
		t.Error("ClassOverride")
	}

	// A 12 MHz override does not fit a GPS stream: ignored.
	d, out, p = newClockDecoder()
	d.TS.SetClassOverride(model.ClockHW12, true)
	feedAt(d, p, host, BeastStatusType, GPSTimestamp(36000, 0), statusPayload(0x10, 0, gpsUTCLocked))
	feedAt(d, p, host, BeastModeSLong, GPSTimestamp(36000, 5), long1)
	if out.frames[1].ClockCls != model.ClockGPSUTC {
		t.Errorf("mismatched override applied: %v", out.frames[1].ClockCls)
	}

	// Override none: no ticks at all.
	d, out, p = newClockDecoder()
	d.TS.SetClassOverride(model.ClockNone, true)
	feedAt(d, p, host, BeastStatusType, GPSTimestamp(36000, 0), statusPayload(0x10, 0, gpsUTCLocked))
	feedAt(d, p, host, BeastModeSLong, GPSTimestamp(36000, 5), long1)
	if out.frames[1].ClockCls != model.ClockNone || out.frames[1].Ticks != 0 || len(out.events) != 0 || out.frames[1].GPSNs != 36000e9+5 {
		t.Errorf("override none: %+v events %d", out.frames[1], len(out.events))
	}

	// MLAT results and all-zero timestamps are never timed but carry HostNs.
	d, out, p = newClockDecoder()
	feedAt(d, p, host, BeastStatusType, GPSTimestamp(36000, 0), statusPayload(0x10, 0, gpsUTCLocked))
	feedAt(d, p, host, BeastModeSLong, 0xFF004D4C4154, long1)
	feedAt(d, p, host, BeastModeSLong, 0, long1)
	for _, f := range out.frames[1:] {
		if f.ClockCls != model.ClockNone || f.Ticks != 0 || f.Epoch != 0 || f.HostNs != host.UnixNano() {
			t.Errorf("untimed frame %+v", f)
		}
	}

	// 12 MHz with a Radarcape status frame: hw12, ticks = counter.
	d, out, p = newClockDecoder()
	feedAt(d, p, host, BeastStatusType, 1000, statusPayload(0x00, 0, 0))
	feedAt(d, p, host.Add(time.Millisecond), BeastModeSShort, 1000+12000, short1)
	if f := out.frames[1]; f.ClockCls != model.ClockHW12 || f.Ticks != 13000 || f.Clock12MHz != 13000 {
		t.Errorf("12 MHz frame %+v", f)
	}
}

func TestTimestamperStatisticalClassAndReconnectEpoch(t *testing.T) {
	out := &clockSink{}
	d := &AVRDecoder{Sink: out, FeedID: "sdr"}
	d.TS.OnClockEvent = out.event
	d.TS.SetEpochBase(41)
	p := NewAVRParser(d.Handle)
	host := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	// A 12 MHz counter over 6 s: detected statistically as sdr12.
	for i := 0; i < 40; i++ {
		h := host.Add(time.Duration(i) * 150 * time.Millisecond)
		ts := uint64(5e9) + uint64(i)*1_800_000
		d.Recv = h
		p.Write([]byte("@" + hex12(ts) + "5D4840D6202CC3;\n"))
	}
	if d.TS.Class() != model.ClockSDR12 {
		t.Fatalf("class %v", d.TS.Class())
	}
	if len(out.events) != 1 || out.events[0].NewEpoch != 42 || out.events[0].Detail != ClockDetailReconnect {
		t.Errorf("events %+v", out.events)
	}
	lastF := out.frames[len(out.frames)-1]
	if lastF.ClockCls != model.ClockSDR12 || lastF.Ticks != uint64(5e9)+39*1_800_000 || lastF.Epoch != 42 {
		t.Errorf("last frame %+v", lastF)
	}
}

func BenchmarkBeastDecodeTimed(b *testing.B) {
	var stream []byte
	for i := 0; i < 1000; i++ {
		stream = AppendBeastFrame(stream, BeastModeSLong, GPSTimestamp(40000+i/100, (i%100)*10_000_000), byte(i), long2)
	}
	d := &BeastDecoder{Sink: &nullSink{}}
	d.TS.SetStatus(BeastStatus{GPSTimestamps: true, TrueUTCRange: true, GPSDetail: true, AntennaOK: true, Sats3: true, UTC: true, FPGASync: true})
	// The same stream is replayed every iteration; a huge tolerance keeps
	// the replays in one epoch so the steady-state cost is measured.
	d.TS.Clock().BackwardsTolerance = 1 << 62
	p := NewBeastParser(d.Handle)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	host := time.Date(2026, 11, 11, 11, 6, 40, 0, time.UTC)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Recv = host
		p.Write(stream)
	}
}
