package ntp

import (
	"context"
	"errors"
	"math"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
)

func TestPacketRoundTrip(t *testing.T) {
	// A typical stratum-2 server reply.
	p := Packet{
		Leap:           LeapAddSecond,
		Version:        4,
		Mode:           ModeServer,
		Stratum:        2,
		Poll:           6,
		Precision:      -23,
		RootDelay:      0x00000A3D, // ~40 ms
		RootDispersion: 0x0000051F, // ~20 ms
		RefID:          0xC0000201, // 192.0.2.1
		Reference:      0xE9A1B2C3_11223344,
		Origin:         0xE9A1B2C4_55667788,
		Receive:        0xE9A1B2C4_99AABBCC,
		Transmit:       0xE9A1B2C4_DDEEFF00,
	}
	b := p.Marshal()
	if b[0] != 0x64 { // 01 100 100
		t.Fatalf("first byte = %#02x, want 0x64", b[0])
	}
	if b[1] != 2 || b[2] != 6 || b[3] != 0xE9 { // -23 as two's complement
		t.Fatalf("stratum/poll/precision bytes = % x", b[1:4])
	}
	if got := [4]byte(b[12:16]); got != [4]byte{192, 0, 2, 1} {
		t.Fatalf("refid bytes = %v", got)
	}
	if got := [8]byte(b[40:48]); got != [8]byte{0xE9, 0xA1, 0xB2, 0xC4, 0xDD, 0xEE, 0xFF, 0x00} {
		t.Fatalf("transmit bytes = % x", got)
	}
	q, err := ParsePacket(b[:])
	if err != nil {
		t.Fatal(err)
	}
	if q != p {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", q, p)
	}
	// Trailing extension/MAC bytes are ignored.
	if q2, err := ParsePacket(append(b[:], 1, 2, 3, 4)); err != nil || q2 != p {
		t.Fatalf("packet with trailer: %v %+v", err, q2)
	}
	if _, err := ParsePacket(b[:47]); !errors.Is(err, ErrShortPacket) {
		t.Fatalf("short packet error = %v", err)
	}
	// The classic SNTP client request: first byte 0x23 (v4, mode 3), rest zero.
	req := Packet{Version: 4, Mode: ModeClient}
	rb := req.Marshal()
	if rb[0] != 0x23 {
		t.Fatalf("client first byte = %#02x, want 0x23", rb[0])
	}
}

func TestTimestampConversion(t *testing.T) {
	// Unix epoch = 2208988800 s into NTP era 0.
	if got := TimestampFromTime(time.Unix(0, 0)); got != Timestamp(0x83AA7E80)<<32 {
		t.Fatalf("epoch timestamp = %#x", uint64(got))
	}
	// Half a second is 0x80000000 in the fraction.
	if got := TimestampFromTime(time.Unix(0, 5e8)); uint32(got) != 0x80000000 {
		t.Fatalf("fraction = %#x", uint32(got))
	}
	cases := []time.Time{
		time.Date(2026, 11, 11, 12, 34, 56, 123456789, time.UTC),
		time.Date(1999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		time.Date(2036, 2, 7, 6, 28, 15, 0, time.UTC),         // last second of era 0
		time.Date(2036, 2, 7, 6, 28, 16, 250000000, time.UTC), // first second of era 1
		time.Date(2060, 6, 1, 0, 0, 0, 1, time.UTC),
	}
	for _, want := range cases {
		ts := TimestampFromTime(want)
		// Pivot within a few years of the true time, as the local clock would be.
		got := ts.Time(want.Add(-300 * 24 * time.Hour))
		if d := got.Sub(want); d > time.Nanosecond || d < -time.Nanosecond {
			t.Errorf("%v: round trip off by %v (got %v)", want, d, got)
		}
	}
	// Era 1 wraps the seconds field back to small numbers.
	if secs := uint32(TimestampFromTime(cases[3]) >> 32); secs != 0 {
		t.Errorf("era 1 first second = %d, want 0", secs)
	}
}

func TestShortFormat(t *testing.T) {
	if d := Short(0x00010000).Duration(); d != time.Second {
		t.Errorf("1.0 short = %v", d)
	}
	if d := Short(0x00008000).Duration(); d != 500*time.Millisecond {
		t.Errorf("0.5 short = %v", d)
	}
	if s := ShortFromDuration(1500 * time.Millisecond); s != 0x00018000 {
		t.Errorf("1.5 s = %#x", uint32(s))
	}
	if s := ShortFromDuration(-time.Second); s != 0 {
		t.Errorf("negative = %#x", uint32(s))
	}
	if d := log2Duration(-20); d < 953*time.Nanosecond || d > 954*time.Nanosecond {
		t.Errorf("2^-20 s = %v", d)
	}
	if d := log2Duration(6); d != 64*time.Second {
		t.Errorf("2^6 s = %v", d)
	}
}

func TestOffsetDelayMaths(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	cases := []struct {
		name               string
		t1, t2, t3, t4     time.Duration
		wantOff, wantDelay time.Duration
	}{
		// Server 1 s ahead, 10 ms each way, 2 ms processing.
		{"server ahead", 0, ms(1010), ms(1012), ms(22), ms(1000), ms(20)},
		// Server 250 ms behind, symmetric 40 ms path, no processing time.
		{"server behind", 0, ms(-210), ms(-210), ms(80), ms(-250), ms(80)},
		// Perfectly synchronised, asymmetric path (5 ms out, 15 ms back):
		// the asymmetry shows up as half the difference.
		{"asymmetric", 0, ms(5), ms(5), ms(20), ms(-5), ms(20)},
		{"zero", 0, 0, 0, 0, 0, 0},
	}
	for _, c := range cases {
		off, delay := offsetDelay(base.Add(c.t1), base.Add(c.t2), base.Add(c.t3), base.Add(c.t4))
		if off != c.wantOff || delay != c.wantDelay {
			t.Errorf("%s: offset %v delay %v, want %v %v", c.name, off, delay, c.wantOff, c.wantDelay)
		}
	}
}

func TestFormatRefID(t *testing.T) {
	cases := []struct {
		stratum int
		id      uint32
		want    string
	}{
		{1, 0x47505300, "GPS"},
		{1, 0x50505320, "PPS"}, // space padded
		{1, 0x47414C00, "GAL"},
		{0, 0x52415445, "RATE"},
		{2, 0xC0000201, "192.0.2.1"},
		{3, 0x0A000001, "10.0.0.1"},
		{1, 0x01020304, "0x01020304"}, // unprintable
		{1, 0, ""},
	}
	for _, c := range cases {
		if got := FormatRefID(c.stratum, c.id); got != c.want {
			t.Errorf("FormatRefID(%d, %#x) = %q, want %q", c.stratum, c.id, got, c.want)
		}
	}
}

func TestNormalizeServer(t *testing.T) {
	ok := map[string]string{
		"pool.example.org":      "pool.example.org:123",
		" ntp.lan ":             "ntp.lan:123",
		"ntp.lan:1123":          "ntp.lan:1123",
		"ntp.lan:":              "ntp.lan:123",
		"192.168.1.10":          "192.168.1.10:123",
		"192.168.1.10:12300":    "192.168.1.10:12300",
		"::1":                   "[::1]:123",
		"2001:db8::1":           "[2001:db8::1]:123",
		"[2001:db8::1]":         "[2001:db8::1]:123",
		"[2001:db8::1]:1234":    "[2001:db8::1]:1234",
		"fe80::1%eth0":          "[fe80::1%eth0]:123",
		"[fe80::1%eth0]:123":    "[fe80::1%eth0]:123",
		"radarcape":             "radarcape:123",
		"TIME.EXAMPLE.ORG:0123": "TIME.EXAMPLE.ORG:0123",
	}
	for in, want := range ok {
		got, err := NormalizeServer(in)
		if err != nil || got != want {
			t.Errorf("NormalizeServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "a b", "http://x/", ":123", "host:port:extra", "[::1", "host:99999", "[nope]"} {
		if got, err := NormalizeServer(in); err == nil {
			t.Errorf("NormalizeServer(%q) = %q, want error", in, got)
		}
	}
	if _, err := NormalizeServer(""); !errors.Is(err, ErrNoServer) {
		t.Errorf("empty server error = %v", err)
	}
}

// fakeServer is a minimal NTP server on a loopback UDP socket. handler maps a
// request to the datagrams to send back (possibly none, possibly several).
type fakeServer struct {
	conn     *net.UDPConn
	addr     string
	requests atomic.Int64
	wg       sync.WaitGroup
}

func startFakeServer(t *testing.T, network, listen string, handler func(req Packet, now time.Time) [][]byte) *fakeServer {
	t.Helper()
	la, err := net.ResolveUDPAddr(network, listen)
	if err != nil {
		t.Skipf("resolve %s: %v", listen, err)
	}
	conn, err := net.ListenUDP(network, la)
	if err != nil {
		t.Skipf("listen %s %s: %v", network, listen, err)
	}
	fs := &fakeServer{conn: conn, addr: conn.LocalAddr().String()}
	fs.wg.Add(1)
	go func() {
		defer fs.wg.Done()
		buf := make([]byte, 512)
		for {
			n, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			now := preciseNow()
			req, err := ParsePacket(buf[:n])
			if err != nil {
				continue
			}
			fs.requests.Add(1)
			for _, out := range handler(req, now) {
				_, _ = conn.WriteToUDP(out, peer)
			}
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		fs.wg.Wait()
	})
	return fs
}

// goodReply builds a valid server reply for req with the server clock running
// `skew` ahead of the host clock.
func goodReply(req Packet, now time.Time, skew time.Duration) Packet {
	srv := now.Add(skew)
	return Packet{
		Leap:           LeapNone,
		Version:        req.Version,
		Mode:           ModeServer,
		Stratum:        1,
		Poll:           req.Poll,
		Precision:      -20,
		RootDelay:      ShortFromDuration(0),
		RootDispersion: ShortFromDuration(2 * time.Millisecond),
		RefID:          0x47505300, // "GPS"
		Reference:      TimestampFromTime(srv.Add(-3 * time.Second)),
		Origin:         req.Transmit,
		Receive:        TimestampFromTime(srv),
		Transmit:       TimestampFromTime(srv),
	}
}

func marshal(p Packet) []byte {
	b := p.Marshal()
	return b[:]
}

func TestQueryAgainstFakeServer(t *testing.T) {
	const skew = 1500 * time.Millisecond
	fs := startFakeServer(t, "udp4", "127.0.0.1:0", func(req Packet, now time.Time) [][]byte {
		if req.Mode != ModeClient || req.Version != 4 {
			return nil
		}
		return [][]byte{marshal(goodReply(req, now, skew))}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Query(ctx, fs.addr)
	if err != nil {
		t.Fatal(err)
	}
	if d := r.Offset - skew; d > 100*time.Millisecond || d < -100*time.Millisecond {
		t.Errorf("offset = %v, want ≈ %v", r.Offset, skew)
	}
	if r.Delay < 0 || r.Delay > 500*time.Millisecond {
		t.Errorf("delay = %v", r.Delay)
	}
	if r.Stratum != 1 || r.RefID != "GPS" || r.Leap != LeapNone || r.Version != 4 {
		t.Errorf("stratum %d refid %q leap %d version %d", r.Stratum, r.RefID, r.Leap, r.Version)
	}
	if r.Precision != -20 || r.RootDispersion < 1900*time.Microsecond || r.RootDispersion > 2100*time.Microsecond {
		t.Errorf("precision %d root dispersion %v", r.Precision, r.RootDispersion)
	}
	if r.RootDistance < r.RootDispersion {
		t.Errorf("root distance %v < root dispersion %v", r.RootDistance, r.RootDispersion)
	}
	if r.Server != fs.addr || r.Addr == "" {
		t.Errorf("server %q addr %q", r.Server, r.Addr)
	}
	if age := r.TransmitTime.Sub(r.ReferenceTime); age < 2*time.Second || age > 4*time.Second {
		t.Errorf("reference age = %v", age)
	}
	// A negative skew works the same way.
	fs2 := startFakeServer(t, "udp4", "127.0.0.1:0", func(req Packet, now time.Time) [][]byte {
		return [][]byte{marshal(goodReply(req, now, -40*time.Second))}
	})
	r, err = Query(ctx, fs2.addr)
	if err != nil {
		t.Fatal(err)
	}
	if d := r.Offset + 40*time.Second; d > 100*time.Millisecond || d < -100*time.Millisecond {
		t.Errorf("offset = %v, want ≈ -40s", r.Offset)
	}
}

func TestQueryIPv6(t *testing.T) {
	fs := startFakeServer(t, "udp6", "[::1]:0", func(req Packet, now time.Time) [][]byte {
		p := goodReply(req, now, 0)
		p.Stratum = 2
		p.RefID = 0xC0000201
		return [][]byte{marshal(p)}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Query(ctx, fs.addr)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stratum != 2 || r.RefID != "192.0.2.1" {
		t.Errorf("stratum %d refid %q", r.Stratum, r.RefID)
	}
	if r.Offset > 100*time.Millisecond || r.Offset < -100*time.Millisecond {
		t.Errorf("offset = %v", r.Offset)
	}
}

func TestQueryIgnoresForeignPackets(t *testing.T) {
	fs := startFakeServer(t, "udp4", "127.0.0.1:0", func(req Packet, now time.Time) [][]byte {
		spoof := goodReply(req, now, 3600*time.Second)
		spoof.Origin = req.Transmit ^ 0xFFFF // wrong nonce
		return [][]byte{
			{1, 2, 3},      // garbage
			marshal(spoof), // spoofed / stale reply
			marshal(goodReply(req, now, 0)),
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Query(ctx, fs.addr)
	if err != nil {
		t.Fatal(err)
	}
	if r.Offset > 100*time.Millisecond || r.Offset < -100*time.Millisecond {
		t.Errorf("offset = %v: the spoofed packet was accepted", r.Offset)
	}
}

func TestQueryRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *Packet)
		check  func(t *testing.T, err error)
	}{
		{"kiss RATE", func(p *Packet) { p.Stratum = 0; p.RefID = 0x52415445 }, func(t *testing.T, err error) {
			var ke *KissError
			if !errors.As(err, &ke) || ke.Code != "RATE" || !errors.Is(err, ErrKissOfDeath) {
				t.Errorf("err = %v", err)
			}
		}},
		{"kiss DENY with alarm", func(p *Packet) { p.Stratum = 0; p.Leap = LeapNotInSync; p.RefID = 0x44454E59 }, func(t *testing.T, err error) {
			var ke *KissError
			if !errors.As(err, &ke) || ke.Code != "DENY" {
				t.Errorf("err = %v", err)
			}
		}},
		{"leap alarm", func(p *Packet) { p.Leap = LeapNotInSync }, func(t *testing.T, err error) {
			if !errors.Is(err, ErrUnsynchronized) {
				t.Errorf("err = %v", err)
			}
		}},
		{"stratum 16", func(p *Packet) { p.Stratum = 16 }, func(t *testing.T, err error) {
			if !errors.Is(err, ErrUnsynchronized) {
				t.Errorf("err = %v", err)
			}
		}},
		{"root distance", func(p *Packet) { p.RootDispersion = ShortFromDuration(17 * time.Second) }, func(t *testing.T, err error) {
			if !errors.Is(err, ErrUnsynchronized) {
				t.Errorf("err = %v", err)
			}
		}},
		{"client mode echo", func(p *Packet) { p.Mode = ModeClient }, func(t *testing.T, err error) {
			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("err = %v", err)
			}
		}},
		{"zero transmit", func(p *Packet) { p.Transmit = 0 }, func(t *testing.T, err error) {
			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("err = %v", err)
			}
		}},
		{"transmit before receive", func(p *Packet) { p.Transmit = p.Receive - 1<<32 }, func(t *testing.T, err error) {
			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("err = %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := startFakeServer(t, "udp4", "127.0.0.1:0", func(req Packet, now time.Time) [][]byte {
				p := goodReply(req, now, 0)
				c.mutate(&p)
				return [][]byte{marshal(p)}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := Query(ctx, fs.addr)
			if err == nil {
				t.Fatalf("accepted: %+v", r)
			}
			c.check(t, err)
		})
	}
}

func TestQueryTimeoutAndCancel(t *testing.T) {
	fs := startFakeServer(t, "udp4", "127.0.0.1:0", func(Packet, time.Time) [][]byte { return nil })
	c := &Client{Timeout: 150 * time.Millisecond}
	start := time.Now()
	_, err := c.Query(context.Background(), fs.addr)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want timeout", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("timeout took %v", el)
	}
	if fs.requests.Load() != 1 {
		t.Errorf("server saw %d requests", fs.requests.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start = time.Now()
	_, err = (&Client{Timeout: 10 * time.Second}).Query(ctx, fs.addr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("cancel took %v", el)
	}

	if _, err := Query(context.Background(), "  "); !errors.Is(err, ErrNoServer) {
		t.Errorf("empty server: %v", err)
	}
}

// scripted is a fake Query function returning queued results.
type scripted struct {
	mu    sync.Mutex
	queue []scriptedResult
	calls int
}

type scriptedResult struct {
	offset, delay time.Duration
	err           error
}

func (s *scripted) push(r ...scriptedResult) {
	s.mu.Lock()
	s.queue = append(s.queue, r...)
	s.mu.Unlock()
}

func (s *scripted) query(_ context.Context, server string) (*Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.queue) == 0 {
		return nil, ErrTimeout
	}
	r := s.queue[0]
	s.queue = s.queue[1:]
	if r.err != nil {
		return nil, r.err
	}
	return &Response{
		Server: server, Time: time.Now(), Offset: r.offset, Delay: r.delay,
		Stratum: 1, RefID: "GPS",
	}, nil
}

func newTestSampler(sc *scripted, burst int) *Sampler {
	return NewSampler(
		model.NTPConfig{Enabled: true, Server: "ntp.test", IntervalSec: 64},
		Options{Burst: burst, BurstGap: time.Millisecond, Query: sc.query, HistorySize: 16},
	)
}

func msDur(f float64) time.Duration { return time.Duration(f * float64(time.Millisecond)) }

func TestSamplerBurstKeepsMinimumDelay(t *testing.T) {
	sc := &scripted{}
	sc.push(
		scriptedResult{offset: msDur(30), delay: msDur(12)},
		scriptedResult{err: ErrTimeout},
		scriptedResult{offset: msDur(10), delay: msDur(2)}, // best
		scriptedResult{offset: msDur(22), delay: msDur(7)},
	)
	s := newTestSampler(sc, 4)
	var got []Sample
	s.OnSample(func(x Sample) { got = append(got, x) })

	if s.Synced() || s.Offset() != 0 {
		t.Fatal("synced before the first sample")
	}
	smp, err := s.SampleNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(smp.OffsetMs-10) > 1e-9 || math.Abs(smp.DelayMs-2) > 1e-9 {
		t.Errorf("kept offset %.3f delay %.3f, want the minimum-delay reply (10, 2)", smp.OffsetMs, smp.DelayMs)
	}
	if smp.Sent != 4 || smp.Replies != 3 {
		t.Errorf("sent %d replies %d", smp.Sent, smp.Replies)
	}
	if len(got) != 1 || got[0] != smp {
		t.Errorf("OnSample got %+v", got)
	}
	if !s.Synced() || s.Offset() != 10*time.Millisecond {
		t.Errorf("synced %v offset %v", s.Synced(), s.Offset())
	}
	st := s.Status()
	if !st.Enabled || !st.Synced || st.Server != "ntp.test" || st.Stratum != 1 || st.RefID != "GPS" ||
		math.Abs(st.OffsetMs-10) > 1e-9 || math.Abs(st.DelayMs-2) > 1e-9 || st.LastOK != smp.T || st.LastError != "" {
		t.Errorf("status = %+v", st)
	}
}

func TestSamplerFailureAndEarlyAbort(t *testing.T) {
	sc := &scripted{}
	s := newTestSampler(sc, 4)
	_, err := s.SampleNow(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if sc.calls != 2 {
		t.Errorf("burst sent %d queries to a dead server, want 2", sc.calls)
	}
	st := s.Status()
	if st.Synced || st.LastError == "" {
		t.Errorf("status = %+v", st)
	}
	// A kiss-o'-death stops the burst at once and lengthens the next delay.
	sc.push(scriptedResult{err: &KissError{Code: "RATE"}})
	sc.calls = 0
	_, err = s.SampleNow(context.Background())
	if !errors.Is(err, ErrKissOfDeath) || sc.calls != 1 {
		t.Errorf("err = %v calls = %d", err, sc.calls)
	}
	base := 64 * time.Second
	d := s.nextDelay(s.Config(), err)
	if d < time.Duration(1.9*float64(base)) || d > time.Duration(2.1*float64(base)) {
		t.Errorf("delay after RATE = %v, want ≈ 2×interval", d)
	}
	d = s.nextDelay(s.Config(), nil)
	if d < time.Duration(0.95*float64(base)) || d > time.Duration(1.05*float64(base)) {
		t.Errorf("delay after success = %v, want interval ±5%%", d)
	}
	d = s.nextDelay(s.Config(), &KissError{Code: "DENY"})
	if d < 57*time.Minute {
		t.Errorf("delay after DENY = %v, want ≈ 1 h", d)
	}
	stats := s.Stats(0)
	if stats.Polls != 2 || stats.Failures != 2 || stats.Count != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

// Public servers rate-limit bursts; the sampler must notice and stop sending them.
func TestSamplerAdaptsToRateLimitedServer(t *testing.T) {
	ctx := context.Background()
	ok := scriptedResult{offset: msDur(3), delay: msDur(1)}

	// A RATE kiss in the middle of a burst: keep the reply, stop bursting.
	sc := &scripted{}
	s := newTestSampler(sc, 4)
	sc.push(ok, scriptedResult{err: &KissError{Code: "RATE"}})
	smp, err := s.SampleNow(ctx)
	if err != nil || smp.Sent != 2 || smp.Replies != 1 {
		t.Fatalf("burst with a RATE kiss: %+v, %v", smp, err)
	}
	sc.push(ok, ok, ok, ok)
	sc.calls = 0
	if smp, err = s.SampleNow(ctx); err != nil || sc.calls != 1 || smp.Sent != 1 {
		t.Errorf("after a RATE kiss the burst sent %d queries, want 1 (%v)", sc.calls, err)
	}
	// A different server gets full bursts again.
	s.Reconfigure(model.NTPConfig{Enabled: true, Server: "lan.test", IntervalSec: 64})
	sc.mu.Lock()
	sc.queue, sc.calls = nil, 0
	sc.mu.Unlock()
	sc.push(ok, ok, ok, ok)
	if smp, err = s.SampleNow(ctx); err != nil || sc.calls != 4 || smp.Replies != 4 {
		t.Errorf("burst after server change: %d queries, %v", sc.calls, err)
	}

	// Silent rate limiting: the first query is answered, the rest is dropped.
	// Once is a glitch, twice in a row is a policy.
	sc = &scripted{}
	s = newTestSampler(sc, 4)
	lost := scriptedResult{err: ErrTimeout}
	for i := 0; i < 2; i++ {
		sc.push(ok, lost, lost)
		sc.calls = 0
		if smp, err = s.SampleNow(ctx); err != nil || sc.calls != 3 || smp.Replies != 1 {
			t.Fatalf("burst %d with a dropped tail: %d queries, %v", i, sc.calls, err)
		}
	}
	sc.push(ok, ok)
	sc.calls = 0
	if _, err = s.SampleNow(ctx); err != nil || sc.calls != 1 {
		t.Errorf("after two dropped tails the burst sent %d queries, want 1 (%v)", sc.calls, err)
	}
	// A single glitch followed by a clean burst does not switch.
	sc = &scripted{}
	s = newTestSampler(sc, 4)
	sc.push(ok, lost, lost)
	if _, err = s.SampleNow(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		sc.push(ok, ok, ok, ok)
		sc.calls = 0
		if _, err = s.SampleNow(ctx); err != nil || sc.calls != 4 {
			t.Errorf("clean burst %d sent %d queries, want 4 (%v)", i, sc.calls, err)
		}
	}
	sc.push(ok, lost, lost)
	sc.calls = 0
	if _, err = s.SampleNow(ctx); err != nil || sc.calls != 3 {
		t.Errorf("isolated second glitch: %d queries (%v)", sc.calls, err)
	}
	sc.push(ok, ok, ok, ok)
	sc.calls = 0
	if _, err = s.SampleNow(ctx); err != nil || sc.calls != 4 {
		t.Errorf("glitches that are not consecutive switched bursts off (%d queries, %v)", sc.calls, err)
	}
}

func TestSamplerSmoothingJitterAndStep(t *testing.T) {
	sc := &scripted{}
	s := newTestSampler(sc, 1)
	feed := func(offMs float64) Sample {
		t.Helper()
		sc.push(scriptedResult{offset: msDur(offMs), delay: msDur(1)})
		smp, err := s.SampleNow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return smp
	}
	// Stable clock around 5 ms with one wild outlier: the median ignores it.
	for _, o := range []float64{5.0, 5.2, 4.8, 5.1} {
		feed(o)
	}
	smp := feed(400) // outlier (e.g. a delayed packet)
	if math.Abs(smp.SmoothedMs-5.1) > 1e-4 {
		t.Errorf("smoothed after outlier = %.3f, want 5.1", smp.SmoothedMs)
	}
	smp = feed(5.0)
	if math.Abs(smp.SmoothedMs-5.05) > 1e-4 {
		t.Errorf("smoothed = %.3f, want 5.05", smp.SmoothedMs)
	}
	if s.Stats(0).Steps != 0 {
		t.Error("single outlier counted as a clock step")
	}
	// Jitter = RMS of successive differences over the recent window.
	want := rmsSuccessive([]float64{5.0, 5.2, 4.8, 5.1, 400, 5.0})
	if math.Abs(smp.JitterMs-want) > 1e-4 {
		t.Errorf("jitter = %.4f, want %.4f", smp.JitterMs, want)
	}
	// The host clock is stepped by -250 ms: two agreeing samples re-seed the filter.
	feed(255.0)
	smp = feed(255.3)
	if math.Abs(smp.SmoothedMs-255.15) > 1e-4 {
		t.Errorf("smoothed after step = %.3f, want 255.15", smp.SmoothedMs)
	}
	if got := s.Offset(); got < 255*time.Millisecond || got > 256*time.Millisecond {
		t.Errorf("Offset() = %v", got)
	}
	if s.Stats(0).Steps != 1 {
		t.Errorf("steps = %d, want 1", s.Stats(0).Steps)
	}
	if math.Abs(smp.JitterMs-0.3) > 1e-4 {
		t.Errorf("jitter after step = %.4f, want 0.3", smp.JitterMs)
	}
}

func TestSamplerHistoryRingAndSeed(t *testing.T) {
	sc := &scripted{}
	s := newTestSampler(sc, 1) // ring capacity 16
	for i := 0; i < 20; i++ {
		sc.push(scriptedResult{offset: msDur(float64(i)), delay: msDur(1)})
		if _, err := s.SampleNow(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h := s.History(time.Time{})
	if len(h) != 16 {
		t.Fatalf("history length %d, want 16", len(h))
	}
	if math.Abs(h[0].OffsetMs-4) > 1e-9 || math.Abs(h[15].OffsetMs-19) > 1e-9 {
		t.Errorf("ring kept %v .. %v, want 4 .. 19", h[0].OffsetMs, h[15].OffsetMs)
	}
	for i := 1; i < len(h); i++ {
		if h[i].T < h[i-1].T {
			t.Fatal("history not in time order")
		}
	}
	if got := s.History(time.Now().Add(time.Hour)); len(got) != 0 {
		t.Errorf("future since returned %d samples", len(got))
	}

	// Seeding a fresh sampler keeps order and only prepends older samples.
	s2 := newTestSampler(sc, 1)
	sc.push(scriptedResult{offset: msDur(1), delay: msDur(1)})
	live, _ := s2.SampleNow(context.Background())
	s2.Seed([]Sample{
		{T: live.T - 2000, OffsetMs: -2},
		{T: live.T - 3000, OffsetMs: -3},
		{T: live.T + 5000, OffsetMs: 99}, // newer than live data: dropped
	})
	h = s2.History(time.Time{})
	if len(h) != 3 || h[0].OffsetMs != -3 || h[1].OffsetMs != -2 || h[2].T != live.T {
		t.Errorf("seeded history = %+v", h)
	}
	if got := s2.Offset(); got != time.Millisecond {
		t.Errorf("seed changed Offset(): %v", got)
	}
}

func TestSamplerDrift(t *testing.T) {
	// 20 ppm slow host clock: offset grows 20 µs per second.
	var h []Sample
	for i := 0; i < 30; i++ {
		tSec := float64(i * 64)
		h = append(h, Sample{T: 1_700_000_000_000 + int64(tSec*1000), OffsetMs: 3 + tSec*20e-6*1e3})
	}
	ppm, ok := driftPPM(h)
	if !ok || math.Abs(ppm-20) > 1e-6 {
		t.Errorf("drift = %v %v, want 20 ppm", ppm, ok)
	}
	if _, ok := driftPPM(h[:3]); ok {
		t.Error("drift from 3 samples should be unavailable")
	}
}

func TestSamplerReconfigureAndDisabled(t *testing.T) {
	sc := &scripted{}
	s := newTestSampler(sc, 1)
	sc.push(scriptedResult{offset: msDur(7), delay: msDur(1)})
	if _, err := s.SampleNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.Synced() {
		t.Fatal("not synced")
	}
	// Interval-only change keeps the sync state.
	s.Reconfigure(model.NTPConfig{Enabled: true, Server: "ntp.test", IntervalSec: 16})
	if !s.Synced() || s.Config().IntervalSec != 16 {
		t.Errorf("after interval change: synced %v cfg %+v", s.Synced(), s.Config())
	}
	// Server change drops it.
	s.Reconfigure(model.NTPConfig{Enabled: true, Server: "other.test", IntervalSec: 16})
	if s.Synced() || s.Offset() != 0 {
		t.Error("still synced after server change")
	}
	if st := s.Status(); st.Server != "other.test" || st.LastOK != 0 || !st.Enabled {
		t.Errorf("status = %+v", st)
	}
	if len(s.History(time.Time{})) != 1 {
		t.Error("history ring lost on reconfigure")
	}
	// Disabled: no queries, no sync.
	s.Reconfigure(model.NTPConfig{Enabled: false, Server: "other.test"})
	if _, err := s.SampleNow(context.Background()); !errors.Is(err, ErrNoServer) {
		t.Errorf("SampleNow while disabled: %v", err)
	}
	if st := s.Status(); st.Enabled || st.Synced {
		t.Errorf("status = %+v", st)
	}
	// Interval clamping.
	s.Reconfigure(model.NTPConfig{Enabled: true, Server: "x", IntervalSec: 1})
	if got := s.Config().IntervalSec; got != MinIntervalSec {
		t.Errorf("interval = %d, want %d", got, MinIntervalSec)
	}
	s.Reconfigure(model.NTPConfig{Enabled: true, Server: "x", IntervalSec: 0})
	if got := s.Config().IntervalSec; got != DefaultIntervalSec {
		t.Errorf("interval = %d, want %d", got, DefaultIntervalSec)
	}
}

func TestSamplerSyncedExpires(t *testing.T) {
	sc := &scripted{}
	s := newTestSampler(sc, 1)
	sc.push(scriptedResult{offset: msDur(7), delay: msDur(1)})
	if _, err := s.SampleNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Pretend the last good sample is 4 intervals old.
	s.mu.Lock()
	s.lastOK = time.Now().Add(-4 * 64 * time.Second)
	s.mu.Unlock()
	if s.Synced() {
		t.Error("Synced() true 4 intervals after the last sample")
	}
	if s.Offset() != 7*time.Millisecond {
		t.Errorf("Offset() = %v during holdover, want 7ms", s.Offset())
	}
	s.mu.Lock()
	s.lastOK = time.Now().Add(-16 * time.Minute)
	s.mu.Unlock()
	if s.Offset() != 0 {
		t.Errorf("Offset() = %v after holdover, want 0", s.Offset())
	}
}

// End to end: Run loop against a real UDP socket with a known artificial offset.
func TestSamplerRunEndToEnd(t *testing.T) {
	const skew = -2750 * time.Millisecond
	fs := startFakeServer(t, "udp4", "127.0.0.1:0", func(req Packet, now time.Time) [][]byte {
		return [][]byte{marshal(goodReply(req, now, skew))}
	})
	s := NewSampler(model.NTPConfig{Enabled: false}, Options{BurstGap: 5 * time.Millisecond})
	samples := make(chan Sample, 4)
	s.OnSample(func(x Sample) {
		select {
		case samples <- x:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Disabled: nothing is sent.
	time.Sleep(60 * time.Millisecond)
	if fs.requests.Load() != 0 {
		t.Fatal("queries sent while disabled")
	}
	// Enabling live starts sampling immediately.
	s.Reconfigure(model.NTPConfig{Enabled: true, Server: fs.addr, IntervalSec: 64})
	select {
	case smp := <-samples:
		if math.Abs(smp.OffsetMs-float64(skew.Milliseconds())) > 100 {
			t.Errorf("sample offset %.1f ms, want ≈ %d ms", smp.OffsetMs, skew.Milliseconds())
		}
		if smp.Sent != DefaultBurst || smp.Replies != DefaultBurst {
			t.Errorf("burst sent %d replies %d", smp.Sent, smp.Replies)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no sample within 5 s")
	}
	if !s.Synced() {
		t.Error("not synced after first sample")
	}
	if d := s.Offset() - skew; d > 100*time.Millisecond || d < -100*time.Millisecond {
		t.Errorf("Offset() = %v, want ≈ %v", s.Offset(), skew)
	}
	if got := fs.requests.Load(); got != DefaultBurst {
		t.Errorf("server saw %d requests, want %d", got, DefaultBurst)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// Live test against a real NTP server; opt in with AIRVIEW_LIVE_TESTS=1.
// AIRVIEW_NTP_SERVER overrides the server (default pool.ntp.org).
func TestLiveQuery(t *testing.T) {
	if os.Getenv("AIRVIEW_LIVE_TESTS") == "" {
		t.Skip("set AIRVIEW_LIVE_TESTS=1 to query a real NTP server")
	}
	server := os.Getenv("AIRVIEW_NTP_SERVER")
	if server == "" {
		server = "pool.ntp.org"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := Query(ctx, server)
	if err != nil {
		t.Fatalf("Query(%s): %v", server, err)
	}
	t.Logf("%s (%s): offset %v delay %v stratum %d refid %q leap %d version %d poll %d precision %d (%v) rootdelay %v rootdisp %v rootdist %v ref %v",
		r.Server, r.Addr, r.Offset, r.Delay, r.Stratum, r.RefID, r.Leap, r.Version, r.Poll, r.Precision, r.PrecisionDur,
		r.RootDelay, r.RootDispersion, r.RootDistance, r.ReferenceTime.Format(time.RFC3339))
	if r.Stratum < 1 || r.Stratum > 15 || r.Delay <= 0 || r.Delay > 2*time.Second || r.RefID == "" {
		t.Errorf("implausible response: %+v", *r)
	}
	if r.Offset > time.Hour || r.Offset < -time.Hour {
		t.Errorf("offset %v: era or epoch handling is off (or the host clock is very wrong)", r.Offset)
	}
	if age := r.Time.Sub(r.ReferenceTime); r.ReferenceTime.IsZero() || age < -time.Minute || age > 48*time.Hour {
		t.Errorf("reference time %v is implausible", r.ReferenceTime)
	}
}
