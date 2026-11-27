package tracker

import (
	"context"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/store"
)

// netPair builds an aggregator tracker with n shards (1 = unsharded) on its
// own store, with located stations around the simulator's receiver.
func netPair(t testing.TB, cfg model.Config, n int, st *store.Store) (*Tracker, *Lanes) {
	t.Helper()
	res := newFakeRes()
	for s := model.StationIdx(1); s <= 20; s++ {
		res.set(s, "sid", cfg.Receiver.Lat+float64(s%5)*0.05, cfg.Receiver.Lon+float64(s/5)*0.05, true)
	}
	ls := NewLanes(LaneOptions{Resolver: res})
	tr := New(Options{Config: &cfg, Store: st, Logger: quietLog(), Shards: n})
	tr.SetNetwork(res, ls)
	return tr, ls
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenWithOptions(t.TempDir(), store.Options{Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestShardedMatchesUnsharded feeds the same unique transmissions to an
// unsharded and a 4-shard tracker and compares everything they publish:
// the live picture, counters, stored minute rows, flights and records.
func TestShardedMatchesUnsharded(t *testing.T) {
	frames, cfg := uniqueFrames(t, 120, 150*time.Second)
	st1, st4 := openTestStore(t), openTestStore(t)
	t1, _ := netPair(t, cfg, 1, st1)
	t4, _ := netPair(t, cfg, 4, st4)
	if t1.sh.Load() != nil {
		t.Fatal("Shards: 1 must not shard")
	}
	s := t4.sh.Load()
	if s == nil || s.n != 4 {
		t.Fatal("Shards: 4 did not shard")
	}
	for i, f := range frames {
		f.Station = model.StationIdx(1 + i%20)
		r1, r4 := t1.Handle(f), t4.Handle(f)
		if r1.Status != r4.Status || r1.Key != r4.Key || r1.Pos != r4.Pos || r1.Lat != r4.Lat || r1.Lon != r4.Lon {
			t.Fatalf("frame %d: results differ: %+v vs %+v", i, r1, r4)
		}
	}
	// Every aircraft lives in exactly one shard: its route.
	for i, p := range s.parts {
		p.mu.Lock()
		for key := range p.ac {
			if ShardOf(key, s.n) != i {
				t.Errorf("aircraft %06X in shard %d, routes to %d", key, i, ShardOf(key, s.n))
			}
		}
		p.mu.Unlock()
	}

	a, b := t1.Snapshot(), t4.Snapshot()
	if a.Counts.Aircraft == 0 || a.Counts.Aircraft != b.Counts.Aircraft || a.Counts.WithPos != b.Counts.WithPos ||
		a.Counts.ADSB != b.Counts.ADSB || a.Counts.ModeS != b.Counts.ModeS || a.Counts.MLAT != b.Counts.MLAT {
		t.Fatalf("counts differ: %+v vs %+v", a.Counts, b.Counts)
	}
	if math.Abs(a.Counts.MsgRate-b.Counts.MsgRate) > 0.02*a.Counts.MsgRate+1 {
		t.Errorf("message rate %v vs %v", a.Counts.MsgRate, b.Counts.MsgRate)
	}
	for i := range a.Aircraft {
		x, y := a.Aircraft[i], b.Aircraft[i]
		if x.ICAO != y.ICAO || x.Callsign != y.Callsign || x.Msgs != y.Msgs || !eqp(x.Lat, y.Lat) || !eqp(x.Lon, y.Lon) ||
			!eqp(x.AltBaro, y.AltBaro) || !eqp(x.GS, y.GS) || x.Source != y.Source || x.Squawk != y.Squawk {
			t.Errorf("aircraft %d differs:\n%+v\n%+v", i, x, y)
		}
		if i > 0 && b.Aircraft[i-1].ICAO >= y.ICAO {
			t.Errorf("merged snapshot not in ICAO order at %d", i)
		}
	}
	// Projections and per-aircraft look-ups route to the right shard.
	v := View{Scope: model.ScopeAdmin, Station: 1}
	pa, pb := t1.SnapshotView(v), t4.SnapshotView(v)
	for i := range pa.Aircraft {
		if pa.Aircraft[i].ICAO != pb.Aircraft[i].ICAO || !eqp(pa.Aircraft[i].DistKm, pb.Aircraft[i].DistKm) ||
			len(pa.Aircraft[i].Stations) != len(pb.Aircraft[i].Stations) {
			t.Errorf("view of %s differs", pa.Aircraft[i].ICAO)
		}
	}
	for _, ac := range a.Aircraft[:min(20, len(a.Aircraft))] {
		key, err := modes.ParseICAO(ac.ICAO)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := t4.Aircraft(key); !ok {
			t.Errorf("Aircraft(%s) not found", ac.ICAO)
		}
		if _, ok := t4.AircraftView(key, v); !ok {
			t.Errorf("AircraftView(%s) not found", ac.ICAO)
		}
		if len(t4.HeardBy(key)) != len(t1.HeardBy(key)) {
			t.Errorf("HeardBy(%s): %v vs %v", ac.ICAO, t4.HeardBy(key), t1.HeardBy(key))
		}
		if len(t4.Trail(key)) != len(t1.Trail(key)) {
			t.Errorf("Trail(%s) differs", ac.ICAO)
		}
	}
	if len(t4.Trails(0)) != len(t1.Trails(0)) {
		t.Errorf("Trails: %d vs %d", len(t4.Trails(0)), len(t1.Trails(0)))
	}

	c1, c4 := t1.Counters(), t4.Counters()
	if c1.Frames != c4.Frames || c1.Accepted != c4.Accepted || c1.Positions != c4.Positions || c1.Flights != c4.Flights ||
		c1.CRCBad != c4.CRCBad || c1.Rejected != c4.Rejected || c1.Tracked != c4.Tracked || c1.Aircraft != c4.Aircraft {
		t.Errorf("counters differ:\n%+v\n%+v", c1, c4)
	}
	for k, v := range c1.DF {
		if c4.DF[k] != v {
			t.Errorf("DF %s: %d vs %d", k, v, c4.DF[k])
		}
	}
	if len(c1.Feeds) != len(c4.Feeds) || len(c1.Feeds) > 0 && c1.Feeds[0].Frames != c4.Feeds[0].Frames {
		t.Errorf("feeds differ: %+v vs %+v", c1.Feeds, c4.Feeds)
	}
	if ls := t4.LockStats(); ls.Shards != 4 {
		t.Errorf("lock stats shards %d", ls.Shards)
	}

	ctx := context.Background()
	if err := t1.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := t4.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	from, to := frames[0].Recv.Unix()-3600, frames[len(frames)-1].Recv.Unix()+3600
	r1, err := st1.MinuteStats(ctx, "", from, to)
	if err != nil {
		t.Fatal(err)
	}
	r4, err := st4.MinuteStats(ctx, "", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1) < 3 || len(r1) != len(r4) {
		t.Fatalf("minute rows: %d vs %d", len(r1), len(r4))
	}
	for i := range r1 {
		x, y := r1[i], r4[i]
		if x.T != y.T || x.Frames != y.Frames || x.DF17 != y.DF17 || x.DF11 != y.DF11 || x.DF4 != y.DF4 ||
			x.Positions != y.Positions || x.MLATPositions != y.MLATPositions || x.CRCBad != y.CRCBad {
			t.Errorf("minute %d counters differ:\n%+v\n%+v", x.T, x, y)
		}
		// Gauges: each shard classifies its aircraft at its own minute roll
		// (within a frame of the unsharded one): equal up to an aircraft
		// whose class or listing changes in that instant.
		if d := x.Aircraft - y.Aircraft; d < -1 || d > 1 || abs(x.AircraftPos-y.AircraftPos) > 1 || abs(x.ADSB-y.ADSB) > 1 {
			t.Errorf("minute %d gauges differ: %d/%d/%d vs %d/%d/%d", x.T, x.Aircraft, x.AircraftPos, x.ADSB, y.Aircraft, y.AircraftPos, y.ADSB)
		}
		if math.Abs(x.RSSIAvg-y.RSSIAvg) > 1e-6 || x.RSSIMin != y.RSSIMin || x.RSSIMax != y.RSSIMax {
			t.Errorf("minute %d rssi differs: %v %v %v vs %v %v %v", x.T, x.RSSIAvg, x.RSSIMin, x.RSSIMax, y.RSSIAvg, y.RSSIMin, y.RSSIMax)
		}
		if math.Abs(x.Secs-y.Secs) > 1 {
			t.Errorf("minute %d secs %v vs %v", x.T, x.Secs, y.Secs)
		}
	}

	f1, err := st1.RecentFlights(ctx, 10000, 0)
	if err != nil {
		t.Fatal(err)
	}
	f4, err := st4.RecentFlights(ctx, 10000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(f1) == 0 || len(f1) != len(f4) {
		t.Fatalf("flights: %d vs %d", len(f1), len(f4))
	}
	byICAO := map[uint32]store.Flight{}
	ids := map[int64]bool{}
	for _, f := range f4 {
		byICAO[f.ICAO] = f
		if ids[f.ID] {
			t.Errorf("flight id %d used twice", f.ID)
		}
		ids[f.ID] = true
	}
	for _, f := range f1 {
		g, ok := byICAO[f.ICAO]
		if !ok || g.Msgs != f.Msgs || g.Positions != f.Positions || g.Start != f.Start || g.End != f.End {
			t.Errorf("flight of %06X differs:\n%+v\n%+v", f.ICAO, f, g)
		}
	}
	rec := func(st *store.Store) map[string]float64 {
		rows, err := st.Records(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]float64{}
		for _, r := range rows {
			m[r.Key] = r.Value
		}
		return m
	}
	rr1, rr4 := rec(st1), rec(st4)
	for _, k := range []string{RecordMaxAltFt, RecordMaxGSKt, RecordMaxFlightS} {
		if rr1[k] != rr4[k] {
			t.Errorf("record %s: %v vs %v", k, rr1[k], rr4[k])
		}
	}
	if d := rr1[RecordMaxAircraft] - rr4[RecordMaxAircraft]; d < -2 || d > 2 || rr4[RecordMaxAircraft] == 0 {
		t.Errorf("record %s: %v vs %v", RecordMaxAircraft, rr1[RecordMaxAircraft], rr4[RecordMaxAircraft])
	}
}

func eqp(a, b *float64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b || *a != *a && *b != *b)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// TestShardCount checks how the shard count is chosen.
func TestShardCount(t *testing.T) {
	cfg := model.DefaultConfig()
	mk := func(opt, conf int) int {
		c := cfg
		c.Aggregator.Limits.TrackerShards = conf
		tr := New(Options{Config: &c, Shards: opt, Logger: quietLog()})
		tr.SetNetwork(newFakeRes(), NewLanes(LaneOptions{}))
		if s := tr.sh.Load(); s != nil {
			return s.n
		}
		return 1
	}
	if n := mk(3, 0); n != 3 {
		t.Errorf("Options.Shards 3: %d", n)
	}
	if n := mk(0, 5); n != 5 {
		t.Errorf("config 5: %d", n)
	}
	if n := mk(1, 8); n != 1 {
		t.Errorf("Options.Shards 1 overrides the config: %d", n)
	}
	if n := mk(0, 0); n != AutoShards() {
		t.Errorf("auto: %d, want %d", n, AutoShards())
	}
	if n := mk(1000, 0); n != MaxShards {
		t.Errorf("clamp: %d", n)
	}
	p := runtime.GOMAXPROCS(0)
	if a := AutoShards(); p < 4 && a != 1 || p >= 4 && (a < 2 || a > 8) {
		t.Errorf("AutoShards %d at GOMAXPROCS %d", a, p)
	}
	// Standalone trackers never shard.
	tr := New(Options{Config: &cfg, Shards: 4})
	if tr.sh.Load() != nil {
		t.Error("standalone tracker sharded")
	}
	// A tracker that already has data is not split.
	s, scfg := newSim(t, 10, nil)
	tr = New(Options{Config: &scfg, Shards: 4, Logger: quietLog()})
	advance(t, s, 5*time.Second, tr)
	tr.SetNetwork(newFakeRes(), NewLanes(LaneOptions{}))
	if tr.sh.Load() != nil {
		t.Error("tracker with data was sharded")
	}
}

// TestShardedBackToStandalone: SetNetwork(nil, nil) drops the shards and the
// tracker works unsharded again.
func TestShardedBackToStandalone(t *testing.T) {
	frames, cfg := uniqueFrames(t, 30, 20*time.Second)
	tr, _ := netPair(t, cfg, 4, nil)
	for _, f := range frames[:len(frames)/2] {
		f.Station = 1
		tr.Handle(f)
	}
	if tr.Snapshot().Counts.Aircraft == 0 {
		t.Fatal("no aircraft")
	}
	tr.SetNetwork(nil, nil)
	if tr.sh.Load() != nil || tr.Network() {
		t.Fatal("still sharded / networked")
	}
	for _, f := range frames[len(frames)/2:] {
		tr.HandleFrame(f)
	}
	if tr.Counters().Frames == 0 {
		t.Error("standalone tracker got no frames")
	}
}

// TestShardedConcurrent hammers a sharded tracker from many goroutines
// (frames, updates, reads, Run) and checks that nothing is lost.
func TestShardedConcurrent(t *testing.T) {
	frames, cfg := uniqueFrames(t, 80, 30*time.Second)
	tr, ls := netPair(t, cfg, 8, nil)
	var notices atomic.Int64
	tr.SetOnNotice(func(Notice) { notices.Add(1) })
	var inHook, maxIn atomic.Int32
	tr.SetGuessSampleHook(func([]GuessSample) {
		n := inHook.Add(1)
		if n > maxIn.Load() {
			maxIn.Store(n)
		}
		time.Sleep(50 * time.Microsecond)
		inHook.Add(-1)
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = tr.Run(ctx); close(runDone) }()
	const workers = 8
	var wg sync.WaitGroup
	var handled atomic.Uint64
	modeS := 0
	for _, f := range frames {
		if (f.Kind == model.FrameModeSShort || f.Kind == model.FrameModeSLong) && (len(f.Data) == 7 || len(f.Data) == 14) {
			modeS++
		}
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < len(frames); i += workers {
				f := frames[i]
				f.Station = model.StationIdx(1 + i%20)
				r := tr.Handle(f)
				handled.Add(1)
				if r.Status == ResultOK {
					ls.Credit(model.StationIdx(1+(i+3)%20), -1, -40, &r)
				}
				if i%997 == 0 {
					tr.HandleUpdate(model.DecodedUpdate{ICAO: 0xABC123, Recv: f.Recv, Station: 2, FeedID: "x"})
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			s := tr.SnapshotView(View{Scope: model.ScopeAdmin, Station: 1})
			for j := 1; j < len(s.Aircraft); j++ {
				if s.Aircraft[j-1].ICAO >= s.Aircraft[j].ICAO {
					t.Error("snapshot out of order")
					return
				}
			}
			_ = tr.Counters()
			_ = tr.LockStats()
			_ = tr.FeedLatency()
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	cancel()
	<-runDone
	c := tr.Counters()
	if c.Frames != uint64(modeS) || handled.Load() != uint64(len(frames)) {
		t.Errorf("frames %d, want %d (handled %d)", c.Frames, modeS, handled.Load())
	}
	if c.Updates == 0 {
		t.Error("updates lost")
	}
	if maxIn.Load() > 1 {
		t.Errorf("guess hook ran %d times concurrently", maxIn.Load())
	}
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestShardedRouting checks that every frame kind reaches the shard of its
// aircraft, including parity-address frames (the address is in the CRC
// residual) and frames without an address (shard 0).
func TestShardedRouting(t *testing.T) {
	frames, cfg := uniqueFrames(t, 40, 30*time.Second)
	tr, _ := netPair(t, cfg, 5, nil)
	s := tr.sh.Load()
	byDF := map[uint8]int{}
	for _, f := range frames {
		f.Station = 1
		r := tr.Handle(f)
		if r.Status != ResultOK {
			continue
		}
		a, ok := RouteKey(f.Data)
		if !ok || a != r.Key&0xFFFFFF {
			t.Fatalf("DF%d: route key %06X ok=%v, applied to %06X", r.DF, a, ok, r.Key)
		}
		byDF[r.DF]++
	}
	for _, df := range []uint8{4, 5, 11, 17, 20, 21} {
		if byDF[df] == 0 {
			t.Logf("no accepted DF%d in the sample", df)
		}
	}
	if byDF[17] == 0 || byDF[11] == 0 {
		t.Fatalf("accepted frames per DF: %v", byDF)
	}
	tr.Handle(model.RawFrame{Kind: model.FrameModeAC, Data: []byte{0x12, 0x34}, Recv: frames[len(frames)-1].Recv, Station: 1})
	s.parts[0].mu.Lock()
	n := s.parts[0].c.ModeAC
	s.parts[0].mu.Unlock()
	if n != 1 {
		t.Errorf("Mode A/C frame not in shard 0 (%d)", n)
	}
}

// TestShardedReset clears every shard and the shared records.
func TestShardedReset(t *testing.T) {
	frames, cfg := uniqueFrames(t, 30, 30*time.Second)
	tr, _ := netPair(t, cfg, 3, nil)
	for _, f := range frames {
		f.Station = 1
		tr.Handle(f)
	}
	if tr.Counters().Frames == 0 || tr.recs.v[recMaxAlt].Load() == recNone {
		t.Fatal("no data / no altitude record before the reset")
	}
	tr.Reset()
	c := tr.Counters()
	if c.Frames != 0 || c.Tracked != 0 || tr.Snapshot().Counts.Aircraft != 0 {
		t.Errorf("after Reset: %+v", c)
	}
	if tr.recs.v[recMaxAlt].Load() != recNone {
		t.Error("records survived Reset")
	}
}

// TestShardedHooksSerialised: the EHS hook of a sharded tracker never runs
// concurrently with itself.
func TestShardedHooksSerialised(t *testing.T) {
	frames, cfg := uniqueFrames(t, 80, 40*time.Second)
	tr, _ := netPair(t, cfg, 8, nil)
	var in, maxIn, calls atomic.Int32
	tr.SetEHSHook(func(EHSEvent) EHSResult {
		calls.Add(1)
		if n := in.Add(1); n > maxIn.Load() {
			maxIn.Store(n)
		}
		time.Sleep(20 * time.Microsecond)
		in.Add(-1)
		return EHSResult{}
	})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < len(frames); i += 8 {
				f := frames[i]
				f.Station = 1
				tr.Handle(f)
			}
		}()
	}
	wg.Wait()
	if calls.Load() == 0 {
		t.Skip("simulator produced no EHS data")
	}
	if maxIn.Load() > 1 {
		t.Errorf("EHS hook ran %d times concurrently", maxIn.Load())
	}
}

// TestMinuteMerger checks the merge of the shards' minute rows.
func TestMinuteMerger(t *testing.T) {
	var got []store.StatsRow
	m := &minuteMerger{}
	m.init(3, nil)
	m.tap = func(r []store.StatsRow) { got = append(got, r...) }
	row := func(frames int64, ac int, avg, lo, hi, secs float64) store.StatsRow {
		return store.StatsRow{T: 60, Frames: frames, DF17: frames, Aircraft: ac, ADSB: ac, RSSIAvg: avg, RSSIMin: lo, RSSIMax: hi,
			MaxKm: math.NaN(), Secs: secs}
	}
	m.add(0, 60, []store.StatsRow{row(10, 2, -20, -30, -10, 40)}, []int64{10})
	m.add(1, 60, nil, nil) // shard 1 had nothing this minute
	if len(got) != 0 {
		t.Fatal("written before every shard contributed")
	}
	m.add(2, 60, []store.StatsRow{row(30, 5, -10, -25, -5, 45)}, []int64{30})
	if len(got) != 1 {
		t.Fatalf("rows %d", len(got))
	}
	r := got[0]
	if r.Frames != 40 || r.DF17 != 40 || r.Aircraft != 7 || r.ADSB != 7 || r.RSSIMin != -30 || r.RSSIMax != -5 ||
		math.Abs(r.RSSIAvg-(-12.5)) > 1e-9 || r.Secs != 45 || r.T != 60 {
		t.Errorf("merged row %+v", r)
	}

	// A shard that moves on to a newer minute completes the older one; a
	// minute far behind the newest is written incomplete.
	got = nil
	m.add(0, 120, []store.StatsRow{row(1, 1, -20, -20, -20, 60)}, []int64{1})
	m.add(1, 180, []store.StatsRow{row(2, 1, -20, -20, -20, 60)}, []int64{2})
	if len(got) != 0 {
		t.Fatal("minute 120 written before shard 2 contributed")
	}
	m.add(2, 180, []store.StatsRow{row(3, 1, -20, -20, -20, 60)}, []int64{3})
	if len(got) != 1 || got[0].T != 60 && got[0].Frames != 1 {
		t.Fatalf("minute 120: %+v", got)
	}
	m.add(0, 400, nil, nil) // 180 is now more than 2 minutes behind
	if len(got) != 2 || got[1].Frames != 5 {
		t.Fatalf("forced minute 180: %+v", got)
	}

	// A second contribution of one shard to the same minute (final flush)
	// writes the first round and starts a new one.
	got = nil
	m2 := &minuteMerger{}
	m2.init(2, nil)
	m2.tap = func(r []store.StatsRow) { got = append(got, r...) }
	m2.add(0, 60, []store.StatsRow{row(5, 1, -20, -20, -20, 30)}, []int64{5})
	m2.add(0, 60, []store.StatsRow{row(6, 1, -20, -20, -20, 30)}, []int64{6})
	if len(got) != 1 || got[0].Frames != 5 {
		t.Fatalf("double contribution: %+v", got)
	}
	m2.flushAll()
	if len(got) != 2 || got[1].Frames != 6 {
		t.Fatalf("flushAll: %+v", got)
	}
	// Clock stepped back: written as is.
	got = nil
	m2.add(1, 120, []store.StatsRow{row(7, 1, -20, -20, -20, 30)}, []int64{7})
	m2.add(1, 60, []store.StatsRow{row(8, 1, -20, -20, -20, 30)}, []int64{8})
	if len(got) != 1 || got[0].Frames != 8 {
		t.Fatalf("stepped back: %+v", got)
	}
	// Unknown covered seconds win.
	m3 := &minuteMerger{}
	m3.init(2, nil)
	got = nil
	m3.tap = func(r []store.StatsRow) { got = append(got, r...) }
	m3.add(0, 60, []store.StatsRow{row(1, 1, -20, -20, -20, 0)}, []int64{1})
	m3.add(1, 60, []store.StatsRow{row(1, 1, -20, -20, -20, 50)}, []int64{1})
	if len(got) != 1 || got[0].Secs != 0 {
		t.Fatalf("secs: %+v", got)
	}
}

// TestMergeSnapshots checks the k-way merge.
func TestMergeSnapshots(t *testing.T) {
	mk := func(now int64, icaos ...string) model.LiveSnapshot {
		s := model.LiveSnapshot{Now: now}
		for _, x := range icaos {
			s.Aircraft = append(s.Aircraft, model.Aircraft{ICAO: x})
		}
		s.Counts.Aircraft = len(icaos)
		s.Counts.MsgRate = 1.5
		s.Counts.MaxRangeKm = float64(len(icaos))
		return s
	}
	snaps := []model.LiveSnapshot{mk(5, "100000", "400000"), mk(7), mk(6, "200000", "300000", "500000")}
	sides := [][]netSide{{{n: 1}, {n: 4}}, nil, {{n: 2}, {n: 3}, {n: 5}}}
	out, side := mergeSnapshots(snaps, sides, true)
	if out.Now != 7 || out.Counts.Aircraft != 5 || out.Counts.MsgRate != 4.5 || out.Counts.MaxRangeKm != 3 {
		t.Errorf("counts %+v now %d", out.Counts, out.Now)
	}
	for i, a := range out.Aircraft {
		if want := []string{"100000", "200000", "300000", "400000", "500000"}[i]; a.ICAO != want || side[i].n != uint8(i+1) {
			t.Errorf("%d: %s side %d", i, a.ICAO, side[i].n)
		}
	}
}
