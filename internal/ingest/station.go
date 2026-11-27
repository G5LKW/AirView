package ingest

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// ---------------------------------------------------------------- table

const stChunkBits = 10

type stChunk [1 << stChunkBits]atomic.Pointer[stState]

// stationTable maps StationIdx to per-station state. Lookups are lock-free;
// growth takes a mutex.
type stationTable struct {
	dir atomic.Pointer[[]*stChunk]
	mu  sync.Mutex
}

// get returns the state of st, creating it when create is set.
func (t *stationTable) get(st model.StationIdx, create bool) *stState {
	ci, ii := int(st>>stChunkBits), int(st&(1<<stChunkBits-1))
	if d := t.dir.Load(); d != nil && ci < len(*d) {
		if c := (*d)[ci]; c != nil {
			if s := c[ii].Load(); s != nil || !create {
				return s
			}
		}
	}
	if !create {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var cur []*stChunk
	if d := t.dir.Load(); d != nil {
		cur = *d
	}
	if ci >= len(cur) || cur[ci] == nil {
		// Published directories are never written: build a new one.
		n := make([]*stChunk, max(len(cur), ci+1))
		copy(n, cur)
		if n[ci] == nil {
			n[ci] = new(stChunk)
		}
		cur = n
		t.dir.Store(&n)
	}
	c := cur[ci]
	if s := c[ii].Load(); s != nil {
		return s
	}
	s := newStState(st)
	c[ii].Store(s)
	return s
}

// each calls fn for every station state.
func (t *stationTable) each(fn func(*stState)) {
	d := t.dir.Load()
	if d == nil {
		return
	}
	for _, c := range *d {
		if c == nil {
			continue
		}
		for i := range c {
			if s := c[i].Load(); s != nil {
				fn(s)
			}
		}
	}
}

// ---------------------------------------------------------------- estimator

// Estimator constants.
const (
	envBuckets  = 6
	envBucketNs = int64(2 * time.Second) // tick envelope: 6 x 2 s = 12 s
	envMinN     = 8                      // samples before the tick mapping is used
	envJumpNs   = float64(50 * time.Millisecond)
	envHighNs   = float64(2 * time.Second)
	envHighMax  = 64
	envRebaseNs = 1e12 // re-base the float tick offset every ~17 min

	arrBuckets  = 6
	arrBucketNs = int64(5 * time.Second) // arrival envelope: 30 s

	latWarmN      = 16
	latWarmGain   = 0.3
	latWarmClip   = float64(3 * time.Second)
	latGain       = 0.05
	spreadGain    = 0.05
	latMaxNs      = float64(10 * time.Second)
	spreadMinNs   = float64(500 * time.Microsecond)
	spreadInitMap = float64(10 * time.Millisecond)
	spreadInitRaw = float64(60 * time.Millisecond)
)

// envelope maps the ticks of one clock epoch to the feeder's host time with
// the lower envelope of (host time - tick time): reading and batching only
// ever delay the host stamp, so the smallest difference over the last ~12 s
// is the delay-free mapping. The result has the precision of the receiver
// clock (plus at most ~1 ms of frequency drift for a 100 ppm SDR), instead of
// the tens of milliseconds of host jitter (USB blocks, batching).
type envelope struct {
	valid     bool
	epoch     uint32
	cls       model.ClockClass
	nsPerTick float64
	baseTicks uint64
	baseHost  int64
	min       [envBuckets]float64
	id        [envBuckets]int64
	cur       int
	cachedMin float64
	n         int
	high      int
}

func (e *envelope) reset(epoch uint32, cls model.ClockClass, ticks uint64, host int64) {
	*e = envelope{valid: true, epoch: epoch, cls: cls, nsPerTick: 1e9 / cls.NominalHz(),
		baseTicks: ticks, baseHost: host, cachedMin: math.Inf(1)}
	for i := range e.min {
		e.min[i] = math.Inf(1)
		e.id[i] = math.MinInt64
	}
	e.id[0] = host / envBucketNs
}

// update adds a sample and returns the mapped host time (ok false while the
// mapping is not yet established or was just reset).
func (e *envelope) update(epoch uint32, cls model.ClockClass, ticks uint64, host int64) (int64, bool) {
	if !e.valid || e.epoch != epoch || e.cls != cls || ticks < e.baseTicks {
		e.reset(epoch, cls, ticks, host)
	}
	tn := float64(ticks-e.baseTicks) * e.nsPerTick
	if tn > envRebaseNs {
		shift := int64(tn)
		e.baseTicks = ticks
		e.baseHost += shift
		d := float64(shift) - tn // residual change of the re-based values
		for i := range e.min {
			e.min[i] += d
		}
		e.cachedMin += d
		tn = 0
	}
	r := float64(host-e.baseHost) - tn
	b := host / envBucketNs
	slot := e.cur
	if b < e.id[e.cur] {
		// A host time older than the current bucket (a reordered or
		// back-stepped host clock): credit the bucket it belongs to, if kept.
		slot = -1
		for i := range e.id {
			if e.id[i] == b {
				slot = i
			}
		}
	} else if b != e.id[e.cur] {
		e.cur = (e.cur + 1) % envBuckets
		slot = e.cur
		e.id[e.cur], e.min[e.cur] = b, math.Inf(1)
		e.cachedMin = math.Inf(1)
		for i := range e.min {
			if e.id[i] > b-envBuckets && e.min[i] < e.cachedMin {
				e.cachedMin = e.min[i]
			}
		}
	}
	if e.n >= envMinN && r < e.cachedMin-envJumpNs {
		// The ticks stepped forward against host time (a silent clock step):
		// the old mapping is wrong.
		e.reset(epoch, cls, ticks, host)
		return host, false
	}
	if r > e.cachedMin+envHighNs {
		if e.high++; e.high > envHighMax { // stepped backwards
			e.reset(epoch, cls, ticks, host)
			return host, false
		}
	} else {
		e.high = 0
	}
	if slot >= 0 && r < e.min[slot] {
		e.min[slot] = r
	}
	if r < e.cachedMin {
		e.cachedMin = r
	}
	e.n++
	if e.n < envMinN {
		return host, false
	}
	return e.baseHost + int64(tn+e.cachedMin), true
}

// arrival is the windowed maximum of (feeder host time - server arrival
// time): the feeder's host-clock offset minus the smallest one-way delay. It
// stands in for the PING/PONG offset until one is known (and for links
// without pings).
type arrival struct {
	max [arrBuckets]int64
	id  [arrBuckets]int64
	cur int
	n   int
}

func (a *arrival) update(host, arr int64) {
	d := host - arr
	b := arr / arrBucketNs
	if a.n == 0 {
		for i := range a.max {
			a.max[i], a.id[i] = math.MinInt64, math.MinInt64
		}
		a.id[0] = b
	}
	if b > a.id[a.cur] {
		a.cur = (a.cur + 1) % arrBuckets
		a.id[a.cur], a.max[a.cur] = b, math.MinInt64
	}
	if d > a.max[a.cur] {
		a.max[a.cur] = d
	}
	a.n++
}

func (a *arrival) value() (int64, bool) {
	if a.n == 0 {
		return 0, false
	}
	m, ok := int64(math.MinInt64), false
	newest := a.id[a.cur]
	for i := range a.max {
		if a.id[i] > newest-arrBuckets && a.max[i] > m {
			m, ok = a.max[i], true
		}
	}
	return m, ok
}

// stState is the per-station state of the ingest.
type stState struct {
	idx model.StationIdx

	mu  sync.Mutex // env, arr
	env envelope
	arr arrival

	// pingOff is the feeder host-clock offset (feeder minus server, ns) from
	// AVF PING/PONG; pingRTT the round trip of that sample.
	pingOff atomic.Int64
	pingRTT atomic.Int64
	pingSet atomic.Bool
	arrOff  atomic.Int64 // cached arrival envelope (ns)
	arrSet  atomic.Bool

	// lat is the learned relative latency (ns) and spread the mean absolute
	// residual (ns) of the corrected time against the groups it shares.
	lat     atomic.Int64
	spread  atomic.Int64
	latN    atomic.Int64
	mapped  atomic.Bool // the last frame used the tick mapping
	noTimes atomic.Bool // the owner disabled timestamps (AVF RECEIVER timestamps:false)

	// counters (read by sessions once a second)
	frames atomic.Uint64
	lastNs atomic.Int64 // server time of the last frame
}

func newStState(idx model.StationIdx) *stState {
	s := &stState{idx: idx}
	s.spread.Store(int64(spreadInitRaw))
	return s
}

// corrected returns the frame's corrected time (server domain, Unix ns), the
// same without the learned latency (raw), and the station's current spread
// estimate. now is the server time of arrival.
func (s *stState) corrected(f *model.RawFrame, now int64) (tc, raw int64, spread float64) {
	host := f.HostNs
	if host == 0 {
		host = f.Recv.UnixNano()
		if f.Recv.IsZero() {
			host = now
		}
	}
	base := host
	s.mu.Lock()
	s.arr.update(host, now)
	if s.arr.n&63 == 1 {
		if v, ok := s.arr.value(); ok {
			s.arrOff.Store(v)
			s.arrSet.Store(true)
		}
	}
	mapped := false
	if f.ClockCls.Timed() && f.Ticks != 0 {
		if m, ok := s.env.update(f.Epoch, f.ClockCls, f.Ticks, host); ok {
			base, mapped = m, true
		}
	}
	s.mu.Unlock()
	if s.mapped.Load() != mapped {
		s.mapped.Store(mapped)
	}
	raw = base - s.offset()
	// tc may be slightly ahead of now: the arrival-envelope offset and the
	// learned latency place it in a common, not an absolute, time frame.
	tc = raw - s.lat.Load()
	sp := float64(s.spread.Load())
	if s.latN.Load() < latWarmN {
		sp = max(sp, spreadInitRaw) // not calibrated yet: wide tolerance
	}
	return tc, raw, sp
}

// offset returns the feeder host-clock offset to subtract (ns).
func (s *stState) offset() int64 {
	if s.pingSet.Load() {
		return s.pingOff.Load()
	}
	if s.arrSet.Load() {
		return s.arrOff.Load()
	}
	return 0
}

// serverTime converts a feeder host time to the server clock without the
// learned latency (the data time handed to the tracker).
func (s *stState) serverTime(host int64) int64 { return host - s.offset() }

// learn updates the relative latency and spread from one residual e (ns):
// the copy's corrected time (with the current latency) minus the group's
// median. During warm-up large steps are allowed (a station whose host
// clock is far off converges within a few position squitters); afterwards
// the update is clipped, so single outliers cannot move it far.
func (s *stState) learn(e float64, maxMatch float64) {
	n := s.latN.Add(1)
	lat := float64(s.lat.Load())
	sp := float64(s.spread.Load())
	if n <= latWarmN {
		lat += latWarmGain * clampF(e, -latWarmClip, latWarmClip)
	} else {
		lim := max(4*sp, float64(5*time.Millisecond))
		lat += latGain * clampF(e, -lim, lim)
	}
	lat = clampF(lat, -latMaxNs, latMaxNs)
	ae := math.Min(math.Abs(e), maxMatch)
	sp += spreadGain * (ae - sp)
	if sp < spreadMinNs {
		sp = spreadMinNs
	}
	s.lat.Store(int64(lat))
	s.spread.Store(int64(sp))
}

// resetTiming forgets the tick mapping (new epoch announced).
func (s *stState) resetTiming() {
	s.mu.Lock()
	s.env.valid = false
	s.mu.Unlock()
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// TimingInfo describes a station's time correction (admin diagnostics).
type TimingInfo struct {
	// Mapped is set when the last frame's ticks were mapped to host time.
	Mapped bool `json:"mapped"`
	// OffsetNs is the feeder host-clock offset subtracted (feeder minus
	// server) and OffsetSrc where it came from: "ping", "arrival" or "none".
	OffsetNs  int64  `json:"offsetNs"`
	OffsetSrc string `json:"offsetSrc"`
	// RTTNs is the PING round trip of the offset sample (ping only).
	RTTNs int64 `json:"rttNs,omitempty"`
	// LatencyNs is the learned relative latency, SpreadNs the mean absolute
	// residual against shared groups, Samples the number of groups learned
	// from.
	LatencyNs int64 `json:"latencyNs"`
	SpreadNs  int64 `json:"spreadNs"`
	Samples   int64 `json:"samples"`
	// Frames is the number of frames seen; LastMs the server time of the
	// last one (Unix ms).
	Frames uint64 `json:"frames"`
	LastMs int64  `json:"lastMs"`
}

func (s *stState) timing() TimingInfo {
	ti := TimingInfo{Mapped: s.mapped.Load(), LatencyNs: s.lat.Load(), SpreadNs: s.spread.Load(),
		Samples: s.latN.Load(), Frames: s.frames.Load(), LastMs: s.lastNs.Load() / 1e6, OffsetSrc: "none"}
	switch {
	case s.pingSet.Load():
		ti.OffsetNs, ti.OffsetSrc, ti.RTTNs = s.pingOff.Load(), "ping", s.pingRTT.Load()
	case s.arrSet.Load():
		ti.OffsetNs, ti.OffsetSrc = s.arrOff.Load(), "arrival"
	}
	return ti
}
