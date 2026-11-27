package engine

import (
	"sync/atomic"

	"airview/internal/model"
)

// hostSink puts the aggregator host's own feeds into the network ingest as
// station #1 (DESIGN §1 "Host station"). Frames and updates keep everything
// they carry (clock class, ticks, channel, signal); the station index is
// set and the local feed ID is replaced by the namespaced form
// st/<hostSID>/<rx>, where rx is the feed's 1-based position in the
// configured feed list (0 for feeds that are not configured, such as MLAT
// results injected by an uplink).
type hostSink struct {
	ingest NetIngest
	sid    string
	feeds  atomic.Pointer[map[string]string]
	frames atomic.Uint64
	upd    atomic.Uint64
	unk    string // feed ID of unconfigured local sources
}

func newHostSink(in NetIngest, sid string) *hostSink {
	h := &hostSink{ingest: in, sid: sid, unk: model.StationFeedID(sid, 0)}
	m := map[string]string{}
	h.feeds.Store(&m)
	return h
}

// setFeeds rebuilds the local-to-namespaced feed ID map.
func (h *hostSink) setFeeds(feeds []model.Feed) {
	m := make(map[string]string, len(feeds))
	for i, f := range feeds {
		if f.ID != "" {
			m[f.ID] = model.StationFeedID(h.sid, i+1)
		}
	}
	h.feeds.Store(&m)
}

func (h *hostSink) feedID(local string) string {
	if id, ok := (*h.feeds.Load())[local]; ok {
		return id
	}
	return h.unk
}

// HandleFrame implements model.FrameSink.
func (h *hostSink) HandleFrame(f model.RawFrame) {
	f.Station = model.HostStation
	f.FeedID = h.feedID(f.FeedID)
	h.frames.Add(1)
	h.ingest.HandleFrame(f)
}

// HandleUpdate implements model.FrameSink.
func (h *hostSink) HandleUpdate(u model.DecodedUpdate) {
	u.Station = model.HostStation
	u.FeedID = h.feedID(u.FeedID)
	h.upd.Add(1)
	h.ingest.HandleUpdate(u)
}
