package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Layout migrations of the statistics tables of airview.db
// (DB-EFFICIENCY-DESIGN §3.6, §3.11, §5.2):
//
//   - heat 2: heat is rebuilt keyed (tile, cell, ant) WITHOUT ROWID. The DDL
//     step renames the 1.0.0 table to heat_v1 and creates the new one (the
//     writer UPSERTs into the new table at once); the data migration then
//     moves one antenna per step from heat_v1 (merging with what the writer
//     added meanwhile, the UPSERT rules), and drops heat_v1 when empty.
//     Readers (HeatCells, the heat logical table) merge heat_v1 while it
//     exists.
//   - range 2: range_hour gets the compact distance histogram (lossless
//     varint form of compact.go; the sector maxima keep their exact float32
//     form) and is rebuilt WITHOUT ROWID (no autoindex). The steps re-encode
//     1,000 rows each in place (readers decode both forms), the last step
//     rebuilds the table in one transaction.
//
// Both are KindMain only: stations.db already has heat keyed (ant, cell)
// and compact range_hour BLOBs.

const (
	heatLegacyTable = "heat_v1"
	rangeStepRows   = 1000
)

// heatTiled reports that airview.db's heat is keyed (tile, cell, ant): the
// "heat" 2 DDL is registered, the writer prepares sqlMergeHeatTiled on
// airview.db (kind.go mainPrepared) and Batch.MergeHeat writes with
// heatMergeSQL (ops.go). It switches itself on when the writer side is in
// place (hooks DBEFF-C -> kind.go / ops.go), so the tree never runs the
// new layout with the old UPSERT.
var heatTiled = slices.Contains(mainPrepared, sqlMergeHeatTiled) && !slices.Contains(mainPrepared, sqlMergeHeat)

const heatV2DDL = `ALTER TABLE heat RENAME TO heat_v1;
CREATE TABLE heat(
  cell     INTEGER NOT NULL,
  ant      TEXT NOT NULL,
  count    INTEGER NOT NULL DEFAULT 0,
  rssi_sum REAL NOT NULL DEFAULT 0,
  rssi_n   INTEGER NOT NULL DEFAULT 0,
  min_alt  INTEGER,
  max_alt  INTEGER,
  last_t   INTEGER NOT NULL DEFAULT 0,
  tile     INTEGER NOT NULL,
  PRIMARY KEY(tile, cell, ant)
) WITHOUT ROWID;`

// heatCols are the 1.0.0 columns of heat in their order (exports).
var heatCols = []string{"cell", "ant", "count", "rssi_sum", "rssi_n", "min_alt", "max_alt", "last_t"}

func init() {
	if heatTiled {
		if err := RegisterMigrationFor(KindMain, "heat", 2, heatV2DDL); err != nil {
			panic(err)
		}
	}
	registerHourly("range_hour", compactRecentRangeHours)
	registerStatsMigrations()
}

// heatMigrateStep moves the cells of one antenna from heat_v1 into heat.
// done is true when heat_v1 is empty (or gone).
func heatMigrateStep(ctx context.Context, tx *sql.Tx) (moved int64, done bool, err error) {
	if !txHasTable(ctx, tx, heatLegacyTable) {
		return 0, true, nil
	}
	var ant string
	err = tx.QueryRowContext(ctx, `SELECT ant FROM `+heatLegacyTable+` LIMIT 1`).Scan(&ant)
	if isNoRows(err) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO heat(cell, ant, count, rssi_sum, rssi_n, min_alt, max_alt, last_t, tile)
SELECT cell, ant, count, rssi_sum, rssi_n, min_alt, max_alt, last_t, `+heatTileSQL("cell")+` AS tile
FROM `+heatLegacyTable+` WHERE ant = ? ORDER BY tile, cell, ant
ON CONFLICT(tile, cell, ant) DO UPDATE SET
  `+heatMergeSet, ant)
	if err != nil {
		return 0, false, err
	}
	moved, _ = r.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+heatLegacyTable+` WHERE ant = ?`, ant); err != nil {
		return 0, false, err
	}
	var more int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM `+heatLegacyTable+` LIMIT 1`).Scan(&more)
	if isNoRows(err) {
		return moved, true, nil
	}
	return moved, false, err
}

// heatMigrateFinish drops the emptied legacy table.
func heatMigrateFinish(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+heatLegacyTable)
	return err
}

func txHasTable(ctx context.Context, q querier, name string) bool {
	var one int
	return q.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&one) == nil
}

// rangeMigrateStep re-encodes up to budget range_hour rows after cursor
// (t, ant) with the compact distance histogram; when the table is walked it
// rebuilds it WITHOUT ROWID. next is the new cursor.
func rangeMigrateStep(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) (next []byte, n int, done bool, err error) {
	if budget <= 0 {
		budget = rangeStepRows
	}
	t0, a0 := int64(-1<<62), ""
	if c := string(cursor); c != "" {
		if i := strings.IndexByte(c, ' '); i > 0 {
			if v, perr := strconv.ParseInt(c[:i], 10, 64); perr == nil {
				t0, a0 = v, c[i+1:]
			}
		}
	}
	rs, err := tx.QueryContext(ctx, `SELECT t, ant, dist_hist FROM range_hour WHERE (t, ant) > (?, ?) ORDER BY t, ant LIMIT ?`, t0, a0, budget)
	if err != nil {
		return cursor, 0, false, err
	}
	type row struct {
		t    int64
		ant  string
		hist []byte
	}
	var rows []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.t, &r.ant, &r.hist); err != nil {
			rs.Close()
			return cursor, 0, false, err
		}
		rows = append(rows, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return cursor, 0, false, err
	}
	if err := recodeRangeRows(ctx, tx, func(yield func(t int64, ant string, hist []byte) error) error {
		for _, r := range rows {
			if err := yield(r.t, r.ant, r.hist); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return cursor, 0, false, err
	}
	if len(rows) == budget {
		last := rows[len(rows)-1]
		return []byte(strconv.FormatInt(last.t, 10) + " " + last.ant), len(rows), false, nil
	}
	if err := rebuildRangeHour(ctx, tx); err != nil {
		return cursor, len(rows), false, err
	}
	return nil, len(rows), true, nil
}

// recodeRangeRows writes the compact form of every raw distance histogram
// the iterator yields.
func recodeRangeRows(ctx context.Context, tx *sql.Tx, each func(yield func(t int64, ant string, hist []byte) error) error) error {
	var st *sql.Stmt
	defer func() {
		if st != nil {
			st.Close()
		}
	}()
	return each(func(t int64, ant string, hist []byte) error {
		if len(hist) == 0 || isCompact(hist) {
			return nil
		}
		if st == nil {
			var err error
			if st, err = tx.PrepareContext(ctx, `UPDATE range_hour SET dist_hist = ? WHERE t = ? AND ant = ?`); err != nil {
				return err
			}
		}
		_, err := st.ExecContext(ctx, EncodeDistHistCompact(DecodeDistHist(hist)), t, ant)
		return err
	})
}

// rebuildRangeHour turns a rowid range_hour into a WITHOUT ROWID table with
// the same columns (no-op when it already is one).
func rebuildRangeHour(ctx context.Context, tx *sql.Tx) error {
	var ddl string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'range_hour'`).Scan(&ddl); err != nil {
		if isNoRows(err) {
			return nil
		}
		return err
	}
	if strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID") {
		return nil
	}
	i := strings.Index(ddl, "(")
	if i < 0 {
		return errors.New("store: range_hour: unexpected DDL")
	}
	newDDL := "CREATE TABLE range_hour_new" + ddl[i:] + " WITHOUT ROWID"
	cols, err := orderedColumns(ctx, tx, "range_hour")
	if err != nil {
		return err
	}
	list := strings.Join(cols, ", ")
	for _, q := range []string{
		`DROP TABLE IF EXISTS range_hour_new`,
		newDDL,
		`INSERT INTO range_hour_new(` + list + `) SELECT ` + list + ` FROM range_hour ORDER BY t, ant`,
		`DROP TABLE range_hour`,
		`ALTER TABLE range_hour_new RENAME TO range_hour`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func orderedColumns(ctx context.Context, q querier, table string) ([]string, error) {
	rs, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var n string
		if err := rs.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, quoteIdent(n))
	}
	return out, rs.Err()
}

// compactRecentRangeHours is the hourly job that re-encodes raw distance
// histograms written in the last three days (a writer that still writes
// the raw form; a no-op once Batch.AddRangeHours writes compact BLOBs).
func compactRecentRangeHours(ctx context.Context, s *Store) error {
	if s.kind != KindMain || !s.HasTable("range_hour") {
		return nil
	}
	since := floorTo(time.Now().Unix()-3*86400, 3600)
	return s.Write(ctx, func(tx *sql.Tx) error {
		rs, err := tx.QueryContext(ctx, `SELECT t, ant, dist_hist FROM range_hour WHERE t >= ? AND length(dist_hist) % 4 = 0 AND length(dist_hist) > 0`, since)
		if err != nil {
			return err
		}
		type row struct {
			t    int64
			ant  string
			hist []byte
		}
		var rows []row
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.t, &r.ant, &r.hist); err != nil {
				rs.Close()
				return err
			}
			rows = append(rows, r)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		return recodeRangeRows(ctx, tx, func(yield func(int64, string, []byte) error) error {
			for _, r := range rows {
				if err := yield(r.t, r.ant, r.hist); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// registerStatsMigrations registers the data migrations and logical tables
// of the statistics tables (KindMain).
func registerStatsMigrations() {
	if heatTiled {
		registerDataMigration(DataMigration{
			Name: "heat", Version: 2, Kinds: KindMain,
			Estimate: func(ctx context.Context, q Querier) (int64, int64, error) {
				if !txHasTable(ctx, q, heatLegacyTable) {
					return 0, 0, nil
				}
				var n int64
				err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+heatLegacyTable).Scan(&n)
				return n, n * 48, err // the largest antenna is copied before its rows are freed
			},
			Step: func(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) ([]byte, bool, error) {
				_, done, err := heatMigrateStep(ctx, tx)
				return nil, done, err
			},
			Finish: heatMigrateFinish,
		})
	}
	// While heat_v1 exists (migration "heat" 2 pending) the logical heat
	// table is heat plus the legacy cells not yet merged into it, so
	// /api/db/stats keeps the 1.0.0 count throughout the migration. Otherwise
	// ok = false: the plain COUNT(*) of heat.
	registerTableCounter("heat", countHeatLogical)
	// Active only on a database whose heat has the tile column.
	RegisterLogicalTable(LogicalTable{
		Name: "heat", Kinds: KindMain, Columns: heatCols,
		Scan: scanHeatLogical,
		Active: func(ctx context.Context, q Querier) (bool, error) {
			return heatHasTile(ctx, q), nil
		},
	})
	registerDataMigration(DataMigration{
		Name: "range", Version: 2, Kinds: KindMain,
		Estimate: func(ctx context.Context, q Querier) (int64, int64, error) {
			if !txHasTable(ctx, q, "range_hour") {
				return 0, 0, nil
			}
			var n, bytes sql.NullInt64
			err := q.QueryRowContext(ctx, `SELECT COUNT(*), SUM(length(dist_hist) + length(sector_max) + 80) FROM range_hour`).Scan(&n, &bytes)
			return n.Int64, bytes.Int64, err // the rebuild copies the (packed) table once
		},
		Step: func(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) ([]byte, bool, error) {
			next, _, done, err := rangeMigrateStep(ctx, tx, cursor, min(max(budget, 50), rangeStepRows))
			return next, done, err
		},
	})
	registerDataMigration(DataMigration{
		Name: flightsHourTable, Version: 1, Kinds: KindMain,
		Estimate: func(ctx context.Context, q Querier) (int64, int64, error) {
			var lo, hi sql.NullInt64
			err := q.QueryRowContext(ctx, `SELECT (SELECT MIN(start) FROM flights), (SELECT MAX(start) FROM flights)`).Scan(&lo, &hi)
			if !lo.Valid {
				return 0, 0, err
			}
			hours := (hi.Int64-lo.Int64)/3600 + 1
			return hours, hours * 1200, err
		},
		Step: func(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) ([]byte, bool, error) {
			done, err := rollFlightHoursTx(ctx, tx, time.Now().Unix(), flightsHourStep)
			return nil, done, err
		},
	})
	registerDataMigration(DataMigration{
		Name: histDayTable, Version: 1, Kinds: KindMain,
		Estimate: func(ctx context.Context, q Querier) (int64, int64, error) {
			var n int64
			for _, t := range []string{"coverage", "signal_hist", "elev_hist"} {
				var c int64
				if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&c); err != nil {
					return 0, 0, err
				}
				n += c
			}
			return n, 0, nil // sealing a day frees more than it writes
		},
		Step: func(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) ([]byte, bool, error) {
			_, sealed, err := sealOneDay(ctx, tx, DayOf(time.Now().Unix())-histSealAge-1)
			return nil, !sealed, err
		},
	})
	for _, tbl := range []int{histCoverage, histSignal, histElev} {
		tbl := tbl
		RegisterLogicalTable(LogicalTable{
			Name: histTableNames[tbl], Kinds: KindMain, Columns: histCols[tbl],
			TimeCol: "day", Unit: 86400,
			Scan: func(ctx context.Context, q Querier, span *Span, limit int64, fn func([]any) error) error {
				return scanHistLogical(ctx, q, tbl, span, limit, fn)
			},
			Count: func(ctx context.Context, q Querier) (int64, error) {
				var live, sealed sql.NullInt64
				err := q.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM `+histTableNames[tbl]+`), (SELECT SUM(n) FROM hist_day WHERE tbl = ?)`, tbl).Scan(&live, &sealed)
				return live.Int64 + sealed.Int64, err
			},
			Active: func(ctx context.Context, q Querier) (bool, error) {
				return txHasTable(ctx, q, histDayTable), nil
			},
		})
	}
}

// heatHasTile reports that heat has the 1.1 layout.
func heatHasTile(ctx context.Context, q Querier) bool {
	var one int
	return q.QueryRowContext(ctx, `SELECT 1 FROM pragma_table_info('heat') WHERE name = 'tile'`).Scan(&one) == nil
}

// scanHeatLogical streams heat as 1.0.0 held it: the 1.0.0 columns in
// primary-key order (cell, ant), heat_v1 merged in while it exists.
func scanHeatLogical(ctx context.Context, q Querier, span *Span, limit int64, fn func([]any) error) error {
	cols := strings.Join(heatCols, ", ")
	query := `SELECT ` + cols + ` FROM heat ORDER BY cell, ant`
	if txHasTable(ctx, q, heatLegacyTable) {
		query = `SELECT cell, ant, SUM(count), SUM(rssi_sum), SUM(rssi_n), MIN(min_alt), MAX(max_alt), MAX(last_t) FROM (
  SELECT ` + cols + ` FROM heat UNION ALL SELECT ` + cols + ` FROM ` + heatLegacyTable + `)
GROUP BY cell, ant ORDER BY cell, ant`
	}
	if limit > 0 {
		query += ` LIMIT ` + strconv.FormatInt(limit, 10)
	}
	rs, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rs.Close()
	return scanRowsAny(rs, len(heatCols), fn)
}

// countHeatLogical counts the logical heat rows while heat_v1 exists: every
// row of heat plus the legacy cells that have no row in heat yet.
func countHeatLogical(ctx context.Context, tx *sql.Tx) (int64, bool, error) {
	if !txHasTable(ctx, tx, heatLegacyTable) || !heatHasTile(ctx, tx) {
		return 0, false, nil
	}
	var n int64
	err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM heat) +
  (SELECT COUNT(*) FROM `+heatLegacyTable+` v WHERE NOT EXISTS (
     SELECT 1 FROM heat h WHERE h.tile = `+heatTileSQL("v.cell")+` AND h.cell = v.cell AND h.ant = v.ant))`).Scan(&n)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}
