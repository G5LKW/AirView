package uplink

import (
	"context"
	"errors"
	"slices"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
)

// session is one established AVF/1 connection. The record stream itself
// (DEFLATE, flush rules, inflate guards, automatic PONG, the 30 s PING/ACK
// liveness) is avf.Conn; the session decides what to send and in which
// order, and numbers the seq records.
type session struct {
	c       *Client
	conn    *avf.Conn
	feats   []string
	nextSeq uint64 // writer goroutine only
	buf     []byte

	readErr chan error

	announced map[int]uint32 // rx -> epoch announced in this session

	bfTokens  float64
	bfLast    time.Time
	liveBytes int64
	rateStart time.Time
	liveRate  float64 // bytes/s of live records (EWMA)
}

// connectOnce dials, performs the AVF/1 handshake and runs the session
// until it ends. up is how long the session was established.
func (c *Client) connectOnce(ctx context.Context) (up time.Duration, err error) {
	c.mu.Lock()
	cfg := c.cfg.Clone()
	if c.rotate {
		c.seqEpoch = avf.NewSeqEpoch()
		c.q.resetSeqs()
		c.rotate = false
	}
	seqEpoch := c.seqEpoch
	resume := ""
	if c.resume != "" && time.Since(c.resumeAt) < 10*time.Minute {
		resume = c.resume
	}
	// The token is sent until the server we last talked to accepted it
	// (a single-use token must not be presented twice).
	tok := ""
	if cfg.EnrolToken != "" && c.st.Enrolled[c.st.ServerID] != tokenHash(cfg.EnrolToken) {
		tok = cfg.EnrolToken
	}
	c.mu.Unlock()

	tcfg, err := avf.ClientTLSConfig(cfg.Host, cfg.ServerPin, nil)
	if err != nil {
		return 0, err
	}
	hctx, cancel := context.WithTimeout(ctx, c.o.HandshakeTO)
	defer cancel()
	raw, err := c.o.Dial(hctx, "tcp", cfg.Addr())
	if err != nil {
		return 0, err
	}
	c.setState(StateHandshake, "", nil)
	feats := slices.Clone(avf.AllFeatures)
	if !cfg.ReturnMLAT {
		feats = slices.DeleteFunc(feats, func(f string) bool { return f == avf.FeatResults })
	}
	if !cfg.ShareGNSS {
		feats = slices.DeleteFunc(feats, func(f string) bool { return f == avf.FeatGNSS })
	}
	conn, res, err := avf.ClientHandshake(ctx, raw, tcfg, avf.ClientConfig{
		Identity: c.id.AVF,
		Body: avf.HelloBody{UUID: c.id.UUID, Software: c.o.Software, Version: c.o.Version, Features: feats,
			SeqEpoch: seqEpoch, ResumeToken: resume, EnrolToken: tok},
		HandshakeTimeout: c.o.HandshakeTO,
		Session:          avf.ConnOptions{IdleTimeout: c.o.Liveness},
	})
	if res != nil && res.HelloS.ServerID != "" {
		c.mu.Lock()
		c.stat.serverID, c.stat.serverName, c.stat.admission = res.HelloS.ServerID, res.HelloS.Name, res.HelloS.Admission
		c.mu.Unlock()
	}
	if err != nil {
		var de *avf.DeniedError
		switch {
		case errors.As(err, &de):
			code := de.Welcome.Code
			if code == "" {
				code = avf.CodeDenied
			}
			return 0, &ServerError{Code: code, Reason: de.Welcome.Reason, ReconnectIn: de.Welcome.ReconnectIn, Welcome: true}
		case errors.Is(err, avf.ErrProtoVersion):
			return 0, &ServerError{Code: avf.CodeProtoVersion, Reason: err.Error() + "; update this software"}
		}
		return 0, err
	}
	w := res.Welcome
	s := &session{c: c, conn: conn, feats: res.Features, readErr: make(chan error, 1),
		announced: map[int]uint32{}, bfLast: time.Now(), rateStart: time.Now()}

	c.mu.Lock()
	sid := res.HelloS.ServerID
	if c.st.ServerID != sid || tok != "" {
		c.st.ServerID = sid
		if tok != "" {
			c.st.Enrolled[sid] = tokenHash(tok)
		}
		if err := saveState(c.dir, c.st); err != nil {
			c.o.Logf("uplink %s: state: %v", cfg.ID, err)
		}
	}
	trusted := w.AckSeq <= c.q.lastSeq
	if !trusted {
		// The server holds more of this seq space than we ever sent: carry
		// on above its watermark and use a fresh seq space next time.
		c.rotate = true
		c.o.Logf("uplink %s: server watermark %d is above our last seq %d; renumbering", cfg.ID, w.AckSeq, c.q.lastSeq)
	}
	c.q.connect(w.AckSeq, trusted, c.o.Now())
	s.nextSeq = w.AckSeq + 1
	c.resume, c.resumeAt = w.ResumeToken, time.Now()
	st := &c.stat
	st.approval, st.feederID, st.sessionID = w.Status, w.FeederID, w.SessionID
	st.mode, st.mlat, st.returnResults, st.motd = w.Config.Mode, w.Config.MLAT, w.Config.ReturnResults, w.MOTD
	st.features = res.Features
	st.connectedAt = c.o.Now()
	st.ackSeq = w.AckSeq
	st.paused, st.slow = false, 1
	if st.mode == "" {
		st.mode = avf.ModeFull
	}
	c.flow = flowState{batchMs: DefaultBatchMs, slow: 1}
	if w.Config.BatchMs >= 10 && w.Config.BatchMs <= 5000 {
		c.flow.batchMs = w.Config.BatchMs
	}
	sel := w.Config.Mode == avf.ModeSelective && slices.Contains(res.Features, avf.FeatSelective)
	c.tmpl.selective, c.tmpl.slow = sel, 1
	for _, rx := range c.rx {
		rx.filt.selective, rx.filt.slow = sel, 1
	}
	c.side = nil
	c.sess = s
	c.mu.Unlock()
	c.ctr.sessions.Add(1)
	c.setState(StateConnected, "", nil)
	c.o.Logf("uplink %s: connected to %s (%s, %s mode)", cfg.ID, cfg.Addr(), w.Status, st.mode)

	start := time.Now()
	err = s.run(ctx)
	up = time.Since(start)
	_ = conn.Close()

	c.mu.Lock()
	// every seq written in this session (also RECEIVER and side records,
	// which are not held) bounds the watermark the server can report next
	c.q.lastSeq = max(c.q.lastSeq, s.nextSeq-1)
	c.sess = nil
	c.side = nil
	c.q.disconnect()
	c.flow.paused, c.flow.slow = false, 1
	c.tmpl.slow = 1
	for _, rx := range c.rx {
		rx.filt.slow = 1
	}
	for _, cw := range c.claims {
		cw.ch <- claimResult{err: errors.New("connection closed")}
	}
	c.claims = nil
	c.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		c.o.Logf("uplink %s: disconnected after %s: %v", cfg.ID, up.Round(time.Second), err)
	}
	return up, err
}

// run is the writer loop.
func (s *session) run(ctx context.Context) error {
	go s.readLoop()
	c := s.c
	c.mu.Lock()
	nos := make([]int, 0, len(c.rxByNo))
	for n := range c.rxByNo {
		nos = append(nos, n)
	}
	slices.Sort(nos)
	// Queued live data may still belong to an older epoch (an epoch change
	// shortly before the reconnect): announce the epoch of each receiver's
	// first queued record, the queued CLOCK_EVENTs then lead to the current.
	firstEpoch := map[int]uint32{}
	for i := c.q.live.head; i < len(c.q.live.items); i++ {
		r := c.q.live.items[i]
		if r.rxNo == 0 {
			continue
		}
		if _, ok := firstEpoch[r.rxNo]; ok {
			continue
		}
		switch r.typ {
		case avf.TFrames, avf.TReceiver:
			if r.epoch != 0 {
				firstEpoch[r.rxNo] = r.epoch
			}
		case avf.TClockEvent:
			firstEpoch[r.rxNo] = r.oldEpoch
		}
	}
	first := make([]*rec, 0, len(nos))
	for _, n := range nos {
		rx := c.rxByNo[n]
		ep, cls := rx.epoch, rx.class
		if e, ok := firstEpoch[n]; ok && e != 0 && e != rx.epoch {
			if k, ok := rx.classes[e]; ok {
				ep, cls = e, k
			}
		}
		first = append(first, c.receiverRecordEpochLocked(rx, ep, cls))
	}
	c.mu.Unlock()
	for _, r := range first {
		if err := s.writeNew(r); err != nil {
			return err
		}
	}
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		if err := s.pump(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return s.shutdown()
		case err := <-s.readErr:
			return err
		case <-c.wake:
		case <-t.C:
		}
	}
}

// writeSeq writes a seq record with the given seq.
func (s *session) writeSeq(t avf.RecordType, seq uint64, body []byte) error {
	s.buf = append(avf.AppendSeq(s.buf[:0], seq), body...)
	if err := s.conn.WriteRecord(t, s.buf); err != nil {
		return err
	}
	s.c.ctr.recordsSent.Add(1)
	return nil
}

// writeNew writes a record with the next seq, not kept for retransmission
// (fresh RECEIVER records, side records).
func (s *session) writeNew(r *rec) error {
	seq := s.nextSeq
	s.nextSeq++
	s.noteAnnounce(r)
	return s.writeSeq(r.typ, seq, r.body)
}

func (s *session) isAnnounced(rxNo int) bool {
	_, ok := s.announced[rxNo]
	return ok
}

func (s *session) noteAnnounce(r *rec) {
	if r.typ == avf.TReceiver || r.typ == avf.TClockEvent {
		s.announced[r.rxNo] = r.epoch
	}
}

// allowed reports whether a record type may be sent with the effective
// features.
func (s *session) allowed(t avf.RecordType) bool {
	switch t {
	case avf.TDecoded:
		return slices.Contains(s.feats, avf.FeatDecoded)
	case avf.TMinuteStats:
		return slices.Contains(s.feats, avf.FeatMinuteStats)
	case avf.TGNSSPos:
		return slices.Contains(s.feats, avf.FeatGNSS)
	}
	return true
}

// pump writes what is due: side records, live records, then (rate-limited)
// backfill. avf.Conn flushes per AVF §4.1 (batch interval, 32 KiB, control
// records).
func (s *session) pump() error {
	c := s.c
	now := time.Now()

	c.mu.Lock()
	side := c.side
	c.side = nil
	paused := c.flow.paused
	var keep, sendSide []*rec
	for _, r := range side {
		switch {
		case paused && r.typ != avf.TClockPair && r.typ != avf.TInputStatus:
			keep = append(keep, r)
		case r.typ == avf.TClockPair && s.announced[r.rxNo] != r.epoch:
			// epoch not announced in this session yet: drop (next in 5 s)
		case r.rxNo != 0 && !s.isAnnounced(r.rxNo):
			// A receiver that appeared after the session started: its
			// RECEIVER record is still in the live queue, and a receiver's
			// first record must be RECEIVER. Send this after it.
			keep = append(keep, r)
		case !s.allowed(r.typ):
		default:
			sendSide = append(sendSide, r)
		}
	}
	c.side = append(keep, c.side...)
	c.mu.Unlock()
	for _, r := range sendSide {
		if err := s.writeNew(r); err != nil {
			return err
		}
	}
	if paused {
		return nil
	}

	var out []*rec
	bytes := 0
	c.mu.Lock()
	for bytes < 256<<10 && len(out) < 512 {
		r := c.q.live.popFront()
		if r == nil {
			break
		}
		if !s.allowed(r.typ) {
			continue
		}
		c.q.sent(r, s.nextSeq)
		s.nextSeq++
		out = append(out, r)
		bytes += len(r.body)
	}
	more := c.q.live.len() > 0
	c.mu.Unlock()
	for _, r := range out {
		s.noteAnnounce(r)
		if err := s.writeSeq(r.typ, r.seq, r.body); err != nil {
			return err
		}
		if r.typ == avf.TFrames {
			c.ctr.framesSent.Add(uint64(r.frames))
		}
	}
	s.liveBytes += int64(bytes)
	if d := now.Sub(s.rateStart); d >= time.Second {
		r := float64(s.liveBytes) / d.Seconds()
		s.liveRate += 0.3 * (r - s.liveRate)
		s.liveBytes, s.rateStart = 0, now
	}
	if more {
		c.notify()
	} else if err := s.backfill(now); err != nil {
		return err
	}
	c.ctr.bytesWire.Store(s.conn.Stats().Out.WireBytes)
	return nil
}

// backfill sends one chunk of backfill wrapped in BACKFILL begin/end, at
// most 25 % of the link (a third of the live rate, at least BackfillMinBps).
func (s *session) backfill(now time.Time) error {
	c := s.c
	if !slices.Contains(s.feats, avf.FeatBackfill) || len(s.announced) == 0 {
		return nil // no receiver announced in this session yet
	}
	rate := max(float64(c.o.BackfillMinBps), s.liveRate/3)
	s.bfTokens = min(s.bfTokens+now.Sub(s.bfLast).Seconds()*rate, max(rate, 64<<10))
	s.bfLast = now
	if s.bfTokens <= 0 {
		return nil
	}
	limit := min(int(s.bfTokens)+1, 60<<10)
	var out []*rec
	bytes, frames := 0, 0
	var fromSeq, toSeq uint64
	var first, last int64
	c.mu.Lock()
	if !c.q.hasBackfill() {
		c.mu.Unlock()
		return nil
	}
	for bytes < limit && len(out) < 256 {
		r := c.q.nextBackfill()
		if r == nil {
			break
		}
		if !s.allowed(r.typ) {
			if r.fromSpool {
				// not sendable: keep it in order (seq 0) so the spool's ack
				// position passes it with the next ACK
				c.q.inflight.push(r)
			}
			continue
		}
		if r.typ == avf.TFrames && !r.marked {
			b, n, err := markLate(r.body)
			if err != nil {
				c.o.Logf("uplink %s: dropping an unreadable backfill record: %v", c.uid, err)
				if r.fromSpool {
					c.q.inflight.push(r)
				}
				continue
			}
			r.body, r.frames, r.marked = b, n, true
		}
		r.late = true
		out = append(out, r)
		bytes += len(r.body)
		if r.typ == avf.TFrames {
			frames += r.frames
		}
		if r.origSeq != 0 {
			if fromSeq == 0 || r.origSeq < fromSeq {
				fromSeq = r.origSeq
			}
			toSeq = max(toSeq, r.origSeq)
		}
		if first == 0 || r.hostNs < first {
			first = r.hostNs
		}
		last = max(last, r.hostNs)
	}
	if len(out) == 0 {
		c.mu.Unlock()
		return nil
	}
	beginSeq := s.nextSeq
	s.nextSeq++
	for _, r := range out {
		c.q.sent(r, s.nextSeq)
		s.nextSeq++
	}
	endSeq := s.nextSeq
	s.nextSeq++
	c.mu.Unlock()

	mk := func(phase string) []byte {
		b, _ := avf.AppendJSON(nil, avf.Backfill{Phase: phase, FromSeq: fromSeq, ToSeq: toSeq, SpanNs: last - first})
		return b
	}
	if err := s.writeSeq(avf.TBackfill, beginSeq, mk(avf.BackfillBegin)); err != nil {
		return err
	}
	for _, r := range out {
		if err := s.writeSeq(r.typ, r.seq, r.body); err != nil {
			return err
		}
	}
	if err := s.writeSeq(avf.TBackfill, endSeq, mk(avf.BackfillEnd)); err != nil {
		return err
	}
	c.ctr.framesLate.Add(uint64(frames))
	c.ctr.framesSent.Add(uint64(frames))
	s.bfTokens -= float64(bytes)
	return nil
}

// shutdown lets the server acknowledge what is queued (up to 2 s), then
// says GOODBYE and closes.
func (s *session) shutdown() error {
	c := s.c
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.pump(); err != nil {
			return err
		}
		_ = s.conn.Flush()
		c.mu.Lock()
		empty := c.q.inflight.len() == 0 && c.q.live.len() == 0
		c.mu.Unlock()
		if empty {
			break
		}
		select {
		case err := <-s.readErr:
			return err
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = s.conn.Goodbye(avf.CodeShuttingDown, "uplink stopping", 0)
	return context.Canceled
}

// readLoop reads the server's records until an error. PING is answered by
// avf.Conn; a silent server ends the session with avf.ErrIdle.
func (s *session) readLoop() {
	for {
		rec, err := s.conn.ReadRecord()
		if err == nil {
			err = s.dispatch(rec.Type, rec.Payload)
		}
		if err != nil {
			select {
			case s.readErr <- err:
			default:
			}
			return
		}
	}
}

func (s *session) dispatch(typ avf.RecordType, p []byte) error {
	c := s.c
	switch typ {
	case avf.TAck:
		seq, err := avf.DecodeAck(p)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.q.ack(seq, c.o.Now())
		c.stat.ackSeq = seq
		c.mu.Unlock()
	case avf.TInterest:
		var in avf.Interest
		if err := avf.DecodeJSON(p, &in); err != nil {
			return err
		}
		c.applyInterest(&in)
	case avf.TFlow:
		var fl avf.Flow
		if err := avf.DecodeJSON(p, &fl); err != nil {
			return err
		}
		c.applyFlow(&fl)
	case avf.TMLATResults:
		fixes, err := avf.DecodeMLATResults(nil, p)
		if err != nil {
			return err
		}
		c.deliverResults(fixes)
	case avf.TFeedback:
		var fb avf.Feedback
		if err := avf.DecodeJSON(p, &fb); err != nil {
			return err
		}
		c.mu.Lock()
		if rx := c.rxByNo[fb.RxNo]; rx != nil {
			rx.feedback = &fb
		}
		c.mu.Unlock()
	case avf.TClaimCode:
		var cc avf.ClaimCode
		if err := avf.DecodeJSON(p, &cc); err != nil {
			return err
		}
		c.claimDone(cc.RxNo, claimResult{code: cc})
	case avf.TNotice:
		var n avf.Notice
		if err := avf.DecodeJSON(p, &n); err != nil {
			return err
		}
		c.addNotice(n)
	case avf.TGoodbyeS:
		var g avf.Goodbye
		_ = avf.DecodeJSON(p, &g)
		if g.Code == "" {
			g.Code = avf.CodeInternal
		}
		return &ServerError{Code: g.Code, Reason: g.Reason, ReconnectIn: g.ReconnectIn}
	}
	return nil
}

// applyInterest applies INTEREST to one receiver or (rxNo 0) to all.
func (c *Client) applyInterest(in *avf.Interest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil && !slices.Contains(c.sess.feats, avf.FeatSelective) && in.Mode == avf.ModeSelective {
		in.Mode = ""
	}
	if in.RxNo == 0 {
		c.tmpl.apply(in)
		for _, rx := range c.rx {
			rx.filt.apply(in)
		}
	} else if rx := c.rxByNo[in.RxNo]; rx != nil {
		rx.filt.apply(in)
	}
	if in.Mode != "" {
		c.stat.mode = in.Mode
	}
}

// applyFlow applies FLOW back-pressure.
func (c *Client) applyFlow(fl *avf.Flow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flow.paused = fl.Pause
	if fl.BatchMs >= 10 && fl.BatchMs <= 5000 {
		c.flow.batchMs = fl.BatchMs
		if c.sess != nil {
			c.sess.conn.SetBatch(time.Duration(fl.BatchMs) * time.Millisecond)
		}
	}
	slow := fl.SlowFactor
	if !(slow > 0 && slow <= 1) {
		slow = 1
	}
	c.flow.slow = slow
	c.tmpl.slow = slow
	for _, rx := range c.rx {
		rx.filt.slow = slow
	}
	c.stat.paused, c.stat.slow = fl.Pause, slow
	c.notify()
}

// deliverResults injects MLAT results into the local tracker (never
// forwarded: FeedID "uplink/<id>", MLAT set) and hands them to OnResults.
func (c *Client) deliverResults(fixes []model.MLATFix) {
	c.ctr.resultsIn.Add(uint64(len(fixes)))
	c.mu.Lock()
	ret := c.cfg.ReturnMLAT
	c.mu.Unlock()
	if !ret {
		return
	}
	if c.o.OnResults != nil {
		c.o.OnResults(c.uid, fixes)
	}
	if c.o.Inject == nil {
		return
	}
	now := c.o.Now()
	for i := range fixes {
		if !model.Displayable(fixes[i].Grade) {
			continue
		}
		c.o.Inject.HandleUpdate(FixUpdate(&fixes[i], FeedPrefix+c.uid, now))
		c.ctr.resultsInjected.Add(1)
	}
}

func (c *Client) claimDone(rxNo int, r claimResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range c.claims {
		if w.rxNo == rxNo || rxNo == 0 {
			w.ch <- r
			c.claims = slices.Delete(c.claims, i, i+1)
			return
		}
	}
}

func (c *Client) addNotice(n avf.Notice) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch n.Code {
	case "approved":
		c.stat.approval = avf.StatusApproved
	case "pending":
		c.stat.approval = avf.StatusPending
	case avf.CodeRateLimited:
		for _, w := range c.claims {
			w.ch <- claimResult{err: errors.New("claim rate limited by the server")}
		}
		c.claims = nil
	}
	c.stat.notices = append(c.stat.notices, NoticeStatus{Code: n.Code, Level: n.Level, Text: n.Text, AtMs: c.o.Now().UnixMilli()})
	if len(c.stat.notices) > 10 {
		c.stat.notices = c.stat.notices[len(c.stat.notices)-10:]
	}
}
