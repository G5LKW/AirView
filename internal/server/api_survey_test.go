package server

import (
	"strings"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/survey"
)

func init() {
	for _, k := range []string{"GET /api/me/stations/{sid}/survey", "POST /api/me/stations/{sid}/survey",
		"GET /api/admin/stations/{sid}/survey", "POST /api/admin/stations/{sid}/survey",
		"DELETE /api/admin/stations/{sid}/survey", "GET /api/admin/mlat/survey", "GET /api/me/stations/{sid}/locate"} {
		specialTests[k] = "TestSurveyRoutes"
	}
}

// TestSurveyRoutes: owner/admin access to the survey routes; without a
// running surveyor (the fixture has no MLAT engine) status says so and
// actions are 404.
func TestSurveyRoutes(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	sidA := f.stations["A"].st.SID
	rec := f.as(f.alice, "GET", "/api/me/stations/"+sidA+"/survey", "")
	must(t, rec, 200, "owner status")
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("status: %s", rec.Body)
	}
	must(t, f.as(f.bob, "GET", "/api/me/stations/"+sidA+"/survey", ""), 404, "foreign status")
	must(t, f.as(f.bob, "GET", "/api/me/stations/"+sidA+"/locate", ""), 404, "foreign locate")
	if rec := f.as(f.alice, "GET", "/api/me/stations/"+sidA+"/locate", ""); rec.Code != 200 && rec.Code != 404 {
		t.Fatalf("owner locate: %d %s", rec.Code, rec.Body)
	}
	must(t, f.as(f.alice, "POST", "/api/me/stations/"+sidA+"/survey", ""), 404, "no surveyor")
	if rec := f.as(f.alice, "GET", "/api/admin/stations/"+sidA+"/survey", ""); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("owner on admin route: %d", rec.Code)
	}
	must(t, f.as(f.admin, "GET", "/api/admin/stations/"+sidA+"/survey", ""), 200, "admin status")
	must(t, f.as(f.admin, "POST", "/api/admin/stations/"+sidA+"/survey", ""), 404, "admin start")
	must(t, f.as(f.admin, "DELETE", "/api/admin/stations/"+sidA+"/survey", ""), 404, "admin cancel")
	rec = f.as(f.admin, "GET", "/api/admin/mlat/survey", "")
	must(t, rec, 200, "admin stats")
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("stats: %s", rec.Body)
	}
	if rec := f.anon("GET", "/api/admin/mlat/survey", "192.0.2.9:1", ""); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("anonymous stats: %d", rec.Code)
	}
}

// TestSurveyResultTO: the DTO carries the solution only when valid, and
// non-finite numbers encode as null.
func TestSurveyResultTO(t *testing.T) {
	now := time.Unix(1_763_000_000, 0)
	r := survey.Result{Job: 3, Reason: survey.ReasonOwner, State: survey.StateNoData, Started: now, Finished: now}
	o := surveyResultTO(&r)
	if o.Lat != nil || o.Valid {
		t.Fatalf("invalid result carries a position: %+v", o)
	}
	b, err := marshal(o)
	if err != nil || !strings.Contains(string(b), `"sigmaHM":null`) {
		t.Fatalf("%s %v", b, err)
	}
	r.Valid, r.Stable = true, true
	r.Solution.LLH = geo.LLH{Lat: 52.1234567, Lon: 0.7654321, H: 55.55}
	r.Solution.SigmaHM, r.AltRef.Chosen = 4.26, "msl"
	o = surveyResultTO(&r)
	if o.Lat == nil || *o.Lat != 52.123457 || *o.Alt != 55.6 || float64(o.SigmaHM) != 4.3 || o.AltRef != "msl" {
		t.Fatalf("valid result: %+v", o)
	}
}
