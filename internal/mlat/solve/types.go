package solve

import (
	"errors"
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
	"airview/internal/model"
)

// Size limits.
const (
	// MaxStations is the largest number of TOA measurements in one problem.
	// Larger clusters must be thinned by the caller (keep the best-timed
	// and best-spread stations); beyond ~20 stations the accuracy gain is
	// negligible.
	MaxStations = 64
	// maxRows = TOA rows + altitude row + 3 prior rows.
	maxRows = MaxStations + 4
	// nParam is the number of unknowns: ENU position about the seed and
	// the transmit-time term b = c·τ (metres).
	nParam = 4
)

// Speed of light (m/s), from mlat/geo.
const c = geo.C

// Errors returned by Solve. A returned error means no Fix was produced; a
// produced but unusable fix has Fix.Reject set instead.
var (
	ErrTooFew     = errors.New("solve: not enough measurements (need 4 TOA, 3 TOA + altitude, or 2 TOA + altitude/prior)")
	ErrTooMany    = errors.New("solve: more than MaxStations measurements")
	ErrBadInput   = errors.New("solve: non-finite or inconsistent input")
	ErrNoSolution = errors.New("solve: no initialisation converged to a plausible solution")
)

// Station is one TOA measurement of the cluster.
type Station struct {
	// Pos is the antenna position, ECEF metres, with the height above the
	// WGS-84 ellipsoid (convert MSL with internal/geoid first).
	Pos geo.Vec3
	// TOA is the network-time time of arrival in seconds relative to
	// Problem.EpochNs. Keep the epoch close to the cluster (within a few
	// seconds) so that float64 keeps sub-picosecond resolution.
	TOA float64
	// Sigma is the 1σ uncertainty of TOA in seconds (jitter and clock),
	// used only when Problem.Cov is nil.
	Sigma float64
	// ID is an opaque caller tag (station index); it is not used by the
	// solver.
	ID int32
}

// Altitude is an altitude measurement row: geometric height above the
// ellipsoid with its 1σ (see AltitudeFromBaro).
type Altitude struct {
	Valid bool
	HAE   float64 // m above the WGS-84 ellipsoid
	Sigma float64 // 1σ, m
}

// Prior is a position prediction (normally from the tracker).
//
// A prior whose horizontal σ is below Options.PriorSeedMaxSigma is the
// first initialisation (DESIGN §9.4 step 1) and the tie-breaker between
// ambiguous hypotheses. It becomes three extra measurement rows ("fused")
// when Fuse is set or when the TOA and altitude rows alone cannot fix the
// position (2 stations + altitude, 3 stations without altitude); the
// result is then a posterior, flagged FlagPriorFused.
type Prior struct {
	Valid bool
	Pos   geo.Vec3   // ECEF, m
	Cov   [9]float64 // ECEF covariance, m², row-major
	Fuse  bool
}

// Motion describes the last accepted position of the aircraft for the
// implied-speed check (DESIGN §9.4 reject list).
type Motion struct {
	Valid  bool
	Pos    geo.Vec3 // ECEF, m
	SigmaM float64  // 1σ horizontal uncertainty of Pos, m
	DtS    float64  // time from that position to this transmission, s (> 0)
	// Mach is the aircraft's reported Mach number (BDS 6,0), 0 when
	// unknown: the speed limit becomes max(MaxSpeedMps, MachFactor·Mach·a(h)).
	Mach float64
}

// Problem is one MLAT cluster to solve. The slices are read during Solve
// only and never retained.
type Problem struct {
	// EpochNs is the network-time reference of the TOAs (Unix ns; UTC when
	// the component is anchored).
	EpochNs  int64
	Stations []Station
	// Cov is the optional full m×m covariance of the TOAs in s² (row-major,
	// m = len(Stations)): the clock-filter covariance of the stations'
	// offsets at the TOA plus diag(jitter²). nil means diag(Sigma²). Only
	// the lower triangle is read. See CovFromClock.
	Cov   []float64
	Alt   Altitude
	Prior Prior
	Last  Motion
	// HostFirstS is the latency-corrected host time of the first arriving
	// copy, in seconds relative to EpochNs; with HostValid the solution's
	// transmit time must agree within Options.HostTolS.
	HostFirstS float64
	HostValid  bool
}

// InitMethod says which initialisation produced the final solution.
type InitMethod uint8

// Initialisation methods (DESIGN §9.4 order).
const (
	InitNone     InitMethod = iota
	InitPrior               // track prediction
	InitLinear              // linear TOA least squares (N ≥ 5)
	InitBancroft            // Bancroft (N ≥ 4)
	InitQuartic             // 3 + altitude closed form (spherical shell)
	InitGrid                // 3 + altitude grid search
	InitCentroid            // receiver centroid at the altitude
	InitRANSAC              // minimal-subset consensus
)

var initNames = [...]string{"none", "prior", "linear", "bancroft", "quartic", "grid", "centroid", "ransac"}

// String returns the method name.
func (m InitMethod) String() string {
	if int(m) < len(initNames) {
		return initNames[m]
	}
	return "init?"
}

// Flags describe how a fix was obtained.
type Flags uint16

// Fix flags.
const (
	FlagConverged       Flags = 1 << iota // LM met the step tolerance
	FlagAltConstrained                    // the altitude row was used
	FlagPriorFused                        // the prior was used as measurement rows
	FlagAmbiguous                         // a second plausible hypothesis exists (Fix.Alt)
	FlagHuber                             // at least one measurement was down-weighted by Huber IRLS
	FlagRAIMExcluded                      // RAIM excluded at least one measurement
	FlagRAIMFailed                        // the χ² test still fails after exclusion (covariance inflated)
	FlagRANSAC                            // the inlier set came from RANSAC
	FlagUnderdetermined                   // DOF = 0: no consistency check possible
)

// Reject is a bit set of reasons why a fix must not be used (DESIGN §9.4).
type Reject uint16

// Reject reasons.
const (
	RejectSigma        Reject = 1 << iota // horizontal σ > Options.MaxSigmaH
	RejectAltitude                        // solution height disagrees with the altitude by > 4σ
	RejectHorizon                         // outside the radio horizon of a used station
	RejectSpeed                           // implied speed from Problem.Last too high
	RejectHostTime                        // transmit time inconsistent with the host arrival time
	RejectImplausible                     // height outside [MinHAE, MaxHAE] or too far away
	RejectNotConverged                    // LM did not converge
	RejectRAIM                            // χ² test failed and nothing more could be excluded
)

var rejectNames = [...]string{"sigma", "altitude", "horizon", "speed", "hosttime", "implausible", "notconverged", "raim"}

// String lists the reasons separated by "|" ("" for none).
func (r Reject) String() string {
	s := ""
	for i, n := range rejectNames {
		if r&(1<<i) != 0 {
			if s != "" {
				s += "|"
			}
			s += n
		}
	}
	return s
}

// DOP holds unit-variance dilution-of-precision figures of the used
// stations at the fix (TOA rows only, in the fix's ENU frame).
type DOP struct {
	G, P, H, V, T float64
}

// Hypothesis is an alternative solution (3 + altitude and 4-station
// ambiguity).
type Hypothesis struct {
	ECEF   geo.Vec3
	Chi2   float64
	SigmaH float64
	Init   InitMethod
	// CovECEF is the alternative's own CRLB position covariance (m², ECEF,
	// unscaled): the far branch of a 3+alt pair is often ten times less
	// certain than the near one, and a tracker hypothesis started or updated
	// with the chosen branch's covariance would gate its own later fixes
	// away (H-track).
	CovECEF [9]float64
}

// Fix is the result of a solve. All covariance matrices are 3×3 row-major
// in m².
type Fix struct {
	ECEF geo.Vec3
	LLH  geo.LLH
	// TxS is the transmit time in seconds relative to Problem.EpochNs.
	TxS float64
	// CovECEF and CovENU are the a-posteriori position covariance
	// CRLB·Scale with Scale = max(1, χ²/DOF); CovENU is in the ENU frame at
	// the fix. CRLBENU is the unscaled Cramér-Rao bound (HᵀWH)⁻¹.
	CovECEF, CovENU, CRLBENU [9]float64
	Scale                    float64
	// VarTx is the variance of TxS (s²), scaled like CovENU.
	VarTx float64
	// SigmaH = sqrt(σE² + σN²) and SigmaV = σU of CovENU (m).
	SigmaH, SigmaV float64
	Ellipse        linalg.Ellipse
	DOP            DOP
	// Chi2 is the weighted SSE rᵀΣ⁻¹r of the used rows under the nominal
	// covariance; DOF = rows − 4.
	Chi2 float64
	DOF  int
	// NStations is the number of TOA stations used. Used and Excluded are
	// bit masks over Problem.Stations indices.
	NStations      int
	Used, Excluded uint64
	AltUsed        bool
	AltExcluded    bool
	PriorUsed      bool
	// Per station (Problem.Stations index): Resid is model minus
	// measurement in metres at the fix (also for excluded stations),
	// WTest the Baarda w-test statistic (0 for excluded stations), Weight
	// the final Huber weight.
	Resid  [MaxStations]float32
	WTest  [MaxStations]float32
	Weight [MaxStations]float32
	// MDB is Baarda's minimal detectable bias of each used station (m):
	// the smallest timing fault (×c) the RAIM test finds with 80 %
	// probability at α = 1e-3 (internal reliability; +Inf without
	// redundancy, 0 for unused stations).
	MDB [MaxStations]float32
	// AltResid is solution height minus measured altitude (m).
	AltResid float64
	// Iter counts LM iterations over all starts, Starts the starts tried,
	// Exclusions the RAIM exclusions.
	Iter, Starts, Exclusions int
	Init                     InitMethod
	Flags                    Flags
	Reject                   Reject
	// Grade is model.GradeFor(SigmaH, DOF) (GradeNone when rejected).
	Grade uint8
	// Alt holds up to two alternative hypotheses (NAlt of them).
	NAlt int
	Alt  [2]Hypothesis
}

// OK reports whether the fix passed every rejection test.
func (f *Fix) OK() bool { return f.Reject == 0 }

// MLATFix converts the fix to the network result contract. The station
// count goes into a bucket; station identities are never included.
func (f *Fix) MLATFix(icao uint32, epochNs int64) model.MLATFix {
	out := model.MLATFix{
		ICAO:    icao,
		TUnixNs: epochNs + int64(math.Round(f.TxS*1e9)),
		ECEF:    [3]float64{f.ECEF.X, f.ECEF.Y, f.ECEF.Z},
		NBucket: model.NBucketFor(f.NStations),
		Grade:   f.Grade,
	}
	cv := &f.CovECEF
	out.Cov = [6]float32{float32(cv[0]), float32(cv[1]), float32(cv[2]), float32(cv[4]), float32(cv[5]), float32(cv[8])}
	if f.Flags&FlagAltConstrained != 0 {
		out.Flags |= model.FixFlagAltConstrained
	}
	if f.Flags&FlagAmbiguous != 0 {
		out.Flags |= model.FixFlagAmbiguous
	}
	return out
}

// Options tune the solver. The zero value of every field selects the
// documented default (see Defaults).
type Options struct {
	// Atmosphere is the refraction model of the propagation ranges
	// (DefaultAtmosphere when zero). NoRefraction uses straight-line
	// geometric ranges (tests, comparisons).
	Atmosphere   geo.Atmosphere
	NoRefraction bool
	// MaxIter bounds the LM iterations per start (10).
	MaxIter int
	// HuberK is the Huber threshold on the w-test statistic (1.345).
	HuberK float64
	// PFA is the false-alarm probability of the RAIM χ² test (1e-3).
	PFA float64
	// MaxSigmaH rejects fixes with a larger horizontal σ (2000 m).
	MaxSigmaH float64
	// AltRejectSigma rejects fixes whose height disagrees with the
	// altitude measurement by more than this many σ (4).
	AltRejectSigma float64
	// PriorSeedMaxSigma: a prior with a horizontal σ below this seeds the
	// solve (2000 m).
	PriorSeedMaxSigma float64
	// GridStepM and GridHalfM define the 3 + altitude grid (5 km over
	// ±400 km).
	GridStepM, GridHalfM float64
	// MaxSpeedMps is the implied-speed limit (600 kt = 308.7 m/s);
	// MachFactor scales a reported Mach (1.25).
	MaxSpeedMps, MachFactor float64
	// HostTolS is the transmit-time tolerance against the host arrival
	// time (0.05 s).
	HostTolS float64
	// HorizonSlackM is the clearance below the 4/3-earth horizon that is
	// still accepted (300 m).
	HorizonSlackM float64
	// MinHAE / MaxHAE bound plausible heights (-500 m / 25 km); MaxRangeM
	// bounds the distance from the nearest station (800 km).
	MinHAE, MaxHAE, MaxRangeM float64
	// RelinM: the refraction excess is linearised about a point and
	// re-linearised when the estimate moves further than this during the
	// iterations (5000 m), and once more at the solution when it ended more
	// than 100 m from the linearisation point.
	RelinM float64
	// RANSACMinStations enables RANSAC from this many stations (6) when
	// RAIM has failed twice; RANSACMaxSubsets bounds the subsets (64).
	RANSACMinStations, RANSACMaxSubsets int
	// AlwaysAlternates searches for the second 3+alt / 4-station hypothesis
	// even when the prior seed already gave a consistent solution.
	AlwaysAlternates bool
}

// Defaults returns o with every zero field replaced by its default.
func (o Options) Defaults() Options {
	if o.Atmosphere == (geo.Atmosphere{}) {
		o.Atmosphere = geo.DefaultAtmosphere()
	}
	o.Atmosphere = o.Atmosphere.Normalize()
	def := func(v *float64, d float64) {
		if !(*v > 0) {
			*v = d
		}
	}
	if o.MaxIter <= 0 {
		o.MaxIter = 10
	}
	def(&o.HuberK, 1.345)
	if !(o.PFA > 0 && o.PFA < 0.5) {
		o.PFA = 1e-3
	}
	def(&o.MaxSigmaH, 2000)
	def(&o.AltRejectSigma, 4)
	def(&o.PriorSeedMaxSigma, 2000)
	def(&o.GridStepM, 5000)
	def(&o.GridHalfM, 400000)
	def(&o.MaxSpeedMps, 600*1852.0/3600)
	def(&o.MachFactor, 1.25)
	def(&o.HostTolS, 0.05)
	def(&o.HorizonSlackM, 300)
	if o.MinHAE == 0 {
		o.MinHAE = -500
	}
	def(&o.MaxHAE, 25000)
	def(&o.MaxRangeM, 800000)
	def(&o.RelinM, 5000)
	if o.RANSACMinStations <= 0 {
		o.RANSACMinStations = 6
	}
	if o.RANSACMaxSubsets <= 0 {
		o.RANSACMaxSubsets = 64
	}
	return o
}
