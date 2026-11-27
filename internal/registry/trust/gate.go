package trust

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// State is a station's timing state as the gate applies it.
type State uint8

const (
	// Trusted copies reach the MLAT engine.
	Trusted State = iota
	// Quarantined copies are kept out of the engine and judged.
	Quarantined
	// Excluded copies are kept out and not judged (admin override).
	Excluded
	// Unknown: the station source does not know the station (treated as
	// trusted: the gate never drops data of stations it cannot place).
	Unknown
)

func (s State) String() string {
	switch s {
	case Quarantined:
		return "quarantined"
	case Excluded:
		return "excluded"
	case Unknown:
		return "unknown"
	}
	return "trusted"
}

// StationInfo is what the gate needs to know about a station.
type StationInfo struct {
	State State
	// Located / Lat, Lon (degrees) / HAE (metres above the ellipsoid): the
	// effective position (needed to judge timing; an unlocated station
	// stays quarantined).
	Located       bool
	Lat, Lon, HAE float64
}

// Stations supplies station states and positions (the registry adapter
// FromRegistry, or a test table). Calls come from the ingest sweeper.
type Stations interface {
	TrustInfo(st model.StationIdx) (StationInfo, bool)
}

// Gate sits between the ingest and the MLAT engine (ingest.MLATSink and
// ingest.LateObserver): copies of quarantined and excluded stations are
// removed from every group before it reaches the engine, and quarantined
// stations are judged on the DF17 position groups they share with trusted
// stations (see doc.go). It also implements mlatc.TrustSource.
type Gate struct {
	next ingest.MLATSink
	late ingest.LateObserver
	src  Stations
	cfg  Config
	// Promote is called (from the sweeper goroutine) when a quarantined
	// station's timing is proven; it must persist the decision (the
	// registry adapter calls Registry.ProveTiming) and return quickly
	// (start a goroutine for I/O).
	promote func(st model.StationIdx, rep Report)

	// states is a copy-on-write table StationIdx -> State+1 (0 = not
	// loaded); read lock-free by the hot path and by Quarantined.
	states atomic.Pointer[[]uint8]

	// sweeper-goroutine state
	ent       map[model.StationIdx]*entry
	lastLoad  int64 // group time of the last refresh (ns)
	scratch   ingest.Group
	copies    []ingest.Copy
	refs      []int
	activeRef map[model.StationIdx]int64 // trusted, located, timed stations: last copy (ns)

	mu   sync.Mutex // reports for Status / Stats (copied from entries)
	reps map[model.StationIdx]Report

	c counters
}

type counters struct {
	groups, filtered, dropped, copiesOut, samples, windows, promoted atomic.Uint64
}

// entry is the sweeper's view of one station.
type entry struct {
	info   StationInfo
	ecef   geo.Vec3
	ref    *evalState // judging state (quarantined stations only)
	loaded int64      // group time of the last read of the station source
	seen   int64      // group time of the last copy (refreshAll forgets idle stations)
	// provenAt: group time of the gate's proof (ProvenGrace)
	provenAt int64
	// tick base per clock epoch (judging works on tick differences)
	baseEpoch uint32
	baseTicks uint64
	baseSet   bool
}

// New returns a gate in front of next (the MLAT engine). promote may be
// nil (proofs are then only reported).
func New(next ingest.MLATSink, src Stations, cfg Config, promote func(model.StationIdx, Report)) *Gate {
	g := &Gate{next: next, src: src, cfg: cfg.normalized(), promote: promote,
		ent: map[model.StationIdx]*entry{}, reps: map[model.StationIdx]Report{}, activeRef: map[model.StationIdx]int64{}}
	g.late, _ = next.(ingest.LateObserver)
	empty := []uint8{}
	g.states.Store(&empty)
	return g
}

var (
	_ ingest.MLATSink     = (*Gate)(nil)
	_ ingest.LateObserver = (*Gate)(nil)
)

// state returns the cached state of st (lock-free).
func (g *Gate) state(st model.StationIdx) (State, bool) {
	s := *g.states.Load()
	if int(st) < len(s) && s[st] != 0 {
		return State(s[st] - 1), true
	}
	return Unknown, false
}

// Quarantined implements mlatc.TrustSource: true while the station's
// timing is unproven (also for an excluded station, which must not be
// asked for more than it already sends either).
func (g *Gate) Quarantined(st model.StationIdx) bool {
	s, ok := g.state(st)
	if !ok && g.cfg.on {
		if info, ok := g.src.TrustInfo(st); ok {
			s = info.State
		}
	}
	return g.cfg.on && (s == Quarantined || s == Excluded)
}

// Refresh re-reads every known station on the next group (call after an
// admin override for an immediate effect; otherwise states are re-read
// every Config.RefreshEvery).
func (g *Gate) Refresh() {
	// Dropping the cached table makes the hot path reload stations lazily;
	// the sweeper re-reads entries on its next refresh.
	empty := []uint8{}
	g.states.Store(&empty)
}

// setState publishes st's state in the copy-on-write table.
func (g *Gate) setState(st model.StationIdx, s State) {
	old := *g.states.Load()
	if int(st) < len(old) && old[st] == uint8(s)+1 {
		return
	}
	n := max(len(old), int(st)+1)
	next := make([]uint8, n, n+n/4+8)
	copy(next, old)
	next[st] = uint8(s) + 1
	g.states.Store(&next)
}

// load returns the entry of st, reading the station source when it is new
// or stale. Sweeper goroutine.
func (g *Gate) load(st model.StationIdx, nowNs int64) *entry {
	e := g.ent[st]
	if e != nil {
		e.seen = nowNs
		if nowNs-e.loaded < int64(g.cfg.RefreshEvery) {
			if _, ok := g.state(st); ok {
				return e
			}
		}
	}
	info, ok := g.src.TrustInfo(st)
	if !ok {
		info = StationInfo{State: Unknown}
	}
	if e == nil {
		e = &entry{seen: nowNs}
		g.ent[st] = e
	}
	if info.State == Quarantined && e.provenAt != 0 && nowNs-e.provenAt < int64(g.cfg.ProvenGrace) {
		info.State = Trusted // the proof is still being written to the source
	}
	prev := e.info.State
	e.info, e.loaded = info, nowNs
	if info.Located {
		e.ecef = geo.ToECEF(info.Lat, info.Lon, info.HAE)
	}
	if info.State != Quarantined {
		e.ref = nil
		g.dropReport(st)
	} else if e.ref == nil || prev != Quarantined {
		e.ref = newEvalState(nowNs)
	}
	g.setState(st, info.State)
	return e
}

// Observe implements ingest.MLATSink.
func (g *Gate) Observe(grp *ingest.Group) { g.observe(grp, false) }

// ObserveLate implements ingest.LateObserver.
func (g *Gate) ObserveLate(grp *ingest.Group) { g.observe(grp, true) }

func (g *Gate) observe(grp *ingest.Group, late bool) {
	g.c.groups.Add(1)
	if !g.cfg.on {
		g.forward(grp, late)
		return
	}
	now := grp.FirstNs
	if now-g.lastLoad >= int64(g.cfg.RefreshEvery) {
		g.refreshAll(now)
	}
	keep := true
	for i := range grp.Copies {
		c := &grp.Copies[i]
		s, ok := g.state(c.Station)
		if !ok {
			s = g.load(c.Station, now).info.State
		}
		if s == Quarantined || s == Excluded {
			keep = false
		}
	}
	if keep {
		if !late {
			g.noteRefs(grp, now)
		}
		g.forward(grp, late)
		return
	}
	if !late {
		g.judge(grp, now)
	}
	// Forward the group without the copies of untrusted stations.
	g.copies = g.copies[:0]
	timed := 0
	for i := range grp.Copies {
		c := &grp.Copies[i]
		if s, _ := g.state(c.Station); s == Quarantined || s == Excluded {
			continue
		}
		if c.Timed && !c.SameStation {
			timed++
		}
		g.copies = append(g.copies, *c)
	}
	g.c.filtered.Add(1)
	g.c.copiesOut.Add(uint64(len(grp.Copies) - len(g.copies)))
	if len(g.copies) == 0 {
		g.c.dropped.Add(1)
		return
	}
	g.scratch = *grp
	g.scratch.Copies = g.copies
	g.scratch.Timed = timed
	if !late {
		g.noteRefs(&g.scratch, now)
	}
	g.forward(&g.scratch, late)
}

// refreshAll re-reads every known station (every RefreshEvery of group
// time) and evicts stations not seen for a long time.
func (g *Gate) refreshAll(now int64) {
	g.lastLoad = now
	for st, e := range g.ent {
		if now-e.seen > int64(g.cfg.ForgetAfter) {
			delete(g.ent, st)
			delete(g.activeRef, st)
			g.dropReport(st)
			continue
		}
		e.loaded = math.MinInt64 / 2
		g.load(st, now)
	}
}

func (g *Gate) forward(grp *ingest.Group, late bool) {
	if late {
		if g.late != nil {
			g.late.ObserveLate(grp)
		}
		return
	}
	g.next.Observe(grp)
}

// noteRefs records the trusted, located, timed stations active in a group
// (the bootstrap rule needs their number).
func (g *Gate) noteRefs(grp *ingest.Group, now int64) {
	for i := range grp.Copies {
		c := &grp.Copies[i]
		if !c.Timed || c.SameStation {
			continue
		}
		if e := g.ent[c.Station]; e != nil && e.info.State == Trusted && e.info.Located {
			g.activeRef[c.Station] = now
		}
	}
}

// activeRefs counts trusted reference stations seen within RefActiveFor.
func (g *Gate) activeRefs(now int64) int {
	n := 0
	for st, t := range g.activeRef {
		if now-t > int64(g.cfg.RefActiveFor) {
			delete(g.activeRef, st)
			continue
		}
		n++
	}
	return n
}

// Stats are the gate's counters.
type Stats struct {
	Enabled     bool   `json:"enabled"`
	Groups      uint64 `json:"groups"`      // groups received
	Filtered    uint64 `json:"filtered"`    // groups forwarded without untrusted copies
	Dropped     uint64 `json:"dropped"`     // groups with only untrusted copies (not forwarded)
	CopiesOut   uint64 `json:"copiesOut"`   // copies removed
	Samples     uint64 `json:"samples"`     // timing comparisons made
	Windows     uint64 `json:"windows"`     // fit windows judged
	Promoted    uint64 `json:"promoted"`    // stations proven
	Quarantined int    `json:"quarantined"` // stations currently judged
}

// Stats returns the counters.
func (g *Gate) Stats() Stats {
	g.mu.Lock()
	q := len(g.reps)
	g.mu.Unlock()
	return Stats{Enabled: g.cfg.on, Groups: g.c.groups.Load(), Filtered: g.c.filtered.Load(), Dropped: g.c.dropped.Load(),
		CopiesOut: g.c.copiesOut.Load(), Samples: g.c.samples.Load(), Windows: g.c.windows.Load(),
		Promoted: g.c.promoted.Load(), Quarantined: q}
}

// Status returns the judging progress of a quarantined station (owner and
// admin views; no peer identities). ok is false for a station that is not
// being judged.
func (g *Gate) Status(st model.StationIdx) (Report, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.reps[st]
	return r, ok
}

func (g *Gate) setReport(st model.StationIdx, r Report) {
	g.mu.Lock()
	g.reps[st] = r
	g.mu.Unlock()
}

func (g *Gate) dropReport(st model.StationIdx) {
	g.mu.Lock()
	delete(g.reps, st)
	g.mu.Unlock()
}

// wallNow is the wall clock used only for report timestamps.
var wallNow = time.Now
