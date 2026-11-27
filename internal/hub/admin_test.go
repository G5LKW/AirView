package hub

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuf is a goroutine-safe log sink for slog.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), substr)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startAdminServer starts an admin-only server whose log is captured.
func startAdminServer(t *testing.T, opts Options) (*Server, int, *logBuf) {
	t.Helper()
	lb := &logBuf{}
	opts.AdminOnly = true
	opts.Port = -1
	opts.BindAddr = "127.0.0.1"
	opts.Logger = slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if opts.FlushInterval == 0 {
		opts.FlushInterval = time.Millisecond
	}
	s := NewServer(opts)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, s.Addr().(*net.TCPAddr).Port, lb
}

// beastFrame is one Beast-format Mode S short frame as a feeder would send it.
var beastFrame = []byte{0x1A, '2', 0, 0, 0, 0, 0, 0, 0xC8, 0x5D, 0x8D, 0x48, 0x40, 0xD6, 0x20, 0x2C}

// probe connects, sends payload (may be nil), and returns everything the
// server sent until it closed the connection. It fails the test when the
// server does not close the connection within wait.
func probe(t *testing.T, port int, payload []byte, wait time.Duration) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if payload != nil {
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	conn.SetReadDeadline(time.Now().Add(wait))
	var got bytes.Buffer
	_, err = io.Copy(&got, conn)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("server kept the connection open for %v (got %q)", wait, got.String())
	}
	// A reset (unread input on close) is fine: the server closed it.
	return got.String()
}

// onlyGreeting checks that the server sent its greeting and nothing else
// (in particular no ERR line).
func onlyGreeting(t *testing.T, what, got string) {
	t.Helper()
	if strings.Contains(got, "ERR") {
		t.Errorf("%s: server replied %q, want a quiet close", what, got)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if got != "" && (len(lines) != 1 || !strings.HasPrefix(lines[0], "AIRVIEW-HUB 1 hmac-sha256 ")) {
		t.Errorf("%s: server sent %q, want just the greeting", what, got)
	}
}

const quietMsg = "closed non-hub client"

func TestAdminOnlyBeastClosedAndLoggedOnce(t *testing.T) {
	srv, port, lb := startAdminServer(t, Options{Password: "pw", FailLimit: 2})

	// An ADSBx-style results connector: connects and streams Beast frames,
	// reconnecting whenever it is dropped.
	const attempts = 6
	for i := 0; i < attempts; i++ {
		start := time.Now()
		got := probe(t, port, bytes.Repeat(beastFrame, 4), 5*time.Second)
		onlyGreeting(t, "beast", got)
		if d := time.Since(start); d > time.Second {
			t.Errorf("beast connect %d closed after %v, want immediately", i, d)
		}
	}
	waitFor(t, "rejection count", func() bool { return srv.QuietRejections() == attempts })
	if n := lb.count(quietMsg); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, lb)
	}
	if !strings.Contains(lb.String(), "0x1A") {
		t.Errorf("log line does not name the reason:\n%s", lb)
	}
	if n := srv.Status().Clients; n != 0 {
		t.Errorf("%d clients after beast connects", n)
	}

	// The quiet closes did not count as failed logins: the administrator
	// (same IP) still gets in with the password, even with FailLimit 2.
	_, _, reply := rawLogin(t, port, "pw")
	if !strings.HasPrefix(reply, "OK ") {
		t.Fatalf("admin login after beast connects: %q", reply)
	}
	// A genuine hub client with a wrong password gets the normal reply.
	if _, _, reply = rawLogin(t, port, "bad"); !strings.HasPrefix(reply, "ERR auth") {
		t.Fatalf("wrong password: %q, want ERR auth", reply)
	}
}

func TestAdminOnlySilentAndGarbageClients(t *testing.T) {
	srv, port, lb := startAdminServer(t, Options{Password: "pw", ProbeTimeout: 200 * time.Millisecond})

	// Silent peer: closed once the probe window has passed, quietly.
	start := time.Now()
	onlyGreeting(t, "silent", probe(t, port, nil, 5*time.Second))
	if d := time.Since(start); d < 150*time.Millisecond || d > 3*time.Second {
		t.Errorf("silent peer closed after %v, want about 200 ms", d)
	}
	// A partial line that never completes counts as "no AUTH in time".
	onlyGreeting(t, "partial", probe(t, port, []byte("AUTH 00"), 5*time.Second))
	// Garbage lines, malformed AUTH and over-long lines are closed quietly.
	for _, junk := range []string{"HELLO\n", "GET / HTTP/1.1\r\n\r\n", "AUTH zz yy\n", "AUTH " + strings.Repeat("ab", 32) + "\n", strings.Repeat("x", 2000) + "\n"} {
		onlyGreeting(t, "junk "+junk[:min(len(junk), 10)], probe(t, port, []byte(junk), 5*time.Second))
	}
	waitFor(t, "rejection count", func() bool { return srv.QuietRejections() == 7 })
	if n := lb.count(quietMsg); n != 1 {
		t.Errorf("logged %d times for one IP, want once:\n%s", n, lb)
	}
	if n := srv.Status().Clients; n != 0 {
		t.Errorf("%d clients after failed probes", n)
	}
}

func TestAdminOnlyRelogsAfterIntervalWithSuppressedCount(t *testing.T) {
	srv, port, lb := startAdminServer(t, Options{Password: "pw", QuietLogInterval: 300 * time.Millisecond})
	for i := 0; i < 3; i++ {
		probe(t, port, beastFrame, 5*time.Second)
	}
	waitFor(t, "rejections", func() bool { return srv.QuietRejections() == 3 })
	if n := lb.count(quietMsg); n != 1 {
		t.Fatalf("logged %d times, want 1", n)
	}
	time.Sleep(350 * time.Millisecond)
	probe(t, port, beastFrame, 5*time.Second)
	waitFor(t, "second log line", func() bool { return lb.count(quietMsg) == 2 })
	if !strings.Contains(lb.String(), "suppressed=2") {
		t.Errorf("second log line does not report the 2 suppressed closes:\n%s", lb)
	}
}

func TestAdminOnlyRealClientWorks(t *testing.T) {
	srv, port, _ := startAdminServer(t, Options{Password: "pw"})
	sink := &collector{}
	client, _ := runClient(t, ClientOptions{Port: port, Password: "pw"}, sink)
	waitFor(t, "login", func() bool { return client.Connected() && srv.Status().Clients == 1 })
	for _, f := range testFrames() {
		srv.Broadcast(f)
	}
	waitFor(t, "frames", func() bool { n, _ := sink.counts(); return n == len(testFrames()) })
	if srv.QuietRejections() != 0 {
		t.Errorf("genuine client counted as quiet rejection")
	}
}

func TestAdminOnlyWithoutPasswordRefusesEveryone(t *testing.T) {
	srv, port, lb := startAdminServer(t, Options{})
	states := &stateRec{}
	client, _ := runClient(t, ClientOptions{Port: port, OnState: states.on}, &collector{})
	waitFor(t, "auth error", func() bool { return errors.Is(states.lastErr(), ErrAuth) })
	if client.Connected() || srv.Status().Clients != 0 {
		t.Fatal("open login accepted in admin-only mode")
	}
	_, _, reply := rawLogin(t, port, "")
	if !strings.HasPrefix(reply, "ERR auth") {
		t.Fatalf("reply %q, want ERR auth", reply)
	}
	if n := lb.count("data port login refused"); n != 1 {
		t.Errorf("no-password warning logged %d times, want once:\n%s", n, lb)
	}
}

func TestSetAdminOnlyAtRunTime(t *testing.T) {
	srv, port := startServer(t, Options{}) // standalone, open access
	if srv.AdminOnly() {
		t.Fatal("AdminOnly default should be false")
	}
	_, _, reply := rawLogin(t, port, "")
	if !strings.HasPrefix(reply, "OK ") {
		t.Fatalf("open login: %q", reply)
	}
	waitFor(t, "registration", func() bool { return srv.Status().Clients == 1 })

	srv.SetAdminOnly(true)
	waitFor(t, "open client dropped", func() bool { return srv.Status().Clients == 0 })
	if _, _, reply = rawLogin(t, port, ""); !strings.HasPrefix(reply, "ERR auth") {
		t.Fatalf("open login in admin-only mode: %q", reply)
	}
	srv.SetPassword("pw")
	if _, _, reply = rawLogin(t, port, "pw"); !strings.HasPrefix(reply, "OK ") {
		t.Fatalf("admin login: %q", reply)
	}

	// Back to standalone: non-hub input gets the normal ERR proto reply again.
	srv.SetAdminOnly(false)
	conn, br, _ := dialGreeting(t, port)
	io.WriteString(conn, "HELLO\n")
	if line, _ := readLine(br); !strings.HasPrefix(line, "ERR proto") {
		t.Errorf("standalone reply %q, want ERR proto", line)
	}
	// Only the one no-password refusal above was counted.
	if srv.QuietRejections() != 1 {
		t.Errorf("quiet rejections = %d, want 1", srv.QuietRejections())
	}
}

func TestQuietLimiterBounded(t *testing.T) {
	s := NewServer(Options{Logger: quiet, QuietLogInterval: time.Hour})
	now := time.Now()
	for i := 0; i < maxQuietKeys+500; i++ {
		ip := "10.0." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256)
		s.quietReject(ip, ip, ip+":1", quietBeast, now)
	}
	if n := len(s.quiet); n > maxQuietKeys {
		t.Errorf("limiter holds %d keys, want <= %d", n, maxQuietKeys)
	}
	s.mu.Lock()
	s.pruneQuietLocked(now.Add(2 * time.Hour))
	n := len(s.quiet)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d keys left after prune", n)
	}
}

func dialGreeting(t *testing.T, port int) (net.Conn, *bufio.Reader, string) {
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
	return conn, br, line
}

func BenchmarkQuietReject(b *testing.B) {
	s := NewServer(Options{Logger: quiet})
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.quietReject("192.0.2.1", "192.0.2.1", "192.0.2.1:5000", quietBeast, now)
	}
}

// SEC-10: in admin-only (aggregator) mode no raw client address is logged.
func TestAdminOnlyLogsNoRawIP(t *testing.T) {
	srv, port, lb := startAdminServer(t, Options{Password: "pw", FailLimit: 2})
	probe(t, port, bytes.Repeat(beastFrame, 4), 5*time.Second)
	waitFor(t, "rejection", func() bool { return srv.QuietRejections() == 1 })
	out := lb.String()
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, quietMsg) && (strings.Contains(ln, "addr=") || strings.Contains(ln, "ip=")) {
			t.Errorf("raw address logged: %s", ln)
		}
	}
	if !strings.Contains(out, "net=127.0.0.0/24") {
		t.Errorf("network not logged:\n%s", out)
	}
	if got := maskedNet("[2001:db8:1:2::5]:80"); got != "2001:db8:1::/48" {
		t.Errorf("v6 mask %q", got)
	}
}
