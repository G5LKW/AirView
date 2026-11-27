package tracker

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/store"
)

// fakeRes is a StationResolver over fixed maps.
type fakeRes struct {
	mu  sync.Mutex
	geo map[model.StationIdx]model.StationGeo
	sid map[model.StationIdx]string
}

func newFakeRes() *fakeRes {
	return &fakeRes{geo: map[model.StationIdx]model.StationGeo{}, sid: map[model.StationIdx]string{}}
}

func (f *fakeRes) set(idx model.StationIdx, sid string, lat, lon float64, located bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sid[idx] = sid
	if located {
		f.geo[idx] = model.StationGeo{Lat: lat, Lon: lon, HAE: 60, Source: model.LocSrcManual}
	} else {
		delete(f.geo, idx)
	}
}

func (f *fakeRes) Info(idx model.StationIdx) (model.StationMeta, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sid, ok := f.sid[idx]
	if !ok {
		return model.StationMeta{}, false
	}
	_, located := f.geo[idx]
	return model.StationMeta{Idx: idx, SID: sid, Located: located}, true
}

func (f *fakeRes) Geo(idx model.StationIdx) (model.StationGeo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.geo[idx]
	return g, ok
}

// uniqueFrames records d of simulator output and keeps one copy of each
// transmission (the first antenna channel), as a grouper would.
func uniqueFrames(t testing.TB, aircraft int, d time.Duration) ([]model.RawFrame, model.Config) {
	t.Helper()
	s, cfg := newSim(t, aircraft, nil)
	rec := &recorder{}
	advance(t, s, d, rec)
	out := rec.frames[:0]
	for _, f := range rec.frames {
		if f.Channel <= 0 {
			out = append(out, f)
		}
	}
	return out, cfg
}

// rowTap collects lane rows.
type rowTap struct {
	mu   sync.Mutex
	min  []store.StatsRow
	hour []store.RangeHour
}

func (r *rowTap) tap(min []store.StatsRow, hour []store.RangeHour) {
	r.mu.Lock()
	r.min = append(r.min, min...)
	r.hour = append(r.hour, hour...)
	r.mu.Unlock()
}

func (r *rowTap) frames(ant string) (frames, positions int64, aircraft int, maxKm float64) {
	maxKm = math.NaN()
	for _, row := range r.min {
		if row.Ant != ant {
			continue
		}
		frames += row.Frames
		positions += row.Positions
		aircraft = max(aircraft, row.Aircraft)
		if !(maxKm >= row.MaxKm) && row.MaxKm == row.MaxKm {
			maxKm = row.MaxKm
		}
	}
	return
}

func (r *rowTap) hours(ant string) int {
	n := 0
	for _, row := range r.hour {
		if row.Ant == ant {
			n++
		}
	}
	return n
}

// netFixture is a network tracker fed with simulator traffic: station 1
// (located at the simulator's receiver) hears every transmission first,
// station 2 (located 60 km north) and station 3 (unlocated, channel 0)
// receive copies through Lanes.Credit.
type netFixture struct {
	tr      *Tracker
	ls      *Lanes
	res     *fakeRes
	tap     *rowTap
	cfg     model.Config
	handled int64 // Mode S frames handled
	pos     int64 // results with a position
	fsRows  []FlightStation
	fsMu    sync.Mutex
}

func newNetFixture(t testing.TB, aircraft int, d time.Duration) *netFixture {
	t.Helper()
	frames, cfg := uniqueFrames(t, aircraft, d)
	fx := &netFixture{res: newFakeRes(), tap: &rowTap{}, cfg: cfg}
	rx := cfg.Receiver
	fx.res.set(1, "sid1aaaaaaaaaaaa", rx.Lat, rx.Lon, true)
	nLat, nLon := geo.Destination(rx.Lat, rx.Lon, 0, 60)
	fx.res.set(2, "sid2bbbbbbbbbbbb", nLat, nLon, true)
	fx.res.set(3, "sid3cccccccccccc", 0, 0, false)
	fx.ls = NewLanes(LaneOptions{Resolver: fx.res, OnFlightStation: func(rows []FlightStation) {
		fx.fsMu.Lock()
		fx.fsRows = append(fx.fsRows, rows...)
		fx.fsMu.Unlock()
	}})
	fx.ls.tapRows = fx.tap.tap
	fx.tr = New(Options{Config: &cfg, Logger: quietLog()})
	fx.tr.SetNetwork(fx.res, fx.ls)
	for _, f := range frames {
		f.Station = 1
		r := fx.tr.Handle(f)
		if r.Status == ResultNone {
			continue
		}
		fx.handled++
		if r.Pos {
			fx.pos++
		}
		fx.ls.Credit(2, -1, f.Signal-3, &r)
		fx.ls.Credit(3, 0, f.Signal, &r)
	}
	return fx
}

func TestNetworkHandleAndLanes(t *testing.T) {
	fx := newNetFixture(t, 40, 150*time.Second)
	if err := fx.tr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.handled == 0 || fx.pos == 0 {
		t.Fatalf("handled %d frames, %d positions", fx.handled, fx.pos)
	}
	// Station 3 reports only channel 0: no duplicate "s3/0" rows.
	if frames, _, _, _ := fx.tap.frames("s3/0"); frames != 0 {
		t.Errorf("single-channel station wrote %d channel frames", frames)
	}
	for _, ant := range []string{"s1", "s2", "s3"} {
		frames, positions, aircraft, _ := fx.tap.frames(ant)
		if frames != fx.handled {
			t.Errorf("%s: %d frames in minute rows, want %d", ant, frames, fx.handled)
		}
		if positions != fx.pos {
			t.Errorf("%s: %d positions, want %d", ant, positions, fx.pos)
		}
		if aircraft == 0 {
			t.Errorf("%s: no aircraft gauge", ant)
		}
	}
	// Geometry products only for located stations.
	if fx.tap.hours("s1") == 0 || fx.tap.hours("s2") == 0 {
		t.Errorf("no range_hour rows for located stations: s1 %d s2 %d", fx.tap.hours("s1"), fx.tap.hours("s2"))
	}
	if n := fx.tap.hours("s3") + fx.tap.hours("s3/0"); n != 0 {
		t.Errorf("unlocated station got %d range_hour rows", n)
	}
	_, _, _, max1 := fx.tap.frames("s1")
	if !(max1 > 0 && max1 <= 800) {
		t.Errorf("s1 max range %v", max1)
	}
	// The merged picture stores no receiver geometry.
	snap := fx.tr.Snapshot()
	if len(snap.Aircraft) == 0 {
		t.Fatal("empty snapshot")
	}
	for _, a := range snap.Aircraft {
		if a.DistKm != nil || a.Bearing != nil || a.Elevation != nil || a.SlantKm != nil {
			t.Fatalf("%s: geometry stored in aggregator mode", a.ICAO)
		}
	}
	// heardBy holds all three stations.
	key, err := modes.ParseICAO(snap.Aircraft[0].ICAO)
	if err != nil {
		t.Fatal(err)
	}
	hb := fx.tr.HeardBy(key)
	slices.Sort(hb)
	if !slices.Equal(hb, []model.StationIdx{1, 2, 3}) {
		t.Errorf("heardBy = %v", hb)
	}
	// Lane summaries.
	for _, st := range []model.StationIdx{1, 2, 3} {
		li, ok := fx.ls.Info(st)
		if !ok || li.Frames != uint64(fx.handled) {
			t.Errorf("lane %d: %+v %v", st, li, ok)
		}
		if li.Located != (st != 3) {
			t.Errorf("lane %d located = %v", st, li.Located)
		}
	}
	if n := len(fx.ls.List()); n != 3 {
		t.Errorf("List: %d lanes", n)
	}
	if got := fx.ls.HeardICAOs(2, 0, nil); len(got) == 0 {
		t.Error("HeardICAOs empty")
	}
}

func TestNetworkSnapshotViewScopes(t *testing.T) {
	fx := newNetFixture(t, 30, 60*time.Second)
	rx := fx.cfg.Receiver

	pub := fx.tr.SnapshotView(View{Scope: model.ScopePublic, Station: 1, Own: []model.StationIdx{1}})
	if len(pub.Aircraft) == 0 {
		t.Fatal("empty public view")
	}
	b, _ := json.Marshal(pub)
	for _, k := range []string{`"distKm"`, `"bearing"`, `"stations"`, `"feeds"`, `"antennas"`, `"rssi"`, `"mine"`, `"slantKm"`, `"elevation"`, `"cpaKm"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("public view contains %s", k)
		}
	}
	if pub.Counts.MaxRangeKm != 0 {
		t.Errorf("public max range %v", pub.Counts.MaxRangeKm)
	}

	// Owner of station 2, geometry from station 2.
	own := fx.tr.SnapshotView(View{Scope: model.ScopeOwner, Station: 2, Own: []model.StationIdx{2}})
	nLat, nLon := geo.Destination(rx.Lat, rx.Lon, 0, 60)
	st2 := geo.NewStation(nLat, nLon, 60, 0)
	withGeo := 0
	for _, a := range own.Aircraft {
		if !a.Mine || !slices.Equal(a.Stations, []string{"sid2bbbbbbbbbbbb"}) {
			t.Fatalf("%s: owner Mine=%v Stations=%v", a.ICAO, a.Mine, a.Stations)
		}
		if a.Feeds != nil || a.RSSI != nil {
			t.Fatalf("%s: owner sees feeds / RSSI", a.ICAO)
		}
		if a.Lat != nil && !a.Predicted {
			if a.DistKm == nil || a.Bearing == nil {
				t.Fatalf("%s: no geometry for own station", a.ICAO)
			}
			km := st2.DistanceKm(*a.Lat, *a.Lon)
			if math.Abs(km-*a.DistKm) > 0.5 { // HAE->MSL differs, distance not
				t.Fatalf("%s: dist %v, want %v", a.ICAO, *a.DistKm, km)
			}
			withGeo++
		}
	}
	if withGeo == 0 {
		t.Error("no aircraft with geometry in the owner view")
	}
	if own.Counts.MaxRangeKm <= 0 {
		t.Error("owner view max range not computed")
	}
	// An owner cannot select someone else's station.
	other := fx.tr.SnapshotView(View{Scope: model.ScopeOwner, Station: 1, Own: []model.StationIdx{2}})
	for _, a := range other.Aircraft {
		if a.DistKm != nil {
			t.Fatalf("%s: geometry of a foreign station", a.ICAO)
		}
	}
	// An owner whose station heard nothing.
	none := fx.tr.SnapshotView(View{Scope: model.ScopeOwner, Own: []model.StationIdx{9}})
	for _, a := range none.Aircraft {
		if a.Mine || a.Stations != nil {
			t.Fatalf("%s: Mine for a station that heard nothing", a.ICAO)
		}
	}
	// Admin: every station, any located station's geometry.
	adm := fx.tr.SnapshotView(View{Scope: model.ScopeAdmin, Station: 1})
	all := []string{"sid1aaaaaaaaaaaa", "sid2bbbbbbbbbbbb", "sid3cccccccccccc"}
	for _, a := range adm.Aircraft {
		if !slices.Equal(a.Stations, all) {
			t.Fatalf("%s: admin stations %v", a.ICAO, a.Stations)
		}
	}
	// The base snapshot is untouched by the projections.
	for _, a := range fx.tr.Snapshot().Aircraft {
		if a.DistKm != nil || a.Stations != nil {
			t.Fatal("projection leaked into the shared snapshot")
		}
	}
	// AircraftView agrees with SnapshotView.
	key, _ := modes.ParseICAO(own.Aircraft[0].ICAO)
	a, ok := fx.tr.AircraftView(key, View{Scope: model.ScopeOwner, Station: 2, Own: []model.StationIdx{2}})
	if !ok || !a.Mine || len(a.Stations) != 1 {
		t.Errorf("AircraftView: %v %+v", ok, a)
	}
	if pa, _ := fx.tr.AircraftView(key, View{}); pa.Mine || pa.Stations != nil || pa.Feeds != nil {
		t.Error("AircraftView public leaks receiver fields")
	}
}

func TestStandaloneSnapshotViewUnchanged(t *testing.T) {
	frames, cfg := uniqueFrames(t, 20, 30*time.Second)
	tr := New(Options{Config: &cfg})
	for _, f := range frames {
		tr.HandleFrame(f)
	}
	s := tr.SnapshotView(View{Scope: model.ScopePublic})
	geoN := 0
	for _, a := range s.Aircraft {
		if a.DistKm != nil {
			geoN++
		}
	}
	if geoN == 0 {
		t.Fatal("standalone view lost the receiver geometry")
	}
	if tr.Network() || tr.Lanes() != nil {
		t.Fatal("standalone tracker reports network mode")
	}
}

func TestNetworkRangeGate(t *testing.T) {
	frames, cfg := uniqueFrames(t, 30, 60*time.Second)
	rx := cfg.Receiver
	run := func(setup func(res *fakeRes), credit model.StationIdx) Counters {
		res := newFakeRes()
		setup(res)
		ls := NewLanes(LaneOptions{Resolver: res})
		tr := New(Options{Config: &cfg})
		tr.SetNetwork(res, ls)
		for _, f := range frames {
			f.Station = 1
			r := tr.Handle(f)
			if credit != 0 {
				ls.Credit(credit, -1, f.Signal, &r)
			}
		}
		return tr.Counters()
	}
	// Hearing station far away (> 800 km): every position rejected.
	far := run(func(res *fakeRes) { res.set(1, "far", rx.Lat-30, rx.Lon, true) }, 0)
	if far.Positions != 0 || far.PosRejected == 0 {
		t.Errorf("far station: %d positions, %d rejected", far.Positions, far.PosRejected)
	}
	// Unlocated hearing station: the gate is open.
	open := run(func(res *fakeRes) { res.set(1, "unl", 0, 0, false) }, 0)
	if open.Positions == 0 {
		t.Error("unlocated station: no positions")
	}
	// Far hearing station, but a located near station in heardBy.
	both := run(func(res *fakeRes) {
		res.set(1, "far", rx.Lat-30, rx.Lon, true)
		res.set(2, "near", rx.Lat, rx.Lon, true)
	}, 2)
	if both.Positions == 0 {
		t.Error("near station in heardBy did not open the gate")
	}
}

func TestNetworkLateFrame(t *testing.T) {
	res := newFakeRes()
	res.set(1, "s", 51, 0, true)
	ls := NewLanes(LaneOptions{Resolver: res})
	tap := &rowTap{}
	ls.tapRows = tap.tap
	tr := New(Options{})
	tr.SetNetwork(res, ls)
	now := simStart
	// A live frame starts the lane's minute.
	if r := tr.Handle(model.RawFrame{Recv: now, Kind: model.FrameModeSShort, Data: df11(0x4840D6), Station: 1, Channel: -1, GPSNs: -1, Signal: -10}); r.Status != ResultOK {
		t.Fatalf("live frame status %d", r.Status)
	}
	old := now.Add(-10 * time.Minute)
	late := model.RawFrame{Recv: now, HostNs: old.UnixNano(), Kind: model.FrameModeSShort, Data: df11(0x3C6586), Station: 1, Channel: -1, GPSNs: -1, Signal: -20, Late: true}
	if r := tr.Handle(late); r.Status != ResultLate {
		t.Fatalf("late frame status %d", r.Status)
	}
	if _, ok := tr.Aircraft(0x3C6586); ok {
		t.Fatal("late frame reached the live picture")
	}
	// A parity-address frame of an aircraft the station never heard is rejected.
	late.Data = df4(0x3C6586, 0x1234)
	tr.Handle(late)
	tr.HandleUpdate(model.DecodedUpdate{Recv: old, ICAO: 0x3C6587, Station: 1, Late: true, Msgs: 3})
	ls.FlushAll(now.UnixMilli() + 1000)
	oldMin := old.Unix() - old.Unix()%60
	var got *store.StatsRow
	for i := range tap.min {
		if tap.min[i].T == oldMin && tap.min[i].Ant == "s1" {
			got = &tap.min[i]
		}
	}
	if got == nil {
		t.Fatalf("no late minute row: %+v", tap.min)
	}
	if got.Frames != 5 || got.DF11 != 1 || got.CRCBad != 1 || got.DFOther != 3 || got.Aircraft != 2 {
		t.Errorf("late row %+v", *got)
	}
	if li, _ := ls.Info(1); li.LateFrames != 3 {
		t.Errorf("late frames %d", li.LateFrames)
	}
}

func TestNetworkUpdateCredit(t *testing.T) {
	res := newFakeRes()
	res.set(4, "s4", 51, 0, true)
	ls := NewLanes(LaneOptions{Resolver: res})
	tr := New(Options{})
	tr.SetNetwork(res, ls)
	lat, lon, alt := 51.3, 0.2, 30000.0
	for i := 0; i < 5; i++ {
		tr.HandleUpdate(model.DecodedUpdate{
			Recv: simStart.Add(time.Duration(i) * time.Second), ICAO: 0x4840D6, Station: 4, Msgs: 2,
			Lat: &lat, Lon: &lon, AltBaro: &alt,
		})
	}
	// Own MLAT results credit no lane.
	tr.HandleUpdate(model.DecodedUpdate{Recv: simStart.Add(6 * time.Second), ICAO: 0x4840D7, Lat: &lat, Lon: &lon, MLAT: true, Src: model.SrcMLATAV})
	li, ok := ls.Info(4)
	if !ok || li.Frames != 5 || li.Positions == 0 {
		t.Errorf("lane 4: %+v %v", li, ok)
	}
	if _, ok := ls.Info(0); ok {
		t.Error("MLAT result credited lane 0")
	}
	if hb := tr.HeardBy(0x4840D6); !slices.Equal(hb, []model.StationIdx{4}) {
		t.Errorf("heardBy %v", hb)
	}
}

// laneResult is a synthetic accepted ADS-B frame.
func laneResult(tMs int64, key uint32, flight int64, pos bool, lat, lon float64) Result {
	r := Result{TMs: tMs, Key: key, Status: ResultOK, DF: 17, DFIdx: uint8(modes.DFIndex(17)), Shown: true, FlightID: flight, Class: classADSB}
	if pos {
		r.Pos, r.Lat, r.Lon, r.AltFt, r.PosSrc, r.Cell = true, lat, lon, 30000, store.SrcADSB, geo.HeatCell(lat, lon)
	}
	return r
}

func TestLaneFlightStation(t *testing.T) {
	res := newFakeRes()
	res.set(5, "sid5", 51, 0, true)
	var rows []FlightStation
	ls := NewLanes(LaneOptions{Resolver: res, OnFlightStation: func(r []FlightStation) { rows = append(rows, r...) }})
	base := simStart.UnixMilli()
	for i := 0; i < 10; i++ {
		r := laneResult(base+int64(i)*1000, 0x400001, 42, i%2 == 0, 51.5, float64(i)*0.01)
		ls.Credit(5, -1, -20-float64(i), &r)
	}
	ls.Tick(base + 10_000)
	if len(rows) != 0 {
		t.Fatalf("flight emitted before it closed: %+v", rows)
	}
	ls.closeFlight(0x400001, 42)
	ls.Tick(base + 11_000)
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	fs := rows[0]
	want := geo.NewStation(51, 0, 60, 0).DistanceKm(51.5, 0.08)
	if fs.FlightID != 42 || fs.Station != 5 || fs.SID != "sid5" || fs.ICAO != 0x400001 ||
		fs.Msgs != 10 || fs.Positions != 5 || fs.First != base/1000 || fs.Last != base/1000+9 ||
		fs.RSSIMin != -29 || fs.RSSIMax != -20 || math.Abs(fs.RSSIAvg+24.5) > 1e-9 ||
		math.Abs(fs.MaxDistKm-want) > 0.01 || !(fs.MinDistKm < fs.MaxDistKm) {
		t.Errorf("flight_station %+v (max want %v)", fs, want)
	}
	// A new flight id on the same aircraft closes the previous aggregate.
	r := laneResult(base+20_000, 0x400001, 43, false, 0, 0)
	ls.Credit(5, -1, math.Inf(-1), &r)
	r = laneResult(base+21_000, 0x400001, 44, false, 0, 0)
	ls.Credit(5, -1, math.NaN(), &r)
	ls.Tick(base + 22_000)
	if len(rows) != 2 || rows[1].FlightID != 43 || rows[1].ICAO != 0x400001 || rows[1].Msgs != 1 || !math.IsNaN(rows[1].RSSIAvg) || !math.IsNaN(rows[1].MaxDistKm) {
		t.Fatalf("rows %+v", rows)
	}
	// Stale aggregates are emitted by the slow flush.
	ls.Tick(base + 21_000 + laneStaleMs + 6*60_000)
	if len(rows) != 3 || rows[2].FlightID != 44 {
		t.Fatalf("stale flight not emitted: %+v", rows)
	}
}

func TestLaneMinuteRows(t *testing.T) {
	res := newFakeRes()
	res.set(5, "sid5", 51, 0, true)
	ls := NewLanes(LaneOptions{Resolver: res})
	tap := &rowTap{}
	ls.tapRows = tap.tap
	m0 := simStart.Unix() - simStart.Unix()%60 // minute start
	base := (m0 + 5) * 1000
	credit := func(tMs int64, key uint32, cls uint8, ch int) {
		r := laneResult(tMs, key, 0, true, 51.2, 0.1)
		r.Class = cls
		ls.Credit(5, ch, -15, &r)
	}
	credit(base, 1, classADSB, -1)
	credit(base+1000, 2, classModeS, 1)
	ls.Tick(base + 16_000) // first partial flush of minute m0 (lane 5 flushes at +15 s)
	credit(base+20_000, 3, classMLAT, 1)
	ls.Tick(base + 31_000)                // second partial flush
	credit(base+60_000, 1, classADSB, -1) // next minute
	ls.FlushAll(base + 61_000)
	var gauges []int
	var frames int64
	var secs float64
	for _, r := range tap.min {
		if r.Ant == "s5" && r.T == m0 {
			gauges = append(gauges, r.Aircraft)
			frames += r.Frames
			secs += r.Secs
			if r.ADSB > 1 || r.MLAT > 1 || r.ModeS > 1 {
				t.Errorf("classification %+v", r)
			}
		}
	}
	if len(gauges) < 2 || gauges[0] != 2 || gauges[len(gauges)-1] != 3 {
		t.Errorf("minute gauges %v (want cumulative 2 .. 3)", gauges)
	}
	if frames != 3 {
		t.Errorf("minute frames %d", frames)
	}
	if secs <= 0 || secs > 60 {
		t.Errorf("covered secs %v", secs)
	}
	// Only channel 1 was reported: its rows would duplicate s5's.
	for _, r := range tap.min {
		if r.Ant == "s5/1" {
			t.Errorf("single-channel station wrote a channel row %+v", r)
		}
	}
	if tap.hours("s5") == 0 || tap.hours("s5/1") != 0 {
		t.Errorf("range_hour rows: s5 %d s5/1 %d", tap.hours("s5"), tap.hours("s5/1"))
	}
}

// TestLaneChannelRows checks the per-channel rows of a station that reports
// two antenna channels.
func TestLaneChannelRows(t *testing.T) {
	res := newFakeRes()
	res.set(5, "sid5", 51, 0, true)
	ls := NewLanes(LaneOptions{Resolver: res})
	tap := &rowTap{}
	ls.tapRows = tap.tap
	m0 := simStart.Unix() - simStart.Unix()%60
	base := (m0 + 5) * 1000
	credit := func(tMs int64, key uint32, ch int) {
		r := laneResult(tMs, key, 0, true, 51.2, 0.1)
		r.Class = classModeS
		ls.Credit(5, ch, -15, &r)
	}
	credit(base, 1, 0)
	credit(base+1000, 2, 1)
	credit(base+2000, 3, 1)
	ls.FlushAll(base + 31_000)
	g := map[string]int{}
	for _, r := range tap.min {
		if r.T == m0 {
			g[r.Ant] = max(g[r.Ant], r.Aircraft)
		}
	}
	if g["s5"] != 3 || g["s5/0"] != 1 || g["s5/1"] != 2 {
		t.Errorf("gauges %v", g)
	}
	if tap.hours("s5/0") == 0 || tap.hours("s5/1") == 0 {
		t.Error("no channel range_hour rows")
	}
}

func TestLaneEvict(t *testing.T) {
	ls := NewLanes(LaneOptions{})
	base := simStart.UnixMilli()
	r := laneResult(base, 7, 0, false, 0, 0)
	ls.Credit(9, -1, -10, &r)
	ls.Tick(base + 3_600_000)
	if _, ok := ls.Info(9); !ok {
		t.Fatal("lane evicted too early")
	}
	ls.Tick(base + 25*3_600_000)
	if _, ok := ls.Info(9); ok {
		t.Fatal("lane not evicted after 24 h")
	}
	// It comes back on the next copy.
	r.TMs = base + 26*3_600_000
	ls.Credit(9, -1, -10, &r)
	if li, ok := ls.Info(9); !ok || li.Frames != 1 {
		t.Fatalf("lane after eviction %+v", li)
	}
}

func TestHeardBySet(t *testing.T) {
	var h heardBy
	for st := model.StationIdx(1); st <= 20; st++ {
		h.mark(st, 1000+int64(st))
	}
	got := h.list(1020, nil)
	if len(got) != heardMax {
		t.Fatalf("len %d", len(got))
	}
	slices.Sort(got)
	if got[0] != 5 || got[len(got)-1] != 20 {
		t.Errorf("oldest not replaced: %v", got)
	}
	h.mark(7, 1100) // refresh
	if got := h.list(1100, nil); !slices.Equal(got, []model.StationIdx{7}) {
		t.Errorf("after 60 s: %v", got)
	}
	var z heardBy
	z.mark(0, 50)
	if got := z.list(50, nil); !slices.Equal(got, []model.StationIdx{0}) {
		t.Errorf("station 0: %v", got)
	}
	var nilSet *heardBy
	nilSet.mark(1, 1)
	if nilSet.list(1, nil) != nil {
		t.Error("nil set")
	}
}

func TestStraightCPA(t *testing.T) {
	// 10 km north, flying south at 360 kt: passes overhead.
	cpa, in, ok := straightCPA(10, 0, 360, 180)
	if !ok || cpa > 1e-6 || math.Abs(in-10/(360*ktToKmPerS)) > 1e-6 {
		t.Errorf("head-on: %v %v %v", cpa, in, ok)
	}
	// Moving away: now.
	if cpa, in, ok := straightCPA(10, 0, 360, 0); !ok || cpa != 10 || in != 0 {
		t.Errorf("receding: %v %v %v", cpa, in, ok)
	}
	// 3 km offset.
	km := math.Hypot(10, 3)
	brg := math.Atan2(3, 10) * 180 / math.Pi
	if cpa, _, ok := straightCPA(km, brg, 300, 180); !ok || math.Abs(cpa-3) > 1e-6 {
		t.Errorf("offset: %v %v", cpa, ok)
	}
	// Far ahead.
	if _, _, ok := straightCPA(2000, 0, 100, 180); ok {
		t.Error("CPA beyond 30 min reported")
	}
	if _, _, ok := straightCPA(math.NaN(), 0, 100, 180); ok {
		t.Error("NaN accepted")
	}
}

func TestGuessSamples(t *testing.T) {
	frames, cfg := uniqueFrames(t, 30, 90*time.Second)
	tr := New(Options{Config: &cfg})
	var mu sync.Mutex
	var got []GuessSample
	tr.SetGuessSampleHook(func(s []GuessSample) {
		mu.Lock()
		got = append(got, s...)
		mu.Unlock()
	})
	for _, f := range frames {
		tr.HandleFrame(f)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no guess samples")
	}
	last := map[uint32]int64{}
	for _, s := range got {
		if !geo.ValidLatLon(s.Lat, s.Lon) || s.Station != 0 || s.T == 0 {
			t.Fatalf("bad sample %+v", s)
		}
		if p, ok := last[s.ICAO]; ok && s.T-p < guessEveryMs {
			t.Fatalf("%06X: samples %d ms apart", s.ICAO, s.T-p)
		}
		last[s.ICAO] = s.T
	}
	if len(last) < 5 {
		t.Errorf("samples from only %d aircraft", len(last))
	}
}

// TestLanesConcurrent runs Handle, Credit, Tick and views concurrently (for
// the race detector).
func TestLanesConcurrent(t *testing.T) {
	frames, cfg := uniqueFrames(t, 30, 30*time.Second)
	res := newFakeRes()
	for st := model.StationIdx(1); st <= 40; st++ {
		res.set(st, "sid", cfg.Receiver.Lat+float64(st)*0.01, cfg.Receiver.Lon, st%3 != 0)
	}
	ls := NewLanes(LaneOptions{Resolver: res})
	tr := New(Options{Config: &cfg})
	tr.SetNetwork(res, ls)
	var wg sync.WaitGroup
	const workers = 4
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(frames); i += workers {
				f := frames[i]
				f.Station = model.StationIdx(1 + i%40)
				r := tr.Handle(f)
				for c := 1; c <= 3; c++ {
					ls.Credit(model.StationIdx(1+(i+c*7)%40), c-2, f.Signal, &r)
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			tr.SnapshotView(View{Scope: model.ScopeAdmin, Station: 1})
			ls.List()
		}
	}()
	wg.Wait()
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(ls.List()); n != 40 {
		t.Errorf("%d lanes", n)
	}
}
