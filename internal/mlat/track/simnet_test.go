package track

import (
	"context"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/sim/simnet"
)

// simnet-driven truth tests. simnet supplies the world (aircraft motion,
// who hears which transmission, propagation with its own refraction
// model, multipath, outliers, receiver jitter and quantisation). The clock
// filter (mlat/clock) is not part of this package, so network-time TOAs are
// built from the truth arrival instant plus the receiver's actual tick
// error (TickError: jitter + quantisation) plus a modelled clock-sync
// residual (AR(1), 20 s correlation, σ by device class) — what a working
// clock filter leaves. The frame reference point and β_short are assumed
// calibrated by the clock filter.

// syncSigmaNs is the modelled residual of the clock filter per device.
func syncSigmaNs(d simnet.Device) float64 {
	switch d {
	case simnet.DeviceRadarcapeGPS:
		return 30 // 1PPS discipline jitter of the anchors
	case simnet.DeviceBeast, simnet.DeviceRadarcape12:
		return 50
	case simnet.DeviceAirspy:
		return 80
	}
	return 120
}

func sampleRate(c simnet.ClockSpec) float64 {
	if c.SampleRateHz > 0 {
		return c.SampleRateHz
	}
	switch c.Device {
	case simnet.DeviceRadarcapeGPS:
		return 1e9
	case simnet.DeviceSDR:
		return 2.4e6
	case simnet.DeviceAirspy, simnet.DeviceSBS:
		return 20e6
	}
	return 12e6
}

type simGroup struct {
	txS  float64
	icao uint32
	pos  geo.Vec3
	hae  float64
	alt  bool
	toas []TOA
	rx   []int
}

// simCollector groups observations per transmission and hands complete
// groups (in transmit order) to fn.
type simCollector struct {
	n      *simnet.Network
	rng    *rand.Rand
	sigma  []float64 // per rx white TOA σ (jitter, quantisation), s
	syncS  []float64 // per rx sync residual σ, s
	syncV  []float64
	syncT  []float64
	pos    []geo.Vec3
	open   map[uint64]*simGroup
	fn     func(g *simGroup)
	base   float64
	groups int
	// ownExcess swaps simnet's refraction excess for geo's (isolates the
	// tracker from the propagation-model mismatch).
	ownExcess bool
	atm       geo.Atmosphere
}

func newSimCollector(n *simnet.Network, fn func(*simGroup)) *simCollector {
	c := &simCollector{n: n, rng: rand.New(rand.NewPCG(11, 12)), open: map[uint64]*simGroup{}, fn: fn}
	cfg := n.Config()
	for i, r := range cfg.Receivers {
		q := 1 / sampleRate(r.Clock)
		ss := syncSigmaNs(r.Clock.Device) * 1e-9
		jit := r.Clock.JitterNs * 1e-9
		c.sigma = append(c.sigma, math.Sqrt(jit*jit+q*q/12))
		c.syncS = append(c.syncS, ss)
		c.syncV = append(c.syncV, c.rng.NormFloat64()*ss)
		c.syncT = append(c.syncT, 0)
		rt := n.Receiver(i)
		c.pos = append(c.pos, geo.Vec3{X: rt.Pos[0], Y: rt.Pos[1], Z: rt.Pos[2]})
	}
	return c
}

func (c *simCollector) syncErr(rx int, t float64) float64 {
	a := math.Exp(-math.Max(t-c.syncT[rx], 0) / 20)
	c.syncV[rx] = a*c.syncV[rx] + math.Sqrt(1-a*a)*c.syncS[rx]*c.rng.NormFloat64()
	c.syncT[rx] = t
	return c.syncV[rx]
}

func altBearing(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	switch df := data[0] >> 3; df {
	case 0, 4, 16, 20:
		return true
	case 17, 18:
		if len(data) >= 5 {
			tc := data[4] >> 3
			return tc >= 9 && tc <= 18
		}
	}
	return false
}

func (c *simCollector) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		g := c.open[o.Seq]
		if g == nil {
			tx := o.Tx
			g = &simGroup{txS: tx.TxS, icao: o.ICAO, pos: geo.Vec3{X: tx.Pos[0], Y: tx.Pos[1], Z: tx.Pos[2]},
				hae: tx.HAE, alt: altBearing(o.Data)}
			c.open[o.Seq] = g
		}
		hz := o.Class.NominalHz()
		tick := 0.0
		if hz > 0 {
			tick = c.n.TickError(o) / hz
		}
		toa := o.ArrivalS + (o.MultipathNs+o.OutlierNs)*1e-9 + tick + c.syncErr(o.Rx, o.ArrivalS)
		if c.ownExcess {
			// replace simnet's chord refraction model by the tracker's
			toa += (c.atm.Excess(c.pos[o.Rx], g.pos) - o.ExcessM) / geo.C
		}
		g.toas = append(g.toas, TOA{Station: uint32(o.Rx), Pos: c.pos[o.Rx], T: toa, Sigma: c.sigma[o.Rx], SyncSigma: c.syncS[o.Rx]})
		g.rx = append(g.rx, o.Rx)
	}
}

func (c *simCollector) Advance(h float64) {
	var done []*simGroup
	for k, g := range c.open {
		if g.txS < h-0.05 {
			done = append(done, g)
			delete(c.open, k)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].txS < done[j].txS })
	for _, g := range done {
		c.groups++
		c.fn(g)
	}
}

// simEngine emulates the engine's routing around the tracker.
type simEngine struct {
	tr      *Tracker
	lastAlt map[uint32][2]float64 // ICAO -> (time, HAE) of the last altitude reply
	n       *simnet.Network
	atm     geo.Atmosphere
	rng     *rand.Rand
	altSig  float64
	// restrict, when set, filters the members of a group (2-station tests).
	restrict  func(g *simGroup) (keep []int, ok bool)
	onEst     func(g *simGroup, e *Estimate, r Result)
	fixes     int
	solveErr  int
	ownExcess bool
	// velEveryS > 0 feeds a ground-velocity measurement (truth + 5 m/s
	// noise, like BDS 5,0 track/ground speed) at this interval.
	velEveryS float64
	lastVel   map[uint32]float64
}

func (s *simEngine) handle(g *simGroup) {
	toas, rxs := g.toas, g.rx
	if s.restrict != nil {
		keep, ok := s.restrict(g)
		if ok {
			var nt []TOA
			for _, j := range keep {
				nt = append(nt, toas[j])
			}
			toas = nt
		}
	}
	_ = rxs
	if len(toas) < 2 {
		return
	}
	// cluster times relative to a whole second
	base := math.Floor(g.txS)
	c := Cluster{ICAO: g.icao, T0Ns: s.n.UnixNs(base)}
	for _, m := range toas {
		m.T -= base
		c.TOAs = append(c.TOAs, m)
	}
	var est Estimate
	if s.velEveryS > 0 {
		if s.lastVel == nil {
			s.lastVel = map[uint32]float64{}
		}
		if lv, ok := s.lastVel[g.icao]; !ok || g.txS-lv >= s.velEveryS {
			if at, ok := s.n.AircraftAt(g.icao, g.txS); ok {
				s.lastVel[g.icao] = g.txS
				lf := geo.NewFrameECEF(g.pos)
				v := lf.Rotate(geo.Vec3{X: at.Vel[0], Y: at.Vel[1], Z: at.Vel[2]})
				vm := Velocity{ICAO: g.icao, TNs: s.n.UnixNs(g.txS), East: v.X + 5*s.rng.NormFloat64(), North: v.Y + 5*s.rng.NormFloat64(), SigmaH: 5}
				s.tr.Velocity(&vm, nil)
			}
		}
	}
	if g.alt {
		c.AltHAE, c.AltSigma = g.hae+s.rng.NormFloat64()*s.altSig, s.altSig
		if s.lastAlt == nil {
			s.lastAlt = map[uint32][2]float64{}
		}
		s.lastAlt[g.icao] = [2]float64{g.txS, c.AltHAE}
	} else if la, ok := s.lastAlt[g.icao]; ok && g.txS-la[0] < 10 {
		// the engine attaches the latest Mode S altitude, inflated by its
		// age times the tracked vertical rate (§9.4)
		vz := 15.0
		if s.tr.Get(g.icao, &est) {
			vz = math.Abs(est.VelENU[2]) + 2
		}
		age := g.txS - la[0]
		c.AltHAE, c.AltSigma = la[1], math.Hypot(s.altSig, age*vz)
	}
	confirmed := s.tr.Get(g.icao, &est) && est.Status == StatusConfirmed
	var r Result
	if confirmed {
		r = s.tr.Cluster(&c, &est)
	} else {
		rows := len(c.TOAs)
		a := -1e9
		if c.AltSigma > 0 {
			rows++
			a = c.AltHAE
		}
		if rows < 4 {
			return
		}
		sols := solveTOA(c.TOAs, a, s.altSig, &s.atm)
		if len(sols) == 0 {
			s.solveErr++
			return
		}
		s.fixes++
		f := fixFrom(g.icao, s.n.UnixNs(g.txS), sols, len(c.TOAs))
		r = s.tr.Fix(&f, &est)
	}
	if s.onEst != nil {
		s.onEst(g, &est, r)
	}
}

func runSimnet(t *testing.T, specs []simnet.ReceiverSpec, aircraft int, dur time.Duration, eng *simEngine) *simnet.Network {
	t.Helper()
	n, err := simnet.New(simnet.Config{Seed: 5, Aircraft: aircraft, Receivers: specs, RangeKm: 300})
	if err != nil {
		t.Fatal(err)
	}
	eng.n = n
	// Experiment knobs (unset = defaults): TRACK_TAU, TRACK_QCV, TRACK_SCV and
	// TRACK_QW override SyncTauS, QCV, SojournCVS and QOmega; TRACK_OWNEXCESS
	// swaps simnet's refraction model for the tracker's.
	tau, _ := strconv.ParseFloat(os.Getenv("TRACK_TAU"), 64)
	qcv, _ := strconv.ParseFloat(os.Getenv("TRACK_QCV"), 64)
	scv, _ := strconv.ParseFloat(os.Getenv("TRACK_SCV"), 64)
	qw, _ := strconv.ParseFloat(os.Getenv("TRACK_QW"), 64)
	eng.tr = New(Params{SyncTauS: tau, QCV: qcv, SojournCVS: scv, QOmega: qw})
	eng.atm = geo.DefaultAtmosphere()
	eng.rng = rand.New(rand.NewPCG(5, 6))
	if eng.altSig == 0 {
		eng.altSig = 25
	}
	col := newSimCollector(n, eng.handle)
	col.ownExcess, col.atm = eng.ownExcess, eng.atm
	n.AddSink(col)
	if err := n.Advance(context.Background(), dur); err != nil {
		t.Fatal(err)
	}
	return n
}

func simReceivers(dev simnet.Device, k int, radiusKm float64) []simnet.ReceiverSpec {
	return simnet.RandomReceivers(3, k, simnet.Placement{CenterLat: simLat, CenterLon: simLon, RadiusKm: radiusKm,
		Mix: map[simnet.Device]float64{dev: 1}})
}

// TestSimnetTrackAccuracy runs the tracker on simnet networks and compares
// every confirmed estimate after an accepted update with the truth
// position of that transmission.
func TestSimnetTrackAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	type netCase struct {
		name   string
		dev    simnet.Device
		k      int
		radius float64
		p50    float64 // gates (generous: the tracker alone, not §13.2 end to end)
		p95    float64
	}
	for _, nc := range []netCase{
		{"8 GPS anchors, 150 km", simnet.DeviceRadarcapeGPS, 8, 150, 30, 120},
		{"20 SDR, 150 km", simnet.DeviceSDR, 20, 150, 120, 450},
	} {
		t.Run(nc.name, func(t *testing.T) {
			var es, out errStats // inside / outside the station footprint
			gated, applied := 0, 0
			eng := &simEngine{ownExcess: os.Getenv("TRACK_OWNEXCESS") != ""}
			cen := geo.ToECEF(simLat, simLon, 0)
			eng.onEst = func(g *simGroup, e *Estimate, r Result) {
				switch r {
				case ResApplied:
					applied++
				case ResGated:
					gated++
				}
				if r == ResApplied && e.Status == StatusConfirmed && e.Updates > 10 {
					if horizDist(cen, g.pos) <= nc.radius*1e3 {
						es.add(e, g.pos)
					} else {
						out.add(e, g.pos)
					}
				}
			}
			runSimnet(t, simReceivers(nc.dev, nc.k, nc.radius), 120, 4*time.Minute, eng)
			st := eng.tr.Stats()
			t.Logf("%s, inside the footprint: n=%d P50 %.1f m P95 %.1f m P99 %.1f m, median σh %.1f; NEES within 1σ %.2f (0.68) 2σ %.2f (0.95) 99.9%% %.3f",
				nc.name, len(es.errs), pct(es.errs, .5), pct(es.errs, .95), pct(es.errs, .99), pct(es.sigmas, .5),
				es.within(0.6827), es.within(0.9545), es.within(0.999))
			t.Logf("%s, outside (up to 300 km from the centre): n=%d P50 %.1f m P95 %.1f m, median σh %.1f; NEES within 1σ %.2f 2σ %.2f",
				nc.name, len(out.errs), pct(out.errs, .5), pct(out.errs, .95), pct(out.sigmas, .5), out.within(0.6827), out.within(0.9545))
			t.Logf("%s: tracks %d confirmed %d, fixes %d, applied %d gated %d excluded %d pruned %d deleted %d",
				nc.name, st.Tracks, st.Confirmed, eng.fixes, applied, gated, st.Excluded, st.Pruned, st.Deleted)
			if len(es.errs) < 1000 {
				t.Fatalf("too few estimates: %d", len(es.errs))
			}
			if pct(es.errs, .5) > nc.p50 || pct(es.errs, .95) > nc.p95 {
				t.Fatalf("accuracy P50 %.1f / P95 %.1f above %v / %v", pct(es.errs, .5), pct(es.errs, .95), nc.p50, nc.p95)
			}
			if w := math.Min(es.within(0.9545), out.within(0.9545)); w < 0.8 {
				t.Fatalf("covariance optimistic: %.2f within 2σ", w)
			}
		})
	}
}

// TestSimnetTwoStation: every track inside the station footprint that is
// confirmed with σh < 100 m at t = 100 s is fed only the two stations
// nearest to it (when both heard the transmission) for the next 60 s (the
// coast horizon). Reported: per track, how long σh stays below 300 m; and
// whether the errors stay consistent with σ.
func TestSimnetTwoStation(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	type twoCase struct {
		dev  simnet.Device
		vel  bool    // BDS 5,0-like ground velocity every 5 s (σ 5 m/s)
		full float64 // gate: fraction of tracks holding σh < 300 m for 60 s
		h30  float64 // gate: fraction holding for >= 30 s
	}
	for _, tc := range []twoCase{
		// Without a velocity, slow traffic gets the manoeuvre allowance of
		// an unobservable stretch (tracker.go gapQ, H-track): its σ grows
		// honestly instead of holding < 300 m while a turn goes unseen
		// (before: 71 % / 47 % full holds, but only 93.8 % / 92.4 % of the
		// errors inside the 99.9 % region, single errors 2.7-6.9 km).
		{simnet.DeviceRadarcapeGPS, false, 0.35, 0.8},
		{simnet.DeviceSDR, false, 0.15, 0.5},
		{simnet.DeviceRadarcapeGPS, true, 0.9, 0.95},
		{simnet.DeviceSDR, true, 0.9, 0.95},
	} {
		dev := tc.dev
		name := string(dev)
		if tc.vel {
			name += "+velocity"
		}
		t.Run(name, func(t *testing.T) {
			pairs := map[uint32][2]int{}
			var es errStats
			maxSig := 0.0
			tracks := map[uint32]bool{}
			lost := 0
			eng := &simEngine{velEveryS: map[bool]float64{true: 5}[tc.vel]}
			var col []geo.Vec3
			cenS := geo.ToECEF(simLat, simLon, 0)
			hold := map[uint32]float64{}
			eng.restrict = func(g *simGroup) ([]int, bool) {
				if g.txS < 100 || g.txS > 160 {
					return nil, false
				}
				pr, ok := pairs[g.icao]
				if !ok {
					var e Estimate
					if !eng.tr.Get(g.icao, &e) || e.Status != StatusConfirmed || g.txS > 101 ||
						e.SigmaH > 100 || horizDist(cenS, g.pos) > 150e3 {
						return nil, false
					}
					if col == nil {
						for i := range eng.n.NumReceivers() {
							r := eng.n.Receiver(i)
							col = append(col, geo.Vec3{X: r.Pos[0], Y: r.Pos[1], Z: r.Pos[2]})
						}
					}
					a, b := -1, -1
					for i, p := range col {
						d := p.Dist(g.pos)
						if a < 0 || d < col[a].Dist(g.pos) {
							a, b = i, a
						} else if b < 0 || d < col[b].Dist(g.pos) {
							b = i
						}
					}
					pr = [2]int{a, b}
					pairs[g.icao] = pr
				}
				var keep []int
				for j, rx := range g.rx {
					if rx == pr[0] || rx == pr[1] {
						keep = append(keep, j)
					}
				}
				return keep, true
			}
			eng.onEst = func(g *simGroup, e *Estimate, r Result) {
				if _, ok := pairs[g.icao]; !ok || g.txS < 100 || g.txS > 160 {
					return
				}
				tracks[g.icao] = true
				if e.Status != StatusConfirmed {
					lost++
					return
				}
				if r == ResApplied {
					es.add(e, g.pos)
					maxSig = math.Max(maxSig, e.SigmaH)
				}
				if _, ok := hold[g.icao]; !ok {
					hold[g.icao] = 60
				}
				if e.SigmaH > 300 && g.txS-100 < hold[g.icao] {
					hold[g.icao] = g.txS - 100
				}
			}
			specs := simReceivers(dev, 8, 150)
			if dev == simnet.DeviceSDR {
				specs = simReceivers(dev, 20, 150)
			}
			runSimnet(t, specs, 120, 165*time.Second, eng)
			var hs []float64
			full, h30 := 0, 0
			for _, h := range hold {
				hs = append(hs, h)
				if h >= 60 {
					full++
				}
				if h >= 30 {
					h30++
				}
			}
			n := float64(len(hs))
			t.Logf("%s: %d tracks on 2 stations for 60 s (%d updates): σh < 300 m for the full 60 s: %.0f %%, for >= 30 s: %.0f %%, median hold %.0f s, worst %.0f s; max σh %.0f m; err P50 %.0f P95 %.0f max %.0f m; NEES within 1σ %.2f 99.9%% %.3f; lost %d",
				name, len(tracks), len(es.errs), 100*float64(full)/n, 100*float64(h30)/n, pct(hs, .5), pct(hs, 0), maxSig,
				pct(es.errs, .5), pct(es.errs, .95), es.maxOf(es.errs), es.within(0.6827), es.within(0.999), lost)
			if len(tracks) < 10 || len(es.errs) < 300 {
				t.Fatalf("too little data: %d tracks, %d updates", len(tracks), len(es.errs))
			}
			if float64(full)/n < tc.full || float64(h30)/n < tc.h30 || lost > 0 {
				t.Fatalf("2-station maintenance held σh < 300 m for 60 s on only %.0f %% of the tracks", 100*float64(full)/n)
			}
			// Manoeuvring traffic (helicopters, GA turning > 90°) is barely
			// observable on two stations; the manoeuvre allowance keeps its
			// errors inside σ (0.969 / 0.979 / 0.993 / 0.981 measured).
			if es.within(0.999) < 0.96 {
				t.Fatalf("2-station errors inconsistent with σ: %.3f within the 99.9%% region", es.within(0.999))
			}
		})
	}
}

const simLat, simLon = 50.5, 8.5

// horizDist is the horizontal distance of p from c (local ENU at c).
func horizDist(c, p geo.Vec3) float64 {
	f := geo.NewFrameECEF(c)
	e := f.ToENU(p)
	return math.Hypot(e.X, e.Y)
}
