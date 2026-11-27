package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func backupOpts(mode string, free *atomic.Uint64) Options {
	o := migOpts(fakeMigration(100, 0))
	o.Profile = Profile{Name: ProfileStandard, BackupBeforeMigrate: mode}
	o.testDiskFree = func(string) (uint64, error) { return free.Load(), nil }
	o.testRecheck = 50 * time.Millisecond
	return o
}

func TestPreMigrationBackupAuto(t *testing.T) {
	var free atomic.Uint64
	free.Store(1 << 40)
	dir := t.TempDir()
	s := openDir(t, dir, backupOpts("auto", &free))
	seedFake(t, s, 400)
	// A file the store did not write must survive the expiry.
	foreign := filepath.Join(dir, BackupDirName, "my-own.db")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	info := s.MigrationInfo()
	if info.Backup == "" || !strings.HasPrefix(filepath.Base(info.Backup), "airview-pre-1.1-") {
		t.Fatalf("no backup recorded: %+v", info)
	}
	// The backup is a complete, consistent copy of the pre-migration data.
	data, err := os.ReadFile(info.Backup)
	if err != nil || len(data) == 0 {
		t.Fatalf("backup file: %v", err)
	}
	cp := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(cp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := openDir(t, filepath.Dir(cp), migOpts(fakeMigration(100, 0)))
	if n := scalar[int](t, c, `SELECT COUNT(*) FROM fake_src`); n != 400 {
		t.Fatalf("backup holds %d source rows", n)
	}

	// Kept for 14 days after the migration completed, then deleted.
	if err := s.expireBackupAt(bg, time.Now().Add(13*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(info.Backup); err != nil {
		t.Fatal("backup deleted too early")
	}
	if err := s.expireBackupAt(bg, time.Now().Add(15*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(info.Backup); !os.IsNotExist(err) {
		t.Fatalf("backup not deleted: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("a foreign file was deleted")
	}
	if s.MigrationInfo().Backup != "" {
		t.Fatal("backup still reported")
	}
}

// Power-cut check: a backup interrupted by a crash is left under the
// partial name only; the next attempt removes it (and only it) and writes
// the backup under the first backup name, so no truncated file ever
// carries a backup name and none is orphaned.
func TestPreMigrationBackupInterruptedLeavesNoBackupName(t *testing.T) {
	var free atomic.Uint64
	free.Store(1 << 40)
	dir := t.TempDir()
	s := openDir(t, dir, backupOpts("auto", &free))
	seedFake(t, s, 300)
	bdir := filepath.Join(dir, BackupDirName)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("20060102")
	stale := []string{
		"airview-pre-1.1-" + day + ".db.partial",
		"airview-pre-1.1-" + day + ".db.partial-journal",
		"airview-pre-1.1-20200101-3.db.partial",
	}
	keep := []string{"my-own.db.partial", "stations-pre-1.1-" + day + ".db.partial", "airview-pre-1.1-x.db.partial"}
	for _, n := range append(append([]string{}, stale...), keep...) {
		if err := os.WriteFile(filepath.Join(bdir, n), []byte("truncated"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(bdir, "airview-pre-1.1-"+day+".db")
	if got := s.MigrationInfo().Backup; got != want {
		t.Fatalf("backup %q, want %q", got, want)
	}
	for _, n := range stale {
		if _, err := os.Stat(filepath.Join(bdir, n)); !os.IsNotExist(err) {
			t.Fatalf("stale partial copy %s kept: %v", n, err)
		}
	}
	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(bdir, n)); err != nil {
			t.Fatalf("foreign file %s removed: %v", n, err)
		}
	}
	ents, _ := os.ReadDir(bdir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "airview-pre-1.1-"+day) && e.Name() != filepath.Base(want) {
			t.Fatalf("unexpected file %s", e.Name())
		}
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(cp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := openDir(t, filepath.Dir(cp), migOpts(fakeMigration(100, 0)))
	if n := scalar[int](t, c, `SELECT COUNT(*) FROM fake_src`); n != 300 {
		t.Fatalf("backup holds %d source rows", n)
	}
}

func TestPreMigrationBackupAutoSkipsWithoutSpace(t *testing.T) {
	var free atomic.Uint64
	free.Store(300 << 20) // enough for the migration, not for a backup (+512 MiB)
	dir := t.TempDir()
	s := openDir(t, dir, backupOpts("auto", &free))
	seedFake(t, s, 200)
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	checkFake(t, s, 200, true)
	if s.MigrationInfo().Backup != "" {
		t.Fatal("backup written without space")
	}
	if v, _, _ := s.KVGet(bg, kvPreMigDecision); v != "skipped" {
		t.Fatalf("decision %q", v)
	}
}

func TestPreMigrationBackupAlwaysWaits(t *testing.T) {
	var free atomic.Uint64
	free.Store(300 << 20)
	dir := t.TempDir()
	s := openDir(t, dir, backupOpts("always", &free))
	seedFake(t, s, 200)
	err := s.MigrateNow(bg)
	if err == nil {
		t.Fatal("migrated without the required backup")
	}
	if info := s.MigrationInfo(); info.State != MigWaitingForBackup {
		t.Fatalf("state %+v", info)
	}
	checkFake(t, s, 200, false)
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM fake_dst`); n != 0 {
		t.Fatalf("%d rows migrated", n)
	}
	free.Store(1 << 40)
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	checkFake(t, s, 200, true)
	if s.MigrationInfo().Backup == "" {
		t.Fatal("no backup")
	}
}

func TestPreMigrationBackupNever(t *testing.T) {
	var free atomic.Uint64
	free.Store(1 << 40)
	dir := t.TempDir()
	s := openDir(t, dir, backupOpts("never", &free))
	seedFake(t, s, 100)
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, BackupDirName)); !os.IsNotExist(err) {
		t.Fatalf("backup directory created: %v", err)
	}
}

// No backup on a fresh database (nothing to migrate).
func TestPreMigrationBackupNotForEmptyDatabase(t *testing.T) {
	var free atomic.Uint64
	free.Store(1 << 40)
	dir := t.TempDir()
	s := openDir(t, dir, backupOpts("always", &free))
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, BackupDirName)); !os.IsNotExist(err) {
		t.Fatalf("backup of an empty database: %v", err)
	}
	if !s.DataMigrationDone("fake") {
		t.Fatal("not done")
	}
}
