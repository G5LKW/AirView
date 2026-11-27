package avfsrv

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/avf"
	"airview/internal/ingest"
	"airview/internal/model"
)

// Server is the AVF/1 listener. Create it with New and run ListenAndServe
// (or Serve). It is safe for concurrent use.
type Server struct {
	o   Options
	reg Registry
	ing *ingest.Ingest
	pol atomic.Pointer[Policy]
	lim *limiter

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	ln        net.Listener
	conns     map[*conn]struct{}
	byFeeder  map[string]*conn
	byStation map[model.StationIdx]*conn
	resume    map[string]resumeEntry
	saved     map[string]savedRx
	cooldown  map[string]time.Time
	closed    bool
	wg        sync.WaitGroup

	st      counters
	reasonM sync.Mutex
	reasons map[string]uint64

	minutes *minuteBook // selective-mode MINUTE_STATS merge (minutes.go)
}

type resumeEntry struct {
	feeder  string
	expires time.Time
}

// savedRx is the receiver table of an ended session, kept with its seqEpoch
// watermark so records replayed before the RECEIVER records of the next
// session (AVF.md §5.2 strategy (a)) find their station.
type savedRx struct {
	rx      []rxSaved
	expires time.Time
}

type rxSaved struct {
	rxNo       uint8
	cls        model.ClockClass
	epoch      uint32
	timestamps bool
	name       string
}

type counters struct {
	accepted, handshakeFail, sessions, records, frames, dropped, shed, mlatMagic,
	protoErr, claims, results, decoded atomic.Uint64
	hostClamped atomic.Uint64 // sessions whose host offset was corrected
	minutes     atomic.Uint64 // StationMinute records published
}

// New returns a server feeding ing. reg is the registry (admission, claims,
// connection log).
func New(o Options, reg Registry, ing *ingest.Ingest) (*Server, error) {
	o, err := o.withDefaults()
	if err != nil {
		return nil, err
	}
	if reg == nil || ing == nil {
		return nil, errors.New("avfsrv: registry and ingest are required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{o: o, reg: reg, ing: ing, lim: newLimiter(), ctx: ctx, cancel: cancel,
		conns: map[*conn]struct{}{}, byFeeder: map[string]*conn{}, byStation: map[model.StationIdx]*conn{},
		resume: map[string]resumeEntry{}, saved: map[string]savedRx{}, cooldown: map[string]time.Time{},
		reasons: map[string]uint64{}, minutes: newMinuteBook()}
	p := o.Policy
	s.pol.Store(&p)
	s.lim.setPolicy(p)
	return s, nil
}

// SetPolicy applies changed settings (config apply). Open sessions keep
// their WELCOME values; limits apply to new connections and stations.
func (s *Server) SetPolicy(p Policy) {
	p = p.normalized()
	s.pol.Store(&p)
	s.lim.setPolicy(p)
}

func (s *Server) policy() Policy { return *s.pol.Load() }

// ListenAndServe listens on Options.Addr and serves until ctx is done or
// Close is called. It returns nil after a clean shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := s.o.Listen("tcp", s.o.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve accepts connections on ln until ctx is done or Close is called.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return errors.New("avfsrv: server closed")
	}
	s.ln = ln
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	s.wg.Add(1)
	go s.janitor()
	var tempDelay time.Duration
	for {
		nc, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				tempDelay = min(max(2*tempDelay, 5*time.Millisecond), time.Second)
				time.Sleep(tempDelay)
				continue
			}
			return err
		}
		tempDelay = 0
		s.accept(nc)
	}
}

// Addr returns the listen address (nil before Serve).
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) reason(r string) {
	s.reasonM.Lock()
	s.reasons[r]++
	s.reasonM.Unlock()
}

// accept applies the connection limits and starts the connection goroutine.
func (s *Server) accept(nc net.Conn) {
	ip := addrOf(nc.RemoteAddr())
	if s.o.Gate != nil && !s.o.Gate() {
		s.reason("gate_closed")
		nc.Close()
		return
	}
	if r := s.lim.acquire(ip, s.o.MaxSessions); r != rejectNone {
		s.reason(r)
		nc.Close()
		return
	}
	c := &conn{s: s, nc: nc, ip: ip, rx: map[uint8]*rxState{}, started: s.o.Now(), done: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.lim.release(ip, true)
		nc.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	go c.serve()
}

func addrOf(a net.Addr) netip.Addr {
	if ta, ok := a.(*net.TCPAddr); ok {
		if ip, ok := netip.AddrFromSlice(ta.IP); ok {
			return ip.Unmap()
		}
	}
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.IPv4Unspecified()
}

// Close stops the listener, says GOODBYE shutting_down to every feeder and
// waits until every connection has ended and delivered its data.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.wg.Wait()
		return nil
	}
	s.closed = true
	ln := s.ln
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	s.cancel()
	for _, c := range conns {
		c.shutdown(avf.CodeShuttingDown, "server restart", 15)
	}
	s.wg.Wait()
	return nil
}

// janitor expires resume tokens, saved receiver tables, cool-downs and
// watermarks once a minute.
func (s *Server) janitor() {
	defer s.wg.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			now := s.o.Now()
			s.o.Watermarks.Sweep(now)
			s.minutes.sweep(now)
			s.mu.Lock()
			for k, v := range s.resume {
				if now.After(v.expires) {
					delete(s.resume, k)
				}
			}
			for k, v := range s.saved {
				if now.After(v.expires) {
					delete(s.saved, k)
				}
			}
			for k, v := range s.cooldown {
				if now.After(v) {
					delete(s.cooldown, k)
				}
			}
			s.mu.Unlock()
		}
	}
}

func newToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("avfsrv: crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ------------------------------------------------------------ engine API

// SendResults sends MLAT results to the station's feeder (MLAT_RESULTS, in
// records of at most 1000 fixes). The caller (the MLAT engine) applies the
// contribution rule of DESIGN §8.4; the fixes carry no station identities.
// It reports whether the station has a session that accepts results.
func (s *Server) SendResults(st model.StationIdx, fixes []model.MLATFix) bool {
	c := s.connOf(st)
	if c == nil || c.c == nil || !c.c.HasFeature(avf.FeatResults) || !s.policy().ReturnResults || !c.returnResults {
		return false
	}
	for len(fixes) > 0 {
		n := min(len(fixes), avf.MaxFixesPerRecord)
		if err := c.c.WriteMLATResults(fixes[:n]); err != nil {
			return false
		}
		s.st.results.Add(uint64(n))
		c.addResults(st, uint64(n))
		fixes = fixes[n:]
	}
	return true
}

// SendFeedback sends a FEEDBACK record about station st to its feeder.
func (s *Server) SendFeedback(st model.StationIdx, fb avf.Feedback) bool {
	c := s.connOf(st)
	if c == nil || c.c == nil {
		return false
	}
	rx, ok := c.rxOf(st)
	if !ok {
		return false
	}
	fb.RxNo = int(rx)
	return c.c.WriteJSON(avf.TFeedback, &fb) == nil
}

// Notice sends a NOTICE to the feeder of station st.
func (s *Server) Notice(st model.StationIdx, code, level, text string) bool {
	c := s.connOf(st)
	if c == nil || c.c == nil {
		return false
	}
	return c.notice(code, level, text) == nil
}

// Disconnect closes the session of a feeder with an error code (admin
// "kick", revocation). reconnectIn asks the feeder to wait (seconds).
func (s *Server) Disconnect(feederID, code, reason string, reconnectIn int) bool {
	s.mu.Lock()
	c := s.byFeeder[feederID]
	s.mu.Unlock()
	if c == nil {
		return false
	}
	c.shutdown(code, reason, reconnectIn)
	return true
}

func (s *Server) connOf(st model.StationIdx) *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byStation[st]
}

// ------------------------------------------------------------ status

// Stats are the server counters.
type Stats struct {
	Accepted       uint64            `json:"accepted"`       // sessions established
	Open           int               `json:"open"`           // open connections
	HandshakeFail  uint64            `json:"handshakeFail"`  // TLS/HELLO failures
	Records        uint64            `json:"records"`        // records processed
	Frames         uint64            `json:"frames"`         // frames handed to the ingest
	Dropped        uint64            `json:"dropped"`        // frames of unknown or refused receivers
	Shed           uint64            `json:"shed"`           // frames refused by the global ingest budget
	MLATMagic      uint64            `json:"mlatMagic"`      // frames with the MLAT magic timestamp (refused)
	ProtocolErrors uint64            `json:"protocolErrors"` // sessions closed for a protocol error
	Claims         uint64            `json:"claims"`         // claim codes issued
	Results        uint64            `json:"results"`        // MLAT fixes sent
	Decoded        uint64            `json:"decoded"`        // DECODED updates handed to the tracker
	HostClamped    uint64            `json:"hostClamped"`    // sessions whose host-clock offset was corrected (ROB-1)
	StationMinutes uint64            `json:"stationMinutes"` // merged MINUTE_STATS published (Hooks.OnStationMinute)
	Reasons        map[string]uint64 `json:"reasons"`        // refusals by reason / WELCOME code
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	st := Stats{Accepted: s.st.accepted.Load(), HandshakeFail: s.st.handshakeFail.Load(),
		Records: s.st.records.Load(), Frames: s.st.frames.Load(), Dropped: s.st.dropped.Load(),
		Shed: s.st.shed.Load(), MLATMagic: s.st.mlatMagic.Load(), ProtocolErrors: s.st.protoErr.Load(),
		Claims: s.st.claims.Load(), Results: s.st.results.Load(), Decoded: s.st.decoded.Load(),
		HostClamped: s.st.hostClamped.Load(), StationMinutes: s.st.minutes.Load(),
		Reasons: map[string]uint64{}}
	s.reasonM.Lock()
	for k, v := range s.reasons {
		st.Reasons[k] = v
	}
	s.reasonM.Unlock()
	s.mu.Lock()
	st.Open = len(s.conns)
	s.mu.Unlock()
	return st
}

// SessionInfo describes an open session for the admin view. Remote is a
// raw IP address: admin scope only (DESIGN §8.2).
type SessionInfo struct {
	FeederID  string           `json:"feederId"`
	Remote    string           `json:"remote"`
	Software  string           `json:"software"`
	Version   string           `json:"version"`
	OS        string           `json:"os"`
	Arch      string           `json:"arch"`
	Status    string           `json:"status"` // WELCOME status
	StartedMs int64            `json:"startedMs"`
	RTTMs     float64          `json:"rttMs"`
	OffsetMs  float64          `json:"offsetMs"` // feeder host clock minus server
	RecordsIn uint64           `json:"recordsIn"`
	WireBytes uint64           `json:"wireBytesIn"`
	QueueFill float64          `json:"queueFill"`
	Backfill  bool             `json:"backfill"`
	Features  []string         `json:"features"`
	Stations  []SessionStation `json:"stations"`
	Resumed   bool             `json:"resumed"`
	SeqEpoch  string           `json:"seqEpoch"`
	Watermark uint64           `json:"watermark"`
	FramesIn  uint64           `json:"framesIn"`
	DroppedIn uint64           `json:"droppedIn"`
	// Results counts the MLAT fixes sent on this connection (all its
	// receivers; per receiver in Stations).
	Results uint64 `json:"results"`
	// HostClampMs is the correction applied to the feeder's host-clock
	// offset because its host times claimed frames older than the link
	// allows (0 = none; hostguard.go, finding ROB-1).
	HostClampMs float64 `json:"hostClampMs,omitempty"`
}

// SessionStation is one receiver of a session.
type SessionStation struct {
	Station model.StationIdx `json:"station"`
	RxNo    int              `json:"rxNo"`
	Status  string           `json:"status"` // approved | pending | denied
	Class   string           `json:"clockClass"`
	Epoch   uint32           `json:"epoch"`
	Mode    string           `json:"mode"`
	Results uint64           `json:"results"` // MLAT fixes sent to this station on this session
}

// Sessions lists the open sessions (sorted by feeder ID).
func (s *Server) Sessions() []SessionInfo {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		if c.c != nil {
			conns = append(conns, c)
		}
	}
	s.mu.Unlock()
	out := make([]SessionInfo, 0, len(conns))
	for _, c := range conns {
		out = append(out, c.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FeederID < out[j].FeederID })
	return out
}
