package mlatc

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/model"
)

// ------------------------------------------------------------ fake sink

type evRec struct {
	st model.StationIdx
	ev ingest.ClockEvent
}

type fakeSink struct {
	mu       sync.Mutex
	frames   []model.RawFrame
	events   []evRec
	ts       map[model.StationIdx]bool
	sessions []*fakeSess
	mode     string // mode AddStation returns
	n        atomic.Uint64
	onSubmit func([]model.RawFrame)
}

func newFakeSink() *fakeSink {
	return &fakeSink{ts: map[model.StationIdx]bool{}, mode: ingest.ModeFull}
}

func (s *fakeSink) Submit(fr []model.RawFrame) {
	if s.onSubmit != nil {
		s.onSubmit(fr)
	}
	s.mu.Lock()
	for _, f := range fr {
		f.Data = append([]byte(nil), f.Data...)
		s.frames = append(s.frames, f)
	}
	s.mu.Unlock()
	s.n.Add(uint64(len(fr)))
}

func (s *fakeSink) ClockEvent(st model.StationIdx, ev ingest.ClockEvent) {
	s.mu.Lock()
	s.events = append(s.events, evRec{st, ev})
	s.mu.Unlock()
}

func (s *fakeSink) SetTimestamps(st model.StationIdx, ok bool) {
	s.mu.Lock()
	s.ts[st] = ok
	s.mu.Unlock()
}

func (s *fakeSink) OpenSession(info ingest.SessionInfo) SessionHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs := &fakeSess{info: info, mode: s.mode}
	s.sessions = append(s.sessions, fs)
	return fs
}

func (s *fakeSink) snapshot() ([]model.RawFrame, []evRec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.RawFrame(nil), s.frames...), append([]evRec(nil), s.events...)
}

func (s *fakeSink) session(i int) *fakeSess {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < len(s.sessions) {
		return s.sessions[i]
	}
	return nil
}

type fakeSess struct {
	mu       sync.Mutex
	info     ingest.SessionInfo
	mode     string
	stations []model.StationIdx
	bytes    int
	closed   bool
}

func (f *fakeSess) AddStation(st model.StationIdx) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stations = append(f.stations, st)
	return f.mode
}
func (f *fakeSess) RemoveStation(model.StationIdx) {}
func (f *fakeSess) AddBytes(n int) {
	f.mu.Lock()
	f.bytes += n
	f.mu.Unlock()
}
func (f *fakeSess) SetQueueFill(float64) {}
func (f *fakeSess) Close(time.Duration) {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// ------------------------------------------------------------ clock

type testClock struct{ ns atomic.Int64 }

func newTestClock() *testClock {
	c := &testClock{}
	c.ns.Store(time.Now().UnixNano())
	return c
}
func (c *testClock) Now() time.Time      { return time.Unix(0, c.ns.Load()) }
func (c *testClock) Add(d time.Duration) { c.ns.Add(int64(d)) }
func (c *testClock) Set(t time.Time)     { c.ns.Store(t.UnixNano()) }
func (c *testClock) SetNs(ns int64)      { c.ns.Store(ns) }
func (c *testClock) NowNs() int64        { return c.ns.Load() }
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ------------------------------------------------------------ server

type testEnv struct {
	srv  *Server
	addr string
	reg  *MemRegistry
	sink *fakeSink
}

func startServer(t *testing.T, o Options, reg *MemRegistry, sink Sink) *testEnv {
	t.Helper()
	if reg == nil {
		reg = NewMemRegistry()
	}
	fs, _ := sink.(*fakeSink)
	if sink == nil {
		fs = newFakeSink()
		sink = fs
	}
	srv, err := New(o, reg, sink)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), ln) }()
	t.Cleanup(func() {
		srv.Close()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		if reg.Attached() != 0 {
			t.Errorf("%d attachments left after Close", reg.Attached())
		}
	})
	return &testEnv{srv: srv, addr: ln.Addr().String(), reg: reg, sink: fs}
}

// ------------------------------------------------------------ scripted client

// scriptClient speaks the client side of the protocol, written
// independently of the server code: the reference client's handshake
// padding, zlib/zlib2 compression of its own direction (level 1 zlib
// stream, sync flush, tail stripped, 2-byte length), and inflation of the
// server direction in zlib2 mode.
type scriptClient struct {
	t        *testing.T
	nc       net.Conn
	br       *bufio.Reader
	compress string
	zbuf     bytes.Buffer
	zw       *zlib.Writer
	reply    map[string]json.RawMessage
	msgs     chan map[string]json.RawMessage
	closed   chan struct{}
	wmu      sync.Mutex
}

func defaultHello(uuid string) map[string]any {
	return map[string]any{
		"version": 3, "client_version": "test-0.1", "compress": []string{"none", "zlib", "zlib2"},
		"selective_traffic": true, "heartbeat": true, "return_results": true, "return_stats": true,
		"udp_transport": false, "return_result_format": "ecef", "uuid": uuid, "user": "tester",
		"lat": 51.5, "lon": -0.12, "alt": 30, "clock_type": "dump1090", "privacy": false,
	}
}

func dialClient(t *testing.T, addr string, hello map[string]any) *scriptClient {
	t.Helper()
	b, _ := json.Marshal(hello)
	b = append(b, strings.Repeat(" ", 128)...)
	b = append(b, '\n')
	return dialHello(t, addr, b)
}

// dialHello connects and sends a ready-made handshake line.
func dialHello(t *testing.T, addr string, b []byte) *scriptClient {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	c := &scriptClient{t: t, nc: nc, br: bufio.NewReader(nc), msgs: make(chan map[string]json.RawMessage, 4096),
		closed: make(chan struct{})}
	if _, err := nc.Write(b); err != nil {
		t.Fatal(err)
	}
	nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("no handshake reply: %v", err)
	}
	nc.SetReadDeadline(time.Time{})
	if err := json.Unmarshal(line, &c.reply); err != nil {
		t.Fatalf("reply %q: %v", line, err)
	}
	if _, denied := c.reply["deny"]; denied {
		close(c.closed)
		return c
	}
	json.Unmarshal(c.reply["compress"], &c.compress)
	if c.compress != compressNone {
		c.zw, _ = zlib.NewWriterLevel(&c.zbuf, 1)
	}
	go c.readLoop()
	return c
}

func (c *scriptClient) denied() []string {
	var d []string
	json.Unmarshal(c.reply["deny"], &d)
	return d
}

func (c *scriptClient) readLoop() {
	defer close(c.closed)
	var lines io.Reader = c.br
	if c.compress == compressZlib2 {
		pr, pw := io.Pipe()
		go func() {
			for {
				var hdr [2]byte
				if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
					pw.CloseWithError(err)
					return
				}
				n := int(binary.BigEndian.Uint16(hdr[:]))
				buf := make([]byte, n+4)
				if _, err := io.ReadFull(c.br, buf[:n]); err != nil {
					pw.CloseWithError(err)
					return
				}
				copy(buf[n:], []byte{0, 0, 0xff, 0xff})
				if _, err := pw.Write(buf); err != nil {
					return
				}
			}
		}()
		zr, err := zlib.NewReader(pr)
		if err != nil {
			return
		}
		lines = zr
	}
	sc := bufio.NewScanner(lines)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			c.t.Errorf("server sent invalid JSON %q", sc.Text())
			return
		}
		c.msgs <- m
	}
}

// send writes lines (each must end in \n) as one batch.
func (c *scriptClient) send(lines ...string) error {
	return c.sendRaw([]byte(strings.Join(lines, "")))
}

// sendRaw writes complete lines as one batch (compressed as negotiated).
func (c *scriptClient) sendRaw(raw []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var payload []byte
	if c.compress == compressNone {
		payload = raw
	} else {
		c.zw.Write(raw)
		c.zw.Flush()
		b := c.zbuf.Bytes()
		b = b[:len(b)-4]
		for len(b) > 0 {
			n := min(len(b), 32000)
			payload = binary.BigEndian.AppendUint16(payload, uint16(n))
			payload = append(payload, b[:n]...)
			b = b[n:]
		}
		c.zbuf.Reset()
	}
	_, err := c.nc.Write(payload)
	return err
}

// expect waits for a server message with the given key and returns its value.
func (c *scriptClient) expect(key string, d time.Duration) json.RawMessage {
	c.t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case m := <-c.msgs:
			if v, ok := m[key]; ok {
				return v
			}
		case <-timer.C:
			c.t.Fatalf("no %q message within %v", key, d)
		case <-c.closed:
			// drain what was read before the close
			select {
			case m := <-c.msgs:
				if v, ok := m[key]; ok {
					return v
				}
				continue
			default:
			}
			c.t.Fatalf("connection closed while waiting for %q", key)
		}
	}
}

// waitClosed waits until the server closes the connection.
func (c *scriptClient) waitClosed(d time.Duration) bool {
	if c.compress == "" {
		// denied: read until EOF
		c.nc.SetReadDeadline(time.Now().Add(d))
		_, err := io.Copy(io.Discard, c.br)
		return err == nil
	}
	select {
	case <-c.closed:
		return true
	case <-time.After(d):
		return false
	}
}

func dialClient2(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
