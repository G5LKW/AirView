package rfstat

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"airview/internal/store"
)

const ddlV1 = `
CREATE TABLE IF NOT EXISTS rfstat_hour(
  t INTEGER PRIMARY KEY,   -- hour start, Unix seconds
  frames INTEGER NOT NULL,
  fps REAL, occupancy_pct REAL, crc_bad REAL, pd_pos REAL, range90_km REAL,
  health INTEGER,
  json TEXT NOT NULL       -- HourSummary
);
CREATE TABLE IF NOT EXISTS rfstat_iid_hour(
  t INTEGER NOT NULL, iid INTEGER NOT NULL, replies INTEGER NOT NULL,
  PRIMARY KEY(t, iid)
) WITHOUT ROWID;
`

func init() {
	// Errors are logged by the store; registration of an identical DDL is a no-op.
	_ = store.RegisterMigration("rfstat", 1, ddlV1)
}

// Writer is the write side of *store.Store used for hourly summaries.
type Writer interface {
	Write(ctx context.Context, fn func(tx *sql.Tx) error) error
}

// Run ticks the engine once per second and writes every closed hour to w
// (nil = memory only) until ctx is cancelled. Queued summaries are flushed
// on the way out.
func (s *Stats) Run(ctx context.Context, w Writer) error {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case h := <-s.pending:
					fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					s.write(fctx, w, h)
					cancel()
				default:
					return ctx.Err()
				}
			}
		case now := <-tk.C:
			s.Tick(now)
		case h := <-s.pending:
			s.write(ctx, w, h)
		}
	}
}

func nullF(n Num) sql.NullFloat64 {
	return sql.NullFloat64{Float64: float64(n), Valid: n.Valid()}
}

func (s *Stats) write(ctx context.Context, w Writer, h HourSummary) {
	if w == nil {
		return
	}
	js, err := json.Marshal(h)
	if err != nil {
		s.log.Error("rfstat: marshal hour summary", "err", err)
		return
	}
	t := h.T / 1000
	fn := func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO rfstat_hour(t,frames,fps,occupancy_pct,crc_bad,pd_pos,range90_km,health,json)
			VALUES(?,?,?,?,?,?,?,?,?)`, t, h.Frames, nullF(h.FPS), nullF(h.OccupancyPct), nullF(h.BadFrac),
			nullF(h.PdPos), nullF(h.Range90Km), h.Health, string(js)); err != nil {
			return err
		}
		for code, n := range h.DF11ByIID {
			iid := parseIIDLabel(code)
			if iid < 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO rfstat_iid_hour(t,iid,replies) VALUES(?,?,?)`, t, iid, n); err != nil {
				return err
			}
		}
		return nil
	}
	// A store that offers it takes the rows into its next group commit
	// (internal/store Store.QueueTx) instead of a commit of their own.
	if q, ok := w.(queueWriter); ok {
		q.QueueTx("rfstat_hour", 1+len(h.DF11ByIID), fn)
		return
	}
	if err := w.Write(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		s.log.Error("rfstat: write hour summary", "hour", t, "err", err)
	}
}

// queueWriter is the optional queued form of Writer.
type queueWriter interface {
	QueueTx(name string, rows int, fn func(ctx context.Context, tx *sql.Tx) error)
}

func parseIIDLabel(l string) int {
	for i := 0; i < nIID; i++ {
		if iidLabel(i) == l {
			return i
		}
	}
	return -1
}

// LoadHistory reads persisted hourly summaries with from <= hour < to,
// oldest first (db = store.DB()).
func LoadHistory(ctx context.Context, db *sql.DB, from, to time.Time) ([]HourSummary, error) {
	rows, err := db.QueryContext(ctx, `SELECT json FROM rfstat_hour WHERE t >= ? AND t < ? ORDER BY t`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourSummary
	for rows.Next() {
		var js string
		if err := rows.Scan(&js); err != nil {
			return nil, err
		}
		var h HourSummary
		if err := json.Unmarshal([]byte(js), &h); err != nil {
			continue
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
