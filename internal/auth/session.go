package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"time"

	"airview/internal/model"
)

// Session is one logged-in browser. The bearer token itself is never
// stored: ID is the hex SHA-256 of the token, so a leaked session table
// cannot be replayed.
//
// Raw client IPs are deliberately not part of a session (DESIGN §8.5:
// raw IPs are kept in memory only).
type Session struct {
	ID        string      `json:"id"`        // hex SHA-256(token)
	Scope     model.Scope `json:"scope"`     // ScopeOwner or ScopeAdmin
	AccountID int64       `json:"accountId"` // owning account
	Username  string      `json:"username"`
	// CredEpoch is Account.CredEpoch at login; a password change, TOTP
	// change or reset bumps the account's epoch and ends older sessions.
	CredEpoch int64     `json:"credEpoch"`
	CSRF      string    `json:"csrf"` // synchroniser token (csrf.go)
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"lastSeen"`
	Expires   time.Time `json:"expires"` // absolute end
	// Idle ends the session when LastSeen is older than this (0 = none).
	Idle time.Duration `json:"idle"`
	// MFA records that a second factor (TOTP or recovery code) was used.
	MFA bool `json:"mfa"`
}

// Valid reports whether the session is still usable at now (expiry and
// idle timeout only; the caller also checks the account).
func (s Session) Valid(now time.Time) bool {
	if s.ID == "" || !now.Before(s.Expires) {
		return false
	}
	if s.Idle > 0 && now.Sub(s.LastSeen) > s.Idle {
		return false
	}
	return true
}

// Viewer returns the model.Viewer for this session (network view).
func (s Session) Viewer() model.Viewer {
	return model.Viewer{Scope: s.Scope, OwnerID: s.AccountID}
}

// ErrSessionNotFound is returned by SessionStore.Get for unknown IDs.
var ErrSessionNotFound = errors.New("session not found")

// SessionStore keeps sessions. MemorySessions is the in-process
// implementation; internal/registry provides one backed by stations.db so
// sessions survive restarts. Implementations must be safe for concurrent
// use. All IDs are Session.ID values (token hashes), never raw tokens.
type SessionStore interface {
	// Put inserts or replaces a session.
	Put(ctx context.Context, s Session) error
	// Get returns a session or ErrSessionNotFound. It need not check
	// expiry (Service does).
	Get(ctx context.Context, id string) (Session, error)
	// Touch records activity. Stores may coalesce writes.
	Touch(ctx context.Context, id string, at time.Time) error
	// Delete removes one session (no error when it does not exist).
	Delete(ctx context.Context, id string) error
	// DeleteAccount removes every session of an account except keepID
	// ("" = all) and returns how many it removed.
	DeleteAccount(ctx context.Context, accountID int64, keepID string) (int, error)
	// DeleteAll removes every session.
	DeleteAll(ctx context.Context) error
	// List returns the sessions of one account, newest first.
	List(ctx context.Context, accountID int64) ([]Session, error)
	// Count returns the number of stored sessions.
	Count(ctx context.Context) (int, error)
	// Purge removes sessions that are no longer Valid at now.
	Purge(ctx context.Context, now time.Time) (int, error)
}

// NewSessionToken returns a fresh bearer token (32 random bytes,
// base64url) and its ID.
func NewSessionToken() (token, id string) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, SessionID(token)
}

// SessionID returns the storage ID of a token.
func SessionID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// MemorySessions is an in-memory SessionStore with bounded size. When Max
// is reached the session that expires first is evicted; MaxPerAccount
// limits the sessions of one account the same way.
type MemorySessions struct {
	Max           int
	MaxPerAccount int

	mu     sync.Mutex
	m      map[string]Session
	byAcct map[int64]map[string]struct{} // account -> session IDs
}

// NewMemorySessions returns a store with the given bounds (0 = defaults:
// 100 000 sessions, 32 per account).
func NewMemorySessions(maxTotal, maxPerAccount int) *MemorySessions {
	if maxTotal <= 0 {
		maxTotal = 100_000
	}
	if maxPerAccount <= 0 {
		maxPerAccount = 32
	}
	return &MemorySessions{Max: maxTotal, MaxPerAccount: maxPerAccount,
		m: map[string]Session{}, byAcct: map[int64]map[string]struct{}{}}
}

// Put implements SessionStore.
func (m *MemorySessions) Put(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, exists := m.m[s.ID]; exists {
		m.del(old.ID)
	} else {
		for len(m.byAcct[s.AccountID]) >= m.MaxPerAccount {
			var mine []Session
			for id := range m.byAcct[s.AccountID] {
				mine = append(mine, m.m[id])
			}
			m.del(mine[firstExpiring(mine)].ID)
		}
		if len(m.m) >= m.Max {
			now := time.Now()
			for k, o := range m.m {
				if !o.Valid(now) {
					m.del(k)
				}
			}
			for len(m.m) >= m.Max {
				var oldK string
				var oldT time.Time
				for k, o := range m.m {
					if oldK == "" || o.Expires.Before(oldT) {
						oldK, oldT = k, o.Expires
					}
				}
				m.del(oldK)
			}
		}
	}
	m.m[s.ID] = s
	set := m.byAcct[s.AccountID]
	if set == nil {
		set = map[string]struct{}{}
		m.byAcct[s.AccountID] = set
	}
	set[s.ID] = struct{}{}
	return nil
}

// del removes one session and its index entry (m.mu held).
func (m *MemorySessions) del(id string) {
	s, ok := m.m[id]
	if !ok {
		return
	}
	delete(m.m, id)
	if set := m.byAcct[s.AccountID]; set != nil {
		delete(set, id)
		if len(set) == 0 {
			delete(m.byAcct, s.AccountID)
		}
	}
}

func firstExpiring(list []Session) int {
	best := 0
	for i, s := range list {
		if s.Expires.Before(list[best].Expires) {
			best = i
		}
	}
	return best
}

// Get implements SessionStore.
func (m *MemorySessions) Get(_ context.Context, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.m[id]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return s, nil
}

// Touch implements SessionStore.
func (m *MemorySessions) Touch(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.m[id]; ok && at.After(s.LastSeen) {
		s.LastSeen = at
		m.m[id] = s
	}
	return nil
}

// Delete implements SessionStore.
func (m *MemorySessions) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	m.del(id)
	m.mu.Unlock()
	return nil
}

// DeleteAccount implements SessionStore.
func (m *MemorySessions) DeleteAccount(_ context.Context, accountID int64, keepID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.byAcct[accountID] {
		if k != keepID {
			m.del(k)
			n++
		}
	}
	return n, nil
}

// DeleteAll implements SessionStore.
func (m *MemorySessions) DeleteAll(context.Context) error {
	m.mu.Lock()
	clear(m.m)
	clear(m.byAcct)
	m.mu.Unlock()
	return nil
}

// List implements SessionStore.
func (m *MemorySessions) List(_ context.Context, accountID int64) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for id := range m.byAcct[accountID] {
		out = append(out, m.m[id])
	}
	slices.SortFunc(out, func(a, b Session) int { return b.Created.Compare(a.Created) })
	return out, nil
}

// Count implements SessionStore.
func (m *MemorySessions) Count(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m), nil
}

// Purge implements SessionStore.
func (m *MemorySessions) Purge(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, s := range m.m {
		if !s.Valid(now) {
			m.del(k)
			n++
		}
	}
	return n, nil
}
