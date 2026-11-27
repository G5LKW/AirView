package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"airview/internal/model"
)

// ---------------------------------------------------------------- deletion

// DeleteStation queues a station for deletion (admin, owner erasure or
// the offline purge). The station disappears from every lookup at once
// (it is marked deleting and its public point and open claim codes are
// removed in the same transaction); the deletion job then removes its rows
// from every table of stations.db key range by key range, removes its
// locations, logs and keys, and pseudonymises its audit entries. The queue
// survives restarts. The host station cannot be deleted.
func (r *Registry) DeleteStation(ctx context.Context, actor Actor, idx model.StationIdx, reason string) error {
	if idx == model.HostStation {
		return fmt.Errorf("%w: the host station cannot be deleted", ErrInvalid)
	}
	var changed []*Station
	return r.write(ctx, func(tx *sql.Tx) error {
		changed = nil
		cur := r.cache.load(idx)
		if cur == nil {
			return ErrStationNotFound
		}
		if cur.Deleting {
			return nil
		}
		next := *cur
		next.Deleting = true
		if err := saveStation(ctx, tx, &next); err != nil {
			return err
		}
		// Drops the public point and re-applies the rural guard to the
		// neighbours in the station's 1° cell.
		var err error
		if changed, err = r.refreshPublic(ctx, tx, cur, &next); err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM station_public WHERE idx=?`, `DELETE FROM claims WHERE idx=?`} {
			if _, err := tx.ExecContext(ctx, q, idx); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO deletions(idx, sid, feeder_id, queued, reason) VALUES(?,?,?,?,?)`,
			idx, cur.SID, cur.FeederID, r.now().Unix(), reason); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, r.now(), actor, "station.delete", cur.SID, fmt.Sprintf(`{"reason":%q}`, reason)); err != nil {
			return err
		}
		return nil
	}, func() {
		for _, c := range changed {
			r.cache.put(c)
		}
	})
}

// tableInfo describes a table of stations.db outside the registry.
type tableInfo struct {
	name   string
	cols   map[string]bool
	rowid  bool
	quoted string
}

func (r *Registry) foreignTables(ctx context.Context) ([]tableInfo, error) {
	rows, err := r.wdb.QueryContext(ctx, `SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	var out []tableInfo
	for rows.Next() {
		var t tableInfo
		var ddl string
		if err := rows.Scan(&t.name, &ddl); err != nil {
			rows.Close()
			return nil, err
		}
		if registryTables[t.name] {
			continue
		}
		t.rowid = !strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID")
		t.quoted = `"` + strings.ReplaceAll(t.name, `"`, `""`) + `"`
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].cols = map[string]bool{}
		crow, err := r.wdb.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, out[i].name)
		if err != nil {
			return nil, err
		}
		for crow.Next() {
			var c string
			if err := crow.Scan(&c); err != nil {
				crow.Close()
				return nil, err
			}
			out[i].cols[c] = true
		}
		crow.Close()
	}
	return out, nil
}

// deleteBatched runs DELETE FROM t WHERE <where> in batches of
// Options.DeleteBatch rows (rowid tables), each batch its own transaction
// so the writer is never held for long. It returns the rows removed.
func (r *Registry) deleteBatched(ctx context.Context, t tableInfo, where string, args ...any) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var n int64
		q := `DELETE FROM ` + t.quoted + ` WHERE ` + where
		if t.rowid {
			q = `DELETE FROM ` + t.quoted + ` WHERE rowid IN (SELECT rowid FROM ` + t.quoted + ` WHERE ` + where +
				fmt.Sprintf(` LIMIT %d)`, r.opts.DeleteBatch)
		}
		err := r.write(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, q, args...)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		}, nil)
		if err != nil {
			return total, fmt.Errorf("registry: delete from %s: %w", t.name, err)
		}
		total += n
		if !t.rowid || n < int64(r.opts.DeleteBatch) {
			return total, nil
		}
	}
}

// antRange returns the WHERE clause and arguments selecting the statistics
// keys of a station: "s<idx>" and every "s<idx>/<channel>" (a key range,
// '/'+1 == '0').
func antRange(idx model.StationIdx) (string, []any) {
	k := model.StationAntKey(idx, -1)
	return `(ant = ? OR (ant >= ? AND ant < ?))`, []any{k, k + "/", k + "0"}
}

// ProcessDeletions runs the queued station deletions to completion and
// returns how many stations were removed.
func (r *Registry) ProcessDeletions(ctx context.Context) (int, error) {
	type job struct {
		idx      model.StationIdx
		sid      string
		feederID string
	}
	rows, err := r.rdb.QueryContext(ctx, `SELECT idx, sid, feeder_id FROM deletions ORDER BY queued, idx`)
	if err != nil {
		return 0, err
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.idx, &j.sid, &j.feederID); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if len(jobs) == 0 {
		return 0, rows.Err()
	}
	tables, err := r.foreignTables(ctx)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, j := range jobs {
		// 0. Stop the station's statistics producers and drain their rows.
		if r.opts.BeforeDelete != nil {
			if err := r.opts.BeforeDelete(ctx, j.idx); err != nil {
				return done, fmt.Errorf("registry: deleting station %s: %w", j.sid, err)
			}
		}
		// 1. Namespaced statistics and other station-keyed tables.
		for _, t := range tables {
			if t.cols["ant"] {
				w, a := antRange(j.idx)
				if _, err := r.deleteBatched(ctx, t, w, a...); err != nil {
					return done, err
				}
			}
			if t.cols["sid"] {
				if _, err := r.deleteBatched(ctx, t, `sid = ?`, j.sid); err != nil {
					return done, err
				}
			}
			if t.cols["feed"] {
				p := "st/" + j.sid + "/"
				if _, err := r.deleteBatched(ctx, t, `(feed >= ? AND feed < ?)`, p, "st/"+j.sid+"0"); err != nil {
					return done, err
				}
			}
		}
		// Rows of the station kept outside stations.db (airview.db).
		if r.opts.DeleteExtra != nil {
			if err := r.opts.DeleteExtra(ctx, j.idx, j.sid); err != nil {
				return done, fmt.Errorf("registry: deleting station %s outside stations.db: %w", j.sid, err)
			}
		}
		// 2. The station, its registry rows and keys, and the audit
		// pseudonyms, in one transaction.
		ps := r.pseudonym("station", j.sid)
		err := r.write(ctx, func(tx *sql.Tx) error {
			for _, q := range []string{
				`DELETE FROM conn_log WHERE idx=?`,
				`DELETE FROM location_estimates WHERE idx=?`,
				`DELETE FROM station_public WHERE idx=?`,
				`DELETE FROM claims WHERE idx=?`,
				`DELETE FROM stations WHERE idx=?`,
				`DELETE FROM deletions WHERE idx=?`,
			} {
				if _, err := tx.ExecContext(ctx, q, j.idx); err != nil {
					return err
				}
			}
			fid, fps := "", ""
			if j.feederID != "" {
				res, err := tx.ExecContext(ctx, `DELETE FROM feeders WHERE feeder_id=? AND NOT EXISTS
 (SELECT 1 FROM stations WHERE feeder_id=?)`, j.feederID, j.feederID)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					fid, fps = j.feederID, r.pseudonym("feeder", j.feederID)
				}
			}
			// One pass over the audit log: the SID column and any mention of
			// the SID (or of a feeder key that no longer exists) in details.
			if _, err := tx.ExecContext(ctx, `UPDATE audit SET sid=CASE WHEN sid=?1 THEN ?2 ELSE sid END,
 detail=REPLACE(REPLACE(detail, ?1, ?2), ?3, ?4)
 WHERE sid=?1 OR instr(detail, ?1)>0 OR (?3<>'' AND instr(detail, ?3)>0)`, j.sid, ps, fid, fps); err != nil {
				return err
			}
			return insertAudit(ctx, tx, r.now(), SystemActor, "station.deleted", ps, "")
		}, func() { r.cache.remove(j.idx) })
		if err != nil {
			return done, fmt.Errorf("registry: delete station: %w", err)
		}
		r.seenMu.Lock()
		delete(r.seen, j.idx)
		r.seenMu.Unlock()
		done++
	}
	if done > 0 {
		r.log.Info("registry: stations deleted", "n", done)
	}
	return done, nil
}

// ---------------------------------------------------------------- retention

// RetentionRule removes old rows of a statistics table in stations.db
// (owned by internal/store) by its time column. The rule applies only when
// the table and column exist.
type RetentionRule struct {
	Table  string
	Column string
	// Days is true when the column holds Unix day numbers instead of Unix
	// seconds.
	Days bool
	// Keep returns the retention period from the policy.
	Keep func(model.AggRetention) time.Duration
}

func minuteKeep(r model.AggRetention) time.Duration {
	return time.Duration(r.MinuteDays) * 24 * time.Hour
}

func hourKeep(r model.AggRetention) time.Duration {
	return time.Duration(r.HourMonths) * 30 * 24 * time.Hour // ~month; rounds down by < 1 week per year
}

// DefaultRetentionRules are the statistics tables of DESIGN §6.6 and their
// tiers (§8.5): minute data for MinuteDays, hourly and daily data for
// HourMonths. heat has no time dimension and is bounded by its cells.
var DefaultRetentionRules = []RetentionRule{
	{Table: "stats_minute", Column: "t", Keep: minuteKeep},
	{Table: "station_minute", Column: "t", Keep: minuteKeep},
	{Table: "station_clock_minute", Column: "t", Keep: minuteKeep},
	{Table: "latency_minute", Column: "t", Keep: minuteKeep},
	{Table: "receiver_status", Column: "t", Keep: minuteKeep},
	{Table: "stats_hour", Column: "t", Keep: hourKeep},
	{Table: "range_hour", Column: "t", Keep: hourKeep},
	{Table: "coverage", Column: "day", Days: true, Keep: hourKeep},
	{Table: "signal_hist", Column: "day", Days: true, Keep: hourKeep},
	{Table: "elev_hist", Column: "day", Days: true, Keep: hourKeep},
}

// RetentionReport counts what one RunRetention removed.
type RetentionReport struct {
	ConnLog, Audit, Claims, Tokens, Sessions, Estimates, Salts int64
	Stats                                                      map[string]int64
	Purged                                                     int // stations queued by the offline purge
	Deleted                                                    int // stations whose deletion completed
	Took                                                       time.Duration
}

// RunRetention applies the retention schedule of the current policy
// (DESIGN §8.5):
//
//   - conn_log: closed sessions older than ConnLogDays;
//   - audit: entries older than AuditMonths;
//   - minute / hourly statistics in stations.db per DefaultRetentionRules;
//   - location estimates: latest EstimatesPerMethod per station and method;
//   - unclaimed stations offline for OfflinePurgeDays (never seen: since
//     creation) are deleted; the host station is exempt;
//   - expired claim codes, used ones after 30 days, enrol tokens 90 days
//     after expiry, expired sessions, old conn_log salts.
//
// Queued deletions are processed at the end.
func (r *Registry) RunRetention(ctx context.Context) (RetentionReport, error) {
	start := time.Now()
	rep := RetentionReport{Stats: map[string]int64{}}
	pol := r.Policy().Retention
	now := r.now()
	_ = r.flushSeen(ctx)

	exec := func(dst *int64, q string, args ...any) error {
		return r.write(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, q, args...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			*dst += n
			return nil
		}, nil)
	}
	connCut := now.AddDate(0, 0, -pol.ConnLogDays).Unix()
	n, err := r.deleteBatched(ctx, tableInfo{name: "conn_log", quoted: "conn_log", rowid: true}, `t_off <> 0 AND t_off < ?`, connCut)
	rep.ConnLog = n
	if err != nil {
		return rep, err
	}
	auditCut := now.AddDate(0, -pol.AuditMonths, 0).Unix()
	if rep.Audit, err = r.deleteBatched(ctx, tableInfo{name: "audit", quoted: "audit", rowid: true}, `t < ?`, auditCut); err != nil {
		return rep, err
	}
	if err := exec(&rep.Claims, `DELETE FROM claims WHERE (used = 0 AND expires <= ?) OR (used <> 0 AND used < ?)`,
		now.Unix(), now.AddDate(0, 0, -30).Unix()); err != nil {
		return rep, err
	}
	if err := exec(&rep.Tokens, `DELETE FROM enrol_tokens WHERE expires < ?`, now.AddDate(0, 0, -90).Unix()); err != nil {
		return rep, err
	}
	if s, err := r.Sessions().Purge(ctx, now); err != nil {
		return rep, err
	} else {
		rep.Sessions = int64(s)
	}
	if err := exec(&rep.Estimates, `DELETE FROM location_estimates WHERE id IN (
 SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY idx, method ORDER BY t DESC, id DESC) AS rn FROM location_estimates)
 WHERE rn > ?)`, pol.EstimatesPerMethod); err != nil {
		return rep, err
	}
	if err := exec(&rep.Salts, `DELETE FROM conn_salt WHERE day < ?`, now.UTC().Unix()/86400); err != nil {
		return rep, err
	}

	// Statistics tables kept by internal/store in the same file.
	tables, err := r.foreignTables(ctx)
	if err != nil {
		return rep, err
	}
	byName := map[string]tableInfo{}
	for _, t := range tables {
		byName[t.name] = t
	}
	rules := DefaultRetentionRules
	if r.opts.StoreMaintained {
		rules = nil
	}
	for _, rule := range rules {
		t, ok := byName[rule.Table]
		if !ok || !t.cols[rule.Column] {
			continue
		}
		cut := now.Add(-rule.Keep(pol)).Unix()
		if rule.Days {
			cut /= 86400
		}
		col := `"` + rule.Column + `"`
		n, err := r.deleteBatched(ctx, t, col+` < ?`, cut)
		if err != nil {
			return rep, err
		}
		if n > 0 {
			rep.Stats[rule.Table] = n
		}
	}

	// Offline purge of unclaimed stations.
	purgeCut := now.AddDate(0, 0, -pol.OfflinePurgeDays)
	var stale []model.StationIdx
	r.cache.each(func(st *Station) {
		if st.Deleting || st.OwnerID != 0 || st.Idx == model.HostStation || st.Kind == model.StationKindHost {
			return
		}
		last := st.LastSeen
		if last.IsZero() || last.Before(st.Created) {
			last = st.Created
		}
		if last.Before(purgeCut) {
			stale = append(stale, st.Idx)
		}
	})
	for _, idx := range stale {
		if err := r.DeleteStation(ctx, SystemActor, idx, "offline purge"); err != nil && !errors.Is(err, ErrStationNotFound) {
			return rep, err
		}
		rep.Purged++
	}
	if rep.Deleted, err = r.ProcessDeletions(ctx); err != nil {
		return rep, err
	}
	// Return freed pages to the file system in bounded steps (effective
	// when the file was created with auto_vacuum=INCREMENTAL, as both this
	// package and internal/store create it; a no-op otherwise).
	_ = r.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `PRAGMA incremental_vacuum(4096)`)
		return err
	}, nil)
	if _, err := r.wdb.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		r.log.Debug("registry: PRAGMA optimize", "err", err)
	}
	rep.Took = time.Since(start)
	r.log.Info("registry: retention", "connLog", rep.ConnLog, "audit", rep.Audit, "claims", rep.Claims,
		"tokens", rep.Tokens, "sessions", rep.Sessions, "estimates", rep.Estimates, "stats", rep.Stats,
		"purged", rep.Purged, "deleted", rep.Deleted, "took", rep.Took)
	return rep, nil
}

// Run performs the background jobs until ctx ends: last-seen flushes every
// SeenFlushEvery, queued deletions every DeletionEvery, and the retention
// schedule once at start and then every RetentionEvery. Errors are logged
// and retried on the next tick.
func (r *Registry) Run(ctx context.Context) {
	seen := time.NewTicker(r.opts.SeenFlushEvery)
	del := time.NewTicker(r.opts.DeletionEvery)
	ret := time.NewTicker(r.opts.RetentionEvery)
	defer seen.Stop()
	defer del.Stop()
	defer ret.Stop()
	if _, err := r.RunRetention(ctx); err != nil && ctx.Err() == nil {
		r.log.Warn("registry: retention failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-seen.C:
			if err := r.flushSeen(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("registry: last-seen flush failed", "err", err)
			}
		case <-del.C:
			if _, err := r.ProcessDeletions(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("registry: station deletion failed", "err", err)
			}
		case <-ret.C:
			if _, err := r.RunRetention(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("registry: retention failed", "err", err)
			}
			if _, err := r.ReleasePending(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("registry: pending positions", "err", err)
			}
			if err := r.pruneBandRefs(ctx, TagPeriod(r.now())); err != nil && ctx.Err() == nil {
				r.log.Warn("registry: band references", "err", err)
			}
		}
	}
}
