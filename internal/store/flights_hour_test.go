package store

import (
	"database/sql"
	"math/rand/v2"
	"os"
	"reflect"
	"testing"
	"time"

	"airview/internal/model"
)

// legacyFlightDays is the 1.0.0 query.
func legacyFlightDays(t *testing.T, s *Store, from, to, off int64) map[int64]FlightDay {
	t.Helper()
	out := map[int64]FlightDay{}
	rs, err := s.DB().Query(`SELECT (start + ?) / 86400, COUNT(DISTINCT icao), COUNT(*) FROM flights
WHERE start >= ? AND start < ? GROUP BY 1`, off, from, to)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	for rs.Next() {
		var d, u, n int64
		if err := rs.Scan(&d, &u, &n); err != nil {
			t.Fatal(err)
		}
		out[d] = FlightDay{Aircraft: u, Flights: n}
	}
	return out
}

func insertFlights(t *testing.T, s *Store, rng *rand.Rand, id *int64, from, to int64, n int) {
	t.Helper()
	if err := s.Write(bg, func(tx *sql.Tx) error {
		for i := 0; i < n; i++ {
			*id++
			start := from + rng.Int64N(to-from)
			if _, err := tx.Exec(`INSERT INTO flights(id, icao, start, "end") VALUES(?, ?, ?, ?)`,
				*id, 0x400000+rng.IntN(600), start, start+rng.Int64N(7200)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFlightDaysEqualLegacy: the rollup answers what the 1.0.0 query
// answers for whole-hour and half-hour offsets, partial ranges, the open
// day, late flights and after retention.
func TestFlightDaysEqualLegacy(t *testing.T) {
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(5, 6))
	now := int64(1_790_000_000)
	var id int64
	insertFlights(t, s, rng, &id, now-12*86400, now, 6000)

	check := func(label string) {
		t.Helper()
		for _, off := range []int64{0, 3600, 19800, -28800, 34200} {
			for _, rg := range [][2]int64{{now - 30*86400, now + 3600}, {now - 5*86400 + 1234, now - 2*86400 - 77}, {now - 86400, now}} {
				got, err := s.FlightDays(bg, rg[0], rg[1], off)
				if err != nil {
					t.Fatal(err)
				}
				if want := legacyFlightDays(t, s, rg[0], rg[1], off); !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: off %d range %v:\n got %v\nwant %v", label, off, rg, got, want)
				}
			}
		}
	}
	check("before rollup")
	if err := s.rollFlightHours(bg, now); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM flights_hour`); n < 200 {
		t.Fatalf("hour rows: %d", n)
	}
	check("rolled up")

	// A late flight in a rolled-up hour, and new ones in the open part.
	if err := s.Write(bg, func(tx *sql.Tx) error {
		id++
		_, err := tx.Exec(`INSERT INTO flights(id, icao, start, "end") VALUES(?, ?, ?, ?)`, id, 0x123456, now-6*86400+100, now-6*86400+200)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	insertFlights(t, s, rng, &id, now-3600, now, 50)
	check("late flight")
	if err := s.rollFlightHours(bg, now+3600); err != nil {
		t.Fatal(err)
	}
	check("late flight folded in")

	// Retention removes the oldest flights.
	var res PruneResult
	if err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM flights WHERE "end" < ?`, now-9*86400)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check("after retention")
	if err := pruneFlightsHour(bg, s, model.StorageConfig{FlightRetentionDays: 9}, now, &res); err != nil {
		t.Fatal(err)
	}
	if res.Extra[flightsHourTable] == 0 {
		t.Fatalf("nothing pruned: %+v", res)
	}
	check("after rollup prune")
}

func TestICAOSetCodec(t *testing.T) {
	for _, set := range [][]int64{nil, {0}, {5}, {0x400000, 0x400001, 0xffffff, 1 << 30}} {
		b := encodeICAOSet(set)
		got, err := appendICAOSet(nil, b)
		if err != nil || !reflect.DeepEqual(got, set) && len(set) > 0 {
			t.Fatalf("%v -> %v %v", set, got, err)
		}
	}
}

// TestFlightsHourMigration: the backfill migration rolls up an existing
// flights table (a database of the 1.0.0 layout) and reports done.
func TestFlightsHourMigration(t *testing.T) {
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(7, 8))
	var id int64
	now := int64(1_790_000_000)
	insertFlights(t, s, rng, &id, now-40*86400, now-86400, 3000)
	steps := 0
	for done := false; !done; steps++ {
		if err := s.Write(bg, func(tx *sql.Tx) error {
			var err error
			done, err = rollFlightHoursTx(bg, tx, now, flightsHourStep)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if steps < 6 {
		t.Fatalf("40 days in %d steps", steps)
	}
	got, err := s.FlightDays(bg, now-50*86400, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := legacyFlightDays(t, s, now-50*86400, now, 0); !reflect.DeepEqual(got, want) {
		t.Fatal("migrated rollup differs")
	}
}

// TestFlightsTwoYearsMeasure is an opt-in measurement on a synthetic
// two-year flights table (3.4 M flights, 4,600 a day, a third of the
// airframes return):
//
//	AIRVIEW_DBEFF_C_2Y=<empty scratch dir> go test ./internal/store -run TestFlightsTwoYearsMeasure -v
func TestFlightsTwoYearsMeasure(t *testing.T) {
	dir := os.Getenv("AIRVIEW_DBEFF_C_2Y")
	if dir == "" {
		t.Skip("set AIRVIEW_DBEFF_C_2Y to an empty scratch directory")
	}
	s, err := OpenWithOptions(dir, Options{MigrationDelay: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Unix()
	if scalar[int64](t, s, `SELECT COUNT(*) FROM flights`) == 0 {
		rng := rand.New(rand.NewPCG(1, 2))
		var id int64
		t0 := time.Now()
		for d := int64(730); d > 0; d-- {
			if err := s.Write(bg, func(tx *sql.Tx) error {
				st, err := tx.Prepare(`INSERT INTO flights(id, icao, start, "end", msgs) VALUES(?, ?, ?, ?, ?)`)
				if err != nil {
					return err
				}
				defer st.Close()
				for i := 0; i < 4600; i++ {
					id++
					start := now - d*86400 + rng.Int64N(86400)
					icao := 0x100000 + rng.Int64N(0xe00000)
					if rng.IntN(3) == 0 {
						icao = 0x400000 + rng.Int64N(20000)
					}
					if _, err := st.Exec(id, icao, start, start+rng.Int64N(7200), rng.IntN(3000)); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("generated %d flights in %v", id, time.Since(t0))
		t0 = time.Now()
		if err := s.rollFlightHours(bg, now); err != nil {
			t.Fatal(err)
		}
		t.Logf("rollup of two years: %v", time.Since(t0))
	}
	for i := 0; i < 2; i++ {
		t0 := time.Now()
		n := scalar[int64](t, s, `SELECT COUNT(*) FROM flights`)
		t.Logf("round %d: all-time flights %d: COUNT(*) %v", i, n, time.Since(t0))
		for _, off := range []int64{0, 3600, 19800, -28800} {
			from := (now+off)/86400*86400 - off - 29*86400
			t0 = time.Now()
			want := legacyFlightDays(t, s, from, now+3600, off)
			tl := time.Since(t0)
			t0 = time.Now()
			got, err := s.FlightDays(bg, from, now+3600, off)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("offset %d: answers differ (%v)", off, err)
			}
			t.Logf("round %d: 30-day daily aircraft, offset %+d: 1.0.0 query %v, rollup %v (equal)", i, off, tl, time.Since(t0))
		}
	}
	var size int64
	_ = s.DB().QueryRow(`SELECT SUM(pgsize) FROM dbstat WHERE name LIKE 'flights_hour%'`).Scan(&size)
	t.Logf("flights_hour + index: %d rows, %d B", scalar[int64](t, s, `SELECT COUNT(*) FROM flights_hour`), size)
}
