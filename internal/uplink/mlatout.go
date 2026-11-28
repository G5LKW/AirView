package uplink

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/feed"
	"airview/internal/geo"
	mgeo "airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/modes"
)

// MLAT output: the client side of the mlat-client protocol, so a station
// can take part in the multilateration of a community network (adsb.lol,
// adsb.fi, airplanes.live, ADS-B Exchange, ...) next to a Beast output to
// the same network. Written from AirView's own protocol description
// (docs/aggregator/MLATC.md, which internal/ingest/mlatc implements on the
// server side); no code of mlat-client or any other GPL program was
// consulted or copied.
//
// What it does:
//
//   - handshake: one JSON line padded with spaces; offers zlib (client
//     direction compressed: one continuous zlib stream, sync-flushed chunks
//     without the trailing 00 00 FF FF, each prefixed with a u16 length) and
//     none; the server direction is plain newline-delimited JSON.
//   - tells the server which aircraft it receives (seen / lost) and their
//     ADS-B position rates (rate_report), and sends for the aircraft the
//     server asks for (start_sending / stop_sending) the DF17 airborne
//     positions as sync pairs (or single ssync messages when the server
//     offers split_sync) and the other Mode S replies as mlat messages, with
//     the receiver's timestamps moved to the start of the frame (MLATC.md
//     §5.1): 12 MHz counters as clock_type "dump1090", a Radarcape's GPS
//     timestamps as clock_type "radarcape_gps" (nanoseconds of the UTC day).
//   - receives results (ECEF) and injects them into the local tracker as
//     external MLAT positions; they are never forwarded anywhere.
//
// Clocks: the handshake names one clock type, so one MLAT output carries
// one kind of timestamp. It follows the clock of its inputs: before the
// first timed frame it connects as "dump1090"; when the frames are of the
// other kind and none of the session's kind arrived for a minute it
// reconnects with the right type. GPS timestamps are only sent while the
// receiver reports GPS lock with UTC (class gps_utc); emulated (no lock) and
// GPS-time stamps are counted and skipped. Frames without any timestamp are
// counted and skipped. The receiver position is required by every MLAT
// server and is sent (that is what MLAT needs); the UI says so.

// DefaultMLATPort is the usual port of an mlat-server.
const DefaultMLATPort = 31090

// MLATUplink configures one MLAT output.
type MLATUplink struct {
	ID      string   `json:"id"`
	Host    string   `json:"host"`
	Port    int      `json:"port"`
	User    string   `json:"user,omitempty"` // display name at the network
	UUID    string   `json:"uuid,omitempty"` // "" = the per-uplink generated UUID
	Inputs  []string `json:"inputs,omitempty"`
	Privacy bool     `json:"privacy,omitempty"` // ask the network to hide the station on public maps
	// Lat, Lon (degrees) and AltM (metres) are the receiver position; 0,0 =
	// ask Options.Position when connecting.
	Lat, Lon, AltM float64
	Disabled       bool `json:"disabled,omitempty"`
}

// Normalize applies defaults and limits.
func (u *MLATUplink) Normalize(path string) []string {
	var notes []string
	u.Host = strings.TrimSpace(u.Host)
	if u.Port == 0 {
		u.Port = DefaultMLATPort
	} else if u.Port < 1 || u.Port > 65535 {
		notes = append(notes, fmt.Sprintf("%s.port %d is out of range; using %d", path, u.Port, DefaultMLATPort))
		u.Port = DefaultMLATPort
	}
	if len(u.User) > 64 {
		u.User = u.User[:64]
	}
	return notes
}

// Validate reports a configuration that cannot work.
func (u *MLATUplink) Validate() error {
	switch {
	case u.Host == "" && !u.Disabled:
		return errors.New("no host")
	case !u.Disabled && (u.Lat != 0 || u.Lon != 0) && !geo.ValidLatLon(u.Lat, u.Lon):
		return errors.New("the receiver position is invalid")
	}
	return nil
}

func (u MLATUplink) clone() MLATUplink {
	u.Inputs = slices.Clone(u.Inputs)
	return u
}

func (u MLATUplink) connKey() string {
	return strings.Join([]string{u.Host, strconv.Itoa(u.Port), u.User, u.UUID, strconv.FormatBool(u.Privacy), strconv.FormatBool(u.Disabled),
		strconv.FormatFloat(u.Lat, 'f', 5, 64), strconv.FormatFloat(u.Lon, 'f', 5, 64), strconv.FormatFloat(u.AltM, 'f', 0, 64)}, "|")
}

// MLATStatus is the status of one MLAT output.
type MLATStatus struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
	LastError string    `json:"lastError,omitempty"`
	NextRetry time.Time `json:"nextRetry,omitzero"`
	Compress  string    `json:"compress,omitempty"`
	SplitSync bool      `json:"splitSync,omitempty"`
	MOTD      string    `json:"motd,omitempty"`
	Denied    []string  `json:"denied,omitempty"`

	Seen      int    `json:"seen"`      // aircraft announced
	Requested int    `json:"requested"` // aircraft the server asked for
	SyncSent  uint64 `json:"syncSent"`
	MLATSent  uint64 `json:"mlatSent"`
	Results   uint64 `json:"results"`
	NoClock   uint64 `json:"noClock"` // frames of requested aircraft without a usable timestamp (skipped)
	// ClockType is the clock type of the current / last session: "dump1090"
	// (12 MHz) or "radarcape_gps".
	ClockType string `json:"clockType,omitempty"`
	// OtherClock counts frames whose clock is not the session's (mixed inputs).
	OtherClock uint64 `json:"otherClock,omitempty"`
	// NoGPSLock counts GPS-format frames without GPS lock / UTC (skipped).
	NoGPSLock  uint64  `json:"noGpsLock,omitempty"`
	BytesSent  uint64  `json:"bytesSent"`
	PeerCount  int     `json:"peerCount,omitempty"` // from the server's stats
	OutlierPct float64 `json:"outlierPercent,omitempty"`
	Connects   uint64  `json:"connects"`
}

type mlatCounters struct {
	syncSent, mlatSent, results, noClock, bytesSent, connects atomic.Uint64
	otherClock, noGPSLock                                     atomic.Uint64
}

type mlatAc struct {
	heard   int64 // Unix ms
	posMs   int64 // last DF17 airborne position
	nPos    int   // positions in the current rate window
	evenT   uint64
	evenHex string
	evenMs  int64
	oddT    uint64
	oddHex  string
	oddMs   int64
	pending bool // announced in the next "seen"
}

// mlatClient runs one MLAT output.
type mlatClient struct {
	o   *Options
	uid string

	inputs atomic.Pointer[[]string]
	up     atomic.Bool
	detect atomic.Int32 // clockKind of the latest timed input frame

	mu        sync.Mutex
	cfg       MLATUplink
	uuid      string
	ac        map[uint32]*mlatAc
	req       map[uint32]bool
	split     bool
	lines     []byte    // JSON lines waiting for the writer
	clockMsg  bool      // a clock_reset is due
	sessClock clockKind // clock type of the current session
	ownClock  int64     // Unix ms of the last frame with the session's clock
	reclock   bool      // the inputs' clock differs: reconnect with the right type
	state     string
	since     time.Time
	lastErr   string
	nextRetry time.Time
	compress  string
	motd      string
	denied    []string
	peers     int
	outliers  float64

	retry  chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
	ctr    mlatCounters
}

const (
	mlatMaxPending   = 1 << 20
	mlatFlushEvery   = 200 * time.Millisecond
	mlatHousekeeping = 30 * time.Second
	mlatLostAfter    = 60 * time.Second
	mlatPairWindowMs = 5000
	mlatPosFreshMs   = 10_000
	mlatChunk        = 32 << 10
	mlatIdleTimeout  = 90 * time.Second
	ticks12ModeS     = 768     // start-of-frame correction of a 12 MHz Mode S stamp (MLATC.md §5.1)
	gpsShortNs       = 64_000  // start-of-frame correction of a GPS stamp, 56-bit reply (8 µs preamble + 56 µs)
	gpsLongNs        = 120_000 // ... 112-bit reply (8 + 112 µs)
	mlatReclockAfter = 60_000  // ms without a frame of the session's clock before reconnecting
)

// clockKind is the timestamp kind of a frame / session.
type clockKind int32

const (
	clockNone  clockKind = iota
	clock12MHz           // clock_type "dump1090"
	clockGPS             // clock_type "radarcape_gps"
)

func (k clockKind) handshake() string {
	if k == clockGPS {
		return "radarcape_gps"
	}
	return "dump1090"
}

// errReclock ends a session whose clock type no longer matches the inputs.
var errReclock = errors.New("the receiver clock changed; reconnecting with the matching clock type")

func newMLATClient(o *Options, cfg MLATUplink) *mlatClient {
	c := &mlatClient{o: o, uid: cfg.ID, cfg: cfg.clone(), state: StateIdle, since: o.Now(),
		ac: map[uint32]*mlatAc{}, req: map[uint32]bool{}, retry: make(chan struct{}, 1), done: make(chan struct{})}
	c.uuid = beastUUID(o.StateDir, BeastUplink{ID: cfg.ID, UUID: cfg.UUID}, o.Logf)
	c.setInputs(cfg.Inputs)
	return c
}

func (c *mlatClient) setInputs(in []string) {
	cp := slices.Clone(in)
	c.inputs.Store(&cp)
}

func (c *mlatClient) start() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.run(ctx)
}

func (c *mlatClient) close() {
	if c.cancel != nil {
		c.cancel()
		<-c.done
	}
}

func (c *mlatClient) update(cfg MLATUplink) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg.clone()
	c.setInputs(cfg.Inputs)
}

// Retry cuts a backoff short.
func (c *mlatClient) Retry() {
	select {
	case c.retry <- struct{}{}:
	default:
	}
}

func (c *mlatClient) accept(feedID string) bool {
	if IsInjectedFeed(feedID) || !c.o.Source.Forwardable(feedID) {
		return false
	}
	in := *c.inputs.Load()
	return len(in) == 0 || slices.Contains(in, feedID)
}

// ticks12 returns the 12 MHz timestamp of a frame (ok=false without one).
func ticks12(f *model.RawFrame) (uint64, bool) {
	const mask = 1<<48 - 1
	switch {
	case f.Clock12MHz != 0:
		return f.Clock12MHz & mask, true
	case f.ClockCls.Timed() && f.Ticks != 0 && !f.ClockCls.GPS() && f.ClockCls.NominalHz() == 12e6:
		return f.Ticks & mask, true
	}
	return 0, false
}

// frameTime returns the clock kind of a frame and its start-of-frame
// timestamp in the units of that clock (MLATC.md §5.1). noLock is set for a
// GPS-format stamp without GPS lock / UTC.
func frameTime(f *model.RawFrame) (k clockKind, t uint64, noLock bool) {
	if ts, ok := ticks12(f); ok {
		if ts > ticks12ModeS {
			t = ts - ticks12ModeS
		}
		return clock12MHz, t, false
	}
	gps := f.GPSNs >= 0 || f.ClockCls.GPS()
	if !gps {
		return clockNone, 0, false
	}
	if f.ClockCls != model.ClockGPSUTC {
		return clockNone, 0, true
	}
	ns := f.GPSNs
	if ns < 0 {
		if f.Ticks == 0 {
			return clockNone, 0, false
		}
		ns = int64(f.Ticks % uint64(model.NsPerDay))
	}
	if ns >= model.NsPerDay {
		return clockNone, 0, false
	}
	off := int64(gpsShortNs)
	if f.Kind == model.FrameModeSLong {
		off = gpsLongNs
	}
	return clockGPS, uint64((ns - off + model.NsPerDay) % model.NsPerDay), false
}

// detected is the clock kind of the latest timed frame of the inputs.
func (c *mlatClient) detectedClock() clockKind { return clockKind(c.detect.Load()) }

// handleFrame turns one local frame into protocol messages (never blocks).
func (c *mlatClient) handleFrame(f *model.RawFrame) {
	if !c.up.Load() || f.MLAT || feed.IsMLATTimestamp(f.Clock12MHz) || f.Late {
		return
	}
	if f.Kind != model.FrameModeSShort && f.Kind != model.FrameModeSLong || !c.accept(f.FeedID) {
		return
	}
	var m modes.Message
	if err := modes.DecodeOpts(f.Data, &m, modes.Options{NoCommB: true}); err != nil || m.CRC == modes.CRCBad || m.CRC == modes.CRCUnknown {
		return
	}
	kind, t, noLock := frameTime(f)
	if kind != clockNone {
		c.detect.Store(int32(kind))
	}
	nowMs := f.Recv.UnixMilli()
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.ac[m.ICAO]
	verified := m.CRC == modes.CRCOK && (m.DF == 17 || m.DF == 11)
	if a == nil {
		if !verified {
			return // an address from the parity alone is not proof the aircraft exists
		}
		a = &mlatAc{pending: true}
		c.ac[m.ICAO] = a
	}
	a.heard = nowMs
	airbornePos := m.DF == 17 && m.Valid&modes.FCPR != 0 && !m.CPRSurface
	if airbornePos {
		a.posMs = nowMs
		a.nPos++
	}
	if !c.req[m.ICAO] {
		return
	}
	switch {
	case noLock:
		c.ctr.noGPSLock.Add(1)
		return
	case kind == clockNone:
		c.ctr.noClock.Add(1)
		return
	case kind != c.sessClock:
		c.ctr.otherClock.Add(1)
		if nowMs-c.ownClock > mlatReclockAfter {
			c.reclock = true
		}
		return
	}
	c.ownClock = nowMs
	hx := hex.EncodeToString(f.Data)
	switch {
	case airbornePos && m.CRC == modes.CRCOK:
		if c.split {
			c.line(`{"ssync":{"t":%d,"m":"%s"}}`, t, hx)
			c.ctr.syncSent.Add(1)
			return
		}
		if m.CPROdd {
			a.oddT, a.oddHex, a.oddMs = t, hx, nowMs
		} else {
			a.evenT, a.evenHex, a.evenMs = t, hx, nowMs
		}
		if a.evenHex != "" && a.oddHex != "" && absMs(a.evenMs-a.oddMs) <= mlatPairWindowMs {
			c.line(`{"sync":{"et":%d,"em":"%s","ot":%d,"om":"%s"}}`, a.evenT, a.evenHex, a.oddT, a.oddHex)
			c.ctr.syncSent.Add(1)
			a.evenHex, a.oddHex = "", ""
		}
	case m.DF == 0 || m.DF == 4 || m.DF == 5 || m.DF == 11 || m.DF == 16 || m.DF == 20 || m.DF == 21:
		if nowMs-a.posMs <= mlatPosFreshMs {
			return // it has a good ADS-B position: nothing to multilaterate
		}
		c.line(`{"mlat":{"t":%d,"m":"%s"}}`, t, hx)
		c.ctr.mlatSent.Add(1)
	}
}

func absMs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// line appends one JSON line (caller holds mu); dropped beyond the cap.
func (c *mlatClient) line(format string, a ...any) {
	if len(c.lines) >= mlatMaxPending {
		return
	}
	c.lines = fmt.Appendf(c.lines, format, a...)
	c.lines = append(c.lines, '\n')
}

// handleClockEvent notes a timestamp discontinuity of an input.
func (c *mlatClient) handleClockEvent(feedID string) {
	if !c.up.Load() || !c.accept(feedID) {
		return
	}
	c.mu.Lock()
	c.clockMsg = true
	c.mu.Unlock()
}

// housekeeping adds seen / lost / rate_report / heartbeat lines.
func (c *mlatClient) housekeeping(now time.Time, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clockMsg {
		c.clockMsg = false
		c.line(`{"clock_reset":{"reason":"receiver timestamps changed"}}`)
		for _, a := range c.ac {
			a.evenHex, a.oddHex = "", ""
		}
	}
	var seen []string
	for k, a := range c.ac {
		if a.pending {
			a.pending = false
			seen = append(seen, fmt.Sprintf("%06x", k&0xFFFFFF))
		}
	}
	if len(seen) > 0 {
		b, _ := json.Marshal(map[string][]string{"seen": seen})
		c.line("%s", b)
	}
	if !full {
		return
	}
	nowMs := now.UnixMilli()
	var lost []string
	rates := map[string]float64{}
	for k, a := range c.ac {
		h := fmt.Sprintf("%06x", k&0xFFFFFF)
		if nowMs-a.heard > mlatLostAfter.Milliseconds() {
			lost = append(lost, h)
			delete(c.ac, k)
			delete(c.req, k)
			continue
		}
		if a.nPos > 0 {
			rates[strings.ToUpper(h)] = math.Round(float64(a.nPos)/mlatHousekeeping.Seconds()*100) / 100
			a.nPos = 0
		}
	}
	if len(lost) > 0 {
		b, _ := json.Marshal(map[string][]string{"lost": lost})
		c.line("%s", b)
	}
	b, _ := json.Marshal(map[string]any{"rate_report": rates})
	c.line("%s", b)
	c.line(`{"heartbeat":{"client_time":%.3f}}`, float64(now.UnixNano())/1e9)
}

// --- connection loop ---------------------------------------------------------

func (c *mlatClient) setState(s string, err error, next time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != s {
		c.state, c.since = s, c.o.Now()
	}
	if err != nil {
		c.lastErr = err.Error()
	}
	c.nextRetry = next
}

// errDenied carries the server's reconnect delay.
type errDenied struct {
	reasons []string
	wait    time.Duration
}

func (e *errDenied) Error() string { return "denied by the server: " + strings.Join(e.reasons, "; ") }

func (c *mlatClient) run(ctx context.Context) {
	defer close(c.done)
	bo := newBackoff(max(c.o.BackoffMin, time.Second), max(c.o.BackoffMax, c.o.BackoffMin, 5*time.Minute))
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
		c.up.Store(false)
		if ctx.Err() != nil {
			break
		}
		if time.Since(t0) >= backoffResetAfter {
			bo.reset()
		}
		d := bo.next()
		if errors.Is(err, errReclock) {
			d = 0
			bo.reset()
		}
		var den *errDenied
		if errors.As(err, &den) && den.wait > d {
			d = den.wait
		}
		c.setState(StateBackoff, err, c.o.Now().Add(d))
		if err != nil {
			c.o.Logf("uplink %s: mlat %s:%d: %v (retry in %s)", c.uid, cfg.Host, cfg.Port, err, d.Round(time.Second))
		}
		select {
		case <-ctx.Done():
		case <-c.retry:
		case <-time.After(d):
		}
	}
	c.setState(StateIdle, nil, time.Time{})
}

// hello builds the handshake line.
func (c *mlatClient) hello(cfg MLATUplink, clock clockKind) []byte {
	user := cfg.User
	if user == "" {
		user = "airview"
	}
	h := map[string]any{
		"version": 3, "client_version": model.AppName + "/" + model.Version,
		"compress": []string{"zlib", "none"}, "selective_traffic": true, "heartbeat": true,
		"return_results": true, "return_result_format": "ecef", "return_stats": true, "udp_transport": false,
		"uuid": c.uuid, "user": user, "lat": round5(cfg.Lat), "lon": round5(cfg.Lon), "alt": math.Round(cfg.AltM),
		"clock_type": clock.handshake(), "privacy": cfg.Privacy,
	}
	b, _ := json.Marshal(h)
	b = append(b, bytes.Repeat([]byte(" "), 128)...)
	return append(b, '\n')
}

func round5(v float64) float64 { return math.Round(v*1e5) / 1e5 }

// session runs one connection until it fails or ctx ends.
func (c *mlatClient) session(ctx context.Context, cfg MLATUplink) error {
	if cfg.Lat == 0 && cfg.Lon == 0 && c.o.Position != nil {
		if la, lo, alt, ok := c.o.Position(); ok {
			cfg.Lat, cfg.Lon, cfg.AltM = la, lo, alt
		}
	}
	if (cfg.Lat == 0 && cfg.Lon == 0) || !geo.ValidLatLon(cfg.Lat, cfg.Lon) {
		return errors.New("the receiver position is not set: MLAT needs it (Settings > Station)")
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	nc, err := c.o.Dial(dctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	cancel()
	if err != nil {
		return err
	}
	defer nc.Close()
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	defer stop()
	_ = nc.SetDeadline(time.Now().Add(20 * time.Second))
	clock := c.detectedClock()
	if clock == clockNone {
		clock = clock12MHz
	}
	if _, err := nc.Write(c.hello(cfg, clock)); err != nil {
		return err
	}
	br := bufio.NewReaderSize(nc, 64<<10)
	reply, err := readLine(br, 64<<10)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	var rep struct {
		Compress    string          `json:"compress"`
		Deny        []string        `json:"deny"`
		ReconnectIn float64         `json:"reconnect_in"`
		SplitSync   bool            `json:"split_sync"`
		MOTD        string          `json:"motd"`
		UDP         json.RawMessage `json:"udp_transport"`
	}
	if err := json.Unmarshal(reply, &rep); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if len(rep.Deny) > 0 {
		c.mu.Lock()
		c.denied = rep.Deny
		c.mu.Unlock()
		return &errDenied{reasons: rep.Deny, wait: time.Duration(math.Min(rep.ReconnectIn, 86400) * float64(time.Second))}
	}
	if rep.Compress != "zlib" && rep.Compress != "none" {
		return fmt.Errorf("handshake: the server chose compression %q, which was not offered", rep.Compress)
	}
	_ = nc.SetDeadline(time.Time{})
	c.mu.Lock()
	c.compress, c.split, c.motd, c.denied = rep.Compress, rep.SplitSync, rep.MOTD, nil
	c.sessClock, c.ownClock, c.reclock = clock, c.o.Now().UnixMilli(), false
	c.lines = c.lines[:0]
	c.req = map[uint32]bool{}
	for _, a := range c.ac {
		a.pending, a.evenHex, a.oddHex = true, "", ""
	}
	c.line(`{"rate_report":{}}`)
	c.mu.Unlock()
	c.ctr.connects.Add(1)
	c.up.Store(true)
	c.setState(StateConnected, nil, time.Time{})
	if rep.MOTD != "" {
		c.o.Logf("uplink %s: mlat server says: %s", c.uid, rep.MOTD)
	}

	rerr := make(chan error, 1)
	go func() { rerr <- c.reader(nc, br) }()
	w := &mlatWriter{nc: nc, zlib: rep.Compress == "zlib"}
	tick := time.NewTicker(mlatFlushEvery)
	defer tick.Stop()
	lastHK := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-rerr:
			return err
		case now := <-tick.C:
			full := now.Sub(lastHK) >= mlatHousekeeping
			if full {
				lastHK = now
			}
			c.housekeeping(now, full)
			c.mu.Lock()
			out, reclock := c.lines, c.reclock
			c.lines = nil
			c.mu.Unlock()
			if reclock {
				return errReclock
			}
			if len(out) == 0 {
				continue
			}
			_ = nc.SetWriteDeadline(time.Now().Add(15 * time.Second))
			n, err := w.write(out)
			c.ctr.bytesSent.Add(uint64(n))
			if err != nil {
				return err
			}
		}
	}
}

// mlatWriter frames the client direction (MLATC.md §3).
type mlatWriter struct {
	nc   net.Conn
	zlib bool
	buf  bytes.Buffer
	zw   *zlib.Writer
}

// write sends lines (complete JSON lines) and returns the bytes written.
func (w *mlatWriter) write(lines []byte) (int, error) {
	if !w.zlib {
		return w.nc.Write(lines)
	}
	if w.zw == nil {
		zw, err := zlib.NewWriterLevel(&w.buf, zlib.BestSpeed)
		if err != nil {
			return 0, err
		}
		w.zw = zw
	}
	total := 0
	for len(lines) > 0 {
		n := min(len(lines), mlatChunk)
		// cut at a line end when possible (lines may span chunks, but this keeps them short)
		if i := bytes.LastIndexByte(lines[:n], '\n'); i >= 0 && n < len(lines) {
			n = i + 1
		}
		if _, err := w.zw.Write(lines[:n]); err != nil {
			return total, err
		}
		if err := w.zw.Flush(); err != nil {
			return total, err
		}
		lines = lines[n:]
		chunk := w.buf.Bytes()
		chunk = bytes.TrimSuffix(chunk, []byte{0, 0, 0xFF, 0xFF})
		if len(chunk) > 0xFFFF {
			return total, errors.New("compressed chunk too large")
		}
		hdr := binary.BigEndian.AppendUint16(nil, uint16(len(chunk)))
		k, err := w.nc.Write(append(hdr, chunk...))
		total += k
		w.buf.Reset()
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// readLine reads one line (without the newline) of at most limit bytes.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		frag, isPrefix, err := br.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, frag...)
		if len(out) > limit {
			return nil, errors.New("line too long")
		}
		if !isPrefix {
			return out, nil
		}
	}
}

// reader handles the server's messages.
func (c *mlatClient) reader(nc net.Conn, br *bufio.Reader) error {
	for {
		_ = nc.SetReadDeadline(time.Now().Add(mlatIdleTimeout))
		ln, err := readLine(br, 1<<20)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("the server closed the connection")
			}
			return err
		}
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal(ln, &msg) != nil {
			continue
		}
		for k, v := range msg {
			c.serverMsg(k, v)
		}
	}
}

func parseHexList(v json.RawMessage) []uint32 {
	var hs []string
	if json.Unmarshal(v, &hs) != nil {
		return nil
	}
	out := make([]uint32, 0, len(hs))
	for _, h := range hs {
		if n, err := strconv.ParseUint(strings.TrimSpace(h), 16, 32); err == nil && n <= 0xFFFFFF {
			out = append(out, uint32(n))
		}
	}
	return out
}

func (c *mlatClient) serverMsg(kind string, v json.RawMessage) {
	switch kind {
	case "start_sending":
		c.mu.Lock()
		for _, a := range parseHexList(v) {
			c.req[a] = true
		}
		c.mu.Unlock()
	case "stop_sending":
		c.mu.Lock()
		for _, a := range parseHexList(v) {
			delete(c.req, a)
		}
		c.mu.Unlock()
	case "stats":
		var st struct {
			Peers    int     `json:"peer_count"`
			Outliers float64 `json:"outlier_percent"`
		}
		if json.Unmarshal(v, &st) == nil {
			c.mu.Lock()
			c.peers, c.outliers = st.Peers, st.Outliers
			c.mu.Unlock()
		}
	case "result":
		if u, ok := mlatResult(v, FeedPrefix+"mlat/"+c.uid, c.o.Now()); ok {
			c.ctr.results.Add(1)
			if c.o.Inject != nil {
				c.o.Inject.HandleUpdate(u)
			}
		}
	}
}

// mlatResult converts a server result (ecef format, MLATC.md §6.1).
func mlatResult(v json.RawMessage, feedID string, now time.Time) (model.DecodedUpdate, bool) {
	var r struct {
		T        float64   `json:"@"`
		Addr     string    `json:"addr"`
		ECEF     []float64 `json:"ecef"`
		Cov      []float64 `json:"cov"`
		ND       int       `json:"nd"`
		EW       *float64  `json:"ewvel"`
		NS       *float64  `json:"nsvel"`
		VRate    *float64  `json:"vrate"`
		Callsign string    `json:"callsign"`
		Squawk   string    `json:"squawk"`
		Lat      *float64  `json:"lat"`
		Lon      *float64  `json:"lon"`
		Alt      *float64  `json:"alt"`
		GDOP     *float64  `json:"gdop"`
	}
	if json.Unmarshal(v, &r) != nil {
		return model.DecodedUpdate{}, false
	}
	icao, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(r.Addr), "~"), 16, 32)
	if err != nil || icao > 0xFFFFFF {
		return model.DecodedUpdate{}, false
	}
	u := model.DecodedUpdate{Recv: now, FeedID: feedID, ICAO: uint32(icao), MLAT: true, Msgs: 1, Src: model.SrcMLATExt}
	if r.T > 1e9 && math.Abs(r.T-float64(now.Unix())) < 60 {
		u.Recv = time.Unix(0, int64(r.T*1e9))
	}
	switch {
	case len(r.ECEF) == 3:
		llh := mgeo.FromECEF(mgeo.Vec3{X: r.ECEF[0], Y: r.ECEF[1], Z: r.ECEF[2]})
		if !geo.ValidLatLon(llh.Lat, llh.Lon) {
			return model.DecodedUpdate{}, false
		}
		u.Lat, u.Lon, u.AltGeom = model.F(llh.Lat), model.F(llh.Lon), model.F(math.Round(llh.H*mToFt))
		if len(r.Cov) == 6 {
			fr := mgeo.NewFrame(llh)
			c := r.Cov
			enu := fr.CovToENU([9]float64{c[0], c[1], c[2], c[1], c[3], c[4], c[2], c[4], c[5]})
			u.SigmaM = model.F(math.Sqrt(math.Max(0, enu[0]+enu[4])))
		}
	case r.Lat != nil && r.Lon != nil && geo.ValidLatLon(*r.Lat, *r.Lon):
		u.Lat, u.Lon = model.F(*r.Lat), model.F(*r.Lon)
		if r.Alt != nil {
			u.AltBaro = model.F(*r.Alt)
		}
	default:
		return model.DecodedUpdate{}, false
	}
	if r.EW != nil && r.NS != nil {
		u.GS = model.F(math.Hypot(*r.EW, *r.NS))
		trk := math.Atan2(*r.EW, *r.NS) * 180 / math.Pi
		if trk < 0 {
			trk += 360
		}
		u.Track = model.F(trk)
	}
	if r.VRate != nil {
		u.VertRate = model.F(*r.VRate)
	}
	return u, true
}

// Status returns the output's status.
func (c *mlatClient) Status() MLATStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return MLATStatus{ID: c.uid, Host: c.cfg.Host, Port: c.cfg.Port, State: c.state, Since: c.since, LastError: c.lastErr,
		NextRetry: c.nextRetry, Compress: c.compress, SplitSync: c.split, MOTD: c.motd, Denied: slices.Clone(c.denied),
		Seen: len(c.ac), Requested: len(c.req), SyncSent: c.ctr.syncSent.Load(), MLATSent: c.ctr.mlatSent.Load(),
		Results: c.ctr.results.Load(), NoClock: c.ctr.noClock.Load(), BytesSent: c.ctr.bytesSent.Load(),
		PeerCount: c.peers, OutlierPct: c.outliers, Connects: c.ctr.connects.Load(),
		OtherClock: c.ctr.otherClock.Load(), NoGPSLock: c.ctr.noGPSLock.Load(), ClockType: c.statusClock()}
}

// statusClock is the clock type of the current session ("" before the first).
func (c *mlatClient) statusClock() string {
	if c.sessClock == clockNone {
		return ""
	}
	return c.sessClock.handshake()
}
