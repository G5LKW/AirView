package auth

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"airview/internal/model"
)

// Role is the kind of an account.
type Role uint8

// Roles.
const (
	// RoleOwner owns stations (DESIGN §7): username + password, TOTP
	// optional.
	RoleOwner Role = iota + 1
	// RoleAdmin operates the network: password and TOTP are both
	// mandatory.
	RoleAdmin
)

// String returns "owner" or "admin".
func (r Role) String() string {
	switch r {
	case RoleOwner:
		return "owner"
	case RoleAdmin:
		return "admin"
	}
	return "invalid"
}

// Scope returns the session scope the role logs in with.
func (r Role) Scope() model.Scope {
	if r == RoleAdmin {
		return model.ScopeAdmin
	}
	return model.ScopeOwner
}

// Account is an owner or admin account. There is no e-mail address
// (DESIGN §16: "No e-mail and no magic links").
type Account struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"` // normalised (NormalizeUsername)
	Role         Role   `json:"role"`
	PasswordHash string `json:"passwordHash"` // HashPassword output
	// TOTPSecret is the confirmed shared secret in base32 ("" = TOTP off).
	TOTPSecret string `json:"totpSecret,omitempty"`
	// TOTPPending is a secret shown to the user but not yet confirmed with
	// a code; it becomes TOTPSecret on confirmation.
	TOTPPending string `json:"totpPending,omitempty"`
	// TOTPLastStep is the time step of the last accepted code (replay
	// protection).
	TOTPLastStep int64 `json:"totpLastStep,omitempty"`
	// RecoveryHashes are the unused recovery codes (hashRecovery).
	RecoveryHashes []string `json:"recoveryHashes,omitempty"`
	// CredEpoch increases whenever credentials change; sessions created
	// under an older epoch end.
	CredEpoch int64     `json:"credEpoch"`
	Disabled  bool      `json:"disabled,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	LastLogin time.Time `json:"lastLogin,omitzero"`
}

// TOTPEnabled reports whether a confirmed TOTP secret exists.
func (a Account) TOTPEnabled() bool { return a.TOTPSecret != "" }

// Clone returns a copy that shares no slices with a.
func (a Account) Clone() Account {
	a.RecoveryHashes = slices.Clone(a.RecoveryHashes)
	return a
}

// Account store errors.
var (
	ErrAccountNotFound = errors.New("account not found")
	ErrUsernameTaken   = errors.New("user name is already taken")
	ErrUsernameInvalid = errors.New("user name must be 3-32 characters: letters, digits, '.', '_' or '-', starting with a letter or digit")
)

// AccountStore keeps accounts. MemoryAccounts is the in-process
// implementation; internal/registry provides one backed by stations.db.
// Implementations must be safe for concurrent use and must store and
// return Username already normalised.
type AccountStore interface {
	// Create stores a new account, assigning ID (> 0) when it is 0, and
	// returns it. ErrUsernameTaken if the name exists.
	Create(ctx context.Context, a Account) (Account, error)
	// ByID / ByUsername return ErrAccountNotFound when absent.
	ByID(ctx context.Context, id int64) (Account, error)
	ByUsername(ctx context.Context, username string) (Account, error)
	// Update replaces a stored account (matched by ID).
	Update(ctx context.Context, a Account) error
	// Delete removes an account (no error if absent).
	Delete(ctx context.Context, id int64) error
	// ListRole returns all accounts of a role, ordered by ID.
	ListRole(ctx context.Context, r Role) ([]Account, error)
}

// NormalizeUsername returns the canonical (lower-case, trimmed) form of a
// user name and whether it is acceptable: 3-32 ASCII characters from
// [a-z0-9._-], starting with a letter or digit.
func NormalizeUsername(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) < 3 || len(s) > 32 {
		return s, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || c != '.' && c != '_' && c != '-') {
			return s, false
		}
	}
	return s, true
}

// MemoryAccounts is an in-memory AccountStore.
type MemoryAccounts struct {
	mu     sync.Mutex
	byID   map[int64]Account
	byName map[string]int64
	next   int64
}

// NewMemoryAccounts returns an empty store.
func NewMemoryAccounts() *MemoryAccounts {
	return &MemoryAccounts{byID: map[int64]Account{}, byName: map[string]int64{}, next: 1}
}

// Create implements AccountStore.
func (m *MemoryAccounts) Create(_ context.Context, a Account) (Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byName[a.Username]; ok {
		return Account{}, ErrUsernameTaken
	}
	if a.ID == 0 {
		a.ID = m.next
	}
	if _, ok := m.byID[a.ID]; ok {
		return Account{}, errors.New("account id already exists")
	}
	m.next = max(m.next, a.ID+1)
	a = a.Clone()
	m.byID[a.ID] = a
	m.byName[a.Username] = a.ID
	return a.Clone(), nil
}

// ByID implements AccountStore.
func (m *MemoryAccounts) ByID(_ context.Context, id int64) (Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.byID[id]
	if !ok {
		return Account{}, ErrAccountNotFound
	}
	return a.Clone(), nil
}

// ByUsername implements AccountStore.
func (m *MemoryAccounts) ByUsername(_ context.Context, name string) (Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byName[name]
	if !ok {
		return Account{}, ErrAccountNotFound
	}
	return m.byID[id].Clone(), nil
}

// Update implements AccountStore.
func (m *MemoryAccounts) Update(_ context.Context, a Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.byID[a.ID]
	if !ok {
		return ErrAccountNotFound
	}
	if old.Username != a.Username {
		if _, taken := m.byName[a.Username]; taken {
			return ErrUsernameTaken
		}
		delete(m.byName, old.Username)
		m.byName[a.Username] = a.ID
	}
	m.byID[a.ID] = a.Clone()
	return nil
}

// Delete implements AccountStore.
func (m *MemoryAccounts) Delete(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.byID[id]; ok {
		delete(m.byName, a.Username)
		delete(m.byID, id)
	}
	return nil
}

// ListRole implements AccountStore.
func (m *MemoryAccounts) ListRole(_ context.Context, r Role) ([]Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Account
	for _, a := range m.byID {
		if a.Role == r {
			out = append(out, a.Clone())
		}
	}
	slices.SortFunc(out, func(x, y Account) int { return cmp.Compare(x.ID, y.ID) })
	return out, nil
}
