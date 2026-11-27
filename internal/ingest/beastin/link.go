package beastin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
)

// registryTimeout bounds one Registry.Attach call (an admission). 30 s like
// the AVF admission deadline (SCALE-09: under feeder churn the registry
// lock is busy and a shorter deadline refused admissions).
const registryTimeout = 30 * time.Second

// maxRelaySubs bounds the sub-stations of one trusted relay link.
const maxRelaySubs = 4096

// New relay sub-stations of one link are registered at most at
// relayAttachRate per second (burst relayAttachBurst): each registration
// is a synchronous registry call on the link's reader goroutine.
const (
	relayAttachRate  = 20
	relayAttachBurst = 64
)

// fairnessBatches is how many batches a worker delivers for one link
// before it lets other links go first.
const fairnessBatches = 8

// sub is one station on a link: the link's own station, or a receiver ID
// of a trusted relay. Each has its own decoder, because each is its own
// receiver clock.
type sub struct {
	l        *link
	id       Identity
	att      Attachment
	attached bool
	refused  bool // a relay receiver ID the registry refused: frames dropped
	dec      feed.BeastDecoder

	win rateWin // frame-rate window

	frames, dropped uint64
}

// HandleFrame implements model.FrameSink for the decoder.
func (u *sub) HandleFrame(f model.RawFrame) { u.l.collect(u, f) }

// HandleUpdate implements model.FrameSink (Beast carries no decoded updates).
func (u *sub) HandleUpdate(model.DecodedUpdate) {}

type pingRec struct {
	v    uint32
	at   time.Time
	used bool
}

// link is one TCP connection. Everything except the queue, the published
// stats and shutdown is touched only by the link's reader goroutine.
type link struct {
	s         *Server
	c         net.Conn
	id        uint64
	ip        netip.Addr
	ipKey     string
	connected time.Time

	p    *feed.BeastParser
	own  *sub
	cur  *sub
	subs map[uint64]*sub
	now  time.Time

	uuid       string
	relay      bool
	gotBytes   bool
	lastData   time.Time
	pendingRx  uint64
	hasPending bool

	pingOn   bool
	nextPing time.Time
	pings    [8]pingRec
	pingNext int
	rttLast  time.Duration
	rttMin   time.Duration
	rttAvg   time.Duration

	out      []byte
	lastSlow time.Time
	slowSent uint64
	echoed   uint64
	sent     uint64
	options  uint64
	uuids    uint64
	rxIDs    uint64
	relayIgn uint64
	// attachTok / attachAt: token bucket of relay sub registrations.
	attachTok float64
	attachAt  time.Time
	throttled *sub // stands in for a receiver ID while registrations are throttled

	bytesWin  rateWin
	bytes     uint64
	mlatDrop  uint64
	queueDrop uint64

	// last values added to the server counters
	lastFrames, lastUnknown uint64

	batch *Batch
	dead  bool

	reason atomic.Pointer[string] // set by shutdown / kill

	mu  sync.Mutex
	pub LinkStats

	qmu       sync.Mutex
	q         []*Batch
	scheduled bool
	delivered uint64
}

func newLink(s *Server, c net.Conn, id uint64, ip netip.Addr, now time.Time) *link {
	l := &link{
		s: s, c: c, id: id, ip: ip, connected: now, lastData: now, now: now,
		ipKey: ipKey(s.opt.ServerKey, ip, s.port),
	}
	l.own = l.newSub(Identity{})
	l.cur = l.own
	l.p = feed.NewBeastParser(l.onFrame)
	l.p.OnEscape = l.onEscape
	l.batch = getBatch(s.opt.BatchFrames)
	l.pub = LinkStats{Link: id, Remote: ip.String(), Connected: now}
	return l
}

func (l *link) newSub(id Identity) *sub {
	u := &sub{l: l, id: id}
	u.dec = feed.BeastDecoder{Sink: u}
	u.dec.TS.OnClockEvent = func(ev feed.ClockEvent) {
		l.batch.Events = append(l.batch.Events, ClockEvent{At: len(l.batch.Frames), Station: u.att.Station, Event: ev})
	}
	return u
}

// shutdown closes the link from another goroutine.
func (l *link) shutdown(reason string) {
	l.reason.CompareAndSwap(nil, &reason)
	l.c.Close()
}

// kill closes the link from its own goroutine.
func (l *link) kill(reason string) {
	l.reason.CompareAndSwap(nil, &reason)
	l.dead = true
	l.c.Close()
}

func (l *link) run() {
	o := &l.s.opt
	buf := make([]byte, o.ReadBuffer)
	for !l.dead {
		l.c.SetReadDeadline(l.deadline())
		n, err := l.c.Read(buf)
		l.now = o.Now()
		if n > 0 {
			l.consume(buf[:n])
		}
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) || l.reason.Load() != nil {
				r := ReasonError
				if errors.Is(err, io.EOF) {
					r = ReasonEOF
				}
				l.reason.CompareAndSwap(nil, &r)
				break
			}
		}
		if !l.dead {
			l.tick()
		}
		l.writeOut()
		l.publish()
	}
	l.finish()
}

// deadline returns when the reader must wake up even without data.
func (l *link) deadline() time.Time {
	o := &l.s.opt
	d := l.lastData.Add(o.IdleTimeout)
	if !l.gotBytes {
		d = earlier(d, l.connected.Add(o.HandshakeTimeout))
	} else if !l.own.attached {
		d = earlier(d, l.connected.Add(o.IdentifyWait))
	}
	if l.pingOn {
		d = earlier(d, l.nextPing)
	}
	// time.Now, not Options.Now: the socket deadline is wall-clock based.
	if wait := d.Sub(l.now); l.now.IsZero() || wait < 0 {
		return time.Now()
	} else {
		return time.Now().Add(wait)
	}
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// tick runs the timers: handshake, identification, idle, pings, flush.
func (l *link) tick() {
	o := &l.s.opt
	now := l.now
	switch {
	case !l.gotBytes && now.Sub(l.connected) >= o.HandshakeTimeout:
		l.s.ctr.discSilent.Add(1)
		l.kill(ReasonSilent)
		return
	case now.Sub(l.lastData) >= o.IdleTimeout:
		l.s.ctr.discIdle.Add(1)
		l.kill(ReasonIdle)
		return
	}
	if l.gotBytes && !l.own.attached && now.Sub(l.connected) >= o.IdentifyWait {
		l.attachOwn("")
		if l.dead {
			return
		}
	}
	if l.pingOn && !now.Before(l.nextPing) {
		v := feed.BeastPingValue(now)
		l.pings[l.pingNext] = pingRec{v: v, at: now}
		l.pingNext = (l.pingNext + 1) % len(l.pings)
		l.out = feed.AppendBeastPing(l.out, v)
		l.sent++
		l.nextPing = now.Add(o.PingInterval)
	}
	if l.own.attached {
		l.flush(false)
	}
}

// consume handles one chunk read from the socket.
func (l *link) consume(b []byte) {
	o := &l.s.opt
	l.gotBytes = true
	l.lastData = l.now
	l.bytes += uint64(len(b))
	l.s.ctr.bytes.Add(uint64(len(b)))
	l.byteRate(len(b))
	if l.dead {
		return
	}

	l.p.Write(b)

	if df := l.p.Frames - l.lastFrames; df > 0 {
		l.s.ctr.frames.Add(df)
		l.lastFrames = l.p.Frames
	}
	if du := l.p.Unknown - l.lastUnknown; du > 0 {
		l.s.ctr.unknownEsc.Add(du)
		l.lastUnknown = l.p.Unknown
	}
	if l.dead {
		return
	}
	if !l.own.attached && len(l.batch.Frames) >= o.IdentifyMaxFrames {
		l.attachOwn("")
	}
	if l.own.attached {
		l.flush(false)
	}
}

// rateWin counts events in one-second windows and the run of consecutive
// windows that went over the limit.
type rateWin struct {
	sec      int64
	count    int
	over     bool
	lastOver int64
	run      int // consecutive completed over-limit seconds
}

// add counts n at second sec against limit. It reports whether the window
// is still within the limit and whether this call took it over.
func (w *rateWin) add(sec int64, n, limit int) (ok, crossed bool) {
	if sec != w.sec {
		switch {
		case w.over && w.sec == w.lastOver+1:
			w.run++
		case w.over:
			w.run = 1
		default:
			w.run = 0
		}
		if w.over {
			w.lastOver = w.sec
		}
		w.sec, w.count, w.over = sec, 0, false
	}
	w.count += n
	if w.count > limit {
		crossed = !w.over
		w.over = true
		return false, crossed
	}
	return true, false
}

// byteRate applies the per-link byte limit (scaled by the number of
// stations on a relay link). Bytes are never dropped (that would break the
// framing); the limit leads to 'W' 'S' and, if sustained, a disconnect.
func (l *link) byteRate(n int) {
	pol := l.s.pol.Load()
	if pol.MaxBytesPerSec <= 0 {
		return
	}
	_, crossed := l.bytesWin.add(l.now.Unix(), n, pol.MaxBytesPerSec*max(1, len(l.subs)))
	if crossed {
		l.slowDown()
	}
	if pol.OverLimitSeconds > 0 && l.bytesWin.run >= pol.OverLimitSeconds {
		l.rateKill(pol)
	}
}

// admit applies the per-station frame limit; it reports whether the frame
// may be decoded. Frames over the limit are shed.
func (u *sub) admit(now time.Time) bool {
	l := u.l
	pol := l.s.pol.Load()
	if pol.MaxFPS <= 0 {
		return true
	}
	ok, crossed := u.win.add(now.Unix(), 1, pol.MaxFPS)
	if crossed {
		l.slowDown()
	}
	if pol.OverLimitSeconds > 0 && u.win.run >= pol.OverLimitSeconds {
		l.rateKill(pol)
		return false
	}
	return ok
}

// rateKill disconnects a link that stayed over its limits and cools its
// identity down (DESIGN §4.6: FLOW, then disconnect with a cool-down).
func (l *link) rateKill(pol *policy) {
	if l.dead {
		return
	}
	key := l.ipKey
	if l.own.attached {
		key = l.own.id.Key()
	}
	l.s.coolDown(key, l.now, pol.CoolDown)
	l.s.ctr.discRate.Add(1)
	l.s.opt.Logger.Info("beast link over its rate limit: disconnected", "link", l.id, "key", key, "coolDown", pol.CoolDown)
	l.kill(ReasonRate)
}

// slowDown queues a 'W' 'S' request, at most once per SlowDownEvery.
func (l *link) slowDown() {
	if !l.lastSlow.IsZero() && l.now.Sub(l.lastSlow) < l.s.opt.SlowDownEvery {
		return
	}
	l.lastSlow = l.now
	l.out = feed.AppendBeastOption(l.out, feed.BeastOptSlowDown)
	l.slowSent++
	l.s.ctr.slowDowns.Add(1)
}

// onFrame is the parser's frame callback.
func (l *link) onFrame(fr *feed.BeastFrame) {
	if l.dead {
		return
	}
	u := l.cur
	if u.refused {
		u.dropped++
		return
	}
	if !u.admit(l.now) {
		if !l.dead {
			u.dropped++
			l.s.ctr.rateDropped.Add(1)
		}
		return
	}
	u.dec.Recv = l.now
	u.dec.Handle(fr)
}

// collect receives a decoded frame from a sub's decoder.
func (l *link) collect(u *sub, f model.RawFrame) {
	o := &l.s.opt
	if f.MLAT && !o.AcceptMLATResults {
		l.mlatDrop++
		l.s.ctr.mlatDropped.Add(1)
		return
	}
	if o.StripClock {
		f.ClockCls, f.Epoch, f.Ticks = model.ClockNone, 0, 0
	}
	f.Station = u.att.Station
	f.FeedID = u.att.FeedID
	u.frames++
	l.batch.Frames = append(l.batch.Frames, f)
	if l.own.attached && len(l.batch.Frames) >= o.BatchFrames {
		l.flush(false)
	}
}

// onEscape is the parser's extension-message callback.
func (l *link) onEscape(typ byte, body []byte) {
	if l.dead {
		return
	}
	switch typ {
	case feed.BeastUUID:
		l.uuids++
		u, ok := normalizeUUID(string(body))
		if !ok {
			return
		}
		if !l.own.attached || u != l.uuid {
			l.attachOwn(u)
		}
	case feed.BeastReceiverID:
		l.rxIDs++
		id, ok := feed.ParseBeastReceiverID(body)
		if !ok {
			return
		}
		if !l.own.attached {
			l.pendingRx, l.hasPending = id, true
			return
		}
		l.switchRx(id)
	case feed.BeastPing:
		v, ok := feed.ParseBeastPing(body)
		if !ok {
			return
		}
		if l.matchPing(v) {
			return
		}
		l.out = feed.AppendBeastEscape(l.out, feed.BeastPing, body)
		l.echoed++
		l.s.ctr.pings.Add(1)
	case feed.BeastOption:
		l.options++
		if len(body) == 1 && body[0] == feed.BeastOptPing && !l.pingOn {
			l.pingOn = true
			l.nextPing = l.now
		}
	}
	// Unknown escape types (body == nil) are counted by the parser.
}

// matchPing reports whether v is the echo of one of our pings and records
// the round trip.
func (l *link) matchPing(v uint32) bool {
	for i := range l.pings {
		p := &l.pings[i]
		if p.at.IsZero() || p.used || p.v != v {
			continue
		}
		rtt := l.now.Sub(p.at)
		if rtt < 0 || rtt > time.Minute {
			continue
		}
		p.used = true
		l.rttLast = rtt
		if l.rttMin == 0 || rtt < l.rttMin {
			l.rttMin = rtt
		}
		if l.rttAvg == 0 {
			l.rttAvg = rtt
		} else {
			l.rttAvg += (rtt - l.rttAvg) / 8
		}
		return true
	}
	return false
}

// attachOwn attaches the link's own station: with a UUID, or IP-keyed when
// uuid is "". A link attached before is re-attached (UUID upgrade or
// change); its earlier frames stay with the old station.
func (l *link) attachOwn(uuid string) {
	s := l.s
	pol := s.pol.Load()
	id := Identity{Link: l.id, Kind: model.StationKindBeast, UUID: uuid, IPKey: l.ipKey, RemoteIP: l.ip, Slot: -1}
	if uuid == "" {
		id.Slot = s.takeSlot(l.ip)
	}
	key := id.Key()
	first := !l.own.attached
	fail := func(reason string) {
		if id.Slot >= 0 {
			s.releaseSlot(l.ip, id.Slot)
		}
		l.kill(reason)
	}

	if s.coolingDown(key, l.now) {
		s.ctr.rejCool.Add(1)
		fail(ReasonCoolDown)
		return
	}
	replacing := ""
	if !first {
		replacing = l.own.id.Key()
	}
	if why := s.claimStation(l.ip, key, replacing, pol); why != "" {
		if why == ReasonCloned {
			s.ctr.rejCloned.Add(1)
			s.opt.Logger.Warn("beast link refused: its UUID is live from another address (clone or spoof)", "link", l.id, "key", key)
		} else {
			s.ctr.rejStations.Add(1)
		}
		fail(why)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), registryTimeout)
	att, err := s.reg.Attach(ctx, id)
	cancel()
	if err != nil || !att.Admitted() {
		s.releaseStation(l.ip, key)
		s.coolDown(key, l.now, pol.RefusedCoolDown)
		s.ctr.rejRegistry.Add(1)
		why := att.Reason
		if err != nil {
			why = err.Error()
		}
		s.opt.Logger.Info("beast link refused by the registry", "link", l.id, "key", key, "reason", why)
		fail(ReasonRegistry)
		return
	}

	if first {
		s.attachedFirst(l.ip)
	} else {
		// Frames so far belong to the old station; detach it after they
		// were delivered.
		old, oldAtt, oldKey := l.own.id, l.own.att, l.own.id.Key()
		l.batch.after = append(l.batch.after, func() { s.reg.Detach(old, oldAtt) })
		l.flush(false)
		s.releaseStation(l.ip, oldKey)
		if old.Slot >= 0 {
			s.releaseSlot(l.ip, old.Slot)
		}
		// A new identity is a new receiver as far as the clock goes.
		l.own.dec = feed.BeastDecoder{Sink: l.own}
		u := l.own
		l.own.dec.TS.OnClockEvent = func(ev feed.ClockEvent) {
			l.batch.Events = append(l.batch.Events, ClockEvent{At: len(l.batch.Frames), Station: u.att.Station, Event: ev})
		}
		// Relay sub-stations belong to the old UUID.
		l.detachSubs()
		l.cur = l.own
	}
	l.own.id, l.own.att, l.own.attached = id, att, true
	l.own.dec.FeedID = att.FeedID
	if l.own.dec.TS.Epoch() == 0 {
		l.own.dec.TS.SetEpochBase(att.EpochBase)
	}
	l.uuid = uuid
	l.relay = pol.relayTrusted(uuid, l.ip)
	// Frames and clock events held while waiting for the identity.
	for i := range l.batch.Frames {
		if f := &l.batch.Frames[i]; f.Station == 0 {
			f.Station, f.FeedID = att.Station, att.FeedID
		}
	}
	for i := range l.batch.Events {
		if l.batch.Events[i].Station == 0 {
			l.batch.Events[i].Station = att.Station
		}
	}
	s.opt.Logger.Debug("beast link attached", "link", l.id, "key", key, "station", att.Station, "admission", att.Admission)
	if l.hasPending {
		l.hasPending = false
		l.switchRx(l.pendingRx)
	}
}

// switchRx handles a 0xE3 receiver ID: on a trusted relay the frames that
// follow belong to that receiver's sub-station; elsewhere it is ignored.
func (l *link) switchRx(rx uint64) {
	s := l.s
	if !l.relay {
		l.relayIgn++
		s.ctr.relayIgnored.Add(1)
		return
	}
	u := l.subs[rx]
	if u == nil {
		if len(l.subs) >= maxRelaySubs {
			l.relayIgn++
			s.ctr.relayIgnored.Add(1)
			return
		}
		if !l.takeAttachToken() {
			// Too many new receiver IDs at once: drop this receiver's frames
			// until its next 0xE3, which tries again.
			if l.throttled == nil {
				l.throttled = &sub{l: l, refused: true}
			}
			l.relayIgn++
			s.ctr.relayIgnored.Add(1)
			l.cur = l.throttled
			return
		}
		id := Identity{Link: l.id, Kind: model.StationKindBeast, UUID: l.uuid, IPKey: l.ipKey, RemoteIP: l.ip, Slot: -1, Relay: true, ReceiverID: rx}
		u = l.newSub(id)
		if l.subs == nil {
			l.subs = map[uint64]*sub{}
		}
		l.subs[rx] = u
		if s.coolingDown(id.Key(), l.now) {
			u.refused = true
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), registryTimeout)
			att, err := s.reg.Attach(ctx, id)
			cancel()
			if err != nil || !att.Admitted() {
				u.refused = true
				s.coolDown(id.Key(), l.now, s.pol.Load().RefusedCoolDown)
			} else {
				u.att, u.attached = att, true
				u.dec.FeedID = att.FeedID
				u.dec.TS.SetEpochBase(att.EpochBase)
			}
		}
	}
	l.cur = u
}

// takeAttachToken takes one relay registration token.
func (l *link) takeAttachToken() bool {
	now := l.now
	if l.attachAt.IsZero() {
		l.attachTok, l.attachAt = relayAttachBurst, now
	}
	l.attachTok = min(l.attachTok+now.Sub(l.attachAt).Seconds()*relayAttachRate, relayAttachBurst)
	l.attachAt = now
	if l.attachTok < 1 {
		return false
	}
	l.attachTok--
	return true
}

// detachSubs schedules the detach of every relay sub-station after the
// frames queued so far.
func (l *link) detachSubs() {
	for _, u := range l.subs {
		if u.attached {
			id, att := u.id, u.att
			l.batch.after = append(l.batch.after, func() { l.s.reg.Detach(id, att) })
			u.attached = false
		}
	}
	l.subs = nil
}

// writeOut sends queued pings, echoes and slow-down requests.
func (l *link) writeOut() {
	if len(l.out) == 0 || l.dead {
		l.out = l.out[:0]
		return
	}
	l.c.SetWriteDeadline(time.Now().Add(l.s.opt.WriteTimeout))
	_, err := l.c.Write(l.out)
	l.out = l.out[:0]
	if err != nil {
		l.kill(ReasonWrite)
	}
}

// flush hands the current batch to the delivery queue.
func (l *link) flush(final bool) {
	s := l.s
	b := l.batch
	if !final && len(b.Frames) == 0 && len(b.Events) == 0 && len(b.after) == 0 {
		return
	}
	if n := len(b.Frames); n > 0 && s.opt.Budget != nil && !s.opt.Budget.Take(n) {
		s.ctr.budgetDrp.Add(uint64(n))
		b.Frames, b.Events = b.Frames[:0], b.Events[:0]
		if !final && len(b.after) == 0 {
			return
		}
	}
	b.final = final
	b.l = l
	b.Link = l.id
	l.qmu.Lock()
	if len(l.q) >= s.opt.QueueBatches && !final {
		n := uint64(len(b.Frames))
		l.queueDrop += n
		s.ctr.queueDropped.Add(n)
		if len(b.after) == 0 {
			b.Frames, b.Events = b.Frames[:0], b.Events[:0]
			l.qmu.Unlock()
			return
		}
		// Keep the deferred detaches, drop the frames.
		b.Frames, b.Events = b.Frames[:0], b.Events[:0]
	}
	l.q = append(l.q, b)
	depth := len(l.q)
	sched := !l.scheduled
	l.scheduled = true
	l.qmu.Unlock()
	if sched {
		s.schedule(l)
	}
	if !final {
		l.batch = getBatch(s.opt.BatchFrames)
		if depth*2 > s.opt.QueueBatches {
			l.slowDown()
		}
	} else {
		l.batch = nil
	}
}

// drain delivers queued batches (worker goroutine).
func (l *link) drain() {
	s := l.s
	for i := 0; ; i++ {
		l.qmu.Lock()
		if len(l.q) == 0 {
			l.scheduled = false
			l.qmu.Unlock()
			return
		}
		if i >= fairnessBatches {
			l.qmu.Unlock()
			s.schedule(l) // still scheduled; go to the back of the run queue
			return
		}
		b := l.q[0]
		l.q[0] = nil
		l.q = l.q[1:]
		l.qmu.Unlock()

		if len(b.Frames) > 0 || len(b.Events) > 0 {
			b.MapStatsOnly = true
			s.sink.HandleBeast(b)
			n := uint64(len(b.Frames))
			s.ctr.delivered.Add(n)
			l.qmu.Lock()
			l.delivered += n
			l.qmu.Unlock()
		}
		for _, f := range b.after {
			f()
		}
		final := b.final
		putBatch(b)
		if final {
			l.qmu.Lock()
			l.scheduled = false
			l.qmu.Unlock()
			s.linkGone(l)
			return
		}
	}
}

// finish runs when the reader stops: detach everything after the last
// frames, release the accounting and hand over the final batch.
func (l *link) finish() {
	s := l.s
	l.c.Close()
	reason := ReasonError
	if r := l.reason.Load(); r != nil {
		reason = *r
	}
	l.detachSubs()
	if l.own.attached {
		id, att := l.own.id, l.own.att
		key := id.Key()
		if id.Slot >= 0 {
			s.releaseSlot(l.ip, id.Slot)
		}
		l.batch.after = append(l.batch.after, func() {
			s.reg.Detach(id, att)
			s.releaseStation(l.ip, key)
		})
	}
	l.mu.Lock()
	l.pub.Closed = true
	l.pub.CloseReason = reason
	l.mu.Unlock()
	l.publish()
	wasUnattached := !l.own.attached
	if wasUnattached {
		// Frames held for an identity that never came are not delivered.
		l.batch.Frames, l.batch.Events = l.batch.Frames[:0], l.batch.Events[:0]
	}
	l.flush(true)
	s.readerDone(l, wasUnattached)
	s.opt.Logger.Debug("beast link closed", "link", l.id, "reason", reason, "bytes", l.bytes)
}

// --- batch pool ---------------------------------------------------------------

var batchPool sync.Pool

func getBatch(n int) *Batch {
	if b, ok := batchPool.Get().(*Batch); ok && cap(b.Frames) >= n {
		return b
	}
	return &Batch{Frames: make([]model.RawFrame, 0, n)}
}

func putBatch(b *Batch) {
	clear(b.Frames) // drop the Data references
	b.Frames = b.Frames[:0]
	b.Events = b.Events[:0]
	clear(b.after)
	b.after = b.after[:0]
	b.final, b.l, b.Link, b.MapStatsOnly = false, nil, 0, false
	batchPool.Put(b)
}
