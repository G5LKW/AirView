package server

import (
	"net/http"
	"testing"
)

// SEC-01: in aggregator mode, credentials are refused from remote clients
// over plain HTTP; HTTPS via a trusted proxy and the machine itself work.
func TestInsecureLoginRefused(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.srv.auth.net.testInsecureOK = false
	h := f.srv.Handler()
	body := `{"username":"alice","password":"alice-password"}`

	rec := serve(h, "POST", "/api/login", "198.51.100.20:4000", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote plain-HTTP login: %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(h, "POST", "/api/auth/register", "198.51.100.20:4000", `{"username":"eve","password":"eve-password-1"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote plain-HTTP register: %d", rec.Code)
	}
	rec = serve(h, "POST", "/api/login", "127.0.0.1:4000", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback login: %d %s", rec.Code, rec.Body.String())
	}
	// Behind a trusted proxy that terminated TLS.
	f.srv.auth.net.testProxies = []string{"127.0.0.1/32"}
	rec = serve(h, "POST", "/api/login", "127.0.0.1:4000", body, "X-Forwarded-For", "198.51.100.20", "X-Forwarded-Proto", "https")
	if rec.Code != http.StatusOK {
		t.Fatalf("proxied HTTPS login: %d %s", rec.Code, rec.Body.String())
	}
	if c := rec.Result().Cookies(); len(c) == 0 || !c[0].Secure {
		t.Errorf("session cookie over HTTPS is not Secure: %+v", c)
	}
	rec = serve(h, "POST", "/api/login", "127.0.0.1:4000", body, "X-Forwarded-For", "198.51.100.20", "X-Forwarded-Proto", "http")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("proxied plain-HTTP login: %d", rec.Code)
	}
	// The public map stays available over plain HTTP.
	rec = serve(h, "GET", "/api/public/aircraft", "198.51.100.20:4000", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public map: %d", rec.Code)
	}
}
