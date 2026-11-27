package navdata

import (
	"fmt"
	"strings"

	"airview/internal/store"
)

// MigrationName is the store migration owner name of this package.
const MigrationName = "navdata"

// Table column definitions. Bulk datasets are imported into a *_stage table
// (no indexes) in chunked transactions, then swapped in with one short
// transaction (DROP main, RENAME stage, CREATE indexes, CREATE new stage), so
// readers always see either the complete old or the complete new data.
const (
	colsAirport = `(
  id INTEGER PRIMARY KEY,
  ident TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  name TEXT NOT NULL DEFAULT '',
  lat REAL NOT NULL,
  lon REAL NOT NULL,
  elev_ft INTEGER,
  continent TEXT NOT NULL DEFAULT '',
  country TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '',
  municipality TEXT NOT NULL DEFAULT '',
  scheduled INTEGER NOT NULL DEFAULT 0,
  icao TEXT NOT NULL DEFAULT '',
  iata TEXT NOT NULL DEFAULT '',
  gps TEXT NOT NULL DEFAULT '',
  local TEXT NOT NULL DEFAULT ''
)`
	colsRunway = `(
  id INTEGER PRIMARY KEY,
  airport_id INTEGER NOT NULL,
  airport_ident TEXT NOT NULL DEFAULT '',
  length_ft INTEGER, width_ft INTEGER,
  surface TEXT NOT NULL DEFAULT '',
  lighted INTEGER NOT NULL DEFAULT 0,
  closed INTEGER NOT NULL DEFAULT 0,
  le_ident TEXT NOT NULL DEFAULT '', le_lat REAL, le_lon REAL, le_elev INTEGER, le_hdg REAL, le_disp INTEGER,
  he_ident TEXT NOT NULL DEFAULT '', he_lat REAL, he_lon REAL, he_elev INTEGER, he_hdg REAL, he_disp INTEGER
)`
	colsNavaid = `(
  id INTEGER PRIMARY KEY,
  ident TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  type TEXT NOT NULL DEFAULT '',
  freq_khz INTEGER,
  lat REAL NOT NULL,
  lon REAL NOT NULL,
  elev_ft INTEGER,
  country TEXT NOT NULL DEFAULT '',
  dme_channel TEXT NOT NULL DEFAULT '',
  magvar REAL,
  usage TEXT NOT NULL DEFAULT '',
  power TEXT NOT NULL DEFAULT '',
  airport TEXT NOT NULL DEFAULT ''
)`
	colsCountry = `(
  code TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  continent TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID`
	colsRoute = `(
  callsign TEXT PRIMARY KEY,
  airline TEXT NOT NULL DEFAULT '',
  number TEXT NOT NULL DEFAULT '',
  airports TEXT NOT NULL
) WITHOUT ROWID`
)

// table describes one bulk-imported table.
type table struct {
	name    string
	cols    string
	indexes []string // full CREATE INDEX statements for the main table
	insert  string   // INSERT into the stage table (placeholder %s = table)
}

var (
	tblAirport = table{
		name: "navdata_airport", cols: colsAirport,
		indexes: []string{
			`CREATE INDEX navdata_airport_ident ON navdata_airport(ident)`,
			`CREATE INDEX navdata_airport_icao ON navdata_airport(icao) WHERE icao <> ''`,
			`CREATE INDEX navdata_airport_iata ON navdata_airport(iata) WHERE iata <> ''`,
			`CREATE INDEX navdata_airport_gps ON navdata_airport(gps) WHERE gps <> ''`,
			`CREATE INDEX navdata_airport_lat ON navdata_airport(lat, lon)`,
		},
		insert: `INSERT OR REPLACE INTO %s(id, ident, type, size, name, lat, lon, elev_ft, continent, country, region,
  municipality, scheduled, icao, iata, gps, local) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
	}
	tblRunway = table{
		name: "navdata_runway", cols: colsRunway,
		indexes: []string{`CREATE INDEX navdata_runway_airport ON navdata_runway(airport_id)`},
		insert: `INSERT OR REPLACE INTO %s(id, airport_id, airport_ident, length_ft, width_ft, surface, lighted, closed,
  le_ident, le_lat, le_lon, le_elev, le_hdg, le_disp, he_ident, he_lat, he_lon, he_elev, he_hdg, he_disp)
  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
	}
	tblNavaid = table{
		name: "navdata_navaid", cols: colsNavaid,
		indexes: []string{
			`CREATE INDEX navdata_navaid_lat ON navdata_navaid(lat, lon)`,
			`CREATE INDEX navdata_navaid_ident ON navdata_navaid(ident)`,
		},
		insert: `INSERT OR REPLACE INTO %s(id, ident, name, type, freq_khz, lat, lon, elev_ft, country, dme_channel,
  magvar, usage, power, airport) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
	}
	tblCountry = table{
		name: "navdata_country", cols: colsCountry,
		insert: `INSERT OR REPLACE INTO %s(code, name, continent) VALUES(?,?,?)`,
	}
	tblRoute = table{
		name: "navdata_route", cols: colsRoute,
		insert: `INSERT OR REPLACE INTO %s(callsign, airline, number, airports) VALUES(?,?,?,?)`,
	}
)

func (t table) stage() string { return t.name + "_stage" }

// swapSQL returns the statements that replace the main table with the stage.
func (t table) swapSQL() []string {
	s := []string{
		`DROP TABLE IF EXISTS ` + t.name,
		`ALTER TABLE ` + t.stage() + ` RENAME TO ` + t.name,
	}
	s = append(s, t.indexes...)
	s = append(s, `CREATE TABLE `+t.stage()+` `+t.cols)
	return s
}

func migrationV1() string {
	var b strings.Builder
	for _, t := range []table{tblAirport, tblRunway, tblNavaid, tblCountry, tblRoute} {
		fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s %s;\n", t.name, t.cols)
		fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s %s;\n", t.stage(), t.cols)
		for _, ix := range t.indexes {
			b.WriteString(strings.Replace(ix, "CREATE INDEX", "CREATE INDEX IF NOT EXISTS", 1))
			b.WriteString(";\n")
		}
	}
	b.WriteString(`CREATE TABLE IF NOT EXISTS navdata_meta(
  dataset TEXT PRIMARY KEY,
  url TEXT NOT NULL DEFAULT '',
  imported INTEGER NOT NULL DEFAULT 0,
  records INTEGER NOT NULL DEFAULT 0,
  skipped INTEGER NOT NULL DEFAULT 0,
  bytes INTEGER NOT NULL DEFAULT 0,
  last_modified TEXT NOT NULL DEFAULT '',
  etag TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS navdata_route_cache(
  callsign TEXT PRIMARY KEY,
  fetched INTEGER NOT NULL,
  expires INTEGER NOT NULL,
  found INTEGER NOT NULL,
  airline TEXT NOT NULL DEFAULT '',
  airports TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS navdata_photo(
  icao INTEGER PRIMARY KEY,
  fetched INTEGER NOT NULL,
  expires INTEGER NOT NULL,
  found INTEGER NOT NULL,
  thumb TEXT NOT NULL DEFAULT '', thumb_w INTEGER NOT NULL DEFAULT 0, thumb_h INTEGER NOT NULL DEFAULT 0,
  large TEXT NOT NULL DEFAULT '', large_w INTEGER NOT NULL DEFAULT 0, large_h INTEGER NOT NULL DEFAULT 0,
  link TEXT NOT NULL DEFAULT '',
  photographer TEXT NOT NULL DEFAULT ''
);
`)
	return b.String()
}

func init() {
	if err := store.RegisterMigration(MigrationName, 1, migrationV1()); err != nil {
		panic(err)
	}
}
