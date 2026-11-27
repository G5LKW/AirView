package terrain

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
)

func testAntennas() []model.Antenna {
	return []model.Antenna{
		{ID: "yagi", Name: "Yagi", Type: model.AntYagi, GainDBi: 12, HeightAGLM: 12, AzimuthDeg: 0, HBeamwidthDeg: 40, VBeamwidthDeg: 40, FrontToBackDB: 20},
		{ID: "vert", Name: "Vertical", Type: model.AntVertical, GainDBi: 5, HeightAGLM: 8},
	}
}

func testRequest() Request {
	return Request{
		Lat: stLat, Lon: stLon, GroundElevM: 0, Antennas: testAntennas(),
		RadiusKm: 150, BearingStepDeg: 2, SampleKm: 0.5, Radio: true,
		AltitudesFt: []float64{1000, 10000, 30000},
	}
}

// waitJob waits until the service is not running any more and returns the
// final status.
func waitJob(t *testing.T, s *Service, key string) JobStatus {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st := s.Status()
		if st.Key == key && st.State != StateRunning {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish: %+v", key, s.Status())
	return JobStatus{}
}

func startService(t *testing.T, o ServiceOptions) *Service {
	t.Helper()
	s := NewService(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return s
}

func TestRequestKey(t *testing.T) {
	a := testRequest()
	b := testRequest()
	if a.Key() != b.Key() {
		t.Fatal("equal requests, different keys")
	}
	b.Antennas[1].HeightAGLM++
	if a.Key() == b.Key() {
		t.Error("antenna height not in key")
	}
	c := testRequest()
	c.K = 1.2
	if a.Key() == c.Key() {
		t.Error("k not in key")
	}
	d := testRequest()
	d.Lat += 0.001
	if a.Key() == d.Key() {
		t.Error("position not in key")
	}
	// Defaults are normalised: k 0 == 4/3.
	e := testRequest()
	e.K = 4.0 / 3
	if a.Key() != e.Key() {
		t.Error("k=0 and k=4/3 give different keys")
	}
	n := Request{Lat: 50, Lon: 10}.normalized()
	if n.RadiusKm != 450 || n.BearingStepDeg != 0.5 || n.Zoom != 9 || len(n.Antennas) != 1 || len(n.AltitudesFt) != 7 {
		t.Errorf("defaults %+v", n)
	}
}

func TestServiceRidgeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	s := startService(t, ServiceOptions{Dir: dir, Source: ridgeSource()})
	if s.Analysis() != nil || s.Visible("", 50.5, 10, 10000).Known {
		t.Fatal("analysis before any job")
	}
	req := testRequest()
	if !s.Submit(req, false) {
		t.Fatal("submit refused")
	}
	st := waitJob(t, s, req.Key())
	if st.State != StateDone || st.Progress != 1 || st.FromCache {
		t.Fatalf("status %+v", st)
	}
	a := s.Analysis()
	if a == nil || a.Key != req.Key() || len(a.Antennas) != 2 || !a.DataComplete || a.GroundSource != GroundDEM {
		t.Fatalf("analysis %+v", a)
	}
	// Same request again: nothing to do.
	if s.Submit(req, false) {
		t.Error("current request resubmitted")
	}
	// The polar terrain was cached on disk.
	if m, _ := filepath.Glob(filepath.Join(dir, "polar_*.bin.gz")); len(m) != 1 {
		t.Errorf("polar cache files %v", m)
	}

	// Ridge north (±20°, 30 km, 500 m): an aircraft at 3000 ft 100 km north
	// is hidden, at 100 km south it is visible.
	o := newOrigin(stLat, stLon)
	nLa, nLo := o.destKm(0, 100)
	sLa, sLo := o.destKm(180, 100)
	if v := s.Visible("", nLa, nLo, 3000); !v.Known || v.Visible {
		t.Errorf("north behind ridge: %+v", v)
	}
	if v := s.Visible("vert", sLa, sLo, 3000); !v.Known || !v.Visible || v.AntennaID != "vert" {
		t.Errorf("south: %+v", v)
	}
	// Yagi is 4 m higher, so its ridge floor is lower.
	fy := s.Visible("yagi", nLa, nLo, 3000).FloorFt
	fv := s.Visible("vert", nLa, nLo, 3000).FloorFt
	if !(fy < fv) {
		t.Errorf("yagi floor %.0f ft, vertical %.0f ft", fy, fv)
	}
	// 0.5 km sampling sees the ridge at 30.5 and 31 km (not exactly at its
	// 30 km front edge): between the two closed-form values.
	if f := fv * geo.MetresPerFoot; f < expectedRidgeMinAltM2(100, 8)-40 || f > expectedRidgeMinAltM2(100, 8)+5 {
		t.Errorf("ridge floor (vertical, 100 km) %.1f m, closed form %.1f m", f, expectedRidgeMinAltM2(100, 8))
	}

	// Forecast: flying north at 3000 ft from 10 km south of the station is
	// lost behind the ridge.
	var path []PathPoint
	for i := 0; i <= 60; i++ {
		la, lo := o.destKm(0, -10+float64(i)*2)
		path = append(path, PathPoint{TMs: int64(i) * 10000, Lat: la, Lon: lo, AltFt: 3000})
	}
	f := s.Forecast("", path)
	if !f.Known || !f.VisibleNow || f.LostAtMs == 0 {
		t.Errorf("forecast %+v", f)
	}

	// LOS on demand through the service.
	r, err := s.LOS(context.Background(), "vert", nLa, nLo, 3000, true)
	if err != nil || r.Clear || r.Obstruction == nil || len(r.Profile) == 0 {
		t.Fatalf("LOS %+v %v", r.Clear, err)
	}
	near(t, "obstruction distance", r.Obstruction.DistKm, 30.5, 0.6)
	if r.TotalLossDB < r.FSPLDB+r.DiffractionDB-1e-6 || r.DiffractionDB <= 6 {
		t.Errorf("loss fspl %.1f diff %.1f total %.1f", r.FSPLDB, r.DiffractionDB, r.TotalLossDB)
	}

	// Coverage: the ridge sector is shorter than the open south at 10 000 ft.
	c := a.Antenna("vert")
	l := c.LOS[1]
	if l.AltFt != 10000 || !(l.RangeKm[0] < l.RangeKm[90]) {
		t.Errorf("10k ft LOS north %.1f south %.1f", l.RangeKm[0], l.RangeKm[90])
	}
	if len(c.Radio) != 3 || c.SensitivityDBm == 0 {
		t.Errorf("radio ranges missing: %d", len(c.Radio))
	}
	if a.Advice == nil || len(a.Advice.Heights) != 4 || len(a.Advice.Sectors) != 36 {
		t.Errorf("advice %+v", a.Advice)
	}

	// A new service on the same directory reuses the polar terrain cache.
	s2 := startService(t, ServiceOptions{Dir: dir, Source: ridgeSource()})
	req2 := testRequest()
	req2.Antennas[0].HeightAGLM = 20 // different key, same terrain
	s2.Submit(req2, false)
	if st := waitJob(t, s2, req2.Key()); st.State != StateDone || !st.FromCache {
		t.Errorf("second service %+v", st)
	}
}

// expectedRidgeMinAltM2 generalises expectedRidgeMinAltM to an antenna
// height (both ridge edges, parabolic bulge).
func expectedRidgeMinAltM2(distKm, antM float64) float64 {
	best := 0.0
	for _, x := range []float64{30, 31} {
		t := 500 + x*(distKm-x)/(2*kR)*1000
		if h := antM + (t-antM)*distKm/x; h > best {
			best = h
		}
	}
	return best
}

func TestServiceCancel(t *testing.T) {
	s := startService(t, ServiceOptions{Source: slowSource{FlatSource{}}})
	req := testRequest()
	req.RadiusKm, req.BearingStepDeg, req.SampleKm = 400, 0.5, 0.2
	s.Submit(req, false)
	time.Sleep(50 * time.Millisecond)
	s.Cancel()
	st := waitJob(t, s, req.Key())
	if st.State != StateCancelled || s.Analysis() != nil {
		t.Errorf("after cancel: %+v", st)
	}
	// A newer request supersedes a running one.
	s.Submit(req, false)
	time.Sleep(20 * time.Millisecond)
	small := testRequest()
	small.RadiusKm = 20
	s.Submit(small, false)
	if st := waitJob(t, s, small.Key()); st.State != StateDone {
		t.Errorf("superseding job %+v", st)
	}
	if a := s.Analysis(); a == nil || a.Key != small.Key() {
		t.Error("superseding result not published")
	}
}

// slowSource makes terrain sampling slow enough to cancel.
type slowSource struct{ FlatSource }

func (s slowSource) ElevationM(lat, lon float64) (float64, bool) {
	x := 0.0
	for i := 0; i < 200; i++ {
		x += math.Sqrt(float64(i))
	}
	return s.ElevM + x*0, true
}

// TestServiceWithTileProvider runs the whole pipeline over the HTTP tile
// provider (synthetic cone served as Terrarium PNGs), then offline from the
// cache.
func TestServiceWithTileProvider(t *testing.T) {
	o := newOrigin(stLat, stLon)
	cLa, cLo := o.destKm(90, 20)
	cone := ConeSource{Lat: cLa, Lon: cLo, PeakM: 1200, BaseM: 100, RadiusKm: 6}
	ts := newTileServer(t, func(z, x, y int) ([]float32, int) {
		e := make([]float32, tilePixels)
		RenderTerrariumTile(cone, TileXY{z, x, y}, e)
		return e, http.StatusOK
	})
	dir := t.TempDir()
	prov := NewProvider(ProviderOptions{Dir: filepath.Join(dir, "tiles"), URLTemplate: ts.url()})
	s := startService(t, ServiceOptions{Dir: dir, Provider: prov})
	req := testRequest()
	req.RadiusKm, req.Zoom = 40, 10
	s.Submit(req, false)
	st := waitJob(t, s, req.Key())
	if st.State != StateDone || st.Tiles.Total == 0 || st.Tiles.Downloaded == 0 || st.Tiles.Failed != 0 {
		t.Fatalf("status %+v", st)
	}
	a := s.Analysis()
	if !a.DEMGroundOK || math.Abs(a.GroundM-100) > 1 || !a.DataComplete {
		t.Errorf("ground %+v ok=%v complete=%v", a.GroundM, a.DEMGroundOK, a.DataComplete)
	}
	h := a.Antenna("vert").Horizon
	i := int(90 / h.StepDeg)
	want := geo.ElevationDeg(20, 108, 1200, 0)
	near(t, "cone skyline from tiles", h.ElevationDeg[i], want, 0.05)

	// Offline from the cache: identical result, no network.
	ts.srv.Close()
	prov2 := NewProvider(ProviderOptions{Dir: filepath.Join(dir, "tiles"), URLTemplate: ts.url(), Offline: true})
	s2 := startService(t, ServiceOptions{Provider: prov2}) // no polar cache dir: recompute from tiles
	s2.Submit(req, false)
	if st := waitJob(t, s2, req.Key()); st.State != StateDone || st.Tiles.Cached != st.Tiles.Total {
		t.Fatalf("offline status %+v", st)
	}
	near(t, "offline skyline", s2.Analysis().Antenna("vert").Horizon.ElevationDeg[i], h.ElevationDeg[i], 1e-9)

	// Without any tiles: sea level with the no-data flag, but a result.
	prov3 := NewProvider(ProviderOptions{Dir: filepath.Join(dir, "empty"), Offline: true})
	s3 := startService(t, ServiceOptions{Provider: prov3})
	s3.Submit(req, false)
	if st := waitJob(t, s3, req.Key()); st.State != StateDone {
		t.Fatalf("no-data status %+v", st)
	}
	a3 := s3.Analysis()
	if a3.DataComplete || a3.NoDataFraction != 1 || a3.DEMGroundOK || a3.GroundSource != GroundConfig {
		t.Errorf("no-data analysis complete=%v nodata=%v groundOK=%v", a3.DataComplete, a3.NoDataFraction, a3.DEMGroundOK)
	}
}

// ---------------------------------------------------------------------------
// HTTP API

func getJSON(t *testing.T, h http.Handler, method, target string, wantStatus int, v any) []byte {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s: status %d, want %d: %s", method, target, rec.Code, wantStatus, body)
	}
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			t.Fatalf("%s: %v: %.200s", target, err, body)
		}
	}
	if bytes.Contains(body, []byte("NaN")) || bytes.Contains(body, []byte("Inf")) {
		t.Errorf("%s: non-finite number in JSON", target)
	}
	return body
}

func TestHTTPAPI(t *testing.T) {
	s := startService(t, ServiceOptions{Source: ridgeSource()})
	mux := http.NewServeMux()
	var adminCalls int
	s.Mount(mux, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { adminCalls++; h.ServeHTTP(w, r) })
	})

	// Before any analysis.
	var sr StatusResponse
	getJSON(t, mux, "GET", "/api/terrain/status", 200, &sr)
	if sr.Available || sr.Job.State != StateIdle || sr.Attribution == "" {
		t.Errorf("status %+v", sr)
	}
	getJSON(t, mux, "GET", "/api/terrain/coverage", 503, nil)
	getJSON(t, mux, "POST", "/api/terrain/recompute", 409, nil)
	getJSON(t, mux, "GET", "/api/terrain/visible?lat=50&lon=10", 400, nil)

	req := testRequest()
	s.Submit(req, false)
	waitJob(t, s, req.Key())

	getJSON(t, mux, "GET", "/api/terrain/status", 200, &sr)
	if !sr.Available || sr.Key != req.Key() || sr.Job.State != StateDone {
		t.Errorf("status %+v", sr)
	}
	var a Analysis
	getJSON(t, mux, "GET", "/api/terrain/coverage", 200, &a)
	if len(a.Antennas) != 2 || len(a.Antennas[0].LOS) != 3 || len(a.Antennas[0].Horizon.ElevationDeg) != 180 {
		t.Errorf("coverage %d antennas", len(a.Antennas))
	}

	var fc FeatureCollection
	getJSON(t, mux, "GET", "/api/terrain/coverage.geojson?ant=vert&kind=los", 200, &fc)
	if fc.Type != "FeatureCollection" || len(fc.Features) != 3 {
		t.Fatalf("geojson %+v", fc.Type)
	}
	// Highest altitude first; rings closed.
	if fc.Features[0].Properties["altFt"].(float64) != 30000 {
		t.Errorf("first feature %v", fc.Features[0].Properties)
	}
	ring := fc.Features[0].Geometry.Coordinates.([]any)[0].([]any)
	if len(ring) != 181 || ring[0].([]any)[0] != ring[180].([]any)[0] {
		t.Errorf("ring length %d", len(ring))
	}
	getJSON(t, mux, "GET", "/api/terrain/coverage.geojson?kind=radio", 200, &fc)
	if fc.Features[0].Properties["kind"] != KindRadio {
		t.Errorf("radio kind %v", fc.Features[0].Properties["kind"])
	}
	getJSON(t, mux, "GET", "/api/terrain/coverage.geojson?ant=nope", 404, nil)

	var hz Horizon
	getJSON(t, mux, "GET", "/api/terrain/horizon?ant=yagi", 200, &hz)
	if hz.AntennaID != "yagi" || len(hz.BearingDeg) != 180 || hz.MaxSkylineDeg <= 0 {
		t.Errorf("horizon %+v", hz.AntennaID)
	}
	var adv SiteAdvice
	getJSON(t, mux, "GET", "/api/terrain/advice", 200, &adv)
	if len(adv.Sectors) != 36 || len(adv.Summary) == 0 {
		t.Errorf("advice %+v", adv)
	}
	checkAdviceLines(t, &adv)
	// Per-antenna advice, also inside the coverage document.
	var advY SiteAdvice
	getJSON(t, mux, "GET", "/api/terrain/advice?ant=yagi", 200, &advY)
	if advY.AntennaID != "yagi" || len(advY.Sectors) != 36 {
		t.Errorf("advice?ant=yagi: %q", advY.AntennaID)
	}
	checkAdviceLines(t, &advY)
	getJSON(t, mux, "GET", "/api/terrain/advice?ant=nope", 404, nil)
	var cv struct {
		Advice   SiteAdvice         `json:"advice"`
		Antennas []*AntennaCoverage `json:"antennas"`
	}
	getJSON(t, mux, "GET", "/api/terrain/coverage?ant=yagi", 200, &cv)
	if cv.Advice.AntennaID != "yagi" || len(cv.Antennas) != 2 {
		t.Errorf("coverage?ant=yagi advice %q, %d antennas", cv.Advice.AntennaID, len(cv.Antennas))
	}
	getJSON(t, mux, "GET", "/api/terrain/coverage?ant=nope", 404, nil)
	var fg FloorGrid
	getJSON(t, mux, "GET", "/api/terrain/floor?cell=0.1", 200, &fg)
	if fg.Rows*fg.Cols != len(fg.FloorFt) || fg.Rows == 0 || fg.HeatAligned {
		t.Errorf("floor grid %dx%d len %d", fg.Rows, fg.Cols, len(fg.FloorFt))
	}
	getJSON(t, mux, "GET", "/api/terrain/floor", 200, &fg)
	if !fg.HeatAligned || fg.CellDeg != geo.HeatCellDeg {
		t.Errorf("default floor grid cell %g", fg.CellDeg)
	}

	var fi FloorImageResponse
	getJSON(t, mux, "GET", "/api/terrain/floor-image?w=256", 200, &fi)
	if fi.FloorImage == nil || fi.Width != 256 || fi.Height < 100 || !strings.HasPrefix(fi.URL, "/api/terrain/floor.png?") {
		t.Fatalf("floor image %+v", fi)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", fi.URL, nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("floor.png %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	cfg, err := png.DecodeConfig(rec.Body)
	if err != nil || cfg.Width != 256 || cfg.Height != fi.Height {
		t.Errorf("floor.png %v %+v", err, cfg)
	}

	o := newOrigin(stLat, stLon)
	nLa, nLo := o.destKm(0, 100)
	q := url.Values{"lat": {ftoa(nLa)}, "lon": {ftoa(nLo)}, "altFt": {"3000"}}
	var vis Visibility
	getJSON(t, mux, "GET", "/api/terrain/visible?"+q.Encode(), 200, &vis)
	if !vis.Known || vis.Visible {
		t.Errorf("visible %+v", vis)
	}
	var lr LOSResult
	getJSON(t, mux, "GET", "/api/terrain/los?ant=vert&"+q.Encode(), 200, &lr)
	if lr.Clear || lr.Obstruction == nil || len(lr.Profile) == 0 {
		t.Errorf("los clear=%v", lr.Clear)
	}
	var lr0 LOSResult
	getJSON(t, mux, "GET", "/api/terrain/los?profile=0&"+q.Encode(), 200, &lr0)
	if len(lr0.Profile) != 0 || lr0.Clear {
		t.Error("profile returned with profile=0")
	}
	getJSON(t, mux, "GET", "/api/terrain/los?ant=nope&"+q.Encode(), 404, nil)
	// Effective earth factor: clamped, reported, invalid rejected.
	var lrK LOSResult
	getJSON(t, mux, "GET", "/api/terrain/los?profile=0&k=50&"+q.Encode(), 200, &lrK)
	if lrK.K != MaxLOSK {
		t.Errorf("k=50 not clamped: %g", lrK.K)
	}
	getJSON(t, mux, "GET", "/api/terrain/los?profile=0&k=0.1&"+q.Encode(), 200, &lrK)
	if lrK.K != MinLOSK {
		t.Errorf("k=0.1 not clamped: %g", lrK.K)
	}
	getJSON(t, mux, "GET", "/api/terrain/los?k=abc&"+q.Encode(), 400, nil)
	var er ElevationResponse
	getJSON(t, mux, "GET", "/api/terrain/elevation?"+q.Encode(), 200, &er)
	if !er.OK || er.ElevM != 0 || er.Zoom != 12 {
		t.Errorf("elevation %+v", er)
	}
	ridgeLa, ridgeLo := o.destKm(0, 30.5)
	getJSON(t, mux, "GET", "/api/terrain/elevation?lat="+ftoa(ridgeLa)+"&lon="+ftoa(ridgeLo), 200, &er)
	if er.ElevM != 500 {
		t.Errorf("ridge elevation %+v", er)
	}
	// Requests from the network stay inside the analysed area (they would
	// otherwise download DEM tiles anywhere on Earth) and z is capped.
	getJSON(t, mux, "GET", "/api/terrain/los?lat=-33.9&lon=151.2&altFt=30000", 400, nil)
	getJSON(t, mux, "GET", "/api/terrain/elevation?z=15&"+q.Encode(), 200, &er)
	if er.Zoom != HTTPMaxElevationZoom {
		t.Errorf("elevation zoom not capped: %+v", er)
	}

	// Admin routes go through the wrapper.
	var started map[string]any
	getJSON(t, mux, "POST", "/api/terrain/recompute", 202, &started)
	if started["started"] != true {
		t.Errorf("recompute %v", started)
	}
	waitJob(t, s, req.Key())
	getJSON(t, mux, "POST", "/api/terrain/cancel", 200, nil)
	if adminCalls != 3 { // recompute (409) + recompute + cancel
		t.Errorf("admin wrapper called %d times", adminCalls)
	}
	getJSON(t, mux, "GET", "/api/terrain/recompute", 405, nil)
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// TestLiveTerrarium downloads real tiles from AWS. Run with
// AIRVIEW_LIVE_TERRAIN=1 go test ./internal/terrain -run Live -v
func TestLiveTerrarium(t *testing.T) {
	if os.Getenv("AIRVIEW_LIVE_TERRAIN") == "" {
		t.Skip("set AIRVIEW_LIVE_TERRAIN=1 to download real terrain tiles")
	}
	p := NewProvider(ProviderOptions{Dir: t.TempDir(), Zoom: 12})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cases := []struct {
		name          string
		lat, lon      float64
		want, tolM    float64
		z             int
		wantSeaClamp0 bool
	}{
		// Zugspitze summit 2962 m (DEM peaks are smoothed: generous tolerance).
		{"Zugspitze", 47.42116, 10.98530, 2962, 150, 12, false},
		// Lake Constance surface ≈ 395 m.
		{"Bodensee", 47.6000, 9.4000, 395, 15, 12, false},
		// Mont Blanc summit 4806 m.
		{"Mont Blanc", 45.83265, 6.86517, 4806, 200, 12, false},
		// North Sea (bathymetry clamped to 0).
		{"North Sea", 54.5, 4.0, 0, 0.001, 10, true},
	}
	for _, c := range cases {
		res, n, err := p.Fetch(ctx, TileAt(c.lat, c.lon, c.z))
		if err != nil {
			t.Fatalf("%s: %s %v", c.name, res, err)
		}
		e, ok := p.ElevationAtZoom(c.z, c.lat, c.lon)
		t.Logf("%s: tile %v %s %d bytes → %.1f m (ok=%v)", c.name, TileAt(c.lat, c.lon, c.z), res, n, e, ok)
		if !ok || math.Abs(e-c.want) > c.tolM {
			t.Errorf("%s: %.1f m, want %.0f ± %.0f", c.name, e, c.want, c.tolM)
		}
	}
	// A coarse prefetch with progress, then everything cached.
	pr, err := p.Prefetch(ctx, 47.4, 11.0, 30, 9, nil)
	if err != nil || pr.Failed != 0 {
		t.Fatalf("prefetch %+v %v", pr, err)
	}
	pr2, _ := p.Prefetch(ctx, 47.4, 11.0, 30, 9, nil)
	if pr2.Cached != pr2.Total {
		t.Errorf("second prefetch %+v", pr2)
	}
	// LOS from the Zugspitze to an aircraft over Munich at FL100.
	if _, err := p.PrefetchPath(ctx, 47.42116, 10.98530, 48.14, 11.58, 11); err != nil {
		t.Fatal(err)
	}
	r := LOS(p.At(11), LOSInput{RxLat: 47.42116, RxLon: 10.98530, GroundFromDEM: true, RxAGLM: 5, TgtLat: 48.14, TgtLon: 11.58, TgtAltM: 3048})
	t.Logf("Zugspitze → Munich FL100: clear=%v groundKm=%.1f minClear=%.0f m fresnel=%.2f loss=%.1f dB noData=%d",
		r.Clear, r.GroundKm, r.MinClearanceM, r.FresnelRatio, r.TotalLossDB, r.NoDataSamples)
}
