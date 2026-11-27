package mlat

import (
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/track"
	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// Mode A/C acceptance (DESIGN §9.9, §13; WP-MODEAC): simnet row-1 network
// (8 GPS anchors in 150 km) whose clocks are synchronised by the ordinary
// Mode S traffic, plus Mode A/C-only aircraft that simnet does not model.
// acGen (a simnet.Sink) generates their SSR replies: every aircraft is
// interrogated by several radars, each sweep gives a dwell of replies 4 ms
// apart alternating identity (Mode A) and altitude (Mode C); every reply is
// propagated (simnet's refraction model) to every receiver in radio range,
// heard with a probability, stamped by the receiver's true clock
// (simnet.ClockAt: offset, drift, epoch) plus jitter, and handed to the
// engine with ObserveModeAC. FRUIT (random codes at random times) is added
// at every receiver. Nothing in the engine sees the truth.
//
// Reported: availability (share of aircraft-seconds in the evaluation
// window for which a shown target lies within 1 km of the aircraft, among
// aircraft heard by ≥ 4 Mode A/C stations), horizontal error P50/P95 of the
// shown targets against the truth at the target's time, identities per
// aircraft, squawk/altitude correctness, and false targets.

type acAir struct {
	p0, v  geo.Vec3 // ENU (m, m/s) in the generator frame at t=0
	squawk uint16
	phase  []float64
}

type acGen struct {
	h       *harness
	fr      geo.Frame
	air     []acAir
	radars  []float64
	dwell   int
	pHear   float64
	extraNs float64 // A/C leading-edge jitter on top of the clock's (1σ)
	fruitHz float64 // per receiver
	startS  float64
	midS    float64
	prevS   float64
	rng     *rand.Rand
	rx      []simnet.ReceiverTruth
	specs   []simnet.ReceiverSpec
	buf     []ModeACReply

	heard4 map[int]float64 // aircraft -> last time a reply was heard by >= 4 Mode A/C stations
	sent   int
}

func newACGen(h *harness, seed uint64, n, vfrEvery int) *acGen {
	g := &acGen{h: h, fr: geo.NewFrame(geo.LLH{Lat: 51, Lon: 9}), radars: []float64{4, 4.8, 6, 10},
		dwell: 12, pHear: 0.75, extraNs: 40, fruitHz: 100, rng: rand.New(rand.NewPCG(seed, 321)),
		rx: h.n.Receivers(), specs: h.n.Config().Receivers, heard4: map[int]float64{}}
	sq := map[uint16]bool{}
	for i := 0; i < n; i++ {
		hd := g.rng.Float64() * 2 * math.Pi
		sp := 60 + 180*g.rng.Float64()
		r := 60e3 * math.Sqrt(g.rng.Float64())
		b := g.rng.Float64() * 2 * math.Pi
		a := acAir{p0: geo.Vec3{X: r * math.Cos(b), Y: r * math.Sin(b), Z: 800 + 9000*g.rng.Float64()},
			v: geo.Vec3{X: sp * math.Sin(hd), Y: sp * math.Cos(hd), Z: (g.rng.Float64() - 0.5) * 8}}
		// every vfrEvery-th squawks VFR 7000; the rest unique codes
		if vfrEvery > 0 && i%vfrEvery == 0 {
			a.squawk = 0o7000
		} else {
			for a.squawk == 0 || sq[a.squawk] || a.squawk == 0o7000 {
				a.squawk = uint16(g.rng.IntN(0o7777))
			}
			sq[a.squawk] = true
		}
		for _, P := range g.radars {
			a.phase = append(a.phase, g.rng.Float64()*P)
		}
		g.air = append(g.air, a)
	}
	return g
}

// pos returns aircraft i's true ECEF position at simulation time tS
// (straight line through its position at midS, the middle of the Mode A/C
// window, so that it stays within ~110 km of the centre).
func (g *acGen) pos(i int, tS float64) geo.Vec3 {
	a := &g.air[i]
	p := a.p0.Add(a.v.Scale(tS - g.midS))
	p.Z = math.Min(math.Max(p.Z, 500), 12000)
	return g.fr.FromENU(p)
}

func (g *acGen) altCode(i int, tS float64) uint16 {
	h := geo.FromECEF(g.pos(i, tS)).H - 48 // pressure altitude ≈ HAE − geoid (≈ 48 m at 51°N 9°E), ISA
	c, _ := track.ACFromAltitudeFt(int(math.Round(h / 0.3048)))
	return c
}

// stamp returns the reply as stamped by receiver k for a reply leaving
// position p at tS; ok false when not heard.
func (g *acGen) stamp(k int, p geo.Vec3, tS float64, code uint16) (ModeACReply, bool) {
	rt := &g.rx[k]
	if !rt.Class.GPS() {
		return ModeACReply{}, false
	}
	rp := geo.Vec3{X: rt.Pos[0], Y: rt.Pos[1], Z: rt.Pos[2]}
	d := rp.Dist(p)
	hac := geo.FromECEF(p).H
	hz := 4120 * (math.Sqrt(math.Max(rt.HAE, 1)) + math.Sqrt(math.Max(hac, 1)))
	if d > math.Min(hz, 400e3) || g.rng.Float64() >= g.pHear {
		return ModeACReply{}, false
	}
	atm := g.h.n.Config().Atmosphere
	sp := simnet.Vec3{p.X, p.Y, p.Z}
	ex := atm.ExcessPathM(rt.Pos, sp, rt.Pos.Norm()-rt.HAE, d)
	arr := tS + (d+ex)/simnet.SpeedOfLight
	cs := &g.specs[k].Clock
	at := arr + cs.FrameRefACNs*1e-9
	ct, _ := g.h.n.ClockAt(k, at)
	noise := (cs.JitterNs*g.rng.NormFloat64() + g.extraNs*g.rng.NormFloat64()) * 1e-9
	local := ct.LocalS + noise
	ticks := g.h.n.UnixNs(0) + int64(math.Floor(local*1e9))
	return ModeACReply{Station: model.StationIdx(k + 2), Epoch: ct.Epoch, Ticks: uint64(ticks), Code: code}, true
}

// Observe implements simnet.Sink (Mode S observations are the harness').
func (g *acGen) Observe([]simnet.Observation) {}

// Advance implements simnet.Sink: the replies of (prevS, hz].
func (g *acGen) Advance(hz float64) {
	from := g.prevS
	g.prevS = hz
	if hz <= g.startS {
		return
	}
	from = math.Max(from, g.startS)
	g.buf = g.buf[:0]
	for i := range g.air {
		a := &g.air[i]
		for r, P := range g.radars {
			// sweeps starting in (from - dwell, hz]
			k0 := math.Floor((from - a.phase[r] - 0.1) / P)
			for t := a.phase[r] + k0*P; t <= hz; t += P {
				for j := 0; j < g.dwell; j++ {
					tt := t + float64(j)*0.004
					if tt <= from || tt > hz {
						continue
					}
					p := g.pos(i, tt)
					code := track.ACFromSquawk(a.squawk)
					if j%2 == 1 {
						code = g.altCode(i, tt)
					}
					n := 0
					for k := range g.rx {
						if rep, ok := g.stamp(k, p, tt, code); ok {
							g.buf = append(g.buf, rep)
							if g.h.e.planner.ModeACEligible(rep.Station) {
								n++
							}
						}
					}
					if n >= 4 {
						g.heard4[i] = tt
					}
				}
			}
		}
	}
	// FRUIT at every GPS receiver
	for k := range g.rx {
		if !g.rx[k].Class.GPS() {
			continue
		}
		for t := from + g.rng.ExpFloat64()/g.fruitHz; t <= hz; t += g.rng.ExpFloat64() / g.fruitHz {
			ct, _ := g.h.n.ClockAt(k, t)
			g.buf = append(g.buf, ModeACReply{Station: model.StationIdx(k + 2), Epoch: ct.Epoch,
				Ticks: uint64(g.h.n.UnixNs(0) + int64(math.Floor(ct.LocalS*1e9))), Code: uint16(1 + g.rng.IntN(1<<13-1))})
		}
	}
	g.sent += len(g.buf)
	g.h.e.ObserveModeAC(g.buf)
}

type acEvalRes struct {
	samples, covered     int // aircraft-seconds heard by >= 4 / with a shown target within 1 km
	errs                 []float64
	falseT               map[uint32]bool
	idsOf                map[int]map[uint32]bool
	sqOK, sqN, altOK, nA int
	shownAC              map[int]bool
	eligible             map[int]bool
}

// acNetCfg is one Mode A/C netsim scenario.
type acNetCfg struct {
	name     string
	seed     uint64
	aircraft int
	vfrEvery int // every vfrEvery-th aircraft squawks 7000
	radars   []float64
	pHear    float64
	extraNs  float64
	fruitHz  float64
}

// acNetRes are the measured figures of a scenario.
type acNetRes struct {
	avail, p50, p95, idsPer, sqOK, altOK float64
	eligible, shown, falseT, stations    int
	fixP50, fixP95                       float64
}

func runModeACNetsim(t *testing.T, c acNetCfg) acNetRes {
	mins := envDur("MLAT_MODEAC_MIN", 16*time.Minute)
	evalFrom := mins.Seconds() - 240
	h := newHarness(t, harnessCfg{seed: c.seed, devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 8},
		radiusKm: 150, modeSFrac: 0.3, evalFromS: 1e9, modeAC: true})
	g := newACGen(h, c.seed, c.aircraft, c.vfrEvery)
	g.radars, g.pHear, g.extraNs, g.fruitHz = c.radars, c.pHear, c.extraNs, c.fruitHz
	for i := range g.air {
		g.air[i].phase = g.air[i].phase[:0]
		for _, P := range g.radars {
			g.air[i].phase = append(g.air[i].phase, g.rng.Float64()*P)
		}
	}
	g.startS = 540 // the GPS stations become anchors after their 10-minute probation
	g.midS = (g.startS + mins.Seconds()) / 2
	h.n.AddSink(g)
	var fixes, noTarget int
	var fixErr, fixSig []float64
	if err := h.e.EnableModeAC(func(f ModeACFix) {
		fixes++
		if f.Target == 0 {
			noTarget++
		}
		tS := float64(f.TUnixNs-h.n.UnixNs(0)) / 1e9
		p := geo.Vec3{X: f.ECEF[0], Y: f.ECEF[1], Z: f.ECEF[2]}
		best := math.Inf(1)
		for i := range g.air {
			best = math.Min(best, horiz(p, g.pos(i, tS)))
		}
		fixErr = append(fixErr, best)
		fixSig = append(fixSig, f.SigmaHM)
	}); err != nil {
		t.Fatal(err)
	}
	res := acEvalRes{falseT: map[uint32]bool{}, idsOf: map[int]map[uint32]bool{}, shownAC: map[int]bool{}, eligible: map[int]bool{}}
	start := time.Now()
	for s := 1.0; s <= mins.Seconds(); s++ {
		h.run(time.Second)
		if s < evalFrom {
			continue
		}
		g.evaluate(h, s, &res)
	}
	st := h.e.ModeACStats()
	nAir := len(res.eligible)
	frag := 0
	for i := range res.eligible {
		frag += len(res.idsOf[i])
	}
	out := acNetRes{avail: float64(res.covered) / float64(max(res.samples, 1)), p50: quantile(res.errs, 0.5), p95: quantile(res.errs, 0.95),
		idsPer: float64(frag) / float64(max(nAir, 1)), sqOK: float64(res.sqOK) / float64(max(res.sqN, 1)),
		altOK: float64(res.altOK) / float64(max(res.nA, 1)), eligible: nAir, shown: len(res.shownAC), falseT: len(res.falseT),
		stations: st.Stations, fixP50: quantile(fixErr, 0.5), fixP95: quantile(fixErr, 0.95)}
	t.Logf("%s (%v sim, %.0f s wall): %d replies sent, %d fixes; stations %d; stats %+v",
		c.name, mins, time.Since(start).Seconds(), g.sent, fixes, st.Stations, st)
	t.Logf("%s: fixes %d (no target %d), error to nearest aircraft P50 %.0f m P90 %.0f m P95 %.0f m; σh P50 %.0f m",
		c.name, fixes, noTarget, out.fixP50, quantile(fixErr, 0.9), out.fixP95, quantile(fixSig, 0.5))
	t.Logf("%s: availability %.1f %% of %d aircraft-seconds (aircraft heard by >= 4 Mode A/C stations: %d, shown at least once: %d); "+
		"error P50 %.0f m P95 %.0f m (%d samples); identities per aircraft %.2f; squawk correct %.1f %%, altitude present %.1f %%; false targets %d",
		c.name, 100*out.avail, res.samples, nAir, out.shown, out.p50, out.p95, len(res.errs), out.idsPer,
		100*out.sqOK, 100*out.altOK, out.falseT)
	if os.Getenv("MLAT_MODEAC_DEBUG") != "" {
		for _, tg := range h.e.ModeACTargets() {
			tS := float64(tg.TUnixNs-h.n.UnixNs(0)) / 1e9
			p := geo.ToECEF(tg.Lat, tg.Lon, tg.AltGeoM)
			best, bi := math.Inf(1), -1
			for i := range g.air {
				if d := horiz(p, g.pos(i, tS)); d < best {
					best, bi = d, i
				}
			}
			t.Logf("target %s sq %s (truth %04o) d %.0f m σ %.0f grade %s fixes %d upd %d age %.1f seen %.0f crowded %v",
				tg.ID, tg.Squawk, g.air[bi].squawk, best, tg.SigmaHM, tg.Grade, tg.Fixes, tg.Updates, tg.AgeS, tg.SeenS, tg.Crowded)
		}
	}
	for _, a := range h.e.ModeACAircraft() {
		if a.AddrType != "modeac" || a.ICAO[0] != '#' || a.Lat == nil {
			t.Fatalf("aircraft DTO %+v", a)
		}
	}
	if st.Stations < 5 || nAir < c.aircraft/2 {
		t.Fatalf("only %d Mode A/C stations, %d aircraft in the 4-station footprint", st.Stations, nAir)
	}
	return out
}

// TestModeACNetsim is the acceptance run: 40 Mode A/C-only aircraft (a
// quarter on 7000), four SSR radars, 75 % reception per reply and station,
// 50 ns extra A/C timing noise, 100 FRUIT replies/s per station.
func TestModeACNetsim(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet Mode A/C acceptance (≈1 min)")
	}
	r := runModeACNetsim(t, acNetCfg{name: "Mode A/C netsim", seed: 11, aircraft: 40, vfrEvery: 4,
		radars: []float64{4, 4.8, 6, 10}, pHear: 0.75, extraNs: 50, fruitHz: 100})
	if r.avail < 0.95 {
		t.Errorf("availability %.1f %% (>= 95 %%)", 100*r.avail)
	}
	if r.p50 > 50 || r.p95 > 200 {
		t.Errorf("error P50 %.0f m (<= 50) P95 %.0f m (<= 200)", r.p50, r.p95)
	}
	if r.falseT > 0 {
		t.Errorf("%d false targets", r.falseT)
	}
	if r.sqOK < 0.95 {
		t.Errorf("squawk correct %.1f %%", 100*r.sqOK)
	}
	if r.idsPer > 1.2 {
		t.Errorf("%.2f identities per aircraft", r.idsPer)
	}
}

// TestModeACNetsimHard is a stress variant (MLAT_MODEAC_HARD=1): 80
// aircraft, half of them on 7000, two radars, 50 % reception, 80 ns extra
// timing noise, 500 FRUIT replies/s per station. Looser gates; the figures
// are recorded in WIRING.md §6.
func TestModeACNetsimHard(t *testing.T) {
	if testing.Short() || os.Getenv("MLAT_MODEAC_HARD") == "" {
		t.Skip("set MLAT_MODEAC_HARD=1 (≈2 min)")
	}
	r := runModeACNetsim(t, acNetCfg{name: "Mode A/C netsim hard", seed: 12, aircraft: 80, vfrEvery: 2,
		radars: []float64{4.8, 10}, pHear: 0.5, extraNs: 80, fruitHz: 500})
	if r.avail < 0.8 || r.p95 > 500 || r.falseT > 2 || r.idsPer > 1.5 {
		t.Errorf("hard: availability %.1f %% P95 %.0f m false %d ids/aircraft %.2f", 100*r.avail, r.p95, r.falseT, r.idsPer)
	}
}

// evaluate compares the published targets with the truth at second s.
func (g *acGen) evaluate(h *harness, s float64, r *acEvalRes) {
	ts := h.e.ModeACTargets()
	// aircraft heard by >= 4 Mode A/C stations within the last 15 s
	var live []int
	for i := range g.air {
		if hs, ok := g.heard4[i]; ok && s-hs <= 15 {
			live = append(live, i)
			r.eligible[i] = true
		}
	}
	matched := map[int]bool{}
	for _, tg := range ts {
		tS := float64(tg.TUnixNs-h.n.UnixNs(0)) / 1e9
		p := geo.ToECEF(tg.Lat, tg.Lon, tg.AltGeoM)
		best, bi := math.Inf(1), -1
		for i := range g.air {
			if d := horiz(p, g.pos(i, tS)); d < best {
				best, bi = d, i
			}
		}
		if best > 2000 {
			if !r.falseT[tg.Key] && os.Getenv("MLAT_MODEAC_DEBUG") != "" {
				println("false target", tg.ID, int(s), "sq", tg.Squawk, "d", int(best), "σ", int(tg.SigmaHM), "grade", tg.Grade,
					"fixes", tg.Fixes, "upd", tg.Updates, "seen", int(tg.SeenS), "age", int(tg.AgeS), "alt", int(tg.AltGeoM), "truth sq", g.air[bi].squawk)
			}
			r.falseT[tg.Key] = true
			continue
		}
		r.errs = append(r.errs, best)
		r.shownAC[bi] = true
		if r.idsOf[bi] == nil {
			r.idsOf[bi] = map[uint32]bool{}
		}
		r.idsOf[bi][tg.Key] = true
		if best <= 1000 {
			matched[bi] = true
		}
		r.nA++
		if tg.AltFt != nil {
			r.altOK++
		}
		if tg.Squawk != "" {
			r.sqN++
			if tg.Squawk == track.ACSquawkString(track.ACFromSquawk(g.air[bi].squawk)) {
				r.sqOK++
			}
		}
	}
	for _, i := range live {
		r.samples++
		if matched[i] {
			r.covered++
		}
	}
}

func horiz(a, b geo.Vec3) float64 {
	lf := geo.NewFrame(geo.FromECEF(b))
	d := lf.ToENU(a)
	return math.Hypot(d.X, d.Y)
}

func quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	w := slices.Clone(v)
	slices.Sort(w)
	return w[min(len(w)-1, int(q*float64(len(w))))]
}
