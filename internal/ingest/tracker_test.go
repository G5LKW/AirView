package ingest

import (
	"context"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

// collectObs keeps the raw frames of a simnet run per receiver.
type collectObs struct {
	n      *simnet.Network
	frames [][]model.RawFrame
}

func (c *collectObs) Observe(obs []simnet.Observation) {
	for i := range obs {
		f := c.n.RawFrame(&obs[i])
		if f.MLAT {
			continue
		}
		f.Data = append([]byte(nil), f.Data...)
		f.Recv = time.Time{}
		c.frames[obs[i].Rx] = append(c.frames[obs[i].Rx], f)
	}
}
func (c *collectObs) Advance(float64) {}

// TestRealTrackerConcurrent runs the ingest with the real tracker and lanes,
// one goroutine per station delivering its frames in real time order
// (compressed), the sweeper running: every Mode S copy must be credited to
// its station's lane exactly once (first copies inside Handle, the others
// through Lanes.Credit, including credits owed while Handle was running).
func TestRealTrackerConcurrent(t *testing.T) {
	const rx = 24
	n := newSimNet(t, 3, rx, 120, nil)
	col := &collectObs{n: n, frames: make([][]model.RawFrame, rx)}
	n.AddSink(col)
	if err := n.Advance(context.Background(), 15*time.Second); err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{}
	res := newFakeRes()
	lanes := tracker.NewLanes(tracker.LaneOptions{Resolver: res})
	tr := tracker.New(tracker.Options{Config: &cfg, Logger: quietLogger()})
	tr.SetNetwork(res, lanes)

	// The frames carry simulated time; the server clock follows it: now =
	// newest delivered host time + 20 ms.
	var mu sync.Mutex
	var newest int64
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return time.Unix(0, newest+int64(20*time.Millisecond))
	}
	ing, err := New(Options{Handler: tr, Resolver: res, Now: now, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ing.Run(ctx); close(done) }()

	start := n.UnixNs(0)
	wall := time.Now()
	const speed = 30 // 15 s of traffic in 0.5 s
	var wg sync.WaitGroup
	modeS := make([]int, rx)
	for r := 0; r < rx; r++ {
		for _, f := range col.frames[r] {
			if len(f.Data) == 7 || len(f.Data) == 14 {
				modeS[r]++
			}
		}
		wg.Add(1)
		go func(fs []model.RawFrame) {
			defer wg.Done()
			for i := 0; i < len(fs); {
				el := time.Since(wall) * speed
				j := i
				for j < len(fs) && fs[j].HostNs-start <= int64(el) {
					j++
				}
				if j == i {
					time.Sleep(time.Millisecond)
					continue
				}
				mu.Lock()
				newest = max(newest, fs[j-1].HostNs)
				mu.Unlock()
				ing.Submit(fs[i:j], false)
				i = j
			}
		}(col.frames[r])
	}
	wg.Wait()
	cancel()
	<-done

	st := ing.Stats()
	total := 0
	for r := 0; r < rx; r++ {
		info, ok := lanes.Info(model.StationIdx(r + 2))
		if !ok {
			t.Fatalf("no lane for station %d", r+2)
		}
		if int(info.Frames)+int(info.LateFrames) != modeS[r] {
			t.Errorf("station %d: lane credited %d (+%d late) of %d Mode S copies", r+2, info.Frames, info.LateFrames, modeS[r])
		}
		total += modeS[r]
	}
	snap := tr.Snapshot()
	t.Logf("%d copies, %d unique, joined %d, tooOld %d, lost %d; tracker shows %d aircraft", total, st.Groups, st.Joined, st.TooOld, st.Lost, snap.Counts.Aircraft)
	if snap.Counts.Aircraft < 20 {
		t.Errorf("tracker shows only %d aircraft", snap.Counts.Aircraft)
	}
	if st.Lost != 0 {
		t.Errorf("%d credits lost", st.Lost)
	}
}
