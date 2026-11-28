package routes

import "airview/internal/store"

// MigrationName owns the routes_* tables in airview.db.
const MigrationName = "routes"

// migrationV1 holds the tables of docs/ROUTES-DESIGN.md §5 that this
// release uses. Online answers stay in navdata_route_cache (expiring,
// restricted class) and are never copied here.
const migrationV1 = `
CREATE TABLE IF NOT EXISTS routes_override(
  callsign TEXT PRIMARY KEY, airports TEXT NOT NULL, mode TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '', author TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL, expires INTEGER
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS routes_override_log(
  id INTEGER PRIMARY KEY, callsign TEXT NOT NULL, before TEXT NOT NULL DEFAULT '', after TEXT NOT NULL DEFAULT '',
  author TEXT NOT NULL DEFAULT '', t INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS routes_obs(
  callsign TEXT NOT NULL, dep TEXT NOT NULL, arr TEXT NOT NULL, day INTEGER NOT NULL,
  n INTEGER NOT NULL, w REAL NOT NULL, tod_sin REAL NOT NULL, tod_cos REAL NOT NULL,
  last_t INTEGER NOT NULL, events INTEGER NOT NULL,
  PRIMARY KEY(callsign, dep, arr, day)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS routes_obs_day ON routes_obs(day);
CREATE TABLE IF NOT EXISTS routes_learned(
  callsign TEXT NOT NULL, rank INTEGER NOT NULL, airports TEXT NOT NULL, p REAL NOT NULL, n_days INTEGER NOT NULL,
  last_t INTEGER NOT NULL, tod_min INTEGER NOT NULL, stale_standing INTEGER NOT NULL,
  PRIMARY KEY(callsign, rank)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS routes_leg(
  flight INTEGER NOT NULL, seq INTEGER NOT NULL, start INTEGER NOT NULL, "end" INTEGER NOT NULL, callsign TEXT NOT NULL,
  dep TEXT NOT NULL, dep_conf REAL NOT NULL, dep_tier TEXT NOT NULL,
  arr TEXT NOT NULL, arr_conf REAL NOT NULL, arr_tier TEXT NOT NULL,
  route TEXT NOT NULL, route_src TEXT NOT NULL,
  PRIMARY KEY(flight, seq)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS routes_leg_start ON routes_leg(start);
CREATE TABLE IF NOT EXISTS routes_meta(k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
`

func init() {
	if err := store.RegisterMigration(MigrationName, 1, migrationV1); err != nil {
		panic(err)
	}
}
