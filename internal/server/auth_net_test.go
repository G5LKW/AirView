package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	avauth "airview/internal/auth"
	"airview/internal/model"
)

// Aggregator-mode auth tests (DESIGN §7). The standalone rules are covered
// by TestAuthFlow, which must keep passing unchanged.

// aggRig is a demo rig switched to the aggregator auth rules with a cheap
// password hash.
func aggRig(t *testing.T, proxies ...string) *rig {
	t.Helper()
	r := newRig(t, rigOptions{})
	r.srv.auth.net.testMode = model.ModeAggregator
	r.srv.auth.net.testProxies = append([]string{}, proxies...)
	r.srv.auth.failDelay = 0
	r.srv.SetAccountService(avauth.NewService(avauth.NewMemoryAccounts(), avauth.NewMemorySessions(0, 0),
		avauth.Options{PasswordIterations: 1000}))
	return r
}

// serve sends a request to h (like rig.do).
func serve(h http.Handler, method, target, remote, body string, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.RemoteAddr = remote
	req.Host = "192.168.1.10:8080"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
		} else {
			req.Header.Add(hdr[i], hdr[i+1])
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// authMux is the server handler; the account routes are part of the route
// table (auth_routes.go).
func authMux(r *rig) http.Handler { return r.srv.Handler() }

func decodeInfo(t *testing.T, rec *httptest.ResponseRecorder) sessionInfo {
	t.Helper()
	var info sessionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return info
}

// makeAdmin creates a ready admin account and returns its TOTP generator.
func makeAdmin(t *testing.T, r *rig, name, pw string) avauth.TOTP {
	t.Helper()
	ctx := context.Background()
	svc := r.srv.AccountService()
	setup, err := svc.InitAdmin(ctx, name, pw)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := avauth.ParseTOTPSecret(setup.Secret)
	if _, err := svc.ConfirmAdminTOTP(ctx, name, g.Code(time.Now().Add(-30*time.Second))); err != nil {
		t.Fatal(err)
	}
	return g
}

// A request relayed by a reverse proxy on the same machine is not "from
// this machine": loopback-admin must not apply to it (standalone), and in
// aggregator mode loopback is never admin at all.
func TestProxiedLoopbackIsNotAdmin(t *testing.T) {
	r := newRig(t, rigOptions{})
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{"http":{"port":80,"tileCache":true,"adminPasswordHash":"plain:s3cret"}}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	// Direct loopback stays admin (standalone behaviour unchanged).
	if info := decodeInfo(t, r.do("GET", "/api/session", loopbackAddr, "")); !info.Admin || !info.Loopback {
		t.Errorf("direct loopback: %+v", info)
	}
	for _, h := range [][]string{
		{"X-Forwarded-For", "203.0.113.9"}, {"Forwarded", "for=203.0.113.9;proto=https"},
		{"X-Real-IP", "203.0.113.9"}, {"X-Forwarded-Host", "airview.example.org"},
	} {
		info := decodeInfo(t, r.do("GET", "/api/session", loopbackAddr, "", h...))
		if info.Admin || info.Loopback {
			t.Errorf("standalone, proxied loopback via %s: %+v", h[0], info)
		}
		if rec := r.do("POST", "/api/factory-reset", loopbackAddr, `{"confirm":"NO"}`, h...); rec.Code != http.StatusUnauthorized {
			t.Errorf("standalone, proxied loopback via %s reached an admin route: %d", h[0], rec.Code)
		}
	}

	// Aggregator mode: loopback is not admin, proxied or not, trusted proxy
	// or not, password configured or not.
	a := aggRig(t, "127.0.0.1/32")
	for _, hdr := range [][]string{nil, {"X-Forwarded-For", "127.0.0.1"}, {"X-Forwarded-For", "203.0.113.9"}} {
		info := decodeInfo(t, a.do("GET", "/api/session", loopbackAddr, "", hdr...))
		if info.Admin || info.Loopback || !info.Aggregator || info.Scope != "public" {
			t.Errorf("aggregator loopback %v: %+v", hdr, info)
		}
		if rec := a.do("PUT", "/api/config", loopbackAddr, `{}`, hdr...); rec.Code != http.StatusUnauthorized {
			t.Errorf("aggregator loopback %v PUT config: %d", hdr, rec.Code)
		}
		var st model.Status
		_ = json.Unmarshal(a.do("GET", "/api/status", loopbackAddr, "", hdr...).Body.Bytes(), &st)
		if !st.AdminLocked {
			t.Errorf("aggregator loopback %v: status not locked", hdr)
		}
	}
	// A spoofed loopback address in X-Forwarded-For from the internet is
	// not admin either.
	if info := decodeInfo(t, a.do("GET", "/api/session", "203.0.113.9:4000", "", "X-Forwarded-For", "127.0.0.1")); info.Admin {
		t.Error("spoofed X-Forwarded-For: 127.0.0.1 is admin")
	}
}

func TestTrustedProxyClientIP(t *testing.T) {
	a := aggRig(t, "10.0.0.0/8")
	req := func(remote string, hdr ...string) *http.Request {
		q := httptest.NewRequest("GET", "/", nil)
		q.RemoteAddr = remote
		for i := 0; i+1 < len(hdr); i += 2 {
			q.Header.Add(hdr[i], hdr[i+1])
		}
		return q
	}
	if ip := a.srv.remoteIP(req("10.0.0.2:5000", "X-Forwarded-For", "1.2.3.4, 198.51.100.7")); ip != "198.51.100.7" {
		t.Errorf("trusted proxy: %s", ip)
	}
	if ip := a.srv.remoteIP(req("192.0.2.50:5000", "X-Forwarded-For", "198.51.100.7")); ip != "192.0.2.50" {
		t.Errorf("untrusted peer: %s", ip)
	}
	if !a.srv.client(req("10.0.0.2:5000", "X-Forwarded-Proto", "https")).Secure {
		t.Error("X-Forwarded-Proto from a trusted proxy ignored")
	}
	if a.srv.client(req("192.0.2.50:5000", "X-Forwarded-Proto", "https")).Secure {
		t.Error("X-Forwarded-Proto from an untrusted peer honoured")
	}
	// Standalone never honours X-Forwarded-For.
	s := newRig(t, rigOptions{})
	if ip := s.srv.remoteIP(req("10.0.0.2:5000", "X-Forwarded-For", "198.51.100.7")); ip != "10.0.0.2" {
		t.Errorf("standalone: %s", ip)
	}
	// The per-client login throttle uses the forwarded address: one
	// guesser behind the proxy does not lock out everybody else.
	makeAdmin(t, a, "root", "admin-password-1")
	for range 6 {
		a.do("POST", "/api/login", "10.0.0.2:1", `{"username":"root","password":"wrong-password"}`, "X-Forwarded-For", "198.51.100.66")
	}
	if rec := a.do("POST", "/api/login", "10.0.0.2:1", `{"username":"nobody1","password":"x-password"}`, "X-Forwarded-For", "198.51.100.66"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("guesser not throttled: %d", rec.Code)
	}
	if rec := a.do("POST", "/api/login", "10.0.0.2:1", `{"username":"nobody2","password":"x-password"}`, "X-Forwarded-For", "198.51.100.67"); rec.Code != http.StatusUnauthorized {
		t.Errorf("other client behind the proxy: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAggregatorAdminLoginCSRF(t *testing.T) {
	a := aggRig(t, "10.0.0.1")
	info := decodeInfo(t, a.do("GET", "/api/session", lanAddr, ""))
	if !info.SetupRequired {
		t.Error("setupRequired false without an admin")
	}
	g := makeAdmin(t, a, "root", "admin-password-1")
	if info := decodeInfo(t, a.do("GET", "/api/session", lanAddr, "")); info.SetupRequired {
		t.Error("setupRequired true with an admin")
	}

	// Cross-origin login is refused.
	if rec := a.do("POST", "/api/login", lanAddr, `{"username":"root","password":"admin-password-1"}`, "Origin", "https://evil.example"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin login: %d", rec.Code)
	}
	// The standalone-style body (password only) does not log anybody in.
	if rec := a.do("POST", "/api/login", lanAddr, `{"password":"admin-password-1"}`); rec.Code != http.StatusUnauthorized || cookieOf(rec) != "" {
		t.Errorf("password-only login: %d", rec.Code)
	}
	// Password alone: 401 with codeRequired, no cookie.
	rec := a.do("POST", "/api/login", lanAddr, `{"username":"root","password":"admin-password-1"}`)
	if rec.Code != http.StatusUnauthorized || cookieOf(rec) != "" || !decodeInfo(t, rec).CodeRequired {
		t.Fatalf("password without code: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.do("POST", "/api/login", lanAddr, `{"username":"root","password":"admin-password-1","code":"000000"}`); rec.Code != http.StatusUnauthorized || cookieOf(rec) != "" {
		t.Errorf("wrong code: %d", rec.Code)
	}
	// Password + code through a trusted TLS-terminating proxy.
	rec = a.do("POST", "/api/login", "10.0.0.1:443", `{"username":"root","password":"admin-password-1","code":"`+g.Code(time.Now())+`"}`,
		"X-Forwarded-For", "198.51.100.20", "X-Forwarded-Proto", "https")
	cookie := cookieOf(rec)
	if rec.Code != 200 || cookie == "" {
		t.Fatalf("admin login: %d %s", rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get("Set-Cookie")
	for _, attr := range []string{"HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(raw, attr) {
			t.Errorf("cookie lacks %s: %s", attr, raw)
		}
	}
	login := decodeInfo(t, rec)
	if !login.Admin || login.Scope != "admin" || login.CSRFToken == "" || login.Username != "root" {
		t.Fatalf("login reply: %+v", login)
	}
	// Over plain HTTP the cookie is not Secure (it would be dropped).
	if rec := a.do("GET", "/api/session", lanAddr, "", "Cookie", cookie); decodeInfo(t, rec).CSRFToken != login.CSRFToken {
		t.Error("session endpoint does not return the CSRF token")
	}

	csrf := login.CSRFToken
	// Mutating with the cookie but without / with a wrong CSRF token: 403.
	if rec := a.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "CSRF") {
		t.Errorf("PUT without CSRF token: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, avauth.CSRFHeader, csrf[:len(csrf)-2]+"xx"); rec.Code != http.StatusForbidden {
		t.Errorf("PUT with wrong CSRF token: %d", rec.Code)
	}
	// Right token: allowed, also on a public host name (no rebinding rule
	// in aggregator mode: nothing is granted without a session).
	if rec := a.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, avauth.CSRFHeader, csrf, "Host", "net.example.org"); rec.Code != 200 {
		t.Errorf("PUT with CSRF token: %d %s", rec.Code, rec.Body.String())
	}
	// The token of another session does not work.
	if rec := a.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, avauth.CSRFHeader, avauth.NewCSRFToken()); rec.Code != http.StatusForbidden {
		t.Errorf("foreign token: %d", rec.Code)
	}
	// Cross-origin with cookie and token is still refused.
	if rec := a.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, avauth.CSRFHeader, csrf, "Origin", "https://evil.example"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin with token: %d", rec.Code)
	}
	// GETs need no token.
	if rec := a.do("GET", "/api/system", lanAddr, "", "Cookie", cookie); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"dataDir":"`) || strings.Contains(rec.Body.String(), `"dataDir":""`) {
		t.Errorf("admin GET system: %d", rec.Code)
	}
	// Viewer.
	q := httptest.NewRequest("GET", "/", nil)
	q.Header.Set("Cookie", cookie)
	if v := a.srv.viewer(q); v.Scope != model.ScopeAdmin {
		t.Errorf("viewer %+v", v)
	}
	if v := a.srv.viewer(httptest.NewRequest("GET", "/", nil)); v.Scope != model.ScopePublic {
		t.Errorf("anonymous viewer %+v", v)
	}

	// Logout needs the token too, then the session is gone.
	if rec := a.do("POST", "/api/logout", lanAddr, "", "Cookie", cookie); rec.Code != http.StatusForbidden {
		t.Errorf("logout without token: %d", rec.Code)
	}
	rec = a.do("POST", "/api/logout", lanAddr, "", "Cookie", cookie, avauth.CSRFHeader, csrf)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("logout: %d %v", rec.Code, rec.Header())
	}
	if rec := a.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, avauth.CSRFHeader, csrf); rec.Code != http.StatusUnauthorized {
		t.Errorf("PUT after logout: %d", rec.Code)
	}
	// Factory reset (and dropAll) ends aggregator sessions as well.
	rec = a.do("POST", "/api/login", lanAddr, `{"username":"root","password":"admin-password-1","code":"`+g.Code(time.Now().Add(30*time.Second))+`"}`)
	if rec.Code != 200 {
		t.Fatalf("second login: %d %s", rec.Code, rec.Body.String())
	}
	if a.srv.auth.count() != 1 {
		t.Errorf("session count %d", a.srv.auth.count())
	}
	a.srv.auth.dropAll()
	if a.srv.auth.count() != 0 {
		t.Error("dropAll kept aggregator sessions")
	}
}

func TestAggregatorOwnerScope(t *testing.T) {
	a := aggRig(t)
	h := authMux(a)
	makeAdmin(t, a, "root", "admin-password-1")

	rec := serve(h, "POST", "/api/auth/register", lanAddr, `{"username":"Alice","password":"alice-password"}`)
	cookie := cookieOf(rec)
	if rec.Code != 200 || cookie == "" {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	info := decodeInfo(t, rec)
	if info.Scope != "owner" || info.Admin || info.Username != "alice" || info.CSRFToken == "" {
		t.Fatalf("register reply: %+v", info)
	}
	csrf := info.CSRFToken
	if rec := serve(h, "POST", "/api/auth/register", lanAddr, `{"username":"alice","password":"alice-password"}`); rec.Code != http.StatusConflict {
		t.Errorf("duplicate register: %d", rec.Code)
	}
	// Owners are not admin.
	if rec := serve(h, "PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, avauth.CSRFHeader, csrf); rec.Code != http.StatusUnauthorized {
		t.Errorf("owner PUT config: %d", rec.Code)
	}
	q := httptest.NewRequest("GET", "/", nil)
	q.Header.Set("Cookie", cookie)
	if v := a.srv.viewer(q); v.Scope != model.ScopeOwner || v.OwnerID == 0 {
		t.Errorf("owner viewer %+v", v)
	}
	w := httptest.NewRecorder()
	if _, ok := a.srv.requireScope(w, q, model.ScopeAdmin); ok || w.Code != http.StatusForbidden {
		t.Errorf("requireScope admin for owner: %v %d", ok, w.Code)
	}
	w = httptest.NewRecorder()
	if _, ok := a.srv.requireScope(w, httptest.NewRequest("GET", "/", nil), model.ScopeOwner); ok || w.Code != http.StatusUnauthorized {
		t.Errorf("requireScope owner for public: %v %d", ok, w.Code)
	}

	// TOTP enrolment through the API; the CSRF token is required.
	if rec := serve(h, "POST", "/api/auth/totp/begin", lanAddr, `{"password":"alice-password"}`, "Cookie", cookie); rec.Code != http.StatusForbidden {
		t.Errorf("totp begin without CSRF: %d", rec.Code)
	}
	rec = serve(h, "POST", "/api/auth/totp/begin", lanAddr, `{"password":"alice-password"}`, "Cookie", cookie, avauth.CSRFHeader, csrf)
	var setup avauth.TOTPSetup
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &setup) != nil || setup.Secret == "" {
		t.Fatalf("totp begin: %d %s", rec.Code, rec.Body.String())
	}
	g, _ := avauth.ParseTOTPSecret(setup.Secret)
	rec = serve(h, "POST", "/api/auth/totp/confirm", lanAddr, `{"code":"`+g.Code(time.Now().Add(-30*time.Second))+`"}`, "Cookie", cookie, avauth.CSRFHeader, csrf)
	var conf struct{ RecoveryCodes []string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &conf) != nil || len(conf.RecoveryCodes) != avauth.RecoveryCodeCount {
		t.Fatalf("totp confirm: %d %s", rec.Code, rec.Body.String())
	}
	// The enrolling session survives; a new login needs a code.
	if info := decodeInfo(t, serve(h, "GET", "/api/session", lanAddr, "", "Cookie", cookie)); !info.LoggedIn || !info.TOTP {
		t.Errorf("session after enrolment: %+v", info)
	}
	if rec := serve(h, "POST", "/api/login", lanAddr, `{"username":"alice","password":"alice-password"}`); !decodeInfo(t, rec).CodeRequired {
		t.Errorf("owner with TOTP, no code: %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(h, "POST", "/api/login", lanAddr, `{"username":"alice","password":"alice-password","code":"`+conf.RecoveryCodes[0]+`"}`)
	if rec.Code != 200 || decodeInfo(t, rec).RecoveryLeft == nil || *decodeInfo(t, rec).RecoveryLeft != avauth.RecoveryCodeCount-1 {
		t.Errorf("recovery-code login: %d %s", rec.Code, rec.Body.String())
	}
	// Sessions list and logout-others.
	rec = serve(h, "GET", "/api/auth/sessions", lanAddr, "", "Cookie", cookie)
	if rec.Code != 200 || strings.Count(rec.Body.String(), `"id"`) != 2 || !strings.Contains(rec.Body.String(), `"current":true`) {
		t.Errorf("sessions: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h, "POST", "/api/auth/logout-others", lanAddr, "", "Cookie", cookie, avauth.CSRFHeader, csrf); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ended":1`) {
		t.Errorf("logout-others: %d %s", rec.Code, rec.Body.String())
	}
	// Password change.
	if rec := serve(h, "POST", "/api/auth/password", lanAddr, `{"current":"alice-password","new":"alice-password-2"}`, "Cookie", cookie, avauth.CSRFHeader, csrf); rec.Code != 200 {
		t.Errorf("password change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h, "POST", "/api/auth/password", lanAddr, `{"current":"x","new":"y"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("password change without session: %d", rec.Code)
	}
}

func TestAggregatorBootstrapAPI(t *testing.T) {
	a := aggRig(t)
	h := authMux(a)
	rec := serve(h, "GET", "/api/auth/setup", lanAddr, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"required":true`) {
		t.Fatalf("setup state: %d %s", rec.Code, rec.Body.String())
	}
	tok, err := a.srv.AccountService().MintBootstrapToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// UX-03: the machine itself is told where the token file is; the LAN
	// is not.
	if rec := serve(h, "GET", "/api/auth/setup", "127.0.0.1:5000", ""); !strings.Contains(rec.Body.String(), `"tokenFile":`) ||
		!strings.Contains(rec.Body.String(), "setup-token") {
		t.Errorf("loopback setup state without the token file: %s", rec.Body.String())
	}
	if rec := serve(h, "GET", "/api/auth/setup", lanAddr, ""); strings.Contains(rec.Body.String(), `"tokenFile"`) {
		t.Errorf("LAN setup state names the token file: %s", rec.Body.String())
	}
	if rec := serve(h, "POST", "/api/auth/setup/begin", lanAddr, `{"token":"wrong","username":"root","password":"admin-password-1"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", rec.Code)
	}
	if rec := serve(h, "POST", "/api/auth/setup/begin", lanAddr, `{"token":"`+tok+`","username":"root","password":"admin-password-1"}`, "Origin", "https://evil.example"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin setup: %d", rec.Code)
	}
	rec = serve(h, "POST", "/api/auth/setup/begin", lanAddr, `{"token":"`+tok+`","username":"root","password":"admin-password-1"}`)
	var setup avauth.TOTPSetup
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &setup) != nil {
		t.Fatalf("setup begin: %d %s", rec.Code, rec.Body.String())
	}
	g, _ := avauth.ParseTOTPSecret(setup.Secret)
	rec = serve(h, "POST", "/api/auth/setup/finish", lanAddr, `{"token":"`+tok+`","username":"root","code":"`+g.Code(time.Now().Add(-30*time.Second))+`"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "recoveryCodes") {
		t.Fatalf("setup finish: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h, "GET", "/api/auth/setup", lanAddr, ""); !strings.Contains(rec.Body.String(), `"required":false`) {
		t.Errorf("setup state after: %s", rec.Body.String())
	}
	rec = serve(h, "POST", "/api/login", lanAddr, `{"username":"root","password":"admin-password-1","code":"`+g.Code(time.Now())+`"}`)
	if rec.Code != 200 || !decodeInfo(t, rec).Admin {
		t.Errorf("admin login after setup: %d %s", rec.Code, rec.Body.String())
	}
}

// Standalone: account routes do not exist, the CSRF token is issued and,
// when sent, checked - and everything TestAuthFlow covers is unchanged.
func TestStandaloneCSRFAndAccountRoutes(t *testing.T) {
	r := newRig(t, rigOptions{})
	h := authMux(r)
	for _, rt := range authRoutes {
		if rec := serve(h, rt.Method, rt.Path, loopbackAddr, `{}`); rec.Code != http.StatusNotFound {
			t.Errorf("standalone %s %s: %d", rt.Method, rt.Path, rec.Code)
		}
	}
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{"http":{"port":80,"tileCache":true,"adminPasswordHash":"plain:s3cret"}}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec := r.do("POST", "/api/login", lanAddr, `{"password":"s3cret"}`)
	cookie, info := cookieOf(rec), decodeInfo(t, rec)
	if rec.Code != 200 || info.CSRFToken == "" || info.Aggregator || info.Scope != "" {
		t.Fatalf("standalone login: %d %+v", rec.Code, info)
	}
	if got := decodeInfo(t, r.do("GET", "/api/session", lanAddr, "", "Cookie", cookie)).CSRFToken; got != info.CSRFToken {
		t.Error("session endpoint CSRF token differs")
	}
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie); rec.Code != 200 {
		t.Errorf("standalone PUT without token (unchanged behaviour): %d", rec.Code)
	}
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, avauth.CSRFHeader, "wrong"); rec.Code != http.StatusForbidden {
		t.Errorf("standalone PUT with wrong token: %d", rec.Code)
	}
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, avauth.CSRFHeader, info.CSRFToken); rec.Code != 200 {
		t.Errorf("standalone PUT with token: %d", rec.Code)
	}
	// Logged-out session info has no token.
	if got := decodeInfo(t, r.do("GET", "/api/session", lanAddr, "")); got.CSRFToken != "" || got.LoggedIn {
		t.Errorf("anonymous session info: %+v", got)
	}
}
