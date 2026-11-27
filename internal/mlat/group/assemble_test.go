package group

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/modes"
)

func TestTimeArithmetic(t *testing.T) {
	a := MakeTime(1_763_000_000_123_456_789, 0.25)
	b := a.Add(1234.875)
	if b.Ns != 1_763_000_000_123_458_024 || math.Abs(b.Frac-0.125) > 1e-12 {
		t.Fatalf("Add: %+v", b)
	}
	if d := b.Sub(a); math.Abs(d-1234.875) > 1e-9 {
		t.Fatalf("Sub: %v", d)
	}
	c := a.Add(-0.5)
	if c.Ns != a.Ns-1 || math.Abs(c.Frac-0.75) > 1e-12 || !c.Before(a) || a.Before(c) {
		t.Fatalf("negative Add: %+v", c)
	}
	if m := MakeTime(10, -2.5); m.Ns != 7 || m.Frac != 0.5 {
		t.Fatalf("MakeTime: %+v", m)
	}
	if !(Time{}).IsZero() || a.UnixNs() != a.Ns {
		t.Fatal("IsZero / UnixNs")
	}
}

func TestFrameAltitude(t *testing.T) {
	for _, alt := range []int32{-1000, -975, 0, 25, 1525, 12300, 35000, 41975, 50175, 60000, 99000} {
		code, ok := modes.EncodeAC13(alt)
		if !ok {
			continue
		}
		d := df4(alt)
		got, src := frameAltitude(d)
		if src != AltFrameBaro || got != float64(alt) {
			t.Errorf("AC13 %d: got %v src %d (code %#x)", alt, got, src, code)
		}
		// DF20 carries the same field.
		d20 := make([]byte, 14)
		copy(d20, d)
		d20[0] = 20 << 3
		if got, _ := frameAltitude(d20); got != float64(alt) {
			t.Errorf("DF20 %d: %v", alt, got)
		}
		// Extended squitter airborne position (TC 11) and GNSS height (TC 20).
		c12, ok := modes.EncodeAC12(alt)
		if !ok {
			continue
		}
		for _, tc := range []byte{11, 20} {
			es := make([]byte, 14)
			es[0] = 17<<3 | 5
			es[4] = tc << 3
			es[5] = byte(c12 >> 4)
			es[6] = byte(c12&0xF) << 4
			got, src := frameAltitude(es)
			want := AltFrameBaro
			if tc == 20 {
				want = AltFrameGNSS
			}
			if got != float64(alt) || src != want {
				t.Errorf("ES TC%d %d: %v src %d", tc, alt, got, src)
			}
			es[0] = 18<<3 | 2 // TIS-B
			if _, src := frameAltitude(es); src != AltNone {
				t.Errorf("TIS-B altitude used")
			}
		}
	}
	// Gillham (100 ft) codes, e.g. from old transponders.
	for _, alt := range []int32{-1000, 300, 12300, 62700} {
		ma, ok := modes.EncodeGillham(alt)
		if !ok {
			t.Fatalf("EncodeGillham(%d)", alt)
		}
		code := modes.EncodeID13(ma)
		if v, ok := decodeAC13(code); !ok || v != alt {
			t.Errorf("Gillham %d: %v %v", alt, v, ok)
		}
	}
	if _, src := frameAltitude([]byte{11 << 3, 0, 0, 0, 0, 0, 0}); src != AltNone {
		t.Error("DF11 has no altitude")
	}
	if _, ok := decodeAC13(0); ok {
		t.Error("zero field")
	}
	if _, ok := decodeAC13(0x0040); ok {
		t.Error("metric field")
	}
}

// harness builds an assembler over a world with a tick clock.
type harness struct {
	w   *world
	clk *tickClock
	out *collect
	a   *Assembler
}

func newHarness(t testing.TB, w *world, mut func(*Options)) *harness {
	t.Helper()
	h := &harness{w: w, clk: &tickClock{base: 1_763_000_000_000_000_000, cv: map[model.StationIdx]float64{}, jv: map[model.StationIdx]float64{}}, out: &collect{}}
	o := Options{Config: DefaultConfig(), Clock: h.clk, Sites: w.sites, Sink: h.out}
	if mut != nil {
		mut(&o)
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	h.a = a
	return h
}

func TestSingleTransmission(t *testing.T) {
	w := newWorld(t, 3, 30, 40, 200)
	h := newHarness(t, w, nil)
	rng := rand.New(rand.NewPCG(1, 2))
	for st := range w.sites {
		h.clk.jv[st] = 100 * 100
	}
	nTx := 0
	for _, ac := range w.ac {
		p := v3(ac.Pos)
		x := tx{pos: p, at: Time{Ns: h.clk.base + 1e9}}
		g := &ingest.Group{Hash: 1, Data: df4(30000), Key: 0x123456, DF: 4, Kind: model.FrameModeSShort, FirstNs: x.at.Ns}
		for _, st := range w.st {
			if w.visible(st, p) {
				g.Copies = append(g.Copies, copyOf(h.clk, st, toa(w, x, st, 100, rng)))
			}
		}
		if len(g.Copies) < 4 {
			continue
		}
		nTx++
		before := len(h.out.cl)
		h.a.Observe(g)
		got := h.out.cl[before:]
		if len(got) != 1 {
			t.Fatalf("aircraft %06X: %d clusters from %d copies", ac.ICAO, len(got), len(g.Copies))
		}
		c := got[0]
		if len(c.Members) != len(g.Copies) || c.Use != UseSolve || c.Parts != 1 || c.MaxRatio > 1 {
			t.Fatalf("cluster: %d members of %d, use %v, parts %d, ratio %.3f", len(c.Members), len(g.Copies), c.Use, c.Parts, c.MaxRatio)
		}
		if c.AltSrc != AltFrameBaro || c.AltFt != 30000 {
			t.Fatalf("altitude %v src %d", c.AltFt, c.AltSrc)
		}
		// Ref is the lowest-variance site; members of a site are contiguous.
		for i, s := range c.Sites {
			if s.Var < c.Sites[c.Ref].Var {
				t.Fatalf("ref %d not minimal (site %d)", c.Ref, i)
			}
			for k := s.First; k < s.First+s.Count; k++ {
				if int(c.Members[k].Site) != i {
					t.Fatalf("member %d site %d, want %d", k, c.Members[k].Site, i)
				}
			}
		}
	}
	if nTx < 10 {
		t.Fatalf("only %d transmissions", nTx)
	}
	st := h.a.Stats()
	if st.Clusters != uint64(nTx) || st.Leftover != 0 || st.Split != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// burstResult summarises a burst experiment.
type burstResult struct {
	trials, clusters, mixed, mixedSolve, solve int
	copies, recovered                          int // copies of transmissions with >= 2 copies, and those found in a pure cluster of their own tx
	splitOK                                    int // trials where both transmissions (>= 2 copies each) came out as separate pure clusters
	splitTrials                                int
}

// runBurst emits nTx identical replies spaced by gapNs, each heard by every
// visible station with probability pHear; a station that hears several
// contributes one of its copies to the group (the ingest's repeat rule puts
// the others into other groups). Timing noise: jitter sigmaNs plus a clock
// error with sigma clockNs (fixed per station and trial).
func runBurst(t testing.TB, w *world, gapNs float64, nTx int, hintSigma float64, sigmaNs, clockNs float64, trials int, seed uint64) burstResult {
	var hint *fixedHint
	h := newHarness(t, w, func(o *Options) {
		if hintSigma > 0 {
			hint = &fixedHint{pos: map[uint32]geo.Vec3{}, sigma: hintSigma}
			o.Hints = hint
		}
	})
	for st := range w.sites {
		h.clk.jv[st] = sigmaNs * sigmaNs
		h.clk.cv[st] = clockNs * clockNs
	}
	rng := rand.New(rand.NewPCG(seed, 99))
	var r burstResult
	for trial := 0; trial < trials; trial++ {
		ac := w.ac[trial%len(w.ac)]
		p := v3(ac.Pos)
		key := uint32(0x100000 + trial)
		if hint != nil {
			// The hint is off by an error drawn from its own sigma.
			e := geo.Vec3{X: rng.NormFloat64(), Y: rng.NormFloat64(), Z: rng.NormFloat64()}.Scale(hintSigma)
			hint.pos[key] = p.Add(e)
		}
		t0 := Time{Ns: h.clk.base + int64(trial)*1e8}
		txs := make([]tx, nTx)
		for i := range txs {
			txs[i] = tx{id: i, pos: p, at: t0.Add(float64(i) * gapNs)}
		}
		g := &ingest.Group{Hash: uint64(trial), Data: df4(25000), Key: key, DF: 4, Kind: model.FrameModeSShort, FirstNs: t0.Ns}
		truth := map[model.StationIdx]int{}
		perTx := make([]int, nTx)
		for _, st := range w.st {
			if !w.visible(st, p) {
				continue
			}
			var heard []int
			for i := range txs {
				if rng.Float64() < 0.7 {
					heard = append(heard, i)
				}
			}
			if len(heard) == 0 {
				continue
			}
			i := heard[rng.IntN(len(heard))]
			clockErr := rng.NormFloat64() * clockNs
			g.Copies = append(g.Copies, copyOf(h.clk, st, toa(w, txs[i], st, sigmaNs, rng).Add(clockErr)))
			truth[st] = i
			perTx[i]++
		}
		if len(g.Copies) < 2 {
			continue
		}
		r.trials++
		before := len(h.out.cl)
		h.a.Observe(g)
		pureOf := make([]int, nTx) // copies of tx i recovered in a pure cluster
		for _, c := range h.out.cl[before:] {
			r.clusters++
			ids := map[int]int{}
			for _, m := range c.Members {
				ids[truth[m.Station]]++
			}
			if c.Use == UseSolve {
				r.solve++
			}
			if len(ids) > 1 {
				r.mixed++
				if c.Use == UseSolve {
					r.mixedSolve++
				}
				continue
			}
			for id, n := range ids {
				pureOf[id] = max(pureOf[id], n)
			}
		}
		for i := range perTx {
			if perTx[i] >= 2 {
				r.copies += perTx[i]
				r.recovered += pureOf[i]
			}
		}
		if nTx >= 2 {
			nMulti := 0
			ok := true
			for i := range perTx {
				if perTx[i] >= 2 {
					nMulti++
					if pureOf[i] != perTx[i] {
						ok = false
					}
				}
			}
			if nMulti >= 2 {
				r.splitTrials++
				if ok {
					r.splitOK++
				}
			}
		}
	}
	return r
}

// TestBurstSplit is the WP-GR acceptance test "cluster split of DF4 bursts":
// identical DF4 replies to successive interrogations, mixed into one ingest
// group, must come out as separate clusters, each containing only copies of
// its own transmission.
func TestBurstSplit(t *testing.T) {
	w := newWorld(t, 7, 40, 80, 250)
	cases := []struct {
		name      string
		gapNs     float64
		nTx       int
		hint      float64
		minRecall float64 // fraction of copies recovered in a pure cluster
		maxMixed  float64 // fraction of clusters mixing transmissions
		minSplit  float64 // fraction of two-transmission trials split exactly
	}{
		// Radar interrogations 2.5-50 ms apart: more than any TDOA in the
		// network, so the baseline gate alone separates them exactly.
		// (A copy is lost only to a > 4σ timing tail on a pair whose range
		// difference is close to its baseline: ~1e-4 of copies.)
		{"radar-2.5ms", 2.5e6, 2, 0, 0.999, 0, 0.995},
		{"radar-5ms-x3", 5e6, 3, 0, 0.999, 0, 0.995},
		{"radar-50ms", 50e6, 2, 0, 0.999, 0, 0.995},
		// ACAS pair 0.74 ms apart: with a 1 km track hint the hint gate
		// separates them on every pair.
		{"acas-0.74ms-hint1km", 0.74e6, 2, 1000, 0.995, 0.002, 0.99},
		{"acas-0.74ms-hint3km", 0.74e6, 2, 3000, 0.99, 0.005, 0.97},
		// Without a hint, baselines over 222 km cannot tell 0.74 ms apart;
		// the maximum clique still recovers most copies.
		{"acas-0.74ms-nohint", 0.74e6, 2, 0, 0.6, 0.6, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runBurst(t, w, tc.gapNs, tc.nTx, tc.hint, 150, 50, 600, 11)
			recall := float64(r.recovered) / float64(max(r.copies, 1))
			mixed := float64(r.mixed) / float64(max(r.clusters, 1))
			split := float64(r.splitOK) / float64(max(r.splitTrials, 1))
			t.Logf("%s: trials %d clusters %d (solve %d) mixed %d (solve %d) = %.3f %%, recall %.4f, exact split %d/%d = %.4f",
				tc.name, r.trials, r.clusters, r.solve, r.mixed, r.mixedSolve, 100*mixed, recall, r.splitOK, r.splitTrials, split)
			if r.trials < 400 || r.splitTrials < 100 {
				t.Fatalf("too few trials: %+v", r)
			}
			if recall < tc.minRecall || mixed > tc.maxMixed || split < tc.minSplit {
				t.Fatalf("recall %.4f (min %.4f), mixed %.4f (max %.4f), split %.4f (min %.4f)",
					recall, tc.minRecall, mixed, tc.maxMixed, split, tc.minSplit)
			}
		})
	}
}
