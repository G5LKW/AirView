package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airview/internal/engine"
	"airview/internal/model"
)

// TestMLATRoutes covers the MLAT views: without an engine (the net fixture
// fakes aggregator mode on a demo engine) every route answers
// running:false; with a real aggregator engine the handlers return the
// engine's reports, owners never see peer SIDs.
func TestMLATRoutes(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	sidA := f.stations["A"].st.SID
	for _, u := range []string{"/api/admin/mlat", "/api/admin/mlat/quality", "/api/admin/mlat/sync",
		"/api/admin/mlat/crlb", "/api/admin/mlat/history", "/api/admin/mlat/stations", "/api/admin/stations/" + sidA + "/mlat"} {
		rec := f.as(f.admin, "GET", u, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"running":false`) {
			t.Errorf("admin %s: %d %.200s", u, rec.Code, rec.Body.String())
		}
	}
	rec := f.as(f.alice, "GET", "/api/me/stations/"+sidA+"/mlat", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"running":false`) {
		t.Errorf("owner: %d %.200s", rec.Code, rec.Body.String())
	}
	if rec := f.as(f.alice, "GET", "/api/admin/mlat", ""); rec.Code != http.StatusForbidden {
		t.Errorf("owner on admin route: %d", rec.Code)
	}
	pub := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(pub, httptest.NewRequest("GET", "/api/public/mlat", nil))
	if pub.Code != http.StatusOK || !strings.Contains(pub.Body.String(), `"quality":null`) {
		t.Errorf("public: %d %.200s", pub.Code, pub.Body.String())
	}

	// A real aggregator engine with the MLAT engine running.
	if testing.Short() {
		return
	}
	e := engine.New(t.TempDir(), engine.Options{Offline: true, Logger: quietLogger(), ListenHost: "127.0.0.1",
		ConfigSets: []string{"mode=aggregator", "ntp.enabled=false", "aggregator.beastPort=0", "aggregator.avfPort=0",
			"aggregator.mlatPort=0", "aggregator.sbsInLocalPort=0"}})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	if e.MLAT() == nil {
		t.Fatal("no MLAT engine")
	}
	s := New(e, Options{})
	s.SetNetwork(Network{Registry: e.Registry()})
	host, _ := e.Registry().Station(model.HostStation)
	req := func(u string) *http.Request {
		r := httptest.NewRequest("GET", u, nil)
		return r.WithContext(context.WithValue(r.Context(), ctxStationKey, host))
	}
	for name, h := range map[string]func(*Server, *http.Request) (any, error){
		"stats": apiAdminMLAT, "quality": apiAdminMLATQuality, "sync": apiAdminMLATSync, "crlb": apiAdminMLATCRLB,
		"history": apiAdminMLATHistory, "stations": apiAdminMLATStations,
		"admin station": apiAdminStationMLAT, "owner station": apiMeStationMLAT, "public": apiPublicMLAT,
	} {
		v, err := h(s, req("/x?window=30&cells=1"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, err := marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal %v", name, err)
		}
		if strings.Contains(string(b), `"running":false`) {
			t.Errorf("%s: engine not seen: %s", name, b)
		}
		if name == "owner station" {
			var d mlatStationDTO
			if err := json.Unmarshal(b, &d); err != nil || !d.Running || d.SID != host.SID {
				t.Errorf("owner station %s %v", b, err)
			}
		}
	}
	if _, err := apiAdminMLATQuality(s, req("/x?window=0")); err == nil {
		t.Error("window 0 accepted")
	}
}
