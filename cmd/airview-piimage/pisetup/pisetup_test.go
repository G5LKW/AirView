package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/config"
)

func TestValidate(t *testing.T) {
	good := values{Server: "feed.example.net:30160", Name: "my-pi", Lat: "51.47", Lon: "-0.4543", Alt: "35 m",
		AltRef: "", AGL: "25ft", Privacy: "", Input: "", Timestamps: true, Password: "secret123", Password2: "secret123"}
	v := good
	if err := v.validate(true); err != nil {
		t.Fatal(err)
	}
	if v.Alt != "35m" || v.AGL != "25ft" || v.AltRef != "msl" || v.Privacy != "hidden" || v.Input != "auto" {
		t.Fatalf("normalised %+v", v)
	}
	cases := map[string]func(*values){
		"server":    func(v *values) { v.Server = "" },
		"server2":   func(v *values) { v.Server = "bad host!" },
		"server3":   func(v *values) { v.Server = "x.net:70000" },
		"pin":       func(v *values) { v.Pin = "sha256/short" },
		"name":      func(v *values) { v.Name = "has space" },
		"lat":       func(v *values) { v.Lat = "91" },
		"lon":       func(v *values) { v.Lon = "abc" },
		"alt":       func(v *values) { v.Alt = "35" }, // unit is mandatory
		"altRef":    func(v *values) { v.AltRef = "agl" },
		"agl":       func(v *values) { v.AGL = "-3m" },
		"privacy":   func(v *values) { v.Privacy = "exact" },
		"password":  func(v *values) { v.Password, v.Password2 = "short", "short" },
		"password2": func(v *values) { v.Password2 = "different" },
		"input":     func(v *values) { v.Input = "a b" },
		"token":     func(v *values) { v.Token = "bad token" },
	}
	for name, mut := range cases {
		v := good
		mut(&v)
		err := v.validate(true)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if f := fieldOf(err); !strings.HasPrefix(name, f) || f == "" {
			t.Errorf("%s: field %q", name, f)
		}
	}
	v = good
	v.NoLocation, v.Lat, v.Lon, v.Alt = true, "", "", ""
	if err := v.validate(true); err != nil {
		t.Fatalf("no location: %v", err)
	}
	for _, s := range []string{"10.0.0.5", "10.0.0.5:30160", "[2001:db8::1]:30160", "2001:db8::1", "feed.example.net"} {
		if err := checkServer(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

func TestInstallerArgs(t *testing.T) {
	v := values{Server: "feed.example.net:30160", Pin: "sha256/" + strings.Repeat("A", 43) + "=", Token: "TOK-1", Name: "pi1",
		Lat: "51.5", Lon: "-0.1", Alt: "35m", AltRef: "msl", AGL: "10m", Input: "auto", Privacy: "region", Timestamps: false, AutoUpdate: false}
	got := strings.Join(v.installerArgs("/opt/airview-image/bin"), " ")
	want := "--binary /opt/airview-image/bin --server feed.example.net:30160 --pin sha256/" + strings.Repeat("A", 43) + "= --token TOK-1 --name pi1 " +
		"--lat 51.5 --lon -0.1 --alt 35m --alt-ref msl --agl 10m --input auto --privacy region --no-timestamps --no-auto-update --non-interactive --no-start"
	if got != want {
		t.Fatalf("args\n got %s\nwant %s", got, want)
	}
	v.NoLocation, v.Timestamps, v.AutoUpdate = true, true, true
	got = strings.Join(v.installerArgs("b"), " ")
	if !strings.Contains(got, "--no-location") || strings.Contains(got, "--lat") || strings.Contains(got, "--no-timestamps") {
		t.Fatalf("args %s", got)
	}
	if r := strings.Join(redact(v.installerArgs("b")), " "); strings.Contains(r, "TOK-1") || !strings.Contains(r, "--token (redacted)") {
		t.Fatalf("redact: %s", r)
	}
	if r := redact([]string{"config", "set", "ui.passwordHash=pbkdf2-x"}); r[2] != "ui.passwordHash=(redacted)" {
		t.Fatalf("redact hash: %v", r)
	}
}

func TestParseSetupFile(t *testing.T) {
	in := "\ufeff# comment\r\nSERVER = feed.example.net\r\nTOKEN=\"abcd\"\r\nNAME=station-1\r\nLAT=51.5\r\nLON=-0.1\r\nALT=40m\r\nTIMESTAMPS=no\r\nPRIVACY=cell01\r\nUI_PASSWORD=longenough\r\n"
	v, ok, err := parseSetupFile([]byte(in), defaults("airview-feeder"))
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if v.Server != "feed.example.net" || v.Token != "abcd" || v.Name != "station-1" || v.Timestamps || v.Privacy != "cell01" || v.NoLocation || v.Password != "longenough" {
		t.Fatalf("%+v", v)
	}
	if err := v.validate(false); err != nil {
		t.Fatal(err)
	}
	// Without a position: the network locates the station.
	v, _, _ = parseSetupFile([]byte("SERVER=x.net\n"), defaults("h"))
	if !v.NoLocation || !v.AutoUpdate || !v.Timestamps || v.Name != "h" {
		t.Fatalf("%+v", v)
	}
	for _, bad := range []string{"NOPE=1\n", "SERVER\n", "TIMESTAMPS=maybe\n"} {
		if _, _, err := parseSetupFile([]byte(bad), defaults("h")); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// The shipped template parses, uses only known keys (uncommented) and requests no setup.
	tpl, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "feeder", "pi-image", "feeder-setup.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := parseSetupFile(tpl, defaults("h")); err != nil || ok {
		t.Fatalf("template: ok=%v err=%v", ok, err)
	}
	var uncommented []string
	for _, l := range strings.Split(string(tpl), "\n") {
		if m := regexp.MustCompile(`^# ([A-Z_]+)=`).FindStringSubmatch(l); m != nil {
			uncommented = append(uncommented, strings.TrimPrefix(l, "# "))
		}
	}
	if len(uncommented) < 12 {
		t.Fatalf("template keys: %v", uncommented)
	}
	if _, _, err := parseSetupFile([]byte(strings.Join(uncommented, "\n")), defaults("h")); err != nil {
		t.Fatalf("template keys do not parse: %v", err)
	}
}

func TestClientAndHostChecks(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:5": true, "192.168.1.20:5": true, "10.1.2.3:5": true, "172.20.0.1:5": true, "100.100.1.1:5": true,
		"[fe80::1]:5": true, "[fd00::1]:5": true, "[::1]:5": true, "8.8.8.8:5": false, "[2001:db8::1]:5": false, "[::ffff:192.168.1.2]:5": true,
	} {
		if clientAllowed(addr) != want {
			t.Errorf("client %s: want %v", addr, want)
		}
	}
	for host, want := range map[string]bool{
		"192.168.1.5:8099": true, "[fe80::1]:8099": true, "localhost:8099": true, "airview-feeder:8099": true,
		"airview-feeder.local:8099": true, "AIRVIEW-FEEDER.LOCAL.": true, "airview-feeder.home.arpa": true,
		"airview-feeder.evil.example:8099": false, "evil.example": false, "": true,
	} {
		if hostAllowed(host, "airview-feeder") != want {
			t.Errorf("host %q: want %v", host, want)
		}
	}
}

// fakeRunner records commands.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	fail  string
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.fail != "" && (name == f.fail || (len(args) > 0 && strings.HasSuffix(args[0], f.fail))) {
		return "installer said no", context.DeadlineExceeded
	}
	return "ok", nil
}

func (f *fakeRunner) joined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s []string
	for _, c := range f.calls {
		s = append(s, strings.Join(c, " "))
	}
	return strings.Join(s, "\n")
}

func testApp(t *testing.T) (*app, *fakeRunner) {
	dir := t.TempDir()
	a := newApp(options{
		listen: "127.0.0.1:0", setupFile: filepath.Join(dir, "feeder-setup.txt"), installer: "/opt/airview-image/install-feeder.sh",
		binary: "/opt/airview-image/airview-feeder-linux-arm64", feeder: "/opt/airview-feeder/airview-feeder",
		config: filepath.Join(dir, "feeder.json"), unit: "airview-pisetup.service", service: "airview-feeder.service",
		uiAddr: "0.0.0.0:8099", runTimeout: time.Minute,
	})
	fr := &fakeRunner{}
	a.run = fr.run
	a.host = func() (string, error) { return "airview-feeder", nil }
	a.addrs = func() []string { return []string{"192.168.1.50"} }
	a.initial = defaults("airview-feeder")
	a.initial.ManifestKey = testManifestKey
	return a, fr
}

// testManifestKey is a syntactically valid Ed25519 public key (base64).
const testManifestKey = "g5f6wBvtvtt5GfYhg1aQ6eg+cvLmHcfLD61AK20Q/SU="

func form(token string, mut func(url.Values)) url.Values {
	f := url.Values{
		"csrf": {token}, "server": {"feed.example.net"}, "token": {"T0K-1234"}, "name": {"pi-one"}, "location": {"manual"},
		"lat": {"51.5"}, "lon": {"-0.1"}, "alt": {"30m"}, "altRef": {"msl"}, "agl": {"8m"}, "input": {"auto"},
		"privacy": {"hidden"}, "timestamps": {"yes"}, "autoUpdate": {"yes"}, "password": {"correct horse"}, "password2": {"correct horse"},
	}
	if mut != nil {
		mut(f)
	}
	return f
}

func post(s *server, host, remote string, f url.Values, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://"+host+"/setup", strings.NewReader(f.Encode()))
	r.Host = host
	r.RemoteAddr = remote
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestSetupPageFlow(t *testing.T) {
	a, fr := testApp(t)
	s := newServer(a)
	// GET renders the form with the consent texts and the token.
	r := httptest.NewRequest("GET", "http://airview-feeder.local:8099/", nil)
	r.RemoteAddr = "192.168.1.9:4000"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, s.token) || !strings.Contains(body, consentTimestamps[:40]) ||
		!strings.Contains(body, "Show my station on the public map?") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("GET %d:\n%s", w.Code, body)
	}
	// Refusals.
	if w := post(s, "airview-feeder.local:8099", "8.8.8.8:1", form(s.token, nil), ""); w.Code != 403 {
		t.Errorf("public client: %d", w.Code)
	}
	if w := post(s, "rebind.evil.example:8099", "192.168.1.9:1", form(s.token, nil), ""); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("rebinding host: %d", w.Code)
	}
	if w := post(s, "192.168.1.50:8099", "192.168.1.9:1", form("wrong", nil), ""); w.Code != 403 {
		t.Errorf("bad csrf: %d", w.Code)
	}
	if w := post(s, "192.168.1.50:8099", "192.168.1.9:1", form(s.token, nil), "http://evil.example"); w.Code != 403 {
		t.Errorf("cross origin: %d", w.Code)
	}
	if fr.joined() != "" {
		t.Fatalf("commands ran on refused requests: %s", fr.joined())
	}
	// Validation error: re-rendered with the field marked, password not echoed.
	w = post(s, "192.168.1.50:8099", "192.168.1.9:1", form(s.token, func(f url.Values) { f.Set("alt", "30") }), "http://192.168.1.50:8099")
	if w.Code != 422 || !strings.Contains(w.Body.String(), "unit") || strings.Contains(w.Body.String(), "correct horse") {
		t.Fatalf("invalid alt: %d\n%s", w.Code, w.Body.String())
	}
	// Installer failure: output shown, nothing finished.
	fr.fail = "install-feeder.sh"
	w = post(s, "192.168.1.50:8099", "192.168.1.9:1", form(s.token, nil), "")
	if w.Code != 500 || !strings.Contains(w.Body.String(), "installer said no") {
		t.Fatalf("installer failure: %d\n%s", w.Code, w.Body.String())
	}
	fr.fail = ""
	fr.calls = nil
	// Success.
	w = post(s, "airview-feeder.local:8099", "192.168.1.9:1", form(s.token, nil), "http://airview-feeder.local:8099")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "http://192.168.1.50:8099/") {
		t.Fatalf("success page %d:\n%s", w.Code, w.Body.String())
	}
	calls := fr.joined()
	if !strings.Contains(calls, "bash /opt/airview-image/install-feeder.sh --binary /opt/airview-image/airview-feeder-linux-arm64 --server feed.example.net --token T0K-1234 --name pi-one --lat 51.5 --lon -0.1 --alt 30m --alt-ref msl --agl 8m --input auto --privacy hidden --manifest-key "+testManifestKey+" --non-interactive --no-start") {
		t.Fatalf("installer call:\n%s", calls)
	}
	m := regexp.MustCompile(`config set -config \S+ ui.addr=0.0.0.0:8099 ui.passwordHash=(\S+)`).FindStringSubmatch(calls)
	if m == nil || !config.IsPasswordHash(m[1]) || strings.Contains(calls, "correct horse") {
		t.Fatalf("config set call:\n%s", calls)
	}
	select {
	case v := <-s.finished:
		if v.Name != "pi-one" {
			t.Fatalf("finished %+v", v)
		}
	default:
		t.Fatal("setup did not finish")
	}
}

func TestMainUnattendedAndConfigured(t *testing.T) {
	a, fr := testApp(t)
	os.WriteFile(a.o.setupFile, []byte("SERVER=feed.example.net:30160\nNAME=pi-two\nUI_PASSWORD=longenough\n"), 0o600)
	if err := a.main(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := fr.joined()
	for _, want := range []string{"--server feed.example.net:30160", "--no-location", "ui.addr=0.0.0.0:8099",
		"systemctl disable airview-pisetup.service", "systemctl start --no-block airview-feeder.service"} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls lack %q:\n%s", want, calls)
		}
	}
	if _, err := os.Stat(a.o.setupFile); !os.IsNotExist(err) {
		t.Error("setup answers not deleted")
	}
	// Configured already: only disable.
	a, fr = testApp(t)
	os.WriteFile(a.o.config, []byte("{}"), 0o640)
	if err := a.main(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := fr.joined(); c != "systemctl disable airview-pisetup.service" {
		t.Fatalf("configured: %s", c)
	}
}

func TestServeHandsOverPort(t *testing.T) {
	a, _ := testApp(t)
	s := newServer(a)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	done := make(chan error, 1)
	go func() { _, err := s.serve(context.Background(), ln); done <- err }()
	resp, err := http.PostForm("http://"+addr+"/setup", form(s.token, nil))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after the setup")
	}
	// The port is free for the feeder.
	l2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port still taken: %v", err)
	}
	l2.Close()
}

// TestConsentTextsMatchDoc: C1 and C2 word for word as in CONSENT.md.
func TestConsentTextsMatchDoc(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "aggregator", "CONSENT.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(b), "\r\n", "\n")
	section := func(id string) []string {
		var out []string
		in := false
		for _, l := range strings.Split(doc, "\n") {
			if strings.HasPrefix(l, "## ") || strings.HasPrefix(l, "---") {
				in = strings.HasPrefix(l, "## "+id)
				continue
			}
			if in && strings.HasPrefix(l, ">") {
				l = strings.TrimSpace(strings.TrimPrefix(l, ">"))
				l = strings.TrimPrefix(l, "- ")
				l = strings.Trim(strings.ReplaceAll(l, "**", ""), `"`)
				if l != "" {
					out = append(out, l)
				}
			}
		}
		return out
	}
	if c1 := section("C1"); len(c1) != 1 || c1[0] != consentTimestamps {
		t.Errorf("C1 differs:\n%q\n%q", c1, consentTimestamps)
	}
	if c2 := section("C2"); strings.Join(c2, "|") != strings.Join(consentPinLines, "|") {
		t.Errorf("C2 differs:\n%q\n%q", c2, consentPinLines)
	}
}

func statusHashFor(t *testing.T, pw string) string {
	t.Helper()
	h := config.HashPassword(pw)
	if !config.IsPasswordHash(h) {
		t.Fatalf("hash %q", h)
	}
	return h
}

// DLV-03: the release key and URL baked into the image (update.conf) reach
// the feeder installer; without a key automatic updates are switched off
// and the page says so instead of offering a checkbox that does nothing.
func TestUpdateConfAndAutoUpdate(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "update.conf")
	if err := os.WriteFile(conf, []byte("# baked in by airview-piimage\r\nMANIFEST_KEY="+testManifestKey+"\r\nUPDATE_URL=https://releases.example.net/airview/\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := testApp(t)
	a.o.updateConf = conf
	base := a.baseValues()
	if base.ManifestKey != testManifestKey || base.UpdateURL != "https://releases.example.net/airview/" || !base.AutoUpdate {
		t.Fatalf("update.conf not applied: %+v", base)
	}
	v := base
	v.Server, v.NoLocation = "feed.example.net", true
	if err := v.validate(false); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(v.installerArgs("/bin/f"), " ")
	if !strings.Contains(args, "--url https://releases.example.net/airview --manifest-key "+testManifestKey) || strings.Contains(args, "--no-auto-update") {
		t.Fatalf("installer args: %s", args)
	}
	// The user may still opt out: key and URL are kept for manual updates.
	v.AutoUpdate = false
	if args := strings.Join(v.installerArgs("/bin/f"), " "); !strings.Contains(args, "--manifest-key") || !strings.Contains(args, "--no-auto-update") {
		t.Fatalf("opt-out args: %s", args)
	}

	// No update.conf (or an empty key): no automatic updates.
	a.o.updateConf = filepath.Join(dir, "missing.conf")
	base = a.baseValues()
	if base.AutoUpdate || base.CanAutoUpdate() {
		t.Fatalf("auto-update offered without a key: %+v", base)
	}
	v = base
	v.Server, v.NoLocation, v.AutoUpdate = "feed.example.net", true, true // e.g. AUTO_UPDATE=yes in feeder-setup.txt
	if err := v.validate(false); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(v.installerArgs("/bin/f"), " "); !strings.Contains(args, "--no-auto-update") || strings.Contains(args, "--manifest-key") {
		t.Fatalf("keyless args: %s", args)
	}
	a.initial = base
	s := newServer(a)
	r := httptest.NewRequest("GET", "http://airview-feeder.local:8099/", nil)
	r.RemoteAddr = "192.168.1.9:4000"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `id="noupdate"`) || strings.Contains(body, `name="autoUpdate" value="yes" checked`) {
		t.Fatalf("keyless page still offers automatic updates:\n%s", body)
	}
	a.initial.ManifestKey = testManifestKey
	a.initial.AutoUpdate = true
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if body := w.Body.String(); strings.Contains(body, `id="noupdate"`) || !strings.Contains(body, `name="autoUpdate" value="yes" checked`) {
		t.Fatalf("page with a key does not offer automatic updates:\n%s", body)
	}
}

func TestSetupFileUpdateKeys(t *testing.T) {
	v, _, err := parseSetupFile([]byte("SERVER=feed.example.net\nMANIFEST_KEY="+testManifestKey+"\nUPDATE_URL=https://feed.example.net\n"), defaults("x"))
	if err != nil || v.ManifestKey != testManifestKey || v.UpdateURL != "https://feed.example.net" {
		t.Fatalf("%+v %v", v, err)
	}
	for _, bad := range []values{{ManifestKey: "short="}, {UpdateURL: "ftp://x"}, {UpdateURL: "https://a b"}} {
		bad.Server, bad.Name, bad.NoLocation = "feed.example.net", "x", true
		if err := bad.validate(false); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// SEC-R9: the enrol token of feeder-setup.txt is never rendered.
func TestSetupPageHidesFileToken(t *testing.T) {
	a, _ := testApp(t)
	a.fileToken = "SECRETTOKEN123456"
	s := newServer(a)
	r := httptest.NewRequest("GET", "http://airview-feeder.local:8099/", nil)
	r.RemoteAddr = "192.168.1.9:4000"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	body := w.Body.String()
	if strings.Contains(body, a.fileToken) || !strings.Contains(body, "kept, not shown") {
		t.Fatalf("token shown or not announced:\n%s", body)
	}
}
