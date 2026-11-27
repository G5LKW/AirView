package engine

import (
	"container/list"
	"log/slog"
	"sync"
	"time"

	"airview/internal/model"
	"airview/internal/tracker"
)

// Per-station runtimes (DESIGN §6.5).
//
// Some components are per station rather than per network: the analysis
// read side (analytics.Analyzer with an ant prefix), the latency chain of a
// GPS-class station, the RF statistics. They are expensive enough that only
// the stations someone is looking at, or that are busy, should have one.
// StationRuntimes owns their lifecycle:
//
//   - a runtime is created lazily by Acquire (a page asks for a station's
//     products) and lives in an LRU capped at MaxActive (default 32);
//   - observe (from the engine's maintenance loop, with the lanes' list)
//     records when each station was last heard;
//   - a runtime whose station has been silent for RetireAfter (default
//     30 minutes) is retired, and the least recently used one is retired
//     when the cap is reached.
//
// The components themselves come from Factories, so internal/analytics,
// internal/latency and internal/rfstat can be plugged in without changing
// the lifecycle (HOOKS: WP-E -> internal/analytics WithAntPrefix).

// StationComponent is one per-station component.
type StationComponent interface {
	// Close releases the component; it is called once, when the runtime
	// is retired or the engine stops.
	Close()
}

// StationFactory builds a component for a station. It returns nil when the
// component does not apply to the station (for example a latency chain for
// a station without GPS timestamps).
type StationFactory func(meta model.StationMeta) StationComponent

// StationRuntime is the set of components of one active station.
type StationRuntime struct {
	Idx        model.StationIdx
	Created    time.Time
	Components map[string]StationComponent

	lastUse   time.Time // last Acquire
	lastHeard time.Time // last activity seen in the lanes
	elem      *list.Element
}

// Component returns the named component (nil when absent).
func (r *StationRuntime) Component(name string) StationComponent {
	if r == nil {
		return nil
	}
	return r.Components[name]
}

type stationRuntimeOptions struct {
	MaxActive   int
	RetireAfter time.Duration
	Logger      *slog.Logger
}

// StationRuntimes is the LRU of per-station runtimes. Safe for concurrent
// use.
type StationRuntimes struct {
	opts stationRuntimeOptions

	mu        sync.Mutex
	factories map[string]StationFactory
	byIdx     map[model.StationIdx]*StationRuntime
	lru       *list.List // front = most recently used
	heard     map[model.StationIdx]stationActivity
	retired   uint64
}

type stationActivity struct {
	frames uint64
	at     time.Time
}

func newStationRuntimes(o stationRuntimeOptions) *StationRuntimes {
	if o.MaxActive <= 0 {
		o.MaxActive = 32
	}
	if o.RetireAfter <= 0 {
		o.RetireAfter = 30 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &StationRuntimes{
		opts:      o,
		factories: map[string]StationFactory{},
		byIdx:     map[model.StationIdx]*StationRuntime{},
		lru:       list.New(),
		heard:     map[model.StationIdx]stationActivity{},
	}
}

// Register adds (or with a nil factory removes) a component factory.
// Existing runtimes are not changed; new ones get the component.
func (s *StationRuntimes) Register(name string, f StationFactory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f == nil {
		delete(s.factories, name)
		return
	}
	s.factories[name] = f
}

// Acquire returns the runtime of idx, creating it when necessary. meta
// describes the station (from the registry resolver).
func (s *StationRuntimes) Acquire(meta model.StationMeta, now time.Time) *StationRuntime {
	var evicted []*StationRuntime
	s.mu.Lock()
	r := s.byIdx[meta.Idx]
	if r == nil {
		r = &StationRuntime{Idx: meta.Idx, Created: now, Components: map[string]StationComponent{}, lastHeard: now}
		for name, f := range s.factories {
			if c := f(meta); c != nil {
				r.Components[name] = c
			}
		}
		r.elem = s.lru.PushFront(r)
		s.byIdx[meta.Idx] = r
		for s.lru.Len() > s.opts.MaxActive {
			old := s.lru.Back().Value.(*StationRuntime)
			evicted = append(evicted, s.removeLocked(old))
		}
	} else {
		s.lru.MoveToFront(r.elem)
	}
	r.lastUse = now
	s.mu.Unlock()
	closeRuntimes(evicted)
	return r
}

// Get returns the runtime of idx without creating it.
func (s *StationRuntimes) Get(idx model.StationIdx) (*StationRuntime, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byIdx[idx]
	return r, ok
}

// Len returns the number of active runtimes.
func (s *StationRuntimes) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lru.Len()
}

// Retired returns how many runtimes were retired so far.
func (s *StationRuntimes) Retired() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retired
}

func (s *StationRuntimes) removeLocked(r *StationRuntime) *StationRuntime {
	s.lru.Remove(r.elem)
	delete(s.byIdx, r.Idx)
	s.retired++
	return r
}

func closeRuntimes(rs []*StationRuntime) {
	for _, r := range rs {
		for _, c := range r.Components {
			c.Close()
		}
	}
}

// observe takes the lanes' activity and retires runtimes of stations that
// have been silent for RetireAfter (and that nobody acquired meanwhile).
func (s *StationRuntimes) observe(lanes []tracker.LaneInfo, now time.Time) {
	var retired []*StationRuntime
	s.mu.Lock()
	live := make(map[model.StationIdx]bool, len(lanes))
	for _, l := range lanes {
		live[l.Station] = true
		a, ok := s.heard[l.Station]
		if !ok || l.Frames != a.frames {
			s.heard[l.Station] = stationActivity{frames: l.Frames, at: now}
			if r := s.byIdx[l.Station]; r != nil {
				r.lastHeard = now
			}
		}
	}
	for idx := range s.heard {
		if !live[idx] {
			delete(s.heard, idx) // lane evicted
		}
	}
	for e := s.lru.Back(); e != nil; {
		prev := e.Prev()
		r := e.Value.(*StationRuntime)
		last := r.lastHeard
		if r.lastUse.After(last) {
			last = r.lastUse
		}
		if now.Sub(last) >= s.opts.RetireAfter {
			retired = append(retired, s.removeLocked(r))
		}
		e = prev
	}
	s.mu.Unlock()
	for _, r := range retired {
		s.opts.Logger.Debug("station runtime retired", "station", uint32(r.Idx))
	}
	closeRuntimes(retired)
}

// closeAll retires every runtime (engine stop).
func (s *StationRuntimes) closeAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	var all []*StationRuntime
	for e := s.lru.Front(); e != nil; e = e.Next() {
		all = append(all, e.Value.(*StationRuntime))
	}
	s.lru.Init()
	clear(s.byIdx)
	s.mu.Unlock()
	closeRuntimes(all)
}
