package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

// Logical tables (docs/perf/DB-EFFICIENCY-DESIGN.md §3.17).
//
// Some 1.0.0 tables changed their physical layout (positions -> trail tail
// + chunks, met_obs -> met_sample, heat key order, sealed day histograms).
// Their owners register a LogicalTable that streams the rows exactly as
// the 1.0.0 table held them: the same column names in the same order, the
// same NULLs, the same value types and the same row order as
// `SELECT * FROM <table>` returned. The CSV export (/api/db/export), the
// export list and the parity harness (tools/dbparity rowcheck) read every
// table through ScanTable, which uses the registered logical table and
// falls back to the physical table otherwise.

// Span limits a scan to TimeCol values in [From, To) (in the column's own
// unit: seconds, or day numbers).
type Span struct {
	From, To int64
}

// LogicalTable describes a 1.0.0 table whose layout changed.
type LogicalTable struct {
	Name    string
	Kinds   Kind     // database kinds (0 = KindMain)
	Columns []string // 1.0.0 columns, in order
	TimeCol string   // column the export span filters on ("" = none)
	Unit    int64    // seconds per TimeCol unit (1, 86400); 0 = 1
	// Scan calls fn for every row (values typed as database/sql scans them
	// into `any`: int64, float64, string, []byte or nil), in 1.0.0 order,
	// restricted to span when it is not nil, at most limit rows when
	// limit > 0. Returning an error from fn stops the scan with it.
	Scan func(ctx context.Context, q Querier, span *Span, limit int64, fn func(row []any) error) error
	// Count returns the row count (nil: counted through Scan).
	Count func(ctx context.Context, q Querier) (int64, error)
	// Active reports whether the logical form must be used for this
	// database (nil = always). An owner whose new layout only exists after
	// its DDL migration can check for its table here.
	Active func(ctx context.Context, q Querier) (bool, error)
}

func (t LogicalTable) kinds() Kind {
	if t.Kinds&kindAll == 0 {
		return KindMain
	}
	return t.Kinds & kindAll
}

var logical struct {
	mu     sync.RWMutex
	tables map[string]LogicalTable // name + "/" + kind
}

// RegisterLogicalTable registers (or replaces) the logical form of a 1.0.0
// table. Call it from an init function of the owner's file.
func RegisterLogicalTable(t LogicalTable) {
	if t.Name == "" || len(t.Columns) == 0 || t.Scan == nil {
		panic("store: RegisterLogicalTable: Name, Columns and Scan are required")
	}
	if t.Unit <= 0 {
		t.Unit = 1
	}
	logical.mu.Lock()
	defer logical.mu.Unlock()
	if logical.tables == nil {
		logical.tables = map[string]LogicalTable{}
	}
	for _, k := range []Kind{KindMain, KindStations} {
		if t.kinds()&k != 0 {
			logical.tables[t.Name+"/"+k.String()] = t
		}
	}
}

// LogicalTableFor returns the logical table registered for name on kind.
func LogicalTableFor(name string, kind Kind) (LogicalTable, bool) {
	logical.mu.RLock()
	defer logical.mu.RUnlock()
	t, ok := logical.tables[name+"/"+kind.String()]
	return t, ok
}

// LogicalTableNames lists the logical tables registered for kind.
func LogicalTableNames(kind Kind) []string {
	logical.mu.RLock()
	defer logical.mu.RUnlock()
	var out []string
	for _, t := range logical.tables {
		if t.kinds()&kind != 0 {
			out = append(out, t.Name)
		}
	}
	sortStringsAsc(out)
	return dedupe(out)
}

func dedupe(v []string) []string {
	out := v[:0]
	for i, s := range v {
		if i == 0 || s != v[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func sortStringsAsc(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// ScanTable streams the rows of a 1.0.0 table name of this database: the
// registered logical table when there is one, else `SELECT *` of the
// physical table (filtered on timeCol, the caller's 1.0.0 time column, when
// span is set). header is called once with the column names before the
// first row (also for an empty table). The table name must come from a
// whitelist: it is quoted, never bound.
func (s *Store) ScanTable(ctx context.Context, name, timeCol string, span *Span, limit int64,
	header func(cols []string) error, row func(vals []any) error) error {
	if s.closed.Load() {
		return ErrClosed
	}
	return ScanTableDB(ctx, s.DB(), s.kind, name, timeCol, span, limit, header, row)
}

// ScanTableDB is ScanTable on any connection to a database of kind (the
// parity harness opens the files read-only itself).
func ScanTableDB(ctx context.Context, q Querier, kind Kind, name, timeCol string, span *Span, limit int64,
	header func(cols []string) error, row func(vals []any) error) error {
	if t, ok := LogicalTableFor(name, kind); ok {
		active := true
		if t.Active != nil {
			var err error
			if active, err = t.Active(ctx, q); err != nil {
				return err
			}
		}
		if active {
			if err := header(append([]string(nil), t.Columns...)); err != nil {
				return err
			}
			return t.Scan(ctx, q, span, limit, row)
		}
	}
	return scanPhysical(ctx, q, name, timeCol, span, limit, header, row)
}

func scanPhysical(ctx context.Context, q Querier, name, timeCol string, span *Span, limit int64,
	header func(cols []string) error, row func(vals []any) error) error {
	query := `SELECT * FROM ` + quoteIdent(name)
	var args []any
	if span != nil && timeCol != "" {
		query += ` WHERE ` + quoteIdent(timeCol) + ` >= ? AND ` + quoteIdent(timeCol) + ` < ?`
		args = append(args, span.From, span.To)
	}
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
	if err := header(cols); err != nil {
		return err
	}
	return scanRowsAny(rows, len(cols), row)
}

// scanRowsAny feeds every row of rows to fn as []any (the slice is reused
// between calls).
func scanRowsAny(rows *sql.Rows, n int, fn func(vals []any) error) error {
	vals := make([]any, n)
	ptrs := make([]any, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if err := fn(vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

// LogicalCount returns the row count of a 1.0.0 table through its logical
// form (or COUNT(*) of the physical table).
func LogicalCount(ctx context.Context, q Querier, kind Kind, name string) (int64, error) {
	if t, ok := LogicalTableFor(name, kind); ok {
		active := true
		if t.Active != nil {
			var err error
			if active, err = t.Active(ctx, q); err != nil {
				return 0, err
			}
		}
		if active {
			if t.Count != nil {
				return t.Count(ctx, q)
			}
			var n int64
			err := t.Scan(ctx, q, nil, 0, func([]any) error { n++; return nil })
			return n, err
		}
	}
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(name)).Scan(&n)
	if err != nil && errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: count %s: %w", name, err)
	}
	return n, nil
}
