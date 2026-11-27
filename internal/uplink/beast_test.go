package uplink

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/feed"
	"airview/internal/ingest/beastin"
	"airview/internal/model"
	"airview/internal/uplink/avftest"
)

// frameSink collects frames delivered by beastin.
type frameSink struct {
	mu     sync.Mutex
	frames []model.RawFrame
}

func (s *frameSink) HandleFrame(f model.RawFrame) {
	s.mu.Lock()
	f.Data = append([]byte(nil), f.Data...)
	s.frames = append(s.frames, f)
	s.mu.Unlock()
}
func (s *frameSink) HandleUpdate(model.DecodedUpdate) {}
func (s *frameSink) list() []model.RawFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.RawFrame(nil), s.frames...)
}

// startBeastin runs AirView's own clean-room Beast ingest (the 30004
// server) as the third-party sink.
func startBeastin(t *testing.T, opt beastin.Options) (*beastin.Server, *beastin.MemRegistry, *frameSink, int) {
	t.Helper()
	reg := beastin.NewMemRegistry()
	sink := &frameSink{}
	opt.Addr = "127.0.0.1:0"
	s, err := beastin.New(opt, reg, beastin.FrameSinkAdapter{Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", opt.Addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); s.Close(); <-done })
	return s, reg, sink, ln.Addr().(*net.TCPAddr).Port
}

func TestBeastNormalize(t *testing.T) {
	b := BeastUplink{ID: "lol", Host: " in.example.org ", Format: "reduce_plus", ReduceMs: 10, UUID: "not-a-uuid"}
	notes := b.Normalize("beast[0]")
	if b.Host != "in.example.org" || b.Port != DefaultBeastPort || b.Format != BeastReducePlus || b.ReduceMs != 50 || b.UUID != "" {
		t.Fatalf("normalised %+v", b)
	}
	if len(notes) != 2 || !strings.HasPrefix(notes[0], "beast[0].") {
		t.Fatalf("notes %q", notes)
	}
	b = BeastUplink{ID: "x", Host: "h", Port: 70000, Format: "zzz", UUID: "0123456789abcdef0123456789abcdef"}
	if n := b.Normalize("p"); len(n) != 2 || b.Port != DefaultBeastPort || b.Format != BeastReducePlus || b.UUID == "" || b.ReduceMs != 250 {
		t.Fatalf("%+v %q", b, n)
	}
	if err := (&BeastUplink{}).Validate(); err == nil {
		t.Fatal("empty host accepted")
	}
	if err := (&BeastUplink{Host: "a b"}).Validate(); err == nil {
		t.Fatal("bad host accepted")
	}
}

func TestBeastTimestamp(t *testing.T) {
	cases := []struct {
		f    model.RawFrame
		want uint64
	}{
		{model.RawFrame{Clock12MHz: 0x123456789ABC, GPSNs: -1}, 0x123456789ABC},
		{model.RawFrame{GPSNs: 3723*1e9 + 500, ClockCls: model.ClockGPSUTC}, feed.GPSTimestamp(3723, 500)},
		{model.RawFrame{GPSNs: -1, ClockCls: model.ClockGPSUTC, Ticks: uint64(2*model.NsPerDay + 61e9 + 7)}, feed.GPSTimestamp(61, 7)},
		{model.RawFrame{GPSNs: -1, ClockCls: model.ClockSDR12, Ticks: 1<<48 + 5}, 5},
		{model.RawFrame{GPSNs: -1, ClockCls: model.ClockSBS20, Ticks: 20e6}, 12e6},
		{model.RawFrame{GPSNs: -1}, 0},
	}
	for i, c := range cases {
		if got := beastTimestamp(&c.f); got != c.want {
			t.Errorf("case %d: %x want %x", i, got, c.want)
		}
	}
}

func TestBeastReducer(t *testing.T) {
	var r beastReducer
	r.init(250 * time.Millisecond)
	pos := df17(0x4840D6, 0x58C382D690C8AC) // TC 11 airborne position (even)
	odd := append([]byte(nil), pos...)
	odd[6] |= 0x04                             // odd CPR: its own key
	ident := df17(0x4840D6, 0x20_2CC371C32CE0) // TC 4 identification
	sq := df4(0x4840D6)
	type tc struct {
		d    []byte
		atMs int64
		want bool
	}
	steps := []tc{
		{pos, 0, true}, {pos, 100, false}, {odd, 120, true}, {pos, 249, false}, {pos, 250, true},
		{ident, 0, true}, {ident, 900, false}, {ident, 1000, true},
		{sq, 0, true}, {sq, 999, false}, {sq, 1000, true},
	}
	for i, s := range steps {
		if got := r.pass(s.d, s.atMs*1e6, 0); got != s.want {
			t.Fatalf("step %d: pass %v want %v", i, got, s.want)
		}
	}
	// slow-down level 1 doubles the interval
	r.reset()
	if !r.pass(pos, 0, 1) || r.pass(pos, 400e6, 1) || !r.pass(pos, 500e6, 1) {
		t.Fatal("slow level not applied")
	}
	// a frame with an older host time than the last one passes (no wedge)
	if !r.pass(pos, 100e6, 1) {
		t.Fatal("clock step back wedged the reducer")
	}
}

// beastTruth replays the reduce rule over the frames to predict what a
// beast_reduce_plus_out output sends.
func beastTruth(frames []model.RawFrame, base time.Duration) map[string]int {
	var r beastReducer
	r.init(base)
	out := map[string]int{}
	for i := range frames {
		f := &frames[i]
		if f.MLAT || feed.IsMLATTimestamp(f.Clock12MHz) || f.Late || !crcPlausible(f.Data) || f.FeedID == "hub" {
			continue
		}
		if r.pass(f.Data, f.HostNs, 0) {
			out[hex.EncodeToString(f.Data)]++
		}
	}
	return out
}

// TestDualUplinkAVFAndBeast is the acceptance test of "also feed others":
// one station feeds an AVF/1 aggregator and a third-party Beast aggregator
// (beast_reduce_plus_out with UUID) at the same time. AVF gets every
// forwardable frame; the Beast sink gets exactly the reduced set with the
// receiver's timestamps; neither sees MLAT results, late frames, frames of
// non-forwardable feeds or bad-CRC frames; pings are echoed.
func TestDualUplinkAVFAndBeast(t *testing.T) {
	avfSrv := avftest.Start(t, avftest.Options{AckEvery: 50 * time.Millisecond})
	bsrv, reg, sink, port := startBeastin(t, beastin.Options{PingInterval: 50 * time.Millisecond})

	o := testOptions(t)
	o.StateDir = t.TempDir()
	o.Source = SourceFuncs{ForwardableFn: func(id string) bool { return id != "hub" }}
	m := newMgr(t, o, avfSrv.Uplink("club"))
	const myUUID = "6f3a2d1c-0b9e-4c8d-a7f6-5e4d3c2b1a09"
	if notes := m.ApplyBeast([]BeastUplink{{ID: "community", Host: "127.0.0.1", Port: port, UUID: myUUID}}); len(notes) != 0 {
		t.Fatal(notes)
	}
	waitFor(t, 5*time.Second, "beast connected", func() bool {
		st := m.BeastStatus()
		return len(st) == 1 && st[0].State == StateConnected
	})
	waitFor(t, 5*time.Second, "avf connected", func() bool { return m.Status()[0].State == StateConnected })

	// 20 aircraft for 3 s at 2 ms per frame: positions (even/odd), identity,
	// DF4; plus frames that must never leave the station.
	h0 := time.Now().UnixNano()
	var frames []model.RawFrame
	ticks := uint64(1_000_000)
	for i := 0; i < 1500; i++ {
		ac := uint32(0x400000 + i%20)
		var d []byte
		switch i % 4 {
		case 0:
			d = df17(ac, 0x58C382D690C8AC)
		case 1:
			d = df17(ac, 0x58C386435CC412)
		case 2:
			d = df17(ac, 0x20_2CC371C32CE0)
		default:
			d = df4(ac)
		}
		ticks += 24_000
		f := model.RawFrame{Recv: time.Unix(0, h0+int64(i)*2e6), HostNs: h0 + int64(i)*2e6, Kind: model.FrameModeSLong, Data: d,
			FeedID: "rx1", Channel: -1, Signal: -20, ClockCls: model.ClockHW12, Epoch: 1, Ticks: ticks, Clock12MHz: ticks, GPSNs: -1}
		if len(d) == 7 {
			f.Kind = model.FrameModeSShort
		}
		switch i % 50 {
		case 7: // an MLAT result from the local decoder (flag)
			f.MLAT = true
		case 17: // an MLAT result with the magic timestamp
			f.Clock12MHz = 0xFF004D4C4154
		case 27: // backfilled data of another link
			f.Late = true
		case 37: // a hub feed (another station's data)
			f.FeedID = "hub"
		case 47: // a corrupted DF17
			if len(d) == 14 {
				f.Data = append([]byte(nil), d...)
				f.Data[5] ^= 0x10
			}
		}
		frames = append(frames, f)
	}
	for i := range frames {
		m.HandleFrame(frames[i])
	}
	m.Flush()

	// AVF: every forwardable, non-MLAT, non-late frame (bad CRC included:
	// the aggregator decides).
	wantAVF := 0
	for i := range frames {
		f := &frames[i]
		if !f.MLAT && !feed.IsMLATTimestamp(f.Clock12MHz) && !f.Late && f.FeedID != "hub" {
			wantAVF++
		}
	}
	waitFor(t, 10*time.Second, "avf frames", func() bool { return avfSrv.FrameCount() >= wantAVF })
	time.Sleep(100 * time.Millisecond)
	if n := avfSrv.FrameCount(); n != wantAVF {
		t.Fatalf("avf frames %d want %d", n, wantAVF)
	}

	// Beast: exactly the reduced set.
	truth := beastTruth(frames, DefaultBeastReduceMs*time.Millisecond)
	wantN := 0
	for _, n := range truth {
		wantN += n
	}
	waitFor(t, 10*time.Second, "beast frames", func() bool { return len(sink.list()) >= wantN })
	time.Sleep(150 * time.Millisecond)
	got := map[string]int{}
	// (beastin's timestamp-mode detection needs real-time spacing, so the
	// timestamps are checked on the raw stream in TestBeastFormatsAndApply.)
	for _, f := range sink.list() {
		if len(f.Data) == 2 && f.Data[0] == 0 && f.Data[1] == 0 {
			continue // keep-alive
		}
		if f.MLAT {
			t.Fatal("an MLAT frame reached the beast sink")
		}
		got[hex.EncodeToString(f.Data)]++
	}
	if len(got) != len(truth) {
		t.Fatalf("beast keys %d want %d", len(got), len(truth))
	}
	for k, n := range truth {
		if got[k] != n {
			t.Fatalf("beast frame %s: %d want %d", k, got[k], n)
		}
	}
	st := m.BeastStatus()[0]
	if st.FramesSent != uint64(wantN) || st.MLATRefused != 60 || st.BadCRC == 0 || st.UUID != myUUID || st.Reduced == 0 {
		t.Fatalf("beast status %+v (want sent %d)", st, wantN)
	}
	// the UUID made the link a known station; pings were echoed
	key := "uuid:" + strings.ReplaceAll(myUUID, "-", "")
	if _, ok := reg.Lookup(key); !ok {
		t.Fatalf("station %s not attached", key)
	}
	waitFor(t, 5*time.Second, "ping echo", func() bool {
		ls := bsrv.Links()
		return len(ls) == 1 && ls[0].PingOn && ls[0].RTTLastMs >= 0 && ls[0].PingsSent > 0 && m.BeastStatus()[0].PingsEchoed > 0
	})
	if errs := avfSrv.Errors(); len(errs) != 0 {
		t.Fatal(errs)
	}

	// The status API lists both.
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	var resp StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Uplinks) != 1 || len(resp.Beast) != 1 || resp.Beast[0].ID != "community" {
		t.Fatalf("status %s (%v)", rec.Body.String(), err)
	}
	// Removing the Beast output leaves AVF alone.
	m.ApplyBeast(nil)
	if len(m.BeastStatus()) != 0 || m.Status()[0].State != StateConnected {
		t.Fatal("removing the beast output disturbed the avf uplink")
	}
}

// rawBeastServer accepts one connection at a time and records the bytes.
type rawBeastServer struct {
	ln    net.Listener
	mu    sync.Mutex
	data  []byte
	conns []net.Conn
	accN  int
}

func startRawBeast(t *testing.T) *rawBeastServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &rawBeastServer{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.accN++
			s.mu.Unlock()
			go func() {
				b := make([]byte, 4096)
				for {
					n, err := c.Read(b)
					s.mu.Lock()
					s.data = append(s.data, b[:n]...)
					s.mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	})
	return s
}

func (s *rawBeastServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }
func (s *rawBeastServer) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...)
}
func (s *rawBeastServer) last() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) == 0 {
		return nil
	}
	return s.conns[len(s.conns)-1]
}
func (s *rawBeastServer) accepted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accN
}

// TestBeastPreambleSlowDownReconnect checks the wire preamble (UUID padded
// to 36 bytes, 'W' 'O'), the generated per-uplink UUID kept in the state
// directory, the 'W' 'S' slow-down, the ping echo, and that frames offered
// while disconnected are dropped, not queued.
func TestBeastPreambleSlowDownReconnect(t *testing.T) {
	srv := startRawBeast(t)
	o := testOptions(t)
	o.StateDir = t.TempDir()
	m := newMgr(t, o)
	m.ApplyBeast([]BeastUplink{{ID: "other", Host: "127.0.0.1", Port: srv.port()}})
	waitFor(t, 5*time.Second, "connected", func() bool { return m.BeastStatus()[0].State == StateConnected })
	waitFor(t, 5*time.Second, "preamble", func() bool { return len(srv.bytes()) >= 2+36+3 })
	b := srv.bytes()
	if b[0] != 0x1A || b[1] != feed.BeastUUID || b[38] != 0x1A || b[39] != feed.BeastOption || b[40] != feed.BeastOptPing {
		t.Fatalf("preamble % x", b[:41])
	}
	sent := string(b[2:38])
	kept, err := os.ReadFile(filepath.Join(o.StateDir, "uplinks", "other", UUIDFile))
	if err != nil || strings.TrimSpace(string(kept)) != sent || sent == m.Identity().UUID {
		t.Fatalf("uuid %q kept %q (%v)", sent, kept, err)
	}
	if _, ok := feed.NormalizeUUID(sent); !ok {
		t.Fatalf("uuid %q not valid", sent)
	}

	// ping echo and slow-down
	c := srv.last()
	_, _ = c.Write(feed.AppendBeastOption(feed.AppendBeastPing(nil, 0x1A2B3C), feed.BeastOptSlowDown))
	waitFor(t, 5*time.Second, "echo", func() bool {
		return strings.Contains(string(srv.bytes()), string(feed.AppendBeastPing(nil, 0x1A2B3C)))
	})
	waitFor(t, 5*time.Second, "slow", func() bool { return m.BeastStatus()[0].SlowDowns == 1 })
	if st := m.BeastStatus()[0]; st.PingsEchoed != 1 || st.SlowUntil.IsZero() {
		t.Fatalf("%+v", st)
	}
	// with the slow-down, one position per 500 ms
	h := time.Now().UnixNano()
	for i := 0; i < 10; i++ {
		f := model.RawFrame{HostNs: h + int64(i)*100e6, Kind: model.FrameModeSLong, Data: df17(0x400001, 0x58C382D690C8AC),
			FeedID: "rx", GPSNs: -1, Signal: -10}
		m.HandleFrame(f)
	}
	waitFor(t, 5*time.Second, "sent", func() bool { return m.BeastStatus()[0].FramesSent == 2 })
	if st := m.BeastStatus()[0]; st.Reduced != 8 {
		t.Fatalf("reduced %d", st.Reduced)
	}

	// the server goes away: frames are dropped while down, then it reconnects
	c.Close()
	waitFor(t, 5*time.Second, "backoff", func() bool { return m.BeastStatus()[0].State != StateConnected })
	m.HandleFrame(model.RawFrame{HostNs: h + 5e9, Kind: model.FrameModeSLong, Data: df17(0x400002, 0x58C382D690C8AC), FeedID: "rx", GPSNs: -1})
	if st := m.BeastStatus()[0]; st.Dropped != 1 {
		t.Fatalf("dropped %d", st.Dropped)
	}
	waitFor(t, 5*time.Second, "reconnect", func() bool { return srv.accepted() == 2 && m.BeastStatus()[0].State == StateConnected })
	if st := m.BeastStatus()[0]; st.Connects != 2 {
		t.Fatalf("connects %d", st.Connects)
	}
}

// TestBeastFormatsAndApply checks beast_out (no preamble, no reduction),
// in-place updates, reconnect on a connection change, and the ID rules.
func TestBeastFormatsAndApply(t *testing.T) {
	srv := startRawBeast(t)
	avfSrv := avftest.Start(t, avftest.Options{})
	m := newMgr(t, testOptions(t), avfSrv.Uplink("club"))
	notes := m.ApplyBeast([]BeastUplink{
		{ID: "full", Host: "127.0.0.1", Port: srv.port(), Format: BeastFull, NoTimestamps: true},
		{ID: "club", Host: "127.0.0.1", Port: srv.port()}, // clashes with the AVF uplink
		{ID: "Bad ID", Host: "x"},
		{ID: "nohost"},
	})
	if len(notes) != 3 || len(m.BeastStatus()) != 1 {
		t.Fatalf("notes %q status %d", notes, len(m.BeastStatus()))
	}
	waitFor(t, 5*time.Second, "connected", func() bool { return m.BeastStatus()[0].State == StateConnected })
	h := time.Now().UnixNano()
	for i := 0; i < 5; i++ {
		m.HandleFrame(model.RawFrame{HostNs: h + int64(i), Kind: model.FrameModeSLong, Data: df17(0x400001, 0x58C382D690C8AC),
			FeedID: "rx", GPSNs: -1, Clock12MHz: 12345, ClockCls: model.ClockHW12, Ticks: 12345, Epoch: 1})
	}
	waitFor(t, 5*time.Second, "frames", func() bool { return m.BeastStatus()[0].FramesSent == 5 })
	var n int
	p := feed.NewBeastParser(func(fr *feed.BeastFrame) {
		n++
		if fr.Timestamp != 0 {
			t.Errorf("timestamp %x with NoTimestamps", fr.Timestamp)
		}
	})
	esc := 0
	p.OnEscape = func(byte, []byte) { esc++ }
	waitFor(t, 5*time.Second, "bytes", func() bool { return len(srv.bytes()) >= 5*23 })
	p.Write(srv.bytes())
	if n != 5 || esc != 0 {
		t.Fatalf("frames %d escapes %d", n, esc)
	}
	// in-place change: no reconnect
	m.ApplyBeast([]BeastUplink{{ID: "full", Host: "127.0.0.1", Port: srv.port(), Format: BeastFull, Inputs: []string{"other"}}})
	time.Sleep(100 * time.Millisecond)
	if srv.accepted() != 1 {
		t.Fatal("in-place change reconnected")
	}
	m.HandleFrame(model.RawFrame{HostNs: h, Kind: model.FrameModeSLong, Data: df17(0x400001, 0x58C382D690C8AC), FeedID: "rx", GPSNs: -1})
	if m.BeastStatus()[0].FramesIn != 5 {
		t.Fatal("inputs filter not applied")
	}
	// connection change: reconnect
	m.ApplyBeast([]BeastUplink{{ID: "full", Host: "127.0.0.1", Port: srv.port(), Format: BeastReduce}})
	waitFor(t, 5*time.Second, "reconnect", func() bool { return srv.accepted() == 2 && m.BeastStatus()[0].State == StateConnected })
	// timestamps on: the receiver's 12 MHz stamp and a GPS stamp go out as is
	m.HandleFrame(model.RawFrame{HostNs: h, Kind: model.FrameModeSLong, Data: df17(0x400003, 0x58C382D690C8AC), FeedID: "rx", GPSNs: -1,
		Clock12MHz: 0xABCDEF123456, ClockCls: model.ClockHW12, Ticks: 0xABCDEF123456, Epoch: 1, Signal: -6})
	m.HandleFrame(model.RawFrame{HostNs: h, Kind: model.FrameModeSLong, Data: df17(0x400004, 0x58C382D690C8AC), FeedID: "rx",
		GPSNs: 45296*1e9 + 789, ClockCls: model.ClockGPSUTC, Epoch: 1, Signal: -6})
	waitFor(t, 5*time.Second, "stamped frames", func() bool { return m.BeastStatus()[0].FramesSent == 2 }) // a new client after the reconnect
	stamps := map[uint64]byte{}
	p2 := feed.NewBeastParser(func(fr *feed.BeastFrame) { stamps[fr.Timestamp] = fr.Signal })
	waitFor(t, 5*time.Second, "stamped bytes", func() bool {
		clear(stamps)
		p2.Reset()
		p2.Write(srv.bytes())
		_, a := stamps[0xABCDEF123456]
		_, b := stamps[feed.GPSTimestamp(45296, 789)]
		return a && b
	})
	if stamps[0xABCDEF123456] != sigByte(-6) {
		t.Fatalf("signal byte %d", stamps[0xABCDEF123456])
	}
	if err := m.Retry("full"); err != nil {
		t.Fatal(err)
	}
	// disabled
	m.ApplyBeast([]BeastUplink{{ID: "full", Host: "127.0.0.1", Port: srv.port(), Disabled: true}})
	waitFor(t, 5*time.Second, "disabled", func() bool { return m.BeastStatus()[0].State == StateDisabled })
}

func BenchmarkBeastHandleFrame(b *testing.B) {
	srv := startRawBeastB(b)
	o := Options{Logf: func(string, ...any) {}, SDRoot: func() bool { return false }}
	m, err := NewManager(o)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	m.ApplyBeast([]BeastUplink{{ID: "b", Host: "127.0.0.1", Port: srv}})
	end := time.Now().Add(5 * time.Second)
	for m.BeastStatus()[0].State != StateConnected && time.Now().Before(end) {
		time.Sleep(10 * time.Millisecond)
	}
	fs := make([]model.RawFrame, 256)
	for i := range fs {
		fs[i] = model.RawFrame{Kind: model.FrameModeSLong, Data: df17(0x400000+uint32(i), 0x58C382D690C8AC), FeedID: "rx",
			GPSNs: -1, Clock12MHz: 1, ClockCls: model.ClockHW12, Ticks: 1, Epoch: 1}
	}
	h := time.Now().UnixNano()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := fs[i&255]
		f.HostNs = h + int64(i)*1000
		m.HandleFrame(f)
	}
}

func startRawBeastB(b *testing.B) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}
