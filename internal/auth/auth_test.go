package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

func TestMain(m *testing.M) {
	// 600k iterations cost ~0.2 s each; the default is checked once in
	// TestPasswordDefaultCost, everything else runs with a cheap cost.
	iterations = 2000
	os.Exit(m.Run())
}

// ---------------------------------------------------------------- passwords

func TestPasswordDefaultCost(t *testing.T) {
	h, err := HashPasswordCost("correct horse battery", DefaultIterations)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") {
		t.Fatalf("hash %q does not record 600000 iterations", h)
	}
	if !VerifyPassword(h, "correct horse battery") || VerifyPassword(h, "correct horse batterY") {
		t.Fatal("verify")
	}
}

func TestPasswordHash(t *testing.T) {
	h1, _ := HashPassword("pw-123456")
	h2, _ := HashPassword("pw-123456")
	if h1 == h2 {
		t.Fatal("salt not random")
	}
	if !VerifyPassword(h1, "pw-123456") || !VerifyPassword(h2, "pw-123456") {
		t.Fatal("verify failed")
	}
	for _, bad := range []string{"", "pw-12345", "pw-1234567", "PW-123456"} {
		if VerifyPassword(h1, bad) {
			t.Errorf("verified %q", bad)
		}
	}
	for _, mal := range []string{"", "sha256:00:00", "pbkdf2-sha256$x$a$b", "pbkdf2-sha256$10$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"pbkdf2-sha256$99999999$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA", "pbkdf2-sha256$2000$!!$AAAA", strings.Repeat("a", 300)} {
		if VerifyPassword(mal, "pw-123456") || ValidPasswordHash(mal) {
			t.Errorf("malformed hash %q accepted", mal)
		}
	}
	if NeedsRehash(h1) {
		t.Error("fresh hash needs rehash")
	}
	low, _ := HashPasswordCost("x", 1000)
	if !NeedsRehash(low) {
		t.Error("cheap hash does not need rehash")
	}
	if _, err := HashPassword(strings.Repeat("x", maxPassword+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Error("long password hashed")
	}
}

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		pw    string
		admin bool
		want  error
	}{
		{"short", false, ErrPasswordTooShort},
		{"eightchr", false, nil},
		{"eightchr", true, ErrPasswordTooShort},
		{"twelve-chars", true, nil},
		{"tab\there-pw", false, ErrPasswordInvalid},
		{"\xff\xfe-invalid-utf8", false, ErrPasswordInvalid},
		{"ünïcödé-pw", false, nil},
	}
	for _, c := range cases {
		if got := CheckPasswordPolicy(c.pw, c.admin); !errors.Is(got, c.want) && got != c.want {
			t.Errorf("%q admin=%v: %v, want %v", c.pw, c.admin, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- TOTP

// RFC 6238 Appendix B test vectors (8 digits, T0 = 0, X = 30).
func TestTOTPRFC6238Vectors(t *testing.T) {
	seeds := map[string][]byte{
		"SHA1":   []byte("12345678901234567890"),
		"SHA256": []byte("12345678901234567890123456789012"),
		"SHA512": []byte("1234567890123456789012345678901234567890123456789012345678901234"),
	}
	vectors := []struct {
		unix int64
		alg  string
		want string
	}{
		{59, "SHA1", "94287082"}, {59, "SHA256", "46119246"}, {59, "SHA512", "90693936"},
		{1111111109, "SHA1", "07081804"}, {1111111109, "SHA256", "68084774"}, {1111111109, "SHA512", "25091201"},
		{1111111111, "SHA1", "14050471"}, {1111111111, "SHA256", "67062674"}, {1111111111, "SHA512", "99943326"},
		{1234567890, "SHA1", "89005924"}, {1234567890, "SHA256", "91819424"}, {1234567890, "SHA512", "93441116"},
		{2000000000, "SHA1", "69279037"}, {2000000000, "SHA256", "90698825"}, {2000000000, "SHA512", "38618901"},
		{20000000000, "SHA1", "65353130"}, {20000000000, "SHA256", "77737706"}, {20000000000, "SHA512", "47863826"},
	}
	for _, v := range vectors {
		g := TOTP{Secret: seeds[v.alg], Algorithm: v.alg, Digits: 8, Period: 30}
		at := time.Unix(v.unix, 0).UTC()
		if got := g.Code(at); got != v.want {
			t.Errorf("T=%d %s: %s, want %s", v.unix, v.alg, got, v.want)
		}
		if _, ok := g.Verify(v.want, at, -1); !ok {
			t.Errorf("T=%d %s: Verify refused the vector", v.unix, v.alg)
		}
	}
}

// RFC 4226 Appendix D HOTP values (6 digits) for counters 0..9.
func TestHOTPRFC4226Vectors(t *testing.T) {
	g := TOTP{Secret: []byte("12345678901234567890")}
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for c, w := range want {
		if got := g.hotp(int64(c)); got != w {
			t.Errorf("counter %d: %s, want %s", c, got, w)
		}
	}
}

func TestTOTPVerifyWindowAndReplay(t *testing.T) {
	g := NewTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	step := g.Step(now)
	cur := g.Code(now)
	got, ok := g.Verify(cur, now, 0)
	if !ok || got != step {
		t.Fatalf("current code: %v %d", ok, got)
	}
	// Replay of the same code is refused.
	if _, ok := g.Verify(cur, now, got); ok {
		t.Error("replayed code accepted")
	}
	// One step of drift either way is fine; two are not.
	if _, ok := g.Verify(g.Code(now.Add(-30*time.Second)), now, 0); !ok {
		t.Error("previous step refused")
	}
	if _, ok := g.Verify(g.Code(now.Add(30*time.Second)), now, 0); !ok {
		t.Error("next step refused")
	}
	if _, ok := g.Verify(g.Code(now.Add(-61*time.Second)), now, 0); ok {
		t.Error("code two steps old accepted")
	}
	// An older code after a newer one was used is refused too.
	if _, ok := g.Verify(g.Code(now.Add(-30*time.Second)), now, step); ok {
		t.Error("older code accepted after a newer one")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "12 34 5x"} {
		if _, ok := g.Verify(bad, now, 0); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	// Spaces inside a code are tolerated ("123 456").
	if _, ok := g.Verify(cur[:3]+" "+cur[3:], now, 0); !ok {
		t.Error("spaced code refused")
	}
}

func TestTOTPSecretAndURI(t *testing.T) {
	g := NewTOTPSecret()
	if len(g.Secret) != TOTPSecretBytes {
		t.Fatal("secret size")
	}
	p, err := ParseTOTPSecret(strings.ToLower(g.Base32()[:4] + " " + g.Base32()[4:]))
	if err != nil || string(p.Secret) != string(g.Secret) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ParseTOTPSecret("not base32!"); err == nil {
		t.Error("bad secret parsed")
	}
	u := g.URI("AirView Network", "alice")
	if !strings.HasPrefix(u, "otpauth://totp/AirView%20Network:alice?") || !strings.Contains(u, "secret="+g.Base32()) ||
		!strings.Contains(u, "digits=6") || !strings.Contains(u, "period=30") || !strings.Contains(u, "algorithm=SHA1") {
		t.Errorf("uri %s", u)
	}
}

// ---------------------------------------------------------------- recovery codes

func TestRecoveryCodes(t *testing.T) {
	codes, hashes := NewRecoveryCodes(RecoveryCodeCount)
	if len(codes) != RecoveryCodeCount || len(hashes) != RecoveryCodeCount {
		t.Fatal("count")
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 11 || c[5] != '-' || seen[c] || !LooksLikeRecoveryCode(c) {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if LooksLikeRecoveryCode("123456") {
		t.Error("TOTP code looks like a recovery code")
	}
	rest, ok := UseRecoveryCode(hashes, strings.ToLower(strings.ReplaceAll(codes[3], "-", "")))
	if !ok || len(rest) != RecoveryCodeCount-1 {
		t.Fatal("valid code (lower case, no hyphen) refused")
	}
	if _, ok := UseRecoveryCode(rest, codes[3]); ok {
		t.Error("recovery code reused")
	}
	if _, ok := UseRecoveryCode(rest, "AAAAA-AAAAA"); ok {
		t.Error("made-up code accepted")
	}
}

// ---------------------------------------------------------------- proxies

func TestProxyResolve(t *testing.T) {
	trust, err := ParseTrustedProxies([]string{"10.0.0.0/8", "127.0.0.1", "::1", " ", "fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTrustedProxies([]string{"10.0.0.0/33"}); err == nil {
		t.Error("bad CIDR accepted")
	}
	if _, err := ParseTrustedProxies([]string{"proxy.example"}); err == nil {
		t.Error("host name accepted")
	}
	req := func(remote string, hdr ...string) *Client {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Add(hdr[i], hdr[i+1])
		}
		c := trust.Resolve(r)
		return &c
	}
	ip := func(s string) netip.Addr { return netip.MustParseAddr(s) }

	// Untrusted peer: its headers are ignored.
	c := req("203.0.113.9:5000", "X-Forwarded-For", "127.0.0.1", "X-Forwarded-Proto", "https")
	if c.IP != ip("203.0.113.9") || c.ViaTrustedProxy || c.Secure || !c.Proxied || c.Local() {
		t.Errorf("untrusted peer: %+v", *c)
	}
	// Trusted proxy: rightmost untrusted entry is the client; spoofed
	// entries further left are ignored.
	c = req("10.1.2.3:443", "X-Forwarded-For", "127.0.0.1, 198.51.100.7, 10.9.9.9", "X-Forwarded-Proto", "https")
	if c.IP != ip("198.51.100.7") || !c.ViaTrustedProxy || !c.Secure || c.Local() {
		t.Errorf("trusted chain: %+v", *c)
	}
	// Multiple header lines are one list.
	c = req("10.1.2.3:443", "X-Forwarded-For", "192.0.2.1", "X-Forwarded-For", "198.51.100.8")
	if c.IP != ip("198.51.100.8") {
		t.Errorf("multi-line XFF: %v", c.IP)
	}
	// Ports and brackets in entries.
	c = req("[::1]:443", "X-Forwarded-For", "[2001:db8::5]:1234")
	if c.IP != ip("2001:db8::5") {
		t.Errorf("bracketed v6 entry: %v", c.IP)
	}
	// Garbage stops the walk at the last good hop.
	c = req("10.1.2.3:443", "X-Forwarded-For", "198.51.100.7, garbage")
	if c.IP != ip("10.1.2.3") || c.ViaTrustedProxy {
		t.Errorf("garbage entry: %+v", *c)
	}
	// IPv4-mapped peer is treated as IPv4.
	c = req("[::ffff:10.0.0.5]:80", "X-Forwarded-For", "192.0.2.44")
	if c.IP != ip("192.0.2.44") {
		t.Errorf("mapped peer: %v", c.IP)
	}
	// Direct loopback without forwarding headers is local...
	c = req("127.0.0.1:5555")
	if !c.Local() || c.Proxied {
		t.Errorf("plain loopback: %+v", *c)
	}
	// ...but a proxied request from loopback is not, whether the proxy is
	// trusted or not, and whichever forwarding header it uses.
	for _, h := range [][]string{
		{"X-Forwarded-For", "203.0.113.50"}, {"Forwarded", "for=203.0.113.50"}, {"X-Real-IP", "203.0.113.50"},
		{"X-Forwarded-Host", "airview.example.org"},
	} {
		if c := req("127.0.0.1:5555", h...); c.Local() {
			t.Errorf("proxied loopback (%s) is local", h[0])
		}
	}
	untrusting := MustTrustedProxies([]string{"bogus", "192.0.2.0/24"})
	if untrusting.Empty() || !untrusting.Trusted(ip("192.0.2.9")) {
		t.Error("MustTrustedProxies dropped a valid entry")
	}
	var nilTrust *ProxyTrust
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	if c := nilTrust.Resolve(r); c.IP != ip("127.0.0.1") || c.Local() {
		t.Errorf("nil trust: %+v", c)
	}
	r = httptest.NewRequest("GET", "https://x/", nil)
	r.TLS = &tls.ConnectionState{}
	if c := nilTrust.Resolve(r); !c.Secure {
		t.Error("TLS request not secure")
	}
}

func TestSameClient(t *testing.T) {
	ip := netip.MustParseAddr
	if !SameClient(ip("192.0.2.1"), ip("::ffff:192.0.2.1")) || SameClient(ip("192.0.2.1"), ip("192.0.2.2")) {
		t.Error("IPv4")
	}
	if !SameClient(ip("2001:db8:1:2::1"), ip("2001:db8:1:2:ffff::9")) || SameClient(ip("2001:db8:1:2::1"), ip("2001:db8:1:3::1")) {
		t.Error("IPv6 /64")
	}
	if SameClient(ip("192.0.2.1"), ip("2001:db8::1")) || SameClient(netip.Addr{}, ip("192.0.2.1")) {
		t.Error("mixed / invalid")
	}
}

// ---------------------------------------------------------------- CSRF

func TestCSRF(t *testing.T) {
	tok := NewCSRFToken()
	if len(tok) != 43 || tok == NewCSRFToken() {
		t.Fatal("token")
	}
	r := httptest.NewRequest("POST", "/api/x", nil)
	if CheckCSRF(r, tok) {
		t.Error("missing header accepted")
	}
	r.Header.Set(CSRFHeader, tok[:42]+"A")
	if tok[42] != 'A' && CheckCSRF(r, tok) {
		t.Error("wrong token accepted")
	}
	r.Header.Set(CSRFHeader, tok)
	if !CheckCSRF(r, tok) {
		t.Error("right token refused")
	}
	if CheckCSRF(r, "") {
		t.Error("empty expected token matched")
	}
	for m, want := range map[string]bool{"GET": true, "HEAD": true, "OPTIONS": true, "POST": false, "PUT": false, "DELETE": false, "PATCH": false} {
		if SafeMethod(m) != want {
			t.Errorf("SafeMethod(%s)", m)
		}
	}
}

// ---------------------------------------------------------------- rate limits

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time      { return f.t }
func (f *fakeClock) add(d time.Duration) { f.t = f.t.Add(d) }
func newClock() *fakeClock               { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func TestThrottle(t *testing.T) {
	clk := newClock()
	th := NewThrottle(3, 15*time.Minute, time.Minute, 3)
	th.now = clk.now
	for i := 0; i < 2; i++ {
		if d := th.Fail("a"); d != 0 {
			t.Fatalf("free failure %d throttled", i)
		}
	}
	if d := th.Fail("a"); d != 2*time.Second || th.Blocked("a") != 2*time.Second {
		t.Fatalf("third failure: %v", d)
	}
	if d := th.Fail("a"); d != 4*time.Second {
		t.Fatalf("doubling: %v", d)
	}
	for range 10 {
		th.Fail("a")
	}
	if th.Blocked("a") != time.Minute {
		t.Errorf("cap: %v", th.Blocked("a"))
	}
	clk.add(time.Minute)
	if th.Blocked("a") != 0 {
		t.Error("still blocked after the delay")
	}
	clk.add(16 * time.Minute)
	if th.Blocked("a") != 0 || th.Len() != 0 {
		t.Error("not forgotten after the window")
	}
	th.Fail("b")
	th.Success("b")
	if th.Len() != 0 {
		t.Error("success did not clear")
	}
	// Bounded key count: the oldest key is evicted.
	for _, k := range []string{"k1", "k2", "k3", "k4"} {
		th.Fail(k)
		clk.add(time.Second)
	}
	if th.Len() != 3 {
		t.Errorf("keys: %d", th.Len())
	}
}

func TestWindowLimiter(t *testing.T) {
	clk := newClock()
	l := NewWindowLimiter(5, 10*time.Minute, 2)
	l.now = clk.now
	for i := range 5 {
		if ok, _ := l.Allow("ip"); !ok {
			t.Fatalf("event %d refused", i)
		}
		clk.add(time.Minute)
	}
	ok, d := l.Allow("ip")
	if ok || d != 5*time.Minute {
		t.Fatalf("sixth event: %v %v", ok, d)
	}
	clk.add(5 * time.Minute)
	if ok, _ := l.Allow("ip"); !ok {
		t.Error("not allowed after the oldest left the window")
	}
	if !l.Peek("other") {
		t.Error("peek")
	}
	l.Allow("k2")
	if ok, _ := l.Allow("k3"); ok {
		t.Error("new key admitted beyond MaxKeys")
	}
}

// ---------------------------------------------------------------- sessions

func TestMemorySessions(t *testing.T) {
	ctx := context.Background()
	m := NewMemorySessions(4, 2)
	now := time.Now()
	mk := func(acct int64, exp time.Duration) Session {
		_, id := NewSessionToken()
		s := Session{ID: id, AccountID: acct, Scope: model.ScopeOwner, Created: now, LastSeen: now, Expires: now.Add(exp)}
		_ = m.Put(ctx, s)
		return s
	}
	a1 := mk(1, time.Hour)
	mk(1, 2*time.Hour)
	mk(1, 3*time.Hour) // evicts a1 (per-account cap 2)
	if _, err := m.Get(ctx, a1.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Error("per-account cap not enforced")
	}
	mk(2, 4*time.Hour)
	mk(3, 5*time.Hour)
	if n, _ := m.Count(ctx); n != 4 {
		t.Errorf("count %d", n)
	}
	mk(4, 6*time.Hour) // total cap 4: the earliest expiring goes
	if n, _ := m.Count(ctx); n != 4 {
		t.Errorf("total cap: %d", n)
	}
	if l, _ := m.List(ctx, 1); len(l) != 1 {
		t.Errorf("account 1 sessions: %d", len(l))
	}
	if n, _ := m.DeleteAccount(ctx, 1, ""); n != 1 {
		t.Errorf("delete account: %d", n)
	}
	expired := Session{ID: "x", AccountID: 9, Expires: now.Add(-time.Second)}
	_ = m.Put(ctx, expired)
	if n, _ := m.Purge(ctx, now); n != 1 {
		t.Errorf("purge: %d", n)
	}
	_ = m.DeleteAll(ctx)
	if n, _ := m.Count(ctx); n != 0 {
		t.Error("delete all")
	}
	if SessionID("abc") == "abc" || len(SessionID("abc")) != 64 {
		t.Error("session id is not a hash")
	}
	if (Session{ID: "a", Expires: now.Add(time.Hour), LastSeen: now.Add(-2 * time.Hour), Idle: time.Hour}).Valid(now) {
		t.Error("idle session valid")
	}
}

func TestUsernames(t *testing.T) {
	for in, want := range map[string]bool{"Alice": true, "bob.smith_2-x": true, "ab": false, "_lead": false,
		"has space": false, "ünï": false, strings.Repeat("a", 33): false, "  carol  ": true} {
		if _, ok := NormalizeUsername(in); ok != want {
			t.Errorf("%q: %v", in, ok)
		}
	}
	if n, _ := NormalizeUsername(" Alice "); n != "alice" {
		t.Error("not normalised")
	}
}
