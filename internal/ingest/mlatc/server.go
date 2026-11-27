package mlatc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// Disconnect / refusal reasons (Stats.Closed, LinkStats.Reason).
const (
	ReasonEOF        = "eof"
	ReasonIdle       = "idle"
	ReasonHandshake  = "handshake"
	ReasonDenied     = "denied"
	ReasonRateLimit  = "rate_limit"
	ReasonGarbage    = "garbage"
	ReasonWrite      = "write"
	ReasonShutdown   = "shutdown"
	ReasonReplaced   = "replaced"
	ReasonKicked     = "kicked"
	ReasonIngest     = "ingest"
	ReasonLimits     = "limits"
	ReasonProtocol   = "protocol"
	ReasonReadError  = "read_error"
	ReasonReset      = "reset"
	ReasonCompressed = "compression"
)

// Server is the mlat-client compat server (DESIGN §5.2). Create it with New
// and run Serve / ListenAndServe. All methods are safe for concurrent use.
type Server struct {
	o    Options
	reg  Registry
	sink Sink
	pol  atomic.Pointer[policy]

	mu        sync.Mutex
	ln        net.Listener
	port      uint16
	closing   bool
	nextID    uint64
	conns     map[uint64]*conn
	byStation map[model.StationIdx]*conn
	byKey     map[string]*conn
	ips       map[netip.Addr]*ipState
	nets      map[netip.Prefix]int
	cool      map[string]time.Time
	epochs    map[model.StationIdx]uint32
	noSplit   map[string]time.Time // identity key -> no split_sync until (SplitSyncAuto fallback)
	wg        sync.WaitGroup
	lastSweep time.Time

	// UDP transport (udp.go). udpPC, udpPort and udpErr are guarded by mu;
	// udpKeys (key -> client; nil = reserved during a handshake) by udpMu.
	udpPC   *net.UDPConn
	udpPort uint16
	udpErr  string
	udpMu   sync.RWMutex
	udpKeys map[uint32]*conn

	c counters
}

type ipState struct {
	conns, unauth int
	slots         map[int]bool // UUID-less slots in use
	stations      map[model.StationIdx]int
}

type counters struct {
	accepted, refused, handshakes, closed            atomic.Uint64
	lines, badLines, longLines, frames, syncs, mlats atomic.Uint64
	dropRate, dropDup, dropBad, results, resultsDrop atomic.Uint64
	epochs, wireBytes, ssyncs, splitFallbacks        atomic.Uint64
	udpPackets, udpBytes, udpUnknown, udpBadSrc      atomic.Uint64
	udpBad, udpLost, udpDup, udpLate, modeAC         atomic.Uint64
	byReason                                         sync.Map // reason -> *atomic.Uint64
}

func (c *counters) reason(r string) {
	v, _ := c.byReason.LoadOrStore(r, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

// New returns a Server delivering to sink and admitting through reg.
func New(o Options, reg Registry, sink Sink) (*Server, error) {
	if reg == nil || sink == nil {
		return nil, errors.New("mlatc: registry and sink are required")
	}
	o = o.normalized()
	if len(o.ServerKey) == 0 {
		o.ServerKey = make([]byte, 32)
		if _, err := rand.Read(o.ServerKey); err != nil {
			return nil, fmt.Errorf("mlatc: server key: %w", err)
		}
	}
	s := &Server{
		o: o, reg: reg, sink: sink,
		conns: map[uint64]*conn{}, byStation: map[model.StationIdx]*conn{}, byKey: map[string]*conn{},
		ips: map[netip.Addr]*ipState{}, nets: map[netip.Prefix]int{}, cool: map[string]time.Time{},
		epochs: map[model.StationIdx]uint32{}, noSplit: map[string]time.Time{}, udpKeys: map[uint32]*conn{},
	}
	s.pol.Store(compilePolicy(o.Policy))
	return s, nil
}

// SetPolicy replaces the limits; connected clients pick them up at once.
func (s *Server) SetPolicy(p Policy) { s.pol.Store(compilePolicy(p)) }

// Policy returns the policy in force (defaults filled in).
func (s *Server) Policy() Policy { return s.pol.Load().Policy }

func (s *Server) policy() *policy { return s.pol.Load() }

// ListenAndServe listens on Options.Addr and serves until ctx is cancelled
// or Close is called.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.o.Addr)
	if err != nil {
		return fmt.Errorf("mlatc: listen %s: %w", s.o.Addr, err)
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

// Serve accepts clients on ln until ctx is cancelled or Close is called; it
// returns nil after a regular shutdown (when every client has been
// detached). Serve may be called once.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	if s.ln != nil || s.closing {
		s.mu.Unlock()
		ln.Close()
		return errors.New("mlatc: server already started or closed")
	}
	s.ln = ln
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		s.port = uint16(ta.Port)
	}
	needUDP := s.o.UDP && s.udpPC == nil
	s.mu.Unlock()
	if needUDP {
		s.startUDP(ln)
	}
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	s.o.Logger.Info("mlat-client ingest listening", "addr", ln.Addr().String())
	var delay time.Duration
	for {
		nc, err := ln.Accept()
		if err != nil {
			if s.isClosing() {
				s.wg.Wait()
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				delay = min(max(delay*2, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			s.Close()
			return fmt.Errorf("mlatc: accept: %w", err)
		}
		delay = 0
		s.accept(nc)
	}
}

func (s *Server) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// Close stops accepting, disconnects every client and waits until each was
// detached. It is safe to call more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	first := !s.closing
	s.closing = true
	ln, pc := s.ln, s.udpPC
	all := make([]*conn, 0, len(s.conns))
	for _, c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()
	if first && ln != nil {
		ln.Close()
	}
	if first && pc != nil {
		pc.Close()
	}
	for _, c := range all {
		c.shutdown(ReasonShutdown)
	}
	s.wg.Wait()
	return nil
}

func subnetOf(ip netip.Addr) netip.Prefix {
	bits := 48
	if ip.Is4() {
		bits = 24
	}
	p, _ := ip.Prefix(bits)
	return p
}

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ipKey is the station key of a UUID-less client:
// "ip:" + base32(HMAC-SHA256(serverKey, ip16 ‖ listen port)[:10]).
func (s *Server) ipKey(ip netip.Addr) string {
	m := hmac.New(sha256.New, s.o.ServerKey)
	a := ip.Unmap().As16()
	m.Write(a[:])
	m.Write([]byte{byte(s.port >> 8), byte(s.port)})
	sum := m.Sum(nil)
	return "ip:" + strings.ToLower(keyEncoding.EncodeToString(sum[:model.IDBytes]))
}

// accept applies the connection limits and starts the client goroutine.
func (s *Server) accept(nc net.Conn) {
	ip := netip.Addr{}
	if ta, ok := nc.RemoteAddr().(*net.TCPAddr); ok {
		ip, _ = netip.AddrFromSlice(ta.IP)
	}
	ip = ip.Unmap()
	pol := s.policy()
	exempt := pol.allowed(ip)
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		nc.Close()
		return
	}
	st := s.ips[ip]
	if st == nil {
		st = &ipState{}
	}
	reason := ""
	switch {
	case len(s.conns) >= pol.MaxClients:
		reason = denyBusy
	case !exempt && st.conns >= pol.MaxConnsPerIP:
		reason = denyLimit
	case !exempt && st.unauth >= pol.MaxUnauthPerIP:
		reason = denyLimit
	case !exempt && s.nets[subnetOf(ip)] >= pol.MaxConnsPerSubnet:
		reason = denyLimit
	}
	if reason != "" {
		s.mu.Unlock()
		s.c.refused.Add(1)
		s.c.reason(ReasonLimits)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.denyRaw(nc, reason)
		}()
		return
	}
	s.ips[ip] = st
	st.conns++
	st.unauth++
	s.nets[subnetOf(ip)]++
	s.nextID++
	c := newConn(s, nc, s.nextID, ip)
	s.conns[c.id] = c
	s.wg.Add(1)
	s.mu.Unlock()
	s.c.accepted.Add(1)
	go c.run()
}

// denyRaw answers a connection refused by the address limits. It reads
// the handshake first (briefly), so the refusal is not lost to a reset of
// a connection closed with unread data.
func (s *Server) denyRaw(nc net.Conn, reason string) {
	nc.SetDeadline(time.Now().Add(2 * time.Second))
	newLineReader(io.LimitReader(nc, maxLine+1)).readLine()
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	nc.Write(marshalLine(denyReply{Deny: []string{reason}, ReconnectIn: int(s.o.ReconnectIn / time.Second)}))
	nc.Close()
}

// coolingDown reports whether key is in a cool-down.
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

func (s *Server) coolDown(key string, d time.Duration, now time.Time) {
	if d <= 0 || key == "" {
		return
	}
	s.mu.Lock()
	s.cool[key] = now.Add(d)
	if now.Sub(s.lastSweep) > time.Minute {
		s.lastSweep = now
		for k, t := range s.cool {
			if now.After(t) {
				delete(s.cool, k)
			}
		}
	}
	s.mu.Unlock()
}

// takeSlot gives a UUID-less client from ip the lowest free slot.
func (s *Server) takeSlot(ip netip.Addr) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.ips[ip]
	if st.slots == nil {
		st.slots = map[int]bool{}
	}
	for i := 0; ; i++ {
		if !st.slots[i] {
			st.slots[i] = true
			return i
		}
	}
}

// claimKey registers c as the live client of key. A client already live
// under the key is replaced when it has been silent for replaceAfter
// (a half-open connection of the same feeder); otherwise the newcomer is
// refused (DESIGN SEC-04: a clone must not kick the real feeder).
const replaceAfter = 30 * time.Second

func (s *Server) claimKey(key string, c *conn, now time.Time) (old *conn, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.byKey[key]; o != nil && o != c {
		if now.Sub(o.lastRead()) < replaceAfter {
			return nil, false
		}
		old = o
	}
	s.byKey[key] = c
	return old, true
}

// bindStation records c as the client of st and returns the next epoch.
func (s *Server) bindStation(st model.StationIdx, c *conn, base uint32) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byStation[st] = c
	c.boundSt = st
	e := max(s.epochs[st], base)
	if ip := s.ips[c.ip]; ip != nil {
		if ip.stations == nil {
			ip.stations = map[model.StationIdx]int{}
		}
		ip.stations[st]++
	}
	return e
}

// stationsOf returns the number of stations with a live client from ip,
// not counting the station of a live client under key (a reconnect of the
// same feeder is not an additional station).
func (s *Server) stationsOf(ip netip.Addr, key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.ips[ip]
	if st == nil {
		return 0
	}
	n := len(st.stations)
	if o := s.byKey[key]; o != nil && o.ip == ip && o.boundSt != 0 && st.stations[o.boundSt] > 0 {
		n--
	}
	return n
}

func (s *Server) setEpoch(st model.StationIdx, e uint32) {
	s.mu.Lock()
	if e > s.epochs[st] {
		s.epochs[st] = e
	}
	s.mu.Unlock()
}

// authed moves c from "unauthenticated" to "attached" for the IP limits.
func (s *Server) authed(c *conn) {
	s.mu.Lock()
	if st := s.ips[c.ip]; st != nil && st.unauth > 0 {
		st.unauth--
	}
	s.mu.Unlock()
}

// remove forgets a finished client.
func (s *Server) remove(c *conn) {
	s.mu.Lock()
	delete(s.conns, c.id)
	if c.key != "" && s.byKey[c.key] == c {
		delete(s.byKey, c.key)
	}
	if c.att.Station != 0 && s.byStation[c.att.Station] == c {
		delete(s.byStation, c.att.Station)
	}
	if st := s.ips[c.ip]; st != nil {
		st.conns--
		if !c.authedFlag.Load() {
			st.unauth--
		}
		if c.att.Station != 0 && st.stations != nil {
			if st.stations[c.att.Station]--; st.stations[c.att.Station] <= 0 {
				delete(st.stations, c.att.Station)
			}
		}
		if c.hasSlot && st.slots != nil {
			delete(st.slots, c.ident.Slot)
		}
		if st.conns <= 0 {
			delete(s.ips, c.ip)
		}
	}
	n := subnetOf(c.ip)
	if s.nets[n]--; s.nets[n] <= 0 {
		delete(s.nets, n)
	}
	s.mu.Unlock()
}

func (s *Server) connOf(st model.StationIdx) *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byStation[st]
}

// SendResult queues one result for the client of station st, in the format
// the client asked for. It returns false when the station has no client,
// the client did not ask for results, results are switched off, or its
// output queue is full. The engine calls it once per contributing station
// (DESIGN §8.4 contribution rule): results never go to other clients.
func (s *Server) SendResult(st model.StationIdx, r Result) bool {
	if s.policy().DisableResults {
		return false
	}
	c := s.connOf(st)
	if c == nil {
		return false
	}
	return c.sendResult(&r)
}

// SendResults is SendResult for plain fixes (the same signature as
// avfsrv.Server.SendResults, so the engine routes both alike). It returns
// true when every fix was queued.
func (s *Server) SendResults(st model.StationIdx, fixes []model.MLATFix) bool {
	if s.policy().DisableResults {
		return false
	}
	c := s.connOf(st)
	if c == nil {
		return false
	}
	ok := true
	for i := range fixes {
		r := Result{MLATFix: fixes[i]}
		ok = c.sendResult(&r) && ok
	}
	return ok
}

// StartSending asks the client of st to send the given aircraft, on top
// of (and overriding) the server's own selection. keys are tracker keys:
// non-ICAO keys (bit 24) cannot be requested and are ignored. It
// implements plan.SendingController (internal/mlat/plan), so the MLAT
// planner can drive mlat-client stations with plan.MLATCOut{C: srv}.
func (s *Server) StartSending(st model.StationIdx, keys []uint32) { s.force(st, keys, true) }

// StopSending asks the client of st to stop sending the given aircraft
// (overriding the server's own selection).
func (s *Server) StopSending(st model.StationIdx, keys []uint32) { s.force(st, keys, false) }

func (s *Server) force(st model.StationIdx, keys []uint32, on bool) {
	c := s.connOf(st)
	if c == nil || c.sel == nil {
		return
	}
	c.sel.mu.Lock()
	for _, a := range keys {
		if a <= 0xffffff {
			c.sel.force(a, on)
		}
	}
	c.sel.mu.Unlock()
}

// Disconnect closes the client of station st (admin kick, revocation). The
// identity is cooled down for coolDown (0 = none).
func (s *Server) Disconnect(st model.StationIdx, coolDown time.Duration) bool {
	c := s.connOf(st)
	if c == nil {
		return false
	}
	if coolDown > 0 {
		s.coolDown(c.key, coolDown, s.o.Now())
	}
	c.shutdown(ReasonKicked)
	return true
}

// Online reports whether station st has a connected client.
func (s *Server) Online(st model.StationIdx) bool { return s.connOf(st) != nil }

// Stats are server totals.
type Stats struct {
	Clients, Attached                     int
	Accepted, Refused, Handshakes, Closed uint64
	Lines, BadLines, LongLines            uint64
	Frames, Syncs, MLATs                  uint64
	DroppedRate, DroppedDup, DroppedBad   uint64
	Results, ResultsDropped               uint64
	Epochs, WireBytes                     uint64
	// SSyncs counts valid single-sync messages; SplitFallbacks the
	// identities switched back to sync pairs (SplitSyncAuto).
	SSyncs, SplitFallbacks uint64
	// UDP transport: datagrams and bytes received, and the datagrams
	// dropped as unattributable (too short / unknown key / wrong source),
	// malformed or duplicate; UDPLost estimates datagrams lost in transit
	// from sequence gaps, UDPLate counts out-of-order arrivals.
	UDPPackets, UDPBytes                uint64
	UDPUnknownKey, UDPBadSource, UDPBad uint64
	UDPDup, UDPLost, UDPLate            uint64
	// UDPActive: the datagram socket is open; UDPError: why it is not
	// (Options.UDP set but the bind failed).
	UDPActive bool
	UDPError  string
	// ModeACFrames counts Mode A/C replies received (both transports).
	ModeACFrames uint64
	Reasons      map[string]uint64
}

// Stats returns the server totals.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	st := Stats{Clients: len(s.conns), Attached: len(s.byStation)}
	s.mu.Unlock()
	c := &s.c
	st.Accepted, st.Refused, st.Handshakes, st.Closed = c.accepted.Load(), c.refused.Load(), c.handshakes.Load(), c.closed.Load()
	st.Lines, st.BadLines, st.LongLines = c.lines.Load(), c.badLines.Load(), c.longLines.Load()
	st.Frames, st.Syncs, st.MLATs = c.frames.Load(), c.syncs.Load(), c.mlats.Load()
	st.DroppedRate, st.DroppedDup, st.DroppedBad = c.dropRate.Load(), c.dropDup.Load(), c.dropBad.Load()
	st.Results, st.ResultsDropped = c.results.Load(), c.resultsDrop.Load()
	st.Epochs, st.WireBytes = c.epochs.Load(), c.wireBytes.Load()
	st.SSyncs, st.SplitFallbacks = c.ssyncs.Load(), c.splitFallbacks.Load()
	st.UDPPackets, st.UDPBytes = c.udpPackets.Load(), c.udpBytes.Load()
	st.UDPUnknownKey, st.UDPBadSource, st.UDPBad = c.udpUnknown.Load(), c.udpBadSrc.Load(), c.udpBad.Load()
	st.UDPDup, st.UDPLost, st.UDPLate = c.udpDup.Load(), c.udpLost.Load(), c.udpLate.Load()
	st.UDPActive, st.UDPError = s.UDPStatus()
	st.ModeACFrames = c.modeAC.Load()
	st.Reasons = map[string]uint64{}
	c.byReason.Range(func(k, v any) bool {
		st.Reasons[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	return st
}

// LinkStats describes one client. It holds the remote address and UUID:
// admin scope only.
type LinkStats struct {
	ID            uint64
	Station       model.StationIdx
	FeedID        string
	Admission     string
	UUID          string
	User          string
	ClientVersion string
	ClockType     string
	ClockClass    string
	Compression   string
	Selective     bool
	Mode          string // traffic selection: "full" or "selective"
	ReturnResults bool
	ResultFormat  string // "ecef" or "legacy"
	Privacy       bool
	Located       bool
	IP            netip.Addr
	Since         time.Time
	LastRead      time.Time
	Epoch         uint32
	Lines, Frames uint64
	Syncs, MLATs  uint64
	Seen          int
	Requested     int
	MsgRate       float64 // frames/s over the last second
	KBps          float64 // received kB/s over the last second
	// Results counts the MLAT results queued to this connection (the owner's
	// "results received"); ResultsDrop those dropped on a full queue.
	Results     uint64
	ResultsDrop uint64
	BadLines    uint64
	// SplitSync: split_sync was offered in the handshake reply; SSyncs
	// counts the valid single-sync messages received.
	SplitSync bool
	SSyncs    uint64
	// ADSBAircraft / ADSBRate summarise the client's last rate_report: the
	// aircraft with ADS-B positions it hears and their summed position
	// message rate (msgs/s).
	ADSBAircraft int
	ADSBRate     float64
	// UDP: the client sends its timed frames as datagrams (udp_transport
	// accepted); UDPPackets / UDPLost (sequence gaps) / UDPDup / UDPBad
	// count its datagrams.
	UDP                                 bool
	UDPPackets, UDPLost, UDPDup, UDPBad uint64
	// ModeAC: the client receives Mode A/C requests (MLATC.md §10);
	// ModeACFrames counts the Mode A/C replies it sent.
	ModeAC       bool
	ModeACFrames uint64
}

// Links returns every attached client.
func (s *Server) Links() []LinkStats {
	s.mu.Lock()
	all := make([]*conn, 0, len(s.conns))
	for _, c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()
	out := make([]LinkStats, 0, len(all))
	for _, c := range all {
		if l, ok := c.stats(); ok {
			out = append(out, l)
		}
	}
	return out
}
