package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

func openStationsTest(t *testing.T, dir string) *Store {
	t.Helper()
	o := manual
	o.Logger = quiet()
	s, err := OpenStations(dir, o)
	if err != nil {
		t.Fatalf("open stations: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func tableSet(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	names, err := queryStrings(bg, s.DB(), `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestOpenStationsMigrationSubset(t *testing.T) {
	dir := t.TempDir()
	st := openStationsTest(t, dir)
	main := openDir(t, dir, manual) // airview.db next to it

	if st.Kind() != KindStations || main.Kind() != KindMain {
		t.Fatal("kinds")
	}
	if !strings.HasSuffix(st.Path(), StationsFileName) || !strings.HasSuffix(main.Path(), FileName) {
		t.Fatalf("paths %s %s", st.Path(), main.Path())
	}
	have := tableSet(t, st)
	for _, n := range []string{"kv", "stats_minute", "stats_hour", "stats_day", "range_hour", "coverage", "signal_hist",
		"elev_hist", "heat", "receiver_status", "station_minute", "station_clock_minute", "schema_migrations"} {
		if !have[n] {
			t.Errorf("stations.db lacks %s", n)
		}
	}
	for _, n := range []string{"aircraft", "flights", "positions", "weather_obs", "acdb", "flight_station", "mlat_minute",
		"records", "events", "met_obs"} {
		if have[n] {
			t.Errorf("stations.db must not have %s", n)
		}
	}
	if n := scalar[int](t, st, `SELECT COUNT(*) FROM schema_migrations WHERE name = 'core'`); n != 0 {
		t.Fatalf("core migrations applied to stations.db: %d", n)
	}
	mh := tableSet(t, main)
	for _, n := range []string{"flight_station", "mlat_minute", "mlat_quality_hour", "stats_day"} {
		if !mh[n] {
			t.Errorf("airview.db lacks %s", n)
		}
	}
	if mh["station_minute"] {
		t.Error("airview.db must not have station_minute")
	}

	// Writers: namespaced stats and rollup work on stations.db.
	const h0 = int64(1_699_999_200)
	var rows []StatsRow
	for _, ant := range []string{"s7", "s7/0", "s70"} {
		for i := int64(0); i < 60; i++ {
			rows = append(rows, StatsRow{T: h0 + i*60, Ant: ant, Frames: 10, Aircraft: 3, MaxKm: math.NaN(),
				RSSIAvg: math.NaN(), RSSIMin: math.NaN(), RSSIMax: math.NaN()})
		}
	}
	st.AddMinuteStats(rows)
	st.AddStationMinutes([]StationMinute{{T: h0 + 5, SID: "ABC", Frames: 5, UpSecs: 30, RTTMs: math.NaN()},
		{T: h0 + 50, SID: "ABC", Frames: 7, UpSecs: 45, RTTMs: 12}})
	st.AddStationClockMinutes([]StationClockMinute{{T: h0, SID: "ABC", SyncedSecs: 60, SigmaNs: 40, DriftPPM: math.NaN(),
		BetaShortNs: math.NaN(), ResidRMSNs: math.NaN(), Peers: 3, Outliers: 1}})
	if n, err := st.RollupHours(bg); err != nil || n != 3 {
		t.Fatalf("rollup: %d %v", n, err)
	}
	hs, err := st.HourStats(bg, "s7/0", 0, 0)
	if err != nil || len(hs) != 1 || hs[0].Frames != 600 || hs[0].Minutes != 60 {
		t.Fatalf("hour rows: %v %+v", err, hs)
	}
	sm, err := st.StationMinutes(bg, "ABC", 0, 0)
	if err != nil || len(sm) != 1 || sm[0].Frames != 12 || sm[0].UpSecs != 60 || sm[0].RTTMs != 12 {
		t.Fatalf("station minute: %v %+v", err, sm)
	}
	cm, err := st.StationClockMinutes(bg, "ABC", 0, 0)
	if err != nil || len(cm) != 1 || cm[0].Peers != 3 || cm[0].SigmaNs != 40 || !math.IsNaN(cm[0].DriftPPM) {
		t.Fatalf("clock minute: %v %+v", err, cm)
	}
	keys, err := st.StationAntKeys(bg, "s7")
	if err != nil || strings.Join(keys, ",") != "s7,s7/0" {
		t.Fatalf("ant keys of s7: %v %v", keys, err)
	}

	// An operation for a table stations.db does not have fails alone.
	b := NewBatch()
	b.UpsertFlights([]Flight{NewFlight(1, 0xABCDEF, h0)})
	b.AddMinuteStats([]StatsRow{{T: h0, Ant: "s9", Frames: 1, MaxKm: math.NaN(), RSSIAvg: math.NaN(), RSSIMin: math.NaN(), RSSIMax: math.NaN()}})
	st.Submit(b)
	mustFlush(t, st)
	if st.Stats().FailedOps != 1 {
		t.Fatalf("failed ops: %+v", st.Stats())
	}
	if n := scalar[int](t, st, `SELECT COUNT(*) FROM stats_minute WHERE ant = 's9'`); n != 1 {
		t.Fatal("the good operation of the batch was lost")
	}

	// Reset is refused (registry lives in the same file); Prune works.
	if err := st.Reset(); !errors.Is(err, ErrResetUnsupported) {
		t.Fatalf("reset: %v", err)
	}
	if _, err := st.Prune(bg, defaultStorage()); err != nil {
		t.Fatalf("prune on stations.db: %v", err)
	}

	// Reopen: nothing re-applied.
	before := scalar[int](t, st, `SELECT COUNT(*) FROM schema_migrations`)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2 := openStationsTest(t, dir)
	if after := scalar[int](t, st2, `SELECT COUNT(*) FROM schema_migrations`); after != before {
		t.Fatalf("migrations re-applied: %d -> %d", before, after)
	}
}

func TestRegisterMigrationKinds(t *testing.T) {
	dir := t.TempDir()
	st := openStationsTest(t, dir)
	main := openDir(t, dir, manual)

	if err := RegisterMigrationFor(KindStations, "ktest_st", 1, `CREATE TABLE ktest_st(x)`); err != nil {
		t.Fatal(err)
	}
	if !st.HasTable("ktest_st") || main.HasTable("ktest_st") {
		t.Fatal("KindStations migration applied to the wrong database")
	}
	// Plain RegisterMigration: main only (unless shared).
	if err := RegisterMigration("ktest_main", 1, `CREATE TABLE ktest_main(x)`); err != nil {
		t.Fatal(err)
	}
	if st.HasTable("ktest_main") || !main.HasTable("ktest_main") {
		t.Fatal("RegisterMigration must target airview.db only")
	}
	// Extending the kinds of an identical registration applies it to the
	// newly covered database only.
	if err := RegisterMigrationFor(KindMain|KindStations, "ktest_main", 1, `CREATE TABLE ktest_main(x)`); err != nil {
		t.Fatalf("extend: %v", err)
	}
	if !st.HasTable("ktest_main") {
		t.Fatal("extension not applied to stations.db")
	}
	if err := RegisterMigrationFor(KindMain, "stations", 9, `x`); err == nil {
		t.Fatal("reserved name accepted")
	}
	if err := RegisterMigrationFor(0, "ktest_none", 1, `x`); err == nil {
		t.Fatal("no kind accepted")
	}
	// The shared list puts latency_minute into stations.db.
	if !stationShared["latency"] {
		t.Fatal("latency must be shared")
	}
}

func TestAntKeys(t *testing.T) {
	cases := []struct{ p, a, want string }{{"", "yagi", "yagi"}, {"", "", ""}, {"s7", "", "s7"}, {"s7", "1", "s7/1"}}
	for _, c := range cases {
		if got := AntKey(c.p, c.a); got != c.want {
			t.Errorf("AntKey(%q,%q) = %q", c.p, c.a, got)
		}
	}
	if p, a := SplitAntKey("s12/3"); p != "s12" || a != "3" {
		t.Error("split s12/3")
	}
	if p, a := SplitAntKey("s12"); p != "s12" || a != "" {
		t.Error("split s12")
	}
	if p, a := SplitAntKey("yagi/x"); p != "" || a != "yagi/x" {
		t.Error("split standalone")
	}
	for _, bad := range []string{"", "s", "s0", "s01", "x1", "s1a", "s99999999999"} {
		if ValidAntPrefix(bad) {
			t.Errorf("ValidAntPrefix(%q)", bad)
		}
	}
	if StationAntPrefix(42) != "s42" {
		t.Error("StationAntPrefix")
	}
	lo, hi := AntKeyRange("s7")
	for _, k := range []string{"s7/0", "s7/7", "s7/zz"} {
		if !(k >= lo && k < hi) {
			t.Errorf("%s not in range", k)
		}
	}
	for _, k := range []string{"s7", "s70", "s70/1", "s8", "s6/1"} {
		if k >= lo && k < hi {
			t.Errorf("%s in range", k)
		}
	}
}

func TestFlightStationMergeAndPrune(t *testing.T) {
	s := openTest(t, manual)
	now := time.Now().Unix()
	f := NewFlight(5, 0x400001, now-40*86400)
	f.End = now - 39*86400
	s.UpsertFlights([]Flight{f})
	nan := math.NaN()
	s.UpsertFlightStation([]FlightStation{
		{FlightID: 5, SID: "AAA", ICAO: 0x400001, First: 100, Last: 200, Msgs: 10, Positions: 2, MaxDistKm: 50, MinDistKm: 20, RSSIMin: -30, RSSIMax: -10, RSSIAvg: -20},
		{FlightID: 5, SID: "BBB", First: 150, Last: 160, Msgs: 1, MaxDistKm: nan, MinDistKm: nan, RSSIMin: nan, RSSIMax: nan, RSSIAvg: nan},
		{FlightID: 0, SID: "CCC"}, {FlightID: 6, SID: ""},
	})
	s.UpsertFlightStation([]FlightStation{
		{FlightID: 5, SID: "AAA", First: 90, Last: 250, Msgs: 30, Positions: 3, MaxDistKm: 80, MinDistKm: nan, RSSIMin: -35, RSSIMax: -12, RSSIAvg: -40},
	})
	mustFlush(t, s)
	rows, err := s.FlightStations(bg, 5)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %v %+v", err, rows)
	}
	a := rows[0]
	if a.SID != "AAA" || a.ICAO != 0x400001 || a.First != 90 || a.Last != 250 || a.Msgs != 40 || a.Positions != 5 ||
		a.MaxDistKm != 80 || a.MinDistKm != 20 || a.RSSIMin != -35 || a.RSSIMax != -10 || !near(a.RSSIAvg, -35) {
		t.Fatalf("merged %+v", a)
	}
	if b := rows[1]; !math.IsNaN(b.MaxDistKm) || !math.IsNaN(b.RSSIAvg) || b.Msgs != 1 {
		t.Fatalf("unlocated %+v", b)
	}
	sf, err := s.StationFlights(bg, "AAA", 0, 0, 0)
	if err != nil || len(sf) != 1 || sf[0].FlightID != 5 {
		t.Fatalf("station flights %v %+v", err, sf)
	}
	// Flight retention removes the flight_station rows with the flight.
	st := defaultStorage()
	st.FlightRetentionDays = 30
	if _, err := s.Prune(bg, st); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM flight_station`); n != 0 {
		t.Fatalf("flight_station rows left: %d", n)
	}
}

func TestMLATTables(t *testing.T) {
	s := openTest(t, manual)
	const t0 = int64(1_700_000_000)
	nan := math.NaN()
	s.AddMLATMinutes([]MLATMinute{
		{T: t0 + 5, SID: "AAA", Frames: 100, Groups: 10, Fixes: 4, Peers: 3, SyncedSecs: 40, SigmaNs: 50, ResidRMSNs: nan, DriftPPM: 1},
		{T: t0 + 30, SID: "AAA", Frames: 50, Groups: 5, Fixes: 1, Outliers: 2, Peers: 5, SyncedSecs: 40, SigmaNs: nan, ResidRMSNs: 80, DriftPPM: 2},
		{T: t0, SID: "", Fixes: 9, SyncedSecs: nan, SigmaNs: nan, ResidRMSNs: nan, DriftPPM: nan},
	})
	mustFlush(t, s)
	m, err := s.MLATMinutes(bg, "AAA", 0, 0)
	if err != nil || len(m) != 1 {
		t.Fatalf("%v %+v", err, m)
	}
	r := m[0]
	if r.T != floorTo(t0, 60) || r.Frames != 150 || r.Groups != 15 || r.Fixes != 5 || r.Outliers != 2 || r.Peers != 5 ||
		r.SyncedSecs != 60 || r.SigmaNs != 50 || r.ResidRMSNs != 80 || r.DriftPPM != 2 {
		t.Fatalf("merged %+v", r)
	}
	if net, _ := s.MLATMinutes(bg, "", 0, 0); len(net) != 1 || !math.IsNaN(net[0].SyncedSecs) {
		t.Fatalf("network row %+v", net)
	}

	// Quality: histograms merge, percentiles follow.
	cell := Cell1Deg(51.5, -0.1)
	if la, lo := Cell1DegOrigin(cell); la != 51 || lo != -1 {
		t.Fatalf("cell origin %v %v", la, lo)
	}
	h1 := make([]uint32, MLATErrBins)
	h1[MLATErrBin(20)] = 50
	h2 := make([]uint32, MLATErrBins)
	h2[MLATErrBin(20)] = 40
	h2[MLATErrBin(500)] = 10
	s.MergeMLATQuality([]MLATQualityInc{{T: t0, Cell: cell, AltBand: 3, Hist: h1}, {T: t0, Cell: -1, Hist: h1}})
	s.MergeMLATQuality([]MLATQualityInc{{T: t0 + 100, Cell: cell, AltBand: 3, Hist: h2}})
	mustFlush(t, s)
	q, err := s.MLATQualityHours(bg, 0, 0, -1)
	if err != nil || len(q) != 1 {
		t.Fatalf("%v %+v", err, q)
	}
	if q[0].N != 100 || q[0].T != floorTo(t0, 3600) || !(q[0].P50 > 15 && q[0].P50 < 26) || !(q[0].P99 > 400 && q[0].P99 < 650) {
		t.Fatalf("quality %+v", q[0])
	}
	if MLATErrBin(0.3) != 0 || MLATErrBin(1e9) != MLATErrBins-1 || !math.IsNaN(MLATErrPercentile(nil, 0.5)) {
		t.Fatal("bin edges")
	}
}

func TestRollupDaysAndHourTier(t *testing.T) {
	s := openTest(t, manual)
	day0 := int64(19_000) * 86400
	var rows []StatsRow
	for h := int64(0); h < 48; h++ { // two full days, one row per minute 0 and 30
		for _, m := range []int64{0, 30} {
			rows = append(rows, StatsRow{T: day0 + h*3600 + m*60, Ant: "s1", Frames: 60, Positions: 6, Aircraft: int(h%24) + 1,
				MaxKm: float64(h), RSSIAvg: -20, RSSIMin: -30, RSSIMax: -5, Secs: 60})
		}
	}
	s.AddMinuteStats(rows)
	if _, err := s.RollupHours(bg); err != nil {
		t.Fatal(err)
	}
	n, err := s.RollupDays(bg)
	if err != nil || n != 2 {
		t.Fatalf("days: %d %v", n, err)
	}
	ds, err := s.DayStats(bg, "s1", 0, 0)
	if err != nil || len(ds) != 2 {
		t.Fatalf("%v %+v", err, ds)
	}
	d := ds[1]
	if d.T != day0+86400 || d.Frames != 48*60 || d.Positions != 48*6 || d.Aircraft != 24 || d.MaxKm != 47 ||
		d.Minutes != 48 || d.Secs != 48*60 || !near(d.AircraftAvg, 12.5) || d.RSSIMin != -30 {
		t.Fatalf("day row %+v", d)
	}
	// Idempotent.
	if _, err := s.RollupDays(bg); err != nil {
		t.Fatal(err)
	}
	if ds2, _ := s.DayStats(bg, "s1", 0, 0); len(ds2) != 2 || ds2[1].Frames != d.Frames {
		t.Fatalf("not idempotent %+v", ds2)
	}

	// Hour tier: hours older than the cut-off go, the day rows stay frozen.
	now := day0 + 400*86400
	p := TierPolicy{HourDays: 395, OptimizeEvery: 1000 * time.Hour}
	rep, err := s.maintainAt(bg, p, now)
	if err != nil {
		t.Fatal(err)
	}
	// The newest day stays open (its hours are kept until a later hour
	// arrives), exactly like the newest hour of RollupHours.
	if rep.Deleted["stats_hour"] != 24 {
		t.Fatalf("hours deleted %+v", rep)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM stats_day`); n != 2 {
		t.Fatal("day rows lost")
	}
	// A rollup after the prune does not touch the frozen days.
	s.AddMinuteStats([]StatsRow{{T: day0 + 5*60, Ant: "s1", Frames: 1, MaxKm: math.NaN(), RSSIAvg: math.NaN(), RSSIMin: math.NaN(), RSSIMax: math.NaN()}})
	if _, err := s.RollupHours(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RollupDays(bg); err != nil {
		t.Fatal(err)
	}
	if ds3, _ := s.DayStats(bg, "s1", day0, day0); len(ds3) != 1 || ds3[0].Frames != d.Frames {
		t.Fatalf("frozen day recomputed: %+v", ds3)
	}
}

func TestHistogramMonthBuckets(t *testing.T) {
	s := openTest(t, manual)
	// 2023-01-01 .. 2023-03-10, coverage + signal + elev rows every day.
	d0 := DayOf(time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	days := 68
	var cov []CoverageInc
	var sig []SignalHistInc
	var elev []ElevHistInc
	for d := 0; d < days; d++ {
		for _, ant := range []string{"s1", "s2/0"} {
			cov = append(cov, CoverageInc{Day: d0 + d, Ant: ant, BrgBin: 5, AltBand: 2, MaxKm: float64(d), Count: 2, RSSISum: -40, RSSIN: 2})
			sig = append(sig, SignalHistInc{Day: d0 + d, Ant: ant, DistBin: 3, RSSIBin: -20, Count: 3})
			elev = append(elev, ElevHistInc{Day: d0 + d, Ant: ant, ElevBin: 10, Count: 1, RSSISum: -20, ResidSum: 1})
		}
	}
	s.MergeCoverage(cov)
	s.MergeSignalHist(sig)
	s.MergeElevHist(elev)
	mustFlush(t, s)
	totals := func() (int64, int64, int64, float64) {
		return scalar[int64](t, s, `SELECT SUM(count) FROM coverage`), scalar[int64](t, s, `SELECT SUM(count) FROM signal_hist`),
			scalar[int64](t, s, `SELECT SUM(count) FROM elev_hist`), scalar[float64](t, s, `SELECT MAX(max_km) FROM coverage WHERE ant='s1'`)
	}
	c0, s0, e0, m0 := totals()
	// Keep 20 days per day: everything before (2023-03-10 - 20 d) = 2023-02-18
	// is bucketed: January fully, February 2..17.
	now := DayStart(d0+days) + 3600
	rep, err := s.maintainAt(bg, TierPolicy{HistDailyDays: 20, OptimizeEvery: 1000 * time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	c1, s1, e1, m1 := totals()
	if c1 != c0 || s1 != s0 || e1 != e0 || m1 != m0 {
		t.Fatalf("totals changed: %d/%d %d/%d %d/%d %v/%v", c0, c1, s0, s1, e0, e1, m0, m1)
	}
	feb1 := DayOf(time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC).Unix())
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM coverage WHERE ant='s1' AND day < ?`, feb1); n != 1 {
		t.Fatalf("january rows: %d", n)
	}
	if n := scalar[int](t, s, `SELECT count FROM coverage WHERE ant='s1' AND day = ?`, d0); n != 62 {
		t.Fatalf("january bucket count %d", n)
	}
	if mk := scalar[float64](t, s, `SELECT max_km FROM coverage WHERE ant='s1' AND day = ?`, d0); mk != 30 {
		t.Fatalf("january bucket max %v", mk)
	}
	cut := DayOf(now - 20*86400)
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM coverage WHERE ant='s1' AND day >= ? AND day < ?`, feb1, cut); n != 1 {
		t.Fatalf("february rows before the cut: %d", n)
	}
	if rep.Bucketed["coverage"] == 0 || rep.Bucketed["signal_hist"] == 0 || rep.Bucketed["elev_hist"] == 0 {
		t.Fatalf("report %+v", rep)
	}
	// Second run: nothing more to do; later the rest of February joins.
	rep2, err := s.maintainAt(bg, TierPolicy{HistDailyDays: 20, OptimizeEvery: 1000 * time.Hour}, now)
	if err != nil || rep2.Bucketed["coverage"] != 0 {
		t.Fatalf("second run %+v %v", rep2, err)
	}
	if _, err := s.maintainAt(bg, TierPolicy{HistDailyDays: 20, OptimizeEvery: 1000 * time.Hour}, now+15*86400); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM coverage WHERE ant='s2/0' AND day >= ? AND day < ?`, feb1, feb1+28); n != 1 {
		t.Fatalf("february not fully bucketed: %d", n)
	}
	if c2, _, _, _ := totals(); c2 != c0 {
		t.Fatal("totals changed on the second bucketing")
	}
	// Retention of histograms by HistDays.
	if _, err := s.maintainAt(bg, TierPolicy{HistDays: 30, OptimizeEvery: 1000 * time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM coverage WHERE day < ?`, DayOf(now-30*86400)); n != 0 {
		t.Fatal("old histogram rows kept")
	}
}

func TestMaintainMinuteTierAndHeat(t *testing.T) {
	dir := t.TempDir()
	st := openStationsTest(t, dir)
	now := int64(1_700_000_000)
	old, young := now-40*86400, now-86400
	nan := math.NaN()
	for _, tt := range []int64{old, young} {
		st.AddMinuteStats([]StatsRow{{T: tt, Ant: "s1", Frames: 1, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan}})
		st.AddStationMinutes([]StationMinute{{T: tt, SID: "A", Frames: 1, UpSecs: nan, RTTMs: nan}})
		st.AddReceiverStatus([]ReceiverStatus{{T: tt, Feed: "st/A/1", JSON: "{}"}})
	}
	var heat []HeatInc
	for c := int64(0); c < heatChunk+500; c++ { // more than one chunk
		lt := young
		if c%2 == 0 {
			lt = old
		}
		heat = append(heat, HeatInc{Cell: c, Ant: "s1", Count: 1, MinAlt: nan, MaxAlt: nan, LastT: lt})
	}
	st.MergeHeat(heat)
	mustFlush(t, st)
	p := DefaultTierPolicy()
	p.HeatIdleDays = 30
	rep, err := st.maintainAt(bg, p, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted["stats_minute"] != 1 || rep.Deleted["station_minute"] != 1 || rep.Deleted["receiver_status"] != 1 {
		t.Fatalf("minute tier %+v", rep.Deleted)
	}
	if rep.Deleted["heat"] != int64((heatChunk+500+1)/2) {
		t.Fatalf("heat deleted %d", rep.Deleted["heat"])
	}
	if n := scalar[int](t, st, `SELECT COUNT(*) FROM heat`); n != (heatChunk+500)/2 {
		t.Fatalf("heat left %d", n)
	}
	if !rep.Optimized {
		t.Fatal("optimize did not run")
	}
	// The heat walk runs at most daily, optimize at most hourly.
	rep2, err := st.maintainAt(bg, p, now+1800)
	if err != nil || rep2.Optimized || rep2.Deleted["heat"] != 0 {
		t.Fatalf("second run %+v %v", rep2, err)
	}
}

func TestDeleteStationData(t *testing.T) {
	dir := t.TempDir()
	st := openStationsTest(t, dir)
	main := openDir(t, dir, manual)
	// A registry-like table with a sid column must never be touched.
	if err := st.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE stations(idx INTEGER PRIMARY KEY, sid TEXT); INSERT INTO stations VALUES(7, 'SEVEN')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	nan := math.NaN()
	var rows []StatsRow
	for _, ant := range []string{"s7", "s7/0", "s7/1", "s70", "s8"} {
		for i := int64(0); i < 3000; i++ {
			rows = append(rows, StatsRow{T: 1_700_000_000 + i*60, Ant: ant, Frames: 1, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan})
		}
	}
	st.AddMinuteStats(rows)
	st.MergeHeat([]HeatInc{{Cell: 1, Ant: "s7/0", Count: 1, MinAlt: nan, MaxAlt: nan}, {Cell: 1, Ant: "s70", Count: 1, MinAlt: nan, MaxAlt: nan}})
	st.AddStationMinutes([]StationMinute{{T: 1, SID: "SEVEN", UpSecs: nan, RTTMs: nan}, {T: 1, SID: "OTHER", UpSecs: nan, RTTMs: nan}})
	st.AddReceiverStatus([]ReceiverStatus{{T: 1, Feed: "st/SEVEN/1", JSON: "{}"}, {T: 1, Feed: "st/SEVENX/1", JSON: "{}"}})
	mustFlush(t, st)
	main.UpsertFlightStation([]FlightStation{{FlightID: 1, SID: "SEVEN", MaxDistKm: nan, MinDistKm: nan, RSSIMin: nan, RSSIMax: nan, RSSIAvg: nan},
		{FlightID: 1, SID: "OTHER", MaxDistKm: nan, MinDistKm: nan, RSSIMin: nan, RSSIMax: nan, RSSIAvg: nan}})
	main.AddMLATMinutes([]MLATMinute{{T: 60, SID: "SEVEN", SyncedSecs: nan, SigmaNs: nan, ResidRMSNs: nan, DriftPPM: nan}})
	mustFlush(t, main)

	k := StationKeys{Prefix: "s7", SID: "SEVEN"}
	got, err := st.DeleteStationData(bg, k)
	if err != nil {
		t.Fatal(err)
	}
	if got["stats_minute"] != 9000 || got["heat"] != 1 || got["station_minute"] != 1 || got["receiver_status"] != 1 {
		t.Fatalf("deleted %+v", got)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM stats_minute`:                  6000,
		`SELECT COUNT(*) FROM stats_minute WHERE ant='s70'`:  3000,
		`SELECT COUNT(*) FROM heat`:                          1,
		`SELECT COUNT(*) FROM station_minute`:                1,
		`SELECT COUNT(*) FROM receiver_status`:               1,
		`SELECT COUNT(*) FROM stations`:                      1,
		`SELECT COUNT(*) FROM station_clock_minute`:          0,
		`SELECT COUNT(*) FROM stats_minute WHERE ant='s7/1'`: 0,
	} {
		if n := scalar[int](t, st, q); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
	got2, err := main.DeleteStationData(bg, k)
	if err != nil || got2["flight_station"] != 1 || got2["mlat_minute"] != 1 {
		t.Fatalf("main %+v %v", got2, err)
	}
	if _, err := st.DeleteStationData(bg, StationKeys{Prefix: "bogus"}); err == nil {
		t.Fatal("invalid keys accepted")
	}
}

func TestExportStationData(t *testing.T) {
	dir := t.TempDir()
	st := openStationsTest(t, dir)
	nan := math.NaN()
	st.AddMinuteStats([]StatsRow{{T: 1_700_000_000, Ant: "s3", Frames: 5, MaxKm: 10, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan},
		{T: 1_700_000_000, Ant: "s4", Frames: 6, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan}})
	hist := make([]uint32, DistHistBins)
	hist[2] = 9
	st.AddRangeHours([]RangeHour{{T: 1_700_000_000, Ant: "s3/0", MaxKm: 12, P99Km: nan, P95Km: nan, P90Km: nan, P50Km: nan, ResidDB: nan,
		DistHist: hist, SectorMax: make([]float32, SectorBins)}})
	if _, err := st.RollupHours(bg); err != nil {
		t.Fatal(err)
	}
	files := map[string]any{}
	add := func(name string, v any) error { files[name] = v; return nil }
	if err := st.ExportStationData(bg, ExportSpec{Keys: StationKeys{Prefix: "s3", SID: "X"}}, add); err != nil {
		t.Fatal(err)
	}
	if _, ok := files["stations/stats_minute.json"]; ok {
		t.Fatal("minute tier exported without Minutes")
	}
	hr, ok := files["stations/stats_hour.json"].([]map[string]any)
	if !ok || len(hr) != 1 || hr[0]["ant"] != "s3" {
		t.Fatalf("stats_hour %+v", files["stations/stats_hour.json"])
	}
	rh := files["stations/range_hour.json"].([]map[string]any)
	if h, ok := rh[0]["dist_hist"].([]uint32); !ok || h[2] != 9 {
		t.Fatalf("decoded hist %+v", rh[0]["dist_hist"])
	}
	sum := files["stations/_export.json"].(ExportSummary)
	if sum.Rows["stats_hour"] != 1 || sum.Database != "stations" {
		t.Fatalf("summary %+v", sum)
	}
	files = map[string]any{}
	if err := st.ExportStationData(bg, ExportSpec{Keys: StationKeys{Prefix: "s3"}, Minutes: true, MaxRows: 1}, add); err != nil {
		t.Fatal(err)
	}
	if mr := files["stations/stats_minute.json"].([]map[string]any); len(mr) != 1 || mr[0]["frames"] != int64(5) {
		t.Fatalf("minute export %+v", mr)
	}
}

func TestTableSizes(t *testing.T) {
	s := openTest(t, manual)
	sizes, err := s.TableSizes(bg)
	if err != nil || len(sizes) == 0 {
		t.Fatalf("%v %v", sizes, err)
	}
	seen := map[string]bool{}
	for _, z := range sizes {
		seen[z.Name] = true
		if z.Bytes <= 0 {
			t.Fatalf("size %+v", z)
		}
	}
	if !seen["flights"] || seen["flights_icao"] {
		t.Fatalf("indexes must be folded into their table: %v", seen)
	}
}

func TestMonthBucket(t *testing.T) {
	d := DayOf(time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC).Unix())
	f, n := monthBucket(d)
	if DayStart(f) != time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC).Unix() || DayStart(n) != time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatal("feb 2024")
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	s := openTest(t, manual)
	if _, err := s.Maintain(ctx, DefaultTierPolicy()); err == nil {
		t.Fatal("cancelled maintain must fail")
	}
}

func defaultStorage() model.StorageConfig { return model.DefaultConfig().Storage }

// TestStationsReadersProbeKeys: per-station reads on stations.db must
// search the (t, ant) key per step, never scan the time range of all
// stations.
func TestStationsReadersProbeKeys(t *testing.T) {
	st := openStationsTest(t, t.TempDir())
	nan := math.NaN()
	st.AddMinuteStats([]StatsRow{{T: 1_700_000_000, Ant: "s2", Frames: 1, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan},
		{T: 1_700_003_600, Ant: "s3", Frames: 1, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan}})
	mustFlush(t, st)
	for _, table := range []string{"stats_minute", "stats_hour", "range_hour"} {
		with, cond, args, err := st.timeCond(bg, st.DB(), table, "t", 0, 1<<62)
		if err != nil {
			t.Fatal(err)
		}
		if table == "stats_minute" && with == "" {
			t.Fatal("stats_minute must use a series")
		}
		if with == "" {
			continue // empty table
		}
		rows, err := st.DB().Query(`EXPLAIN QUERY PLAN `+with+`SELECT * FROM `+table+` WHERE `+cond+` AND ant = ?`, append(args, "s2")...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var a, b, c int
			var d string
			if err := rows.Scan(&a, &b, &c, &d); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, d)
		}
		rows.Close()
		if !strings.Contains(strings.Join(plan, "|"), "(t=? AND ant=?)") {
			t.Fatalf("%s plan: %v", table, plan)
		}
	}
	rows, err := st.MinuteStats(bg, "s2", 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %+v", err, rows)
	}
	if w, c, _ := SeriesIn("x", "t", 60, 100, 50); w != "" || c != "0" {
		t.Fatal("empty series")
	}
	if w, _, _ := SeriesIn("x", "t", 1, 0, 1e9); w != "" {
		t.Fatal("huge series must fall back to a range")
	}
}

func TestCompactBlobs(t *testing.T) {
	h := make([]uint32, DistHistBins)
	h[0], h[3], h[4], h[100], h[119] = 1, 300, 70000, 1<<31, 5
	c := EncodeDistHistCompact(h)
	if len(c)%4 == 0 || len(c) > 40 {
		t.Fatalf("compact len %d", len(c))
	}
	got := DecodeDistHist(c)
	for i := range h {
		if got[i] != h[i] {
			t.Fatalf("bin %d: %d != %d", i, got[i], h[i])
		}
	}
	s := []float32{0, 12.34, 500, float32(math.NaN()), -3}
	gs := DecodeSectorMax(EncodeSectorMaxCompact(s))
	if len(gs) != 5 || gs[0] != 0 || math.Abs(float64(gs[1])-12.3) > 1e-4 || gs[2] != 500 || gs[3] != 0 || gs[4] != 0 {
		t.Fatalf("sectors %v", gs)
	}
	// Raw arrays are still decoded as raw, also when they start with the magic.
	raw := EncodeDistHist([]uint32{compactMagic, 2})
	if r := DecodeDistHist(raw); len(r) != 2 || r[0] != compactMagic {
		t.Fatalf("raw %v", r)
	}
	// stations.db stores range_hour compactly (airview.db too since 1.1); both read back.
	dir := t.TempDir()
	st := openStationsTest(t, dir)
	mn := openDir(t, dir, manual)
	nan := math.NaN()
	for _, s := range []*Store{st, mn} {
		s.AddRangeHours([]RangeHour{{T: 3600 * 500000, Ant: "s5", MaxKm: 9, P99Km: nan, P95Km: nan, P90Km: nan, P50Km: nan,
			ResidDB: nan, DistHist: h, SectorMax: make([]float32, SectorBins)}})
		mustFlush(t, s)
		rh, err := s.RangeHours(bg, "s5", 0, 0)
		if err != nil || len(rh) != 1 || rh[0].DistHist[4] != 70000 {
			t.Fatalf("%v %+v", err, rh)
		}
	}
	if n := scalar[int](t, st, `SELECT length(dist_hist) FROM range_hour`); n >= 4*DistHistBins {
		t.Fatalf("stations.db blob not compact: %d", n)
	}
	// airview.db: compact since 1.1 (design §3.11), raw in 1.0.0 files;
	// RangeHours above reads either.
	if n := scalar[int](t, mn, `SELECT length(dist_hist) FROM range_hour`); n > 4*DistHistBins {
		t.Fatalf("airview.db blob larger than raw: %d", n)
	}
}

func TestMaintainFlightStationTier(t *testing.T) {
	s := openTest(t, manual)
	now := int64(1_700_000_000)
	nan := math.NaN()
	var fl []Flight
	var fs []FlightStation
	for i := 0; i < 450; i++ { // more than two chunks
		f := NewFlight(int64(i+1), 0x400000, now-int64(i)*86400/2)
		f.End = f.Start + 600
		fl = append(fl, f)
		fs = append(fs, FlightStation{FlightID: f.ID, SID: "A", MaxDistKm: nan, MinDistKm: nan, RSSIMin: nan, RSSIMax: nan, RSSIAvg: nan},
			FlightStation{FlightID: f.ID, SID: "B", MaxDistKm: nan, MinDistKm: nan, RSSIMin: nan, RSSIMax: nan, RSSIAvg: nan})
	}
	s.UpsertFlights(fl)
	s.UpsertFlightStation(fs)
	mustFlush(t, s)
	p := TierPolicy{FlightStationDays: 90, OptimizeEvery: 1000 * time.Hour}
	rep, err := s.maintainAt(bg, p, now)
	if err != nil {
		t.Fatal(err)
	}
	want := scalar[int](t, s, `SELECT COUNT(*) FROM flights WHERE "end" < ?`, now-90*86400)
	if rep.Deleted["flight_station"] != int64(2*want) || want < 250 {
		t.Fatalf("deleted %d, want %d", rep.Deleted["flight_station"], 2*want)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM flights`); n != 450 {
		t.Fatal("flights must stay")
	}
	rep2, err := s.maintainAt(bg, p, now+86400)
	if err != nil || rep2.Deleted["flight_station"] != 4 {
		t.Fatalf("next day %+v %v", rep2.Deleted, err)
	}
}
