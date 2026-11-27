package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"airview/internal/registry"
)

func init() {
	for k, v := range map[string]string{
		"POST /api/me/stations/{sid}/location/pending/confirm": "TestHardenRoutes",
		"PUT /api/me/stations/{sid}/mlat/survey":               "TestHardenRoutes",
		"POST /api/admin/stations/{sid}/timing":                "TestHardenRoutes",
		"GET /api/admin/mlat/quarantine":                       "TestHardenRoutes",
	} {
		specialTests[k] = v
	}
}

// TestHardenRoutes: held feeder positions, the owner's survey choice, the
// admin timing override and the quarantine list (H-mlatc, H-track).
func TestHardenRoutes(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	ctx := context.Background()
	a := f.stations["A"].st
	sidA := a.SID

	// No pending position yet.
	must(t, f.as(f.alice, "POST", "/api/me/stations/"+sidA+"/location/pending/confirm", ""), 404, "nothing pending")
	// The feeder announces its position, then a 5 km move: held.
	if _, _, err := f.reg.FeederPosition(ctx, a.Idx, registry.FeederPos{Lat: -41.28713, Lon: 174.77617, Alt: 80, AltRef: "msl", SetEffective: true}); err != nil {
		t.Fatal(err)
	}
	if _, d, err := f.reg.FeederPosition(ctx, a.Idx, registry.FeederPos{Lat: -41.33, Lon: 174.77617, Alt: 80, AltRef: "msl", SetEffective: true}); err != nil || d != registry.PosHeld {
		t.Fatalf("move: %q %v", d, err)
	}
	var det stationDetail
	rec := f.as(f.alice, "GET", "/api/me/stations/"+sidA, "")
	must(t, rec, 200, "detail")
	jsonOf(t, rec, &det)
	if det.PendingPosition == nil || det.PendingPosition.Lat != -41.33 || det.PendingPosition.Reason != registry.PosHeld {
		t.Fatalf("pending: %+v", det.PendingPosition)
	}
	var tim struct {
		State string `json:"state"`
	}
	b, _ := json.Marshal(det.Timing)
	if json.Unmarshal(b, &tim) != nil || tim.State == "" {
		t.Fatalf("timing: %s", b)
	}
	must(t, f.as(f.bob, "POST", "/api/me/stations/"+sidA+"/location/pending/confirm", ""), 404, "foreign confirm")
	rec = f.as(f.alice, "POST", "/api/me/stations/"+sidA+"/location/pending/confirm", "")
	must(t, rec, 200, "confirm pending")
	var dto stationDTO
	jsonOf(t, rec, &dto)
	if dto.Lat == nil || *dto.Lat != -41.33 {
		t.Fatalf("confirmed: %+v", dto)
	}

	// Survey choice: a body is required; without a running MLAT engine 404.
	must(t, f.as(f.alice, "PUT", "/api/me/stations/"+sidA+"/mlat/survey", `{}`), 400, "no reject")
	if rec := f.as(f.alice, "PUT", "/api/me/stations/"+sidA+"/mlat/survey", `{"reject":true}`); rec.Code != 404 && rec.Code != 200 {
		t.Fatalf("survey: %d %s", rec.Code, rec.Body)
	}

	// Admin timing override and the quarantine list.
	if rec := f.as(f.alice, "POST", "/api/admin/stations/"+sidA+"/timing", `{"mode":"excluded"}`); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("owner override: %d", rec.Code)
	}
	must(t, f.as(f.admin, "POST", "/api/admin/stations/"+sidA+"/timing", `{"mode":"maybe"}`), 400, "bad mode")
	rec = f.as(f.admin, "POST", "/api/admin/stations/"+sidA+"/timing", `{"mode":"excluded"}`)
	must(t, rec, 200, "exclude")
	if !strings.Contains(rec.Body.String(), `"timing":"excluded"`) {
		t.Fatalf("exclude: %s", rec.Body)
	}
	must(t, f.as(f.admin, "POST", "/api/admin/stations/"+sidA+"/timing", `{"mode":"quarantine"}`), 200, "quarantine")
	rec = f.as(f.admin, "GET", "/api/admin/mlat/quarantine", "")
	must(t, rec, 200, "quarantine list")
	if !strings.Contains(rec.Body.String(), sidA) {
		t.Fatalf("quarantine list without A: %s", rec.Body)
	}
	if rec := f.as(f.alice, "GET", "/api/admin/mlat/quarantine", ""); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("owner list: %d", rec.Code)
	}
}
