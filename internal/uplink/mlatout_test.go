package uplink

import (
	"bufio"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/modes"
)

// fakeMLATServer speaks the server side of MLATC.md: it checks the hello,
// answers with zlib (client direction compressed), inflates the client's
// chunked stream and records every message.
type fakeMLATServer struct {
	t      *testing.T
	ln     net.Listener
	mu     sync.Mutex
	hello  map[string]any
	hellos []map[string]any
	msgs   []map[string]json.RawMessage
	conn   net.Conn
	split  bool
}

func startFakeMLAT(t *testing.T, split bool) *fakeMLATServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeMLATServer{t: t, ln: ln, split: split}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *fakeMLATServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeMLATServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *fakeMLATServer) handle(c net.Conn) {
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	var h map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(line)), &h) != nil {
		return
	}
	s.mu.Lock()
	s.hello, s.conn = h, c
	s.hellos = append(s.hellos, h)
	s.mu.Unlock()
	rep, _ := json.Marshal(map[string]any{"compress": "zlib", "split_sync": s.split, "motd": "test server"})
	c.Write(append(rep, '\n'))
	// client direction: u16 length + deflate chunk without 00 00 FF FF
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		var hdr [2]byte
		for {
			if _, err := io.ReadFull(br, hdr[:]); err != nil {
				return
			}
			chunk := make([]byte, binary.BigEndian.Uint16(hdr[:]))
			if _, err := io.ReadFull(br, chunk); err != nil {
				return
			}
			pw.Write(append(chunk, 0, 0, 0xFF, 0xFF))
		}
	}()
	zr, err := zlib.NewReader(pr)
	if err != nil {
		return
	}
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			s.mu.Lock()
			s.msgs = append(s.msgs, m)
			s.mu.Unlock()
		}
	}
}

func (s *fakeMLATServer) send(v string) {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	c.Write([]byte(v + "\n"))
}

func (s *fakeMLATServer) count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.msgs {
		if _, ok := m[kind]; ok {
			n++
		}
	}
	return n
}

func (s *fakeMLATServer) first(kind string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if v, ok := m[kind]; ok {
			return v
		}
	}
	return nil
}

type captureSink struct {
	mu sync.Mutex
	u  []model.DecodedUpdate
}

func (c *captureSink) HandleFrame(model.RawFrame) {}
func (c *captureSink) HandleUpdate(u model.DecodedUpdate) {
	c.mu.Lock()
	c.u = append(c.u, u)
	c.mu.Unlock()
}
func (c *captureSink) n() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.u) }

// The classic even / odd airborne positions of 40621D, and a DF4 reply.
var (
	meEven = uint64(0x58C382D690C8AC)
	meOdd  = uint64(0x58C386435CC412)
)

func df4Reply(icao uint32) []byte {
	f := []byte{4 << 3, 0, 0x0C, 0x38, 0, 0, 0}
	modes.SetParity(f, icao)
	return f
}

func testMLATOut(t *testing.T, split bool) {
	srv := startFakeMLAT(t, split)
	sink := &captureSink{}
	m, err := NewManager(Options{Logf: t.Logf, SDRoot: func() bool { return false }, Inject: sink,
		Position: func() (float64, float64, float64, bool) { return 52.25, 3.92, 30, true }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if notes := m.ApplyMLAT([]MLATUplink{{ID: "net", Host: "127.0.0.1", Port: srv.port(), User: "tester", Privacy: true}}); len(notes) != 0 {
		t.Fatal(notes)
	}
	waitFor(t, 5*time.Second, "connected", func() bool { s := m.MLATStatus(); return len(s) == 1 && s[0].State == StateConnected })
	srv.mu.Lock()
	h := srv.hello
	srv.mu.Unlock()
	if h["clock_type"] != "dump1090" || h["user"] != "tester" || h["privacy"] != true || h["lat"].(float64) != 52.25 || h["return_result_format"] != "ecef" {
		t.Fatalf("hello %v", h)
	}
	for k := range h {
		if strings.Contains(strings.ToLower(k), "mail") {
			t.Fatalf("hello carries %s", k)
		}
	}
	const icao = 0x40621D
	t0 := time.Now()
	frame := func(dt time.Duration, data []byte) model.RawFrame {
		return model.RawFrame{Recv: t0.Add(dt), Kind: model.FrameModeSLong, Data: data, FeedID: "rx", GPSNs: -1,
			Clock12MHz: 1_000_000 + uint64(dt.Microseconds())*12}
	}
	// Not requested yet: only "seen".
	m.HandleFrame(frame(0, df17(icao, meEven)))
	waitFor(t, 3*time.Second, "seen", func() bool { return srv.count("seen") > 0 })
	if srv.count("sync")+srv.count("ssync") != 0 {
		t.Fatal("sync for an aircraft that was never requested")
	}
	srv.send(`{"start_sending":["40621d"]}`)
	waitFor(t, 3*time.Second, "requested", func() bool { return m.MLATStatus()[0].Requested == 1 })
	m.HandleFrame(frame(100*time.Millisecond, df17(icao, meEven)))
	m.HandleFrame(frame(600*time.Millisecond, df17(icao, meOdd)))
	if split {
		waitFor(t, 3*time.Second, "ssync", func() bool { return srv.count("ssync") >= 2 })
		var ss struct {
			T uint64 `json:"t"`
			M string `json:"m"`
		}
		json.Unmarshal(srv.first("ssync"), &ss)
		if ss.T != 1_000_000+100_000*12-768 || len(ss.M) != 28 {
			t.Fatalf("ssync %+v", ss)
		}
	} else {
		waitFor(t, 3*time.Second, "sync", func() bool { return srv.count("sync") == 1 })
		var sy map[string]any
		json.Unmarshal(srv.first("sync"), &sy)
		if sy["em"] == nil || sy["om"] == nil || sy["et"].(float64) >= sy["ot"].(float64) {
			t.Fatalf("sync %v", sy)
		}
	}
	// A Mode S reply while the aircraft has a fresh ADS-B position: no mlat message.
	m.HandleFrame(model.RawFrame{Recv: t0.Add(time.Second), Kind: model.FrameModeSShort, Data: df4Reply(icao), FeedID: "rx", GPSNs: -1, Clock12MHz: 5_000_000})
	// ... but after the position went stale it is sent.
	m.HandleFrame(model.RawFrame{Recv: t0.Add(20 * time.Second), Kind: model.FrameModeSShort, Data: df4Reply(icao), FeedID: "rx", GPSNs: -1, Clock12MHz: 9_000_000})
	waitFor(t, 3*time.Second, "mlat", func() bool { return srv.count("mlat") == 1 })
	// MLAT results (our own magic-timestamp frames) are never forwarded.
	m.HandleFrame(model.RawFrame{Recv: t0.Add(21 * time.Second), Kind: model.FrameModeSShort, Data: df4Reply(icao), FeedID: "rx", MLAT: true, Clock12MHz: 9_100_000})
	// A result comes back and is injected.
	srv.send(`{"result":{"@":` + jsonNum(float64(time.Now().UnixNano())/1e9) + `,"addr":"40621d","ecef":[3870000,265000,5020000],"cov":[100,0,0,100,0,100],"nd":4,"ewvel":300,"nsvel":0,"vrate":-500}}`)
	waitFor(t, 3*time.Second, "result", func() bool { return sink.n() == 1 })
	sink.mu.Lock()
	u := sink.u[0]
	sink.mu.Unlock()
	if !u.MLAT || u.ICAO != icao || u.Src != model.SrcMLATExt || !IsInjectedFeed(u.FeedID) || u.Lat == nil || *u.GS < 299 || *u.Track != 90 || *u.VertRate != -500 || u.SigmaM == nil {
		t.Fatalf("%+v", u)
	}
	time.Sleep(300 * time.Millisecond)
	if n := srv.count("mlat"); n != 1 {
		t.Fatalf("%d mlat messages (an MLAT frame was forwarded?)", n)
	}
	st := m.MLATStatus()[0]
	if st.Results != 1 || st.MOTD != "test server" || st.Compress != "zlib" || st.SplitSync != split {
		t.Fatalf("%+v", st)
	}
}

func TestMLATOutPairs(t *testing.T)     { testMLATOut(t, false) }
func TestMLATOutSplitSync(t *testing.T) { testMLATOut(t, true) }

func jsonNum(f float64) string { b, _ := json.Marshal(f); return string(b) }

func TestMLATOutDeniedAndNoPosition(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		bufio.NewReader(c).ReadString('\n')
		c.Write([]byte(`{"deny":["too many connections"],"reconnect_in":600}` + "\n"))
		c.Close()
	}()
	m, err := NewManager(Options{Logf: t.Logf, SDRoot: func() bool { return false },
		Position: func() (float64, float64, float64, bool) { return 52, 4, 0, true }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.ApplyMLAT([]MLATUplink{{ID: "net", Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}})
	waitFor(t, 5*time.Second, "backoff", func() bool { s := m.MLATStatus()[0]; return s.State == StateBackoff })
	st := m.MLATStatus()[0]
	if len(st.Denied) != 1 || time.Until(st.NextRetry) < 9*time.Minute {
		t.Fatalf("%+v", st)
	}
	// Without a position nothing connects.
	m2, _ := NewManager(Options{Logf: t.Logf, SDRoot: func() bool { return false }})
	defer m2.Close()
	m2.ApplyMLAT([]MLATUplink{{ID: "x", Host: "127.0.0.1", Port: 1}})
	waitFor(t, 5*time.Second, "no position", func() bool { s := m2.MLATStatus()[0]; return strings.Contains(s.LastError, "position") })
}

func TestSplitMLAT(t *testing.T) {
	ups := []model.Uplink{{ID: "a", Host: "h", Port: 30160}, {ID: "b", Type: model.UplinkBeast, Host: "h"}, {ID: "c", Type: model.UplinkMLAT, Host: "h", Port: 31090, User: "me", Pin: model.PinRegion}}
	avf, beast := SplitUplinks(ups)
	ml := SplitMLAT(ups)
	if len(avf) != 1 || len(beast) != 1 || len(ml) != 1 || ml[0].User != "me" || ml[0].Privacy {
		t.Fatalf("%v %v %v", avf, beast, ml)
	}
}

// TestMLATOutGPSClock: a Radarcape in GPS mode (class gps_utc) takes part
// with clock_type "radarcape_gps": the output reconnects with that type when
// only GPS frames arrive, sends nanoseconds of the UTC day moved to the start
// of the frame (MLATC.md §5.1, wrapping at midnight), and skips GPS frames
// without lock.
func TestMLATOutGPSClock(t *testing.T) {
	srv := startFakeMLAT(t, true)
	m, err := NewManager(Options{Logf: t.Logf, SDRoot: func() bool { return false }, Inject: &captureSink{},
		Position: func() (float64, float64, float64, bool) { return 52.25, 3.92, 30, true }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.ApplyMLAT([]MLATUplink{{ID: "net", Host: "127.0.0.1", Port: srv.port()}})
	waitFor(t, 5*time.Second, "connected", func() bool { s := m.MLATStatus(); return len(s) == 1 && s[0].State == StateConnected })
	srv.mu.Lock()
	ct := srv.hello["clock_type"]
	srv.mu.Unlock()
	if ct != "dump1090" {
		t.Fatalf("first hello before any frame: %v", ct)
	}
	const icao = 0x40621D
	gps := func(ns int64, cls model.ClockClass, data []byte) model.RawFrame {
		// Recv two minutes ahead of the session start: no frame of the session's clock for over a minute
		return model.RawFrame{Recv: time.Now().Add(2 * time.Minute), Kind: model.FrameModeSLong, Data: data, FeedID: "rc", GPSNs: ns, ClockCls: cls}
	}
	srv.send(`{"start_sending":["40621d"]}`)
	waitFor(t, 3*time.Second, "requested", func() bool { return m.MLATStatus()[0].Requested == 1 })
	m.HandleFrame(gps(1_000_000_000, model.ClockGPSUTC, df17(icao, meEven)))
	m.HandleFrame(gps(1_000_000_000, model.ClockGPSUTC, df17(icao, meEven)))
	waitFor(t, 5*time.Second, "reconnect as radarcape_gps", func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.hellos) == 2 && srv.hellos[1]["clock_type"] == "radarcape_gps"
	})
	waitFor(t, 3*time.Second, "connected again", func() bool {
		s := m.MLATStatus()[0]
		return s.State == StateConnected && s.ClockType == "radarcape_gps"
	})
	if s := m.MLATStatus()[0]; s.OtherClock == 0 {
		t.Fatalf("other-clock frames not counted: %+v", s)
	}
	srv.send(`{"start_sending":["40621d"]}`)
	waitFor(t, 3*time.Second, "requested again", func() bool { return m.MLATStatus()[0].Requested == 1 })
	before := srv.count("ssync")
	m.HandleFrame(gps(1_000_000_000, model.ClockGPSUTC, df17(icao, meEven)))
	m.HandleFrame(gps(50_000, model.ClockGPSUTC, df17(icao, meOdd))) // 50 µs after midnight: wraps to the previous day
	m.HandleFrame(gps(2_000_000_000, model.ClockGPSEmul, df17(icao, meEven)))
	waitFor(t, 3*time.Second, "ssync", func() bool { return srv.count("ssync") >= before+2 })
	srv.mu.Lock()
	var ts []uint64
	for _, msg := range srv.msgs {
		if v, ok := msg["ssync"]; ok {
			var ss struct {
				T uint64 `json:"t"`
			}
			json.Unmarshal(v, &ss)
			ts = append(ts, ss.T)
		}
	}
	srv.mu.Unlock()
	got := ts[len(ts)-2:]
	if got[0] != 1_000_000_000-120_000 || got[1] != uint64(model.NsPerDay)-70_000 {
		t.Fatalf("GPS stamps %v", got)
	}
	time.Sleep(300 * time.Millisecond)
	if s := m.MLATStatus()[0]; s.NoGPSLock != 1 || srv.count("ssync") != before+2 {
		t.Fatalf("emulated GPS frame: %+v, %d ssync", s, srv.count("ssync")-before)
	}
}
