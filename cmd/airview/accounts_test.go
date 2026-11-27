package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// SEC-09: the setup token goes to a 0600 file, not the journal.
func TestWriteSecretFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), setupTokenFile)
	if err := writeSecretFile(p, "tok\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(p, "tok2\n"); err != nil { // replaced
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "tok2\n" {
		t.Fatalf("%q %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
}
