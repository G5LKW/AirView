package ref

// Options tunes a Store. Zero values select the documented defaults
// (withDefaults); every default is justified in doc.go.
type Options struct {
	// Window is the number of measurement nodes kept per track (default 16,
	// 6..64). Each node is one position or velocity report; at the usual
	// 2 Hz + 2 Hz ADS-B rates 16 nodes cover about 4 s, which bounds how far
	// back a transmit instant can be interpolated (older instants are
	// extrapolated backwards and flagged). Memory is about 350 B per node.
	Window int

	// QHorizontal and QVertical are the white-jerk power spectral densities
	// of the process model in m²/s⁵ (defaults 4 and 1).
	QHorizontal float64
	QVertical   float64

	// PosWhiteSigmaM is white position noise added to the CPR quantisation
	// of every position report, per horizontal axis (default 0.5 m).
	PosWhiteSigmaM float64
	// VelSigmaMinMps / VelSigmaMaxMps bound the 1σ noise of a reported
	// horizontal velocity component; inside the bounds it is derived from
	// NACv (HFOM/2.45) and the 1 kt (4 kt supersonic) quantisation
	// (defaults 0.3 and 2 m/s; 1 m/s when NACv is unknown).
	VelSigmaMinMps float64
	VelSigmaMaxMps float64
	// VertRateSigmaMps is the 1σ noise of a reported vertical rate (default
	// 0.4 m/s); barometric rates get an extra 5 % of the rate.
	VertRateSigmaMps float64

	// MaxPosAgeS is the largest distance in time between a queried
	// transmit instant and the nearest position report for the state to be
	// usable as a sync reference (DESIGN §9.2.2: 1 s).
	MaxPosAgeS float64
	// MaxExtrapS is how far past the newest node (or before the oldest) a
	// query is answered at all (default 5 s).
	MaxExtrapS float64
	// MinNACp and MinNIC are the sync gates (DESIGN §9.2.2: 8 and 7).
	MinNACp uint8
	MinNIC  uint8
	// NoNUCp disables the fallback that derives an equivalent NACp from the
	// position type code while no operational status has been received
	// (doc.go "Quality").
	NoNUCp bool
	// MaxVertSigmaM is the largest 1σ height uncertainty of a usable
	// reference (default 60 m).
	MaxVertSigmaM float64

	// DefaultLatencyS is the prior position latency of T=0 transmitters
	// (default 0) and LatencySigmaS its 1σ (default 0.15 s: the DO-260B
	// −200..+400 ms budget). The estimate is refined by ObserveFix.
	DefaultLatencyS float64
	LatencySigmaS   float64
	// RetagS: the position nodes are re-tagged with a new latency estimate
	// once it differs from the one in use by more than this (default 2 ms).
	RetagS float64

	// GateChi2 is the Mahalanobis gate for horizontal position innovations
	// (2 degrees of freedom, default 30, p ≈ 3e-7) and GateFloorM the
	// innovation size that is always accepted (default 25 m). VelGateMps is
	// the matching floor for velocity innovations (default 15 m/s).
	GateChi2   float64
	GateFloorM float64
	VelGateMps float64
	// MaxSpeedMps bounds the implied ground speed between accepted
	// positions (default 600 m/s, about Mach 1.9 at altitude).
	MaxSpeedMps float64
	// JumpConfirm is the number of consecutive rejected but mutually
	// consistent positions that re-initialise the track (default 3).
	JumpConfirm int
	// ProbationS is how long a track is unusable after a jump, a T=1 fault
	// or a velocity inconsistency (default 30 s).
	ProbationS float64
	// UnstableJumps jumps (or ConflictRejects rejected positions) within
	// UnstableWindowS mark the aircraft unstable (duplicate address or
	// spoofer) and suspend it for SuspendS (defaults 2, 6, 600 s, 600 s).
	UnstableJumps   int
	ConflictRejects int
	UnstableWindowS float64
	SuspendS        float64
	// ConfirmPositions accepted positions plus one velocity confirm a track
	// (default 3).
	ConfirmPositions int
	// PairMaxS is the largest time between an even and an odd CPR frame
	// used for a global decode (default 10 s); StaleS the position gap
	// after which a track restarts from a global decode (default 30 s).
	PairMaxS float64
	StaleS   float64
	// TrackTimeoutS: Expire drops tracks not updated for this long
	// (default 120 s).
	TrackTimeoutS float64
	// HorizonK and HorizonMarginM configure CheckHeard: a position more
	// than HorizonMarginM below the radio horizon of a receiver that heard
	// it with effective-earth factor HorizonK is implausible (defaults 2 and
	// 2000 m; generous for ducting).
	HorizonK       float64
	HorizonMarginM float64

	// AltReplyMinS: the barometric altitude of a Mode S reply (DF0/4/16/20)
	// of an aircraft with a track enters its vertical channel at most once
	// per AltReplyMinS (default 2 s), so replies fill the gaps between
	// position squitters without pushing them out of the Window.
	// NoAltReplies ignores those replies altogether.
	AltReplyMinS float64
	NoAltReplies bool
	// PairBaroS: a GNSS height (TC 20-22) and a pressure altitude reported
	// within PairBaroS of each other (default 1.5 s, vertical rate
	// compensated) give the aircraft's GNSS−baro difference and one field
	// observation.
	PairBaroS float64

	// Field configures the GNSS−baro field (FieldOptions).
	Field FieldOptions
}

func (o Options) withDefaults() Options {
	def := func(v *float64, d float64) {
		if !(*v > 0) {
			*v = d
		}
	}
	defi := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defi(&o.Window, 16)
	o.Window = min(max(o.Window, 6), 64)
	def(&o.QHorizontal, 4)
	def(&o.QVertical, 1)
	def(&o.PosWhiteSigmaM, 0.5)
	def(&o.VelSigmaMinMps, 0.3)
	def(&o.VelSigmaMaxMps, 2)
	def(&o.VertRateSigmaMps, 0.4)
	def(&o.MaxPosAgeS, 1)
	def(&o.MaxExtrapS, 5)
	if o.MinNACp == 0 {
		o.MinNACp = 8
	}
	if o.MinNIC == 0 {
		o.MinNIC = 7
	}
	def(&o.MaxVertSigmaM, 60)
	def(&o.LatencySigmaS, 0.15)
	def(&o.RetagS, 0.002)
	def(&o.GateChi2, 30)
	def(&o.GateFloorM, 25)
	def(&o.VelGateMps, 15)
	def(&o.MaxSpeedMps, 600)
	defi(&o.JumpConfirm, 3)
	def(&o.ProbationS, 30)
	defi(&o.UnstableJumps, 2)
	defi(&o.ConflictRejects, 6)
	def(&o.UnstableWindowS, 600)
	def(&o.SuspendS, 600)
	defi(&o.ConfirmPositions, 3)
	def(&o.PairMaxS, 10)
	def(&o.StaleS, 30)
	def(&o.TrackTimeoutS, 120)
	def(&o.HorizonK, 2)
	def(&o.HorizonMarginM, 2000)
	def(&o.AltReplyMinS, 2)
	def(&o.PairBaroS, 1.5)
	o.Field = o.Field.withDefaults()
	return o
}
