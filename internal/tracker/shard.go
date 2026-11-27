package tracker

import (
	"context"
	"hash/fnv"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

// ICAO-sharded tracker (docs/aggregator/DESIGN.md §14 phase 5, §20.2).
//
// In aggregator mode a Tracker can split itself into N shard Trackers when
// SetNetwork is called on a fresh tracker (Options.Shards, else
// aggregator.limits.trackerShards, else AutoShards). The Tracker the engine
// holds becomes the front: every exported method delegates, so callers do
// not change.
//
//   - Routing: a frame goes to shard ShardOf(RouteKey(data), N) (frames
//     without an address - Mode A/C, status, bad length - to shard 0), an
//     update to ShardOf(ICAO, N), look-ups by address the same way. Every
//     aircraft therefore lives in exactly one shard, with its trail, flight,
//     heardBy set and CPR state; identical frame data always reaches the
//     same shard, so the standalone de-duplication stays exact.
//   - Shared: the Lanes (already concurrent; only shard 0 drives its 1 Hz
//     Tick), the all-time records (one table, its own mutex, lock-free reads
//     of the current values), the flight-id source (store.NextFlightID, or
//     one atomic counter without a store), the trajectory manager, RF
//     statistics and latency analysis (all concurrency-safe).
//   - Merged on read: snapshots (each shard builds and caches its own in
//     parallel; the front merges them in ICAO order, sums the counts and
//     runs the enrichment hook once on the result), counters, feed latency
//     (each feed is sampled by one shard), lock statistics, trails.
//   - Merged on write: the combined statistics slot's minute rows
//     (minuteMerger): the shards' rows of one minute are summed into one
//     row - counters and gauges add (the gauges count disjoint aircraft
//     sets), RSSI min/max/mean and max range combine - before they are
//     written, so stats_minute is identical to an unsharded tracker's.
//     Day tables (heat) and per-aircraft rows are additive or per aircraft
//     and are written by the shards directly. Aggregator mode stores no
//     receiver geometry, so the tracker writes no range_hour rows there.
//   - Records the front computes from the totals: most aircraft at once and
//     peak message rate (Run, once a second).
//   - Hooks that promise "never concurrently with itself" (EHS, new
//     airframe, guess samples) are serialised across the shards.
//
// Each shard has its own mutex and data clock, so N goroutines handle N
// shards' frames in parallel and the 1 Hz housekeeping and snapshot builds
// of 10k+ aircraft are split N ways. Standalone trackers are never sharded:
// HandleFrame keeps its single-mutex fast path (one atomic load more).

// MaxShards is the most ICAO shards a tracker splits into.
const MaxShards = 256

// AutoShards is the shard count used when none is configured
// (aggregator.limits.trackerShards = 0): one shard per two CPUs
// (GOMAXPROCS), at least 1 and at most 8. Below 4 CPUs the tracker stays
// unsharded: the frame rate a 2-vCPU host can ingest is far below what one
// tracker mutex handles. Beyond 8 shards the 1,000-station replay gained
// nothing more (WIRING.md "Scaling").
func AutoShards() int {
	p := runtime.GOMAXPROCS(0)
	if p < 4 {
		return 1
	}
	return min(p/2, 8)
}

// shardCount is the number of shards SetNetwork splits a fresh aggregator
// tracker into (1 = none). Caller holds mu.
func (t *Tracker) shardCount() int {
	n := t.opts.Shards
	if n == 0 {
		n = t.cfg.Aggregator.Limits.TrackerShards
	}
	if n == 0 {
		n = AutoShards()
	}
	return min(max(n, 1), MaxShards)
}

// shardSet is the front's view of its shards.
type shardSet struct {
	n     int
	parts []*Tracker
	mm    minuteMerger

	flightSeq atomic.Int64 // flight ids without a store

	// Each shard's live aircraft and message rate at its last tick, and the
	// earliest shard start (data ms): the records of the totals.
	liveN   []atomic.Int64
	rateN   []atomic.Uint64 // math.Float64bits
	startMs atomic.Int64

	// Serialise the hooks that must not run concurrently with themselves.
	hookMu, afMu, guessMu sync.Mutex

	// Merged snapshot cache.
	snapMu   sync.Mutex
	snap     *model.LiveSnapshot
	snapSide []netSide
	snapKey  uint64
	snapAt   time.Time
	gen      atomic.Uint64 // bumped by front-level changes that invalidate it
}

// shardPart identifies a shard within its set.
type shardPart struct {
	set *shardSet
	idx int
}

// ownsFeed reports whether this tracker samples feed id's GPS latency
// (always when it is not a shard).
func (p *shardPart) ownsFeed(id string) bool {
	if p == nil {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return ShardOf(h.Sum32(), p.set.n) == p.idx
}

// shardMaxAircraft is each shard's share of the aircraft limit, with 25 %
// headroom for an uneven hash spread.
func shardMaxAircraft(total, n int) int {
	per := (total + n - 1) / n
	return per + per/4 + 1
}

// startShards splits the (fresh, aggregator-mode) tracker t into n shards.
func (t *Tracker) startShards(n int, res model.StationResolver, lanes *Lanes) {
	s := &shardSet{n: n, parts: make([]*Tracker, n), liveN: make([]atomic.Int64, n), rateN: make([]atomic.Uint64, n)}
	t.mu.Lock()
	s.mm.init(n, t.st)
	cfg := t.cfg
	o := t.opts
	o.Config = &cfg
	o.OnNotice = nil
	o.Latency = t.lat
	o.MaxRangeKm = t.maxRangeKm
	o.MaxAircraft = shardMaxAircraft(t.maxAircraft, n)
	o.Shards = 1
	t.recs.shared = true // nobody else uses the table yet
	for i := range s.parts {
		p := newTracker(o, false)
		p.recs = t.recs
		p.part = &shardPart{set: s, idx: i}
		p.flightSeq = &s.flightSeq
		p.tj, p.coastMs, p.rf = t.tj, t.coastMs, t.rf
		p.started = t.started
		p.decOpts = t.decOpts
		for _, fs := range p.feedList {
			fs.latOwn = p.part.ownsFeed(fs.id)
		}
		p.setNetwork(res, lanes)
		s.parts[i] = p
	}
	log := t.log
	t.mu.Unlock()
	s.setHooks(t)
	t.sh.Store(s)
	log.Info("tracker: ICAO-sharded", "shards", n, "maxAircraftPerShard", o.MaxAircraft)
}

// route returns the shard of frame data d.
func (s *shardSet) route(d []byte) *Tracker {
	if a, ok := RouteKey(d); ok {
		return s.parts[ShardOf(a, s.n)]
	}
	return s.parts[0]
}

// invalidate drops the merged snapshot.
func (s *shardSet) invalidate() { s.gen.Add(1) }

// setHooks hands the front's callbacks to the shards, serialising those that
// must not run concurrently with themselves.
func (s *shardSet) setHooks(t *Tracker) {
	t.mu.Lock()
	hook, notice, af, guess, ra := t.hook, t.onNotice, t.onNewAF, t.guessFn, t.onACAS
	t.mu.Unlock()
	var wh func(EHSEvent) EHSResult
	if hook != nil {
		wh = func(ev EHSEvent) EHSResult {
			s.hookMu.Lock()
			defer s.hookMu.Unlock()
			return hook(ev)
		}
	}
	var waf func(uint32)
	if af != nil {
		waf = func(icao uint32) {
			s.afMu.Lock()
			defer s.afMu.Unlock()
			af(icao)
		}
	}
	var wg func([]GuessSample)
	if guess != nil {
		wg = func(q []GuessSample) {
			s.guessMu.Lock()
			defer s.guessMu.Unlock()
			guess(q)
		}
	}
	for _, p := range s.parts {
		p.hookMu.Lock()
		p.mu.Lock()
		p.hook = wh
		if wh == nil {
			p.hookPending = false
		}
		p.onNotice = notice
		p.onNewAF = waf
		p.onACAS = ra // the recorder is safe for concurrent use
		p.guessFn = wg
		if wg == nil {
			p.guessQ, p.guessDue = p.guessQ[:0], false
		}
		p.mu.Unlock()
		p.hookMu.Unlock()
	}
}

// setNetwork is SetNetwork of a sharded tracker. Switching back to
// standalone mode writes everything and drops the shards (and the live
// aircraft with them); the front continues unsharded.
func (s *shardSet) setNetwork(t *Tracker, res model.StationResolver, lanes *Lanes) {
	if res == nil && lanes == nil {
		_ = s.flush(context.Background(), t)
		t.sh.Store(nil)
		t.setNetwork(nil, nil)
		return
	}
	for _, p := range s.parts {
		p.setNetwork(res, lanes)
	}
	t.setNetwork(res, lanes)
	s.invalidate()
}

// setConfig is SetConfig of a sharded tracker.
func (s *shardSet) setConfig(t *Tracker, cfg model.Config) {
	t.mu.Lock()
	t.applyConfig(cfg)
	t.snap = nil
	t.mu.Unlock()
	for _, p := range s.parts {
		p.SetConfig(cfg)
	}
	s.invalidate()
}

// tick is the front's Run step (once a second): every shard's clock moves
// on while it gets no frames, its slow work runs and its aircraft database
// look-ups are resolved; then the network-wide records are updated.
func (s *shardSet) tick(ctx context.Context, t *Tracker) {
	for _, p := range s.parts {
		p.mu.Lock()
		if p.nowMs != 0 {
			n := p.lastTickMs + time.Since(p.wallAtTick).Milliseconds()
			if n >= p.nextTickMs {
				p.advance(n)
			}
		}
		p.finishSlow()
		p.resolveRefdata(ctx)
	}
}

// noteTick is called by shard idx (p, with its mutex held) at the end of its
// 1 Hz tick: it updates the records only the totals of all shards define
// (most aircraft at once, peak message rate) from every shard's latest
// values.
func (s *shardSet) noteTick(p *Tracker, idx, live int, rate float64, now int64) {
	s.liveN[idx].Store(int64(live))
	s.rateN[idx].Store(math.Float64bits(rate))
	for {
		cur := s.startMs.Load()
		if p.startMs == 0 || (cur != 0 && cur <= p.startMs) || s.startMs.CompareAndSwap(cur, p.startMs) {
			break
		}
	}
	var total int64
	var sum float64
	for i := range s.liveN {
		total += s.liveN[i].Load()
		sum += math.Float64frombits(s.rateN[i].Load())
	}
	sec := now / 1000
	if total > 0 {
		p.record(recMaxAircraft, float64(total), 0, sec, recordMax, nil)
	}
	if start := s.startMs.Load(); start != 0 && now-start > 20_000 {
		p.record(recMaxMsgRate, math.Round(sum*10)/10, 0, sec, recordMax, nil)
	}
}

// flush is Flush of a sharded tracker.
func (s *shardSet) flush(ctx context.Context, t *Tracker) error {
	var now int64
	for _, p := range s.parts {
		p.resolveRefdata(ctx)
		p.mu.Lock()
		p.flush(true)
		now = max(now, p.nowMs)
		p.finishSlow()
	}
	s.mm.flushAll()
	if ls := t.lanesP.Load(); ls != nil {
		ls.FlushAll(now)
	}
	if t.st == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.st.Flush()
}

// reset is Reset of a sharded tracker.
func (s *shardSet) reset(t *Tracker) {
	for _, p := range s.parts {
		p.Reset()
	}
	s.mm.reset()
	s.flightSeq.Store(0)
	s.startMs.Store(0)
	for i := range s.liveN {
		s.liveN[i].Store(0)
		s.rateN[i].Store(0)
	}
	t.mu.Lock()
	t.started = time.Now()
	for _, p := range s.parts {
		p.started = t.started // Reset just set it: same instant everywhere
	}
	t.mu.Unlock()
	s.snapMu.Lock()
	s.snap, s.snapSide = nil, nil
	s.snapMu.Unlock()
	s.invalidate()
}

// ------------------------------------------------------------ snapshots

// snapshotSide returns the merged live picture and its side data. The
// shards build (or reuse) their own snapshots in parallel; the merge is
// cached until a shard's data clock ticks, the front changes something, or
// a second of wall time passed.
func (s *shardSet) snapshotSide(t *Tracker) (model.LiveSnapshot, []netSide) {
	key := s.gen.Load() << 40
	for _, p := range s.parts {
		p.mu.Lock()
		key += p.tickN
		p.mu.Unlock()
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if s.snap != nil && s.snapKey == key && time.Since(s.snapAt) < time.Second {
		return *s.snap, s.snapSide
	}
	snaps := make([]model.LiveSnapshot, s.n)
	sides := make([][]netSide, s.n)
	var wg sync.WaitGroup
	for i, p := range s.parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snaps[i], sides[i] = p.snapshotSide()
		}()
	}
	wg.Wait()
	t.mu.Lock()
	enrich, net := t.enrich, t.net != nil
	t.mu.Unlock()
	snap, side := mergeSnapshots(snaps, sides, net)
	if enrich != nil {
		enrich(snap)
	}
	s.snap, s.snapSide, s.snapKey, s.snapAt = snap, side, key, time.Now()
	return *snap, side
}

// mergeSnapshots merges the shards' snapshots (each sorted by ICAO) into a
// new one in ICAO order. The aircraft are copied (the shards' snapshots are
// shared and read-only); their pointer fields stay shared, as in a
// projection.
func mergeSnapshots(snaps []model.LiveSnapshot, sides [][]netSide, net bool) (*model.LiveSnapshot, []netSide) {
	total := 0
	for i := range snaps {
		total += len(snaps[i].Aircraft)
	}
	out := &model.LiveSnapshot{Aircraft: make([]model.Aircraft, 0, total)}
	var side []netSide
	if net {
		side = make([]netSide, 0, total)
	}
	var msgRate, posRate float64
	for i := range snaps {
		c := &snaps[i].Counts
		out.Now = max(out.Now, snaps[i].Now)
		out.Counts.Aircraft += c.Aircraft
		out.Counts.WithPos += c.WithPos
		out.Counts.ADSB += c.ADSB
		out.Counts.MLAT += c.MLAT
		out.Counts.TISB += c.TISB
		out.Counts.ModeS += c.ModeS
		out.Counts.Predicted += c.Predicted
		out.Counts.MaxRangeKm = max(out.Counts.MaxRangeKm, c.MaxRangeKm)
		msgRate += c.MsgRate
		posRate += c.PosRate
	}
	out.Counts.MsgRate, out.Counts.PosRate = round1(msgRate), round1(posRate)

	// k-way merge with a binary min-heap of shard indices.
	pos := make([]int, len(snaps))
	h := make([]int, 0, len(snaps))
	less := func(a, b int) bool {
		x, y := snaps[a].Aircraft[pos[a]].ICAO, snaps[b].Aircraft[pos[b]].ICAO
		return x < y || x == y && a < b
	}
	down := func(i int) {
		for {
			l, m := 2*i+1, i
			if l < len(h) && less(h[l], h[m]) {
				m = l
			}
			if r := l + 1; r < len(h) && less(h[r], h[m]) {
				m = r
			}
			if m == i {
				return
			}
			h[i], h[m] = h[m], h[i]
			i = m
		}
	}
	for i := range snaps {
		if len(snaps[i].Aircraft) > 0 {
			h = append(h, i)
		}
	}
	for i := len(h)/2 - 1; i >= 0; i-- {
		down(i)
	}
	for len(h) > 0 {
		k := h[0]
		out.Aircraft = append(out.Aircraft, snaps[k].Aircraft[pos[k]])
		if net {
			var sd netSide
			if pos[k] < len(sides[k]) {
				sd = sides[k][pos[k]]
			}
			side = append(side, sd)
		}
		pos[k]++
		if pos[k] == len(snaps[k].Aircraft) {
			h[0] = h[len(h)-1]
			h = h[:len(h)-1]
		}
		down(0)
	}
	return out, side
}

// ------------------------------------------------------------ read side

// counters sums the shards' counters.
func (s *shardSet) counters(t *Tracker) Counters {
	var out Counters
	feeds := map[string]int{}
	var msgRate, posRate float64
	for i, p := range s.parts {
		c := p.Counters()
		if i == 0 {
			out = c
			out.Antennas = append([]AntennaCounters(nil), c.Antennas...)
			out.Feeds = out.Feeds[:0:0]
		} else {
			out.Now = max(out.Now, c.Now)
			out.Frames += c.Frames
			out.Accepted += c.Accepted
			out.Duplicates += c.Duplicates
			out.CRCBad += c.CRCBad
			out.Fixed += c.Fixed
			out.Rejected += c.Rejected
			out.Unsupported += c.Unsupported
			out.BadLength += c.BadLength
			out.ModeAC += c.ModeAC
			out.Status += c.Status
			out.Updates += c.Updates
			out.UpdateMsgs += c.UpdateMsgs
			out.Positions += c.Positions
			out.MLATPositions += c.MLATPositions
			out.PosRejected += c.PosRejected
			out.PosPending += c.PosPending
			out.PosJumps += c.PosJumps
			for k, v := range c.DF {
				out.DF[k] += v
			}
			for k, v := range c.BDS {
				out.BDS[k] += v
			}
			out.Flights += c.Flights
			out.Emergencies += c.Emergencies
			out.Aircraft += c.Aircraft
			out.Tracked += c.Tracked
			for j := range c.Antennas {
				if j < len(out.Antennas) && out.Antennas[j].ID == c.Antennas[j].ID {
					a := &out.Antennas[j]
					a.Frames += c.Antennas[j].Frames
					a.Positions += c.Antennas[j].Positions
					a.MsgRate += c.Antennas[j].MsgRate
				}
			}
		}
		msgRate += c.MsgRate
		posRate += c.PosRate
		for _, f := range c.Feeds {
			j, ok := feeds[f.ID]
			if !ok {
				feeds[f.ID] = len(out.Feeds)
				out.Feeds = append(out.Feeds, f)
				continue
			}
			o := &out.Feeds[j]
			o.Frames += f.Frames
			o.Updates += f.Updates
			o.UpdateMsgs += f.UpdateMsgs
			o.MsgRate += f.MsgRate
			o.LastFrame = max(o.LastFrame, f.LastFrame)
		}
	}
	out.MsgRate, out.PosRate = round1(msgRate), round1(posRate)
	for j := range out.Antennas {
		out.Antennas[j].MsgRate = round1(out.Antennas[j].MsgRate)
	}
	for j := range out.Feeds {
		out.Feeds[j].MsgRate = round1(out.Feeds[j].MsgRate)
	}
	sort.SliceStable(out.Feeds, func(a, b int) bool { return out.Feeds[a].ID < out.Feeds[b].ID })
	t.mu.Lock()
	out.Started = t.started.UnixMilli()
	out.UptimeS = round1(time.Since(t.started).Seconds())
	t.mu.Unlock()
	return out
}

// feedLatency merges the shards' feed latency: every feed is sampled by one
// shard (feedState.latOwn); Total counts the feed's GPS-stamped frames in
// every shard.
func (s *shardSet) feedLatency() []FeedLatency {
	idx := map[string]int{}
	var out []FeedLatency
	for _, p := range s.parts {
		for _, fl := range p.FeedLatency() {
			j, ok := idx[fl.FeedID]
			if !ok {
				idx[fl.FeedID] = len(out)
				out = append(out, fl)
				continue
			}
			total := out[j].Total + fl.Total
			if fl.Samples > out[j].Samples || len(fl.History) > len(out[j].History) {
				out[j] = fl
			}
			out[j].Total = total
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return strings.Compare(out[a].FeedID, out[b].FeedID) < 0 })
	if out == nil {
		out = []FeedLatency{}
	}
	return out
}

// lockStats combines the shards' mutex statistics.
func (s *shardSet) lockStats() LockStats {
	out := LockStats{Shards: s.n}
	var hold float64
	for _, p := range s.parts {
		l := p.LockStats()
		out.Samples += l.Samples
		hold += l.HoldNs * float64(l.Samples)
		out.Utilization += l.Utilization
		out.MaxShardUtilization = max(out.MaxShardUtilization, l.Utilization)
	}
	if out.Samples > 0 {
		out.HoldNs = hold / float64(out.Samples)
	}
	return out
}

// ------------------------------------------------------------ minute rows

// minuteMerger sums the shards' stats_minute rows of the combined slot.
// Each shard closes a minute on its own data clock (within about a second
// of the others) and contributes its rows; a minute is written once every
// shard has contributed or moved past it. A shard that contributes a newer
// minute has nothing more for the older ones. Rows of a minute that stays
// incomplete for 2 minutes of the shards' data time (a shard without frames
// whose clock is not driven by Run) are written as they are.
type minuteMerger struct {
	mu     sync.Mutex
	n      int
	st     *store.Store
	last   []int64 // per shard: newest minute contributed (math.MinInt64 = none)
	rounds []*minuteRound
	// tap, when set (tests), receives every written batch of rows.
	tap func([]store.StatsRow)
}

// minuteRound is one minute being merged.
type minuteRound struct {
	t     int64
	done  []bool
	ndone int
	rows  []store.StatsRow
	rssiN []int64
	secs  float64
	zero  bool // a contribution had unknown covered seconds (Secs 0)
}

// minuteForceSec is how far (data seconds) a minute may lag the newest
// contributed one before it is written incomplete.
const minuteForceSec = 120

func (m *minuteMerger) init(n int, st *store.Store) {
	m.n, m.st = n, st
	m.last = make([]int64, n)
	for i := range m.last {
		m.last[i] = math.MinInt64
	}
}

// add records shard i's rows of minute t (rssiN: the RSSI sample count
// behind each row's mean). Called with the shard's mutex held.
func (m *minuteMerger) add(i int, t int64, rows []store.StatsRow, rssiN []int64) {
	m.mu.Lock()
	var out []store.StatsRow
	if t < m.last[i] {
		// The shard's clock stepped back (rebase): write its rows as they are.
		out = append(out, rows...)
		m.mu.Unlock()
		m.write(out)
		return
	}
	m.last[i] = max(m.last[i], t)
	r := m.round(t)
	if r.done[i] {
		// A second contribution to the same minute (a final flush before
		// the minute ended): write what is there and start again, as an
		// unsharded tracker writes a second row the store merges.
		out = append(out, m.take(r)...)
		r = m.round(t)
	}
	r.done[i] = true
	r.ndone++
	for k := range rows {
		r.merge(&rows[k], rssiN[k])
	}
	// Shard i has nothing more for the minutes before t.
	for _, o := range m.rounds {
		if o.t < t && !o.done[i] {
			o.done[i] = true
			o.ndone++
		}
	}
	newest := int64(math.MinInt64)
	for _, l := range m.last {
		newest = max(newest, l)
	}
	keep := m.rounds[:0]
	for _, o := range m.rounds {
		if o.ndone >= m.n || o.t < newest-minuteForceSec {
			out = append(out, o.rows...)
			continue
		}
		keep = append(keep, o)
	}
	clear(m.rounds[len(keep):])
	m.rounds = keep
	m.mu.Unlock()
	m.write(out)
}

// round returns the open round of minute t, creating it (rounds stay sorted
// by minute). Caller holds mu.
func (m *minuteMerger) round(t int64) *minuteRound {
	j := 0
	for j < len(m.rounds) && m.rounds[j].t < t {
		j++
	}
	if j < len(m.rounds) && m.rounds[j].t == t {
		return m.rounds[j]
	}
	r := &minuteRound{t: t, done: make([]bool, m.n)}
	for k, l := range m.last {
		if l > t { // shard k has moved past this minute
			r.done[k] = true
			r.ndone++
		}
	}
	m.rounds = append(m.rounds, nil)
	copy(m.rounds[j+1:], m.rounds[j:])
	m.rounds[j] = r
	return r
}

// take removes round r and returns its rows. Caller holds mu.
func (m *minuteMerger) take(r *minuteRound) []store.StatsRow {
	for j, o := range m.rounds {
		if o == r {
			m.rounds = append(m.rounds[:j], m.rounds[j+1:]...)
			break
		}
	}
	return r.rows
}

// merge adds one shard's row to the round.
func (r *minuteRound) merge(x *store.StatsRow, rssiN int64) {
	if x.Secs == 0 {
		r.zero = true
	}
	r.secs = max(r.secs, x.Secs)
	for k := range r.rows {
		y := &r.rows[k]
		if y.Ant != x.Ant {
			continue
		}
		y.Frames += x.Frames
		y.DF0 += x.DF0
		y.DF4 += x.DF4
		y.DF5 += x.DF5
		y.DF11 += x.DF11
		y.DF16 += x.DF16
		y.DF17 += x.DF17
		y.DF18 += x.DF18
		y.DF20 += x.DF20
		y.DF21 += x.DF21
		y.DFOther += x.DFOther
		y.CRCBad += x.CRCBad
		y.Positions += x.Positions
		y.MLATPositions += x.MLATPositions
		y.Aircraft += x.Aircraft
		y.AircraftPos += x.AircraftPos
		y.ADSB += x.ADSB
		y.MLAT += x.MLAT
		y.ModeS += x.ModeS
		y.MaxKm = nanMax(y.MaxKm, x.MaxKm)
		y.RSSIMin = nanMin(y.RSSIMin, x.RSSIMin)
		y.RSSIMax = nanMax(y.RSSIMax, x.RSSIMax)
		if n := r.rssiN[k] + rssiN; rssiN > 0 && n > 0 {
			if r.rssiN[k] == 0 || y.RSSIAvg != y.RSSIAvg {
				y.RSSIAvg = x.RSSIAvg
			} else {
				y.RSSIAvg = (y.RSSIAvg*float64(r.rssiN[k]) + x.RSSIAvg*float64(rssiN)) / float64(n)
			}
			r.rssiN[k] = n
		}
		y.Secs = r.covered()
		return
	}
	row := *x
	row.Secs = r.covered()
	r.rows = append(r.rows, row)
	r.rssiN = append(r.rssiN, rssiN)
	for k := range r.rows {
		r.rows[k].Secs = row.Secs
	}
}

// covered is the round's covered seconds: the longest of the shards' (each
// shard counts from its first frame), unknown (0) when any shard's is.
func (r *minuteRound) covered() float64 {
	if r.zero {
		return 0
	}
	return r.secs
}

// flushAll writes every open minute (Flush).
func (m *minuteMerger) flushAll() {
	m.mu.Lock()
	var out []store.StatsRow
	for _, r := range m.rounds {
		out = append(out, r.rows...)
	}
	m.rounds = m.rounds[:0]
	m.mu.Unlock()
	m.write(out)
}

// reset forgets the open minutes (Reset: nothing is written).
func (m *minuteMerger) reset() {
	m.mu.Lock()
	m.rounds = m.rounds[:0]
	for i := range m.last {
		m.last[i] = math.MinInt64
	}
	m.mu.Unlock()
}

// write submits merged rows to the store.
func (m *minuteMerger) write(rows []store.StatsRow) {
	if len(rows) == 0 {
		return
	}
	if m.tap != nil {
		m.tap(rows)
	}
	if m.st != nil {
		b := store.NewBatch()
		b.AddMinuteStats(rows)
		m.st.Submit(b)
	}
}

func nanMax(a, b float64) float64 {
	switch {
	case a != a:
		return b
	case b != b:
		return a
	}
	return math.Max(a, b)
}

func nanMin(a, b float64) float64 {
	switch {
	case a != a:
		return b
	case b != b:
		return a
	}
	return math.Min(a, b)
}
