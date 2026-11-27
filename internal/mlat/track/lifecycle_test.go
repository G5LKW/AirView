package track

import (
	"math"
	"sync"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// lcSetup returns a synthetic network, a straight path and a helper that
// makes fixes and clusters at time t.
type lcEnv struct {
	s  *synth
	pa *path
}

func newLC() *lcEnv {
	st := stationsAround(50, 8, 6, 80e3, true)
	return &lcEnv{s: newSynth(9, st, 50), pa: straightPath(50, 8, -20e3, 10e3, 9500, 200, 50)}
}

func (l *lcEnv) fix(icao uint32, t float64) Fix {
	p, _ := l.pa.at(t)
	c := l.s.cluster(icao, t, p, nil, geo.FromECEF(p).H, 25)
	return fixFrom(icao, l.s.tNs(t), solveTOA(c.TOAs, c.AltHAE, 25, &l.s.atm), len(c.TOAs))
}

func (l *lcEnv) cluster(icao uint32, t float64, use []int) Cluster {
	p, _ := l.pa.at(t)
	return l.s.cluster(icao, t, p, use, geo.FromECEF(p).H, 25)
}

func TestLifecycle(t *testing.T) {
	l := newLC()
	tr := New(Params{})
	var est Estimate
	c := l.cluster(1, 0, nil)
	if r := tr.Cluster(&c, &est); r != ResNoTrack || est.Status != StatusNone {
		t.Fatalf("cluster without track: %v %v", r, est.Status)
	}
	f := l.fix(1, 0)
	if r := tr.Fix(&f, &est); r != ResCreated || est.Status != StatusTentative {
		t.Fatalf("first fix: %v %v", r, est.Status)
	}
	c2 := l.cluster(1, 0.5, []int{0, 1})
	if r := tr.Cluster(&c2, &est); r != ResNotConfirmed {
		t.Fatalf("2-station cluster on a tentative track: %v", r)
	}
	f = l.fix(1, 1)
	if r := tr.Fix(&f, &est); r != ResApplied || est.Status != StatusConfirmed {
		t.Fatalf("second fix: %v %v", r, est.Status)
	}
	for k := 3; k <= 40; k++ {
		c := l.cluster(1, float64(k)*0.5, nil)
		if r := tr.Cluster(&c, &est); r != ResApplied {
			t.Fatalf("cluster %d: %v", k, r)
		}
	}
	// 2-station maintenance is accepted once confirmed
	c2 = l.cluster(1, 20.5, []int{0, 1})
	if r := tr.Cluster(&c2, &est); r != ResApplied {
		t.Fatalf("2-station cluster on a confirmed track: %v", r)
	}
	// a late copy within MaxLagS is applied out of sequence, older is dropped
	c3 := l.cluster(1, 19.2, nil)
	if r := tr.Cluster(&c3, &est); r != ResApplied || tr.Stats().OOSM == 0 {
		t.Fatalf("late cluster: %v", r)
	}
	c4 := l.cluster(1, 15, nil)
	if r := tr.Cluster(&c4, &est); r != ResTooLate {
		t.Fatalf("too-late cluster: %v", r)
	}
	// 1 Hz output by Sweep, coasting, deletion after CoastS
	n := 0
	tr.Sweep(l.s.tNs(30), func(e *Estimate) {
		n++
		p, _ := l.pa.at(30)
		if e.ECEF.Dist(p) > 300 || e.TNs != l.s.tNs(30) {
			t.Errorf("predicted estimate off by %.0f m", e.ECEF.Dist(p))
		}
	})
	if n != 1 {
		t.Fatalf("sweep emitted %d estimates", n)
	}
	tr.Sweep(l.s.tNs(20.5+61), nil)
	if tr.Len() != 0 {
		t.Fatal("track not deleted after coasting 60 s")
	}

	// a lone tentative hypothesis dies after the confirmation window
	f = l.fix(2, 0)
	tr.Fix(&f, nil)
	tr.Sweep(l.s.tNs(5), nil)
	if tr.Len() != 1 {
		t.Fatal("tentative track deleted too early")
	}
	tr.Sweep(l.s.tNs(10.5), nil)
	if tr.Len() != 0 {
		t.Fatal("unconfirmed track survived the confirmation window")
	}
	// σ limit
	tr2 := New(Params{MaxSigmaHM: 150})
	for k := range 3 {
		f := l.fix(3, float64(k))
		tr2.Fix(&f, nil)
	}
	tr2.Sweep(l.s.tNs(5), nil)
	if tr2.Len() != 1 {
		t.Fatal("track deleted too early by the σ limit")
	}
	tr2.Sweep(l.s.tNs(50), nil)
	if tr2.Len() != 0 {
		t.Fatal("track whose σ exceeds MaxSigmaHM survived")
	}
	s := tr.Stats()
	if s.Created < 2 || s.Confirmations < 1 || s.Deleted < 2 {
		t.Fatalf("stats: %+v", s)
	}
}

// TestRespawn: an ICAO whose fixes jump 40 km away (re-used address, or a
// track that locked onto garbage) is re-acquired from the new fixes.
func TestRespawn(t *testing.T) {
	l := newLC()
	tr := New(Params{})
	var est Estimate
	for k := range 10 {
		f := l.fix(7, float64(k))
		tr.Fix(&f, &est)
	}
	other := straightPath(50, 8, 30e3, -25e3, 6000, -150, 0)
	for k := 10; k < 20; k++ {
		p, _ := other.at(float64(k))
		c := l.s.cluster(7, float64(k), p, nil, geo.FromECEF(p).H, 25)
		f := fixFrom(7, l.s.tNs(float64(k)), solveTOA(c.TOAs, c.AltHAE, 25, &l.s.atm), 7)
		tr.Fix(&f, &est)
	}
	p, _ := other.at(19)
	if est.Status != StatusConfirmed || est.ECEF.Dist(p) > 300 || est.Hypotheses != 1 {
		t.Fatalf("not re-acquired: %v, %.0f m off, %d hypotheses (%+v)", est.Status, est.ECEF.Dist(p), est.Hypotheses, tr.Stats())
	}
	if tr.Stats().Respawns == 0 {
		t.Fatal("no respawn counted")
	}
}

func TestInvalidInputs(t *testing.T) {
	tr := New(Params{})
	var est Estimate
	if r := tr.Fix(&Fix{ICAO: 1, NHyp: 0}, &est); r != ResIgnored {
		t.Fatalf("NHyp 0: %v", r)
	}
	if r := tr.Fix(&Fix{ICAO: 1, NHyp: 1, Hyp: [2]FixHyp{{ECEF: geo.Vec3{X: math.NaN()}}}}, &est); r != ResIgnored {
		t.Fatalf("NaN fix: %v", r)
	}
	if r := tr.Fix(&Fix{ICAO: 1, NHyp: 1}, &est); r != ResIgnored {
		t.Fatalf("fix at the earth centre: %v", r)
	}
	l := newLC()
	f := l.fix(1, 0)
	f.Hyp[0].Cov = [9]float64{} // unusable covariance: default 500 m is used
	if r := tr.Fix(&f, &est); r != ResCreated {
		t.Fatalf("fix without covariance: %v", r)
	}
	c := l.cluster(1, 0.5, []int{0})
	if r := tr.Cluster(&c, &est); r != ResIgnored {
		t.Fatalf("single-station cluster: %v", r)
	}
	c = l.cluster(1, 0.5, nil)
	for i := range c.TOAs {
		c.TOAs[i].Sigma = 0
	}
	if r := tr.Cluster(&c, &est); r != ResIgnored {
		t.Fatalf("cluster without σ: %v", r)
	}
	if r := tr.Velocity(&Velocity{ICAO: 99, SigmaH: 1}, &est); r != ResNoTrack {
		t.Fatalf("velocity without track: %v", r)
	}
	if r := tr.Altitude(&Altitude{ICAO: 1, Sigma: 0}, &est); r != ResNoTrack {
		t.Fatalf("altitude without σ: %v", r)
	}
}

// TestClusterCovariance: an explicit full TOA covariance equal to
// diag(σ²) gives the same result as the σ form; correlated TOAs are
// accepted.
func TestClusterCovariance(t *testing.T) {
	l := newLC()
	a, b := New(Params{}), New(Params{})
	for k := range 2 {
		f := l.fix(1, float64(k))
		a.Fix(&f, nil)
		b.Fix(&f, nil)
	}
	var ea, eb Estimate
	for k := 4; k < 30; k++ {
		c := l.cluster(1, float64(k)*0.5, nil)
		m := len(c.TOAs)
		c2 := c
		c2.TOAs = append([]TOA(nil), c.TOAs...)
		c2.Cov = make([]float64, m*m)
		for i := range m {
			c2.Cov[i*m+i] = c.TOAs[i].Sigma * c.TOAs[i].Sigma
		}
		a.Cluster(&c, &ea)
		b.Cluster(&c2, &eb)
	}
	if ea.ECEF.Dist(eb.ECEF) > 1e-6 || math.Abs(ea.SigmaH-eb.SigmaH) > 1e-9 {
		t.Fatalf("Cov form differs: %v vs %v", ea.ECEF, eb.ECEF)
	}
	// a common (perfectly correlated) clock error cancels in the TDOAs
	c := l.cluster(1, 15, nil)
	m := len(c.TOAs)
	c.Cov = make([]float64, m*m)
	for i := range m {
		for j := range m {
			c.Cov[i*m+j] = 1e-14 // 100 ns common
		}
		c.Cov[i*m+i] += c.TOAs[i].Sigma * c.TOAs[i].Sigma
		c.TOAs[i].T += 1e-7
	}
	if r := a.Cluster(&c, &ea); r != ResApplied {
		t.Fatalf("correlated cluster: %v", r)
	}
}

func TestMLATFixConversion(t *testing.T) {
	l := newLC()
	tr := New(Params{})
	var est Estimate
	for k := range 2 {
		f := l.fix(0xABCDEF, float64(k))
		tr.Fix(&f, &est)
	}
	for k := 4; k < 20; k++ {
		c := l.cluster(0xABCDEF, float64(k)*0.5, nil)
		tr.Cluster(&c, &est)
	}
	f := est.MLATFix()
	if f.ICAO != 0xABCDEF || f.Flags&model.FixFlagTracked == 0 || f.Flags&model.FixFlagAltConstrained == 0 ||
		f.Vel == nil || f.Grade != model.GradeA || f.NBucket != model.NBucket5to8 || f.TUnixNs != est.TNs {
		t.Fatalf("MLATFix: %+v (σh %.1f, dof %d)", f, est.SigmaH, est.MaxDOF)
	}
	if math.Abs(float64(f.Cov[0])-est.Cov[0]) > 1e-3*est.Cov[0] || math.Abs(float64(f.Cov[3])-est.Cov[4]) > 1e-3*est.Cov[4] {
		t.Fatal("covariance packing")
	}
	back := FixFromMLAT(&f, 7, 4)
	if back.Hyp[0].ECEF.Dist(est.ECEF) > 1e-6 || math.Abs(back.Hyp[0].Cov[5]-est.Cov[5]) > 1e-3*math.Abs(est.Cov[5])+1e-3 {
		t.Fatal("FixFromMLAT round trip")
	}
	// velocity in local ENU agrees with the path (200, 50)
	if math.Abs(est.VelENU[0]-200) > 5 || math.Abs(est.VelENU[1]-50) > 5 {
		t.Fatalf("velocity %v", est.VelENU)
	}
	if est.Ellipse.SemiMajor < est.Ellipse.SemiMinor || est.Ellipse.SemiMajor > est.SigmaH+1e-9 {
		t.Fatalf("ellipse %+v σh %.1f", est.Ellipse, est.SigmaH)
	}
}

// TestShardedMatchesTracker: concurrent use of Sharded gives exactly the
// results of a single Tracker fed the same per-ICAO sequences.
func TestShardedMatchesTracker(t *testing.T) {
	const nAC = 32
	l := newLC()
	type item struct {
		fix *Fix
		cl  *Cluster
	}
	seqs := make([][]item, nAC)
	for a := range nAC {
		icao := uint32(0x100 + a)
		for k := range 2 {
			f := l.fix(icao, float64(k))
			seqs[a] = append(seqs[a], item{fix: &f})
		}
		for k := 4; k < 40; k++ {
			c := l.cluster(icao, float64(k)*0.5, nil)
			seqs[a] = append(seqs[a], item{cl: &c})
		}
	}
	ref := New(Params{})
	for a := range nAC {
		for _, it := range seqs[a] {
			if it.fix != nil {
				ref.Fix(it.fix, nil)
			} else {
				ref.Cluster(it.cl, nil)
			}
		}
	}
	sh := NewSharded(Params{}, 8)
	var wg sync.WaitGroup
	for a := range nAC {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, it := range seqs[a] {
				if it.fix != nil {
					sh.Fix(it.fix, nil)
				} else {
					sh.Cluster(it.cl, nil)
				}
			}
		}()
	}
	wg.Wait()
	if sh.Len() != nAC || sh.Stats().Confirmed != nAC {
		t.Fatalf("sharded: %d tracks, %+v", sh.Len(), sh.Stats())
	}
	for a := range nAC {
		var e1, e2 Estimate
		ref.Get(uint32(0x100+a), &e1)
		sh.Get(uint32(0x100+a), &e2)
		if e1.ECEF != e2.ECEF || e1.Cov != e2.Cov {
			t.Fatalf("ICAO %d differs between Sharded and Tracker", a)
		}
	}
	n := 0
	sh.Sweep(l.s.tNs(21), func(*Estimate) { n++ })
	if n != nAC {
		t.Fatalf("sweep emitted %d", n)
	}
	sh.Delete(0x100)
	if sh.Len() != nAC-1 {
		t.Fatal("Delete")
	}
}
