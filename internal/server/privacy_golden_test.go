package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Golden privacy tests (DESIGN §8.4): crawl every route, the live streams
// and the /data/*.json endpoints over the fixture (known exact station
// coordinates, addresses, SIDs, UUIDs, feeder IDs) and fail on any leak.
//
// The demo tracker of the fixture is not in network mode, so the raw live
// picture carries distances, bearings, RSSI, feeds and antennas of the demo
// receiver: the projections, not the tracker, must remove them.

func TestGoldenPublicCrawl(t *testing.T) {
	f := newNetFixture(t, rigOptions{listen: true})
	f.withMLAT()
	f.waitTraffic(3)
	f.eng.Notify("info", "operator notice")
	fx := f.publicFixture()
	sidA := f.stations["A"].st.SID
	crawled := 0
	for _, rt := range Routes() {
		if rt.Method != http.MethodGet || rt.Raw != nil && strings.HasSuffix(rt.Path, "/events") {
			continue
		}
		u := f.netSample(rt, sidA)
		rec := f.anon("GET", u, "192.0.2.10:4000", "")
		name := "anonymous GET " + u
		acc := routeAccess(&rt)
		switch acc {
		case AccessOwner, AccessAdmin:
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status %d, want 401 (%s route)", name, rec.Code, acc)
			}
		default:
			if rec.Code >= 500 {
				t.Errorf("%s: status %d: %.300s", name, rec.Code, rec.Body.String())
			}
		}
		scanOrFail(t, fx, name, crawlBody(u, rec.Body.Bytes()))
		crawled++
	}
	if crawled < 150 {
		t.Errorf("only %d routes crawled", crawled)
	}
	// The public routes really answer (the crawl must not pass by being
	// refused everywhere).
	for _, u := range []string{"/api/public/aircraft", "/api/aircraft", "/data/aircraft.json", "/api/public/stations",
		"/api/public/stats", "/api/public/summary", "/api/status", "/api/config", "/api/trails", "/api/public/trails", "/data/receiver.json", "/healthz"} {
		rec := f.anon("GET", u, "192.0.2.11:4000", "")
		must(t, rec, 200, u)
	}
	// A public station list: B's region pin, A only with stats (no SID).
	var list struct {
		Stations []struct {
			SID    string
			Name   string
			Online bool
			Pin    *struct{ Lat, Lon float64 }
			Stats  *struct{ MsgRate float64 }
		}
		Approved, Online int
	}
	jsonOf(t, f.anon("GET", "/api/public/stations", "192.0.2.12:4000", ""), &list)
	if list.Approved != 4 || len(list.Stations) != 2 {
		t.Fatalf("public stations: %+v", list)
	}
	for _, e := range list.Stations {
		switch {
		case e.SID == f.stations["B"].st.SID:
			if e.Pin == nil || e.Name != "" {
				t.Errorf("B entry %+v", e)
			}
		case e.SID == "" && e.Name == "Alice Home":
			if e.Pin != nil || e.Stats == nil || !e.Online {
				t.Errorf("A entry: %+v", e)
			}
		default:
			t.Errorf("unexpected entry %+v", e)
		}
	}
	// Live streams: the normal stream for a public viewer and the public
	// stream, for a few ticks each.
	for _, u := range []string{"/api/events", "/api/public/events"} {
		body := f.readSSE(t, u, "192.0.2.20:4000", 2500*time.Millisecond)
		if !strings.Contains(string(body), "event: aircraft") || !strings.Contains(string(body), "event: status") {
			t.Errorf("%s: no aircraft/status event: %.300s", u, body)
		}
		scanOrFail(t, fx, "anonymous stream "+u, body)
	}
}

func TestGoldenOwnerCrawl(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.withMLAT()
	f.waitTraffic(3)
	fx := f.ownerFixture("A")
	sidA, sidB := f.stations["A"].st.SID, f.stations["B"].st.SID
	for _, rt := range Routes() {
		if rt.Method != http.MethodGet || strings.HasSuffix(rt.Path, "/events") || rt.Path == "/api/me/export" {
			continue
		}
		acc := routeAccess(&rt)
		if acc == AccessAdmin {
			rec := f.as(f.alice, "GET", f.netSample(rt, sidA), "")
			if rec.Code != http.StatusForbidden {
				t.Errorf("owner GET %s: %d, want 403", rt.Path, rec.Code)
			}
			continue
		}
		u := f.netSample(rt, sidA)
		rec := f.as(f.alice, "GET", u, "")
		if rec.Code >= 500 {
			t.Errorf("owner GET %s: %d %.300s", u, rec.Code, rec.Body.String())
		}
		scanOrFail(t, fx, "owner GET "+u, crawlBody(u, rec.Body.Bytes()))
		// Another owner's station: 404, nothing leaks.
		if rt.Station == StationPath {
			u2 := f.netSample(rt, sidB)
			rec := f.as(f.alice, "GET", u2, "")
			if rec.Code != http.StatusNotFound {
				t.Errorf("owner GET foreign %s: %d", u2, rec.Code)
			}
			scanOrFail(t, fx, "owner GET foreign "+u2, rec.Body.Bytes())
		}
	}
	// The owner sees the own station exactly.
	rec := f.as(f.alice, "GET", "/api/me/stations/"+sidA, "")
	must(t, rec, 200, "own station")
	if !strings.Contains(rec.Body.String(), "-41.28713") {
		t.Errorf("own station without exact position: %.300s", rec.Body.String())
	}
	// Owner live stream.
	body := f.readSSE(t, "/api/events", f.alice.addr, 1500*time.Millisecond, "Cookie", f.alice.cookie)
	scanOrFail(t, fx, "owner stream", body)
	// The owner's export holds only the own station.
	rec = f.as(f.alice, "GET", "/api/me/export", "")
	must(t, rec, 200, "export")
	if !strings.HasPrefix(rec.Body.String(), "PK") {
		t.Errorf("export is not a zip")
	}
}

// Standalone mode is unchanged: the legacy routes answer as before and the
// aggregator-only routes do not exist.
func TestStandaloneNetRoutes(t *testing.T) {
	r := shared(t)
	r.waitTraffic(1)
	for _, rt := range Routes() {
		if !rt.NetOnly {
			continue
		}
		u := strings.NewReplacer("{sid}", "x", "{icao}", "3C6444", "{id}", "x", "{link}", "1").Replace(rt.Path)
		if rec := r.do(rt.Method, u, loopbackAddr, `{}`); rec.Code != http.StatusNotFound {
			t.Errorf("standalone %s %s: %d", rt.Method, u, rec.Code)
		}
	}
	// The legacy picture still carries the receiver fields in standalone.
	rec := r.get("/api/aircraft")
	if !strings.Contains(rec.Body.String(), `"distKm"`) || !strings.Contains(rec.Body.String(), `"feeds"`) {
		t.Errorf("standalone /api/aircraft lost receiver fields")
	}
	// The public projection exists in standalone too and has none.
	rec = r.get("/api/public/aircraft")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"distKm"`) || strings.Contains(rec.Body.String(), `"rssi"`) {
		t.Errorf("standalone /api/public/aircraft: %d %.200s", rec.Code, rec.Body.String())
	}
	if rec := r.get("/data/receiver.json"); !strings.Contains(rec.Body.String(), `"lat"`) {
		t.Errorf("standalone receiver.json without position: %s", rec.Body.String())
	}
}
