package server

import (
	"strings"
	"testing"
)

func init() {
	for _, k := range []string{"GET /api/me/stations/{sid}/minutes", "GET /api/admin/stations/{sid}/minutes",
		"GET /api/admin/federation", "GET /api/admin/mlat/modeac"} {
		specialTests[k] = "TestWave3Routes"
	}
}

// TestWave3Routes: the station minutes (owner/admin), federation and Mode
// A/C admin routes answer for the right scopes and say "off" when the
// feature is not running.
func TestWave3Routes(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	sidA := f.stations["A"].st.SID
	rec := f.as(f.alice, "GET", "/api/me/stations/"+sidA+"/minutes", "")
	must(t, rec, 200, "owner minutes")
	if !strings.Contains(rec.Body.String(), `"minutes":[]`) {
		t.Fatalf("minutes: %s", rec.Body)
	}
	must(t, f.as(f.bob, "GET", "/api/me/stations/"+sidA+"/minutes", ""), 404, "foreign minutes")
	must(t, f.as(f.admin, "GET", "/api/admin/stations/"+sidA+"/minutes", ""), 200, "admin minutes")
	rec = f.as(f.admin, "GET", "/api/admin/federation", "")
	must(t, rec, 200, "federation")
	if !strings.Contains(rec.Body.String(), `"running":false`) {
		t.Fatalf("federation: %s", rec.Body)
	}
	rec = f.as(f.admin, "GET", "/api/admin/mlat/modeac", "")
	must(t, rec, 200, "modeac")
	if !strings.Contains(rec.Body.String(), `"targets":[]`) {
		t.Fatalf("modeac: %s", rec.Body)
	}
	for _, u := range []string{"/api/admin/federation", "/api/admin/mlat/modeac", "/api/admin/stations/" + sidA + "/minutes"} {
		if rec := f.anon("GET", u, "192.0.2.9:1", ""); rec.Code != 401 && rec.Code != 403 {
			t.Fatalf("anonymous %s: %d", u, rec.Code)
		}
		if rec := f.as(f.alice, "GET", u, ""); rec.Code != 401 && rec.Code != 403 {
			t.Fatalf("owner on %s: %d", u, rec.Code)
		}
	}
}
