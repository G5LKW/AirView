package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationFlightAirports opens a database that has only the core v1
// schema (a pre-v2 install with one closed flight) and checks that the
// dep / arr columns and indexes are added with NULL values, and that the
// resolver API round-trips.
func TestMigrationFlightAirports(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaMigrationsDDL, coreSchemaV1,
		`INSERT INTO schema_migrations(name, version, applied) VALUES('core', 1, 0)`,
		`INSERT INTO flights(id, icao, callsign, start, "end") VALUES(7, 4660, 'OLD1', 1000, 2000)`,
		`INSERT INTO flights(id, icao, callsign, start, "end") VALUES(8, 4661, 'OLD2', 1500, 2500)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%.40s: %v", q, err)
		}
	}
	_ = db.Close()

	s := openDir(t, dir, manual)
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM schema_migrations WHERE name = 'core'`); n != len(coreMigrations) {
		t.Fatalf("core migrations: %d", n)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM flights WHERE dep IS NULL AND arr IS NULL`); n != 2 {
		t.Fatalf("unresolved after migration: %d", n)
	}
	// Migration "flights" 2: flights_unresolved gone, partial dep / arr
	// indexes, events_icao.
	for _, ix := range []string{"flights_dep", "flights_arr", "events_icao"} {
		if n := scalar[int](t, s, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, ix); n != 1 {
			t.Fatalf("index %s missing", ix)
		}
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'flights_unresolved'`); n != 0 {
		t.Fatal("flights_unresolved still exists")
	}
	if sql := scalar[string](t, s, `SELECT sql FROM sqlite_master WHERE name = 'flights_dep'`); !strings.Contains(sql, "WHERE dep IS NOT NULL") {
		t.Fatalf("flights_dep is not partial: %s", sql)
	}
	ids, err := s.UnresolvedFlights(bg, 3000, 10)
	if err != nil || len(ids) != 2 || ids[0] != 8 || ids[1] != 7 {
		t.Fatalf("unresolved = %v, %v", ids, err)
	}
	if ids, _ := s.UnresolvedFlights(bg, 2200, 10); len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("endBefore not applied: %v", ids)
	}
	if err := s.SetFlightAirports(bg, []FlightAirports{{ID: 7, Dep: "EDDF", Arr: "EGLL"}, {ID: 8}}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.UnresolvedFlights(bg, 3000, 10); len(ids) != 0 {
		t.Fatalf("still unresolved: %v", ids)
	}
	f, ok, err := s.FlightByID(bg, 7)
	if err != nil || !ok || f.Dep != "EDDF" || f.Arr != "EGLL" {
		t.Fatalf("flight 7 = %+v %v %v", f, ok, err)
	}
	if n, err := s.ClearUnmatchedAirports(bg); err != nil || n != 1 {
		t.Fatalf("clear unmatched = %d, %v", n, err)
	}
	if ids, _ := s.UnresolvedFlights(bg, 3000, 10); len(ids) != 1 || ids[0] != 8 {
		t.Fatalf("after clear: %v", ids)
	}

	// An upsert of a resolved flight (the flight continued) makes it
	// unresolved again.
	fl := NewFlight(7, 4660, 1000)
	fl.End = 2600
	s.UpsertFlights([]Flight{fl})
	mustFlush(t, s)
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM flights WHERE id = 7 AND dep IS NULL`); n != 1 {
		t.Fatal("upsert kept stale airports")
	}
}

// The report / airport filters keep using the (now partial) dep / arr
// indexes, and the per-aircraft event list uses events_icao.
func TestFlightsIndexPlans(t *testing.T) {
	s := openTest(t, manual)
	plan := func(q string, args ...any) string {
		t.Helper()
		rs, err := s.DB().Query(`EXPLAIN QUERY PLAN `+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var b strings.Builder
		for rs.Next() {
			var id, parent, unused int
			var detail string
			if err := rs.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			b.WriteString(detail + "; ")
		}
		return b.String()
	}
	for _, c := range []struct{ q, want string }{
		{`SELECT id FROM flights f WHERE f."end" >= 1 AND f.start <= 2 AND f.dep IN ('EDDF', 'FRA')`, "flights_dep"},
		{`SELECT id FROM flights f WHERE f."end" >= 1 AND f.start <= 2 AND f.arr IN ('EGLL')`, "flights_arr"},
		{`SELECT id, t, kind FROM events WHERE 1=1 AND icao = 5 ORDER BY id DESC LIMIT 100`, "events_icao"},
		{`SELECT id, "end" FROM flights WHERE "end" < 5 AND "end" >= 1 AND dep IS NULL ORDER BY "end" DESC LIMIT 10`, "flights_end"},
	} {
		if p := plan(c.q); !strings.Contains(p, c.want) {
			t.Errorf("%s: plan %s does not use %s", c.q, p, c.want)
		}
	}
}

// UnresolvedFlights walks flights_end from a floor: flights resolved once
// are not visited again, new closed flights are found, and clearing the
// unmatched airports re-opens the walk.
func TestUnresolvedFlightsFloor(t *testing.T) {
	s := openTest(t, manual)
	var fl []Flight
	for i := 1; i <= 50; i++ {
		f := NewFlight(int64(i), uint32(i), int64(i*100))
		f.End = int64(i*100 + 50)
		fl = append(fl, f)
	}
	s.UpsertFlights(fl)
	mustFlush(t, s)
	resolveAll := func(endBefore int64) int {
		n := 0
		for {
			ids, err := s.UnresolvedFlights(bg, endBefore, 7)
			if err != nil {
				t.Fatal(err)
			}
			if len(ids) == 0 {
				return n
			}
			var out []FlightAirports
			for _, id := range ids {
				out = append(out, FlightAirports{ID: id, Dep: fmt.Sprint("A", id), Arr: "B"})
			}
			if err := s.SetFlightAirports(bg, out); err != nil {
				t.Fatal(err)
			}
			n += len(ids)
		}
	}
	if n := resolveAll(3000); n != 29 { // ends 150 .. 2950
		t.Fatalf("resolved %d", n)
	}
	if f := unresolvedFloorOf(s.cur.Load()); f != 3000 {
		t.Fatalf("floor %d", f)
	}
	if n := resolveAll(10_000); n != 21 {
		t.Fatalf("second round resolved %d", n)
	}
	// A flight that becomes unresolved again because its row is replaced
	// (resolved flight continued: the 1.0.0 upsert clears dep / arr) has a
	// new end above the floor.
	f := fl[10]
	f.End = 20_000
	s.UpsertFlights([]Flight{f})
	mustFlush(t, s)
	if ids, _ := s.UnresolvedFlights(bg, 30_000, 10); len(ids) != 1 || ids[0] != f.ID {
		t.Fatalf("continued flight: %v", ids)
	}
	if err := s.SetFlightAirports(bg, []FlightAirports{{ID: 3}, {ID: f.ID, Dep: "X", Arr: "Y"}}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ClearUnmatchedAirports(bg); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if ids, _ := s.UnresolvedFlights(bg, 30_000, 10); len(ids) != 1 || ids[0] != 3 {
		t.Fatalf("after clear: %v", ids)
	}
}
