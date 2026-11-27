package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Owner data export (DESIGN §8.5: "Owners can export their data (JSON
// zip)"). internal/registry writes the zip and the registry part; the
// statistics of a station come from here through registry.Options.ExportExtra
// (see WIRING.md).

// ExportSpec selects what ExportStationData exports.
type ExportSpec struct {
	Keys StationKeys
	// From limits time-keyed tables to rows at or after this Unix second
	// (0 = everything kept).
	From int64
	// Minutes includes the minute tier (stats_minute, station_minute,
	// station_clock_minute, mlat_minute, latency_minute, receiver_status):
	// about 1,440 rows per key and day.
	Minutes bool
	// MaxRows caps the rows exported per table (default 2,000,000); a
	// table that hits the cap is exported truncated and reported in the
	// "_export.json" summary.
	MaxRows int
}

// exportTimeCol is the time column of each exportable table and whether it
// holds day numbers.
var exportTimeCol = map[string]struct {
	col  string
	days bool
	tier string
}{
	"stats_minute": {"t", false, "minute"}, "receiver_status": {"t", false, "minute"},
	"station_minute": {"t", false, "minute"}, "station_clock_minute": {"t", false, "minute"},
	"mlat_minute": {"t", false, "minute"}, "latency_minute": {"t", false, "minute"},
	"stats_hour": {"t", false, "hour"}, "stats_day": {"t", false, "day"}, "range_hour": {"t", false, "hour"},
	"coverage": {"day", true, "day"}, "signal_hist": {"day", true, "day"}, "elev_hist": {"day", true, "day"},
	"heat": {"last_t", false, "all"}, "flight_station": {"first", false, "flight"},
}

// ExportSummary is written as "_export.json" by ExportStationData.
type ExportSummary struct {
	Database  string           `json:"database"`
	Rows      map[string]int64 `json:"rows"`
	Truncated []string         `json:"truncated,omitempty"`
}

// ExportStationData adds one JSON value per table of this database that
// holds rows of the station (k.Prefix ant keys, k.SID sid / FeedID rows):
// add("<kind>/<table>.json", []map[string]any) - column name to value, with
// the BLOB columns decoded (dist_hist / err_hist as count arrays, sector_max
// as km per 10° sector) - plus add("<kind>/_export.json", ExportSummary).
// Tables without rows of the station are skipped. Readers run on the
// read-only pool and never block the writer.
func (s *Store) ExportStationData(ctx context.Context, spec ExportSpec, add func(name string, v any) error) error {
	if add == nil {
		return errors.New("store: ExportStationData: nil add")
	}
	if spec.MaxRows <= 0 {
		spec.MaxRows = 2_000_000
	}
	sum := ExportSummary{Database: s.kind.String(), Rows: map[string]int64{}}
	for _, st := range stationTables {
		tc, ok := exportTimeCol[st.table]
		if !ok || (tc.tier == "minute" && !spec.Minutes) {
			continue
		}
		where, args, ok := spec.Keys.where(st.col)
		if !ok || !s.HasTable(st.table) {
			continue
		}
		if spec.From > 0 {
			from := spec.From
			if tc.days {
				from = int64(DayOf(spec.From))
			}
			where += " AND " + tc.col + " >= ?"
			args = append(args, from)
		}
		rows, truncated, err := s.dumpRows(ctx, st.table, where, args, spec.MaxRows)
		if err != nil {
			return fmt.Errorf("store: exporting %s: %w", st.table, err)
		}
		if len(rows) == 0 {
			continue
		}
		sum.Rows[st.table] = int64(len(rows))
		if truncated {
			sum.Truncated = append(sum.Truncated, st.table)
		}
		if err := add(s.kind.String()+"/"+st.table+".json", rows); err != nil {
			return err
		}
	}
	return add(s.kind.String()+"/_export.json", sum)
}

// dumpRows selects SELECT * ... WHERE where as maps (at most max rows).
func (s *Store) dumpRows(ctx context.Context, table, where string, args []any, max int) ([]map[string]any, bool, error) {
	rs, err := s.DB().QueryContext(ctx, `SELECT * FROM `+quoteIdent(table)+` WHERE `+where+` LIMIT ?`, append(args, max+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, false, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var out []map[string]any
	for rs.Next() {
		if len(out) == max {
			return out, true, nil
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, false, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = exportValue(strings.ToLower(c), vals[i])
		}
		out = append(out, m)
	}
	return out, false, rs.Err()
}

// exportValue makes a column value JSON-friendly: BLOB histograms are
// decoded, other BLOBs become strings.
func exportValue(col string, v any) any {
	b, ok := v.([]byte)
	if !ok {
		return v
	}
	switch col {
	case "dist_hist", "err_hist":
		return DecodeDistHist(b)
	case "sector_max":
		return DecodeSectorMax(b)
	}
	return string(b)
}
