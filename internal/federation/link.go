package federation

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/avf"
)

// Session timing.
const (
	pingInterval  = 5 * time.Second
	idleTimeout   = 30 * time.Second
	writeTimeout  = 30 * time.Second
	streamBatch   = 50 * time.Millisecond
	maxDelayBytes = 64 << 20 // bound of a delayed peer's FIFO
)

// outRec is one queued record.
type outRec struct {
	t  RecordType
	p  []byte
	at time.Time // enqueue time (delay)
}

// link is one established session with a peer.
type link struct {
	n        *Node
	p        *peer
	tc       *tls.Conn
	info     PeerInfo
	outbound bool
	dialer   string // node ID of the side that dialled
	since    time.Time

	w  *avf.StreamWriter
	sr *avf.StreamReader
	rr *recordReader

	q         chan outRec
	done      chan struct{}
	closeOnce sync.Once
	closeErr  atomic.Value // string

	// Export state: touched only by the export goroutine.
	lastSent   map[uint32]int64
	lastFullMs int64
	transitSeq uint64
	forceFull  atomic.Bool

	rttNs   atomic.Int64
	pingSeq atomic.Uint64

	acOut, acIn, mlatOut, mlatIn atomic.Uint64
	recOut, recIn, dropped       atomic.Uint64
	rejected                     atomic.Uint64
}

// deadlineConn sets a write deadline before writes (at most once a second).
type deadlineConn struct {
	c       net.Conn
	mu      sync.Mutex
	lastSet time.Time
}

func (d *deadlineConn) Write(p []byte) (int, error) {
	d.mu.Lock()
	now := time.Now()
	if now.Sub(d.lastSet) > time.Second {
		_ = d.c.SetWriteDeadline(now.Add(writeTimeout))
		d.lastSet = now
	}
	d.mu.Unlock()
	return d.c.Write(p)
}

func newLink(n *Node, p *peer, tc *tls.Conn, info PeerInfo, outbound bool) *link {
	l := &link{n: n, p: p, tc: tc, info: info, outbound: outbound, since: n.now(),
		q: make(chan outRec, n.cfg.QueueRecords), done: make(chan struct{}), lastSent: map[uint32]int64{}}
	l.dialer = info.NodeID
	if outbound {
		l.dialer = n.id
	}
	g := avf.Guard{MaxRate: n.cfg.MaxInflateBps, RateWindow: 10 * time.Second, MaxRatio: 200, RatioBlock: 1 << 20}
	l.w = avf.NewStreamWriter(&deadlineConn{c: tc}, streamBatch)
	l.sr = avf.NewStreamReader(tc, g)
	l.rr = newRecordReader(l.sr)
	l.forceFull.Store(true)
	return l
}

// writeNow writes a control record immediately (any goroutine).
func (l *link) writeNow(t RecordType, p []byte) error {
	if err := l.w.WriteRecord(avf.RecordType(t), p); err != nil {
		return err
	}
	l.recOut.Add(1)
	if t.control() {
		return l.w.Flush()
	}
	return nil
}

func (l *link) writeJSON(t RecordType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return l.writeNow(t, b)
}

// enqueue queues a data record without blocking. A full queue drops it and
// schedules a full refresh.
func (l *link) enqueue(t RecordType, p []byte) bool {
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case l.q <- outRec{t: t, p: p, at: l.n.now()}:
		return true
	default:
		l.dropped.Add(1)
		l.forceFull.Store(true)
		return false
	}
}

// close ends the session (idempotent). bye, when non-empty, is sent first.
func (l *link) close(bye, reason string) {
	l.closeOnce.Do(func() {
		if reason != "" {
			l.closeErr.Store(reason)
		}
		if bye != "" {
			_ = l.writeJSON(TGoodbye, &Goodbye{Code: bye, Reason: reason})
		}
		_ = l.tc.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = l.w.Close()
		_ = l.tc.Close()
		close(l.done)
	})
}

func (l *link) reason() string {
	if s, ok := l.closeErr.Load().(string); ok {
		return s
	}
	return ""
}

// run serves the session until it ends; it blocks.
func (l *link) run() {
	send := l.p.cfg.Send
	_ = l.writeJSON(TInfo, &Info{Name: l.n.cfg.Name, Software: Software, Version: l.n.opts.Version,
		Aircraft: send.Aircraft, MLAT: send.MLAT, Transit: send.Transit && l.n.cfg.Transit,
		IntervalMs: l.n.cfg.IntervalMs, DelayS: send.DelayS})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		l.writer()
	}()
	go func() { // node shutdown ends the session
		defer wg.Done()
		select {
		case <-l.n.ctx.Done():
			l.close(ByeShutdown, "")
		case <-l.done:
		}
	}()
	err := l.reader()
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
		l.close("", "")
	case errors.Is(err, ErrProtocol), errors.Is(err, avf.ErrProtocol):
		l.n.stats.protoErrors.Add(1)
		l.close(ByeProtocol, err.Error())
	default:
		l.close("", err.Error())
	}
	wg.Wait()
}

func (l *link) reader() error {
	var lastArm time.Time
	for {
		if now := time.Now(); now.Sub(lastArm) > time.Second {
			_ = l.tc.SetReadDeadline(now.Add(idleTimeout))
			lastArm = now
		}
		t, p, err := l.rr.next()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return errors.New("federation: peer idle")
			}
			select {
			case <-l.done:
				return nil
			default:
			}
			return err
		}
		l.recIn.Add(1)
		switch t {
		case TPing:
			id, ns, err := decodePing(p)
			if err != nil {
				return err
			}
			if err := l.writeNow(TPong, appendPing(nil, id, ns)); err != nil {
				return err
			}
		case TPong:
			_, ns, err := decodePing(p)
			if err != nil {
				return err
			}
			if rtt := time.Now().UnixNano() - ns; rtt >= 0 && rtt < int64(time.Minute) {
				l.rttNs.Store(rtt)
			}
		case TInfo:
			var in Info
			if err := json.Unmarshal(p, &in); err != nil {
				return perr("INFO: %v", err)
			}
			l.p.setInfo(in)
		case TGoodbye:
			var g Goodbye
			_ = json.Unmarshal(p, &g)
			l.closeErr.Store("peer said goodbye: " + cleanStr([]byte(g.Code)))
			return nil
		case TState:
			if err := l.n.onState(l, p); err != nil {
				return err
			}
		case TMLAT:
			if err := l.n.onMLAT(l, p); err != nil {
				return err
			}
		}
	}
}

func (l *link) writer() {
	delay := time.Duration(l.p.cfg.Send.DelayS) * time.Second
	pingT := time.NewTicker(pingInterval)
	defer pingT.Stop()
	var fifo []outRec
	fifoBytes := 0
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	write := func(r outRec) bool {
		if err := l.writeNow(r.t, r.p); err != nil {
			l.close("", "write: "+err.Error())
			return false
		}
		return true
	}
	ping := func() bool {
		id := l.pingSeq.Add(1)
		if err := l.writeNow(TPing, appendPing(nil, id, time.Now().UnixNano())); err != nil {
			l.close("", "write: "+err.Error())
			return false
		}
		return true
	}
	if !ping() {
		return
	}
	for {
		select {
		case <-l.done:
			return
		case <-pingT.C:
			if !ping() {
				return
			}
		case r := <-l.q:
			if delay <= 0 {
				if !write(r) {
					return
				}
				continue
			}
			fifo = append(fifo, r)
			fifoBytes += len(r.p)
			for fifoBytes > maxDelayBytes && len(fifo) > 1 {
				fifoBytes -= len(fifo[0].p)
				fifo = fifo[1:]
				l.dropped.Add(1)
			}
		case <-timer.C:
		}
		if delay > 0 && len(fifo) > 0 {
			now := l.n.now()
			for len(fifo) > 0 && !fifo[0].at.Add(delay).After(now) {
				if !write(fifo[0]) {
					return
				}
				fifoBytes -= len(fifo[0].p)
				fifo[0] = outRec{}
				fifo = fifo[1:]
			}
			if len(fifo) > 0 {
				timer.Reset(fifo[0].at.Add(delay).Sub(now))
			}
		}
	}
}
