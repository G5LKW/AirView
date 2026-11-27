package mlat

import (
	"container/heap"
	"context"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/group"
	"airview/internal/mlat/solve"
	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

// The end-to-end harness: simnet → RawFrames delivered with network latency
// → the real ingest grouper (virtual clock) → the Engine (synchronous mode,
// real clock filter, reference store, planner, assembler, solver,
// tracker) → published fixes compared with simnet's truth. Nothing in the
// engine sees the truth: stations get their declared (here: true) positions
// and classes, as the registry would give them.

// fakeTracker is the global tracker stand-in: it reports a key, the DF and
// whether a frame carries an airborne position (like the real tracker).
type fakeTracker struct{}

func (fakeTracker) Handle(f model.RawFrame) tracker.Result {
	k, _ := tracker.RouteKey(f.Data)
	var df uint8
	if len(f.Data) > 0 {
		df = f.Data[0] >> 3
	}
	r := tracker.Result{Status: tracker.ResultOK, Key: k, DF: df}
	if (df == 17 || (df == 18 && f.Data[0]&7 < 2)) && len(f.Data) == 14 {
		if tc := f.Data[4] >> 3; (tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22) {
			r.Pos = true
		}
	}
	return r
}
func (fakeTracker) HandleUpdate(model.DecodedUpdate) {}
func (fakeTracker) Lanes() *tracker.Lanes            { return nil }

type dItem struct {
	at, seq int64
	f       model.RawFrame
}
type dq []dItem

func (h dq) Len() int { return len(h) }
func (h dq) Less(i, j int) bool {
	return h[i].at < h[j].at || (h[i].at == h[j].at && h[i].seq < h[j].seq)
}
func (h dq) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *dq) Push(x any)   { *h = append(*h, x.(dItem)) }
func (h *dq) Pop() any {
	o := *h
	it := o[len(o)-1]
	*h = o[:len(o)-1]
	return it
}

// errSample is one position compared with the truth.
type errSample struct {
	pos    geo.Vec3
	flags  uint8
	icao   uint32
	tS     float64
	errH   float64
	sigmaH float64
	grade  uint8
	inside bool // inside the network radius
}

type acEval struct {
	modeSOnly  bool
	known      bool
	heard4     int // transmissions heard by >= 4 stations during the evaluation
	heard4anch int // ... by >= 4 anchors (GPS stations)
	published  int
	firstPubS  float64
}

type harnessCfg struct {
	seed        uint64
	devs        map[simnet.Device]int
	radiusKm    float64
	aircraft    int
	modeSFrac   float64
	holdout     float64
	evalFromS   float64
	netLatMs    [2]float64 // uniform network latency range
	clockOver   func(i int, c *simnet.ClockSpec)
	workers     int
	posLatMs    float64 // simnet PosLatencyMaxMs (0 = simnet default 300 ms)
	noSuspend   bool
	warmup      time.Duration
	rotate      time.Duration
	noClockView bool
	// noGPSStatus: never feed GPS status (no anchors: relative time, as
	// over AVF/1 links without receiver status); untimed: receivers whose
	// frames arrive without timestamps (map-only compat feeds); absent:
	// receivers whose frames are dropped entirely.
	noGPSStatus bool
	untimed     map[int]bool
	absent      map[int]bool
	// modeAC turns aggregator.mlat.modeAc on (modeac_netsim_test.go).
	modeAC bool
}

type harness struct {
	tb     testing.TB
	cfg    harnessCfg
	n      *simnet.Network
	ing    *ingest.Ingest
	e      *Engine
	q      dq
	nseq   int64
	now    int64
	sweep  int64
	nextAd int64
	rng    *rand.Rand
	lastAt map[int]int64
	epoch  map[int]uint32
	gps    []int
	nextG  float64
	centre geo.Vec3

	seqHeard map[uint64]*seqRec
	ac       map[uint32]*acEval

	pub, hold, snap []errSample
	holdSnap        []errSample
	buffer          bool // tap: keep the groups instead of passing them on
	buffered        []*ingest.Group
	dbgHold         int
	frames          int

	// hooks of the results suite (results_test.go): obsHook may change a
	// frame before it is delivered (fault injection); pubHook sees every
	// published fix of the evaluation window with its truth (ok = false:
	// no simulated aircraft with that address); engWall is the wall time
	// spent in the ingest + engine (Advance).
	obsHook func(o *simnet.Observation, f *model.RawFrame)
	pubHook func(f *model.MLATFix, tr simnet.AircraftTruth, ok bool)
	engWall time.Duration
}

type seqRec struct {
	icao uint32
	rx   []int
}

func newHarness(tb testing.TB, hc harnessCfg) *harness {
	if hc.radiusKm == 0 {
		hc.radiusKm = 150
	}
	var specs []simnet.ReceiverSpec
	total := 0
	order := []simnet.Device{simnet.DeviceRadarcapeGPS, simnet.DeviceBeast, simnet.DeviceRadarcape12, simnet.DeviceAirspy, simnet.DeviceSDR, simnet.DeviceSBS}
	for _, d := range order {
		total += hc.devs[d]
	}
	base := simnet.RandomReceivers(hc.seed, total, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: hc.radiusKm})
	rr := rand.New(rand.NewPCG(hc.seed, 77))
	k := 0
	for _, d := range order {
		for j := 0; j < hc.devs[d]; j++ {
			s := base[k]
			s.Clock = simnet.DefaultClockSpec(d, rr)
			s.Reception = simnet.DefaultReception(d, rr)
			if hc.clockOver != nil {
				hc.clockOver(k, &s.Clock)
			}
			specs = append(specs, s)
			k++
		}
	}
	return newHarnessSpecs(tb, hc, specs)
}

// newHarnessSpecs builds a harness for explicit receiver specs.
func newHarnessSpecs(tb testing.TB, hc harnessCfg, specs []simnet.ReceiverSpec) *harness {
	if hc.radiusKm == 0 {
		hc.radiusKm = 150
	}
	if hc.aircraft == 0 {
		hc.aircraft = 250
	}
	if hc.netLatMs == [2]float64{} {
		hc.netLatMs = [2]float64{20, 80}
	}
	if hc.workers == 0 {
		hc.workers = -1
	}
	n, err := simnet.New(simnet.Config{Seed: hc.seed, Aircraft: hc.aircraft, Receivers: specs,
		ModeSOnlyFraction: hc.modeSFrac, CenterLat: 51, CenterLon: 9, PosLatencyMaxMs: hc.posLatMs})
	if err != nil {
		tb.Fatal(err)
	}
	h := &harness{tb: tb, cfg: hc, n: n, rng: rand.New(rand.NewPCG(hc.seed, 5)), lastAt: map[int]int64{},
		epoch: map[int]uint32{}, seqHeard: map[uint64]*seqRec{}, ac: map[uint32]*acEval{},
		centre: geo.ToECEF(51, 9, 0)}
	h.now = n.UnixNs(0)
	cfg := model.DefaultAggregator().MLAT
	cfg.Holdout = hc.holdout
	cfg.ModeAC = hc.modeAC
	// A/B switches for the H-clock wiring (integrator): MLAT_INGEST_GATE=1
	// gives the ingest the gate view, MLAT_FLUSH_EST=1 the adaptive flush.
	cfg.Engine.IngestGate = os.Getenv("MLAT_INGEST_GATE") == "1"
	a := n.Config().Atmosphere
	e, err := New(Options{Config: cfg, OriginUnixNs: n.UnixNs(0), Now: h.clock, Workers: hc.workers,
		Atmosphere: geo.Atmosphere{N0: a.N0, H: a.ScaleHeightM, K: a.KAtm}, HoldoutSalt: hc.seed,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), noBlacklistSuspend: hc.noSuspend, SyncWarmup: hc.warmup,
		HoldoutRotate: hc.rotate})
	if err != nil {
		tb.Fatal(err)
	}
	h.e = e
	var cv ingest.ClockView = e.ClockView()
	if hc.noClockView {
		cv = nil
	}
	iopt := ingest.Options{Handler: fakeTracker{}, MLAT: (*tap)(h), Clock: cv, ClockObserver: e, Now: h.clock}
	if os.Getenv("MLAT_FLUSH_EST") == "1" {
		iopt.Flush = group.NewFlushEstimator()
	}
	ing, err := ingest.New(iopt)
	if err != nil {
		tb.Fatal(err)
	}
	h.ing = ing
	for i, r := range n.Receivers() {
		e.SetStation(model.StationIdx(i+2), StationInfo{Class: r.Class,
			Declared: Position{Valid: true, ECEF: geo.Vec3{X: r.Pos[0], Y: r.Pos[1], Z: r.Pos[2]}, SigmaM: 5}})
		if r.Device == simnet.DeviceRadarcapeGPS {
			h.gps = append(h.gps, i)
		}
	}
	for _, w := range e.workers {
		w.dbgFix = h.onFix
	}
	n.AddSink(h)
	return h
}

func (h *harness) clock() time.Time { return time.Unix(0, h.now) }

func (h *harness) truth(icao uint32, tNs int64) (at simnet.AircraftTruth, ok bool) {
	// simnet.AircraftAt can index a snapshot's truth table with the state
	// of another snapshot (filed in HOOKS.md); treat that as "no truth"
	defer func() {
		if recover() != nil {
			at, ok = simnet.AircraftTruth{}, false
		}
	}()
	return h.n.AircraftAt(icao, float64(tNs-h.n.UnixNs(0))/1e9)
}

func toGeo(v simnet.Vec3) geo.Vec3 { return geo.Vec3{X: v[0], Y: v[1], Z: v[2]} }

// horizErr returns the horizontal error of pos against the truth and the
// horizontal 1σ of the ECEF covariance (row-major 3×3).
func horizErr(pos geo.Vec3, cov [9]float64, tr simnet.AircraftTruth) (float64, float64) {
	fr := geo.NewFrame(geo.LLH{Lat: tr.Lat, Lon: tr.Lon, H: tr.HAE})
	d := fr.Rotate(pos.Sub(toGeo(tr.Pos)))
	ce := fr.CovToENU(cov)
	return math.Hypot(d.X, d.Y), math.Sqrt(math.Max(ce[0]+ce[4], 0))
}

func (h *harness) inside(p geo.Vec3) bool {
	fr := geo.NewFrameECEF(h.centre)
	d := fr.ToENU(p)
	return math.Hypot(d.X, d.Y) <= h.cfg.radiusKm*1000
}

// onFix records every accepted snapshot fix (worker hook).
func (h *harness) onFix(key uint32, epochNs int64, f *solve.Fix, holdout bool) {
	h.evalSnap(key, epochNs+int64(math.Round(f.TxS*1e9)), f, holdout)
}

func (h *harness) evalSnap(key uint32, txNs int64, f *solve.Fix, holdout bool) {
	tS := float64(txNs-h.n.UnixNs(0)) / 1e9
	if tS < h.cfg.evalFromS || f.Flags&solve.FlagAmbiguous != 0 || !model.Displayable(f.Grade) {
		return
	}
	tr, ok := h.truth(key, txNs)
	if !ok {
		return
	}
	e, s := horizErr(f.ECEF, f.CovECEF, tr)
	smp := errSample{icao: key, tS: tS, errH: e, sigmaH: s, grade: f.Grade, inside: h.inside(toGeo(tr.Pos))}
	if holdout {
		h.holdSnap = append(h.holdSnap, smp)
	} else {
		h.snap = append(h.snap, smp)
	}
}

// Observe implements simnet.Sink.
func (h *harness) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		if o.Kind != model.FrameModeSShort && o.Kind != model.FrameModeSLong {
			continue
		}
		if h.cfg.absent[o.Rx] {
			continue
		}
		f := h.n.RawFrame(o)
		f.Data = append([]byte(nil), f.Data...)
		if h.cfg.untimed[o.Rx] {
			f.ClockCls, f.Ticks, f.GPSNs, f.Clock12MHz = model.ClockNone, 0, -1, 0
		}
		if h.obsHook != nil {
			h.obsHook(o, &f)
		}
		st := f.Station
		if ep, ok := h.epoch[o.Rx]; ok && ep != o.Epoch {
			var reason model.ClockEventReason
			for _, ev := range h.n.ClockEvents(o.Rx) {
				if ev.NewEpoch == o.Epoch {
					reason = ev.Reason
				}
			}
			h.ing.ClockEvent(st, ingest.ClockEvent{OldEpoch: ep, NewEpoch: o.Epoch, Reason: reason, HostNs: o.HostNs})
		}
		h.epoch[o.Rx] = o.Epoch
		lat := h.cfg.netLatMs[0] + h.rng.Float64()*(h.cfg.netLatMs[1]-h.cfg.netLatMs[0])
		at := max(o.HostNs+int64(lat*1e6), h.lastAt[o.Rx])
		h.lastAt[o.Rx] = at
		h.nseq++
		heap.Push(&h.q, dItem{at: at, seq: h.nseq, f: f})
		h.frames++
		if o.ArrivalS >= h.cfg.evalFromS {
			r := h.seqHeard[o.Seq]
			if r == nil {
				r = &seqRec{icao: o.ICAO}
				h.seqHeard[o.Seq] = r
			}
			r.rx = append(r.rx, o.Rx)
		}
	}
}

// Advance implements simnet.Sink.
func (h *harness) Advance(hz float64) {
	t0 := time.Now()
	until := h.n.UnixNs(hz)
	for len(h.q) > 0 && h.q[0].at <= until {
		it := heap.Pop(&h.q).(dItem)
		h.step(it.at)
		h.ing.Submit([]model.RawFrame{it.f}, false)
	}
	h.step(until)
	// GPS status once a second
	for ; !h.cfg.noGPSStatus && h.nextG <= hz; h.nextG++ {
		for _, i := range h.gps {
			ct, _ := h.n.ClockAt(i, h.nextG)
			h.e.Filter().GPS(model.StationIdx(i+2), clock.GPSStatus{HostNs: h.n.UnixNs(h.nextG), Locked: ct.Locked, UTC: !ct.GPSTime})
		}
	}
	h.engWall += time.Since(t0)
	// footprint bookkeeping (Mode S-only aircraft heard by >= 4 stations)
	for seq, r := range h.seqHeard {
		a := h.ac[r.icao]
		if a == nil {
			a = &acEval{}
			h.ac[r.icao] = a
		}
		if !a.known {
			if tr, ok := h.truth(r.icao, h.n.UnixNs(hz)); ok {
				a.known, a.modeSOnly = true, tr.Sim.MLAT
			}
		}
		d := make([]bool, h.n.NumReceivers()) // (was [64]bool: undercounted networks > 64 stations)
		nd, anch := 0, 0
		for _, x := range r.rx {
			if x < len(d) && !d[x] {
				d[x] = true
				nd++
				if slices.Contains(h.gps, x) {
					anch++
				}
			}
		}
		if nd >= 4 {
			a.heard4++
		}
		if anch >= 4 {
			a.heard4anch++
		}
		delete(h.seqHeard, seq)
	}
}

// step advances the virtual clock to t: ingest sweeps every 10 ms, the
// engine every 100 ms; published fixes are evaluated at once (the truth
// history is short).
func (h *harness) step(t int64) {
	if h.sweep == 0 {
		h.sweep = t
	}
	for h.sweep+10e6 <= t {
		h.sweep += 10e6
		h.now = h.sweep
		h.ing.Sweep(time.Unix(0, h.sweep))
		if h.sweep >= h.nextAd {
			h.e.Advance(h.sweep)
			h.nextAd = h.sweep + 100e6
		}
		h.collect()
	}
	h.now = max(h.now, t)
}

func (h *harness) collect() {
	for {
		select {
		case f := <-h.e.Fixes():
			h.evalPub(&f)
		default:
			return
		}
	}
}

func (h *harness) evalPub(f *model.MLATFix) {
	tS := float64(f.TUnixNs-h.n.UnixNs(0)) / 1e9
	if tS < h.cfg.evalFromS {
		return
	}
	tr, ok := h.truth(f.ICAO, f.TUnixNs)
	if h.pubHook != nil {
		h.pubHook(f, tr, ok)
	}
	if !ok {
		return
	}
	c := f.Cov
	cov := [9]float64{float64(c[0]), float64(c[1]), float64(c[2]), float64(c[1]), float64(c[3]), float64(c[4]),
		float64(c[2]), float64(c[4]), float64(c[5])}
	p := geo.Vec3{X: f.ECEF[0], Y: f.ECEF[1], Z: f.ECEF[2]}
	e, s := horizErr(p, cov, tr)
	smp := errSample{icao: f.ICAO, tS: tS, errH: e, sigmaH: s, grade: f.Grade, inside: h.inside(toGeo(tr.Pos)), pos: p, flags: f.Flags}
	if f.Flags&model.FixFlagHoldout != 0 {
		h.hold = append(h.hold, smp)
		if h.dbgHold > 0 && e > 50 && smp.inside {
			h.dbgHold--
			h.tb.Logf("hold-out sample icao %06x tS %.2f now %.2f err %.0f σ %.0f flags %b grade %d", f.ICAO, tS, float64(h.now-h.n.UnixNs(0))/1e9, e, s, f.Flags, f.Grade)
		}
		return
	}
	h.pub = append(h.pub, smp)
	if a := h.ac[f.ICAO]; a != nil {
		if a.published == 0 {
			a.firstPubS = tS
		}
		a.published++
	} else {
		h.ac[f.ICAO] = &acEval{published: 1, firstPubS: tS}
	}
}

func (h *harness) run(d time.Duration) {
	if err := h.n.Advance(context.Background(), d); err != nil {
		h.tb.Fatal(err)
	}
	h.collect()
}

// dist summarises a sample set.
type dist struct {
	n             int
	p50, p95, p99 float64
	cal1, cal2    float64
	gross         int // > 5σ + 50 m
}

func summarize(s []errSample, inside bool) dist {
	var e []float64
	in1, in2, gross := 0, 0, 0
	for _, x := range s {
		if inside && !x.inside {
			continue
		}
		e = append(e, x.errH)
		if x.errH <= x.sigmaH {
			in1++
		}
		if x.errH <= 2*x.sigmaH {
			in2++
		}
		if x.errH > 5*x.sigmaH+50 {
			gross++
		}
	}
	if len(e) == 0 {
		return dist{}
	}
	slices.Sort(e)
	q := func(p float64) float64 { return e[min(len(e)-1, int(p*float64(len(e))))] }
	return dist{n: len(e), p50: q(0.5), p95: q(0.95), p99: q(0.99),
		cal1: float64(in1) / float64(len(e)), cal2: float64(in2) / float64(len(e)), gross: gross}
}

// tap sits between the ingest and the engine (allocation measurements).
type tap harness

func (t *tap) Observe(g *ingest.Group) {
	if t.buffer {
		t.buffered = append(t.buffered, g.Clone())
		return
	}
	t.e.Observe(g)
}

func (t *tap) ObserveLate(g *ingest.Group) {
	if t.buffer {
		return
	}
	t.e.ObserveLate(g)
}
