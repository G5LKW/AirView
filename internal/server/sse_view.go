package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"math"
	"net/http"
	"sort"
	"strconv"

	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

// Viewport subscriptions of the live stream (SO-1, DESIGN §20.2).
//
// A map shows a small part of a 10k-aircraft network: a typical browser
// view (zoom 7, 1920x1080) is about 10° x 5°. A stream opened with
// ?bbox=south,west,north,east (or ?view=1 for the whole world, to be
// narrowed later) only carries the aircraft inside that box:
//
//   - first an event "stream" {"id":"<token>","bbox":[s,w,n,e]|null}: the
//     token names this stream for POST /api/events/view?id=<token>&bbox=...
//     (a new viewport without a new keyframe) and for ?replace=<token> on a
//     reconnect (the old stream of the token is closed first, so the one
//     public stream per address is not refused while the old one lingers);
//   - delta streams (?delta=1, public): a keyframe ("aircraft" with seq)
//     holding the aircraft inside the box, then "delta" events with the
//     same chain (seq / base) as the unfiltered stream: aircraft that enter
//     the view are sent in full in "upd", aircraft in view only with their
//     changed members (the very bytes of the shared delta), aircraft that
//     leave the view (or the picture, or lose their position) in "del";
//   - full streams (public without delta, owner, admin, standalone): the
//     "aircraft" event lists only the aircraft inside the box.
//
// "counts" always describe the whole picture. Aircraft without a position
// are never in a viewport. live.js needs no change to understand these
// frames: a viewport delta is an ordinary delta of a smaller picture.
//
// Hysteresis: an aircraft enters a delta view when it is inside the box and
// leaves it only when it is outside the box grown by viewPadFrac of its
// size on every side (at least viewPadMinDeg). An aircraft flying along the
// edge therefore does not alternate between a full "upd" and a "del" every
// second, and the client keeps aircraft just outside the visible map,
// which it may draw while the user pans.
//
// Cost: the shared encoder indexes the picture once per new picture
// (1° cells); a viewport frame then costs O(aircraft in or near the view)
// and copies bytes only - no JSON encoding per client.

const (
	viewPadFrac   = 0.1
	viewPadMinDeg = 0.05
	// viewScanCells: a box covering more 1° cells than this is checked
	// aircraft by aircraft instead of cell by cell.
	viewScanCells = 4096
)

// viewEnt is one positioned aircraft of the indexed picture.
type viewEnt struct {
	icao     string
	lat, lon float64
}

// viewIndex is a 1° grid over the positioned aircraft of one picture.
type viewIndex struct {
	ents  []viewEnt
	pos   map[string]int32  // icao -> ents index
	cells map[int32][]int32 // cell -> ents indexes
}

func viewCell(lat, lon float64) int32 {
	la := int32(math.Floor(lat)) + 90
	lo := int32(math.Floor(lon)) + 180
	la = min(max(la, 0), 180)
	lo = ((lo % 360) + 360) % 360
	return la*360 + lo
}

// build indexes the positioned aircraft of snap.
func (x *viewIndex) build(snap *publicdto.Snapshot) {
	x.ents = make([]viewEnt, 0, len(snap.Aircraft))
	x.pos = make(map[string]int32, len(snap.Aircraft))
	x.cells = make(map[int32][]int32)
	for i := range snap.Aircraft {
		a := &snap.Aircraft[i]
		if a.Lat == nil || a.Lon == nil || !finite(*a.Lat) || !finite(*a.Lon) {
			continue
		}
		k := int32(len(x.ents))
		x.ents = append(x.ents, viewEnt{icao: a.ICAO, lat: *a.Lat, lon: *a.Lon})
		x.pos[a.ICAO] = k
		c := viewCell(*a.Lat, *a.Lon)
		x.cells[c] = append(x.cells[c], k)
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// query calls fn for every aircraft inside b (nil = all), in index order.
func (x *viewIndex) query(b *geoBox, fn func(k int32)) {
	if b == nil {
		for k := range x.ents {
			fn(int32(k))
		}
		return
	}
	la0, la1 := int32(math.Floor(b.s)), int32(math.Floor(b.n))
	type rng struct{ a, b int32 }
	var lons [2]rng
	nl := 1
	if b.w <= b.e {
		lons[0] = rng{int32(math.Floor(b.w)), int32(math.Floor(b.e))}
	} else {
		lons[0] = rng{int32(math.Floor(b.w)), 179}
		lons[1] = rng{-180, int32(math.Floor(b.e))}
		nl = 2
	}
	cells := 0
	for i := 0; i < nl; i++ {
		cells += int(lons[i].b-lons[i].a+1) * int(la1-la0+1)
	}
	if cells > viewScanCells || cells > len(x.cells) {
		for k := range x.ents {
			if e := &x.ents[k]; b.contains(e.lat, e.lon) {
				fn(int32(k))
			}
		}
		return
	}
	var hits []int32
	for la := la0; la <= la1; la++ {
		for i := 0; i < nl; i++ {
			for lo := lons[i].a; lo <= lons[i].b; lo++ {
				for _, k := range x.cells[viewCell(float64(la)+0.5, float64(lo)+0.5)] {
					if e := &x.ents[k]; b.contains(e.lat, e.lon) {
						hits = append(hits, k)
					}
				}
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i] < hits[j] })
	for _, k := range hits {
		fn(k)
	}
}

// padded returns b grown by viewPadFrac of its size on every side (nil
// stays nil: the whole world).
func (b *geoBox) padded() *geoBox {
	if b == nil {
		return nil
	}
	dLat := math.Max((b.n-b.s)*viewPadFrac, viewPadMinDeg)
	span := b.e - b.w
	if span < 0 {
		span += 360
	}
	dLon := math.Max(span*viewPadFrac, viewPadMinDeg)
	out := &geoBox{s: math.Max(-90, b.s-dLat), n: math.Min(90, b.n+dLat)}
	if span+2*dLon >= 360 {
		out.w, out.e = -180, 180
		return out
	}
	out.w, out.e = wrapLon(b.w-dLon), wrapLon(b.e+dLon)
	return out
}

// wrapLon maps a longitude into [-180, 180].
func wrapLon(v float64) float64 {
	for v < -180 {
		v += 360
	}
	for v > 180 {
		v -= 360
	}
	return v
}

// boxKey names a box in frame-cache keys ("" = the whole world).
func boxKey(b *geoBox) string {
	if b == nil {
		return ""
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	return f(b.s) + "," + f(b.w) + "," + f(b.n) + "," + f(b.e)
}

// boxJSON encodes b as [s,w,n,e] (null = the whole world).
func boxJSON(b *geoBox) string {
	if b == nil {
		return "null"
	}
	return "[" + boxKey(b) + "]"
}

// newStreamToken returns a random stream id (128 bits, hex).
func newStreamToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// streamHello is the "stream" event of a viewport stream.
func streamHello(c *liveClient) []byte {
	return sseFrame("stream", []byte(`{"id":"`+c.token+`","bbox":`+boxJSON(c.box)+`}`))
}

// ------------------------------------------------------------ delta views

// viewKeyframe returns the keyframe of the encoder's current picture
// restricted to c's box and resets c's view to it. Caller holds h.dmu.
func (d *deltaEncoder) viewKeyframe(c *liveClient) []byte {
	if d.prev == nil {
		return nil
	}
	var b bytes.Buffer
	b.Grow(256 + 400*len(c.inView))
	b.WriteString("event: aircraft\ndata: ")
	b.WriteString(`{"now":`)
	b.WriteString(strconv.FormatInt(d.keep.Now, 10))
	b.WriteString(`,"aircraft":[`)
	clear(c.inView)
	if c.inView == nil {
		c.inView = map[string]struct{}{}
	}
	n := 0
	d.idx.query(c.box, func(k int32) {
		icao := d.idx.ents[k].icao
		raw := d.raw[icao]
		if raw == nil {
			return
		}
		if n > 0 {
			b.WriteByte(',')
		}
		n++
		b.Write(raw)
		c.inView[icao] = struct{}{}
	})
	b.WriteString(`],"counts":`)
	b.Write(d.countsOrNull())
	b.WriteString(`,"seq":`)
	b.WriteString(strconv.FormatUint(d.seq, 10))
	b.WriteString("}\n\n")
	c.viewSeq = d.seq
	return b.Bytes()
}

func (d *deltaEncoder) countsOrNull() []byte {
	if d.counts == nil {
		return []byte("null")
	}
	return d.counts
}

// viewDelta returns c's delta from seq-1 to seq (nil when c already has
// seq). A client whose chain does not end at seq-1 gets a keyframe.
// Caller holds h.dmu.
func (d *deltaEncoder) viewDelta(c *liveClient) []byte {
	if d.prev == nil || c.viewSeq == d.seq {
		return nil
	}
	if c.viewSeq != d.seq-1 || c.inView == nil {
		return d.viewKeyframe(c)
	}
	var b bytes.Buffer
	b.Grow(256 + 96*len(c.inView))
	b.WriteString("event: delta\ndata: ")
	b.WriteString(`{"seq":`)
	b.WriteString(strconv.FormatUint(d.seq, 10))
	b.WriteString(`,"base":`)
	b.WriteString(strconv.FormatUint(d.seq-1, 10))
	b.WriteString(`,"now":`)
	b.WriteString(strconv.FormatInt(d.keep.Now, 10))
	b.WriteString(`,"counts":`)
	b.Write(d.countsOrNull())
	b.WriteString(`,"upd":[`)
	n := 0
	put := func(p []byte) {
		if n > 0 {
			b.WriteByte(',')
		}
		n++
		b.Write(p)
	}
	// Aircraft in view: changed members, or gone.
	pad := c.box.padded()
	var gone []string
	for icao := range c.inView {
		k, ok := d.idx.pos[icao]
		if !ok || !pad.contains(d.idx.ents[k].lat, d.idx.ents[k].lon) {
			gone = append(gone, icao)
			continue
		}
		if f := d.frag[icao]; f != nil {
			put(f)
		}
	}
	for _, icao := range gone {
		delete(c.inView, icao)
	}
	// Aircraft entering the view: in full.
	d.idx.query(c.box, func(k int32) {
		icao := d.idx.ents[k].icao
		if _, ok := c.inView[icao]; ok {
			return
		}
		raw := d.raw[icao]
		if raw == nil {
			return
		}
		put(raw)
		c.inView[icao] = struct{}{}
	})
	b.WriteString(`],"del":[`)
	sort.Strings(gone)
	for i, icao := range gone {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(icao) // hex addresses (optionally ~-prefixed): no escaping needed
		b.WriteByte('"')
	}
	b.WriteString("]}\n\n")
	c.viewSeq = d.seq
	return b.Bytes()
}

// viewDeltaFrameFor is the frame of a public delta viewport client. Called
// with h.mu held; the build function takes h.dmu.
func (h *liveHub) viewDeltaFrameFor(c *liveClient) (string, func() []byte) {
	key := "view|" + strconv.FormatUint(c.id, 10)
	if c.needKey {
		c.needKey = false // set again by broadcast when the frame is lost
		return key, func() []byte {
			h.dmu.Lock()
			defer h.dmu.Unlock()
			if h.denc.prev == nil && h.s != nil && h.s.eng.Tracker() != nil {
				snap, _, gen := h.s.publicSnapshotGen()
				h.denc.advance(snap, gen)
				h.tickDelta = nil
			}
			return h.denc.viewKeyframe(c)
		}
	}
	return key, func() []byte {
		h.dmu.Lock()
		defer h.dmu.Unlock()
		if h.tickDelta == nil && c.viewSeq == h.denc.seq {
			return nil // no new picture this tick
		}
		return h.denc.viewDelta(c)
	}
}

// ------------------------------------------------------------ full views

// filterPublic returns snap with only the aircraft inside b.
func filterPublic(snap publicdto.Snapshot, b *geoBox) publicdto.Snapshot {
	out := snap
	out.Aircraft = make([]publicdto.Aircraft, 0, 64)
	for i := range snap.Aircraft {
		a := &snap.Aircraft[i]
		if a.Lat != nil && a.Lon != nil && b.contains(*a.Lat, *a.Lon) {
			out.Aircraft = append(out.Aircraft, *a)
		}
	}
	return out
}

// filterLive returns snap with only the aircraft inside b.
func filterLive(snap model.LiveSnapshot, b *geoBox) model.LiveSnapshot {
	out := snap
	out.Aircraft = make([]model.Aircraft, 0, 64)
	for i := range snap.Aircraft {
		a := &snap.Aircraft[i]
		if a.Lat != nil && a.Lon != nil && b.contains(*a.Lat, *a.Lon) {
			out.Aircraft = append(out.Aircraft, *a)
		}
	}
	return out
}

// filterAny filters the result of Server.snapshotFor.
func filterAny(v any, b *geoBox) any {
	switch s := v.(type) {
	case model.LiveSnapshot:
		return filterLive(s, b)
	case *model.LiveSnapshot:
		f := filterLive(*s, b)
		return &f
	case publicdto.Snapshot:
		return filterPublic(s, b)
	case *publicdto.Snapshot:
		f := filterPublic(*s, b)
		return &f
	}
	return v
}

// publicViewFrame is the full public "aircraft" event restricted to b.
func (h *liveHub) publicViewFrame(snap publicdto.Snapshot, b *geoBox) []byte {
	f := filterPublic(snap, b)
	data, err := marshal(&f)
	if err != nil {
		return nil
	}
	return sseFrame("aircraft", data)
}

// ------------------------------------------------------------ control

// findStreamLocked returns the client with stream token tok (h.mu held).
func (h *liveHub) findStreamLocked(tok string) *liveClient {
	if tok == "" {
		return nil
	}
	for c := range h.clients {
		if c.token != "" && c.token == tok {
			return c
		}
	}
	return nil
}

// replaceLocked closes the stream of token tok (h.mu held): ?replace= on
// a reconnect with a new viewport.
func (h *liveHub) replaceLocked(tok string) {
	if c := h.findStreamLocked(tok); c != nil {
		delete(h.clients, c)
		close(c.kick)
	}
}

// setView changes the viewport of the stream with token tok; false when
// there is no such stream.
func (h *liveHub) setView(tok string, b *geoBox) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.findStreamLocked(tok)
	if c == nil {
		return false
	}
	h.dmu.Lock()
	c.box = b
	h.dmu.Unlock()
	return true
}

// rawEventsView is POST /api/events/view?id=<stream token>&bbox=s,w,n,e:
// the new viewport of a live stream opened with ?bbox= or ?view=1 (no bbox
// = the whole world). Delta streams continue their chain: the next delta
// brings the aircraft that entered the new view in full and removes those
// that left it. The token (from the stream's "stream" event) is the only
// credential: it is known to that stream's reader alone. 204 on success,
// 404 for an unknown token.
func rawEventsView(s *Server, w http.ResponseWriter, r *http.Request) {
	b, err := parseTrailBox(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.live.setView(qStr(r, "id"), b) {
		writeError(w, http.StatusNotFound, "no such live stream")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
