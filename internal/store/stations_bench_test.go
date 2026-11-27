package store

import (
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"
)

// Scale benchmarks of stations.db (DESIGN §20.2: 1,000+ feeders).

// BenchmarkStationsMinuteFlush measures the write path of the lanes: one op
// is one flush of a minute row for each of 1,000 station keys (the lanes
// spread these over 10 s round-robin; one op is ~10 s of a 1,000-feeder
// network's minute statistics).
func BenchmarkStationsMinuteFlush(b *testing.B) {
	o := manual
	o.Logger = quiet()
	s, err := OpenStations(b.TempDir(), o)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	const stations = 1000
	rows := make([]StatsRow, stations)
	t0 := time.Now().Unix() / 60 * 60
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for k := range rows {
			rows[k] = StatsRow{T: t0 + int64(i/6)*60, Ant: fmt.Sprintf("s%d", k+2), Frames: 3000, DF17: 1500, DF11: 900,
				Positions: 300, Aircraft: 40, AircraftPos: 30, ADSB: 30, MaxKm: 310, RSSIAvg: -18, RSSIMin: -33, RSSIMax: -2, Secs: 10}
		}
		bt := NewBatch()
		bt.AddMinuteStats(rows)
		s.Submit(bt)
		if err := s.Flush(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N*stations)/b.Elapsed().Seconds(), "rows/s")
}

// fillHours writes `days` days of stats_hour rows for `stations` keys with
// one INSERT ... SELECT (fast fixture for the read benchmarks).
func fillHours(b *testing.B, s *Store, stations, days int, t0 int64) {
	b.Helper()
	err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE k(i) AS (SELECT 2 UNION ALL SELECT i + 1 FROM k WHERE i < ?),
h(t) AS (SELECT ? UNION ALL SELECT t + 3600 FROM h WHERE t + 3600 < ?)
INSERT INTO stats_hour(t, ant, frames, positions, aircraft, minutes, max_km, secs)
SELECT h.t, 's' || k.i, 100000, 10000, 40, 60, 300, 3600 FROM h, k`, stations+1, t0, t0+int64(days)*86400)
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
}

// BenchmarkStationHourStats30d reads 30 days of hourly statistics of one
// station from a stations.db holding 1,000 stations × 30 days (720,000
// rows): "per-station probe" uses SeriesIn (stations.db readers), "range
// scan" the plain range condition used on airview.db.
func BenchmarkStationHourStats30d(b *testing.B) {
	o := manual
	o.Logger = quiet()
	s, err := OpenStations(b.TempDir(), o)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	t0 := int64(1_700_000_000) / 86400 * 86400
	fillHours(b, s, 1000, 30, t0)
	b.Run("per-station probe", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			rows, err := s.HourStats(bg, "s517", t0, t0+30*86400)
			if err != nil || len(rows) != 720 {
				b.Fatalf("%d %v", len(rows), err)
			}
		}
	})
	b.Run("per-station probe count", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			with, cond, args := SeriesIn("ts", "t", 3600, t0, t0+30*86400)
			var n int
			err := s.DB().QueryRow(with+`SELECT COUNT(*), SUM(frames) FROM stats_hour WHERE `+cond+` AND ant = ?`,
				append(args, "s517")...).Scan(&n, new(sql.NullInt64))
			if err != nil || n != 720 {
				b.Fatalf("%d %v", n, err)
			}
		}
	})
	b.Run("range scan", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var n int
			err := s.DB().QueryRow(`SELECT COUNT(*), SUM(frames) FROM stats_hour WHERE ant = ? AND t >= ? AND t <= ?`,
				"s517", t0, t0+30*86400).Scan(&n, new(sql.NullInt64))
			if err != nil || n != 720 {
				b.Fatalf("%d %v", n, err)
			}
		}
	})
}

// BenchmarkMaintainSteadyState is the cost of the routine (hourly)
// Maintain call on a 1,000-station database with nothing to reduce.
func BenchmarkMaintainSteadyState(b *testing.B) {
	o := manual
	o.Logger = quiet()
	s, err := OpenStations(b.TempDir(), o)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	t0 := (time.Now().Unix() - 20*86400) / 86400 * 86400
	fillHours(b, s, 1000, 20, t0)
	nan := math.NaN()
	s.AddMinuteStats([]StatsRow{{T: time.Now().Unix(), Ant: "s2", Frames: 1, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan}})
	p := DefaultTierPolicy()
	if _, err := s.Maintain(bg, p); err != nil { // first run: day rollup of the fixture
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Maintain(bg, p); err != nil {
			b.Fatal(err)
		}
	}
}
