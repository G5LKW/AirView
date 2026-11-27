package tracker

import (
	"math"
	"testing"
	"time"

	lchain "airview/internal/latency"
	"airview/internal/model"
)

// TestFeedLatencyMatchesChainReceiver: the tracker's per-feed latency and
// the latency analysis' receiver stage measure the same frames from the same
// reference instant (start of frame), so they agree to the microsecond.
func TestFeedLatencyMatchesChainReceiver(t *testing.T) {
	s, cfg := newSim(t, 40, nil)
	rec := &recorder{}
	advance(t, s, 20*time.Second, rec)
	tr := New(Options{Config: &cfg})
	lat := newLatency(&cfg)
	tr.SetLatency(lat)
	const delay = 3 * time.Millisecond
	short, long := 0, 0
	for _, f := range rec.frames {
		if f.GPSNs < 0 || f.Kind == model.FrameStatus {
			tr.HandleFrame(f)
			continue
		}
		// host read exactly 3 ms after the GPS stamp (end of frame)
		day := f.Recv.Truncate(24 * time.Hour)
		f.Recv = day.Add(time.Duration(f.GPSNs) + delay)
		if f.Kind == model.FrameModeSShort {
			short++
		} else {
			long++
		}
		tr.HandleFrame(f)
	}
	if short == 0 || long == 0 {
		t.Fatalf("need both frame lengths: %d short, %d long", short, long)
	}
	fl := tr.FeedLatency()
	if len(fl) != 1 {
		t.Fatalf("feed latency %+v", fl)
	}
	var recv *lchain.StageStats
	sum := lat.Summary()
	for i := range sum.Stages {
		if sum.Stages[i].Key == "receiver" {
			recv = &sum.Stages[i]
		}
	}
	if recv == nil || recv.Count == 0 {
		t.Fatal("no receiver stage")
	}
	if fl[0].Ref != lchain.RefStartOfFrame || recv.Ref != lchain.RefStartOfFrame {
		t.Errorf("references: tracker %q, chain %q", fl[0].Ref, recv.Ref)
	}
	// Every frame took 3 ms + its own duration (64 / 120 us) from its start.
	wantMin, wantMax := 3.064, 3.120
	t.Logf("tracker min %.4f max %.4f p50 %.4f ms; chain receiver min %.4f max %.4f p50 %.4f ms",
		fl[0].MinMs, fl[0].MaxMs, fl[0].P50Ms, float64(recv.MinMs), float64(recv.MaxMs), float64(recv.P50Ms))
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"tracker min", fl[0].MinMs, wantMin}, {"tracker max", fl[0].MaxMs, wantMax},
		{"chain min", float64(recv.MinMs), wantMin}, {"chain max", float64(recv.MaxMs), wantMax},
	} {
		if math.Abs(c.got-c.want) > 0.001 {
			t.Errorf("%s %.4f ms, want %.4f", c.name, c.got, c.want)
		}
	}
	if d := math.Abs(fl[0].P50Ms - float64(recv.P50Ms)); d > 0.120 {
		t.Errorf("medians differ by %.3f ms (more than a frame duration)", d)
	}
}
