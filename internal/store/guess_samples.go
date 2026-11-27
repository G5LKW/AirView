package store

// Locate Me v2 reception samples (docs/aggregator/DESIGN.md §10.4).
//
// guess_samples(stn INT, t INT, icao INT, lat_e5 INT, lon_e5 INT, alt INT NULL,
//               rssi_x10 INT NULL, ch INT, gnd INT, PK(stn, t, icao)) WITHOUT ROWID
//
//	one ADS-B position received off-air: stn = receiving station index
//	(0 = standalone), t = Unix ms (data time), lat/lon = value/1e5 degrees,
//	alt = barometric altitude ft (NULL unknown), rssi = value/10 dBFS (NULL
//	unknown), ch = RF channel (-1 unknown), gnd = 1 for surface positions.
//	Written by analytics.GuessRecorder from the tracker's guess-sample hook
//	(at most one sample per aircraft per 10 s), read by analytics Locate Me
//	v2. Bounded: PruneGuessSamples enforces the retention and row caps.
//
// The table exists in airview.db (standalone) and stations.db (aggregator:
// stations without a location before their survey converges). It is
// owner/admin data (it locates the station, DESIGN §8.2) and never served on
// public routes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// GuessSamplesTable is the table name.
const GuessSamplesTable = "guess_samples"

const guessSamplesDDL = `CREATE TABLE guess_samples(
  stn      INTEGER NOT NULL,
  t        INTEGER NOT NULL,
  icao     INTEGER NOT NULL,
  lat_e5   INTEGER NOT NULL,
  lon_e5   INTEGER NOT NULL,
  alt      INTEGER,
  rssi_x10 INTEGER,
  ch       INTEGER NOT NULL DEFAULT -1,
  gnd      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(stn, t, icao)
) WITHOUT ROWID;`

// Row counters (docs/perf/DB-EFFICIENCY-DESIGN.md §3.19). Every insert and
// delete of guess_samples updates kv[guessCountPrefix + stn] in the same
// transaction, so the per-station counts are exact without walking the
// table: the pruner, Locate Me and the table counts read them instead of a
// `GROUP BY stn` / COUNT(*) scan (up to ~75 MB every 10 minutes before).
//
// Migration guess_samples/2 seeds the counters from one COUNT per station
// (a single scan, once, inside the migration transaction, before the writer
// starts) - after that nothing ever counts the table again.
const (
	guessCountPrefix = "count.guess_samples."
	guessCountEnd    = "count.guess_samples/" // '/' sorts right after '.'
)

const guessCountSeedDDL = `DELETE FROM kv WHERE key >= '` + guessCountPrefix + `' AND key < '` + guessCountEnd + `';
INSERT INTO kv(key, value, updated)
  SELECT '` + guessCountPrefix + `' || stn, COUNT(*), CAST(strftime('%s', 'now') AS INTEGER)
  FROM guess_samples GROUP BY stn;`

// sqlAddGuessCount adds a (possibly negative) delta ?2 to one station's
// counter ?1. kv.value is TEXT: the sum is computed as an integer and stored
// back as its decimal text; it never goes below zero.
const sqlAddGuessCount = `INSERT INTO kv(key, value, updated) VALUES(?1, MAX(?2, 0), ?3)
ON CONFLICT(key) DO UPDATE SET value = MAX(CAST(value AS INTEGER) + ?2, 0), updated = ?3`

func init() {
	if err := RegisterMigrationFor(KindMain|KindStations, GuessSamplesTable, 1, guessSamplesDDL); err != nil {
		panic(err)
	}
	if err := RegisterMigrationFor(KindMain|KindStations, GuessSamplesTable, 2, guessCountSeedDDL); err != nil {
		panic(err)
	}
	registerTableCounter(GuessSamplesTable, func(ctx context.Context, tx *sql.Tx) (int64, bool, error) {
		ok, err := hasTableTx(ctx, tx, GuessSamplesTable)
		if err != nil || !ok {
			return 0, false, err
		}
		var n sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT SUM(CAST(value AS INTEGER)) FROM kv WHERE key >= ? AND key < ?`,
			guessCountPrefix, guessCountEnd).Scan(&n)
		return n.Int64, err == nil, err
	})
}

func guessCountKey(stn int) string { return guessCountPrefix + strconv.Itoa(stn) }

// addGuessCount applies delta to the counter of stn inside a write
// transaction.
func addGuessCount(ctx context.Context, ex kvExecer, stn int, delta int64) error {
	if delta == 0 {
		return nil
	}
	_, err := ex.ExecContext(ctx, sqlAddGuessCount, guessCountKey(stn), delta, time.Now().Unix())
	return err
}

// kvExecer is what the counter helpers need of *sql.Tx.
type kvExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// GuessSample is one row of guess_samples.
type GuessSample struct {
	Stn    int     // station index (0 = standalone)
	T      int64   // Unix ms
	ICAO   uint32  // address key
	Lat    float64 // degrees
	Lon    float64
	AltFt  float64 // NaN unknown
	RSSI   float64 // dBFS, NaN unknown
	Ch     int     // RF channel, -1 unknown
	Ground bool
}

const sqlInsertGuessSample = `INSERT OR IGNORE INTO guess_samples(stn, t, icao, lat_e5, lon_e5, alt, rssi_x10, ch, gnd)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`

// AddGuessSamples inserts reception samples (duplicates of (stn, t, icao)
// are ignored). They are low-priority rows: under write back-pressure they
// are shed first, like histogram increments. They ride the store's group
// commit (Submit / queue), never a transaction of their own, and the
// per-station row counters are updated in the same transaction.
func (b *Batch) AddGuessSamples(rows []GuessSample) {
	rows = slices.Clone(rows)
	b.add(GuessSamplesTable, tierLow, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlInsertGuessSample)
		if err != nil {
			return err
		}
		// Inserted rows per station (one station on a standalone receiver).
		var oneStn int
		var oneN int64
		var more map[int]int64
		for i := range rows {
			r := &rows[i]
			if !isNum(r.Lat) || !isNum(r.Lon) || math.Abs(r.Lat) > 90 || math.Abs(r.Lon) > 180 {
				continue
			}
			alt := ni(r.AltFt)
			var rssi any
			if isNum(r.RSSI) {
				rssi = int64(math.Round(r.RSSI * 10))
			}
			gnd := 0
			if r.Ground {
				gnd = 1
			}
			res, err := st.ExecContext(tc.ctx, r.Stn, r.T, int64(r.ICAO),
				int64(math.Round(r.Lat*1e5)), int64(math.Round(r.Lon*1e5)), alt, rssi, r.Ch, gnd)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected() // 0 for an ignored duplicate
			if err != nil || n == 0 {
				continue
			}
			switch {
			case oneN == 0 && more == nil:
				oneStn, oneN = r.Stn, n
			case more == nil && r.Stn == oneStn:
				oneN += n
			default:
				if more == nil {
					more = map[int]int64{oneStn: oneN}
				}
				more[r.Stn] += n
			}
		}
		if more == nil {
			return addGuessCount(tc.ctx, tc.tx, oneStn, oneN)
		}
		stns := make([]int, 0, len(more))
		for stn := range more {
			stns = append(stns, stn)
		}
		slices.Sort(stns) // sorted kv keys: sequential page access
		for _, stn := range stns {
			if err := addGuessCount(tc.ctx, tc.tx, stn, more[stn]); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddGuessSamples queues Batch.AddGuessSamples.
func (s *Store) AddGuessSamples(rows []GuessSample) {
	s.queue1(func(b *Batch) { b.AddGuessSamples(rows) })
}

// ScanGuessSamples calls fn for every sample of station stn with
// fromMs <= t < toMs in ascending time order (toMs <= 0 = no upper bound).
// fn returning an error stops the scan with that error.
func (s *Store) ScanGuessSamples(ctx context.Context, stn int, fromMs, toMs int64, fn func(GuessSample) error) error {
	if toMs <= 0 {
		toMs = math.MaxInt64
	}
	rows, err := s.DB().QueryContext(ctx, `SELECT t, icao, lat_e5, lon_e5, alt, rssi_x10, ch, gnd FROM guess_samples
WHERE stn = ? AND t >= ? AND t < ? ORDER BY t`, stn, fromMs, toMs)
	if err != nil {
		return err
	}
	defer rows.Close()
	g := GuessSample{Stn: stn}
	for rows.Next() {
		var icao, lat, lon, ch, gnd int64
		var alt, rssi sql.NullInt64
		if err := rows.Scan(&g.T, &icao, &lat, &lon, &alt, &rssi, &ch, &gnd); err != nil {
			return err
		}
		g.ICAO = uint32(icao)
		g.Lat, g.Lon = float64(lat)/1e5, float64(lon)/1e5
		g.AltFt, g.RSSI = math.NaN(), math.NaN()
		if alt.Valid {
			g.AltFt = float64(alt.Int64)
		}
		if rssi.Valid {
			g.RSSI = float64(rssi.Int64) / 10
		}
		g.Ch, g.Ground = int(ch), gnd != 0
		if err := fn(g); err != nil {
			return err
		}
	}
	return rows.Err()
}

// GuessSampleCounts returns the number of stored samples per station
// (stations without rows are omitted). It reads the row counters: O(number
// of stations), no table scan.
func (s *Store) GuessSampleCounts(ctx context.Context) (map[int]int64, error) {
	return guessCounts(ctx, s.DB())
}

// guessQuerier is the read part of *sql.DB / *sql.Tx.
type guessQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func guessCounts(ctx context.Context, q guessQuerier) (map[int]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, CAST(value AS INTEGER) FROM kv WHERE key >= ? AND key < ?`,
		guessCountPrefix, guessCountEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var key string
		var n int64
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		stn, err := strconv.Atoi(strings.TrimPrefix(key, guessCountPrefix))
		if err != nil || n <= 0 {
			continue
		}
		out[stn] = n
	}
	return out, rows.Err()
}

// GuessSamplesTotal returns the number of rows of guess_samples (all
// stations) from the counters: the O(1) count for table statistics.
func (s *Store) GuessSamplesTotal(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := s.DB().QueryRowContext(ctx, `SELECT SUM(CAST(value AS INTEGER)) FROM kv WHERE key >= ? AND key < ?`,
		guessCountPrefix, guessCountEnd).Scan(&n)
	return n.Int64, err
}

// GuessSampleCount returns the number of stored samples of one station
// with t >= fromMs: the station's counter minus the rows before fromMs (a
// key-range count over the oldest few minutes, as the pruner keeps the
// table at its retention), in one statement, so both come from the same
// snapshot.
func (s *Store) GuessSampleCount(ctx context.Context, stn int, fromMs int64) (int64, error) {
	var n int64
	err := s.DB().QueryRowContext(ctx, `SELECT MAX(
  COALESCE((SELECT CAST(value AS INTEGER) FROM kv WHERE key = ?1), 0) -
  (SELECT COUNT(*) FROM guess_samples WHERE stn = ?2 AND t < ?3), 0)`, guessCountKey(stn), stn, fromMs).Scan(&n)
	return n, err
}

// VerifyGuessCounts recounts guess_samples per station (a full scan: tests
// and diagnostics only) and returns the stations whose counter differs, as
// stn -> {counter, rows}. fix rewrites the differing counters.
func (s *Store) VerifyGuessCounts(ctx context.Context, fix bool) (map[int][2]int64, error) {
	diff := map[int][2]int64{}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		have, err := guessCounts(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT stn, COUNT(*) FROM guess_samples GROUP BY stn`)
		if err != nil {
			return err
		}
		real := map[int]int64{}
		for rows.Next() {
			var stn int
			var n int64
			if err := rows.Scan(&stn, &n); err != nil {
				rows.Close()
				return err
			}
			real[stn] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for stn, n := range real {
			if have[stn] != n {
				diff[stn] = [2]int64{have[stn], n}
			}
		}
		for stn, n := range have {
			if _, ok := real[stn]; !ok {
				diff[stn] = [2]int64{n, 0}
			}
		}
		if !fix {
			return nil
		}
		for stn, d := range diff {
			if err := addGuessCount(ctx, tx, stn, d[1]-d[0]); err != nil {
				return err
			}
		}
		return nil
	})
	return diff, err
}

// GuessPruneLimits bounds guess_samples.
type GuessPruneLimits struct {
	// CutoffMs: samples older than this (Unix ms) are deleted (0 = keep).
	CutoffMs int64
	// PerStation caps the rows of one station (0 = no cap); the oldest go.
	PerStation int64
	// Total caps all rows (0 = no cap). When exceeded, every station keeps
	// a share proportional to its row count (its oldest rows go).
	Total int64
}

// PruneGuessSamples enforces the limits and returns the number of rows
// deleted. Each station's delete is one writer transaction on the primary
// key range (stn, t < cutoff) that also updates the station's row counter;
// run it often (the GuessRecorder does every 10 minutes) so that each
// delete stays small.
//
// Nothing here walks the table: the per-station counts come from the
// counters, the time cut is skipped (no write transaction at all) when the
// station's oldest row - one primary-key seek - is not older than the
// cutoff, and the row caps cost an OFFSET seek only when a counter exceeds
// them.
func (s *Store) PruneGuessSamples(ctx context.Context, lim GuessPruneLimits) (int64, error) {
	var deleted int64
	del := func(stn int, before int64) (int64, error) {
		var n int64
		err := s.Write(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `DELETE FROM guess_samples WHERE stn = ? AND t < ?`, stn, before)
			if err != nil {
				return err
			}
			n, _ = res.RowsAffected()
			return addGuessCount(ctx, tx, stn, -n)
		})
		if err != nil {
			return 0, err
		}
		deleted += n
		return n, nil
	}
	counts, err := s.GuessSampleCounts(ctx)
	if err != nil {
		return 0, err
	}
	stns := make([]int, 0, len(counts))
	for stn := range counts {
		stns = append(stns, stn)
	}
	slices.Sort(stns)
	db := s.DB()
	if lim.CutoffMs > 0 {
		for _, stn := range stns {
			var oldest int64
			err := db.QueryRowContext(ctx, `SELECT t FROM guess_samples WHERE stn = ? ORDER BY t LIMIT 1`, stn).Scan(&oldest)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && oldest >= lim.CutoffMs) {
				continue
			}
			if err != nil {
				return deleted, err
			}
			n, err := del(stn, lim.CutoffMs)
			if err != nil {
				return deleted, fmt.Errorf("store: prune guess_samples: %w", err)
			}
			counts[stn] = max(counts[stn]-n, 0)
		}
	}
	var total int64
	for _, n := range counts {
		total += n
	}
	for _, stn := range stns {
		n := counts[stn]
		keep := n
		if lim.PerStation > 0 && keep > lim.PerStation {
			keep = lim.PerStation
		}
		if lim.Total > 0 && total > lim.Total {
			share := int64(float64(lim.Total) * float64(n) / float64(total))
			if share < keep {
				keep = share
			}
		}
		if keep >= n {
			continue
		}
		// t of the oldest row to keep: everything before it goes (rows
		// sharing that millisecond are kept, so the cap is approximate by
		// at most a few rows).
		var t0 int64
		err := db.QueryRowContext(ctx, `SELECT t FROM guess_samples WHERE stn = ? ORDER BY t LIMIT 1 OFFSET ?`,
			stn, n-keep).Scan(&t0)
		if errors.Is(err, sql.ErrNoRows) {
			// The counter claims more rows than there are (it can only
			// drift through a foreign writer): recount this station.
			if err := s.recountGuess(ctx, stn); err != nil {
				return deleted, err
			}
			continue
		}
		if err != nil {
			return deleted, err
		}
		if _, err := del(stn, t0); err != nil {
			return deleted, fmt.Errorf("store: prune guess_samples: %w", err)
		}
	}
	return deleted, nil
}

// recountGuess rewrites one station's counter from a COUNT of its key range
// (inside the writer, so no insert can slip in between).
func (s *Store) recountGuess(ctx context.Context, stn int) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM guess_samples WHERE stn = ?`, stn).Scan(&n); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO kv(key, value, updated) VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated = excluded.updated`, guessCountKey(stn), n, time.Now().Unix())
		return err
	})
}

// DeleteGuessSamples removes every sample of station stn (for example when
// it has been located, or when its owner deletes it).
func (s *Store) DeleteGuessSamples(ctx context.Context, stn int) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM guess_samples WHERE stn = ?`, stn); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, guessCountKey(stn))
		return err
	})
}
