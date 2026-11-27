package registry

import (
	"net/netip"
	"sync"
)

// CloneGuard tracks the live sessions of native feeder keys in memory
// (raw IPs never leave this process) and implements the clone rule of
// docs/aggregator/AVF.md §9.2: a second session with the same key from a
// different IP while the first is alive is refused ("cloned_feeder"); from
// the same IP the new session replaces the old one. The zero value is not
// usable; use NewCloneGuard.
type CloneGuard struct {
	mu   sync.Mutex
	live map[string]cloneEntry
	next uint64
}

type cloneEntry struct {
	ip    netip.Addr
	token uint64
}

// NewCloneGuard returns an empty guard.
func NewCloneGuard() *CloneGuard { return &CloneGuard{live: map[string]cloneEntry{}} }

// CloneDecision is the result of CloneGuard.Acquire.
type CloneDecision int

// Decisions.
const (
	CloneOK       CloneDecision = iota // no live session: proceed
	CloneReplace                       // same IP: proceed and close the old session (GOODBYE "replaced")
	CloneRejected                      // other IP: refuse with "cloned_feeder"
)

// Acquire registers a new session of feederID from ip. On CloneOK and
// CloneReplace it returns a token for Release; on CloneRejected the
// existing session is kept.
func (g *CloneGuard) Acquire(feederID string, ip netip.Addr) (CloneDecision, uint64) {
	ip = ip.Unmap()
	g.mu.Lock()
	defer g.mu.Unlock()
	d := CloneOK
	if cur, ok := g.live[feederID]; ok {
		if cur.ip != ip {
			return CloneRejected, 0
		}
		d = CloneReplace
	}
	g.next++
	g.live[feederID] = cloneEntry{ip: ip, token: g.next}
	return d, g.next
}

// Release ends the session holding token. A replaced session's Release
// does nothing.
func (g *CloneGuard) Release(feederID string, token uint64) {
	g.mu.Lock()
	if cur, ok := g.live[feederID]; ok && cur.token == token {
		delete(g.live, feederID)
	}
	g.mu.Unlock()
}

// Live returns the number of feeders with a live session.
func (g *CloneGuard) Live() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.live)
}
