package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func collect(t *testing.T, s *Store, name, timeCol string, span *Span, limit int64) (cols []string, rows []string) {
	t.Helper()
	err := s.ScanTable(bg, name, timeCol, span, limit,
		func(c []string) error { cols = c; return nil },
		func(v []any) error {
			parts := make([]string, len(v))
			for i, x := range v {
				parts[i] = fmt.Sprintf("%T:%v", x, x)
			}
			rows = append(rows, strings.Join(parts, ","))
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return cols, rows
}

func TestScanTablePhysicalFallback(t *testing.T) {
	s := openTest(t, manual)
	var b Batch
	b.AddEvents([]Event{{T: 100, Kind: "a"}, {T: 200, Kind: "b", ICAO: 0x3c6444}, {T: 300, Kind: "c"}})
	if err := s.Apply(bg, &b); err != nil {
		t.Fatal(err)
	}
	cols, rows := collect(t, s, "events", "t", nil, 0)
	if strings.Join(cols, ",") != "id,t,kind,icao,detail_json" || len(rows) != 3 {
		t.Fatalf("cols %v rows %v", cols, rows)
	}
	_, rows = collect(t, s, "events", "t", &Span{From: 150, To: 300}, 0)
	if len(rows) != 1 || !strings.Contains(rows[0], "string:b") {
		t.Fatalf("span rows %v", rows)
	}
	_, rows = collect(t, s, "events", "t", nil, 2)
	if len(rows) != 2 {
		t.Fatalf("limit rows %v", rows)
	}
	if n, err := LogicalCount(bg, s.DB(), KindMain, "events"); err != nil || n != 3 {
		t.Fatalf("count %d %v", n, err)
	}
}

// A registered logical table replaces the physical one (its own columns,
// order, span and limit), and Active can defer to the physical layout.
func TestScanTableLogical(t *testing.T) {
	s := openTest(t, manual)
	if err := s.Write(bg, func(tx *sql.Tx) error {
		for _, q := range []string{
			`CREATE TABLE lt_old(t INTEGER, v TEXT)`, `INSERT INTO lt_old VALUES(1, 'old')`,
			`CREATE TABLE lt_new(packed TEXT)`, `INSERT INTO lt_new VALUES('5:x'), ('2:y'), ('9:z')`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	active := true
	RegisterLogicalTable(LogicalTable{
		Name: "lt_old", Columns: []string{"t", "v"}, TimeCol: "t",
		Active: func(ctx context.Context, q Querier) (bool, error) { return active, nil },
		Scan: func(ctx context.Context, q Querier, span *Span, limit int64, fn func([]any) error) error {
			rows, err := q.QueryContext(ctx, `SELECT packed FROM lt_new`)
			if err != nil {
				return err
			}
			defer rows.Close()
			var out [][]any
			for rows.Next() {
				var p string
				if err := rows.Scan(&p); err != nil {
					return err
				}
				var tv int64
				var v string
				fmt.Sscanf(strings.Replace(p, ":", " ", 1), "%d %s", &tv, &v)
				if span != nil && (tv < span.From || tv >= span.To) {
					continue
				}
				out = append(out, []any{tv, v})
			}
			// 1.0.0 order: by t.
			for i := 1; i < len(out); i++ {
				for j := i; j > 0 && out[j][0].(int64) < out[j-1][0].(int64); j-- {
					out[j], out[j-1] = out[j-1], out[j]
				}
			}
			for i, r := range out {
				if limit > 0 && int64(i) >= limit {
					break
				}
				if err := fn(r); err != nil {
					return err
				}
			}
			return rows.Err()
		},
	})
	t.Cleanup(func() {
		logical.mu.Lock()
		delete(logical.tables, "lt_old/main")
		logical.mu.Unlock()
	})
	cols, rows := collect(t, s, "lt_old", "", nil, 0)
	if strings.Join(cols, ",") != "t,v" || strings.Join(rows, "|") != "int64:2,string:y|int64:5,string:x|int64:9,string:z" {
		t.Fatalf("logical %v %v", cols, rows)
	}
	_, rows = collect(t, s, "lt_old", "t", &Span{From: 3, To: 9}, 0)
	if strings.Join(rows, "|") != "int64:5,string:x" {
		t.Fatalf("logical span %v", rows)
	}
	if n, err := LogicalCount(bg, s.DB(), KindMain, "lt_old"); err != nil || n != 3 {
		t.Fatalf("logical count %d %v", n, err)
	}
	if names := LogicalTableNames(KindMain); !strings.Contains(strings.Join(names, ","), "lt_old") {
		t.Fatalf("names %v", names)
	}
	if _, ok := LogicalTableFor("lt_old", KindStations); ok {
		t.Fatal("registered for stations.db too")
	}
	active = false
	_, rows = collect(t, s, "lt_old", "", nil, 0)
	if strings.Join(rows, "|") != "int64:1,string:old" {
		t.Fatalf("inactive logical table not physical: %v", rows)
	}
}
