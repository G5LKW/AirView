package store

import (
	"cmp"
	"database/sql"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"
)

// TestStatsLayoutMeasure is an opt-in measurement of the statistics layout
// changes on a COPY of a real database (it converts the copy in place):
//
//	AIRVIEW_DBEFF_C=<dir holding airview.db> go test ./internal/store -run TestStatsLayoutMeasure -v
func TestStatsLayoutMeasure(t *testing.T) {
	dir := os.Getenv("AIRVIEW_DBEFF_C")
	if dir == "" {
		t.Skip("set AIRVIEW_DBEFF_C to a directory with a copy of airview.db")
	}
	s, err := OpenWithOptions(dir, Options{MigrationDelay: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.DB()
	bytesOf := func(names ...string) (total int64) {
		for _, n := range names {
			var b sql.NullInt64
			if err := db.QueryRow(`SELECT SUM(pgsize) FROM dbstat WHERE name = ? OR name LIKE 'sqlite_autoindex_' || ? || '%'`, n, n).Scan(&b); err != nil {
				t.Fatal(err)
			}
			total += b.Int64
		}
		return total
	}
	count := func(q string) (n int64) {
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// ---- heat
	ants, err := queryInt64s(bg, db, `SELECT COUNT(*) FROM heat GROUP BY ant`)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	var n int
	rs, err := db.Query(`SELECT cell, count, rssi_sum, rssi_n, min_alt, max_alt, last_t FROM heat WHERE ant = ? AND count > 0`, "")
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		n++
	}
	rs.Close()
	oldRead := time.Since(t0)
	heatRows := count(`SELECT COUNT(*) FROM heat`)
	heatBefore := bytesOf("heat")
	before, err := s.HeatCells(bg, "")
	if err != nil {
		t.Fatal(err)
	}
	pagesOld, pagesNew := heatFlushPages(t, s, 60), [2]float64{}
	pagesOld300 := heatFlushPages(t, s, 300)
	t0 = time.Now()
	if err := s.Write(bg, func(tx *sql.Tx) error { _, err := tx.Exec(heatV2DDL); return err }); err != nil {
		t.Fatal(err)
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
	if err := s.Write(bg, func(tx *sql.Tx) error { return heatMigrateFinish(bg, tx) }); err != nil {
		t.Fatal(err)
	}
	heatMig := time.Since(t0)
	t0 = time.Now()
	after, err := s.HeatCells(bg, "")
	newRead := time.Since(t0)
	t0 = time.Now()
	_, _ = s.HeatCells(bg, "")
	t.Logf("heat: HeatCells again %v", time.Since(t0))
	if err != nil || !reflect.DeepEqual(len(after), len(before)) || !heatEqual(after, before) {
		t.Fatalf("heat cells differ after the migration: %v", err)
	}
	pagesNew = heatFlushPages(t, s, 60)
	pagesNew300 := heatFlushPages(t, s, 300)
	t.Logf("heat: %d rows (%v per antenna), %d B -> %d B; antenna read %v (%d rows, old full-scan query) -> %v (HeatCells); migration %v",
		heatRows, ants, heatBefore, bytesOf("heat"), oldRead, n, newRead, heatMig)
	t.Logf("heat leaf pages per flush (60 s of positions): %.0f -> %.0f (%.0f%%); 300 s: %.0f -> %.0f (%.0f%%)",
		pagesOld[0], pagesNew[0], 100*(pagesNew[0]/pagesOld[0]-1), pagesOld300[0], pagesNew300[0], 100*(pagesNew300[0]/pagesOld300[0]-1))

	// ---- range_hour
	rangeRows := count(`SELECT COUNT(*) FROM range_hour`)
	rangeBefore := bytesOf("range_hour")
	t0 = time.Now()
	var cursor []byte
	for done := false; !done; {
		if err := s.Write(bg, func(tx *sql.Tx) error {
			var err error
			cursor, _, done, err = rangeMigrateStep(bg, tx, cursor, rangeStepRows)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("range_hour: %d rows, %d B (%.0f B/row) -> %d B (%.0f B/row); migration %v", rangeRows, rangeBefore,
		float64(rangeBefore)/float64(max(rangeRows, 1)), bytesOf("range_hour"), float64(bytesOf("range_hour"))/float64(max(rangeRows, 1)), time.Since(t0))

	// ---- sealed histograms
	histBefore := bytesOf("coverage", "signal_hist", "elev_hist")
	t0 = time.Now()
	rep, err := s.SealHistDays(bg, time.Now().Unix(), 0)
	if err != nil {
		t.Fatal(err)
	}
	histAfter := bytesOf("coverage", "signal_hist", "elev_hist", "hist_day", "hist_day_key")
	t.Logf("histograms: %d B -> %d B (%.0f%%); sealed %d table-days, %d rows in %v", histBefore, histAfter,
		100*float64(histAfter)/float64(histBefore), rep.Days, rep.Rows, time.Since(t0))

	// ---- flights_hour
	t0 = time.Now()
	if err := s.rollFlightHours(bg, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	t.Logf("flights_hour: %d rows, %d B, rollup %v", count(`SELECT COUNT(*) FROM flights_hour`), bytesOf("flights_hour"), time.Since(t0))
	now := time.Now().Unix()
	for _, off := range []int64{0, 3600, 19800, -28800} {
		from := (now+off)/86400*86400 - off - 29*86400
		t0 = time.Now()
		want := legacyFlightDays(t, s, from, now+3600, off)
		tl := time.Since(t0)
		t0 = time.Now()
		got, err := s.FlightDays(bg, from, now+3600, off)
		tn := time.Since(t0)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("offset %d: rollup answer differs (%v)", off, err)
		}
		t.Logf("daily distinct aircraft, offset %+d s: 1.0.0 query %v, rollup %v (%d days, equal)", off, tl, tn, len(got))
	}
}

// heatFlushPages estimates the distinct heat leaf pages one flush of the
// given length touches: the cells of the positions of every window of that
// length in the newest hour of trails, ranked in the table's key order,
// divided into leaves of the measured rows per leaf. [0] is the mean.
func heatFlushPages(t *testing.T, s *Store, window int64) [2]float64 {
	t.Helper()
	db := s.DB()
	tiled := heatHasTile(bg, db)
	type key struct {
		ant        string
		tile, cell int64
	}
	less := func(a, b key) int {
		if tiled {
			return cmp.Or(cmp.Compare(a.tile, b.tile), cmp.Compare(a.cell, b.cell), cmp.Compare(a.ant, b.ant))
		}
		if c := cmp.Compare(a.cell, b.cell); c != 0 {
			return c
		}
		return cmp.Compare(a.ant, b.ant)
	}
	var keys []key
	rs, err := db.Query(`SELECT cell, ant FROM heat`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var k key
		if err := rs.Scan(&k.cell, &k.ant); err != nil {
			t.Fatal(err)
		}
		k.tile = HeatTile(k.cell)
		keys = append(keys, k)
	}
	rs.Close()
	slices.SortFunc(keys, less)
	var leaves int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM dbstat WHERE name = 'heat' AND pagetype = 'leaf'`).Scan(&leaves); err != nil {
		t.Fatal(err)
	}
	perLeaf := float64(len(keys)) / float64(max(leaves, 1))
	var tmax int64
	if err := db.QueryRow(`SELECT MAX(t) FROM positions`).Scan(&tmax); err != nil {
		t.Fatal(err)
	}
	antsRows, _ := db.Query(`SELECT DISTINCT ant FROM heat`)
	var ants []string
	for antsRows.Next() {
		var a string
		_ = antsRows.Scan(&a)
		ants = append(ants, a)
	}
	antsRows.Close()
	var total float64
	var windows int
	for w0 := tmax - 3600; w0+window <= tmax; w0 += window {
		pages := map[int64]bool{}
		prs, err := db.Query(`SELECT lat_e5, lon_e5 FROM positions WHERE t >= ? AND t < ?`, w0, w0+window)
		if err != nil {
			t.Fatal(err)
		}
		for prs.Next() {
			var la, lo int64
			if err := prs.Scan(&la, &lo); err != nil {
				t.Fatal(err)
			}
			cell := HeatCell(float64(la)/1e5, float64(lo)/1e5)
			for _, a := range ants {
				k := key{a, HeatTile(cell), cell}
				i, _ := slices.BinarySearchFunc(keys, k, less)
				pages[int64(float64(i)/perLeaf)] = true
			}
		}
		prs.Close()
		total += float64(len(pages))
		windows++
	}
	return [2]float64{total / float64(max(windows, 1)), perLeaf}
}
