package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	for _, k := range []string{"GET /feeder/install.sh", "GET /feeder/manifest.json", "GET /feeder/manifest.json.sig", "GET /feeder/dl/{target}",
		"GET /feeder/beta/manifest.json", "GET /feeder/beta/manifest.json.sig", "GET /feeder/beta/dl/{target}"} {
		specialTests[k] = "TestFeederInstallRoutes"
	}
}

// TestFeederInstallRoutes: the installer is served with only validated
// presets; releases come from aggregator.feederReleaseDir.
func TestFeederInstallRoutes(t *testing.T) {
	pin := "sha256/" + strings.Repeat("A", 43) + "="
	key := strings.Repeat("B", 43) + "="
	src := feederInstallScript("feed.example.org", 30160, pin, key)
	for _, want := range []string{`PRESET_SERVER="feed.example.org:30160"`, `PRESET_PIN="` + pin + `"`,
		`PRESET_URL="https://feed.example.org"`, `PRESET_MANIFEST_KEY="` + key + `"`, `PRESET_HTTP_PIN=""`} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Hostile values never reach the script.
	bad := feederInstallScript(`x";rm -rf /;"`, 30160, `sha256/"$(id)"`, `$(id)`)
	for _, l := range []string{`PRESET_SERVER=""`, `PRESET_PIN=""`, `PRESET_URL=""`, `PRESET_MANIFEST_KEY=""`} {
		if !strings.Contains(bad, l) {
			t.Errorf("hostile preset inserted: %s missing", l)
		}
	}

	f := newNetFixture(t, rigOptions{})
	rec := f.anon("GET", "/feeder/install.sh", "192.0.2.9:1", "")
	must(t, rec, 200, "install.sh")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") || !strings.HasPrefix(rec.Body.String(), "#!") {
		t.Fatalf("install.sh: %q %.40q", ct, rec.Body.String())
	}
	must(t, f.anon("GET", "/feeder/manifest.json", "192.0.2.9:1", ""), 404, "no release dir")

	dir := t.TempDir()
	// the layout `deploy/release publish -out` writes
	for name, body := range map[string]string{"manifest.json": `{"version":"1"}`, "manifest.json.sig": "c2ln\n", "dl/linux-arm64": "\x7fELF",
		"beta/manifest.json": `{"version":"2"}`, "beta/manifest.json.sig": "c2ln\n", "beta/dl/linux-amd64": "\x7fELF"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := f.eng.Config().Get()
	cfg.Aggregator.FeederReleaseDir = dir
	if _, _, err := f.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return f.eng.EffectiveConfig().Aggregator.FeederReleaseDir == dir })
	rec = f.anon("GET", "/feeder/manifest.json", "192.0.2.9:1", "")
	must(t, rec, 200, "manifest")
	if rec.Body.String() != `{"version":"1"}` {
		t.Fatalf("manifest body %q", rec.Body.String())
	}
	must(t, f.anon("GET", "/feeder/manifest.json.sig", "192.0.2.9:1", ""), 200, "sig")
	must(t, f.anon("GET", "/feeder/dl/linux-arm64", "192.0.2.9:1", ""), 200, "dl")
	must(t, f.anon("GET", "/feeder/dl/linux-armv7", "192.0.2.9:1", ""), 404, "missing target file")
	must(t, f.anon("GET", "/feeder/dl/..%2fmanifest.json", "192.0.2.9:1", ""), 404, "traversal")
	rec = f.anon("GET", "/feeder/beta/manifest.json", "192.0.2.9:1", "")
	must(t, rec, 200, "beta manifest")
	if rec.Body.String() != `{"version":"2"}` {
		t.Fatalf("beta manifest body %q", rec.Body.String())
	}
	must(t, f.anon("GET", "/feeder/beta/dl/linux-amd64", "192.0.2.9:1", ""), 200, "beta dl")
	must(t, f.anon("GET", "/feeder/beta/dl/linux-arm64", "192.0.2.9:1", ""), 404, "beta missing")
}

// waitUntil polls cond for up to 5 s.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// crawlBody is what the privacy crawl scans of a response: for the Pi
// installer only the preset lines the server fills in (the rest is the
// static script, whose example addresses such as 127.0.0.1:30005 are no
// station data).
func crawlBody(path string, body []byte) []byte {
	if path != "/feeder/install.sh" {
		return body
	}
	var out []byte
	for _, l := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "PRESET_") {
			out = append(out, l...)
			out = append(out, '\n')
		}
	}
	if len(out) == 0 {
		return body // no preset lines: scan everything
	}
	return out
}
