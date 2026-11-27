package store

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"sync"
)

// Departure / arrival airports of stored flights (flights.dep / flights.arr,
// core migration 2). The engine resolves them once a flight is closed:
// NULL = not resolved yet, '' = resolved but no airport fits, else the
// airport code.

// FlightAirports is the resolved departure / arrival of one flight. An empty
// code means "resolved, no airport found".
type FlightAirports struct {
	ID  int64
	Dep string
	Arr string
}

// UnresolvedFlights returns the IDs of flights whose airports are not
// resolved yet and whose last message is before endBefore (Unix s), the most
// recently ended first. limit is clamped to 1..1000.
//
// It walks the flights_end index backwards from endBefore with dep IS NULL
// as a filter (there is no index of the unresolved flights since migration
// "flights" 2). The walk stops at a floor: the end time below which every
// flight was found resolved by an earlier call, so repeated calls only look
// at the flights that closed since. The floor is kept in memory per
// database generation and dropped by ClearUnmatchedAirports.
func (s *Store) UnresolvedFlights(ctx context.Context, endBefore int64, limit int) ([]int64, error) {
	limit = max(1, min(limit, 1000))
	h := s.cur.Load()
	floor := unresolvedFloorOf(h)
	rs, err := s.DB().QueryContext(ctx,
		`SELECT id, "end" FROM flights WHERE "end" < ? AND "end" >= ? AND dep IS NULL ORDER BY "end" DESC LIMIT ?`,
		endBefore, floor, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []int64
	minEnd := endBefore
	for rs.Next() {
		var id, end int64
		if err := rs.Scan(&id, &end); err != nil {
			return nil, err
		}
		out = append(out, id)
		minEnd = min(minEnd, end)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if len(out) < limit {
		// Everything in [floor, endBefore) that is unresolved was returned:
		// below the oldest of them (or endBefore) all flights are resolved.
		raiseUnresolvedFloor(h, floor, minEnd)
	}
	return out, nil
}

// unresolvedFloors holds the UnresolvedFlights floor per database
// generation (*handles -> int64).
var unresolvedFloors sync.Map

func unresolvedFloorOf(h *handles) int64 {
	if v, ok := unresolvedFloors.Load(h); ok {
		return v.(int64)
	}
	return math.MinInt64
}

func raiseUnresolvedFloor(h *handles, old, v int64) {
	if v > old {
		unresolvedFloors.CompareAndSwap(h, old, v)
		if old == math.MinInt64 {
			unresolvedFloors.LoadOrStore(h, v)
		}
	}
}

func dropUnresolvedFloor(h *handles) { unresolvedFloors.Delete(h) }

// SetFlightAirports stores resolved airports (synchronously, one short
// transaction). Unknown IDs are ignored.
func (s *Store) SetFlightAirports(ctx context.Context, rows []FlightAirports) error {
	if len(rows) == 0 {
		return nil
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `UPDATE flights SET dep = ?, arr = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, r := range rows {
			// Upper case: the report filters compare the raw column (index).
			if _, err := st.ExecContext(ctx, strings.ToUpper(r.Dep), strings.ToUpper(r.Arr), r.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClearUnmatchedAirports marks flights that were resolved without finding an
// airport as unresolved again (after new airport data was imported) and
// returns how many rows changed.
func (s *Store) ClearUnmatchedAirports(ctx context.Context) (int64, error) {
	var n int64
	defer dropUnresolvedFloor(s.cur.Load())
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE flights SET dep = NULL, arr = NULL WHERE dep = '' OR arr = ''`)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
