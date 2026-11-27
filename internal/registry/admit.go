package registry

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"airview/internal/model"
)

// Admission statuses returned to a connecting feeder (AVF WELCOME status).
const (
	StatusApproved = "approved"
	StatusPending  = "pending"
	StatusDenied   = "denied"
)

// Denial codes (docs/aggregator/AVF.md §10).
const (
	CodeBadHello      = "bad_hello"
	CodeEnrolRequired = "enrol_required"
	CodeEnrolInvalid  = "enrol_invalid"
	CodeDenied        = "denied"
	CodeRevoked       = "revoked"
	CodeLimitStations = "limit_stations"
)

// Feeder states (feeders.state).
const (
	FeederPending  = "pending"
	FeederApproved = "approved"
	FeederRevoked  = "revoked"
)

// AdmitRequest describes a station link that wants to join.
type AdmitRequest struct {
	// Kind is model.StationKindAVF, StationKindBeast or StationKindMLATC
	// (the host station uses EnsureHost).
	Kind string
	// FeederPub is the native feeder's Ed25519 public key (AVF only; the
	// caller has verified the handshake signature with it).
	FeederPub []byte
	// RxNo is the receiver number within a native feeder (AVF only).
	RxNo int
	// UUID is the compat link's UUID (raw; normalised here). For AVF it is
	// informational and stored on the feeder.
	UUID string
	// RelaySub is a trusted relay's receiver ID (0xE3); the station then is
	// the sub-station "<uuid>/<relaySub>". Pass it only for TrustedRelays.
	RelaySub string
	// IdentKey identifies a compat link without UUID: UnidentifiedKey(ip, 0).
	IdentKey string
	// EnrolToken is the enrol token presented by an AVF feeder.
	EnrolToken string
	// Name is a suggested display name for a new station.
	Name string
	// ClockCls is the link's clock class (0 = unknown, keep the stored one).
	ClockCls model.ClockClass
	// PrivacyOff: an mlat-client that sent "privacy": false. New stations
	// then get the region pin instead of the default (DESIGN §19.2).
	PrivacyOff bool
}

// AdmitResult is the outcome of Admit.
type AdmitResult struct {
	Station Station // zero when denied before a station existed
	Status  string  // StatusApproved, StatusPending or StatusDenied
	Code    string  // denial code
	Reason  string  // human-readable denial reason
	New     bool    // a station row was created
}

// Allowed reports whether the link may stay connected (approved or
// pending). Pending stations feed their own lane only (DESIGN §3).
func (a AdmitResult) Allowed() bool { return a.Status != StatusDenied }

func denied(code, reason string) AdmitResult {
	return AdmitResult{Status: StatusDenied, Code: code, Reason: reason}
}

// Admit identifies a connecting station link, creates the station when it
// is new, and applies the admission policy (DESIGN §3):
//
//   - open: new stations are approved.
//   - approve: new stations are held pending until the admin approves.
//   - invite: native feeders need an enrol token (a feeder that was
//     admitted once may add receivers without a new token); compat
//     stations with a UUID are held pending for the admin (they cannot
//     present a token); compat links without a UUID are refused.
//
// Existing stations keep their state: revoked and denied stations are
// refused, pending ones are approved when the policy has become "open".
// Clone detection and per-IP limits need live connection state and are
// done by the caller (see CloneGuard).
func (r *Registry) Admit(ctx context.Context, req AdmitRequest) (AdmitResult, error) {
	var feederID, uuid string
	switch req.Kind {
	case model.StationKindAVF:
		if len(req.FeederPub) != ed25519.PublicKeySize {
			return denied(CodeBadHello, "feeder public key must be 32 bytes"), nil
		}
		if req.RxNo < 0 || req.RxNo > 255 {
			return denied(CodeBadHello, "receiver number out of range"), nil
		}
		feederID = model.FeederIDFromPub(req.FeederPub)
		if u, ok := model.NormalizeUUID(req.UUID); ok {
			uuid = u
		}
	case model.StationKindBeast, model.StationKindMLATC:
		if req.UUID != "" {
			u, ok := model.NormalizeUUID(req.UUID)
			if !ok {
				return denied(CodeBadHello, "malformed UUID"), nil
			}
			uuid = u
			if req.RelaySub != "" {
				sub := strings.ToLower(strings.TrimSpace(req.RelaySub))
				if len(sub) == 0 || len(sub) > 32 || strings.ContainsAny(sub, "/ ") {
					return denied(CodeBadHello, "malformed relay receiver ID"), nil
				}
				uuid += "/" + sub
			}
		} else if !strings.HasPrefix(req.IdentKey, "ip:") {
			return denied(CodeBadHello, "compat link without UUID needs an identity key"), nil
		}
	default:
		return AdmitResult{}, fmt.Errorf("%w: kind %q", ErrInvalid, req.Kind)
	}

	pol := r.Policy()
	var res AdmitResult
	var changed []*Station
	err := r.writeHigh(ctx, func(tx *sql.Tx) error {
		res, changed = AdmitResult{}, nil
		now := r.now()

		// Native feeder row.
		var fState, fToken string
		feederKnown := false
		if feederID != "" {
			err := tx.QueryRowContext(ctx, `SELECT state, token_id FROM feeders WHERE feeder_id=?`, feederID).Scan(&fState, &fToken)
			switch {
			case err == nil:
				feederKnown = true
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			if fState == FeederRevoked {
				res = denied(CodeRevoked, "this feeder has been revoked by the network operator")
				return nil
			}
		}

		// Existing station?
		var cur *Station
		switch {
		case feederID != "":
			cur = r.cache.lookup(func(c *stationCache) (model.StationIdx, bool) {
				i, ok := c.byFeeder[feederRx{feederID, req.RxNo}]
				return i, ok
			})
		case uuid != "":
			cur = r.cache.lookup(func(c *stationCache) (model.StationIdx, bool) { i, ok := c.byUUID[uuid]; return i, ok })
		default:
			cur = r.cache.lookup(func(c *stationCache) (model.StationIdx, bool) { i, ok := c.byIdent[req.IdentKey]; return i, ok })
		}
		if cur != nil {
			if cur.Deleting {
				res = denied(CodeDenied, "this station is being deleted; reconnect later to register again")
				return nil
			}
			next := *cur
			switch cur.Admission {
			case model.AdmRevoked:
				res = denied(CodeRevoked, "this station has been revoked by the network operator")
				res.Station = *cur
				return nil
			case model.AdmDenied:
				res = denied(CodeDenied, "this station was refused by the network operator")
				res.Station = *cur
				return nil
			case model.AdmPending:
				if pol.Admission == model.AdmissionOpen {
					next.Admission = model.AdmApproved
				}
			}
			switch req.Kind {
			case model.StationKindBeast:
				next.Flags |= FlagBeast
			case model.StationKindMLATC:
				next.Flags |= FlagMLATC
				if next.Kind == model.StationKindBeast {
					next.Kind = model.StationKindMLATC // the timing-capable link names the station
				}
			}
			if req.ClockCls != 0 && req.ClockCls.Valid() {
				next.ClockCls = req.ClockCls
			}
			next.LastSeen = now
			if next != *cur {
				if err := saveStation(ctx, tx, &next); err != nil {
					return err
				}
				var err error
				if changed, err = r.refreshPublic(ctx, tx, cur, &next); err != nil {
					return err
				}
			}
			if feederID != "" {
				if err := touchFeeder(ctx, tx, feederID, next.Admission, now.Unix()); err != nil {
					return err
				}
			}
			res = AdmitResult{Station: next, Status: next.Admission}
			if next.Admission != model.AdmApproved {
				res.Status = StatusPending
			}
			return nil
		}

		// New station: admission decision.
		adm := model.AdmPending
		tokenID := ""
		switch pol.Admission {
		case model.AdmissionOpen:
			adm = model.AdmApproved
		case model.AdmissionApprove:
			adm = model.AdmPending
		case model.AdmissionInvite:
			switch {
			case feederID != "" && feederKnown && fState == FeederApproved:
				adm, tokenID = model.AdmApproved, fToken
			case feederID != "":
				if req.EnrolToken == "" {
					res = denied(CodeEnrolRequired, "this network is invite-only: ask the operator for an enrol token")
					return nil
				}
				id, err := r.redeemToken(ctx, tx, req.EnrolToken, now)
				if err != nil {
					if errors.Is(err, ErrTokenInvalid) {
						res = denied(CodeEnrolInvalid, "the enrol token is unknown, expired, revoked or used up")
						return nil
					}
					return err
				}
				adm, tokenID = model.AdmApproved, id
			case uuid != "":
				adm = model.AdmPending
			default:
				res = denied(CodeEnrolRequired, "this network is invite-only: configure a UUID on the feeder and ask the operator to approve it")
				return nil
			}
		}
		approved, pending := r.countAdmission()
		if adm == model.AdmApproved && approved >= pol.MaxStations {
			res = denied(CodeLimitStations, "the network has reached its station limit")
			return nil
		}
		if adm == model.AdmPending && pending >= pol.MaxStations {
			res = denied(CodeLimitStations, "too many stations are waiting for approval")
			return nil
		}

		if feederID != "" {
			fs := FeederPending
			if adm == model.AdmApproved {
				fs = FeederApproved
			}
			if !feederKnown {
				if _, err := tx.ExecContext(ctx, `INSERT INTO feeders(feeder_id, pubkey, uuid, state, first_seen, last_seen, token_id)
VALUES(?,?,?,?,?,?,?)`, feederID, req.FeederPub, uuid, fs, now.Unix(), now.Unix(), tokenID); err != nil {
					return fmt.Errorf("registry: insert feeder: %w", err)
				}
			} else if err := touchFeeder(ctx, tx, feederID, adm, now.Unix()); err != nil {
				return err
			}
			if tokenID != "" && feederKnown && fToken == "" {
				if _, err := tx.ExecContext(ctx, `UPDATE feeders SET token_id=? WHERE feeder_id=?`, tokenID, feederID); err != nil {
					return err
				}
			}
		}

		st := Station{
			FeederID: feederID, RxNo: req.RxNo, Kind: req.Kind,
			Name: sanitizeName(req.Name), ClockCls: req.ClockCls, LocStatus: model.LocUnknown,
			Pin: pol.DefaultPin, Admission: adm, TokenID: tokenID, Created: now, LastSeen: now,
		}
		if !st.ClockCls.Valid() {
			st.ClockCls = 0
		}
		if !pol.NoTimingQuarantine && req.Kind != model.StationKindBeast { // Beast links never reach MLAT
			st.Flags |= FlagTimingQuarantine
		}
		if req.Kind != model.StationKindAVF {
			st.UUID = uuid
			if uuid == "" {
				st.IdentKey = req.IdentKey
				st.Flags |= FlagUnidentified
			}
			if req.RelaySub != "" {
				st.Flags |= FlagRelaySub
			}
			if req.Kind == model.StationKindBeast {
				st.Flags |= FlagBeast
			} else {
				st.Flags |= FlagMLATC
			}
			if req.Kind == model.StationKindMLATC && req.PrivacyOff {
				st.Pin = model.PinRegion
			}
		}
		if err := r.insertStation(ctx, tx, &st, 0); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, now, SystemActor, "station.register", st.SID,
			fmt.Sprintf(`{"kind":%q,"admission":%q}`, st.Kind, st.Admission)); err != nil {
			return err
		}
		changed = []*Station{&st}
		res = AdmitResult{Station: st, Status: StatusPending, New: true}
		if adm == model.AdmApproved {
			res.Status = StatusApproved
		}
		return nil
	}, func() {
		for _, c := range changed {
			r.cache.put(c)
		}
	})
	if err != nil {
		return AdmitResult{}, err
	}
	for _, c := range changed {
		if c.Idx == res.Station.Idx {
			res.Station = *c
		}
	}
	return res, nil
}

func touchFeeder(ctx context.Context, tx *sql.Tx, feederID, adm string, now int64) error {
	q := `UPDATE feeders SET last_seen=MAX(last_seen, ?) WHERE feeder_id=?`
	if adm == model.AdmApproved {
		q = `UPDATE feeders SET last_seen=MAX(last_seen, ?), state=CASE WHEN state='revoked' THEN state ELSE 'approved' END WHERE feeder_id=?`
	}
	_, err := tx.ExecContext(ctx, q, now, feederID)
	return err
}

func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if rs := []rune(s); len(rs) > 64 {
		s = string(rs[:64])
	}
	return s
}

// countAdmission counts approved and pending stations (cache).
func (r *Registry) countAdmission() (approved, pending int) {
	r.cache.each(func(st *Station) {
		if st.Deleting {
			return
		}
		switch st.Admission {
		case model.AdmApproved:
			approved++
		case model.AdmPending:
			pending++
		}
	})
	return
}

// insertStation inserts st with a fresh SID and fills st.Idx (idx 0 =
// AUTOINCREMENT, otherwise the given index).
func (r *Registry) insertStation(ctx context.Context, tx *sql.Tx, st *Station, idx model.StationIdx) error {
	for attempt := 0; ; attempt++ {
		sid, err := model.NewSID(r.rnd)
		if err != nil {
			return err
		}
		var idxArg any
		if idx != 0 {
			idxArg = int64(idx)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO stations(idx, sid, feeder_id, rx_no, kind, uuid, ident_key, name,
 clock_class, loc_status, pin, owner_id, admission, flags, token_id, created, last_seen)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			idxArg, sid, st.FeederID, st.RxNo, st.Kind, st.UUID, st.IdentKey, st.Name,
			int(st.ClockCls), st.LocStatus, st.Pin, st.OwnerID, st.Admission, int64(st.Flags), st.TokenID,
			unixS(st.Created), unixS(st.LastSeen))
		if err != nil {
			if attempt < 3 && strings.Contains(err.Error(), "stations.sid") {
				continue // 80-bit collision: practically impossible, but cheap to handle
			}
			return fmt.Errorf("registry: insert station: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		st.Idx, st.SID = model.StationIdx(id), sid
		st.Created, st.LastSeen = fromS(unixS(st.Created)), fromS(unixS(st.LastSeen))
		return nil
	}
}

// EnsureHost registers (or updates) the aggregator's in-process host
// station: idx 1 (model.HostStation), kind host, approved, owned by
// ownerID (the admin account; 0 = not yet known). Its location is set
// through the usual setters.
func (r *Registry) EnsureHost(ctx context.Context, name string, ownerID int64) (Station, error) {
	var changed []*Station
	var out Station
	err := r.write(ctx, func(tx *sql.Tx) error {
		changed = nil
		cur := r.cache.load(model.HostStation)
		if cur != nil {
			next := *cur
			if name != "" {
				next.Name = sanitizeName(name)
			}
			if ownerID != 0 {
				next.OwnerID = ownerID
			}
			next.Admission, next.Deleting = model.AdmApproved, false
			if next != *cur {
				if err := saveStation(ctx, tx, &next); err != nil {
					return err
				}
				c, err := r.refreshPublic(ctx, tx, cur, &next)
				if err != nil {
					return err
				}
				changed = c
			}
			out = next
			return nil
		}
		now := r.now()
		st := Station{Kind: model.StationKindHost, Name: sanitizeName(name), LocStatus: model.LocUnknown,
			Pin: model.PinHidden, OwnerID: ownerID, Admission: model.AdmApproved, Created: now, LastSeen: now}
		if err := r.insertStation(ctx, tx, &st, model.HostStation); err != nil {
			return err
		}
		changed = []*Station{&st}
		out = st
		return nil
	}, func() {
		for _, c := range changed {
			r.cache.put(c)
		}
	})
	return out, err
}

// SetAdmission changes a station's admission state (admin action, audited
// as "station.<state>"). Approving a native feeder's station also marks
// the feeder approved, so it can add receivers under invite admission.
func (r *Registry) SetAdmission(ctx context.Context, actor Actor, idx model.StationIdx, state string) (Station, error) {
	switch state {
	case model.AdmApproved, model.AdmPending, model.AdmDenied, model.AdmRevoked:
	default:
		return Station{}, fmt.Errorf("%w: admission %q", ErrInvalid, state)
	}
	if state == model.AdmApproved {
		if st, ok := r.Station(idx); ok && st.Admission != model.AdmApproved {
			if a, _ := r.countAdmission(); a >= r.Policy().MaxStations {
				return Station{}, fmt.Errorf("registry: the station limit (%d) is reached", r.Policy().MaxStations)
			}
		}
	}
	st, err := r.Update(ctx, idx, func(s *Station) error { s.Admission = state; return nil })
	if err != nil {
		return st, err
	}
	if st.FeederID != "" && state == model.AdmApproved {
		_ = r.write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE feeders SET state='approved' WHERE feeder_id=? AND state='pending'`, st.FeederID)
			return err
		}, nil)
	}
	return st, r.Audit(ctx, actor, "station."+state, st.SID, "")
}

// Feeder is a native feeder (one Ed25519 key).
type Feeder struct {
	FeederID  string
	PubKey    []byte
	UUID      string
	State     string
	FirstSeen int64 // Unix seconds
	LastSeen  int64
	TokenID   string
}

// Feeder returns a native feeder by ID.
func (r *Registry) Feeder(ctx context.Context, feederID string) (Feeder, bool, error) {
	var f Feeder
	err := r.rdb.QueryRowContext(ctx, `SELECT feeder_id, pubkey, uuid, state, first_seen, last_seen, token_id FROM feeders WHERE feeder_id=?`,
		feederID).Scan(&f.FeederID, &f.PubKey, &f.UUID, &f.State, &f.FirstSeen, &f.LastSeen, &f.TokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return Feeder{}, false, nil
	}
	return f, err == nil, err
}

// RevokeFeeder revokes a native feeder key and every station under it
// (admin action, audited). The key is refused from then on.
func (r *Registry) RevokeFeeder(ctx context.Context, actor Actor, feederID string) error {
	var changed []*Station
	err := r.write(ctx, func(tx *sql.Tx) error {
		changed = nil
		res, err := tx.ExecContext(ctx, `UPDATE feeders SET state='revoked' WHERE feeder_id=?`, feederID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrStationNotFound
		}
		var list []*Station
		r.cache.each(func(st *Station) {
			if st.FeederID == feederID && !st.Deleting {
				list = append(list, st)
			}
		})
		for _, cur := range list {
			next := *cur
			next.Admission = model.AdmRevoked
			if err := saveStation(ctx, tx, &next); err != nil {
				return err
			}
			c, err := r.refreshPublic(ctx, tx, cur, &next)
			if err != nil {
				return err
			}
			changed = append(changed, c...)
		}
		return insertAudit(ctx, tx, r.now(), actor, "feeder.revoke", "", fmt.Sprintf(`{"feeder":%q}`, feederID))
	}, func() {
		for _, c := range changed {
			r.cache.put(c)
		}
	})
	return err
}
