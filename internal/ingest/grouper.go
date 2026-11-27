package ingest

import (
	"bytes"
	"math"
	"sync"
	"time"

	"airview/internal/model"
	"airview/internal/tracker"
)

// MaxCopies is the most copies a group stores. Further copies are still
// de-duplicated and credited to their lanes, but not handed to MLAT.
const MaxCopies = 64

// copyCaps are the size classes of the per-group copy slices.
var copyCaps = [...]int{4, 8, 16, 32, MaxCopies}

// Same-station repeat rule thresholds (DESIGN §6.2). A Mode S reply lasts at
// least 64 µs, so one clock cannot stamp two transmissions closer together,
// while both channels of a diversity receiver stamp one transmission within
// a few µs. DESIGN §6.2 says "1 ms"; interrogation bursts (in the simulator
// and in reality: ACAS and DF4/20 replies to closely spaced interrogations)
// produce identical replies 0.74 ms apart, so timed copies use sameTxTickNs.
// Untimed copies (host time only) use sameTxHostNs between channels.
const (
	sameTxTickNs = 32e3
	sameTxHostNs = 1e6
)

// gcfg is the grouper configuration (all times in ns).
type gcfg struct {
	window, flushAfter, retain, minHold int64
	maxMatch, minMatch, baseMatch       float64
	uniqueSkew                          int64
	clock                               ClockView
	clockKind                           ClockKindView // clock, when it implements it
	gateFloor                           float64       // ClockGateFloor, ns
	flush                               FlushPolicy
	sampleEvery                         uint64
}

// netTime converts a copy's ticks to network time for the synced-station
// gate: by frame kind (and with the receiver jitter in σ) when the view
// supports it.
func (cfg *gcfg) netTime(st model.StationIdx, epoch uint32, ticks uint64, kind model.FrameKind) (float64, float64, bool) {
	if cfg.clockKind != nil {
		return cfg.clockKind.NetTimeKind(st, epoch, ticks, kind)
	}
	return cfg.clock.NetTime(st, epoch, ticks)
}

// flushDelay returns the flush delay (ns) of a new group with payload d.
func (cfg *gcfg) flushDelay(d []byte) int64 {
	if cfg.flush == nil {
		return cfg.flushAfter
	}
	key, ok := tracker.RouteKey(d)
	if !ok {
		return cfg.flushAfter
	}
	v := int64(cfg.flush.Delay(key))
	if v <= 0 {
		return cfg.flushAfter
	}
	return min(max(v, int64(100*time.Millisecond)), cfg.window)
}

// sampled reports whether the group with payload hash h trains the
// FlushPolicy.
func (cfg *gcfg) sampled(h uint64) bool {
	return cfg.flush != nil && (cfg.sampleEvery <= 1 || ((h*0x9E3779B97F4A7C15)>>40)%cfg.sampleEvery == 0)
}

// entry is one transmission (a group) in a shard.
type entry struct {
	hash       uint64
	data       [14]byte
	dlen       uint8
	kind       model.FrameKind
	live       bool
	unique     bool
	ready      bool // result is set
	flushed    bool // the flush time has passed
	delivered  bool // handed to the MLAT sink at flush
	single     bool // delivered at flush with a single timed copy (sync candidate)
	gen        uint32
	next, prev int32 // same-hash chain: next = older, prev = newer (-1 = none)
	firstNs    int64
	firstSp    float64
	flushAt    int64
	finalAt    int64
	freeAt     int64
	nFlushed   int
	copies     []Copy
	result     tracker.Result
	// overflow holds the lane credits of copies that arrived while the
	// result was pending and did not fit in copies (MaxCopies bounds the
	// MLAT copies, not the statistics; SO-10). Bounded by maxOverflow.
	overflow []pendCredit
	// stations is a 1024-bit blocked Bloom filter of the stations in copies
	// (two bits per station, both in one 64-bit word, so a look-up reads one
	// word): the common "this station is not in the group yet" answer of
	// stationCopy needs no scan of up to MaxCopies copies (SO-4). With a full
	// group (64 stations) a station not in it still scans with probability
	// about 1.5 %, so a copy costs O(1) expected whatever the group size
	// (the earlier 256-bit, one-bit filter scanned in 22 % of the look-ups
	// of a full group; WP-SCALE-tracker).
	stations [stationWords]uint64
	// sidx indexes the first copy of each station once the group has more
	// than sidxMin copies (nil before): an open-addressing table of copy
	// index + 1 by station hash. A station that is in the group (the
	// same-station repeat rule meets it in every older group of a repeated
	// payload: DF11, unchanged DF4/5/20 replies) is then found in O(1)
	// instead of a scan of up to MaxCopies copies (WP-SCALE-tracker).
	sidx []uint16
}

const (
	stationWords = 16
	sidxBits     = 7
	sidxSize     = 1 << sidxBits // load <= MaxCopies/sidxSize = 1/2
	sidxMin      = 8
)

// The station index must keep at least one empty slot (probes end there)
// and copy index + 1 must fit its uint16 slots.
var _ [sidxSize - 2*MaxCopies]struct{}

// sidxSlot is the home slot of st in an entry's station index.
func sidxSlot(st model.StationIdx) int { return int((uint32(st) * 0x9E3779B1) >> (32 - sidxBits)) }

// indexStation records copy k (st's first copy) in e.sidx unless st is
// already there.
func (e *entry) indexStation(st model.StationIdx, k int) {
	for i := sidxSlot(st); ; i = (i + 1) & (sidxSize - 1) {
		v := e.sidx[i]
		if v == 0 {
			e.sidx[i] = uint16(k + 1)
			return
		}
		if e.copies[v-1].Station == st {
			return
		}
	}
}

// stationMask returns the word of st in entry.stations and its two bits.
func stationMask(st model.StationIdx) (int, uint64) {
	h := uint64(st) * 0x9E3779B97F4A7C15
	return int(h >> 60), 1<<((h>>54)&63) | 1<<((h>>48)&63)
}

// markStation records st in e's station filter.
func (e *entry) markStation(st model.StationIdx) {
	w, m := stationMask(st)
	e.stations[w] |= m
}

// maybeStation reports whether st may be in e (false = certainly not).
func (e *entry) maybeStation(st model.StationIdx) bool {
	w, m := stationMask(st)
	return e.stations[w]&m == m
}

// shardStats are counters kept under the shard lock.
type shardStats struct {
	groups      uint64 // entries created (unique transmissions)
	copies      uint64 // copies offered
	joined      uint64 // copies that joined an existing group
	joinUnique  uint64 // ... of them by the unique-payload rule beyond the tolerance
	repeats     uint64 // a station's repeat opened a new group (same-station rule)
	dups        uint64 // exact duplicates dropped
	sameStation uint64 // second channel of a station already in the group
	full        uint64 // copies not stored (MaxCopies)
	flushed     uint64 // groups handed to the MLAT sink
	late        uint64 // late deliveries (ObserveLate)
	redelivered uint64 // single-copy flushes delivered again with every copy
	live        int    // live entries
}

type shard struct {
	mu   sync.Mutex
	idx  hindex
	ents []entry
	free []int32
	// fifo holds entry indices in creation order; head <= finalSeq <= tail
	// and head <= flushSeq <= tail are absolute sequence numbers.
	fifo                           []int32
	head, flushSeq, finalSeq, tail uint64
	pool                           [len(copyCaps)][][]Copy
	sidxPool                       [][]uint16
	st                             shardStats
	_                              [64]byte
}

// ------------------------------------------------------------ hash index

// hindex is an open-addressing (linear probing) map from a payload hash to
// the newest entry of that hash. vals hold entry index + 1 (0 = empty).
type hindex struct {
	keys []uint64
	vals []int32
	n    int
	mask uint64
}

func (h *hindex) init(size int) {
	n := 64
	for n < size {
		n <<= 1
	}
	h.keys, h.vals, h.n, h.mask = make([]uint64, n), make([]int32, n), 0, uint64(n-1)
}

func (h *hindex) get(k uint64) int32 {
	if len(h.vals) == 0 {
		return -1
	}
	for i := k & h.mask; ; i = (i + 1) & h.mask {
		v := h.vals[i]
		if v == 0 {
			return -1
		}
		if h.keys[i] == k {
			return v - 1
		}
	}
}

func (h *hindex) set(k uint64, v int32) {
	if len(h.vals) == 0 || (h.n+1)*2 > len(h.vals) {
		h.grow()
	}
	for i := k & h.mask; ; i = (i + 1) & h.mask {
		if h.vals[i] == 0 {
			h.keys[i], h.vals[i] = k, v+1
			h.n++
			return
		}
		if h.keys[i] == k {
			h.vals[i] = v + 1
			return
		}
	}
}

func (h *hindex) del(k uint64) {
	if len(h.vals) == 0 {
		return
	}
	i := k & h.mask
	for {
		if h.vals[i] == 0 {
			return
		}
		if h.keys[i] == k {
			break
		}
		i = (i + 1) & h.mask
	}
	// backward-shift deletion
	j := i
	for {
		j = (j + 1) & h.mask
		if h.vals[j] == 0 {
			break
		}
		home := h.keys[j] & h.mask
		if (j-home)&h.mask >= (j-i)&h.mask {
			h.keys[i], h.vals[i] = h.keys[j], h.vals[j]
			i = j
		}
	}
	h.vals[i] = 0
	h.n--
}

func (h *hindex) grow() {
	ok, ov := h.keys, h.vals
	h.init(max(64, len(ov)*2))
	for i, v := range ov {
		if v != 0 {
			h.set(ok[i], v-1)
		}
	}
}

// ------------------------------------------------------------ shard helpers

func (sh *shard) getCopies(n int) []Copy {
	for c, size := range copyCaps {
		if size >= n {
			if p := sh.pool[c]; len(p) > 0 {
				s := p[len(p)-1]
				sh.pool[c] = p[:len(p)-1]
				return s[:0]
			}
			return make([]Copy, 0, size)
		}
	}
	return make([]Copy, 0, n)
}

func (sh *shard) putCopies(s []Copy) {
	for c, size := range copyCaps {
		if cap(s) == size {
			clear(s[:cap(s)])
			sh.pool[c] = append(sh.pool[c], s[:0])
			return
		}
	}
}

// getSidx returns a cleared station index.
func (sh *shard) getSidx() []uint16 {
	if n := len(sh.sidxPool); n > 0 {
		s := sh.sidxPool[n-1]
		sh.sidxPool = sh.sidxPool[:n-1]
		return s
	}
	return make([]uint16, sidxSize)
}

func (sh *shard) alloc() int32 {
	if n := len(sh.free); n > 0 {
		i := sh.free[n-1]
		sh.free = sh.free[:n-1]
		return i
	}
	sh.ents = append(sh.ents, entry{})
	return int32(len(sh.ents) - 1)
}

func (sh *shard) push(i int32) {
	if sh.tail-sh.head == uint64(len(sh.fifo)) {
		n := max(256, 2*len(sh.fifo))
		nf := make([]int32, n)
		for s := sh.head; s < sh.tail; s++ {
			nf[s&uint64(n-1)] = sh.fifo[s&uint64(len(sh.fifo)-1)]
		}
		sh.fifo = nf
	}
	sh.fifo[sh.tail&uint64(len(sh.fifo)-1)] = i
	sh.tail++
}

func (sh *shard) at(seq uint64) int32 { return sh.fifo[seq&uint64(len(sh.fifo)-1)] }

// stationCopy returns the index of st's first copy in e (-1 = none).
func stationCopy(e *entry, st model.StationIdx) int {
	if !e.maybeStation(st) {
		return -1 // certainly not in the group
	}
	if e.sidx != nil {
		for i := sidxSlot(st); ; i = (i + 1) & (sidxSize - 1) {
			v := e.sidx[i]
			if v == 0 {
				return -1
			}
			if e.copies[v-1].Station == st {
				return int(v - 1)
			}
		}
	}
	for i := range e.copies {
		if e.copies[i].Station == st {
			return i
		}
	}
	return -1
}

// sameTx applies the same-station repeat rule: is c the same transmission
// as k (both from one station)? Timed copies compare ticks; untimed copies
// can only be told apart by channel: a second copy on the same channel is a
// new transmission, one on another channel within 1 ms of host time is the
// diversity copy.
func sameTx(k, c *Copy) bool {
	if k.Ticks != 0 && c.Ticks != 0 && k.Epoch == c.Epoch && k.ClockCls == c.ClockCls && c.ClockCls.Timed() {
		d := float64(int64(c.Ticks - k.Ticks))
		if d < 0 {
			d = -d
		}
		return d*1e9/c.ClockCls.NominalHz() <= sameTxTickNs
	}
	if k.Channel == c.Channel {
		return false
	}
	d := c.HostNs - k.HostNs
	if d < 0 {
		d = -d
	}
	return d <= sameTxHostNs
}

type matchKind uint8

const (
	matchNone matchKind = iota // open a new group
	matchJoin                  // a new station's copy of an existing group
	matchSame                  // the station's other channel
	matchDup                   // an exact duplicate: drop
)

// match finds the group c belongs to. Caller holds sh.mu.
func (sh *shard) match(c *Copy, d []byte, kind model.FrameKind, h uint64, unique bool, sp float64, cfg *gcfg, now int64) (int32, matchKind) {
	best, bestD := int32(-1), int64(math.MaxInt64)
	var tn, tsig float64
	tnOK, tnDone := false, false
	viaUnique := false
	for i := sh.idx.get(h); i >= 0; i = sh.ents[i].next {
		e := &sh.ents[i]
		if e.freeAt <= now || int(e.dlen) != len(d) || !bytes.Equal(e.data[:e.dlen], d) {
			continue
		}
		if k := stationCopy(e, c.Station); k >= 0 {
			if !sameTx(&e.copies[k], c) {
				continue // the same-station repeat rule: a new transmission
			}
			for j := k; j < len(e.copies); j++ {
				if o := &e.copies[j]; o.Station == c.Station && o.Channel == c.Channel && o.Ticks == c.Ticks && o.HostNs == c.HostNs {
					return i, matchDup // resent by the feeder
				}
			}
			return i, matchSame
		}
		dd := c.CorrNs - e.firstNs
		if dd < 0 {
			dd = -dd
		}
		if dd >= bestD {
			continue
		}
		tol := cfg.baseMatch + 3*(sp+e.firstSp)
		tol = math.Min(math.Max(tol, cfg.minMatch), cfg.maxMatch)
		ok, uq := float64(dd) <= tol, false
		if !ok && unique && e.unique && dd <= cfg.uniqueSkew {
			ok, uq = true, true
		}
		if ok && cfg.clock != nil && c.Timed {
			if !tnDone {
				tn, tsig, tnOK = cfg.netTime(c.Station, c.Epoch, c.Ticks, kind)
				tnDone = true
			}
			if tnOK {
				ok = clockAdmit(cfg, e, c.Station, tn, tsig, kind)
			}
		}
		if ok {
			best, bestD, viaUnique = i, dd, uq
		}
	}
	if best < 0 {
		return -1, matchNone
	}
	if viaUnique {
		sh.st.joinUnique++
	}
	return best, matchJoin
}

// clockAdmit implements the synced-station rule of DESIGN §6.2: when both
// stations are synced, |Δt_net| must be at most d/c + max(5σ,
// ClockGateFloor) against the group's first synced timed copy (both times
// of the group's frame kind when the view supports it).
func clockAdmit(cfg *gcfg, e *entry, st model.StationIdx, tn, sig float64, kind model.FrameKind) bool {
	for k := range e.copies {
		o := &e.copies[k]
		if !o.Timed {
			continue
		}
		to, so, ok := cfg.netTime(o.Station, o.Epoch, o.Ticks, kind)
		if !ok {
			continue
		}
		base, ok := cfg.clock.BaselineNs(st, o.Station)
		if !ok {
			return true
		}
		return math.Abs(tn-to) <= base+max(5*math.Sqrt(sig*sig+so*so), cfg.gateFloor)
	}
	return true
}

// add stores c in group i. It reports false when the group is full (the
// copy is then not stored). Caller holds sh.mu.
func (sh *shard) add(i int32, c *Copy, sp float64) bool {
	e := &sh.ents[i]
	if len(e.copies) >= MaxCopies {
		sh.st.full++
		return false
	}
	if len(e.copies) == cap(e.copies) {
		n := sh.getCopies(cap(e.copies) + 1)
		n = append(n, e.copies...)
		sh.putCopies(e.copies)
		e.copies = n
	}
	e.copies = append(e.copies, *c)
	e.markStation(c.Station)
	switch k := len(e.copies) - 1; {
	case e.sidx != nil:
		e.indexStation(c.Station, k)
	case k >= sidxMin:
		e.sidx = sh.getSidx()
		for j := range e.copies {
			e.indexStation(e.copies[j].Station, j)
		}
	}
	if !c.SameStation && c.CorrNs < e.firstNs {
		e.firstNs, e.firstSp = c.CorrNs, sp
	}
	return true
}

// open creates a group whose first copy is c. Caller holds sh.mu.
func (sh *shard) open(c *Copy, d []byte, kind model.FrameKind, h uint64, unique bool, sp float64, cfg *gcfg, now int64) (int32, uint32) {
	i := sh.alloc()
	e := &sh.ents[i]
	gen := e.gen + 1
	*e = entry{hash: h, kind: kind, live: true, unique: unique, gen: gen, next: -1, prev: -1,
		firstNs: c.CorrNs, firstSp: sp}
	e.dlen = uint8(copy(e.data[:], d))
	e.flushAt = max(c.CorrNs+cfg.flushDelay(d), now+cfg.minHold)
	e.finalAt = max(c.CorrNs+cfg.window, e.flushAt)
	e.freeAt = e.finalAt + cfg.retain
	e.copies = append(sh.getCopies(1), *c)
	e.markStation(c.Station)
	if old := sh.idx.get(h); old >= 0 {
		e.next = old
		sh.ents[old].prev = i
	}
	sh.idx.set(h, i)
	sh.push(i)
	sh.st.groups++
	sh.st.live++
	return i, gen
}

// release frees entry i. Caller holds sh.mu.
func (sh *shard) release(i int32) {
	e := &sh.ents[i]
	if e.prev >= 0 {
		sh.ents[e.prev].next = e.next
	} else if e.next >= 0 {
		sh.idx.set(e.hash, e.next)
	} else {
		sh.idx.del(e.hash)
	}
	if e.next >= 0 {
		sh.ents[e.next].prev = e.prev
	}
	sh.putCopies(e.copies)
	if e.sidx != nil {
		clear(e.sidx)
		sh.sidxPool = append(sh.sidxPool, e.sidx)
	}
	gen := e.gen
	ov := e.overflow[:0]
	*e = entry{gen: gen, next: -1, prev: -1, overflow: ov}
	sh.free = append(sh.free, i)
	sh.st.live--
}

// ------------------------------------------------------------ sweeping

// sweeper is the state of the single goroutine that flushes groups.
type sweeper struct {
	groups []Group // groups to deliver (buffers reused)
	late   []bool  // per group: a late delivery
	data   [][14]byte
	learn  []learnSample
	flush  []flushSample
	ngrp   int32
	tmp    []float64
}

// learnSample is one copy of a group used by the latency estimator: raw is
// its corrected time without the learned latency; grp numbers the group.
type learnSample struct {
	st   model.StationIdx
	raw  int64
	grp  int32
	addr uint32 // the squitter's address (hold-out exclusion)
}

// next returns a cleared group slot.
func (sw *sweeper) next(late bool) *Group {
	n := len(sw.groups)
	if n < cap(sw.groups) {
		sw.groups = sw.groups[:n+1]
	} else {
		sw.groups = append(sw.groups, Group{})
		sw.data = append(sw.data, [14]byte{})
	}
	if n >= len(sw.data) {
		sw.data = append(sw.data, [14]byte{})
	}
	if n >= len(sw.late) {
		sw.late = append(sw.late, late)
	} else {
		sw.late = sw.late[:n+1]
		sw.late[n] = late
	}
	g := &sw.groups[n]
	cp := g.Copies[:0]
	*g = Group{Copies: cp}
	return g
}

// fill copies entry e (copies from index from on) into g.
func (sw *sweeper) fill(g *Group, slot int, e *entry, from int) {
	sw.data[slot] = e.data
	g.Hash, g.Data, g.Kind = e.hash, sw.data[slot][:e.dlen], e.kind
	g.FirstNs = e.firstNs
	if e.ready {
		g.Result, g.HasResult = e.result, true
		g.Key, g.DF = e.result.Key, e.result.DF
	}
	if g.Key == 0 && e.dlen >= 7 {
		g.DF = e.data[0] >> 3
		if a, ok := tracker.RouteKey(e.data[:e.dlen]); ok {
			g.Key = a
		}
	}
	g.Copies = append(g.Copies[:0], e.copies[from:]...)
	g.Timed = 0
	for k := range g.Copies {
		g.Copies[k].pend = false
		if g.Copies[k].Timed {
			g.Timed++
		}
	}
	g.Earlier = from
}

// timedStations counts the distinct stations with a timed copy in e.
func timedStations(e *entry) (n int, first model.StationIdx) {
	for k := range e.copies {
		if e.copies[k].Timed {
			if n == 0 {
				first = e.copies[k].Station
			}
			n++
		}
	}
	return
}

// sweepShard flushes, finalises and frees the due entries of one shard into
// sw. Caller is the single sweeper.
func (ing *Ingest) sweepShard(sh *shard, sw *sweeper, now int64) {
	cfg := &ing.cfg
	sink := ing.mlat
	late, _ := sink.(LateObserver)
	sh.mu.Lock()
	for sh.flushSeq < sh.tail {
		i := sh.at(sh.flushSeq)
		e := &sh.ents[i]
		if e.live && e.flushAt > now {
			break
		}
		sh.flushSeq++
		if !e.live {
			continue
		}
		e.flushed = true
		e.nFlushed = len(e.copies)
		if sink == nil {
			continue
		}
		n, first := timedStations(e)
		ok := n >= 2
		if !ok && n == 1 && cfg.clock != nil {
			key, df := e.result.Key, e.result.DF
			if !e.ready && e.dlen >= 7 {
				df = e.data[0] >> 3
				key, _ = tracker.RouteKey(e.data[:e.dlen])
			}
			if cfg.clock.SyncCandidate(key, df) {
				for k := range e.copies {
					if c := &e.copies[k]; c.Timed && c.Station == first {
						_, _, ok = cfg.clock.NetTime(c.Station, c.Epoch, c.Ticks)
						break
					}
				}
			}
		}
		if ok {
			e.delivered = true
			e.single = n < 2
			slot := len(sw.groups)
			sw.fill(sw.next(false), slot, e, 0)
			sh.st.flushed++
		}
	}
	if sh.finalSeq < sh.flushSeq {
		for sh.finalSeq < sh.flushSeq {
			i := sh.at(sh.finalSeq)
			e := &sh.ents[i]
			if e.live && e.finalAt > now {
				break
			}
			sh.finalSeq++
			if !e.live {
				continue
			}
			if !e.delivered && sink != nil {
				// C2: too few timed stations at the flush, but a slower
				// link's copy joined before the window closed: the group
				// goes to MLAT on close (DESIGN §6.2) with every copy.
				if n, _ := timedStations(e); n >= 2 {
					e.delivered = true
					slot := len(sw.groups)
					sw.fill(sw.next(false), slot, e, 0)
					sh.st.flushed++
				}
			} else if nt, _ := timedStations(e); e.delivered && e.single && sink != nil && nt >= 2 {
				// Flushed early with one synced copy of a sync candidate;
				// other stations' copies arrived since: deliver the whole
				// group once more (a late delivery of the new copies alone
				// would be useless for clock sync).
				slot := len(sw.groups)
				g := sw.next(false)
				sw.fill(g, slot, e, 0)
				g.Redelivery = true
				sh.st.redelivered++
			} else if e.delivered && late != nil && len(e.copies) > e.nFlushed {
				hasTimed := false
				for k := e.nFlushed; k < len(e.copies); k++ {
					hasTimed = hasTimed || e.copies[k].Timed
				}
				if hasTimed {
					slot := len(sw.groups)
					sw.fill(sw.next(true), slot, e, e.nFlushed)
					sh.st.late++
				}
			}
			sw.collectLearn(e)
			if cfg.sampled(e.hash) {
				sw.collectFlush(e)
			}
		}
	}
	for sh.head < sh.finalSeq {
		i := sh.at(sh.head)
		e := &sh.ents[i]
		if e.live && e.freeAt > now {
			break
		}
		sh.head++
		if e.live {
			sh.release(i)
		}
	}
	sh.mu.Unlock()
}

// flushSample is one copy's delivery latency for the FlushPolicy.
type flushSample struct {
	st    model.StationIdx
	key   uint32 // aircraft key (0 = unknown)
	delay int64  // server arrival − corrected antenna time, ns
}

// collectFlush records the delivery latency of every station's copy of a
// finalised group (the FlushPolicy's training data; sampled groups only).
func (sw *sweeper) collectFlush(e *entry) {
	key := e.result.Key
	if !e.ready {
		key, _ = tracker.RouteKey(e.data[:e.dlen])
	}
	for k := range e.copies {
		c := &e.copies[k]
		if c.SameStation || c.recv == 0 {
			continue
		}
		sw.flush = append(sw.flush, flushSample{st: c.Station, key: key, delay: c.recv - c.CorrNs})
	}
}

// collectLearn records the copies of a unique-payload group (a position
// squitter: its membership is certain) for the latency estimator, one copy
// per station. Ambiguous payloads (DF11, DF4/5 repeats) are never learned
// from, so a mis-grouped copy cannot pull a station's correction away.
func (sw *sweeper) collectLearn(e *entry) {
	if !e.unique {
		return
	}
	n := 0
	for k := range e.copies {
		if !e.copies[k].SameStation {
			n++
		}
	}
	if n < 2 {
		return
	}
	// A unique payload is an extended squitter: AA in bytes 1..3.
	addr := uint32(e.data[1])<<16 | uint32(e.data[2])<<8 | uint32(e.data[3])
	for k := range e.copies {
		if c := &e.copies[k]; !c.SameStation {
			sw.learn = append(sw.learn, learnSample{st: c.Station, raw: c.raw, grp: sw.ngrp, addr: addr})
		}
	}
	sw.ngrp++
}
