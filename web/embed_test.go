package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

// The files every build of the UI must ship.
var required = []string{
	"index.html",
	"remote.html",
	"favicon.svg",
	"img/logo.svg",
	"css/tokens.css", "css/base.css", "css/shell.css", "css/components.css", "css/map.css",
	"js/app.js", "js/api.js", "js/live.js", "js/units.js", "js/ui.js", "js/charts.js",
	"js/config.js", "js/geo.js", "js/icons.js", "js/README.md",
	"js/admin.js", "js/alertsound.js", "js/aircraft/admin.js", "js/alerts/sound.js",
	"js/pages/map.js",
	"map/airview-dark.json",
	"vendor/VERSIONS.md",
	"vendor/maplibre/maplibre-gl.mjs", "vendor/maplibre/maplibre-gl-worker.mjs", "vendor/maplibre/maplibre-gl.css",
	"vendor/echarts/echarts.esm.min.mjs",
	"vendor/fonts/inter-latin-wght-normal.woff2", "vendor/fonts/jetbrains-mono-latin-wght-normal.woff2",
}

func TestStaticContainsRequiredFiles(t *testing.T) {
	root := Static()
	for _, name := range required {
		info, err := fs.Stat(root, name)
		if err != nil {
			t.Errorf("missing embedded file %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("embedded file %s is empty", name)
		}
	}
	// FS itself keeps the static/ prefix.
	if _, err := fs.Stat(FS, "static/index.html"); err != nil {
		t.Errorf("FS should expose static/index.html: %v", err)
	}
}

func TestVendorDirectoryStaysSmall(t *testing.T) {
	var total int64
	err := fs.WalkDir(Static(), "vendor", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if const5MB := int64(5 << 20); total > const5MB {
		t.Errorf("vendor directory is %d bytes, budget is %d", total, const5MB)
	}
}

func TestMapStyleIsValidAndAttributed(t *testing.T) {
	data, err := fs.ReadFile(Static(), "map/airview-dark.json")
	if err != nil {
		t.Fatal(err)
	}
	var style struct {
		Version int `json:"version"`
		Sources map[string]struct {
			Type        string `json:"type"`
			URL         string `json:"url"`
			Attribution string `json:"attribution"`
		} `json:"sources"`
		Glyphs string            `json:"glyphs"`
		Layers []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(data, &style); err != nil {
		t.Fatalf("style is not valid JSON: %v", err)
	}
	if style.Version != 8 || len(style.Layers) < 20 {
		t.Errorf("unexpected style: version %d, %d layers", style.Version, len(style.Layers))
	}
	src, ok := style.Sources["openmaptiles"]
	if !ok || src.Type != "vector" || !strings.HasPrefix(src.URL, "https://tiles.openfreemap.org/") {
		t.Errorf("openmaptiles source should point at OpenFreeMap, got %+v", src)
	}
	for _, want := range []string{"OpenFreeMap", "OpenMapTiles", "OpenStreetMap"} {
		if !strings.Contains(src.Attribution, want) {
			t.Errorf("style attribution must mention %s, got %q", want, src.Attribution)
		}
	}
	if !strings.HasPrefix(style.Glyphs, "https://tiles.openfreemap.org/fonts/") {
		t.Errorf("unexpected glyphs URL %q", style.Glyphs)
	}
}

func TestContentType(t *testing.T) {
	cases := map[string]string{
		"js/app.js":                              "text/javascript; charset=utf-8",
		"vendor/maplibre/maplibre-gl-worker.mjs": "text/javascript; charset=utf-8",
		"VENDOR/X.MJS":                           "text/javascript; charset=utf-8",
		"css/base.css":                           "text/css; charset=utf-8",
		"index.html":                             "text/html; charset=utf-8",
		"map/airview-dark.json":                  "application/json; charset=utf-8",
		"favicon.svg":                            "image/svg+xml",
		"vendor/fonts/a.woff2":                   "font/woff2",
		"vendor/echarts/LICENSE":                 "text/plain; charset=utf-8",
		"weird.bin":                              "application/octet-stream",
	}
	for name, want := range cases {
		if got := ContentType(name); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func get(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestHandlerServesIndexAndModules(t *testing.T) {
	h := Handler()

	res := get(t, h, http.MethodGet, "/", nil)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if !bytes.Contains(body, []byte(`src="js/app.js"`)) {
		t.Error("index.html should load js/app.js")
	}

	for _, p := range []string{"/js/app.js", "/vendor/maplibre/maplibre-gl-worker.mjs", "/js/pages/map.js"} {
		res := get(t, h, http.MethodGet, p, nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", p, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("GET %s Content-Type = %q, want a JavaScript type (module scripts require it)", p, ct)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s should send nosniff", p)
		}
	}
}

func TestHandlerNotFoundAndMethods(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/js/pages/stats-not-installed.js", "/js", "/js/", "/../embed.go", "/static/index.html"} {
		if res := get(t, h, http.MethodGet, p, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, res.StatusCode)
		}
	}
	if res := get(t, h, http.MethodPost, "/index.html", nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", res.StatusCode)
	}
	res := get(t, h, http.MethodHead, "/js/app.js", nil)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || len(body) != 0 || res.Header.Get("Content-Length") == "" {
		t.Errorf("HEAD = %d, body %d bytes, Content-Length %q", res.StatusCode, len(body), res.Header.Get("Content-Length"))
	}
}

func TestHandlerETagAndGzip(t *testing.T) {
	h := Handler()
	plain := get(t, h, http.MethodGet, "/js/ui.js", nil)
	want, _ := io.ReadAll(plain.Body)
	etag := plain.Header.Get("ETag")
	if etag == "" || plain.Header.Get("Content-Encoding") != "" {
		t.Fatalf("plain response: etag %q, encoding %q", etag, plain.Header.Get("Content-Encoding"))
	}

	if res := get(t, h, http.MethodGet, "/js/ui.js", map[string]string{"If-None-Match": etag}); res.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match = %d, want 304", res.StatusCode)
	}
	if res := get(t, h, http.MethodGet, "/js/ui.js", map[string]string{"If-None-Match": `W/` + etag + `, "other"`}); res.StatusCode != http.StatusNotModified {
		t.Errorf("weak If-None-Match list = %d, want 304", res.StatusCode)
	}

	gz := get(t, h, http.MethodGet, "/js/ui.js", map[string]string{"Accept-Encoding": "br, gzip;q=0.8"})
	if gz.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip, got %q", gz.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("gzip body differs from the plain body")
	}

	if res := get(t, h, http.MethodGet, "/js/ui.js", map[string]string{"Accept-Encoding": "gzip;q=0"}); res.Header.Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 must not be compressed")
	}
	// Already-compressed formats are served as they are.
	if res := get(t, h, http.MethodGet, "/vendor/fonts/inter-latin-wght-normal.woff2", map[string]string{"Accept-Encoding": "gzip"}); res.Header.Get("Content-Encoding") != "" {
		t.Error("woff2 must not be gzip-compressed")
	}
}

// Every relative static import (`from './x.js'`, `import './x.js'`) and literal dynamic import of the
// UI modules must resolve to an embedded file: a moved helper (js/admin.js, js/alertsound.js) keeps its
// old path as a re-export shim, and a typo would otherwise only show up as a blank page in the browser.
func TestJSRelativeImportsResolve(t *testing.T) {
	root := Static()
	re := regexp.MustCompile(`(?:\bfrom\s*|\bimport\s*\(?\s*)['"](\.{1,2}/[^'"]+)['"]`)
	n := 0
	err := fs.WalkDir(root, "js", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(name, ".js") {
			return err
		}
		b, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if l := strings.TrimSpace(line); strings.HasPrefix(l, "//") || strings.HasPrefix(l, "*") {
				continue // usage examples in comments
			}
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				target := path.Join(path.Dir(name), m[1])
				n++
				if _, err := fs.Stat(root, target); err != nil {
					t.Errorf("%s imports %s, which is not embedded (%s)", name, m[1], target)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 50 {
		t.Errorf("only %d relative imports found; the pattern is probably broken", n)
	}
}
