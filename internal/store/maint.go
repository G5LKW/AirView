package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"os"
	"strconv"
	"time"

	"airview/internal/model"
)

// Keys of the kv table used by the store itself. The prefix "store." is
// reserved for this package.
const (
	// kvRollupHour is the start (Unix seconds) of the first hour that is not
	// final in stats_hour yet: every hour before it has been rolled up and no
	// minute row for it arrived since.
	kvRollupHour = "store.rollup_hour"
	// kvMinutesPruned: stats_minute rows before this time were (or are being)
	// deleted by Prune, so stats_hour rows before it are frozen.
	kvMinutesPruned = "store.minutes_pruned"
	// kvPositionsPruned: the trail positions of every flight that ended
	// before this time have been deleted by Prune.
	kvPositionsPruned = "store.positions_pruned"
)

// kvInt reads an integer value of the kv table inside a transaction.
func kvInt(ctx context.Context, q querier, key string) (v int64, ok bool, err error) {
	var s string
	err = q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	v, perr := strconv.ParseInt(s, 10, 64)
	return v, perr == nil, nil
}

func kvSetInt(ctx context.Context, tx *sql.Tx, key string, v int64) error {
	_, err := tx.ExecContext(ctx, sqlSetKV, key, strconv.FormatInt(v, 10), time.Now().Unix())
	return err
}

// ------------------------------------------------------------------ rollup

// rollupChunkHours bounds the hours aggregated in one transaction, so a long
// backlog (first rollup of an old database) never holds the writer for long.
const rollupChunkHours = 7 * 24

// RollupHours aggregates stats_minute into stats_hour and returns the number
// of hour rows written. Per (hour, antenna) the counters are summed, the
// aircraft gauges, max_km and rssi_max take the maximum, rssi_min the
// minimum, rssi_avg the frame-weighted mean; aircraft_avg is the mean of the
// per-minute aircraft gauge and minutes the number of minute rows.
//
// It is idempotent and incremental: an hour row is always recomputed from
// all of its minute rows (never added to), and only the hours that can have
// changed are recomputed - those from the rollup mark (a kv entry) onwards.
// The mark advances to the newest hour that has minute rows, which stays
// "open" and is recomputed by every call until a later hour appears; a
// minute row that arrives late for an older hour pulls the mark back
// automatically. Hours whose minute rows were deleted by Prune are never
// touched again, so stats_hour is kept forever.
//
// Call it periodically (every minute or few) and before reading stats_hour
// when the newest hour matters. It does not depend on the system clock.
func (s *Store) RollupHours(ctx context.Context) (hours int, err error) {
	for {
		var n int
		var more bool
		err := s.Write(ctx, func(tx *sql.Tx) error {
			var e error
			n, more, e = rollupChunk(ctx, tx)
			return e
		})
		if err != nil {
			return hours, err
		}
		hours += n
		if !more {
			return hours, nil
		}
	}
}

// rollupChunk rolls up at most rollupChunkHours hours starting at the first
// hour, at or after the mark, that has minute rows.
func rollupChunk(ctx context.Context, tx *sql.Tx) (rows int, more bool, err error) {
	var lo int64
	if mark, ok, err := kvInt(ctx, tx, kvRollupHour); err != nil {
		return 0, false, err
	} else if ok {
		lo = mark
	}
	if pruned, ok, err := kvInt(ctx, tx, kvMinutesPruned); err != nil {
		return 0, false, err
	} else if ok && pruned > lo {
		lo = pruned // a late row for an hour whose minutes are gone: leave the hour alone
	}

	var first, last sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		// Two sub-selects: each is one b-tree probe, MIN and MAX together a scan.
		`SELECT (SELECT MIN(t) FROM stats_minute WHERE t >= ?1), (SELECT MAX(t) FROM stats_minute WHERE t >= ?1)`, lo).Scan(&first, &last); err != nil {
		return 0, false, err
	}
	if !first.Valid {
		return 0, false, nil // nothing new
	}
	lo = floorTo(first.Int64, 3600) // skips gaps (receiver down, clock jumps)
	lastHour := floorTo(last.Int64, 3600)
	end := lastHour + 3600
	if limit := lo + rollupChunkHours*3600; limit < end {
		end, more = limit, true
	}

	res, err := tx.ExecContext(ctx, sqlRollupHours, lo, end)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()

	// Everything before the mark is final; the newest hour stays open.
	if err := kvSetInt(ctx, tx, kvRollupHour, min(end, lastHour)); err != nil {
		return 0, false, err
	}
	return int(n), more, nil
}

// ------------------------------------------------------------------- prune

// PruneResult reports the rows Prune deleted.
type PruneResult struct {
	Flights        int64 `json:"flights"`
	Positions      int64 `json:"positions"`
	MinuteStats    int64 `json:"minuteStats"`
	MetObs         int64 `json:"metObs"`
	NTPSamples     int64 `json:"ntpSamples"`
	ReceiverStatus int64 `json:"receiverStatus"`
	// FreedPages is the number of database pages handed back to the file
	// system (only after a large clean-up; normally freed pages are reused).
	FreedPages int64 `json:"freedPages"`
	// Extra holds the rows deleted by registered pruners of other owners
	// that have no field above (name -> rows).
	Extra map[string]int64 `json:"extra,omitempty"`
}

// Total returns the total number of rows deleted.
func (r PruneResult) Total() int64 {
	n := r.Flights + r.Positions + r.MinuteStats + r.MetObs + r.NTPSamples + r.ReceiverStatus
	for _, v := range r.Extra {
		n += v
	}
	return n
}

// AddExtra adds n rows to Extra[name].
func (r *PruneResult) AddExtra(name string, n int64) {
	if n == 0 {
		return
	}
	if r.Extra == nil {
		r.Extra = map[string]int64{}
	}
	r.Extra[name] += n
}

// Chunk sizes of Prune: small enough that one transaction takes well under a
// second on a Raspberry Pi 4 with an SD card.
const (
	pruneFlightChunk   = 200   // flights examined per transaction
	prunePositionChunk = 20000 // position rows deleted per transaction (soft limit)
	pruneRowChunk      = 5000  // rows of a time-keyed table deleted per transaction
	vacuumPages        = 4000  // pages released per incremental-vacuum step
	vacuumMaxSteps     = 8     // steps per Prune call
)

// Prune applies the retention periods of cfg:
//
//	TrailRetentionDays   positions of flights that ended longer ago, and
//	                     met_obs rows (the other per-aircraft raw data)
//	FlightRetentionDays  flights (with their positions) that ended longer ago
//	MinuteRetentionDays  stats_minute, ntp_samples and receiver_status rows
//
// A period <= 0 means "keep forever". Hourly, daily and all-time tables
// (stats_hour, range_hour, coverage, signal_hist, elev_hist, heat, weather,
// met_profile, aircraft, records, events) are never pruned.
//
// stats_minute rows are deleted only after they were rolled up into
// stats_hour (Prune runs RollupHours first). The work is done in many small
// transactions, with queued writes flushed in between, so the writer is
// never blocked for long; cancelling ctx stops between two chunks and the
// next call simply continues. Prune is meant to be called periodically
// (e.g. hourly); concurrent calls are safe but pointless.
func (s *Store) Prune(ctx context.Context, cfg model.StorageConfig) (PruneResult, error) {
	return s.pruneAt(ctx, cfg, time.Now().Unix())
}

func (s *Store) pruneAt(ctx context.Context, cfg model.StorageConfig, now int64) (res PruneResult, err error) {
	defer func() {
		if res.Total() > 0 {
			s.invalidateCounts()
			s.log.Info("store: pruned", "flights", res.Flights, "positions", res.Positions,
				"minute_stats", res.MinuteStats, "met_obs", res.MetObs, "ntp_samples", res.NTPSamples,
				"receiver_status", res.ReceiverStatus, "freed_pages", res.FreedPages)
		}
	}()

	if _, err := s.RollupHours(ctx); err != nil {
		return res, fmt.Errorf("store: prune: rollup: %w", err)
	}
	cutoff := func(days int) int64 { return now - int64(days)*86400 }

	// Registered pruners of the other owners (trail chunks, met samples ...)
	// first: they may still need the flights rows the legacy step deletes.
	for _, p := range registeredPruners() {
		if err := p.fn(ctx, s, cfg, now, &res); err != nil {
			return res, fmt.Errorf("store: prune %s: %w", p.name, err)
		}
	}

	// The 1.0.0 tables (legacy layouts): each step is a no-op when its table
	// is gone or empty after a data migration.
	main := s.kind == KindMain // stations.db has no flights, trails, met or NTP tables
	if main && cfg.FlightRetentionDays > 0 {
		if err := s.pruneFlights(ctx, cutoff(cfg.FlightRetentionDays), &res); err != nil {
			return res, fmt.Errorf("store: prune flights: %w", err)
		}
	}
	if main && cfg.TrailRetentionDays > 0 {
		cut := cutoff(cfg.TrailRetentionDays)
		if s.tableHasRows(ctx, "positions") {
			if err := s.prunePositions(ctx, cut, &res); err != nil {
				return res, fmt.Errorf("store: prune positions: %w", err)
			}
		}
		if s.tableHasRows(ctx, "met_obs") {
			if err := s.pruneByTime(ctx, "met_obs", cut, &res.MetObs); err != nil {
				return res, fmt.Errorf("store: prune met_obs: %w", err)
			}
		}
	}
	if cfg.MinuteRetentionDays > 0 {
		cut := cutoff(cfg.MinuteRetentionDays)
		if err := s.pruneMinutes(ctx, cut, &res); err != nil {
			return res, fmt.Errorf("store: prune stats_minute: %w", err)
		}
		if main {
			if err := s.pruneByTime(ctx, "ntp_samples", cut, &res.NTPSamples); err != nil {
				return res, fmt.Errorf("store: prune ntp_samples: %w", err)
			}
		}
		if err := s.pruneByTime(ctx, "receiver_status", cut, &res.ReceiverStatus); err != nil {
			return res, fmt.Errorf("store: prune receiver_status: %w", err)
		}
	}

	freed, err := s.releaseFreePages(ctx)
	res.FreedPages = freed
	if err != nil {
		return res, fmt.Errorf("store: prune: releasing free pages: %w", err)
	}
	return res, nil
}

// pruneFlights deletes flights that ended before cutoff together with their
// positions.
func (s *Store) pruneFlights(ctx context.Context, cutoff int64, res *PruneResult) error {
	for {
		var flights, positions int64
		done := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			flights, positions, done = 0, 0, false
			ids, err := queryInt64s(ctx, tx,
				`SELECT id FROM flights WHERE "end" < ? ORDER BY "end" LIMIT ?`, cutoff, pruneFlightChunk)
			if err != nil {
				return err
			}
			done = len(ids) < pruneFlightChunk
			delPos, err := prepareDeletePositions(ctx, tx)
			if err != nil {
				return err
			}
			defer delPos.Close()
			delFlight, err := tx.PrepareContext(ctx, `DELETE FROM flights WHERE id = ?`)
			if err != nil {
				return err
			}
			defer delFlight.Close()
			delFS, err := tx.PrepareContext(ctx, `DELETE FROM flight_station WHERE flight_id = ?`)
			if err != nil {
				return err
			}
			defer delFS.Close()
			for _, id := range ids {
				r, err := delPos.ExecContext(ctx, id)
				if err != nil {
					return err
				}
				n, _ := r.RowsAffected()
				positions += n
				if _, err := delFS.ExecContext(ctx, id); err != nil {
					return err
				}
				if _, err := delFlight.ExecContext(ctx, id); err != nil {
					return err
				}
				flights++
				if positions >= prunePositionChunk {
					done = false // the rest is picked up by the next chunk
					break
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		res.Flights += flights
		res.Positions += positions
		if done {
			return nil
		}
	}
}

// prunePositions deletes the trail of every flight that ended before cutoff.
// The positions table has no index on t, so it walks the flights in order of
// their end time (index flights_end) from a persistent watermark, which makes
// repeated calls cheap: each flight is visited once.
func (s *Store) prunePositions(ctx context.Context, cutoff int64, res *PruneResult) error {
	var curEnd, curID int64 // cursor: last flight handled
	if w, ok, err := kvInt(ctx, s.DB(), kvPositionsPruned); err != nil {
		return err
	} else if ok {
		// Flights that ended exactly at the watermark second are revisited.
		curEnd, curID = w-1, int64(^uint64(0)>>1)
	}
	type flightRef struct {
		id, end, positions int64
	}
	for {
		var deleted int64
		var nextEnd, nextID int64
		done := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			deleted, done = 0, false
			nextEnd, nextID = curEnd, curID
			rows, err := tx.QueryContext(ctx, `SELECT id, "end", positions FROM flights
WHERE ("end", id) > (?, ?) AND "end" < ? ORDER BY "end", id LIMIT ?`, curEnd, curID, cutoff, pruneFlightChunk)
			if err != nil {
				return err
			}
			var refs []flightRef
			for rows.Next() {
				var f flightRef
				if err := rows.Scan(&f.id, &f.end, &f.positions); err != nil {
					rows.Close()
					return err
				}
				refs = append(refs, f)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			done = len(refs) < pruneFlightChunk

			del, err := tx.PrepareContext(ctx, `DELETE FROM positions WHERE flight_id = ?`)
			if err != nil {
				return err
			}
			defer del.Close()
			for _, f := range refs {
				if f.positions > 0 { // flights without a position have no trail
					r, err := del.ExecContext(ctx, f.id)
					if err != nil {
						return err
					}
					n, _ := r.RowsAffected()
					deleted += n
				}
				nextEnd, nextID = f.end, f.id
				if deleted >= prunePositionChunk {
					done = false
					break
				}
			}
			if nextEnd > curEnd {
				return kvSetInt(ctx, tx, kvPositionsPruned, nextEnd)
			}
			return nil
		})
		if err != nil {
			return err
		}
		res.Positions += deleted
		curEnd, curID = nextEnd, nextID
		if done {
			break
		}
	}

	// Orphans: old positions of flights whose row never reached the database
	// (or was deleted outside Prune). Only below the oldest stored flight, so
	// the scan is a short range of the primary key.
	for {
		var n int64
		err := s.Write(ctx, func(tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, `DELETE FROM positions WHERE flight_id IN (
  SELECT DISTINCT flight_id FROM positions
  WHERE flight_id < (SELECT MIN(id) FROM flights) AND t < ?1 LIMIT 100) AND t < ?1`, cutoff)
			if err != nil {
				return err
			}
			n, _ = r.RowsAffected()
			return nil
		})
		if err != nil {
			return err
		}
		res.Positions += n
		if n == 0 {
			return nil
		}
	}
}

// pruneMinutes deletes stats_minute rows before cutoff - rounded down to a
// full hour and never beyond the rollup mark, so that only minutes that are
// safely aggregated in stats_hour disappear.
func (s *Store) pruneMinutes(ctx context.Context, cutoff int64, res *PruneResult) error {
	cutoff = floorTo(cutoff, 3600)
	skip := false
	err := s.Write(ctx, func(tx *sql.Tx) error {
		skip = false
		mark, ok, err := kvInt(ctx, tx, kvRollupHour)
		if err != nil {
			return err
		}
		if !ok {
			skip = true // nothing was ever rolled up: there is nothing to prune either
			return nil
		}
		cutoff = min(cutoff, mark)
		// Freeze the hours first: from now on RollupHours leaves them alone,
		// even if this prune is interrupted half-way through an hour.
		if old, ok, err := kvInt(ctx, tx, kvMinutesPruned); err != nil {
			return err
		} else if ok && old >= cutoff {
			return nil
		}
		return kvSetInt(ctx, tx, kvMinutesPruned, cutoff)
	})
	if err != nil || skip {
		return err
	}
	return s.pruneByTime(ctx, "stats_minute", cutoff, &res.MinuteStats)
}

// pruneByTime deletes the rows of table with t < cutoff, oldest first, in
// chunks of pruneRowChunk rows. Every table it is used on has an index (or
// primary key) that starts with t. table is a constant of this package,
// never user input.
func (s *Store) pruneByTime(ctx context.Context, table string, cutoff int64, counter *int64) error {
	return s.pruneByCol(ctx, table, "t", cutoff, counter)
}

// pruneByCol deletes the rows of table with col < cutoff, oldest first, in
// chunks of pruneRowChunk rows. table and col are constants of this package
// (never user input) and col leads the table's primary key or an index.
func (s *Store) pruneByCol(ctx context.Context, table, col string, cutoff int64, counter *int64) error {
	for {
		var n int64
		done := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			n, done = 0, false
			// The t of the last row of this chunk, if a full chunk exists.
			var boundary int64
			err := tx.QueryRowContext(ctx, `SELECT `+col+` FROM `+table+` WHERE `+col+` < ? ORDER BY `+col+` LIMIT 1 OFFSET ?`,
				cutoff, pruneRowChunk-1).Scan(&boundary)
			var r sql.Result
			switch {
			case errors.Is(err, sql.ErrNoRows):
				done = true
				r, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+col+` < ?`, cutoff)
			case err != nil:
				return err
			default:
				r, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+col+` <= ?`, boundary)
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
		*counter += n
		if done {
			return nil
		}
	}
}

// prepareDeletePositions prepares the per-flight delete of legacy trail
// rows; without a positions table (dropped by a data migration) the
// statement is a no-op that takes the same argument.
func prepareDeletePositions(ctx context.Context, tx *sql.Tx) (*sql.Stmt, error) {
	ok, err := hasTableTx(ctx, tx, "positions")
	if err != nil {
		return nil, err
	}
	if !ok {
		return tx.PrepareContext(ctx, `SELECT ? WHERE 0`)
	}
	return tx.PrepareContext(ctx, `DELETE FROM positions WHERE flight_id = ?`)
}

// tableHasRows reports whether table exists and holds at least one row (a
// single b-tree probe).
func (s *Store) tableHasRows(ctx context.Context, table string) bool {
	if !s.HasTable(table) {
		return false
	}
	var one int
	err := s.DB().QueryRowContext(ctx, `SELECT 1 FROM `+quoteIdent(table)+` LIMIT 1`).Scan(&one)
	return err == nil
}

func queryInt64s(ctx context.Context, q querier, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// releaseFreePages returns free pages to the file system when a large part
// of the file is unused (after the retention was shortened, say). In normal
// operation SQLite reuses the pages freed by Prune for new rows, and
// shrinking the file only to grow it again would just wear the SD card, so
// nothing happens unless more than a quarter of the file is free.
func (s *Store) releaseFreePages(ctx context.Context) (freed int64, err error) {
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
			if before < vacuumPages || before*4 < pages {
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
			return freed, nil // nothing (more) to release, or auto_vacuum is off
		}
		freed += before - after
		s.checkpointIfFull(ctx) // each step writes up to 16 MB of WAL
	}
	return freed, nil
}

// drain steps a statement to completion, discarding its rows. Some PRAGMAs
// (incremental_vacuum, wal_checkpoint) do their work while being stepped.
func drain(rows *sql.Rows, err error) error {
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

// ------------------------------------------------- checkpoint, vacuum, stats

// Checkpoint moves the content of the write-ahead log into the database
// file and truncates the log (PRAGMA wal_checkpoint(TRUNCATE)), after
// flushing the queue. SQLite checkpoints automatically every ~4 MiB of WAL;
// call this before copying the database file or to bound the WAL size after
// a burst. If a long-running reader holds an old snapshot, the checkpoint
// is partial; that is not an error, the next one catches up.
func (s *Store) Checkpoint(ctx context.Context) error {
	// Jobs that must precede a TRUNCATE (trail seal) run first.
	s.runBeforeTruncate(ctx)
	return s.execRaw(ctx, func(ctx context.Context, db *sql.DB) error {
		// Do not wait the full busy timeout for a reader.
		if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 1000`); err != nil {
			return err
		}
		defer func() {
			if _, err := db.ExecContext(context.Background(), `PRAGMA busy_timeout = `+busyTimeoutMs); err != nil {
				s.log.Warn("store: restoring busy_timeout failed", "err", err)
			}
		}()
		var busy, logFrames, done sql.NullInt64
		if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil {
			return fmt.Errorf("store: checkpoint: %w", err)
		}
		s.ckpt.truncates.Add(1)
		s.ckpt.lastNs.Store(time.Now().UnixNano())
		s.ckpt.lastFrames.Store(done.Int64)
		if busy.Int64 != 0 {
			s.log.Debug("store: checkpoint incomplete (a reader is active)",
				"wal_frames", logFrames.Int64, "checkpointed", done.Int64)
		}
		return nil
	})
}

// optimizeLimited runs PRAGMA optimize with analysis_limit=400 (hourly).
func (s *Store) optimizeLimited(ctx context.Context) error {
	return s.execRaw(ctx, func(ctx context.Context, db *sql.DB) error {
		if _, err := db.ExecContext(ctx, `PRAGMA analysis_limit = 400`); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `PRAGMA optimize(0x10002)`); err != nil {
			return fmt.Errorf("store: optimize: %w", err)
		}
		return nil
	})
}

// Vacuum rebuilds the database file compactly (SQL VACUUM) and truncates the
// WAL. It blocks the writer for the whole run - seconds to minutes for a
// large database on an SD card - while queued writes pile up in memory
// (bounded, see Options.MaxQueueRows), and it temporarily needs free disk
// space of about the size of the database. Reserve it for maintenance
// requested by the user; routine space reuse needs no VACUUM.
func (s *Store) Vacuum(ctx context.Context) error {
	err := s.execRaw(ctx, func(ctx context.Context, db *sql.DB) error {
		// VACUUM builds the new database in a temporary database: on disk,
		// not in RAM (the connection default is temp_store=MEMORY).
		if _, err := db.ExecContext(ctx, `PRAGMA temp_store = FILE`); err != nil {
			return err
		}
		defer func() {
			if _, err := db.ExecContext(context.Background(), `PRAGMA temp_store = MEMORY`); err != nil {
				s.log.Warn("store: restoring temp_store failed", "err", err)
			}
		}()
		if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
			return fmt.Errorf("store: vacuum: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.invalidateCounts()
	return s.Checkpoint(ctx)
}

// Stats is a snapshot of the store's size and writer counters.
type Stats struct {
	Path     string `json:"path"`
	DBBytes  int64  `json:"dbBytes"`  // size of airview.db
	WALBytes int64  `json:"walBytes"` // size of airview.db-wal

	PageSize      int64 `json:"pageSize"`
	PageCount     int64 `json:"pageCount"`
	FreelistPages int64 `json:"freelistPages"` // unused pages inside the file

	QueuedRows int `json:"queuedRows"` // rows waiting for the next flush
	QueuedOps  int `json:"queuedOps"`

	Flushes     uint64 `json:"flushes"`     // successful batch transactions
	FlushedRows uint64 `json:"flushedRows"` // rows written by them
	FlushErrors uint64 `json:"flushErrors"` // failed flushes (rows were kept and retried)
	FailedOps   uint64 `json:"failedOps"`   // operations dropped because their data could not be written
	// CombinedCommits: synchronous writes (Write, Apply) that shared the
	// commit of the queued rows instead of adding one of their own.
	CombinedCommits uint64 `json:"combinedCommits"`

	// Rows dropped because the queue overflowed (disk too slow or full), by
	// importance: Low = histogram / heat-map increments and raw met
	// observations, Mid = trail positions, NTP samples and receiver status,
	// High = everything else (only when nothing less important was left).
	DroppedLow  uint64 `json:"droppedLow"`
	DroppedMid  uint64 `json:"droppedMid"`
	DroppedHigh uint64 `json:"droppedHigh"`

	LastFlush   int64   `json:"lastFlush"`   // Unix ms of the last successful flush, 0 = never
	LastFlushMs float64 `json:"lastFlushMs"` // its duration
	LastError   string  `json:"lastError,omitempty"`

	// Checkpointer (§3.1): PASSIVE checkpoints run outside the commits,
	// escalations to RESTART, TRUNCATE checkpoints (hourly / Checkpoint),
	// PASSIVE runs a reader kept from finishing, and the WAL content not
	// yet copied into the database file.
	Checkpoints           uint64  `json:"checkpoints"`
	CheckpointRestarts    uint64  `json:"checkpointRestarts"`
	CheckpointTruncates   uint64  `json:"checkpointTruncates"`
	CheckpointsIncomplete uint64  `json:"checkpointsIncomplete"`
	LastCheckpoint        int64   `json:"lastCheckpoint"`   // Unix ms, 0 = none since start
	LastCheckpointMs      float64 `json:"lastCheckpointMs"` // duration of the last PASSIVE / RESTART
	WALPendingBytes       int64   `json:"walPendingBytes"`
	VacuumedPages         int64   `json:"vacuumedPages"` // released by the post-migration vacuum tail
}

// WriterState is the cheap health view of the writer (no I/O).
type WriterState struct {
	LastFlush  time.Time // last successful flush (zero = never)
	QueuedRows int       // rows waiting
	Dropped    uint64    // rows dropped on queue overflow, all tiers
	LastError  string
}

// WriterState returns the writer's health counters without touching the
// disk (for health endpoints).
func (s *Store) WriterState() WriterState {
	w := WriterState{Dropped: s.nDropped[tierLow].Load() + s.nDropped[tierMid].Load() + s.nDropped[tierHigh].Load()}
	if ns := s.lastFlushNs.Load(); ns != 0 {
		w.LastFlush = time.Unix(0, ns)
	}
	if e := s.lastErr.Load(); e != nil {
		w.LastError = *e
	}
	s.qmu.Lock()
	w.QueuedRows = s.qrows
	s.qmu.Unlock()
	return w
}

// Stats returns a snapshot of the store's size and writer counters. It is
// cheap (two file stats and three PRAGMAs on the read pool) and never fails:
// values that cannot be determined are zero.
func (s *Store) Stats() Stats {
	st := Stats{
		Path:            s.path,
		Flushes:         s.nFlushes.Load(),
		FlushedRows:     s.nFlushedRows.Load(),
		FlushErrors:     s.nFlushErrors.Load(),
		FailedOps:       s.nFailedOps.Load(),
		CombinedCommits: s.nCombined.Load(),
		DroppedLow:      s.nDropped[tierLow].Load(),
		DroppedMid:      s.nDropped[tierMid].Load(),
		DroppedHigh:     s.nDropped[tierHigh].Load(),
		LastFlushMs:     float64(s.lastFlushDur.Load()) / 1e6,
	}
	if ns := s.lastFlushNs.Load(); ns != 0 {
		st.LastFlush = ns / 1e6
	}
	if e := s.lastErr.Load(); e != nil {
		st.LastError = *e
	}
	st.Checkpoints = s.ckpt.passive.Load()
	st.CheckpointRestarts = s.ckpt.restarts.Load()
	st.CheckpointTruncates = s.ckpt.truncates.Load()
	st.CheckpointsIncomplete = s.ckpt.incomplete.Load()
	if ns := s.ckpt.lastNs.Load(); ns != 0 {
		st.LastCheckpoint = ns / 1e6
	}
	st.LastCheckpointMs = float64(s.ckpt.lastDurNs.Load()) / 1e6
	st.WALPendingBytes, _ = s.walPendingBytes()
	st.VacuumedPages = s.ckpt.vacuumed.Load()
	if fi, err := os.Stat(s.path); err == nil {
		st.DBBytes = fi.Size()
	}
	if fi, err := os.Stat(s.path + "-wal"); err == nil {
		st.WALBytes = fi.Size()
	}
	s.qmu.Lock()
	st.QueuedRows, st.QueuedOps = s.qrows, len(s.queue)
	s.qmu.Unlock()

	if !s.closed.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		db := s.DB()
		_ = db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&st.PageSize)
		_ = db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&st.PageCount)
		_ = db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&st.FreelistPages)
	}
	return st
}

// -------------------------------------------------------------- acdb import

// acdbImportChunk is the number of rows per import transaction: large
// enough to be fast (one fsync-free WAL append per chunk), small enough that
// the live statistics waiting behind it are delayed by about a second.
const acdbImportChunk = 50000

// ImportACDB bulk-loads the aircraft database from rows (an iterator, so a
// CSV / JSON reader can stream hundreds of thousands of entries without
// holding them in memory). Entries are written with one prepared statement
// in large transactions of 50,000 rows; an entry replaces the stored entry
// with the same ICAO address, entries with ICAO 0 are skipped.
//
// With replace set, the old content of the table is deleted first (in the
// transaction of the first chunk), so the table ends up holding exactly the
// imported set. The import is not atomic as a whole: readers may see it
// half-loaded for a moment, and after an error or a cancelled ctx the rows
// imported so far stay - simply run the import again.
//
// It returns the number of entries written.
func (s *Store) ImportACDB(ctx context.Context, replace bool, rows iter.Seq[ACDBEntry]) (int, error) {
	total := 0
	first := true
	chunk := make([]ACDBEntry, 0, 4096)

	write := func() error {
		if len(chunk) == 0 && !(first && replace) {
			return nil
		}
		var b Batch
		if first && replace {
			b.add("acdb", tierHigh, 1, func(tc *txc) error {
				_, err := tc.tx.ExecContext(tc.ctx, `DELETE FROM acdb`)
				return err
			})
		}
		first = false
		n := len(chunk)
		entries := chunk // insertACDB runs before Apply returns: no copy needed
		if n > 0 {
			b.add("acdb", tierHigh, n, func(tc *txc) error { return insertACDB(tc, entries) })
		}
		if err := s.Apply(ctx, &b); err != nil {
			return err
		}
		total += n
		chunk = chunk[:0]
		return nil
	}

	var err error
	if rows != nil {
		for e := range rows {
			if e.ICAO == 0 {
				continue
			}
			chunk = append(chunk, e)
			if len(chunk) >= acdbImportChunk {
				if err = write(); err != nil {
					break
				}
			}
		}
	}
	if err == nil {
		err = write()
	}
	if err != nil {
		return total, fmt.Errorf("store: importing aircraft database: %w", err)
	}
	return total, nil
}
