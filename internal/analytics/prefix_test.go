package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

// fillStation writes a deterministic set of statistics rows for one
// antenna-key mapping: minute rows for the last 40 hours, range hours,
// coverage / signal / elevation histograms for 20 days and a heat grid.
// scale varies the numbers so that different stations differ.
func fillStation(t *testing.T, st *store.Store, key func(ant string) string, scale float64, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	now := testNow.Unix()
	hour0 := (now/3600 - 40) * 3600
	apply(t, st, func(b *store.Batch) {
		for _, ant := range []string{"", "yagi"} {
			var mins []store.StatsRow
			for m := int64(0); m < 40*60; m++ {
				ts := hour0 + m*60
				if ts >= now {
					break
				}
				f := int64(scale * float64(5000+rng.Intn(2000)))
				mins = append(mins, store.StatsRow{T: ts, Ant: key(ant), Frames: f, DF17: f / 2, DF11: f / 4, DF4: f / 8,
					CRCBad: f / 100, Positions: f / 10, MLATPositions: f / 100, Aircraft: 30 + rng.Intn(20), AircraftPos: 25,
					ADSB: 25, MLAT: 3, ModeS: 5, MaxKm: scale * (250 + float64(rng.Intn(80))), RSSIAvg: -20, RSSIMin: -35,
					RSSIMax: -3, Secs: 60})
			}
			b.AddMinuteStats(mins)
			var rh []store.RangeHour
			for h := int64(0); h < 40; h++ {
				hist := make([]uint32, store.DistHistBins)
				for i := 0; i < 60; i++ {
					hist[i] = uint32(rng.Intn(100))
				}
				sect := make([]float32, store.SectorBins)
				for i := range sect {
					sect[i] = float32(scale * (200 + float64(rng.Intn(150))))
				}
				rh = append(rh, store.RangeHour{T: hour0 + h*3600, Ant: key(ant), MaxKm: scale * 330, P99Km: scale * 300,
					P95Km: scale * 280, P90Km: scale * 260, P50Km: scale * 150, Positions: 600, Aircraft: 40, FarCount: 5,
					ResidDB: 30 + rng.Float64(), ResidN: 50, DistHist: hist, SectorMax: sect})
			}
			b.AddRangeHours(rh)
			day0 := store.DayOf(now) - 19
			var cov []store.CoverageInc
			var sig []store.SignalHistInc
			var elev []store.ElevHistInc
			for d := 0; d < 20; d++ {
				for brg := 0; brg < 180; brg++ {
					for band := 0; band < 7; band++ {
						km := scale * (100 + float64(band*40) + float64(rng.Intn(30)))
						cov = append(cov, store.CoverageInc{Day: day0 + d, Ant: key(ant), BrgBin: brg, AltBand: band,
							MaxKm: km, Count: int64(1 + rng.Intn(20)), RSSISum: -25 * 3, RSSIN: 3})
					}
				}
				for db := 0; db < 50; db++ {
					sig = append(sig, store.SignalHistInc{Day: day0 + d, Ant: key(ant), DistBin: db,
						RSSIBin: -5 - db/2 - rng.Intn(5), Count: int64(1 + rng.Intn(30))})
				}
				for eb := 0; eb < 120; eb++ {
					n := int64(1 + rng.Intn(10))
					elev = append(elev, store.ElevHistInc{Day: day0 + d, Ant: key(ant), ElevBin: eb, Count: n,
						RSSISum: float64(n) * (-30 + float64(eb)/10), ResidSum: float64(n) * 25})
				}
			}
			b.MergeCoverage(cov)
			b.MergeSignalHist(sig)
			b.MergeElevHist(elev)
			var heat []store.HeatInc
			for i := 0; i < 400; i++ {
				lat := 50 + (rng.Float64()-0.5)*6
				lon := 8 + (rng.Float64()-0.5)*9
				heat = append(heat, store.HeatInc{Cell: store.HeatCell(lat, lon), Ant: key(ant), Count: int64(1 + rng.Intn(50)),
					RSSISum: -20 * 5, RSSIN: 5, MinAlt: float64(1000 + rng.Intn(5000)), MaxAlt: 38000, LastT: now - 100})
			}
			b.MergeHeat(heat)
		}
	})
	if _, err := st.RollupHours(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// fillNetwork writes flights, aircraft, records, events and weather into
// airview.db.
func fillNetwork(t *testing.T, st *store.Store) []store.Flight {
	t.Helper()
	now := testNow.Unix()
	var fl []store.Flight
	apply(t, st, func(b *store.Batch) {
		for i := 0; i < 30; i++ {
			f := store.NewFlight(st.NextFlightID(), uint32(0x400000+i%12), now-int64(i)*7000)
			f.End = f.Start + 3000
			f.MaxAlt, f.MinAlt, f.Msgs, f.Positions, f.MaxDistKm = float64(10000+i*500), 2000, int64(100+i), int64(10+i), float64(50+i*3)
			f.Callsign = "TST" + string(rune('A'+i%26))
			fl = append(fl, f)
		}
		b.UpsertFlights(fl)
		var seen []store.AircraftSeen
		for i := 0; i < 12; i++ {
			seen = append(seen, store.AircraftSeen{ICAO: uint32(0x400000 + i), FirstSeen: now - int64(i)*86400, LastSeen: now,
				Operator: "Op" + string(rune('A'+i%3)), Country: []string{"Germany", "France", "Ireland"}[i%3], Type: "A320", Msgs: 100})
		}
		b.UpsertAircraft(seen)
		b.SetRecord(store.Record{Key: "max_dist_km", Value: 400, ICAO: 0x400001, T: now - 500}, store.RecordMax)
		b.AddEvents([]store.Event{{T: now - 100, Kind: "record", ICAO: 0x400001}})
		var wx []store.Weather
		for h := int64(0); h < 40; h++ {
			w := store.NewWeather((now/3600 - 40 + h) * 3600)
			w.TempC, w.PressureMSL, w.RH = 15+float64(h%5), 1013, 70
			wx = append(wx, w)
		}
		b.AddWeatherObs(wx)
	})
	return fl
}

// resultsJSON runs every analysis for ant and returns name -> JSON.
func resultsJSON(t *testing.T, a *Analyzer, ant string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	add := func(name string, v any, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s(%q): %v", name, ant, err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = string(b)
	}
	v1, err := a.Overview(ctx, ant)
	add("overview", v1, err)
	v2, err := a.Traffic(ctx, Range{}, ant)
	add("traffic", v2, err)
	v2b, err := a.Traffic(ctx, LastHours(3), ant)
	add("traffic-minute", v2b, err)
	v3, err := a.WeekHeatmap(ctx, Range{}, ant)
	add("weekheat", v3, err)
	v4, err := a.Daily(ctx, Range{}, ant)
	add("daily", v4, err)
	v6, err := a.AltitudeDistribution(ctx, Range{}, ant)
	add("altdist", v6, err)
	v9, err := a.Coverage(ctx, Range{}, ant)
	add("coverage", v9, err)
	v10, err := a.CoverageGeoJSON(ctx, Range{}, ant)
	add("geojson", v10, err)
	v11, err := a.HeatGrid(ctx, ant, HeatOptions{})
	add("heat", v11, err)
	v12, err := a.Antenna(ctx, Range{}, ant)
	add("antenna", v12, err)
	v13, err := a.Score(ctx, Range{}, ant)
	add("score", v13, err)
	v14, err := a.DayNight(ctx, Range{}, ant)
	add("daynight", v14, err)
	v15, err := a.WeatherImpact(ctx, Range{}, ant)
	add("weather", v15, err)
	v16, err := a.GuessLocation(ctx, ant)
	add("guess", v16, err)
	v5, err := a.Top(ctx, Range{}, 10)
	add("top", v5, err)
	return out
}

// TestPrefixMatchesStandalone stores the same statistics once in a
// standalone airview.db (ant "" / "yagi") and once in stations.db as
// station 7 ("s7" / "s7/yagi") between other stations' rows (s70, s8 - a
// key that shares the prefix text and a neighbour): every analysis of the
// station Analyzer must equal the standalone one.
func TestPrefixMatchesStandalone(t *testing.T) {
	cfg := locatedConfig(50, 8)
	dir := t.TempDir()
	mainSt, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mainSt.Close() })
	stations, err := store.OpenStations(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stations.Close() })

	fillNetwork(t, mainSt)
	fillStation(t, mainSt, func(ant string) string { return ant }, 1, 1)
	fillStation(t, stations, func(ant string) string { return store.AntKey("s70", ant) }, 0.5, 2)
	fillStation(t, stations, func(ant string) string { return store.AntKey("s7", ant) }, 1, 1)
	fillStation(t, stations, func(ant string) string { return store.AntKey("s8", ant) }, 1.5, 3)

	std := New(mainSt, func() model.Config { return cfg })
	stn := New(stations, func() model.Config { return cfg }, WithAntPrefix("s7"), WithNetwork(mainSt, ""))
	for _, a := range []*Analyzer{std, stn} {
		a.SetClock(func() time.Time { return testNow })
		a.SetLocation(time.UTC)
	}
	if stn.Prefix() != "s7" || stn.SID() != "" {
		t.Fatal("options")
	}
	for _, ant := range []string{"", "yagi"} {
		want := resultsJSON(t, std, ant)
		got := resultsJSON(t, stn, ant)
		for name, w := range want {
			if got[name] != w {
				t.Errorf("%s(%q) differs:\nstandalone %.400s\nstation    %.400s", name, ant, w, got[name])
			}
		}
		if !strings.Contains(want["coverage"], `"days":20`) || strings.Contains(want["traffic"], `"frames":0,`) {
			t.Fatalf("fixture did not produce data: %.300s", want["coverage"])
		}
	}
	// Another station's analyzer sees its own data, not s7's.
	s8 := New(stations, func() model.Config { return cfg }, WithAntPrefix("s8"), WithNetwork(mainSt, ""))
	s8.SetClock(func() time.Time { return testNow })
	tr8, err := s8.Traffic(context.Background(), Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	tr7, _ := stn.Traffic(context.Background(), Range{}, "")
	if !(tr8.Totals.Frames > tr7.Totals.Frames) || tr8.Ant != "" {
		t.Fatalf("s8 frames %d vs s7 %d", tr8.Totals.Frames, tr7.Totals.Frames)
	}
}

// TestStationSIDScopesFlights checks that WithNetwork(main, sid) restricts
// the flight figures to the station's flight_station rows.
func TestStationSIDScopesFlights(t *testing.T) {
	cfg := locatedConfig(50, 8)
	dir := t.TempDir()
	mainSt, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mainSt.Close() })
	stations, err := store.OpenStations(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stations.Close() })
	fl := fillNetwork(t, mainSt)
	fillStation(t, stations, func(ant string) string { return store.AntKey("s7", ant) }, 1, 1)

	// Station AAA heard flights 0..9 (with its own counts and distances).
	nan := math.NaN()
	var fs []store.FlightStation
	for i := 0; i < 10; i++ {
		f := fl[i]
		fs = append(fs, store.FlightStation{FlightID: f.ID, SID: "AAA", ICAO: f.ICAO, First: f.Start + 60, Last: f.End,
			Msgs: 7, Positions: 2, MaxDistKm: 99 + float64(i), MinDistKm: 5, RSSIMin: nan, RSSIMax: nan, RSSIAvg: nan})
	}
	fs = append(fs, store.FlightStation{FlightID: fl[20].ID, SID: "BBB", ICAO: fl[20].ICAO, First: fl[20].Start, Last: fl[20].End,
		Msgs: 1, MaxDistKm: nan, MinDistKm: nan, RSSIMin: nan, RSSIMax: nan, RSSIAvg: nan})
	apply(t, mainSt, func(b *store.Batch) { b.UpsertFlightStation(fs) })

	a := New(stations, func() model.Config { return cfg }, WithAntPrefix("s7"), WithNetwork(mainSt, "AAA"))
	a.SetClock(func() time.Time { return testNow })
	a.SetLocation(time.UTC)
	net := New(mainSt, func() model.Config { return cfg })
	net.SetClock(func() time.Time { return testNow })
	net.SetLocation(time.UTC)
	ctx := context.Background()

	inRange := func(from, to int64) (n int64) {
		for i := 0; i < 10; i++ {
			if fl[i].End >= from && fl[i].Start < to {
				n++
			}
		}
		return
	}
	tr, err := a.Traffic(ctx, LastHours(24), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := inRange(tr.From/1000, tr.To/1000); tr.Totals.Flights != want || want == 0 {
		t.Fatalf("station flights in 24 h: %d, want %d", tr.Totals.Flights, want)
	}
	trNet, _ := net.Traffic(ctx, LastHours(24), "")
	if !(trNet.Totals.Flights > tr.Totals.Flights) {
		t.Fatalf("network %d vs station %d", trNet.Totals.Flights, tr.Totals.Flights)
	}
	top, err := a.Top(ctx, LastDays(30), 50)
	if err != nil {
		t.Fatal(err)
	}
	if top.Flights != 10 {
		t.Fatalf("top flights %d", top.Flights)
	}
	for _, e := range top.Airframes {
		if e.Msgs%7 != 0 {
			t.Fatalf("top uses network message counts: %+v", e)
		}
	}
	ov, err := a.Overview(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if ov.AllTime.Flights != 10 || ov.AllTime.Msgs != 70 || float64(ov.AllTime.MaxKm) != 108 || ov.AllTime.Airframes != 10 {
		t.Fatalf("all time %+v", ov.AllTime)
	}
	d, err := a.Daily(ctx, LastDays(30), "")
	if err != nil {
		t.Fatal(err)
	}
	var flights, newAF int64
	maxKm := 0.0
	for _, day := range d.Days {
		flights += day.Flights
		newAF += day.NewAirframes
		if day.Farthest != nil {
			maxKm = max(maxKm, float64(day.Farthest.Km))
		}
	}
	if flights != 10 || newAF != 10 || maxKm != 108 {
		t.Fatalf("daily flights %d new %d far %v", flights, newAF, maxKm)
	}
	alt, err := a.AltitudeDistribution(ctx, LastDays(30), "")
	if err != nil || alt.FlightsN != 10 {
		t.Fatalf("altitude flights %+v %v", alt, err)
	}
}

// TestDailyUsesDayTierBeyondHours: days older than the hourly data come
// from stats_day (aggregator tier); newer days keep their hourly figures.
func TestDailyUsesDayTierBeyondHours(t *testing.T) {
	dir := t.TempDir()
	stations, err := store.OpenStations(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stations.Close() })
	now := testNow.Unix()
	today := store.DayStart(store.DayOf(now))
	nan := math.NaN()
	var mins []store.StatsRow
	// Ten old days (to be reduced to day rows) and the last two days.
	for d := int64(-12); d <= 0; d++ {
		if d == -2 || d == -1 {
			continue
		}
		for h := int64(0); h < 24; h++ {
			ts := today + d*86400 + h*3600
			if ts >= now {
				break
			}
			mins = append(mins, store.StatsRow{T: ts, Ant: "s3", Frames: 100, Positions: 10, Aircraft: 5,
				MaxKm: 200, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan})
		}
	}
	stations.AddMinuteStats(mins)
	ctx := context.Background()
	if _, err := stations.RollupHours(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := stations.RollupDays(ctx); err != nil {
		t.Fatal(err)
	}
	// Drop the hours of the old days (as the hour tier would).
	if err := stations.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM stats_hour WHERE t < ?`, today-2*86400)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	mainSt, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mainSt.Close() })
	a := New(stations, nil, WithAntPrefix("s3"), WithNetwork(mainSt, "S3"))
	a.SetClock(func() time.Time { return testNow })
	a.SetLocation(time.UTC)
	d, err := a.Daily(ctx, LastDays(15), "")
	if err != nil {
		t.Fatal(err)
	}
	var withData int
	var frames int64
	for _, day := range d.Days {
		if day.Frames > 0 {
			withData++
			frames += day.Frames
		}
	}
	// 10 old days from stats_day (24 h × 100) + today's hours.
	todayHours := (now-today)/3600 + 1
	if withData != 11 || frames != 10*24*100+todayHours*100 {
		t.Fatalf("days with data %d frames %d", withData, frames)
	}
}
