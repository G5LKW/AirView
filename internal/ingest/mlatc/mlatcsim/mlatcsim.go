// Package mlatcsim runs one simnet world through the real ingest grouper
// and the real MLAT engine in virtual time, with every receiver connected
// either like a native AirView feeder (timed frames straight into the
// ingest, as AVF/1 delivers them) or as an mlat-client on the real
// internal/ingest/mlatc server (through Server.Offline: the handshake, the
// JSON lines, tick normalisation, de-duplication, selection and results go
// through the production code). It measures the published hold-out error
// against the simulator's truth, so transports and server policies can be
// compared on the same world (H-mlatc: split_sync, the timing quarantine).
//
// The emulated mlat-client follows the behaviour described in
// docs/aggregator/MLATC.md (AirView's own description; no mlat-client code
// was used): nothing is sent for an aircraft before start_sending; DF17
// airborne positions go out as sync (pairs, or single "ssync" messages when
// the server offered split_sync and the emulation honours it); the other
// Mode S downlink formats go out as "mlat" only for aircraft without a
// recent ADS-B position; seen / lost / rate_report are sent periodically;
// timestamps follow the reference convention (12 MHz: −768 ticks; GPS:
// nanoseconds of the UTC day minus 64 / 120 µs).
//
// It is test tooling, never part of the product.
package mlatcsim

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"airview/internal/ingest"
	"airview/internal/ingest/mlatc"
	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

// Transport is how one receiver reaches the aggregator.
type Transport uint8

const (
	// AVF: timed frames go straight into the ingest (AVF/1 semantics).
	AVF Transport = iota
	// MLATC: an emulated mlat-client on the mlatc server.
	MLATC
)

// Pairing is how an emulated mlat-client pairs DF17 positions into "sync"
// messages when it does not use split sync.
type Pairing uint8

const (
	// PairsDisjoint: a pair consumes both halves; the next pair needs a new
	// even and a new odd message (internal/engine/simfeed's emulation).
	PairsDisjoint Pairing = iota
	// PairsOverlap: each new message is paired with the latest message of
	// the other parity (within 5 s), so every message after the first is
	// sent fresh and once more as the old half (the reading of the
	// reference client in MLATC.md; the server drops the repeat).
	PairsOverlap
)

// Hostile corrupts a receiver's timestamps (quarantine tests).
type Hostile struct {
	// NoiseNs adds uniform noise in ±NoiseNs to every tick.
	NoiseNs float64
	// DriftPPM adds a slowly varying wrong clock rate.
	DriftPPM float64
}

// Config describes one run.
type Config struct {
	Seed     uint64
	Devices  []simnet.Device
	Via      []Transport // per receiver (missing = AVF)
	Hostile  map[int]Hostile
	Aircraft int     // default 250
	RadiusKm float64 // receiver disc radius (default 150)
	// ModeSOnly is the fraction of Mode S-only aircraft (default 0.3).
	ModeSOnly float64
	// Holdout fraction (default 0.1).
	Holdout float64
	// NetLatMs is the uniform network latency range (default 20-80 ms).
	NetLatMs [2]float64
	// Duration is the simulated time (default 10 min); EvalFrom starts the
	// evaluation (default 4 min).
	Duration, EvalFrom time.Duration
	// Server split-sync policy; HonourSplit makes the emulated clients use
	// ssync when the server offers it.
	SplitSync   mlatc.SplitSyncMode
	HonourSplit bool
	Pairing     Pairing
	// Wrap, when set, sits between the ingest and the engine (the timing
	// quarantine gate). It must forward ObserveLate if next has it.
	Wrap func(next ingest.MLATSink) ingest.MLATSink
	// Trust is passed to the mlatc server (probe requests).
	Trust mlatc.TrustSource
	// SeenEvery is the emulated client's "seen" report period (default
	// 5 s; internal/engine/simfeed uses 15 s and starts after 15 s).
	SeenEvery time.Duration
	// PosSigmaM is the station position σ given to the engine for AVF
	// receivers (default 5 m); MLATCPosSigmaM the one for mlat-client
	// receivers (default PosSigmaM). The engine's registry wiring gives an
	// mlat-client position (altitude reference unknown) 30 m + |geoid
	// undulation|; this reproduces that.
	PosSigmaM, MLATCPosSigmaM float64
	// OnStart is called once the engine and server exist (registering
	// stations elsewhere, e.g. the gate's station table).
	OnStart func(r *Run)
	// Logf receives progress lines (nil = none).
	Logf func(format string, args ...any)
}

// Dist summarises errors.
type Dist struct {
	N             int
	P50, P95, P99 float64
}

func (d Dist) String() string {
	return fmt.Sprintf("n=%d P50 %.1f m P95 %.1f m P99 %.1f m", d.N, d.P50, d.P95, d.P99)
}

// Result is the outcome of a run.
type Result struct {
	// Holdout: published (tracker) fixes of hold-out aircraft inside the
	// receiver disc against the truth; Published the same for the other
	// MLAT aircraft.
	Holdout, Published Dist
	// HoldoutContrib: share of hold-out fixes with at least one mlatc or
	// hostile station among the contributors is not observable from fixes;
	// instead Stats and Quality carry the engine's own view.
	Stats   mlat.Stats
	Quality mlat.Quality
	MLATC   mlatc.Stats
	// Results counts the MLAT results the emulated clients received.
	Results map[int]int
	Frames  int
	Wall    time.Duration
}

// Run is one running simulation (exposed to Config.OnStart).
type Run struct {
	Cfg     Config
	Net     *simnet.Network
	Ingest  *ingest.Ingest
	Engine  *mlat.Engine
	Server  *mlatc.Server
	centre  geo.Vec3
	now     int64
	sweep   int64
	nextAd  int64
	q       dq
	nseq    int64
	rng     *rand.Rand
	lastAt  map[int]int64
	epoch   map[int]uint32
	gps     []int
	nextG   float64
	clients map[int]*client
	hold    []float64
	pub     []float64
	frames  int
	results map[int]int
	err     error
	gross   int
}

// Station returns the station index of receiver rx.
func Station(rx int) model.StationIdx { return model.StationIdx(rx + 2) }

// New builds the run.
func New(cfg Config) (*Run, error) {
	if cfg.Aircraft == 0 {
		cfg.Aircraft = 250
	}
	if cfg.RadiusKm == 0 {
		cfg.RadiusKm = 150
	}
	if cfg.ModeSOnly == 0 {
		cfg.ModeSOnly = 0.3
	}
	if cfg.Holdout == 0 {
		cfg.Holdout = 0.1
	}
	if cfg.NetLatMs == [2]float64{} {
		cfg.NetLatMs = [2]float64{20, 80}
	}
	if cfg.Duration == 0 {
		cfg.Duration = 10 * time.Minute
	}
	if cfg.EvalFrom == 0 {
		cfg.EvalFrom = 4 * time.Minute
	}
	specs := simnet.RandomReceivers(cfg.Seed, len(cfg.Devices), simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: cfg.RadiusKm})
	rr := rand.New(rand.NewPCG(cfg.Seed, 77))
	for i, d := range cfg.Devices {
		specs[i].Clock = simnet.DefaultClockSpec(d, rr)
		specs[i].Reception = simnet.DefaultReception(d, rr)
		specs[i].Name = "rx" + strconv.Itoa(i)
	}
	n, err := simnet.New(simnet.Config{Seed: cfg.Seed, Aircraft: cfg.Aircraft, Receivers: specs,
		ModeSOnlyFraction: cfg.ModeSOnly, CenterLat: 51, CenterLon: 9})
	if err != nil {
		return nil, err
	}
	r := &Run{Cfg: cfg, Net: n, centre: geo.ToECEF(51, 9, 0), rng: rand.New(rand.NewPCG(cfg.Seed, 5)),
		lastAt: map[int]int64{}, epoch: map[int]uint32{}, clients: map[int]*client{}, results: map[int]int{}}
	r.now = n.UnixNs(0)
	mc := model.DefaultAggregator().MLAT
	mc.Holdout = cfg.Holdout
	a := n.Config().Atmosphere
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srvRes := &resultFan{}
	e, err := mlat.New(mlat.Options{Config: mc, OriginUnixNs: n.UnixNs(0), Now: r.clock, Workers: -1,
		Atmosphere: geo.Atmosphere{N0: a.N0, H: a.ScaleHeightM, K: a.KAtm}, HoldoutSalt: cfg.Seed,
		Logger: quiet, Results: []mlat.ResultSender{srvRes}})
	if err != nil {
		return nil, err
	}
	r.Engine = e
	var sink ingest.MLATSink = e
	if cfg.Wrap != nil {
		sink = cfg.Wrap(e)
	}
	ing, err := ingest.New(ingest.Options{Handler: fakeTracker{}, MLAT: sink, Clock: e.ClockView(), ClockObserver: e,
		Now: r.clock, Logger: quiet})
	if err != nil {
		return nil, err
	}
	r.Ingest = ing
	pol := mlatc.DefaultPolicy()
	pol.AllowCIDRs = []string{"0.0.0.0/0", "::/0"}
	pol.MaxMsgsPerSec, pol.MaxKBps = 1_000_000, 1_000_000
	srv, err := mlatc.New(mlatc.Options{Now: r.clock, SplitSync: cfg.SplitSync, Policy: pol, Logger: quiet,
		Trust: cfg.Trust, Hooks: mlatc.Hooks{OnSeen: e.NoteSeen}}, simRegistry{}, mlatc.FromIngest(ing))
	if err != nil {
		return nil, err
	}
	r.Server = srv
	srvRes.s = srv
	if cfg.SeenEvery == 0 {
		cfg.SeenEvery = 5 * time.Second
	}
	if cfg.PosSigmaM == 0 {
		cfg.PosSigmaM = 5
	}
	if cfg.MLATCPosSigmaM == 0 {
		cfg.MLATCPosSigmaM = cfg.PosSigmaM
	}
	r.Cfg = cfg
	for i, rx := range n.Receivers() {
		info := mlat.StationInfo{Class: rx.Class,
			Declared: mlat.Position{Valid: true, ECEF: geo.Vec3{X: rx.Pos[0], Y: rx.Pos[1], Z: rx.Pos[2]}, SigmaM: cfg.PosSigmaM}}
		if r.via(i) == MLATC {
			info.Declared.SigmaM = cfg.MLATCPosSigmaM
			info.NoStatus = rx.Class.GPS()
			if rx.Device == simnet.DeviceSBS {
				return nil, fmt.Errorf("mlatcsim: rx %d: SBS receivers cannot be mlat-clients here", i)
			}
		} else if rx.Device == simnet.DeviceRadarcapeGPS {
			r.gps = append(r.gps, i)
		}
		e.SetStation(Station(i), info)
	}
	n.AddSink(r)
	return r, nil
}

func (r *Run) via(rx int) Transport {
	if rx < len(r.Cfg.Via) {
		return r.Cfg.Via[rx]
	}
	return AVF
}

func (r *Run) clock() time.Time { return time.Unix(0, r.now) }

func (r *Run) logf(f string, a ...any) {
	if r.Cfg.Logf != nil {
		r.Cfg.Logf(f, a...)
	}
}

// Execute runs the simulation and returns the result.
func (r *Run) Execute(ctx context.Context) (Result, error) {
	t0 := time.Now()
	defer r.Server.Close()
	for i := range r.Cfg.Devices {
		if r.via(i) != MLATC {
			continue
		}
		c := &client{r: r, rx: i, requested: map[uint32]bool{}, seen: map[uint32]float64{}, adsb: map[uint32]float64{},
			pos: map[uint32]int{}, pend: map[uint32]*[2]half{}, newSeen: map[uint32]bool{}}
		if err := c.connect(); err != nil {
			return Result{}, err
		}
		r.clients[i] = c
	}
	if r.Cfg.OnStart != nil {
		r.Cfg.OnStart(r)
	}
	step := time.Minute
	for done := time.Duration(0); done < r.Cfg.Duration; done += step {
		if err := r.Net.Advance(ctx, min(step, r.Cfg.Duration-done)); err != nil {
			return Result{}, err
		}
		if r.err != nil {
			return Result{}, r.err
		}
		r.logf("t=%v frames %d hold-out samples %d", done+step, r.frames, len(r.hold))
	}
	r.collect()
	res := Result{Holdout: summarize(r.hold), Published: summarize(r.pub), Stats: r.Engine.Stats(),
		Quality: r.Engine.QualityReport(int(math.Ceil((r.Cfg.Duration - r.Cfg.EvalFrom).Minutes())), false),
		MLATC:   r.Server.Stats(), Results: r.results, Frames: r.frames, Wall: time.Since(t0)}
	for _, c := range r.clients {
		c.off.Close()
	}
	return res, nil
}

func summarize(e []float64) Dist {
	if len(e) == 0 {
		return Dist{}
	}
	s := slices.Clone(e)
	slices.Sort(s)
	q := func(p float64) float64 { return s[min(len(s)-1, int(p*float64(len(s))))] }
	return Dist{N: len(s), P50: q(0.5), P95: q(0.95), P99: q(0.99)}
}

// ------------------------------------------------------------ delivery

type item struct {
	at, seq int64
	f       model.RawFrame
	c       *client
	line    []byte
}
type dq []item

func (h dq) Len() int { return len(h) }
func (h dq) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h dq) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *dq) Push(x any)   { *h = append(*h, x.(item)) }
func (h *dq) Pop() any {
	o := *h
	x := o[len(o)-1]
	*h = o[:len(o)-1]
	return x
}

func (r *Run) netLat(rx int, host int64) int64 {
	l := r.Cfg.NetLatMs[0] + r.rng.Float64()*(r.Cfg.NetLatMs[1]-r.Cfg.NetLatMs[0])
	at := max(host+int64(l*1e6), r.lastAt[rx]) // TCP: in order
	r.lastAt[rx] = at
	return at
}

func (r *Run) push(it item) {
	r.nseq++
	it.seq = r.nseq
	heap.Push(&r.q, it)
}

// Observe implements simnet.Sink.
func (r *Run) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		if o.Kind != model.FrameModeSShort && o.Kind != model.FrameModeSLong {
			continue
		}
		r.frames++
		if c := r.clients[o.Rx]; c != nil {
			c.observe(o)
			continue
		}
		f := r.Net.RawFrame(o)
		f.Data = append([]byte(nil), f.Data...)
		if h, ok := r.Cfg.Hostile[o.Rx]; ok {
			f.Ticks = corrupt(f.Ticks, f.ClockCls, h, o.ArrivalS, r.rng)
		}
		if ep, ok := r.epoch[o.Rx]; ok && ep != o.Epoch {
			reason := model.ClockEvRestart
			for _, ev := range r.Net.ClockEvents(o.Rx) {
				if ev.NewEpoch == o.Epoch {
					reason = ev.Reason
				}
			}
			r.Ingest.ClockEvent(f.Station, ingest.ClockEvent{OldEpoch: ep, NewEpoch: o.Epoch, Reason: reason, HostNs: o.HostNs})
		}
		r.epoch[o.Rx] = o.Epoch
		r.push(item{at: r.netLat(o.Rx, o.HostNs), f: f})
	}
}

// corrupt applies a hostile timing error to ticks.
func corrupt(t uint64, cls model.ClockClass, h Hostile, atS float64, rng *rand.Rand) uint64 {
	hz := cls.NominalHz()
	errNs := (rng.Float64()*2 - 1) * h.NoiseNs
	errNs += h.DriftPPM * 1e3 * atS * math.Sin(atS/97) // ns: ppm × s × 1e3
	d := int64(math.Round(errNs * hz / 1e9))
	if d < 0 && uint64(-d) > t {
		return t
	}
	return uint64(int64(t) + d)
}

// Advance implements simnet.Sink.
func (r *Run) Advance(hz float64) {
	until := r.Net.UnixNs(hz)
	for len(r.q) > 0 && r.q[0].at <= until {
		it := heap.Pop(&r.q).(item)
		r.step(it.at)
		if it.c != nil {
			it.c.deliver(it.line, it.at)
		} else {
			r.Ingest.Submit([]model.RawFrame{it.f}, false)
		}
	}
	r.step(until)
	for ; r.nextG <= hz; r.nextG++ {
		for _, i := range r.gps {
			ct, _ := r.Net.ClockAt(i, r.nextG)
			r.Engine.Filter().GPS(Station(i), clock.GPSStatus{HostNs: r.Net.UnixNs(r.nextG), Locked: ct.Locked, UTC: !ct.GPSTime})
		}
	}
	for _, c := range r.clients {
		c.periodic(hz)
	}
}

func (r *Run) step(t int64) {
	if r.sweep == 0 {
		r.sweep = t
	}
	for r.sweep+10e6 <= t {
		r.sweep += 10e6
		r.now = r.sweep
		r.Ingest.Sweep(time.Unix(0, r.sweep))
		if r.sweep >= r.nextAd {
			r.Engine.Advance(r.sweep)
			r.nextAd = r.sweep + 100e6
		}
		r.collect()
	}
	r.now = max(r.now, t)
}

func (r *Run) collect() {
	for {
		select {
		case f := <-r.Engine.Fixes():
			r.eval(&f)
		default:
			return
		}
	}
}

func (r *Run) eval(f *model.MLATFix) {
	tS := float64(f.TUnixNs-r.Net.UnixNs(0)) / 1e9
	if tS < r.Cfg.EvalFrom.Seconds() || !model.Displayable(f.Grade) {
		return
	}
	tr, ok := r.truth(f.ICAO, f.TUnixNs)
	if !ok {
		return
	}
	p := geo.Vec3{X: f.ECEF[0], Y: f.ECEF[1], Z: f.ECEF[2]}
	tp := geo.Vec3{X: tr.Pos[0], Y: tr.Pos[1], Z: tr.Pos[2]}
	cf := geo.NewFrameECEF(r.centre)
	if d := cf.ToENU(tp); math.Hypot(d.X, d.Y) > r.Cfg.RadiusKm*1000 {
		return
	}
	fr := geo.NewFrame(geo.LLH{Lat: tr.Lat, Lon: tr.Lon, H: tr.HAE})
	d := fr.Rotate(p.Sub(tp))
	e := math.Hypot(d.X, d.Y)
	if e > 5000 && r.Cfg.Logf != nil && r.gross < 20 {
		r.gross++
		r.logf("gross fix: %06x t=%.1f err %.0f m grade %d bucket %d flags %b modeSOnly %v", f.ICAO, tS, e, f.Grade, f.NBucket, f.Flags, tr.Sim.MLAT)
	}
	if f.Flags&model.FixFlagHoldout != 0 {
		r.hold = append(r.hold, e)
	} else {
		r.pub = append(r.pub, e)
	}
}

func (r *Run) truth(icao uint32, tNs int64) (at simnet.AircraftTruth, ok bool) {
	defer func() {
		if recover() != nil {
			at, ok = simnet.AircraftTruth{}, false
		}
	}()
	return r.Net.AircraftAt(icao, float64(tNs-r.Net.UnixNs(0))/1e9)
}

// ------------------------------------------------------------ plumbing

// fakeTracker stands in for the global tracker (key, DF, position flag).
type fakeTracker struct{}

func (fakeTracker) Handle(f model.RawFrame) tracker.Result {
	k, _ := tracker.RouteKey(f.Data)
	var df uint8
	if len(f.Data) > 0 {
		df = f.Data[0] >> 3
	}
	res := tracker.Result{Status: tracker.ResultOK, Key: k, DF: df}
	if isPosition(f.Data) {
		res.Pos = true
	}
	return res
}
func (fakeTracker) HandleUpdate(model.DecodedUpdate) {}
func (fakeTracker) Lanes() *tracker.Lanes            { return nil }

// isPosition reports a DF17/18 airborne position.
func isPosition(d []byte) bool {
	if len(d) != 14 {
		return false
	}
	df := d[0] >> 3
	if df != 17 && !(df == 18 && d[0]&7 < 2) {
		return false
	}
	tc := d[4] >> 3
	return (tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22)
}

// simRegistry admits every client as the station of its receiver ("user"
// is "rx<i>").
type simRegistry struct{}

func (simRegistry) Attach(_ context.Context, id mlatc.Identity) (mlatc.Attachment, error) {
	i, err := strconv.Atoi(strings.TrimPrefix(id.User, "rx"))
	if err != nil {
		return mlatc.Attachment{}, err
	}
	st := Station(i)
	return mlatc.Attachment{Station: st, FeedID: "rx" + strconv.Itoa(i), Admission: model.AdmApproved}, nil
}
func (simRegistry) Detach(mlatc.Identity, mlatc.Attachment) {}

// resultFan routes engine results to the mlatc server (AVF stations: dropped).
type resultFan struct{ s *mlatc.Server }

func (f *resultFan) SendResults(st model.StationIdx, fixes []model.MLATFix) bool {
	if f.s == nil {
		return false
	}
	return f.s.SendResults(st, fixes)
}

// ------------------------------------------------------------ client

type half struct {
	t    uint64
	data []byte
	at   float64 // ArrivalS
	host int64
	ok   bool
}

// client emulates one mlat-client.
type client struct {
	r         *Run
	rx        int
	off       *mlatc.Offline
	split     bool
	epoch     uint32
	started   bool
	requested map[uint32]bool
	seen      map[uint32]float64 // last frame (s)
	newSeen   map[uint32]bool
	adsb      map[uint32]float64 // last DF17 position (s)
	pos       map[uint32]int     // DF17 positions since the last rate_report
	pend      map[uint32]*[2]half
	nextSeen  float64
	nextRate  float64
	lastHost  int64
}

func (c *client) connect() error {
	rx := c.r.Net.Receiver(c.rx)
	h := map[string]any{"version": 3, "client_version": "mlatcsim", "compress": []string{"none"},
		"selective_traffic": true, "heartbeat": true, "return_results": true, "return_stats": true,
		"udp_transport": false, "return_result_format": "ecef", "uuid": rx.UUID, "user": "rx" + strconv.Itoa(c.rx),
		"lat": rx.DeclaredLat, "lon": rx.DeclaredLon, "alt": rx.DeclaredAlt, "clock_type": rx.Device.MLATClientType()}
	b, _ := json.Marshal(h)
	ip := netip.AddrFrom4([4]byte{10, 0, byte(c.rx >> 8), byte(c.rx)})
	off, reply, err := c.r.Server.Offline(b, ip, c.r.clock())
	if err != nil {
		return fmt.Errorf("mlatcsim: rx %d handshake: %v (%s)", c.rx, err, reply)
	}
	var rep struct {
		Split bool `json:"split_sync"`
	}
	_ = json.Unmarshal(reply, &rep)
	c.off, c.split = off, rep.Split && c.r.Cfg.HonourSplit
	return nil
}

// value converts a receiver stamp to the reference client's convention.
func (c *client) value(o *simnet.Observation) (uint64, bool) {
	ticks := o.Ticks
	if h, ok := c.r.Cfg.Hostile[o.Rx]; ok {
		ticks = corrupt(ticks, o.Class, h, o.ArrivalS, c.r.rng)
	}
	if o.Class.GPS() {
		v := int64(ticks % uint64(model.NsPerDay))
		if len(o.Data) == 7 {
			v -= 64_000
		} else {
			v -= 120_000
		}
		if v < 0 {
			v += model.NsPerDay
		}
		return uint64(v), v > 0
	}
	v := (ticks - 768) & (1<<48 - 1)
	return v, v != 0
}

func (c *client) send(line string, host int64) {
	c.r.push(item{at: c.r.netLat(c.rx, host), c: c, line: []byte(line)})
}

func (c *client) observe(o *simnet.Observation) {
	icao := o.ICAO & 0xFFFFFF
	c.lastHost = o.HostNs
	if _, ok := c.seen[icao]; !ok {
		c.newSeen[icao] = true
	}
	c.seen[icao] = o.ArrivalS
	if !c.started {
		c.started, c.epoch = true, o.Epoch
	}
	if o.Epoch != c.epoch {
		c.epoch = o.Epoch
		reason := "restart"
		for _, ev := range c.r.Net.ClockEvents(c.rx) {
			if ev.NewEpoch == o.Epoch {
				reason = string(ev.Reason)
			}
		}
		c.send(`{"clock_reset":{"reason":`+strconv.Quote(reason)+`}}`+"\n", o.HostNs)
		clear(c.pend)
	}
	d := o.Data
	pos := isPosition(d) && d[0]>>3 == 17
	if pos {
		c.adsb[icao] = o.ArrivalS
		c.pos[icao]++
	}
	if !c.requested[icao] {
		return
	}
	v, ok := c.value(o)
	if !ok {
		return
	}
	if pos {
		c.syncFrame(icao, v, o)
		return
	}
	switch d[0] >> 3 {
	case 0, 4, 5, 11, 16, 20, 21:
	default:
		return
	}
	if t, ok := c.adsb[icao]; ok && o.ArrivalS-t < 60 {
		return // the client sends mlat only for aircraft without ADS-B positions
	}
	c.send(fmt.Sprintf(`{"mlat":{"t":%d,"m":"%s"}}`+"\n", v, hex.EncodeToString(d)), o.HostNs)
}

func (c *client) syncFrame(icao uint32, v uint64, o *simnet.Observation) {
	d := o.Data
	if c.split {
		c.send(fmt.Sprintf(`{"ssync":{"t":%d,"m":"%s"}}`+"\n", v, hex.EncodeToString(d)), o.HostNs)
		return
	}
	odd := int(d[6]>>2) & 1
	p := c.pend[icao]
	if p == nil {
		p = &[2]half{}
		c.pend[icao] = p
	}
	p[odd] = half{t: v, data: append([]byte(nil), d...), at: o.ArrivalS, host: o.HostNs, ok: true}
	q := p[1-odd]
	if !q.ok || o.ArrivalS-q.at > 5 {
		return
	}
	c.send(fmt.Sprintf(`{"sync":{"et":%d,"em":"%s","ot":%d,"om":"%s"}}`+"\n",
		p[0].t, hex.EncodeToString(p[0].data), p[1].t, hex.EncodeToString(p[1].data)), o.HostNs)
	if c.r.Cfg.Pairing == PairsDisjoint {
		*p = [2]half{}
	}
}

// periodic sends seen / lost / rate_report.
func (c *client) periodic(nowS float64) {
	if !c.started {
		return
	}
	if c.nextSeen == 0 {
		c.nextSeen = nowS + c.r.Cfg.SeenEvery.Seconds() // the first report after one period
	}
	if nowS >= c.nextSeen {
		c.nextSeen = nowS + c.r.Cfg.SeenEvery.Seconds()
		var seen, lost []string
		for a := range c.newSeen {
			seen = append(seen, fmt.Sprintf("%06x", a))
		}
		clear(c.newSeen)
		for a, t := range c.seen {
			if nowS-t > 60 {
				lost = append(lost, fmt.Sprintf("%06x", a))
				delete(c.seen, a)
				delete(c.adsb, a)
			}
		}
		if len(seen) > 0 {
			b, _ := json.Marshal(map[string]any{"seen": seen})
			c.send(string(b)+"\n", c.lastHost)
		}
		if len(lost) > 0 {
			b, _ := json.Marshal(map[string]any{"lost": lost})
			c.send(string(b)+"\n", c.lastHost)
		}
	}
	if nowS >= c.nextRate {
		rates := map[string]float64{}
		if c.nextRate > 0 {
			for a, k := range c.pos {
				rates[fmt.Sprintf("%06X", a)] = float64(k) / 30
			}
		}
		clear(c.pos)
		c.nextRate = nowS + 30
		b, _ := json.Marshal(map[string]any{"rate_report": rates})
		c.send(string(b)+"\n", c.lastHost)
	}
}

// deliver hands a line to the server at time at and reads the answers.
func (c *client) deliver(line []byte, at int64) {
	if err := c.off.Lines(line, time.Unix(0, at)); err != nil {
		if c.r.err == nil {
			c.r.err = fmt.Errorf("mlatcsim: rx %d disconnected: %v", c.rx, err)
		}
		return
	}
	out := c.off.Poll(time.Unix(0, at))
	for len(out) > 0 {
		var l []byte
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			l, out = out[:i], out[i+1:]
		} else {
			l, out = out, nil
		}
		c.handle(l)
	}
}

func (c *client) handle(l []byte) {
	var m map[string]json.RawMessage
	if json.Unmarshal(l, &m) != nil {
		return
	}
	for k, v := range m {
		switch k {
		case "start_sending", "stop_sending":
			var list []string
			_ = json.Unmarshal(v, &list)
			for _, s := range list {
				a, err := strconv.ParseUint(s, 16, 32)
				if err != nil {
					continue
				}
				if k == "start_sending" {
					c.requested[uint32(a)] = true
				} else {
					delete(c.requested, uint32(a))
				}
			}
		case "result":
			c.r.results[c.rx]++
		}
	}
}
