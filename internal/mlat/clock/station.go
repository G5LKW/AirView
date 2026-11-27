package clock

import (
	"math"

	"airview/internal/model"
)

// stateKind identifies one clock state of a station block.
type stateKind uint8

const (
	kTheta stateKind = iota // θ offset (ns); δ for anchors
	kPhi                    // φ fractional frequency (ppb = ns/s)
	kPsi                    // ψ frequency rate (ppb/s)
	kBeta                   // β_short (ns)
	numKinds
)

// mode is the clock model of a station.
type mode uint8

const (
	modeFree   mode = iota // θ, φ, [ψ], β
	modeAnchor             // δ (= θ), β; φ ≡ 0
)

// Anchor states (StationClock.AnchorState).
const (
	AnchorNone         = "none"          // not anchor-capable
	AnchorProbation    = "probation"     // GPS class, free clock, being evaluated
	AnchorActive       = "anchor"        // anchor
	AnchorSelfAsserted = "self_asserted" // first anchor of a component, admitted on its status bits alone
	AnchorDemoted      = "demoted"       // was an anchor, now a class-3 free clock
	AnchorFlagged      = "flagged"       // admin flag: never an anchor
)

// accRing holds per-epoch accepted-observation counts (≥ SyncedWindowS).
const accRing = 128

type accSlot struct {
	k int64
	n uint32
}

// EventRecord is one clock event of a station (snapshot stats).
type EventRecord struct {
	T      float64 // network seconds since the origin
	Reason string  // a model.ClockEventReason, or "step", "promoted", "demoted", "leap", ...
	Detail string
}

const maxEvents = 16

// streakCap is the number of streak members kept for the step estimate.
const streakCap = 16

// driftPt is one 60-s bin of an anchor's apparent offset.
type driftPt struct{ t, v float64 }

type station struct {
	idx   model.StationIdx
	info  StationInfo
	cls   model.ClockClass // effective class (downgraded after demotion)
	prior classPrior
	known bool

	// Epoch mapping: local seconds y = (ticks−baseTicks)/hz + baseS
	// − FrameRef(kind) (seconds since the origin).
	epoch, prevEpoch uint32
	hasEpoch         bool
	baseTicks        uint64
	baseS            float64
	hz               float64
	evEpoch          uint32 // epoch announced by an Event
	evReason         model.ClockEventReason
	evValid          bool

	// Layout in the component.
	mode   mode
	psi    bool
	comp   *component
	off    int
	layout [numKinds]int8 // position of each kind in the block, -1 = absent
	dim    int
	fresh  bool    // needs a new block (prior) at the next relayout
	initTh float64 // θ mean for a fresh block
	rejoin bool    // came from another time domain: re-initialise variances

	// Timing.
	lastObsS float64
	lastAccS float64
	acc      [accRing]accSlot

	// Integrity.
	streakSign int8
	streakN    int
	streakSum  float64 // sum of the streak's leave-one-out residuals (ns): the step estimate
	// streakT / streakR are the arrival times (s) and leave-one-out
	// residuals (ns) of the latest streak members (ring of streakCap): a
	// step is told apart from a frequency error by the slope of the
	// residuals (postEpoch, stepEstimate).
	streakT, streakR [streakCap]float64
	suspect          int
	outEWMA          float64
	nAcc, nOut       uint64
	nSteps           int
	// run of same-sign steps (StepRunCount)
	stepRunSign int8
	stepRunN    int
	stepRunT0   float64

	// Adaptation.
	qScale, jitScale float64
	adStart          float64
	adDx2, adDrop    float64
	adN              int
	adW2             float64
	adWN             int

	// Per-epoch influence and bookkeeping.
	influ     float64 // effective observation count of the epoch
	capInfl   float64 // influence-cap variance inflation (≥ 1) for the next epoch
	rate      float64 // accepted observations per second (SyncedWindowS average)
	epAcc     int
	pre       [numKinds]float64 // P⁻ diagonal of the epoch (adaptation)
	preX      [numKinds]float64
	looSum    float64 // sum of leave-one-out residuals this drift bin (ns)
	looN      int
	driftBin0 float64

	// GPS and anchors.
	gpsKnown   bool
	gpsOK      bool
	gpsUTC     bool
	gpsOKSince float64
	// leapApplied: a whole-second offset (GPS time base) was applied to the
	// station's clock; its class became UTC although the receiver keeps
	// reporting UTC=false (finding MLAT-R1).
	leapApplied  bool
	freeSince    float64
	selfAsserted bool
	anchorSince  float64
	demoted      bool
	demoteReq    string
	// demotedTransient: the demotion came from a GPS status loss or a
	// receiver epoch event; once condition (a) holds again the station
	// re-enters probation (finding ROB-5). Drift, revalidation, step and
	// flag demotions stay permanent.
	demotedTransient bool
	// hostDevS is the EWMA of (station time − server corrected time) of
	// the GPS station's sync members (s), over hostDevN samples: the
	// check that a GPS clock tells the true time to within seconds
	// (finding ROB-1).
	hostDevS     float64
	hostDevN     int
	anchorReason string
	drift        []driftPt

	events []EventRecord
	peers  map[model.StationIdx]*peerAcc
}

// peerAcc are the sync statistics of a station pair (member versus the
// group's reference member).
type peerAcc struct {
	n      uint32  // accepted co-observations in the current 30 s window
	last   uint32  // count of the previous window (for the rate)
	sq     float64 // EWMA of the squared pair residual, ns²
	win    int64   // window index
	phiRel float64 // relative frequency (ppb), refreshed at snapshot time
}

func newStation(idx model.StationIdx) *station {
	s := &station{idx: idx, qScale: 1, jitScale: 1, capInfl: 1, peers: map[model.StationIdx]*peerAcc{}}
	for i := range s.layout {
		s.layout[i] = -1
	}
	return s
}

// setInfo applies a station description. A class change starts the clock
// model over (the feeder also starts a new epoch on a class change).
func (s *station) setInfo(in StationInfo) (classChanged bool) {
	old := s.info.Class
	s.info = in
	if !s.known || in.Class != old {
		s.known = true
		s.cls = in.Class
		s.demoted = false
		s.leapApplied = false
		s.prior = priorFor(s.cls)
		s.hz = s.cls.NominalHz()
		s.psi = s.prior.psi
		s.mode = modeFree
		return true
	}
	return false
}

// usable reports whether the station can take part in sync.
func (s *station) usable() bool {
	return s.known && s.info.Located && s.cls.Timed()
}

// jitter returns the current 1-sigma timestamp jitter (ns).
func (s *station) jitter() float64 {
	j := s.prior.jitter
	if s.info.JitterNs > 0 {
		j = s.info.JitterNs
	}
	return j * s.jitScale
}

// anchorCapable reports whether the station may be considered for anchor
// promotion now (classes 1/2, not demoted, not flagged).
func (s *station) anchorCapable() bool {
	return (s.cls == model.ClockGPSUTC || s.cls == model.ClockGPSGPST) && !s.demoted && !s.info.Flagged
}

// wantLayout computes the block layout for the station's mode.
func (s *station) wantLayout() (lay [numKinds]int8, dim int) {
	for i := range lay {
		lay[i] = -1
	}
	lay[kTheta] = 0
	dim = 1
	if s.mode == modeFree {
		lay[kPhi] = int8(dim)
		dim++
		if s.psi {
			lay[kPsi] = int8(dim)
			dim++
		}
	}
	lay[kBeta] = int8(dim)
	dim++
	return
}

// localS converts ticks of the current epoch to local seconds since the
// origin, minus the frame reference refNs.
func (s *station) localS(ticks uint64, refNs float64) float64 {
	return float64(int64(ticks-s.baseTicks))/s.hz + s.baseS - refNs*1e-9
}

// addEvent records an event (ring of maxEvents).
func (s *station) addEvent(t float64, reason, detail string) {
	if len(s.events) >= maxEvents {
		copy(s.events, s.events[1:])
		s.events = s.events[:maxEvents-1]
	}
	s.events = append(s.events, EventRecord{T: t, Reason: reason, Detail: detail})
}

// countAcc adds accepted observations to the epoch ring.
func (s *station) countAcc(k int64, n int) {
	sl := &s.acc[k%accRing]
	if sl.k != k {
		sl.k, sl.n = k, 0
	}
	sl.n += uint32(n)
}

// accIn returns the accepted observations in the last w epochs up to k.
func (s *station) accIn(k int64, w int64) int {
	if w > accRing {
		w = accRing
	}
	n := 0
	for i := int64(0); i < w; i++ {
		kk := k - i
		if kk < 0 {
			break
		}
		if sl := s.acc[kk%accRing]; sl.k == kk {
			n += int(sl.n)
		}
	}
	return n
}

// procVar returns the process-noise variance of θ accumulated over |dt|
// seconds (ns²): the variance of θ(t+dt) given the state at t.
func (s *station) procVar(dt float64) float64 {
	dt = math.Abs(dt)
	if s.mode == modeAnchor {
		return s.qScale * qDelta * dt
	}
	q := s.qScale
	v := q * (s.prior.qTheta*dt + s.prior.qPhi*dt*dt*dt/3)
	if s.psi {
		v += q * s.prior.qPsi * dt * dt * dt * dt * dt / 20
	}
	return v
}

// noteHost adds one sample of (station time − server time), in seconds.
// The first samples are averaged, later ones enter an EWMA (α = 0.05);
// a sample more than 2 s from an established mean is clipped, so a rare
// grouping or correction glitch cannot move it far.
func (s *station) noteHost(d float64) {
	if math.IsNaN(d) || math.IsInf(d, 0) {
		return
	}
	if s.hostDevN >= 20 {
		d = math.Max(s.hostDevS-2, math.Min(s.hostDevS+2, d))
	}
	s.hostDevN++
	a := math.Max(1/float64(s.hostDevN), 0.05)
	s.hostDevS += a * (d - s.hostDevS)
}
