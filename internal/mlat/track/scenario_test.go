package track

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
)

// driver emulates the engine on synthetic data: solver fixes until the
// track is confirmed, raw TOA clusters afterwards (§9.3/§9.5 routing).
type driver struct {
	tr     *Tracker
	s      *synth
	icao   uint32
	altSig float64
	// fixesOnly feeds fixes even for confirmed tracks (no raw clusters).
	fixesOnly bool
}

func (d *driver) step(t float64, p geo.Vec3, use []int, est *Estimate) Result {
	alt := geo.FromECEF(p).H
	c := d.s.cluster(d.icao, t, p, use, alt, d.altSig)
	confirmed := d.tr.Get(d.icao, est) && est.Status == StatusConfirmed
	if !confirmed || d.fixesOnly {
		rows := len(c.TOAs)
		if d.altSig > 0 {
			rows++
		}
		if rows < 4 {
			if confirmed {
				return d.tr.Cluster(&c, est)
			}
			return ResNoTrack
		}
		a := -1e9
		if d.altSig > 0 {
			a = c.AltHAE
		}
		sols := solveTOA(c.TOAs, a, d.altSig, &d.s.atm)
		if len(sols) == 0 {
			return ResIgnored
		}
		f := fixFrom(d.icao, d.s.tNs(t), sols, len(c.TOAs))
		return d.tr.Fix(&f, est)
	}
	return d.tr.Cluster(&c, est)
}

func straightPath(lat, lon float64, e0, n0, alt, ve, vn float64, segs ...seg) *path {
	return &path{fr: geo.NewFrame(geo.LLH{Lat: lat, Lon: lon}), start: vec{e0, n0, alt, ve, vn, 0, 0}, segs: segs}
}

func TestStraightTrackAccuracy(t *testing.T) {
	st := stationsAround(50, 8, 6, 80e3, true)
	d := &driver{tr: New(Params{}), s: newSynth(1, st, 50), icao: 0x3C0001, altSig: 25}
	pa := straightPath(50, 8, -30e3, -20e3, 10000, 180, 120)
	var es errStats
	var est Estimate
	for k := range 600 {
		tt := float64(k) * 0.5
		p, _ := pa.at(tt)
		d.step(tt, p, nil, &est)
		if tt > 20 && est.Status == StatusConfirmed {
			es.add(&est, p)
		}
	}
	p50, p95 := pct(es.errs, 0.5), pct(es.errs, 0.95)
	w1, w2 := es.within(0.6827), es.within(0.9545)
	t.Logf("straight, 7 stations @50 ns: n=%d P50 %.1f m P95 %.1f m, mean σh %.1f m; NEES within 1σ %.2f (0.68), 2σ %.2f (0.95)",
		len(es.errs), p50, p95, mean(es.sigmas), w1, w2)
	if len(es.errs) < 500 || p50 > 15 || p95 > 40 {
		t.Fatalf("accuracy: n=%d P50 %.1f P95 %.1f", len(es.errs), p50, p95)
	}
	if w1 < 0.5 || w1 > 0.9 || w2 < 0.85 {
		t.Fatalf("covariance not calibrated: within1σ %.2f within2σ %.2f", w1, w2)
	}
	if est.Speed() < 200 || est.Speed() > 233 {
		t.Fatalf("speed %.1f m/s, truth 216.3", est.Speed())
	}
	if est.PTurn > 0.5 {
		t.Fatalf("straight flight has P(CT) %.2f", est.PTurn)
	}
}

func mean(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// TestTurnIMM flies 180° turns at 3°/s and 6°/s and compares the IMM with
// a CV-only configuration of the same filter.
func TestTurnIMM(t *testing.T) {
	st := stationsAround(50, 8, 6, 80e3, true)
	for _, degS := range []float64{3, 6} {
		w := degS * math.Pi / 180
		dur := math.Pi / w
		run := func(p Params) (all, turn errStats, maxPT float64, wMean float64) {
			d := &driver{tr: New(p), s: newSynth(2, st, 50), icao: 0x3C0002, altSig: 25}
			pa := straightPath(50, 8, -40e3, 0, 9000, 200, 0, seg{60, 0, 0}, seg{dur, w, 0}, seg{90, 0, 0})
			var est Estimate
			var ws []float64
			for k := range int(2 * (150 + dur)) {
				tt := float64(k) * 0.5
				pp, _ := pa.at(tt)
				d.step(tt, pp, nil, &est)
				if tt < 20 || est.Status != StatusConfirmed {
					continue
				}
				all.add(&est, pp)
				if tt >= 60 && tt <= 65+dur {
					turn.add(&est, pp)
					maxPT = math.Max(maxPT, est.PTurn)
					if tt > 60+dur/4 && tt < 60+dur {
						ws = append(ws, est.TurnRate)
					}
				}
			}
			return all, turn, maxPT, mean(ws)
		}
		imm, immTurn, pt, wm := run(Params{})
		cv, cvTurn, _, _ := run(Params{InitPCT: 0.01, SojournCVS: 3600, SojournCTS: 1})
		t.Logf("%.0f°/s turn, IMM: all P50 %.1f P95 %.1f, turn P50 %.1f P95 %.1f max %.1f m, max P(CT) %.2f, mean ω in turn %.2f°/s; NEES within 1σ in turn %.2f",
			degS, pct(imm.errs, .5), pct(imm.errs, .95), pct(immTurn.errs, .5), pct(immTurn.errs, .95), immTurn.maxOf(immTurn.errs), pt, wm*180/math.Pi, immTurn.within(0.6827))
		t.Logf("%.0f°/s turn, CV : all P50 %.1f P95 %.1f, turn P50 %.1f P95 %.1f max %.1f m; NEES within 1σ in turn %.2f",
			degS, pct(cv.errs, .5), pct(cv.errs, .95), pct(cvTurn.errs, .5), pct(cvTurn.errs, .95), cvTurn.maxOf(cvTurn.errs), cvTurn.within(0.6827))
		if pt < 0.8 {
			t.Fatalf("IMM did not switch to the turn model (max P(CT) %.2f)", pt)
		}
		if math.Abs(wm/w-1) > 0.25 {
			t.Fatalf("mean estimated turn rate %.2f°/s, truth %.0f", wm*180/math.Pi, degS)
		}
		if pct(immTurn.errs, .95) >= pct(cvTurn.errs, .95) {
			t.Fatalf("IMM no better than CV in the turn")
		}
		if pct(immTurn.errs, .95) > 50 || immTurn.within(0.9545) < 0.8 {
			t.Fatalf("IMM turn P95 %.1f m, NEES 2σ %.2f", pct(immTurn.errs, .95), immTurn.within(0.9545))
		}
	}
}

// TestTwoStationMaintenance: a confirmed track keeps σ < 300 m on
// 2-station clusters plus altitude (§14 phase-2 acceptance for WP-TR): at
// least for the 60 s coast horizon with one fixed pair, indefinitely when
// the pair changes or a velocity (BDS 5,0) is available; and the errors
// stay consistent with the reported σ.
func TestTwoStationMaintenance(t *testing.T) {
	st := stationsAround(50, 8, 4, 70e3, true)
	type variant struct {
		name    string
		pairs   [][]int
		sigNs   float64
		vel     bool
		holdS   float64 // σh must stay < 300 m at least this long
		maxSigH float64 // and below this over the 180 s
	}
	for _, v := range []variant{
		{"fixed pair GPS", [][]int{{0, 1}}, 50, false, 60, 5000},
		{"fixed pair SDR", [][]int{{0, 1}}, 250, false, 60, 5000},
		{"alternating pairs SDR", [][]int{{0, 1}, {0, 2}, {1, 3}}, 250, false, 180, 300},
		{"fixed pair SDR + BDS velocity", [][]int{{0, 1}}, 250, true, 180, 300},
	} {
		t.Run(v.name, func(t *testing.T) {
			d := &driver{tr: New(Params{}), s: newSynth(3, st, v.sigNs), icao: 0x3C0003, altSig: 25}
			pa := straightPath(50, 8, -20e3, 30e3, 10500, 210, -60)
			rng := rand.New(rand.NewPCG(3, 3))
			var est Estimate
			var es errStats
			maxSig, firstOver := 0.0, math.Inf(1)
			applied, gated := 0, 0
			for k := range 2 * 240 {
				tt := float64(k) * 0.5
				p, xt := pa.at(tt)
				var use []int
				if tt >= 60 {
					use = v.pairs[(k/2)%len(v.pairs)]
				}
				r := d.step(tt, p, use, &est)
				if v.vel && k%4 == 0 {
					vm := Velocity{ICAO: d.icao, TNs: d.s.tNs(tt), East: xt[iVE] + rng.NormFloat64()*5, North: xt[iVN] + rng.NormFloat64()*5, SigmaH: 5}
					d.tr.Velocity(&vm, &est)
				}
				if tt < 60 {
					continue
				}
				if r == ResApplied {
					applied++
				} else {
					gated++
				}
				if est.Status != StatusConfirmed {
					t.Fatalf("track lost at %.1f s (%v)", tt, r)
				}
				es.add(&est, p)
				maxSig = math.Max(maxSig, est.SigmaH)
				if est.SigmaH > 300 && tt-60 < firstOver {
					firstOver = tt - 60
				}
			}
			f99 := es.within(0.999)
			t.Logf("%s: 180 s on 2 stations: applied %d gated %d, max σh %.0f m (first > 300 m after %.0f s), err P50 %.0f P95 %.0f max %.0f m, NEES within 1σ %.2f, 99.9%% %.3f",
				v.name, applied, gated, maxSig, firstOver, pct(es.errs, .5), pct(es.errs, .95), es.maxOf(es.errs), es.within(0.6827), f99)
			if firstOver < v.holdS || maxSig > v.maxSigH {
				t.Fatalf("σh exceeded 300 m after %.0f s of 2-station maintenance (max %.0f m)", firstOver, maxSig)
			}
			if f99 < 0.97 || gated > 0 {
				t.Fatalf("2-station errors inconsistent with σ: %.3f within the 99.9%% region, %d gated", f99, gated)
			}
		})
	}
}

// TestAmbiguity3Alt: tracks started from 3-station + altitude fixes with
// two branches select the true branch within 10 s (§13.2 gate: > 95 %).
// Gate variant: a fourth station hears 20 % of the transmissions, as at a
// real network edge. The other variants are reported; with only ever the
// same three stations, only the dynamics, speed and radio-horizon checks
// can separate the branches.
func TestAmbiguity3Alt(t *testing.T) {
	for _, fourth := range []float64{0.2, 0.1, 0} {
		correct, wrong, open, trials := ambiguityTrials(fourth, 200, 10)
		frac := float64(correct) / float64(trials)
		t.Logf("3+alt ambiguity, 4th station on %.0f %% of transmissions: %d ambiguous starts, correct and resolved within 10 s: %d (%.1f %%), still ambiguous (flagged) %d, resolved to the wrong branch or lost %d",
			100*fourth, trials, correct, 100*frac, open, wrong)
		if fourth == 0.2 && (trials < 50 || frac < 0.95) {
			t.Fatalf("ambiguity resolution %.1f %% (%d trials)", 100*frac, trials)
		}
		if wrong > 0 {
			t.Fatalf("%d ambiguous starts resolved to the wrong branch", wrong)
		}
		// same three stations only: the kinematic prior resolves the far,
		// slow branches (36.9 % before it)
		if fourth == 0 && frac < 0.4 {
			t.Fatalf("same-three-station resolution %.1f %%", 100*frac)
		}
	}
	// strictly the same three stations, longer: the kinematic prior and the
	// dynamics keep separating the branches (H-track item 2)
	for _, dur := range []float64{30, 60} {
		correct, wrong, open, trials := ambiguityTrials(0, 200, dur)
		t.Logf("3+alt ambiguity, same 3 stations only, after %.0f s: %d (%.1f %%) correct and resolved, %d flagged, %d wrong",
			dur, correct, 100*float64(correct)/float64(trials), open, wrong)
		// near-tangent pairs whose branches stay within a few km of each
		// other can be decided wrongly by chance (likelihood ratio 100)
		if float64(wrong) > 0.05*float64(trials) {
			t.Fatalf("%d of %d ambiguous starts resolved to the wrong branch after %.0f s", wrong, trials, dur)
		}
	}
}

func ambiguityTrials(fourth float64, n int, durS float64) (correct, wrong, open, trials int) {
	rng := rand.New(rand.NewPCG(7, 7))
	st := stationsAround(50, 8, 4, 60e3, false)
	for trial := range n {
		ang := rng.Float64() * 2 * math.Pi
		r := 40e3 + rng.Float64()*120e3
		hdg := rng.Float64() * 2 * math.Pi
		spd := 120 + rng.Float64()*130
		pa := straightPath(50, 8, r*math.Cos(ang), r*math.Sin(ang), 3000+rng.Float64()*9000, spd*math.Sin(hdg), spd*math.Cos(hdg))
		d := &driver{tr: New(Params{}), s: newSynth(uint64(100+trial), st, 100), icao: uint32(0x400000 + trial), altSig: 25}
		// the triple that hears most traffic: the 3 nearest stations
		p0, _ := pa.at(0)
		order := []int{0, 1, 2, 3}
		for i := range order {
			for j := i + 1; j < len(order); j++ {
				if st[order[j]].Dist(p0) < st[order[i]].Dist(p0) {
					order[i], order[j] = order[j], order[i]
				}
			}
		}
		triple := order[:3]
		c := d.s.cluster(d.icao, 0, p0, triple, geo.FromECEF(p0).H, 25)
		sols := solveTOA(c.TOAs, c.AltHAE, 25, &d.s.atm)
		if len(sols) < 2 {
			continue // unambiguous geometry
		}
		trials++
		var est Estimate
		f := fixFrom(d.icao, d.s.tNs(0), sols, 3)
		d.tr.Fix(&f, &est)
		for k := 1; k <= int(2*durS); k++ {
			tt := float64(k) * 0.5
			p, _ := pa.at(tt)
			use := triple
			if rng.Float64() < fourth {
				use = order
			}
			d.step(tt, p, use, &est)
		}
		p, _ := pa.at(durS)
		ok := d.tr.Get(d.icao, &est)
		switch {
		case !ok:
			wrong++
		case est.Ambiguous:
			open++
		case est.ECEF.Dist(p) > 2000:
			wrong++
		default:
			correct++
		}
	}
	return
}

// TestOutOfSequence delivers clusters shuffled within 1.5 s and expects
// almost the in-order accuracy.
func TestOutOfSequence(t *testing.T) {
	st := stationsAround(50, 8, 6, 80e3, true)
	pa := straightPath(50, 8, -30e3, -20e3, 10000, 180, 120)
	run := func(shuffle bool) (errStats, Stats) {
		tr := New(Params{})
		s := newSynth(4, st, 50)
		rng := rand.New(rand.NewPCG(4, 4))
		var es errStats
		var est Estimate
		// start the track from two fixes
		for k := range 2 {
			tt := float64(k)
			p, _ := pa.at(tt)
			c := s.cluster(1, tt, p, nil, geo.FromECEF(p).H, 25)
			f := fixFrom(1, s.tNs(tt), solveTOA(c.TOAs, c.AltHAE, 25, &s.atm), 7)
			tr.Fix(&f, &est)
		}
		type item struct {
			t float64
			c Cluster
		}
		var batch []item
		flush := func() {
			if shuffle {
				rng.Shuffle(len(batch), func(i, j int) { batch[i], batch[j] = batch[j], batch[i] })
			}
			for _, it := range batch {
				tr.Cluster(&it.c, &est)
			}
			batch = batch[:0]
			// evaluate at the newest time
			var e2 Estimate
			if tr.Get(1, &e2) {
				tt := float64(e2.TNs-s.base) / 1e9
				p, _ := pa.at(tt)
				es.add(&e2, p)
			}
		}
		for k := 4; k < 600; k++ {
			tt := float64(k) * 0.5
			p, _ := pa.at(tt)
			batch = append(batch, item{tt, s.cluster(1, tt, p, nil, geo.FromECEF(p).H, 25)})
			if len(batch) == 3 {
				flush()
			}
		}
		return es, tr.Stats()
	}
	in, _ := run(false)
	sh, sst := run(true)
	t.Logf("in order: P50 %.1f P95 %.1f; shuffled within 1.5 s: P50 %.1f P95 %.1f (OOSM %d, gated %d)",
		pct(in.errs, .5), pct(in.errs, .95), pct(sh.errs, .5), pct(sh.errs, .95), sst.OOSM, sst.Gated)
	if sst.OOSM == 0 {
		t.Fatal("no out-of-sequence updates")
	}
	if pct(sh.errs, .5) > 1.3*pct(in.errs, .5)+2 || pct(sh.errs, .95) > 1.3*pct(in.errs, .95)+3 {
		t.Fatal("out-of-sequence handling degrades accuracy too much")
	}
}

// TestOutlierExclusion: one station of each cluster is 3 µs late every
// tenth transmission; the member is excluded and the track stays accurate.
func TestOutlierExclusion(t *testing.T) {
	st := stationsAround(50, 8, 6, 80e3, true)
	d := &driver{tr: New(Params{}), s: newSynth(5, st, 50), icao: 9, altSig: 25}
	pa := straightPath(50, 8, -30e3, -20e3, 10000, 180, 120)
	var es errStats
	var est Estimate
	for k := range 400 {
		tt := float64(k) * 0.5
		p, _ := pa.at(tt)
		if k > 40 && k%10 == 0 && est.Status == StatusConfirmed {
			c := d.s.cluster(d.icao, tt, p, nil, geo.FromECEF(p).H, 25)
			c.TOAs[k%len(c.TOAs)].T += 3e-6
			d.tr.Cluster(&c, &est)
		} else {
			d.step(tt, p, nil, &est)
		}
		if tt > 20 && est.Status == StatusConfirmed {
			es.add(&est, p)
		}
	}
	s := d.tr.Stats()
	t.Logf("outliers: excluded %d, gated %d, P50 %.1f P95 %.1f max %.1f", s.Excluded, s.Gated, pct(es.errs, .5), pct(es.errs, .95), es.maxOf(es.errs))
	if s.Excluded < 30 || pct(es.errs, .95) > 40 {
		t.Fatalf("outlier exclusion: excluded %d, P95 %.1f", s.Excluded, pct(es.errs, .95))
	}
}
