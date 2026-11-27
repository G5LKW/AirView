package ssr

import (
	"math"
	"sort"
	"sync"

	"airview/internal/mlat/geo"
)

// Stats counts what the estimator saw and kept.
type Stats struct {
	Replies       uint64 // replies added
	ResolvedAC    uint64 // Mode A/C replies identified with a tracked aircraft
	UnresolvedAC  uint64 // no tracked aircraft with that code
	AmbiguousAC   uint64 // more than one candidate
	Dwells        uint64 // dwells closed with enough replies
	DwellsNoTrack uint64 // dwells without a position
	PairsTried    uint64
	PairMatches   uint64 // train matches accepted
	Observations  int    // stored observations
	Stride        int    // current thinning stride
	Tracks        int    // tracked aircraft
}

type dwellKey struct {
	AircraftKey
	iid uint8
}

type dwell struct {
	key  dwellKey
	t    []float64
	mid  geo.Vec3
	sepM float64
}

func (d *dwell) start() float64 { return d.t[0] }
func (d *dwell) end() float64   { return d.t[len(d.t)-1] }

// Estimator collects SSR replies and reference aircraft positions of one
// station and turns them into pair observations (PairObs); Solve estimates
// the station position from them. All methods are safe for concurrent use.
// Memory is bounded by Config.MaxObs and the position retention (120 s).
type Estimator struct {
	mu      sync.Mutex
	cfg     Config
	atm     geo.Atmosphere
	tracks  *TrackStore
	pending []Reply
	open    map[dwellKey]*dwell
	recent  []*dwell
	obs     []PairObs
	stride  int
	skip    int
	st      Stats
}

// New returns an estimator. The configuration is normalised; Enabled is
// not checked here (the caller decides whether to feed it).
func New(cfg Config) *Estimator {
	cfg.Normalize()
	return &Estimator{
		cfg:    cfg,
		atm:    geo.DefaultAtmosphere(),
		tracks: NewTrackStore(120, 15, 2),
		open:   map[dwellKey]*dwell{},
		stride: 1,
	}
}

// Config returns the normalised configuration.
func (e *Estimator) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// SetAtmosphere replaces the propagation model used by Solve.
func (e *Estimator) SetAtmosphere(a geo.Atmosphere) {
	e.mu.Lock()
	e.atm = a.Normalize()
	e.mu.Unlock()
}

// AddPosition stores a position report of a reference aircraft.
func (e *Estimator) AddPosition(icao uint32, p Position) {
	if p.SigmaM <= 0 {
		p.SigmaM = e.cfg.TrackSigmaM
	}
	e.mu.Lock()
	e.tracks.Add(icao, p)
	e.mu.Unlock()
}

// AddReply queues a reply; Process handles it once positions after it are
// known.
func (e *Estimator) AddReply(r Reply) {
	e.mu.Lock()
	e.pending = append(e.pending, r)
	e.st.Replies++
	e.mu.Unlock()
}

// Process handles the queued replies older than now - ProcessLagS (now in
// the local time base). Call it about once a second.
func (e *Estimator) Process(now float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cut := now - e.cfg.ProcessLagS
	sort.SliceStable(e.pending, func(i, j int) bool { return e.pending[i].T < e.pending[j].T })
	k := sort.Search(len(e.pending), func(i int) bool { return e.pending[i].T > cut })
	for _, r := range e.pending[:k] {
		e.ingest(r)
	}
	e.pending = append(e.pending[:0], e.pending[k:]...)

	// Close the dwells that ended, oldest first.
	var closing []*dwell
	for key, d := range e.open {
		if d.end() < cut-e.cfg.DwellGapS {
			closing = append(closing, d)
			delete(e.open, key)
		}
	}
	sort.Slice(closing, func(i, j int) bool { return closing[i].end() < closing[j].end() })
	for _, d := range closing {
		e.close(d)
	}
	e.pruneRecent(cut)
}

func (e *Estimator) ingest(r Reply) {
	var key dwellKey
	switch r.Class {
	case ClassModeAC:
		id, ok := e.resolve(r)
		if !ok {
			return
		}
		key = dwellKey{AircraftKey{id, ClassModeAC}, 0}
	case ClassModeS:
		if r.ICAO == 0 {
			return
		}
		key = dwellKey{AircraftKey{r.ICAO, ClassModeS}, r.IID}
	default:
		return
	}
	d := e.open[key]
	if d != nil && (r.T-d.end() > e.cfg.DwellGapS || r.T-d.start() > e.cfg.MaxDwellS) {
		delete(e.open, key)
		e.close(d)
		d = nil
	}
	if d == nil {
		d = &dwell{key: key}
		e.open[key] = d
	}
	if n := len(d.t); n > 0 && r.T <= d.t[n-1] {
		return // duplicate or out of order
	}
	d.t = append(d.t, r.T)
}

func (e *Estimator) resolve(r Reply) (uint32, bool) {
	id, n := e.tracks.ResolveN(r.Code&0o7777, r.T, e.cfg.ConspicuityCodes)
	switch {
	case n == 0:
		e.st.UnresolvedAC++
		return 0, false
	case n > 1:
		e.st.AmbiguousAC++
		return 0, false
	}
	e.st.ResolvedAC++
	return id, true
}

func (e *Estimator) pruneRecent(cut float64) {
	lim := cut - e.cfg.DwellGapS - 0.01
	for _, d := range e.open {
		lim = math.Min(lim, d.start()-0.01)
	}
	i := 0
	for _, d := range e.recent {
		if d.end() >= lim {
			e.recent[i] = d
			i++
		}
	}
	for j := i; j < len(e.recent); j++ {
		e.recent[j] = nil
	}
	e.recent = e.recent[:i]
}

// close pairs a finished dwell with the recently finished dwells of other
// aircraft that overlap it in time.
func (e *Estimator) close(d *dwell) {
	cfg := &e.cfg
	if len(d.t) < cfg.MinMatches {
		return
	}
	mid, _, ok := e.tracks.At(d.key.ICAO, (d.start()+d.end())/2)
	if !ok {
		e.st.DwellsNoTrack++
		return
	}
	d.mid = mid
	e.st.Dwells++
	slack := 2*cfg.MaxPairSepM/geo.C + 1e-4
	tol := cfg.MatchTolNs * 1e-9
	for _, o := range e.recent {
		if o.key.ICAO == d.key.ICAO || o.key.Class != d.key.Class || o.key.iid != d.key.iid {
			continue
		}
		if o.end() < d.start()-slack || o.start() > d.end()+slack {
			continue
		}
		sep := o.mid.Dist(d.mid)
		if sep > cfg.MaxPairSepM {
			continue
		}
		e.st.PairsTried++
		maxLag := (2*sep + 3000) / geo.C
		ms := MatchTrains(d.t, o.t, maxLag, tol, cfg.MinMatches, cfg.AmbiguityRatio, 3)
		for _, m := range ms {
			pk, sk, ok1 := e.tracks.At(d.key.ICAO, m.TA)
			pj, sj, ok2 := e.tracks.At(o.key.ICAO, m.TB)
			if !ok1 || !ok2 {
				continue
			}
			e.st.PairMatches++
			vk, _ := e.tracks.VelAt(d.key.ICAO, m.TA)
			vj, _ := e.tracks.VelAt(o.key.ICAO, m.TB)
			e.addObs(PairObs{
				K: d.key.AircraftKey, J: o.key.AircraftKey, TK: m.TA, TJ: m.TB,
				PK: pk, PJ: pj, SK: sk, SJ: sj, VK: vk, VJ: vj, Delta: m.Delta, N: m.N,
				IID: d.key.iid, Spread: m.Spread, Radar: -1,
			})
		}
	}
	e.recent = append(e.recent, d)
}

func (e *Estimator) addObs(o PairObs) {
	e.skip++
	if e.skip < e.stride {
		return
	}
	e.skip = 0
	if len(e.obs) >= e.cfg.MaxObs {
		n := 0
		for i := 0; i < len(e.obs); i += 2 {
			e.obs[n] = e.obs[i]
			n++
		}
		e.obs = e.obs[:n]
		e.stride *= 2
	}
	e.obs = append(e.obs, o)
}

// Observations returns a copy of the stored pair observations.
func (e *Estimator) Observations() []PairObs {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]PairObs(nil), e.obs...)
}

// Stats returns the counters.
func (e *Estimator) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.st
	s.Observations = len(e.obs)
	s.Stride = e.stride
	s.Tracks = e.tracks.Len()
	return s
}

// Reset drops all observations, dwells and queued replies (positions are
// kept).
func (e *Estimator) Reset() {
	e.mu.Lock()
	e.pending = nil
	e.open = map[dwellKey]*dwell{}
	e.recent = nil
	e.obs = nil
	e.stride, e.skip = 1, 0
	e.st = Stats{}
	e.mu.Unlock()
}

// Solve estimates the station position from the stored observations.
func (e *Estimator) Solve(h Hint) Result {
	e.mu.Lock()
	obs := append([]PairObs(nil), e.obs...)
	cfg := e.cfg
	atm := e.atm
	e.mu.Unlock()
	return SolveObs(obs, h, cfg, atm)
}
