package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func readBuildPS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "build.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The release build wires the deliverables fixes together: the release
// public key reaches the feeder installers and image (DLV-03), the image
// gets the licence texts (DLV-05), and a build whose sources changed while
// it ran is refused (DLV-01).
func TestBuildScriptWiring(t *testing.T) {
	ps := readBuildPS(t)
	for _, want := range []string{
		"$instArgs.ManifestKey = $pub",
		"'-manifest-key', $pub",
		"'-licence', $licence, '-notices-station', $noticesAirview",
		"'-notices-feeder', $noticesFeeder",
		"LastWriteTime -gt $buildStart",
		"looks like a test key",
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("deploy/build.ps1 lacks %q", want)
		}
	}
}

// DLV-04: a signing key in a temporary folder (or named like a test key) is
// refused before anything is built, unless -AllowTestKey.
func TestBuildRefusesTestKey(t *testing.T) {
	if runtime.GOOS != "windows" || testing.Short() {
		t.Skip("runs Windows PowerShell")
	}
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not found")
	}
	tmp := t.TempDir() // below %TEMP%
	key := filepath.Join(tmp, "release.key")
	if err := os.WriteFile(key, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out")
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join("..", "build.ps1"),
		"-OutDir", out, "-SkipTests", "-WindowsOnly", "-SignKey", key)
	b, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(b), "looks like a test key") {
		t.Fatalf("a temporary-folder key was accepted (err %v):\n%s", err, b)
	}
	if _, err := os.Stat(filepath.Join(out, "airview.exe")); err == nil {
		t.Fatal("the build ran before refusing the key")
	}
}
