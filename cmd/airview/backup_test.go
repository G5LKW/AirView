package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airview/internal/config"
	"airview/internal/model"
	"airview/internal/store"
)

func TestSplitCommand(t *testing.T) {
	for _, c := range []struct {
		args []string
		cmd  string
		rest int
	}{
		{nil, "", 0},
		{[]string{"-headless", "-demo"}, "", 2},
		{[]string{"serve", "-port", "8080"}, "serve", 2},
		{[]string{"backup"}, "backup", 0},
	} {
		cmd, rest := splitCommand(c.args)
		if cmd != c.cmd || len(rest) != c.rest {
			t.Errorf("%v: got %q %v", c.args, cmd, rest)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runVersion([]string{"-json"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"version"`) || !strings.Contains(out.String(), model.AppName) {
		t.Fatalf("output %s", out.String())
	}
}

func TestListenHostFlag(t *testing.T) {
	f, err := parseFlags([]string{"-listen-host", "127.0.0.1", "-headless"}, &bytes.Buffer{})
	if err != nil || f.listenHost != "127.0.0.1" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := parseFlags([]string{"stray"}, &bytes.Buffer{}); err == nil {
		t.Fatal("positional argument accepted")
	}
}

// fillDataDir creates a data directory with a database, settings and an
// uplink identity.
func fillDataDir(t *testing.T, dir string) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := model.DefaultConfig()
	cfg.Receiver.Name = "backup-test"
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "uplink", "uplinks", "club", "spool"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "uplink", "identity.key"), []byte("key"), 0o600))
	must(os.WriteFile(filepath.Join(dir, "uplink", "uplinks", "club", "state.json"), []byte("{}"), 0o600))
	must(os.WriteFile(filepath.Join(dir, "uplink", "uplinks", "club", "spool", "seg-1"), []byte("spool"), 0o600))
	must(os.WriteFile(filepath.Join(dir, "airview.log"), []byte("log"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "sharelinks.json"), []byte(`{"links":[]}`), 0o600))
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	src := t.TempDir()
	fillDataDir(t, src)
	// A row written while "the server runs" must be in the snapshot.
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(src, storeDBName)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE backup_probe(v TEXT); INSERT INTO backup_probe VALUES('hello')`); err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	archive := filepath.Join(t.TempDir(), "b.zip")
	var out, errb bytes.Buffer
	if code := runBackup([]string{"-data", src, "-o", archive}, &out, &errb); code != 0 {
		t.Fatalf("backup exit %d: %s", code, errb.String())
	}
	// A second backup to the same file is refused (no overwrite).
	if code := runBackup([]string{"-data", src, "-o", archive}, &out, &errb); code == 0 {
		t.Fatal("backup overwrote an existing file")
	}
	// SO-11: the snapshots were staged in the data directory (not the
	// system temp directory) and removed afterwards.
	if left, _ := filepath.Glob(filepath.Join(src, ".backup-tmp-*")); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
	alt := t.TempDir()
	if code := runBackup([]string{"-data", src, "-tmp", alt, "-o", filepath.Join(t.TempDir(), "c.zip")}, &out, &errb); code != 0 {
		t.Fatalf("backup with -tmp: %d %s", code, errb.String())
	}
	zr, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	zr.Close()
	for _, want := range []string{manifestName, storeDBName, "config.json", "sharelinks.json", "uplink/identity.key", "uplink/uplinks/club/state.json"} {
		if !names[want] {
			t.Errorf("archive lacks %s (has %v)", want, names)
		}
	}
	for _, not := range []string{"airview.log", "uplink/uplinks/club/spool/seg-1"} {
		if names[not] {
			t.Errorf("archive contains %s", not)
		}
	}

	// Restore into a directory that already holds other data.
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "config.json"), []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runRestore([]string{"-data", dst, archive}, &out, &errb); code != 0 {
		t.Fatalf("restore exit %d: %s", code, errb.String())
	}
	cfg, _, err := config.LoadWithWarnings(dst)
	if err != nil || cfg.Receiver.Name != "backup-test" {
		t.Fatalf("restored config: %+v %v", cfg.Receiver, err)
	}
	asides, _ := filepath.Glob(filepath.Join(dst, "pre-restore-*", "config.json"))
	if len(asides) != 1 {
		t.Fatalf("replaced config.json not kept: %v", asides)
	}
	rdb, err := sql.Open("sqlite", sqliteDSN(filepath.Join(dst, storeDBName)))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	var v string
	if err := rdb.QueryRow(`SELECT v FROM backup_probe`).Scan(&v); err != nil || v != "hello" {
		t.Fatalf("restored database: %q %v", v, err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "uplink", "identity.key")); err != nil || string(b) != "key" {
		t.Fatalf("identity: %q %v", b, err)
	}
}

func TestRestoreRejectsDamagedArchive(t *testing.T) {
	src := t.TempDir()
	fillDataDir(t, src)
	var buf bytes.Buffer
	if _, err := writeBackup(context.Background(), src, &buf, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Re-pack with config.json altered but the manifest unchanged.
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var bad bytes.Buffer
	zw := zip.NewWriter(&bad)
	for _, f := range zr.File {
		w, _ := zw.Create(f.Name)
		if f.Name == "config.json" {
			w.Write([]byte(`{"mode":"tampered"}`))
			continue
		}
		rc, _ := f.Open()
		var b bytes.Buffer
		b.ReadFrom(rc)
		rc.Close()
		w.Write(b.Bytes())
	}
	zw.Close()
	archive := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(archive, bad.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	var out, errb bytes.Buffer
	if code := runRestore([]string{"-data", dst, archive}, &out, &errb); code == 0 {
		t.Fatal("damaged archive restored")
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Fatalf("restore touched the data directory: %v", entries)
	}
}

func TestSafeRelPath(t *testing.T) {
	for p, ok := range map[string]bool{
		"airview.db": true, "uplink/identity.key": true,
		"../x": false, "/etc/passwd": false, "a/../../b": false, `a\b`: false, "C:/x": false, "": false, "a//b": false,
	} {
		if safeRelPath(p) != ok {
			t.Errorf("safeRelPath(%q) = %v", p, !ok)
		}
	}
}
