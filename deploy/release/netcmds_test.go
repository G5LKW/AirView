package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/selfupdate"
)

func TestCheckCommand(t *testing.T) {
	keys := filepath.Join(t.TempDir(), "keys")
	if code, _, errs := runCLI(t, "keygen", "-out", keys); code != 0 {
		t.Fatal(errs)
	}
	key := filepath.Join(keys, "airview-release.key")
	pubF := filepath.Join(keys, "airview-release.pub")
	dist := t.TempDir()
	elf := append([]byte("\x7fELF"), bytes.Repeat([]byte{1}, 2000)...)
	os.WriteFile(filepath.Join(dist, "airview-feeder-linux-arm64"), elf, 0o755)
	rel := t.TempDir()
	for _, ch := range []string{"stable", "beta"} {
		v := map[string]string{"stable": "1.0.0", "beta": "1.1.0-beta.1"}[ch]
		if code, _, errs := runCLI(t, "publish", "-key", key, "-product", "airview-feeder", "-version", v, "-channel", ch, "-dist", dist, "-out", rel); code != 0 {
			t.Fatal(errs)
		}
	}
	srv := httptest.NewServer(http.StripPrefix("/feeder", selfupdate.ReleaseHandler{Dir: rel}))
	defer srv.Close()
	murl := srv.URL + "/feeder/manifest.json"

	code, out, errs := runCLI(t, "check", "-pubfile", pubF, "-url", murl, "-download")
	if code != 0 || !strings.Contains(out, "1.0.0 (stable)") || !strings.Contains(out, "linux-arm64    OK") {
		t.Fatalf("stable: %d %s %s", code, out, errs)
	}
	code, out, errs = runCLI(t, "check", "-pubfile", pubF, "-url", murl, "-channel", "beta")
	if code != 0 || !strings.Contains(out, "1.1.0-beta.1 (beta)") || !strings.Contains(out, "/feeder/beta/dl/linux-arm64") {
		t.Fatalf("beta: %d %s %s", code, out, errs)
	}
	// Artifact replaced on the server.
	os.WriteFile(filepath.Join(rel, "dl", "linux-arm64"), append(elf, 1), 0o644)
	if code, _, errs := runCLI(t, "check", "-pubfile", pubF, "-url", murl, "-download"); code != 1 || !strings.Contains(errs, "does not match") {
		t.Fatalf("artifact tamper: %d %s", code, errs)
	}
	// Tampered manifest.
	mp := filepath.Join(rel, "manifest.json")
	body, _ := os.ReadFile(mp)
	os.WriteFile(mp, bytes.Replace(body, []byte("1.0.0"), []byte("9.0.0"), 1), 0o644)
	if code, _, errs := runCLI(t, "check", "-pubfile", pubF, "-url", murl); code != 1 || !strings.Contains(errs, "signature") {
		t.Fatalf("manifest tamper: %d %s", code, errs)
	}
	// Another key.
	other := filepath.Join(t.TempDir(), "k2")
	runCLI(t, "keygen", "-out", other)
	os.WriteFile(mp, body, 0o644)
	if code, _, _ := runCLI(t, "check", "-pubfile", filepath.Join(other, "airview-release.pub"), "-url", murl); code != 1 {
		t.Fatal("accepted with a foreign key")
	}
	// Usage.
	for _, args := range [][]string{{"check", "-url", murl}, {"check", "-pubfile", pubF}, {"check", "-pubfile", pubF, "-url", murl, "-channel", "x"}, {"serve"}} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
