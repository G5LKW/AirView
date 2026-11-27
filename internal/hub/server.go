package hub

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// Options configures a Server. Zero values select the documented defaults.
type Options struct {
	Port     int    // TCP port; 0 = DefaultPort (30154); -1 = ephemeral port (tests)
	BindAddr string // "" = all interfaces
	Password string // shared secret; "" = open access

	MaxClients        int           // simultaneous logged-in clients, default 8
	LoginTimeout      time.Duration // whole handshake, default 10 s
	HeartbeatInterval time.Duration // default 5 s
	WriteTimeout      time.Duration // a single socket write, default 10 s
	// ClientBuffer is the per-client backlog limit in bytes (default 4 MiB).
	// A client whose backlog exceeds it is disconnected.
	ClientBuffer int
	// FlushInterval is how long the per-client writer waits to coalesce
	// records into one socket write (default 10 ms; negative = no delay).
	FlushInterval time.Duration
	// FailLimit is the number of failed logins from one IP address after
	// which further attempts are refused for a growing period (1 s doubling
	// up to 60 s). Default 5.
	FailLimit int

	// AdminOnly selects aggregator-mode behaviour (DESIGN §5.4, see
	// quiet.go): logins always need the password (open access is never
	// offered), and peers that do not speak the hub protocol are closed
	// without a reply and logged at most once per IP per QuietLogInterval.
	// Standalone stations leave it false. It can be changed at run time
	// with SetAdminOnly.
	AdminOnly bool
	// ProbeTimeout is, in admin-only mode, how long after the connect a
	// complete AUTH line must have arrived (default 2 s, capped at
	// LoginTimeout).
	ProbeTimeout time.Duration
	// QuietLogInterval is the minimum time between two log lines about
	// quietly closed connections from the same IP address (default 1 h).
	QuietLogInterval time.Duration

	Logger *slog.Logger // default slog.Default()
}

func (o *Options) setDefaults() {
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	if o.MaxClients <= 0 {
		o.MaxClients = 8
	}
	if o.LoginTimeout <= 0 {
		o.LoginTimeout = 10 * time.Second
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 5 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
	if o.ClientBuffer <= 0 {
		o.ClientBuffer = 4 << 20
	}
	if o.FlushInterval == 0 {
		o.FlushInterval = 10 * time.Millisecond
	}
	if o.FailLimit <= 0 {
		o.FailLimit = 5
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 2 * time.Second
	}
	if o.ProbeTimeout > o.LoginTimeout {
		o.ProbeTimeout = o.LoginTimeout
	}
	if o.QuietLogInterval <= 0 {
		o.QuietLogInterval = time.Hour
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// ClientInfo describes one logged-in client (Server.Clients).
type ClientInfo struct {
	Addr      string `json:"addr"`
	Since     int64  `json:"since"`     // Unix ms of login
	BytesSent uint64 `json:"bytesSent"` // bytes written to the socket
	Backlog   int    `json:"backlog"`   // bytes waiting in the client's buffer
}

// Server is the data port server. Create it with NewServer, call Start, feed
// it with Broadcast / BroadcastUpdate (or use it as a model.FrameSink), and
// stop it with Close or by cancelling the context given to Start.
//
// All methods are safe for concurrent use.
type Server struct {
	opts Options
	log  *slog.Logger

	nClients atomic.Int32 // fast path for Broadcast when nobody listens

	mu          sync.Mutex
	ln          net.Listener
	port        int
	running     bool
	closed      bool
	cancel      context.CancelFunc
	password    string
	clients     map[*srvClient]struct{}
	handshaking map[net.Conn]struct{}
	station     Station
	stationRec  []byte // encoded Station record (nil until SetStation)
	feedIdx     map[string]byte
	feedDefs    []byte // all FeedDef records, concatenated
	scratch     []byte // record encode buffer
	hbSeq       uint32
	fails       map[string]*failEntry
	adminOnly   bool                   // aggregator mode (quiet.go)
	quiet       map[string]*quietEntry // per-IP log limiter for quiet closes

	wg sync.WaitGroup

	dropped      atomic.Uint64 // clients disconnected for being too slow
	quietRejects atomic.Uint64 // connections closed quietly (admin-only mode)
}

type failEntry struct {
	count int
	last  time.Time
	until time.Time
}

// NewServer returns a server that is not yet listening.
func NewServer(opts Options) *Server {
	opts.setDefaults()
	s := &Server{
		opts:        opts,
		log:         opts.Logger.With("component", "hub"),
		password:    opts.Password,
		clients:     make(map[*srvClient]struct{}),
		handshaking: make(map[net.Conn]struct{}),
		feedIdx:     make(map[string]byte),
		fails:       make(map[string]*failEntry),
		adminOnly:   opts.AdminOnly,
		quiet:       make(map[string]*quietEntry),
	}
	s.setStationLocked(Station{})
	return s
}

// Start binds the listening socket and serves clients in the background
// until ctx is cancelled or Close is called. It returns an error when the
// port cannot be bound. A server can be started only once.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running || s.closed {
		s.mu.Unlock()
		return errors.New("hub: server already started")
	}
	port := s.opts.Port
	if port < 0 {
		port = 0
	}
	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(s.opts.BindAddr, strconv.Itoa(port)))
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("hub: listen: %w", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	s.ln = ln
	s.cancel = cancel
	s.running = true
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		s.port = ta.Port
	}
	s.wg.Add(3)
	s.mu.Unlock()

	s.log.Info("data port listening", "addr", ln.Addr().String(), "auth", s.authMode(), "adminOnly", s.AdminOnly())
	go s.acceptLoop(cctx, ln)
	go s.heartbeatLoop(cctx)
	go func() {
		defer s.wg.Done()
		<-cctx.Done()
		s.shutdown()
	}()
	return nil
}

// Close stops the server, disconnects all clients and waits for the
// server's goroutines to finish. It is safe to call more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	s.closed = true
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	return nil
}

// shutdown runs once the server context is cancelled.
func (s *Server) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.running = false
	if s.ln != nil {
		s.ln.Close()
	}
	for c := range s.clients {
		s.removeLocked(c, "server shutting down")
	}
	for conn := range s.handshaking {
		conn.Close()
	}
	s.mu.Unlock()
}

// Addr returns the listening address, or nil before Start.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Status reports the data port state for /api/status.
func (s *Server) Status() model.HubStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return model.HubStatus{Enabled: s.running, Port: s.port, Clients: len(s.clients)}
}

// Clients lists the logged-in clients.
func (s *Server) Clients() []ClientInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ClientInfo, 0, len(s.clients))
	for c := range s.clients {
		c.mu.Lock()
		backlog := len(c.pending)
		c.mu.Unlock()
		out = append(out, ClientInfo{Addr: c.addr, Since: c.since.UnixMilli(), BytesSent: c.sent.Load(), Backlog: backlog})
	}
	return out
}

// DroppedClients returns how many clients were disconnected because they
// could not keep up with the stream.
func (s *Server) DroppedClients() uint64 { return s.dropped.Load() }

// SetPassword changes the login password. Clients that are currently
// logged in are disconnected when the password actually changed (they
// reconnect and must present the new one).
func (s *Server) SetPassword(password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if password == s.password {
		return
	}
	s.password = password
	for c := range s.clients {
		s.removeLocked(c, "password changed")
	}
}

// SetStation publishes the station metadata. It is sent to every client at
// login and re-sent to all clients whenever it changes.
func (s *Server) SetStation(rx model.Receiver, antennas []model.Antenna, feeds []StationFeed) {
	st := Station{Receiver: rx, Antennas: antennas, Feeds: feeds}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.stationRec
	s.setStationLocked(st)
	if bytes.Equal(old, s.stationRec) {
		return
	}
	s.fanoutLocked(s.stationRec)
}

func (s *Server) setStationLocked(st Station) {
	st.App = model.AppName
	st.Version = model.Version
	st.Proto = ProtocolVersion
	if st.Antennas == nil {
		st.Antennas = []model.Antenna{}
	}
	rec, ok := encodeStation(st)
	if !ok {
		// Far too large for one record: drop the free-text notes, which are
		// the only unbounded part.
		ants := make([]model.Antenna, len(st.Antennas))
		copy(ants, st.Antennas)
		for i := range ants {
			ants[i].Notes = ""
		}
		st.Antennas = ants
		if rec, ok = encodeStation(st); !ok {
			s.log.Error("station metadata too large for the data port; not published")
			return
		}
	}
	s.station = st
	s.stationRec = rec
}

func encodeStation(st Station) ([]byte, bool) {
	js, err := json.Marshal(st)
	if err != nil {
		return nil, false
	}
	rec, start := beginRecord(make([]byte, 0, len(js)+recHeader), recStation)
	rec = append(rec, js...)
	return endRecord(rec, start)
}

// Broadcast relays one raw frame to all logged-in clients. It never blocks
// on a client and does not retain f.Data.
func (s *Server) Broadcast(f model.RawFrame) {
	if s.nClients.Load() == 0 {
		return
	}
	s.mu.Lock()
	idx := s.feedIndexLocked(f.FeedID)
	s.scratch = appendFrame(s.scratch[:0], &f, idx)
	s.fanoutLocked(s.scratch)
	s.mu.Unlock()
}

// BroadcastUpdate relays one pre-decoded update to all logged-in clients.
// It never blocks on a client.
func (s *Server) BroadcastUpdate(u model.DecodedUpdate) {
	if s.nClients.Load() == 0 {
		return
	}
	s.mu.Lock()
	idx := s.feedIndexLocked(u.FeedID)
	s.scratch = appendUpdate(s.scratch[:0], &u, idx)
	s.fanoutLocked(s.scratch)
	s.mu.Unlock()
}

// HandleFrame implements model.FrameSink (same as Broadcast).
func (s *Server) HandleFrame(f model.RawFrame) { s.Broadcast(f) }

// HandleUpdate implements model.FrameSink (same as BroadcastUpdate).
func (s *Server) HandleUpdate(u model.DecodedUpdate) { s.BroadcastUpdate(u) }

// feedIndexLocked returns the wire index of a feed ID, defining it for all
// clients on first use.
func (s *Server) feedIndexLocked(id string) byte {
	if idx, ok := s.feedIdx[id]; ok {
		return idx
	}
	if id == "" || len(id) > 200 || len(s.feedIdx) >= unknownFeed {
		return unknownFeed
	}
	idx := byte(len(s.feedIdx))
	s.feedIdx[id] = idx
	before := len(s.feedDefs)
	s.feedDefs = appendFeedDef(s.feedDefs, idx, id)
	s.fanoutLocked(s.feedDefs[before:])
	return idx
}

// fanoutLocked queues rec for every client and disconnects those whose
// buffer overflows.
func (s *Server) fanoutLocked(rec []byte) {
	for c := range s.clients {
		if !c.enqueue(rec, s.opts.ClientBuffer) {
			s.dropped.Add(1)
			s.removeLocked(c, "client too slow (buffer overflow)")
		}
	}
}

// removeLocked disconnects a client. Idempotent.
func (s *Server) removeLocked(c *srvClient, reason string) {
	if _, ok := s.clients[c]; !ok {
		return
	}
	delete(s.clients, c)
	s.nClients.Store(int32(len(s.clients)))
	c.mu.Lock()
	c.closed = true
	c.pending = nil
	c.spare = nil
	c.mu.Unlock()
	close(c.done)
	c.conn.Close()
	s.log.Info("data port client disconnected", append(peerArgsFor(s.adminOnly, c.addr), "reason", reason)...)
}

func (s *Server) remove(c *srvClient, reason string) {
	s.mu.Lock()
	s.removeLocked(c, reason)
	s.mu.Unlock()
}

func (s *Server) authMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.password == "" && !s.adminOnly {
		return authOpen
	}
	return authHMAC
}

func (s *Server) acceptLoop(ctx context.Context, ln net.Listener) {
	defer s.wg.Done()
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient accept failure (e.g. out of file descriptors).
			if delay == 0 {
				delay = 10 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			s.log.Warn("data port accept failed", "err", err)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
			continue
		}
		delay = 0

		s.mu.Lock()
		if s.closed || len(s.handshaking) >= s.opts.MaxClients+16 {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.handshaking[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *Server) heartbeatLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.mu.Lock()
			s.hbSeq++
			s.scratch = appendHeartbeat(s.scratch[:0], now, s.hbSeq)
			s.fanoutLocked(s.scratch)
			s.pruneFailsLocked(now)
			s.pruneQuietLocked(now)
			s.mu.Unlock()
		}
	}
}

// --- login throttling -------------------------------------------------------

const (
	failForget  = 10 * time.Minute
	maxFailKeys = 4096
)

func (s *Server) pruneFailsLocked(now time.Time) {
	for ip, e := range s.fails {
		if now.Sub(e.last) > failForget {
			delete(s.fails, ip)
		}
	}
}

func (s *Server) throttled(ip string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.fails[ip]
	return e != nil && now.Before(e.until)
}

func (s *Server) recordFail(ip string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.fails[ip]
	if e == nil {
		if len(s.fails) >= maxFailKeys {
			s.pruneFailsLocked(now)
			for k := range s.fails { // still full: evict an arbitrary entry
				if len(s.fails) < maxFailKeys {
					break
				}
				delete(s.fails, k)
			}
		}
		e = &failEntry{}
		s.fails[ip] = e
	} else if now.Sub(e.last) > failForget {
		e.count = 0
	}
	e.count++
	e.last = now
	if over := e.count - s.opts.FailLimit; over >= 0 {
		lock := 60 * time.Second
		if over < 6 {
			lock = time.Second << uint(over)
		}
		e.until = now.Add(lock)
	}
}

func (s *Server) clearFails(ip string) {
	s.mu.Lock()
	delete(s.fails, ip)
	s.mu.Unlock()
}

// --- per-connection handling -------------------------------------------------

// maxSpare is the largest write buffer a client keeps for reuse.
const maxSpare = 256 << 10

// srvClient is one logged-in client.
type srvClient struct {
	conn  net.Conn
	addr  string
	since time.Time
	wake  chan struct{} // capacity 1: "pending is non-empty"
	done  chan struct{} // closed when the client is removed
	sent  atomic.Uint64

	mu      sync.Mutex
	pending []byte // records waiting to be written
	spare   []byte // recycled buffer for the next pending
	closed  bool
}

// enqueue appends rec to the client's backlog. It reports false when the
// backlog limit would be exceeded (the caller then drops the client).
func (c *srvClient) enqueue(rec []byte, limit int) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return true // already being removed
	}
	if len(c.pending)+len(rec) > limit {
		c.mu.Unlock()
		return false
	}
	c.pending = append(c.pending, rec...)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return true
}

func writeLine(conn net.Conn, line string) error {
	_, err := io.WriteString(conn, line+"\n")
	return err
}

func remoteIP(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

// serve runs the handshake and then the client's lifetime.
func (s *Server) serve(conn net.Conn) {
	defer s.wg.Done()
	c := s.login(conn)

	s.mu.Lock()
	delete(s.handshaking, conn)
	s.mu.Unlock()
	if c == nil {
		conn.Close()
		return
	}

	s.wg.Add(1)
	go s.writer(c)

	// The client sends nothing after login; reading detects a closed or
	// reset connection promptly. Stray input is discarded.
	_, err := io.Copy(io.Discard, conn)
	reason := "connection closed by client"
	if err != nil {
		reason = err.Error()
	}
	s.remove(c, reason)
}

// login performs the handshake. On success the client is registered (with
// the initial heartbeat, station and feed definitions already queued) and
// returned; on failure nil is returned and the caller closes the connection.
func (s *Server) login(conn net.Conn) *srvClient {
	now := time.Now()
	ip := remoteIP(conn)
	addr := conn.RemoteAddr().String()
	conn.SetDeadline(now.Add(s.opts.LoginTimeout))

	if s.throttled(ip, now) {
		writeLine(conn, "ERR throttled too many failed logins, try again later")
		return nil
	}

	var nonce [nonceLen]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		s.log.Error("data port: no randomness for login nonce", "err", err)
		return nil
	}
	s.mu.Lock()
	password := s.password
	adminOnly := s.adminOnly
	s.mu.Unlock()
	mode := authHMAC
	if password == "" && !adminOnly {
		mode = authOpen
	}
	if err := writeLine(conn, fmt.Sprintf("%s %d %s %s", greetingWord, ProtocolVersion, mode, hex.EncodeToString(nonce[:]))); err != nil {
		return nil
	}

	br := bufio.NewReaderSize(conn, maxLine+2)
	if adminOnly {
		// Admin-only mode: the AUTH line must be complete within the probe
		// window; a Beast stream is recognised by its first byte. Anything
		// that is not the hub protocol is closed without a reply.
		conn.SetReadDeadline(now.Add(s.opts.ProbeTimeout))
		first, err := br.Peek(1)
		if err != nil {
			s.quietReject(ip, ip, addr, probeReason(err), time.Now())
			return nil
		}
		if first[0] == beastEscape {
			s.quietReject(ip, ip, addr, quietBeast, time.Now())
			return nil
		}
	}
	line, err := readLine(br)
	if err != nil {
		if adminOnly {
			s.quietReject(ip, ip, addr, probeReason(err), time.Now())
			return nil
		}
		if !errors.Is(err, io.EOF) {
			s.log.Debug("data port handshake failed", append(s.peerArgs(addr), "err", err)...)
		}
		if errors.Is(err, ErrProtocol) {
			writeLine(conn, "ERR proto malformed handshake")
		}
		return nil
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != "AUTH" {
		if adminOnly {
			s.quietReject(ip, ip, addr, quietNotHub, time.Now())
			return nil
		}
		writeLine(conn, "ERR proto expected AUTH <client-nonce> <proof>")
		return nil
	}
	clientNonce, err1 := hex.DecodeString(parts[1])
	proof, err2 := hex.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(clientNonce) != nonceLen {
		if adminOnly {
			s.quietReject(ip, ip, addr, quietNotHub, time.Now())
			return nil
		}
		writeLine(conn, "ERR proto malformed AUTH line")
		return nil
	}
	if adminOnly {
		// A hub client: the rest of the handshake runs under the normal
		// login timeout again.
		conn.SetReadDeadline(now.Add(s.opts.LoginTimeout))
		if password == "" {
			// Open access is not allowed on an aggregator. Tell the (genuine)
			// client why; log at most once per IP per interval.
			s.quietReject("nopw|"+ip, ip, addr, quietNoPasswd, time.Now())
			writeLine(conn, "ERR auth no data port password is set; aggregator mode requires one")
			return nil
		}
	}
	if password != "" && !hmac.Equal(proof, clientProof(password, nonce[:])) {
		s.recordFail(ip, time.Now())
		s.log.Warn("data port login rejected (wrong password)", s.peerArgs(addr)...)
		writeLine(conn, "ERR auth wrong password")
		return nil
	}
	s.clearFails(ip)

	c := &srvClient{
		conn:  conn,
		addr:  addr,
		since: time.Now(),
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}

	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil
	case s.password != password || (s.adminOnly && password == ""):
		// The password (or the admin-only setting) changed while this
		// client was logging in.
		s.mu.Unlock()
		writeLine(conn, "ERR auth password changed, log in again")
		return nil
	case len(s.clients) >= s.opts.MaxClients:
		s.mu.Unlock()
		writeLine(conn, "ERR busy client limit reached")
		return nil
	}
	// Queue the initial records while holding the lock so that nothing
	// broadcast later can overtake them. The writer is not running yet, so
	// the "OK" line below still reaches the socket first.
	s.hbSeq++
	c.pending = appendHeartbeat(make([]byte, 0, 4096), c.since, s.hbSeq)
	c.pending = append(c.pending, s.stationRec...)
	c.pending = append(c.pending, s.feedDefs...)
	c.wake <- struct{}{}
	s.clients[c] = struct{}{}
	s.nClients.Store(int32(len(s.clients)))
	s.mu.Unlock()

	if err := writeLine(conn, "OK "+hex.EncodeToString(serverProof(password, nonce[:], clientNonce))); err != nil {
		s.remove(c, "handshake write failed: "+err.Error())
		return nil
	}
	conn.SetDeadline(time.Time{})
	s.log.Info("data port client logged in", s.peerArgs(addr)...)
	return c
}

// writer drains the client's backlog to its socket.
func (s *Server) writer(c *srvClient) {
	defer s.wg.Done()
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
		}
		if s.opts.FlushInterval > 0 {
			// Let a burst accumulate: one write per interval instead of one
			// per record.
			select {
			case <-c.done:
				return
			case <-time.After(s.opts.FlushInterval):
			}
		}
		for {
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			buf := c.pending
			if len(buf) == 0 {
				c.mu.Unlock()
				break
			}
			c.pending = c.spare[:0]
			c.spare = nil
			c.mu.Unlock()

			c.conn.SetWriteDeadline(time.Now().Add(s.opts.WriteTimeout))
			n, err := c.conn.Write(buf)
			c.sent.Add(uint64(n))
			if err != nil {
				reason := err.Error()
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					s.dropped.Add(1)
					reason = "client too slow (write timeout)"
				}
				s.remove(c, reason)
				return
			}
			c.mu.Lock()
			// Recycle the buffer, unless a past backlog made it large: a
			// client that fell behind once must not pin megabytes for good.
			if !c.closed && cap(buf) <= maxSpare {
				c.spare = buf[:0]
			}
			c.mu.Unlock()
		}
	}
}
