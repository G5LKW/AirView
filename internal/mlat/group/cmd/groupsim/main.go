// Command groupsim runs a simnet network through the real ingest grouper, the
// MLAT cluster assembler (internal/mlat/group) and the planner
// (internal/mlat/plan), and reports cluster purity against the simulator's
// truth, plan sizes and throughput. The clock filter is replaced by the
// simnet truth (every station synced up to a residual offset error), so the
// numbers isolate the grouping and planning stages.
//
//	groupsim -rx 300 -aircraft 1200 -dur 2m -listen 127.0.0.1:18603
//
// While it runs, GET /stats on -listen returns the current counters as JSON.
package main

import (
	"container/heap"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/group"
	"airview/internal/mlat/plan"
	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

type key struct {
	st    model.StationIdx
	epoch uint32
	ticks uint64
	data  uint64
}

type truth struct {
	seq uint64
	t   group.Time
}

func hash(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h = (h ^ uint64(c)) * 1099511628211
	}
	return h
}

// truthClock converts timestamps with the simnet truth.
type truthClock struct {
	obs    map[key]truth
	jitter map[model.StationIdx]float64
	cur    uint64
	sigC   float64
}

func (c *truthClock) NetTime(st model.StationIdx, ep uint32, ticks uint64, _ model.FrameKind) (group.Time, float64, float64, bool) {
	o, ok := c.obs[key{st, ep, ticks, c.cur}]
	return o.t, c.sigC * c.sigC, c.jitter[st], ok
}

type sites map[model.StationIdx]geo.Vec3

func (s sites) Site(st model.StationIdx) (geo.Vec3, float64, bool) {
	p, ok := s[st]
	return p, 0, ok
}

type stations map[model.StationIdx]plan.StationInfo

func (s stations) Station(st model.StationIdx) (plan.StationInfo, bool) {
	i, ok := s[st]
	return i, ok
}

type handler struct{}

func (handler) Handle(f model.RawFrame) tracker.Result {
	k, _ := tracker.RouteKey(f.Data)
	var df uint8
	if len(f.Data) > 0 {
		df = f.Data[0] >> 3
	}
	r := tracker.Result{Status: tracker.ResultOK, Key: k, DF: df}
	// An airborne position squitter "produces a position" (what the real
	// tracker reports), so the planner sees which aircraft have ADS-B.
	if (df == 17 || (df == 18 && f.Data[0]&7 < 2)) && len(f.Data) == 14 {
		if tc := f.Data[4] >> 3; (tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22) {
			r.Pos = true
		}
	}
	return r
}
func (handler) HandleUpdate(model.DecodedUpdate) {}
func (handler) Lanes() *tracker.Lanes            { return nil }

type item struct {
	at, seq int64
	f       model.RawFrame
}
type pq []item

func (h pq) Len() int { return len(h) }
func (h pq) Less(i, j int) bool {
	return h[i].at < h[j].at || (h[i].at == h[j].at && h[i].seq < h[j].seq)
}
func (h pq) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pq) Push(x any)   { *h = append(*h, x.(item)) }
func (h *pq) Pop() any {
	o := *h
	it := o[len(o)-1]
	*h = o[:len(o)-1]
	return it
}

// counters shared with the HTTP handler.
type counters struct {
	mu        sync.Mutex
	SimS      float64        `json:"simSeconds"`
	Groups    int            `json:"groups"`
	Clusters  int            `json:"clusters"`
	Mixed     int            `json:"mixed"`
	ByUse     map[string]int `json:"byUse"`
	Group     group.Stats    `json:"group"`
	Plan      plan.Summary   `json:"plan"`
	WallS     float64        `json:"wallSeconds"`
	Copies    int            `json:"copies"`
	PerCopyNs float64        `json:"assemblerNsPerCopy"`
}

type driver struct {
	n      *simnet.Network
	ing    *ingest.Ingest
	clk    *truthClock
	q      pq
	nseq   int64
	now    int64
	sweep  int64
	rng    *rand.Rand
	lastAt map[int]int64
	pl     *plan.Planner
	nextRP int64
	// truth entries in insertion order, pruned 10 s after their arrival
	order []keyAt
	head  int
}

type keyAt struct {
	k  key
	at int64
}

func (d *driver) prune(before int64) {
	for d.head < len(d.order) && d.order[d.head].at < before {
		delete(d.clk.obs, d.order[d.head].k)
		d.head++
	}
	if d.head > 1<<16 && d.head > len(d.order)/2 {
		d.order = append(d.order[:0], d.order[d.head:]...)
		d.head = 0
	}
}

func (d *driver) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		if o.Kind == model.FrameModeAC {
			continue
		}
		f := d.n.RawFrame(o)
		if f.MLAT {
			continue
		}
		f.Data = append([]byte(nil), f.Data...)
		errNs := d.n.TickError(o) * 1e9 / o.Class.NominalHz()
		t := group.MakeTime(d.n.UnixNs(0), 0).Add(o.ArrivalS*1e9 + o.MultipathNs + o.OutlierNs + errNs)
		k := key{f.Station, f.Epoch, f.Ticks, hash(f.Data)}
		d.clk.obs[k] = truth{seq: o.Seq, t: t}
		d.order = append(d.order, keyAt{k, o.HostNs})
		if _, ok := d.clk.jitter[f.Station]; !ok {
			j := o.Class.PriorJitterNs()
			d.clk.jitter[f.Station] = j * j
		}
		at := max(o.HostNs+int64(20e6+d.rng.Float64()*60e6), d.lastAt[o.Rx])
		d.lastAt[o.Rx] = at
		d.nseq++
		heap.Push(&d.q, item{at: at, seq: d.nseq, f: f})
	}
}

func (d *driver) Advance(h float64) {
	d.release(d.n.UnixNs(h))
	d.prune(d.n.UnixNs(h) - 10e9)
}

func (d *driver) release(until int64) {
	for len(d.q) > 0 && d.q[0].at <= until {
		it := heap.Pop(&d.q).(item)
		d.sweepTo(it.at)
		d.now = it.at
		d.ing.Submit([]model.RawFrame{it.f}, false)
	}
	d.sweepTo(until)
}

func (d *driver) sweepTo(t int64) {
	if d.sweep == 0 {
		d.sweep = t
	}
	for d.sweep+10e6 <= t {
		d.sweep += 10e6
		d.now = d.sweep
		d.ing.Sweep(time.Unix(0, d.sweep))
		if d.sweep >= d.nextRP {
			d.pl.Replan(d.sweep)
			d.nextRP = d.sweep + 5e9
		}
	}
}

// tap counts groups, informs the planner and forwards to the assembler.
type tap struct {
	clk *truthClock
	pl  *plan.Planner
	asm *group.Assembler
	c   *counters
	ns  time.Duration
}

func (t *tap) Observe(g *ingest.Group) {
	t.clk.cur = hash(g.Data)
	t.pl.ObserveGroup(g)
	t.c.mu.Lock()
	t.c.Groups++
	t.c.Copies += len(g.Copies)
	t.c.mu.Unlock()
	st := time.Now()
	t.asm.Observe(g)
	t.ns += time.Since(st)
}

func (t *tap) ObserveLate(g *ingest.Group) {
	t.clk.cur = hash(g.Data)
	t.asm.ObserveLate(g)
}

func main() {
	rx := flag.Int("rx", 200, "receivers")
	ac := flag.Int("aircraft", 800, "aircraft")
	modeS := flag.Float64("modes", 0.3, "Mode S-only fraction")
	dur := flag.Duration("dur", time.Minute, "simulated duration")
	seed := flag.Uint64("seed", 1, "seed")
	listen := flag.String("listen", "127.0.0.1:18603", "stats listener (empty = none)")
	all := flag.Bool("all", false, "cluster every aircraft (no planner candidate filter)")
	flag.Parse()

	specs := simnet.RandomReceivers(*seed, *rx, simnet.Placement{CenterLat: 51, CenterLon: 5, RadiusKm: 400})
	n, err := simnet.New(simnet.Config{Seed: *seed, Aircraft: *ac, Receivers: specs, ModeSOnlyFraction: *modeS})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st := sites{}
	sts := stations{}
	for _, r := range n.Receivers() {
		idx := model.StationIdx(r.Index + 2)
		p := geo.Vec3{X: r.Pos[0], Y: r.Pos[1], Z: r.Pos[2]}
		st[idx] = p
		sts[idx] = plan.StationInfo{Pos: p, Located: true, Synced: true, Anchor: r.Class.AnchorCapable(), Class: r.Class}
	}
	clk := &truthClock{obs: map[key]truth{}, jitter: map[model.StationIdx]float64{}, sigC: 30}
	c := &counters{ByUse: map[string]int{}}
	drv := &driver{n: n, clk: clk, rng: rand.New(rand.NewPCG(*seed, 3)), lastAt: map[int]int64{}, now: n.UnixNs(0)}
	pl, err := plan.New(plan.Options{Config: plan.DefaultConfig(), Stations: sts,
		Now: func() time.Time { return time.Unix(0, drv.now) }})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sink := group.SinkFunc(func(cl *group.Cluster) {
		seqs := map[uint64]bool{}
		h := hash(cl.Payload())
		for _, m := range cl.Members {
			seqs[clk.obs[key{m.Station, m.Epoch, m.Ticks, h}].seq] = true
		}
		c.mu.Lock()
		c.Clusters++
		c.ByUse[cl.Use.String()]++
		if len(seqs) > 1 {
			c.Mixed++
		}
		c.mu.Unlock()
	})
	opts := group.Options{Config: group.DefaultConfig(), Clock: clk, Sites: st, Sink: sink}
	if !*all {
		opts.Selector = pl
	}
	asm, err := group.New(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tp := &tap{clk: clk, pl: pl, asm: asm, c: c}
	drv.pl = pl
	ing, err := ingest.New(ingest.Options{Handler: handler{}, MLAT: tp, Now: func() time.Time { return time.Unix(0, drv.now) },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	drv.ing = ing
	n.AddSink(drv)

	snapshot := func() []byte {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.Group = asm.Stats()
		c.Plan = pl.Summary()
		c.SimS = n.NowS()
		if c.Copies > 0 {
			c.PerCopyNs = float64(tp.ns.Nanoseconds()) / float64(c.Copies)
		}
		b, _ := json.MarshalIndent(c, "", "  ")
		return b
	}
	var srv *http.Server
	if *listen != "" {
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(snapshot())
		})
		srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		fmt.Fprintf(os.Stderr, "stats on http://%s/stats\n", ln.Addr())
	}
	start := time.Now()
	if err := n.Advance(context.Background(), *dur); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	drv.release(drv.sweep + 3e9)
	ing.Flush()
	c.mu.Lock()
	c.WallS = time.Since(start).Seconds()
	c.mu.Unlock()
	os.Stdout.Write(append(snapshot(), '\n'))
	if srv != nil {
		_ = srv.Close()
	}
}
