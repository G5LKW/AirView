package store

import (
	"cmp"
	"database/sql"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

// ---------------------------------------------------------------- SQL text

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

const sqlUpsertAircraft = `INSERT INTO aircraft(icao, first_seen, last_seen, callsign, reg, type, operator, country, category,
  msgs, flights, positions, max_dist_km, max_alt, src_mask, flags)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(icao) DO UPDATE SET
  first_seen  = MIN(first_seen, excluded.first_seen),
  last_seen   = MAX(last_seen, excluded.last_seen),
  callsign    = CASE WHEN excluded.callsign != '' THEN excluded.callsign ELSE callsign END,
  reg         = CASE WHEN excluded.reg      != '' THEN excluded.reg      ELSE reg      END,
  type        = CASE WHEN excluded.type     != '' THEN excluded.type     ELSE type     END,
  operator    = CASE WHEN excluded.operator != '' THEN excluded.operator ELSE operator END,
  country     = CASE WHEN excluded.country  != '' THEN excluded.country  ELSE country  END,
  category    = CASE WHEN excluded.category != '' THEN excluded.category ELSE category END,
  msgs        = msgs + excluded.msgs,
  flights     = flights + excluded.flights,
  positions   = positions + excluded.positions,
  max_dist_km = MAX(max_dist_km, excluded.max_dist_km),
  max_alt     = COALESCE(MAX(max_alt, excluded.max_alt), max_alt, excluded.max_alt),
  src_mask    = src_mask | excluded.src_mask,
  flags       = flags | excluded.flags`

const sqlUpsertFlight = `INSERT INTO flights(id, icao, callsign, squawk, start, "end", msgs, positions, src_mask,
  max_dist_km, max_dist_brg, min_dist_km, min_alt, max_alt, max_gs, rssi_min, rssi_max, rssi_avg,
  first_lat, first_lon, last_lat, last_lon, emergency)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  icao = excluded.icao, callsign = excluded.callsign, squawk = excluded.squawk,
  start = excluded.start, "end" = excluded."end", msgs = excluded.msgs, positions = excluded.positions,
  src_mask = excluded.src_mask, max_dist_km = excluded.max_dist_km, max_dist_brg = excluded.max_dist_brg,
  min_dist_km = excluded.min_dist_km, min_alt = excluded.min_alt, max_alt = excluded.max_alt,
  max_gs = excluded.max_gs, rssi_min = excluded.rssi_min, rssi_max = excluded.rssi_max,
  rssi_avg = excluded.rssi_avg, first_lat = excluded.first_lat, first_lon = excluded.first_lon,
  last_lat = excluded.last_lat, last_lon = excluded.last_lon, emergency = excluded.emergency,
  dep = NULL, arr = NULL`

// sqlRefreshFlight updates a stored flight without assigning icao, start,
// dep or arr: SQLite rewrites an index entry whenever an indexed column is
// assigned, even to its old value (DB-BASELINE §5.2), so assigning only the
// columns that change keeps flights_icao / flights_dep / flights_arr
// untouched. It matches only when the row's icao and start are the given
// ones and the flight is unresolved (dep and arr NULL) - exactly the cases
// in which sqlUpsertFlight leaves those columns as they are; anything else
// falls back to sqlUpsertFlight, so the stored row is always the row 1.0.0
// stored.
const sqlRefreshFlight = `UPDATE flights SET callsign = ?2, squawk = ?3, "end" = ?4, msgs = ?5, positions = ?6, src_mask = ?7,
  max_dist_km = ?8, max_dist_brg = ?9, min_dist_km = ?10, min_alt = ?11, max_alt = ?12, max_gs = ?13,
  rssi_min = ?14, rssi_max = ?15, rssi_avg = ?16, first_lat = ?17, first_lon = ?18, last_lat = ?19, last_lon = ?20,
  emergency = ?21
WHERE id = ?1 AND icao = ?22 AND start = ?23 AND dep IS NULL AND arr IS NULL`

// sqlRefreshAircraft merges a sighting into an existing aircraft row whose
// first_seen is not after the sighting's and whose last_seen is before it:
// the merge of sqlUpsertAircraft without assigning first_seen (which MIN
// would leave as it is). A row that does not match falls back to
// sqlUpsertAircraft.
const sqlRefreshAircraft = `UPDATE aircraft SET
  last_seen   = ?3,
  callsign    = CASE WHEN ?4 != '' THEN ?4 ELSE callsign END,
  reg         = CASE WHEN ?5 != '' THEN ?5 ELSE reg      END,
  type        = CASE WHEN ?6 != '' THEN ?6 ELSE type     END,
  operator    = CASE WHEN ?7 != '' THEN ?7 ELSE operator END,
  country     = CASE WHEN ?8 != '' THEN ?8 ELSE country  END,
  category    = CASE WHEN ?9 != '' THEN ?9 ELSE category END,
  msgs        = msgs + ?10,
  flights     = flights + ?11,
  positions   = positions + ?12,
  max_dist_km = MAX(max_dist_km, ?13),
  max_alt     = COALESCE(MAX(max_alt, ?14), max_alt, ?14),
  src_mask    = src_mask | ?15,
  flags       = flags | ?16
WHERE icao = ?1 AND first_seen <= ?2 AND last_seen < ?3`

// nullMax / nullMin merge two nullable numbers in an UPSERT: NULL loses.
func nullMax(col string) string {
	return col + " = COALESCE(MAX(" + col + ", excluded." + col + "), " + col + ", excluded." + col + ")"
}

func nullMin(col string) string {
	return col + " = COALESCE(MIN(" + col + ", excluded." + col + "), " + col + ", excluded." + col + ")"
}

// statsInsertCols is the column order of a stats_minute insert.
func statsInsertCols() []string {
	cols := []string{"t", "ant"}
	cols = append(cols, statsCounterCols...)
	cols = append(cols, statsGaugeCols...)
	return append(cols, "max_km", "rssi_avg", "rssi_min", "rssi_max")
}

var sqlMergeStatsMinute = func() string {
	cols := append(statsInsertCols(), "secs")
	var set []string
	for _, c := range statsCounterCols {
		set = append(set, c+" = "+c+" + excluded."+c)
	}
	for _, c := range statsGaugeCols {
		set = append(set, c+" = MAX("+c+", excluded."+c+")")
	}
	set = append(set,
		nullMax("max_km"),
		`rssi_avg = CASE
      WHEN rssi_avg IS NULL THEN excluded.rssi_avg
      WHEN excluded.rssi_avg IS NULL THEN rssi_avg
      WHEN frames + excluded.frames > 0
        THEN (rssi_avg * frames + excluded.rssi_avg * excluded.frames) / (frames + excluded.frames)
      ELSE (rssi_avg + excluded.rssi_avg) / 2.0 END`,
		nullMin("rssi_min"),
		nullMax("rssi_max"),
		// Covered seconds add up (two flushes of one minute), capped at a
		// minute; NULL (unknown = a full minute) wins.
		`secs = CASE WHEN secs IS NULL OR excluded.secs IS NULL THEN NULL ELSE MIN(60.0, secs + excluded.secs) END`,
	)
	return "INSERT INTO stats_minute(" + strings.Join(cols, ", ") + ")\nVALUES(" + placeholders(len(cols)) +
		")\nON CONFLICT(t, ant) DO UPDATE SET\n  " + strings.Join(set, ",\n  ")
}()

// sqlLowerRollupMark re-opens already rolled-up hours when a minute row
// arrives late (e.g. the tail of an hour written after a restart).
const sqlLowerRollupMark = `UPDATE kv SET value = ?1, updated = ?2
WHERE key = '` + kvRollupHour + `' AND CAST(value AS INTEGER) > ?1`

var sqlRollupHours = func() string {
	cols := statsInsertCols()
	sel := []string{"t - t % 3600", "ant"}
	for _, c := range statsCounterCols {
		sel = append(sel, "SUM("+c+")")
	}
	for _, c := range statsGaugeCols {
		sel = append(sel, "MAX("+c+")")
	}
	sel = append(sel,
		"MAX(max_km)",
		`CASE WHEN SUM(CASE WHEN rssi_avg IS NOT NULL THEN frames ELSE 0 END) > 0
      THEN SUM(CASE WHEN rssi_avg IS NOT NULL THEN rssi_avg * frames ELSE 0 END) * 1.0
           / SUM(CASE WHEN rssi_avg IS NOT NULL THEN frames ELSE 0 END)
      ELSE AVG(rssi_avg) END`,
		"MIN(rssi_min)", "MAX(rssi_max)", "AVG(aircraft)", "COUNT(*)", "SUM(COALESCE(secs, 60.0))",
	)
	return "INSERT OR REPLACE INTO stats_hour(" + strings.Join(cols, ", ") + ", aircraft_avg, minutes, secs)\nSELECT " +
		strings.Join(sel, ",\n  ") + "\nFROM stats_minute WHERE t >= ? AND t < ? GROUP BY 1, 2"
}()

const sqlGetRangeHour = `SELECT max_km, p99_km, p95_km, p90_km, p50_km, positions, aircraft, far_count,
  resid_db, resid_n, dist_hist, sector_max FROM range_hour WHERE t = ? AND ant = ?`

const sqlPutRangeHour = `INSERT OR REPLACE INTO range_hour(t, ant, max_km, p99_km, p95_km, p90_km, p50_km,
  positions, aircraft, far_count, resid_db, resid_n, dist_hist, sector_max)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

const sqlMergeCoverage = `INSERT INTO coverage(day, ant, brg_bin, alt_band, max_km, count, rssi_sum, rssi_n)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(day, ant, brg_bin, alt_band) DO UPDATE SET
  max_km = MAX(max_km, excluded.max_km), count = count + excluded.count,
  rssi_sum = rssi_sum + excluded.rssi_sum, rssi_n = rssi_n + excluded.rssi_n`

const sqlMergeSignalHist = `INSERT INTO signal_hist(day, ant, dist_bin, rssi_bin, count) VALUES(?,?,?,?,?)
ON CONFLICT(day, ant, dist_bin, rssi_bin) DO UPDATE SET count = count + excluded.count`

const sqlMergeElevHist = `INSERT INTO elev_hist(day, ant, elev_bin, count, rssi_sum, resid_sum) VALUES(?,?,?,?,?,?)
ON CONFLICT(day, ant, elev_bin) DO UPDATE SET
  count = count + excluded.count, rssi_sum = rssi_sum + excluded.rssi_sum,
  resid_sum = resid_sum + excluded.resid_sum`

var sqlMergeHeat = `INSERT INTO heat(cell, ant, count, rssi_sum, rssi_n, min_alt, max_alt, last_t) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(cell, ant) DO UPDATE SET
  count = count + excluded.count, rssi_sum = rssi_sum + excluded.rssi_sum, rssi_n = rssi_n + excluded.rssi_n,
  ` + nullMin("min_alt") + `,
  ` + nullMax("max_alt") + `,
  last_t = MAX(last_t, excluded.last_t)`

// Weather rows are merged column by column: a value that is present
// replaces the stored one, a missing value (NULL) keeps it. This lets a
// "current conditions" fetch and a "pressure level profile" fetch fill the
// same hour without erasing each other.
var sqlUpsertWeatherObs = func() string {
	cols := append(append([]string{"t"}, weatherCols...), "profile_json")
	var set []string
	for _, c := range cols[1:] {
		set = append(set, c+" = COALESCE(excluded."+c+", "+c+")")
	}
	return "INSERT INTO weather_obs(" + strings.Join(cols, ", ") + ")\nVALUES(" + placeholders(len(cols)) +
		")\nON CONFLICT(t) DO UPDATE SET\n  " + strings.Join(set, ",\n  ")
}()

// A forecast row is replaced as a whole by a newer issue.
var sqlUpsertWeatherFc = func() string {
	cols := append(append([]string{"t", "issued"}, weatherCols...), "profile_json")
	return "INSERT OR REPLACE INTO weather_fc(" + strings.Join(cols, ", ") + ")\nVALUES(" + placeholders(len(cols)) + ")"
}()

var sqlInsertWeatherFcLog = func() string {
	cols := append([]string{"t", "lead_h", "issued"}, weatherCols...)
	return "INSERT OR REPLACE INTO weather_fc_log(" + strings.Join(cols, ", ") + ")\nVALUES(" + placeholders(len(cols)) + ")"
}()

const sqlInsertNTP = `INSERT OR REPLACE INTO ntp_samples(t, server, offset_ms, delay_ms, stratum) VALUES(?,?,?,?,?)`

const sqlMergeMetProfile = `INSERT INTO met_profile(hour, level, u_sum, v_sum, t_sum, n_wind, n_temp) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(hour, level) DO UPDATE SET
  u_sum = u_sum + excluded.u_sum, v_sum = v_sum + excluded.v_sum, t_sum = t_sum + excluded.t_sum,
  n_wind = n_wind + excluded.n_wind, n_temp = n_temp + excluded.n_temp`

const sqlSetRecordBase = `INSERT INTO records(key, value, icao, t, detail_json) VALUES(?,?,?,?,?)
ON CONFLICT(key) DO UPDATE SET
  value = excluded.value, icao = excluded.icao, t = excluded.t, detail_json = excluded.detail_json`

const (
	sqlSetRecordAlways = sqlSetRecordBase
	sqlSetRecordMax    = sqlSetRecordBase + "\nWHERE excluded.value > records.value"
	sqlSetRecordMin    = sqlSetRecordBase + "\nWHERE excluded.value < records.value"
)

const sqlInsertEvent = `INSERT INTO events(t, kind, icao, detail_json) VALUES(?,?,?,?)`

const sqlInsertReceiverStatus = `INSERT OR REPLACE INTO receiver_status(t, feed, json) VALUES(?,?,?)`

const sqlSetKV = `INSERT INTO kv(key, value, updated) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated = excluded.updated`

const sqlDeleteKV = `DELETE FROM kv WHERE key = ?`

const sqlInsertMetObs = `INSERT INTO met_obs(t, icao, lat, lon, alt, wind_dir, wind_kt, oat_c, src) VALUES(?,?,?,?,?,?,?,?,?)`

const sqlInsertACDB = `INSERT OR REPLACE INTO acdb(icao, reg, type, "desc", operator, flags, year) VALUES(?,?,?,?,?,?,?)`

// preparedSQL lists every statement the writer keeps prepared on its
// connection for the lifetime of the database handle.
var preparedSQL = []string{
	sqlUpsertAircraft, sqlUpsertFlight, sqlRefreshFlight, sqlRefreshAircraft, sqlInsertTail, sqlMergeStatsMinute, sqlLowerRollupMark,
	sqlGetRangeHour, sqlPutRangeHour, sqlMergeCoverage, sqlMergeSignalHist, sqlMergeElevHist, sqlMergeHeat,
	sqlUpsertWeatherObs, sqlUpsertWeatherFc, sqlInsertWeatherFcLog, sqlInsertNTP,
	sqlMergeMetProfile, sqlSetRecordAlways, sqlSetRecordMax, sqlSetRecordMin, sqlInsertEvent,
	sqlInsertReceiverStatus, sqlSetKV, sqlDeleteKV, sqlInsertACDB,
}

// ------------------------------------------------------------ value helpers

func isNum(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// nf maps a float to a SQL value: NaN/Inf become NULL.
func nf(v float64) any {
	if !isNum(v) {
		return nil
	}
	return v
}

// ni maps a float to a rounded SQL integer: NaN/Inf become NULL. Absurdly
// large values are clamped (converting them to int64 would be undefined).
func ni(v float64) any {
	if !isNum(v) {
		return nil
	}
	const lim = 1e15
	return int64(math.Round(math.Max(-lim, math.Min(lim, v))))
}

// nu maps an ICAO address to SQL: 0 becomes NULL.
func nu(v uint32) any {
	if v == 0 {
		return nil
	}
	return int64(v)
}

// ns maps a string to SQL: "" becomes NULL.
func ns(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// zf maps a float to itself with NaN/Inf as 0 (for NOT NULL sums / maxima).
func zf(v float64) float64 {
	if !isNum(v) {
		return 0
	}
	return v
}

func nanMax(a, b float64) float64 {
	switch {
	case !isNum(a):
		return b
	case !isNum(b):
		return a
	}
	return math.Max(a, b)
}

// dayNumber accepts a Unix day number, or (defensively) Unix seconds.
func dayNumber(day int) int {
	if day > 10_000_000 { // far beyond any day number: the caller passed seconds
		return DayOf(int64(day))
	}
	return day
}

// hourStart accepts Unix seconds, or (defensively) a Unix hour number.
func hourStart(t int64) int64 {
	if t > 0 && t < 10_000_000 { // before April 1970 as seconds: an hour number
		return t * 3600
	}
	return floorTo(t, 3600)
}

// ------------------------------------------------------------------- Batch

// Batch collects typed row operations for many tables. Fill it from the
// tracker's in-memory accumulators and hand it to Store.Submit (queued,
// flushed with the next batch transaction) or Store.Apply (written
// synchronously in one transaction). A Batch is not safe for concurrent use;
// after Submit/Apply it is empty and can be reused.
//
// Every method copies the rows it is given, so the caller may reuse its
// slices immediately. The Store has a same-named shortcut for each method
// that queues a single operation.
type Batch struct {
	ops  []op
	rows int
}

// NewBatch returns an empty batch.
func NewBatch() *Batch { return &Batch{} }

// Len returns the number of rows collected so far.
func (b *Batch) Len() int { return b.rows }

// Reset discards the collected rows.
func (b *Batch) Reset() { b.ops, b.rows = nil, 0 }

func (b *Batch) add(name string, t tier, rows int, run func(tc *txc) error) {
	if rows <= 0 {
		return
	}
	b.ops = append(b.ops, op{name: name, tier: t, rows: rows, run: run})
	b.rows += rows
}

// UpsertAircraft merges sightings into the aircraft table (see AircraftSeen
// for the merge rules). Rows with ICAO 0 are ignored.
func (b *Batch) UpsertAircraft(rows []AircraftSeen) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b AircraftSeen) int { return cmp.Compare(a.ICAO, b.ICAO) })
	b.add("aircraft", tierHigh, len(rows), func(tc *txc) error {
		ref, err := tc.stmt(sqlRefreshAircraft)
		if err != nil {
			return err
		}
		var st *sql.Stmt
		for i := range rows {
			r := &rows[i]
			if r.ICAO == 0 {
				continue
			}
			first := r.FirstSeen
			if first <= 0 {
				first = r.LastSeen
			}
			args := []any{int64(r.ICAO), first, r.LastSeen,
				r.Callsign, r.Reg, r.Type, r.Operator, r.Country, r.Category,
				r.Msgs, r.Flights, r.Positions, zf(r.MaxDistKm), ni(r.MaxAlt), r.SrcMask, r.Flags}
			res, err := ref.ExecContext(tc.ctx, args...)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				continue
			}
			if st == nil {
				if st, err = tc.stmt(sqlUpsertAircraft); err != nil {
					return err
				}
			}
			if _, err := st.ExecContext(tc.ctx, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpsertFlights inserts or completely replaces flight rows by ID. This one
// operation opens a flight (first upsert of a new ID from NextFlightID),
// updates it while it is live and closes it (the final upsert). Rows with
// ID <= 0 are ignored.
func (b *Batch) UpsertFlights(rows []Flight) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b Flight) int { return cmp.Compare(a.ID, b.ID) })
	b.add("flights", tierHigh, len(rows), func(tc *txc) error {
		ref, err := tc.stmt(sqlRefreshFlight)
		if err != nil {
			return err
		}
		var st *sql.Stmt
		for i := range rows {
			f := &rows[i]
			if f.ID <= 0 {
				continue
			}
			end := f.End
			if end < f.Start {
				end = f.Start
			}
			// Columns 2..21 of sqlRefreshFlight; sqlUpsertFlight takes the
			// same values after (id, icao, callsign, squawk, start).
			vals := []any{f.Callsign, f.Squawk, end, f.Msgs, f.Positions, f.SrcMask,
				nf(f.MaxDistKm), nf(f.MaxDistBrg), nf(f.MinDistKm), ni(f.MinAlt), ni(f.MaxAlt), nf(f.MaxGS),
				nf(f.RSSIMin), nf(f.RSSIMax), nf(f.RSSIAvg),
				nf(f.FirstLat), nf(f.FirstLon), nf(f.LastLat), nf(f.LastLon), f.Emergency}
			args := make([]any, 0, 23)
			args = append(append(append(args, f.ID), vals...), int64(f.ICAO), f.Start)
			res, err := ref.ExecContext(tc.ctx, args...)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				continue
			}
			if st == nil {
				if st, err = tc.stmt(sqlUpsertFlight); err != nil {
					return err
				}
			}
			args = append(append(args[:0], f.ID, int64(f.ICAO), f.Callsign, f.Squawk, f.Start), vals[2:]...)
			if _, err := st.ExecContext(tc.ctx, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddPositions appends trail points. A point with the same (FlightID, T) as
// a stored one replaces it; points without a valid lat/lon or FlightID are
// ignored. The points go to the append-only trail tail (trail_store.go).
func (b *Batch) AddPositions(rows []Position) {
	trs := make([]TrailRow, 0, len(rows))
	for i := range rows {
		if r, ok := trailRowFromPosition(&rows[i]); ok {
			trs = append(trs, r)
		}
	}
	slices.SortStableFunc(trs, cmpTrailKey)
	b.add("positions", tierMid, len(rows), func(tc *txc) error {
		if len(trs) == 0 {
			return nil
		}
		return appendTrailTail(tc, trs)
	})
}

// AddMinuteStats merges per-minute statistics rows (see StatsRow for the
// merge rules). T is rounded down to the minute.
func (b *Batch) AddMinuteStats(rows []StatsRow) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b StatsRow) int {
		return cmp.Or(cmp.Compare(floorTo(a.T, 60), floorTo(b.T, 60)), cmp.Compare(a.Ant, b.Ant))
	})
	b.add("stats_minute", tierHigh, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlMergeStatsMinute)
		if err != nil {
			return err
		}
		minT := int64(math.MaxInt64)
		for i := range rows {
			r := &rows[i]
			t := floorTo(r.T, 60)
			if t < minT {
				minT = t
			}
			if _, err := st.ExecContext(tc.ctx, t, r.Ant,
				r.Frames, r.DF0, r.DF4, r.DF5, r.DF11, r.DF16, r.DF17, r.DF18, r.DF20, r.DF21,
				r.DFOther, r.CRCBad, r.Positions, r.MLATPositions,
				r.Aircraft, r.AircraftPos, r.ADSB, r.MLAT, r.ModeS,
				nf(r.MaxKm), nf(r.RSSIAvg), nf(r.RSSIMin), nf(r.RSSIMax), secsArg(r.Secs)); err != nil {
				return err
			}
		}
		// A minute inside an hour that was already rolled up: roll it up again.
		return tc.exec(sqlLowerRollupMark, floorTo(minT, 3600), time.Now().Unix())
	})
}

// secsArg is the secs column value of a minute row: NULL when unknown,
// else at most a minute.
func secsArg(v float64) any {
	if !(v > 0) {
		return nil
	}
	return min(v, 60)
}

// AddRangeHours merges hourly range metrics (see RangeHour for the merge
// rules). T is rounded down to the hour.
func (b *Batch) AddRangeHours(rows []RangeHour) {
	rows = slices.Clone(rows)
	for i := range rows { // deep copy: the caller may reuse its buffers
		rows[i].DistHist = slices.Clone(rows[i].DistHist)
		rows[i].SectorMax = slices.Clone(rows[i].SectorMax)
	}
	slices.SortStableFunc(rows, func(a, b RangeHour) int {
		return cmp.Or(cmp.Compare(floorTo(a.T, 3600), floorTo(b.T, 3600)), cmp.Compare(a.Ant, b.Ant))
	})
	b.add("range_hour", tierHigh, len(rows), func(tc *txc) error {
		get, err := tc.stmt(sqlGetRangeHour)
		if err != nil {
			return err
		}
		put, err := tc.stmt(sqlPutRangeHour)
		if err != nil {
			return err
		}
		// Compact histogram on both kinds, exact sector maxima on airview.db
		// (compact.go; the decoder reads every encoding).
		encHist, encSect := rangeHourEncoders(tc.h.kind)
		for i := range rows {
			r := rows[i]
			r.T = floorTo(r.T, 3600)
			old, found, err := scanRangeHour(get.QueryRowContext(tc.ctx, r.T, r.Ant))
			if err != nil {
				return err
			}
			if found {
				r = mergeRangeHour(old, r)
			}
			if _, err := put.ExecContext(tc.ctx, r.T, r.Ant,
				nf(r.MaxKm), nf(r.P99Km), nf(r.P95Km), nf(r.P90Km), nf(r.P50Km),
				r.Positions, r.Aircraft, r.FarCount, nf(r.ResidDB), r.ResidN,
				encHist(r.DistHist), encSect(r.SectorMax)); err != nil {
				return err
			}
		}
		return nil
	})
}

// scanRangeHour scans the columns of sqlGetRangeHour (everything but the
// key). found is false when sc is a *sql.Row without a row.
func scanRangeHour(sc interface{ Scan(...any) error }, key ...any) (r RangeHour, found bool, err error) {
	var hist, sect []byte
	dest := append(key, (*nfloat)(&r.MaxKm), (*nfloat)(&r.P99Km), (*nfloat)(&r.P95Km), (*nfloat)(&r.P90Km),
		(*nfloat)(&r.P50Km), &r.Positions, &r.Aircraft, &r.FarCount, (*nfloat)(&r.ResidDB), &r.ResidN, &hist, &sect)
	err = sc.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.DistHist = DecodeDistHist(hist)
	r.SectorMax = DecodeSectorMax(sect)
	return r, true, nil
}

// mergeRangeHour combines the stored row old with the increment in.
func mergeRangeHour(old, in RangeHour) RangeHour {
	out := in
	out.MaxKm = nanMax(old.MaxKm, in.MaxKm)
	out.Positions = old.Positions + in.Positions
	out.Aircraft = max(old.Aircraft, in.Aircraft)
	out.FarCount = old.FarCount + in.FarCount

	// Residual: weighted mean; a known value without a count weighs 1.
	wOld, wIn := float64(old.ResidN), float64(in.ResidN)
	if isNum(old.ResidDB) && wOld <= 0 {
		wOld = 1
	}
	if isNum(in.ResidDB) && wIn <= 0 {
		wIn = 1
	}
	switch {
	case isNum(old.ResidDB) && isNum(in.ResidDB):
		out.ResidDB = (old.ResidDB*wOld + in.ResidDB*wIn) / (wOld + wIn)
	case isNum(old.ResidDB):
		out.ResidDB = old.ResidDB
	}
	out.ResidN = old.ResidN + in.ResidN

	// Histogram: add bin by bin.
	switch {
	case len(old.DistHist) == 0:
	case len(in.DistHist) == 0:
		out.DistHist = old.DistHist
	default:
		n := max(len(old.DistHist), len(in.DistHist))
		h := make([]uint32, n)
		for i := range h {
			var a, b uint64
			if i < len(old.DistHist) {
				a = uint64(old.DistHist[i])
			}
			if i < len(in.DistHist) {
				b = uint64(in.DistHist[i])
			}
			h[i] = uint32(min(a+b, math.MaxUint32))
		}
		out.DistHist = h
	}

	// Sector maxima: max per sector.
	switch {
	case len(old.SectorMax) == 0:
	case len(in.SectorMax) == 0:
		out.SectorMax = old.SectorMax
	default:
		n := max(len(old.SectorMax), len(in.SectorMax))
		sm := make([]float32, n)
		for i := range sm {
			var a, b float32
			if i < len(old.SectorMax) {
				a = old.SectorMax[i]
			}
			if i < len(in.SectorMax) {
				b = in.SectorMax[i]
			}
			sm[i] = max(a, b)
		}
		out.SectorMax = sm
	}

	// Percentiles cannot be merged; recompute them from the merged
	// histogram when both parts contributed one, else keep what is known.
	if len(old.DistHist) > 0 && len(in.DistHist) > 0 {
		pct := func(q float64) float64 {
			v := HistPercentile(out.DistHist, DistHistBinKm, q)
			if isNum(out.MaxKm) && v > out.MaxKm {
				v = out.MaxKm
			}
			return v
		}
		out.P99Km, out.P95Km, out.P90Km, out.P50Km = pct(0.99), pct(0.95), pct(0.90), pct(0.50)
	} else {
		keep := func(in, old float64) float64 {
			if isNum(in) {
				return in
			}
			return old
		}
		out.P99Km, out.P95Km = keep(in.P99Km, old.P99Km), keep(in.P95Km, old.P95Km)
		out.P90Km, out.P50Km = keep(in.P90Km, old.P90Km), keep(in.P50Km, old.P50Km)
	}
	return out
}

// MergeCoverage adds polar-coverage increments.
func (b *Batch) MergeCoverage(rows []CoverageInc) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b CoverageInc) int {
		return cmp.Or(cmp.Compare(dayNumber(a.Day), dayNumber(b.Day)), cmp.Compare(a.Ant, b.Ant),
			cmp.Compare(a.BrgBin, b.BrgBin), cmp.Compare(a.AltBand, b.AltBand))
	})
	b.add("coverage", tierLow, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlMergeCoverage)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if _, err := st.ExecContext(tc.ctx, dayNumber(r.Day), r.Ant, r.BrgBin, r.AltBand,
				zf(r.MaxKm), r.Count, zf(r.RSSISum), r.RSSIN); err != nil {
				return err
			}
		}
		return nil
	})
}

// MergeSignalHist adds RSSI-vs-distance histogram increments.
func (b *Batch) MergeSignalHist(rows []SignalHistInc) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b SignalHistInc) int {
		return cmp.Or(cmp.Compare(dayNumber(a.Day), dayNumber(b.Day)), cmp.Compare(a.Ant, b.Ant),
			cmp.Compare(a.DistBin, b.DistBin), cmp.Compare(a.RSSIBin, b.RSSIBin))
	})
	b.add("signal_hist", tierLow, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlMergeSignalHist)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if _, err := st.ExecContext(tc.ctx, dayNumber(r.Day), r.Ant, r.DistBin, r.RSSIBin, r.Count); err != nil {
				return err
			}
		}
		return nil
	})
}

// MergeElevHist adds elevation-pattern increments.
func (b *Batch) MergeElevHist(rows []ElevHistInc) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b ElevHistInc) int {
		return cmp.Or(cmp.Compare(dayNumber(a.Day), dayNumber(b.Day)), cmp.Compare(a.Ant, b.Ant),
			cmp.Compare(a.ElevBin, b.ElevBin))
	})
	b.add("elev_hist", tierLow, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlMergeElevHist)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if _, err := st.ExecContext(tc.ctx, dayNumber(r.Day), r.Ant, r.ElevBin, r.Count,
				zf(r.RSSISum), zf(r.ResidSum)); err != nil {
				return err
			}
		}
		return nil
	})
}

// MergeHeat adds position-grid increments.
func (b *Batch) MergeHeat(rows []HeatInc) {
	rows = slices.Clone(rows)
	b.add("heat", tierLow, len(rows), func(tc *txc) error {
		if tc.h.kind == KindStations { // key (ant, cell)
			slices.SortStableFunc(rows, func(a, b HeatInc) int { return cmp.Or(cmp.Compare(a.Ant, b.Ant), cmp.Compare(a.Cell, b.Cell)) })
		} else {
			slices.SortStableFunc(rows, cmpHeatKey)
		}
		st, err := tc.stmt(heatMergeSQL(tc.h.kind))
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if _, err := st.ExecContext(tc.ctx, r.Cell, r.Ant, r.Count, zf(r.RSSISum), r.RSSIN,
				ni(r.MinAlt), ni(r.MaxAlt), r.LastT); err != nil {
				return err
			}
		}
		return nil
	})
}

// cmpHeatKey orders heat increments for sequential B-tree access: by
// antenna, then by 16x16-cell tile (HeatTile, DB-EFFICIENCY-DESIGN §3.6),
// then cell - the key order of the tiled airview.db table and, within an
// antenna, close to the (ant, cell) order of stations.db.
func cmpHeatKey(a, b HeatInc) int {
	return cmp.Or(cmp.Compare(a.Ant, b.Ant), cmp.Compare(HeatTile(a.Cell), HeatTile(b.Cell)), cmp.Compare(a.Cell, b.Cell))
}

func weatherArgs(w *Weather) []any {
	return []any{
		nf(w.TempC), nf(w.DewpointC), nf(w.RH), nf(w.PressureMSL), nf(w.SurfacePressure),
		nf(w.WindKmh), nf(w.WindDir), nf(w.GustKmh), nf(w.Cloud), nf(w.CloudLow), nf(w.CloudMid), nf(w.CloudHigh),
		nf(w.PrecipMM), nf(w.RainMM), nf(w.SnowCM), nf(w.VisibilityM), nf(w.CAPE),
		ni(w.WeatherCode), ni(w.IsDay), nf(w.NSurface), nf(w.DNDH), nf(w.KFactor), nf(w.Duct),
	}
}

// AddWeatherObs stores hourly observation / analysis rows. A row for an
// hour that already exists is merged column by column: present values
// replace, missing (NaN / "") values keep what is stored.
func (b *Batch) AddWeatherObs(rows []Weather) {
	rows = slices.Clone(rows)
	b.add("weather_obs", tierHigh, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlUpsertWeatherObs)
		if err != nil {
			return err
		}
		for i := range rows {
			w := &rows[i]
			args := append([]any{w.T}, weatherArgs(w)...)
			args = append(args, ns(w.ProfileJSON))
			if _, err := st.ExecContext(tc.ctx, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetWeatherForecast stores the latest forecast per target hour (weather_fc);
// a row replaces the previous forecast for the same hour. Issued 0 is
// stored as "now".
func (b *Batch) SetWeatherForecast(rows []Weather) {
	rows = slices.Clone(rows)
	b.add("weather_fc", tierHigh, len(rows), func(tc *txc) error {
		if err := finaliseForecastIssue(tc.ctx, tc.tx); err != nil {
			return err
		}
		st, err := tc.stmt(sqlUpsertWeatherFc)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for i := range rows {
			w := &rows[i]
			issued := w.Issued
			if issued <= 0 {
				issued = now
			}
			args := append([]any{w.T, issued}, weatherArgs(w)...)
			args = append(args, ns(w.ProfileJSON))
			if _, err := st.ExecContext(tc.ctx, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddWeatherForecastLog records what was forecast for hour T at lead time
// lead_h (for backtesting). The lead is round((T-Issued)/3600) when Issued
// is set, else the row's LeadH; rows with a negative lead are ignored. One
// row is kept per (T, lead_h): a later issue with the same lead replaces it.
func (b *Batch) AddWeatherForecastLog(rows []Weather) {
	rows = slices.Clone(rows)
	b.add("weather_fc_log", tierHigh, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlInsertWeatherFcLog)
		if err != nil {
			return err
		}
		for i := range rows {
			w := &rows[i]
			lead := int64(w.LeadH)
			issued := w.Issued
			if issued > 0 {
				lead = int64(math.Round(float64(w.T-issued) / 3600))
			} else {
				issued = w.T - lead*3600
			}
			if lead < 0 {
				continue
			}
			args := append([]any{w.T, lead, issued}, weatherArgs(w)...)
			if _, err := st.ExecContext(tc.ctx, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddNTPSamples stores NTP measurements (one per second at most).
func (b *Batch) AddNTPSamples(rows []NTPSample) {
	rows = slices.Clone(rows)
	b.add("ntp_samples", tierMid, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlInsertNTP)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if _, err := st.ExecContext(tc.ctx, r.T, r.Server, nf(r.OffsetMs), nf(r.DelayMs), r.Stratum); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddMetObs appends aircraft-derived meteorological observations. Since
// migration met 2 they are stored in the compact met_sample table
// (AddMetSamples); the logical met_obs table (exports, readers) shows them
// in the 1.0.0 columns.
func (b *Batch) AddMetObs(rows []MetObs) { b.AddMetSamples(rows) }

// MergeMetProfile adds vertical-profile increments.
func (b *Batch) MergeMetProfile(rows []MetProfileInc) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b MetProfileInc) int {
		return cmp.Or(cmp.Compare(hourStart(a.Hour), hourStart(b.Hour)), cmp.Compare(a.Level, b.Level))
	})
	b.add("met_profile", tierLow, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlMergeMetProfile)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if _, err := st.ExecContext(tc.ctx, hourStart(r.Hour), r.Level,
				zf(r.USum), zf(r.VSum), zf(r.TSum), r.NWind, r.NTemp); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetRecord stores an all-time record if it beats the stored one according
// to mode (RecordMax: larger wins, RecordMin: smaller wins, RecordAlways).
// A record that does not exist yet is always stored. Records with an empty
// key or a NaN value are ignored.
func (b *Batch) SetRecord(rec Record, mode RecordMode) {
	if rec.Key == "" || !isNum(rec.Value) {
		return
	}
	query := sqlSetRecordAlways
	switch mode {
	case RecordMax:
		query = sqlSetRecordMax
	case RecordMin:
		query = sqlSetRecordMin
	}
	b.add("records", tierHigh, 1, func(tc *txc) error {
		t := rec.T
		if t <= 0 {
			t = time.Now().Unix()
		}
		return tc.exec(query, rec.Key, rec.Value, nu(rec.ICAO), t, ns(rec.Detail))
	})
}

// AddEvents appends events. T 0 is stored as "now".
func (b *Batch) AddEvents(rows []Event) {
	rows = slices.Clone(rows)
	b.add("events", tierHigh, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlInsertEvent)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for i := range rows {
			e := &rows[i]
			t := e.T
			if t <= 0 {
				t = now
			}
			if _, err := st.ExecContext(tc.ctx, t, e.Kind, nu(e.ICAO), ns(e.Detail)); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddReceiverStatus stores receiver status frames.
func (b *Batch) AddReceiverStatus(rows []ReceiverStatus) {
	rows = slices.Clone(rows)
	b.add("receiver_status", tierMid, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlInsertReceiverStatus)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if _, err := st.ExecContext(tc.ctx, r.T, r.Feed, r.JSON); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetKV stores a key/value pair in the kv table.
func (b *Batch) SetKV(key, value string) {
	if key == "" {
		return
	}
	b.add("kv", tierHigh, 1, func(tc *txc) error {
		return tc.exec(sqlSetKV, key, value, time.Now().Unix())
	})
}

// AddACDB inserts or replaces aircraft-database entries. For a full import
// of hundreds of thousands of rows use Store.ImportACDB instead.
func (b *Batch) AddACDB(rows []ACDBEntry) {
	rows = slices.Clone(rows)
	b.add("acdb", tierHigh, len(rows), func(tc *txc) error {
		return insertACDB(tc, rows)
	})
}

func insertACDB(tc *txc, rows []ACDBEntry) error {
	st, err := tc.stmt(sqlInsertACDB)
	if err != nil {
		return err
	}
	for i := range rows {
		e := &rows[i]
		if e.ICAO == 0 {
			continue
		}
		if _, err := st.ExecContext(tc.ctx, int64(e.ICAO), e.Reg, e.Type, e.Desc, e.Operator, e.Flags, e.Year); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------- Store shortcuts (asynchronous)

// queue1 queues the single operation that fill adds to a fresh batch.
func (s *Store) queue1(fill func(b *Batch)) {
	var b Batch
	fill(&b)
	s.Submit(&b)
}

// UpsertAircraft queues Batch.UpsertAircraft.
func (s *Store) UpsertAircraft(rows []AircraftSeen) {
	s.queue1(func(b *Batch) { b.UpsertAircraft(rows) })
}

// UpsertFlights queues Batch.UpsertFlights.
func (s *Store) UpsertFlights(rows []Flight) { s.queue1(func(b *Batch) { b.UpsertFlights(rows) }) }

// AddPositions queues Batch.AddPositions.
func (s *Store) AddPositions(rows []Position) { s.queue1(func(b *Batch) { b.AddPositions(rows) }) }

// AddMinuteStats queues Batch.AddMinuteStats.
func (s *Store) AddMinuteStats(rows []StatsRow) { s.queue1(func(b *Batch) { b.AddMinuteStats(rows) }) }

// AddRangeHours queues Batch.AddRangeHours.
func (s *Store) AddRangeHours(rows []RangeHour) { s.queue1(func(b *Batch) { b.AddRangeHours(rows) }) }

// MergeCoverage queues Batch.MergeCoverage.
func (s *Store) MergeCoverage(rows []CoverageInc) { s.queue1(func(b *Batch) { b.MergeCoverage(rows) }) }

// MergeSignalHist queues Batch.MergeSignalHist.
func (s *Store) MergeSignalHist(rows []SignalHistInc) {
	s.queue1(func(b *Batch) { b.MergeSignalHist(rows) })
}

// MergeElevHist queues Batch.MergeElevHist.
func (s *Store) MergeElevHist(rows []ElevHistInc) { s.queue1(func(b *Batch) { b.MergeElevHist(rows) }) }

// MergeHeat queues Batch.MergeHeat.
func (s *Store) MergeHeat(rows []HeatInc) { s.queue1(func(b *Batch) { b.MergeHeat(rows) }) }

// AddWeatherObs queues Batch.AddWeatherObs.
func (s *Store) AddWeatherObs(rows []Weather) { s.queue1(func(b *Batch) { b.AddWeatherObs(rows) }) }

// SetWeatherForecast queues Batch.SetWeatherForecast.
func (s *Store) SetWeatherForecast(rows []Weather) {
	s.queue1(func(b *Batch) { b.SetWeatherForecast(rows) })
}

// AddWeatherForecastLog queues Batch.AddWeatherForecastLog.
func (s *Store) AddWeatherForecastLog(rows []Weather) {
	s.queue1(func(b *Batch) { b.AddWeatherForecastLog(rows) })
}

// AddNTPSamples queues Batch.AddNTPSamples.
func (s *Store) AddNTPSamples(rows []NTPSample) { s.queue1(func(b *Batch) { b.AddNTPSamples(rows) }) }

// AddMetObs queues Batch.AddMetObs.
func (s *Store) AddMetObs(rows []MetObs) { s.queue1(func(b *Batch) { b.AddMetObs(rows) }) }

// MergeMetProfile queues Batch.MergeMetProfile.
func (s *Store) MergeMetProfile(rows []MetProfileInc) {
	s.queue1(func(b *Batch) { b.MergeMetProfile(rows) })
}

// SetRecord queues Batch.SetRecord.
func (s *Store) SetRecord(rec Record, mode RecordMode) {
	s.queue1(func(b *Batch) { b.SetRecord(rec, mode) })
}

// AddEvents queues Batch.AddEvents.
func (s *Store) AddEvents(rows []Event) { s.queue1(func(b *Batch) { b.AddEvents(rows) }) }

// AddReceiverStatus queues Batch.AddReceiverStatus.
func (s *Store) AddReceiverStatus(rows []ReceiverStatus) {
	s.queue1(func(b *Batch) { b.AddReceiverStatus(rows) })
}

// SetKVAsync queues Batch.SetKV (use KVSet for a synchronous write).
func (s *Store) SetKVAsync(key, value string) { s.queue1(func(b *Batch) { b.SetKV(key, value) }) }
