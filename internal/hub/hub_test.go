package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// collector is a model.FrameSink that records everything.
type collector struct {
	mu      sync.Mutex
	frames  []model.RawFrame
	updates []model.DecodedUpdate
}

func (c *collector) HandleFrame(f model.RawFrame) {
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
}

func (c *collector) HandleUpdate(u model.DecodedUpdate) {
	c.mu.Lock()
	c.updates = append(c.updates, u)
	c.mu.Unlock()
}

func (c *collector) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames), len(c.updates)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startServer(t *testing.T, opts Options) (*Server, int) {
	t.Helper()
	opts.Port = -1
	opts.BindAddr = "127.0.0.1"
	opts.Logger = quiet
	if opts.FlushInterval == 0 {
		opts.FlushInterval = time.Millisecond
	}
	s := NewServer(opts)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, s.Addr().(*net.TCPAddr).Port
}

type stateRec struct {
	mu     sync.Mutex
	states []error // nil = connected
	conn   int
}

func (r *stateRec) on(connected bool, _ string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if connected {
		r.conn++
		r.states = append(r.states, nil)
	} else {
		r.states = append(r.states, err)
	}
}

func (r *stateRec) lastErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.states) - 1; i >= 0; i-- {
		if r.states[i] != nil {
			return r.states[i]
		}
	}
	return nil
}

func (r *stateRec) connects() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn
}

func runClient(t *testing.T, opts ClientOptions, sink model.FrameSink) (*Client, context.CancelFunc) {
	t.Helper()
	opts.Host = "127.0.0.1"
	opts.Logger = quiet
	if opts.MinBackoff == 0 {
		opts.MinBackoff = 20 * time.Millisecond
		opts.MaxBackoff = 50 * time.Millisecond
	}
	c := NewClient(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, sink)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("client did not stop after context cancel")
		}
	}
	t.Cleanup(stop)
	return c, stop
}

func sameFloat(a, b float64) bool {
	return (math.IsNaN(a) && math.IsNaN(b)) || a == b
}

func testFrames() []model.RawFrame {
	now := time.Now()
	return []model.RawFrame{
		{Recv: now, GPSNs: 43_200_123_456_789, Signal: -12.5, Kind: model.FrameModeSLong,
			Data:   []byte{0x8D, 0x40, 0x62, 0x1D, 0x58, 0xC3, 0x82, 0xD6, 0x90, 0xC8, 0xAC, 0x28, 0x63, 0xA7},
			FeedID: "radarcape", Channel: 1},
		{Recv: now, GPSNs: -1, Clock12MHz: 0xABCDEF012345, Signal: math.NaN(), Kind: model.FrameModeSShort,
			Data: []byte{0x5D, 0x48, 0x40, 0xD6, 0x20, 0x2C, 0xC3}, FeedID: "dump1090", Channel: -1},
		{Recv: now, GPSNs: 0, Signal: -0.25, Kind: model.FrameModeAC, Data: []byte{0x77, 0x00}, FeedID: "radarcape", Channel: 0},
		{Recv: now, GPSNs: -1, Signal: math.NaN(), Kind: model.FrameModeSLong, MLAT: true,
			Data: bytes.Repeat([]byte{0x1A}, 14), FeedID: "mlat", Channel: -1},
		{Recv: now, GPSNs: 86_399_999_999_999, Signal: math.NaN(), Kind: model.FrameStatus,
			Data: []byte{0x10, 0xFE, 0x8F, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, FeedID: "radarcape", Channel: -1},
		{Recv: now, GPSNs: -1, Signal: -30, Kind: model.FrameModeSShort, Data: []byte{1, 2, 3, 4, 5, 6, 7}, FeedID: "", Channel: -1},
	}
}

func testUpdates() []model.DecodedUpdate {
	now := time.Now()
	return []model.DecodedUpdate{
		{Recv: now, FeedID: "sbs", ICAO: 0x4840D6, Callsign: model.S("KLM1023"), Squawk: model.S("7000"),
			Lat: model.F(52.2572), Lon: model.F(3.9194), AltBaro: model.F(38000), AltGeom: model.F(38450),
			GS: model.F(447.5), Track: model.F(271.3), VertRate: model.F(-64), OnGround: model.B(false),
			Signal: model.F(-18.4), Msgs: 7},
		{Recv: now, FeedID: "json", ICAO: 0x1ABCDEF, MLAT: true, OnGround: model.B(true), Msgs: 1},
		{Recv: now, FeedID: "sbs", ICAO: 0x3C6DD2, AltBaro: model.F(-125), Msgs: 3},
	}
}

func TestLoopbackRoundTrip(t *testing.T) {
	srv, port := startServer(t, Options{Password: "s3cret"})
	rx := model.Receiver{Name: "Pi", Model: "Radarcape EVO 2CH", Located: true, Lat: 51.5, Lon: -0.12, GroundElevM: 35}
	ants := model.DefaultConfig().Antennas
	feeds := StationFeeds([]model.Feed{
		{ID: "radarcape", Name: "Radarcape", Enabled: true, Type: model.FeedBeast, AntennaID: "yagi"},
		{ID: "off", Enabled: false, Type: model.FeedSBS},
	})
	srv.SetStation(rx, ants, feeds)

	var (
		stMu     sync.Mutex
		stations []Station
	)
	sink := &collector{}
	states := &stateRec{}
	client, _ := runClient(t, ClientOptions{
		Port: port, Password: "s3cret", OnState: states.on,
		OnStation: func(s Station) {
			stMu.Lock()
			stations = append(stations, s)
			stMu.Unlock()
		},
	}, sink)

	nStations := func() int {
		stMu.Lock()
		defer stMu.Unlock()
		return len(stations)
	}
	waitFor(t, "login + station", func() bool { return client.Connected() && nStations() == 1 })
	if st := srv.Status(); !st.Enabled || st.Port != port || st.Clients != 1 {
		t.Fatalf("server status = %+v", st)
	}
	stMu.Lock()
	st := stations[0]
	stMu.Unlock()
	if st.App != model.AppName || st.Version != model.Version || st.Proto != ProtocolVersion {
		t.Errorf("station header = %q %q %d", st.App, st.Version, st.Proto)
	}
	if st.Receiver != rx {
		t.Errorf("receiver = %+v, want %+v", st.Receiver, rx)
	}
	if len(st.Antennas) != len(ants) || st.Antennas[1] != ants[1] {
		t.Errorf("antennas = %+v", st.Antennas)
	}
	if len(st.Feeds) != 1 || st.Feeds[0].ID != "radarcape" || st.Feeds[0].AntennaID != "yagi" {
		t.Errorf("station feeds = %+v", st.Feeds)
	}

	wantF, wantU := testFrames(), testUpdates()
	for _, f := range wantF {
		// The server must not retain the caller's data slice: hand it a
		// scratch copy and scribble over it afterwards.
		tmp := append([]byte(nil), f.Data...)
		f.Data = tmp
		srv.Broadcast(f)
		for i := range tmp {
			tmp[i] = 0xEE
		}
	}
	for _, u := range wantU {
		srv.HandleUpdate(u)
	}
	waitFor(t, "records", func() bool {
		f, u := sink.counts()
		return f == len(wantF) && u == len(wantU)
	})

	sink.mu.Lock()
	defer sink.mu.Unlock()
	for i, w := range wantF {
		g := sink.frames[i]
		if g.Kind != w.Kind || g.GPSNs != w.GPSNs || g.Clock12MHz != w.Clock12MHz || !sameFloat(g.Signal, w.Signal) ||
			g.FeedID != w.FeedID || g.Channel != w.Channel || g.MLAT != w.MLAT || !bytes.Equal(g.Data, w.Data) {
			t.Errorf("frame %d:\n got %+v\nwant %+v", i, g, w)
		}
		if d := time.Since(g.Recv); d < 0 || d > 10*time.Second {
			t.Errorf("frame %d: Recv %v is not close to now (age %v)", i, g.Recv, d)
		}
	}
	// Host frame spacing is preserved exactly (all test frames share one Recv).
	if !sink.frames[0].Recv.Equal(sink.frames[1].Recv) {
		t.Errorf("Recv of frames with equal host time differ: %v vs %v", sink.frames[0].Recv, sink.frames[1].Recv)
	}
	ptrEq := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	strEq := func(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	for i, w := range wantU {
		g := sink.updates[i]
		ok := g.FeedID == w.FeedID && g.ICAO == w.ICAO && g.MLAT == w.MLAT && g.Msgs == w.Msgs &&
			strEq(g.Callsign, w.Callsign) && strEq(g.Squawk, w.Squawk) &&
			ptrEq(g.Lat, w.Lat) && ptrEq(g.Lon, w.Lon) && ptrEq(g.AltBaro, w.AltBaro) && ptrEq(g.AltGeom, w.AltGeom) &&
			ptrEq(g.GS, w.GS) && ptrEq(g.Track, w.Track) && ptrEq(g.VertRate, w.VertRate) && ptrEq(g.Signal, w.Signal) &&
			(g.OnGround == nil) == (w.OnGround == nil) && (g.OnGround == nil || *g.OnGround == *w.OnGround)
		if !ok {
			t.Errorf("update %d:\n got %+v\nwant %+v", i, g, w)
		}
	}

	// A station change is pushed to connected clients; an identical one is not.
	srv.SetStation(rx, ants, feeds)
	rx.Lat = 48.1
	srv.SetStation(rx, ants, feeds)
	waitFor(t, "station update", func() bool { return nStations() == 2 })
	stMu.Lock()
	if stations[1].Receiver.Lat != 48.1 {
		t.Errorf("updated station lat = %v", stations[1].Receiver.Lat)
	}
	stMu.Unlock()
	if got, ok := client.Station(); !ok || got.Receiver.Lat != 48.1 {
		t.Errorf("Client.Station() = %+v, %v", got.Receiver, ok)
	}
}

func TestFeedIDOverrideAndOpenServer(t *testing.T) {
	srv, port := startServer(t, Options{})
	sink := &collector{}
	// A client that has a password can still use an open host.
	client, _ := runClient(t, ClientOptions{Port: port, Password: "whatever", FeedID: "host"}, sink)
	waitFor(t, "login", client.Connected)
	waitFor(t, "registration", func() bool { return srv.Status().Clients == 1 })
	for _, f := range testFrames() {
		srv.Broadcast(f)
	}
	srv.BroadcastUpdate(testUpdates()[0])
	waitFor(t, "records", func() bool { f, u := sink.counts(); return f == len(testFrames()) && u == 1 })
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for i, f := range sink.frames {
		if f.FeedID != "host" {
			t.Errorf("frame %d FeedID = %q, want override", i, f.FeedID)
		}
	}
	if sink.updates[0].FeedID != "host" {
		t.Errorf("update FeedID = %q", sink.updates[0].FeedID)
	}
}

func TestClientDiscardsMalformedFrames(t *testing.T) {
	srv, port := startServer(t, Options{})
	sink := &collector{}
	client, _ := runClient(t, ClientOptions{Port: port}, sink)
	waitFor(t, "login", func() bool { return client.Connected() && srv.Status().Clients == 1 })

	good := testFrames()[0]
	bad := []model.RawFrame{
		{GPSNs: -1, Signal: math.NaN(), Kind: model.FrameModeSLong, Data: []byte{1, 2, 3}, Channel: -1},
		{GPSNs: -1, Signal: math.NaN(), Kind: model.FrameModeSShort, Channel: -1},
		{GPSNs: -1, Signal: math.NaN(), Kind: model.FrameKind(99), Data: make([]byte, 14), Channel: -1},
		{GPSNs: -1, Signal: math.NaN(), Kind: model.FrameStatus, Data: make([]byte, 200), Channel: -1},
	}
	for _, f := range bad {
		srv.Broadcast(f)
	}
	srv.Broadcast(good)
	waitFor(t, "good frame after bad ones", func() bool { n, _ := sink.counts(); return n == 1 })
	if n := client.Invalid(); n != uint64(len(bad)) {
		t.Errorf("Invalid() = %d, want %d", n, len(bad))
	}
	if frames, _ := client.Counters(); frames != 1 {
		t.Errorf("delivered frames = %d, want 1", frames)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !bytes.Equal(sink.frames[0].Data, good.Data) {
		t.Errorf("delivered frame = %+v", sink.frames[0])
	}
}

func TestAuthFailure(t *testing.T) {
	srv, port := startServer(t, Options{Password: "right", FailLimit: 1000})
	states := &stateRec{}
	sink := &collector{}
	client, stop := runClient(t, ClientOptions{Port: port, Password: "wrong", OnState: states.on}, sink)
	waitFor(t, "auth error", func() bool { return errors.Is(states.lastErr(), ErrAuth) })
	if client.Connected() || states.connects() != 0 {
		t.Error("client reports a connection despite the wrong password")
	}
	if n := srv.Status().Clients; n != 0 {
		t.Errorf("server has %d clients after failed login", n)
	}
	srv.Broadcast(testFrames()[0])
	stop()
	if f, _ := sink.counts(); f != 0 {
		t.Errorf("unauthenticated client received %d frames", f)
	}

	// No password at all against a protected host fails the same way.
	states2 := &stateRec{}
	runClient(t, ClientOptions{Port: port, OnState: states2.on}, sink)
	waitFor(t, "auth error without password", func() bool { return errors.Is(states2.lastErr(), ErrAuth) })
}

// rawLogin performs the client side of the handshake on a plain socket.
func rawLogin(t *testing.T, port int, password string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	line, err := readLine(br)
	if err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if strings.HasPrefix(line, "ERR") {
		return conn, br, line
	}
	parts := strings.Fields(line)
	if len(parts) != 4 || parts[0] != "AIRVIEW-HUB" || parts[1] != "1" {
		t.Fatalf("unexpected greeting %q", line)
	}
	nonce, err := hex.DecodeString(parts[3])
	if err != nil || len(nonce) != 32 {
		t.Fatalf("bad nonce in %q", line)
	}
	cn := bytes.Repeat([]byte{0x42}, 32)
	io.WriteString(conn, "AUTH "+hex.EncodeToString(cn)+" "+hex.EncodeToString(clientProof(password, nonce))+"\n")
	reply, err := readLine(br)
	if err != nil {
		t.Fatalf("login reply: %v", err)
	}
	conn.SetDeadline(time.Time{})
	return conn, br, reply
}

func TestGreetingAndModes(t *testing.T) {
	_, port := startServer(t, Options{Password: "pw"})
	_, _, reply := rawLogin(t, port, "pw")
	if !strings.HasPrefix(reply, "OK ") {
		t.Fatalf("reply = %q, want OK", reply)
	}
	_, _, reply = rawLogin(t, port, "nope")
	if !strings.HasPrefix(reply, "ERR auth") {
		t.Fatalf("reply = %q, want ERR auth", reply)
	}
}

func TestLoginTimeoutAndMalformedHandshake(t *testing.T) {
	srv, port := startServer(t, Options{Password: "pw", LoginTimeout: 150 * time.Millisecond})
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	// A peer that connects and never answers is cut off after the login
	// timeout and never counts as a client.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	if line, err := readLine(br); err != nil || !strings.HasPrefix(line, "AIRVIEW-HUB 1 hmac-sha256 ") {
		t.Fatalf("greeting = %q, %v", line, err)
	}
	start := time.Now()
	if _, err := br.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("silent peer was not disconnected (err %v)", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("disconnect took %v", d)
	}

	// Garbage instead of an AUTH line is answered with ERR proto.
	for _, junk := range []string{"HELLO\n", "AUTH zz yy\n", "AUTH " + strings.Repeat("ab", 32) + "\n", strings.Repeat("x", 2000) + "\n"} {
		c2, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c2.SetDeadline(time.Now().Add(5 * time.Second))
		r2 := bufio.NewReader(c2)
		readLine(r2)
		io.WriteString(c2, junk)
		reply, _ := readLine(r2)
		c2.Close()
		if len(junk) > maxLine && reply == "" {
			continue // closing with unread input may reset the connection before the reply arrives
		}
		if !strings.HasPrefix(reply, "ERR proto") {
			t.Errorf("handshake %.20q...: reply %q, want ERR proto", junk, reply)
		}
	}
	if n := srv.Status().Clients; n != 0 {
		t.Errorf("%d clients after failed handshakes", n)
	}
}

func TestSlowClientDropped(t *testing.T) {
	srv, port := startServer(t, Options{ClientBuffer: 32 << 10, WriteTimeout: 30 * time.Second})
	conn, _, reply := rawLogin(t, port, "")
	if !strings.HasPrefix(reply, "OK ") {
		t.Fatalf("login reply %q", reply)
	}
	waitFor(t, "client registration", func() bool { return srv.Status().Clients == 1 })

	// A healthy second client must be unaffected by the stalled one.
	sink := &collector{}
	good, _ := runClient(t, ClientOptions{Port: port}, sink)
	waitFor(t, "good client", func() bool { return good.Connected() && srv.Status().Clients == 2 })

	// The raw connection never reads. Broadcast until the socket buffers
	// and then the 32 KiB backlog fill up; Broadcast itself must never block.
	f := testFrames()[0]
	sent := 0
	start := time.Now()
	for srv.DroppedClients() == 0 {
		// Bursts stay well below the backlog limit so that the healthy
		// client is never at risk.
		for i := 0; i < 200; i++ {
			srv.Broadcast(f)
		}
		sent += 200
		if time.Since(start) > 20*time.Second {
			t.Fatalf("slow client still connected after %d frames", sent)
		}
		// Give the healthy client time to drain so only the stalled one overflows.
		waitFor(t, "healthy client to keep up", func() bool { n, _ := sink.counts(); return n == sent })
	}
	waitFor(t, "slow client removal", func() bool { return srv.Status().Clients == 1 })
	if !good.Connected() {
		t.Error("healthy client was disconnected too")
	}
	// The dropped connection is closed by the server: reading drains what
	// was buffered and then hits EOF / reset.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("dropped connection was not closed: %v", err)
	}
}

func TestClientLimitAndThrottle(t *testing.T) {
	srv, port := startServer(t, Options{Password: "pw", MaxClients: 1, FailLimit: 2})
	_, _, reply := rawLogin(t, port, "pw")
	if !strings.HasPrefix(reply, "OK ") {
		t.Fatalf("first login: %q", reply)
	}
	waitFor(t, "registration", func() bool { return srv.Status().Clients == 1 })
	if _, _, reply = rawLogin(t, port, "pw"); !strings.HasPrefix(reply, "ERR busy") {
		t.Fatalf("second login: %q, want ERR busy", reply)
	}

	for i := 0; i < 2; i++ {
		if _, _, reply = rawLogin(t, port, "bad"); !strings.HasPrefix(reply, "ERR auth") {
			t.Fatalf("bad login %d: %q", i, reply)
		}
	}
	if _, _, reply = rawLogin(t, port, "pw"); !strings.HasPrefix(reply, "ERR throttled") {
		t.Fatalf("login after %d failures: %q, want ERR throttled", 2, reply)
	}
	if err := errLine(reply); !errors.Is(err, ErrThrottled) {
		t.Errorf("errLine(%q) = %v", reply, err)
	}
}

func TestPasswordChangeDisconnects(t *testing.T) {
	srv, port := startServer(t, Options{Password: "old"})
	states := &stateRec{}
	client, _ := runClient(t, ClientOptions{Port: port, Password: "old", OnState: states.on}, &collector{})
	waitFor(t, "login", client.Connected)
	waitFor(t, "registration", func() bool { return srv.Status().Clients == 1 })
	srv.SetPassword("new")
	waitFor(t, "rejection with the old password", func() bool { return errors.Is(states.lastErr(), ErrAuth) })
	if srv.Status().Clients != 0 {
		t.Error("client with the old password is still logged in")
	}
}

func TestServerCloseStopsClients(t *testing.T) {
	srv, port := startServer(t, Options{})
	client, _ := runClient(t, ClientOptions{Port: port}, &collector{})
	waitFor(t, "login", client.Connected)
	srv.Close()
	waitFor(t, "disconnect", func() bool { return !client.Connected() })
	if st := srv.Status(); st.Enabled || st.Clients != 0 {
		t.Errorf("status after Close = %+v", st)
	}
	srv.Broadcast(testFrames()[0]) // must be harmless
}

func TestRecordCodec(t *testing.T) {
	for i, w := range testFrames() {
		rec := appendFrame(nil, &w, 7)
		if n := int(rec[0])<<8 | int(rec[1]); n != len(rec)-2 || rec[2] != recFrame {
			t.Fatalf("frame %d: bad header % x (len %d)", i, rec[:3], len(rec))
		}
		g, idx, recvNs, err := decodeFrame(rec[3:])
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if idx != 7 || recvNs != w.Recv.UnixNano() || g.Kind != w.Kind || g.GPSNs != w.GPSNs ||
			g.Clock12MHz != w.Clock12MHz || !sameFloat(g.Signal, w.Signal) || g.Channel != w.Channel ||
			g.MLAT != w.MLAT || !bytes.Equal(g.Data, w.Data) {
			t.Errorf("frame %d round trip:\n got %+v\nwant %+v", i, g, w)
		}
		// Every truncation must be rejected or yield shorter data, never panic.
		for cut := 0; cut < len(rec)-3; cut++ {
			decodeFrame(rec[3 : 3+cut])
		}
	}
	for i, w := range testUpdates() {
		rec := appendUpdate(nil, &w, 3)
		_, idx, _, err := decodeUpdate(rec[3:])
		if err != nil || idx != 3 {
			t.Fatalf("update %d: idx %d err %v", i, idx, err)
		}
		for cut := 0; cut < len(rec)-3; cut++ {
			if _, _, _, err := decodeUpdate(rec[3 : 3+cut]); err == nil {
				t.Errorf("update %d truncated to %d bytes decoded without error", i, cut)
			}
		}
	}
	// Message counts are clamped to what a 32-bit int can hold.
	for _, c := range []struct{ in, want int }{{-5, 0}, {0, 0}, {7, 7}, {math.MaxInt32, math.MaxInt32}} {
		u := model.DecodedUpdate{ICAO: 1, Msgs: c.in}
		g, _, _, err := decodeUpdate(appendUpdate(nil, &u, 0)[3:])
		if err != nil || g.Msgs != c.want {
			t.Errorf("Msgs %d round trip = %d (%v), want %d", c.in, g.Msgs, err, c.want)
		}
	}
	raw := appendUpdate(nil, &model.DecodedUpdate{ICAO: 1, Msgs: 1}, 0)
	copy(raw[3+15:], []byte{0xFF, 0xFF, 0xFF, 0xFF}) // a peer sending the full uint32 range
	if g, _, _, err := decodeUpdate(raw[3:]); err != nil || g.Msgs != math.MaxInt32 {
		t.Errorf("Msgs 0xFFFFFFFF decoded as %d (%v)", g.Msgs, err)
	}

	// Out-of-range channel numbers are sent as "unknown".
	f := model.RawFrame{GPSNs: -1, Signal: math.NaN(), Channel: 4000, Kind: model.FrameModeSShort, Data: make([]byte, 7)}
	g, _, _, _ := decodeFrame(appendFrame(nil, &f, 0)[3:])
	if g.Channel != -1 {
		t.Errorf("channel = %d, want -1", g.Channel)
	}
}

func TestBroadcastWithoutClientsDoesNotAllocate(t *testing.T) {
	srv, _ := startServer(t, Options{})
	f := testFrames()[0]
	if n := testing.AllocsPerRun(200, func() { srv.Broadcast(f) }); n != 0 {
		t.Errorf("Broadcast with no clients allocates %v times per call", n)
	}
}
