package store

// Compact meteorological observations (docs/perf/DB-EFFICIENCY-DESIGN.md
// §3.9).
//
// met_sample(k INT PK, lat_e5 INT, lon_e5 INT, alt_d INT, w INT, o INT;
//            virtual: t, icao, alt, wdir_x10, wkt_x10, oat_x100, src)
//
//	one aircraft-derived observation (internal/met, thinned to one per
//	aircraft per minute). The columns a reader uses are those of the design:
//	t = Unix s, icao, lat/lon = lat_e5/lon_e5 / 1e5 degrees, alt = ft,
//	wind direction = wdir_x10 / 10 degrees true (FROM), wind = wkt_x10 /
//	10 kt, OAT = oat_x100 / 100 deg C, src as met_obs.src; NULL = unknown.
//	t, icao, alt, wdir_x10, wkt_x10, oat_x100 and src are VIRTUAL
//	generated columns (no storage) computed from the stored ones:
//	  k     = t << 25 | icao (the 24-bit address plus the non-ICAO flag bit
//	          24): the rowid, so (t, icao) is unique and the table is kept
//	          in time order;
//	  alt_d = alt - 20000 (cruise levels fit in two bytes);
//	  w     = wkt_x10 * 3602 + wdir_x10 (3601 = direction unknown), or
//	          -1 - wdir_x10 when only the direction is known, NULL when
//	          neither is;
//	  o     = oat_x100 * 4 + src (src: the two source bits of
//	          internal/met), or 400000 + src when the temperature is
//	          unknown and src is not 0, NULL when neither is.
//
// It replaces the 1.0.0 rowid table met_obs (REAL columns plus the index
// met_obs_t: 82 B/row). Every read is by time, which the rowid order
// serves, so there is no secondary index. The layout was chosen on
// measurements of demo data (tool dmeasure): the design's first draft, a
// WITHOUT ROWID table keyed (t, icao) with nine stored columns, needs 31 B
// of record and ~40 B/row on disk (index B-tree leaves fill to ~83 % on
// appends); this rowid layout needs ~20 B of record plus the 8-byte rowid,
// and rowid leaves fill completely on appends (~32 B/row). The scales are
// finer than the sources (BDS 4,4 / 5,0 / 6,0: 1 kt, ~0.7 deg, 0.25 C; the
// collector stores wind and temperature rounded to 0.1) and than any
// printed value; lat/lon are rounded at the 5th decimal (~1 m) like the
// trails. A direction outside [0, 360] degrees is stored modulo 360, a
// negative wind speed as unknown, and src keeps its two defined bits (the
// collector produces nothing else).
//
// One row per (t, icao): a second observation of the same aircraft in the
// same second replaces the first (1.0.0 kept both; the collector never
// stores two in one second).
//
// The exact row count is kv[metCountKey] = "n hi": n rows have k <= hi;
// the rows above the watermark hi (at most the last hour: the hourly job
// moves it up, see rollMetCount) are counted by a key range. So an append
// of new rows never writes the counter (no kv page in the WAL of every
// commit); deletions, out-of-order inserts below the watermark and the
// migration update it in their own transaction.
//
// While data migration met/2 (met_migrate.go) runs, the not yet converted
// rows are still in met_obs; MetSamplesSince and the logical table met_obs
// read both.

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
)

// MetSampleTable is the table name.
const MetSampleTable = "met_sample"

const metSampleDDL = `CREATE TABLE IF NOT EXISTS met_sample(
  k        INTEGER PRIMARY KEY,
  lat_e5   INTEGER,
  lon_e5   INTEGER,
  alt_d    INTEGER,
  w        INTEGER,
  o        INTEGER,
  t        INTEGER AS (k >> 25) VIRTUAL,
  icao     INTEGER AS (k & 33554431) VIRTUAL,
  alt      INTEGER AS (alt_d + 20000) VIRTUAL,
  wdir_x10 INTEGER AS (CASE WHEN w < 0 THEN -1 - w WHEN w % 3602 < 3601 THEN w % 3602 END) VIRTUAL,
  wkt_x10  INTEGER AS (CASE WHEN w >= 0 THEN w / 3602 END) VIRTUAL,
  oat_x100 INTEGER AS (CASE WHEN o < 400000 THEN o >> 2 END) VIRTUAL,
  src      INTEGER AS (CASE WHEN o IS NULL THEN 0 WHEN o >= 400000 THEN o - 400000 ELSE o & 3 END) VIRTUAL
);`

// metKeyShift: k = t << metKeyShift | icao.
const metKeyShift = 25

// metKey is the first k of second t.
func metKey(t int64) int64 { return t << metKeyShift }

// metCountKey is the kv key of the met_sample row counter.
const metCountKey = "count.met_sample"

// Writing: the arguments are the design's columns (?1 t, ?2 icao, ?3
// lat_e5, ?4 lon_e5, ?5 alt, ?6 wdir_x10, ?7 wkt_x10, ?8 oat_x100, ?9 src);
// the statement packs them (one definition for the writer and the
// migration).
const (
	metPackCols = `k, lat_e5, lon_e5, alt_d, w, o`
	metPackVals = `(?1 << 25) | ?2, ?3, ?4, ?5 - 20000,
  CASE WHEN ?7 IS NULL THEN (CASE WHEN ?6 IS NULL THEN NULL ELSE -1 - ?6 END) ELSE ?7 * 3602 + COALESCE(?6, 3601) END,
  CASE WHEN ?8 IS NULL THEN (CASE WHEN (?9 & 3) = 0 THEN NULL ELSE 400000 + (?9 & 3) END) ELSE ?8 * 4 + (?9 & 3) END`
	sqlInsertMetSample  = `INSERT OR IGNORE INTO met_sample(` + metPackCols + `) VALUES(` + metPackVals + `)`
	sqlReplaceMetSample = `INSERT OR REPLACE INTO met_sample(` + metPackCols + `) VALUES(` + metPackVals + `)`
	// sqlMetSampleCols reads the design's columns.
	sqlMetSampleCols = `t, icao, lat_e5, lon_e5, alt, wdir_x10, wkt_x10, oat_x100, src`
)

func init() {
	if err := RegisterMigrationFor(KindMain, MetSampleTable, 1, metSampleDDL); err != nil {
		panic(err)
	}
	registerPruner(MetSampleTable, pruneMetSamples)
	registerHourly(MetSampleTable, func(ctx context.Context, s *Store) error {
		if s.Kind() != KindMain || !s.HasTable(MetSampleTable) {
			return nil
		}
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := rollMetCount(ctx, tx)
			return err
		})
	})
	registerTableCounter(MetSampleTable, func(ctx context.Context, tx *sql.Tx) (int64, bool, error) {
		ok, err := hasTableTx(ctx, tx, MetSampleTable)
		if err != nil || !ok {
			return 0, false, err
		}
		n, err := metSampleCount(ctx, tx)
		return n, err == nil, err
	})
}

// metScaled maps v to round(v*scale) as an SQL integer; NaN/Inf become NULL.
func metScaled(v, scale float64) any {
	if !isNum(v) {
		return nil
	}
	const lim = 1e15
	return int64(math.Round(math.Max(-lim, math.Min(lim, v*scale))))
}

// metSampleArgs are the design's column values of one row, in
// sqlInsertMetSample argument order.
func metSampleArgs(r *MetObs) []any {
	dir, kt := metScaled(r.WindDir, 10), metScaled(r.WindKt, 10)
	if d, ok := dir.(int64); ok && (d < 0 || d > 3600) {
		dir = (d%3600 + 3600) % 3600
	}
	if k, ok := kt.(int64); ok && k < 0 {
		kt = nil
	}
	return []any{r.T, int64(r.ICAO), metScaled(r.Lat, 1e5), metScaled(r.Lon, 1e5), ni(r.Alt),
		dir, kt, metScaled(r.OATC, 100), int64(r.Src)}
}

// metUnscale is the inverse of metScaled for a nullable integer column.
func metUnscale(v sql.NullInt64, scale float64) float64 {
	if !v.Valid {
		return math.NaN()
	}
	return float64(v.Int64) / scale
}

// metCount is the decoded kv[metCountKey]: N rows with k <= Hi (Hi -1:
// none counted yet).
type metCount struct{ N, Hi int64 }

func readMetCount(ctx context.Context, q querier) (metCount, error) {
	c := metCount{Hi: -1}
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, metCountKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if f := strings.Fields(v); len(f) == 2 {
		n, err1 := strconv.ParseInt(f[0], 10, 64)
		hi, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 == nil && err2 == nil {
			c = metCount{N: max(n, 0), Hi: hi}
		}
	}
	return c, nil
}

func writeMetCount(ctx context.Context, ex kvExecer, c metCount) error {
	_, err := ex.ExecContext(ctx, sqlSetKV, metCountKey,
		strconv.FormatInt(max(c.N, 0), 10)+" "+strconv.FormatInt(c.Hi, 10), time.Now().Unix())
	return err
}

// rollMetCount moves the watermark to the newest row (counting the rows
// above the old one: a key-range scan of at most the last hour). It
// returns the new state.
func rollMetCount(ctx context.Context, tx *sql.Tx) (metCount, error) {
	c, err := readMetCount(ctx, tx)
	if err != nil {
		return c, err
	}
	var above int64
	var hi sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), MAX(k) FROM met_sample WHERE k > ?`, c.Hi).Scan(&above, &hi); err != nil {
		return c, err
	}
	if above == 0 {
		return c, nil
	}
	c.N, c.Hi = c.N+above, hi.Int64
	return c, writeMetCount(ctx, tx, c)
}

// metSampleCount returns the exact row count of met_sample.
func metSampleCount(ctx context.Context, q querier) (int64, error) {
	c, err := readMetCount(ctx, q)
	if err != nil {
		return 0, err
	}
	var above int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM met_sample WHERE k > ?`, c.Hi).Scan(&above); err != nil {
		return 0, err
	}
	return c.N + above, nil
}

// AddMetSamples appends aircraft-derived meteorological observations
// (met_sample). Low-priority rows of the group commit. Rows whose ICAO does
// not fit an address key (24 bits plus the non-ICAO flag) are skipped.
func (b *Batch) AddMetSamples(rows []MetObs) {
	rows = slices.Clone(rows)
	// Key order: the rows are appended to the table's last page in sequence.
	slices.SortStableFunc(rows, func(a, b MetObs) int {
		if c := cmpKey64(a.T, b.T); c != 0 {
			return c
		}
		return cmpKey64(int64(a.ICAO), int64(b.ICAO))
	})
	b.add(MetSampleTable, tierLow, len(rows), func(tc *txc) error {
		ins, err := tc.stmt(sqlInsertMetSample)
		if err != nil {
			return err
		}
		cnt, err := readMetCount(tc.ctx, tc.tx)
		if err != nil {
			return err
		}
		var below int64 // inserted under the watermark: counted explicitly
		for i := range rows {
			if rows[i].ICAO >= 1<<metKeyShift || rows[i].T < 0 {
				continue
			}
			args := metSampleArgs(&rows[i])
			res, err := ins.ExecContext(tc.ctx, args...)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				if metKey(rows[i].T)|int64(rows[i].ICAO) <= cnt.Hi {
					below += n
				}
				continue
			}
			// Same aircraft, same second: the later observation wins.
			if err := tc.exec(sqlReplaceMetSample, args...); err != nil {
				return err
			}
		}
		if below == 0 {
			return nil
		}
		cnt.N += below
		return writeMetCount(tc.ctx, tc.tx, cnt)
	})
}

// AddMetSamples queues Batch.AddMetSamples.
func (s *Store) AddMetSamples(rows []MetObs) { s.queue1(func(b *Batch) { b.AddMetSamples(rows) }) }

// MetSamplesSince returns the stored observations with t >= from in
// ascending time order (by ICAO within a second), at most limit rows (<= 0:
// 50,000), from met_sample and, while data migration met/2 has not
// converted them yet, met_obs (a legacy row comes first among rows of the
// same second).
func (s *Store) MetSamplesSince(ctx context.Context, from int64, limit int) ([]MetObs, error) {
	if limit <= 0 {
		limit = 50_000
	}
	db := s.DB()
	var out []MetObs
	if ok, err := hasTableTx(ctx, db, MetSampleTable); err != nil {
		return nil, err
	} else if ok {
		rows, err := db.QueryContext(ctx, `SELECT `+sqlMetSampleCols+` FROM met_sample WHERE k >= ? ORDER BY k LIMIT ?`,
			metKey(max(from, 0)), limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var r MetObs
			var icao, src int64
			var lat, lon, alt, dir, kt, oat sql.NullInt64
			if err := rows.Scan(&r.T, &icao, &lat, &lon, &alt, &dir, &kt, &oat, &src); err != nil {
				return nil, err
			}
			r.ICAO, r.Src = uint32(icao), uint8(src)
			r.Lat, r.Lon, r.Alt = metUnscale(lat, 1e5), metUnscale(lon, 1e5), metUnscale(alt, 1)
			r.WindDir, r.WindKt, r.OATC = metUnscale(dir, 10), metUnscale(kt, 10), metUnscale(oat, 100)
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	legacy, err := legacyMetObs(ctx, db, from, limit)
	if err != nil || len(legacy) == 0 {
		return out, err
	}
	merged := make([]MetObs, 0, min(limit, len(out)+len(legacy)))
	i, j := 0, 0
	for len(merged) < limit && (i < len(legacy) || j < len(out)) {
		if j >= len(out) || (i < len(legacy) && legacy[i].T <= out[j].T) {
			merged = append(merged, legacy[i])
			i++
		} else {
			merged = append(merged, out[j])
			j++
		}
	}
	return merged, nil
}

// legacyMetObs reads 1.0.0 met_obs rows with t >= from (none once the
// migration emptied the table).
func legacyMetObs(ctx context.Context, q querier, from int64, limit int) ([]MetObs, error) {
	if ok, err := hasTableTx(ctx, q, "met_obs"); err != nil || !ok {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT t, icao, lat, lon, alt, wind_dir, wind_kt, oat_c, src
FROM met_obs WHERE t >= ? ORDER BY t LIMIT ?`, from, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetObs
	for rows.Next() {
		var r MetObs
		var icao, src sql.NullInt64
		if err := rows.Scan(&r.T, &icao, (*nfloat)(&r.Lat), (*nfloat)(&r.Lon), (*nfloat)(&r.Alt),
			(*nfloat)(&r.WindDir), (*nfloat)(&r.WindKt), (*nfloat)(&r.OATC), &src); err != nil {
			return nil, err
		}
		r.ICAO, r.Src = uint32(icao.Int64), uint8(src.Int64)
		out = append(out, r)
	}
	return out, rows.Err()
}

// pruneMetSamples applies Storage.TrailRetentionDays to met_sample (the
// pruner of met_obs in 1.0.0), oldest first, in chunks of pruneRowChunk
// rows; the deleted rows are reported as PruneResult.MetObs.
func pruneMetSamples(ctx context.Context, s *Store, cfg model.StorageConfig, now int64, res *PruneResult) error {
	if s.Kind() != KindMain || cfg.TrailRetentionDays <= 0 || !s.HasTable(MetSampleTable) {
		return nil
	}
	cutoff := metKey(now - int64(cfg.TrailRetentionDays)*86400)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var n int64
		done := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			n, done = 0, false
			// Every row is under the watermark after the roll: the deleted
			// ones come off N.
			cnt, err := rollMetCount(ctx, tx)
			if err != nil {
				return err
			}
			var boundary int64
			err = tx.QueryRowContext(ctx, `SELECT k FROM met_sample WHERE k < ? ORDER BY k LIMIT 1 OFFSET ?`,
				cutoff, pruneRowChunk-1).Scan(&boundary)
			var r sql.Result
			switch {
			case errors.Is(err, sql.ErrNoRows):
				done = true
				r, err = tx.ExecContext(ctx, `DELETE FROM met_sample WHERE k < ?`, cutoff)
			case err != nil:
				return err
			default:
				r, err = tx.ExecContext(ctx, `DELETE FROM met_sample WHERE k <= ?`, boundary)
			}
			if err != nil {
				return err
			}
			n, _ = r.RowsAffected()
			if n == 0 {
				return nil
			}
			cnt.N -= n
			return writeMetCount(ctx, tx, cnt)
		})
		if err != nil {
			return err
		}
		res.MetObs += n
		if done {
			return nil
		}
	}
}
