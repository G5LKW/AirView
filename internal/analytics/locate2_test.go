package analytics

import (
	"context"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"airview/internal/geo"
)

// Locate Me v2 against simulated truth (l2World, locate2_world_test.go).
//
// The acceptance criterion of DESIGN §10.4 / WP-L2 is "error < 10 km
// typical on simnet / recorded-like data". The default tests check a few
// fixed seeds per configuration; TestLocate2Eval (AIRVIEW_LOCATE2_EVAL=<n>)
// produces the distribution reported in locate2_doc.go.

type l2Mode struct {
	name           string
	terr, useDEM   bool
	hours          float64
	perHour        int
	noRSSI         bool
	medianLimitKm  float64 // gate on the median error
	coverMin       float64 // gate on the share of sites inside the 95 % circle
	shortSeedCount int
}

var l2Modes = []l2Mode{
	{"smooth 24h", false, false, 24, 100, false, 3, 0.8, 2},
	{"smooth 6h", false, false, 6, 100, false, 3, 0.8, 1},
	{"smooth 24h quiet", false, false, 24, 30, false, 3, 0.8, 1},
	{"terrain+DEM 24h", true, true, 24, 100, false, 3, 0.7, 1},
	{"terrain noDEM 24h", true, false, 24, 100, false, 5, 0.7, 1},
	{"smooth 24h no RSSI", false, false, 24, 100, true, 20, 0.7, 1},
	{"terrain noDEM 24h no RSSI", true, false, 24, 100, true, 25, 0.5, 1},
	{"terrain+DEM 24h no RSSI", true, true, 24, 100, true, 10, 0.5, 1},
}

// l2Run locates one synthetic world and returns the error and the result.
func l2Run(t testing.TB, seed int64, m l2Mode) (float64, *Locate2Result) {
	t.Helper()
	w := newL2World(t, seed, m.terr)
	ss := w.generate(seed, m.hours, m.perHour)
	if m.noRSSI {
		for i := range ss {
			ss[i].RSSI = math.NaN()
		}
	}
	opt := Locate2Options{}
	if m.useDEM {
		opt.Terrain = w.Terrain
	}
	r, err := Locate2(context.Background(), ss, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Fatalf("seed %d: no estimate: %s", seed, r.Reason)
	}
	return geo.HaversineKm(w.Lat, w.Lon, r.Lat, r.Lon), r
}

// l2Report logs the error distribution and returns its quantile function.
func l2Report(t testing.TB, name string, errs []float64) func(float64) float64 {
	t.Helper()
	s := append([]float64(nil), errs...)
	sort.Float64s(s)
	q := func(p float64) float64 { return s[int(math.Round(p*float64(len(s)-1)))] }
	t.Logf("%-28s n=%-3d P50 %6.2f km  P90 %6.2f km  max %6.2f km", name, len(s), q(0.5), q(0.9), s[len(s)-1])
	return q
}

func TestLocate2Synthetic(t *testing.T) {
	for _, m := range l2Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			n := 3
			if testing.Short() {
				n = m.shortSeedCount
			}
			var errs []float64
			for seed := int64(101); seed < int64(101+n); seed++ {
				e, r := l2Run(t, seed, m)
				errs = append(errs, e)
				t.Logf("seed %d: error %.2f km  sigma %.2f  r95 %.2f  antenna %v m  %d samples  %d ms",
					seed, e, float64(r.SigmaKm), float64(r.R95Km), float64(r.AntennaM), r.Samples, r.ElapsedMs)
				mustJSON(t, "locate2", r)
			}
			lim := m.medianLimitKm
			if testing.Short() {
				lim *= 2 // one site: no median to speak of
			}
			if q := l2Report(t, m.name, errs); q(0.5) > lim {
				t.Errorf("median error %.2f km > %.0f km", q(0.5), lim)
			}
		})
	}
}

// TestLocate2Eval is the long evaluation behind the reported distribution
// (AIRVIEW_LOCATE2_EVAL=<sites per configuration>). It also gates the
// calibration of the 95 % circle.
func TestLocate2Eval(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("AIRVIEW_LOCATE2_EVAL"))
	if n <= 0 {
		t.Skip("set AIRVIEW_LOCATE2_EVAL=<number of sites>")
	}
	for _, m := range l2Modes {
		var errs, ratios, raised []float64
		cover := 0
		t0 := time.Now()
		var ms []float64
		for seed := int64(1000); seed < int64(1000+n); seed++ {
			e, r := l2Run(t, seed, m)
			errs = append(errs, e)
			ratios = append(ratios, e/float64(r.R95Km))
			if isNum(float64(r.SkylineRaised)) {
				raised = append(raised, float64(r.SkylineRaised))
			}
			ms = append(ms, float64(r.ElapsedMs))
			if e <= float64(r.R95Km) {
				cover++
			}
		}
		q := l2Report(t, m.name, errs)
		l2Report(t, m.name+" error/r95", ratios)
		if len(raised) > 0 {
			l2Report(t, m.name+" skyline raised", raised)
		}
		sort.Float64s(ms)
		t.Logf("%-28s truth inside the 95 %% circle in %d of %d sites; run time P50 %.0f ms max %.0f ms; total %v",
			m.name, cover, n, ms[len(ms)/2], ms[len(ms)-1], time.Since(t0).Round(time.Second))
		if q(0.5) > m.medianLimitKm {
			t.Errorf("%s: median error %.2f km > %.0f km", m.name, q(0.5), m.medianLimitKm)
		}
		if float64(cover) < m.coverMin*float64(n) {
			t.Errorf("%s: 95 %% circle covers the truth in only %d of %d sites", m.name, cover, n)
		}
	}
}

func TestLocate2NotEnoughData(t *testing.T) {
	r, err := Locate2(context.Background(), []L2Sample{{T: 1, Lat: 50, Lon: 8, AltFt: 1000}}, Locate2Options{})
	if err != nil || r.OK || r.Reason == "" || r.Samples != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	mustJSON(t, "locate2", r)
	// many samples of too few aircraft
	var ss []L2Sample
	for i := 0; i < 1000; i++ {
		ss = append(ss, L2Sample{T: int64(1+i) * 10_000, ICAO: uint32(i % 3), Lat: 50 + float64(i)*1e-3, Lon: 8, AltFt: 10000, RSSI: -20})
	}
	r, err = Locate2(context.Background(), ss, Locate2Options{})
	if err != nil || r.OK || r.Sessions != 3 {
		t.Fatalf("%+v %v", r, err)
	}
	// invalid samples are ignored
	bad := []L2Sample{{T: 5, Lat: math.NaN(), Lon: 8, AltFt: 1}, {T: 5, Lat: 95, Lon: 8, AltFt: 1}, {T: 0, Lat: 1, Lon: 1, AltFt: 1}, {T: 5, Lat: 1, Lon: 1, AltFt: math.NaN()}}
	if r, _ := Locate2(context.Background(), bad, Locate2Options{}); r.Samples != 0 {
		t.Fatalf("invalid samples counted: %d", r.Samples)
	}
}

func TestLocate2Cancel(t *testing.T) {
	w := newL2World(t, 7, false)
	ss := w.generate(7, 6, 100)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Locate2(ctx, ss, Locate2Options{}); err == nil {
		t.Fatal("cancelled run returned no error")
	}
}

// TestLocate2Antimeridian checks a station next to 180°: the search plane
// and the robust centre must not wrap.
func TestLocate2Antimeridian(t *testing.T) {
	w := newL2World(t, 3, false)
	w.Lat, w.Lon = -17.75, 179.9
	for i := range w.Airports {
		la, lo := geo.Destination(w.Lat, w.Lon, float64(i)*97, 20+float64(i)*25)
		w.Airports[i] = [3]float64{la, lo, 0}
	}
	ss := w.generate(3, 12, 100)
	r, err := Locate2(context.Background(), ss, Locate2Options{})
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if e := geo.HaversineKm(w.Lat, w.Lon, r.Lat, r.Lon); e > 5 {
		t.Errorf("error %.1f km", e)
	}
	if r.Lon > 180 || r.Lon < -180 {
		t.Errorf("lon %v", r.Lon)
	}
}

// l2Timing builds T=1 timing tracks for the station of w from samples:
// stable aircraft have a constant latency (plus 100 ns noise), jittery ones
// a random one per message (the common case).
func l2Timing(w *l2World, ss []L2Sample, stable, jittery int, seed int64) []L2TimingTrack {
	rng := rand.New(rand.NewSource(seed))
	by := map[uint32][]L2Sample{}
	for _, s := range ss {
		if !s.Ground && isNum(s.AltFt) && s.AltFt > 15000 {
			by[s.ICAO] = append(by[s.ICAO], s)
		}
	}
	var keys []uint32
	for k, v := range by {
		if len(v) >= 40 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	rx := w.GroundM + w.AGL
	var out []L2TimingTrack
	for i, k := range keys {
		if i >= stable+jittery {
			break
		}
		lat0 := 50e3 + rng.Float64()*100e3
		tr := L2TimingTrack{ICAO: k}
		for _, s := range by[k] {
			hM := geo.FeetToMetres(s.AltFt)
			ra, rr := earthM+hM, earthM+rx
			ax, ay, az := unitVec(s.Lat, s.Lon)
			bx, by, bz := unitVec(w.Lat, w.Lon)
			slant := math.Sqrt(ra*ra + rr*rr - 2*ra*rr*(ax*bx+ay*by+az*bz))
			lat := lat0 + rng.NormFloat64()*100
			if i >= stable {
				lat = rng.Float64() * 150e6
			}
			tr.Points = append(tr.Points, L2TimingPoint{Lat: s.Lat, Lon: s.Lon, AltM: hM, MNs: lat + slant/lightMPerNs})
		}
		out = append(out, tr)
	}
	return out
}

func TestLocate2Timing(t *testing.T) {
	w := newL2World(t, 21, false)
	ss := w.generate(21, 12, 100)
	for i := range ss {
		ss[i].RSSI = math.NaN() // horizon only, so the timing term shows
	}
	base, err := Locate2(context.Background(), ss, Locate2Options{})
	if err != nil || !base.OK {
		t.Fatal(err, base.Reason)
	}
	tracks := l2Timing(w, ss, 6, 20, 5)
	n := 0
	for _, tr := range tracks {
		if _, ok := l2UsableTrack(tr); ok {
			n++
		}
	}
	if n != 6 {
		t.Fatalf("usable tracks %d, want the 6 stable ones", n)
	}
	r, err := Locate2(context.Background(), ss, Locate2Options{Timing: tracks})
	if err != nil || !r.OK {
		t.Fatal(err)
	}
	e0 := geo.HaversineKm(w.Lat, w.Lon, base.Lat, base.Lon)
	e1 := geo.HaversineKm(w.Lat, w.Lon, r.Lat, r.Lon)
	t.Logf("horizon only %.2f km (r95 %.1f); with 6 stable T=1 tracks %.2f km (r95 %.1f)", e0, float64(base.R95Km), e1, float64(r.R95Km))
	if e1 > 2 || e1 > e0 {
		t.Errorf("timing did not help: %.2f -> %.2f km", e0, e1)
	}
	var tm *L2Term
	for i := range r.Terms {
		if r.Terms[i].Name == "timing" {
			tm = &r.Terms[i]
		}
	}
	if tm == nil || !tm.Used || tm.N != 6 {
		t.Errorf("timing term: %+v", tm)
	}
	// only jittery tracks: no timing term
	r2, _ := Locate2(context.Background(), ss, Locate2Options{Timing: l2Timing(w, ss, 0, 10, 6)})
	for _, tt := range r2.Terms {
		if tt.Name == "timing" && tt.Used {
			t.Errorf("jittery tracks used: %+v", tt)
		}
	}
}

// BenchmarkLocate2 is one 24 h run of a busy smooth-earth site.
func BenchmarkLocate2(b *testing.B) {
	w := newL2World(b, 1, false)
	ss := w.generate(1, 24, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r, err := Locate2(context.Background(), ss, Locate2Options{}); err != nil || !r.OK {
			b.Fatal(err)
		}
	}
}

// BenchmarkLocate2TerrainDEM is the same with a terrain model.
func BenchmarkLocate2TerrainDEM(b *testing.B) {
	w := newL2World(b, 101, true)
	ss := w.generate(101, 24, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r, err := Locate2(context.Background(), ss, Locate2Options{Terrain: w.Terrain}); err != nil || !r.OK {
			b.Fatal(err)
		}
	}
}
