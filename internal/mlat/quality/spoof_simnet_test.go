package quality

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/solve"
)

// Synthetic spoofers on simnet (DESIGN §9.6 ADS-B verification, phase 4).
//
// Offset spoofers: real aircraft, real receptions (simnet: multipath,
// gross outliers, jitter, the harness's sync error) whose reported
// position is moved by D metres in a fixed direction per aircraft — an
// airframe whose GNSS is spoofed or a transponder fed a wrong position.
// D = 0 is the honest population (with simnet's 0..300 ms airframe latency
// and NACp-consistent errors, which must not trip the test).
//
// Ground spoofers: a transmitter on the ground broadcasting fake aircraft
// 30-150 km away at FL350; its messages reach the stations within 150 km
// and line of sight, with the same timing noise.
//
// Both detectors are scored: Verify (raw arrivals, ≥ 2 stations) and
// CompareMLAT (snapshot MLAT fix, ≥ 4 stations), each through a Verifier
// (3 contradictions within 60 s → suspect), for several parameter sets on
// the same data.

type spoofScore struct {
	msgs, contra, confirm uint64
	ver                   *Verifier
	aircraft              map[uint32]float64 // first check time (s)
	suspectAt             map[uint32]float64
}

func newSpoofScore(onlyConfirmed bool, frac float64) *spoofScore {
	v := NewVerifier()
	v.OnlyConfirmedResets = onlyConfirmed
	v.ContradictFrac = frac
	return &spoofScore{ver: v, aircraft: map[uint32]float64{}, suspectAt: map[uint32]float64{}}
}

func (s *spoofScore) add(key uint32, tS float64, nowNs int64, v Verdict) {
	if v == VerdictNone {
		return
	}
	s.msgs++
	switch v {
	case VerdictContradicted:
		s.contra++
	case VerdictConfirmed:
		s.confirm++
	}
	if _, ok := s.aircraft[key]; !ok {
		s.aircraft[key] = tS
	}
	if s.ver.Record(key, nowNs, v) {
		if _, ok := s.suspectAt[key]; !ok {
			s.suspectAt[key] = tS
		}
	}
}

func (s *spoofScore) caught() float64 {
	return float64(len(s.suspectAt)) / float64(max(len(s.aircraft), 1))
}

func (s *spoofScore) line(name string) string {
	var ttd []float64
	for k, at := range s.suspectAt {
		ttd = append(ttd, at-s.aircraft[k])
	}
	sort.Float64s(ttd)
	med := math.NaN()
	if len(ttd) > 0 {
		med = ttd[len(ttd)/2]
	}
	return fmt.Sprintf("%-16s msgs %6d  contradicted %7.3f%%  confirmed %6.2f%%  aircraft %3d  suspect %3d (%5.1f%%)  median time-to-suspect %4.1f s",
		name, s.msgs, 100*float64(s.contra)/float64(max(s.msgs, 1)), 100*float64(s.confirm)/float64(max(s.msgs, 1)),
		len(s.aircraft), len(s.suspectAt), 100*s.caught(), med)
}

// offsetDir returns a fixed unit direction (east, north) per aircraft.
func offsetDir(key uint32) (float64, float64) {
	a := float64(mix64(uint64(key))%3600) / 3600 * 2 * math.Pi
	return math.Sin(a), math.Cos(a)
}

var spoofOffsets = []float64{0, 150, 300, 500, 1000, 2000, 5000}

// spoofCfg is one parameter set under test.
type spoofCfg struct {
	name          string
	vp            VerifyParams
	cp            CompareParams
	onlyConfirmed bool
	frac          float64
}

type spoofCfgRes struct {
	tdoa, mlat []*spoofScore // per offset
	ground     *spoofScore
	groundMLAT *spoofScore
}

func spoofRun(tb testing.TB, seed uint64, dur time.Duration, cfgs []spoofCfg) []spoofCfgRes {
	tb.Helper()
	atm := geo.DefaultAtmosphere()
	rangeFn := func(rx, ac geo.Vec3) float64 { return atm.Range(rx, ac) }
	sv := solve.NewSolver(solve.Options{Atmosphere: atm})
	rng := rand.New(rand.NewPCG(seed, 31))
	res := make([]spoofCfgRes, len(cfgs))
	for c := range cfgs {
		r := &res[c]
		r.ground, r.groundMLAT = newSpoofScore(cfgs[c].onlyConfirmed, cfgs[c].frac), newSpoofScore(cfgs[c].onlyConfirmed, cfgs[c].frac)
		for range spoofOffsets {
			r.tdoa = append(r.tdoa, newSpoofScore(cfgs[c].onlyConfirmed, cfgs[c].frac))
			r.mlat = append(r.mlat, newSpoofScore(cfgs[c].onlyConfirmed, cfgs[c].frac))
		}
	}
	var f solve.Fix
	var toas [16]TOA
	var sig [16]float64
	var h *harnessQ
	h = newHarnessQ(tb, hOpts{seed: seed, receivers: 60, radiusKm: 250, aircraft: 250}, func(t *hTx) {
		if !t.pos17 {
			return
		}
		epoch := math.Floor(t.txS)
		m := min(len(t.rows), 16)
		for i := 0; i < m; i++ {
			toas[i] = TOA{Pos: t.rows[i].pos, T: t.rows[i].toaS - epoch}
			sig[i] = t.rows[i].sigmaS
		}
		now := h.unixNs(t.txS)
		repAlt := t.hae + rng.NormFloat64()*20
		base := geo.ToECEF(t.repLa, t.repLo, repAlt)
		fr := geo.NewFrameECEF(base)
		cov := RefCovECEF(base, t.vel, t.nacp, 0.15, 30)
		mlatOK := false
		if len(t.rows) >= 4 {
			var pr solve.Problem
			var st [solve.MaxStations]solve.Station
			k := min(len(t.rows), solve.MaxStations)
			for i := 0; i < k; i++ {
				st[i] = solve.Station{Pos: t.rows[i].pos, TOA: t.rows[i].toaS - epoch, Sigma: t.rows[i].sigmaS}
			}
			pr.Stations = st[:k]
			pr.Alt = solve.Altitude{Valid: true, HAE: repAlt, Sigma: 30}
			mlatOK = sv.Solve(&pr, &f) == nil && MLATComparable(&f)
		}
		for oi, d := range spoofOffsets {
			rep := base
			if d > 0 {
				de, dn := offsetDir(t.icao)
				rep = fr.FromENU(geo.Vec3{X: de * d, Y: dn * d})
			}
			for c := range cfgs {
				v := Verify(rep, cov, toas[:m], nil, sig[:m], rangeFn, cfgs[c].vp)
				res[c].tdoa[oi].add(t.icao, t.txS, now, v.Verdict)
				if mlatOK {
					res[c].mlat[oi].add(t.icao, t.txS, now, CompareMLAT(rep, cov, f.ECEF, f.CovECEF, cfgs[c].cp).Verdict)
				}
			}
		}
	})
	h.run(tb, dur)

	// ground spoofers: 6 sites inside the network, two fake aircraft each,
	// 2 position messages per second for the run's duration
	rs := h.n.Receivers()
	for s := 0; s < 6; s++ {
		c := rs[rng.IntN(len(rs))]
		slat, slon := c.Lat+rng.Float64()*0.6-0.3, c.Lon+rng.Float64()*0.8-0.4
		src := geo.ToECEF(slat, slon, 300+rng.Float64()*200)
		for a := 0; a < 2; a++ {
			key := uint32(0xF00000 + s*16 + a)
			brg := rng.Float64() * 2 * math.Pi
			dist := 30e3 + rng.Float64()*120e3
			fr0 := geo.NewFrameECEF(geo.ToECEF(slat, slon, 0))
			p0 := fr0.FromENU(geo.Vec3{X: math.Sin(brg) * dist, Y: math.Cos(brg) * dist})
			l0 := geo.FromECEF(p0)
			fake := geo.ToECEF(l0.Lat, l0.Lon, 10668+47)
			frF := geo.NewFrameECEF(fake)
			vel := frF.Unrotate(geo.Vec3{X: 230 * math.Cos(brg), Y: -230 * math.Sin(brg)})
			for k := 0; k < int(dur.Seconds()*2); k++ {
				tS := float64(k)/2 + rng.Float64()*0.1
				rep := fake.Add(vel.Scale(tS))
				m := 0
				for i := range rs {
					if m == 16 {
						break
					}
					p := h.rxPos[i]
					if p.Dist(src) > 150e3 || p.Dist(src) > geo.RadioHorizon(h.rxHAE[i], 400, 0) || rng.Float64() < 0.2 {
						continue
					}
					sg := math.Sqrt(h.rxSig[i]*h.rxSig[i]+h.syncSig[i]*h.syncSig[i]+100) * 1e-9
					toas[m] = TOA{Pos: p, T: 0.3 + atm.Range(p, src)/geo.C + rng.NormFloat64()*sg}
					sig[m] = sg
					m++
				}
				if m < 2 {
					continue
				}
				cov := RefCovECEF(rep, vel, 9, 0.15, 30)
				nowNs := int64(tS * 1e9)
				solved := false
				if m >= 4 {
					var pr solve.Problem
					var st [16]solve.Station
					for i := 0; i < m; i++ {
						st[i] = solve.Station{Pos: toas[i].Pos, TOA: toas[i].T, Sigma: sig[i]}
					}
					pr.Stations = st[:m]
					pr.Alt = solve.Altitude{Valid: true, HAE: geo.FromECEF(rep).H, Sigma: 30}
					solved = sv.Solve(&pr, &f) == nil && MLATComparable(&f)
				}
				for c := range cfgs {
					res[c].ground.add(key, tS, nowNs, Verify(rep, cov, toas[:m], nil, sig[:m], rangeFn, cfgs[c].vp).Verdict)
					if solved {
						// (no consistent solution at the claimed height: the
						// MLAT check cannot speak; the TDOA check covers it)
						res[c].groundMLAT.add(key, tS, nowNs, CompareMLAT(rep, cov, f.ECEF, f.CovECEF, cfgs[c].cp).Verdict)
					}
				}
			}
		}
	}
	return res
}

func phase2Cfg() spoofCfg { return spoofCfg{name: "phase2"} }
func phase4Cfg() spoofCfg {
	return spoofCfg{name: "phase4", vp: DefaultVerifyParams(), cp: DefaultCompareParams(), onlyConfirmed: true}
}

// TestSpoofSimnet validates the verification thresholds on simnet with
// synthetic spoofers. With the phase-4 defaults: no honest aircraft may
// become suspect and fewer than 0.5 % of honest messages may be
// contradicted (TDOA and MLAT); offsets of 1 km and more must be caught
// for ≥ 95 % of aircraft by TDOA, and by the MLAT comparison for ≥ 85 % at
// 1 km and ≥ 95 % from 2 km; every ground spoofer must be caught by TDOA.
// The table also shows the phase-2 parameters, two neighbouring MDO limits
// and a lower ContradictFrac for comparison.
func TestSpoofSimnet(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	cfgs := []spoofCfg{phase2Cfg(),
		{name: "dropOne", vp: VerifyParams{DropOne: true}},
		phase4Cfg(),
		{name: "mdo300", vp: VerifyParams{DropOne: true, MaxConfirmMDOM: 300}, cp: CompareParams{MaxConfirmMDOM: 300}, onlyConfirmed: true},
		{name: "mdo1000", vp: VerifyParams{DropOne: true, MaxConfirmMDOM: 1000}, cp: CompareParams{MaxConfirmMDOM: 1000}, onlyConfirmed: true},
		{name: "frac0.2", vp: DefaultVerifyParams(), cp: DefaultCompareParams(), onlyConfirmed: true, frac: 0.2},
	}
	all := spoofRun(t, 11, 3*time.Minute, cfgs)
	for c, res := range all {
		var b strings.Builder
		fmt.Fprintf(&b, "parameters %s:", cfgs[c].name)
		for i, d := range spoofOffsets {
			fmt.Fprintf(&b, "\n%s\n%s", res.tdoa[i].line(fmt.Sprintf("TDOA %5.0f m", d)), res.mlat[i].line(fmt.Sprintf("MLAT %5.0f m", d)))
		}
		fmt.Fprintf(&b, "\n%s\n%s", res.ground.line("TDOA ground"), res.groundMLAT.line("MLAT ground"))
		t.Log(b.String())
	}
	res := all[2] // phase 4 defaults
	for _, s := range []*spoofScore{res.tdoa[0], res.mlat[0]} {
		if len(s.suspectAt) != 0 {
			t.Errorf("honest aircraft flagged suspect: %d of %d", len(s.suspectAt), len(s.aircraft))
		}
		if fa := float64(s.contra) / float64(max(s.msgs, 1)); fa > 0.005 {
			t.Errorf("honest per-message contradiction rate %.4f", fa)
		}
	}
	for i, d := range spoofOffsets {
		if d < 1000 {
			continue
		}
		if pd := res.tdoa[i].caught(); pd < 0.95 {
			t.Errorf("offset %.0f m: %.1f %% of aircraft caught by TDOA", d, 100*pd)
		}
		if pd, want := res.mlat[i].caught(), map[bool]float64{true: 0.95, false: 0.85}[d >= 2000]; pd < want {
			t.Errorf("offset %.0f m: %.1f %% of aircraft caught by MLAT", d, 100*pd)
		}
	}
	if len(res.ground.suspectAt) != len(res.ground.aircraft) || len(res.ground.aircraft) == 0 {
		t.Errorf("ground spoofers caught %d of %d", len(res.ground.suspectAt), len(res.ground.aircraft))
	}
}

// TestCompareMLAT checks the χ² arithmetic of the MLAT comparison.
func TestCompareMLAT(t *testing.T) {
	rep := geo.ToECEF(50, 9, 10000)
	fr := geo.NewFrameECEF(rep)
	covE := [9]float64{100, 0, 0, 0, 100, 0, 0, 0, 100} // 10 m per axis, ENU
	cov := fr.CovToECEF(covE)
	// 20 m east: d² = 400 / 200 = 2 → confirmed
	r := CompareMLAT(rep, cov, fr.FromENU(geo.Vec3{X: 20}), cov, CompareParams{})
	if r.Verdict != VerdictConfirmed || math.Abs(r.Chi2-2) > 1e-6 || math.Abs(r.MaxBiasM-20) > 1e-6 {
		t.Fatalf("20 m: %+v", r)
	}
	// MDO = sqrt(λ₀(2)·200)
	if want := math.Sqrt(lam0[2] * 200); math.Abs(r.MDOM-want) > 1e-9 || lam0[2] < 20 || lam0[2] > 35 {
		t.Fatalf("MDO %v want %v (λ₀ %v)", r.MDOM, want, lam0[2])
	}
	// 250 m: χ² huge but below MinBias → none
	if r := CompareMLAT(rep, cov, fr.FromENU(geo.Vec3{Y: 250}), cov, CompareParams{}); r.Verdict != VerdictNone {
		t.Fatalf("250 m: %v", r.Verdict)
	}
	// 400 m → contradicted
	if r := CompareMLAT(rep, cov, fr.FromENU(geo.Vec3{Y: 400}), cov, CompareParams{}); r.Verdict != VerdictContradicted {
		t.Fatalf("400 m: %v", r.Verdict)
	}
	// big covariance: 400 m within σ → confirmed without a power
	// requirement, consistent with MaxConfirmMDOM 500
	bigE := [9]float64{1e5, 0, 0, 0, 1e5, 0, 0, 0, 100}
	if r := CompareMLAT(rep, fr.CovToECEF(bigE), fr.FromENU(geo.Vec3{Y: 400}), cov, CompareParams{}); r.Verdict != VerdictConfirmed {
		t.Fatalf("400 m with σ 316 m: %v", r.Verdict)
	}
	if r := CompareMLAT(rep, fr.CovToECEF(bigE), fr.FromENU(geo.Vec3{Y: 400}), cov, DefaultCompareParams()); r.Verdict != VerdictConsistent {
		t.Fatalf("400 m with σ 316 m, MDO gate: %v (MDO %.0f)", r.Verdict, r.MDOM)
	}
	// RefCovECEF: NACp 9, no latency → σ = 30/2.4477 per axis
	c := RefCovECEF(rep, geo.Vec3{}, 9, 0.15, 30)
	ce := fr.CovToENU(c)
	if s := math.Sqrt(ce[0]); math.Abs(s-30/2.4477) > 1e-6 || math.Abs(math.Sqrt(ce[8])-30) > 1e-6 {
		t.Fatalf("RefCovECEF σE %v σU %v", s, math.Sqrt(ce[8]))
	}
	// latency adds along-track: 200 m/s north, 0.15 s → σN² += 900
	c = RefCovECEF(rep, fr.Unrotate(geo.Vec3{Y: 200}), 9, 0.15, 30)
	ce = fr.CovToENU(c)
	if math.Abs(ce[4]-ce[0]-900) > 1e-6 {
		t.Fatalf("latency term %v", ce[4]-ce[0])
	}
	if hi, ok := VerifyChi2Thresholds(0, 2); math.Abs(hi+2*math.Log(1e-4)) > 1e-3 || math.Abs(ok-chi2Two999) > 1e-3 {
		t.Fatalf("thresholds %v %v", hi, ok)
	}
}

// TestVerifyPhase4 checks the leave-one-out attribution and the MDO on a
// hand-built geometry.
func TestVerifyPhase4(t *testing.T) {
	atm := geo.DefaultAtmosphere()
	rangeFn := func(rx, ac geo.Vec3) float64 { return atm.Range(rx, ac) }
	ac := geo.ToECEF(50, 9, 10000)
	var toas []TOA
	var sig []float64
	for i := 0; i < 7; i++ {
		a := float64(i) / 7 * 2 * math.Pi
		p := geo.ToECEF(50+1.2*math.Cos(a), 9+1.8*math.Sin(a), 200)
		toas = append(toas, TOA{Pos: p, T: 0.1 + atm.Range(p, ac)/geo.C})
		sig = append(sig, 20e-9)
	}
	cov := RefCovECEF(ac, geo.Vec3{}, 9, 0, 30)
	r := Verify(ac, cov, toas, nil, sig, rangeFn, DefaultVerifyParams())
	if r.Verdict != VerdictConfirmed || r.Dof != 6 || r.Culprit != -1 || !(r.MDOM > 10 && r.MDOM < 200) {
		t.Fatalf("clean: %+v", r)
	}
	// one station 3 µs late: phase 2 contradicts, phase 4 blames row 4
	toas[4].T += 3e-6
	if v, _, _, _ := VerifyTDOA(ac, cov, toas, nil, sig, rangeFn, VerifyParams{}); v != VerdictContradicted {
		t.Fatalf("phase 2: %v", v)
	}
	r = Verify(ac, cov, toas, nil, sig, rangeFn, DefaultVerifyParams())
	if r.Verdict != VerdictNone || r.Culprit != 4 {
		t.Fatalf("drop-one: %+v", r)
	}
	toas[4].T -= 3e-6
	// two stations: an offset along the hyperbola is invisible → MDO = +Inf,
	// never confirmed (three stations needed anyway)
	r = Verify(ac, cov, toas[:2], nil, sig[:2], rangeFn, DefaultVerifyParams())
	if r.Verdict != VerdictConsistent || !math.IsInf(r.MDOM, 1) {
		t.Fatalf("two stations: %+v", r)
	}
	// a 5 km offset is contradicted with phase 4 too (no single culprit)
	frA := geo.NewFrameECEF(ac)
	off := frA.FromENU(geo.Vec3{X: 5000})
	if r := Verify(off, cov, toas, nil, sig, rangeFn, DefaultVerifyParams()); r.Verdict != VerdictContradicted {
		t.Fatalf("5 km offset: %+v", r)
	}
	// Verifier with OnlyConfirmedResets: consistent checks do not clear
	v := NewVerifier()
	v.OnlyConfirmedResets = true
	v.Record(1, 1e9, VerdictContradicted)
	v.Record(1, 2e9, VerdictConsistent)
	v.Record(1, 3e9, VerdictContradicted)
	if !v.Record(1, 4e9, VerdictContradicted) {
		t.Fatal("suspect expected after 3 contradictions with a consistent check between")
	}
	v.Record(1, 5e9, VerdictConsistent)
	if s, _ := v.State(1, 5e9); !s {
		t.Fatal("consistent check cleared suspect")
	}
	// one confirmation in the same window does not clear (3 of 4 checks
	// contradicted); three in a fresh window do
	v.Record(1, 6e9, VerdictConfirmed)
	if s, _ := v.State(1, 6e9); !s {
		t.Fatal("a single confirmation cleared suspect")
	}
	for _, at := range []int64{70e9, 71e9, 72e9} {
		v.Record(1, at, VerdictConfirmed)
	}
	if s, ver := v.State(1, 72e9); s || !ver {
		t.Fatal("three confirmations in a fresh window did not clear")
	}
}
