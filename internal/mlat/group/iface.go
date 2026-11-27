package group

import (
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Clock converts receiver timestamps to network time. It is the read side of
// the clock filter snapshot (internal/mlat/clock, DESIGN §9.2.3); the engine
// adapts the snapshot to it. Implementations must be fast (one call per
// copy) and safe for use from the assembler's goroutine while the filter
// publishes new snapshots.
type Clock interface {
	// NetTime converts station st's timestamp (clock epoch, unwrapped ticks
	// in units of the station class's NominalHz) of a frame of the given kind
	// to the network time at which the leading edge of the preamble reached
	// the antenna. The clock applies FrameRef(class, kind) and β_short.
	//
	// clockVar is the marginal variance of the clock-state part of the
	// conversion (θ, φ, ψ, β_short at that instant) and jitterVar the
	// station's per-frame timestamp jitter variance, both in ns². ok is false
	// when the station is not synced (DESIGN §9.2.4) or the epoch is
	// unknown.
	NetTime(st model.StationIdx, epoch uint32, ticks uint64, kind model.FrameKind) (t Time, clockVar, jitterVar float64, ok bool)
}

// CrossCov is optionally implemented by a Clock: the covariance (ns²) of the
// clock-state errors of two synced stations (0 when unknown). Positive
// covariance between two stations of the same component makes their
// difference more precise than the marginals suggest; the gate uses
// var_i + var_k − 2·cov_ik.
type CrossCov interface {
	CrossCov(a, b model.StationIdx) float64
}

// Domains is optionally implemented by a Clock: the time domain (clock
// filter component) of a synced station. Network times of stations in
// different domains are not comparable (DESIGN §9.2.1 gauge: a component
// without anchors has its own relative time), so copies of different
// domains are never put in one cluster.
type Domains interface {
	Domain(st model.StationIdx) uint64
}

// Sites returns the MLAT antenna positions of stations.
type Sites interface {
	// Site returns the antenna position (ECEF, metres, height above the
	// ellipsoid) and its 1-sigma uncertainty (metres) used for MLAT. ok is
	// false for a station without a position usable for MLAT (DESIGN §10.1).
	Site(st model.StationIdx) (pos geo.Vec3, sigmaM float64, ok bool)
}

// Hints optionally supplies prior knowledge about an aircraft from the MLAT
// tracker or the ADS-B reference track. A hint makes the clustering gate
// far tighter than the baseline bound (see the package documentation).
type Hints interface {
	// Predict returns the aircraft's predicted position (ECEF) at network
	// time at and its isotropic 1-sigma uncertainty in metres.
	Predict(key uint32, at Time) (pos geo.Vec3, sigmaM float64, ok bool)
	// AltitudeFt returns a recent pressure altitude (ft) of the aircraft
	// when the frame itself carries none.
	AltitudeFt(key uint32, at Time) (ft float64, ok bool)
}

// Selector decides which aircraft are MLAT candidates and how often they are
// solved (DESIGN §9.3 "Candidates", the planner's budgets). internal/mlat/plan
// implements it.
type Selector interface {
	// Candidate reports whether frames of the aircraft (tracker key, downlink
	// format) are MLAT candidates at server time nowNs. Groups of other
	// aircraft are not clustered at all.
	Candidate(key uint32, df uint8, nowNs int64) bool
	// Admit is asked once per cluster that could be solved (sites >= 3) and
	// returns false when the aircraft's solve rate or the global solve
	// budget is exhausted. A cluster that is not admitted is downgraded to
	// UseTrack: the tracker may still use its TDOAs.
	Admit(key uint32, sites int, nowNs int64) bool
}

// Sink receives clusters. The *Cluster and its slices are valid only during
// the call; use Clone to keep one.
type Sink interface {
	Cluster(c *Cluster)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(c *Cluster)

// Cluster implements Sink.
func (f SinkFunc) Cluster(c *Cluster) { f(c) }
