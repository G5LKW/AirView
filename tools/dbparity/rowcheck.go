package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"airview/internal/store"
)

// Row-level comparison of two stopped data directories.
//
// For every table of every database, the rows up to cutoff (by the
// table's time column, so rows the instances wrote after their start - idle
// minutes, receiver status - are left out) are compared:
//
//   - same columns in both: every row, every column (sorted by all
//     columns, numbers compared exactly, REAL within 1e-9);
//   - columns changed by the new build: the logical extract below for that
//     table must exist and agree (an efficiency change may re-encode a
//     table, e.g. a new column layout, but must keep what the extract
//     reads); a changed table without an extract fails the check, so the
//     harness is extended in the same change;
//   - table missing in new: fails unless it has a logical extract that
//     reads the data from where the new build keeps it.
//
// Excluded tables (rowSkip) hold process bookkeeping, not data.

// rowSkip are tables that are not compared row by row, with the reason.
var rowSkip = map[string]string{
	"schema_migrations": "migration bookkeeping (the new build may add migrations; checked separately: every old migration must still be recorded)",
	"kv":                "maintenance watermarks and last-run times (when PRAGMA optimize / prune ran)",
}

// rowSkipCols are columns that are not compared (table.column), with the
// reason.
var rowSkipCols = map[string]string{
	"alerts_rule.updated": "written when the built-in rules are (re)seeded at start",
}

// rowAbs are the numeric columns compared within an absolute tolerance
// (table.column -> tolerance), each with its reason.
//
// met-precision (design §3.9): met observations are stored as scaled
// integers (lat/lon 1e-5 deg, wind direction 0.1 deg, wind 0.1 kt, OAT
// 0.01 C), finer than their BDS sources; the logical met_obs rows carry the
// rounding of the scaled value (the same 0.06 as the met answers).
var rowAbs = map[string]float64{
	"met_obs.lat": 0.06, "met_obs.lon": 0.06, "met_obs.wind_dir": 0.06,
	"met_obs.wind_kt": 0.06, "met_obs.oat_c": 0.06,
}

// trail-retention: 1.0.0 prunes the trail of a flight that ended before
// the trail retention only when the flight row counts positions; a point
// that arrived after the flight row's last update (positions = 0) is never
// deleted. The 1.1 trail pruner deletes every point past the retention.
// The logical positions rows of the flights that the old build itself has
// already passed with its prune watermark (store.positions_pruned) are
// therefore not compared.
func prunedFlights(ctx context.Context, old *sql.DB) (map[int64]bool, error) {
	ids, err := old.QueryContext(ctx, `SELECT id FROM flights WHERE "end" < (
  SELECT CAST(value AS INTEGER) FROM kv WHERE key = 'store.positions_pruned')`)
	if err != nil {
		return nil, err
	}
	defer ids.Close()
	out := map[int64]bool{}
	for ids.Next() {
		var id int64
		if err := ids.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, ids.Err()
}

// logicalExtract is a table's data reduced to what the features read; used
// when the new build changes the table's layout. old and new are the
// queries against the 1.0.0 schema and the new one; both must return the
// same columns. A new layout adds its query here.
type logicalExtract struct {
	old, new string
}

var logicalExtracts = map[string]logicalExtract{
	"positions": {
		old: `SELECT flight_id, t, lat_e5, lon_e5, alt, gs, track, rssi_x10, src FROM positions WHERE t <= ? ORDER BY flight_id, t`,
		new: `SELECT flight_id, t, lat_e5, lon_e5, alt, gs, track, rssi_x10, src FROM positions WHERE t <= ? ORDER BY flight_id, t`,
	},
	"flights": {
		old: `SELECT id, icao, callsign, squawk, start, "end", msgs, positions, src_mask, max_dist_km, min_alt, max_alt FROM flights WHERE start <= ? ORDER BY id`,
		new: `SELECT id, icao, callsign, squawk, start, "end", msgs, positions, src_mask, max_dist_km, min_alt, max_alt FROM flights WHERE start <= ? ORDER BY id`,
	},
	"stats_hour": {
		old: `SELECT t, ant, frames, positions, aircraft, adsb, mlat FROM stats_hour WHERE t <= ? ORDER BY t, ant`,
		new: `SELECT t, ant, frames, positions, aircraft, adsb, mlat FROM stats_hour WHERE t <= ? ORDER BY t, ant`,
	},
	"stats_minute": {
		old: `SELECT t, ant, frames, positions, aircraft FROM stats_minute WHERE t <= ? ORDER BY t, ant`,
		new: `SELECT t, ant, frames, positions, aircraft FROM stats_minute WHERE t <= ? ORDER BY t, ant`,
	},
}

// tableResult is the outcome for one table.
type tableResult struct {
	DB, Table string
	Mode      string // full | logical | skipped | missing
	RowsA     int64
	RowsB     int64
	Diffs     []string
	Note      string
}

// compareDirs compares the databases of two stopped data directories.
// cutoff is in Unix seconds (rows with a later time are ignored).
func compareDirs(ctx context.Context, dirA, dirB string, cutoff time.Time) ([]tableResult, error) {
	var out []tableResult
	for _, fa := range dbFiles(dirA) {
		name := filepath.Base(fa)
		fb := filepath.Join(dirB, name)
		dba, err := openDB(fa, true)
		if err != nil {
			return nil, err
		}
		dbb, err := openDB(fb, true)
		if err != nil {
			dba.Close()
			return nil, err
		}
		res, err := compareDBs(ctx, name, dba, dbb, cutoff)
		dba.Close()
		dbb.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, res...)
	}
	return out, nil
}

func compareDBs(ctx context.Context, name string, a, b *sql.DB, cutoff time.Time) ([]tableResult, error) {
	ta, err := listTables(ctx, a)
	if err != nil {
		return nil, err
	}
	tb, err := listTables(ctx, b)
	if err != nil {
		return nil, fmt.Errorf("new database unreadable: %w", err)
	}
	byName := map[string]tableInfo{}
	for _, t := range tb {
		byName[t.Name] = t
	}
	var out []tableResult
	for _, t := range ta {
		r := tableResult{DB: name, Table: t.Name, RowsA: t.Rows}
		if why, ok := rowSkip[t.Name]; ok {
			r.Mode, r.Note = "skipped", why
			if bt, ok := byName[t.Name]; ok {
				r.RowsB = bt.Rows
			}
			out = append(out, r)
			continue
		}
		bt, ok := byName[t.Name]
		if ok {
			r.RowsB = bt.Rows
		}
		sameCols := ok && colNames(t) == colNames(bt)
		ex, hasEx := logicalExtracts[t.Name]
		// A table whose layout the new build changed is read through the
		// store's logical-table registry (the same code that serves the
		// CSV export): it must yield exactly the 1.0.0 rows.
		if lt, lok := newLogical(ctx, name, t.Name, b); lok {
			r.Mode = "logical"
			r.Note = "store logical table"
			var nb int64
			r.Diffs, nb, err = compareLogical(ctx, a, b, name, t, lt, cutoff)
			r.RowsB = nb
			if err != nil {
				r.Diffs = append(r.Diffs, "logical scan failed: "+err.Error())
				err = nil
			}
			out = append(out, r)
			continue
		}
		switch {
		case sameCols:
			r.Mode = "full"
			r.Diffs, err = compareRows(ctx, a, b, fullQuery(t, cutoff), nil, t.Name)
		case hasEx:
			r.Mode = "logical"
			r.Diffs, err = compareRows(ctx, a, b, ex.old, &ex.new, t.Name, cutoff.Unix())
		case !ok:
			r.Mode = "missing"
			r.Diffs = []string{"table is missing in the new database and has no logical extract (rowcheck.go logicalExtracts)"}
		default:
			r.Mode = "changed"
			r.Diffs = []string{fmt.Sprintf("columns changed (old %s, new %s) and no logical extract exists (rowcheck.go logicalExtracts)", colNames(t), colNames(bt))}
		}
		if err != nil {
			r.Diffs = append(r.Diffs, "query failed: "+err.Error())
			err = nil
		}
		out = append(out, r)
	}
	return out, nil
}

func colNames(t tableInfo) string {
	var n []string
	for _, c := range t.Cols {
		n = append(n, c.Name+" "+strings.ToUpper(c.Type))
	}
	sort.Strings(n)
	return strings.Join(n, ",")
}

// fullQuery selects every compared column of t, rows up to cutoff by the
// table's time column (the first INTEGER column with a time name; the
// cutoff is expressed in that column's unit, found from its values).
func fullQuery(t tableInfo, cutoff time.Time) string {
	var cols []string
	for _, c := range t.Cols {
		if _, skip := rowSkipCols[t.Name+"."+c.Name]; skip {
			continue
		}
		cols = append(cols, quoteIdent(c.Name))
	}
	where := ""
	for _, c := range t.Cols {
		if strings.Contains(strings.ToUpper(c.Type), "INT") && timeColumn.MatchString(strings.ToLower(c.Name)) {
			// Every unit's cutoff: a value counts as "after the cutoff"
			// when it is in a unit's window and beyond that unit's cutoff.
			var conds []string
			for _, u := range timeUnits {
				_, hi := u.window(cutoff)
				cv := int64(float64(cutoff.Unix()) * u.perSec)
				conds = append(conds, fmt.Sprintf("(%s BETWEEN %d AND %d)", quoteIdent(c.Name), cv+1, hi))
			}
			where = " WHERE NOT (" + strings.Join(conds, " OR ") + ")"
			break
		}
	}
	return "SELECT " + strings.Join(cols, ",") + " FROM " + quoteIdent(t.Name) + where + " ORDER BY " + strings.Join(cols, ",")
}

// compareRows runs qa on a and qb (default qa) on b and compares the rows
// in order.
func compareRows(ctx context.Context, a, b *sql.DB, qa string, qb *string, table string, args ...any) ([]string, error) {
	if qb == nil {
		qb = &qa
	}
	ra, err := a.QueryContext(ctx, qa, args...)
	if err != nil {
		return nil, fmt.Errorf("old: %w", err)
	}
	defer ra.Close()
	rb, err := b.QueryContext(ctx, *qb, args...)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}
	defer rb.Close()
	ca, _ := ra.Columns()
	var diffs []string
	add := func(s string) {
		if len(diffs) < 8 {
			diffs = append(diffs, s)
		} else if len(diffs) == 8 {
			diffs = append(diffs, "...")
		}
	}
	va := make([]any, len(ca))
	vb := make([]any, len(ca))
	pa := make([]any, len(ca))
	pb := make([]any, len(ca))
	for i := range va {
		pa[i], pb[i] = &va[i], &vb[i]
	}
	n, extraA, extraB := 0, 0, 0
	for {
		hasA, hasB := ra.Next(), rb.Next()
		if !hasA && !hasB {
			break
		}
		if hasA && !hasB {
			extraA++
			continue
		}
		if hasB && !hasA {
			extraB++
			continue
		}
		if err := ra.Scan(pa...); err != nil {
			return diffs, err
		}
		if err := rb.Scan(pb...); err != nil {
			return diffs, err
		}
		n++
		for i := range ca {
			if !sqlEqual(va[i], vb[i]) {
				add(fmt.Sprintf("row %d %s: old %s, new %s", n, ca[i], sqlText(va[i]), sqlText(vb[i])))
				break
			}
		}
	}
	if extraA > 0 || extraB > 0 {
		add(fmt.Sprintf("row count: old has %d more, new has %d more (compared %d)", extraA, extraB, n))
	}
	if err := ra.Err(); err != nil {
		return diffs, err
	}
	return diffs, rb.Err()
}

// sqlNear is sqlEqual, with numbers allowed to differ by up to abs.
func sqlNear(a, b any, abs float64) bool {
	if sqlEqual(a, b) {
		return true
	}
	if abs <= 0 {
		return false
	}
	x, ok1 := sqlNum(a)
	y, ok2 := sqlNum(b)
	return ok1 && ok2 && math.Abs(x-y) <= abs
}

func sqlNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func sqlEqual(a, b any) bool {
	switch x := a.(type) {
	case float64:
		switch y := b.(type) {
		case float64:
			d := x - y
			if d < 0 {
				d = -d
			}
			s := max(x, -x, y, -y)
			return d <= 1e-9*s || d == 0
		case int64:
			return x == float64(y)
		}
	case int64:
		if y, ok := b.(float64); ok {
			return float64(x) == y
		}
	case []byte:
		if y, ok := b.([]byte); ok {
			return string(x) == string(y)
		}
		if y, ok := b.(string); ok {
			return string(x) == y
		}
	case string:
		if y, ok := b.([]byte); ok {
			return x == string(y)
		}
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func sqlText(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		if len(x) > 32 {
			return fmt.Sprintf("blob(%d)", len(x))
		}
		return fmt.Sprintf("%q", x)
	case string:
		return trunc(fmt.Sprintf("%q", x))
	}
	return fmt.Sprint(v)
}

// checkMigrations verifies that every migration recorded in the old
// database is still recorded in the new one, and PRAGMA integrity_check.
func checkMigrations(ctx context.Context, dirOld, dirNew string) []string {
	var probs []string
	for _, fa := range dbFiles(dirOld) {
		name := filepath.Base(fa)
		fb := filepath.Join(dirNew, name)
		a, err := openDB(fa, true)
		if err != nil {
			probs = append(probs, name+": "+err.Error())
			continue
		}
		b, err := openDB(fb, true)
		if err != nil {
			a.Close()
			probs = append(probs, name+": new: "+err.Error())
			continue
		}
		var ic string
		if err := b.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
			probs = append(probs, fmt.Sprintf("%s: integrity_check: %s %v", name, ic, err))
		}
		old := migrations(ctx, a)
		nw := migrations(ctx, b)
		for k := range old {
			if !nw[k] {
				probs = append(probs, fmt.Sprintf("%s: migration %s recorded in the old database is missing in the new one", name, k))
			}
		}
		a.Close()
		b.Close()
	}
	return probs
}

func migrations(ctx context.Context, db *sql.DB) map[string]bool {
	m := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name, version FROM schema_migrations`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		var v int
		if rows.Scan(&n, &v) == nil {
			m[fmt.Sprintf("%s/%d", n, v)] = true
		}
	}
	return m
}

// migrationList returns the recorded migrations of a database (sorted).
func migrationList(ctx context.Context, path string) []string {
	db, err := openDB(path, true)
	if err != nil {
		return nil
	}
	defer db.Close()
	var out []string
	for k := range migrations(ctx, db) {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dbKind maps a database file name to its store kind.
func dbKind(file string) (store.Kind, bool) {
	switch file {
	case store.FileName:
		return store.KindMain, true
	case store.StationsFileName:
		return store.KindStations, true
	}
	return 0, false
}

// newLogical returns the logical table the new build registered for a
// 1.0.0 table, when it is active on the new database.
func newLogical(ctx context.Context, file, table string, b *sql.DB) (store.LogicalTable, bool) {
	kind, ok := dbKind(file)
	if !ok {
		return store.LogicalTable{}, false
	}
	lt, ok := store.LogicalTableFor(table, kind)
	if !ok {
		return lt, false
	}
	if lt.Active != nil {
		if on, err := lt.Active(ctx, b); err != nil || !on {
			return lt, false
		}
	}
	return lt, true
}

// compareLogical compares the 1.0.0 rows of table t (old, physical) with
// the rows the new build's logical table yields, both restricted to rows up
// to cutoff and compared as sorted multisets (row order is checked by the
// byte comparison of the CSV export).
func compareLogical(ctx context.Context, a, b *sql.DB, file string, t tableInfo, lt store.LogicalTable, cutoff time.Time) ([]string, int64, error) {
	kind, _ := dbKind(file)
	oldCols := map[string]bool{}
	for _, c := range t.Cols {
		oldCols[c.Name] = true
	}
	var sel []string
	for _, c := range lt.Columns {
		if !oldCols[c] {
			return []string{fmt.Sprintf("logical column %q is not a 1.0.0 column of %s", c, t.Name)}, 0, nil
		}
		sel = append(sel, quoteIdent(c))
	}
	if len(lt.Columns) != len(t.Cols) {
		return []string{fmt.Sprintf("logical table has %d columns, 1.0.0 had %d", len(lt.Columns), len(t.Cols))}, 0, nil
	}
	tcol := -1
	for i, c := range lt.Columns {
		for _, oc := range t.Cols {
			if oc.Name == c && strings.Contains(strings.ToUpper(oc.Type), "INT") && timeColumn.MatchString(strings.ToLower(c)) {
				tcol = i
			}
		}
		if tcol >= 0 {
			break
		}
	}
	keep := func(row []any) bool {
		if tcol < 0 {
			return true
		}
		v, ok := row[tcol].(int64)
		if !ok {
			return true
		}
		return !afterCutoff(v, cutoff)
	}
	var skip []int
	for i, c := range lt.Columns {
		if _, ok := rowSkipCols[t.Name+"."+c]; ok {
			skip = append(skip, i)
		}
	}
	clean := func(row []any) []any {
		out := append([]any(nil), row...)
		for i, v := range out {
			if bs, ok := v.([]byte); ok {
				out[i] = append([]byte(nil), bs...)
			}
		}
		for _, i := range skip {
			out[i] = nil
		}
		return out
	}

	if t.Name == "positions" && len(lt.Columns) > 0 && lt.Columns[0] == "flight_id" {
		pruned, err := prunedFlights(ctx, a)
		if err != nil && !strings.Contains(err.Error(), "no such") {
			return nil, 0, fmt.Errorf("old: %w", err)
		}
		if len(pruned) > 0 {
			inner := keep
			keep = func(row []any) bool {
				if id, ok := row[0].(int64); ok && pruned[id] {
					return false
				}
				return inner(row)
			}
		}
	}

	var oldRows [][]any
	rs, err := a.QueryContext(ctx, `SELECT `+strings.Join(sel, ",")+` FROM `+quoteIdent(t.Name))
	if err != nil {
		return nil, 0, fmt.Errorf("old: %w", err)
	}
	vals := make([]any, len(sel))
	ptrs := make([]any, len(sel))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rs.Next() {
		if err := rs.Scan(ptrs...); err != nil {
			rs.Close()
			return nil, 0, err
		}
		if keep(vals) {
			oldRows = append(oldRows, clean(vals))
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, 0, err
	}

	var newRows [][]any
	var cols []string
	err = store.ScanTableDB(ctx, b, kind, t.Name, "", nil, 0,
		func(c []string) error { cols = c; return nil },
		func(row []any) error {
			if keep(row) {
				newRows = append(newRows, clean(row))
			}
			return nil
		})
	if err != nil {
		return nil, int64(len(newRows)), fmt.Errorf("new: %w", err)
	}
	var diffs []string
	if strings.Join(cols, ",") != strings.Join(lt.Columns, ",") {
		diffs = append(diffs, fmt.Sprintf("logical header %v", cols))
	}
	sortRows(oldRows)
	sortRows(newRows)
	n := min(len(oldRows), len(newRows))
	tol := make([]float64, len(lt.Columns))
	for j, c := range lt.Columns {
		tol[j] = rowAbs[t.Name+"."+c]
	}
	for i := 0; i < n && len(diffs) < 8; i++ {
		for j := range lt.Columns {
			if !sqlNear(oldRows[i][j], newRows[i][j], tol[j]) {
				diffs = append(diffs, fmt.Sprintf("row %d %s: old %s, new %s", i+1, lt.Columns[j], sqlText(oldRows[i][j]), sqlText(newRows[i][j])))
				break
			}
		}
	}
	if len(oldRows) != len(newRows) {
		diffs = append(diffs, fmt.Sprintf("row count: old %d, new %d", len(oldRows), len(newRows)))
	}
	return diffs, int64(len(newRows)), nil
}

// afterCutoff reports whether v, read as a time in any plausible unit, is
// later than cutoff (the rule of fullQuery).
func afterCutoff(v int64, cutoff time.Time) bool {
	for _, u := range timeUnits {
		_, hi := u.window(cutoff)
		cv := int64(float64(cutoff.Unix()) * u.perSec)
		if v >= cv+1 && v <= hi {
			return true
		}
	}
	return false
}

// sortRows orders rows by all columns (NULL < numbers < text < blobs).
func sortRows(rows [][]any) {
	rank := func(v any) int {
		switch v.(type) {
		case nil:
			return 0
		case int64, float64:
			return 1
		case string:
			return 2
		}
		return 3
	}
	num := func(v any) float64 {
		switch x := v.(type) {
		case int64:
			return float64(x)
		case float64:
			return x
		}
		return 0
	}
	str := func(v any) string {
		switch x := v.(type) {
		case string:
			return x
		case []byte:
			return string(x)
		}
		return ""
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for k := range rows[i] {
			a, b := rows[i][k], rows[j][k]
			ra, rb := rank(a), rank(b)
			if ra != rb {
				return ra < rb
			}
			switch ra {
			case 1:
				if x, y := num(a), num(b); x != y {
					return x < y
				}
			case 2, 3:
				if x, y := str(a), str(b); x != y {
					return x < y
				}
			}
		}
		return false
	})
}
