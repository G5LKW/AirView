package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"airview/internal/selfupdate"
)

// findBash returns a POSIX bash (Git Bash on Windows, never WSL's
// System32\bash.exe).
func findBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		for _, p := range []string{`C:\Program Files\Git\bin\bash.exe`, `C:\Program Files\Git\usr\bin\bash.exe`} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		t.Skip("Git Bash not found")
	}
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	return p
}

// TestInstallerCompat checks that a release published by this tool is
// accepted by the Pi installer's own verification code (OpenSSL Ed25519
// over the exact bytes, awk field extraction), taken from
// deploy/feeder/pi/install.sh at test time.
func TestInstallerCompat(t *testing.T) {
	bash := findBash(t)
	if out, err := exec.Command(bash, "-c", "openssl version").CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "OpenSSL 3") {
		t.Skipf("OpenSSL 3 not available: %s", out)
	}
	script, err := os.ReadFile(filepath.Join("..", "feeder", "pi", "install.sh"))
	if err != nil {
		t.Skipf("installer not found: %v", err)
	}
	var funcs []string
	for _, name := range []string{"have", "ed25519_verify", "manifest_field", "manifest_version"} {
		f := extractFunc(string(script), name)
		if f == "" && name != "have" {
			t.Fatalf("function %s not found in install.sh", name)
		}
		funcs = append(funcs, f)
	}
	if funcs[0] == "" {
		funcs[0] = "have() { command -v \"$1\" >/dev/null 2>&1; }"
	}
	dir := t.TempDir()
	pub, priv, _ := selfupdate.GenerateKey()
	art := filepath.Join(dir, "a")
	os.WriteFile(art, append([]byte("\x7fELF"), bytes.Repeat([]byte{7}, 4096)...), 0o755)
	m, err := selfupdate.Publish(selfupdate.PublishOptions{Product: "airview-feeder", Version: "3.1.4", Channel: "stable",
		Notes: "braces {} and \"quotes\" linux-arm64", Artifacts: map[string]string{"linux-arm64": art, "linux-amd64": art},
		OutDir: filepath.Join(dir, "rel"), Key: priv})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := m.FileFor("linux-arm64")
	lib := filepath.Join(dir, "lib.sh")
	os.WriteFile(lib, []byte(strings.Join(funcs, "\n")+"\n"), 0o644)
	mp := filepath.ToSlash(filepath.Join(dir, "rel", "manifest.json"))
	cmd := exec.Command(bash, "-c", `. "$1"; ed25519_verify "$2" "$3" "$3.sig" && echo VERIFIED; `+
		`echo "sha=$(manifest_field "$3" linux-arm64 sha256) url=$(manifest_field "$3" linux-arm64 url) size=$(manifest_field "$3" linux-arm64 size) ver=$(manifest_version "$3")"`,
		"bash", filepath.ToSlash(lib), pub.String(), mp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "sha=" + f.SHA256 + " url=dl/linux-arm64 size=4100 ver=3.1.4"
	if !strings.Contains(string(out), "VERIFIED") || !strings.Contains(string(out), want) {
		t.Fatalf("installer view:\n%s\nwant %s", out, want)
	}
	// And the installer rejects a tampered manifest.
	body, _ := os.ReadFile(filepath.Join(dir, "rel", "manifest.json"))
	os.WriteFile(filepath.Join(dir, "rel", "manifest.json"), bytes.Replace(body, []byte("3.1.4"), []byte("3.1.5"), 1), 0o644)
	out, _ = exec.Command(bash, "-c", `. "$1"; ed25519_verify "$2" "$3" "$3.sig"; echo "rc=$?"`, "bash", filepath.ToSlash(lib), pub.String(), mp).CombinedOutput()
	if !strings.Contains(string(out), "rc=1") {
		t.Fatalf("tampered manifest: %s", out)
	}
}

// extractFunc returns the shell function NAME() { ... } (up to the first
// line that is exactly "}").
func extractFunc(script, name string) string {
	lines := strings.Split(strings.ReplaceAll(script, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, name+"() {") {
			if strings.HasSuffix(strings.TrimSpace(l), "}") && strings.Count(l, "{") == strings.Count(l, "}") {
				return l
			}
			for j := i + 1; j < len(lines); j++ {
				if lines[j] == "}" {
					return strings.Join(lines[i:j+1], "\n")
				}
			}
		}
	}
	return ""
}
