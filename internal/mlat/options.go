package mlat

import (
	"log/slog"
	"runtime"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/plan"
	"airview/internal/mlat/ref"
	"airview/internal/model"
)

// UpdateSink receives the engine's positions for the global tracker
// (ingest.Ingest.HandleUpdate in aggregator mode). Updates carry MLAT =
// true and Src = model.SrcMLATAV; uplinks never forward them (DESIGN §9.7).
type UpdateSink interface {
	HandleUpdate(u model.DecodedUpdate)
}

// ResultSender delivers MLAT results to one station's feeder. Both
// avfsrv.Server and mlatc.Server implement it; SendResults returns false
// when the station has no link of that kind (the engine then tries the
// next sender).
type ResultSender interface {
	SendResults(st model.StationIdx, fixes []model.MLATFix) bool
}

// SurveyRequester is the phase-3 seam to internal/mlat/survey: the engine
// asks for a reverse-MLAT survey of a station whose residuals are biased
// for an hour (DESIGN §9.6) or that has no usable position.
type SurveyRequester interface {
	RequestSurvey(st model.StationIdx, reason string)
}

// PositionChange describes a change of a station's effective MLAT antenna
// position (PositionNotifier). Old/New are the sources; ECEF and SigmaM
// the new effective position (zero when New is PosNone). AutoSurvey is set
// when the change is the automatic use of a survey (DESIGN §19.3: the owner
// is notified and can override with Engine.OverrideSurvey). The position
// is owner/admin data: never publish it (DESIGN §8).
type PositionChange struct {
	Station    model.StationIdx
	Old, New   PosSource
	ECEF       geo.Vec3
	SigmaM     float64
	AutoSurvey bool
	// Reason is "registry" (SetStation), "survey" (SetSurvey) or "override"
	// (OverrideSurvey).
	Reason string
}

// PositionNotifier receives the position changes (the owner notification
// of DESIGN §19.3; internal/engine turns them into owner notices). It is
// called synchronously from SetStation / SetSurvey / OverrideSurvey and
// must not call back into the engine.
type PositionNotifier interface {
	PositionChanged(c PositionChange)
}

// SurveySink is the seam the phase-3 survey (internal/mlat/survey) and the
// owner API use: survey results in, owner overrides in. *Engine
// implements it.
type SurveySink interface {
	// SetSurvey records a reverse-MLAT result (AutoUseSurvey decides
	// whether it becomes the MLAT position).
	SetSurvey(st model.StationIdx, sv Survey)
	// OverrideSurvey records the owner's decision on the automatic use of
	// the station's survey: reject = true keeps the survey out of the solver
	// geometry (the declared position is used), false allows it again.
	OverrideSurvey(st model.StationIdx, reject bool) bool
}

// Options configure an Engine. Zero values take the defaults.
type Options struct {
	// Config is the aggregator's MLAT block (model.DefaultAggregator().MLAT
	// when the caller has nothing better). Its sub-blocks are mapped onto
	// the parts with their own mappers (ref/group/plan/solve/track).
	Config model.AggMLAT

	// OriginUnixNs is the clock filter's network-time origin (default: Now
	// truncated to the second).
	OriginUnixNs int64
	// MaxAircraft bounds the planner's aircraft table (0 = the planner
	// default; production passes aggregator.limits.maxAircraft).
	MaxAircraft int
	// Now is the server clock (default time.Now; ntp.Now in production).
	Now func() time.Time
	// Logger (default slog.Default()).
	Logger *slog.Logger

	// Workers is the number of processing goroutines (default GOMAXPROCS,
	// at most 64). Aircraft are partitioned over the workers by a hash of
	// their address; each worker owns its cluster assembler, solver and
	// tracker partition, so nothing on the per-cluster path is shared.
	// Workers < 0 processes every group synchronously inside Observe (tests,
	// deterministic replays).
	Workers int
	// QueueLen is the per-worker queue length in groups (default 4096).
	// When a worker's queue is full the group is dropped and counted
	// (backpressure never reaches the ingest's sweeper).
	QueueLen int

	// Updates receives positions for the map (nil = none).
	Updates UpdateSink
	// Results deliver results to contributing feeders (avfsrv, mlatc).
	Results []ResultSender
	// PlanOut receives the planner's interest-set changes (plan.Fanout of
	// plan.IngestOut and plan.MLATCOut; nil = none).
	PlanOut plan.Out
	// Weather is the GNSS−baro field's surface-weather fallback (nil =
	// geoid only).
	Weather ref.Weather
	// Survey receives survey requests (phase 3; nil = none).
	Survey SurveyRequester
	// OnPosition (optional) is told when a station's effective MLAT position
	// source changes (e.g. a survey position was auto-used, DESIGN §19.3: the
	// owner is notified).
	OnPosition func(st model.StationIdx, src PosSource)
	// Notifier (optional) receives the full position change (old and new
	// source, position, σ, whether a survey was auto-used) for the owner
	// notification; called after OnPosition.
	Notifier PositionNotifier

	// Atmosphere overrides the refraction model built from the config's
	// refractivity settings (zero = from Config).
	Atmosphere geo.Atmosphere

	// ClockLag is how far behind real time the clock filter steps (default
	// 1 s: sync groups are flushed 400 ms after reception and late copies
	// arrive up to 1 s later).
	ClockLag time.Duration
	// PublishInterval is the shortest interval between two map updates of
	// one aircraft (default 500 ms; DESIGN §9.5 "1 Hz or on update").
	PublishInterval time.Duration
	// MaxSolveSites caps the sites used in one solve (default 16; the
	// lowest-variance sites are kept, beyond ~16 the gain is negligible
	// and the cost grows linearly).
	MaxSolveSites int
	// ContribWindow and MinTimedPerMin are the minimum-contribution rule of
	// DESIGN §8.4: a station receives an aircraft's results only when it
	// contributed to the track within ContribWindow (default 60 s) and
	// delivered at least MinTimedPerMin timed frames per minute while
	// synced (default 10).
	ContribWindow  time.Duration
	MinTimedPerMin int

	// HoldoutRotate rotates the hold-out set (default 10 min); HoldoutSalt
	// keys its hash (default derived from OriginUnixNs).
	HoldoutRotate time.Duration
	HoldoutSalt   uint64
	// LatencyFeedback feeds hold-out fixes back into the reference store's
	// position-latency estimate (ref.Store.ObserveFix). DESIGN §9.6 excludes
	// hold-out aircraft from latency calibration, because the feedback makes
	// the hold-out statistic optimistic; default off.
	LatencyFeedback bool
	// JitterFeedback passes the jitter learned from solver residuals
	// (mlat/quality) to the clock filter's station prior. The clock filter
	// learns jitter from its sync innovations as well; default off.
	JitterFeedback bool
	// ApplyKAtm applies the hourly k_atm estimate to the solver and the
	// clock filter's successor snapshots (default off: advisory, see
	// quality.KAtm).
	ApplyKAtm bool
	// VerifyEvery is the per-aircraft interval of the ADS-B TDOA
	// verification (default 5 s; < 0 = off).
	VerifyEvery time.Duration
	// SyncWarmup holds sync observations back for this long after the
	// first Advance (default 60 s): the reference store needs a minute to
	// confirm enough ADS-B tracks, and a clock filter started on a handful
	// of unconfirmed references can lock SDR clocks into a wrong solution
	// (measured on simnet, see doc.go). < 0 = none.
	SyncWarmup time.Duration
	// IngestGate gives the ingest the clock filter's GateView (synced-station
	// network-time gate with a 50 µs floor, early flush of sync candidates
	// with a later re-delivery) instead of the neutral view (default off;
	// H-clock, see ClockView).
	IngestGate bool

	// ModeACCodeFormat is the layout of received 2-byte Mode A/C payloads
	// (ModeACPulses, the default, or ModeACNibbles); see
	// ModeACReplyFromFrame. Only used with Config.ModeAC.
	ModeACCodeFormat string
	// ModeACJitterNs is the extra timing noise of Mode A/C replies (1σ, ns;
	// 0 = ModeACJitterNs, < 0 = none).
	ModeACJitterNs float64
	// ModeACOut (optional) is told when a station starts or stops taking
	// part in Mode A/C MLAT, so that its link can ask for Mode A/C replies
	// (plan.MLATCModeAC for mlat-client sessions).
	ModeACOut plan.ModeACOut

	noBlacklistSuspend bool // tests: do not wire the clock blacklist to the reference store
}

// applyConfig fills the tunables left at zero from the config file's
// aggregator.mlat.engine block (zero there = the package default; −1 turns
// VerifyEvery / SyncWarmup off). Explicit Options values win.
func (o Options) applyConfig() Options {
	c := o.Config.Engine
	sec := func(v int) time.Duration { return time.Duration(v) * time.Second }
	if o.Workers == 0 && c.Workers > 0 {
		o.Workers = c.Workers
	}
	if o.QueueLen == 0 && c.QueueLen > 0 {
		o.QueueLen = c.QueueLen
	}
	if o.PublishInterval == 0 && c.PublishIntervalMs > 0 {
		o.PublishInterval = time.Duration(c.PublishIntervalMs) * time.Millisecond
	}
	if o.MaxSolveSites == 0 && c.MaxSolveSites > 0 {
		o.MaxSolveSites = c.MaxSolveSites
	}
	if o.ContribWindow == 0 && c.ContribWindowSec > 0 {
		o.ContribWindow = sec(c.ContribWindowSec)
	}
	if o.MinTimedPerMin == 0 && c.MinTimedPerMin > 0 {
		o.MinTimedPerMin = c.MinTimedPerMin
	}
	if o.HoldoutRotate == 0 && c.HoldoutRotateMin > 0 {
		o.HoldoutRotate = time.Duration(c.HoldoutRotateMin) * time.Minute
	}
	o.LatencyFeedback = o.LatencyFeedback || c.LatencyFeedback
	o.JitterFeedback = o.JitterFeedback || c.JitterFeedback
	o.ApplyKAtm = o.ApplyKAtm || c.ApplyKAtm
	o.IngestGate = o.IngestGate || c.IngestGate
	if o.VerifyEvery == 0 && c.VerifyEverySec != 0 {
		o.VerifyEvery = sec(c.VerifyEverySec) // −1 s = off
	}
	if o.SyncWarmup == 0 && c.SyncWarmupSec != 0 {
		o.SyncWarmup = sec(c.SyncWarmupSec) // −1 s = none
	}
	if o.ModeACCodeFormat == "" {
		o.ModeACCodeFormat = c.ModeACCodeFormat
	}
	if o.ModeACJitterNs == 0 {
		o.ModeACJitterNs = c.ModeACJitterNs // < 0 = none
	}
	return o
}

func (o Options) withDefaults() Options {
	o = o.applyConfig()
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.OriginUnixNs == 0 {
		o.OriginUnixNs = o.Now().Truncate(time.Second).UnixNano()
	}
	if o.Workers == 0 {
		o.Workers = runtime.GOMAXPROCS(0)
	}
	o.Workers = min(o.Workers, 64)
	if o.QueueLen <= 0 {
		o.QueueLen = 4096
	}
	if o.ClockLag <= 0 {
		o.ClockLag = time.Second
	}
	if o.PublishInterval <= 0 {
		o.PublishInterval = 500 * time.Millisecond
	}
	if o.MaxSolveSites <= 0 {
		o.MaxSolveSites = 16
	}
	o.MaxSolveSites = min(max(o.MaxSolveSites, 4), 64)
	if o.ContribWindow <= 0 {
		o.ContribWindow = 60 * time.Second
	}
	if o.MinTimedPerMin <= 0 {
		o.MinTimedPerMin = 10
	}
	if o.HoldoutRotate <= 0 {
		o.HoldoutRotate = 10 * time.Minute
	}
	if o.HoldoutSalt == 0 {
		o.HoldoutSalt = uint64(o.OriginUnixNs) ^ 0x6d6c61742d686f6c
	}
	if o.SyncWarmup == 0 {
		o.SyncWarmup = 60 * time.Second
	}
	if o.SyncWarmup < 0 {
		o.SyncWarmup = 0
	}
	if o.VerifyEvery == 0 {
		o.VerifyEvery = 5 * time.Second
	}
	if o.Atmosphere == (geo.Atmosphere{}) {
		c := o.Config
		o.Atmosphere = geo.Atmosphere{N0: c.RefractivityN0, H: c.RefractivityScaleHeightM, K: 1}
	}
	o.Atmosphere = o.Atmosphere.Normalize()
	return o
}
