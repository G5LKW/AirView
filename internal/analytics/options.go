package analytics

import (
	"database/sql"

	"airview/internal/store"
)

// Option configures an Analyzer (see New).
type Option func(*Analyzer)

// WithAntPrefix makes the Analyzer read the statistics of one station of an
// aggregator (DESIGN §6.6): every antenna argument ant of the public methods
// is mapped to the stored key store.AntKey(prefix, ant), so ant "" reads
// the station combined ("s<idx>") and ant "1" its channel 1 ("s<idx>/1").
// Results keep reporting the unprefixed ant. The Analyzer's store should be
// stations.db (store.OpenStations); prefix "" is standalone behaviour.
//
// The station geometry (location, antennas) still comes from the cfg
// function given to New: pass one that returns the station's location as
// Config.Receiver (and its antennas, if known) so that coverage, horizon and
// antenna analyses refer to the station, not to the host. Never give an
// Analyzer of another owner's station to a viewer (privacy, §8.2).
func WithAntPrefix(prefix string) Option {
	return func(a *Analyzer) { a.prefix = prefix }
}

// WithNetwork names the airview.db store that holds flights, aircraft,
// records, events and weather when the Analyzer's own store is stations.db,
// and the SID of the station: flight-based figures (unique aircraft,
// flights, top lists, farthest flights, new airframes, flight altitudes,
// all-time totals) are then restricted to the flights the station heard
// (flight_station), with that station's message counts and distances. sid ""
// keeps the network-wide flight figures. main nil keeps the Analyzer's own
// store for those tables - which stations.db does not have, so an Analyzer
// on stations.db needs WithNetwork for the flight-based analyses (Traffic,
// Daily, Top, AltitudeDistribution, Overview, Records, RecentEvents,
// WeatherImpact); without it they fail with "no such table".
func WithNetwork(main *store.Store, sid string) Option {
	return func(a *Analyzer) { a.net, a.sid = main, sid }
}

// antKey maps an API antenna ID to the stored key.
func (a *Analyzer) antKey(ant string) string { return store.AntKey(a.prefix, ant) }

// Prefix returns the station key prefix ("" = standalone).
func (a *Analyzer) Prefix() string { return a.prefix }

// SID returns the station SID set by WithNetwork ("" = none).
func (a *Analyzer) SID() string { return a.sid }

// mainSt is the store that holds flights, aircraft, records, events and
// weather.
func (a *Analyzer) mainSt() *store.Store {
	if a.net != nil {
		return a.net
	}
	return a.st
}

// flightDB is the read pool of mainSt.
func (a *Analyzer) flightDB() *sql.DB { return a.mainSt().DB() }

// flightsSrc returns the FROM source of the flight queries and its
// arguments (they go first among the arguments that follow the FROM
// clause). It exposes the columns id, icao, callsign, start, "end", msgs,
// positions, max_dist_km, min_alt, max_alt; with a station SID the counts
// and distances are the station's (flight_station) and only its flights are
// listed.
func (a *Analyzer) flightsSrc() (string, []any) {
	if a.sid == "" {
		return "flights", nil
	}
	return `(SELECT f.id AS id, f.icao AS icao, f.callsign AS callsign, f.start AS start, f."end" AS "end",
  fs.msgs AS msgs, fs.positions AS positions, fs.max_dist_km AS max_dist_km, f.min_alt AS min_alt, f.max_alt AS max_alt
  FROM flight_station fs JOIN flights f ON f.id = fs.flight_id WHERE fs.sid = ?)`, []any{a.sid}
}

// dayCond returns the condition selecting day numbers d0..d1 (inclusive)
// of a day-keyed statistics table: the plain range in standalone mode; for
// a station of stations.db (keyed (day, ant, ...) for all stations) an IN
// list of the days, so SQLite probes (day, ant) once per day instead of
// scanning every station's rows (store.SeriesIn). with goes before SELECT;
// its arguments wargs come first, the condition's arguments cargs after
// those of the conditions that precede cond in the query text.
func (a *Analyzer) dayCond(d0, d1 int) (with, cond string, wargs, cargs []any) {
	if a.prefix == "" {
		return "", "day >= ? AND day <= ?", nil, []any{d0, d1}
	}
	with, cond, wargs = store.SeriesIn("dd", "day", 1, int64(d0), int64(d1))
	return with, cond, wargs, nil
}

// sqlArgs concatenates SQL argument lists.
func sqlArgs(parts ...[]any) []any {
	var out []any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
