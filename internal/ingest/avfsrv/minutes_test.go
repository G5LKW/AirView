package avfsrv

import (
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/ingest"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/uplink"
)

func mfDF17(icao uint32, me uint64) []byte {
	f := make([]byte, 14)
	f[0] = 0x8D
	f[1], f[2], f[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(me >> (48 - 8*i))
	}
	modes.SetParity(f, 0)
	return f
}

func mfDF11(icao uint32) []byte {
	f := []byte{0x5D, byte(icao >> 16), byte(icao >> 8), byte(icao), 0, 0, 0}
	modes.SetParity(f, 0)
	return f
}

func mfDF4(icao uint32) []byte {
	f := []byte{0x20, 0x00, 0x13, 0x37, 0, 0, 0}
	modes.SetParity(f, icao)
	return f
}

func TestMinuteCountsArithmetic(t *testing.T) {
	var c MinuteCounts
	c.countFrame(mfDF17(0x400001, 0x58C382D690C8AC)) // TC 11 position
	c.countFrame(mfDF17(0x400001, 0x202CC371C32CE0)) // TC 4 ident
	c.countFrame(mfDF11(0x400001))
	c.countFrame(mfDF4(0x400001))
	c.countFrame([]byte{0x12, 0x34})
	c.countFrame([]byte{0xF8, 0, 0, 0, 0, 0, 0}) // DF 31 -> bin 24
	if c.Frames != 6 || c.ModeAC != 1 || c.Positions != 1 || c.DF[17] != 2 || c.DF[11] != 1 || c.DF[4] != 1 || c.DF[24] != 1 {
		t.Fatalf("%+v", c)
	}
	ex := avf.MinuteStats{Minute: 60, DF: map[string]uint64{"17": 10, "11": 5, "4": 3, "30": 1, "x": 9}, ModeAC: 2, Positions: 6, ICAOs: 4}
	e := ExactCounts(&ex)
	if e.Frames != 21 || e.DF[17] != 10 || e.DF[24] != 1 || e.ModeAC != 2 || e.Positions != 6 {
		t.Fatalf("exact %+v", e)
	}
	sm := StationMinute{Exact: ex, ReceivedKnown: true, Received: MinuteCounts{Frames: 8, DF: [25]uint64{4: 3, 11: 1, 17: 4}, Positions: 2}}
	sh := sm.Shed()
	if sh.Frames != 13 || sh.DF[17] != 6 || sh.DF[11] != 4 || sh.DF[4] != 0 || sh.ModeAC != 2 || sh.Positions != 4 || sh.DF[24] != 1 {
		t.Fatalf("shed %+v", sh)
	}
	if mg := sm.Merged(); mg != e {
		t.Fatalf("merged %+v want %+v", mg, e)
	}
	// received above exact (a partial record): merged takes the larger
	sm.Received.DF[4] = 7
	if mg := sm.Merged(); mg.DF[4] != 7 || mg.Frames != 21 {
		t.Fatalf("merged %+v", mg)
	}
	sm.ReceivedKnown = false
	if sm.Shed() != (MinuteCounts{}) || sm.Merged() != e {
		t.Fatal("unknown received must use the exact counts only")
	}

	// two parts of one minute
	a := avf.MinuteStats{RxNo: 1, DF: map[string]uint64{"17": 5}, ICAOs: 7, Positions: 2, CRCFail: 1, RSSI: &avf.RSSIHist{Lo: -50, Counts: []uint64{1, 2, 3}}}
	b := avf.MinuteStats{RxNo: 2, DF: map[string]uint64{"17": 4, "11": 1}, ICAOs: 9, Positions: 1, ModeAC: 3, RSSI: &avf.RSSIHist{Lo: -49, Counts: []uint64{10, 20, 30}}}
	mergeMinuteStats(&a, &b)
	if a.DF["17"] != 9 || a.DF["11"] != 1 || a.ICAOs != 9 || a.Positions != 3 || a.ModeAC != 3 || a.CRCFail != 1 || a.RxNo != 2 {
		t.Fatalf("merged %+v", a)
	}
	if a.RSSI.Counts[0] != 1 || a.RSSI.Counts[1] != 12 || a.RSSI.Counts[2] != 53 {
		t.Fatalf("rssi %v", a.RSSI.Counts)
	}
}

func TestStationMinutesWindow(t *testing.T) {
	var s stationMinutes
	base := int64(1_763_000_040) // a minute start
	now := (base + 90) * 1e9
	mk := func(minute int64, n int, d []byte) []model.RawFrame {
		out := make([]model.RawFrame, n)
		for i := range out {
			out[i] = model.RawFrame{HostNs: minute*1e9 + int64(i)*1e6, Data: d}
		}
		return out
	}
	s.countFrames(mk(base, 10, mfDF11(0x400001)), false, now)
	s.countFrames(mk(base, 5, mfDF4(0x400001)), true, now)
	s.countFrames(mk(base+60, 3, mfDF11(0x400001)), false, now)

	sm := s.addExact(7, avf.MinuteStats{RxNo: 1, Minute: base, DF: map[string]uint64{"11": 40, "4": 20}, ICAOs: 3}, false, now)
	if !sm.ReceivedKnown || !sm.Selective || sm.Records != 1 || sm.Received.Frames != 15 || sm.Shed().Frames != 45 || sm.Station != 7 {
		t.Fatalf("%+v", sm)
	}
	// second part of the same minute (restart): cumulative
	sm = s.addExact(7, avf.MinuteStats{RxNo: 1, Minute: base, DF: map[string]uint64{"11": 2}, ICAOs: 1}, false, now)
	if sm.Records != 2 || sm.Exact.DF["11"] != 42 || sm.Exact.ICAOs != 3 || sm.Merged().Frames != 62 {
		t.Fatalf("%+v", sm)
	}
	sm = s.addExact(7, avf.MinuteStats{Minute: base + 60, DF: map[string]uint64{"11": 3}}, false, now)
	if sm.Selective || sm.Shed().Frames != 0 || sm.Received.Frames != 3 {
		t.Fatalf("%+v", sm)
	}
	// far older than the window: exact only
	sm = s.addExact(7, avf.MinuteStats{Minute: base - 3600, DF: map[string]uint64{"11": 3}}, true, now)
	if sm.ReceivedKnown || sm.Records != 1 || !sm.Late {
		t.Fatalf("%+v", sm)
	}
	// a minute far in the future (bad feeder clock) does not wipe the window
	_ = s.addExact(7, avf.MinuteStats{Minute: base + 86400, DF: map[string]uint64{"11": 1}}, false, now)
	sm = s.addExact(7, avf.MinuteStats{Minute: base, DF: map[string]uint64{"11": 1}}, false, now)
	if sm.Records != 3 || !sm.ReceivedKnown {
		t.Fatalf("window lost: %+v", sm)
	}
	// slot recycling: 16 minutes later the old slot is reused
	later := base + minuteSlots*60
	s.countFrames(mk(later, 2, mfDF11(0x400001)), false, (later+30)*1e9)
	sm = s.addExact(7, avf.MinuteStats{Minute: base, DF: map[string]uint64{"11": 1}}, true, (later+30)*1e9)
	if sm.ReceivedKnown {
		t.Fatalf("recycled slot still merged: %+v", sm)
	}
	// frames older than a slot's minute are not counted into it
	s.countFrames(mk(base, 2, mfDF11(0x400001)), false, (later+30)*1e9)
	sm = s.addExact(7, avf.MinuteStats{Minute: later, DF: map[string]uint64{"11": 2}}, false, (later+30)*1e9)
	if sm.Received.Frames != 2 {
		t.Fatalf("%+v", sm)
	}

	b := newMinuteBook()
	if b.get(3) != b.get(3) || b.size() != 1 {
		t.Fatal("book get")
	}
	b.get(3).used = time.Now().Add(-time.Hour).UnixNano()
	b.get(4).used = time.Now().UnixNano()
	b.sweep(time.Now())
	if b.size() != 1 {
		t.Fatalf("sweep left %d", b.size())
	}
	if floorMinute(-1) != -60 || floorMinute(59e9) != 0 || floorMinute(60e9) != 60 {
		t.Fatal("floorMinute")
	}
}

// TestSelectiveMinuteStatsMergeEndToEnd runs a real feeder uplink
// (internal/uplink) against the real server in selective mode. The feeder
// thins its frames; the merged StationMinute must reproduce the truth
// exactly: Exact = every frame of the minute, Received = what the selective
// filter let through (replayed independently with uplink.Filter), Shed =
// the difference, Merged = the truth.
func TestSelectiveMinuteStatsMergeEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var got []StationMinute
	var raw []avf.MinuteStats
	e := newEnv(t, model.AdmissionOpen, func(o *Options, io *ingest.Options) {
		io.Limits = model.DefaultAggregator().Limits
		io.Limits.SelectiveAbove = 0 // every approved station is selective
		io.Limits.MaxFPSPerStation = 1_000_000
		io.Limits.MaxKBpsPerStation = 1_000_000
		o.Hooks.OnStationMinute = func(m StationMinute) {
			mu.Lock()
			got = append(got, m)
			mu.Unlock()
		}
		o.Hooks.OnMinuteStats = func(_ model.StationIdx, m avf.MinuteStats, _ bool) {
			mu.Lock()
			raw = append(raw, m)
			mu.Unlock()
		}
	})
	host, port, _ := net.SplitHostPort(e.addr)
	p, _ := strconv.Atoi(port)
	u := model.DefaultUplink()
	u.ID, u.Host, u.Port, u.ServerPin, u.SpoolMB = "agg", host, p, e.pin, 0
	m, err := uplink.NewManager(uplink.Options{Software: "airview-test", Version: "1", BackoffMin: 20 * time.Millisecond,
		BackoffMax: 100 * time.Millisecond, SDRoot: func() bool { return false }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Apply([]model.Uplink{u})

	now := time.Now()
	cur := now.Unix() - now.Unix()%60
	minA := cur - 120 // warm-up minute
	minB := cur - 60  // measured minute
	ticks := uint64(1000)
	frame := func(hostNs int64, d []byte) model.RawFrame {
		ticks += 1200
		kind := model.FrameModeSLong
		if len(d) == 7 {
			kind = model.FrameModeSShort
		}
		return model.RawFrame{Recv: time.Unix(0, hostNs), HostNs: hostNs, Kind: kind, Data: d, FeedID: "rx1", Channel: -1,
			Signal: -15, ClockCls: model.ClockHW12, Epoch: 1, Ticks: ticks, Clock12MHz: ticks, GPSNs: -1}
	}
	// warm-up: announce the receiver and wait until the server made it selective
	m.HandleFrame(frame(minA*1e9+1e9, mfDF11(0x400001)))
	m.Flush()
	waitUntil(t, 10*time.Second, "selective mode", func() bool {
		st := m.Status()
		return len(st) == 1 && len(st[0].Rx) == 1 && st[0].Rx[0].Mode == avf.ModeSelective
	})

	// the measured minute: 30 aircraft, 4 frames each per 100 ms
	var truth MinuteCounts
	filt := uplink.NewFilter()
	filt.Apply(&avf.Interest{Mode: avf.ModeSelective, ReduceMs: 500, Reset: true})
	var sent MinuteCounts
	var frames []model.RawFrame
	for k := 0; k < 600; k++ {
		h := minB*1e9 + int64(k)*100e6
		for a := uint32(0); a < 30; a++ {
			for j, d := range [][]byte{mfDF17(0x400100+a, 0x58C382D690C8AC), mfDF17(0x400100+a, 0x202CC371C32CE0), mfDF11(0x400100 + a), mfDF4(0x400100 + a)} {
				frames = append(frames, frame(h+int64(a)*1000+int64(j), d))
			}
		}
	}
	for i := range frames {
		truth.countFrame(frames[i].Data)
		if filt.Pass(frames[i].Data, frames[i].HostNs) {
			sent.countFrame(frames[i].Data)
		}
		m.HandleFrame(frames[i])
	}
	// the next minute's first frame closes the measured minute
	m.HandleFrame(frame(cur*1e9+1e9, mfDF11(0x400001)))
	m.Flush()

	// The feeder may close the (past) minute early from its 1 s
	// housekeeping while frames of it are still coming (the test feeds a
	// past minute faster than real time); the server then merges the parts.
	// The last StationMinute of the minute is the cumulative one (upsert).
	var sm StationMinute
	waitUntil(t, 20*time.Second, "merged minute", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, g := range got {
			if g.Minute == minB {
				sm = g
			}
		}
		return sm.Minute == minB && ExactCounts(&sm.Exact).Frames == truth.Frames
	})
	if !sm.ReceivedKnown || !sm.Selective || sm.Records < 1 {
		t.Fatalf("flags %+v", sm)
	}
	if ex := ExactCounts(&sm.Exact); ex != truth {
		t.Fatalf("exact %+v\ntruth %+v", ex, truth)
	}
	if sm.Exact.ICAOs != 30 { // a lower bound when Records > 1, still 30 here
		t.Fatalf("icaos %d", sm.Exact.ICAOs)
	}
	if sm.Received != sent {
		t.Fatalf("received %+v\nsent by the filter %+v", sm.Received, sent)
	}
	if sm.Merged() != truth {
		t.Fatalf("merged %+v", sm.Merged())
	}
	sh := sm.Shed()
	if sh.Frames != truth.Frames-sent.Frames || sh.Frames == 0 || sent.Frames*5 > truth.Frames {
		t.Fatalf("shed %d truth %d sent %d", sh.Frames, truth.Frames, sent.Frames)
	}
	mu.Lock()
	nraw := len(raw)
	mu.Unlock()
	if nraw < 2 || e.srv.Stats().StationMinutes < 2 {
		t.Fatalf("raw hook %d, stats %+v", nraw, e.srv.Stats())
	}
	t.Logf("minute: truth %d frames, sent %d (%.1f %%), shed %d", truth.Frames, sent.Frames,
		100*float64(sent.Frames)/float64(truth.Frames), sh.Frames)
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func BenchmarkCountFrames(b *testing.B) {
	var s stationMinutes
	raw := make([]model.RawFrame, 100)
	h := time.Now().UnixNano()
	for i := range raw {
		raw[i] = model.RawFrame{HostNs: h + int64(i)*1e6, Data: mfDF17(0x400000+uint32(i), 0x58C382D690C8AC)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.countFrames(raw, true, h)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(raw)), "ns/frame")
}
