package store

import (
	"database/sql"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func heatSnapshot(t *testing.T, s *Store) (map[string][]HeatRow, [][]any) {
	t.Helper()
	cells := map[string][]HeatRow{}
	for _, ant := range []string{"", "yagi", "vertical"} {
		c, err := s.HeatCells(bg, ant)
		if err != nil {
			t.Fatal(err)
		}
		cells[ant] = c
	}
	var rows [][]any
	err := ScanTableDB(bg, s.DB(), KindMain, "heat", "", nil, 0, func([]string) error { return nil }, func(v []any) error {
		rows = append(rows, append([]any(nil), v...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return cells, rows
}

func applyHeatV2(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Write(bg, func(tx *sql.Tx) error { _, err := tx.Exec(heatV2DDL); return err }); err != nil {
		t.Fatal(err)
	}
}

// legacyHeat replaces the store's heat table (already tiled by the "heat" 2
// DDL at Open) with the 1.0.0 layout keyed (cell, ant) and merges rows into
// it with the 1.0.0 UPSERT, as a 1.0.0 build would have written them.
func legacyHeat(t *testing.T, s *Store, rows []HeatInc) {
	t.Helper()
	if err := s.Write(bg, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DROP TABLE IF EXISTS ` + heatLegacyTable,
			`DROP TABLE IF EXISTS heat`,
			`CREATE TABLE heat(
  cell     INTEGER NOT NULL,
  ant      TEXT NOT NULL,
  count    INTEGER NOT NULL DEFAULT 0,
  rssi_sum REAL NOT NULL DEFAULT 0,
  rssi_n   INTEGER NOT NULL DEFAULT 0,
  min_alt  INTEGER,
  max_alt  INTEGER,
  last_t   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(cell, ant)
) WITHOUT ROWID`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		for _, r := range rows {
			if _, err := tx.Exec(sqlMergeHeat, r.Cell, r.Ant, r.Count, zf(r.RSSISum), r.RSSIN, ni(r.MinAlt), ni(r.MaxAlt), r.LastT); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestHeatMigration: a 1.0.0 heat table (key (cell, ant)) is moved into the
// (tile, cell, ant) layout one antenna per step; readers and the export see
// the same cells before, during and after, and increments written into the
// new table meanwhile are merged with the UPSERT rules.
func TestHeatMigration(t *testing.T) {
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(21, 22))
	p := randomIncs(rng, 20000, 6000)
	legacyHeat(t, s, p.Heat)
	oldCells, oldRows := heatSnapshot(t, s) // 1.0.0 layout: physical SELECT *

	applyHeatV2(t, s)
	if !heatHasTile(bg, s.DB()) || !s.HasTable(heatLegacyTable) {
		t.Fatal("DDL not applied")
	}
	steps := 0
	for {
		cells, rows := heatSnapshot(t, s)
		if !reflect.DeepEqual(rows, oldRows) {
			t.Fatalf("step %d: export rows differ", steps)
		}
		for ant := range oldCells {
			if !heatEqual(cells[ant], oldCells[ant]) {
				t.Fatalf("step %d: cells of %q differ", steps, ant)
			}
		}
		if n := heatLogicalCount(t, s); n != int64(len(oldRows)) {
			t.Fatalf("step %d: logical heat count %d, want %d", steps, n, len(oldRows))
		}
		var done bool
		if err := s.Write(bg, func(tx *sql.Tx) error {
			var err error
			_, done, err = heatMigrateStep(bg, tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		steps++
		if done {
			break
		}
	}
	if steps != 3 {
		t.Fatalf("three antennas in %d steps", steps)
	}
	if err := s.Write(bg, func(tx *sql.Tx) error { return heatMigrateFinish(bg, tx) }); err != nil {
		t.Fatal(err)
	}
	if s.HasTable(heatLegacyTable) {
		t.Fatal("heat_v1 left")
	}
	cells, rows := heatSnapshot(t, s)
	if !reflect.DeepEqual(rows, oldRows) {
		t.Fatal("export rows differ after the migration")
	}
	for ant := range oldCells {
		if !heatEqual(cells[ant], oldCells[ant]) {
			t.Fatalf("cells of %q differ after the migration", ant)
		}
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM heat WHERE tile != `+heatTileSQL("cell")); n != 0 {
		t.Fatalf("%d rows with a wrong tile", n)
	}
}

// TestHeatMigrationMergesNewWrites: increments the writer adds to the new
// table before an antenna is moved are merged with its legacy cells.
func TestHeatMigrationMergesNewWrites(t *testing.T) {
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(23, 24))
	base := randomIncs(rng, 20000, 3000)
	extra := randomIncs(rng, 20000, 500)
	legacyHeat(t, s, base.Heat)
	want := map[string][]HeatRow{}
	for _, ant := range []string{"", "yagi", "vertical"} {
		c, err := s.HeatCells(bg, ant)
		if err != nil {
			t.Fatal(err)
		}
		want[ant] = OverlayHeat(c, ant, extra.Heat)
	}
	applyHeatV2(t, s)
	if err := s.Write(bg, func(tx *sql.Tx) error {
		for _, r := range extra.Heat {
			if _, err := tx.Exec(sqlMergeHeatTiled, r.Cell, r.Ant, r.Count, zf(r.RSSISum), r.RSSIN, ni(r.MinAlt), ni(r.MaxAlt), r.LastT); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check := func(label string) {
		for ant := range want {
			got, err := s.HeatCells(bg, ant)
			if err != nil {
				t.Fatal(err)
			}
			if !heatNear(got, want[ant]) {
				t.Fatalf("%s: cells of %q differ", label, ant)
			}
		}
	}
	check("during")
	wantN := 0
	for _, c := range want {
		wantN += len(c)
	}
	if n := heatLogicalCount(t, s); n != int64(wantN) {
		t.Fatalf("logical heat count %d during the migration, want %d", n, wantN)
	}
	for done := false; !done; {
		if err := s.Write(bg, func(tx *sql.Tx) error {
			var err error
			_, done, err = heatMigrateStep(bg, tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	check("after")
}

// heatNear: as heatEqual, RSSI sums up to the order of two additions.
func heatNear(a, b []HeatRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		y.RSSISum = x.RSSISum
		if math.Abs(a[i].RSSISum-b[i].RSSISum) > 1e-9*math.Max(1, math.Abs(b[i].RSSISum)) || !heatEqual([]HeatRow{x}, []HeatRow{y}) {
			return false
		}
	}
	return true
}

// TestRangeMigration: raw 1.0.0 range_hour rows are re-encoded in steps and
// the table rebuilt WITHOUT ROWID; every row reads back identically and the
// writer keeps merging into the rebuilt table.
func TestRangeMigration(t *testing.T) {
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(25, 26))
	const h0 = int64(1_790_000_000) / 3600 * 3600
	if err := s.Write(bg, func(tx *sql.Tx) error {
		for i := 0; i < 50; i++ {
			for _, ant := range []string{"", "yagi"} {
				hist := make([]uint32, DistHistBins)
				for j := 0; j < 40; j++ {
					hist[rng.IntN(60)] += uint32(rng.IntN(500))
				}
				sect := make([]float32, SectorBins)
				for j := range sect {
					sect[j] = rng.Float32() * 400
				}
				if _, err := tx.Exec(`INSERT INTO range_hour(t, ant, max_km, p99_km, p95_km, p90_km, p50_km, positions, aircraft,
  far_count, resid_db, resid_n, dist_hist, sector_max) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					h0+int64(i)*3600, ant, 300.5, 290.1, 280, 250, 120, 1000+i, 20, 3, -1.25, 900, EncodeDistHist(hist), EncodeSectorMax(sect)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read := func() []RangeHour {
		var all []RangeHour
		for _, ant := range []string{"", "yagi"} {
			r, err := s.RangeHours(bg, ant, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, r...)
		}
		return all
	}
	before := read()
	var cursor []byte
	steps := 0
	for done := false; !done; steps++ {
		if err := s.Write(bg, func(tx *sql.Tx) error {
			var err error
			cursor, _, done, err = rangeMigrateStep(bg, tx, cursor, 7)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rangeJSON(read()), rangeJSON(before)) {
			t.Fatalf("step %d: rows differ", steps)
		}
	}
	if steps != 15 {
		t.Fatalf("100 rows in %d steps of 7", steps)
	}
	ddl := scalar[string](t, s, `SELECT sql FROM sqlite_master WHERE name = 'range_hour'`)
	if !strings.Contains(ddl, "WITHOUT ROWID") {
		t.Fatalf("not rebuilt: %s", ddl)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM range_hour WHERE length(dist_hist) % 4 = 0 OR length(sector_max) != ?`, 4*SectorBins); n != 0 {
		t.Fatalf("%d rows not in the 1.1 form", n)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'sqlite_autoindex_range_hour%'`); n != 0 {
		t.Fatal("autoindex left")
	}
	// The writer merges into the rebuilt table.
	nan := math.NaN()
	s.AddRangeHours([]RangeHour{{T: h0, Ant: "yagi", MaxKm: 500, P99Km: nan, P95Km: nan, P90Km: nan, P50Km: nan, Positions: 5, ResidDB: nan}})
	mustFlush(t, s)
	r, err := s.RangeHours(bg, "yagi", h0, h0)
	if err != nil || len(r) != 1 || r[0].MaxKm != 500 || r[0].Positions != 1005 {
		t.Fatalf("merge after rebuild: %v %+v", err, r)
	}
}

// heatLogicalCount is the heat row count /api/db/stats reports: the
// registered counter while heat_v1 exists, else COUNT(*).
func heatLogicalCount(t *testing.T, s *Store) int64 {
	t.Helper()
	tx, err := s.DB().BeginTx(bg, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	n, ok, err := countHeatLogical(bg, tx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM heat`).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	return n
}
