package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %T: %v: %.300s", v, err, body)
	}
	return v
}

// TestExtAdmin covers the terrain, navdata and photo routes the generic
// test skips.
func TestExtAdmin(t *testing.T) {
	r := newRig(t, rigOptions{})
	if rec := r.do("POST", "/api/terrain/prefetch", loopbackAddr, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"job"`) {
		t.Errorf("prefetch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("POST", "/api/terrain/recompute", loopbackAddr, ""); rec.Code != http.StatusAccepted {
		t.Errorf("recompute: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("POST", "/api/terrain/cancel", loopbackAddr, ""); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("cancel: %d %v", rec.Code, rec.Header())
	}
	// Wait for the (re-)analysis, then fetch the floor image.
	deadline := time.Now().Add(60 * time.Second)
	for r.eng.Terrain().Analysis() == nil || r.eng.Terrain().Status().State == "running" {
		if time.Now().After(deadline) {
			t.Fatal("terrain analysis did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	img := decode[struct {
		URL string `json:"url"`
	}](t, r.get("/api/terrain/floor-image?w=128").Body.Bytes())
	if rec := r.get(img.URL); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || rec.Body.Len() < 100 {
		t.Errorf("floor.png: %d %q %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	// Disabled photo look-up.
	if rec := r.get("/api/navdata/photo/3C6444"); rec.Code != http.StatusConflict {
		t.Errorf("photo: %d %s", rec.Code, rec.Body.String())
	}
	// An import of an unknown dataset fails at once, without the network.
	if rec := r.do("POST", "/api/navdata/import", loopbackAddr, `{"datasets":["nosuch"]}`); rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		p := decode[struct {
			Running bool   `json:"running"`
			Error   string `json:"error"`
		}](t, r.get("/api/navdata/progress").Body.Bytes())
		if !p.Running {
			if p.Error == "" {
				t.Error("import of an unknown dataset did not fail")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("import did not end")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rec := r.do("POST", "/api/navdata/cancel", loopbackAddr, ""); rec.Code != 200 {
		t.Errorf("cancel import: %d", rec.Code)
	}
	if rec := r.do("POST", "/api/navdata/route-cache/clear", loopbackAddr, ""); rec.Code != 200 {
		t.Errorf("route cache: %d %s", rec.Code, rec.Body.String())
	}
	// Admin routes refuse a LAN client once a password is set.
	cfg := r.eng.Config().Get()
	cfg.HTTP.AdminPasswordHash = "plain:secret"
	if _, _, err := r.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"/api/terrain/prefetch", "/api/navdata/import", "/api/alerts/rules"} {
		if rec := r.do("POST", u, lanAddr, `{}`, "Host", "192.168.1.10:8080"); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s from the LAN: %d", u, rec.Code)
		}
	}
}

// TestAlertsAPI covers the alert rule and watch-list endpoints.
func TestAlertsAPI(t *testing.T) {
	r := newRig(t, rigOptions{})
	rule := `{"name":"Low and close","enabled":true,"severity":"warning","match":{"op":"and","conds":[{"type":"distance","max":30},{"type":"altitude","max":5000}]},"cooldownSec":600,"actions":{"notify":true,"highlight":"#00ff88"}}`
	rec := r.do("POST", "/api/alerts/rules", loopbackAddr, rule)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	created := decode[struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}](t, rec.Body.Bytes())
	if !strings.HasPrefix(created.ID, "r-") {
		t.Fatalf("id %q", created.ID)
	}
	if rec := r.get("/api/alerts/rules/" + created.ID); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Low and close") {
		t.Errorf("get: %d %s", rec.Code, rec.Body.String())
	}
	upd := strings.Replace(rule, "Low and close", "Low and very close", 1)
	if rec := r.do("PUT", "/api/alerts/rules/"+created.ID, loopbackAddr, upd); rec.Code != 200 || !strings.Contains(rec.Body.String(), "very close") {
		t.Errorf("put: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("PUT", "/api/alerts/rules/r-nosuch", loopbackAddr, upd); rec.Code != 404 {
		t.Errorf("put unknown: %d", rec.Code)
	}
	if rec := r.do("POST", "/api/alerts/rules", loopbackAddr, `{"name":"bad","match":{"type":"nosuch"}}`); rec.Code != 400 {
		t.Errorf("invalid rule: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("POST", "/api/alerts/rules/"+created.ID+"/enabled", loopbackAddr, `{"enabled":false}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("enabled: %d %s", rec.Code, rec.Body.String())
	}
	r.waitTraffic(2)
	rec = r.do("POST", "/api/alerts/test", loopbackAddr, `{"rule":{"name":"t","enabled":true,"severity":"info","match":{"type":"distance","max":5000},"actions":{}}}`)
	if rec.Code != 200 {
		t.Fatalf("test: %d %s", rec.Code, rec.Body.String())
	}
	tr := decode[struct {
		Aircraft int               `json:"aircraft"`
		Matches  []json.RawMessage `json:"matches"`
	}](t, rec.Body.Bytes())
	if tr.Aircraft == 0 || len(tr.Matches) == 0 {
		t.Errorf("test matched nothing: %+v", tr)
	}
	exp := r.get("/api/alerts/export")
	if exp.Code != 200 || !strings.Contains(exp.Body.String(), created.ID) {
		t.Fatalf("export: %d", exp.Code)
	}
	if rec := r.do("POST", "/api/alerts/import", loopbackAddr, exp.Body.String()); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"imported"`) {
		t.Errorf("import: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("POST", "/api/alerts/webhook-test", loopbackAddr, `{"url":"ftp://x"}`); rec.Code != 400 {
		t.Errorf("webhook-test invalid url: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("DELETE", "/api/alerts/rules/"+created.ID, loopbackAddr, ""); rec.Code != 200 {
		t.Errorf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.get("/api/alerts/rules/" + created.ID); rec.Code != 404 {
		t.Errorf("deleted rule still there: %d", rec.Code)
	}
	if rec := r.do("POST", "/api/alerts/rules/reset-defaults", loopbackAddr, ""); rec.Code != 200 {
		t.Errorf("reset-defaults: %d", rec.Code)
	}

	// Watch-lists.
	rec = r.do("POST", "/api/alerts/watchlists/vips/import", loopbackAddr, "3C6444 Lufthansa\n# comment\n4CA2D1,Ryanair\nnot-hex\n", "Content-Type", "text/plain")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"imported":2`) {
		t.Fatalf("watch-list import: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do("PUT", "/api/alerts/watchlists/vips/400A0B", loopbackAddr, `{"note":"BA"}`); rec.Code != 200 {
		t.Errorf("add: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.get("/api/alerts/watchlists/vips"); rec.Code != 200 || strings.Count(rec.Body.String(), `"icao"`) != 3 {
		t.Errorf("list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.get("/api/alerts/watchlists/vips?format=csv"); rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
		t.Errorf("csv: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := r.get("/api/aircraft/3C6444"); !strings.Contains(rec.Body.String(), `"watch":{"list":"vips"`) {
		t.Errorf("aircraft detail without watch info: %.300s", rec.Body.String())
	}
	if rec := r.do("DELETE", "/api/alerts/watchlists/vips/400A0B", loopbackAddr, ""); rec.Code != 200 {
		t.Errorf("remove: %d", rec.Code)
	}
	if rec := r.do("DELETE", "/api/alerts/watchlists/vips", loopbackAddr, ""); rec.Code != 200 {
		t.Errorf("delete list: %d", rec.Code)
	}
	if rec := r.get("/api/alerts/watchlists/vips"); rec.Code != 404 {
		t.Errorf("deleted list: %d", rec.Code)
	}
}

// TestAlertsSSE: a firing rule reaches the live stream as an "alert" event,
// marks the aircraft in the live picture and (user rule) sends a notice.
func TestAlertsSSE(t *testing.T) {
	r := newRig(t, rigOptions{listen: true})
	r.waitTraffic(3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(r.srv.LocalURL(), "/")+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rule := `{"name":"Everything nearby","enabled":true,"severity":"warning","match":{"type":"distance","max":5000},"cooldownSec":3600,"actions":{"notify":true,"sound":true,"highlight":"#00ff88"}}`
	if rec := r.do("POST", "/api/alerts/rules", loopbackAddr, rule); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var event string
	gotAlert, gotNotice, gotMark := false, false, false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() && !(gotAlert && gotNotice && gotMark) {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := []byte(strings.TrimPrefix(line, "data: "))
			switch event {
			case "alert":
				var a struct {
					RuleName string `json:"ruleName"`
					Severity string `json:"severity"`
					ICAO     string `json:"icao"`
					Text     string `json:"text"`
					Sound    bool   `json:"sound"`
				}
				if err := json.Unmarshal(data, &a); err != nil {
					t.Fatal(err)
				}
				if a.RuleName == "Everything nearby" && a.ICAO != "" && a.Text != "" && a.Sound && a.Severity == "warning" {
					gotAlert = true
				}
			case "notice":
				if strings.Contains(string(data), `"kind":"alert"`) {
					gotNotice = true
				}
			case "aircraft":
				var s model.LiveSnapshot
				if err := json.Unmarshal(data, &s); err != nil {
					t.Fatal(err)
				}
				for _, a := range s.Aircraft {
					if a.Highlight != nil && a.Highlight.Color == "#00ff88" && a.Highlight.Label == "Everything nearby" {
						gotMark = true
					}
				}
			}
		}
	}
	if !gotAlert || !gotNotice || !gotMark {
		t.Fatalf("alert event %v, alert notice %v, highlight %v (%v)", gotAlert, gotNotice, gotMark, sc.Err())
	}
	if rec := r.get("/api/alerts/recent?limit=5"); !strings.Contains(rec.Body.String(), "Everything nearby") {
		t.Errorf("recent: %.300s", rec.Body.String())
	}
}

// TestLiveEnrichment: the live picture carries terrain visibility, closest
// approach and (from the seeded standing data) no bogus fields; the counts
// separate predicted aircraft.
func TestLiveEnrichment(t *testing.T) {
	r := shared(t)
	deadline := time.Now().Add(15 * time.Second)
	for {
		s := r.eng.Tracker().Snapshot()
		vis, cpa := 0, 0
		for _, a := range s.Aircraft {
			if a.TerrainVisible != nil && a.LOSMarginDeg != nil {
				vis++
			}
			if a.CPAKm != nil && a.CPAInS != nil {
				cpa++
			}
			if a.Predicted && (a.CoastS == nil || a.LastRealLat == nil || a.PredSigmaM == nil) {
				t.Errorf("predicted aircraft %s without coasting fields", a.ICAO)
			}
		}
		if vis > 0 && cpa > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no enrichment: terrain %d, cpa %d of %d aircraft", vis, cpa, len(s.Aircraft))
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The route of a seeded callsign resolves from the standing data.
	if rec := r.get("/api/navdata/route/DLH400?lat=50.2&lon=7.9"); !strings.Contains(rec.Body.String(), `"found":true`) ||
		!strings.Contains(rec.Body.String(), "KJFK") {
		t.Errorf("route: %.400s", rec.Body.String())
	}
	if rec := r.get("/api/navdata/airports?bbox=49,7,51,10&runways=1&format=geojson"); !strings.Contains(rec.Body.String(), "EDDF") ||
		!strings.Contains(rec.Body.String(), `"runway"`) {
		t.Errorf("airport overlay: %.300s", rec.Body.String())
	}
}

// TestReplayAndReports checks the replay frames and report filters on the
// seeded history.
func TestReplayAndReports(t *testing.T) {
	r := shared(t)
	rp := decode[struct {
		Step     int `json:"step"`
		Aircraft []struct {
			ICAO string `json:"icao"`
		} `json:"aircraft"`
		Frames []struct {
			T int64           `json:"t"`
			P [][]json.Number `json:"p"`
		} `json:"frames"`
	}](t, r.get("/api/replay?hours=2&step=60").Body.Bytes())
	if rp.Step != 60 || len(rp.Frames) < 100 || len(rp.Aircraft) == 0 {
		t.Fatalf("replay: step %d, %d frames, %d aircraft", rp.Step, len(rp.Frames), len(rp.Aircraft))
	}
	points := 0
	for _, f := range rp.Frames {
		for _, p := range f.P {
			if len(p) != 8 {
				t.Fatalf("frame row %v", p)
			}
			i, _ := p[0].Int64()
			if int(i) >= len(rp.Aircraft) {
				t.Fatalf("index %d out of range", i)
			}
			points++
		}
	}
	if points == 0 {
		t.Fatal("replay frames are empty")
	}
	if rec := r.get("/api/replay?hours=48"); rec.Code != 400 {
		t.Errorf("48 h replay: %d", rec.Code)
	}
	rep := decode[struct {
		Total int `json:"total"`
		Rows  []struct {
			Callsign string `json:"callsign"`
			ICAO     string `json:"icao"`
		} `json:"rows"`
	}](t, r.get("/api/reports?hours=6&limit=5&sort=maxDistKm").Body.Bytes())
	if rep.Total == 0 || len(rep.Rows) == 0 {
		t.Fatalf("report: %+v", rep)
	}
	icao := rep.Rows[0].ICAO
	one := decode[struct {
		Total int `json:"total"`
	}](t, r.get("/api/reports?hours=6&icao="+icao).Body.Bytes())
	if one.Total < 1 || one.Total > rep.Total {
		t.Errorf("filter icao=%s: %d of %d", icao, one.Total, rep.Total)
	}
	if rec := r.get("/api/reports?sort=evil"); rec.Code != 400 {
		t.Errorf("bad sort: %d", rec.Code)
	}
	for _, ext := range []string{"kml", "csv"} {
		u := sampleURL(t, r, Route{Sample: "/api/export/flight/{flight}." + ext})
		if rec := r.get(u); rec.Code != 200 || checkBody(rec.Header().Get("Content-Type"), rec.Body.Bytes()) != nil ||
			!strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: %d %q", u, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := r.get("/api/export/flight/1.txt"); rec.Code != 404 {
		t.Errorf("unknown export format: %d", rec.Code)
	}
}
