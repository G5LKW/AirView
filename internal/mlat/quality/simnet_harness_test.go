package quality

import (
	"context"
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/solve"
	"airview/internal/sim/simnet"
)

// The quality simnet harness: simnet supplies the stations, the aircraft
// and every reception (true arrival time with multipath, outliers and the
// receiver's white jitter); the harness adds the timestamp quantisation of
// the device and a clock-synchronisation error per station (re-drawn every
// syncPeriodS with the class σ), so each row is what a synchronised network
// would hand the solver. The truth (aircraft position, reported ADS-B
// position, airframe latency) stays available to score against. The clock
// filter itself is validated in internal/mlat/clock; the features here
// (k_atm, spoof detection, latency calibration, CRLB maps) consume its
// output, so an idealised but statistically honest sync is the right input.

// hRow is one reception of a transmission.
type hRow struct {
	rx     int
	pos    geo.Vec3
	toaS   float64 // measured TOA, s since simnet start
	sigmaS float64 // the row's 1σ as the engine would state it
	gross  bool    // simnet gross outlier
}

// hTx is one transmission with its receptions.
type hTx struct {
	seq   uint64
	icao  uint32
	txS   float64
	pos   geo.Vec3 // true ECEF
	vel   geo.Vec3 // true ECEF velocity
	lat   float64
	lon   float64
	hae   float64
	nacp  uint8
	pos17 bool    // airborne position squitter re-encoded by simnet
	repLa float64 // reported (decoded) position
	repLo float64
	latS  float64 // airframe position latency (truth)
	rows  []hRow
}

type hSync struct {
	periodStart float64
	off         float64
}

type harnessQ struct {
	n       *simnet.Network
	rxPos   []geo.Vec3
	rxHAE   []float64
	rxSig   []float64 // jitter ⊕ quantisation, ns
	syncSig []float64 // sync error σ, ns
	sync    []hSync
	rng     *rand.Rand
	pend    map[uint64]*hTx
	keys    []uint64
	onTx    func(*hTx)
	minRows int
}

const syncPeriodS = 30.0

// deviceTiming returns the quantisation step (ns) and the sync σ (ns) the
// harness gives a device.
func deviceTiming(d simnet.Device) (stepNs, syncNs float64) {
	switch d {
	case simnet.DeviceRadarcapeGPS:
		return 1, 5
	case simnet.DeviceRadarcape12, simnet.DeviceBeast:
		return 1e9 / 12e6, 15
	case simnet.DeviceAirspy:
		return 1e9 / 12e6, 30
	case simnet.DeviceSBS:
		return 50, 60
	}
	return 1e9 / 2.4e6, 60 // SDR
}

type hOpts struct {
	seed      uint64
	receivers int
	radiusKm  float64
	aircraft  int
	mix       map[simnet.Device]float64
	kAtm      float64
	posLatMs  float64 // 0 = simnet default (300 ms), < 0 = none
	minRows   int     // smallest reception count handed to onTx (default 2)
}

func newHarnessQ(tb testing.TB, o hOpts, onTx func(*hTx)) *harnessQ {
	tb.Helper()
	if o.radiusKm == 0 {
		o.radiusKm = 250
	}
	specs := simnet.RandomReceivers(o.seed, o.receivers, simnet.Placement{CenterLat: 50.5, CenterLon: 9, RadiusKm: o.radiusKm, Mix: o.mix})
	cfg := simnet.Config{Seed: o.seed, Aircraft: o.aircraft, Receivers: specs, PosLatencyMaxMs: o.posLatMs,
		Atmosphere: simnet.Atmosphere{KAtm: o.kAtm}, ModeSOnlyFraction: -1}
	n, err := simnet.New(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	h := &harnessQ{n: n, rng: rand.New(rand.NewPCG(o.seed, 77)), pend: map[uint64]*hTx{}, onTx: onTx, minRows: max(o.minRows, 2)}
	for i, r := range n.Receivers() {
		h.rxPos = append(h.rxPos, geo.Vec3{X: r.Pos[0], Y: r.Pos[1], Z: r.Pos[2]})
		h.rxHAE = append(h.rxHAE, r.HAE)
		step, syn := deviceTiming(r.Device)
		j := specs[i].Clock.JitterNs
		h.rxSig = append(h.rxSig, math.Sqrt(j*j+step*step/12))
		h.syncSig = append(h.syncSig, syn)
		h.sync = append(h.sync, hSync{periodStart: -1})
	}
	n.AddSink(h)
	return h
}

func (h *harnessQ) syncOff(rx int, t float64) float64 {
	s := &h.sync[rx]
	p := math.Floor(t/syncPeriodS) * syncPeriodS
	if s.periodStart != p {
		s.periodStart = p
		s.off = h.rng.NormFloat64() * h.syncSig[rx]
	}
	return s.off
}

// Observe implements simnet.Sink.
func (h *harnessQ) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		t := h.pend[o.Seq]
		if t == nil {
			tx := o.Tx
			t = &hTx{seq: o.Seq, icao: o.ICAO, txS: tx.TxS,
				pos: geo.Vec3{X: tx.Pos[0], Y: tx.Pos[1], Z: tx.Pos[2]},
				vel: geo.Vec3{X: tx.Vel[0], Y: tx.Vel[1], Z: tx.Vel[2]},
				lat: tx.Lat, lon: tx.Lon, hae: tx.HAE, nacp: tx.NACp,
				pos17: tx.Position, repLa: tx.ReportedLat, repLo: tx.ReportedLon, latS: tx.PosLatencyS}
			h.pend[o.Seq] = t
			h.keys = append(h.keys, o.Seq)
		}
		step, _ := deviceTiming(h.n.Receiver(o.Rx).Device)
		q := (h.rng.Float64() - 0.5) * step
		errNs := o.MultipathNs + o.OutlierNs + o.NoiseNs + q + h.syncOff(o.Rx, o.ArrivalS)
		sig := math.Sqrt(h.rxSig[o.Rx]*h.rxSig[o.Rx] + h.syncSig[o.Rx]*h.syncSig[o.Rx] + 10*10)
		t.rows = append(t.rows, hRow{rx: o.Rx, pos: h.rxPos[o.Rx], toaS: o.ArrivalS + errNs*1e-9, sigmaS: sig * 1e-9, gross: o.OutlierNs != 0})
	}
}

// Advance implements simnet.Sink: transmissions older than the horizon
// minus 50 ms are complete.
func (h *harnessQ) Advance(horizonS float64) {
	sort.Slice(h.keys, func(i, j int) bool { return h.keys[i] < h.keys[j] })
	keep := h.keys[:0]
	for _, k := range h.keys {
		t := h.pend[k]
		if t.txS < horizonS-0.05 {
			delete(h.pend, k)
			if len(t.rows) >= h.minRows {
				sort.Slice(t.rows, func(i, j int) bool { return t.rows[i].rx < t.rows[j].rx })
				h.onTx(t)
			}
			continue
		}
		keep = append(keep, k)
	}
	h.keys = keep
}

func (h *harnessQ) run(tb testing.TB, d time.Duration) {
	tb.Helper()
	if err := h.n.Advance(context.Background(), d); err != nil {
		tb.Fatal(err)
	}
}

// unixNs converts simnet seconds to Unix ns.
func (h *harnessQ) unixNs(s float64) int64 { return h.n.UnixNs(s) }

// solveTx solves a transmission as a Mode S-only snapshot fix (altitude row
// from the true height with altSigma noise when altSigma > 0) using the
// given solver. Rows are capped at solve.MaxStations.
func solveTx(sv *solve.Solver, t *hTx, rng *rand.Rand, altSigma float64, f *solve.Fix) (epoch float64, ok bool) {
	var pr solve.Problem
	epoch = math.Floor(t.txS)
	var st [solve.MaxStations]solve.Station
	m := min(len(t.rows), solve.MaxStations)
	for i := 0; i < m; i++ {
		r := &t.rows[i]
		st[i] = solve.Station{Pos: r.pos, TOA: r.toaS - epoch, Sigma: r.sigmaS, ID: int32(r.rx)}
	}
	pr.Stations = st[:m]
	pr.EpochNs = int64(epoch * 1e9)
	if altSigma > 0 {
		pr.Alt = solve.Altitude{Valid: true, HAE: t.hae + rng.NormFloat64()*altSigma, Sigma: altSigma}
	}
	if err := sv.Solve(&pr, f); err != nil || !f.OK() {
		return epoch, false
	}
	return epoch, true
}

// enuErr returns the horizontal error (east, north) of p against the
// reference ref in the ENU frame at ref.
func enuErr(p, ref geo.Vec3) (e, n, u float64) {
	fr := geo.NewFrameECEF(ref)
	d := fr.ToENU(p)
	return d.X, d.Y, d.Z
}

// velENU returns the horizontal velocity in the ENU frame at ref.
func velENU(v, ref geo.Vec3) (ve, vn float64) {
	fr := geo.NewFrameECEF(ref)
	d := fr.Rotate(v)
	return d.X, d.Y
}

// simExcess returns simnet's refraction excess (m) from receiver rx to the
// ECEF point tx under atmosphere a (simnet's own model: the truth).
func (h *harnessQ) simExcess(a simnet.Atmosphere, rx int, tx geo.Vec3) float64 {
	p := h.rxPos[rx]
	r := simnet.Vec3{p.X, p.Y, p.Z}
	t := simnet.Vec3{tx.X, tx.Y, tx.Z}
	return a.ExcessPathM(r, t, p.Norm()-h.rxHAE[rx], tx.Dist(p))
}
