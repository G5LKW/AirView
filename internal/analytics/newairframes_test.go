package analytics

import (
	"context"
	"testing"

	"airview/internal/model"
	"airview/internal/store"
)

// An address in the aircraft table without a flight (heard too briefly to
// open one) must not make "new airframes" exceed "unique aircraft" for the day.
func TestDailyNewAirframesNotAboveUnique(t *testing.T) {
	a, st := newTest(t, model.DefaultConfig())
	ctx := context.Background()
	now := testNow.Unix()
	apply(t, st, func(b *store.Batch) {
		f := store.NewFlight(st.NextFlightID(), 0x3C6444, now-600)
		f.MaxAlt, f.MinAlt, f.Msgs = 37000, 3000, 500
		b.UpsertFlights([]store.Flight{f})
		b.UpsertAircraft([]store.AircraftSeen{
			{ICAO: 0x3C6444, FirstSeen: now - 600, LastSeen: now, Msgs: 500, Flights: 1},
			{ICAO: 0x4CA2D1, FirstSeen: now - 300, LastSeen: now - 290, Msgs: 2},
		})
	})
	d, err := a.Daily(ctx, LastDays(1), "")
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, day := range d.Days {
		if day.UniqueAircraft > 0 {
			seen = true
		}
		if day.UniqueAircraft > 0 && day.NewAirframes > day.UniqueAircraft {
			t.Errorf("%s: new airframes %d > unique aircraft %d", day.Date, day.NewAirframes, day.UniqueAircraft)
		}
	}
	if !seen {
		t.Fatalf("no day with flights: %+v", d.Days)
	}
}
