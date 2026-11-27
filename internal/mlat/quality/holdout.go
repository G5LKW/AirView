package quality

import (
	"sync"
	"time"
)

// HoldOut selects the self-validation set (DESIGN §9.6): a fraction of the
// ADS-B aircraft with NACp >= MinNACp, chosen by a keyed hash of the
// address and rotated every Rotate. Members are excluded from clock sync
// (the engine hides them from the reference source), solved as Mode S-only
// aircraft and compared with their reference positions.
//
// Selection is a pure function of (address, rotation index, salt), so
// every component that asks gets the same answer without coordination, and
// a restart with the same salt keeps the set. The NACp condition uses the
// latest announced NACp (NoteNACp), so an aircraft enters the set only once
// it has announced a good one, and leaves it when the last announcement is
// older than nacpTTL.
type HoldOut struct {
	fraction float64
	rotate   int64
	salt     uint64
	minNACp  uint8
	thresh   uint64 // fraction scaled to 2^64

	sh [64]holdShard
}

type holdShard struct {
	mu sync.Mutex
	m  map[uint32]nacpRec
}

type nacpRec struct {
	nacp uint8
	at   int64
}

// nacpTTL: a NACp announcement counts for this long (operational status
// messages come every 2.5-5 s).
const nacpTTL = int64(60e9)

// NewHoldOut returns a selector. fraction is clamped to 0..0.5 (0 = no
// hold-out), rotate defaults to 10 minutes, minNACp to 9.
func NewHoldOut(fraction float64, rotate time.Duration, salt uint64, minNACp uint8) *HoldOut {
	fraction = min(max(fraction, 0), 0.5)
	if rotate <= 0 {
		rotate = 10 * time.Minute
	}
	if minNACp == 0 {
		minNACp = 9
	}
	h := &HoldOut{fraction: fraction, rotate: int64(rotate), salt: salt, minNACp: minNACp}
	if fraction > 0 {
		h.thresh = uint64(fraction * (1 << 63) * 2)
	}
	for i := range h.sh {
		h.sh[i].m = map[uint32]nacpRec{}
	}
	return h
}

// Fraction returns the configured fraction.
func (h *HoldOut) Fraction() float64 { return h.fraction }

func mix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

// Selected reports whether the address hash falls into the hold-out set of
// the rotation period containing nowNs (no NACp condition).
func (h *HoldOut) Selected(key uint32, nowNs int64) bool {
	if h == nil || h.thresh == 0 {
		return false
	}
	r := uint64(nowNs / h.rotate)
	return mix64(uint64(key)^h.salt^mix64(r)) < h.thresh
}

// NoteNACp records an announced NACp of an aircraft (TC 29 / 31).
func (h *HoldOut) NoteNACp(key uint32, nacp uint8, nowNs int64) {
	if h == nil || h.thresh == 0 {
		return
	}
	s := &h.sh[key&63]
	s.mu.Lock()
	s.m[key] = nacpRec{nacp: nacp, at: nowNs}
	s.mu.Unlock()
}

// In reports whether aircraft key is in the hold-out set at nowNs: hash
// selected and a NACp >= MinNACp announced within the last minute. Safe for
// concurrent use; it does not allocate.
func (h *HoldOut) In(key uint32, nowNs int64) bool {
	if !h.Selected(key, nowNs) {
		return false
	}
	s := &h.sh[key&63]
	s.mu.Lock()
	r, ok := s.m[key]
	s.mu.Unlock()
	return ok && r.nacp >= h.minNACp && nowNs-r.at <= nacpTTL
}

// Expire forgets NACp records older than beforeNs.
func (h *HoldOut) Expire(beforeNs int64) {
	if h == nil {
		return
	}
	for i := range h.sh {
		s := &h.sh[i]
		s.mu.Lock()
		for k, r := range s.m {
			if r.at < beforeNs {
				delete(s.m, k)
			}
		}
		s.mu.Unlock()
	}
}
