package navdata

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/store"
)

func routesZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	add("standing-data-main/README.md", "# Standing Data\n")
	add("standing-data-main/airports/schema-01/E/EG.csv", "\xef\xbb\xbfCode,Name,ICAO\nEGLL,Heathrow,EGLL\n")
	add("standing-data-main/routes/schema-01/B/BAW-all.csv", fixRoutesBAW)
	add("standing-data-main/routes/schema-01/E/EZY-all.csv", fixRoutesEZY)
	add("standing-data-main/routes/schema-01/X/BROKEN-all.csv", "not,a,routes,file\n1,2,3,4\n")
	add("standing-data-main/airlines/schema-01/airlines.csv", "\xef\xbb\xbfCode,Name,ICAO,IATA,PositioningFlightPattern,CharterFlightPattern\n"+
		"BAW,British Airways,BAW,BA,9\\d\\d\\d,^(45|85|86)\\d\\d$\nEZY,easyJet,EZY,U2,,\n1B,Abacus International,,1B,,\n")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fixtureServer struct {
	*httptest.Server
	hits    atomic.Int64
	files   map[string][]byte
	fail    atomic.Bool
	notMods atomic.Int64
}

func newFixtureServer(t *testing.T) *fixtureServer {
	fs := &fixtureServer{files: map[string][]byte{
		"/airports.csv":  []byte(fixAirports),
		"/runways.csv":   []byte(fixRunways),
		"/navaids.csv":   []byte(fixNavaids),
		"/countries.csv": []byte(fixCountries),
		"/routes.zip":    routesZip(t),
		"/empty.csv":     []byte(`"id","ident","type","name","latitude_deg","longitude_deg"` + "\n"),
	}}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.hits.Add(1)
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "no UA", 400)
			return
		}
		if fs.fail.Load() {
			http.Error(w, "boom", 500)
			return
		}
		b, ok := fs.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		etag := `"v1-` + strings.TrimPrefix(r.URL.Path, "/") + `"`
		if r.Header.Get("If-None-Match") == etag {
			fs.notMods.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", "Thu, 12 Nov 2026 02:53:51 GMT")
		_, _ = w.Write(b)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fixtureServer) urls() map[Dataset]string {
	return map[Dataset]string{
		DatasetAirports: fs.URL + "/airports.csv", DatasetRunways: fs.URL + "/runways.csv",
		DatasetNavaids: fs.URL + "/navaids.csv", DatasetCountries: fs.URL + "/countries.csv",
		DatasetRoutes: fs.URL + "/routes.zip",
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// imported returns a NavData with all fixtures imported.
func imported(t *testing.T) (*NavData, *fixtureServer) {
	t.Helper()
	fs := newFixtureServer(t)
	nd := New(openStore(t), Options{})
	var progress []Progress
	res, err := nd.Import(context.Background(), ImportOptions{URLs: fs.urls(), Progress: func(p Progress) { progress = append(progress, p) }})
	if err != nil {
		t.Fatalf("import: %v (%+v)", err, res)
	}
	if len(res.Datasets) != 5 {
		t.Fatalf("datasets %d", len(res.Datasets))
	}
	if len(progress) == 0 || progress[len(progress)-1].Stage != "done" || progress[len(progress)-1].Fraction() != 1 {
		t.Fatalf("progress %+v", progress)
	}
	return nd, fs
}

func TestParseAirports(t *testing.T) {
	var got []Airport
	st, err := ParseAirports(strings.NewReader(fixAirports), func(a Airport) error { got = append(got, a); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != 8 || st.Skipped != 1 {
		t.Fatalf("stats %+v", st)
	}
	a := got[0]
	if a.ID != 2434 || a.ICAO != "EGLL" || a.IATA != "LHR" || a.Size != SizeLarge || !a.Scheduled || *a.ElevFt != 83 ||
		a.Country != "GB" || a.Municipality != "London" || math.Abs(a.Lat-51.4706) > 1e-9 {
		t.Fatalf("EGLL %+v", a)
	}
	if got[5].Size != SizeOther || got[5].Scheduled {
		t.Fatalf("heliport %+v", got[5])
	}
	// Old layout without icao_code: ICAO derived from a matching 4-letter GPS code.
	old := "id,ident,type,name,latitude_deg,longitude_deg,gps_code\n1,EDDF,large_airport,Frankfurt,50.03,8.57,EDDF\n2,00A,heliport,X,40,-74,K00A\n"
	got = nil
	if _, err := ParseAirports(strings.NewReader(old), func(a Airport) error { got = append(got, a); return nil }); err != nil {
		t.Fatal(err)
	}
	if got[0].ICAO != "EDDF" || got[1].ICAO != "" {
		t.Fatalf("legacy icao %+v", got)
	}
	// Hostile / wrong input.
	if _, err := ParseAirports(strings.NewReader("<html>nope</html>"), func(Airport) error { return nil }); !errors.Is(err, ErrBadHeader) {
		t.Fatalf("html: %v", err)
	}
	if _, err := ParseAirports(strings.NewReader(""), func(Airport) error { return nil }); !errors.Is(err, ErrBadHeader) {
		t.Fatalf("empty: %v", err)
	}
	stop := errors.New("stop")
	if _, err := ParseAirports(strings.NewReader(fixAirports), func(Airport) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("sink error: %v", err)
	}
}

func TestParseRunwaysNavaidsCountries(t *testing.T) {
	var rws []Runway
	st, err := ParseRunways(strings.NewReader(fixRunways), func(r Runway) error { rws = append(rws, r); return nil })
	if err != nil || st.Records != 5 || st.Skipped != 1 {
		t.Fatalf("runways %+v %v", st, err)
	}
	r := rws[0]
	if r.LE.Ident != "09L" || *r.LE.HeadingT != 89.6 || *r.LE.DisplacedFt != 1007 || r.HE.DisplacedFt != nil || *r.LengthFt != 12799 || !r.Lighted {
		t.Fatalf("09L/27R %+v", r)
	}
	if !rws[3].Closed || rws[3].LE.ElevFt != nil {
		t.Fatalf("closed %+v", rws[3])
	}
	if rws[4].LE.Lat != nil {
		t.Fatalf("helipad has no coords")
	}
	var nav []Navaid
	st, err = ParseNavaids(strings.NewReader(fixNavaids), func(n Navaid) error { nav = append(nav, n); return nil })
	if err != nil || st.Records != 2 || st.Skipped != 1 {
		t.Fatalf("navaids %+v %v", st, err)
	}
	if nav[0].Ident != "LAM" || *nav[0].FreqKHz != 115600 || nav[0].DMEChannel != "103X" || *nav[0].MagVarDeg != -1 || nav[0].Usage != "BOTH" {
		t.Fatalf("LAM %+v", nav[0])
	}
	var cs []Country
	st, err = ParseCountries(strings.NewReader(fixCountries), func(c Country) error { cs = append(cs, c); return nil })
	if err != nil || st.Records != 3 || cs[0].Name != "United Kingdom" {
		t.Fatalf("countries %+v %v", cs, err)
	}
}

func TestParseRoutes(t *testing.T) {
	var rr []RouteRecord
	st, err := ParseRoutesCSV(strings.NewReader(fixRoutesBAW), func(r RouteRecord) error { rr = append(rr, r); return nil })
	if err != nil || st.Records != 3 || st.Skipped != 2 {
		t.Fatalf("routes %+v %v", st, err)
	}
	if rr[0].Callsign != "BAW1" || rr[0].Airline != "BAW" || strings.Join(rr[0].Codes, ",") != "EGLL,KJFK" {
		t.Fatalf("BAW1 %+v", rr[0])
	}
	if len(rr[2].Codes) != 3 {
		t.Fatalf("multi-leg %+v", rr[2])
	}
	z := routesZip(t)
	rr = nil
	st, err = ParseRoutesZip(context.Background(), bytes.NewReader(z), int64(len(z)), func(r RouteRecord) error { rr = append(rr, r); return nil })
	if err != nil || st.Records != 5 {
		t.Fatalf("zip %+v %v", st, err)
	}
	if _, err := ParseRoutesZip(context.Background(), bytes.NewReader([]byte("PK\x03\x04junk")), 8, func(RouteRecord) error { return nil }); err == nil {
		t.Fatal("junk zip accepted")
	}
	for _, c := range []struct{ in, want string }{{" baw1 ", "BAW1"}, {"", ""}, {"TOOLONG123", ""}, {"BA W1", ""}, {"ÄBC", ""}} {
		if got := NormalizeCallsign(c.in); got != c.want {
			t.Errorf("NormalizeCallsign(%q)=%q", c.in, got)
		}
	}
}

func TestImportAndQueries(t *testing.T) {
	nd, fs := imported(t)
	ctx := context.Background()

	ds, err := nd.Datasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[Dataset]int64{DatasetCountries: 3, DatasetAirports: 8, DatasetRunways: 5, DatasetNavaids: 2, DatasetRoutes: 5}
	for _, d := range ds {
		if d.Rows != want[d.Dataset] || d.Imported == 0 || d.Source.Licence == "" {
			t.Errorf("dataset %+v", d)
		}
	}

	// The airline list comes with the routes zip.
	al, err := nd.Airlines(ctx)
	if err != nil || len(al) != 3 {
		t.Fatalf("airlines %+v %v", al, err)
	}
	for _, a := range al {
		if a.Code == "BAW" && (a.Name != "British Airways" || a.IATA != "BA" || a.Positioning != `9\d\d\d` || a.Charter != `^(45|85|86)\d\d$`) {
			t.Fatalf("BAW %+v", a)
		}
	}

	// By code: ICAO, IATA, lower-case, unknown.
	for _, code := range []string{"EGLL", "lhr", " egll "} {
		a, ok, err := nd.AirportByCode(ctx, code, true)
		if err != nil || !ok || a.ID != 2434 || len(a.Runways) != 2 || a.CountryName != "United Kingdom" {
			t.Fatalf("AirportByCode(%q) %v %v %+v", code, ok, err, a)
		}
	}
	if _, ok, _ := nd.AirportByCode(ctx, "ZZZZ", false); ok {
		t.Fatal("ZZZZ found")
	}

	// Bounding box around London: 4 airports + heliport; size filter.
	box := BBox{S: 51, W: -1, N: 52, E: 1}
	as, err := nd.AirportsInBBox(ctx, AirportQuery{BBox: box, MinSize: SizeMedium, Runways: true})
	if err != nil || len(as) != 4 {
		t.Fatalf("bbox medium+ %d %v", len(as), err)
	}
	if as[0].Size != SizeLarge || as[3].Ident != "EGGW" {
		t.Fatalf("order %v %v", as[0].Ident, as[3].Ident)
	}
	as, _ = nd.AirportsInBBox(ctx, AirportQuery{BBox: box, MinSize: SizeOther})
	if len(as) != 5 {
		t.Fatalf("bbox all %d", len(as))
	}
	if _, err := nd.AirportsInBBox(ctx, AirportQuery{BBox: BBox{S: 10, N: 5}}); !errors.Is(err, ErrBadInput) {
		t.Fatal("bad bbox accepted")
	}
	// Antimeridian box: Auckland (174.8E) and Honolulu (157.9W).
	as, _ = nd.AirportsInBBox(ctx, AirportQuery{BBox: BBox{S: -40, W: 170, N: 25, E: -150}})
	if len(as) != 2 {
		t.Fatalf("antimeridian %d", len(as))
	}

	// Nearest to central London: Heathrow (~22 km) vs London heliport for SizeOther.
	a, ok, err := nd.NearestAirport(ctx, 51.5074, -0.1278, SizeSmall, 100)
	if err != nil || !ok || a.Ident != "EGLL" || *a.DistKm < 22 || *a.DistKm > 25 {
		t.Fatalf("nearest %+v %v", a, err)
	}
	a, _, _ = nd.NearestAirport(ctx, 51.5074, -0.1278, SizeOther, 100)
	if a.Ident != "EGLW" {
		t.Fatalf("nearest any %s", a.Ident)
	}
	near, _ := nd.AirportsNear(ctx, 51.5074, -0.1278, 60, SizeSmall, 0, false)
	if len(near) != 4 || near[0].Ident != "EGLL" {
		t.Fatalf("near %d", len(near))
	}

	// Runway headings with displaced threshold.
	ends, err := nd.RunwayHeadings(ctx, "EGLL")
	if err != nil || len(ends) != 4 {
		t.Fatalf("ends %d %v", len(ends), err)
	}
	if ends[0].Runway != "09L" || ends[0].HeadingSrc != "published" || ends[0].Lon <= ends[0].EndLon {
		t.Fatalf("09L %+v", ends[0])
	}
	if d := (ends[0].Lon - ends[0].EndLon) * 111.195 * math.Cos(51.4775*math.Pi/180) * 1000; math.Abs(d-307) > 5 {
		t.Fatalf("displaced threshold %f m, want ~307 (1007 ft)", d)
	}
	gw, _ := nd.RunwayHeadings(ctx, "EGKK")
	if len(gw) != 2 {
		t.Fatalf("closed runway not excluded: %d", len(gw))
	}

	nav, err := nd.NavaidsInBBox(ctx, NavaidQuery{BBox: box, Types: []string{"vor-dme"}})
	if err != nil || len(nav) != 2 {
		t.Fatalf("navaids %d %v", len(nav), err)
	}
	cs, _ := nd.Countries(ctx)
	if len(cs) != 3 {
		t.Fatalf("countries %d", len(cs))
	}
	found, _ := nd.SearchAirports(ctx, "gatwick", 10)
	if len(found) != 1 || found[0].Ident != "EGKK" {
		t.Fatalf("search %+v", found)
	}
	if found, _ := nd.SearchAirports(ctx, "%", 10); len(found) != 0 {
		t.Fatalf("LIKE wildcard not escaped: %d", len(found))
	}

	// Second import: unchanged (ETag) for every dataset; Force re-imports.
	before := fs.notMods.Load()
	res, err := nd.Import(ctx, ImportOptions{URLs: fs.urls()})
	if err != nil || fs.notMods.Load()-before != 5 || !res.Datasets[0].Unchanged {
		t.Fatalf("etag: %v %d %+v", err, fs.notMods.Load()-before, res)
	}
	res, err = nd.Import(ctx, ImportOptions{URLs: fs.urls(), Force: true, Datasets: []Dataset{DatasetAirports}})
	if err != nil || res.Datasets[0].Unchanged || res.Datasets[0].Records != 8 {
		t.Fatalf("force %+v %v", res, err)
	}

	// A failing or empty download keeps the old data.
	fs.fail.Store(true)
	if _, err := nd.Import(ctx, ImportOptions{URLs: fs.urls(), Force: true, Datasets: []Dataset{DatasetAirports}}); err == nil {
		t.Fatal("HTTP 500 not reported")
	}
	fs.fail.Store(false)
	u := fs.urls()
	u[DatasetAirports] = fs.URL + "/empty.csv"
	if _, err := nd.Import(ctx, ImportOptions{URLs: u, Datasets: []Dataset{DatasetAirports}}); !errors.Is(err, ErrNoRecords) {
		t.Fatalf("empty: %v", err)
	}
	if _, ok, _ := nd.AirportByCode(ctx, "EGLL", false); !ok {
		t.Fatal("old data lost after failed import")
	}
	if _, err := nd.Import(ctx, ImportOptions{URLs: u, Datasets: []Dataset{"bogus"}}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("bogus dataset: %v", err)
	}
}

func TestImportLimitsAndCancel(t *testing.T) {
	fs := newFixtureServer(t)
	nd := New(openStore(t), Options{})
	ctx := context.Background()
	_, err := nd.Import(ctx, ImportOptions{URLs: fs.urls(), Datasets: []Dataset{DatasetAirports}, MaxBytes: 100})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("max bytes: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	res, err := nd.Import(cctx, ImportOptions{URLs: fs.urls()})
	if !errors.Is(err, context.Canceled) || !res.Cancelled {
		t.Fatalf("cancel: %v %+v", err, res)
	}
	// Stalled server: idle timeout.
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write([]byte(`"id","ident","type","name","latitude_deg","longitude_deg"` + "\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer stall.Close()
	_, err = nd.Import(ctx, ImportOptions{URLs: map[Dataset]string{DatasetAirports: stall.URL}, Datasets: []Dataset{DatasetAirports}, IdleTimeout: 1500 * time.Millisecond})
	if !errors.Is(err, ErrIdle) {
		t.Fatalf("idle: %v", err)
	}
	// Background job API.
	if err := nd.StartImport(ctx, ImportOptions{URLs: fs.urls()}); err != nil {
		t.Fatal(err)
	}
	if err := nd.StartImport(ctx, ImportOptions{URLs: fs.urls()}); !errors.Is(err, ErrImportRunning) {
		// The first job may already have finished on a fast machine.
		if err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for nd.ImportStatus().Running && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s := nd.ImportStatus()
	if s.Running || s.Result == nil || s.Error != "" || s.Fraction != 1 {
		t.Fatalf("status %+v", s)
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"datasets"`) {
		t.Fatalf("json %s", b)
	}
	nd.CancelImport() // no-op after completion
	// Local file import.
	dr, err := nd.ImportReader(ctx, DatasetRoutes, strings.NewReader(fixRoutesEZY), nil)
	if err != nil || dr.Records != 2 {
		t.Fatalf("ImportReader %+v %v", dr, err)
	}
	if _, ok, _ := nd.StoredRoute(ctx, "BAW1"); ok {
		t.Fatal("routes table not replaced by local import")
	}
}

func TestRoutesStandingData(t *testing.T) {
	nd, _ := imported(t)
	ctx := context.Background()
	// Over Ireland, on the EGLL-KJFK great circle (~ 53.0N 8.6W): plausible.
	ri, err := nd.Route(ctx, "baw117", &Point{Lat: 53.0, Lon: -8.6})
	if err != nil || !ri.Found || ri.Source != "standing-data" || len(ri.Airports) != 2 || !ri.Airports[1].Resolved {
		t.Fatalf("route %+v %v", ri, err)
	}
	if math.Abs(ri.DistanceKm-5540) > 20 {
		t.Fatalf("EGLL-KJFK distance %f, want ~5540", ri.DistanceKm)
	}
	if ri.Check == nil || !ri.Check.Plausible || ri.Check.Doubtful || ri.Check.AlongPct < 5 || ri.Check.AlongPct > 15 {
		t.Fatalf("check %+v", ri.Check)
	}
	if p := ri.Path; len(p) < 50 || p[0] != [2]float64{-0.461941, 51.4706} || p[len(p)-1] != [2]float64{-73.779317, 40.639447} {
		t.Fatalf("path %d %v %v", len(p), p[0], p[len(p)-1])
	}
	// Over Rome: doubtful.
	ri, _ = nd.Route(ctx, "BAW117", &Point{Lat: 41.9, Lon: 12.5})
	if ri.Check == nil || ri.Check.Plausible || !ri.Check.Doubtful {
		t.Fatalf("Rome %+v", ri.Check)
	}
	// Near Gatwick on a 3-leg route.
	ri, _ = nd.Route(ctx, "BAW9X", &Point{Lat: 51.2, Lon: -0.2})
	if len(ri.Airports) != 3 || !ri.Check.Plausible {
		t.Fatalf("multi-leg %+v", ri)
	}
	// Antimeridian route: path longitudes are continuous.
	ri, _ = nd.Route(ctx, "ANZ1", nil)
	for i := 1; i < len(ri.Path); i++ {
		if math.Abs(ri.Path[i][0]-ri.Path[i-1][0]) > 10 {
			t.Fatalf("lon jump at %d: %v %v", i, ri.Path[i-1], ri.Path[i])
		}
	}
	// Unknown callsign, online disabled: not found, no request.
	ri, err = nd.Route(ctx, "XYZ999", nil)
	if err != nil || ri.Found {
		t.Fatalf("unknown %+v %v", ri, err)
	}
	if _, err := nd.Route(ctx, "bad callsign!", nil); !errors.Is(err, ErrBadInput) {
		t.Fatalf("bad callsign %v", err)
	}
	b, err := json.Marshal(ri)
	if err != nil || strings.Contains(string(b), "NaN") {
		t.Fatalf("json %s %v", b, err)
	}
}

func TestRoutesOnline(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch strings.TrimPrefix(r.URL.Path, "/v0/callsign/") {
		case "DLH400":
			_, _ = w.Write([]byte(`{"response":{"flightroute":{"callsign":"DLH400","callsign_icao":"DLH400","callsign_iata":"LH400","airline":{"name":"Lufthansa","icao":"DLH","iata":"LH"},"origin":{"iata_code":"FRA","icao_code":"EDDF","latitude":50.03333,"longitude":8.57056,"name":"Frankfurt am Main Airport"},"destination":{"iata_code":"JFK","icao_code":"KJFK","latitude":40.6398,"longitude":-73.7789,"name":"John F Kennedy International Airport"}}}}`))
		case "SLOW1":
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"response":"unknown callsign"}`))
		}
	}))
	defer srv.Close()
	nd, _ := imported(t)
	ctx := context.Background()
	opts := Options{Routes: RouteOptions{Online: true, BaseURL: srv.URL + "/v0/callsign/", MinInterval: time.Millisecond}}
	nd.SetOptions(opts)
	ri, err := nd.Route(ctx, "DLH400", nil)
	if err != nil || !ri.Found || ri.Source != "adsbdb" || ri.Airline != "DLH" || ri.Cached ||
		ri.Airports[0].Code != "EDDF" || ri.Airports[0].Resolved || !ri.Airports[1].Resolved {
		t.Fatalf("online %+v %v", ri, err)
	}
	ri, _ = nd.Route(ctx, "DLH400", nil)
	if !ri.Cached || hits.Load() != 1 {
		t.Fatalf("cache: cached=%v hits=%d", ri.Cached, hits.Load())
	}
	// Negative answers are cached too.
	ri, err = nd.Route(ctx, "NOPE1", nil)
	if err != nil || ri.Found {
		t.Fatalf("404 %+v %v", ri, err)
	}
	ri, _ = nd.Route(ctx, "NOPE1", nil)
	if !ri.Cached || hits.Load() != 2 {
		t.Fatalf("negative cache hits=%d", hits.Load())
	}
	// Standing data wins without a request.
	if ri, _ := nd.Route(ctx, "BAW1", nil); ri.Source != "standing-data" || hits.Load() != 2 {
		t.Fatal("standing data not preferred")
	}
	// 429 backs off; the next request is refused locally.
	if _, err := nd.Route(ctx, "SLOW1", nil); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	if _, err := nd.Route(ctx, "OTHER1", nil); !errors.Is(err, ErrRateLimited) || hits.Load() != 3 {
		t.Fatalf("backoff: %v hits=%d", err, hits.Load())
	}
	if err := nd.ClearRouteCache(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }
	l.configure(2*time.Second, 3)
	if w, err := l.reserve(0); err != nil || w != 0 {
		t.Fatalf("first %v %v", w, err)
	}
	if _, err := l.reserve(time.Second); !errors.Is(err, ErrRateLimited) {
		t.Fatal("second within interval accepted without waiting budget")
	}
	if w, err := l.reserve(5 * time.Second); err != nil || w != 2*time.Second {
		t.Fatalf("wait %v %v", w, err)
	}
	now = now.Add(10 * time.Second)
	if _, err := l.reserve(0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if _, err := l.reserve(0); !errors.Is(err, ErrRateLimited) {
		t.Fatal("daily limit not enforced")
	}
	now = now.Add(25 * time.Hour)
	if _, err := l.reserve(0); err != nil {
		t.Fatalf("new day: %v", err)
	}
}

func TestPhotos(t *testing.T) {
	var hits atomic.Int64
	var ua atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		ua.Store(r.Header.Get("User-Agent"))
		switch r.URL.Path {
		case "/pub/photos/hex/3C6444":
			if r.URL.Query().Get("reg") != "D-AIBD" {
				t.Errorf("reg param %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"photos":[{"id":"1","thumbnail":{"src":"https://t.plnspttrs.net/1_t.jpg","size":{"width":200,"height":133}},"thumbnail_large":{"src":"https://t.plnspttrs.net/1_280.jpg","size":{"width":420,"height":280}},"link":"https://www.planespotters.net/photo/1/d-aibd","photographer":"Jane Doe"}]}`))
		case "/pub/photos/hex/4CA123":
			// No photographer: must not be shown.
			_, _ = w.Write([]byte(`{"photos":[{"thumbnail":{"src":"https://t.plnspttrs.net/2.jpg"},"link":"https://www.planespotters.net/photo/2"}]}`))
		default:
			_, _ = w.Write([]byte(`{"photos":[]}`))
		}
	}))
	defer srv.Close()
	nd := New(openStore(t), Options{})
	ctx := context.Background()
	if _, err := nd.Photo(ctx, 0x3C6444, ""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("default must be disabled: %v", err)
	}
	nd.SetOptions(Options{Photos: PhotoOptions{Enabled: true, BaseURL: srv.URL + "/pub/photos", MinInterval: time.Millisecond}})
	if _, err := nd.Photo(ctx, 0x3C6444, ""); !errors.Is(err, ErrContactMissing) {
		t.Fatalf("contact: %v", err)
	}
	nd.SetOptions(Options{Photos: PhotoOptions{Enabled: true, Contact: "https://station.example.org/contact", BaseURL: srv.URL + "/pub/photos", MinInterval: time.Millisecond}})
	p, err := nd.Photo(ctx, 0x3C6444, "d-aibd")
	if err != nil || !p.Found || p.Photographer != "Jane Doe" || p.Thumbnail.Width != 200 || p.Large == nil ||
		p.Attribution != "© Jane Doe / planespotters.net" || p.ICAO != "3C6444" {
		t.Fatalf("photo %+v %v", p, err)
	}
	if got := ua.Load().(string); !strings.Contains(got, "(+https://station.example.org/contact)") || !strings.HasPrefix(got, "AirView/") {
		t.Fatalf("UA %q", got)
	}
	p, _ = nd.Photo(ctx, 0x3C6444, "")
	if !p.Cached || !p.Found || hits.Load() != 1 {
		t.Fatalf("cache %+v hits=%d", p, hits.Load())
	}
	p, err = nd.Photo(ctx, 0x4CA123, "")
	if err != nil || p.Found || p.Thumbnail != nil || p.Link != "" {
		t.Fatalf("unattributed photo exposed %+v", p)
	}
	p, _ = nd.Photo(ctx, 0x400000, "")
	if p.Found {
		t.Fatal("empty list found")
	}
	if _, err := nd.Photo(ctx, 0x1000000, ""); !errors.Is(err, ErrBadInput) {
		t.Fatal("bad icao")
	}
	for _, c := range []struct {
		s  string
		ok bool
	}{{"me@example.org", true}, {"https://x.org/c", true}, {"", false}, {"hello", false}, {"a@b", false}, {"x) evil (", false}, {"ftp://x.org", false}} {
		if validContact(c.s) != c.ok {
			t.Errorf("validContact(%q) != %v", c.s, c.ok)
		}
	}
}

// TestLive downloads the real files. Run with AIRVIEW_LIVE_TESTS=1.
func TestLive(t *testing.T) {
	if os.Getenv("AIRVIEW_LIVE_TESTS") == "" {
		t.Skip("set AIRVIEW_LIVE_TESTS=1 to download the real OurAirports / standing-data files")
	}
	nd := New(openStore(t), Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := nd.Import(ctx, ImportOptions{})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	min := map[Dataset]int{DatasetCountries: 200, DatasetAirports: 50000, DatasetRunways: 30000, DatasetNavaids: 8000, DatasetRoutes: 300000}
	for _, d := range res.Datasets {
		t.Logf("%s: %d records, %d skipped, %d bytes, %d ms", d.Dataset, d.Records, d.Skipped, d.Bytes, d.ElapsedMs)
		if d.Records < min[d.Dataset] {
			t.Errorf("%s: only %d records", d.Dataset, d.Records)
		}
	}
	a, ok, err := nd.AirportByCode(ctx, "EGLL", true)
	if err != nil || !ok || len(a.Runways) < 2 {
		t.Fatalf("EGLL %+v %v", a, err)
	}
	ri, err := nd.Route(ctx, "BAW1", nil)
	t.Logf("BAW1: %+v %v", ri.Airports, err)
}
