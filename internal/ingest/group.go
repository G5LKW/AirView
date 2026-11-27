package ingest

import (
	"time"

	"airview/internal/model"
	"airview/internal/tracker"
)

// Copy is one station's copy of a transmission inside a Group.
type Copy struct {
	// Station heard the transmission on Channel (-1 = unknown).
	Station model.StationIdx
	Channel int8
	// ClockCls, Epoch and Ticks are the receiver timestamp (Ticks in units of
	// ClockCls.NominalHz(), only meaningful when Timed).
	ClockCls model.ClockClass
	Epoch    uint32
	Ticks    uint64
	// HostNs is the feeder's host time of reception (Unix ns, uncorrected).
	HostNs int64
	// CorrNs is the corrected time in the server clock domain (Unix ns): the
	// grouper's estimate of when the transmission reached the antenna.
	CorrNs int64
	// Signal is the RSSI in dBFS (NaN = unknown).
	Signal float32
	// Timed is set when the copy may be used for multilateration: a timed
	// clock class with valid ticks, from an approved station whose owner
	// allows timestamps, and not a Beast 30004 (MapStatsOnly) copy.
	Timed bool
	// MapOnly marks a copy from a MapStatsOnly source (Beast 30004): never
	// for MLAT, clock sync or survey (DESIGN §5.1).
	MapOnly bool
	// SameStation marks a second copy of the same transmission from a station
	// that already has one in the group (the other channel of a diversity
	// receiver, DESIGN §6.2). It is credited to that channel's lane but must
	// not be used as a second MLAT observation.
	SameStation bool

	pend bool  // lane credit owed until the first copy's result is ready
	raw  int64 // CorrNs without the learned latency (estimator input)
	recv int64 // server time the copy arrived (FlushPolicy delivery latency)
}

// Group is one transmission as heard by every station that delivered a copy
// within the grouping window. A *Group handed to an MLATSink is valid only
// during the call; use Clone to keep it.
type Group struct {
	// Hash is the 64-bit payload hash; Data the payload (2, 7 or 14 bytes).
	Hash uint64
	Data []byte
	Kind model.FrameKind
	// Key is the aircraft address key (24-bit address, non-ICAO flag as in
	// tracker.Result.Key) and DF the downlink format: from the tracker's
	// result when it was ready, else from the frame (tracker.RouteKey).
	Key uint32
	DF  uint8
	// Result is the tracker's outcome for the first copy (valid only when
	// HasResult).
	Result    tracker.Result
	HasResult bool
	// FirstNs is the corrected time of the earliest copy (server clock, Unix ns).
	FirstNs int64
	// Copies are the copies in arrival order (at most MaxCopies). For a late
	// delivery (MLATSink.ObserveLate) only the copies that arrived after the
	// group was flushed.
	Copies []Copy
	// Timed is the number of distinct stations with a Timed copy in Copies.
	Timed int
	// Earlier is, for a late delivery, the number of copies delivered with
	// the original flush.
	Earlier int
	// Redelivery marks a group that was flushed earlier with a single timed
	// copy (a sync candidate heard by a synced station, ClockView) and later
	// got timed copies of other stations within the window: it is delivered
	// once more through Observe with every copy (not as a late delivery
	// with the new copies only, which is useless for clock sync; HOOKS
	// WP-M-engine -> internal/ingest item 2). Sinks that keep per-message
	// side effects (reference tracks, counters) should skip them for a
	// re-delivery: the frame was already seen with the first delivery.
	Redelivery bool
}

// Clone returns a deep copy of g that may be retained.
func (g *Group) Clone() *Group {
	c := *g
	c.Data = append([]byte(nil), g.Data...)
	c.Copies = append([]Copy(nil), g.Copies...)
	return &c
}

// MLATSink receives closed groups (DESIGN §6.2 "On close"). The MLAT engine
// implements it (internal/mlat, phase 2). Calls come from one goroutine (the
// ingest sweeper) in roughly chronological order and must not block for
// long; g is valid only during the call.
type MLATSink interface {
	// Observe receives a group when its flush time has come: when it has at
	// least two timed copies from different stations, or one timed copy that
	// is a sync candidate heard by a synced station (ClockView).
	Observe(g *Group)
}

// LateObserver is optionally implemented by an MLATSink: copies of an
// already flushed group that arrived before the end of the window are
// delivered once more, with only the new copies (DESIGN §9.3 "late copies
// still update the track").
type LateObserver interface {
	ObserveLate(g *Group)
}

// ClockView is the grouper's optional window into the MLAT clock filter
// (DESIGN §6.2 "Synced stations", §9.2). Implementations must be safe for
// concurrent use and fast (they are called per copy).
type ClockView interface {
	// NetTime converts a station's ticks to network time (ns) with its
	// 1-sigma uncertainty; ok is false when the station is not synced.
	NetTime(st model.StationIdx, epoch uint32, ticks uint64) (tNs, sigmaNs float64, ok bool)
	// BaselineNs returns the distance between two stations divided by c, in
	// ns (ok false when either station is not located).
	BaselineNs(a, b model.StationIdx) (float64, bool)
	// SyncCandidate reports whether a transmission of the aircraft key with
	// downlink format df is usable for clock sync (an aircraft with a good
	// reference track).
	SyncCandidate(key uint32, df uint8) bool
}

// ClockKindView is optionally implemented by a ClockView. The synced-station
// gate then compares network times of the frame's own kind (receivers stamp
// short and long frames at different points of the frame, DESIGN §4.3, and
// β_short applies to short frames only), and sigmaNs must include the
// receiver's per-frame timestamp jitter, not only the clock-state
// uncertainty (an RTL-SDR stamps with ~400 ns jitter against ~100 ns of
// clock σ; HOOKS WP-M-engine -> internal/ingest item 1).
type ClockKindView interface {
	NetTimeKind(st model.StationIdx, epoch uint32, ticks uint64, kind model.FrameKind) (tNs, sigmaNs float64, ok bool)
}

// FlushPolicy chooses when a group is handed to the MLAT sink (DESIGN §9.3
// adaptive flush). internal/mlat/group.FlushEstimator implements it: the
// p99 delivery latency of the stations that usually hear the aircraft,
// clamped to its bounds. Implementations must be safe for concurrent use
// and cheap: Delay is called once per new group (under a shard lock),
// ObserveDelay from the sweeper for the copies of a sample of the groups.
type FlushPolicy interface {
	// Delay is the flush delay after the first copy's corrected time for a
	// new group of aircraft key (24-bit address key as tracker.RouteKey).
	Delay(key uint32) time.Duration
	// ObserveDelay records one copy's delivery latency: the server time it
	// arrived minus its corrected antenna time (ns), at server time nowNs.
	ObserveDelay(st model.StationIdx, delayNs, nowNs int64)
}

// HearerNoter is optionally implemented by a FlushPolicy: the ingest tells
// it which stations heard an aircraft (the stations of the sampled groups),
// so the policy needs no other source of hearers.
type HearerNoter interface {
	NoteHearer(key uint32, st model.StationIdx, nowNs int64)
}

// ClockObserver receives the clock records of the stations (AVF
// CLOCK_EVENT / CLOCK_PAIR / CLOCK_STATUS, Beast epoch changes). The MLAT
// clock filter implements it. Calls may come from many goroutines.
type ClockObserver interface {
	ClockEvent(st model.StationIdx, ev ClockEvent)
	ClockPair(st model.StationIdx, p ClockPair)
	ClockStatus(st model.StationIdx, s ClockStatus)
}

// ClockEvent is a new receiver clock epoch.
type ClockEvent struct {
	OldEpoch, NewEpoch uint32
	Reason             model.ClockEventReason
	SamplesLost        int64
	HostNs             int64
	// MapOnly is set for Beast 30004 links (never for clock sync).
	MapOnly bool
}

// ClockPair is one (ticks, host UTC) sample of a station.
type ClockPair struct {
	Epoch        uint32
	Ticks        uint64
	HostUTCNs    int64
	HostErrEstNs uint64
}

// ClockStatus is a Radarcape type-'4' status of a station.
type ClockStatus struct {
	HostNs         int64
	Settings       uint8
	GPSStatus      uint8
	PPSOffsetTicks int64
	Raw            []byte // valid only during the call
}
