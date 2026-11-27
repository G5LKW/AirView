package mlat

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// The MLAT results suite (docs/aggregator/MLAT-RESULTS.md): every DESIGN
// §13.2 accuracy criterion measured end to end (simnet → real ingest →
// engine) on realistic receiver mixes, with the truth kept outside the
// engine. Opt-in, because the large networks simulate for many minutes:
//
//	MLAT_RESULTS=all|<name>[,<name>...] [MLAT_RESULTS_OUT=file.jsonl] go test -run TestMLATResults -v -timeout 6h ./internal/mlat
//
// Per scenario it reports:
//   - horizontal and vertical error of the published positions (tracker
//     output, displayable grades) against the true position at the
//     published transmit time: P50/P95/P99, σ calibration;
//   - the same for the per-cluster solver fixes and the hold-out tracks;
//   - availability: Mode S-only aircraft (no ADS-B position) inside the
//     network radius heard by >= 4 stations, per aircraft and per second;
//   - clock-sync accuracy: every synced station's network-time error
//     against the truth, relative to the median of the anchors (or of the
//     free clocks) of its component, per device, plus normalised errors;
//   - false fixes: positions > 1 km or > 5σ + 50 m off, and positions of
//     addresses no simulated aircraft has;
//   - latency from transmission to publication (virtual time, 10 ms
//     resolution, 20-80 ms network latency unless the scenario says);
//   - CPU: wall time of ingest + engine per simulated second (one core,
//     synchronous mode) and the engine's own µs per group; clock epoch
//     cost; memory retained by ingest + engine (heap before and after
//     releasing them);
//   - the step-recovery and anchor-fault scenarios add their own timings.

type resScenario struct {
	name  string
	about string
	hc    harnessCfg
	specs []simnet.ReceiverSpec
	dur   time.Duration
	// silent step on one station (0 = none)
	stepRx int
	stepAt float64
	stepNs float64
	// anchor fault: +faultNs on every frame of a GPS station from faultAt
	faultRx int
	faultAt float64
	faultNs float64
}

// buildSpecs places receivers like newHarness (device order GPS, Beast,
// Radarcape12, Airspy, SDR, SBS) and lets the scenario change them.
func buildSpecs(seed uint64, devs map[simnet.Device]int, radiusKm float64, mut func(i int, s *simnet.ReceiverSpec)) []simnet.ReceiverSpec {
	order := []simnet.Device{simnet.DeviceRadarcapeGPS, simnet.DeviceBeast, simnet.DeviceRadarcape12, simnet.DeviceAirspy, simnet.DeviceSDR, simnet.DeviceSBS}
	total := 0
	for _, d := range order {
		total += devs[d]
	}
	base := simnet.RandomReceivers(seed, total, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: radiusKm})
	rr := rand.New(rand.NewPCG(seed, 77))
	var out []simnet.ReceiverSpec
	k := 0
	for _, d := range order {
		for j := 0; j < devs[d]; j++ {
			s := base[k]
			s.Clock = simnet.DefaultClockSpec(d, rr)
			s.Reception = simnet.DefaultReception(d, rr)
			if mut != nil {
				mut(k, &s)
			}
			out = append(out, s)
			k++
		}
	}
	return out
}

func resScenarios() []resScenario {
	const minute = time.Minute
	ev := 12 * 60.0
	mixed20 := map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 3, simnet.DeviceRadarcape12: 2, simnet.DeviceSDR: 15}
	var out []resScenario
	// §13.2 rows 1-3 (the CI gate configurations and seeds)
	out = append(out,
		resScenario{name: "row1", about: "8 GPS Radarcapes, 150 km (§13.2 row 1)", dur: 16 * minute,
			hc: harnessCfg{seed: 11, devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 8}, radiusKm: 150, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev}},
		resScenario{name: "row2", about: "4 GPS + 20 RTL-SDR, 150 km (§13.2 row 2)", dur: 16 * minute,
			hc: harnessCfg{seed: 12, devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 4, simnet.DeviceSDR: 20}, radiusKm: 150, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev}},
		resScenario{name: "row3", about: "20 RTL-SDR only, 150 km (§13.2 row 3)", dur: 16 * minute,
			hc: harnessCfg{seed: 13, devs: map[simnet.Device]int{simnet.DeviceSDR: 20}, radiusKm: 150, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev}},
	)
	// realistic community mixes at three sizes
	out = append(out,
		resScenario{name: "mix20", about: "20 stations: 3 GPS Radarcapes, 2 Radarcapes (12 MHz free clock), 15 RTL-SDR (12 MHz timestamps, 2.4 MS/s); 150 km; 250 aircraft",
			dur: 16 * minute, hc: harnessCfg{seed: 21, radiusKm: 150, aircraft: 250, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev},
			specs: buildSpecs(21, mixed20, 150, nil)},
		resScenario{name: "mix100", about: "100 stations, DefaultMix (62 % RTL-SDR, 10 % Airspy, 10 % Beast, 4 % Radarcape 12 MHz, 10 % GPS Radarcape, 4 % SBS); 300 km; 600 aircraft",
			dur: 16 * minute, hc: harnessCfg{seed: 22, radiusKm: 300, aircraft: 600, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev},
			specs: simnet.RandomReceivers(22, 100, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 300})},
		resScenario{name: "mix300", about: "300 stations, DefaultMix; 450 km; 1,500 aircraft",
			dur: 16 * minute, hc: harnessCfg{seed: 23, radiusKm: 450, aircraft: 1500, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev},
			specs: simnet.RandomReceivers(23, 300, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 450})},
	)
	// harsh: heavy multipath and outliers, SDR clock steps and drops, slow links
	out = append(out, resScenario{name: "harsh20",
		about: "mix20 layout with heavy impairments: multipath 8 % (mean 250 ns), outliers 0.2 % (±5 µs), SDR silent steps 2/h (±30 µs), silent drops 0.5/h, declared drops 1/h, network latency 20-300 ms",
		dur:   16 * minute, hc: harnessCfg{seed: 24, radiusKm: 150, aircraft: 250, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev, netLatMs: [2]float64{20, 300}},
		specs: buildSpecs(24, mixed20, 150, func(i int, s *simnet.ReceiverSpec) {
			s.Reception.MultipathProb, s.Reception.MultipathMeanNs = 0.08, 250
			s.Reception.OutlierProb, s.Reception.OutlierMaxNs = 2e-3, 5000
			if s.Clock.Device == simnet.DeviceSDR {
				s.Clock.SilentStepsPerHour, s.Clock.SilentStepMaxNs = 2, 30000
				s.Clock.SilentDropsPerHour, s.Clock.DeclaredDropsPerHour = 0.5, 1
			}
		})})
	// poor geometry: every station on the southern half of the disc (coast)
	coast := simnet.RandomReceivers(25, 30, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 150})
	for i := range coast {
		if coast[i].Lat > 51 {
			coast[i].Lat = 51 - (coast[i].Lat - 51)
		}
	}
	out = append(out, resScenario{name: "coast30", about: "30 stations, DefaultMix, all on the southern half of a 150 km disc (a coastline): aircraft over the sea are outside the hull",
		dur: 16 * minute, hc: harnessCfg{seed: 25, radiusKm: 150, aircraft: 300, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev}, specs: coast})
	// silent 22 µs step on an RTL-SDR after everything has settled
	out = append(out, resScenario{name: "step", about: "mix20 layout (seed 26); silent 22 µs step on one RTL-SDR at t = 800 s",
		dur: 16 * minute, hc: harnessCfg{seed: 26, radiusKm: 150, aircraft: 250, modeSFrac: 0.3, holdout: 0.1, evalFromS: ev},
		specs: buildSpecs(26, mixed20, 150, func(i int, s *simnet.ReceiverSpec) {
			if i == 6 { // an RTL-SDR
				s.Clock.SilentDropsPerHour, s.Clock.DeclaredDropsPerHour = 0, 0
				s.Clock.Steps = []simnet.ClockStep{{AtS: 800, StepNs: 22000}}
			}
		}), stepRx: 6, stepAt: 800, stepNs: 22000})
	// 1 µs fault on a GPS anchor (its status bits stay good)
	out = append(out, resScenario{name: "anchorfault", about: "8 GPS Radarcapes + 2 Beasts + 12 RTL-SDR, 150 km; +1 µs on every timestamp of one GPS anchor from t = 900 s, GPS status stays locked",
		dur: 22 * minute, hc: harnessCfg{seed: 27, radiusKm: 150, aircraft: 250, modeSFrac: 0.3, holdout: 0.1, evalFromS: 18 * 60},
		specs:   buildSpecs(27, map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 8, simnet.DeviceBeast: 2, simnet.DeviceSDR: 12}, 150, nil),
		faultRx: 2, faultAt: 900, faultNs: 1000})
	return out
}

// resPub is one published position against the truth.
type resPub struct {
	icao             uint32
	tS, latS         float64
	errH, errV       float64
	sigH, sigV       float64
	inside, holdout  bool
	grade            uint8
	alt3, ambiguous  bool
	relative, coastd bool
	climbing         bool // |true vertical rate| > 2.5 m/s
}

type resSeq struct {
	icao uint32
	txS  float64
	rx   []int32
}

type clockSample struct {
	dev  string
	rel  float64 // ns, against the component reference
	norm float64 // rel / σ
}

// resSink collects the truth-side bookkeeping (added to the network after
// the harness, so it sees every observation and advance after it).
type resSink struct {
	h      *harness
	sc     *resScenario
	seqs   map[uint64]*resSeq
	bias   []float64
	biasN  []float64
	devs   []string
	gps    []bool
	nextS  float64
	nextC  float64
	lastSq uint64

	heardSec map[uint32]map[int32]bool // icao → seconds with a >= 4-station transmission
	heard4   map[uint32]int
	acSec    map[uint32]map[int32]bool // icao → seconds Mode S-only and inside
	modeS    map[uint32]bool
	pubSec   map[uint32]map[int32]bool

	pubs    []resPub
	noTruth int
	ever    map[uint32]bool // every address the simulation has had
	ghost   int
	clk     []clockSample
	netUTC  []float64 // median anchor error (network time - UTC), ns

	// step scenario
	stepDetect, stepResync float64
	stepBeyond3, stepEp    int
	// anchor fault
	demotedAt float64
	cmSeries  [][2]float64 // (t, common mode of the good anchors)
}

func newResSink(h *harness, sc *resScenario) *resSink {
	n := h.n.NumReceivers()
	r := &resSink{h: h, sc: sc, seqs: map[uint64]*resSeq{}, bias: make([]float64, n), biasN: make([]float64, n),
		devs: make([]string, n), gps: make([]bool, n), heardSec: map[uint32]map[int32]bool{}, heard4: map[uint32]int{},
		acSec: map[uint32]map[int32]bool{}, modeS: map[uint32]bool{}, ever: map[uint32]bool{}, pubSec: map[uint32]map[int32]bool{},
		stepDetect: -1, stepResync: -1, demotedAt: -1, nextS: math.Ceil(sc.hc.evalFromS)}
	for i, rt := range h.n.Receivers() {
		r.devs[i] = string(rt.Device)
		r.gps[i] = rt.Device == simnet.DeviceRadarcapeGPS
	}
	h.pubHook = r.onPub
	if sc.faultNs != 0 {
		rx, at, d := sc.faultRx, sc.faultAt, sc.faultNs
		h.obsHook = func(o *simnet.Observation, f *model.RawFrame) {
			if o.Rx == rx && o.ArrivalS >= at {
				f.Ticks += uint64(d)
				f.GPSNs = int64(f.Ticks % uint64(model.NsPerDay))
			}
		}
	}
	return r
}

func (r *resSink) Observe(obs []simnet.Observation) {
	ev := r.sc.hc.evalFromS
	for i := range obs {
		o := &obs[i]
		if o.Kind != model.FrameModeSShort && o.Kind != model.FrameModeSLong {
			continue
		}
		// quantisation bias of the receiver (truth comparison of the clocks)
		if r.biasN[o.Rx] < 20000 {
			r.bias[o.Rx] += r.h.n.TickError(o)
			r.biasN[o.Rx]++
		}
		if o.Tx.TxS < ev {
			continue
		}
		s := r.seqs[o.Seq]
		if s == nil {
			s = &resSeq{icao: o.ICAO, txS: o.Tx.TxS}
			r.seqs[o.Seq] = s
		}
		if !slices.Contains(s.rx, int32(o.Rx)) {
			s.rx = append(s.rx, int32(o.Rx))
		}
	}
}

func setAdd(m map[uint32]map[int32]bool, k uint32, s int32) {
	x := m[k]
	if x == nil {
		x = map[int32]bool{}
		m[k] = x
	}
	x[s] = true
}

func (r *resSink) Advance(hz float64) {
	h := r.h
	for k, s := range r.seqs {
		if s.txS > hz-1 {
			continue
		}
		delete(r.seqs, k)
		if len(s.rx) >= 4 {
			setAdd(r.heardSec, s.icao, int32(math.Floor(s.txS)))
			r.heard4[s.icao]++
		}
	}
	if math.Floor(hz) != math.Floor(hz-0.1) || len(r.ever) == 0 {
		for _, a := range h.n.Aircraft() {
			r.ever[a.ICAO] = true
		}
	}
	for ; r.nextS <= hz; r.nextS++ {
		for _, a := range h.n.Aircraft() {
			ms := a.Sim.MLAT
			r.modeS[a.ICAO] = ms
			if ms && h.inside(toGeo(a.Pos)) {
				setAdd(r.acSec, a.ICAO, int32(r.nextS))
			}
		}
	}
	snap := h.e.Filter().Snapshot()
	if snap == nil || snap.Seq == r.lastSq {
		return
	}
	r.lastSq = snap.Seq
	tq := snap.T - 0.05
	if r.sc.stepNs != 0 && tq >= r.sc.stepAt-5 {
		r.stepCheck(snap, tq)
	}
	if r.sc.faultNs != 0 {
		r.faultCheck(snap, tq)
	}
	if snap.T < r.nextC || snap.T < r.sc.hc.evalFromS {
		return
	}
	r.nextC = snap.T + 5
	r.clockSample(snap, tq)
}

// stationErr is the network-time error of a station's clock at t (ns)
// and its σ: the snapshot's conversion of the receiver's true (noise-free)
// local time, minus t. It mirrors the clock package's simnet harness.
func (r *resSink) stationErr(rx int, t float64) (e, sig float64, synced, anchor bool, comp uint32, ok bool) {
	h := r.h
	sc := h.e.Filter().Snapshot().Station(model.StationIdx(rx + 2))
	if sc == nil || sc.Hz == 0 {
		return
	}
	ct, good := h.n.ClockAt(rx, t)
	if !good || ct.Epoch != sc.Epoch {
		return
	}
	meanBias := 0.0
	if r.biasN[rx] > 0 {
		meanBias = r.bias[rx] / r.biasN[rx]
	}
	extra := 0.0
	if r.sc.faultNs != 0 && rx == r.sc.faultRx && t >= r.sc.faultAt {
		extra = r.sc.faultNs
	}
	if ct.Class.GPS() && ct.Locked {
		extra -= ct.PPSErrNs // white 1PPS noise, not a trackable offset
	}
	var y float64
	if sc.Hz == 1e9 {
		start := h.n.UnixNs(0)
		y = sc.BaseS + (float64(start-int64(sc.BaseTicks))+ct.LocalS*1e9+meanBias+extra)/1e9
	} else {
		y = (ct.Ticks+meanBias-float64(sc.BaseTicks))/sc.Hz + sc.BaseS + extra*1e-9
	}
	tn, v := sc.NetFromLocal(y, false)
	return (tn - t) * 1e9, math.Sqrt(v), sc.Synced, sc.Anchor, sc.Comp, true
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	w := slices.Clone(v)
	slices.Sort(w)
	return w[len(w)/2]
}

type compAcc struct {
	anch, all []float64
}

func (r *resSink) refs(t float64, skip int) map[uint32]float64 {
	acc := map[uint32]*compAcc{}
	for rx := range r.devs {
		if rx == skip {
			continue
		}
		e, _, synced, anchor, comp, ok := r.stationErr(rx, t)
		if !ok || !synced {
			continue
		}
		a := acc[comp]
		if a == nil {
			a = &compAcc{}
			acc[comp] = a
		}
		a.all = append(a.all, e)
		if anchor {
			a.anch = append(a.anch, e)
		}
	}
	out := map[uint32]float64{}
	for c, a := range acc {
		if len(a.anch) > 0 {
			out[c] = median(a.anch)
		} else {
			out[c] = median(a.all)
		}
	}
	return out
}

// clockSample records every synced station's error against its
// component reference and the network-time offset against UTC.
func (r *resSink) clockSample(snap *clock.Snapshot, t float64) {
	ref := r.refs(t, -1)
	var anch []float64
	for rx := range r.devs {
		e, sg, synced, anchor, comp, ok := r.stationErr(rx, t)
		if !ok || !synced {
			continue
		}
		if anchor {
			anch = append(anch, e)
			if snap.Station(model.StationIdx(rx+2)).Anchor && r.gps[rx] && len(snap.Components) > 0 {
				// anchors are their own reference: skip them in rel stats only when they define it alone
			}
		}
		rel := e - ref[comp]
		r.clk = append(r.clk, clockSample{dev: r.devs[rx], rel: rel, norm: rel / math.Max(sg, 1)})
	}
	if len(anch) > 0 {
		r.netUTC = append(r.netUTC, median(anch))
	}
}

// stepCheck follows the stepped station every epoch.
func (r *resSink) stepCheck(snap *clock.Snapshot, t float64) {
	rx := r.sc.stepRx
	sc := snap.Station(model.StationIdx(rx + 2))
	if sc == nil {
		return
	}
	if r.stepDetect < 0 && t >= r.sc.stepAt {
		for _, ev := range sc.Events {
			if ev.Reason == "step" && ev.T >= r.sc.stepAt-0.5 {
				r.stepDetect = ev.T
			}
		}
	}
	e, sg, synced, _, comp, ok := r.stationErr(rx, t)
	if !ok || !synced {
		return
	}
	ref, okr := r.refs(t, rx)[comp]
	if !okr {
		return
	}
	d := e - ref
	if t >= r.sc.stepAt && t <= r.sc.stepAt+30 {
		r.stepEp++
		if math.Abs(d) > 3*sg+5 {
			r.stepBeyond3++
		}
	}
	if r.stepDetect > 0 && r.stepResync < 0 && t > r.stepDetect && math.Abs(d) < 3*sg+5 {
		r.stepResync = t
	}
}

// faultCheck follows the faulty anchor and the network time of the others.
func (r *resSink) faultCheck(snap *clock.Snapshot, t float64) {
	rx := r.sc.faultRx
	if sc := snap.Station(model.StationIdx(rx + 2)); sc != nil && r.demotedAt < 0 && t >= r.sc.faultAt && !sc.Anchor {
		r.demotedAt = t
	}
	if t < r.sc.faultAt-180 || math.Mod(math.Round(t*2), 4) != 0 {
		return
	}
	var es []float64
	for i := range r.devs {
		if i == rx || !r.gps[i] {
			continue
		}
		if e, _, synced, _, _, ok := r.stationErr(i, t); ok && synced {
			es = append(es, e)
		}
	}
	if len(es) > 0 {
		r.cmSeries = append(r.cmSeries, [2]float64{t, median(es)})
	}
}

// ---------------------------------------------------------------- fixes

func (r *resSink) onPub(f *model.MLATFix, tr simnet.AircraftTruth, ok bool) {
	h := r.h
	hold := f.Flags&model.FixFlagHoldout != 0
	if !ok {
		if !hold {
			r.noTruth++
			if !r.ever[f.ICAO] {
				r.ghost++
			}
		}
		return
	}
	c := f.Cov
	cov := [9]float64{float64(c[0]), float64(c[1]), float64(c[2]), float64(c[1]), float64(c[3]), float64(c[4]),
		float64(c[2]), float64(c[4]), float64(c[5])}
	p := geo.Vec3{X: f.ECEF[0], Y: f.ECEF[1], Z: f.ECEF[2]}
	fr := geo.NewFrame(geo.LLH{Lat: tr.Lat, Lon: tr.Lon, H: tr.HAE})
	d := fr.Rotate(p.Sub(toGeo(tr.Pos)))
	ce := fr.CovToENU(cov)
	tS := float64(f.TUnixNs-h.n.UnixNs(0)) / 1e9
	lat := float64(h.now-f.TUnixNs) / 1e9
	x := resPub{icao: f.ICAO, tS: tS, latS: lat, errH: math.Hypot(d.X, d.Y), errV: math.Abs(d.Z),
		sigH: math.Sqrt(math.Max(ce[0]+ce[4], 0)), sigV: math.Sqrt(math.Max(ce[8], 0)),
		inside: h.inside(toGeo(tr.Pos)), holdout: hold, grade: f.Grade,
		alt3: f.Flags&model.FixFlagAltConstrained != 0, ambiguous: f.Flags&model.FixFlagAmbiguous != 0,
		relative: f.Flags&model.FixFlagRelativeTime != 0, coastd: lat < 0.0005,
		climbing: math.Abs(fr.Rotate(toGeo(tr.Vel)).Z) > 2.5}
	r.pubs = append(r.pubs, x)
	if !hold {
		setAdd(r.pubSec, f.ICAO, int32(math.Floor(tS)))
	}
}

// ---------------------------------------------------------------- summary

type pct struct {
	N             int
	P50, P95, P99 float64
}

func pcts(v []float64) pct {
	if len(v) == 0 {
		return pct{}
	}
	w := slices.Clone(v)
	slices.Sort(w)
	q := func(p float64) float64 { return w[min(len(w)-1, int(p*float64(len(w))))] }
	return pct{N: len(w), P50: q(0.5), P95: q(0.95), P99: q(0.99)}
}

// ResResult is one scenario's measurements (JSON lines for MLAT-RESULTS.md).
type ResResult struct {
	Name, About               string
	Stations, Aircraft        int
	SimMin, EvalMin           float64
	Devices                   map[string]int
	PubH, PubV, PubHAll       pct     // published, inside radius (H, V), and everywhere (H)
	PubVLevel, PubVClimb      pct     // vertical, inside: level flight / |vertical rate| > 2.5 m/s
	PubCal1, PubCal2          float64 // fraction of horizontal errors within 1σ / 2σ (inside)
	PubPerMin                 float64
	AltShare                  float64 // share of published positions with the altitude row used
	FixH                      pct     // per-cluster solver fixes (inside)
	HoldH                     pct     // hold-out track estimates vs truth (inside)
	HoldLiveP50, HoldLiveP95  float64 // the engine's live hold-out statistic (vs ADS-B reference)
	AvailAircraft             [2]int  // Mode S-only inside, >= 20 s with a transmission heard by >= 4: [n, with fixes]
	AvailTime                 [2]int  // aircraft-seconds with a >= 4-station transmission: [n, with a fix within ±2 s]
	AvailAnyTime              [2]int  // Mode S-only aircraft-seconds inside the radius: [n, with a fix within ±2 s]
	FalseKm, FalseGross       float64 // share of published positions > 1 km off / > 5σ + 50 m off
	FalseKmN, FalseGrossN     int
	FalseKmIn, FalseGrossIn   float64 // the same, inside the network radius
	NoTruth                   int     // published positions without a truth (aircraft left the simulation)
	Ghost                     int     // published positions of addresses the simulation never had
	LatMeas                   pct     // transmission → publication, measurement updates (s)
	CoastShare                float64
	ClockRel                  map[string]pct // |relative clock error| ns per device
	ClockWithin3              float64        // share of |rel| <= 3σ
	NetUTC                    pct            // |network time − UTC| ns (anchored)
	Synced, Anchors           int
	ClockSteps                uint64
	EngineCores               float64 // ingest + engine wall per simulated second (one goroutine)
	EngineUsPerGroup          float64
	GroupsPerS, ClustersPerS  float64
	EpochMs                   float64
	EngineHeapMB, TotalHeapMB float64
	WallS                     float64
	StepDetectS, StepResyncS  float64
	StepBeyond3, StepEpochs   int
	StepFixesBeyond3          [2]int // published fixes in the 30 s after the step: [n, > 3σ]
	BaseFixesBeyond3          [2]int // the same, in the 30 s before
	DemotedAfterS             float64
	NetShiftNs                float64 // |mean common mode after demotion − before the fault|
	NetShiftMaxNs             float64 // largest |common mode − before| between fault and end
}

func (r *resSink) result(wall time.Duration) ResResult {
	h, sc := r.h, r.sc
	res := ResResult{Name: sc.name, About: sc.about, Stations: h.n.NumReceivers(), Aircraft: h.cfg.aircraft,
		SimMin: sc.dur.Minutes(), EvalMin: sc.dur.Minutes() - sc.hc.evalFromS/60, Devices: map[string]int{},
		WallS: wall.Seconds(), Ghost: r.ghost, NoTruth: r.noTruth, StepDetectS: -1, StepResyncS: -1, DemotedAfterS: -1}
	for _, d := range r.devs {
		res.Devices[d]++
	}
	var hIn, vIn, hAll, lat, fixIn, holdIn, vLevel, vClimb []float64
	in1, in2, alt3, coast, nPub, nIn, kmIn, grIn := 0, 0, 0, 0, 0, 0, 0, 0
	for _, p := range r.pubs {
		if p.holdout {
			if p.inside && !p.coastd {
				holdIn = append(holdIn, p.errH)
			}
			continue
		}
		nPub++
		hAll = append(hAll, p.errH)
		if p.errH > 1000 {
			res.FalseKmN++
		}
		if p.errH > 5*p.sigH+50 {
			res.FalseGrossN++
		}
		if p.alt3 {
			alt3++
		}
		if p.coastd {
			coast++
		} else {
			lat = append(lat, p.latS)
		}
		if !p.inside {
			continue
		}
		nIn++
		if p.errH > 1000 {
			kmIn++
		}
		if p.errH > 5*p.sigH+50 {
			grIn++
		}
		hIn = append(hIn, p.errH)
		vIn = append(vIn, p.errV)
		if p.climbing {
			vClimb = append(vClimb, p.errV)
		} else {
			vLevel = append(vLevel, p.errV)
		}
		if p.errH <= p.sigH {
			in1++
		}
		if p.errH <= 2*p.sigH {
			in2++
		}
	}
	for _, s := range h.snap {
		if s.inside {
			fixIn = append(fixIn, s.errH)
		}
	}
	res.PubH, res.PubV, res.PubHAll, res.FixH, res.HoldH, res.LatMeas = pcts(hIn), pcts(vIn), pcts(hAll), pcts(fixIn), pcts(holdIn), pcts(lat)
	res.PubVLevel, res.PubVClimb = pcts(vLevel), pcts(vClimb)
	if len(hIn) > 0 {
		res.PubCal1, res.PubCal2 = float64(in1)/float64(len(hIn)), float64(in2)/float64(len(hIn))
	}
	if nPub > 0 {
		res.FalseKm, res.FalseGross = float64(res.FalseKmN)/float64(nPub+r.noTruth), float64(res.FalseGrossN)/float64(nPub+r.noTruth)
		res.AltShare, res.CoastShare = float64(alt3)/float64(nPub), float64(coast)/float64(nPub)
		res.PubPerMin = float64(nPub) / res.EvalMin
	}
	if nIn > 0 {
		res.FalseKmIn, res.FalseGrossIn = float64(kmIn)/float64(nIn), float64(grIn)/float64(nIn)
	}
	q := h.e.QualityReport(60, false)
	res.HoldLiveP50, res.HoldLiveP95 = q.TrackTotal.P50, q.TrackTotal.P95
	// availability
	near := func(icao uint32, s int32) bool {
		ps := r.pubSec[icao]
		for d := int32(-2); d <= 2; d++ {
			if ps[s+d] {
				return true
			}
		}
		return false
	}
	for icao, secs := range r.acSec {
		for s := range secs {
			res.AvailAnyTime[0]++
			if near(icao, s) {
				res.AvailAnyTime[1]++
			}
			if r.heardSec[icao][s] {
				res.AvailTime[0]++
				if near(icao, s) {
					res.AvailTime[1]++
				}
			}
		}
		n4 := 0
		for s := range r.heardSec[icao] {
			if secs[s] {
				n4++
			}
		}
		if n4 >= 20 {
			res.AvailAircraft[0]++
			if len(r.pubSec[icao]) > 0 {
				res.AvailAircraft[1]++
			}
		}
	}
	// clocks
	byDev := map[string][]float64{}
	w3 := 0
	for _, c := range r.clk {
		byDev[c.dev] = append(byDev[c.dev], math.Abs(c.rel))
		if math.Abs(c.norm) <= 3 {
			w3++
		}
	}
	res.ClockRel = map[string]pct{}
	for d, v := range byDev {
		res.ClockRel[d] = pcts(v)
	}
	if len(r.clk) > 0 {
		res.ClockWithin3 = float64(w3) / float64(len(r.clk))
	}
	var nu []float64
	for _, v := range r.netUTC {
		nu = append(nu, math.Abs(v))
	}
	res.NetUTC = pcts(nu)
	for _, s := range h.e.Filter().Snapshot().Stations() {
		if s.Synced {
			res.Synced++
		}
		if s.Anchor {
			res.Anchors++
		}
	}
	st := h.e.Stats()
	res.ClockSteps = st.Clock.Steps
	simS := sc.dur.Seconds()
	res.EngineCores = h.engWall.Seconds() / simS
	res.EngineUsPerGroup = st.ProcUsPerGroup
	res.GroupsPerS, res.ClustersPerS = float64(st.Groups)/simS, float64(st.Clusters)/simS
	res.EpochMs = float64(st.Clock.MeanEpochNs) / 1e6
	// step / fault
	if sc.stepNs != 0 {
		if r.stepDetect >= 0 {
			res.StepDetectS = r.stepDetect - sc.stepAt
		}
		if r.stepResync >= 0 {
			res.StepResyncS = r.stepResync - sc.stepAt
		}
		res.StepBeyond3, res.StepEpochs = r.stepBeyond3, r.stepEp
		for _, p := range r.pubs {
			if p.holdout || p.coastd {
				continue
			}
			var b *[2]int
			switch {
			case p.tS >= sc.stepAt && p.tS < sc.stepAt+30:
				b = &res.StepFixesBeyond3
			case p.tS >= sc.stepAt-30 && p.tS < sc.stepAt:
				b = &res.BaseFixesBeyond3
			default:
				continue
			}
			b[0]++
			if p.errH > 3*p.sigH {
				b[1]++
			}
		}
	}
	if sc.faultNs != 0 {
		if r.demotedAt >= 0 {
			res.DemotedAfterS = r.demotedAt - sc.faultAt
		}
		var before, after []float64
		for _, x := range r.cmSeries {
			switch {
			case x[0] >= sc.faultAt-120 && x[0] < sc.faultAt:
				before = append(before, x[1])
			case r.demotedAt >= 0 && x[0] >= r.demotedAt+30 && x[0] < r.demotedAt+150:
				after = append(after, x[1])
			}
		}
		mean := func(v []float64) float64 {
			s := 0.0
			for _, x := range v {
				s += x
			}
			return s / float64(max(len(v), 1))
		}
		b := mean(before)
		if len(after) > 0 {
			res.NetShiftNs = math.Abs(mean(after) - b)
		}
		for _, x := range r.cmSeries {
			if x[0] >= sc.faultAt {
				res.NetShiftMaxNs = math.Max(res.NetShiftMaxNs, math.Abs(x[1]-b))
			}
		}
	}
	return res
}

func heapMB() float64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapAlloc) / (1 << 20)
}

func TestMLATResults(t *testing.T) {
	sel := os.Getenv("MLAT_RESULTS")
	if sel == "" {
		t.Skip("set MLAT_RESULTS=all or a comma-separated list of scenario names")
	}
	want := map[string]bool{}
	for _, s := range strings.Split(sel, ",") {
		want[strings.TrimSpace(s)] = true
	}
	var out *os.File
	if p := os.Getenv("MLAT_RESULTS_OUT"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		out = f
	}
	for _, sc := range resScenarios() {
		if !want["all"] && !want[sc.name] {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			start := time.Now()
			var h *harness
			if sc.specs != nil {
				h = newHarnessSpecs(t, sc.hc, sc.specs)
			} else {
				h = newHarness(t, sc.hc)
			}
			r := newResSink(h, &sc)
			h.n.AddSink(r)
			h.run(sc.dur)
			wall := time.Since(start)
			res := r.result(wall)
			total := heapMB()
			h.e, h.ing = nil, nil
			engine := total - heapMB()
			res.EngineHeapMB, res.TotalHeapMB = engine, total
			b, _ := json.Marshal(res)
			t.Logf("%s", b)
			t.Logf("%s", res.text())
			if out != nil {
				fmt.Fprintf(out, "%s\n", b)
			}
		})
	}
}

func (r ResResult) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d stations, %d aircraft, %.0f min (eval %.0f), wall %.0f s\n", r.Name, r.Stations, r.Aircraft, r.SimMin, r.EvalMin, r.WallS)
	fmt.Fprintf(&b, "  published inside: H P50 %.1f P95 %.1f P99 %.1f m (n %d) | V P50 %.1f P95 %.1f P99 %.1f | all H P50 %.1f P95 %.1f P99 %.1f | 1σ %.1f %% 2σ %.1f %% | alt row %.1f %% | %.0f/min\n",
		r.PubH.P50, r.PubH.P95, r.PubH.P99, r.PubH.N, r.PubV.P50, r.PubV.P95, r.PubV.P99, r.PubHAll.P50, r.PubHAll.P95, r.PubHAll.P99, 100*r.PubCal1, 100*r.PubCal2, 100*r.AltShare, r.PubPerMin)
	fmt.Fprintf(&b, "  vertical inside: level P50 %.1f P95 %.1f P99 %.1f (n %d); climbing/descending P50 %.1f P95 %.1f P99 %.1f (n %d)\n",
		r.PubVLevel.P50, r.PubVLevel.P95, r.PubVLevel.P99, r.PubVLevel.N, r.PubVClimb.P50, r.PubVClimb.P95, r.PubVClimb.P99, r.PubVClimb.N)
	fmt.Fprintf(&b, "  solver fixes inside H P50 %.1f P95 %.1f P99 %.1f (n %d); hold-out tracks vs truth P50 %.1f P95 %.1f (n %d); live hold-out (vs ADS-B) P50 %.1f P95 %.1f\n",
		r.FixH.P50, r.FixH.P95, r.FixH.P99, r.FixH.N, r.HoldH.P50, r.HoldH.P95, r.HoldH.N, r.HoldLiveP50, r.HoldLiveP95)
	fmt.Fprintf(&b, "  availability: aircraft %d/%d; >=4-station seconds %d/%d; all Mode S-only seconds inside %d/%d\n",
		r.AvailAircraft[1], r.AvailAircraft[0], r.AvailTime[1], r.AvailTime[0], r.AvailAnyTime[1], r.AvailAnyTime[0])
	fmt.Fprintf(&b, "  false (all): >1 km %d (%.3f %%), >5σ+50 m %d (%.3f %%); inside: %.3f %% / %.3f %%; no truth %d, ghost %d | latency P50 %.3f P95 %.3f P99 %.3f s (coast share %.1f %%)\n",
		r.FalseKmN, 100*r.FalseKm, r.FalseGrossN, 100*r.FalseGross, 100*r.FalseKmIn, 100*r.FalseGrossIn, r.NoTruth, r.Ghost, r.LatMeas.P50, r.LatMeas.P95, r.LatMeas.P99, 100*r.CoastShare)
	devs := make([]string, 0, len(r.ClockRel))
	for d := range r.ClockRel {
		devs = append(devs, d)
	}
	slices.Sort(devs)
	for _, d := range devs {
		p := r.ClockRel[d]
		fmt.Fprintf(&b, "  clock %-14s |rel| P50 %.1f P95 %.1f P99 %.1f ns (n %d)\n", d, p.P50, p.P95, p.P99, p.N)
	}
	fmt.Fprintf(&b, "  clock within 3σ %.1f %%, |net−UTC| P50 %.1f P95 %.1f ns, synced %d anchors %d steps %d\n", 100*r.ClockWithin3, r.NetUTC.P50, r.NetUTC.P95, r.Synced, r.Anchors, r.ClockSteps)
	fmt.Fprintf(&b, "  cpu: ingest+engine %.3f cores, engine %.1f µs/group, %.0f groups/s, %.0f clusters/s, epoch %.1f ms; heap engine+ingest %.0f MB of %.0f MB\n",
		r.EngineCores, r.EngineUsPerGroup, r.GroupsPerS, r.ClustersPerS, r.EpochMs, r.EngineHeapMB, r.TotalHeapMB)
	if r.StepEpochs > 0 || r.StepDetectS >= 0 {
		fmt.Fprintf(&b, "  step: detected +%.1f s, re-synced +%.1f s, synced epochs beyond 3σ %d/%d; fixes > 3σ after %d/%d, before %d/%d\n",
			r.StepDetectS, r.StepResyncS, r.StepBeyond3, r.StepEpochs, r.StepFixesBeyond3[1], r.StepFixesBeyond3[0], r.BaseFixesBeyond3[1], r.BaseFixesBeyond3[0])
	}
	if r.DemotedAfterS != -1 || r.NetShiftMaxNs > 0 {
		fmt.Fprintf(&b, "  anchor fault: demoted +%.1f s, network-time shift %.1f ns (max %.1f ns)\n", r.DemotedAfterS, r.NetShiftNs, r.NetShiftMaxNs)
	}
	return b.String()
}
