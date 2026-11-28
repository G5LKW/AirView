package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
)

var bg = context.Background()

// manual disables the automatic flush triggers so that tests decide when
// queued rows are written.
var manual = Options{FlushInterval: time.Hour, FlushRows: 1 << 30, MaxQueueRows: 1 << 30}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openDir(t *testing.T, dir string, o Options) *Store {
	t.Helper()
	if o.Logger == nil {
		o.Logger = quiet()
	}
	s, err := OpenWithOptions(dir, o)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openTest(t *testing.T, o Options) *Store { return openDir(t, t.TempDir(), o) }

// scalar runs a single-value query on the read pool.
func scalar[T any](t *testing.T, s *Store, query string, args ...any) T {
	t.Helper()
	var v T
	if err := s.DB().QueryRowContext(bg, query, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

func mustFlush(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// ------------------------------------------------------------ open / pragmas

func TestOpenPragmasAndReadOnlyPool(t *testing.T) {
	s := openTest(t, manual)
	w := s.cur.Load().wdb
	t.Logf("SQLite %s", scalar[string](t, s, `SELECT sqlite_version()`))
	get := func(db *sql.DB, pragma string) string {
		var v string
		if err := db.QueryRowContext(bg, "PRAGMA "+pragma).Scan(&v); err != nil {
			t.Fatalf("PRAGMA %s: %v", pragma, err)
		}
		return strings.ToLower(v)
	}
	p := s.Profile()
	if p.Name != ProfileStandard && p.Name != ProfileLowWear {
		t.Fatalf("profile %q", p.Name)
	}
	itoa := func(v int64) string { return fmt.Sprint(v) }
	for pragma, want := range map[string]string{
		"journal_mode": "wal", "synchronous": "1", "foreign_keys": "1", "temp_store": "2",
		"busy_timeout": "10000", "auto_vacuum": "2",
		// Hard cap only (the checkpointer runs PASSIVE checkpoints).
		"wal_autocheckpoint": itoa(int64(p.WALHardCapPages)),
		"journal_size_limit": itoa(p.JournalSizeLimit),
		"cache_size":         itoa(-int64(p.WriterCacheKiB)),
		"mmap_size":          itoa(p.MmapBytes),
		"analysis_limit":     "400",
	} {
		if got := get(w, pragma); got != want {
			t.Errorf("writer PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
	c := s.cur.Load().cdb
	for pragma, want := range map[string]string{
		"journal_mode": "wal", "busy_timeout": "1000", "wal_autocheckpoint": "0", "query_only": "0",
	} {
		if got := get(c, pragma); got != want {
			t.Errorf("checkpointer PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
	r := s.DB()
	for pragma, want := range map[string]string{
		"journal_mode": "wal", "query_only": "1", "busy_timeout": "10000", "temp_store": "2", "foreign_keys": "1",
		"cache_size": itoa(-int64(p.ReaderCacheKiB)), "mmap_size": itoa(p.MmapBytes),
	} {
		if got := get(r, pragma); got != want {
			t.Errorf("reader PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
	if _, err := r.ExecContext(bg, `INSERT INTO kv(key, value, updated) VALUES('x', 'y', 0)`); err == nil {
		t.Fatal("a write through the read pool must fail")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), FileName)); err != nil {
		t.Fatalf("database file: %v", err)
	}
	if s.Path() != filepath.Join(s.Dir(), FileName) {
		t.Fatalf("path %q", s.Path())
	}
}

func TestOpenReplacesFileThatIsNotADatabase(t *testing.T) {
	dir := t.TempDir()
	junk := []byte(strings.Repeat("this is definitely not an SQLite database. ", 200))
	if err := os.WriteFile(filepath.Join(dir, FileName), junk, 0o644); err != nil {
		t.Fatal(err)
	}
	s := openDir(t, dir, manual)
	if err := s.KVSet(bg, "k", "v"); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(filepath.Join(dir, FileName+".corrupt-1"))
	if err != nil || string(kept) != string(junk) {
		t.Fatalf("damaged file not preserved: %v", err)
	}
}

// --------------------------------------------------------------- migrations

func TestMigrationsIdempotentAndRegistered(t *testing.T) {
	// Registered before Open: applied by Open.
	if err := RegisterMigration("mtest_early", 1, `CREATE TABLE mtest_early(id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := RegisterMigration("mtest_early", 2, `ALTER TABLE mtest_early ADD COLUMN w INTEGER NOT NULL DEFAULT 7`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := openDir(t, dir, manual)

	if n := scalar[int](t, s, `SELECT COUNT(*) FROM schema_migrations WHERE name = 'core'`); n != len(coreMigrations) {
		t.Fatalf("core migrations recorded: %d", n)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM schema_migrations WHERE name = 'mtest_early'`); n != 2 {
		t.Fatalf("early migrations recorded: %d", n)
	}
	if err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO mtest_early(id, v) VALUES(1, 'a')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if w := scalar[int](t, s, `SELECT w FROM mtest_early WHERE id = 1`); w != 7 {
		t.Fatalf("version 2 not applied after version 1: w=%d", w)
	}

	// Registered while open: applied immediately.
	if err := RegisterMigration("mtest_late", 1, `CREATE TABLE mtest_late(id INTEGER PRIMARY KEY); INSERT INTO mtest_late VALUES(42);`); err != nil {
		t.Fatal(err)
	}
	if v := scalar[int](t, s, `SELECT id FROM mtest_late`); v != 42 {
		t.Fatalf("late migration: %d", v)
	}
	// Same registration again: no-op. Different DDL: refused.
	if err := RegisterMigration("mtest_late", 1, `CREATE TABLE mtest_late(id INTEGER PRIMARY KEY); INSERT INTO mtest_late VALUES(42);`); err != nil {
		t.Fatalf("identical re-registration: %v", err)
	}
	if err := RegisterMigration("mtest_late", 1, `CREATE TABLE other(x)`); err == nil {
		t.Fatal("conflicting re-registration must fail")
	}
	for _, bad := range []struct {
		name string
		ver  int
		ddl  string
	}{{"", 1, "x"}, {"core", 9, "x"}, {"a", 0, "x"}, {"a", 1, "  "}} {
		if err := RegisterMigration(bad.name, bad.ver, bad.ddl); err == nil {
			t.Fatalf("RegisterMigration(%q, %d, %q) must fail", bad.name, bad.ver, bad.ddl)
		}
	}
	// A broken migration reports its error and leaves no trace.
	if err := RegisterMigration("mtest_broken", 1, `CREATE TABLE mtest_broken(x); THIS IS NOT SQL;`); err == nil {
		t.Fatal("broken DDL must be reported")
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'mtest_broken'`); n != 0 {
		t.Fatal("a failed migration must be rolled back")
	}
	registry.mu.Lock() // do not let the broken one fail later Opens
	for i, m := range registry.migs {
		if m.name == "mtest_broken" {
			registry.migs = append(registry.migs[:i], registry.migs[i+1:]...)
			break
		}
	}
	registry.mu.Unlock()

	// Reopen: nothing is applied twice, data survives.
	before := scalar[int](t, s, `SELECT COUNT(*) FROM schema_migrations`)
	applied := scalar[int64](t, s, `SELECT applied FROM schema_migrations WHERE name = 'mtest_late'`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openDir(t, dir, manual)
	if after := scalar[int](t, s2, `SELECT COUNT(*) FROM schema_migrations`); after != before {
		t.Fatalf("migrations re-applied: %d -> %d", before, after)
	}
	if a := scalar[int64](t, s2, `SELECT applied FROM schema_migrations WHERE name = 'mtest_late'`); a != applied {
		t.Fatal("migration row rewritten")
	}
	if n := scalar[int](t, s2, `SELECT COUNT(*) FROM mtest_late`); n != 1 {
		t.Fatalf("late migration ran again: %d rows", n)
	}
	if v := scalar[string](t, s2, `SELECT v FROM mtest_early WHERE id = 1`); v != "a" {
		t.Fatal("data lost across reopen")
	}
}

func TestSchemaHasEveryDocumentedTable(t *testing.T) {
	s := openTest(t, manual)
	counts, err := s.TableCounts(bg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kv", "aircraft", "flights", "positions", "stats_minute", "stats_hour", "range_hour",
		"coverage", "signal_hist", "elev_hist", "heat", "weather_obs", "weather_fc", "weather_fc_log", "ntp_samples",
		"met_obs", "met_profile", "records", "events", "acdb", "receiver_status", "schema_migrations"} {
		if _, ok := counts[name]; !ok {
			t.Errorf("table %s missing", name)
		}
	}
}

// ------------------------------------------------------------- batch writer

func TestBatchQueueFlushAndOrder(t *testing.T) {
	s := openTest(t, manual)
	b := NewBatch()
	b.AddEvents([]Event{{T: 100, Kind: "feed_up"}, {T: 101, Kind: "emergency", ICAO: 0xABCDEF, Detail: `{"squawk":"7700"}`}})
	b.SetKV("a", "1")
	if b.Len() != 3 {
		t.Fatalf("batch len %d", b.Len())
	}
	s.Submit(b)
	if b.Len() != 0 {
		t.Fatal("Submit must empty the batch")
	}
	s.SetKVAsync("a", "2") // later operation wins
	if st := s.Stats(); st.QueuedRows != 4 || st.QueuedOps != 3 {
		t.Fatalf("queued: %+v", st)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events`); n != 0 {
		t.Fatal("queued rows must not be visible before the flush")
	}
	mustFlush(t, s)
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events`); n != 2 {
		t.Fatalf("events after flush: %d", n)
	}
	if v, ok, err := s.KVGet(bg, "a"); err != nil || !ok || v != "2" {
		t.Fatalf("kv: %q %v %v", v, ok, err)
	}
	st := s.Stats()
	if st.QueuedRows != 0 || st.Flushes != 1 || st.FlushedRows != 4 || st.LastFlush == 0 {
		t.Fatalf("stats after flush: %+v", st)
	}

	// A synchronous Write sees (and comes after) everything queued before it.
	s.SetKVAsync("b", "queued")
	err := s.Write(bg, func(tx *sql.Tx) error {
		var v string
		if err := tx.QueryRow(`SELECT value FROM kv WHERE key = 'b'`).Scan(&v); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE kv SET value = ? WHERE key = 'b'`, v+"+sync")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.KVGet(bg, "b"); v != "queued+sync" {
		t.Fatalf("order: %q", v)
	}

	// A failing Write is rolled back and reports its error; so is a panic.
	boom := errors.New("boom")
	err = s.Write(bg, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM kv`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Write error: %v", err)
	}
	err = s.Write(bg, func(tx *sql.Tx) error {
		_, _ = tx.Exec(`DELETE FROM kv`)
		panic("oops")
	})
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("panic not reported: %v", err)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM kv WHERE key IN ('a','b')`); n != 2 {
		t.Fatal("failed transactions must be rolled back")
	}
	// The writer survived both.
	if err := s.KVSet(bg, "c", "3"); err != nil {
		t.Fatal(err)
	}
	if err := s.KVDelete(bg, "c"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.KVGet(bg, "c"); ok {
		t.Fatal("KVDelete")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAutomaticFlushByTimerAndBySize(t *testing.T) {
	// Timer.
	s := openTest(t, Options{FlushInterval: 30 * time.Millisecond})
	s.AddEvents([]Event{{T: 1, Kind: "x"}})
	waitFor(t, "timer flush", func() bool { return scalar[int](t, s, `SELECT COUNT(*) FROM events`) == 1 })

	// Size: the interval is far away, the row threshold triggers the flush.
	s2 := openTest(t, Options{FlushInterval: time.Hour, FlushRows: 50})
	for i := 0; i < 49; i++ {
		s2.AddEvents([]Event{{T: int64(i + 1), Kind: "x"}})
	}
	time.Sleep(50 * time.Millisecond)
	if n := scalar[int](t, s2, `SELECT COUNT(*) FROM events`); n != 0 {
		t.Fatalf("flushed below the threshold: %d", n)
	}
	s2.AddEvents([]Event{{T: 50, Kind: "x"}})
	waitFor(t, "size flush", func() bool { return scalar[int](t, s2, `SELECT COUNT(*) FROM events`) == 50 })
}

func TestQueueOverflowDropsLowValueRowsFirst(t *testing.T) {
	s := openTest(t, Options{FlushInterval: time.Hour, FlushRows: 1000, MaxQueueRows: 1000})

	// Stall the writer so that the queue fills up.
	started, release := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- s.Write(bg, func(tx *sql.Tx) error { close(started); <-release; return nil })
	}()
	<-started

	day := DayOf(1_700_000_000)
	ten := func(base int) []CoverageInc {
		rows := make([]CoverageInc, 10)
		for i := range rows {
			rows[i] = CoverageInc{Day: day, Ant: "", BrgBin: (base*10 + i) % BrgBins, AltBand: (base / 18) % AltBands, MaxKm: 1, Count: 1}
		}
		return rows
	}
	pos := func(base int) []Position {
		rows := make([]Position, 10)
		for i := range rows {
			rows[i] = Position{FlightID: 1, T: int64(base*10 + i + 1), Lat: 50, Lon: 8, Alt: math.NaN(), GS: math.NaN(), Track: math.NaN(), RSSI: math.NaN()}
		}
		return rows
	}
	for i := 0; i < 10; i++ { // 100 high-value rows
		ev := make([]Event, 10)
		for j := range ev {
			ev[j] = Event{T: int64(i*10 + j + 1), Kind: "keep"}
		}
		s.AddEvents(ev)
	}
	for i := 0; i < 60; i++ { // 600 mid-value rows
		s.AddPositions(pos(i))
	}
	for i := 0; i < 100; i++ { // 1000 low-value rows: overflow
		s.MergeCoverage(ten(i))
	}

	st := s.Stats()
	if st.QueuedRows > 1000 {
		t.Fatalf("queue not bounded: %d rows", st.QueuedRows)
	}
	if st.DroppedLow == 0 || st.DroppedMid != 0 || st.DroppedHigh != 0 {
		t.Fatalf("only low-value rows should have been dropped: %+v", st)
	}
	if got := st.DroppedLow + uint64(st.QueuedRows); got != 1700 {
		t.Fatalf("dropped + queued = %d, want 1700", got)
	}

	// Keep pushing mid-value rows: once no low-value rows are left, the
	// oldest positions go, but never the events.
	for i := 60; i < 260; i++ {
		s.AddPositions(pos(i))
	}
	st = s.Stats()
	if st.DroppedMid == 0 || st.DroppedHigh != 0 || st.QueuedRows > 1000 {
		t.Fatalf("after mid overflow: %+v", st)
	}

	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	mustFlush(t, s)
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events WHERE kind = 'keep'`); n != 100 {
		t.Fatalf("high-value rows lost: %d of 100", n)
	}
	// The newest positions survived (oldest are dropped first).
	if n := posCount(t, s, func(p posRow) bool { return p.t == 2600 }); n != 1 {
		t.Fatal("newest position was dropped")
	}
	if n := posCount(t, s, func(p posRow) bool { return p.t == 1 }); n != 0 {
		t.Fatal("oldest position should have been dropped")
	}
}

func TestBadOperationIsIsolated(t *testing.T) {
	s := openTest(t, manual)
	var b Batch
	b.AddEvents([]Event{{T: 1, Kind: "before"}})
	b.add("bad", tierHigh, 1, func(tc *txc) error {
		if err := tc.exec(sqlInsertEvent, 5, "half", nil, nil); err != nil { // must be undone
			return err
		}
		return tc.exec(`INSERT INTO kv(key, value, updated) VALUES(NULL, NULL, NULL)`) // NOT NULL violation
	})
	b.add("panics", tierHigh, 1, func(tc *txc) error { panic("kaboom") })
	b.AddEvents([]Event{{T: 2, Kind: "after"}})
	s.Submit(&b)
	if err := s.Flush(); err != nil {
		t.Fatalf("flush must succeed around bad operations: %v", err)
	}
	kinds := ""
	rows, err := s.DB().QueryContext(bg, `SELECT kind FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		kinds += k + " "
	}
	rows.Close()
	if kinds != "before after " {
		t.Fatalf("events: %q", kinds)
	}
	st := s.Stats()
	if st.FailedOps != 2 || st.QueuedRows != 0 || st.LastError == "" {
		t.Fatalf("stats: %+v", st)
	}

	// Apply (synchronous) is all-or-nothing instead.
	b.AddEvents([]Event{{T: 3, Kind: "apply"}})
	b.add("bad", tierHigh, 1, func(tc *txc) error { return errors.New("nope") })
	if err := s.Apply(bg, &b); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Apply error: %v", err)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events WHERE kind = 'apply'`); n != 0 {
		t.Fatal("failed Apply must write nothing")
	}
}

func TestEnvironmentErrorKeepsRowsQueued(t *testing.T) {
	s := openTest(t, manual)
	var calls atomic.Int32
	var b Batch
	b.AddEvents([]Event{{T: 1, Kind: "first"}})
	b.add("flaky", tierHigh, 1, func(tc *txc) error {
		if calls.Add(1) == 1 {
			return sql.ErrConnDone // looks like a database-level problem
		}
		return tc.exec(sqlInsertEvent, 2, "second", nil, nil)
	})
	s.Submit(&b)
	if err := s.Flush(); err == nil {
		t.Fatal("first flush should report the failure")
	}
	st := s.Stats()
	if st.FlushErrors != 1 || st.QueuedRows != 2 || st.FailedOps != 0 {
		t.Fatalf("rows must stay queued: %+v", st)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events`); n != 0 {
		t.Fatal("failed flush must not commit anything")
	}
	s.AddEvents([]Event{{T: 3, Kind: "third"}})
	mustFlush(t, s)
	var kinds []string
	rows, _ := s.DB().QueryContext(bg, `SELECT kind FROM events ORDER BY id`)
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		kinds = append(kinds, k)
	}
	rows.Close()
	if strings.Join(kinds, ",") != "first,second,third" {
		t.Fatalf("retry lost rows or order: %v", kinds)
	}
}

func TestCloseFlushesAndRejectsLaterWrites(t *testing.T) {
	dir := t.TempDir()
	s := openDir(t, dir, manual)
	s.AddEvents([]Event{{T: 1, Kind: "pending"}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := s.Write(bg, func(*sql.Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
	if err := s.Flush(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Flush after Close: %v", err)
	}
	if err := s.Reset(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reset after Close: %v", err)
	}
	s.AddEvents([]Event{{T: 2, Kind: "late"}}) // discarded, must not panic
	// Close checkpointed: the WAL is empty or gone.
	if fi, err := os.Stat(filepath.Join(dir, FileName+"-wal")); err == nil && fi.Size() != 0 {
		t.Fatalf("WAL not truncated by Close: %d bytes", fi.Size())
	}
	s2 := openDir(t, dir, manual)
	if n := scalar[int](t, s2, `SELECT COUNT(*) FROM events WHERE kind = 'pending'`); n != 1 {
		t.Fatal("Close did not flush the queue")
	}
	if n := scalar[int](t, s2, `SELECT COUNT(*) FROM events`); n != 1 {
		t.Fatal("rows queued after Close must be discarded")
	}
}

// ------------------------------------------------------------ UPSERT merges

func TestAircraftUpsertMerge(t *testing.T) {
	s := openTest(t, manual)
	s.UpsertAircraft([]AircraftSeen{
		{ICAO: 0x4CA2D1, FirstSeen: 1000, LastSeen: 1100, Callsign: "RYR1AB", Country: "Ireland", Category: "A3",
			Msgs: 500, Flights: 1, Positions: 40, MaxDistKm: 120.5, MaxAlt: math.NaN(), SrcMask: MaskADSB},
		{ICAO: 0}, // ignored
	})
	s.UpsertAircraft([]AircraftSeen{
		{ICAO: 0x4CA2D1, LastSeen: 2000, Callsign: "", Reg: "EI-DCL", Msgs: 250, Flights: 1, Positions: 10,
			MaxDistKm: 80, MaxAlt: 38000, SrcMask: MaskMLAT, Flags: FlagInteresting},
		{ICAO: 0x4CA2D1, FirstSeen: 900, LastSeen: 950, Callsign: "RYR9ZZ", Msgs: 1, MaxDistKm: math.NaN(), MaxAlt: 36000},
	})
	mustFlush(t, s)

	a, ok, err := s.AircraftByICAO(bg, 0x4CA2D1)
	if err != nil || !ok {
		t.Fatalf("lookup: %v %v", ok, err)
	}
	if a.FirstSeen != 900 || a.LastSeen != 2000 {
		t.Errorf("seen: %d..%d", a.FirstSeen, a.LastSeen)
	}
	if a.Callsign != "RYR9ZZ" || a.Reg != "EI-DCL" || a.Country != "Ireland" || a.Category != "A3" {
		t.Errorf("text merge: %+v", a)
	}
	if a.Msgs != 751 || a.Flights != 2 || a.Positions != 50 {
		t.Errorf("counters: %+v", a)
	}
	if a.MaxDistKm != 120.5 || a.MaxAlt != 38000 || a.SrcMask != MaskADSB|MaskMLAT || a.Flags != FlagInteresting {
		t.Errorf("maxima / masks: %+v", a)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM aircraft`); n != 1 {
		t.Fatalf("rows: %d", n)
	}
	if _, ok, err := s.AircraftByICAO(bg, 0x123456); ok || err != nil {
		t.Fatalf("unknown aircraft: %v %v", ok, err)
	}

	// Joined with acdb: reference data fills what the tracker never learned,
	// but never overrides it; flags are OR-ed.
	if _, err := s.ImportACDB(bg, false, func(yield func(ACDBEntry) bool) {
		yield(ACDBEntry{ICAO: 0x4CA2D1, Reg: "EI-OLD", Type: "B738", Desc: "BOEING 737-800", Operator: "Ryanair", Flags: FlagMilitary, Year: 2005})
	}); err != nil {
		t.Fatal(err)
	}
	a, _, _ = s.AircraftByICAO(bg, 0x4CA2D1)
	if a.Reg != "EI-DCL" || a.Type != "B738" || a.TypeDesc != "BOEING 737-800" || a.Operator != "Ryanair" ||
		a.Flags != FlagInteresting|FlagMilitary || a.Year != 2005 {
		t.Fatalf("acdb join: %+v", a)
	}
}

func TestFlightUpsertAndPositions(t *testing.T) {
	s := openTest(t, manual)
	id1, id2 := s.NextFlightID(), s.NextFlightID()
	if id1 != 1 || id2 != 2 {
		t.Fatalf("flight ids: %d %d", id1, id2)
	}
	f := NewFlight(id1, 0xABCDEF, 5000)
	f.Callsign, f.Msgs = "DLH123", 10
	s.UpsertFlights([]Flight{f, {ID: 0, ICAO: 1}}) // ID 0 ignored
	mustFlush(t, s)

	got, ok, err := s.FlightByID(bg, id1)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if got.Callsign != "DLH123" || got.End != 5000 || !math.IsNaN(got.MaxDistKm) || !math.IsNaN(got.FirstLat) || !math.IsNaN(got.RSSIAvg) {
		t.Fatalf("open flight: %+v", got)
	}

	// Update / close: the same ID replaces the row.
	f.End, f.Msgs, f.Positions, f.Squawk, f.Emergency = 5600, 900, 3, "7700", "general"
	f.MaxDistKm, f.MaxDistBrg, f.MinDistKm, f.MinAlt, f.MaxAlt, f.MaxGS = 211.25, 271, 12.5, 2000, 36000, 455
	f.RSSIMin, f.RSSIMax, f.RSSIAvg = -31.5, -4, -18.25
	f.FirstLat, f.FirstLon, f.LastLat, f.LastLon = 50.1, 8.5, 51.2, 6.9
	f.SrcMask = MaskADSB
	s.UpsertFlights([]Flight{f})
	s.AddPositions([]Position{
		{FlightID: id1, T: 5010, Lat: 50.123456, Lon: 8.654321, Alt: 12025.4, GS: 300.6, Track: 271.4, RSSI: -12.34, Src: SrcADSB},
		{FlightID: id1, T: 5020, Lat: 50.2, Lon: -8.7, Alt: math.NaN(), GS: math.NaN(), Track: math.NaN(), RSSI: math.NaN(), Src: SrcMLAT},
		{FlightID: id1, T: 5020, Lat: 50.3, Lon: -8.8, Alt: 100, GS: math.NaN(), Track: math.NaN(), RSSI: math.NaN(), Src: SrcMLAT}, // replaces
		{FlightID: id1, T: 5030, Lat: math.NaN(), Lon: 1}, // ignored
		{FlightID: 0, T: 5030, Lat: 1, Lon: 1},            // ignored
	})
	s.UpsertAircraft([]AircraftSeen{{ICAO: 0xABCDEF, LastSeen: 5600, Reg: "D-AIAB", Country: "Germany"}})
	mustFlush(t, s)

	got, _, _ = s.FlightByID(bg, id1)
	if got.End != 5600 || got.Msgs != 900 || got.Squawk != "7700" || got.Emergency != "general" || got.MaxDistKm != 211.25 ||
		got.MinAlt != 2000 || got.RSSIAvg != -18.25 || got.LastLon != 6.9 || got.Reg != "D-AIAB" || got.Country != "Germany" {
		t.Fatalf("closed flight: %+v", got)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM flights`); n != 1 {
		t.Fatalf("flight rows: %d", n)
	}

	ps, err := s.Positions(bg, id1, 0, 0)
	if err != nil || len(ps) != 2 {
		t.Fatalf("positions: %v %+v", err, ps)
	}
	p := ps[0]
	if !near(p.Lat, 50.12346) || !near(p.Lon, 8.65432) || p.Alt != 12025 || p.GS != 301 || p.Track != 271 || !near(p.RSSI, -12.3) || p.Src != SrcADSB {
		t.Fatalf("stored resolution: %+v", p)
	}
	if q := ps[1]; !near(q.Lat, 50.3) || q.Alt != 100 || !math.IsNaN(q.GS) || !math.IsNaN(q.RSSI) || q.Src != SrcMLAT {
		t.Fatalf("replaced / NULL fields: %+v", q)
	}
	if ps, _ := s.Positions(bg, id1, 5015, 5025); len(ps) != 1 || ps[0].T != 5020 {
		t.Fatalf("time window: %+v", ps)
	}
	trail, err := s.Trail(bg, id1)
	if err != nil || len(trail) != 2 || trail[0].T != 5010_000 || trail[1].Src != SrcMLAT {
		t.Fatalf("trail: %v %+v", err, trail)
	}
	if rt := PositionFromTrail(7, trail[0], 90); rt.T != 5010 || rt.FlightID != 7 || rt.Track != 90 || rt.Lat != trail[0].Lat {
		t.Fatalf("PositionFromTrail: %+v", rt)
	}

	var seen int
	err = s.ForEachPosition(bg, 5000, 5015, func(icao uint32, p Position) error {
		seen++
		if icao != 0xABCDEF || p.T != 5010 {
			t.Errorf("ForEachPosition: %06X %+v", icao, p)
		}
		return nil
	})
	if err != nil || seen != 1 {
		t.Fatalf("ForEachPosition: %v, %d rows", err, seen)
	}

	// Flight IDs continue after a reopen.
	dir := s.Dir()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openDir(t, dir, manual)
	if id := s2.NextFlightID(); id != 2 {
		t.Fatalf("next flight id after reopen: %d", id)
	}
}

func TestMinuteStatsMergeAndRollup(t *testing.T) {
	s := openTest(t, manual)
	const h0 = int64(1_699_999_200) // a full hour
	if h0%3600 != 0 {
		t.Fatal("test bug")
	}
	h1 := h0 + 3600

	// One minute written in two parts merges.
	s.AddMinuteStats([]StatsRow{{T: h0 + 65, Ant: "x", Frames: 100, DF17: 60, CRCBad: 1, Positions: 5, Aircraft: 4, AircraftPos: 2,
		MaxKm: 100, RSSIAvg: -20, RSSIMin: -30, RSSIMax: -5}})
	s.AddMinuteStats([]StatsRow{{T: h0 + 60, Ant: "x", Frames: 300, DF17: 40, DF11: 7, Positions: 1, Aircraft: 9, AircraftPos: 1,
		MaxKm: math.NaN(), RSSIAvg: -10, RSSIMin: -25, RSSIMax: -8}})
	s.AddMinuteStats([]StatsRow{{T: h0 + 60, Ant: "x", Frames: 0, Aircraft: 1, MaxKm: 150, RSSIAvg: math.NaN(), RSSIMin: math.NaN(), RSSIMax: math.NaN()}})
	mustFlush(t, s)
	ms, err := s.MinuteStats(bg, "x", h0, h0+3599)
	if err != nil || len(ms) != 1 {
		t.Fatalf("minute rows: %v %+v", err, ms)
	}
	m := ms[0]
	if m.T != h0+60 || m.Frames != 400 || m.DF17 != 100 || m.DF11 != 7 || m.CRCBad != 1 || m.Positions != 6 ||
		m.Aircraft != 9 || m.AircraftPos != 2 || m.MaxKm != 150 || !near(m.RSSIAvg, -12.5) || m.RSSIMin != -30 || m.RSSIMax != -5 {
		t.Fatalf("merged minute: %+v", m)
	}

	// Two antennas, hour h0 complete, hour h1 half way.
	var rows []StatsRow
	for _, ant := range []string{"", "yagi"} {
		for i := int64(0); i < 90; i++ {
			rows = append(rows, StatsRow{T: h0 + i*60, Ant: ant, Frames: 100, DF17: 60, DF11: 40, Positions: 10, MLATPositions: 1,
				Aircraft: int(i%10) + 1, AircraftPos: 1, ADSB: 1, MaxKm: float64(i), RSSIAvg: -20, RSSIMin: -30 - float64(i), RSSIMax: -5})
		}
	}
	s.AddMinuteStats(rows)
	n, err := s.RollupHours(bg) // flushes the queue first
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 { // ("",h0) ("",h1) (yagi,h0) (yagi,h1) (x,h0)
		t.Fatalf("hour rows written: %d", n)
	}
	hs, err := s.HourStats(bg, "yagi", 0, 0)
	if err != nil || len(hs) != 2 {
		t.Fatalf("hour rows: %v %+v", err, hs)
	}
	a, b := hs[0], hs[1]
	if a.T != h0 || a.Frames != 6000 || a.DF17 != 3600 || a.DF11 != 2400 || a.Positions != 600 || a.MLATPositions != 60 ||
		a.Aircraft != 10 || a.MaxKm != 59 || a.RSSIMin != -89 || a.RSSIMax != -5 || !near(a.RSSIAvg, -20) ||
		a.Minutes != 60 || !near(a.AircraftAvg, 5.5) {
		t.Fatalf("hour 0: %+v", a)
	}
	if b.T != h1 || b.Frames != 3000 || b.Minutes != 30 || b.MaxKm != 89 {
		t.Fatalf("hour 1: %+v", b)
	}
	// Frame-weighted mean signal for the merged antenna.
	if x, _ := s.HourStats(bg, "x", 0, 0); len(x) != 1 || !near(x[0].RSSIAvg, -12.5) || x[0].Minutes != 1 {
		t.Fatalf("antenna x: %+v", x)
	}
	if mark := scalar[string](t, s, `SELECT value FROM kv WHERE key = ?`, kvRollupHour); mark != fmt.Sprint(h1) {
		t.Fatalf("rollup mark %s, want %d", mark, h1)
	}

	// Idempotent: a second run only recomputes the open hour, with the same result.
	dump := func() string {
		var sb strings.Builder
		rs, err := s.DB().QueryContext(bg, `SELECT * FROM stats_hour ORDER BY t, ant`)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		cols, _ := rs.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rs.Next() {
			_ = rs.Scan(ptrs...)
			fmt.Fprintln(&sb, vals...)
		}
		return sb.String()
	}
	before := dump()
	n, err = s.RollupHours(bg)
	if err != nil || n != 2 {
		t.Fatalf("second rollup: %d rows, %v", n, err)
	}
	if dump() != before {
		t.Fatal("rollup is not idempotent")
	}

	// A late minute for the finished hour re-opens it.
	s.AddMinuteStats([]StatsRow{{T: h0 + 300, Ant: "yagi", Frames: 100, Aircraft: 50, MaxKm: 400, RSSIAvg: -20, RSSIMin: -20, RSSIMax: -20}})
	if _, err := s.RollupHours(bg); err != nil {
		t.Fatal(err)
	}
	hs, _ = s.HourStats(bg, "yagi", h0, h0)
	if len(hs) != 1 || hs[0].Frames != 6100 || hs[0].Aircraft != 50 || hs[0].MaxKm != 400 || hs[0].Minutes != 60 {
		t.Fatalf("late minute not rolled up: %+v", hs)
	}
	if mark := scalar[string](t, s, `SELECT value FROM kv WHERE key = ?`, kvRollupHour); mark != fmt.Sprint(h1) {
		t.Fatalf("mark after late row: %s", mark)
	}

	// A later hour appears after a gap: the gap is skipped, the mark moves on.
	s.AddMinuteStats([]StatsRow{{T: h0 + 100*3600 + 120, Ant: "", Frames: 1, MaxKm: math.NaN(), RSSIAvg: math.NaN(), RSSIMin: math.NaN(), RSSIMax: math.NaN()}})
	if n, err = s.RollupHours(bg); err != nil || n != 3 { // h1 x2 antennas + the new hour
		t.Fatalf("rollup over a gap: %d rows, %v", n, err)
	}
	last, _ := s.HourStats(bg, "", h0+100*3600, 0)
	if len(last) != 1 || last[0].Frames != 1 || !math.IsNaN(last[0].MaxKm) || !math.IsNaN(last[0].RSSIAvg) {
		t.Fatalf("sparse hour: %+v", last)
	}
	if n, _ := s.RollupHours(bg); n != 1 {
		t.Fatalf("steady state should recompute only the open hour: %d", n)
	}
}

func TestRollupBacklogIsChunked(t *testing.T) {
	s := openTest(t, manual)
	const h0 = int64(1_699_999_200)
	hours := rollupChunkHours*2 + 5
	rows := make([]StatsRow, 0, hours)
	for i := 0; i < hours; i++ {
		rows = append(rows, StatsRow{T: h0 + int64(i)*3600 + 600, Ant: "", Frames: int64(i + 1), MaxKm: math.NaN(), RSSIAvg: math.NaN(), RSSIMin: math.NaN(), RSSIMax: math.NaN()})
	}
	s.AddMinuteStats(rows)
	n, err := s.RollupHours(bg)
	if err != nil || n != hours {
		t.Fatalf("rollup: %d of %d hours, %v", n, hours, err)
	}
	if sum := scalar[int64](t, s, `SELECT SUM(frames) FROM stats_hour`); sum != int64(hours*(hours+1)/2) {
		t.Fatalf("sum of frames: %d", sum)
	}
}

func TestHistogramAndGridMerges(t *testing.T) {
	s := openTest(t, manual)
	day := DayOf(1_700_000_000)
	for pass := 0; pass < 2; pass++ {
		b := NewBatch()
		b.MergeCoverage([]CoverageInc{
			{Day: day, Ant: "yagi", BrgBin: 45, AltBand: 5, MaxKm: 300 - float64(pass)*100, Count: 10, RSSISum: -200, RSSIN: 10},
			{Day: day, Ant: "yagi", BrgBin: 46, AltBand: 5, MaxKm: 100 + float64(pass)*50, Count: 1, RSSISum: -20, RSSIN: 1},
		})
		b.MergeSignalHist([]SignalHistInc{{Day: day, Ant: "", DistBin: 12, RSSIBin: -23, Count: 7}})
		b.MergeElevHist([]ElevHistInc{{Day: day, Ant: "", ElevBin: 10, Count: 3, RSSISum: -60, ResidSum: 90}})
		b.MergeHeat([]HeatInc{
			{Cell: HeatCell(50.03, 8.57), Ant: "", Count: 4, RSSISum: -80, RSSIN: 4, MinAlt: 3000 + float64(pass)*1000, MaxAlt: 9000 - float64(pass)*1000, LastT: 1000 + int64(pass)},
			{Cell: HeatCell(1, 1), Ant: "", Count: 1, MinAlt: math.NaN(), MaxAlt: math.NaN(), LastT: 5},
		})
		b.MergeMetProfile([]MetProfileInc{{Hour: 1_700_000_000, Level: 35, USum: 10, VSum: -5, TSum: -50, NWind: 2, NTemp: 1}})
		s.Submit(b)
	}
	// A heat cell first seen without altitude later learns one.
	s.MergeHeat([]HeatInc{{Cell: HeatCell(1, 1), Ant: "", Count: 1, MinAlt: 500, MaxAlt: 700, LastT: 3}})
	mustFlush(t, s)

	var maxKm, rssiSum float64
	var count, rssiN int64
	if err := s.DB().QueryRow(`SELECT max_km, count, rssi_sum, rssi_n FROM coverage WHERE day = ? AND ant = 'yagi' AND brg_bin = 45 AND alt_band = 5`, day).
		Scan(&maxKm, &count, &rssiSum, &rssiN); err != nil {
		t.Fatal(err)
	}
	if maxKm != 300 || count != 20 || rssiSum != -400 || rssiN != 20 {
		t.Fatalf("coverage: %v %v %v %v", maxKm, count, rssiSum, rssiN)
	}
	if v := scalar[float64](t, s, `SELECT max_km FROM coverage WHERE brg_bin = 46`); v != 150 {
		t.Fatalf("coverage max grows: %v", v)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM coverage`); n != 2 {
		t.Fatalf("coverage rows: %d", n)
	}
	if c := scalar[int64](t, s, `SELECT count FROM signal_hist WHERE dist_bin = 12 AND rssi_bin = -23`); c != 14 {
		t.Fatalf("signal_hist: %d", c)
	}
	var es, er float64
	if err := s.DB().QueryRow(`SELECT count, rssi_sum, resid_sum FROM elev_hist WHERE elev_bin = 10`).Scan(&count, &es, &er); err != nil {
		t.Fatal(err)
	}
	if count != 6 || es != -120 || er != 180 {
		t.Fatalf("elev_hist: %v %v %v", count, es, er)
	}
	var minAlt, maxAlt, lastT int64
	if err := s.DB().QueryRow(`SELECT count, rssi_sum, rssi_n, min_alt, max_alt, last_t FROM heat WHERE cell = ?`, HeatCell(50.03, 8.57)).
		Scan(&count, &rssiSum, &rssiN, &minAlt, &maxAlt, &lastT); err != nil {
		t.Fatal(err)
	}
	if count != 8 || rssiSum != -160 || rssiN != 8 || minAlt != 3000 || maxAlt != 9000 || lastT != 1001 {
		t.Fatalf("heat: %v %v %v %v %v %v", count, rssiSum, rssiN, minAlt, maxAlt, lastT)
	}
	if err := s.DB().QueryRow(`SELECT count, min_alt, max_alt, last_t FROM heat WHERE cell = ?`, HeatCell(1, 1)).
		Scan(&count, &minAlt, &maxAlt, &lastT); err != nil {
		t.Fatal(err)
	}
	if count != 3 || minAlt != 500 || maxAlt != 700 || lastT != 5 {
		t.Fatalf("heat NULL merge: %v %v %v %v", count, minAlt, maxAlt, lastT)
	}
	var u, v, ts float64
	var nw, nt, hour int64
	if err := s.DB().QueryRow(`SELECT hour, u_sum, v_sum, t_sum, n_wind, n_temp FROM met_profile WHERE level = 35`).
		Scan(&hour, &u, &v, &ts, &nw, &nt); err != nil {
		t.Fatal(err)
	}
	if hour != 1_699_999_200 || u != 20 || v != -10 || ts != -100 || nw != 4 || nt != 2 {
		t.Fatalf("met_profile: %v %v %v %v %v %v", hour, u, v, ts, nw, nt)
	}
}

func TestRangeHourMerge(t *testing.T) {
	s := openTest(t, manual)
	const h = int64(1_699_999_200)
	hist1 := make([]uint32, DistHistBins)
	hist1[2], hist1[10] = 90, 10 // 90 positions at 10-15 km, 10 at 50-55 km
	sect1 := make([]float32, SectorBins)
	sect1[3], sect1[4] = 55, 20
	s.AddRangeHours([]RangeHour{{T: h + 77, Ant: "yagi", MaxKm: 54.5, P99Km: 54, P95Km: 52, P90Km: 15, P50Km: 12,
		Positions: 100, Aircraft: 8, FarCount: 0, ResidDB: 30, ResidN: 100, DistHist: hist1, SectorMax: sect1}})
	hist1[2] = 12345 // the caller's buffers may be reused at once
	mustFlush(t, s)

	hist2 := make([]uint32, DistHistBins)
	hist2[40] = 100 // 100 positions at 200-205 km
	sect2 := make([]float32, SectorBins)
	sect2[4], sect2[35] = 204, 90
	s.AddRangeHours([]RangeHour{{T: h, Ant: "yagi", MaxKm: 204, P99Km: 204, P95Km: 204, P90Km: 204, P50Km: 202,
		Positions: 100, Aircraft: 5, FarCount: 100, ResidDB: 40, ResidN: 300, DistHist: hist2, SectorMax: sect2}})
	mustFlush(t, s)

	if n := scalar[int](t, s, `SELECT COUNT(*) FROM range_hour`); n != 1 {
		t.Fatalf("rows: %d", n)
	}
	var r RangeHour
	var hb, sb []byte
	if err := s.DB().QueryRow(`SELECT t, max_km, p99_km, p50_km, positions, aircraft, far_count, resid_db, resid_n, dist_hist, sector_max FROM range_hour`).
		Scan(&r.T, &r.MaxKm, &r.P99Km, &r.P50Km, &r.Positions, &r.Aircraft, &r.FarCount, &r.ResidDB, &r.ResidN, &hb, &sb); err != nil {
		t.Fatal(err)
	}
	if r.T != h || r.MaxKm != 204 || r.Positions != 200 || r.Aircraft != 8 || r.FarCount != 100 || r.ResidN != 400 || !near(r.ResidDB, 37.5) {
		t.Fatalf("merged row: %+v", r)
	}
	if len(DecodeDistHist(hb)) != DistHistBins || len(DecodeSectorMax(sb)) != SectorBins {
		t.Fatalf("blob sizes: %d %d", len(hb), len(sb))
	}
	hist, sect := DecodeDistHist(hb), DecodeSectorMax(sb)
	if hist[2] != 90 || hist[10] != 10 || hist[40] != 100 {
		t.Fatalf("histogram: %v", hist[:45])
	}
	if sect[3] != 55 || sect[4] != 204 || sect[35] != 90 {
		t.Fatalf("sectors: %v", sect)
	}
	// Percentiles recomputed from the merged histogram: the median of 200
	// positions is the 100th, the last one of the 50-55 km bin.
	if !near(r.P50Km, 55) || r.P99Km < 200 || r.P99Km > 204 {
		t.Fatalf("percentiles: p50 %v p99 %v", r.P50Km, r.P99Km)
	}
}

func TestHistPercentileAndBlobCodecs(t *testing.T) {
	h := []uint32{0, 10, 0, 10}
	if v := HistPercentile(h, 5, 0.5); !near(v, 10) {
		t.Fatalf("p50 = %v", v)
	}
	if v := HistPercentile(h, 5, 0.25); !near(v, 7.5) {
		t.Fatalf("p25 = %v", v)
	}
	if v := HistPercentile(h, 5, 1); !near(v, 20) {
		t.Fatalf("p100 = %v", v)
	}
	if !math.IsNaN(HistPercentile(nil, 5, 0.5)) || !math.IsNaN(HistPercentile(make([]uint32, 9), 5, 0.5)) {
		t.Fatal("empty histogram must give NaN")
	}
	if EncodeDistHist(nil) != nil || DecodeDistHist(nil) != nil || EncodeSectorMax(nil) != nil || DecodeSectorMax([]byte{1, 2}) != nil {
		t.Fatal("empty codecs")
	}
	in := []uint32{0, 1, 0xDEADBEEF}
	enc := EncodeDistHist(in)
	if len(enc) != 12 || enc[8] != 0xEF || enc[11] != 0xDE {
		t.Fatalf("not little-endian: % x", enc)
	}
	if out := DecodeDistHist(enc); len(out) != 3 || out[2] != 0xDEADBEEF {
		t.Fatalf("round trip: %v", out)
	}
	if out := DecodeSectorMax(EncodeSectorMax([]float32{1.5, -2})); len(out) != 2 || out[0] != 1.5 || out[1] != -2 {
		t.Fatalf("sector round trip: %v", out)
	}
}

func TestRecordsOnlyIfBetter(t *testing.T) {
	s := openTest(t, manual)
	s.SetRecord(Record{Key: "max_dist_km", Value: 300, ICAO: 0x111111, T: 10, Detail: `{"a":1}`}, RecordMax)
	s.SetRecord(Record{Key: "max_dist_km", Value: 250, ICAO: 0x222222, T: 20}, RecordMax) // worse
	s.SetRecord(Record{Key: "max_dist_km", Value: 300, ICAO: 0x333333, T: 30}, RecordMax) // equal: first one stays
	s.SetRecord(Record{Key: "min_rssi", Value: -30, ICAO: 0x111111, T: 10}, RecordMin)
	s.SetRecord(Record{Key: "min_rssi", Value: -35, ICAO: 0x222222, T: 20}, RecordMin) // better
	s.SetRecord(Record{Key: "min_rssi", Value: -31, ICAO: 0x333333, T: 30}, RecordMin) // worse
	s.SetRecord(Record{Key: "last", Value: 5, T: 10}, RecordAlways)
	s.SetRecord(Record{Key: "last", Value: 1, T: 20}, RecordAlways)
	s.SetRecord(Record{Key: "", Value: 1}, RecordMax)             // ignored
	s.SetRecord(Record{Key: "nan", Value: math.NaN()}, RecordMax) // ignored
	mustFlush(t, s)

	recs, err := s.Records(bg)
	if err != nil || len(recs) != 3 {
		t.Fatalf("records: %v %+v", err, recs)
	}
	byKey := map[string]Record{}
	for _, r := range recs {
		byKey[r.Key] = r
	}
	if r := byKey["max_dist_km"]; r.Value != 300 || r.ICAO != 0x111111 || r.T != 10 || r.Detail != `{"a":1}` {
		t.Fatalf("max record: %+v", r)
	}
	if r := byKey["min_rssi"]; r.Value != -35 || r.ICAO != 0x222222 || r.T != 20 {
		t.Fatalf("min record: %+v", r)
	}
	if r := byKey["last"]; r.Value != 1 || r.ICAO != 0 || r.T != 20 {
		t.Fatalf("always record: %+v", r)
	}
	if r, ok, err := s.RecordByKey(bg, "min_rssi"); err != nil || !ok || r.Value != -35 {
		t.Fatalf("RecordByKey: %+v %v %v", r, ok, err)
	}
	if _, ok, _ := s.RecordByKey(bg, "none"); ok {
		t.Fatal("unknown record")
	}
}

func TestWeatherNTPMetReceiverStatus(t *testing.T) {
	s := openTest(t, manual)
	const h = int64(1_699_999_200)

	w := NewWeather(h)
	w.TempC, w.PressureMSL, w.WindKmh, w.WeatherCode, w.IsDay = 12.5, 1013.2, 20, 61, 1
	s.AddWeatherObs([]Weather{w})
	// A second source fills in the refractivity columns without erasing the rest.
	w2 := NewWeather(h)
	w2.NSurface, w2.DNDH, w2.KFactor, w2.Duct, w2.ProfileJSON = 320, -45, 1.4, 0, `{"levels":[1000,925]}`
	w2.TempC = 13
	s.AddWeatherObs([]Weather{w2})

	fc := NewWeather(h + 7200)
	fc.Issued, fc.TempC = h, 9
	fc2 := fc
	fc2.Issued, fc2.TempC, fc2.RH = h+3600, 10, 80
	s.SetWeatherForecast([]Weather{fc})
	s.SetWeatherForecast([]Weather{fc2})        // newer issue replaces
	s.AddWeatherForecastLog([]Weather{fc, fc2}) // leads 2 and 1
	stale := NewWeather(h)                      // issued after the target hour: ignored
	stale.Issued = h + 7200
	s.AddWeatherForecastLog([]Weather{stale, fc}) // same (t, lead) again: replaced, not duplicated

	s.AddNTPSamples([]NTPSample{{T: 100, Server: "pool", OffsetMs: 1.5, DelayMs: 20, Stratum: 2}, {T: 100, Server: "pool", OffsetMs: 2.5, DelayMs: math.NaN(), Stratum: 2}})
	s.AddMetObs([]MetObs{{T: 100, ICAO: 0xABCDEF, Lat: 50, Lon: 8, Alt: 35000.4, WindDir: 270, WindKt: 80, OATC: math.NaN(), Src: 1}})
	s.AddReceiverStatus([]ReceiverStatus{{T: 100, Feed: "radarcape", JSON: `{"gps":true}`}, {T: 100, Feed: "radarcape", JSON: `{"gps":false}`}})
	mustFlush(t, s)

	obs, err := s.WeatherObs(bg, 0, 0)
	if err != nil || len(obs) != 1 {
		t.Fatalf("obs: %v %+v", err, obs)
	}
	o := obs[0]
	if o.TempC != 13 || o.PressureMSL != 1013.2 || o.WindKmh != 20 || o.WeatherCode != 61 || o.IsDay != 1 ||
		o.NSurface != 320 || o.DNDH != -45 || o.KFactor != 1.4 || o.Duct != 0 || o.ProfileJSON != `{"levels":[1000,925]}` ||
		!math.IsNaN(o.RH) || !math.IsNaN(o.CAPE) {
		t.Fatalf("merged observation: %+v", o)
	}
	fcs, err := s.WeatherForecast(bg, h, 0)
	if err != nil || len(fcs) != 1 || fcs[0].Issued != h+3600 || fcs[0].TempC != 10 || fcs[0].RH != 80 {
		t.Fatalf("forecast: %v %+v", err, fcs)
	}
	logs, err := s.WeatherForecastLog(bg, 0, 0)
	if err != nil || len(logs) != 2 || logs[0].LeadH != 1 || logs[0].TempC != 10 || logs[1].LeadH != 2 || logs[1].TempC != 9 || logs[1].Issued != h {
		t.Fatalf("forecast log: %v %+v", err, logs)
	}
	if off := scalar[float64](t, s, `SELECT offset_ms FROM ntp_samples WHERE t = 100`); off != 2.5 {
		t.Fatalf("ntp: %v", off)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM ntp_samples WHERE delay_ms IS NULL`); n != 1 {
		t.Fatal("NaN must be stored as NULL")
	}
	// met_obs is a logical table since 1.1 (met_sample + legacy rows).
	var alt any
	for _, r := range metObsRows(t, s) {
		if r[1] == int64(0xABCDEF) && r[7] == nil && r[8] == int64(1) {
			alt = r[4]
		}
	}
	if alt != int64(35000) {
		t.Fatalf("met_obs: %v", alt)
	}
	if j := scalar[string](t, s, `SELECT json FROM receiver_status`); j != `{"gps":false}` {
		t.Fatalf("receiver_status: %s", j)
	}
}

// metObsRows returns the rows of the logical met_obs table (1.0.0 columns
// t, icao, lat, lon, alt, wind_dir, wind_kt, oat_c, src), whatever layout
// stores them.
func metObsRows(t *testing.T, s *Store) [][]any {
	t.Helper()
	var rows [][]any
	err := ScanTableDB(bg, s.DB(), KindMain, "met_obs", "", nil, 0, func([]string) error { return nil }, func(v []any) error {
		rows = append(rows, append([]any(nil), v...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// ---------------------------------------------------------------- read API

func TestListAircraftSearchSortPaging(t *testing.T) {
	s := openTest(t, manual)
	var seen []AircraftSeen
	for i := 0; i < 30; i++ {
		seen = append(seen, AircraftSeen{
			ICAO: uint32(0x400000 + i), FirstSeen: 1000, LastSeen: int64(2000 + i),
			Callsign: fmt.Sprintf("BAW%03d", i), Country: "United Kingdom", Msgs: int64(100 - i), MaxDistKm: float64(i), MaxAlt: math.NaN(),
		})
	}
	seen = append(seen,
		AircraftSeen{ICAO: 0x3C6444, LastSeen: 5000, Callsign: "DLH4AB", Reg: "D-AIBD", Type: "A319", Operator: "Lufthansa", MaxAlt: 39000},
		AircraftSeen{ICAO: 0xA00001, LastSeen: 100, Callsign: "N1", Reg: "N1", MaxAlt: math.NaN()},
		AircraftSeen{ICAO: 0x00B0B0, LastSeen: 200, Callsign: "100%_X", MaxAlt: math.NaN()},
	)
	s.UpsertAircraft(seen)
	mustFlush(t, s)
	if _, err := s.ImportACDB(bg, false, func(yield func(ACDBEntry) bool) {
		_ = yield(ACDBEntry{ICAO: 0x400003, Reg: "G-EUUA", Type: "A320", Desc: "AIRBUS A-320", Operator: "British Airways"}) &&
			yield(ACDBEntry{ICAO: 0x999999, Reg: "ZZ-NOT-SEEN", Type: "A320"})
	}); err != nil {
		t.Fatal(err)
	}

	// Default: newest first, 100 per page.
	rows, total, err := s.ListAircraft(bg, AircraftQuery{Desc: true})
	if err != nil || total != 33 || len(rows) != 33 || rows[0].ICAO != 0x3C6444 || rows[32].ICAO != 0xA00001 {
		t.Fatalf("default list: %v total=%d len=%d", err, total, len(rows))
	}
	// Paging.
	p1, total, _ := s.ListAircraft(bg, AircraftQuery{Sort: "icao", Limit: 10})
	p2, _, _ := s.ListAircraft(bg, AircraftQuery{Sort: "icao", Limit: 10, Offset: 10})
	if total != 33 || len(p1) != 10 || len(p2) != 10 || p1[0].ICAO != 0x00B0B0 || p1[9].ICAO >= p2[0].ICAO {
		t.Fatalf("paging: %06X..%06X then %06X", p1[0].ICAO, p1[9].ICAO, p2[0].ICAO)
	}
	if last, _, _ := s.ListAircraft(bg, AircraftQuery{Sort: "icao", Limit: 10, Offset: 30}); len(last) != 3 {
		t.Fatalf("last page: %d", len(last))
	}
	// Sort by a numeric column.
	if rows, _, _ := s.ListAircraft(bg, AircraftQuery{Sort: "max_dist_km", Desc: true, Limit: 1}); rows[0].ICAO != 0x40001D {
		t.Fatalf("sort by distance: %06X", rows[0].ICAO)
	}
	// Sort by a column that comes from the acdb join; unknown sort keys fall back.
	if rows, _, _ := s.ListAircraft(bg, AircraftQuery{Sort: "reg", Desc: true, Limit: 1}); rows[0].Reg != "N1" {
		t.Fatalf("sort by reg: %+v", rows[0])
	}
	if rows, _, err := s.ListAircraft(bg, AircraftQuery{Sort: "icao; DROP TABLE aircraft", Limit: 1}); err != nil || len(rows) != 1 {
		t.Fatalf("hostile sort key: %v", err)
	}

	search := func(q string) []uint32 {
		rows, total, err := s.ListAircraft(bg, AircraftQuery{Search: q, Sort: "icao"})
		if err != nil || total != len(rows) {
			t.Fatalf("search %q: %v total=%d len=%d", q, err, total, len(rows))
		}
		var out []uint32
		for _, r := range rows {
			out = append(out, r.ICAO)
		}
		return out
	}
	eq := func(got []uint32, want ...uint32) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if got := search("3c64"); !eq(got, 0x3C6444) { // hex address, case-insensitive
		t.Fatalf("by icao: %06X", got)
	}
	if got := search("baw02"); len(got) != 10 { // callsign substring BAW020..BAW029
		t.Fatalf("by callsign: %d", len(got))
	}
	if got := search("  d-aibd "); !eq(got, 0x3C6444) {
		t.Fatalf("by reg: %06X", got)
	}
	if got := search("lufth"); !eq(got, 0x3C6444) {
		t.Fatalf("by operator: %06X", got)
	}
	if got := search("a319"); !eq(got, 0x3C6444) {
		t.Fatalf("by type: %06X", got)
	}
	if got := search("g-euua"); !eq(got, 0x400003) { // registration known only from acdb
		t.Fatalf("by acdb reg: %06X", got)
	}
	if got := search("british"); !eq(got, 0x400003) {
		t.Fatalf("by acdb operator: %06X", got)
	}
	if got := search("airbus a-320"); !eq(got, 0x400003) {
		t.Fatalf("by acdb description: %06X", got)
	}
	if got := search("zz-not"); len(got) != 0 { // acdb-only aircraft were never seen
		t.Fatalf("unseen aircraft listed: %06X", got)
	}
	if got := search("100%_"); !eq(got, 0x00B0B0) { // LIKE wildcards are literal
		t.Fatalf("wildcard escaping: %06X", got)
	}
	if got := search("%"); !eq(got, 0x00B0B0) {
		t.Fatalf("wildcard escaping: %06X", got)
	}
	if got := search("' OR 1=1 --"); len(got) != 0 {
		t.Fatalf("injection: %06X", got)
	}
	// Time window.
	if rows, total, _ := s.ListAircraft(bg, AircraftQuery{Since: 2025, Until: 2029}); total != 5 || len(rows) != 5 {
		t.Fatalf("since/until: %d", total)
	}

	if e, ok, err := s.ACDBLookup(bg, 0x999999); err != nil || !ok || e.Reg != "ZZ-NOT-SEEN" {
		t.Fatalf("ACDBLookup: %+v %v %v", e, ok, err)
	}
	if _, ok, _ := s.ACDBLookup(bg, 0x111111); ok {
		t.Fatal("ACDBLookup of unknown address")
	}
}

func TestFlightsEventsKVAndCounts(t *testing.T) {
	s := openTest(t, manual)
	var fl []Flight
	for i := 0; i < 12; i++ {
		f := NewFlight(s.NextFlightID(), uint32(0x500000+i%3), int64(1000+i*100))
		f.End = f.Start + 50
		f.Callsign = fmt.Sprintf("TST%d", i)
		fl = append(fl, f)
	}
	s.UpsertFlights(fl)
	s.UpsertAircraft([]AircraftSeen{{ICAO: 0x500000, LastSeen: 2100, Reg: "OE-ABC", MaxAlt: math.NaN()}})
	s.AddEvents([]Event{
		{T: 10, Kind: "feed_up", Detail: `{"feed":"radarcape"}`},
		{T: 20, Kind: "emergency", ICAO: 0x500001, Detail: `{"squawk":"7700"}`},
		{T: 30, Kind: "record", ICAO: 0x500002},
		{T: 40, Kind: "emergency", ICAO: 0x500002},
		{Kind: "now"},
	})
	mustFlush(t, s)

	mine, err := s.FlightsForAircraft(bg, 0x500000, 0, 0)
	if err != nil || len(mine) != 4 || mine[0].Callsign != "TST9" || mine[3].Callsign != "TST0" || mine[0].Reg != "OE-ABC" {
		t.Fatalf("FlightsForAircraft: %v %+v", err, mine)
	}
	if page, _ := s.FlightsForAircraft(bg, 0x500000, 2, 1); len(page) != 2 || page[0].Callsign != "TST6" {
		t.Fatalf("flight paging: %+v", page)
	}
	recent, err := s.RecentFlights(bg, 5, 0)
	if err != nil || len(recent) != 5 || recent[0].Callsign != "TST11" || recent[4].Callsign != "TST7" {
		t.Fatalf("RecentFlights: %v %+v", err, recent)
	}
	if lf, ok, err := s.LatestFlight(bg, 0x500001); err != nil || !ok || lf.Callsign != "TST10" {
		t.Fatalf("LatestFlight: %+v %v %v", lf, ok, err)
	}
	if _, ok, err := s.LatestFlight(bg, 0x123456); ok || err != nil {
		t.Fatalf("LatestFlight of unknown aircraft: %v %v", ok, err)
	}
	if between, err := s.FlightsBetween(bg, 1150, 1310, 0); err != nil || len(between) != 3 || between[0].Callsign != "TST1" {
		t.Fatalf("FlightsBetween: %v %+v", err, between)
	}
	if _, ok, err := s.FlightByID(bg, 9999); ok || err != nil {
		t.Fatalf("FlightByID unknown: %v %v", ok, err)
	}

	ev, err := s.Events(bg, EventQuery{})
	if err != nil || len(ev) != 5 || ev[0].Kind != "now" || ev[4].Kind != "feed_up" {
		t.Fatalf("events: %v %+v", err, ev)
	}
	if now := time.Now().Unix(); ev[0].T < now-60 || ev[0].T > now+60 {
		t.Fatalf("event without a time must get the current time: %d", ev[0].T)
	}
	if ev[4].Detail != `{"feed":"radarcape"}` || ev[4].ICAO != 0 || ev[3].ICAO != 0x500001 {
		t.Fatalf("event fields: %+v", ev)
	}
	if em, _ := s.Events(bg, EventQuery{Kind: "emergency"}); len(em) != 2 || em[0].T != 40 {
		t.Fatalf("by kind: %+v", em)
	}
	if by, _ := s.Events(bg, EventQuery{ICAO: 0x500002}); len(by) != 2 {
		t.Fatalf("by icao: %+v", by)
	}
	if since, _ := s.Events(bg, EventQuery{Since: 30, Limit: 2}); len(since) != 2 || since[0].Kind != "now" {
		t.Fatalf("since/limit: %+v", since)
	}
	if win, _ := s.Events(bg, EventQuery{Since: 30, Until: 41}); len(win) == 0 || win[0].T != 40 {
		t.Fatalf("since/until: %+v", win)
	} else {
		for _, e := range win {
			if e.T < 30 || e.T >= 41 {
				t.Fatalf("since/until: event at %d outside [30, 41)", e.T)
			}
		}
	}
	if older, _ := s.Events(bg, EventQuery{BeforeID: ev[2].ID}); len(older) != 2 || older[0].ID != ev[3].ID {
		t.Fatalf("paging backwards: %+v", older)
	}

	// kv
	if _, ok, err := s.KVGet(bg, "missing"); ok || err != nil {
		t.Fatalf("missing key: %v %v", ok, err)
	}
	if err := s.KVSet(bg, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet(bg, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := s.KVGet(bg, "k"); !ok || v != "v2" {
		t.Fatalf("kv: %q", v)
	}
	if err := s.KVSet(bg, "", "x"); err == nil {
		t.Fatal("empty key must be refused")
	}

	// Sizes and counts.
	counts, err := s.TableCounts(bg)
	if err != nil || counts["flights"] != 12 || counts["events"] != 5 || counts["aircraft"] != 1 || counts["positions"] != 0 {
		t.Fatalf("counts: %v %v", err, counts)
	}
	counts["flights"] = -1 // the cache must not be shared with callers
	s.AddEvents([]Event{{T: 50, Kind: "x"}})
	mustFlush(t, s) // a flush invalidates the cache
	if counts, _ = s.TableCounts(bg); counts["events"] != 6 || counts["flights"] != 12 {
		t.Fatalf("counts after flush: %v", counts)
	}
	if s.DBSize() <= 0 {
		t.Fatal("DBSize")
	}
	st := s.Stats()
	if st.DBBytes <= 0 || st.PageSize <= 0 || st.PageCount <= 0 || st.Path != s.Path() || st.DBBytes+st.WALBytes != s.DBSize() {
		t.Fatalf("stats: %+v", st)
	}
	if _, err := json.Marshal(st); err != nil {
		t.Fatal(err)
	}
}

func TestTableCountsCachePolicy(t *testing.T) {
	s := openTest(t, manual)
	events := func() int64 {
		t.Helper()
		c, err := s.TableCounts(bg)
		if err != nil {
			t.Fatal(err)
		}
		return c["events"]
	}
	insert := func() {
		t.Helper()
		if err := s.Write(bg, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO events(t, kind) VALUES(1, 'x')`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setCache := func(took, age time.Duration) {
		s.counts.mu.Lock()
		s.counts.took, s.counts.at = took, time.Now().Add(-age)
		s.counts.mu.Unlock()
	}

	// Cheap counts: exact after every write (synchronous Write included).
	// (setCache pins the measured cost so a slow test machine cannot turn
	// these counts into "expensive" ones.)
	if n := events(); n != 0 {
		t.Fatalf("empty: %d", n)
	}
	setCache(time.Millisecond, 0)
	insert()
	if n := events(); n != 1 {
		t.Fatalf("after a write: %d", n)
	}
	setCache(time.Millisecond, 0)
	// ... but without a write the cache answers: a row smuggled in behind the
	// store's back is not noticed until the cache expires.
	if _, err := s.cur.Load().wdb.Exec(`INSERT INTO events(t, kind) VALUES(2, 'hidden')`); err != nil {
		t.Fatal(err)
	}
	if n := events(); n != 1 {
		t.Fatalf("cache not used: %d", n)
	}
	setCache(time.Millisecond, countCacheTTL+time.Second)
	if n := events(); n != 2 {
		t.Fatalf("expired cache not refreshed: %d", n)
	}

	// Expensive counts are reused whatever is written, for 50x their cost.
	setCache(2*time.Second, 40*time.Second)
	insert()
	if n := events(); n != 2 {
		t.Fatalf("an expensive count must outlive writes: %d", n)
	}
	setCache(2*time.Second, 101*time.Second)
	if n := events(); n != 3 {
		t.Fatalf("expensive count not refreshed after its lifetime: %d", n)
	}

	// A count in progress never blocks the writer: invalidation (done by
	// every flush) only takes a short mutex, not the counting lock.
	s.counts.run <- struct{}{} // pretend a slow count is running
	done := make(chan struct{})
	go func() {
		defer close(done)
		insert()
		s.AddEvents([]Event{{T: 3, Kind: "y"}})
		_ = s.Flush()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writer blocked behind a running count")
	}
	// A second caller waits for the running count, but honours its context.
	ctx, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	if _, err := s.TableCounts(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting caller: %v", err)
	}
	cancel()
	<-s.counts.run
	if n := events(); n != 5 {
		t.Fatalf("after the count finished: %d", n)
	}

	// Reset drops the cache even when the last count was expensive.
	setCache(time.Hour, 0)
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if n := events(); n != 0 {
		t.Fatalf("after reset: %d", n)
	}
}

func TestFailedFlushBacksOffUntilTheTimer(t *testing.T) {
	// Every enqueue kicks the writer (FlushRows 1); the timer never fires.
	s := openTest(t, Options{FlushInterval: time.Hour, FlushRows: 1})
	var calls atomic.Int32
	var b Batch
	b.add("flaky", tierHigh, 1, func(tc *txc) error {
		if calls.Add(1) == 1 {
			return sql.ErrConnDone // a database-level failure
		}
		return tc.exec(sqlInsertEvent, 1, "flaky", nil, nil)
	})
	s.Submit(&b)
	waitFor(t, "first flush attempt", func() bool { return s.Stats().FlushErrors == 1 })

	// More rows arrive and kick again, but the writer does not spin on the
	// broken database: no further attempt until the timer or a Flush.
	for i := 0; i < 20; i++ {
		s.AddEvents([]Event{{T: int64(10 + i), Kind: "later"}})
	}
	time.Sleep(100 * time.Millisecond)
	if st := s.Stats(); calls.Load() != 1 || st.FlushErrors != 1 || st.QueuedRows != 21 {
		t.Fatalf("writer retried without waiting: calls=%d %+v", calls.Load(), st)
	}
	mustFlush(t, s)
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events`); n != 21 {
		t.Fatalf("rows after the retry: %d", n)
	}
	// Back to normal: size-triggered flushes work again.
	s.AddEvents([]Event{{T: 99, Kind: "normal"}})
	waitFor(t, "size-triggered flush", func() bool { return scalar[int](t, s, `SELECT COUNT(*) FROM events`) == 22 })
}

func TestRangeHoursAndNTPSamplesRead(t *testing.T) {
	s := openTest(t, manual)
	const h = int64(1_699_999_200)
	hist := make([]uint32, DistHistBins)
	hist[3] = 7
	sect := make([]float32, SectorBins)
	sect[9] = 123.4
	s.AddRangeHours([]RangeHour{
		{T: h, Ant: "yagi", MaxKm: 123.4, P99Km: 120, P95Km: 110, P90Km: 100, P50Km: 17, Positions: 7, Aircraft: 2,
			ResidDB: 31.5, ResidN: 7, DistHist: hist, SectorMax: sect},
		{T: h + 3600, Ant: "yagi", MaxKm: math.NaN(), P99Km: math.NaN(), P95Km: math.NaN(), P90Km: math.NaN(),
			P50Km: math.NaN(), ResidDB: math.NaN()}, // an hour without positions
		{T: h, Ant: "", MaxKm: 200, P99Km: 1, P95Km: 1, P90Km: 1, P50Km: 1, ResidDB: math.NaN()},
	})
	s.AddNTPSamples([]NTPSample{
		{T: 100, Server: "pool", OffsetMs: 1, DelayMs: 10, Stratum: 2},
		{T: 164, Server: "pool", OffsetMs: -2, DelayMs: math.NaN(), Stratum: 2},
		{T: 228, Server: "pool", OffsetMs: 3, DelayMs: 12, Stratum: 3},
	})
	mustFlush(t, s)

	rh, err := s.RangeHours(bg, "yagi", 0, 0)
	if err != nil || len(rh) != 2 {
		t.Fatalf("RangeHours: %v %+v", err, rh)
	}
	a, b := rh[0], rh[1]
	if a.T != h || a.Ant != "yagi" || a.MaxKm != 123.4 || a.P50Km != 17 || a.Positions != 7 || a.Aircraft != 2 ||
		a.ResidDB != 31.5 || a.ResidN != 7 || len(a.DistHist) != DistHistBins || a.DistHist[3] != 7 ||
		len(a.SectorMax) != SectorBins || a.SectorMax[9] != 123.4 {
		t.Fatalf("row 0: %+v", a)
	}
	if b.T != h+3600 || !math.IsNaN(b.MaxKm) || !math.IsNaN(b.P99Km) || !math.IsNaN(b.ResidDB) || b.DistHist != nil || b.SectorMax != nil {
		t.Fatalf("row 1: %+v", b)
	}
	if all, _ := s.RangeHours(bg, "", h, h); len(all) != 1 || all[0].MaxKm != 200 {
		t.Fatalf("combined antenna: %+v", all)
	}
	if none, err := s.RangeHours(bg, "yagi", h+7200, 0); err != nil || len(none) != 0 {
		t.Fatalf("empty window: %v %+v", err, none)
	}
	js, err := json.Marshal(rh)
	if err != nil {
		t.Fatalf("range hours with NaN must marshal: %v", err)
	}
	if str := string(js); !strings.Contains(str, `"t":1699999200000`) || !strings.Contains(str, `"maxKm":123.4`) ||
		!strings.Contains(str, `123.4,0`) || strings.Contains(str, "NaN") ||
		!strings.Contains(str, `{"t":1700002800000,"ant":"yagi","positions":0,"aircraft":0,"farCount":0,"residN":0}`) {
		t.Fatalf("range hour JSON: %s", str)
	}

	ns, err := s.NTPSamples(bg, 0, 0, 0)
	if err != nil || len(ns) != 3 || ns[0].T != 100 || ns[2].Stratum != 3 || !math.IsNaN(ns[1].DelayMs) || ns[1].OffsetMs != -2 {
		t.Fatalf("NTPSamples: %v %+v", err, ns)
	}
	if last, _ := s.NTPSamples(bg, 0, 0, 2); len(last) != 2 || last[0].T != 164 || last[1].T != 228 {
		t.Fatalf("newest two, oldest first: %+v", last)
	}
	if win, _ := s.NTPSamples(bg, 101, 200, 10); len(win) != 1 || win[0].T != 164 {
		t.Fatalf("window: %+v", win)
	}
	if js, err = json.Marshal(ns[1]); err != nil || string(js) != `{"t":164000,"server":"pool","offsetMs":-2,"stratum":2}` {
		t.Fatalf("ntp JSON: %v %s", err, js)
	}
}

func TestRowsMarshalToAPIJSON(t *testing.T) {
	f := NewFlight(7, 0x00ABCD, 1000) // every optional value unknown (NaN)
	f.MaxAlt = 35000
	b, err := json.Marshal([]Flight{f})
	if err != nil {
		t.Fatalf("flight with NaN fields must marshal: %v", err)
	}
	js := string(b)
	if !strings.Contains(js, `"icao":"00ABCD"`) || !strings.Contains(js, `"start":1000000`) || !strings.Contains(js, `"maxAlt":35000`) ||
		strings.Contains(js, "maxDistKm") || strings.Contains(js, "NaN") {
		t.Fatalf("flight JSON: %s", js)
	}

	b, err = json.Marshal(AircraftRow{ICAO: 0x4CA2D1, FirstSeen: 1, LastSeen: 2, MaxAlt: math.NaN(), Flags: FlagMilitary})
	if err != nil || !strings.Contains(string(b), `"military":true`) || strings.Contains(string(b), "maxAlt") ||
		!strings.Contains(string(b), `"lastSeen":2000`) {
		t.Fatalf("aircraft JSON: %v %s", err, b)
	}

	b, err = json.Marshal(StatsRow{T: 60, Ant: "", Frames: 5, MaxKm: math.NaN(), RSSIAvg: -3.5, RSSIMin: math.NaN(), RSSIMax: math.Inf(1), AircraftAvg: math.NaN()})
	if err != nil || !strings.Contains(string(b), `"rssiAvg":-3.5`) || strings.Contains(string(b), "maxKm") ||
		strings.Contains(string(b), "rssiMax") || strings.Contains(string(b), "minutes") {
		t.Fatalf("stats JSON: %v %s", err, b)
	}

	w := NewWeather(3600)
	w.TempC, w.ProfileJSON = 4, `{"p":[1000]}`
	b, err = json.Marshal(w)
	if err != nil || string(b) != `{"t":3600000,"tempC":4,"profile":{"p":[1000]}}` {
		t.Fatalf("weather JSON: %v %s", err, b)
	}
	w.Issued, w.T = 3600, 3*3600
	if b, _ = json.Marshal(w); !strings.Contains(string(b), `"leadH":2`) || !strings.Contains(string(b), `"issued":3600000`) {
		t.Fatalf("forecast JSON: %s", b)
	}

	b, err = json.Marshal([]Event{{ID: 1, T: 5, Kind: "record", ICAO: 0xF, Detail: `{"km":412}`}, {ID: 2, T: 6, Kind: "note", Detail: "not json"}})
	if err != nil || string(b) != `[{"id":1,"t":5000,"kind":"record","icao":"00000F","detail":{"km":412}},{"id":2,"t":6000,"kind":"note","detail":"not json"}]` {
		t.Fatalf("event JSON: %v %s", err, b)
	}
	b, err = json.Marshal(Record{Key: "k", Value: 1.5, T: 2})
	if err != nil || string(b) != `{"key":"k","value":1.5,"t":2000}` {
		t.Fatalf("record JSON: %v %s", err, b)
	}
	b, err = json.Marshal(ACDBEntry{ICAO: 0xABCDEF, Reg: "D-ABCD", Desc: "X", Flags: FlagMilitary})
	if err != nil || string(b) != `{"icao":"ABCDEF","reg":"D-ABCD","typeDesc":"X","flags":1,"military":true}` {
		t.Fatalf("acdb JSON: %v %s", err, b)
	}
	if HexICAO(0) != "" || HexICAO(0xabc) != "000ABC" {
		t.Fatal("HexICAO")
	}
}

// ------------------------------------------------------------- acdb import

func TestImportACDB(t *testing.T) {
	s := openTest(t, manual)
	const n = 120_000 // more than two chunks
	gen := func(count int, tag string) func(yield func(ACDBEntry) bool) {
		return func(yield func(ACDBEntry) bool) {
			for i := 1; i <= count; i++ {
				if i == 10 && !yield(ACDBEntry{ICAO: 0, Reg: "SKIPPED"}) {
					return
				}
				if !yield(ACDBEntry{ICAO: uint32(i), Reg: fmt.Sprintf("%s-%d", tag, i), Type: "B738", Desc: "BOEING 737-800", Operator: "Op", Flags: i & 1, Year: 2000 + i%20}) {
					return
				}
			}
		}
	}
	start := time.Now()
	got, err := s.ImportACDB(bg, false, gen(n, "A"))
	if err != nil || got != n {
		t.Fatalf("import: %d %v", got, err)
	}
	t.Logf("imported %d rows in %v", n, time.Since(start))
	if c, err := s.ACDBCount(bg); err != nil || c != n {
		t.Fatalf("count: %d %v", c, err)
	}
	if e, ok, _ := s.ACDBLookup(bg, 77777); !ok || e.Reg != "A-77777" || e.Flags != 1 || e.Year != 2017 || e.Desc != "BOEING 737-800" {
		t.Fatalf("lookup: %+v", e)
	}

	// Import without replace updates existing entries and keeps the others.
	if got, err = s.ImportACDB(bg, false, gen(100, "B")); err != nil || got != 100 {
		t.Fatalf("update import: %d %v", got, err)
	}
	if e, _, _ := s.ACDBLookup(bg, 50); e.Reg != "B-50" {
		t.Fatalf("entry not updated: %+v", e)
	}
	if c, _ := s.ACDBCount(bg); c != n {
		t.Fatalf("count after update: %d", c)
	}
	// With replace the table holds exactly the new set.
	if got, err = s.ImportACDB(bg, true, gen(500, "C")); err != nil || got != 500 {
		t.Fatalf("replace import: %d %v", got, err)
	}
	if c, _ := s.ACDBCount(bg); c != 500 {
		t.Fatalf("count after replace: %d", c)
	}
	// Replace with nothing empties the table; a nil iterator is fine.
	if got, err = s.ImportACDB(bg, true, nil); err != nil || got != 0 {
		t.Fatalf("empty replace: %d %v", got, err)
	}
	if c, _ := s.ACDBCount(bg); c != 0 {
		t.Fatalf("count after empty replace: %d", c)
	}
	// A cancelled context stops the import with an error.
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := s.ImportACDB(ctx, false, gen(10, "D")); err == nil {
		t.Fatal("cancelled import must fail")
	}
	// Small batches go through the ordinary queue.
	s.Submit(func() *Batch { b := NewBatch(); b.AddACDB([]ACDBEntry{{ICAO: 1, Reg: "Q"}, {ICAO: 0}}); return b }())
	mustFlush(t, s)
	if c, _ := s.ACDBCount(bg); c != 1 {
		t.Fatalf("AddACDB: %d", c)
	}
}

// ------------------------------------------------------------------- prune

func TestPrune(t *testing.T) {
	s := openTest(t, manual)
	now := time.Now().Unix()
	const day = int64(86400)
	cfg := model.StorageConfig{TrailRetentionDays: 30, FlightRetentionDays: 60, MinuteRetentionDays: 10}
	nan := math.NaN()

	orphan := s.NextFlightID() // positions without a flight row
	b := NewBatch()
	var flights []Flight
	var positions []Position
	addFlights := func(count int, end int64, nPos int) (ids []int64) {
		for i := 0; i < count; i++ {
			f := NewFlight(s.NextFlightID(), uint32(0x600000+i), end-600)
			f.End = end + int64(i%7) // a few share the same second
			f.Positions = int64(nPos)
			flights = append(flights, f)
			for p := 0; p < nPos; p++ {
				positions = append(positions, Position{FlightID: f.ID, T: f.Start + int64(p)*5, Lat: 50, Lon: 8, Alt: nan, GS: nan, Track: nan, RSSI: nan})
			}
			ids = append(ids, f.ID)
		}
		return ids
	}
	veryOld := addFlights(450, now-90*day, 3) // beyond flight retention
	old := addFlights(450, now-45*day, 3)     // beyond trail retention only
	recent := addFlights(20, now-day, 3)      // kept
	noTrail := NewFlight(s.NextFlightID(), 0x6FFFFF, now-45*day)
	flights = append(flights, noTrail)
	b.UpsertFlights(flights)
	b.AddPositions(positions)
	b.AddPositions([]Position{
		{FlightID: orphan, T: now - 50*day, Lat: 1, Lon: 1, Alt: nan, GS: nan, Track: nan, RSSI: nan},
		{FlightID: orphan, T: now - day, Lat: 1, Lon: 1, Alt: nan, GS: nan, Track: nan, RSSI: nan}, // recent orphan: kept
	})

	// Minute statistics: 50 old hours x 60 minutes x 2 antennas, and a recent hour.
	oldHour := floorTo(now-20*day, 3600)
	recentHour := floorTo(now-day, 3600)
	var minutes []StatsRow
	for _, ant := range []string{"", "yagi"} {
		for h := int64(0); h < 50; h++ {
			for m := int64(0); m < 60; m++ {
				minutes = append(minutes, StatsRow{T: oldHour + h*3600 + m*60, Ant: ant, Frames: 10, Aircraft: 3, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan})
			}
		}
		for m := int64(0); m < 60; m++ {
			minutes = append(minutes, StatsRow{T: recentHour + m*60, Ant: ant, Frames: 10, Aircraft: 3, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan})
		}
	}
	b.AddMinuteStats(minutes)

	b.AddMetObs([]MetObs{{T: now - 40*day, ICAO: 1, WindDir: nan, WindKt: nan, OATC: nan}, {T: now - day, ICAO: 1, WindDir: nan, WindKt: nan, OATC: nan}})
	b.AddNTPSamples([]NTPSample{{T: now - 11*day, Server: "a"}, {T: now - 9*day, Server: "a"}})
	b.AddReceiverStatus([]ReceiverStatus{{T: now - 11*day, Feed: "f", JSON: "{}"}, {T: now - 9*day, Feed: "f", JSON: "{}"}})
	b.AddEvents([]Event{{T: now - 400*day, Kind: "ancient"}})
	if err := s.Apply(bg, b); err != nil {
		t.Fatal(err)
	}

	res, err := s.Prune(bg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := PruneResult{Flights: 450, Positions: 450*3 + 450*3 + 1, MinuteStats: 6000, MetObs: 1, NTPSamples: 1, ReceiverStatus: 1}
	res.FreedPages, res.Extra = 0, nil
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("prune result:\n got %+v\nwant %+v", res, want)
	}

	count := func(q string, args ...any) int { return scalar[int](t, s, q, args...) }
	if n := count(`SELECT COUNT(*) FROM flights`); n != 450+20+1 {
		t.Fatalf("flights left: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM flights WHERE id = ?`, veryOld[0]); n != 0 {
		t.Fatal("very old flight kept")
	}
	if n := count(`SELECT COUNT(*) FROM flights WHERE id = ?`, old[449]); n != 1 {
		t.Fatal("flight inside the flight retention deleted")
	}
	if n := posCount(t, s, func(p posRow) bool { return p.flight == veryOld[5] || p.flight == old[5] }); n != 0 {
		t.Fatal("old positions kept")
	}
	if n := posCount(t, s, nil); n != 20*3+1 {
		t.Fatalf("positions left: %d", n)
	}
	if n := posCount(t, s, func(p posRow) bool { return p.flight == recent[0] }); n != 3 {
		t.Fatal("recent trail damaged")
	}
	if n := posCount(t, s, func(p posRow) bool { return p.flight == orphan }); n != 1 {
		t.Fatalf("orphan positions: %d (the recent one must stay)", n)
	}
	if n := count(`SELECT COUNT(*) FROM stats_minute`); n != 120 {
		t.Fatalf("minute rows left: %d", n)
	}
	// The pruned minutes live on in stats_hour.
	if n := count(`SELECT COUNT(*) FROM stats_hour`); n != 102 {
		t.Fatalf("hour rows: %d", n)
	}
	if f := scalar[int64](t, s, `SELECT SUM(frames) FROM stats_hour WHERE t < ?`, recentHour); f != 60000 {
		t.Fatalf("frames in rolled-up hours: %d", f)
	}
	if n := len(metObsRows(t, s)) + count(`SELECT COUNT(*) FROM ntp_samples`) + count(`SELECT COUNT(*) FROM receiver_status`); n != 3 {
		t.Fatalf("met/ntp/status rows left: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM events`); n != 1 {
		t.Fatal("events must never be pruned")
	}

	// Nothing left to do: the second run is a no-op.
	res, err = s.Prune(bg, cfg)
	if err != nil || res.Total() != 0 {
		t.Fatalf("second prune: %+v %v", res, err)
	}

	// A minute row that arrives late for an hour whose minutes were pruned
	// must not replace the complete hour row by a partial one.
	s.AddMinuteStats([]StatsRow{{T: oldHour + 120, Ant: "", Frames: 1, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan}})
	if _, err := s.RollupHours(bg); err != nil {
		t.Fatal(err)
	}
	if f := scalar[int64](t, s, `SELECT frames FROM stats_hour WHERE t = ? AND ant = ''`, oldHour); f != 600 {
		t.Fatalf("frozen hour was recomputed: frames = %d", f)
	}
	// ... and the open hour is still maintained.
	s.AddMinuteStats([]StatsRow{{T: recentHour + 60, Ant: "", Frames: 5, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan}})
	if _, err := s.RollupHours(bg); err != nil {
		t.Fatal(err)
	}
	if f := scalar[int64](t, s, `SELECT frames FROM stats_hour WHERE t = ? AND ant = ''`, recentHour); f != 605 {
		t.Fatalf("open hour: frames = %d", f)
	}

	// Retention 0 / negative keeps everything.
	res, err = s.Prune(bg, model.StorageConfig{})
	if err != nil || res.Flights != 0 || res.Positions != 0 || res.MinuteStats != 0 {
		t.Fatalf("zero retention must not delete: %+v %v", res, err)
	}
	// Shorter flight retention than trail retention removes trails with their flights.
	res, err = s.Prune(bg, model.StorageConfig{TrailRetentionDays: 365, FlightRetentionDays: 40})
	if err != nil || res.Flights != 451 || res.Positions != 0 {
		t.Fatalf("flight-only prune: %+v %v", res, err)
	}
	if n := count(`SELECT COUNT(*) FROM flights`); n != 20 {
		t.Fatalf("flights left: %d", n)
	}
	// A cancelled context stops the work with an error.
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := s.Prune(ctx, cfg); err == nil {
		t.Fatal("cancelled prune must fail")
	}
}

func TestCheckpointAndVacuum(t *testing.T) {
	s := openTest(t, manual)
	var ev []Event
	for i := 0; i < 5000; i++ {
		ev = append(ev, Event{T: int64(i + 1), Kind: "bulk", Detail: strings.Repeat("x", 200)})
	}
	s.AddEvents(ev)
	mustFlush(t, s)
	wal := s.Path() + "-wal"
	if fi, err := os.Stat(wal); err != nil || fi.Size() == 0 {
		t.Fatalf("expected a non-empty WAL before the checkpoint: %v", err)
	}
	if err := s.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(wal); err != nil || fi.Size() != 0 {
		t.Fatalf("WAL not truncated: %v", err)
	}
	var timeout int
	if err := s.cur.Load().wdb.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 10000 {
		t.Fatalf("busy_timeout not restored: %d %v", timeout, err)
	}

	big := s.Stats().DBBytes
	if err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM events WHERE id > 10`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Vacuum(bg); err != nil {
		t.Fatal(err)
	}
	small := s.Stats()
	if small.DBBytes >= big || small.FreelistPages != 0 {
		t.Fatalf("vacuum did not shrink the file: %d -> %d (free pages %d)", big, small.DBBytes, small.FreelistPages)
	}
	var tempStore int
	if err := s.cur.Load().wdb.QueryRow(`PRAGMA temp_store`).Scan(&tempStore); err != nil || tempStore != 2 {
		t.Fatalf("temp_store not restored: %d %v", tempStore, err)
	}
	// Still fully functional (prepared statements survive the rebuild).
	s.AddEvents([]Event{{T: 1, Kind: "after-vacuum"}})
	mustFlush(t, s)
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events`); n != 11 {
		t.Fatalf("events after vacuum: %d", n)
	}
}

func TestPruneReleasesFreePages(t *testing.T) {
	s := openTest(t, manual)
	now := time.Now().Unix()
	// ~40 MB of old receiver status rows, then prune them all.
	payload := strings.Repeat("s", 4000)
	for chunk := 0; chunk < 10; chunk++ {
		rows := make([]ReceiverStatus, 1000)
		for i := range rows {
			rows[i] = ReceiverStatus{T: now - 100*86400 + int64(chunk*1000+i), Feed: "f", JSON: payload}
		}
		s.AddReceiverStatus(rows)
		mustFlush(t, s)
	}
	if err := s.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	before := s.Stats()
	res, err := s.Prune(bg, model.StorageConfig{MinuteRetentionDays: 30})
	if err != nil || res.ReceiverStatus != 10000 {
		t.Fatalf("prune: %+v %v", res, err)
	}
	if res.FreedPages < vacuumPages {
		t.Fatalf("expected free pages to be released: %+v (before: %+v)", res, before)
	}
	if err := s.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	after := s.Stats()
	if after.DBBytes >= before.DBBytes {
		t.Fatalf("file did not shrink: %d -> %d", before.DBBytes, after.DBBytes)
	}
}

// ------------------------------------------------------------------- reset

func TestReset(t *testing.T) {
	if err := RegisterMigration("mtest_reset", 1, `CREATE TABLE mtest_reset(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	s := openTest(t, manual)
	f := NewFlight(s.NextFlightID(), 0x700000, 1000)
	s.UpsertFlights([]Flight{f})
	s.UpsertAircraft([]AircraftSeen{{ICAO: 0x700000, LastSeen: 1000, MaxAlt: math.NaN()}})
	if err := s.KVSet(bg, "keep?", "no"); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, s)
	s.AddEvents([]Event{{T: 1, Kind: "queued-before-reset"}}) // still queued: discarded
	oldPool := s.DB()
	if s.NextFlightID() != 2 {
		t.Fatal("flight id")
	}

	if err := s.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}

	if s.DB() == oldPool {
		t.Fatal("Reset must hand out a new read pool")
	}
	if err := oldPool.PingContext(bg); err == nil {
		t.Fatal("the old read pool must be closed")
	}
	counts, err := s.TableCounts(bg)
	if err != nil {
		t.Fatal(err)
	}
	for table, n := range counts {
		if n != 0 && table != "schema_migrations" && table != "mtest_late" { // mtest_late's DDL inserts a row
			t.Errorf("table %s has %d rows after reset", table, n)
		}
	}
	if _, ok := counts["mtest_reset"]; !ok {
		t.Fatal("registered migrations must be re-applied after reset")
	}
	if counts["schema_migrations"] < 2 {
		t.Fatalf("schema_migrations: %d", counts["schema_migrations"])
	}
	if st := s.Stats(); st.QueuedRows != 0 {
		t.Fatalf("queue not discarded: %+v", st)
	}
	if id := s.NextFlightID(); id != 1 {
		t.Fatalf("flight ids must restart: %d", id)
	}
	// Fully usable afterwards, synchronous and queued.
	if err := s.KVSet(bg, "after", "reset"); err != nil {
		t.Fatal(err)
	}
	s.AddEvents([]Event{{T: 2, Kind: "after-reset"}})
	mustFlush(t, s)
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events`); n != 1 {
		t.Fatalf("events after reset: %d", n)
	}
	if n, err := s.RollupHours(bg); err != nil || n != 0 {
		t.Fatalf("rollup after reset: %d %v", n, err)
	}
	for _, f := range []string{FileName} {
		if _, err := os.Stat(filepath.Join(s.Dir(), f)); err != nil {
			t.Fatalf("%s missing after reset: %v", f, err)
		}
	}
	// And it can be reset again and closed cleanly.
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResetWhileReadersAndWritersAreActive(t *testing.T) {
	s := openTest(t, Options{FlushInterval: 5 * time.Millisecond})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				s.AddEvents([]Event{{T: int64(i + 1), Kind: "bg"}})
				_ = s.KVSet(bg, fmt.Sprintf("k%d", g), "v")   // may fail around the reset, must not hang
				_, _, _ = s.ListAircraft(bg, AircraftQuery{}) // may fail with "database is closed"
				_, _ = s.Events(bg, EventQuery{Limit: 5})
			}
		}(g)
	}
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		if err := s.Reset(); err != nil {
			t.Fatalf("reset %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if err := s.KVSet(bg, "final", "ok"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.KVGet(bg, "final"); err != nil || !ok || v != "ok" {
		t.Fatalf("store unusable after resets: %q %v %v", v, ok, err)
	}
}

// ------------------------------------------------------------- concurrency

func TestConcurrentReadersWhileWriting(t *testing.T) {
	s := openTest(t, Options{FlushInterval: 10 * time.Millisecond, FlushRows: 500})
	const writers, perWriter, readers = 4, 150, 6
	stop := make(chan struct{})
	errCh := make(chan error, 64)
	report := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	var rwg sync.WaitGroup
	var reads atomic.Int64
	for r := 0; r < readers; r++ {
		rwg.Add(1)
		go func(r int) {
			defer rwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var err error
				switch r % 3 {
				case 0:
					_, _, err = s.ListAircraft(bg, AircraftQuery{Search: "A", Limit: 20})
				case 1:
					// A multi-statement read inside one snapshot must be consistent:
					// every flight batch is committed together with its positions.
					var tx *sql.Tx
					if tx, err = s.DB().BeginTx(bg, &sql.TxOptions{ReadOnly: true}); err == nil {
						var nf, np int64
						err = tx.QueryRow(`SELECT COUNT(*) FROM flights`).Scan(&nf)
						if err == nil {
							np, err = LogicalCount(bg, tx, KindMain, "positions") // legacy rows, tail and chunks
						}
						_ = tx.Rollback()
						if err == nil && np != nf*5 {
							err = fmt.Errorf("inconsistent snapshot: %d flights, %d positions", nf, np)
						}
					}
				default:
					_, err = s.RecentFlights(bg, 10, 0)
					if err == nil {
						_, err = s.Events(bg, EventQuery{Limit: 10})
					}
				}
				if err != nil {
					report(fmt.Errorf("reader %d: %w", r, err))
					return
				}
				reads.Add(1)
			}
		}(r)
	}

	var wwg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wwg.Add(1)
		go func(w int) {
			defer wwg.Done()
			b := NewBatch()
			for i := 0; i < perWriter; i++ {
				id := s.NextFlightID()
				icao := uint32(0xA00000 + w*1000 + i)
				f := NewFlight(id, icao, int64(1000+i))
				f.Positions = 5
				ps := make([]Position, 5)
				for k := range ps {
					ps[k] = Position{FlightID: id, T: int64(1000 + i + k), Lat: 50, Lon: 8, Alt: 30000, GS: 400, Track: 90, RSSI: -10}
				}
				b.UpsertFlights([]Flight{f})
				b.AddPositions(ps)
				b.UpsertAircraft([]AircraftSeen{{ICAO: icao, LastSeen: int64(1000 + i), Callsign: fmt.Sprintf("A%d", i), Msgs: 1, MaxAlt: 30000}})
				b.AddMinuteStats([]StatsRow{{T: int64(60 * (i + 1)), Ant: "", Frames: 1, MaxKm: math.NaN(), RSSIAvg: math.NaN(), RSSIMin: math.NaN(), RSSIMax: math.NaN()}})
				s.Submit(b)
				switch i % 25 {
				case 7:
					if err := s.KVSet(bg, fmt.Sprintf("w%d", w), fmt.Sprint(i)); err != nil {
						report(fmt.Errorf("writer %d: %w", w, err))
						return
					}
				case 13:
					if err := s.Write(bg, func(tx *sql.Tx) error {
						_, err := tx.Exec(`INSERT INTO events(t, kind) VALUES(?, 'sync')`, i)
						return err
					}); err != nil {
						report(fmt.Errorf("writer %d: %w", w, err))
						return
					}
				case 19:
					if _, err := s.RollupHours(bg); err != nil {
						report(fmt.Errorf("writer %d rollup: %w", w, err))
						return
					}
				}
			}
		}(w)
	}
	wwg.Wait()
	mustFlush(t, s)
	time.Sleep(30 * time.Millisecond) // let the readers see the final state too
	close(stop)
	rwg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if reads.Load() < readers {
		t.Fatalf("readers hardly ran: %d reads", reads.Load())
	}

	total := writers * perWriter
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM flights`); n != total {
		t.Fatalf("flights: %d of %d", n, total)
	}
	if n := posCount(t, s, nil); n != total*5 {
		t.Fatalf("positions: %d of %d", n, total*5)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM aircraft`); n != total {
		t.Fatalf("aircraft: %d of %d", n, total)
	}
	if f := scalar[int](t, s, `SELECT SUM(frames) FROM stats_minute`); f != total {
		t.Fatalf("minute frames merged: %d of %d", f, total)
	}
	if n := scalar[int](t, s, `SELECT COUNT(DISTINCT id) FROM flights`); n != total {
		t.Fatal("flight ids are not unique")
	}
	st := s.Stats()
	if st.FlushErrors != 0 || st.FailedOps != 0 || st.DroppedLow+st.DroppedMid+st.DroppedHigh != 0 {
		t.Fatalf("writer problems: %+v", st)
	}
}

// -------------------------------------------------------------------- bins

func TestBins(t *testing.T) {
	if DayOf(0) != 0 || DayOf(86399) != 0 || DayOf(86400) != 1 || DayOf(-1) != -1 || DayStart(DayOf(1_700_000_000)) != 1_699_920_000 {
		t.Fatal("DayOf / DayStart")
	}
	for alt, band := range map[float64]int{-500: 0, 999: 0, 1000: 1, 4999: 1, 5000: 2, 10000: 3, 19999: 3, 20000: 4, 30000: 5, 39999: 5, 40000: 6, 60000: 6} {
		if got := AltBand(alt); got != band {
			t.Errorf("AltBand(%v) = %d, want %d", alt, got, band)
		}
		if lo, hi := AltBandRange(band); alt < lo || alt >= hi {
			t.Errorf("AltBandRange(%d) = [%v, %v) does not contain %v", band, lo, hi, alt)
		}
	}
	if AltBand(math.NaN()) != 0 {
		t.Error("AltBand(NaN)")
	}
	for deg, bin := range map[float64]int{0: 0, 1.99: 0, 2: 1, 359.9: 179, 360: 0, 361: 0, -1: 179, 720 + 90: 45} {
		if got := BrgBin(deg); got != bin {
			t.Errorf("BrgBin(%v) = %d, want %d", deg, got, bin)
		}
	}
	if BrgBin(math.NaN()) != 0 || BrgBin(math.Inf(1)) != 0 {
		t.Error("BrgBin of non-numbers")
	}
	for km, bin := range map[float64]int{-5: 0, 0: 0, 9.99: 0, 10: 1, 455: 45, 989: 98, 990: 99, 5000: 99} {
		if got := DistBin(km); got != bin {
			t.Errorf("DistBin(%v) = %d, want %d", km, got, bin)
		}
	}
	for db, bin := range map[float64]int{0: -1, -0.1: -1, -1: -1, -1.5: -2, -22.5: -23, -23: -23, -59: -59, -60: -60,
		-80: -60, -120: -60, 3: -1, math.NaN(): -60} {
		if got := RSSIBin(db); got != bin {
			t.Errorf("RSSIBin(%v) = %d, want %d", db, got, bin)
		}
	}
	// The tracker's in-memory index (geo) and the on-disk bin agree on every
	// level, also exactly on whole dB.
	for x := 5.0; x >= -70; x -= 0.25 {
		if g, s := geo.RSSIBinStored(geo.RSSIBin(x)), RSSIBin(x); g != s {
			t.Fatalf("level %v: geo -> %d, store %d", x, g, s)
		}
		if b := RSSIBin(x); x < 0 && x >= -60 && (float64(b) > x || x >= float64(b)+1) {
			t.Fatalf("level %v not inside bin [%d, %d)", x, b, b+1)
		}
	}
	for deg, bin := range map[float64]int{-10: 0, -2: 0, -1.6: 0, -1.5: 1, 0: 4, 0.49: 4, 0.5: 5, 89.9: 183, 90: 183, 120: 183} {
		if got := ElevBin(deg); got != bin {
			t.Errorf("ElevBin(%v) = %d, want %d", deg, got, bin)
		}
	}
	if ElevBinLow(0) != -2 || ElevBinLow(4) != 0 || ElevBinLow(ElevBins-1) != 89.5 {
		t.Error("ElevBinLow")
	}
	for alt, lvl := range map[float64]int{0: 0, 999: 0, 1000: 1, 35400: 35, -200: -1, -5000: -1, 99000: 60} {
		if got := MetLevel(alt); got != lvl {
			t.Errorf("MetLevel(%v) = %d, want %d", alt, got, lvl)
		}
	}

	cell := HeatCell(50.03, 8.57)
	if cell != (2800<<16 | 3771) {
		t.Fatalf("HeatCell = %d (lat idx %d, lon idx %d)", cell, cell>>16, cell&0xffff)
	}
	lat, lon := HeatCellCenter(cell)
	if !near(lat, 50.025) || !near(lon, 8.575) {
		t.Fatalf("HeatCellCenter = %v, %v", lat, lon)
	}
	seen := map[int64]bool{}
	for _, p := range [][2]float64{{-90, -180}, {90, 180}, {89.99, 179.99}, {0, 0}, {-0.01, -0.01}, {120, 540}, {-120, -540}} {
		c := HeatCell(p[0], p[1])
		if li, lo := c>>16, c&0xffff; li < 0 || li > 3599 || lo < 0 || lo > 7199 {
			t.Errorf("HeatCell(%v) out of range: %d/%d", p, li, lo)
		}
		seen[c] = true
	}
	if HeatCell(0, 180) != HeatCell(0, -180) {
		t.Error("longitude must wrap")
	}
	if HeatCell(math.NaN(), 0) != 0 {
		t.Error("HeatCell(NaN)")
	}
	if floorTo(-1, 60) != -60 || floorTo(119, 60) != 60 || floorDiv(-7, 2) != -4 {
		t.Error("floor helpers")
	}
}

// posRow is one trail point of the logical 1.0.0 positions table.
type posRow struct{ flight, t int64 }

// posCount counts the trail points (through the logical positions table:
// legacy rows, trail tail and chunks alike) that match keep (nil = all).
func posCount(t *testing.T, s *Store, keep func(posRow) bool) int {
	t.Helper()
	n := 0
	err := s.ScanTable(bg, "positions", "t", nil, 0, func([]string) error { return nil }, func(v []any) error {
		p := posRow{flight: v[0].(int64), t: v[1].(int64)}
		if keep == nil || keep(p) {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
