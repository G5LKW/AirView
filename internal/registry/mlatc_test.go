package registry

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"airview/internal/ingest/mlatc"
	"airview/internal/model"
)

func TestMLATCAdapter(t *testing.T) {
	ctx := context.Background()
	r, _, _ := openTest(t, model.AdmissionOpen)
	m := r.MLATC()
	ip := netip.MustParseAddr("192.0.2.20")

	id := mlatc.Identity{Link: 1, Kind: model.StationKindMLATC, UUID: beastTestUUID, Slot: -1, RemoteIP: ip,
		User: "rooftop", ClockCls: model.ClockSDR12, Located: true, Lat: 50.1, Lon: 8.2, Alt: 120}
	a, err := m.Attach(ctx, id)
	if err != nil || !a.Admitted() || a.Admission != model.AdmApproved {
		t.Fatalf("attach: %+v %v", a, err)
	}
	st, ok := r.Station(a.Station)
	if !ok || st.UUID != beastTestUUID || a.FeedID != model.StationFeedID(st.SID, 0) || st.Kind != model.StationKindMLATC {
		t.Fatalf("station %+v feed %q", st, a.FeedID)
	}
	if !st.Declared.Valid || st.Declared.Lat != 50.1 || st.DeclaredAltRef != model.AltRefAmbiguous {
		t.Fatalf("declared %+v %q", st.Declared, st.DeclaredAltRef)
	}
	if log, err := r.ConnLog(ctx, a.Station, time.Time{}, 10); err != nil || len(log) != 1 || log[0].Proto != model.StationKindMLATC {
		t.Fatalf("conn log %+v %v", log, err)
	}
	m.Detach(id, a)
	// An owner-declared position is never replaced by the handshake's.
	if _, err := r.SetDeclared(ctx, a.Station, Position{Lat: 51, Lon: 9, Alt: 10, Valid: true}, model.AltRefMSL); err != nil {
		t.Fatal(err)
	}
	id.Link = 2
	a2, err := m.Attach(ctx, id)
	if err != nil || a2.Station != a.Station {
		t.Fatalf("reconnect %+v %v", a2, err)
	}
	if st, _ := r.Station(a.Station); st.Declared.Lat != 51 || st.DeclaredAltRef != model.AltRefMSL {
		t.Fatalf("owner position replaced: %+v", st.Declared)
	}
	m.Detach(id, a2)
	// UUID-less slots are separate stations and stable per key.
	u0 := mlatc.Identity{Link: 4, IPKey: "ip:bbbb", Slot: 0}
	u1 := mlatc.Identity{Link: 5, IPKey: "ip:bbbb", Slot: 1}
	a0, err0 := m.Attach(ctx, u0)
	a1, err1 := m.Attach(ctx, u1)
	if err0 != nil || err1 != nil || a0.Station == a1.Station || !a0.Admitted() {
		t.Fatalf("ip slots %+v %+v %v %v", a0, a1, err0, err1)
	}
	if again, _ := m.Attach(ctx, u0); again.Station != a0.Station {
		t.Fatal("ip slot 0 not stable")
	}
}
