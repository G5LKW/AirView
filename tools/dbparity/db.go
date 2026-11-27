package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // driver "sqlite"
)

// openDB opens a SQLite database of a scratch data directory. ro opens it
// read-only (safe next to a running instance: WAL readers never block the
// writer).
func openDB(path string, ro bool) (*sql.DB, error) {
	q := url.Values{}
	q.Set("_busy_timeout", "10000")
	if ro {
		q.Set("mode", "ro")
	}
	dsn := "file:" + filepath.ToSlash(path) + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// column is one column of a table.
type column struct {
	Name string
	Type string
	PK   int
}

// tableInfo is one table of a database.
type tableInfo struct {
	Name    string
	Cols    []column
	Rows    int64
	Virtual bool
}

// listTables returns the ordinary tables of db (no sqlite_* internals), sorted.
func listTables(ctx context.Context, db *sql.DB) ([]tableInfo, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []tableInfo
	for rows.Next() {
		var name string
		var ddl sql.NullString
		if err := rows.Scan(&name, &ddl); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, tableInfo{Name: name, Virtual: strings.HasPrefix(strings.ToUpper(ddl.String), "CREATE VIRTUAL")})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cr, err := db.QueryContext(ctx, `SELECT name, type, pk FROM pragma_table_info(?)`, out[i].Name)
		if err != nil {
			return nil, err
		}
		for cr.Next() {
			var c column
			if err := cr.Scan(&c.Name, &c.Type, &c.PK); err != nil {
				cr.Close()
				return nil, err
			}
			out[i].Cols = append(out[i].Cols, c)
		}
		cr.Close()
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+quoteIdent(out[i].Name)).Scan(&out[i].Rows); err != nil {
			return nil, fmt.Errorf("count %s: %w", out[i].Name, err)
		}
	}
	return out, nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// dbFiles returns the *.db files of a data directory, sorted by name.
func dbFiles(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "*.db"))
	sort.Strings(m)
	return m
}

// dumpSchema writes every table of every database of dir with its row count
// and, for each integer column, the min / max value (to see time units).
func dumpSchema(ctx context.Context, w io.Writer, dir string) error {
	for _, f := range dbFiles(dir) {
		db, err := openDB(f, true)
		if err != nil {
			return err
		}
		tabs, err := listTables(ctx, db)
		if err != nil {
			db.Close()
			return fmt.Errorf("%s: %w", f, err)
		}
		fmt.Fprintf(w, "== %s\n", filepath.Base(f))
		for _, t := range tabs {
			fmt.Fprintf(w, "%-28s rows=%d\n", t.Name, t.Rows)
			if t.Rows == 0 {
				continue
			}
			for _, c := range t.Cols {
				if !strings.Contains(strings.ToUpper(c.Type), "INT") {
					continue
				}
				var lo, hi sql.NullInt64
				_ = db.QueryRowContext(ctx, `SELECT min(`+quoteIdent(c.Name)+`), max(`+quoteIdent(c.Name)+`) FROM `+quoteIdent(t.Name)).Scan(&lo, &hi)
				fmt.Fprintf(w, "    %-20s %-8s %d .. %d\n", c.Name, c.Type, lo.Int64, hi.Int64)
			}
		}
		db.Close()
	}
	return nil
}

// copyDir copies the regular files of src into dst (created), recursively.
// SQLite files must be copied while no process has them open.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
