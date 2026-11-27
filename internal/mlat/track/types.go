package track

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
	"airview/internal/model"
)

// TOA is one station's time of arrival of a transmission, already converted
// to network time by the clock snapshot (DESIGN §9.3 step 1).
type TOA struct {
	// Station is the engine's station key. It identifies the station in the
	// refraction cache and must be stable for the station's lifetime.
	Station uint32
	// Pos is the antenna position, ECEF metres (height above the ellipsoid).
	Pos geo.Vec3
	// T is the network time of arrival in seconds relative to Cluster.T0Ns.
	T float64
	// Sigma is the 1σ uncertainty of T in seconds that is independent from
	// one transmission to the next (timestamp jitter, quantisation); used
	// when Cluster.Cov is nil. Must be > 0.
	Sigma float64
	// SyncSigma is the 1σ of the slowly varying clock-sync error of the
	// station (the clock filter's marginal σ_θ), in seconds. It is
	// correlated over Params.SyncTauS, so the tracker does not let repeated
	// use of the same station average it away (doc.go §5.3). 0 = none.
	SyncSigma float64
}

// Cluster is one transmission heard by m >= 2 stations (§9.5: "raw TOA
// clusters with m ≥ 2").
type Cluster struct {
	ICAO uint32
	// T0Ns is the network-time epoch (Unix ns) the TOA offsets refer to.
	T0Ns int64
	TOAs []TOA
	// Cov is optionally the full len(TOAs)×len(TOAs) covariance of the
	// per-transmission TOA errors (s², row-major), e.g. diag(jitter²) plus
	// the instantaneous cross-covariance of the clock snapshot (§9.4
	// weighting). nil = diag(Sigma²). SyncSigma is added on top.
	Cov []float64
	// AltHAE is the aircraft's height above the ellipsoid (Mode S altitude
	// through the GNSS−baro field) with 1σ AltSigma; AltSigma <= 0 = none.
	AltHAE, AltSigma float64
}

// FixHyp is one position hypothesis of a fix.
type FixHyp struct {
	ECEF geo.Vec3
	// Cov is the 3×3 ECEF position covariance (m², row-major).
	Cov [9]float64
}

// Fix is a snapshot solution from the solver (mlat/solve). A 3+alt solution
// can carry two hypotheses (the two intersections of the hyperboloids with
// the altitude surface); the tracker resolves them by gating (§9.5).
type Fix struct {
	ICAO uint32
	TNs  int64 // transmit instant, network Unix ns
	Hyp  [2]FixHyp
	NHyp int // 1 or 2
	// NStations and DOF describe the solution (for output buckets and
	// grades).
	NStations, DOF int
}

// FixFromMLAT converts a solver result in the model contract into a
// single-hypothesis Fix.
func FixFromMLAT(f *model.MLATFix, nStations, dof int) Fix {
	c := f.Cov
	cov := [9]float64{
		float64(c[0]), float64(c[1]), float64(c[2]),
		float64(c[1]), float64(c[3]), float64(c[4]),
		float64(c[2]), float64(c[4]), float64(c[5]),
	}
	return Fix{ICAO: f.ICAO, TNs: f.TUnixNs, NHyp: 1, NStations: nStations, DOF: dof,
		Hyp: [2]FixHyp{{ECEF: geo.Vec3{X: f.ECEF[0], Y: f.ECEF[1], Z: f.ECEF[2]}, Cov: cov}}}
}

// Velocity is a velocity measurement in the local ENU frame at the aircraft:
// DF17 TC19 ground velocity, or BDS 5,0 track/ground speed (and BDS 6,0 with
// met wind) with suitably inflated σ.
type Velocity struct {
	ICAO        uint32
	TNs         int64
	East, North float64 // m/s
	SigmaH      float64 // 1σ per horizontal axis, m/s (> 0)
	// Up is the vertical rate (m/s) when SigmaUp > 0.
	Up, SigmaUp float64
}

// Altitude is a Mode S altitude reply converted to height above the
// ellipsoid.
type Altitude struct {
	ICAO  uint32
	TNs   int64
	HAE   float64
	Sigma float64 // 1σ, m (> 0)
}

// Status is the lifecycle state of a track.
type Status uint8

// Track states.
const (
	StatusNone      Status = iota
	StatusTentative        // created, not yet confirmed (never output to the map)
	StatusConfirmed        // confirmed (§9.5 initiation rule met)
)

func (s Status) String() string {
	switch s {
	case StatusTentative:
		return "tentative"
	case StatusConfirmed:
		return "confirmed"
	}
	return "none"
}

// Result tells what a measurement did.
type Result uint8

// Results.
const (
	ResIgnored      Result = iota // invalid input
	ResNoTrack                    // no track for the ICAO and the measurement cannot start one
	ResCreated                    // a fix started a new (tentative) track or hypothesis set
	ResApplied                    // the measurement updated at least one hypothesis
	ResGated                      // the measurement failed the gate of every hypothesis
	ResTooLate                    // older than the track by more than MaxLagS
	ResNotConfirmed               // a 2-station cluster for a track that is not confirmed
)

var resNames = [...]string{"ignored", "no-track", "created", "applied", "gated", "too-late", "not-confirmed"}

func (r Result) String() string {
	if int(r) < len(resNames) {
		return resNames[r]
	}
	return "?"
}

// Estimate is the tracker's output for one aircraft: the best hypothesis'
// IMM-combined state.
type Estimate struct {
	ICAO uint32
	TNs  int64 // network Unix ns of the state
	// ECEF position and its 3×3 covariance (m², row-major).
	ECEF geo.Vec3
	Cov  [9]float64
	LLH  geo.LLH
	// VelENU is the velocity in the local ENU frame at the aircraft (m/s)
	// and VelCov its covariance (m²/s², row-major).
	VelENU [3]float64
	VelCov [9]float64
	// SigmaH is the horizontal 1σ (sqrt of the trace of the horizontal
	// covariance, m), SigmaV the vertical 1σ (m).
	SigmaH, SigmaV float64
	// Ellipse is the horizontal 1σ error ellipse.
	Ellipse linalg.Ellipse
	// TurnRate is the IMM-combined turn rate (rad/s, positive = left /
	// counter-clockwise seen from above), PTurn the CT mode probability.
	TurnRate, PTurn float64
	Status          Status
	// Hypotheses is the number of hypotheses the ICAO still has; Ambiguous
	// is set while more than one is alive.
	Hypotheses int
	Ambiguous  bool
	// Updates counts accepted measurements of the hypothesis; LastStations
	// / LastDOF describe the most recent position-bearing update, MaxDOF the
	// largest degrees of freedom among the position-bearing updates of the
	// last 10 s; AltUsed tells whether that update had an altitude row.
	Updates      uint32
	LastStations int
	LastDOF      int
	MaxDOF       int
	AltUsed      bool
	// LastUpdateNs is when the hypothesis last accepted a measurement.
	LastUpdateNs int64

	velF32 [3]float32
}

// Speed returns the horizontal ground speed (m/s).
func (e *Estimate) Speed() float64 { return math.Hypot(e.VelENU[0], e.VelENU[1]) }

// MLATFix converts the estimate into the model's result type with
// FixFlagTracked set. Vel points into e: copy the result's Vel before e is
// reused.
func (e *Estimate) MLATFix() model.MLATFix {
	f := model.MLATFix{ICAO: e.ICAO, TUnixNs: e.TNs, ECEF: [3]float64{e.ECEF.X, e.ECEF.Y, e.ECEF.Z}}
	c := &e.Cov
	f.Cov = [6]float32{float32(c[0]), float32(c[1]), float32(c[2]), float32(c[4]), float32(c[5]), float32(c[8])}
	e.velF32 = [3]float32{float32(e.VelENU[0]), float32(e.VelENU[1]), float32(e.VelENU[2])}
	f.Vel = &e.velF32
	f.NBucket = model.NBucketFor(e.LastStations)
	f.Grade = model.GradeFor(e.SigmaH, e.MaxDOF)
	f.Flags = model.FixFlagTracked
	if e.AltUsed {
		f.Flags |= model.FixFlagAltConstrained
	}
	if e.Ambiguous {
		f.Flags |= model.FixFlagAmbiguous
	}
	return f
}

// Stats are tracker counters.
type Stats struct {
	Tracks        int // ICAOs with at least one hypothesis
	Confirmed     int // ICAOs whose best hypothesis is confirmed
	Hypotheses    int
	Clusters      uint64
	Fixes         uint64
	Applied       uint64 // measurements applied to at least one hypothesis
	Gated         uint64 // measurements rejected by every hypothesis
	Excluded      uint64 // cluster members excluded as outliers
	OOSM          uint64 // out-of-sequence measurements applied
	TooLate       uint64
	Created       uint64 // hypotheses created
	Confirmations uint64
	Pruned        uint64 // hypotheses dropped by the likelihood ratio
	Deleted       uint64 // tracks deleted (coast, σ, confirm timeout)
	Respawns      uint64 // hypotheses started from repeatedly gated fixes
}
