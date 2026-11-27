package plan

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// Options are the planner's collaborators.
type Options struct {
	Config Config
	// Stations gives station positions and clock state (required).
	Stations Stations
	// Positions (optional) gives aircraft position estimates: tracked
	// aircraft get lower solve rates and their station selection uses the
	// real geometry.
	Positions Positions
	// HoldOut (optional) reports whether an ADS-B aircraft is in the quality
	// hold-out set (internal/mlat/quality, DESIGN §9.6).
	HoldOut func(key uint32, nowNs int64) bool
	// Out receives interest-set changes (nil = none: the planner still
	// admits solves and keeps its sets for inspection).
	Out Out
	// Now is the server clock for Run (default time.Now).
	Now func() time.Time
	// ModeACOut (optional) is told when a station starts or stops taking
	// part in Mode A/C MLAT (ModeACEligible changes), so that its link can
	// ask the receiver for Mode A/C replies (modeac.go).
	ModeACOut ModeACOut
}

// Planner keeps the per-station syncSet and mlatSet (DESIGN §4.4, §9.1
// mlat/plan), the per-aircraft solve plans, and admits solves against the
// rate limits and the global budget. All methods are safe for concurrent
// use; Replan runs one at a time.
type Planner struct {
	cfg      Config
	stations Stations
	pos      Positions
	holdOut  func(uint32, int64) bool
	out      Out
	now      func() time.Time
	acOut    ModeACOut

	t table
	g globalBucket

	rp    sync.Mutex // one Replan at a time; guards everything below
	work  workspace
	state map[model.StationIdx]*stState
	last  atomic.Pointer[Summary]
	modeA atomic.Pointer[map[model.StationIdx]struct{}]

	replans atomic.Uint64

	survey surveySet // survey targets (survey.go)
}

// stState is what a station was last told.
type stState struct {
	sync, mlat map[uint32]int64 // key -> time added
	told       bool
}

// New returns a planner.
func New(o Options) (*Planner, error) {
	if o.Stations == nil {
		return nil, errors.New("plan: Stations is required")
	}
	p := &Planner{cfg: o.Config.Normalize(), stations: o.Stations, pos: o.Positions, holdOut: o.HoldOut, out: o.Out,
		now: o.Now, state: map[model.StationIdx]*stState{}, acOut: o.ModeACOut}
	if p.now == nil {
		p.now = time.Now
	}
	p.t.init(p.now().UnixNano()-1e9, p.cfg.MaxAircraft)
	p.g.rate = p.cfg.GlobalSolveHz
	p.g.cap = p.cfg.GlobalSolveHz
	p.work.init()
	return p, nil
}

// Config returns the normalised configuration.
func (p *Planner) Config() Config { return p.cfg }

// Run replans every ReplanEvery until ctx is done.
func (p *Planner) Run(ctx context.Context) error {
	tk := time.NewTicker(p.cfg.ReplanEvery)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
			p.Replan(p.now().UnixNano())
		}
	}
}

// ModeACEligible reports whether station st takes part in Mode A/C MLAT
// (an anchor with enough anchor peers within ModeAC.MaxBaselineKm; always
// false while ModeAC is disabled). Updated by Replan.
func (p *Planner) ModeACEligible(st model.StationIdx) bool {
	m := p.modeA.Load()
	if m == nil {
		return false
	}
	_, ok := (*m)[st]
	return ok
}

// Summary describes the last replan (admin scope).
type Summary struct {
	AtNs        int64         `json:"atNs"`
	Took        time.Duration `json:"took"`
	Aircraft    int           `json:"aircraft"`   // aircraft heard within HeardTTL
	Candidates  [NumPrio]int  `json:"candidates"` // MLAT candidates per class
	Stations    int           `json:"stations"`
	SyncMembers int           `json:"syncMembers"` // sum of syncSet sizes
	MLATMembers int           `json:"mlatMembers"` // sum of mlatSet sizes
	Requests    int           `json:"requests"`    // (station, aircraft) MLAT requests before station caps
	Dropped     int           `json:"dropped"`     // requests dropped by station caps / budgets
	Deltas      int           `json:"deltas"`      // non-empty deltas sent
	Forgotten   int           `json:"forgotten"`   // aircraft removed from the table
	ModeAC      int           `json:"modeAc"`      // Mode A/C eligible stations
	// Underserved counts MLAT aircraft with fewer than 4 planned stations.
	Underserved int `json:"underserved"`
	// SurveyTargets counts the stations whose syncSet was chosen for a
	// reverse-MLAT survey (SetSurveyTarget).
	SurveyTargets int `json:"surveyTargets"`
	// Admission counters since start, per class.
	Admitted [NumPrio]uint64 `json:"admitted"`
	Denied   [NumPrio]uint64 `json:"denied"`
}

// Summary returns the last replan's summary (zero before the first).
func (p *Planner) Summary() Summary {
	var s Summary
	if l := p.last.Load(); l != nil {
		s = *l
	}
	p.g.mu.Lock()
	s.Admitted, s.Denied = p.g.ok, p.g.denied
	p.g.mu.Unlock()
	return s
}

// Sets returns copies of the syncSet and mlatSet station st was last told.
func (p *Planner) Sets(st model.StationIdx) (syncSet, mlatSet []uint32) {
	p.rp.Lock()
	defer p.rp.Unlock()
	s := p.state[st]
	if s == nil {
		return nil, nil
	}
	for k := range s.sync {
		syncSet = append(syncSet, k)
	}
	for k := range s.mlat {
		mlatSet = append(mlatSet, k)
	}
	sortKeys(syncSet)
	sortKeys(mlatSet)
	return syncSet, mlatSet
}

// AircraftPlan returns the stations the last replan chose to send frames of
// an MLAT aircraft and the expected horizontal CRLB (m) of that set.
func (p *Planner) AircraftPlan(key uint32) (stations []model.StationIdx, sigmaHM float64, ok bool) {
	p.rp.Lock()
	defer p.rp.Unlock()
	w := &p.work
	i, ok := w.acByKey[key]
	if !ok {
		return nil, 0, false
	}
	a := &w.ac[i]
	for _, r := range w.req[a.r0:a.r1] {
		stations = append(stations, w.st[r.si].idx)
	}
	return stations, a.sigmaH, true
}
