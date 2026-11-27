package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"airview/internal/auth"
	"airview/internal/model"
)

// Accounts returns the auth.AccountStore backed by stations.db (DESIGN §7:
// owners and admins; no e-mail). Deleting an account also releases its
// stations (they become unclaimed) and pseudonymises its audit entries;
// DeleteOwner additionally deletes the stations.
func (r *Registry) Accounts() auth.AccountStore { return accountStore{r} }

type accountStore struct{ r *Registry }

var _ auth.AccountStore = accountStore{}

const accountCols = `id, username, role, password_hash, totp_secret, totp_pending, totp_last_step, recovery,
 cred_epoch, disabled, created_ns, updated_ns, last_login_ns`

func scanAccount(sc scanner) (auth.Account, error) {
	var a auth.Account
	var role, disabled int
	var rec string
	var c, u, l int64
	if err := sc.Scan(&a.ID, &a.Username, &role, &a.PasswordHash, &a.TOTPSecret, &a.TOTPPending, &a.TOTPLastStep,
		&rec, &a.CredEpoch, &disabled, &c, &u, &l); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.Account{}, auth.ErrAccountNotFound
		}
		return auth.Account{}, err
	}
	a.Role, a.Disabled = auth.Role(role), disabled != 0
	a.Created, a.Updated, a.LastLogin = fromNs(c), fromNs(u), fromNs(l)
	if rec != "" && rec != "[]" {
		if err := json.Unmarshal([]byte(rec), &a.RecoveryHashes); err != nil {
			return auth.Account{}, fmt.Errorf("registry: account %d recovery codes: %w", a.ID, err)
		}
	}
	return a, nil
}

func recoveryJSON(h []string) string {
	if len(h) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(h)
	return string(b)
}

func isUnique(err error, col string) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE") && strings.Contains(err.Error(), col)
}

// Create implements auth.AccountStore.
func (s accountStore) Create(ctx context.Context, a auth.Account) (auth.Account, error) {
	err := s.r.write(ctx, func(tx *sql.Tx) error {
		var idArg any
		if a.ID != 0 {
			idArg = a.ID
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO accounts(id, username, role, password_hash, totp_secret, totp_pending,
 totp_last_step, recovery, cred_epoch, disabled, created_ns, updated_ns, last_login_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			idArg, a.Username, int(a.Role), a.PasswordHash, a.TOTPSecret, a.TOTPPending, a.TOTPLastStep,
			recoveryJSON(a.RecoveryHashes), a.CredEpoch, b2i(a.Disabled), unixNs(a.Created), unixNs(a.Updated), unixNs(a.LastLogin))
		switch {
		case isUnique(err, "accounts.username"):
			return auth.ErrUsernameTaken
		case isUnique(err, "accounts.id") || (err != nil && strings.Contains(err.Error(), "PRIMARY KEY")):
			return errors.New("account id already exists")
		case err != nil:
			return fmt.Errorf("registry: create account: %w", err)
		}
		a.ID, err = res.LastInsertId()
		return err
	}, nil)
	if err != nil {
		return auth.Account{}, err
	}
	return a.Clone(), nil
}

// ByID implements auth.AccountStore.
func (s accountStore) ByID(ctx context.Context, id int64) (auth.Account, error) {
	return scanAccount(s.r.rdb.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id=?`, id))
}

// ByUsername implements auth.AccountStore.
func (s accountStore) ByUsername(ctx context.Context, name string) (auth.Account, error) {
	return scanAccount(s.r.rdb.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE username=?`, name))
}

// Update implements auth.AccountStore.
func (s accountStore) Update(ctx context.Context, a auth.Account) error {
	return s.r.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE accounts SET username=?, role=?, password_hash=?, totp_secret=?, totp_pending=?,
 totp_last_step=?, recovery=?, cred_epoch=?, disabled=?, created_ns=?, updated_ns=?, last_login_ns=? WHERE id=?`,
			a.Username, int(a.Role), a.PasswordHash, a.TOTPSecret, a.TOTPPending, a.TOTPLastStep, recoveryJSON(a.RecoveryHashes),
			a.CredEpoch, b2i(a.Disabled), unixNs(a.Created), unixNs(a.Updated), unixNs(a.LastLogin), a.ID)
		if isUnique(err, "accounts.username") {
			return auth.ErrUsernameTaken
		}
		if err != nil {
			return fmt.Errorf("registry: update account: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return auth.ErrAccountNotFound
		}
		return nil
	}, nil)
}

// Delete implements auth.AccountStore. It also removes the account's
// sessions, releases its stations and pseudonymises its audit entries.
func (s accountStore) Delete(ctx context.Context, id int64) error {
	r := s.r
	var released []*Station
	return r.write(ctx, func(tx *sql.Tx) error {
		released = nil
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id=?`, id).Scan(&n); err != nil || n == 0 {
			return err
		}
		for _, q := range []string{
			`DELETE FROM accounts WHERE id=?`,
			`DELETE FROM sessions WHERE account_id=?`,
			`UPDATE claims SET used_by=0 WHERE used_by=?`,
			`UPDATE stations SET owner_id=0 WHERE owner_id=?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return fmt.Errorf("registry: delete account: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE audit SET actor_id=0, actor=? WHERE actor_id=?`,
			r.pseudonym("account", fmt.Sprint(id)), id); err != nil {
			return err
		}
		r.cache.each(func(st *Station) {
			if st.OwnerID == id {
				n := *st
				n.OwnerID = 0
				released = append(released, &n)
			}
		})
		return insertAudit(ctx, tx, r.now(), SystemActor, "account.delete", "",
			fmt.Sprintf(`{"account":%q,"stationsReleased":%d}`, r.pseudonym("account", fmt.Sprint(id)), len(released)))
	}, func() {
		for _, st := range released {
			r.cache.put(st)
		}
	})
}

// ListRole implements auth.AccountStore.
func (s accountStore) ListRole(ctx context.Context, role auth.Role) ([]auth.Account, error) {
	rows, err := s.r.rdb.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE role=? ORDER BY id`, int(role))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- sessions

// Sessions returns the auth.SessionStore backed by stations.db, so logins
// survive restarts. Only Session.ID (SHA-256 of the token) is stored, and
// no IP (DESIGN §8.5). At most Options.MaxSessionsPerAccount sessions are
// kept per account; the one that expires first is evicted.
func (r *Registry) Sessions() auth.SessionStore { return sessionStore{r} }

type sessionStore struct{ r *Registry }

var _ auth.SessionStore = sessionStore{}

const sessionCols = `id, account_id, scope, username, cred_epoch, csrf, created_ns, last_seen_ns, expires_ns, idle_ns, mfa`

func scanSession(sc scanner) (auth.Session, error) {
	var s auth.Session
	var scope, mfa int
	var c, l, e, idle int64
	if err := sc.Scan(&s.ID, &s.AccountID, &scope, &s.Username, &s.CredEpoch, &s.CSRF, &c, &l, &e, &idle, &mfa); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.Session{}, auth.ErrSessionNotFound
		}
		return auth.Session{}, err
	}
	s.Scope, s.MFA = model.Scope(scope), mfa != 0
	s.Created, s.LastSeen, s.Expires, s.Idle = fromNs(c), fromNs(l), fromNs(e), time.Duration(idle)
	return s, nil
}

// Put implements auth.SessionStore.
func (s sessionStore) Put(ctx context.Context, x auth.Session) error {
	if x.ID == "" {
		return errors.New("registry: session without ID")
	}
	r := s.r
	return r.write(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id=?`, x.ID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			// Evict the first-expiring sessions of the account beyond the cap.
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id IN (
 SELECT id FROM sessions WHERE account_id=? ORDER BY expires_ns DESC LIMIT -1 OFFSET ?)`,
				x.AccountID, r.opts.MaxSessionsPerAccount-1); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions(`+sessionCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET account_id=excluded.account_id, scope=excluded.scope, username=excluded.username,
 cred_epoch=excluded.cred_epoch, csrf=excluded.csrf, created_ns=excluded.created_ns, last_seen_ns=excluded.last_seen_ns,
 expires_ns=excluded.expires_ns, idle_ns=excluded.idle_ns, mfa=excluded.mfa`,
			x.ID, x.AccountID, int(x.Scope), x.Username, x.CredEpoch, x.CSRF, unixNs(x.Created), unixNs(x.LastSeen),
			unixNs(x.Expires), int64(x.Idle), b2i(x.MFA))
		return err
	}, nil)
}

// Get implements auth.SessionStore.
func (s sessionStore) Get(ctx context.Context, id string) (auth.Session, error) {
	return scanSession(s.r.rdb.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id=?`, id))
}

// Touch implements auth.SessionStore (auth.Service already coalesces calls
// to at most one per Policy.TouchEvery per session).
func (s sessionStore) Touch(ctx context.Context, id string, at time.Time) error {
	return s.r.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_ns=? WHERE id=? AND last_seen_ns<?`, unixNs(at), id, unixNs(at))
		return err
	}, nil)
}

// Delete implements auth.SessionStore.
func (s sessionStore) Delete(ctx context.Context, id string) error {
	return s.r.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
		return err
	}, nil)
}

// DeleteAccount implements auth.SessionStore.
func (s sessionStore) DeleteAccount(ctx context.Context, accountID int64, keepID string) (int, error) {
	var n int64
	err := s.r.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE account_id=? AND id<>?`, accountID, keepID)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	}, nil)
	return int(n), err
}

// DeleteAll implements auth.SessionStore.
func (s sessionStore) DeleteAll(ctx context.Context) error {
	return s.r.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions`)
		return err
	}, nil)
}

// List implements auth.SessionStore.
func (s sessionStore) List(ctx context.Context, accountID int64) ([]auth.Session, error) {
	rows, err := s.r.rdb.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE account_id=? ORDER BY created_ns DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Count implements auth.SessionStore.
func (s sessionStore) Count(ctx context.Context) (int, error) {
	var n int
	err := s.r.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n, err
}

// Purge implements auth.SessionStore: it removes sessions that are no
// longer auth.Session.Valid at now (expired, or idle longer than Idle).
func (s sessionStore) Purge(ctx context.Context, now time.Time) (int, error) {
	var n int64
	ns := now.UnixNano()
	err := s.r.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_ns<=? OR (idle_ns>0 AND ?-last_seen_ns>idle_ns)`, ns, ns)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	}, nil)
	return int(n), err
}
