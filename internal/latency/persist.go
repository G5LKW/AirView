package latency

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/store"
)

// MigrationDDL creates the latency_minute table (registered with the store
// as owner "latency", version 1).
const MigrationDDL = `CREATE TABLE IF NOT EXISTS latency_minute(
	t INTEGER NOT NULL,
	feed TEXT NOT NULL,
	n INTEGER NOT NULL,
	min_us INTEGER, p50_us INTEGER, p95_us INTEGER, max_us INTEGER, mean_us INTEGER,
	jitter_us INTEGER, neg INTEGER, absurd INTEGER,
	prop_p50_us INTEGER, proc_p50_us INTEGER, t1_n INTEGER, t1_p50_us INTEGER,
	PRIMARY KEY(t, feed)
) WITHOUT ROWID;`

func init() {
	// Errors are logged by the store; a conflicting re-registration can only
	// happen if this DDL is edited, which must never be done (add version 2).
	_ = store.RegisterMigration("latency", 1, MigrationDDL)
}

// Persist queues minute rows (from DrainMinutes) and the deletion of rows
// older than retentionDays (0 = keep) for the store's next group commit:
// once a minute, a commit of its own (plus the forced flush of the queue
// before it) was the most frequent small commit of a station. The rows are
// in the database within the store's flush interval (Store.Flush waits for
// them); the analyzer serves the recent minutes from memory meanwhile.
// Call it about once a minute, never per frame. The error is ctx's.
func Persist(ctx context.Context, st *store.Store, rows []MinuteSummary, retentionDays int) error {
	if st == nil || (len(rows) == 0 && retentionDays <= 0) {
		return nil
	}
	rows = slices.Clone(rows)
	var cut int64
	if retentionDays > 0 {
		cut = time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	}
	st.QueueTx("latency_minute", max(len(rows), 1), func(ctx context.Context, tx *sql.Tx) error {
		if len(rows) > 0 {
			stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO latency_minute
				(t, feed, n, min_us, p50_us, p95_us, max_us, mean_us, jitter_us, neg, absurd, prop_p50_us, proc_p50_us, t1_n, t1_p50_us)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, r := range rows {
				if _, err := stmt.ExecContext(ctx, r.T, r.Feed, r.N, r.MinUs, r.P50Us, r.P95Us, r.MaxUs, r.MeanUs,
					r.JitterUs, r.Neg, r.Absurd, r.PropP50Us, r.ProcP50Us, r.T1N, r.T1P50Us); err != nil {
					return err
				}
			}
		}
		if cut > 0 {
			// A key-range probe: usually one minute of rows or nothing to
			// delete, and no page is written when nothing matches.
			if _, err := tx.ExecContext(ctx, `DELETE FROM latency_minute WHERE t < ?`, cut); err != nil {
				return err
			}
		}
		return nil
	})
	return ctx.Err()
}

// LoadMinutes reads persisted rows at or after fromUnix (seconds), oldest
// first, at most limit rows (<= 0: 100000). Use with Seed at start-up or for
// history charts.
func LoadMinutes(ctx context.Context, db *sql.DB, fromUnix int64, feed *string, limit int) ([]MinuteSummary, error) {
	if limit <= 0 {
		limit = 100000
	}
	q := `SELECT t, feed, n, min_us, p50_us, p95_us, max_us, mean_us, jitter_us, neg, absurd, prop_p50_us, proc_p50_us, t1_n, t1_p50_us
		FROM latency_minute WHERE t >= ?`
	args := []any{fromUnix}
	if feed != nil {
		q += ` AND feed = ?`
		args = append(args, *feed)
	}
	q += ` ORDER BY t, feed LIMIT ?`
	args = append(args, limit)
	rs, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []MinuteSummary
	for rs.Next() {
		var r MinuteSummary
		var mn, p50, p95, mx, mean, jit, neg, abs, pp, pr, t1n, t1p sql.NullInt64
		if err := rs.Scan(&r.T, &r.Feed, &r.N, &mn, &p50, &p95, &mx, &mean, &jit, &neg, &abs, &pp, &pr, &t1n, &t1p); err != nil {
			return nil, err
		}
		r.MinUs, r.P50Us, r.P95Us, r.MaxUs, r.MeanUs = mn.Int64, p50.Int64, p95.Int64, mx.Int64, mean.Int64
		r.JitterUs, r.Neg, r.Absurd, r.PropP50Us, r.ProcP50Us = jit.Int64, neg.Int64, abs.Int64, pp.Int64, pr.Int64
		r.T1N, r.T1P50Us = t1n.Int64, t1p.Int64
		out = append(out, r)
	}
	return out, rs.Err()
}

// SetFrame copies the receiver-side fields of a raw frame into o.
func (o *Obs) SetFrame(f *model.RawFrame) {
	o.RecvHost = f.Recv
	o.GPSNs = f.GPSNs
	o.FeedID = f.FeedID
	o.MLAT = f.MLAT
	o.Kind = f.Kind
}

// SetMessage copies the decoded Mode S fields of m into o: DF, address,
// type code and - for ADS-B positions - the T and F bits. Position, altitude
// and velocity (decoded by the tracker) are left to the caller.
func (o *Obs) SetMessage(m *modes.Message) {
	o.DF = m.DF
	o.ICAO = m.AddrKey()
	o.TC = m.TC
	o.IsPosition = (m.DF == 17 || m.DF == 18) && m.Has(modes.FCPR)
	o.TFlag = o.IsPosition && m.CPRTime
	o.CPROdd = o.IsPosition && m.CPROdd
}

// TFlagFromME reads the T bit (ME bit 21) of an ADS-B position ME field
// (56-bit, as modes.Message.ME()) for type codes 5-18 and 20-22; it is the
// same bit modes.Message.CPRTime exposes (TIS-B/ADS-R use it as IMF).
func TFlagFromME(me uint64) bool {
	tc := uint8(me >> 51)
	if (tc >= 5 && tc <= 18) || (tc >= 20 && tc <= 22) {
		return me>>(56-21)&1 == 1
	}
	return false
}
