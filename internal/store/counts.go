package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Table row counts (DB-EFFICIENCY-DESIGN §3.13).
//
// SQLite has no stored row count: COUNT(*) walks a whole B-tree, which took
// 16.7 s on a cold 7-day positions table (1.0.0) and 25-110 s on an SD card.
// TableCounts therefore reads exact counters for the big logical tables -
// positions = points in trail chunks + tail rows + legacy positions rows,
// all kept in kv (trail.state) by the transactions that change them - and
// runs COUNT(*) only for the other (small) tables, with the 1.0.0 cache
// policy on top. Other packages register counters for their big tables with
// registerTableCounter. VerifyCounts recounts from the tables once a day and
// corrects drift.

// Row-count cache policy for the COUNT(*) part.
const (
	// countCacheTTL is the longest a count is served from the cache when no
	// write happened in between.
	countCacheTTL = 30 * time.Second
	// countCheapLimit: a count that took at most this long is simply repeated
	// after a write; a slower one is kept until its (longer) lifetime ends.
	countCheapLimit = 150 * time.Millisecond
	// countCostFactor: a slow count is reused for countCostFactor times as
	// long as it took, so counting never uses more than ~2% of one core.
	countCostFactor = 50
)

// countCache is the state behind TableCounts.
type countCache struct {
	mu    sync.Mutex // guards the fields below; never held while counting
	vals  map[string]int64
	at    time.Time     // when vals was counted
	took  time.Duration // how long that took
	seq   uint64        // value of dirty when the count started
	dirty uint64        // incremented by every write that may change a count
	gen   uint64        // incremented when the database is replaced (Reset)

	run chan struct{} // capacity 1: held by the goroutine that is counting
}

// get returns a copy of the cached counts if they may still be served.
func (c *countCache) get() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.vals == nil {
		return nil
	}
	age := time.Since(c.at)
	if c.took <= countCheapLimit {
		if c.seq != c.dirty || age >= countCacheTTL {
			return nil
		}
	} else if age < 0 || age >= max(countCacheTTL, countCostFactor*c.took) {
		return nil
	}
	return maps.Clone(c.vals)
}

// TableCounter returns the exact row count of a (logical) table without
// scanning it, inside the read transaction of TableCounts. ok = false means
// "unknown": TableCounts falls back to COUNT(*) of the physical table.
type TableCounter func(ctx context.Context, tx *sql.Tx) (n int64, ok bool, err error)

var tableCounters = struct {
	mu sync.Mutex
	m  map[string]TableCounter
}{m: map[string]TableCounter{}}

// registerTableCounter installs the counter of a table (called from init of
// the file that owns the table). The table is reported under name even when
// no physical table of that name exists (a logical table).
func registerTableCounter(name string, fn TableCounter) {
	tableCounters.mu.Lock()
	tableCounters.m[name] = fn
	tableCounters.mu.Unlock()
}

func init() {
	registerTableCounter("positions", func(ctx context.Context, tx *sql.Tx) (int64, bool, error) {
		return countPositions(ctx, tx)
	})
}

// countPositions is the logical positions table: chunk points + tail rows +
// legacy rows. Tail rows that repeat a (flight_id, t) of a chunk are counted
// until the next seal (at most a few per seal interval).
func countPositions(ctx context.Context, q Querier) (int64, bool, error) {
	ok, err := hasTableTx(ctx, q, "trail_chunk")
	if err != nil || !ok {
		return 0, false, err
	}
	st, err := readTrailState(ctx, q)
	if err != nil {
		return 0, false, err
	}
	legacy, ok, err := legacyCount(ctx, q, st)
	if err != nil || !ok {
		return 0, false, err
	}
	return st.Chunk + st.Tail + legacy, true, nil
}

// legacyCount returns the rows left in the legacy positions table from the
// once-counted base (kv trail.legacy_base = "rows legacyDelAtCount") and
// the deletions since. ok = false when the base was never counted and the
// table is not empty.
func legacyCount(ctx context.Context, q querier, st trailState) (int64, bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM positions LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	v, ok, err := kvString(ctx, q, kvTrailLegacy)
	if err != nil || !ok {
		return 0, false, err
	}
	f := strings.Fields(v)
	if len(f) != 2 {
		return 0, false, nil
	}
	base, err1 := strconv.ParseInt(f[0], 10, 64)
	delAt, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false, nil
	}
	return max(0, base-(st.LegacyDel-delAt)), true, nil
}

// countLegacyBase counts the legacy positions table once (on a read
// snapshot, so the writer is not blocked) and stores the base.
func (s *Store) countLegacyBase(ctx context.Context) error {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	st, err := readTrailState(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	var n int64
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM positions`).Scan(&n)
	_ = tx.Rollback()
	if err != nil {
		return err
	}
	return s.KVSet(ctx, kvTrailLegacy, fmt.Sprintf("%d %d", n, st.LegacyDel))
}

// TableCounts returns the number of rows in every table (including tables
// other packages registered through RegisterMigration). Logical tables with
// a registered counter (positions) are exact and cost O(1); the others are
// counted with COUNT(*), cached for at most 30 s and only until the next
// write as long as counting is cheap, else for 50 times as long as it took.
// Concurrent callers share one count. The returned map belongs to the
// caller.
func (s *Store) TableCounts(ctx context.Context) (map[string]int64, error) {
	c := &s.counts
	if vals := c.get(); vals != nil {
		return vals, nil
	}
	// One counter at a time; the others wait for its result.
	select {
	case c.run <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.run }()
	if vals := c.get(); vals != nil {
		return vals, nil
	}

	c.mu.Lock()
	seq, gen := c.dirty, c.gen
	c.mu.Unlock()
	start := time.Now()

	counts, missing, err := s.countTables(ctx)
	if err != nil {
		return nil, err
	}
	if missing && s.kind == KindMain {
		// First count of a 1.0.0 positions table: count it once, then the
		// counter serves.
		if err := s.countLegacyBase(ctx); err != nil {
			return nil, err
		}
		if counts, _, err = s.countTables(ctx); err != nil {
			return nil, err
		}
	}

	c.mu.Lock()
	if c.gen == gen { // not counted across a Reset
		c.vals, c.at, c.took, c.seq = counts, time.Now(), time.Since(start), seq
	}
	c.mu.Unlock()
	return maps.Clone(counts), nil
}

// countTables counts every table in one read transaction. missing reports
// a registered counter that could not answer (its table was COUNT(*)-ed).
func (s *Store) countTables(ctx context.Context) (map[string]int64, bool, error) {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	names, err := queryStrings(ctx, tx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, false, err
	}
	tableCounters.mu.Lock()
	ctrs := maps.Clone(tableCounters.m)
	tableCounters.mu.Unlock()

	counts := make(map[string]int64, len(names))
	missing := false
	for _, name := range names {
		if fn := ctrs[name]; fn != nil {
			n, ok, err := fn(ctx, tx)
			if err != nil {
				return nil, false, err
			}
			if ok {
				counts[name] = n
				continue
			}
			missing = missing || name == "positions"
			if name == "positions" && s.kind == KindMain {
				continue // counted once by countLegacyBase, then again here
			}
		}
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(name)).Scan(&n); err != nil {
			return nil, false, err
		}
		counts[name] = n
	}
	// Logical tables without a physical table of their name.
	for name, fn := range ctrs {
		if _, done := counts[name]; done {
			continue
		}
		if n, ok, err := fn(ctx, tx); err != nil {
			return nil, false, err
		} else if ok {
			counts[name] = n
		}
	}
	return counts, missing, nil
}

// invalidateCounts notes that row counts may have changed. It never blocks
// on a count in progress (it is called from the writer goroutine).
func (s *Store) invalidateCounts() {
	s.counts.mu.Lock()
	s.counts.dirty++
	s.counts.mu.Unlock()
}

// dropCounts forgets the cached counts: the database was replaced.
func (s *Store) dropCounts() {
	s.counts.mu.Lock()
	s.counts.vals = nil
	s.counts.dirty++
	s.counts.gen++
	s.counts.mu.Unlock()
}

// CountDrift is what VerifyCounts found and corrected.
type CountDrift struct {
	Chunk, Tail, Legacy int64 // recounted minus counter (0 = exact)
}

// VerifyCounts recounts the trail counters from the tables (chunk n
// columns, not decoded; tail and legacy rows) on a read snapshot and
// corrects the kv counters by the difference. Meant for a daily job; it
// reads the trail_chunk table once.
func (s *Store) VerifyCounts(ctx context.Context) (CountDrift, error) {
	var d CountDrift
	if s.kind != KindMain || !s.HasTable("trail_chunk") {
		return d, nil
	}
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return d, err
	}
	tailMinT := int64(math.MaxInt64)
	st, err := readTrailState(ctx, tx)
	if err == nil {
		var chunk, minT sql.NullInt64
		var tail int64
		if err = tx.QueryRowContext(ctx, `SELECT SUM(n) FROM trail_chunk`).Scan(&chunk); err == nil {
			var pts sql.NullInt64
			err = tx.QueryRowContext(ctx, `SELECT SUM(n), MIN(t0) FROM trail_tail`).Scan(&pts, &minT)
			tail = pts.Int64
		}
		d.Chunk, d.Tail = chunk.Int64-st.Chunk, tail-st.Tail
		if minT.Valid && minT.Int64 < st.TailMinT {
			tailMinT = minT.Int64 // the readers must not skip tail rows
		}
	}
	var legacyOK bool
	var legacyCounted, legacyNow int64
	if err == nil {
		var cerr error
		legacyNow, legacyOK, cerr = legacyCount(ctx, tx, st)
		if cerr == nil && legacyOK && legacyNow > 0 {
			cerr = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM positions`).Scan(&legacyCounted)
			d.Legacy = legacyCounted - legacyNow
		}
		err = cerr
	}
	_ = tx.Rollback()
	if err != nil || (d == (CountDrift{}) && tailMinT == math.MaxInt64) {
		return d, err
	}
	s.log.Warn("store: trail counters drifted, corrected", "chunk", d.Chunk, "tail", d.Tail, "legacy", d.Legacy)
	err = s.Write(ctx, func(tx *sql.Tx) error {
		cur, err := readTrailState(ctx, tx)
		if err != nil {
			return err
		}
		cur.Chunk += d.Chunk
		cur.Tail += d.Tail
		cur.TailMinT = min(cur.TailMinT, tailMinT)
		if d.Legacy != 0 {
			// Fewer legacy rows than the counter says = more deleted.
			cur.LegacyDel -= d.Legacy
		}
		return writeTrailState(ctx, tx, cur)
	})
	return d, err
}
