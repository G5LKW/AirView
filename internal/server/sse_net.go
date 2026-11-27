package server

import (
	"net/http"
	"strconv"

	"airview/internal/engine"
	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

// Per-viewer live stream frames (DESIGN §8.4: "the SSE stream needs one
// projection per (scope, owner, station) cache key").
//
// Every tick the hub encodes each distinct frame once and hands the same
// bytes to every client of that class:
//
//   - standalone, normal stream: one full snapshot (as before);
//   - public (aggregator public viewers and every /api/public/events
//     stream): one publicdto frame, shared with the public routes;
//   - aggregator owner / admin: one frame per model.Viewer.CacheKey.
//
// Sessions are re-checked with every status event (5 s): a stream whose
// session ended continues as a public stream.

// public reports whether c gets the public projection.
func (c *liveClient) public(net bool) bool {
	return c.forcePublic || (net && c.viewer.Scope == model.ScopePublic)
}

// restricted reports whether c must not receive admin-only events (alerts,
// record and feed notices).
func (c *liveClient) restricted(net bool) bool {
	return c.forcePublic || (net && c.viewer.Scope != model.ScopeAdmin)
}

// frameFn names the frame class of a client and how to build its frame.
type frameFn func(c *liveClient, net bool) (key string, build func() []byte)

// framePicker returns a broadcast pick function that builds each frame
// class at most once.
func (h *liveHub) framePicker(fn frameFn) func(*liveClient) []byte {
	net := h.netMode()
	cache := map[string][]byte{}
	return func(c *liveClient) []byte {
		key, build := fn(c, net)
		if f, ok := cache[key]; ok {
			return f
		}
		f := build()
		cache[key] = f
		return f
	}
}

// tickFrames sends the aircraft frame (and the status frame when status is
// set) to every client.
func (h *liveHub) tickFrames(status bool) {
	if status && h.s.aggregatorMode() {
		h.refreshViewers()
		h.dropPublicIfClosed()
	}
	h.advanceDelta()
	h.broadcast(nil, h.framePicker(h.aircraftFrameFor))
	if status {
		h.sendStatus()
	}
}

// dropPublicIfClosed disconnects every public-scope stream once the
// operator switched aggregator.publicMap off (PRIV-A1): streams opened
// before the change must not keep the merged live map flowing.
func (h *liveHub) dropPublicIfClosed() {
	if h.s.eng.EffectiveConfig().Aggregator.PublicMap {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.viewer.Scope == model.ScopePublic {
			delete(h.clients, c)
			close(c.kick)
			h.dropped.Add(1)
		}
	}
}

// refreshViewers re-resolves the sessions of the non-public clients.
func (h *liveHub) refreshViewers() {
	h.mu.Lock()
	var cs []*liveClient
	for c := range h.clients {
		if !c.forcePublic && c.viewer.Scope != model.ScopePublic {
			cs = append(cs, c)
		}
	}
	h.mu.Unlock()
	if len(cs) == 0 {
		return
	}
	vs := make([]model.Viewer, len(cs))
	for i, c := range cs {
		vs[i] = h.s.viewer(c.req)
		vs[i].SID = c.viewer.SID // the selection was checked at connect
	}
	h.mu.Lock()
	for i, c := range cs {
		c.viewer = vs[i]
	}
	h.mu.Unlock()
}

func (h *liveHub) aircraftFrameFor(c *liveClient, net bool) (string, func() []byte) {
	switch {
	case c.public(net) && c.delta && c.view:
		return h.viewDeltaFrameFor(c)
	case c.public(net) && c.delta:
		// Called with h.mu held (broadcast, firstFrames): c may be updated.
		if c.needKey {
			c.needKey = false // set again by broadcast when the frame is lost
			return "pubkey", h.keyframe
		}
		return "pubdelta", func() []byte {
			h.dmu.Lock()
			defer h.dmu.Unlock()
			return h.tickDelta
		}
	case c.public(net) && c.view:
		// The picture restricted to the client's box: one encoding per
		// (picture, box).
		snap, _, gen := h.s.publicSnapshotGen()
		if gen == c.lastGen {
			return "pubsame", func() []byte { return nil }
		}
		c.lastGen = gen
		h.dmu.Lock()
		b := c.box
		h.dmu.Unlock()
		return "pubv" + strconv.FormatUint(gen, 10) + "|" + boxKey(b), func() []byte { return h.publicViewFrame(snap, b) }
	case c.public(net):
		// One encoding per picture, shared; a picture already sent to this
		// client (the first frame) is not sent again.
		f, gen := h.s.publicFrame()
		if gen == c.lastGen {
			return "pubsame", func() []byte { return nil }
		}
		c.lastGen = gen
		return "pub" + strconv.FormatUint(gen, 10), func() []byte { return f }
	}
	var box *geoBox
	if c.view {
		h.dmu.Lock()
		box = c.box
		h.dmu.Unlock()
	}
	if !net {
		if !c.view {
			return "full", h.aircraftFrame
		}
		return "full|" + boxKey(box), func() []byte {
			tr := h.s.eng.Tracker()
			if tr == nil {
				return nil
			}
			b, err := marshal(filterLive(tr.Snapshot(), box))
			if err != nil {
				return nil
			}
			return sseFrame("aircraft", b)
		}
	}
	v, sel, hasSel := c.viewer, c.sel, c.hasSel
	key := "v|" + v.CacheKey()
	if c.view {
		key += "|" + boxKey(box) + "|v" // "|v": a world view still filters out aircraft without a position
	}
	view := c.view
	return key, func() []byte {
		snap := h.s.snapshotFor(v, sel, hasSel)
		if view {
			snap = filterAny(snap, box)
		}
		b, err := marshal(snap)
		if err != nil {
			return nil
		}
		return sseFrame("aircraft", b)
	}
}

// advanceDelta moves the shared delta encoder to the current public
// picture when a delta client is connected.
func (h *liveHub) advanceDelta() {
	if h.s == nil || h.s.eng.Tracker() == nil {
		return
	}
	net := h.netMode()
	h.mu.Lock()
	any := false
	for c := range h.clients {
		if c.delta && c.public(net) {
			any = true
			break
		}
	}
	h.mu.Unlock()
	h.dmu.Lock()
	defer h.dmu.Unlock()
	if !any {
		h.tickDelta, h.denc = nil, deltaEncoder{} // start afresh with the next client
		return
	}
	snap, _, gen := h.s.publicSnapshotGen()
	h.tickDelta = h.denc.advance(snap, gen)
}

// keyframe returns the keyframe of the encoder's current picture: a delta
// client's chain continues from it with the next delta.
func (h *liveHub) keyframe() []byte {
	h.dmu.Lock()
	defer h.dmu.Unlock()
	if h.denc.prev == nil && h.s != nil && h.s.eng.Tracker() != nil {
		snap, _, gen := h.s.publicSnapshotGen()
		h.denc.advance(snap, gen)
		h.tickDelta = nil
	}
	return h.denc.keyframeFrame()
}

func (h *liveHub) statusFrameFor(c *liveClient, net bool) (string, func() []byte) {
	if c.restricted(net) {
		return "pub", func() []byte {
			b, err := marshal(h.s.publicStatus())
			if err != nil {
				return nil
			}
			return sseFrame("status", b)
		}
	}
	if h.s.isAdmin(c.req) {
		return "adm1", func() []byte { return h.statusFrame(true) }
	}
	return "adm0", func() []byte { return h.statusFrame(false) }
}

// noticeEvent sends a notice: in full to unrestricted clients, as a public
// notice (aircraft kinds only) to the others.
func (h *liveHub) noticeEvent(nt engine.Notice) {
	full, err := marshal(nt)
	if err != nil {
		return
	}
	var restricted []byte
	if publicdto.PublicNoticeKind(nt.Kind) {
		if b, err := marshal(publicdto.Notice{Level: nt.Level, Text: nt.Text, T: nt.T, Kind: nt.Kind, ICAO: nt.ICAO}); err == nil {
			restricted = sseFrame("notice", b)
		}
	}
	h.broadcastEvent(sseFrame("notice", full), restricted)
}

// firstFrames returns the status and aircraft frames sent right after a
// client connects.
func (h *liveHub) firstFrames(c *liveClient) []byte {
	net := h.netMode()
	var out []byte
	_, st := h.statusFrameFor(c, net)
	out = append(out, st()...)
	h.mu.Lock() // aircraftFrameFor updates the client's stream state
	_, ac := h.aircraftFrameFor(c, net)
	h.mu.Unlock()
	return append(out, ac()...)
}

// publicStreamsLocked counts the public streams and those of ip (h.mu held).
func (h *liveHub) publicStreamsLocked(ip string) (total, fromIP int) {
	net := h.netMode()
	for o := range h.clients {
		if o.public(net) {
			total++
			if o.ip == ip && !o.loopback {
				fromIP++
			}
		}
	}
	return total, fromIP
}

// maxPublicStreams is aggregator.limits.maxPublicSse.
func (s *Server) maxPublicStreams() int {
	n := model.DefaultAggregator().Limits.MaxPublicSSE
	if s.eng != nil {
		if h := s.eng.Config(); h != nil {
			n = h.Get().Aggregator.Limits.MaxPublicSSE
		}
	}
	return max(1, n)
}

// rawPublicEvents is GET /api/public/events: the live stream with the
// public projection, in every mode.
func rawPublicEvents(s *Server, w http.ResponseWriter, r *http.Request) {
	serveEvents(s, w, r, true)
}

// netMode reports whether the aggregator projections apply (false for a
// hub without a server, as in unit tests).
func (h *liveHub) netMode() bool { return h.s != nil && h.s.aggregatorMode() }
