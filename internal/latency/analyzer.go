package latency

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
)

// Obs is one received message as seen by the tracker. Unknown float fields
// must be NaN (use NewObs or Reset, which set them).
type Obs struct {
	// RecvHost is the host system-clock time the frame was read from the
	// socket (model.RawFrame.Recv).
	RecvHost time.Time
	// Handled is the host system-clock time the tracker finished handling
	// the frame (zero = unknown).
	Handled time.Time
	// GPSNs is the receiver GPS timestamp, ns since UTC midnight, as in
	// model.RawFrame.GPSNs (-1 = none). Radarcape: end of the frame.
	GPSNs  int64
	FeedID string
	ICAO   uint32
	DF, TC uint8
	// TFlag is the ADS-B position "T" (time synchronisation) bit,
	// modes.Message.CPRTime.
	TFlag bool
	// CPROdd is the CPR format bit F (modes.Message.CPROdd).
	CPROdd bool
	// IsPosition marks an ADS-B position message (TC5-18, 20-22).
	IsPosition bool
	// Lat/Lon is the position decoded from THIS message (NaN if it could not
	// be decoded); AltFt its altitude (baro or GNSS); GS (kt) and Track (deg
	// true) the aircraft's latest velocity. NaN = unknown.
	Lat, Lon, AltFt, GS, Track float64
	// MLAT marks synthetic MLAT results; they are counted but excluded from
	// every latency statistic (their timestamps are not times of arrival).
	MLAT bool
	// Kind is the raw frame kind (set by SetFrame). It selects the
	// frame-end correction: Mode A/C 20.3 us (stamped at F2), Mode S short
	// 64 us, long 120 us, status frames 0. Zero = unknown: derived from DF.
	Kind model.FrameKind
}

// NewObs returns an Obs with all optional numeric fields unknown.
func NewObs() Obs {
	var o Obs
	o.Reset()
	return o
}

// Reset clears o to "nothing known" (NaN floats, GPSNs -1).
func (o *Obs) Reset() {
	nan := math.NaN()
	*o = Obs{GPSNs: -1, Lat: nan, Lon: nan, AltFt: nan, GS: nan, Track: nan}
}

// NTPInfo is the quality of the configured NTP reference.
type NTPInfo struct {
	Enabled     bool   `json:"enabled"`
	Synced      bool   `json:"synced"`
	Server      string `json:"server"`
	Stratum     int    `json:"stratum"`
	RefID       string `json:"refId"`
	Leap        int    `json:"leap"`
	OffsetMs    Num    `json:"offsetMs"`
	DelayMs     Num    `json:"delayMs"`
	JitterMs    Num    `json:"jitterMs"`
	RootDelayMs Num    `json:"rootDelayMs"`
	RootDispMs  Num    `json:"rootDispMs"`
	LastOK      int64  `json:"lastOk"` // Unix ms
	LastError   string `json:"lastError,omitempty"`
}

// Options tune an Analyzer. The zero value gives the defaults.
type Options struct {
	Window         time.Duration // summary window (default 15 min, 1..120 min)
	HistoryMinutes int           // per-feed minute history (default 1440 = 24 h, max 10080)
	MaxAircraft    int           // per-aircraft table size (default 4096)
	MaxFeeds       int           // tracked feeds (default 8; more share the "other" slot)
	AircraftTTL    time.Duration // forget aircraft silent this long (default 10 min)
	// NoFrameEndCorrection disables subtracting the frame duration (64 / 120
	// us) from the receiver timestamp. Keep it off for a Radarcape, whose
	// GPS timestamp marks the end of the frame.
	NoFrameEndCorrection bool
	// Receiver delays outside [MinReceiverDelay, MaxReceiverDelay] are gated
	// out of the statistics (defaults -500 ms and 5 s).
	MinReceiverDelay time.Duration
	MaxReceiverDelay time.Duration
	// MinSpeedKt is the minimum ground speed for the kinematic position-age
	// estimator (default 80 kt).
	MinSpeedKt float64
	// PendingRows bounds the minute rows waiting for DrainMinutes (default 2880).
	PendingRows int
}

func (o Options) withDefaults() Options {
	if o.Window <= 0 {
		o.Window = 15 * time.Minute
	}
	if o.Window < time.Minute {
		o.Window = time.Minute
	}
	if o.Window > 120*time.Minute {
		o.Window = 120 * time.Minute
	}
	if o.HistoryMinutes <= 0 {
		o.HistoryMinutes = 1440
	}
	if o.HistoryMinutes > 10080 {
		o.HistoryMinutes = 10080
	}
	if o.MaxAircraft <= 0 {
		o.MaxAircraft = 4096
	}
	if o.MaxFeeds <= 0 {
		o.MaxFeeds = 8
	}
	if o.AircraftTTL <= 0 {
		o.AircraftTTL = 10 * time.Minute
	}
	if o.MinReceiverDelay == 0 {
		o.MinReceiverDelay = -500 * time.Millisecond
	}
	if o.MaxReceiverDelay <= 0 {
		o.MaxReceiverDelay = 5 * time.Second
	}
	if o.MinSpeedKt <= 0 {
		o.MinSpeedKt = 80
	}
	if o.PendingRows <= 0 {
		o.PendingRows = 2880
	}
	return o
}

// Stage indices.
const (
	stProp     = iota // slant range / c
	stRecv            // antenna TOA -> host socket read (true UTC)
	stProc            // socket read -> tracker handled
	stXpdr            // T=1: transmit - time of applicability
	stAgeDelta        // pair method: change of position age between messages
	stAirToSys        // transmit -> handled (prop + receiver + processing)
	stEndToEnd        // T=1: time of applicability -> handled
	nStages
)

var stageKeys = [nStages]string{"propagation", "receiver", "processing", "transponder", "ageDelta", "airToSystem", "endToEnd"}

// Compliance limits used for flags (see doc.go for sources).
const (
	// T=1 register timing: loaded 150..50 ms before the time of
	// applicability and held until the next load, so transmit - ToA is within
	// about +-150 ms for a compliant UTC-coupled transmitter.
	t1ToleranceNs = int64(150 * time.Millisecond)
	// 14 CFR 91.227(e): <= 0.6 s uncompensated latency, <= 2.0 s total.
	uncompensatedLimitS = 0.6
	totalLimitS         = 2.0
	// DO-260B uncompensated latency budget (George et al. 2020): -200..+400 ms.
	ulBudgetMinS = -0.2
	ulBudgetMaxS = 0.4

	filterIntervalNs = int64(10 * time.Second) // receiver-delay minimum filter interval
	filterRing       = 64                      // interval minima kept (~10 min)
	filterUse        = 8                       // floor = min of the last 8 minima (NTP clock-filter style)
	ktToMS           = 1852.0 / 3600.0
	maxSlantM        = 1_000_000.0
)

type slot struct {
	minute int64
	st     [nStages]hist
}

type counters struct {
	frames, gps, noGPS, mlat       int64
	positions, posRange, t1, t0    int64
	negD, absurdD, legacy1s, gpsTS int64
	legacyRange, procBad, propBad  int64
	xpdrOutOfSpec, t1BadTC         int64
	pairs, turns, stale, pairGated int64
}

// Analyzer accumulates latency statistics. All methods are safe for
// concurrent use; Observe does not allocate in steady state.
type Analyzer struct {
	mu  sync.Mutex
	opt Options
	// tap receives every observation before the analysis (SetTap).
	tap atomic.Pointer[func(*Obs)]

	rx      receiverGeo
	clk     model.Clock
	ntpInfo func() NTPInfo
	offNs   int64
	synced  bool
	offAt   int64
	offSet  bool
	host    hostClock // host-vs-GPS offset estimate while the host clock is not NTP-synced

	slots     []slot
	cur       *slot
	curMinute int64
	lastNs    int64 // latest host receive time seen
	started   bool

	c counters

	feeds     map[string]*feedState
	feedList  []*feedState
	otherFeed *feedState

	ac    []acState
	acIdx map[uint32]int32

	pending []MinuteSummary
	scratch [nStages]hist
}

// New creates an Analyzer.
func New(opt Options) *Analyzer {
	opt = opt.withDefaults()
	w := int(opt.Window / time.Minute)
	a := &Analyzer{
		opt:   opt,
		slots: make([]slot, w),
		feeds: make(map[string]*feedState, opt.MaxFeeds+1),
		ac:    make([]acState, 0, opt.MaxAircraft),
		acIdx: make(map[uint32]int32, opt.MaxAircraft),
	}
	for i := range a.slots {
		a.slots[i].minute = math.MinInt64
	}
	a.cur = &a.slots[0]
	return a
}

// SetReceiver sets the antenna position: WGS84 degrees and the antenna
// height in metres AMSL (site elevation + mast). NaN / invalid input clears
// it (propagation and everything that needs it are then not computed).
func (a *Analyzer) SetReceiver(lat, lon, heightAMSL float64) {
	r := newReceiverGeo(lat, lon, heightAMSL)
	a.mu.Lock()
	a.rx = r
	a.mu.Unlock()
}

// SetClock installs the host clock correction (true UTC = host + Offset())
// and an optional getter for the NTP server quality (see SamplerInfo).
// Either may be nil.
func (a *Analyzer) SetClock(clk model.Clock, info func() NTPInfo) {
	a.mu.Lock()
	a.clk = clk
	a.ntpInfo = info
	a.offSet = false
	a.offNs, a.synced = 0, false
	a.mu.Unlock()
}

// Reset drops all statistics (receiver and clock settings are kept).
func (a *Analyzer) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.slots {
		a.slots[i] = slot{minute: math.MinInt64}
	}
	a.cur = &a.slots[0]
	a.started = false
	a.c = counters{}
	a.feeds = make(map[string]*feedState, a.opt.MaxFeeds+1)
	a.feedList = nil
	a.otherFeed = nil
	a.ac = a.ac[:0]
	a.acIdx = make(map[uint32]int32, a.opt.MaxAircraft)
	a.pending = nil
}

// refreshClock updates the cached NTP offset at most once per second of
// data time.
func (a *Analyzer) refreshClock(nowNs int64) {
	if a.clk == nil {
		a.offNs, a.synced = 0, false
		return
	}
	if a.offSet && nowNs-a.offAt < nsPerSec && nowNs >= a.offAt {
		return
	}
	a.offNs = int64(a.clk.Offset())
	a.synced = a.clk.Synced()
	a.offAt = nowNs
	a.offSet = true
}

// SetTap installs fn, which sees every observation handed to Observe
// (before the analysis, on the caller's goroutine, under the caller's
// locks: it must be cheap and must not keep o). nil removes it. Locate Me
// v2 collects its T=1 timing tracks this way.
func (a *Analyzer) SetTap(fn func(*Obs)) {
	if fn == nil {
		a.tap.Store(nil)
		return
	}
	a.tap.Store(&fn)
}

// Observe records one message.
func (a *Analyzer) Observe(o *Obs) {
	if o == nil {
		return
	}
	if fn := a.tap.Load(); fn != nil {
		(*fn)(o)
	}
	if o.RecvHost.IsZero() {
		return
	}
	recvNs := o.RecvHost.UnixNano()
	a.mu.Lock()
	defer a.mu.Unlock()

	a.c.frames++
	if o.MLAT {
		a.c.mlat++
		return
	}
	a.refreshClock(recvNs)
	minute := floorDiv(recvNs, 60*nsPerSec)
	a.advance(minute, recvNs)
	cur := a.cur

	hostUTC := recvNs + a.offNs
	var handledUTC int64
	hasHandled := !o.Handled.IsZero()
	if hasHandled {
		hn := o.Handled.UnixNano()
		proc := hn - recvNs
		if proc < 0 || proc > 10*nsPerSec {
			a.c.procBad++
			hasHandled = false
		} else {
			handledUTC = hn + a.offNs
			cur.st[stProc].add(proc)
		}
	}

	f := a.feed(o.FeedID)

	// C + D: time of arrival and receiver -> host delay.
	var toa int64
	hasTOA, recvGated, toaAbsBad := false, false, false
	if o.GPSNs >= 0 {
		t, legacy, ok := GPSToUnixNs(o.GPSNs, hostUTC)
		if ok {
			if legacy {
				a.c.legacyRange++
			}
			if !a.opt.NoFrameEndCorrection {
				t -= frameAdjustNs(o.Kind, o.DF)
			}
			// The TOA itself comes from GPS and stays usable for the
			// transponder stage even when the host clock is off; only the
			// day choice depends on the host (within +-12 h).
			toa, hasTOA = t, true
			a.c.gps++
			f.roll(a.curMinute)
			d := hostUTC - toa
			// Without a synchronised host clock, a gross host clock error
			// would show as receiver delay (or gate everything out). The
			// receiver's GPS time stamps tell the offset: correct the
			// host-side times by the estimate and keep the stages relative.
			if !a.synced {
				a.host.observe(recvNs, d)
			} else {
				a.host.clear()
			}
			// Not synced and no estimate yet: the host-side delay is
			// meaningless, keep it out rather than blame the receiver.
			pending := !a.synced && !a.host.have
			if a.host.active {
				hostUTC -= a.host.est
				if hasHandled {
					handledUTC -= a.host.est
				}
				d -= a.host.est
				if a.host.ambiguous1s {
					// legacy +1 s receiver stamp or host 1 s behind: an odd
					// second flips the epoch parity, keep TOA out of T=1
					toaAbsBad = true
				}
			}
			if pending {
				recvGated = true
				if x := d + nsPerSec; x > -300_000_000 && x < 300_000_000 {
					toaAbsBad = true // possibly legacy +1 s stamps (see classifyAbsurd)
				}
			} else if d >= int64(a.opt.MinReceiverDelay) && d <= int64(a.opt.MaxReceiverDelay) {
				if d < 0 {
					a.c.negD++
					f.neg++
					f.curNeg++
				}
				cur.st[stRecv].add(d)
				f.observe(hostUTC, d)
			} else {
				recvGated = true
				if a.classifyAbsurd(d) {
					// The GPS timestamp itself is suspect (legacy +1 s, or
					// it disagrees with an NTP-synced host). A whole odd
					// number of seconds flips the 0.2 s epoch parity (1 s =
					// 5 epochs), so the T=1 stage would be off by 200 ms:
					// keep the TOA out of every absolute-time stage. Pair
					// differences stay valid and still use it.
					toaAbsBad = true
				}
				f.absurd++
				f.curAbsurd++
			}
		} else {
			a.c.noGPS++
		}
	} else {
		a.c.noGPS++
	}

	// Per-aircraft (ADS-B only).
	if o.DF != 17 && o.DF != 18 {
		return
	}
	ac := a.aircraft(o.ICAO, recvNs)
	if ac == nil {
		return
	}
	ac.n++
	ac.last = recvNs
	if !o.IsPosition {
		return
	}
	a.c.positions++
	ac.pos++
	lm := &ac.lastMsg
	*lm = lastMsg{
		valid: true, hostRecv: hostUTC, handled: handledUTC, hasHandled: hasHandled,
		toa: toa, hasTOA: hasTOA, tflag: o.TFlag, odd: o.CPROdd, tc: o.TC, df: o.DF,
		feed: f.id, prop: math.NaN(), slantM: math.NaN(), elev: math.NaN(),
		ageDelta: math.NaN(), recvGated: recvGated,
	}
	if o.TFlag {
		a.c.t1++
		ac.t1++
		if !t1TypeCode(o.TC) {
			a.c.t1BadTC++
			ac.badTC = true
		}
	} else {
		a.c.t0++
		ac.t0++
	}
	ac.lastTF = o.TFlag
	ac.lastTC = o.TC

	// B: propagation.
	var prop float64
	hasProp := false
	if a.rx.ok && geo.ValidLatLon(o.Lat, o.Lon) && !math.IsNaN(o.AltFt) && !math.IsInf(o.AltFt, 0) && o.AltFt > -2000 && o.AltFt < 100000 {
		altM := o.AltFt * geo.MetresPerFoot
		dist, elev := a.rx.slant(o.Lat, o.Lon, altM)
		if dist < maxSlantM {
			prop = propagationNs(dist, a.rx.hM, altM)
			hasProp = true
			a.c.posRange++
			cur.st[stProp].add(int64(math.Round(prop)))
			ac.prop.add(prop / 1e3) // us
			lm.prop, lm.hasProp, lm.slantM, lm.elev = prop, true, dist, elev
			if hasTOA && hasHandled && !lm.recvGated {
				cur.st[stAirToSys].add(handledUTC - (toa - int64(math.Round(prop))))
			}
		} else {
			a.c.propBad++
		}
	}

	// A (exact): T=1 time of applicability.
	var lNs int64
	hasL := false
	if o.TFlag && hasTOA && hasProp && !toaAbsBad {
		tx := toa - int64(math.Round(prop))
		e := NearestEpoch(toa, o.CPROdd)
		lNs = tx - e
		hasL = true
		cur.st[stXpdr].add(lNs)
		ac.xpdr.add(float64(lNs) / 1e6) // ms
		if lNs > t1ToleranceNs || lNs < -t1ToleranceNs {
			a.c.xpdrOutOfSpec++
			ac.outSpec++
		}
		lm.epoch, lm.l, lm.hasL = e, lNs, true
		if hasHandled && !lm.recvGated {
			cur.st[stEndToEnd].add(handledUTC - e)
		}
	}

	// A (estimated): kinematic pair method on TOA (GPS) time.
	if hasTOA && geo.ValidLatLon(o.Lat, o.Lon) {
		a.pairEstimate(ac, o, toa, lNs, hasL, cur)
		ac.prev = prevPos{ok: true, toa: toa, lat: o.Lat, lon: o.Lon, l: lNs, hasL: hasL}
	}
}

func t1TypeCode(tc uint8) bool {
	switch tc {
	case 5, 6, 9, 10, 20, 21:
		return true
	}
	return false
}

// classifyAbsurd counts a gated receiver delay, recognising whole-second
// timestamp offsets. It reports whether the GPS timestamp must not be used
// as an absolute UTC time (legacy +1 s, or any gross disagreement with an
// NTP-synced host). GPS-time stamping (+18 s, an even number of seconds =
// 90 epochs) keeps the epoch parity and the T=1 difference transmit - epoch
// exact, so it is not marked.
func (a *Analyzer) classifyAbsurd(d int64) bool {
	near := func(target int64) bool {
		x := d - target
		return x > -300*int64(time.Millisecond) && x < 300*int64(time.Millisecond)
	}
	switch {
	case near(-nsPerSec) && !a.synced:
		// Host 1 s behind or legacy receiver stamps: without NTP nobody can
		// tell (the host clock finding says so); keep T=1 out to be safe.
		a.c.absurdD++
		return true
	case near(-nsPerSec):
		a.c.legacy1s++
		return true
	case d < -15*nsPerSec && d > -20*nsPerSec:
		// GPS time is ahead of UTC by the leap seconds (18 s since 2017):
		// the receiver is time-stamping in GPS time, not UTC.
		a.c.gpsTS++
		return false
	default:
		a.c.absurdD++
		return a.synced
	}
}

// pairEstimate updates the position-age estimator with the along-track
// residual between the previous and this position.
func (a *Analyzer) pairEstimate(ac *acState, o *Obs, toa, lNs int64, hasL bool, cur *slot) {
	p := &ac.prev
	if !p.ok {
		return
	}
	dt := float64(toa-p.toa) / 1e9
	if dt < 0.1 || dt > 3 || math.IsNaN(o.GS) || math.IsNaN(o.Track) || o.GS < a.opt.MinSpeedKt {
		return
	}
	v := o.GS * ktToMS
	// WGS84 meridional (M) and prime-vertical (N) radii at the mean latitude:
	// lat/lon are geodetic, and a spherical radius would scale the east
	// component by up to ~0.3 % (a dt-proportional bias of the age delta).
	const e2 = geo.WGS84F * (2 - geo.WGS84F)
	sl, cl := math.Sincos((o.Lat + p.lat) / 2 * math.Pi / 180)
	w := 1 - e2*sl*sl
	rn := geo.WGS84A / math.Sqrt(w)
	rm := rn * (1 - e2) / w
	// GNSS ground speed is the horizontal velocity AT the aircraft's height:
	// one degree there is (R+h)/R longer than on the ellipsoid (+0.17 % at
	// FL360, otherwise a +0.9 ms bias per 0.5 s pair).
	if h := o.AltFt * geo.MetresPerFoot; h > -1000 && h < 30000 {
		rn += h
		rm += h
	}
	dN := (o.Lat - p.lat) * math.Pi / 180 * rm
	dLon := o.Lon - p.lon
	if dLon > 180 {
		dLon -= 360
	} else if dLon < -180 {
		dLon += 360
	}
	dE := dLon * math.Pi / 180 * rn * cl
	if dN == 0 && dE == 0 {
		a.c.stale++
		ac.stale++
		return
	}
	st, ct := math.Sincos(o.Track * math.Pi / 180)
	along := dN*ct + dE*st
	cross := -dN*st + dE*ct
	if math.Abs(cross) > 0.15*v*dt+30 {
		a.c.turns++
		ac.turns++
		return
	}
	delta := (v*dt - along) / v // seconds: age(this) - age(previous)
	if math.Abs(delta) > 1 {
		a.c.pairGated++
		ac.pairGated++
		return
	}
	a.c.pairs++
	ac.ageD.add(delta)
	cs := cross / v
	ac.noise2 += cs * cs
	cur.st[stAgeDelta].add(int64(math.Round(delta * 1e9)))
	ac.lastMsg.ageDelta = delta
	if hasL && p.hasL {
		ac.pairCheck.add(delta - float64(lNs-p.l)/1e9)
	}
}

// advance moves the minute machinery to minute m.
func (a *Analyzer) advance(m, nowNs int64) {
	if nowNs > a.lastNs {
		a.lastNs = nowNs
	}
	if !a.started {
		a.started = true
		a.curMinute = m
		a.useSlot(m)
		return
	}
	if m <= a.curMinute {
		return // same minute, or the host clock stepped back: keep filling the current one
	}
	a.finishMinute(a.curMinute)
	a.curMinute = m
	a.useSlot(m)
	a.expireAircraft(nowNs)
}

func (a *Analyzer) useSlot(m int64) {
	s := &a.slots[int(((m%int64(len(a.slots)))+int64(len(a.slots)))%int64(len(a.slots)))]
	if s.minute != m {
		for i := range s.st {
			s.st[i].reset()
		}
		s.minute = m
	}
	a.cur = s
}

// finishMinute closes minute m: per-feed history rows and pending rows.
func (a *Analyzer) finishMinute(m int64) {
	for _, f := range a.feedList {
		if f.curMinute == m && (f.cur.n > 0 || f.curAbsurd > 0) {
			row := f.closeMinute(m)
			a.queueRow(row)
		}
	}
	if a.otherFeed != nil && a.otherFeed.curMinute == m && a.otherFeed.cur.n > 0 {
		a.queueRow(a.otherFeed.closeMinute(m))
	}
	if a.cur != nil && a.cur.minute == m {
		s := a.cur
		row := MinuteSummary{T: m * 60, Feed: "", N: s.st[stRecv].n}
		fillMinute(&row, &s.st[stRecv])
		row.PropP50Us = usOf(s.st[stProp].quantile(0.5))
		row.ProcP50Us = usOf(s.st[stProc].quantile(0.5))
		row.T1N = s.st[stXpdr].n
		row.T1P50Us = usOf(s.st[stXpdr].quantile(0.5))
		if row.N > 0 || row.T1N > 0 || s.st[stProp].n > 0 {
			a.queueRow(row)
		}
	}
}

func (a *Analyzer) queueRow(r MinuteSummary) {
	if len(a.pending) >= a.opt.PendingRows {
		copy(a.pending, a.pending[1:])
		a.pending = a.pending[:len(a.pending)-1]
	}
	a.pending = append(a.pending, r)
}

// DrainMinutes returns (and forgets) the finished minute rows queued since
// the last call, for persistence (see Persist). Rows have Feed "" for the
// all-feeds aggregate.
func (a *Analyzer) DrainMinutes() []MinuteSummary {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.pending
	a.pending = nil
	return out
}

func usOf(ns float64) int64 {
	if math.IsNaN(ns) {
		return 0
	}
	return int64(math.Round(ns / 1e3))
}

// feed returns the state of feed id (bounded; overflow shares "other").
func (a *Analyzer) feed(id string) *feedState {
	if f, ok := a.feeds[id]; ok {
		return f
	}
	if len(a.feedList) >= a.opt.MaxFeeds {
		if a.otherFeed == nil {
			a.otherFeed = newFeedState("other", a.opt.HistoryMinutes)
		}
		return a.otherFeed
	}
	f := newFeedState(id, a.opt.HistoryMinutes)
	a.feeds[id] = f
	a.feedList = append(a.feedList, f)
	return f
}

// aircraft returns the table entry for icao, creating (and if necessary
// evicting the least recently seen) one.
func (a *Analyzer) aircraft(icao uint32, nowNs int64) *acState {
	if i, ok := a.acIdx[icao]; ok {
		return &a.ac[i]
	}
	if len(a.ac) >= a.opt.MaxAircraft {
		oldest := 0
		for i := range a.ac {
			if a.ac[i].last < a.ac[oldest].last {
				oldest = i
			}
		}
		a.removeAircraft(oldest)
	}
	a.ac = append(a.ac, acState{icao: icao, first: nowNs, last: nowNs})
	a.acIdx[icao] = int32(len(a.ac) - 1)
	return &a.ac[len(a.ac)-1]
}

func (a *Analyzer) removeAircraft(i int) {
	delete(a.acIdx, a.ac[i].icao)
	last := len(a.ac) - 1
	if i != last {
		a.ac[i] = a.ac[last]
		a.acIdx[a.ac[i].icao] = int32(i)
	}
	a.ac[last] = acState{}
	a.ac = a.ac[:last]
}

func (a *Analyzer) expireAircraft(nowNs int64) {
	ttl := int64(a.opt.AircraftTTL)
	for i := len(a.ac) - 1; i >= 0; i-- {
		if nowNs-a.ac[i].last > ttl {
			a.removeAircraft(i)
		}
	}
}

// --- per-aircraft state -------------------------------------------------------

type prevPos struct {
	ok       bool
	toa      int64
	lat, lon float64
	l        int64
	hasL     bool
}

type lastMsg struct {
	valid      bool
	hostRecv   int64 // true UTC ns
	handled    int64
	hasHandled bool
	toa        int64
	hasTOA     bool
	recvGated  bool
	prop       float64 // ns
	hasProp    bool
	slantM     float64
	elev       float64
	tflag, odd bool
	tc, df     uint8
	epoch, l   int64
	hasL       bool
	ageDelta   float64 // s, NaN when none
	feed       string
}

type acState struct {
	icao        uint32
	first, last int64
	n, pos      int64
	t1, t0      int64
	lastTF      bool
	lastTC      uint8
	badTC       bool
	xpdr        welford // ms
	outSpec     int64
	prop        welford // us
	ageD        welford // s
	noise2      float64 // sum of (cross/v)^2, s^2
	pairCheck   welford // s: pair-method delta minus exact delta (T=1)
	stale       int64
	turns       int64
	pairGated   int64
	prev        prevPos
	lastMsg     lastMsg
}

// ageModel returns the kinematic position-age estimate: sigma of the age
// (s), and the mean age under the uniform-register model (s).
func (s *acState) ageModel() (sigma, mean float64, ok bool) {
	if s.ageD.n < 10 {
		return math.NaN(), math.NaN(), false
	}
	v := s.ageD.variance() - s.noise2/float64(s.ageD.n)
	if v < 0 {
		v = 0
	}
	sigma = math.Sqrt(v / 2)
	return sigma, math.Sqrt(3) * sigma, true
}

// --- per-feed state ------------------------------------------------------------

type ivMin struct {
	t int64 // interval start, true UTC ns
	v int64 // minimum receiver delay, ns
}

type feedState struct {
	id string

	cur       hist
	curMinute int64
	curNeg    int64
	curAbsurd int64
	neg       int64
	absurd    int64
	total     int64
	lastD     int64
	lastT     int64

	ivStart int64
	ivVal   int64
	ivHave  bool
	ring    [filterRing]ivMin
	ringN   int
	ringH   int // next write position

	hist  []FeedMinute
	histH int
	histN int
}

func newFeedState(id string, n int) *feedState {
	return &feedState{id: id, hist: make([]FeedMinute, n), curMinute: math.MinInt64}
}

// roll starts a new minute bucket when minute differs from the current one.
// Rows of an unfinished older minute are dropped here; Analyzer.finishMinute
// closes them in the normal case.
func (f *feedState) roll(minute int64) {
	if f.curMinute != minute {
		f.cur.reset()
		f.curNeg, f.curAbsurd = 0, 0
		f.curMinute = minute
	}
}

func (f *feedState) observe(tUTC, d int64) {
	f.cur.add(d)
	f.total++
	f.lastD, f.lastT = d, tUTC
	iv := floorDiv(tUTC, filterIntervalNs) * filterIntervalNs
	if f.ivHave && iv != f.ivStart {
		f.pushInterval()
	}
	if !f.ivHave || iv != f.ivStart {
		f.ivStart, f.ivVal, f.ivHave = iv, d, true
	} else if d < f.ivVal {
		f.ivVal = d
	}
}

func (f *feedState) pushInterval() {
	f.ring[f.ringH] = ivMin{t: f.ivStart, v: f.ivVal}
	f.ringH = (f.ringH + 1) % filterRing
	if f.ringN < filterRing {
		f.ringN++
	}
	f.ivHave = false
}

// minima returns up to n most recent interval minima, oldest first, plus the
// running (incomplete) interval.
func (f *feedState) minima(dst []ivMin, n int) []ivMin {
	if n > f.ringN {
		n = f.ringN
	}
	for i := n; i >= 1; i-- {
		dst = append(dst, f.ring[(f.ringH-i+filterRing)%filterRing])
	}
	if f.ivHave {
		dst = append(dst, ivMin{t: f.ivStart, v: f.ivVal})
	}
	return dst
}

// filterStats computes the clock-filter floor, jitter and drift.
func (f *feedState) filterStats() (floor, jitter, driftUsPerMin float64, n int) {
	var buf [filterRing + 1]ivMin
	m := f.minima(buf[:0], filterRing)
	n = len(m)
	if n == 0 {
		return math.NaN(), math.NaN(), math.NaN(), 0
	}
	floor = math.Inf(1)
	start := n - filterUse
	if start < 0 {
		start = 0
	}
	for _, x := range m[start:] {
		floor = math.Min(floor, float64(x.v))
	}
	jitter = math.NaN()
	js := n - 16
	if js < 0 {
		js = 0
	}
	if n-js >= 3 {
		var s float64
		for i := js + 1; i < n; i++ {
			d := float64(m[i].v - m[i-1].v)
			s += d * d
		}
		jitter = math.Sqrt(s / float64(n-js-1))
	}
	driftUsPerMin = math.NaN()
	if n >= 6 && m[n-1].t-m[0].t >= 2*60*nsPerSec {
		var sx, sy, sxx, sxy float64
		for _, x := range m {
			tx := float64(x.t-m[0].t) / 60e9 // minutes
			ty := float64(x.v) / 1e3         // us
			sx += tx
			sy += ty
			sxx += tx * tx
			sxy += tx * ty
		}
		k := float64(n)
		den := k*sxx - sx*sx
		if den > 0 {
			driftUsPerMin = (k*sxy - sx*sy) / den
		}
	}
	return floor, jitter, driftUsPerMin, n
}

func (f *feedState) closeMinute(m int64) MinuteSummary {
	row := MinuteSummary{T: m * 60, Feed: f.id, N: f.cur.n, Neg: f.curNeg, Absurd: f.curAbsurd}
	fillMinute(&row, &f.cur)
	floor, jit, _, _ := f.filterStats()
	fm := FeedMinute{
		T: m * 60 * 1000, N: f.cur.n, Neg: f.curNeg, Absurd: f.curAbsurd,
		MinMs: msNum(float64(f.cur.min)), P50Ms: msNum(f.cur.quantile(0.5)),
		P95Ms: msNum(f.cur.quantile(0.95)), MaxMs: msNum(float64(f.cur.max)),
		MeanMs: msNum(f.cur.mean()), FloorMs: msNum(floor), JitterMs: msNum(jit),
	}
	if f.cur.n == 0 {
		fm.MinMs, fm.MaxMs = Num(math.NaN()), Num(math.NaN())
	}
	row.JitterUs = usOf(jit)
	f.hist[f.histH] = fm
	f.histH = (f.histH + 1) % len(f.hist)
	if f.histN < len(f.hist) {
		f.histN++
	}
	f.cur.reset()
	f.curNeg, f.curAbsurd = 0, 0
	return row
}

func fillMinute(r *MinuteSummary, h *hist) {
	if h.n == 0 {
		return
	}
	r.MinUs = usOf(float64(h.min))
	r.P50Us = usOf(h.quantile(0.5))
	r.P95Us = usOf(h.quantile(0.95))
	r.MaxUs = usOf(float64(h.max))
	r.MeanUs = usOf(h.mean())
}
