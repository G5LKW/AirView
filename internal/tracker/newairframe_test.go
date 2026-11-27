package tracker

import (
	"context"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

type afRecorder struct {
	mu sync.Mutex
	n  map[uint32]int
}

func (r *afRecorder) hook(icao uint32) {
	r.mu.Lock()
	r.n[icao]++
	r.mu.Unlock()
}

func (r *afRecorder) count(icao uint32) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[icao]
}

func feedDF11(tr *Tracker, icao uint32, at time.Time, n int) {
	for i := 0; i < n; i++ {
		tr.HandleFrame(model.RawFrame{Recv: at.Add(time.Duration(i) * 100 * time.Millisecond), GPSNs: -1, Signal: -20,
			Kind: model.FrameModeSShort, Data: df11(icao), FeedID: "x", Channel: -1})
	}
}

// TestNewAirframeHookFlushRace: the aircraft row of a brand-new airframe is
// written by a flush BEFORE the first-sighting check runs (the order the
// periodic flush in HandleFrame or a concurrent Flush produces). The hook
// must still fire, exactly once; a known airframe must never fire.
func TestNewAirframeHookFlushRace(t *testing.T) {
	st, err := store.OpenWithOptions(t.TempDir(), store.Options{Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const fresh, known = 0x3C6444, 0x406B75
	// known: seen a day earlier
	b := store.NewBatch()
	b.UpsertAircraft([]store.AircraftSeen{{ICAO: known, FirstSeen: simStart.Add(-24 * time.Hour).Unix(), LastSeen: simStart.Add(-23 * time.Hour).Unix(), Msgs: 5}})
	if err := st.Apply(ctx, b); err != nil {
		t.Fatal(err)
	}
	cfg := model.DefaultConfig()
	tr := New(Options{Store: st, Config: &cfg, Logger: quietLog()})
	rec := &afRecorder{n: map[uint32]int{}}
	tr.SetNewAirframeHook(rec.hook)

	feedDF11(tr, fresh, simStart, 3)
	feedDF11(tr, known, simStart, 3)
	// The flush wins the race: rows are committed before the check.
	tr.mu.Lock()
	tr.flush(true)
	tr.mu.Unlock()
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.AircraftByICAO(ctx, fresh); !ok {
		t.Fatal("precondition: the new airframe's row was not written by the flush")
	}
	tr.resolveRefdata(ctx)
	if err := tr.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	tr.resolveRefdata(ctx)
	if n := rec.count(fresh); n != 1 {
		t.Errorf("new airframe hook fired %d times, want 1", n)
	}
	if n := rec.count(known); n != 0 {
		t.Errorf("known airframe fired %d times", n)
	}

	// The same airframe returning later in the session (new instance) does
	// not fire again.
	feedDF11(tr, fresh, simStart.Add(40*time.Minute), 3)
	if err := tr.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rec.count(fresh); n != 1 {
		t.Errorf("returning airframe: hook fired %d times, want 1", n)
	}
}

// TestNewAirframeHookDuringFlush creates aircraft while flushes and checks
// run concurrently (run with -race where available).
func TestNewAirframeHookDuringFlush(t *testing.T) {
	st, err := store.OpenWithOptions(t.TempDir(), store.Options{Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cfg := model.DefaultConfig()
	tr := New(Options{Store: st, Config: &cfg, Logger: quietLog()})
	rec := &afRecorder{n: map[uint32]int{}}
	tr.SetNewAirframeHook(rec.hook)

	const n = 60
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = tr.Flush(ctx)
		}
	}()
	for i := 0; i < n; i++ {
		feedDF11(tr, 0x3C0000+uint32(i), simStart.Add(time.Duration(i)*time.Second), 3)
		if i%7 == 0 {
			tr.mu.Lock()
			tr.flush(true)
			tr.mu.Unlock()
		}
	}
	close(done)
	wg.Wait()
	if err := tr.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	tr.resolveRefdata(ctx)
	for i := 0; i < n; i++ {
		if c := rec.count(0x3C0000 + uint32(i)); c != 1 {
			t.Errorf("%06X: hook fired %d times, want 1", 0x3C0000+i, c)
		}
	}
}
