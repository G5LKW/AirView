package simfeed

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/uplink"
)

// ------------------------------------------------------------- results

// Results records the MLAT results one feeder received.
type Results struct {
	mu     sync.Mutex
	byICAO map[uint32]int
	grades map[uint8]int
	n      int
	first  time.Time
}

func newResults() *Results { return &Results{byICAO: map[uint32]int{}, grades: map[uint8]int{}} }

func (r *Results) add(icao uint32, grade uint8) {
	r.mu.Lock()
	if r.n == 0 {
		r.first = time.Now()
	}
	r.byICAO[icao&0xFFFFFF]++
	r.grades[grade]++
	r.n++
	r.mu.Unlock()
}

// Count returns the number of results received.
func (r *Results) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// Aircraft returns the addresses results were received for, with counts.
func (r *Results) Aircraft() map[uint32]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uint32]int, len(r.byICAO))
	for k, v := range r.byICAO {
		out[k] = v
	}
	return out
}

// Grades returns the result counts per grade (mlatc results carry none:
// they are counted under model.GradeNone).
func (r *Results) Grades() map[uint8]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uint8]int, len(r.grades))
	for k, v := range r.grades {
		out[k] = v
	}
	return out
}

// ------------------------------------------------------------- heard

// Heard records, per receiver, when it last heard each aircraft (simulated
// arrival time, seconds since the network start). Attach it with
// AttachHeard before the first Advance.
type Heard struct {
	mu sync.Mutex
	m  []map[uint32]float64
}

type heardSink struct {
	h  *Heard
	rx int
}

func (s heardSink) Observe(o *simnet.Observation) {
	s.h.mu.Lock()
	s.h.m[s.rx][o.ICAO&0xFFFFFF] = o.ArrivalS
	s.h.mu.Unlock()
}
func (heardSink) Advance(float64) {}

// AttachHeard attaches a recorder to every receiver of n.
func AttachHeard(n *simnet.Network) *Heard {
	h := &Heard{m: make([]map[uint32]float64, n.NumReceivers())}
	for i := range h.m {
		h.m[i] = map[uint32]float64{}
		n.Attach(i, heardSink{h, i})
	}
	return h
}

// LastHeard returns when receiver rx last heard icao (ok false = never).
func (h *Heard) LastHeard(rx int, icao uint32) (float64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.m[rx][icao&0xFFFFFF]
	return t, ok
}

// ------------------------------------------------------------- AVF

// AVFOptions configure an AVF feeder.
type AVFOptions struct {
	Addr  string // aggregator AVF/1 address host:port
	Pin   string // server SPKI pin (sha256/...; "" = system roots)
	Token string // enrol token (invite admission)
	// NoTimestamps sends frames without timing (uplink "timestamps":
	// false): the station feeds the map but never MLAT.
	NoTimestamps bool
	// NoResults asks for no MLAT results (uplink "returnMlat": false).
	NoResults bool
	// Unlocated sends no receiver location (a feeder whose owner never
	// declared one): the aggregator must locate it by the survey.
	Unlocated bool
}

// AVFFeeder is one native feeder (one identity, one receiver).
type AVFFeeder struct {
	Rx      int
	ID      string
	M       *uplink.Manager
	Results *Results
	n       *simnet.Network
}

// NewAVFFeeder creates the feeder of receiver rx and attaches it to n.
func NewAVFFeeder(n *simnet.Network, rx int, o AVFOptions) (*AVFFeeder, error) {
	host, ps, err := net.SplitHostPort(o.Addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return nil, err
	}
	sp := n.Receiver(rx)
	f := &AVFFeeder{Rx: rx, ID: fmt.Sprintf("rx%d", rx), Results: newResults(), n: n}
	loc := &avf.Location{Lat: sp.Lat, Lon: sp.Lon, Alt: sp.HAE, AltRef: model.AltRefHAE, Source: model.LocSrcManual}
	if o.Unlocated {
		loc = nil
	}
	m, err := uplink.NewManager(uplink.Options{Software: "airview-simfeed", Version: model.Version,
		Logf: func(string, ...any) {},
		Source: uplink.SourceFuncs{ReceiverFn: func(string) uplink.ReceiverInfo {
			return uplink.ReceiverInfo{RxNo: 1, Name: f.ID, Decoder: "simnet " + string(sp.Device), Connected: true,
				InputType: "beast", Location: loc}
		}},
		OnResults: func(_ string, fixes []model.MLATFix) {
			for i := range fixes {
				f.Results.add(fixes[i].ICAO, fixes[i].Grade)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	u := model.DefaultUplink()
	u.ID, u.Host, u.Port, u.ServerPin, u.EnrolToken, u.SpoolMB = "agg", host, port, o.Pin, o.Token, 0
	u.Timestamps = !o.NoTimestamps
	u.ReturnMLAT = !o.NoResults
	m.Apply([]model.Uplink{u})
	f.M = m
	n.Attach(rx, f)
	return f, nil
}

// Observe implements simnet.RxSink.
func (f *AVFFeeder) Observe(o *simnet.Observation) {
	fr := f.n.RawFrame(o)
	fr.FeedID, fr.Station = f.ID, 0
	f.M.HandleFrame(fr)
}

// Advance implements simnet.RxSink.
func (f *AVFFeeder) Advance(float64) {}

// Connected reports whether the uplink is connected.
func (f *AVFFeeder) Connected() bool {
	for _, s := range f.M.Status() {
		if s.State == "connected" {
			return true
		}
	}
	return false
}

// Close stops the uplink.
func (f *AVFFeeder) Close() { f.M.Close() }

// ------------------------------------------------------------- Beast

// BeastFeeder keeps one readsb-style beast_reduce_plus_out link to addr.
type BeastFeeder struct {
	Rx   int
	addr string
	ch   chan []byte
	up   atomic.Bool
	sent atomic.Uint64
}

// NewBeastFeeder creates the feeder of receiver rx and attaches its emitter
// to n; Run keeps the connection.
func NewBeastFeeder(n *simnet.Network, rx int, addr string) *BeastFeeder {
	b := &BeastFeeder{Rx: rx, addr: addr, ch: make(chan []byte, 4096)}
	n.NewBeastEmitter(rx, b, simnet.BeastOptions{Format: simnet.BeastReducePlus})
	return b
}

// Write implements simnet.ByteSink (never blocks: a full queue drops).
func (b *BeastFeeder) Write(_ int64, p []byte) {
	select {
	case b.ch <- append([]byte(nil), p...):
	default:
	}
}

// Up reports whether the link is connected.
func (b *BeastFeeder) Up() bool { return b.up.Load() }

// Run connects and reconnects (every 2 s) until ctx ends. The stream head
// (UUID escape) is sent again on every reconnect.
func (b *BeastFeeder) Run(ctx context.Context) {
	var head []byte
	for ctx.Err() == nil {
		c, err := net.DialTimeout("tcp", b.addr, 5*time.Second)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		b.up.Store(true)
		if head != nil {
			_, _ = c.Write(head)
		}
		for err == nil {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case p := <-b.ch:
				if head == nil {
					head = p
				}
				_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_, err = c.Write(p)
				b.sent.Add(uint64(len(p)))
			}
		}
		b.up.Store(false)
		c.Close()
	}
}

// ------------------------------------------------------------- mlat-client

// mlat-client frame-start offsets (see the package comment).
const (
	off12ModeS     = 768
	offGPSShortNs  = 64_000
	offGPSLongNs   = 120_000
	mask48         = 1<<48 - 1
	nsPerDay       = int64(86_400e9)
	syncPairWindow = 5.0 // s between the even and odd half of a sync pair
)

// MLATClientOptions configure an MLATClient.
type MLATClientOptions struct {
	Addr string // aggregator mlat-client address host:port
	// Privacy is the handshake privacy flag (default false).
	Privacy bool
	// NoResults sends return_results:false.
	NoResults bool
	// NoSplitSync ignores the server's split_sync offer and always sends
	// sync pairs (the behaviour of clients without split sync).
	NoSplitSync bool
}

// MLATClient is one simulated mlat-client (one receiver).
type MLATClient struct {
	Rx      int
	Results *Results
	n       *simnet.Network
	o       MLATClientOptions
	dev     simnet.Device

	mu        sync.Mutex
	epoch     uint32
	started   bool
	requested map[uint32]bool
	seen      map[uint32]float64 // ICAO -> last arrival (s)
	pend      map[uint32]*[2]pendHalf
	lastSeenS float64
	connS     float64 // simulated time of the current session's first frame (-1 = none yet)
	split     bool    // the server offered split_sync: single "ssync" messages
	out       chan []byte

	up       atomic.Bool
	denied   atomic.Value // string
	starts   atomic.Uint64
	sentMsgs atomic.Uint64
}

type pendHalf struct {
	t    uint64
	data []byte
	at   float64
	ok   bool
}

// NewMLATClient creates the client of receiver rx and attaches it to n;
// Run connects it.
func NewMLATClient(n *simnet.Network, rx int, o MLATClientOptions) (*MLATClient, error) {
	dev := n.Receiver(rx).Device
	if dev == simnet.DeviceSBS {
		return nil, errors.New("simfeed: SBS receivers are not supported as mlat-clients")
	}
	c := &MLATClient{Rx: rx, Results: newResults(), n: n, o: o, dev: dev,
		requested: map[uint32]bool{}, seen: map[uint32]float64{}, pend: map[uint32]*[2]pendHalf{},
		out: make(chan []byte, 8192)}
	n.Attach(rx, c)
	return c, nil
}

// Up reports whether the client is connected and past the handshake.
func (c *MLATClient) Up() bool { return c.up.Load() }

// Denied returns the server's deny reason ("" = none).
func (c *MLATClient) Denied() string {
	s, _ := c.denied.Load().(string)
	return s
}

// Requested returns how many aircraft the server currently requests.
func (c *MLATClient) Requested() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requested)
}

func (c *MLATClient) handshake() []byte {
	r := c.n.Receiver(c.Rx)
	h := map[string]any{
		"version": 3, "client_version": "simfeed-0.1", "compress": []string{"none"},
		"selective_traffic": true, "heartbeat": true, "return_results": !c.o.NoResults, "return_stats": true,
		"udp_transport": false, "return_result_format": "ecef", "uuid": r.UUID, "user": r.Name,
		"lat": r.DeclaredLat, "lon": r.DeclaredLon, "alt": r.DeclaredAlt, "clock_type": c.dev.MLATClientType(),
		"privacy": c.o.Privacy,
	}
	b, _ := json.Marshal(h)
	return append(b, '\n')
}

// value converts an observation's raw stamp to the client convention.
func (c *MLATClient) value(o *simnet.Observation) (uint64, bool) {
	ts := o.BeastTS
	if o.Class.GPS() {
		v := int64(ts>>30)*1e9 + int64(ts&(1<<30-1))
		if len(o.Data) == 7 {
			v -= offGPSShortNs
		} else {
			v -= offGPSLongNs
		}
		if v < 0 {
			v += nsPerDay
		}
		return uint64(v), v > 0
	}
	v := (ts - off12ModeS) & mask48
	return v, v != 0
}

func (c *MLATClient) emit(line string) {
	select {
	case c.out <- []byte(line):
		c.sentMsgs.Add(1)
	default:
	}
}

// Observe implements simnet.RxSink.
func (c *MLATClient) Observe(o *simnet.Observation) {
	if len(o.Data) < 7 {
		return // Mode A/C: not sent
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	icao := o.ICAO & 0xFFFFFF
	c.seen[icao] = o.ArrivalS
	if !c.started {
		c.started, c.epoch = true, o.Epoch
	}
	if o.Epoch != c.epoch {
		c.epoch = o.Epoch
		reason := "restart"
		for _, ev := range c.n.ClockEvents(c.Rx) {
			if ev.NewEpoch == o.Epoch {
				reason = string(ev.Reason)
			}
		}
		c.emit(`{"clock_reset":{"reason":` + strconv.Quote(reason) + `}}` + "\n")
		clear(c.pend)
	}
	if c.connS < 0 {
		c.connS = o.ArrivalS
	}
	// The seen list goes out every 2 s during the first 10 s of a session
	// (the first report would otherwise list only the aircraft of the
	// first frame and delay every request by 15 s), then every 15 s.
	every := 15.0
	if o.ArrivalS-c.connS < 10 {
		every = 2
	}
	if o.ArrivalS-c.lastSeenS >= every {
		c.lastSeenS = o.ArrivalS
		c.reportSeen(o.ArrivalS)
	}
	if !c.requested[icao] || !c.up.Load() {
		return
	}
	v, ok := c.value(o)
	if !ok {
		return
	}
	d := o.Data
	if d[0]>>3 == 17 && len(d) == 14 {
		if tc := d[4] >> 3; (tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22) {
			if c.split { // split_sync: one message per position, as it arrives
				c.emit(fmt.Sprintf(`{"ssync":{"t":%d,"m":"%s"}}`+"\n", v, hex.EncodeToString(d)))
				return
			}
			odd := int(d[6]>>2) & 1
			p := c.pend[icao]
			if p == nil {
				p = &[2]pendHalf{}
				c.pend[icao] = p
			}
			p[odd] = pendHalf{t: v, data: append([]byte(nil), d...), at: o.ArrivalS, ok: true}
			if q := p[1-odd]; q.ok && o.ArrivalS-q.at <= syncPairWindow {
				c.emit(fmt.Sprintf(`{"sync":{"et":%d,"em":"%s","ot":%d,"om":"%s"}}`+"\n",
					p[0].t, hex.EncodeToString(p[0].data), p[1].t, hex.EncodeToString(p[1].data)))
				*p = [2]pendHalf{}
			}
			return
		}
	}
	c.emit(fmt.Sprintf(`{"mlat":{"t":%d,"m":"%s"}}`+"\n", v, hex.EncodeToString(d)))
}

// reportSeen sends the seen list and the lost aircraft (no frame for 60
// s). Caller holds mu.
func (c *MLATClient) reportSeen(now float64) {
	var seen, lost []string
	for a, t := range c.seen {
		if now-t > 60 {
			lost = append(lost, fmt.Sprintf("%06x", a))
			delete(c.seen, a)
			continue
		}
		seen = append(seen, fmt.Sprintf("%06x", a))
	}
	if len(seen) > 0 {
		b, _ := json.Marshal(map[string]any{"seen": seen})
		c.emit(string(b) + "\n")
	}
	if len(lost) > 0 {
		b, _ := json.Marshal(map[string]any{"lost": lost})
		c.emit(string(b) + "\n")
	}
}

// Advance implements simnet.RxSink.
func (c *MLATClient) Advance(float64) {}

// Run connects (and reconnects every 2 s) until ctx ends.
func (c *MLATClient) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.session(ctx); err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (c *MLATClient) session(ctx context.Context) error {
	conn, err := net.DialTimeout("tcp", c.o.Addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := conn.Write(c.handshake()); err != nil {
		return err
	}
	rd := bufio.NewReaderSize(conn, 64<<10)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := rd.ReadBytes('\n')
	if err != nil {
		return err
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(line, &reply); err != nil {
		return err
	}
	if d, ok := reply["deny"]; ok {
		c.denied.Store(string(d))
		return errors.New("denied: " + string(d))
	}
	_ = conn.SetReadDeadline(time.Time{})
	var split bool
	if v, ok := reply["split_sync"]; ok {
		_ = json.Unmarshal(v, &split)
	}
	c.mu.Lock()
	c.requested = map[uint32]bool{}
	c.lastSeenS = -1e9 // report the seen list at once
	c.connS = -1
	c.split = split && !c.o.NoSplitSync
	clear(c.pend)
	c.mu.Unlock()
	c.emit(`{"input_connected":"connected"}` + "\n")
	c.up.Store(true)
	defer c.up.Store(false)

	errc := make(chan error, 2)
	go func() { errc <- c.reader(rd) }()
	go func() {
		w := bufio.NewWriterSize(conn, 64<<10)
		hb := time.NewTicker(30 * time.Second)
		defer hb.Stop()
		for {
			select {
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			case <-hb.C:
				_, _ = w.WriteString(`{"heartbeat":{"client_time":` + strconv.FormatInt(time.Now().Unix(), 10) + `}}` + "\n")
			case p := <-c.out:
				_, _ = w.Write(p)
			drain:
				for {
					select {
					case p := <-c.out:
						_, _ = w.Write(p)
					default:
						break drain
					}
				}
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := w.Flush(); err != nil {
				errc <- err
				return
			}
		}
	}()
	err = <-errc
	conn.Close()
	return err
}

// reader handles the server's messages: start/stop_sending and results.
func (c *MLATClient) reader(rd *bufio.Reader) error {
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return err
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		for k, v := range m {
			switch k {
			case "start_sending", "stop_sending":
				var addrs []string
				if json.Unmarshal(v, &addrs) != nil {
					continue
				}
				c.mu.Lock()
				for _, a := range addrs {
					x, err := strconv.ParseUint(strings.TrimPrefix(a, "~"), 16, 32)
					if err != nil {
						continue
					}
					if k == "start_sending" {
						c.requested[uint32(x)] = true
						c.starts.Add(1)
					} else {
						delete(c.requested, uint32(x))
					}
				}
				c.mu.Unlock()
			case "result":
				var r struct {
					Addr string `json:"addr"`
				}
				if json.Unmarshal(v, &r) != nil {
					continue
				}
				if x, err := strconv.ParseUint(strings.TrimPrefix(r.Addr, "~"), 16, 32); err == nil {
					c.Results.add(uint32(x), model.GradeNone)
				}
			}
		}
	}
}

// ------------------------------------------------------------- driver

// RealTime advances n in step increments so that simulated time follows
// the wall clock from the call on, until ctx ends or n fails. between runs
// on the driving goroutine after every step (attach emitters there, never
// concurrently with Advance).
func RealTime(ctx context.Context, n *simnet.Network, step time.Duration, between func()) error {
	start := time.Now()
	sim := time.Duration(0)
	tick := time.NewTicker(step / 5)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		for time.Since(start) >= sim {
			if err := n.Advance(ctx, step); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			sim += step
			if between != nil {
				between()
			}
		}
	}
}
