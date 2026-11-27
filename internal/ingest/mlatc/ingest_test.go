package mlatc

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/model"
	"airview/internal/tracker"
)

// netHandler is a minimal tracker.NetHandler: it accepts every first copy.
type netHandler struct{}

func (netHandler) Handle(f model.RawFrame) tracker.Result {
	key, _ := tracker.RouteKey(f.Data)
	var df uint8
	if len(f.Data) > 0 {
		df = f.Data[0] >> 3
	}
	return tracker.Result{Status: tracker.ResultOK, Key: key, DF: df, TMs: f.Recv.UnixMilli()}
}
func (netHandler) HandleUpdate(model.DecodedUpdate) {}
func (netHandler) Lanes() *tracker.Lanes            { return nil }

// groupRec is an ingest.MLATSink keeping every group.
type groupRec struct {
	mu     sync.Mutex
	groups []*ingest.Group
	late   []*ingest.Group
}

func (g *groupRec) Observe(x *ingest.Group) {
	g.mu.Lock()
	g.groups = append(g.groups, x.Clone())
	g.mu.Unlock()
}
func (g *groupRec) ObserveLate(x *ingest.Group) {
	g.mu.Lock()
	g.late = append(g.late, x.Clone())
	g.mu.Unlock()
}

// teeSink records what the server delivers (in order) and forwards it to
// the real ingest.
type teeSink struct {
	Sink
	mu     sync.Mutex
	frames map[model.StationIdx][]model.RawFrame
	n      int
}

func (s *teeSink) Submit(fr []model.RawFrame) {
	s.mu.Lock()
	for _, f := range fr {
		f.Data = append([]byte(nil), f.Data...)
		s.frames[f.Station] = append(s.frames[f.Station], f)
	}
	s.n += len(fr)
	s.mu.Unlock()
	s.Sink.Submit(fr)
}

// TestSimnetIngestEndToEnd runs reference-client streams of a simnet
// network through the server into the real station-aware ingest and
// checks the MLAT groups against the truth: every copy in a group is the
// truth observation of that station (same transmission), and the
// transmissions the clients reported from several stations arrive as
// groups with all their copies.
func TestSimnetIngestEndToEnd(t *testing.T) {
	t.Run("pairs", func(t *testing.T) { runEndToEnd(t, false) })
	t.Run("split_sync", func(t *testing.T) { runEndToEnd(t, true) })
}

func runEndToEnd(t *testing.T, split bool) {
	const nrx = 10
	n := simNetAllDevices(t, 41, nrx, 150)
	clients := make([]*realClient, nrx)
	var all []chunk
	recs := make([]*chunkRec, nrx)
	for i := 0; i < nrx; i++ {
		recs[i] = &chunkRec{rx: i}
		clients[i] = newRealClient(n, i, recs[i])
		clients[i].split = split
	}
	if err := n.Advance(context.Background(), 60*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		all = append(all, r.chunks...)
	}
	clk := newTestClock()
	gr := &groupRec{}
	ing, err := ingest.New(ingest.Options{Handler: netHandler{}, MLAT: gr, Now: clk.Now,
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	tee := &teeSink{Sink: FromIngest(ing), frames: map[model.StationIdx][]model.RawFrame{}}
	mode := SplitSyncOff
	if split {
		mode = SplitSyncOn
	}
	env := startServer(t, Options{Now: clk.Now, SplitSync: mode, Policy: loopbackPolicy}, nil, tee)
	r := &replay{t: t, env: env, clk: clk, clients: map[int]*scriptClient{},
		after: func(ns int64) { ing.Sweep(time.Unix(0, ns)) }}
	r.run(all, int64(20*time.Millisecond))
	clk.Add(5 * time.Second)
	ing.Sweep(clk.Now())
	ing.Flush()

	// truth of every delivered copy: (station, epoch, ticks) -> transmission
	type key struct {
		st    model.StationIdx
		epoch uint32
		ticks uint64
	}
	truth := map[key]uint64{}
	sentBy := map[uint64]map[model.StationIdx]bool{} // transmission -> stations that reported it
	isSync := map[uint64]bool{}
	for i, c := range clients {
		st, _, _ := env.reg.Lookup("uuid:" + mustUUID(t, n.Receiver(i).UUID))
		got := tee.frames[st]
		if len(got) != len(c.sent) {
			t.Fatalf("rx %d: %d delivered, %d sent", i, len(got), len(c.sent))
		}
		for k, f := range got {
			seq := c.sent[k].obs.Seq
			isSync[seq] = isSync[seq] || c.sent[k].sync
			truth[key{st, f.Epoch, f.Ticks}] = seq
			if sentBy[seq] == nil {
				sentBy[seq] = map[model.StationIdx]bool{}
			}
			sentBy[seq][st] = true
		}
	}
	// copies per transmission as the MLAT sink saw them (flush + late)
	seen := map[uint64]map[model.StationIdx]bool{}
	mixed, groups, copies := 0, 0, 0
	for _, list := range [][]*ingest.Group{gr.groups, gr.late} {
		for _, g := range list {
			var seqs []uint64
			for _, c := range g.Copies {
				if !c.Timed {
					continue
				}
				seq, ok := truth[key{c.Station, c.Epoch, c.Ticks}]
				if !ok {
					t.Fatalf("group copy station %d epoch %d ticks %d is not a delivered frame", c.Station, c.Epoch, c.Ticks)
				}
				seqs = append(seqs, seq)
				if seen[seq] == nil {
					seen[seq] = map[model.StationIdx]bool{}
				}
				seen[seq][c.Station] = true
				copies++
			}
			if len(seqs) == 0 {
				continue
			}
			groups++
			sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
			if seqs[0] != seqs[len(seqs)-1] {
				mixed++
			}
		}
	}
	multi, complete, partial := 0, 0, 0
	var cat [2][2]int // [sync][complete]
	for seq, sts := range sentBy {
		if len(sts) < 2 {
			continue
		}
		multi++
		got := seen[seq]
		sy := 0
		if isSync[seq] {
			sy = 1
		}
		switch {
		case len(got) == len(sts):
			complete++
			cat[sy][1]++
		case len(got) > 0:
			partial++
			cat[sy][0]++
		default:
			cat[sy][0]++
		}
	}
	pComplete := float64(complete) / float64(multi)
	pMLAT := float64(cat[0][1]) / float64(max(cat[0][0]+cat[0][1], 1))
	pSync := float64(cat[1][1]) / float64(max(cat[1][0]+cat[1][1], 1))
	t.Logf("complete: mlat frames %.3f %% of %d, sync frames %.3f %% of %d (split_sync=%v)", 100*pMLAT, cat[0][0]+cat[0][1],
		100*pSync, cat[1][0]+cat[1][1], split)
	pMixed := float64(mixed) / float64(max(groups, 1))
	st := ing.Stats()
	t.Logf("%d frames from %d clients; %d transmissions reported by >= 2 stations: %d complete (%.3f %%), %d partial, %d missing; "+
		"%d MLAT groups (%d late deliveries), %d timed copies, %d mixing transmissions (%.3f %%); ingest copies %d, tooOld %d",
		tee.n, nrx, multi, complete, 100*pComplete, partial, multi-complete-partial, len(gr.groups), len(gr.late), copies,
		mixed, 100*pMixed, st.Copies, st.TooOld)
	if multi < 5000 {
		t.Fatalf("too little multi-station traffic: %d", multi)
	}
	// Mode S frames for MLAT must arrive complete. Sync pairs are sent
	// only when the second half arrives, so a station whose pair partner
	// came late delivers its copy of the first half late (after the
	// grouping window): a property of the paired protocol, which
	// split_sync removes.
	if pMLAT < 0.995 {
		t.Errorf("only %.2f %% of multi-station Mode S (mlat) transmissions reached MLAT complete (gate 99.5 %%)", 100*pMLAT)
	}
	if minSync := map[bool]float64{false: 0.8, true: 0.99}[split]; pSync < minSync {
		t.Errorf("only %.2f %% of multi-station sync transmissions complete (gate %.0f %%)", 100*pSync, 100*minSync)
	}
	if pMixed > 0.01 {
		t.Errorf("%.2f %% of MLAT groups mix transmissions (gate 1 %%)", 100*pMixed)
	}
}
