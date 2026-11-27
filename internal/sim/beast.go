package sim

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"sync"
	"time"

	"airview/internal/model"
)

// Beast binary framing (see the package comment for the sources).
const (
	beastEsc       = 0x1A
	beastShort     = '2' // 6-byte timestamp, signal, 7 bytes Mode S
	beastLong      = '3' // 6-byte timestamp, signal, 14 bytes Mode S
	beastStatus    = '4' // 6-byte timestamp, unused byte, 14 bytes Radarcape status
	beastPosition  = '5' // 21 bytes Radarcape receiver position
	beastModeAC    = '1' // 6-byte timestamp, signal, 2 bytes Mode A/C
	beastMLATStamp = 0xFF004D4C4154

	beastMaxPending   = 4 << 20 // bytes queued per client before it is dropped
	beastWriteTimeout = 15 * time.Second
	beastPositionSecs = 10 // a receiver position frame follows every n-th status frame
)

// appendBeast appends one escaped Beast frame: 0x1A, type, then (unless it is
// a position frame) the 48-bit big-endian timestamp and the signal byte,
// then the payload. Every 0x1A after the type byte is doubled.
func appendBeast(dst []byte, typ byte, ts uint64, sig byte, payload []byte) []byte {
	dst = append(dst, beastEsc, typ)
	if typ != beastPosition {
		for shift := 40; shift >= 0; shift -= 8 {
			b := byte(ts >> shift)
			dst = append(dst, b)
			if b == beastEsc {
				dst = append(dst, beastEsc)
			}
		}
		dst = append(dst, sig)
		if sig == beastEsc {
			dst = append(dst, beastEsc)
		}
	}
	for _, b := range payload {
		dst = append(dst, b)
		if b == beastEsc {
			dst = append(dst, beastEsc)
		}
	}
	return dst
}

// gpsTimestamp converts nanoseconds since UTC midnight to the Radarcape GPS
// timestamp: second of the day in the upper 18 bits, nanoseconds in the
// lower 30 bits.
func gpsTimestamp(gpsNs int64) uint64 {
	if gpsNs < 0 {
		return 0
	}
	return uint64(gpsNs/1e9)<<30 | uint64(gpsNs%1e9)
}

// signalByte converts dBFS to the Beast signal byte: amplitude scaled to
// 0..255 (dBFS = 20*log10(byte/255)), never 0 for a known level.
func signalByte(dbfs float64) byte {
	if math.IsNaN(dbfs) {
		return 0
	}
	v := math.Round(255 * math.Pow(10, dbfs/20))
	if v < 1 {
		return 1
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// AppendBeastFrame appends f in Radarcape-style Beast binary to dst and
// returns the extended slice. Mode S and Mode A/C frames become type '2',
// '3' or '1' with the GPS timestamp (or the 12 MHz counter when GPSNs < 0)
// and the signal byte; MLAT results carry the conventional synthetic
// timestamp 0xFF004D4C4154 and signal 0; status frames become type '4'.
// Frames with an unexpected payload length are skipped.
func AppendBeastFrame(dst []byte, f *model.RawFrame) []byte {
	var typ byte
	switch {
	case f.Kind == model.FrameModeSShort && len(f.Data) == 7:
		typ = beastShort
	case f.Kind == model.FrameModeSLong && len(f.Data) == 14:
		typ = beastLong
	case f.Kind == model.FrameModeAC && len(f.Data) == 2:
		typ = beastModeAC
	case f.Kind == model.FrameStatus && len(f.Data) == 14:
		return appendBeast(dst, beastStatus, frameTimestamp(f), 0, f.Data)
	default:
		return dst
	}
	if f.MLAT {
		return appendBeast(dst, typ, beastMLATStamp, 0, f.Data)
	}
	return appendBeast(dst, typ, frameTimestamp(f), signalByte(f.Signal), f.Data)
}

func frameTimestamp(f *model.RawFrame) uint64 {
	if f.GPSNs >= 0 {
		return gpsTimestamp(f.GPSNs)
	}
	return f.Clock12MHz & 0xFFFFFFFFFFFF
}

// BeastOptions selects what a Beast listener serves.
type BeastOptions struct {
	// AntennaID restricts the stream to the frames received by one antenna
	// (its RF channel), e.g. to feed two Beast inputs that are attributed to
	// the vertical and the yagi through Feed.AntennaID. Empty = the combined
	// receiver output: one frame per received transmission, taken from the
	// channel that heard it best, the way a single multi-channel receiver
	// reports it. (The Beast format has no channel field.)
	AntennaID string
	// NoMLAT leaves the synthetic MLAT result frames out of the stream.
	NoMLAT bool
	// NoPosition suppresses the Radarcape receiver position frame (type
	// '5') that otherwise follows every tenth status frame.
	NoPosition bool
}

// beastStream describes how a batch is encoded for one client.
type beastStream struct {
	channel    int // RF channel, -1 = combined
	noMLAT     bool
	noPosition bool
	pos        [21]byte // receiver position frame payload
}

// sameTx reports whether two consecutive frames of a batch are copies of the
// same transmission received on different channels.
func sameTx(a, b *model.RawFrame) bool {
	return !b.MLAT && b.Kind == a.Kind && len(a.Data) > 0 && len(b.Data) > 0 && &a.Data[0] == &b.Data[0]
}

// appendBatch encodes the frames of one step.
func (st *beastStream) appendBatch(dst []byte, batch []model.RawFrame) []byte {
	for i := 0; i < len(batch); i++ {
		f := &batch[i]
		switch {
		case f.Kind == model.FrameStatus:
			dst = AppendBeastFrame(dst, f)
			if !st.noPosition && f.GPSNs >= 0 && f.GPSNs/1e9%beastPositionSecs == 0 {
				dst = appendBeast(dst, beastPosition, 0, 0, st.pos[:])
			}
		case f.MLAT:
			if !st.noMLAT {
				dst = AppendBeastFrame(dst, f)
			}
		case st.channel >= 0:
			if f.Channel == st.channel {
				dst = AppendBeastFrame(dst, f)
			}
		default:
			for i+1 < len(batch) && sameTx(f, &batch[i+1]) {
				i++
				if batch[i].Signal > f.Signal {
					f = &batch[i]
				}
			}
			dst = AppendBeastFrame(dst, f)
		}
	}
	return dst
}

// beastClient is one connected TCP client with its outgoing queue.
type beastClient struct {
	conn   net.Conn
	stream beastStream

	mu      sync.Mutex
	pending []byte
	closed  bool

	wake chan struct{}
	done chan struct{}
	once sync.Once
}

func (c *beastClient) close() {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.done)
		c.conn.Close()
	})
}

// push queues the frames of one step for the client. A client that does not
// keep up is dropped; with block set (Advance / CatchUp, which outrun any
// consumer by design) the caller instead waits for the queue to drain.
func (c *beastClient) push(ctx context.Context, batch []model.RawFrame, block bool) {
	deadline := time.Time{}
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		if len(c.pending) < beastMaxPending {
			c.pending = c.stream.appendBatch(c.pending, batch)
			c.mu.Unlock()
			select {
			case c.wake <- struct{}{}:
			default:
			}
			return
		}
		c.mu.Unlock()
		if deadline.IsZero() {
			deadline = time.Now().Add(beastWriteTimeout)
		}
		if !block || ctx.Err() != nil || time.Now().After(deadline) {
			slog.Warn("sim: dropping slow Beast client", "remote", c.conn.RemoteAddr().String())
			c.close()
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// serve writes the queue to the connection until the client is closed.
func (c *beastClient) serve() {
	defer c.close()
	// Whatever the client sends (a Radarcape accepts option commands) is
	// read and ignored; a read error means the peer is gone.
	go func() {
		io.Copy(io.Discard, c.conn)
		c.close()
	}()
	spare := make([]byte, 0, 16<<10)
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
		}
		for {
			c.mu.Lock()
			buf := c.pending
			c.pending = spare[:0]
			c.mu.Unlock()
			if len(buf) == 0 {
				spare = buf
				break
			}
			c.conn.SetWriteDeadline(time.Now().Add(beastWriteTimeout))
			if _, err := c.conn.Write(buf); err != nil {
				return
			}
			spare = buf
		}
	}
}

func (s *Sim) addClient(c *beastClient) {
	s.outMu.Lock()
	s.clients = append(append([]*beastClient(nil), s.clients...), c)
	s.outMu.Unlock()
}

func (s *Sim) removeClient(c *beastClient) {
	s.outMu.Lock()
	out := make([]*beastClient, 0, len(s.clients))
	for _, x := range s.clients {
		if x != c {
			out = append(out, x)
		}
	}
	s.clients = out
	s.outMu.Unlock()
}

// ServeBeast listens on listenAddr (e.g. ":10003" or "127.0.0.1:0") and
// serves the simulated receiver output to every TCP client in Radarcape-style
// Beast binary, the combined stream of BeastOptions{}. It blocks until ctx is
// cancelled (returning nil) or the listener fails. The simulation runs while
// ServeBeast is active, paced by Config.Speed; see Run.
//
// Stream content: Mode S frames as type '2' / '3' with the GPS timestamp
// (second of the day << 30 | nanoseconds) and the signal byte, a status frame
// (type '4') on every full second, a receiver position frame (type '5') every
// ten seconds, and MLAT results as DF18 frames with the synthetic timestamp
// 0xFF004D4C4154. Every 0x1A after a frame's type byte is doubled.
func (s *Sim) ServeBeast(ctx context.Context, listenAddr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return err
	}
	return s.ServeBeastOn(ctx, ln, BeastOptions{})
}

// ServeBeastOn is ServeBeast on an existing listener (which it closes on
// return) with explicit stream options. Use it to learn the bound address of
// an ephemeral port, or to serve one stream per antenna.
func (s *Sim) ServeBeastOn(ctx context.Context, ln net.Listener, opt BeastOptions) error {
	defer ln.Close()
	stream := beastStream{channel: -1, noMLAT: opt.NoMLAT, noPosition: opt.NoPosition}
	if opt.AntennaID != "" {
		found := false
		for i := range s.ants {
			if s.ants[i].id == opt.AntennaID {
				stream.channel, found = s.ants[i].channel, true
				break
			}
		}
		if !found {
			return errAntenna
		}
	}
	h := 0.0
	if len(s.ants) > 0 {
		h = s.ants[0].heightM
	}
	binary.LittleEndian.PutUint32(stream.pos[4:], math.Float32bits(float32(s.st.lat)))
	binary.LittleEndian.PutUint32(stream.pos[8:], math.Float32bits(float32(s.st.lon)))
	binary.LittleEndian.PutUint32(stream.pos[12:], math.Float32bits(float32(s.st.groundM+h)))

	s.acquire()
	defer s.release()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-stop:
		}
	}()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		mine = map[*beastClient]struct{}{}
	)
	defer func() {
		mu.Lock()
		for c := range mine {
			c.close()
		}
		mu.Unlock()
		wg.Wait()
	}()

	backoff := 5 * time.Millisecond
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				// transient accept failure (e.g. out of descriptors): retry
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, time.Second)
				continue
			}
			return err
		}
		backoff = 5 * time.Millisecond
		c := &beastClient{
			conn:   conn,
			stream: stream,
			wake:   make(chan struct{}, 1),
			done:   make(chan struct{}),
		}
		mu.Lock()
		mine[c] = struct{}{}
		mu.Unlock()
		s.addClient(c)
		slog.Info("sim: Beast client connected", "remote", conn.RemoteAddr().String())
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.serve()
			s.removeClient(c)
			mu.Lock()
			delete(mine, c)
			mu.Unlock()
			slog.Info("sim: Beast client disconnected", "remote", conn.RemoteAddr().String())
		}()
	}
}
