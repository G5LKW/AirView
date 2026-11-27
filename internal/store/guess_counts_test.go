package store

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

// Tests of the guess_samples row counters (DB-EFFICIENCY-DESIGN §3.19).

func gcOpen(t *testing.T, dir string, kind Kind) *Store {
	t.Helper()
	st, err := OpenWithOptions(dir, Options{Kind: kind, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// gcRealCounts is the 1.0.0 way of counting: GROUP BY over the table.
func gcRealCounts(t *testing.T, st *Store) map[int]int64 {
	t.Helper()
	rows, err := st.DB().Query(`SELECT stn, COUNT(*) FROM guess_samples GROUP BY stn`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var stn int
		var n int64
		if err := rows.Scan(&stn, &n); err != nil {
			t.Fatal(err)
		}
		out[stn] = n
	}
	return out
}

func gcEqual(a, b map[int]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func gcSamples(r *rand.Rand, n int, stns []int, t0, span int64) []GuessSample {
	out := make([]GuessSample, 0, n)
	for range n {
		out = append(out, GuessSample{
			Stn: stns[r.IntN(len(stns))], T: t0 + r.Int64N(span), ICAO: uint32(r.IntN(50) + 1),
			Lat: 50 + r.Float64(), Lon: 8 + r.Float64(), AltFt: math.NaN(), RSSI: -20, Ch: -1,
		})
	}
	return out
}

func TestGuessCountersTrackInsertsAndDuplicates(t *testing.T) {
	for _, kind := range []Kind{KindMain, KindStations} {
		st := gcOpen(t, t.TempDir(), kind)
		r := rand.New(rand.NewPCG(1, 2))
		for range 20 {
			rows := gcSamples(r, 500, []int{0, 3, 17}, 1_000_000, 200_000)                   // many duplicate keys
			rows = append(rows, GuessSample{Stn: 3, T: 5, ICAO: 1, Lat: math.NaN(), Lon: 1}) // skipped: no position
			st.AddGuessSamples(rows)
		}
		if err := st.Flush(); err != nil {
			t.Fatal(err)
		}
		got, err := st.GuessSampleCounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := gcRealCounts(t, st)
		if !gcEqual(got, want) || len(want) != 3 {
			t.Fatalf("kind %v: counters %v, table %v", kind, got, want)
		}
		total, _ := st.GuessSamplesTotal(context.Background())
		if total != want[0]+want[3]+want[17] {
			t.Fatalf("total %d", total)
		}
		if tc, err := st.TableCounts(context.Background()); err != nil || tc[GuessSamplesTable] != total {
			t.Fatalf("TableCounts guess_samples %d (%v), want %d", tc[GuessSamplesTable], err, total)
		}
		for _, from := range []int64{0, 1_050_000, 1_100_000, 1_199_999, 2_000_000} {
			n, err := st.GuessSampleCount(context.Background(), 3, from)
			if err != nil {
				t.Fatal(err)
			}
			var exp int64
			_ = st.DB().QueryRow(`SELECT COUNT(*) FROM guess_samples WHERE stn = 3 AND t >= ?`, from).Scan(&exp)
			if n != exp {
				t.Fatalf("GuessSampleCount(3, %d) = %d, want %d", from, n, exp)
			}
		}
		if n, _ := st.GuessSampleCount(context.Background(), 99, 0); n != 0 {
			t.Fatalf("unknown station: %d", n)
		}
	}
}

// pruneOld is PruneGuessSamples of 1.0.0 (GROUP BY counting), the reference.
func pruneOld(ctx context.Context, s *Store, lim GuessPruneLimits) {
	counts := func() map[int]int64 {
		rows, _ := s.DB().QueryContext(ctx, `SELECT stn, COUNT(*) FROM guess_samples GROUP BY stn`)
		defer rows.Close()
		out := map[int]int64{}
		for rows.Next() {
			var stn int
			var n int64
			_ = rows.Scan(&stn, &n)
			out[stn] = n
		}
		return out
	}
	del := func(stn int, before int64) {
		_ = s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM guess_samples WHERE stn = ? AND t < ?`, stn, before)
			return err
		})
	}
	c := counts()
	if lim.CutoffMs > 0 {
		for stn := range c {
			del(stn, lim.CutoffMs)
		}
		c = counts()
	}
	var total int64
	for _, n := range c {
		total += n
	}
	for stn, n := range c {
		keep := n
		if lim.PerStation > 0 && keep > lim.PerStation {
			keep = lim.PerStation
		}
		if lim.Total > 0 && total > lim.Total {
			if share := int64(float64(lim.Total) * float64(n) / float64(total)); share < keep {
				keep = share
			}
		}
		if keep >= n {
			continue
		}
		var t0 int64
		if err := s.DB().QueryRowContext(ctx, `SELECT t FROM guess_samples WHERE stn = ? ORDER BY t LIMIT 1 OFFSET ?`, stn, n-keep).Scan(&t0); err != nil {
			continue
		}
		del(stn, t0)
	}
}

func gcDump(t *testing.T, st *Store) []GuessSample {
	t.Helper()
	var out []GuessSample
	for _, stn := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9} {
		if err := st.ScanGuessSamples(context.Background(), stn, 0, 0, func(g GuessSample) error {
			out = append(out, g)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestGuessPruneMatchesLegacyAndKeepsCounters(t *testing.T) {
	ctx := context.Background()
	limits := []GuessPruneLimits{
		{CutoffMs: 1_050_000},
		{PerStation: 300},
		{Total: 700},
		{CutoffMs: 1_020_000, PerStation: 400, Total: 900},
		{},
	}
	for i, lim := range limits {
		newSt := gcOpen(t, t.TempDir(), KindMain)
		oldSt := gcOpen(t, t.TempDir(), KindMain)
		r := rand.New(rand.NewPCG(uint64(i), 9))
		rows := gcSamples(r, 3000, []int{1, 2, 5}, 1_000_000, 100_000)
		newSt.AddGuessSamples(rows)
		oldSt.AddGuessSamples(rows)
		_ = newSt.Flush()
		_ = oldSt.Flush()
		before := gcRealCounts(t, newSt)
		n, err := newSt.PruneGuessSamples(ctx, lim)
		if err != nil {
			t.Fatal(err)
		}
		pruneOld(ctx, oldSt, lim)
		a, b := gcDump(t, newSt), gcDump(t, oldSt)
		if len(a) != len(b) {
			t.Fatalf("limits %+v: %d rows left, legacy pruner %d", lim, len(a), len(b))
		}
		for j := range a {
			if a[j] != b[j] && !(math.IsNaN(a[j].AltFt) && math.IsNaN(b[j].AltFt)) {
				t.Fatalf("limits %+v: row %d differs: %+v vs %+v", lim, j, a[j], b[j])
			}
		}
		var sumBefore int64
		for _, v := range before {
			sumBefore += v
		}
		if n != sumBefore-int64(len(a)) {
			t.Fatalf("limits %+v: deleted %d, want %d", lim, n, sumBefore-int64(len(a)))
		}
		if diff, err := newSt.VerifyGuessCounts(ctx, false); err != nil || len(diff) != 0 {
			t.Fatalf("limits %+v: counter drift %v %v", lim, diff, err)
		}
	}
}

func TestGuessPruneHealsDriftedCounter(t *testing.T) {
	ctx := context.Background()
	st := gcOpen(t, t.TempDir(), KindMain)
	st.AddGuessSamples(gcSamples(rand.New(rand.NewPCG(4, 4)), 200, []int{2}, 1000, 10_000))
	_ = st.Flush()
	// A foreign writer that bypasses the counters (cannot happen through
	// the store API; e.g. a manual DELETE in an SQL shell).
	_ = st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM guess_samples WHERE stn = 2 AND t < 10500`)
		return err
	})
	if diff, _ := st.VerifyGuessCounts(ctx, false); len(diff) != 1 {
		t.Fatalf("expected one drifted counter, got %v", diff)
	}
	if _, err := st.PruneGuessSamples(ctx, GuessPruneLimits{PerStation: 150}); err != nil {
		t.Fatal(err)
	}
	if diff, _ := st.VerifyGuessCounts(ctx, false); len(diff) != 0 {
		t.Fatalf("counter not healed: %v", diff)
	}
}

func TestGuessDeleteDropsCounter(t *testing.T) {
	ctx := context.Background()
	st := gcOpen(t, t.TempDir(), KindStations)
	st.AddGuessSamples(gcSamples(rand.New(rand.NewPCG(5, 5)), 100, []int{4, 6}, 1000, 10_000))
	_ = st.Flush()
	if err := st.DeleteGuessSamples(ctx, 4); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GuessSampleCounts(ctx)
	if _, ok := got[4]; ok || got[6] == 0 {
		t.Fatalf("counts after delete: %v", got)
	}
	var keys int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM kv WHERE key = ?`, guessCountKey(4)).Scan(&keys)
	if keys != 0 {
		t.Fatal("counter row of the deleted station is left behind")
	}
}

// TestGuessCounterSeedMigration simulates a 1.0.0 database: rows written
// without counters and no guess_samples/2 migration record. Reopening seeds
// the counters from the table once.
func TestGuessCounterSeedMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := gcOpen(t, dir, KindMain)
	st.AddGuessSamples(gcSamples(rand.New(rand.NewPCG(6, 6)), 1000, []int{0, 1}, 1000, 100_000))
	_ = st.Flush()
	_ = st.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM kv WHERE key LIKE 'count.guess_samples.%'`); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM schema_migrations WHERE name = 'guess_samples' AND version = 2`)
		return err
	})
	if got, _ := st.GuessSampleCounts(ctx); len(got) != 0 {
		t.Fatalf("setup: counters left: %v", got)
	}
	want := gcRealCounts(t, st)
	_ = st.Close()

	st2 := gcOpen(t, dir, KindMain)
	got, err := st2.GuessSampleCounts(ctx)
	if err != nil || !gcEqual(got, want) {
		t.Fatalf("seeded counters %v (%v), want %v", got, err, want)
	}
}

// TestGuessPruneNeedsNoScan checks the statements of a prune: none of them
// may count or group the whole table.
func TestGuessPruneNeedsNoScan(t *testing.T) {
	ctx := context.Background()
	st := gcOpen(t, t.TempDir(), KindMain)
	st.AddGuessSamples(gcSamples(rand.New(rand.NewPCG(7, 7)), 2000, []int{0}, 1000, 100_000))
	_ = st.Flush()
	// The plan of every reader statement of the pruner is a key search.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT key, CAST(value AS INTEGER) FROM kv WHERE key >= ? AND key < ?`, []any{guessCountPrefix, guessCountEnd}},
		{`SELECT t FROM guess_samples WHERE stn = ? ORDER BY t LIMIT 1`, []any{0}},
		{`SELECT t FROM guess_samples WHERE stn = ? ORDER BY t LIMIT 1 OFFSET ?`, []any{0, 10}},
		{`DELETE FROM guess_samples WHERE stn = ? AND t < ?`, []any{0, 5000}},
	} {
		rows, err := st.DB().QueryContext(ctx, `EXPLAIN QUERY PLAN `+q.sql, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, notused int
			var detail string
			_ = rows.Scan(&id, &parent, &notused, &detail)
			if !strings.Contains(detail, "SEARCH") {
				t.Errorf("%s: plan %q is not a key search", q.sql, detail)
			}
		}
		rows.Close()
	}
	if _, err := st.PruneGuessSamples(ctx, GuessPruneLimits{CutoffMs: 50_000, PerStation: 500}); err != nil {
		t.Fatal(err)
	}
}
