package main

import (
	"bytes"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// DLV-06: the release stub (no build tags) links the asInvoker manifest,
// so /CURRENTUSER needs no UAC; requireAdministrator is opt-in
// (-tags setupadmin).
func TestReleaseStubIsAsInvoker(t *testing.T) {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "windows", "amd64", false
	match := func(file string, tags ...string) bool {
		c := ctx
		c.BuildTags = tags
		ok, err := c.MatchFile(".", file)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !match("res_user.go") || match("res_admin.go") {
		t.Fatal("the default (release) build must link res/user (asInvoker), not res/admin")
	}
	if match("res_user.go", "setupadmin") || !match("res_admin.go", "setupadmin") {
		t.Fatal("-tags setupadmin must link res/admin")
	}
	for dir, want := range map[string]string{"res/user": `"as invoker"`, "res/admin": `"require administrator"`} {
		b, err := os.ReadFile(filepath.Join(dir, "winres.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("%s/winres.json: execution-level is not %s", dir, want)
		}
	}
}

// The linked stub really carries level="asInvoker" in its PE manifest.
func TestReleaseStubManifestInBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the stub")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not found")
	}
	out := filepath.Join(t.TempDir(), "stub.exe")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-H windowsgui -s -w", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=windows", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`level="asInvoker"`)) || bytes.Contains(b, []byte(`level="requireAdministrator"`)) {
		t.Fatalf("stub manifest is not asInvoker (%s)", runtime.GOOS)
	}
}
