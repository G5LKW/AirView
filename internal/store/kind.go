package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Kind selects which AirView database a Store is: airview.db (KindMain, the
// default and the only kind in standalone mode) or stations.db
// (KindStations, aggregator mode, DESIGN §6.6).
//
// The two kinds share the writer design, the Batch API and the statistics
// table layout; they differ in the file name and in the migrations applied:
//
//   - KindMain: the complete core schema plus every registered migration
//     (RegisterMigration), plus the network tables flight_station,
//     mlat_minute and mlat_quality_hour (empty in standalone mode).
//   - KindStations: only the station-relevant subset - kv, stats_minute,
//     stats_hour, stats_day, range_hour, coverage, signal_hist, elev_hist,
//     heat, receiver_status, station_minute, station_clock_minute - plus the
//     registered migrations named in stationShared or registered with
//     RegisterMigrationFor(KindStations, ...). Rows are keyed by station:
//     ant = AntKey(...) ('s<idx>' / 's<idx>/<ch>'), sid, or feed
//     ('st/<sid>/<rx>'). The registry tables of internal/registry live in the
//     same file; this package never creates or touches them.
//
// Batch operations for tables a kind does not have (flights, positions,
// weather ... on stations.db) fail on their own data and are dropped and
// counted like any other bad operation (Stats.FailedOps).
type Kind uint8

const (
	// KindMain is airview.db.
	KindMain Kind = 1 << iota
	// KindStations is stations.db.
	KindStations

	kindAll = KindMain | KindStations
)

// StationsFileName is the database file of KindStations inside the data
// directory. internal/registry opens the same file (registry.FileName).
const StationsFileName = "stations.db"

// String returns "main" or "stations".
func (k Kind) String() string {
	switch k {
	case KindMain:
		return "main"
	case KindStations:
		return "stations"
	}
	return "invalid"
}

func (k Kind) fileName() string {
	if k == KindStations {
		return StationsFileName
	}
	return FileName
}

// ErrResetUnsupported is returned by Reset on stations.db: that file also
// holds the registry (accounts, keys, owners), which a statistics reset must
// never delete. Use DeleteKeyRange / DeleteStationData to remove station
// data, or remove the file while nothing has it open.
var ErrResetUnsupported = errors.New("store: Reset is not supported on stations.db (it also holds the registry)")

// OpenStations opens (creating it if necessary) stations.db in dir, applies
// the station migrations and starts the writer. It is OpenWithOptions with
// opts.Kind = KindStations.
func OpenStations(dir string, opts Options) (*Store, error) {
	opts.Kind = KindStations
	return OpenWithOptions(dir, opts)
}

// Kind returns the database kind of the store.
func (s *Store) Kind() Kind { return s.kind }

// stationsPrepared lists the statements the writer prepares on stations.db
// (the subset of preparedSQL whose tables exist there).
var stationsPrepared = []string{
	sqlMergeStatsMinute, sqlLowerRollupMark, sqlGetRangeHour, sqlPutRangeHour, sqlMergeCoverage,
	sqlMergeSignalHist, sqlMergeElevHist, sqlMergeHeat, sqlInsertReceiverStatus, sqlSetKV, sqlDeleteKV,
	sqlMergeStationMinute, sqlMergeStationClockMinute,
}

// mainPrepared lists the statements the writer prepares on airview.db.
// heat on airview.db is keyed (ant, tile, cell) since migration "heat" 2
// (design §3.6): its UPSERT carries the tile (sqlMergeHeatTiled, C);
// stations.db keeps sqlMergeHeat.
var mainPrepared = func() []string {
	out := make([]string, 0, len(preparedSQL)+4)
	for _, q := range preparedSQL {
		if q == sqlMergeHeat {
			q = sqlMergeHeatTiled
		}
		out = append(out, q)
	}
	return append(out, sqlMergeFlightStation, sqlMergeMLATMinute, sqlGetMLATQuality, sqlPutMLATQuality)
}()

func (k Kind) prepared() []string {
	if k == KindStations {
		return stationsPrepared
	}
	return mainPrepared
}

// HasTable reports whether the database has a table of that name (at open
// time, plus tables created by migrations registered since). It is cheap.
func (s *Store) HasTable(name string) bool {
	if s.closed.Load() {
		return false
	}
	var one int
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&one)
	return err == nil
}

// hasTableTx is HasTable inside a write transaction.
func hasTableTx(ctx context.Context, q querier, name string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// tableColumns returns the column names of a table (empty when it does not
// exist).
func tableColumns(ctx context.Context, q querier, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols[strings.ToLower(c)] = true
	}
	return cols, rows.Err()
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
