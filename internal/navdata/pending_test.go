package navdata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestOnlineCacheRidesGroupCommit: online route and photo answers are
// queued for the store's group commit; until then the overlay answers, and
// its answer equals the one read back from the committed row.
func TestOnlineCacheRidesGroupCommit(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/DLH400"):
			_, _ = w.Write([]byte(`{"response":{"flightroute":{"callsign":"DLH400","callsign_icao":"DLH400","callsign_iata":"LH400","airline":{"name":"Lufthansa","icao":"DLH","iata":"LH"},"origin":{"iata_code":"FRA","icao_code":"EDDF","latitude":50.03333,"longitude":8.57056,"name":"Frankfurt am Main Airport"},"destination":{"iata_code":"JFK","icao_code":"KJFK","latitude":40.6398,"longitude":-73.7789,"name":"John F Kennedy International Airport"}}}}`))
		case strings.HasSuffix(r.URL.Path, "/hex/3C6444"):
			_, _ = w.Write([]byte(`{"photos":[{"id":"1","thumbnail":{"src":"https://t.plnspttrs.net/1_t.jpg","size":{"width":200,"height":133}},"thumbnail_large":{"src":"https://t.plnspttrs.net/1_280.jpg","size":{"width":420,"height":280}},"link":"https://www.planespotters.net/photo/1/d-aibd","photographer":"Jane Doe"}]}`))
		case strings.Contains(r.URL.Path, "/hex/"):
			_, _ = w.Write([]byte(`{"photos":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"response":"unknown callsign"}`))
		}
	}))
	defer srv.Close()
	nd, _ := imported(t)
	ctx := context.Background()
	nd.SetOptions(Options{
		Routes: RouteOptions{Online: true, BaseURL: srv.URL + "/v0/callsign/", MinInterval: time.Millisecond},
		Photos: PhotoOptions{Enabled: true, Contact: "https://station.example.org/contact", BaseURL: srv.URL + "/pub/photos", MinInterval: time.Millisecond},
	})
	js := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	routes := []string{"DLH400", "NOPE1"}
	photos := []uint32{0x3C6444, 0x400000}
	for _, cs := range routes {
		if _, err := nd.Route(ctx, cs, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, icao := range photos {
		if _, err := nd.Photo(ctx, icao, ""); err != nil {
			t.Fatal(err)
		}
	}
	fetched := hits.Load()
	// Second lookups: from the overlay (or the row, if a flush already ran),
	// never online again.
	var fromOverlay []string
	for _, cs := range routes {
		ri, err := nd.Route(ctx, cs, nil)
		if err != nil || !ri.Cached {
			t.Fatalf("%s: cached=%v %v", cs, ri.Cached, err)
		}
		fromOverlay = append(fromOverlay, js(ri))
	}
	for _, icao := range photos {
		p, err := nd.Photo(ctx, icao, "")
		if err != nil || !p.Cached {
			t.Fatalf("%06X: cached=%v %v", icao, p.Cached, err)
		}
		fromOverlay = append(fromOverlay, js(p))
	}
	if hits.Load() != fetched {
		t.Fatalf("second lookups went online: %d requests, want %d", hits.Load(), fetched)
	}
	// Commit, drop the overlay: the rows answer the same.
	if err := nd.st.Flush(); err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = nd.st.DB().QueryRow(`SELECT (SELECT COUNT(*) FROM navdata_route_cache) + (SELECT COUNT(*) FROM navdata_photo)`).Scan(&rows)
	if rows != len(routes)+len(photos) {
		t.Fatalf("%d cache rows after the flush, want %d", rows, len(routes)+len(photos))
	}
	nd.pend = pendingCache{}
	var fromRows []string
	for _, cs := range routes {
		ri, _ := nd.Route(ctx, cs, nil)
		fromRows = append(fromRows, js(ri))
	}
	for _, icao := range photos {
		p, _ := nd.Photo(ctx, icao, "")
		fromRows = append(fromRows, js(p))
	}
	for i := range fromRows {
		if fromRows[i] != fromOverlay[i] {
			t.Errorf("answer %d:\n overlay %s\n row     %s", i, fromOverlay[i], fromRows[i])
		}
	}
	if hits.Load() != fetched {
		t.Fatalf("lookups after the flush went online")
	}
	// ClearRouteCache removes queued and committed answers.
	_, _ = nd.Route(ctx, "OTHER1", nil)
	if err := nd.ClearRouteCache(ctx); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	if _, err := nd.Route(ctx, "OTHER1", nil); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != before+1 {
		t.Fatal("cleared route answered from the cache")
	}
}
