package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"airview/internal/model"
)

// Data migration "trail" 1 (DB-EFFICIENCY-DESIGN §5.2): the legacy
// positions rows of a 1.0.0 database move into compacted trail chunks,
// lowest flight ID (= oldest flight) first.
//
// One step converts at most trailMigrateFlights flights or budget points
// (default trailMigrateBudget) in the caller's transaction: per flight it
// reads the legacy rows and any chunks the flight already has (a flight that
// continued after the upgrade), merges them (chunk wins on equal t, as in the
// readers), writes 256-point deflate chunks whose decode is verified before
// anything is deleted, deletes the old chunks and the legacy rows, and moves
// the counters. The cursor is the last flight ID converted, so a step that
// is re-run after a crash (its transaction rolled back) does the same work
// again, and a committed step is never repeated. Readers merge legacy rows,
// chunks and tail at every point in between. Finish keeps the empty
// positions table (old tools still find it).

const (
	trailMigrateFlights = 64
	trailMigrateBudget  = 50_000
)

// trailMigrateEstimate returns the legacy rows left and the extra bytes the
// rest of the migration needs at its peak: the chunks are written before
// the legacy pages they replace become free (about 7 B per point, plus WAL
// that a checkpoint returns).
func trailMigrateEstimate(ctx context.Context, q querier) (rows, peakBytes int64, err error) {
	st, err := readTrailState(ctx, q)
	if err != nil {
		return 0, 0, err
	}
	n, ok, err := legacyCount(ctx, q, st)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		// Never counted: estimate from the table's pages (38 B per row on
		// the 1.0.0 layout) instead of walking it.
		var pages int64
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM dbstat WHERE name = 'positions'`).Scan(&pages); err != nil {
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM positions`).Scan(&n); err != nil {
				return 0, 0, err
			}
		} else {
			n = pages * 4096 / 38
		}
	}
	return n, n*7 + 64<<20, nil
}

func parseTrailCursor(cursor []byte) int64 {
	if len(cursor) == 0 {
		return math.MinInt64
	}
	v, err := strconv.ParseInt(string(cursor), 10, 64)
	if err != nil {
		return math.MinInt64
	}
	return v
}

// trailMigrateStep converts the legacy rows of the next flights after
// cursor (see the file comment). budget <= 0 selects trailMigrateBudget.
func trailMigrateStep(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) (next []byte, done bool, err error) {
	if budget <= 0 {
		budget = trailMigrateBudget
	}
	last := parseTrailCursor(cursor)
	st, err := readTrailState(ctx, tx)
	if err != nil {
		return cursor, false, err
	}
	w, err := newChunkWriter(ctx, tx)
	if err != nil {
		return cursor, false, err
	}
	defer w.close()
	seek, err := tx.PrepareContext(ctx, `SELECT flight_id FROM positions WHERE flight_id > ? ORDER BY flight_id LIMIT 1`)
	if err != nil {
		return cursor, false, err
	}
	defer seek.Close()

	points, flights := 0, 0
	var legacy, chunk []TrailRow
	for flights < trailMigrateFlights && points < budget {
		var fid int64
		err := seek.QueryRowContext(ctx, last).Scan(&fid)
		if errors.Is(err, sql.ErrNoRows) {
			done = true
			break
		}
		if err != nil {
			return cursor, false, err
		}
		if legacy, err = queryTrailRows(ctx, tx, legacy[:0], `SELECT `+trailCols+` FROM positions WHERE flight_id = ? ORDER BY t`, fid); err != nil {
			return cursor, false, err
		}
		cs, err := queryChunks(ctx, tx, `SELECT id, flight_id, t0, t1, n, data FROM trail_chunk WHERE flight_id = ? ORDER BY t0`, fid)
		if err != nil {
			return cursor, false, err
		}
		if chunk, err = decodeChunks(chunk[:0], fid, cs); err != nil {
			return cursor, false, err
		}
		merged := mergeTrail(legacy, chunk, nil)
		if err := deleteChunks(ctx, tx, cs); err != nil {
			return cursor, false, err
		}
		if err := w.write(fid, merged, trailLevelCompact); err != nil {
			return cursor, false, err
		}
		r, err := tx.ExecContext(ctx, `DELETE FROM positions WHERE flight_id = ?`, fid)
		if err != nil {
			return cursor, false, err
		}
		deleted, _ := r.RowsAffected()
		var oldN int64
		for _, c := range cs {
			oldN += c.n
		}
		st.Chunk += int64(len(merged)) - oldN
		st.LegacyDel += deleted
		points += len(legacy)
		flights++
		last = fid
	}
	if err := writeTrailState(ctx, tx, st); err != nil {
		return cursor, false, err
	}
	return []byte(strconv.FormatInt(last, 10)), done, nil
}

// trailMigrateFinish runs once the legacy table is empty.
func trailMigrateFinish(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, kvTrailLegacy)
	return err
}

// migrateTrailsNow runs the trail migration to completion through the
// writer, one step per transaction (tests and tools; the store's
// data-migration scheduler runs the same steps with pauses).
func (s *Store) migrateTrailsNow(ctx context.Context, budget int) (steps int, err error) {
	var cursor []byte
	for {
		if err := ctx.Err(); err != nil {
			return steps, err
		}
		done := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			next, d, err := trailMigrateStep(ctx, tx, cursor, budget)
			if err != nil {
				return err
			}
			cursor, done = next, d
			if done {
				return trailMigrateFinish(ctx, tx)
			}
			return nil
		})
		steps++
		if err != nil || done {
			return steps, err
		}
	}
}

// ------------------------------------------------------------- registrations

func init() {
	registerDataMigration(DataMigration{
		Name: "trail", Version: 1, Kinds: KindMain,
		Estimate: func(ctx context.Context, q Querier) (int64, int64, error) { return trailMigrateEstimate(ctx, q) },
		Step:     trailMigrateStep,
		Finish:   trailMigrateFinish,
	})
	// The logical positions table: exports, the export list and the parity
	// harness see the 1.0.0 rows in the 1.0.0 order.
	RegisterLogicalTable(LogicalTable{
		Name: "positions", Kinds: KindMain, Columns: TrailColumns, TimeCol: "t", Unit: 1,
		Scan: func(ctx context.Context, q Querier, span *Span, limit int64, fn func(row []any) error) error {
			from, to := int64(math.MinInt64), int64(0)
			if span != nil {
				from, to = span.From, span.To
				if to <= from {
					return nil
				}
			}
			return forEachTrailRowQ(ctx, q, from, to, int(min(limit, math.MaxInt32)), func(r TrailRow) error {
				return fn(r.Values())
			})
		},
		Count: func(ctx context.Context, q Querier) (int64, error) {
			n, ok, err := countPositions(ctx, q)
			if err != nil || ok {
				return n, err
			}
			// Legacy rows never counted: count them (the 1.0.0 cost, once).
			st, err := readTrailState(ctx, q)
			if err != nil {
				return 0, err
			}
			err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM positions`).Scan(&n)
			return n + st.Chunk + st.Tail, err
		},
		Active: func(ctx context.Context, q Querier) (bool, error) { return hasTableTx(ctx, q, "trail_chunk") },
	})
	// Seal the tail on the checkpointer's schedule (profile SealInterval)
	// and before every TRUNCATE checkpoint.
	registerPeriodic("trail-seal", func(p Profile) time.Duration { return p.SealInterval }, true,
		func(ctx context.Context, s *Store) error {
			_, err := s.SealTrails(ctx)
			return err
		})
	registerHourly("trail", trailHourly)
	registerPruner("trail", pruneTrails)
}

// kvTrailVerified: Unix time of the last VerifyCounts.
const kvTrailVerified = "trail.verified_at"

// trailHourly compacts the chunks of flights that ended more than an hour
// ago and, once a day, verifies the point counters.
func trailHourly(ctx context.Context, s *Store) error {
	if s.kind != KindMain {
		return nil
	}
	if _, err := s.CompactTrails(ctx); err != nil {
		return err
	}
	now := time.Now().Unix()
	if last, ok, err := kvInt(ctx, s.DB(), kvTrailVerified); err != nil {
		return err
	} else if ok && now-last < 86400 && now >= last {
		return nil
	}
	if _, err := s.VerifyCounts(ctx); err != nil {
		return err
	}
	return s.KVSet(ctx, kvTrailVerified, strconv.FormatInt(now, 10))
}

// kvTrailPruned: the trails of every flight that ended before this time
// have been deleted by the trail retention.
const kvTrailPruned = "trail.pruned_end"

// pruneTrails applies the 1.0.0 trail rules to every tier (legacy rows,
// chunks, tail): the trails of flights that the flight retention is about to
// delete, the trails of flights that ended before the trail retention, and
// the orphans below the oldest flight. It runs before the legacy pruning of
// maint.go, which then finds nothing left to delete.
func pruneTrails(ctx context.Context, s *Store, cfg model.StorageConfig, now int64, res *PruneResult) error {
	if s.kind != KindMain || !s.HasTable("trail_chunk") {
		return nil
	}
	cutoff := func(days int) int64 { return now - int64(days)*86400 }
	if cfg.FlightRetentionDays > 0 {
		n, err := s.deleteTrailsEndedBefore(ctx, cutoff(cfg.FlightRetentionDays), "")
		res.Positions += n
		if err != nil {
			return fmt.Errorf("store: prune trails of old flights: %w", err)
		}
	}
	if cfg.TrailRetentionDays > 0 {
		cut := cutoff(cfg.TrailRetentionDays)
		n, err := s.deleteTrailsEndedBefore(ctx, cut, kvTrailPruned)
		res.Positions += n
		if err != nil {
			return fmt.Errorf("store: prune trails: %w", err)
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			var k int64
			if err := s.Write(ctx, func(tx *sql.Tx) error {
				var err error
				k, err = pruneTrailOrphans(ctx, tx, cut)
				return err
			}); err != nil {
				return fmt.Errorf("store: prune orphan trails: %w", err)
			}
			res.Positions += k
			if k == 0 {
				break
			}
		}
	}
	return nil
}

// deleteTrailsEndedBefore deletes the trails of the flights that ended
// before cutoff, walking flights_end in small transactions; with a kv key
// it resumes from (and advances) a watermark, so each flight is visited
// once.
func (s *Store) deleteTrailsEndedBefore(ctx context.Context, cutoff int64, watermark string) (int64, error) {
	curEnd, curID := int64(math.MinInt64), int64(0)
	if watermark != "" {
		if w, ok, err := kvInt(ctx, s.DB(), watermark); err != nil {
			return 0, err
		} else if ok {
			curEnd, curID = w-1, math.MaxInt64
		}
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var deleted, nextEnd, nextID int64
		done := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			deleted, done, nextEnd, nextID = 0, false, curEnd, curID
			rows, err := tx.QueryContext(ctx, `SELECT id, "end" FROM flights
WHERE ("end", id) > (?, ?) AND "end" < ? ORDER BY "end", id LIMIT ?`, curEnd, curID, cutoff, pruneFlightChunk)
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
			if deleted, err = deleteTrails(ctx, tx, ids...); err != nil {
				return err
			}
			if watermark != "" && len(ids) > 0 && nextEnd > curEnd {
				return kvSetInt(ctx, tx, watermark, nextEnd)
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		total += deleted
		curEnd, curID = nextEnd, nextID
		if done {
			return total, nil
		}
	}
}
