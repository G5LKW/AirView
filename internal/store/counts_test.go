package store

import (
	"context"
	"database/sql"
	"math/rand/v2"
	"testing"
	"time"
)

// TableCounts reports every table with its exact count: COUNT(*) for the
// small tables, the O(1) counters for the logical positions table (legacy
// rows + chunks + tail), and it sees new writes.
func TestTableCountsExact(t *testing.T) {
	SetTrailSealInterval(time.Hour)
	defer SetTrailSealInterval(0)
	s := openTest(t, Options{FlushInterval: time.Hour, FlushRows: 1 << 30, MaxQueueRows: 1 << 30, MigrationDelay: -1})
	rng := rand.New(rand.NewPCG(31, 32))
	var legacy []TrailRow
	for k := int64(0); k < 500; k++ {
		legacy = append(legacy, randTrailRow(rng, 1+k%7, 1_000_000+k))
	}
	insertLegacy(t, s, legacy)
	s.AddEvents([]Event{{T: 5, Kind: "x"}, {T: 6, Kind: "y"}})
	mustFlush(t, s)

	check := func(label string, wantPositions int) {
		t.Helper()
		s.invalidateCounts()
		s.counts.mu.Lock()
		s.counts.vals = nil // force a recount whatever the cache policy says
		s.counts.mu.Unlock()
		c, err := s.TableCounts(bg)
		if err != nil {
			t.Fatal(err)
		}
		if c["positions"] != int64(wantPositions) {
			t.Fatalf("%s: positions %d, want %d", label, c["positions"], wantPositions)
		}
		if c["events"] != 2 || c["trail_tail"] != int64(scalar[int](t, s, `SELECT COUNT(*) FROM trail_tail`)) ||
			c["trail_chunk"] != int64(scalar[int](t, s, `SELECT COUNT(*) FROM trail_chunk`)) {
			t.Fatalf("%s: counts %v", label, c)
		}
		names, _ := queryStrings(bg, s.DB(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
		for _, n := range names {
			if _, ok := c[n]; !ok {
				t.Fatalf("%s: table %s missing", label, n)
			}
		}
	}
	check("legacy table", 500)
	if v, ok, _ := s.KVGet(bg, kvTrailLegacy); !ok || v != "500 0" {
		t.Fatalf("legacy base not recorded: %q", v)
	}
	var fresh []TrailRow
	for k := int64(0); k < 300; k++ {
		fresh = append(fresh, randTrailRow(rng, 20+k%3, 2_000_000+k))
	}
	addTrailRows(s, fresh)
	mustFlush(t, s)
	check("tail", 800)
	if _, err := s.SealTrails(bg); err != nil {
		t.Fatal(err)
	}
	check("sealed", 800)
	if _, err := s.migrateTrailsNow(bg, 100); err != nil {
		t.Fatal(err)
	}
	check("migrated", 800)
	if _, ok, _ := s.KVGet(bg, kvTrailLegacy); ok {
		t.Fatal("legacy base kept after the migration")
	}
}

// A drifted counter (rows changed behind the store's back) is found and
// corrected by VerifyCounts.
func TestVerifyCountsCorrectsDrift(t *testing.T) {
	SetTrailSealInterval(time.Hour)
	defer SetTrailSealInterval(0)
	s := openTest(t, Options{FlushInterval: time.Hour, FlushRows: 1 << 30, MaxQueueRows: 1 << 30, MigrationDelay: -1})
	rng := rand.New(rand.NewPCG(1, 31))
	var rows []TrailRow
	for k := int64(0); k < 100; k++ {
		rows = append(rows, randTrailRow(rng, 3, 5_000+k))
	}
	addTrailRows(s, rows)
	mustFlush(t, s)
	if _, err := s.SealTrails(bg); err != nil {
		t.Fatal(err)
	}
	addTrailRows(s, rows[:10]) // tail
	mustFlush(t, s)
	// Tamper: delete a chunk and a tail row directly.
	if err := s.Write(bg, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM trail_tail WHERE rowid = (SELECT MIN(rowid) FROM trail_tail)`); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE kv SET value = '1 2 3 4' WHERE key = ?`, kvTrailState)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The tail held one row (the 10 points of that flush); it is gone.
	d, err := s.VerifyCounts(context.Background())
	if err != nil || d.Chunk != 99 || d.Tail != -2 {
		t.Fatalf("drift %+v %v", d, err)
	}
	st, _ := readTrailState(bg, s.DB())
	if st.Chunk != 100 || st.Tail != 0 {
		t.Fatalf("state after verify %+v", st)
	}
	// A tail the counter does not know about is found, and the readers see
	// it again (TailMinT repaired).
	addTrailRows(s, []TrailRow{randTrailRow(rng, 3, 9_000)})
	mustFlush(t, s)
	if err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE kv SET value = '100 0 0 9223372036854775807' WHERE key = ?`, kvTrailState)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := trailTestCount(t, s, nil); got != 100 {
		t.Fatalf("hidden tail visible: %d", got)
	}
	if d, err := s.VerifyCounts(bg); err != nil || d.Tail != 1 {
		t.Fatalf("drift %+v %v", d, err)
	}
	if d, err := s.VerifyCounts(bg); err != nil || d != (CountDrift{}) {
		t.Fatalf("second verify %+v %v", d, err)
	}
	if got := trailTestCount(t, s, nil); got != 101 {
		t.Fatalf("logical rows %d", got)
	}
}
