package avfsrv

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/avf"
	"airview/internal/feed"
	"airview/internal/ingest"
	"airview/internal/model"
	"airview/internal/registry"
)

// conn is one feeder connection.
type conn struct {
	s       *Server
	nc      net.Conn
	ip      netip.Addr
	started time.Time

	// set by the handshake (read-only afterwards)
	c             *avf.Conn
	feeder        string
	body          avf.HelloBody
	pub           []byte
	status        string // WELCOME status
	resumed       bool
	returnResults bool
	cloneTok      uint64
	sess          *ingest.Session

	seq       *avf.Sequencer
	processed atomic.Uint64 // watermark of records the worker finished
	q         chan item
	qBytes    atomic.Int64

	mu         sync.Mutex
	rx         map[uint8]*rxState
	backfill   atomic.Bool // inside BACKFILL begin..end (set by the worker)
	lastClaim  time.Time
	closing    atomic.Bool
	graceful   atomic.Bool // the client said GOODBYE: no resume grace
	authed     bool
	nFrames    atomic.Uint64
	dropped    atomic.Uint64
	lastWire   uint64
	shutdownMu sync.Once
	done       chan struct{} // closed when the connection has ended
	hg         hostGuard     // host-time plausibility (ROB-1, hostguard.go)

	// endMu / endWhy / endIn: why the session ended (the first cause wins),
	// logged with "feeder disconnected" (SO-8).
	endMu  sync.Mutex
	endWhy string
	endIn  int // reconnectIn of a server-side close (s)
}

// setEnd records why the session ends unless a cause is known already.
func (c *conn) setEnd(why string, reconnectIn int) {
	c.endMu.Lock()
	if c.endWhy == "" {
		c.endWhy, c.endIn = why, reconnectIn
	}
	c.endMu.Unlock()
}

// rxState is one receiver (station) of a connection.
type rxState struct {
	rxNo       uint8
	station    model.StationIdx
	feedID     string
	cls        model.ClockClass
	epoch      uint32
	timestamps bool
	name       string
	status     string // registry.StatusApproved / StatusPending / StatusDenied
	connID     int64
	counted    bool // counted in the per-IP station limit
	attached   bool // attached to the ingest session
	mode       string
	results    uint64          // MLAT fixes sent to this station (c.mu)
	mins       *stationMinutes // received counts per minute (minutes.go); set at admission
}

// item is one seq-carrying record queued for the worker; the payload is a
// private copy.
type item struct {
	typ avf.RecordType
	seq uint64
	buf *[]byte
	at  int64 // server time the record was read (Unix ns)
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

func getBuf(n int) *[]byte {
	b := bufPool.Get().(*[]byte)
	if cap(*b) < n {
		*b = make([]byte, 0, max(n, 4096))
	}
	*b = (*b)[:n]
	return b
}

func putBuf(b *[]byte) {
	if cap(*b) <= avf.MaxRecordLen+64 {
		bufPool.Put(b)
	}
}

// maxQueueBytes bounds the payload bytes queued per connection.
const maxQueueBytes = 4 << 20

// serve runs one connection from TCP accept to close.
func (c *conn) serve() {
	s := c.s
	defer s.wg.Done()
	defer close(c.done)
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		s.lim.release(c.ip, !c.authed)
	}()
	if !c.handshake() {
		return
	}
	defer c.cleanup()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	c.q = make(chan item, s.o.QueueBatches)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); c.worker() }()
	go func() { defer wg.Done(); _ = c.c.RunServerKeepalive(ctx, c.processed.Load) }()
	go func() { defer wg.Done(); c.monitor(ctx) }()
	c.restoreReceivers()
	err := c.readLoop()
	switch {
	case c.closing.Load():
	case err == nil:
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
		c.setEnd("closed by the feeder (EOF)", 0)
	case errors.Is(err, avf.ErrProtocol):
		c.setEnd("protocol error: "+err.Error(), 0)
		s.st.protoErr.Add(1)
		s.o.Logger.Info("avfsrv: protocol error", "feeder", c.feeder, "err", err)
		_ = c.c.Goodbye(avf.CodeProtocolError, "protocol error", 0)
	case errors.Is(err, avf.ErrIdle):
		c.setEnd("idle timeout", 0)
		s.o.Logger.Info("avfsrv: session idle", "feeder", c.feeder)
	default:
		c.setEnd("read error: "+errNoAddr(err), 0)
		s.o.Logger.Debug("avfsrv: session ended", "feeder", c.feeder, "err", err)
	}
	close(c.q)
	cancel()
	_ = c.c.Close()
	wg.Wait()
}

// handshake runs TLS + HELLO, the clone rule and the feeder admission, and
// sends WELCOME. It reports whether the session is established.
func (c *conn) handshake() bool {
	s := c.s
	pol := s.policy()
	p, err := avf.ServerHandshake(s.ctx, c.nc, s.o.TLSConfig, avf.ServerConfig{
		ServerID: s.o.ServerID, Name: pol.Name, Admission: pol.Admission,
		HandshakeTimeout: time.Duration(pol.Limits.HandshakeTimeoutSec) * time.Second,
		Session:          avf.ConnOptions{Guard: s.o.Guard, Now: s.o.Now, Uncompressed: true}, Now: s.o.Now,
	})
	if err != nil {
		s.st.handshakeFail.Add(1)
		var he *avf.HandshakeError
		if errors.As(err, &he) {
			s.reason(he.Code)
		} else {
			s.reason("handshake")
		}
		return false
	}
	c.authed = true
	s.lim.authenticated(c.ip)
	deny := func(code, reason string, in int) bool {
		s.reason(code)
		_ = p.Deny(code, reason, in)
		return false
	}
	c.feeder, c.body, c.pub = p.FeederID, p.Body, p.Pub
	now := s.o.Now()

	s.mu.Lock()
	until, cooled := s.cooldown[c.feeder]
	s.mu.Unlock()
	if cooled && now.Before(until) {
		return deny(avf.CodeRateLimited, "the station exceeded its rate limit; wait before reconnecting", int(math.Ceil(until.Sub(now).Seconds())))
	}

	// Clone rule (AVF.md §9.2).
	d, tok := s.o.CloneGuard.Acquire(c.feeder, c.ip)
	if d == registry.CloneRejected {
		return deny(avf.CodeClonedFeeder, "this feeder identity is connected from another address; run 'airview-feeder new-identity' on the copy", 0)
	}
	c.cloneTok = tok
	ok := false
	defer func() {
		if !ok {
			s.o.CloneGuard.Release(c.feeder, tok)
		}
	}()

	// Feeder-level admission; the per-receiver decision follows with
	// registry.Admit when the RECEIVER records arrive.
	status, code, reason := c.feederAdmission(pol)
	if status == avf.StatusDenied {
		return deny(code, reason, 0)
	}
	c.status = status

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return deny(avf.CodeShuttingDown, "server restart", 15)
	}
	old := s.byFeeder[c.feeder]
	if re, okr := s.resume[c.body.ResumeToken]; okr && c.body.ResumeToken != "" && re.feeder == c.feeder && now.Before(re.expires) {
		c.resumed = true
		delete(s.resume, c.body.ResumeToken)
	}
	tokNew := newToken()
	s.resume[tokNew] = resumeEntry{feeder: c.feeder, expires: now.Add(avf.ResumeTokenTTL)}
	s.mu.Unlock()
	if old != nil && old != c {
		// The old session ends first, so its processed watermark is final
		// before WELCOME reports it (no record is processed twice).
		old.shutdown(avf.CodeReplaced, "a newer session of this feeder took over", 0)
		select {
		case <-old.done:
		case <-time.After(3 * time.Second):
		}
	}

	selective := slices.Contains(p.Features, avf.FeatSelective)
	mode := avf.ModeFull
	if selective && s.ing.DesiredMode() == ingest.ModeSelective {
		mode = avf.ModeSelective
	}
	c.returnResults = pol.ReturnResults && slices.Contains(p.Features, avf.FeatResults)
	ack := s.o.Watermarks.Begin(c.feeder, c.body.SeqEpoch)
	conn, err := p.Accept(avf.Welcome{Status: status, SessionID: newToken()[:16], ResumeToken: tokNew, AckSeq: ack,
		Config: avf.WelcomeConfig{BatchMs: int(avf.DefaultBatch / time.Millisecond), Mode: mode,
			MLAT: pol.MLAT, ReturnResults: c.returnResults}, MOTD: pol.MOTD})
	if err != nil {
		s.o.Watermarks.End(c.feeder, c.body.SeqEpoch, ack, now)
		return false
	}
	c.c = conn
	c.seq = avf.NewSequencer(ack)
	c.processed.Store(ack)
	c.sess = s.ing.OpenSession(ingest.SessionInfo{Proto: "avf", Label: c.feeder, Controller: c, Selective: selective})
	s.mu.Lock()
	s.byFeeder[c.feeder] = c
	s.mu.Unlock()
	s.st.accepted.Add(1)
	s.o.Logger.Info("avfsrv: feeder connected", "feeder", c.feeder, "status", status, "software", c.body.Software,
		"version", c.body.Version, "resumed", c.resumed, "ack", ack)
	ok = true
	return true
}

// feederAdmission decides the WELCOME status before any receiver is known
// (DESIGN §3): revoked feeders are refused; a known approved feeder is
// approved; otherwise the admission policy applies (invite: a valid enrol
// token, which is consumed by the first receiver's registry.Admit).
func (c *conn) feederAdmission(pol Policy) (status, code, reason string) {
	s := c.s
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	f, known, err := s.reg.Feeder(ctx, c.feeder)
	if err != nil {
		s.o.Logger.Warn("avfsrv: registry lookup failed", "feeder", c.feeder, "err", err)
		return avf.StatusDenied, avf.CodeInternal, "temporary server error"
	}
	switch {
	case known && f.State == registry.FeederRevoked:
		return avf.StatusDenied, avf.CodeRevoked, "this feeder has been revoked by the network operator"
	case known && f.State == registry.FeederApproved:
		return avf.StatusApproved, "", ""
	}
	switch pol.Admission {
	case model.AdmissionOpen:
		return avf.StatusApproved, "", ""
	case model.AdmissionApprove:
		return avf.StatusPending, "", ""
	}
	// invite
	if c.body.EnrolToken == "" {
		if known { // pending under an earlier policy: keep it waiting
			return avf.StatusPending, "", ""
		}
		return avf.StatusDenied, avf.CodeEnrolRequired, "this network is invite-only: ask the operator for an enrol token"
	}
	valid, err := s.reg.CheckToken(ctx, c.body.EnrolToken)
	if err != nil {
		return avf.StatusDenied, avf.CodeInternal, "temporary server error"
	}
	if !valid {
		return avf.StatusDenied, avf.CodeEnrolInvalid, "the enrol token is unknown, expired, revoked or used up"
	}
	return avf.StatusApproved, "", ""
}

// readLoop reads records, applies the seq rule and queues seq-carrying
// records for the worker in order.
func (c *conn) readLoop() error {
	s := c.s
	for {
		rec, err := c.c.ReadRecord()
		if err != nil {
			return err
		}
		switch rec.Type {
		case avf.TPong, avf.TPing, avf.TAck:
			continue // PONG feeds the clock estimator inside ReadRecord
		case avf.TGoodbyeC:
			var g avf.Goodbye
			_ = avf.DecodeJSON(rec.Payload, &g)
			c.graceful.Store(true)
			c.setEnd("feeder goodbye: "+g.Code, 0)
			s.o.Logger.Info("avfsrv: feeder said goodbye", "feeder", c.feeder, "code", g.Code)
			return nil
		case avf.TClaim:
			var cl avf.Claim
			if err := avf.DecodeJSON(rec.Payload, &cl); err != nil {
				return err
			}
			c.claim(cl.RxNo)
			continue
		}
		if !rec.Type.HasSeq() {
			continue
		}
		seq, _, err := avf.SplitSeq(rec.Payload)
		if err != nil {
			return err
		}
		fresh, err := c.seq.Accept(seq)
		if err != nil {
			return err
		}
		if !fresh {
			continue
		}
		b := getBuf(len(rec.Payload))
		copy(*b, rec.Payload)
		c.qBytes.Add(int64(len(*b)))
		for c.qBytes.Load() > maxQueueBytes && len(c.q) > 0 {
			time.Sleep(time.Millisecond) // back-pressure: TCP slows the feeder down
		}
		c.q <- item{typ: rec.Type, seq: seq, buf: b, at: s.o.Now().UnixNano()}
	}
}

// worker processes the queued records in order and advances the processed
// watermark (what ACK reports).
func (c *conn) worker() {
	s := c.s
	var fr avf.Frames
	var raw []model.RawFrame
	for it := range c.q {
		p := *it.buf
		var err error
		switch it.typ {
		case avf.TReceiver:
			var r avf.Receiver
			if _, err = avf.DecodeSeqJSON(p, &r); err == nil {
				if err = r.Validate(); err == nil {
					c.receiver(r)
				}
			}
		case avf.TFrames:
			if err = fr.Decode(p); err == nil {
				raw = c.frames(&fr, raw[:0], it.at)
			}
		case avf.TClockEvent:
			var ev avf.ClockEvent
			if _, err = avf.DecodeSeqJSON(p, &ev); err == nil {
				c.clockEvent(ev)
			}
		case avf.TClockPair:
			var cp avf.ClockPair
			if err = cp.Decode(p); err == nil {
				if r := c.rxLookup(cp.RxNo); r != nil {
					s.ing.ClockPair(r.station, ingest.ClockPair{Epoch: cp.EpochID, Ticks: cp.Ticks, HostUTCNs: cp.HostUTCNs, HostErrEstNs: cp.HostErrEstNs})
				}
			}
		case avf.TClockStatus:
			var cs avf.ClockStatus
			if err = cs.Decode(p); err == nil {
				if r := c.rxLookup(cs.RxNo); r != nil {
					s.ing.ClockStatus(r.station, ingest.ClockStatus{HostNs: cs.HostNs, Settings: cs.Settings,
						GPSStatus: cs.GPSStatus, PPSOffsetTicks: cs.PPSOffsetTicks, Raw: cs.Raw})
				}
			}
		case avf.TInputStatus:
			var is avf.InputStatus
			if _, err = avf.DecodeSeqJSON(p, &is); err == nil && is.RxNo >= 1 && is.RxNo <= 255 {
				if r := c.rxLookup(uint8(is.RxNo)); r != nil && s.o.Hooks.OnInputStatus != nil {
					s.o.Hooks.OnInputStatus(r.station, is)
				}
			}
		case avf.TGNSSPos:
			var g avf.GNSSPos
			if _, err = avf.DecodeSeqJSON(p, &g); err == nil && g.RxNo >= 1 && g.RxNo <= 255 && c.c.HasFeature(avf.FeatGNSS) {
				if r := c.rxLookup(uint8(g.RxNo)); r != nil && s.o.Hooks.OnGNSS != nil {
					s.o.Hooks.OnGNSS(r.station, g)
				}
			}
		case avf.TDecoded:
			var rxNo uint8
			var ups []avf.DecodedUpdate
			if _, rxNo, ups, err = avf.DecodeDecoded(p); err == nil {
				c.decoded(rxNo, ups)
			}
		case avf.TMinuteStats:
			var m avf.MinuteStats
			if _, err = avf.DecodeSeqJSON(p, &m); err == nil && m.RxNo >= 1 && m.RxNo <= 255 {
				if r := c.rxLookup(uint8(m.RxNo)); r != nil {
					now := s.o.Now()
					late := c.backfill.Load() || m.Minute < now.Unix()-120
					c.mu.Lock()
					mins := r.mins
					c.mu.Unlock()
					if s.o.Hooks.OnStationMinute != nil && mins != nil {
						// merge first: OnMinuteStats may keep m
						sm := mins.addExact(r.station, m, late, now.UnixNano())
						s.st.minutes.Add(1)
						s.o.Hooks.OnStationMinute(sm)
					}
					if s.o.Hooks.OnMinuteStats != nil {
						s.o.Hooks.OnMinuteStats(r.station, m, late)
					}
				}
			}
		case avf.TBackfill:
			var b avf.Backfill
			if _, err = avf.DecodeSeqJSON(p, &b); err == nil {
				c.backfill.Store(b.Phase == avf.BackfillBegin)
			}
		}
		c.qBytes.Add(-int64(len(p)))
		putBuf(it.buf)
		s.st.records.Add(1)
		if err != nil {
			// A malformed record ends the session; keep the watermark below
			// it so it is not acknowledged.
			s.st.protoErr.Add(1)
			s.o.Logger.Info("avfsrv: bad record", "feeder", c.feeder, "type", it.typ.String(), "err", err)
			c.shutdown(avf.CodeProtocolError, "malformed "+it.typ.String()+" record", 0)
			for it := range c.q { // drain
				c.qBytes.Add(-int64(len(*it.buf)))
				putBuf(it.buf)
			}
			return
		}
		c.processed.Store(it.seq)
	}
}

func (c *conn) rxLookup(rxNo uint8) *rxState {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rx[rxNo]
	if r == nil || r.status == registry.StatusDenied || r.station == 0 {
		return nil
	}
	return r
}

func (c *conn) rxOf(st model.StationIdx) (uint8, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n, r := range c.rx {
		if r.station == st {
			return n, true
		}
	}
	return 0, false
}

// receiver handles a RECEIVER record: the first one of a receiver in this
// session admits it as a station; later ones update its clock class, epoch
// and timestamp consent.
func (c *conn) receiver(r avf.Receiver) {
	s := c.s
	rxNo := uint8(r.RxNo)
	c.mu.Lock()
	cur := c.rx[rxNo]
	c.mu.Unlock()
	if cur != nil && cur.attached {
		c.mu.Lock()
		cur.cls, cur.epoch, cur.timestamps, cur.name = r.Class(), r.EpochID, r.Timestamps, r.Name
		c.mu.Unlock()
		s.ing.SetTimestamps(cur.station, r.Timestamps)
		if s.o.Hooks.OnReceiver != nil {
			s.o.Hooks.OnReceiver(cur.station, r)
		}
		return
	}
	if cur != nil && cur.status == registry.StatusDenied {
		return // refused earlier in this session
	}
	st := &rxState{rxNo: rxNo, cls: r.Class(), epoch: r.EpochID, timestamps: r.Timestamps, name: r.Name}
	if c.admit(st) && s.o.Hooks.OnReceiver != nil {
		s.o.Hooks.OnReceiver(st.station, r)
	}
}

// admit runs registry.Admit for a receiver and attaches the station.
func (c *conn) admit(st *rxState) bool {
	s := c.s
	// 30 s: under feeder churn the registry's write queue can be long; the
	// admission overtakes queued bulk writes (registry writelock.go) but
	// must not give up behind the one in progress (finding SCALE-09).
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	res, err := s.reg.Admit(ctx, registry.AdmitRequest{Kind: model.StationKindAVF, FeederPub: c.pub, RxNo: int(st.rxNo),
		UUID: c.body.UUID, EnrolToken: c.body.EnrolToken, Name: st.name, ClockCls: st.cls})
	if err != nil {
		s.o.Logger.Warn("avfsrv: admission failed", "feeder", c.feeder, "rx", st.rxNo, "err", err)
		st.status = registry.StatusDenied
		c.setRx(st)
		_ = c.notice(avf.CodeInternal, avf.LevelError, "the server could not register this receiver; it will retry on reconnect")
		return false
	}
	if !res.Allowed() {
		st.status = registry.StatusDenied
		c.setRx(st)
		s.reason(res.Code)
		_ = c.notice(res.Code, avf.LevelError, res.Reason)
		if c.attachedCount() == 0 && (res.Code == registry.CodeRevoked || res.Code == registry.CodeEnrolInvalid ||
			res.Code == registry.CodeEnrolRequired || res.Code == registry.CodeLimitStations) {
			c.shutdown(res.Code, res.Reason, 0)
		}
		return false
	}
	if !s.lim.addStation(c.ip) {
		st.status = registry.StatusDenied
		c.setRx(st)
		s.reason(rejectStation)
		_ = c.notice(avf.CodeLimitStations, avf.LevelError, "too many stations from this address")
		return false
	}
	st.counted = true
	st.station, st.status = res.Station.Idx, res.Status
	st.feedID = model.StationFeedID(res.Station.SID, int(st.rxNo))
	if s.o.Hooks.OnStationMinute != nil {
		st.mins = s.minutes.get(st.station)
	}
	if id, err := s.reg.ConnOpen(ctx, st.station, c.ip, model.StationKindAVF); err == nil {
		st.connID = id
	}
	s.ing.SetTimestamps(st.station, st.timestamps)
	st.mode = c.sess.AddStation(st.station)
	st.attached = true
	c.setRx(st)
	s.mu.Lock()
	prev := s.byStation[st.station]
	s.byStation[st.station] = c
	s.mu.Unlock()
	if prev != nil && prev != c {
		prev.detachStation(st.station)
	}
	s.reg.Seen(st.station, s.o.Now())
	if s.o.Hooks.OnOnline != nil {
		s.o.Hooks.OnOnline(st.station, true)
	}
	if st.status == registry.StatusPending {
		_ = c.notice(avf.NoticePending, avf.LevelInfo, "this station waits for the network operator's approval; its data is kept for its own statistics only")
	}
	if st.mode == ingest.ModeSelective && c.c.HasFeature(avf.FeatSelective) {
		_ = c.c.WriteJSON(avf.TInterest, &avf.Interest{RxNo: int(st.rxNo), Mode: avf.ModeSelective, ReduceMs: 500, Reset: true})
	}
	return true
}

func (c *conn) setRx(st *rxState) {
	c.mu.Lock()
	c.rx[st.rxNo] = st
	c.mu.Unlock()
}

func (c *conn) attachedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.rx {
		if r.attached {
			n++
		}
	}
	return n
}

// detachStation forgets a station that moved to another connection.
func (c *conn) detachStation(st model.StationIdx) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.rx {
		if r.station == st {
			r.attached = false
			r.status = registry.StatusDenied
		}
	}
}

// restoreReceivers re-admits the receivers of the previous session of the
// same seqEpoch, so records it replays before its RECEIVER records find
// their station (AVF.md §3.5, §5.2 strategy (a)).
func (c *conn) restoreReceivers() {
	s := c.s
	key := c.feeder + "|" + c.body.SeqEpoch
	s.mu.Lock()
	sv, ok := s.saved[key]
	delete(s.saved, key)
	s.mu.Unlock()
	if !ok {
		return
	}
	for _, r := range sv.rx {
		c.admit(&rxState{rxNo: r.rxNo, cls: r.cls, epoch: r.epoch, timestamps: r.timestamps, name: r.name})
	}
}

// frames normalises a FRAMES record and hands it to the ingest.
func (c *conn) frames(fr *avf.Frames, raw []model.RawFrame, at int64) []model.RawFrame {
	s := c.s
	r := c.rxLookup(fr.RxNo)
	if r == nil {
		s.st.dropped.Add(uint64(len(fr.Frames)))
		c.dropped.Add(uint64(len(fr.Frames)))
		return raw
	}
	c.mu.Lock()
	cls, timestamps, mode, st, feedID, mins := r.cls, r.timestamps, r.mode, r.station, r.feedID, r.mins
	c.mu.Unlock()
	prio := ingest.PrioNormal
	if mode == ingest.ModeSelective {
		prio = ingest.PrioEssential
	}
	if !s.ing.Budget().TakePrio(len(fr.Frames), prio) {
		s.st.shed.Add(uint64(len(fr.Frames)))
		return raw
	}
	timed := cls.Timed() && timestamps
	for i := range fr.Frames {
		f := &fr.Frames[i]
		var kind model.FrameKind
		switch len(f.Data) {
		case 2:
			kind = model.FrameModeAC
		case 7:
			kind = model.FrameModeSShort
		default:
			kind = model.FrameModeSLong
		}
		rf := model.RawFrame{Kind: kind, Data: f.Data, Station: st, FeedID: feedID, Channel: int(f.Channel) - 1,
			Signal: feed.BeastSignalDBFS(f.Sig), HostNs: f.HostNs, Late: f.Late || c.backfill.Load(), GPSNs: -1, Epoch: fr.EpochID}
		if timed && !f.TsInvalid && f.Ticks != 0 {
			rf.ClockCls, rf.Ticks = cls, f.Ticks
			switch {
			case cls.GPS():
				rf.GPSNs = int64(f.Ticks % uint64(model.NsPerDay))
			case cls.NominalHz() == 12e6:
				rf.Clock12MHz = f.Ticks & (1<<48 - 1)
				if feed.IsMLATTimestamp(rf.Clock12MHz) {
					s.st.mlatMagic.Add(1)
					continue // feeders must never forward MLAT results (AVF.md §5.5)
				}
			}
		}
		raw = append(raw, rf)
	}
	c.hg.observe(raw, at)
	if mins != nil {
		mins.countFrames(raw, mode == ingest.ModeSelective, at)
	}
	c.nFrames.Add(uint64(len(raw)))
	s.st.frames.Add(uint64(len(raw)))
	s.ing.Submit(raw, false)
	return raw
}

// clockEvent forwards a CLOCK_EVENT and tracks the receiver's epoch.
func (c *conn) clockEvent(ev avf.ClockEvent) {
	if ev.RxNo < 1 || ev.RxNo > 255 {
		return
	}
	r := c.rxLookup(uint8(ev.RxNo))
	if r == nil {
		return
	}
	late := c.backfill.Load()
	if !late {
		c.mu.Lock()
		r.epoch = ev.NewEpoch
		c.mu.Unlock()
		c.s.ing.ClockEvent(r.station, ingest.ClockEvent{OldEpoch: ev.OldEpoch, NewEpoch: ev.NewEpoch,
			Reason: ev.Reason, SamplesLost: ev.SamplesLost, HostNs: ev.HostNs})
	}
}

// decoded hands DECODED updates (untimed, map only) to the tracker.
func (c *conn) decoded(rxNo uint8, ups []avf.DecodedUpdate) {
	s := c.s
	r := c.rxLookup(rxNo)
	if r == nil {
		return
	}
	for i := range ups {
		u := &ups[i]
		addr, non, ok := avf.ParseICAO(u.ICAO)
		if !ok || u.TMs <= 0 {
			continue
		}
		if non {
			addr |= 1 << 24
		}
		du := model.DecodedUpdate{Recv: time.UnixMilli(u.TMs), FeedID: r.feedID, ICAO: addr, Callsign: u.CS, Squawk: u.Sq,
			Lat: u.Lat, Lon: u.Lon, GS: u.GS, Track: u.Trk, OnGround: u.Gnd, Signal: u.Sig, Msgs: max(u.N, 1),
			Station: r.station, Late: c.backfill.Load()}
		if u.AltBaro != nil {
			du.AltBaro = model.F(float64(*u.AltBaro))
		}
		if u.AltGeom != nil {
			du.AltGeom = model.F(float64(*u.AltGeom))
		}
		if u.VR != nil {
			du.VertRate = model.F(float64(*u.VR))
		}
		if (du.Lat == nil) != (du.Lon == nil) {
			du.Lat, du.Lon = nil, nil
		}
		s.ing.HandleUpdate(du)
		s.st.decoded.Add(1)
	}
}

// claim answers a CLAIM with a CLAIM_CODE (at most one per 10 s).
func (c *conn) claim(rxNo int) {
	s := c.s
	if rxNo < 1 || rxNo > 255 || !c.c.HasFeature(avf.FeatClaim) {
		return
	}
	now := s.o.Now()
	c.mu.Lock()
	r := c.rx[uint8(rxNo)]
	limited := now.Sub(c.lastClaim) < 10*time.Second
	if !limited {
		c.lastClaim = now
	}
	c.mu.Unlock()
	switch {
	case limited:
		_ = c.notice("rate_limited", avf.LevelWarn, "claim codes can be requested every 10 s")
		return
	case r == nil || r.station == 0 || r.status == registry.StatusDenied:
		_ = c.notice(avf.CodeDenied, avf.LevelWarn, "this receiver is not registered on the network")
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	code, exp, err := s.reg.MintClaim(ctx, r.station)
	if err != nil {
		_ = c.notice(avf.CodeDenied, avf.LevelWarn, err.Error())
		return
	}
	s.st.claims.Add(1)
	_ = c.c.WriteJSON(avf.TClaimCode, &avf.ClaimCode{RxNo: rxNo, Code: code, ExpiresMs: exp.UnixMilli()})
}

func (c *conn) notice(code, level, text string) error {
	return c.c.WriteJSON(avf.TNotice, &avf.Notice{Code: code, Level: level, Text: text})
}

// monitor reports the link state to the ingest session once a second and
// watches admission changes every 5 s.
func (c *conn) monitor(ctx context.Context) {
	s := c.s
	t := time.NewTicker(time.Second)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n++
		if rtt, off, ok := c.c.Clock(); ok {
			c.sess.SetHostOffset(c.hostOffset(off, rtt), rtt)
		}
		st := c.c.Stats()
		c.sess.AddBytes(int(st.In.WireBytes - c.lastWire))
		c.lastWire = st.In.WireBytes
		fill := float64(len(c.q)) / float64(cap(c.q))
		fill = math.Max(fill, float64(c.qBytes.Load())/maxQueueBytes)
		c.sess.SetQueueFill(fill)
		s.o.Watermarks.Update(c.feeder, c.body.SeqEpoch, c.processed.Load())
		if n%5 == 0 {
			c.checkAdmission()
		}
	}
}

// checkAdmission notices admin decisions about the connected stations:
// pending -> approved (NOTICE approved), approved -> revoked or denied
// (GOODBYE).
func (c *conn) checkAdmission() {
	s := c.s
	now := s.o.Now()
	type change struct {
		st     model.StationIdx
		status string
	}
	var ch []change
	c.mu.Lock()
	for _, r := range c.rx {
		if !r.attached {
			continue
		}
		s.reg.Seen(r.station, now)
		if s.o.Resolver == nil {
			continue
		}
		m, ok := s.o.Resolver.Info(r.station)
		if !ok {
			continue
		}
		switch {
		case m.Admission == model.AdmApproved && r.status == registry.StatusPending:
			r.status = registry.StatusApproved
			ch = append(ch, change{r.station, model.AdmApproved})
		case m.Admission == model.AdmRevoked || m.Admission == model.AdmDenied:
			ch = append(ch, change{r.station, m.Admission})
		}
	}
	c.mu.Unlock()
	for _, x := range ch {
		switch x.status {
		case model.AdmApproved:
			_ = c.notice(avf.NoticeApproved, avf.LevelInfo, "your station was approved")
		case model.AdmRevoked:
			c.shutdown(avf.CodeRevoked, "this station has been revoked by the network operator", 0)
			return
		default:
			c.shutdown(avf.CodeDenied, "this station was refused by the network operator", 0)
			return
		}
	}
}

// shutdown says GOODBYE and closes the connection (once).
func (c *conn) shutdown(code, reason string, reconnectIn int) {
	c.shutdownMu.Do(func() {
		why := code
		if reason != "" {
			why += ": " + reason
		}
		c.setEnd("server: "+why, reconnectIn)
		c.closing.Store(true)
		if code == avf.CodeReplaced {
			c.graceful.Store(true) // the new session takes the stations over
		}
		if c.c != nil {
			go func() { _ = c.c.Goodbye(code, reason, reconnectIn) }()
		} else {
			c.nc.Close()
		}
	})
}

// cleanup releases everything a session held.
func (c *conn) cleanup() {
	s := c.s
	now := s.o.Now()
	wm := c.processed.Load()
	s.o.Watermarks.End(c.feeder, c.body.SeqEpoch, wm, now)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := c.c.Stats()
	c.mu.Lock()
	rxs := make([]*rxState, 0, len(c.rx))
	for _, r := range c.rx {
		rxs = append(rxs, r)
	}
	c.mu.Unlock()
	sort.Slice(rxs, func(i, j int) bool { return rxs[i].rxNo < rxs[j].rxNo })
	var saved savedRx
	attached := 0
	for _, r := range rxs {
		if r.attached {
			attached++
		}
	}
	for _, r := range rxs {
		if r.counted {
			s.lim.dropStation(c.ip)
		}
		if !r.attached {
			continue
		}
		saved.rx = append(saved.rx, rxSaved{rxNo: r.rxNo, cls: r.cls, epoch: r.epoch, timestamps: r.timestamps, name: r.name})
		if r.connID != 0 {
			_ = s.reg.ConnClose(ctx, r.connID, int64(st.In.WireBytes)/int64(max(attached, 1)))
		}
		s.mu.Lock()
		if s.byStation[r.station] == c {
			delete(s.byStation, r.station)
		}
		s.mu.Unlock()
		if s.o.Hooks.OnOnline != nil {
			s.o.Hooks.OnOnline(r.station, false)
		}
	}
	grace := s.o.ResumeGrace
	if c.graceful.Load() {
		grace = 0
	}
	c.sess.Close(grace)
	s.mu.Lock()
	if s.byFeeder[c.feeder] == c {
		delete(s.byFeeder, c.feeder)
	}
	if len(saved.rx) > 0 {
		saved.expires = now.Add(avf.WatermarkTTL)
		s.saved[c.feeder+"|"+c.body.SeqEpoch] = saved
	}
	s.mu.Unlock()
	s.o.CloneGuard.Release(c.feeder, c.cloneTok)
	c.endMu.Lock()
	why, in := c.endWhy, c.endIn
	c.endMu.Unlock()
	if why == "" {
		why = "connection closed"
	}
	args := []any{"feeder", c.feeder, "reason", why, "frames", c.nFrames.Load(),
		"watermark", wm, "up", now.Sub(c.started).Round(time.Second)}
	if in > 0 {
		args = append(args, "reconnectIn", time.Duration(in)*time.Second)
	}
	s.o.Logger.Info("avfsrv: feeder disconnected", args...)
}

// ------------------------------------------------------------ ingest.Controller

// SendFlow implements ingest.Controller.
func (c *conn) SendFlow(f ingest.FlowCmd) error {
	return c.c.WriteJSON(avf.TFlow, &avf.Flow{BatchMs: f.BatchMs, Pause: f.Pause, SlowFactor: f.SlowFactor})
}

// SendInterest implements ingest.Controller.
func (c *conn) SendInterest(st model.StationIdx, in ingest.InterestCmd) error {
	if !c.c.HasFeature(avf.FeatSelective) {
		return nil
	}
	c.mu.Lock()
	var rxNo uint8
	for n, r := range c.rx {
		if r.station == st && r.attached {
			rxNo = n
			r.mode = in.Mode
		}
	}
	c.mu.Unlock()
	if rxNo == 0 {
		return nil
	}
	out := avf.Interest{RxNo: int(rxNo), Mode: in.Mode, ReduceMs: in.ReduceMs, Reset: in.Reset,
		SyncAdd: icaos(in.SyncAdd), SyncDel: icaos(in.SyncDel), MLATAdd: icaos(in.MLATAdd), MLATDel: icaos(in.MLATDel)}
	return c.c.WriteJSON(avf.TInterest, &out)
}

// Disconnect implements ingest.Controller.
func (c *conn) Disconnect(code, reason string, reconnectIn int) {
	if code == avf.CodeRateLimited && reconnectIn > 0 {
		s := c.s
		now := s.o.Now()
		s.mu.Lock()
		prev, cooling := s.cooldown[c.feeder]
		s.cooldown[c.feeder] = now.Add(time.Duration(reconnectIn) * time.Second)
		s.mu.Unlock()
		s.reason(avf.CodeRateLimited)
		if !cooling || !prev.After(now) { // once per cool-down
			s.o.Logger.Warn("avfsrv: feeder over its rate limit: disconnected with a cool-down",
				"feeder", c.feeder, "reason", reason, "reconnectIn", time.Duration(reconnectIn)*time.Second)
		}
	}
	c.shutdown(code, reason, reconnectIn)
}

func icaos(keys []uint32) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = avf.FormatICAO(k&0xFFFFFF, k&(1<<24) != 0)
	}
	return out
}

// info returns the admin view of the session.
func (c *conn) info() SessionInfo {
	st := c.c.Stats()
	si := SessionInfo{FeederID: c.feeder, Remote: c.ip.String(), Software: c.body.Software, Version: c.body.Version,
		OS: c.body.OS, Arch: c.body.Arch, Status: c.status, StartedMs: c.started.UnixMilli(), RecordsIn: st.RecordsIn,
		WireBytes: st.In.WireBytes, Features: c.c.Features(), Resumed: c.resumed, SeqEpoch: c.body.SeqEpoch,
		Watermark: c.processed.Load(), FramesIn: c.nFrames.Load(), DroppedIn: c.dropped.Load()}
	if rtt, off, ok := c.c.Clock(); ok {
		si.RTTMs, si.OffsetMs = float64(rtt)/1e6, float64(off)/1e6
	}
	si.HostClampMs = float64(c.hg.adj.Load()) / 1e6
	if c.q != nil {
		si.QueueFill = float64(len(c.q)) / float64(cap(c.q))
	}
	c.mu.Lock()
	si.Backfill = c.backfill.Load()
	for _, r := range c.rx {
		si.Stations = append(si.Stations, SessionStation{Station: r.station, RxNo: int(r.rxNo), Status: r.status,
			Class: r.cls.String(), Epoch: r.epoch, Mode: r.mode, Results: r.results})
		si.Results += r.results
	}
	c.mu.Unlock()
	sort.Slice(si.Stations, func(i, j int) bool { return si.Stations[i].RxNo < si.Stations[j].RxNo })
	return si
}

// errNoAddr describes a read error without the socket addresses a
// *net.OpError carries: logs name no raw client IPs (DESIGN §8.4).
func errNoAddr(err error) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Op + ": " + op.Err.Error()
	}
	return err.Error()
}

// addResults counts n MLAT fixes sent to station st on this session.
func (c *conn) addResults(st model.StationIdx, n uint64) {
	c.mu.Lock()
	for _, r := range c.rx {
		if r.station == st {
			r.results += n
			break
		}
	}
	c.mu.Unlock()
}
