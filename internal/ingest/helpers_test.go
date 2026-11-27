package ingest

import (
	"container/heap"
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

// ------------------------------------------------------------ fakes

// countHandler is a tracker.NetHandler that counts first copies.
type countHandler struct {
	n, late, updates atomic.Uint64
	mu               sync.Mutex
	byDF             map[uint8]int
	frames           []model.RawFrame // when keep is set
	keep             bool
	lanes            *tracker.Lanes
}

func newCountHandler() *countHandler { return &countHandler{byDF: map[uint8]int{}} }

func (h *countHandler) Handle(f model.RawFrame) tracker.Result {
	if f.Late {
		h.late.Add(1)
		return tracker.Result{Status: tracker.ResultLate}
	}
	h.n.Add(1)
	var df uint8
	if len(f.Data) >= 7 {
		df = f.Data[0] >> 3
	}
	h.mu.Lock()
	h.byDF[df]++
	if h.keep {
		f.Data = append([]byte(nil), f.Data...)
		h.frames = append(h.frames, f)
	}
	h.mu.Unlock()
	key, _ := tracker.RouteKey(f.Data)
	return tracker.Result{Status: tracker.ResultOK, Key: key, DF: df, TMs: f.Recv.UnixMilli()}
}

func (h *countHandler) HandleUpdate(model.DecodedUpdate) { h.updates.Add(1) }
func (h *countHandler) Lanes() *tracker.Lanes            { return h.lanes }

// fakeRes is a StationResolver with settable admission.
type fakeRes struct {
	mu  sync.RWMutex
	adm map[model.StationIdx]string
}

func newFakeRes() *fakeRes { return &fakeRes{adm: map[model.StationIdx]string{}} }

func (r *fakeRes) set(st model.StationIdx, adm string) {
	r.mu.Lock()
	r.adm[st] = adm
	r.mu.Unlock()
}

func (r *fakeRes) Info(st model.StationIdx) (model.StationMeta, bool) {
	r.mu.RLock()
	a, ok := r.adm[st]
	r.mu.RUnlock()
	if !ok {
		a = model.AdmApproved
	}
	return model.StationMeta{Idx: st, Admission: a}, true
}

func (r *fakeRes) Geo(model.StationIdx) (model.StationGeo, bool) { return model.StationGeo{}, false }

// groupSink records MLAT groups (deep copies).
type groupSink struct {
	mu     sync.Mutex
	groups []*Group
	late   []*Group
}

func (s *groupSink) Observe(g *Group) {
	s.mu.Lock()
	s.groups = append(s.groups, g.Clone())
	s.mu.Unlock()
}

func (s *groupSink) ObserveLate(g *Group) {
	s.mu.Lock()
	s.late = append(s.late, g.Clone())
	s.mu.Unlock()
}

// vclock is a virtual clock.
type vclock struct{ ns atomic.Int64 }

func (c *vclock) Now() time.Time { return time.Unix(0, c.ns.Load()) }
func (c *vclock) set(ns int64)   { c.ns.Store(ns) }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestIngest(t testing.TB, h tracker.NetHandler, clk *vclock, mut func(*Options)) *Ingest {
	t.Helper()
	o := Options{Handler: h, Now: clk.Now, Logger: quietLogger()}
	if mut != nil {
		mut(&o)
	}
	ing, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return ing
}

// ------------------------------------------------------------ simnet driver

// arrival is one observation queued for its server arrival time.
type arrivalItem struct {
	at  int64
	seq uint64 // tie breaker: insertion order
	f   model.RawFrame
	tx  uint64 // truth transmission
}

type arrivalHeap []arrivalItem

func (h arrivalHeap) Len() int { return len(h) }
func (h arrivalHeap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h arrivalHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *arrivalHeap) Push(x any)   { *h = append(*h, x.(arrivalItem)) }
func (h *arrivalHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}

// netDriver feeds simnet observations to an Ingest in server-arrival order
// on a virtual clock and records the truth.
type netDriver struct {
	t    testing.TB
	n    *simnet.Network
	ing  *Ingest
	clk  *vclock
	q    arrivalHeap
	nseq uint64
	// per-receiver behaviour
	latencyNs []int64 // link latency
	jitterNs  []int64
	hostErrNs []int64 // extra feeder host clock error
	untimed   []bool  // strip ticks (class none)
	mapOnly   []bool  // Beast 30004 copies
	lastAt    []int64
	rng       uint64

	lastSweep int64
	// truth
	truthTx    map[uint64]uint8 // seq -> DF (or 255 for Mode A/C)
	byTick     map[tickKey]uint64
	byHost     map[hostKey]uint64
	frames     uint64
	maxPending int
}

type hostKey struct {
	st   model.StationIdx
	host int64
	data string
}

type tickKey struct {
	st    model.StationIdx
	epoch uint32
	ticks uint64
}

func newNetDriver(t testing.TB, n *simnet.Network, ing *Ingest, clk *vclock) *netDriver {
	d := &netDriver{t: t, n: n, ing: ing, clk: clk, truthTx: map[uint64]uint8{}, byTick: map[tickKey]uint64{}, byHost: map[hostKey]uint64{}, rng: 0x9E3779B97F4A7C15}
	rx := n.Config().Receivers
	for i := range rx {
		d.latencyNs = append(d.latencyNs, int64(rx[i].Link.LatencyMs*1e6))
		d.jitterNs = append(d.jitterNs, int64(rx[i].Link.JitterMs*1e6))
	}
	d.hostErrNs = make([]int64, len(rx))
	d.untimed = make([]bool, len(rx))
	d.mapOnly = make([]bool, len(rx))
	d.lastAt = make([]int64, len(rx))
	return d
}

func (d *netDriver) rand() uint64 {
	d.rng ^= d.rng << 13
	d.rng ^= d.rng >> 7
	d.rng ^= d.rng << 17
	return d.rng
}

// Observe implements simnet.Sink.
func (d *netDriver) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		f := d.n.RawFrame(o)
		if f.MLAT {
			continue
		}
		f.Data = append([]byte(nil), f.Data...)
		f.Recv = time.Time{}
		f.HostNs += d.hostErrNs[o.Rx]
		df := uint8(255)
		if len(f.Data) >= 7 {
			df = f.Data[0] >> 3
		}
		d.truthTx[o.Seq] = df
		d.byHost[hostKey{f.Station, f.HostNs, string(f.Data)}] = o.Seq
		if d.untimed[o.Rx] {
			f.ClockCls, f.Ticks, f.Epoch = model.ClockNone, 0, 0
			f.GPSNs, f.Clock12MHz = -1, 0
		} else {
			d.byTick[tickKey{f.Station, f.Epoch, f.Ticks}] = o.Seq
		}
		at := o.HostNs + d.latencyNs[o.Rx]
		if j := d.jitterNs[o.Rx]; j > 0 {
			at += int64(d.rand() % uint64(j))
		}
		at = max(at, d.lastAt[o.Rx]) // one TCP link: no reordering
		d.lastAt[o.Rx] = at
		d.nseq++
		heap.Push(&d.q, arrivalItem{at: at, seq: d.nseq, f: f, tx: o.Seq})
		d.frames++
	}
	d.maxPending = max(d.maxPending, len(d.q))
}

// Advance implements simnet.Sink: everything that arrives before the
// horizon is delivered.
func (d *netDriver) Advance(h float64) { d.release(d.n.UnixNs(h)) }

func (d *netDriver) release(until int64) {
	for len(d.q) > 0 && d.q[0].at <= until {
		it := heap.Pop(&d.q).(arrivalItem)
		d.sweepTo(it.at)
		d.clk.set(it.at)
		f := it.f
		d.ing.frame(&f, d.mapOnly[f.Station-2], it.at)
	}
	d.sweepTo(until)
}

func (d *netDriver) sweepTo(t int64) {
	const step = int64(10 * time.Millisecond)
	if d.lastSweep == 0 {
		d.lastSweep = t
	}
	for d.lastSweep+step <= t {
		d.lastSweep += step
		d.clk.set(d.lastSweep)
		d.ing.Sweep(time.Unix(0, d.lastSweep))
		if d.lastSweep%int64(time.Second) < step {
			d.ing.Tick(time.Unix(0, d.lastSweep))
		}
	}
}

func (d *netDriver) run(dur time.Duration) {
	if err := d.n.Advance(context.Background(), dur); err != nil {
		d.t.Fatal(err)
	}
	last := d.lastSweep
	for _, it := range d.q {
		last = max(last, it.at)
	}
	d.release(last)
	d.sweepTo(last + int64(5*time.Second))
	d.ing.Flush()
}

func (d *netDriver) truthCount(df int) int {
	n := 0
	for _, v := range d.truthTx {
		if df < 0 || int(v) == df {
			n++
		}
	}
	return n
}

func newSimNet(t testing.TB, seed uint64, rx, aircraft int, mut func(*simnet.Config)) *simnet.Network {
	return newSimNetFull(t, seed, rx, aircraft, nil, mut)
}

func newSimNetMix(t testing.TB, seed uint64, rx, aircraft int, mix map[simnet.Device]float64) *simnet.Network {
	return newSimNetFull(t, seed, rx, aircraft, mix, nil)
}

func newSimNetFull(t testing.TB, seed uint64, rx, aircraft int, mix map[simnet.Device]float64, mut func(*simnet.Config)) *simnet.Network {
	t.Helper()
	specs := simnet.RandomReceivers(seed, rx, simnet.Placement{CenterLat: 52, CenterLon: 0, RadiusKm: 250, Mix: mix})
	cfg := simnet.Config{Seed: seed, Aircraft: aircraft, Receivers: specs}
	if mut != nil {
		mut(&cfg)
	}
	n, err := simnet.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
