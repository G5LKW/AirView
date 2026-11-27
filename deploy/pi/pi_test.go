package pi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func findSh() string {
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{`C:\Program Files\Git\usr\bin\sh.exe`, `C:\Program Files\Git\bin\sh.exe`} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

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

// is-active fails while the installed binary reports the version in
// $STUB_STATE/fail_version (a release that does not start).
const stubSystemctl = `#!/bin/sh
echo "systemctl $*" >> "$STUB_STATE/log"
case "$1" in
    is-active)
        [ -f "$STUB_STATE/active" ] || exit 3
        if [ -f "$STUB_STATE/fail_version" ] &&
            "$AIRVIEW_DESTDIR/opt/airview/airview" -version | grep -q "AirView $(cat "$STUB_STATE/fail_version") "; then
            exit 3
        fi
        exit 0 ;;
    start | restart) touch "$STUB_STATE/active" ;;
    stop) rm -f "$STUB_STATE/active" ;;
esac
exit 0
`

const stubCurl = `#!/bin/sh
echo "curl $*" >> "$STUB_STATE/log"
out=""; url=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        --retry) shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
f="$STUB_STATE/serve/${url##*/}"
[ -f "$f" ] || exit 22
cp "$f" "$out"
`

var stubs = map[string]string{
	"systemctl":  stubSystemctl,
	"curl":       stubCurl,
	"useradd":    "#!/bin/sh\necho \"useradd $*\" >> \"$STUB_STATE/log\"\ntouch \"$STUB_STATE/user\"\n",
	"userdel":    "#!/bin/sh\necho \"userdel $*\" >> \"$STUB_STATE/log\"\nrm -f \"$STUB_STATE/user\"\n",
	"id":         "#!/bin/sh\ncase \"$1\" in -u) echo 0 ;; airview) [ -f \"$STUB_STATE/user\" ] ;; *) exit 1 ;; esac\n",
	"chown":      "#!/bin/sh\necho \"chown $*\" >> \"$STUB_STATE/log\"\n",
	"journalctl": "#!/bin/sh\necho 'fake journal'\n",
	"hostname":   "#!/bin/sh\necho 192.0.2.10\n",
	"sleep":      "#!/bin/sh\nexit 0\n",
}

const fakeAirview = `#!/bin/sh
V=__VERSION__
case "$1" in
    -version) echo "AirView $V (linux/arm64, go1.27.3)" ;;
    config)
        case "$2" in
            example) echo "// example $V" > "$4" ;;
            check) echo "$AIRVIEW_CONFIG" > "$STUB_STATE/check_env" ;;
        esac ;;
    *) exit 2 ;;
esac
`

type rig struct {
	t                      *testing.T
	sh, root, state, stubs string
	tmp                    string
}

func newRig(t *testing.T) *rig {
	sh := findSh()
	if sh == "" {
		t.Skip("no POSIX sh available")
	}
	base := t.TempDir()
	r := &rig{t: t, sh: sh, root: filepath.Join(base, "root"), state: filepath.Join(base, "state"),
		stubs: filepath.Join(base, "stubs"), tmp: filepath.Join(base, "tmp")}
	for _, d := range []string{r.root, r.state, r.stubs, r.tmp, filepath.Join(r.state, "serve")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	for n, b := range stubs {
		must(t, os.WriteFile(filepath.Join(r.stubs, n), []byte(b), 0o755))
	}
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (r *rig) run(script string, args ...string) (string, int) {
	r.t.Helper()
	cmd := exec.Command(r.sh, append([]string{posix(script)}, args...)...)
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AIRVIEW_") || strings.HasPrefix(strings.ToUpper(kv), "PATH=") || strings.HasPrefix(kv, "TMPDIR=") {
			continue
		}
		env = append(env, kv)
	}
	path := r.stubs + string(os.PathListSeparator) + os.Getenv("PATH")
	if runtime.GOOS == "windows" {
		path += string(os.PathListSeparator) + filepath.Dir(r.sh)
	}
	cmd.Env = append(env, "PATH="+path, "AIRVIEW_DESTDIR="+posix(r.root), "AIRVIEW_ARCH=aarch64",
		"STUB_STATE="+posix(r.state), "TMPDIR="+posix(r.tmp))
	cmd.Dir = r.tmp
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			r.t.Fatalf("run %v: %v", cmd.Args, err)
		}
		return b.String(), ee.ExitCode()
	}
	return b.String(), 0
}

var bundleFiles = []string{"install.sh", "update.sh", "uninstall.sh", "airview.service", "README.md"}

// files of an unpacked bundle of version v (LF line ends as the release build writes them).
func bundleContent(t *testing.T, v string) map[string][]byte {
	m := map[string][]byte{}
	for _, f := range bundleFiles {
		b, err := os.ReadFile(f)
		must(t, err)
		m[f] = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	}
	fake := []byte(strings.ReplaceAll(fakeAirview, "__VERSION__", v))
	m["airview-linux-arm64"] = fake
	m["airview-linux-armv7"] = fake
	m["VERSION"] = []byte(v + "\n")
	return m
}

func (r *rig) bundle(v string) string {
	dir := filepath.Join(r.t.TempDir(), "airview-pi-"+v)
	must(r.t, os.MkdirAll(dir, 0o755))
	for n, b := range bundleContent(r.t, v) {
		must(r.t, os.WriteFile(filepath.Join(dir, n), b, 0o755))
	}
	return dir
}

// serve publishes a release site with airview-pi-<v>.tar.gz and SHA256SUMS.
func (r *rig) serve(v string, corrupt bool) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := "airview-pi-" + v
	must(r.t, tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755}))
	for n, b := range bundleContent(r.t, v) {
		must(r.t, tw.WriteHeader(&tar.Header{Name: top + "/" + n, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(b))}))
		_, err := tw.Write(b)
		must(r.t, err)
	}
	must(r.t, tw.Close())
	must(r.t, gz.Close())
	name := top + ".tar.gz"
	sum := sha256.Sum256(buf.Bytes())
	sums := strings.Repeat("0", 64) + "  airview-server-linux-amd64.tar.gz\n" + hex.EncodeToString(sum[:]) + "  " + name + "\n"
	body := buf.Bytes()
	if corrupt {
		body = append(append([]byte{}, body...), 0)
	}
	must(r.t, os.WriteFile(filepath.Join(r.state, "serve", name), body, 0o644))
	must(r.t, os.WriteFile(filepath.Join(r.state, "serve", "SHA256SUMS"), []byte(sums), 0o644))
}

func (r *rig) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(rel)))
	if err != nil {
		r.t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func (r *rig) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(r.root, filepath.FromSlash(rel)))
	return err == nil
}

func (r *rig) log() string {
	b, _ := os.ReadFile(filepath.Join(r.state, "log"))
	return string(b)
}

func (r *rig) installedVersion() string {
	return strings.TrimSpace(r.read("opt/airview/VERSION"))
}

func TestInstallFreshThenIdempotentAndUpgrade(t *testing.T) {
	r := newRig(t)
	out, code := r.run(filepath.Join(r.bundle("1.0.0"), "install.sh"))
	if code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if !strings.Contains(out, "Installing AirView 1.0.0") || !strings.Contains(out, "AirView 1.0.0 is running") {
		t.Fatalf("output:\n%s", out)
	}
	if r.installedVersion() != "1.0.0" || !strings.Contains(r.read("opt/airview/airview"), "V=1.0.0") {
		t.Fatal("binary/VERSION not installed")
	}
	for _, f := range []string{"opt/airview/update.sh", "opt/airview/uninstall.sh", "opt/airview/install.sh",
		"etc/systemd/system/airview.service", "etc/airview/airview.jsonc", "etc/airview/airview.example.jsonc"} {
		if !r.exists(f) {
			t.Errorf("%s missing", f)
		}
	}
	if r.exists("opt/airview/airview.prev") {
		t.Error("airview.prev on a first install")
	}
	if c := strings.Count(r.log(), "useradd"); c != 1 {
		t.Errorf("useradd ran %d times", c)
	}
	// The operator edits the configuration; a reinstall keeps it.
	must(t, os.WriteFile(filepath.Join(r.root, "etc", "airview", "airview.jsonc"), []byte("{ \"http\": {\"port\": 8080} }\n"), 0o640))
	out, code = r.run(filepath.Join(r.bundle("1.0.0"), "install.sh"))
	if code != 0 || !strings.Contains(out, "Reinstalling AirView 1.0.0") {
		t.Fatalf("reinstall: %d\n%s", code, out)
	}
	if !strings.Contains(r.read("etc/airview/airview.jsonc"), "8080") {
		t.Error("reinstall overwrote the operator file")
	}
	if c := strings.Count(r.log(), "useradd"); c != 1 {
		t.Errorf("useradd ran %d times after a reinstall", c)
	}
	// Upgrade via update.sh from an unpacked bundle.
	out, code = r.run(filepath.Join(r.bundle("1.1.0"), "update.sh"))
	if code != 0 || !strings.Contains(out, "Upgrading AirView 1.0.0 -> 1.1.0") {
		t.Fatalf("upgrade: %d\n%s", code, out)
	}
	if r.installedVersion() != "1.1.0" || !strings.Contains(r.read("opt/airview/airview.prev"), "V=1.0.0") {
		t.Fatal("upgrade did not keep the previous binary")
	}
	if !strings.Contains(r.read("etc/airview/airview.example.jsonc"), "example 1.1.0") {
		t.Error("example configuration not refreshed")
	}
	if !strings.Contains(r.log(), "systemctl stop airview") {
		t.Error("the running service was not stopped before the binary swap")
	}
}

func TestUpgradeRollsBackWhenTheNewVersionFails(t *testing.T) {
	r := newRig(t)
	if out, code := r.run(filepath.Join(r.bundle("1.0.0"), "install.sh")); code != 0 {
		t.Fatalf("install: %s", out)
	}
	must(t, os.WriteFile(filepath.Join(r.state, "fail_version"), []byte("2.0.0"), 0o644))
	out, code := r.run(filepath.Join(r.bundle("2.0.0"), "install.sh"))
	if code == 0 {
		t.Fatalf("a failing upgrade succeeded:\n%s", out)
	}
	if !strings.Contains(out, "Rolling back") {
		t.Fatalf("no roll-back:\n%s", out)
	}
	if r.installedVersion() != "1.0.0" || !strings.Contains(r.read("opt/airview/airview"), "V=1.0.0") {
		t.Fatalf("roll-back left version %s", r.installedVersion())
	}
}

func TestUpgradeRequiresInstallation(t *testing.T) {
	r := newRig(t)
	out, code := r.run(filepath.Join(r.bundle("1.0.0"), "update.sh"))
	if code == 0 || !strings.Contains(out, "not installed") {
		t.Fatalf("update without installation: %d\n%s", code, out)
	}
}

func TestDownloadInstallAndUpdate(t *testing.T) {
	r := newRig(t)
	r.serve("1.0.0", false)
	inst := filepath.Join(r.bundle("0.0.0"), "install.sh") // only the script is used in download mode
	out, code := r.run(inst, "--base-url", "https://releases.example.invalid/airview/")
	if code != 0 {
		t.Fatalf("download install: %d\n%s", code, out)
	}
	if r.installedVersion() != "1.0.0" || !strings.Contains(out, "SHA-256 OK") {
		t.Fatalf("installed %s\n%s", r.installedVersion(), out)
	}
	if got := r.read("etc/airview/update.env"); !strings.Contains(got, "BASE_URL=https://releases.example.invalid/airview") {
		t.Fatalf("update.env: %q", got)
	}
	// Nothing new: the installed update.sh says so and downloads no bundle.
	upd := filepath.Join(r.root, "opt", "airview", "update.sh")
	out, code = r.run(upd)
	if code != 0 || !strings.Contains(out, "already installed") {
		t.Fatalf("update (same version): %d\n%s", code, out)
	}
	// A new release on the remembered site.
	r.serve("1.2.0", false)
	out, code = r.run(upd)
	if code != 0 || r.installedVersion() != "1.2.0" {
		t.Fatalf("update: %d version %s\n%s", code, r.installedVersion(), out)
	}
	// A corrupted download is refused and changes nothing.
	r.serve("1.3.0", true)
	out, code = r.run(upd)
	if code == 0 || !strings.Contains(out, "checksum mismatch") || r.installedVersion() != "1.2.0" {
		t.Fatalf("corrupt download: %d version %s\n%s", code, r.installedVersion(), out)
	}
}

func TestUpdateWithoutSite(t *testing.T) {
	r := newRig(t)
	if out, code := r.run(filepath.Join(r.bundle("1.0.0"), "install.sh")); code != 0 {
		t.Fatalf("install: %s", out)
	}
	out, code := r.run(filepath.Join(r.root, "opt", "airview", "update.sh"))
	if code != 2 || !strings.Contains(out, "no release site known") {
		t.Fatalf("update without a site: %d\n%s", code, out)
	}
}

func TestNoStartAndUninstall(t *testing.T) {
	r := newRig(t)
	out, code := r.run(filepath.Join(r.bundle("1.0.0"), "install.sh"), "--no-start")
	if code != 0 || !strings.Contains(out, "not started") || strings.Contains(r.log(), "restart") {
		t.Fatalf("--no-start: %d\n%s\n%s", code, out, r.log())
	}
	must(t, os.MkdirAll(filepath.Join(r.root, "var", "lib", "airview"), 0o755))
	must(t, os.WriteFile(filepath.Join(r.root, "var", "lib", "airview", "airview.db"), []byte("db"), 0o644))
	un := filepath.Join(r.bundle("1.0.0"), "uninstall.sh")
	if out, code := r.run(un); code != 0 {
		t.Fatalf("uninstall: %s", out)
	}
	if r.exists("opt/airview") || r.exists("etc/systemd/system/airview.service") || !r.exists("var/lib/airview/airview.db") || !r.exists("etc/airview/airview.jsonc") {
		t.Fatal("uninstall removed the wrong things")
	}
	// Again (idempotent), then with --purge.
	if out, code := r.run(un); code != 0 {
		t.Fatalf("second uninstall: %s", out)
	}
	if out, code := r.run(un, "--purge"); code != 0 {
		t.Fatalf("purge: %s", out)
	}
	if r.exists("var/lib/airview") || r.exists("etc/airview") || !strings.Contains(r.log(), "userdel airview") {
		t.Fatal("purge kept data")
	}
}

// The SD-card image runs install.sh without a VERSION file: the version
// comes from the binary.
func TestVersionFromBinaryWithoutVersionFile(t *testing.T) {
	r := newRig(t)
	dir := r.bundle("3.1.4")
	must(t, os.Remove(filepath.Join(dir, "VERSION")))
	out, code := r.run(filepath.Join(dir, "install.sh"))
	if code != 0 || r.installedVersion() != "3.1.4" {
		t.Fatalf("%d %s\n%s", code, r.installedVersion(), out)
	}
}

func TestScriptsAreLFAndHaveShebangs(t *testing.T) {
	for _, f := range []string{"install.sh", "update.sh", "uninstall.sh"} {
		b, err := os.ReadFile(f)
		must(t, err)
		if !bytes.HasPrefix(b, []byte("#!/bin/sh\n")) && !bytes.HasPrefix(b, []byte("#!/bin/sh\r\n")) {
			t.Errorf("%s: no #!/bin/sh line", f)
		}
		if bytes.Contains(b, []byte("bash")) && !strings.Contains(string(b), "dash") {
			// POSIX sh only: Raspberry Pi OS runs these with dash.
			for _, bashism := range []string{"[[", "function ", "local -a", "$'"} {
				if bytes.Contains(b, []byte(bashism)) {
					t.Errorf("%s: bashism %q", f, bashism)
				}
			}
		}
	}
}

// TestLowWearJournalIsOptIn: the journald drop-in is written only with
// --low-wear-journal, and the unit leaves time for the final flush.
func TestLowWearJournalIsOptIn(t *testing.T) {
	const dropIn = "etc/systemd/journald.conf.d/airview-low-wear.conf"
	r := newRig(t)
	if out, code := r.run(filepath.Join(r.bundle("1.0.0"), "install.sh")); code != 0 {
		t.Fatalf("install: %s", out)
	}
	if r.exists(dropIn) || strings.Contains(r.log(), "systemd-journald") {
		t.Fatal("journald changed without --low-wear-journal")
	}
	out, code := r.run(filepath.Join(r.bundle("1.0.0"), "install.sh"), "--low-wear-journal")
	if code != 0 {
		t.Fatalf("install --low-wear-journal: %d\n%s", code, out)
	}
	c := r.read(dropIn)
	if !strings.Contains(c, "[Journal]") || !strings.Contains(c, "Storage=volatile") {
		t.Fatalf("drop-in:\n%s", c)
	}
	if !strings.Contains(r.log(), "systemctl restart systemd-journald") {
		t.Error("journald not restarted")
	}
	if !strings.Contains(out, "airview.log") {
		t.Errorf("summary does not name the log file:\n%s", out)
	}
	unit := r.read("etc/systemd/system/airview.service")
	if !strings.Contains(unit, "TimeoutStopSec=120") {
		t.Error("unit has no stop timeout for the final flush")
	}
}
