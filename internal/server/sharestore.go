package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Share links (DESIGN §8.2 "My map … share links in phase 4", §14 WP-H2):
// an owner hands out a revocable, scoped, read-only view of what the own
// stations receive. The store keeps one JSON file in the data directory
// (<data>/sharelinks.json, mode 0600). Only the SHA-256 of a link's secret
// is stored: the full link is shown once, when it is created (like enrol
// tokens). The file is small (at most shareMaxTotal links) and written
// atomically on every change; use counters are written at most once per
// shareFlushEvery.

const (
	shareFile = "sharelinks.json"
	// shareMaxPerOwner bounds the active (not revoked, not expired) links
	// of one account; shareMaxTotal bounds the file.
	shareMaxPerOwner = 20
	shareMaxTotal    = 20000
	// shareMaxStations bounds the stations of one link.
	shareMaxStations = 16
	// shareMaxDays is the longest validity; shareDefaultDays the default.
	shareMaxDays     = 365
	shareDefaultDays = 30
	// shareKeepRevoked: revoked and expired links stay listed this long
	// (so the owner sees what happened), then they are dropped.
	shareKeepRevoked = 30 * 24 * time.Hour
	shareFlushEvery  = time.Minute
	// shareSecretBytes of randomness in a link (base64url, 32 characters).
	shareSecretBytes = 24
)

// Share scopes. "map" shows the aircraft the stations heard in the last
// 60 s; "map+stats" adds the stations' non-locating statistics.
const (
	shareScopeMap      = "map"
	shareScopeMapStats = "map+stats"
)

// shareLink is one stored link. Secret material: Hash only.
type shareLink struct {
	ID      string   `json:"id"`   // public handle (owner/admin lists, revoke)
	Hash    string   `json:"hash"` // hex SHA-256 of the secret
	OwnerID int64    `json:"ownerId"`
	SIDs    []string `json:"sids"`
	Label   string   `json:"label,omitempty"`
	Scope   string   `json:"scope"`
	// CoarseLow rounds low aircraft (below 1000 ft AGL and on the ground)
	// to the island-guard grid: they reveal the station's area best.
	CoarseLow bool   `json:"coarseLow"`
	Created   int64  `json:"created"`           // Unix ms
	Expires   int64  `json:"expires"`           // Unix ms
	Revoked   int64  `json:"revoked,omitempty"` // Unix ms, 0 = active
	RevokedBy string `json:"revokedBy,omitempty"`
	LastUsed  int64  `json:"lastUsed,omitempty"`
	Uses      uint64 `json:"uses,omitempty"`
}

// active reports whether the link may be used at t.
func (l *shareLink) active(t time.Time) bool {
	return l.Revoked == 0 && t.UnixMilli() < l.Expires
}

// state is "active", "revoked" or "expired".
func (l *shareLink) state(t time.Time) string {
	switch {
	case l.Revoked != 0:
		return "revoked"
	case t.UnixMilli() >= l.Expires:
		return "expired"
	}
	return "active"
}

type shareFileV1 struct {
	Version int          `json:"version"`
	Links   []*shareLink `json:"links"`
}

// shareStore holds the links. The zero value is not usable: see
// newShareStore. path "" keeps everything in memory (tests, no data dir).
type shareStore struct {
	mu      sync.Mutex
	path    string
	loaded  bool
	loadErr error
	links   []*shareLink
	byHash  map[string]*shareLink
	dirty   bool // use counters changed since the last write
	flushed time.Time
	now     func() time.Time
}

func newShareStore(path string) *shareStore {
	return &shareStore{path: path, byHash: map[string]*shareLink{}, now: time.Now}
}

var (
	errShareLimit    = errors.New("too many share links")
	errShareNotFound = errors.New("no such share link")
)

// load reads the file once (caller holds mu). A missing file is an empty
// store; a damaged file is reported and keeps the store read-only so that
// nothing overwrites it.
func (st *shareStore) load() error {
	if st.loaded {
		return st.loadErr
	}
	st.loaded = true
	if st.path == "" {
		return nil
	}
	b, err := os.ReadFile(st.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		st.loadErr = err
		return err
	}
	var f shareFileV1
	if err := json.Unmarshal(b, &f); err != nil {
		st.loadErr = errors.New("share links file is damaged: " + err.Error())
		return st.loadErr
	}
	for _, l := range f.Links {
		if l == nil || l.ID == "" || len(l.Hash) != 64 {
			continue
		}
		st.links = append(st.links, l)
		st.byHash[l.Hash] = l
	}
	return nil
}

// saveLocked writes the file atomically (caller holds mu).
func (st *shareStore) saveLocked() error {
	st.dirty = false
	st.flushed = st.now()
	if st.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(shareFileV1{Version: 1, Links: st.links}, "", " ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(st.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sharelinks-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		_ = os.Chmod(name, 0o600)
		werr = os.Rename(name, st.path)
	}
	if werr != nil {
		_ = os.Remove(name)
	}
	return werr
}

// pruneLocked drops links that ended more than shareKeepRevoked ago.
func (st *shareStore) pruneLocked(now time.Time) bool {
	cut := now.Add(-shareKeepRevoked).UnixMilli()
	n := 0
	for _, l := range st.links {
		end := l.Expires
		if l.Revoked != 0 {
			end = l.Revoked
		}
		if end < cut {
			delete(st.byHash, l.Hash)
			continue
		}
		st.links[n] = l
		n++
	}
	pruned := n != len(st.links)
	clear(st.links[n:])
	st.links = st.links[:n]
	return pruned
}

func hashShareSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// validShareSecret reports whether s has the shape of a secret (so that
// junk is refused before hashing; the answer is the same 404 either way).
func validShareSecret(s string) bool {
	if len(s) != base64.RawURLEncoding.EncodedLen(shareSecretBytes) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// create stores a new link and returns it with its secret (shown once).
func (st *shareStore) create(l shareLink, maxPerOwner int) (*shareLink, string, error) {
	if maxPerOwner <= 0 {
		maxPerOwner = shareMaxPerOwner
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.load(); err != nil {
		return nil, "", err
	}
	now := st.now()
	st.pruneLocked(now)
	n := 0
	for _, x := range st.links {
		if x.OwnerID == l.OwnerID && x.active(now) {
			n++
		}
	}
	if n >= maxPerOwner || len(st.links) >= shareMaxTotal {
		return nil, "", errShareLimit
	}
	var raw [shareSecretBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw[:])
	var idb [6]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return nil, "", err
	}
	l.ID = hex.EncodeToString(idb[:])
	l.Hash = hashShareSecret(secret)
	l.Created = now.UnixMilli()
	l.SIDs = slices.Clone(l.SIDs)
	c := l
	st.links = append(st.links, &c)
	st.byHash[c.Hash] = &c
	if err := st.saveLocked(); err != nil {
		st.links = st.links[:len(st.links)-1]
		delete(st.byHash, c.Hash)
		return nil, "", err
	}
	out := c
	return &out, secret, nil
}

// resolve returns a copy of the active link for secret and counts the use.
func (st *shareStore) resolve(secret string) (shareLink, bool) {
	if !validShareSecret(secret) {
		return shareLink{}, false
	}
	h := hashShareSecret(secret)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.load() != nil {
		return shareLink{}, false
	}
	l := st.byHash[h]
	// The map lookup already selected by the full hash; the comparison
	// keeps the check explicit and constant-time.
	if l == nil || subtle.ConstantTimeCompare([]byte(l.Hash), []byte(h)) != 1 {
		return shareLink{}, false
	}
	now := st.now()
	if !l.active(now) {
		return shareLink{}, false
	}
	l.Uses++
	l.LastUsed = now.UnixMilli()
	st.dirty = true
	if now.Sub(st.flushed) >= shareFlushEvery {
		_ = st.saveLocked()
	}
	c := *l
	c.SIDs = slices.Clone(l.SIDs)
	return c, true
}

// list returns copies of the links matching keep (nil = all), newest
// first.
func (st *shareStore) list(keep func(*shareLink) bool) ([]shareLink, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.load(); err != nil {
		return nil, err
	}
	if st.pruneLocked(st.now()) {
		_ = st.saveLocked()
	}
	out := []shareLink{}
	for i := len(st.links) - 1; i >= 0; i-- {
		l := st.links[i]
		if keep == nil || keep(l) {
			c := *l
			c.SIDs = slices.Clone(l.SIDs)
			out = append(out, c)
		}
	}
	return out, nil
}

// revoke ends a link. ownerID 0 = any owner (admin). It answers
// errShareNotFound for an unknown ID or another owner's link.
func (st *shareStore) revoke(id string, ownerID int64, by string) (shareLink, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.load(); err != nil {
		return shareLink{}, err
	}
	for _, l := range st.links {
		if l.ID != id || ownerID != 0 && l.OwnerID != ownerID {
			continue
		}
		if l.Revoked == 0 {
			l.Revoked = st.now().UnixMilli()
			l.RevokedBy = by
			if err := st.saveLocked(); err != nil {
				l.Revoked, l.RevokedBy = 0, ""
				return shareLink{}, err
			}
		}
		return *l, nil
	}
	return shareLink{}, errShareNotFound
}

// revokeOwner ends every active link of an account (account deletion).
func (st *shareStore) revokeOwner(ownerID int64, by string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.load() != nil {
		return 0
	}
	now := st.now()
	n := 0
	for _, l := range st.links {
		if l.OwnerID == ownerID && l.Revoked == 0 {
			l.Revoked, l.RevokedBy = now.UnixMilli(), by
			n++
		}
	}
	if n > 0 {
		_ = st.saveLocked()
	}
	return n
}

// flush writes pending use counters (shutdown).
func (st *shareStore) flush() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.loaded && st.loadErr == nil && st.dirty {
		_ = st.saveLocked()
	}
}
