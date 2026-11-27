package group

import (
	"container/heap"
	"context"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

// End-to-end: simnet -> real ingest grouper -> Assembler, with a truth
// clock (every station perfectly synced up to a residual offset error and its
// own timestamp jitter) and the truth transmission of every copy.

type nopHandler struct{}

func (nopHandler) Handle(f model.RawFrame) tracker.Result {
	key, _ := tracker.RouteKey(f.Data)
	var df uint8
	if len(f.Data) > 0 {
		df = f.Data[0] >> 3
	}
	return tracker.Result{Status: tracker.ResultOK, Key: key, DF: df, TMs: f.Recv.UnixMilli()}
}
func (nopHandler) HandleUpdate(model.DecodedUpdate) {}
func (nopHandler) Lanes() *tracker.Lanes            { return nil }

// tickKey identifies a copy. The payload hash is part of it: two
// different frames can be stamped with the same ticks by one station (two
// aircraft whose replies happen to arrive together).
type tickKey struct {
	st    model.StationIdx
	epoch uint32
	ticks uint64
	data  uint64
}

func payloadHash(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h = (h ^ uint64(c)) * 1099511628211
	}
	return h
}

type truthObs struct {
	seq uint64
	txS float64
	ac  uint32
	t   Time // true antenna arrival + clock residual + jitter
}

// truthClock is a Clock from the simnet truth.
type truthClock struct {
	obs    map[tickKey]truthObs
	offset map[model.StationIdx]float64 // residual sync error, ns
	sigmaC float64
	jitter map[model.StationIdx]float64 // ns² reported
	unsync map[model.StationIdx]bool
	cur    uint64 // payload hash of the group being processed
}

func (c *truthClock) NetTime(st model.StationIdx, epoch uint32, ticks uint64, _ model.FrameKind) (Time, float64, float64, bool) {
	o, ok := c.obs[tickKey{st, epoch, ticks, c.cur}]
	if !ok || c.unsync[st] {
		return Time{}, 0, 0, false
	}
	return o.t, c.sigmaC * c.sigmaC, c.jitter[st], true
}

type simItem struct {
	at  int64
	seq uint64
	f   model.RawFrame
}
type simHeap []simItem

func (h simHeap) Len() int { return len(h) }
func (h simHeap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h simHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *simHeap) Push(x any)   { *h = append(*h, x.(simItem)) }
func (h *simHeap) Pop() any {
	o := *h
	it := o[len(o)-1]
	*h = o[:len(o)-1]
	return it
}

// simDriver feeds simnet observations to the ingest in arrival order on a
// virtual clock (the same arrangement as the ingest's own gate tests).
type simDriver struct {
	t      testing.TB
	n      *simnet.Network
	ing    *ingest.Ingest
	now    int64
	q      simHeap
	nseq   uint64
	sweep  int64
	clk    *truthClock
	rng    *rand.Rand
	lastAt map[int]int64
	slow   map[int]bool // receivers delivering 450-800 ms late
	order  []tickAt     // truth entries in insertion order (pruned after 10 s)
	head   int
}

type tickAt struct {
	k  tickKey
	at int64
}

func (d *simDriver) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		f := d.n.RawFrame(o)
		if f.MLAT || o.Kind == model.FrameModeAC {
			continue
		}
		f.Data = append([]byte(nil), f.Data...)
		hz := o.Class.NominalHz()
		errNs := d.n.TickError(o) * 1e9 / hz
		tt := MakeTime(d.n.UnixNs(0), 0).Add(o.ArrivalS*1e9 + o.MultipathNs + o.OutlierNs + errNs + d.clk.offset[f.Station])
		tk := tickKey{f.Station, f.Epoch, f.Ticks, payloadHash(f.Data)}
		d.clk.obs[tk] = truthObs{seq: o.Seq, t: tt, txS: o.Tx.TxS, ac: o.Tx.ICAO}
		d.order = append(d.order, tickAt{tk, o.HostNs})
		if _, ok := d.clk.jitter[f.Station]; !ok {
			j := o.Class.PriorJitterNs()
			d.clk.jitter[f.Station] = j * j
			d.clk.offset[f.Station] = d.rng.NormFloat64() * d.clk.sigmaC
		}
		lat := 20e6 + d.rng.Float64()*30e6
		if d.slow[o.Rx] {
			lat += 450e6 + d.rng.Float64()*350e6
		}
		at := max(o.HostNs+int64(lat), d.lastAt[o.Rx])
		d.lastAt[o.Rx] = at
		d.nseq++
		heap.Push(&d.q, simItem{at: at, seq: d.nseq, f: f})
	}
}

func (d *simDriver) Advance(h float64) {
	d.release(d.n.UnixNs(h))
	for cut := d.n.UnixNs(h) - 10e9; d.head < len(d.order) && d.order[d.head].at < cut; d.head++ {
		delete(d.clk.obs, d.order[d.head].k)
	}
	if d.head > 1<<16 && d.head > len(d.order)/2 {
		d.order = append(d.order[:0], d.order[d.head:]...)
		d.head = 0
	}
}

func (d *simDriver) release(until int64) {
	for len(d.q) > 0 && d.q[0].at <= until {
		it := heap.Pop(&d.q).(simItem)
		d.sweepTo(it.at)
		d.now = it.at
		d.ing.Submit([]model.RawFrame{it.f}, false)
	}
	d.sweepTo(until)
}

func (d *simDriver) sweepTo(t int64) {
	const step = int64(10 * time.Millisecond)
	if d.sweep == 0 {
		d.sweep = t
	}
	for d.sweep+step <= t {
		d.sweep += step
		d.now = d.sweep
		d.ing.Sweep(time.Unix(0, d.sweep))
	}
}

// truthHint predicts from the simnet truth, displaced by a Gaussian error of
// its sigma.
type truthHint struct {
	n     *simnet.Network
	sigma float64
	rng   *rand.Rand
}

func (h *truthHint) Predict(key uint32, at Time) (geo.Vec3, float64, bool) {
	s := float64(at.Ns-h.n.UnixNs(0)) / 1e9
	a, ok := h.n.AircraftAt(key&0xFFFFFF, s)
	if !ok {
		return geo.Vec3{}, 0, false
	}
	e := geo.Vec3{X: h.rng.NormFloat64(), Y: h.rng.NormFloat64(), Z: h.rng.NormFloat64()}.Scale(h.sigma)
	return v3(a.Pos).Add(e), h.sigma, true
}
func (h *truthHint) AltitudeFt(uint32, Time) (float64, bool) { return 0, false }

type e2eResult struct {
	groups, mixedGroups      int // ingest groups (>= 2 usable copies) and those mixing transmissions
	clusters, mixed, solve   int
	mixedSolve               int
	usable, inPure           int // usable copies; copies placed in a pure cluster of their transmission
	byDF                     map[uint8][2]int
	stats                    Stats
	ingestFlushed, ingestLat uint64
}

func runE2E(t *testing.T, seed uint64, rx, aircraft int, dur time.Duration, hintSigma float64, slowEvery int) e2eResult {
	t.Helper()
	specs := simnet.RandomReceivers(seed, rx, simnet.Placement{CenterLat: 52, CenterLon: 0, RadiusKm: 250})
	n, err := simnet.New(simnet.Config{Seed: seed, Aircraft: aircraft, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	sites := siteMap{}
	for _, r := range n.Receivers() {
		sites[model.StationIdx(r.Index+2)] = v3(r.Pos)
	}
	clk := &truthClock{obs: map[tickKey]truthObs{}, offset: map[model.StationIdx]float64{}, sigmaC: 30,
		jitter: map[model.StationIdx]float64{}}
	res := e2eResult{byDF: map[uint8][2]int{}}
	var asm *Assembler
	sink := SinkFunc(func(c *Cluster) {
		res.clusters++
		seqs := map[uint64]int{}
		for _, m := range c.Members {
			seqs[clk.obs[tickKey{m.Station, m.Epoch, m.Ticks, payloadHash(c.Payload())}].seq]++
		}
		if c.Use == UseSolve {
			res.solve++
		}
		x := res.byDF[c.DF]
		x[0]++
		if len(seqs) > 1 {
			if os.Getenv("GROUP_DEBUG") != "" && res.mixed < 5 {
				for _, m := range c.Members {
					o := clk.obs[tickKey{m.Station, m.Epoch, m.Ticks, payloadHash(c.Payload())}]
					println("mixed DF", c.DF, "seq", o.seq, "txS", o.txS, "ac", o.ac, "rx", m.Ticks, "st", m.Station, "t", m.T.Ns%1e9, "late", m.Late, "hinted", c.Hinted, "parts", c.Parts)
				}
			}
			res.mixed++
			x[1]++
			if c.Use == UseSolve {
				res.mixedSolve++
			}
		} else if !c.Late {
			res.inPure += len(c.Members)
		}
		res.byDF[c.DF] = x
	})
	// Count ingest-level mixing on the way in.
	pre := &groupTap{clk: clk, res: &res}
	var hints Hints
	if hintSigma > 0 {
		hints = &truthHint{n: n, sigma: hintSigma, rng: rand.New(rand.NewPCG(seed, 5))}
	}
	asm, err = New(Options{Config: DefaultConfig(), Clock: clk, Sites: sites, Hints: hints, Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	pre.next = asm
	d := &simDriver{t: t, n: n, clk: clk, rng: rand.New(rand.NewPCG(seed, 7)), lastAt: map[int]int64{}, slow: map[int]bool{}}
	for i := 0; slowEvery > 0 && i < rx; i += slowEvery {
		d.slow[i] = true
	}
	ing, err := ingest.New(ingest.Options{Handler: nopHandler{}, MLAT: pre, Now: func() time.Time { return time.Unix(0, d.now) },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	d.ing = ing
	n.AddSink(d)
	if err := n.Advance(context.Background(), dur); err != nil {
		t.Fatal(err)
	}
	d.release(d.sweep + int64(3*time.Second))
	ing.Flush()
	res.stats = asm.Stats()
	st := ing.Stats()
	res.ingestFlushed, res.ingestLat = st.Flushed, st.LateMLAT
	return res
}

// groupTap counts what the ingest delivers, then forwards to the assembler.
type groupTap struct {
	clk  *truthClock
	res  *e2eResult
	next *Assembler
}

func (g *groupTap) Observe(gr *ingest.Group) {
	g.clk.cur = payloadHash(gr.Data)
	seqs := map[uint64]bool{}
	usable := 0
	for _, c := range gr.Copies {
		if !c.Timed || c.SameStation || c.MapOnly {
			continue
		}
		if o, ok := g.clk.obs[tickKey{c.Station, c.Epoch, c.Ticks, g.clk.cur}]; ok {
			seqs[o.seq] = true
			usable++
		}
	}
	if usable >= 2 {
		g.res.groups++
		g.res.usable += usable
		if len(seqs) > 1 {
			g.res.mixedGroups++
		}
	}
	g.next.Observe(gr)
}

func (g *groupTap) ObserveLate(gr *ingest.Group) {
	g.clk.cur = payloadHash(gr.Data)
	g.next.ObserveLate(gr)
}

// TestSimnetEndToEnd runs real simnet traffic through the real ingest
// grouper and the assembler and compares every cluster with the truth.
func TestSimnetEndToEnd(t *testing.T) {
	dur := 30 * time.Second
	if testing.Short() {
		dur = 10 * time.Second
	}
	if os.Getenv("GROUP_LONG") != "" {
		dur = 5 * time.Minute
	}
	for _, tc := range []struct {
		name string
		hint float64
		slow int
	}{{"baseline", 0, 0}, {"hint1km", 1000, 0}, {"slow-feeders", 1000, 6}} {
		t.Run(tc.name, func(t *testing.T) {
			r := runE2E(t, 21, 60, 250, dur, tc.hint, tc.slow)
			ingMixed := float64(r.mixedGroups) / float64(max(r.groups, 1))
			mixed := float64(r.mixed) / float64(max(r.clusters, 1))
			mixedSolve := float64(r.mixedSolve) / float64(max(r.solve, 1))
			kept := float64(r.inPure) / float64(max(r.usable, 1))
			t.Logf("%s %v: ingest groups %d (mixing %.3f %%); clusters %d (solve %d), mixed %d = %.4f %% (solve: %.4f %%); on-time usable copies in a pure cluster %.4f; split groups %d, hinted %d, hint conflicts %d, late attached %d",
				tc.name, dur, r.groups, 100*ingMixed, r.clusters, r.solve, r.mixed, 100*mixed, 100*mixedSolve, kept,
				r.stats.Split, r.stats.Hinted, r.stats.HintConflict, r.stats.LateAttached)
			t.Logf("  ingest flushed %d, late deliveries %d; late clusters %d", r.ingestFlushed, r.ingestLat, r.stats.LateClusters)
			var dfs []int
			for df := range r.byDF {
				dfs = append(dfs, int(df))
			}
			sort.Ints(dfs)
			for _, df := range dfs {
				x := r.byDF[uint8(df)]
				t.Logf("  DF%-2d clusters %7d mixed %4d", df, x[0], x[1])
			}
			if tc.slow > 0 && (r.stats.LateAttached == 0 || r.stats.LateClusters == 0) {
				t.Fatalf("slow feeders but no late copies joined: %+v", r.stats)
			}
			if r.groups < 1000 {
				t.Fatalf("too few groups: %d", r.groups)
			}
			// Mixed groups must be split, not passed on: cluster mixing well
			// below the ingest's group mixing; with a hint, (nearly) none.
			maxMixed := 0.25 * ingMixed
			if tc.hint > 0 {
				maxMixed = 0.0005
			}
			if mixed > maxMixed || kept < 0.97 {
				t.Fatalf("mixed %.5f (max %.5f), kept %.4f", mixed, maxMixed, kept)
			}
			if math.IsNaN(kept) {
				t.Fatal("NaN")
			}
		})
	}
}
