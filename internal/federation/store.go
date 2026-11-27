package federation

import (
	"slices"
	"sync"

	"airview/internal/privacy/publicdto"
)

// remoteEntry is the best known remote state of one aircraft. Every time
// in a is in the local clock (rebased on receipt).
type remoteEntry struct {
	a      publicdto.Aircraft
	key    uint32
	posMs  int64    // local ms of the position (0 = none)
	path   []string // origin first, sending peer last
	peer   string   // node ID of the sending peer
	recvMs int64    // local ms of receipt
	seq    uint64   // store sequence of the last update
}

// store holds remote aircraft, one entry per address: the freshest report,
// preferring shorter paths among reports of about the same age.
type store struct {
	mu      sync.RWMutex
	m       map[uint32]*remoteEntry
	perPeer map[string]int
	seq     uint64
	max     int
	ttlMs   int64
}

func newStore(max int, ttlMs int64) *store {
	return &store{m: map[uint32]*remoteEntry{}, perPeer: map[string]int{}, max: max, ttlMs: ttlMs}
}

// sameAgeMs is the window within which two reports count as equally fresh
// and the shorter path wins.
const sameAgeMs = 2000

// rebase converts a decoded aircraft (sender clock, tMs) into the local
// clock (nowMs) in place and returns the local position time.
func rebase(a *publicdto.Aircraft, tMs, nowMs int64) (posMs int64) {
	off := nowMs - tMs
	if a.LastSeen != 0 {
		a.LastSeen += off
	}
	if a.FirstSeen != 0 {
		a.FirstSeen += off
	}
	if a.LastSeen > nowMs {
		a.LastSeen = nowMs
	}
	if a.FirstSeen > a.LastSeen {
		a.FirstSeen = a.LastSeen
	}
	if a.Lat != nil && a.PosAge != nil {
		posMs = nowMs - int64(*a.PosAge*1000)
	} else if a.Lat != nil {
		posMs = a.LastSeen
	}
	return posMs
}

// upsert offers one report. It returns whether the report became the best
// entry and the LastSeen of the entry it replaced (0 = none). peerMax
// bounds the entries held from one peer.
func (s *store) upsert(peer string, path []string, a *publicdto.Aircraft, key uint32, posMs, nowMs int64, peerMax int) (bool, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[key]
	var prev int64
	if e != nil {
		prev = e.a.LastSeen
		expired := nowMs-e.recvMs > s.ttlMs
		replace := expired
		switch {
		case replace:
		case e.peer == peer && slices.Equal(e.path, path):
			replace = a.LastSeen >= e.a.LastSeen
		case a.LastSeen > e.a.LastSeen+sameAgeMs:
			replace = true
		case len(path) < len(e.path):
			replace = a.LastSeen >= e.a.LastSeen-sameAgeMs
		case len(path) == len(e.path):
			replace = a.LastSeen > e.a.LastSeen
		}
		if !replace {
			return false, prev
		}
		if e.peer != peer {
			if s.perPeer[peer] >= peerMax {
				return false, prev
			}
			s.perPeer[e.peer]--
			s.perPeer[peer]++
		}
	} else {
		if len(s.m) >= s.max || s.perPeer[peer] >= peerMax {
			return false, 0
		}
		e = &remoteEntry{key: key}
		s.m[key] = e
		s.perPeer[peer]++
	}
	s.seq++
	e.a, e.posMs, e.peer, e.recvMs, e.seq = *a, posMs, peer, nowMs, s.seq
	if !slices.Equal(e.path, path) {
		e.path = slices.Clone(path)
	}
	return true, prev
}

// expire removes entries older than the TTL and returns how many.
func (s *store) expire(nowMs int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, e := range s.m {
		if nowMs-e.recvMs > s.ttlMs {
			delete(s.m, k)
			s.perPeer[e.peer]--
			n++
		}
	}
	return n
}

// len returns the number of entries.
func (s *store) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

// fromPeer returns the number of entries the peer supplied.
func (s *store) fromPeer(peer string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.perPeer[peer]
}

// curSeq returns the current sequence.
func (s *store) curSeq() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seq
}

// view returns a copy of every entry's aircraft with PosAge rebased to
// nowMs.
func (s *store) view(nowMs int64, fn func(e *remoteEntry, a publicdto.Aircraft)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.m {
		if nowMs-e.recvMs > s.ttlMs {
			continue
		}
		a := e.a
		if a.Lat != nil && e.posMs > 0 {
			age := float64(max(nowMs-e.posMs, 0)) / 1000
			a.PosAge = &age
		}
		fn(e, a)
	}
}

// changed calls fn for every live entry updated after seq (all when seq is
// 0) and returns the current sequence.
func (s *store) changed(seq uint64, nowMs int64, fn func(e *remoteEntry, a publicdto.Aircraft)) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.m {
		if e.seq <= seq || nowMs-e.recvMs > s.ttlMs {
			continue
		}
		a := e.a
		if a.Lat != nil && e.posMs > 0 {
			age := float64(max(nowMs-e.posMs, 0)) / 1000
			a.PosAge = &age
		}
		fn(e, a)
	}
	return s.seq
}
