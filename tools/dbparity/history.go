package main

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// History synthesis ("N simulated hours" without a controllable clock).
//
// AirView timestamps everything with the wall clock (feed manager:
// RawFrame.Recv = ntp.Now()), and the 1.0.0 build has no clock override, so
// hours of history cannot be fed in faster than real time. Instead the
// fixture is built in sessions: the OLD build records a few minutes of
// simulated traffic, is stopped, and every time value in its databases is
// moved back by whole days (shiftDays); then the next session records
// again. After K sessions the database holds K recording windows spread
// over K-1 days of history, all written by the old build itself: the
// rollups (minute -> hour -> day), month buckets and retention tiers then
// run on real old-format data when the builds under test start on copies.
//
// Shifting is schema-agnostic: every INTEGER column whose name looks like a
// time (timeColumn) is moved when its value lies in a plausible window
// around now, in the unit its magnitude shows (Unix s, Unix ms, Unix µs,
// day number, hour number, minute number). Values outside that window
// (counters, IDs, bins) are left alone. Whole days keep day / hour / minute
// keys aligned. kv values that are bare integers in such a window are
// moved too (rollup / pruning watermarks). JSON blobs are not rewritten.

// timeColumn matches the column names that hold times in AirView's schemas
// (t, ts, day, hour, minute, start, end, first/last(_seen), updated,
// issued, applied, created, expires, last_t, *_at, *_ms, *_t, *_ts, ...).
var timeColumn = regexp.MustCompile(`^(t|ts|t0|t1|day|hour|minute|month|start|end|first|last|first_seen|last_seen|seen|updated|issued|applied|created|expires|expiry|until|since|valid_from|valid_to|fetched|last_t|last_ok|last_err|.*_at|.*_t|.*_ts|.*_ms|.*_time|.*_day|.*_hour|.*_seen)$`)

// timeUnit is one representation of an instant: a scale relative to Unix
// seconds and a window of plausible values.
type timeUnit struct {
	name   string
	perSec float64 // units per second (ms: 1000; day: 1/86400)
}

var timeUnits = []timeUnit{
	{"s", 1}, {"ms", 1e3}, {"us", 1e6}, {"day", 1.0 / 86400}, {"hour", 1.0 / 3600}, {"minute", 1.0 / 60},
}

// window returns the [lo, hi] value range of u that counts as a time: from
// 20 years ago to 30 days ahead (forecasts reach a few days ahead).
func (u timeUnit) window(now time.Time) (int64, int64) {
	lo := float64(now.Add(-20*365*24*time.Hour).Unix()) * u.perSec
	hi := float64(now.Add(30*24*time.Hour).Unix()) * u.perSec
	return int64(lo), int64(hi)
}

// shiftReport counts what shiftDays moved.
type shiftReport struct {
	Tables  int
	Columns []string // table.column(unit)
	KV      int
}

// shiftDays moves every time value of the database at path back by days
// whole days. The process owning the database must be stopped.
func shiftDays(ctx context.Context, path string, days int) (*shiftReport, error) {
	db, err := openDB(path, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tabs, err := listTables(ctx, db)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rep := &shiftReport{}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, t := range tabs {
		if t.Virtual || t.Rows == 0 || t.Name == "schema_migrations" {
			continue
		}
		var exprs []string
		changed := false
		for _, c := range t.Cols {
			e := quoteIdent(c.Name)
			if strings.Contains(strings.ToUpper(c.Type), "INT") && timeColumn.MatchString(strings.ToLower(c.Name)) {
				var cases []string
				for _, u := range timeUnits {
					lo, hi := u.window(now)
					d := int64(float64(days*86400) * u.perSec)
					if d == 0 {
						continue
					}
					cases = append(cases, fmt.Sprintf("WHEN %s BETWEEN %d AND %d THEN %s - %d", e, lo, hi, e, d))
				}
				// Units overlap in magnitude only where the windows do not
				// (s ~1.8e9, ms ~1.8e12, µs ~1.8e15, day ~2e4, hour ~5e5,
				// minute ~3e7), so the first matching window decides.
				e = "CASE " + strings.Join(cases, " ") + " ELSE " + e + " END"
				var n int64
				_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+quoteIdent(t.Name)+` WHERE `+quoteIdent(c.Name)+` IS NOT NULL`).Scan(&n)
				if n > 0 {
					changed = true
					rep.Columns = append(rep.Columns, t.Name+"."+c.Name)
				}
			}
			exprs = append(exprs, e)
		}
		if !changed && t.Name != "kv" {
			continue
		}
		if t.Name == "kv" {
			if err := shiftKV(ctx, tx, days, now, &rep.KV); err != nil {
				return nil, err
			}
		}
		if !changed {
			continue
		}
		// Copy out, delete, re-insert shifted: an in-place UPDATE of a key
		// column can collide with a row that has not been moved yet.
		tmp := quoteIdent("dbparity_shift_" + t.Name)
		names := make([]string, len(t.Cols))
		for i, c := range t.Cols {
			names[i] = quoteIdent(c.Name)
		}
		stmts := []string{
			`DROP TABLE IF EXISTS temp.` + tmp,
			`CREATE TEMP TABLE ` + tmp + ` AS SELECT * FROM ` + quoteIdent(t.Name),
			`DELETE FROM ` + quoteIdent(t.Name),
			`INSERT INTO ` + quoteIdent(t.Name) + `(` + strings.Join(names, ",") + `) SELECT ` + strings.Join(exprs, ",") + ` FROM temp.` + tmp,
			`DROP TABLE temp.` + tmp,
		}
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				return nil, fmt.Errorf("shift %s: %w (%.200s)", t.Name, err, s)
			}
		}
		rep.Tables++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return nil, err
	}
	return rep, nil
}

// shiftKV moves kv values that are bare integers in a time window.
func shiftKV(ctx context.Context, tx *sql.Tx, days int, now time.Time, n *int) error {
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM kv`)
	if err != nil {
		return err
	}
	type kv struct{ k, v string }
	var upd []kv
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		x, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		for _, u := range timeUnits {
			lo, hi := u.window(now)
			if x >= lo && x <= hi {
				upd = append(upd, kv{k, strconv.FormatInt(x-int64(float64(days*86400)*u.perSec), 10)})
				break
			}
		}
	}
	rows.Close()
	for _, e := range upd {
		if _, err := tx.ExecContext(ctx, `UPDATE kv SET value = ? WHERE key = ?`, e.v, e.k); err != nil {
			return err
		}
		*n++
	}
	return nil
}
