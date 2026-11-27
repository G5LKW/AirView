package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/selfupdate"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestReleaseWorkflow(t *testing.T) {
	keys := filepath.Join(t.TempDir(), "keys")
	code, out, errs := runCLI(t, "keygen", "-out", keys)
	if code != 0 {
		t.Fatalf("keygen: %d %s", code, errs)
	}
	pubText := strings.TrimSpace(string(must(os.ReadFile(filepath.Join(keys, "airview-release.pub")))))
	if !strings.Contains(out, pubText) {
		t.Fatalf("keygen output %q", out)
	}
	if _, err := selfupdate.ParsePublicKey(pubText); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "keygen", "-out", keys); code != 1 {
		t.Fatal("keygen overwrote a key")
	}
	key := filepath.Join(keys, "airview-release.key")
	if code, out, _ := runCLI(t, "pubkey", "-key", key); code != 0 || strings.TrimSpace(out) != pubText {
		t.Fatalf("pubkey: %s", out)
	}

	dist := t.TempDir()
	elf := append([]byte("\x7fELF"), bytes.Repeat([]byte{1}, 1000)...)
	pe := append([]byte("MZ"), bytes.Repeat([]byte{2}, 1000)...)
	os.WriteFile(filepath.Join(dist, "airview-feeder-linux-arm64"), elf, 0o755)
	os.WriteFile(filepath.Join(dist, "airview-feeder-linux-amd64"), append(elf, 3), 0o755)
	os.WriteFile(filepath.Join(dist, "airview-feeder-windows-amd64.exe"), pe, 0o755)
	os.WriteFile(filepath.Join(dist, "airview-linux-amd64"), elf, 0o755)
	rel := filepath.Join(t.TempDir(), "release")
	code, out, errs = runCLI(t, "publish", "-key", key, "-product", "airview-feeder", "-version", "1.2.3", "-dist", dist, "-out", rel, "-notes", "test {release}")
	if code != 0 {
		t.Fatalf("publish: %d %s", code, errs)
	}
	if strings.Count(out, "\n") != 4 || !strings.Contains(out, "windows-amd64") {
		t.Fatalf("publish output %s", out)
	}
	mpath := filepath.Join(rel, "manifest.json")
	m, err := selfupdate.ParseManifest(must(os.ReadFile(mpath)))
	if err != nil || len(m.Files) != 3 || m.Notes != "test {release}" || m.Channel != "stable" {
		t.Fatalf("%+v %v", m, err)
	}
	if code, out, errs := runCLI(t, "verify", "-pub", pubText, "-in", mpath, "-dir", rel); code != 0 || strings.Count(out, " OK") != 3 {
		t.Fatalf("verify: %d %s %s", code, out, errs)
	}
	// A replaced artifact and a tampered manifest are detected.
	os.WriteFile(filepath.Join(rel, "dl", "linux-arm64"), append(elf, 9), 0o644)
	if code, _, errs := runCLI(t, "verify", "-pubfile", filepath.Join(keys, "airview-release.pub"), "-in", mpath, "-dir", rel); code != 1 || !strings.Contains(errs, "linux-arm64") {
		t.Fatalf("artifact tamper: %d %s", code, errs)
	}
	body := must(os.ReadFile(mpath))
	os.WriteFile(mpath, bytes.Replace(body, []byte("1.2.3"), []byte("1.2.4"), 1), 0o644)
	if code, _, errs := runCLI(t, "verify", "-pub", pubText, "-in", mpath); code != 1 || !strings.Contains(errs, "signature") {
		t.Fatalf("manifest tamper: %d %s", code, errs)
	}
	// sign re-signs a (now valid) hand-edited manifest.
	if code, _, errs := runCLI(t, "sign", "-key", key, "-in", mpath); code != 0 {
		t.Fatalf("sign: %s", errs)
	}
	if code, _, _ := runCLI(t, "verify", "-pub", pubText, "-in", mpath); code != 0 {
		t.Fatal("re-signed manifest not valid")
	}
	os.WriteFile(mpath, []byte(`{"schema":1}`), 0o644)
	if code, _, _ := runCLI(t, "sign", "-key", key, "-in", mpath); code != 1 {
		t.Fatal("signed an invalid manifest")
	}
	// Beta channel and explicit files.
	code, _, errs = runCLI(t, "publish", "-key", key, "-product", "airview", "-version", "2.0.0-beta.1", "-channel", "beta",
		"-file", "linux-amd64="+filepath.Join(dist, "airview-linux-amd64"), "-out", rel)
	if code != 0 {
		t.Fatalf("beta: %s", errs)
	}
	if _, err := os.Stat(filepath.Join(rel, "beta", "dl", "linux-amd64")); err != nil {
		t.Fatal(err)
	}
	// Usage errors.
	for _, args := range [][]string{{}, {"nope"}, {"publish", "-key", key, "-product", "Bad", "-version", "1", "-dist", dist, "-out", rel},
		{"publish", "-key", key, "-product", "airview", "-version", "x", "-dist", dist, "-out", rel},
		{"publish", "-key", key, "-product", "airview", "-version", "1.0.0", "-channel", "nightly", "-dist", dist, "-out", rel},
		{"publish", "-key", key, "-product", "airview", "-version", "1.0.0", "-file", "bad=x", "-out", rel},
		{"verify", "-in", mpath}, {"keygen"}} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	// A wrong-format artifact is refused at publish time.
	os.WriteFile(filepath.Join(dist, "airview-feeder-linux-armv7"), pe, 0o755)
	if code, _, errs := runCLI(t, "publish", "-key", key, "-product", "airview-feeder", "-version", "1.2.5", "-dist", dist, "-out", rel); code != 1 || !strings.Contains(errs, "ELF") {
		t.Fatalf("wrong format: %d %s", code, errs)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
