package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
)

// Tiered retention, downsampling and maintenance for large databases
// (DESIGN §20.2: 1,000+ feeders on one aggregator). Storage stays bounded
// by moving data through coarser tiers instead of keeping everything:
//
//	tier      tables                                         kept (TierPolicy)
//	minute    stats_minute, receiver_status, station_minute,  MinuteDays
//	          station_clock_minute, mlat_minute, latency_minute
//	hour      stats_hour, range_hour, mlat_quality_hour        HourDays
//	day       stats_day (rollup of stats_hour)                 DayDays (0 = forever)
//	histogram coverage, signal_hist, elev_hist: per day for    HistDailyDays, then one
//	          the recent past, then merged into one row per    bucket per month until
//	          calendar month (day = first day of the month)    HistDays (0 = forever)
//	grid      heat: cells not updated for HeatIdleDays         HeatIdleDays (0 = forever)
//
// Every step runs in small transactions (the writer is never held for long),
// is idempotent and resumes where it stopped, so Maintain can be called as
// often as convenient (hourly is typical) and cancelled at any time.
//
// Month buckets: an older per-day histogram row is added into the row of
// the first day of its UTC month and deleted. Readers that sum a day range
// see the same totals as long as the range covers whole months; a range
// that starts inside a bucketed month counts that month entirely or not at
// all (depending on whether the range contains its first day). Maxima
// (coverage.max_km) are kept exactly per bucket.

// Keys of the kv table used by the tiered maintenance.
const (
	// kvRollupDay: start (Unix s) of the oldest UTC day that RollupDays
	// still recomputes; earlier days are final.
	kvRollupDay = "store.rollup_day"
	// kvHoursPruned: stats_hour rows before this time were deleted, so the
	// stats_day rows before it are frozen.
	kvHoursPruned = "store.hours_pruned"
	// kvHistBucketed: day number below which the per-day histograms have
	// been merged into month buckets.
	kvHistBucketed = "store.hist_bucketed"
	// kvLastOptimize: Unix s of the last PRAGMA optimize run by Maintain.
	kvLastOptimize = "store.last_optimize"
	// kvFSPruned: flight_station rows of every flight that ended before
	// this time are deleted.
	kvFSPruned = "store.flight_station_pruned"
	// kvHeatPruned: Unix s of the last completed heat walk.
	kvHeatPruned = "store.heat_pruned"
)

// TierPolicy configures Maintain. A field <= 0 disables that step ("keep
// forever"), except where noted. DefaultTierPolicy returns the aggregator
// defaults of DESIGN §8.5 / §20.2.
type TierPolicy struct {
	// MinuteDays: minute tables (stats_minute only after its rows were
	// rolled up into stats_hour).
	MinuteDays int
	// HourDays: hourly tables (stats_hour only after its days were rolled
	// up into stats_day).
	HourDays int
	// DayDays: stats_day (0 = forever, the default).
	DayDays int
	// HistDailyDays: per-day histogram rows younger than this stay per day;
	// older ones are merged into month buckets.
	HistDailyDays int
	// HistDays: histogram rows (day or month bucket) older than this are
	// deleted.
	HistDays int
	// HeatIdleDays: heat cells whose last update (last_t) is older are
	// deleted.
	HeatIdleDays int
	// FlightStationDays: flight_station rows (airview.db) of flights that
	// ended longer ago are deleted, independently of the flights retention
	// (one row per station and flight is ~115 bytes: with 1,000 feeders the
	// table grows by hundreds of MB per day).
	FlightStationDays int
	// TrailFullDays: stored position trails (airview.db) of flights that
	// ended longer ago are thinned to one point per TrailThinSec (see
	// downsample.go); 0 = keep full resolution until the trail retention
	// deletes them. TrailThinSec <= 1 disables thinning too.
	TrailFullDays int
	TrailThinSec  int

	// FreePct: when more than this percentage of the file is free pages, up
	// to FreeStepPages × 8 pages are released per run (incremental vacuum;
	// default 10 %). Normally freed pages are simply reused.
	FreePct int
	// WALCheckpointBytes: a WAL larger than this is checkpointed with
	// TRUNCATE (default 64 MiB).
	WALCheckpointBytes int64
	// OptimizeEvery: interval of PRAGMA optimize (ANALYZE where the query
	// planner's statistics are stale, bounded by analysis_limit=400;
	// default 1 h).
	OptimizeEvery time.Duration
}

// DefaultTierPolicy returns the aggregator defaults: minute data 30 days,
// hourly data 395 days (13 months), daily totals forever, histograms per
// day for 35 days then per month for 395 days, heat cells idle for 395
// days removed, flight_station rows kept 90 days, position trails at full
// resolution for 2 days and one point per 30 s after that.
func DefaultTierPolicy() TierPolicy {
	return TierPolicy{
		MinuteDays: 30, HourDays: 395, DayDays: 0,
		HistDailyDays: 35, HistDays: 395, HeatIdleDays: 395, FlightStationDays: 90,
		TrailFullDays: DefaultTrailFullDays, TrailThinSec: DefaultTrailThinSec,
		FreePct: 10, WALCheckpointBytes: 64 << 20, OptimizeEvery: time.Hour,
	}
}

// TierPolicyFromConfig maps the aggregator's storage block
// (aggregator.storage, with minuteDays / hourDays falling back to
// aggregator.retention) onto a TierPolicy.
func TierPolicyFromConfig(a model.Aggregator) TierPolicy {
	s := a.Storage
	p := TierPolicy{
		MinuteDays: s.MinuteDays, HourDays: s.HourDays, DayDays: s.DayDays,
		HistDailyDays: s.HistDailyDays, HistDays: s.HistDays, HeatIdleDays: s.HeatIdleDays,
		FlightStationDays: s.FlightStationDays, FreePct: s.FreePct,
		WALCheckpointBytes: int64(s.WALCheckpointMB) << 20,
		OptimizeEvery:      time.Duration(s.OptimizeHours) * time.Hour,
	}
	if p.MinuteDays <= 0 {
		p.MinuteDays = a.Retention.MinuteDays
	}
	if p.HourDays <= 0 {
		p.HourDays = 30 * a.Retention.HourMonths
	}
	// Trail tiers: 0 in either field disables thinning (Maintain skips it).
	p.TrailFullDays, p.TrailThinSec = s.TrailFullDays, s.TrailThinSec
	return p
}

func (p TierPolicy) withDefaults() TierPolicy {
	if p.FreePct <= 0 {
		p.FreePct = 10
	}
	if p.WALCheckpointBytes <= 0 {
		p.WALCheckpointBytes = 64 << 20
	}
	if p.OptimizeEvery <= 0 {
		p.OptimizeEvery = time.Hour // cheap: analysis_limit=400 (design §3.16)
	}
	return p
}

// MaintReport counts what one Maintain call did.
type MaintReport struct {
	HourRows     int              `json:"hourRows"`     // stats_hour rows (re)computed
	DayRows      int              `json:"dayRows"`      // stats_day rows (re)computed
	Deleted      map[string]int64 `json:"deleted"`      // rows deleted per table
	Bucketed     map[string]int64 `json:"bucketed"`     // per-day rows merged into month buckets
	FreedPages   int64            `json:"freedPages"`   // pages returned to the file system
	Checkpointed bool             `json:"checkpointed"` // WAL truncated
	Optimized    bool             `json:"optimized"`    // PRAGMA optimize ran
	// ThinnedPositions: trail points removed by the trail tier (ThinTrails).
	ThinnedPositions int64         `json:"thinnedPositions"`
	Took             time.Duration `json:"took"`
}

// minuteTables / hourTables are the time-keyed (column t, Unix s) tables of
// the minute and hour tiers. Tables that do not exist in the database are
// skipped. stats_minute and stats_hour are handled separately (rollup
// marks).
var (
	minuteTables = []string{"receiver_status", "station_minute", "station_clock_minute", "mlat_minute", "latency_minute"}
	hourTables   = []string{"range_hour", "mlat_quality_hour"}
	histTables   = []string{"coverage", "signal_hist", "elev_hist"}
)

// Maintain runs the tiered retention and the routine maintenance of the
// database (see TierPolicy and the comment at the top of this file):
// RollupHours, RollupDays, the retention of every tier, month bucketing of
// the histograms, incremental vacuum, WAL checkpoint and PRAGMA optimize.
// It works on both kinds (tables a kind does not have are skipped). It is
// idempotent, resumable and safe to call concurrently with writers.
func (s *Store) Maintain(ctx context.Context, p TierPolicy) (MaintReport, error) {
	return s.maintainAt(ctx, p, time.Now().Unix())
}

func (s *Store) maintainAt(ctx context.Context, p TierPolicy, now int64) (rep MaintReport, err error) {
	start := time.Now()
	p = p.withDefaults()
	rep.Deleted, rep.Bucketed = map[string]int64{}, map[string]int64{}
	defer func() {
		rep.Took = time.Since(start)
		var del, buck int64
		for _, n := range rep.Deleted {
			del += n
		}
		for _, n := range rep.Bucketed {
			buck += n
		}
		del += rep.ThinnedPositions
		if del+buck > 0 {
			s.invalidateCounts()
			s.log.Info("store: maintenance", "db", s.kind.String(), "deleted", del, "bucketed", buck,
				"freed_pages", rep.FreedPages, "took", rep.Took.Round(time.Millisecond))
		}
	}()
	cut := func(days int) int64 { return now - int64(days)*86400 }

	if rep.HourRows, err = s.RollupHours(ctx); err != nil {
		return rep, fmt.Errorf("store: maintain: rollup hours: %w", err)
	}
	if rep.DayRows, err = s.RollupDays(ctx); err != nil {
		return rep, fmt.Errorf("store: maintain: rollup days: %w", err)
	}

	// Minute tier.
	if p.MinuteDays > 0 {
		c := cut(p.MinuteDays)
		var res PruneResult
		if err := s.pruneMinutes(ctx, c, &res); err != nil {
			return rep, fmt.Errorf("store: maintain: stats_minute: %w", err)
		}
		rep.Deleted["stats_minute"] += res.MinuteStats
		for _, t := range minuteTables {
			if err := s.pruneIfExists(ctx, t, "t", c, &rep); err != nil {
				return rep, err
			}
		}
	}
	// Hour tier.
	if p.HourDays > 0 {
		c := cut(p.HourDays)
		if err := s.pruneHours(ctx, c, &rep); err != nil {
			return rep, fmt.Errorf("store: maintain: stats_hour: %w", err)
		}
		for _, t := range hourTables {
			if err := s.pruneIfExists(ctx, t, "t", c, &rep); err != nil {
				return rep, err
			}
		}
	}
	// Day tier.
	if p.DayDays > 0 {
		if err := s.pruneIfExists(ctx, "stats_day", "t", cut(p.DayDays), &rep); err != nil {
			return rep, err
		}
	}
	// Histograms: delete, then bucket what is left.
	if p.HistDays > 0 {
		d := int64(DayOf(cut(p.HistDays)))
		for _, t := range histTables {
			if err := s.pruneIfExists(ctx, t, "day", d, &rep); err != nil {
				return rep, err
			}
		}
	}
	if p.HistDailyDays > 0 {
		if err := s.bucketHistograms(ctx, DayOf(cut(p.HistDailyDays)), &rep); err != nil {
			return rep, fmt.Errorf("store: maintain: month buckets: %w", err)
		}
	}
	// Heat grid.
	if p.HeatIdleDays > 0 && s.HasTable("heat") {
		if err := s.pruneHeat(ctx, now, cut(p.HeatIdleDays), &rep); err != nil {
			return rep, fmt.Errorf("store: maintain: heat: %w", err)
		}
	}

	if p.FlightStationDays > 0 && s.HasTable("flight_station") && s.HasTable("flights") {
		n, err := s.pruneFlightStation(ctx, cut(p.FlightStationDays))
		rep.Deleted["flight_station"] += n
		if err != nil {
			return rep, fmt.Errorf("store: maintain: flight_station: %w", err)
		}
	}

	// Position trails: full resolution -> one point per TrailThinSec.
	if err := s.thinTrailsStep(ctx, p, now, &rep); err != nil {
		return rep, fmt.Errorf("store: maintain: %w", err)
	}

	// Space, WAL, planner statistics.
	if rep.FreedPages, err = s.releaseFreePagesPct(ctx, p.FreePct); err != nil {
		return rep, fmt.Errorf("store: maintain: releasing free pages: %w", err)
	}
	if st := s.Stats(); st.WALBytes > p.WALCheckpointBytes {
		if err := s.Checkpoint(ctx); err != nil {
			return rep, err
		}
		rep.Checkpointed = true
	}
	last, _, _ := kvInt(ctx, s.DB(), kvLastOptimize)
	if now-last >= int64(p.OptimizeEvery/time.Second) {
		if err := s.Optimize(ctx); err != nil {
			return rep, err
		}
		if err := s.KVSet(ctx, kvLastOptimize, strconv.FormatInt(now, 10)); err != nil {
			return rep, err
		}
		rep.Optimized = true
	}
	return rep, nil
}

// pruneIfExists runs pruneByCol when the table exists.
func (s *Store) pruneIfExists(ctx context.Context, table, col string, cutoff int64, rep *MaintReport) error {
	if !s.HasTable(table) {
		return nil
	}
	var n int64
	if err := s.pruneByCol(ctx, table, col, cutoff, &n); err != nil {
		return fmt.Errorf("store: maintain: %s: %w", table, err)
	}
	rep.Deleted[table] += n
	return nil
}

// Optimize runs PRAGMA optimize, which runs ANALYZE on the tables whose
// planner statistics are missing or stale (cheap when nothing changed).
func (s *Store) Optimize(ctx context.Context) error {
	return s.execRaw(ctx, func(ctx context.Context, db *sql.DB) error {
		if _, err := db.ExecContext(ctx, `PRAGMA optimize(0x10002)`); err != nil {
			return fmt.Errorf("store: optimize: %w", err)
		}
		return nil
	})
}

// releaseFreePagesPct releases free pages when more than pct % of the file
// is free (see releaseFreePages, which uses 25 % for Prune).
func (s *Store) releaseFreePagesPct(ctx context.Context, pct int) (freed int64, err error) {
	for step := 0; step < vacuumMaxSteps; step++ {
		var before, after int64
		err := s.execRaw(ctx, func(ctx context.Context, db *sql.DB) error {
			var pages int64
			if err := db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&before); err != nil {
				return err
			}
			if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
				return err
			}
			after = before
			if before == 0 || before*100 <= pages*int64(pct) {
				return nil
			}
			if err := drain(db.QueryContext(ctx, `PRAGMA incremental_vacuum(`+strconv.Itoa(vacuumPages)+`)`)); err != nil {
				return err
			}
			return db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&after)
		})
		if err != nil {
			return freed, err
		}
		if after >= before {
			return freed, nil
		}
		freed += before - after
		s.checkpointIfFull(ctx) // each step writes up to 16 MB of WAL
	}
	return freed, nil
}

// ------------------------------------------------------------- day rollup

// dayRecompute is how many days before the newest stats_hour day RollupDays
// always recomputes (late hour rows - backfill after an outage - land in
// the last day or so).
const (
	dayRecompute    = 3
	dayRollupChunkD = 31
)

var sqlRollupDays = func() string {
	cols := statsInsertCols()
	sel := []string{"t - t % 86400", "ant"}
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
		"MIN(rssi_min)", "MAX(rssi_max)",
		`CASE WHEN SUM(CASE WHEN aircraft_avg IS NOT NULL THEN minutes ELSE 0 END) > 0
      THEN SUM(CASE WHEN aircraft_avg IS NOT NULL THEN aircraft_avg * minutes ELSE 0 END) * 1.0
           / SUM(CASE WHEN aircraft_avg IS NOT NULL THEN minutes ELSE 0 END)
      ELSE AVG(aircraft_avg) END`,
		"SUM(minutes)", "SUM(COALESCE(secs, minutes * 60.0))", "COUNT(*)",
	)
	return "INSERT OR REPLACE INTO stats_day(" + strings.Join(cols, ", ") + ", aircraft_avg, minutes, secs, hours)\nSELECT " +
		strings.Join(sel, ",\n  ") + "\nFROM stats_hour WHERE t >= ? AND t < ? GROUP BY 1, 2"
}()

// RollupDays aggregates stats_hour into stats_day (UTC days) and returns
// the number of day rows written: counters are summed, gauges, max_km and
// rssi_max take the maximum, rssi_min the minimum, rssi_avg the
// frame-weighted mean, aircraft_avg the minute-weighted mean of the hours'
// aircraft_avg, minutes / secs the sums and hours the number of hour rows.
//
// Like RollupHours it is idempotent and incremental: the days from the
// rollup mark onwards, and always the last dayRecompute days before the
// newest hour, are recomputed from all of their hour rows; days whose hour
// rows were pruned are never touched again. Call RollupHours first.
func (s *Store) RollupDays(ctx context.Context) (days int, err error) {
	if !s.HasTable("stats_day") {
		return 0, nil
	}
	for {
		var n int
		var more bool
		err := s.Write(ctx, func(tx *sql.Tx) error {
			var e error
			n, more, e = rollupDayChunk(ctx, tx)
			return e
		})
		if err != nil {
			return days, err
		}
		days += n
		if !more {
			return days, nil
		}
	}
}

func rollupDayChunk(ctx context.Context, tx *sql.Tx) (rows int, more bool, err error) {
	var newest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(t) FROM stats_hour`).Scan(&newest); err != nil {
		return 0, false, err
	}
	if !newest.Valid {
		return 0, false, nil
	}
	lastDay := floorTo(newest.Int64, 86400)
	lo := lastDay - dayRecompute*86400
	mark, ok, err := kvInt(ctx, tx, kvRollupDay)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		lo = int64(-1) << 62 // first run: everything
	} else if mark < lo {
		lo = mark
	}
	if frozen, ok, err := kvInt(ctx, tx, kvHoursPruned); err != nil {
		return 0, false, err
	} else if ok && frozen > lo {
		lo = frozen
	}
	var first sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(t) FROM stats_hour WHERE t >= ?`, lo).Scan(&first); err != nil {
		return 0, false, err
	}
	if !first.Valid {
		return 0, false, nil
	}
	lo = floorTo(first.Int64, 86400)
	end := lastDay + 86400
	if limit := lo + dayRollupChunkD*86400; limit < end {
		end, more = limit, true
	}
	res, err := tx.ExecContext(ctx, sqlRollupDays, lo, end)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	next := end
	if !more {
		next = lastDay
	}
	return int(n), more, kvSetInt(ctx, tx, kvRollupDay, next)
}

// pruneHours deletes stats_hour rows before cutoff (rounded down to a UTC
// day, never beyond the day-rollup window), freezing those days first.
func (s *Store) pruneHours(ctx context.Context, cutoff int64, rep *MaintReport) error {
	if !s.HasTable("stats_hour") {
		return nil
	}
	cutoff = floorTo(cutoff, 86400)
	skip := false
	err := s.Write(ctx, func(tx *sql.Tx) error {
		skip = false
		hasDay, err := hasTableTx(ctx, tx, "stats_day")
		if err != nil {
			return err
		}
		if hasDay {
			mark, ok, err := kvInt(ctx, tx, kvRollupDay)
			if err != nil {
				return err
			}
			if !ok {
				skip = true // never rolled up into days: keep the hours
				return nil
			}
			cutoff = min(cutoff, mark)
		}
		if old, ok, err := kvInt(ctx, tx, kvHoursPruned); err != nil {
			return err
		} else if ok && old >= cutoff {
			return nil
		}
		return kvSetInt(ctx, tx, kvHoursPruned, cutoff)
	})
	if err != nil || skip {
		return err
	}
	var n int64
	if err := s.pruneByCol(ctx, "stats_hour", "t", cutoff, &n); err != nil {
		return err
	}
	rep.Deleted["stats_hour"] += n
	return nil
}

// DayStats returns stats_day rows of one antenna key with from <= t <= to
// (Unix seconds; t is the start of a UTC day), oldest first. Minutes is the
// number of covered minutes of the day, AircraftAvg the mean aircraft gauge.
func (s *Store) DayStats(ctx context.Context, ant string, from, to int64) ([]StatsRow, error) {
	return s.queryStats(ctx, "stats_day", ant, from, to)
}

// -------------------------------------------------------- month buckets

// monthBucket returns the day number of the first day of the UTC month of
// day, and of the next month.
func monthBucket(day int) (first, next int) {
	t := time.Unix(DayStart(day), 0).UTC()
	m := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return DayOf(m.Unix()), DayOf(m.AddDate(0, 1, 0).Unix())
}

// histBucketSQL merges the rows of one table, month and ant range into the
// bucket day and deletes them. The ON CONFLICT clause adds counts (sums)
// and keeps maxima exactly like the live merge statements.
var histBucketSQL = map[string][2]string{
	"coverage": {
		`INSERT INTO coverage(day, ant, brg_bin, alt_band, max_km, count, rssi_sum, rssi_n)
SELECT ?1, ant, brg_bin, alt_band, MAX(max_km), SUM(count), SUM(rssi_sum), SUM(rssi_n) FROM coverage
WHERE day > ?1 AND day < ?2 AND ant >= ?3 AND ant <= ?4 GROUP BY ant, brg_bin, alt_band
ON CONFLICT(day, ant, brg_bin, alt_band) DO UPDATE SET
  max_km = MAX(max_km, excluded.max_km), count = count + excluded.count,
  rssi_sum = rssi_sum + excluded.rssi_sum, rssi_n = rssi_n + excluded.rssi_n`,
		`DELETE FROM coverage WHERE day > ?1 AND day < ?2 AND ant >= ?3 AND ant <= ?4`,
	},
	"signal_hist": {
		`INSERT INTO signal_hist(day, ant, dist_bin, rssi_bin, count)
SELECT ?1, ant, dist_bin, rssi_bin, SUM(count) FROM signal_hist
WHERE day > ?1 AND day < ?2 AND ant >= ?3 AND ant <= ?4 GROUP BY ant, dist_bin, rssi_bin
ON CONFLICT(day, ant, dist_bin, rssi_bin) DO UPDATE SET count = count + excluded.count`,
		`DELETE FROM signal_hist WHERE day > ?1 AND day < ?2 AND ant >= ?3 AND ant <= ?4`,
	},
	"elev_hist": {
		`INSERT INTO elev_hist(day, ant, elev_bin, count, rssi_sum, resid_sum)
SELECT ?1, ant, elev_bin, SUM(count), SUM(rssi_sum), SUM(resid_sum) FROM elev_hist
WHERE day > ?1 AND day < ?2 AND ant >= ?3 AND ant <= ?4 GROUP BY ant, elev_bin
ON CONFLICT(day, ant, elev_bin) DO UPDATE SET
  count = count + excluded.count, rssi_sum = rssi_sum + excluded.rssi_sum,
  resid_sum = resid_sum + excluded.resid_sum`,
		`DELETE FROM elev_hist WHERE day > ?1 AND day < ?2 AND ant >= ?3 AND ant <= ?4`,
	},
}

// bucketAntChunk is the number of distinct ant keys merged per transaction.
const bucketAntChunk = 64

// bucketHistograms merges the per-day rows of the histogram tables with
// day < before into month buckets, month by month and ant chunk by ant
// chunk, from a persistent watermark.
func (s *Store) bucketHistograms(ctx context.Context, before int, rep *MaintReport) error {
	var tables []string
	for _, t := range histTables {
		if s.HasTable(t) {
			tables = append(tables, t)
		}
	}
	if len(tables) == 0 {
		return nil
	}
	from := 0
	if w, ok, err := kvInt(ctx, s.DB(), kvHistBucketed); err != nil {
		return err
	} else if ok {
		from = int(w)
	} else {
		// First run: start at the oldest day present.
		oldest := before
		for _, t := range tables {
			var d sql.NullInt64
			if err := s.DB().QueryRowContext(ctx, `SELECT MIN(day) FROM `+t).Scan(&d); err != nil {
				return err
			}
			if d.Valid && int(d.Int64) < oldest {
				oldest = int(d.Int64)
			}
		}
		from = oldest
	}
	for from < before {
		if err := ctx.Err(); err != nil {
			return err
		}
		first, next := monthBucket(from)
		end := min(next, before) // merge days [first+1, end) of this month
		for _, t := range tables {
			n, err := s.bucketMonth(ctx, t, first, end)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			rep.Bucketed[t] += n
		}
		if err := s.KVSet(ctx, kvHistBucketed, strconv.Itoa(end)); err != nil {
			return err
		}
		from = end
	}
	return nil
}

// bucketMonth merges the rows of table with first < day < end into day
// first, ant chunk by ant chunk, and returns the number of rows removed.
func (s *Store) bucketMonth(ctx context.Context, table string, first, end int) (int64, error) {
	if end <= first+1 {
		return 0, nil
	}
	q := histBucketSQL[table]
	ants, err := queryStrings(ctx, s.DB(),
		`SELECT DISTINCT ant FROM `+table+` WHERE day > ? AND day < ? ORDER BY ant`, first, end)
	if err != nil {
		return 0, err
	}
	var total int64
	for i := 0; i < len(ants); i += bucketAntChunk {
		lo, hi := ants[i], ants[min(i+bucketAntChunk, len(ants))-1]
		err := s.Write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, q[0], first, end, lo, hi); err != nil {
				return err
			}
			r, err := tx.ExecContext(ctx, q[1], first, end, lo, hi)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			total += n
			return nil
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// ------------------------------------------------------------------- heat

// heatChunk is the number of heat rows examined per transaction.
const heatChunk = 20000

// heatPruneEvery bounds how often the heat walk runs (it reads the whole
// table).
const heatPruneEvery = 24 * 3600

// pruneHeat deletes heat cells whose last update is before cutoff, at most
// once per heatPruneEvery. heat has no time index (its key is (cell, ant) in
// airview.db and (ant, cell) in stations.db), so the table is walked in key
// order in chunks of heatChunk rows from a cursor, each chunk its own
// transaction.
func (s *Store) pruneHeat(ctx context.Context, now, cutoff int64, rep *MaintReport) error {
	if last, ok, err := kvInt(ctx, s.DB(), kvHeatPruned); err != nil {
		return err
	} else if ok && now-last < heatPruneEvery && now >= last {
		return nil
	}
	key := "cell, ant"
	cur := []any{int64(-1) << 62, ""}
	if s.kind == KindStations {
		key = "ant, cell"
		cur = []any{"", int64(-1) << 62}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var n int64
		next := make([]any, 2)
		done := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			n, done = 0, false
			err := tx.QueryRowContext(ctx, `SELECT `+key+` FROM heat WHERE (`+key+`) > (?, ?)
ORDER BY `+key+` LIMIT 1 OFFSET ?`, cur[0], cur[1], heatChunk-1).Scan(&next[0], &next[1])
			var r sql.Result
			switch {
			case isNoRows(err):
				done = true
				r, err = tx.ExecContext(ctx, `DELETE FROM heat WHERE (`+key+`) > (?, ?) AND last_t < ?`,
					cur[0], cur[1], cutoff)
			case err != nil:
				return err
			default:
				r, err = tx.ExecContext(ctx, `DELETE FROM heat WHERE (`+key+`) > (?, ?) AND (`+key+`) <= (?, ?)
AND last_t < ?`, cur[0], cur[1], next[0], next[1], cutoff)
			}
			if err != nil {
				return err
			}
			n, _ = r.RowsAffected()
			return nil
		})
		if err != nil {
			return err
		}
		rep.Deleted["heat"] += n
		if done {
			return s.KVSet(ctx, kvHeatPruned, strconv.FormatInt(now, 10))
		}
		cur = next
	}
}

// ------------------------------------------------------ station deletion

// stationTables lists the tables of this package (both kinds) that hold
// per-station rows, with the key column: "ant" (AntKeyWhere), "sid", or
// "feed" (StationFeedRange). Tables missing in a database are skipped.
var stationTables = []struct{ table, col string }{
	{"stats_minute", "ant"}, {"stats_hour", "ant"}, {"stats_day", "ant"}, {"range_hour", "ant"},
	{"coverage", "ant"}, {"signal_hist", "ant"}, {"elev_hist", "ant"}, {"heat", "ant"},
	{"receiver_status", "feed"}, {"latency_minute", "feed"},
	{"station_minute", "sid"}, {"station_clock_minute", "sid"},
	{"flight_station", "sid"}, {"mlat_minute", "sid"},
}

// deleteChunk is the number of rows deleted per transaction by
// DeleteStationData.
const deleteChunk = 5000

// StationKeys identifies the rows of one station: its ant key prefix
// ('s<idx>', StationAntPrefix) and its SID. Either may be empty (then the
// tables keyed by it are skipped).
type StationKeys struct {
	Prefix string
	SID    string
}

func (k StationKeys) where(col string) (string, []any, bool) {
	switch col {
	case "ant":
		if !ValidAntPrefix(k.Prefix) {
			return "", nil, false
		}
		w, a := AntKeyWhere("ant", k.Prefix)
		return w, a, true
	case "sid":
		if k.SID == "" {
			return "", nil, false
		}
		return "sid = ?", []any{k.SID}, true
	case "feed":
		if k.SID == "" {
			return "", nil, false
		}
		lo, hi := StationFeedRange(k.SID)
		return "(feed >= ? AND feed < ?)", []any{lo, hi}, true
	}
	return "", nil, false
}

// DeleteStationData removes every row of one station from the statistics
// and network tables of this database (see stationTables): ant keys of
// k.Prefix, sid = k.SID and FeedIDs 'st/<sid>/...'. Tables of other packages
// (the registry) are never touched. The work runs in transactions of at most
// deleteChunk rows; it is resumable (call again after an error or a
// cancelled ctx). It returns the rows deleted per table.
func (s *Store) DeleteStationData(ctx context.Context, k StationKeys) (map[string]int64, error) {
	out := map[string]int64{}
	if !ValidAntPrefix(k.Prefix) && k.SID == "" {
		return out, errors.New("store: DeleteStationData: no valid station key")
	}
	for _, st := range stationTables {
		where, args, ok := k.where(st.col)
		if !ok || !s.HasTable(st.table) {
			continue
		}
		pk, err := s.primaryKey(ctx, st.table)
		if err != nil {
			return out, err
		}
		n, err := s.deleteWhereChunked(ctx, st.table, pk, where, args)
		out[st.table] += n
		if err != nil {
			return out, fmt.Errorf("store: deleting station rows from %s: %w", st.table, err)
		}
	}
	s.invalidateCounts()
	return out, nil
}

// primaryKey returns the primary-key columns of a table in key order
// ("rowid" for a rowid table without an explicit key).
func (s *Store) primaryKey(ctx context.Context, table string) ([]string, error) {
	rows, err := s.DB().QueryContext(ctx, `SELECT name, pk FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			return nil, err
		}
		cols = append(cols, quoteIdent(c))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		cols = []string{"rowid"}
	}
	return cols, nil
}

// deleteWhereChunked deletes rows matching where in chunks of deleteChunk
// rows selected by primary key (works for rowid and WITHOUT ROWID tables).
func (s *Store) deleteWhereChunked(ctx context.Context, table string, pk []string, where string, args []any) (int64, error) {
	key := strings.Join(pk, ", ")
	if len(pk) > 1 {
		key = "(" + key + ")"
	}
	q := `DELETE FROM ` + quoteIdent(table) + ` WHERE ` + key + ` IN (SELECT ` + strings.Join(pk, ", ") +
		` FROM ` + quoteIdent(table) + ` WHERE ` + where + ` LIMIT ` + strconv.Itoa(deleteChunk) + `)`
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var n int64
		err := s.Write(ctx, func(tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, q, args...)
			if err != nil {
				return err
			}
			n, _ = r.RowsAffected()
			return nil
		})
		total += n
		if err != nil {
			return total, err
		}
		if n < deleteChunk {
			return total, nil
		}
	}
}

// ------------------------------------------------------------ table sizes

// TableSize is the space one table (with its indexes) takes in the file.
type TableSize struct {
	Name    string `json:"name"`
	Bytes   int64  `json:"bytes"`   // pages of the table and its indexes
	Payload int64  `json:"payload"` // bytes of stored record payload
	Pages   int64  `json:"pages"`
}

// TableSizes returns the space taken by every table and its indexes, largest
// first, from the dbstat virtual table. It reads every page of the file:
// seconds for a multi-gigabyte database, so call it for status pages and
// measurements, not per request.
func (s *Store) TableSizes(ctx context.Context) ([]TableSize, error) {
	rows, err := s.DB().QueryContext(ctx, `SELECT COALESCE(m.tbl_name, d.name), SUM(d.pgsize), SUM(d.payload), COUNT(*)
FROM dbstat d LEFT JOIN sqlite_master m ON m.name = d.name
GROUP BY 1 ORDER BY 2 DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: table sizes: %w", err)
	}
	defer rows.Close()
	var out []TableSize
	for rows.Next() {
		var t TableSize
		if err := rows.Scan(&t.Name, &t.Bytes, &t.Payload, &t.Pages); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// pruneFlightStation deletes the flight_station rows of flights that ended
// before cutoff. It walks the flights by end time (index flights_end) from a
// persistent watermark and deletes by flight_id (the key's first column),
// pruneFlightChunk flights per transaction.
func (s *Store) pruneFlightStation(ctx context.Context, cutoff int64) (int64, error) {
	var curEnd, curID int64 = -1 << 62, 0
	if w, ok, err := kvInt(ctx, s.DB(), kvFSPruned); err != nil {
		return 0, err
	} else if ok {
		curEnd, curID = w-1, int64(^uint64(0)>>1) // revisit the watermark second
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var n, nextEnd, nextID int64
		done := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			n, done, nextEnd, nextID = 0, false, curEnd, curID
			rows, err := tx.QueryContext(ctx, `SELECT id, "end" FROM flights WHERE ("end", id) > (?, ?) AND "end" < ?
ORDER BY "end", id LIMIT ?`, curEnd, curID, cutoff, pruneFlightChunk)
			if err != nil {
				return err
			}
			var ids []int64
			for rows.Next() {
				if err := rows.Scan(&nextID, &nextEnd); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, nextID)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			done = len(ids) < pruneFlightChunk
			del, err := tx.PrepareContext(ctx, `DELETE FROM flight_station WHERE flight_id = ?`)
			if err != nil {
				return err
			}
			defer del.Close()
			for _, id := range ids {
				r, err := del.ExecContext(ctx, id)
				if err != nil {
					return err
				}
				k, _ := r.RowsAffected()
				n += k
			}
			if len(ids) > 0 && nextEnd > curEnd {
				return kvSetInt(ctx, tx, kvFSPruned, nextEnd)
			}
			return nil
		})
		total += n
		if err != nil || done {
			return total, err
		}
		curEnd, curID = nextEnd, nextID
	}
}
