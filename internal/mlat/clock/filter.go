package clock

import (
	"context"
	"math"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/ntp"
)

// Filter is the joint clock filter of the MLAT engine (DESIGN §9.2). Feed
// it with AddSync (or ObserveGroup), SetStation, StationEvent and GPS;
// call Step once per epoch (or Run); read Snapshot from any goroutine.
// AddSync, SetStation, StationEvent, GPS and Snapshot are safe for
// concurrent use; Step calls are serialised.
type Filter struct {
	cfg  Config
	prop func(rx, ac geo.Vec3) float64

	// inbox (mu)
	mu     sync.Mutex
	inObs  []obsRec
	inMem  []Member
	inInfo []infoReq
	inEv   []evReq
	inGPS  []gpsReq
	quota  map[model.StationIdx]quotaCnt
	black  map[uint32]float64        // aircraft key -> blacklisted until (s)
	pref   map[model.StationIdx]bool // anchors and synced stations (set by Step)
	quotaN float64
	// shiftS: the internal time origin is OriginUnixNs + shiftS seconds
	// (reorigin); written with mu and stepMu held.
	shiftS  int64
	nIn     uint64
	nQuota  uint64
	nBlack  uint64
	nBadObs uint64

	// owned by Step (stepMu)
	stepMu  sync.Mutex
	st      []*station
	comps   []*component
	domN    map[uint32]int // partitions per domain (scratch of domainParts)
	nextID  uint32
	k       int64
	tE      float64
	started bool
	carry   []obsRec
	carryM  []Member
	edges   map[uint64]float64
	work    []workObs
	wm      []workMember
	acft    map[uint32]*acftStat
	seq     uint64
	stats   Stats
	epochEW float64 // EWMA of the Step wall time, ns
	posMap  map[model.StationIdx]geo.Vec3
	posDirt bool

	acftTrack bool
	snap      atomic.Pointer[Snapshot]
}

type obsRec struct {
	key    uint32
	short  bool
	hostS  float64
	pos    geo.Vec3
	cov    [6]float64
	m0, m1 int32
}

type infoReq struct {
	st     model.StationIdx
	info   StationInfo
	remove bool
}

type evReq struct {
	st model.StationIdx
	ev Event
}

type gpsReq struct {
	st model.StationIdx
	s  GPSStatus
}

// quotaCnt is a per-station token bucket: SyncRatePerStation tokens per
// second, at most two epochs' worth stored.
type quotaCnt struct {
	tokens float64
	lastS  float64
}

type acftStat struct {
	n, fails int
	lastS    float64
}

// Stats are filter-wide counters (Snapshot-independent).
type Stats struct {
	Epochs         int64
	Admitted       uint64 // sync groups admitted by AddSync
	QuotaDropped   uint64 // refused by the per-station rate target
	Blacklisted    uint64 // refused: aircraft blacklisted
	BadObs         uint64 // malformed sync groups
	Processed      uint64 // groups processed by components
	Accepted       uint64 // accepted (passed the integrity tests)
	Rejected       uint64
	Steps          uint64 // detected clock steps
	FreqResets     uint64 // frequency resets after a run of same-sign steps
	FreqSteps      uint64 // steps whose residual run showed a frequency error (φ corrected too)
	UpdateFailures uint64 // numerically failed epoch updates (state kept)
	// ExternalStates is the number of cross-partition external states
	// marginalised in the last epoch (all partitions; external.go), and
	// ExternalFailures counts partition epochs whose external block was not
	// positive definite (its information was dropped for that epoch).
	ExternalStates   int
	ExternalFailures uint64
	Components       int
	Stations         int
	// LastEpochNs is the wall-clock cost of the last Step, measured with the
	// precise system clock (ntp.Now, < 1 µs on Windows; epochCost).
	LastEpochNs int64
	// MeanEpochNs is an EWMA (about 32 epochs) of the Step wall time.
	MeanEpochNs   int64
	BlacklistedAc int // aircraft currently blacklisted
	// BlacklistCapped counts aircraft that met the blacklist rule but were
	// not blacklisted because BlacklistMaxFrac of the sync aircraft already
	// were (a station fault, not an aircraft fault; finding ROB-2).
	BlacklistCapped uint64
	// PanicResets counts Steps that panicked and were recovered by resetting
	// every clock state (Config.OnPanic; finding SCALE-01).
	PanicResets uint64
}

// New creates a filter.
func New(cfg Config) (*Filter, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	f := &Filter{
		cfg:       cfg,
		quota:     map[model.StationIdx]quotaCnt{},
		black:     map[uint32]float64{},
		pref:      map[model.StationIdx]bool{},
		edges:     map[uint64]float64{},
		acft:      map[uint32]*acftStat{},
		posMap:    map[model.StationIdx]geo.Vec3{},
		acftTrack: true,
	}
	f.quotaN = cfg.SyncRatePerStation * cfg.EpochS
	if cfg.Propagation != nil {
		f.prop = cfg.Propagation
	} else {
		atm := cfg.Atmosphere
		f.prop = func(rx, ac geo.Vec3) float64 { return atm.PropagationNs(rx, ac) }
	}
	f.snap.Store(&Snapshot{OriginUnixNs: cfg.OriginUnixNs, EpochS: cfg.EpochS, frameRef: cfg.FrameRefNs, prop: f.prop, pos: map[model.StationIdx]geo.Vec3{}})
	return f, nil
}

// Config returns the normalised configuration.
func (f *Filter) Config() Config { return f.cfg }

// Snapshot returns the latest published snapshot (never nil).
func (f *Filter) Snapshot() *Snapshot { return f.snap.Load() }

// Stats returns the filter counters as of the last Step.
func (f *Filter) Stats() Stats {
	f.stepMu.Lock()
	s := f.stats
	f.stepMu.Unlock()
	f.mu.Lock()
	s.Admitted, s.QuotaDropped, s.Blacklisted, s.BadObs = f.nIn, f.nQuota, f.nBlack, f.nBadObs
	f.mu.Unlock()
	return s
}

// Seconds converts a Unix-ns time to network seconds since the origin.
func (f *Filter) Seconds(unixNs int64) float64 {
	return float64(unixNs-f.cfg.OriginUnixNs) / 1e9
}

// intS converts a Unix-ns time to the filter-internal seconds (since
// OriginUnixNs + shiftS; doc.go "Internal time origin"). Caller holds f.mu
// or stepMu (shiftS changes only while both are held).
func (f *Filter) intS(unixNs int64) float64 {
	return float64(unixNs-f.cfg.OriginUnixNs-f.shiftS*1e9) / 1e9
}

// SetStation registers or updates a station.
func (f *Filter) SetStation(st model.StationIdx, info StationInfo) {
	f.mu.Lock()
	f.inInfo = append(f.inInfo, infoReq{st: st, info: info})
	f.mu.Unlock()
}

// RemoveStation forgets a station.
func (f *Filter) RemoveStation(st model.StationIdx) {
	f.mu.Lock()
	f.inInfo = append(f.inInfo, infoReq{st: st, remove: true})
	f.mu.Unlock()
}

// StationEvent reports a new clock epoch of a station.
func (f *Filter) StationEvent(st model.StationIdx, ev Event) {
	f.mu.Lock()
	f.inEv = append(f.inEv, evReq{st: st, ev: ev})
	f.mu.Unlock()
}

// GPS reports the GPS status of a station (Radarcape status frame).
func (f *Filter) GPS(st model.StationIdx, s GPSStatus) {
	f.mu.Lock()
	f.inGPS = append(f.inGPS, gpsReq{st: st, s: s})
	f.mu.Unlock()
}

// AddSync queues one sync observation for the next epoch. It returns false
// when the observation was not admitted (malformed, blacklisted aircraft,
// or every member already has its share of sync observations for the
// epoch). The members are copied.
func (f *Filter) AddSync(o *SyncObs) bool {
	if (o.Kind != model.FrameModeSShort && o.Kind != model.FrameModeSLong) || len(o.Members) < 2 {
		f.mu.Lock()
		f.nBadObs++
		f.mu.Unlock()
		return false
	}
	rate := f.cfg.SyncRatePerStation
	burst := 2 * f.quotaN
	f.mu.Lock()
	defer f.mu.Unlock()
	hostS := f.intS(o.HostNs)
	if until, ok := f.black[o.Key]; ok {
		if until > hostS {
			f.nBlack++
			return false
		}
		delete(f.black, o.Key)
	}
	// refresh the token buckets; a member is "needy" with a token left
	needy := 0
	for _, m := range o.Members {
		q, ok := f.quota[m.Station]
		if !ok {
			q = quotaCnt{tokens: burst, lastS: hostS}
		}
		if dt := hostS - q.lastS; dt > 0 {
			q.tokens = math.Min(burst, q.tokens+rate*dt)
			q.lastS = hostS
		}
		f.quota[m.Station] = q
		if q.tokens >= 1 {
			needy++
		}
	}
	if needy == 0 {
		f.nQuota++
		return false
	}
	// Large groups: keep the needy members and the preferred (anchors,
	// synced) ones up to MaxMembers, so a big group does not use up the
	// share of stations it cannot serve.
	m0 := int32(len(f.inMem))
	if len(o.Members) <= f.cfg.MaxMembers {
		f.inMem = append(f.inMem, o.Members...)
	} else {
		pref := 0
		for pass := 0; pass < 3 && len(f.inMem)-int(m0) < f.cfg.MaxMembers; pass++ {
			for _, m := range o.Members {
				if len(f.inMem)-int(m0) >= f.cfg.MaxMembers {
					break
				}
				q := f.quota[m.Station]
				p := f.pref[m.Station]
				var take bool
				switch pass {
				case 0: // a few precise references first
					take = p && pref < 4
					if take {
						pref++
					}
				case 1:
					take = q.tokens >= 1 && !(p && pref <= 4 && containsMember(f.inMem[m0:], m.Station))
				case 2:
					take = !containsMember(f.inMem[m0:], m.Station)
				}
				if take && !containsMember(f.inMem[m0:], m.Station) {
					f.inMem = append(f.inMem, m)
				}
			}
		}
	}
	for _, m := range f.inMem[m0:] {
		q := f.quota[m.Station]
		q.tokens = math.Max(q.tokens-1, -1)
		f.quota[m.Station] = q
	}
	f.inObs = append(f.inObs, obsRec{key: o.Key, short: o.Kind == model.FrameModeSShort, hostS: hostS,
		pos: o.Pos, cov: o.PosCov, m0: m0, m1: int32(len(f.inMem))})
	f.nIn++
	return true
}

// Run steps the filter every epoch until ctx ends. now returns the server
// time (Unix ns); lag is how far behind real time the epochs run (late
// copies of a group must have arrived; default 1 s when ≤ 0).
func (f *Filter) Run(ctx context.Context, now func() int64, lag time.Duration) {
	if now == nil {
		now = func() int64 { return time.Now().UnixNano() }
	}
	if lag <= 0 {
		lag = time.Second
	}
	t := time.NewTicker(time.Duration(f.cfg.EpochS * float64(time.Second)))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.Step(f.Seconds(now() - int64(lag)))
		}
	}
}

func (f *Filter) station(idx model.StationIdx, create bool) *station {
	if int(idx) < len(f.st) {
		if s := f.st[idx]; s != nil || !create {
			return s
		}
	} else if !create {
		return nil
	}
	for int(idx) >= len(f.st) {
		f.st = append(f.st, nil)
	}
	s := newStation(idx)
	f.st[idx] = s
	return s
}

func (f *Filter) logf(format string, args ...any) {
	if f.cfg.Logf != nil {
		f.cfg.Logf(format, args...)
	}
}

// Step runs one filter epoch with states at network time tE (seconds since
// the origin): it applies the queued station changes, admits the sync
// observations with host time ≤ tE, restructures the components, predicts,
// gates and updates every component (in parallel), runs the integrity,
// anchor and adaptation logic and publishes a new Snapshot. Calls with a
// non-increasing tE are ignored.
func (f *Filter) Step(tExt float64) {
	f.stepMu.Lock()
	defer f.stepMu.Unlock()
	tE := tExt - float64(f.shiftS)
	if f.started && !(tE > f.tE) {
		return
	}
	muHeld := false
	defer func() {
		if r := recover(); r != nil {
			if muHeld {
				f.mu.Unlock()
			}
			f.recoverStep(r, debug.Stack(), tE)
		}
	}()
	t0, p0 := time.Now(), ntp.Now()
	f.mu.Lock()
	muHeld = true
	var shift float64
	if f.started && tE > f.cfg.ReoriginS {
		// Keep the internal times small (doc.go "Internal time origin"):
		// move the origin by whole seconds so that tE lands in [1, 2).
		shift = math.Floor(tE) - 1
		f.shiftS += int64(shift)
		tE -= shift
		f.reoriginInbox(shift)
	}
	obs, mem := f.inObs, f.inMem
	infos, evs, gps := f.inInfo, f.inEv, f.inGPS
	f.inObs, f.inMem, f.inInfo, f.inEv, f.inGPS = nil, nil, nil, nil, nil
	f.mu.Unlock()
	muHeld = false
	if shift != 0 {
		f.reorigin(shift)
	}
	if !f.started {
		f.started = true
		f.tE = tE - f.cfg.EpochS
	}
	dt := tE - f.tE
	f.k++

	f.applyInfos(infos)
	f.applyEvents(evs, tE)
	f.applyGPS(gps)
	f.validate(obs, mem, tE)
	f.restructure(tE)
	for _, c := range f.comps {
		c.predict(tE)
		if c.gauge == gaugeAnchor {
			c.gTheta *= math.Exp(-dt / f.cfg.GaugeRelaxS)
		}
	}
	f.partitionDrift(dt)
	f.setGauges()
	f.assign()
	f.runComponents()
	f.postEpoch(tE)
	f.tE = tE
	f.stats.Epochs = f.k
	f.publish(tE)
	d := epochCost(time.Since(t0), ntp.Now().Sub(p0))
	f.stats.LastEpochNs = d
	if f.k <= 1 {
		f.epochEW = float64(d)
	} else {
		f.epochEW += (float64(d) - f.epochEW) / 32
	}
	f.stats.MeanEpochNs = int64(f.epochEW)
}

// shiftT subtracts d from an internal time; 0 means "never" and stays 0 (a
// time that would become exactly 0 becomes −1 ps instead).
func shiftT(p *float64, d float64) {
	if *p == 0 {
		return
	}
	if *p -= d; *p == 0 {
		*p = -1e-12
	}
}

// reoriginInbox moves the times of the inbox state by −d s (caller holds
// mu).
func (f *Filter) reoriginInbox(d float64) {
	for i := range f.inObs {
		f.inObs[i].hostS -= d
	}
	for k, q := range f.quota {
		shiftT(&q.lastS, d)
		f.quota[k] = q
	}
	for k, v := range f.black {
		f.black[k] = v - d
	}
}

// reorigin moves every internal time of the Step-owned state by −d s
// (whole seconds; caller holds stepMu).
func (f *Filter) reorigin(d float64) {
	shiftT(&f.tE, d)
	for i := range f.carry {
		f.carry[i].hostS -= d
	}
	for k, v := range f.edges {
		f.edges[k] = v - d
	}
	for _, a := range f.acft {
		shiftT(&a.lastS, d)
	}
	for _, c := range f.comps {
		c.tE -= d
	}
	for _, s := range f.st {
		if s == nil {
			continue
		}
		s.baseS -= d
		for _, p := range []*float64{&s.lastObsS, &s.lastAccS, &s.gpsOKSince, &s.freeSince, &s.anchorSince,
			&s.adStart, &s.driftBin0, &s.stepRunT0} {
			shiftT(p, d)
		}
		for i := range s.streakT {
			s.streakT[i] -= d
		}
		for i := range s.drift {
			s.drift[i].t -= d
		}
		for i := range s.events {
			s.events[i].T -= d
		}
	}
}

// epochCost returns the wall time of one Step (ns). Go's monotonic clock
// ticks in about 0.5 ms steps on Windows, so a fast epoch read 0 there
// (HOOKS WP-W2-web). The precise system clock (ntp.Now:
// GetSystemTimePreciseAsFileTime on Windows, < 1 µs) is used instead; it
// carries no monotonic reading, so a wall-clock adjustment during the epoch
// can make it wrong: then (negative or more than a second off the monotonic
// reading) the monotonic value is used.
func epochCost(mono, precise time.Duration) int64 {
	if precise < 0 || (precise-mono).Abs() > time.Second {
		return mono.Nanoseconds()
	}
	return precise.Nanoseconds()
}

func (f *Filter) applyInfos(infos []infoReq) {
	for _, r := range infos {
		if r.remove {
			if s := f.station(r.st, false); s != nil {
				s.known = false
				s.info.Located = false
				delete(f.posMap, r.st)
				f.posDirt = true
			}
			continue
		}
		s := f.station(r.st, true)
		was := s.known
		oldPos := s.info.Pos
		if s.setInfo(r.info) && was {
			// class change: a new clock model and a new base
			s.fresh = true
			s.hasEpoch = false
			s.addEvent(f.tE, string(model.ClockEvModeChange), "class "+s.cls.String())
		}
		if r.info.Located && (oldPos != r.info.Pos || !was) {
			f.posMap[r.st] = r.info.Pos
			f.posDirt = true
		}
		if r.info.Flagged && s.mode == modeAnchor {
			s.demoteReq = "flagged"
		}
	}
}

func (f *Filter) applyEvents(evs []evReq, tE float64) {
	for _, r := range evs {
		s := f.station(r.st, false)
		if s == nil {
			continue
		}
		s.evEpoch, s.evReason, s.evValid = r.ev.NewEpoch, r.ev.Reason, true
		if s.hasEpoch && r.ev.NewEpoch != s.epoch && s.mode == modeAnchor {
			s.demoteReq = "epoch_" + string(r.ev.Reason)
		}
	}
}

func (f *Filter) applyGPS(gps []gpsReq) {
	for _, r := range gps {
		s := f.station(r.st, false)
		if s == nil {
			continue
		}
		hs := f.intS(r.s.HostNs)
		// A receiver on GPS time (declared class 2) keeps reporting UTC=false
		// after the filter applied its leap offset and made its class UTC;
		// the declared class, not the mutable one, decides (finding MLAT-R1:
		// such a station was demoted a second after promotion, for good).
		ok := r.s.Locked && (r.s.UTC || s.info.Class == model.ClockGPSGPST || s.cls == model.ClockGPSGPST)
		if ok && !(s.gpsKnown && s.gpsOK) {
			s.gpsOKSince = hs
		}
		if !ok && s.mode == modeAnchor {
			s.demoteReq = "gps_status"
		}
		s.gpsKnown, s.gpsOK, s.gpsUTC = true, ok, r.s.UTC
	}
}

// validate turns the admitted observations of the epoch into work items:
// known, located stations of a timed class with a matching clock epoch.
// It also maintains the co-observation edges and starts new clock epochs.
func (f *Filter) validate(obs []obsRec, mem []Member, tE float64) {
	f.work, f.wm = f.work[:0], f.wm[:0]
	// carried observations first (older)
	all := f.carry
	allM := f.carryM
	base := int32(len(allM))
	for _, o := range obs {
		o.m0 += base
		o.m1 += base
		all = append(all, o)
	}
	allM = append(allM, mem...)
	var keep []obsRec
	var keepM []Member
	minS := tE - f.cfg.MaxObsAgeS
	hostVar := f.hostVar()
	// pass 1: clock epochs, in arrival order. A station whose epoch changes
	// is re-based on the first member of its new epoch; members of its old
	// epoch in this batch are dropped (they belong to another base).
	use := all[:0:0]
	for _, o := range all {
		if o.hostS > tE {
			m0 := int32(len(keepM))
			keepM = append(keepM, allM[o.m0:o.m1]...)
			o.m0, o.m1 = m0, int32(len(keepM))
			keep = append(keep, o)
			continue
		}
		if o.hostS < minS {
			continue
		}
		use = append(use, o)
		for _, m := range allM[o.m0:o.m1] {
			s := f.station(m.Station, false)
			if s == nil || !s.usable() {
				continue
			}
			if !s.hasEpoch || (m.Epoch != s.epoch && m.Epoch != s.prevEpoch) {
				f.newEpoch(s, m.Epoch, m.Ticks, o.hostS, o.short, hostVar, tE)
			}
		}
	}
	// pass 2: members of the current epochs
	for _, o := range use {
		w := workObs{key: o.key, short: o.short, hostS: o.hostS, pos: o.pos, cov: o.cov, m0: int32(len(f.wm))}
		var prev model.StationIdx
		first := true
		for _, m := range allM[o.m0:o.m1] {
			s := f.station(m.Station, false)
			if s == nil || !s.usable() || !s.hasEpoch || m.Epoch != s.epoch {
				continue
			}
			// one member per station
			dup := false
			for _, x := range f.wm[w.m0:] {
				if x.st == s {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			y := s.localS(m.Ticks, f.frameRef(s.cls, o.short))
			if s.cls.GPS() {
				s.noteHost(y - o.hostS)
			}
			d := o.pos.Sub(s.info.Pos)
			f.wm = append(f.wm, workMember{st: s, y: y, rho: f.prop(s.info.Pos, o.pos), u: d.Unit()})
			s.lastObsS = tE
			if !first {
				a, b := prev, s.idx
				if a > b {
					a, b = b, a
				}
				f.edges[uint64(a)<<32|uint64(b)] = tE
			}
			prev, first = s.idx, false
		}
		w.m1 = int32(len(f.wm))
		if w.m1-w.m0 >= 2 {
			f.work = append(f.work, w)
		} else {
			f.wm = f.wm[:w.m0]
		}
	}
	f.carry, f.carryM = keep, keepM
}

func (f *Filter) hostVar() float64 {
	v := f.cfg.HostSigmaS * 1e9
	return v * v
}

// newEpoch starts a new clock epoch of a station at the member (ticks,
// hostS): a new base, θ re-initialised from the host time (σ =
// HostSigmaS), φ kept unless the receiver restarted (DESIGN §9.2.4).
func (f *Filter) newEpoch(s *station, ep uint32, ticks uint64, hostS float64, short bool, hostVar, tE float64) {
	reason := model.ClockEventReason("")
	if s.evValid && s.evEpoch == ep {
		reason = s.evReason
		s.evValid = false
	}
	rebase := s.hasEpoch && s.epoch == ep // same epoch, re-based after a drop-out
	if s.hasEpoch && !rebase {
		s.prevEpoch = s.epoch
	}
	if s.hasEpoch && !rebase && s.prevEpoch != 0 {
		r := string(reason)
		if r == "" {
			r = "unannounced"
		}
		s.addEvent(tE, "epoch", r)
		if s.mode == modeAnchor {
			s.demoteReq = "epoch_" + r
		}
	}
	s.epoch, s.hasEpoch = ep, true
	s.baseTicks = ticks
	ref := f.frameRef(s.cls, short)
	var th float64
	if s.cls.GPS() {
		// GPS ticks are Unix (or GPS-time) ns: the nominal mapping is exact
		s.baseS = float64(int64(ticks)-f.cfg.OriginUnixNs-f.shiftS*1e9) / 1e9
		th = (s.baseS - ref*1e-9 - hostS) * 1e9
	} else {
		s.baseS = hostS + ref*1e-9
	}
	s.streakN, s.streakSign, s.suspect = 0, 0, 0
	s.hostDevS, s.hostDevN = 0, 0
	s.freeSince = tE
	if s.comp != nil && !s.fresh {
		c := s.comp
		it := s.off + int(s.layout[kTheta])
		c.x[it] = th
		c.resetState(it, hostVar)
		if reason == model.ClockEvRestart && s.mode == modeFree {
			ip := s.off + int(s.layout[kPhi])
			c.x[ip] = 0
			c.resetState(ip, s.prior.phiSig*s.prior.phiSig)
			if s.psi {
				is := s.off + int(s.layout[kPsi])
				c.x[is] = 0
				c.resetState(is, s.prior.psiSig*s.prior.psiSig)
			}
		}
		return
	}
	s.initTh = th
	s.fresh = true
}

// assign hands every work item to the components of its members.
func (f *Filter) assign() {
	for _, c := range f.comps {
		c.jobs = c.jobs[:0]
	}
	var seen [8]*component
	for i := range f.work {
		w := &f.work[i]
		ns := 0
		for _, m := range f.wm[w.m0:w.m1] {
			c := m.st.comp
			if c == nil {
				continue
			}
			dup := false
			for _, x := range seen[:ns] {
				if x == c {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			if ns < len(seen) {
				seen[ns] = c
				ns++
			}
			c.jobs = append(c.jobs, int32(i))
		}
	}
}

// runComponents gates, accumulates and updates every component, in
// parallel.
func (f *Filter) runComponents() {
	prev := f.snap.Load()
	workers := min(f.cfg.Workers, len(f.comps))
	if workers <= 1 {
		for _, c := range f.comps {
			f.runComponent(c, prev)
		}
		return
	}
	ch := make(chan *component)
	var wg sync.WaitGroup
	// A panic in a worker goroutine cannot be recovered by Step; it is
	// caught here (the worker keeps draining the channel so the sender never
	// blocks) and re-raised on Step's goroutine after the epoch.
	var pmu sync.Mutex
	var pv any
	var pstack []byte
	run := func(c *component) {
		defer func() {
			if r := recover(); r != nil {
				pmu.Lock()
				if pv == nil {
					pv, pstack = r, debug.Stack()
				}
				pmu.Unlock()
			}
		}()
		f.runComponent(c, prev)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range ch {
				run(c)
			}
		}()
	}
	// largest first for balance
	order := make([]*component, len(f.comps))
	copy(order, f.comps)
	sortCompsBySize(order)
	for _, c := range order {
		ch <- c
	}
	close(ch)
	wg.Wait()
	if pv != nil {
		panic(workerPanic{v: pv, stack: pstack})
	}
}

// workerPanic carries a component worker's panic (and its own stack) to
// Step's goroutine.
type workerPanic struct {
	v     any
	stack []byte
}

// recoverStep handles a panic inside Step (caller holds stepMu): it reports
// it, discards every component and clock state and lets the stations re-sync
// from their class priors, so a filter fault costs a re-convergence instead
// of the whole process (finding SCALE-01).
func (f *Filter) recoverStep(v any, stack []byte, tE float64) {
	if wp, ok := v.(workerPanic); ok {
		v, stack = wp.v, wp.stack
	}
	f.stats.PanicResets++
	if f.cfg.OnPanic != nil {
		f.cfg.OnPanic(v, stack)
	}
	f.logf("clock: panic in Step, resetting every clock state: %v", v)
	f.resetAll(tE)
	func() {
		defer func() { _ = recover() }()
		f.publish(tE)
	}()
}

// resetAll drops every component; known stations start afresh (new epoch
// base, class prior) at their next observation.
func (f *Filter) resetAll(tE float64) {
	f.comps = nil
	f.carry, f.carryM = nil, nil
	f.work, f.wm = f.work[:0], f.wm[:0]
	for _, s := range f.st {
		if s == nil {
			continue
		}
		s.comp = nil
		s.fresh = true
		s.rejoin = false
		s.hasEpoch = false
		if s.mode == modeAnchor {
			s.mode = modeFree
			s.demoteReq = ""
			s.selfAsserted = false
			s.addEvent(tE, "anchor_lost", "clock filter reset")
		}
	}
	f.stats.Components, f.stats.Stations = 0, 0
	if tE > f.tE {
		f.tE = tE
	}
	f.started = true
}

func (f *Filter) runComponent(c *component, prev *Snapshot) {
	c.ar.reset()
	c.acRes, c.peerRes = c.acRes[:0], c.peerRes[:0]
	c.nAcc, c.nRej, c.updated, c.xsFailed = 0, 0, false, false
	for _, s := range c.sts {
		s.influ, s.epAcc = 0, 0
	}
	c.xs.reset(c.n, f.cfg.ExternalStates)
	for _, j := range c.jobs {
		f.processGroup(c, &f.work[j], prev)
	}
	if len(c.ar.grp) == 0 {
		return
	}
	n := c.n
	c.ensure()
	clear(c.J)
	clear(c.g)
	f.accumulate(c)
	c.xsFailed = !c.marginalize()
	f.addGauges(c)
	for _, s := range c.sts {
		if s.epAcc == 0 {
			continue
		}
		for k := stateKind(0); k < numKinds; k++ {
			if l := s.layout[k]; l >= 0 {
				i := s.off + int(l)
				s.pre[k], s.preX[k] = c.P[i*n+i], c.x[i]
			}
		}
	}
	if c.update() {
		c.symmetrize()
		c.updated = true
	}
}

// addGauges adds the component's gauge constraints to J, g.
func (f *Filter) addGauges(c *component) {
	var idxA [64]int
	var wA [64]float64
	switch c.gauge {
	case gaugeAnchor:
		idx, w := c.gaugeIndices(kTheta, idxA[:0], wA[:0])
		c.addConstraint(idx, w, c.gTheta, f.cfg.AnchorGaugeSigmaNs)
	case gaugeRelative:
		idx, w := c.gaugeIndices(kTheta, idxA[:0], wA[:0])
		c.addConstraint(idx, w, c.gTheta, f.cfg.RelGaugeThetaSigmaNs)
		idx, w = c.gaugeIndices(kPhi, idx[:0], w[:0])
		c.addConstraint(idx, w, c.gPhi, f.cfg.RelGaugeFreqSigmaPPB)
		if c.betaGauge {
			idx, w = c.gaugeIndices(kBeta, idx[:0], w[:0])
			c.addConstraint(idx, w, c.gBeta, f.cfg.RelGaugeBetaSigmaNs)
		}
	}
}

// regaugeTo installs a new gauge (kind and set) on a component: the targets
// are the current means (continuity) and the accumulated information of the
// old gauge combination is released (component.projectGauge), so that a
// change of the gauge set does not pin the stations that join or leave it.
func (f *Filter) regaugeTo(c *component, kind gaugeKind, set []model.StationIdx) {
	old := c.gauge
	c.betaGauge = false
	c.gauge, c.gSet = kind, set
	if c.n == 0 {
		return
	}
	var idxA, uA [64]int
	var wA [64]float64
	if kind == gaugeNone {
		// A partition that stops defining the time scale is pinned by its
		// external members from now on: release the old definition (to
		// 1 µs, which the external members tighten within an epoch).
		if old != gaugeNone && !f.cfg.NoGaugeProjection {
			u := c.commonIndices(kTheta, uA[:0])
			w := wA[:0]
			for range u {
				w = append(w, 1/float64(len(u)))
			}
			c.projectGauge(u, u, w, 1000)
		}
		return
	}
	idx, w := c.gaugeIndices(kTheta, idxA[:0], wA[:0])
	c.gTheta = c.sumState(idx, w)
	sig := f.cfg.AnchorGaugeSigmaNs
	if kind == gaugeRelative {
		sig = f.cfg.RelGaugeThetaSigmaNs
	}
	if !f.cfg.NoGaugeProjection {
		c.projectGauge(c.commonIndices(kTheta, uA[:0]), idx, w, sig)
	}
	if kind != gaugeRelative {
		return
	}
	idx, w = c.gaugeIndices(kPhi, idx[:0], w[:0])
	c.gPhi = c.sumState(idx, w)
	if !f.cfg.NoGaugeProjection {
		c.projectGauge(c.commonIndices(kPhi, uA[:0]), idx, w, f.cfg.RelGaugeFreqSigmaPPB)
	}
	c.betaGauge = f.cfg.RelGaugeBetaSigmaNs > 0 && !c.gpsInSet()
	if c.betaGauge {
		idx, w = c.gaugeIndices(kBeta, idx[:0], w[:0])
		c.gBeta = c.sumState(idx, w)
		if !f.cfg.NoGaugeProjection {
			c.projectGauge(c.commonIndices(kBeta, uA[:0]), idx, w, f.cfg.RelGaugeBetaSigmaNs)
		}
	}
}

// setGauges decides every component's gauge and keeps its target
// continuous when the gauge set changes.
func (f *Filter) setGauges() {
	domAnch := map[uint32]*component{} // the partition with the most anchors
	domPrimary := map[uint32]*component{}
	for _, c := range f.comps {
		c.anchors = 0
		for _, s := range c.sts {
			if s.mode == modeAnchor {
				c.anchors++
			}
		}
		if p := domAnch[c.domain]; c.anchors > 0 && (p == nil || c.anchors > p.anchors || (c.anchors == p.anchors && c.id < p.id)) {
			domAnch[c.domain] = c
		}
		if p := domPrimary[c.domain]; p == nil || len(c.sts) > len(p.sts) || (len(c.sts) == len(p.sts) && c.id < p.id) {
			domPrimary[c.domain] = c
		}
	}
	k := f.k
	win := int64(math.Ceil(f.cfg.SyncedWindowS / f.cfg.EpochS))
	for _, c := range f.comps {
		c.relative = domAnch[c.domain] == nil
		kind := gaugeNone
		var set []model.StationIdx
		switch {
		case domAnch[c.domain] == c:
			// One gauge per time domain: in a partitioned domain only the
			// partition with the most anchors defines network time (the mean
			// of its anchors); the others are pinned to it through their
			// external members. A gauge per anchored partition gave each
			// its own time scale (mean cable delay of its own anchors),
			// and their mismatch showed as anchor errors 2.5× their σ.
			kind = gaugeAnchor
			for _, s := range c.sts {
				if s.mode == modeAnchor {
					set = append(set, s.idx)
				}
			}
		case c.relative && domPrimary[c.domain] == c:
			// The relative time scale follows the GPS-class clocks when
			// there are any (their tight frequency prior is a statement
			// about UTC, so the scale must not run at the mean rate of the
			// free-running crystals), else the synced free clocks.
			kind = gaugeRelative
			for _, s := range c.sts {
				if s.mode == modeFree && s.cls.GPS() && !s.demoted {
					set = append(set, s.idx)
				}
			}
			if len(set) == 0 {
				for _, s := range c.sts {
					if s.mode == modeFree && f.syncedNow(c, s, k, win) {
						set = append(set, s.idx)
					}
				}
			}
			if len(set) == 0 {
				for _, s := range c.sts {
					set = append(set, s.idx)
				}
			}
		}
		sortIdx(set)
		if kind != c.gauge || !equalIdx(set, c.gSet) {
			f.regaugeTo(c, kind, set)
		}
	}
}

// syncedNow evaluates the synced rule of DESIGN §9.2.4 for a station.
func (f *Filter) syncedNow(c *component, s *station, k, win int64) bool {
	if s.comp != c || s.dim == 0 {
		return false
	}
	it := s.off + int(s.layout[kTheta])
	sig := math.Sqrt(math.Max(c.P[it*c.n+it], 0))
	lim := s.cls.SyncedSigmaNs()
	if lim == 0 {
		lim = 300
	}
	return sig < lim && s.accIn(k, win) >= f.cfg.SyncedMinObs && s.suspect < 2
}

func containsMember(ms []Member, st model.StationIdx) bool {
	for _, m := range ms {
		if m.Station == st {
			return true
		}
	}
	return false
}
