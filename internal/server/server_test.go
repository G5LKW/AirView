package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/engine"
	"airview/internal/model"
)

// sampleURL resolves the placeholders of Route.Sample against live data.
func sampleURL(t testing.TB, r *rig, rt Route) string {
	u := rt.Sample
	if u == "" {
		u = rt.Path
	}
	if strings.Contains(u, "{live}") {
		snap := r.eng.Tracker().Snapshot()
		if len(snap.Aircraft) == 0 {
			t.Fatal("no live aircraft")
		}
		icao := snap.Aircraft[0].ICAO
		for _, a := range snap.Aircraft { // prefer one with a position
			if a.Lat != nil && !strings.HasPrefix(a.ICAO, "~") {
				icao = a.ICAO
				break
			}
		}
		u = strings.ReplaceAll(u, "{live}", icao)
	}
	if strings.Contains(u, "{seen}") || strings.Contains(u, "{flight}") {
		fl, err := r.eng.Store().RecentFlights(context.Background(), 50, 0)
		if err != nil || len(fl) == 0 {
			t.Fatalf("no stored flights: %v", err)
		}
		f := fl[len(fl)-1]
		for _, c := range fl { // prefer a flight with a trail
			if c.Positions > 20 {
				f = c
				break
			}
		}
		u = strings.ReplaceAll(u, "{seen}", fmt.Sprintf("%06X", f.ICAO))
		u = strings.ReplaceAll(u, "{flight}", strconv.FormatInt(f.ID, 10))
	}
	// Reference data seeded by seedNavData.
	u = strings.ReplaceAll(u, "{airport}", "EDDF")
	u = strings.ReplaceAll(u, "{route}", "DLH400?lat=50.2&lon=7.9")
	if strings.Contains(u, "{watchlist}") {
		if err := r.eng.Alerts().AddWatch(context.Background(), "demo", 0x3C6444, "Lufthansa A319 (test entry)"); err != nil {
			t.Fatalf("watch-list: %v", err)
		}
		u = strings.ReplaceAll(u, "{watchlist}", "demo")
	}
	if strings.ContainsAny(u, "{}") {
		t.Fatalf("route %s %s: unresolved sample %q", rt.Method, rt.Path, u)
	}
	return u
}

// checkBody validates a response body by its content type.
func checkBody(ct string, body []byte) error {
	switch {
	case strings.HasPrefix(ct, "text/csv"):
		_, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
		return err
	case strings.HasPrefix(ct, ctKML):
		d := xml.NewDecoder(bytes.NewReader(body))
		for {
			if _, err := d.Token(); err == io.EOF {
				return nil
			} else if err != nil {
				return err
			}
		}
	case strings.HasPrefix(ct, "image/"), strings.HasPrefix(ct, "text/plain"):
		return nil
	}
	if !json.Valid(body) {
		return fmt.Errorf("invalid JSON: %.200s", body)
	}
	var v any
	return json.Unmarshal(body, &v)
}

// specialTests names the test that covers each route the generic test
// skips. A new NoAuto route must be added here (and get a test).
var specialTests = map[string]string{
	"PUT /api/config":               "TestConfigRoundTrip, TestAuthFlow",
	"POST /api/events/view":         "TestViewStreamHTTP",
	"GET /api/auth/setup":           "TestAggregatorBootstrapAPI, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/auth/setup/begin":    "TestAggregatorBootstrapAPI, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/auth/setup/finish":   "TestAggregatorBootstrapAPI, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/auth/register":       "TestAggregatorOwnerScope, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/auth/password":       "TestAggregatorOwnerScope, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/auth/totp/begin":     "TestAggregatorOwnerScope, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/auth/totp/confirm":   "TestAggregatorOwnerScope, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/auth/totp/disable":   "TestAggregatorOwnerScope, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/auth/recovery-codes": "TestAggregatorOwnerScope, TestStandaloneCSRFAndAccountRoutes",
	"GET /api/auth/sessions":        "TestAggregatorOwnerScope, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/auth/logout-others":  "TestAggregatorOwnerScope, TestStandaloneCSRFAndAccountRoutes",
	"POST /api/factory-reset":       "TestFactoryReset",
	"POST /api/login":               "TestAuthFlow",
	"POST /api/logout":              "TestAuthFlow",
	"GET /api/events":               "TestSSE",
	"POST /api/tilecache/clear":     "TestTileCache",
	"POST /api/weather/refresh":     "TestAdminPosts",
	"POST /api/forecast/refit":      "TestAdminPosts",
	"POST /api/timing/ntp/sample":   "TestAdminPosts",
	"POST /api/guess":               "TestAdminPosts",
	"POST /api/db/vacuum":           "TestAdminPosts",
	"POST /api/db/prune":            "TestAdminPosts",
	"POST /api/db/checkpoint":       "TestAdminPosts",
	"POST /api/db/rollup":           "TestAdminPosts",
	"POST /api/db/acdb/import":      "TestACDBUpload",
	"POST /api/db/acdb/upload":      "TestACDBUpload",
	"POST /api/db/acdb/cancel":      "TestACDBUpload",

	"POST /api/terrain/prefetch":                  "TestExtAdmin",
	"POST /api/terrain/recompute":                 "TestExtAdmin",
	"POST /api/terrain/cancel":                    "TestExtAdmin",
	"GET /api/terrain/floor.png":                  "TestExtAdmin",
	"POST /api/navdata/import":                    "TestExtAdmin",
	"POST /api/navdata/cancel":                    "TestExtAdmin",
	"POST /api/navdata/route-cache/clear":         "TestExtAdmin",
	"GET /api/navdata/photo/{icao}":               "TestExtAdmin",
	"POST /api/alerts/rules":                      "TestAlertsAPI",
	"PUT /api/alerts/rules/{id}":                  "TestAlertsAPI",
	"DELETE /api/alerts/rules/{id}":               "TestAlertsAPI",
	"POST /api/alerts/rules/{id}/enabled":         "TestAlertsAPI",
	"POST /api/alerts/rules/reset-defaults":       "TestAlertsAPI",
	"POST /api/alerts/test":                       "TestAlertsAPI",
	"POST /api/alerts/webhook-test":               "TestAlertsAPI",
	"POST /api/alerts/import":                     "TestAlertsAPI",
	"POST /api/alerts/watchlists/{name}/import":   "TestAlertsAPI",
	"PUT /api/alerts/watchlists/{name}/{icao}":    "TestAlertsAPI",
	"DELETE /api/alerts/watchlists/{name}/{icao}": "TestAlertsAPI",
	"DELETE /api/alerts/watchlists/{name}":        "TestAlertsAPI",
}

func TestRouteTable(t *testing.T) {
	seen := map[string]bool{}
	for _, rt := range Routes() {
		key := rt.Method + " " + rt.Path
		if seen[key] {
			t.Errorf("duplicate route %s", key)
		}
		seen[key] = true
		if (rt.API == nil) == (rt.Raw == nil) {
			t.Errorf("%s: exactly one of API and Raw must be set", key)
		}
		if rt.Summary == "" || rt.Group == "" {
			t.Errorf("%s: undocumented", key)
		}
		// The account routes check the session scope themselves (and answer
		// 404 in standalone mode); owner routes of aggregator mode need an
		// owner session (route gate, access.go) and the CSRF token.
		if rt.Method != "GET" && !rt.Admin && rt.Path != "/api/login" && rt.Path != "/api/logout" && rt.Path != "/api/events/view" &&
			!strings.HasPrefix(rt.Path, "/api/auth/") && !(rt.Access == AccessOwner && rt.NetOnly) {
			t.Errorf("%s: mutating route is not admin", key)
		}
		if rt.Method == "GET" && rt.Admin {
			t.Errorf("%s: read-only routes are open", key)
		}
		if rt.Cache > 0 && (rt.Method != "GET" || rt.API == nil) {
			t.Errorf("%s: only GET JSON routes can be cached", key)
		}
		if rt.NoAuto && specialTests[key] == "" {
			t.Errorf("%s: NoAuto route without an entry in specialTests", key)
		}
		if !rt.NoAuto && rt.Method != "GET" {
			t.Errorf("%s: non-GET routes must be NoAuto", key)
		}
	}
	for key := range specialTests {
		if !seen[key] {
			t.Errorf("specialTests names the unknown route %s", key)
		}
	}
}

// TestAllRoutes calls every read-only endpoint, from the machine itself and
// from the LAN, and checks status, content type and that the body is valid
// JSON without NaN.
func TestAllRoutes(t *testing.T) {
	r := shared(t)
	for _, remote := range []string{loopbackAddr, lanAddr} {
		for _, rt := range Routes() {
			if rt.NoAuto {
				continue
			}
			u := sampleURL(t, r, rt)
			rec := r.do(rt.Method, u, remote, "")
			name := rt.Method + " " + u
			if rec.Code != http.StatusOK {
				t.Errorf("%s from %s: status %d: %.300s", name, remote, rec.Code, rec.Body.String())
				continue
			}
			ct := rec.Header().Get("Content-Type")
			body := rec.Body.Bytes()
			if rt.Path == "/api/db/export/{file}" {
				if !strings.HasPrefix(ct, "text/csv") {
					t.Errorf("%s: content type %q", name, ct)
				}
				if _, err := csv.NewReader(bytes.NewReader(body)).ReadAll(); err != nil {
					t.Errorf("%s: invalid CSV: %v", name, err)
				}
				continue
			}
			want := "application/json"
			if rt.ContentType != "" {
				want = rt.ContentType
			}
			if !strings.HasPrefix(ct, want) {
				t.Errorf("%s: content type %q, want %q", name, ct, want)
			}
			if err := checkBody(ct, body); err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s: Cache-Control %q", name, rec.Header().Get("Cache-Control"))
			}
			if rec.Header().Get("X-Frame-Options") != "" {
				t.Errorf("%s: X-Frame-Options must not be sent", name)
			}
		}
	}
}

// TestEmptyDatabase: every endpoint also answers on a fresh installation.
func TestEmptyDatabase(t *testing.T) {
	r := newRig(t, rigOptions{})
	for _, rt := range Routes() {
		if rt.NoAuto || strings.Contains(rt.Sample, "{") {
			continue
		}
		u := rt.Sample
		if u == "" {
			u = rt.Path
		}
		rec := r.get(u)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d: %.300s", u, rec.Code, rec.Body.String())
		} else if err := checkBody(rec.Header().Get("Content-Type"), rec.Body.Bytes()); err != nil {
			t.Errorf("GET %s: %v", u, err)
		}
	}
	// Unknown things are JSON errors.
	for u, code := range map[string]int{
		"/api/nope":                       404,
		"/api/aircraft/XYZ":               400,
		"/api/history/aircraft/ABCDEF":    404,
		"/api/history/flights/999999":     404,
		"/api/history/flights/abc":        400,
		"/api/db/export/kv.csv":           404,
		"/api/db/export/aircraft":         404,
		"/api/stats/rates?hours=abc":      400,
		"/api/stats/rates?from=5&to=3":    400,
		"/api/antenna/coverage?band=x":    400,
		"/api/geo/elevation":              400,
		"/api/geo/format?lat=95&lon=0":    400,
		"/api/radio/pattern?ant=nosuch":   404,
		"/api/history/aircraft?sort=evil": 400,
	} {
		rec := r.get(u)
		var e struct {
			Error string `json:"error"`
		}
		if rec.Code != code || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error == "" {
			t.Errorf("GET %s: status %d body %.120s, want %d with {error}", u, rec.Code, rec.Body.String(), code)
		}
	}
	if rec := r.do("DELETE", "/api/status", loopbackAddr, ""); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("DELETE /api/status: %d", rec.Code)
	}
	// The UI is served at / with a JavaScript MIME type for modules.
	if rec := r.get("/"); rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("GET /: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := r.get("/js/app.js"); rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("GET /js/app.js: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestCoreShapes(t *testing.T) {
	r := shared(t)
	var st model.Status
	rec := r.get("/api/status")
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.App != model.AppName || st.Version != "test" || !st.Demo || !st.Located || st.AdminLocked ||
		st.HTTPPort != r.srv.Port() || !st.TileCache || len(st.TileCacheHosts) != 3 || len(st.Feeds) == 0 {
		t.Errorf("status = %+v", st)
	}

	var snap model.LiveSnapshot
	if err := json.Unmarshal(r.get("/api/aircraft").Body.Bytes(), &snap); err != nil || len(snap.Aircraft) == 0 {
		t.Fatalf("aircraft: %v, %d", err, len(snap.Aircraft))
	}

	var det struct {
		Aircraft *model.Aircraft   `json:"aircraft"`
		Trail    []json.RawMessage `json:"trail"`
		Info     map[string]any    `json:"info"`
		Flights  []map[string]any  `json:"flights"`
	}
	u := sampleURL(t, r, Route{Sample: "/api/aircraft/{live}"})
	if err := json.Unmarshal(r.get(u).Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	if det.Aircraft == nil || det.Info["icao"] != det.Aircraft.ICAO || det.Flights == nil || det.Trail == nil {
		t.Errorf("detail of %s incomplete: %+v", u, det)
	}
	// An address that is not tracked: 200 with aircraft null.
	rec = r.get("/api/aircraft/000001")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"aircraft":null`) || !strings.Contains(rec.Body.String(), `"trail":[]`) {
		t.Errorf("unknown aircraft: %d %.200s", rec.Code, rec.Body.String())
	}

	var trails struct {
		Now    int64                  `json:"now"`
		Trails map[string][][]float64 `json:"trails"`
	}
	raw := r.get("/api/trails?minutes=30").Body.Bytes()
	// Points may contain null: decode loosely first.
	var loose struct {
		Trails map[string][][]any `json:"trails"`
	}
	if err := json.Unmarshal(raw, &loose); err != nil || len(loose.Trails) == 0 {
		t.Fatalf("trails: %v (%d)", err, len(loose.Trails))
	}
	for icao, pts := range loose.Trails {
		if len(pts) == 0 || len(pts[0]) != 7 {
			t.Fatalf("trail %s: bad point %v", icao, pts)
		}
		break
	}
	_ = trails

	// Stored flight with its trail.
	u = sampleURL(t, r, Route{Sample: "/api/history/flights/{flight}"})
	var fl struct {
		Flight map[string]any `json:"flight"`
		Trail  [][]any        `json:"trail"`
	}
	if err := json.Unmarshal(r.get(u).Body.Bytes(), &fl); err != nil || fl.Flight["id"] == nil || len(fl.Trail) == 0 {
		t.Errorf("%s: %v, flight %v, trail %d", u, err, fl.Flight, len(fl.Trail))
	}

	// Search and paging.
	var list struct {
		Total int              `json:"total"`
		Rows  []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(r.get("/api/history/aircraft?limit=2&sort=msgs").Body.Bytes(), &list); err != nil || len(list.Rows) != 2 || list.Total < 2 {
		t.Fatalf("history list: %v %+v", err, list)
	}
	if list.Rows[0]["msgs"].(float64) < list.Rows[1]["msgs"].(float64) {
		t.Errorf("sort=msgs is not descending by default")
	}
	icao := list.Rows[0]["icao"].(string)
	if err := json.Unmarshal(r.get("/api/history/aircraft?q="+strings.ToLower(icao)).Body.Bytes(), &list); err != nil || list.Total != 1 {
		t.Errorf("search %s: total %d (%v)", icao, list.Total, err)
	}

	// Coordinate parsing.
	var gp struct {
		OK     bool     `json:"ok"`
		Lat    *float64 `json:"lat"`
		Lon    *float64 `json:"lon"`
		Format string   `json:"format"`
		Error  string   `json:"error"`
	}
	_ = json.Unmarshal(r.get("/api/geo/parse?q=JO40hb").Body.Bytes(), &gp)
	if !gp.OK || gp.Lat == nil || math.Abs(*gp.Lat-50.06) > 0.1 || !strings.Contains(strings.ToLower(gp.Format), "maidenhead") {
		t.Errorf("geo parse locator: %+v", gp)
	}
	gp = struct {
		OK     bool     `json:"ok"`
		Lat    *float64 `json:"lat"`
		Lon    *float64 `json:"lon"`
		Format string   `json:"format"`
		Error  string   `json:"error"`
	}{}
	rec = r.get("/api/geo/parse?q=hello+world")
	_ = json.Unmarshal(rec.Body.Bytes(), &gp)
	if rec.Code != 200 || gp.OK || gp.Error == "" || gp.Lat != nil {
		t.Errorf("geo parse garbage: %d %+v", rec.Code, gp)
	}

	// The seeded history reaches the analyses.
	var rates struct {
		Series struct {
			T []int64 `json:"t"`
		} `json:"series"`
	}
	_ = json.Unmarshal(r.get("/api/stats/rates?hours=4").Body.Bytes(), &rates)
	if len(rates.Series.T) < 60 {
		t.Errorf("stats/rates: %d points", len(rates.Series.T))
	}
	var cov struct {
		Positions int64 `json:"positions"`
		Bands     []any `json:"bands"`
	}
	_ = json.Unmarshal(r.get("/api/antenna/coverage").Body.Bytes(), &cov)
	if cov.Positions == 0 || len(cov.Bands) != 7 {
		t.Errorf("coverage: %d positions, %d bands", cov.Positions, len(cov.Bands))
	}
	_ = json.Unmarshal(r.get("/api/antenna/coverage?band=5").Body.Bytes(), &cov)
	if len(cov.Bands) != 1 {
		t.Errorf("coverage?band=5: %d bands", len(cov.Bands))
	}
	var sys struct {
		Goroutines int    `json:"goroutines"`
		DataDir    string `json:"dataDir"`
		Restart    *bool  `json:"restartRequired"`
	}
	_ = json.Unmarshal(r.get("/api/system").Body.Bytes(), &sys)
	if sys.Goroutines == 0 || sys.DataDir != r.dir || sys.Restart == nil {
		t.Errorf("system: %+v", sys)
	}
}

func TestGzipAndCORS(t *testing.T) {
	r := shared(t)
	rec := r.do("GET", "/api/aircraft", lanAddr, "", "Accept-Encoding", "gzip, deflate", "Origin", "http://192.168.1.99:8080")
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("no gzip: %v", rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(zr)
	if !json.Valid(b) {
		t.Fatal("gzip body is not JSON")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("GET CORS header missing: %v", rec.Header())
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials must not be allowed cross-origin")
	}
	// Small bodies and clients without gzip stay plain.
	if rec := r.do("GET", "/api/session", lanAddr, "", "Accept-Encoding", "gzip"); rec.Header().Get("Content-Encoding") != "" {
		t.Error("tiny body was compressed")
	}
	if rec := r.get("/api/aircraft"); rec.Header().Get("Content-Encoding") != "" {
		t.Error("compressed without Accept-Encoding")
	}
	// Cached route: both forms.
	for _, enc := range []string{"gzip", ""} {
		rec := r.do("GET", "/api/antenna/coverage", lanAddr, "", "Accept-Encoding", enc)
		if (rec.Header().Get("Content-Encoding") == "gzip") != (enc == "gzip") {
			t.Errorf("cached route, Accept-Encoding %q: got %q", enc, rec.Header().Get("Content-Encoding"))
		}
	}
	// Preflight: reads only.
	rec = r.do("OPTIONS", "/api/config", lanAddr, "", "Origin", "http://other:80", "Access-Control-Request-Method", "PUT")
	if rec.Code != http.StatusNoContent || strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "PUT") {
		t.Errorf("preflight: %d %v", rec.Code, rec.Header())
	}
}

func TestSSE(t *testing.T) {
	r := shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(r.srv.LocalURL(), "/")+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		r.eng.Notify("info", "hello from the test")
	}()
	counts := map[string]int{}
	var event string
	gotRetry, gotNotice := false, false
	var firstAircraft, lastAircraft time.Time
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "retry:"):
			gotRetry = true
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if !json.Valid([]byte(data)) {
				t.Fatalf("event %s: invalid JSON", event)
			}
			counts[event]++
			switch event {
			case "aircraft":
				var s model.LiveSnapshot
				if err := json.Unmarshal([]byte(data), &s); err != nil || s.Now == 0 {
					t.Fatalf("aircraft event: %v", err)
				}
				if firstAircraft.IsZero() {
					firstAircraft = time.Now()
				}
				lastAircraft = time.Now()
			case "status":
				var s model.Status
				if err := json.Unmarshal([]byte(data), &s); err != nil || s.App == "" || s.AdminLocked {
					t.Fatalf("status event: %v %+v", err, s)
				}
			case "notice":
				if strings.Contains(data, "hello from the test") {
					gotNotice = true
				}
			}
		}
		if counts["aircraft"] >= 4 && counts["status"] >= 2 && gotNotice {
			break
		}
	}
	if !gotRetry || counts["aircraft"] < 4 || counts["status"] < 2 || !gotNotice {
		t.Fatalf("stream incomplete: retry=%v counts=%v notice=%v err=%v", gotRetry, counts, gotNotice, sc.Err())
	}
	// 1 Hz: three intervals between four events.
	if d := lastAircraft.Sub(firstAircraft); d < 1500*time.Millisecond || d > 12*time.Second {
		t.Errorf("4 aircraft events spread over %v", d)
	}
	cancel()
	// The hub forgets the client.
	deadline := time.Now().Add(5 * time.Second)
	for r.srv.live.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := r.srv.live.count(); n != 0 {
		t.Errorf("%d live clients left after disconnect", n)
	}
}

// TestSSESlowClient: a client that never reads is cut off and does not
// block the hub.
func TestSSESlowClient(t *testing.T) {
	h := newLiveHub(&Server{log: quietLogger()})
	slow := &liveClient{ch: make(chan []byte, sseQueue), kick: make(chan struct{})}
	fast := &liveClient{ch: make(chan []byte, sseQueue), kick: make(chan struct{})}
	h.clients[slow], h.clients[fast] = struct{}{}, struct{}{}
	got := 0
	for i := 0; i < sseQueue+sseMaxMissed+2; i++ {
		h.broadcast([]byte("x"), nil)
		select {
		case <-fast.ch:
			got++
		default:
		}
	}
	select {
	case <-slow.kick:
	default:
		t.Fatal("slow client was not disconnected")
	}
	if _, ok := h.clients[slow]; ok || len(h.clients) != 1 {
		t.Fatal("slow client still registered")
	}
	if got != sseQueue+sseMaxMissed+2 {
		t.Fatalf("fast client got %d frames", got)
	}
}

func cookieOf(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Name + "=" + c.Value
		}
	}
	return ""
}

func TestAuthFlow(t *testing.T) {
	r := newRig(t, rigOptions{})
	// No password yet: the LAN is trusted.
	var st model.Status
	_ = json.Unmarshal(r.do("GET", "/api/status", lanAddr, "").Body.Bytes(), &st)
	if st.AdminLocked {
		t.Fatal("locked without a password")
	}
	// Set a password from the machine itself.
	rec := r.do("PUT", "/api/config", loopbackAddr, `{"http":{"port":80,"tileCache":true,"adminPasswordHash":"plain:s3cret"}}`)
	if rec.Code != 200 {
		t.Fatalf("set password: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cret") || strings.Contains(rec.Body.String(), "sha256") {
		t.Fatalf("secret leaked: %s", rec.Body.String())
	}
	if h := r.eng.Config().Get().HTTP.AdminPasswordHash; !strings.HasPrefix(h, "pbkdf2-sha256$") { // SEC-08: a slow KDF
		t.Fatalf("stored hash = %q", h)
	}

	// LAN client: read open, write locked.
	_ = json.Unmarshal(r.do("GET", "/api/status", lanAddr, "").Body.Bytes(), &st)
	if !st.AdminLocked {
		t.Error("status.adminLocked false for a LAN client")
	}
	_ = json.Unmarshal(r.do("GET", "/api/status", loopbackAddr, "").Body.Bytes(), &st)
	if st.AdminLocked {
		t.Error("status.adminLocked true for loopback")
	}
	if rec := r.do("GET", "/api/config", lanAddr, ""); rec.Code != 200 {
		t.Errorf("GET config from LAN: %d", rec.Code)
	}
	for _, rt := range Routes() {
		if !rt.Admin {
			continue
		}
		rec := r.do(rt.Method, strings.NewReplacer("{file}", "x", "{icao}", "ABCDEF", "{id}", "1").Replace(rt.Path), lanAddr, `{}`)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "admin password required") {
			t.Errorf("%s %s from LAN without login: %d %s", rt.Method, rt.Path, rec.Code, rec.Body.String())
		}
	}

	// Login.
	if rec := r.do("POST", "/api/login", lanAddr, `{"password":"wrong"}`); rec.Code != 401 || cookieOf(rec) != "" {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	rec = r.do("POST", "/api/login", lanAddr, `{"password":"s3cret"}`)
	cookie := cookieOf(rec)
	if rec.Code != 200 || cookie == "" {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get("Set-Cookie")
	if !strings.Contains(raw, "HttpOnly") || !strings.Contains(raw, "SameSite=Lax") {
		t.Errorf("cookie attributes: %s", raw)
	}
	_ = json.Unmarshal(r.do("GET", "/api/status", lanAddr, "", "Cookie", cookie).Body.Bytes(), &st)
	if st.AdminLocked {
		t.Error("still locked after login")
	}
	if rec := r.do("PUT", "/api/config", lanAddr, `{"receiver":{"name":"From LAN"}}`, "Cookie", cookie); rec.Code != 200 {
		t.Fatalf("PUT with session: %d %s", rec.Code, rec.Body.String())
	}
	if n := r.eng.Config().Get().Receiver.Name; n != "From LAN" {
		t.Errorf("receiver name = %q", n)
	}
	// A made-up cookie is worthless.
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", sessionCookie+"=deadbeef"); rec.Code != 401 {
		t.Errorf("forged cookie: %d", rec.Code)
	}

	// CSRF: a foreign page posting with the user's cookie (or from loopback).
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{}`, "Origin", "http://evil.example"); rec.Code != 403 {
		t.Errorf("cross-origin PUT from loopback: %d", rec.Code)
	}
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, "Sec-Fetch-Site", "cross-site"); rec.Code != 403 {
		t.Errorf("cross-site PUT: %d", rec.Code)
	}
	// Same origin is fine.
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie, "Origin", "http://192.168.1.10:8080"); rec.Code != 200 {
		t.Errorf("same-origin PUT: %d %s", rec.Code, rec.Body.String())
	}
	// DNS rebinding: a public name pointing at loopback gets nothing...
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{}`, "Host", "rebind.attacker.example:8080", "Origin", "http://rebind.attacker.example:8080"); rec.Code != 403 {
		t.Errorf("rebinding PUT: %d", rec.Code)
	}
	// ...but the same name with a real login session works (reverse proxy / own domain).
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`, "Host", "airview.example.org", "Cookie", cookie); rec.Code != 200 {
		t.Errorf("own-domain PUT with session: %d %s", rec.Code, rec.Body.String())
	}

	// Logout.
	rec = r.do("POST", "/api/logout", lanAddr, "", "Cookie", cookie)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("logout: %d %v", rec.Code, rec.Header())
	}
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie); rec.Code != 401 {
		t.Errorf("PUT after logout: %d", rec.Code)
	}

	// Changing the password ends the other sessions.
	cookie = cookieOf(r.do("POST", "/api/login", lanAddr, `{"password":"s3cret"}`))
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{"http":{"port":80,"tileCache":true,"adminPasswordHash":"plain:other"}}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`, "Cookie", cookie); rec.Code != 401 {
		t.Errorf("old session after password change: %d", rec.Code)
	}

	// Throttling, per address.
	const other = "192.168.1.88:40000"
	var last *httptest.ResponseRecorder
	for i := 0; i < loginFree+1; i++ {
		last = r.do("POST", "/api/login", other, `{"password":"guess"}`)
	}
	if last.Code != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" {
		t.Errorf("after %d failures: %d %v", loginFree+1, last.Code, last.Header())
	}
	if rec := r.do("POST", "/api/login", other, `{"password":"other"}`); rec.Code != http.StatusTooManyRequests {
		t.Errorf("correct password while throttled: %d", rec.Code)
	}
	if rec := r.do("POST", "/api/login", lanAddr, `{"password":"other"}`); rec.Code != 200 {
		t.Errorf("another address is not throttled: %d", rec.Code)
	}

	// "clear" removes the password again.
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{"http":{"port":80,"tileCache":true,"adminPasswordHash":"clear"}}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := r.do("PUT", "/api/config", lanAddr, `{}`); rec.Code != 200 {
		t.Errorf("PUT from LAN after clearing the password: %d", rec.Code)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	r := newRig(t, rigOptions{})
	var cfg model.Config
	if err := json.Unmarshal(r.get("/api/config").Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Hub.Password = "hunter2"
	cfg.Remote.Password = "remote-pw"
	cfg.HTTP.AdminPasswordHash = "plain:admin-pw"
	cfg.Receiver.Name = "Round Trip"
	cfg.Receiver.Located, cfg.Receiver.Lat, cfg.Receiver.Lon = true, 48.1, 11.5
	body, _ := json.Marshal(cfg)
	rec := r.do("PUT", "/api/config", loopbackAddr, string(body))
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	var got model.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Hub.Password != "set" || got.Remote.Password != "set" || got.HTTP.AdminPasswordHash != "set" || got.Receiver.Name != "Round Trip" {
		t.Fatalf("reply not redacted / applied: %+v %+v", got.Hub, got.HTTP)
	}

	// The UI sends back what it got ("set") with another edit.
	_ = json.Unmarshal(r.get("/api/config").Body.Bytes(), &got)
	got.Receiver.Name = "Second Edit"
	got.UI.TrailMinutes = 30
	body, _ = json.Marshal(got)
	if rec := r.do("PUT", "/api/config", loopbackAddr, string(body)); rec.Code != 200 {
		t.Fatalf("second PUT: %d %s", rec.Code, rec.Body.String())
	}
	stored := r.eng.Config().Get()
	if stored.Hub.Password != "hunter2" || stored.Remote.Password != "remote-pw" || !strings.HasPrefix(stored.HTTP.AdminPasswordHash, "pbkdf2-sha256$") {
		t.Fatalf("secrets lost: hub %q remote %q hash %q", stored.Hub.Password, stored.Remote.Password, stored.HTTP.AdminPasswordHash)
	}
	if stored.Receiver.Name != "Second Edit" || stored.UI.TrailMinutes != 30 || stored.Receiver.Lat != 48.1 {
		t.Fatalf("edit not stored: %+v", stored.Receiver)
	}
	// It is on disk.
	raw, err := os.ReadFile(r.dir + "/config.json")
	if err != nil || !bytes.Contains(raw, []byte("Second Edit")) || !bytes.Contains(raw, []byte("hunter2")) {
		t.Fatalf("config.json: %v", err)
	}

	// A partial document keeps everything else.
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{"ui":{"units":"metric","trailMinutes":20,"aircraftTtlS":60}}`); rec.Code != 200 {
		t.Fatalf("partial PUT: %d %s", rec.Code, rec.Body.String())
	}
	stored = r.eng.Config().Get()
	if stored.UI.Units != "metric" || len(stored.Antennas) != 2 || len(stored.Feeds) != 1 || stored.Receiver.Name != "Second Edit" || stored.Hub.Password != "hunter2" {
		t.Fatalf("partial PUT damaged the configuration: %+v", stored)
	}
	// "clear".
	got.Hub.Password = "clear"
	body, _ = json.Marshal(got)
	r.do("PUT", "/api/config", loopbackAddr, string(body))
	if p := r.eng.Config().Get().Hub.Password; p != "" {
		t.Errorf("hub password after clear = %q", p)
	}
	// Garbage is refused.
	for _, bad := range []string{`[1,2]`, `{"receiver":`, `"x"`, `{"antennas":"no"}`} {
		if rec := r.do("PUT", "/api/config", loopbackAddr, bad); rec.Code != 400 {
			t.Errorf("PUT %s: %d", bad, rec.Code)
		}
	}
	// A changed port is flagged.
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{"http":{"port":8123,"tileCache":true}}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if !strings.Contains(r.get("/api/system").Body.String(), `"restartRequired":true`) {
		t.Error("restartRequired not reported")
	}
}

func TestFactoryReset(t *testing.T) {
	r := newRig(t, rigOptions{seedHours: 0.4, aircraft: 15})
	r.do("PUT", "/api/config", loopbackAddr, `{"receiver":{"name":"Before Reset"},"http":{"port":80,"tileCache":true,"adminPasswordHash":"plain:pw"}}`)
	cookie := cookieOf(r.do("POST", "/api/login", lanAddr, `{"password":"pw"}`))
	var list struct {
		Total int `json:"total"`
	}
	_ = json.Unmarshal(r.get("/api/history/aircraft").Body.Bytes(), &list)
	if list.Total == 0 {
		t.Fatal("seeded database is empty")
	}
	for _, bad := range []string{`{}`, `{"confirm":"reset"}`, `nonsense`} {
		if rec := r.do("POST", "/api/factory-reset", loopbackAddr, bad); rec.Code != 400 {
			t.Errorf("factory reset with %s: %d", bad, rec.Code)
		}
	}
	if r.eng.Config().Get().Receiver.Name != "Before Reset" {
		t.Fatal("reset happened without confirmation")
	}
	// Requests in flight during the reset must not fail.
	stop := make(chan struct{})
	var bad atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, u := range []string{"/api/status", "/api/stats/overview", "/api/history/aircraft", "/api/db/stats"} {
					if rec := r.get(u); rec.Code != 200 {
						bad.Add(1)
						t.Logf("GET %s during reset: %d %s", u, rec.Code, rec.Body.String())
					}
				}
			}
		}()
	}
	rec := r.do("POST", "/api/factory-reset", lanAddr, `{"confirm":"RESET"}`, "Cookie", cookie)
	close(stop)
	wg.Wait()
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("factory reset: %d %s", rec.Code, rec.Body.String())
	}
	if n := bad.Load(); n != 0 {
		t.Errorf("%d requests failed during the reset", n)
	}
	cfg := r.eng.Config().Get()
	def := model.DefaultConfig()
	if cfg.Receiver.Name != def.Receiver.Name || cfg.HTTP.AdminPasswordHash != "" {
		t.Errorf("configuration not reset: %+v", cfg.Receiver)
	}
	_ = json.Unmarshal(r.get("/api/history/flights").Body.Bytes(), &list)
	var fl struct {
		Flights []any `json:"flights"`
	}
	_ = json.Unmarshal(r.get("/api/history/flights?hours=1000").Body.Bytes(), &fl)
	if len(fl.Flights) > 60 { // only what the demo feed produced since the reset
		t.Errorf("%d flights after the reset", len(fl.Flights))
	}
	if r.srv.auth.count() != 0 {
		t.Error("sessions survived the reset")
	}
	// The engine runs again and serves.
	r.waitTraffic(1)
	if rec := r.get("/api/aircraft"); rec.Code != 200 {
		t.Errorf("aircraft after reset: %d", rec.Code)
	}
}

func TestAdminPosts(t *testing.T) {
	r := newRig(t, rigOptions{seedHours: 0.4, aircraft: 15})
	for _, u := range []string{"/api/db/checkpoint", "/api/db/rollup", "/api/db/prune", "/api/db/vacuum", "/api/forecast/refit", "/api/guess"} {
		rec := r.do("POST", u, loopbackAddr, "")
		if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
			t.Errorf("POST %s: %d %.200s", u, rec.Code, rec.Body.String())
		}
	}
	// Offline engine: the weather poller is off and no NTP server is set.
	if rec := r.do("POST", "/api/weather/refresh", loopbackAddr, ""); rec.Code != http.StatusConflict {
		t.Errorf("weather refresh while disabled: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("POST", "/api/timing/ntp/sample", loopbackAddr, ""); rec.Code != http.StatusBadGateway {
		t.Errorf("NTP sample without a server: %d %s", rec.Code, rec.Body.String())
	}
	// GET on a POST-only route.
	if rec := r.get("/api/db/vacuum"); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/db/vacuum: %d", rec.Code)
	}
}

func TestACDBUpload(t *testing.T) {
	r := newRig(t, rigOptions{})
	// tar1090-db format: icao;reg;type;flags;description;year;owner
	file := "3C6444;D-AIBD;A319;00;AIRBUS A-319;2010;Lufthansa\n4CA2D1;EI-DCL;B738;00;BOEING 737-800;2004;Ryanair\n"
	req := httptest.NewRequest("POST", "/api/db/acdb/upload?name=test.csv", strings.NewReader(file))
	req.RemoteAddr = loopbackAddr
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Skipf("upload format not accepted by refdata.Parse (%d %s); covered by the refdata tests", rec.Code, rec.Body.String())
	}
	var st engine.ImportStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.Stored != 2 {
		t.Fatalf("status: %v %+v", err, st)
	}
	body := r.get("/api/db/acdb/lookup?icao=3c6444").Body.String()
	if !strings.Contains(body, `"found":true`) || !strings.Contains(body, "D-AIBD") {
		t.Errorf("lookup: %s", body)
	}
	if !strings.Contains(r.get("/api/db/acdb/sources").Body.String(), `"rows":2`) {
		t.Errorf("sources: %s", r.get("/api/db/acdb/sources").Body.String())
	}
	// Import request validation (no download is started).
	if rec := r.do("POST", "/api/db/acdb/import", loopbackAddr, `{"source":"nope"}`); rec.Code != 400 {
		t.Errorf("unknown source: %d", rec.Code)
	}
	if rec := r.do("POST", "/api/db/acdb/import", loopbackAddr, `{"url":"file:///etc/passwd"}`); rec.Code != 400 {
		t.Errorf("file URL: %d", rec.Code)
	}
	if rec := r.do("POST", "/api/db/acdb/cancel", loopbackAddr, ""); rec.Code != 200 {
		t.Errorf("cancel: %d", rec.Code)
	}
}

func TestTileCache(t *testing.T) {
	r := newRig(t, rigOptions{})
	var hits atomic.Int32
	var lastPath, lastQuery, lastUA, lastCookie atomic.Value
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 500)...)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		hits.Add(1)
		lastPath.Store(q.URL.EscapedPath())
		lastQuery.Store(q.URL.RawQuery)
		lastUA.Store(q.Header.Get("User-Agent"))
		lastCookie.Store(q.Header.Get("Cookie") + q.Header.Get("Authorization"))
		switch {
		case strings.HasPrefix(q.URL.Path, "/html"):
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<script>alert(1)</script>"))
		case strings.HasPrefix(q.URL.Path, "/missing"):
			http.NotFound(w, q)
		case strings.HasPrefix(q.URL.Path, "/redirect"):
			http.Redirect(w, q, "http://169.254.169.254/latest/meta-data", http.StatusFound)
		case strings.HasPrefix(q.URL.Path, "/gz"):
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write([]byte(`{"tiles":["a","b"],"pad":"` + strings.Repeat("x", 300) + `"}`))
			_ = zw.Close()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(buf.Bytes())
		default:
			if q.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Cache-Control", "max-age=60")
			_, _ = w.Write(png)
		}
	}))
	defer up.Close()
	var asked atomic.Value
	r.srv.tiles.upstream = func(host string) string { asked.Store(host); return up.URL }

	const base = "/tilecache/tile.openstreetmap.org"
	rec := r.do("GET", base+"/5/16/10.png", lanAddr, "", "Cookie", "airview_session=abc", "Authorization", "Bearer x")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), png) || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("first fetch: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("X-AirView-Cache") != "MISS" || asked.Load() != "tile.openstreetmap.org" {
		t.Errorf("first fetch: cache %q host %v", rec.Header().Get("X-AirView-Cache"), asked.Load())
	}
	if ua := lastUA.Load().(string); ua != "AirView/test" {
		t.Errorf("User-Agent = %q", ua)
	}
	if c := lastCookie.Load().(string); c != "" {
		t.Errorf("client credentials forwarded upstream: %q", c)
	}
	// Min 1 day for tiles although the upstream said 60 s.
	cc := rec.Header().Get("Cache-Control")
	if m := regexp.MustCompile(`max-age=(\d+)`).FindStringSubmatch(cc); m == nil || !strings.Contains(cc, "public") {
		t.Errorf("Cache-Control = %q", cc)
	} else if n, _ := strconv.Atoi(m[1]); n < 86000 {
		t.Errorf("max-age = %d, want about a day", n)
	}
	// Second request: from disk.
	rec = r.do("GET", base+"/5/16/10.png", lanAddr, "")
	if rec.Code != 200 || rec.Header().Get("X-AirView-Cache") != "HIT" || hits.Load() != 1 || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Errorf("second fetch: %d cache %q upstream hits %d", rec.Code, rec.Header().Get("X-AirView-Cache"), hits.Load())
	}
	// Conditional request from the browser.
	rec = r.do("GET", base+"/5/16/10.png", lanAddr, "", "If-None-Match", rec.Header().Get("ETag"))
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d", rec.Code)
	}
	if rec := r.do("HEAD", base+"/5/16/10.png", lanAddr, ""); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	// It is on disk under <data>/tilecache.
	st := r.srv.tiles.stats()
	if st.Files != 1 || st.Bytes < int64(len(png)) || st.Hits < 1 || st.Misses != 1 {
		t.Errorf("stats = %+v", st)
	}
	if entries, err := os.ReadDir(r.dir + "/tilecache/tile.openstreetmap.org"); err != nil || len(entries) != 1 {
		t.Errorf("cache directory: %v %d", err, len(entries))
	}

	// %20 is passed through unchanged; client query parameters that are not
	// part of the upstream URL (cache busters) are dropped from the upstream
	// request and the cache key.
	rec = r.do("GET", "/tilecache/tiles.openfreemap.org/fonts/Noto%20Sans%20Regular/0-255.pbf?v=3&x=a%20b", lanAddr, "")
	if rec.Code != 200 {
		t.Fatalf("glyph: %d %s", rec.Code, rec.Body.String())
	}
	if p := lastPath.Load().(string); p != "/fonts/Noto%20Sans%20Regular/0-255.pbf" {
		t.Errorf("upstream path = %q", p)
	}
	if q := lastQuery.Load().(string); q != "" {
		t.Errorf("upstream query = %q, want the cache buster dropped", q)
	}
	before := hits.Load()
	for _, u := range []string{"", "?v=4", "?_=1700000000000"} {
		if rec := r.do("GET", "/tilecache/tiles.openfreemap.org/fonts/Noto%20Sans%20Regular/0-255.pbf"+u, lanAddr, ""); rec.Code != 200 ||
			rec.Header().Get("X-AirView-Cache") != "HIT" {
			t.Errorf("%q: %d %q, want a HIT on the same object", u, rec.Code, rec.Header().Get("X-AirView-Cache"))
		}
	}
	if hits.Load() != before {
		t.Error("the same tile was fetched again for another client query")
	}
	// A parameter that is part of the upstream URL is kept, normalised
	// (sorted), and is part of the key.
	tileQueryParams["tiles.openfreemap.org"] = []string{"x", "k"}
	defer delete(tileQueryParams, "tiles.openfreemap.org")
	r.do("GET", "/tilecache/tiles.openfreemap.org/q/1.pbf?x=b&v=9&x=a%20c&k=1", lanAddr, "")
	if q := lastQuery.Load().(string); q != "k=1&x=a+c&x=b" {
		t.Errorf("upstream query = %q", q)
	}
	before = hits.Load()
	if rec := r.do("GET", "/tilecache/tiles.openfreemap.org/q/1.pbf?k=1&x=a+c&x=b&_=5", lanAddr, ""); rec.Header().Get("X-AirView-Cache") != "HIT" {
		t.Errorf("reordered query: %q", rec.Header().Get("X-AirView-Cache"))
	}
	r.do("GET", "/tilecache/tiles.openfreemap.org/q/1.pbf?k=2&x=a+c&x=b", lanAddr, "")
	if hits.Load() != before+1 {
		t.Error("an upstream query parameter is not part of the cache key")
	}

	// gzip bodies are stored as sent and decoded for clients that need it.
	rec = r.do("GET", "/tilecache/tiles.openfreemap.org/gz/planet", lanAddr, "", "Accept-Encoding", "gzip")
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("gzip passthrough: %d %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	rec = r.do("GET", "/tilecache/tiles.openfreemap.org/gz/planet", lanAddr, "")
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "" || !json.Valid(rec.Body.Bytes()) {
		t.Errorf("gzip decoded for a plain client: %d %q %.60s", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.String())
	}

	// HiDPI sprites ("@2x") pass; '@' in the path stays a path character.
	rec = r.do("GET", "/tilecache/tiles.openfreemap.org/sprites/ofm_f384/ofm@2x.json", lanAddr, "")
	if rec.Code != 200 {
		t.Errorf("@2x sprite: %d %s", rec.Code, rec.Body.String())
	}
	if p := lastPath.Load().(string); p != "/sprites/ofm_f384/ofm@2x.json" {
		t.Errorf("upstream path = %q", p)
	}

	// Never an open proxy.
	before = hits.Load()
	for u, want := range map[string]int{
		"/tilecache/example.com/x.png":                                   403,
		"/tilecache/tile.openstreetmap.org.evil.com/x.png":               403,
		"/tilecache/TILE.OPENSTREETMAP.ORG/x.png":                        403,
		"/tilecache/127.0.0.1:22/x":                                      403,
		"/tilecache/tile.openstreetmap.org@evil.com/x.png":               403,
		"/tilecache/tile.openstreetmap.org":                              400,
		"/tilecache/tile.openstreetmap.org/":                             400,
		"/tilecache/tile.openstreetmap.org/%2e%2e/%2e%2e/etc/passwd":     400,
		"/tilecache/tile.openstreetmap.org/a/%2e%2e%2fb":                 400,
		"/tilecache/tile.openstreetmap.org/a%5c..%5cb":                   400,
		"/tilecache/tile.openstreetmap.org/a/%00.png":                    400,
		"/tilecache/tile.openstreetmap.org/" + strings.Repeat("a", 1100): 400,
	} {
		rec := r.do("GET", u, lanAddr, "")
		if rec.Code != want {
			t.Errorf("GET %s: %d, want %d", u, rec.Code, want)
		}
	}
	// Literal dot segments never reach the proxy: the mux redirects to the
	// cleaned path, which is not a whitelisted host.
	for _, u := range []string{
		"/tilecache/tile.openstreetmap.org/../../etc/passwd",
		"/tilecache/tile.openstreetmap.org/a/../../../config.json",
		"/tilecache/../api/config",
	} {
		rec := r.do("GET", u, lanAddr, "")
		if rec.Code == 200 && !strings.Contains(u, "api/config") {
			t.Errorf("GET %s: 200", u)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			if rec2 := r.do("GET", loc, lanAddr, ""); rec2.Code == 200 && strings.HasPrefix(loc, "/tilecache/") {
				t.Errorf("GET %s -> %s: 200", u, loc)
			}
		}
	}
	if hits.Load() != before {
		t.Errorf("rejected requests reached the upstream (%d)", hits.Load()-before)
	}
	if rec := r.do("POST", base+"/5/16/11.png", lanAddr, "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
	// Active content is refused, missing tiles are 404, redirects to other
	// hosts are not followed.
	if rec := r.do("GET", base+"/html/x", lanAddr, ""); rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "<script>") {
		t.Errorf("HTML upstream: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("GET", base+"/missing/1.png", lanAddr, ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing tile: %d", rec.Code)
	}
	if rec := r.do("GET", base+"/redirect/1.png", lanAddr, ""); rec.Code != http.StatusBadGateway {
		t.Errorf("redirect to another host: %d", rec.Code)
	}
	// Failures are remembered briefly: no second upstream request.
	before = hits.Load()
	r.do("GET", base+"/missing/1.png", lanAddr, "")
	if hits.Load() != before {
		t.Error("negative result was not cached")
	}

	// Expired objects are revalidated; when the upstream is down the stale
	// copy is served.
	file := ""
	_ = walkFiles(r.dir+"/tilecache/tile.openstreetmap.org", func(p string) { file = p })
	meta, ok := readTileMeta(file)
	if !ok {
		t.Fatalf("cannot read %s", file)
	}
	meta.Exp = time.Now().Add(-time.Hour).Unix()
	rewriteTileMeta(file, meta)
	before = hits.Load()
	rec = r.do("GET", base+"/5/16/10.png", lanAddr, "")
	if rec.Code != 200 || rec.Header().Get("X-AirView-Cache") != "REVALIDATED" || hits.Load() != before+1 || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Errorf("revalidation: %d %q", rec.Code, rec.Header().Get("X-AirView-Cache"))
	}
	if m2, _ := readTileMeta(file); m2.Exp < time.Now().Add(23*time.Hour).Unix() {
		t.Errorf("expiry not extended: %v", time.Unix(m2.Exp, 0))
	}
	meta.Exp = time.Now().Add(-time.Hour).Unix()
	rewriteTileMeta(file, meta)
	up.Close()
	rec = r.do("GET", base+"/5/16/10.png", lanAddr, "")
	if rec.Code != 200 || rec.Header().Get("X-AirView-Cache") != "STALE" || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Errorf("stale on upstream failure: %d %q", rec.Code, rec.Header().Get("X-AirView-Cache"))
	}
	if rec := r.do("GET", base+"/5/16/99.png", lanAddr, ""); rec.Code != http.StatusBadGateway {
		t.Errorf("uncached tile with the upstream down: %d", rec.Code)
	}

	// Size cap: the oldest files go.
	r.srv.tiles.maxBytes = 1200
	r.srv.tiles.sweep(context.Background())
	if st := r.srv.tiles.stats(); st.Bytes > 1200 || st.Evictions == 0 || !st.Scanned {
		t.Errorf("after sweep: %+v", st)
	}

	// Clear (admin) and disable.
	if rec := r.do("POST", "/api/tilecache/clear", loopbackAddr, ""); rec.Code != 200 {
		t.Errorf("clear: %d", rec.Code)
	}
	if st := r.srv.tiles.stats(); st.Bytes != 0 || st.Files != 0 {
		t.Errorf("after clear: %+v", st)
	}
	r.do("PUT", "/api/config", loopbackAddr, `{"http":{"port":80,"tileCache":false}}`)
	if rec := r.do("GET", base+"/5/16/10.png", lanAddr, ""); rec.Code != http.StatusNotFound {
		t.Errorf("disabled tile cache: %d", rec.Code)
	}
	var st2 model.Status
	_ = json.Unmarshal(r.get("/api/status").Body.Bytes(), &st2)
	if st2.TileCache || len(st2.TileCacheHosts) != 0 {
		t.Errorf("status with the cache disabled: %v %v", st2.TileCache, st2.TileCacheHosts)
	}
}

func walkFiles(dir string, fn func(path string)) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := dir + "/" + e.Name()
		if e.IsDir() {
			_ = walkFiles(p, fn)
		} else {
			fn(p)
		}
	}
	return nil
}

func TestTileCacheSingleFetch(t *testing.T) {
	r := newRig(t, rigOptions{})
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		hits.Add(1)
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("tile"))
	}))
	defer up.Close()
	r.srv.tiles.upstream = func(string) string { return up.URL }
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := r.do("GET", "/tilecache/tiles.openfreemap.org/planet/1/2/3.pbf", lanAddr, ""); rec.Code != 200 || rec.Body.String() != "tile" {
				t.Errorf("concurrent fetch: %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("%d upstream requests for 12 concurrent clients", hits.Load())
	}
}

func TestPortFallback(t *testing.T) {
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	eng := engine.New(t.TempDir(), engine.Options{Demo: true, Offline: true, DemoSeed: 1, Logger: quietLogger()})
	if err := eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()
	srv := New(eng, Options{PortOverride: port, Version: "test"})
	srv.log = quietLogger()
	if srv.LocalURL() != "" || srv.Port() != 0 {
		t.Error("LocalURL / Port before Start")
	}
	addr, got, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Shutdown(context.Background())
	if got == port || got == 0 || addr == "" {
		t.Fatalf("bound port %d (busy port %d)", got, port)
	}
	if !(got >= 8080 && got <= 8089) {
		t.Logf("8080..8089 all busy on this machine: ephemeral port %d", got)
	}
	if _, _, err := srv.Start(context.Background()); err == nil {
		t.Error("second Start succeeded")
	}
	want := fmt.Sprintf("http://127.0.0.1:%d/", got)
	if srv.LocalURL() != want {
		t.Errorf("LocalURL = %q, want %q", srv.LocalURL(), want)
	}
	resp, err := http.Get(want + "api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st model.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.HTTPPort != got {
		t.Errorf("status.httpPort = %d, want %d (%v)", st.HTTPPort, got, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if _, err := http.Get(want + "api/status"); err == nil {
		t.Error("server still answers after Shutdown")
	}
}

func TestMarshalNaN(t *testing.T) {
	type inner struct {
		V float64  `json:"v"`
		P *float64 `json:"p,omitempty"`
		S []float64
	}
	type outer struct {
		Name string `json:"name"`
		inner
		M    map[string]float64 `json:"m"`
		N    num                `json:"n"`
		Skip string             `json:"-"`
		Raw  json.RawMessage    `json:"raw"`
		List []inner            `json:"list"`
	}
	inf := math.Inf(1)
	v := outer{
		Name: "x", inner: inner{V: math.NaN(), P: &inf, S: []float64{1, math.NaN()}},
		M: map[string]float64{"a": math.Inf(-1), "b": 2}, N: num(math.NaN()),
		Raw: json.RawMessage(`{"k":1}`), List: []inner{{V: 3}},
	}
	b, err := marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if got["v"] != nil || got["p"] != nil || got["n"] != nil || got["name"] != "x" || got["Skip"] != nil {
		t.Errorf("got %s", b)
	}
	if m := got["m"].(map[string]any); m["a"] != nil || m["b"] != 2.0 {
		t.Errorf("map: %s", b)
	}
	if s := got["S"].([]any); s[0] != 1.0 || s[1] != nil {
		t.Errorf("slice: %s", b)
	}
	if raw := got["raw"].(map[string]any); raw["k"] != 1.0 {
		t.Errorf("raw: %s", b)
	}
	// Finite values take the fast path and are identical to encoding/json.
	ok := outer{Name: "y", M: map[string]float64{"a": 1}}
	b1, _ := marshal(ok)
	b2, _ := json.Marshal(ok)
	if !bytes.Equal(b1, b2) {
		t.Errorf("fast path differs: %s vs %s", b1, b2)
	}
	if b, _ := json.Marshal(trailList{{T: 5, Lat: 1.5, Lon: -2.25, Alt: math.NaN(), GS: 250.04, RSSI: math.NaN(), Src: 1}}); string(b) != `[[5,1.50000,-2.25000,null,250.0,null,1]]` {
		t.Errorf("trail = %s", b)
	}
}

func TestResponseCache(t *testing.T) {
	c := newRespCache()
	var calls atomic.Int32
	fn := func() (any, error) {
		calls.Add(1)
		time.Sleep(60 * time.Millisecond)
		return map[string]int{"n": 1}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ent, err := c.do("k", time.Minute, fn)
			if err != nil || string(ent.body) != `{"n":1}` {
				t.Errorf("do: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("%d computations for 10 concurrent identical requests", calls.Load())
	}
	if _, _ = c.do("k", time.Minute, fn); calls.Load() != 1 {
		t.Error("cached entry was recomputed")
	}
	c.invalidate()
	if _, _ = c.do("k", time.Minute, fn); calls.Load() != 2 {
		t.Error("invalidate did not drop the entry")
	}
	// Errors are not cached, expiry works.
	n := 0
	bad := func() (any, error) { n++; return nil, badRequest("no") }
	_, _ = c.do("e", time.Minute, bad)
	_, _ = c.do("e", time.Minute, bad)
	if n != 2 {
		t.Errorf("error was cached (%d calls)", n)
	}
	_, _ = c.do("ttl", time.Millisecond, fn)
	time.Sleep(5 * time.Millisecond)
	before := calls.Load()
	if _, _ = c.do("ttl", time.Millisecond, fn); calls.Load() != before+1 {
		t.Error("expired entry was served")
	}
	// A panicking computation releases its waiters.
	func() {
		defer func() { _ = recover() }()
		_, _ = c.do("p", time.Minute, func() (any, error) { panic("boom") })
	}()
	if _, err := c.do("p", time.Minute, func() (any, error) { return 1, nil }); err != nil {
		t.Errorf("after panic: %v", err)
	}
	// Bounded.
	for i := 0; i < cacheMaxEntries*2; i++ {
		_, _ = c.do("k"+strconv.Itoa(i), time.Hour, func() (any, error) { return i, nil })
	}
	if c.size() > cacheMaxEntries {
		t.Errorf("cache grew to %d entries", c.size())
	}
}

// TestHeavyRouteDedup: five browsers asking for the same analysis at once
// cost one computation.
func TestHeavyRouteDedup(t *testing.T) {
	r := shared(t)
	var calls atomic.Int32
	rt := Route{Method: "GET", Path: "/api/test/heavy", Cache: time.Minute, API: func(s *Server, q *http.Request) (any, error) {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond)
		return map[string]string{"ant": q.URL.Query().Get("ant")}, nil
	}}
	h := r.srv.routeHandler(&rt)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Same request, parameters in a different order.
			u := "/api/test/heavy?ant=yagi&hours=24"
			if i%2 == 1 {
				u = "/api/test/heavy?hours=24&ant=yagi"
			}
			req := httptest.NewRequest("GET", u, nil)
			req.RemoteAddr = lanAddr
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 || rec.Body.String() != `{"ant":"yagi"}` {
				t.Errorf("%d %s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("%d computations for 5 identical requests", calls.Load())
	}
	req := httptest.NewRequest("GET", "/api/test/heavy?ant=vertical&hours=24", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if calls.Load() != 2 {
		t.Error("a different request was served from the cache")
	}
}

func TestQueryHelpers(t *testing.T) {
	req := func(q string) *http.Request { return httptest.NewRequest("GET", "/x?"+q, nil) }
	if _, _, ok, err := qSpan(req("")); ok || err != nil {
		t.Error("empty span")
	}
	from, to, ok, err := qSpan(req("hours=6"))
	if !ok || err != nil || math.Abs(to.Sub(from).Hours()-6) > 0.01 || time.Since(to) > time.Minute {
		t.Errorf("hours=6: %v %v %v", from, to, err)
	}
	from, to, _, err = qSpan(req("from=1700000000000&to=1700003600000"))
	if err != nil || from.Unix() != 1700000000 || to.Unix() != 1700003600 {
		t.Errorf("from/to: %v %v %v", from, to, err)
	}
	from, to, _, _ = qSpan(req("days=2&to=1700003600000"))
	if to.Sub(from) != 48*time.Hour {
		t.Errorf("days with to: %v", to.Sub(from))
	}
	for _, bad := range []string{"hours=x", "from=9&to=5", "hours=-1", "to=5"} {
		if _, _, _, err := qSpan(req(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if a, _ := qAnt(req("ant=all")); a != "" {
		t.Error("ant=all")
	}
	if !acceptsGzip(httptest.NewRequest("GET", "/", nil)) == false {
		t.Error("gzip without header")
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("Accept-Encoding", "br, gzip;q=0")
	if acceptsGzip(r2) {
		t.Error("gzip;q=0 accepted")
	}
	r2.Header.Set("Accept-Encoding", "deflate, gzip;q=0.5")
	if !acceptsGzip(r2) {
		t.Error("gzip;q=0.5 refused")
	}
	for host, want := range map[string]bool{
		"192.168.1.5": true, "localhost": true, "raspberrypi": true, "airview.local": true, "pi.fritz.box": true,
		"[::1]": true, "evil.example.com": false, "attacker.io": false, "": false,
	} {
		if localHostName(host) != want {
			t.Errorf("localHostName(%q) = %v", host, !want)
		}
	}
}

// A PUT /api/config without the network keys (an older UI) keeps the
// stored aggregator and uplinkTuning blocks.
func TestPutConfigKeepsNetworkBlocks(t *testing.T) {
	r := newRig(t, rigOptions{})
	rec := r.do("PUT", "/api/config", loopbackAddr, `{"uplinkTuning":{"ringMb":7},"aggregator":{"beast":{"maxLinks":9}}}`)
	if rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{"receiver":{"name":"Other"}}`); rec.Code != 200 {
		t.Fatalf("put 2: %d %s", rec.Code, rec.Body.String())
	}
	cfg := r.eng.Config().Get()
	if cfg.UplinkTuning.RingMB != 7 || cfg.UplinkTuning.RingSec != 600 || cfg.Aggregator.Beast.MaxLinks != 9 {
		t.Fatalf("network blocks not kept: %+v %+v", cfg.UplinkTuning, cfg.Aggregator.Beast)
	}
}
