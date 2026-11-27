package uplink

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/feed"
	"airview/internal/model"
	"airview/internal/uplink/avftest"
)

// fakeClock is a settable host clock.
type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) Now() time.Time       { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) Set(t time.Time)      { c.ns.Store(t.UnixNano()) }
func (c *fakeClock) Add(d time.Duration)  { c.ns.Add(int64(d)) }
func newFakeClock(t time.Time) *fakeClock { c := &fakeClock{}; c.Set(t); return c }

// sinkRec collects injected updates.
type sinkRec struct {
	mu  sync.Mutex
	ups []model.DecodedUpdate
}

func (s *sinkRec) HandleFrame(model.RawFrame) {}
func (s *sinkRec) HandleUpdate(u model.DecodedUpdate) {
	s.mu.Lock()
	s.ups = append(s.ups, u)
	s.mu.Unlock()
}
func (s *sinkRec) list() []model.DecodedUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.DecodedUpdate(nil), s.ups...)
}

func waitFor(t testing.TB, d time.Duration, what string, cond func() bool) {
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

func testOptions(t testing.TB) Options {
	return Options{Software: "airview-test", Version: "1", BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
		Logf: t.Logf, SDRoot: func() bool { return false }}
}

func newMgr(t testing.TB, o Options, ups ...model.Uplink) *Manager {
	t.Helper()
	m, err := NewManager(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	m.Apply(ups)
	return m
}

// frameGen makes timed long frames of one feed.
type frameGen struct {
	feedID string
	epoch  uint32
	class  model.ClockClass
	ticks  uint64
	n      int
}

func (g *frameGen) next(hostNs int64) model.RawFrame {
	g.n++
	g.ticks += 12_000 + uint64(g.n%7)
	me := uint64(0x58C382D690C8AC) ^ uint64(g.n)<<8
	f := model.RawFrame{Recv: time.Unix(0, hostNs), HostNs: hostNs, Kind: model.FrameModeSLong,
		Data: df17(0x400000+uint32(g.n%64), me), FeedID: g.feedID, Channel: -1, Signal: -12.5,
		ClockCls: g.class, Epoch: g.epoch, Ticks: g.ticks, GPSNs: -1}
	if g.class.GPS() {
		f.GPSNs = int64(g.ticks % uint64(model.NsPerDay))
	} else {
		f.Clock12MHz = g.ticks & (1<<48 - 1)
	}
	return f
}

func payloadKey(f *model.RawFrame) string { return hex.EncodeToString(f.Data) }

func TestConnectSendFrames(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 50 * time.Millisecond, Name: "Club"})
	m := newMgr(t, testOptions(t), srv.Uplink("club"))
	g := &frameGen{feedID: "beast", epoch: 3, class: model.ClockHW12}
	h := time.Now().UnixNano()
	want := map[string]model.RawFrame{}
	for i := 0; i < 3000; i++ {
		f := g.next(h + int64(i)*1e6)
		want[payloadKey(&f)] = f
		m.HandleFrame(f)
	}
	m.Flush()
	waitFor(t, 10*time.Second, "all frames", func() bool { return len(srv.Frames()) >= len(want) })
	for _, f := range srv.Frames() {
		w, ok := want[f.Payload]
		if !ok {
			t.Fatal("unexpected frame", f.Payload)
		}
		if f.Ticks != w.Ticks || f.HostNs/1000 != w.HostNs/1000 || f.Late || f.TsInvalid || f.Epoch != 3 || f.RxNo != 1 ||
			f.Sig != sigByte(-12.5) || f.Channel != -1 {
			t.Fatalf("frame mismatch: %+v vs ticks %d host %d", f, w.Ticks, w.HostNs)
		}
	}
	recs := srv.Records()
	if recs[0].Type != avf.TReceiver {
		t.Fatal("first record", recs[0].Type)
	}
	var rcv avf.Receiver
	_ = json.Unmarshal(recs[0].Body, &rcv)
	if rcv.ClockClass != "none" || !rcv.Timestamps || rcv.Privacy.Pin != model.PinHidden {
		// the first RECEIVER is sent before the first frame announced hw12
		t.Logf("first receiver: %+v", rcv)
	}
	found := false
	for _, r := range recs {
		if r.Type == avf.TReceiver {
			_ = json.Unmarshal(r.Body, &rcv)
			if rcv.ClockClass == "hw12" && rcv.EpochID == 3 && rcv.NominalHz == 12e6 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no RECEIVER announcing hw12 epoch 3")
	}
	if errs := srv.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
	st := m.Status()[0]
	if st.State != StateConnected || st.ServerName != "Club" || st.Approval != avf.StatusApproved || st.FeederID != m.Identity().FeederID {
		t.Fatalf("%+v", st)
	}
	waitFor(t, 5*time.Second, "ACK of everything", func() bool { return m.Status()[0].Buffer.RingRecords == 0 })
	hello := srv.Hellos()[0]
	if hello.Software != "airview-test" || hello.SeqEpoch == "" || hello.EnrolToken != "" || len(hello.Pub) != 32 {
		t.Fatalf("%+v", hello)
	}
}

// MLAT results, frames of non-forwardable feeds and injected results never
// leave the station; untimed frames go out with tsInvalid only.
func TestNeverForwardMLAT(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 50 * time.Millisecond})
	o := testOptions(t)
	o.Source = SourceFuncs{ForwardableFn: func(id string) bool { return id != "mlat-in" }}
	u := srv.Uplink("club")
	m := newMgr(t, o, u)
	h := time.Now().UnixNano()
	g := &frameGen{feedID: "beast", epoch: 1, class: model.ClockHW12}
	forbidden := map[string]bool{}
	allowed := map[string]bool{}
	for i := 0; i < 200; i++ {
		f := g.next(h + int64(i)*1e6)
		switch i % 5 {
		case 0: // MLAT flag
			f.MLAT = true
			forbidden[payloadKey(&f)] = true
		case 1: // magic timestamp without the flag
			f.Clock12MHz = 0xFF004D4C4154 + 3
			forbidden[payloadKey(&f)] = true
		case 2: // NoForward / MLATOnly feed
			f.FeedID = "mlat-in"
			forbidden[payloadKey(&f)] = true
		case 3: // zero timestamp: only as untimed
			if i%2 == 0 {
				f.Ticks = 0
			} else {
				f.Clock12MHz = 0 // all-zero 48-bit Beast timestamp
			}
			allowed["u"+payloadKey(&f)] = true
		default:
			allowed[payloadKey(&f)] = true
		}
		m.HandleFrame(f)
	}
	m.HandleUpdate(model.DecodedUpdate{FeedID: "beast", ICAO: 0x4840D6, MLAT: true, Lat: model.F(1), Lon: model.F(2)})
	m.HandleUpdate(model.DecodedUpdate{FeedID: FeedPrefix + "club", ICAO: 0x4840D6, Lat: model.F(1), Lon: model.F(2)})
	m.Flush()
	waitFor(t, 5*time.Second, "frames", func() bool { return len(srv.Frames()) >= len(allowed) })
	time.Sleep(200 * time.Millisecond)
	for _, f := range srv.Frames() {
		if forbidden[f.Payload] {
			t.Fatal("forbidden frame was sent", f.Payload)
		}
		key := f.Payload
		if f.TsInvalid {
			key = "u" + key
		}
		if !allowed[key] {
			t.Fatalf("frame sent with wrong timing flag: %+v", f)
		}
	}
	for _, r := range srv.Records() {
		if r.Type == avf.TDecoded {
			t.Fatal("MLAT / injected update forwarded as DECODED")
		}
	}
	st := m.Status()[0]
	if st.Counters.MLATRefused != 81 { // 40 flagged + 40 magic + 1 MLAT update
		t.Fatalf("mlatRefused %d", st.Counters.MLATRefused)
	}
	if errs := srv.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
}

func TestTimestampsOff(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 50 * time.Millisecond})
	u := srv.Uplink("club")
	u.Timestamps = false
	m := newMgr(t, testOptions(t), u)
	g := &frameGen{feedID: "beast", epoch: 1, class: model.ClockGPSUTC}
	h := time.Now().UnixNano()
	for i := 0; i < 100; i++ {
		m.HandleFrame(g.next(h + int64(i)*1e6))
	}
	m.HandleClockEvent("beast", feed.ClockEvent{OldEpoch: 1, NewEpoch: 2, Reason: model.ClockEvJump, NewClass: model.ClockGPSUTC})
	m.Flush()
	waitFor(t, 5*time.Second, "frames", func() bool { return len(srv.Frames()) >= 100 })
	for _, f := range srv.Frames() {
		if !f.TsInvalid || f.Ticks != 0 || f.Epoch != 0 {
			t.Fatalf("timed frame with Timestamps off: %+v", f)
		}
	}
	for _, r := range srv.Records() {
		switch r.Type {
		case avf.TClockEvent, avf.TClockPair, avf.TClockStatus:
			t.Fatal("clock record with Timestamps off:", r.Type.String())
		case avf.TReceiver:
			var rc avf.Receiver
			_ = json.Unmarshal(r.Body, &rc)
			if rc.Timestamps || rc.ClockClass != "none" {
				t.Fatalf("%+v", rc)
			}
		}
	}
}

func TestClockEventOrdering(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 50 * time.Millisecond})
	m := newMgr(t, testOptions(t), srv.Uplink("club"))
	g := &frameGen{feedID: "beast", epoch: 5, class: model.ClockGPSUTC}
	h := time.Now().UnixNano()
	for i := 0; i < 50; i++ {
		m.HandleFrame(g.next(h + int64(i)*1e6))
	}
	m.HandleClockEvent("beast", feed.ClockEvent{OldEpoch: 5, NewEpoch: 6, Reason: model.ClockEvBackwards, NewClass: model.ClockGPSUTC})
	g.epoch, g.ticks = 6, 1000
	for i := 50; i < 100; i++ {
		m.HandleFrame(g.next(h + int64(i)*1e6))
	}
	// an epoch change without an event is synthesised
	g.epoch, g.ticks = 7, 500
	for i := 100; i < 150; i++ {
		m.HandleFrame(g.next(h + int64(i)*1e6))
	}
	m.Flush()
	waitFor(t, 5*time.Second, "frames", func() bool { return len(srv.Frames()) >= 150 })
	seen := map[uint32]bool{5: true}
	var events []avf.ClockEvent
	for _, r := range srv.Records() {
		switch r.Type {
		case avf.TClockEvent:
			var ev avf.ClockEvent
			_ = json.Unmarshal(r.Body, &ev)
			events = append(events, ev)
			seen[ev.NewEpoch] = true
		case avf.TFrames:
			for _, f := range srv.Frames() {
				if f.Seq == r.Seq && !seen[f.Epoch] {
					t.Fatalf("frames of epoch %d before its CLOCK_EVENT", f.Epoch)
				}
			}
		}
	}
	if len(events) != 2 || events[0].Reason != "backwards" || events[0].OldEpoch != 5 || events[1].NewEpoch != 7 {
		t.Fatalf("%+v", events)
	}
	if errs := srv.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
}

// A dropped connection resumes without loss or duplicates: unacknowledged
// young data is resent live, renumbered above the server's watermark.
func TestResumeAfterDrop(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 300 * time.Millisecond})
	m := newMgr(t, testOptions(t), srv.Uplink("club"))
	g := &frameGen{feedID: "beast", epoch: 1, class: model.ClockHW12}
	waitFor(t, 5*time.Second, "connect", func() bool { return srv.Sessions() == 1 })
	total := 0
	for round := 0; round < 5; round++ {
		h := time.Now().UnixNano()
		for i := 0; i < 500; i++ {
			m.HandleFrame(g.next(h + int64(i)*10e3))
			total++
		}
		m.Flush()
		time.Sleep(30 * time.Millisecond)
		before := m.Status()[0].Sessions
		srv.DropAll()
		// Wait for a new session, not for the stale state of the dropped one.
		waitFor(t, 5*time.Second, "reconnect", func() bool {
			st := m.Status()[0]
			return srv.Sessions() == 1 && st.State == StateConnected && st.Sessions > before
		})
	}
	waitFor(t, 10*time.Second, "all frames", func() bool { return len(srv.Frames()) >= total })
	time.Sleep(300 * time.Millisecond)
	keys := map[string]int{}
	for _, f := range srv.Frames() {
		keys[f.Key()]++
		if f.Late {
			t.Fatal("young data was backfilled")
		}
	}
	if len(keys) != total {
		t.Fatalf("%d distinct frames, want %d", len(keys), total)
	}
	for k, n := range keys {
		if n != 1 {
			t.Fatalf("frame %s received %d times", k, n)
		}
	}
	if errs := srv.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if m.Status()[0].Sessions < 6 {
		t.Fatal("sessions", m.Status()[0].Sessions)
	}
}

// Data older than the resume window is backfilled with the late bit inside
// BACKFILL markers; live data still flows.
func TestOutageBackfillRAM(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 50 * time.Millisecond})
	clk := newFakeClock(time.Now())
	o := testOptions(t)
	o.Now = clk.Now
	m := newMgr(t, o, srv.Uplink("club"))
	waitFor(t, 5*time.Second, "connect", func() bool { return srv.Sessions() == 1 })
	srv.Refuse(true)
	srv.DropAll()
	waitFor(t, 5*time.Second, "disconnect", func() bool { return m.Status()[0].State != StateConnected })
	g := &frameGen{feedID: "beast", epoch: 1, class: model.ClockHW12}
	start := clk.Now()
	// 3 minutes of data at 50 fps while disconnected
	for i := 0; i < 9000; i++ {
		clk.Set(start.Add(time.Duration(i) * 20 * time.Millisecond))
		m.HandleFrame(g.next(clk.Now().UnixNano()))
	}
	m.Flush()
	srv.Refuse(false)
	_ = m.Retry("club")
	waitFor(t, 10*time.Second, "backfill", func() bool { return len(srv.Frames()) >= 9000 })
	cut := clk.Now().Add(-DefaultResumeWindow).UnixNano()
	late := 0
	for _, f := range srv.Frames() {
		if f.HostNs < cut-int64(time.Second) && !f.Late {
			t.Fatalf("old frame sent live: %d s old", (clk.Now().UnixNano()-f.HostNs)/1e9)
		}
		if f.HostNs > cut+int64(time.Second) && f.Late {
			t.Fatal("young frame backfilled")
		}
		if f.Late {
			late++
		}
	}
	if late < 7000 {
		t.Fatal("late frames", late)
	}
	var begin, end int
	for _, r := range srv.Records() {
		if r.Type == avf.TBackfill {
			var b avf.Backfill
			_ = json.Unmarshal(r.Body, &b)
			if b.Phase == "begin" {
				begin++
			} else {
				end++
			}
		}
	}
	if begin == 0 || begin != end {
		t.Fatal("BACKFILL markers", begin, end)
	}
	if errs := srv.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if st := m.Status()[0]; st.Counters.FramesLate != uint64(late) {
		t.Fatalf("framesLate %d, server saw %d", st.Counters.FramesLate, late)
	}
}

// Data spilled to the disk spool survives a restart of the uplink and is
// backfilled by the next process.
func TestSpoolSurvivesRestart(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 50 * time.Millisecond})
	srv.Refuse(true)
	dir := t.TempDir()
	clk := newFakeClock(time.Now())
	o := testOptions(t)
	o.StateDir, o.Now = dir, clk.Now
	u := srv.Uplink("club")
	u.SpoolMB = 16
	m1, err := NewManager(o)
	if err != nil {
		t.Fatal(err)
	}
	m1.Apply([]model.Uplink{u})
	g := &frameGen{feedID: "beast", epoch: 1, class: model.ClockHW12}
	start := clk.Now()
	for i := 0; i < 2000; i++ {
		clk.Set(start.Add(time.Duration(i) * 10 * time.Millisecond))
		m1.HandleFrame(g.next(clk.Now().UnixNano()))
	}
	m1.Close() // spills the RAM ring to the spool
	if st, err := openSpool(dirSpool(dir, "club"), 16<<20, DefaultSegmentBytes); err != nil || st.Bytes() == 0 {
		t.Fatal("nothing spooled", err)
	} else {
		_ = st.Close(time.Now())
	}
	srv.Refuse(false)
	m2 := newMgr(t, o, u)
	waitFor(t, 10*time.Second, "backfill after restart", func() bool { return len(srv.Frames()) >= 2000 })
	for _, f := range srv.Frames() {
		if !f.Late {
			t.Fatal("spooled frame not late")
		}
	}
	if model.FeederIDFromPub(srv.Hellos()[0].Pub) != m2.Identity().FeederID {
		t.Fatal("identity changed across restart")
	}
	waitFor(t, 5*time.Second, "spool drained", func() bool { return m2.Status()[0].Buffer.SpoolBytes == 0 })
	if errs := srv.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
}

func dirSpool(state, id string) string { return state + "/uplinks/" + id + "/spool" }

func TestMLATResultsInjected(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{})
	sink := &sinkRec{}
	var cb atomic.Int64
	o := testOptions(t)
	o.Inject = sink
	o.OnResults = func(id string, fx []model.MLATFix) { cb.Add(int64(len(fx))) }
	m := newMgr(t, o, srv.Uplink("club"))
	waitFor(t, 5*time.Second, "connect", func() bool { return m.Status()[0].State == StateConnected })
	fixes := []model.MLATFix{
		{ICAO: 0x4840D6, TUnixNs: time.Now().UnixNano(), ECEF: [3]float64{3980000, -10000, 4970000}, Cov: [6]float32{400, 0, 0, 400, 0, 900}, Grade: model.GradeA},
		{ICAO: 0x4840D7, TUnixNs: time.Now().UnixNano(), ECEF: [3]float64{3980000, -10000, 4970000}, Grade: model.GradeD},
	}
	srv.SendResults(fixes)
	waitFor(t, 5*time.Second, "injection", func() bool { return len(sink.list()) == 1 && cb.Load() == 2 })
	u := sink.list()[0]
	if u.ICAO != 0x4840D6 || !u.MLAT || u.FeedID != FeedPrefix+"club" || u.Grade != model.GradeA {
		t.Fatalf("%+v", u)
	}
	// the injected update coming back through the tee is not forwarded
	m.HandleUpdate(u)
	m.Flush()
	time.Sleep(200 * time.Millisecond)
	for _, r := range srv.Records() {
		if r.Type == avf.TDecoded {
			t.Fatal("injected result forwarded")
		}
	}
	// ReturnMLAT off: no "results" feature and nothing injected
	srv2 := avftest.Start(t, avftest.Options{})
	sink2 := &sinkRec{}
	o2 := testOptions(t)
	o2.Inject = sink2
	u2 := srv2.Uplink("other")
	u2.ReturnMLAT = false
	m2 := newMgr(t, o2, u2)
	waitFor(t, 5*time.Second, "connect", func() bool { return m2.Status()[0].State == StateConnected })
	if slices.Contains(srv2.Hellos()[0].Features, avf.FeatResults) {
		t.Fatal("results feature offered with ReturnMLAT off")
	}
	srv2.SendResults(fixes)
	time.Sleep(200 * time.Millisecond)
	if len(sink2.list()) != 0 {
		t.Fatal("results injected with ReturnMLAT off")
	}
}

func TestInterestAndFlow(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 50 * time.Millisecond, Mode: avf.ModeSelective})
	m := newMgr(t, testOptions(t), srv.Uplink("club"))
	waitFor(t, 5*time.Second, "connect", func() bool { return m.Status()[0].State == StateConnected })
	if m.Status()[0].Mode != avf.ModeSelective {
		t.Fatal("mode", m.Status()[0].Mode)
	}
	srv.SendJSON(avf.TInterest, avf.Interest{Mode: avf.ModeSelective, ReduceMs: 1000, SyncAdd: []string{"400001"}})
	time.Sleep(100 * time.Millisecond)
	g := &frameGen{feedID: "beast", epoch: 1, class: model.ClockHW12}
	h := time.Now().UnixNano()
	n := 0
	for i := 0; i < 640; i++ { // 64 aircraft x 10, over 6.4 s of host time
		f := g.next(h + int64(i)*10e6)
		if f.Data[3] == 0x01 {
			n++
		}
		m.HandleFrame(f)
	}
	m.Flush()
	time.Sleep(300 * time.Millisecond)
	sync, other := 0, 0
	for _, f := range srv.Frames() {
		if strings.HasPrefix(f.Payload, "8d400001") {
			sync++
		} else {
			other++
		}
	}
	if sync != n || other == 0 || other > 63*7 {
		t.Fatalf("sync %d of %d, other %d", sync, n, other)
	}
	if m.Status()[0].Counters.Filtered == 0 {
		t.Fatal("nothing filtered")
	}
	// FLOW pause holds data; resume releases it
	srv.SendJSON(avf.TFlow, avf.Flow{Pause: true})
	waitFor(t, 2*time.Second, "pause", func() bool { return m.Status()[0].Paused })
	before := len(srv.Frames())
	srv.SendJSON(avf.TInterest, avf.Interest{Mode: avf.ModeFull})
	time.Sleep(50 * time.Millisecond)
	h = time.Now().UnixNano()
	for i := 0; i < 100; i++ {
		m.HandleFrame(g.next(h + int64(i)*1e6))
	}
	m.Flush()
	time.Sleep(300 * time.Millisecond)
	if len(srv.Frames()) != before {
		t.Fatal("frames sent while paused")
	}
	srv.SendJSON(avf.TFlow, avf.Flow{Pause: false})
	waitFor(t, 5*time.Second, "release", func() bool { return len(srv.Frames()) == before+100 })
	if errs := srv.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
}

func TestClaimAndStatusHandler(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{})
	m := newMgr(t, testOptions(t), srv.Uplink("club"))
	waitFor(t, 5*time.Second, "connect", func() bool { return m.Status()[0].State == StateConnected })
	code, exp, err := m.Claim(context.Background(), "", 0)
	if err != nil || code != "ABCDOIEB" || time.Until(exp) < 23*time.Hour {
		t.Fatal(code, exp, err)
	}
	ts := httptest.NewServer(m)
	defer ts.Close()
	res, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	var sr StatusResponse
	_ = json.NewDecoder(res.Body).Decode(&sr)
	res.Body.Close()
	if sr.FeederID != m.Identity().FeederID || len(sr.Uplinks) != 1 || sr.Uplinks[0].State != StateConnected {
		t.Fatalf("%+v", sr)
	}
	b, _ := json.Marshal(ActionRequest{Action: "claim", ID: "club"})
	res, err = http.Post(ts.URL, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	var cr ClaimResponse
	_ = json.NewDecoder(res.Body).Decode(&cr)
	res.Body.Close()
	if cr.Code != "ABCDOIEB" {
		t.Fatalf("%+v", cr)
	}
	res, _ = http.Post(ts.URL, "application/json", strings.NewReader(`{"action":"claim","id":"nope"}`))
	if res.StatusCode != http.StatusNotFound {
		t.Fatal(res.StatusCode)
	}
	res.Body.Close()
	// a status never contains the enrol token
	raw, _ := json.Marshal(m.Status())
	if strings.Contains(string(raw), "enrol") {
		t.Fatal("status mentions the enrol token")
	}
}

func TestEnrolment(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{EnrolToken: "tok-123", Admission: model.AdmissionInvite})
	dir := t.TempDir()
	o := testOptions(t)
	o.StateDir = dir
	// without a token: stopped with enrol_required
	u := srv.Uplink("club")
	m := newMgr(t, o, u)
	waitFor(t, 5*time.Second, "stopped", func() bool { return m.Status()[0].State == StateStopped })
	if m.Status()[0].ErrorCode != "enrol_required" {
		t.Fatal(m.Status()[0].ErrorCode)
	}
	// with the token: approved; the token is sent once
	u.EnrolToken = "tok-123"
	m.Apply([]model.Uplink{u})
	waitFor(t, 5*time.Second, "connect", func() bool { return m.Status()[0].State == StateConnected })
	srv.DropAll()
	waitFor(t, 5*time.Second, "reconnect", func() bool { return srv.TotalSessions() == 2 && m.Status()[0].State == StateConnected })
	hs := srv.Hellos()
	if hs[len(hs)-2].EnrolToken != "tok-123" || hs[len(hs)-1].EnrolToken != "" {
		t.Fatal("token not sent exactly once")
	}
	// a wrong token is a stop as well
	srv2 := avftest.Start(t, avftest.Options{EnrolToken: "right"})
	u2 := srv2.Uplink("other")
	u2.EnrolToken = "wrong"
	m2 := newMgr(t, testOptions(t), u2)
	waitFor(t, 5*time.Second, "stopped", func() bool { return m2.Status()[0].State == StateStopped })
	if m2.Status()[0].ErrorCode != "enrol_invalid" {
		t.Fatal(m2.Status()[0].ErrorCode)
	}
}

func TestPinMismatchAndDeny(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{})
	u := srv.Uplink("club")
	_, other, _ := avftest.SelfSigned()
	u.ServerPin = other
	m := newMgr(t, testOptions(t), u)
	waitFor(t, 5*time.Second, "pin error", func() bool {
		s := m.Status()[0]
		return s.State == StateBackoff && strings.Contains(s.LastError, "pin")
	})
	if len(srv.Hellos()) != 0 {
		t.Fatal("handshake continued after a pin mismatch")
	}
	srv2 := avftest.Start(t, avftest.Options{Decide: func(avf.HelloBody) avftest.Decision {
		return avftest.Decision{Status: avf.StatusDenied, Code: "server_full", Reason: "full", ReconnectIn: 120}
	}})
	m2 := newMgr(t, testOptions(t), srv2.Uplink("x"))
	waitFor(t, 5*time.Second, "denied", func() bool { return m2.Status()[0].ErrorCode == "server_full" })
	s := m2.Status()[0]
	if s.State != StateBackoff || time.Until(time.UnixMilli(s.RetryAtMs)) < 100*time.Second {
		t.Fatalf("reconnectIn not honoured: %+v", s)
	}
}

func TestGoodbyeReconnectIn(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{})
	m := newMgr(t, testOptions(t), srv.Uplink("club"))
	waitFor(t, 5*time.Second, "connect", func() bool { return m.Status()[0].State == StateConnected })
	srv.Goodbye("shutting_down", "restart", 30)
	waitFor(t, 5*time.Second, "backoff", func() bool { return m.Status()[0].State == StateBackoff })
	if s := m.Status()[0]; time.Until(time.UnixMilli(s.RetryAtMs)) < 20*time.Second || s.ErrorCode != "shutting_down" {
		t.Fatalf("%+v", s)
	}
	m.Close()
	// graceful close sends GOODBYE
	srv2 := avftest.Start(t, avftest.Options{})
	m2, _ := NewManager(testOptions(t))
	m2.Apply([]model.Uplink{srv2.Uplink("x")})
	waitFor(t, 5*time.Second, "connect", func() bool { return m2.Status()[0].State == StateConnected })
	m2.Close()
	waitFor(t, 2*time.Second, "goodbye", func() bool { return len(srv2.Goodbyes()) == 1 })
}

func TestPingPongAndLiveness(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{PingEvery: 50 * time.Millisecond, AckEvery: time.Hour})
	o := testOptions(t)
	m := newMgr(t, o, srv.Uplink("club"))
	waitFor(t, 5*time.Second, "pongs", func() bool { return srv.Pongs() >= 3 })
	// a server that goes silent is left after Liveness
	srv2 := avftest.Start(t, avftest.Options{PingEvery: time.Hour, AckEvery: time.Hour})
	o2 := testOptions(t)
	o2.Liveness = 300 * time.Millisecond
	m2 := newMgr(t, o2, srv2.Uplink("x"))
	// The server counts a session when it accepts it, the client after its
	// handshake: wait for both (the old check raced under load).
	waitFor(t, 5*time.Second, "liveness reconnect", func() bool {
		return srv2.TotalSessions() >= 2 && m2.Status()[0].Sessions >= 2
	})
	_ = m
}

func TestApplyChanges(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{})
	u := srv.Uplink("club")
	m := newMgr(t, testOptions(t), u)
	waitFor(t, 5*time.Second, "connect", func() bool { return m.Status()[0].State == StateConnected })
	// a data-only change keeps the session and re-announces the receiver
	g := &frameGen{feedID: "beast", epoch: 1, class: model.ClockHW12}
	m.HandleFrame(g.next(time.Now().UnixNano()))
	u.Pin = model.PinRegion
	m.Apply([]model.Uplink{u})
	waitFor(t, 5*time.Second, "receiver update", func() bool {
		for _, r := range srv.Records() {
			var rc avf.Receiver
			if r.Type == avf.TReceiver && json.Unmarshal(r.Body, &rc) == nil && rc.Privacy.Pin == model.PinRegion {
				return true
			}
		}
		return false
	})
	if srv.TotalSessions() != 1 {
		t.Fatal("data change reconnected")
	}
	// disabling stops the connection; removing stops the client
	u.Disabled = true
	m.Apply([]model.Uplink{u})
	waitFor(t, 5*time.Second, "disabled", func() bool { return m.Status()[0].State == StateDisabled && srv.Sessions() == 0 })
	m.Apply(nil)
	if m.Active() || len(m.Status()) != 0 {
		t.Fatal("uplink not removed")
	}
}

// An epoch change shortly before a reconnect: the queued young frames of the
// old epoch go out live after a RECEIVER announcing that epoch, followed by
// the queued CLOCK_EVENT and the new epoch's frames.
func TestEpochChangeDuringOutage(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 50 * time.Millisecond})
	m := newMgr(t, testOptions(t), srv.Uplink("club"))
	g := &frameGen{feedID: "beast", epoch: 5, class: model.ClockHW12}
	h := time.Now().UnixNano()
	for i := 0; i < 20; i++ {
		m.HandleFrame(g.next(h + int64(i)*1e6))
	}
	m.Flush()
	waitFor(t, 5*time.Second, "first frames", func() bool { return srv.FrameCount() == 20 })
	srv.Refuse(true)
	srv.DropAll()
	waitFor(t, 5*time.Second, "disconnect", func() bool { return m.Status()[0].State != StateConnected })
	h = time.Now().UnixNano()
	for i := 0; i < 20; i++ {
		m.HandleFrame(g.next(h + int64(i)*1e6))
	}
	m.HandleClockEvent("beast", feed.ClockEvent{OldEpoch: 5, NewEpoch: 6, Reason: model.ClockEvRestart, NewClass: model.ClockSDR12})
	g.epoch, g.class, g.ticks = 6, model.ClockSDR12, 1000
	for i := 20; i < 40; i++ {
		m.HandleFrame(g.next(h + int64(i)*1e6))
	}
	m.Flush()
	srv.Refuse(false)
	_ = m.Retry("club")
	waitFor(t, 5*time.Second, "all frames", func() bool { return srv.FrameCount() == 60 })
	if errs := srv.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
	var rcv []string
	for _, r := range srv.Records() {
		if r.Session == 2 && r.Type == avf.TReceiver {
			var rc avf.Receiver
			_ = json.Unmarshal(r.Body, &rc)
			rcv = append(rcv, fmt.Sprintf("%s/%d", rc.ClockClass, rc.EpochID))
		}
	}
	if len(rcv) == 0 || rcv[0] != "hw12/5" || rcv[len(rcv)-1] != "sdr12/6" {
		t.Fatalf("receivers announced in session 2: %v", rcv)
	}
	for _, f := range srv.Frames() {
		if f.Late {
			t.Fatal("young frame backfilled")
		}
	}
}
