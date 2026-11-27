package mlatc

import (
	"compress/flate"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/ingest"
	"airview/internal/model"
)

// maxLine is the longest accepted JSON line (DESIGN §5.2: 64 KB).
const maxLine = 64 << 10

// conn is one mlat-client connection.
type conn struct {
	s  *Server
	nc net.Conn
	id uint64
	ip netip.Addr

	// set during the handshake, then read-only
	hello      hello
	ident      Identity
	key        string
	att        Attachment
	hasSlot    bool
	boundSt    model.StationIdx // station bound in the server tables (guarded by Server.mu)
	authedFlag atomic.Bool
	compress   string
	since      time.Time
	sess       SessionHandle

	sel *selection

	closeOnce sync.Once
	reason    atomic.Value // string
	done      chan struct{}

	lastReadNs atomic.Int64
	wire       countingReader
	offline    bool // an Offline session: no reader / writer goroutines, no compression

	// output
	outMu    sync.Mutex
	out      []byte
	spare    []byte
	outSig   chan struct{}
	zw       storedWriter
	resOK    atomic.Uint64
	resDrop  atomic.Uint64
	outLimit int

	// reader-goroutine state
	clk      clockState
	p        parser
	batch    []model.RawFrame
	arena    []byte
	recent   [dedupSlots]dedupEntry
	nowNs    int64
	inputUp  bool
	inputCnt int

	// rates (reader goroutine; published atomically for Links)
	secStart   int64
	secFrames  int
	seenBuf    []uint32 // reader goroutine: addresses new to the seen set
	secLines   int
	secBytes   uint64
	overSecs   int
	rateFrames atomic.Uint64 // float64 bits
	rateKB     atomic.Uint64

	lines, frames, syncs, mlats, bad atomic.Uint64
	epoch                            atomic.Uint32

	// split sync (handshake, then reader goroutine)
	split       bool          // split_sync was offered
	ssyncOK     atomic.Uint64 // valid ssync messages
	ssyncBad    int           // malformed ssync messages
	unknownMsgs int           // messages of unknown kind

	// rate_report summary (published for Links)
	adsbN, adsbRate atomic.Uint64 // aircraft count, Σ msgs/s (float64 bits)

	// proc orders the processing of the client's data: the TCP reader takes
	// it per line, the UDP readers per datagram (udp.go). Everything the
	// reader-goroutine state above holds is guarded by it once a datagram
	// can arrive; procClosed (under proc) stops processing after finish.
	proc       sync.Mutex
	procClosed bool

	// UDP transport (udp.go): key 0 = TCP only. Set in the handshake.
	udpKey                     uint32
	udpSeq                     seqWindow // under proc
	udpLost                    int64     // under proc: loss estimate
	um                         message   // under proc: decoded datagram message
	udpPkts, udpLostN, udpDupN atomic.Uint64
	udpBadN                    atomic.Uint64
	modeACn                    atomic.Uint64 // Mode A/C frames received
}

type dedupEntry struct{ ticks, h uint64 }

const dedupSlots = 256 // a sync half repeats within seconds; older entries are not needed

// countingReader counts wire bytes.
type countingReader struct {
	r io.Reader
	n atomic.Uint64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(uint64(n))
	return n, err
}

func newConn(s *Server, nc net.Conn, id uint64, ip netip.Addr) *conn {
	c := &conn{s: s, nc: nc, id: id, ip: ip, done: make(chan struct{}), outSig: make(chan struct{}, 1),
		outLimit: s.o.MaxOutQueue}
	c.wire.r = nc
	c.since = s.o.Now()
	c.lastReadNs.Store(c.since.UnixNano())
	return c
}

func (c *conn) lastRead() time.Time { return time.Unix(0, c.lastReadNs.Load()) }

// shutdown closes the connection; the reader goroutine then cleans up.
func (c *conn) shutdown(reason string) {
	c.closeOnce.Do(func() {
		c.reason.Store(reason)
		close(c.done)
		c.nc.Close()
	})
}

func (c *conn) closeReason() string {
	if r, ok := c.reason.Load().(string); ok {
		return r
	}
	return ReasonEOF
}

// run is the connection goroutine: handshake, then the read loop. It owns
// the cleanup.
func (c *conn) run() {
	s := c.s
	defer s.wg.Done()
	defer c.finish()
	br := newLineReader(&c.wire)
	if !c.handshake(br) {
		return
	}
	go c.writer()
	lr, err := c.lineReader(br)
	if err != nil {
		c.shutdown(ReasonCompressed)
		return
	}
	c.readLoop(lr)
}

// finish delivers what is left, detaches the station and forgets the
// client (once, after its reader stopped).
func (c *conn) finish() {
	s := c.s
	c.shutdown(ReasonEOF)
	s.udpUnregister(c)
	c.proc.Lock()
	c.procClosed = true // datagrams still in flight are dropped from here on
	c.flushBatch()
	c.proc.Unlock()
	if c.att.Admitted() {
		if c.sess != nil {
			c.sess.Close(0)
		}
		s.setEpoch(c.att.Station, c.clk.epoch)
		s.reg.Detach(c.ident, c.att)
		if h := s.o.Hooks.OnDetach; h != nil {
			h(c.att.Station, c.closeReason())
		}
	}
	s.remove(c)
	s.c.closed.Add(1)
	s.c.reason(c.closeReason())
}

// deny answers the handshake with a refusal and closes.
func (c *conn) deny(reason, code string, reconnect time.Duration) {
	c.s.c.refused.Add(1)
	c.nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	c.nc.Write(marshalLine(denyReply{Deny: []string{reason}, ReconnectIn: int(reconnect / time.Second)}))
	c.shutdown(code)
}

func (c *conn) handshake(br *lineReader) bool {
	s := c.s
	o := &s.o
	c.nc.SetReadDeadline(time.Now().Add(o.HandshakeTimeout))
	// The reference client pads the line with spaces; the 64 KB cap
	// applies to the handshake too. Bytes after it stay in br.
	line, too, err := br.readLine()
	if err != nil || too {
		c.shutdown(ReasonHandshake)
		return false
	}
	c.lastReadNs.Store(o.Now().UnixNano())
	h, ok := parseHello(line)
	if !ok {
		c.deny(denyBadHello, ReasonHandshake, o.ReconnectIn)
		return false
	}
	c.hello = h
	now := o.Now()
	if h.version != 2 && h.version != 3 {
		c.deny(denyVersion, ReasonHandshake, 10*time.Minute)
		return false
	}
	if o.Gate != nil && !o.Gate() {
		c.deny(denyNotReady, ReasonDenied, 5*time.Minute)
		return false
	}
	pol := s.policy()
	cls, base := classifyClock(h.clockType)
	id := Identity{Link: c.id, Kind: model.StationKindMLATC, UUID: h.uuid, Slot: -1, RemoteIP: c.ip,
		User: h.user, ClientVersion: h.clientVersion, ClockType: h.clockType, ClockCls: cls,
		Privacy: h.privacySet && h.privacy, PrivacyOff: h.privacySet && !h.privacy,
		Located: h.located, Lat: h.lat, Lon: h.lon, Alt: h.alt, AltRef: model.AltRefAmbiguous}
	id.IPKey = s.ipKey(c.ip)
	if id.UUID == "" {
		id.Slot = s.takeSlot(c.ip)
		c.hasSlot = true
	}
	c.ident = id
	c.key = id.Key()
	if s.coolingDown(c.key, now) {
		c.deny(denyCoolDown, ReasonDenied, o.ReconnectIn)
		return false
	}
	if !pol.allowed(c.ip) && s.stationsOf(c.ip, c.key) >= pol.PerIPStations {
		c.deny(denyStationsIP, ReasonLimits, 5*time.Minute)
		return false
	}
	old, ok := s.claimKey(c.key, c, now)
	if !ok {
		c.deny(denyDuplicate, ReasonDenied, o.ReconnectIn)
		return false
	}
	if old != nil {
		old.shutdown(ReasonReplaced)
	}
	// 30 s like the AVF admission deadline (SCALE-09).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	att, err := s.reg.Attach(ctx, id)
	cancel()
	if err != nil || !att.Admitted() {
		reason := denyRegistry
		if err == nil && att.Reason != "" {
			reason = sanitize(att.Reason, 120)
		}
		if err == nil {
			// a refusal: the station exists but must not feed
			att = Attachment{}
		}
		o.Logger.Info("mlat-client refused", "link", c.id, "ip", c.ip.String(), "user", sanitize(h.user, 64),
			"reason", reason, "err", err, "coolDown", pol.RefusedCoolDown.String())
		s.coolDown(c.key, pol.RefusedCoolDown, now)
		c.deny(reason, ReasonDenied, max(o.ReconnectIn, pol.RefusedCoolDown))
		return false
	}
	c.att = att
	s.authed(c)
	s.c.handshakes.Add(1)
	st := att.Station
	c.clk = clockState{raw: o.RawTicks}
	c.clk.setClass(cls, base)
	c.sel = newSelection(pol.MaxRequested, pol.MaxSeen)
	c.batch = make([]model.RawFrame, 0, o.BatchFrames)
	c.arena = make([]byte, 0, o.BatchFrames*14)
	c.compress = chooseCompression(h.compress, !o.DisableCompression && !c.offline)
	c.clk.epoch = s.bindStation(st, c, att.EpochBase) + 1
	s.setEpoch(st, c.clk.epoch)
	c.epoch.Store(c.clk.epoch)
	c.authedFlag.Store(true)

	c.split = s.offerSplit(c.key, now)
	reply := helloReply{Compress: c.compress, UDPTransport: s.udpOffer(c), SplitSync: c.split, MOTD: pol.MOTD}
	c.nc.SetWriteDeadline(time.Now().Add(o.WriteTimeout))
	if _, err := c.nc.Write(marshalLine(reply)); err != nil {
		c.shutdown(ReasonWrite)
		return false // att is admitted: run's cleanup detaches
	}
	s.sink.SetTimestamps(st, !att.NoTimestamps)
	c.sess = s.sink.OpenSession(ingest.SessionInfo{Proto: "mlatc", Label: "mlatc/" + strconv.FormatUint(c.id, 10),
		Controller: c, Selective: true})
	if c.sess != nil {
		mode := c.sess.AddStation(st)
		c.sel.mu.Lock()
		c.sel.selective = mode == ingest.ModeSelective
		c.sel.mu.Unlock()
	}
	s.sink.ClockEvent(st, ingest.ClockEvent{OldEpoch: c.clk.epoch - 1, NewEpoch: c.clk.epoch,
		Reason: model.ClockEvRestart, HostNs: now.UnixNano()})
	s.c.epochs.Add(1)
	if hk := o.Hooks.OnAttach; hk != nil {
		hk(st, id)
	}
	s.udpPublish(c) // datagrams count from here, after the epoch start
	o.Logger.Debug("mlat-client attached", "link", c.id, "station", uint32(st), "clock", h.clockType,
		"compress", c.compress, "version", h.clientVersion)
	return true
}

// lineReader returns the reader of the client's message lines after the
// handshake: the raw connection, or the inflated chunk stream.
func (c *conn) lineReader(br *lineReader) (*lineReader, error) {
	if c.compress == compressNone {
		return br, nil
	}
	br.release()
	return newLineReader(&lazyInflater{cr: newChunkReader(br)}), nil
}

// lazyInflater opens the inflater on the first read (the zlib header is in
// the first chunk, which may come much later than the handshake) and
// accepts a raw deflate stream as well as a zlib-wrapped one.
type lazyInflater struct {
	cr *chunkReader
	r  io.Reader
}

func (z *lazyInflater) Read(p []byte) (int, error) {
	if z.r == nil {
		b0, err := z.cr.ReadByte()
		if err != nil {
			return 0, err
		}
		b1, err := z.cr.ReadByte()
		if err != nil {
			return 0, err
		}
		pre := &prefixReader{pre: [2]byte{b0, b1}, r: z.cr}
		if b0&0x0f == 8 && (uint16(b0)<<8|uint16(b1))%31 == 0 {
			zr, err := zlib.NewReader(pre)
			if err != nil {
				return 0, err
			}
			z.r = zr
		} else {
			z.r = flate.NewReader(pre)
		}
	}
	return z.r.Read(p)
}

// prefixReader returns two peeked bytes, then the chunk stream. It is a
// ByteReader so the inflater does not add read-ahead buffering.
type prefixReader struct {
	pre [2]byte
	n   int
	r   *chunkReader
}

func (p *prefixReader) Read(b []byte) (int, error) {
	if p.n < 2 {
		k := copy(b, p.pre[p.n:])
		p.n += k
		return k, nil
	}
	return p.r.Read(b)
}

func (p *prefixReader) ReadByte() (byte, error) {
	if p.n < 2 {
		p.n++
		return p.pre[p.n-1], nil
	}
	return p.r.ReadByte()
}

// readLoop processes message lines until the connection ends.
func (c *conn) readLoop(lr *lineReader) {
	s := c.s
	o := &s.o
	var lastDeadline time.Time
	c.proc.Lock()
	c.nowNs = o.Now().UnixNano()
	c.proc.Unlock()
	udp := c.udpKey != 0
	if udp {
		// A UDP client may stay quiet on TCP while its datagrams flow: the
		// writer's idle check (any data, either path) replaces the deadline.
		c.nc.SetReadDeadline(time.Time{})
	}
	for {
		if lr.Buffered() == 0 {
			// The next read may block: deliver what we have first, then
			// refresh the idle deadline (at most once a second).
			c.proc.Lock()
			c.flushBatch()
			c.proc.Unlock()
			if t := time.Now(); !udp && t.Sub(lastDeadline) > time.Second {
				lastDeadline = t
				c.nc.SetReadDeadline(t.Add(o.IdleTimeout))
			}
		}
		line, too, err := lr.readLine()
		if err != nil {
			var ne net.Error
			switch {
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
			case errors.As(err, &ne) && ne.Timeout():
				c.shutdown(ReasonIdle)
			case errors.As(err, new(*net.OpError)):
				c.shutdown(ReasonReset) // connection reset or aborted by the peer
			case errors.As(err, new(flate.CorruptInputError)), errors.Is(err, zlib.ErrHeader),
				errors.Is(err, errEmptyChunk):
				c.shutdown(ReasonCompressed)
			default:
				c.shutdown(ReasonReadError)
			}
			return
		}
		c.proc.Lock()
		if lr.Buffered() == 0 || c.nowNs == 0 {
			c.nowNs = o.Now().UnixNano()
		}
		ok := c.processLine(line, too)
		c.proc.Unlock()
		if !ok {
			return
		}
		select {
		case <-c.done:
			return
		default:
		}
	}
}

// processLine handles one received line at c.nowNs; false = the client was
// disconnected.
func (c *conn) processLine(line []byte, too bool) bool {
	s := c.s
	c.lastReadNs.Store(c.nowNs)
	c.lines.Add(1)
	s.c.lines.Add(1)
	if too {
		s.c.longLines.Add(1)
		return c.badLine()
	}
	if !c.rateTick() {
		return false
	}
	if c.secLines++; c.secLines > 4*c.s.policy().MaxMsgsPerSec+1000 {
		s.c.dropRate.Add(1)
		return true
	}
	if err := c.handleLine(line); err != nil {
		return c.badLine()
	}
	return true
}

// badLine counts a malformed line; false = the client was disconnected.
func (c *conn) badLine() bool {
	c.s.c.badLines.Add(1)
	b := c.bad.Add(1)
	if int(b) > c.s.policy().MaxBadLines && b*2 > c.lines.Load() {
		c.shutdown(ReasonGarbage)
		return false
	}
	return true
}

// rateTick does the per-second rate accounting; false = disconnected.
func (c *conn) rateTick() bool {
	sec := c.nowNs / 1e9
	if sec == c.secStart {
		return true
	}
	pol := c.s.policy()
	wire := c.wire.n.Load()
	db := wire - c.secBytes
	el := float64(sec - c.secStart)
	if c.secStart == 0 || el <= 0 {
		el = 1
	}
	c.rateFrames.Store(math.Float64bits(float64(c.secFrames) / el))
	c.rateKB.Store(math.Float64bits(float64(db) / 1e3 / el))
	// MaxKBps is a budget for compressed links; an uncompressed client
	// sends the same frames as plain JSON lines (several times the bytes).
	// Its bytes count at 1/uncompressedByteFactor so a busy station is not
	// cut off just for not compressing (soak: 3,500 frames/s = 214 kB/s
	// of plain lines, refused every 5 minutes); the frame and line limits
	// bound its cost.
	lim := db
	if c.compress == compressNone {
		lim = db / uncompressedByteFactor
	}
	// Lines are bounded too: a small compressed chunk can inflate to a
	// huge number of (empty or ignored) lines.
	over := c.secFrames > pol.MaxMsgsPerSec || float64(lim)/el > float64(pol.MaxKBps)*1e3 ||
		c.secLines > 4*pol.MaxMsgsPerSec+1000
	if c.secStart != 0 && sec-c.secStart == 1 && over {
		c.overSecs++
	} else if !over {
		c.overSecs = 0
	}
	c.s.c.wireBytes.Add(db)
	if c.sess != nil {
		c.sess.AddBytes(int(lim)) // the session's byte limit is the same budget
	}
	c.secStart, c.secFrames, c.secLines, c.secBytes = sec, 0, 0, wire
	if c.overSecs >= pol.OverLimitSeconds {
		c.s.o.Logger.Info("mlat-client disconnected: over its rate limit", "link", c.id, "station", uint32(c.att.Station),
			"framesPerS", math.Float64frombits(c.rateFrames.Load()), "kBps", math.Float64frombits(c.rateKB.Load()),
			"overSeconds", c.overSecs, "coolDown", pol.CoolDown.String())
		c.s.coolDown(c.key, pol.CoolDown, time.Unix(0, c.nowNs))
		c.shutdown(ReasonRateLimit)
		return false
	}
	return true
}

// handleLine processes one JSON line.
func (c *conn) handleLine(line []byte) error {
	p := &c.p
	if err := p.begin(line); err != nil {
		// tolerate blank lines (keep-alive newlines)
		if len(trimWS(line)) == 0 {
			return nil
		}
		return err
	}
	for {
		more, err := p.next()
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		c.handleMsg(&p.msg)
		if c.split && !c.checkSplit() {
			return nil
		}
	}
}

func trimWS(b []byte) []byte {
	for len(b) > 0 && isWS(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isWS(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

func (c *conn) handleMsg(m *message) {
	s := c.s
	st := c.att.Station
	hk := &s.o.Hooks
	switch m.kind {
	case kMLAT, kSSync, kSync:
		if m.kind == kSync {
			c.syncs.Add(1)
			s.c.syncs.Add(1)
		} else {
			c.mlats.Add(1)
			s.c.mlats.Add(1)
		}
		first, good := len(c.batch), 0
		for i := 0; i < m.n; i++ {
			if !m.tok[i] || m.m[i] == nil {
				s.c.dropBad.Add(1)
				continue
			}
			good++
			c.frame(m.t[i], m.m[i])
		}
		switch {
		case m.kind == kSync && first <= len(c.batch):
			c.backdateSyncPair(first)
		case m.kind == kSSync && good > 0:
			c.ssyncOK.Add(1)
			s.c.ssyncs.Add(1)
		case m.kind == kSSync:
			c.ssyncBad++
		}
	case kSeen:
		// Address lists count towards the client's message budget (one
		// frame-equivalent per 4 addresses), so a flood of them trips the
		// rate limit like a frame flood (finding ROB-3).
		c.secFrames += (len(m.addrs) + 3) / 4
		c.sel.mu.Lock()
		c.seenBuf = c.sel.seenAdd(m.addrs, c.seenBuf[:0])
		c.sel.mu.Unlock()
		if hk.OnSeen != nil && len(c.seenBuf) > 0 {
			hk.OnSeen(st, c.seenBuf)
		}
	case kLost:
		c.secFrames += (len(m.addrs) + 3) / 4
		c.sel.mu.Lock()
		c.sel.seenDel(m.addrs)
		c.sel.mu.Unlock()
		if hk.OnLost != nil && len(m.addrs) > 0 {
			hk.OnLost(st, m.addrs)
		}
	case kRate:
		c.noteRates(m.rates)
		if hk.OnRates != nil && len(m.rates) > 0 {
			hk.OnRates(st, m.rates)
		}
	case kClockReset:
		c.clockReset(m)
	case kClockJump:
		c.newEpoch(model.ClockEvJump)
	case kInputDisconnected:
		c.inputUp = false
		c.inputCnt++
		if hk.OnInput != nil {
			hk.OnInput(st, false)
		}
	case kInputConnected:
		// After a receiver reconnect the client's clock is a new one.
		if c.inputCnt > 0 && !c.inputUp {
			c.newEpoch(model.ClockEvRestart)
		}
		c.inputUp = true
		if hk.OnInput != nil {
			hk.OnInput(st, true)
		}
	case kPosition:
		if m.hasPos && hk.OnPosition != nil {
			alt := m.alt
			if math.IsNaN(alt) {
				alt = 0
			}
			hk.OnPosition(st, m.lat, m.lon, alt)
		}
	case kUnknown:
		c.unknownMsgs++
	case kHeartbeat:
	}
}

// clockReset handles a clock_reset message (MLATC.md §5.3).
func (c *conn) clockReset(m *message) {
	reason := strings.ToLower(m.reason)
	if m.mode != "" || m.freq > 0 {
		if cls, base, ok := modeChange(c.hello.clockType, m.mode, m.freq, m.epoch); ok {
			c.flushBatch()
			c.clk.setClass(cls, base)
			c.newEpoch(model.ClockEvModeChange)
			return
		}
	}
	switch {
	case strings.Contains(reason, "rollover") && c.clk.base == baseGPS:
		// The day rollover of a GPS clock is not a discontinuity in
		// AirView ticks (model.GPSTicks resolves the day).
		return
	case strings.Contains(reason, "drop"):
		c.newEpoch(model.ClockEvSampleDrop)
	case strings.Contains(reason, "jump"):
		c.newEpoch(model.ClockEvJump)
	case strings.Contains(reason, "back"):
		c.newEpoch(model.ClockEvBackwards)
	case strings.Contains(reason, "mode"):
		c.newEpoch(model.ClockEvModeChange)
	default:
		c.newEpoch(model.ClockEvRestart)
	}
}

// newEpoch starts a new clock epoch: frames so far are delivered first,
// then the event, so the ingest never mixes the two tick mappings.
func (c *conn) newEpoch(reason model.ClockEventReason) {
	c.flushBatch()
	old := c.clk.epoch
	c.clk.epoch++
	c.clk.have = false
	c.epoch.Store(c.clk.epoch)
	c.s.setEpoch(c.att.Station, c.clk.epoch)
	c.s.c.epochs.Add(1)
	c.s.sink.ClockEvent(c.att.Station, ingest.ClockEvent{OldEpoch: old, NewEpoch: c.clk.epoch, Reason: reason,
		HostNs: c.nowNs})
}

// frame converts one timed frame and appends it to the batch.
func (c *conn) frame(t uint64, data []byte) {
	s := c.s
	ticks, step, ok := c.clk.normalise(t, len(data), c.nowNs)
	if !ok {
		s.c.dropBad.Add(1)
		return
	}
	if step != stepNone {
		r := model.ClockEvJump
		if step == stepBack {
			r = model.ClockEvBackwards
		}
		c.newEpoch(r)
		if ticks, _, ok = c.clk.normalise(t, len(data), c.nowNs); !ok {
			s.c.dropBad.Add(1)
			return
		}
	}
	// The reference client may send the same position message in two
	// consecutive sync pairs; deliver each reception once.
	h := frameHash(data)
	slot := &c.recent[(h^ticks*0x9e3779b97f4a7c15)>>56&(dedupSlots-1)]
	if slot.ticks == ticks && slot.h == h {
		s.c.dropDup.Add(1)
		return
	}
	slot.ticks, slot.h = ticks, h
	if c.secFrames++; c.secFrames > s.policy().MaxMsgsPerSec {
		s.c.dropRate.Add(1)
		return
	}
	if len(data) == 2 {
		c.modeACn.Add(1)
		s.c.modeAC.Add(1)
	}
	if len(c.batch) == cap(c.batch) || len(c.arena)+len(data) > cap(c.arena) {
		c.flushBatch()
	}
	off := len(c.arena)
	c.arena = append(c.arena, data...)
	c.batch = append(c.batch, model.RawFrame{
		GPSNs: -1, Signal: math.NaN(), Kind: frameKind(len(data)), Data: c.arena[off:len(c.arena):len(c.arena)],
		FeedID: c.att.FeedID, Channel: -1, Station: c.att.Station, Epoch: c.clk.epoch, ClockCls: c.clk.cls,
		Ticks: ticks, HostNs: c.nowNs,
	})
	c.frames.Add(1)
	s.c.frames.Add(1)
}

// frameHash is FNV-1a over the payload.
func frameHash(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, x := range b {
		h ^= uint64(x)
		h *= 1099511628211
	}
	return h
}

// flushBatch hands the collected frames to the sink.
func (c *conn) flushBatch() {
	if len(c.batch) == 0 {
		return
	}
	c.s.sink.Submit(c.batch)
	clear(c.batch)
	c.batch = c.batch[:0]
	c.arena = c.arena[:0]
}

// ---------------------------------------------------------------- output

// enqueue appends a complete line (or lines) to the output queue. Results
// (droppable) are refused when the queue is over its limit.
func (c *conn) enqueue(b []byte, droppable bool) bool {
	c.outMu.Lock()
	if droppable && len(c.out)+len(b) > c.outLimit {
		c.outMu.Unlock()
		return false
	}
	c.out = append(c.out, b...)
	c.outMu.Unlock()
	select {
	case c.outSig <- struct{}{}:
	default:
	}
	return true
}

func (c *conn) sendResult(r *Result) bool {
	if !c.hello.returnResults {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
	}
	var buf [512]byte
	line := appendResult(buf[:0], r, c.hello.ecef)
	if !c.enqueue(line, true) {
		c.resDrop.Add(1)
		c.s.c.resultsDrop.Add(1)
		return false
	}
	c.resOK.Add(1)
	c.s.c.results.Add(1)
	return true
}

// writer sends queued output, heartbeats, stats and selection updates.
func (c *conn) writer() {
	o := &c.s.o
	tick := time.NewTicker(min(o.FlushEvery, o.HeartbeatEvery))
	defer tick.Stop()
	now := o.Now()
	nextHB, nextStats := now.Add(o.HeartbeatEvery), now.Add(min(o.StatsEvery, 30*time.Second))
	var start, stop []uint32
	var acStart, acStop []uint16
	var wbuf []byte
	for {
		select {
		case <-c.done:
			return
		case <-c.outSig:
		case <-tick.C:
			now = o.Now()
			if c.udpKey != 0 && now.UnixNano()-c.lastReadNs.Load() > int64(o.IdleTimeout) {
				// UDP clients have no TCP read deadline (readLoop): nothing on
				// either path for IdleTimeout.
				c.shutdown(ReasonIdle)
				return
			}
			var ctl []byte
			ctl, acStart, acStop = c.appendModeAC(ctl, acStart, acStop)
			c.sel.mu.Lock()
			start, stop = c.sel.take(start[:0], stop[:0])
			c.sel.mu.Unlock()
			for len(stop) > 0 {
				n := min(len(stop), 1000)
				ctl = appendAddrList(ctl, "stop_sending", stop[:n])
				stop = stop[n:]
			}
			for len(start) > 0 {
				n := min(len(start), 1000)
				ctl = appendAddrList(ctl, "start_sending", start[:n])
				start = start[n:]
			}
			if !now.Before(nextHB) {
				nextHB = now.Add(o.HeartbeatEvery)
				ctl = appendHeartbeat(ctl, now.UnixNano())
			}
			if o.Stats != nil && c.hello.returnStats && !now.Before(nextStats) {
				nextStats = now.Add(o.StatsEvery)
				if cs, ok := o.Stats.ClientStats(c.att.Station); ok {
					ctl = appendStats(ctl, cs)
				}
			}
			if len(ctl) > 0 {
				c.enqueue(ctl, false)
			}
			if c.sess != nil {
				c.outMu.Lock()
				fill := float64(len(c.out)) / float64(c.outLimit)
				c.outMu.Unlock()
				c.sess.SetQueueFill(min(fill, 1))
			}
		}
		c.outMu.Lock()
		if len(c.out) == 0 {
			c.outMu.Unlock()
			continue
		}
		data := c.out
		c.out = c.spare[:0]
		c.outMu.Unlock()
		payload := data
		if c.compress == compressZlib2 {
			wbuf = c.zw.appendFrames(wbuf[:0], data)
			payload = wbuf
		}
		c.nc.SetWriteDeadline(time.Now().Add(o.WriteTimeout))
		_, err := c.nc.Write(payload)
		c.outMu.Lock()
		c.spare = data[:0]
		c.outMu.Unlock()
		if err != nil {
			c.shutdown(ReasonWrite)
			return
		}
	}
}

// ------------------------------------------------- ingest.Controller

// SendFlow implements ingest.Controller: mlat-client has no FLOW record;
// its rate is steered by the selection, so this is a no-op.
func (c *conn) SendFlow(ingest.FlowCmd) error { return nil }

// SendInterest implements ingest.Controller: the traffic mode and the
// planner's interest sets become start_sending / stop_sending.
func (c *conn) SendInterest(_ model.StationIdx, in ingest.InterestCmd) error {
	c.sel.mu.Lock()
	c.sel.interestCmd(in)
	c.sel.mu.Unlock()
	return nil
}

// Disconnect implements ingest.Controller.
func (c *conn) Disconnect(code, reason string, reconnectIn int) {
	if code == "rate_limited" && reconnectIn > 0 {
		c.s.o.Logger.Info("mlat-client disconnected: rate limit (ingest)", "link", c.id, "station", uint32(c.att.Station),
			"coolDownS", reconnectIn, "reason", reason)
		c.s.coolDown(c.key, time.Duration(reconnectIn)*time.Second, c.s.o.Now())
		c.shutdown(ReasonRateLimit)
		return
	}
	c.shutdown(ReasonIngest)
}

var _ ingest.Controller = (*conn)(nil)

func (c *conn) stats() (LinkStats, bool) {
	if !c.authedFlag.Load() {
		return LinkStats{}, false
	}
	l := LinkStats{
		ID: c.id, Station: c.att.Station, FeedID: c.att.FeedID, Admission: c.att.Admission,
		UUID: c.ident.UUID, User: c.ident.User, ClientVersion: c.ident.ClientVersion,
		ClockType: c.hello.clockType, Compression: c.compress, Selective: c.hello.selective,
		ReturnResults: c.hello.returnResults, Privacy: c.ident.Privacy, Located: c.ident.Located,
		IP: c.ip, Since: c.since, LastRead: c.lastRead(), Epoch: c.epoch.Load(),
		Lines: c.lines.Load(), Frames: c.frames.Load(), Syncs: c.syncs.Load(), MLATs: c.mlats.Load(),
		MsgRate: math.Float64frombits(c.rateFrames.Load()), KBps: math.Float64frombits(c.rateKB.Load()),
		Results: c.resOK.Load(), ResultsDrop: c.resDrop.Load(), BadLines: c.bad.Load(),
		SplitSync: c.split, SSyncs: c.ssyncOK.Load(),
		ADSBAircraft: int(c.adsbN.Load()), ADSBRate: math.Float64frombits(c.adsbRate.Load()),
		UDP: c.udpKey != 0, UDPPackets: c.udpPkts.Load(), UDPLost: c.udpLostN.Load(), UDPDup: c.udpDupN.Load(),
		UDPBad: c.udpBadN.Load(), ModeAC: c.modeACWire(), ModeACFrames: c.modeACn.Load(),
	}
	l.ResultFormat = "legacy"
	if c.hello.ecef {
		l.ResultFormat = "ecef"
	}
	c.sel.mu.Lock()
	l.Seen, l.Requested = len(c.sel.seen), len(c.sel.requested)
	l.Mode = ingest.ModeFull
	if c.sel.selective {
		l.Mode = ingest.ModeSelective
	}
	c.sel.mu.Unlock()
	l.ClockClass = c.ident.ClockCls.String()
	return l, true
}
