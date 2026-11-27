package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/privacy"
	"airview/internal/registry"
)

func init() {
	for k, v := range netSpecialTests {
		specialTests[k] = v
	}
}

// netSpecialTests names the tests of the NoAuto network routes (merged into
// specialTests for TestRouteTable).
var netSpecialTests = map[string]string{
	"GET /api/public/events":                         "TestGoldenPublicCrawl, TestPublicStreamPerIP",
	"GET /api/me":                                    "TestOwnerRoutes, TestGoldenOwnerCrawl",
	"GET /api/me/stations":                           "TestOwnerRoutes, TestGoldenOwnerCrawl",
	"GET /api/me/stations/{sid}":                     "TestOwnerRoutes, TestGoldenOwnerCrawl",
	"POST /api/me/stations/{sid}":                    "TestOwnerRoutes",
	"POST /api/me/stations/{sid}/unclaim":            "TestOwnerRoutes",
	"GET /api/me/stations/{sid}/stats":               "TestGoldenOwnerCrawl",
	"GET /api/me/stations/{sid}/connections":         "TestGoldenOwnerCrawl",
	"GET /api/me/stations/{sid}/mlat":                "TestGoldenOwnerCrawl, TestMLATRoutes",
	"GET /api/admin/mlat":                            "TestMLATRoutes",
	"GET /api/admin/mlat/quality":                    "TestMLATRoutes",
	"GET /api/admin/mlat/sync":                       "TestMLATRoutes",
	"GET /api/admin/mlat/crlb":                       "TestMLATRoutes, TestMLATConsoleRoutes",
	"GET /api/admin/mlat/history":                    "TestMLATConsoleRoutes, TestMLATHistoryReset",
	"GET /api/admin/mlat/stations":                   "TestMLATConsoleRoutes",
	"GET /api/admin/mlat/longterm":                   "TestMLATLongterm",
	"GET /api/me/stations/{sid}/mlat/longterm":       "TestMLATLongterm, TestGoldenOwnerCrawl",
	"GET /api/admin/stations/{sid}/mlat":             "TestMLATRoutes",
	"GET /api/me/aircraft":                           "TestGoldenOwnerCrawl, TestOwnerRoutes",
	"GET /data/me/aircraft.json":                     "TestGoldenOwnerCrawl",
	"GET /api/me/check":                              "TestFeedCheckAndClaims",
	"PUT /api/me/stations/{sid}/privacy":             "TestOwnerRoutes",
	"PUT /api/me/stations/{sid}/name":                "TestOwnerRoutes",
	"PUT /api/me/stations/{sid}/location":            "TestOwnerRoutes",
	"POST /api/me/stations/{sid}/location/confirm":   "TestOwnerRoutes",
	"PUT /api/admin/stations/{sid}":                  "TestAdminRoutes",
	"POST /api/admin/stations/{sid}/admission":       "TestAdminRoutes",
	"POST /api/admin/stations/{sid}/reveal":          "TestAdminRoutes",
	"GET /api/admin/network":                         "TestAdminRoutes",
	"POST /api/me/claim":                             "TestFeedCheckAndClaims",
	"GET /api/me/export":                             "TestGoldenOwnerCrawl",
	"DELETE /api/me/account":                         "TestOwnerRoutes",
	"GET /api/admin/stations":                        "TestAdminRoutes",
	"GET /api/admin/stations/{sid}":                  "TestAdminRoutes",
	"POST /api/admin/stations/{sid}":                 "TestAdminRoutes",
	"DELETE /api/admin/stations/{sid}":               "TestAdminRoutes",
	"POST /api/admin/feeders/{id}/revoke":            "TestAdminRoutes",
	"GET /api/admin/tokens":                          "TestAdminRoutes",
	"POST /api/admin/tokens":                         "TestAdminRoutes",
	"DELETE /api/admin/tokens/{id}":                  "TestAdminRoutes",
	"GET /api/admin/audit":                           "TestAdminRoutes",
	"POST /api/admin/retention":                      "TestAdminRoutes",
	"GET /api/admin/links":                           "TestAdminRoutes",
	"GET /api/admin/ingest/beast":                    "TestAdminRoutes",
	"POST /api/admin/ingest/beast/{link}/disconnect": "TestAdminRoutes",
	"GET /api/admin/aircraft/{icao}":                 "TestAdminRoutes",
	"GET /api/uplinks":                               "TestUplinksMount",
	"POST /api/uplinks":                              "TestUplinksMount",
}

func TestFeedCheckAndClaims(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	fx := f.publicFixture()
	// The visitor may see the last 6 characters of the UUID of a link from
	// the own address (DESIGN §11), nothing else.
	var sec []string
	for _, s := range fx.Secrets {
		if s != "ddeeff" {
			sec = append(sec, s)
		}
	}
	fx.Secrets = sec

	// Anonymous visitor from C's address (PRIV-A8): only that a link
	// exists, no details (the address may be shared, CGNAT).
	rec := f.anon("GET", "/api/me/check", "198.51.100.9:6000", "")
	must(t, rec, 200, "check C")
	scanOrFail(t, fx, "feed check", rec.Body.Bytes())
	var chk checkReply
	jsonOf(t, rec, &chk)
	if !chk.Matched || chk.Count != 1 || !chk.Limited || len(chk.Links) != 0 {
		t.Fatalf("check C anonymous: %+v", chk)
	}
	// Signed in (not the owner): claimable by UUID; no rates, no address,
	// UUID, SID or position in the answer.
	carol := f.register(t, "carol", "198.51.100.9:6001")
	rec = f.as(carol, "GET", "/api/me/check", "")
	must(t, rec, 200, "check C carol")
	scanOrFail(t, fx, "feed check", rec.Body.Bytes())
	jsonOf(t, rec, &chk)
	if !chk.Matched || len(chk.Links) != 1 || !chk.Links[0].Claimable || chk.Links[0].ClaimMethod != "uuid" ||
		chk.Links[0].IDTail != "…ddeeff" || chk.Links[0].MsgRate != 0 || chk.Links[0].Details || chk.Links[0].SID != "" ||
		chk.Links[0].UUIDMissing || chk.Links[0].Admission != "" {
		t.Fatalf("check C: %+v", chk)
	}
	// IPv6 /64 match; B is claimed. Anonymous: count only.
	jsonOf(t, f.anon("GET", "/api/me/check", "[2001:db8:aa:bb:1::5]:6000", ""), &chk)
	if chk.Count != 1 || len(chk.Links) != 0 {
		t.Fatalf("check B anonymous: %+v", chk)
	}
	// Bob sees his SID and the link's details there.
	jsonOf(t, f.as(f.bob, "GET", "/api/me/check", ""), &chk)
	if len(chk.Links) != 1 || !chk.Links[0].Yours || chk.Links[0].SID != f.stations["B"].st.SID || chk.Links[0].StationName != "Bob Hill" ||
		!chk.Links[0].Details || !chk.Links[0].Claimed || chk.Links[0].Claimable {
		t.Fatalf("check B as bob: %+v", chk)
	}
	// Two UUIDs on one address are flagged.
	f.links.mu.Lock()
	f.links.ls = append(f.links.ls, LiveLink{Proto: "mlatc", UUID: "ffffffffffffffffffffffffffffffff", IP: f.links.ls[2].IP, Since: time.Now()})
	f.links.mu.Unlock()
	jsonOf(t, f.as(carol, "GET", "/api/me/check", ""), &chk)
	if len(chk.Links) != 2 || !chk.Links[0].UUIDMismatch || !chk.Links[1].UUIDMismatch {
		t.Fatalf("mismatch: %+v", chk)
	}
	// Nobody else.
	jsonOf(t, f.anon("GET", "/api/me/check", "192.0.2.50:6000", ""), &chk)
	if chk.Matched || len(chk.Links) != 0 || chk.Count != 0 {
		t.Fatalf("stranger: %+v", chk)
	}

	// Claims need a session and the CSRF token.
	uuidC := f.stations["C"].uuid
	must(t, f.anon("POST", "/api/me/claim", "198.51.100.9:6000", `{"uuid":"`+uuidC+`"}`), 401, "anonymous claim")
	must(t, serve(f.srv.Handler(), "POST", "/api/me/claim", carol.addr, `{"uuid":"`+uuidC+`"}`, "Cookie", carol.cookie), 403, "claim without CSRF")
	// From another address the compat claim fails.
	carolAway := carol
	carolAway.addr = "192.0.2.77:6000"
	rec = f.as(carolAway, "POST", "/api/me/claim", `{"uuid":"`+uuidC+`"}`)
	must(t, rec, 400, "claim from elsewhere")
	if !strings.Contains(rec.Body.String(), "own IP") {
		t.Errorf("message: %s", rec.Body.String())
	}
	rec = f.as(carol, "POST", "/api/me/claim", `{"uuid":"`+uuidC+`"}`)
	must(t, rec, 200, "compat claim")
	if st, _ := f.reg.Station(f.stations["C"].st.Idx); st.OwnerID != carol.id {
		t.Fatalf("C owner %d", st.OwnerID)
	}
	// Native: a claim code minted over AVF.
	code, _, err := f.reg.MintClaim(context.Background(), f.stations["N"].st.Idx)
	if err != nil {
		t.Fatal(err)
	}
	must(t, f.as(carol, "POST", "/api/me/claim", `{"code":"WRONGCDE"}`), 400, "wrong code")
	rec = f.as(carol, "POST", "/api/me/claim", `{"code":"`+code+`"}`)
	must(t, rec, 200, "code claim")
	if !strings.Contains(rec.Body.String(), f.stations["N"].st.SID) {
		t.Errorf("claim reply: %s", rec.Body.String())
	}
	// Rate limit: 5 attempts per 10 minutes per address -> 429 with Retry-After.
	got429 := false
	for i := 0; i < 8; i++ {
		rec = f.as(carol, "POST", "/api/me/claim", `{"code":"BADCODE`+string(rune('A'+i))+`"}`)
		if rec.Code == http.StatusTooManyRequests {
			got429 = rec.Header().Get("Retry-After") != ""
			break
		}
	}
	if !got429 {
		t.Error("claim attempts are not rate-limited")
	}
}

func TestOwnerRoutes(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	sidA := f.stations["A"].st.SID
	var me meReply
	jsonOf(t, f.as(f.alice, "GET", "/api/me", ""), &me)
	if me.Username != "alice" || len(me.Stations) != 1 || me.Stations[0].SID != sidA || me.Stations[0].Lat == nil ||
		*me.Stations[0].Lat != -41.28713 || me.Stations[0].PublicPoint != nil || me.Stations[0].Stats == nil {
		t.Fatalf("me: %+v", me)
	}
	must(t, f.anon("GET", "/api/me", "192.0.2.1:1", ""), 401, "anonymous /api/me")

	// Privacy settings: the pin preview appears; bob cannot touch A.
	rec := f.as(f.alice, "POST", "/api/me/stations/"+sidA, `{"pin":"cell01","name":"Alice Roof","declared":{"lat":-41.2872,"lon":174.7763,"alt":85}}`)
	must(t, rec, 200, "update A")
	var dto stationDTO
	jsonOf(t, rec, &dto)
	if dto.Pin != model.PinCell01 || dto.Name != "Alice Roof" || dto.PublicPoint == nil {
		t.Fatalf("updated A: %+v", dto)
	}
	if dto.PublicPoint.Lat == -41.28713 || privacy.RoundGrid(dto.PublicPoint.Lat) == 0 {
		t.Errorf("public pin equals the exact position: %+v", dto.PublicPoint)
	}
	var det stationDetail
	jsonOf(t, f.as(f.alice, "GET", "/api/me/stations/"+sidA, ""), &det)
	if det.Location.Declared == nil || det.Location.Declared.AltRef != "msl" || det.Location.Declared.Lat != -41.2872 ||
		len(det.Connection.Links) != 1 || det.Connection.Links[0].IP != "203.0.113.5" {
		t.Fatalf("detail: %+v", det)
	}
	// The UI's PUT routes.
	rec = f.as(f.alice, "PUT", "/api/me/stations/"+sidA+"/privacy", `{"pin":"region","publicStats":false,"showName":false}`)
	must(t, rec, 200, "PUT privacy")
	jsonOf(t, rec, &dto)
	if dto.Pin != model.PinRegion || dto.PublicStats || dto.ShowName {
		t.Errorf("PUT privacy: %+v", dto)
	}
	jsonOf(t, f.as(f.alice, "PUT", "/api/me/stations/"+sidA+"/name", `{"name":"Alice Mast"}`), &dto)
	if dto.Name != "Alice Mast" {
		t.Errorf("PUT name: %+v", dto)
	}
	must(t, f.as(f.alice, "PUT", "/api/me/stations/"+sidA+"/location", `{"lat":-41.3,"lon":174.8,"alt":90,"altRef":"hae"}`), 200, "PUT location")
	must(t, f.as(f.alice, "PUT", "/api/me/stations/"+sidA+"/location", `{"lat":-141.3,"lon":174.8,"alt":90}`), 400, "PUT bad location")
	// Confirming a survey estimate makes it the effective position.
	eid, err := f.reg.AddEstimate(context.Background(), registry.Estimate{Idx: f.stations["A"].st.Idx, T: time.Now(), Method: "survey",
		Lat: -41.28799, Lon: 174.77699, HAE: 130, SigmaE: 12, SigmaN: 16, SigmaU: 30, N: 500, Status: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	must(t, f.as(f.alice, "POST", "/api/me/stations/"+sidA+"/location/confirm", `{"estimate":"999999"}`), 404, "unknown estimate")
	rec = f.as(f.alice, "POST", "/api/me/stations/"+sidA+"/location/confirm", `{"estimate":"`+itoa(eid)+`"}`)
	must(t, rec, 200, "confirm estimate")
	jsonOf(t, rec, &dto)
	if dto.Lat == nil || *dto.Lat != -41.28799 || dto.LocStatus != model.LocConfirmed || dto.LocSource != model.LocSrcEstimated || dto.SigmaM != 14.1 {
		t.Errorf("confirmed: %+v", dto)
	}
	must(t, f.as(f.bob, "PUT", "/api/me/stations/"+sidA+"/name", `{"name":"x"}`), 404, "foreign PUT")
	must(t, f.as(f.alice, "POST", "/api/me/stations/"+sidA, `{"pin":"exact"}`), 400, "bad pin")
	must(t, f.as(f.alice, "POST", "/api/me/stations/"+sidA, `{"bogus":1}`), 400, "unknown member")
	must(t, f.as(f.bob, "POST", "/api/me/stations/"+sidA, `{"pin":"hidden"}`), 404, "foreign update")
	must(t, serve(f.srv.Handler(), "POST", "/api/me/stations/"+sidA, f.alice.addr, `{"pin":"hidden"}`, "Cookie", f.alice.cookie), 403, "no CSRF")

	// Owner live picture: no receiver fields of the (non-network) demo
	// tracker; only=mine is empty (the demo stations are not hers).
	rec = f.as(f.alice, "GET", "/api/me/aircraft?only=mine", "")
	must(t, rec, 200, "me aircraft")
	if strings.Contains(rec.Body.String(), `"distKm"`) || strings.Contains(rec.Body.String(), `"feeds"`) {
		t.Errorf("owner picture has receiver fields: %.300s", rec.Body.String())
	}
	must(t, f.as(f.alice, "GET", "/api/me/aircraft?station="+f.stations["B"].st.SID, ""), 404, "foreign station selection")
	must(t, f.as(f.alice, "GET", "/api/me/aircraft?station="+sidA, ""), 200, "own station selection")

	// Unclaim, then the station is gone from /api/me.
	must(t, f.as(f.alice, "POST", "/api/me/stations/"+sidA+"/unclaim", ""), 200, "unclaim")
	jsonOf(t, f.as(f.alice, "GET", "/api/me", ""), &me)
	if len(me.Stations) != 0 {
		t.Fatalf("after unclaim: %+v", me)
	}

	// Account deletion ends the session and removes the account.
	must(t, f.as(f.bob, "DELETE", "/api/me/account", `{"confirm":"NO"}`), 400, "delete without confirm")
	rec = f.as(f.bob, "DELETE", "/api/me/account", `{"confirm":"DELETE"}`)
	must(t, rec, 200, "delete account")
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("cookie not cleared: %q", rec.Header().Get("Set-Cookie"))
	}
	must(t, f.as(f.bob, "GET", "/api/me", ""), 401, "deleted account")
	if _, err := f.reg.Accounts().ByUsername(context.Background(), "bob"); err == nil {
		t.Error("account still exists")
	}
}

func TestAdminRoutes(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.waitTraffic(1)
	sidA := f.stations["A"].st.SID
	// The registry list carries exact positions and owners; it is audited.
	rec := f.as(f.admin, "GET", "/api/admin/stations?limit=2", "")
	must(t, rec, 200, "admin stations")
	var list adminStationsReply
	jsonOf(t, rec, &list)
	if list.Total != 4 || len(list.Stations) != 2 || list.Stations[0].SID != sidA || list.Stations[0].Owner != "alice" ||
		list.Stations[0].Lat == nil || *list.Stations[0].Lat != -41.28713 {
		t.Errorf("admin list: %.600s", rec.Body.String())
	}
	jsonOf(t, f.as(f.admin, "GET", "/api/admin/stations?offset=3", ""), &list)
	if list.Total != 4 || len(list.Stations) != 1 {
		t.Errorf("offset: %+v", list)
	}
	if rec := f.as(f.admin, "GET", "/api/admin/audit?action=station.list", ""); !strings.Contains(rec.Body.String(), `"station.list"`) {
		t.Errorf("list not audited: %s", rec.Body.String())
	}
	// Reveal.
	rec = f.as(f.admin, "POST", "/api/admin/stations/"+sidA+"/reveal", "")
	must(t, rec, 200, "reveal")
	var rv revealReply
	jsonOf(t, rec, &rv)
	if rv.Lat == nil || *rv.Lat != -41.28713 || rv.IP != "203.0.113.5" {
		t.Errorf("reveal: %+v", rv)
	}
	// The detail has them and is audited.
	rec = f.as(f.admin, "GET", "/api/admin/stations/"+sidA, "")
	must(t, rec, 200, "admin station")
	if !strings.Contains(rec.Body.String(), "-41.28713") || !strings.Contains(rec.Body.String(), "203.0.113.5") {
		t.Errorf("admin detail: %.400s", rec.Body.String())
	}
	rec = f.as(f.admin, "GET", "/api/admin/audit?sid="+sidA+"&action=station.reveal", "")
	must(t, rec, 200, "audit")
	if !strings.Contains(rec.Body.String(), `"station.reveal"`) {
		t.Errorf("reveal not audited: %s", rec.Body.String())
	}
	// Owners and anonymous visitors are refused.
	must(t, f.as(f.alice, "GET", "/api/admin/stations", ""), 403, "owner admin list")
	must(t, f.anon("GET", "/api/admin/stations", "192.0.2.1:1", ""), 401, "anonymous admin list")
	// Changes.
	rec = f.as(f.admin, "POST", "/api/admin/stations/"+sidA, `{"admission":"pending","name":"Renamed","trustDelta":5,"anchor":true}`)
	must(t, rec, 200, "admin update")
	var dto stationDTO
	jsonOf(t, rec, &dto)
	if dto.Admission != model.AdmPending || dto.Name != "Renamed" || dto.Trust != 5 || !dto.Anchor {
		t.Errorf("admin update: %+v", dto)
	}
	must(t, f.as(f.admin, "POST", "/api/admin/stations/"+sidA, `{"admission":"maybe"}`), 400, "bad admission")
	must(t, f.as(f.admin, "POST", "/api/admin/stations/"+sidA, `{"ownerId":`+itoa(f.bob.id)+`}`), 200, "assign owner")
	// Tokens: the secret only once.
	rec = f.as(f.admin, "POST", "/api/admin/tokens", `{"note":"club","maxUses":3,"ttlHours":24}`)
	must(t, rec, 200, "mint")
	var mint mintReply
	jsonOf(t, rec, &mint)
	if mint.Token == "" || mint.ID == "" || mint.MaxUses != 3 || mint.Label != "club" {
		t.Fatalf("mint: %+v", mint)
	}
	rec = f.as(f.admin, "POST", "/api/admin/tokens", `{"label":"ui","maxUses":1,"days":2}`)
	must(t, rec, 200, "mint (UI body)")
	rec = f.as(f.admin, "GET", "/api/admin/tokens", "")
	if strings.Contains(rec.Body.String(), mint.Token) || !strings.Contains(rec.Body.String(), mint.ID) || !strings.Contains(rec.Body.String(), `"label":"ui"`) {
		t.Errorf("token list: %s", rec.Body.String())
	}
	must(t, f.as(f.admin, "DELETE", "/api/admin/tokens/"+mint.ID, ""), 200, "revoke token")
	// UI edit and admission routes.
	rec = f.as(f.admin, "PUT", "/api/admin/stations/"+sidA, `{"name":"Edited","trust":2,"owner":"alice"}`)
	must(t, rec, 200, "PUT station")
	jsonOf(t, rec, &dto)
	if dto.Name != "Edited" || dto.Trust != 2 || dto.Owner != "alice" {
		t.Errorf("PUT station: %+v", dto)
	}
	must(t, f.as(f.admin, "PUT", "/api/admin/stations/"+sidA, `{"owner":"nobody-here"}`), 404, "PUT unknown owner")
	rec = f.as(f.admin, "POST", "/api/admin/stations/"+sidA+"/admission", `{"admission":"approved"}`)
	must(t, rec, 200, "admission")
	must(t, f.as(f.admin, "GET", "/api/admin/network", ""), 200, "network status")
	// Links (audited), Beast view without listener, receiver list.
	rec = f.as(f.admin, "GET", "/api/admin/links", "")
	must(t, rec, 200, "links")
	if !strings.Contains(rec.Body.String(), "198.51.100.9") {
		t.Errorf("links: %.300s", rec.Body.String())
	}
	must(t, f.as(f.admin, "GET", "/api/admin/ingest/beast", ""), 503, "beast without listener")
	must(t, f.as(f.admin, "POST", "/api/admin/ingest/beast/1/disconnect", ""), 503, "beast disconnect without listener")
	live := f.eng.Tracker().Snapshot().Aircraft[0].ICAO
	rec = f.as(f.admin, "GET", "/api/admin/aircraft/"+live, "")
	must(t, rec, 200, "admin aircraft")
	must(t, f.as(f.admin, "POST", "/api/admin/retention", ""), 200, "retention")
	// Revoke the native feeder, delete C.
	must(t, f.as(f.admin, "POST", "/api/admin/feeders/"+f.stations["N"].st.FeederID+"/revoke", ""), 200, "revoke feeder")
	must(t, f.as(f.admin, "DELETE", "/api/admin/stations/"+f.stations["C"].st.SID, `{"reason":"test"}`), 200, "delete station")
	must(t, f.as(f.admin, "GET", "/api/admin/stations/"+f.stations["C"].st.SID, ""), 404, "deleted station")
	// Admin mutations need the CSRF token.
	must(t, serve(f.srv.Handler(), "POST", "/api/admin/stations/"+sidA, f.admin.addr, `{"name":"x"}`, "Cookie", f.admin.cookie), 403, "admin without CSRF")
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestPublicRateLimit(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.srv.testRate = &rateSpec{rps: 1, burst: 3}
	codes := []int{}
	for i := 0; i < 5; i++ {
		codes = append(codes, f.anon("GET", "/api/public/status", "192.0.2.33:1", "").Code)
	}
	if codes[0] != 200 || codes[2] != 200 || codes[3] != http.StatusTooManyRequests {
		t.Fatalf("codes %v", codes)
	}
	rec := f.anon("GET", "/api/public/status", "192.0.2.33:1", "")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// Other addresses, logged-in users and the health check are not limited.
	must(t, f.anon("GET", "/api/public/status", "192.0.2.34:1", ""), 200, "other address")
	for i := 0; i < 5; i++ {
		must(t, f.as(f.alice, "GET", "/api/public/status", ""), 200, "owner")
		must(t, f.anon("GET", "/healthz", "192.0.2.33:1", ""), 200, "health")
	}
}

func TestPublicStreamPerIP(t *testing.T) {
	f := newNetFixture(t, rigOptions{listen: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	go func() {
		req := httptest.NewRequest("GET", "/api/public/events", nil).WithContext(ctx)
		req.RemoteAddr = "192.0.2.40:1"
		close(started)
		f.srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started
	deadline := time.Now().Add(5 * time.Second)
	for f.srv.live.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rec := f.anon("GET", "/api/events", "192.0.2.40:2", "")
	must(t, rec, http.StatusServiceUnavailable, "second public stream from one address")
	if !strings.Contains(rec.Body.String(), "one public live stream") {
		t.Errorf("message: %s", rec.Body.String())
	}
	// An owner from the same address still gets a stream.
	body := f.readSSE(t, "/api/events", "192.0.2.40:3", 300*time.Millisecond, "Cookie", f.alice.cookie)
	if !strings.Contains(string(body), "event: status") {
		t.Errorf("owner stream: %.200s", body)
	}
}

func TestHealthz(t *testing.T) {
	r := newRig(t, rigOptions{})
	rec := r.get("/healthz")
	must(t, rec, 200, "standalone health")
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) || !strings.Contains(rec.Body.String(), `"engine":"ok"`) {
		t.Errorf("health: %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("health is cacheable")
	}
	r.srv.auth.net.testMode = model.ModeAggregator
	rec = r.get("/healthz")
	must(t, rec, 200, "aggregator health without registry")
	if !strings.Contains(rec.Body.String(), `"degraded"`) {
		t.Errorf("aggregator health: %s", rec.Body.String())
	}
	if rec := r.do("HEAD", "/healthz", lanAddr, ""); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d %d", rec.Code, rec.Body.Len())
	}
	r.eng.Stop()
	if rec := r.get("/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("stopped engine: %d %s", rec.Code, rec.Body.String())
	}
}

// fakeUplinks echoes the request so the mount can be checked.
type fakeUplinks struct{}

func (fakeUplinks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, 200, map[string]string{"method": r.Method})
}

func TestUplinksMount(t *testing.T) {
	r := newRig(t, rigOptions{})
	must(t, r.get("/api/uplinks"), http.StatusServiceUnavailable, "no manager")
	r.srv.SetNetwork(Network{Uplinks: fakeUplinks{}})
	must(t, r.do("GET", "/api/uplinks", lanAddr, ""), 200, "standalone GET from the LAN")
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{"http":{"port":80,"tileCache":true,"adminPasswordHash":"plain:s3cret"}}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	must(t, r.do("POST", "/api/uplinks", lanAddr, `{"action":"retry","id":"x"}`), 401, "standalone POST without admin")
	must(t, r.do("POST", "/api/uplinks", loopbackAddr, `{"action":"retry","id":"x"}`), 200, "standalone POST from loopback")
	// Aggregator: admin only.
	r.srv.auth.net.testMode = model.ModeAggregator
	must(t, r.do("GET", "/api/uplinks", lanAddr, ""), 401, "aggregator anonymous")
}

// TestCacheKeyPerViewer: a cached answer of one viewer is never served to
// another (DESIGN §8.4).
func TestCacheKeyPerViewer(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	rt := &Route{Path: "/api/trails", Access: AccessViewer}
	req := func(s session4) *http.Request {
		q := httptest.NewRequest("GET", "/api/trails?minutes=5", nil)
		if s.cookie != "" {
			q.Header.Set("Cookie", s.cookie)
		}
		return q
	}
	kPub := f.srv.viewerCacheKey(req(session4{}), rt)
	kA := f.srv.viewerCacheKey(req(f.alice), rt)
	kB := f.srv.viewerCacheKey(req(f.bob), rt)
	kAdm := f.srv.viewerCacheKey(req(f.admin), rt)
	if kPub == kA || kA == kB || kA == kAdm || kPub == kAdm {
		t.Errorf("keys not distinct: %q %q %q %q", kPub, kA, kB, kAdm)
	}
	// Public routes share one key per viewer class: signed-in viewers
	// share theirs, anonymous viewers have their own (the public gates
	// refuse only anonymous viewers: finding PRIV-3).
	pub := &Route{Path: "/api/public/aircraft", Access: AccessPublic}
	if f.srv.viewerCacheKey(req(f.alice), pub) != f.srv.viewerCacheKey(req(f.bob), pub) {
		t.Error("public routes should share one key among signed-in viewers")
	}
	if f.srv.viewerCacheKey(req(f.alice), pub) == f.srv.viewerCacheKey(req(session4{}), pub) {
		t.Error("a signed-in viewer's public answer must not be cached for anonymous viewers")
	}
	// Standalone: unchanged keys.
	f.srv.auth.net.testMode = model.ModeStandalone
	if k := f.srv.viewerCacheKey(req(session4{}), rt); k != "/api/trails|minutes=5" {
		t.Errorf("standalone key %q", k)
	}
}
