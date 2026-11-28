package server

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRouteAccessLint: every route declares its scope and station
// selector, consistently with its path (DESIGN §8.4 "a lint test fails if
// any route lacks one").
func TestRouteAccessLint(t *testing.T) {
	for i := range routes {
		rt := &routes[i]
		key := rt.Method + " " + rt.Path
		acc := routeAccess(rt)
		if acc == accessUnset {
			t.Errorf("%s: no Access declared", key)
			continue
		}
		// Only the account routes (another work package's file) may rely on
		// the group fallback.
		if rt.Access == accessUnset && rt.Group != grpAccounts {
			t.Errorf("%s: Access must be set explicitly", key)
		}
		p := rt.Path
		switch {
		case p == "/api/me/check":
			if acc != AccessPublic || !rt.NetOnly || rt.Cache > 0 {
				t.Errorf("%s: the feed check is public (IP-matched), aggregator-only and never cached", key)
			}
		case strings.HasPrefix(p, "/api/me/") || p == "/api/me" || strings.HasPrefix(p, "/data/me/"):
			if acc != AccessOwner || !rt.NetOnly {
				t.Errorf("%s: owner routes need AccessOwner and NetOnly (got %s)", key, acc)
			}
		case strings.HasPrefix(p, "/api/admin/"):
			if acc != AccessAdmin || !rt.NetOnly {
				t.Errorf("%s: admin routes need AccessAdmin and NetOnly (got %s)", key, acc)
			}
		case strings.HasPrefix(p, "/api/public/"), strings.HasPrefix(p, "/data/"), p == healthPath:
			if acc != AccessPublic || rt.NetOnly {
				t.Errorf("%s: public surfaces need AccessPublic in every mode (got %s)", key, acc)
			}
		case strings.HasPrefix(p, "/api/auth/"):
			if acc != AccessSession {
				t.Errorf("%s: account routes check their session (AccessSession), got %s", key, acc)
			}
		}
		hasSID := strings.Contains(p, "{sid}")
		if hasSID != (rt.Station == StationPath) {
			t.Errorf("%s: {sid} routes must use StationPath and only they", key)
		}
		if rt.Station == StationQuery {
			found := false
			for _, prm := range rt.Params {
				if prm.Name == "station" {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: StationQuery without a documented station parameter", key)
			}
		}
		if rt.Station != StationNone && acc != AccessOwner && acc != AccessAdmin && acc != AccessViewer {
			t.Errorf("%s: station selection needs owner, admin or per-viewer projection", key)
		}
		mutating := rt.Method != http.MethodGet && rt.Method != http.MethodHead
		// POST /api/events/view changes only the viewport of the caller's own
		// live stream, named by its 128-bit random token (sse_view.go).
		if mutating && (acc == AccessPublic || acc == AccessViewer) && rt.Path != "/api/login" && rt.Path != "/api/logout" && rt.Path != "/api/events/view" {
			t.Errorf("%s: a mutating route cannot be public", key)
		}
		// Legacy single-station routes that are open in aggregator mode
		// must be on the reviewed list: anything else is admin-only.
		if !rt.NetOnly && !strings.HasPrefix(p, "/api/public/") && !strings.HasPrefix(p, "/data/") && p != healthPath &&
			!strings.HasPrefix(p, "/api/auth/") && (acc == AccessPublic || acc == AccessViewer) && reviewedOpen[key] == "" {
			t.Errorf("%s: open legacy route not on the reviewed list (reviewedOpen)", key)
		}
	}
	for k := range reviewedOpen {
		found := false
		for _, rt := range routes {
			if rt.Method+" "+rt.Path == k {
				found = true
			}
		}
		if !found {
			t.Errorf("reviewedOpen names the unknown route %s", k)
		}
	}
}

// reviewedOpen lists the legacy routes that public viewers may call in
// aggregator mode, with the reason. AccessViewer routes project their
// answer per viewer (api_view.go); AccessPublic routes contain no station
// data at all.
var reviewedOpen = map[string]string{
	"GET /api/routes/board/{airport}":      "viewer: built from the caller's projection of the live picture; landed legs only for the station's own users",
	"GET /api/routes/airports":             "public: airport reference data only",
	"GET /api/routes/batch":                "viewer: route shorts from the caller's projection of the live picture",
	"GET /api/routes/stats":                "viewer: 403 below owner on an aggregator",
	"GET /api/routes/{callsign}":           "public: offline data and the cache only; online look-ups admin only; restricted answers hidden below owner",
	"GET /api/status":                      "viewer: publicdto.Status below admin",
	"GET /api/config":                      "viewer: display settings only below admin",
	"POST /api/login":                      "public: login",
	"POST /api/logout":                     "public: logout",
	"GET /api/session":                     "public: own session state",
	"GET /api/aircraft":                    "viewer: publicdto / owner projection",
	"GET /api/aircraft/{icao}":             "viewer: public detail below admin",
	"GET /api/trails":                      "viewer: trails without signal below admin",
	"GET /api/events":                      "viewer: per-viewer stream",
	"POST /api/events/view":                "public: viewport of the caller's own stream (random token)",
	"GET /api/notices":                     "viewer: aircraft notices only below admin",
	"GET /api/geo/parse":                   "public: parses the given text",
	"GET /api/geo/format":                  "public: formats the given position",
	"GET /api/geo/magvar":                  "public: model at the given position",
	"GET /api/refdata/icao/{icao}":         "public: reference tables",
	"GET /api/refdata/callsign/{callsign}": "public: reference tables",
	"GET /api/refdata/type/{code}":         "public: reference tables",
	"GET /api/refdata/squawk/{code}":       "public: reference tables",
	"GET /api/navdata/airports":            "viewer: needs bbox or lat/lon below admin (no receiver default)",
	"GET /api/navdata/search":              "public: airport search",
	"GET /api/navdata/navaids":             "public: bbox required",
	"GET /api/navdata/airport/{code}":      "public: airport data",
	"GET /api/navdata/route/{callsign}":    "public: route lookup with the given position",
	"GET /api/navdata/photo/{icao}":        "public: aircraft photo",
	"GET /api/alerts/placeholders":         "public: static list",
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter()
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	spec := rateSpec{rps: 1, burst: 3}
	for i := 0; i < 3; i++ {
		if _, ok := l.allow("a", spec); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	wait, ok := l.allow("a", spec)
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("4th request: ok=%v wait=%v", ok, wait)
	}
	if _, ok := l.allow("b", spec); !ok {
		t.Fatal("other client limited")
	}
	now = now.Add(1500 * time.Millisecond)
	if _, ok := l.allow("a", spec); !ok {
		t.Fatal("token not refilled")
	}
	if _, ok := l.allow("a", spec); ok {
		t.Fatal("half a token was enough")
	}
	if _, ok := l.allow("x", rateSpec{}); !ok {
		t.Fatal("rps 0 must disable the limit")
	}
	// Idle buckets are swept once the map is large.
	for i := 0; i < 5000; i++ {
		l.allow("k"+string(rune('a'+i%26))+time.Duration(i).String(), spec)
	}
	now = now.Add(time.Hour)
	l.allow("new", spec)
	if n := l.size(); n > 10 {
		t.Errorf("idle buckets not swept: %d", n)
	}
	if retryAfter(10*time.Millisecond) != "1" || retryAfter(2500*time.Millisecond) != "3" {
		t.Error("retryAfter")
	}
}

// BenchmarkRateLimiter: one public request's limiter check with 10,000
// active client addresses.
func BenchmarkRateLimiter(b *testing.B) {
	l := newRateLimiter()
	spec := rateSpec{rps: 1e9, burst: 1e9}
	keys := make([]string, 10000)
	for i := range keys {
		keys[i] = "198.51.100." + strconv.Itoa(i)
		l.allow(keys[i], spec)
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		l.allow(keys[i%len(keys)], spec)
		i++
	}
}
