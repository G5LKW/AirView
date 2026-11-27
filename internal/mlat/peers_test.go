package mlat

import (
	"testing"
	"time"

	"airview/internal/sim/simnet"
)

// TestSymmetricPeers: every edge of the sync matrix appears in the peer
// list of both stations, SyncedPeers and the mlat-client PeerCount agree,
// and a reference station that lists no peers of its own still reports
// the stations syncing against it.
func TestSymmetricPeers(t *testing.T) {
	h := newHarness(t, harnessCfg{seed: 11, devs: map[simnet.Device]int{simnet.DeviceRadarcapeGPS: 3, simnet.DeviceSDR: 5},
		radiusKm: 120, modeSFrac: 0.3, evalFromS: 1e9})
	h.run(90 * time.Second)
	edges := h.e.SyncMatrix()
	if len(edges) == 0 {
		t.Fatal("no sync edges after 90 s")
	}
	ownEmpty := 0
	for _, ed := range edges {
		for _, pair := range [][2]uint32{{uint32(ed.A), uint32(ed.B)}, {uint32(ed.B), uint32(ed.A)}} {
			rep, ok := h.e.Station(ed.A)
			if pair[0] == uint32(ed.B) {
				rep, ok = h.e.Station(ed.B)
			}
			if !ok || rep.Clock == nil {
				t.Fatalf("station %d: no clock report", pair[0])
			}
			found := false
			for _, p := range rep.Clock.Peers {
				found = found || uint32(p.Peer) == pair[1]
			}
			if !found {
				t.Fatalf("edge %d-%d missing from the peers of %d: %+v", ed.A, ed.B, pair[0], rep.Clock.Peers)
			}
		}
	}
	for _, sc := range h.e.Filter().Snapshot().Stations() {
		rep, _ := h.e.Station(sc.Station)
		cs, ok := h.e.ClientStats(sc.Station)
		if !ok || rep.Clock == nil || cs.PeerCount != rep.Clock.SyncedPeers {
			t.Fatalf("station %d: PeerCount %d vs SyncedPeers %+v", sc.Station, cs.PeerCount, rep.Clock)
		}
		if rep.Clock.SyncedPeers < sc.SyncedPeers() {
			t.Fatalf("station %d: symmetric %d < own %d", sc.Station, rep.Clock.SyncedPeers, sc.SyncedPeers())
		}
		if len(sc.Peers) == 0 && len(rep.Clock.Peers) > 0 {
			ownEmpty++
		}
	}
	t.Logf("%d edges; %d stations with no own peers gained mirrored ones", len(edges), ownEmpty)
}
