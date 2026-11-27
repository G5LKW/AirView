package uplink

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"airview/internal/sim/simnet"
	"airview/internal/uplink/avftest"
)

// simFeed turns one simnet receiver's observations into RawFrames for the
// uplink (the way feed.BeastDecoder would deliver them) and drives the
// uplink's host clock from simulated time.
type simFeed struct {
	n    *simnet.Network
	m    *Manager
	clk  *fakeClock
	sent map[string]int64 // frame key -> host ns
}

func simKey(rx int, ticks uint64, hostNs int64, data []byte) string {
	return avftest.Frame{RxNo: rx, Ticks: ticks, HostNs: hostNs, Payload: hex.EncodeToString(data)}.Key()
}

func (s *simFeed) Observe(o *simnet.Observation) {
	f := s.n.RawFrame(o)
	f.FeedID, f.Station = "sim", 0
	if o.Class.Timed() && o.Ticks != 0 {
		s.sent[simKey(1, o.Ticks, o.HostNs, o.Data)] = o.HostNs
	}
	s.m.HandleFrame(f)
}

func (s *simFeed) Advance(h float64) {
	if t := s.n.UnixNs(h); t > s.clk.ns.Load() {
		s.clk.ns.Store(t)
	}
}

func runSim(t *testing.T, n *simnet.Network, d time.Duration) {
	t.Helper()
	if err := n.Advance(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

// TestSimnetOutage30Min is the WP-U acceptance test: a simnet receiver feeds
// an uplink, the aggregator is unreachable for 30 minutes, and after the
// reconnect the outage is backfilled (late bit, inside BACKFILL) while live
// data flows; nothing is duplicated and nothing timed is lost when a spool
// is configured. Without a spool exactly the RAM ring (the last 10 minutes)
// is backfilled.
func TestSimnetOutage30Min(t *testing.T) {
	if testing.Short() {
		t.Skip("simulates 32 minutes of traffic")
	}
	for _, tc := range []struct {
		name  string
		spool bool
	}{{"spool", true}, {"ram", false}} {
		t.Run(tc.name, func(t *testing.T) { simOutage(t, tc.spool) })
	}
}

func simOutage(t *testing.T, withSpool bool) {
	t0 := time.Now()
	srv := avftest.Start(t, avftest.Options{AckEvery: 100 * time.Millisecond})
	spec := simnet.ReceiverSpec{Lat: 50, Lon: 8, GroundHAE: 120, AntennaAGL: 10,
		Clock: simnet.ClockSpec{Device: simnet.DeviceBeast}}
	n, err := simnet.New(simnet.Config{Seed: 7, Aircraft: 25, Receivers: []simnet.ReceiverSpec{spec}})
	if err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock(n.Start())
	o := testOptions(t)
	o.Logf = func(string, ...any) {}
	o.Now = clk.Now
	o.BackfillMinBps = 64 << 20 // fast backfill in the test
	u := srv.Uplink("club")
	if withSpool {
		o.StateDir = t.TempDir()
		u.SpoolMB = 64
		o.SegmentBytes = 1 << 20
	}
	m := newMgr(t, o, u)
	feed := &simFeed{n: n, m: m, clk: clk, sent: map[string]int64{}}
	n.Attach(0, feed)

	// 1 minute connected
	waitFor(t, 5*time.Second, "connect", func() bool { return m.Status()[0].State == StateConnected })
	runSim(t, n, time.Minute)
	m.Flush()
	waitFor(t, 20*time.Second, "live data acknowledged", func() bool {
		s := m.Status()[0]
		return s.Buffer.RingRecords == 0 && len(srv.Frames()) > 0
	})
	liveBefore := len(srv.Frames())

	// 30 minutes outage
	srv.Refuse(true)
	srv.DropAll()
	waitFor(t, 5*time.Second, "disconnect", func() bool { return m.Status()[0].State != StateConnected })
	outFrom := clk.Now().UnixNano()
	runSim(t, n, 30*time.Minute)
	m.Flush()
	outTo := clk.Now().UnixNano()
	st := m.Status()[0]
	t.Logf("outage buffered: ring %d records / %.0f s, spool %d bytes, spilled %d, dropped %d",
		st.Buffer.RingRecords, st.Buffer.RingSpanS, st.Buffer.SpoolBytes, st.Counters.SpilledRecords, st.Counters.DroppedRecords)
	if st.Buffer.RingSpanS > DefaultRingAge.Seconds()+1 {
		t.Fatalf("RAM ring spans %.0f s", st.Buffer.RingSpanS)
	}

	// reconnect and keep running live for 2 minutes
	srv.Refuse(false)
	_ = m.Retry("club")
	waitFor(t, 10*time.Second, "reconnect", func() bool { return m.Status()[0].State == StateConnected })
	reconnectAt := clk.Now().UnixNano()
	runSim(t, n, 2*time.Minute)
	m.Flush()

	want := 0
	for _, h := range feed.sent {
		if withSpool || h < outFrom || h >= outTo-int64(DefaultRingAge)+int64(time.Second) {
			want++
		}
	}
	waitFor(t, 60*time.Second, "backfill complete", func() bool { return len(srv.Frames()) >= want })
	time.Sleep(300 * time.Millisecond)

	got := map[string]int{}
	late := 0
	for _, f := range srv.Frames() {
		k := f.Key()
		got[k]++
		h, ok := feed.sent[k]
		if !ok {
			t.Fatalf("server got a frame that was not fed: %+v", f)
		}
		switch {
		case h < outFrom && f.Late:
			t.Fatal("pre-outage frame marked late")
		case h >= outFrom && h < reconnectAt-int64(DefaultResumeWindow)-int64(time.Second) && !f.Late:
			t.Fatalf("outage frame (%.0f s before reconnect) sent live", float64(reconnectAt-h)/1e9)
		case h > reconnectAt && f.Late:
			t.Fatal("post-outage frame marked late")
		}
		if f.Late {
			late++
		}
	}
	for k, c := range got {
		if c != 1 {
			t.Fatalf("frame %s received %d times", k, c)
		}
	}
	missing := 0
	for k, h := range feed.sent {
		if got[k] == 0 {
			if withSpool || h < outFrom || h >= outTo-int64(DefaultRingAge)+int64(time.Second) {
				missing++
			}
		}
	}
	if missing > 0 {
		t.Fatalf("%d frames lost", missing)
	}
	if !withSpool {
		// the first 20 minutes of the outage did not fit the RAM ring
		for k, h := range feed.sent {
			if h >= outFrom && h < outTo-int64(DefaultRingAge)-int64(time.Second) && got[k] > 0 {
				t.Fatal("frame older than the RAM ring delivered without a spool")
			}
		}
		if m.Status()[0].Counters.DroppedRecords == 0 {
			t.Fatal("drops not counted")
		}
	}
	if errs := srv.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if srv.Duplicates() != 0 {
		t.Fatal("duplicates", srv.Duplicates())
	}
	st = m.Status()[0]
	t.Logf("fed %d timed frames, server %d (%d before the outage, %d late), wire %d bytes, %s",
		len(feed.sent), len(srv.Frames()), liveBefore, late, st.Counters.BytesWire, time.Since(t0).Round(time.Millisecond))
	if withSpool {
		waitFor(t, 10*time.Second, "spool drained", func() bool { return m.Status()[0].Buffer.SpoolBytes == 0 })
	}
}
