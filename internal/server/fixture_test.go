package server

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/engine"
	"airview/internal/met"
	"airview/internal/model"
	"airview/internal/navdata"
	"airview/internal/predict"
	"airview/internal/sim"
	"airview/internal/store"
	"airview/internal/tracker"
	"airview/internal/weather"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// seedHistory fills an empty data directory with hours of simulated traffic
// (through the real tracker and met collector, replayed as fast as the CPU
// allows), synthetic hourly weather with pressure-level profiles, a
// forecast and NTP samples, so that the analyses have something to show.
func seedHistory(tb testing.TB, dir string, hours float64, aircraft int) {
	tb.Helper()
	ctx := context.Background()
	st, err := store.OpenWithOptions(dir, store.Options{Logger: quietLogger()})
	if err != nil {
		tb.Fatalf("seed: open store: %v", err)
	}
	cfg := model.ExampleRadarcapeConfig()
	cfg.Receiver.Located = true
	cfg.Receiver.Lat, cfg.Receiver.Lon, cfg.Receiver.GroundElevM = sim.DefaultLat, sim.DefaultLon, sim.DefaultGroundElevM
	cfg.Feeds = []model.Feed{{ID: engine.DemoFeedID, Name: "Simulator (demo)", Enabled: true, Type: model.FeedDemo}}
	tr := tracker.New(tracker.Options{Store: st, Config: &cfg, Logger: quietLogger()})
	mc := met.New(st, met.Options{Logger: quietLogger()})
	tr.SetEHSHook(func(ev tracker.EHSEvent) tracker.EHSResult {
		d := mc.Observe(met.Input{
			T: ev.T, ICAO: ev.ICAO, Lat: ev.Lat, Lon: ev.Lon, AltFt: ev.AltFt, GS: ev.GS, Track: ev.Track,
			TAS: ev.TAS, IAS: ev.IAS, Mach: ev.Mach, MagHeading: ev.MagHeading, Roll: ev.Roll,
			MRARWindSpd: ev.MRARWindSpd, MRARWindDir: ev.MRARWindDir, MRARTemp: ev.MRARTemp,
		})
		return tracker.EHSResult{WindDir: d.WindDir, WindSpd: d.WindSpd, OAT: d.OAT, HasWind: d.HasWind, HasOAT: d.HasOAT}
	})

	now := time.Now()
	start := now.Add(-time.Duration(hours * float64(time.Hour)))
	sm := sim.New(sim.Config{
		Aircraft: aircraft, Receiver: cfg.Receiver, Antennas: cfg.Antennas, FeedID: engine.DemoFeedID,
		Seed: 7, Start: start, EmergencyPerHour: 0.02,
	})
	step := 10 * time.Minute
	for t := start; t.Before(now.Add(-time.Minute)); t = t.Add(step) {
		d := step
		if rem := now.Add(-time.Minute).Sub(t); rem < d {
			d = rem
		}
		if err := sm.Advance(ctx, d, tr); err != nil {
			tb.Fatalf("seed: simulate: %v", err)
		}
		mc.Flush()
	}
	if err := tr.Flush(ctx); err != nil {
		tb.Fatalf("seed: tracker flush: %v", err)
	}
	mc.Flush()
	_ = mc.SaveBiases(ctx)

	// Weather: a diurnal cycle with a pressure wave and a night-time
	// inversion, so that refractivity varies.
	elev := cfg.Receiver.GroundElevM
	h0 := start.Unix() / 3600 * 3600
	nowH := now.Unix() / 3600 * 3600
	var obs, fc []store.Weather
	for h := h0 - 3600; h <= nowH+72*3600; h += 3600 {
		hp := syntheticHour(h)
		row := predict.RowFromHour(&hp, elev)
		if h <= nowH {
			obs = append(obs, row)
		}
		if h >= nowH {
			row.Issued = nowH - 1800
			fc = append(fc, row)
		}
	}
	st.AddWeatherObs(obs)
	st.SetWeatherForecast(fc)

	var ntpRows []store.NTPSample
	for t := start.Unix(); t < now.Unix(); t += 64 {
		x := float64(t-start.Unix()) / 3600
		ntpRows = append(ntpRows, store.NTPSample{
			T: t, Server: "pool.ntp.org", OffsetMs: 3 + 1.5*math.Sin(x) + 0.2*math.Sin(x*37), DelayMs: 18 + 4*math.Abs(math.Sin(x*11)), Stratum: 2,
		})
	}
	st.AddNTPSamples(ntpRows)
	seedNavData(tb, st)
	if err := st.Flush(); err != nil {
		tb.Fatalf("seed: flush: %v", err)
	}
	if _, err := st.RollupHours(ctx); err != nil {
		tb.Fatalf("seed: rollup: %v", err)
	}
	if err := st.Close(); err != nil {
		tb.Fatalf("seed: close: %v", err)
	}
}

// Small OurAirports / standing-data fixtures around the demo site (the real
// column layout).
const (
	fixAirports = `"id","ident","type","name","latitude_deg","longitude_deg","elevation_ft","continent","iso_country","iso_region","municipality","scheduled_service","icao_code","iata_code","gps_code","local_code","home_link","wikipedia_link","keywords"
2212,"EDDF","large_airport","Frankfurt am Main Airport",50.030241,8.561096,364,"EU","DE","DE-HE","Frankfurt am Main","yes","EDDF","FRA","EDDF",,,,
2218,"EDDK","large_airport","Cologne Bonn Airport",50.865917,7.142744,302,"EU","DE","DE-NW","Cologne","yes","EDDK","CGN","EDDK",,,,
2214,"EDDM","large_airport","Munich Airport",48.353802,11.7861,1487,"EU","DE","DE-BY","Munich","yes","EDDM","MUC","EDDM",,,,
2229,"EDFH","medium_airport","Frankfurt-Hahn Airport",49.9487,7.26389,1649,"EU","DE","DE-RP","Hahn","yes","EDFH","HHN","EDFH",,,,
3622,"KJFK","large_airport","John F Kennedy International Airport",40.639447,-73.779317,13,"NA","US","US-NY","New York","yes","KJFK","JFK","KJFK","JFK",,,
2434,"EGLL","large_airport","London Heathrow Airport",51.4706,-0.461941,83,"EU","GB","GB-ENG","London","yes","EGLL","LHR","EGLL",,,,
`
	fixRunways = `"id","airport_ref","airport_ident","length_ft","width_ft","surface","lighted","closed","le_ident","le_latitude_deg","le_longitude_deg","le_elevation_ft","le_heading_degT","le_displaced_threshold_ft","he_ident","he_latitude_deg","he_longitude_deg","he_elevation_ft","he_heading_degT","he_displaced_threshold_ft"
240001,2212,"EDDF",13123,148,"CON",1,0,"07C",50.032692,8.534564,328,69.9,,"25C",50.045030,8.586981,364,249.9,
240002,2212,"EDDF",9240,148,"CON",1,0,"18",50.034444,8.525833,326,180,,"36",49.998611,8.526389,312,0,
`
	fixNavaids = `"id","filename","ident","name","type","frequency_khz","latitude_deg","longitude_deg","elevation_ft","iso_country","dme_frequency_khz","dme_channel","dme_latitude_deg","dme_longitude_deg","dme_elevation_ft","slaved_variation_deg","magnetic_variation_deg","usageType","power","associated_airport"
85001,"Frankfurt_VOR-DME_DE","FFM","Frankfurt","VOR-DME",114200,50.053611,8.637222,328,"DE",,"89X",,,,,2.5,"BOTH","HIGH","EDDF"
`
	fixRoutes = "\xef\xbb\xbfCallsign,Code,Number,AirlineCode,AirportCodes\n" +
		"DLH400,DLH,400,DLH,EDDF-KJFK\n" +
		"DLH900,DLH,900,DLH,EDDF-EGLL\n"
)

// seedNavData imports the reference data fixtures.
func seedNavData(tb testing.TB, st *store.Store) {
	tb.Helper()
	nd := navdata.New(st, navdata.Options{Logger: quietLogger()})
	ctx := context.Background()
	for _, d := range []struct {
		ds   navdata.Dataset
		data string
	}{{navdata.DatasetAirports, fixAirports}, {navdata.DatasetRunways, fixRunways}, {navdata.DatasetNavaids, fixNavaids}, {navdata.DatasetRoutes, fixRoutes}} {
		if _, err := nd.ImportReader(ctx, d.ds, strings.NewReader(d.data), nil); err != nil {
			tb.Fatalf("seed: navdata %s: %v", d.ds, err)
		}
	}
}

// syntheticHour builds one plausible Open-Meteo hour with a profile.
func syntheticHour(h int64) weather.HourPoint {
	x := float64(h) / 3600
	hod := math.Mod(x, 24)
	diurnal := math.Cos((hod - 15) / 24 * 2 * math.Pi) // warmest at 15 UTC
	temp := 11 + 6*diurnal
	rh := 72 - 18*diurnal
	p := 1016 + 7*math.Sin(x/40)
	n := func(v float64) weather.Num { return weather.Num(v) }
	hp := weather.HourPoint{T: h * 1000}
	hp.TempC, hp.RH, hp.DewPointC = n(temp), n(rh), n(temp-(100-rh)/5)
	hp.PressureMSL, hp.SurfacePressure = n(p), n(p-13)
	hp.WindKmh, hp.WindDir, hp.GustKmh = n(12+6*math.Sin(x/9)), n(math.Mod(230+40*math.Sin(x/17)+360, 360)), n(22+8*math.Sin(x/9))
	hp.Cloud, hp.CloudLow, hp.CloudMid, hp.CloudHigh = n(50+40*math.Sin(x/13)), n(30+25*math.Sin(x/13)), n(20), n(35)
	hp.PrecipMM, hp.RainMM, hp.SnowfallCM = n(math.Max(0, math.Sin(x/13)-0.8)*4), n(math.Max(0, math.Sin(x/13)-0.8)*4), n(0)
	hp.VisibilityM, hp.CAPE, hp.WeatherCode = n(24000-9000*math.Max(0, -diurnal)), n(math.Max(0, 300*diurnal)), n(2)
	hp.IsDay = n(0)
	if hod > 6 && hod < 18 {
		hp.IsDay = n(1)
	}
	hp.PrecipProb, hp.BoundaryLayerM, hp.LiftedIndex, hp.CIN, hp.FreezingLevelM = n(20), n(700+500*diurnal), n(2), n(-10), n(2600)
	// Night-time surface inversion with a sharp humidity drop: enhanced
	// refraction, on some nights a duct.
	inv := math.Max(0, -diurnal) * (1.5 + math.Sin(x/50))
	for _, hpa := range weather.DefaultLevels {
		z := 44330 * (1 - math.Pow(float64(hpa)/p, 0.1903))
		t := temp - 0.0065*z
		r := rh - z/120
		if z < 600 {
			t += inv * 3 * (z / 600)
			r -= inv * 30 * (z / 600)
		} else {
			t += inv * 3 * math.Exp(-(z-600)/800)
			r -= inv * 30
		}
		hp.Levels = append(hp.Levels, weather.Level{
			HPa: float64(hpa), TempC: n(t), RH: n(math.Max(5, math.Min(100, r))), HeightM: n(z + 110),
			WindKmh: n(20 + z/150), WindDir: n(math.Mod(240+z/200, 360)),
		})
	}
	return hp
}

// rig is an engine in demo mode plus a server.
type rig struct {
	tb  testing.TB
	dir string
	eng *engine.Engine
	srv *Server
}

type rigOptions struct {
	seedHours float64 // 0 = empty database
	aircraft  int
	listen    bool // Start the server on a free port (needed for SSE)
}

func freePort(tb testing.TB) int {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newRigIn(tb testing.TB, dir string, o rigOptions) *rig {
	tb.Helper()
	if o.seedHours > 0 {
		n := o.aircraft
		if n == 0 {
			n = 25
		}
		seedHistory(tb, dir, o.seedHours, n)
	}
	eng := engine.New(dir, engine.Options{Demo: true, Offline: true, DemoSeed: 42, Logger: quietLogger()})
	if err := eng.Start(context.Background()); err != nil {
		tb.Fatalf("engine start: %v", err)
	}
	opts := Options{Version: "test"}
	if o.listen {
		opts.PortOverride = freePort(tb)
	}
	srv := New(eng, opts)
	srv.log = quietLogger()
	srv.auth.failDelay = 0
	srv.auth.net.testInsecureOK = true // most tests log in from documentation addresses over plain HTTP; see TestInsecureLoginRefused
	srv.elevation = func(ctx context.Context, lat, lon float64) (float64, error) { return 123.4, nil }
	r := &rig{tb: tb, dir: dir, eng: eng, srv: srv}
	// The terrain analysis (synthetic terrain offline) runs in the
	// background right after start: the terrain endpoints answer 503 until
	// it is there.
	for deadline := time.Now().Add(60 * time.Second); eng.Terrain().Analysis() == nil; {
		if time.Now().After(deadline) {
			eng.Stop()
			tb.Fatalf("terrain analysis did not finish: %+v", eng.Terrain().Status())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if o.listen {
		if _, _, err := srv.Start(context.Background()); err != nil {
			eng.Stop()
			tb.Fatalf("server start: %v", err)
		}
	}
	return r
}

func newRig(t *testing.T, o rigOptions) *rig {
	t.Helper()
	r := newRigIn(t, t.TempDir(), o)
	t.Cleanup(r.close)
	return r
}

func (r *rig) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.srv.Shutdown(ctx)
	r.eng.Stop()
}

const (
	loopbackAddr = "127.0.0.1:50000"
	lanAddr      = "192.168.1.77:50000"
)

// do sends a request straight to the handler with a chosen peer address.
func (r *rig) do(method, target, remote, body string, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.RemoteAddr = remote
	req.Host = "192.168.1.10:8080"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (r *rig) get(target string) *httptest.ResponseRecorder {
	return r.do(http.MethodGet, target, loopbackAddr, "")
}

// waitTraffic waits until the demo feed delivered aircraft with positions.
func (r *rig) waitTraffic(n int) {
	r.tb.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		c := r.eng.Tracker().Snapshot().Counts
		if c.WithPos >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.tb.Fatalf("no demo traffic: %+v", r.eng.Tracker().Snapshot().Counts)
}

// shared is the read-only rig used by the tests that do not change state.
var (
	sharedOnce sync.Once
	sharedRig  *rig
	sharedDir  string
)

func shared(t *testing.T) *rig {
	t.Helper()
	sharedOnce.Do(func() {
		dir, err := os.MkdirTemp("", "airview-server-test-")
		if err != nil {
			t.Fatal(err)
		}
		sharedDir = dir
		sharedRig = newRigIn(t, dir, rigOptions{seedHours: 5, aircraft: 20, listen: true})
		sharedRig.waitTraffic(3)
		// Make the live data of the last seconds visible to the analyses.
		_ = sharedRig.eng.Tracker().Flush(context.Background())
		_ = sharedRig.eng.Store().Flush()
	})
	if sharedRig == nil {
		t.Fatal("shared rig failed to start")
	}
	sharedRig.tb = t
	return sharedRig
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedRig != nil {
		sharedRig.close()
	}
	if sharedDir != "" {
		_ = os.RemoveAll(sharedDir)
	}
	os.Exit(code)
}
