package registry

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"airview/internal/model"
)

// Claiming (DESIGN §7).
//
// Native feeders: the feeder asks for a code over AVF (CLAIM record); the
// server mints 8 base32 characters (40 bits), valid 24 h, single use,
// stored as HMAC(claimKey, code). The owner enters it at /claim. Because a
// code proves physical control of the feeder, redeeming it also moves a
// station that another account owns (account recovery without e-mail,
// DESIGN §15).
//
// Compat feeders: the visitor must come from the station's current IP
// (checked by the caller, which knows the live links) and enter the full
// UUID. UUID-less stations need the admin (AssignOwner).
//
// Rate limits: 5 claim attempts per 10 minutes per client and 20 per day
// per account, successful or not.

// Claim errors. Their messages are safe to show to users.
var (
	ErrClaimInvalid   = errors.New("the claim code is wrong, expired or already used")
	ErrRateLimited    = errors.New("too many claim attempts; try again later")
	ErrNotClaimable   = errors.New("this station cannot be claimed this way")
	ErrAlreadyOwned   = errors.New("this station already belongs to another account; ask the operator")
	ErrNeedsAdmin     = errors.New("stations without a UUID are assigned by the network operator")
	ErrIPMismatch     = errors.New("claiming a compat feeder requires visiting from the feeder's own IP address")
	ErrNotOwner       = errors.New("this station does not belong to your account")
	ErrUnknownAccount = errors.New("unknown account")
)

// RateLimitError carries the wait time of ErrRateLimited.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }

// Unwrap makes errors.Is(err, ErrRateLimited) work.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// claimLimiter is a sliding-window limiter with an injectable clock.
type claimLimiter struct {
	n      int
	window time.Duration
	now    func() time.Time
	mu     *sync.Mutex
	keys   map[string][]time.Time
}

func newClaimLimiter(n int, window time.Duration, now func() time.Time) claimLimiter {
	return claimLimiter{n: n, window: window, now: now, mu: &sync.Mutex{}, keys: map[string][]time.Time{}}
}

func (l claimLimiter) allow(key string) (bool, time.Duration) {
	if key == "" {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ev := l.keys[key]
	i := 0
	for i < len(ev) && now.Sub(ev[i]) >= l.window {
		i++
	}
	ev = ev[i:]
	if len(ev) >= l.n {
		l.keys[key] = ev
		return false, ev[0].Add(l.window).Sub(now)
	}
	if len(l.keys) > 65536 { // bounded memory under a flood: drop idle keys
		for k, v := range l.keys {
			if len(v) == 0 || now.Sub(v[len(v)-1]) >= l.window {
				delete(l.keys, k)
			}
		}
	}
	l.keys[key] = append(ev, now)
	return true, 0
}

func (r *Registry) claimAllowed(ownerID int64, client string) error {
	if ok, wait := r.claimIP.allow(client); !ok {
		return &RateLimitError{RetryAfter: wait}
	}
	if ok, wait := r.claimAcct.allow(fmt.Sprint(ownerID)); !ok {
		return &RateLimitError{RetryAfter: wait}
	}
	return nil
}

func (r *Registry) claimHash(code string) string {
	return hex.EncodeToString(mac(r.keys.claim, code))
}

// MintClaim mints a claim code for a native feeder's station. At most
// Options.MaxOpenClaims unused codes exist per station; minting another
// drops the oldest. The code is returned once and never stored or logged
// in plain text.
func (r *Registry) MintClaim(ctx context.Context, idx model.StationIdx) (code string, expires time.Time, err error) {
	st, ok := r.Station(idx)
	if !ok || st.Deleting {
		return "", time.Time{}, ErrStationNotFound
	}
	if st.Kind != model.StationKindAVF || st.Admission == model.AdmRevoked || st.Admission == model.AdmDenied {
		return "", time.Time{}, ErrNotClaimable
	}
	code, err = model.NewClaimCode(r.rnd)
	if err != nil {
		return "", time.Time{}, err
	}
	now := r.now()
	expires = fromS(now.Add(r.opts.ClaimTTL).Unix())
	err = r.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM claims WHERE idx=? AND (used<>0 OR expires<=?)`, idx, now.Unix()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM claims WHERE code_hash IN (
 SELECT code_hash FROM claims WHERE idx=? AND used=0 ORDER BY created DESC, expires DESC LIMIT -1 OFFSET ?)`,
			idx, r.opts.MaxOpenClaims-1); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO claims(code_hash, idx, created, expires) VALUES(?,?,?,?)`,
			r.claimHash(code), idx, now.Unix(), unixS(expires))
		return err
	}, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("registry: mint claim: %w", err)
	}
	return code, expires, nil
}

// RedeemClaim gives the station of a claim code to ownerID. client is the
// rate-limit key of the visitor (its IP; "" = no per-client limit). The
// code is consumed atomically with the ownership change, so it can never
// be used twice, even by concurrent requests.
func (r *Registry) RedeemClaim(ctx context.Context, ownerID int64, code, client string) (Station, error) {
	if ownerID <= 0 {
		return Station{}, ErrUnknownAccount
	}
	if err := r.claimAllowed(ownerID, client); err != nil {
		return Station{}, err
	}
	c, ok := model.NormalizeClaimCode(code)
	if !ok {
		return Station{}, ErrClaimInvalid
	}
	now := r.now()
	var changed []*Station
	var out Station
	err := r.write(ctx, func(tx *sql.Tx) error {
		changed = nil
		if err := accountExists(ctx, tx, ownerID); err != nil {
			return err
		}
		var idx model.StationIdx
		err := tx.QueryRowContext(ctx, `UPDATE claims SET used=?, used_by=? WHERE code_hash=? AND used=0 AND expires>? RETURNING idx`,
			now.Unix(), ownerID, r.claimHash(c), now.Unix()).Scan(&idx)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrClaimInvalid
		}
		if err != nil {
			return err
		}
		cur := r.cache.load(idx)
		if cur == nil || cur.Deleting {
			return ErrClaimInvalid
		}
		next := *cur
		prev := cur.OwnerID
		next.OwnerID = ownerID
		if err := saveStation(ctx, tx, &next); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM claims WHERE idx=? AND used=0`, idx); err != nil {
			return err
		}
		detail := ""
		if prev != 0 && prev != ownerID {
			detail = fmt.Sprintf(`{"transferredFrom":%d}`, prev)
		}
		if err := insertAudit(ctx, tx, now, ownerActor(ctx, ownerID), "station.claim", next.SID, detail); err != nil {
			return err
		}
		changed = []*Station{&next}
		out = next
		return nil
	}, func() {
		for _, c := range changed {
			r.cache.put(c)
		}
	})
	return out, err
}

// ClaimCompat gives a compat station (UUID-keyed) to ownerID. sameIP must
// be the caller's check that the visitor comes from the station's current
// link IP (exact IPv4 or IPv6 /64, auth.SameClient); uuid must be the full
// UUID. client is the rate-limit key.
func (r *Registry) ClaimCompat(ctx context.Context, ownerID int64, uuid string, sameIP bool, client string) (Station, error) {
	if ownerID <= 0 {
		return Station{}, ErrUnknownAccount
	}
	if err := r.claimAllowed(ownerID, client); err != nil {
		return Station{}, err
	}
	u, ok := model.NormalizeUUID(uuid)
	if !ok {
		return Station{}, ErrNotClaimable
	}
	st, ok := r.ByUUID(u)
	if !ok {
		return Station{}, ErrNotClaimable
	}
	if st.Has(FlagUnidentified) {
		return Station{}, ErrNeedsAdmin
	}
	if !sameIP {
		return Station{}, ErrIPMismatch
	}
	if st.OwnerID != 0 && st.OwnerID != ownerID {
		return Station{}, ErrAlreadyOwned
	}
	return r.setOwner(ctx, ownerActor(ctx, ownerID), st.Idx, ownerID, "station.claim", func(cur *Station) error {
		if cur.OwnerID != 0 && cur.OwnerID != ownerID {
			return ErrAlreadyOwned
		}
		return nil
	})
}

// AssignOwner sets (or with ownerID 0 removes) a station's owner (admin
// action, audited).
func (r *Registry) AssignOwner(ctx context.Context, actor Actor, idx model.StationIdx, ownerID int64) (Station, error) {
	return r.setOwner(ctx, actor, idx, ownerID, "station.assign", nil)
}

// Unclaim releases a station the owner owns.
func (r *Registry) Unclaim(ctx context.Context, ownerID int64, idx model.StationIdx) (Station, error) {
	return r.setOwner(ctx, ownerActor(ctx, ownerID), idx, 0, "station.unclaim", func(cur *Station) error {
		if cur.OwnerID != ownerID || ownerID == 0 {
			return ErrNotOwner
		}
		return nil
	})
}

func (r *Registry) setOwner(ctx context.Context, actor Actor, idx model.StationIdx, ownerID int64, action string, check func(*Station) error) (Station, error) {
	var out Station
	var changed []*Station
	err := r.write(ctx, func(tx *sql.Tx) error {
		changed = nil
		cur := r.cache.load(idx)
		if cur == nil || cur.Deleting {
			return ErrStationNotFound
		}
		if check != nil {
			if err := check(cur); err != nil {
				return err
			}
		}
		if ownerID != 0 {
			if err := accountExists(ctx, tx, ownerID); err != nil {
				return err
			}
		}
		next := *cur
		if cur.OwnerID != 0 && ownerID != cur.OwnerID {
			// A station leaves its owner (unclaim, admin reassignment). A
			// first claim of a never-owned station keeps what the feeder
			// reported: that is the claimant's own device.
			r.resetForNewOwner(&next)
			// The previous owner's location history must not reach the next.
			if _, err := tx.ExecContext(ctx, `DELETE FROM location_estimates WHERE idx=?`, idx); err != nil {
				return fmt.Errorf("registry: owner change: %w", err)
			}
		} else if ownerID != 0 && ownerID != cur.OwnerID && !cur.OwnedSince.IsZero() {
			// A claim of a station that had an owner before: the new
			// owner's history starts now, not at the release.
			next.OwnedSince = r.now().UTC().Truncate(time.Second)
		}
		next.OwnerID = ownerID
		if err := saveStation(ctx, tx, &next); err != nil {
			return err
		}
		var err error
		if changed, err = r.refreshPublic(ctx, tx, cur, &next); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, r.now(), actor, action, next.SID, fmt.Sprintf(`{"owner":%d}`, ownerID)); err != nil {
			return err
		}
		out = next
		for _, c := range changed {
			if c.Idx == idx {
				out = *c
			}
		}
		return nil
	}, func() {
		for _, c := range changed {
			r.cache.put(c)
		}
	})
	if err == nil {
		r.mergeSeen(&out)
	}
	return out, err
}

// resetForNewOwner clears everything a station's previous owner set or that
// locates the previous owner's site (DESIGN §8.5): a feeder given away or
// sold keeps its SID, and the next owner must learn neither the old name
// nor the old home position, and the old public opt-ins must not carry
// over to the new site without the new owner's consent. The position is
// learnt again (GNSS report, survey or a new declaration); the public pin
// returns to the policy default. OwnedSince starts the new owner's history.
func (r *Registry) resetForNewOwner(st *Station) {
	st.Name = ""
	st.Declared, st.DeclaredAltRef = Position{}, ""
	st.GNSS = Position{}
	st.Estimated, st.EstSigmaM = Position{}, 0
	st.Effective, st.Located = model.StationGeo{}, false
	st.LocStatus = model.LocUnknown
	st.Anchor = false
	st.Pin = r.policy.Load().DefaultPin
	st.ShowName, st.PublicStats = false, false
	st.OwnedSince = r.now().UTC().Truncate(time.Second)
}

func accountExists(ctx context.Context, tx *sql.Tx, id int64) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id=?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrUnknownAccount
	}
	return nil
}
