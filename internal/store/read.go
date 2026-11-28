package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

// nfloat scans a nullable SQL number into a float64, NULL becoming NaN.
type nfloat float64

// Scan implements sql.Scanner.
func (n *nfloat) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n = nfloat(math.NaN())
	case float64:
		*n = nfloat(v)
	case int64:
		*n = nfloat(v)
	case []byte:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			return err
		}
		*n = nfloat(f)
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return err
		}
		*n = nfloat(f)
	default:
		return fmt.Errorf("store: cannot scan %T into a float", src)
	}
	return nil
}

// nstring scans a nullable SQL text into a string, NULL becoming "".
type nstring string

// Scan implements sql.Scanner.
func (n *nstring) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n = ""
	case string:
		*n = nstring(v)
	case []byte:
		*n = nstring(v)
	default:
		*n = nstring(fmt.Sprint(v))
	}
	return nil
}

// nicao scans a nullable SQL integer into an ICAO address, NULL becoming 0.
type nicao uint32

// Scan implements sql.Scanner.
func (n *nicao) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n = 0
	case int64:
		*n = nicao(uint32(v))
	default:
		return fmt.Errorf("store: cannot scan %T into an ICAO address", src)
	}
	return nil
}

// querier is implemented by *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ---------------------------------------------------------------- aircraft

const aircraftSelect = `SELECT a.icao, a.first_seen, a.last_seen, a.callsign,
  CASE WHEN a.reg != '' THEN a.reg ELSE COALESCE(d.reg, '') END,
  CASE WHEN a.type != '' THEN a.type ELSE COALESCE(d.type, '') END,
  COALESCE(d."desc", ''),
  CASE WHEN a.operator != '' THEN a.operator ELSE COALESCE(d.operator, '') END,
  a.country, a.category, a.msgs, a.flights, a.positions, a.max_dist_km, a.max_alt, a.src_mask,
  a.flags | COALESCE(d.flags, 0), COALESCE(d.year, 0)
FROM aircraft a LEFT JOIN acdb d ON d.icao = a.icao`

func scanAircraft(sc interface{ Scan(...any) error }) (AircraftRow, error) {
	var r AircraftRow
	var icao int64
	err := sc.Scan(&icao, &r.FirstSeen, &r.LastSeen, &r.Callsign, &r.Reg, &r.Type, &r.TypeDesc, &r.Operator,
		&r.Country, &r.Category, &r.Msgs, &r.Flights, &r.Positions, &r.MaxDistKm, (*nfloat)(&r.MaxAlt),
		&r.SrcMask, &r.Flags, &r.Year)
	r.ICAO = uint32(icao)
	return r, err
}

// AircraftByICAO returns the stored airframe record joined with the
// aircraft database. ok is false when the aircraft was never seen (use
// ACDBLookup for reference data about unseen aircraft).
func (s *Store) AircraftByICAO(ctx context.Context, icao uint32) (row AircraftRow, ok bool, err error) {
	row, err = scanAircraft(s.DB().QueryRowContext(ctx, aircraftSelect+` WHERE a.icao = ?`, int64(icao)))
	if errors.Is(err, sql.ErrNoRows) {
		return AircraftRow{}, false, nil
	}
	return row, err == nil, err
}

// AircraftQuery selects and orders aircraft for ListAircraft.
type AircraftQuery struct {
	// Search is a case-insensitive substring matched against the hex ICAO
	// address, registration, callsign, type designator, type description
	// and operator. "" matches everything.
	Search string
	// Since / Until restrict last_seen (Unix seconds, 0 = open).
	Since, Until int64
	// Sort is one of: last_seen (default), first_seen, icao, callsign, reg,
	// type, operator, country, msgs, flights, positions, max_dist_km, max_alt.
	Sort string
	Desc bool
	// Limit defaults to 100 and is capped at 5000.
	Limit, Offset int
}

var aircraftSortCols = map[string]string{
	"":            "a.last_seen",
	"last_seen":   "a.last_seen",
	"first_seen":  "a.first_seen",
	"icao":        "a.icao",
	"callsign":    "a.callsign",
	"reg":         "5", // result columns: the acdb-backed values
	"type":        "6",
	"operator":    "8",
	"country":     "a.country",
	"msgs":        "a.msgs",
	"flights":     "a.flights",
	"positions":   "a.positions",
	"max_dist_km": "a.max_dist_km",
	"max_alt":     "a.max_alt",
}

// likePattern builds a LIKE pattern matching s as a substring, with the
// LIKE wildcards in s escaped by '\'.
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// ListAircraft returns one page of the aircraft ever seen plus the total
// number of rows matching the query (for paging).
func (s *Store) ListAircraft(ctx context.Context, q AircraftQuery) (rows []AircraftRow, total int, err error) {
	var where []string
	var args []any
	if search := strings.TrimSpace(q.Search); search != "" {
		p := likePattern(search)
		where = append(where, `(printf('%06X', a.icao) LIKE ? ESCAPE '\' OR a.callsign LIKE ? ESCAPE '\'
  OR a.reg LIKE ? ESCAPE '\' OR a.type LIKE ? ESCAPE '\' OR a.operator LIKE ? ESCAPE '\'
  OR d.reg LIKE ? ESCAPE '\' OR d.type LIKE ? ESCAPE '\' OR d."desc" LIKE ? ESCAPE '\' OR d.operator LIKE ? ESCAPE '\')`)
		for i := 0; i < 9; i++ {
			args = append(args, p)
		}
	}
	if q.Since > 0 {
		where = append(where, "a.last_seen >= ?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		where = append(where, "a.last_seen <= ?")
		args = append(args, q.Until)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}

	db := s.DB()
	if err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM aircraft a LEFT JOIN acdb d ON d.icao = a.icao`+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	col, ok := aircraftSortCols[strings.ToLower(strings.TrimSpace(q.Sort))]
	if !ok {
		col = aircraftSortCols[""]
	}
	dir := " ASC"
	if q.Desc {
		dir = " DESC"
	}
	limit, offset := pageBounds(q.Limit, q.Offset, 100, 5000)
	query := aircraftSelect + cond + " ORDER BY " + col + dir + ", a.icao ASC LIMIT ? OFFSET ?"
	rs, err := db.QueryContext(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rs.Close()
	for rs.Next() {
		r, err := scanAircraft(rs)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, r)
	}
	return rows, total, rs.Err()
}

func pageBounds(limit, offset, def, maxLimit int) (int, int) {
	if limit <= 0 {
		limit = def
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// ----------------------------------------------------------------- flights

const flightSelect = `SELECT f.id, f.icao, f.callsign, f.squawk, f.start, f."end", f.msgs, f.positions, f.src_mask,
  f.max_dist_km, f.max_dist_brg, f.min_dist_km, f.min_alt, f.max_alt, f.max_gs, f.rssi_min, f.rssi_max, f.rssi_avg,
  f.first_lat, f.first_lon, f.last_lat, f.last_lon, f.emergency,
  CASE WHEN a.reg != '' THEN a.reg ELSE COALESCE(d.reg, '') END,
  CASE WHEN a.type != '' THEN a.type ELSE COALESCE(d.type, '') END,
  CASE WHEN a.operator != '' THEN a.operator ELSE COALESCE(d.operator, '') END,
  COALESCE(a.country, ''), f.dep, f.arr
FROM flights f LEFT JOIN aircraft a ON a.icao = f.icao LEFT JOIN acdb d ON d.icao = f.icao`

func scanFlight(sc interface{ Scan(...any) error }) (Flight, error) {
	var f Flight
	var icao int64
	err := sc.Scan(&f.ID, &icao, &f.Callsign, &f.Squawk, &f.Start, &f.End, &f.Msgs, &f.Positions, &f.SrcMask,
		(*nfloat)(&f.MaxDistKm), (*nfloat)(&f.MaxDistBrg), (*nfloat)(&f.MinDistKm), (*nfloat)(&f.MinAlt),
		(*nfloat)(&f.MaxAlt), (*nfloat)(&f.MaxGS), (*nfloat)(&f.RSSIMin), (*nfloat)(&f.RSSIMax), (*nfloat)(&f.RSSIAvg),
		(*nfloat)(&f.FirstLat), (*nfloat)(&f.FirstLon), (*nfloat)(&f.LastLat), (*nfloat)(&f.LastLon), &f.Emergency,
		(*nstring)(&f.Reg), (*nstring)(&f.Type), (*nstring)(&f.Operator), &f.Country,
		(*nstring)(&f.Dep), (*nstring)(&f.Arr))
	f.ICAO = uint32(icao)
	return f, err
}

func (s *Store) queryFlights(ctx context.Context, tail string, args ...any) ([]Flight, error) {
	rs, err := s.DB().QueryContext(ctx, flightSelect+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Flight
	for rs.Next() {
		f, err := scanFlight(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rs.Err()
}

// FlightsForAircraft returns the flights of one airframe, newest first.
// limit defaults to 50 (max 1000).
func (s *Store) FlightsForAircraft(ctx context.Context, icao uint32, limit, offset int) ([]Flight, error) {
	limit, offset = pageBounds(limit, offset, 50, 1000)
	return s.queryFlights(ctx, ` WHERE f.icao = ? ORDER BY f.start DESC, f.id DESC LIMIT ? OFFSET ?`,
		int64(icao), limit, offset)
}

// RecentFlights returns the most recently started flights of all aircraft,
// newest first. limit defaults to 50 (max 1000).
func (s *Store) RecentFlights(ctx context.Context, limit, offset int) ([]Flight, error) {
	limit, offset = pageBounds(limit, offset, 50, 1000)
	return s.queryFlights(ctx, ` ORDER BY f.start DESC, f.id DESC LIMIT ? OFFSET ?`, limit, offset)
}

// FlightsBetween returns the flights that overlap the time range
// [from, to] (Unix seconds), oldest first. limit defaults to 1000 (max 20000).
func (s *Store) FlightsBetween(ctx context.Context, from, to int64, limit int) ([]Flight, error) {
	limit, _ = pageBounds(limit, 0, 1000, 20000)
	return s.queryFlights(ctx, ` WHERE f."end" >= ? AND f.start <= ? ORDER BY f."end" ASC LIMIT ?`, from, to, limit)
}

// FlightByID returns one flight.
func (s *Store) FlightByID(ctx context.Context, id int64) (Flight, bool, error) {
	fl, err := s.queryFlights(ctx, ` WHERE f.id = ?`, id)
	if err != nil || len(fl) == 0 {
		return Flight{}, false, err
	}
	return fl[0], true, nil
}

// LatestFlight returns the most recent flight of an airframe, e.g. so the
// tracker can resume a session after a restart when its End is recent.
func (s *Store) LatestFlight(ctx context.Context, icao uint32) (Flight, bool, error) {
	fl, err := s.queryFlights(ctx, ` WHERE f.icao = ? ORDER BY f.start DESC, f.id DESC LIMIT 1`, int64(icao))
	if err != nil || len(fl) == 0 {
		return Flight{}, false, err
	}
	return fl[0], true, nil
}

// --------------------------------------------------------------- positions

const positionCols = `flight_id, t, lat_e5, lon_e5, alt, gs, track, rssi_x10, src`

// ------------------------------------------------------------------ events

// EventQuery filters Events. The zero value returns the latest 100 events.
type EventQuery struct {
	Kind     string // "" = all kinds
	ICAO     uint32 // 0 = all aircraft
	Since    int64  // t >= Since (Unix seconds), 0 = open
	Until    int64  // t < Until (Unix seconds), 0 = open
	BeforeID int64  // id < BeforeID, for paging backwards; 0 = from the newest
	Limit    int    // default 100, max 5000
}

// Events returns events newest first.
func (s *Store) Events(ctx context.Context, q EventQuery) ([]Event, error) {
	where := []string{"1=1"}
	var args []any
	if q.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, q.Kind)
	}
	if q.ICAO != 0 {
		where = append(where, "icao = ?")
		args = append(args, int64(q.ICAO))
	}
	if q.Since > 0 {
		where = append(where, "t >= ?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		where = append(where, "t < ?")
		args = append(args, q.Until)
	}
	if q.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, q.BeforeID)
	}
	limit, _ := pageBounds(q.Limit, 0, 100, 5000)
	rs, err := s.DB().QueryContext(ctx, `SELECT id, t, kind, icao, detail_json FROM events WHERE `+
		strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Event
	for rs.Next() {
		var e Event
		if err := rs.Scan(&e.ID, &e.T, &e.Kind, (*nicao)(&e.ICAO), (*nstring)(&e.Detail)); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rs.Err()
}

// ----------------------------------------------------------------- records

// Records returns all all-time records ordered by key.
func (s *Store) Records(ctx context.Context) ([]Record, error) {
	rs, err := s.DB().QueryContext(ctx, `SELECT key, value, icao, t, detail_json FROM records ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Record
	for rs.Next() {
		var r Record
		if err := rs.Scan(&r.Key, &r.Value, (*nicao)(&r.ICAO), &r.T, (*nstring)(&r.Detail)); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// RecordByKey returns one record.
func (s *Store) RecordByKey(ctx context.Context, key string) (Record, bool, error) {
	var r Record
	err := s.DB().QueryRowContext(ctx, `SELECT key, value, icao, t, detail_json FROM records WHERE key = ?`, key).
		Scan(&r.Key, &r.Value, (*nicao)(&r.ICAO), &r.T, (*nstring)(&r.Detail))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	return r, err == nil, err
}

// ---------------------------------------------------------------------- kv

// KVGet reads a value from the kv table. Values written with KVSet are
// visible immediately; values queued with SetKVAsync after the next flush.
func (s *Store) KVGet(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.DB().QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// KVSet writes a key/value pair synchronously.
func (s *Store) KVSet(ctx context.Context, key, value string) error {
	if key == "" {
		return errors.New("store: KVSet: empty key")
	}
	var b Batch
	b.SetKV(key, value)
	return s.Apply(ctx, &b)
}

// KVDelete removes a key synchronously (no error if it does not exist).
func (s *Store) KVDelete(ctx context.Context, key string) error {
	var b Batch
	b.add("kv", tierHigh, 1, func(tc *txc) error { return tc.exec(sqlDeleteKV, key) })
	return s.Apply(ctx, &b)
}

// -------------------------------------------------------------------- acdb

// ACDBLookup returns the aircraft-database entry for an ICAO address.
func (s *Store) ACDBLookup(ctx context.Context, icao uint32) (ACDBEntry, bool, error) {
	e := ACDBEntry{ICAO: icao}
	err := s.DB().QueryRowContext(ctx,
		`SELECT reg, type, "desc", operator, flags, year FROM acdb WHERE icao = ?`, int64(icao)).
		Scan(&e.Reg, &e.Type, &e.Desc, &e.Operator, &e.Flags, &e.Year)
	if errors.Is(err, sql.ErrNoRows) {
		return ACDBEntry{}, false, nil
	}
	return e, err == nil, err
}

// ACDBCount returns the number of entries in the aircraft database.
func (s *Store) ACDBCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM acdb`).Scan(&n)
	return n, err
}

// ----------------------------------------------------------------- weather

func weatherScanDest(w *Weather) []any {
	return []any{
		(*nfloat)(&w.TempC), (*nfloat)(&w.DewpointC), (*nfloat)(&w.RH), (*nfloat)(&w.PressureMSL),
		(*nfloat)(&w.SurfacePressure), (*nfloat)(&w.WindKmh), (*nfloat)(&w.WindDir), (*nfloat)(&w.GustKmh),
		(*nfloat)(&w.Cloud), (*nfloat)(&w.CloudLow), (*nfloat)(&w.CloudMid), (*nfloat)(&w.CloudHigh),
		(*nfloat)(&w.PrecipMM), (*nfloat)(&w.RainMM), (*nfloat)(&w.SnowCM), (*nfloat)(&w.VisibilityM),
		(*nfloat)(&w.CAPE), (*nfloat)(&w.WeatherCode), (*nfloat)(&w.IsDay), (*nfloat)(&w.NSurface),
		(*nfloat)(&w.DNDH), (*nfloat)(&w.KFactor), (*nfloat)(&w.Duct),
	}
}

func (s *Store) queryWeather(ctx context.Context, table string, from, to int64) ([]Weather, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	head := "t, 0, 0"
	tail := ", profile_json"
	order := "t"
	switch table {
	case "weather_fc":
		head = "t, issued, 0"
	case "weather_fc_log":
		head, tail, order = "t, issued, lead_h", ", NULL", "t, lead_h"
	}
	rs, err := s.DB().QueryContext(ctx, "SELECT "+head+", "+strings.Join(weatherCols, ", ")+tail+
		" FROM "+table+" WHERE t >= ? AND t <= ? ORDER BY "+order, from, to)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Weather
	for rs.Next() {
		var w Weather
		dest := append([]any{&w.T, &w.Issued, &w.LeadH}, weatherScanDest(&w)...)
		dest = append(dest, (*nstring)(&w.ProfileJSON))
		if err := rs.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rs.Err()
}

// WeatherObs returns the hourly observations with from <= t <= to (Unix
// seconds, to 0 = open), oldest first.
func (s *Store) WeatherObs(ctx context.Context, from, to int64) ([]Weather, error) {
	return s.queryWeather(ctx, "weather_obs", from, to)
}

// WeatherForecast returns the latest forecast rows with from <= t <= to,
// with the issue time of the newest forecast applied (see
// SetWeatherForecastDiff): the rows as 1.0.0 stored them.
func (s *Store) WeatherForecast(ctx context.Context, from, to int64) ([]Weather, error) {
	rows, err := s.queryWeather(ctx, "weather_fc", from, to)
	if err != nil {
		return nil, err
	}
	if err := s.ApplyForecastIssue(ctx, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// WeatherForecastLog returns the logged forecasts (one per target hour and
// lead time) with from <= t <= to, ordered by (t, lead).
func (s *Store) WeatherForecastLog(ctx context.Context, from, to int64) ([]Weather, error) {
	return s.queryWeather(ctx, "weather_fc_log", from, to)
}

// ------------------------------------------------------------ stats & size

// MinuteStats returns stats_minute rows of one antenna ("" = combined) with
// from <= t <= to, oldest first.
func (s *Store) MinuteStats(ctx context.Context, ant string, from, to int64) ([]StatsRow, error) {
	return s.queryStats(ctx, "stats_minute", ant, from, to)
}

// HourStats returns stats_hour rows of one antenna ("" = combined) with
// from <= t <= to, oldest first.
func (s *Store) HourStats(ctx context.Context, ant string, from, to int64) ([]StatsRow, error) {
	return s.queryStats(ctx, "stats_hour", ant, from, to)
}

func (s *Store) queryStats(ctx context.Context, table, ant string, from, to int64) ([]StatsRow, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	extra := ", NULL, 0, secs"
	if table == "stats_hour" || table == "stats_day" {
		extra = ", aircraft_avg, minutes, secs"
	}
	db := s.DB()
	with, cond, targs, err := s.timeCond(ctx, db, table, "t", from, to)
	if err != nil {
		return nil, err
	}
	rs, err := db.QueryContext(ctx, with+"SELECT "+strings.Join(statsInsertCols(), ", ")+extra+
		" FROM "+table+" WHERE "+cond+" AND ant = ?"+s.orderByT(), append(targs, ant)...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []StatsRow
	for rs.Next() {
		var r StatsRow
		if err := rs.Scan(&r.T, &r.Ant,
			&r.Frames, &r.DF0, &r.DF4, &r.DF5, &r.DF11, &r.DF16, &r.DF17, &r.DF18, &r.DF20, &r.DF21,
			&r.DFOther, &r.CRCBad, &r.Positions, &r.MLATPositions,
			&r.Aircraft, &r.AircraftPos, &r.ADSB, &r.MLAT, &r.ModeS,
			(*nfloat)(&r.MaxKm), (*nfloat)(&r.RSSIAvg), (*nfloat)(&r.RSSIMin), (*nfloat)(&r.RSSIMax),
			(*nfloat)(&r.AircraftAvg), &r.Minutes, (*nfloat)(&r.Secs)); err != nil {
			return nil, err
		}
		if !(r.Secs > 0) {
			r.Secs = 0 // NULL: unknown
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if s.kind == KindStations {
		slices.SortFunc(out, func(a, b StatsRow) int { return cmp.Compare(a.T, b.T) })
	}
	return out, nil
}

// RangeHours returns the range_hour rows of one antenna ("" = combined) with
// from <= t <= to (Unix seconds, to 0 = open), oldest first, with the
// histogram and sector BLOBs decoded.
func (s *Store) RangeHours(ctx context.Context, ant string, from, to int64) ([]RangeHour, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	db := s.DB()
	with, cond, targs, err := s.timeCond(ctx, db, "range_hour", "t", from, to)
	if err != nil {
		return nil, err
	}
	rs, err := db.QueryContext(ctx, with+`SELECT t, ant, max_km, p99_km, p95_km, p90_km, p50_km, positions, aircraft,
  far_count, resid_db, resid_n, dist_hist, sector_max
FROM range_hour WHERE `+cond+` AND ant = ?`+s.orderByT(), append(targs, ant)...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []RangeHour
	for rs.Next() {
		var t int64
		var a string
		r, _, err := scanRangeHour(rs, &t, &a)
		if err != nil {
			return nil, err
		}
		r.T, r.Ant = t, a
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if s.kind == KindStations {
		slices.SortFunc(out, func(a, b RangeHour) int { return cmp.Compare(a.T, b.T) })
	}
	return out, nil
}

// NTPSamples returns the NTP measurements with from <= t <= to (Unix
// seconds, to 0 = open), oldest first. limit <= 0 means no limit; with a
// limit the NEWEST limit samples of the range are returned.
func (s *Store) NTPSamples(ctx context.Context, from, to int64, limit int) ([]NTPSample, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rs, err := s.DB().QueryContext(ctx, `SELECT t, server, offset_ms, delay_ms, COALESCE(stratum, 0) FROM (
  SELECT * FROM ntp_samples WHERE t >= ? AND t <= ? ORDER BY t DESC LIMIT ?) ORDER BY t`, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []NTPSample
	for rs.Next() {
		var n NTPSample
		if err := rs.Scan(&n.T, &n.Server, (*nfloat)(&n.OffsetMs), (*nfloat)(&n.DelayMs), &n.Stratum); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rs.Err()
}

// DBSize returns the size on disk of the database file plus its WAL, in bytes.
func (s *Store) DBSize() int64 {
	var n int64
	for _, f := range []string{s.path, s.path + "-wal"} {
		if st, err := os.Stat(f); err == nil {
			n += st.Size()
		}
	}
	return n
}

func queryStrings(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
