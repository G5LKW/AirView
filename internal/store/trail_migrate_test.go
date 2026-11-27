package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"
)

// The migration step is bounded by its flight and point budgets, resumes
// from its cursor, and a step rolled back by a crash is simply redone.
func TestTrailMigrateStepBudgetAndResume(t *testing.T) {
	SetTrailSealInterval(time.Hour)
	defer SetTrailSealInterval(0)
	s := openTest(t, Options{FlushInterval: time.Hour, FlushRows: 1 << 30, MaxQueueRows: 1 << 30, MigrationDelay: -1})
	rng := rand.New(rand.NewPCG(11, 12))
	m := trailModel{}
	var legacy []TrailRow
	for fid := int64(1); fid <= 150; fid++ {
		for k := int64(0); k < 40+fid; k++ {
			r := randTrailRow(rng, fid, 1_000_000+fid*10+k*5)
			legacy = append(legacy, r)
			m.put(r)
		}
	}
	insertLegacy(t, s, legacy)

	var cursor []byte
	steps := 0
	for {
		var next []byte
		var done bool
		crash := steps%3 == 1
		err := s.Write(bg, func(tx *sql.Tx) error {
			var err error
			next, done, err = trailMigrateStep(bg, tx, cursor, 1000)
			if err != nil {
				return err
			}
			if crash {
				return errors.New("crash")
			}
			return nil
		})
		steps++
		if crash {
			if err == nil {
				t.Fatal("crash not reported")
			}
			continue // nothing committed: the same cursor again
		}
		if err != nil {
			t.Fatal(err)
		}
		if steps > 1000 {
			t.Fatal("migration does not finish")
		}
		// Budget: at most 64 flights, and it stops after the flight that
		// crossed 1000 points.
		if c := parseTrailCursor(next); c-parseTrailCursor(cursor) > trailMigrateFlights && cursor != nil {
			t.Fatalf("step converted %d flights", c-parseTrailCursor(cursor))
		}
		cursor = next
		got := trailTestRows(t, s)
		if !slices.Equal(got, m.rows(nil)) {
			t.Fatalf("step %d: logical rows changed", steps)
		}
		if done {
			break
		}
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM positions`); n != 0 {
		t.Fatalf("%d legacy rows left", n)
	}
	// Every flight is one run of full 256-point chunks plus a last chunk.
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM trail_chunk c WHERE n != 256 AND t1 != (SELECT MAX(t1) FROM trail_chunk d WHERE d.flight_id = c.flight_id)`); n != 0 {
		t.Fatalf("%d short chunks", n)
	}
	if d, err := s.VerifyCounts(bg); err != nil || d != (CountDrift{}) {
		t.Fatalf("drift %+v %v", d, err)
	}
	// The registered migration runs through the framework too.
	found := false
	dataMigs.mu.Lock()
	for _, dm := range dataMigs.list {
		found = found || (dm.Name == "trail" && dm.Version == 1 && dm.Step != nil && dm.Estimate != nil)
	}
	dataMigs.mu.Unlock()
	if !found {
		t.Fatal("data migration trail/1 not registered")
	}
}

// The logical positions table registered for exports and the parity
// harness yields the 1.0.0 columns, order, span and limit.
func TestTrailLogicalTable(t *testing.T) {
	SetTrailSealInterval(time.Hour)
	defer SetTrailSealInterval(0)
	s := openTest(t, Options{FlushInterval: time.Hour, FlushRows: 1 << 30, MaxQueueRows: 1 << 30, MigrationDelay: -1})
	rng := rand.New(rand.NewPCG(2, 5))
	m := trailModel{}
	var legacy, fresh []TrailRow
	for fid := int64(1); fid <= 9; fid++ {
		for k := int64(0); k < 50; k++ {
			r := randTrailRow(rng, fid, 2_000_000+k*7)
			if fid%2 == 0 {
				legacy = append(legacy, r)
			} else {
				fresh = append(fresh, r)
			}
			m.put(r)
		}
	}
	insertLegacy(t, s, legacy)
	addTrailRows(s, fresh[:200])
	mustFlush(t, s)
	if _, err := s.SealTrails(bg); err != nil {
		t.Fatal(err)
	}
	addTrailRows(s, fresh[200:])
	mustFlush(t, s)
	lt, ok := LogicalTableFor("positions", KindMain)
	if !ok || !slices.Equal(lt.Columns, TrailColumns) || lt.TimeCol != "t" {
		t.Fatalf("logical table %+v %v", lt, ok)
	}
	for _, c := range []struct {
		span  *Span
		limit int64
	}{{nil, 0}, {&Span{From: 2_000_070, To: 2_000_200}, 0}, {&Span{From: 2_000_000, To: 2_000_300}, 33}} {
		var got [][]any
		if err := lt.Scan(bg, s.DB(), c.span, c.limit, func(row []any) error {
			got = append(got, row)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		want := m.rows(func(r TrailRow) bool { return c.span == nil || (r.T >= c.span.From && r.T < c.span.To) })
		if c.limit > 0 && int64(len(want)) > c.limit {
			want = want[:c.limit]
		}
		if len(got) != len(want) {
			t.Fatalf("%+v: %d rows, want %d", c, len(got), len(want))
		}
		for i := range want {
			if !slices.Equal(got[i], want[i].Values()) {
				t.Fatalf("%+v row %d: %v want %v", c, i, got[i], want[i].Values())
			}
		}
	}
	if n, err := lt.Count(bg, s.DB()); err != nil || n != int64(len(m)) {
		t.Fatalf("count %d %v (want %d)", n, err, len(m))
	}
}

// TestTrailMigrateGen7 measures the trail migration on a copy of a real
// generator database: AIRVIEW_TRAILMIGRATE=<directory holding a COPY of a
// 1.0.0 airview.db> (the file is changed).
func TestTrailMigrateGen7(t *testing.T) {
	dir := os.Getenv("AIRVIEW_TRAILMIGRATE")
	if dir == "" {
		t.Skip("AIRVIEW_TRAILMIGRATE not set")
	}
	ctx := context.Background()
	sizes := func(s *Store) map[string]int64 {
		rs, err := s.DB().QueryContext(ctx, `SELECT COALESCE(m.tbl_name, d.name), SUM(d.pgsize) FROM dbstat d
LEFT JOIN sqlite_master m ON m.name = d.name GROUP BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		out := map[string]int64{}
		for rs.Next() {
			var n string
			var b int64
			if err := rs.Scan(&n, &b); err != nil {
				t.Fatal(err)
			}
			out[n] = b
		}
		return out
	}
	t0 := time.Now()
	s, err := OpenWithOptions(dir, Options{MigrationDelay: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("open (DDL migrations) %v", time.Since(t0).Round(time.Millisecond))
	before := sizes(s)
	t.Logf("before: positions %.1f MB, file %.1f MB", float64(before["positions"])/1e6, float64(s.DBSize())/1e6)

	t1 := time.Now()
	c0, err := s.TableCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("TableCounts on the 1.0.0 layout (counts the legacy table once): %v, positions %d", time.Since(t1).Round(time.Millisecond), c0["positions"])
	t1 = time.Now()
	s.invalidateCounts()
	if _, err := s.TableCounts(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("TableCounts again (counters): %v", time.Since(t1).Round(time.Microsecond))

	// Replay reads (the store part of /api/replay) over the last day / 6 h.
	replays := func(label string) {
		var tmax int64
		if err := s.DB().QueryRowContext(ctx, `SELECT MAX("end") FROM flights`).Scan(&tmax); err != nil {
			t.Fatal(err)
		}
		for _, h := range []int64{24, 6} {
			span := h * 3600
			step := int64(math.Max(5, math.Ceil(float64(span)/600)))
			stale := max(step*2, 3*5, 30)
			t4 := time.Now()
			st, err := s.ForEachPositionStep(ctx, tmax-span, tmax, step, stale, func(uint32, Position) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: replay %d h: %d points read, %d used, %v", label, h, st.Read, st.Used, time.Since(t4).Round(time.Millisecond))
		}
	}
	replays("1.0.0 layout")
	t2 := time.Now()
	steps, err := s.migrateTrailsNow(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("migration: %d steps in %v", steps, time.Since(t2).Round(time.Millisecond))
	if _, err := s.CompactTrails(ctx); err != nil {
		t.Fatal(err)
	}
	after := sizes(s)
	trail := after["positions"] + after["trail_chunk"] + after["trail_tail"]
	t.Logf("after: positions %.2f MB + trail_chunk %.2f MB + trail_tail %.2f MB = %.1f MB (%.2f B/point)",
		float64(after["positions"])/1e6, float64(after["trail_chunk"])/1e6, float64(after["trail_tail"])/1e6,
		float64(trail)/1e6, float64(trail)/float64(c0["positions"]))
	s.invalidateCounts()
	c1, err := s.TableCounts(ctx)
	if err != nil || c1["positions"] != c0["positions"] {
		t.Fatalf("positions count %d -> %d (%v)", c0["positions"], c1["positions"], err)
	}
	if d, err := s.VerifyCounts(ctx); err != nil || d != (CountDrift{}) {
		t.Fatalf("drift %+v %v", d, err)
	}

	// The longest trail.
	var fid, n int64
	if err := s.DB().QueryRowContext(ctx, `SELECT flight_id, SUM(n) FROM trail_chunk GROUP BY flight_id ORDER BY 2 DESC LIMIT 1`).Scan(&fid, &n); err != nil {
		t.Fatal(err)
	}
	var ds []time.Duration
	for i := 0; i < 20; i++ {
		t3 := time.Now()
		tr, err := s.Trail(ctx, fid)
		if err != nil || int64(len(tr)) != n {
			t.Fatalf("trail %d: %d points (%v)", fid, len(tr), err)
		}
		ds = append(ds, time.Since(t3))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("Trail of flight %d (%d points): median %v, max %v", fid, n, ds[len(ds)/2], ds[len(ds)-1])

	replays("after the migration")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, FileName)); err == nil {
		t.Logf("file after close: %.1f MB (freed pages are reused; incremental vacuum returns them)", float64(fi.Size())/1e6)
	}
}
