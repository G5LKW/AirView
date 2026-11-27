package ingest

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

// compactObs is a simnet observation reduced to what a RawFrame needs.
type compactObs struct {
	host  int64
	ticks uint64
	epoch uint32
	st    uint16
	cls   model.ClockClass
	n     uint8
	sig   int8
	data  [14]byte
}

type compactSink struct {
	n     *simnet.Network
	stOff uint16 // added to the station indices (several networks)
	out   []compactObs
}

func (c *compactSink) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		if len(o.Data) != 7 && len(o.Data) != 14 && len(o.Data) != 2 {
			continue
		}
		co := compactObs{host: o.HostNs, ticks: o.Ticks, epoch: o.Epoch, st: uint16(o.Rx+2) + c.stOff, cls: o.Class,
			n: uint8(len(o.Data)), sig: int8(o.SignalDBFS)}
		copy(co.data[:], o.Data)
		c.out = append(c.out, co)
	}
}
func (c *compactSink) Advance(float64) {}

func (o *compactObs) frame() model.RawFrame {
	kind := model.FrameModeSLong
	switch o.n {
	case 2:
		kind = model.FrameModeAC
	case 7:
		kind = model.FrameModeSShort
	}
	return model.RawFrame{Station: model.StationIdx(o.st), HostNs: o.host, Ticks: o.ticks, Epoch: o.epoch,
		ClockCls: o.cls, Kind: kind, Data: o.data[:o.n], Signal: float64(o.sig), Channel: -1, GPSNs: -1}
}

// TestSimnet1000Throughput is the WP-I acceptance load test: a simulated
// network of 1,000 stations (DESIGN §20.2) is replayed into the ingest by
// GOMAXPROCS goroutines, each owning a set of stations and delivering in
// arrival order, first with a no-op tracker (the ingest's own capacity),
// then with the real tracker and lanes. It reports copies/s, unique
// transmissions/s, the grouper's accuracy against the simulator's truth
// and memory. Run with INGEST_LOAD=1 (it needs ~2 GB and a minute).
//
//	INGEST_LOAD=1 go test ./internal/ingest -run Simnet1000 -v
//	INGEST_LOAD_STATIONS=1000 INGEST_LOAD_AIRCRAFT=1500 INGEST_LOAD_SECONDS=6
//	INGEST_LOAD_SHARDS=1,0 (tracker shard counts to compare; 0 = automatic)
//	INGEST_LOAD_PACED=1 (replay at real time: the CPU report is then the
//	cores a live network of that size needs)
func TestSimnet1000Throughput(t *testing.T) {
	if os.Getenv("INGEST_LOAD") == "" {
		t.Skip("set INGEST_LOAD=1 to run the 1,000-station load test")
	}
	env := func(k string, d int) int {
		if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
			return v
		}
		return d
	}
	nst, nac, secs := env("INGEST_LOAD_STATIONS", 1000), env("INGEST_LOAD_AIRCRAFT", 1500), env("INGEST_LOAD_SECONDS", 6)
	paced := os.Getenv("INGEST_LOAD_PACED") != ""
	t0 := time.Now()
	// simnet simulates at most simnet.MaxAircraft aircraft: larger skies are
	// several networks side by side (same area and start, own seeds), each
	// with its share of the stations and aircraft (station indices and
	// transmission numbers kept apart), replayed as one.
	nets := (nac + simnet.MaxAircraft - 1) / simnet.MaxAircraft
	var obs []compactObs
	truth := map[uint64]struct{}{}
	for j := 0; j < nets; j++ {
		seed := uint64(31 + j)
		specs := simnet.RandomReceivers(seed, nst/nets, simnet.Placement{CenterLat: 51, CenterLon: 5, RadiusKm: 600})
		n, err := simnet.New(simnet.Config{Seed: seed, Aircraft: nac / nets, Receivers: specs})
		if err != nil {
			t.Fatal(err)
		}
		cs := &compactSink{n: n, stOff: uint16(j * (nst / nets)), out: obs}
		n.AddSink(cs)
		ts := &truthSink{seen: truth, net: uint64(j)}
		n.AddSink(ts)
		if err := n.Advance(context.Background(), time.Duration(secs)*time.Second); err != nil {
			t.Fatal(err)
		}
		obs = cs.out
	}
	sort.SliceStable(obs, func(i, j int) bool { return obs[i].host < obs[j].host })
	span := float64(obs[len(obs)-1].host-obs[0].host) / 1e9
	t.Logf("simnet: %d stations, %d aircraft (%d network(s)), %.1f s of traffic: %d copies of %d transmissions (%.0f copies/s, %.0f unique/s, %.1f copies per transmission) in %v",
		nst, nac, nets, span, len(obs), len(truth), float64(len(obs))/span, float64(len(truth))/span,
		float64(len(obs))/float64(len(truth)), time.Since(t0).Round(time.Second))

	run := func(name string, h tracker.NetHandler) {
		workers := runtime.GOMAXPROCS(0)
		parts := make([][]compactObs, workers)
		for i := range obs {
			w := int(obs[i].st) % workers
			parts[w] = append(parts[w], obs[i])
		}
		// The replay runs faster than real time, so the server clock is
		// virtual: the stations advance in lockstep windows of 20 ms of
		// traffic (as real links deliver concurrently), the sweeper runs
		// after each window.
		const window = int64(20 * time.Millisecond)
		var vnow atomic.Int64
		vnow.Store(obs[0].host)
		ing, err := New(Options{Handler: h, Logger: quietLogger(), Now: func() time.Time { return time.Unix(0, vnow.Load()) }})
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		start := time.Now()
		cpu0 := processCPU()
		pos := make([]int, workers)
		bufs := make([][]model.RawFrame, workers)
		for w := range bufs {
			bufs[w] = make([]model.RawFrame, 0, 64)
		}
		end := obs[len(obs)-1].host
		var wg sync.WaitGroup
		for T := obs[0].host + window; T < end+window; T += window {
			vnow.Store(T + int64(20*time.Millisecond)) // ~20 ms network latency
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					p, i, buf := parts[w], pos[w], bufs[w]
					for i < len(p) && p[i].host <= T {
						buf = buf[:0]
						for ; i < len(p) && p[i].host <= T && len(buf) < cap(buf); i++ {
							buf = append(buf, p[i].frame())
						}
						ing.Submit(buf, false)
					}
					pos[w] = i
				}(w)
			}
			wg.Wait()
			ing.Sweep(time.Unix(0, vnow.Load()))
			if paced {
				// Real time: the CPU report then reads as the cores a live
				// network of this size needs.
				if d := time.Duration(T-obs[0].host) - time.Since(start); d > 0 {
					time.Sleep(d)
				}
			}
		}
		el := time.Since(start)
		cpu := processCPU() - cpu0
		var ms1 runtime.MemStats
		runtime.ReadMemStats(&ms1)
		live := ing.Stats().Live
		ing.Flush()
		st := ing.Stats()
		errPct := 100 * (float64(st.Groups) - float64(len(truth))) / float64(len(truth))
		t.Logf("%s: %d workers, %d copies in %v = %.2f M copies/s (%.0f ns/copy/core, %.1fx real time); unique %d (truth %d, %+.3f %%), joined %d (beyond MaxCopies %d), tooOld %d; live groups %d, heap %.0f MB",
			name, workers, len(obs), el.Round(time.Millisecond), float64(len(obs))/el.Seconds()/1e6,
			el.Seconds()*1e9*float64(workers)/float64(len(obs)), span/el.Seconds(), st.Groups, len(truth), errPct,
			st.Joined, st.Full, st.TooOld, live, float64(ms1.HeapAlloc)/1e6)
		if cpu > 0 {
			t.Logf("%s: CPU %.1f s for %.1f s of traffic = %.2f cores at real time; %.0f ns CPU per copy, %.0f ns CPU per unique transmission",
				name, cpu.Seconds(), span, cpu.Seconds()/span, float64(cpu.Nanoseconds())/float64(len(obs)),
				float64(cpu.Nanoseconds())/float64(st.Groups))
		}
	}
	run("nop tracker", &nopHandler{})
	// The real tracker and lanes, unsharded and ICAO-sharded
	// (INGEST_LOAD_SHARDS, default "1,0": 0 = tracker.AutoShards()).
	shards := os.Getenv("INGEST_LOAD_SHARDS")
	if shards == "" {
		shards = "1,0"
	}
	for _, s := range strings.Split(shards, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			t.Fatalf("INGEST_LOAD_SHARDS: %v", err)
		}
		if n == 0 {
			n = tracker.AutoShards()
		}
		cfg := model.Config{}
		res := newFakeRes()
		lanes := tracker.NewLanes(tracker.LaneOptions{Resolver: res})
		tr := tracker.New(tracker.Options{Config: &cfg, Logger: quietLogger(), Shards: n, MaxAircraft: 4 * nac})
		tr.SetNetwork(res, lanes)
		run(fmt.Sprintf("real tracker+lanes, %d shard(s)", n), tr)
		ls := tr.LockStats()
		t0 := time.Now()
		snap := tr.Snapshot()
		t.Logf("tracker (%d shards): %d aircraft (snapshot %v), mutex hold %.0f ns per unique frame, busiest shard %.0f %% of a core at replay speed",
			ls.Shards, snap.Counts.Aircraft, time.Since(t0).Round(time.Microsecond), ls.HoldNs, 100*ls.MaxShardUtilization)
	}
}

type truthSink struct {
	seen map[uint64]struct{}
	net  uint64 // network number (several networks)
}

func (s *truthSink) Observe(obs []simnet.Observation) {
	for i := range obs {
		s.seen[s.net<<56|obs[i].Seq] = struct{}{}
	}
}
func (s *truthSink) Advance(float64) {}
