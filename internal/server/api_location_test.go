package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"airview/internal/registry"
	"airview/internal/selfupdate"
)

func init() {
	for _, k := range []string{"GET /api/me/stations/{sid}/notices", "GET /api/me/notices", "GET /api/admin/mlat/surveys"} {
		specialTests[k] = "TestLocationRoutes"
	}
	for _, k := range []string{"GET /api/update", "POST /api/update/check", "POST /api/update/install"} {
		specialTests[k] = "TestUpdateRoutes"
	}
}

// TestLocationNotice: every location audit action becomes the right
// notice; other actions and position-source changes without the survey
// are not notices.
func TestLocationNotice(t *testing.T) {
	at := time.Unix(1_763_000_000, 0)
	cases := []struct {
		action, detail string
		ok             bool
		kind, level    string
		text           string
	}{
		{"station.survey", `{"reason":"unlocated","state":"done","valid":true,"stable":true,"suspect":false,"sigmaHM":4,"autoUse":true,"altRef":""}`, true, "survey", "ok", "±4 m. It is precise enough to be used for MLAT automatically"},
		{"station.survey", `{"state":"unstable","valid":true,"stable":false,"sigmaHM":350}`, true, "survey", "info", "without a stable result (±350 m)"},
		{"station.survey", `{"state":"done","valid":true,"stable":true,"suspect":true,"sigmaHM":4}`, true, "survey", "warn", "consistency checks"},
		{"station.survey", `{"state":"nodata","valid":false,"observations":3}`, true, "survey", "info", "without enough data"},
		{"station.survey", `{"state":"done","valid":true,"stable":true,"sigmaHM":12,"altRef":"msl"}`, true, "survey", "ok", "above mean sea level"},
		{"station.survey.declared", `{"suspect":true,"discrepancyM":312}`, true, "declared", "warn", "312 m from where the network measures"},
		{"station.survey.declared", `{"suspect":false,"discrepancyM":12}`, true, "declared", "ok", "agrees"},
		{"station.survey.start", `{"reason":"admin"}`, true, "survey", "info", "network operator started"},
		{"station.survey.start", `{"reason":"owner"}`, true, "survey", "info", "You asked"},
		{"station.mlat.position", `{"from":"none","to":"survey","autoSurvey":true,"reason":"survey","sigmaM":6}`, true, "mlat", "ok", "MLAT now uses the network's survey position"},
		{"station.mlat.position", `{"from":"survey","to":"declared","autoSurvey":false,"reason":"override"}`, true, "mlat", "info", "it uses your declared position"},
		{"station.mlat.position", `{"from":"declared","to":"gnss"}`, false, "", "", ""},
		{"station.mlat.survey", `{"reject":true}`, true, "owner", "info", "not used for MLAT (your choice)"},
		{"station.location.survey", `{"located":true,"source":"survey"}`, true, "located", "ok", "located this station"},
		{"station.location.survey", `{"located":false,"source":"survey"}`, true, "located", "info", "no longer used"},
		{"station.location.confirm", `{"method":"survey"}`, true, "owner", "ok", "You confirmed"},
		{"station.reveal", `{}`, false, "", "", ""},
		{"station.rename", `{}`, false, "", "", ""},
	}
	for _, c := range cases {
		n, ok := locationNotice(registry.AuditEntry{T: at, Action: c.action, Detail: c.detail})
		if ok != c.ok {
			t.Errorf("%s %s: ok %v", c.action, c.detail, ok)
			continue
		}
		if !ok {
			continue
		}
		if n.Kind != c.kind || n.Level != c.level || !strings.Contains(n.Text, c.text) || n.T != at.UnixMilli() {
			t.Errorf("%s %s: %+v, want kind %s level %s text ~%q", c.action, c.detail, n, c.kind, c.level, c.text)
		}
	}
	// Garbage details do not crash and still give a notice.
	if n, ok := locationNotice(registry.AuditEntry{Action: "station.survey.declared", Detail: "{not json"}); !ok || !strings.Contains(n.Text, "agrees") {
		t.Errorf("garbage detail: %+v %v", n, ok)
	}
}

// TestLocationRoutes: owner notices (own stations only, no coordinates)
// and the admin survey console (admin only, no coordinates).
func TestLocationRoutes(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	ctx := context.Background()
	sidA := f.stations["A"].st.SID
	sidB := f.stations["B"].st.SID
	_ = f.reg.Audit(ctx, registry.SystemActor, "station.survey", sidA, `{"reason":"check","state":"done","valid":true,"stable":true,"suspect":false,"sigmaHM":5,"autoUse":false,"altRef":""}`)
	_ = f.reg.Audit(ctx, registry.SystemActor, "station.survey.declared", sidA, `{"suspect":true,"discrepancyM":250}`)
	_ = f.reg.Audit(ctx, registry.SystemActor, "station.reveal", sidA, `{}`)
	_ = f.reg.Audit(ctx, registry.SystemActor, "station.survey.declared", sidB, `{"suspect":true,"discrepancyM":999}`)

	rec := f.as(f.alice, "GET", "/api/me/stations/"+sidA+"/notices", "")
	must(t, rec, 200, "owner notices")
	var r struct {
		Notices []noticeDTO `json:"notices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Notices) != 2 || r.Notices[0].Kind != "declared" || r.Notices[1].Kind != "survey" {
		t.Fatalf("notices (newest first, reveal left out): %+v", r.Notices)
	}
	must(t, f.as(f.bob, "GET", "/api/me/stations/"+sidA+"/notices", ""), 404, "foreign notices")
	if rec := f.anon("GET", "/api/me/stations/"+sidA+"/notices", "192.0.2.9:1", ""); rec.Code != 401 && rec.Code != 403 && rec.Code != 404 {
		t.Fatalf("anonymous notices: %d", rec.Code)
	}

	rec = f.as(f.alice, "GET", "/api/me/notices", "")
	must(t, rec, 200, "all own notices")
	body := rec.Body.String()
	if !strings.Contains(body, "Alice Home") || strings.Contains(body, "999 m") || strings.Contains(body, sidB) {
		t.Fatalf("own notices leak or miss: %s", body)
	}

	if rec := f.as(f.alice, "GET", "/api/admin/mlat/surveys", ""); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("owner on the admin console: %d", rec.Code)
	}
	rec = f.as(f.admin, "GET", "/api/admin/mlat/surveys", "")
	must(t, rec, 200, "admin console")
	var a struct {
		Enabled  bool           `json:"enabled"`
		Stations []surveyRowDTO `json:"stations"`
		Quality  locQualityDTO  `json:"quality"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.Enabled || len(a.Stations) != 4 || a.Quality.Stations != 4 || a.Quality.ByStatus["declared"] != 4 {
		t.Fatalf("console: %+v", a)
	}
	for _, k := range []string{`"lat"`, `"lon"`, "-41.28", "174.77"} {
		if strings.Contains(rec.Body.String(), k) {
			t.Fatalf("the console carries coordinates (%s): %s", k, rec.Body)
		}
	}
}

// fakeUpdater is an UpdateController for TestUpdateRoutes.
type fakeUpdater struct {
	st         UpdateState
	checkErr   error
	installErr error
	installs   int
}

func (f *fakeUpdater) UpdateState() UpdateState { return f.st }
func (f *fakeUpdater) CheckUpdate(context.Context) (UpdateState, error) {
	if f.checkErr == nil {
		f.st.Available = "9.9.9"
	}
	return f.st, f.checkErr
}
func (f *fakeUpdater) StartInstall() (UpdateState, error) {
	f.installs++
	if f.installErr == nil {
		f.st.Busy = true
	}
	return f.st, f.installErr
}

// TestUpdateRoutes: state, check and install through the controller;
// without one the page learns that there is no self-update.
func TestUpdateRoutes(t *testing.T) {
	r := newRig(t, rigOptions{})
	rec := r.get("/api/update")
	must(t, rec, 200, "state without a controller")
	if !strings.Contains(rec.Body.String(), `"supported":false`) {
		t.Fatalf("no controller: %s", rec.Body)
	}
	must(t, r.do("POST", "/api/update/check", loopbackAddr, ""), 409, "check without a controller")

	fu := &fakeUpdater{st: UpdateState{Status: selfupdate.Status{Product: "airview", Version: "1.0.0", Channel: "stable"}, Configured: true, CanInstall: true}}
	r.srv.opts.Updater = fu
	rec = r.get("/api/update")
	must(t, rec, 200, "state")
	if !strings.Contains(rec.Body.String(), `"supported":true`) || !strings.Contains(rec.Body.String(), `"version":"1.0.0"`) {
		t.Fatalf("state: %s", rec.Body)
	}
	rec = r.do("POST", "/api/update/check", loopbackAddr, "")
	must(t, rec, 200, "check")
	if !strings.Contains(rec.Body.String(), `"available":"9.9.9"`) {
		t.Fatalf("check: %s", rec.Body)
	}
	fu.checkErr = errors.New("manifest: HTTP 404")
	rec = r.do("POST", "/api/update/check", loopbackAddr, "")
	must(t, rec, 200, "failed check is part of the state")
	if !strings.Contains(rec.Body.String(), `"actionError":"manifest: HTTP 404"`) {
		t.Fatalf("check error: %s", rec.Body)
	}
	fu.checkErr = selfupdate.ErrNotConfigured
	must(t, r.do("POST", "/api/update/check", loopbackAddr, ""), 409, "not configured")

	rec = r.do("POST", "/api/update/install", loopbackAddr, "")
	must(t, rec, 202, "install")
	if !strings.Contains(rec.Body.String(), `"busy":true`) || fu.installs != 1 {
		t.Fatalf("install: %s", rec.Body)
	}
	fu.installErr = errors.Join(ErrUpdateNotPossible, errors.New("desktop window"))
	must(t, r.do("POST", "/api/update/install", loopbackAddr, ""), 409, "install not possible")
	fu.installErr = ErrUpToDate
	must(t, r.do("POST", "/api/update/install", loopbackAddr, ""), 409, "up to date")

	// Admin only: with an admin password a LAN client must log in.
	r.do("PUT", "/api/config", loopbackAddr, `{"http":{"adminPasswordHash":"plain:pw"}}`)
	if rec := r.do("POST", "/api/update/install", lanAddr, ""); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("anonymous LAN install: %d", rec.Code)
	}
	if rec := r.do("GET", "/api/update", lanAddr, ""); rec.Code != 401 && rec.Code != 403 {
		t.Logf("GET /api/update from the LAN without login: %d (standalone viewers may read admin GETs)", rec.Code)
	}
}
