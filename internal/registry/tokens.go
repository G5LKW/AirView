package registry

import (
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Enrol tokens (DESIGN §3): minted by the admin, a maximum use count
// (default 1), an expiry (default 7 days), revocable, stored hashed.
//
// A token is "avf-" followed by 32 lower-case base32 characters (160
// random bits). Only HMAC(tokenKey, token) is stored; the plain token is
// returned once by MintToken and never logged.

// TokenPrefix starts every enrol token.
const TokenPrefix = "avf-"

// ErrTokenInvalid: the enrol token is unknown, expired, revoked or used up.
var ErrTokenInvalid = errors.New("registry: enrol token is unknown, expired, revoked or used up")

var tokenEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// EnrolToken describes a minted token (never the secret itself).
type EnrolToken struct {
	ID        string // public handle for listing and revoking
	Note      string
	MaxUses   int
	Uses      int
	Created   time.Time
	Expires   time.Time
	Revoked   bool
	CreatedBy int64
	LastUsed  time.Time
}

// Usable reports whether the token can still admit a feeder at now.
func (t EnrolToken) Usable(now time.Time) bool {
	return !t.Revoked && t.Uses < t.MaxUses && now.Before(t.Expires)
}

func (r *Registry) tokenHash(tok string) string {
	return hex.EncodeToString(mac(r.keys.token, strings.ToLower(strings.TrimSpace(tok))))
}

// MintToken creates an enrol token. maxUses <= 0 and ttl <= 0 take the
// defaults (Options.TokenMaxUses, Options.TokenTTL). The returned secret is
// shown to the admin once; it cannot be recovered later.
func (r *Registry) MintToken(ctx context.Context, actor Actor, note string, maxUses int, ttl time.Duration) (secret string, tok EnrolToken, err error) {
	if maxUses <= 0 {
		maxUses = r.opts.TokenMaxUses
	}
	if maxUses > 100000 {
		return "", EnrolToken{}, fmt.Errorf("%w: maxUses %d", ErrInvalid, maxUses)
	}
	if ttl <= 0 {
		ttl = r.opts.TokenTTL
	}
	if ttl > 3650*24*time.Hour {
		return "", EnrolToken{}, fmt.Errorf("%w: token lifetime", ErrInvalid)
	}
	raw := make([]byte, 20)
	if _, err := io.ReadFull(r.rnd, raw); err != nil {
		return "", EnrolToken{}, err
	}
	idb := make([]byte, 6)
	if _, err := io.ReadFull(r.rnd, idb); err != nil {
		return "", EnrolToken{}, err
	}
	secret = TokenPrefix + strings.ToLower(tokenEnc.EncodeToString(raw))
	now := r.now()
	tok = EnrolToken{ID: "t" + hex.EncodeToString(idb), Note: sanitizeName(note), MaxUses: maxUses,
		Created: fromS(now.Unix()), Expires: fromS(now.Add(ttl).Unix()), CreatedBy: actor.ID}
	err = r.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO enrol_tokens(id, hash, note, max_uses, created, expires, created_by)
VALUES(?,?,?,?,?,?,?)`, tok.ID, r.tokenHash(secret), tok.Note, tok.MaxUses, unixS(tok.Created), unixS(tok.Expires), actor.ID); err != nil {
			return fmt.Errorf("registry: mint token: %w", err)
		}
		return insertAudit(ctx, tx, now, actor, "token.mint", "", fmt.Sprintf(`{"token":%q,"maxUses":%d}`, tok.ID, maxUses))
	}, nil)
	if err != nil {
		return "", EnrolToken{}, err
	}
	return secret, tok, nil
}

// redeemToken consumes one use of a token inside tx and returns its ID.
func (r *Registry) redeemToken(ctx context.Context, tx *sql.Tx, secret string, now time.Time) (string, error) {
	s := strings.ToLower(strings.TrimSpace(secret))
	if !strings.HasPrefix(s, TokenPrefix) || len(s) > 64 {
		return "", ErrTokenInvalid
	}
	h := r.tokenHash(s)
	var id string
	err := tx.QueryRowContext(ctx, `UPDATE enrol_tokens SET uses=uses+1, last_used=?
WHERE hash=? AND revoked=0 AND expires>? AND uses<max_uses RETURNING id`, now.Unix(), h, now.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTokenInvalid
	}
	if err != nil {
		return "", fmt.Errorf("registry: redeem token: %w", err)
	}
	return id, nil
}

// CheckToken reports whether a token would be accepted now, without
// consuming a use (for installers that validate before connecting).
func (r *Registry) CheckToken(ctx context.Context, secret string) (bool, error) {
	var n int
	err := r.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM enrol_tokens WHERE hash=? AND revoked=0 AND expires>? AND uses<max_uses`,
		r.tokenHash(secret), r.now().Unix()).Scan(&n)
	return n > 0, err
}

// RevokeToken revokes a token by ID (admin action, audited).
func (r *Registry) RevokeToken(ctx context.Context, actor Actor, id string) error {
	return r.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE enrol_tokens SET revoked=1 WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("registry: no enrol token %q", id)
		}
		return insertAudit(ctx, tx, r.now(), actor, "token.revoke", "", fmt.Sprintf(`{"token":%q}`, id))
	}, nil)
}

// Tokens lists enrol tokens, newest first.
func (r *Registry) Tokens(ctx context.Context) ([]EnrolToken, error) {
	rows, err := r.rdb.QueryContext(ctx, `SELECT id, note, max_uses, uses, created, expires, revoked, created_by, last_used
FROM enrol_tokens ORDER BY created DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnrolToken
	for rows.Next() {
		var t EnrolToken
		var c, e, l int64
		var rev int
		if err := rows.Scan(&t.ID, &t.Note, &t.MaxUses, &t.Uses, &c, &e, &rev, &t.CreatedBy, &l); err != nil {
			return nil, err
		}
		t.Created, t.Expires, t.LastUsed, t.Revoked = fromS(c), fromS(e), fromS(l), rev != 0
		out = append(out, t)
	}
	return out, rows.Err()
}
