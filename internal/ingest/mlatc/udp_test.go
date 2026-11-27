package mlatc

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

// ------------------------------------------------------------ scripted UDP client
//
// The datagrams below are assembled byte by byte from the layout in
// MLATC.md §9 (independently of the server's decoder and of the
// AppendUDP* helpers, which TestUDPAppendHelpers checks against them).

type udpScript struct {
	key  uint32
	seq  uint16
	base uint64
	b    []byte
}

func (u *udpScript) begin(base uint64) {
	u.base = base
	u.b = u.b[:0]
	var h [14]byte
	binary.BigEndian.PutUint32(h[0:], u.key)
	binary.BigEndian.PutUint16(h[4:], u.seq)
	binary.BigEndian.PutUint64(h[6:], base)
	u.b = append(u.b, h[:]...)
	u.seq++
}

func (u *udpScript) i32(v int64) {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], uint32(int32(v)))
	u.b = append(u.b, x[:]...)
}

func (u *udpScript) u64(v uint64) {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	u.b = append(u.b, x[:]...)
}

func (u *udpScript) mlat(t uint64, frame []byte) {
	if len(frame) == 7 {
		u.b = append(u.b, 2)
		u.i32(int64(t) - int64(u.base))
		u.b = append(u.b, frame...)
		return
	}
	u.b = append(u.b, 3)
	u.i32(int64(t) - int64(u.base))
	pad := make([]byte, 14)
	copy(pad, frame)
	u.b = append(u.b, pad...)
}

func (u *udpScript) ssync(t uint64, frame []byte) {
	u.b = append(u.b, 4)
	u.i32(int64(t) - int64(u.base))
	u.b = append(u.b, frame...)
}

func (u *udpScript) sync(et uint64, em []byte, ot uint64, om []byte) {
	u.b = append(u.b, 1)
	u.i32(int64(et) - int64(u.base))
	u.i32(int64(ot) - int64(u.base))
	u.b = append(u.b, em...)
	u.b = append(u.b, om...)
}

func (u *udpScript) absSync(et uint64, em []byte, ot uint64, om []byte) {
	u.b = append(u.b, 6)
	u.u64(et)
	u.u64(ot)
	u.b = append(u.b, em...)
	u.b = append(u.b, om...)
}

func (u *udpScript) rebase(base uint64) {
	u.b = append(u.b, 5)
	u.u64(base)
	u.base = base
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// udpReply decodes the handshake's udp_transport: [host, port, key].
func udpReply(t *testing.T, raw json.RawMessage) (string, int, uint32, bool) {
	t.Helper()
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		if b {
			t.Fatalf("udp_transport true is not a valid reply")
		}
		return "", 0, 0, false
	}
	var v []json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil || len(v) != 3 {
		t.Fatalf("udp_transport %s", raw)
	}
	var host string
	var port int
	var key uint32
	if json.Unmarshal(v[0], &host) != nil || json.Unmarshal(v[1], &port) != nil || json.Unmarshal(v[2], &key) != nil {
		t.Fatalf("udp_transport %s", raw)
	}
	return host, port, key, true
}

func startUDPServer(t *testing.T, o Options) *testEnv {
	t.Helper()
	o.UDP = true
	if o.UDPAddr == "" {
		o.UDPAddr = "127.0.0.1:0"
	}
	env := startServer(t, o, nil, nil)
	waitFor(t, "UDP socket", 3*time.Second, func() bool { return env.srv.UDPAddr() != nil })
	return env
}

func udpHello(uuid string) map[string]any {
	h := defaultHello(uuid)
	h["udp_transport"] = 2
	h["clock_type"] = "beast"
	h["compress"] = []string{"zlib2"} // UDP clients still compress their TCP side
	return h
}

func framesOf(fr []model.RawFrame, st model.StationIdx) []model.RawFrame {
	var out []model.RawFrame
	for _, f := range fr {
		if f.Station == st {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b model.RawFrame) int {
		if a.Ticks != b.Ticks {
			if a.Ticks < b.Ticks {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.Data, b.Data)
	})
	return out
}

// TestUDPInterop: a scripted client offers udp_transport 2, receives
// [host, port, key] and sends every datagram message type over a real
// socket; a second client sends the same receptions as JSON lines on TCP.
// Both stations must deliver identical frames (kind, payload, ticks, epoch)
// to the ingest: the datagram decoder is checked against the JSON path.
func TestUDPInterop(t *testing.T) {
	env := startUDPServer(t, Options{FlushEvery: 50 * time.Millisecond})
	a := dialClient(t, env.addr, udpHello(uuidA))
	host, port, key, ok := udpReply(t, a.reply["udp_transport"])
	if !ok || host != "127.0.0.1" || port != env.srv.UDPAddr().(*net.UDPAddr).Port || key == 0 {
		t.Fatalf("udp_transport %s", a.reply["udp_transport"])
	}
	hb := defaultHello(uuidB)
	hb["clock_type"] = "beast"
	b := dialClient(t, env.addr, hb)
	if _, _, _, ok := udpReply(t, b.reply["udp_transport"]); ok {
		t.Fatal("UDP offered to a client that did not ask")
	}
	stA, _, _ := env.reg.Lookup("uuid:" + uuidA)
	stB, _, _ := env.reg.Lookup("uuid:" + uuidB)

	uc, err := net.Dial("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()

	t0 := uint64(5_000_000_000)
	short := mustHex(shortHex)
	long := mustHex(longHex)
	odd := mustHex("8d4840d6202cc371c32ce0576099")
	df20 := mustHex("a0001838ca3e51f0a8000047a5f1")
	df21 := mustHex("a8001838ca3e51f0a8000047a5f2")
	ssync := mustHex("8d4840d6582cc371c32ce057609a")
	ac := []byte{0x0a, 0x12}

	u := &udpScript{key: key}
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)+"\n") }

	// datagram 1: one of each frame message
	u.begin(t0)
	u.mlat(t0, short)
	add(`{"mlat":{"t":%d,"m":"%s"}}`, t0, shortHex)
	u.mlat(t0+1200, df20)
	add(`{"mlat":{"t":%d,"m":"%x"}}`, t0+1200, df20)
	u.ssync(t0+2400, ssync)
	add(`{"ssync":{"t":%d,"m":"%x"}}`, t0+2400, ssync)
	u.sync(t0+3600, long, t0+4800, odd)
	add(`{"sync":{"et":%d,"em":"%x","ot":%d,"om":"%x"}}`, t0+3600, long, t0+4800, odd)
	d1 := slices.Clone(u.b)
	// datagram 2: negative delta, then a rebase
	u.begin(t0 + 100_000)
	u.mlat(t0+99_000, df21)
	add(`{"mlat":{"t":%d,"m":"%x"}}`, t0+99_000, df21)
	u.rebase(t0 + 1_200_000)
	u.mlat(t0+1_200_007, short[:])
	add(`{"mlat":{"t":%d,"m":"%s"}}`, t0+1_200_007, shortHex)
	d2 := slices.Clone(u.b)
	// datagram 3: absolute sync (halves further apart than an i32 delta)
	u.begin(t0 + 2_000_000)
	u.absSync(t0+2_000_000, long, t0+2_060_000, odd)
	add(`{"sync":{"et":%d,"em":"%x","ot":%d,"om":"%x"}}`, t0+2_000_000, long, t0+2_060_000, odd)
	d3 := slices.Clone(u.b)
	// datagram 4: a Mode A/C reply (zero-padded long frame)
	u.begin(t0 + 2_100_000)
	u.mlat(t0+2_100_123, ac)
	add(`{"mlat":{"t":%d,"m":"0a12"}}`, t0+2_100_123)
	d4 := slices.Clone(u.b)

	for _, d := range [][]byte{d1, d2, d3, d4} {
		if _, err := uc.Write(d); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // keep loopback order (several readers)
	}
	if err := b.send(lines...); err != nil {
		t.Fatal(err)
	}
	const want = 10
	// Generous deadline: waitFor polls, so this only matters when the machine is loaded (a full
	// parallel `go test ./...` with the MLAT suites took more than 5 s here).
	waitFor(t, "frames", 30*time.Second, func() bool {
		fr, _ := env.sink.snapshot()
		return len(framesOf(fr, stA)) >= want && len(framesOf(fr, stB)) >= want
	})
	time.Sleep(50 * time.Millisecond)
	fr, _ := env.sink.snapshot()
	fa, fb := framesOf(fr, stA), framesOf(fr, stB)
	if len(fa) != want || len(fb) != want {
		t.Fatalf("frames: udp %d, tcp %d, want %d", len(fa), len(fb), want)
	}
	for i := range fa {
		x, y := fa[i], fb[i]
		if x.Ticks != y.Ticks || x.Kind != y.Kind || !bytes.Equal(x.Data, y.Data) || x.Epoch != y.Epoch ||
			x.ClockCls != y.ClockCls || x.HostNs == 0 {
			t.Fatalf("frame %d: udp %+v\ntcp %+v", i, x, y)
		}
	}
	// the Mode A/C reply: 2 bytes, 12 MHz offset 244 ticks added back
	last := fa[len(fa)-1]
	if last.Kind != model.FrameModeAC || !bytes.Equal(last.Data, ac) || last.Ticks != t0+2_100_123+244 {
		t.Fatalf("Mode A/C frame %+v", last)
	}
	if fa[0].Ticks != t0+768 || fa[0].Kind != model.FrameModeSShort {
		t.Fatalf("first frame %+v", fa[0])
	}
	var la *LinkStats
	for _, l := range env.srv.Links() {
		if l.Station == stA {
			la = &l
		}
	}
	if la == nil || !la.UDP || la.UDPPackets != 4 || la.UDPLost != 0 || la.UDPDup != 0 || la.UDPBad != 0 ||
		la.ModeACFrames != 1 || la.SSyncs != 1 || la.Syncs != 2 {
		t.Fatalf("link %+v", la)
	}
	st := env.srv.Stats()
	if !st.UDPActive || st.UDPPackets != 4 || st.UDPBad != 0 || st.UDPUnknownKey != 0 || st.ModeACFrames != 2 {
		t.Fatalf("stats %+v", st)
	}

	// After the client leaves, its key is dead.
	a.nc.Close()
	waitFor(t, "detach", 30*time.Second, func() bool { return !env.srv.Online(stA) })
	if _, err := uc.Write(d1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "unknown key", 30*time.Second, func() bool { return env.srv.Stats().UDPUnknownKey == 1 })
}

// TestUDPOffer: when udp_transport is (not) offered in the reply.
func TestUDPOffer(t *testing.T) {
	env := startUDPServer(t, Options{UDPHost: "mlat.example.net", UDPPort: 40000})
	for i, tc := range []struct {
		offer any
		want  bool
	}{{2, true}, {"2", false}, {false, false}, {true, false}, {3, false}, {nil, false}} {
		h := udpHello(fmt.Sprintf("%032x", i+1))
		h["compress"] = []string{"none"}
		if tc.offer == nil {
			delete(h, "udp_transport")
		} else {
			h["udp_transport"] = tc.offer
		}
		c := dialClient(t, env.addr, h)
		host, port, key, ok := udpReply(t, c.reply["udp_transport"])
		if ok != tc.want {
			t.Fatalf("offer %v: reply %s", tc.offer, c.reply["udp_transport"])
		}
		if ok && (host != "mlat.example.net" || port != 40000 || key == 0) {
			t.Fatalf("offer %v: reply %s", tc.offer, c.reply["udp_transport"])
		}
		c.nc.Close()
	}
	// UDP not enabled: never offered.
	env2 := startServer(t, Options{}, nil, nil)
	c := dialClient(t, env2.addr, udpHello(uuidC))
	if _, _, _, ok := udpReply(t, c.reply["udp_transport"]); ok {
		t.Fatal("offered without Options.UDP")
	}
	if active, _ := env2.srv.UDPStatus(); active {
		t.Fatal("UDP active without Options.UDP")
	}
}

// TestUDPBindFailure: a port that cannot be bound leaves the server on TCP.
func TestUDPBindFailure(t *testing.T) {
	busy, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	env := startServer(t, Options{UDP: true, UDPAddr: busy.LocalAddr().String()}, nil, nil)
	waitFor(t, "bind error", 3*time.Second, func() bool { _, e := env.srv.UDPStatus(); return e != "" })
	c := dialClient(t, env.addr, udpHello(uuidA))
	if _, _, _, ok := udpReply(t, c.reply["udp_transport"]); ok {
		t.Fatal("UDP offered without a socket")
	}
	if c.compress == "" {
		t.Fatal("client refused")
	}
}

// TestUDPSourceCheck: datagrams must come from the TCP client's address
// unless UDPAnySource.
func TestUDPSourceCheck(t *testing.T) {
	for _, anySrc := range []bool{false, true} {
		env := startUDPServer(t, Options{UDPAnySource: anySrc})
		c := dialClient(t, env.addr, udpHello(uuidA))
		_, _, key, ok := udpReply(t, c.reply["udp_transport"])
		if !ok {
			t.Fatal("no UDP")
		}
		// The key goes live after the reply is written (udpPublish, after
		// the epoch start); under load the test could otherwise send first.
		waitFor(t, "UDP key published", 3*time.Second, func() bool {
			env.srv.udpMu.RLock()
			defer env.srv.udpMu.RUnlock()
			return env.srv.udpKeys[key] != nil
		})
		u := &udpScript{key: key}
		u.begin(1_000_000)
		u.mlat(1_000_000, mustHex(shortHex))
		env.srv.handleDatagram(u.b, mustAddr("192.0.2.7"), time.Now().UnixNano())
		st := env.srv.Stats()
		if anySrc && (st.UDPBadSource != 0 || st.Frames != 1) || !anySrc && (st.UDPBadSource != 1 || st.Frames != 0) {
			t.Fatalf("anySource %v: %+v", anySrc, st)
		}
		// the right source is always accepted
		u.begin(1_000_100)
		u.mlat(1_000_100, mustHex(shortHex))
		env.srv.handleDatagram(u.b, mustAddr("::ffff:127.0.0.1"), time.Now().UnixNano())
		if f := env.srv.Stats().Frames; f != map[bool]uint64{false: 1, true: 2}[anySrc] {
			t.Fatalf("anySource %v: frames %d", anySrc, f)
		}
	}
}

// offlineUDP opens an Offline session that negotiated UDP.
func offlineUDP(t *testing.T, o Options, extra map[string]any) (*Server, *Offline, *testClock) {
	t.Helper()
	clk := newTestClock()
	o.UDP, o.Now = true, clk.Now
	srv, err := New(o, NewMemRegistry(), newFakeSink())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if extra == nil {
		extra = map[string]any{}
	}
	extra["udp_transport"] = 2
	off, reply, err := srv.Offline(offlineHello(t, uuidA, extra), mustAddr("198.51.100.4"), clk.Now())
	if err != nil {
		t.Fatalf("%v: %s", err, reply)
	}
	var r map[string]json.RawMessage
	json.Unmarshal(reply, &r)
	if _, _, key, ok := udpReply(t, r["udp_transport"]); !ok || key != off.UDPKey() {
		t.Fatalf("reply %s", reply)
	}
	return srv, off, clk
}

func linkOf(srv *Server) LinkStats {
	l := srv.Links()
	if len(l) != 1 {
		panic("want one link")
	}
	return l[0]
}

// TestUDPSequence: loss, duplicate and reordering accounting.
func TestUDPSequence(t *testing.T) {
	srv, off, clk := offlineUDP(t, Options{}, nil)
	u := &udpScript{key: off.UDPKey()}
	pkt := func(seq uint16, t0 uint64) []byte {
		u.seq = seq
		u.begin(t0)
		u.mlat(t0, mustHex(shortHex))
		return slices.Clone(u.b)
	}
	send := func(b []byte) {
		clk.Add(10 * time.Millisecond)
		if err := off.Datagram(b, clk.Now()); err != nil {
			t.Fatal(err)
		}
	}
	p10 := pkt(10, 2_000_000)
	send(p10)
	send(pkt(11, 2_001_000))
	p13 := pkt(13, 2_003_000)
	send(p13)
	if l := linkOf(srv); l.UDPLost != 1 {
		t.Fatalf("lost %d", l.UDPLost)
	}
	send(p13) // duplicate: dropped whole
	send(pkt(12, 2_002_000))
	l := linkOf(srv)
	if l.UDPLost != 0 || l.UDPDup != 1 || l.UDPPackets != 5 || l.Frames != 4 {
		t.Fatalf("%+v", l)
	}
	st := srv.Stats()
	if st.UDPDup != 1 || st.UDPLate != 1 || st.UDPLost != 1 {
		t.Fatalf("%+v", st)
	}
	// wrap-around: 65535 -> 0 is no loss
	var w seqWindow
	for _, s := range []uint16{65534, 65535, 0, 1} {
		if gap, dup, late := w.note(s); gap != 0 || dup || late {
			t.Fatalf("seq %d: %d %v %v", s, gap, dup, late)
		}
	}
	if gap, _, _ := w.note(5); gap != 3 {
		t.Fatalf("gap %d", gap)
	}
	if _, dup, _ := w.note(65535); !dup {
		t.Fatal("old duplicate not detected")
	}
}

// TestUDPMalformed: truncated / unknown messages keep what came before; a
// client sending mostly garbage datagrams is disconnected.
func TestUDPMalformed(t *testing.T) {
	srv, off, clk := offlineUDP(t, Options{Policy: Policy{MaxBadLines: 3}}, nil)
	u := &udpScript{key: off.UDPKey()}
	u.begin(3_000_000)
	u.mlat(3_000_000, mustHex(shortHex))
	good := len(u.b)
	u.mlat(3_000_100, mustHex(longHex))
	trunc := u.b[:len(u.b)-3] // second message truncated
	if err := off.Datagram(trunc, clk.Now()); err != nil {
		t.Fatal(err)
	}
	l := linkOf(srv)
	if l.Frames != 1 || l.UDPBad != 1 {
		t.Fatalf("%+v", l)
	}
	unknown := append(slices.Clone(u.b[:good]), 9, 1, 2, 3)
	unknown[4], unknown[5] = 0, 1 // next sequence number
	off.Datagram(unknown, clk.Now())
	if l := linkOf(srv); l.UDPBad != 2 {
		t.Fatalf("%+v", l)
	}
	// negative timestamp: the frame is dropped, the datagram is well-formed
	neg := &udpScript{key: off.UDPKey(), seq: 2}
	neg.begin(10)
	neg.mlat(10, mustHex(shortHex))
	binary.BigEndian.PutUint32(neg.b[15:], uint32(0xffffff00)) // delta -256
	off.Datagram(neg.b, clk.Now())
	if l := linkOf(srv); l.UDPBad != 2 || srv.Stats().DroppedBad == 0 {
		t.Fatalf("%+v", l)
	}
	if srv.Stats().UDPUnknownKey != 0 {
		t.Fatal("unknown key counted")
	}
	off.Datagram([]byte{1, 2, 3}, clk.Now())
	if srv.Stats().UDPBad != 3 {
		t.Fatalf("%+v", srv.Stats())
	}
	// garbage until the disconnect
	var err error
	for i := 0; i < 10 && err == nil; i++ {
		g := &udpScript{key: off.UDPKey(), seq: uint16(10 + i)}
		g.begin(5)
		g.b = append(g.b, 0xee)
		err = off.Datagram(g.b, clk.Now())
	}
	if err != ErrOfflineClosed {
		t.Fatalf("not disconnected: %v", err)
	}
	waitFor(t, "garbage close", 3*time.Second, func() bool { return srv.Stats().Reasons[ReasonGarbage] == 1 })
}

// TestUDPIdle: a UDP client that is silent on TCP stays connected while
// its datagrams flow and is dropped once both paths are idle.
func TestUDPIdle(t *testing.T) {
	env := startUDPServer(t, Options{IdleTimeout: 400 * time.Millisecond, FlushEvery: 50 * time.Millisecond})
	c := dialClient(t, env.addr, udpHello(uuidA))
	host, port, key, _ := udpReply(t, c.reply["udp_transport"])
	uc, err := net.Dial("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	st, _, _ := env.reg.Lookup("uuid:" + uuidA)
	u := &udpScript{key: key}
	for i := range 12 {
		u.begin(uint64(1_000_000 + i*1_200_000))
		u.mlat(u.base, mustHex(shortHex))
		uc.Write(u.b)
		time.Sleep(100 * time.Millisecond)
	}
	if !env.srv.Online(st) {
		t.Fatal("dropped while datagrams flowed")
	}
	if !c.waitClosed(3 * time.Second) {
		t.Fatal("idle UDP client not dropped")
	}
	waitFor(t, "idle close", 3*time.Second, func() bool { return env.srv.Stats().Reasons[ReasonIdle] == 1 })
}

// TestUDPAppendHelpers: the exported datagram builders produce the bytes
// of the hand-written layout.
func TestUDPAppendHelpers(t *testing.T) {
	short, long, odd := mustHex(shortHex), mustHex(longHex), mustHex("8d4840d6202cc371c32ce0576099")
	u := &udpScript{key: 0xdeadbeef, seq: 7}
	u.begin(1000)
	u.mlat(990, short)
	u.mlat(1010, long)
	u.mlat(1020, []byte{1, 2})
	u.ssync(1030, long)
	u.sync(1040, long, 1050, odd)
	u.rebase(5000)
	u.absSync(1<<40, long, 1<<40+5, odd)

	b := AppendUDPHeader(nil, 0xdeadbeef, 7, 1000)
	var err error
	step := func(nb []byte, e error) {
		b = nb
		if e != nil && err == nil {
			err = e
		}
	}
	step(AppendUDPMLAT(b, -10, short))
	step(AppendUDPMLAT(b, 10, long))
	step(AppendUDPMLAT(b, 20, []byte{1, 2}))
	step(AppendUDPSSync(b, 30, long))
	step(AppendUDPSync(b, 40, 50, long, odd))
	b = AppendUDPRebase(b, 5000)
	step(AppendUDPAbsSync(b, 1<<40, 1<<40+5, long, odd))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, u.b) {
		t.Fatalf("\n%x\n%x", b, u.b)
	}
	if _, err := AppendUDPMLAT(nil, 0, make([]byte, 5)); err == nil {
		t.Fatal("5-byte frame accepted")
	}
	if _, err := AppendUDPSSync(nil, 0, short); err == nil {
		t.Fatal("short ssync accepted")
	}
}

// ------------------------------------------------------------ Mode A/C requests

func TestModeACRequests(t *testing.T) {
	// a client that announced the extension
	clk := newTestClock()
	srv, err := New(Options{Now: clk.Now}, NewMemRegistry(), newFakeSink())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	off, _, err := srv.Offline(offlineHello(t, uuidA, map[string]any{"modeac": true}), mustAddr("198.51.100.5"), clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	st := off.Station().Station
	if !srv.ModeACCapable(st) || !srv.SetModeACAll(st, true) || !srv.StartSendingModeAC(st, []uint16{0x7700, 0x0a12}) {
		t.Fatal("not capable")
	}
	out := string(off.Poll(clk.Now()))
	if !strings.Contains(out, `{"start_sending_modeac":["*"]}`+"\n") ||
		!strings.Contains(out, `{"start_sending_modeac":["0a12","7700"]}`+"\n") {
		t.Fatalf("poll %q", out)
	}
	if out := off.Poll(clk.Now()); strings.Contains(string(out), "modeac") {
		t.Fatalf("repeated: %q", out)
	}
	srv.StopSendingModeAC(st, []uint16{0x0a12})
	srv.SetModeACAll(st, false)
	srv.StartSendingModeAC(st, []uint16{0x7700}) // already requested: no line
	out = string(off.Poll(clk.Now()))
	if !strings.Contains(out, `{"stop_sending_modeac":["*"]}`+"\n") ||
		!strings.Contains(out, `{"stop_sending_modeac":["0a12"]}`+"\n") || strings.Contains(out, "start_sending_modeac") {
		t.Fatalf("poll %q", out)
	}
	// a Mode A/C reply over JSON: Mode A/C kind, 244-tick offset
	if err := off.Lines([]byte(`{"mlat":{"t":1000000,"m":"7700"}}`), clk.Now()); err != nil {
		t.Fatal(err)
	}
	l := linkOf(srv)
	if !l.ModeAC || l.ModeACFrames != 1 {
		t.Fatalf("%+v", l)
	}
	off.Close()

	// a reference client: state kept, nothing sent
	off2, _, err := srv.Offline(offlineHello(t, uuidB, nil), mustAddr("198.51.100.6"), clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	st2 := off2.Station().Station
	if srv.ModeACCapable(st2) || srv.SetModeACAll(st2, true) || srv.StartSendingModeAC(st2, []uint16{1}) {
		t.Fatal("capable without the flag")
	}
	if out := off2.Poll(clk.Now()); strings.Contains(string(out), "modeac") {
		t.Fatalf("sent to a reference client: %q", out)
	}
	off2.Close()
	if srv.SetModeACAll(st2, true) {
		t.Fatal("request for an offline station")
	}

	// ModeACAllClients: every client gets them
	srv3, _ := New(Options{Now: clk.Now, ModeACAllClients: true}, NewMemRegistry(), newFakeSink())
	defer srv3.Close()
	off3, _, err := srv3.Offline(offlineHello(t, uuidC, nil), mustAddr("198.51.100.7"), clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv3.SetModeACAll(off3.Station().Station, true)
	if out := off3.Poll(clk.Now()); !strings.Contains(string(out), `{"start_sending_modeac":["*"]}`) {
		t.Fatalf("poll %q", out)
	}
	off3.Close()
}

// TestModeACRequestsLive: the writer goroutine sends the requests on TCP.
func TestModeACRequestsLive(t *testing.T) {
	env := startServer(t, Options{FlushEvery: 20 * time.Millisecond}, nil, nil)
	h := defaultHello(uuidA)
	h["modeac"] = true
	c := dialClient(t, env.addr, h)
	st, _, _ := env.reg.Lookup("uuid:" + uuidA)
	if !env.srv.StartSendingModeAC(st, []uint16{0x1200}) {
		t.Fatal("not capable")
	}
	var codes []string
	if err := json.Unmarshal(c.expect("start_sending_modeac", 3*time.Second), &codes); err != nil ||
		!slices.Equal(codes, []string{"1200"}) {
		t.Fatalf("%v %v", codes, err)
	}
}

// FuzzDatagram: arbitrary datagram bodies never panic, and every frame
// handed on has a valid length.
func FuzzDatagram(f *testing.F) {
	f.Add(udpBenchPacket(3))
	u := &udpScript{key: 1}
	u.begin(1 << 40)
	u.sync(1<<40+5, mustHex(longHex), 1<<40-5, mustHex(longHex))
	u.rebase(7)
	u.absSync(1<<63+1, mustHex(longHex), 3, mustHex(longHex))
	u.mlat(9, []byte{1, 2})
	f.Add(u.b)
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < udpHeaderLen {
			return
		}
		c := udpBenchConn(t)
		c.s.sink = &checkSink{t: t}
		c.datagram(b, c.nowNs)
	})
}

type checkSink struct {
	nopSink
	t *testing.T
}

func (s *checkSink) Submit(fr []model.RawFrame) {
	for _, f := range fr {
		if n := len(f.Data); n != 2 && n != 7 && n != 14 {
			s.t.Fatalf("frame of %d bytes", n)
		}
	}
}

// ------------------------------------------------------------ cost

func udpBenchPacket(n int) []byte {
	b := AppendUDPHeader(nil, 1, 0, 1_000_000_000)
	for i := range n {
		b, _ = AppendUDPMLAT(b, int32(i*1200), mustHex(shortHex))
	}
	return b
}

func udpBenchConn(tb testing.TB) *conn {
	c := benchConn(tb)
	c.udpKey = 1
	return c
}

// TestUDPDatagramZeroAlloc pins the 0-allocation property of the datagram
// path (decode, convert, dedup, batch, hand-over).
func TestUDPDatagramZeroAlloc(t *testing.T) {
	c := udpBenchConn(t)
	pkt := udpBenchPacket(100)
	seq := uint16(0)
	n := testing.AllocsPerRun(500, func() {
		c.recent = [dedupSlots]dedupEntry{}
		c.clk.have = false
		seq++
		binary.BigEndian.PutUint16(pkt[4:], seq)
		c.datagram(pkt, c.nowNs)
	})
	if n != 0 {
		t.Fatalf("%v allocations per datagram", n)
	}
	if c.frames.Load() == 0 {
		t.Fatal("no frames")
	}
}

// BenchmarkUDPDatagram: one ~1,214-byte datagram of 100 short mlat messages
// (the reference client flushes at about 1,400 bytes).
func BenchmarkUDPDatagram(b *testing.B) {
	c := udpBenchConn(b)
	pkt := udpBenchPacket(100)
	b.SetBytes(int64(len(pkt)))
	b.ReportAllocs()
	seq := uint16(0)
	for b.Loop() {
		c.recent = [dedupSlots]dedupEntry{}
		c.clk.have = false
		seq++
		binary.BigEndian.PutUint16(pkt[4:], seq)
		c.datagram(pkt, c.nowNs)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/100, "ns/msg")
}
