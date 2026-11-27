package mlatcsim

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSteadyStateMemory runs the MLAT engine, the ingest and the
// mlat-client server for 40 simulated minutes (aircraft come and go) and
// requires the live heap to stop growing once the bounded stores have
// filled: between minute 20 and minute 40 it may grow by at most 16 MB
// (release soak finding: an aggregator's memory was still rising after 30
// minutes). Track and selection counts must stay near the aircraft count.
func TestSteadyStateMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("simulated 40 minutes")
	}
	via := make([]Transport, len(mixedDevices))
	for _, i := range mixedMLATC {
		via[i] = MLATC
	}
	const aircraft = 250
	heap := map[int]uint64{}
	var trace []string
	var r *Run
	cfg := Config{Seed: 3, Devices: mixedDevices, Via: via, Aircraft: aircraft,
		Duration: 40 * time.Minute, EvalFrom: 40 * time.Minute, // no evaluation samples: they accumulate by design
		Logf: func(format string, args ...any) {
			var m int
			if _, err := fmt.Sscanf(strings.TrimPrefix(fmt.Sprintf(format, args...), "t="), "%dm", &m); err != nil {
				return
			}
			if m%10 != 0 {
				return
			}
			runtime.GC()
			runtime.GC() // pooled buffers survive one cycle
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			heap[m] = ms.HeapAlloc
			st := r.Engine.Stats()
			trace = append(trace, fmt.Sprintf("t=%dm heap %.1f MB tracks %d hypotheses %d ref %+v", m,
				float64(ms.HeapAlloc)/(1<<20), st.Track.Tracks, st.Track.Hypotheses, st.Ref))
			if st.Track.Tracks+st.Track.Hypotheses > 3*aircraft {
				t.Errorf("t=%dm: %d tracks + %d hypotheses for %d aircraft", m, st.Track.Tracks, st.Track.Hypotheses, aircraft)
			}
		}}
	var err error
	r, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, l := range trace {
		t.Log(l)
	}
	h20, h40 := heap[20], heap[40]
	if h20 == 0 || h40 == 0 {
		t.Fatalf("no heap samples: %v", heap)
	}
	if h40 > h20+16<<20 {
		t.Errorf("live heap still growing: %.1f MB at 20 min, %.1f MB at 40 min", float64(h20)/(1<<20), float64(h40)/(1<<20))
	}
}
