package engine

import (
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/ingest/beastin"
	"airview/internal/model"
	"airview/internal/tracker"
)

type closeCounter struct{ n *atomic.Int32 }

func (c closeCounter) Close() { c.n.Add(1) }

func TestStationRuntimesLRUAndRetire(t *testing.T) {
	var closed atomic.Int32
	s := newStationRuntimes(stationRuntimeOptions{MaxActive: 2, RetireAfter: 30 * time.Minute, Logger: quietLogger()})
	s.Register("probe", func(m model.StationMeta) StationComponent {
		if m.Kind == model.StationKindBeast {
			return nil // does not apply
		}
		return closeCounter{&closed}
	})
	t0 := time.Unix(1_800_000_000, 0)
	r2 := s.Acquire(model.StationMeta{Idx: 2, Kind: model.StationKindAVF}, t0)
	if r2.Component("probe") == nil {
		t.Fatal("component not built")
	}
	if r := s.Acquire(model.StationMeta{Idx: 3, Kind: model.StationKindBeast}, t0); r.Component("probe") != nil {
		t.Fatal("factory returned nil but a component exists")
	}
	if again := s.Acquire(model.StationMeta{Idx: 2}, t0.Add(time.Second)); again != r2 {
		t.Fatal("second Acquire built a new runtime")
	}
	// A third station evicts the least recently used one (3).
	s.Acquire(model.StationMeta{Idx: 4, Kind: model.StationKindAVF}, t0.Add(2*time.Second))
	if _, ok := s.Get(3); ok || s.Len() != 2 {
		t.Fatalf("LRU: len %d", s.Len())
	}
	// Station 2 keeps being heard, station 4 goes silent: 4 is retired.
	for m := 0; m <= 31; m++ {
		now := t0.Add(time.Duration(m) * time.Minute)
		s.observe([]tracker.LaneInfo{{Station: 2, Frames: uint64(100 + m)}, {Station: 4, Frames: 7}}, now)
	}
	if _, ok := s.Get(4); ok {
		t.Fatal("silent station not retired")
	}
	if _, ok := s.Get(2); !ok {
		t.Fatal("busy station retired")
	}
	if closed.Load() != 1 || s.Retired() != 2 {
		t.Fatalf("closed %d retired %d", closed.Load(), s.Retired())
	}
	s.closeAll()
	if closed.Load() != 2 || s.Len() != 0 {
		t.Fatalf("closeAll: closed %d len %d", closed.Load(), s.Len())
	}
}

type recSink struct {
	frames  []model.RawFrame
	updates []model.DecodedUpdate
}

func (r *recSink) HandleFrame(f model.RawFrame)       { r.frames = append(r.frames, f) }
func (r *recSink) HandleUpdate(u model.DecodedUpdate) { r.updates = append(r.updates, u) }
func (r *recSink) HandleBeast(b *beastin.Batch) {
	for _, f := range b.Frames {
		r.HandleFrame(f)
	}
}
func (r *recSink) asIngest() NetIngest { return r }

func TestHostSinkNamespacesFeeds(t *testing.T) {
	r := &recSink{}
	h := newHostSink(r.asIngest(), "abcdefghijklmnop")
	h.setFeeds([]model.Feed{{ID: "evo"}, {ID: "sbs"}})
	h.HandleFrame(model.RawFrame{FeedID: "sbs", Channel: 1, Data: []byte{1}})
	h.HandleFrame(model.RawFrame{FeedID: "uplink/club"})
	h.HandleUpdate(model.DecodedUpdate{FeedID: "evo", ICAO: 5})
	if len(r.frames) != 2 || len(r.updates) != 1 {
		t.Fatalf("got %d frames %d updates", len(r.frames), len(r.updates))
	}
	if f := r.frames[0]; f.Station != model.HostStation || f.FeedID != "st/abcdefghijklmnop/2" || f.Channel != 1 {
		t.Fatalf("frame %+v", f)
	}
	if f := r.frames[1]; f.FeedID != "st/abcdefghijklmnop/0" {
		t.Fatalf("unconfigured source: %q", f.FeedID)
	}
	if u := r.updates[0]; u.Station != model.HostStation || u.FeedID != "st/abcdefghijklmnop/1" {
		t.Fatalf("update %+v", u)
	}
	if h.frames.Load() != 2 {
		t.Fatalf("frames %d", h.frames.Load())
	}
}
