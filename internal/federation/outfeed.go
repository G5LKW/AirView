package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

// Federation-safe outbound feeds for other networks (WIRING.md §4).
//
// They carry only this network's own aircraft (never data received from
// peers) and never anything that locates or identifies a feeder:
//   - Beast: off-air Mode S frames of aircraft in the local picture, each
//     frame once (duplicates of the many stations that heard it are
//     removed), timestamp and signal byte zero (no station clock, no
//     reception level), no receiver status / position frames, no MLAT
//     results, no backfilled frames, and nothing of aircraft the island
//     guard flags or the policy filters out. Frames whose CRC does not
//     resolve to an aircraft of the local picture are dropped.
//   - JSON: one tar1090-style aircraft file per line (publicdto, so no
//     receiver fields at all), island aircraft coarsened or dropped.

// Beast frame type bytes and the escape.
const (
	beastEsc     = 0x1A
	beastModeAC  = '1'
	beastShort   = '2'
	beastLong    = '3'
	feedFlush    = 50 * time.Millisecond
	feedQueue    = 256
	dedupWindow  = 2 * time.Second
	maxFeedBatch = 256 << 10
)

// appendBeast appends one Beast frame with a zero timestamp and signal.
func appendBeast(dst []byte, typ byte, data []byte) []byte {
	dst = append(dst, beastEsc, typ, 0, 0, 0, 0, 0, 0, 0)
	for _, b := range data {
		dst = append(dst, b)
		if b == beastEsc {
			dst = append(dst, beastEsc)
		}
	}
	return dst
}

// Mode S CRC-24 (generator 0x1FFF409, ICAO Annex 10 Vol IV), table driven.
var crcTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 16
		for range 8 {
			if c&0x800000 != 0 {
				c = c<<1 ^ 0xFFF409
			} else {
				c <<= 1
			}
		}
		t[i] = c & 0xFFFFFF
	}
	return
}()

func crc24(b []byte) uint32 {
	var c uint32
	for _, x := range b {
		c = (c<<8 ^ crcTable[byte(c>>16)^x]) & 0xFFFFFF
	}
	return c
}

// frameAddr returns the 24-bit address a Mode S frame resolves to and
// whether the frame is usable: DF11/17/18 must check (DF11 allows the
// interrogator code in the low 7 bits), DF0/4/5/16/20/21 carry the address
// as parity. Other formats are not forwarded.
func frameAddr(d []byte) (uint32, bool) {
	n := len(d)
	if n != 7 && n != 14 {
		return 0, false
	}
	df := d[0] >> 3
	if (df < 16) != (n == 7) {
		return 0, false
	}
	pi := uint32(d[n-3])<<16 | uint32(d[n-2])<<8 | uint32(d[n-1])
	res := crc24(d[:n-3]) ^ pi
	aa := uint32(d[1])<<16 | uint32(d[2])<<8 | uint32(d[3])
	switch df {
	case 11:
		return aa, res&^0x7F == 0
	case 17, 18:
		return aa, res == 0
	case 0, 4, 5, 16, 20, 21:
		return res, true
	}
	return 0, false
}

// FeedStatus is the status of one outbound feed.
type FeedStatus struct {
	Name      string `json:"name"`
	Format    string `json:"format"`
	Mode      string `json:"mode"`
	Addr      string `json:"addr"`
	Clients   int    `json:"clients"`
	Connected bool   `json:"connected"`
	FramesIn  uint64 `json:"framesIn,omitempty"`
	FramesOut uint64 `json:"framesOut,omitempty"`
	Filtered  uint64 `json:"filtered,omitempty"`
	Dupes     uint64 `json:"dupes,omitempty"`
	Aircraft  int    `json:"aircraft"`
	BytesOut  uint64 `json:"bytesOut"`
	Dropped   uint64 `json:"dropped"`
	LastError string `json:"lastError,omitempty"`
}

type feedClient struct {
	c     net.Conn
	q     chan []byte
	done  chan struct{}
	once  sync.Once
	fails int
}

func (c *feedClient) close() {
	c.once.Do(func() {
		c.c.Close()
		close(c.done)
	})
}

type outFeed struct {
	n     *Node
	cfg   FeedConfig
	allow []netip.Prefix

	mu        sync.Mutex
	clients   map[*feedClient]struct{}
	ln        net.Listener
	connected bool
	lastErr   string

	// Beast: allowed addresses of the current picture and the dedupe sets.
	allowed atomic.Pointer[map[uint32]struct{}]
	bmu     sync.Mutex
	pending []byte
	cur     map[uint64]struct{}
	prev    map[uint64]struct{}
	rotAt   time.Time

	aircraft                             atomic.Int64
	framesIn, framesOut, filtered, dupes atomic.Uint64
	bytesOut, dropped                    atomic.Uint64
	stopOnce                             sync.Once
	stopCh                               chan struct{}
}

func newOutFeed(n *Node, cfg FeedConfig) (*outFeed, error) {
	f := &outFeed{n: n, cfg: cfg, clients: map[*feedClient]struct{}{}, stopCh: make(chan struct{}),
		cur: map[uint64]struct{}{}, prev: map[uint64]struct{}{}}
	for _, a := range cfg.Allow {
		p, err := parsePrefix(a)
		if err != nil {
			return nil, fmt.Errorf("federation: feed %s: allow %q: %w", cfg.Name, a, err)
		}
		f.allow = append(f.allow, p.Masked())
	}
	empty := map[uint32]struct{}{}
	f.allowed.Store(&empty)
	return f, nil
}

func (f *outFeed) start(ctx context.Context, wg *sync.WaitGroup) error {
	if f.cfg.Mode == ModeListen {
		ln, err := net.Listen("tcp", f.cfg.Addr)
		if err != nil {
			return fmt.Errorf("federation: feed %s: listen: %w", f.cfg.Name, err)
		}
		f.mu.Lock()
		f.ln = ln
		f.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.acceptLoop(ctx, ln)
		}()
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.connectLoop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.emitLoop(ctx)
	}()
	return nil
}

func (f *outFeed) stop() {
	f.stopOnce.Do(func() {
		close(f.stopCh)
		f.mu.Lock()
		if f.ln != nil {
			f.ln.Close()
		}
		cl := make([]*feedClient, 0, len(f.clients))
		for c := range f.clients {
			cl = append(cl, c)
		}
		f.mu.Unlock()
		for _, c := range cl {
			c.close()
		}
	})
}

// Addr returns the bound listener address of a listen feed.
func (f *outFeed) Addr() net.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln == nil {
		return nil
	}
	return f.ln.Addr()
}

func (f *outFeed) permitted(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	for _, p := range f.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (f *outFeed) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !f.permitted(c.RemoteAddr()) {
			c.Close()
			continue
		}
		f.mu.Lock()
		full := len(f.clients) >= f.cfg.MaxClients
		f.mu.Unlock()
		if full {
			c.Close()
			continue
		}
		go f.serveClient(c)
	}
}

func (f *outFeed) connectLoop(ctx context.Context) {
	backoff := time.Second
	for {
		var d net.Dialer
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		c, err := d.DialContext(dctx, "tcp", f.cfg.Addr)
		cancel()
		if err == nil {
			f.mu.Lock()
			f.connected = true
			f.mu.Unlock()
			start := time.Now()
			f.serveClient(c)
			f.mu.Lock()
			f.connected = false
			f.mu.Unlock()
			if time.Since(start) > time.Minute {
				backoff = time.Second
			}
		} else {
			f.mu.Lock()
			f.lastErr = err.Error()
			f.mu.Unlock()
		}
		wait := time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64()))
		backoff = min(2*backoff, time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-f.stopCh:
			return
		case <-time.After(wait):
		}
	}
}

// serveClient registers c and writes queued chunks until it fails; it
// blocks.
func (f *outFeed) serveClient(c net.Conn) {
	fc := &feedClient{c: c, q: make(chan []byte, feedQueue), done: make(chan struct{})}
	f.mu.Lock()
	select {
	case <-f.stopCh:
		f.mu.Unlock()
		c.Close()
		return
	default:
	}
	f.clients[fc] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.clients, fc)
		f.mu.Unlock()
		fc.close()
	}()
	// Discard anything the client sends; EOF ends the session.
	go func() {
		_, _ = io.Copy(io.Discard, c)
		fc.close()
	}()
	for {
		select {
		case <-fc.done:
			return
		case b := <-fc.q:
			_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := c.Write(b); err != nil {
				f.mu.Lock()
				f.lastErr = err.Error()
				f.mu.Unlock()
				return
			}
			f.bytesOut.Add(uint64(len(b)))
		}
	}
}

// broadcast hands b to every client without blocking; a client whose
// queue stays full is disconnected.
func (f *outFeed) broadcast(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.clients {
		select {
		case c.q <- b:
			c.fails = 0
		default:
			f.dropped.Add(1)
			c.fails++
			if c.fails > 100 {
				go c.close()
			}
		}
	}
}

func (f *outFeed) clientCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.clients)
}

// refresh receives the latest local picture (export goroutine).
func (f *outFeed) refresh(prep []local) {
	if f.cfg.Format != FormatBeast {
		return
	}
	pol := &f.cfg.Policy
	m := make(map[uint32]struct{}, len(prep))
	var tmp publicdto.Aircraft
	for i := range prep {
		if prep[i].island { // a raw frame cannot be coarsened
			continue
		}
		if !pol.apply(&prep[i].a, false, &tmp) {
			continue
		}
		m[prep[i].key&0xFFFFFF] = struct{}{}
	}
	f.aircraft.Store(int64(len(m)))
	f.allowed.Store(&m)
}

// frame offers one received frame to a Beast feed.
func (f *outFeed) frame(rf *model.RawFrame) {
	f.framesIn.Add(1)
	if rf.MLAT || rf.Late {
		f.filtered.Add(1)
		return
	}
	var typ byte
	switch rf.Kind {
	case model.FrameModeAC:
		if !f.cfg.ModeAC || len(rf.Data) != 2 {
			f.filtered.Add(1)
			return
		}
		typ = beastModeAC
	case model.FrameModeSShort, model.FrameModeSLong:
		addr, ok := frameAddr(rf.Data)
		if !ok {
			f.filtered.Add(1)
			return
		}
		if _, known := (*f.allowed.Load())[addr]; !known {
			f.filtered.Add(1)
			return
		}
		typ = beastShort
		if len(rf.Data) == 14 {
			typ = beastLong
		}
	default: // status / position frames never leave
		f.filtered.Add(1)
		return
	}
	h := fnv64(rf.Data)
	now := time.Now()
	f.bmu.Lock()
	defer f.bmu.Unlock()
	if now.Sub(f.rotAt) >= dedupWindow {
		f.prev, f.cur = f.cur, make(map[uint64]struct{}, len(f.cur))
		f.rotAt = now
	}
	if _, dup := f.cur[h]; dup {
		f.dupes.Add(1)
		return
	}
	if _, dup := f.prev[h]; dup {
		f.dupes.Add(1)
		return
	}
	f.cur[h] = struct{}{}
	if len(f.pending) >= maxFeedBatch {
		f.dropped.Add(1)
		return
	}
	f.pending = appendBeast(f.pending, typ, rf.Data)
	f.framesOut.Add(1)
}

func fnv64(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h ^ uint64(len(b))<<56
}

// emitLoop flushes Beast frames every 50 ms or emits a JSON file every
// IntervalMs.
func (f *outFeed) emitLoop(ctx context.Context) {
	iv := feedFlush
	if f.cfg.Format == FormatJSON {
		iv = time.Duration(f.cfg.IntervalMs) * time.Millisecond
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.stopCh:
			return
		case <-t.C:
		}
		if f.clientCount() == 0 {
			if f.cfg.Format == FormatBeast {
				f.bmu.Lock()
				f.pending = f.pending[:0]
				f.bmu.Unlock()
			}
			continue
		}
		if f.cfg.Format == FormatBeast {
			f.bmu.Lock()
			b := f.pending
			f.pending = make([]byte, 0, max(cap(b), 4096))
			f.bmu.Unlock()
			if len(b) > 0 {
				f.broadcast(b)
			}
			continue
		}
		b, cnt := f.n.aircraftJSON(&f.cfg.Policy)
		f.aircraft.Store(int64(cnt))
		f.broadcast(append(b, '\n'))
	}
}

func (f *outFeed) status() FeedStatus {
	f.mu.Lock()
	st := FeedStatus{Name: f.cfg.Name, Format: f.cfg.Format, Mode: f.cfg.Mode, Addr: f.cfg.Addr,
		Clients: len(f.clients), Connected: f.connected, LastError: f.lastErr}
	if f.ln != nil {
		st.Addr = f.ln.Addr().String()
	}
	f.mu.Unlock()
	st.FramesIn, st.FramesOut, st.Filtered, st.Dupes = f.framesIn.Load(), f.framesOut.Load(), f.filtered.Load(), f.dupes.Load()
	st.BytesOut, st.Dropped, st.Aircraft = f.bytesOut.Load(), f.dropped.Load(), int(f.aircraft.Load())
	return st
}

// HandleFrame offers a received raw frame to the Beast out-feeds. The
// engine tees every frame of the merged ingest here (cheap when no Beast
// feed is configured). Safe for concurrent use.
func (n *Node) HandleFrame(rf model.RawFrame) {
	for _, f := range n.feeds {
		if f.cfg.Format == FormatBeast {
			f.frame(&rf)
		}
	}
}

// HandleUpdate is a no-op so a Node can sit in a model.FrameSink tee.
func (n *Node) HandleUpdate(model.DecodedUpdate) {}

// WantsFrames reports whether any Beast out-feed is configured (the
// engine may skip the tee otherwise).
func (n *Node) WantsFrames() bool {
	for _, f := range n.feeds {
		if f.cfg.Format == FormatBeast {
			return true
		}
	}
	return false
}

// FeedAddr returns the bound address of the named listen feed.
func (n *Node) FeedAddr(name string) net.Addr {
	for _, f := range n.feeds {
		if f.cfg.Name == name {
			return f.Addr()
		}
	}
	return nil
}

// AircraftJSON renders the local picture through policy p as one
// tar1090-style aircraft file (publicdto.TarAircraftFile): only this
// network's own aircraft, no receiver fields, island aircraft coarsened or
// dropped per p.
func (n *Node) AircraftJSON(p *Policy) []byte {
	b, _ := n.aircraftJSON(p)
	return b
}

func (n *Node) aircraftJSON(p *Policy) ([]byte, int) {
	n.prepMu.RLock()
	snap := publicdto.Snapshot{Now: n.prepNowMs, Aircraft: make([]publicdto.Aircraft, 0, len(n.prep))}
	var a publicdto.Aircraft
	for i := range n.prep {
		if p.MaxAircraft > 0 && len(snap.Aircraft) >= p.MaxAircraft {
			break
		}
		if p.apply(&n.prep[i].a, n.prep[i].island, &a) {
			snap.Aircraft = append(snap.Aircraft, a)
		}
	}
	n.prepMu.RUnlock()
	if snap.Now == 0 {
		snap.Now = n.now().UnixMilli()
	}
	cnt := len(snap.Aircraft)
	file := publicdto.TarFromSnapshot(&snap, 0)
	for i := range file.Aircraft {
		sanitizeTar(&file.Aircraft[i])
	}
	b, err := json.Marshal(&file)
	if err != nil {
		return []byte(`{"now":0,"messages":0,"aircraft":[]}`), 0
	}
	return b, cnt
}

// sanitizeTar removes non-finite values (json.Marshal refuses them).
func sanitizeTar(t *publicdto.TarAircraft) {
	for _, p := range []**float64{&t.GS, &t.Mach, &t.Track, &t.TrackRt, &t.Roll, &t.MagHdg, &t.NavQNH, &t.NavHdg, &t.Lat, &t.Lon, &t.SeenPos} {
		if *p != nil && (math.IsNaN(**p) || math.IsInf(**p, 0)) {
			*p = nil
		}
	}
}
