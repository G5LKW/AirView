package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Read API of the network tables (see ops_net.go).

const flightStationCols = `flight_id, sid, icao, first, last, msgs, positions,
  max_dist_km, min_dist_km, rssi_min, rssi_max, rssi_avg`

func scanFlightStation(sc interface{ Scan(...any) error }) (FlightStation, error) {
	var r FlightStation
	var icao int64
	err := sc.Scan(&r.FlightID, &r.SID, &icao, &r.First, &r.Last, &r.Msgs, &r.Positions,
		(*nfloat)(&r.MaxDistKm), (*nfloat)(&r.MinDistKm), (*nfloat)(&r.RSSIMin), (*nfloat)(&r.RSSIMax), (*nfloat)(&r.RSSIAvg))
	r.ICAO = uint32(icao)
	return r, err
}

func queryFlightStations(ctx context.Context, q querier, query string, args ...any) ([]FlightStation, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlightStation
	for rows.Next() {
		r, err := scanFlightStation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FlightStations returns the flight_station rows of one flight (which
// stations heard it), ordered by SID. Owner-scoped callers must filter the
// SIDs (privacy: receivers per aircraft are admin-only, DESIGN §8.2).
func (s *Store) FlightStations(ctx context.Context, flightID int64) ([]FlightStation, error) {
	return queryFlightStations(ctx, s.DB(), `SELECT `+flightStationCols+` FROM flight_station WHERE flight_id = ? ORDER BY sid`, flightID)
}

// StationFlights returns the flight_station rows of one station whose
// first copy is in from <= first <= to (Unix seconds, to 0 = open), newest
// first; limit <= 0 means 1000.
func (s *Store) StationFlights(ctx context.Context, sid string, from, to int64, limit int) ([]FlightStation, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	if limit <= 0 {
		limit = 1000
	}
	return queryFlightStations(ctx, s.DB(), `SELECT `+flightStationCols+` FROM flight_station
WHERE sid = ? AND first >= ? AND first <= ? ORDER BY first DESC LIMIT ?`, sid, from, to, limit)
}

// MLATMinutes returns the mlat_minute rows of one SID ("" = network) with
// from <= t <= to (to 0 = open), oldest first.
func (s *Store) MLATMinutes(ctx context.Context, sid string, from, to int64) ([]MLATMinute, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	rows, err := s.DB().QueryContext(ctx, `SELECT t, sid, frames, sync_frames, groups, fixes, outliers, peers,
  synced_secs, sigma_ns, resid_rms_ns, drift_ppm FROM mlat_minute WHERE sid = ? AND t >= ? AND t <= ? ORDER BY t`, sid, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MLATMinute
	for rows.Next() {
		var r MLATMinute
		if err := rows.Scan(&r.T, &r.SID, &r.Frames, &r.SyncFrames, &r.Groups, &r.Fixes, &r.Outliers, &r.Peers,
			(*nfloat)(&r.SyncedSecs), (*nfloat)(&r.SigmaNs), (*nfloat)(&r.ResidRMSNs), (*nfloat)(&r.DriftPPM)); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MLATQualityHours returns the mlat_quality_hour rows with from <= t <= to
// (to 0 = open), ordered by (t, cell, altband). cell < 0 selects every
// cell, else only that Cell1Deg cell.
func (s *Store) MLATQualityHours(ctx context.Context, from, to int64, cell int) ([]MLATQualityHour, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	q := `SELECT t, cell1deg, altband, n, p50, p95, p99, err_hist FROM mlat_quality_hour WHERE t >= ? AND t <= ?`
	args := []any{from, to}
	if cell >= 0 {
		q += ` AND cell1deg = ?`
		args = append(args, cell)
	}
	rows, err := s.DB().QueryContext(ctx, q+` ORDER BY t, cell1deg, altband`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MLATQualityHour
	for rows.Next() {
		var r MLATQualityHour
		var blob []byte
		if err := rows.Scan(&r.T, &r.Cell, &r.AltBand, &r.N, (*nfloat)(&r.P50), (*nfloat)(&r.P95), (*nfloat)(&r.P99), &blob); err != nil {
			return nil, err
		}
		r.Hist = DecodeDistHist(blob)
		out = append(out, r)
	}
	return out, rows.Err()
}

// StationMinutes returns the station_minute rows of one SID with from <= t
// <= to (to 0 = open), oldest first.
func (s *Store) StationMinutes(ctx context.Context, sid string, from, to int64) ([]StationMinute, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	rows, err := s.DB().QueryContext(ctx, `SELECT t, sid, frames, timed, late, dropped, bytes, up_secs, rtt_ms
FROM station_minute WHERE t >= ? AND t <= ? AND sid = ? ORDER BY t`, from, to, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StationMinute
	for rows.Next() {
		var r StationMinute
		if err := rows.Scan(&r.T, &r.SID, &r.Frames, &r.Timed, &r.Late, &r.Dropped, &r.Bytes,
			(*nfloat)(&r.UpSecs), (*nfloat)(&r.RTTMs)); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StationClockMinutes returns the station_clock_minute rows of one SID with
// from <= t <= to (to 0 = open), oldest first.
func (s *Store) StationClockMinutes(ctx context.Context, sid string, from, to int64) ([]StationClockMinute, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	rows, err := s.DB().QueryContext(ctx, `SELECT t, sid, synced_secs, sigma_ns, drift_ppm, beta_short_ns, resid_rms_ns,
  outliers, peers, epoch_changes FROM station_clock_minute WHERE t >= ? AND t <= ? AND sid = ? ORDER BY t`, from, to, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StationClockMinute
	for rows.Next() {
		var r StationClockMinute
		if err := rows.Scan(&r.T, &r.SID, (*nfloat)(&r.SyncedSecs), (*nfloat)(&r.SigmaNs), (*nfloat)(&r.DriftPPM),
			(*nfloat)(&r.BetaShortNs), (*nfloat)(&r.ResidRMSNs), &r.Outliers, &r.Peers, &r.EpochChanges); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StationAntKeys returns the distinct ant keys of one station prefix
// present in stats_hour (the station key and its channel keys), sorted. It
// scans stats_hour (keyed by time first): use it for exports and admin
// pages, not per request.
func (s *Store) StationAntKeys(ctx context.Context, prefix string) ([]string, error) {
	where, args := AntKeyWhere("ant", prefix)
	return queryStrings(ctx, s.DB(), `SELECT DISTINCT ant FROM stats_hour WHERE `+where+` ORDER BY ant`, args...)
}

// ForecastIssue returns the issue window of the newest forecast written by
// SetWeatherForecastDiff (zero when none).
func (s *Store) ForecastIssue(ctx context.Context) (ForecastIssue, error) {
	var v string
	err := s.DB().QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, KVForecastIssue).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return ForecastIssue{}, nil
	}
	if err != nil {
		return ForecastIssue{}, err
	}
	fi, _ := ParseForecastIssue(v)
	return fi, nil
}

// ApplyForecastIssue sets the issue time of the forecast rows (from
// weather_fc) that belong to the newest issue: the value the 1.0.0 writer
// stored in them. Idempotent.
func (s *Store) ApplyForecastIssue(ctx context.Context, rows []Weather) error {
	if len(rows) == 0 {
		return nil
	}
	fi, err := s.ForecastIssue(ctx)
	if err != nil {
		return err
	}
	fi.Apply(rows)
	return nil
}

// ForecastRows is WeatherForecast (kept for its callers): the rows with
// the issue time of the newest forecast applied, as 1.0.0 stored them.
func (s *Store) ForecastRows(ctx context.Context, from, to int64) ([]Weather, error) {
	return s.WeatherForecast(ctx, from, to)
}

// weatherFcColumns are the 1.0.0 columns of weather_fc in order.
var weatherFcColumns = append(append([]string{"t", "issued"}, weatherCols...), "profile_json")

func init() {
	// The export and the parity harness read weather_fc with the issue time
	// of the newest forecast applied (SetWeatherForecastDiff), exactly as
	// 1.0.0 stored the rows.
	RegisterLogicalTable(LogicalTable{
		Name:    "weather_fc",
		Kinds:   KindMain,
		Columns: weatherFcColumns,
		TimeCol: "t",
		Unit:    1,
		Scan:    scanLogicalWeatherFc,
	})
}

func scanLogicalWeatherFc(ctx context.Context, q Querier, span *Span, limit int64, fn func(row []any) error) error {
	var fi ForecastIssue
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, KVForecastIssue).Scan(&v)
	switch {
	case err == nil:
		fi, _ = ParseForecastIssue(v)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	query := `SELECT * FROM weather_fc`
	var args []any
	if span != nil {
		query += ` WHERE t >= ? AND t < ?`
		args = append(args, span.From, span.To)
	}
	query += ` ORDER BY t`
	if limit > 0 {
		query += ` LIMIT ` + strconv.FormatInt(limit, 10)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if len(cols) != len(weatherFcColumns) {
		return errors.New("store: weather_fc has an unexpected layout")
	}
	return scanRowsAny(rows, len(cols), func(vals []any) error {
		if t, ok := vals[0].(int64); ok && fi.Issued > 0 && t >= fi.From && t <= fi.To {
			vals[1] = fi.Issued
		}
		return fn(vals)
	})
}

// CheckpointServes reports whether a Store of this process has the
// database file at path open: its checkpointer then checkpoints the file
// off the commit path (checkpointer.go). internal/registry, which opens
// stations.db with a pool of its own, sets its writer's
// wal_autocheckpoint to the server profile's hard cap only while this is
// true (otherwise SQLite's own 1,000-page checkpoint keeps the WAL small).
func CheckpointServes(path string) bool {
	want, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for s := range registry.stores {
		if s.closed.Load() {
			continue
		}
		if have, err := filepath.Abs(s.path); err == nil && sameDBPath(have, want) {
			return true
		}
	}
	return false
}

func sameDBPath(a, b string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
