package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"airview/internal/model"
)

// fix:surface regression tests (SEC-R2, SEC-R3, SEC-R4, SEC-R7, SEC-R8).

// SEC-R2: without an admin password only private-network clients are
// admin; an internet client is not, unless http.allowOpenAdmin is set.
func TestStandaloneOpenAdminPrivateOnly(t *testing.T) {
	r := newRig(t, rigOptions{})
	const inet = "203.0.113.9:4000"
	var st model.Status
	_ = json.Unmarshal(r.do("GET", "/api/status", lanAddr, "").Body.Bytes(), &st)
	if st.AdminLocked {
		t.Error("LAN client locked without a password")
	}
	_ = json.Unmarshal(r.do("GET", "/api/status", inet, "").Body.Bytes(), &st)
	if !st.AdminLocked {
		t.Error("internet client is admin without a password")
	}
	if rec := r.do("POST", "/api/factory-reset", inet, `{}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("internet factory reset: %d", rec.Code)
	}
	if rec := r.do("PUT", "/api/config", inet, `{"http":{"allowOpenAdmin":true}}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("internet PUT config: %d", rec.Code)
	}
	// A private peer relayed by a proxy is not trusted either.
	if r.srv.isAdmin(reqFrom("GET", "/api/status", "192.168.1.2:4000", "X-Forwarded-For", "203.0.113.9")) {
		t.Error("proxied request is admin")
	}
	for _, a := range []string{"10.1.2.3:1", "172.16.0.9:1", "[fd00::1]:1", "[fe80::1]:1", "127.0.0.1:1"} {
		if !r.srv.isAdmin(reqFrom("GET", "/api/status", a)) {
			t.Errorf("%s: not admin without a password", a)
		}
	}
	for _, a := range []string{"100.64.0.1:1", "[2001:db8::1]:1", "8.8.8.8:1"} {
		if r.srv.isAdmin(reqFrom("GET", "/api/status", a)) {
			t.Errorf("%s: admin without a password", a)
		}
	}
	// The explicit opt-in restores the old behaviour.
	cfg := r.eng.Config().Get()
	cfg.HTTP.AllowOpenAdmin = true
	if _, _, err := r.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if !r.srv.isAdmin(reqFrom("GET", "/api/status", inet)) {
		t.Error("allowOpenAdmin ignored")
	}
}

// SEC-R3: admin-scope routes carry no Access-Control-Allow-Origin, and a
// cross-site request from loopback is not admin.
func TestCORSOnlyOnViewerRoutes(t *testing.T) {
	r := newRig(t, rigOptions{})
	const evil = "https://evil.example"
	rec := r.do("GET", "/api/status", loopbackAddr, "", "Origin", evil)
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("status lost its CORS header")
	}
	for _, rt := range Routes() {
		if rt.Method != "GET" || rt.Access == AccessPublic || rt.Access == AccessViewer {
			continue
		}
		if !corsReadable(&rt) {
			continue
		}
		t.Errorf("%s readable cross-origin", rt.Path)
	}
	for _, p := range []string{"/api/alerts/rules", "/api/history/flights"} {
		rec := r.do("GET", p, loopbackAddr, "", "Origin", evil)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: ACAO on an admin-scope route", p)
		}
	}
	// /api/config read cross-site from loopback is the public form.
	req := reqFrom("GET", "/api/config", loopbackAddr, "Origin", evil)
	if r.srv.isAdmin(req) {
		t.Error("cross-site request from loopback is admin")
	}
	req = reqFrom("GET", "/api/config", loopbackAddr, "Sec-Fetch-Site", "cross-site")
	if r.srv.isAdmin(req) {
		t.Error("Sec-Fetch-Site cross-site from loopback is admin")
	}
	if !r.srv.isAdmin(reqFrom("GET", "/api/config", loopbackAddr, "Sec-Fetch-Site", "same-origin")) {
		t.Error("same-origin loopback request is not admin")
	}
}

// SEC-R4: the self-update trust anchors cannot be changed through the web
// API.
func TestUpdateTrustAnchorsLocked(t *testing.T) {
	r := newRig(t, rigOptions{})
	before := r.eng.Config().Get().Update
	rec := r.do("PUT", "/api/config", loopbackAddr, `{"update":{"manifestUrl":"https://attacker.example/m.json","keys":["`+strings.Repeat("A", 43)+`="]}}`)
	if rec.Code == http.StatusOK {
		after := r.eng.Config().Get().Update
		if after.ManifestURL != before.ManifestURL || strings.Join(after.Keys, ",") != strings.Join(before.Keys, ",") {
			t.Fatalf("trust anchors changed via the web: %+v", after)
		}
	} else if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
		t.Fatalf("PUT update: %d %s", rec.Code, rec.Body.String())
	}
}

// SEC-R7: the standalone throttle keys an IPv6 /64 as one client, evicts
// the oldest entry instead of clearing the table, and has a global budget.
func TestStandaloneThrottle(t *testing.T) {
	a := newAuth()
	k1 := "2001:db8:1:2::"
	for i := 0; i < loginFree+1; i++ {
		a.fail(k1)
	}
	if a.blocked(k1) <= 0 {
		t.Fatal("not throttled")
	}
	// Flood with fresh keys: k1 stays throttled.
	for i := 0; i < maxThrottleIPs+10; i++ {
		a.mu.Lock()
		a.global = throttle{} // keep the global budget out of this part
		a.mu.Unlock()
		a.fail("flood-" + strconv.Itoa(i))
	}
	if len(a.throttle) > maxThrottleIPs {
		t.Errorf("table grew to %d", len(a.throttle))
	}
	if _, ok := a.throttle[k1]; ok && a.blocked(k1) <= 0 {
		t.Error("k1 throttle reset by the flood")
	}
	// Global budget: many addresses with one guess each.
	b := newAuth()
	for i := 0; i < loginGlobalFree+1; i++ {
		b.fail("g-" + strconv.Itoa(i))
	}
	if b.blocked("fresh") <= 0 {
		t.Error("no global throttle after many failures from many addresses")
	}
}

// SEC-R8: a plain-HTTP credential request relayed by an untrusted local
// proxy is refused.
func TestInsecureLoginUntrustedLocalProxy(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.srv.auth.net.testInsecureOK = false
	body := `{"username":"alice","password":"alice-password"}`
	rec := serve(f.srv.Handler(), "POST", "/api/login", "127.0.0.1:4000", body, "X-Forwarded-For", "198.51.100.20")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("login via an untrusted local proxy over HTTP: %d %s", rec.Code, rec.Body.String())
	}
}

// reqFrom builds a request from remote with extra header pairs.
func reqFrom(method, target, remote string, hdr ...string) *http.Request {
	req, _ := http.NewRequest(method, "http://192.168.1.10:8080"+target, nil)
	req.RemoteAddr = remote
	req.Host = "192.168.1.10:8080"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	return req
}
