package clock

import (
	"errors"
	"math"
	"runtime"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Config configures a Filter. Zero fields take the documented defaults
// (normalize). All durations are seconds of network time.
type Config struct {
	// OriginUnixNs is the network-time origin: every time the filter and its
	// snapshots use is "seconds since OriginUnixNs" (float64). Required;
	// use a round value near the server start, e.g.
	// time.Now().Truncate(time.Second).
	//
	// Resolution: a float64 of t seconds has an ulp of 2^(⌊log2 t⌋−52) s:
	// below 0.5 ns for the first 24 days, 0.93 ns from 48.5 days, 1.86 ns
	// from 97 days, 3.7 ns from 194 days. The TOA conversion the solver
	// uses (Snapshot.NetTimeUnix) works in split form (integer seconds plus
	// a small remainder) and keeps sub-picosecond resolution at any uptime,
	// and the filter itself works on a moving internal origin (ReoriginS).
	// Only the float64 values of the public API (Snapshot.T, NetTime,
	// Convert, Local, NetFromLocal) carry the rounding of t (rms ulp/√12:
	// 0.5 ns after 97 days, 1.1 ns after a year).
	OriginUnixNs int64

	// ReoriginS: when the filter-internal time passes this many seconds
	// the internal origin moves forward by whole seconds (default 86400,
	// at least 10). Inside the filter every time is a float64 relative to
	// that origin, so its resolution stays below 0.02 ns at any uptime
	// (doc.go "Internal time origin"); the published API keeps "seconds
	// since OriginUnixNs".
	ReoriginS float64

	// EpochS is the filter epoch (default 0.5 s, model.AggMLAT.EpochMs).
	EpochS float64

	// Atmosphere is the refraction model for the propagation delay (zero =
	// geo.DefaultAtmosphere()). Propagation, when set, replaces it: it must
	// return the propagation time from the transmitter ac to the receiver
	// rx in nanoseconds.
	Atmosphere  geo.Atmosphere
	Propagation func(rx, ac geo.Vec3) float64

	// FrameRefNs returns the frame reference offset (ns after the leading
	// edge of the preamble at which a receiver of the class stamps a frame
	// of the kind). nil = DefaultFrameRefNs. The residual short-versus-long
	// error is the β_short state.
	FrameRefNs func(cls model.ClockClass, kind model.FrameKind) float64

	// HostSigmaS is the 1-sigma error of server-side host times used to
	// initialise a new clock epoch (default 0.05 s: host latency of SDR
	// sample blocks plus NTP error).
	HostSigmaS float64
	// PPSCorrelatedNs is the part of a GPS-class station's timestamp error
	// that is common to all frames of one second (the 1PPS discipline
	// error, DESIGN §13.1: 15-45 ns). Treating it as white would count it
	// once per frame; the filter adds PPSCorrelatedNs² × (frames per
	// second) to such a member's variance instead (default 30; negative =
	// off).
	PPSCorrelatedNs float64
	// ResetThetaSigmaNs is the minimum σ(θ) after a detected step (default
	// 1e6 = 1 ms, DESIGN §9.2.4). The mean is moved by the step estimated
	// from the outliers that triggered the detection, and σ is at least half
	// that estimate.
	ResetThetaSigmaNs float64

	// MaxMembers caps the stations used from one sync group (default 32;
	// the most precisely known clocks are kept).
	MaxMembers int
	// SyncRatePerStation is the target number of sync groups per station
	// and second admitted by AddSync (default 6; 0 < x <= 100). A group is
	// admitted while any of its members is below the target.
	SyncRatePerStation float64
	// MaxObsAgeS drops sync groups older than this at their epoch (default 5).
	MaxObsAgeS float64
	// StationTimeoutS removes a station from its component after this long
	// without an accepted observation (default 120).
	StationTimeoutS float64
	// CoObsWindowS is the co-observation window that defines connected
	// components (default 60, DESIGN §9.2.3).
	CoObsWindowS float64

	// Anchor probation (DESIGN §9.2.5).
	ProbationS           float64 // default 600
	AnchorFreqMaxPPB     float64 // |φ| limit for promotion, default 2
	AnchorOffsetMaxNs    float64 // |θ − median anchor δ| limit, default 200
	AnchorDriftMaxNsPerH float64 // demotion: |dδ/dt|, default 100
	AnchorDriftWindowS   float64 // drift regression window, default 600
	LeapToleranceNs      float64 // k·1 s detection tolerance, default 1000
	// AnchorHostMaxS: a GPS station's clock must agree with the server's
	// corrected time of its sync groups (the earliest copy's antenna time,
	// in the server clock domain) to within this before it can become the
	// first or a self-asserted anchor, and a k·1 s leap is only applied
	// when it brings the station to within this of server time (default
	// 0.4 s; finding ROB-1: a lone fake-GPS feeder must not set network
	// time). AnchorHostMinObs is the number of samples needed (default 20).
	AnchorHostMaxS   float64
	AnchorHostMinObs int

	// Gauges (DESIGN §9.2.1).
	AnchorGaugeSigmaNs   float64 // mean(δ_anchor) = g, default 10
	RelGaugeThetaSigmaNs float64 // mean θ = const over the gauge clocks of a relative component, default 10
	RelGaugeFreqSigmaPPB float64 // mean φ = const, default 1
	// RelGaugeBetaSigmaNs: mean β_short = const over the gauge clocks of a
	// relative component without GPS-class clocks in its gauge set (default
	// 10; negative = no β gauge). Without
	// anchors the common mode of β_short is unobservable: every member of a
	// short-frame group gets the same shift, which the transmit time
	// absorbs.
	RelGaugeBetaSigmaNs float64
	GaugeRelaxS         float64 // time constant of g -> 0 (default 3600)
	// NoGaugeProjection disables the release of the old gauge's information
	// when the gauge set changes (component.projectGauge). For experiments
	// only: without it SDR-only networks pin joining stations to a wrong φ.
	NoGaugeProjection bool

	// Integrity (DESIGN §9.2.4).
	GateProb  float64 // NIS gate probability, default 0.999
	WTestMax  float64 // max |studentised innovation| of an accepted member, default 4.5
	StepSigma float64 // default 4
	StepCount int     // consecutive same-sign outliers that declare a step, default 5
	// StepRunCount same-sign steps of a free clock within StepRunWindowS
	// also reset its frequency (φ, ψ) to the prior variance: a run of
	// steps means θ is being chased by a wrong φ (default 3 in 60 s;
	// StepRunCount < 0 = off).
	StepRunCount    int
	StepRunWindowS  float64
	InfluenceCap    float64 // per-station information cap, × component median, default 3
	BlacklistS      float64 // per-aircraft sync blacklist, default 600
	BlacklistMinGrp int     // groups before an aircraft can be blacklisted, default 10
	BlacklistRate   float64 // failed-group rate that blacklists an aircraft, default 0.5
	// BlacklistMaxFrac caps the blacklisted share of the sync aircraft
	// (default 0.2): beyond it a fault is in the stations, not in the
	// aircraft, so no more aircraft are blacklisted and the filter counts
	// BlacklistCapped (finding ROB-2). StationBadOutlier: a member whose
	// outlier EWMA is at least this (default 0.25) does not count towards
	// an aircraft's failure.
	BlacklistMaxFrac  float64
	StationBadOutlier float64

	// Adaptive noise (covariance matching).
	AdaptWindowS     float64 // default 600
	QScaleMin        float64 // default 0.25
	QScaleMax        float64 // default 10
	NoJitterLearning bool    // disable the on-line jitter learning
	JitterScaleMin   float64 // default 0.5
	JitterScaleMax   float64 // default 4

	// Synced state (DESIGN §9.2.4): ≥ SyncedMinObs accepted observations in
	// the last SyncedWindowS and the class's σ_θ limit.
	SyncedMinObs  int     // default 5
	SyncedWindowS float64 // default 30

	// MaxComponentStations partitions a time domain (connected component)
	// with more stations than this into several jointly estimated
	// partitions (default 200; DESIGN §20.2, phase-5 seam, see partition.go).
	MaxComponentStations int
	// PartitionStations is the largest partition of a domain that is
	// partitioned (default min(100, MaxComponentStations); 4 at least).
	// The epoch cost of a partitioned domain of N stations grows as
	// N·PartitionStations² (cubic per partition), so smaller partitions are
	// cheaper; every partition boundary costs some accuracy (external
	// members, external.go). Measured scaling: bench_test.go, scale_test.go.
	PartitionStations int
	// ExternalInflate scales the variance of an external member (a station
	// of another partition of the same domain, taken from the previous
	// snapshot) before it enters the measurement noise (default 2; ≥ 1).
	// The external estimate is correlated with the partition's own state
	// (both were informed by the groups that cross the boundary), and the
	// consider treatment ignores that cross-covariance; counting the shared
	// information twice made the partitioned filter over-confident (e²/σ²
	// 1.44 against 0.87 for the 12 MHz clocks of the same network
	// unpartitioned). Inflating by 2 is the covariance-intersection bound
	// with equal weights: consistent for any unknown correlation.
	ExternalInflate float64
	// ExternalStates caps the cross-partition members per partition and
	// epoch that are estimated as shared epoch-local states and
	// marginalised out (external.go; default 64, 0 = default, negative =
	// off: every external member's prediction variance goes into its
	// group's measurement noise, the pre-phase-5 treatment).
	ExternalStates int
	// PartitionDriftNs is the common-mode random walk (ns/√s) added every
	// epoch to the clock offsets of a partition that does not carry its
	// domain's gauge (partition.go "Common-mode drift"; default 1,
	// negative = off). Such a partition's time scale is pinned only by its
	// external members, whose estimates were themselves partly informed by
	// this partition in earlier epochs; without the drift that circular
	// information never decays for the anchors (δ walks 1 ns/√h) and the
	// partitions lock each other at tens of ns with σ of a few ns.
	PartitionDriftNs float64
	// Workers is the number of goroutines that update components in
	// parallel (default GOMAXPROCS).
	Workers int

	// OnBlacklist, when set, is called (from Step) for every aircraft the
	// filter blacklists from sync, with the end of the blacklisting in Unix
	// ns; wire it to the reference store (mlat/ref Store.Suspend) so the
	// aircraft stops being offered as a reference.
	OnBlacklist func(key uint32, untilUnixNs int64)

	// Logf receives rare diagnostic messages (nil = none).
	Logf func(format string, args ...any)

	// PosSigmaFreeM is the station position σ that the observation model
	// already tolerates (default 30 m, the default σ of a declared
	// position); only StationInfo.PosSigmaM beyond it inflates the
	// observation variance (finding MLAT-R4).
	PosSigmaFreeM float64

	// OnPanic, when set, is told about a panic inside Step (value and
	// stack). Step recovers it, resets every clock state (stations keep
	// their registration and re-sync from their priors) and carries on, so
	// a filter fault never takes the whole process down (finding SCALE-01).
	OnPanic func(v any, stack []byte)
}

func (c *Config) normalize() error {
	if c.OriginUnixNs == 0 {
		return errors.New("clock: Config.OriginUnixNs is required")
	}
	def := func(p *float64, v float64) {
		if !(*p > 0) {
			*p = v
		}
	}
	defI := func(p *int, v int) {
		if *p <= 0 {
			*p = v
		}
	}
	def(&c.EpochS, 0.5)
	def(&c.ReoriginS, 86400)
	c.ReoriginS = math.Max(c.ReoriginS, 10)
	c.EpochS = math.Min(math.Max(c.EpochS, 0.05), 5)
	if c.Atmosphere == (geo.Atmosphere{}) {
		c.Atmosphere = geo.DefaultAtmosphere()
	} else {
		c.Atmosphere = c.Atmosphere.Normalize()
	}
	if c.FrameRefNs == nil {
		c.FrameRefNs = DefaultFrameRefNs
	}
	def(&c.HostSigmaS, 0.05)
	def(&c.ResetThetaSigmaNs, 1e6)
	switch {
	case c.PPSCorrelatedNs == 0:
		c.PPSCorrelatedNs = 30
	case c.PPSCorrelatedNs < 0:
		c.PPSCorrelatedNs = 0
	}
	defI(&c.MaxMembers, 32)
	if c.MaxMembers < 3 {
		c.MaxMembers = 3
	}
	def(&c.SyncRatePerStation, 6)
	c.SyncRatePerStation = math.Min(c.SyncRatePerStation, 100)
	def(&c.MaxObsAgeS, 5)
	def(&c.StationTimeoutS, 120)
	def(&c.CoObsWindowS, 60)
	def(&c.PosSigmaFreeM, 30)
	def(&c.ProbationS, 600)
	def(&c.AnchorFreqMaxPPB, 2)
	def(&c.AnchorOffsetMaxNs, 200)
	def(&c.AnchorDriftMaxNsPerH, 100)
	def(&c.AnchorDriftWindowS, 600)
	def(&c.LeapToleranceNs, 1000)
	def(&c.AnchorHostMaxS, 0.4)
	defI(&c.AnchorHostMinObs, 20)
	def(&c.AnchorGaugeSigmaNs, 10)
	def(&c.RelGaugeThetaSigmaNs, 10)
	def(&c.RelGaugeFreqSigmaPPB, 1)
	switch {
	case c.RelGaugeBetaSigmaNs == 0:
		c.RelGaugeBetaSigmaNs = 10
	case c.RelGaugeBetaSigmaNs < 0:
		c.RelGaugeBetaSigmaNs = 0
	}
	def(&c.GaugeRelaxS, 3600)
	def(&c.GateProb, 0.999)
	if c.GateProb >= 1 {
		c.GateProb = 0.999
	}
	def(&c.WTestMax, 4.5)
	def(&c.StepSigma, 4)
	defI(&c.StepCount, 5)
	if c.StepRunCount == 0 { // negative = off (stepRun skips it)
		c.StepRunCount = 3
	}
	def(&c.StepRunWindowS, 60)
	def(&c.InfluenceCap, 3)
	def(&c.BlacklistS, 600)
	defI(&c.BlacklistMinGrp, 10)
	def(&c.BlacklistRate, 0.5)
	def(&c.BlacklistMaxFrac, 0.2)
	def(&c.StationBadOutlier, 0.25)
	def(&c.AdaptWindowS, 600)
	def(&c.QScaleMin, 0.25)
	def(&c.QScaleMax, 10)
	def(&c.JitterScaleMin, 0.5)
	def(&c.JitterScaleMax, 4)
	defI(&c.SyncedMinObs, 5)
	def(&c.SyncedWindowS, 30)
	defI(&c.MaxComponentStations, 200)
	if c.MaxComponentStations < 4 {
		c.MaxComponentStations = 4
	}
	if c.PartitionStations <= 0 {
		c.PartitionStations = min(100, c.MaxComponentStations)
	}
	c.PartitionStations = min(max(c.PartitionStations, 4), c.MaxComponentStations)
	def(&c.ExternalInflate, 2)
	switch {
	case c.ExternalStates == 0:
		c.ExternalStates = 64
	case c.ExternalStates < 0:
		c.ExternalStates = 0 // off
	}
	c.ExternalInflate = math.Max(c.ExternalInflate, 1)
	switch {
	case c.PartitionDriftNs == 0:
		c.PartitionDriftNs = 1
	case c.PartitionDriftNs < 0 || math.IsNaN(c.PartitionDriftNs):
		c.PartitionDriftNs = 0 // off
	}
	defI(&c.Workers, runtime.GOMAXPROCS(0))
	return nil
}

// DefaultFrameRefNs is the default frame reference table FrameRef(class,
// kind) of DESIGN §4.3 (ns after the preamble start):
//
//   - GPS classes (Radarcape GPS mode): end of frame, length dependent:
//     A/C 20.3 µs, short 64 µs, long 120 µs.
//   - hw12 (Beast / Radarcape 12 MHz): one fixed point that does not depend
//     on the frame length: 64 µs for both Mode S kinds, 20.3 µs for A/C.
//     This is the observed 12 MHz interop convention (the reference
//     client removes 768 ticks from short and long frames alike, MLATC.md
//     §5); it is not the GPS table. A length-dependent table here would put
//     long frames 56 µs early against short ones (finding M2).
//   - sbs20 (SBS-3): end of frame. The reference client adds 56 µs to short
//     and 96 µs to A/C stamps so that everything lines up with the end of a
//     long frame; the server removes those offsets again, so the raw stamps
//     are end-of-frame: short 64 µs, long 120 µs, A/C 120 − 96 = 24 µs
//     (finding M1).
//   - SDR-derived classes: preamble start (0).
//
// A constant error of this table is absorbed by θ; a short-versus-long
// error only up to the β_short prior (1 µs, so tens of µs are not); an
// A/C error is not estimated (A/C is not used for sync).
func DefaultFrameRefNs(cls model.ClockClass, kind model.FrameKind) float64 {
	switch cls {
	case model.ClockGPSUTC, model.ClockGPSGPST, model.ClockGPSEmul:
		switch kind {
		case model.FrameModeAC:
			return 20300
		case model.FrameModeSShort:
			return 64000
		case model.FrameModeSLong:
			return 120000
		}
	case model.ClockHW12:
		switch kind {
		case model.FrameModeAC:
			return 20300
		case model.FrameModeSShort, model.FrameModeSLong:
			return 64000
		}
	case model.ClockSBS20:
		switch kind {
		case model.FrameModeAC:
			return 24000
		case model.FrameModeSShort:
			return 64000
		case model.FrameModeSLong:
			return 120000
		}
	}
	return 0
}

// classPrior holds the clock-model priors of a class in filter units
// (θ ns, φ ppb, ψ ppb/s, β ns; process noise spectral densities per s).
type classPrior struct {
	qTheta float64 // white frequency noise: ns²/s
	qPhi   float64 // frequency random walk: ppb²/s
	qPsi   float64 // drift random walk: (ppb/s)²/s (classes with ψ only)
	psi    bool    // has the ψ state
	phiSig float64 // prior σ(φ), ppb
	psiSig float64 // prior σ(ψ), ppb/s
	betSig float64 // prior σ(β_short), ns
	jitter float64 // prior timestamp jitter, ns
}

// qBeta is the β_short random walk (1 ns/√h) and qDelta the anchor δ random
// walk (1 ns/√h), both ns²/s.
const (
	qBeta  = 1.0 / 3600
	qDelta = 1.0 / 3600
)

// priorFor returns the priors of a class. The process noise values are
// physical estimates (DESIGN §13.1 / RESEARCH §4): GPS-disciplined
// Radarcapes hold frequency to ppb; 12 MHz FPGA clocks random-walk at about
// 0.2 ppb/√s; RTL-SDR crystals random-walk at about 1 ppb/√s and ramp
// thermally by up to ~10 ppb/s, modelled by the ψ state (classes 5 and 7).
// The adaptive Q scale (×0.25…×10) absorbs station-to-station spread.
//
// Deviation from DESIGN §9.2.1: class 6 (Airspy-class, sdr12_hires) also
// gets the ψ state. Its crystal shows the same thermal frequency ramps
// (a few ppb/s); without ψ a ramp leaves a tracking lag of hundreds of ns
// (measured on simnet), with ψ it is followed.
func priorFor(cls model.ClockClass) classPrior {
	p := classPrior{
		jitter: cls.PriorJitterNs(),
		phiSig: cls.PriorFreqSigma() * 1e9,
		psiSig: 20,
		betSig: 1000,
	}
	switch cls {
	case model.ClockGPSUTC, model.ClockGPSGPST:
		p.qTheta, p.qPhi = 1, 1e-4
		p.betSig = 50
		p.jitter = 10 // white part; the 1PPS error is PPSCorrelatedNs
	case model.ClockGPSEmul:
		p.qTheta, p.qPhi = 1, 0.01
		p.betSig = 50
	case model.ClockHW12:
		p.qTheta, p.qPhi = 1, 0.04
	case model.ClockSDR12:
		p.qTheta, p.qPhi, p.qPsi, p.psi = 10, 1, 0.17, true
	case model.ClockSDR12HiRes:
		p.qTheta, p.qPhi, p.qPsi, p.psi = 4, 0.25, 0.02, true
	case model.ClockSBS20:
		p.qTheta, p.qPhi, p.qPsi, p.psi = 10, 1, 0.04, true
	default:
		p.qTheta, p.qPhi = 10, 1
		p.jitter = 500
		p.phiSig = 1e5
	}
	if p.jitter <= 0 {
		p.jitter = 500
	}
	if p.phiSig <= 0 {
		p.phiSig = 1e5
	}
	return p
}
