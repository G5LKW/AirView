package engine

import (
	"context"
	"math"
	"testing"
	"time"

	"airview/internal/navdata"
	"airview/internal/store"
)

// TestResolveFlightAirports stores a closed flight that took off from one
// runway and landed on another and checks that the resolver stores both
// airports, leaves open flights alone and marks a flight far from any
// airport as resolved-without-airport.
func TestResolveFlightAirports(t *testing.T) {
	e := New(t.TempDir(), Options{Offline: true, Logger: quietLogger()})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	ctx := context.Background()

	f64 := func(v float64) *float64 { return &v }
	i := func(v int) *int { return &v }
	rwy := func(lat, lon, hdg float64) []navdata.Runway {
		return []navdata.Runway{{LE: navdata.RunwayEnd{Ident: "09", Lat: f64(lat), Lon: f64(lon - 0.02), HeadingT: f64(hdg)},
			HE: navdata.RunwayEnd{Ident: "27", Lat: f64(lat), Lon: f64(lon + 0.02), HeadingT: f64(hdg + 180)}}}
	}
	reg := navdata.NewRegion([]navdata.Airport{
		{ID: 1, Ident: "EDDA", ICAO: "EDDA", IATA: "AAA", Type: "large_airport", Size: navdata.SizeLarge, Lat: 50, Lon: 8, ElevFt: i(300), Runways: rwy(50, 8, 90)},
		{ID: 2, Ident: "EDDB", ICAO: "EDDB", IATA: "BBB", Type: "medium_airport", Size: navdata.SizeMedium, Lat: 51, Lon: 10, ElevFt: i(200), Runways: rwy(51, 10, 90)},
	})

	st := e.Store()
	now := time.Now()
	closedEnd := now.Add(-2 * time.Hour).Unix()
	mk := func(id int64, end int64, pts [][3]float64) {
		f := store.NewFlight(id, uint32(0xABC000+id), end-int64(len(pts))*60)
		f.End = end
		st.UpsertFlights([]store.Flight{f})
		ps := make([]store.Position, len(pts))
		for k, p := range pts {
			ps[k] = store.Position{FlightID: id, T: f.Start + int64(k)*60, Lat: p[0], Lon: p[1], Alt: p[2], GS: 150, Track: 90, RSSI: math.NaN()}
		}
		st.AddPositions(ps)
	}
	// Departure from EDDA climbing east, arrival at EDDB.
	mk(1001, closedEnd, [][3]float64{{50, 8.01, 600}, {50, 8.05, 1500}, {50, 8.2, 5000}, {50.2, 8.5, 20000}, {50.4, 8.8, 30000}, {50.6, 9.2, 30000}, {50.8, 9.5, 20000}, {50.95, 9.7, 8000}, {51, 9.8, 2500}, {51, 9.9, 1500}, {51, 9.97, 600}})
	// En route only: far from both airports and high.
	mk(1002, closedEnd-10, [][3]float64{{52.5, 13, 35000}, {52.6, 13.5, 35000}})
	// Still open (ended 5 min ago).
	mk(1003, now.Add(-5*time.Minute).Unix(), [][3]float64{{50, 8.01, 600}})
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	// Start kicked navLoop's start-up region load, which stores nil for an
	// unlocated receiver and may overwrite the test region at any moment.
	// Wait for the kick to be taken, then (re)store the region until a
	// resolver pass ran with it: a pass without region does nothing.
	for deadline := time.Now().Add(5 * time.Second); len(e.ext.regionKick) > 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	var n int
	for deadline := time.Now().Add(10 * time.Second); ; {
		e.ext.region.Store(reg)
		var err error
		if n, err = e.resolveAirports(ctx, now, 50, 0); err != nil {
			t.Fatal(err)
		}
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n < 2 {
		t.Fatalf("resolved %d flights, want >= 2", n)
	}
	f, ok, err := st.FlightByID(ctx, 1001)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if f.Dep != "EDDA" || f.Arr != "EDDB" {
		t.Fatalf("flight 1001 dep=%q arr=%q, want EDDA/EDDB", f.Dep, f.Arr)
	}
	var dep, arr *string
	if err := st.DB().QueryRowContext(ctx, `SELECT dep, arr FROM flights WHERE id = 1002`).Scan(&dep, &arr); err != nil {
		t.Fatal(err)
	}
	if dep == nil || arr == nil || *dep != "" || *arr != "" {
		t.Fatalf("flight 1002 dep=%v arr=%v, want resolved empty", dep, arr)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT dep, arr FROM flights WHERE id = 1003`).Scan(&dep, &arr); err != nil {
		t.Fatal(err)
	}
	if dep != nil || arr != nil {
		t.Fatal("open flight 1003 was resolved")
	}
	// Nothing left; a run without a region does nothing.
	e.ext.region.Store(reg)
	if n, err := e.resolveAirports(ctx, now, 50, 0); err != nil || n != 0 {
		t.Fatalf("second pass = %d, %v", n, err)
	}
	e.ext.region.Store(nil)
	if n, err := e.resolveAirports(ctx, now.Add(time.Hour), 50, 0); err != nil || n != 0 {
		t.Fatalf("without region = %d, %v", n, err)
	}
}
