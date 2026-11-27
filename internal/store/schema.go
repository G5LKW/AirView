package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Column lists shared by several tables / statements.

// statsCounterCols are summed when rows are merged or rolled up.
var statsCounterCols = []string{
	"frames", "df0", "df4", "df5", "df11", "df16", "df17", "df18", "df20", "df21",
	"df_other", "crc_bad", "positions", "mlat_positions",
}

// statsGaugeCols take the maximum when rows are merged or rolled up.
var statsGaugeCols = []string{"aircraft", "aircraft_pos", "adsb", "mlat", "modes"}

// weatherCols are the measurement columns shared by weather_obs, weather_fc
// and weather_fc_log, in the order of the Weather struct fields.
var weatherCols = []string{
	"temp_c", "dewpoint_c", "rh", "pressure_msl", "surface_pressure", "wind_kmh", "wind_dir", "gust_kmh",
	"cloud", "cloud_low", "cloud_mid", "cloud_high", "precip_mm", "rain_mm", "snow_cm", "visibility_m",
	"cape", "weather_code", "is_day", "n_surface", "dn_dh", "k_factor", "duct",
}

func statsColsDDL() string {
	var b strings.Builder
	for _, c := range statsCounterCols {
		b.WriteString("  " + c + " INTEGER NOT NULL DEFAULT 0,\n")
	}
	for _, c := range statsGaugeCols {
		b.WriteString("  " + c + " INTEGER NOT NULL DEFAULT 0,\n")
	}
	b.WriteString("  max_km REAL,\n  rssi_avg REAL,\n  rssi_min REAL,\n  rssi_max REAL")
	return b.String()
}

func weatherColsDDL() string {
	var b strings.Builder
	for i, c := range weatherCols {
		if i > 0 {
			b.WriteString(",\n")
		}
		typ := "REAL"
		if c == "weather_code" || c == "is_day" {
			typ = "INTEGER"
		}
		b.WriteString("  " + c + " " + typ)
	}
	return b.String()
}

// coreSchemaV1 is the complete core schema (migration "core" version 1).
// See doc.go for the column reference.
var coreSchemaV1 = `
CREATE TABLE kv(
  key     TEXT PRIMARY KEY,
  value   TEXT NOT NULL,
  updated INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE aircraft(
  icao        INTEGER PRIMARY KEY,
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  callsign    TEXT NOT NULL DEFAULT '',
  reg         TEXT NOT NULL DEFAULT '',
  type        TEXT NOT NULL DEFAULT '',
  operator    TEXT NOT NULL DEFAULT '',
  country     TEXT NOT NULL DEFAULT '',
  category    TEXT NOT NULL DEFAULT '',
  msgs        INTEGER NOT NULL DEFAULT 0,
  flights     INTEGER NOT NULL DEFAULT 0,
  positions   INTEGER NOT NULL DEFAULT 0,
  max_dist_km REAL NOT NULL DEFAULT 0,
  max_alt     INTEGER,
  src_mask    INTEGER NOT NULL DEFAULT 0,
  flags       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX aircraft_last_seen ON aircraft(last_seen);

CREATE TABLE flights(
  id           INTEGER PRIMARY KEY,
  icao         INTEGER NOT NULL,
  callsign     TEXT NOT NULL DEFAULT '',
  squawk       TEXT NOT NULL DEFAULT '',
  start        INTEGER NOT NULL,
  "end"        INTEGER NOT NULL,
  msgs         INTEGER NOT NULL DEFAULT 0,
  positions    INTEGER NOT NULL DEFAULT 0,
  src_mask     INTEGER NOT NULL DEFAULT 0,
  max_dist_km  REAL,
  max_dist_brg REAL,
  min_dist_km  REAL,
  min_alt      INTEGER,
  max_alt      INTEGER,
  max_gs       REAL,
  rssi_min     REAL,
  rssi_max     REAL,
  rssi_avg     REAL,
  first_lat    REAL,
  first_lon    REAL,
  last_lat     REAL,
  last_lon     REAL,
  emergency    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX flights_icao ON flights(icao, start);
CREATE INDEX flights_start ON flights(start);
CREATE INDEX flights_end ON flights("end");

CREATE TABLE positions(
  flight_id INTEGER NOT NULL,
  t         INTEGER NOT NULL,
  lat_e5    INTEGER NOT NULL,
  lon_e5    INTEGER NOT NULL,
  alt       INTEGER,
  gs        INTEGER,
  track     INTEGER,
  rssi_x10  INTEGER,
  src       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(flight_id, t)
) WITHOUT ROWID;

CREATE TABLE stats_minute(
  t   INTEGER NOT NULL,
  ant TEXT NOT NULL,
` + statsColsDDL() + `,
  PRIMARY KEY(t, ant)
) WITHOUT ROWID;

CREATE TABLE stats_hour(
  t   INTEGER NOT NULL,
  ant TEXT NOT NULL,
` + statsColsDDL() + `,
  aircraft_avg REAL,
  minutes      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(t, ant)
) WITHOUT ROWID;

CREATE TABLE range_hour(
  t          INTEGER NOT NULL,
  ant        TEXT NOT NULL,
  max_km     REAL,
  p99_km     REAL,
  p95_km     REAL,
  p90_km     REAL,
  p50_km     REAL,
  positions  INTEGER NOT NULL DEFAULT 0,
  aircraft   INTEGER NOT NULL DEFAULT 0,
  far_count  INTEGER NOT NULL DEFAULT 0,
  resid_db   REAL,
  resid_n    INTEGER NOT NULL DEFAULT 0,
  dist_hist  BLOB,
  sector_max BLOB,
  PRIMARY KEY(t, ant)
);

CREATE TABLE coverage(
  day      INTEGER NOT NULL,
  ant      TEXT NOT NULL,
  brg_bin  INTEGER NOT NULL,
  alt_band INTEGER NOT NULL,
  max_km   REAL NOT NULL DEFAULT 0,
  count    INTEGER NOT NULL DEFAULT 0,
  rssi_sum REAL NOT NULL DEFAULT 0,
  rssi_n   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(day, ant, brg_bin, alt_band)
) WITHOUT ROWID;

CREATE TABLE signal_hist(
  day      INTEGER NOT NULL,
  ant      TEXT NOT NULL,
  dist_bin INTEGER NOT NULL,
  rssi_bin INTEGER NOT NULL,
  count    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(day, ant, dist_bin, rssi_bin)
) WITHOUT ROWID;

CREATE TABLE elev_hist(
  day       INTEGER NOT NULL,
  ant       TEXT NOT NULL,
  elev_bin  INTEGER NOT NULL,
  count     INTEGER NOT NULL DEFAULT 0,
  rssi_sum  REAL NOT NULL DEFAULT 0,
  resid_sum REAL NOT NULL DEFAULT 0,
  PRIMARY KEY(day, ant, elev_bin)
) WITHOUT ROWID;

CREATE TABLE heat(
  cell     INTEGER NOT NULL,
  ant      TEXT NOT NULL,
  count    INTEGER NOT NULL DEFAULT 0,
  rssi_sum REAL NOT NULL DEFAULT 0,
  rssi_n   INTEGER NOT NULL DEFAULT 0,
  min_alt  INTEGER,
  max_alt  INTEGER,
  last_t   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(cell, ant)
) WITHOUT ROWID;

CREATE TABLE weather_obs(
  t INTEGER PRIMARY KEY,
` + weatherColsDDL() + `,
  profile_json TEXT
);

CREATE TABLE weather_fc(
  t      INTEGER PRIMARY KEY,
  issued INTEGER NOT NULL,
` + weatherColsDDL() + `,
  profile_json TEXT
);

CREATE TABLE weather_fc_log(
  t      INTEGER NOT NULL,
  lead_h INTEGER NOT NULL,
  issued INTEGER NOT NULL,
` + weatherColsDDL() + `,
  PRIMARY KEY(t, lead_h)
) WITHOUT ROWID;

CREATE TABLE ntp_samples(
  t         INTEGER PRIMARY KEY,
  server    TEXT NOT NULL DEFAULT '',
  offset_ms REAL,
  delay_ms  REAL,
  stratum   INTEGER
);

CREATE TABLE met_obs(
  t        INTEGER NOT NULL,
  icao     INTEGER NOT NULL,
  lat      REAL,
  lon      REAL,
  alt      INTEGER,
  wind_dir REAL,
  wind_kt  REAL,
  oat_c    REAL,
  src      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX met_obs_t ON met_obs(t);

CREATE TABLE met_profile(
  hour   INTEGER NOT NULL,
  level  INTEGER NOT NULL,
  u_sum  REAL NOT NULL DEFAULT 0,
  v_sum  REAL NOT NULL DEFAULT 0,
  t_sum  REAL NOT NULL DEFAULT 0,
  n_wind INTEGER NOT NULL DEFAULT 0,
  n_temp INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(hour, level)
) WITHOUT ROWID;

CREATE TABLE records(
  key         TEXT PRIMARY KEY,
  value       REAL NOT NULL,
  icao        INTEGER,
  t           INTEGER NOT NULL,
  detail_json TEXT
) WITHOUT ROWID;

CREATE TABLE events(
  id          INTEGER PRIMARY KEY,
  t           INTEGER NOT NULL,
  kind        TEXT NOT NULL,
  icao        INTEGER,
  detail_json TEXT
);
CREATE INDEX events_t ON events(t);

CREATE TABLE acdb(
  icao     INTEGER PRIMARY KEY,
  reg      TEXT NOT NULL DEFAULT '',
  type     TEXT NOT NULL DEFAULT '',
  "desc"   TEXT NOT NULL DEFAULT '',
  operator TEXT NOT NULL DEFAULT '',
  flags    INTEGER NOT NULL DEFAULT 0,
  year     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE receiver_status(
  t    INTEGER NOT NULL,
  feed TEXT NOT NULL,
  json TEXT NOT NULL,
  PRIMARY KEY(t, feed)
) WITHOUT ROWID;
`

// coreName is the migration name reserved for this package's own schema.
const coreName = "core"

type migration struct {
	name    string
	version int
	ddl     string
	kinds   Kind // the database kinds the migration applies to
}

// coreMigrations are applied to airview.db (KindMain) before any registered
// migration.
var coreMigrations = []migration{
	{coreName, 1, coreSchemaV1, KindMain},
	{coreName, 2, coreSchemaV2, KindMain},
	{coreName, 3, coreSchemaV3, KindMain},
	{coreName, 4, coreSchemaV4, KindMain},
}

// coreSchemaV4 adds the network tables of aggregator mode to airview.db
// (DESIGN §6.4, §6.6) and the daily statistics tier (§20.2):
//
//   - flight_station: what one station contributed to one flight, written by
//     the tracker's lanes when the flight closes (Batch.UpsertFlightStation);
//   - mlat_minute: per-minute MLAT counters of a station (sid) or of the
//     whole network (empty sid), written by the MLAT engine (Batch.AddMLATMinutes);
//   - mlat_quality_hour: hourly empirical MLAT error per 1° cell and
//     altitude band with a mergeable error histogram (Batch.MergeMLATQuality);
//   - stats_day: daily rollup of stats_hour (Store.RollupDays), the tier that
//     outlives the hourly statistics.
//
// In standalone mode the network tables stay empty.
var coreSchemaV4 = `
CREATE TABLE flight_station(
  flight_id   INTEGER NOT NULL,
  sid         TEXT NOT NULL,
  icao        INTEGER NOT NULL DEFAULT 0,
  first       INTEGER NOT NULL,
  last        INTEGER NOT NULL,
  msgs        INTEGER NOT NULL DEFAULT 0,
  positions   INTEGER NOT NULL DEFAULT 0,
  max_dist_km REAL,
  min_dist_km REAL,
  rssi_min    REAL,
  rssi_max    REAL,
  rssi_avg    REAL,
  PRIMARY KEY(flight_id, sid)
) WITHOUT ROWID;
CREATE INDEX flight_station_sid ON flight_station(sid, first);

` + mlatMinuteDDL + `

CREATE TABLE mlat_quality_hour(
  t        INTEGER NOT NULL,
  cell1deg INTEGER NOT NULL,
  altband  INTEGER NOT NULL,
  n        INTEGER NOT NULL DEFAULT 0,
  p50      REAL,
  p95      REAL,
  p99      REAL,
  err_hist BLOB,
  PRIMARY KEY(t, cell1deg, altband)
) WITHOUT ROWID;

` + statsDayDDL

// mlatMinuteDDL is the per-minute MLAT contribution table (airview.db).
const mlatMinuteDDL = `CREATE TABLE mlat_minute(
  t            INTEGER NOT NULL,
  sid          TEXT NOT NULL,
  frames       INTEGER NOT NULL DEFAULT 0,
  sync_frames  INTEGER NOT NULL DEFAULT 0,
  groups       INTEGER NOT NULL DEFAULT 0,
  fixes        INTEGER NOT NULL DEFAULT 0,
  outliers     INTEGER NOT NULL DEFAULT 0,
  peers        INTEGER NOT NULL DEFAULT 0,
  synced_secs  REAL,
  sigma_ns     REAL,
  resid_rms_ns REAL,
  drift_ppm    REAL,
  PRIMARY KEY(t, sid)
) WITHOUT ROWID;`

// statsDayDDL is the daily statistics tier, shared by both database kinds.
var statsDayDDL = `CREATE TABLE stats_day(
  t   INTEGER NOT NULL,
  ant TEXT NOT NULL,
` + statsColsDDL() + `,
  aircraft_avg REAL,
  minutes      INTEGER NOT NULL DEFAULT 0,
  secs         REAL,
  hours        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(t, ant)
) WITHOUT ROWID;`

// stationsName is the migration name of the stations.db core schema.
const stationsName = "stations"

// stationsMigrations are applied to stations.db (KindStations) before any
// registered migration: the statistics tables of the standalone schema
// (same columns, same writers, keys namespaced 's<idx>' / 's<idx>/<ch>',
// see AntKey) plus the per-station health tables. The registry tables of
// internal/registry live in the same file but are created by that package.
var stationsMigrations = []migration{
	{stationsName, 1, stationsSchemaV1, KindStations},
}

// stationsSchemaV1: kv (rollup marks), the per-antenna statistics tables
// with the columns of core v1-v3 (stats_minute/stats_hour with secs), the
// daily tier, receiver_status, and
//
//   - station_minute(t, sid, ...): link health per minute (ingest);
//   - station_clock_minute(t, sid, ...): clock-filter state per minute (MLAT).
//
// Key order: the time-keyed tables keep (t|day, ant, ...) so retention,
// rollup and month bucketing are range scans; per-station reads probe the
// key once per time step (see SeriesIn). heat, which has no time column, is
// keyed (ant, cell) here so one station's grid is a range scan.
var stationsSchemaV1 = `
CREATE TABLE kv(
  key     TEXT PRIMARY KEY,
  value   TEXT NOT NULL,
  updated INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE stats_minute(
  t   INTEGER NOT NULL,
  ant TEXT NOT NULL,
` + statsColsDDL() + `,
  secs REAL,
  PRIMARY KEY(t, ant)
) WITHOUT ROWID;

CREATE TABLE stats_hour(
  t   INTEGER NOT NULL,
  ant TEXT NOT NULL,
` + statsColsDDL() + `,
  aircraft_avg REAL,
  minutes      INTEGER NOT NULL DEFAULT 0,
  secs         REAL,
  PRIMARY KEY(t, ant)
) WITHOUT ROWID;

` + statsDayDDL + `

CREATE TABLE range_hour(
  t          INTEGER NOT NULL,
  ant        TEXT NOT NULL,
  max_km     REAL,
  p99_km     REAL,
  p95_km     REAL,
  p90_km     REAL,
  p50_km     REAL,
  positions  INTEGER NOT NULL DEFAULT 0,
  aircraft   INTEGER NOT NULL DEFAULT 0,
  far_count  INTEGER NOT NULL DEFAULT 0,
  resid_db   REAL,
  resid_n    INTEGER NOT NULL DEFAULT 0,
  dist_hist  BLOB,
  sector_max BLOB,
  PRIMARY KEY(t, ant)
);

CREATE TABLE coverage(
  day      INTEGER NOT NULL,
  ant      TEXT NOT NULL,
  brg_bin  INTEGER NOT NULL,
  alt_band INTEGER NOT NULL,
  max_km   REAL NOT NULL DEFAULT 0,
  count    INTEGER NOT NULL DEFAULT 0,
  rssi_sum REAL NOT NULL DEFAULT 0,
  rssi_n   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(day, ant, brg_bin, alt_band)
) WITHOUT ROWID;

CREATE TABLE signal_hist(
  day      INTEGER NOT NULL,
  ant      TEXT NOT NULL,
  dist_bin INTEGER NOT NULL,
  rssi_bin INTEGER NOT NULL,
  count    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(day, ant, dist_bin, rssi_bin)
) WITHOUT ROWID;

CREATE TABLE elev_hist(
  day       INTEGER NOT NULL,
  ant       TEXT NOT NULL,
  elev_bin  INTEGER NOT NULL,
  count     INTEGER NOT NULL DEFAULT 0,
  rssi_sum  REAL NOT NULL DEFAULT 0,
  resid_sum REAL NOT NULL DEFAULT 0,
  PRIMARY KEY(day, ant, elev_bin)
) WITHOUT ROWID;

CREATE TABLE heat(
  cell     INTEGER NOT NULL,
  ant      TEXT NOT NULL,
  count    INTEGER NOT NULL DEFAULT 0,
  rssi_sum REAL NOT NULL DEFAULT 0,
  rssi_n   INTEGER NOT NULL DEFAULT 0,
  min_alt  INTEGER,
  max_alt  INTEGER,
  last_t   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(ant, cell)
) WITHOUT ROWID;

CREATE TABLE receiver_status(
  t    INTEGER NOT NULL,
  feed TEXT NOT NULL,
  json TEXT NOT NULL,
  PRIMARY KEY(t, feed)
) WITHOUT ROWID;

CREATE TABLE station_minute(
  t         INTEGER NOT NULL,
  sid       TEXT NOT NULL,
  frames    INTEGER NOT NULL DEFAULT 0,
  timed     INTEGER NOT NULL DEFAULT 0,
  late      INTEGER NOT NULL DEFAULT 0,
  dropped   INTEGER NOT NULL DEFAULT 0,
  bytes     INTEGER NOT NULL DEFAULT 0,
  up_secs   REAL,
  rtt_ms    REAL,
  PRIMARY KEY(t, sid)
) WITHOUT ROWID;

CREATE TABLE station_clock_minute(
  t             INTEGER NOT NULL,
  sid           TEXT NOT NULL,
  synced_secs   REAL,
  sigma_ns      REAL,
  drift_ppm     REAL,
  beta_short_ns REAL,
  resid_rms_ns  REAL,
  outliers      INTEGER NOT NULL DEFAULT 0,
  peers         INTEGER NOT NULL DEFAULT 0,
  epoch_changes INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(t, sid)
) WITHOUT ROWID;
`

// coreSchemaV3 records how many seconds of each statistics minute were
// actually covered (StatsRow.Secs): the first minute after a start and the
// last before a stop are partial, and dividing their counts by 60 would
// understate the rate. NULL = written before this column existed (a full
// minute). stats_hour.secs is the sum over the hour's minutes.
var coreSchemaV3 = `
ALTER TABLE stats_minute ADD COLUMN secs REAL;
ALTER TABLE stats_hour ADD COLUMN secs REAL;
`

// coreSchemaV2 stores the departure / arrival airports of a flight
// (migration "core" version 2). dep / arr are NULL until the engine has
// resolved a closed flight, an empty string when it was resolved but no airport fits, else
// the airport code (navdata.Airport.Code: ICAO, else GPS code, else ident).
// flights_unresolved serves the engine's backfill (newest unresolved first),
// flights_dep / flights_arr the report filters.
var coreSchemaV2 = `
ALTER TABLE flights ADD COLUMN dep TEXT;
ALTER TABLE flights ADD COLUMN arr TEXT;
CREATE INDEX flights_unresolved ON flights("end") WHERE dep IS NULL;
CREATE INDEX flights_dep ON flights(dep, start);
CREATE INDEX flights_arr ON flights(arr, start);
`

// registry holds the migrations registered by other packages and the set of
// currently open stores (so a late registration can be applied immediately).
var registry = struct {
	mu     sync.Mutex
	migs   []migration
	stores map[*Store]struct{}
}{stores: map[*Store]struct{}{}}

// stationShared names the registered migrations that RegisterMigration
// also applies to stations.db: tables that hold per-station data keyed by
// ant ('s<idx>'), sid or feed ('st/<sid>/<rx>'). Everything else registered
// with RegisterMigration (predict, navdata, met, alerts ...) is network- or
// host-wide and lives in airview.db only. RegisterMigrationFor chooses the
// kinds explicitly.
var stationShared = map[string]bool{
	"latency": true, // latency_minute(t, feed): per-feed latency chain
}

// RegisterMigration adds a schema migration owned by another package, so
// that it can keep its own tables in the AirView database:
//
//	func init() {
//	    store.RegisterMigration("predict", 1, `CREATE TABLE predict_model(...);`)
//	    store.RegisterMigration("predict", 2, `ALTER TABLE predict_model ADD COLUMN ...;`)
//	}
//
// name identifies the owner (a package name; "core" and "stations" are
// reserved), version numbers a name's migrations from 1 upwards, and ddl is
// one or more SQL statements that run exactly once per database inside one
// transaction (recorded in the schema_migrations table), so statements that
// cannot run in a transaction (VACUUM, PRAGMA journal_mode ...) are not
// allowed and a failing migration leaves nothing behind. Migrations of a
// name are applied in ascending version order. Never edit a migration that
// has shipped - add a new version. Prefix table names with the owner name to
// avoid clashes. The tables are recreated (empty) by Store.Reset.
//
// The migration applies to airview.db; the few names listed in stationShared
// (per-station data such as "latency") also apply to stations.db. Use
// RegisterMigrationFor to choose the database kinds explicitly.
//
// It may be called before Open (the usual case: from an init function) or
// later, in which case the migration is applied at once to every open Store
// of a matching kind. Registering the identical migration twice is a no-op.
// The returned error reports invalid arguments, a conflicting
// re-registration, or a failure to apply to an open Store; it is also logged.
func RegisterMigration(name string, version int, ddl string) error {
	kinds := KindMain
	if stationShared[strings.TrimSpace(name)] {
		kinds |= KindStations
	}
	return RegisterMigrationFor(kinds, name, version, ddl)
}

// RegisterMigrationFor is RegisterMigration for an explicit set of database
// kinds (KindMain, KindStations or both). Registering the same (name,
// version, ddl) again with more kinds extends the set and applies the
// migration to the open stores of the added kinds.
func RegisterMigrationFor(kinds Kind, name string, version int, ddl string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || name == coreName || name == stationsName:
		return fmt.Errorf("store: RegisterMigration: invalid name %q", name)
	case version < 1:
		return fmt.Errorf("store: RegisterMigration(%s): version must be >= 1, got %d", name, version)
	case strings.TrimSpace(ddl) == "":
		return fmt.Errorf("store: RegisterMigration(%s, %d): empty DDL", name, version)
	case kinds&kindAll == 0:
		return fmt.Errorf("store: RegisterMigration(%s, %d): no database kind", name, version)
	}
	kinds &= kindAll

	registry.mu.Lock()
	var m migration
	found := false
	for i := range registry.migs {
		e := &registry.migs[i]
		if e.name != name || e.version != version {
			continue
		}
		if e.ddl != ddl {
			registry.mu.Unlock()
			return fmt.Errorf("store: migration %s/%d is already registered with different DDL", name, version)
		}
		added := kinds &^ e.kinds
		if added == 0 {
			registry.mu.Unlock()
			return nil
		}
		e.kinds |= added
		m, found = *e, true
		m.kinds = added // apply to the newly covered kinds only
		break
	}
	if !found {
		m = migration{name, version, ddl, kinds}
		registry.migs = append(registry.migs, m)
	}
	open := make([]*Store, 0, len(registry.stores))
	for s := range registry.stores {
		if s.kind&m.kinds != 0 {
			open = append(open, s)
		}
	}
	registry.mu.Unlock()

	var errs []error
	for _, s := range open {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := s.Write(ctx, func(tx *sql.Tx) error {
			_, err := applyMigration(ctx, tx, m)
			return err
		})
		cancel()
		if err != nil && !errors.Is(err, ErrClosed) {
			s.log.Error("store: applying migration failed", "name", name, "version", version, "err", err)
			errs = append(errs, fmt.Errorf("store: migration %s/%d: %w", name, version, err))
		}
	}
	return errors.Join(errs...)
}

// allMigrations returns the core migrations of kind followed by the
// registered ones for kind, ordered by (name, version).
func allMigrations(kind Kind) []migration {
	registry.mu.Lock()
	var extra []migration
	for _, m := range registry.migs {
		if m.kinds&kind != 0 {
			extra = append(extra, m)
		}
	}
	registry.mu.Unlock()
	sort.SliceStable(extra, func(i, j int) bool {
		if extra[i].name != extra[j].name {
			return extra[i].name < extra[j].name
		}
		return extra[i].version < extra[j].version
	})
	var core []migration
	for _, m := range append(append([]migration(nil), coreMigrations...), stationsMigrations...) {
		if m.kinds&kind != 0 {
			core = append(core, m)
		}
	}
	return append(core, extra...)
}

const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations(
  name    TEXT NOT NULL,
  version INTEGER NOT NULL,
  applied INTEGER NOT NULL,
  PRIMARY KEY(name, version)
) WITHOUT ROWID`

// migrate brings the database behind db up to date for its kind. It is called from Open
// and Reset before the writer goroutine exists, so it uses db directly.
func migrate(ctx context.Context, db *sql.DB, kind Kind) error {
	if _, err := db.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("store: creating schema_migrations: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaDataMigrationsDDL); err != nil {
		return fmt.Errorf("store: creating schema_data_migrations: %w", err)
	}
	for _, m := range allMigrations(kind) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: migration %s/%d: %w", m.name, m.version, err)
		}
		if _, err := applyMigration(ctx, tx, m); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %s/%d: %w", m.name, m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %s/%d: %w", m.name, m.version, err)
		}
	}
	return nil
}

// applyMigration runs m inside tx unless schema_migrations says it already
// ran. It reports whether the DDL was executed.
func applyMigration(ctx context.Context, tx *sql.Tx, m migration) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM schema_migrations WHERE name = ? AND version = ?`, m.name, m.version).Scan(&one)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, m.ddl); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(name, version, applied) VALUES(?, ?, ?)`,
		m.name, m.version, time.Now().Unix())
	return err == nil, err
}
