package registry

import (
	"bytes"
	"context"
	"net/netip"
	"testing"
	"time"

	"airview/internal/ingest/beastin"
	"airview/internal/model"
)

const beastTestUUID = "0123456789abcdef0123456789abcdef"

func TestBeastAdapter(t *testing.T) {
	ctx := context.Background()
	r, _, _ := openTest(t, model.AdmissionOpen)
	b := r.Beast()
	ip := netip.MustParseAddr("192.0.2.10")

	id := beastin.Identity{Link: 1, Kind: model.StationKindBeast, UUID: beastTestUUID, Slot: -1, RemoteIP: ip}
	a, err := b.Attach(ctx, id)
	if err != nil || !a.Admitted() || a.Admission != model.AdmApproved {
		t.Fatalf("attach: %+v %v", a, err)
	}
	st, ok := r.Station(a.Station)
	if !ok || st.UUID != beastTestUUID || a.FeedID != model.StationFeedID(st.SID, 0) {
		t.Fatalf("station %+v feed %q", st, a.FeedID)
	}
	if log, err := r.ConnLog(ctx, a.Station, time.Time{}, 10); err != nil || len(log) != 1 || log[0].Proto != model.StationKindBeast {
		t.Fatalf("conn log %+v %v", log, err)
	}
	b.Detach(id, a)
	// A reconnect of the same UUID gets the same station.
	if a2, err := b.Attach(ctx, beastin.Identity{Link: 2, UUID: beastTestUUID, Slot: -1}); err != nil || a2.Station != a.Station {
		t.Fatalf("reconnect %+v %v", a2, err)
	}
	// A relay sub-station is a separate station.
	rel, err := b.Attach(ctx, beastin.Identity{Link: 3, UUID: beastTestUUID, Slot: -1, Relay: true, ReceiverID: 0xabc})
	if err != nil || rel.Station == a.Station || !rel.Admitted() {
		t.Fatalf("relay %+v %v", rel, err)
	}
	// UUID-less slots are separate stations and stable per key.
	u0 := beastin.Identity{Link: 4, IPKey: "ip:aaaa", Slot: 0}
	u1 := beastin.Identity{Link: 5, IPKey: "ip:aaaa", Slot: 1}
	a0, err0 := b.Attach(ctx, u0)
	a1, err1 := b.Attach(ctx, u1)
	if err0 != nil || err1 != nil || a0.Station == a1.Station || !a0.Admitted() {
		t.Fatalf("ip slots %+v %+v %v %v", a0, a1, err0, err1)
	}
	if again, _ := b.Attach(ctx, u0); again.Station != a0.Station {
		t.Fatal("ip slot 0 not stable")
	}
	if k := r.IngestKey(); len(k) != 32 || bytes.Equal(k, r.IngestKey()[:0]) {
		t.Fatalf("ingest key %x", k)
	}
}

func TestBeastAdapterInvite(t *testing.T) {
	ctx := context.Background()
	r, _, _ := openTest(t, model.AdmissionInvite)
	b := r.Beast()
	a, err := b.Attach(ctx, beastin.Identity{Link: 1, UUID: beastTestUUID, Slot: -1})
	if err != nil || a.Admission != model.AdmPending || !a.Admitted() {
		t.Fatalf("invite UUID: %+v %v", a, err)
	}
	a, err = b.Attach(ctx, beastin.Identity{Link: 2, IPKey: "ip:bbbb", Slot: 0})
	if err != nil || a.Admitted() || a.Reason == "" {
		t.Fatalf("invite without UUID must be refused: %+v %v", a, err)
	}
}
