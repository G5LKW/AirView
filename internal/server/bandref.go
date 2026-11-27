package server

import (
	"context"
	"strconv"
	"sync"
	"time"

	"airview/internal/registry"
)

// Peer distance bands (DESIGN §8.2) are computed from a position frozen per
// viewer station and tag period (finding PRIV-1). The owner controls the
// station's effective position (RECEIVER / handshake records), and the
// peer tags stay the same for the whole ISO week; bands computed from the
// live position were a distance oracle: move, query, move back, and every
// band flip measured the distance to one hidden peer. With the frozen
// reference, each viewer station gets bands from one position per tag
// period, however often it moves; a new period brings new tags, so bands
// of different periods cannot be joined per peer either.

type geoPoint struct{ lat, lon float64 }

type bandRefEntry struct {
	period string
	pos    geoPoint
}

// bandRefTable maps a viewer station SID to its frozen reference.
type bandRefTable struct {
	mu sync.Mutex
	m  map[string]bandRefEntry
}

// tagPeriod names the peer-tag period of t (registry.PeerTag: the ISO week).
func tagPeriod(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return strconv.Itoa(y) + "W" + strconv.Itoa(w)
}

// bandRef returns the position the viewer station's peer bands are computed
// from: for the admin the live position; for an owner the station's
// position at its first request of the current tag period. ok is false
// when the station has no position.
func (s *Server) bandRef(st *registry.Station, admin bool, now time.Time) (geoPoint, bool) {
	if st == nil || !st.Located {
		return geoPoint{}, false
	}
	cur := geoPoint{st.Effective.Lat, st.Effective.Lon}
	if admin {
		return cur, true
	}
	if reg, err := s.reg(); err == nil && reg != nil {
		// Persisted in stations.db: a restart does not refreeze (PRIV-1).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lat, lon := reg.FrozenBandRef(ctx, st.SID, registry.TagPeriod(now), cur.lat, cur.lon)
		return geoPoint{lat, lon}, true
	}
	return s.bandRefs.get(st.SID, tagPeriod(now), cur), true
}

// get returns the frozen reference of sid for period, freezing cur when
// there is none for this period. Entries of other periods are replaced;
// the table holds at most one entry per station.
func (t *bandRefTable) get(sid, period string, cur geoPoint) geoPoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]bandRefEntry{}
	}
	if e, ok := t.m[sid]; ok && e.period == period {
		return e.pos
	}
	if len(t.m) >= 100_000 { // far beyond any station count: start over
		clear(t.m)
	}
	t.m[sid] = bandRefEntry{period: period, pos: cur}
	return cur
}
