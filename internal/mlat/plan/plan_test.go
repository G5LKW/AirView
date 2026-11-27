package plan

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/group"
	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

var _ group.Selector = (*Planner)(nil)

type stMap map[model.StationIdx]StationInfo

func (m stMap) Station(st model.StationIdx) (StationInfo, bool) {
	s, ok := m[st]
	return s, ok
}

type posMap map[uint32]struct {
	p geo.Vec3
	s float64
}

func (m posMap) Approx(key uint32) (geo.Vec3, float64, bool) {
	v, ok := m[key]
	return v.p, v.s, ok
}

const t0 = int64(1_763_000_000_000_000_000)

func newPlanner(t testing.TB, st Stations, mut func(*Options)) *Planner {
	t.Helper()
	o := Options{Config: DefaultConfig(), Stations: st, Now: func() time.Time { return time.Unix(0, t0) }}
	if mut != nil {
		mut(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrioAndCandidate(t *testing.T) {
	pos := posMap{}
	hold := map[uint32]bool{0x300: true}
	p := newPlanner(t, stMap{}, func(o *Options) {
		o.Positions = pos
		o.HoldOut = func(k uint32, _ int64) bool { return hold[k] }
	})
	now := t0 + 1e9
	if p.Prio(0x100, now) != PrioAcquire || !p.Candidate(0x100, 4, now) {
		t.Fatal("unknown aircraft is not an acquisition candidate")
	}
	p.NoteADSB(0x100, now, true)
	if p.Prio(0x100, now) != PrioNone || p.Candidate(0x100, 17, now) {
		t.Fatal("good ADS-B aircraft is a candidate")
	}
	if p.Prio(0x100, now+11e9) != PrioAcquire {
		t.Fatal("ADS-B older than 10 s still counts")
	}
	p.NoteADSB(0x100, now, false)
	if p.Prio(0x100, now) != PrioAcquire {
		t.Fatal("poor ADS-B is not a candidate")
	}
	pos[0x200] = struct {
		p geo.Vec3
		s float64
	}{geo.Vec3{X: 1}, 500}
	if p.Prio(0x200, now) != PrioTracked {
		t.Fatal("tracked")
	}
	p.NoteADSB(0x300, now, true)
	if p.Prio(0x300, now) != PrioHoldOut || !p.Candidate(0x300, 17, now) {
		t.Fatal("hold-out")
	}
	p.NoteADSB(0x400, now, true)
	p.MarkSuspect(0x400, now+60e9)
	if p.Prio(0x400, now) != PrioSuspect || p.Prio(0x400, now+61e9) != PrioAcquire {
		t.Fatal("suspect")
	}
	// ObserveGroup with a DF17 position result marks ADS-B.
	g := &ingest.Group{Key: 0x500, DF: 17, Data: []byte{17 << 3}, FirstNs: now, HasResult: true, Result: tracker.Result{Pos: true}}
	p.ObserveGroup(g)
	if p.Prio(0x500, now) != PrioNone {
		t.Fatal("DF17 position not noted")
	}
	// DF18 TIS-B positions do not count.
	g = &ingest.Group{Key: 0x600, DF: 18, Data: []byte{18<<3 | 2}, FirstNs: now, HasResult: true, Result: tracker.Result{Pos: true}}
	p.ObserveGroup(g)
	if p.Prio(0x600, now) != PrioAcquire {
		t.Fatal("TIS-B counted as ADS-B")
	}
	for pr, s := range map[Prio]string{PrioAcquire: "acquire", PrioTracked: "tracked", PrioHoldOut: "holdout", PrioSuspect: "suspect", PrioNone: "none"} {
		if pr.String() != s {
			t.Errorf("%d: %s", pr, pr.String())
		}
	}
}

func TestAdmitRates(t *testing.T) {
	pos := posMap{}
	p := newPlanner(t, stMap{}, func(o *Options) { o.Positions = pos })
	count := func(key uint32, hz float64, secs float64) int {
		n := 0
		for i := 0; i < int(hz*secs); i++ {
			if p.Admit(key, 5, t0+int64(float64(i)/hz*1e9)) {
				n++
			}
		}
		return n
	}
	// Acquire: 4/s + burst 3 over 10 s at 100 calls/s.
	if n := count(1, 100, 10); n < 42 || n > 44 {
		t.Fatalf("acquire admitted %d", n)
	}
	pos[2] = struct {
		p geo.Vec3
		s float64
	}{geo.Vec3{}, 100}
	if n := count(2, 100, 10); n < 22 || n > 24 {
		t.Fatalf("tracked admitted %d", n)
	}
	// Global saturation: budget 100/s; 200 suspect aircraft and 20
	// acquisitions all at their own rate limits.
	q := newPlanner(t, stMap{}, func(o *Options) { o.Config.GlobalSolveHz = 100 })
	now := t0 + 1e9
	for k := uint32(1000); k < 1200; k++ {
		q.NoteADSB(k, now, true)
		q.MarkSuspect(k, now+1e12)
	}
	adm := [NumPrio]int{}
	for step := 0; step < 1000; step++ { // 10 s in 10 ms steps
		ts := now + int64(step)*1e7
		for k := uint32(1000); k < 1200; k++ {
			if step%20 == int(k%20) && q.Admit(k, 5, ts) {
				adm[PrioSuspect]++
			}
		}
		for k := uint32(1); k <= 20; k++ {
			if step%25 == int(k%25) && q.Admit(k, 5, ts) {
				adm[PrioAcquire]++
			}
		}
		for k := uint32(1000); k < 1200; k++ { // keep their ADS-B fresh
			if step%100 == 0 {
				q.NoteADSB(k, ts, true)
			}
		}
	}
	// Acquisitions: 20 aircraft x 4/s x 10 s = 800 asked, all fit (rate
	// limited by their own buckets); suspects get only what is left above
	// the 50 % reserve.
	if adm[PrioAcquire] < 380 { // 20 aircraft * 10 s * 4/s / 2 (asked every 250 ms)
		t.Fatalf("acquisitions starved: %v", adm)
	}
	tot := adm[PrioAcquire] + adm[PrioSuspect]
	if tot > 100*10+100+5 {
		t.Fatalf("global budget exceeded: %d", tot)
	}
	s := q.Summary()
	if s.Denied[PrioSuspect] == 0 || s.Admitted[PrioAcquire] != uint64(adm[PrioAcquire]) {
		t.Fatalf("summary %+v", s)
	}
}

// netWorld is a simnet geometry with synthetic hearing.
type netWorld struct {
	sts   stMap
	ids   []model.StationIdx
	ac    []simnet.AircraftTruth
	heard map[uint32][]model.StationIdx
}

func newNetWorld(t testing.TB, seed uint64, rx, aircraft int, radiusKm float64) *netWorld {
	t.Helper()
	specs := simnet.RandomReceivers(seed, rx, simnet.Placement{CenterLat: 52, CenterLon: 0, RadiusKm: radiusKm})
	n, err := simnet.New(simnet.Config{Seed: seed, Aircraft: aircraft, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Advance(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	w := &netWorld{sts: stMap{}, heard: map[uint32][]model.StationIdx{}}
	rng := rand.New(rand.NewPCG(seed, 1))
	for _, r := range n.Receivers() {
		st := model.StationIdx(r.Index + 2)
		w.ids = append(w.ids, st)
		w.sts[st] = StationInfo{Pos: geo.Vec3{X: r.Pos[0], Y: r.Pos[1], Z: r.Pos[2]}, Located: true,
			Synced: rng.Float64() < 0.85, Anchor: r.Class.AnchorCapable(), Class: r.Class}
	}
	for _, a := range n.Aircraft() {
		if a.HAE < 300 {
			continue
		}
		w.ac = append(w.ac, a)
		p := geo.Vec3{X: a.Pos[0], Y: a.Pos[1], Z: a.Pos[2]}
		for _, st := range w.ids {
			r := w.sts[st].Pos
			if d := r.Dist(p); d < 400e3 && geo.LineOfSight(r, p, 0) && rng.Float64() < 1-d/500e3 {
				w.heard[a.ICAO] = append(w.heard[a.ICAO], st)
			}
		}
	}
	return w
}

func (w *netWorld) apos(a simnet.AircraftTruth) geo.Vec3 {
	return geo.Vec3{X: a.Pos[0], Y: a.Pos[1], Z: a.Pos[2]}
}

// feed tells the planner who hears whom; adsbFrac of the aircraft have ADS-B.
func (w *netWorld) feed(p *Planner, now int64, adsbEvery int) {
	for i, a := range w.ac {
		for _, st := range w.heard[a.ICAO] {
			p.NoteHearer(a.ICAO, st, now)
		}
		// rate: ~8 groups/s
		g := &ingest.Group{Key: a.ICAO, DF: 4, FirstNs: now}
		for k := 0; k < 8; k++ {
			g.FirstNs = now - int64(k)*125e6
			p.ObserveGroup(g)
		}
		if adsbEvery > 0 && i%adsbEvery != 0 {
			p.NoteADSB(a.ICAO, now, true)
		}
	}
}

// crlbH returns the horizontal CRLB (m) of a station set for an aircraft at
// p (independent implementation: normal equations solved by Gaussian
// elimination).
func crlbH(sts stMap, set []model.StationIdx, p geo.Vec3) float64 {
	var J [4][8]float64
	for _, st := range set {
		in := sts[st]
		d := p.Sub(in.Pos)
		r := d.Norm()
		h := [4]float64{d.X / r, d.Y / r, d.Z / r, 1}
		s := timingSigmaM(in)
		for i := 0; i < 4; i++ {
			for j := 0; j < 4; j++ {
				J[i][j] += h[i] * h[j] / (s * s)
			}
		}
	}
	for i := 0; i < 4; i++ {
		J[i][i] += 1 / (priorPosM * priorPosM)
		J[i][4+i] = 1
	}
	for c := 0; c < 4; c++ {
		piv := c
		for r := c + 1; r < 4; r++ {
			if math.Abs(J[r][c]) > math.Abs(J[piv][c]) {
				piv = r
			}
		}
		J[c], J[piv] = J[piv], J[c]
		for r := 0; r < 4; r++ {
			if r == c {
				continue
			}
			f := J[r][c] / J[c][c]
			for k := 0; k < 8; k++ {
				J[r][k] -= f * J[c][k]
			}
		}
	}
	var P [3][3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			P[i][j] = J[i][4+j] / J[i][i]
		}
	}
	u := p.Unit()
	uv := [3]float64{u.X, u.Y, u.Z}
	var vu float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			vu += uv[i] * P[i][j] * uv[j]
		}
	}
	return math.Sqrt(max(P[0][0]+P[1][1]+P[2][2]-vu, 0))
}

// TestReplanPlanSizes is the WP-GR acceptance test "plan sizes respected"
// on a simnet geometry, and checks that the deltas reproduce the sets.
func TestReplanPlanSizes(t *testing.T) {
	w := newNetWorld(t, 3, 150, 400, 300)
	told := map[model.StationIdx]map[uint32]bool{}
	toldSync := map[model.StationIdx]map[uint32]bool{}
	toldMLAT := map[model.StationIdx]map[uint32]bool{}
	pos := posMap{}
	var deltas, maxNonReset int
	p := newPlanner(t, w.sts, func(o *Options) {
		o.Positions = pos
		o.Out = OutFunc(func(st model.StationIdx, d *Delta) {
			deltas++
			if d.Reset {
				told[st], toldSync[st], toldMLAT[st] = map[uint32]bool{}, map[uint32]bool{}, map[uint32]bool{}
			} else {
				maxNonReset = max(maxNonReset, len(d.SyncAdd)+len(d.MLATAdd))
			}
			for _, k := range d.SendDel {
				delete(told[st], k)
			}
			for _, k := range d.SendAdd {
				told[st][k] = true
			}
			for _, k := range d.SyncDel {
				delete(toldSync[st], k)
			}
			for _, k := range d.SyncAdd {
				toldSync[st][k] = true
			}
			for _, k := range d.MLATDel {
				delete(toldMLAT[st], k)
			}
			for _, k := range d.MLATAdd {
				toldMLAT[st][k] = true
			}
		})
	})
	// Tracked positions for half of the Mode S-only aircraft.
	for i, a := range w.ac {
		if i%6 == 0 {
			pos[a.ICAO] = struct {
				p geo.Vec3
				s float64
			}{w.apos(a), 300}
		}
	}
	cfg := p.Config()
	now := t0 + 2e9
	for round := 0; round < 4; round++ {
		w.feed(p, now, 3) // one aircraft in 3 is Mode S-only
		p.Replan(now)
		now += 5e9
	}
	sum := p.Summary()
	t.Logf("summary: %+v", sum)
	if sum.Aircraft < 300 || sum.Candidates[PrioAcquire]+sum.Candidates[PrioTracked] < 100 {
		t.Fatalf("too small: %+v", sum)
	}
	var syncSizes, mlatSizes []int
	for _, st := range w.ids {
		ss, ms := p.Sets(st)
		syncSizes = append(syncSizes, len(ss))
		mlatSizes = append(mlatSizes, len(ms))
		if len(ss) > cfg.SyncSetMax || len(ms) > cfg.MLATSetMax {
			t.Fatalf("station %d: sync %d mlat %d", st, len(ss), len(ms))
		}
		// The deltas reproduce the sets, and the union for mlat-client.
		gotS, gotM := keysOf(toldSync[st]), keysOf(toldMLAT[st])
		if !slices.Equal(gotS, ss) || !slices.Equal(gotM, ms) {
			t.Fatalf("station %d: deltas give sync %v mlat %v, planner has %v %v", st, gotS, gotM, ss, ms)
		}
		union := keysOf(told[st])
		want := append(append([]uint32(nil), ss...), ms...)
		slices.Sort(want)
		want = slices.Compact(want)
		if !slices.Equal(union, want) {
			t.Fatalf("station %d: send union %v want %v", st, union, want)
		}
		// A station that hears enough ADS-B aircraft gets at least the minimum.
		nSync := 0
		for _, a := range w.ac {
			if slices.Contains(w.heard[a.ICAO], st) && p.Prio(a.ICAO, now) == PrioNone {
				nSync++
			}
		}
		if nSync >= 2*cfg.SyncSetMin && len(ss) < cfg.SyncSetMin {
			t.Fatalf("station %d hears %d sync aircraft but has %d", st, nSync, len(ss))
		}
	}
	slices.Sort(syncSizes)
	slices.Sort(mlatSizes)
	t.Logf("syncSet sizes p10/p50/p90 %d/%d/%d; mlatSet %d/%d/%d", syncSizes[len(syncSizes)/10], syncSizes[len(syncSizes)/2],
		syncSizes[len(syncSizes)*9/10], mlatSizes[len(mlatSizes)/10], mlatSizes[len(mlatSizes)/2], mlatSizes[len(mlatSizes)*9/10])
	if maxNonReset > cfg.MaxChurn {
		t.Fatalf("churn %d > %d", maxNonReset, cfg.MaxChurn)
	}
	// Per-aircraft plans: within the size limits, and D-optimal selection
	// close to using every hearer, better than a random subset of equal size.
	rng := rand.New(rand.NewPCG(9, 9))
	var ratioSum, randSum float64
	n := 0
	for _, a := range w.ac {
		sts, sig, ok := p.AircraftPlan(a.ICAO)
		if !ok || p.Prio(a.ICAO, now) == PrioNone {
			continue
		}
		if len(sts) > cfg.StationsPerAircraft {
			t.Fatalf("aircraft %06X: %d stations", a.ICAO, len(sts))
		}
		var located []model.StationIdx
		for _, st := range w.heard[a.ICAO] {
			if w.sts[st].Located {
				located = append(located, st)
			}
		}
		if len(located) >= cfg.MinStationsPerAircraft && len(sts) < cfg.MinStationsPerAircraft {
			t.Fatalf("aircraft %06X: %d stations of %d hearers", a.ICAO, len(sts), len(located))
		}
		if len(sts) < 5 || len(located) <= len(sts) {
			continue
		}
		pt := w.apos(a)
		all := crlbH(w.sts, located, pt)
		mine := crlbH(w.sts, sts, pt)
		perm := rng.Perm(len(located))
		var rnd []model.StationIdx
		for _, i := range perm[:len(sts)] {
			rnd = append(rnd, located[i])
		}
		rs := crlbH(w.sts, rnd, pt)
		if _, tracked := pos[a.ICAO]; tracked && math.Abs(mine-sig)/mine > 0.01 {
			t.Fatalf("reported sigma %.1f, recomputed %.1f", sig, mine)
		}
		ratioSum += mine / all
		randSum += rs / all
		n++
	}
	if n < 30 {
		t.Fatalf("only %d plans to compare", n)
	}
	t.Logf("CRLB of the planned subset / all hearers: mean %.3f; random subset of equal size: %.3f (%d aircraft)", ratioSum/float64(n), randSum/float64(n), n)
	if ratioSum >= randSum {
		t.Fatal("D-optimal selection no better than random")
	}
	// Stability: an unchanged world gives (almost) no deltas.
	before := deltas
	w.feed(p, now, 3)
	p.Replan(now)
	if extra := deltas - before; extra > len(w.ids)/10 {
		t.Fatalf("%d deltas without a change", extra)
	}
	// Stations that hear nothing any more are emptied.
	p.Replan(now + int64(2*cfg.HeardTTL))
	for _, st := range w.ids {
		if ss, ms := p.Sets(st); len(ss)+len(ms) != 0 {
			t.Fatalf("station %d keeps %d+%d after silence", st, len(ss), len(ms))
		}
	}
}

func keysOf(m map[uint32]bool) []uint32 {
	var k []uint32
	for x := range m {
		k = append(k, x)
	}
	slices.Sort(k)
	return k
}

func TestModeACEligible(t *testing.T) {
	o := geo.ToECEF(52, 0, 0)
	f := geo.NewFrameECEF(o)
	sts := stMap{}
	for i, d := range [][2]float64{{0, 0}, {50e3, 0}, {0, 60e3}, {-40e3, -40e3}, {400e3, 0}, {10e3, 10e3}} {
		sts[model.StationIdx(i+2)] = StationInfo{Pos: f.FromENU(geo.Vec3{X: d[0], Y: d[1]}), Located: true, Synced: true, Anchor: i != 5}
	}
	p := newPlanner(t, sts, func(o *Options) { o.Config.ModeAC.Enabled = true })
	for st := range sts {
		p.NoteHearer(1, st, t0)
	}
	p.Replan(t0)
	for st, want := range map[model.StationIdx]bool{2: true, 3: true, 4: true, 5: true, 6: false, 7: false} {
		if p.ModeACEligible(st) != want {
			t.Errorf("station %d: %v", st, !want)
		}
	}
	q := newPlanner(t, sts, nil)
	q.Replan(t0)
	if q.ModeACEligible(2) {
		t.Fatal("Mode A/C while disabled")
	}
}

func TestConfigNormalize(t *testing.T) {
	c := Config{SyncSetMin: 50, SyncSetMax: 10, StationsPerAircraft: 2, MinStationsPerAircraft: 100, TargetSigmaM: math.NaN()}.Normalize()
	if c.SyncSetMax != 50 || c.StationsPerAircraft != 4 || c.MinStationsPerAircraft != 4 || c.TargetSigmaM != 30 {
		t.Fatalf("%+v", c)
	}
	if DefaultConfig().Normalize() != DefaultConfig() {
		t.Fatal("defaults not stable")
	}
	if _, err := New(Options{}); err == nil {
		t.Fatal("nil Stations accepted")
	}
}

type fakeIng struct {
	got map[model.StationIdx][]ingest.InterestCmd
}

func (f *fakeIng) SetInterest(st model.StationIdx, in ingest.InterestCmd) error {
	f.got[st] = append(f.got[st], in)
	return nil
}

type fakeSend struct{ start, stop map[model.StationIdx][]uint32 }

func (f *fakeSend) StartSending(st model.StationIdx, k []uint32) {
	f.start[st] = append(f.start[st], k...)
}
func (f *fakeSend) StopSending(st model.StationIdx, k []uint32) {
	f.stop[st] = append(f.stop[st], k...)
}

func TestAdapters(t *testing.T) {
	fi := &fakeIng{got: map[model.StationIdx][]ingest.InterestCmd{}}
	fs := &fakeSend{start: map[model.StationIdx][]uint32{}, stop: map[model.StationIdx][]uint32{}}
	out := Fanout{IngestOut{Ingest: fi}, MLATCOut{C: fs}}
	buf := []uint32{1, 2}
	d := &Delta{Reset: true, SyncAdd: buf, MLATAdd: []uint32{3}, SendAdd: []uint32{1, 2, 3}, SendDel: []uint32{9}}
	out.Interest(5, d)
	buf[0] = 99 // the planner reuses its buffers
	if in := fi.got[5][0]; !in.Reset || in.SyncAdd[0] != 1 || in.MLATAdd[0] != 3 || in.SyncDel != nil {
		t.Fatalf("ingest got %+v", in)
	}
	if !slices.Equal(fs.start[5], []uint32{1, 2, 3}) || !slices.Equal(fs.stop[5], []uint32{9}) {
		t.Fatalf("mlatc got %v / %v", fs.start[5], fs.stop[5])
	}
	if (&Delta{}).Empty() != true || d.Empty() {
		t.Fatal("Empty")
	}
}

func TestRunStops(t *testing.T) {
	p := newPlanner(t, stMap{}, func(o *Options) { o.Config.ReplanEvery = time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- p.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
