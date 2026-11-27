package engine

import (
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/ingest/beastin"
	"airview/internal/model"
	"airview/internal/tracker"
)

// NetIngest is the aggregator ingest pipeline as the station runtime uses
// it (docs/aggregator/DESIGN.md §2, §6.2): every station's frames enter
// here, are grouped into transmissions, and reach the tracker (first copy)
// or the lanes (later copies).
//
// The station-aware ingest core of internal/ingest (WP-I: sessions,
// host-offset estimator, normaliser, grouper with the full repeat rule,
// FLOW and budgets) implements this interface; until it is wired the engine
// uses its own minimal grouper (newGroupIngest), which gives correct
// de-duplication for the map and the lanes but no MLAT groups.
//
// Every method must be safe for concurrent use and must not block for long.
type NetIngest interface {
	// HandleFrame takes one copy of a transmission. f.Station is set.
	HandleFrame(f model.RawFrame)
	// HandleUpdate takes a decoded update (SBS/JSON inputs, MLAT results).
	HandleUpdate(u model.DecodedUpdate)
	// HandleBeast takes a batch of the compat Beast listener (map and stats
	// only, never MLAT; beastin.Batch.MapStatsOnly).
	HandleBeast(b *beastin.Batch)
}

// NetIngestStats are the counters of the engine's fallback grouper.
type NetIngestStats struct {
	Copies   uint64 `json:"copies"`   // frames offered
	Unique   uint64 `json:"unique"`   // transmissions handed to the tracker
	Credits  uint64 `json:"credits"`  // later copies credited to lanes
	Repeats  uint64 `json:"repeats"`  // same-station repeats opened as a new transmission
	Pending  uint64 `json:"pending"`  // copies of not-approved stations (own lane only)
	Late     uint64 `json:"late"`     // backfilled copies (lanes only)
	Updates  uint64 `json:"updates"`  // decoded updates passed on
	Unknown  uint64 `json:"unknown"`  // copies of stations the resolver does not know (dropped)
	Overflow uint64 `json:"overflow"` // copies beyond the per-transmission copy list (credited, not recorded)
}

// Grouper parameters (DESIGN §6.2).
const (
	groupShards    = 64
	groupWindow    = time.Second // a transmission's copies arrive within this
	groupMaxCopies = 32          // copies remembered per transmission
)

// groupIngest is the engine's minimal grouper. It keys transmissions by a
// 64-bit hash of the payload bytes within a one-second arrival window:
//
//   - the first copy goes to tracker.Handle and its Result is cached;
//   - later copies are credited with Lanes.Credit and that Result (no
//     tracker mutex);
//   - a copy from a station and channel that already contributed to the
//     entry is a repeated transmission (DF11 squitters, identical DF17
//     frames) and opens a new entry: the simplified form of the
//     same-station repeat rule, by channel instead of receiver ticks,
//     because compat Beast ticks are not trustworthy (§5.1);
//   - stations that are not approved feed their own lane only (§3
//     admission): their copies go to Lanes.Backfill, never the live map;
//   - late (backfilled) frames bypass the grouper.
//
// It allocates only when a shard's map grows; entries are recycled.
type groupIngest struct {
	h   tracker.NetHandler
	res model.StationResolver
	now func() int64 // monotonic-ish arrival clock, ns

	shards [groupShards]groupShard

	copies, unique, credits, repeats  atomic.Uint64
	pending, late, updates, unknownSt atomic.Uint64
	overflow                          atomic.Uint64
}

type groupShard struct {
	mu        sync.Mutex
	cur, prev map[uint64]*groupEntry
	rotated   int64 // arrival ns of the last rotation
	free      []*groupEntry
	_         [32]byte // keep shards on separate cache lines
}

type groupEntry struct {
	res   tracker.Result
	first int64 // arrival ns of the first copy
	n     int
	st    [groupMaxCopies]model.StationIdx
	ch    [groupMaxCopies]int8
}

// newGroupIngest returns the fallback grouper in front of h. res decides
// admission (nil = every station is approved).
func newGroupIngest(h tracker.NetHandler, res model.StationResolver) *groupIngest {
	g := &groupIngest{h: h, res: res}
	start := time.Now()
	g.now = func() int64 { return int64(time.Since(start)) + 1 }
	for i := range g.shards {
		g.shards[i].cur = make(map[uint64]*groupEntry, 256)
		g.shards[i].prev = make(map[uint64]*groupEntry, 256)
	}
	return g
}

// payloadHash is FNV-1a 64 over the frame bytes (no allocation).
func payloadHash(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	// Final avalanche so that the shard bits are well mixed.
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	return h
}

// approved reports whether frames of st may reach the live map.
func (g *groupIngest) approved(st model.StationIdx) (approved, known bool) {
	if g.res == nil || st == model.LocalStation {
		return true, true
	}
	m, ok := g.res.Info(st)
	if !ok {
		return false, false
	}
	return m.Admission == model.AdmApproved, true
}

// HandleFrame implements NetIngest.
func (g *groupIngest) HandleFrame(f model.RawFrame) {
	g.copies.Add(1)
	if f.Late {
		g.late.Add(1)
		g.h.Handle(f) // routes to Lanes.Backfill
		return
	}
	ok, known := g.approved(f.Station)
	if !known {
		g.unknownSt.Add(1)
		return
	}
	if !ok {
		g.pending.Add(1)
		if ls := g.h.Lanes(); ls != nil {
			ls.Backfill(f)
		}
		return
	}
	if len(f.Data) == 0 {
		// Mode A/C or status frames: nothing to group.
		g.unique.Add(1)
		g.h.Handle(f)
		return
	}
	key := payloadHash(f.Data)
	sh := &g.shards[key%groupShards]
	now := g.now()
	ch := int8(f.Channel)

	sh.mu.Lock()
	if now-sh.rotated >= int64(groupWindow) {
		sh.rotate(now)
	}
	e := sh.cur[key]
	if e == nil {
		if pe := sh.prev[key]; pe != nil && now-pe.first < int64(groupWindow) {
			e = pe
		}
	}
	if e != nil {
		repeat := false
		for i := 0; i < e.n; i++ {
			if e.st[i] == f.Station && e.ch[i] == ch {
				repeat = true
				break
			}
		}
		if !repeat {
			if e.n < groupMaxCopies {
				e.st[e.n], e.ch[e.n] = f.Station, ch
				e.n++
			} else {
				g.overflow.Add(1)
			}
			if ls := g.h.Lanes(); ls != nil {
				ls.Credit(f.Station, f.Channel, f.Signal, &e.res)
			}
			sh.mu.Unlock()
			g.credits.Add(1)
			return
		}
		g.repeats.Add(1)
	}
	// A new transmission (or a repeat): the first copy decides.
	ne := sh.alloc()
	ne.first, ne.n = now, 1
	ne.st[0], ne.ch[0] = f.Station, ch
	ne.res = g.h.Handle(f)
	if old := sh.cur[key]; old != nil {
		sh.free = append(sh.free, old)
	}
	sh.cur[key] = ne
	sh.mu.Unlock()
	g.unique.Add(1)
}

// rotate starts a new window generation. Caller holds mu.
func (sh *groupShard) rotate(now int64) {
	for k, e := range sh.prev {
		if sh.cur[k] != e {
			sh.free = append(sh.free, e)
		}
	}
	clear(sh.prev)
	sh.prev, sh.cur = sh.cur, sh.prev
	sh.rotated = now
}

func (sh *groupShard) alloc() *groupEntry {
	if n := len(sh.free); n > 0 {
		e := sh.free[n-1]
		sh.free = sh.free[:n-1]
		*e = groupEntry{}
		return e
	}
	return &groupEntry{}
}

// HandleUpdate implements NetIngest.
func (g *groupIngest) HandleUpdate(u model.DecodedUpdate) {
	g.updates.Add(1)
	if u.Late {
		g.late.Add(1)
		g.h.HandleUpdate(u)
		return
	}
	ok, known := g.approved(u.Station)
	if !known {
		g.unknownSt.Add(1)
		return
	}
	if !ok {
		g.pending.Add(1)
		if ls := g.h.Lanes(); ls != nil {
			ls.BackfillUpdate(u)
		}
		return
	}
	g.h.HandleUpdate(u)
}

// HandleBeast implements NetIngest (and beastin.Sink). The batch's clock
// events are not needed: compat Beast frames never reach MLAT.
func (g *groupIngest) HandleBeast(b *beastin.Batch) {
	for i := range b.Frames {
		g.HandleFrame(b.Frames[i])
	}
}

// Stats returns the grouper counters.
func (g *groupIngest) Stats() NetIngestStats {
	return NetIngestStats{
		Copies: g.copies.Load(), Unique: g.unique.Load(), Credits: g.credits.Load(),
		Repeats: g.repeats.Load(), Pending: g.pending.Load(), Late: g.late.Load(),
		Updates: g.updates.Load(), Unknown: g.unknownSt.Load(), Overflow: g.overflow.Load(),
	}
}
