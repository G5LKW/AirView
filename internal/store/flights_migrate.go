package store

// Flights indexes (DB-EFFICIENCY-DESIGN §3.10), migration "flights" 2 (the
// columns dep / arr were core migration 2, hence version 2 of the flights
// indexes):
//
//   - flights_unresolved (a partial index on "end" WHERE dep IS NULL) is
//     dropped: on a station without navigation data it was a full copy of
//     flights_end. UnresolvedFlights walks flights_end instead
//     (airports.go).
//   - flights_dep / flights_arr become partial indexes WHERE ... IS NOT
//     NULL: the report filters "dep IN (...)" / "arr IN (...)" imply NOT
//     NULL, so SQLite still uses them, and the unresolved flights (all of
//     them without navigation data) cost no index entry.
//   - events_icao: the per-aircraft event list (Events with ICAO) was a
//     full scan of events, which grow forever.
//
// One transaction; on 2 years of flights (3.4 M rows) the two index builds
// take seconds on a Pi.
const flightsSchemaV2 = `
DROP INDEX IF EXISTS flights_unresolved;
DROP INDEX IF EXISTS flights_dep;
DROP INDEX IF EXISTS flights_arr;
CREATE INDEX flights_dep ON flights(dep, start) WHERE dep IS NOT NULL;
CREATE INDEX flights_arr ON flights(arr, start) WHERE arr IS NOT NULL;
CREATE INDEX IF NOT EXISTS events_icao ON events(icao, id);
`

func init() {
	if err := RegisterMigrationFor(KindMain, "flights", 2, flightsSchemaV2); err != nil {
		panic(err)
	}
}
