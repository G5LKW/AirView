package tracker

import (
	"math"
	"testing"
	"time"

	"airview/internal/model"
)

// A surface position without altitude due north of the receiver (bearing
// bin 0) used to index the coverage grid at -1 and panic with mu held.
func TestSurfacePositionNorthNoAltitude(t *testing.T) {
	cfg := model.DefaultConfig()
	cfg.Receiver.Located, cfg.Receiver.Lat, cfg.Receiver.Lon = true, 51.5, 7.0
	cfg.Feeds = []model.Feed{{ID: "json", Enabled: true, Type: model.FeedSBS}}
	tr := New(Options{Config: &cfg})
	for i := 0; i < 5; i++ {
		tr.HandleUpdate(model.DecodedUpdate{
			Recv: simStart.Add(time.Duration(i) * time.Second), FeedID: "json", ICAO: 0x3C6444,
			Lat: model.F(51.52), Lon: model.F(7.0), OnGround: model.B(true), Msgs: 1,
		})
	}
	done := make(chan struct{})
	go func() {
		tr.Snapshot()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot blocked: tracker mutex left locked")
	}
	if ac, ok := tr.Aircraft(0x3C6444); !ok || ac.Lat == nil {
		t.Fatalf("surface position not tracked: %v %+v", ok, ac.Lat)
	}

	s := newAntStats("x")
	s.addRange(2, 0.5, 0, 2, math.NaN(), -20) // must not panic
	if s.covDirty {
		t.Error("altitude-less position entered the coverage grid")
	}
}

// A panic inside ingest must not leave t.mu locked.
func TestIngestPanicReleasesLock(t *testing.T) {
	cfg := model.DefaultConfig()
	tr := New(Options{Config: &cfg})
	tr.ac = nil // assignment to a nil map panics inside ingestUpdate
	func() {
		defer func() { _ = recover() }()
		tr.HandleUpdate(model.DecodedUpdate{Recv: simStart, FeedID: "x", ICAO: 0x111111, Msgs: 1})
	}()
	if !tr.mu.TryLock() {
		t.Fatal("mutex still held after a recovered panic")
	}
	tr.mu.Unlock()
}

// After the host clock steps backwards, data time follows the frames again
// instead of staying frozen at the old (future) base.
func TestClockStepBackwards(t *testing.T) {
	old := clockStepHold
	clockStepHold = 50 * time.Millisecond
	defer func() { clockStepHold = old }()

	cfg := model.DefaultConfig()
	cfg.Receiver.Located, cfg.Receiver.Lat, cfg.Receiver.Lon = true, 50.0, 8.5
	cfg.Feeds = []model.Feed{{ID: "sbs", Enabled: true, Type: model.FeedSBS}}
	tr := New(Options{Config: &cfg})
	upd := func(at time.Time) {
		tr.HandleUpdate(model.DecodedUpdate{Recv: at, FeedID: "sbs", ICAO: 0x3C4B26,
			Lat: model.F(50.5), Lon: model.F(9.0), AltBaro: model.F(30000), Msgs: 1})
	}
	for i := 0; i < 30; i++ {
		upd(simStart.Add(time.Duration(i) * time.Second))
	}
	back := simStart.Add(-time.Hour)
	upd(back)
	time.Sleep(80 * time.Millisecond)
	for i := 1; i <= 10; i++ {
		upd(back.Add(time.Duration(i) * time.Second))
	}
	tr.mu.Lock()
	now := tr.nowMs
	tr.mu.Unlock()
	want := back.Add(10 * time.Second).UnixMilli()
	if d := now - want; d < -1000 || d > 1000 {
		t.Fatalf("data time %v, want about %v (off by %d ms)", time.UnixMilli(now).UTC(), time.UnixMilli(want).UTC(), d)
	}

	// A single late frame (a delayed feed) does not rebase.
	upd(back.Add(-2 * time.Minute))
	tr.mu.Lock()
	now2 := tr.nowMs
	tr.mu.Unlock()
	if now2 != now {
		t.Errorf("single late frame moved data time: %d -> %d", now, now2)
	}
}
