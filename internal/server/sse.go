package server

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/alerts"
	avauth "airview/internal/auth"
	"airview/internal/model"
)

// The live stream (GET /api/events, Server-Sent Events).
//
// One goroutine (liveHub.run) produces the frames: the aircraft snapshot is
// encoded once per second and the same bytes go to every client; the status
// event is encoded every 5 s in at most two variants (adminLocked true /
// false). Each client has a small queue; a client that cannot keep up loses
// frames and, when it stays stuck, is disconnected - it never slows the
// hub or the other clients down.

const (
	sseAircraftEvery  = time.Second
	sseStatusTicks    = 5  // status every 5 aircraft ticks
	sseHeartbeatTicks = 15 // comment line every 15 s
	sseQueue          = 8
	// sseEventQueue holds the small one-off events (alert, notice) apart
	// from the periodic frames: a rule that fires for every aircraft at
	// once produces a burst of them, which must neither push out the
	// aircraft picture nor count as a stuck client.
	sseEventQueue   = 256
	sseMaxMissed    = 20 // consecutive dropped frames before a client is cut
	sseMaxClients   = 64
	sseMaxPerIP     = 8  // streams per non-loopback client address
	sseReserve      = 16 // extra streams for loopback / logged-in admins
	sseWriteTimeout = 10 * time.Second
	// sseGzipMax caps the streams that are gzip-compressed at once. A
	// BestSpeed flate compressor holds about 800 KiB for the life of the
	// stream; with all sseMaxClients+sseReserve streams that would be some
	// 60 MiB on a Raspberry Pi. Streams above the cap are sent plain. It
	// is the standalone default of http.sseGzipMax; an aggregator (a
	// server, SCALE-07) compresses every stream by default.
	sseGzipMax = 12
)

type liveClient struct {
	ch     chan []byte   // periodic frames (aircraft, status, heartbeat)
	ev     chan []byte   // events (alert, notice); nil = none
	req    *http.Request // for the per-client admin state
	missed int           // owned by the hub goroutine (under hub.mu)
	kick   chan struct{} // closed by the hub to disconnect the client
	id     uint64        // connection order (older = smaller)
	ip     string
	// loopback clients (this machine) are exempt from the per-IP cap.
	loopback bool
	// Aggregator mode (sse_net.go): the client's viewer, re-checked with
	// every status event; public streams have forcePublic set.
	viewer      model.Viewer
	forcePublic bool
	sel         model.StationIdx // station selected with ?station= (owner / admin)
	hasSel      bool
	// Public streams (SO-1): delta clients (?delta=1) get a keyframe, then
	// deltas; needKey asks for a keyframe (new client, or a frame was
	// lost). lastGen is the public picture generation last sent to a full
	// (non-delta) public stream, so the same picture is never sent twice.
	delta   bool
	needKey bool
	lastGen uint64
	// Viewport subscription (sse_view.go): view is set by ?bbox= or
	// ?view=1; box nil = the whole world. token names the stream for
	// POST /api/events/view and ?replace=. inView / viewSeq are the
	// aircraft a delta viewport client holds and the seq they belong to.
	// box, inView and viewSeq are guarded by hub.dmu.
	view    bool
	box     *geoBox
	token   string
	inView  map[string]struct{}
	viewSeq uint64
}

type liveHub struct {
	s       *Server
	mu      sync.Mutex
	clients map[*liveClient]struct{}
	done    chan struct{} // closed when run ends
	served  atomic.Uint64
	dropped atomic.Uint64
	// gzStreams is the number of streams holding a gzip compressor.
	gzStreams atomic.Int32
	// droppedEvents counts alert / notice frames lost on full event queues.
	droppedEvents atomic.Uint64
	seq           atomic.Uint64

	// dmu guards denc, the shared public delta encoder (sse_delta.go).
	// Lock order: h.mu before dmu.
	dmu       sync.Mutex
	denc      deltaEncoder
	tickDelta []byte // this tick's delta frame (nil = no new picture)
}

func newLiveHub(s *Server) *liveHub {
	return &liveHub{s: s, clients: map[*liveClient]struct{}{}, done: make(chan struct{})}
}

// sseFrame formats one event.
func sseFrame(event string, data []byte) []byte {
	b := make([]byte, 0, len(event)+len(data)+20)
	b = append(b, "event: "...)
	b = append(b, event...)
	b = append(b, "\ndata: "...)
	b = append(b, data...) // encoding/json never emits raw newlines
	b = append(b, "\n\n"...)
	return b
}

func (h *liveHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// broadcast queues a frame for every client (frame == nil: per-client frame
// from pick).
func (h *liveHub) broadcast(frame []byte, pick func(c *liveClient) []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		f := frame
		if pick != nil {
			f = pick(c)
		}
		if f == nil {
			continue
		}
		select {
		case c.ch <- f:
			c.missed = 0
		default:
			c.needKey = c.delta // a lost delta breaks the chain
			c.missed++
			if c.missed >= sseMaxMissed {
				delete(h.clients, c)
				close(c.kick)
				h.dropped.Add(1)
			}
		}
	}
}

// broadcastEvent queues an alert / notice frame for every client. A client
// whose event queue is full loses the event; that alone never disconnects
// it (stuck clients are detected on the periodic frames).
func (h *liveHub) broadcastEvent(frame, restricted []byte) {
	net := h.netMode()
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		f := frame
		if c.restricted(net) {
			f = restricted
		}
		if f == nil {
			continue
		}
		select {
		case c.ev <- f:
		default:
			h.droppedEvents.Add(1)
		}
	}
}

func (h *liveHub) aircraftFrame() []byte {
	tr := h.s.eng.Tracker()
	if tr == nil {
		return nil
	}
	b, err := marshal(tr.Snapshot())
	if err != nil {
		h.s.log.Warn("encoding the live snapshot", "err", err)
		return nil
	}
	return sseFrame("aircraft", b)
}

func (h *liveHub) statusFrame(admin bool) []byte {
	b, err := marshal(h.s.status(admin))
	if err != nil {
		return nil
	}
	return sseFrame("status", b)
}

// run produces the frames until ctx is cancelled.
func (h *liveHub) run(ctx context.Context) {
	defer close(h.done)
	notices, cancelNotices := h.s.eng.Notices()
	defer cancelNotices()
	var alertCh <-chan alerts.Alert
	if al := h.s.eng.Alerts(); al != nil {
		ch, cancelAlerts := al.Subscribe(64)
		defer cancelAlerts()
		alertCh = ch
	}
	tick := time.NewTicker(sseAircraftEvery)
	defer tick.Stop()
	heartbeat := []byte(": hb\n\n")
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case nt, ok := <-notices:
			if !ok {
				notices = nil // engine stopped
				continue
			}
			h.noticeEvent(nt)
		case al := <-alertCh:
			if !h.s.eng.EffectiveConfig().Alerts.Sound {
				al.Sound = false
			}
			if b, err := marshal(al); err == nil {
				h.broadcastEvent(sseFrame("alert", b), nil) // never below admin in aggregator mode
			}
		case <-tick.C:
			n++
			if h.count() == 0 {
				continue
			}
			// Not during a factory reset: the engine is being rebuilt.
			if !h.s.resetMu.TryRLock() {
				if n%sseHeartbeatTicks == 0 {
					h.broadcast(heartbeat, nil)
				}
				continue
			}
			if h.s.eng.Running() {
				h.tickFrames(n%sseStatusTicks == 0)
			}
			h.s.resetMu.RUnlock()
			if n%sseHeartbeatTicks == 0 {
				h.broadcast(heartbeat, nil)
			}
		}
	}
}

// pushStatus sends a fresh status event to every client now (after a
// configuration change, login state aside).
func (h *liveHub) pushStatus() {
	if h.count() == 0 {
		return
	}
	h.sendStatus()
}

// sendStatus sends the status event to every client (one encoding per
// viewer class, see sse_net.go).
func (h *liveHub) sendStatus() {
	h.broadcast(nil, h.framePicker(h.statusFrameFor))
}

// admitLocked decides whether a new client may connect (h.mu held). Every
// non-loopback address may hold at most sseMaxPerIP streams, so one LAN
// client cannot take all slots. Above sseMaxClients only privileged clients
// (loopback, logged-in admin) are admitted, up to sseMaxClients+sseReserve;
// when even that is full the oldest stream of the address holding the most
// streams is closed to make room. It returns "" or the refusal message.
func (h *liveHub) admitLocked(c *liveClient, priv bool) string {
	net := h.netMode()
	if c.public(net) && net {
		// Aggregator public streams (DESIGN §8.4): one per client address
		// and aggregator.limits.maxPublicSse in total; they do not use the
		// slots of logged-in users.
		total, fromIP := h.publicStreamsLocked(c.ip)
		if !c.loopback && fromIP > 0 {
			return "only one public live stream per address"
		}
		if total >= h.s.maxPublicStreams() {
			return "too many live connections"
		}
		return ""
	}
	perIP := map[string]int{}
	n := 0
	for o := range h.clients {
		if net && o.public(net) {
			continue // counted against maxPublicSse
		}
		n++
		if !o.loopback {
			perIP[o.ip]++
		}
	}
	if !c.loopback && perIP[c.ip] >= sseMaxPerIP {
		return "too many live connections from this address"
	}
	if n < sseMaxClients || (priv && n < sseMaxClients+sseReserve) {
		return ""
	}
	if !priv {
		return "too many live connections"
	}
	var victim *liveClient
	for o := range h.clients {
		if o.loopback || (net && o.public(net)) {
			continue
		}
		if victim == nil || perIP[o.ip] > perIP[victim.ip] ||
			(perIP[o.ip] == perIP[victim.ip] && o.id < victim.id) {
			victim = o
		}
	}
	if victim == nil {
		return "too many live connections"
	}
	delete(h.clients, victim)
	close(victim.kick)
	h.dropped.Add(1)
	return ""
}

// rawEvents is GET /api/events (per-viewer projection in aggregator mode).
func rawEvents(s *Server, w http.ResponseWriter, r *http.Request) {
	serveEvents(s, w, r, false)
}

// serveEvents runs one live stream; forcePublic selects the public
// projection regardless of the session.
func serveEvents(s *Server, w http.ResponseWriter, r *http.Request, forcePublic bool) {
	h := s.live
	rc := http.NewResponseController(w)
	c := &liveClient{ch: make(chan []byte, sseQueue), ev: make(chan []byte, sseEventQueue), req: r, kick: make(chan struct{}), id: h.seq.Add(1)}
	c.ip, c.loopback = avauth.ClientKeyString(s.remoteIP(r)), isLoopback(r) // one IPv6 /64 is one client
	c.forcePublic = forcePublic
	c.delta = qBool(r, "delta", false)
	c.needKey = c.delta
	if bb, err := parseTrailBox(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if bb != nil || qBool(r, "view", false) {
		c.view, c.box, c.token = true, bb, newStreamToken()
	}
	// PRIV-A1: with aggregator.publicMap off a public viewer gets no live
	// stream at all (owners and admins still do).
	if err := s.publicGate(r, mapOn, http.StatusForbidden); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if s.aggregatorMode() {
		c.viewer = s.viewerOf(r) // with the station selected by ?station=
		if st, ok := selectedStation(r); ok {
			c.sel, c.hasSel = st.Idx, true
		}
	}
	// Loopback and logged-in admins may use the reserve above sseMaxClients
	// (with no admin password everybody is admin: the per-IP cap still holds).
	priv := c.loopback || (s.passwordHash() != "" && s.isAdmin(r))
	h.mu.Lock()
	if tok := qStr(r, "replace"); tok != "" {
		h.replaceLocked(tok) // a reconnect with a new viewport
	}
	if msg := h.admitLocked(c, priv); msg != "" {
		h.mu.Unlock()
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, msg)
		return
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.served.Add(1)
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	}()

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("Cache-Control", "no-store, no-transform")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no") // nginx: do not buffer
	hd.Add("Vary", "Accept-Encoding")
	// The aircraft snapshot is JSON with a lot of repetition: gzip it when
	// the client accepts it (browsers do for EventSource and decode it
	// transparently, so live.js is unchanged). Every event is flushed
	// through the compressor (a sync flush), so events still arrive one by
	// one, as they are produced. At most sseGzipMax streams are compressed
	// at once (memory); the others are sent plain.
	var out io.Writer = w
	var zw *gzip.Writer
	if acceptsGzip(r) && h.acquireGzip() {
		defer h.gzStreams.Add(-1)
		hd.Set("Content-Encoding", "gzip")
		zw = gzPool.Get().(*gzip.Writer)
		zw.Reset(w)
		out = zw
		defer func() {
			_ = rc.SetWriteDeadline(time.Now().Add(time.Second))
			_ = zw.Close()
			zw.Reset(io.Discard)
			gzPool.Put(zw)
		}()
	}
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return // the connection cannot stream
	}

	send := func(b []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
		if _, err := out.Write(b); err != nil {
			return false
		}
		if zw != nil {
			if err := zw.Flush(); err != nil {
				return false
			}
		}
		return rc.Flush() == nil
	}
	// First picture at once, without waiting for the next tick.
	if !send([]byte("retry: 2000\n\n")) {
		return
	}
	if c.view && !send(streamHello(c)) {
		return
	}
	if s.resetMu.TryRLock() {
		var first []byte
		if s.eng.Running() {
			first = h.firstFrames(c)
		}
		s.resetMu.RUnlock()
		if len(first) > 0 && !send(first) {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-c.kick:
			return
		case b := <-c.ch:
			if !send(b) {
				return
			}
		case b := <-c.ev:
			if !send(b) {
				return
			}
		}
	}
}

// gzipMax is the effective http.sseGzipMax (SCALE-07).
func (h *liveHub) gzipMax() int32 {
	n := 0
	if h.s != nil && h.s.eng != nil {
		if c := h.s.eng.Config(); c != nil {
			n = c.Get().HTTP.SSEGzipMax
		}
	}
	switch {
	case n > 0:
		return int32(min(n, 100000))
	case h.s != nil && h.s.aggregatorMode():
		return sseMaxClients + sseReserve
	}
	return sseGzipMax
}

// acquireGzip reserves one of the sseGzipMax compressed-stream slots; the
// caller releases it with gzStreams.Add(-1).
func (h *liveHub) acquireGzip() bool {
	if h.gzStreams.Add(1) <= h.gzipMax() {
		return true
	}
	h.gzStreams.Add(-1)
	return false
}
