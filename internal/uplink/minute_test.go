package uplink

import (
	"encoding/json"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
	"airview/internal/uplink/avftest"
)

// TestMinuteStatsOrderAndCloseFlush checks the two feeder-side rules the
// server's selective-mode merge relies on (avfsrv minutes.go): a minute's
// frames are sent before its MINUTE_STATS, and the minute in progress is
// sent when the uplink closes (the next run sends the rest; the server adds
// the parts).
func TestMinuteStatsOrderAndCloseFlush(t *testing.T) {
	srv := avftest.Start(t, avftest.Options{AckEvery: 20 * time.Millisecond, Mode: avf.ModeSelective})
	m, err := NewManager(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	m.Apply([]model.Uplink{srv.Uplink("club")})
	waitFor(t, 5*time.Second, "connected", func() bool { return m.Status()[0].State == StateConnected })
	now := time.Now()
	cur := now.Unix() - now.Unix()%60
	g := &frameGen{feedID: "rx", epoch: 1, class: model.ClockHW12}
	perMinute := map[int64]uint64{}
	// three minutes: two complete, the last one in progress at close
	for mn := cur - 120; mn <= cur; mn += 60 {
		for i := 0; i < 400; i++ {
			h := mn*1e9 + int64(i)*10e6 // the first 4 s of each minute
			m.HandleFrame(g.next(h))
			perMinute[mn]++
		}
	}
	m.Close() // flushes the open minute, waits for the ACKs
	recs := srv.Records()
	stats := map[int64]uint64{}
	statSeq := map[int64]uint64{}
	for _, r := range recs {
		if r.Type != avf.TMinuteStats {
			continue
		}
		var ms avf.MinuteStats
		if err := json.Unmarshal(r.Body, &ms); err != nil {
			t.Fatal(err)
		}
		n := ms.ModeAC
		for _, v := range ms.DF {
			n += v
		}
		stats[ms.Minute] += n
		statSeq[ms.Minute] = r.Seq
	}
	for mn, n := range perMinute {
		if stats[mn] != n {
			t.Fatalf("minute %d: stats %d frames %d (all %v)", mn, stats[mn], n, stats)
		}
	}
	// no frame of a minute arrives after that minute's MINUTE_STATS
	for _, f := range srv.Frames() {
		mn := f.HostNs / 1e9
		mn -= mn % 60
		if s, ok := statSeq[mn]; ok && f.Seq > s {
			t.Fatalf("frame of minute %d in seq %d after its MINUTE_STATS (seq %d)", mn, f.Seq, s)
		}
	}
}
