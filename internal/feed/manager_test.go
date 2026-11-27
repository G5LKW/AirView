package feed

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/hub"
	"airview/internal/model"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// safeSink is a concurrency-safe collecting sink.
type safeSink struct {
	mu      sync.Mutex
	frames  []model.RawFrame
	updates []model.DecodedUpdate
}

func (s *safeSink) HandleFrame(f model.RawFrame) {
	s.mu.Lock()
	s.frames = append(s.frames, f)
	s.mu.Unlock()
}

func (s *safeSink) HandleUpdate(u model.DecodedUpdate) {
	s.mu.Lock()
	s.updates = append(s.updates, u)
	s.mu.Unlock()
}

func (s *safeSink) nFrames() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

func (s *safeSink) nUpdates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.updates)
}

func (s *safeSink) frame(i int) model.RawFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames[i]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testManager(t *testing.T, sink model.FrameSink) *Manager {
	t.Helper()
	m := NewWithOptions(sink, Options{
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond,
		IdleTimeout: 2 * time.Second, JSONInterval: 30 * time.Millisecond, Logger: quiet,
	})
	t.Cleanup(m.Close)
	return m
}

// tcpSource is a test server: every accepted connection is handed to serve.
type tcpSource struct {
	ln       net.Listener
	accepted atomic.Int32
	mu       sync.Mutex
	conns    []net.Conn
}

func newTCPSource(t *testing.T, serve func(conn net.Conn, n int)) *tcpSource {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &tcpSource{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n := int(s.accepted.Add(1))
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			go serve(conn, n)
		}
	}()
	t.Cleanup(s.close)
	return s
}

func (s *tcpSource) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *tcpSource) close() {
	s.ln.Close()
	s.closeConns()
}

func (s *tcpSource) closeConns() {
	s.mu.Lock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
	s.mu.Unlock()
}

func status(m *Manager, id string) (model.FeedStatus, bool) {
	for _, st := range m.Status() {
		if st.ID == id {
			return st, true
		}
	}
	return model.FeedStatus{}, false
}

// radarcapeBurst is what a Radarcape sends right after connect in the tests.
func radarcapeBurst() []byte {
	var b []byte
	b = AppendBeastFrame(b, BeastStatusType, GPSTimestamp(43200, 0), 0, statusPayload(0x90, 1, 0x9F))
	b = AppendBeastFrame(b, BeastPosition, 0, 0, positionPayload(48.25, 11.5, 520))
	b = AppendBeastFrame(b, BeastModeSLong, GPSTimestamp(43200, 1000), 200, long1)
	b = AppendBeastFrame(b, BeastModeSShort, GPSTimestamp(43200, 2000), 100, short1)
	b = AppendBeastFrame(b, BeastModeSLong, GPSTimestamp(43200, 3000), 50, long2)
	return b
}

func TestManagerBeastLifecycle(t *testing.T) {
	src := newTCPSource(t, func(conn net.Conn, n int) {
		conn.Write(radarcapeBurst())
		io.Copy(io.Discard, conn) // hold the connection open
	})
	sink := &safeSink{}
	m := testManager(t, sink)
	var (
		posMu sync.Mutex
		pos   []ReceiverPosition
	)
	m.OnReceiverPosition(func(id string, p ReceiverPosition) {
		posMu.Lock()
		pos = append(pos, p)
		posMu.Unlock()
	})

	rc := model.Feed{ID: "rc", Name: "Radarcape", Enabled: true, Type: model.FeedBeast, Host: "127.0.0.1", Port: src.port()}
	off := model.Feed{ID: "off", Name: "Disabled", Enabled: false, Type: model.FeedSBS, Host: "127.0.0.1", Port: 1}
	m.Apply([]model.Feed{rc, off})

	waitFor(t, "first burst", func() bool { return sink.nFrames() == 4 })
	st, _ := status(m, "rc")
	if !st.Connected || !st.Enabled || st.Type != model.FeedBeast || st.Name != "Radarcape" || st.Frames != 4 ||
		st.Bytes != uint64(len(radarcapeBurst())) || !st.GPSTime || st.LastError != "" || st.LastFrame == 0 || st.Since == 0 {
		t.Errorf("status = %+v", st)
	}
	if !strings.Contains(st.Info, "Radarcape") || !strings.Contains(st.Info, "GPS locked") || !strings.Contains(st.Info, "48.25000") {
		t.Errorf("info = %q", st.Info)
	}
	if !strings.HasPrefix(st.Addr, "127.0.0.1:") {
		t.Errorf("addr = %q", st.Addr)
	}
	all := m.Status()
	if len(all) != 2 || all[0].ID != "rc" || all[1].ID != "off" || all[1].Enabled || all[1].Connected {
		t.Errorf("Status() = %+v", all)
	}
	if f := sink.frame(1); f.Kind != model.FrameModeSLong || f.GPSNs != 43200*1e9+1000 || f.FeedID != "rc" || f.Channel != -1 {
		t.Errorf("frame = %+v", f)
	}
	if f := sink.frame(0); f.Kind != model.FrameStatus {
		t.Errorf("first frame kind = %v, want status", f.Kind)
	}
	posMu.Lock()
	if len(pos) != 1 || pos[0].Lat != 48.25 {
		t.Errorf("receiver positions = %+v", pos)
	}
	posMu.Unlock()

	// Applying the same configuration again must not disturb the feed.
	m.Apply([]model.Feed{rc, off})
	time.Sleep(100 * time.Millisecond)
	if n := src.accepted.Load(); n != 1 {
		t.Fatalf("unchanged Apply reconnected the feed (%d connections)", n)
	}

	// The peer drops the connection: reconnect with backoff, counters continue.
	src.closeConns()
	waitFor(t, "reconnect", func() bool { return src.accepted.Load() >= 2 && sink.nFrames() == 8 })
	if st, _ = status(m, "rc"); st.Frames != 8 {
		t.Errorf("frames after reconnect = %d", st.Frames)
	}

	// A changed definition restarts the feed (fresh counters).
	rc2 := rc
	rc2.Name = "Renamed"
	rc2.MLATOnly = true
	m.Apply([]model.Feed{rc2, off})
	waitFor(t, "restart", func() bool { st, _ := status(m, "rc"); return st.Name == "Renamed" && st.Frames == 4 && st.Connected })
	if f := sink.frame(sink.nFrames() - 1); !f.MLAT {
		t.Error("MLATOnly feed delivered a frame without the MLAT flag")
	}
	if f := sink.frame(sink.nFrames() - 4); f.Kind != model.FrameStatus || f.MLAT {
		t.Errorf("status frame of an MLATOnly feed = %+v (must not be flagged MLAT)", f)
	}

	// Disable: disconnects and stays listed.
	rc3 := rc2
	rc3.Enabled = false
	before := sink.nFrames()
	m.Apply([]model.Feed{rc3, off})
	waitFor(t, "disable", func() bool { st, _ := status(m, "rc"); return !st.Connected && !st.Enabled })
	accepted := src.accepted.Load()
	time.Sleep(150 * time.Millisecond)
	if src.accepted.Load() != accepted || sink.nFrames() != before {
		t.Error("disabled feed is still active")
	}

	// Remove.
	m.Apply([]model.Feed{off})
	if all := m.Status(); len(all) != 1 || all[0].ID != "off" {
		t.Errorf("Status() after removal = %+v", all)
	}
}

func TestManagerReconnectOnRefusedAndIdle(t *testing.T) {
	// A port nobody listens on: the feed reports the error and keeps trying.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	silent := newTCPSource(t, func(conn net.Conn, n int) { io.Copy(io.Discard, conn) })
	sink := &safeSink{}
	m := NewWithOptions(sink, Options{MinBackoff: 10 * time.Millisecond, MaxBackoff: 30 * time.Millisecond,
		IdleTimeout: 150 * time.Millisecond, DialTimeout: 2 * time.Second, Logger: quiet})
	defer m.Close()
	m.Apply([]model.Feed{
		{ID: "dead", Enabled: true, Type: model.FeedBeast, Host: "127.0.0.1", Port: port},
		{ID: "silent", Enabled: true, Type: model.FeedSBS, Host: "127.0.0.1", Port: silent.port()},
		{ID: "nohost", Enabled: true, Type: model.FeedAVR},
		{ID: "weird", Enabled: true, Type: "carrier-pigeon", Host: "x"},
	})
	waitFor(t, "connection error", func() bool { st, _ := status(m, "dead"); return st.LastError != "" })
	if st, _ := status(m, "dead"); st.Connected {
		t.Errorf("dead feed connected: %+v", st)
	}
	// A link that delivers nothing is torn down and re-established.
	waitFor(t, "idle reconnects", func() bool { return silent.accepted.Load() >= 3 })
	waitFor(t, "idle error text", func() bool {
		st, _ := status(m, "silent")
		return strings.Contains(st.LastError, "no data for")
	})
	if st, _ := status(m, "nohost"); st.Connected || !strings.Contains(st.LastError, "no host") {
		t.Errorf("nohost status = %+v", st)
	}
	if st, _ := status(m, "weird"); st.Connected || !strings.Contains(st.LastError, "unknown feed type") {
		t.Errorf("unknown type status = %+v", st)
	}

	// Close stops everything promptly.
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	n := silent.accepted.Load()
	time.Sleep(100 * time.Millisecond)
	if silent.accepted.Load() != n {
		t.Error("feed reconnected after Close")
	}
	m.Apply([]model.Feed{{ID: "late", Enabled: true, Type: model.FeedBeast, Host: "127.0.0.1", Port: port}})
	if len(m.Status()) != 4 {
		t.Error("Apply after Close changed the feed set")
	}
}

func TestManagerContextStop(t *testing.T) {
	src := newTCPSource(t, func(conn net.Conn, n int) {
		conn.Write(radarcapeBurst())
		io.Copy(io.Discard, conn)
	})
	sink := &safeSink{}
	m := testManager(t, sink)
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	m.Apply([]model.Feed{{ID: "rc", Enabled: true, Type: model.FeedBeast, Host: "127.0.0.1", Port: src.port()}})
	waitFor(t, "frames", func() bool { return sink.nFrames() == 4 })
	cancel()
	waitFor(t, "stop on context cancel", func() bool { st, _ := status(m, "rc"); return !st.Connected })
	m.Close() // waits for the goroutines; must not hang
}

func TestManagerFormatSniffAndAVR(t *testing.T) {
	// The feed is configured as Beast but the port speaks AVR text (a
	// Radarcape raw port can be switched): the stream decides.
	avr := newTCPSource(t, func(conn net.Conn, n int) {
		io.WriteString(conn, "*8D4840D6202CC371C32CE0576098;\r\n*5D4840D6202CC3;\r\n@016CE3671C7423FFE7AB7BFCAB;\r\n")
		io.Copy(io.Discard, conn)
	})
	// ... and the other way round.
	beast := newTCPSource(t, func(conn net.Conn, n int) {
		conn.Write(radarcapeBurst())
		io.Copy(io.Discard, conn)
	})
	sink := &safeSink{}
	m := testManager(t, sink)
	m.Apply([]model.Feed{
		{ID: "a", Enabled: true, Type: model.FeedBeast, Host: "127.0.0.1", Port: avr.port()},
		{ID: "b", Enabled: true, Type: model.FeedAVR, Host: "127.0.0.1", Port: beast.port()},
	})
	waitFor(t, "frames from both", func() bool { return sink.nFrames() == 7 })
	sa, _ := status(m, "a")
	sb, _ := status(m, "b")
	if sa.Frames != 3 || !strings.Contains(sa.Info, "AVR") {
		t.Errorf("AVR-on-beast feed: %+v", sa)
	}
	if sb.Frames != 4 || !strings.Contains(sb.Info, "Beast") || !sb.GPSTime {
		t.Errorf("Beast-on-AVR feed: %+v", sb)
	}
}

func TestManagerSBSAndJSON(t *testing.T) {
	sbs := newTCPSource(t, func(conn net.Conn, n int) {
		io.WriteString(conn, "MSG,3,1,1,4840D6,1,2026/11/11,12:00:01.123,2026/11/11,12:00:01.150,,38000,,,52.25720,3.91937,,,0,,0,0\r\n"+
			"MSG,1,1,1,4840D6,1,2026/11/11,12:00:00.412,2026/11/11,12:00:00.440,KLM1023 ,,,,,,,,,,,0\r\n")
		io.Copy(io.Discard, conn)
	})
	var polls atomic.Int32
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the readsb path exists: the poller has to find it.
		if r.URL.Path != "/data/aircraft.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if polls.Add(1) == 1 {
			io.WriteString(w, readsbDoc1)
		} else {
			io.WriteString(w, readsbDoc2)
		}
	}))
	defer web.Close()

	sink := &safeSink{}
	m := testManager(t, sink)
	m.Apply([]model.Feed{
		{ID: "sbs", Enabled: true, Type: model.FeedSBS, Host: "127.0.0.1", Port: sbs.port()},
		{ID: "json", Enabled: true, Type: model.FeedJSON, URL: web.URL},
		{ID: "badjson", Enabled: true, Type: model.FeedJSON},
	})
	// 2 SBS updates + 4 from the first JSON poll + 2 from the second.
	waitFor(t, "updates", func() bool { return sink.nUpdates() == 8 })
	waitFor(t, "more polls", func() bool { return polls.Load() >= 4 })
	if n := sink.nUpdates(); n != 8 {
		t.Errorf("%d updates after repeated identical polls, want 8", n)
	}
	if st, _ := status(m, "sbs"); !st.Connected || st.Frames != 2 || st.Bytes == 0 {
		t.Errorf("sbs status = %+v", st)
	}
	st, _ := status(m, "json")
	if !st.Connected || st.Frames != 6 || st.Bytes == 0 || !strings.HasSuffix(st.Addr, "/data/aircraft.json") || !strings.Contains(st.Info, "aircraft.json") {
		t.Errorf("json status = %+v", st)
	}
	if st, _ := status(m, "badjson"); st.Connected || st.LastError == "" {
		t.Errorf("json feed without address: %+v", st)
	}

	sink.mu.Lock()
	feeds := map[string]int{}
	for _, u := range sink.updates {
		feeds[u.FeedID]++
	}
	sink.mu.Unlock()
	if feeds["sbs"] != 2 || feeds["json"] != 6 {
		t.Errorf("updates per feed = %v", feeds)
	}

	// The document disappears: the feed reports it and recovers afterwards.
	web.Close()
	waitFor(t, "json failure", func() bool { st, _ := status(m, "json"); return !st.Connected && st.LastError != "" })
}

func TestManagerRegisterType(t *testing.T) {
	sink := &safeSink{}
	m := testManager(t, sink)
	demo := model.Feed{ID: "demo", Name: "Simulator", Enabled: true, Type: model.FeedDemo}
	m.Apply([]model.Feed{demo})
	if st, _ := status(m, "demo"); st.Connected || !strings.Contains(st.LastError, "unknown feed type") {
		t.Fatalf("status before registration = %+v", st)
	}

	var runs atomic.Int32
	m.RegisterType(model.FeedDemo, func(cfg model.Feed) (Runner, error) {
		return RunnerFunc(func(ctx context.Context, s *Session) error {
			n := runs.Add(1)
			if n == 1 {
				return nil // a runner that ends is restarted
			}
			if n == 2 {
				panic("simulated bug") // and so is one that panics
			}
			s.SetConnected("simulator")
			s.SetGPSTime(true)
			s.SetInfo("60 simulated aircraft")
			s.AddBytes(23)
			s.HandleFrame(model.RawFrame{GPSNs: 5, Kind: model.FrameModeSLong, Data: long1, Channel: 1})
			s.HandleUpdate(model.DecodedUpdate{ICAO: 0x4840D6})
			<-ctx.Done()
			return ctx.Err()
		}), nil
	})
	waitFor(t, "demo feed", func() bool { return sink.nFrames() == 1 && sink.nUpdates() == 1 })
	st, _ := status(m, "demo")
	if !st.Connected || !st.GPSTime || st.Info != "60 simulated aircraft" || st.Frames != 2 || st.Bytes != 23 || st.Addr != "simulator" || st.LastError != "" {
		t.Errorf("demo status = %+v", st)
	}
	f := sink.frame(0)
	if f.FeedID != "demo" || f.Channel != 1 || f.Recv.IsZero() {
		t.Errorf("frame = %+v (FeedID and Recv must be filled in)", f)
	}
	sink.mu.Lock()
	u := sink.updates[0]
	sink.mu.Unlock()
	if u.FeedID != "demo" || u.Msgs != 1 || u.Recv.IsZero() {
		t.Errorf("update = %+v", u)
	}
	if runs.Load() != 3 {
		t.Errorf("runner started %d times, want 3", runs.Load())
	}
}

func TestSessionRate(t *testing.T) {
	m := testManager(t, &safeSink{})
	s := newSession(m, model.Feed{ID: "x", Type: model.FeedBeast})
	t0 := time.Unix(1_800_000_000, 0)
	s.tick(t0)
	for i := 1; i <= 30; i++ {
		s.frames.Add(1000)
		s.tick(t0.Add(time.Duration(i) * time.Second))
	}
	if r := s.status().MsgRate; r < 999 || r > 1001 {
		t.Errorf("rate = %v, want 1000", r)
	}
	for i := 31; i <= 80; i++ {
		s.tick(t0.Add(time.Duration(i) * time.Second))
	}
	if r := s.status().MsgRate; r != 0 {
		t.Errorf("rate after silence = %v, want 0", r)
	}
}

func TestBackoff(t *testing.T) {
	b := backoff{min: time.Second, max: 30 * time.Second}
	nominal := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, n := range nominal {
		d := b.next()
		if lo, hi := n*time.Second/2, n*time.Second; d < lo || d > hi {
			t.Errorf("delay %d = %v, want within [%v, %v]", i, d, lo, hi)
		}
	}
	b.reset()
	if d := b.next(); d > time.Second {
		t.Errorf("delay after reset = %v", d)
	}
}

func TestManagerHubFeed(t *testing.T) {
	srv := hub.NewServer(hub.Options{Port: -1, BindAddr: "127.0.0.1", Password: "pw", Logger: quiet, FlushInterval: time.Millisecond, FailLimit: 1000})
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	port := srv.Addr().(*net.TCPAddr).Port
	rx := model.Receiver{Name: "Pi host", Model: "Radarcape EVO 2CH", Located: true, Lat: 51.5, Lon: -0.12}
	srv.SetStation(rx, model.DefaultConfig().Antennas, hub.StationFeeds(model.DefaultConfig().Feeds))

	sink := &safeSink{}
	m := testManager(t, sink)
	var (
		stMu     sync.Mutex
		stations []hub.Station
		from     string
	)
	m.OnStation(func(id string, st hub.Station) {
		stMu.Lock()
		stations, from = append(stations, st), id
		stMu.Unlock()
	})
	feed := model.Feed{ID: "host", Name: "Pi", Enabled: true, Type: model.FeedHub, Host: "127.0.0.1", Port: port}

	// Wrong (empty) password first.
	m.Apply([]model.Feed{feed})
	waitFor(t, "auth failure", func() bool {
		st, _ := status(m, "host")
		return !st.Connected && strings.Contains(st.LastError, "authentication failed")
	})

	m.SetHubOptions(HubOptions{Password: "pw"})
	waitFor(t, "hub login", func() bool {
		st, _ := status(m, "host")
		return st.Connected && srv.Status().Clients == 1
	})
	waitFor(t, "station", func() bool { stMu.Lock(); defer stMu.Unlock(); return len(stations) == 1 })
	stMu.Lock()
	if from != "host" || stations[0].Receiver != rx || len(stations[0].Antennas) != len(model.DefaultConfig().Antennas) || len(stations[0].Feeds) != 1 {
		t.Errorf("station from %q = %+v", from, stations[0])
	}
	stMu.Unlock()

	orig := model.RawFrame{Recv: time.Now(), GPSNs: 43_200_000_001_000, Signal: -7.5, Kind: model.FrameModeSLong,
		Data: long1, FeedID: "radarcape", Channel: 1}
	srv.Broadcast(orig)
	srv.BroadcastUpdate(model.DecodedUpdate{Recv: time.Now(), FeedID: "sbs", ICAO: 0x4840D6, AltBaro: model.F(38000), Msgs: 2})
	waitFor(t, "relayed data", func() bool { return sink.nFrames() == 1 && sink.nUpdates() == 1 })
	f := sink.frame(0)
	if f.FeedID != "host" || f.GPSNs != orig.GPSNs || f.Signal != orig.Signal || f.Channel != 1 || f.Kind != orig.Kind || string(f.Data) != string(long1) {
		t.Errorf("relayed frame = %+v", f)
	}
	st, _ := status(m, "host")
	if st.Frames != 2 || st.Bytes == 0 || !st.GPSTime || !strings.Contains(st.Info, "Pi host") {
		t.Errorf("hub feed status = %+v", st)
	}

	// Keeping the host's feed IDs is an option; changing it reconnects.
	m.SetHubOptions(HubOptions{Password: "pw", KeepHostFeedIDs: true})
	waitFor(t, "relogin", func() bool {
		stMu.Lock()
		defer stMu.Unlock()
		return len(stations) == 2 && srv.Status().Clients == 1
	})
	waitFor(t, "connected", func() bool { st, _ := status(m, "host"); return st.Connected })
	srv.Broadcast(orig)
	waitFor(t, "second frame", func() bool { return sink.nFrames() == 2 })
	if f := sink.frame(1); f.FeedID != "radarcape" {
		t.Errorf("FeedID with KeepHostFeedIDs = %q", f.FeedID)
	}
}
