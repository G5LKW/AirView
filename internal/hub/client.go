package hub

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// ClientOptions configures a Client. Zero values select the defaults.
type ClientOptions struct {
	Host     string
	Port     int // 0 = DefaultPort
	Password string

	// FeedID, when non-empty, replaces the FeedID of every delivered frame
	// and update (use the local hub feed's ID). When empty, frames keep the
	// host's feed IDs (see Station.Feeds).
	FeedID string

	// OnStation is called (from the client's goroutine) with the host's
	// station metadata after login and whenever the host changes it.
	OnStation func(Station)
	// OnState is called on every connection state change: connected=true
	// with the remote address after a successful login, connected=false
	// with the cause when the connection is lost or an attempt fails.
	OnState func(connected bool, addr string, err error)
	// OnBytes is called with the number of bytes read from the socket.
	OnBytes func(n int)

	DialTimeout time.Duration // default 10 s
	// IdleTimeout is the longest silence tolerated before the connection is
	// considered dead (the host sends heartbeats every 5 s). Default 60 s.
	IdleTimeout time.Duration
	MinBackoff  time.Duration // first reconnect delay, default 1 s
	MaxBackoff  time.Duration // reconnect delay ceiling, default 30 s

	Logger *slog.Logger // default slog.Default()
}

func (o *ClientOptions) setDefaults() {
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 60 * time.Second
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = 30 * time.Second
		if o.MaxBackoff < o.MinBackoff {
			o.MaxBackoff = o.MinBackoff
		}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Client connects to an AirView host's data port and delivers its stream to
// a model.FrameSink. It reconnects automatically (exponential backoff with
// jitter, 1 s .. 30 s) until its context is cancelled.
type Client struct {
	opts ClientOptions
	log  *slog.Logger

	connected atomic.Bool
	frames    atomic.Uint64
	updates   atomic.Uint64
	invalid   atomic.Uint64

	mu         sync.Mutex
	station    Station
	hasStation bool
}

// NewClient returns a client; nothing is dialled until Run.
func NewClient(opts ClientOptions) *Client {
	opts.setDefaults()
	return &Client{opts: opts, log: opts.Logger.With("component", "hub-client")}
}

// Dial returns a client for host:port with the given password and default
// options. The connection is established (and re-established) by Run.
func Dial(host string, port int, password string) *Client {
	return NewClient(ClientOptions{Host: host, Port: port, Password: password})
}

// Addr returns the "host:port" the client connects to.
func (c *Client) Addr() string {
	return net.JoinHostPort(c.opts.Host, strconv.Itoa(c.opts.Port))
}

// Connected reports whether the client is currently logged in.
func (c *Client) Connected() bool { return c.connected.Load() }

// Counters returns the number of frames and updates delivered so far.
func (c *Client) Counters() (frames, updates uint64) { return c.frames.Load(), c.updates.Load() }

// Invalid returns the number of relayed frames that were discarded because
// their data length did not match their kind.
func (c *Client) Invalid() uint64 { return c.invalid.Load() }

// Station returns the most recent station metadata received from the host.
func (c *Client) Station() (Station, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.station, c.hasStation
}

// Run connects, logs in and delivers the host's stream to sink until ctx is
// cancelled; it reconnects after every failure. It always returns ctx.Err().
//
// Delivered frames carry the host receiver's original GPS / 12 MHz
// timestamps, signal level, channel and MLAT flag. RawFrame.Recv is the
// host's receive time translated into the local clock (the constant offset
// between both system clocks is estimated from heartbeats), so frame
// spacing is exactly what the host saw. Each frame's Data slice is freshly
// allocated and may be retained by the sink.
func (c *Client) Run(ctx context.Context, sink model.FrameSink) error {
	delay := c.opts.MinBackoff
	lastLogged := ""
	for {
		start := time.Now()
		loggedIn, err := c.session(ctx, sink)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.state(false, c.Addr(), err)
		// Log state changes, not every retry against a host that is down.
		if msg := fmt.Sprint(err); loggedIn || msg != lastLogged {
			lastLogged = msg
			if loggedIn {
				c.log.Warn("hub connection lost", "addr", c.Addr(), "err", err)
			} else {
				c.log.Warn("hub connection failed", "addr", c.Addr(), "err", err)
			}
		}

		switch {
		case errors.Is(err, ErrAuth), errors.Is(err, ErrThrottled):
			// Retrying quickly with a wrong password only gets this address
			// throttled by the host.
			delay = c.opts.MaxBackoff
		case loggedIn && time.Since(start) > 5*time.Second:
			delay = c.opts.MinBackoff
		}
		// Jitter: wait between 50 % and 100 % of the nominal delay.
		wait := delay/2 + time.Duration(mrand.Int64N(int64(delay/2)+1))
		if delay *= 2; delay > c.opts.MaxBackoff {
			delay = c.opts.MaxBackoff
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) state(connected bool, addr string, err error) {
	c.connected.Store(connected)
	if c.opts.OnState != nil {
		c.opts.OnState(connected, addr, err)
	}
}

// countingReader reports bytes read to a callback.
type countingReader struct {
	r  io.Reader
	fn func(int)
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.fn(n)
	}
	return n, err
}

// session runs one connection: dial, handshake, read records until failure.
func (c *Client) session(ctx context.Context, sink model.FrameSink) (loggedIn bool, err error) {
	d := net.Dialer{Timeout: c.opts.DialTimeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.Addr())
	if err != nil {
		return false, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() {
		stop()
		conn.Close()
	}()

	var src io.Reader = conn
	if c.opts.OnBytes != nil {
		src = &countingReader{r: conn, fn: c.opts.OnBytes}
	}
	br := bufio.NewReaderSize(src, 64<<10)

	conn.SetDeadline(time.Now().Add(c.opts.DialTimeout))
	if err := c.handshake(conn, br); err != nil {
		return false, err
	}
	conn.SetDeadline(time.Time{})
	c.state(true, conn.RemoteAddr().String(), nil)
	c.log.Info("logged in to hub", "addr", conn.RemoteAddr().String())

	return true, c.readRecords(conn, br, sink)
}

func (c *Client) handshake(conn net.Conn, br *bufio.Reader) error {
	line, err := readLine(br)
	if err != nil {
		return fmt.Errorf("reading greeting: %w", err)
	}
	if err := errLine(line); err != nil {
		return err
	}
	parts := strings.Fields(line)
	if len(parts) != 4 || parts[0] != greetingWord {
		return fmt.Errorf("%w: not an AirView data port", ErrProtocol)
	}
	if parts[1] != strconv.Itoa(ProtocolVersion) {
		return fmt.Errorf("%w: host speaks protocol version %s, this client version %d", ErrProtocol, parts[1], ProtocolVersion)
	}
	mode := parts[2]
	if mode != authHMAC && mode != authOpen {
		return fmt.Errorf("%w: unsupported login method %q", ErrProtocol, mode)
	}
	serverNonce, err := hex.DecodeString(parts[3])
	if err != nil || len(serverNonce) != nonceLen {
		return fmt.Errorf("%w: malformed nonce", ErrProtocol)
	}

	var clientNonce [nonceLen]byte
	if _, err := rand.Read(clientNonce[:]); err != nil {
		return err
	}
	auth := "AUTH " + hex.EncodeToString(clientNonce[:]) + " " +
		hex.EncodeToString(clientProof(c.opts.Password, serverNonce)) + "\n"
	if _, err := io.WriteString(conn, auth); err != nil {
		return err
	}

	line, err = readLine(br)
	if err != nil {
		return fmt.Errorf("reading login reply: %w", err)
	}
	if err := errLine(line); err != nil {
		return err
	}
	parts = strings.Fields(line)
	if len(parts) != 2 || parts[0] != "OK" {
		return fmt.Errorf("%w: unexpected login reply", ErrProtocol)
	}
	if mode == authHMAC {
		proof, err := hex.DecodeString(parts[1])
		if err != nil || !hmac.Equal(proof, serverProof(c.opts.Password, serverNonce, clientNonce[:])) {
			return fmt.Errorf("%w: host did not prove knowledge of the password", ErrAuth)
		}
	} else if c.opts.Password != "" {
		c.log.Info("hub host has no password set; connected without authentication")
	}
	return nil
}

// errLine converts an "ERR <code> <text>" line into an error.
func errLine(line string) error {
	if !strings.HasPrefix(line, "ERR") {
		return nil
	}
	parts := strings.SplitN(line, " ", 3)
	code, text := "", ""
	if len(parts) > 1 {
		code = parts[1]
	}
	if len(parts) > 2 {
		text = parts[2]
	}
	base := ErrProtocol
	switch code {
	case "auth":
		base = ErrAuth
	case "busy":
		base = ErrBusy
	case "throttled":
		base = ErrThrottled
	}
	if text == "" {
		return base
	}
	return fmt.Errorf("%w (%s)", base, text)
}

// readRecords is the receive loop of one logged-in connection.
func (c *Client) readRecords(conn net.Conn, br *bufio.Reader, sink model.FrameSink) error {
	var (
		hdr      [recHeader]byte
		payload  = make([]byte, 0, 4096)
		feeds    [256]string // feed index -> ID for this connection
		haveOff  bool
		clockOff int64 // local clock - host clock, nanoseconds
		deadline time.Time
	)
	override := c.opts.FeedID

	// toLocal converts a host receive time to the local clock.
	toLocal := func(hostNs int64) time.Time {
		now := time.Now()
		if hostNs == 0 || !haveOff {
			return now
		}
		t := time.Unix(0, hostNs+clockOff)
		if t.After(now) {
			return now
		}
		return t
	}

	for {
		// Refresh the read deadline at most about once a second; setting it
		// for every record would cost a system call per frame.
		if now := time.Now(); now.Sub(deadline) > -c.opts.IdleTimeout+time.Second {
			deadline = now.Add(c.opts.IdleTimeout)
			conn.SetReadDeadline(deadline)
		}
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return describeReadErr(err, c.opts.IdleTimeout)
		}
		n := int(binary.BigEndian.Uint16(hdr[:]))
		if n < 1 {
			return fmt.Errorf("%w: empty record", ErrProtocol)
		}
		typ := hdr[2]
		n-- // type byte already read
		if cap(payload) < n {
			payload = make([]byte, n)
		}
		payload = payload[:n]
		if _, err := io.ReadFull(br, payload); err != nil {
			return describeReadErr(err, c.opts.IdleTimeout)
		}

		switch typ {
		case recFrame:
			f, idx, recvNs, err := decodeFrame(payload)
			if err != nil {
				return err
			}
			if !validFrame(&f) {
				// Never hand the sink a frame whose length contradicts its
				// kind (it indexes Data by kind); skip it and carry on.
				c.invalid.Add(1)
				continue
			}
			f.Recv = toLocal(recvNs)
			if override != "" {
				f.FeedID = override
			} else {
				f.FeedID = feeds[idx]
			}
			c.frames.Add(1)
			sink.HandleFrame(f)

		case recUpdate:
			u, idx, recvNs, err := decodeUpdate(payload)
			if err != nil {
				return err
			}
			u.Recv = toLocal(recvNs)
			if override != "" {
				u.FeedID = override
			} else {
				u.FeedID = feeds[idx]
			}
			c.updates.Add(1)
			sink.HandleUpdate(u)

		case recFeedDef:
			if len(payload) < 1 {
				return errShort
			}
			if payload[0] != unknownFeed {
				feeds[payload[0]] = string(payload[1:])
			}

		case recHeartbeat:
			if len(payload) < 8 {
				return errShort
			}
			// local - host includes the network delay; the smallest value
			// seen is the best estimate of the pure clock offset. Let it
			// creep upwards slowly so that clock drift is followed.
			d := time.Now().UnixNano() - int64(binary.BigEndian.Uint64(payload))
			switch {
			case !haveOff || d < clockOff:
				clockOff, haveOff = d, true
			default:
				clockOff += (d - clockOff) / 32
			}

		case recStation:
			var st Station
			if err := json.Unmarshal(payload, &st); err != nil {
				return fmt.Errorf("%w: station metadata: %v", ErrProtocol, err)
			}
			c.mu.Lock()
			c.station, c.hasStation = st, true
			c.mu.Unlock()
			if c.opts.OnStation != nil {
				c.opts.OnStation(st)
			}

		default:
			// Unknown record type from a newer host: skip.
		}
	}
}

// validFrame reports whether a relayed frame has the data length its kind
// requires: 2 bytes Mode A/C, 7 short, 14 long; a status payload is free-form
// but short. Frames of a kind this version does not know are rejected.
func validFrame(f *model.RawFrame) bool {
	switch f.Kind {
	case model.FrameModeAC:
		return len(f.Data) == 2
	case model.FrameModeSShort:
		return len(f.Data) == 7
	case model.FrameModeSLong:
		return len(f.Data) == 14
	case model.FrameStatus:
		return len(f.Data) >= 1 && len(f.Data) <= 64
	}
	return false
}

func describeReadErr(err error, idle time.Duration) error {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("no data from hub for %s", idle)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return errors.New("connection closed by hub")
	}
	return err
}
