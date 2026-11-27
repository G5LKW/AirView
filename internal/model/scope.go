package model

import "strconv"

// Scope is the visibility level of a request or a piece of data in
// aggregator mode (docs/aggregator/DESIGN.md §8). Every route declares the
// minimum Scope it needs; deny is the default.
type Scope uint8

// Scopes, ordered from least to most privileged.
const (
	// ScopePublic: no session. Never sees station coordinates, receiver
	// lists, IPs or ns-precision timestamps.
	ScopePublic Scope = iota
	// ScopeOwner: a logged-in station owner; exact data only for the
	// owner's own stations.
	ScopeOwner
	// ScopeAdmin: the network operator; sees everything (exact-data views
	// are audited).
	ScopeAdmin
)

// String returns "public", "owner" or "admin".
func (s Scope) String() string {
	switch s {
	case ScopePublic:
		return "public"
	case ScopeOwner:
		return "owner"
	case ScopeAdmin:
		return "admin"
	}
	return "scope" + strconv.Itoa(int(s))
}

// ParseScope parses a String result.
func ParseScope(v string) (Scope, bool) {
	switch v {
	case "public":
		return ScopePublic, true
	case "owner":
		return ScopeOwner, true
	case "admin":
		return ScopeAdmin, true
	}
	return ScopePublic, false
}

// Valid reports whether s is a defined scope.
func (s Scope) Valid() bool { return s <= ScopeAdmin }

// Allows reports whether a request with scope s may use something that
// requires scope need. An undefined s allows nothing beyond public.
func (s Scope) Allows(need Scope) bool {
	if !s.Valid() {
		return need == ScopePublic
	}
	return s >= need
}

// Viewer identifies who a response is produced for. Response caches must
// key on Viewer.CacheKey so that data of one scope, owner or station is
// never served to another.
type Viewer struct {
	Scope   Scope
	OwnerID int64  // owner account ID; 0 for public and for an admin without an owner account
	SID     string // selected station ("" = network view)
}

// CacheKey returns a key that differs for every (scope, owner, station).
func (v Viewer) CacheKey() string {
	return v.Scope.String() + "|" + strconv.FormatInt(v.OwnerID, 10) + "|" + v.SID
}

// PublicViewer is the anonymous network viewer.
var PublicViewer = Viewer{Scope: ScopePublic}
