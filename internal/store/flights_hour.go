package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
)

// flights_hour: the hourly rollup of the flights table behind the daily
// "unique aircraft" and "flights" figures (DB-EFFICIENCY-DESIGN §3.15).
//
//	flights_hour(hour INTEGER PRIMARY KEY, flights INTEGER, icaos BLOB)
//
// One row per closed UTC hour with at least one flight that started in it:
// the number of flights and the sorted set of their ICAO addresses
// (zig-zag first value, then uvarint deltas). The distinct aircraft of a
// local day for a whole-hour UTC offset is the union of 24 hour sets; the
// open part of the range, hours split by a half-hour offset and hours the
// rollup cannot vouch for are read from flights itself, so the answer is
// always the one of the 1.0.0 query
//
//	SELECT (start + off) / 86400, COUNT(DISTINCT icao), COUNT(*) FROM flights
//	WHERE start >= ? AND start < ? GROUP BY 1
//
// Validity: the kv mark "flights_hour.mark" = "<until> <maxid>" says every
// hour before until is rolled up with the flights whose id <= maxid. A
// flight inserted later with a start before until (never seen on a station:
// rows are written within a minute of the start and hours are rolled up two
// hours after they close) is merged in by the reader and folded into its
// hour by the next roll. Retention deletes flights by end time: an hour
// before MIN("end") may have lost flights, so the reader reads those hours
// from flights and the pruner drops their rollup rows.

const (
	flightsHourTable = "flights_hour"
	kvFlightsHour    = "flights_hour.mark"
	// flightsHourLag: an hour is rolled up this long after it closed.
	flightsHourLag = 2 * 3600
	// flightsHourStep: hours rolled up per transaction.
	flightsHourStep = 7 * 24
)

const flightsHourDDL = `CREATE TABLE flights_hour(
  hour    INTEGER PRIMARY KEY,
  flights INTEGER NOT NULL,
  icaos   BLOB NOT NULL
);`

func init() {
	if err := RegisterMigrationFor(KindMain, flightsHourTable, 1, flightsHourDDL); err != nil {
		panic(err)
	}
	registerHourly(flightsHourTable, func(ctx context.Context, s *Store) error { return s.rollFlightHours(ctx, time.Now().Unix()) })
	registerPruner(flightsHourTable, pruneFlightsHour)
}

// encodeICAOSet packs a sorted, de-duplicated set.
func encodeICAOSet(set []int64) []byte {
	b := make([]byte, 0, 2+2*len(set))
	prev := int64(0)
	for i, v := range set {
		if i == 0 {
			b = binary.AppendVarint(b, v)
		} else {
			b = binary.AppendUvarint(b, uint64(v-prev))
		}
		prev = v
	}
	return b
}

// appendICAOSet appends the decoded set to dst.
func appendICAOSet(dst []int64, b []byte) ([]int64, error) {
	if len(b) == 0 {
		return dst, nil
	}
	v, k := binary.Varint(b)
	if k <= 0 {
		return dst, errors.New("store: flights_hour: bad icao set")
	}
	dst = append(dst, v)
	for b = b[k:]; len(b) > 0; b = b[k:] {
		var d uint64
		if d, k = binary.Uvarint(b); k <= 0 {
			return dst, errors.New("store: flights_hour: bad icao set")
		}
		v += int64(d)
		dst = append(dst, v)
	}
	return dst, nil
}

func sortedSet(v []int64) []int64 {
	slices.Sort(v)
	return slices.Compact(v)
}

type flightsHourMark struct {
	until, maxID int64
	ok           bool
}

func readFlightsHourMark(ctx context.Context, q querier) (flightsHourMark, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, kvFlightsHour).Scan(&v)
	if isNoRows(err) {
		return flightsHourMark{}, nil
	}
	if err != nil {
		return flightsHourMark{}, err
	}
	f := strings.Fields(v)
	if len(f) != 2 {
		return flightsHourMark{}, nil
	}
	u, err1 := strconv.ParseInt(f[0], 10, 64)
	m, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return flightsHourMark{}, nil
	}
	return flightsHourMark{until: u, maxID: m, ok: true}, nil
}

func writeFlightsHourMark(ctx context.Context, tx *sql.Tx, m flightsHourMark) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO kv(key, value, updated) VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated = excluded.updated`,
		kvFlightsHour, fmt.Sprintf("%d %d", m.until, m.maxID), time.Now().Unix())
	return err
}

// rollHours recomputes the rows of the hours [h0, h1) from flights.
func rollHours(ctx context.Context, tx *sql.Tx, h0, h1 int64) error {
	if h0 >= h1 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM flights_hour WHERE hour >= ? AND hour < ?`, h0, h1); err != nil {
		return err
	}
	type acc struct {
		n     int64
		icaos []int64
	}
	hours := map[int64]*acc{}
	rs, err := tx.QueryContext(ctx, `SELECT start, icao FROM flights WHERE start >= ? AND start < ?`, h0, h1)
	if err != nil {
		return err
	}
	for rs.Next() {
		var start int64
		var icao sql.NullInt64
		if err := rs.Scan(&start, &icao); err != nil {
			rs.Close()
			return err
		}
		h := floorTo(start, 3600)
		a := hours[h]
		if a == nil {
			a = &acc{}
			hours[h] = a
		}
		a.n++
		if icao.Valid {
			a.icaos = append(a.icaos, icao.Int64)
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	keys := make([]int64, 0, len(hours))
	for h := range hours {
		keys = append(keys, h)
	}
	slices.Sort(keys)
	st, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO flights_hour(hour, flights, icaos) VALUES(?, ?, ?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, h := range keys {
		a := hours[h]
		if _, err := st.ExecContext(ctx, h, a.n, encodeICAOSet(sortedSet(a.icaos))); err != nil {
			return err
		}
	}
	return nil
}

// rollFlightHoursTx advances the rollup by at most maxHours hours and folds
// late flights into their hours. done reports that every hour that may be
// rolled up (closed for flightsHourLag) is.
func rollFlightHoursTx(ctx context.Context, tx *sql.Tx, now int64, maxHours int) (done bool, err error) {
	m, err := readFlightsHourMark(ctx, tx)
	if err != nil {
		return false, err
	}
	var maxID, minStart sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT MAX(id) FROM flights), (SELECT MIN(start) FROM flights)`).Scan(&maxID, &minStart); err != nil {
		return false, err
	}
	end := floorTo(now-flightsHourLag, 3600)
	if !m.ok {
		m = flightsHourMark{until: end, ok: true}
		if minStart.Valid {
			m.until = min(end, floorTo(minStart.Int64, 3600))
		}
	} else if maxID.Int64 > m.maxID {
		// Late flights: inserted after their hour was rolled up.
		late, err := queryInt64s(ctx, tx, `SELECT DISTINCT (start / 3600) * 3600 FROM flights WHERE id > ? AND +start < ?`, m.maxID, m.until)
		if err != nil {
			return false, err
		}
		for _, h := range late {
			if err := rollHours(ctx, tx, h, h+3600); err != nil {
				return false, err
			}
		}
	}
	to := min(end, m.until+int64(maxHours)*3600)
	if to > m.until {
		if err := rollHours(ctx, tx, m.until, to); err != nil {
			return false, err
		}
		m.until = to
	}
	m.maxID = max(m.maxID, maxID.Int64)
	if err := writeFlightsHourMark(ctx, tx, m); err != nil {
		return false, err
	}
	return m.until >= end, nil
}

// rollFlightHours is the hourly job: roll up every closed hour.
func (s *Store) rollFlightHours(ctx context.Context, now int64) error {
	if s.kind != KindMain || !s.HasTable(flightsHourTable) {
		return nil
	}
	for {
		done := false
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			done, err = rollFlightHoursTx(ctx, tx, now, flightsHourStep)
			return err
		}); err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// pruneFlightsHour drops the rollup rows of hours that flight retention
// may have thinned (hours before the oldest remaining flight end, and
// before this run's flight cutoff).
func pruneFlightsHour(ctx context.Context, s *Store, cfg model.StorageConfig, now int64, res *PruneResult) error {
	if s.kind != KindMain || !s.HasTable(flightsHourTable) {
		return nil
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		var minEnd sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN("end") FROM flights`).Scan(&minEnd); err != nil {
			return err
		}
		cut := minEnd.Int64
		if !minEnd.Valid {
			cut = now
		}
		if cfg.FlightRetentionDays > 0 {
			cut = max(cut, now-int64(cfg.FlightRetentionDays)*86400)
		}
		r, err := tx.ExecContext(ctx, `DELETE FROM flights_hour WHERE hour < ?`, cut)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n > 0 && res != nil {
			if res.Extra == nil {
				res.Extra = map[string]int64{}
			}
			res.Extra[flightsHourTable] += n
		}
		return nil
	})
}

// FlightDay is the flight count and the distinct aircraft of one day.
type FlightDay struct {
	Aircraft int64
	Flights  int64
}

// FlightDays returns, per local day number floor((start + off) / 86400),
// the distinct aircraft and the flights that started in [from, to): the
// answer of the 1.0.0 COUNT(DISTINCT icao) query, from the hourly rollup
// where it applies (see the file comment) and from flights elsewhere.
func (s *Store) FlightDays(ctx context.Context, from, to, off int64) (map[int64]FlightDay, error) {
	out := map[int64]FlightDay{}
	if from >= to {
		return out, nil
	}
	db := s.DB()
	var m flightsHourMark
	var minEnd sql.NullInt64
	if s.kind == KindMain && s.HasTable(flightsHourTable) {
		var err error
		if m, err = readFlightsHourMark(ctx, db); err != nil {
			return nil, err
		}
		if err := db.QueryRowContext(ctx, `SELECT MIN("end") FROM flights`).Scan(&minEnd); err != nil {
			return nil, err
		}
		if !minEnd.Valid {
			return out, nil // no flights at all
		}
	}
	if !m.ok { // no rollup (yet): the 1.0.0 query
		err := s.queryEach(ctx, `SELECT (start + ?) / 86400, COUNT(DISTINCT icao), COUNT(*) FROM flights
WHERE start >= ? AND start < ? GROUP BY 1`, []any{off, from, to}, func(rs *sql.Rows) error {
			var d, u, n int64
			if err := rs.Scan(&d, &u, &n); err != nil {
				return err
			}
			out[d] = FlightDay{Aircraft: u, Flights: n}
			return nil
		})
		return out, err
	}
	// Rollup region: whole hours inside [from, to), at or after the oldest
	// flight end, before the mark.
	rlo, rhi := int64(0), int64(0)
	if m.ok {
		rlo = max(floorTo(from+3599, 3600), floorTo(minEnd.Int64+3599, 3600))
		rhi = min(floorTo(to, 3600), m.until)
	}
	type dayAcc struct {
		n     int64
		icaos []int64
	}
	days := map[int64]*dayAcc{}
	dayOf := func(t int64) *dayAcc {
		d := floorDiv(t+off, 86400)
		a := days[d]
		if a == nil {
			a = &dayAcc{}
			days[d] = a
		}
		return a
	}
	addLive := func(q string, args ...any) error {
		rs, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var start int64
			var icao sql.NullInt64
			if err := rs.Scan(&start, &icao); err != nil {
				return err
			}
			a := dayOf(start)
			a.n++
			if icao.Valid {
				a.icaos = append(a.icaos, icao.Int64)
			}
		}
		return rs.Err()
	}
	const liveQ = `SELECT start, icao FROM flights WHERE start >= ? AND start < ?`
	if rlo >= rhi { // nothing the rollup can answer
		if err := addLive(liveQ, from, to); err != nil {
			return nil, err
		}
	} else {
		if err := addLive(liveQ, from, rlo); err != nil {
			return nil, err
		}
		if err := addLive(liveQ, rhi, to); err != nil {
			return nil, err
		}
		// Hours split by a day boundary (UTC offsets that are not whole
		// hours) are read from flights; the others from the rollup.
		split := (off%3600+3600)%3600 != 0
		rs, err := db.QueryContext(ctx, `SELECT hour, flights, icaos FROM flights_hour WHERE hour >= ? AND hour < ?`, rlo, rhi)
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var h, n int64
			var b []byte
			if err := rs.Scan(&h, &n, &b); err != nil {
				rs.Close()
				return nil, err
			}
			if split && floorDiv(h+off, 86400) != floorDiv(h+3599+off, 86400) {
				continue
			}
			a := dayOf(h)
			a.n += n
			if a.icaos, err = appendICAOSet(a.icaos, b); err != nil {
				rs.Close()
				return nil, err
			}
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return nil, err
		}
		if split {
			for h := rlo; h < rhi; h += 3600 {
				if floorDiv(h+off, 86400) != floorDiv(h+3599+off, 86400) {
					if err := addLive(liveQ, h, h+3600); err != nil {
						return nil, err
					}
				}
			}
		}
		// Flights inserted after their hour was rolled up.
		lateQ := `SELECT start, icao FROM flights WHERE id > ? AND +start >= ? AND +start < ?`
		if !split {
			if err := addLive(lateQ, m.maxID, rlo, rhi); err != nil {
				return nil, err
			}
		} else {
			rs, err := db.QueryContext(ctx, `SELECT start, icao FROM flights WHERE id > ? AND +start >= ? AND +start < ?`, m.maxID, rlo, rhi)
			if err != nil {
				return nil, err
			}
			for rs.Next() {
				var start int64
				var icao sql.NullInt64
				if err := rs.Scan(&start, &icao); err != nil {
					rs.Close()
					return nil, err
				}
				h := floorTo(start, 3600)
				if floorDiv(h+off, 86400) != floorDiv(h+3599+off, 86400) {
					continue // already read live with its split hour
				}
				a := dayOf(start)
				a.n++
				if icao.Valid {
					a.icaos = append(a.icaos, icao.Int64)
				}
			}
			rs.Close()
			if err := rs.Err(); err != nil {
				return nil, err
			}
		}
	}
	for d, a := range days {
		out[d] = FlightDay{Aircraft: int64(len(sortedSet(a.icaos))), Flights: a.n}
	}
	return out, nil
}
