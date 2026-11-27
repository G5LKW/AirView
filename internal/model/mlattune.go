package model

import "strings"

// Tunables of the MLAT engine's parts (DESIGN §9, §20.3). Every block uses
// "zero = default": a field left at 0 (or absent from the file) takes the
// package default named in its comment, so a config file lists only the
// values it changes. Normalize resets negative values (where a negative
// value has no meaning) to 0 with a note; the packages' own mappers
// (ref/group/plan/solve/track/mlatc OptionsFromConfig) clamp the rest.

// AggMLATRef tunes the ADS-B reference tracks (internal/mlat/ref, DESIGN
// §9.2.2, §9.6). Zero = default.
type AggMLATRef struct {
	// Window is the number of measurement nodes kept per track (default 16,
	// 6..64).
	Window int `json:"window"`
	// QHorizontal is the horizontal white-jerk PSD in m²/s⁵ (default 4).
	QHorizontal float64 `json:"qHorizontal"`
	// QVertical is the vertical white-jerk PSD in m²/s⁵ (default 1).
	QVertical float64 `json:"qVertical"`
	// MinNACp is the sync-reference NACp gate (default 8, 1..11).
	MinNACp int `json:"minNacp"`
	// MinNIC is the sync-reference NIC gate (default 7, 1..11).
	MinNIC int `json:"minNic"`
	// MaxPosAgeS is the largest time from a queried instant to the nearest
	// position report of a usable reference (default 1 s).
	MaxPosAgeS float64 `json:"maxPosAgeS"`
	// LatencySigmaS is the prior 1σ position latency of T=0 transmitters
	// (default 0.15 s).
	LatencySigmaS float64 `json:"latencySigmaS"`
	// GateChi2 is the position innovation gate (2 degrees of freedom,
	// default 30).
	GateChi2 float64 `json:"gateChi2"`
	// GateFloorM is the innovation always accepted (default 25 m).
	GateFloorM float64 `json:"gateFloorM"`
	// ProbationS keeps a track unusable after a jump (default 30 s).
	ProbationS float64 `json:"probationS"`
	// SuspendS suspends an unstable (duplicate or spoofed) address (default
	// 600 s).
	SuspendS float64 `json:"suspendS"`
	// TrackTimeoutS drops tracks not updated for this long (default 120 s).
	TrackTimeoutS float64 `json:"trackTimeoutS"`
	// AltReplyMinS: the barometric altitude of a Mode S reply (DF0/4/16/20)
	// updates a reference track's vertical channel at most once per this
	// interval (default 2 s).
	AltReplyMinS float64 `json:"altReplyMinS"`
	// PairBaroS: a GNSS height (TC 20-22) and a pressure altitude reported
	// within this interval form one GNSS−baro sample (default 1.5 s).
	PairBaroS float64 `json:"pairBaroS"`
	// NoAltReplies ignores the altitude of Mode S replies in the reference
	// tracks (default false).
	NoAltReplies bool `json:"noAltReplies"`
	// Field tunes the GNSS−baro field.
	Field AggMLATField `json:"field"`
}

// AggMLATField tunes the regional GNSS−baro field (DESIGN §9.6). Zero =
// default.
type AggMLATField struct {
	// CellKm is the horizontal cell size (default 50 km).
	CellKm float64 `json:"cellKm"`
	// BandFt is the altitude band of a cell (default 2000 ft).
	BandFt float64 `json:"bandFt"`
	// DriftMPerH is the random-walk rate of a cell in m per √h (default 20).
	DriftMPerH float64 `json:"driftMPerH"`
	// ObsSigmaM is the 1σ of one per-aircraft observation (default 12 m).
	ObsSigmaM float64 `json:"obsSigmaM"`
	// ProfileSigmaM is the 1σ of the weather-profile fallback used where no
	// cell has been learned (default 25 m).
	ProfileSigmaM float64 `json:"profileSigmaM"`
	// ProfileSigmaTK is the 1σ temperature error of that profile (default
	// 1.5 K).
	ProfileSigmaTK float64 `json:"profileSigmaTK"`
}

// AggMLATGroup tunes the cluster assembler (internal/mlat/group, DESIGN
// §9.3). Zero = default.
type AggMLATGroup struct {
	// NSigma is the gate width in σ of the timing noise (default 4, 2..8).
	NSigma float64 `json:"nSigma"`
	// SlackM is the geometric slack of the baseline gate (default 1000 m).
	SlackM float64 `json:"slackM"`
	// HintSlackM is the geometric slack of the predicted-position gate
	// (default 300 m).
	HintSlackM float64 `json:"hintSlackM"`
	// HintMaxSigmaM ignores position hints with a larger 1σ (default 5000 m).
	HintMaxSigmaM float64 `json:"hintMaxSigmaM"`
	// ColocatedM: stations closer than this are one site (default 1000 m).
	ColocatedM float64 `json:"colocatedM"`
	// MinSites is the number of sites needed to solve without an altitude
	// (default 4, 3..8).
	MinSites int `json:"minSites"`
	// MinSitesAlt is the number of sites needed with an altitude (default 3).
	MinSitesAlt int `json:"minSitesAlt"`
	// MaxParts is the most clusters taken from one ingest group (default 4,
	// 1..8).
	MaxParts int `json:"maxParts"`
	// LateHoldMs keeps emitted clusters this long for late copies (default
	// 2000, ..5000).
	LateHoldMs int `json:"lateHoldMs"`
	// LateSlots is the size of the late-copy ring per MLAT worker (default
	// 16384, about 1 kB per slot); above 4 workers the rings share 4x this
	// size (at least 2048 slots each).
	LateSlots int `json:"lateSlots"`
}

// AggMLATPlan tunes the MLAT planner (internal/mlat/plan, DESIGN §4.4,
// §9.3). Zero = default.
type AggMLATPlan struct {
	// SyncSetMin is the smallest syncSet per station (default 15).
	SyncSetMin int `json:"syncSetMin"`
	// SyncSetMax is the largest syncSet per station (default 30).
	SyncSetMax int `json:"syncSetMax"`
	// MLATSetMax caps a station's mlatSet in aircraft (default 400).
	MLATSetMax int `json:"mlatSetMax"`
	// StationFPSBudget caps the frame rate a selective station is asked for
	// (default 400 frames/s).
	StationFPSBudget float64 `json:"stationFpsBudget"`
	// StationsPerAircraft is the most stations asked for one MLAT aircraft
	// (default 12, 4..64).
	StationsPerAircraft int `json:"stationsPerAircraft"`
	// MinStationsPerAircraft is the number always asked for when available
	// (default 6).
	MinStationsPerAircraft int `json:"minStationsPerAircraft"`
	// TargetSigmaM: above the minimum, stations are added while the expected
	// horizontal σ is above this (default 30 m).
	TargetSigmaM float64 `json:"targetSigmaM"`
	// HeardTTLS: a station counts as hearing an aircraft this long after its
	// last copy (default 60 s).
	HeardTTLS int `json:"heardTtlS"`
	// ADSBGoodAgeS: an aircraft with a good ADS-B position this recent is no
	// MLAT candidate (default 10 s).
	ADSBGoodAgeS int `json:"adsbGoodAgeS"`
	// MinDwellS keeps a set member at least this long (default 30 s).
	MinDwellS int `json:"minDwellS"`
	// MaxChurn is the most set changes per station and replan (default 10).
	MaxChurn int `json:"maxChurn"`
	// ReplanEveryS is the replan period (default 5 s).
	ReplanEveryS int `json:"replanEveryS"`
	// AcquireHz is the solve rate of aircraft being acquired (default 4).
	AcquireHz float64 `json:"acquireHz"`
	// TrackedHz is the solve rate of tracked aircraft (default 2).
	TrackedHz float64 `json:"trackedHz"`
	// HoldOutHz is the solve rate of hold-out ADS-B aircraft (default 1).
	HoldOutHz float64 `json:"holdOutHz"`
	// SuspectHz is the solve rate of suspect ADS-B aircraft (default 0.5).
	SuspectHz float64 `json:"suspectHz"`
	// GlobalSolveHz is the solver budget of the whole network (default
	// 20000 solves/s).
	GlobalSolveHz float64 `json:"globalSolveHz"`
}

// AggMLATSolve tunes the position solver (internal/mlat/solve, DESIGN
// §9.4). Zero = default.
type AggMLATSolve struct {
	// MaxIter bounds the Levenberg-Marquardt iterations per start (default 10).
	MaxIter int `json:"maxIter"`
	// HuberK is the Huber threshold on the w-test (default 1.345).
	HuberK float64 `json:"huberK"`
	// PFA is the false-alarm probability of the RAIM test (default 0.001).
	PFA float64 `json:"pfa"`
	// MaxSigmaHM rejects fixes with a larger horizontal σ (default 2000 m).
	MaxSigmaHM float64 `json:"maxSigmaHM"`
	// AltRejectSigma rejects fixes whose height disagrees with the altitude
	// by more σ (default 4).
	AltRejectSigma float64 `json:"altRejectSigma"`
	// PriorSeedMaxSigmaM: a track prior with a smaller horizontal σ seeds the
	// solve (default 2000 m).
	PriorSeedMaxSigmaM float64 `json:"priorSeedMaxSigmaM"`
	// GridStepM is the step of the 3+altitude start grid (default 5000 m).
	GridStepM float64 `json:"gridStepM"`
	// GridHalfM is the half width of that grid (default 400000 m).
	GridHalfM float64 `json:"gridHalfM"`
	// MaxSpeedMps is the implied-speed limit (default 308.7 m/s = 600 kt).
	MaxSpeedMps float64 `json:"maxSpeedMps"`
	// MachFactor scales a reported Mach number (default 1.25).
	MachFactor float64 `json:"machFactor"`
	// HostTolS is the transmit-time tolerance against the host arrival time
	// (default 0.05 s).
	HostTolS float64 `json:"hostTolS"`
	// HorizonSlackM is the accepted clearance below the radio horizon
	// (default 300 m).
	HorizonSlackM float64 `json:"horizonSlackM"`
	// MinHAEM is the lowest plausible height above the ellipsoid (default
	// -500 m; may be negative).
	MinHAEM float64 `json:"minHaeM"`
	// MaxHAEM is the highest plausible height (default 25000 m).
	MaxHAEM float64 `json:"maxHaeM"`
	// MaxRangeM bounds the distance from the nearest station (default
	// 800000 m).
	MaxRangeM float64 `json:"maxRangeM"`
	// RelinM re-linearises the refraction excess after this movement
	// (default 5000 m).
	RelinM float64 `json:"relinM"`
	// RANSACMinStations enables RANSAC from this many stations (default 6).
	RANSACMinStations int `json:"ransacMinStations"`
	// RANSACMaxSubsets bounds the RANSAC subsets (default 64).
	RANSACMaxSubsets int `json:"ransacMaxSubsets"`
	// AlwaysAlternates always searches for the second solution branch
	// (default false).
	AlwaysAlternates bool `json:"alwaysAlternates"`
}

// AggMLATTrack tunes the MLAT tracker (internal/mlat/track, DESIGN §9.5).
// Zero = default.
type AggMLATTrack struct {
	// QCv is the horizontal acceleration PSD of the constant-velocity model
	// (default 0.3 m²/s³).
	QCv float64 `json:"qCv"`
	// QCt is the horizontal acceleration PSD of the turn model (default 0.5).
	QCt float64 `json:"qCt"`
	// QVert is the vertical acceleration PSD (default 1).
	QVert float64 `json:"qVert"`
	// QOmega is the turn-rate acceleration PSD (default 3e-5 rad²/s³).
	QOmega float64 `json:"qOmega"`
	// OmegaTauS is the correlation time of the turn rate (default 20 s).
	OmegaTauS float64 `json:"omegaTauS"`
	// SojournCvS is the mean time in the straight-flight mode (default 300 s).
	SojournCvS float64 `json:"sojournCvS"`
	// SojournCtS is the mean time in the turn mode (default 20 s).
	SojournCtS float64 `json:"sojournCtS"`
	// InitPCt is the turn-mode probability of a new track (default 0.1).
	InitPCt float64 `json:"initPCt"`
	// InitSigmaVelH is the prior 1σ horizontal speed of a new track (default
	// 150 m/s).
	InitSigmaVelH float64 `json:"initSigmaVelH"`
	// InitSigmaVelU is the prior 1σ vertical rate (default 15 m/s).
	InitSigmaVelU float64 `json:"initSigmaVelU"`
	// InitSigmaOmega is the prior 1σ turn rate (default 0.03 rad/s).
	InitSigmaOmega float64 `json:"initSigmaOmega"`
	// GateProb is the probability mass of the gate (default 0.999).
	GateProb float64 `json:"gateProb"`
	// Iterations is the iterated-EKF relinearisations per update (default 3).
	Iterations int `json:"iterations"`
	// ConfirmWindowS is the confirmation window of a new track (default 10 s).
	ConfirmWindowS float64 `json:"confirmWindowS"`
	// ConfirmUpdates is the number of updates that confirm it (default 2).
	ConfirmUpdates int `json:"confirmUpdates"`
	// CoastS deletes a track without updates for this long (default 60 s).
	CoastS float64 `json:"coastS"`
	// MaxSigmaHM deletes a track with a larger horizontal σ (default 5000 m).
	MaxSigmaHM float64 `json:"maxSigmaHM"`
	// MaxSpeedMS is the speed ceiling of a hypothesis (default 360 m/s).
	MaxSpeedMS float64 `json:"maxSpeedMS"`
	// HypLR drops a competing hypothesis at this likelihood ratio (default 100).
	HypLR float64 `json:"hypLR"`
	// MaxHyp is the number of hypotheses kept per address (default 4).
	MaxHyp int `json:"maxHyp"`
	// RespawnMisses starts a competing hypothesis after this many
	// non-gating fixes (default 3).
	RespawnMisses int `json:"respawnMisses"`
	// LOSMarginM penalises a hypothesis hidden below a hearer's radio
	// horizon by more than this (default 3000 m).
	LOSMarginM float64 `json:"losMarginM"`
	// MaxMembers caps the stations used from one cluster (default 24, 2..32).
	MaxMembers int `json:"maxMembers"`
	// MaxExclude is the number of outlier members that may be excluded from
	// a cluster (default 2, negative = none, ..4).
	MaxExclude int `json:"maxExclude"`
	// MaxLagS drops measurements older than the track by more (default 2.5 s).
	MaxLagS float64 `json:"maxLagS"`
	// ExcessRefreshM recomputes a cached refraction excess after this
	// movement (default 2000 m).
	ExcessRefreshM float64 `json:"excessRefreshM"`
	// RecentreM moves the local frame after this offset (default 5000 m).
	RecentreM float64 `json:"recentreM"`
	// SyncTauS is the correlation time of clock-sync errors (default 15 s).
	SyncTauS float64 `json:"syncTauS"`
}

// AggMLATEngine tunes the MLAT engine root (internal/mlat, DESIGN §9,
// §20.3). Zero = default.
type AggMLATEngine struct {
	// Workers is the number of processing goroutines (default GOMAXPROCS,
	// 1..64).
	Workers int `json:"workers"`
	// QueueLen is the per-worker queue length in groups (default 4096); a
	// full queue drops the group and counts it.
	QueueLen int `json:"queueLen"`
	// PublishIntervalMs is the shortest interval between two map updates of
	// one aircraft (default 500 ms).
	PublishIntervalMs int `json:"publishIntervalMs"`
	// MaxSolveSites caps the stations used in one solve (default 16, 4..64).
	MaxSolveSites int `json:"maxSolveSites"`
	// ContribWindowSec: a station receives an aircraft's results only when
	// it contributed to its track within this window (default 60 s,
	// DESIGN §8.4).
	ContribWindowSec int `json:"contribWindowSec"`
	// MinTimedPerMin is the timed frames per minute a synced station must
	// deliver to receive results (default 10).
	MinTimedPerMin int `json:"minTimedPerMin"`
	// HoldoutRotateMin rotates the hold-out set (default 10 minutes).
	HoldoutRotateMin int `json:"holdoutRotateMin"`
	// LatencyFeedback feeds hold-out fixes into the reference store's
	// position-latency estimate (default false; DESIGN §9.6 excludes it).
	LatencyFeedback bool `json:"latencyFeedback"`
	// JitterFeedback passes the jitter learned from solver residuals to the
	// clock filter (default false).
	JitterFeedback bool `json:"jitterFeedback"`
	// ApplyKAtm applies the hourly k_atm estimate to the solver and the
	// clock filter (default false: advisory only).
	ApplyKAtm bool `json:"applyKAtm"`
	// VerifyEverySec is the per-aircraft interval of the ADS-B TDOA
	// verification (default 5 s; -1 = off).
	VerifyEverySec int `json:"verifyEverySec"`
	// SyncWarmupSec holds sync observations back after the start (default
	// 60 s; -1 = none).
	SyncWarmupSec int `json:"syncWarmupSec"`
	// IngestGate gives the ingest the clock filter's gate view: synced
	// copies are checked against their network time (with a 50 µs floor)
	// and sync candidates are flushed early and re-delivered with every
	// copy (default false).
	IngestGate bool `json:"ingestGate"`
	// ModeACCodeFormat is the layout of the 2-byte Mode A/C payload
	// delivered by the feeders: "pulses" (default, "" = pulses: the reply
	// pulse order of the Beast description) or "nibbles" (4 octal digits).
	ModeACCodeFormat string `json:"modeAcCodeFormat"`
	// ModeACJitterNs is the extra timing noise of Mode A/C replies (default
	// 50 ns, up to 1000; negative = none).
	ModeACJitterNs float64 `json:"modeAcJitterNs"`
}

// AggMLATClock tunes the joint clock filter (internal/mlat/clock, DESIGN
// §9.2). The epoch and the component partition size stay in
// aggregator.mlat (epochMs, maxComponentStations). Zero = default.
type AggMLATClock struct {
	// HostSigmaS is the 1σ error of server-side host times used to start a
	// new clock epoch (default 0.05 s).
	HostSigmaS float64 `json:"hostSigmaS"`
	// PPSCorrelatedNs is the part of a GPS-class station's timestamp error
	// that is common to one second (1PPS discipline error; default 30 ns,
	// negative = off).
	PPSCorrelatedNs float64 `json:"ppsCorrelatedNs"`
	// ResetThetaSigmaNs is the smallest clock-offset 1σ after a detected
	// step (default 1000000 ns = 1 ms).
	ResetThetaSigmaNs float64 `json:"resetThetaSigmaNs"`
	// MaxMembers caps the stations used from one sync group (default 32,
	// 3..256).
	MaxMembers int `json:"maxMembers"`
	// SyncRatePerStation is the target number of sync groups per station and
	// second (default 6, up to 100).
	SyncRatePerStation float64 `json:"syncRatePerStation"`
	// MaxObsAgeS drops sync groups older than this at their epoch (default
	// 5 s).
	MaxObsAgeS float64 `json:"maxObsAgeS"`
	// StationTimeoutS removes a station from its component after this long
	// without an accepted observation (default 120 s).
	StationTimeoutS float64 `json:"stationTimeoutS"`
	// CoObsWindowS is the co-observation window that defines the connected
	// clock components (default 60 s).
	CoObsWindowS float64 `json:"coObsWindowS"`
	// ProbationS is the anchor probation of a GPS station (default 600 s).
	ProbationS float64 `json:"probationS"`
	// AnchorFreqMaxPPB is the frequency limit for anchor promotion (default
	// 2 ppb).
	AnchorFreqMaxPPB float64 `json:"anchorFreqMaxPpb"`
	// AnchorOffsetMaxNs is the offset limit against the other anchors for
	// promotion (default 200 ns).
	AnchorOffsetMaxNs float64 `json:"anchorOffsetMaxNs"`
	// AnchorDriftMaxNsPerH demotes an anchor that drifts faster (default 100
	// ns/h).
	AnchorDriftMaxNsPerH float64 `json:"anchorDriftMaxNsPerH"`
	// AnchorHostMaxS is how far a GPS station's clock may be from the
	// server's corrected time before it can become an anchor (default 0.4 s).
	AnchorHostMaxS float64 `json:"anchorHostMaxS"`
	// GateProb is the probability of the sync innovation gate (default
	// 0.999, below 1).
	GateProb float64 `json:"gateProb"`
	// WTestMax is the largest studentised innovation of an accepted sync
	// member (default 4.5).
	WTestMax float64 `json:"wTestMax"`
	// StepSigma is the outlier size, in σ, that counts towards a step
	// (default 4).
	StepSigma float64 `json:"stepSigma"`
	// StepCount consecutive same-sign outliers declare a clock step
	// (default 5).
	StepCount int `json:"stepCount"`
	// StepRunCount same-sign steps of a free clock within StepRunWindowS
	// also reset its frequency (default 3; -1 = off).
	StepRunCount int `json:"stepRunCount"`
	// StepRunWindowS is the window of StepRunCount (default 60 s).
	StepRunWindowS float64 `json:"stepRunWindowS"`
	// BlacklistS is how long an aircraft that keeps failing sync is not used
	// as a reference (default 600 s).
	BlacklistS float64 `json:"blacklistS"`
	// BlacklistMaxFrac caps the blacklisted share of the sync aircraft
	// (default 0.2, up to 1).
	BlacklistMaxFrac float64 `json:"blacklistMaxFrac"`
	// StationBadOutlier: a member whose outlier share is at least this does
	// not count towards an aircraft's failure (default 0.25, up to 1).
	StationBadOutlier float64 `json:"stationBadOutlier"`
	// SyncedMinObs is the accepted observations in SyncedWindowS a station
	// needs to count as synced (default 5).
	SyncedMinObs int `json:"syncedMinObs"`
	// SyncedWindowS is the window of SyncedMinObs (default 30 s).
	SyncedWindowS float64 `json:"syncedWindowS"`
	// Workers is the number of goroutines that update clock components in
	// parallel (default GOMAXPROCS, up to 64).
	Workers int `json:"workers"`
	// NoJitterLearning turns off the on-line learning of receiver jitter
	// (default false).
	NoJitterLearning bool `json:"noJitterLearning"`
	// ReoriginS moves the filter's internal time origin forward after this
	// many seconds (default 86400, at least 10).
	ReoriginS float64 `json:"reoriginS"`
	// RelGaugeBetaSigmaNs is the β_short gauge of relative components
	// without GPS-class clocks (default 10 ns; negative = no β gauge).
	RelGaugeBetaSigmaNs float64 `json:"relGaugeBetaSigmaNs"`
	// ExternalInflate scales the variance of a member from another
	// partition (default 2, at least 1).
	ExternalInflate float64 `json:"externalInflate"`
	// PartitionStations is the largest partition of a partitioned time
	// domain (default min(100, maxComponentStations); 4..2000).
	PartitionStations int `json:"partitionStations"`
	// ExternalStates caps the cross-partition members per partition and
	// epoch that are estimated as shared states (default 64, up to 1024;
	// negative = off).
	ExternalStates int `json:"externalStates"`
	// PartitionDriftNs is the common-mode random walk (ns/sqrt(s)) of the
	// partitions that do not carry the gauge (default 1, up to 100;
	// negative = off).
	PartitionDriftNs float64 `json:"partitionDriftNs"`
}

// AggMLATC tunes the mlat-client compatible listener on MLATPort
// (internal/ingest/mlatc, DESIGN §5.2). The rate and connection limits are
// in AggLimits. Zero = default.
type AggMLATC struct {
	// SplitSync is the split_sync policy: "auto" (default, "" = auto:
	// offered, with a per-client fallback to sync pairs), "on" (always
	// offered) or "off" (never; the reference protocol's default). The old
	// boolean form is accepted: true = "on", false = "auto" (DESIGN §5.2).
	SplitSync SplitSyncSetting `json:"splitSync"`
	// MaxRequested caps the aircraft one client is asked to send (default
	// 2000).
	MaxRequested int `json:"maxRequested"`
	// MaxSeen caps a client's remembered seen set (default 10000).
	MaxSeen int `json:"maxSeen"`
	// MaxBadLines disconnects a client after this many malformed lines
	// (default 200).
	MaxBadLines int `json:"maxBadLines"`
	// IdleTimeoutSec drops a silent client (default 150 s).
	IdleTimeoutSec int `json:"idleTimeoutSec"`
	// HeartbeatSec is the server heartbeat period (default 30 s).
	HeartbeatSec int `json:"heartbeatSec"`
	// StatsSec is the period of stats messages (default 60 s).
	StatsSec int `json:"statsSec"`
	// OverLimitSeconds above the rate limit disconnect a client (default 5).
	OverLimitSeconds int `json:"overLimitSeconds"`
	// CoolDownSec refuses a disconnected rate offender this long (default
	// 300 s).
	CoolDownSec int `json:"coolDownSec"`
	// MaxClients caps concurrent connections (default 4096).
	MaxClients int `json:"maxClients"`
	// MaxOutQueueKB is the per-client output queue (default 256 KiB).
	MaxOutQueueKB int `json:"maxOutQueueKB"`
	// UDPHost is the host told to clients for the UDP transport ("" = the
	// local address of the client's TCP connection; set it behind NAT or
	// in a port-mapped container). Needs aggregator.mlat.udp.
	UDPHost string `json:"udpHost"`
	// UDPPort is the externally reachable UDP port told to clients (0 = the
	// bound UDP port, 1..65535).
	UDPPort int `json:"udpPort"`
	// UDPListen is the datagram listen address ("" = the TCP listener's
	// address and port).
	UDPListen string `json:"udpListen"`
	// UDPAnySource accepts datagrams from any source address (default
	// false: only from the address of the client's TCP connection; needed
	// behind carrier-grade NAT).
	UDPAnySource bool `json:"udpAnySource"`
	// UDPReaders is the number of goroutines reading the datagram socket
	// (default min(4, GOMAXPROCS), up to 64).
	UDPReaders int `json:"udpReaders"`
	// ModeACAllClients sends Mode A/C requests also to clients that did not
	// announce "modeac" in their handshake (default false).
	ModeACAllClients bool `json:"modeAcAllClients"`
}

// nonNeg resets negative values to 0 (= default) with a note.
func nonNegI(n *notes, path string, v *int) {
	if *v < 0 {
		n.addf("%s %d is negative; the default is used", path, *v)
		*v = 0
	}
}

func nonNegF(n *notes, path string, v *float64) {
	if *v < 0 {
		n.addf("%s %g is negative; the default is used", path, *v)
		*v = 0
	}
}

// normalizeMLATTuning checks the tunable blocks of the MLAT parts.
func (a *Aggregator) normalizeMLATTuning(n *notes) {
	p := "aggregator.mlat."
	r := &a.MLAT.Ref
	for _, f := range []struct {
		k string
		v *float64
	}{{"qHorizontal", &r.QHorizontal}, {"qVertical", &r.QVertical}, {"maxPosAgeS", &r.MaxPosAgeS},
		{"latencySigmaS", &r.LatencySigmaS}, {"gateChi2", &r.GateChi2}, {"gateFloorM", &r.GateFloorM},
		{"probationS", &r.ProbationS}, {"suspendS", &r.SuspendS}, {"trackTimeoutS", &r.TrackTimeoutS},
		{"field.cellKm", &r.Field.CellKm}, {"field.bandFt", &r.Field.BandFt},
		{"field.driftMPerH", &r.Field.DriftMPerH}, {"field.obsSigmaM", &r.Field.ObsSigmaM}} {
		nonNegF(n, p+"ref."+f.k, f.v)
	}
	nonNegI(n, p+"ref.window", &r.Window)
	for _, f := range []struct {
		k string
		v *int
	}{{"minNacp", &r.MinNACp}, {"minNic", &r.MinNIC}} {
		if *f.v < 0 || *f.v > 11 {
			n.addf("%sref.%s %d is out of range 0..11; the default is used", p, f.k, *f.v)
			*f.v = 0
		}
	}

	g := &a.MLAT.Group
	for _, f := range []struct {
		k string
		v *float64
	}{{"nSigma", &g.NSigma}, {"slackM", &g.SlackM}, {"hintSlackM", &g.HintSlackM},
		{"hintMaxSigmaM", &g.HintMaxSigmaM}, {"colocatedM", &g.ColocatedM}} {
		nonNegF(n, p+"group."+f.k, f.v)
	}
	for _, f := range []struct {
		k string
		v *int
	}{{"minSites", &g.MinSites}, {"minSitesAlt", &g.MinSitesAlt}, {"maxParts", &g.MaxParts},
		{"lateHoldMs", &g.LateHoldMs}, {"lateSlots", &g.LateSlots}} {
		nonNegI(n, p+"group."+f.k, f.v)
	}

	pl := &a.MLAT.Plan
	for _, f := range []struct {
		k string
		v *int
	}{{"syncSetMin", &pl.SyncSetMin}, {"syncSetMax", &pl.SyncSetMax}, {"mlatSetMax", &pl.MLATSetMax},
		{"stationsPerAircraft", &pl.StationsPerAircraft}, {"minStationsPerAircraft", &pl.MinStationsPerAircraft},
		{"heardTtlS", &pl.HeardTTLS}, {"adsbGoodAgeS", &pl.ADSBGoodAgeS}, {"minDwellS", &pl.MinDwellS},
		{"maxChurn", &pl.MaxChurn}, {"replanEveryS", &pl.ReplanEveryS}} {
		nonNegI(n, p+"plan."+f.k, f.v)
	}
	for _, f := range []struct {
		k string
		v *float64
	}{{"stationFpsBudget", &pl.StationFPSBudget}, {"targetSigmaM", &pl.TargetSigmaM},
		{"acquireHz", &pl.AcquireHz}, {"trackedHz", &pl.TrackedHz}, {"holdOutHz", &pl.HoldOutHz},
		{"suspectHz", &pl.SuspectHz}, {"globalSolveHz", &pl.GlobalSolveHz}} {
		nonNegF(n, p+"plan."+f.k, f.v)
	}

	s := &a.MLAT.Solve
	for _, f := range []struct {
		k string
		v *float64
	}{{"huberK", &s.HuberK}, {"pfa", &s.PFA}, {"maxSigmaHM", &s.MaxSigmaHM}, {"altRejectSigma", &s.AltRejectSigma},
		{"priorSeedMaxSigmaM", &s.PriorSeedMaxSigmaM}, {"gridStepM", &s.GridStepM}, {"gridHalfM", &s.GridHalfM},
		{"maxSpeedMps", &s.MaxSpeedMps}, {"machFactor", &s.MachFactor}, {"hostTolS", &s.HostTolS},
		{"horizonSlackM", &s.HorizonSlackM}, {"maxHaeM", &s.MaxHAEM}, {"maxRangeM", &s.MaxRangeM}, {"relinM", &s.RelinM}} {
		nonNegF(n, p+"solve."+f.k, f.v)
	}
	if s.PFA >= 0.5 {
		n.addf("%ssolve.pfa %g is out of range 0..0.5; the default is used", p, s.PFA)
		s.PFA = 0
	}
	for _, f := range []struct {
		k string
		v *int
	}{{"maxIter", &s.MaxIter}, {"ransacMinStations", &s.RANSACMinStations}, {"ransacMaxSubsets", &s.RANSACMaxSubsets}} {
		nonNegI(n, p+"solve."+f.k, f.v)
	}
	if s.MaxHAEM != 0 && s.MinHAEM >= s.MaxHAEM {
		n.addf("%ssolve.minHaeM %g is not below maxHaeM %g; the defaults are used", p, s.MinHAEM, s.MaxHAEM)
		s.MinHAEM, s.MaxHAEM = 0, 0
	}

	t := &a.MLAT.Track
	for _, f := range []struct {
		k string
		v *float64
	}{{"qCv", &t.QCv}, {"qCt", &t.QCt}, {"qVert", &t.QVert}, {"qOmega", &t.QOmega}, {"omegaTauS", &t.OmegaTauS},
		{"sojournCvS", &t.SojournCvS}, {"sojournCtS", &t.SojournCtS}, {"initPCt", &t.InitPCt},
		{"initSigmaVelH", &t.InitSigmaVelH}, {"initSigmaVelU", &t.InitSigmaVelU}, {"initSigmaOmega", &t.InitSigmaOmega},
		{"gateProb", &t.GateProb}, {"confirmWindowS", &t.ConfirmWindowS}, {"coastS", &t.CoastS},
		{"maxSigmaHM", &t.MaxSigmaHM}, {"maxSpeedMS", &t.MaxSpeedMS}, {"hypLR", &t.HypLR}, {"losMarginM", &t.LOSMarginM},
		{"maxLagS", &t.MaxLagS}, {"excessRefreshM", &t.ExcessRefreshM}, {"recentreM", &t.RecentreM}, {"syncTauS", &t.SyncTauS}} {
		nonNegF(n, p+"track."+f.k, f.v)
	}
	for _, f := range []struct {
		k string
		v *int
	}{{"iterations", &t.Iterations}, {"confirmUpdates", &t.ConfirmUpdates}, {"maxHyp", &t.MaxHyp},
		{"respawnMisses", &t.RespawnMisses}, {"maxMembers", &t.MaxMembers}} {
		nonNegI(n, p+"track."+f.k, f.v)
	}
	if t.InitPCt >= 1 {
		n.addf("%strack.initPCt %g is out of range 0..1; the default is used", p, t.InitPCt)
		t.InitPCt = 0
	}
	if t.GateProb >= 1 {
		n.addf("%strack.gateProb %g is out of range 0..1; the default is used", p, t.GateProb)
		t.GateProb = 0
	}

	e := &a.MLAT.Engine
	for _, f := range []struct {
		k string
		v *int
	}{{"workers", &e.Workers}, {"queueLen", &e.QueueLen}, {"publishIntervalMs", &e.PublishIntervalMs},
		{"maxSolveSites", &e.MaxSolveSites}, {"contribWindowSec", &e.ContribWindowSec},
		{"minTimedPerMin", &e.MinTimedPerMin}, {"holdoutRotateMin", &e.HoldoutRotateMin}} {
		nonNegI(n, p+"engine."+f.k, f.v)
	}
	if e.Workers > 64 {
		n.addf("%sengine.workers %d is above 64; 64 is used", p, e.Workers)
		e.Workers = 64
	}
	for _, f := range []struct {
		k string
		v *int
	}{{"verifyEverySec", &e.VerifyEverySec}, {"syncWarmupSec", &e.SyncWarmupSec}} {
		if *f.v < -1 {
			n.addf("%sengine.%s %d is below -1 (off); the default is used", p, f.k, *f.v)
			*f.v = 0
		}
	}

	ck := &a.MLAT.Clock
	for _, f := range []struct {
		k string
		v *float64
	}{{"hostSigmaS", &ck.HostSigmaS}, {"resetThetaSigmaNs", &ck.ResetThetaSigmaNs},
		{"syncRatePerStation", &ck.SyncRatePerStation}, {"maxObsAgeS", &ck.MaxObsAgeS},
		{"stationTimeoutS", &ck.StationTimeoutS}, {"coObsWindowS", &ck.CoObsWindowS}, {"probationS", &ck.ProbationS},
		{"anchorFreqMaxPpb", &ck.AnchorFreqMaxPPB}, {"anchorOffsetMaxNs", &ck.AnchorOffsetMaxNs},
		{"anchorDriftMaxNsPerH", &ck.AnchorDriftMaxNsPerH}, {"anchorHostMaxS", &ck.AnchorHostMaxS},
		{"gateProb", &ck.GateProb}, {"wTestMax", &ck.WTestMax}, {"stepSigma", &ck.StepSigma},
		{"stepRunWindowS", &ck.StepRunWindowS}, {"blacklistS", &ck.BlacklistS},
		{"blacklistMaxFrac", &ck.BlacklistMaxFrac}, {"stationBadOutlier", &ck.StationBadOutlier},
		{"syncedWindowS", &ck.SyncedWindowS}} {
		nonNegF(n, p+"clock."+f.k, f.v)
	}
	for _, f := range []struct {
		k string
		v *int
	}{{"maxMembers", &ck.MaxMembers}, {"stepCount", &ck.StepCount},
		{"syncedMinObs", &ck.SyncedMinObs}, {"workers", &ck.Workers}} {
		nonNegI(n, p+"clock."+f.k, f.v)
	}
	for _, f := range []struct {
		k   string
		v   *float64
		max float64
		inc bool // the maximum itself is allowed
	}{{"gateProb", &ck.GateProb, 1, false}, {"blacklistMaxFrac", &ck.BlacklistMaxFrac, 1, true},
		{"stationBadOutlier", &ck.StationBadOutlier, 1, true}, {"syncRatePerStation", &ck.SyncRatePerStation, 100, true}} {
		if *f.v > f.max || (!f.inc && *f.v == f.max) {
			n.addf("%sclock.%s %g is out of range 0..%g; the default is used", p, f.k, *f.v, f.max)
			*f.v = 0
		}
	}
	if ck.MaxMembers != 0 && (ck.MaxMembers < 3 || ck.MaxMembers > 256) {
		n.addf("%sclock.maxMembers %d is out of range 3..256; the default is used", p, ck.MaxMembers)
		ck.MaxMembers = 0
	}
	if ck.Workers > 64 {
		n.addf("%sclock.workers %d is above 64; 64 is used", p, ck.Workers)
		ck.Workers = 64
	}

	c := &a.MLATC
	for _, f := range []struct {
		k string
		v *int
	}{{"maxRequested", &c.MaxRequested}, {"maxSeen", &c.MaxSeen}, {"maxBadLines", &c.MaxBadLines},
		{"idleTimeoutSec", &c.IdleTimeoutSec}, {"heartbeatSec", &c.HeartbeatSec}, {"statsSec", &c.StatsSec},
		{"overLimitSeconds", &c.OverLimitSeconds}, {"coolDownSec", &c.CoolDownSec}, {"maxClients", &c.MaxClients},
		{"maxOutQueueKB", &c.MaxOutQueueKB}} {
		nonNegI(n, "aggregator.mlatc."+f.k, f.v)
	}
	c.UDPHost = strings.TrimSpace(c.UDPHost)
	c.UDPListen = strings.TrimSpace(c.UDPListen)
	if c.UDPPort < 0 || c.UDPPort > 65535 {
		n.addf("aggregator.mlatc.udpPort %d is out of range 0..65535; the default is used", c.UDPPort)
		c.UDPPort = 0
	}
	if c.UDPReaders < 0 || c.UDPReaders > 64 {
		n.addf("aggregator.mlatc.udpReaders %d is out of range 0..64; the default is used", c.UDPReaders)
		c.UDPReaders = 0
	}
	a.normalizeWave3(n)
	a.normalizeLatencyCal(n)
	a.normalizeShareLinks(n)
}
