package plan

import (
	"cmp"
	"math"
	"slices"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

type acSnap struct {
	key    uint32
	prio   Prio // PrioNone: a sync aircraft
	rate   float32
	posOK  bool
	pos    geo.Vec3
	h0, h1 int32 // hearers in w.hs
	r0, r1 int32 // MLAT requests in w.req
	sigmaH float64
}

type hSnap struct {
	si  int32
	sig int8
}

type stSnap struct {
	idx      model.StationIdx
	info     StationInfo
	ok       bool
	w        float64 // sync peer weight
	sigM     float64 // timing 1-sigma in metres
	sc0, sc1 int32   // sync candidates in w.scand
	mq0, mq1 int32   // MLAT requests (indices into w.req) in w.sreq
	ch0, ch1 int32   // chosen sync keys in w.chosen
	m0, m1   int32   // chosen MLAT keys in w.chosen
}

type request struct {
	ai, si int32
	prio   Prio
	gain   float64
}

type workspace struct {
	ac      []acSnap
	hs      []hSnap
	st      []stSnap
	stIdx   map[model.StationIdx]int32
	acByKey map[uint32]int32
	scand   []int32
	req     []request
	sreq    []int32
	cnt     []int32
	cov     []float32
	touched []int32
	heap    []heapItem
	cand    []int32
	chosen  []uint32
	keys    []uint32
	idle    []model.StationIdx
	d       Delta
	bufs    [6][]uint32
}

func (w *workspace) init() {
	w.stIdx = map[model.StationIdx]int32{}
	w.acByKey = map[uint32]int32{}
}

func (w *workspace) station(st model.StationIdx) int32 {
	if i, ok := w.stIdx[st]; ok {
		return i
	}
	i := int32(len(w.st))
	w.st = append(w.st, stSnap{idx: st})
	w.stIdx[st] = i
	return i
}

// timingSigmaM returns a station's timing 1-sigma in metres for geometry
// weighting: the reported sigma, else the class prior; unsynced stations are
// counted at 4x plus 300 ns (they are useful once they sync, not now).
func timingSigmaM(in StationInfo) float64 {
	ns := in.SigmaNs
	if !(ns > 0) {
		ns = in.Class.PriorJitterNs()
		if !(ns > 0) {
			ns = 400
		}
		ns = math.Hypot(ns, in.Class.SyncedSigmaNs())
	}
	if !in.Synced {
		ns = 4*ns + 300
	}
	return ns * geo.C * 1e-9
}

// Replan recomputes every station's syncSet and mlatSet and every MLAT
// aircraft's station plan, and sends the changes to Out. Run calls it every
// ReplanEvery; tests and the engine may call it directly.
func (p *Planner) Replan(nowNs int64) {
	start := time.Now()
	p.rp.Lock()
	defer p.rp.Unlock()
	cfg := &p.cfg
	w := &p.work
	w.ac, w.hs, w.st, w.scand, w.req, w.sreq, w.chosen = w.ac[:0], w.hs[:0], w.st[:0], w.scand[:0], w.req[:0], w.sreq[:0], w.chosen[:0]
	clear(w.stIdx)
	clear(w.acByKey)
	sum := Summary{AtNs: nowNs}

	// 1. Snapshot the aircraft heard within HeardTTL.
	minDs := p.t.ds(nowNs - int64(cfg.HeardTTL))
	for i := range p.t.sh {
		s := &p.t.sh[i]
		s.mu.Lock()
		for _, j := range s.idx {
			e := &s.recs[j]
			if e.lastSeen < nowNs-int64(cfg.HeardTTL) {
				continue
			}
			a := acSnap{key: e.key, prio: p.prio(e, nowNs), rate: e.rate, h0: int32(len(w.hs))}
			for k := range e.h {
				if h := &e.h[k]; h.lastDs >= minDs {
					w.hs = append(w.hs, hSnap{si: w.station(h.st), sig: h.sig})
				}
			}
			a.h1 = int32(len(w.hs))
			if a.h1 == a.h0 {
				continue
			}
			w.acByKey[e.key] = int32(len(w.ac))
			w.ac = append(w.ac, a)
		}
		s.mu.Unlock()
	}
	sum.Aircraft = len(w.ac)
	sum.Forgotten = p.t.forget(nowNs - int64(10*cfg.HeardTTL))

	// 2. Station state.
	for i := range w.st {
		s := &w.st[i]
		s.info, s.ok = p.stations.Station(s.idx)
		switch {
		case !s.ok || !s.info.Located:
			s.w = 0
		case s.info.Anchor:
			s.w = 3
		case s.info.Synced:
			s.w = 2
		default:
			s.w = 1
		}
		s.sigM = timingSigmaM(s.info)
	}
	sum.Stations = len(w.st)
	if cap(w.cnt) < len(w.st)+1 {
		w.cov = make([]float32, len(w.st))
		w.cnt = make([]int32, len(w.st)+1)
	}
	w.cov, w.cnt = w.cov[:len(w.st)], w.cnt[:len(w.st)+1]

	// 3. Sync candidates per station (CSR).
	clear(w.cnt)
	for i := range w.ac {
		if a := &w.ac[i]; a.prio == PrioNone {
			for _, h := range w.hs[a.h0:a.h1] {
				w.cnt[h.si+1]++
			}
		}
	}
	for i := 1; i < len(w.cnt); i++ {
		w.cnt[i] += w.cnt[i-1]
	}
	w.scand = slices.Grow(w.scand, int(w.cnt[len(w.st)]))[:w.cnt[len(w.st)]]
	for i := range w.st {
		w.st[i].sc0, w.st[i].sc1 = w.cnt[i], w.cnt[i]
	}
	for i := range w.ac {
		if a := &w.ac[i]; a.prio == PrioNone {
			for _, h := range w.hs[a.h0:a.h1] {
				s := &w.st[h.si]
				w.scand[s.sc1] = int32(i)
				s.sc1++
			}
		}
	}

	// 4. MLAT plans per candidate aircraft.
	for i := range w.ac {
		a := &w.ac[i]
		if a.prio >= NumPrio {
			continue
		}
		sum.Candidates[a.prio]++
		if p.pos != nil {
			a.pos, _, a.posOK = p.pos.Approx(a.key)
		}
		w.selectStations(int32(i), cfg)
		if a.r1-a.r0 < 4 {
			sum.Underserved++
		}
	}
	sum.Requests = len(w.req)

	// 5. Requests per station, best first: (priority, gain).
	clear(w.cnt)
	for _, r := range w.req {
		w.cnt[r.si+1]++
	}
	for i := 1; i < len(w.cnt); i++ {
		w.cnt[i] += w.cnt[i-1]
	}
	w.sreq = slices.Grow(w.sreq, len(w.req))[:len(w.req)]
	for i := range w.st {
		w.st[i].mq0, w.st[i].mq1 = w.cnt[i], w.cnt[i]
	}
	for i, r := range w.req {
		s := &w.st[r.si]
		w.sreq[s.mq1] = int32(i)
		s.mq1++
	}

	// 6. Per station: sync set, then MLAT set within the caps.
	for si := range w.st {
		s := &w.st[si]
		ss := p.state[s.idx]
		var cur map[uint32]int64
		if ss != nil {
			cur = ss.sync
		}
		s.ch0 = int32(len(w.chosen))
		var fps float64
		if p.survey.has(s.idx) {
			fps = w.selectSurveySync(int32(si), cfg, p.pos, cfg.StationFPSBudget/2)
			sum.SurveyTargets++
		} else {
			fps = w.selectSync(int32(si), cfg, cur, cfg.StationFPSBudget/2)
		}
		s.ch1 = int32(len(w.chosen))
		mq := w.sreq[s.mq0:s.mq1]
		slices.SortFunc(mq, func(x, y int32) int {
			a, b := &w.req[x], &w.req[y]
			if c := cmp.Compare(a.prio, b.prio); c != 0 {
				return c
			}
			return cmp.Compare(b.gain, a.gain)
		})
		s.m0 = int32(len(w.chosen))
		for _, ri := range mq {
			a := &w.ac[w.req[ri].ai]
			if int(len(w.chosen))-int(s.m0) >= cfg.MLATSetMax || fps+float64(a.rate) > cfg.StationFPSBudget {
				sum.Dropped++
				continue
			}
			fps += float64(a.rate)
			w.chosen = append(w.chosen, a.key)
		}
		s.m1 = int32(len(w.chosen))
	}

	// 7. Deltas.
	for si := range w.st {
		s := &w.st[si]
		ss := p.state[s.idx]
		if ss == nil {
			ss = &stState{sync: map[uint32]int64{}, mlat: map[uint32]int64{}}
			p.state[s.idx] = ss
		}
		if p.applyDelta(s.idx, ss, w.chosen[s.ch0:s.ch1], w.chosen[s.m0:s.m1], s, nowNs) {
			sum.Deltas++
		}
		sum.SyncMembers += len(ss.sync)
		sum.MLATMembers += len(ss.mlat)
	}
	// Stations that heard nothing within HeardTTL: empty their sets (in
	// station order, so the Interest calls are reproducible).
	w.idle = w.idle[:0]
	for st := range p.state {
		if _, ok := w.stIdx[st]; !ok {
			w.idle = append(w.idle, st)
		}
	}
	slices.Sort(w.idle)
	for _, st := range w.idle {
		ss := p.state[st]
		if len(ss.sync)+len(ss.mlat) > 0 && p.applyDelta(st, ss, nil, nil, nil, nowNs) {
			sum.Deltas++
		}
		if len(ss.sync)+len(ss.mlat) == 0 {
			delete(p.state, st)
		}
	}

	// 8. Mode A/C participants.
	if cfg.ModeAC.Enabled {
		m := map[model.StationIdx]struct{}{}
		lim := cfg.ModeAC.MaxBaselineKm * 1000
		for i := range w.st {
			a := &w.st[i]
			if !a.ok || !a.info.Anchor || !a.info.Located {
				continue
			}
			peers := 0
			for j := range w.st {
				b := &w.st[j]
				if i != j && b.ok && b.info.Anchor && b.info.Located && a.info.Pos.Dist(b.info.Pos) <= lim {
					peers++
				}
			}
			if peers >= cfg.ModeAC.MinPeers {
				m[a.idx] = struct{}{}
			}
		}
		p.modeACNotify(m)
		p.modeA.Store(&m)
		sum.ModeAC = len(m)
	} else {
		p.modeACNotify(nil)
		p.modeA.Store(nil)
	}

	sum.Took = time.Since(start)
	p.last.Store(&sum)
	p.replans.Add(1)
}

// applyDelta moves station st's sets towards the desired ones (subject to
// MinDwell and MaxChurn) and sends the change. s is the station's snapshot
// (nil when it heard nothing). It reports whether a non-empty delta was sent.
func (p *Planner) applyDelta(st model.StationIdx, ss *stState, wantSync, wantMLAT []uint32, s *stSnap, nowNs int64) bool {
	w := &p.work
	cfg := &p.cfg
	d := &w.d
	for i := range w.bufs {
		w.bufs[i] = w.bufs[i][:0]
	}
	*d = Delta{}
	heard := func(key uint32) bool {
		if s == nil {
			return false
		}
		ai, ok := w.acByKey[key]
		if !ok {
			return false
		}
		a := &w.ac[ai]
		for _, h := range w.hs[a.h0:a.h1] {
			if w.st[h.si].idx == st {
				return true
			}
		}
		return false
	}
	reset := !ss.told
	budget := cfg.MaxChurn
	if reset {
		budget = math.MaxInt32
	}
	// diff computes adds (in want order = best first) and removals with the
	// dwell rule; stale members (not heard any more) always go.
	diff := func(cur map[uint32]int64, want []uint32, add, del *[]uint32) {
		for _, k := range want {
			if _, ok := cur[k]; !ok {
				*add = append(*add, k)
			}
		}
		w.keys = w.keys[:0]
		for _, k := range want {
			w.keys = append(w.keys, k)
		}
		slices.Sort(w.keys)
		n0 := len(*del)
		for k, at := range cur {
			if _, found := slices.BinarySearch(w.keys, k); found {
				continue
			}
			if heard(k) && nowNs-at < int64(cfg.MinDwell) {
				continue
			}
			*del = append(*del, k)
		}
		// Map order must not decide which removals fit the churn budget:
		// sorted, a run is bit-reproducible (H-track).
		slices.Sort((*del)[n0:])
	}
	syncAdd, syncDel, mlatAdd, mlatDel := &w.bufs[0], &w.bufs[1], &w.bufs[2], &w.bufs[3]
	diff(ss.sync, wantSync, syncAdd, syncDel)
	diff(ss.mlat, wantMLAT, mlatAdd, mlatDel)
	// Churn limit: stale removals are free; then MLAT adds (Mode S-only
	// aircraft first), sync adds, and the other removals.
	take := func(src *[]uint32, stale bool) []uint32 {
		out := (*src)[:0]
		var rest []uint32
		for _, k := range *src {
			if stale && !heard(k) {
				out = append(out, k)
				continue
			}
			if stale {
				rest = append(rest, k)
				continue
			}
			if budget <= 0 {
				break
			}
			budget--
			out = append(out, k)
		}
		if stale {
			for _, k := range rest {
				if budget <= 0 {
					break
				}
				budget--
				out = append(out, k)
			}
		}
		return out
	}
	*syncDel = take(syncDel, true)
	*mlatDel = take(mlatDel, true)
	*mlatAdd = take(mlatAdd, false)
	*syncAdd = take(syncAdd, false)

	// Union change for mlat-client (start/stop_sending).
	inUnion := func(k uint32) bool {
		_, a := ss.sync[k]
		_, b := ss.mlat[k]
		return a || b
	}
	sendAdd, sendDel := &w.bufs[4], &w.bufs[5]
	for _, k := range *syncAdd {
		if !inUnion(k) {
			*sendAdd = append(*sendAdd, k)
		}
	}
	for _, k := range *mlatAdd {
		if !inUnion(k) && !slices.Contains(*sendAdd, k) {
			*sendAdd = append(*sendAdd, k)
		}
	}
	for _, k := range *syncDel {
		delete(ss.sync, k)
	}
	for _, k := range *mlatDel {
		delete(ss.mlat, k)
	}
	for _, k := range *syncAdd {
		ss.sync[k] = nowNs
	}
	for _, k := range *mlatAdd {
		ss.mlat[k] = nowNs
	}
	for _, list := range [][]uint32{*syncDel, *mlatDel} {
		for _, k := range list {
			if !inUnion(k) && !slices.Contains(*sendDel, k) {
				*sendDel = append(*sendDel, k)
			}
		}
	}
	d.Reset = reset
	if reset {
		// A reset carries the whole sets.
		*syncAdd, *mlatAdd, *sendAdd = (*syncAdd)[:0], (*mlatAdd)[:0], (*sendAdd)[:0]
		for k := range ss.sync {
			*syncAdd = append(*syncAdd, k)
		}
		for k := range ss.mlat {
			*mlatAdd = append(*mlatAdd, k)
			if _, ok := ss.sync[k]; !ok {
				*sendAdd = append(*sendAdd, k)
			}
		}
		*sendAdd = append(*sendAdd, *syncAdd...)
		*syncDel, *mlatDel, *sendDel = (*syncDel)[:0], (*mlatDel)[:0], (*sendDel)[:0]
		ss.told = true
	}
	d.SyncAdd, d.SyncDel, d.MLATAdd, d.MLATDel, d.SendAdd, d.SendDel = *syncAdd, *syncDel, *mlatAdd, *mlatDel, *sendAdd, *sendDel
	for _, l := range [][]uint32{d.SyncAdd, d.SyncDel, d.MLATAdd, d.MLATDel, d.SendAdd, d.SendDel} {
		sortKeys(l)
	}
	if d.Empty() {
		return false
	}
	if p.out != nil {
		p.out.Interest(st, d)
	}
	return true
}

func sortKeys(k []uint32) { slices.Sort(k) }
