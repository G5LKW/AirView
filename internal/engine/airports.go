package engine

import (
	"context"
	"time"

	"airview/internal/navdata"
	"airview/internal/store"
)

// Departure / arrival airports of stored flights.
//
// The tracker closes a flight after 30 min of silence (its final upsert);
// the engine then resolves the airports with the same navdata guess the
// trace page uses and stores them in flights.dep / flights.arr. One loop
// does both the "just closed" work and the backfill of older flights: it
// takes the most recently ended unresolved flights first, in small batches
// with a pause between flights, so new closes are handled within about a
// minute and history is filled in slowly behind them. It is a runtime
// goroutine, so a factory reset (runtime stop) stops it. Without airport
// data (no import, receiver not located) nothing is resolved and the rows
// stay NULL until a region exists.

const (
	// airportCloseAfter is how long after its last message a flight is
	// treated as closed (tracker gap 30 min plus margin for clock skew and
	// the tracker's flush interval).
	airportCloseAfter = 35 * time.Minute
	airportBatch      = 20                     // flights per batch
	airportPace       = 100 * time.Millisecond // pause between flights of a batch
	airportBatchPause = 3 * time.Second        // pause between full batches (backfill)
	airportIdle       = time.Minute            // poll interval when nothing is pending
)

// airportLoop runs until ctx ends.
func (e *Engine) airportLoop(ctx context.Context) {
	x := e.ext
	wait := 20 * time.Second // let the start-up work settle first
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-x.airportKick:
			t.Stop()
		case <-t.C:
		}
		n, err := e.resolveAirports(ctx, time.Now(), airportBatch, airportPace)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			e.log.Debug("flight airports", "err", err)
			wait = airportIdle
		case n >= airportBatch:
			wait = airportBatchPause
		default:
			wait = airportIdle
		}
	}
}

// resolveAirports resolves up to limit closed flights (ended before
// now-airportCloseAfter) and returns how many were stored. It does nothing
// while no airport region is loaded.
func (e *Engine) resolveAirports(ctx context.Context, now time.Time, limit int, pace time.Duration) (int, error) {
	reg := e.Region()
	if reg == nil || reg.Len() == 0 {
		return 0, nil
	}
	ids, err := e.st.UnresolvedFlights(ctx, now.Add(-airportCloseAfter).Unix(), limit)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	out := make([]store.FlightAirports, 0, len(ids))
	var tp []navdata.TrackPoint
	for i, id := range ids {
		if i > 0 && pace > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(pace):
			}
		}
		ps, err := e.st.Positions(ctx, id, 0, 0)
		if err != nil {
			return 0, err
		}
		tp = tp[:0]
		for _, p := range ps {
			tp = append(tp, navdata.TrackPoint{T: p.T * 1000, Lat: p.Lat, Lon: p.Lon, AltFt: p.Alt, GS: p.GS, Track: p.Track})
		}
		fa := store.FlightAirports{ID: id}
		if len(tp) > 0 {
			if g, ok := navdata.GuessDeparture(tp, reg); ok {
				fa.Dep = g.Airport.Code()
			}
			if g, ok := navdata.GuessArrival(tp, reg); ok {
				fa.Arr = g.Airport.Code()
			}
		}
		out = append(out, fa)
	}
	if err := e.st.SetFlightAirports(ctx, out); err != nil {
		return 0, err
	}
	return len(out), nil
}

// airportsAfterImport re-queues the flights that found no airport before
// new airport data existed, then wakes the resolver.
func (e *Engine) airportsAfterImport(ctx context.Context) {
	if n, err := e.st.ClearUnmatchedAirports(ctx); err != nil {
		e.log.Debug("flight airports: re-queue after import", "err", err)
	} else if n > 0 {
		e.log.Info("flight airports: re-resolving flights after the import", "flights", n)
	}
	e.kick(e.ext.airportKick)
}
