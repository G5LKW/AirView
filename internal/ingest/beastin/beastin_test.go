package beastin

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
)

// --- test helpers ---------------------------------------------------------------

// collector is a Sink that keeps copies of everything delivered.
type collector struct {
	mu      sync.Mutex
	frames  []model.RawFrame
	events  []ClockEvent
	batches int
	notMap  int // batches without MapStatsOnly
	links   map[uint64]int
}

func (c *collector) HandleBeast(b *Batch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batches++
	if !b.MapStatsOnly {
		c.notMap++
	}
	if c.links == nil {
		c.links = map[uint64]int{}
	}
	c.links[b.Link] += len(b.Frames)
	c.frames = append(c.frames, b.Frames...)
	c.events = append(c.events, b.Events...)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

func (c *collector) byStation() map[model.StationIdx]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := map[model.StationIdx]int{}
	for _, f := range c.frames {
		m[f.Station]++
	}
	return m
}

func (c *collector) snapshot() []model.RawFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]model.RawFrame(nil), c.frames...)
}

// fakeClock is an adjustable clock for Options.Now.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

func startServer(t testing.TB, opt Options, reg Registry, sink Sink) *Server {
	t.Helper()
	if opt.Addr == "" {
		opt.Addr = "127.0.0.1:0"
	}
	s, err := New(opt, reg, sink)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", opt.Addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	for s.Addr() == nil {
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		s.Close()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return s
}

func dial(t testing.TB, s *Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// waitFor polls cond for up to 5 s.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

const testUUID = "0123456789abcdef0123456789abcdef"

// df17 returns a 14-byte DF17 payload for icao (content is not decoded by
// beastin; only the length matters).
func df17(icao uint32, n byte) []byte {
	return []byte{0x8D, byte(icao >> 16), byte(icao >> 8), byte(icao), 0x58, 0x1A, n, 0x1A, 0, 0, 0, 0, 0, n}
}

// frames appends n long frames with 12 MHz timestamps.
func frames(dst []byte, n int, ts0 uint64) []byte {
	for i := 0; i < n; i++ {
		dst = feed.AppendBeastFrame(dst, feed.BeastModeSLong, ts0+uint64(i)*12000, 0x80, df17(0x3C0000+uint32(i%50), byte(i)))
	}
	return dst
}

func linkOf(t testing.TB, s *Server) LinkStats {
	t.Helper()
	ls := s.Links()
	if len(ls) != 1 {
		t.Fatalf("%d links", len(ls))
	}
	return ls[0]
}

// readEscapes reads from c until want escape messages were parsed or the
// timeout passes.
func readEscapes(t testing.TB, c net.Conn, want int, timeout time.Duration) [][2][]byte {
	t.Helper()
	var got [][2][]byte
	p := feed.NewBeastParser(func(*feed.BeastFrame) {})
	p.OnEscape = func(typ byte, body []byte) {
		got = append(got, [2][]byte{{typ}, append([]byte(nil), body...)})
	}
	buf := make([]byte, 256)
	end := time.Now().Add(timeout)
	for len(got) < want && time.Now().Before(end) {
		c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := c.Read(buf)
		p.Write(buf[:n])
		if err != nil && !errors.Is(err, net.ErrClosed) && !isTimeout(err) {
			break
		}
	}
	return got
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// closedByServer reports whether the server closed c within timeout.
func closedByServer(c net.Conn, timeout time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 64)
	for {
		_, err := c.Read(buf)
		if err != nil {
			return !isTimeout(err)
		}
	}
}

// --- tests ----------------------------------------------------------------------

func TestUUIDLinkDelivers(t *testing.T) {
	reg := NewMemRegistry()
	col := &collector{}
	s := startServer(t, Options{}, reg, col)
	c := dial(t, s)

	var b []byte
	b = feed.AppendBeastUUID(b, "01234567-89ab-cdef-0123-456789abcdef")
	b = frames(b, 100, 1_000_000)
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "100 frames", func() bool { return col.count() == 100 })
	st, ok := reg.Lookup("uuid:" + testUUID)
	if !ok {
		t.Fatal("station not registered under the UUID")
	}
	for _, f := range col.snapshot() {
		if f.Station != st || !strings.HasPrefix(f.FeedID, "st/") || f.Kind != model.FrameModeSLong || len(f.Data) != 14 {
			t.Fatalf("frame %+v", f)
		}
		if f.HostNs == 0 || f.Recv.IsZero() {
			t.Fatalf("no receive time: %+v", f)
		}
	}
	if col.notMap != 0 {
		t.Fatalf("%d batches without MapStatsOnly", col.notMap)
	}
	ls := linkOf(t, s)
	if ls.UUID != testUUID || !ls.Attached || ls.Relay || ls.Frames != 100 || ls.UUIDEscapes != 1 {
		t.Fatalf("link stats %+v", ls)
	}
	if !strings.HasPrefix(ls.IPKey, "ip:") || len(ls.IPKey) != 3+model.IDLen {
		t.Fatalf("ip key %q", ls.IPKey)
	}
}

func TestUnknownEscapesKeepLink(t *testing.T) {
	col := &collector{}
	s := startServer(t, Options{}, NewMemRegistry(), col)
	c := dial(t, s)
	var b []byte
	b = feed.AppendBeastUUID(b, testUUID)
	for i := 0; i < 20; i++ {
		b = frames(b, 5, uint64(i)*1e6)
		// Unknown escape types with junk bodies, garbage and a stray 0x1A 0x1A.
		b = append(b, 0x1A, 0x99, 1, 2, 3, 0x1A, 0xE5, 0xFF, 0x00, 0x7F, 0x42, 0x1A, 0x1A)
		b = feed.AppendBeastOption(b, 'Z')
	}
	b = frames(b, 7, 99e6)
	// Write in awkward pieces to split frames and escapes.
	for i := 0; i < len(b); i += 13 {
		if _, err := c.Write(b[i:min(i+13, len(b))]); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "all frames", func() bool { return col.count() == 107 })
	ls := linkOf(t, s)
	if ls.Closed || ls.UnknownEscapes < 20 {
		t.Fatalf("link %+v", ls)
	}
	// Still alive: more frames go through.
	if _, err := c.Write(frames(nil, 3, 5e8)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "frames after unknown escapes", func() bool { return col.count() == 110 })
	if s.Stats().UnknownEscapes < 20 {
		t.Fatalf("stats %+v", s.Stats())
	}
}

func TestUnidentifiedThenUUID(t *testing.T) {
	reg := NewMemRegistry()
	col := &collector{}
	s := startServer(t, Options{IdentifyWait: 50 * time.Millisecond}, reg, col)
	c := dial(t, s)
	if _, err := c.Write(frames(nil, 10, 1e6)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "unidentified attach", func() bool { return col.count() == 10 })
	ls := linkOf(t, s)
	if !ls.Attached || ls.UUID != "" {
		t.Fatalf("link %+v", ls)
	}
	ipst, ok := reg.Lookup(ls.IPKey)
	if !ok {
		t.Fatal("no IP-keyed station")
	}
	if got := s.Stats(); got.Unidentified != 1 {
		t.Fatalf("stats %+v", got)
	}
	// A UUID arriving later upgrades the link; earlier frames stay with the
	// IP-keyed station and its attachment is released.
	b := feed.AppendBeastUUID(nil, testUUID)
	b = frames(b, 5, 2e6)
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "upgrade", func() bool { return col.count() == 15 })
	ust, ok := reg.Lookup("uuid:" + testUUID)
	if !ok || ust == ipst {
		t.Fatalf("uuid station %v %v", ust, ok)
	}
	m := col.byStation()
	if m[ipst] != 10 || m[ust] != 5 {
		t.Fatalf("by station %v", m)
	}
	waitFor(t, "old attachment released", func() bool { return reg.Attached() == 1 })
}

func TestUnidentifiedSlots(t *testing.T) {
	reg := NewMemRegistry()
	col := &collector{}
	s := startServer(t, Options{IdentifyWait: 20 * time.Millisecond}, reg, col)
	a := dial(t, s)
	a.Write(frames(nil, 2, 1))
	waitFor(t, "first", func() bool { return col.count() == 2 })
	b := dial(t, s)
	b.Write(frames(nil, 3, 1))
	waitFor(t, "second", func() bool { return col.count() == 5 })
	ls := s.Links()
	k0 := ls[0].IPKey
	s0, ok0 := reg.Lookup(k0)
	s1, ok1 := reg.Lookup(k0 + "/1")
	if !ok0 || !ok1 || s0 == s1 {
		t.Fatalf("slots: %v %v %v %v", s0, ok0, s1, ok1)
	}
	if m := col.byStation(); m[s0] != 2 || m[s1] != 3 {
		t.Fatalf("by station %v", m)
	}
	// Both leave; a feeder reconnecting alone gets slot 0 again.
	a.Close()
	b.Close()
	waitFor(t, "links gone", func() bool { return len(s.Links()) == 0 })
	c := dial(t, s)
	c.Write(frames(nil, 4, 1))
	waitFor(t, "third", func() bool { return col.count() == 9 })
	if m := col.byStation(); m[s0] != 6 || reg.Stations() != 2 {
		t.Fatalf("by station %v, %d stations", m, reg.Stations())
	}
}

func TestHeldFramesBeforeUUID(t *testing.T) {
	// Frames that arrive before the UUID (in the same read) are credited to
	// the UUID station.
	reg := NewMemRegistry()
	col := &collector{}
	s := startServer(t, Options{IdentifyWait: time.Second}, reg, col)
	c := dial(t, s)
	b := frames(nil, 4, 1e6)
	b = feed.AppendBeastUUID(b, testUUID)
	b = frames(b, 4, 2e6)
	c.Write(b)
	waitFor(t, "frames", func() bool { return col.count() == 8 })
	st, _ := reg.Lookup("uuid:" + testUUID)
	if m := col.byStation(); m[st] != 8 {
		t.Fatalf("by station %v", m)
	}
}

func TestPingEchoAndOwnPings(t *testing.T) {
	col := &collector{}
	s := startServer(t, Options{PingInterval: 100 * time.Millisecond}, NewMemRegistry(), col)
	c := dial(t, s)

	// A client-originated ping comes back verbatim (0x1A escaped).
	b := feed.AppendBeastUUID(nil, testUUID)
	b = feed.AppendBeastPing(b, 0x011A03)
	c.Write(b)
	got := readEscapes(t, c, 1, 2*time.Second)
	if len(got) != 1 || got[0][0][0] != feed.BeastPing || !bytes.Equal(got[0][1], []byte{0x01, 0x1A, 0x03}) {
		t.Fatalf("echo %v", got)
	}

	// After 'W' 'O' the server pings; echoing measures the round trip.
	c.Write(feed.AppendBeastOption(nil, feed.BeastOptPing))
	got = readEscapes(t, c, 1, 2*time.Second)
	if len(got) < 1 || got[0][0][0] != feed.BeastPing {
		t.Fatalf("no server ping: %v", got)
	}
	time.Sleep(20 * time.Millisecond)
	c.Write(feed.AppendBeastEscape(nil, feed.BeastPing, got[0][1]))
	waitFor(t, "rtt", func() bool { return linkOf(t, s).RTTLastMs > 0 })
	ls := linkOf(t, s)
	if !ls.PingOn || ls.PingsSent == 0 || ls.PingsEchoed != 1 || ls.RTTLastMs < 15 || ls.RTTLastMs > 2000 {
		t.Fatalf("link %+v", ls)
	}
	// Our echoed ping is not echoed back again: only further server pings arrive.
	more := readEscapes(t, c, 2, 300*time.Millisecond)
	for _, e := range more {
		if e[0][0] == feed.BeastPing && bytes.Equal(e[1], got[0][1]) {
			t.Fatal("echo of our own ping was echoed back")
		}
	}
}

func TestTrustedRelaySubStations(t *testing.T) {
	reg := NewMemRegistry()
	col := &collector{}
	opt := Options{Policy: Policy{TrustedRelays: []string{strings.ToUpper(testUUID) + "@127.0.0.0/8"}}}
	s := startServer(t, opt, reg, col)
	c := dial(t, s)
	b := feed.AppendBeastUUID(nil, testUUID)
	b = frames(b, 3, 1e6) // relay's own frames
	b = feed.AppendBeastReceiverID(b, 0x1A1A000000000001)
	b = frames(b, 4, 2e6)
	b = feed.AppendBeastReceiverID(b, 2)
	b = frames(b, 5, 3e6)
	b = feed.AppendBeastReceiverID(b, 0x1A1A000000000001)
	b = frames(b, 1, 4e6)
	c.Write(b)
	waitFor(t, "frames", func() bool { return col.count() == 13 })
	own, _ := reg.Lookup("uuid:" + testUUID)
	r1, ok1 := reg.Lookup("uuid:" + testUUID + "/rx:1a1a000000000001")
	r2, ok2 := reg.Lookup("uuid:" + testUUID + "/rx:0000000000000002")
	if !ok1 || !ok2 {
		t.Fatal("relay sub-stations not registered")
	}
	if m := col.byStation(); m[own] != 3 || m[r1] != 5 || m[r2] != 5 {
		t.Fatalf("by station %v", m)
	}
	waitFor(t, "stats", func() bool { return len(linkOf(t, s).Stations) == 3 })
	if ls := linkOf(t, s); !ls.Relay || ls.ReceiverIDs != 3 {
		t.Fatalf("link %+v", ls)
	}
	c.Close()
	waitFor(t, "detach all", func() bool { return reg.Attached() == 0 && len(s.Links()) == 0 })
}

func TestUntrustedReceiverIDIgnored(t *testing.T) {
	reg := NewMemRegistry()
	col := &collector{}
	s := startServer(t, Options{}, reg, col)
	c := dial(t, s)
	b := feed.AppendBeastUUID(nil, testUUID)
	b = feed.AppendBeastReceiverID(b, 7)
	b = frames(b, 6, 1e6)
	c.Write(b)
	waitFor(t, "frames", func() bool { return col.count() == 6 })
	own, _ := reg.Lookup("uuid:" + testUUID)
	if m := col.byStation(); m[own] != 6 || len(m) != 1 {
		t.Fatalf("by station %v", m)
	}
	if ls := linkOf(t, s); ls.RelayIgnored != 1 || ls.Relay {
		t.Fatalf("link %+v", ls)
	}
	if reg.Stations() != 1 {
		t.Fatalf("%d stations", reg.Stations())
	}
}

func TestReceiverIDBeforeUUID(t *testing.T) {
	// A trusted relay that sends 0xE3 before its UUID still gets the
	// sub-station once identified.
	reg := NewMemRegistry()
	col := &collector{}
	s := startServer(t, Options{Policy: Policy{TrustedRelays: []string{testUUID + "@127.0.0.1"}}}, reg, col)
	c := dial(t, s)
	b := feed.AppendBeastReceiverID(nil, 9)
	b = feed.AppendBeastUUID(b, testUUID)
	b = frames(b, 2, 1e6)
	c.Write(b)
	waitFor(t, "frames", func() bool { return col.count() == 2 })
	r9, ok := reg.Lookup("uuid:" + testUUID + "/rx:0000000000000009")
	if m := col.byStation(); !ok || m[r9] != 2 {
		t.Fatalf("by station %v", m)
	}
}

func TestMLATResultsDropped(t *testing.T) {
	col := &collector{}
	s := startServer(t, Options{}, NewMemRegistry(), col)
	c := dial(t, s)
	b := feed.AppendBeastUUID(nil, testUUID)
	b = feed.AppendBeastFrame(b, feed.BeastModeSLong, 0xFF004D4C4154, 0x50, df17(0xABCDEF, 1))
	b = frames(b, 2, 1e6)
	c.Write(b)
	waitFor(t, "frames", func() bool { return col.count() == 2 })
	if ls := linkOf(t, s); ls.MLATDropped != 1 {
		t.Fatalf("link %+v", ls)
	}
	for _, f := range col.snapshot() {
		if f.MLAT {
			t.Fatal("MLAT result delivered")
		}
	}
}

func TestStripClock(t *testing.T) {
	col := &collector{}
	s := startServer(t, Options{StripClock: true}, NewMemRegistry(), col)
	c := dial(t, s)
	b := feed.AppendBeastUUID(nil, testUUID)
	b = frames(b, 50, 1e6)
	c.Write(b)
	waitFor(t, "frames", func() bool { return col.count() == 50 })
	for _, f := range col.snapshot() {
		if f.ClockCls != model.ClockNone || f.Ticks != 0 || f.Epoch != 0 {
			t.Fatalf("clock kept: %+v", f)
		}
	}
}

func TestFrameRateLimit(t *testing.T) {
	clk := newFakeClock()
	reg := NewMemRegistry()
	col := &collector{}
	opt := Options{Now: clk.Now, Policy: Policy{MaxFPS: 100, MaxBytesPerSec: -1, OverLimitSeconds: 3, CoolDown: time.Minute}}
	s := startServer(t, opt, reg, col)
	c := dial(t, s)
	c.Write(feed.AppendBeastUUID(nil, testUUID))
	waitFor(t, "attach", func() bool { return reg.Attached() == 1 })

	sendSecond := func() {
		c.Write(frames(nil, 300, 1e6))
		waitFor(t, "frames read", func() bool { l, ok := s.Link(1); return ok && l.Frames%300 == 0 && l.Frames > 0 })
	}
	sendSecond()
	waitFor(t, "first second", func() bool { return col.count() == 100 })
	// 'W' 'S' was sent once.
	got := readEscapes(t, c, 1, 2*time.Second)
	if len(got) != 1 || got[0][0][0] != feed.BeastOption || got[0][1][0] != feed.BeastOptSlowDown {
		t.Fatalf("slow down: %v", got)
	}
	for i := 0; i < 3; i++ {
		clk.Advance(time.Second)
		c.Write(frames(nil, 300, 1e6))
		time.Sleep(50 * time.Millisecond)
	}
	clk.Advance(time.Second)
	c.Write(frames(nil, 1, 1e6))
	if !closedByServer(c, 3*time.Second) {
		t.Fatal("link not closed after sustained overload")
	}
	st := s.Stats()
	if st.Dropped[ReasonRate] != 1 || st.RateDropped < 600 {
		t.Fatalf("stats %+v", st)
	}
	// The identity is cooling down: a reconnect with the same UUID is closed.
	c2 := dial(t, s)
	c2.Write(append(feed.AppendBeastUUID(nil, testUUID), frames(nil, 1, 1)...))
	if !closedByServer(c2, 3*time.Second) {
		t.Fatal("cooling identity accepted")
	}
	if s.Stats().Rejected[ReasonCoolDown] != 1 {
		t.Fatalf("stats %+v", s.Stats())
	}
}

func TestByteRateLimit(t *testing.T) {
	clk := newFakeClock()
	col := &collector{}
	opt := Options{Now: clk.Now, Policy: Policy{MaxFPS: -1, MaxBytesPerSec: 1000, OverLimitSeconds: 2}}
	s := startServer(t, opt, NewMemRegistry(), col)
	c := dial(t, s)
	c.Write(feed.AppendBeastUUID(nil, testUUID))
	for i := 0; i < 3; i++ {
		c.Write(frames(nil, 100, 1e6)) // ~2.3 kB
		time.Sleep(50 * time.Millisecond)
		clk.Advance(time.Second)
	}
	c.Write(frames(nil, 1, 1e6))
	if !closedByServer(c, 3*time.Second) {
		t.Fatal("link not closed")
	}
	if s.Stats().Dropped[ReasonRate] != 1 || s.Stats().SlowDowns == 0 {
		t.Fatalf("stats %+v", s.Stats())
	}
}

func TestConnectionLimits(t *testing.T) {
	col := &collector{}
	opt := Options{Policy: Policy{MaxConnsPerIP: 3, MaxUnattachedPerIP: 2}}
	s := startServer(t, opt, NewMemRegistry(), col)
	a := dial(t, s)
	b := dial(t, s)
	waitFor(t, "two links", func() bool { return len(s.Links()) == 2 })
	// Third link while two are unattached: refused.
	x := dial(t, s)
	if !closedByServer(x, 2*time.Second) {
		t.Fatal("third unattached link accepted")
	}
	// Identify one; now a third is accepted, a fourth exceeds MaxConnsPerIP.
	a.Write(feed.AppendBeastUUID(nil, testUUID))
	waitFor(t, "attach", func() bool { return s.Stats().Identified == 1 })
	dial(t, s)
	waitFor(t, "three links", func() bool { return len(s.Links()) == 3 })
	y := dial(t, s)
	if !closedByServer(y, 2*time.Second) {
		t.Fatal("fourth link accepted")
	}
	st := s.Stats()
	if st.Rejected[ReasonUnattached] != 1 || st.Rejected[ReasonPerIP] != 1 {
		t.Fatalf("stats %+v", st)
	}
	_ = b
}

func TestAllowCIDRExempt(t *testing.T) {
	opt := Options{Policy: Policy{MaxConnsPerIP: 1, MaxUnattachedPerIP: 1,
		AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}}
	s := startServer(t, opt, NewMemRegistry(), &collector{})
	for i := 0; i < 4; i++ {
		dial(t, s)
	}
	waitFor(t, "four links", func() bool { return len(s.Links()) == 4 })
}

func TestPerIPStations(t *testing.T) {
	reg := NewMemRegistry()
	s := startServer(t, Options{Policy: Policy{PerIPStations: 2}}, reg, &collector{})
	uuids := []string{"11111111111111111111111111111111", "22222222222222222222222222222222", "33333333333333333333333333333333"}
	var conns []net.Conn
	for _, u := range uuids[:2] {
		c := dial(t, s)
		c.Write(feed.AppendBeastUUID(nil, u))
		conns = append(conns, c)
	}
	waitFor(t, "two stations", func() bool { return reg.Attached() == 2 })
	// A second link of an existing station is fine; a third station is not.
	c := dial(t, s)
	c.Write(feed.AppendBeastUUID(nil, uuids[0]))
	waitFor(t, "same station twice", func() bool { return reg.Attached() == 3 })
	x := dial(t, s)
	x.Write(feed.AppendBeastUUID(nil, uuids[2]))
	if !closedByServer(x, 2*time.Second) {
		t.Fatal("third station accepted")
	}
	if s.Stats().Rejected[ReasonStations] != 1 {
		t.Fatalf("stats %+v", s.Stats())
	}
}

func TestRegistryRefusal(t *testing.T) {
	reg := NewMemRegistry()
	reg.Decide = func(id Identity) (string, error) {
		if id.UUID == "" {
			return "", errors.New("no anonymous feeders")
		}
		return model.AdmDenied, nil
	}
	col := &collector{}
	s := startServer(t, Options{IdentifyWait: 20 * time.Millisecond}, reg, col)
	c := dial(t, s)
	c.Write(append(feed.AppendBeastUUID(nil, testUUID), frames(nil, 3, 1)...))
	if !closedByServer(c, 2*time.Second) {
		t.Fatal("denied link kept")
	}
	c2 := dial(t, s)
	c2.Write(frames(nil, 3, 1))
	if !closedByServer(c2, 2*time.Second) {
		t.Fatal("refused unidentified link kept")
	}
	if col.count() != 0 {
		t.Fatalf("%d frames delivered for refused links", col.count())
	}
	// Refused identities cool down: no second registry call within the window.
	calls := 0
	reg.Decide = func(Identity) (string, error) { calls++; return model.AdmApproved, nil }
	c3 := dial(t, s)
	c3.Write(feed.AppendBeastUUID(nil, testUUID))
	if !closedByServer(c3, 2*time.Second) || calls != 0 {
		t.Fatalf("cool-down not applied (calls %d)", calls)
	}
	st := s.Stats()
	if st.Rejected[ReasonRegistry] != 2 || st.Rejected[ReasonCoolDown] != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestPendingAdmissionFlows(t *testing.T) {
	reg := NewMemRegistry()
	reg.Admission = model.AdmPending
	col := &collector{}
	s := startServer(t, Options{}, reg, col)
	c := dial(t, s)
	c.Write(append(feed.AppendBeastUUID(nil, testUUID), frames(nil, 3, 1)...))
	waitFor(t, "frames", func() bool { return col.count() == 3 })
	if ls := linkOf(t, s); !ls.Pending {
		t.Fatalf("link %+v", ls)
	}
	if s.Stats().Pending != 1 {
		t.Fatalf("stats %+v", s.Stats())
	}
}

func TestGateClosed(t *testing.T) {
	var ready atomic.Bool
	s := startServer(t, Options{Gate: ready.Load}, NewMemRegistry(), &collector{})
	c := dial(t, s)
	if !closedByServer(c, 2*time.Second) {
		t.Fatal("link accepted while not ready")
	}
	ready.Store(true)
	dial(t, s)
	waitFor(t, "link", func() bool { return len(s.Links()) == 1 })
	if s.Stats().Rejected[ReasonGate] != 1 {
		t.Fatalf("stats %+v", s.Stats())
	}
}

func TestSilentAndIdleTimeouts(t *testing.T) {
	s := startServer(t, Options{HandshakeTimeout: 50 * time.Millisecond, IdleTimeout: 200 * time.Millisecond}, NewMemRegistry(), &collector{})
	silent := dial(t, s)
	if !closedByServer(silent, 2*time.Second) {
		t.Fatal("silent link kept")
	}
	idle := dial(t, s)
	idle.Write(feed.AppendBeastUUID(nil, testUUID))
	if !closedByServer(idle, 2*time.Second) {
		t.Fatal("idle link kept")
	}
	st := s.Stats()
	if st.Dropped[ReasonSilent] != 1 || st.Dropped[ReasonIdle] != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// slowSink blocks until released, to fill the per-link queue.
type slowSink struct {
	release chan struct{}
	n       atomic.Int64
}

func (s *slowSink) HandleBeast(b *Batch) {
	<-s.release
	s.n.Add(int64(len(b.Frames)))
}

func TestQueueSlowDownAndDrop(t *testing.T) {
	sink := &slowSink{release: make(chan struct{})}
	opt := Options{QueueBatches: 4, BatchFrames: 10, Workers: 1, Policy: Policy{MaxFPS: -1, MaxBytesPerSec: -1}}
	s := startServer(t, opt, NewMemRegistry(), sink)
	c := dial(t, s)
	c.Write(feed.AppendBeastUUID(nil, testUUID))
	for i := 0; i < 20; i++ {
		c.Write(frames(nil, 10, uint64(i)*1e6))
		time.Sleep(5 * time.Millisecond)
	}
	got := readEscapes(t, c, 1, 2*time.Second)
	if len(got) != 1 || got[0][0][0] != feed.BeastOption || got[0][1][0] != feed.BeastOptSlowDown {
		t.Fatalf("no slow-down: %v", got)
	}
	waitFor(t, "drops", func() bool { return s.Stats().QueueDropped > 0 })
	close(sink.release)
	waitFor(t, "drain", func() bool { l, _ := s.Link(1); return l.QueueDepth == 0 })
	st := s.Stats()
	if uint64(sink.n.Load())+st.QueueDropped != 200 {
		t.Fatalf("delivered %d + dropped %d != 200", sink.n.Load(), st.QueueDropped)
	}
}

type budget struct{ ok atomic.Bool }

func (b *budget) Take(int) bool { return b.ok.Load() }

func TestBudget(t *testing.T) {
	bud := &budget{}
	col := &collector{}
	s := startServer(t, Options{Budget: bud}, NewMemRegistry(), col)
	c := dial(t, s)
	c.Write(append(feed.AppendBeastUUID(nil, testUUID), frames(nil, 5, 1)...))
	waitFor(t, "budget drop", func() bool { return s.Stats().BudgetDropped == 5 })
	bud.ok.Store(true)
	c.Write(frames(nil, 5, 1e6))
	waitFor(t, "frames", func() bool { return col.count() == 5 })
}

func TestCloseDeliversAndDetaches(t *testing.T) {
	reg := NewMemRegistry()
	col := &collector{}
	s, err := New(Options{}, reg, col)
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), ln) }()
	for s.Addr() == nil {
		time.Sleep(time.Millisecond)
	}
	var cs []net.Conn
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", s.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		u := strings.Repeat(string("abcde"[i]), 32)
		c.Write(append(feed.AppendBeastUUID(nil, u), frames(nil, 20, 1)...))
		cs = append(cs, c)
	}
	waitFor(t, "all frames read", func() bool { return s.Stats().Frames == 100 })
	s.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if col.count() != 100 || reg.Attached() != 0 || len(s.Links()) != 0 {
		t.Fatalf("after close: %d frames, %d attached, %d links", col.count(), reg.Attached(), len(s.Links()))
	}
	for _, c := range cs {
		if !closedByServer(c, time.Second) {
			t.Fatal("link left open")
		}
	}
	s.Close() // idempotent
}

func TestSetPolicyAndConfig(t *testing.T) {
	a := model.DefaultAggregator()
	a.BeastPort = 30005
	a.Limits.MaxFPSPerStation = 1234
	a.Limits.MaxKBpsPerStation = 50
	a.Limits.AllowCIDRs = []string{"10.0.0.0/8", "192.0.2.7"}
	a.TrustedRelays = []string{"01234567-89AB-CDEF-0123-456789ABCDEF", "junk"}
	o := OptionsFromConfig(a)
	p := o.Policy
	if o.Addr != ":30005" || p.MaxFPS != 1234 || p.MaxBytesPerSec != 50000 || len(p.AllowCIDRs) != 2 ||
		len(p.TrustedRelays) != 1 || p.TrustedRelays[0] != testUUID || o.HandshakeTimeout != 10*time.Second {
		t.Fatalf("options %+v", o)
	}
	if !compilePolicy(p).allowed(netip.MustParseAddr("192.0.2.7")) || compilePolicy(p).allowed(netip.MustParseAddr("192.0.2.8")) {
		t.Fatal("allow list")
	}
	// aggregator.beast tunables (defaults, then custom values).
	if o.IdentifyWait != 2*time.Second || o.IdleTimeout != 3*time.Minute || o.PingInterval != 5*time.Second ||
		o.QueueBatches != 64 || o.MaxLinks != 4096 || p.OverLimitSeconds != 5 || p.CoolDown != 5*time.Minute {
		t.Fatalf("beast defaults %+v", o)
	}
	a.Beast.IdentifyWaitMs, a.Beast.MaxLinks, a.Beast.CoolDownSec, a.Beast.AcceptMlatResults = 500, 10, 0, true
	o = OptionsFromConfig(a)
	if o.IdentifyWait != 500*time.Millisecond || o.MaxLinks != 10 || o.Policy.CoolDown >= 0 || !o.AcceptMLATResults {
		t.Fatalf("beast custom %+v", o)
	}
	s, _ := New(Options{}, NewMemRegistry(), &collector{})
	s.SetPolicy(Policy{MaxFPS: 10})
	if got := s.Policy(); got.MaxFPS != 10 || got.MaxConnsPerIP != 16 {
		t.Fatalf("policy %+v", got)
	}
	s.Close()
}

func TestIdentityHelpers(t *testing.T) {
	cases := map[string]string{
		"01234567-89ab-cdef-0123-456789abcdef":     testUUID,
		testUUID + "ffff":                          testUUID,
		"0123456789ABCDEF0123456789ABCDEF\x00\x00": testUUID,
		"0123456789abcdef0123456789abffffffff":     "0123456789abcdef0123456789abffff",
	}
	for in, want := range cases {
		if got, ok := normalizeUUID(in); !ok || got != want {
			t.Errorf("normalizeUUID(%q) = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "zz", strings.Repeat("0", 36), "0123456789abcdef0123"} {
		if _, ok := normalizeUUID(bad); ok {
			t.Errorf("normalizeUUID(%q) accepted", bad)
		}
	}
	k := []byte("k")
	a := ipKey(k, netip.MustParseAddr("192.0.2.1"), 30004)
	if a != ipKey(k, netip.MustParseAddr("::ffff:192.0.2.1"), 30004) {
		t.Error("mapped address keys differ")
	}
	if a == ipKey(k, netip.MustParseAddr("192.0.2.1"), 31090) || a == ipKey([]byte("x"), netip.MustParseAddr("192.0.2.1"), 30004) {
		t.Error("key ignores port or server key")
	}
	if strings.Contains(a, "192") || len(a) != 3+model.IDLen {
		t.Errorf("key %q", a)
	}
	if subnetOf(netip.MustParseAddr("192.0.2.77")).String() != "192.0.2.0/24" ||
		subnetOf(netip.MustParseAddr("2001:db8:1:2::5")).String() != "2001:db8:1::/48" {
		t.Error("subnets")
	}
	id := Identity{UUID: testUUID, Relay: true, ReceiverID: 0xAB}
	if id.Key() != "uuid:"+testUUID+"/rx:00000000000000ab" {
		t.Errorf("key %q", id.Key())
	}
	if (Identity{IPKey: "ip:x"}).Key() != "ip:x" || !(Identity{}).Unidentified() {
		t.Error("ip identity")
	}
}

func TestRateWindow(t *testing.T) {
	var w rateWin
	for sec := int64(100); sec < 103; sec++ {
		for i := 0; i < 15; i++ {
			w.add(sec, 1, 10)
		}
	}
	w.add(103, 1, 10)
	if w.run != 3 {
		t.Fatalf("run %d", w.run)
	}
	w.add(105, 1, 10) // second 103 was within the limit
	if w.run != 0 {
		t.Fatalf("run after a good second %d", w.run)
	}
}

func TestUpgradeAtStationLimit(t *testing.T) {
	// With one station allowed per address, an unidentified link that later
	// sends its UUID replaces its own station instead of being refused.
	reg := NewMemRegistry()
	col := &collector{}
	s := startServer(t, Options{IdentifyWait: 20 * time.Millisecond, Policy: Policy{PerIPStations: 1}}, reg, col)
	c := dial(t, s)
	c.Write(frames(nil, 2, 1))
	waitFor(t, "unidentified", func() bool { return col.count() == 2 })
	c.Write(append(feed.AppendBeastUUID(nil, testUUID), frames(nil, 2, 2)...))
	waitFor(t, "upgraded", func() bool { return col.count() == 4 })
	if ls := linkOf(t, s); ls.Closed || ls.UUID != testUUID {
		t.Fatalf("link %+v", ls)
	}
}

// SEC-03: a relay UUID is trusted only from its bound addresses; a plain
// UUID only from AllowCIDRs.
func TestRelayBoundToSource(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	other := netip.MustParseAddr("198.51.100.9")
	p := compilePolicy(Policy{TrustedRelays: []string{testUUID + "@127.0.0.0/8"}})
	if !p.relayTrusted(testUUID, lo) || p.relayTrusted(testUUID, other) || p.relayTrusted("", lo) {
		t.Fatal("bound relay")
	}
	p = compilePolicy(Policy{TrustedRelays: []string{testUUID}})
	if p.relayTrusted(testUUID, lo) {
		t.Fatal("plain UUID trusted without AllowCIDRs")
	}
	p = compilePolicy(Policy{TrustedRelays: []string{testUUID}, AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}})
	if !p.relayTrusted(testUUID, other) || p.relayTrusted(testUUID, lo) {
		t.Fatal("plain UUID with AllowCIDRs")
	}

	// End to end: the relay's UUID from a source outside its CIDR gets no
	// sub-stations.
	reg := NewMemRegistry()
	col := &collector{}
	s := startServer(t, Options{Policy: Policy{TrustedRelays: []string{testUUID + "@192.0.2.0/24"}}}, reg, col)
	c := dial(t, s)
	b := feed.AppendBeastUUID(nil, testUUID)
	b = feed.AppendBeastReceiverID(b, 7)
	b = frames(b, 4, 1e6)
	c.Write(b)
	waitFor(t, "frames", func() bool { return col.count() == 4 })
	if ls := linkOf(t, s); ls.Relay || ls.RelayIgnored != 1 || reg.Stations() != 1 {
		t.Fatalf("spoofed relay: %+v, %d stations", ls, reg.Stations())
	}
}

// SEC-03: new relay sub-stations are registered at a bounded rate.
func TestRelayAttachRateLimited(t *testing.T) {
	l := &link{now: time.Unix(1000, 0)}
	n := 0
	for range 200 {
		if l.takeAttachToken() {
			n++
		}
	}
	if n != relayAttachBurst {
		t.Fatalf("burst %d", n)
	}
	l.now = l.now.Add(time.Second)
	n = 0
	for range 200 {
		if l.takeAttachToken() {
			n++
		}
	}
	if n != relayAttachRate {
		t.Fatalf("refill %d", n)
	}
}

// SEC-04: a second link with a live station's UUID from another address is
// refused as a clone; from the same address it is accepted.
func TestBeastUUIDCloneRefused(t *testing.T) {
	s := &Server{perIP: map[netip.Addr]*ipState{}, uuidAt: map[string]uuidHolder{}}
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.2")
	for _, ip := range []netip.Addr{a, b} {
		s.perIP[ip] = &ipState{stations: map[string]int{}}
	}
	pol := compilePolicy(Policy{})
	key := "uuid:" + testUUID
	if why := s.claimStation(a, key, "", pol); why != "" {
		t.Fatal(why)
	}
	if why := s.claimStation(b, key, "", pol); why != ReasonCloned {
		t.Fatalf("clone from another address: %q", why)
	}
	if why := s.claimStation(a, key, "", pol); why != "" {
		t.Fatalf("same address: %q", why)
	}
	s.releaseStation(a, key)
	s.releaseStation(a, key)
	if why := s.claimStation(b, key, "", pol); why != "" {
		t.Fatalf("after the first link left: %q", why)
	}
}
