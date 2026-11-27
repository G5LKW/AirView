package mlat

import (
	"context"
	"errors"
	"math"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/plan"
	"airview/internal/mlat/quality"
	"airview/internal/mlat/ref"
	"airview/internal/model"
)

// Engine is AirView's own MLAT engine (DESIGN §9): it sits behind the
// ingest grouper (ingest.MLATSink, ingest.LateObserver, ingest.ClockView
// via ClockView, ingest.ClockObserver) and wires the reference tracks
// (mlat/ref), the clock filter (mlat/clock), the planner (mlat/plan), the
// cluster assembler (mlat/group), the solver (mlat/solve) and the tracker
// (mlat/track); it routes the results to the global tracker and to the
// contributing feeders, and runs the hold-out self-validation and the
// quality statistics (mlat/quality). See doc.go.
type Engine struct {
	o   Options
	cfg model.AggMLAT
	atm atomic.Pointer[geo.Atmosphere]

	peerIdx atomic.Pointer[peerIndex] // symmetric peer lists of the latest clock snapshot

	filter  *clock.Filter
	refs    *ref.Store
	planner *plan.Planner
	hold    *quality.HoldOut
	qFix    *quality.ErrorStats // hold-out: snapshot fixes
	qTrack  *quality.ErrorStats // hold-out: published track estimates
	cells   *quality.Cells
	stq     *quality.Stations
	verify  *quality.Verifier
	vbrk    verifyBreaker // network-wide contradiction breaker (SCALE-05)
	blame   blameTable    // contradictions charged to declared stations (blame.go)
	katm    *quality.KAtm
	latcal  *quality.LatencyCal // nil = aggregator.mlat.latencyCal off
	st      *stationTable
	pos     *posCache
	route   *router
	rates   *rateTable
	href    clock.RefSource // holdRef (tests may replace it)

	workers []*worker
	sync    bool
	syncMu  sync.Mutex // serialises Observe in synchronous mode
	wg      sync.WaitGroup
	closed  atomic.Bool
	fixes   chan model.MLATFix

	nowNs    atomic.Int64 // latest server time seen (Observe / Advance)
	firstNs  atomic.Int64 // first Advance (rates)
	syncFrom atomic.Int64 // sync observations are held back before this server time (reference warm-up)

	advMu      sync.Mutex
	nextEpochS float64
	nextReplan int64
	nextSecond int64
	nextMinute int64
	nextTen    int64
	started    bool

	modeAC atomic.Pointer[modeACState]
	wd     map[model.StationIdx]*wdRec // clock watchdog (Advance only)

	ctr counters
}

// counters are the engine-level counters (atomic).
type counters struct {
	groups, late, dropped, syncAdmitted, refObs   atomic.Uint64
	clusters, lateSkipped, solves, solveOK        atomic.Uint64
	solveErr, rejected, fixToTrack, clusterTrack  atomic.Uint64
	applied, gated, published, holdoutFix         atomic.Uint64
	holdoutTrack, verified, contradicted, suspect atomic.Uint64
	verifyCulprit                                 atomic.Uint64 // phase-4 verify blamed one station row
	advancePanics                                 atomic.Uint64 // recovered panics in Advance (safeAdvance)
	verifyMuted                                   atomic.Uint64 // contradictions not charged: network-wide breaker (verifybreaker.go)
	velocities, resultsSent, resultsRefused       atomic.Uint64
	fixesDropped, lateAfterAccept, deleted        atomic.Uint64
	restarts, blacklisted, clockResets            atomic.Uint64
	gateSolves, verifyBlamed                      atomic.Uint64
	grades                                        [5]atomic.Uint64
	rejects                                       [8]atomic.Uint64
	procNs                                        atomic.Int64
}

var (
	_ ingest.MLATSink      = (*Engine)(nil)
	_ ingest.LateObserver  = (*Engine)(nil)
	_ ingest.ClockObserver = (*Engine)(nil)
)

// New creates an engine. Workers start immediately; call Run (or Advance
// from a test driver) to step the clock filter, replan and publish, and
// Close to stop.
func New(o Options) (*Engine, error) {
	o = o.withDefaults()
	e := &Engine{o: o, cfg: o.Config, fixes: make(chan model.MLATFix, 4096)}
	atm := o.Atmosphere
	e.atm.Store(&atm)
	e.syncFrom.Store(math.MaxInt64) // until the first Advance
	var err error
	e.refs = ref.New(ref.OptionsFromConfig(o.Config.Ref), o.Weather)
	e.hold = quality.NewHoldOut(o.Config.Holdout, o.HoldoutRotate, o.HoldoutSalt, 9)
	e.href = holdRef{e: e, now: &e.nowNs}
	e.filter, err = clock.New(clock.ConfigFromModel(o.Config.Clock, clock.Config{
		OriginUnixNs:         o.OriginUnixNs,
		EpochS:               float64(o.Config.EpochMs) / 1000,
		MaxComponentStations: o.Config.MaxComponentStations,
		Atmosphere:           atm,
		OnBlacklist:          e.onBlacklist,
		Logf:                 func(f string, a ...any) { o.Logger.Debug("mlat clock: " + sprintf(f, a...)) },
		OnPanic: func(v any, stack []byte) {
			o.Logger.Error("mlat clock: filter panic recovered, every clock state reset", "panic", sprintf("%v", v), "stack", string(stack))
		},
	}))
	if err != nil {
		return nil, err
	}
	e.st = newStationTable(o.Config.AutoUseSurvey, autoMax(o.Config.AutoUseMaxSigmaM))
	if v := o.Config.Survey.SuspectMinM; v > 0 {
		e.st.suspectMin = v
	}
	e.pos = newPosCache()
	e.rates = newRateTable()
	e.route = newRouter(e)
	e.qFix, e.qTrack = &quality.ErrorStats{}, &quality.ErrorStats{}
	e.cells = quality.NewCells()
	e.stq = quality.NewStations()
	e.stq.OnFlag = func(st model.StationIdx, biasNs float64) {
		o.Logger.Info("mlat: station residual bias flagged", "station", st, "biasNs", int(biasNs))
		if o.Survey != nil {
			o.Survey.RequestSurvey(st, "residual bias")
		}
	}
	e.verify = quality.NewVerifierPhase4()
	e.katm = &quality.KAtm{}
	if lc := o.Config.LatencyCal; lc.Enabled {
		e.latcal = quality.NewLatencyCal(latencyCalParams(lc, o.HoldoutSalt))
	}
	pc := plan.ConfigFromModel(o.Config)
	if o.MaxAircraft > 0 {
		pc.MaxAircraft = o.MaxAircraft
		pc = pc.Normalize()
	}
	e.planner, err = plan.New(plan.Options{Config: pc, Stations: planStations{e}, Positions: planPositions{e},
		HoldOut: e.hold.In, Out: o.PlanOut, Now: o.Now, ModeACOut: o.ModeACOut})
	if err != nil {
		return nil, err
	}
	if o.Config.ModeAC {
		st, err := e.newModeAC()
		if err != nil {
			return nil, err
		}
		e.modeAC.Store(st)
	}
	n := o.Workers
	if n < 0 {
		e.sync = true
		n = 1
	}
	for i := 0; i < n; i++ {
		w, err := newWorker(e, i)
		if err != nil {
			return nil, err
		}
		e.workers = append(e.workers, w)
	}
	if !e.sync {
		for _, w := range e.workers {
			e.wg.Add(1)
			go w.loop(&e.wg)
		}
	}
	return e, nil
}

// onBlacklist is the clock filter's per-aircraft sync blacklist (§9.2.4):
// the reference store stops offering the aircraft.
func (e *Engine) onBlacklist(key uint32, untilNs int64) {
	e.ctr.blacklisted.Add(1)
	if !e.o.noBlacklistSuspend {
		e.refs.Suspend(key, untilNs)
	}
}

func autoMax(v float64) float64 {
	if v <= 0 {
		return 50
	}
	return v
}

// Filter returns the clock filter (read-only use: Snapshot, Stats; tests
// may feed GPS status).
func (e *Engine) Filter() *clock.Filter { return e.filter }

// Refs returns the reference store.
func (e *Engine) Refs() *ref.Store { return e.refs }

// Planner returns the planner (mlat-client OnSeen hooks: NoteHearer).
func (e *Engine) Planner() *plan.Planner { return e.planner }

// SurveyTarget tells the planner that st is (or no longer is) under
// reverse-MLAT survey, so its syncSet asks for spread-out reference
// aircraft (survey.Targets; plan.Planner.SetSurveyTarget).
func (e *Engine) SurveyTarget(st model.StationIdx, active bool) {
	if e.planner != nil {
		e.planner.SetSurveyTarget(st, active)
	}
}

// HoldOut returns the hold-out selector.
func (e *Engine) HoldOut() *quality.HoldOut { return e.hold }

// ClockView returns the ingest.ClockView of the engine (pass it as
// ingest.Options.Clock). It deliberately switches off the two ingest
// behaviours that feed back into the clock filter (measured on simnet,
// 4 GPS + 20 SDR, 14 simulated minutes; doc.go "Ingest interface"):
//
//   - NetTime always answers "not synced", so the ingest's synced-station
//     gate (|Δt_net| <= d/c + 5σ against the group's first synced copy)
//     never runs. The gate removes exactly the copies of a clock that has
//     started to drift beyond its σ, so the filter loses the evidence it
//     needs to correct it: 1,237 false clock steps with the gate (even with
//     the receiver jitter and the short/long frame-reference ambiguity
//     added to σ), 105 without, and the published P95 fell from 112 m to
//     84 m. Copies of repeated identical replies are still separated by the
//     ingest's corrected-time tolerance, and µs-level splitting is the
//     cluster assembler's job (mlat/group, exact cliques).
//   - SyncCandidate answers false: with true, a reference aircraft's group
//     with one synced copy is flushed at once and the other stations' copies
//     arrive as a late delivery that carries the new copies only, useless
//     for sync.
//
// BaselineNs is the filter's.
//
// With Options.IngestGate the ingest gets clock.GateView instead (frame
// kind and receiver jitter in σ, a 50 µs floor on the ingest side, and
// real sync candidates whose early single-copy flush is re-delivered with
// every copy, ingest.Group.Redelivery).
func (e *Engine) ClockView() ingest.ClockView {
	if e.o.IngestGate {
		return clock.GateView{F: e.filter, Ref: e.href}
	}
	return ingestView{View: clock.View{F: e.filter, Ref: e.href}}
}

type ingestView struct{ clock.View }

// SyncCandidate implements ingest.ClockView (always false, see ClockView).
func (v ingestView) SyncCandidate(uint32, uint8) bool { return false }

// NetTime implements ingest.ClockView (never ok, see ClockView).
func (v ingestView) NetTime(model.StationIdx, uint32, uint64) (float64, float64, bool) {
	return 0, 0, false
}

// Atmosphere returns the refraction model in use.
func (e *Engine) Atmosphere() geo.Atmosphere { return *e.atm.Load() }

// --- stations ---

var _ SurveySink = (*Engine)(nil)

// SetStation registers or updates a station (registry change).
func (e *Engine) SetStation(st model.StationIdx, in StationInfo) {
	ci, ch, changed, _ := e.st.set(st, &in, nil)
	e.filter.SetStation(st, ci)
	e.notify(ch, changed, "registry")
}

// SetSurvey records a reverse-MLAT survey result for a station (phase 3,
// mlat/survey; SurveySink). With AutoUseSurvey (default on, DESIGN §19.3) a
// stable, non-suspect survey with σ_h <= AutoUseMaxSigmaM becomes the
// station's MLAT position unless the station has GNSS or an owner-confirmed
// position or the owner rejected it; OnPosition and the Notifier are told.
// An OverrideSurvey rejection stays in force for later surveys of the
// station (it applies to the station, not to one survey run) until the owner
// lifts it; Survey.OwnerRejected rejects just this result.
func (e *Engine) SetSurvey(st model.StationIdx, sv Survey) {
	ci, ch, changed, ok := e.st.set(st, nil, &sv)
	if !ok {
		return // removed or unknown station (MLAT-R6)
	}
	e.filter.SetStation(st, ci)
	e.notify(ch, changed, "survey")
}

// OverrideSurvey records the owner's decision on the automatic use of the
// station's survey (DESIGN §19.3 "the owner is notified and can override";
// SurveySink): reject keeps the survey out of the solver geometry, false
// allows it again. It reports false for an unknown station.
func (e *Engine) OverrideSurvey(st model.StationIdx, reject bool) bool {
	ci, ch, changed, ok := e.st.override(st, reject)
	if !ok {
		return false
	}
	e.filter.SetStation(st, ci)
	e.notify(ch, changed, "override")
	return true
}

// SurveyOf returns the station's last survey (owner/admin views; never
// public, DESIGN §8).
func (e *Engine) SurveyOf(st model.StationIdx) (Survey, bool) { return e.st.surveyOf(st) }

// notify tells OnPosition and the Notifier about a source change.
func (e *Engine) notify(ch PositionChange, changed bool, reason string) {
	if !changed {
		return
	}
	ch.Reason = reason
	if e.o.OnPosition != nil {
		e.o.OnPosition(ch.Station, ch.New)
	}
	if e.o.Notifier != nil {
		e.o.Notifier.PositionChanged(ch)
	}
}

// RemoveStation forgets a station.
func (e *Engine) RemoveStation(st model.StationIdx) {
	e.st.remove(st)
	e.filter.RemoveStation(st)
}

// PositionSource returns the source of a station's effective MLAT position.
func (e *Engine) PositionSource(st model.StationIdx) PosSource { return e.st.source(st) }

// --- ingest.ClockObserver (delegated to the clock filter) ---

// ClockEvent implements ingest.ClockObserver.
func (e *Engine) ClockEvent(st model.StationIdx, ev ingest.ClockEvent) { e.filter.ClockEvent(st, ev) }

// ClockPair implements ingest.ClockObserver.
func (e *Engine) ClockPair(st model.StationIdx, p ingest.ClockPair) { e.filter.ClockPair(st, p) }

// ClockStatus implements ingest.ClockObserver.
func (e *Engine) ClockStatus(st model.StationIdx, s ingest.ClockStatus) {
	e.filter.ClockStatus(st, s)
}

// --- ingest.MLATSink ---

func (e *Engine) shard(key uint32) int {
	if len(e.workers) == 1 {
		return 0
	}
	// multiplicative hash: neighbouring addresses spread over workers
	return int((uint64(key) * 0x9e3779b97f4a7c15 >> 32) % uint64(len(e.workers)))
}

// Observe implements ingest.MLATSink. It never blocks: in asynchronous mode
// the group is copied into the owning worker's queue (dropped and counted
// when the queue is full).
func (e *Engine) Observe(g *ingest.Group) { e.observe(g, false) }

// ObserveLate implements ingest.LateObserver.
func (e *Engine) ObserveLate(g *ingest.Group) { e.observe(g, true) }

func (e *Engine) observe(g *ingest.Group, late bool) {
	if e.closed.Load() {
		return
	}
	if late {
		e.ctr.late.Add(1)
	} else {
		e.ctr.groups.Add(1)
	}
	for {
		n := e.nowNs.Load()
		if g.FirstNs <= n || e.nowNs.CompareAndSwap(n, g.FirstNs) {
			break
		}
	}
	w := e.workers[e.shard(g.Key)]
	if e.sync {
		e.syncMu.Lock()
		w.process(g, late)
		e.syncMu.Unlock()
		return
	}
	if !w.submit(g, late) {
		e.ctr.dropped.Add(1)
	}
}

// Fixes returns the stream of published results (map positions, incl.
// hold-out fixes flagged model.FixFlagHoldout). The channel is buffered;
// results are dropped (and counted) when nobody reads it.
func (e *Engine) Fixes() <-chan model.MLATFix { return e.fixes }

func (e *Engine) emitFix(f model.MLATFix) {
	select {
	case e.fixes <- f:
	default:
		e.ctr.fixesDropped.Add(1)
	}
}

// --- time driver ---

// Advance runs the time-driven work up to server time nowNs: clock filter
// epochs (ClockLag behind), planner replans, the 1 Hz track sweep and
// result delivery, the minute counters and expiries. Run calls it every
// 100 ms; a test driver calls it with virtual time. Safe for concurrent
// use with Observe.
func (e *Engine) Advance(nowNs int64) {
	e.advMu.Lock()
	defer e.advMu.Unlock()
	for {
		n := e.nowNs.Load()
		if nowNs <= n || e.nowNs.CompareAndSwap(n, nowNs) {
			break
		}
	}
	if !e.started {
		e.started = true
		e.firstNs.Store(nowNs)
		e.syncFrom.Store(nowNs + int64(e.o.SyncWarmup))
		ep := e.filter.Config().EpochS
		t := e.filter.Seconds(nowNs - int64(e.o.ClockLag))
		e.nextEpochS = (float64(int64(t/ep)) + 1) * ep
		e.nextReplan = nowNs + int64(e.planner.Config().ReplanEvery)/2
		e.nextSecond = nowNs + 1e9
		e.nextMinute = nowNs + 60e9
		e.nextTen = nowNs + 10e9
	}
	ep := e.filter.Config().EpochS
	lim := e.filter.Seconds(nowNs - int64(e.o.ClockLag))
	for i := 0; e.nextEpochS <= lim && i < 100; i++ {
		e.filter.Step(e.nextEpochS)
		e.nextEpochS += ep
	}
	if e.nextEpochS <= lim { // fell far behind (suspended host): skip ahead
		e.nextEpochS = (float64(int64(lim/ep)) + 1) * ep
	}
	if nowNs >= e.nextReplan {
		e.planner.Replan(nowNs)
		e.nextReplan = nowNs + int64(e.planner.Config().ReplanEvery)
	}
	e.flushModeAC(nowNs)
	if nowNs >= e.nextSecond {
		e.nextSecond = nowNs + 1e9
		e.route.flush()
		for _, w := range e.workers {
			if e.sync {
				e.syncMu.Lock()
				w.tick(nowNs)
				e.syncMu.Unlock()
			} else {
				w.postTick(nowNs)
			}
		}
		if e.sync {
			e.route.flush()
		}
	}
	if nowNs >= e.nextTen {
		e.nextTen = nowNs + 10e9
		e.clockWatchdog(nowNs)
		e.refs.Expire(nowNs)
		e.hold.Expire(nowNs - 120e9)
		e.verify.Expire(nowNs - 600e9)
		e.blame.expire(nowNs - int64(2*blameSurveyEvery))
		e.pos.expire(nowNs - posTTL)
	}
	if nowNs >= e.nextMinute {
		e.nextMinute = nowNs + 60e9
		e.rates.rotate()
		e.vbrk.rotate()
		e.stq.Forget(nowNs - 2*3600e9)
		e.cells.Prune(nowNs - 24*3600e9)
		e.minuteFeedback(nowNs)
	}
}

// Clock watchdog. A station whose frequency state went far wrong (seen on
// simnet for SDR clocks of an SDR-only network: φ off by 30-50 ppm after a
// start-up on few references) is "stepped" by the filter every few seconds
// for ever: a step resets θ but keeps φ (DESIGN §9.2.4), so the station
// never re-converges and keeps pulling its peers. When a station has
// WatchdogSteps steps within a minute, the engine re-initialises its clock
// (RemoveStation + SetStation: fresh state from the class prior, re-synced
// within seconds by its peers), at most once per WatchdogHold. Filed in
// HOOKS.md for the filter itself (reset φ after repeated steps).
const (
	watchdogSteps = 6
	watchdogHold  = int64(5 * 60e9)
)

type wdRec struct {
	steps   [7]int   // cumulative steps at the last 7 ten-second ticks
	at      [7]int64 // their times
	n       int
	resetAt int64
}

func (e *Engine) clockWatchdog(nowNs int64) {
	snap := e.filter.Snapshot()
	if e.wd == nil {
		e.wd = map[model.StationIdx]*wdRec{}
	}
	for _, sc := range snap.Stations() {
		r := e.wd[sc.Station]
		if r == nil {
			r = &wdRec{}
			e.wd[sc.Station] = r
		}
		k := r.n % len(r.steps)
		r.steps[k], r.at[k] = sc.Steps, nowNs
		r.n++
		if r.n < len(r.steps) {
			continue
		}
		old := (r.n) % len(r.steps) // the oldest sample (60 s ago)
		if sc.Steps-r.steps[old] < watchdogSteps || nowNs-r.resetAt < watchdogHold || sc.Steps < r.steps[old] {
			continue
		}
		ci, ok := e.st.clockInfoOf(sc.Station)
		if !ok {
			continue
		}
		r.resetAt = nowNs
		e.filter.RemoveStation(sc.Station)
		e.filter.SetStation(sc.Station, ci)
		e.ctr.clockResets.Add(1)
		e.o.Logger.Info("mlat: clock re-initialised after repeated steps", "station", sc.Station, "steps", sc.Steps-r.steps[old])
	}
}

// minuteFeedback applies the learned station jitter and k_atm when
// configured.
func (e *Engine) minuteFeedback(nowNs int64) {
	if e.o.JitterFeedback {
		for _, st := range e.st.all() {
			if j, ok := e.stq.LearnedJitterNs(st); ok {
				if ci, ch := e.st.setJitter(st, j); ch {
					e.filter.SetStation(st, ci)
				}
			}
		}
	}
	if e.latcal != nil {
		e.latcal.Decide(nowNs)
	}
	if e.o.ApplyKAtm && e.katm.Report().Ready {
		cur := e.Atmosphere()
		if k, _, _, ok := e.katm.Estimate(cur.K); ok && k != cur.K {
			na := cur
			na.K = k
			na = na.Normalize()
			e.atm.Store(&na)
		}
	}
}

// Run drives Advance from the server clock every 100 ms until ctx is done,
// then closes the engine.
func (e *Engine) Run(ctx context.Context) error {
	tk := time.NewTicker(100 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			e.Close()
			return nil
		case <-tk.C:
			e.safeAdvance(e.o.Now().UnixNano())
		}
	}
}

// safeAdvance runs Advance and turns a panic into an error log (with the
// stack) instead of a crash of the whole aggregator: HTTP, ingest and every
// feeder session would go down with it (finding SCALE-01). The clock filter
// recovers its own panics (clock.Config.OnPanic); this covers the rest of
// the time-driven work, which simply runs again on the next tick.
func (e *Engine) safeAdvance(nowNs int64) {
	defer func() {
		if r := recover(); r != nil {
			e.ctr.advancePanics.Add(1)
			e.o.Logger.Error("mlat: panic in the time-driven work recovered", "panic", sprintf("%v", r), "stack", string(debug.Stack()))
		}
	}()
	e.Advance(nowNs)
}

// Close stops the workers (pending groups are discarded). It is safe to
// call more than once.
func (e *Engine) Close() {
	if e.closed.Swap(true) {
		return
	}
	if !e.sync {
		for _, w := range e.workers {
			close(w.quit)
		}
		e.wg.Wait()
	}
}

// Drain waits until every worker queue is empty (tests, benchmarks). It
// returns an error when the engine is synchronous or closed.
func (e *Engine) Drain(ctx context.Context) error {
	if e.sync || e.closed.Load() {
		return errors.New("mlat: engine is synchronous or closed")
	}
	for _, w := range e.workers {
		if err := w.drain(ctx); err != nil {
			return err
		}
	}
	return nil
}
