package group

import (
	"math"
	"math/bits"
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// groupFor builds a group of transmission x heard by sts (noise sigmaNs).
func groupFor(h *harness, x tx, sts []model.StationIdx, data []byte, key uint32, sigmaNs float64, rng *rand.Rand) *ingest.Group {
	kind := model.FrameModeSShort
	if len(data) == 14 {
		kind = model.FrameModeSLong
	}
	g := &ingest.Group{Hash: uint64(key)<<8 | uint64(len(data)), Data: data, Key: key, DF: data[0] >> 3, Kind: kind, FirstNs: x.at.Ns}
	for _, st := range sts {
		g.Copies = append(g.Copies, copyOf(h.clk, st, toa(h.w, x, st, sigmaNs, rng)))
	}
	return g
}

// visibleStations returns the stations that see p, nearest first is not
// required; at most n.
func visibleStations(w *world, p geo.Vec3, n int) []model.StationIdx {
	var out []model.StationIdx
	for _, st := range w.st {
		if w.visible(st, p) && len(out) < n {
			out = append(out, st)
		}
	}
	return out
}

type fakeSel struct {
	cand, admit bool
	asked       int
}

func (s *fakeSel) Candidate(uint32, uint8, int64) bool { return s.cand }
func (s *fakeSel) Admit(uint32, int, int64) bool {
	s.asked++
	return s.admit
}

func TestUseClassification(t *testing.T) {
	w := newWorld(t, 5, 30, 40, 150)
	sel := &fakeSel{cand: true, admit: true}
	hint := &fixedHint{pos: map[uint32]geo.Vec3{}}
	h := newHarness(t, w, func(o *Options) { o.Selector = sel; o.Hints = hint })
	rng := rand.New(rand.NewPCG(3, 4))
	p := v3(w.ac[0].Pos)
	x := tx{pos: p, at: Time{Ns: h.clk.base + 5e9}}
	sts := visibleStations(w, p, 30)
	if len(sts) < 5 {
		t.Fatalf("only %d visible", len(sts))
	}
	df11 := []byte{11 << 3, 0x12, 0x34, 0x56, 0, 0, 0}
	run := func(data []byte, n int) *Cluster {
		h.out.cl = h.out.cl[:0]
		h.a.Observe(groupFor(h, x, sts[:n], data, 0x123456, 50, rng))
		if len(h.out.cl) == 0 {
			return nil
		}
		if len(h.out.cl) != 1 {
			t.Fatalf("%d clusters", len(h.out.cl))
		}
		return h.out.cl[0]
	}
	if c := run(df4(20000), 3); c == nil || c.Use != UseSolveAlt || c.AltSrc != AltFrameBaro {
		t.Fatalf("3 + alt: %+v", c)
	}
	if c := run(df11, 3); c == nil || c.Use != UseTrack || c.AltSrc != AltNone {
		t.Fatalf("3 without alt: %+v", c)
	}
	hint.altFt = 31000
	if c := run(df11, 3); c == nil || c.Use != UseSolveAlt || c.AltSrc != AltHint || c.AltFt != 31000 {
		t.Fatalf("3 + hint alt: %+v", c)
	}
	hint.altFt = 0
	if c := run(df11, 2); c == nil || c.Use != UseTrack {
		t.Fatalf("2 stations: %+v", c)
	}
	if c := run(df11, 4); c == nil || c.Use != UseSolve {
		t.Fatalf("4 stations: %+v", c)
	}
	sel.admit = false
	if c := run(df11, 5); c == nil || c.Use != UseTrack {
		t.Fatalf("not admitted: %+v", c)
	}
	if h.a.Stats().NotAdmitted != 1 {
		t.Fatalf("NotAdmitted %d", h.a.Stats().NotAdmitted)
	}
	sel.cand = false
	if c := run(df11, 5); c != nil {
		t.Fatal("not a candidate but clustered")
	}
	sel.cand = true
	// Unsynced stations and stations without a site are dropped.
	h.clk.off = map[model.StationIdx]bool{sts[0]: true}
	delete(w.sites, sts[1])
	c := run(df11, 5)
	w.sites[sts[1]] = v3(w.n.Receiver(int(sts[1]) - 2).Pos)
	h.clk.off = nil
	if c == nil || len(c.Members) != 3 {
		t.Fatalf("drops: %+v", c)
	}
	st := h.a.Stats()
	if st.NoClock != 1 || st.NoSite != 1 || st.NotCandidate != 1 {
		t.Fatalf("stats %+v", st)
	}
	// TIS-B and Mode A/C groups are excluded.
	tisb := make([]byte, 14)
	tisb[0] = 18<<3 | 2
	h.out.cl = h.out.cl[:0]
	h.a.Observe(groupFor(h, x, sts[:5], tisb, 1, 50, rng))
	if len(h.out.cl) != 0 || h.a.Stats().Excluded != 1 {
		t.Fatal("TIS-B clustered")
	}
	// Untimed / map-only / second-channel copies never count.
	g := groupFor(h, x, sts[:5], df11, 2, 50, rng)
	g.Copies[0].Timed = false
	g.Copies[1].MapOnly = true
	g.Copies[2].SameStation = true
	h.out.cl = h.out.cl[:0]
	h.a.Observe(g)
	if len(h.out.cl) != 1 || len(h.out.cl[0].Members) != 2 {
		t.Fatalf("untimed copies used: %d clusters", len(h.out.cl))
	}
}

func TestColocatedFusion(t *testing.T) {
	w := newWorld(t, 9, 20, 40, 150)
	p := v3(w.ac[0].Pos)
	sts := visibleStations(w, p, 6)
	if len(sts) < 4 {
		t.Fatal("too few stations")
	}
	// Two extra stations 300 m and 700 m from the first one.
	base := w.sites[sts[0]]
	f := geo.NewFrameECEF(base)
	w.sites[9001] = f.FromENU(geo.Vec3{X: 300, Y: 0, Z: 5})
	w.sites[9002] = f.FromENU(geo.Vec3{X: -200, Y: 670, Z: -3})
	all := append([]model.StationIdx{9001, 9002}, sts...)
	h := newHarness(t, w, nil)
	h.clk.jv[9001], h.clk.jv[9002], h.clk.jv[sts[0]] = 100, 400, 900 // different weights
	x := tx{pos: p, at: Time{Ns: h.clk.base + 3e9}}
	rng := rand.New(rand.NewPCG(5, 6))
	h.a.Observe(groupFor(h, x, all, df4(18000), 77, 0, rng))
	if len(h.out.cl) != 1 {
		t.Fatalf("%d clusters", len(h.out.cl))
	}
	c := h.out.cl[0]
	if len(c.Members) != len(all) || c.NSites() != len(all)-2 {
		t.Fatalf("members %d sites %d", len(c.Members), c.NSites())
	}
	var fused *Site
	for i := range c.Sites {
		if c.Sites[i].Count == 3 {
			fused = &c.Sites[i]
		}
	}
	if fused == nil {
		t.Fatal("no fused site")
	}
	// The fused time is the arrival time at the fused position to second
	// order: compare with the noise-free truth.
	want := x.at.Add(fused.Pos.Dist(p) / mPerNs)
	errNs := math.Abs(fused.T.Sub(want))
	var b, wsum float64
	for k := fused.First; k < fused.First+fused.Count; k++ {
		m := c.Members[k]
		wsum += m.W
		d := m.Pos.Sub(fused.Pos)
		b += m.W * d.Dot(d)
	}
	rho := fused.Pos.Dist(p)
	bound := b/(2*rho)/mPerNs + 3.0/tickScale // second-order bias + tick rounding
	if math.Abs(wsum-1) > 1e-12 || errNs > bound {
		t.Fatalf("fused time error %.4f ns (bound %.4f), weights sum %v", errNs, bound, wsum)
	}
	// Inverse-variance weights: the member with variance 100 dominates.
	vInv := 1/100.0 + 1/400.0 + 1/900.0
	geom := b / (2 * DefaultConfig().FuseMinRangeM) / mPerNs
	if wantVar := 1/vInv + geom*geom; math.Abs(fused.Var-wantVar) > 1e-6*wantVar {
		t.Fatalf("fused var %v want %v", fused.Var, wantVar)
	}
	// Without fusion every station is a site.
	h2 := newHarness(t, w, func(o *Options) { o.Config.ColocatedM = 0 })
	h2.a.Observe(groupFor(h2, x, all, df4(18000), 77, 0, rng))
	if h2.out.cl[0].NSites() != len(all) {
		t.Fatalf("ColocatedM 0: %d sites", h2.out.cl[0].NSites())
	}
}

func TestHintConflict(t *testing.T) {
	w := newWorld(t, 7, 40, 40, 250)
	hint := &fixedHint{pos: map[uint32]geo.Vec3{}, sigma: 500}
	h := newHarness(t, w, func(o *Options) { o.Hints = hint })
	rng := rand.New(rand.NewPCG(8, 9))
	conflicts, n := 0, 0
	for i, ac := range w.ac {
		p := v3(ac.Pos)
		sts := visibleStations(w, p, 40)
		if len(sts) < 8 {
			continue
		}
		key := uint32(i + 1)
		hint.pos[key] = p.Add(geo.Vec3{X: 40e3, Y: -30e3}) // 50 km off, claims 500 m
		x := tx{pos: p, at: Time{Ns: h.clk.base + int64(i)*1e9}}
		h.out.cl = h.out.cl[:0]
		h.a.Observe(groupFor(h, x, sts, df4(30000), key, 100, rng))
		n++
		if len(h.out.cl) == 1 && h.out.cl[0].HintConflict && len(h.out.cl[0].Members) == len(sts) {
			conflicts++
		}
	}
	if n < 10 || conflicts < n*9/10 {
		t.Fatalf("wrong hints recognised in %d of %d groups", conflicts, n)
	}
}

func TestLateCopies(t *testing.T) {
	w := newWorld(t, 7, 40, 40, 250)
	h := newHarness(t, w, nil)
	rng := rand.New(rand.NewPCG(10, 11))
	var p geo.Vec3
	var sts []model.StationIdx
	for _, ac := range w.ac {
		p = v3(ac.Pos)
		if sts = visibleStations(w, p, 9); len(sts) == 9 {
			break
		}
	}
	if len(sts) < 9 {
		t.Fatal("no aircraft with 9 stations")
	}
	xa := tx{pos: p, at: Time{Ns: h.clk.base + 1e9}}
	xb := tx{pos: p, at: xa.at.Add(5e6)} // identical reply 5 ms later
	data := df4(22000)
	g := groupFor(h, xa, sts[:4], data, 0xABCDEF, 50, rng)
	h.a.Observe(g)
	if len(h.out.cl) != 1 {
		t.Fatalf("%d clusters", len(h.out.cl))
	}
	seq := h.out.cl[0].Seq
	// Late: two more copies of A, and three copies of B.
	late := &ingest.Group{Hash: g.Hash, Data: data, Key: g.Key, DF: 4, Kind: g.Kind, FirstNs: g.FirstNs, Earlier: 4}
	for _, st := range sts[4:6] {
		late.Copies = append(late.Copies, copyOf(h.clk, st, toa(w, xa, st, 50, rng)))
	}
	for _, st := range sts[6:9] {
		late.Copies = append(late.Copies, copyOf(h.clk, st, toa(w, xb, st, 50, rng)))
	}
	h.out.cl = h.out.cl[:0]
	h.a.ObserveLate(late)
	if len(h.out.cl) != 2 {
		t.Fatalf("late: %d clusters", len(h.out.cl))
	}
	var joined, fresh *Cluster
	for _, c := range h.out.cl {
		if c.Seq == seq {
			joined = c
		} else {
			fresh = c
		}
	}
	if joined == nil || !joined.Late || len(joined.Members) != 6 {
		t.Fatalf("joined: %+v", joined)
	}
	nl := 0
	for _, m := range joined.Members {
		if m.Late {
			nl++
		}
	}
	if nl != 2 {
		t.Fatalf("%d late members", nl)
	}
	if fresh == nil || !fresh.Late || len(fresh.Members) != 3 || fresh.Seq <= seq {
		t.Fatalf("fresh: %+v", fresh)
	}
	st := h.a.Stats()
	if st.LateAttached != 2 || st.LateClusters != 2 {
		t.Fatalf("stats %+v", st)
	}
	// After the hold time the record is not used any more.
	late2 := *late
	late2.FirstNs += 3e9
	late2.Copies = late.Copies[:2]
	h.out.cl = h.out.cl[:0]
	h.a.ObserveLate(&late2)
	for _, c := range h.out.cl {
		if c.Seq == seq {
			t.Fatal("expired record joined")
		}
	}
	// A single usable copy is retained so a late copy can pair with it.
	single := groupFor(h, xa, sts[:1], data, 0x111111, 50, rng)
	h.out.cl = h.out.cl[:0]
	h.a.Observe(single)
	lateS := groupFor(h, xa, sts[1:2], data, 0x111111, 50, rng)
	h.a.ObserveLate(lateS)
	if len(h.out.cl) != 1 || len(h.out.cl[0].Members) != 2 || !h.out.cl[0].Late {
		t.Fatalf("single + late: %d clusters", len(h.out.cl))
	}
}

// TestLateStoreEviction fills the ring past its size: old records are
// overwritten, never returned for another hash.
func TestLateStoreEviction(t *testing.T) {
	s := newLateStore(256, 4)
	cl := &Cluster{Members: []Member{{Station: 1}, {Station: 2}}}
	for i := 0; i < 5000; i++ {
		cl.Hash, cl.Seq, cl.FirstNs = uint64(i%700), uint64(i+1), int64(i)
		s.retain(cl, 4)
	}
	var out [bucketWays]int32
	for hsh := uint64(0); hsh < 700; hsh++ {
		n := s.find(hsh, 5000, 1e9, &out)
		for i := 0; i < n; i++ {
			if s.recs[out[i]].h.hash != hsh {
				t.Fatalf("hash %d returned a record of %d", hsh, s.recs[out[i]].h.hash)
			}
		}
	}
	// The most recent 256 are all findable unless their bucket overflowed.
	found := 0
	for i := 5000 - 256; i < 5000; i++ {
		n := s.find(uint64(i%700), int64(i), 0, &out)
		for k := 0; k < n; k++ {
			if s.recs[out[k]].seq == uint64(i+1) {
				found++
			}
		}
	}
	if found < 250 {
		t.Fatalf("only %d of the last 256 records found", found)
	}
}

// TestCliqueExact compares the search with brute force on random graphs.
func TestCliqueExact(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	var adj [MaxMembers]uint64
	var rat [MaxMembers][MaxMembers]float32
	s := cliqueSearch{adj: &adj, ratio: &rat}
	for trial := 0; trial < 3000; trial++ {
		n := 1 + rng.IntN(16)
		dens := rng.Float64()
		for i := range adj[:n] {
			adj[i] = 0
		}
		for i := 0; i < n; i++ {
			for k := i + 1; k < n; k++ {
				r := float32(rng.Float64())
				rat[i][k], rat[k][i] = r, r
				if rng.Float64() < dens {
					adj[i] |= 1 << k
					adj[k] |= 1 << i
				}
			}
		}
		all := uint64(1)<<n - 1
		got := s.run(all, 1<<20)
		if s.exhausted {
			t.Fatal("budget")
		}
		best, bestScore := 0, 0.0
		for m := uint64(1); m <= all; m++ {
			ok := true
			for a := m; a != 0 && ok; a &= a - 1 {
				i := bits.TrailingZeros64(a)
				if (m&^(1<<i))&^adj[i] != 0 {
					ok = false
				}
			}
			if !ok {
				continue
			}
			c := bits.OnesCount64(m)
			var sc float64
			for a := m; a != 0; a &= a - 1 {
				i := bits.TrailingZeros64(a)
				for b := a & (a - 1); b != 0; b &= b - 1 {
					q := float64(rat[i][bits.TrailingZeros64(b)])
					sc += q * q
				}
			}
			if c > best || (c == best && sc < bestScore) {
				best, bestScore = c, sc
			}
		}
		if bits.OnesCount64(got) != best || math.Abs(s.bestScore-bestScore) > 1e-9 {
			t.Fatalf("trial %d n %d: got size %d score %v, brute force %d score %v", trial, n, bits.OnesCount64(got), s.bestScore, best, bestScore)
		}
		// greedy always returns a clique
		var order [MaxMembers]int8
		for i := 0; i < n; i++ {
			order[i] = int8(i)
		}
		g := greedy(&adj, all, order[:n])
		for a := g; a != 0; a &= a - 1 {
			i := bits.TrailingZeros64(a)
			if (g&^(1<<i))&^adj[i] != 0 {
				t.Fatal("greedy result is not a clique")
			}
		}
	}
	// A tiny budget still returns a valid (maybe smaller) clique.
	for i := 0; i < 64; i++ {
		adj[i] = ^uint64(0) &^ (1 << i)
	}
	if got := s.run(^uint64(0), 65); bits.OnesCount64(got) != 64 || s.exhausted {
		t.Fatalf("complete graph: %d", bits.OnesCount64(got))
	}
	if got := s.run(^uint64(0), 10); !s.exhausted || got != 0 {
		t.Fatalf("budget 10: %x exhausted %v", got, s.exhausted)
	}
}

func TestModeAC(t *testing.T) {
	// Six anchors within ~60 km, one far station, one non-anchor.
	o := geo.ToECEF(52, 0, 50)
	f := geo.NewFrameECEF(o)
	sites := siteMap{}
	offs := [][2]float64{{0, 0}, {40e3, 5e3}, {-35e3, 20e3}, {10e3, -45e3}, {-20e3, -30e3}, {30e3, 35e3}, {300e3, 0}, {5e3, 5e3}}
	for i, d := range offs {
		sites[model.StationIdx(i+2)] = f.FromENU(geo.Vec3{X: d[0], Y: d[1]})
	}
	anchor := func(st model.StationIdx) bool { return st != 9 }
	m, err := NewModeAC(DefaultConfig(), sites, anchor)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(2, 3))
	p1 := f.FromENU(geo.Vec3{X: 20e3, Y: 60e3, Z: 9000})
	p2 := f.FromENU(geo.Vec3{X: -70e3, Y: -20e3, Z: 3000})
	t0 := Time{Ns: 1_763_000_000_000_000_000}
	var obs []ACObs
	add := func(p geo.Vec3, at Time, code uint16) {
		for st, r := range sites {
			obs = append(obs, ACObs{Station: st, Code: code, T: at.Add(r.Dist(p)/mPerNs + 30*rng.NormFloat64()), Var: 900})
		}
	}
	add(p1, t0, 0x0A55)
	add(p2, t0.Add(300e3), 0x0A55)       // same code 0.3 ms later
	add(p2, t0.Add(10e3), 0x0A55|0x0040) // X bit set: still the same code
	add(p1, t0.Add(50e3), 0x1234)        // other code
	var got []ACCluster
	n := m.Cluster(obs, func(c *ACCluster) {
		cc := *c
		cc.Members = append([]ACMember(nil), c.Members...)
		got = append(got, cc)
	})
	if n != len(got) || n < 3 {
		t.Fatalf("%d clusters", n)
	}
	for _, c := range got {
		if len(c.Members) != 6 {
			t.Errorf("code %04X: %d members", c.Code, len(c.Members))
		}
		for _, mb := range c.Members {
			if mb.Station == 8 || mb.Station == 9 {
				t.Errorf("station %d used (far or not an anchor)", mb.Station)
			}
		}
		if c.MaxRatio > 1 {
			t.Errorf("ratio %v", c.MaxRatio)
		}
	}
	if _, err := NewModeAC(DefaultConfig(), nil, nil); err == nil {
		t.Fatal("nil sites accepted")
	}
}

func TestFlushEstimator(t *testing.T) {
	f := NewFlushEstimator()
	now := int64(1e18)
	rng := rand.New(rand.NewPCG(4, 4))
	for i := 0; i < 5000; i++ {
		f.ObserveDelay(2, int64(40e6+rng.Float64()*20e6), now)     // fast: 40-60 ms
		f.ObserveDelay(3, int64(250e6+rng.ExpFloat64()*15e6), now) // p99 ≈ 250 + 69 = 319 ms
		f.ObserveDelay(4, int64(900e6), now)                       // very slow
		f.ObserveDelay(5, int64(10e6), now)                        // only a few samples below
	}
	f.NoteHearer(1, 2, now)
	f.NoteHearer(2, 2, now)
	f.NoteHearer(2, 3, now)
	f.NoteHearer(3, 4, now)
	if d := f.Delay(1); d != 150*time.Millisecond {
		t.Errorf("fast only: %v", d)
	}
	want := 250e6 + 15e6*math.Log(100)
	if d := f.Delay(2); math.Abs(float64(d)-want)/want > 0.1 {
		t.Errorf("mixed: %v want ~%v", d, time.Duration(want))
	}
	if d := f.Delay(3); d != 600*time.Millisecond {
		t.Errorf("slow: %v", d)
	}
	if d := f.Delay(99); d != 400*time.Millisecond {
		t.Errorf("unknown: %v", d)
	}
	if q, ok := f.StationQuantile(2, 0.5); !ok || q < 45e6 || q > 60e6 {
		t.Errorf("median %v %v", q, ok)
	}
	if d := f.Delay(2); math.Abs(float64(d)-want)/want > 0.1 {
		t.Errorf("cached quantile clobbered: %v", d)
	}
	// Decay: after the half-life the old samples weigh half.
	for k := int64(1); k <= 8; k++ {
		for i := 0; i < 3000; i++ {
			f.ObserveDelay(3, 50e6, now+k*int64(11*time.Minute))
		}
	}
	if d := f.Delay(2); d > 200*time.Millisecond {
		t.Errorf("after decay: %v", d)
	}
	f.Prune(now + 1)
	if d := f.Delay(2); d != 400*time.Millisecond {
		t.Errorf("after prune: %v", d)
	}
	f.NoteGroup(&Cluster{Key: 7, FirstNs: now, Members: []Member{{Station: 2}, {Station: 2}}})
	if r := f.ac[7]; r == nil || r.n != 1 {
		t.Error("NoteGroup duplicate hearer")
	}
}

func TestConfigNormalize(t *testing.T) {
	c := Config{NSigma: math.NaN(), MinSites: 1, MinSitesAlt: 9, LateSlots: 1, MaxParts: 99}.Normalize()
	if c.NSigma != 4 || c.MinSites != 3 || c.MinSitesAlt != 3 || c.LateSlots != 256 || c.MaxParts != 8 {
		t.Fatalf("%+v", c)
	}
	if DefaultConfig().Normalize() != DefaultConfig() {
		t.Fatal("defaults not stable")
	}
	if _, err := New(Options{}); err == nil {
		t.Fatal("empty options accepted")
	}
	for u, s := range map[Use]string{UseNone: "none", UseTrack: "track", UseSolveAlt: "solve_alt", UseSolve: "solve"} {
		if u.String() != s {
			t.Errorf("%d: %s", u, u.String())
		}
	}
}

func TestObserveAllocs(t *testing.T) {
	w := newWorld(t, 7, 40, 40, 250)
	hint := &fixedHint{pos: map[uint32]geo.Vec3{}, sigma: 1000}
	h := newHarness(t, w, func(o *Options) {
		o.Hints = hint
		o.Sink = SinkFunc(func(*Cluster) {})
		o.Selector = &fakeSel{cand: true, admit: true}
	})
	rng := rand.New(rand.NewPCG(1, 1))
	p := v3(w.ac[0].Pos)
	hint.pos[5] = p
	sts := visibleStations(w, p, 40)
	x := tx{pos: p, at: Time{Ns: h.clk.base}}
	g := groupFor(h, x, sts, df4(30000), 5, 100, rng)
	xb := tx{pos: p, at: x.at.Add(0.74e6)}
	for i, st := range sts {
		if i%3 == 0 {
			g.Copies[i] = copyOf(h.clk, st, toa(w, xb, st, 100, rng))
		}
	}
	late := *g
	late.Copies = g.Copies[:3]
	if n := testing.AllocsPerRun(200, func() { h.a.Observe(g) }); n != 0 {
		t.Fatalf("Observe: %v allocs", n)
	}
	if n := testing.AllocsPerRun(200, func() { h.a.ObserveLate(&late) }); n != 0 {
		t.Fatalf("ObserveLate: %v allocs", n)
	}
}

type domClock struct {
	*tickClock
	dom map[model.StationIdx]uint64
}

func (d domClock) Domain(st model.StationIdx) uint64 { return d.dom[st] }

// TestDomains: stations of different clock components are never clustered
// together, even when their times are consistent.
func TestDomains(t *testing.T) {
	w := newWorld(t, 5, 30, 40, 150)
	p := v3(w.ac[0].Pos)
	sts := visibleStations(w, p, 8)
	if len(sts) < 8 {
		t.Fatal("too few stations")
	}
	h := newHarness(t, w, nil)
	dc := domClock{tickClock: h.clk, dom: map[model.StationIdx]uint64{}}
	for i, st := range sts {
		dc.dom[st] = uint64(1 + i%2)
	}
	a, err := New(Options{Config: DefaultConfig(), Clock: dc, Sites: w.sites, Sink: h.out})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	a.Observe(groupFor(h, tx{pos: p, at: Time{Ns: h.clk.base + 1e9}}, sts, df4(20000), 9, 50, rng))
	if len(h.out.cl) != 2 {
		t.Fatalf("%d clusters", len(h.out.cl))
	}
	for _, c := range h.out.cl {
		d := dc.dom[c.Members[0].Station]
		for _, m := range c.Members {
			if dc.dom[m.Station] != d {
				t.Fatal("cluster spans two time domains")
			}
		}
		if len(c.Members) != 4 {
			t.Fatalf("%d members", len(c.Members))
		}
	}
}
