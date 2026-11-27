package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func openAt(t testing.TB, path string, clk *fakeClock, pol Policy) *Registry {
	t.Helper()
	r, err := Open(path, Options{Policy: pol, Now: clk.now, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func openTest(t testing.TB, admission string) (*Registry, *fakeClock, string) {
	t.Helper()
	clk := newClock()
	path := filepath.Join(t.TempDir(), FileName)
	r := openAt(t, path, clk, Policy{Admission: admission})
	t.Cleanup(func() { _ = r.Close() })
	return r, clk, path
}

// feederKey returns a deterministic Ed25519 public key for test feeder n.
func feederKey(n int) []byte {
	seed := sha256.Sum256(binary.BigEndian.AppendUint32([]byte("feeder"), uint32(n)))
	return ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
}

func admitAVF(t testing.TB, r *Registry, n, rx int, token string) AdmitResult {
	t.Helper()
	res, err := r.Admit(context.Background(), AdmitRequest{Kind: model.StationKindAVF, FeederPub: feederKey(n), RxNo: rx, EnrolToken: token})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func mustApproved(t testing.TB, res AdmitResult) Station {
	t.Helper()
	if res.Status != StatusApproved {
		t.Fatalf("status %s code %s (%s), want approved", res.Status, res.Code, res.Reason)
	}
	return res.Station
}

func locate(t testing.TB, r *Registry, idx model.StationIdx, lat, lon float64) Station {
	t.Helper()
	st, err := r.SetEffective(context.Background(), idx, model.StationGeo{Lat: lat, Lon: lon, HAE: 50, SigmaM: 5, Source: model.LocSrcManual}, true, model.LocDeclared)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestOpenSchemaAndHostIdx(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	if st.Idx != 2 {
		t.Errorf("first ordinary station idx = %d, want 2 (1 is reserved for the host)", st.Idx)
	}
	if !model.ValidID(st.SID) {
		t.Errorf("SID %q", st.SID)
	}
	h, err := r.EnsureHost(ctx, "Host", 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.Idx != model.HostStation || h.Kind != model.StationKindHost || h.Admission != model.AdmApproved {
		t.Errorf("host %+v", h)
	}
	h2, err := r.EnsureHost(ctx, "Host renamed", 7)
	if err != nil || h2.Idx != 1 || h2.SID != h.SID || h2.Name != "Host renamed" || h2.OwnerID != 7 {
		t.Errorf("EnsureHost again: %+v %v", h2, err)
	}
	if err := r.DeleteStation(ctx, SystemActor, model.HostStation, "x"); err == nil {
		t.Error("host station deleted")
	}
	var av, jm string
	_ = r.wdb.QueryRow(`PRAGMA auto_vacuum`).Scan(&av)
	_ = r.wdb.QueryRow(`PRAGMA journal_mode`).Scan(&jm)
	if av != "2" || jm != "wal" {
		t.Errorf("auto_vacuum %s journal_mode %s, want 2 (incremental) and wal", av, jm)
	}
	if _, err := r.RunRetention(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPRFStableAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	r := openAt(t, path, clk, Policy{Admission: model.AdmissionOpen})
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	locate(t, r, st.Idx, 51.4772, -0.4613)
	st, err := r.SetPrivacy(ctx, st.Idx, model.PinRegion, true, true)
	if err != nil {
		t.Fatal(err)
	}
	p1, ok := r.PublicPoint(st.Idx)
	if !ok || p1.Effective != model.PinRegion {
		t.Fatalf("public point %+v %v", p1, ok)
	}
	// Inside the 0.5° cell, not the exact position.
	if math.Floor(p1.Lat/0.5) != math.Floor(51.4772/0.5) || math.Floor(p1.Lon/0.5) != math.Floor(-0.4613/0.5) {
		t.Errorf("point %v,%v outside the station's region cell", p1.Lat, p1.Lon)
	}
	if math.Abs(p1.Lat-51.4772) < 1e-9 && math.Abs(p1.Lon+0.4613) < 1e-9 {
		t.Error("public point equals the exact position")
	}
	key1 := append([]byte(nil), r.keys.server...)
	tag1 := r.PeerTag("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", clk.now())
	ipk1 := r.UnidentifiedKey(mustAddr("192.0.2.7"), 0)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2 := openAt(t, path, clk, Policy{Admission: model.AdmissionOpen})
	defer r2.Close()
	if string(r2.keys.server) != string(key1) {
		t.Fatal("server key changed across restart")
	}
	p2, ok := r2.PublicPoint(st.Idx)
	if !ok || p2 != p1 {
		t.Fatalf("stored point changed across restart: %+v vs %+v", p2, p1)
	}
	// Recomputing from the key gives the same value (PRF, not random).
	la, lo := cellIndex(51.4772, -0.4613, 0.5)
	if lat, lon := r2.pinPoint(st.SID, model.PinRegion, la, lo); lat != p1.Lat || lon != p1.Lon {
		t.Errorf("PRF recomputation differs: %v,%v vs %v,%v", lat, lon, p1.Lat, p1.Lon)
	}
	if r2.PeerTag("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", clk.now()) != tag1 {
		t.Error("peer tag changed across restart")
	}
	if r2.UnidentifiedKey(mustAddr("::ffff:192.0.2.7"), 0) != ipk1 {
		t.Error("unidentified key changed across restart / IPv4-mapped form")
	}
	// Moving inside the same cell keeps the point (§8.3 step 3).
	locate(t, r2, st.Idx, 51.30, -0.30)
	if p3, _ := r2.PublicPoint(st.Idx); p3 != p1 {
		t.Errorf("point changed for a move inside the cell: %+v", p3)
	}
	// Moving to another cell recomputes it.
	locate(t, r2, st.Idx, 52.6, 1.2)
	p4, _ := r2.PublicPoint(st.Idx)
	if p4 == p1 || math.Floor(p4.Lat/0.5) != math.Floor(52.6/0.5) {
		t.Errorf("point after moving cells: %+v", p4)
	}

	// Another database (another server key) gives a different point.
	r3 := openAt(t, filepath.Join(t.TempDir(), FileName), clk, Policy{})
	defer r3.Close()
	if lat, lon := r3.pinPoint(st.SID, model.PinRegion, la, lo); lat == p1.Lat && lon == p1.Lon {
		t.Error("PRF does not depend on the server key")
	}
}

func TestPublicPointRuralGuard(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	a := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	locate(t, r, a.Idx, 51.21, -0.71)
	if _, ok := r.PublicPoint(a.Idx); ok {
		t.Error("hidden pin has a public point")
	}
	if _, err := r.SetPrivacy(ctx, a.Idx, model.PinCell01, false, false); err != nil {
		t.Fatal(err)
	}
	p, ok := r.PublicPoint(a.Idx)
	if !ok || p.Precision != model.PinCell01 || p.Effective != model.PinRegion {
		t.Fatalf("lone cell01 station must be shown as region: %+v", p)
	}
	// Hidden stations never count toward the guard (PRIV-A3: the pin's
	// precision would otherwise reveal them).
	h1 := mustApproved(t, admitAVF(t, r, 4, 0, ""))
	h2 := mustApproved(t, admitAVF(t, r, 5, 0, ""))
	locate(t, r, h1.Idx, 51.3, -0.3)
	locate(t, r, h2.Idx, 51.4, -0.4)
	if p, _ := r.PublicPoint(a.Idx); p.Effective != model.PinRegion {
		t.Fatalf("hidden stations lifted the rural guard: %+v", p)
	}
	// Two more approved, located stations with public pins in the same 1°
	// cell lift the guard.
	b := mustApproved(t, admitAVF(t, r, 2, 0, ""))
	c := mustApproved(t, admitAVF(t, r, 3, 0, ""))
	for _, x := range []model.StationIdx{b.Idx, c.Idx} {
		if _, err := r.SetPrivacy(ctx, x, model.PinRegion, false, false); err != nil {
			t.Fatal(err)
		}
	}
	locate(t, r, b.Idx, 51.9, -0.1)
	if p, _ := r.PublicPoint(a.Idx); p.Effective != model.PinRegion {
		t.Errorf("2 stations: %+v", p)
	}
	locate(t, r, c.Idx, 51.5, -0.5)
	p, _ = r.PublicPoint(a.Idx)
	if p.Effective != model.PinCell01 {
		t.Fatalf("3 stations: neighbour not refreshed: %+v", p)
	}
	if math.Floor(p.Lat/0.1+1e-9) != math.Floor(51.21/0.1+1e-9) || math.Floor(p.Lon/0.1) != math.Floor(-0.71/0.1) {
		t.Errorf("cell01 point %v,%v not in the 0.1° cell", p.Lat, p.Lon)
	}
	// Revoking one of them drops the cell below the guard again.
	if _, err := r.SetAdmission(ctx, SystemActor, c.Idx, model.AdmRevoked); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.PublicPoint(a.Idx); p.Effective != model.PinRegion {
		t.Errorf("after revoke: %+v", p)
	}
	// Pending stations never have a public point.
	if _, err := r.SetAdmission(ctx, SystemActor, a.Idx, model.AdmPending); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.PublicPoint(a.Idx); ok {
		t.Error("pending station has a public point")
	}
}

func TestResolver(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	res := r.Resolver()
	m, ok := res.Info(st.Idx)
	if !ok || m.SID != st.SID || m.Located || m.Admission != model.AdmApproved {
		t.Fatalf("info %+v %v", m, ok)
	}
	if _, ok := res.Geo(st.Idx); ok {
		t.Error("geo of unlocated station")
	}
	locate(t, r, st.Idx, 10, 20)
	g, ok := res.Geo(st.Idx)
	if !ok || g.Lat != 10 || g.Lon != 20 || g.HAE != 50 {
		t.Errorf("geo %+v", g)
	}
	if m, _ := res.Info(st.Idx); !m.Located {
		t.Error("Located not set")
	}
	if _, ok := res.Info(9999); ok {
		t.Error("unknown idx resolved")
	}
	if _, ok := res.Info(model.StationIdx(5000 << chunkBits)); ok {
		t.Error("idx beyond the chunk directory resolved")
	}
	if got, ok := r.BySID(st.SID); !ok || got.Idx != st.Idx {
		t.Error("BySID")
	}
	if got, ok := r.ByFeeder(st.FeederID, 0); !ok || got.Idx != st.Idx {
		t.Error("ByFeeder")
	}
}

func TestValidation(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	if _, err := r.SetPrivacy(ctx, st.Idx, "exact", false, false); err == nil {
		t.Error("invalid pin accepted")
	}
	if _, err := r.SetDeclared(ctx, st.Idx, Position{Lat: 91, Lon: 0, Valid: true}, model.AltRefMSL); err == nil {
		t.Error("lat 91 accepted")
	}
	if _, err := r.SetDeclared(ctx, st.Idx, Position{Lat: 50, Lon: 0, Valid: true}, "feet"); err == nil {
		t.Error("bad altRef accepted")
	}
	if _, err := r.SetName(ctx, st.Idx, string(make([]rune, 80))); err == nil {
		t.Error("long name accepted")
	}
	got, err := r.SetDeclared(ctx, st.Idx, Position{Lat: 50, Lon: 1, Alt: 30, Valid: true}, model.AltRefMSL)
	if err != nil || !got.Declared.Valid || got.DeclaredAltRef != model.AltRefMSL || got.Located {
		t.Errorf("declared %+v %v", got, err)
	}
	if _, err := r.SetName(ctx, 4242, "x"); err != ErrStationNotFound {
		t.Errorf("unknown station: %v", err)
	}
}

func TestSeenFlush(t *testing.T) {
	r, clk, path := openTest(t, model.AdmissionOpen)
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	clk.add(time.Hour)
	r.Seen(st.Idx, clk.now())
	if got, _ := r.Station(st.Idx); !got.LastSeen.Equal(clk.now()) {
		t.Errorf("pending last seen not merged: %v", got.LastSeen)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := openAt(t, path, clk, Policy{})
	defer r2.Close()
	if got, _ := r2.Station(st.Idx); !got.LastSeen.Equal(clk.now()) {
		t.Errorf("last seen not flushed on Close: %v", got.LastSeen)
	}
}

func TestCloneGuard(t *testing.T) {
	g := NewCloneGuard()
	a, b := mustAddr("192.0.2.1"), mustAddr("198.51.100.2")
	d, t1 := g.Acquire("f1", a)
	if d != CloneOK {
		t.Fatal(d)
	}
	if d, _ := g.Acquire("f1", b); d != CloneRejected {
		t.Errorf("other IP: %v", d)
	}
	d, t2 := g.Acquire("f1", a)
	if d != CloneReplace {
		t.Errorf("same IP: %v", d)
	}
	g.Release("f1", t1) // the replaced session's release must not free the key
	if d, _ := g.Acquire("f1", b); d != CloneRejected {
		t.Error("replaced session released the live one")
	}
	g.Release("f1", t2)
	if d, _ := g.Acquire("f1", b); d != CloneOK || g.Live() != 1 {
		t.Error("released key still live")
	}
}
