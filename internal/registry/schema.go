package registry

import (
	"context"
	"database/sql"
	"fmt"
)

// Times: registry tables use Unix seconds (UTC), like internal/store;
// accounts and sessions use Unix nanoseconds so that auth.Account and
// auth.Session values round-trip exactly. 0 means "never / unset".

// migrations are applied in order; each runs in one transaction and the
// version is recorded in registry_meta. Never edit an applied migration;
// append a new one.
var migrations = []string{
	// 1: initial schema.
	`
CREATE TABLE IF NOT EXISTS registry_keys(
  name    TEXT PRIMARY KEY,
  key     BLOB NOT NULL,
  created INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS feeders(
  feeder_id  TEXT PRIMARY KEY,
  pubkey     BLOB NOT NULL,
  uuid       TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL DEFAULT 0,
  token_id   TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS stations(
  idx          INTEGER PRIMARY KEY AUTOINCREMENT,
  sid          TEXT NOT NULL UNIQUE,
  feeder_id    TEXT NOT NULL DEFAULT '',
  rx_no        INTEGER NOT NULL DEFAULT 0,
  kind         TEXT NOT NULL,
  uuid         TEXT NOT NULL DEFAULT '',
  ident_key    TEXT NOT NULL DEFAULT '',
  name         TEXT NOT NULL DEFAULT '',
  clock_class  INTEGER NOT NULL DEFAULT 0,
  decl_lat     REAL, decl_lon REAL, decl_alt REAL,
  decl_alt_ref TEXT NOT NULL DEFAULT '',
  gnss_lat     REAL, gnss_lon REAL, gnss_hae REAL,
  est_lat      REAL, est_lon REAL, est_hae REAL, est_sigma REAL,
  eff_lat      REAL, eff_lon REAL, eff_hae REAL, eff_sigma REAL,
  eff_source   TEXT NOT NULL DEFAULT '',
  loc_status   TEXT NOT NULL DEFAULT 'unknown',
  pin          TEXT NOT NULL DEFAULT 'hidden',
  show_name    INTEGER NOT NULL DEFAULT 0,
  public_stats INTEGER NOT NULL DEFAULT 0,
  owner_id     INTEGER NOT NULL DEFAULT 0,
  trust        INTEGER NOT NULL DEFAULT 0,
  admission    TEXT NOT NULL,
  anchor       INTEGER NOT NULL DEFAULT 0,
  flags        INTEGER NOT NULL DEFAULT 0,
  token_id     TEXT NOT NULL DEFAULT '',
  created      INTEGER NOT NULL,
  last_seen    INTEGER NOT NULL DEFAULT 0,
  deleting     INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS stations_feeder_rx ON stations(feeder_id, rx_no) WHERE feeder_id <> '';
CREATE UNIQUE INDEX IF NOT EXISTS stations_uuid ON stations(uuid) WHERE uuid <> '';
CREATE UNIQUE INDEX IF NOT EXISTS stations_ident ON stations(ident_key) WHERE ident_key <> '';
CREATE INDEX IF NOT EXISTS stations_owner ON stations(owner_id) WHERE owner_id <> 0;

CREATE TABLE IF NOT EXISTS station_public(
  idx        INTEGER PRIMARY KEY,
  precision  TEXT NOT NULL,
  effective  TEXT NOT NULL,
  cell_lat   INTEGER NOT NULL,
  cell_lon   INTEGER NOT NULL,
  pub_lat    REAL NOT NULL,
  pub_lon    REAL NOT NULL,
  computed   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS location_estimates(
  id      INTEGER PRIMARY KEY,
  idx     INTEGER NOT NULL,
  t       INTEGER NOT NULL,
  method  TEXT NOT NULL,
  lat     REAL NOT NULL,
  lon     REAL NOT NULL,
  hae     REAL NOT NULL,
  sigma_e REAL NOT NULL DEFAULT 0,
  sigma_n REAL NOT NULL DEFAULT 0,
  sigma_u REAL NOT NULL DEFAULT 0,
  n       INTEGER NOT NULL DEFAULT 0,
  status  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS location_estimates_idx ON location_estimates(idx, method, t);

CREATE TABLE IF NOT EXISTS accounts(
  id             INTEGER PRIMARY KEY,
  username       TEXT NOT NULL UNIQUE,
  role           INTEGER NOT NULL,
  password_hash  TEXT NOT NULL,
  totp_secret    TEXT NOT NULL DEFAULT '',
  totp_pending   TEXT NOT NULL DEFAULT '',
  totp_last_step INTEGER NOT NULL DEFAULT 0,
  recovery       TEXT NOT NULL DEFAULT '[]',
  cred_epoch     INTEGER NOT NULL DEFAULT 0,
  disabled       INTEGER NOT NULL DEFAULT 0,
  created_ns     INTEGER NOT NULL DEFAULT 0,
  updated_ns     INTEGER NOT NULL DEFAULT 0,
  last_login_ns  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS accounts_role ON accounts(role, id);

CREATE TABLE IF NOT EXISTS sessions(
  id           TEXT PRIMARY KEY,
  account_id   INTEGER NOT NULL,
  scope        INTEGER NOT NULL,
  username     TEXT NOT NULL,
  cred_epoch   INTEGER NOT NULL,
  csrf         TEXT NOT NULL,
  created_ns   INTEGER NOT NULL,
  last_seen_ns INTEGER NOT NULL,
  expires_ns   INTEGER NOT NULL,
  idle_ns      INTEGER NOT NULL,
  mfa          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS sessions_account ON sessions(account_id);
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_ns);

CREATE TABLE IF NOT EXISTS claims(
  code_hash  TEXT PRIMARY KEY,
  idx        INTEGER NOT NULL,
  created    INTEGER NOT NULL,
  expires    INTEGER NOT NULL,
  used       INTEGER NOT NULL DEFAULT 0,
  used_by    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS claims_idx ON claims(idx);

CREATE TABLE IF NOT EXISTS enrol_tokens(
  id         TEXT PRIMARY KEY,
  hash       TEXT NOT NULL UNIQUE,
  note       TEXT NOT NULL DEFAULT '',
  max_uses   INTEGER NOT NULL,
  uses       INTEGER NOT NULL DEFAULT 0,
  created    INTEGER NOT NULL,
  expires    INTEGER NOT NULL,
  revoked    INTEGER NOT NULL DEFAULT 0,
  created_by INTEGER NOT NULL DEFAULT 0,
  last_used  INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS conn_log(
  id      INTEGER PRIMARY KEY,
  idx     INTEGER NOT NULL,
  t_on    INTEGER NOT NULL,
  t_off   INTEGER NOT NULL DEFAULT 0,
  ip_hmac TEXT NOT NULL,
  proto   TEXT NOT NULL,
  bytes   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS conn_log_idx ON conn_log(idx, t_on);
CREATE INDEX IF NOT EXISTS conn_log_on ON conn_log(t_on);

CREATE TABLE IF NOT EXISTS conn_salt(
  day  INTEGER PRIMARY KEY,
  salt BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS audit(
  id       INTEGER PRIMARY KEY,
  t        INTEGER NOT NULL,
  actor_id INTEGER NOT NULL DEFAULT 0,
  actor    TEXT NOT NULL DEFAULT '',
  action   TEXT NOT NULL,
  sid      TEXT NOT NULL DEFAULT '',
  detail   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_t ON audit(t);
CREATE INDEX IF NOT EXISTS audit_sid ON audit(sid) WHERE sid <> '';
CREATE INDEX IF NOT EXISTS audit_actor ON audit(actor_id) WHERE actor_id <> 0;

CREATE TABLE IF NOT EXISTS deletions(
  idx       INTEGER PRIMARY KEY,
  sid       TEXT NOT NULL,
  feeder_id TEXT NOT NULL DEFAULT '',
  queued    INTEGER NOT NULL,
  reason    TEXT NOT NULL DEFAULT '',
  stage     INTEGER NOT NULL DEFAULT 0
);
`,
	// 2: owned_since - when the current owner was assigned (0 = before
	// this column existed, or unowned). Owner views and exports only show
	// station history from this time on (a re-claimed feeder must not hand
	// the previous owner's history to the next one).
	`ALTER TABLE stations ADD COLUMN owned_since INTEGER NOT NULL DEFAULT 0;`,
	// 3: feeder-announced positions (finding PRIV-1, position.go): the
	// rate limit and the position held pending a survey, per station; and
	// the frozen peer-band reference per viewer station and tag period, so
	// a restart does not refreeze it. Both are keyed by sid, so the station
	// deletion job removes their rows generically.
	`
CREATE TABLE IF NOT EXISTS station_feeder_pos(
  sid          TEXT PRIMARY KEY,
  last_change  INTEGER NOT NULL DEFAULT 0,
  applied_lat  REAL, applied_lon REAL, applied_alt REAL,
  applied_gnss INTEGER NOT NULL DEFAULT 0,
  pend_lat     REAL, pend_lon REAL, pend_alt REAL,
  pend_ref     TEXT NOT NULL DEFAULT '',
  pend_gnss    INTEGER NOT NULL DEFAULT 0,
  pend_sigma   REAL NOT NULL DEFAULT 0,
  pend_eff     INTEGER NOT NULL DEFAULT 0,
  pend_since   INTEGER NOT NULL DEFAULT 0,
  pend_last    INTEGER NOT NULL DEFAULT 0,
  pend_reason  TEXT NOT NULL DEFAULT '',
  held         INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS band_refs(
  sid    TEXT PRIMARY KEY,
  period TEXT NOT NULL,
  lat    REAL NOT NULL,
  lon    REAL NOT NULL
) WITHOUT ROWID;
`,
}

// registryTables are the tables this package owns. The station deletion
// job treats every other table of stations.db generically (by its ant / sid
// / feed column); these are handled explicitly.
var registryTables = map[string]bool{
	"registry_meta": true, "registry_keys": true, "feeders": true, "stations": true,
	"station_public": true, "location_estimates": true, "accounts": true, "sessions": true,
	"claims": true, "enrol_tokens": true, "conn_log": true, "conn_salt": true,
	"audit": true, "deletions": true, "sqlite_sequence": true,
}

// migrate brings the schema to the latest version.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS registry_meta(
  key   TEXT PRIMARY KEY,
  value INTEGER NOT NULL
) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("registry: create meta: %w", err)
	}
	var cur int
	err := db.QueryRowContext(ctx, `SELECT value FROM registry_meta WHERE key='schema'`).Scan(&cur)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("registry: read schema version: %w", err)
	}
	if cur > len(migrations) {
		return fmt.Errorf("registry: stations.db schema version %d is newer than this program (%d); upgrade AirView", cur, len(migrations))
	}
	for v := cur + 1; v <= len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v-1]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("registry: migration %d: %w", v, err)
		}
		if v == 1 {
			// Reserve idx 1 for the host station (model.HostStation): ordinary
			// stations start at 2 even when the host is registered later.
			if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence(name, seq)
SELECT 'stations', 1 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='stations')`); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("registry: reserve host idx: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO registry_meta(key, value) VALUES('schema', ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, v); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("registry: migration %d commit: %w", v, err)
		}
	}
	return nil
}
