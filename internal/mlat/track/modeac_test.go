package track

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Synthetic Mode A/C scenario: targets on straight (optionally climbing)
// paths, interrogated by several SSR radars with their own antenna periods.
// Every sweep gives a dwell of replies 4 ms apart that alternate between the
// identity (Mode A) and the altitude (Mode C) code. Each reply becomes a
// solver fix (noisy truth with its covariance), a 2-3 station TOA cluster,
// or nothing. FRUIT adds isolated false fixes with random codes.

type acTruth struct {
	p0     geo.Vec3 // ENU at t=0
	v      geo.Vec3 // ENU m/s
	squawk uint16
}

type acScenario struct {
	fr      geo.Frame
	targets []acTruth
	radars  []float64 // antenna periods (s)
	phase   [][]float64
	dwell   int // replies per dwell
	pFix    float64
	pClus   float64
	sigH    float64 // fix horizontal σ (m)
	sigV    float64
	fruitHz float64
	durS    float64
	t0Ns    int64
	sites   []geo.Vec3
	rng     *rand.Rand
}

func newACScenario(seed uint64, n int) *acScenario {
	s := &acScenario{fr: geo.NewFrame(geo.LLH{Lat: 51, Lon: 9}), radars: []float64{4.8, 6, 10},
		dwell: 10, pFix: 0.5, pClus: 0.3, sigH: 40, sigV: 120, durS: 180, t0Ns: 1_780_000_000e9,
		rng: rand.New(rand.NewPCG(seed, 99))}
	for k := range 8 {
		a := float64(k) / 8 * 2 * math.Pi
		s.sites = append(s.sites, s.fr.FromENU(geo.Vec3{X: 80e3 * math.Cos(a), Y: 80e3 * math.Sin(a), Z: 100}))
	}
	for i := 0; i < n; i++ {
		hd := s.rng.Float64() * 2 * math.Pi
		sp := 80 + 160*s.rng.Float64()
		s.targets = append(s.targets, acTruth{
			p0:     geo.Vec3{X: (s.rng.Float64() - 0.5) * 120e3, Y: (s.rng.Float64() - 0.5) * 120e3, Z: 1000 + 9000*s.rng.Float64()},
			v:      geo.Vec3{X: sp * math.Sin(hd), Y: sp * math.Cos(hd), Z: (s.rng.Float64() - 0.5) * 10},
			squawk: uint16(s.rng.IntN(0o7777)),
		})
	}
	return s
}

func (s *acScenario) pos(i int, tS float64) geo.Vec3 {
	tr := &s.targets[i]
	return s.fr.FromENU(tr.p0.Add(tr.v.Scale(tS)))
}

// altCode is the Mode C code of target i at tS (pressure altitude taken as
// the height above the ellipsoid minus 50 m, a geoid-like offset).
func (s *acScenario) altCode(i int, tS float64) uint16 {
	h := geo.FromECEF(s.pos(i, tS)).H - 50
	r, _ := ACFromAltitudeFt(int(math.Round(h / 0.3048)))
	return r
}

type acEvent struct {
	tS     float64
	target int // -1: FRUIT
	code   uint16
}

func (s *acScenario) events() []acEvent {
	var ev []acEvent
	if len(s.phase) == 0 {
		for range s.targets {
			var ph []float64
			for _, P := range s.radars {
				ph = append(ph, s.rng.Float64()*P)
			}
			s.phase = append(s.phase, ph)
		}
	}
	for i := range s.targets {
		for r, P := range s.radars {
			for t := s.phase[i][r]; t < s.durS; t += P {
				for k := 0; k < s.dwell; k++ {
					tt := t + float64(k)*0.004
					code := ACFromSquawk(s.targets[i].squawk)
					if k%2 == 1 {
						code = s.altCode(i, tt)
					}
					ev = append(ev, acEvent{tS: tt, target: i, code: code})
				}
			}
		}
	}
	for t := 0.0; s.fruitHz > 0 && t < s.durS; t += s.rng.ExpFloat64() / s.fruitHz {
		ev = append(ev, acEvent{tS: t, target: -1, code: uint16(s.rng.IntN(1 << 13))})
	}
	slices.SortStableFunc(ev, func(a, b acEvent) int {
		switch {
		case a.tS < b.tS:
			return -1
		case a.tS > b.tS:
			return 1
		}
		return 0
	})
	return ev
}

func (s *acScenario) ns(tS float64) int64 { return s.t0Ns + int64(tS*1e9) }

// fix returns a noisy fix of position p with the scenario's covariance.
func (s *acScenario) fix(p geo.Vec3, tS float64) Fix {
	lf := geo.NewFrame(geo.FromECEF(p))
	n := geo.Vec3{X: s.sigH / math.Sqrt2 * s.rng.NormFloat64(), Y: s.sigH / math.Sqrt2 * s.rng.NormFloat64(), Z: s.sigV * s.rng.NormFloat64()}
	q := lf.FromENU(lf.ToENU(p).Add(n))
	h2 := s.sigH * s.sigH / 2
	cov := lf.CovToECEF([9]float64{h2, 0, 0, 0, h2, 0, 0, 0, s.sigV * s.sigV})
	return Fix{TNs: s.ns(tS), NHyp: 1, NStations: 6, DOF: 2, Hyp: [2]FixHyp{{ECEF: q, Cov: cov}}}
}

// cluster returns a TOA cluster of m random sites for a transmission from p.
func (s *acScenario) cluster(p geo.Vec3, tS float64, m int) Cluster {
	idx := s.rng.Perm(len(s.sites))[:m]
	c := Cluster{T0Ns: s.ns(tS)}
	for _, k := range idx {
		r := s.sites[k].Dist(p)
		c.TOAs = append(c.TOAs, TOA{Station: uint32(k), Pos: s.sites[k], T: r/geo.C + 30e-9*s.rng.NormFloat64(), Sigma: 30e-9})
	}
	return c
}

type acRun struct {
	ids        map[uint32]map[int]int // target id -> truth -> fixes
	errs       []float64              // horizontal errors of shown targets at sweeps
	falseShown map[uint32]bool
	shownOf    map[int]map[uint32]bool // truth -> shown ids
	last       []ACEstimate
	results    map[Result]int
}

// run feeds the scenario into a fresh tracker; 1 Hz sweeps evaluate every
// shown target against the nearest truth.
func (s *acScenario) run(t *testing.T, p ACParams) (*ACTracker, *acRun) {
	a := NewACTracker(p)
	r := &acRun{ids: map[uint32]map[int]int{}, falseShown: map[uint32]bool{}, shownOf: map[int]map[uint32]bool{},
		results: map[Result]int{}}
	next := 1.0
	for _, e := range s.events() {
		for e.tS >= next {
			s.sweep(a, r, next)
			next++
		}
		var p geo.Vec3
		if e.target >= 0 {
			if u := s.rng.Float64(); u < s.pFix {
				// solved below
			} else if u < s.pFix+s.pClus {
				c := s.cluster(s.pos(e.target, e.tS), e.tS, 2+s.rng.IntN(2))
				res := a.Cluster(e.code, &c)
				r.results[res.Res]++
				if res.Res == ResApplied {
					r.note(res.ID, e.target)
				}
				continue
			} else {
				continue
			}
			p = s.pos(e.target, e.tS)
		} else {
			p = s.fr.FromENU(geo.Vec3{X: (s.rng.Float64() - 0.5) * 200e3, Y: (s.rng.Float64() - 0.5) * 200e3, Z: 10000 * s.rng.Float64()})
		}
		f := s.fix(p, e.tS)
		res := a.Fix(e.code, &f)
		r.results[res.Res]++
		if res.ID != 0 && (res.Res == ResApplied || res.Res == ResCreated) {
			r.note(res.ID, e.target)
		}
	}
	return a, r
}

func (r *acRun) note(id uint32, target int) {
	m := r.ids[id]
	if m == nil {
		m = map[int]int{}
		r.ids[id] = m
	}
	m[target]++
}

func (s *acScenario) sweep(a *ACTracker, r *acRun, tS float64) {
	r.last = r.last[:0]
	a.Sweep(s.ns(tS), func(e *ACEstimate) {
		r.last = append(r.last, *e)
		best, bi := math.Inf(1), -1
		for i := range s.targets {
			if d := e.ECEF.Dist(s.pos(i, tS)); d < best {
				best, bi = d, i
			}
		}
		if best > 3000 {
			if !r.falseShown[e.ICAO] && testing.Verbose() {
				println("false", e.ICAO&0xFFFFFF, int(tS), int(best), e.Fixes, e.Clusters, int(e.SigmaH), fmtIDs(r.ids[e.ICAO]))
			}
			r.falseShown[e.ICAO] = true
			return
		}
		if r.shownOf[bi] == nil {
			r.shownOf[bi] = map[uint32]bool{}
		}
		r.shownOf[bi][e.ICAO] = true
		lf := geo.NewFrame(geo.FromECEF(s.pos(bi, tS)))
		d := lf.ToENU(e.ECEF)
		r.errs = append(r.errs, math.Hypot(d.X, d.Y))
	})
}

func acPct(v []float64, q float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	w := slices.Clone(v)
	slices.Sort(w)
	return w[min(len(w)-1, int(q*float64(len(w))))]
}

// One target: one identity, squawk and altitude decoded, error near the
// fix noise, IDs flagged as synthetic.
func TestACTrackerSingle(t *testing.T) {
	s := newACScenario(1, 1)
	s.targets[0].v.Z = 5 // climbing: the Mode C code changes
	a, r := s.run(t, ACParams{})
	if len(r.ids) != 1 {
		t.Fatalf("%d targets for one aircraft: %v", len(r.ids), r.ids)
	}
	if len(r.last) != 1 {
		t.Fatalf("%d shown at the end", len(r.last))
	}
	e := r.last[0]
	if !IsACID(e.ICAO) || e.ICAO&0xFFFFFF == 0 {
		t.Fatalf("id %x", e.ICAO)
	}
	if !e.SquawkOK || ACSquawk(e.Squawk) != s.targets[0].squawk {
		t.Fatalf("squawk %04o ok=%v want %04o", ACSquawk(e.Squawk), e.SquawkOK, s.targets[0].squawk)
	}
	wantFt, _ := ACAltitudeFt(s.altCode(0, s.durS))
	if !e.AltOK || math.Abs(float64(e.AltFt-wantFt)) > 200 {
		t.Fatalf("altitude %d ok=%v want %d", e.AltFt, e.AltOK, wantFt)
	}
	p50, p95 := acPct(r.errs, 0.5), acPct(r.errs, 0.95)
	t.Logf("single: P50 %.0f m P95 %.0f m, %d samples, grade %s, codes %d, results %v, stats %+v",
		p50, p95, len(r.errs), model.GradeString(e.Grade), e.Codes, r.results, a.Stats())
	if p50 > 60 || p95 > 200 {
		t.Fatalf("error P50 %.0f P95 %.0f", p50, p95)
	}
	if e.Grade == model.GradeNone || e.Grade > model.GradeB {
		t.Fatalf("grade %d", e.Grade)
	}
	if a.Stats().ClusterApplied == 0 {
		t.Fatal("no cluster updates")
	}
	var g ACEstimate
	if !a.Get(e.ICAO, &g) || g.ICAO != e.ICAO || !a.Shown(e.ICAO) {
		t.Fatal("Get/Shown")
	}
}

// A crowd: 30 targets in 120 km, a third of them squawking 7000 (VFR),
// plus FRUIT. Every target is shown, few identities per target, no
// identity mixes two aircraft for long, no false targets.
func TestACTrackerCrowd(t *testing.T) {
	s := newACScenario(2, 30)
	for i := 0; i < 10; i++ {
		s.targets[i].squawk = 0o7000
	}
	// two 7000 targets crossing 2000 ft apart
	s.targets[0].p0, s.targets[0].v = geo.Vec3{X: -10e3, Y: 0, Z: 1500}, geo.Vec3{X: 100}
	s.targets[1].p0, s.targets[1].v = geo.Vec3{X: 10e3, Y: 0, Z: 2100}, geo.Vec3{X: -100}
	s.fruitHz = 20
	a, r := s.run(t, ACParams{})
	frag := 0
	for i := range s.targets {
		n := len(r.shownOf[i])
		if n == 0 {
			t.Errorf("target %d never shown", i)
		}
		frag += n
	}
	// purity: fixes of each shown id that belong to its main truth
	tot, main := 0, 0
	for id, m := range r.ids {
		if !r.shownOfAny(id) {
			continue
		}
		mx := 0
		for tr, n := range m {
			tot += n
			if tr >= 0 {
				mx = max(mx, n)
			}
		}
		main += mx
	}
	purity := float64(main) / float64(max(tot, 1))
	st := a.Stats()
	t.Logf("crowd: ids/target %.2f, purity %.4f, false shown %d, P50 %.0f m P95 %.0f m, stats %+v",
		float64(frag)/float64(len(s.targets)), purity, len(r.falseShown), acPct(r.errs, 0.5), acPct(r.errs, 0.95), st)
	if float64(frag)/float64(len(s.targets)) > 1.3 {
		t.Errorf("fragmentation %.2f identities per target", float64(frag)/float64(len(s.targets)))
	}
	if purity < 0.98 {
		t.Errorf("purity %.4f", purity)
	}
	if len(r.falseShown) != 0 {
		t.Errorf("%d false targets shown", len(r.falseShown))
	}
	if acPct(r.errs, 0.95) > 300 {
		t.Errorf("P95 %.0f m", acPct(r.errs, 0.95))
	}
	crowded := 0
	for _, e := range r.last {
		if e.Crowded {
			crowded++
		}
	}
	t.Logf("crowded at the end: %d", crowded)
}

func (r *acRun) shownOfAny(id uint32) bool {
	for _, m := range r.shownOf {
		if m[id] {
			return true
		}
	}
	return false
}

// FRUIT alone never produces a shown target, and the target table stays
// bounded by MaxTargets.
func TestACTrackerFRUIT(t *testing.T) {
	s := newACScenario(3, 0)
	s.fruitHz = 200
	s.durS = 60
	a, r := s.run(t, ACParams{MaxTargets: 500})
	if len(r.falseShown) != 0 || a.Stats().Confirmed != 0 {
		t.Fatalf("FRUIT shown: %d (stats %+v)", len(r.falseShown), a.Stats())
	}
	if a.Len() > 500 {
		t.Fatalf("%d targets", a.Len())
	}
	if a.Stats().FixDropped == 0 {
		t.Fatal("MaxTargets never reached")
	}
	a.Sweep(s.ns(200), nil)
	if a.Len() != 0 || len(a.grid) != 0 || len(a.byCode) != 0 {
		t.Fatalf("not cleaned up: %d targets, %d cells, %d codes", a.Len(), len(a.grid), len(a.byCode))
	}
}

// After confirmation, 2-3 station clusters alone keep a target alive and
// accurate; clusters with a code the target never sent are not used.
func TestACTrackerClustersOnly(t *testing.T) {
	s := newACScenario(4, 1)
	s.durS = 40
	a, r := s.run(t, ACParams{})
	if len(r.last) != 1 {
		t.Fatalf("not shown: %v", r.ids)
	}
	id := r.last[0].ICAO
	before := a.Stats().ClusterApplied
	s.pFix, s.pClus = 0, 1
	s.phase = nil
	ev := s.events()
	var errs []float64
	next := 41.0
	for _, e := range ev {
		tS := e.tS + 40
		for tS >= next {
			var est Estimate
			if !a.Predict(id, s.ns(next), &est) {
				t.Fatalf("target lost at %.0f s", next)
			}
			lf := geo.NewFrame(geo.FromECEF(s.pos(0, next)))
			d := lf.ToENU(est.ECEF)
			errs = append(errs, math.Hypot(d.X, d.Y))
			next++
		}
		c := s.cluster(s.pos(0, tS), tS, 3)
		a.Cluster(e.code, &c)
	}
	st := a.Stats()
	t.Logf("clusters only: applied %d, errors %v", st.ClusterApplied-before, errs)
	if st.ClusterApplied-before < 50 {
		t.Fatalf("clusters applied %d", st.ClusterApplied-before)
	}
	if acPct(errs, 0.95) > 500 {
		t.Fatalf("errors %v", errs)
	}
	c := s.cluster(s.pos(0, 80), 80, 3)
	if res := a.Cluster(ACFromSquawk(0o1234)^1, &c); res.Res != ResNoTrack {
		t.Fatalf("foreign code: %v", res.Res)
	}
	var h Estimate
	if !a.Hint(ACFromSquawk(s.targets[0].squawk), s.ns(80), geo.Vec3{}, &h) || h.ICAO != id {
		t.Fatal("Hint")
	}
}

func TestACParamsNormalize(t *testing.T) {
	d := DefaultACParams()
	if n := (ACParams{}).Normalize(); n.GateChi2 != d.GateChi2 || n.Track.CoastS != 30 || n.Track.ConfirmUpdates != 3 || n.MaxTargets != 20000 {
		t.Fatalf("%+v", n)
	}
	n := ACParams{GateChi2: 1, ConfirmFixes: 1000, MaxTargets: -1, CodeTTLS: math.NaN()}.Normalize()
	if n.GateChi2 != 4 || n.ConfirmFixes != 100 || n.MaxTargets != 20000 || n.CodeTTLS != 120 {
		t.Fatalf("%+v", n)
	}
}

func BenchmarkACTrackerFix(b *testing.B) {
	// 2000 established targets; each iteration one fix of a random target
	s := newACScenario(5, 2000)
	s.fr = geo.NewFrame(geo.LLH{Lat: 51, Lon: 9})
	for i := range s.targets {
		s.targets[i].p0.X *= 8 // spread over ~1000 km
		s.targets[i].p0.Y *= 8
	}
	a := NewACTracker(ACParams{})
	for tS := 0.0; tS < 6; tS += 2 {
		for i := range s.targets {
			f := s.fix(s.pos(i, tS), tS)
			a.Fix(ACFromSquawk(s.targets[i].squawk), &f)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	tS := 6.0
	for k := 0; k < b.N; k++ {
		i := k % len(s.targets)
		if i == 0 {
			tS += 2
		}
		f := s.fix(s.pos(i, tS), tS)
		a.Fix(ACFromSquawk(s.targets[i].squawk), &f)
	}
}

func fmtIDs(m map[int]int) string {
	out := ""
	for k, v := range m {
		out += " " + itoa(k) + ":" + itoa(v)
	}
	return out
}

func itoa(v int) string {
	if v < 0 {
		return "-" + itoa(-v)
	}
	if v < 10 {
		return string(rune('0' + v))
	}
	return itoa(v/10) + string(rune('0'+v%10))
}

// A target whose Mode C replies were solved only in other dwells than its
// Mode A replies gets a second target; once both are tracked, the two are
// merged.
func TestACTrackerMerge(t *testing.T) {
	s := newACScenario(6, 1)
	s.durS = 40
	s.pFix, s.pClus = 1, 0
	a := NewACTracker(ACParams{})
	idA := ACFromSquawk(s.targets[0].squawk)
	next := 1.0
	ids := map[uint32]bool{}
	for _, e := range s.events() {
		for e.tS >= next {
			a.Sweep(s.ns(next), nil)
			next++
		}
		if (e.tS < 10 && e.code != idA) || (e.tS >= 10 && e.tS < 20 && e.code == idA) {
			continue // only Mode A replies solved for 10 s, then only Mode C
		}
		f := s.fix(s.pos(0, e.tS), e.tS)
		if r := a.Fix(e.code, &f); r.ID != 0 {
			ids[r.ID] = true
		}
	}
	var shown []ACEstimate
	a.Sweep(s.ns(s.durS), func(e *ACEstimate) { shown = append(shown, *e) })
	st := a.Stats()
	if len(ids) != 2 || st.Merged != 1 || len(shown) != 1 || a.Len() != 1 {
		t.Fatalf("ids %d merged %d shown %d targets %d", len(ids), st.Merged, len(shown), a.Len())
	}
	if e := shown[0]; !e.SquawkOK || ACSquawk(e.Squawk) != s.targets[0].squawk || !e.AltOK || e.Codes < 2 {
		t.Fatalf("merged target %+v", e)
	}
}

// A target started and fed only by 4-station fixes (no redundancy) needs
// WeakConfirmFixes fixes in WeakConfirmBursts bursts before it is shown;
// one redundant fix makes it strong.
func TestACTrackerWeak(t *testing.T) {
	s := newACScenario(7, 1)
	a := NewACTracker(ACParams{})
	code := ACFromSquawk(s.targets[0].squawk)
	feed := func(tS float64, dof int) {
		f := s.fix(s.pos(0, tS), tS)
		f.DOF = dof
		a.Fix(code, &f)
	}
	shownAfter := func(dof int) int {
		*a = *NewACTracker(ACParams{})
		for k := 0; k < 20; k++ {
			feed(float64(k)*2, dof)
			if a.Stats().Confirmed > 0 {
				return k + 1
			}
		}
		return -1
	}
	if n := shownAfter(0); n != a.p.WeakConfirmFixes {
		t.Fatalf("weak target shown after %d fixes (want %d)", n, a.p.WeakConfirmFixes)
	}
	if n := shownAfter(1); n != a.p.ConfirmFixes {
		t.Fatalf("strong target shown after %d fixes (want %d)", n, a.p.ConfirmFixes)
	}
	// SpawnMinDOF 1: 4-station fixes start nothing
	b := NewACTracker(ACParams{SpawnMinDOF: 1})
	f := s.fix(s.pos(0, 0), 0)
	f.DOF = 0
	if r := b.Fix(code, &f); r.Res != ResNoTrack || b.Len() != 0 || b.Stats().FixNoSpawn != 1 {
		t.Fatalf("spawned from DOF 0: %+v", r)
	}
}
