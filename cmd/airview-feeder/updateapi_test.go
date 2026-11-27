package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/model"
	"airview/internal/selfupdate"
)

// feederRelease publishes an airview-feeder release for the running
// target and serves it.
func feederRelease(t *testing.T, ver string) (url, pub string) {
	t.Helper()
	pk, priv, err := selfupdate.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	art := filepath.Join(dir, "artifact")
	magic := map[string][]byte{"windows": []byte("MZ\x90\x00"), "linux": []byte("\x7fELF")}[strings.SplitN(selfupdate.CurrentTarget(), "-", 2)[0]]
	if magic == nil {
		magic = []byte{0xcf, 0xfa, 0xed, 0xfe}
	}
	if err := os.WriteFile(art, append(magic, make([]byte, 2048)...), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "rel")
	if _, err := selfupdate.Publish(selfupdate.PublishOptions{Product: "airview-feeder", Version: ver, Channel: "stable",
		Artifacts: map[string]string{selfupdate.CurrentTarget(): art}, OutDir: out, Key: priv}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(selfupdate.ReleaseHandler{Dir: out})
	t.Cleanup(srv.Close)
	return srv.URL + "/manifest.json", pk.String()
}

// TestUpdateAPI: the status page's self-update routes: loopback, Host and
// CSRF protection, state, check, and the install refusals with their
// reasons (the in-process install itself is covered by internal/selfupdate
// and the airview e2e; here it would replace the test binary).
func TestUpdateAPI(t *testing.T) {
	murl, pub := feederRelease(t, "99.1.0")
	cfg := model.DefaultFeederConfig()
	cfg.AutoUpdate = true
	cfg.Update.ManifestURL, cfg.Update.Keys, cfg.Update.Channel = murl, []string{pub}, "stable"
	f := &feeder{o: runOptions{DataDir: t.TempDir(), Logf: t.Logf}, lc: &loadedConfig{Eff: cfg}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	a := newUpdateAPI(f, next)
	a.goos = "linux"
	a.writable = func(string) error { return nil }

	do := func(method, path, remote, host string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr, req.Host = remote, host
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		return rec
	}
	const lo, host = "127.0.0.1:5555", "127.0.0.1:8099"
	csrf := map[string]string{"X-AirView-Feeder": "1"}

	if rec := do("GET", "/api/status", lo, host, nil); rec.Code != 299 {
		t.Fatalf("other paths must reach the page: %d", rec.Code)
	}
	if rec := do("GET", "/api/update", "192.168.1.20:4000", host, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("LAN client: %d", rec.Code)
	}
	if rec := do("GET", "/api/update", lo, "evil.example:8099", nil); rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("DNS-rebinding host: %d", rec.Code)
	}
	if rec := do("POST", "/api/update/check", lo, host, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("POST without the CSRF header: %d", rec.Code)
	}
	if rec := do("POST", "/api/update/check", lo, host, map[string]string{"X-AirView-Feeder": "1", "Origin": "http://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", rec.Code)
	}

	rec := do("GET", "/api/update", lo, "localhost:8099", nil)
	var st updateState
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &st) != nil || !st.Configured || !st.AutoUpdate || st.Product != "airview-feeder" || !st.CanInstall {
		t.Fatalf("state: %d %s", rec.Code, rec.Body)
	}
	rec = do("POST", "/api/update/check", lo, host, csrf)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"available":"99.1.0"`) {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}

	// Windows: the scheduled task / tray install; Pi: the root timer.
	a.goos = "windows"
	rec = do("POST", "/api/update/install", lo, host, csrf)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "tray") {
		t.Fatalf("windows install: %d %s", rec.Code, rec.Body)
	}
	a.goos = "linux"
	t.Setenv("AIRVIEW_FEEDER_UPDATER", "external")
	rec = do("POST", "/api/update/install", lo, host, csrf)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "airview-feeder-setup --update") {
		t.Fatalf("external updater: %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/api/update", lo, host, nil); !strings.Contains(rec.Body.String(), `"canInstall":false`) {
		t.Fatalf("state with the external updater: %s", rec.Body)
	}

	// Not configured.
	f.lc.Eff.Update.ManifestURL = ""
	if rec := do("POST", "/api/update/check", lo, host, csrf); rec.Code != http.StatusConflict {
		t.Fatalf("check without a manifest: %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/api/update/nothing", lo, host, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown update path: %d", rec.Code)
	}

	// The "Software updates" page: loopback only, strict CSP, its assets.
	for path, want := range map[string]string{"/update": "Software updates", "/update/page.js": "/api/update/check", "/update/page.css": "--accent"} {
		rec := do("GET", path, lo, host, nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
			t.Fatalf("%s: CSP %q", path, csp)
		}
	}
	if rec := do("GET", "/update", "192.168.1.20:4000", host, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("page from the LAN: %d", rec.Code)
	}
	if rec := do("GET", "/update", lo, "evil.example", nil); rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("page with a rebinding host: %d", rec.Code)
	}
	if rec := do("POST", "/update", lo, host, csrf); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST page: %d", rec.Code)
	}
	if rec := do("GET", "/update/other.js", lo, host, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown page asset: %d", rec.Code)
	}
}
