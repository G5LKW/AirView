package registry

import (
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airview/internal/ingest/mlatc"
	"airview/internal/model"
)

// TestTimingQuarantineFlags: new timed stations start quarantined, Beast
// links and stations of the NoTimingQuarantine policy do not; ProveTiming
// and the admin overrides behave and are audited.
func TestTimingQuarantineFlags(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	st := mustApproved(t, admitAVF(t, r, 1, 1, ""))
	if st.Timing() != TimingQuarantined || !st.Has(FlagTimingQuarantine) {
		t.Fatalf("new AVF station timing %q flags %b", st.Timing(), st.Flags)
	}
	b, err := r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, UUID: "11111111-2222-3333-4444-555555555555"})
	if err != nil || b.Station.Timing() != TimingTrusted {
		t.Fatalf("beast station %v %q", err, b.Station.Timing())
	}
	if q := r.Quarantined(); len(q) != 1 || q[0].Idx != st.Idx {
		t.Fatalf("quarantined list %v", q)
	}
	up, err := r.ProveTiming(ctx, st.Idx, `{"windows":9}`)
	if err != nil || up.Timing() != TimingTrusted {
		t.Fatalf("prove: %v %q", err, up.Timing())
	}
	for _, c := range []struct{ mode, want string }{
		{TimingExcluded, TimingExcluded}, {TimingTrusted, TimingTrusted}, {TimingRequarantine, TimingQuarantined},
		{TimingTrusted, TimingTrusted}, {TimingAuto, TimingQuarantined},
	} {
		up, err = r.SetTiming(ctx, Actor{ID: 1, Name: "root"}, st.Idx, c.mode)
		if err != nil || up.Timing() != c.want {
			t.Fatalf("SetTiming(%s): %v %q", c.mode, err, up.Timing())
		}
	}
	if _, err := r.SetTiming(ctx, Actor{ID: 1}, st.Idx, "bogus"); err == nil {
		t.Fatal("bogus mode accepted")
	}
	if s, ok := r.TimingState(st.Idx); !ok || s != TimingQuarantined {
		t.Fatalf("TimingState %q %v", s, ok)
	}
	log, err := r.AuditLog(ctx, AuditFilter{SID: st.SID, ActionPrefix: "station.timing"})
	if err != nil || len(log) != 6 {
		t.Fatalf("audit %d entries, err %v", len(log), err)
	}
	// existing stations are not quarantined by the upgrade; a policy can
	// switch quarantine off for new ones
	r.SetPolicy(Policy{Admission: model.AdmissionOpen, NoTimingQuarantine: true})
	if st2 := mustApproved(t, admitAVF(t, r, 2, 1, "")); st2.Timing() != TimingTrusted {
		t.Fatalf("NoTimingQuarantine: %q", st2.Timing())
	}
}

// TestFeederPositionRules covers PRIV-1: first position applied, jitter
// ignored, moderate changes rate-limited, large moves held until the
// survey confirms them, owner confirmation, release after the interval,
// and an owner-declared position never replaced by a handshake.
func TestFeederPositionRules(t *testing.T) {
	r, clk, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	st := mustApproved(t, admitAVF(t, r, 1, 1, ""))
	at := func(lat, lon float64) FeederPos {
		return FeederPos{Lat: lat, Lon: lon, Alt: 100, AltRef: model.AltRefMSL, SetEffective: true}
	}
	up, d, err := r.FeederPosition(ctx, st.Idx, at(50, 8))
	if err != nil || d != PosFirst || !up.Located || up.Effective.Lat != 50 {
		t.Fatalf("first: %v %q %+v", err, d, up.Effective)
	}
	if _, d, _ = r.FeederPosition(ctx, st.Idx, at(50.0001, 8)); d != PosIgnored { // ≈ 11 m
		t.Fatalf("jitter: %q", d)
	}
	// 300 m move inside the 24 h interval: pending, then released
	if up, d, _ = r.FeederPosition(ctx, st.Idx, at(50.0027, 8)); d != PosLimited || up.Effective.Lat != 50 {
		t.Fatalf("limited: %q %v", d, up.Effective.Lat)
	}
	if p, ok, _ := r.PendingPosition(ctx, st.Idx); !ok || p.Reason != PosLimited {
		t.Fatalf("pending %+v %v", p, ok)
	}
	clk.add(25 * time.Hour)
	if n, err := r.ReleasePending(ctx); err != nil || n != 1 {
		t.Fatalf("release: %d %v", n, err)
	}
	if up, _ = r.Station(st.Idx); up.Effective.Lat != 50.0027 {
		t.Fatalf("not released: %v", up.Effective.Lat)
	}
	// a 20 km move is held even after the interval, until the survey agrees
	clk.add(25 * time.Hour)
	if up, d, _ = r.FeederPosition(ctx, st.Idx, at(50.18, 8)); d != PosHeld || up.Effective.Lat != 50.0027 {
		t.Fatalf("held: %q %v", d, up.Effective.Lat)
	}
	if _, d, _ = r.FeederPosition(ctx, st.Idx, at(50.18, 8)); d != PosHeld { // repeated: still held
		t.Fatalf("held again: %q", d)
	}
	if _, err := r.SetEstimated(ctx, st.Idx, Position{Lat: 50.0027, Lon: 8, Alt: 150, Valid: true}, 20); err != nil {
		t.Fatal(err)
	}
	if up, _ = r.Station(st.Idx); up.Declared.Lat != 50.0027 {
		t.Fatal("a survey at the old place released the move")
	}
	if _, err := r.SetEstimated(ctx, st.Idx, Position{Lat: 50.1801, Lon: 8.0005, Alt: 150, Valid: true}, 20); err != nil {
		t.Fatal(err)
	}
	if up, _ = r.Station(st.Idx); up.Declared.Lat != 50.18 {
		t.Fatalf("survey did not release the held move: %v", up.Declared.Lat)
	}
	// another large move, confirmed by the owner
	clk.add(25 * time.Hour)
	if _, d, _ = r.FeederPosition(ctx, st.Idx, at(51, 8)); d != PosHeld {
		t.Fatalf("held 2: %q", d)
	}
	if up, ok, err := r.ConfirmPendingPosition(ctx, Actor{ID: 7, Name: "owner"}, st.Idx); err != nil || !ok || up.Declared.Lat != 51 {
		t.Fatalf("confirm: %v %v %v", err, ok, up.Declared.Lat)
	}
	// a held move announced for HoldMax is applied
	clk.add(25 * time.Hour)
	if _, d, _ = r.FeederPosition(ctx, st.Idx, at(52, 8)); d != PosHeld {
		t.Fatalf("held 3: %q", d)
	}
	clk.add(8 * 24 * time.Hour)
	if up, d, _ = r.FeederPosition(ctx, st.Idx, at(52, 8)); d != PosApplied || up.Declared.Lat != 52 {
		t.Fatalf("hold expiry: %q %v", d, up.Declared.Lat)
	}
	// going back to the current place drops a pending move
	clk.add(time.Hour)
	if _, d, _ = r.FeederPosition(ctx, st.Idx, at(52.003, 8)); d != PosLimited {
		t.Fatalf("limited 2: %q", d)
	}
	if _, d, _ = r.FeederPosition(ctx, st.Idx, at(52, 8)); d != PosIgnored {
		t.Fatalf("back: %q", d)
	}
	if _, ok, _ := r.PendingPosition(ctx, st.Idx); ok {
		t.Fatal("pending kept after going back")
	}
	if log, _ := r.AuditLog(ctx, AuditFilter{SID: st.SID, ActionPrefix: "station.position"}); len(log) < 6 {
		t.Fatalf("audit %d", len(log))
	}
	for _, e := range mustAudit(t, r, st.SID) {
		if strings.Contains(e.Detail, "50.18") || strings.Contains(e.Detail, "lat") {
			t.Fatalf("coordinates in the audit log: %s", e.Detail)
		}
	}
	if _, _, err := r.FeederPosition(ctx, st.Idx, FeederPos{Lat: 95, Lon: 8}); err == nil {
		t.Fatal("invalid latitude accepted")
	}
}

func mustAudit(t *testing.T, r *Registry, sid string) []AuditEntry {
	t.Helper()
	l, err := r.AuditLog(context.Background(), AuditFilter{SID: sid, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestMLATCHandshakePositionHeld: an mlat-client handshake moving the
// station is held; an owner-declared position is never replaced.
func TestMLATCHandshakePositionHeld(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	m := r.MLATC()
	id := mlatc.Identity{Link: 1, Kind: model.StationKindMLATC, UUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Slot: -1,
		RemoteIP: netip.MustParseAddr("192.0.2.1"), User: "x", Located: true, Lat: 50, Lon: 8, Alt: 120}
	a, err := m.Attach(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	m.Detach(id, a)
	st, _ := r.Station(a.Station)
	if !st.Declared.Valid || st.Declared.Lat != 50 || st.DeclaredAltRef != model.AltRefAmbiguous {
		t.Fatalf("declared %+v %q", st.Declared, st.DeclaredAltRef)
	}
	id.Lat, id.Link = 50.5, 2 // 55 km away
	a, _ = m.Attach(ctx, id)
	m.Detach(id, a)
	if st, _ = r.Station(a.Station); st.Declared.Lat != 50 {
		t.Fatalf("handshake move applied at once: %v", st.Declared.Lat)
	}
	if p, ok, _ := r.PendingPosition(ctx, a.Station); !ok || p.Reason != PosHeld || p.Lat != 50.5 {
		t.Fatalf("pending %+v %v", p, ok)
	}
	// the owner enters a position: handshakes never replace it
	if _, err := r.SetDeclared(ctx, a.Station, Position{Lat: 49, Lon: 8, Alt: 100, Valid: true}, model.AltRefMSL); err != nil {
		t.Fatal(err)
	}
	id.Lat, id.Link = 49.2, 3
	a, _ = m.Attach(ctx, id)
	m.Detach(id, a)
	if st, _ = r.Station(a.Station); st.Declared.Lat != 49 || st.DeclaredAltRef != model.AltRefMSL {
		t.Fatalf("owner position replaced: %+v", st.Declared)
	}
	if !st.Has(FlagTimingQuarantine) {
		t.Fatal("new mlat-client station not quarantined")
	}
}

// TestFrozenBandRefPersists: the peer-band reference survives a restart
// within its tag period and is replaced in the next one.
func TestFrozenBandRefPersists(t *testing.T) {
	clk := newClock()
	path := filepath.Join(t.TempDir(), FileName)
	r := openAt(t, path, clk, Policy{Admission: model.AdmissionOpen})
	ctx := context.Background()
	p := TagPeriod(clk.now())
	if lat, lon := r.FrozenBandRef(ctx, "s1", p, 50, 8); lat != 50 || lon != 8 {
		t.Fatal("first freeze")
	}
	if lat, _ := r.FrozenBandRef(ctx, "s1", p, 51, 9); lat != 50 {
		t.Fatal("moved within the period")
	}
	_ = r.Close()
	r = openAt(t, path, clk, Policy{Admission: model.AdmissionOpen})
	defer r.Close()
	if lat, lon := r.FrozenBandRef(ctx, "s1", p, 52, 10); lat != 50 || lon != 8 {
		t.Fatalf("restart refroze: %v %v", lat, lon)
	}
	next := TagPeriod(clk.now().Add(8 * 24 * time.Hour))
	if lat, _ := r.FrozenBandRef(ctx, "s1", next, 52, 10); lat != 52 {
		t.Fatal("next period kept the old reference")
	}
	if err := r.pruneBandRefs(ctx, next); err != nil {
		t.Fatal(err)
	}
}
