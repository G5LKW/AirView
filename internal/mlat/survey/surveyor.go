package survey

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/geoid"
	"airview/internal/mlat"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Sink receives finished surveys (mlat.SurveySink; *mlat.Engine implements
// it). AutoUseSurvey is applied there (DESIGN §19.3).
type Sink interface {
	SetSurvey(st model.StationIdx, sv mlat.Survey)
}

// DeclaredFlagger is told whether a station's declared position is suspect
// (DESIGN §10.3). The engine wiring maps it onto mlat.StationInfo.Suspect /
// DiscrepancyM and the registry's location status (HOOKS.md).
type DeclaredFlagger interface {
	SetDeclaredSuspect(st model.StationIdx, suspect bool, discrepancyM float64)
}

// Notifier receives every finished job (owner notices: result, suspect
// declared position, MSL/HAE verdict). Called from the surveyor's
// goroutine; must not block and must not call back into the surveyor.
// The Result carries exact positions: owner/admin data only (DESIGN §8).
type Notifier interface {
	SurveyFinished(r Result)
}

// Targets is told when a station becomes a survey target and when it stops
// being one, so the MLAT planner asks it for the reference aircraft it
// hears (DESIGN §10.2 "31090 frames the planner requests from U's visible
// set"; AVF feeders in selective mode likewise). Called without locks
// held; must not block.
type Targets interface {
	SurveyTarget(st model.StationIdx, active bool)
}

// Terrain is a digital elevation model (terrain.Source implements it):
// ground height above mean sea level.
type Terrain interface {
	ElevationM(lat, lon float64) (elevM float64, ok bool)
}

// Declared is an owner-declared position.
type Declared struct {
	Lat, Lon, Alt float64
	// AltRef is model.AltRefMSL, AltRefHAE or AltRefAmbiguous (mlat-client
	// altitudes: the survey decides).
	AltRef string
}

// Station is what the surveyor knows about a station (the registry's view;
// pushed with SetStation).
type Station struct {
	// Class is the station's clock class (model.ClockNone: no survey).
	Class model.ClockClass
	// Timestamps is the owner's consent (DESIGN §10.5): false means the
	// receiver sends no timestamps and no survey is run, started or kept.
	Timestamps bool
	// GNSS: the station has its own GNSS position (auto-confirmed, never
	// surveyed automatically). Confirmed: the owner confirmed a position
	// (not surveyed automatically; checked like a declared one when
	// CheckConfirmed is set).
	GNSS, Confirmed bool
	// Declared is the owner's declared position (nil = none).
	Declared *Declared
	// AGLM is the antenna height above ground (0 = Config.DefaultAGLM).
	AGLM float64
	// Hint is a coarse prior (Locate Me v2); nil = none.
	Hint *geo.LLH
}

func (s *Station) located() bool { return s.GNSS || s.Confirmed || s.Declared != nil }

// Reason is why a job was started.
type Reason string

// Job reasons.
const (
	ReasonUnlocated Reason = "unlocated" // no position at all (scheduler)
	ReasonCheck     Reason = "check"     // periodic declared-position check (scheduler, every CheckEvery)
	ReasonAltRef    Reason = "altref"    // MSL/HAE test of an ambiguous declared altitude (scheduler)
	ReasonBias      Reason = "bias"      // the engine's residual-bias flag (mlat.SurveyRequester)
	ReasonOwner     Reason = "owner"     // the owner asked
	ReasonAdmin     Reason = "admin"     // an administrator asked
)

// State is a job's state.
type State string

// Job states.
const (
	StateCollecting State = "collecting"
	StateDone       State = "done"      // a stable solution
	StateUnstable   State = "unstable"  // MaxDuration reached without a stable solution (best effort kept)
	StateNoData     State = "nodata"    // too few observations
	StateCancelled  State = "cancelled" // consent withdrawn, station removed, or Cancel
)

// Errors of the job API.
var (
	ErrNoConsent      = errors.New("survey: the station does not send timestamps (no consent)")
	ErrUnknownStation = errors.New("survey: unknown station")
	ErrBusy           = errors.New("survey: too many surveys running")
	ErrRateLimited    = errors.New("survey: a survey of this station ran recently")
	ErrNotTimed       = errors.New("survey: the station's clock class has no timestamps")
)

// AltRefVerdict is the outcome of the MSL/HAE test (DESIGN §10.2).
type AltRefVerdict struct {
	Tested bool
	// Chosen is model.AltRefMSL or AltRefHAE, "" when undecided.
	Chosen string
	// FreeHAE / FreeSigmaM: the surveyed height with a free height and its
	// 1σ; ZMSL / ZHAE the normalised distances of the two interpretations.
	FreeHAE, FreeSigmaM float64
	ZMSL, ZHAE          float64
	UndulationM         float64
}

// Result is a finished (or in-progress, Progress) survey. It holds exact
// positions: owner/admin data only, never public, never logged.
type Result struct {
	Station  model.StationIdx
	Job      uint64
	Reason   Reason
	State    State
	Started  time.Time
	Finished time.Time
	// Observations collected, and the solve (Valid = a solution exists).
	Observations int
	Valid        bool
	// Error is the solver's reason when there is no solution.
	Error    string
	Solution Solution
	// Stable: DESIGN §10.2 (σ_h below the class limit, azimuth spread,
	// aircraft); Suspect: the survey failed one of its own consistency
	// checks (SuspectWhy).
	Stable     bool
	Suspect    bool
	SuspectWhy []string
	// AutoUse: the result qualifies for AutoUseSurvey (stable, not suspect,
	// σ_h <= AutoUseMaxSigmaM, AutoUse on). The engine decides; the owner
	// can override (mlat.Engine.OverrideSurvey).
	AutoUse bool
	// Ellipse95 is the 95 % horizontal error ellipse (m, degrees from north).
	Ellipse95A, Ellipse95B, Ellipse95AngleDeg float64
	// Declared-position check (DESIGN §10.3).
	DeclaredChecked bool
	DeclaredDistM   float64
	DeclaredLimitM  float64
	DeclaredSuspect bool
	AltRef          AltRefVerdict
}

// Status is a station's survey status for the owner/admin views.
type Status struct {
	Running  bool
	Job      uint64
	Reason   Reason
	Started  time.Time
	Observed int
	// Last is the last finished result (nil = none); History the last few
	// (newest first, including Last).
	Last    *Result
	History []Result
	// NextCheck is when the scheduler will survey the station next (zero =
	// not scheduled).
	NextCheck time.Time
}

// Stats are the surveyor's counters (admin).
type Stats struct {
	Stations, Running, Queued                               int
	Started, Done, Unstable, NoData, Cancelled              uint64
	Groups, Matched, Thinned, FewPeers, NoRef, Inconsistent uint64
	Accepted, Masked                                        uint64
}

// Options configure a Surveyor.
type Options struct {
	Config Config
	// Clock and Refs are the engine's clock filter and reference store
	// (mlat.Engine.Filter(), Refs()). Required.
	Clock ClockSource
	Refs  RefSource
	// Atmosphere returns the engine's propagation model
	// (mlat.Engine.Atmosphere; nil = geo.DefaultAtmosphere()).
	Atmosphere func() geo.Atmosphere
	// Sink receives results (the engine); Declared the declared-suspect
	// verdicts; Notifier every finished job. All optional.
	Sink     Sink
	Declared DeclaredFlagger
	Notifier Notifier
	// Targets (optional) is told about survey targets (the planner).
	Targets Targets
	// Terrain is the DEM for the height prior (nil = declared height or
	// the peers' heights).
	Terrain Terrain
	// Undulation returns the geoid height N (default internal/geoid).
	Undulation func(lat, lon float64) (float64, bool)
	// Now is the server clock (default time.Now).
	Now func() time.Time
	// Logger (default slog.Default()). Positions are never logged.
	Logger *slog.Logger
	// Synchronous runs evaluations inside Tick (deterministic tests and
	// replays); otherwise they run on Workers background goroutines.
	Synchronous bool
	// Workers bounds concurrent evaluations (default 2).
	Workers int
}

type stationRec struct {
	info      Station
	next      time.Time // scheduler due time
	backoff   time.Duration
	lastOwner time.Time
	history   []Result // newest first, at most historyLen
	pending   Reason   // requested job not yet started ("" = none)
	pendingAt time.Time
	suspect   bool // declared position flagged suspect
}

const historyLen = 5

type job struct {
	id       uint64
	st       model.StationIdx
	reason   Reason
	started  time.Time
	nextEval time.Time
	col      *collector
	evalBusy atomic.Bool
	best     *Result
	info     Station
	// approx is the approximate position for the tap's elevation
	// pre-filter (nil = unknown: everything is kept).
	approx atomic.Pointer[approxPos]
}

// Surveyor runs reverse-MLAT surveys (DESIGN §10.2-10.3). Create it with
// New, feed it groups through Observe (or a Tee), push the registry's view
// with SetStation and drive it with Run (or Tick).
type Surveyor struct {
	o   Options
	cfg Config
	log *slog.Logger

	active   atomic.Pointer[active]
	excluded atomic.Pointer[active]
	tap      tapCounters

	mu       sync.Mutex
	stations map[model.StationIdx]*stationRec
	jobs     map[model.StationIdx]*job
	nextID   uint64
	sem      chan struct{}
	wg       sync.WaitGroup
	closed   bool
	after    []func() // callbacks queued under mu, run by unlockRun

	started, done, unstable, nodata, cancelled atomic.Uint64
}

// New returns a Surveyor.
func New(o Options) (*Surveyor, error) {
	if o.Clock == nil || o.Refs == nil {
		return nil, errors.New("survey: Clock and Refs are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Undulation == nil {
		o.Undulation = geoid.Undulation
	}
	if o.Workers <= 0 {
		o.Workers = 2
	}
	cfg := o.Config
	cfg.Normalize()
	s := &Surveyor{o: o, cfg: cfg, log: o.Logger.With("component", "mlat.survey"),
		stations: map[model.StationIdx]*stationRec{}, jobs: map[model.StationIdx]*job{},
		sem: make(chan struct{}, o.Workers)}
	s.active.Store(&active{})
	s.excluded.Store(&active{})
	return s, nil
}

// Config returns the normalised configuration.
func (s *Surveyor) Config() Config { return s.cfg }

func (s *Surveyor) atmosphere() geo.Atmosphere {
	if s.o.Atmosphere != nil {
		if a := s.o.Atmosphere(); a != (geo.Atmosphere{}) {
			return a
		}
	}
	return geo.DefaultAtmosphere()
}

// SetStation records the registry's view of a station. Withdrawing consent
// (Timestamps = false) or losing the timed clock class cancels a running
// job at once. A changed declared position schedules a check.
func (s *Surveyor) SetStation(st model.StationIdx, info Station) {
	s.mu.Lock()
	defer s.unlockRun()
	now := s.o.Now()
	r := s.stations[st]
	if r == nil {
		r = &stationRec{next: now}
		s.stations[st] = r
	} else if !sameDeclared(r.info.Declared, info.Declared) {
		r.next, r.backoff = now, 0
		if r.suspect {
			// the owner corrected the position: the verdict no longer applies
			r.suspect = false
			s.flagDeclaredLocked(st, false, 0)
		}
	}
	r.info = info
	if j := s.jobs[st]; j != nil {
		j.info = info
		if !info.Timestamps || !info.Class.Timed() {
			s.finishLocked(j, StateCancelled, nil)
		}
	}
	s.publishLocked()
}

func sameDeclared(a, b *Declared) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// RemoveStation forgets a station (unclaimed or deleted): its job is
// cancelled and its results are dropped.
func (s *Surveyor) RemoveStation(st model.StationIdx) {
	s.mu.Lock()
	defer s.unlockRun()
	if j := s.jobs[st]; j != nil {
		s.finishLocked(j, StateCancelled, nil)
	}
	delete(s.stations, st)
	s.publishLocked()
}

// Start starts a survey of a station at the owner's or an admin's request
// (the consent-gated job API). It fails with ErrNoConsent when the station
// sends no timestamps, ErrRateLimited when an owner asks again within
// OwnerMinInterval, ErrBusy when MaxJobs surveys are running (the request
// is then queued and started by the scheduler). A running job is reused.
func (s *Surveyor) Start(st model.StationIdx, reason Reason) (uint64, error) {
	s.mu.Lock()
	defer s.unlockRun()
	r := s.stations[st]
	if r == nil {
		return 0, ErrUnknownStation
	}
	if !r.info.Timestamps {
		return 0, ErrNoConsent
	}
	if !r.info.Class.Timed() {
		return 0, ErrNotTimed
	}
	if j := s.jobs[st]; j != nil {
		return j.id, nil
	}
	now := s.o.Now()
	if reason == ReasonOwner {
		if !r.lastOwner.IsZero() && now.Sub(r.lastOwner) < s.cfg.OwnerMinInterval {
			return 0, ErrRateLimited
		}
		r.lastOwner = now
	}
	if len(s.jobs) >= s.cfg.MaxJobs {
		r.pending, r.pendingAt = reason, now
		return 0, ErrBusy
	}
	j := s.startLocked(st, r, reason, now)
	return j.id, nil
}

// RequestSurvey implements mlat.SurveyRequester (the engine's residual-bias
// flag): the station is surveyed as soon as a slot is free, when its owner
// allows timestamps.
func (s *Surveyor) RequestSurvey(st model.StationIdx, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.stations[st]
	if r == nil || !r.info.Timestamps || s.jobs[st] != nil {
		return
	}
	r.pending, r.pendingAt = ReasonBias, s.o.Now()
	s.log.Info("survey requested", "station", st, "reason", reason)
}

// Cancel stops a running job (no result is published).
func (s *Surveyor) Cancel(st model.StationIdx) bool {
	s.mu.Lock()
	defer s.unlockRun()
	j := s.jobs[st]
	if j == nil {
		return false
	}
	s.finishLocked(j, StateCancelled, nil)
	return true
}

// Status returns a station's survey status (owner/admin only: it contains
// exact positions).
func (s *Surveyor) Status(st model.StationIdx) (Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.stations[st]
	if r == nil {
		return Status{}, false
	}
	out := Status{History: append([]Result(nil), r.history...)}
	if len(out.History) > 0 {
		l := out.History[0]
		out.Last = &l
	}
	if j := s.jobs[st]; j != nil {
		out.Running, out.Job, out.Reason, out.Started, out.Observed = true, j.id, j.reason, j.started, j.col.count()
	} else if r.info.Timestamps && s.autoReason(r) != "" {
		out.NextCheck = r.next
	}
	return out, true
}

// Restore puts a persisted result back (after a restart), so the periodic
// check is not run at once for every station. It does not publish to the
// sink (the engine gets the registry's positions on its own).
func (s *Surveyor) Restore(st model.StationIdx, r Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.stations[st]
	if rec == nil {
		return
	}
	rec.history = append([]Result{r}, rec.history...)
	rec.history = rec.history[:min(len(rec.history), historyLen)]
	if r.State == StateDone || r.State == StateUnstable {
		if nx := r.Finished.Add(s.cfg.CheckEvery); nx.After(rec.next) {
			rec.next = nx
		}
	}
	rec.suspect = r.DeclaredSuspect
	s.publishLocked()
}

// Stats returns the counters.
func (s *Surveyor) Stats() Stats {
	s.mu.Lock()
	out := Stats{Stations: len(s.stations), Running: len(s.jobs)}
	for _, r := range s.stations {
		if r.pending != "" {
			out.Queued++
		}
	}
	s.mu.Unlock()
	out.Started, out.Done, out.Unstable = s.started.Load(), s.done.Load(), s.unstable.Load()
	out.NoData, out.Cancelled = s.nodata.Load(), s.cancelled.Load()
	out.Groups, out.Matched, out.Thinned = s.tap.groups.Load(), s.tap.matched.Load(), s.tap.thinned.Load()
	out.FewPeers, out.NoRef, out.Inconsistent = s.tap.fewPeers.Load(), s.tap.noRef.Load(), s.tap.inconsistent.Load()
	out.Accepted, out.Masked = s.tap.accepted.Load(), s.tap.masked.Load()
	return out
}

// Run drives the scheduler and the evaluations every ScanEvery (or every
// 5 s while jobs are collecting) until ctx ends, then waits for running
// evaluations.
func (s *Surveyor) Run(ctx context.Context) error {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var lastScan time.Time
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return nil
		case <-t.C:
			now := s.o.Now()
			if now.Sub(lastScan) >= s.cfg.ScanEvery || s.running() > 0 {
				lastScan = now
				s.Tick()
			}
		}
	}
}

func (s *Surveyor) running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// Close cancels nothing but waits for running evaluations; later Ticks do
// nothing.
func (s *Surveyor) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wg.Wait()
}

// autoReason is the scheduler's reason to survey a station ("" = none).
func (s *Surveyor) autoReason(r *stationRec) Reason {
	in := &r.info
	switch {
	case !in.Timestamps || !in.Class.Timed() || in.GNSS:
		return ""
	case in.Declared != nil && in.Declared.AltRef == model.AltRefAmbiguous && !in.Confirmed:
		return ReasonAltRef
	case in.Declared != nil:
		return ReasonCheck
	case in.Confirmed:
		return ""
	}
	return ReasonUnlocated
}

// Tick runs one scheduler pass: it starts due and requested jobs (up to
// MaxJobs), evaluates collecting jobs whose evaluation is due and ends
// expired ones.
func (s *Surveyor) Tick() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	now := s.o.Now()
	// requested jobs first (oldest first), then due scheduler jobs
	type cand struct {
		st     model.StationIdx
		reason Reason
		at     time.Time
		pri    int
	}
	var cs []cand
	for st, r := range s.stations {
		if s.jobs[st] != nil {
			continue
		}
		if r.pending != "" {
			if !r.info.Timestamps || !r.info.Class.Timed() {
				r.pending = ""
				continue
			}
			cs = append(cs, cand{st, r.pending, r.pendingAt, 0})
			continue
		}
		if rs := s.autoReason(r); rs != "" && !now.Before(r.next) {
			pri := 2
			if rs == ReasonUnlocated || rs == ReasonAltRef {
				pri = 1
			}
			cs = append(cs, cand{st, rs, r.next, pri})
		}
	}
	slices.SortFunc(cs, func(a, b cand) int {
		if a.pri != b.pri {
			return a.pri - b.pri
		}
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return int(a.st) - int(b.st)
	})
	// Checks of located stations take them out of the peer set while they
	// run: at most MaxCheckFrac of the stations are checked at once.
	checks := 0
	for _, j := range s.jobs {
		if j.reason == ReasonCheck || j.reason == ReasonAltRef {
			checks++
		}
	}
	maxChecks := max(1, int(s.cfg.MaxCheckFrac*float64(len(s.stations))))
	for _, c := range cs {
		if len(s.jobs) >= s.cfg.MaxJobs {
			break
		}
		if c.reason == ReasonCheck || c.reason == ReasonAltRef {
			if checks >= maxChecks {
				continue
			}
			checks++
		}
		r := s.stations[c.st]
		r.pending = ""
		s.startLocked(c.st, r, c.reason, now)
	}
	// evaluations
	var due []*job
	for _, j := range s.jobs {
		age := now.Sub(j.started)
		n := j.col.count()
		switch {
		case age >= s.cfg.NoDataAfter && n < 20:
			s.finishLocked(j, StateNoData, nil)
		case age >= s.cfg.MaxDuration:
			due = append(due, j) // final evaluation
		case age >= s.cfg.MinDuration && !now.Before(j.nextEval):
			due = append(due, j)
		}
	}
	s.unlockRun()
	slices.SortFunc(due, func(a, b *job) int { return int(a.st) - int(b.st) })
	for _, j := range due {
		if !j.evalBusy.CompareAndSwap(false, true) {
			continue
		}
		if s.o.Synchronous {
			s.evaluate(j, now)
			continue
		}
		s.wg.Add(1)
		go func(j *job) {
			defer s.wg.Done()
			s.sem <- struct{}{}
			defer func() { <-s.sem }()
			s.evaluate(j, now)
		}(j)
	}
}

func (s *Surveyor) startLocked(st model.StationIdx, r *stationRec, reason Reason, now time.Time) *job {
	s.nextID++
	j := &job{id: s.nextID, st: st, reason: reason, started: now, nextEval: now.Add(s.cfg.MinDuration),
		col: newCollector(int64(s.cfg.MinInterval), s.cfg.MaxObs), info: r.info}
	if d := r.info.Declared; d != nil {
		// a wrong declaration may be off by kilometres: wide margin
		j.approx.Store(newApprox(geo.ToECEF(d.Lat, d.Lon, d.Alt), s.cfg.MaskDeg, 1.5))
	}
	s.jobs[st] = j
	s.started.Add(1)
	if s.o.Targets != nil {
		s.after = append(s.after, func() { s.o.Targets.SurveyTarget(st, true) })
	}
	s.publishLocked()
	s.log.Info("survey started", "station", st, "job", j.id, "reason", string(reason))
	return j
}

// publishLocked rebuilds the hot-path sets: the targets (stations with a
// collecting job) and the excluded peers (declared-suspect stations).
func (s *Surveyor) publishLocked() {
	a := &active{jobs: make(map[model.StationIdx]*job, len(s.jobs))}
	for st, j := range s.jobs {
		a.set(st)
		a.jobs[st] = j
		a.n++
	}
	s.active.Store(a)
	x := &active{}
	for st, r := range s.stations {
		if r.suspect {
			x.set(st)
			x.n++
		}
	}
	s.excluded.Store(x)
}

func (a *active) set(st model.StationIdx) {
	i := int(st) >> 6
	for len(a.bits) <= i {
		a.bits = append(a.bits, 0)
	}
	a.bits[i] |= 1 << (uint(st) & 63)
}

// finishLocked ends a job and records its result (res nil: no solution).
func (s *Surveyor) finishLocked(j *job, state State, res *Result) {
	if s.jobs[j.st] != j {
		return
	}
	delete(s.jobs, j.st)
	now := s.o.Now()
	if s.o.Targets != nil {
		st := j.st
		s.after = append(s.after, func() { s.o.Targets.SurveyTarget(st, false) })
	}
	var out Result
	if res != nil {
		out = *res
	}
	out.Station, out.Job, out.Reason, out.State, out.Started, out.Finished = j.st, j.id, j.reason, state, j.started, now
	if out.Observations == 0 {
		out.Observations = j.col.count()
	}
	r := s.stations[j.st]
	switch state {
	case StateDone:
		s.done.Add(1)
	case StateUnstable:
		s.unstable.Add(1)
	case StateNoData:
		s.nodata.Add(1)
	case StateCancelled:
		s.cancelled.Add(1)
	}
	if r != nil {
		if state != StateCancelled {
			r.history = append([]Result{out}, r.history...)
			r.history = r.history[:min(len(r.history), historyLen)]
		}
		switch {
		case state == StateDone:
			r.next, r.backoff = now.Add(s.cfg.CheckEvery), 0
		case state == StateCancelled:
			r.next = now.Add(s.cfg.RetryAfter)
		default:
			// unlocated: retry with a doubling pause; checks: at the next period
			if r.backoff == 0 {
				r.backoff = s.cfg.RetryAfter
			} else {
				r.backoff = min(2*r.backoff, s.cfg.CheckEvery)
			}
			if r.info.located() {
				r.next = now.Add(min(r.backoff, s.cfg.CheckEvery))
			} else {
				r.next = now.Add(r.backoff)
			}
		}
	}
	s.publishLocked()
	s.log.Info("survey finished", "station", j.st, "job", j.id, "state", string(state), "obs", out.Observations,
		"valid", out.Valid, "sigmaH", math.Round(out.Solution.SigmaHM*10)/10, "stable", out.Stable, "suspect", out.Suspect,
		"declaredSuspect", out.DeclaredSuspect, "altRef", out.AltRef.Chosen)
	if state == StateCancelled || r == nil {
		return
	}
	st := j.st
	if out.Valid && s.o.Sink != nil {
		sv := mlat.Survey{Valid: true, ECEF: out.Solution.ECEF, SigmaHM: out.Solution.SigmaHM,
			SigmaVM: out.Solution.SigmaVM, Stable: out.Stable, Suspect: out.Suspect}
		s.after = append(s.after, func() { s.o.Sink.SetSurvey(st, sv) })
	}
	// a suspect verdict is taken from any result; it is cleared only by a
	// stable one (weak evidence never clears it)
	if out.DeclaredChecked && out.DeclaredSuspect != r.suspect && (out.DeclaredSuspect || out.Stable) {
		r.suspect = out.DeclaredSuspect
		s.flagDeclaredLocked(st, out.DeclaredSuspect, out.DeclaredDistM)
		s.publishLocked()
	}
	if s.o.Notifier != nil {
		s.after = append(s.after, func() { s.o.Notifier.SurveyFinished(out) })
	}
}

// unlockRun releases mu and runs the callbacks queued under it (sink,
// flagger, notifier: never called with mu held).
func (s *Surveyor) unlockRun() {
	f := s.after
	s.after = nil
	s.mu.Unlock()
	for _, fn := range f {
		fn()
	}
}

func (s *Surveyor) flagDeclaredLocked(st model.StationIdx, suspect bool, d float64) {
	if s.o.Declared != nil {
		s.after = append(s.after, func() { s.o.Declared.SetDeclaredSuspect(st, suspect, d) })
	}
}
