package acars

import (
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store sizes.
const (
	DefaultKeep = 2000
	MinKeep     = 100
	MaxKeep     = 20000
	// DupWindow: an identical message (same aircraft, label, message
	// number and text) seen again within this window is a duplicate, e.g.
	// the same transmission heard by two decoders.
	DupWindow = 30 * time.Second
	// maxLabels bounds the per-label counters (labels are 2 characters,
	// but nothing stops a broken sender from inventing them).
	maxLabels = 512
)

// Resolver finds the ICAO address of an aircraft in the live picture by its
// normalised registration (NormReg); "" when none matches.
type Resolver func(normReg string) string

// Store holds the newest messages in a ring. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	ring    []Message
	next    int // write index
	n       int // valid entries
	lastID  uint64
	resolve Resolver

	dups     map[uint64]int64 // content hash -> arrival ms
	dupPrune int64

	total, duplicates, parseErrors, notACARS, badCRC, dropped uint64
	bySource                                                  map[string]uint64
	byLabel                                                   map[string]uint64
	lastT                                                     int64
}

// NewStore returns a store keeping keep messages (0 = DefaultKeep).
func NewStore(keep int) *Store {
	if keep <= 0 {
		keep = DefaultKeep
	}
	keep = min(max(keep, MinKeep), MaxKeep)
	return &Store{
		ring:     make([]Message, keep),
		dups:     make(map[uint64]int64),
		bySource: make(map[string]uint64),
		byLabel:  make(map[string]uint64),
	}
}

// SetResolver installs the registration -> ICAO look-up (nil = none).
func (s *Store) SetResolver(r Resolver) {
	s.mu.Lock()
	s.resolve = r
	s.mu.Unlock()
}

// CountError records a rejected input line.
func (s *Store) CountError(err error) {
	s.mu.Lock()
	switch err {
	case ErrNotACARS:
		s.notACARS++
	case ErrBadMessage:
		s.badCRC++
	default:
		s.parseErrors++
	}
	s.mu.Unlock()
}

// CountDropped records messages dropped by the input rate limit.
func (s *Store) CountDropped(n int) {
	s.mu.Lock()
	s.dropped += uint64(n)
	s.mu.Unlock()
}

// Add stores m (assigning its ID) unless it duplicates a recent message.
// It reports whether m was stored. The resolver, when set, is called
// without the store lock.
func (s *Store) Add(m Message, now time.Time) bool {
	s.mu.Lock()
	res := s.resolve
	s.mu.Unlock()
	if m.ICAO == "" && m.Reg != "" && res != nil {
		if icao := res(NormReg(m.Reg)); icao != "" {
			m.ICAO, m.ICAOFrom = icao, "reg"
		}
	}
	h := contentHash(&m)
	nowMs := now.UnixMilli()

	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.dups[h]; ok && nowMs-t < DupWindow.Milliseconds() {
		s.duplicates++
		return false
	}
	s.dups[h] = nowMs
	if nowMs-s.dupPrune > DupWindow.Milliseconds() {
		for k, t := range s.dups {
			if nowMs-t >= DupWindow.Milliseconds() {
				delete(s.dups, k)
			}
		}
		s.dupPrune = nowMs
	}
	s.lastID++
	m.ID = s.lastID
	s.ring[s.next] = m
	s.next = (s.next + 1) % len(s.ring)
	if s.n < len(s.ring) {
		s.n++
	}
	s.total++
	s.bySource[m.Source]++
	if _, ok := s.byLabel[m.Label]; ok || len(s.byLabel) < maxLabels {
		s.byLabel[m.Label]++
	}
	s.lastT = max(s.lastT, m.T)
	return true
}

func contentHash(m *Message) uint64 {
	h := fnv.New64a()
	id := m.ICAO
	if id == "" {
		id = NormReg(m.Reg)
	}
	for _, p := range []string{id, m.Flight, m.Label, m.Sublbl, m.MsgNo, m.Text} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// Filter selects messages for List. Empty fields match everything.
type Filter struct {
	ICAO    string // exact, upper case
	Reg     string // compared with NormReg
	Flight  string // prefix, case-insensitive
	Label   string // exact
	Text    string // substring, case-insensitive
	SinceID uint64 // only messages with a larger ID
	Limit   int    // newest first; 0 = 100, at most MaxKeep
}

// List returns matching messages, newest first.
func (s *Store) List(f Filter) []Message {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	f.Limit = min(f.Limit, MaxKeep)
	f.ICAO = strings.ToUpper(f.ICAO)
	reg := NormReg(f.Reg)
	flight := strings.ToUpper(strings.TrimSpace(f.Flight))
	text := strings.ToLower(f.Text)

	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, 0, min(f.Limit, s.n))
	for i := 0; i < s.n && len(out) < f.Limit; i++ {
		m := &s.ring[(s.next-1-i+2*len(s.ring))%len(s.ring)]
		if m.ID <= f.SinceID {
			break // newest first: every older message has a smaller ID
		}
		if f.ICAO != "" && m.ICAO != f.ICAO {
			continue
		}
		if reg != "" && NormReg(m.Reg) != reg {
			continue
		}
		if flight != "" && !strings.HasPrefix(m.Flight, flight) {
			continue
		}
		if f.Label != "" && m.Label != f.Label {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(m.Text), text) {
			continue
		}
		out = append(out, *m)
	}
	return out
}

// LabelCount is one row of Stats.Labels.
type LabelCount struct {
	Label string `json:"label"`
	Name  string `json:"name,omitempty"`
	N     uint64 `json:"n"`
}

// Stats summarises the input.
type Stats struct {
	Kept        int               `json:"kept"`
	Capacity    int               `json:"capacity"`
	Total       uint64            `json:"total"`
	Duplicates  uint64            `json:"duplicates"`
	ParseErrors uint64            `json:"parseErrors"`
	NonACARS    uint64            `json:"nonAcars"` // VDL2 frames without ACARS content
	BadCRC      uint64            `json:"badCrc"`
	Dropped     uint64            `json:"dropped"` // over the input rate limit
	BySource    map[string]uint64 `json:"bySource"`
	Labels      []LabelCount      `json:"labels"` // most frequent first, at most 20
	LastT       int64             `json:"lastT,omitempty"`
	Aircraft    int               `json:"aircraft"` // distinct aircraft among the kept messages
	Linked      int               `json:"linked"`   // kept messages linked to an ICAO address
}

// Stats returns the counters.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		Kept: s.n, Capacity: len(s.ring), Total: s.total, Duplicates: s.duplicates,
		ParseErrors: s.parseErrors, NonACARS: s.notACARS, BadCRC: s.badCRC, Dropped: s.dropped,
		BySource: make(map[string]uint64, len(s.bySource)), LastT: s.lastT,
	}
	for k, v := range s.bySource {
		st.BySource[k] = v
	}
	for k, v := range s.byLabel {
		st.Labels = append(st.Labels, LabelCount{Label: k, Name: LabelName(k), N: v})
	}
	sort.Slice(st.Labels, func(i, j int) bool {
		if st.Labels[i].N != st.Labels[j].N {
			return st.Labels[i].N > st.Labels[j].N
		}
		return st.Labels[i].Label < st.Labels[j].Label
	})
	if len(st.Labels) > 20 {
		st.Labels = st.Labels[:20]
	}
	seen := make(map[string]struct{})
	for i := 0; i < s.n; i++ {
		m := &s.ring[i]
		id := m.ICAO
		if id != "" {
			st.Linked++
		} else {
			id = NormReg(m.Reg)
		}
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	st.Aircraft = len(seen)
	return st
}
