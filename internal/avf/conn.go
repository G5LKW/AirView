package avf

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// ConnOptions tunes a record session. Zero values select the defaults.
type ConnOptions struct {
	// Batch is the writer's flush interval (default 50 ms; the client uses
	// WELCOME config.batchMs when this is 0). Negative = flush every record.
	Batch time.Duration
	// Guard limits decompression of incoming data (default DefaultGuard).
	Guard *Guard
	// IdleTimeout: the server closes after this long without any record
	// (default 60 s); the client after this long without PING or ACK
	// (default 30 s). Negative disables it.
	IdleTimeout time.Duration
	// WriteTimeout bounds each write to the network (default 30 s).
	// Negative disables it.
	WriteTimeout time.Duration
	// NoAutoPong disables the client's automatic PONG reply to PING.
	NoAutoPong bool
	// HuffmanOnly compresses the outgoing stream without LZ77 matching
	// (NewStreamWriterHuffman): much less writer memory, slightly larger
	// output. The aggregator sets it for the server direction.
	HuffmanOnly bool
	// Uncompressed sends the outgoing stream as stored DEFLATE blocks
	// (NewStreamWriterStored): no compressor memory at all; for a direction
	// that carries only small records. It wins over HuffmanOnly.
	Uncompressed bool
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// Conn is an established AVF/1 record session (after WELCOME). Writes are
// safe for concurrent use; ReadRecord must be called from one goroutine.
type Conn struct {
	nc       net.Conn
	out      Direction // direction of records this side sends
	opts     ConnOptions
	features []string

	w  *StreamWriter
	dw *deadlineWriter
	sr *StreamReader
	rr *RecordReader

	lastRecv   atomic.Int64 // unix ns of the last record received
	lastLive   atomic.Int64 // client: unix ns of the last PING or ACK
	deadlineAt time.Time    // read deadline last set (reader goroutine only)

	clock     ClockEstimator // server: PING/PONG RTT and host offset
	pingMu    sync.Mutex
	pingSent  map[uint64]int64
	pingNext  uint64
	recvCount atomic.Uint64 // records received (for the ACK rule)

	closeOnce sync.Once
	closeErr  error
}

// sendBufs holds the encode buffers of every connection's Send. One pool for
// all connections: a per-connection sync.Pool keeps up to two buffers of up
// to MaxRecordLen per CPU thread for each connection (release soak, 61 AVF
// links on 32 threads: the result buffers kept growing for half an hour).
var sendBufs sync.Pool

type deadlineWriter struct {
	nc      net.Conn
	timeout time.Duration
	now     func() time.Time
	lastSet time.Time
	closing atomic.Bool // Close set a short final deadline: keep it
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if d.timeout > 0 && !d.closing.Load() {
		now := d.now()
		if now.Sub(d.lastSet) > time.Second {
			_ = d.nc.SetWriteDeadline(now.Add(d.timeout))
			d.lastSet = now
		}
	}
	return d.nc.Write(p)
}

// NewConn wraps an established connection whose handshake already
// completed (used by tests and by callers that run their own handshake).
// out is the direction of the records this side sends.
func NewConn(nc net.Conn, out Direction, opts ConnOptions, features []string) *Conn {
	return newConn(nc, out, opts, features)
}

func newConn(nc net.Conn, out Direction, opts ConnOptions, features []string) *Conn {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Batch == 0 {
		opts.Batch = DefaultBatch
	}
	if opts.IdleTimeout == 0 {
		if out == FromServer {
			opts.IdleTimeout = ServerIdleTimeout
		} else {
			opts.IdleTimeout = ClientLiveness
		}
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 30 * time.Second
	}
	g := DefaultGuard()
	if opts.Guard != nil {
		g = *opts.Guard
	}
	if g.Now == nil {
		g.Now = opts.Now
	}
	in := FromClient
	if out == FromClient {
		in = FromServer
	}
	c := &Conn{nc: nc, out: out, opts: opts, features: features, pingSent: map[uint64]int64{}}
	batch := opts.Batch
	if batch < 0 {
		batch = 0
	}
	c.dw = &deadlineWriter{nc: nc, timeout: opts.WriteTimeout, now: opts.Now}
	switch {
	case opts.Uncompressed:
		c.w = NewStreamWriterStored(c.dw, batch)
	case opts.HuffmanOnly:
		c.w = NewStreamWriterHuffman(c.dw, batch)
	default:
		c.w = NewStreamWriter(c.dw, batch)
	}
	c.sr = NewStreamReader(nc, g)
	c.rr = NewRecordReader(c.sr, in)
	now := opts.Now().UnixNano()
	c.lastRecv.Store(now)
	c.lastLive.Store(now)
	c.clock.Window = 60 * time.Second
	return c
}

// Features returns the effective feature set.
func (c *Conn) Features() []string { return c.features }

// HasFeature reports whether a feature is effective.
func (c *Conn) HasFeature(f string) bool { return slices.Contains(c.features, f) }

// NetConn returns the underlying connection.
func (c *Conn) NetConn() net.Conn { return c.nc }

// RemoteAddr returns the peer address.
func (c *Conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

// SetBatch changes the flush interval (FLOW batchMs).
func (c *Conn) SetBatch(d time.Duration) { c.w.SetBatch(d) }

// LastReceive returns when the last record arrived.
func (c *Conn) LastReceive() time.Time { return time.Unix(0, c.lastRecv.Load()) }

// LastLiveness returns when the last PING or ACK arrived (client side).
func (c *Conn) LastLiveness() time.Time { return time.Unix(0, c.lastLive.Load()) }

// ErrIdle is returned when the idle/liveness timeout expires.
var ErrIdle = errors.New("avf: session idle timeout")

// ReadRecord returns the next record. The payload is valid until the next
// call. On the client a PING is answered with a PONG automatically (unless
// NoAutoPong) and still returned; on the server a PONG updates the clock
// estimator and is still returned.
func (c *Conn) ReadRecord() (Record, error) {
	c.armReadDeadline()
	rec, err := c.rr.Next()
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return rec, ErrIdle
		}
		return rec, err
	}
	now := c.opts.Now()
	c.lastRecv.Store(now.UnixNano())
	c.recvCount.Add(1)
	switch rec.Type {
	case TPing:
		c.lastLive.Store(now.UnixNano())
		c.deadlineAt = time.Time{} // extend the client liveness deadline
		if !c.opts.NoAutoPong {
			var p Ping
			if err := p.Decode(rec.Payload); err != nil {
				return rec, err
			}
			rx := now.UnixNano()
			if err := c.WritePong(Pong{ID: p.ID, RxNs: rx, TxNs: c.opts.Now().UnixNano()}); err != nil {
				return rec, err
			}
		}
	case TAck:
		c.lastLive.Store(now.UnixNano())
		c.deadlineAt = time.Time{}
	case TPong:
		var p Pong
		if err := p.Decode(rec.Payload); err != nil {
			return rec, err
		}
		c.pingMu.Lock()
		sent, ok := c.pingSent[p.ID]
		delete(c.pingSent, p.ID)
		c.pingMu.Unlock()
		if ok {
			c.clock.Add(sent, now.UnixNano(), p)
		}
	}
	return rec, nil
}

// armReadDeadline extends the read deadline. The server extends it on every
// record; the client only on PING/ACK (deadlineAt is reset by those).
func (c *Conn) armReadDeadline() {
	if c.opts.IdleTimeout <= 0 {
		return
	}
	now := c.opts.Now()
	if c.out == FromServer {
		if now.Sub(c.deadlineAt) < time.Second {
			return
		}
	} else if !c.deadlineAt.IsZero() {
		return
	}
	c.deadlineAt = now
	_ = c.nc.SetReadDeadline(now.Add(c.opts.IdleTimeout))
}

func (c *Conn) getBuf() *[]byte {
	if b, ok := sendBufs.Get().(*[]byte); ok {
		*b = (*b)[:0]
		return b
	}
	b := make([]byte, 0, 4096)
	return &b
}

func (c *Conn) putBuf(b *[]byte) {
	if cap(*b) <= MaxRecordLen+64 {
		sendBufs.Put(b)
	}
}

// WriteRecord writes one record with a ready payload.
func (c *Conn) WriteRecord(t RecordType, payload []byte) error {
	if t.Direction() != c.out {
		return protoErr("cannot send %s in this direction", t)
	}
	return c.w.WriteRecord(t, payload)
}

// Send encodes a payload with enc into a pooled buffer and writes it.
func (c *Conn) Send(t RecordType, enc func(dst []byte) ([]byte, error)) error {
	b := c.getBuf()
	defer c.putBuf(b)
	p, err := enc(*b)
	*b = p
	if err != nil {
		return err
	}
	return c.WriteRecord(t, p)
}

// WriteFrames writes a FRAMES record.
func (c *Conn) WriteFrames(r *Frames) error {
	return c.Send(TFrames, func(d []byte) ([]byte, error) { return AppendFrames(d, r) })
}

// WriteJSON writes a no-seq JSON record (CLAIM, GOODBYE, NOTICE, FLOW, ...).
func (c *Conn) WriteJSON(t RecordType, v any) error {
	return c.Send(t, func(d []byte) ([]byte, error) { return AppendJSON(d, v) })
}

// WriteSeqJSON writes a seq-carrying JSON record (RECEIVER, CLOCK_EVENT, ...).
func (c *Conn) WriteSeqJSON(t RecordType, seq uint64, v any) error {
	return c.Send(t, func(d []byte) ([]byte, error) { return AppendSeqJSON(d, seq, v) })
}

// WriteAck writes an ACK.
func (c *Conn) WriteAck(seq uint64) error {
	return c.Send(TAck, func(d []byte) ([]byte, error) { return AppendAck(d, seq), nil })
}

// WritePong writes a PONG.
func (c *Conn) WritePong(p Pong) error {
	return c.Send(TPong, func(d []byte) ([]byte, error) { return AppendPong(d, p), nil })
}

// WritePing writes a PING with the next id and remembers its send time for
// the clock estimator.
func (c *Conn) WritePing() error {
	now := c.opts.Now().UnixNano()
	c.pingMu.Lock()
	c.pingNext++
	id := c.pingNext
	c.pingSent[id] = now
	for k := range c.pingSent { // bound the table (lost PONGs)
		if len(c.pingSent) <= 32 {
			break
		}
		if k+32 < id {
			delete(c.pingSent, k)
		}
	}
	c.pingMu.Unlock()
	return c.Send(TPing, func(d []byte) ([]byte, error) {
		return AppendPing(d, Ping{ID: id, ServerSendNs: now}), nil
	})
}

// WriteMLATResults writes an MLAT_RESULTS record.
func (c *Conn) WriteMLATResults(fixes []model.MLATFix) error {
	return c.Send(TMLATResults, func(d []byte) ([]byte, error) { return AppendMLATResults(d, fixes) })
}

// Flush forces a sync flush.
func (c *Conn) Flush() error { return c.w.Flush() }

// Goodbye sends GOODBYE (flushed), then closes the connection.
func (c *Conn) Goodbye(code, reason string, reconnectIn int) error {
	g := Goodbye{Code: code, Reason: reason}
	if c.out == FromServer {
		g.ReconnectIn = reconnectIn
	}
	err := c.WriteJSON(GoodbyeType(c.out), &g)
	if cerr := c.Close(); err == nil {
		err = cerr
	}
	return err
}

// Close flushes what it can and closes the connection.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.dw.closing.Store(true)
		_ = c.nc.SetWriteDeadline(c.opts.Now().Add(2 * time.Second))
		_ = c.w.Close()
		c.closeErr = c.nc.Close()
	})
	return c.closeErr
}

// ConnStats are a session's traffic counters.
type ConnStats struct {
	Out WriterStats
	In  ReaderStats
	// RecordsIn counts records received; SkippedIn unknown records skipped.
	RecordsIn uint64
	SkippedIn uint64
}

// Stats returns the session's counters.
func (c *Conn) Stats() ConnStats {
	return ConnStats{Out: c.w.Stats(), In: c.sr.Stats(), RecordsIn: c.recvCount.Load()}
}

// Clock returns the server's min-RTT estimate of the round-trip time and of
// the feeder host-clock offset (feeder minus server), if any PONG arrived
// within the window.
func (c *Conn) Clock() (rtt, offset time.Duration, ok bool) { return c.clock.Best() }

// RunServerKeepalive sends PING every PingInterval and ACK{watermark()}
// every AckInterval while records arrive or the watermark moved, until ctx
// is done or a write fails. watermark may be nil (no ACKs).
func (c *Conn) RunServerKeepalive(ctx context.Context, watermark func() uint64) error {
	ackT := time.NewTicker(AckInterval)
	defer ackT.Stop()
	pingT := time.NewTicker(PingInterval)
	defer pingT.Stop()
	if err := c.WritePing(); err != nil {
		return err
	}
	var lastAck uint64
	var lastCount uint64
	acked := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pingT.C:
			if err := c.WritePing(); err != nil {
				return err
			}
		case <-ackT.C:
			if watermark == nil {
				continue
			}
			n := c.recvCount.Load()
			wm := watermark()
			if n != lastCount || wm != lastAck || !acked {
				if err := c.WriteAck(wm); err != nil {
					return err
				}
				lastAck, lastCount, acked = wm, n, true
			}
		}
	}
}

// ---------------------------------------------------------------- clock

// ClockEstimator keeps the PING/PONG samples of a sliding window and
// reports the minimum-RTT sample's RTT and feeder host-clock offset
// (AVF.md §6.1): offset ≈ ((rxNs − sendNs) + (txNs − recvNs)) / 2.
type ClockEstimator struct {
	// Window is the sample lifetime (default 60 s).
	Window time.Duration

	mu      sync.Mutex
	samples []clockSample
}

type clockSample struct {
	at, rtt, off int64
}

// Add records one PING (sent at sendNs, server clock) answered by p and
// received at recvNs (server clock).
func (e *ClockEstimator) Add(sendNs, recvNs int64, p Pong) {
	rtt := (recvNs - sendNs) - (p.TxNs - p.RxNs)
	if rtt < 0 {
		rtt = 0
	}
	off := ((p.RxNs - sendNs) + (p.TxNs - recvNs)) / 2
	w := e.Window
	if w <= 0 {
		w = 60 * time.Second
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	keep := e.samples[:0]
	for _, s := range e.samples {
		if recvNs-s.at <= int64(w) {
			keep = append(keep, s)
		}
	}
	e.samples = append(keep, clockSample{at: recvNs, rtt: rtt, off: off})
	if len(e.samples) > 256 {
		e.samples = append(e.samples[:0], e.samples[len(e.samples)-256:]...)
	}
}

// Best returns the minimum-RTT sample of the window.
func (e *ClockEstimator) Best() (rtt, offset time.Duration, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, s := range e.samples {
		if i == 0 || s.rtt < int64(rtt) {
			rtt, offset, ok = time.Duration(s.rtt), time.Duration(s.off), true
		}
	}
	return
}
