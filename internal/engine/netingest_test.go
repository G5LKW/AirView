package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/tracker"
)

// fakeNet records what the grouper hands to the tracker.
type fakeNet struct {
	handled atomic.Int64
	late    atomic.Int64
	updates atomic.Int64
}

func (f *fakeNet) Handle(fr model.RawFrame) tracker.Result {
	if fr.Late {
		f.late.Add(1)
	} else {
		f.handled.Add(1)
	}
	return tracker.Result{}
}
func (f *fakeNet) HandleUpdate(model.DecodedUpdate) { f.updates.Add(1) }
func (f *fakeNet) Lanes() *tracker.Lanes            { return nil }

// fakeRes approves every station except those listed as pending.
type fakeRes struct{ pending map[model.StationIdx]bool }

func (r fakeRes) Info(idx model.StationIdx) (model.StationMeta, bool) {
	if idx > 1000 {
		return model.StationMeta{}, false
	}
	adm := model.AdmApproved
	if r.pending[idx] {
		adm = model.AdmPending
	}
	return model.StationMeta{Idx: idx, Admission: adm}, true
}
func (r fakeRes) Geo(model.StationIdx) (model.StationGeo, bool) { return model.StationGeo{}, false }

func frame(st model.StationIdx, ch int, data ...byte) model.RawFrame {
	return model.RawFrame{Station: st, Channel: ch, Data: data, Recv: time.Now()}
}

func TestGroupIngestDedup(t *testing.T) {
	h := &fakeNet{}
	g := newGroupIngest(h, fakeRes{pending: map[model.StationIdx]bool{7: true}})
	clock := int64(1)
	g.now = func() int64 { return clock }

	a := []byte{0x8d, 1, 2, 3, 4, 5, 6}
	b := []byte{0x5d, 9, 9, 9}
	// One transmission heard by three stations (one of them on two channels).
	g.HandleFrame(frame(2, 0, a...))
	g.HandleFrame(frame(3, 0, a...))
	g.HandleFrame(frame(4, 0, a...))
	g.HandleFrame(frame(4, 1, a...)) // the other channel of station 4: same transmission
	if got := h.handled.Load(); got != 1 {
		t.Fatalf("tracker saw %d copies, want 1", got)
	}
	if s := g.Stats(); s.Credits != 3 || s.Unique != 1 {
		t.Fatalf("stats %+v", s)
	}
	// Station 2 hears the same bytes again on the same channel: a repeated
	// transmission (DF11 squitter, identical DF17), not a copy.
	clock += int64(400 * time.Millisecond)
	g.HandleFrame(frame(2, 0, a...))
	if got := h.handled.Load(); got != 2 {
		t.Fatalf("repeat not opened as a new transmission: %d", got)
	}
	if s := g.Stats(); s.Repeats != 1 {
		t.Fatalf("repeats %d", s.Repeats)
	}
	// Copies of the repeat from other stations are credited to it.
	g.HandleFrame(frame(3, 0, a...))
	if got := h.handled.Load(); got != 2 {
		t.Fatalf("copy of the repeat handled: %d", got)
	}
	// Another payload is another transmission.
	g.HandleFrame(frame(3, 0, b...))
	if got := h.handled.Load(); got != 3 {
		t.Fatalf("handled %d", got)
	}
	// After the window the same bytes are a new transmission.
	clock += int64(2500 * time.Millisecond)
	g.HandleFrame(frame(5, 0, a...))
	if got := h.handled.Load(); got != 4 {
		t.Fatalf("after the window: handled %d", got)
	}
	// Pending stations never reach the live picture; unknown ones are dropped.
	g.HandleFrame(frame(7, 0, b...))
	g.HandleFrame(frame(5000, 0, b...))
	if got := h.handled.Load(); got != 4 {
		t.Fatalf("pending/unknown reached the tracker: %d", got)
	}
	if s := g.Stats(); s.Pending != 1 || s.Unknown != 1 {
		t.Fatalf("stats %+v", s)
	}
	// Late frames bypass the grouper.
	lf := frame(2, 0, a...)
	lf.Late = true
	g.HandleFrame(lf)
	if h.late.Load() != 1 || h.handled.Load() != 4 {
		t.Fatalf("late frame: late=%d handled=%d", h.late.Load(), h.handled.Load())
	}
	// Updates: approved pass, pending do not.
	g.HandleUpdate(model.DecodedUpdate{Station: 2, ICAO: 1})
	g.HandleUpdate(model.DecodedUpdate{Station: 7, ICAO: 1})
	if h.updates.Load() != 1 {
		t.Fatalf("updates %d", h.updates.Load())
	}
}

func TestGroupIngestWindowBoundary(t *testing.T) {
	h := &fakeNet{}
	g := newGroupIngest(h, nil)
	ms := func(v int) int64 { return int64(v) * int64(time.Millisecond) }
	clock := ms(500)
	g.now = func() int64 { return clock }
	a := []byte{1, 2, 3}
	g.HandleFrame(frame(2, 0, a...))
	// 0.7 s later the shard rotates (1 s since its last rotation), but the
	// copy is still inside the first copy's window: credited, not handled.
	clock = ms(1200)
	g.HandleFrame(frame(3, 0, a...))
	if s := g.Stats(); s.Unique != 1 || s.Credits != 1 {
		t.Fatalf("across the rotation: %+v", s)
	}
	// 1.1 s after the first copy: a new transmission.
	clock = ms(1600)
	g.HandleFrame(frame(4, 0, a...))
	if s := g.Stats(); s.Unique != 2 {
		t.Fatalf("after the window: %+v", s)
	}
}

func TestGroupIngestConcurrent(t *testing.T) {
	h := &fakeNet{}
	g := newGroupIngest(h, nil)
	const stations, msgs = 16, 2000
	var wg sync.WaitGroup
	for s := 0; s < stations; s++ {
		wg.Add(1)
		go func(st model.StationIdx) {
			defer wg.Done()
			for i := 0; i < msgs; i++ {
				g.HandleFrame(frame(st, 0, byte(i), byte(i>>8), 0x8d, 0x40))
			}
		}(model.StationIdx(s + 2))
	}
	wg.Wait()
	s := g.Stats()
	if s.Unique != msgs {
		t.Fatalf("unique %d, want %d (stats %+v)", s.Unique, msgs, s)
	}
	if s.Copies != stations*msgs || s.Unique+s.Credits != s.Copies {
		t.Fatalf("stats %+v", s)
	}
}

func BenchmarkGroupIngestCopy(b *testing.B) {
	h := &fakeNet{}
	g := newGroupIngest(h, fakeRes{})
	data := make([][]byte, 1024)
	for i := range data {
		data[i] = []byte{0x8d, byte(i), byte(i >> 8), 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 8 copies per transmission from 8 stations.
		g.HandleFrame(model.RawFrame{Station: model.StationIdx(2 + i%8), Data: data[(i/8)%1024]})
	}
}
