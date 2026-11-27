package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// Trail storage (DB-EFFICIENCY-DESIGN §3.5, §3.8, §3.14).
//
// 1.0.0 kept every trail point as one row of positions (WITHOUT ROWID,
// key (flight_id, t)): 38 B per point, and every 15-s flush re-appended the
// "open tail" page of each active flight to the WAL. 1.1 keeps trails in two
// tables (migration "trail" 1):
//
//   - trail_tail: recent points, append-only (rowid = arrival order, no
//     index), one row per flight and trail flush: the flush's points of that
//     flight as a varint-coded blob (trail_codec.go). The tracker's trail
//     stream appends here once a minute; 250 active flights land on a few
//     consecutive pages, and a minute of trail costs one row per flight
//     instead of one per point.
//   - trail_chunk: sealed, packed chunks of one flight each (trail_codec.go),
//     key (flight_id, t0), disjoint [t0, t1] ranges per flight.
//
// Sealing (every TrailSealInterval, from the writer, see appendTrailTail)
// moves the whole tail into chunks in one transaction: grouped by flight,
// de-duplicated by (flight_id, t) with the last row winning (the INSERT OR
// REPLACE semantic of 1.0.0), merged into an overlapping chunk when a point
// falls inside one, and verified by decoding before the tail rows go.
// Compaction rewrites the chunks of flights that ended more than an hour ago
// into 256-point deflate chunks. The legacy positions table stays readable:
// until the data migration "trail" 1 (trail_migrate.go) has moved every row,
// readers merge legacy rows, chunks and tail rows, with the precedence
// tail > chunk > legacy for equal (flight_id, t).
//
// Point counts are kept in kv (trail.state) inside the same transactions, so
// TableCounts never walks a trail (counts.go).

const trailSchemaV1 = `
CREATE TABLE trail_tail(
  flight_id INTEGER NOT NULL,
  t0        INTEGER NOT NULL,
  t1        INTEGER NOT NULL,
  n         INTEGER NOT NULL,
  data      BLOB NOT NULL
);
CREATE TABLE trail_chunk(
  id        INTEGER PRIMARY KEY,
  flight_id INTEGER NOT NULL,
  t0        INTEGER NOT NULL,
  t1        INTEGER NOT NULL,
  n         INTEGER NOT NULL,
  data      BLOB NOT NULL
);
CREATE UNIQUE INDEX trail_chunk_flight ON trail_chunk(flight_id, t0);
`

func init() {
	if err := RegisterMigrationFor(KindMain, "trail", 1, trailSchemaV1); err != nil {
		panic(err)
	}
}

// Timing of the trail tiers. The seal interval follows the storage profile
// (standard 5 min, low-wear 10 min); the engine sets it with
// SetTrailSealInterval.
var trailSealEvery atomic.Int64

func init() { trailSealEvery.Store(int64(5 * time.Minute)) }

// SetTrailSealInterval sets how often the tail is sealed into chunks
// (process-wide; <= 0 restores the default of 5 minutes).
func SetTrailSealInterval(d time.Duration) {
	if d <= 0 {
		d = 5 * time.Minute
	}
	trailSealEvery.Store(int64(d))
}

const (
	// trailCompactAfter: chunks of a flight are compacted once it ended
	// this long ago.
	trailCompactAfter = 3600
	// trailCompactEvery: how often trails are compacted (the hourly job).
	trailCompactEvery = time.Hour
	// trailWriterSealFactor: the writer seals by itself after this many
	// seal intervals without a seal.
	trailWriterSealFactor = 3
	// trailTailMargin: a flight whose stored end is this much older than
	// the oldest tail point has no tail rows (its row is refreshed every
	// minute while points arrive).
	trailTailMargin = 900
	// compaction budget of one writer transaction.
	trailCompactExamine = 500
	trailCompactRewrite = 64
)

// trailVerify makes every chunk write decode its blob and compare it with
// the points before anything is deleted (on by default; benchmarks may
// switch it off).
var trailVerify = true

// ------------------------------------------------------------------ state

const (
	kvTrailState     = "trail.state"
	kvTrailCompacted = "trail.compacted_end" // flights ended before this (s) are compacted
	kvTrailLegacy    = "trail.legacy_base"   // "rows deletedAtCount": legacy positions counted once
)

// trailState is the kv row trail.state: the point counters and the oldest
// tail point.
type trailState struct {
	Chunk     int64 // points in trail_chunk
	Tail      int64 // points in trail_tail
	LegacyDel int64 // legacy positions rows deleted by this package, cumulative
	TailMinT  int64 // smallest t in trail_tail (math.MaxInt64 when empty)
}

func (st trailState) String() string {
	return fmt.Sprintf("%d %d %d %d", st.Chunk, st.Tail, st.LegacyDel, st.TailMinT)
}

func parseTrailState(v string) (trailState, bool) {
	f := strings.Fields(v)
	if len(f) != 4 {
		return trailState{TailMinT: math.MaxInt64}, false
	}
	var n [4]int64
	for i := range f {
		x, err := strconv.ParseInt(f[i], 10, 64)
		if err != nil {
			return trailState{TailMinT: math.MaxInt64}, false
		}
		n[i] = x
	}
	return trailState{n[0], n[1], n[2], n[3]}, true
}

func kvString(ctx context.Context, q querier, key string) (string, bool, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

type trailExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func kvPut(ctx context.Context, x trailExecer, key, value string) error {
	_, err := x.ExecContext(ctx, sqlSetKV, key, value, time.Now().Unix())
	return err
}

// readTrailState reads trail.state. A database without the row (new, or
// migrated from 1.0.0) has empty chunk / tail tables: zero counters.
func readTrailState(ctx context.Context, q querier) (trailState, error) {
	v, ok, err := kvString(ctx, q, kvTrailState)
	if err != nil || !ok {
		return trailState{TailMinT: math.MaxInt64}, err
	}
	st, _ := parseTrailState(v)
	return st, nil
}

func writeTrailState(ctx context.Context, x trailExecer, st trailState) error {
	if st.Tail <= 0 {
		st.Tail, st.TailMinT = 0, math.MaxInt64
	}
	return kvPut(ctx, x, kvTrailState, st.String())
}

// ------------------------------------------------------------------ rows

const trailCols = `flight_id, t, lat_e5, lon_e5, alt, gs, track, rssi_x10, src`

// scanTrailRow scans the columns of trailCols.
func scanTrailRow(sc interface{ Scan(...any) error }, extra ...any) (TrailRow, error) {
	var r TrailRow
	var alt, gs, trk, rssi sql.NullInt64
	dest := append([]any{&r.FlightID, &r.T, &r.LatE5, &r.LonE5, &alt, &gs, &trk, &rssi, &r.Src}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return r, err
	}
	if alt.Valid {
		r.Alt, r.Has = alt.Int64, r.Has|TrailHasAlt
	}
	if gs.Valid {
		r.GS, r.Has = gs.Int64, r.Has|TrailHasGS
	}
	if trk.Valid {
		r.Track, r.Has = trk.Int64, r.Has|TrailHasTrack
	}
	if rssi.Valid {
		r.RSSIX10, r.Has = rssi.Int64, r.Has|TrailHasRSSI
	}
	return r, nil
}

func trailArgs(r *TrailRow) []any { return r.Values() }

func cmpTrailKey(a, b TrailRow) int {
	if c := cmp.Compare(a.FlightID, b.FlightID); c != 0 {
		return c
	}
	return cmp.Compare(a.T, b.T)
}

// dedupeLast sorts rows of one flight by t (stable) and keeps the last row
// of every t.
func dedupeLast(rows []TrailRow) []TrailRow {
	slices.SortStableFunc(rows, func(a, b TrailRow) int { return cmp.Compare(a.T, b.T) })
	out := rows[:0]
	for i := range rows {
		if len(out) > 0 && out[len(out)-1].T == rows[i].T {
			out[len(out)-1] = rows[i]
			continue
		}
		out = append(out, rows[i])
	}
	return out
}

// mergeByT merges two sorted, unique-t lists; b wins on equal t.
func mergeByT(dst, a, b []TrailRow) []TrailRow {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j == len(b) || (i < len(a) && a[i].T < b[j].T):
			dst = append(dst, a[i])
			i++
		case i == len(a) || b[j].T < a[i].T:
			dst = append(dst, b[j])
			j++
		default:
			dst = append(dst, b[j])
			i++
			j++
		}
	}
	return dst
}

// mergeTrail combines the stored layers of one flight: legacy and chunk
// lists sorted by t, tail rows in arrival order (modified). Precedence for
// equal t: tail > chunk > legacy.
func mergeTrail(legacy, chunk, tail []TrailRow) []TrailRow {
	base := chunk
	if len(legacy) > 0 {
		base = mergeByT(make([]TrailRow, 0, len(legacy)+len(chunk)), legacy, chunk)
	}
	if len(tail) == 0 {
		return base
	}
	tail = dedupeLast(tail)
	return mergeByT(make([]TrailRow, 0, len(base)+len(tail)), base, tail)
}

// ------------------------------------------------------------------ writes

const sqlInsertTail = `INSERT INTO trail_tail(flight_id, t0, t1, n, data) VALUES(?,?,?,?,?)`

// trailRuntime is the writer-side timing state of one database generation.
type trailRuntime struct {
	lastSeal    time.Time
	lastCompact time.Time
	compactBusy bool // the last compaction step had more work
}

var trailRTs sync.Map // *handles -> *trailRuntime (used by the writer goroutine only)

func trailRT(h *handles) *trailRuntime {
	if v, ok := trailRTs.Load(h); ok {
		return v.(*trailRuntime)
	}
	rt := &trailRuntime{}
	trailRTs.Store(h, rt)
	return rt
}

// appendTrailTail appends rows (sorted by (flight_id, t), stable) to the
// tail and seals / compacts when due. It runs inside the writer's
// transaction.
func appendTrailTail(tc *txc, rows []TrailRow) error {
	ctx, tx := tc.ctx, tc.tx
	st, err := readTrailState(ctx, tx)
	if err != nil {
		return err
	}
	ins, err := tc.stmt(sqlInsertTail)
	if err != nil {
		return err
	}
	// One row per flight: its points of this batch, de-duplicated (the last
	// of a repeated second wins, as with INSERT OR REPLACE), varint coded.
	var buf []byte
	for i := 0; i < len(rows); {
		j := i + 1
		for j < len(rows) && rows[j].FlightID == rows[i].FlightID {
			j++
		}
		pts := dedupeLast(slices.Clone(rows[i:j])) // the op may run again after a failed commit
		buf = encodeTrailChunk(buf[:0], pts, trailLevelNone)
		if _, err := ins.ExecContext(ctx, rows[i].FlightID, pts[0].T, pts[len(pts)-1].T, len(pts), buf); err != nil {
			return err
		}
		st.Tail += int64(len(pts))
		st.TailMinT = min(st.TailMinT, pts[0].T)
		i = j
	}
	// Normally the checkpointer seals on the profile's schedule
	// (registerPeriodic in trail_migrate.go) and the hourly job compacts;
	// the writer steps in when that has not happened for a while (no
	// checkpointer running: tools, tests), so the tail stays bounded.
	rt := trailRT(tc.h)
	now := time.Now()
	if rt.lastSeal.IsZero() {
		rt.lastSeal = now // the first seal is the checkpointer's
	}
	if now.Sub(rt.lastSeal) >= trailWriterSealFactor*time.Duration(trailSealEvery.Load()) {
		if st, err = sealTrailTx(ctx, tx, st); err != nil {
			return fmt.Errorf("seal: %w", err)
		}
		rt.lastSeal = now
		if rt.lastCompact.IsZero() {
			rt.lastCompact = now
		}
		if rt.compactBusy || now.Sub(rt.lastCompact) >= 2*trailCompactEvery {
			more, err := compactTrailsTx(ctx, tx, &st, now.Unix()-trailCompactAfter)
			if err != nil {
				return fmt.Errorf("compact: %w", err)
			}
			rt.compactBusy = more
			if !more {
				rt.lastCompact = now
			}
		}
	}
	return writeTrailState(ctx, tx, st)
}

// trailChunkWriter inserts chunks of one transaction.
type trailChunkWriter struct {
	ctx context.Context
	tx  *sql.Tx
	ins *sql.Stmt
	buf []byte
	dec []TrailRow
}

func newChunkWriter(ctx context.Context, tx *sql.Tx) (*trailChunkWriter, error) {
	ins, err := tx.PrepareContext(ctx, `INSERT INTO trail_chunk(flight_id, t0, t1, n, data) VALUES(?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	return &trailChunkWriter{ctx: ctx, tx: tx, ins: ins}, nil
}

func (w *trailChunkWriter) close() { _ = w.ins.Close() }

// write stores pts (one flight, sorted, unique t) as chunks of at most
// trailChunkMax points.
func (w *trailChunkWriter) write(fid int64, pts []TrailRow, level int) error {
	for len(pts) > 0 {
		k := min(len(pts), trailChunkMax)
		part := pts[:k]
		w.buf = encodeTrailChunk(w.buf[:0], part, level)
		if trailVerify {
			var err error
			w.dec, err = decodeTrailChunk(w.dec[:0], fid, w.buf)
			if err != nil || !slices.Equal(w.dec, part) {
				return fmt.Errorf("store: trail chunk of flight %d does not round-trip (%v)", fid, err)
			}
		}
		if _, err := w.ins.ExecContext(w.ctx, fid, part[0].T, part[k-1].T, k, w.buf); err != nil {
			return err
		}
		pts = pts[k:]
	}
	return nil
}

// chunkRef is one stored chunk.
type chunkRef struct {
	id, fid, t0, t1, n int64
	data               []byte
}

func queryChunks(ctx context.Context, q querier, query string, args ...any) ([]chunkRef, error) {
	rs, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []chunkRef
	for rs.Next() {
		var c chunkRef
		if err := rs.Scan(&c.id, &c.fid, &c.t0, &c.t1, &c.n, &c.data); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rs.Err()
}

func decodeChunks(dst []TrailRow, fid int64, cs []chunkRef) ([]TrailRow, error) {
	var err error
	for _, c := range cs {
		if dst, err = decodeTrailChunk(dst, fid, c.data); err != nil {
			return dst, fmt.Errorf("store: trail chunk %d of flight %d: %w", c.id, fid, err)
		}
	}
	return dst, nil
}

func deleteChunks(ctx context.Context, tx *sql.Tx, cs []chunkRef) error {
	for _, c := range cs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM trail_chunk WHERE id = ?`, c.id); err != nil {
			return err
		}
	}
	return nil
}

// tailBlob is one trail_tail row.
type tailBlob struct {
	rowid, fid, t0, t1, n int64
	data                  []byte
}

// readTailRows decodes the trail_tail rows a query returns (columns rowid,
// flight_id, t0, t1, n, data, in rowid order) and appends their points, in
// arrival order, per flight.
func readTailRows(ctx context.Context, q querier, into map[int64][]TrailRow, query string, args ...any) (maxRowid, rows int64, err error) {
	rs, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, 0, err
	}
	defer rs.Close()
	for rs.Next() {
		var b tailBlob
		var raw sql.RawBytes
		if err := rs.Scan(&b.rowid, &b.fid, &b.t0, &b.t1, &b.n, &raw); err != nil {
			return 0, 0, err
		}
		if into[b.fid], err = decodeTrailChunk(into[b.fid], b.fid, raw); err != nil {
			return 0, 0, fmt.Errorf("store: trail tail row %d of flight %d: %w", b.rowid, b.fid, err)
		}
		maxRowid = max(maxRowid, b.rowid)
		rows++
	}
	return maxRowid, rows, rs.Err()
}

// sealTrailTx moves the whole tail into chunks (see the file comment).
func sealTrailTx(ctx context.Context, tx *sql.Tx, st trailState) (trailState, error) {
	byFlight := map[int64][]TrailRow{}
	maxRowid, rowsRead, err := readTailRows(ctx, tx, byFlight,
		`SELECT rowid, flight_id, t0, t1, n, data FROM trail_tail ORDER BY rowid`)
	if err != nil {
		return st, err
	}
	var pointsRead int64
	for _, pts := range byFlight {
		pointsRead += int64(len(pts))
	}
	if rowsRead == 0 {
		st.Tail, st.TailMinT = 0, math.MaxInt64
		return st, nil
	}
	w, err := newChunkWriter(ctx, tx)
	if err != nil {
		return st, err
	}
	defer w.close()
	fids := make([]int64, 0, len(byFlight))
	for fid := range byFlight {
		fids = append(fids, fid)
	}
	slices.Sort(fids)
	// While 1.0.0 rows remain (data migration "trail" 1 not finished), a
	// sealed point replaces a legacy row of the same (flight_id, t): delete
	// it, so legacy rows and chunks never hold the same point.
	var legacyDel *sql.Stmt
	var one int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM positions LIMIT 1`).Scan(&one); {
	case err == nil:
		if legacyDel, err = tx.PrepareContext(ctx, `DELETE FROM positions WHERE flight_id = ? AND t = ?`); err != nil {
			return st, err
		}
		defer legacyDel.Close()
	case !errors.Is(err, sql.ErrNoRows):
		return st, err
	}
	var old []TrailRow
	for _, fid := range fids {
		pts := dedupeLast(byFlight[fid])
		a, b := pts[0].T, pts[len(pts)-1].T
		if legacyDel != nil {
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM positions WHERE flight_id = ? AND t >= ? AND t <= ? LIMIT 1`, fid, a, b).Scan(&one)
			if err == nil {
				for i := range pts {
					r, err := legacyDel.ExecContext(ctx, fid, pts[i].T)
					if err != nil {
						return st, err
					}
					n, _ := r.RowsAffected()
					st.LegacyDel += n
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return st, err
			}
		}
		over, err := queryChunks(ctx, tx, `SELECT id, flight_id, t0, t1, n, data FROM trail_chunk
WHERE flight_id = ? AND t0 <= ? AND t1 >= ? ORDER BY t0`, fid, b, a)
		if err != nil {
			return st, err
		}
		merged := pts
		var oldN int64
		if len(over) > 0 {
			if old, err = decodeChunks(old[:0], fid, over); err != nil {
				return st, err
			}
			for _, c := range over {
				oldN += c.n
			}
			merged = mergeByT(make([]TrailRow, 0, len(old)+len(pts)), old, pts)
			if err := deleteChunks(ctx, tx, over); err != nil {
				return st, err
			}
		}
		if err := w.write(fid, merged, trailLevelSeal); err != nil {
			return st, err
		}
		st.Chunk += int64(len(merged)) - oldN
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trail_tail WHERE rowid <= ?`, maxRowid); err != nil {
		return st, err
	}
	st.Tail -= pointsRead
	if st.Tail < 0 {
		st.Tail = 0
	}
	// The writer appends and seals in the same goroutine: the tail is empty.
	st.TailMinT = math.MaxInt64
	return st, nil
}

// SealTrails seals the trail tail now (normally the writer does this every
// TrailSealInterval while points arrive) and returns the points sealed.
func (s *Store) SealTrails(ctx context.Context) (int64, error) {
	if s.kind != KindMain {
		return 0, nil
	}
	// Nothing to seal: no write transaction at all (this also runs before
	// every TRUNCATE checkpoint).
	var one int
	switch err := s.DB().QueryRowContext(ctx, `SELECT 1 FROM trail_tail LIMIT 1`).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return 0, nil
	case err != nil:
		return 0, err
	}
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		st, err := readTrailState(ctx, tx)
		if err != nil {
			return err
		}
		before := st.Tail
		if st, err = sealTrailTx(ctx, tx, st); err != nil {
			return err
		}
		n = before - st.Tail
		trailRT(s.cur.Load()).lastSeal = time.Now() // on the writer goroutine
		return writeTrailState(ctx, tx, st)
	})
	return n, err
}

// compactTrailsTx rewrites the chunks of flights that ended before cutoff
// into trailChunkMax-point deflate chunks, walking the flights by end time
// from the watermark trail.compacted_end. It does a bounded amount of work
// and reports whether more is left.
func compactTrailsTx(ctx context.Context, tx *sql.Tx, st *trailState, cutoff int64) (more bool, err error) {
	var curEnd, curID int64 = math.MinInt64, 0
	if v, ok, err := kvString(ctx, tx, kvTrailCompacted); err != nil {
		return false, err
	} else if ok {
		if w, err := strconv.ParseInt(v, 10, 64); err == nil {
			curEnd, curID = w-1, math.MaxInt64
		}
	}
	rs, err := tx.QueryContext(ctx, `SELECT id, "end" FROM flights WHERE ("end", id) > (?, ?) AND "end" < ?
ORDER BY "end", id LIMIT ?`, curEnd, curID, cutoff, trailCompactExamine)
	if err != nil {
		return false, err
	}
	type ref struct{ id, end int64 }
	var refs []ref
	for rs.Next() {
		var f ref
		if err := rs.Scan(&f.id, &f.end); err != nil {
			rs.Close()
			return false, err
		}
		refs = append(refs, f)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return false, err
	}
	more = len(refs) == trailCompactExamine
	w, err := newChunkWriter(ctx, tx)
	if err != nil {
		return false, err
	}
	defer w.close()
	rewritten := 0
	lastEnd := curEnd
	var pts []TrailRow
	for _, f := range refs {
		cs, err := queryChunks(ctx, tx, `SELECT id, flight_id, t0, t1, n, NULL FROM trail_chunk WHERE flight_id = ? ORDER BY t0`, f.id)
		if err != nil {
			return false, err
		}
		if needsCompaction(cs) {
			cs, err = queryChunks(ctx, tx, `SELECT id, flight_id, t0, t1, n, data FROM trail_chunk WHERE flight_id = ? ORDER BY t0`, f.id)
			if err != nil {
				return false, err
			}
			if pts, err = decodeChunks(pts[:0], f.id, cs); err != nil {
				return false, err
			}
			if err := deleteChunks(ctx, tx, cs); err != nil {
				return false, err
			}
			if err := w.write(f.id, pts, trailLevelCompact); err != nil {
				return false, err
			}
			rewritten++
		}
		lastEnd = f.end
		if rewritten >= trailCompactRewrite {
			more = true
			break
		}
	}
	if lastEnd > curEnd && lastEnd != math.MinInt64 {
		if err := kvPut(ctx, tx, kvTrailCompacted, strconv.FormatInt(lastEnd, 10)); err != nil {
			return false, err
		}
	}
	return more, nil
}

// needsCompaction: anything but the 256-point layout (all chunks full but
// the last) is rewritten; a flight already in that layout is left alone.
func needsCompaction(cs []chunkRef) bool {
	if len(cs) == 0 {
		return false
	}
	if len(cs) == 1 {
		return false // one chunk: nothing to merge (re-encoding gains < 5 %)
	}
	for i, c := range cs {
		if i < len(cs)-1 && c.n != trailChunkMax {
			return true
		}
	}
	return false
}

// CompactTrails runs trail compaction until it is caught up (for the
// hourly job and tools) and returns the number of writer transactions used.
func (s *Store) CompactTrails(ctx context.Context) (int, error) {
	if s.kind != KindMain {
		return 0, nil
	}
	steps := 0
	for {
		if err := ctx.Err(); err != nil {
			return steps, err
		}
		more := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			st, err := readTrailState(ctx, tx)
			if err != nil {
				return err
			}
			if more, err = compactTrailsTx(ctx, tx, &st, time.Now().Unix()-trailCompactAfter); err != nil {
				return err
			}
			if !more {
				rt := trailRT(s.cur.Load())
				rt.lastCompact, rt.compactBusy = time.Now(), false
			}
			return writeTrailState(ctx, tx, st)
		})
		steps++
		if err != nil || !more {
			return steps, err
		}
	}
}

// ------------------------------------------------------------------ deletes

// deleteTrails removes every stored point (legacy rows, chunks, tail rows)
// of the given flights inside tx and returns how many points went. It keeps
// the counters exact. For Prune / pruneFlights (maint.go).
func deleteTrails(ctx context.Context, tx *sql.Tx, ids ...int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	st, err := readTrailState(ctx, tx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, id := range ids {
		r, err := tx.ExecContext(ctx, `DELETE FROM positions WHERE flight_id = ?`, id)
		if err != nil {
			return total, err
		}
		n, _ := r.RowsAffected()
		st.LegacyDel += n
		total += n

		var pts sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT SUM(n) FROM trail_chunk WHERE flight_id = ?`, id).Scan(&pts); err != nil {
			return total, err
		}
		if pts.Int64 > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM trail_chunk WHERE flight_id = ?`, id); err != nil {
				return total, err
			}
			st.Chunk -= pts.Int64
			total += pts.Int64
		}
		if st.Tail > 0 {
			has, err := flightMayHaveTail(ctx, tx, id, st)
			if err != nil {
				return total, err
			}
			if has {
				var n sql.NullInt64
				if err := tx.QueryRowContext(ctx, `SELECT SUM(n) FROM trail_tail WHERE flight_id = ?`, id).Scan(&n); err != nil {
					return total, err
				}
				if n.Int64 > 0 {
					if _, err := tx.ExecContext(ctx, `DELETE FROM trail_tail WHERE flight_id = ?`, id); err != nil {
						return total, err
					}
					st.Tail -= n.Int64
					total += n.Int64
				}
			}
		}
	}
	return total, writeTrailState(ctx, tx, st)
}

// flightMayHaveTail reports whether the tail can hold points of flight id.
func flightMayHaveTail(ctx context.Context, q querier, id int64, st trailState) (bool, error) {
	if st.Tail <= 0 || st.TailMinT == math.MaxInt64 {
		return false, nil
	}
	var end int64
	err := q.QueryRowContext(ctx, `SELECT "end" FROM flights WHERE id = ?`, id).Scan(&end)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil // row not written yet: an active flight
	}
	if err != nil {
		return false, err
	}
	return end+trailTailMargin >= st.TailMinT, nil
}

// pruneTrailOrphans deletes the points older than cutoff of flights below
// the oldest stored flight (their row never reached the database or was
// deleted outside Prune), at most a bounded amount per call; it returns the
// points deleted (0 = done). The 1.0.0 rule of prunePositions.
func pruneTrailOrphans(ctx context.Context, tx *sql.Tx, cutoff int64) (int64, error) {
	st, err := readTrailState(ctx, tx)
	if err != nil {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, `DELETE FROM positions WHERE flight_id IN (
  SELECT DISTINCT flight_id FROM positions
  WHERE flight_id < (SELECT MIN(id) FROM flights) AND t < ?1 LIMIT 100) AND t < ?1`, cutoff)
	if err != nil {
		return 0, err
	}
	total, _ := r.RowsAffected()
	st.LegacyDel += total

	cs, err := queryChunks(ctx, tx, `SELECT id, flight_id, t0, t1, n, data FROM trail_chunk
WHERE flight_id < (SELECT MIN(id) FROM flights) AND t0 < ? LIMIT 100`, cutoff)
	if err != nil {
		return total, err
	}
	if len(cs) > 0 {
		w, err := newChunkWriter(ctx, tx)
		if err != nil {
			return total, err
		}
		defer w.close()
		var pts []TrailRow
		for _, c := range cs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM trail_chunk WHERE id = ?`, c.id); err != nil {
				return total, err
			}
			st.Chunk -= c.n
			if c.t1 < cutoff {
				total += c.n
				continue
			}
			if pts, err = decodeTrailChunk(pts[:0], c.fid, c.data); err != nil {
				return total, err
			}
			keep := pts[:0]
			for _, p := range pts {
				if p.T >= cutoff {
					keep = append(keep, p)
				}
			}
			total += c.n - int64(len(keep))
			st.Chunk += int64(len(keep))
			if err := w.write(c.fid, keep, trailLevelCompact); err != nil {
				return total, err
			}
		}
	}
	if st.Tail > 0 && st.TailMinT < cutoff {
		// Tail rows of orphans with points before cutoff (practically never:
		// the tail holds minutes): drop those points, keep the rest.
		rs, err := tx.QueryContext(ctx, `SELECT rowid, flight_id, n, data FROM trail_tail
WHERE flight_id < (SELECT MIN(id) FROM flights) AND t0 < ? LIMIT 100`, cutoff)
		if err != nil {
			return total, err
		}
		var bs []tailBlob
		for rs.Next() {
			var b tailBlob
			if err := rs.Scan(&b.rowid, &b.fid, &b.n, &b.data); err != nil {
				rs.Close()
				return total, err
			}
			bs = append(bs, b)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return total, err
		}
		var pts []TrailRow
		for _, b := range bs {
			if pts, err = decodeTrailChunk(pts[:0], b.fid, b.data); err != nil {
				return total, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM trail_tail WHERE rowid = ?`, b.rowid); err != nil {
				return total, err
			}
			keep := clipT(pts, cutoff, math.MaxInt64)
			total += b.n - int64(len(keep))
			st.Tail -= b.n - int64(len(keep))
			if len(keep) > 0 {
				if _, err := tx.ExecContext(ctx, sqlInsertTail, b.fid, keep[0].T, keep[len(keep)-1].T, len(keep),
					encodeTrailChunk(nil, keep, trailLevelNone)); err != nil {
					return total, err
				}
			}
		}
	}
	return total, writeTrailState(ctx, tx, st)
}

// maxStoredFlightID returns the highest flight ID that reached the
// database: flights rows, legacy positions, chunks or tail rows (Open
// continues NextFlightID after it).
func maxStoredFlightID(ctx context.Context, q querier) (int64, error) {
	var best int64
	for _, query := range []string{
		`SELECT MAX(id) FROM flights`,
		`SELECT MAX(flight_id) FROM positions`,
		`SELECT MAX(flight_id) FROM trail_chunk`,
		`SELECT MAX(flight_id) FROM trail_tail`,
	} {
		var v sql.NullInt64
		if err := q.QueryRowContext(ctx, query).Scan(&v); err != nil {
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return 0, err
		}
		best = max(best, v.Int64)
	}
	return best, nil
}

// ------------------------------------------------------------------ reads

// trailSnap is one consistent read of the trail tiers: a read transaction
// on the store's pool, or the caller's connection / transaction (tools and
// the logical-table scan of the parity harness).
type trailSnap struct {
	ctx    context.Context
	tx     querier
	end    func()
	st     trailState
	legacy bool // the legacy positions table has rows
}

func (s *Store) beginTrailSnap(ctx context.Context) (*trailSnap, error) {
	return beginTrailSnapQ(ctx, s.DB())
}

// beginTrailSnapQ starts a snapshot on q: a *sql.DB gets a read
// transaction of its own, anything else (a *sql.Tx, a *sql.Conn) is used as
// it is.
func beginTrailSnapQ(ctx context.Context, q querier) (*trailSnap, error) {
	sn := &trailSnap{ctx: ctx, tx: q, end: func() {}}
	if db, ok := q.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		sn.tx, sn.end = tx, func() { _ = tx.Rollback() }
	}
	var err error
	if sn.st, err = readTrailState(ctx, sn.tx); err != nil {
		sn.end()
		return nil, err
	}
	var one int
	err = sn.tx.QueryRowContext(ctx, `SELECT 1 FROM positions LIMIT 1`).Scan(&one)
	switch {
	case err == nil:
		sn.legacy = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		sn.end()
		return nil, err
	}
	return sn, nil
}

func (sn *trailSnap) close() { sn.end() }

func queryTrailRows(ctx context.Context, q querier, dst []TrailRow, query string, args ...any) ([]TrailRow, error) {
	rs, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return dst, err
	}
	defer rs.Close()
	for rs.Next() {
		r, err := scanTrailRow(rs)
		if err != nil {
			return dst, err
		}
		dst = append(dst, r)
	}
	return dst, rs.Err()
}

// flight returns the merged trail of one flight with from <= t <= to.
func (sn *trailSnap) flight(fid, from, to int64) ([]TrailRow, error) {
	ctx, tx := sn.ctx, sn.tx
	var legacy, chunk, tail []TrailRow
	var err error
	if sn.legacy {
		if legacy, err = queryTrailRows(ctx, tx, nil, `SELECT `+trailCols+` FROM positions
WHERE flight_id = ? AND t >= ? AND t <= ? ORDER BY t`, fid, from, to); err != nil {
			return nil, err
		}
	}
	cs, err := queryChunks(ctx, tx, `SELECT id, flight_id, t0, t1, n, data FROM trail_chunk
WHERE flight_id = ? AND t0 <= ? AND t1 >= ? ORDER BY t0`, fid, to, from)
	if err != nil {
		return nil, err
	}
	if len(cs) > 0 {
		n := 0
		for _, c := range cs {
			n += int(c.n)
		}
		if chunk, err = decodeChunks(make([]TrailRow, 0, n), fid, cs); err != nil {
			return nil, err
		}
		chunk = clipT(chunk, from, to)
	}
	if sn.st.Tail > 0 && sn.st.TailMinT <= to {
		has, err := flightMayHaveTail(ctx, tx, fid, sn.st)
		if err != nil {
			return nil, err
		}
		if has {
			m := map[int64][]TrailRow{}
			if _, _, err := readTailRows(ctx, tx, m, `SELECT rowid, flight_id, t0, t1, n, data FROM trail_tail
WHERE flight_id = ? AND t1 >= ? AND t0 <= ? ORDER BY rowid`, fid, from, to); err != nil {
				return nil, err
			}
			tail = clipUnsorted(m[fid], from, to)
		}
	}
	return mergeTrail(legacy, chunk, tail), nil
}

// clipT keeps the points of a sorted list with from <= t <= to.
func clipT(pts []TrailRow, from, to int64) []TrailRow {
	i, _ := slices.BinarySearchFunc(pts, from, func(p TrailRow, t int64) int { return cmp.Compare(p.T, t) })
	j, found := slices.BinarySearchFunc(pts, to, func(p TrailRow, t int64) int { return cmp.Compare(p.T, t) })
	if found {
		j++
	}
	return pts[i:j]
}

// clipUnsorted keeps the points (in arrival order) with from <= t <= to.
func clipUnsorted(pts []TrailRow, from, to int64) []TrailRow {
	out := pts[:0]
	for _, p := range pts {
		if p.T >= from && p.T <= to {
			out = append(out, p)
		}
	}
	return out
}

// tailByFlight reads the tail rows with from <= t <= to, per flight in
// arrival order (nil when the tail cannot hold any).
func (sn *trailSnap) tailByFlight(from, to int64) (map[int64][]TrailRow, error) {
	if sn.st.Tail <= 0 || sn.st.TailMinT > to {
		return nil, nil
	}
	out := map[int64][]TrailRow{}
	if _, _, err := readTailRows(sn.ctx, sn.tx, out, `SELECT rowid, flight_id, t0, t1, n, data FROM trail_tail
WHERE t1 >= ? AND t0 <= ? ORDER BY rowid`, from, to); err != nil {
		return nil, err
	}
	for fid, pts := range out {
		out[fid] = clipUnsorted(pts, from, to)
	}
	return out, nil
}

// trailRows returns the stored trail of a flight with from <= t <= to (to
// <= 0 = open) in the stored form.
func (s *Store) trailRows(ctx context.Context, flightID, from, to int64) ([]TrailRow, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	sn, err := s.beginTrailSnap(ctx)
	if err != nil {
		return nil, err
	}
	defer sn.close()
	return sn.flight(flightID, from, to)
}

// Positions returns the stored trail of a flight in time order, optionally
// limited to [from, to] (Unix seconds; 0 = open end).
func (s *Store) Positions(ctx context.Context, flightID, from, to int64) ([]Position, error) {
	rows, err := s.trailRows(ctx, flightID, from, to)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]Position, len(rows))
	for i := range rows {
		out[i] = rows[i].Position()
	}
	return out, nil
}

// Trail returns the stored trail of a flight as API trail points (Unix ms).
func (s *Store) Trail(ctx context.Context, flightID int64) ([]model.TrailPoint, error) {
	rows, err := s.trailRows(ctx, flightID, 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.TrailPoint, len(rows))
	for i := range rows {
		out[i] = rows[i].Position().TrailPoint()
	}
	return out, nil
}

// forEachFlightTrail calls fn with the merged points (from <= t <= to) of
// every flight with "end" >= from and start <= to, flights in order of
// their end time (then id), as ForEachPosition always did.
func (s *Store) forEachFlightTrail(ctx context.Context, from, to int64, fn func(fid int64, icao uint32, pts []TrailRow) error) error {
	sn, err := s.beginTrailSnap(ctx)
	if err != nil {
		return err
	}
	defer sn.close()
	tx := sn.tx

	type fl struct {
		id   int64
		icao uint32
	}
	var flights []fl
	rs, err := tx.QueryContext(ctx, `SELECT id, icao FROM flights WHERE "end" >= ?1 AND start <= ?2 ORDER BY "end", id`, from, to)
	if err != nil {
		return err
	}
	for rs.Next() {
		var f fl
		var icao int64
		if err := rs.Scan(&f.id, &icao); err != nil {
			rs.Close()
			return err
		}
		f.icao = uint32(icao)
		flights = append(flights, f)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	if len(flights) == 0 {
		return nil
	}
	tail, err := sn.tailByFlight(from, to)
	if err != nil {
		return err
	}

	// One pass over the chunks of all those flights, in the same order.
	crs, err := tx.QueryContext(ctx, `SELECT c.flight_id, c.data FROM flights f JOIN trail_chunk c ON c.flight_id = f.id
WHERE f."end" >= ?1 AND f.start <= ?2 AND c.t0 <= ?2 AND c.t1 >= ?1
ORDER BY f."end", f.id, c.t0`, from, to)
	if err != nil {
		return err
	}
	defer crs.Close()
	var pendFID int64
	var pendData []byte
	havePend := false
	nextChunk := func() error {
		havePend = false
		if crs.Next() {
			pendData = pendData[:0]
			var raw sql.RawBytes
			if err := crs.Scan(&pendFID, &raw); err != nil {
				return err
			}
			pendData = append(pendData, raw...)
			havePend = true
			return nil
		}
		return crs.Err()
	}
	if err := nextChunk(); err != nil {
		return err
	}
	// While 1.0.0 rows remain: one pass over them too, in the same order
	// (the 1.0.0 ForEachPosition query).
	var lrs *sql.Rows
	if sn.legacy {
		if lrs, err = tx.QueryContext(ctx, `SELECT p.flight_id, p.t, p.lat_e5, p.lon_e5, p.alt, p.gs, p.track, p.rssi_x10, p.src
FROM flights f JOIN positions p ON p.flight_id = f.id
WHERE f."end" >= ?1 AND f.start <= ?2 AND p.t >= ?1 AND p.t <= ?2
ORDER BY f."end", f.id, p.t`, from, to); err != nil {
			return err
		}
		defer lrs.Close()
	}
	var lRow TrailRow
	lOK := false
	nextL := func() error {
		lOK = false
		if lrs == nil {
			return nil
		}
		if lrs.Next() {
			r, err := scanTrailRow(lrs)
			if err != nil {
				return err
			}
			lRow, lOK = r, true
			return nil
		}
		return lrs.Err()
	}
	if err := nextL(); err != nil {
		return err
	}
	var chunk, legacy []TrailRow
	for _, f := range flights {
		chunk = chunk[:0]
		for havePend && pendFID == f.id {
			if chunk, err = decodeTrailChunk(chunk, f.id, pendData); err != nil {
				return fmt.Errorf("store: trail chunk of flight %d: %w", f.id, err)
			}
			if err := nextChunk(); err != nil {
				return err
			}
		}
		legacy = legacy[:0]
		for lOK && lRow.FlightID == f.id {
			legacy = append(legacy, lRow)
			if err := nextL(); err != nil {
				return err
			}
		}
		pts := mergeTrail(legacy, clipT(chunk, from, to), tail[f.id])
		if len(pts) == 0 {
			continue
		}
		if err := fn(f.id, f.icao, pts); err != nil {
			return err
		}
	}
	return nil
}

// ForEachPosition calls fn for every stored position with from <= t <= to
// (Unix seconds), grouped by flight (flights in order of their end time,
// positions of a flight in time order), together with the flight's ICAO
// address. Returning an error from fn stops the iteration.
func (s *Store) ForEachPosition(ctx context.Context, from, to int64, fn func(icao uint32, p Position) error) error {
	return s.forEachFlightTrail(ctx, from, to, func(_ int64, icao uint32, pts []TrailRow) error {
		for i := range pts {
			if err := fn(icao, pts[i].Position()); err != nil {
				return err
			}
		}
		return nil
	})
}

// StepStats is the result of ForEachPositionStep.
type StepStats struct {
	Read int // stored points in [from-stale, to] of the flights visited
	Used int // points handed to fn
}

// ForEachPositionStep is ForEachPosition for a replay with frames at from,
// from+step, ... <= to that show, per flight, the newest point at or before
// the frame time if it is at most stale seconds old. It reads the points
// with from-stale <= t <= to and hands fn only the points a frame uses plus
// the first point of every flight (which fixes the flight's position in the
// aircraft list and its first frame), so a frame built from what fn
// receives equals the frame built from every point. Flights come in the
// order of ForEachPosition. Returning an error from fn stops the iteration.
func (s *Store) ForEachPositionStep(ctx context.Context, from, to, step, stale int64, fn func(icao uint32, p Position) error) (StepStats, error) {
	var stats StepStats
	if step <= 0 {
		step = 1
	}
	var used []bool
	err := s.forEachFlightTrail(ctx, from-stale, to, func(_ int64, icao uint32, pts []TrailRow) error {
		stats.Read += len(pts)
		used = append(used[:0], make([]bool, len(pts))...)
		used[0] = true
		// First frame at or after the first point.
		k := int64(0)
		if pts[0].T > from {
			k = (pts[0].T - from + step - 1) / step
		}
		last := pts[len(pts)-1].T + stale
		j := 0
		for ft := from + k*step; ft <= to && ft <= last; ft += step {
			for j+1 < len(pts) && pts[j+1].T <= ft {
				j++
			}
			if pts[j].T <= ft && ft-pts[j].T <= stale {
				used[j] = true
			}
		}
		for i := range pts {
			if !used[i] {
				continue
			}
			stats.Used++
			if err := fn(icao, pts[i].Position()); err != nil {
				return err
			}
		}
		return nil
	})
	return stats, err
}

// ForEachTrailRow calls fn for every stored trail point with from <= t < to
// (to <= 0 = no upper bound) in the order of the 1.0.0 positions table
// (flight_id, t), in stored form, for at most limit rows (<= 0 = all). It
// is the logical positions table of exports and the parity harness.
func (s *Store) ForEachTrailRow(ctx context.Context, from, to int64, limit int, fn func(r TrailRow) error) error {
	return forEachTrailRowQ(ctx, s.DB(), from, to, limit, fn)
}

// forEachTrailRowQ is ForEachTrailRow on any connection (see
// beginTrailSnapQ).
func forEachTrailRowQ(ctx context.Context, q querier, from, to int64, limit int, fn func(r TrailRow) error) error {
	if to <= 0 {
		to = math.MaxInt64
	}
	hi := to - 1 // inclusive bound
	sn, err := beginTrailSnapQ(ctx, q)
	if err != nil {
		return err
	}
	defer sn.close()
	tx := sn.tx

	// Tail rows of the range, by flight.
	tailMap, err := sn.tailByFlight(from, hi)
	if err != nil {
		return err
	}
	tailIDs := make([]int64, 0, len(tailMap))
	for id := range tailMap {
		tailIDs = append(tailIDs, id)
	}
	slices.Sort(tailIDs)

	// Two ordered streams: chunks and legacy rows.
	crs, err := tx.QueryContext(ctx, `SELECT flight_id, data FROM trail_chunk WHERE t0 <= ? AND t1 >= ? ORDER BY flight_id, t0`, hi, from)
	if err != nil {
		return err
	}
	defer crs.Close()
	var lrs *sql.Rows
	if sn.legacy {
		if lrs, err = tx.QueryContext(ctx, `SELECT `+trailCols+` FROM positions WHERE t >= ? AND t <= ? ORDER BY flight_id, t`, from, hi); err != nil {
			return err
		}
		defer lrs.Close()
	}
	var cFID int64
	var cData []byte
	cOK := false
	nextC := func() error {
		cOK = false
		if crs.Next() {
			var raw sql.RawBytes
			if err := crs.Scan(&cFID, &raw); err != nil {
				return err
			}
			cData = append(cData[:0], raw...)
			cOK = true
			return nil
		}
		return crs.Err()
	}
	var lRow TrailRow
	lOK := false
	nextL := func() error {
		lOK = false
		if lrs != nil && lrs.Next() {
			r, err := scanTrailRow(lrs)
			if err != nil {
				return err
			}
			lRow, lOK = r, true
			return nil
		}
		if lrs != nil {
			return lrs.Err()
		}
		return nil
	}
	if err := nextC(); err != nil {
		return err
	}
	if err := nextL(); err != nil {
		return err
	}
	ti := 0
	emitted := 0
	var chunk, legacy []TrailRow
	for cOK || lOK || ti < len(tailIDs) {
		fid := int64(math.MaxInt64)
		if cOK {
			fid = min(fid, cFID)
		}
		if lOK {
			fid = min(fid, lRow.FlightID)
		}
		if ti < len(tailIDs) {
			fid = min(fid, tailIDs[ti])
		}
		chunk = chunk[:0]
		for cOK && cFID == fid {
			if chunk, err = decodeTrailChunk(chunk, fid, cData); err != nil {
				return fmt.Errorf("store: trail chunk of flight %d: %w", fid, err)
			}
			if err := nextC(); err != nil {
				return err
			}
		}
		legacy = legacy[:0]
		for lOK && lRow.FlightID == fid {
			legacy = append(legacy, lRow)
			if err := nextL(); err != nil {
				return err
			}
		}
		var tail []TrailRow
		if ti < len(tailIDs) && tailIDs[ti] == fid {
			tail = tailMap[fid]
			ti++
		}
		for _, r := range mergeTrail(legacy, clipT(chunk, from, hi), tail) {
			if err := fn(r); err != nil {
				return err
			}
			emitted++
			if limit > 0 && emitted >= limit {
				return nil
			}
		}
	}
	return nil
}

// openReadOnly opens an SQLite file read-only (tests and tools).
func openReadOnly(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	return sql.Open("sqlite", "file:"+strings.ReplaceAll(path, `\`, `/`)+"?"+q.Encode())
}
