package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"airview/internal/model"
)

// Hook registries for the store files of other owners (trails, statistics,
// met ...). Each owner registers from an init function of its own file; the
// store core (maint.go, checkpointer.go, HourlyMaintenance) calls the hooks.
// Hooks are global and run for every Store; a hook checks s.Kind() itself
// when it applies to one database kind only.

// pruneFunc applies retention to the tables of its owner. It runs inside
// Prune after RollupHours and before the legacy 1.0.0 pruning of maint.go
// (so it can still join on the flights that the legacy step deletes), and
// adds what it deleted to res (its own counters, or res.Extra[name]). It
// must work in small transactions through s.Write (never hold the writer
// for long) and stop between chunks when ctx is cancelled.
type pruneFunc func(ctx context.Context, s *Store, cfg model.StorageConfig, now int64, res *PruneResult) error

// hourlyFunc is an hourly maintenance job (HourlyMaintenance), e.g. trail
// compaction, sealing closed histogram days, flights_hour rollups. It runs
// on the caller's goroutine and writes through s.Write / s.Apply.
type hourlyFunc func(ctx context.Context, s *Store) error

// periodicFunc is a job the checkpointer goroutine runs every interval
// (e.g. sealing the trail tail). It writes through s.Write; it never runs
// on the writer goroutine.
type periodicFunc func(ctx context.Context, s *Store) error

type namedPruner struct {
	name string
	fn   pruneFunc
}

type namedHourly struct {
	name string
	fn   hourlyFunc
}

type namedPeriodic struct {
	name string
	// every returns the interval for a store's profile (<= 0: never).
	every func(p Profile) time.Duration
	// beforeTruncate: also run right before every TRUNCATE checkpoint.
	beforeTruncate bool
	fn             periodicFunc
}

var hooks struct {
	mu       sync.Mutex
	pruners  []namedPruner
	hourly   []namedHourly
	periodic []namedPeriodic
}

// registerPruner adds a retention step to Prune. Names must be unique; a
// second registration under the same name replaces the first.
func registerPruner(name string, fn func(ctx context.Context, s *Store, cfg model.StorageConfig, now int64, res *PruneResult) error) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	for i := range hooks.pruners {
		if hooks.pruners[i].name == name {
			hooks.pruners[i].fn = fn
			return
		}
	}
	hooks.pruners = append(hooks.pruners, namedPruner{name, fn})
	sort.SliceStable(hooks.pruners, func(i, j int) bool { return hooks.pruners[i].name < hooks.pruners[j].name })
}

// registerHourly adds a job to HourlyMaintenance (run in name order after
// RollupHours, before PRAGMA optimize and the TRUNCATE checkpoint).
func registerHourly(name string, fn func(ctx context.Context, s *Store) error) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	for i := range hooks.hourly {
		if hooks.hourly[i].name == name {
			hooks.hourly[i].fn = fn
			return
		}
	}
	hooks.hourly = append(hooks.hourly, namedHourly{name, fn})
	sort.SliceStable(hooks.hourly, func(i, j int) bool { return hooks.hourly[i].name < hooks.hourly[j].name })
}

// registerPeriodic adds a job the checkpointer runs every every(profile)
// (e.g. the trail seal: Profile.SealInterval) and, with beforeTruncate,
// before every TRUNCATE checkpoint (Checkpoint, HourlyMaintenance).
func registerPeriodic(name string, every func(p Profile) time.Duration, beforeTruncate bool,
	fn func(ctx context.Context, s *Store) error) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	np := namedPeriodic{name: name, every: every, beforeTruncate: beforeTruncate, fn: fn}
	for i := range hooks.periodic {
		if hooks.periodic[i].name == name {
			hooks.periodic[i] = np
			return
		}
	}
	hooks.periodic = append(hooks.periodic, np)
	sort.SliceStable(hooks.periodic, func(i, j int) bool { return hooks.periodic[i].name < hooks.periodic[j].name })
}

func registeredPruners() []namedPruner {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	return append([]namedPruner(nil), hooks.pruners...)
}

func registeredHourly() []namedHourly {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	return append([]namedHourly(nil), hooks.hourly...)
}

func registeredPeriodic() []namedPeriodic {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	return append([]namedPeriodic(nil), hooks.periodic...)
}

// runBeforeTruncate runs the periodic jobs flagged beforeTruncate (errors
// are logged, never fatal for the checkpoint).
func (s *Store) runBeforeTruncate(ctx context.Context) {
	for _, p := range registeredPeriodic() {
		if !p.beforeTruncate {
			continue
		}
		if err := safely(func() error { return p.fn(ctx, s) }); err != nil && ctx.Err() == nil {
			s.log.Warn("store: periodic job failed", "job", p.name, "err", err)
		}
	}
}

// HourlyReport says what one HourlyMaintenance call did.
type HourlyReport struct {
	Hours        int               `json:"hours"`            // stats_hour rows (re)computed
	Jobs         []string          `json:"jobs"`             // hourly jobs run
	Errors       map[string]string `json:"errors,omitempty"` // job -> error
	Optimized    bool              `json:"optimized"`
	Checkpointed bool              `json:"checkpointed"`
	Took         time.Duration     `json:"tookNs"`
}

// HourlyMaintenance is the hourly job of a station (the engine's scheduler
// calls it once an hour instead of RollupHours + Checkpoint):
//
//  1. RollupHours;
//  2. every registered hourly job (trail compaction, histogram sealing,
//     rollups of other owners; a failing job is logged and the rest run);
//  3. PRAGMA optimize with analysis_limit=400 (planner statistics stay
//     fresh on a station that is never stopped cleanly);
//  4. the periodic jobs that must precede a TRUNCATE (trail seal), then a
//     TRUNCATE checkpoint with a 1 s busy timeout.
//
// It returns the first hard error (rollup, optimize or checkpoint); job
// errors are in the report.
func (s *Store) HourlyMaintenance(ctx context.Context) (HourlyReport, error) {
	t0 := time.Now()
	rep := HourlyReport{}
	defer func() { rep.Took = time.Since(t0) }()
	n, err := s.RollupHours(ctx)
	rep.Hours = n
	if err != nil {
		return rep, err
	}
	for _, j := range registeredHourly() {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		rep.Jobs = append(rep.Jobs, j.name)
		if err := safely(func() error { return j.fn(ctx, s) }); err != nil {
			if rep.Errors == nil {
				rep.Errors = map[string]string{}
			}
			rep.Errors[j.name] = err.Error()
			if ctx.Err() == nil {
				s.log.Warn("store: hourly job failed", "job", j.name, "err", err)
			}
		}
	}
	if err := s.optimizeLimited(ctx); err != nil {
		return rep, err
	}
	rep.Optimized = true
	if err := s.Checkpoint(ctx); err != nil {
		return rep, err
	}
	rep.Checkpointed = true
	return rep, nil
}
