package registry

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"

	"airview/internal/model"
)

// Connection log (DESIGN §6.6, §8.4): one row per link session with the
// station, on/off times, protocol, byte count and HMAC(daySalt, ip). The
// salt is random per UTC day; older salts are deleted, so an IP hash can
// neither be linked across days nor recomputed later. Raw IPs never reach
// the database.

// ConnEntry is one conn_log row.
type ConnEntry struct {
	ID     int64
	Idx    model.StationIdx
	On     time.Time
	Off    time.Time // zero while the link is open
	IPHash string    // 16 hex digits, salted per day
	Proto  string    // "avf", "beast", "mlatc", "host"
	Bytes  int64
}

// daySalt returns the salt of the UTC day of now, creating it if needed,
// and deletes older salts.
func (r *Registry) daySalt(ctx context.Context, now time.Time) ([]byte, error) {
	day := now.UTC().Unix() / 86400
	r.saltMu.Lock()
	defer r.saltMu.Unlock()
	if r.saltDay == day && r.salt != nil {
		return r.salt, nil
	}
	salt := make([]byte, 32)
	if _, err := io.ReadFull(r.rnd, salt); err != nil {
		return nil, err
	}
	err := r.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM conn_salt WHERE day<>?`, day); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO conn_salt(day, salt) VALUES(?,?)`, day, salt); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT salt FROM conn_salt WHERE day=?`, day).Scan(&salt)
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: conn salt: %w", err)
	}
	r.saltDay, r.salt = day, salt
	return salt, nil
}

// IPHash returns the day-salted hash conn_log stores for ip at now. The
// Feed Check page can use it to compare a visitor with today's links
// without keeping raw IPs anywhere but memory.
func (r *Registry) IPHash(ctx context.Context, ip netip.Addr, now time.Time) (string, error) {
	salt, err := r.daySalt(ctx, now)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(mac(salt, ip.Unmap().String())[:8]), nil
}

// ConnOpen records the start of a link session and returns its log ID.
func (r *Registry) ConnOpen(ctx context.Context, idx model.StationIdx, ip netip.Addr, proto string) (int64, error) {
	now := r.now()
	h, err := r.IPHash(ctx, ip, now)
	if err != nil {
		return 0, err
	}
	var id int64
	err = r.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conn_log(idx, t_on, ip_hmac, proto) VALUES(?,?,?,?)`, idx, now.Unix(), h, proto)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}, nil)
	return id, err
}

// ConnClose records the end of a link session.
func (r *Registry) ConnClose(ctx context.Context, id int64, bytes int64) error {
	return r.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE conn_log SET t_off=MAX(?, t_on), bytes=? WHERE id=? AND t_off=0`, r.now().Unix(), bytes, id)
		return err
	}, nil)
}

// closeDangling ends sessions left open by a crash or kill: their end is
// the station's last-seen time (or the start, if that is later).
func (r *Registry) closeDangling(ctx context.Context) error {
	return r.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE conn_log SET t_off=MAX(t_on, COALESCE((SELECT last_seen FROM stations WHERE stations.idx=conn_log.idx), 0))
WHERE t_off=0`)
		return err
	}, nil)
}

// ConnLog returns the connection log of a station since a time, newest
// first (at most limit rows; 0 = 500).
func (r *Registry) ConnLog(ctx context.Context, idx model.StationIdx, since time.Time, limit int) ([]ConnEntry, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.rdb.QueryContext(ctx, `SELECT id, idx, t_on, t_off, ip_hmac, proto, bytes FROM conn_log
WHERE idx=? AND t_on>=? ORDER BY t_on DESC, id DESC LIMIT ?`, idx, unixS(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnEntry
	for rows.Next() {
		var e ConnEntry
		var on, off int64
		if err := rows.Scan(&e.ID, &e.Idx, &on, &off, &e.IPHash, &e.Proto, &e.Bytes); err != nil {
			return nil, err
		}
		e.On, e.Off = fromS(on), fromS(off)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
