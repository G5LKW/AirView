package ingest

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
)

// Traffic modes (DESIGN §4.4).
const (
	ModeFull      = "full"
	ModeSelective = "selective"
)

// FlowCmd is a FLOW instruction for a link (AVF FLOW, DESIGN §4.4).
type FlowCmd struct {
	BatchMs    int     // 0 = keep the current interval
	Pause      bool    // send only control records until resumed
	SlowFactor float64 // (0,1]: fraction of the current data rate; 0 = 1
}

// InterestCmd is a traffic-mode / interest-set update for one station (AVF
// INTEREST). Addresses are tracker keys (24-bit ICAO; bit 24 marks a
// non-ICAO address).
type InterestCmd struct {
	Mode     string
	ReduceMs int
	Reset    bool
	SyncAdd  []uint32
	SyncDel  []uint32
	MLATAdd  []uint32
	MLATDel  []uint32
}

// Controller is implemented by a protocol session (avfsrv) so the ingest can
// steer the link. Calls come from the ingest's tick goroutine; they must not
// block for long (queue the record).
type Controller interface {
	// SendFlow sends a FLOW instruction to the link.
	SendFlow(FlowCmd) error
	// SendInterest sends a traffic mode / interest update for station st.
	SendInterest(st model.StationIdx, in InterestCmd) error
	// Disconnect closes the link with an error code (AVF GOODBYE) and asks
	// the client to stay away reconnectIn seconds.
	Disconnect(code, reason string, reconnectIn int)
}

// SessionInfo describes a link that carries one or more stations.
type SessionInfo struct {
	// Proto is "avf", "beast", "mlatc" or "host".
	Proto string
	// Label identifies the link in logs and admin views (no IP address).
	Label string
	// Controller steers the link (nil = no FLOW / INTEREST possible).
	Controller Controller
	// Selective is set when the link can be moved to selective traffic.
	Selective bool
}

// Session is a registered link. Create it with Ingest.OpenSession and Close
// it when the link ends.
type Session struct {
	ing  *Ingest
	id   uint64
	info SessionInfo

	bytes   atomic.Uint64
	fillBit atomic.Uint64 // float64 bits of the queue fill
	offSet  atomic.Bool
	off     atomic.Int64
	rtt     atomic.Int64

	// guarded by ing.sess.mu
	stations   map[model.StationIdx]*sessStation
	mon        avf.FlowMonitor
	flowActive bool
	calmSince  time.Time
	forced     bool // selective forced by the flow rules
	lastBytes  uint64
	lastTick   time.Time
	bps        float64
	closed     bool
	kicked     bool // disconnected for its rate (once)
}

type sessStation struct {
	mode       string
	lastFrames uint64
	fps        float64
}

// sessions is the ingest's session table.
type sessions struct {
	mu       sync.Mutex
	next     uint64
	all      map[uint64]*Session
	byStn    map[model.StationIdx]*Session
	grace    map[model.StationIdx]time.Time // stations kept online after a short gap
	interest map[model.StationIdx]*interestSets
	limits   model.AggLimits
	global   string // current global mode
}

type interestSets struct {
	sync, mlat map[uint32]struct{}
}

func (ss *sessions) init() {
	ss.all = map[uint64]*Session{}
	ss.byStn = map[model.StationIdx]*Session{}
	ss.grace = map[model.StationIdx]time.Time{}
	ss.interest = map[model.StationIdx]*interestSets{}
	ss.global = ModeFull
}

// OpenSession registers a link.
func (ing *Ingest) OpenSession(info SessionInfo) *Session {
	ss := &ing.sess
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.limits.MaxFPSPerStation == 0 {
		ss.limits = ing.o.Limits
	}
	ss.next++
	s := &Session{ing: ing, id: ss.next, info: info, stations: map[model.StationIdx]*sessStation{}}
	s.mon = avf.FlowMonitor{Hold: ing.o.FlowHold, Grace: ing.o.FlowGrace}
	s.lastTick = ing.o.Now()
	ss.all[s.id] = s
	return s
}

// ID returns the session number.
func (s *Session) ID() uint64 { return s.id }

// AddStation attaches station st to the session (an AVF RECEIVER, a Beast
// attach) and returns the traffic mode it should start in. A station already
// attached to another session moves here.
func (s *Session) AddStation(st model.StationIdx) string {
	ss := &s.ing.sess
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if s.closed {
		return ModeFull
	}
	if old := ss.byStn[st]; old != nil && old != s {
		delete(old.stations, st)
	}
	ss.byStn[st] = s
	delete(ss.grace, st)
	mode := ModeFull
	if s.info.Selective && (ss.global == ModeSelective || s.forced) {
		mode = ModeSelective
	}
	sst := &sessStation{mode: mode}
	if x := s.ing.st.get(st, true); x != nil {
		sst.lastFrames = x.frames.Load()
		if s.offSet.Load() {
			x.pingOff.Store(s.off.Load())
			x.pingRTT.Store(s.rtt.Load())
			x.pingSet.Store(true)
		}
	}
	s.stations[st] = sst
	return mode
}

// RemoveStation detaches st (receiver denied or revoked).
func (s *Session) RemoveStation(st model.StationIdx) {
	ss := &s.ing.sess
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if _, ok := s.stations[st]; ok {
		delete(s.stations, st)
		if ss.byStn[st] == s {
			delete(ss.byStn, st)
		}
	}
}

// AddBytes counts received wire bytes (compressed) for the rate limits.
func (s *Session) AddBytes(n int) { s.bytes.Add(uint64(n)) }

// SetQueueFill reports the link's server-side queue fill (0..1).
func (s *Session) SetQueueFill(f float64) { s.fillBit.Store(math.Float64bits(f)) }

// SetHostOffset reports the feeder's host-clock offset (feeder minus server)
// and round trip from PING/PONG; it applies to every station of the link.
func (s *Session) SetHostOffset(offset, rtt time.Duration) {
	s.off.Store(int64(offset))
	s.rtt.Store(int64(rtt))
	s.offSet.Store(true)
	ss := &s.ing.sess
	ss.mu.Lock()
	sts := make([]model.StationIdx, 0, len(s.stations))
	for st := range s.stations {
		sts = append(sts, st)
	}
	ss.mu.Unlock()
	for _, st := range sts {
		x := s.ing.st.get(st, true)
		x.pingOff.Store(int64(offset))
		x.pingRTT.Store(int64(rtt))
		x.pingSet.Store(true)
	}
}

// Mode returns st's current traffic mode in this session.
func (s *Session) Mode(st model.StationIdx) string {
	ss := &s.ing.sess
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if x := s.stations[st]; x != nil {
		return x.mode
	}
	return ModeFull
}

// Close ends the session. keepOnline keeps its stations counted as online
// for that long (AVF resume token: gaps under 30 s, DESIGN §4.5).
func (s *Session) Close(keepOnline time.Duration) {
	ss := &s.ing.sess
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	delete(ss.all, s.id)
	until := s.ing.o.Now().Add(keepOnline)
	for st := range s.stations {
		if ss.byStn[st] == s {
			delete(ss.byStn, st)
			if keepOnline > 0 {
				ss.grace[st] = until
			}
		}
	}
}

// DesiredMode returns the traffic mode a new link should be told in its
// WELCOME: selective when more approved stations are online than
// Limits.SelectiveAbove.
func (ing *Ingest) DesiredMode() string {
	ss := &ing.sess
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.global
}

// SetInterest applies an interest-set change for station st (from the MLAT
// planner) and forwards it to the station's link when it is in selective
// mode. With Reset the sets are replaced.
func (ing *Ingest) SetInterest(st model.StationIdx, in InterestCmd) error {
	ss := &ing.sess
	ss.mu.Lock()
	sets := ss.interest[st]
	if sets == nil {
		sets = &interestSets{sync: map[uint32]struct{}{}, mlat: map[uint32]struct{}{}}
		ss.interest[st] = sets
	}
	if in.Reset {
		clear(sets.sync)
		clear(sets.mlat)
	}
	for _, k := range in.SyncDel {
		delete(sets.sync, k)
	}
	for _, k := range in.MLATDel {
		delete(sets.mlat, k)
	}
	for _, k := range in.SyncAdd {
		sets.sync[k] = struct{}{}
	}
	for _, k := range in.MLATAdd {
		sets.mlat[k] = struct{}{}
	}
	s := ss.byStn[st]
	var ctl Controller
	if s != nil && s.info.Controller != nil && s.info.Selective {
		if x := s.stations[st]; x != nil && x.mode == ModeSelective {
			ctl = s.info.Controller
		}
	}
	ss.mu.Unlock()
	if ctl == nil {
		return nil
	}
	in.Mode = ModeSelective
	if in.ReduceMs == 0 {
		in.ReduceMs = ing.o.ReduceMs
	}
	return ctl.SendInterest(st, in)
}

// fullInterest builds a reset INTEREST for st with its whole sets. Caller
// holds ss.mu.
func (ss *sessions) fullInterest(st model.StationIdx, mode string, reduceMs int) InterestCmd {
	in := InterestCmd{Mode: mode, ReduceMs: reduceMs, Reset: true}
	if mode == ModeSelective {
		if sets := ss.interest[st]; sets != nil {
			for k := range sets.sync {
				in.SyncAdd = append(in.SyncAdd, k)
			}
			for k := range sets.mlat {
				in.MLATAdd = append(in.MLATAdd, k)
			}
			sort.Slice(in.SyncAdd, func(i, j int) bool { return in.SyncAdd[i] < in.SyncAdd[j] })
			sort.Slice(in.MLATAdd, func(i, j int) bool { return in.MLATAdd[i] < in.MLATAdd[j] })
		}
	} else {
		in.ReduceMs = 0
	}
	return in
}

// Flow recovery: FLOW is lifted after the queue stayed below calmFill (and
// the rates within the limits) for calmFor.
const (
	calmFill = 0.25
	calmFor  = 30 * time.Second
)

type ctlAction struct {
	ctl     Controller
	st      model.StationIdx
	flow    *FlowCmd
	in      *InterestCmd
	discon  bool
	code    string
	reason  string
	seconds int
}

// Tick evaluates the sessions once a second (Run calls it): per-station
// rates, the FLOW escalation (FLOW, selective, disconnect; DESIGN §4.4,
// §4.6), FLOW recovery, and the full/selective traffic-mode policy.
func (ing *Ingest) Tick(now time.Time) {
	ss := &ing.sess
	var acts []ctlAction
	ss.mu.Lock()
	for st, until := range ss.grace {
		if now.After(until) {
			delete(ss.grace, st)
		}
	}
	approved := 0
	count := func(st model.StationIdx) {
		if ing.admission(st) == model.AdmApproved {
			approved++
		}
	}
	for st := range ss.byStn {
		count(st)
	}
	for st := range ss.grace {
		if ss.byStn[st] == nil {
			count(st)
		}
	}
	// Stations without a session (Beast 30004, the host station) count while
	// they delivered frames in the last minute: they load the network too.
	active := now.Add(-time.Minute).UnixNano()
	ing.st.each(func(x *stState) {
		if _, ok := ss.byStn[x.idx]; ok {
			return
		}
		if _, ok := ss.grace[x.idx]; ok {
			return
		}
		if x.lastNs.Load() >= active {
			count(x.idx)
		}
	})
	lim := ss.limits
	if lim.MaxFPSPerStation == 0 {
		lim = ing.o.Limits
	}
	global := ModeFull
	if approved > lim.SelectiveAbove {
		global = ModeSelective
	}
	ss.global = global
	maxFPS := float64(lim.MaxFPSPerStation)
	maxBps := float64(lim.MaxKBpsPerStation) * 1000
	for _, s := range ss.all {
		dt := now.Sub(s.lastTick).Seconds()
		if dt <= 0 {
			continue
		}
		s.lastTick = now
		b := s.bytes.Load()
		s.bps = float64(b-s.lastBytes) / dt
		s.lastBytes = b
		top := 0.0
		for st, x := range s.stations {
			if xs := ing.st.get(st, false); xs != nil {
				f := xs.frames.Load()
				x.fps = float64(f-x.lastFrames) / dt
				x.lastFrames = f
			}
			top = max(top, x.fps)
		}
		n := max(1, len(s.stations))
		fill := math.Float64frombits(s.fillBit.Load())
		s.mon.MaxFPS, s.mon.MaxBytesPerSec = maxFPS, maxBps*float64(n)
		ctl := s.info.Controller
		act := s.mon.Observe(now, fill, top, s.bps)
		switch act {
		case avf.FlowSendOverLimits:
			sf := 1.0
			if maxFPS > 0 && top > maxFPS {
				sf = maxFPS / top
			}
			if maxBps > 0 && s.bps > maxBps*float64(n) {
				sf = min(sf, maxBps*float64(n)/s.bps)
			}
			s.flowActive, s.calmSince = true, time.Time{}
			acts = append(acts, ctlAction{ctl: ctl, flow: &FlowCmd{SlowFactor: clampF(sf, 0.05, 1)}})
		case avf.FlowSend:
			s.flowActive, s.calmSince = true, time.Time{}
			acts = append(acts, ctlAction{ctl: ctl, flow: &FlowCmd{BatchMs: 200, SlowFactor: 0.5}})
		case avf.FlowSelective:
			s.forced = true
		case avf.FlowDisconnect:
			if s.kicked {
				break
			}
			s.kicked = true
			acts = append(acts, ctlAction{ctl: ctl, discon: true, code: avf.CodeRateLimited,
				reason: "the station exceeded its rate limit", seconds: 300})
		}
		if s.flowActive && act == avf.FlowNone {
			calm := fill < calmFill && (maxFPS <= 0 || top <= maxFPS) && (maxBps <= 0 || s.bps <= maxBps*float64(n))
			switch {
			case !calm:
				s.calmSince = time.Time{}
			case s.calmSince.IsZero():
				s.calmSince = now
			case now.Sub(s.calmSince) >= calmFor:
				s.flowActive, s.calmSince = false, time.Time{}
				acts = append(acts, ctlAction{ctl: ctl, flow: &FlowCmd{BatchMs: int(avf.DefaultBatch / time.Millisecond), SlowFactor: 1}})
			}
		}
		if !s.info.Selective {
			continue
		}
		for st, x := range s.stations {
			want := ModeFull
			if global == ModeSelective || s.forced {
				want = ModeSelective
			}
			if want != x.mode {
				x.mode = want
				in := ss.fullInterest(st, want, ing.o.ReduceMs)
				acts = append(acts, ctlAction{ctl: ctl, st: st, in: &in})
			}
		}
	}
	ss.mu.Unlock()
	for _, a := range acts {
		if a.ctl == nil {
			continue
		}
		switch {
		case a.discon:
			a.ctl.Disconnect(a.code, a.reason, a.seconds)
		case a.flow != nil:
			if err := a.ctl.SendFlow(*a.flow); err != nil {
				ing.o.Logger.Debug("ingest: FLOW failed", "err", err)
			}
		case a.in != nil:
			if err := a.ctl.SendInterest(a.st, *a.in); err != nil {
				ing.o.Logger.Debug("ingest: INTEREST failed", "err", err)
			}
		}
	}
}

// SessionStatus describes a session for the admin view (no addresses).
type SessionStatus struct {
	ID          uint64          `json:"id"`
	Proto       string          `json:"proto"`
	Label       string          `json:"label"`
	QueueFill   float64         `json:"queueFill"`
	BytesPerSec float64         `json:"bytesPerSec"`
	Flow        bool            `json:"flow"`     // FLOW is active
	Forced      bool            `json:"forced"`   // forced to selective by the flow rules
	Stations    []StationStatus `json:"stations"` // sorted by station
}

// StationStatus is one station of a session.
type StationStatus struct {
	Station model.StationIdx `json:"station"`
	Mode    string           `json:"mode"`
	FPS     float64          `json:"fps"`
}

// Sessions lists the open sessions.
func (ing *Ingest) Sessions() []SessionStatus {
	ss := &ing.sess
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]SessionStatus, 0, len(ss.all))
	for _, s := range ss.all {
		o := SessionStatus{ID: s.id, Proto: s.info.Proto, Label: s.info.Label,
			QueueFill: math.Float64frombits(s.fillBit.Load()), BytesPerSec: s.bps, Flow: s.flowActive, Forced: s.forced}
		for st, x := range s.stations {
			o.Stations = append(o.Stations, StationStatus{Station: st, Mode: x.mode, FPS: x.fps})
		}
		sort.Slice(o.Stations, func(i, j int) bool { return o.Stations[i].Station < o.Stations[j].Station })
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Online reports whether st has an open session (or is within its resume
// grace period).
func (ing *Ingest) Online(st model.StationIdx) bool {
	ss := &ing.sess
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.byStn[st] != nil {
		return true
	}
	_, ok := ss.grace[st]
	return ok
}
