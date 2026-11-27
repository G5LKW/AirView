package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"airview/internal/server"
)

// call is one GET request made to both instances.
type call struct {
	Key   string // route key "GET /api/x" (+ " [variant]")
	URL   string // path and query
	Kind  string // json | csv | kml | geojson | text | binary
	Route string // route path pattern
}

// skipRoutes are GET routes that are not compared, with the reason. Every
// other GET route of the station's route table is called.
var skipRoutes = map[string]string{
	"/api/events":                   "server-sent event stream (never ends); its snapshots are the same data as /api/aircraft and /api/trails, which are compared",
	"/api/public/events":            "server-sent event stream (never ends); same data as /api/public/aircraft",
	"/api/navdata/photo/{icao}":     "fetches a photo from the internet (navData.photos is off)",
	"/api/terrain/floor.png":        "terrain is off (needs elevation tiles from the internet); the JSON terrain routes are still called",
	"/api/uplinks":                  "NoAuto streaming route of the uplink feature (no uplinks configured)",
	"/api/auth/setup":               "aggregator account bootstrap (404 in standalone mode)",
	"/api/auth/sessions":            "account sessions (per login; none in this harness)",
	"/api/alerts/watchlists/{name}": "needs a watch-list; the watch-list index /api/alerts/watchlists is compared",
}

// noAutoCompared are NoAuto GET routes (excluded from the server's generic
// every-route test for their own reasons) that are safe to call here.
var noAutoCompared = map[string]bool{
	"/api/ssr":    true, // SSR self-location status (read-only)
	"/api/update": true, // self-update state (no check is triggered by GET)
}

// rangeVariants are added for every route that takes the common time-range
// parameters (from/to, hours, days): the comparison then covers the
// historic part of the database (the shifted sessions) and not only the
// last hours.
func rangeVariants(fromMs, toMs int64) []struct{ Name, Query string } {
	return []struct{ Name, Query string }{
		{"days=7", "days=7"},
		{"hist", fmt.Sprintf("from=%d&to=%d", fromMs, toMs)},
	}
}

// buildCalls returns the requests of one comparison round. ph resolves the
// sample placeholders ({live}, {seen}, {flight}, ...) to values both
// instances know; historic adds the range variants.
func buildCalls(ph map[string]string, histFrom, histTo int64) []call {
	var out []call
	for _, rt := range server.Routes() {
		if rt.Method != "GET" || rt.NetOnly {
			continue
		}
		if _, skip := skipRoutes[rt.Path]; skip {
			continue
		}
		if rt.NoAuto && !noAutoCompared[rt.Path] {
			// The remaining NoAuto GET routes are listed in skipRoutes;
			// a new one is reported, not silently ignored.
			logf("routes: NOTE new NoAuto GET route %s is not compared (add it to skipRoutes with a reason or handle it)", rt.Path)
			continue
		}
		u := rt.Sample
		if u == "" {
			u = rt.Path
		}
		missing := false
		for k, v := range ph {
			if strings.Contains(u, "{"+k+"}") {
				if v == "" {
					missing = true
				}
				u = strings.ReplaceAll(u, "{"+k+"}", v)
			}
		}
		if missing || strings.Contains(u, "{") {
			logf("routes: %s skipped: no value for its placeholder (%s)", rt.Path, u)
			continue
		}
		kind := kindOf(rt.ContentType, u)
		key := "GET " + rt.Path
		out = append(out, call{Key: key, URL: u, Kind: kind, Route: rt.Path})
		if takesRange(rt) && histTo > 0 {
			for _, v := range rangeVariants(histFrom, histTo) {
				out = append(out, call{Key: key + " [" + v.Name + "]", URL: withQuery(u, v.Query), Kind: kind, Route: rt.Path})
			}
		}
	}
	// Exports that the table lists only once with one sample file.
	out = append(out, extraCalls(ph)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// extraCalls: every file of /api/db/export, the KML / GeoJSON / CSV exports
// with a historic window, and the trace of a historic flight.
func extraCalls(ph map[string]string) []call {
	var out []call
	for _, f := range strings.Split(ph["exportFiles"], ",") {
		if f == "" || f == "records.csv" {
			continue // records.csv is the route's own sample
		}
		out = append(out, call{Key: "GET /api/db/export/{file} [" + f + "]", URL: "/api/db/export/" + f, Kind: kindOf("", f), Route: "/api/db/export/{file}"})
	}
	if id := ph["histFlight"]; id != "" {
		out = append(out,
			call{Key: "GET /api/history/flights/{id} [historic]", URL: "/api/history/flights/" + id, Kind: "json", Route: "/api/history/flights/{id}"},
			call{Key: "GET /api/history/flights/{id}/trace [historic]", URL: "/api/history/flights/" + id + "/trace", Kind: "json", Route: "/api/history/flights/{id}/trace"},
			call{Key: "GET /api/export/flight/{file} [historic]", URL: "/api/export/flight/" + id + ".geojson", Kind: "geojson", Route: "/api/export/flight/{file}"},
			call{Key: "GET /api/export/flight/{file} [kml]", URL: "/api/export/flight/" + id + ".kml", Kind: "kml", Route: "/api/export/flight/{file}"},
			call{Key: "GET /api/export/flight/{file} [csv]", URL: "/api/export/flight/" + id + ".csv", Kind: "csv", Route: "/api/export/flight/{file}"},
		)
	}
	if icao := ph["histICAO"]; icao != "" {
		out = append(out, call{Key: "GET /api/history/aircraft/{icao} [historic]", URL: "/api/history/aircraft/" + icao, Kind: "json", Route: "/api/history/aircraft/{icao}"})
	}
	out = append(out,
		call{Key: "GET /api/history/flights [all]", URL: "/api/history/flights?limit=500", Kind: "json", Route: "/api/history/flights"},
		call{Key: "GET /api/history/aircraft [all]", URL: "/api/history/aircraft?limit=500", Kind: "json", Route: "/api/history/aircraft"},
		call{Key: "GET /api/replay [hist]", URL: "/api/replay?hours=96&step=600", Kind: "json", Route: "/api/replay"},
		call{Key: "GET /api/reports.csv [hist]", URL: "/api/reports.csv?days=7&limit=500", Kind: "csv", Route: "/api/reports.csv"},
		call{Key: "GET /api/export/live.kml [60]", URL: "/api/export/live.kml?minutes=60", Kind: "kml", Route: "/api/export/live.kml"},
		call{Key: "GET /api/stats/events [all]", URL: "/api/stats/events?limit=500", Kind: "json", Route: "/api/stats/events"},
	)
	return out
}

func takesRange(rt server.Route) bool {
	for _, p := range rt.Params {
		if strings.HasPrefix(p.Name, "from") || p.Name == "days" || p.Name == "hours" {
			return true
		}
	}
	return false
}

func withQuery(u, q string) string {
	base, query, _ := strings.Cut(u, "?")
	vals, _ := url.ParseQuery(query)
	add, _ := url.ParseQuery(q)
	// A variant replaces the sample's own range parameters.
	for _, k := range []string{"from", "to", "hours", "days", "minutes"} {
		vals.Del(k)
	}
	for k, v := range add {
		vals[k] = v
	}
	return base + "?" + vals.Encode()
}

func kindOf(contentType, u string) string {
	p, _, _ := strings.Cut(u, "?")
	switch {
	case strings.HasSuffix(p, ".csv") || strings.Contains(contentType, "csv"):
		return "csv"
	case strings.HasSuffix(p, ".kml") || strings.Contains(contentType, "kml"):
		return "kml"
	case strings.HasSuffix(p, ".geojson") || strings.Contains(contentType, "geo+json"):
		return "geojson"
	case strings.HasSuffix(p, ".png") || strings.HasPrefix(contentType, "image/"):
		return "binary"
	case contentType == "" || strings.Contains(contentType, "json"):
		return "json"
	}
	return "text"
}

// fetch performs one GET.
func fetch(ctx context.Context, base, u string) (int, string, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+u, nil)
	if err != nil {
		return 0, "", nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), b, err
}

func getJSON(ctx context.Context, base, u string, v any) error {
	code, _, b, err := fetch(ctx, base, u)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("GET %s: status %d", u, code)
	}
	return json.Unmarshal(b, v)
}

// placeholders resolves the sample placeholders from what BOTH instances
// report: a live aircraft with a position that both see, a stored flight
// (with a trail) and an aircraft from the history that both list, and the
// oldest stored flight (historic).
func placeholders(ctx context.Context, a, b *instance) map[string]string {
	ph := map[string]string{"airport": "EDDF", "route": "DLH400?lat=50.2&lon=7.9", "watchlist": "", "live": "", "seen": "", "flight": ""}
	type ac struct {
		ICAO string   `json:"icao"`
		Hex  string   `json:"hex"`
		Lat  *float64 `json:"lat"`
	}
	liveOf := func(in *instance) map[string]bool {
		var r struct {
			Aircraft []ac `json:"aircraft"`
		}
		_ = getJSON(ctx, in.URL(), "/api/aircraft", &r)
		m := map[string]bool{}
		for _, x := range r.Aircraft {
			id := x.ICAO
			if id == "" {
				id = x.Hex
			}
			if x.Lat != nil && id != "" && !strings.HasPrefix(id, "~") {
				m[id] = true
			}
		}
		return m
	}
	la, lb := liveOf(a), liveOf(b)
	var common []string
	for k := range la {
		if lb[k] {
			common = append(common, k)
		}
	}
	sort.Strings(common)
	if len(common) > 0 {
		ph["live"] = common[0]
	}
	type fl struct {
		ID        int64  `json:"id"`
		ICAO      string `json:"icao"`
		Start     int64  `json:"start"`
		Positions int    `json:"positions"`
	}
	flightsOf := func(in *instance) []fl {
		var r struct {
			Flights []fl `json:"flights"`
		}
		_ = getJSON(ctx, in.URL(), "/api/history/flights?limit=500", &r)
		return r.Flights
	}
	fa, fb := flightsOf(a), flightsOf(b)
	inB := map[int64]fl{}
	for _, f := range fb {
		inB[f.ID] = f
	}
	var both []fl
	for _, f := range fa {
		if g, ok := inB[f.ID]; ok && g.ICAO == f.ICAO && g.Start == f.Start {
			both = append(both, f)
		}
	}
	sort.Slice(both, func(i, j int) bool { return both[i].ID > both[j].ID })
	for _, f := range both { // newest with a trail
		if f.Positions > 20 {
			ph["flight"], ph["seen"] = strconv.FormatInt(f.ID, 10), f.ICAO
			break
		}
	}
	for i := len(both) - 1; i >= 0; i-- { // oldest with a trail
		if both[i].Positions > 5 {
			ph["histFlight"], ph["histICAO"] = strconv.FormatInt(both[i].ID, 10), both[i].ICAO
			break
		}
	}
	if ph["flight"] == "" && len(both) > 0 {
		ph["flight"], ph["seen"] = strconv.FormatInt(both[0].ID, 10), both[0].ICAO
	}
	var ex struct {
		Tables []struct {
			Table string `json:"table"`
		} `json:"tables"`
	}
	if err := getJSON(ctx, a.URL(), "/api/db/export", &ex); err == nil {
		var names []string
		for _, f := range ex.Tables {
			names = append(names, f.Table+".csv")
		}
		ph["exportFiles"] = strings.Join(names, ",")
	}
	return ph
}
