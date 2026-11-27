package ingest

import (
	"errors"
	"log/slog"
	"time"

	"airview/internal/model"
	"airview/internal/tracker"
)

// Options configures an Ingest. Zero values select the defaults.
type Options struct {
	// Handler is the global tracker (required). Ingest calls Handle for the
	// first copy of every transmission, Lanes().Credit for later copies,
	// Handle with Late set for backfilled frames and HandleUpdate for
	// pre-decoded updates.
	Handler tracker.NetHandler
	// Resolver gives the stations' admission state (registry Resolver).
	// nil = every station is treated as approved.
	Resolver model.StationResolver
	// MLAT receives closed timed groups (nil = none; phase 2 wires the MLAT
	// engine). If it also implements LateObserver it receives late copies.
	MLAT MLATSink
	// Clock is the clock filter's view for the synced-station rule and sync
	// candidates (nil = not used).
	Clock ClockView
	// ClockObserver receives the stations' clock records (nil = none).
	ClockObserver ClockObserver
	// LearnExclude (optional) names aircraft whose squitters must not train
	// the stations' latency estimates: the MLAT hold-out set (DESIGN §9.6).
	// Called from the sweeper with the 24-bit address and the sweep time;
	// it must be cheap and safe for concurrent use.
	LearnExclude func(addr uint32, nowNs int64) bool
	// ModeAC (optional) receives every timed Mode A/C reply (FrameModeAC,
	// 2 bytes) of an approved station with timestamps allowed, with the
	// station's own ticks, before grouping (DESIGN §9.9; the MLAT engine's
	// ObserveModeACFrames). Called from the receive goroutines; it must be
	// cheap and must not retain f.
	ModeAC func(f model.RawFrame)
	// Now is the server clock (default time.Now; use ntp.Now in production).
	Now func() time.Time
	// Logger (default slog.Default()).
	Logger *slog.Logger

	// Window is how long after its first copy a group accepts new copies
	// (default 1 s, DESIGN §6.2).
	Window time.Duration
	// FlushAfter is when a group is handed to the MLAT sink, after its first
	// copy's corrected time (default 400 ms, 100 ms..Window). Copies that
	// arrive later within Window are delivered through LateObserver.
	FlushAfter time.Duration
	// Flush (optional) replaces the fixed FlushAfter by a per-aircraft delay
	// (DESIGN §9.3 adaptive flush; internal/mlat/group.FlushEstimator). Its
	// delay is clamped to 100 ms..Window. The ingest feeds it the delivery
	// latency of the copies of every FlushSampleEvery-th group and, when it
	// implements HearerNoter, their stations.
	Flush FlushPolicy
	// FlushSampleEvery: one group in this many (by payload hash) trains the
	// FlushPolicy (default 4; 1 = every group).
	FlushSampleEvery int
	// ClockGateFloor is the smallest network-time tolerance of the
	// synced-station gate (DESIGN §6.2): two synced copies join a group
	// when |Δt_net| <= d/c + max(5σ, ClockGateFloor) (default 50 µs). The
	// gate exists to separate identical transmissions, which are at least
	// one reply length (64 µs) apart; with a tolerance of 5σ alone it also
	// removed the copies of a clock that had started to drift, so the clock
	// filter never saw the evidence it needed (HOOKS WP-M-engine ->
	// internal/ingest item 1). Negative = no floor.
	ClockGateFloor time.Duration
	// MinHold is the minimum time a group stays open after it was created on
	// the server (default 100 ms): a group opened by a slow station still
	// waits briefly for the others.
	MinHold time.Duration
	// Retain keeps a group for de-duplication after Window (default 1 s):
	// copies arriving then are credited to their lanes but not grouped anew.
	Retain time.Duration
	// MaxMatch caps the corrected-time tolerance of a match (default 300 ms;
	// it must stay below the shortest repeat interval of identical
	// transmissions, about 0.8 s for DF11).
	MaxMatch time.Duration
	// MinMatch is the smallest tolerance (default 10 ms).
	MinMatch time.Duration
	// UniqueSkew is the largest corrected-time difference at which a
	// position squitter (a payload that does not repeat) joins a group even
	// though the stations' time correction is not calibrated (default 2.5 s).
	UniqueSkew time.Duration
	// LateAfter routes frames whose corrected time is older than this to the
	// lanes only, like backfill (default Window + Retain + 500 ms).
	LateAfter time.Duration
	// Shards is the number of grouper shards (power of two, default 64).
	Shards int
	// SweepEvery is the sweeper period of Run (default 10 ms).
	SweepEvery time.Duration

	// Limits are the per-station and global limits (DESIGN §4.6; default
	// model.DefaultAggregator().Limits).
	Limits model.AggLimits
	// FlowHold and FlowGrace tune the FLOW rules (defaults 5 s and 10 s).
	FlowHold, FlowGrace time.Duration
	// ReduceMs is the reduce interval of selective mode (default 500).
	ReduceMs int
}

// OptionsFromConfig maps the aggregator configuration onto Options. Handler,
// Resolver, MLAT, Clock, ClockObserver, Now and Logger are set by the caller.
func OptionsFromConfig(a model.Aggregator) Options {
	g := a.Ingest
	ms := func(v int) time.Duration { return time.Duration(v) * time.Millisecond }
	return Options{
		Limits:     a.Limits,
		Window:     ms(g.WindowMs),
		FlushAfter: ms(g.FlushAfterMs),
		Retain:     ms(g.RetainMs),
		MaxMatch:   ms(g.MaxMatchMs),
		MinMatch:   ms(g.MinMatchMs),
		UniqueSkew: ms(g.UniqueSkewMs),
		LateAfter:  ms(g.LateAfterMs), // 0 = derived default
		Shards:     g.Shards,
		ReduceMs:   g.ReduceMs,
		FlowHold:   time.Duration(g.FlowHoldSec) * time.Second,
		FlowGrace:  time.Duration(g.FlowGraceSec) * time.Second,
		// 0 = default (50 µs), negative = no floor (withDefaults).
		ClockGateFloor:   time.Duration(g.ClockGateFloorUs) * time.Microsecond,
		FlushSampleEvery: g.FlushSampleEvery,
	}
}

func (o Options) withDefaults() (Options, error) {
	if o.Handler == nil {
		return o, errors.New("ingest: Options.Handler is required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Window, time.Second)
	def(&o.FlushAfter, 400*time.Millisecond)
	o.FlushAfter = min(max(o.FlushAfter, 100*time.Millisecond), o.Window)
	def(&o.MinHold, 100*time.Millisecond)
	def(&o.Retain, time.Second)
	def(&o.MaxMatch, 300*time.Millisecond)
	def(&o.MinMatch, 10*time.Millisecond)
	o.MinMatch = min(o.MinMatch, o.MaxMatch)
	def(&o.UniqueSkew, 2500*time.Millisecond)
	def(&o.LateAfter, o.Window+o.Retain+500*time.Millisecond)
	def(&o.SweepEvery, 10*time.Millisecond)
	if o.FlushSampleEvery <= 0 {
		o.FlushSampleEvery = 4
	}
	switch {
	case o.ClockGateFloor == 0:
		o.ClockGateFloor = 50 * time.Microsecond
	case o.ClockGateFloor < 0:
		o.ClockGateFloor = 0
	}
	def(&o.FlowHold, 5*time.Second)
	def(&o.FlowGrace, 10*time.Second)
	if o.Shards <= 0 {
		o.Shards = 64
	}
	n := 1
	for n < o.Shards {
		n <<= 1
	}
	o.Shards = min(n, 4096)
	if o.ReduceMs <= 0 {
		o.ReduceMs = 500
	}
	dl := model.DefaultAggregator().Limits
	if o.Limits.IngestFPS <= 0 && o.Limits.MaxFPSPerStation <= 0 && o.Limits.SelectiveAbove == 0 {
		o.Limits = dl // an unset block: all defaults (SelectiveAbove 0 would mean "always selective")
	}
	if o.Limits.IngestFPS <= 0 {
		o.Limits.IngestFPS = dl.IngestFPS
	}
	if o.Limits.MaxFPSPerStation <= 0 {
		o.Limits.MaxFPSPerStation = dl.MaxFPSPerStation
	}
	if o.Limits.MaxKBpsPerStation <= 0 {
		o.Limits.MaxKBpsPerStation = dl.MaxKBpsPerStation
	}
	if o.Limits.SelectiveAbove < 0 {
		o.Limits.SelectiveAbove = 0
	}
	return o, nil
}
