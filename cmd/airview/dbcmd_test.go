package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airview/internal/store"
)

func TestDBSnapshot(t *testing.T) {
	dir := t.TempDir()
	fillDataDir(t, dir)
	out := t.TempDir()
	var so, se bytes.Buffer
	if code := runDB([]string{"snapshot", "-h"}, &so, &se); code != 0 {
		t.Fatalf("-h exit %d", code)
	}
	if code := runDB([]string{"snapshot", "-data", dir, "-o", out}, &so, &se); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	srcs, _ := filepath.Glob(filepath.Join(dir, "*.db"))
	if len(srcs) == 0 {
		t.Fatal("test data dir has no database")
	}
	for _, s := range srcs {
		if st, err := os.Stat(filepath.Join(out, filepath.Base(s))); err != nil || st.Size() == 0 {
			t.Fatalf("no snapshot of %s: %v", s, err)
		}
	}
	// A second run into the same directory refuses to overwrite.
	if code := runDB([]string{"snapshot", "-data", dir, "-o", out}, &so, &se); code == 0 {
		t.Fatal("overwrote existing snapshots")
	}
	if code := runDB([]string{"snapshot", "-data", dir}, &so, &se); code != 2 {
		t.Fatalf("missing -o: exit %d", code)
	}
}

// TestDBProfile: "airview db profile" resolves the same profile as the
// server, with the storage.tuning overrides, as text and JSON.
func TestDBProfile(t *testing.T) {
	dir := t.TempDir()
	var so, se bytes.Buffer
	args := []string{"profile", "-data", dir, "-set", "storage.profile=low-wear", "-set", "storage.tuning.cacheMB=3", "-json"}
	if code := runDB(args, &so, &se); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	var got struct {
		Databases []dbProfileJSON `json:"databases"`
	}
	if err := json.Unmarshal(so.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, so.String())
	}
	if len(got.Databases) != 1 {
		t.Fatalf("databases %+v", got.Databases)
	}
	d := got.Databases[0]
	if d.DB != "airview.db" || d.Profile.Name != store.ProfileLowWear || d.Profile.WriterCacheKiB != 3<<10 ||
		d.Profile.FlushIntervalMs != 30000 || strings.Join(d.Overrides, ",") != "storage.tuning.cacheMB" {
		t.Fatalf("profile %+v", d)
	}

	// Aggregator mode: both databases, server profile.
	so.Reset()
	if code := runDB([]string{"profile", "-data", dir, "-set", "mode=aggregator"}, &so, &se); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	text := so.String()
	for _, want := range []string{"airview.db: profile server (auto: aggregator mode)", "stations.db: profile server",
		"group commit", "5s", "8 x 16 MiB"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	// The environment layer counts too (AIRVIEW_STORAGE_PROFILE).
	so.Reset()
	if code := runDBProfile([]string{"-data", dir, "-json"}, &so, &se, []string{"AIRVIEW_STORAGE_PROFILE=standard"}); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	if !strings.Contains(so.String(), `"name": "standard"`) || !strings.Contains(so.String(), `"requested": "standard"`) {
		t.Fatalf("env profile:\n%s", so.String())
	}
	if code := runDB([]string{"profile", "-data", dir, "extra"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("extra argument: exit %d", code)
	}
}

func TestDBMigrateUsage(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"migrate", "-data", dir},
		{"migrate", "-data", dir, "--status", "--now"},
		{"migrate", "-data", dir, "--bogus"},
	} {
		if code := runDB(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code := runDB([]string{"migrate", "-data", dir, "--status"}, io.Discard, io.Discard); code != 1 {
		t.Errorf("status without a database: exit %d, want 1", code)
	}
	if code := runDB([]string{"migrate", "-h"}, io.Discard, io.Discard); code != 0 {
		t.Errorf("-h: exit %d", code)
	}
}

// oldFormatDB writes a database in the 1.0.0 layout: the 1.0.0 tables
// with a few rows and no data-migration bookkeeping.
func oldFormatDB(t *testing.T, dir string) {
	t.Helper()
	src := filepath.Join("testdata", "airview-1.0.0.db")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "airview.db"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, path, table string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return n
}

// TestDBMigrateOldFormat: status, backup and --now on a database written
// by AirView 1.0.0 (testdata/airview-1.0.0.db, recorded by the 1.0.0
// build in demo mode). The migration keeps every flight and trail point.
func TestDBMigrateOldFormat(t *testing.T) {
	dir := t.TempDir()
	oldFormatDB(t, dir)
	dbPath := filepath.Join(dir, "airview.db")
	flights := countRows(t, dbPath, "flights")
	points := countRows(t, dbPath, "positions")
	logical := []string{"aircraft", "flights", "positions", "events", "heat", "coverage", "signal_hist", "elev_hist",
		"range_hour", "met_obs", "guess_samples", "stats_minute"}
	pre := map[string]int64{}
	for _, tb := range logical {
		pre[tb] = countRows(t, dbPath, tb)
	}
	if flights == 0 || points == 0 {
		t.Fatalf("fixture has %d flights, %d points", flights, points)
	}

	// --status on the untouched 1.0.0 file: not started, nothing written.
	before, _ := os.Stat(dbPath)
	var so, se bytes.Buffer
	if code := runDB([]string{"migrate", "-data", dir, "--status"}, &so, &se); code != 0 {
		t.Fatalf("status: exit %d: %s", code, se.String())
	}
	if !strings.Contains(so.String(), "not started") {
		t.Fatalf("status of a 1.0.0 file:\n%s", so.String())
	}
	if after, _ := os.Stat(dbPath); !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("--status changed the database")
	}

	// --backup: a consistent copy under backup/, a second one gets a suffix.
	so.Reset()
	if code := runDB([]string{"migrate", "-data", dir, "--backup"}, &so, &se); code != 0 {
		t.Fatalf("backup: exit %d: %s", code, se.String())
	}
	day := time.Now().UTC().Format("20060102")
	b1 := filepath.Join(dir, store.BackupDirName, "airview-pre-1.1-"+day+".db")
	if countRows(t, b1, "positions") != points {
		t.Fatal("backup lacks trail points")
	}
	if code := runDB([]string{"migrate", "-data", dir, "--backup"}, &so, &se); code != 0 {
		t.Fatalf("second backup: exit %d: %s", code, se.String())
	}
	if _, err := os.Stat(filepath.Join(dir, store.BackupDirName, "airview-pre-1.1-"+day+"-2.db")); err != nil {
		t.Fatalf("second backup: %v\n%s", err, so.String())
	}

	// --now converts it; every flight and trail point is still there.
	so.Reset()
	t.Setenv("AIRVIEW_STORAGE_BACKUP_BEFORE_MIGRATE", "never")
	if code := runDB([]string{"migrate", "-data", dir, "--now"}, &so, &se); code != 0 {
		t.Fatalf("--now: exit %d: %s\n%s", code, se.String(), so.String())
	}
	info, err := fileMigrationInfo(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != store.MigDone {
		t.Fatalf("after --now: %+v\n%s", info, so.String())
	}
	st, err := store.OpenWithOptions(dir, store.Options{MigrationDelay: -1, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	counts, err := st.TableCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range logical {
		if counts[tb] != pre[tb] {
			t.Errorf("%s: %d rows after the migration, %d before", tb, counts[tb], pre[tb])
		}
	}
	t.Logf("migrated:\n%s", so.String())
	so.Reset()
	if code := runDB([]string{"migrate", "-data", dir, "--status", "-json"}, &so, &se); code != 0 {
		t.Fatalf("status: exit %d", code)
	}
	if !strings.Contains(so.String(), `"state": "done"`) {
		t.Fatalf("status after --now:\n%s", so.String())
	}
}

// TestDBVacuum: "airview db vacuum" after the migration of a 1.0.0 file
// returns the freed pages and keeps every logical row.
func TestDBVacuum(t *testing.T) {
	dir := t.TempDir()
	if code := runDB([]string{"vacuum", "-data", dir}, io.Discard, io.Discard); code != 1 {
		t.Errorf("vacuum without a database: exit %d, want 1", code)
	}
	if code := runDB([]string{"vacuum", "-data", dir, "extra"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("vacuum with an argument: exit %d, want 2", code)
	}
	oldFormatDB(t, dir)
	dbPath := filepath.Join(dir, "airview.db")
	t.Setenv("AIRVIEW_STORAGE_BACKUP_BEFORE_MIGRATE", "never")
	var so, se bytes.Buffer
	if code := runDB([]string{"migrate", "-data", dir, "--now"}, &so, &se); code != 0 {
		t.Fatalf("--now: exit %d: %s", code, se.String())
	}
	counts := func() map[string]int64 {
		st, err := store.OpenWithOptions(dir, store.Options{MigrationDelay: -1, Logger: quietLog()})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		c, err := st.TableCounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	pre := counts()
	before := fileSize(dbPath)
	so.Reset()
	if code := runDB([]string{"vacuum", "-data", dir}, &so, &se); code != 0 {
		t.Fatalf("vacuum: exit %d: %s", code, se.String())
	}
	if !strings.Contains(so.String(), "airview.db:") {
		t.Fatalf("vacuum output: %q", so.String())
	}
	if after := fileSize(dbPath); after > before {
		t.Errorf("file grew: %d -> %d", before, after)
	}
	post := counts()
	for _, tb := range []string{"flights", "positions", "aircraft", "heat", "coverage", "range_hour", "met_obs"} {
		if post[tb] != pre[tb] {
			t.Errorf("%s: %d rows after vacuum, %d before", tb, post[tb], pre[tb])
		}
	}
	t.Logf("%s", so.String())
}
