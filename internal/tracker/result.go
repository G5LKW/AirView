package tracker

import (
	"math"
	"sync/atomic"

	"airview/internal/model"
)

// Result is what Tracker.Handle learned from one unique transmission
// (docs/aggregator/DESIGN.md §6.2-6.4). The aggregator's grouper caches it
// with the group and passes it to Lanes.Credit for every later copy of the
// same transmission heard by other stations, so a copy costs a lane update
// instead of a second decode and never takes the tracker mutex.
//
// A Result is a plain value (no allocation); it holds one internal pointer
// (to the aircraft's lock-free heardBy set) and is safe to copy and to read
// from any goroutine. It is meaningful for about a second (the grouper
// window): the position and flight it reports are those current when the
// first copy was applied.
type Result struct {
	// TMs is the tracker data time (Unix ms) the transmission was applied at.
	TMs int64
	// Key is the aircraft address key (24-bit address | modes.NonICAOFlag);
	// 0 unless Status == ResultOK.
	Key uint32
	// FlightID is the id of the aircraft's open flight session (0 = none yet:
	// the aircraft is not established).
	FlightID int64
	// Lat / Lon is the position this transmission produced (Pos), else 0.
	Lat, Lon float64
	// AltFt is the aircraft's best altitude at TMs (NaN = unknown); set with Pos.
	AltFt float64
	// Cell is the heat-map cell (geo.HeatCell) of the position; set with Pos.
	Cell uint32
	// Msgs is the number of messages an update represents (0 for a raw
	// frame, >= 1 for a pre-decoded update).
	Msgs uint32
	// Status is the frame outcome (Result* constants).
	Status uint8
	// DF is the downlink format, DFIdx its statistics counter index
	// (modes.DFIndex).
	DF, DFIdx uint8
	// PosSrc is the store.Src* code of the position (with Pos).
	PosSrc uint8
	// Class is the aircraft's source class for the minute gauges
	// (class* codes: ADS-B, MLAT, TIS-B, Mode S, other).
	Class uint8
	// Pos is true when this transmission produced an accepted position.
	Pos bool
	// Surface is true for a surface position (with Pos).
	Surface bool
	// Shown is true when the aircraft is established (listed, owns flights);
	// only shown aircraft count in the per-minute aircraft gauges.
	Shown bool

	heard *heardBy // the aircraft's heardBy set (nil unless ResultOK)
}

// Frame outcomes (Result.Status). The first four values match the
// de-duplication cache codes.
const (
	// ResultNone: not a Mode S frame (Mode A/C, receiver status, bad length);
	// lanes ignore it.
	ResultNone uint8 = 0
	// ResultOK: accepted and applied to aircraft Key.
	ResultOK = resOK
	// ResultBadCRC: parity failure (or a repaired frame of an unknown aircraft).
	ResultBadCRC = resBad
	// ResultRejected: parity-address frame of an unknown or unconfirmed address.
	ResultRejected = resReject
	// ResultUnsupported: a downlink format the decoder does not handle.
	ResultUnsupported = resOther
	// ResultLate: a backfilled (late) frame; it went to the lanes only.
	ResultLate uint8 = 5
)

// Source classes of Result.Class (the minute gauges of stats_minute).
const (
	classNone uint8 = iota
	classADSB
	classMLAT
	classTISB
	classModeS
	classOther
)

// classOf maps a model.Src* label to a class code.
func classOf(label string) uint8 {
	switch label {
	case model.SrcADSB:
		return classADSB
	case model.SrcMLAT:
		return classMLAT
	case model.SrcTISB, model.SrcADSR:
		return classTISB
	case model.SrcModeS:
		return classModeS
	}
	return classOther
}

// ------------------------------------------------------------------ heardBy

// heardMax is the most stations an aircraft's heardBy set holds; heardTTLSec
// is how long a station stays in it after its last copy (DESIGN §6.4).
const (
	heardMax    = 16
	heardTTLSec = 60
)

// heardBy is the lock-free set of stations that heard an aircraft recently.
// Each slot packs (station << 32 | data-time second); 0 = empty. Writers are
// Tracker.Handle (under the tracker mutex) and Lanes.Credit (without it), so
// every access is atomic. A race between two writers can at worst store a
// station twice or drop one update; readers de-duplicate and a station's
// next copy (aircraft transmit several times a second) repairs the rest.
type heardBy struct {
	s [heardMax]atomic.Uint64
}

// mark records that station st heard the aircraft at data second sec. When
// the set is full the least recently heard station is replaced.
func (h *heardBy) mark(st model.StationIdx, sec int64) { h.markHint(st, sec, -1) }

// markHint is mark with a slot hint: the slot a previous call returned for
// the same station (-1 = none). With a correct hint a re-mark is one load and
// at most one compare-and-swap; a stale hint falls back to the scan. It
// returns the slot the station now occupies (-1 = not stored).
func (h *heardBy) markHint(st model.StationIdx, sec int64, hint int) int {
	if h == nil || sec <= 0 || sec > math.MaxUint32 {
		return -1
	}
	nv := uint64(st)<<32 | uint64(sec)
	if uint(hint) < heardMax {
		if v := h.s[hint].Load(); v != 0 && model.StationIdx(v>>32) == st {
			if uint32(v) < uint32(sec) {
				h.s[hint].CompareAndSwap(v, nv)
			}
			return hint
		}
	}
	// A station lives in its home slot unless that was taken.
	home := int(st) & (heardMax - 1)
	v := h.s[home].Load()
	if v != 0 && model.StationIdx(v>>32) == st {
		if uint32(v) < uint32(sec) {
			h.s[home].CompareAndSwap(v, nv)
		}
		return home
	}
	victim, vOld, oldest := -1, uint64(0), uint32(math.MaxUint32)
	if v == 0 || int64(uint32(v)) < sec-heardTTLSec {
		victim, vOld, oldest = home, v, 0 // empty or expired home slot: preferred
	}
	for i := range h.s {
		v := h.s[i].Load()
		ts := uint32(v)
		if v != 0 && model.StationIdx(v>>32) == st {
			if ts < uint32(sec) {
				h.s[i].CompareAndSwap(v, nv)
			}
			return i
		}
		if ts < oldest || victim < 0 {
			victim, vOld, oldest = i, v, ts
		}
	}
	if h.s[victim].CompareAndSwap(vOld, nv) {
		return victim
	}
	return -1
}

// list appends the stations heard within heardTTLSec of nowSec to dst
// (de-duplicated, unordered).
func (h *heardBy) list(nowSec int64, dst []model.StationIdx) []model.StationIdx {
	if h == nil {
		return dst
	}
	base := len(dst)
outer:
	for i := range h.s {
		v := h.s[i].Load()
		if v == 0 {
			continue
		}
		if d := nowSec - int64(uint32(v)); d > heardTTLSec || d < -heardTTLSec {
			continue
		}
		st := model.StationIdx(v >> 32)
		for _, o := range dst[base:] {
			if o == st {
				continue outer
			}
		}
		dst = append(dst, st)
	}
	return dst
}

// HeardBy returns the stations that heard the aircraft of r within the last
// 60 s of nowMs (data time). It is empty for results without an aircraft.
func (r *Result) HeardBy(nowMs int64, dst []model.StationIdx) []model.StationIdx {
	return r.heard.list(nowMs/1000, dst)
}
