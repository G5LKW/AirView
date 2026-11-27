package mlat

import (
	"sort"

	"airview/internal/mlat/clock"
	"airview/internal/model"
)

// Symmetric peer lists.
//
// The clock filter's StationClock.Peers lists only the reference peers a
// station's own residuals were taken against. A station that mostly serves
// as the reference (a GNSS station in an SDR network) would list no peers
// and report SyncedPeers 0 while every other station syncs against it. The
// engine therefore mirrors each edge A→B as B→A (relative frequency
// negated, the same RMS and count); a pair seen in both directions keeps
// the direction with more co-observations. The index is built once per
// clock snapshot (O(stations × peers)) and shared by all readers.

type peerIndex struct {
	snap  *clock.Snapshot
	peers map[model.StationIdx][]clock.PeerStat
}

func buildPeerIndex(snap *clock.Snapshot) *peerIndex {
	by := map[model.StationIdx]map[model.StationIdx]clock.PeerStat{}
	put := func(self model.StationIdx, p clock.PeerStat) {
		if p.Count == 0 || p.Peer == self {
			return
		}
		m := by[self]
		if m == nil {
			m = map[model.StationIdx]clock.PeerStat{}
			by[self] = m
		}
		if cur, ok := m[p.Peer]; ok && cur.Count >= p.Count {
			return
		}
		m[p.Peer] = p
	}
	for _, sc := range snap.Stations() {
		for _, p := range sc.Peers {
			put(sc.Station, p)
		}
	}
	for _, sc := range snap.Stations() {
		for _, p := range sc.Peers {
			put(p.Peer, clock.PeerStat{Peer: sc.Station, Count: p.Count, RMSNs: p.RMSNs, RelPPB: -p.RelPPB, SameDom: p.SameDom})
		}
	}
	idx := &peerIndex{snap: snap, peers: make(map[model.StationIdx][]clock.PeerStat, len(by))}
	for st, m := range by {
		l := make([]clock.PeerStat, 0, len(m))
		for _, p := range m {
			l = append(l, p)
		}
		sort.Slice(l, func(i, j int) bool { return l[i].Peer < l[j].Peer })
		idx.peers[st] = l
	}
	return idx
}

// symPeers returns the symmetric peer list of a station in the latest
// clock snapshot (read-only; shared) and the number of synced peers
// (co-observations in the window and the same clock domain).
func (e *Engine) symPeers(st model.StationIdx) ([]clock.PeerStat, int) {
	snap := e.filter.Snapshot()
	idx := e.peerIdx.Load()
	if idx == nil || idx.snap != snap {
		idx = buildPeerIndex(snap)
		e.peerIdx.Store(idx)
	}
	l := idx.peers[st]
	n := 0
	for _, p := range l {
		if p.SameDom {
			n++
		}
	}
	return l, n
}
