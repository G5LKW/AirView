package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func findBash(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("AVF_TEST_BASH"); b != "" {
		return b
	}
	if runtime.GOOS == "windows" {
		// Not exec.LookPath("bash"): System32\bash.exe is WSL, not Git Bash.
		for _, p := range []string{`C:\Program Files\Git\bin\bash.exe`, `C:\Program Files (x86)\Git\bin\bash.exe`} {
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

// TestInstallerAcceptsOurArguments runs the real feeder installer
// (deploy/feeder/pi/install.sh) in its --dry-run mode with the arguments the
// setup page generates, so a change of its option rules breaks this test.
func TestInstallerAcceptsOurArguments(t *testing.T) {
	bash := findBash(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "airview-feeder-linux-arm64")
	hdr := make([]byte, 64)
	copy(hdr, "\x7fELF\x02\x01\x01")
	hdr[18] = 183 // EM_AARCH64
	if err := os.WriteFile(bin, hdr, 0o755); err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join("..", "..", "..", "deploy", "feeder", "pi", "install.sh")
	for name, v := range map[string]values{
		"located": {Server: "feed.example.net:30160", Pin: "sha256/" + strings.Repeat("A", 43) + "=", Token: "T0K-1234", Name: "pi-one",
			Lat: "51.5", Lon: "-0.1", Alt: "30m", AltRef: "msl", AGL: "8m", Input: "auto", Privacy: "region", Timestamps: true, AutoUpdate: true},
		"network": {Server: "10.0.0.5", Name: "pi-two", NoLocation: true, AGL: "25ft", Input: "192.168.1.7:30005", Privacy: "hidden"},
	} {
		if err := v.validate(false); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		args := append([]string{filepath.ToSlash(installer)}, v.installerArgs(filepath.ToSlash(bin))...)
		args = append(args, "--arch", "arm64", "--dry-run", "--dry-run-root", filepath.ToSlash(filepath.Join(dir, name)))
		cmd := exec.Command(bash, args...)
		cmd.Env = append(os.Environ(), "NO_COLOR=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: installer refused our arguments: %v\n%s", name, err, out)
		}
		if strings.Contains(string(out), "ERROR") {
			t.Fatalf("%s:\n%s", name, out)
		}
	}
}

// TestFeederAcceptsStatusPageSettings builds the real airview-feeder and runs
// the "config set" call configure makes on a feeder.json written by the
// installer's dry run, then "config check".
func TestFeederAcceptsStatusPageSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("builds airview-feeder")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not in PATH")
	}
	bash := findBash(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "airview-feeder-linux-arm64")
	hdr := make([]byte, 64)
	copy(hdr, "\x7fELF\x02\x01\x01")
	hdr[18] = 183
	os.WriteFile(bin, hdr, 0o755)
	v := values{Server: "feed.example.net", Name: "pi-one", NoLocation: true, Input: "auto", Privacy: "hidden", Timestamps: true}
	root := filepath.Join(dir, "root")
	args := append([]string{filepath.ToSlash(filepath.Join("..", "..", "..", "deploy", "feeder", "pi", "install.sh"))}, v.installerArgs(filepath.ToSlash(bin))...)
	args = append(args, "--arch", "arm64", "--dry-run", "--dry-run-root", filepath.ToSlash(root))
	if out, err := exec.Command(bash, args...).CombinedOutput(); err != nil {
		t.Fatalf("installer: %v\n%s", err, out)
	}
	conf := filepath.Join(root, "etc", "airview-feeder", "feeder.json")
	if _, err := os.Stat(conf); err != nil {
		t.Fatalf("installer wrote no feeder.json: %v", err)
	}
	feeder := filepath.Join(dir, "airview-feeder"+exeSuffix())
	if out, err := exec.Command(goBin, "build", "-o", feeder, "airview/cmd/airview-feeder").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	a, fr := testApp(t)
	a.o.feeder = feeder
	a.o.config = conf
	a.run = a.execRun // real commands from here on
	_ = fr
	hash := statusHashFor(t, "correct horse")
	out, err := a.execRun(t.Context(), feeder, "config", "set", "-config", conf, "ui.addr=0.0.0.0:8099", "ui.passwordHash="+hash)
	if err != nil {
		t.Fatalf("config set: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(conf)
	if !strings.Contains(string(b), `"addr": "0.0.0.0:8099"`) || !strings.Contains(string(b), hash) {
		t.Fatalf("feeder.json:\n%s", b)
	}
	if out, err := exec.Command(feeder, "config", "check", "-config", conf).CombinedOutput(); err != nil {
		t.Fatalf("config check: %v\n%s", err, out)
	}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
