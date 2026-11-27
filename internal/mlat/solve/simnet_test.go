package solve

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// Simnet-driven accuracy test: the multi-receiver simulator (its own
// propagation model: WGS-84 slant range plus a chord-integrated refraction
// excess, multipath, outliers, quantisation, receiver jitter) produces the
// arrival times; the clock filter is emulated as perfect knowledge of each
// receiver's offset plus a residual error of σ_clk per station and
// cluster. The solver sees only station positions, network-time TOAs, their
// σ, and optionally an altitude row (truth HAE + 20 m noise, the level of
// the GNSS−baro field). Errors are measured against simnet's transmit
// position.

type simObs struct {
	rx  int
	toa float64
}

type simTx struct {
	pos  geo.Vec3
	hae  float64
	txS  float64
	obs  []simObs
	icao uint32
}

type collector struct {
	n     *simnet.Network
	clkNs float64
	rng   *rand.Rand
	txs   map[uint64]*simTx
}

func (c *collector) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		tx := c.txs[o.Seq]
		if tx == nil {
			p := o.Tx.Pos
			tx = &simTx{pos: geo.Vec3{X: p[0], Y: p[1], Z: p[2]}, hae: o.Tx.HAE, txS: o.Tx.TxS, icao: o.ICAO}
			c.txs[o.Seq] = tx
		}
		hz := o.Class.NominalHz()
		toa := o.ArrivalS + (o.MultipathNs+o.OutlierNs)*1e-9 + c.n.TickError(o)/hz + c.clkNs*1e-9*c.rng.NormFloat64()
		tx.obs = append(tx.obs, simObs{rx: o.Rx, toa: toa})
	}
}

func (c *collector) Advance(float64) {}

type simCase struct {
	name     string
	devs     map[simnet.Device]int
	radiusKm float64
	clkNs    map[simnet.Device]float64
	alt      bool
	p50, p95 float64 // gates (0 = report only)
	minCal   float64
	maxCal   float64
}

func runSimnet(t *testing.T, cs simCase, seed uint64, dur time.Duration) {
	var specs []simnet.ReceiverSpec
	total := 0
	for _, n := range cs.devs {
		total += n
	}
	// one device kind per receiver, positions from RandomReceivers
	base := simnet.RandomReceivers(seed, total, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: cs.radiusKm})
	rr := rand.New(rand.NewPCG(seed, 77))
	k := 0
	for _, d := range []simnet.Device{simnet.DeviceRadarcapeGPS, simnet.DeviceBeast, simnet.DeviceSDR, simnet.DeviceAirspy} {
		for j := 0; j < cs.devs[d]; j++ {
			s := base[k]
			s.Clock = simnet.DefaultClockSpec(d, rr)
			s.Clock.ShortLongErrorNs = 0 // β_short is the clock filter's job
			s.Reception = simnet.DefaultReception(d, rr)
			specs = append(specs, s)
			k++
		}
	}
	n, err := simnet.New(simnet.Config{Seed: seed, Aircraft: 250, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	col := &collector{n: n, rng: rand.New(rand.NewPCG(seed, 3)), txs: map[uint64]*simTx{}}
	// per-receiver σ: jitter, quantisation and the clock residual
	cfg := n.Config()
	sig := make([]float64, len(specs))
	pos := make([]geo.Vec3, len(specs))
	clk := make([]float64, len(specs))
	for i, rt := range n.Receivers() {
		pos[i] = geo.Vec3{X: rt.Pos[0], Y: rt.Pos[1], Z: rt.Pos[2]}
		cl := cfg.Receivers[i].Clock
		q := 1.0 // ns
		switch rt.Device {
		case simnet.DeviceSDR:
			q = 1e9 / 2.4e6
		case simnet.DeviceBeast, simnet.DeviceRadarcape12, simnet.DeviceAirspy:
			q = 1e9 / 12e6
		}
		clk[i] = cs.clkNs[rt.Device]
		sig[i] = math.Sqrt(cl.JitterNs*cl.JitterNs+q*q/12+clk[i]*clk[i]) * 1e-9
	}
	// the collector draws one clock residual per observation with the
	// station's σ_clk
	n.AddSink(&perRxClock{col: col, clk: clk})
	if err := n.Advance(context.Background(), dur); err != nil {
		t.Fatal(err)
	}

	s := NewSolver(Options{})
	ar := rand.New(rand.NewPCG(seed, 9))
	var errs []float64
	var st []Station
	in1, solved, accepted, clusters, rejected, gross := 0, 0, 0, 0, 0, 0
	amb, ambHit, covIn1 := 0, 0, 0
	var cov []float64
	centre := geo.ToECEF(51, 9, 0)
	var grades [5]int
	for _, tx := range col.txs {
		need := 4
		if cs.alt {
			need = 3
		}
		if len(tx.obs) < need || len(tx.obs) > MaxStations {
			continue
		}
		clusters++
		st = st[:0]
		for _, o := range tx.obs {
			st = append(st, Station{Pos: pos[o.rx], TOA: o.toa, Sigma: sig[o.rx]})
		}
		pr := Problem{Stations: st}
		if cs.alt {
			pr.Alt = Altitude{Valid: true, HAE: tx.hae + 20*ar.NormFloat64(), Sigma: 20}
		}
		var f Fix
		if err := s.Solve(&pr, &f); err != nil {
			continue
		}
		solved++
		if !f.OK() {
			rejected++
			continue
		}
		accepted++
		grades[f.Grade]++
		e := enuErr(f.ECEF, tx.pos)
		h := math.Hypot(e.X, e.Y)
		if f.Flags&FlagAmbiguous != 0 {
			// the tracker resolves the branch (TestThreePlusAltBranches);
			// count whether the truth is among the hypotheses
			amb++
			hit := h < 5*f.SigmaH+200
			for j := 0; j < f.NAlt; j++ {
				hit = hit || f.Alt[j].ECEF.Dist(tx.pos) < 5*f.Alt[j].SigmaH+200
			}
			if hit {
				ambHit++
			}
			continue
		}
		if model.Displayable(f.Grade) && centre.Dist(tx.pos) < cs.radiusKm*1000 {
			cov = append(cov, h)
			if h <= f.SigmaH {
				covIn1++
			}
		}
		errs = append(errs, h)
		if h <= f.SigmaH {
			in1++
		}
		if h > 5*f.SigmaH+50 {
			gross++
			if gross <= 8 && testing.Verbose() {
				t.Logf("gross: err %.0f m σh %.0f n %d dof %d init %v flags %b chi2 %.2f nalt %d hae %.0f altres %.0f", h, f.SigmaH, f.NStations, f.DOF, f.Init, f.Flags, f.Chi2, f.NAlt, tx.hae, f.AltResid)
			}
		}
	}
	p50, p95 := percentile(cov, 0.5), percentile(cov, 0.95)
	cal := float64(covIn1) / math.Max(1, float64(len(cov)))
	t.Logf("%s: inside the network radius, displayable grades: %d fixes, P50 %.1f m P95 %.1f m, 1σ calibration %.1f %%; ambiguous %d (truth among hypotheses %d)",
		cs.name, len(cov), p50, p95, 100*cal, amb, ambHit)
	t.Logf("%s: %d clusters, %d solved, %d accepted (%d rejected); grades A %d B %d C %d D %d; all unambiguous: horizontal P50 %.1f m P95 %.1f m P99 %.1f m; 1σ calibration %.1f %%; gross (> 5σ) %d",
		cs.name, clusters, solved, accepted, rejected, grades[1], grades[2], grades[3], grades[4], percentile(errs, 0.5), percentile(errs, 0.95), percentile(errs, 0.99), 100*float64(in1)/math.Max(1, float64(len(errs))), gross)
	if amb > 0 && float64(ambHit) < 0.95*float64(amb) {
		t.Errorf("ambiguous fixes: truth among the hypotheses only %d/%d", ambHit, amb)
	}
	if accepted < clusters/2 {
		t.Errorf("only %d of %d clusters accepted", accepted, clusters)
	}
	if cs.p50 > 0 && p50 > cs.p50 {
		t.Errorf("P50 %.1f m > %.0f m", p50, cs.p50)
	}
	if cs.p95 > 0 && p95 > cs.p95 {
		t.Errorf("P95 %.1f m > %.0f m", p95, cs.p95)
	}
	if cs.minCal > 0 && (cal < cs.minCal || cal > cs.maxCal) {
		t.Errorf("1σ calibration %.1f %% outside %.0f-%.0f %%", 100*cal, 100*cs.minCal, 100*cs.maxCal)
	}
}

// perRxClock forwards to the collector with the right σ_clk per receiver.
type perRxClock struct {
	col *collector
	clk []float64
}

func (p *perRxClock) Observe(obs []simnet.Observation) {
	for i := range obs {
		p.col.clkNs = p.clk[obs[i].Rx]
		p.col.Observe(obs[i : i+1])
	}
}

func (p *perRxClock) Advance(h float64) {}

var simDur = 20 * time.Second

func TestSimnetAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	cases := []simCase{
		// DESIGN §13.2 row 1 at solver level (clock residual 10 ns):
		// P50 < 20 m, P95 < 60 m, 1σ calibration 60-75 % (a little
		// slack for the heavy-tailed multipath)
		{name: "8 GPS, 150 km", devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 8}, radiusKm: 150,
			clkNs: map[simnet.Device]float64{simnet.DeviceRadarcapeGPS: 10}, alt: false, p50: 20, p95: 60, minCal: 0.58, maxCal: 0.80},
		{name: "8 GPS, 150 km, altitude row", devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 8}, radiusKm: 150,
			clkNs: map[simnet.Device]float64{simnet.DeviceRadarcapeGPS: 10}, alt: true, p50: 20, p95: 60, minCal: 0.58, maxCal: 0.80},
		// row 2 / 3 orientation (clock residuals 20 / 150 ns)
		{name: "4 GPS + 20 SDR", devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 4, simnet.DeviceSDR: 20}, radiusKm: 150,
			clkNs: map[simnet.Device]float64{simnet.DeviceRadarcapeGPS: 20, simnet.DeviceSDR: 150}, alt: true, p50: 80, p95: 250},
		{name: "20 SDR", devs: map[simnet.Device]int{simnet.DeviceSDR: 20}, radiusKm: 150,
			clkNs: map[simnet.Device]float64{simnet.DeviceSDR: 150}, alt: true, p50: 200},
	}
	for i, cs := range cases {
		t.Run(cs.name, func(t *testing.T) { runSimnet(t, cs, uint64(100+i), simDur) })
	}
}
