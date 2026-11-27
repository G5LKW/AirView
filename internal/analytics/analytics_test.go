package analytics

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/store"
)

var testNow = time.Date(2026, 6, 15, 12, 0, 30, 0, time.UTC)

func newTest(t *testing.T, cfg model.Config) (*Analyzer, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := New(st, func() model.Config { return cfg })
	a.SetClock(func() time.Time { return testNow })
	a.SetLocation(time.UTC)
	return a, st
}

func apply(t *testing.T, st *store.Store, fill func(b *store.Batch)) {
	t.Helper()
	b := store.NewBatch()
	fill(b)
	if err := st.Apply(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

func locatedConfig(lat, lon float64) model.Config {
	cfg := model.DefaultConfig()
	cfg.Receiver.Located = true
	cfg.Receiver.Lat, cfg.Receiver.Lon, cfg.Receiver.GroundElevM = lat, lon, 100
	return cfg
}

// mustJSON marshals v and fails on error (NaN would be an error).
func mustJSON(t *testing.T, name string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: json: %v", name, err)
	}
	if !json.Valid(b) {
		t.Fatalf("%s: invalid JSON", name)
	}
	return string(b)
}

// allResults runs every analysis and checks that it encodes.
func allResults(t *testing.T, a *Analyzer, ant string) {
	t.Helper()
	ctx := context.Background()
	check := func(name string, v any, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mustJSON(t, name, v)
	}
	v1, err := a.Overview(ctx, ant)
	check("overview", v1, err)
	v2, err := a.Traffic(ctx, Range{}, ant)
	check("traffic", v2, err)
	v2b, err := a.Traffic(ctx, LastHours(3), ant)
	check("traffic-minute", v2b, err)
	v3, err := a.WeekHeatmap(ctx, Range{}, ant)
	check("weekheat", v3, err)
	v4, err := a.Daily(ctx, Range{}, ant)
	check("daily", v4, err)
	v5, err := a.Top(ctx, Range{}, 10)
	check("top", v5, err)
	v6, err := a.AltitudeDistribution(ctx, Range{}, ant)
	check("altdist", v6, err)
	v7, err := a.Records(ctx)
	check("records", v7, err)
	v8, err := a.RecentEvents(ctx, 10)
	check("events", v8, err)
	v9, err := a.Coverage(ctx, Range{}, ant)
	check("coverage", v9, err)
	v10, err := a.CoverageGeoJSON(ctx, Range{}, ant)
	check("geojson", v10, err)
	v11, err := a.HeatGrid(ctx, ant, HeatOptions{})
	check("heat", v11, err)
	v12, err := a.Antenna(ctx, Range{}, ant)
	check("antenna", v12, err)
	v13, err := a.Score(ctx, Range{}, ant)
	check("score", v13, err)
	v14, err := a.DayNight(ctx, Range{}, ant)
	check("daynight", v14, err)
	v15, err := a.WeatherImpact(ctx, Range{}, ant)
	check("weather", v15, err)
	v16, err := a.GuessLocation(ctx, ant)
	check("guess", v16, err)
	v17, err := a.Sun(ctx, time.Time{})
	check("sun", v17, err)
}

func TestEmptyDatabase(t *testing.T) {
	for _, cfg := range []model.Config{model.DefaultConfig(), locatedConfig(50, 8)} {
		a, _ := newTest(t, cfg)
		allResults(t, a, "")
		allResults(t, a, "yagi")
		ctx := context.Background()
		cov, _ := a.Coverage(ctx, Range{}, "")
		if len(cov.Bands) != 7 || len(cov.Overall.MaxKm) != 180 || cov.Days != 0 || len(cov.Obstructions) != 0 {
			t.Errorf("empty coverage: %+v", cov.Overall.Bearings)
		}
		fc, _ := a.CoverageGeoJSON(ctx, Range{}, "")
		if fc.Type != "FeatureCollection" || len(fc.Features) != 0 {
			t.Errorf("empty geojson: %+v", fc)
		}
		g, _ := a.GuessLocation(ctx, "")
		if g.OK || !strings.Contains(g.Reason, "not enough data") {
			t.Errorf("empty guess: %+v", g)
		}
		w, _ := a.WeatherImpact(ctx, Range{}, "")
		if w.Enough || len(w.Conclusions) != 1 || !strings.Contains(w.Conclusions[0], "Not enough data yet: 0 hours") {
			t.Errorf("empty weather conclusions: %v", w.Conclusions)
		}
		sc, _ := a.Score(ctx, Range{}, "")
		if sc.Grade != "no data" {
			t.Errorf("empty score grade %q", sc.Grade)
		}
		if s := mustJSON(t, "score", sc); !strings.Contains(s, `"score":null`) {
			t.Errorf("empty score should be null: %s", s)
		}
		ant, _ := a.Antenna(ctx, Range{}, "")
		if len(ant.Antennas) != 2 || ant.Yagi == nil || ant.Yagi.OK || len(ant.LinkBudget) != 7 {
			t.Errorf("empty antenna report: %d antennas", len(ant.Antennas))
		}
		sun, _ := a.Sun(ctx, time.Time{})
		if sun.Located != cfg.Receiver.Located || (sun.Located && (sun.Position == nil || sun.Events == nil)) || len(sun.NightSide.Sunset) < 10 {
			t.Errorf("sun: %+v", sun.Located)
		}
	}
}

func TestNumJSON(t *testing.T) {
	type s struct {
		A Num  `json:"a"`
		B Nums `json:"b"`
	}
	got := mustJSON(t, "num", s{Num(math.NaN()), Nums{1.23456, math.Inf(1), math.NaN(), -0.0001, 3}})
	if got != `{"a":null,"b":[1.235,null,null,0,3]}` {
		t.Errorf("got %s", got)
	}
}

func TestRangeResolve(t *testing.T) {
	now := int64(1_000_000_000)
	f, to := Range{}.resolve(now, 86400, 3600)
	if to < now || to-now >= 3600 || to%3600 != 0 || f%3600 != 0 || to-f < 86400 || to-f > 86400+7200 {
		t.Errorf("default: %d %d", f, to)
	}
	f, to = RangeMs(1000_000, 5000_000).resolve(now, 86400, 1)
	if f != 1000 || to != 5000 {
		t.Errorf("explicit: %d %d", f, to)
	}
	f, to = LastHours(2).resolve(now, 86400, 60)
	if to-f < 7200 || to-f > 7200+120 {
		t.Errorf("last: %d %d", f, to)
	}
}

// A station that started receiving mid-hour and has been receiving since is
// not reported as down for the part of the hour before its first data.
func TestScoreUptimeFromFirstData(t *testing.T) {
	a, st := newTest(t, model.DefaultConfig())
	now := testNow.Unix()
	start := now - 14*60
	var mins []store.StatsRow
	for ts := start - start%60; ts < now; ts += 60 {
		mins = append(mins, store.StatsRow{T: ts, Frames: 6000, DF17: 3000, Positions: 100, Aircraft: 10, MaxKm: 200, RSSIAvg: -20})
	}
	apply(t, st, func(b *store.Batch) { b.AddMinuteStats(mins) })
	sc, err := a.Score(context.Background(), Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sc.Subs {
		if s.Key == "uptime" && (!s.Available || float64(s.Score) < 90) {
			t.Errorf("uptime %+v", s)
		}
	}
	for _, f := range sc.Findings {
		if f.Code == "uptime" {
			t.Errorf("false uptime finding: %s", f.Text)
		}
	}
}

func TestTrafficAndDaily(t *testing.T) {
	a, st := newTest(t, model.DefaultConfig())
	ctx := context.Background()
	now := testNow.Unix()
	hour0 := (now/3600 - 30) * 3600 // 30 hours ago
	var mins []store.StatsRow
	for m := int64(0); m < 30*60; m++ {
		ts := hour0 + m*60
		if ts >= now {
			break
		}
		mins = append(mins, store.StatsRow{T: ts, Frames: 6000, DF17: 3000, DF11: 2000, DF4: 1000, CRCBad: 60,
			Positions: 600, MLATPositions: 60, Aircraft: 40 + int(m%7), AircraftPos: 30, ADSB: 30, MLAT: 4, ModeS: 6,
			MaxKm: 300 + float64(m%50), RSSIAvg: -20, RSSIMin: -35, RSSIMax: -3})
	}
	apply(t, st, func(b *store.Batch) {
		b.AddMinuteStats(mins)
		f1 := store.NewFlight(st.NextFlightID(), 0x3C6444, now-7200)
		f1.MaxAlt, f1.MinAlt, f1.MaxDistKm, f1.Msgs = 37000, 3000, 250, 500
		f2 := store.NewFlight(st.NextFlightID(), 0x3C6444, now-90000)
		f2.MaxAlt, f2.MinAlt, f2.Msgs = 35000, 2000, 300
		f3 := store.NewFlight(st.NextFlightID(), 0x4CA2D1, now-3600)
		f3.MaxAlt, f3.MinAlt, f3.Msgs = 12000, 500, 100
		b.UpsertFlights([]store.Flight{f1, f2, f3})
		b.UpsertAircraft([]store.AircraftSeen{
			{ICAO: 0x3C6444, FirstSeen: now - 90000, LastSeen: now, Operator: "Lufthansa", Country: "Germany", Type: "A320", Reg: "D-AIAB", Msgs: 800, Flights: 2, MaxDistKm: 250},
			{ICAO: 0x4CA2D1, FirstSeen: now - 3600, LastSeen: now, Operator: "Ryanair", Country: "Ireland", Type: "B738", Msgs: 100, Flights: 1},
		})
		b.AddACDB([]store.ACDBEntry{{ICAO: 0x4CA2D1, Reg: "EI-DCL", Type: "B738", Desc: "BOEING 737-800", Operator: "Ryanair"}})
		b.SetRecord(store.Record{Key: "max_dist_km", Value: 512.3, ICAO: 0x3C6444, T: now - 500}, store.RecordMax)
		b.AddEvents([]store.Event{{T: now - 100, Kind: "record", ICAO: 0x3C6444, Detail: `{"km":512.3}`}})
	})
	// Roll part of the data up so both sources (stats_hour + minute tail) are used.
	if _, err := st.RollupHours(ctx); err != nil {
		t.Fatal(err)
	}

	tr, err := a.Traffic(ctx, LastHours(3), "")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Resolution != "minute" || len(tr.Series.T) < 179 || len(tr.Series.T) > 181 {
		t.Fatalf("minute series: %s %d", tr.Resolution, len(tr.Series.T))
	}
	if math.Abs(tr.Series.FramesPerSec[0]-100) > 1e-9 || math.Abs(float64(tr.Totals.DecodePct)-99) > 1e-6 {
		t.Errorf("rate %v decode %v", tr.Series.FramesPerSec[0], tr.Totals.DecodePct)
	}
	if tr.Totals.UniqueAircraft != 2 || tr.Totals.Flights != 2 {
		t.Errorf("flights in 3h: %+v", tr.Totals)
	}
	tr2, _ := a.Traffic(ctx, LastHours(3), "")
	if tr2 != tr {
		t.Error("second call should be served from the cache")
	}

	th, err := a.Traffic(ctx, Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if th.Resolution != "hour" || len(th.Series.T) < 24 || len(th.Series.T) > 26 {
		t.Fatalf("hour series: %s %d", th.Resolution, len(th.Series.T))
	}
	if math.Abs(th.Series.FramesPerSec[3]-100) > 1e-9 {
		t.Errorf("hour rate %v", th.Series.FramesPerSec[3])
	}
	if float64(th.Totals.UptimePct) < 99 || float64(th.Sources.ADSBPct) != 75 || th.DF[5].DF != "DF17" || float64(th.DF[5].Pct) != 50 {
		t.Errorf("totals: uptime %v sources %+v df %+v", th.Totals.UptimePct, th.Sources, th.DF[5])
	}

	d, err := a.Daily(ctx, LastDays(3), "")
	if err != nil {
		t.Fatal(err)
	}
	var flights, newAF, frames int64
	for _, day := range d.Days {
		flights += day.Flights
		newAF += day.NewAirframes
		frames += day.Frames
	}
	if flights != 3 || newAF != 2 || frames != int64(len(mins))*6000 || d.BusiestDay == nil || d.BusiestHour == nil || d.BusiestHour.Aircraft != 46 {
		t.Errorf("daily: flights %d new %d frames %d busiest %+v %+v", flights, newAF, frames, d.BusiestDay, d.BusiestHour)
	}

	top, err := a.Top(ctx, Range{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Operators) != 2 || top.Operators[0].Key != "Lufthansa" || top.Operators[0].Flights != 2 || top.Operators[0].Aircraft != 1 ||
		top.Airframes[0].Key != "3C6444" || top.Airframes[0].Label != "D-AIAB" || top.Countries[0].Key != "Germany" {
		t.Errorf("top: %+v", top)
	}
	for _, e := range top.Types {
		if e.Key == "B738" && e.Label != "BOEING 737-800" {
			t.Errorf("type label from acdb: %+v", e)
		}
	}

	hm, err := a.WeekHeatmap(ctx, Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, row := range hm.Hours {
		for _, c := range row {
			n += c
		}
	}
	if len(hm.Aircraft) != 7 || n < 30 || n > 31 || hm.BusiestDay < 0 {
		t.Errorf("heatmap: %d hours, busiest %d", n, hm.BusiestDay)
	}

	ad, err := a.AltitudeDistribution(ctx, Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ad.FlightsN != 3 || ad.FlightAlt[18] != 1 || ad.FlightAlt[6] != 1 || float64(ad.LowestFt) != 500 || float64(ad.HighestFt) != 37000 {
		t.Errorf("altitude distribution: %+v", ad)
	}

	ov, err := a.Overview(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if ov.AllTime.Airframes != 2 || ov.AllTime.Flights != 3 || ov.AllTime.Msgs != 900 || len(ov.Records) != 1 || len(ov.Events) != 1 || ov.AllTime.Countries != 2 {
		t.Errorf("overview: %+v", ov.AllTime)
	}
	if s := mustJSON(t, "overview", ov); !strings.Contains(s, `"icao":"3C6444"`) {
		t.Errorf("overview JSON lacks record passthrough")
	}
	allResults(t, a, "")
}

// plantCoverage writes days of coverage: range per bearing from fn(band, brg).
func plantCoverage(t *testing.T, st *store.Store, days int, fn func(band int, brg float64) float64) {
	t.Helper()
	today := store.DayOf(testNow.Unix())
	rng := rand.New(rand.NewSource(1))
	apply(t, st, func(b *store.Batch) {
		var rows []store.CoverageInc
		for d := 0; d < days; d++ {
			for bin := 0; bin < 180; bin++ {
				for band := 0; band < 7; band++ {
					km := fn(band, geo.BearingBinCenter(bin))
					if km <= 0 {
						continue
					}
					km *= 0.9 + 0.1*rng.Float64()
					rows = append(rows, store.CoverageInc{Day: today - d, BrgBin: bin, AltBand: band, MaxKm: km, Count: 200, RSSISum: -20 * 200, RSSIN: 200})
				}
			}
		}
		b.MergeCoverage(rows)
	})
}

func TestCoverage(t *testing.T) {
	cfg := locatedConfig(50, 8)
	a, st := newTest(t, cfg)
	ctx := context.Background()
	antM := 108.0
	// Horizon-limited station with a shadow between 40 and 70 degrees for
	// everything below 20000 ft.
	plantCoverage(t, st, 12, func(band int, brg float64) float64 {
		_, hi := geo.AltBandBoundsFt(band)
		if math.IsInf(hi, 1) {
			hi = 45000
		}
		km := 0.9 * (geo.HorizonKm(antM, 0) + geo.HorizonKm(geo.FeetToMetres(hi), 0))
		if brg > 40 && brg < 70 && band <= 3 {
			km *= 0.3
		}
		return km
	})
	// One absurd spike on one bearing and day must not move the robust envelope.
	apply(t, st, func(b *store.Batch) {
		b.MergeCoverage([]store.CoverageInc{{Day: store.DayOf(testNow.Unix()), BrgBin: 100, AltBand: 5, MaxKm: 900, Count: 1}})
	})

	cov, err := a.Coverage(ctx, Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if cov.Days != 12 || cov.Overall.Bearings != 180 || len(cov.Bands) != 7 {
		t.Fatalf("days %d bearings %d", cov.Days, cov.Overall.Bearings)
	}
	b5 := cov.Bands[5]
	if b5.MaxKm[100] != 900 || b5.RobustKm[100] > 1.3*b5.RobustKm[90] {
		t.Errorf("spike handling: max %v robust %v neighbour %v", b5.MaxKm[100], b5.RobustKm[100], b5.RobustKm[90])
	}
	if p := float64(b5.MeanPct); p < 80 || p > 95 {
		t.Errorf("band 5 mean pct of horizon = %v (planted 90 %% x 0.9..1.0)", p)
	}
	if h := float64(b5.HorizonKm); h < 480 || h > 515 {
		t.Errorf("horizon at 40000 ft = %v", h)
	}
	if len(cov.Obstructions) != 1 {
		t.Fatalf("obstructions: %+v", cov.Obstructions)
	}
	o := cov.Obstructions[0]
	if o.FromDeg < 34 || o.FromDeg > 44 || o.ToDeg < 66 || o.ToDeg > 76 || o.Compass != "NE" && o.Compass != "ENE" || float64(o.RangePct) > 45 {
		t.Errorf("obstruction: %+v", o)
	}
	if cov.Skyline.Source != "coverage" || cov.Skyline.Bearings != 180 {
		t.Errorf("skyline: %s %d", cov.Skyline.Source, cov.Skyline.Bearings)
	}
	if cov.Skyline.ElevDeg[27] <= cov.Skyline.ElevDeg[100]+0.3 { // 55 deg vs 201 deg
		t.Errorf("skyline should be higher in the shadow: %v vs %v", cov.Skyline.ElevDeg[27], cov.Skyline.ElevDeg[100])
	}

	fc, err := a.CoverageGeoJSON(ctx, Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.Features) != 16 {
		t.Fatalf("features: %d", len(fc.Features))
	}
	for _, f := range fc.Features {
		ring := f.Geometry.Coordinates[0]
		if f.Geometry.Type != "Polygon" || len(ring) != 181 || ring[0] != ring[len(ring)-1] {
			t.Fatalf("ring not closed: %d points", len(ring))
		}
		// Counter-clockwise (positive signed area) and around the receiver.
		var area float64
		for i := 0; i+1 < len(ring); i++ {
			area += ring[i][0]*ring[i+1][1] - ring[i+1][0]*ring[i][1]
		}
		if area <= 0 {
			t.Errorf("ring %v is not counter-clockwise", f.Properties)
		}
		if f.Properties["band"] == 5 && f.Properties["kind"] == "robust" {
			d := geo.HaversineKm(50, 8, ring[10][1], ring[10][0])
			if d < 380 || d > 500 {
				t.Errorf("ring radius %v km", d)
			}
		}
	}
	mustJSON(t, "geojson", fc)

	sc, err := a.Score(ctx, Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range sc.Findings {
		if f.Code == "obstruction" && strings.Contains(f.Text, "Blocked to the") {
			found = true
		}
	}
	if !found || !isNum(float64(sc.Score)) || float64(sc.Score) < 50 {
		t.Errorf("score %v findings %+v", sc.Score, sc.Findings)
	}
	rep, err := a.Antenna(ctx, Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	row := rep.LinkBudget[5]
	if !(float64(row.PredictedKm) > 300) || !(float64(row.AchievedRobustKm) > 300) || float64(row.PctOfPredicted) < 70 {
		t.Errorf("link budget row: %+v", row)
	}
	allResults(t, a, "")
}

// plantHeat fills the heat grid around (lat, lon) from fn, which returns
// the mean RSSI and the lowest / highest altitude (ft) for a cell at the
// given distance and bearing; ok = false skips the cell.
func plantHeat(t *testing.T, st *store.Store, ant string, lat, lon, maxKm, stepKm, stepDeg float64,
	fn func(km, brg float64) (rssi, minAlt, maxAlt float64, ok bool)) {
	t.Helper()
	seen := map[uint32]bool{}
	var rows []store.HeatInc
	for d := stepKm; d <= maxKm; d += stepKm {
		for brg := 0.0; brg < 360; brg += stepDeg {
			la, lo := geo.Destination(lat, lon, brg, d)
			cell := geo.HeatCell(la, lo)
			if seen[cell] {
				continue
			}
			seen[cell] = true
			cla, clo := geo.HeatCellCenter(cell)
			km, b := geo.DistanceBearing(lat, lon, cla, clo)
			rssi, lo1, hi1, ok := fn(km, b)
			if !ok {
				continue
			}
			rows = append(rows, store.HeatInc{Cell: int64(cell), Ant: ant, Count: 60, RSSISum: rssi * 50, RSSIN: 50,
				MinAlt: lo1, MaxAlt: hi1, LastT: testNow.Unix()})
		}
	}
	apply(t, st, func(b *store.Batch) { b.MergeHeat(rows) })
}

func TestYagiLobeRecovery(t *testing.T) {
	const lat, lon, trueAz, trueBW, trueGain = 50.0, 8.0, 240.0, 40.0, 7.0
	cfg := locatedConfig(lat, lon)
	cfg.Antennas[1].AzimuthDeg = 228 // configured 12 degrees left of the truth
	a, st := newTest(t, cfg)
	rng := rand.New(rand.NewSource(7))
	n := lobeExponent(trueBW)
	gen := func(km, brg float64) (float64, float64, float64, bool) {
		altM := geo.FeetToMetres(34000)
		slant := geo.SlantRangeKm(km, 108, altM)
		g := math.Max(0, trueGain+lobeDB(geo.AngleDiff(brg, trueAz), n))
		return 28 + g - 20*math.Log10(slant) + rng.NormFloat64()*0.8, 30000, 38000, true
	}
	plantHeat(t, st, "", lat, lon, 320, 7, 1.5, gen)
	plantHeat(t, st, "yagi", lat, lon, 320, 7, 1.5, gen)

	rep, err := a.Antenna(context.Background(), Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	az := rep.Azimuth
	if az == nil || az.Method != "heat-residual" || az.Bearings < 170 {
		t.Fatalf("azimuth pattern: %+v", az)
	}
	if d := math.Abs(geo.AngleDiff(float64(az.PeakBrg), trueAz)); d > 8 || math.Abs(float64(az.PeakDB)-trueGain) > 1.5 {
		t.Errorf("measured peak %v dB at %v", az.PeakDB, az.PeakBrg)
	}
	y := rep.Yagi
	if y == nil || !y.OK {
		t.Fatalf("yagi fit: %+v", y)
	}
	if d := math.Abs(geo.AngleDiff(float64(y.AzimuthDeg), trueAz)); d > 3 {
		t.Errorf("boresight %v, want %v", y.AzimuthDeg, trueAz)
	}
	if math.Abs(float64(y.BeamwidthDeg)-trueBW) > 8 || math.Abs(float64(y.GainOverOmniDB)-trueGain) > 1 {
		t.Errorf("beamwidth %v gain %v", y.BeamwidthDeg, y.GainOverOmniDB)
	}
	if float64(y.Confidence) < 0.6 || y.Configured == nil || y.Configured.AntennaID != "yagi" {
		t.Errorf("confidence %v configured %+v", y.Confidence, y.Configured)
	}
	if e := float64(y.PointingErrorDeg); math.Abs(e-12) > 3 {
		t.Errorf("pointing error %v, want +12", e)
	}
	if !strings.Contains(y.Text, "to the right") {
		t.Errorf("text: %s", y.Text)
	}
	if len(y.FitDB) != 180 {
		t.Errorf("fit curve length %d", len(y.FitDB))
	}
	// Per-antenna rows exist for "yagi": measured pattern + own fit.
	var ym *AntennaModel
	for i := range rep.Antennas {
		if rep.Antennas[i].ID == "yagi" {
			ym = &rep.Antennas[i]
		}
	}
	if ym == nil || ym.Measured == nil || ym.Fit == nil || !ym.Fit.OK || ym.Compare == nil || len(ym.AzGainDBi) != 180 || len(ym.ElGainDBi) != 184 {
		t.Fatalf("per-antenna model: %+v", ym)
	}
	if d := math.Abs(geo.AngleDiff(float64(ym.Fit.AzimuthDeg), trueAz)); d > 3 {
		t.Errorf("per-antenna boresight %v", ym.Fit.AzimuthDeg)
	}
	if rep.Antennas[0].Measured != nil {
		t.Errorf("the vertical has no rows of its own")
	}

	sc, err := a.Score(context.Background(), Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, f := range sc.Findings {
		if f.Code == "yagi-pointing" && f.Level == "warn" && strings.Contains(f.Text, "appears to point") {
			ok = true
		}
	}
	if !ok {
		t.Errorf("no pointing finding: %+v", sc.Findings)
	}
	allResults(t, a, "")
	allResults(t, a, "yagi")
}

func TestOmniHasNoLobe(t *testing.T) {
	cfg := locatedConfig(50, 8)
	a, st := newTest(t, cfg)
	rng := rand.New(rand.NewSource(3))
	plantHeat(t, st, "", 50, 8, 320, 7, 1.5, func(km, brg float64) (float64, float64, float64, bool) {
		slant := geo.SlantRangeKm(km, 108, geo.FeetToMetres(34000))
		return 28 - 20*math.Log10(slant) + rng.NormFloat64()*0.8, 30000, 38000, true
	})
	rep, err := a.Antenna(context.Background(), Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Azimuth == nil || float64(rep.Azimuth.SpreadDB) > 1.5 {
		t.Fatalf("flat pattern expected: %+v", rep.Azimuth)
	}
	if rep.Yagi.OK && float64(rep.Yagi.Confidence) >= 0.4 {
		t.Errorf("no lobe should be reported with confidence: %+v", rep.Yagi)
	}
}

func TestSignalDistanceFit(t *testing.T) {
	a, st := newTest(t, model.DefaultConfig())
	day := store.DayOf(testNow.Unix())
	rng := rand.New(rand.NewSource(5))
	hist := map[[2]int]int64{}
	for i := 0; i < 200000; i++ {
		d := 5 + rng.Float64()*395
		rssi := 30 - 24*math.Log10(d) + rng.NormFloat64()*3
		if rssi > 0 {
			rssi = 0
		}
		hist[[2]int{store.DistBin(d), store.RSSIBin(rssi)}]++
	}
	apply(t, st, func(b *store.Batch) {
		var rows []store.SignalHistInc
		for k, n := range hist {
			rows = append(rows, store.SignalHistInc{Day: day, DistBin: k[0], RSSIBin: k[1], Count: n})
		}
		b.MergeSignalHist(rows)
		var el []store.ElevHistInc
		for bin := 4; bin < 60; bin++ {
			e := geo.ElevBinCenter(bin)
			resid := 10 - 0.3*math.Abs(e-5)
			el = append(el, store.ElevHistInc{Day: day, ElevBin: bin, Count: 1000, RSSISum: -20 * 1000, ResidSum: resid * 1000})
		}
		b.MergeElevHist(el)
	})
	rep, err := a.Antenna(context.Background(), Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	sd := rep.Signal
	if sd.Total != 200000 || sd.Fit == nil {
		t.Fatalf("signal: total %d fit %v", sd.Total, sd.Fit)
	}
	if s := float64(sd.Fit.SlopeDBPerDecade); math.Abs(s-24) > 1.5 {
		t.Errorf("slope %v, want 24", s)
	}
	if ic := float64(sd.Fit.InterceptDB); math.Abs(ic-30) > 3 {
		t.Errorf("intercept %v, want 30", ic)
	}
	if len(sd.Fit.LineKm) != 25 || float64(sd.Fit.R2) < 0.95 {
		t.Errorf("fit line %d r2 %v", len(sd.Fit.LineKm), sd.Fit.R2)
	}
	el := rep.Elevation
	if el.Total != 56000 || math.Abs(float64(el.PeakDeg)-5) > 0.6 || el.RelDB[geo.ElevBin(5.1)] < -0.2 || !math.IsNaN(el.RelDB[100]) {
		t.Errorf("elevation: total %d peak %v", el.Total, el.PeakDeg)
	}
	if v := el.RelDB[geo.ElevBin(15.1)]; math.Abs(v+3) > 0.4 {
		t.Errorf("elevation rel at 15 deg = %v, want -3", v)
	}
}

func TestGuessLocation(t *testing.T) {
	const lat, lon = 47.5, 9.2
	a, st := newTest(t, model.DefaultConfig()) // receiver NOT configured
	rng := rand.New(rand.NewSource(11))
	plantHeat(t, st, "", lat, lon, 380, 8, 3, func(km, brg float64) (float64, float64, float64, bool) {
		floorM := math.Max(300, km*km/(2*effEarthKm)*1000) + math.Abs(rng.NormFloat64())*400
		hiM := 12000.0
		mid := (floorM + hiM) / 2
		slant := math.Sqrt(km*km + mid*mid/1e6)
		return -2 - 20*math.Log10(slant) + rng.NormFloat64(), geo.MetresToFeet(floorM), geo.MetresToFeet(hiM), true
	})
	g, err := a.GuessLocation(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !g.OK || g.Fused == nil || len(g.Estimates) != 4 || g.Located {
		t.Fatalf("guess: %+v", g)
	}
	limits := map[string]float64{"centroid": 25, "signal": 5, "floor": 12, "symmetry": 12}
	for _, e := range g.Estimates {
		if !e.OK {
			t.Errorf("%s failed: %s", e.Method, e.Reason)
			continue
		}
		d := geo.HaversineKm(lat, lon, e.Lat, e.Lon)
		t.Logf("%-9s error %6.2f km  uncertainty %6.2f km  weight %.2f  %s", e.Method, d, float64(e.UncertaintyKm), float64(e.Weight), e.Detail)
		if d > limits[e.Method] {
			t.Errorf("%s is %.1f km off (limit %.0f)", e.Method, d, limits[e.Method])
		}
		if !(float64(e.UncertaintyKm) > 0) || isNum(float64(e.ErrorKm)) {
			t.Errorf("%s: uncertainty %v error %v", e.Method, e.UncertaintyKm, e.ErrorKm)
		}
	}
	d := geo.HaversineKm(lat, lon, g.Fused.Lat, g.Fused.Lon)
	t.Logf("fused     error %6.2f km  uncertainty %6.2f km", d, float64(g.Fused.UncertaintyKm))
	if d > 5 {
		t.Errorf("fused estimate is %.1f km off", d)
	}
	s := g.Surface
	if s == nil || s.Method != "signal" || len(s.Cost) != s.Rows*s.Cols || s.Rows != guessSurfaceN {
		t.Fatalf("surface: %+v", s)
	}
	if geo.HaversineKm(lat, lon, s.MinLat, s.MinLon) > 12 {
		t.Errorf("surface minimum at %v,%v", s.MinLat, s.MinLon)
	}
	if g.Maidenhead == "" || g.Formatted == "" {
		t.Errorf("formatted output missing")
	}
	mustJSON(t, "guess", g)

	// With a configured position the error is reported.
	cfg := locatedConfig(lat+0.5, lon)
	a2 := New(st, func() model.Config { return cfg })
	g2, err := a2.GuessLocation(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if e := float64(g2.Fused.ErrorKm); math.Abs(e-55.6) > 6 {
		t.Errorf("error vs configured = %v, want ~55.6 km", e)
	}
}

func TestGuessDegradesGracefully(t *testing.T) {
	a, st := newTest(t, model.DefaultConfig())
	// A handful of cells: no estimate at all.
	plantHeat(t, st, "", 40, -3, 20, 8, 90, func(km, brg float64) (float64, float64, float64, bool) {
		return -10, 5000, 30000, true
	})
	g, err := a.GuessLocation(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if g.OK || g.Fused != nil || !strings.Contains(g.Reason, "not enough data") {
		t.Errorf("tiny grid: %+v", g)
	}
	mustJSON(t, "guess", g)

	// Coverage of one quadrant only, no signal levels and no altitudes:
	// only the centroid can answer, and it must say so honestly.
	a.Invalidate()
	var rows []store.HeatInc
	for d := 10.0; d < 200; d += 9 {
		for brg := 0.0; brg < 90; brg += 6 {
			la, lo := geo.Destination(40, -3, brg, d)
			rows = append(rows, store.HeatInc{Cell: int64(geo.HeatCell(la, lo)), Count: 5, MinAlt: math.NaN(), MaxAlt: math.NaN()})
		}
	}
	apply(t, st, func(b *store.Batch) { b.MergeHeat(rows) })
	g, err = a.GuessLocation(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !g.OK || g.Fused == nil || float64(g.Fused.UncertaintyKm) < 15 {
		t.Fatalf("quadrant: %+v", g.Fused)
	}
	okCount := 0
	for _, e := range g.Estimates {
		if e.OK {
			okCount++
		} else if e.Reason == "" {
			t.Errorf("%s failed without a reason", e.Method)
		}
	}
	if okCount < 1 || okCount > 2 {
		t.Errorf("expected only the geometric methods to answer, got %d", okCount)
	}
	mustJSON(t, "guess", g)
}

// plantWeather writes hourly range + weather rows with a known dependence.
func plantWeather(t *testing.T, st *store.Store, hours int) (from int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(21))
	end := testNow.Unix() / 3600 * 3600
	from = end - int64(hours)*3600
	var rh []store.RangeHour
	var wx []store.Weather
	for i := 0; i < hours; i++ {
		ts := from + int64(i)*3600
		hod := float64(ts%86400) / 3600
		press := 1013 + 9*math.Sin(2*math.Pi*float64(i)/(24*5.3)) + rng.NormFloat64()*1.5
		temp := 15 + 6*math.Sin(2*math.Pi*(hod-9)/24) + rng.NormFloat64()
		ac := 45 + 30*math.Sin(2*math.Pi*(hod-8)/24) + rng.NormFloat64()*4
		if ac < 3 {
			ac = 3
		}
		w := store.NewWeather(ts)
		w.PressureMSL, w.TempC, w.RH = press, temp, 60+rng.NormFloat64()*10
		w.DewpointC = temp - 6 + rng.NormFloat64()
		w.WindKmh, w.Cloud = 10+rng.Float64()*20, rng.Float64()*100
		w.DNDH, w.KFactor, w.Duct, w.NSurface = -40+rng.NormFloat64()*5, 1.33, 0, 320+rng.NormFloat64()*5
		p95 := 250 + 3*(press-1013) + 25*math.Log1p(ac) + rng.NormFloat64()*5
		if i >= 200 && i < 206 { // a ducting episode
			w.Duct, w.DNDH, w.KFactor = 1, -180, 1000
			p95 += 180
		}
		wx = append(wx, w)
		rh = append(rh, store.RangeHour{T: ts, MaxKm: p95 + 30, P99Km: p95 + 10, P95Km: p95, P90Km: p95 - 10, P50Km: p95 / 2,
			Positions: int64(ac) * 100, Aircraft: int(ac), FarCount: int64(math.Max(0, p95-300)), ResidDB: 30 + rng.NormFloat64(), ResidN: 1000})
	}
	apply(t, st, func(b *store.Batch) {
		b.AddRangeHours(rh)
		b.AddWeatherObs(wx)
	})
	return from
}

func TestWeatherImpact(t *testing.T) {
	a, st := newTest(t, locatedConfig(50, 8))
	from := plantWeather(t, st, 24*20)
	w, err := a.WeatherImpact(context.Background(), LastDays(20), "")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Enough || w.Hours != 480 {
		t.Fatalf("hours %d enough %v", w.Hours, w.Enough)
	}
	var pc *WeatherCorr
	for i := range w.Correlations {
		c := &w.Correlations[i]
		if c.Var == "pressure_msl" && c.Metric == "p95_km" {
			pc = c
		}
		if c.Var == "cloud" && c.Metric == "p95_km" && c.Significant {
			t.Errorf("cloud cover must not be significant: %+v", c)
		}
	}
	if pc == nil || float64(pc.Partial) < 0.6 || !pc.Significant || float64(pc.Spearman) < 0.3 || pc.N != 480 || pc.BestLagH != 0 {
		t.Fatalf("pressure correlation: %+v", pc)
	}
	m := w.Model
	if m == nil || !m.OK || float64(m.R2) < 0.6 || float64(m.WeatherR2) < 0.2 {
		t.Fatalf("model: %+v", m)
	}
	var press, traffic *RidgeFactor
	firstWeather := ""
	for i := range m.Factors {
		f := &m.Factors[i]
		if f.Group == "weather" && firstWeather == "" {
			firstWeather = f.Name
		}
		switch f.Name {
		case "pressure_msl":
			press = f
		case "traffic":
			traffic = f
		}
	}
	if press == nil || float64(press.Beta) < 0.3 || firstWeather != "pressure_msl" {
		t.Errorf("pressure must be the leading weather factor: %+v (first %s)", press, firstWeather)
	}
	if p := float64(press.PerUnit); p < 1.5 || p > 4.5 {
		t.Errorf("pressure coefficient %v km/hPa, planted 3", p)
	}
	if traffic == nil || float64(traffic.Beta) <= 0 {
		t.Errorf("traffic factor: %+v", traffic)
	}
	if len(w.Episodes) != 1 {
		t.Fatalf("episodes: %+v", w.Episodes)
	}
	ep := w.Episodes[0]
	if !ep.DuctFlag || !ep.Anomalous || ep.Hours != 6 || ep.From != (from+200*3600)*1000 || float64(ep.MinDNDH) != -180 {
		t.Errorf("episode: %+v", ep)
	}
	foundResp := false
	for _, r := range w.Responses {
		if r.Var == "pressure_msl" && len(r.Bins) >= 6 && r.Bins[len(r.Bins)-1].YMean > r.Bins[0].YMean+20 {
			foundResp = true
		}
	}
	if !foundResp {
		t.Errorf("pressure response curve missing or flat")
	}
	text := strings.Join(w.Conclusions, " ")
	if !strings.Contains(text, "Range increases") || !strings.Contains(text, "pressure") || strings.Contains(text, "Not enough data") {
		t.Errorf("conclusions: %v", w.Conclusions)
	}
	mustJSON(t, "weather", w)

	dn, err := a.DayNight(context.Background(), LastDays(20), "")
	if err != nil {
		t.Fatal(err)
	}
	if dn.Hours != 480 || len(dn.Phases) != 3 || len(dn.ByHour) != 24 || dn.TrafficFit == nil || float64(dn.TrafficFit.SlopeKm) < 10 {
		t.Fatalf("daynight: hours %d phases %d fit %+v", dn.Hours, len(dn.Phases), dn.TrafficFit)
	}
	total := 0
	for _, p := range dn.Phases {
		total += p.Hours
	}
	if total != 480 || dn.Phases[0].Hours < 200 || dn.Phases[1].Hours+dn.Phases[2].Hours < 100 {
		t.Errorf("phase hours: %+v", dn.Phases)
	}
	// Planted: no day/night effect beyond traffic. The raw difference is
	// large (less traffic at night), the normalised one small.
	c := dn.NightVsDay
	if c == nil || math.Abs(float64(c.DiffKm)) > 12 || float64(c.RawDiffKm) > -10 {
		t.Errorf("night vs day: %+v", c)
	}
	mustJSON(t, "daynight", dn)
	allResults(t, a, "")
}

func TestWeatherNotEnoughData(t *testing.T) {
	a, st := newTest(t, model.DefaultConfig())
	plantWeather(t, st, 10)
	w, err := a.WeatherImpact(context.Background(), Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Enough || w.Hours != 10 || len(w.Conclusions) != 1 || !strings.Contains(w.Conclusions[0], "Not enough data yet: 10 hours") {
		t.Errorf("conclusions: %v", w.Conclusions)
	}
	if w.Model == nil || w.Model.OK {
		t.Errorf("model must not be fitted: %+v", w.Model)
	}
	dn, err := a.DayNight(context.Background(), Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(dn.Conclusions) != 1 || !strings.Contains(dn.Conclusions[0], "Not enough data yet: 10 hours") || dn.Located {
		t.Errorf("daynight conclusions: %v", dn.Conclusions)
	}
	mustJSON(t, "weather", w)
	mustJSON(t, "daynight", dn)
}

func TestHeatGrid(t *testing.T) {
	a, st := newTest(t, locatedConfig(50, 8))
	plantHeat(t, st, "", 50, 8, 200, 5, 2, func(km, brg float64) (float64, float64, float64, bool) {
		return -10 - km/20, 1000 + km*50, 38000, true
	})
	ctx := context.Background()
	full, err := a.HeatGrid(ctx, "", HeatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if full.Step != 1 || full.Cells != full.TotalCells || full.Cells < 1000 || len(full.Lat) != full.Cells || len(full.MinAlt) != full.Cells {
		t.Fatalf("full grid: step %d cells %d/%d", full.Step, full.Cells, full.TotalCells)
	}
	small, err := a.HeatGrid(ctx, "", HeatOptions{MaxCells: 300})
	if err != nil {
		t.Fatal(err)
	}
	if small.Step <= 1 || small.Cells > 300 || small.Positions != full.Positions {
		t.Errorf("aggregated grid: step %d cells %d positions %d vs %d", small.Step, small.Cells, small.Positions, full.Positions)
	}
	box, err := a.HeatGrid(ctx, "", HeatOptions{Bounds: &[4]float64{8, 50, 9, 51}})
	if err != nil {
		t.Fatal(err)
	}
	if box.Cells == 0 || box.Cells >= full.Cells {
		t.Errorf("bounded grid: %d cells", box.Cells)
	}
	for i := range box.Lat {
		if box.Lat[i] < 50 || box.Lat[i] > 51 || box.Lon[i] < 8 || box.Lon[i] > 9 {
			t.Fatalf("cell outside bounds: %v %v", box.Lat[i], box.Lon[i])
		}
	}
	// Skyline from the heat grid: the planted floor rises 50 ft per km.
	cov, err := a.Coverage(ctx, Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if cov.Skyline.Source != "heat" || cov.Skyline.Bearings < 170 {
		t.Fatalf("skyline: %s %d", cov.Skyline.Source, cov.Skyline.Bearings)
	}
	if m := float64(cov.Skyline.MedianDeg); m < 0.2 || m > 1.5 {
		t.Errorf("skyline median %v deg", m)
	}
}

func TestConcurrentCalls(t *testing.T) {
	a, st := newTest(t, locatedConfig(50, 8))
	plantWeather(t, st, 72)
	ctx := context.Background()
	done := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func(i int) {
			var err error
			switch i % 4 {
			case 0:
				_, err = a.Overview(ctx, "")
			case 1:
				_, err = a.Score(ctx, Range{}, "")
			case 2:
				_, err = a.WeatherImpact(ctx, Range{}, "")
			default:
				_, err = a.GuessLocation(ctx, "")
			}
			done <- err
		}(i)
	}
	for i := 0; i < 16; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("deadlock")
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.Coverage(cctx, LastDays(7), ""); err == nil {
		t.Error("cancelled context must fail an uncached query")
	}
	if _, err := a.Coverage(ctx, LastDays(7), ""); err != nil {
		t.Errorf("error must not be cached: %v", err)
	}
}
