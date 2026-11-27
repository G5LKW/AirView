package plan

import (
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Prio is an MLAT aircraft's priority class (lower is more important).
type Prio uint8

// Priority classes (DESIGN §9.3 candidates; non-ADS-B aircraft first).
const (
	// PrioAcquire: no good ADS-B position and no MLAT track yet.
	PrioAcquire Prio = iota
	// PrioTracked: no good ADS-B position, MLAT track established.
	PrioTracked
	// PrioHoldOut: ADS-B aircraft in the quality hold-out set (§9.6).
	PrioHoldOut
	// PrioSuspect: ADS-B aircraft whose positions failed TDOA verification.
	PrioSuspect
	// NumPrio is the number of classes.
	NumPrio
	// PrioNone: not an MLAT candidate (good ADS-B).
	PrioNone Prio = 255
)

// String returns "acquire", "tracked", "holdout", "suspect" or "none".
func (p Prio) String() string {
	switch p {
	case PrioAcquire:
		return "acquire"
	case PrioTracked:
		return "tracked"
	case PrioHoldOut:
		return "holdout"
	case PrioSuspect:
		return "suspect"
	}
	return "none"
}

// StationInfo is what the planner needs to know about a station.
type StationInfo struct {
	// Pos is the MLAT antenna position (ECEF) when Located.
	Pos     geo.Vec3
	Located bool
	// Synced / Anchor: clock state (DESIGN §9.2.4, §9.2.5).
	Synced bool
	Anchor bool
	// SigmaNs is the station's current timing 1-sigma (clock + jitter) used
	// to weight its geometry; 0 = the class prior.
	SigmaNs float64
	Class   model.ClockClass
}

// Stations gives the planner the stations' state (registry positions and the
// clock snapshot). It is called during Replan only.
type Stations interface {
	Station(st model.StationIdx) (StationInfo, bool)
}

// Positions optionally gives an aircraft's current position estimate (MLAT
// track or ADS-B reference) and its 1-sigma (m).
type Positions interface {
	Approx(key uint32) (pos geo.Vec3, sigmaM float64, ok bool)
}

// Delta is an interest-set change for one station. Slices are valid only
// during the Out call. Send* is the change of the union of both sets (what an
// mlat-client is asked to send: start_sending / stop_sending).
type Delta struct {
	Reset            bool
	SyncAdd, SyncDel []uint32
	MLATAdd, MLATDel []uint32
	SendAdd, SendDel []uint32
}

// Empty reports whether the delta changes nothing.
func (d *Delta) Empty() bool {
	return !d.Reset && len(d.SyncAdd)+len(d.SyncDel)+len(d.MLATAdd)+len(d.MLATDel) == 0
}

// Out receives the planner's interest-set changes (AVF INTEREST through the
// ingest, mlat-client start/stop_sending). Calls come from the Replan
// goroutine.
type Out interface {
	Interest(st model.StationIdx, d *Delta)
}

// OutFunc adapts a function to Out.
type OutFunc func(st model.StationIdx, d *Delta)

// Interest implements Out.
func (f OutFunc) Interest(st model.StationIdx, d *Delta) { f(st, d) }
