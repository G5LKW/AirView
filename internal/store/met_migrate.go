package store

// Data migration met/2: 1.0.0 met_obs -> met_sample (met_store.go,
// docs/perf/DB-EFFICIENCY-DESIGN.md §3.9 and §5.2), and the logical table
// met_obs that exports met_sample in the 1.0.0 layout.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// metMigrateBudget is the most rows one step converts.
const metMigrateBudget = 20_000

// legacyMetObsDDL is the 1.0.0 met_obs table without its index: what
// Finish leaves behind (empty) so that statements of other code that still
// name met_obs keep preparing.
const legacyMetObsDDL = `CREATE TABLE met_obs(
  t        INTEGER NOT NULL,
  icao     INTEGER NOT NULL,
  lat      REAL,
  lon      REAL,
  alt      INTEGER,
  wind_dir REAL,
  wind_kt  REAL,
  oat_c    REAL,
  src      INTEGER NOT NULL DEFAULT 0
)`

// metObsColumns are the 1.0.0 met_obs columns in order.
var metObsColumns = []string{"t", "icao", "lat", "lon", "alt", "wind_dir", "wind_kt", "oat_c", "src"}

// sqlMetSampleAsObs selects met_sample rows in the 1.0.0 met_obs form
// (same names, same value types: REAL for the scaled columns, NULL kept).
const sqlMetSampleAsObs = `SELECT t, icao, lat_e5 / 1e5, lon_e5 / 1e5, alt, wdir_x10 / 10.0, wkt_x10 / 10.0,
  oat_x100 / 100.0, src FROM met_sample`

func init() {
	registerDataMigration(DataMigration{
		Name:     "met",
		Version:  2,
		Kinds:    KindMain,
		Estimate: estimateMetMigration,
		Step:     stepMetMigration,
		Finish:   finishMetMigration,
	})
	RegisterLogicalTable(LogicalTable{
		Name:    "met_obs",
		Kinds:   KindMain,
		Columns: metObsColumns,
		TimeCol: "t",
		Unit:    1,
		Scan:    scanLogicalMetObs,
		Count: func(ctx context.Context, q Querier) (int64, error) {
			n, _, err := countLogicalMetObs(ctx, q)
			return n, err
		},
		Active: func(ctx context.Context, q Querier) (bool, error) { return hasTableTx(ctx, q, MetSampleTable) },
	})
	registerTableCounter("met_obs", func(ctx context.Context, tx *sql.Tx) (int64, bool, error) {
		return countLogicalMetObs(ctx, tx)
	})
}

// countLogicalMetObs is the row count of the logical met_obs: the
// met_sample counter plus the legacy rows not converted yet (counted only
// while there are any: the table is empty after the migration).
func countLogicalMetObs(ctx context.Context, q querier) (int64, bool, error) {
	ok, err := hasTableTx(ctx, q, MetSampleTable)
	if err != nil || !ok {
		return 0, false, err
	}
	n, err := metSampleCount(ctx, q)
	if err != nil {
		return 0, false, err
	}
	legacy, err := legacyMetRows(ctx, q)
	if err != nil {
		return 0, false, err
	}
	return n + legacy, true, nil
}

// legacyMetRows counts met_obs (0 without a table; a cheap probe first).
func legacyMetRows(ctx context.Context, q querier) (int64, error) {
	if ok, err := hasTableTx(ctx, q, "met_obs"); err != nil || !ok {
		return 0, err
	}
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM met_obs LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var n int64
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM met_obs`).Scan(&n)
	return n, err
}

func estimateMetMigration(ctx context.Context, q Querier) (rows, peak int64, err error) {
	if ok, err := hasTableTx(ctx, q, "met_obs"); err != nil || !ok {
		return 0, 0, err
	}
	// rowid span: cheap (two seeks) and exact for an append-only table
	// pruned from the front, an upper bound otherwise.
	var lo, hi sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MIN(rowid), MAX(rowid) FROM met_obs`).Scan(&lo, &hi); err != nil {
		return 0, 0, err
	}
	if !lo.Valid {
		return 0, 0, nil
	}
	rows = hi.Int64 - lo.Int64 + 1
	// The converted rows (~33 B) are written before the pages of the
	// deleted legacy rows (~82 B) are reused: at worst one copy of the
	// smaller form.
	return rows, rows * 36, nil
}

// stepMetMigration converts the oldest legacy rows (rowid order = insertion
// order) and deletes them in the same transaction. A legacy row whose
// (t, icao) is already in met_sample (written live after the upgrade, or a
// 1.0.0 duplicate) is dropped: the stored one wins. The cursor is the last
// converted rowid (informational: converted rows are gone, so a re-run
// starts at the first remaining row).
func stepMetMigration(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) ([]byte, bool, error) {
	if ok, err := hasTableTx(ctx, tx, "met_obs"); err != nil || !ok {
		return cursor, err == nil, err
	}
	if budget <= 0 || budget > metMigrateBudget {
		budget = metMigrateBudget
	}
	var last int64
	err := tx.QueryRowContext(ctx, `SELECT rowid FROM met_obs ORDER BY rowid LIMIT 1 OFFSET ?`, budget-1).Scan(&last)
	done := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Fewer than budget rows left: convert them all.
		var hi sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(rowid) FROM met_obs`).Scan(&hi); err != nil {
			return nil, false, err
		}
		if !hi.Valid {
			return cursor, true, nil
		}
		last, done = hi.Int64, true
	case err != nil:
		return nil, false, err
	}
	// Count: with no row above the watermark (roll), every inserted row is
	// counted explicitly and the watermark moves to the newest row.
	if _, err := rollMetCount(ctx, tx); err != nil {
		return nil, false, err
	}
	// The scaling of metSampleArgs and the packing of metPackVals, in SQL
	// (round() rounds half away from zero like math.Round). Rows that
	// cannot be keyed (negative t, an ICAO key wider than 25 bits: 1.0.0
	// never wrote such rows) are dropped.
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO met_sample(`+metPackCols+`)
SELECT (t << 25) | icao, la, lo, al - 20000,
  CASE WHEN kt IS NULL THEN (CASE WHEN d IS NULL THEN NULL ELSE -1 - d END) ELSE kt * 3602 + COALESCE(d, 3601) END,
  CASE WHEN oa IS NULL THEN (CASE WHEN s = 0 THEN NULL ELSE 400000 + s END) ELSE oa * 4 + s END
FROM (SELECT t, icao, la, lo, al, oa, s,
        CASE WHEN dd IS NULL OR dd BETWEEN 0 AND 3600 THEN dd ELSE ((dd % 3600) + 3600) % 3600 END AS d,
        CASE WHEN kk >= 0 THEN kk END AS kt
      FROM (SELECT t, icao, CAST(round(lat * 1e5) AS INTEGER) AS la, CAST(round(lon * 1e5) AS INTEGER) AS lo,
              CAST(round(alt) AS INTEGER) AS al, CAST(round(wind_dir * 10) AS INTEGER) AS dd,
              CAST(round(wind_kt * 10) AS INTEGER) AS kk, CAST(round(oat_c * 100) AS INTEGER) AS oa,
              COALESCE(src, 0) & 3 AS s
            FROM met_obs WHERE rowid <= ? AND t >= 0 AND icao >= 0 AND icao < 33554432))
ORDER BY 1`, last)
	if err != nil {
		return nil, false, err
	}
	added, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM met_obs WHERE rowid <= ?`, last); err != nil {
		return nil, false, err
	}
	if added > 0 {
		cnt, err := readMetCount(ctx, tx)
		if err != nil {
			return nil, false, err
		}
		var hi int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(k), -1) FROM met_sample`).Scan(&hi); err != nil {
			return nil, false, err
		}
		if err := writeMetCount(ctx, tx, metCount{N: cnt.N + added, Hi: hi}); err != nil {
			return nil, false, err
		}
	}
	return []byte(strconv.FormatInt(last, 10)), done, nil
}

// finishMetMigration drops the emptied legacy table and its index and
// leaves an empty met_obs without index (one page) behind, so that 1.0.0
// statements naming the table still prepare.
func finishMetMigration(ctx context.Context, tx *sql.Tx) error {
	n, err := legacyMetRows(ctx, tx)
	if err != nil {
		return err
	}
	if n > 0 {
		// Rows written by legacy code after the last step: convert them
		// first (never drop data).
		if _, _, err := stepMetMigration(ctx, tx, nil, metMigrateBudget); err != nil {
			return err
		}
		if n, err = legacyMetRows(ctx, tx); err != nil || n > 0 {
			if err == nil {
				err = errors.New("met_obs not empty")
			}
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DROP INDEX IF EXISTS met_obs_t; DROP TABLE IF EXISTS met_obs; `+legacyMetObsDDL)
	return err
}

// scanLogicalMetObs streams met_obs in the 1.0.0 form: legacy rows not
// converted yet, then met_sample, in time order (legacy rows first within
// a second; met_sample rows by ICAO within a second).
func scanLogicalMetObs(ctx context.Context, q Querier, span *Span, limit int64, fn func(row []any) error) error {
	legacy, err := legacyMetRows(ctx, q)
	if err != nil {
		return err
	}
	// met_sample is filtered on its key, met_obs on t (met_obs_t).
	ws, wl := "", ""
	var argsS, argsL []any
	if span != nil {
		ws, wl = ` WHERE k >= ? AND k < ?`, ` WHERE t >= ? AND t < ?`
		argsS = []any{metKey(max(span.From, 0)), metKey(max(span.To, 0))}
		argsL = []any{span.From, span.To}
	}
	query, args := sqlMetSampleAsObs+ws+" ORDER BY k", argsS
	if legacy > 0 {
		query = `SELECT t, icao, lat, lon, alt, wind_dir, wind_kt, oat_c, src FROM (
SELECT t, icao, lat, lon, alt, wind_dir, wind_kt, oat_c, src, 0 AS o, rowid AS r FROM met_obs` + wl + `
UNION ALL
SELECT t, icao, lat_e5 / 1e5, lon_e5 / 1e5, alt, wdir_x10 / 10.0, wkt_x10 / 10.0, oat_x100 / 100.0, src, 1, k
FROM met_sample` + ws + `) ORDER BY t, o, r`
		args = append(argsL, argsS...)
	}
	if limit > 0 {
		query += " LIMIT " + strconv.FormatInt(limit, 10)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanRowsAny(rows, len(metObsColumns), fn)
}
