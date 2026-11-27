package ingest

import (
	"context"
	"math"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/ingest/beastin"
	"airview/internal/model"
	"airview/internal/tracker"
)

// Ingest is the aggregator's station-aware ingest (see the package
// documentation). Create it with New, start Run, and hand it every station's
// frames. All methods are safe for concurrent use.
type Ingest struct {
	o        Options
	cfg      gcfg
	h        tracker.NetHandler
	res      model.StationResolver
	mlat     MLATSink
	clockObs ClockObserver
	// learnExclude: Options.LearnExclude.
	learnExclude func(addr uint32, nowNs int64) bool

	shards []shard
	shift  uint

	st     stationTable
	budget *Budget
	sess   sessions

	sweepMu sync.Mutex
	sw      sweeper

	c      counters
	frames stripedCounter // every frame: striped by station (one atomic per copy would be contended)
}

type counters struct {
	updates, late, tooOld, pending, refused, bypass, badLen, lost atomic.Uint64
	_                                                             [64]byte
}

// counterStripes is the number of cache lines a stripedCounter spreads over.
const counterStripes = 32

// stripedCounter is a counter that many reader goroutines add to: each
// stripe is its own cache line, chosen by station, so the per-copy count does
// not bounce one line between cores (at 32 readers a single atomic counter
// cost ~130 ns per copy in the 1,000-station replay; WP-SCALE-tracker).
type stripedCounter struct {
	s [counterStripes]struct {
		n atomic.Uint64
		_ [56]byte
	}
}

func (c *stripedCounter) add(k uint32) { c.s[k%counterStripes].n.Add(1) }

func (c *stripedCounter) load() uint64 {
	var n uint64
	for i := range c.s {
		n += c.s[i].n.Load()
	}
	return n
}

var (
	_ model.FrameSink = (*Ingest)(nil)
	_ beastin.Sink    = (*Ingest)(nil)
	_ beastin.Budget  = (*Budget)(nil)
)

// New returns an Ingest.
func New(o Options) (*Ingest, error) {
	o, err := o.withDefaults()
	if err != nil {
		return nil, err
	}
	ing := &Ingest{o: o, h: o.Handler, res: o.Resolver, mlat: o.MLAT, clockObs: o.ClockObserver, learnExclude: o.LearnExclude}
	ing.cfg = gcfg{
		window: int64(o.Window), flushAfter: int64(o.FlushAfter), retain: int64(o.Retain),
		minHold: int64(o.MinHold), maxMatch: float64(o.MaxMatch), minMatch: float64(o.MinMatch),
		baseMatch: float64(5 * time.Millisecond), uniqueSkew: int64(o.UniqueSkew), clock: o.Clock,
		gateFloor: float64(o.ClockGateFloor), flush: o.Flush, sampleEvery: uint64(o.FlushSampleEvery),
	}
	if kv, ok := o.Clock.(ClockKindView); ok {
		ing.cfg.clockKind = kv
	}
	ing.shards = make([]shard, o.Shards)
	for i := range ing.shards {
		ing.shards[i].idx.init(256)
	}
	ing.shift = uint(64 - bits.TrailingZeros(uint(o.Shards))) // 64 for one shard: h>>64 == 0
	ing.budget = NewBudget(o.Limits.IngestFPS, o.Now)
	ing.sess.init()
	return ing, nil
}

// Budget returns the global ingest budget (pass it to beastin.Options.Budget).
func (ing *Ingest) Budget() *Budget { return ing.budget }

// SetLimits applies changed limits (config apply): the ingest budget,
// per-station rate limits and the selective threshold.
func (ing *Ingest) SetLimits(l model.AggLimits) {
	ing.budget.SetFPS(l.IngestFPS)
	ing.sess.mu.Lock()
	ing.sess.limits = l
	ing.sess.mu.Unlock()
}

func (ing *Ingest) shardOf(h uint64) *shard {
	if len(ing.shards) == 1 {
		return &ing.shards[0]
	}
	return &ing.shards[h>>ing.shift]
}

func (ing *Ingest) now() int64 { return ing.o.Now().UnixNano() }

// ------------------------------------------------------------ entry points

// HandleFrame implements model.FrameSink for in-process sources (the host
// station's feeds, bridges). A frame with Station 0 is the host station's.
func (ing *Ingest) HandleFrame(f model.RawFrame) { ing.frame(&f, false, ing.now()) }

// HandleUpdate implements model.FrameSink: a pre-decoded update. Updates of
// stations held pending reach their own lane only; own MLAT results
// (Src mlat-av, Station 0) and every other update go to the tracker.
func (ing *Ingest) HandleUpdate(u model.DecodedUpdate) {
	ing.c.updates.Add(1)
	if u.Station == 0 && u.Src != model.SrcMLATAV && u.Src != model.SrcMLATExt && !u.MLAT {
		u.Station = model.HostStation
	}
	if u.Station != 0 {
		switch ing.admission(u.Station) {
		case model.AdmApproved:
		case model.AdmPending:
			ing.c.pending.Add(1)
			if ls := ing.h.Lanes(); ls != nil {
				u.Late = true
				ls.BackfillUpdate(u)
			}
			return
		default:
			ing.c.refused.Add(1)
			return
		}
	}
	ing.h.HandleUpdate(u)
}

// Submit hands a batch of frames of one source to the ingest. mapOnly marks
// a MapStatsOnly source (Beast 30004): its frames are never timed copies.
// The frames' Data must stay valid until Submit returns; Station must be
// set. A zero Recv is filled with the frame's host time in the server clock.
func (ing *Ingest) Submit(frames []model.RawFrame, mapOnly bool) {
	now := ing.now()
	for i := range frames {
		ing.frame(&frames[i], mapOnly, now)
	}
}

// HandleBeast implements beastin.Sink.
func (ing *Ingest) HandleBeast(b *beastin.Batch) {
	now := ing.now()
	ev := 0
	for i := range b.Frames {
		for ev < len(b.Events) && b.Events[ev].At <= i {
			ing.beastEvent(&b.Events[ev], b.MapStatsOnly)
			ev++
		}
		ing.frame(&b.Frames[i], b.MapStatsOnly, now)
	}
	for ; ev < len(b.Events); ev++ {
		ing.beastEvent(&b.Events[ev], b.MapStatsOnly)
	}
}

func (ing *Ingest) beastEvent(e *beastin.ClockEvent, mapOnly bool) {
	ing.ClockEvent(e.Station, ClockEvent{OldEpoch: e.Event.OldEpoch, NewEpoch: e.Event.NewEpoch,
		Reason: e.Event.Reason, HostNs: e.Event.HostNs, MapOnly: mapOnly})
}

// ClockEvent reports a new clock epoch of st (AVF CLOCK_EVENT, Beast epoch
// change): the tick mapping restarts and the clock observer is told.
func (ing *Ingest) ClockEvent(st model.StationIdx, ev ClockEvent) {
	if s := ing.st.get(st, false); s != nil {
		s.resetTiming()
	}
	if ing.clockObs != nil {
		ing.clockObs.ClockEvent(st, ev)
	}
}

// ClockPair forwards an AVF CLOCK_PAIR of st to the clock observer.
func (ing *Ingest) ClockPair(st model.StationIdx, p ClockPair) {
	if ing.clockObs != nil {
		ing.clockObs.ClockPair(st, p)
	}
}

// ClockStatus forwards an AVF CLOCK_STATUS of st to the clock observer.
func (ing *Ingest) ClockStatus(st model.StationIdx, s ClockStatus) {
	if ing.clockObs != nil {
		ing.clockObs.ClockStatus(st, s)
	}
}

// SetTimestamps records whether st's owner allows its timestamps to be used
// (AVF RECEIVER "timestamps"). Without them its copies are never timed.
func (ing *Ingest) SetTimestamps(st model.StationIdx, ok bool) {
	ing.st.get(st, true).noTimes.Store(!ok)
}

// Timing returns st's time-correction state (admin diagnostics).
func (ing *Ingest) Timing(st model.StationIdx) (TimingInfo, bool) {
	s := ing.st.get(st, false)
	if s == nil {
		return TimingInfo{}, false
	}
	return s.timing(), true
}

func (ing *Ingest) admission(st model.StationIdx) string {
	if ing.res == nil {
		return model.AdmApproved
	}
	m, ok := ing.res.Info(st)
	if !ok {
		if st == model.HostStation {
			return model.AdmApproved
		}
		return model.AdmDenied
	}
	return m.Admission
}

// frame routes one frame. now is the server time of its arrival.
func (ing *Ingest) frame(f *model.RawFrame, mapOnly bool, now int64) {
	if f.Station == 0 {
		f.Station = model.HostStation
	}
	ing.frames.add(uint32(f.Station))
	s := ing.st.get(f.Station, true)
	s.frames.Add(1)
	s.lastNs.Store(now)
	if f.MLAT || f.Kind == model.FrameStatus {
		ing.c.bypass.Add(1)
		ing.fillRecv(f, s, now)
		ing.h.Handle(*f)
		return
	}
	switch ing.admission(f.Station) {
	case model.AdmApproved:
	case model.AdmPending:
		ing.c.pending.Add(1)
		ing.fillRecv(f, s, now)
		if ls := ing.h.Lanes(); ls != nil {
			ls.Backfill(*f)
		}
		return
	default:
		ing.c.refused.Add(1)
		return
	}
	if f.Late {
		ing.c.late.Add(1)
		ing.fillRecv(f, s, now)
		ing.h.Handle(*f)
		return
	}
	n := len(f.Data)
	if n != 2 && n != 7 && n != 14 {
		ing.c.badLen.Add(1)
		ing.fillRecv(f, s, now)
		ing.h.Handle(*f)
		return
	}
	if f.Kind == model.FrameModeAC && ing.o.ModeAC != nil && !mapOnly && f.Ticks != 0 && f.ClockCls.Timed() && !s.noTimes.Load() {
		ing.o.ModeAC(*f)
	}
	tc, raw, sp := s.corrected(f, now)
	ing.fillRecv(f, s, now)
	if now-tc > int64(ing.o.LateAfter) {
		ing.c.tooOld.Add(1)
		f.Late = true
		ing.h.Handle(*f)
		return
	}
	ch := f.Channel
	if ch < -1 || ch > 127 {
		ch = -1
	}
	c := Copy{Station: f.Station, Channel: int8(ch), ClockCls: f.ClockCls, Epoch: f.Epoch, Ticks: f.Ticks,
		HostNs: f.HostNs, CorrNs: tc, Signal: float32(f.Signal), MapOnly: mapOnly, raw: raw, recv: now}
	c.Timed = !mapOnly && f.ClockCls.Timed() && f.Ticks != 0 && !s.noTimes.Load()
	ing.group(f, &c, sp, now)
}

// fillRecv sets a zero Recv to the frame's host time in the server clock
// (never in the future).
func (ing *Ingest) fillRecv(f *model.RawFrame, s *stState, now int64) {
	if !f.Recv.IsZero() {
		return
	}
	t := now
	if f.HostNs != 0 {
		t = min(s.serverTime(f.HostNs), now)
	}
	f.Recv = time.Unix(0, t)
}

// maxOverflow bounds the credits of copies beyond MaxCopies kept per group
// while its result is pending.
const maxOverflow = 192

type pendCredit struct {
	st   model.StationIdx
	ch   int8
	sig  float32
	same bool // a diversity copy (another channel of a station already credited)
}

// group de-duplicates one copy (DESIGN §6.2).
func (ing *Ingest) group(f *model.RawFrame, c *Copy, sp float64, now int64) {
	h := payloadHash(f.Data)
	sh := ing.shardOf(h)
	unique := uniquePayload(f.Data)
	sh.mu.Lock()
	sh.st.copies++
	i, mk := sh.match(c, f.Data, f.Kind, h, unique, sp, &ing.cfg, now)
	switch mk {
	case matchDup:
		sh.st.dups++
		sh.mu.Unlock()
		return
	case matchSame, matchJoin:
		if mk == matchSame {
			c.SameStation, c.Timed = true, false
			sh.st.sameStation++
		} else {
			sh.st.joined++
		}
		e := &sh.ents[i]
		if e.ready {
			r := e.result
			sh.add(i, c, sp)
			sh.mu.Unlock()
			ing.credit(c.Station, c.Channel, c.Signal, c.SameStation, &r)
			return
		}
		c.pend = true
		if !sh.add(i, c, sp) {
			// Not stored for MLAT, but still credited to its station when
			// the result arrives (SO-10).
			if len(e.overflow) < maxOverflow {
				e.overflow = append(e.overflow, pendCredit{c.Station, c.Channel, c.Signal, c.SameStation})
			} else {
				ing.c.lost.Add(1)
			}
		}
		sh.mu.Unlock()
		return
	}
	i, gen := sh.open(c, f.Data, f.Kind, h, unique, sp, &ing.cfg, now)
	sh.mu.Unlock()

	r := ing.h.Handle(*f)

	var buf [MaxCopies]pendCredit
	var ov []pendCredit // copies beyond MaxCopies (rare)
	n := 0
	sh.mu.Lock()
	if e := &sh.ents[i]; e.live && e.gen == gen {
		e.result, e.ready = r, true
		for k := range e.copies {
			if cp := &e.copies[k]; cp.pend {
				cp.pend = false
				buf[n] = pendCredit{cp.Station, cp.Channel, cp.Signal, cp.SameStation}
				n++
			}
		}
		if len(e.overflow) > 0 {
			ov = append(ov, e.overflow...)
			e.overflow = e.overflow[:0]
		}
	}
	sh.mu.Unlock()
	for k := 0; k < n; k++ {
		ing.credit(buf[k].st, buf[k].ch, buf[k].sig, buf[k].same, &r)
	}
	for k := range ov {
		ing.credit(ov[k].st, ov[k].ch, ov[k].sig, ov[k].same, &r)
	}
}

// credit credits one copy to its station's lane. A same-station copy (the
// transmission heard again on another antenna channel) moves only that
// channel's statistics: the station's own counters already have it (C1).
func (ing *Ingest) credit(st model.StationIdx, ch int8, sig float32, same bool, r *tracker.Result) {
	ls := ing.h.Lanes()
	if ls == nil {
		return
	}
	if same {
		ls.CreditChannel(st, int(ch), float64(sig), r)
		return
	}
	ls.Credit(st, int(ch), float64(sig), r)
}

// ------------------------------------------------------------ sweeping

// Sweep flushes, finalises and frees the groups due at now, delivers them
// to the MLAT sink and updates the stations' latency estimates. Run calls it
// every SweepEvery; tests with a virtual clock call it directly.
func (ing *Ingest) Sweep(now time.Time) { ing.sweep(now.UnixNano()) }

func (ing *Ingest) sweep(now int64) {
	ing.sweepMu.Lock()
	defer ing.sweepMu.Unlock()
	sw := &ing.sw
	sw.groups, sw.late, sw.learn, sw.flush, sw.ngrp = sw.groups[:0], sw.late[:0], sw.learn[:0], sw.flush[:0], 0
	for i := range ing.shards {
		ing.sweepShard(&ing.shards[i], sw, now)
	}
	if p := ing.cfg.flush; p != nil && len(sw.flush) > 0 && now != math.MaxInt64 {
		hn, _ := p.(HearerNoter)
		for _, s := range sw.flush {
			p.ObserveDelay(s.st, s.delay, now)
			if hn != nil && s.key != 0 {
				hn.NoteHearer(s.key, s.st, now)
			}
		}
	}
	if len(sw.groups) > 0 {
		lo, _ := ing.mlat.(LateObserver)
		for i := range sw.groups {
			if sw.late[i] {
				if lo != nil {
					lo.ObserveLate(&sw.groups[i])
				}
			} else {
				ing.mlat.Observe(&sw.groups[i])
			}
		}
	}
	ing.learn(sw, now)
}

// learn feeds the collected unique-payload groups to the stations' latency
// estimators. Residuals are computed with the stations' current latencies
// (not those in force when the copies arrived), so the many groups in
// flight do not apply the same correction over and over.
//
// Groups of aircraft that Options.LearnExclude names (the MLAT hold-out set)
// are skipped: DESIGN §9.6 keeps hold-out aircraft out of every latency
// calibration.
func (ing *Ingest) learn(sw *sweeper, now int64) {
	mm := ing.cfg.maxMatch
	ls := sw.learn
	for len(ls) > 0 {
		n := 1
		for n < len(ls) && ls[n].grp == ls[0].grp {
			n++
		}
		g := ls[:n]
		ls = ls[n:]
		if ex := ing.learnExclude; ex != nil && ex(g[0].addr, now) {
			continue
		}
		sw.tmp = sw.tmp[:0]
		for _, l := range g {
			var lat int64
			if s := ing.st.get(l.st, false); s != nil {
				lat = s.lat.Load()
			}
			sw.tmp = append(sw.tmp, float64(l.raw-lat))
		}
		cur := append(sw.tmp[len(sw.tmp):], sw.tmp...) // keep the per-copy order in sw.tmp
		slices.Sort(cur)
		k := len(cur)
		med := cur[k/2]
		if k%2 == 0 {
			med = (cur[k/2-1] + cur[k/2]) / 2
		}
		for i, l := range g {
			if s := ing.st.get(l.st, false); s != nil {
				s.learn(sw.tmp[i]-med, mm)
			}
		}
	}
}

// Flush closes every open group now (shutdown): groups are delivered to the
// MLAT sink and forgotten.
func (ing *Ingest) Flush() { ing.sweep(math.MaxInt64) }

// Run sweeps the groups every SweepEvery and evaluates the sessions (FLOW,
// traffic modes) once a second until ctx is done; then it flushes.
func (ing *Ingest) Run(ctx context.Context) error {
	t := time.NewTicker(ing.o.SweepEvery)
	defer t.Stop()
	lastTick := ing.o.Now()
	for {
		select {
		case <-ctx.Done():
			ing.Flush()
			return ctx.Err()
		case <-t.C:
			now := ing.o.Now()
			ing.Sweep(now)
			if now.Sub(lastTick) >= time.Second {
				lastTick = now
				ing.Tick(now)
			}
		}
	}
}

// ------------------------------------------------------------ stats

// Stats are the ingest counters (monotonic since start, except the gauges).
type Stats struct {
	Frames   uint64 `json:"frames"`   // frames received
	Updates  uint64 `json:"updates"`  // pre-decoded updates
	Copies   uint64 `json:"copies"`   // copies offered to the grouper
	Groups   uint64 `json:"groups"`   // unique transmissions (first copies handled)
	Joined   uint64 `json:"joined"`   // copies credited to an existing group
	Unique   uint64 `json:"unique"`   // ... joined by the unique-payload rule
	Same     uint64 `json:"same"`     // second channel of a station
	Dups     uint64 `json:"dups"`     // exact duplicates dropped
	Full     uint64 `json:"full"`     // copies beyond MaxCopies (credited, not stored)
	Lost     uint64 `json:"lost"`     // credits lost (full group before the result was ready)
	Late     uint64 `json:"late"`     // backfilled frames (lanes only)
	TooOld   uint64 `json:"tooOld"`   // frames too old to group (lanes only)
	Pending  uint64 `json:"pending"`  // frames/updates of pending stations (own lane only)
	Refused  uint64 `json:"refused"`  // frames of denied/unknown stations (dropped)
	Bypass   uint64 `json:"bypass"`   // MLAT-result and status frames (not grouped)
	BadLen   uint64 `json:"badLen"`   // frames of an unexpected length
	Flushed  uint64 `json:"flushed"`  // groups handed to the MLAT sink
	LateMLAT uint64 `json:"lateMlat"` // late deliveries to the MLAT sink
	// Redelivered counts single-copy flushes delivered again with every copy
	// (Group.Redelivery).
	Redelivered uint64      `json:"redelivered"`
	Live        int         `json:"live"` // groups in memory
	Budget      BudgetStats `json:"budget"`
}

// Stats returns the counters.
func (ing *Ingest) Stats() Stats {
	s := Stats{Frames: ing.frames.load(), Updates: ing.c.updates.Load(), Late: ing.c.late.Load(),
		TooOld: ing.c.tooOld.Load(), Pending: ing.c.pending.Load(), Refused: ing.c.refused.Load(),
		Bypass: ing.c.bypass.Load(), BadLen: ing.c.badLen.Load(), Lost: ing.c.lost.Load(),
		Budget: ing.budget.Stats()}
	for i := range ing.shards {
		sh := &ing.shards[i]
		sh.mu.Lock()
		st := sh.st
		sh.mu.Unlock()
		s.Copies += st.copies
		s.Groups += st.groups
		s.Joined += st.joined
		s.Unique += st.joinUnique
		s.Same += st.sameStation
		s.Dups += st.dups
		s.Full += st.full
		s.Flushed += st.flushed
		s.LateMLAT += st.late
		s.Redelivered += st.redelivered
		s.Live += st.live
	}
	return s
}
