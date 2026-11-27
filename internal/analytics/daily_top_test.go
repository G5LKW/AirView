package analytics

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"airview/internal/model"
	"airview/internal/store"
)

// Two synthetic local days: each day's busiest hour and farthest flight
// are reported on that day's row.
func TestDailyBusiestHourAndFarthest(t *testing.T) {
	a, st := newTest(t, model.DefaultConfig())
	ctx := context.Background()
	today := testNow.Unix() / 86400 * 86400 // UTC midnight (location is UTC)
	day1, day2 := today-2*86400, today-86400
	var mins []store.StatsRow
	plant := func(hourStart int64, aircraft int, frames int64) {
		for m := int64(0); m < 60; m++ {
			mins = append(mins, store.StatsRow{T: hourStart + m*60, Frames: frames, Aircraft: aircraft, MaxKm: 100, RSSIAvg: -20})
		}
	}
	plant(day1+3*3600, 5, 100)
	plant(day1+9*3600, 30, 200) // day 1 busiest: 09:00
	plant(day1+20*3600, 12, 300)
	plant(day2+1*3600, 8, 100)
	plant(day2+17*3600, 41, 400) // day 2 busiest: 17:00
	plant(day2+22*3600, 40, 500)

	ids := map[string]int64{}
	apply(t, st, func(b *store.Batch) {
		b.AddMinuteStats(mins)
		mk := func(icao uint32, cs string, start int64, km float64) store.Flight {
			f := store.NewFlight(st.NextFlightID(), icao, start)
			f.Callsign, f.MaxDistKm, f.Msgs = cs, km, 10
			ids[cs] = f.ID
			return f
		}
		b.UpsertFlights([]store.Flight{
			mk(0x3C6444, "DLH4AB", day1+3600, 120),
			mk(0x4CA2D1, "RYR12", day1+10*3600, 333.3), // day 1 farthest
			mk(0x400001, "", day1+11*3600, 200),
			mk(0x3C6444, "DLH5CD", day2+2*3600, 410.5), // day 2 farthest
			mk(0x4CA2D1, "RYR34", day2+5*3600, 90),
			mk(0x400002, "NOPOS", day2+6*3600, 0), // no position: ignored
		})
	})
	d, err := a.Daily(ctx, Range{From: day1, To: today}, "")
	if err != nil {
		t.Fatal(err)
	}
	byT := map[int64]DayTotals{}
	for _, day := range d.Days {
		byT[day.T] = day
	}
	d1, d2 := byT[day1*1000], byT[day2*1000]
	if d1.BusiestHour == nil || d1.BusiestHour.T != (day1+9*3600)*1000 || d1.BusiestHour.Aircraft != 30 || d1.BusiestHour.Frames != 60*200 {
		t.Errorf("day 1 busiest hour %+v", d1.BusiestHour)
	}
	if d2.BusiestHour == nil || d2.BusiestHour.T != (day2+17*3600)*1000 || d2.BusiestHour.Aircraft != 41 {
		t.Errorf("day 2 busiest hour %+v", d2.BusiestHour)
	}
	if f := d1.Farthest; f == nil || f.ICAO != "4CA2D1" || f.Callsign != "RYR12" || f.FlightID != ids["RYR12"] || float64(f.Km) != 333.3 || f.T != (day1+10*3600)*1000 {
		t.Errorf("day 1 farthest %+v", d1.Farthest)
	}
	if f := d2.Farthest; f == nil || f.ICAO != "3C6444" || f.Callsign != "DLH5CD" || float64(f.Km) != 410.5 {
		t.Errorf("day 2 farthest %+v", d2.Farthest)
	}
	if d.BusiestHour == nil || d.BusiestHour.Aircraft != 41 {
		t.Errorf("range busiest hour %+v", d.BusiestHour)
	}
	b, _ := json.Marshal(d1)
	for _, k := range []string{`"busiestHour":{"t":`, `"farthest":{"flightId":`, `,"icao":"4CA2D1","callsign":"RYR12","km":333.3,"t":`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON lacks %s: %s", k, b)
		}
	}
}

// An empty answer (fresh database) is not kept in the cache: the first
// flights show up on the next call, well within ttlTraffic.
func TestTopEmptyNotCached(t *testing.T) {
	a, st := newTest(t, model.DefaultConfig())
	ctx := context.Background()
	top, err := a.Top(ctx, Range{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if top.Flights != 0 || len(top.Operators) != 0 || len(top.Airframes) != 0 {
		t.Fatalf("empty store: %+v", top)
	}
	daily, _ := a.Daily(ctx, Range{}, "")
	ad, _ := a.AltitudeDistribution(ctx, Range{}, "")
	if ad.FlightsN != 0 {
		t.Fatalf("empty altdist %d", ad.FlightsN)
	}
	now := testNow.Unix()
	apply(t, st, func(b *store.Batch) {
		f := store.NewFlight(st.NextFlightID(), 0x3C6444, now-600)
		f.MaxAlt, f.MinAlt, f.Msgs = 30000, 1000, 50
		b.UpsertFlights([]store.Flight{f})
		b.UpsertAircraft([]store.AircraftSeen{{ICAO: 0x3C6444, FirstSeen: now - 600, LastSeen: now, Operator: "Lufthansa", Country: "Germany", Type: "A320", Msgs: 50, Flights: 1}})
	})
	top2, err := a.Top(ctx, Range{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if top2.Flights != 1 || len(top2.Operators) != 1 || top2.Operators[0].Key != "Lufthansa" || len(top2.Countries) != 1 || len(top2.Types) != 1 {
		t.Errorf("after first flight: %+v", top2)
	}
	daily2, _ := a.Daily(ctx, Range{}, "")
	if daily2 == daily {
		t.Error("empty daily answer was cached")
	}
	ad2, _ := a.AltitudeDistribution(ctx, Range{}, "")
	if ad2.FlightsN != 1 {
		t.Errorf("altdist after first flight: %d", ad2.FlightsN)
	}
	// A non-empty answer is cached (briefly while sparse).
	if top3, _ := a.Top(ctx, Range{}, 10); top3 != top2 {
		t.Error("non-empty top should be served from the cache")
	}
}
