package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/registry"
)

// fix:surface regression tests (PRIV-A1, PRIV-A5, PRIV-A6).

// closePublicMap switches aggregator.publicMap off.
func (f *netFixture) closePublicMap(t *testing.T) {
	t.Helper()
	cfg := f.eng.EffectiveConfig()
	cfg.Aggregator.PublicMap = false
	if _, _, err := f.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// PRIV-A1: publicMap=false closes the viewer routes and the live streams,
// not only /api/public/aircraft.
func TestPublicMapOffClosesViewerRoutes(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.closePublicMap(t)
	for _, u := range []string{"/api/aircraft", "/api/aircraft/3C6444", "/api/trails?minutes=5", "/api/public/trails", "/api/public/aircraft/3C6444"} {
		if c := f.anon("GET", u, "192.0.2.51:4000", "").Code; c != http.StatusForbidden {
			t.Errorf("%s: anonymous %d with publicMap off, want 403", u, c)
		}
		// Owners and admins keep their own views.
		if c := f.as(f.admin, "GET", u, "").Code; c == http.StatusForbidden {
			t.Errorf("%s: admin refused", u)
		}
	}
	for _, u := range []string{"/api/events?delta=1", "/api/events", "/api/public/events"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req := httptest.NewRequest("GET", u, nil).WithContext(ctx)
		req.RemoteAddr = "192.0.2.52:4000"
		req.Host = "192.168.1.10:8080"
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		cancel()
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: anonymous stream %d with publicMap off, want 403", u, rec.Code)
		}
	}
}

// PRIV-A1: streams opened while the map was public are closed once the
// operator switches it off.
func TestPublicMapOffDropsRunningStreams(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	h := f.srv.live
	pub := &liveClient{ch: make(chan []byte, 1), kick: make(chan struct{}), viewer: model.Viewer{Scope: model.ScopePublic}, forcePublic: true}
	adm := &liveClient{ch: make(chan []byte, 1), kick: make(chan struct{}), viewer: model.Viewer{Scope: model.ScopeAdmin}}
	h.mu.Lock()
	h.clients[pub] = struct{}{}
	h.clients[adm] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, pub)
		delete(h.clients, adm)
		h.mu.Unlock()
	}()
	h.dropPublicIfClosed()
	select {
	case <-pub.kick:
		t.Fatal("public stream dropped while the map is public")
	default:
	}
	f.closePublicMap(t)
	h.dropPublicIfClosed()
	select {
	case <-pub.kick:
	default:
		t.Error("public stream kept after publicMap was switched off")
	}
	select {
	case <-adm.kick:
		t.Error("admin stream dropped")
	default:
	}
}

// PRIV-A6: a low Mode A/C target (no Mode S address, no anchor list) is
// coarsened by the island guard instead of always passing.
func TestIslandGuardModeAC(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	cfg := f.eng.EffectiveConfig()
	cfg.Aggregator.IslandGuard = true
	if _, _, err := f.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	g := f.srv.islandGuard()
	if g == nil {
		t.Fatal("guard off")
	}
	lat, lon := 51.2345, 7.6543
	low, high := 900.0, 30000.0
	ac := model.Aircraft{ICAO: "#00a1b2", AddrType: "modeac", Lat: &lat, Lon: &lon, AltBaro: &low}
	if !g(&ac) {
		t.Error("low Mode A/C target not coarsened")
	}
	ac.AltBaro = &high
	if g(&ac) {
		t.Error("high Mode A/C target coarsened")
	}
}

// PRIV-A7: the order of anonymous station entries changes from day to day.
func TestAnonOrderKeyPerDay(t *testing.T) {
	d1 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	d1b := d1.Add(5 * time.Hour)
	d2 := d1.Add(24 * time.Hour)
	if anonOrderKey("abc", d1) != anonOrderKey("abc", d1b) {
		t.Error("key changes within a day")
	}
	if anonOrderKey("abc", d1) == anonOrderKey("abc", d2) {
		t.Error("key stable across days")
	}
	if anonOrderKey("abc", d1) == anonOrderKey("abd", d1) {
		t.Error("keys of two stations collide")
	}
}

// UX-08: audit entries written with an actor ID only show the account name.
func TestAuditActorName(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	if err := f.reg.Audit(context.Background(), registry.Actor{ID: f.bob.id}, "station.claim", "", ""); err != nil {
		t.Fatal(err)
	}
	rec := f.as(f.admin, "GET", "/api/admin/audit?action=station.claim", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"actor":"bob"`) {
		t.Fatalf("audit actor: %d %s", rec.Code, rec.Body.String())
	}
}
