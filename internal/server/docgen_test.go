package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"airview/internal/navdata"
)

// docs/API.md is generated: its endpoint list, parameters and notes come
// from the route table, the examples from real responses of a demo
// instance with simulated history, and the type reference from the Go
// result types (field names from the json tags, remarks from the source
// comments). Regenerate with
//
//	$env:AIRVIEW_GEN_API_DOC="1"; go test ./internal/server -run TestGenerateAPIDoc; Remove-Item Env:AIRVIEW_GEN_API_DOC
//
// TestDocsCoverage (always run) fails when the file and the route table
// disagree.

const apiDocPath = "../../docs/API.md"

// docTempPath matches a JSON string holding a path inside the demo
// instance's temporary data directory (group 1: the part below it);
// docHostname the host name of the generating machine.
var (
	docTempPath = regexp.MustCompile(`"[^"]*TestGenerateAPIDoc[0-9]+(?:\\\\|/)001((?:(?:\\\\|/)[^"\\/]*)*)"`)
	docHostname = regexp.MustCompile(`"hostname": "[^"]*"`)
)

var docHeading = regexp.MustCompile("(?m)^### `([A-Z]+) (/[^`]+)`")

func TestDocsCoverage(t *testing.T) {
	raw, err := os.ReadFile(apiDocPath)
	if err != nil {
		t.Fatalf("docs/API.md is missing (generate it, see docgen_test.go): %v", err)
	}
	documented := map[string]bool{}
	for _, m := range docHeading.FindAllStringSubmatch(string(raw), -1) {
		documented[m[1]+" "+m[2]] = true
	}
	want := map[string]bool{"GET /tilecache/{host}/{path...}": true}
	for _, rt := range Routes() {
		want[rt.Method+" "+rt.Path] = true
	}
	pending := netRoutesPendingAPIDoc(t)
	for k := range want {
		if !documented[k] {
			if pending[k] {
				// Network routes are listed in internal/server/WIRING.md until
				// docs/API.md is regenerated (HOOKS.md, WP-H -> integrator).
				continue
			}
			t.Errorf("docs/API.md does not document %s", k)
		}
	}
	for k := range documented {
		if !want[k] {
			t.Errorf("docs/API.md documents %s, which is not in the route table", k)
		}
	}
	// Every parameter of every route is mentioned in its section.
	doc := string(raw)
	for _, rt := range Routes() {
		head := "### `" + rt.Method + " " + rt.Path + "`"
		i := strings.Index(doc, head)
		if i < 0 {
			continue
		}
		section := doc[i+len(head):]
		if j := strings.Index(section, "\n### "); j >= 0 {
			section = section[:j]
		}
		for _, p := range rt.Params {
			if p.Name == "station" && pending[rt.Method+" "+rt.Path] {
				continue // aggregator station selection: in WIRING.md until docs/API.md is regenerated
			}
			for _, name := range strings.Split(p.Name, ",") {
				if !strings.Contains(section, "`"+strings.TrimSpace(name)+"`") {
					t.Errorf("docs/API.md: %s %s: parameter %s is not documented", rt.Method, rt.Path, name)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Generator
// ---------------------------------------------------------------------------

func TestGenerateAPIDoc(t *testing.T) {
	if os.Getenv("AIRVIEW_GEN_API_DOC") == "" {
		t.Skip("set AIRVIEW_GEN_API_DOC=1 to regenerate docs/API.md")
	}
	r := newRig(t, rigOptions{seedHours: 60, aircraft: 45, listen: true})
	r.waitTraffic(8)
	time.Sleep(25 * time.Second) // live trails, latency window, met observations
	ctx := context.Background()
	_ = r.eng.Tracker().Flush(ctx)
	_ = r.eng.Store().Flush()
	_ = r.eng.Forecaster().Refit(ctx)
	r.eng.Analyzer().Invalidate()
	r.eng.Notify("info", "Example notice")
	// Give one live callsign a route in the standing data so that the live
	// picture example shows `route`.
	for _, a := range r.eng.Tracker().Snapshot().Aircraft {
		if a.Callsign != "" && a.Lat != nil && !a.Military {
			data := fixRoutes + a.Callsign + "," + a.Callsign[:3] + ",1," + a.Callsign[:3] + ",EDDF-EGLL\n"
			if _, err := r.eng.NavData().ImportReader(ctx, navdata.DatasetRoutes, strings.NewReader(data), nil); err != nil {
				t.Fatal(err)
			}
			r.get("/api/navdata/route/" + a.Callsign)
			g0 := r.get("/api/navdata/route/" + a.Callsign)
			t.Logf("route for %s: %.200s", a.Callsign, g0.Body.String())
			break
		}
	}
	time.Sleep(1500 * time.Millisecond)

	g := &docgen{t: t, r: r, comments: map[string]string{}, types: map[string]reflect.Type{}, parsed: map[string]bool{}}
	var b bytes.Buffer
	b.WriteString(docIntro)

	b.WriteString("\n## Contents\n\n")
	var groups []string
	seen := map[string]bool{}
	for _, rt := range Routes() {
		if !seen[rt.Group] {
			seen[rt.Group] = true
			groups = append(groups, rt.Group)
		}
	}
	for _, grp := range groups {
		fmt.Fprintf(&b, "**%s**\n\n", grp)
		for _, rt := range Routes() {
			if rt.Group != grp {
				continue
			}
			admin := ""
			if rt.Admin {
				admin = " *(admin)*"
			}
			fmt.Fprintf(&b, "- `%s %s`%s - %s\n", rt.Method, rt.Path, admin, firstSentence(rt.Summary))
		}
		if grp == grpCore {
			b.WriteString("- `GET /tilecache/{host}/{path...}` - caching proxy for the whitelisted map tile hosts\n")
		}
		b.WriteString("\n")
	}

	for _, grp := range groups {
		fmt.Fprintf(&b, "\n## %s\n", grp)
		for _, rt := range Routes() {
			if rt.Group == grp {
				g.route(&b, rt)
			}
		}
		if grp == grpCore {
			b.WriteString(docTileCache)
		}
	}

	b.WriteString(docTypesHead)
	// Printing a type registers the types it refers to: repeat until no
	// new one turns up, then emit in alphabetical order.
	sections := map[string]string{}
	for progress := true; progress; {
		progress = false
		for n, typ := range g.types {
			if _, ok := sections[n]; !ok {
				var tb bytes.Buffer
				g.typeSection(&tb, n, typ)
				sections[n] = tb.String()
				progress = true
			}
		}
	}
	names := make([]string, 0, len(sections))
	for n := range sections {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	for _, n := range names {
		b.WriteString(sections[n])
	}
	b.WriteString(docStoreTypes)

	out := strings.ReplaceAll(b.String(), "\r\n", "\n")
	// The examples come from a demo instance in a temporary directory: do
	// not publish the generating machine's path (user name).
	out = docTempPath.ReplaceAllStringFunc(out, func(m string) string {
		rest := docTempPath.FindStringSubmatch(m)[1]
		return `"/var/lib/airview` + strings.ReplaceAll(rest, `\\`, "/") + `"`
	})
	out = docHostname.ReplaceAllString(out, `"hostname": "airview"`)
	if err := os.WriteFile(apiDocPath, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s: %d bytes, %d routes, %d types", apiDocPath, len(out), len(Routes()), len(g.types))
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i]
	}
	return strings.TrimSuffix(s, ".")
}

type docgen struct {
	t        *testing.T
	r        *rig
	ruleID   string            // alert rule created by the POST /api/alerts/rules example
	comments map[string]string // "pkgpath.Type.Field" -> remark; "pkgpath.Type" -> type doc
	parsed   map[string]bool
	types    map[string]reflect.Type // display name -> struct type
}

func (g *docgen) route(b *bytes.Buffer, rt Route) {
	fmt.Fprintf(b, "\n### `%s %s`\n\n", rt.Method, rt.Path)
	b.WriteString(rt.Summary + "\n\n")
	if rt.Admin {
		b.WriteString("**Admin endpoint**: `401 {\"error\":\"admin password required\"}` for a non-loopback client that is not logged in while a password is set; same-origin only.\n\n")
	}
	if len(rt.Params) > 0 {
		b.WriteString("| Parameter | Meaning |\n|---|---|\n")
		for _, p := range rt.Params {
			names := strings.Split(p.Name, ",")
			for i := range names {
				names[i] = "`" + strings.TrimSpace(names[i]) + "`"
			}
			fmt.Fprintf(b, "| %s | %s |\n", strings.Join(names, ", "), strings.ReplaceAll(p.Desc, "|", "\\|"))
		}
		b.WriteString("\n")
	}
	if rt.Body != "" {
		b.WriteString("Request body: " + rt.Body + "\n\n")
	}
	if rt.Notes != "" {
		b.WriteString(rt.Notes + "\n\n")
	}
	if rt.Cache > 0 {
		fmt.Fprintf(b, "Server-side cache: %s (identical requests share one computation).\n\n", rt.Cache)
	}
	if rt.Returns != "" {
		b.WriteString("Response: " + rt.Returns + "\n\n")
	}

	// Response type, from a direct call of the handler.
	if rt.API != nil && !rt.NoAuto {
		u := sampleURL(g.t, g.r, rt)
		req := httptest.NewRequest(rt.Method, u, nil)
		req.RemoteAddr = loopbackAddr
		setPathValues(req, rt.Path, u)
		v, err := rt.API(g.r.srv, req)
		if err != nil {
			g.t.Fatalf("%s %s: %v", rt.Method, u, err)
		}
		if rt.Returns == "" {
			fmt.Fprintf(b, "Response: %s\n\n", g.typeRef(reflect.TypeOf(v), true))
		}
	}
	defer func() {
		if rt.Path == "/api/aircraft" && rt.Method == "GET" {
			g.coastingExample(b)
		}
	}()
	switch {
	case rt.Path == "/api/events":
		b.WriteString(g.sseExample())
	case rt.NoAuto:
		g.specialExample(b, rt)
	default:
		u := sampleURL(g.t, g.r, rt)
		rec := g.r.get(u)
		if rec.Code != 200 {
			g.t.Fatalf("GET %s: %d %s", u, rec.Code, rec.Body.String())
		}
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
			lines := strings.SplitN(rec.Body.String(), "\n", 4)
			if len(lines) > 3 {
				lines = lines[:3]
			}
			fmt.Fprintf(b, "Example: `GET %s`\n\n```\n%s\n...\n```\n", u, strings.Join(lines, "\n"))
			return
		}
		if strings.HasPrefix(rec.Header().Get("Content-Type"), ctKML) {
			x := rec.Body.String()
			if len(x) > 900 {
				x = x[:900] + " ..."
			}
			fmt.Fprintf(b, "Example: `GET %s` (shortened)\n\n```xml\n%s\n```\n", u, strings.ReplaceAll(x, "><", ">\n<"))
			return
		}
		fmt.Fprintf(b, "Example: `GET %s`\n\n```jsonc\n%s\n```\n", u, exampleJSON(rec.Body.Bytes()))
	}
}

// specialExample shows a real exchange for the routes the generic test
// does not call (those that are safe to run here).
func (g *docgen) specialExample(b *bytes.Buffer, rt Route) {
	show := func(method, u, body string, hdr ...string) *httptest.ResponseRecorder {
		rec := g.r.do(method, u, loopbackAddr, body, hdr...)
		fmt.Fprintf(b, "Example: `%s %s`", method, u)
		if body != "" {
			fmt.Fprintf(b, " with body `%s`", body)
		}
		fmt.Fprintf(b, " -> `%d`\n\n```jsonc\n%s\n```\n", rec.Code, exampleJSON(rec.Body.Bytes()))
		return rec
	}
	switch rt.Method + " " + rt.Path {
	case "PUT /api/config":
		show("PUT", rt.Path, `{"ui":{"units":"metric","trailMinutes":20,"rangeRingsKm":[100,200,300],"aircraftTtlS":60,"mapStyle":"airview-dark","colorTrailsBy":"altitude"}}`)
	case "POST /api/login":
		show("POST", rt.Path, `{"password":"secret"}`)
	case "POST /api/logout":
		show("POST", rt.Path, "")
	case "POST /api/guess", "POST /api/db/checkpoint", "POST /api/db/prune", "POST /api/db/rollup", "POST /api/db/vacuum",
		"POST /api/forecast/refit", "POST /api/db/acdb/cancel":
		show("POST", rt.Path, "")
	case "POST /api/factory-reset":
		show("POST", rt.Path, `{"confirm":"no"}`)
	case "POST /api/weather/refresh", "POST /api/timing/ntp/sample":
		show("POST", rt.Path, "")
		b.WriteString("\n(The demo instance used for these examples runs offline.)\n")
	case "POST /api/db/acdb/upload":
		req := httptest.NewRequest("POST", rt.Path+"?name=sample.csv", strings.NewReader(
			"3C6444;D-AIBD;A319;00;AIRBUS A-319;2010;Lufthansa\n4CA2D1;EI-DCL;B738;00;BOEING 737-800;2004;Ryanair\n"))
		req.RemoteAddr, req.Host = loopbackAddr, "127.0.0.1:8080"
		rec := httptest.NewRecorder()
		g.r.srv.Handler().ServeHTTP(rec, req)
		fmt.Fprintf(b, "Example: `POST %s?name=sample.csv` with a two-line tar1090-db file as body -> `%d`\n\n```jsonc\n%s\n```\n", rt.Path, rec.Code, exampleJSON(rec.Body.Bytes()))

	// ---- extensions
	case "POST /api/terrain/prefetch", "POST /api/terrain/recompute", "POST /api/terrain/cancel",
		"POST /api/navdata/cancel", "POST /api/navdata/route-cache/clear", "POST /api/alerts/rules/reset-defaults":
		show("POST", rt.Path, "")
		if rt.Path == "/api/terrain/recompute" || rt.Path == "/api/terrain/prefetch" {
			g.waitTerrain()
		}
	case "GET /api/terrain/floor.png":
		img := struct {
			URL string `json:"url"`
		}{}
		_ = json.Unmarshal(g.r.get("/api/terrain/floor-image?w=256").Body.Bytes(), &img)
		rec := g.r.get(img.URL)
		fmt.Fprintf(b, "Example: `GET %s` -> `%d`, `%s`, %d bytes.\n", img.URL, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	case "POST /api/navdata/import":
		show("POST", rt.Path, `{"datasets":["nosuch"]}`)
		b.WriteString("\n(The documentation build runs offline, so this example names an unknown dataset; it starts and fails at once - `GET /api/navdata/progress` then reports the error. A real import reports `progress` per dataset.)\n")
		time.Sleep(300 * time.Millisecond)
	case "GET /api/navdata/photo/{icao}":
		show("GET", rt.Sample, "")
		b.WriteString("\n(Photos are off in the documentation instance. With `config.navData.photos` on and a contact set the answer is `navdata.Photo`: `{icao, found, thumbnail:{src,width,height}, large:{...}, link, photographer, attribution, cached}`.)\n")
	case "POST /api/alerts/rules":
		rec := show("POST", rt.Path, `{"name":"Heavies over the house","enabled":true,"severity":"info","match":{"op":"and","conds":[{"type":"category","values":["A5"]},{"type":"distance","max":25},{"type":"altitude","max":10000}]},"cooldownSec":900,"message":"{ident} ({type}) at {alt} ft, {dist} km","actions":{"notify":true,"sound":false,"log":true,"highlight":"#0a84ff"}}`)
		var rule struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &rule)
		g.ruleID = rule.ID
	case "PUT /api/alerts/rules/{id}":
		show("PUT", "/api/alerts/rules/"+g.ruleID, `{"name":"Heavies over the house","enabled":true,"severity":"warning","match":{"op":"and","conds":[{"type":"category","values":["A5"]},{"type":"distance","max":30}]},"cooldownSec":900,"actions":{"notify":true,"sound":true,"log":true,"highlight":"#0a84ff"}}`)
	case "DELETE /api/alerts/rules/{id}":
		rec := g.r.do("POST", "/api/alerts/rules", loopbackAddr, `{"name":"Temporary","enabled":false,"severity":"info","match":{"type":"military"},"actions":{}}`)
		var rule struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &rule)
		show("DELETE", "/api/alerts/rules/"+rule.ID, "")
	case "POST /api/alerts/rules/{id}/enabled":
		show("POST", "/api/alerts/rules/builtin-new-airframe/enabled", `{"enabled":true}`)
	case "POST /api/alerts/test":
		show("POST", rt.Path, `{"rule":{"name":"Above FL300","enabled":true,"severity":"info","match":{"type":"altitude","min":30000},"actions":{}}}`)
	case "POST /api/alerts/webhook-test":
		hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
		defer hook.Close()
		rec := g.r.do("POST", rt.Path, loopbackAddr, `{"url":"`+hook.URL+`/hook"}`)
		fmt.Fprintf(b, "Example: `POST %s` with body `{\"url\":\"http://127.0.0.1:<port>/hook\"}` (a local test receiver) -> `%d`\n\n```jsonc\n%s\n```\n", rt.Path, rec.Code, exampleJSON(rec.Body.Bytes()))
	case "POST /api/alerts/import":
		show("POST", rt.Path, `[{"name":"Imported: police helicopters","enabled":true,"severity":"info","match":{"op":"and","conds":[{"type":"category","values":["A7"]},{"type":"operator","values":["*POLIZEI*","*POLICE*"]}]},"cooldownSec":1800,"actions":{"notify":true,"highlight":"#bf5af2"}}]`)
	case "POST /api/alerts/watchlists/{name}/import":
		rec := g.r.do("POST", "/api/alerts/watchlists/vips/import", loopbackAddr, "3C6444 Lufthansa A319\n# comment\n4CA2D1,Ryanair\nnot-hex\n", "Content-Type", "text/plain")
		fmt.Fprintf(b, "Example: `POST /api/alerts/watchlists/vips/import` with the text body `3C6444 Lufthansa A319` / `# comment` / `4CA2D1,Ryanair` / `not-hex` (four lines) -> `%d`\n\n```jsonc\n%s\n```\n", rec.Code, exampleJSON(rec.Body.Bytes()))
	case "PUT /api/alerts/watchlists/{name}/{icao}":
		show("PUT", "/api/alerts/watchlists/vips/400A0B", `{"note":"British Airways"}`)
	case "DELETE /api/alerts/watchlists/{name}/{icao}":
		show("DELETE", "/api/alerts/watchlists/vips/400A0B", "")
	case "DELETE /api/alerts/watchlists/{name}":
		show("DELETE", "/api/alerts/watchlists/vips", "")
	}
}

// coastingExample documents one aircraft entry of the live picture that is
// shown at a predicted position (the demo feed drops position squitters of
// some aircraft now and then) and one with a route and an alert highlight
// when available.
func (g *docgen) coastingExample(b *bytes.Buffer) {
	snap := g.r.eng.Tracker().Snapshot()
	for _, a := range snap.Aircraft {
		if a.Route != nil {
			js, _ := json.Marshal(a)
			fmt.Fprintf(b, "\nExample of an aircraft with a `route` from the offline standing data (`plausible`: the position fits the great circle between the airports; `checked` false when that could not be tested):\n\n```jsonc\n%s\n```\n", exampleJSON(js))
			break
		}
	}
	for _, a := range snap.Aircraft {
		if a.Highlight != nil {
			js, _ := json.Marshal(a)
			fmt.Fprintf(b, "\nExample of an aircraft highlighted by an alert rule (`highlight`; here the built-in military rule):\n\n```jsonc\n%s\n```\n", exampleJSON(js))
			break
		}
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		snap := g.r.eng.Tracker().Snapshot()
		for _, a := range snap.Aircraft {
			if a.Predicted && a.CoastS != nil && *a.CoastS >= 8 {
				js, _ := json.Marshal(a)
				fmt.Fprintf(b, "\nExample of a coasting aircraft (no position received for %.0f s; `predicted:true`, the position is the tracking filter's prediction, `lastRealLat` / `lastRealLon` the last received one):\n\n```jsonc\n%s\n```\n", *a.CoastS, exampleJSON(js))
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	g.t.Log("docgen: no coasting aircraft appeared for the example")
}

// waitTerrain waits until no terrain job is running.
func (g *docgen) waitTerrain() {
	deadline := time.Now().Add(60 * time.Second)
	for g.r.eng.Terrain().Status().State == "running" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

func (g *docgen) sseExample() string {
	resp, err := http.Get(strings.TrimSuffix(g.r.srv.LocalURL(), "/") + "/api/events")
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(1200 * time.Millisecond)
		g.r.eng.Notify("alert", "Emergency: DLH4AB squawks 7700 (general emergency)")
		// A rule that fires once for the next aircraft above FL100: an
		// "alert" event (and, being a user rule, a notice of kind alert).
		g.r.do("POST", "/api/alerts/rules", loopbackAddr, `{"name":"Example rule","enabled":true,"severity":"warning","match":{"type":"altitude","min":10000},"cooldownSec":3600,"ruleCooldownSec":86400,"message":"{rule}: {ident} at {alt} ft, {dist} km","actions":{"notify":true,"sound":true,"highlight":"#ffd60a"}}`)
	}()
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 64<<10)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !(bytes.Contains(buf, []byte("event: notice")) && bytes.Contains(buf, []byte("event: alert"))) {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	var out strings.Builder
	out.WriteString("Example stream (data shortened):\n\n```\n")
	shown := map[string]bool{}
	for _, block := range strings.Split(string(buf), "\n\n") {
		lines := strings.SplitN(block, "\n", 2)
		key := lines[0]
		if shown[key] || block == "" {
			continue
		}
		shown[key] = true
		if len(lines) == 2 && strings.HasPrefix(lines[1], "data: ") {
			data := strings.TrimPrefix(lines[1], "data: ")
			if key == "event: alert" {
				out.WriteString(lines[0] + "\ndata: " + data + "\n\n")
				continue
			}
			if len(data) > 230 {
				data = data[:230] + " ..."
			}
			out.WriteString(lines[0] + "\ndata: " + data + "\n\n")
		} else {
			out.WriteString(block + "\n\n")
		}
	}
	out.WriteString(": hb\n\n```\n")
	return out.String()
}

// setPathValues fills r.PathValue for a direct handler call.
func setPathValues(r *http.Request, pattern, target string) {
	pp := strings.Split(pattern, "/")
	path := target
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	tp := strings.Split(path, "/")
	for i, seg := range pp {
		if strings.HasPrefix(seg, "{") && i < len(tp) {
			r.SetPathValue(strings.Trim(seg, "{}."), tp[i])
		}
	}
}

// ------------------------------------------------------------ type reference

var docScalars = map[string]string{
	"analytics.Num":        "number \\| null",
	"server.num":           "number \\| null",
	"weather.Num":          "number \\| null",
	"rfstat.Num":           "number \\| null",
	"server.replayPoints":  "array of `[aircraftIndex, lat, lon, altFt\\|null, gsKt\\|null, trackDeg\\|null, src, ageS]`",
	"server.tracePoints":   "array of `[tMs, lat, lon, altFt\\|null, gsKt\\|null, trackDeg\\|null, rssi\\|null, src]`",
	"navdata.SizeClass":    "integer (3 large, 2 medium, 1 small, 0 other, -1 closed)",
	"navdata.Dataset":      "string (`countries` \\| `airports` \\| `runways` \\| `navaids` \\| `routes`)",
	"analytics.Nums":       "(number \\| null)[]",
	"server.trailList":     "TrailPoint[] (compact arrays `[tMs, lat, lon, altFt\\|null, gsKt\\|null, rssi\\|null, src]`)",
	"json.RawMessage":      "any JSON",
	"time.Duration":        "integer (ns)",
	"time.Time":            "string (RFC 3339)",
	"store.AircraftRow":    "[store.AircraftRow](#store-rows)",
	"store.Flight":         "[store.Flight](#store-rows)",
	"store.StatsRow":       "[store.StatsRow](#store-rows)",
	"store.Weather":        "[store.Weather](#store-rows)",
	"store.RangeHour":      "[store.RangeHour](#store-rows)",
	"store.NTPSample":      "[store.NTPSample](#store-rows)",
	"store.Event":          "[store.Event](#store-rows)",
	"store.Record":         "[store.Record](#store-rows)",
	"store.ACDBEntry":      "[store.ACDBEntry](#store-rows)",
	"refdata.AllocKind":    "string (`state` \\| `territory` \\| `region` \\| `reserved` \\| `none`)",
	"refdata.CallsignKind": "string (`unknown` \\| `flight` \\| `registration` \\| `military` \\| `other`)",
}

func typeAnchor(name string) string {
	return "t-" + strings.NewReplacer(".", "-", "[", "-", "]", "", "*", "").Replace(name)
}

// typeRef renders a Go type the way it appears in JSON and registers the
// struct types it refers to.
func (g *docgen) typeRef(t reflect.Type, top bool) string {
	if t == nil {
		return "any"
	}
	if s, ok := docScalars[t.String()]; ok && s != "" {
		return s
	}
	switch t.Kind() {
	case reflect.Pointer:
		inner := g.typeRef(t.Elem(), top)
		if top {
			return inner
		}
		return inner + " \\| null"
	case reflect.Slice, reflect.Array:
		inner := g.typeRef(t.Elem(), false)
		if strings.Contains(inner, " ") && !strings.HasPrefix(inner, "[") {
			inner = "(" + inner + ")"
		}
		return inner + "[]"
	case reflect.Map:
		return "{ [key: string]: " + g.typeRef(t.Elem(), false) + " }"
	case reflect.Struct:
		if t.Name() == "" {
			return "object (see the example)"
		}
		name := t.String()
		g.types[name] = t
		return "[" + name + "](#" + typeAnchor(name) + ")"
	case reflect.Interface:
		return "any JSON (see the example)"
	case reflect.Bool:
		return "boolean"
	case reflect.String:
		return "string"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	}
	return t.String()
}

type docField struct {
	name, typ, note string
}

func (g *docgen) fields(t reflect.Type, out *[]docField) {
	g.parsePackage(t.PkgPath())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		ft := f.Type
		if f.Anonymous && name == "" {
			et := ft
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct && !et.Implements(marshalerType) && !reflect.PointerTo(et).Implements(marshalerType) {
				g.fields(et, out)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		note := g.comments[t.PkgPath()+"."+t.Name()+"."+f.Name]
		if strings.Contains(opts, "omitempty") {
			if note != "" {
				note = "*optional* - " + note
			} else {
				note = "*optional*"
			}
		}
		typ := g.typeRef(ft, false)
		if strings.Contains(opts, "omitempty") {
			typ = strings.TrimSuffix(typ, " \\| null")
		}
		// Replace an earlier field of the same name (outer fields shadow
		// embedded ones).
		replaced := false
		for j := range *out {
			if (*out)[j].name == name {
				(*out)[j] = docField{name, typ, note}
				replaced = true
			}
		}
		if !replaced {
			*out = append(*out, docField{name, typ, note})
		}
	}
}

func (g *docgen) typeSection(b *bytes.Buffer, name string, t reflect.Type) {
	g.parsePackage(t.PkgPath())
	fmt.Fprintf(b, "\n<a id=\"%s\"></a>\n\n#### %s\n\n", typeAnchor(name), name)
	if doc := g.comments[t.PkgPath()+"."+t.Name()]; doc != "" {
		b.WriteString(doc + "\n\n")
	}
	var fs []docField
	g.fields(t, &fs)
	b.WriteString("| Field | Type | Notes |\n|---|---|---|\n")
	for _, f := range fs {
		fmt.Fprintf(b, "| `%s` | %s | %s |\n", f.name, f.typ, strings.ReplaceAll(f.note, "|", "\\|"))
	}
}

// parsePackage collects the struct field comments of a package directory.
func (g *docgen) parsePackage(pkgPath string) {
	if pkgPath == "" || g.parsed[pkgPath] {
		return
	}
	g.parsed[pkgPath] = true
	var dir string
	switch {
	case pkgPath == "airview/internal/server":
		dir = "."
	case strings.HasPrefix(pkgPath, "airview/internal/"):
		dir = filepath.Join("..", strings.TrimPrefix(pkgPath, "airview/internal/"))
	default:
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		g.t.Fatalf("docgen: %v", err)
	}
	fset := token.NewFileSet()
	oneLine := func(cg *ast.CommentGroup) string {
		if cg == nil {
			return ""
		}
		return strings.Join(strings.Fields(cg.Text()), " ")
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ParseComments)
		if err != nil {
			g.t.Fatalf("docgen: %v", err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, sp := range gd.Specs {
				ts := sp.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				doc := oneLine(ts.Doc)
				if doc == "" {
					doc = oneLine(gd.Doc)
				}
				g.comments[pkgPath+"."+ts.Name.Name] = doc
				// A comment block above a group of fields applies to the
				// first field only; trailing comments are per field.
				for _, fl := range st.Fields.List {
					note := oneLine(fl.Comment)
					if d := oneLine(fl.Doc); d != "" {
						if note != "" {
							note = d + " " + note
						} else {
							note = d
						}
					}
					note = strings.Trim(strings.TrimSpace(note), "/ ")
					for _, n := range fl.Names {
						g.comments[pkgPath+"."+ts.Name.Name+"."+n.Name] = note
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------- examples

// jsonNode is JSON with the key order kept.
type jsonNode struct {
	kind  byte // 'o' object, 'a' array, 's' scalar
	keys  []string
	vals  []*jsonNode
	token string // scalar, as JSON text
}

func parseNode(dec *json.Decoder) (*jsonNode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			n := &jsonNode{kind: 'o'}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				child, err := parseNode(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k.(string))
				n.vals = append(n.vals, child)
			}
			_, err := dec.Token()
			return n, err
		case '[':
			n := &jsonNode{kind: 'a'}
			for dec.More() {
				child, err := parseNode(dec)
				if err != nil {
					return nil, err
				}
				n.vals = append(n.vals, child)
			}
			_, err := dec.Token()
			return n, err
		}
		return nil, fmt.Errorf("unexpected %v", v)
	case string:
		if len(v) > 140 {
			v = v[:140] + "…"
		}
		b, _ := json.Marshal(v)
		return &jsonNode{kind: 's', token: string(b)}, nil
	case json.Number:
		return &jsonNode{kind: 's', token: v.String()}, nil
	case bool:
		return &jsonNode{kind: 's', token: fmt.Sprint(v)}, nil
	case nil:
		return &jsonNode{kind: 's', token: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

func (n *jsonNode) scalarOnly() bool {
	for _, v := range n.vals {
		if v.kind != 's' {
			return false
		}
	}
	return true
}

// flat reports whether an array holds only scalars or arrays of scalars
// (compact rows such as trail points).
func (n *jsonNode) flat() bool {
	for _, v := range n.vals {
		if v.kind == 'o' || (v.kind == 'a' && !v.scalarOnly()) {
			return false
		}
	}
	return true
}

func (n *jsonNode) write(b *strings.Builder, indent string, depth int, tight bool) {
	switch n.kind {
	case 's':
		b.WriteString(n.token)
	case 'a':
		if len(n.vals) == 0 {
			b.WriteString("[]")
			return
		}
		if n.scalarOnly() {
			keep := 8
			if tight {
				keep = 4
			}
			b.WriteString("[")
			for i, v := range n.vals {
				if i == keep {
					fmt.Fprintf(b, ", /* … %d more */", len(n.vals)-keep)
					break
				}
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(v.token)
			}
			b.WriteString("]")
			return
		}
		keep := 2
		if depth >= 2 || tight || !n.flat() {
			keep = 1
		}
		if n.flat() {
			keep = 2
		}
		b.WriteString("[\n")
		for i, v := range n.vals {
			if i == keep {
				fmt.Fprintf(b, "%s  /* … %d more */\n", indent, len(n.vals)-keep)
				break
			}
			b.WriteString(indent + "  ")
			v.write(b, indent+"  ", depth+1, tight)
			if i < len(n.vals)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "]")
	case 'o':
		if len(n.keys) == 0 {
			b.WriteString("{}")
			return
		}
		// A map with many entries of the same kind: show a few.
		keep := len(n.keys)
		if keep > 14 {
			same := true
			for _, v := range n.vals {
				if v.kind != n.vals[0].kind || v.kind == 'o' {
					same = false
				}
			}
			if same && n.vals[0].kind == 'a' {
				keep = 2
			}
		}
		b.WriteString("{\n")
		for i := 0; i < keep; i++ {
			kb, _ := json.Marshal(n.keys[i])
			b.WriteString(indent + "  " + string(kb) + ": ")
			n.vals[i].write(b, indent+"  ", depth+1, tight)
			if i < len(n.keys)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		if keep < len(n.keys) {
			fmt.Fprintf(b, "%s  /* … %d more entries */\n", indent, len(n.keys)-keep)
		}
		b.WriteString(indent + "}")
	}
}

// exampleJSON pretty-prints a response with long arrays shortened.
func exampleJSON(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := parseNode(dec)
	if err != nil {
		if err == io.EOF {
			return "(empty)"
		}
		return string(raw)
	}
	var b strings.Builder
	n.write(&b, "", 0, false)
	if b.Len() > 9000 {
		b.Reset()
		n.write(&b, "", 0, true)
	}
	return b.String()
}

func TestExampleJSON(t *testing.T) {
	got := exampleJSON([]byte(`{"b":[1,2,3,4,5,6,7,8,9,10],"a":[{"x":1},{"x":2},{"x":3}],"e":[],"s":"str","n":null}`))
	want := "{\n  \"b\": [1, 2, 3, 4, 5, 6, 7, 8, /* … 2 more */],\n  \"a\": [\n    {\n      \"x\": 1\n    },\n    /* … 2 more */\n  ],\n  \"e\": [],\n  \"s\": \"str\",\n  \"n\": null\n}"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// ------------------------------------------------------------------- prose

const docIntro = `# AirView HTTP API

> Generated from the route table of ` + "`internal/server`" + ` (routes.go) and from real responses of a demo instance
> with 60 hours of simulated traffic and synthetic weather. Do not edit by hand: change the route table or the
> result types and regenerate (see ` + "`internal/server/docgen_test.go`" + `). A test fails when this file and the
> route table disagree.

Base URL: the origin the UI was loaded from (` + "`http://<host>[:port]`" + `). In the web UI use ` + "`ctx.api.get(path, {params, signal})`" + `
(see ` + "`web/static/js/README.md`" + `).

## Conventions

- **JSON, UTF-8.** Every endpoint answers ` + "`application/json`" + ` unless stated otherwise. Responses are never cached by the
  browser (` + "`Cache-Control: no-store`" + `) and are gzip-compressed when the client accepts it.
- **Errors:** HTTP status plus ` + "`{\"error\":\"message\"}`" + `. ` + "`400`" + ` bad parameter, ` + "`401`" + ` admin login needed, ` + "`403`" + ` cross-origin
  change refused, ` + "`404`" + ` unknown endpoint / object, ` + "`409`" + ` not possible in the current state, ` + "`429`" + ` throttled,
  ` + "`502`" + ` an upstream service (Open-Meteo, NTP, tile host) failed, ` + "`503`" + ` engine restarting (factory reset), ` + "`504`" + ` timed out.
- **Units:** timestamps Unix **milliseconds** UTC; altitude ft; speed kt; vertical rate ft/min; distance km; angles
  degrees true, clockwise from north; signal dBFS (negative, 0 = full scale); weather in degC, hPa, km/h, mm, %, m.
  Wind directions are where the wind blows **from**.
- **Unknown values:** numbers that are not known are ` + "`null`" + ` (types written ` + "`number | null`" + `) or the field is omitted
  (marked *optional*). NaN never appears. Arrays are never ` + "`null`" + `: an empty result is ` + "`[]`" + `.
- **Time range parameters** (wherever "from, to / hours / days" is listed): ` + "`from`" + ` and ` + "`to`" + ` in Unix ms, or ` + "`hours=N`" + ` /
  ` + "`days=N`" + ` for the span ending now (or at ` + "`to`" + `). Without any of them each endpoint uses its documented default span.
- **` + "`ant`" + ` parameter:** an antenna ID from ` + "`config.antennas[].id`" + `; empty (or ` + "`all`" + ` / ` + "`combined`" + `) means all antennas
  combined. Statistics per antenna exist only for feeds attributed to an antenna (` + "`feed.antennaId`" + `) and in demo mode
  (` + "`vertical`" + `, ` + "`yagi`" + `); an antenna without data yields empty results, not an error.
- **Altitude bands** (` + "`band`" + ` 0..6): <1000 ft, 1-5k, 5-10k, 10-20k, 20-30k, 30-40k, >=40k ft.
- **Per-bearing arrays** have 180 entries (2 degree bins, centre = ` + "`bearingDeg[i]`" + ` = 1, 3, 5 ... 359 degrees).
- **Position source codes** (` + "`src`" + ` in trail points): 0 ADS-B, 1 MLAT, 2 TIS-B / ADS-R, 3 other. ` + "`srcMask`" + ` in history rows
  is a bit set: 1 ADS-B, 2 MLAT, 4 TIS-B, 8 other, 16 Mode S without position.
- **Examples** below are real responses, shortened: ` + "`/* … N more */`" + ` marks omitted array elements.

## Access control

- **Read-only endpoints (all ` + "`GET`" + `) are open** to every client that can reach the server, and may be fetched
  cross-origin (` + "`Access-Control-Allow-Origin: *`" + `, no credentials) - the remote shell page and other AirView
  instances use this.
- **Admin endpoints** (everything that changes something; marked *admin*): requests from the machine itself
  (loopback) are always admin. Other clients are admin while no password is configured
  (` + "`config.http.adminPasswordHash`" + ` empty); once one is set they must ` + "`POST /api/login`" + ` first, otherwise the
  endpoint answers ` + "`401 {\"error\":\"admin password required\"}`" + `. ` + "`status.adminLocked`" + ` (and ` + "`GET /api/session`" + `)
  tell the UI whether this client has to log in.
- **Same-origin only for changes:** a ` + "`PUT`" + ` / ` + "`POST`" + ` whose ` + "`Origin`" + ` header names another host, or that a browser
  marks as cross-site, is refused with ` + "`403`" + `. Changes addressed to a public DNS name (not an IP address, a
  single-label or ` + "`.local`" + `-style name) are accepted only with a login session (protection against DNS rebinding).
- The session cookie is ` + "`airview_session`" + ` (HttpOnly, SameSite=Lax, 30 days); sessions live in memory and end on
  restart, logout, factory reset or password change.
`

const docTileCache = `
### ` + "`GET /tilecache/{host}/{path...}`" + `

Caching proxy for map tiles, glyphs, sprites and style documents: fetches ` + "`https://{host}/{path}?{query}`" + ` once and
serves it from ` + "`<data>/tilecache`" + ` afterwards. Enabled by ` + "`config.http.tileCache`" + ` (` + "`status.tileCache`" + `); ` + "`404`" + ` when
disabled.

- **Whitelist** (exact host names, also in ` + "`status.tileCacheHosts`" + `): ` + "`tiles.openfreemap.org`" + `,
  ` + "`tile.openstreetmap.org`" + `, ` + "`tilecache.rainviewer.com`" + `. Any other host: ` + "`403`" + `. It is not an open proxy: only
  ` + "`GET`" + ` / ` + "`HEAD`" + `, no cookies or client headers are forwarded, redirects to other hosts are not followed, and only
  image, vector-tile, font and JSON content is passed on.
- The path is passed through as written (` + "`%20`" + ` stays ` + "`%20`" + `), the query string too. Paths with ` + "`..`" + `, encoded slashes
  or backslashes, control characters or more than 1024 characters are refused with ` + "`400`" + `.
- Upstream requests carry ` + "`User-Agent: AirView/<version>`" + `; at most 8 run at a time; identical concurrent requests
  share one download; timeout 25 s.
- **Freshness:** the upstream ` + "`Cache-Control: max-age`" + ` / ` + "`Expires`" + ` is honoured with a minimum of 1 day (10 min for
  JSON documents) and a maximum of 30 days. Expired objects are revalidated (` + "`If-None-Match`" + ` /
  ` + "`If-Modified-Since`" + `); if the upstream is unreachable the stale copy is served.
- **Response headers:** the upstream ` + "`Content-Type`" + `, ` + "`ETag`" + ` / ` + "`Last-Modified`" + `, ` + "`Cache-Control: public, max-age=<s>`" + `,
  ` + "`Access-Control-Allow-Origin: *`" + ` and ` + "`X-AirView-Cache: HIT | MISS | REVALIDATED | STALE`" + `. Conditional requests
  from the browser are answered with ` + "`304`" + `.
- **Errors:** ` + "`404`" + ` tile does not exist upstream, ` + "`502`" + ` upstream failure or unexpected content (remembered for
  2 min), ` + "`503`" + ` proxy busy.
- **Disk cap:** 512 MB by default (environment variable ` + "`AIRVIEW_TILECACHE_MB`" + `); above it the least recently used
  files are deleted. See ` + "`GET /api/tilecache`" + ` and ` + "`POST /api/tilecache/clear`" + `.

In MapLibre, rewrite whitelisted URLs in ` + "`transformRequest`" + `:
` + "`https://tiles.openfreemap.org/planet/...`" + ` -> ` + "`/tilecache/tiles.openfreemap.org/planet/...`" + `.
`

const docTypesHead = `
## Type reference

Field tables of the response types named above, in alphabetical order (` + "`package.Type`" + ` as in the Go source).
The *Notes* column is the comment of the field in the source. A type written ` + "`T | null`" + ` can be null; fields
marked *optional* are left out when empty / unknown. ` + "`model.Config`" + `, ` + "`model.Status`" + `, ` + "`model.Aircraft`" + ` and
` + "`model.LiveSnapshot`" + ` are the shared contract types of ` + "`internal/model`" + `.
`

const docStoreTypes = `
<a id="store-rows"></a>

#### Database row types (store.*)

These encode themselves; unknown values are omitted. Times are Unix ms.

| Type | Fields |
|---|---|
| ` + "`store.AircraftRow`" + ` | ` + "`icao`, `firstSeen`, `lastSeen`, `callsign?`, `reg?`, `type?`, `typeDesc?`, `operator?`, `country?`, `category?`, `msgs`, `flights`, `positions` (lifetime totals), `maxDistKm?`, `maxAlt?` (ft), `srcMask`, `flags` (1 military, 2 interesting, 4 PIA, 8 LADD), `military?`, `year?`" + ` |
| ` + "`store.Flight`" + ` | ` + "`id`, `icao`, `callsign?`, `squawk?`, `start`, `end`, `msgs`, `positions`, `srcMask`, `maxDistKm?`, `maxDistBrg?`, `minDistKm?`, `minAlt?`, `maxAlt?` (ft), `maxGs?` (kt), `rssiMin?`, `rssiMax?`, `rssiAvg?` (dBFS), `firstLat?`, `firstLon?`, `lastLat?`, `lastLon?`, `emergency?`, `reg?`, `type?`, `operator?`, `country?`" + ` |
| ` + "`store.Event`" + ` | ` + "`id`, `t`, `kind` (`emergency` \\| `military` \\| `interesting` \\| `record` \\| `feed_up` \\| `feed_down`), `icao?`, `detail?`" + ` (object: callsign, squawk, emergency, reg, type, operator, country, lat, lon, altFt, distKm, bearing, flightId; record events: key, value, previous; feed events: feed, name, error) |
| ` + "`store.Record`" + ` | ` + "`key`, `value` (number \\| null), `icao?`, `t`, `detail?`" + ` (object: callsign, reg, type, lat, lon, altFt, distKm, bearing, gsKt, rssi) |
| ` + "`store.RangeHour`" + ` | ` + "`t` (start of the hour), `ant`, `maxKm?`, `p99Km?`, `p95Km?`, `p90Km?`, `p50Km?`, `positions`, `aircraft`, `farCount` (positions beyond 300 km), `residDb?` (mean of rssi + 20 log10(slant km)), `residN`, `distHist?` (120 counts, 5 km bins), `sectorMax?` (36 maxima in km, 10 degree sectors)" + ` |
| ` + "`store.Weather`" + ` | ` + "`t` (start of the hour), `issued?`, `leadH?` (forecast rows), `tempC?`, `dewpointC?`, `rh?`, `pressureMsl?`, `surfacePressure?`, `windKmh?`, `windDir?`, `gustKmh?`, `cloud?`, `cloudLow?`, `cloudMid?`, `cloudHigh?`, `precipMm?`, `rainMm?`, `snowCm?`, `visibilityM?`, `cape?`, `weatherCode?` (WMO), `isDay?` (1 / 0), `nSurface?` (N-units), `dnDh?` (N/km), `kFactor?`, `duct?` (M-units, 0 = none), `profile?` (`{levels:[{hpa,t,rh,z,ws,wd}], liftedIndex, cin, boundaryLayerM, precipProb, freezingLevelM}`, only when asked for)" + ` |
| ` + "`store.NTPSample`" + ` | ` + "`t`, `server?`, `offsetMs?`, `delayMs?`, `stratum`" + ` |
| ` + "`store.StatsRow`" + ` | ` + "`t`, `ant`, `frames`, `df0`, `df4`, `df5`, `df11`, `df16`, `df17`, `df18`, `df20`, `df21`, `dfOther`, `crcBad`, `positions`, `mlatPositions`, `aircraft`, `aircraftPos`, `adsb`, `mlat`, `modes`, `maxKm?`, `rssiAvg?`, `rssiMin?`, `rssiMax?`, `aircraftAvg?`, `minutes?`" + ` |
| ` + "`store.ACDBEntry`" + ` | ` + "`icao`, `reg?`, `type?`, `typeDesc?`, `operator?`, `flags`, `military?`, `year?`" + ` |
`
