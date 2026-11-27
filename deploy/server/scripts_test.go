package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"airview/internal/config"
	"airview/internal/model"
)

// ---------------------------------------------------------------------------
// Test rig: a scratch root (AIRVIEW_DESTDIR), stub system commands first in
// PATH, and a fake "airview" binary (a shell script) in each bundle.
// ---------------------------------------------------------------------------

// findSh returns a POSIX shell, or "" when there is none.
func findSh() string {
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\Program Files\Git\usr\bin\sh.exe`,
			`C:\Program Files\Git\bin\sh.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// posix converts a native path for the shell (C:\a\b -> /c/a/b on Windows).
func posix(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	p = filepath.ToSlash(p)
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + strings.ToLower(p[:1]) + p[2:]
	}
	return p
}

type rig struct {
	t     *testing.T
	sh    string
	root  string // AIRVIEW_DESTDIR
	state string // stub state: log, active flag, user flag, served files
	stubs string
	tmp   string
}

const stubSystemctl = `#!/bin/sh
echo "systemctl $*" >> "$STUB_STATE/log"
case "$1" in
    is-active) [ -f "$STUB_STATE/active" ]; exit $? ;;
    is-failed) exit 1 ;;
    start | restart) touch "$STUB_STATE/active" ;;
    try-restart) [ ! -f "$STUB_STATE/active" ] || touch "$STUB_STATE/active" ;;
    stop) rm -f "$STUB_STATE/active" ;;
esac
exit 0
`

const stubCurl = `#!/bin/sh
echo "curl $*" >> "$STUB_STATE/log"
out=""
url=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        --proto | --max-time | --retry) shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
case "$url" in
    */healthz)
        if [ -f "$STUB_STATE/bad_version" ] &&
            "$AIRVIEW_DESTDIR/opt/airview/airview" -version | grep -q "$(cat "$STUB_STATE/bad_version")"; then
            exit 22
        fi
        exit 0
        ;;
    *)
        f="$STUB_STATE/serve/${url##*/}"
        [ -f "$f" ] || exit 22
        cp "$f" "$out"
        ;;
esac
`

const stubSqlite3 = `#!/bin/sh
# sqlite3 DB ".timeout N" ".backup 'DEST'": copy DB with a marker.
db="$1"
for a in "$@"; do last="$a"; done
dest="$(printf '%s' "$last" | sed "s/^\.backup '//; s/'$//")"
{ printf 'snap:'; cat "$db"; } > "$dest"
echo "sqlite3 $db -> $dest" >> "$STUB_STATE/log"
`

var stubs = map[string]string{
	"systemctl": stubSystemctl,
	"curl":      stubCurl,
	"sqlite3":   stubSqlite3,
	"useradd":   "#!/bin/sh\necho \"useradd $*\" >> \"$STUB_STATE/log\"\ntouch \"$STUB_STATE/user\"\n",
	"userdel":   "#!/bin/sh\necho \"userdel $*\" >> \"$STUB_STATE/log\"\nrm -f \"$STUB_STATE/user\"\n",
	"id": `#!/bin/sh
case "$1" in
    -u) echo 0 ;;
    -un) echo root ;;
    airview) [ -f "$STUB_STATE/user" ] ;;
    *) exit 1 ;;
esac
`,
	"chown":      "#!/bin/sh\necho \"chown $*\" >> \"$STUB_STATE/log\"\n",
	"runuser":    "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n",
	"journalctl": "#!/bin/sh\nexit 0\n",
	"hostname":   "#!/bin/sh\nif [ \"${1:-}\" = -I ]; then echo 192.0.2.10; else echo testhost; fi\n",
	"sleep":      "#!/bin/sh\nexit 0\n",
}

// fakeAirview is the stand-in binary: -version, config example/check/print.
const fakeAirview = `#!/bin/sh
V=__VERSION__
case "$1" in
    -version) echo "AirView $V (linux/amd64, go1.27.3)" ;;
    config)
        case "$2" in
            example) [ "$3" = -o ] && echo "// example $V" > "$4" ;;
            check)
                echo "$AIRVIEW_CONFIG|$AIRVIEW_DATA" > "$STUB_STATE/check_env"
                exit "$(cat "$STUB_STATE/check_rc" 2>/dev/null || echo 0)"
                ;;
            print)
                printf '{\n  "mode": "aggregator",\n  "http": {\n    "port": 8080,\n    "bindAddr": "127.0.0.1"\n  }\n}\n'
                ;;
        esac
        ;;
    db)
        # "db snapshot -data DIR -o OUT" exists only when the test says so;
        # like the real binary, unknown commands exit 2.
        [ -f "$STUB_STATE/has_snapshot" ] || exit 2
        [ "$3" = -h ] && exit 0
        for f in "$4"/*.db; do { printf 'vac:'; cat "$f"; } > "$6/$(basename "$f")"; done
        ;;
    *) exit 2 ;;
esac
`

func newRig(t *testing.T) *rig {
	t.Helper()
	sh := findSh()
	if sh == "" {
		t.Skip("no POSIX sh available")
	}
	base := t.TempDir()
	r := &rig{t: t, sh: sh,
		root:  filepath.Join(base, "root"),
		state: filepath.Join(base, "state"),
		stubs: filepath.Join(base, "stubs"),
		tmp:   filepath.Join(base, "tmp"),
	}
	for _, d := range []string{r.root, r.state, r.stubs, r.tmp, filepath.Join(r.state, "serve")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	for name, body := range stubs {
		must(t, os.WriteFile(filepath.Join(r.stubs, name), []byte(body), 0o755))
	}
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// needGNUTar skips when tar is not GNU tar (backup.sh uses --transform).
func (r *rig) needGNUTar() {
	out, _ := r.shell("tar --version 2>&1 | head -n 1")
	if !strings.Contains(out, "GNU tar") {
		r.t.Skip("GNU tar not available: " + out)
	}
}

func (r *rig) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AIRVIEW_") || strings.HasPrefix(strings.ToUpper(kv), "PATH=") || strings.HasPrefix(kv, "TMPDIR=") {
			continue
		}
		env = append(env, kv)
	}
	path := r.stubs + string(os.PathListSeparator) + os.Getenv("PATH")
	if runtime.GOOS == "windows" {
		// Git for Windows' tools next to the shell.
		path += string(os.PathListSeparator) + filepath.Dir(r.sh)
	}
	return append(env,
		"PATH="+path,
		"AIRVIEW_DESTDIR="+posix(r.root),
		"AIRVIEW_ARCH=x86_64",
		"STUB_STATE="+posix(r.state),
		"TMPDIR="+posix(r.tmp),
	)
}

// run runs a script with arguments; it returns the combined output and the
// exit status.
func (r *rig) run(script string, args ...string) (string, int) {
	r.t.Helper()
	cmd := exec.Command(r.sh, append([]string{posix(script)}, args...)...)
	return r.exec(cmd, nil)
}

// shell runs a shell command line in the rig environment.
func (r *rig) shell(line string) (string, int) {
	return r.exec(exec.Command(r.sh, "-c", line), nil)
}

func (r *rig) exec(cmd *exec.Cmd, stdin io.Reader) (string, int) {
	r.t.Helper()
	cmd.Env = r.env()
	cmd.Dir = r.tmp
	cmd.Stdin = stdin
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			r.t.Fatalf("run %v: %v", cmd.Args, err)
		}
		code = ee.ExitCode()
	}
	return b.String(), code
}

// bundle builds an unpacked server bundle with a fake binary of version v.
func (r *rig) bundle(v string) string {
	r.t.Helper()
	dir := filepath.Join(r.t.TempDir(), "airview-server-linux-amd64")
	must(r.t, os.MkdirAll(filepath.Join(dir, "proxy"), 0o755))
	entries, err := os.ReadDir(".")
	must(r.t, err)
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !(strings.HasSuffix(n, ".sh") || n == "airview.service" || n == "airview.logrotate" || strings.HasSuffix(n, ".md")) {
			continue
		}
		copyFile(r.t, n, filepath.Join(dir, n))
	}
	proxies, err := os.ReadDir("proxy")
	must(r.t, err)
	for _, e := range proxies {
		copyFile(r.t, filepath.Join("proxy", e.Name()), filepath.Join(dir, "proxy", e.Name()))
	}
	must(r.t, os.WriteFile(filepath.Join(dir, "airview"), []byte(strings.ReplaceAll(fakeAirview, "__VERSION__", v)), 0o755))
	return dir
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	must(t, err)
	// The scripts must run with LF endings even from a CRLF checkout.
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	must(t, os.WriteFile(to, b, 0o755))
}

func (r *rig) path(rel string) string { return filepath.Join(r.root, filepath.FromSlash(rel)) }

func (r *rig) read(rel string) string {
	r.t.Helper()
	b, err := os.ReadFile(r.path(rel))
	if err != nil {
		r.t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func (r *rig) exists(rel string) bool {
	_, err := os.Stat(r.path(rel))
	return err == nil
}

func (r *rig) log() string {
	b, _ := os.ReadFile(filepath.Join(r.state, "log"))
	return string(b)
}

func (r *rig) resetLog() { _ = os.Remove(filepath.Join(r.state, "log")) }

func (r *rig) setState(name, value string) {
	must(r.t, os.WriteFile(filepath.Join(r.state, name), []byte(value), 0o644))
}

func (r *rig) installedVersion() string {
	out, _ := r.shell(`"$AIRVIEW_DESTDIR/opt/airview/airview" -version`)
	f := strings.Fields(out)
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

// install performs a fresh install of version v with args.
func (r *rig) install(v string, args ...string) string {
	r.t.Helper()
	b := r.bundle(v)
	out, code := r.run(filepath.Join(b, "install.sh"), args...)
	if code != 0 {
		r.t.Fatalf("install.sh exit %d:\n%s", code, out)
	}
	return out
}

// archiveMembers lists a .tar.gz.
func archiveMembers(t *testing.T, file string) map[string]string {
	t.Helper()
	f, err := os.Open(file)
	must(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	must(t, err)
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		must(t, err)
		b, _ := io.ReadAll(tr)
		out[strings.TrimSuffix(h.Name, "/")] = string(b)
	}
	return out
}

func newestBackup(t *testing.T, dir string) string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "airview-backup-*.tar.gz"))
	if len(m) == 0 {
		t.Fatalf("no backup in %s", dir)
	}
	slices.Sort(m)
	return m[len(m)-1]
}

// ---------------------------------------------------------------------------
// Static checks
// ---------------------------------------------------------------------------

func TestScriptsSyntax(t *testing.T) {
	r := newRig(t)
	files, _ := filepath.Glob("*.sh")
	if len(files) < 7 {
		t.Fatalf("expected the server scripts, found %v", files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		must(t, err)
		if bytes.Contains(b, []byte("\r\n")) {
			t.Errorf("%s has CRLF line endings", f)
		}
		if !bytes.HasPrefix(b, []byte("#!/bin/sh\n")) && f != "lib.sh" {
			t.Errorf("%s must start with #!/bin/sh", f)
		}
		// Bash in POSIX mode parses the script without running it.
		if out, code := r.shell("bash --posix -n " + posix(mustAbs(t, f)) + " 2>&1 || sh -n " + posix(mustAbs(t, f))); code != 0 {
			t.Errorf("%s: syntax error:\n%s", f, out)
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	a, err := filepath.Abs(p)
	must(t, err)
	return a
}

// unitDirectives parses a systemd unit into "Section.Key" -> values.
func unitDirectives(t *testing.T, file string) map[string][]string {
	f, err := os.Open(file)
	must(t, err)
	defer f.Close()
	out := map[string][]string{}
	sec := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		if strings.HasPrefix(l, "[") {
			sec = strings.Trim(l, "[]")
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			t.Fatalf("%s: bad line %q", file, l)
		}
		out[sec+"."+k] = append(out[sec+"."+k], v)
	}
	return out
}

func TestUnitFile(t *testing.T) {
	u := unitDirectives(t, "airview.service")
	want := map[string]string{
		"Service.User":                  "airview",
		"Service.Group":                 "airview",
		"Service.ExecStart":             "/opt/airview/airview -headless -data /var/lib/airview",
		"Service.ReadWritePaths":        "/var/lib/airview",
		"Service.NoNewPrivileges":       "true",
		"Service.ProtectSystem":         "strict",
		"Service.ProtectHome":           "true",
		"Service.PrivateTmp":            "true",
		"Service.AmbientCapabilities":   "CAP_NET_BIND_SERVICE",
		"Service.CapabilityBoundingSet": "CAP_NET_BIND_SERVICE",
		"Service.Restart":               "always",
		"Service.SyslogIdentifier":      "airview",
		"Service.LimitNOFILE":           "65536",
		"Install.WantedBy":              "multi-user.target",
	}
	for k, v := range want {
		if got := u[k]; len(got) != 1 || got[0] != v {
			t.Errorf("%s = %v, want %q", k, got, v)
		}
	}
	env := strings.Join(u["Service.Environment"], " ")
	for _, e := range []string{"AIRVIEW_CONFIG=/etc/airview/airview.jsonc", "AIRVIEW_DATA=/var/lib/airview", "GOMEMLIMIT=off",
		"AIRVIEW_PROCESS_JOURNAL_LEVEL=info"} {
		if !strings.Contains(env, e) {
			t.Errorf("Environment lacks %s", e)
		}
	}
	// The wrapper, the lib and the unit agree on the paths.
	lib, _ := os.ReadFile("lib.sh")
	for _, p := range []string{"/opt/airview", "/etc/airview/airview.jsonc", "/var/lib/airview", "/etc/systemd/system/airview.service", "/etc/logrotate.d/airview"} {
		if !bytes.Contains(lib, []byte(p)) {
			t.Errorf("lib.sh does not use %s", p)
		}
	}
	lr, _ := os.ReadFile("airview.logrotate")
	for _, s := range []string{"/var/lib/airview/airview.log {", "su airview airview", "copytruncate", "dateext"} {
		if !bytes.Contains(lr, []byte(s)) {
			t.Errorf("airview.logrotate lacks %q", s)
		}
	}
}

// TestDockerContext checks that the build stage of the Dockerfile copies
// every directory of this module that the two binaries are built from.
// (Docker itself is not needed for this check.)
func TestDockerContext(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	must(t, err)
	df, err := os.ReadFile(filepath.Join(repo, "Dockerfile"))
	must(t, err)
	var copied []string
	for _, l := range strings.Split(string(df), "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == "COPY" && !strings.HasPrefix(f[1], "--") {
			for _, src := range f[1 : len(f)-1] {
				copied = append(copied, filepath.Join(repo, filepath.FromSlash(src)))
			}
		}
	}
	for _, need := range []string{"go.mod", "go.sum"} {
		if !slices.Contains(copied, filepath.Join(repo, need)) {
			t.Errorf("Dockerfile does not copy %s", need)
		}
	}
	for _, c := range copied {
		if _, err := os.Stat(c); err != nil {
			t.Errorf("Dockerfile copies %s, which does not exist", c)
		}
	}
	for _, pkg := range []string{"./cmd/airview", "./deploy/server/healthcheck"} {
		if !strings.Contains(string(df), strings.TrimPrefix(pkg, "./")) {
			t.Errorf("Dockerfile does not build %s", pkg)
		}
	}
	cmd := exec.Command("go", "list", "-e", "-deps", "-f", "{{if .Module}}{{if .Module.Main}}{{.Dir}}{{end}}{{end}}",
		"./cmd/airview", "./deploy/server/healthcheck")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	n := 0
	for _, dir := range strings.Fields(string(out)) {
		n++
		ok := false
		for _, c := range copied {
			if dir == c || strings.HasPrefix(dir, c+string(filepath.Separator)) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("package directory %s is not in the Docker build context", dir)
		}
	}
	if n < 5 {
		t.Fatalf("go list found only %d module packages", n)
	}
}

// ---------------------------------------------------------------------------
// install / upgrade / rollback
// ---------------------------------------------------------------------------

func TestInstallFresh(t *testing.T) {
	r := newRig(t)
	out := r.install("9.9.9", "--name", "Test Net", "--public-host", "feed.example.org", "--proxy")

	for _, f := range []string{
		"opt/airview/airview", "opt/airview/lib.sh", "opt/airview/backup.sh", "opt/airview/restore.sh",
		"opt/airview/upgrade.sh", "opt/airview/uninstall.sh", "opt/airview/install.sh", "opt/airview/avf-cert-sync.sh",
		"opt/airview/README.md", "opt/airview/FIREWALL.md", "opt/airview/proxy/Caddyfile", "opt/airview/proxy/nginx.conf",
		"etc/systemd/system/airview.service", "etc/logrotate.d/airview", "usr/local/bin/airview",
		"etc/airview/airview.example.jsonc", "etc/airview/airview.jsonc", "var/lib/airview", "var/backups/airview",
	} {
		if !r.exists(f) {
			t.Errorf("missing %s", f)
		}
	}
	if r.exists("opt/airview/airview.new") {
		t.Error("airview.new left behind")
	}
	if v := r.installedVersion(); v != "9.9.9" {
		t.Errorf("installed version %q", v)
	}
	log := r.log()
	for _, s := range []string{"useradd --system --user-group", "systemctl daemon-reload", "systemctl enable airview", "systemctl restart airview", "/healthz"} {
		if !strings.Contains(log, s) {
			t.Errorf("log lacks %q:\n%s", s, log)
		}
	}
	if !strings.Contains(out, "airview admin init") {
		t.Errorf("summary does not explain the admin bootstrap:\n%s", out)
	}
	// config check ran with the service's paths.
	env, _ := os.ReadFile(filepath.Join(r.state, "check_env"))
	if want := posix(r.path("etc/airview/airview.jsonc")) + "|" + posix(r.path("var/lib/airview")); strings.TrimSpace(string(env)) != want {
		t.Errorf("config check env %q, want %q", env, want)
	}

	// The generated operator file is valid for the real configuration loader.
	opFile := r.path("etc/airview/airview.jsonc")
	data := t.TempDir()
	rep := config.Check(config.LoadOptions{Dir: data, File: opFile, ReadOnly: true})
	if rep.Errors() != 0 || rep.Warnings() != 0 {
		t.Fatalf("operator file issues: %+v\n%s", rep.Issues, r.read("etc/airview/airview.jsonc"))
	}
	cfg, _, _, err := config.LoadLayered(config.LoadOptions{Dir: data, File: opFile, ReadOnly: true})
	must(t, err)
	if cfg.Mode != model.ModeAggregator || cfg.HTTP.Port != 8080 || cfg.HTTP.BindAddr != "127.0.0.1" {
		t.Errorf("mode/http = %q %d %q", cfg.Mode, cfg.HTTP.Port, cfg.HTTP.BindAddr)
	}
	a := cfg.Aggregator
	if cfg.Storage.Profile != model.StorageProfileServer {
		t.Errorf("storage.profile = %q, want server", cfg.Storage.Profile)
	}
	if a.Name != "Test Net" || a.PublicHost != "feed.example.org" || !slices.Equal(a.TrustedProxies, []string{"127.0.0.1/32", "::1/128"}) ||
		a.AVFPort != 30160 || a.BeastPort != 30004 || a.MLATPort != 31090 || a.Admission != "invite" {
		t.Errorf("aggregator block = %+v", a)
	}
}

func TestInstallDefaultsAndValidation(t *testing.T) {
	r := newRig(t)
	r.install("9.9.9", "--mode", "standalone")
	opFile := r.path("etc/airview/airview.jsonc")
	cfg, _, _, err := config.LoadLayered(config.LoadOptions{Dir: t.TempDir(), File: opFile, ReadOnly: true})
	must(t, err)
	if cfg.Mode != model.ModeStandalone || cfg.HTTP.Port != 80 || cfg.HTTP.BindAddr != "" || len(cfg.Aggregator.TrustedProxies) != 0 {
		t.Errorf("standalone defaults: mode %q port %d bind %q proxies %v", cfg.Mode, cfg.HTTP.Port, cfg.HTTP.BindAddr, cfg.Aggregator.TrustedProxies)
	}
	if cfg.Storage.Profile != "" {
		t.Errorf("standalone server install pins storage.profile %q (want automatic)", cfg.Storage.Profile)
	}

	for _, args := range [][]string{
		{"--mode", "feeder"},
		{"--name", `bad "name"`},
		{"--public-host", "https://x/y"},
		{"--http-port", "70000"},
		{"--bogus"},
	} {
		r2 := newRig(t)
		b := r2.bundle("9.9.9")
		if out, code := r2.run(filepath.Join(b, "install.sh"), args...); code == 0 {
			t.Errorf("install.sh %v succeeded:\n%s", args, out)
		}
		if r2.exists("opt/airview/airview") {
			t.Errorf("install.sh %v installed despite the error", args)
		}
	}

	// A rejected configuration stops a fresh install before the service starts.
	r3 := newRig(t)
	r3.setState("check_rc", "1")
	b := r3.bundle("9.9.9")
	if out, code := r3.run(filepath.Join(b, "install.sh")); code == 0 || strings.Contains(r3.log(), "restart airview") {
		t.Errorf("install with an invalid config: exit %d, log:\n%s\n%s", code, r3.log(), out)
	}
}

func TestUpgradeKeepsConfigAndBacksUp(t *testing.T) {
	r := newRig(t)
	r.needGNUTar()
	r.install("9.9.9", "--proxy")
	op := r.read("etc/airview/airview.jsonc") + "// operator edit\n"
	must(t, os.WriteFile(r.path("etc/airview/airview.jsonc"), []byte(op), 0o640))
	must(t, os.WriteFile(r.path("var/lib/airview/airview.db"), []byte("db-v1"), 0o640))
	r.resetLog()

	b := r.bundle("9.9.10")
	out, code := r.run(filepath.Join(b, "upgrade.sh"), "--name", "ignored")
	if code != 0 {
		t.Fatalf("upgrade exit %d:\n%s", code, out)
	}
	if v := r.installedVersion(); v != "9.9.10" {
		t.Errorf("version after upgrade %q", v)
	}
	if !strings.Contains(r.read("opt/airview/airview.prev"), "V=9.9.9") {
		t.Error("airview.prev is not the previous binary")
	}
	if r.read("etc/airview/airview.jsonc") != op {
		t.Error("the operator file was changed by the upgrade")
	}
	if !strings.Contains(r.read("etc/airview/airview.example.jsonc"), "9.9.10") {
		t.Error("the example config was not refreshed")
	}
	if !strings.Contains(out, "ignored on an upgrade: --name") {
		t.Errorf("first-install option not reported as ignored:\n%s", out)
	}
	bk := newestBackup(t, r.path("var/backups/airview"))
	if !strings.Contains(filepath.Base(bk), "pre-upgrade-9.9.9") {
		t.Errorf("backup name %s", bk)
	}
	if m := archiveMembers(t, bk); m["airview-backup/data/airview.db"] != "db-v1" {
		t.Errorf("pre-upgrade backup lacks the database: %v", keys(m))
	}
	log := r.log()
	if i, j := strings.Index(log, "systemctl stop airview"), strings.Index(log, "systemctl start airview"); i < 0 || j < i {
		t.Errorf("expected stop before start:\n%s", log)
	}
}

func TestUpgradeRejectedConfigChangesNothing(t *testing.T) {
	r := newRig(t)
	r.install("9.9.9")
	r.setState("check_rc", "1")
	r.resetLog()
	b := r.bundle("9.9.10")
	out, code := r.run(filepath.Join(b, "upgrade.sh"), "--no-backup")
	if code == 0 {
		t.Fatalf("upgrade succeeded despite the rejected config:\n%s", out)
	}
	if v := r.installedVersion(); v != "9.9.9" {
		t.Errorf("version %q after a refused upgrade", v)
	}
	if r.exists("opt/airview/airview.new") {
		t.Error("airview.new left behind")
	}
	if strings.Contains(r.log(), "systemctl stop") {
		t.Errorf("service stopped although nothing was changed:\n%s", r.log())
	}
	// --force goes ahead.
	if out, code := r.run(filepath.Join(b, "upgrade.sh"), "--no-backup", "--force"); code != 0 {
		t.Fatalf("upgrade --force: exit %d\n%s", code, out)
	}
	if v := r.installedVersion(); v != "9.9.10" {
		t.Errorf("version %q after --force", v)
	}
}

func TestUpgradeRollsBackUnhealthyVersion(t *testing.T) {
	r := newRig(t)
	r.install("9.9.9")
	r.setState("bad_version", "9.9.10") // /healthz fails while 9.9.10 is installed
	b := r.bundle("9.9.10")
	out, code := r.run(filepath.Join(b, "upgrade.sh"), "--no-backup")
	if code == 0 {
		t.Fatalf("unhealthy upgrade reported success:\n%s", out)
	}
	if v := r.installedVersion(); v != "9.9.9" {
		t.Errorf("not rolled back: version %q\n%s", v, out)
	}
	if !strings.Contains(out, "rolled back") {
		t.Errorf("no rollback message:\n%s", out)
	}

	// --no-rollback keeps the new version.
	out, code = r.run(filepath.Join(b, "upgrade.sh"), "--no-backup", "--no-rollback")
	if code == 0 || r.installedVersion() != "9.9.10" {
		t.Errorf("--no-rollback: exit %d version %q\n%s", code, r.installedVersion(), out)
	}
}

func TestInstalledCopyRefusesSelfUpgrade(t *testing.T) {
	r := newRig(t)
	r.install("9.9.9")
	out, code := r.run(r.path("opt/airview/upgrade.sh"))
	if code == 0 || !strings.Contains(out, "installed copy") {
		t.Errorf("upgrade from /opt/airview: exit %d\n%s", code, out)
	}
}

// ---------------------------------------------------------------------------
// one-line install (download + checksum)
// ---------------------------------------------------------------------------

// tarGz packs dir (its base name becomes the top directory).
func tarGz(t *testing.T, dir, out string) {
	t.Helper()
	f, err := os.Create(out)
	must(t, err)
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	parent := filepath.Dir(dir)
	must(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(parent, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Mode: 0o755, Typeflag: tar.TypeDir})
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if strings.HasSuffix(rel, ".sh") || strings.HasSuffix(rel, "/airview") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: mode, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	}))
	must(t, tw.Close())
	must(t, zw.Close())
	must(t, f.Close())
}

func TestOneLineInstall(t *testing.T) {
	r := newRig(t)
	r.needGNUTar()
	serve := filepath.Join(r.state, "serve")
	tgz := filepath.Join(serve, "airview-server-linux-amd64.tar.gz")
	tarGz(t, r.bundle("9.9.9"), tgz)
	b, err := os.ReadFile(tgz)
	must(t, err)
	sum := sha256.Sum256(b)
	good := hex.EncodeToString(sum[:]) + "  airview-server-linux-amd64.tar.gz\n" +
		strings.Repeat("0", 64) + "  airview-server-linux-arm64.tar.gz\n"
	script, err := os.ReadFile("install.sh")
	must(t, err)

	pipe := func(args ...string) (string, int) {
		cmd := exec.Command(r.sh, append([]string{"-s", "--"}, args...)...)
		return r.exec(cmd, bytes.NewReader(script))
	}

	// Checksum mismatch: nothing is installed.
	must(t, os.WriteFile(filepath.Join(serve, "SHA256SUMS"), []byte(strings.Repeat("1", 64)+"  airview-server-linux-amd64.tar.gz\n"), 0o644))
	if out, code := pipe("--base-url", "https://releases.example.invalid/airview"); code == 0 || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("mismatch: exit %d\n%s", code, out)
	}
	if r.exists("opt/airview/airview") {
		t.Fatal("installed despite a checksum mismatch")
	}
	// Not listed.
	must(t, os.WriteFile(filepath.Join(serve, "SHA256SUMS"), []byte(strings.Repeat("1", 64)+"  other.tar.gz\n"), 0o644))
	if out, code := pipe("--base-url", "https://releases.example.invalid/airview"); code == 0 || !strings.Contains(out, "not listed") {
		t.Fatalf("unlisted: exit %d\n%s", code, out)
	}

	must(t, os.WriteFile(filepath.Join(serve, "SHA256SUMS"), []byte(good), 0o644))
	out, code := pipe("--base-url", "https://releases.example.invalid/airview/", "--name", "Piped Net", "--proxy")
	if code != 0 {
		t.Fatalf("one-line install: exit %d\n%s", code, out)
	}
	if r.installedVersion() != "9.9.9" || !strings.Contains(r.read("etc/airview/airview.jsonc"), `"name": "Piped Net"`) ||
		!strings.Contains(r.read("etc/airview/airview.jsonc"), `"bindAddr": "127.0.0.1"`) {
		t.Errorf("one-line install result:\n%s\n%s", out, r.read("etc/airview/airview.jsonc"))
	}
	if !strings.Contains(r.log(), "https://releases.example.invalid/airview/airview-server-linux-amd64.tar.gz") {
		t.Errorf("download URL not as expected:\n%s", r.log())
	}
}

// ---------------------------------------------------------------------------
// backup / restore / uninstall
// ---------------------------------------------------------------------------

func seedData(t *testing.T, r *rig) {
	files := map[string]string{
		"var/lib/airview/airview.db":        "db1",
		"var/lib/airview/airview.db-wal":    "wal1",
		"var/lib/airview/stations.db":       "st1",
		"var/lib/airview/config.json":       `{"mode":"aggregator"}`,
		"var/lib/airview/avf-tls/key.pem":   "KEY",
		"var/lib/airview/uplinks/spool/s1":  "spool",
		"var/lib/airview/tilecache/a/1.png": "tile",
		"var/lib/airview/terrain/x.bin":     "dem",
		"var/lib/airview/airview.log":       "log",
		"var/lib/airview/airview.log.1":     "log1",
		"etc/airview/tls/avf.crt":           "CERT",
	}
	for rel, body := range files {
		p := r.path(rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o640))
	}
}

func keys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	slices.Sort(k)
	return k
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	r := newRig(t)
	r.needGNUTar()
	r.install("9.9.9")
	seedData(t, r)
	opFile := r.read("etc/airview/airview.jsonc")
	r.resetLog()

	out, code := r.run(r.path("opt/airview/backup.sh"), "--tag", "t1")
	if code != 0 {
		t.Fatalf("backup exit %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	archive := strings.TrimSpace(lines[len(lines)-1])
	if !strings.Contains(archive, "airview-backup-testhost-") || !strings.HasSuffix(archive, "-t1.tar.gz") {
		t.Fatalf("archive path %q", archive)
	}
	m := archiveMembers(t, newestBackup(t, r.path("var/backups/airview")))
	want := map[string]string{
		"airview-backup/data/airview.db":       "db1",
		"airview-backup/data/airview.db-wal":   "wal1",
		"airview-backup/data/stations.db":      "st1",
		"airview-backup/data/avf-tls/key.pem":  "KEY",
		"airview-backup/data/uplinks/spool/s1": "spool",
		"airview-backup/etc/airview.jsonc":     opFile,
		"airview-backup/etc/tls/avf.crt":       "CERT",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("archive %s = %q, want %q", k, m[k], v)
		}
	}
	for _, k := range []string{"airview-backup/data/tilecache", "airview-backup/data/terrain", "airview-backup/data/airview.log", "airview-backup/data/airview.log.1"} {
		if _, ok := m[k]; ok {
			t.Errorf("archive should not contain %s", k)
		}
	}
	if !strings.Contains(m["airview-backup/MANIFEST"], "version=9.9.9") || !strings.Contains(m["airview-backup/MANIFEST"], "method=cold") {
		t.Errorf("MANIFEST:\n%s", m["airview-backup/MANIFEST"])
	}
	log := r.log()
	if i, j := strings.Index(log, "systemctl stop airview"), strings.Index(log, "systemctl start airview"); i < 0 || j < i {
		t.Errorf("cold backup must stop and restart the service:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(r.state, "active")); err != nil {
		t.Error("service not running after the backup")
	}

	// Change the live data, then restore.
	must(t, os.WriteFile(r.path("var/lib/airview/airview.db"), []byte("changed"), 0o640))
	must(t, os.WriteFile(r.path("etc/airview/airview.jsonc"), []byte("{}"), 0o640))
	out, code = r.run(r.path("opt/airview/restore.sh"), newestBackup(t, r.path("var/backups/airview")), "--yes")
	if code != 0 {
		t.Fatalf("restore exit %d\n%s", code, out)
	}
	if got := r.read("var/lib/airview/airview.db"); got != "db1" {
		t.Errorf("restored db = %q", got)
	}
	if got := r.read("var/lib/airview/avf-tls/key.pem"); got != "KEY" {
		t.Errorf("restored key = %q", got)
	}
	if got := r.read("etc/airview/airview.jsonc"); got != opFile {
		t.Errorf("restored operator file = %q", got)
	}
	if !strings.Contains(r.read("etc/airview/airview.example.jsonc"), "9.9.9") {
		t.Error("example config not regenerated after restore")
	}
	pre, _ := filepath.Glob(r.path("var/lib/airview.pre-restore-*"))
	if len(pre) != 1 {
		t.Fatalf("pre-restore copies: %v", pre)
	}
	if b, _ := os.ReadFile(filepath.Join(pre[0], "airview.db")); string(b) != "changed" {
		t.Errorf("pre-restore copy holds %q", b)
	}
	if st, _ := filepath.Glob(r.path("var/lib/airview.restore-*")); len(st) != 0 {
		t.Errorf("staging directory left behind: %v", st)
	}

	// --keep-config leaves /etc/airview alone.
	must(t, os.WriteFile(r.path("etc/airview/airview.jsonc"), []byte("{} // mine"), 0o640))
	if out, code := r.run(r.path("opt/airview/restore.sh"), newestBackup(t, r.path("var/backups/airview")), "--yes", "--keep-config"); code != 0 {
		t.Fatalf("restore --keep-config exit %d\n%s", code, out)
	}
	if r.read("etc/airview/airview.jsonc") != "{} // mine" {
		t.Error("--keep-config replaced the configuration")
	}
}

func TestBackupOnlineAndKeep(t *testing.T) {
	r := newRig(t)
	r.needGNUTar()
	r.install("9.9.9")
	seedData(t, r)
	r.resetLog()
	for i, tag := range []string{"a", "b", "c"} {
		out, code := r.run(r.path("opt/airview/backup.sh"), "--online", "--keep", "2", "--tag", tag, "--include-logs", "-q")
		if code != 0 {
			t.Fatalf("online backup %d exit %d\n%s", i, code, out)
		}
		if n := len(strings.Split(strings.TrimSpace(out), "\n")); n != 1 {
			t.Errorf("--quiet printed %d lines:\n%s", n, out)
		}
	}
	all, _ := filepath.Glob(r.path("var/backups/airview/airview-backup-*.tar.gz"))
	if len(all) != 2 {
		t.Errorf("--keep 2 left %d archives", len(all))
	}
	if strings.Contains(r.log(), "systemctl stop") {
		t.Errorf("online backup stopped the service:\n%s", r.log())
	}
	m := archiveMembers(t, all[0])
	if m["airview-backup/data/airview.db"] != "snap:db1" || m["airview-backup/data/stations.db"] != "snap:st1" {
		t.Errorf("databases not from the online snapshot: %q %q", m["airview-backup/data/airview.db"], m["airview-backup/data/stations.db"])
	}
	if _, ok := m["airview-backup/data/airview.db-wal"]; ok {
		t.Error("online backup contains the WAL file")
	}
	if m["airview-backup/data/airview.log"] != "log" || !strings.Contains(m["airview-backup/MANIFEST"], "method=online") {
		t.Errorf("--include-logs / MANIFEST: %v", keys(m))
	}

	// A binary with "airview db snapshot" is preferred over sqlite3.
	r.setState("has_snapshot", "")
	out, code := r.run(r.path("opt/airview/backup.sh"), "--online", "--tag", "builtin", "-q")
	if code != 0 {
		t.Fatalf("online backup (builtin) exit %d\n%s", code, out)
	}
	m = archiveMembers(t, strings.TrimSpace(strings.ReplaceAll(posixToNative(strings.TrimSpace(out)), "\r", "")))
	if m["airview-backup/data/airview.db"] != "vac:db1" || m["airview-backup/data/stations.db"] != "vac:st1" {
		t.Errorf("databases not from airview db snapshot: %q %q", m["airview-backup/data/airview.db"], m["airview-backup/data/stations.db"])
	}
}

// posixToNative undoes posix() for paths printed by the scripts.
func posixToNative(p string) string {
	if runtime.GOOS != "windows" || len(p) < 3 || p[0] != '/' || p[2] != '/' {
		return p
	}
	return strings.ToUpper(p[1:2]) + ":" + filepath.FromSlash(p[2:])
}

func TestRestoreRejectsForeignArchives(t *testing.T) {
	r := newRig(t)
	r.needGNUTar()
	r.install("9.9.9")
	seedData(t, r)

	mk := func(name string, members map[string]string) string {
		p := filepath.Join(r.tmp, name)
		f, err := os.Create(p)
		must(t, err)
		zw := gzip.NewWriter(f)
		tw := tar.NewWriter(zw)
		for _, n := range keys(members) {
			must(t, tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(members[n])), Typeflag: tar.TypeReg}))
			_, _ = tw.Write([]byte(members[n]))
		}
		must(t, tw.Close())
		must(t, zw.Close())
		must(t, f.Close())
		return p
	}
	cases := map[string]string{
		mk("evil.tar.gz", map[string]string{"airview-backup/MANIFEST": "format=1", "airview-backup/data/x": "x", "airview-backup/../../etc/passwd": "p"}): "unexpected member",
		mk("other.tar.gz", map[string]string{"something/else": "x"}):                                                                                      "unexpected member",
		mk("nomanifest.tar.gz", map[string]string{"airview-backup/data/airview.db": "x"}):                                                                 "no MANIFEST",
		mk("nodata.tar.gz", map[string]string{"airview-backup/MANIFEST": "format=1"}):                                                                     "no data directory",
	}
	for file, msg := range cases {
		out, code := r.run(r.path("opt/airview/restore.sh"), file, "--yes")
		if code == 0 || !strings.Contains(out, msg) {
			t.Errorf("%s: exit %d, want %q in:\n%s", filepath.Base(file), code, msg, out)
		}
	}
	if r.read("var/lib/airview/airview.db") != "db1" {
		t.Error("a rejected restore changed the data")
	}
	// Without --yes and without a terminal, restore refuses.
	out, code := r.run(r.path("opt/airview/restore.sh"), mk("ok.tar.gz", map[string]string{"airview-backup/MANIFEST": "format=1", "airview-backup/data/airview.db": "x"}))
	if code == 0 || !strings.Contains(out, "--yes") {
		t.Errorf("unconfirmed restore: exit %d\n%s", code, out)
	}
}

func TestUninstall(t *testing.T) {
	r := newRig(t)
	r.install("9.9.9")
	seedData(t, r)
	if out, code := r.run(r.path("opt/airview/uninstall.sh")); code != 0 {
		t.Fatalf("uninstall exit %d\n%s", code, out)
	}
	for _, gone := range []string{"opt/airview", "etc/systemd/system/airview.service", "etc/logrotate.d/airview", "usr/local/bin/airview"} {
		if r.exists(gone) {
			t.Errorf("%s still exists", gone)
		}
	}
	for _, kept := range []string{"var/lib/airview/airview.db", "etc/airview/airview.jsonc"} {
		if !r.exists(kept) {
			t.Errorf("%s was removed without --purge", kept)
		}
	}
	// Reinstall picks up the kept configuration and data.
	r.install("9.9.9")
	if r.read("var/lib/airview/airview.db") != "db1" {
		t.Error("reinstall lost the data")
	}
	// --purge needs confirmation when not on a terminal.
	if _, code := r.run(r.path("opt/airview/uninstall.sh"), "--purge"); code == 0 || !r.exists("var/lib/airview/airview.db") {
		t.Error("--purge without --yes deleted data")
	}
	if out, code := r.run(r.path("opt/airview/uninstall.sh"), "--purge", "--yes"); code != 0 {
		t.Fatalf("purge exit %d\n%s", code, out)
	}
	for _, gone := range []string{"var/lib/airview", "etc/airview", "opt/airview"} {
		if r.exists(gone) {
			t.Errorf("%s still exists after --purge", gone)
		}
	}
	if !strings.Contains(r.log(), "userdel airview") {
		t.Error("--purge did not remove the account")
	}
	if !r.exists("var/backups/airview") {
		t.Error("--purge removed the backups")
	}
}

// SEC-07: a truncated `curl | sh` download must run nothing: all top-level
// code is inside main(), called on the last line.
func TestInstallerRunsFromMain(t *testing.T) {
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if lines[len(lines)-1] != `main "$@"` {
		t.Fatalf("last line %q", lines[len(lines)-1])
	}
	inFunc := false
	for i, ln := range lines[:len(lines)-1] {
		switch {
		case strings.HasSuffix(ln, "() {"):
			inFunc = true
		case ln == "}":
			inFunc = false
		case inFunc, ln == "", strings.HasPrefix(ln, "#"), ln == "set -eu":
		default:
			t.Errorf("line %d runs at top level: %q", i+1, ln)
		}
	}
}
