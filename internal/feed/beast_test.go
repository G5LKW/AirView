package feed

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

// sink collects everything a decoder delivers.
type sink struct {
	frames  []model.RawFrame
	updates []model.DecodedUpdate
}

func (s *sink) HandleFrame(f model.RawFrame)       { s.frames = append(s.frames, f) }
func (s *sink) HandleUpdate(u model.DecodedUpdate) { s.updates = append(s.updates, u) }

type parsed struct {
	typ     byte
	ts      uint64
	sig     byte
	payload []byte
}

func collectBeast(chunks ...[]byte) ([]parsed, *BeastParser) {
	var out []parsed
	p := NewBeastParser(func(f *BeastFrame) {
		out = append(out, parsed{f.Type, f.Timestamp, f.Signal, append([]byte(nil), f.Payload...)})
	})
	for _, c := range chunks {
		p.Write(c)
	}
	return out, p
}

var (
	long1  = []byte{0x8D, 0x48, 0x40, 0xD6, 0x20, 0x2C, 0xC3, 0x71, 0xC3, 0x2C, 0xE0, 0x57, 0x60, 0x98}
	long2  = []byte{0x8D, 0x1A, 0x1A, 0x1A, 0x58, 0xC3, 0x82, 0xD6, 0x90, 0xC8, 0xAC, 0x28, 0x63, 0x1A} // escapes inside and at the end
	short1 = []byte{0x5D, 0x48, 0x40, 0xD6, 0x20, 0x2C, 0xC3}
	modeAC = []byte{0x1A, 0x00} // Mode A code that happens to contain the escape byte
)

func positionPayload(lat, lon, alt float32) []byte {
	p := make([]byte, 21)
	binary.LittleEndian.PutUint32(p[4:], math.Float32bits(lat))
	binary.LittleEndian.PutUint32(p[8:], math.Float32bits(lon))
	binary.LittleEndian.PutUint32(p[12:], math.Float32bits(alt))
	return p
}

func statusPayload(settings byte, pps int8, gps byte) []byte {
	p := make([]byte, 14)
	p[0], p[1], p[2] = settings, byte(pps), gps
	return p
}

// testStream is a Beast stream with every frame type and escapes in the
// timestamp, the signal byte and the payload.
func testStream() ([]byte, []parsed) {
	want := []parsed{
		{BeastModeSLong, GPSTimestamp(43200, 123456789), 0x80, long1},
		{BeastModeSShort, 0x1A1A1A1A1A1A, 0x1A, short1},
		{BeastModeAC, 0x0000001A0000, 0x40, modeAC},
		{BeastStatusType, GPSTimestamp(43201, 0), 0, statusPayload(0x10, -2, 0x8F)},
		{BeastPosition, 0, 0, positionPayload(51.5, -0.125, 35)},
		{BeastModeSLong, GPSTimestamp(43201, 26), 0xFF, long2},
	}
	var b []byte
	for _, w := range want {
		b = AppendBeastFrame(b, w.typ, w.ts, w.sig, w.payload)
	}
	return b, want
}

func checkParsed(t *testing.T, label string, got, want []parsed) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d frames, want %d", label, len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.typ != w.typ || g.ts != w.ts || g.sig != w.sig || !bytes.Equal(g.payload, w.payload) {
			t.Fatalf("%s: frame %d = {%q %#x %#x % x}, want {%q %#x %#x % x}",
				label, i, g.typ, g.ts, g.sig, g.payload, w.typ, w.ts, w.sig, w.payload)
		}
	}
}

func TestBeastParserAllTypesAndEscapes(t *testing.T) {
	stream, want := testStream()
	// Hand-check the wire form of the trickiest frame: every 0x1A of the
	// timestamp, signal and payload must be doubled.
	enc := AppendBeastFrame(nil, BeastModeAC, 0x0000001A0000, 0x1A, []byte{0x1A, 0x00})
	wire := []byte{0x1A, '1', 0, 0, 0, 0x1A, 0x1A, 0, 0, 0x1A, 0x1A, 0x1A, 0x1A, 0x00}
	if !bytes.Equal(enc, wire) {
		t.Fatalf("encoded frame = % x, want % x", enc, wire)
	}

	got, p := collectBeast(stream)
	checkParsed(t, "whole stream", got, want)
	if p.Frames != uint64(len(want)) || p.Garbage != 0 || p.Truncated != 0 {
		t.Errorf("counters: frames %d garbage %d truncated %d", p.Frames, p.Garbage, p.Truncated)
	}
}

func TestBeastParserSplitReads(t *testing.T) {
	stream, want := testStream()

	// One byte at a time.
	chunks := make([][]byte, len(stream))
	for i := range stream {
		chunks[i] = stream[i : i+1]
	}
	got, _ := collectBeast(chunks...)
	checkParsed(t, "byte by byte", got, want)

	// Every possible two-way and a set of three-way splits.
	for i := 0; i <= len(stream); i++ {
		got, p := collectBeast(stream[:i], stream[i:])
		checkParsed(t, "split", got, want)
		if p.Garbage != 0 || p.Truncated != 0 {
			t.Fatalf("split at %d: garbage %d truncated %d", i, p.Garbage, p.Truncated)
		}
		if j := i + 3; j <= len(stream) {
			got, _ = collectBeast(stream[:i], stream[i:j], stream[j:])
			checkParsed(t, "three-way split", got, want)
		}
	}
}

func TestBeastParserResync(t *testing.T) {
	good1 := AppendBeastFrame(nil, BeastModeSLong, 1000, 10, long1)
	good2 := AppendBeastFrame(nil, BeastModeSShort, 2000, 20, short1)
	good3 := AppendBeastFrame(nil, BeastModeSLong, 3000, 30, long2)
	want := []parsed{
		{BeastModeSLong, 1000, 10, long1},
		{BeastModeSShort, 2000, 20, short1},
		{BeastModeSLong, 3000, 30, long2},
	}

	var s []byte
	s = append(s, "\r\nsome junk *8D4840D6;"...) // text garbage
	s = append(s, 0x1A, 0x1A)                    // tail of a frame we joined in the middle of: escaped 0x1A
	s = append(s, 0x33, 0x44)                    // looks like a type byte but follows an escape pair
	s = append(s, good1...)
	s = append(s, 0x1A, 'Z', 1, 2, 3) // unknown frame type
	s = append(s, good2...)
	s = append(s, good1[:9]...) // frame cut short ...
	s = append(s, good3...)     // ... by the start of the next one

	for i := 0; i <= len(s); i++ {
		got, p := collectBeast(s[:i], s[i:])
		checkParsed(t, "resync", got, want)
		if p.Truncated != 1 {
			t.Fatalf("split %d: truncated = %d, want 1", i, p.Truncated)
		}
		if p.Garbage == 0 {
			t.Fatalf("split %d: no garbage counted", i)
		}
	}

	// readsb extension frames are skipped without losing sync.
	var e []byte
	e = append(e, 0x1A, 0xE3, 1, 2, 3, 4, 5, 6, 7, 8) // receiver ID
	e = append(e, 0x1A, 'P', 9, 9, 9)                 // ping
	e = append(e, good2...)
	got, p := collectBeast(e)
	checkParsed(t, "extensions", got, want[1:2])
	if p.Skipped != 2 || p.Garbage != 0 {
		t.Errorf("skipped %d garbage %d", p.Skipped, p.Garbage)
	}
}

func decodeBeast(d *BeastDecoder, recv time.Time, stream []byte) {
	d.Recv = recv
	NewBeastParser(d.Handle).Write(stream)
}

func TestBeastDecoderRadarcapeGPS(t *testing.T) {
	out := &sink{}
	var (
		statuses  []BeastStatus
		positions []ReceiverPosition
		modes     []TimestampMode
	)
	d := &BeastDecoder{
		Sink: out, FeedID: "rc",
		OnStatus:   func(s BeastStatus) { statuses = append(statuses, s) },
		OnPosition: func(p ReceiverPosition) { positions = append(positions, p) },
		OnMode:     func(m TimestampMode) { modes = append(modes, m) },
	}
	recv := time.Date(2026, 11, 11, 12, 0, 1, 0, time.UTC)

	var s []byte
	// Before any status frame the timestamp meaning is unknown.
	s = AppendBeastFrame(s, BeastModeSLong, GPSTimestamp(43200, 999_999_999), 255, long1)
	// Status: GPS timestamps, 1PPS offset -2 ticks, antenna ok + tracking + >=3 sats + UTC + DPLL sync + true-UTC range.
	s = AppendBeastFrame(s, BeastStatusType, GPSTimestamp(43201, 0), 0, statusPayload(0x10|0x80, -2, 0x9F))
	s = AppendBeastFrame(s, BeastModeSLong, GPSTimestamp(43201, 123_456_789), 128, long2)
	s = AppendBeastFrame(s, BeastModeSShort, GPSTimestamp(0, 5), 0, short1)
	s = AppendBeastFrame(s, BeastModeAC, GPSTimestamp(86399, 999_999_999), 1, modeAC)
	s = AppendBeastFrame(s, BeastModeSLong, 0xFF004D4C4154, 0, long1) // MLAT result from mlat-client
	s = AppendBeastFrame(s, BeastPosition, 0, 0, positionPayload(48.25, 11.5, 520))
	s = AppendBeastFrame(s, BeastStatusType, GPSTimestamp(43202, 0), 0, statusPayload(0x10|0x80, -2, 0x9F)) // unchanged
	decodeBeast(d, recv, s)

	if len(out.frames) != 7 {
		t.Fatalf("got %d frames, want 7", len(out.frames))
	}
	type exp struct {
		kind  model.FrameKind
		gps   int64
		sig   float64
		mlat  bool
		bytes int
	}
	want := []exp{
		{model.FrameModeSLong, -1, 0, false, 14},
		{model.FrameStatus, 43201 * 1e9, math.NaN(), false, 14},
		{model.FrameModeSLong, 43201*1e9 + 123_456_789, 20 * math.Log10(128.0/255), false, 14},
		{model.FrameModeSShort, 5, math.NaN(), false, 7},
		{model.FrameModeAC, 86399*1e9 + 999_999_999, 20 * math.Log10(1.0/255), false, 2},
		{model.FrameModeSLong, -1, math.NaN(), true, 14},
		{model.FrameStatus, 43202 * 1e9, math.NaN(), false, 14},
	}
	for i, w := range want {
		f := out.frames[i]
		sigOK := (math.IsNaN(w.sig) && math.IsNaN(f.Signal)) || math.Abs(f.Signal-w.sig) < 1e-12
		if f.Kind != w.kind || f.GPSNs != w.gps || !sigOK || f.MLAT != w.mlat || len(f.Data) != w.bytes ||
			f.Clock12MHz != 0 || f.FeedID != "rc" || f.Channel != -1 || !f.Recv.Equal(recv) {
			t.Errorf("frame %d = %+v, want %+v", i, f, w)
		}
	}
	if got := out.frames[2].Signal; math.Abs(got-(-5.986)) > 0.001 {
		t.Errorf("signal byte 128 = %.3f dBFS, want -5.986", got)
	}
	if !bytes.Equal(out.frames[2].Data, long2) || !bytes.Equal(out.frames[4].Data, modeAC) {
		t.Error("payload corrupted")
	}

	if len(statuses) != 1 {
		t.Fatalf("OnStatus called %d times, want 1 (second status frame is identical)", len(statuses))
	}
	st := statuses[0]
	if !st.GPSTimestamps || !st.ModeAC || st.DF17Only || st.PPSOffset != -2 || st.PPSOffsetNs() != -30 ||
		!st.AntennaOK || !st.Tracking || !st.Sats3 || !st.UTC || !st.TrueUTCRange || !st.GPSLocked() {
		t.Errorf("status = %+v", st)
	}
	if got, ok := ParseBeastStatus(out.frames[1].Data); !ok || got != st {
		t.Errorf("ParseBeastStatus(frame data) = %+v, %v", got, ok)
	}
	if len(positions) != 1 || positions[0] != (ReceiverPosition{48.25, 11.5, 520}) {
		t.Errorf("positions = %+v", positions)
	}
	if len(modes) != 1 || modes[0] != TimestampGPS {
		t.Errorf("mode changes = %v", modes)
	}
	if st.String() == "" {
		t.Error("empty status text")
	}
}

func TestBeastDecoderLegacyDayRangeAndMLATOnly(t *testing.T) {
	out := &sink{}
	d := &BeastDecoder{Sink: out, FeedID: "rc", MLATOnly: true}
	var s []byte
	// GPS status without bit 7: second-of-day counts 1..86400.
	s = AppendBeastFrame(s, BeastStatusType, GPSTimestamp(1, 0), 0, statusPayload(0x10, 0, 0x07))
	s = AppendBeastFrame(s, BeastModeSLong, GPSTimestamp(1, 500), 100, long1)     // first second of the day
	s = AppendBeastFrame(s, BeastModeSLong, GPSTimestamp(86400, 500), 100, long1) // last second of the day
	s = AppendBeastFrame(s, BeastModeSLong, GPSTimestamp(0, 500), 100, long1)     // mlat-client treats 0 as 86399
	s = AppendBeastFrame(s, BeastModeSLong, 0xFFFFFFFFFFFF, 100, long1)           // corrupt: nanos >= 1e9
	decodeBeast(d, time.Now(), s)

	want := []int64{0, 500, 86399*1e9 + 500, 86399*1e9 + 500, -1}
	if len(out.frames) != len(want) {
		t.Fatalf("got %d frames", len(out.frames))
	}
	for i, w := range want {
		if out.frames[i].GPSNs != w {
			t.Errorf("frame %d GPSNs = %d, want %d", i, out.frames[i].GPSNs, w)
		}
	}
	for i, f := range out.frames[1:] {
		if !f.MLAT {
			t.Errorf("frame %d: MLATOnly feed did not set MLAT", i+1)
		}
	}
}

func TestBeastDecoder12MHzStatus(t *testing.T) {
	out := &sink{}
	d := &BeastDecoder{Sink: out}
	var s []byte
	s = AppendBeastFrame(s, BeastStatusType, 123456, 0, statusPayload(0x02, 0, 0)) // legacy timestamps, DF17 only
	s = AppendBeastFrame(s, BeastModeSLong, 0x123456789ABC, 200, long1)
	decodeBeast(d, time.Now(), s)
	if f := out.frames[1]; f.GPSNs != -1 || f.Clock12MHz != 0x123456789ABC {
		t.Errorf("frame = %+v", f)
	}
	if st, _ := ParseBeastStatus(out.frames[0].Data); st.GPSTimestamps || !st.DF17Only || st.GPSLocked() {
		t.Errorf("status = %+v", st)
	}
}

func TestBeastStatusWithoutGPSDetailAndZeroTimestamp(t *testing.T) {
	// Old firmware: GPS timestamps selected, GPS status byte all zero. That
	// is "not reported", not an antenna fault.
	st, ok := ParseBeastStatus(statusPayload(0x10, 1, 0x00))
	if !ok || st.GPSDetail || st.GPSLocked() {
		t.Fatalf("status = %+v", st)
	}
	if s := st.String(); !strings.Contains(s, "not reported") || strings.Contains(s, "fault") {
		t.Errorf("status text = %q", s)
	}
	if st, _ := ParseBeastStatus(statusPayload(0x10, 0, 0x80)); !st.GPSDetail || !strings.Contains(st.String(), "antenna fault") {
		t.Errorf("modern firmware with a dead GPS antenna: %q", st.String())
	}
	if st, _ := ParseBeastStatus(statusPayload(0x10, 0, 0x83)); !strings.Contains(st.String(), "acquiring") {
		t.Errorf("tracking one satellite: %q", st.String())
	}
	if st, _ := ParseBeastStatus(statusPayload(0x10, 0, 0xAF)); !strings.Contains(st.String(), "not valid") {
		t.Errorf("timestamp flagged bad: %q", st.String())
	}

	// A frame with an all-zero timestamp has no receiver time, whatever the
	// stream's timestamp mode is.
	for _, settings := range []byte{0x10, 0x00} {
		out := &sink{}
		d := &BeastDecoder{Sink: out}
		var s []byte
		s = AppendBeastFrame(s, BeastStatusType, GPSTimestamp(100, 0), 0, statusPayload(settings, 0, 0x8F))
		s = AppendBeastFrame(s, BeastModeSLong, 0, 77, long1)
		decodeBeast(d, time.Now(), s)
		if f := out.frames[1]; f.GPSNs != -1 || f.Clock12MHz != 0 || f.MLAT {
			t.Errorf("settings %#x: zero-timestamp frame = %+v", settings, f)
		}
	}
}

// feedStat drives a Timestamper with n+1 frames spread evenly over the
// interval [from, from+dur] after t0; stamp maps the time since t0 to a
// receiver timestamp.
func feedStat(ts *Timestamper, t0 time.Time, from, dur time.Duration, n int, stamp func(elapsed time.Duration) uint64) (changed int) {
	for i := 0; i <= n; i++ {
		el := from + time.Duration(int64(dur)*int64(i)/int64(n))
		if ts.Observe(stamp(el), t0.Add(el)) {
			changed++
		}
	}
	return changed
}

func TestTimestamperStatisticalDetection(t *testing.T) {
	const sec = time.Second
	t0 := time.Date(2026, 11, 11, 23, 59, 57, 0, time.UTC) // crosses midnight
	gps := func(el time.Duration) uint64 {
		ns := int64(86397)*1e9 + 250_000_000 + int64(el)
		ns %= 86400 * 1e9
		return GPSTimestamp(int(ns/1e9), int(ns%1e9))
	}
	// A 12 MHz counter that has been running for a while.
	counter := func(el time.Duration) uint64 {
		return 0x0000_3000_0000 + uint64(el.Seconds()*12e6)
	}

	var a Timestamper
	if gpsNs, clock := a.Convert(gps(0)); gpsNs != -1 || clock != 0 {
		t.Errorf("unknown mode must not produce timestamps: %d %d", gpsNs, clock)
	}
	if feedStat(&a, t0, 0, 6*sec, 300, gps) != 1 || a.Mode() != TimestampGPS {
		t.Fatalf("GPS stream detected as %v", a.Mode())
	}
	if gpsNs, clock := a.Convert(GPSTimestamp(10, 20)); gpsNs != 10*1e9+20 || clock != 0 {
		t.Errorf("Convert in GPS mode = %d, %d", gpsNs, clock)
	}

	var b Timestamper
	if feedStat(&b, t0, 0, 6*sec, 300, counter) != 1 || b.Mode() != Timestamp12MHz {
		t.Fatalf("12 MHz stream detected as %v", b.Mode())
	}
	if gpsNs, clock := b.Convert(0xABCDEF); gpsNs != -1 || clock != 0xABCDEF {
		t.Errorf("Convert in 12 MHz mode = %d, %d", gpsNs, clock)
	}

	// Too few frames, or too short a time: no verdict yet.
	var c, c2 Timestamper
	feedStat(&c, t0, 0, 8*sec, 5, gps)
	feedStat(&c2, t0, 0, 4*sec, 400, gps)
	if c.Mode() != TimestampUnknown || c2.Mode() != TimestampUnknown {
		t.Errorf("premature verdict: %v / %v", c.Mode(), c2.Mode())
	}
	// ... and the sparse stream is recognised once enough frames arrived.
	if feedStat(&c, t0, 9*sec, 2*sec, 20, gps); c.Mode() != TimestampGPS {
		t.Errorf("sparse GPS stream detected as %v", c.Mode())
	}

	// One contradicting window is not enough to flip an established mode
	// (a's second window started at about t0+5 s) ...
	if feedStat(&a, t0, 6*sec, 4500*time.Millisecond, 100, counter) != 0 || a.Mode() != TimestampGPS {
		t.Fatalf("mode flipped after a single contradicting window")
	}
	// ... two in a row are.
	if feedStat(&a, t0, 10500*time.Millisecond, 5500*time.Millisecond, 100, counter) != 1 || a.Mode() != Timestamp12MHz {
		t.Fatalf("mode did not flip after two contradicting windows: %v", a.Mode())
	}

	// A status frame overrides statistics for good.
	a.SetStatus(BeastStatus{GPSTimestamps: true, TrueUTCRange: true})
	feedStat(&a, t0, 20*sec, 12*sec, 400, counter)
	if a.Mode() != TimestampGPS {
		t.Errorf("statistics overrode the status frame")
	}
}

func TestBeastDecoderStatisticalStream(t *testing.T) {
	// A GPS-timestamped Beast stream without status frames (e.g. relayed by
	// other software): timestamps appear once the detector has decided.
	out := &sink{}
	var modes []TimestampMode
	d := &BeastDecoder{Sink: out, OnMode: func(m TimestampMode) { modes = append(modes, m) }}
	p := NewBeastParser(d.Handle)
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 700; i++ {
		el := time.Duration(i) * 10 * time.Millisecond
		d.Recv = t0.Add(el)
		ns := int64(36000)*1e9 + int64(el)
		p.Write(AppendBeastFrame(nil, BeastModeSLong, GPSTimestamp(int(ns/1e9), int(ns%1e9)), 90, long1))
	}
	if len(modes) != 1 || modes[0] != TimestampGPS {
		t.Fatalf("modes = %v", modes)
	}
	if out.frames[0].GPSNs != -1 || out.frames[0].Clock12MHz != 0 {
		t.Errorf("first frame already carries a timestamp: %+v", out.frames[0])
	}
	last := out.frames[len(out.frames)-1]
	if want := int64(36000)*1e9 + int64(6990*time.Millisecond); last.GPSNs != want {
		t.Errorf("last GPSNs = %d, want %d", last.GPSNs, want)
	}
}

func TestBeastDataOwnershipAndAllocations(t *testing.T) {
	// Each frame owns its Data: later frames must not disturb retained ones.
	out := &sink{}
	d := &BeastDecoder{Sink: out}
	var s []byte
	s = AppendBeastFrame(s, BeastModeSLong, 1, 1, long1)
	s = AppendBeastFrame(s, BeastModeSLong, 2, 2, long2)
	s = AppendBeastFrame(s, BeastModeSShort, 3, 3, short1)
	decodeBeast(d, time.Now(), s)
	if !bytes.Equal(out.frames[0].Data, long1) || !bytes.Equal(out.frames[1].Data, long2) || !bytes.Equal(out.frames[2].Data, short1) {
		t.Fatal("retained Data slices were overwritten")
	}
	if cap(out.frames[2].Data) != 7 {
		t.Errorf("Data capacity = %d, want exactly 7", cap(out.frames[2].Data))
	}

	// The parser itself never allocates; the decoder allocates exactly the
	// Data slice per frame.
	stream, _ := testStream()
	n := 0
	p := NewBeastParser(func(*BeastFrame) { n++ })
	if a := testing.AllocsPerRun(100, func() { p.Write(stream) }); a != 0 {
		t.Errorf("parser allocates %v times per stream", a)
	}
	null := &nullSink{}
	d2 := &BeastDecoder{Sink: null}
	p2 := NewBeastParser(d2.Handle)
	one := AppendBeastFrame(nil, BeastModeSLong, 12345, 99, long1)
	if a := testing.AllocsPerRun(200, func() { p2.Write(one) }); a != 1 {
		t.Errorf("decoder allocates %v times per frame, want 1 (the Data slice)", a)
	}
}

type nullSink struct{ n int }

func (s *nullSink) HandleFrame(model.RawFrame)       { s.n++ }
func (s *nullSink) HandleUpdate(model.DecodedUpdate) { s.n++ }

func TestBeastSignalAndHelpers(t *testing.T) {
	if !math.IsNaN(BeastSignalDBFS(0)) || BeastSignalDBFS(255) != 0 {
		t.Error("signal table endpoints wrong")
	}
	if got := BeastSignalDBFS(1); math.Abs(got-(-48.13)) > 0.01 {
		t.Errorf("signal byte 1 = %.2f dBFS", got)
	}
	if !IsMLATTimestamp(0xFF004D4C4154) || !IsMLATTimestamp(0xFF004D4C415E) || IsMLATTimestamp(0xFF004D4C415F) || IsMLATTimestamp(0) {
		t.Error("IsMLATTimestamp range wrong")
	}
	if _, ok := ParseBeastPosition(positionPayload(float32(math.NaN()), 1, 1)); ok {
		t.Error("NaN position accepted")
	}
	if _, ok := ParseBeastPosition(positionPayload(95, 1, 1)); ok {
		t.Error("out-of-range latitude accepted")
	}
	if _, ok := ParseBeastStatus([]byte{1, 2}); ok {
		t.Error("short status accepted")
	}
}

func BenchmarkBeastDecode(b *testing.B) {
	var stream []byte
	for i := 0; i < 1000; i++ {
		stream = AppendBeastFrame(stream, BeastModeSLong, GPSTimestamp(40000+i/100, i*1000), byte(i), long2)
	}
	d := &BeastDecoder{Sink: &nullSink{}}
	d.TS.SetStatus(BeastStatus{GPSTimestamps: true, TrueUTCRange: true})
	p := NewBeastParser(d.Handle)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Write(stream)
	}
}
