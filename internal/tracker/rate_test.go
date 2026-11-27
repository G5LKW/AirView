package tracker

import (
	"context"
	"math"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

// TestRateFirstPartialMinute: a station that starts 30 s into a minute and
// receives a constant 50 frames/s must show 50/s right away - for the whole
// station, its antenna slot, its feed and the aircraft - not a value that
// ramps up from 0 (the plain EWMA read 63 % after 5 s), and again after a
// reset.
func TestRateFirstPartialMinute(t *testing.T) {
	cfg := model.DefaultConfig()
	tr := New(Options{Config: &cfg})
	start := time.Date(2026, 3, 10, 12, 0, 30, 0, time.UTC) // 30 s into the minute
	const perSec = 50
	feed := func(from time.Time, secs int) {
		for i := 0; i < secs*perSec; i++ {
			tr.HandleFrame(model.RawFrame{Recv: from.Add(time.Duration(i) * time.Second / perSec), GPSNs: -1, Signal: -20,
				Kind: model.FrameModeSShort, Data: df11(0x3C6444), FeedID: "rc", Channel: -1})
		}
	}
	check := func(what string) {
		t.Helper()
		c := tr.Counters()
		if math.Abs(c.MsgRate-perSec) > 0.05*perSec {
			t.Errorf("%s: station rate %.1f/s, want %d", what, c.MsgRate, perSec)
		}
		found := false
		for _, f := range c.Feeds {
			if f.ID == "rc" {
				found = true
				if math.Abs(f.MsgRate-perSec) > 0.05*perSec {
					t.Errorf("%s: feed rate %.1f/s", what, f.MsgRate)
				}
			}
		}
		if !found {
			t.Errorf("%s: feed missing: %+v", what, c.Feeds)
		}
		if len(c.Antennas) == 0 || math.Abs(c.Antennas[0].MsgRate-perSec) > 0.05*perSec {
			t.Errorf("%s: antenna rate %+v", what, c.Antennas)
		}
		if ac, ok := tr.Aircraft(0x3C6444); !ok || math.Abs(ac.MsgRate-perSec) > 0.05*perSec {
			t.Errorf("%s: aircraft rate %v", what, ac.MsgRate)
		}
	}
	feed(start, 3)
	check("3 s after start")
	feed(start.Add(3*time.Second), 20)
	check("23 s after start")
	tr.Reset()
	feed(start.Add(time.Hour), 2)
	check("2 s after a reset")
}

// TestMinuteRowCoveredSecs: the stored minute rows of a station that starts
// 30 s into a minute and stops 10 s into the next carry the covered seconds
// (30 and 10), so frames / secs is the true rate, not half or a sixth of it.
func TestMinuteRowCoveredSecs(t *testing.T) {
	st, err := store.OpenWithOptions(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := model.DefaultConfig()
	tr := New(Options{Config: &cfg, Store: st})
	start := time.Date(2026, 3, 10, 12, 0, 30, 0, time.UTC)
	const perSec = 50
	for i := 0; i < 40*perSec; i++ {
		tr.HandleFrame(model.RawFrame{Recv: start.Add(time.Duration(i) * time.Second / perSec), GPSNs: -1, Signal: -20,
			Kind: model.FrameModeSShort, Data: df11(0x3C6444), FeedID: "rc", Channel: -1})
	}
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	m0 := start.Unix() - 30
	rows, err := st.MinuteStats(context.Background(), "", m0, m0+3600)
	if err != nil || len(rows) != 2 {
		t.Fatalf("minute rows: %v %+v", err, rows)
	}
	for i, want := range []float64{30, 10} {
		r := rows[i]
		if math.Abs(r.Secs-want) > 0.1 {
			t.Errorf("minute %d: secs %.2f, want %v", i, r.Secs, want)
		}
		if rate := float64(r.Frames) / r.CoveredSec(false); math.Abs(rate-perSec) > 0.05*perSec {
			t.Errorf("minute %d: %d frames / %.1f s = %.1f/s, want %d", i, r.Frames, r.Secs, rate, perSec)
		}
	}
}
