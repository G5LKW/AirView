package registry

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"airview/internal/model"
)

// metaServerID is the registry_meta key of the AVF server ID. The value is
// stored with the prefix metaIDPrefix: the column has INTEGER affinity, and
// an all-digit ID must not be converted to a number.
const (
	metaServerID = "avf_server_id"
	metaIDPrefix = "id:"
)

// ServerID returns the aggregator's AVF/1 network identity (AVF.md §3.2:
// "generated randomly once and kept in stations.db"). On the first call it
// adopts legacy (the value an older version kept in <data>/avf-tls/server-id)
// when that is a valid ID, so feeders do not see a changed network
// identity; otherwise it generates a new one. The value never changes
// afterwards.
func (r *Registry) ServerID(ctx context.Context, legacy string) (string, error) {
	var id string
	err := r.rdb.QueryRowContext(ctx, `SELECT value FROM registry_meta WHERE key=?`, metaServerID).Scan(&id)
	if err == nil {
		if c, ok := model.CanonicalID(strings.TrimPrefix(id, metaIDPrefix)); ok {
			return c, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id, ok := model.CanonicalID(legacy)
	if !ok {
		if id, err = model.NewSID(r.rnd); err != nil {
			return "", err
		}
	}
	var stored string
	err = r.write(ctx, func(tx *sql.Tx) error {
		// INSERT OR IGNORE + read back: a concurrent first call keeps one ID.
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO registry_meta(key, value) VALUES(?, ?)`, metaServerID, metaIDPrefix+id); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT value FROM registry_meta WHERE key=?`, metaServerID).Scan(&stored)
	}, nil)
	if err != nil {
		return "", err
	}
	if c, ok := model.CanonicalID(strings.TrimPrefix(stored, metaIDPrefix)); ok {
		return c, nil
	}
	return "", errors.New("registry: stored AVF server ID is invalid")
}
