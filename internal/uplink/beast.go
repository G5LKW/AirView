package uplink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
	"airview/internal/modes"
)

// "Also feed others" (DESIGN §12.1 phase 5): a Beast output to a
// third-party aggregator (a community readsb on port 30004, for example),
// next to the native AVF/1 uplinks. Written from the documented and observed
// wire behaviour of readsb's beast_reduce_plus_out connector (see
// docs/aggregator/RESEARCH.md §1.4 and internal/feed/beastext.go); no code of
// readsb or any other GPL program was consulted or copied.
//
// Wire behaviour of the default format (BeastReducePlus):
//
//   - on connect: 0x1A 0xE4 + the 36-character ASCII UUID, then 0x1A 'W' 'O'
//     (asks the server for pings);
//   - Beast binary frames (types '1', '2', '3' with the receiver's 48-bit
//     timestamp and the signal byte), thinned per aircraft and message type
//     to one per reduce interval (default 250 ms; data that does not affect
//     the position at 4x the interval);
//   - a 0x1A 'P' ping from the server is echoed verbatim; 0x1A 'W' 'S'
//     (slow down) doubles the reduce interval for a minute (up to 8x);
//   - an all-zero Mode A/C frame as keep-alive after 30 s without output.
//
// Beast outputs are live only: nothing is buffered while disconnected and
// nothing is backfilled (third-party aggregators expect live data). The same
// "never forwarded" rules as for AVF/1 apply: MLAT results (magic timestamp
// or MLAT flag), late (backfilled) frames, frames of feeds that are not
// Source.Forwardable, and the uplinks' own injected results.

// Beast output formats (BeastUplink.Format). The names are those of the
// readsb connector types so owners recognise them.
const (
	BeastReducePlus = "beast_reduce_plus_out" // reduce + UUID + 'W' 'O' (default)
	BeastReduce     = "beast_reduce_out"      // reduce, no UUID
	BeastFull       = "beast_out"             // every frame, no UUID
)

// Defaults of BeastUplink.
const (
	DefaultBeastPort     = 30004
	DefaultBeastReduceMs = 250
	// beastKeepAlive is the silence after which a keep-alive frame is sent.
	beastKeepAlive = 30 * time.Second
	// beastMaxPending caps the bytes waiting for the writer (about 2 s of a
	// busy unreduced station); beyond it frames are dropped, not queued.
	beastMaxPending = 1 << 20
	// beastFlush is the writer's flush interval.
	beastFlush = 50 * time.Millisecond
	// beastWriteTimeout bounds one write to the aggregator.
	beastWriteTimeout = 15 * time.Second
	// beastSlowFor is how long one 'W' 'S' request slows the output.
	beastSlowFor = time.Minute
)

// BeastUplink configures one Beast output ("uplinks[]" entries of type
// "beast", see WIRING.md §6). Zero values select the defaults.
type BeastUplink struct {
	// ID is a stable slug, unique among all uplinks (AVF and Beast).
	ID string `json:"id"`
	// Host and Port of the aggregator's Beast ingest (default port 30004).
	Host string `json:"host"`
	Port int    `json:"port"`
	// Format is BeastReducePlus (default), BeastReduce or BeastFull.
	Format string `json:"format,omitempty"`
	// UUID is sent in the 0xE4 message (BeastReducePlus only). "" = a random
	// UUID generated for this uplink and kept in
	// <StateDir>/uplinks/<id>/uuid, so the native feeder UUID is not shared
	// with third parties. Set it to the UUID of the owner's mlat-client
	// towards the same aggregator, so the aggregator joins both links into
	// one station.
	UUID string `json:"uuid,omitempty"`
	// Inputs lists the local feed IDs forwarded. Empty = every forwardable
	// feed.
	Inputs []string `json:"inputs,omitempty"`
	// ReduceMs is the reduce interval (default 250 ms, 50..5000). Ignored
	// for BeastFull.
	ReduceMs int `json:"reduceMs,omitempty"`
	// NoTimestamps sends all-zero timestamps instead of the receiver's.
	NoTimestamps bool `json:"noTimestamps,omitempty"`
	// Disabled keeps the entry but does not connect.
	Disabled bool `json:"disabled,omitempty"`
}

// Normalize applies the defaults and range limits and reports what it
// changed (config check notes, prefixed with path).
func (b *BeastUplink) Normalize(path string) []string {
	var notes []string
	note := func(f string, a ...any) { notes = append(notes, path+"."+fmt.Sprintf(f, a...)) }
	b.Host = strings.TrimSpace(b.Host)
	if b.Port == 0 {
		b.Port = DefaultBeastPort
	} else if b.Port < 1 || b.Port > 65535 {
		note("port %d is out of range; using %d", b.Port, DefaultBeastPort)
		b.Port = DefaultBeastPort
	}
	switch b.Format {
	case "":
		b.Format = BeastReducePlus
	case BeastReducePlus, BeastReduce, BeastFull:
	case "reduce_plus", "beast_reduce_plus":
		b.Format = BeastReducePlus
	case "reduce", "beast_reduce":
		b.Format = BeastReduce
	case "beast", "full":
		b.Format = BeastFull
	default:
		note("format %q is unknown; using %s", b.Format, BeastReducePlus)
		b.Format = BeastReducePlus
	}
	if b.ReduceMs == 0 {
		b.ReduceMs = DefaultBeastReduceMs
	} else if b.ReduceMs < 50 || b.ReduceMs > 5000 {
		v := min(max(b.ReduceMs, 50), 5000)
		note("reduceMs %d is out of range (50..5000); using %d", b.ReduceMs, v)
		b.ReduceMs = v
	}
	if b.UUID != "" {
		if _, ok := feed.NormalizeUUID(b.UUID); !ok {
			note("uuid is not a valid UUID (28..36 hex digits, dashes allowed); a generated one is used")
			b.UUID = ""
		} else {
			b.UUID = strings.TrimSpace(b.UUID)
		}
	}
	return notes
}

// Validate reports a configuration that cannot work (after Normalize).
func (b *BeastUplink) Validate() error {
	if b.Host == "" {
		return errors.New("host is required")
	}
	if strings.ContainsAny(b.Host, " /") {
		return fmt.Errorf("host %q is not a host name or address", b.Host)
	}
	return nil
}

func (b BeastUplink) clone() BeastUplink {
	b.Inputs = slices.Clone(b.Inputs)
	return b
}

// connKey are the settings that need a new connection when they change.
func (b BeastUplink) connKey() string {
	return strings.Join([]string{b.Host, strconv.Itoa(b.Port), b.Format, b.UUID, strconv.FormatBool(b.Disabled)}, "|")
}

// BeastStatus is the status of one Beast output (Manager.BeastStatus and
// the "beast" list of the status API). UUID is the identity at that
// aggregator: owner eyes only, like the rest of the status API.
type BeastStatus struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Format    string    `json:"format"`
	UUID      string    `json:"uuid,omitempty"`
	State     string    `json:"state"` // idle | disabled | connecting | connected | backoff
	Since     time.Time `json:"since"`
	LastError string    `json:"lastError,omitempty"`
	LastErrAt time.Time `json:"lastErrorAt,omitzero"`
	NextRetry time.Time `json:"nextRetry,omitzero"`
	ReduceMs  int       `json:"reduceMs"`
	SlowUntil time.Time `json:"slowUntil,omitzero"` // the aggregator asked to slow down

	FramesIn    uint64 `json:"framesIn"`    // frames offered (forwardable inputs)
	FramesSent  uint64 `json:"framesSent"`  // frames written to the socket
	Reduced     uint64 `json:"reduced"`     // dropped by the reduce filter
	Dropped     uint64 `json:"dropped"`     // dropped while disconnected or over the buffer cap
	MLATRefused uint64 `json:"mlatRefused"` // MLAT results never forwarded
	BadCRC      uint64 `json:"badCrc"`      // DF11/17/18 frames with a CRC error, not forwarded
	BytesSent   uint64 `json:"bytesSent"`
	PingsEchoed uint64 `json:"pingsEchoed"`
	SlowDowns   uint64 `json:"slowDowns"`
	Connects    uint64 `json:"connects"`
}

type beastCounters struct {
	framesIn, framesSent, reduced, dropped, mlatRefused, badCRC atomic.Uint64
	bytesSent, pingsEchoed, slowDowns, connects                 atomic.Uint64
}

// beastClient runs one Beast output.
type beastClient struct {
	o   *Options
	uid string

	inputs atomic.Pointer[[]string]
	up     atomic.Bool // connected: frames are encoded only while up

	mu        sync.Mutex
	cfg       BeastUplink
	uuid      string // UUID sent (BeastReducePlus)
	red       beastReducer
	buf       []byte // frames waiting for the writer
	nBuf      uint64 // frames in buf
	ctl       []byte // ping echoes waiting for the writer
	slowUntil time.Time
	slowLevel int // 'W' 'S' requests in force (interval x 2^level, at most 3)
	state     string
	since     time.Time
	lastErr   string
	lastErrAt time.Time
	nextRetry time.Time

	wake   chan struct{}
	retry  chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
	ctr    beastCounters
}

func newBeastClient(o *Options, cfg BeastUplink) *beastClient {
	c := &beastClient{o: o, uid: cfg.ID, cfg: cfg.clone(), state: StateIdle, since: o.Now(),
		wake: make(chan struct{}, 1), retry: make(chan struct{}, 1), done: make(chan struct{})}
	c.uuid = beastUUID(o.StateDir, cfg, o.Logf)
	c.red.init(time.Duration(cfg.ReduceMs) * time.Millisecond)
	c.setInputs(cfg.Inputs)
	return c
}

// beastUUID returns the UUID of a Beast output: the configured one, else a
// per-uplink one kept in its state directory (memory only without one).
func beastUUID(stateDir string, cfg BeastUplink, logf func(string, ...any)) string {
	if cfg.UUID != "" {
		return cfg.UUID
	}
	if stateDir == "" || cfg.ID == "" {
		return newUUID()
	}
	dir := filepath.Join(stateDir, "uplinks", cfg.ID)
	p := filepath.Join(dir, UUIDFile)
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			if _, ok := feed.NormalizeUUID(s); ok {
				return s
			}
		}
	}
	u := newUUID()
	if err := os.MkdirAll(dir, 0o700); err == nil {
		err = writeFileAtomic(p, []byte(u+"\n"), 0o600)
		if err != nil {
			logf("uplink %s: cannot keep the generated UUID: %v", cfg.ID, err)
		}
	}
	return u
}

func (c *beastClient) setInputs(in []string) {
	cp := slices.Clone(in)
	c.inputs.Store(&cp)
}

func (c *beastClient) start() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.run(ctx)
}

func (c *beastClient) close() {
	if c.cancel != nil {
		c.cancel()
		<-c.done
	}
}

// update applies settings that need no new connection.
func (c *beastClient) update(cfg BeastUplink) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg.clone()
	c.setInputs(cfg.Inputs)
	c.red.base = time.Duration(cfg.ReduceMs) * time.Millisecond
}

// Retry cuts a backoff short.
func (c *beastClient) Retry() {
	select {
	case c.retry <- struct{}{}:
	default:
	}
}

func (c *beastClient) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// accept applies the per-feed forwarding rule.
func (c *beastClient) accept(feedID string) bool {
	if IsInjectedFeed(feedID) || !c.o.Source.Forwardable(feedID) {
		return false
	}
	in := *c.inputs.Load()
	return len(in) == 0 || slices.Contains(in, feedID)
}

// handleFrame encodes one local frame (never blocks on the network).
func (c *beastClient) handleFrame(f *model.RawFrame) {
	if f.MLAT || feed.IsMLATTimestamp(f.Clock12MHz) {
		c.ctr.mlatRefused.Add(1)
		return
	}
	if f.Late || f.Kind == model.FrameStatus || !wireKind(f.Kind, len(f.Data)) || !c.accept(f.FeedID) {
		return
	}
	c.ctr.framesIn.Add(1)
	if !crcPlausible(f.Data) {
		c.ctr.badCRC.Add(1)
		return
	}
	if !c.up.Load() {
		c.ctr.dropped.Add(1)
		return
	}
	hostNs := f.HostNs
	if hostNs == 0 {
		hostNs = f.Recv.UnixNano()
	}
	c.mu.Lock()
	cfg := &c.cfg
	if cfg.Format != BeastFull && !c.red.pass(f.Data, hostNs, c.slowLevel) {
		c.mu.Unlock()
		c.ctr.reduced.Add(1)
		return
	}
	if len(c.buf) >= beastMaxPending {
		c.mu.Unlock()
		c.ctr.dropped.Add(1)
		return
	}
	var ts uint64
	if !cfg.NoTimestamps {
		ts = beastTimestamp(f)
	}
	c.buf = feed.AppendBeastFrame(c.buf, beastFrameType(len(f.Data)), ts, sigByte(f.Signal), f.Data)
	c.nBuf++
	big := len(c.buf) >= 32<<10
	c.mu.Unlock()
	if big {
		c.notify()
	}
}

// crcPlausible drops frames whose checksum is known to be bad: DF17/18 must
// have a zero parity residual and DF11 at most an interrogator code in the
// low 7 bits. Other formats carry the address in the parity and pass.
func crcPlausible(d []byte) bool {
	if len(d) < 7 {
		return true
	}
	switch d[0] >> 3 {
	case 17, 18:
		return len(d) == 14 && modes.Residual(d) == 0
	case 11:
		return modes.Residual(d)&^0x7F == 0
	}
	return true
}

func beastFrameType(n int) byte {
	switch n {
	case 2:
		return feed.BeastModeAC
	case 7:
		return feed.BeastModeSShort
	}
	return feed.BeastModeSLong
}

// beastTimestamp rebuilds the receiver's 48-bit Beast timestamp from a
// model frame: the raw 12 MHz counter, a Radarcape GPS-format second/
// nanosecond stamp, or the 12 MHz equivalent of a 20 MHz SBS counter. 0 when
// the frame has no usable timestamp.
func beastTimestamp(f *model.RawFrame) uint64 {
	const mask = 1<<48 - 1
	switch {
	case f.Clock12MHz != 0:
		return f.Clock12MHz & mask
	case f.GPSNs >= 0 && (f.ClockCls.GPS() || f.GPSNs > 0):
		ns := f.GPSNs % model.NsPerDay
		return feed.GPSTimestamp(int(ns/1e9), int(ns%1e9))
	case f.ClockCls.Timed() && f.Ticks != 0:
		switch {
		case f.ClockCls.GPS():
			ns := int64(f.Ticks % uint64(model.NsPerDay))
			return feed.GPSTimestamp(int(ns/1e9), int(ns%1e9))
		case f.ClockCls.NominalHz() == 12e6:
			return f.Ticks & mask
		case f.ClockCls.NominalHz() == 20e6:
			return (f.Ticks / 5 * 3) & mask
		}
	}
	return 0
}

// --- connection loop ---------------------------------------------------------

func (c *beastClient) setState(s string, err error, next time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != s {
		c.state, c.since = s, c.o.Now()
	}
	if err != nil {
		c.lastErr, c.lastErrAt = err.Error(), c.o.Now()
	}
	c.nextRetry = next
}

func (c *beastClient) run(ctx context.Context) {
	defer close(c.done)
	// Options.BackoffMin defaults to 1 s (tests lower it).
	bo := newBackoff(c.o.BackoffMin, max(c.o.BackoffMax, c.o.BackoffMin))
	for ctx.Err() == nil {
		c.mu.Lock()
		cfg := c.cfg
		c.mu.Unlock()
		if cfg.Disabled {
			c.setState(StateDisabled, nil, time.Time{})
			select {
			case <-ctx.Done():
				return
			case <-c.retry:
				continue
			}
		}
		c.setState(StateConnecting, nil, time.Time{})
		t0 := time.Now()
		err := c.session(ctx, cfg)
		if ctx.Err() != nil {
			break
		}
		if time.Since(t0) >= backoffResetAfter {
			bo.reset()
		}
		d := bo.next()
		c.setState(StateBackoff, err, c.o.Now().Add(d))
		if err != nil {
			c.o.Logf("uplink %s: beast %s:%d: %v (retry in %s)", c.uid, cfg.Host, cfg.Port, err, d.Round(time.Second))
		}
		select {
		case <-ctx.Done():
		case <-c.retry:
		case <-time.After(d):
		}
	}
	c.setState(StateIdle, nil, time.Time{})
}

// session runs one connection until it fails or ctx ends.
func (c *beastClient) session(ctx context.Context, cfg BeastUplink) error {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	nc, err := c.o.Dial(dctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	cancel()
	if err != nil {
		return err
	}
	defer nc.Close()
	c.ctr.connects.Add(1)

	var pre []byte
	if cfg.Format == BeastReducePlus {
		pre = feed.AppendBeastEscape(pre, feed.BeastUUID, uuid36(c.uuid))
		pre = feed.AppendBeastOption(pre, feed.BeastOptPing)
	}
	c.mu.Lock()
	c.buf, c.nBuf, c.ctl = c.buf[:0], 0, c.ctl[:0]
	c.red.reset()
	c.mu.Unlock()
	if len(pre) > 0 {
		_ = nc.SetWriteDeadline(time.Now().Add(beastWriteTimeout))
		if _, err := nc.Write(pre); err != nil {
			return err
		}
		c.ctr.bytesSent.Add(uint64(len(pre)))
	}
	c.up.Store(true)
	c.setState(StateConnected, nil, time.Time{})
	defer c.up.Store(false)

	rerr := make(chan error, 1)
	go func() { rerr <- c.reader(nc) }()
	tk := time.NewTicker(beastFlush)
	defer tk.Stop()
	var out []byte
	lastOut := time.Now()
	for {
		select {
		case <-ctx.Done():
			c.flushOnce(nc, &out) // last frames, best effort
			return nil
		case err := <-rerr:
			if err == nil || errors.Is(err, io.EOF) {
				err = errors.New("closed by the aggregator")
			}
			return err
		case <-tk.C:
		case <-c.wake:
		}
		n, err := c.flushOnce(nc, &out)
		if err != nil {
			return err
		}
		now := time.Now()
		if n > 0 {
			lastOut = now
		} else if now.Sub(lastOut) >= beastKeepAlive {
			ka := feed.AppendBeastFrame(nil, feed.BeastModeAC, 0, 0, []byte{0, 0})
			_ = nc.SetWriteDeadline(now.Add(beastWriteTimeout))
			if _, err := nc.Write(ka); err != nil {
				return err
			}
			c.ctr.bytesSent.Add(uint64(len(ka)))
			lastOut = now
		}
	}
}

// flushOnce writes the pending echoes and frames; it returns the bytes
// written.
func (c *beastClient) flushOnce(nc net.Conn, out *[]byte) (int, error) {
	c.mu.Lock()
	*out = append((*out)[:0], c.ctl...)
	*out = append(*out, c.buf...)
	frames := c.nBuf
	c.ctl, c.buf, c.nBuf = c.ctl[:0], c.buf[:0], 0
	if !c.slowUntil.IsZero() && time.Now().After(c.slowUntil) {
		c.slowUntil, c.slowLevel = time.Time{}, 0
	}
	c.mu.Unlock()
	if len(*out) == 0 {
		return 0, nil
	}
	_ = nc.SetWriteDeadline(time.Now().Add(beastWriteTimeout))
	n, err := nc.Write(*out)
	c.ctr.bytesSent.Add(uint64(n))
	if err != nil {
		c.ctr.dropped.Add(frames)
		return n, err
	}
	c.ctr.framesSent.Add(frames)
	if cap(*out) > 4*beastMaxPending {
		*out = nil
	}
	return n, nil
}

// reader handles what the aggregator sends: ping echoes and slow-down
// requests. Anything else is ignored.
func (c *beastClient) reader(nc net.Conn) error {
	p := feed.NewBeastParser(func(*feed.BeastFrame) {})
	p.OnEscape = func(typ byte, body []byte) {
		switch typ {
		case feed.BeastPing:
			if len(body) == 3 {
				c.mu.Lock()
				if len(c.ctl) < 4096 {
					c.ctl = feed.AppendBeastEscape(c.ctl, feed.BeastPing, body)
				}
				c.mu.Unlock()
				c.ctr.pingsEchoed.Add(1)
				c.notify()
			}
		case feed.BeastOption:
			if len(body) == 1 && body[0] == feed.BeastOptSlowDown {
				c.ctr.slowDowns.Add(1)
				c.mu.Lock()
				c.slowLevel = min(c.slowLevel+1, 3)
				c.slowUntil = time.Now().Add(beastSlowFor)
				c.mu.Unlock()
			}
		}
	}
	buf := make([]byte, 4096)
	for {
		n, err := nc.Read(buf)
		if n > 0 {
			p.Write(buf[:n])
		}
		if err != nil {
			return err
		}
	}
}

// uuid36 is the 0xE4 body: the UUID as written (dashes kept), padded with
// 'f' or cut to 36 characters; bytes that are not printable ASCII become
// 'f'.
func uuid36(u string) []byte {
	b := make([]byte, feed.BeastUUIDLen)
	for i := range b {
		b[i] = 'f'
	}
	n := copy(b, strings.TrimSpace(u))
	for i := 0; i < n; i++ {
		if b[i] < 0x21 || b[i] > 0x7E {
			b[i] = 'f'
		}
	}
	return b
}

// Status returns the output's status.
func (c *beastClient) Status() BeastStatus {
	c.mu.Lock()
	s := BeastStatus{ID: c.uid, Host: c.cfg.Host, Port: c.cfg.Port, Format: c.cfg.Format, State: c.state, Since: c.since,
		LastError: c.lastErr, LastErrAt: c.lastErrAt, NextRetry: c.nextRetry, ReduceMs: c.cfg.ReduceMs, SlowUntil: c.slowUntil}
	if c.cfg.Format == BeastReducePlus {
		s.UUID = c.uuid
	}
	c.mu.Unlock()
	s.FramesIn, s.FramesSent, s.Reduced = c.ctr.framesIn.Load(), c.ctr.framesSent.Load(), c.ctr.reduced.Load()
	s.Dropped, s.MLATRefused, s.BadCRC = c.ctr.dropped.Load(), c.ctr.mlatRefused.Load(), c.ctr.badCRC.Load()
	s.BytesSent, s.PingsEchoed, s.SlowDowns, s.Connects = c.ctr.bytesSent.Load(), c.ctr.pingsEchoed.Load(),
		c.ctr.slowDowns.Load(), c.ctr.connects.Load()
	return s
}
