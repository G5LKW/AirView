package survey

import (
	"container/heap"
	"context"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

// The simnet harness: simnet → RawFrames with network latency → the real
// ingest grouper (virtual clock) → survey.Tee → the real MLAT engine
// (clock filter, reference store). The survey target's position is never
// given to the engine (unlocated) or given wrong (declared check); the
// survey's results are compared with simnet's truth.

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

// target describes one survey target of a harness run.
type target struct {
	dev simnet.Device
	// east/north offset from the network centre (km)
	eKm, nKm float64
	// declared: the position given to the engine and the survey (nil =
	// unlocated); declErrM moves it north-east by that many metres.
	declared bool
	declErrM float64
	altRef   string // declared altitude reference (simnet DeclaredAltRef)
	hint     bool   // give a Locate Me hint 20 km off
}

type simCfg struct {
	seed     uint64
	gps, sdr int // peers
	beast    int
	targets  []target
	aircraft int
	cfg      Config
	warmup   time.Duration
	// bundle: a Bundler is fed (after the engine) and the targets go to it
	// instead of the survey
	bundle bool
}

type simHarness struct {
	tb      testing.TB
	n       *simnet.Network
	ing     *ingest.Ingest
	e       *mlat.Engine
	s       *Surveyor
	bd      *Bundler
	q       dq
	nseq    int64
	now     int64
	sweep   int64
	nextAd  int64
	nextTk  int64
	rng     *rand.Rand
	lastAt  map[int]int64
	epoch   map[int]uint32
	gps     []int
	nextG   float64
	tIdx    []int // receiver index of each target
	results []Result
	flags   map[model.StationIdx]float64 // declared-suspect verdicts (discrepancy)
	sunk    map[model.StationIdx]mlat.Survey
	pending []pendingTarget
	t0      int64 // when the targets were registered
}

const geoidN = 47.0 // simnet's default geoid undulation

func newSim(tb testing.TB, c simCfg) *simHarness {
	if c.aircraft == 0 {
		c.aircraft = 250
	}
	peers := c.gps + c.sdr + c.beast
	base := simnet.RandomReceivers(c.seed, peers, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 150})
	rr := rand.New(rand.NewPCG(c.seed, 77))
	var specs []simnet.ReceiverSpec
	devs := []simnet.Device{}
	for i := 0; i < c.gps; i++ {
		devs = append(devs, simnet.DeviceRadarcapeGPS)
	}
	for i := 0; i < c.beast; i++ {
		devs = append(devs, simnet.DeviceBeast)
	}
	for i := 0; i < c.sdr; i++ {
		devs = append(devs, simnet.DeviceSDR)
	}
	for i, d := range devs {
		s := base[i]
		s.Clock = simnet.DefaultClockSpec(d, rr)
		s.Reception = simnet.DefaultReception(d, rr)
		s.DeclaredErrorM = 0
		specs = append(specs, s)
	}
	cf := geo.NewFrame(geo.LLH{Lat: 51, Lon: 9})
	var tIdx []int
	for _, t := range c.targets {
		p := geo.FromECEF(cf.FromENU(geo.Vec3{X: t.eKm * 1000, Y: t.nKm * 1000}))
		s := simnet.ReceiverSpec{Lat: p.Lat, Lon: p.Lon, GroundHAE: 180 + geoidN, AntennaAGL: 12}
		s.Clock = simnet.DefaultClockSpec(t.dev, rr)
		s.Reception = simnet.DefaultReception(t.dev, rr)
		// a silent sample drop would be a different test; keep the run clean
		s.Clock.SilentDropsPerHour, s.Clock.DeclaredDropsPerHour = 0, 0
		s.DeclaredAltRef = t.altRef
		if t.altRef == "ambiguous" || t.altRef == "" {
			s.DeclaredAltRef = "msl" // the truth behind an ambiguous declaration
		}
		tIdx = append(tIdx, len(specs))
		specs = append(specs, s)
	}
	n, err := simnet.New(simnet.Config{Seed: c.seed, Aircraft: c.aircraft, Receivers: specs, CenterLat: 51, CenterLon: 9,
		GeoidUndulationM: geoidN})
	if err != nil {
		tb.Fatal(err)
	}
	h := &simHarness{tb: tb, n: n, rng: rand.New(rand.NewPCG(c.seed, 5)), lastAt: map[int]int64{}, epoch: map[int]uint32{},
		tIdx: tIdx, flags: map[model.StationIdx]float64{}, sunk: map[model.StationIdx]mlat.Survey{}}
	h.now = n.UnixNs(0)
	a := n.Config().Atmosphere
	mc := model.DefaultAggregator().MLAT
	e, err := mlat.New(mlat.Options{Config: mc, OriginUnixNs: n.UnixNs(0), Now: h.clock, Workers: -1,
		Atmosphere: geo.Atmosphere{N0: a.N0, H: a.ScaleHeightM, K: a.KAtm}, HoldoutSalt: c.seed,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		tb.Fatal(err)
	}
	h.e = e
	cfg := c.cfg
	cfg.AutoUse = true
	sv, err := New(Options{Config: cfg, Clock: e.Filter(), Refs: e.Refs(), Atmosphere: e.Atmosphere, Sink: (*sinkTap)(h),
		Declared: (*sinkTap)(h), Notifier: (*sinkTap)(h), Now: h.clock, Synchronous: true,
		Undulation: func(float64, float64) (float64, bool) { return geoidN, true },
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		tb.Fatal(err)
	}
	h.s = sv
	var next ingest.MLATSink = e
	if c.bundle {
		bd, err := NewBundler(BundlerOptions{Refs: e.Refs(), Atmosphere: e.Atmosphere, Now: h.clock,
			Undulation: func(float64, float64) (float64, bool) { return geoidN, true },
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			tb.Fatal(err)
		}
		h.bd = bd
		next = bundleTee{e: e, b: bd}
	}
	ing, err := ingest.New(ingest.Options{Handler: fakeTracker{}, MLAT: Tee{Next: next, S: sv}, Clock: e.ClockView(), ClockObserver: e, Now: h.clock})
	if err != nil {
		tb.Fatal(err)
	}
	h.ing = ing
	isTarget := map[int]int{}
	for k, i := range tIdx {
		isTarget[i] = k
	}
	for i, r := range n.Receivers() {
		st := model.StationIdx(i + 2)
		k, tgt := isTarget[i]
		if r.Device == simnet.DeviceRadarcapeGPS {
			h.gps = append(h.gps, i)
		}
		if !tgt {
			e.SetStation(st, mlat.StationInfo{Class: r.Class,
				Declared: mlat.Position{Valid: true, ECEF: geo.Vec3{X: r.Pos[0], Y: r.Pos[1], Z: r.Pos[2]}, SigmaM: 5}})
			// peers: GNSS or owner-confirmed (no periodic checks in this harness)
			sv.SetStation(st, Station{Class: r.Class, Timestamps: true, GNSS: r.Class.GPS(), Confirmed: !r.Class.GPS()})
			if h.bd != nil {
				pos := geo.Vec3{X: r.Pos[0], Y: r.Pos[1], Z: r.Pos[2]}
				h.bd.SetStation(st, BundleMember{Class: r.Class, Timestamps: true, Anchor: &pos, AnchorSigmaM: 5})
			}
			continue
		}
		t := c.targets[k]
		info := Station{Class: r.Class, Timestamps: true, AGLM: 12}
		mi := mlat.StationInfo{Class: r.Class}
		if t.declared {
			fr := geo.NewFrame(geo.LLH{Lat: r.Lat, Lon: r.Lon, H: r.HAE})
			d := t.declErrM / math.Sqrt2
			p := geo.FromECEF(fr.FromENU(geo.Vec3{X: d, Y: d}))
			alt := r.HAE
			ref := model.AltRefHAE
			switch t.altRef {
			case model.AltRefMSL:
				alt, ref = r.HAE-geoidN, model.AltRefMSL
			case model.AltRefAmbiguous:
				alt, ref = r.HAE-geoidN, model.AltRefAmbiguous // truth: MSL
			}
			info.Declared = &Declared{Lat: p.Lat, Lon: p.Lon, Alt: alt, AltRef: ref}
			mi.Declared = mlat.Position{Valid: true, ECEF: geo.ToECEF(p.Lat, p.Lon, r.HAE), SigmaM: 30}
		}
		if t.hint {
			fr := geo.NewFrame(geo.LLH{Lat: r.Lat, Lon: r.Lon})
			p := geo.FromECEF(fr.FromENU(geo.Vec3{X: 14e3, Y: -14e3}))
			info.Hint = &p
		}
		e.SetStation(st, mi)
		if h.bd != nil {
			h.bd.SetStation(st, BundleMember{Class: r.Class, Timestamps: true, Declared: info.Declared, Hint: info.Hint})
			continue
		}
		h.pending = append(h.pending, pendingTarget{st, info})
	}
	n.AddSink(h)
	if c.warmup > 0 {
		// the network runs (and its GPS stations become anchors) before
		// the targets are registered with the survey
		h.run(c.warmup)
	}
	for _, p := range h.pending {
		sv.SetStation(p.st, p.info)
	}
	h.t0 = h.now
	return h
}

type pendingTarget struct {
	st   model.StationIdx
	info Station
}

// sinkTap records what the surveyor publishes.
type sinkTap simHarness

func (t *sinkTap) SetSurvey(st model.StationIdx, sv mlat.Survey) {
	t.sunk[st] = sv
	t.e.SetSurvey(st, sv)
}
func (t *sinkTap) SetDeclaredSuspect(st model.StationIdx, suspect bool, d float64) {
	if suspect {
		t.flags[st] = d
	} else {
		delete(t.flags, st)
	}
}
func (t *sinkTap) SurveyFinished(r Result) { t.results = append(t.results, r) }

func (h *simHarness) clock() time.Time { return time.Unix(0, h.now) }

// Observe implements simnet.Sink.
func (h *simHarness) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		if o.Kind != model.FrameModeSShort && o.Kind != model.FrameModeSLong {
			continue
		}
		f := h.n.RawFrame(o)
		f.Data = append([]byte(nil), f.Data...)
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
		lat := 20 + h.rng.Float64()*60
		at := max(o.HostNs+int64(lat*1e6), h.lastAt[o.Rx])
		h.lastAt[o.Rx] = at
		h.nseq++
		heap.Push(&h.q, dItem{at: at, seq: h.nseq, f: f})
	}
}

// Advance implements simnet.Sink.
func (h *simHarness) Advance(hz float64) {
	until := h.n.UnixNs(hz)
	for len(h.q) > 0 && h.q[0].at <= until {
		it := heap.Pop(&h.q).(dItem)
		h.step(it.at)
		h.ing.Submit([]model.RawFrame{it.f}, false)
	}
	h.step(until)
	for ; h.nextG <= hz; h.nextG++ {
		for _, i := range h.gps {
			ct, _ := h.n.ClockAt(i, h.nextG)
			h.e.Filter().GPS(model.StationIdx(i+2), clock.GPSStatus{HostNs: h.n.UnixNs(h.nextG), Locked: ct.Locked, UTC: !ct.GPSTime})
		}
	}
}

func (h *simHarness) step(t int64) {
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
		if h.sweep >= h.nextTk {
			h.s.Tick()
			h.nextTk = h.sweep + 5e9
		}
		for {
			select {
			case <-h.e.Fixes():
				continue
			default:
			}
			break
		}
	}
	h.now = max(h.now, t)
}

func (h *simHarness) run(d time.Duration) {
	if err := h.n.Advance(context.Background(), d); err != nil {
		h.tb.Fatal(err)
	}
}

// truthErr returns the horizontal and vertical error of a result against
// the target's true position.
func (h *simHarness) truthErr(k int, r *Result) (eh, ev float64) {
	tr := h.n.Receiver(h.tIdx[k])
	fr := geo.NewFrame(geo.LLH{Lat: tr.Lat, Lon: tr.Lon, H: tr.HAE})
	d := fr.ToENU(r.Solution.ECEF)
	return math.Hypot(d.X, d.Y), d.Z
}

func (h *simHarness) station(k int) model.StationIdx { return model.StationIdx(h.tIdx[k] + 2) }

// resultOf returns the first finished result of target k.
func (h *simHarness) resultOf(k int) (Result, bool) {
	for _, r := range h.results {
		if r.Station == h.station(k) {
			return r, true
		}
	}
	return Result{}, false
}

func (h *simHarness) logResult(k int, label string, r Result) {
	eh, ev := h.truthErr(k, &r)
	sol := &r.Solution
	h.tb.Logf("stats %+v", h.s.Stats())
	if r.Error != "" {
		h.tb.Logf("%s: error %s", label, r.Error)
	}
	h.tb.Logf("%s: state %s after %.0f s: err %.1f m (dz %+.1f) σh %.1f σv %.1f stable %v suspect %v %v autoUse %v | obs %d used %d rej %d ac %d az %.0f° jitter %.0f ns seg %d brk %d halves %.1f m χ² %.1f | declared %v dist %.0f m limit %.0f m suspect %v | altRef %+v",
		label, r.State, r.Finished.Sub(r.Started).Seconds(), eh, ev, sol.SigmaHM, sol.SigmaVM, r.Stable, r.Suspect, r.SuspectWhy, r.AutoUse,
		r.Observations, sol.Used, sol.Rejected, sol.Aircraft, sol.AzSpreadDeg, sol.JitterNs, sol.Segments, sol.Breaks, sol.HalvesM, sol.HalvesChi2,
		r.DeclaredChecked, r.DeclaredDistM, r.DeclaredLimitM, r.DeclaredSuspect, r.AltRef)
}
