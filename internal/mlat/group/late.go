package group

import (
	"math/bits"

	"airview/internal/ingest"
	"airview/internal/model"
)

// Late copies (DESIGN §9.3 "Late copies (up to 2 s) still update the
// track"). Every emitted cluster is retained in a fixed ring for
// Config.LateHoldNs; copies that the ingest delivers after the flush
// (ingest.LateObserver) are joined to the retained cluster of the same
// payload they are consistent with, and the grown cluster is emitted again
// with Late set and its original Seq. Late copies that fit no retained
// cluster are clustered among themselves.
//
// Memory is bounded: LateSlots records of LateMembers copies each, found
// through 8-way hash buckets. Under overload the oldest records are
// overwritten first, which only shortens the effective hold time.

type lateMember struct {
	st     model.StationIdx
	ch     int8
	epoch  uint32
	ticks  uint64
	t      Time
	cv, jv float32
	sig    float32
}

type lateRec struct {
	h      header
	gen    uint64 // insertion generation (0 = never used)
	seq    uint64 // emitted cluster Seq (0 = a single copy, not emitted)
	n      int
	altFt  float64
	altSrc uint8
}

type bucketEntry struct {
	hash uint64
	gen  uint64
	slot int32
}

type lateStore struct {
	recs  []lateRec
	mem   []lateMember
	cap   int
	next  uint64
	bkt   []bucketEntry
	bmask uint64
}

const bucketWays = 8

func newLateStore(slots, members int) *lateStore {
	nb := 1
	for nb*bucketWays < slots*2 {
		nb <<= 1
	}
	return &lateStore{recs: make([]lateRec, slots), mem: make([]lateMember, slots*members), cap: members,
		bkt: make([]bucketEntry, nb*bucketWays), bmask: uint64(nb - 1)}
}

func (s *lateStore) bucket(hash uint64) []bucketEntry {
	b := (hash ^ hash>>29) & s.bmask
	return s.bkt[b*bucketWays : (b+1)*bucketWays]
}

// alloc takes the next ring slot for a record of hash and indexes it.
func (s *lateStore) alloc(hash uint64) (int, *lateRec) {
	slot := int(s.next % uint64(len(s.recs)))
	s.next++
	r := &s.recs[slot]
	*r = lateRec{gen: s.next}
	b := s.bucket(hash)
	w := 0
	for i := range b {
		e := &b[i]
		if e.gen == 0 || s.recs[e.slot].gen != e.gen { // free or overwritten
			w = i
			break
		}
		if e.gen < b[w].gen {
			w = i
		}
	}
	b[w] = bucketEntry{hash: hash, gen: r.gen, slot: int32(slot)}
	return slot, r
}

func (s *lateStore) members(slot int) []lateMember { return s.mem[slot*s.cap : (slot+1)*s.cap] }

// retain stores an emitted cluster, keeping its lowest-variance members.
func (s *lateStore) retain(cl *Cluster, capM int) {
	slot, r := s.alloc(cl.Hash)
	r.h = header{hash: cl.Hash, key: cl.Key, df: cl.DF, kind: cl.Kind, data: cl.Data, dlen: cl.DataLen, firstNs: cl.FirstNs}
	r.seq, r.altFt, r.altSrc = cl.Seq, cl.AltFt, cl.AltSrc
	dst := s.members(slot)
	if len(cl.Members) <= capM {
		for i := range cl.Members {
			dst[i] = toLate(&cl.Members[i])
		}
		r.n = len(cl.Members)
		return
	}
	// Keep the capM members with the smallest variance (selection by
	// repeated minimum over a bit set; capM, len <= 64).
	var used uint64
	for k := 0; k < capM; k++ {
		bi, bv := -1, 0.0
		for i := range cl.Members {
			if used&(1<<uint(i)) != 0 {
				continue
			}
			if v := cl.Members[i].Var(); bi < 0 || v < bv {
				bi, bv = i, v
			}
		}
		used |= 1 << uint(bi)
		dst[k] = toLate(&cl.Members[bi])
	}
	r.n = capM
}

// retainSingle stores the single usable copy of a group so that late copies
// can still form a cluster with it.
func (s *lateStore) retainSingle(h *header, c *cand) {
	slot, r := s.alloc(h.hash)
	r.h = *h
	s.members(slot)[0] = lateMember{st: c.st, ch: c.ch, epoch: c.epoch, ticks: c.ticks, t: c.t,
		cv: float32(c.cv), jv: float32(c.jv), sig: c.sig}
	r.n = 1
	r.altSrc = AltNone
}

func toLate(m *Member) lateMember {
	return lateMember{st: m.Station, ch: m.Channel, epoch: m.Epoch, ticks: m.Ticks, t: m.T,
		cv: float32(m.ClockVar), jv: float32(m.JitterVar), sig: m.Signal}
}

// find returns the live records of hash whose first copy is within hold of
// firstNs (at most bucketWays), most recent first.
func (s *lateStore) find(hash uint64, firstNs, hold int64, out *[bucketWays]int32) int {
	n := 0
	for _, e := range s.bucket(hash) {
		if e.gen == 0 || e.hash != hash {
			continue
		}
		r := &s.recs[e.slot]
		if r.gen != e.gen || r.h.hash != hash {
			continue
		}
		if d := firstNs - r.h.firstNs; d > hold || d < -hold {
			continue
		}
		out[n] = e.slot
		n++
	}
	// most recent (highest gen) first
	for i := 1; i < n; i++ {
		for j := i; j > 0 && s.recs[out[j]].gen > s.recs[out[j-1]].gen; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return n
}

// ObserveLate implements ingest.LateObserver: copies of an already flushed
// group that arrived after the flush.
func (a *Assembler) ObserveLate(g *ingest.Group) {
	a.stats.lateGroups.Add(1)
	if excluded(g) {
		return
	}
	if a.sel != nil && !a.sel.Candidate(g.Key, g.DF, g.FirstNs) {
		return
	}
	nl := a.load(g, a.lc[:], true)
	if nl == 0 {
		return
	}
	a.stats.lateCopies.Add(uint64(nl))
	var free uint64 = 1<<uint(nl) - 1
	if nl == 64 {
		free = ^uint64(0)
	}
	if a.late != nil {
		var slots [bucketWays]int32
		nr := a.late.find(g.Hash, g.FirstNs, a.cfg.LateHoldNs, &slots)
		for i := 0; i < nr && free != 0; i++ {
			free = a.joinLate(int(slots[i]), nl, free)
		}
	}
	if bits.OnesCount64(free) < 2 {
		return
	}
	// The rest: cluster among themselves.
	a.n = 0
	for m := free; m != 0; m &= m - 1 {
		a.c[a.n] = a.lc[bits.TrailingZeros64(m)]
		a.n++
	}
	a.prepare(g.Key)
	h := headerOf(g)
	altFt, altSrc := frameAltitude(g.Data)
	a.clusterAll(&h, altFt, altSrc, true)
}

// joinLate tries to join the free late copies (a.lc, bits of free) to the
// retained record in slot and returns the copies still free.
func (a *Assembler) joinLate(slot, nl int, free uint64) uint64 {
	s := a.late
	r := &s.recs[slot]
	mem := s.members(slot)[:r.n]
	// Working set: the retained members, then the free late copies whose
	// station is not in the record yet.
	a.n = 0
	var inRec uint64 // late-copy indices whose station already is a member
	for i := range mem {
		m := &mem[i]
		pos, ps, ok := a.sites.Site(m.st)
		if !ok {
			continue
		}
		c := &a.c[a.n]
		*c = cand{st: m.st, ch: m.ch, epoch: m.epoch, ticks: m.ticks, sig: m.sig, t: m.t,
			cv: float64(m.cv), jv: float64(m.jv), pos: pos, ps: ps}
		c.v = max(c.cv+c.jv, 1e-6)
		if a.doms != nil {
			c.dom = a.doms.Domain(m.st)
		}
		a.n++
		for f := free; f != 0; f &= f - 1 {
			j := bits.TrailingZeros64(f)
			if a.lc[j].st == m.st {
				inRec |= 1 << uint(j)
			}
		}
	}
	nOld := a.n
	if nOld == 0 {
		return free
	}
	var from [MaxMembers]int8
	for f := free &^ inRec; f != 0 && a.n < MaxMembers; f &= f - 1 {
		j := bits.TrailingZeros64(f)
		from[a.n] = int8(j)
		a.c[a.n] = a.lc[j]
		a.n++
	}
	if a.n == nOld {
		return free
	}
	a.prepare(r.h.key)
	if a.hinted {
		a.useHint()
	} else {
		a.useBaseline()
	}
	old := uint64(1)<<uint(nOld) - 1
	newM := (uint64(1)<<uint(a.n) - 1) &^ old
	p := newM
	for m := old; m != 0; m &= m - 1 {
		p &= a.srch.adj[bits.TrailingZeros64(m)]
	}
	if p == 0 {
		return free
	}
	a.srch.best, a.srch.bestN, a.srch.bestScore = 0, 0, 0
	a.srch.budget, a.srch.exhausted = a.cfg.SearchBudget, false
	a.srch.bk(old, nOld, p, 0)
	got := a.srch.best & newM
	if got == 0 {
		return free
	}
	for m := got; m != 0; m &= m - 1 {
		free &^= 1 << uint(from[bits.TrailingZeros64(m)])
	}
	a.stats.lateAttached.Add(uint64(bits.OnesCount64(got)))
	seq := r.seq
	if seq == 0 {
		a.seq++
		seq = a.seq
		r.seq = seq
	}
	altFt, altSrc := r.altFt, r.altSrc
	if altSrc == AltNone {
		altFt, altSrc = frameAltitude(r.h.data[:r.h.dlen])
	}
	h := r.h
	a.emit(h, a.c[:a.n], a.srch.best, 0, 1, seq, altFt, altSrc, a.hinted, false, true)
	// Grow the record with the joined copies (while there is room).
	for m := got; m != 0 && r.n < s.cap; m &= m - 1 {
		c := &a.c[bits.TrailingZeros64(m)]
		s.members(slot)[r.n] = lateMember{st: c.st, ch: c.ch, epoch: c.epoch, ticks: c.ticks, t: c.t,
			cv: float32(c.cv), jv: float32(c.jv), sig: c.sig}
		r.n++
	}
	return free
}
