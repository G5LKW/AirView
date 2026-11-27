package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"
)

// migState is what the harness saw of the new build's background data
// migrations (design §5.1) through /api/db/stats.
type migState struct {
	present bool          // the build reports a migration object (1.1+)
	state   string        // final state ("done", "none", ...)
	runAt   time.Duration // first poll that saw "running", since the process start
	doneAt  time.Duration // first poll that saw "done", since the process start
	steps   int64
	err     error
}

func (m migState) note() string {
	if !m.present {
		return "data migrations: not reported by this build"
	}
	if m.runAt == 0 {
		return fmt.Sprintf("data migrations: %s (none ran in this process)", m.state)
	}
	return fmt.Sprintf("data migrations: %s; running from %.1fs to %.1fs after the start (%d steps)",
		m.state, m.runAt.Seconds(), m.doneAt.Seconds(), m.steps)
}

// window returns the kill window of the migration kills: from a little
// before the first "running" poll to the "done" poll.
func (m migState) window() (lo, hi time.Duration, ok bool) {
	if !m.present || m.runAt == 0 || m.doneAt < m.runAt {
		return 0, 0, false
	}
	lo = m.runAt - 200*time.Millisecond
	if lo < 0 {
		lo = 0
	}
	hi = m.doneAt
	if hi-lo < 300*time.Millisecond {
		hi = lo + 300*time.Millisecond
	}
	return lo, hi, true
}

type dbStatsMigration struct {
	Migration *struct {
		State      string `json:"state"`
		Migrations []struct {
			Name  string `json:"name"`
			State string `json:"state"`
			Steps int64  `json:"steps"`
		} `json:"migrations"`
	} `json:"migration"`
}

// waitMigrated polls the instance until its data migrations are done (or
// it reports none, or max passes).
func (h *harness) waitMigrated(ctx context.Context, in *instance, max time.Duration) migState {
	var st migState
	deadline := time.Now().Add(max)
	for {
		var v dbStatsMigration
		err := getJSON(ctx, in.URL(), "/api/db/stats", &v)
		since := time.Since(in.began)
		switch {
		case err != nil:
			st.err = err
		case v.Migration == nil:
			st.state, st.err = "none", nil
			return st
		default:
			st.present, st.err = true, nil
			st.state = v.Migration.State
			var steps int64
			for _, m := range v.Migration.Migrations {
				steps += m.Steps
			}
			st.steps = steps
			if st.state == "running" && st.runAt == 0 {
				st.runAt = since
			}
			if st.state == "done" || st.state == "none" {
				if steps > 0 && st.runAt == 0 {
					st.runAt = since // finished between two polls
				}
				st.doneAt = since
				return st
			}
			if st.state == "failed" {
				st.err = fmt.Errorf("a data migration failed: %+v", v.Migration.Migrations)
			}
		}
		if time.Now().After(deadline) {
			if st.err == nil {
				st.err = fmt.Errorf("not done after %s (state %q)", max, st.state)
			}
			return st
		}
		select {
		case <-ctx.Done():
			st.err = ctx.Err()
			return st
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// migProgress summarises schema_data_migrations of a stopped data
// directory ("3/8 done" or "none").
func migProgress(ctx context.Context, dir string) string {
	db, err := openDB(filepath.Join(dir, "airview.db"), true)
	if err != nil {
		return "unreadable"
	}
	defer db.Close()
	var total, done sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(done = 2) FROM schema_data_migrations`).Scan(&total, &done); err != nil {
		return "none recorded"
	}
	return fmt.Sprintf("%d/%d complete", done.Int64, total.Int64)
}
