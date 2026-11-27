package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/feederui"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/uplink"
)

// Local delivery of MLAT results (DESIGN §12.1 "MLAT results"): results
// returned by an aggregator are re-encoded as ADS-B-style Mode S frames in
// Beast format with the MLAT magic timestamp, so readsb / dump1090 /
// tar1090 / VRS show them marked as MLAT. They are written to
//
//   - a Beast listener on 127.0.0.1:<results.beastPort> (default 30105) that
//     local programs connect to (readsb: --net-connector 127.0.0.1,30105,beast_in), and
//   - optionally a readsb Beast input the feeder connects to
//     (results.readsbAddr, typically 127.0.0.1:30104).
//
// Results are never fed back into the uplink: the feeder's own feed tee
// does not see them, and the uplink refuses the magic timestamp anyway.
//
// Encoding (our own, from the public Mode S / ADS-B message formats): per
// fix two DF17 airborne-position messages (even and odd CPR, so a decoder
// can use global decoding) with type code 20-22 (GNSS height above the
// ellipsoid, the height MLAT actually solves for; the type code follows the
// horizontal uncertainty: 20 below 7.5 m, 21 below 25 m, else 22), plus a
// DF17 airborne-velocity message (subtype 1, ground speed, GNSS vertical
// rate) when the fix carries a velocity.

// mlatMagic is the Beast timestamp that marks synthetic MLAT frames
// ("\xFF\x00MLAT"; see feed.IsMLATTimestamp).
const mlatMagic uint64 = 0xFF004D4C4154

// appendBeastLong appends one escaped Beast type-'3' frame.
func appendBeastLong(dst []byte, ts uint64, sig byte, msg []byte) []byte {
	dst = append(dst, 0x1a, '3')
	put := func(b byte) {
		dst = append(dst, b)
		if b == 0x1a {
			dst = append(dst, b)
		}
	}
	for i := 5; i >= 0; i-- {
		put(byte(ts >> (8 * i)))
	}
	put(sig)
	for _, b := range msg {
		put(b)
	}
	return dst
}

// esFrame builds a DF17 frame (CA 5) with a 56-bit ME field.
func esFrame(icao uint32, me uint64) []byte {
	f := make([]byte, 14)
	f[0] = 17<<3 | 5
	f[1], f[2], f[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(me >> (8 * (6 - i)))
	}
	modes.SetParity(f, 0)
	return f
}

// posTypeCode picks the GNSS-height position type code for a horizontal
// uncertainty (NIC radius of containment 7.5 m / 25 m / larger).
func posTypeCode(sigmaM float64) uint64 {
	switch {
	case sigmaM > 0 && sigmaM < 7.5:
		return 20
	case sigmaM > 0 && sigmaM < 25:
		return 21
	}
	return 22
}

// airbornePosME returns the ME field of an airborne position message.
func airbornePosME(tc uint64, altFt float64, lat, lon float64, odd bool) uint64 {
	var alt uint64
	if a, ok := modes.EncodeAC12(int32(math.Round(altFt))); ok {
		alt = uint64(a)
	}
	cl, co := modes.CPREncode(lat, lon, odd, false)
	me := tc << 51
	me |= alt << 36
	if odd {
		me |= 1 << 34
	}
	me |= uint64(cl) << 17
	me |= uint64(co)
	return me
}

// velocityME returns the ME field of an airborne velocity message, subtype
// 1, from east/north speed in knots and the vertical rate in ft/min
// (GNSS source).
func velocityME(ewKt, nsKt, vrFpm float64) uint64 {
	comp := func(v float64) (sign, mag uint64) {
		if v < 0 {
			sign = 1
		}
		m := math.Round(math.Abs(v)) + 1
		if m > 1023 {
			m = 1023
		}
		return sign, uint64(m)
	}
	sew, vew := comp(ewKt) // sign 1 = moving west
	sns, vns := comp(nsKt) // sign 1 = moving south
	var svr, vr uint64
	if vrFpm < 0 {
		svr = 1
	}
	v := math.Round(math.Abs(vrFpm)/64) + 1
	if v > 511 {
		v = 511
	}
	vr = uint64(v)
	me := uint64(19) << 51
	me |= 1 << 48 // subtype 1
	me |= sew << 42
	me |= vew << 32
	me |= sns << 31
	me |= vns << 21
	// VrSrc bit 20 = 0: GNSS
	me |= svr << 19
	me |= vr << 10
	return me
}

// encodeFix returns the Beast bytes of one MLAT fix ("" when the fix is
// unusable: no position).
func encodeFix(dst []byte, f *model.MLATFix, now time.Time) []byte {
	u := uplink.FixUpdate(f, "", now)
	if u.Lat == nil || u.Lon == nil || f.ICAO == 0 || f.ICAO > 0xFFFFFF {
		return dst
	}
	alt := 0.0
	if u.AltGeom != nil {
		alt = *u.AltGeom
	}
	sigma := 0.0
	if u.SigmaM != nil {
		sigma = *u.SigmaM
	}
	tc := posTypeCode(sigma)
	for _, odd := range []bool{false, true} {
		dst = appendBeastLong(dst, mlatMagic, 0, esFrame(f.ICAO, airbornePosME(tc, alt, *u.Lat, *u.Lon, odd)))
	}
	if f.Vel != nil {
		e, n, up := float64(f.Vel[0]), float64(f.Vel[1]), float64(f.Vel[2])
		const mpsToKt, mpsToFpm = 1.943844492, 196.8503937
		dst = appendBeastLong(dst, mlatMagic, 0, esFrame(f.ICAO, velocityME(e*mpsToKt, n*mpsToKt, up*mpsToFpm)))
	}
	return dst
}

// resultsOut fans encoded results out to the Beast listener's clients and
// the optional readsb connection.
type resultsOut struct {
	logf func(string, ...any)

	received atomic.Uint64
	frames   atomic.Uint64

	mu       sync.Mutex
	port     int
	ln       net.Listener
	lnErr    string
	clients  map[*outClient]struct{}
	readsb   *pusher
	closed   bool
	listenAt string // host override for the listener (tests); "" = results.bindAddr
	bindAt   string // host the listener is bound to
}

type outClient struct {
	ch chan []byte
}

func newResultsOut(logf func(string, ...any)) *resultsOut {
	return &resultsOut{logf: logf, clients: map[*outClient]struct{}{}}
}

// apply (re)configures the outputs.
func (r *resultsOut) apply(c model.FeederResults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	host := r.listenAt
	if host == "" {
		host = c.BindAddr
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if c.BeastPort != r.port || host != r.bindAt || (r.ln == nil && c.BeastPort > 0) {
		if r.ln != nil {
			r.ln.Close()
			r.ln = nil
		}
		r.port, r.bindAt, r.lnErr = c.BeastPort, host, ""
		if c.BeastPort > 0 {
			ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(c.BeastPort)))
			if err != nil {
				r.lnErr = err.Error()
				r.logf("results: cannot listen on port %d: %v", c.BeastPort, err)
			} else {
				r.ln = ln
				r.logf("results: Beast output with MLAT results on %s", ln.Addr())
				go r.accept(ln)
			}
		}
	}
	addr := c.ReadsbAddr
	if r.readsb != nil && r.readsb.addr != addr {
		r.readsb.close()
		r.readsb = nil
	}
	if r.readsb == nil && addr != "" {
		r.readsb = newPusher(addr, r.logf)
	}
}

// listenAddr returns the listener address ("" when not listening).
func (r *resultsOut) listenAddr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return ""
	}
	return r.ln.Addr().String()
}

func (r *resultsOut) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		oc := &outClient{ch: make(chan []byte, 256)}
		r.mu.Lock()
		if r.closed || r.ln != ln {
			r.mu.Unlock()
			c.Close()
			return
		}
		r.clients[oc] = struct{}{}
		r.mu.Unlock()
		go r.serve(c, oc, ln)
	}
}

func (r *resultsOut) serve(c net.Conn, oc *outClient, ln net.Listener) {
	defer func() {
		r.mu.Lock()
		delete(r.clients, oc)
		r.mu.Unlock()
		c.Close()
	}()
	// a reader notices the peer closing (and discards anything it sends)
	gone := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := c.Read(buf); err != nil {
				close(gone)
				return
			}
		}
	}()
	for {
		select {
		case b, ok := <-oc.ch:
			if !ok {
				return
			}
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := c.Write(b); err != nil {
				return
			}
		case <-gone:
			return
		}
	}
}

// handle is uplink.Options.OnResults.
func (r *resultsOut) handle(_ string, fixes []model.MLATFix) {
	r.received.Add(uint64(len(fixes)))
	now := time.Now()
	var b []byte
	n := 0
	for i := range fixes {
		before := len(b)
		b = encodeFix(b, &fixes[i], now)
		if len(b) > before {
			n += 2
			if fixes[i].Vel != nil {
				n++
			}
		}
	}
	if len(b) == 0 {
		return
	}
	r.mu.Lock()
	targets := 0
	for oc := range r.clients {
		select {
		case oc.ch <- b:
			targets++
		default: // slow client: drop this batch for it
		}
	}
	p := r.readsb
	r.mu.Unlock()
	if p != nil && p.send(b) {
		targets++
	}
	if targets > 0 {
		r.frames.Add(uint64(n * targets))
	}
}

func (r *resultsOut) status() feederui.ResultsStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := feederui.ResultsStatus{BeastPort: r.port, Listening: r.ln != nil, ListenErr: r.lnErr, Clients: len(r.clients),
		Received: r.received.Load(), Frames: r.frames.Load()}
	if r.readsb != nil {
		s.ReadsbAddr = r.readsb.addr
		s.ReadsbUp, s.ReadsbErr = r.readsb.state()
	}
	return s
}

func (r *resultsOut) close() {
	r.mu.Lock()
	r.closed = true
	if r.ln != nil {
		r.ln.Close()
		r.ln = nil
	}
	for oc := range r.clients {
		close(oc.ch)
	}
	r.clients = map[*outClient]struct{}{}
	p := r.readsb
	r.readsb = nil
	r.mu.Unlock()
	if p != nil {
		p.close()
	}
}

// pusher keeps a TCP connection to a readsb Beast input and writes result
// batches to it. It reconnects with backoff (1 s .. 30 s).
type pusher struct {
	addr   string
	logf   func(string, ...any)
	ch     chan []byte
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	up      bool
	lastErr string
}

func newPusher(addr string, logf func(string, ...any)) *pusher {
	ctx, cancel := context.WithCancel(context.Background())
	p := &pusher{addr: addr, logf: logf, ch: make(chan []byte, 256), cancel: cancel, done: make(chan struct{})}
	go p.run(ctx)
	return p
}

func (p *pusher) send(b []byte) bool {
	p.mu.Lock()
	up := p.up
	p.mu.Unlock()
	if !up {
		return false
	}
	select {
	case p.ch <- b:
		return true
	default:
		return false
	}
}

func (p *pusher) state() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.up, p.lastErr
}

func (p *pusher) set(up bool, err error) {
	p.mu.Lock()
	p.up = up
	if err != nil {
		p.lastErr = err.Error()
	} else if up {
		p.lastErr = ""
	}
	p.mu.Unlock()
}

func (p *pusher) run(ctx context.Context) {
	defer close(p.done)
	backoff := time.Second
	for ctx.Err() == nil {
		var d net.Dialer
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		c, err := d.DialContext(dctx, "tcp", p.addr)
		cancel()
		if err == nil {
			p.logf("results: connected to readsb input %s", p.addr)
			p.set(true, nil)
			backoff = time.Second
			err = p.pump(ctx, c)
			p.set(false, err)
			c.Close()
			if ctx.Err() != nil {
				return
			}
			p.logf("results: readsb input %s: %v", p.addr, err)
		} else {
			p.set(false, err)
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff)/5+1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (p *pusher) pump(ctx context.Context, c net.Conn) error {
	gone := make(chan error, 1)
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := c.Read(buf); err != nil {
				gone <- err
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-gone:
			if err == nil {
				err = errors.New("closed")
			}
			return fmt.Errorf("connection closed: %w", err)
		case b := <-p.ch:
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := c.Write(b); err != nil {
				return err
			}
		}
	}
}

func (p *pusher) close() {
	p.cancel()
	<-p.done
}

// splitHostPortDefault splits host:port; a bare host gets def.
func splitHostPortDefault(s string, def int) (string, int, error) {
	host, ps, err := net.SplitHostPort(s)
	if err != nil {
		if _, perr := strconv.Atoi(s); perr == nil || s == "" {
			return "", 0, err
		}
		return s, def, nil
	}
	p, err := strconv.Atoi(ps)
	if err != nil || p < 1 || p > 65535 {
		return "", 0, fmt.Errorf("bad port %q", ps)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host, p, nil
}
