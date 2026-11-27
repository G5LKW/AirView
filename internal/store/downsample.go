package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Tiered position trails (SO-6, DESIGN §20.2).
//
// The positions table is by far the largest table of a busy aggregator: one
// row per aircraft every storage.trailIntervalSec (5 s), about 0.6 GB per
// 1,000 aircraft and day. Trails therefore pass through three tiers:
//
//	tier      age of the flight's end          content
//	full      < TierPolicy.TrailFullDays       every stored point
//	thinned   TrailFullDays .. trail retention one point per TrailThinSec
//	                                           (the first point of every
//	                                           TrailThinSec bucket of Unix
//	                                           time) plus the flight's last
//	                                           point
//	summary   > trail retention                the flights row only (first /
//	                                           last position, extremes);
//	                                           positions deleted by Prune
//	                                           (storage.trailRetentionDays,
//	                                           aggregator.storage.trailDays)
//
// ThinTrails implements the middle tier; Maintain runs it on airview.db.
// The rule "first point of each bucket, plus the last point" is idempotent
// (thinning a thinned trail removes nothing), keeps the start and end of
// every flight exactly, and bounds the gap between two kept points by
// TrailThinSec + trailIntervalSec (for example 35 s).
//
// Error bound. Between two kept points a reader interpolates a straight
// line. On a straight leg the error is zero (up to the position noise). In a
// turn of radius r flown at speed v, two kept points dt apart lie on an arc
// of angle θ = v·dt / r; the arc's distance from its chord (the sagitta) is
// r·(1 - cos(θ/2)). With dt = 35 s:
//
//	standard-rate turn (3°/s) at 250 kt: r = 2.46 km, θ = 105° -> 0.96 km
//	25° bank at 450 kt (cruise):         r = 11.7 km, θ = 40°  -> 0.69 km
//
// so the thinned trail stays within about 1 km of the full one, which is
// below the line width of a map at the zoom levels a multi-day history is
// looked at. The worst case is a slow aircraft completing a whole circle
// within one bucket (θ ≥ 180°): the deviation is then at most the circle's
// diameter 2r (about 2 km for a standard-rate turn at 100 kt).
// TestThinTrailsSimnet measures it against simnet trajectories (461 flights
// with holds, approaches and departures, 90 minutes): distance of the full
// trail from the thinned polyline p50 1 m, p95 2 m, p99 195 m, max 1.1 km;
// 17 % of the points kept, no gap above 30 s.
//
// Size. With trailIntervalSec = 5 and TrailThinSec = 30 a thinned trail
// keeps 1/6 of its points (plus one per flight). At the default seven
// trail days and two full days the positions table holds 2 + 5 × 0.15
// ≈ 2.8 days of full-resolution bytes instead of 7 (-60 %).
// TestTrailTierGrowth (AIRVIEW_GROWTH=1) measures it on disk: 35 B per
// row, 0.61 GB per 1,000 aircraft and day at full resolution, thinned
// trails take 15 % of the bytes after VACUUM; steady state at 10,000
// aircraft 42.8 GB before, 16.9 GB after. ThinTrails deletes about 750,000
// rows/s (NVMe), so the 145 million rows a 10,000-aircraft day sheds take
// about 3 minutes of writer time per day, spread over the hourly runs.

// kvTrailsThinned: the trail of every flight that ended before this time
// (Unix s) has been thinned.
const kvTrailsThinned = "store.trails_thinned"

// Defaults of the trail tiers (TierPolicy.TrailFullDays / TrailThinSec).
const (
	DefaultTrailFullDays = 2
	DefaultTrailThinSec  = 30
)

// ThinTrails thins the stored trail of every flight that ended before
// cutoff (Unix s) to one point per thinSec plus its last point (see the
// comment at the top of this file). It walks the flights in order of their
// end time from a persistent watermark, so each flight is visited once and
// repeated calls are cheap; it works in small transactions and stops
// between two of them when ctx is cancelled. It returns the number of
// position rows deleted.
func (s *Store) ThinTrails(ctx context.Context, cutoff int64, thinSec int) (int64, error) {
	if thinSec <= 1 || !s.HasTable("positions") || !s.HasTable("flights") {
		return 0, nil
	}
	var curEnd, curID int64
	if w, ok, err := kvInt(ctx, s.DB(), kvTrailsThinned); err != nil {
		return 0, err
	} else if ok {
		// Flights that ended exactly at the watermark second are revisited
		// (thinning is idempotent).
		curEnd, curID = w-1, int64(^uint64(0)>>1)
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var deleted, nextEnd, nextID int64
		done := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			deleted, done = 0, false
			nextEnd, nextID = curEnd, curID
			rows, err := tx.QueryContext(ctx, `SELECT id, "end", positions FROM flights
WHERE ("end", id) > (?, ?) AND "end" < ? ORDER BY "end", id LIMIT ?`, curEnd, curID, cutoff, pruneFlightChunk)
			if err != nil {
				return err
			}
			type ref struct{ id, end, positions int64 }
			var refs []ref
			for rows.Next() {
				var f ref
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
			th, err := newTrailThinner(ctx, tx, thinSec)
			if err != nil {
				return err
			}
			defer th.close()
			for _, f := range refs {
				if f.positions > 0 {
					n, err := th.thin(f.id)
					if err != nil {
						return err
					}
					deleted += n
				}
				nextEnd, nextID = f.end, f.id
				if deleted >= prunePositionChunk {
					done = false
					break
				}
			}
			if err := th.finish(); err != nil {
				return err
			}
			if nextEnd > curEnd {
				return kvSetInt(ctx, tx, kvTrailsThinned, nextEnd)
			}
			return nil
		})
		if err != nil {
			return total, fmt.Errorf("store: thin trails: %w", err)
		}
		total += deleted
		curEnd, curID = nextEnd, nextID
		if done {
			return total, nil
		}
	}
}

// trailThinner thins the trails of single flights inside one transaction.
// A flight stored only in the legacy positions table is thinned in place
// with sqlThinTrail; a flight with sealed chunks (or tail rows) is merged,
// thinned with thinKeep (the same rule) and rewritten as compacted chunks.
type trailThinner struct {
	ctx     context.Context
	tx      *sql.Tx
	thinSec int64
	st      trailState
	del     *sql.Stmt
	w       *trailChunkWriter
}

func newTrailThinner(ctx context.Context, tx *sql.Tx, thinSec int) (*trailThinner, error) {
	st, err := readTrailState(ctx, tx)
	if err != nil {
		return nil, err
	}
	del, err := tx.PrepareContext(ctx, sqlThinTrail)
	if err != nil {
		return nil, err
	}
	w, err := newChunkWriter(ctx, tx)
	if err != nil {
		_ = del.Close()
		return nil, err
	}
	return &trailThinner{ctx: ctx, tx: tx, thinSec: int64(thinSec), st: st, del: del, w: w}, nil
}

func (th *trailThinner) close() {
	_ = th.del.Close()
	th.w.close()
}

// finish stores the counters.
func (th *trailThinner) finish() error { return writeTrailState(th.ctx, th.tx, th.st) }

// thin thins one flight and returns the points removed.
func (th *trailThinner) thin(id int64) (int64, error) {
	ctx, tx := th.ctx, th.tx
	cs, err := queryChunks(ctx, tx, `SELECT id, flight_id, t0, t1, n, data FROM trail_chunk WHERE flight_id = ? ORDER BY t0`, id)
	if err != nil {
		return 0, err
	}
	hasTail, err := flightMayHaveTail(ctx, tx, id, th.st)
	if err != nil {
		return 0, err
	}
	if len(cs) == 0 && !hasTail {
		r, err := th.del.ExecContext(ctx, id, th.thinSec)
		if err != nil {
			return 0, err
		}
		n, _ := r.RowsAffected()
		th.st.LegacyDel += n
		return n, nil
	}
	legacy, err := queryTrailRows(ctx, tx, nil, `SELECT `+trailCols+` FROM positions WHERE flight_id = ? ORDER BY t`, id)
	if err != nil {
		return 0, err
	}
	chunk, err := decodeChunks(nil, id, cs)
	if err != nil {
		return 0, err
	}
	var tail []TrailRow
	if hasTail {
		m := map[int64][]TrailRow{}
		if _, _, err := readTailRows(ctx, tx, m, `SELECT rowid, flight_id, t0, t1, n, data FROM trail_tail WHERE flight_id = ? ORDER BY rowid`, id); err != nil {
			return 0, err
		}
		tail = m[id]
	}
	pts := mergeTrail(legacy, chunk, tail)
	ts := make([]int64, len(pts))
	for i := range pts {
		ts[i] = pts[i].T
	}
	keep := thinKeep(ts, th.thinSec)
	kept := pts[:0:0]
	for i, k := range keep {
		if k {
			kept = append(kept, pts[i])
		}
	}
	if len(kept) == len(pts) {
		return 0, nil
	}
	if err := deleteChunks(ctx, tx, cs); err != nil {
		return 0, err
	}
	for _, c := range cs {
		th.st.Chunk -= c.n
	}
	if len(legacy) > 0 {
		r, err := tx.ExecContext(ctx, `DELETE FROM positions WHERE flight_id = ?`, id)
		if err != nil {
			return 0, err
		}
		n, _ := r.RowsAffected()
		th.st.LegacyDel += n
	}
	if len(tail) > 0 {
		var n sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT SUM(n) FROM trail_tail WHERE flight_id = ?`, id).Scan(&n); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM trail_tail WHERE flight_id = ?`, id); err != nil {
			return 0, err
		}
		th.st.Tail -= n.Int64
	}
	if err := th.w.write(id, kept, trailLevelCompact); err != nil {
		return 0, err
	}
	th.st.Chunk += int64(len(kept))
	return int64(len(pts) - len(kept)), nil
}

// sqlThinTrail deletes every point of flight ?1 except the first of each
// ?2-second bucket and the last one. The subqueries are range scans of the
// primary key (flight_id, t).
const sqlThinTrail = `DELETE FROM positions WHERE flight_id = ?1
  AND t NOT IN (SELECT MIN(t) FROM positions WHERE flight_id = ?1 GROUP BY t / ?2)
  AND t < (SELECT MAX(t) FROM positions WHERE flight_id = ?1)`

// thinKeep reports which of the sorted times ts ThinTrails keeps (the
// reference implementation of sqlThinTrail, for tests and estimates).
func thinKeep(ts []int64, thinSec int64) []bool {
	keep := make([]bool, len(ts))
	last := int64(-1 << 62)
	for i, t := range ts {
		b := floorDiv(t, thinSec)
		if b != last {
			keep[i] = true
			last = b
		}
	}
	if len(ts) > 0 {
		keep[len(ts)-1] = true
	}
	return keep
}

// thinTrailsStep is the trail step of Maintain.
func (s *Store) thinTrailsStep(ctx context.Context, p TierPolicy, now int64, rep *MaintReport) error {
	if p.TrailFullDays <= 0 || p.TrailThinSec <= 1 || s.kind != KindMain {
		return nil
	}
	start := time.Now()
	n, err := s.ThinTrails(ctx, now-int64(p.TrailFullDays)*86400, p.TrailThinSec)
	rep.ThinnedPositions += n
	if n > 0 {
		s.log.Debug("store: trails thinned", "rows", n, "took", time.Since(start).Round(time.Millisecond))
	}
	return err
}
