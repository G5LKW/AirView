package beastin

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Rejection and disconnect reasons (Stats, LinkStats.CloseReason).
const (
	ReasonGate       = "not ready"   // Options.Gate returned false
	ReasonMaxLinks   = "server full" // Options.MaxLinks
	ReasonPerIP      = "too many links from this address"
	ReasonUnattached = "too many links waiting for identification from this address"
	ReasonSubnet     = "too many links from this network"
	ReasonStations   = "too many stations behind this address"
	ReasonCloned     = "cloned station" // the UUID is live from another address
	ReasonCoolDown   = "cooling down"
	ReasonRegistry   = "refused"
	ReasonRate       = "over rate limit"
	ReasonIdle       = "idle"
	ReasonSilent     = "no data"
	ReasonEOF        = "closed by peer"
	ReasonError      = "read error"
	ReasonWrite      = "write error"
	ReasonShutdown   = "server shutdown"
)

// Server is the Beast ingest listener. Create it with New, run it with
// ListenAndServe or Serve, stop it with Close (or by cancelling the
// context).
type Server struct {
	opt  Options
	reg  Registry
	sink Sink
	pol  atomic.Pointer[policy]

	mu     sync.Mutex
	ln     net.Listener
	port   uint16
	links  map[uint64]*link
	perIP  map[netip.Addr]*ipState
	perNet map[netip.Prefix]int
	// uuidAt is where each live UUID identity is attached from (SEC-04):
	// a second link with the same UUID from another client while the
	// first is alive is a clone (or a spoof) and is refused, as for AVF.
	uuidAt   map[string]uuidHolder
	cool     map[string]time.Time
	coolScan time.Time
	closing  bool

	nextLink atomic.Uint64
	runq     chan *link // never nil; closed once (qClosed) after every link is gone
	qClosed  bool
	readers  sync.WaitGroup // link reader goroutines
	alive    sync.WaitGroup // links until their final batch is delivered
	workers  sync.WaitGroup
	start    sync.Once
	done     chan struct{}

	ctr counters
}

type ipState struct {
	conns, unattached int
	stations          map[string]int // identity key -> attached links
	slots             []bool         // unidentified slots in use (Identity.Slot)
}

// counters are the server-wide totals (atomic; read by Stats).
type counters struct {
	accepted                                          atomic.Uint64
	rejGate, rejMax, rejIP, rejUnid, rejNet           atomic.Uint64
	rejStations, rejCool, rejRegistry, rejCloned      atomic.Uint64
	discRate, discIdle, discSilent                    atomic.Uint64
	bytes, frames, delivered                          atomic.Uint64
	mlatDropped, rateDropped, queueDropped, budgetDrp atomic.Uint64
	unknownEsc, relayIgnored, slowDowns, pings        atomic.Uint64
}

// New returns a server. reg and sink must not be nil.
func New(opt Options, reg Registry, sink Sink) (*Server, error) {
	if reg == nil || sink == nil {
		return nil, errors.New("beastin: registry and sink are required")
	}
	opt = opt.normalized()
	if len(opt.ServerKey) == 0 {
		opt.ServerKey = make([]byte, 32)
		if _, err := rand.Read(opt.ServerKey); err != nil {
			return nil, fmt.Errorf("beastin: server key: %w", err)
		}
	}
	s := &Server{
		opt: opt, reg: reg, sink: sink,
		links:  map[uint64]*link{},
		perIP:  map[netip.Addr]*ipState{},
		perNet: map[netip.Prefix]int{},
		uuidAt: map[string]uuidHolder{},
		cool:   map[string]time.Time{},
		runq:   make(chan *link, opt.MaxLinks+opt.Workers+1),
		done:   make(chan struct{}),
	}
	s.pol.Store(compilePolicy(opt.Policy))
	return s, nil
}

// SetPolicy replaces the limits; links pick the new values up immediately.
func (s *Server) SetPolicy(p Policy) { s.pol.Store(compilePolicy(p)) }

// Policy returns the policy in force (with defaults filled in).
func (s *Server) Policy() Policy { return s.pol.Load().Policy }

// ListenAndServe listens on Options.Addr and serves until ctx is cancelled
// or Close is called.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opt.Addr)
	if err != nil {
		return fmt.Errorf("beastin: listen %s: %w", s.opt.Addr, err)
	}
	return s.Serve(ctx, ln)
}

// Addr returns the listener address once serving (nil before).
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Serve accepts links on ln until ctx is cancelled or Close is called. It
// returns nil after a regular shutdown. Serve may be called once.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	if s.ln != nil || s.closing {
		s.mu.Unlock()
		ln.Close()
		return errors.New("beastin: server already started or closed")
	}
	s.ln = ln
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		s.port = uint16(ta.Port)
	}
	s.mu.Unlock()
	s.startWorkers()

	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	s.opt.Logger.Info("beast ingest listening", "addr", ln.Addr().String())
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.isClosing() {
				s.wait()
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || isTemporary(err) {
				delay = min(max(delay*2, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			s.Close()
			s.wait()
			return fmt.Errorf("beastin: accept: %w", err)
		}
		delay = 0
		s.accept(c)
	}
}

func isTemporary(err error) bool {
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
}

func (s *Server) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// Close stops accepting, closes every link and waits until their last
// frames were delivered and detached. It is safe to call more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		<-s.done
		return nil
	}
	s.closing = true
	ln := s.ln
	links := make([]*link, 0, len(s.links))
	for _, l := range s.links {
		links = append(links, l)
	}
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, l := range links {
		l.shutdown(ReasonShutdown)
	}
	s.wait()
	return nil
}

// wait waits for readers, the final deliveries and the workers, once.
func (s *Server) wait() {
	s.readers.Wait()
	s.alive.Wait()
	s.mu.Lock()
	first := !s.qClosed
	if first {
		s.qClosed = true
		close(s.runq)
	}
	s.mu.Unlock()
	if first {
		s.workers.Wait()
		close(s.done)
	}
	<-s.done
}

func (s *Server) startWorkers() {
	s.start.Do(func() {
		q := s.runq
		for i := 0; i < s.opt.Workers; i++ {
			s.workers.Add(1)
			go func() {
				defer s.workers.Done()
				for l := range q {
					l.drain()
				}
			}()
		}
	})
}

// accept applies the connection limits and starts the link.
func (s *Server) accept(c net.Conn) {
	ip := remoteIP(c)
	if s.opt.Gate != nil && !s.opt.Gate() {
		s.ctr.rejGate.Add(1)
		c.Close()
		return
	}
	pol := s.pol.Load()
	allowed := pol.allowed(ip)
	now := s.opt.Now()

	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		c.Close()
		return
	}
	reason := ""
	st := s.perIP[ip]
	net24 := subnetOf(ip)
	switch {
	case len(s.links) >= s.opt.MaxLinks:
		reason = ReasonMaxLinks
		s.ctr.rejMax.Add(1)
	case allowed:
	case st != nil && pol.MaxConnsPerIP > 0 && st.conns >= pol.MaxConnsPerIP:
		reason = ReasonPerIP
		s.ctr.rejIP.Add(1)
	case st != nil && pol.MaxUnattachedPerIP > 0 && st.unattached >= pol.MaxUnattachedPerIP:
		reason = ReasonUnattached
		s.ctr.rejUnid.Add(1)
	case pol.MaxConnsPerSubnet > 0 && s.perNet[net24] >= pol.MaxConnsPerSubnet:
		reason = ReasonSubnet
		s.ctr.rejNet.Add(1)
	}
	if reason != "" {
		s.mu.Unlock()
		s.opt.Logger.Debug("beast link refused", "reason", reason)
		c.Close()
		return
	}
	if st == nil {
		st = &ipState{stations: map[string]int{}}
		s.perIP[ip] = st
	}
	st.conns++
	st.unattached++
	s.perNet[net24]++
	id := s.nextLink.Add(1)
	l := newLink(s, c, id, ip, now)
	s.links[id] = l
	s.readers.Add(1)
	s.alive.Add(1)
	s.mu.Unlock()
	s.ctr.accepted.Add(1)
	go l.run()
}

func remoteIP(c net.Conn) netip.Addr {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		if a, ok := netip.AddrFromSlice(ta.IP); ok {
			return a.Unmap()
		}
	}
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.IPv4Unspecified()
}

// coolingDown reports whether key is in a cool-down at now.
func (s *Server) coolingDown(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.cool[key]
	if ok && now.Before(until) {
		return true
	}
	if ok {
		delete(s.cool, key)
	}
	return false
}

// coolDown puts key into a cool-down for d.
func (s *Server) coolDown(key string, now time.Time, d time.Duration) {
	if d <= 0 || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.cool[key]; !ok || u.Before(now.Add(d)) {
		s.cool[key] = now.Add(d)
	}
	if now.Sub(s.coolScan) > time.Minute {
		s.coolScan = now
		for k, u := range s.cool {
			if now.After(u) {
				delete(s.cool, k)
			}
		}
	}
}

// attachedFirst moves a link out of the unattached count of its address.
func (s *Server) attachedFirst(ip netip.Addr) {
	s.mu.Lock()
	if st := s.perIP[ip]; st != nil && st.unattached > 0 {
		st.unattached--
	}
	s.mu.Unlock()
}

// takeSlot returns the lowest free unidentified slot of ip.
func (s *Server) takeSlot(ip netip.Addr) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.perIP[ip]
	if st == nil {
		return 0
	}
	for i, used := range st.slots {
		if !used {
			st.slots[i] = true
			return i
		}
	}
	st.slots = append(st.slots, true)
	return len(st.slots) - 1
}

func (s *Server) releaseSlot(ip netip.Addr, slot int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.perIP[ip]; st != nil && slot < len(st.slots) {
		st.slots[slot] = false
	}
}

// claimStation counts identity key against the address's station limit.
// replacing is the key the link is about to give up ("" for none); a
// station held only by this link does not count against the new one. It
// reports false when the address already has the maximum number of other
// stations.
func (s *Server) claimStation(ip netip.Addr, key, replacing string, pol *policy) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.perIP[ip]
	if st == nil {
		return ReasonStations
	}
	uuidKey := strings.HasPrefix(key, "uuid:")
	if h, ok := s.uuidAt[key]; uuidKey && ok && h.n > 0 && !sameClient(h.ip, ip) {
		return ReasonCloned
	}
	if st.stations[key] == 0 && pol.PerIPStations > 0 && !pol.allowed(ip) {
		n := len(st.stations)
		if replacing != "" && replacing != key && st.stations[replacing] == 1 {
			n--
		}
		if n >= pol.PerIPStations {
			return ReasonStations
		}
	}
	st.stations[key]++
	if uuidKey {
		h := s.uuidAt[key]
		h.ip, h.n = ip, h.n+1
		s.uuidAt[key] = h
	}
	return ""
}

// uuidHolder is the client a live UUID identity is attached from.
type uuidHolder struct {
	ip netip.Addr
	n  int // attached links
}

// sameClient: equal IPv4 addresses, or the same IPv6 /64.
func sameClient(a, b netip.Addr) bool {
	a, b = a.Unmap(), b.Unmap()
	if a.Is4() || b.Is4() {
		return a == b
	}
	p, _ := a.Prefix(64)
	return p.Contains(b)
}

func (s *Server) releaseStation(ip netip.Addr, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.perIP[ip]; st != nil {
		if st.stations[key]--; st.stations[key] <= 0 {
			delete(st.stations, key)
		}
	}
	if h, ok := s.uuidAt[key]; ok {
		if h.n--; h.n <= 0 {
			delete(s.uuidAt, key)
		} else {
			s.uuidAt[key] = h
		}
	}
}

// readerDone removes the link from the connection accounting (called when
// its reader goroutine exits).
func (s *Server) readerDone(l *link, wasUnattached bool) {
	s.mu.Lock()
	if st := s.perIP[l.ip]; st != nil {
		st.conns--
		if wasUnattached && st.unattached > 0 {
			st.unattached--
		}
		if st.conns <= 0 && len(st.stations) == 0 {
			delete(s.perIP, l.ip)
		}
	}
	n := subnetOf(l.ip)
	if s.perNet[n]--; s.perNet[n] <= 0 {
		delete(s.perNet, n)
	}
	s.mu.Unlock()
	s.readers.Done()
}

// linkGone is called after the final batch of l was delivered.
func (s *Server) linkGone(l *link) {
	s.mu.Lock()
	delete(s.links, l.id)
	if st := s.perIP[l.ip]; st != nil && st.conns <= 0 && len(st.stations) == 0 {
		delete(s.perIP, l.ip)
	}
	s.mu.Unlock()
	s.alive.Done()
}

// schedule puts l on the run queue (the caller set l.scheduled).
func (s *Server) schedule(l *link) {
	s.runq <- l
}

// Stats is a server-wide snapshot.
type Stats struct {
	Links        int `json:"links"`        // connected links
	Identified   int `json:"identified"`   // links with a UUID
	Unidentified int `json:"unidentified"` // attached without UUID
	Waiting      int `json:"waiting"`      // not yet attached
	Pending      int `json:"pending"`      // attached with admission pending
	Relays       int `json:"relays"`       // trusted relay links
	Stations     int `json:"stations"`     // attached stations (incl. relay sub-stations)

	Accepted uint64            `json:"accepted"`
	Rejected map[string]uint64 `json:"rejected,omitempty"` // by reason
	Dropped  map[string]uint64 `json:"disconnected,omitempty"`

	Bytes          uint64 `json:"bytes"`
	Frames         uint64 `json:"frames"`    // Mode S / A/C frames parsed
	Delivered      uint64 `json:"delivered"` // frames handed to the Sink
	MLATDropped    uint64 `json:"mlatDropped"`
	RateDropped    uint64 `json:"rateDropped"`
	QueueDropped   uint64 `json:"queueDropped"`
	BudgetDropped  uint64 `json:"budgetDropped"`
	UnknownEscapes uint64 `json:"unknownEscapes"`
	RelayIgnored   uint64 `json:"relayIgnored"` // 0xE3 from untrusted links
	SlowDowns      uint64 `json:"slowDowns"`    // 'W' 'S' sent
	PingsEchoed    uint64 `json:"pingsEchoed"`
}

// Stats returns the server-wide counters.
func (s *Server) Stats() Stats {
	st := Stats{
		Accepted:       s.ctr.accepted.Load(),
		Bytes:          s.ctr.bytes.Load(),
		Frames:         s.ctr.frames.Load(),
		Delivered:      s.ctr.delivered.Load(),
		MLATDropped:    s.ctr.mlatDropped.Load(),
		RateDropped:    s.ctr.rateDropped.Load(),
		QueueDropped:   s.ctr.queueDropped.Load(),
		BudgetDropped:  s.ctr.budgetDrp.Load(),
		UnknownEscapes: s.ctr.unknownEsc.Load(),
		RelayIgnored:   s.ctr.relayIgnored.Load(),
		SlowDowns:      s.ctr.slowDowns.Load(),
		PingsEchoed:    s.ctr.pings.Load(),
		Rejected:       map[string]uint64{},
		Dropped:        map[string]uint64{},
	}
	add := func(m map[string]uint64, k string, v uint64) {
		if v > 0 {
			m[k] = v
		}
	}
	add(st.Rejected, ReasonGate, s.ctr.rejGate.Load())
	add(st.Rejected, ReasonMaxLinks, s.ctr.rejMax.Load())
	add(st.Rejected, ReasonPerIP, s.ctr.rejIP.Load())
	add(st.Rejected, ReasonUnattached, s.ctr.rejUnid.Load())
	add(st.Rejected, ReasonSubnet, s.ctr.rejNet.Load())
	add(st.Rejected, ReasonStations, s.ctr.rejStations.Load())
	add(st.Rejected, ReasonCloned, s.ctr.rejCloned.Load())
	add(st.Rejected, ReasonCoolDown, s.ctr.rejCool.Load())
	add(st.Rejected, ReasonRegistry, s.ctr.rejRegistry.Load())
	add(st.Dropped, ReasonRate, s.ctr.discRate.Load())
	add(st.Dropped, ReasonIdle, s.ctr.discIdle.Load())
	add(st.Dropped, ReasonSilent, s.ctr.discSilent.Load())
	for _, ls := range s.Links() {
		st.Links++
		switch {
		case !ls.Attached:
			st.Waiting++
		case ls.UUID != "":
			st.Identified++
		default:
			st.Unidentified++
		}
		if ls.Pending {
			st.Pending++
		}
		if ls.Relay {
			st.Relays++
		}
		st.Stations += len(ls.Stations)
	}
	return st
}

// Links returns a snapshot of every connected link, ordered by link number.
// It contains private data (remote address, UUID): admin scope only.
func (s *Server) Links() []LinkStats {
	s.mu.Lock()
	ls := make([]*link, 0, len(s.links))
	for _, l := range s.links {
		ls = append(ls, l)
	}
	s.mu.Unlock()
	out := make([]LinkStats, 0, len(ls))
	for _, l := range ls {
		if st, ok := l.snapshot(); ok {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Link < out[j].Link })
	return out
}

// Link returns the snapshot of one link.
func (s *Server) Link(id uint64) (LinkStats, bool) {
	s.mu.Lock()
	l := s.links[id]
	s.mu.Unlock()
	if l == nil {
		return LinkStats{}, false
	}
	return l.snapshot()
}

// Disconnect closes a link (admin action); its identity is not cooled down.
func (s *Server) Disconnect(id uint64) bool {
	s.mu.Lock()
	l := s.links[id]
	s.mu.Unlock()
	if l == nil {
		return false
	}
	l.shutdown("disconnected by admin")
	return true
}
