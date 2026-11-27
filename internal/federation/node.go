package federation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/avf"
	mgeo "airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/privacy"
	"airview/internal/privacy/publicdto"
)

// Options are the run-time inputs of a Node besides its Config.
type Options struct {
	// Identity is the node's Ed25519 key (required). Its FeederID is the
	// node ID peers configure.
	Identity avf.Identity
	// Certificate serves the listener's TLS certificate. nil = an
	// in-memory self-signed certificate (fine: peers authenticate the node
	// by its identity, see ClientTLSConfig).
	Certificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// Roots verifies peers whose pin is "webpki" (nil = system roots).
	Roots *x509.CertPool
	// Source returns the local merged picture, unprojected (the tracker's
	// Snapshot). It is called once per export interval. nil = nothing of
	// our own is shared.
	Source func() model.LiveSnapshot
	// Guard is the island guard (DESIGN §8.6) of the public map; nil when
	// the guard is off.
	Guard publicdto.Guard
	// Sink, when set, receives remote aircraft and MLAT results as
	// model.DecodedUpdate with FeedID FeedPrefix+<peer node ID>.
	Sink model.FrameSink
	// Version is the software version sent to peers.
	Version string
	// Logf logs (nil = silent). Lines never carry feeder data.
	Logf func(format string, args ...any)
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// Node is one network's federation service: listener, peer links,
// exporter, remote store and outbound feeds.
type Node struct {
	cfg  Config
	opts Options
	id   string
	hc   HandshakeConfig

	srvTLS *tls.Config
	store  *store
	peers  map[string]*peer
	order  []*peer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	ln     net.Listener
	lnMu   sync.Mutex

	started atomic.Bool
	closed  atomic.Bool
	startMs int64

	// The latest local picture (export goroutine writes; feeds read).
	prepMu    sync.RWMutex
	prep      []local
	prepNowMs int64

	mlatMu      sync.Mutex
	mlatLocal   []Fix
	mlatTransit []transitFix
	remoteFix   map[uint32]remoteFix

	feeds []*outFeed
	stats nodeStats
	hsSem chan struct{} // bounds concurrent inbound handshakes
}

// maxPendingHandshakes bounds unauthenticated inbound handshakes.
const maxPendingHandshakes = 64

type transitFix struct {
	path []string
	peer string
	f    Fix
}

type remoteFix struct {
	f      Fix // TMs rebased to the local clock
	peer   string
	recvMs int64
}

type nodeStats struct {
	loops, hopDrops, spoofed, protoErrors, denied atomic.Uint64
	mlatLocalDropped                              atomic.Uint64
	exports                                       atomic.Uint64
}

// peer is the configuration and connection state of one peer.
type peer struct {
	cfg PeerConfig

	mu        sync.Mutex
	link      *link
	state     string
	lastErr   string
	lastErrMs int64
	connects  uint64
	info      *Info
	pinfo     PeerInfo
	backoff   time.Duration
	nextDial  time.Time
	totalIn   uint64
	totalOut  uint64
}

// Peer states (Status).
const (
	StateConnected  = "connected"
	StateConnecting = "connecting"
	StateBackoff    = "backoff"
	StateWaiting    = "waiting" // accept-only peer not connected
	StateDisabled   = "disabled"
)

func (p *peer) setInfo(in Info) {
	in.Name, in.Software, in.Version = cleanStr([]byte(in.Name)), cleanStr([]byte(in.Software)), cleanStr([]byte(in.Version))
	p.mu.Lock()
	p.info = &in
	p.mu.Unlock()
}

func (p *peer) current() *link {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.link
}

func (p *peer) fail(err string, nowMs int64) {
	p.mu.Lock()
	p.lastErr, p.lastErrMs = err, nowMs
	p.mu.Unlock()
}

// New builds a node. cfg is normalized; Check problems are returned as an
// error.
func New(cfg Config, opts Options) (*Node, error) {
	cfg.Normalize()
	if probs := cfg.Check("federation"); len(probs) > 0 {
		return nil, fmt.Errorf("federation: invalid config: %s", strings.Join(probs, "; "))
	}
	if opts.Identity.Priv == nil {
		return nil, errors.New("federation: identity missing")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	n := &Node{cfg: cfg, opts: opts, id: opts.Identity.FeederID(), peers: map[string]*peer{},
		store: newStore(cfg.MaxRemoteAircraft, int64(cfg.TTLS)*1000), remoteFix: map[uint32]remoteFix{},
		hsSem: make(chan struct{}, maxPendingHandshakes)}
	n.hc = HandshakeConfig{Identity: opts.Identity, Name: cfg.Name, Version: opts.Version, Now: opts.Now}
	for _, pc := range cfg.Peers {
		if pc.ID == n.id {
			return nil, fmt.Errorf("federation: peer %q has this node's own ID", pc.Name)
		}
		p := &peer{cfg: pc, state: StateWaiting, backoff: time.Second}
		if pc.Disabled {
			p.state = StateDisabled
		}
		n.peers[pc.ID] = p
		n.order = append(n.order, p)
	}
	getCert := opts.Certificate
	if getCert == nil && cfg.Listen != "" {
		certPEM, keyPEM, err := avf.GenerateSelfSigned([]string{"airview-federation"})
		if err != nil {
			return nil, err
		}
		c, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		getCert = avf.StaticCertificate(&c)
	}
	if getCert != nil {
		n.srvTLS = ServerTLSConfig(getCert)
	}
	for _, fc := range cfg.Feeds {
		if fc.Disabled {
			continue
		}
		f, err := newOutFeed(n, fc)
		if err != nil {
			return nil, err
		}
		n.feeds = append(n.feeds, f)
	}
	return n, nil
}

// ID returns the node ID (what peer operators put in peers[].id).
func (n *Node) ID() string { return n.id }

func (n *Node) now() time.Time { return n.opts.Now() }

func (n *Node) logf(format string, args ...any) {
	if n.opts.Logf != nil {
		n.opts.Logf("federation: "+format, args...)
	}
}

// Addr returns the listener address (nil before Start or without Listen).
func (n *Node) Addr() net.Addr {
	n.lnMu.Lock()
	defer n.lnMu.Unlock()
	if n.ln == nil {
		return nil
	}
	return n.ln.Addr()
}

// Start opens the listener, starts the dialers, the exporter and the
// outbound feeds. It returns after the listener is bound.
func (n *Node) Start(ctx context.Context) error {
	if !n.started.CompareAndSwap(false, true) {
		return errors.New("federation: already started")
	}
	n.ctx, n.cancel = context.WithCancel(ctx)
	n.startMs = n.now().UnixMilli()
	if n.cfg.Listen != "" {
		ln, err := net.Listen("tcp", n.cfg.Listen)
		if err != nil {
			n.cancel()
			return fmt.Errorf("federation: listen %s: %w", n.cfg.Listen, err)
		}
		n.lnMu.Lock()
		n.ln = ln
		n.lnMu.Unlock()
		n.wg.Add(1)
		go n.acceptLoop(ln)
	}
	for _, f := range n.feeds {
		if err := f.start(n.ctx, &n.wg); err != nil {
			n.Close()
			return err
		}
	}
	for _, p := range n.order {
		if p.cfg.Disabled || p.cfg.Addr == "" {
			continue
		}
		n.wg.Add(1)
		go n.dialLoop(p)
	}
	n.wg.Add(2)
	go n.exportLoop()
	go n.mlatLoop()
	return nil
}

// Close says goodbye to every peer and stops everything. It waits for the
// goroutines.
func (n *Node) Close() error {
	if !n.closed.CompareAndSwap(false, true) {
		return nil
	}
	if n.cancel != nil {
		n.cancel()
	}
	n.lnMu.Lock()
	if n.ln != nil {
		n.ln.Close()
	}
	n.lnMu.Unlock()
	for _, p := range n.order {
		if l := p.current(); l != nil {
			l.close(ByeShutdown, "")
		}
	}
	for _, f := range n.feeds {
		f.stop()
	}
	n.wg.Wait()
	return nil
}

// ---------------------------------------------------------------- links

func (n *Node) acceptLoop(ln net.Listener) {
	defer n.wg.Done()
	var tempDelay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if n.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			tempDelay = min(max(2*tempDelay, 5*time.Millisecond), time.Second)
			select {
			case <-time.After(tempDelay):
			case <-n.ctx.Done():
				return
			}
			continue
		}
		tempDelay = 0
		select {
		case n.hsSem <- struct{}{}:
		default: // too many unauthenticated handshakes in flight
			c.Close()
			continue
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.serveConn(c)
		}()
	}
}

func (n *Node) authorize(peerID string) (string, string) {
	p := n.peers[peerID]
	switch {
	case p == nil:
		n.stats.denied.Add(1)
		return DenyUnknownPeer, "node is not a configured peer"
	case p.cfg.Disabled:
		n.stats.denied.Add(1)
		return DenyDisabled, "peering is disabled"
	}
	return "", ""
}

func (n *Node) serveConn(c net.Conn) {
	tc, info, err := ServerHandshake(n.ctx, c, n.srvTLS, n.hc, n.authorize)
	<-n.hsSem
	if err != nil {
		var de *DeniedError
		if errors.As(err, &de) {
			n.logf("denied inbound node %s", de.Code)
		}
		return
	}
	p := n.peers[info.NodeID]
	l := newLink(n, p, tc, info, false)
	n.runLink(l)
}

func (n *Node) dialLoop(p *peer) {
	defer n.wg.Done()
	host, _, _ := net.SplitHostPort(p.cfg.Addr)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		p.mu.Lock()
		wait := p.link != nil || n.now().Before(p.nextDial)
		p.mu.Unlock()
		if !wait {
			n.dialOnce(p, host)
		}
		select {
		case <-n.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (n *Node) dialOnce(p *peer, host string) {
	p.mu.Lock()
	p.state = StateConnecting
	p.mu.Unlock()
	fail := func(err error, wait time.Duration) {
		p.mu.Lock()
		if wait <= 0 {
			wait = p.backoff
			p.backoff = min(2*p.backoff, time.Minute)
		}
		wait = time.Duration(float64(wait) * (0.8 + 0.4*rand.Float64()))
		p.nextDial = n.now().Add(wait)
		if p.link == nil {
			p.state = StateBackoff
		}
		p.lastErr, p.lastErrMs = err.Error(), n.now().UnixMilli()
		p.mu.Unlock()
	}
	tlsCfg, err := ClientTLSConfig(host, p.cfg.Pin, n.opts.Roots)
	if err != nil {
		fail(err, 0)
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, 15*time.Second)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", p.cfg.Addr)
	if err != nil {
		fail(err, 0)
		return
	}
	tc, info, err := ClientHandshake(ctx, nc, tlsCfg, n.hc, p.cfg.ID)
	if err != nil {
		var de *DeniedError
		if errors.As(err, &de) {
			fail(err, 5*time.Minute)
		} else {
			fail(err, 0)
		}
		return
	}
	p.mu.Lock()
	p.backoff = time.Second
	p.mu.Unlock()
	l := newLink(n, p, tc, info, true)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.runLink(l)
	}()
}

// runLink registers the link (one link per peer), serves it and
// unregisters it.
func (n *Node) runLink(l *link) {
	if n.closed.Load() {
		l.close(ByeShutdown, "")
		return
	}
	p := l.p
	p.mu.Lock()
	old := p.link
	keep := true
	if old != nil {
		// Two links to one peer (both dialled at once, or a stale link):
		// the same dialler replaces its old link; otherwise the link
		// dialled by the lower node ID wins on both sides.
		if old.dialer != l.dialer && l.dialer > old.dialer {
			keep = false
		}
	}
	if keep {
		p.link = l
		p.state = StateConnected
		p.pinfo = l.info
		p.connects++
	}
	p.mu.Unlock()
	if !keep {
		l.close(ByeDuplicate, "a link to this peer exists")
		return
	}
	if old != nil {
		old.close(ByeDuplicate, "replaced by a newer link")
	}
	n.logf("peer %s connected (%s)", p.cfg.Name, map[bool]string{true: "outbound", false: "inbound"}[l.outbound])
	l.run()
	p.mu.Lock()
	if p.link == l {
		p.link = nil
		if p.cfg.Addr == "" {
			p.state = StateWaiting
		} else {
			p.state = StateBackoff
		}
		if r := l.reason(); r != "" {
			p.lastErr, p.lastErrMs = r, n.now().UnixMilli()
		}
	}
	p.totalIn += l.acIn.Load()
	p.totalOut += l.acOut.Load()
	p.mu.Unlock()
	n.logf("peer %s disconnected: %s", p.cfg.Name, l.reason())
}

// links returns the current links.
func (n *Node) links() []*link {
	out := make([]*link, 0, len(n.order))
	for _, p := range n.order {
		if l := p.current(); l != nil {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------- export

func (n *Node) exportLoop() {
	defer n.wg.Done()
	t := time.NewTicker(n.cfg.Interval())
	defer t.Stop()
	exp := time.NewTicker(5 * time.Second)
	defer exp.Stop()
	var buf []local
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-exp.C:
			n.store.expire(n.now().UnixMilli())
			n.expireFixes(n.now().UnixMilli())
		case <-t.C:
			buf = n.exportOnce(buf)
		}
	}
}

// exportOnce runs one export cycle and returns a recycled buffer.
func (n *Node) exportOnce(buf []local) []local {
	now := n.now()
	nowMs := now.UnixMilli()
	var prep []local
	if n.opts.Source != nil {
		s := n.opts.Source()
		prep = prepare(&s, n.opts.Guard, buf)
	}
	n.prepMu.Lock()
	old := n.prep
	n.prep, n.prepNowMs = prep, nowMs
	n.prepMu.Unlock()
	for _, f := range n.feeds {
		f.refresh(prep)
	}
	n.stats.exports.Add(1)
	localKeys := make(map[uint32]struct{}, len(prep))
	for i := range prep {
		localKeys[prep[i].key] = struct{}{}
	}
	for _, l := range n.links() {
		n.exportTo(l, prep, localKeys, nowMs)
	}
	return old[:0]
}

func (n *Node) exportTo(l *link, prep []local, localKeys map[uint32]struct{}, nowMs int64) {
	pol := &l.p.cfg.Send
	if pol.Aircraft == ShareNone {
		return
	}
	full := l.forceFull.Swap(false) || nowMs-l.lastFullMs >= int64(n.cfg.FullRefreshS)*1000
	if full {
		l.lastFullMs = nowMs
	}
	items := make([]publicdto.Aircraft, 0, len(prep))
	var a publicdto.Aircraft
	for i := range prep {
		if pol.MaxAircraft > 0 && len(items) >= pol.MaxAircraft {
			break
		}
		if !pol.apply(&prep[i].a, prep[i].island, &a) {
			continue
		}
		if !full && l.lastSent[prep[i].key] == a.LastSeen {
			continue
		}
		l.lastSent[prep[i].key] = a.LastSeen
		items = append(items, a)
	}
	if full {
		for k := range l.lastSent {
			if _, ok := localKeys[k]; !ok {
				delete(l.lastSent, k)
			}
		}
	}
	self := []string{n.id}
	n.sendState(l, nowMs, full, self, items)

	// Transit: forward other peers' aircraft when both sides allow it.
	if !n.cfg.Transit || !pol.Transit {
		return
	}
	since := l.transitSeq
	if full {
		since = 0
	}
	groups := map[string]*transitGroup{}
	var keys []string
	l.transitSeq = n.store.changed(since, nowMs, func(e *remoteEntry, ra publicdto.Aircraft) {
		if _, ok := localKeys[e.key]; ok {
			return // our own reception wins
		}
		if len(e.path)+1 > n.cfg.MaxHops || slices.Contains(e.path, l.info.NodeID) {
			return // hop limit, split horizon
		}
		var out publicdto.Aircraft
		if !pol.apply(&ra, false, &out) {
			return
		}
		k := strings.Join(e.path, ",")
		g := groups[k]
		if g == nil {
			g = &transitGroup{path: append(slices.Clone(e.path), n.id)}
			groups[k] = g
			keys = append(keys, k)
		}
		g.items = append(g.items, out)
	})
	slices.Sort(keys)
	for _, k := range keys {
		g := groups[k]
		n.sendState(l, nowMs, false, g.path, g.items)
	}
}

type transitGroup struct {
	path  []string
	items []publicdto.Aircraft
}

func (n *Node) sendState(l *link, nowMs int64, full bool, path []string, items []publicdto.Aircraft) {
	if len(items) == 0 && !full {
		return
	}
	for first := true; first || len(items) > 0; first = false {
		p, k := AppendState(make([]byte, 0, 4096), nowMs, full, path, items)
		if !l.enqueue(TState, p) {
			return
		}
		l.acOut.Add(uint64(k))
		items = items[k:]
	}
}

// ---------------------------------------------------------------- receive

func (n *Node) checkPath(l *link, path []string) bool {
	switch {
	case path[len(path)-1] != l.info.NodeID:
		n.stats.spoofed.Add(1)
		return false
	case slices.Contains(path, n.id):
		n.stats.loops.Add(1)
		return false
	case len(path) > n.cfg.MaxHops:
		n.stats.hopDrops.Add(1)
		return false
	}
	for i := 1; i < len(path); i++ {
		if slices.Contains(path[:i], path[i]) {
			n.stats.loops.Add(1)
			return false
		}
	}
	return true
}

func (n *Node) onState(l *link, p []byte) error {
	sb, err := DecodeState(p, nil)
	if err != nil {
		return err
	}
	if !n.checkPath(l, sb.Path) {
		l.rejected.Add(uint64(len(sb.Aircraft)))
		return nil
	}
	rp := &l.p.cfg.Receive
	nowMs := n.now().UnixMilli()
	for i := range sb.Aircraft {
		a := &sb.Aircraft[i]
		if !rp.accept(a) {
			l.rejected.Add(1)
			continue
		}
		key, ok := aircraftKey(a.ICAO)
		if !ok {
			continue
		}
		posMs := rebase(a, sb.TMs, nowMs)
		ok, prev := n.store.upsert(l.info.NodeID, sb.Path, a, key, posMs, nowMs, rp.MaxAircraft)
		if !ok {
			continue
		}
		l.acIn.Add(1)
		if n.opts.Sink != nil && a.LastSeen > prev {
			n.opts.Sink.HandleUpdate(updateFromAircraft(a, key, l.info.NodeID))
		}
	}
	return nil
}

func (n *Node) onMLAT(l *link, p []byte) error {
	mb, err := DecodeMLAT(p, nil)
	if err != nil {
		return err
	}
	if !n.checkPath(l, mb.Path) {
		l.rejected.Add(uint64(len(mb.Fixes)))
		return nil
	}
	rp := &l.p.cfg.Receive
	nowMs := n.now().UnixMilli()
	off := nowMs - mb.TMs
	var ups []model.DecodedUpdate
	n.mlatMu.Lock()
	for i := range mb.Fixes {
		f := mb.Fixes[i]
		if !rp.acceptFix(&f) {
			l.rejected.Add(1)
			continue
		}
		f.TMs += off
		l.mlatIn.Add(1)
		if len(n.remoteFix) < n.cfg.MaxRemoteAircraft || n.remoteFix[f.ICAO].recvMs != 0 {
			n.remoteFix[f.ICAO] = remoteFix{f: f, peer: l.info.NodeID, recvMs: nowMs}
		}
		if n.cfg.Transit && len(n.mlatTransit) < maxPendingFixes {
			n.mlatTransit = append(n.mlatTransit, transitFix{path: mb.Path, peer: l.info.NodeID, f: f})
		}
		if n.opts.Sink != nil {
			ups = append(ups, updateFromFix(&f, l.info.NodeID))
		}
	}
	n.mlatMu.Unlock()
	for _, u := range ups {
		n.opts.Sink.HandleUpdate(u)
	}
	return nil
}

func (n *Node) expireFixes(nowMs int64) {
	ttl := int64(n.cfg.TTLS) * 1000
	n.mlatMu.Lock()
	for k, r := range n.remoteFix {
		if nowMs-r.recvMs > ttl {
			delete(n.remoteFix, k)
		}
	}
	n.mlatMu.Unlock()
}

// updateFromAircraft converts a remote aircraft (local clock) for a Sink.
func updateFromAircraft(a *publicdto.Aircraft, key uint32, peerID string) model.DecodedUpdate {
	u := model.DecodedUpdate{Recv: time.UnixMilli(a.LastSeen), FeedID: FeedPrefix + peerID, ICAO: key, Msgs: 1,
		AltBaro: a.AltBaro, AltGeom: a.AltGeom, GS: a.GS, Track: a.Track, VertRate: a.VertRate, OnGround: a.OnGround}
	if a.Callsign != "" {
		s := a.Callsign
		u.Callsign = &s
	}
	if a.Squawk != "" {
		s := a.Squawk
		u.Squawk = &s
	}
	if a.Lat != nil && a.Lon != nil {
		u.Lat, u.Lon = a.Lat, a.Lon
		if a.Source == model.SrcMLAT {
			u.MLAT, u.Src, u.SigmaM = true, SrcMLATFed, a.MLATSigmaM
			u.Grade = gradeFromString(a.MLATGrade)
		}
	}
	return u
}

func gradeFromString(s string) uint8 {
	if len(s) == 1 && s[0] >= 'A' && s[0] <= 'D' {
		return model.GradeA + s[0] - 'A'
	}
	return model.GradeNone
}

// updateFromFix converts a remote MLAT fix for a Sink.
func updateFromFix(f *Fix, peerID string) model.DecodedUpdate {
	lat, lon := f.Lat, f.Lon
	alt := f.AltM * 3.28084
	sig := f.SigmaHM
	u := model.DecodedUpdate{Recv: time.UnixMilli(f.TMs), FeedID: FeedPrefix + peerID, ICAO: f.ICAO, MLAT: true,
		Lat: &lat, Lon: &lon, AltGeom: &alt, Msgs: 1, Src: SrcMLATFed, SigmaM: &sig, Grade: f.Grade}
	if f.Vel != nil {
		e, nn, up := float64(f.Vel[0]), float64(f.Vel[1]), float64(f.Vel[2])
		gs := math.Hypot(e, nn) * 1.943844
		trk := math.Mod(math.Atan2(e, nn)*180/math.Pi+360, 360)
		vr := up * 196.850394
		u.GS, u.Track, u.VertRate = &gs, &trk, &vr
	}
	return u
}

// ---------------------------------------------------------------- MLAT

const maxPendingFixes = 50_000

// PublishMLAT queues own MLAT results for peers whose policy shares MLAT.
// Only displayable grades (A-C) are shared; hold-out fixes are not. Safe
// for concurrent use; it never blocks on the network.
func (n *Node) PublishMLAT(fixes []model.MLATFix) {
	if n.closed.Load() {
		return
	}
	var conv []Fix
	for i := range fixes {
		if f, ok := FixFromModel(&fixes[i]); ok {
			conv = append(conv, f)
		}
	}
	if len(conv) == 0 {
		return
	}
	n.mlatMu.Lock()
	n.mlatLocal = append(n.mlatLocal, conv...)
	if over := len(n.mlatLocal) - maxPendingFixes; over > 0 {
		n.mlatLocal = append(n.mlatLocal[:0], n.mlatLocal[over:]...)
		n.stats.mlatLocalDropped.Add(uint64(over))
	}
	n.mlatMu.Unlock()
}

// FixFromModel converts an own MLAT result to the federation form. It
// reports false for fixes that are not shared (grade D/none, hold-out).
func FixFromModel(m *model.MLATFix) (Fix, bool) {
	if !model.Displayable(m.Grade) || m.Flags&model.FixFlagHoldout != 0 {
		return Fix{}, false
	}
	if m.ICAO > 0xFFFFFF {
		// Synthetic identities (Mode A/C targets, track.ACIDFlag) are
		// never shared: masking them to 24 bits would alias a real
		// ICAO address, and the island guard cannot judge them
		// (PRIV-A6).
		return Fix{}, false
	}
	llh := mgeo.FromECEF(mgeo.Vec3{X: m.ECEF[0], Y: m.ECEF[1], Z: m.ECEF[2]})
	if math.IsNaN(llh.Lat) || math.IsNaN(llh.Lon) || math.IsNaN(llh.H) {
		return Fix{}, false
	}
	sl, cl := math.Sincos(llh.Lat * math.Pi / 180)
	so, co := math.Sincos(llh.Lon * math.Pi / 180)
	e := [3]float64{-so, co, 0}
	nn := [3]float64{-sl * co, -sl * so, cl}
	u := [3]float64{cl * co, cl * so, sl}
	c := [3][3]float64{
		{float64(m.Cov[0]), float64(m.Cov[1]), float64(m.Cov[2])},
		{float64(m.Cov[1]), float64(m.Cov[3]), float64(m.Cov[4])},
		{float64(m.Cov[2]), float64(m.Cov[4]), float64(m.Cov[5])},
	}
	quad := func(v [3]float64) float64 {
		s := 0.0
		for i := range 3 {
			for j := range 3 {
				s += v[i] * c[i][j] * v[j]
			}
		}
		return max(s, 0)
	}
	f := Fix{ICAO: m.ICAO & 0xFFFFFF, TMs: m.TUnixNs / 1e6, Lat: llh.Lat, Lon: llh.Lon, AltM: llh.H,
		SigmaHM: math.Sqrt(quad(e) + quad(nn)), SigmaVM: math.Sqrt(quad(u)),
		Grade: m.Grade, NBucket: m.NBucket, Flags: m.Flags}
	if m.Vel != nil {
		v := *m.Vel
		f.Vel = &v
	}
	return f, true
}

func (n *Node) mlatLoop() {
	defer n.wg.Done()
	t := time.NewTicker(time.Duration(n.cfg.MLATIntervalMs) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
			n.flushMLAT()
		}
	}
}

func (n *Node) flushMLAT() {
	n.mlatMu.Lock()
	own, tr := n.mlatLocal, n.mlatTransit
	n.mlatLocal, n.mlatTransit = nil, nil
	n.mlatMu.Unlock()
	if len(own) == 0 && len(tr) == 0 {
		return
	}
	nowMs := n.now().UnixMilli()
	self := []string{n.id}
	meta := n.fixMeta(own)
	for _, l := range n.links() {
		pol := &l.p.cfg.Send
		if !pol.MLAT {
			continue
		}
		var out []Fix
		for i := range own {
			f := own[i]
			if pol.fixAllowed(&f) && pol.ownFix(&f, meta) {
				out = append(out, f)
			}
		}
		n.sendMLAT(l, nowMs, self, out)
		if !n.cfg.Transit || !pol.Transit {
			continue
		}
		groups := map[string][]Fix{}
		paths := map[string][]string{}
		var keys []string
		for i := range tr {
			t := &tr[i]
			if len(t.path)+1 > n.cfg.MaxHops || slices.Contains(t.path, l.info.NodeID) || !pol.fixAllowed(&t.f) {
				continue
			}
			k := strings.Join(t.path, ",")
			if _, ok := paths[k]; !ok {
				paths[k] = append(slices.Clone(t.path), n.id)
				keys = append(keys, k)
			}
			groups[k] = append(groups[k], t.f)
		}
		for _, k := range keys {
			n.sendMLAT(l, nowMs, paths[k], groups[k])
		}
	}
}

// fixLocal is what the local picture says about the aircraft of an own
// MLAT fix (PRIV-A2): the island-guard verdict, ground and military.
type fixLocal struct {
	island, ground, military bool
}

// fixMetaSet maps the 24-bit address of own fixes to the local picture;
// guarded says whether an island guard is configured (without one a fix
// whose aircraft is not in the picture is not island-checked).
type fixMetaSet struct {
	m       map[uint32]fixLocal
	guarded bool
}

// fixMeta looks the aircraft of fixes up in the last prepared picture.
func (n *Node) fixMeta(fixes []Fix) fixMetaSet {
	out := fixMetaSet{guarded: n.opts.Guard != nil}
	if len(fixes) == 0 {
		return out
	}
	want := make(map[uint32]struct{}, len(fixes))
	for i := range fixes {
		want[fixes[i].ICAO] = struct{}{}
	}
	out.m = make(map[uint32]fixLocal, len(want))
	n.prepMu.RLock()
	for i := range n.prep {
		l := &n.prep[i]
		if _, ok := want[l.key]; !ok {
			continue
		}
		out.m[l.key] = fixLocal{island: l.island, ground: l.a.OnGround != nil && *l.a.OnGround, military: l.a.Military}
	}
	n.prepMu.RUnlock()
	return out
}

// ownFix applies the island guard and the NoGround / NoMilitary policy to
// an own MLAT fix exactly as to a STATE record (PRIV-A2): an island
// aircraft is dropped or coarsened to the public grid, and a fix whose
// aircraft is not (yet) in the local picture is held back whenever a
// verdict is needed. It may rewrite *f (coarsening).
func (p *Policy) ownFix(f *Fix, meta fixMetaSet) bool {
	loc, ok := meta.m[f.ICAO]
	if !ok {
		if meta.guarded || p.NoGround || p.NoMilitary {
			return false
		}
		return true
	}
	if p.NoMilitary && loc.military {
		return false
	}
	if p.NoGround && loc.ground {
		return false
	}
	if loc.island {
		if p.Island == IslandDrop {
			return false
		}
		g := privacy.IslandGridDeg
		f.Lat = math.Round(math.Round(f.Lat/g)*g*1e4) / 1e4
		f.Lon = math.Round(math.Round(f.Lon/g)*g*1e4) / 1e4
		// The grid cell's own uncertainty (half a cell along a meridian).
		f.SigmaHM = max(f.SigmaHM, g*111_320/2)
		f.Vel = nil
		f.Flags &^= model.FixFlagTracked
	}
	return true
}

func (n *Node) sendMLAT(l *link, nowMs int64, path []string, fixes []Fix) {
	for len(fixes) > 0 {
		p, k := AppendMLAT(make([]byte, 0, 4096), nowMs, path, fixes)
		if !l.enqueue(TMLAT, p) {
			return
		}
		l.mlatOut.Add(uint64(k))
		fixes = fixes[k:]
	}
}

// ---------------------------------------------------------------- views

// RemoteAircraft returns the aircraft received from peers (the best report
// per address) in public form, times in the local clock.
func (n *Node) RemoteAircraft() []publicdto.Aircraft {
	nowMs := n.now().UnixMilli()
	var out []publicdto.Aircraft
	n.store.view(nowMs, func(_ *remoteEntry, a publicdto.Aircraft) { out = append(out, a) })
	slices.SortFunc(out, func(x, y publicdto.Aircraft) int { return strings.Compare(x.ICAO, y.ICAO) })
	return out
}

// RemoteFixes returns the latest MLAT result per address received from
// peers (times in the local clock).
func (n *Node) RemoteFixes() []Fix {
	n.mlatMu.Lock()
	defer n.mlatMu.Unlock()
	out := make([]Fix, 0, len(n.remoteFix))
	for _, r := range n.remoteFix {
		out = append(out, r.f)
	}
	slices.SortFunc(out, func(a, b Fix) int { return int(a.ICAO) - int(b.ICAO) })
	return out
}

// Overlay adds the remote aircraft that the snapshot does not hold to a
// public snapshot (the "federated" map layer) and marks nothing else. The
// added aircraft keep their Source; their reference data is empty unless
// enrich fills it (may be nil).
func (n *Node) Overlay(s *publicdto.Snapshot, enrich func(a *publicdto.Aircraft)) int {
	have := make(map[string]struct{}, len(s.Aircraft))
	for i := range s.Aircraft {
		have[strings.ToUpper(s.Aircraft[i].ICAO)] = struct{}{}
	}
	added := 0
	for _, a := range n.RemoteAircraft() {
		if _, ok := have[strings.ToUpper(a.ICAO)]; ok {
			continue
		}
		if enrich != nil {
			enrich(&a)
		}
		s.Aircraft = append(s.Aircraft, a)
		added++
	}
	return added
}
