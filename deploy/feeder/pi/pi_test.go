// The tests of the Raspberry Pi feeder installer
// (deploy/feeder/pi/install.sh). They drive the script in --dry-run mode,
// which changes nothing on the machine and stages every file it would write
// under a temporary root, then check the staged files: feeder.json must decode
// into model.FeederConfig and normalise without corrections, the systemd units
// must carry the DESIGN §12.2 hardening, the wrappers must match the source
// tree, and the download path must verify the Ed25519-signed manifest.
//
// The tests need bash (Git Bash on Windows) and skip without it. Set
// AVF_TEST_BASH to choose the interpreter.

package pi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"airview/internal/model"
)

func findBash(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("AVF_TEST_BASH"); b != "" {
		return b
	}
	if runtime.GOOS == "windows" {
		// Not exec.LookPath("bash"): System32\bash.exe is WSL, not Git Bash.
		for _, p := range []string{
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		t.Skip("Git Bash not found; set AVF_TEST_BASH")
	}
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	return p
}

// slash turns a native path into one bash accepts on every platform.
func slash(p string) string { return filepath.ToSlash(p) }

type result struct {
	out  string
	code int
}

// runScript runs a script of this directory with args and extra env.
func runScript(t *testing.T, script string, env []string, args ...string) result {
	t.Helper()
	bash := findBash(t)
	cmd := exec.Command(bash, append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "AVF_NO_WHIPTAIL=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", script, err)
		}
		code = ee.ExitCode()
	}
	return result{string(out), code}
}

// fakeELF writes a small file that starts with the ELF magic.
func fakeELF(t *testing.T, dir, name string, fill byte) string {
	t.Helper()
	b := append([]byte{0x7f, 'E', 'L', 'F'}, bytes.Repeat([]byte{fill}, 4096)...)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func staged(root, sysPath string) string { return filepath.Join(root, filepath.FromSlash(sysPath)) }

// decodeFeeder decodes a staged feeder.json strictly into model.FeederConfig.
func decodeFeeder(t *testing.T, root string) model.FeederConfig {
	t.Helper()
	raw := readFile(t, staged(root, "/etc/airview-feeder/feeder.json"))
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	cfg := model.DefaultFeederConfig()
	cfg.Receivers, cfg.Uplinks = nil, nil
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("feeder.json does not decode into model.FeederConfig: %v\n%s", err, raw)
	}
	return cfg
}

func baseArgs(root, bin string) []string {
	return []string{"--dry-run", "--dry-run-root", slash(root), "--non-interactive", "--binary", slash(bin)}
}

func TestDryRunInstall(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	bin := fakeELF(t, tmp, "feeder", 1)
	args := append(baseArgs(root, bin),
		"--server", "agg.example.org", "--token", "tok_EN-1234",
		"--pin", "sha256/"+base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"--name", "club-pi_1", "--lat", "52.3731", "--lon", "-4.8922", "--alt", "400ft",
		"--alt-ref", "hae", "--agl", "25ft", "--privacy", "cell01", "--input", "radarcape:192.168.1.50")
	r := runScript(t, "install.sh", []string{"AVF_PYTHON="}, args...)
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.out)
	}
	for _, want := range []string{"[dry-run] useradd --system", "systemctl enable --now airview-feeder-update.timer",
		"systemctl enable airview-feeder", "[dry-run] write /etc/airview-feeder/feeder.json (mode 0640, owner root:airview-feeder)",
		"[dry-run] write /etc/airview-feeder/install.env (mode 0600, owner root:root)", "Claim code",
		"journalctl -u airview-feeder", "uninstall.sh [--purge]"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q\n%s", want, r.out)
		}
	}
	if strings.Contains(r.out, "tok_EN-1234") {
		t.Error("the enrol token is printed")
	}

	cfg := decodeFeeder(t, root)
	if notes := cfg.Normalize(); len(notes) != 0 {
		t.Errorf("feeder.json needed corrections: %v", notes)
	}
	if len(cfg.Receivers) != 1 || len(cfg.Uplinks) != 1 {
		t.Fatalf("receivers/uplinks: %+v", cfg)
	}
	rx, up := cfg.Receivers[0], cfg.Uplinks[0]
	if rx.Name != "club-pi_1" || !rx.Located || rx.Lat != 52.3731 || rx.Lon != -4.8922 || rx.AltRef != model.AltRefHAE {
		t.Errorf("receiver %+v", rx)
	}
	if rx.AltM != 121.9 || rx.AGLM != 7.6 { // 400 ft = 121.92 m, 25 ft = 7.62 m
		t.Errorf("altitude conversion: altM %v aglM %v", rx.AltM, rx.AGLM)
	}
	if rx.Input.Type != model.FeedBeast || rx.Input.Host != "192.168.1.50" || rx.Input.Port != 10003 || !rx.Input.Enabled {
		t.Errorf("input %+v", rx.Input)
	}
	if up.Host != "agg.example.org" || up.Port != model.DefaultAVFPort || up.EnrolToken != "tok_EN-1234" ||
		up.Pin != model.PinCell01 || !up.Timestamps || !up.ReturnMLAT || !up.SendLocation || up.ServerPin == "" {
		t.Errorf("uplink %+v", up)
	}
	if cfg.UI.Addr != model.DefaultFeederUIAddr || cfg.Results.BeastPort != model.DefaultResultsPort || !cfg.AutoUpdate {
		t.Errorf("ui/results/autoUpdate: %+v %+v %v", cfg.UI, cfg.Results, cfg.AutoUpdate)
	}

	unit := readFile(t, staged(root, "/etc/systemd/system/airview-feeder.service"))
	for _, d := range []string{"User=airview-feeder", "Restart=always", "RestartSec=5", "NoNewPrivileges=true",
		"ProtectSystem=strict", "ProtectHome=true", "PrivateTmp=true", "ReadWritePaths=/var/lib/airview-feeder",
		"\nAmbientCapabilities=\n", "\nCapabilityBoundingSet=\n", "MemoryMax=128M",
		"Environment=AIRVIEW_CONFIG=/etc/airview-feeder/feeder.json", "ExecStart=/opt/airview-feeder/airview-feeder run"} {
		if !strings.Contains(unit, d) {
			t.Errorf("service unit lacks %q", d)
		}
	}
	timer := readFile(t, staged(root, "/etc/systemd/system/airview-feeder-update.timer"))
	if !strings.Contains(timer, "OnCalendar=daily") || !strings.Contains(timer, "RandomizedDelaySec=") {
		t.Errorf("timer: %s", timer)
	}
	if !strings.Contains(readFile(t, staged(root, "/etc/systemd/system/airview-feeder-update.service")),
		"ExecStart=/opt/airview-feeder/update.sh --quiet") {
		t.Error("update service does not run update.sh")
	}

	// Wrappers and the installed copy of the script equal the source tree.
	for _, f := range []string{"update.sh", "uninstall.sh", "status.sh", "install.sh"} {
		if got, want := readFile(t, staged(root, "/opt/airview-feeder/"+f)), readFile(t, f); got != want {
			t.Errorf("staged %s differs from deploy/feeder/pi/%s", f, f)
		}
	}
	if got := readFile(t, staged(root, "/opt/airview-feeder/airview-feeder")); got != readFile(t, bin) {
		t.Error("staged binary differs")
	}
}

func TestDryRunNoLocationNoTimestamps(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	bin := fakeELF(t, tmp, "feeder", 2)
	r := runScript(t, "install.sh", []string{"AVF_PYTHON="}, append(baseArgs(root, bin),
		"--server=[2001:db8::1]:31000", "--no-location", "--no-timestamps", "--input", "avr:10.0.0.2:30002",
		"--no-auto-update", "--readsb-results", "--name", "x")...)
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.out)
	}
	cfg := decodeFeeder(t, root)
	if notes := cfg.Normalize(); len(notes) != 0 {
		t.Errorf("corrections: %v", notes)
	}
	rx, up := cfg.Receivers[0], cfg.Uplinks[0]
	if rx.Located || rx.Lat != 0 || up.Timestamps || up.Host != "[2001:db8::1]" || up.Port != 31000 {
		t.Errorf("rx %+v up %+v", rx, up)
	}
	if rx.Input.Type != model.FeedAVR || rx.Input.Port != 30002 || cfg.AutoUpdate || cfg.Results.ReadsbAddr != "127.0.0.1:30104" {
		t.Errorf("input %+v autoUpdate %v results %+v", rx.Input, cfg.AutoUpdate, cfg.Results)
	}
	if !strings.Contains(r.out, "systemctl disable --now airview-feeder-update.timer") {
		t.Error("update timer not disabled with --no-auto-update")
	}
	if !strings.Contains(r.out, "not given - the network will locate the station") {
		t.Error("no-location summary missing")
	}
}

// A re-run without flags keeps the saved answers; a flag changes just that value.
func TestDryRunRerunKeepsValues(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	bin := fakeELF(t, tmp, "feeder", 3)
	first := runScript(t, "install.sh", []string{"AVF_PYTHON="}, append(baseArgs(root, bin),
		"--server", "agg.example.org:30161", "--token", "secret-token", "--name", "alpha",
		"--lat", "10.5", "--lon", "20.25", "--alt", "100m")...)
	if first.code != 0 {
		t.Fatalf("first run exit %d\n%s", first.code, first.out)
	}
	second := runScript(t, "install.sh", []string{"AVF_PYTHON="}, append(baseArgs(root, bin), "--privacy", "region")...)
	if second.code != 0 {
		t.Fatalf("second run exit %d\n%s", second.code, second.out)
	}
	cfg := decodeFeeder(t, root)
	rx, up := cfg.Receivers[0], cfg.Uplinks[0]
	if rx.Name != "alpha" || rx.Lat != 10.5 || rx.Lon != 20.25 || rx.AltM != 100 || up.Port != 30161 ||
		up.EnrolToken != "secret-token" || up.Pin != model.PinRegion {
		t.Errorf("re-run lost values: rx %+v up %+v", rx, up)
	}
	// Without python3 an unchanged config is not backed up; a changed one is.
	if !strings.Contains(second.out, "feeder.json.bak-") {
		t.Errorf("changed config without python3 should be backed up\n%s", second.out)
	}
	env := readFile(t, staged(root, "/etc/airview-feeder/install.env"))
	if !strings.Contains(env, "S_PRIVACY=region") || !strings.Contains(env, "S_TOKEN=secret-token") {
		t.Errorf("install.env: %s", env)
	}
}

func findPython(t *testing.T) string {
	for _, c := range []string{"python3", "python", `C:\Python314\python.exe`} {
		p, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		if exec.Command(p, "-c", "import json, sys; sys.exit(0)").Run() == nil {
			return p
		}
	}
	t.Skip("python3 not available")
	return ""
}

// With python3 a re-run merges into feeder.json and keeps everything the
// installer does not manage.
func TestDryRunMergeKeepsOtherSettings(t *testing.T) {
	py := findPython(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	bin := fakeELF(t, tmp, "feeder", 4)
	conf := staged(root, "/etc/airview-feeder/feeder.json")
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"version":1,"mode":"feeder",
	 "receivers":[{"rxNo":1,"name":"old","input":{"id":"input-1","type":"auto","enabled":true},"altRef":"msl"},
	              {"rxNo":2,"name":"second","input":{"id":"input-2","type":"beast","host":"10.1.1.1","port":30005,"enabled":true},"altRef":"msl"}],
	 "uplinks":[{"id":"other","host":"club.example.net","port":30160}],
	 "ui":{"addr":"0.0.0.0:8099","passwordHash":"keep-me"},"ntp":{"enabled":true,"server":"pool.ntp.org","intervalSec":64},
	 "autoUpdate":true}`
	if err := os.WriteFile(conf, []byte(existing), 0o640); err != nil {
		t.Fatal(err)
	}
	r := runScript(t, "install.sh", []string{"AVF_PYTHON=" + slash(py)}, append(baseArgs(root, bin),
		"--server", "agg.example.org", "--name", "new", "--no-location")...)
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "other settings kept") {
		t.Errorf("merge not used\n%s", r.out)
	}
	cfg := decodeFeeder(t, root)
	if len(cfg.Receivers) != 2 || cfg.Receivers[0].Name != "new" || cfg.Receivers[1].Name != "second" {
		t.Errorf("receivers %+v", cfg.Receivers)
	}
	if len(cfg.Uplinks) != 2 || cfg.Uplinks[0].ID != "other" || cfg.Uplinks[1].ID != "aggregator" ||
		cfg.Uplinks[1].Host != "agg.example.org" {
		t.Errorf("uplinks %+v", cfg.Uplinks)
	}
	if cfg.UI.PasswordHash != "keep-me" || cfg.NTP.Server != "pool.ntp.org" {
		t.Errorf("unmanaged settings lost: ui %+v ntp %+v", cfg.UI, cfg.NTP)
	}
}

func TestValidationErrors(t *testing.T) {
	tmp := t.TempDir()
	bin := fakeELF(t, tmp, "feeder", 5)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no server", nil, "no aggregator given"},
		{"bad name", []string{"--server", "a.b", "--name", "bad name!"}, "invalid --name"},
		{"long name", []string{"--server", "a.b", "--name", strings.Repeat("a", 33)}, "invalid --name"},
		{"alt without unit", []string{"--server", "a.b", "--lat", "1", "--lon", "2", "--alt", "120"}, "invalid --alt"},
		{"alt missing", []string{"--server", "a.b", "--lat", "1", "--lon", "2"}, "--alt is needed"},
		{"lat range", []string{"--server", "a.b", "--lat", "91", "--lon", "2", "--alt", "1m"}, "invalid --lat"},
		{"lon only", []string{"--server", "a.b", "--lon", "2", "--alt", "1m"}, "--lat and --lon are both needed"},
		{"alt range", []string{"--server", "a.b", "--lat", "1", "--lon", "2", "--alt", "40000ft"}, "out of range"},
		{"bad port", []string{"--server", "a.b:70000"}, "invalid --server"},
		{"bad pin", []string{"--server", "a.b", "--pin", "md5/abc"}, "invalid --pin"},
		{"bad token", []string{"--server", "a.b", "--token", "a b c d"}, "invalid --token"},
		{"bad privacy", []string{"--server", "a.b", "--privacy", "exact"}, "invalid --privacy"},
		{"bad input", []string{"--server", "a.b", "--input", "serial:/dev/ttyUSB0"}, "invalid --input"},
		{"bad arch", []string{"--server", "a.b", "--arch", "armv6"}, "--arch must be"},
		{"unknown flag", []string{"--frobnicate"}, "unknown option"},
		{"plain http unsigned", []string{"--server", "a.b", "--url", "http://a.b"}, "refusing an unsigned download"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "root")
			args := []string{"--dry-run", "--dry-run-root", slash(root), "--non-interactive"}
			if c.name != "plain http unsigned" {
				args = append(args, "--binary", slash(bin))
			}
			r := runScript(t, "install.sh", []string{"AVF_PYTHON="}, append(args, c.args...)...)
			if r.code == 0 || !strings.Contains(r.out, c.want) {
				t.Errorf("exit %d, want error %q\n%s", r.code, c.want, r.out)
			}
			if _, err := os.Stat(staged(root, "/etc/airview-feeder/feeder.json")); err == nil {
				t.Error("feeder.json was staged despite the error")
			}
		})
	}
}

// releaseServer serves a signed manifest and the binaries like an aggregator's
// /feeder/ routes. It listens on the WP's assigned port when it is free.
type releaseServer struct {
	url      string
	pub      string
	manifest []byte
	sig      string
	bins     map[string][]byte
}

func newReleaseServer(t *testing.T, bins map[string][]byte, tamper func(*releaseServer)) *releaseServer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for target, b := range bins {
		sum := sha256.Sum256(b)
		files = append(files, fmt.Sprintf(`{"target": %q, "url": "/feeder/dl/%s", "sha256": %q, "size": %d}`,
			target, target, hex.EncodeToString(sum[:]), len(b)))
	}
	rs := &releaseServer{bins: bins, pub: base64.StdEncoding.EncodeToString(pub)}
	rs.manifest = []byte("{\n  \"product\": \"airview-feeder\",\n  \"version\": \"1.2.3\",\n  \"channel\": \"stable\",\n  \"files\": [\n    " +
		strings.Join(files, ",\n    ") + "\n  ]\n}\n")
	rs.sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, rs.manifest)) + "\n"
	if tamper != nil {
		tamper(rs)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/feeder/manifest.json", func(w http.ResponseWriter, r *http.Request) { w.Write(rs.manifest) })
	mux.HandleFunc("/feeder/manifest.json.sig", func(w http.ResponseWriter, r *http.Request) { writeStr(w, rs.sig) })
	mux.HandleFunc("/feeder/dl/", func(w http.ResponseWriter, r *http.Request) {
		b, ok := rs.bins[strings.TrimPrefix(r.URL.Path, "/feeder/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:18528")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	rs.url = "http://" + ln.Addr().String()
	return rs
}

func writeStr(w http.ResponseWriter, s string) { w.Write([]byte(s)) }

func haveOpenSSL3(t *testing.T) {
	t.Helper()
	bash := findBash(t)
	out, err := exec.Command(bash, "-c", "openssl version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "OpenSSL 3") {
		t.Skipf("OpenSSL 3 not available in bash: %s", out)
	}
	if out, err := exec.Command(bash, "-c", "command -v curl || command -v wget").CombinedOutput(); err != nil {
		t.Skipf("no curl/wget: %s", out)
	}
}

func elfBlob(fill byte) []byte {
	return append([]byte{0x7f, 'E', 'L', 'F'}, bytes.Repeat([]byte{fill}, 8192)...)
}

func TestSignedDownload(t *testing.T) {
	haveOpenSSL3(t)
	bins := map[string][]byte{"linux-arm64": elfBlob(0xa1), "linux-armv7": elfBlob(0xa2), "linux-amd64": elfBlob(0xa3)}
	for _, arch := range []struct{ flag, target string }{{"arm64", "linux-arm64"}, {"armhf", "linux-armv7"}, {"amd64", "linux-amd64"}} {
		t.Run(arch.flag, func(t *testing.T) {
			rs := newReleaseServer(t, bins, nil)
			root := filepath.Join(t.TempDir(), "root")
			r := runScript(t, "install.sh", []string{"AVF_PYTHON="}, "--dry-run", "--dry-run-root", slash(root),
				"--non-interactive", "--server", "127.0.0.1", "--url", rs.url, "--manifest-key", rs.pub, "--arch", arch.flag)
			if r.code != 0 {
				t.Fatalf("exit %d\n%s", r.code, r.out)
			}
			if !strings.Contains(r.out, "manifest signature verified") || !strings.Contains(r.out, "SHA-256 matches") ||
				!strings.Contains(r.out, "airview-feeder 1.2.3 for "+arch.target) {
				t.Errorf("verification not reported\n%s", r.out)
			}
			got, err := os.ReadFile(staged(root, "/opt/airview-feeder/airview-feeder"))
			if err != nil || !bytes.Equal(got, bins[arch.target]) {
				t.Errorf("staged binary is not the %s build", arch.target)
			}
		})
	}
}

func TestSignedDownloadRejects(t *testing.T) {
	haveOpenSSL3(t)
	bins := map[string][]byte{"linux-amd64": elfBlob(0xb1)}
	cases := []struct {
		name   string
		tamper func(*releaseServer)
		key    func(*releaseServer) string
		want   string
	}{
		{"bad signature", func(rs *releaseServer) {
			rs.manifest = bytes.Replace(rs.manifest, []byte("1.2.3"), []byte("6.6.6"), 1)
		}, nil, "signature is INVALID"},
		{"wrong key", nil, func(*releaseServer) string {
			pub, _, _ := ed25519.GenerateKey(rand.Reader)
			return base64.StdEncoding.EncodeToString(pub)
		}, "signature is INVALID"},
		{"hash mismatch", func(rs *releaseServer) {
			rs.bins = map[string][]byte{"linux-amd64": elfBlob(0xb2)}
		}, nil, "SHA-256 mismatch"},
		{"not elf", func(rs *releaseServer) {
			// Re-sign a manifest for a non-ELF payload.
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			b := bytes.Repeat([]byte{'x'}, 100)
			sum := sha256.Sum256(b)
			rs.bins = map[string][]byte{"linux-amd64": b}
			rs.manifest = []byte(fmt.Sprintf(`{"version":"1","files":[{"target":"linux-amd64","sha256":"%x"}]}`, sum))
			rs.sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, rs.manifest))
			rs.pub = base64.StdEncoding.EncodeToString(pub)
		}, nil, "not a Linux executable"},
		{"missing target", func(rs *releaseServer) {
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			rs.manifest = []byte(`{"version":"1","files":[{"target":"linux-arm64","sha256":"00"}]}`)
			rs.sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, rs.manifest))
			rs.pub = base64.StdEncoding.EncodeToString(pub)
		}, nil, "no sha256 for linux-amd64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs := newReleaseServer(t, bins, c.tamper)
			key := rs.pub
			if c.key != nil {
				key = c.key(rs)
			}
			root := filepath.Join(t.TempDir(), "root")
			r := runScript(t, "install.sh", []string{"AVF_PYTHON="}, "--dry-run", "--dry-run-root", slash(root),
				"--non-interactive", "--server", "127.0.0.1", "--url", rs.url, "--manifest-key", key, "--arch", "amd64")
			if r.code == 0 || !strings.Contains(r.out, c.want) {
				t.Errorf("exit %d, want %q\n%s", r.code, c.want, r.out)
			}
			if _, err := os.Stat(staged(root, "/opt/airview-feeder/airview-feeder")); err == nil {
				t.Error("a binary was staged despite the failed verification")
			}
		})
	}
}

func TestUpdateDryRun(t *testing.T) {
	haveOpenSSL3(t)
	bins := map[string][]byte{"linux-amd64": elfBlob(0xc1)}
	rs := newReleaseServer(t, bins, nil)
	root := filepath.Join(t.TempDir(), "root")
	common := []string{"--dry-run", "--dry-run-root", slash(root)}
	r := runScript(t, "install.sh", []string{"AVF_PYTHON="}, append(common, "--non-interactive", "--server", "127.0.0.1",
		"--url", rs.url, "--manifest-key", rs.pub, "--arch", "amd64")...)
	if r.code != 0 {
		t.Fatalf("install exit %d\n%s", r.code, r.out)
	}
	// Same binary on the server: nothing to do.
	r = runScript(t, "update.sh", nil, common...)
	if r.code != 0 || !strings.Contains(r.out, "up to date (1.2.3)") {
		t.Fatalf("update (same) exit %d\n%s", r.code, r.out)
	}
	// New binary: swap with backup, restart, health check.
	rs2 := newReleaseServer(t, map[string][]byte{"linux-amd64": elfBlob(0xc2)}, nil)
	env := readFile(t, staged(root, "/etc/airview-feeder/install.env"))
	env = strings.NewReplacer(rs.url, rs2.url, rs.pub, rs2.pub).Replace(env)
	if err := os.WriteFile(staged(root, "/etc/airview-feeder/install.env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runScript(t, "update.sh", nil, common...)
	if r.code != 0 {
		t.Fatalf("update exit %d\n%s", r.code, r.out)
	}
	for _, want := range []string{"cp -p /opt/airview-feeder/airview-feeder /opt/airview-feeder/airview-feeder.old",
		"systemctl restart airview-feeder", "roll back"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("update output lacks %q\n%s", want, r.out)
		}
	}
	if got := readFile(t, staged(root, "/opt/airview-feeder/airview-feeder")); got != string(elfBlob(0xc2)) {
		t.Error("updated binary not staged")
	}
	// autoUpdate=false: the quiet timer run does nothing.
	conf := staged(root, "/etc/airview-feeder/feeder.json")
	c := strings.Replace(readFile(t, conf), `"autoUpdate": true`, `"autoUpdate": false`, 1)
	os.WriteFile(conf, []byte(c), 0o640)
	r = runScript(t, "update.sh", nil, append(common, "--quiet")...)
	if r.code != 0 || strings.Contains(r.out, "Downloading") {
		t.Errorf("autoUpdate=false still updates: exit %d\n%s", r.code, r.out)
	}
}

func TestUpdateNotInstalled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	r := runScript(t, "update.sh", nil, "--dry-run", "--dry-run-root", slash(root))
	if r.code == 0 || !strings.Contains(r.out, "not installed") {
		t.Errorf("exit %d\n%s", r.code, r.out)
	}
}

func TestUninstallDryRun(t *testing.T) {
	for _, purge := range []bool{false, true} {
		root := filepath.Join(t.TempDir(), "root")
		args := []string{"--dry-run", "--dry-run-root", slash(root), "-y"}
		if purge {
			args = append(args, "--purge")
		}
		r := runScript(t, "uninstall.sh", nil, args...)
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.out)
		}
		for _, want := range []string{"systemctl disable --now airview-feeder.service", "rm -rf /opt/airview-feeder",
			"rm -f /usr/local/sbin/airview-feeder-setup", "not touched"} {
			if !strings.Contains(r.out, want) {
				t.Errorf("purge=%v: lacks %q\n%s", purge, want, r.out)
			}
		}
		removesData := strings.Contains(r.out, "rm -rf /etc/airview-feeder /var/lib/airview-feeder")
		if removesData != purge || strings.Contains(r.out, "userdel airview-feeder") != purge {
			t.Errorf("purge=%v: data removal %v\n%s", purge, removesData, r.out)
		}
		for _, line := range strings.Split(r.out, "\n") {
			if strings.HasPrefix(line, "[dry-run]") && (strings.Contains(line, "readsb") || strings.Contains(line, "dump1090")) {
				t.Errorf("uninstall touches another decoder: %s", line)
			}
		}
	}
}

func TestStatusDryRun(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	r := runScript(t, "status.sh", nil, "--dry-run", "--dry-run-root", slash(root))
	if r.code == 0 || !strings.Contains(r.out, "Not installed") {
		t.Errorf("status before install: exit %d\n%s", r.code, r.out)
	}
	bin := fakeELF(t, tmp, "feeder", 6)
	if r := runScript(t, "install.sh", []string{"AVF_PYTHON="}, append(baseArgs(root, bin), "--server", "a.example",
		"--token", "hidden-token", "--name", "st")...); r.code != 0 {
		t.Fatalf("install: %s", r.out)
	}
	r = runScript(t, "status.sh", nil, "--dry-run", "--dry-run-root", slash(root))
	if r.code != 0 || !strings.Contains(r.out, "Aggregator   : a.example:30160") || !strings.Contains(r.out, "Station      : st") {
		t.Errorf("status: exit %d\n%s", r.code, r.out)
	}
	if strings.Contains(r.out, "hidden-token") {
		t.Error("status prints the enrol token")
	}
}

// The script also works when piped into bash; the installed copy is then
// regenerated from its functions and must itself run.
func TestPipedRunRegeneratesScript(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	bin := fakeELF(t, tmp, "feeder", 7)
	src, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(findBash(t), append([]string{"-s", "--"}, append(baseArgs(root, bin), "--server", "p.example", "--name", "piped")...)...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "AVF_PYTHON=")
	cmd.Stdin = bytes.NewReader(src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("piped run: %v\n%s", err, out)
	}
	regen := staged(root, "/opt/airview-feeder/install.sh")
	body := readFile(t, regen)
	if !strings.Contains(body, "regenerated from a piped run") || !strings.Contains(body, "avf_presets ()") ||
		!strings.HasSuffix(body, "main \"$@\"\n") {
		t.Fatalf("regenerated script looks wrong:\n%.400s", body)
	}
	r := runScript(t, slash(regen), nil, "--version")
	if r.code != 0 || !strings.Contains(r.out, "airview-feeder installer 1.0.0") {
		t.Errorf("regenerated script does not run: %d %s", r.code, r.out)
	}
}

// The aggregator pre-fills the PRESET_ lines when it serves the script.
func TestPresets(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	bin := fakeELF(t, tmp, "feeder", 8)
	src := readFile(t, "install.sh")
	pin := "sha256/" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	for _, kv := range [][2]string{{"PRESET_SERVER", "preset.example:30170"}, {"PRESET_PIN", pin}, {"PRESET_URL", "https://preset.example"}} {
		line := "\t" + kv[0] + `=""`
		if strings.Count(src, line) != 1 {
			t.Fatalf("preset line %q not found exactly once", line)
		}
		src = strings.Replace(src, line, "\t"+kv[0]+`="`+kv[1]+`"`, 1)
	}
	p := filepath.Join(tmp, "served-install.sh")
	if err := os.WriteFile(p, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runScript(t, slash(p), []string{"AVF_PYTHON="}, append(baseArgs(root, bin), "--name", "pre")...)
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.out)
	}
	up := decodeFeeder(t, root).Uplinks[0]
	if up.Host != "preset.example" || up.Port != 30170 || up.ServerPin != pin {
		t.Errorf("presets not used: %+v", up)
	}
	if !strings.Contains(r.out, "https://preset.example/me") {
		t.Errorf("preset URL not used\n%s", r.out)
	}
}

func TestHelpAndVersion(t *testing.T) {
	r := runScript(t, "install.sh", nil, "--help")
	for _, flag := range []string{"--server", "--token", "--lat", "--alt", "--name", "--non-interactive", "--dry-run", "--uninstall", "--status"} {
		if !strings.Contains(r.out, flag) {
			t.Errorf("--help lacks %s", flag)
		}
	}
	if r := runScript(t, "install.sh", nil, "--version"); r.code != 0 || !strings.Contains(r.out, "1.0.0") {
		t.Errorf("--version: %s", r.out)
	}
}

// TestConsentPinMatchesDoc: the installer shows consent text C2 (public pin)
// before the privacy-level prompt, word for word as in CONSENT.md.
func TestConsentPinMatchesDoc(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "aggregator", "CONSENT.md"))
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	in := false
	for _, line := range strings.Split(strings.ReplaceAll(string(doc), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "---") {
			in = strings.HasPrefix(line, "## C2")
			continue
		}
		if in && strings.HasPrefix(line, ">") {
			l := strings.TrimSpace(strings.TrimPrefix(line, ">"))
			l = strings.TrimPrefix(l, "- ")
			want.WriteString(strings.Trim(strings.ReplaceAll(l, "**", ""), `"`) + "\n")
		}
	}
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ReplaceAll(s, "  - ", "")), " ") }
	sh, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(sh), "\r\n", "\n")
	i := strings.Index(s, `CONSENT_PIN="`)
	if i < 0 {
		t.Fatal("CONSENT_PIN not in install.sh")
	}
	rest := s[i+len(`CONSENT_PIN="`):]
	got := rest[:strings.Index(rest, `"`)]
	if w := norm(want.String()); w == "" || norm(got) != w {
		t.Fatalf("C2 differs from CONSENT.md:\n got  %q\n want %q", norm(got), w)
	}
	if !strings.Contains(s, `show_text "$CONSENT_PIN"`) {
		t.Fatal("C2 is not shown")
	}
}

// TestChannelAndCheck covers the update command's release channel and its
// check-only mode (WP-W3-wire): --channel is written into feeder.json
// (update.channel, the feeder's selfupdate settings) and the saved answers;
// --update --check asks the feeder's own updater and installs nothing.
func TestChannelAndCheck(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	bin := fakeELF(t, tmp, "feeder", 3)
	args := append(baseArgs(root, bin), "--server", "agg.example.org", "--channel", "beta")
	r := runScript(t, "install.sh", []string{"AVF_PYTHON="}, args...)
	if r.code != 0 {
		t.Fatalf("install exit %d\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "(channel beta)") {
		t.Errorf("settings summary lacks the channel\n%s", r.out)
	}
	if cfg := decodeFeeder(t, root); cfg.Update.Channel != "beta" {
		t.Errorf("update.channel = %q, want beta", cfg.Update.Channel)
	}
	if st := readFile(t, staged(root, "/etc/airview-feeder/install.env")); !strings.Contains(st, "S_CHANNEL=beta") {
		t.Errorf("saved answers lack the channel:\n%s", st)
	}
	common := []string{"--dry-run", "--dry-run-root", slash(root)}

	// Check only: the feeder's updater with -check, nothing installed.
	r = runScript(t, "update.sh", nil, append(common, "--check")...)
	if r.code != 0 || !strings.Contains(r.out, "/opt/airview-feeder/airview-feeder update -check -config /etc/airview-feeder/feeder.json -data /var/lib/airview-feeder") {
		t.Errorf("--check: exit %d\n%s", r.code, r.out)
	}
	if strings.Contains(r.out, "systemctl restart") {
		t.Errorf("--check restarted the service\n%s", r.out)
	}

	// Switching the channel on update (needs python3 to edit feeder.json).
	py := ""
	for _, c := range []string{"python3", "python"} {
		if p, err := exec.LookPath(c); err == nil {
			if exec.Command(p, "-c", "import json").Run() == nil {
				py = p
				break
			}
		}
	}
	if py != "" {
		r = runScript(t, "update.sh", []string{"AVF_PYTHON=" + slash(py)}, append(common, "--check", "--channel", "stable")...)
		if r.code != 0 || !strings.Contains(r.out, "update -check -channel stable") {
			t.Errorf("--check --channel: exit %d\n%s", r.code, r.out)
		}
		if cfg := decodeFeeder(t, root); cfg.Update.Channel != "stable" {
			t.Errorf("after --channel stable: update.channel = %q", cfg.Update.Channel)
		}
		if st := readFile(t, staged(root, "/etc/airview-feeder/install.env")); !strings.Contains(st, "S_CHANNEL=stable") {
			t.Errorf("saved answers keep the old channel:\n%s", st)
		}
	} else {
		t.Log("no python3: channel switch on update not exercised")
	}

	// Validation.
	if r = runScript(t, "update.sh", nil, append(common, "--channel", "nightly")...); r.code == 0 || !strings.Contains(r.out, "invalid --channel") {
		t.Errorf("bad channel accepted: exit %d\n%s", r.code, r.out)
	}
	if r = runScript(t, "install.sh", nil, append(common, "--status", "--check")...); r.code == 0 || !strings.Contains(r.out, "--check goes with --update") {
		t.Errorf("--check without --update accepted: exit %d\n%s", r.code, r.out)
	}
}

// DLV-05: the licence texts next to a local --binary (the bundle, the
// SD-card image's /opt/airview-image) are installed into /opt/airview-feeder.
func TestDryRunInstallsNotices(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	bin := fakeELF(t, tmp, "airview-feeder-linux-arm64", 1)
	for _, f := range []string{"LICENSE.txt", "THIRD-PARTY-NOTICES.txt"} {
		if err := os.WriteFile(filepath.Join(tmp, f), []byte(f+" text\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := runScript(t, "install.sh", []string{"AVF_PYTHON="}, append(baseArgs(root, bin), "--server", "agg.example.org", "--no-location")...)
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.out)
	}
	for _, f := range []string{"LICENSE.txt", "THIRD-PARTY-NOTICES.txt"} {
		if !strings.Contains(r.out, "[dry-run] write /opt/airview-feeder/"+f+" (mode 0644, owner root:root)") {
			t.Errorf("%s not installed:\n%s", f, r.out)
		}
	}
	// Without them next to the binary nothing is copied (and nothing fails).
	tmp2 := t.TempDir()
	bin2 := fakeELF(t, tmp2, "airview-feeder-linux-arm64", 1)
	r = runScript(t, "install.sh", []string{"AVF_PYTHON="}, append(baseArgs(filepath.Join(tmp2, "root"), bin2), "--server", "agg.example.org", "--no-location")...)
	if r.code != 0 || strings.Contains(r.out, "/opt/airview-feeder/LICENSE.txt") {
		t.Fatalf("exit %d\n%s", r.code, r.out)
	}
}
