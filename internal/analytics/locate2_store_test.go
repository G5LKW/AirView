package analytics

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/latency"
	"airview/internal/model"
	"airview/internal/store"
	"airview/internal/tracker"
)

// l2Shift moves the samples so that the last one is 30 min before testNow.
func l2Shift(ss []L2Sample) {
	var last int64
	for _, s := range ss {
		last = max(last, s.T)
	}
	d := testNow.Add(-30*time.Minute).UnixMilli() - last
	for i := range ss {
		ss[i].T += d
	}
}

func l2Inputs(ss []L2Sample, stn model.StationIdx) []GuessInput {
	in := make([]GuessInput, len(ss))
	for i, s := range ss {
		in[i] = GuessInput{T: s.T, Station: stn, ICAO: s.ICAO, Lat: s.Lat, Lon: s.Lon, AltFt: s.AltFt,
			RSSI: s.RSSI, Channel: s.Ch, OnGround: s.Ground}
	}
	return in
}

func TestLocateMeFromGuessSamples(t *testing.T) {
	w := newL2World(t, 31, false)
	ss := w.generate(31, 8, 100)
	l2Shift(ss)
	cfg := locatedConfig(w.Lat+0.1, w.Lon) // configured 11 km off the truth
	a, st := newTest(t, cfg)

	rec := NewGuessRecorder(st, GuessRecorderOptions{})
	defer rec.Close()
	in := l2Inputs(ss, 0)
	for i := 0; i < len(in); i += 5000 {
		rec.Record(in[i:min(i+5000, len(in))])
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	if wr, _ := rec.Stats(); wr != int64(len(ss)) {
		t.Fatalf("written %d of %d", wr, len(ss))
	}
	n, err := st.GuessSampleCount(context.Background(), 0, 0)
	if err != nil || n < int64(len(ss))*9/10 { // (stn, t, icao) duplicates collapse
		t.Fatalf("stored %d of %d: %v", n, len(ss), err)
	}

	r, err := a.LocateMe(context.Background(), LocateRequest{})
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	e := geo.HaversineKm(w.Lat, w.Lon, r.Lat, r.Lon)
	t.Logf("error %.2f km  r95 %.2f km  vs configured %.2f km  source %s", e, float64(r.R95Km), float64(r.ErrorKm), r.Source)
	if e > 3 || r.Source != store.GuessSamplesTable || r.RefSource != "configured" {
		t.Errorf("error %.2f km source %q ref %q", e, r.Source, r.RefSource)
	}
	if want := geo.HaversineKm(w.Lat+0.1, w.Lon, r.Lat, r.Lon); math.Abs(float64(r.ErrorKm)-want) > 0.01 {
		t.Errorf("ErrorKm %v, want %v", r.ErrorKm, want)
	}

	// The receiver's GNSS position takes precedence as the reference.
	SetLocateSources(LocateSources{GNSS: func() (float64, float64, bool) { return w.Lat, w.Lon, true }})
	t.Cleanup(func() { SetLocateSources(LocateSources{}) })
	a.Invalidate()
	r2, err := a.LocateMe(context.Background(), LocateRequest{})
	if err != nil || r2.RefSource != "gnss" || math.Abs(float64(r2.ErrorKm)-e) > 0.01 {
		t.Fatalf("gnss reference: %+v %v", r2, err)
	}

	// The guess game: Locate Me v2 is the fused estimate with its circle,
	// the classic methods stay for comparison.
	g, err := a.GuessLocation(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	// (r101: the classic methods weigh in by 1/sigma^2, a few per cent at most,
	// so the fused point stays within a fraction of the v2 circle.)
	if !g.OK || g.Fused == nil || g.Locate2 == nil || geo.HaversineKm(g.Fused.Lat, g.Fused.Lon, r2.Lat, r2.Lon) > 0.25*float64(r2.R95Km) || !(float64(g.Fused.R95Km) >= float64(r2.R95Km)) {
		t.Fatalf("guess: %+v", g)
	}
	var l2 *GuessEstimate
	for i := range g.Estimates {
		if g.Estimates[i].Method == "locate2" {
			l2 = &g.Estimates[i]
		} else if g.Estimates[i].Weight > 0.2 {
			t.Errorf("classic %s has weight %v next to v2", g.Estimates[i].Method, g.Estimates[i].Weight)
		}
	}
	if l2 == nil || !l2.OK || !(l2.Weight > 0.8) || !strings.Contains(l2.Detail, "signal") {
		t.Fatalf("locate2 estimate: %+v", l2)
	}
	if g.Surface == nil || g.Surface.Method != "locate2" || len(g.Surface.Cost) != g.Surface.Rows*g.Surface.Cols {
		t.Errorf("surface: %+v", g.Surface)
	}
	if math.Abs(float64(g.Fused.ErrorKm)-float64(r.ErrorKm)) > 0.25*float64(r2.R95Km) { // vs configured
		t.Errorf("fused error %v vs %v", g.Fused.ErrorKm, r.ErrorKm)
	}
	mustJSON(t, "guess", g)

	// The cached classic result is not modified by the merge.
	c, _ := a.guessClassic(context.Background(), "")
	for _, e := range c.Estimates {
		if e.Method == "locate2" {
			t.Fatal("classic cache entry modified")
		}
	}
}

func TestLocateMeStationAndFallback(t *testing.T) {
	w := newL2World(t, 32, false)
	ss := w.generate(32, 6, 100)
	l2Shift(ss)

	// An aggregator station analyzer reads its own station's samples only.
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec := NewGuessRecorder(st, GuessRecorderOptions{Want: func(s model.StationIdx) bool { return s != 9 }})
	rec.Record(l2Inputs(ss, 7))
	rec.Record(l2Inputs(ss[:1000], 9)) // not wanted
	rec.Close()
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	counts, _ := st.GuessSampleCounts(context.Background())
	if counts[9] != 0 || counts[7] == 0 {
		t.Fatalf("counts %v", counts)
	}
	a := New(st, model.DefaultConfig, WithAntPrefix(store.StationAntPrefix(7)))
	a.SetClock(func() time.Time { return testNow })
	r, err := a.LocateMe(context.Background(), LocateRequest{Ref: &LatLon{Lat: w.Lat, Lon: w.Lon}, RefSource: "survey"})
	if err != nil || !r.OK || r.RefSource != "survey" || float64(r.ErrorKm) > 3 {
		t.Fatalf("station 7: %+v %v", r, err)
	}
	b := New(st, model.DefaultConfig, WithAntPrefix(store.StationAntPrefix(8)))
	b.SetClock(func() time.Time { return testNow })
	r, err = b.LocateMe(context.Background(), LocateRequest{})
	if err != nil || r.OK || r.Samples != 0 || r.Source != store.GuessSamplesTable {
		t.Fatalf("station 8: %+v %v", r, err)
	}
	bad := New(st, model.DefaultConfig, WithAntPrefix("x"))
	if _, err := bad.LocateMe(context.Background(), LocateRequest{}); err == nil {
		t.Fatal("bad prefix accepted")
	}

	// A standalone station without guess samples uses its stored trails.
	a2, st2 := newTest(t, model.DefaultConfig())
	flights := map[uint32]int64{}
	var fl []store.Flight
	var pos []store.Position
	for _, s := range ss {
		if s.Ground {
			continue
		}
		id, ok := flights[s.ICAO]
		if !ok {
			id = st2.NextFlightID()
			flights[s.ICAO] = id
			f := store.NewFlight(id, s.ICAO, s.T/1000)
			f.End = testNow.Unix()
			fl = append(fl, f)
		}
		pos = append(pos, store.Position{FlightID: id, T: s.T / 1000, Lat: s.Lat, Lon: s.Lon, Alt: s.AltFt,
			GS: math.NaN(), Track: math.NaN(), RSSI: s.RSSI, Src: store.SrcADSB})
	}
	st2.UpsertFlights(fl)
	st2.AddPositions(pos)
	if err := st2.Flush(); err != nil {
		t.Fatal(err)
	}
	r, err = a2.LocateMe(context.Background(), LocateRequest{})
	if err != nil || !r.OK || r.Source != "positions" {
		t.Fatalf("fallback: %+v %v", r, err)
	}
	if e := geo.HaversineKm(w.Lat, w.Lon, r.Lat, r.Lon); e > 5 {
		t.Errorf("fallback error %.2f km", e)
	}
	if isNum(float64(r.ErrorKm)) {
		t.Errorf("error without a reference: %v", r.ErrorKm)
	}
}

func TestGuessRecorderPrune(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := testNow
	rec := NewGuessRecorder(st, GuessRecorderOptions{Retention: time.Hour, PerStation: 100, PruneEvery: time.Minute})
	rec.now = func() time.Time { return now }
	var in []GuessInput
	for i := 0; i < 300; i++ {
		in = append(in, GuessInput{T: now.Add(-2 * time.Hour).Add(time.Duration(i) * 30 * time.Second).UnixMilli(),
			Station: 1, ICAO: uint32(i), Lat: 50, Lon: 8, AltFt: math.NaN(), RSSI: math.NaN(), Channel: -1})
	}
	rec.Record(in) // the first call prunes at once (nothing stored yet)
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	rec.wg.Wait()
	now = now.Add(2 * time.Minute)
	rec.Record(nil) // triggers the prune
	rec.Close()
	n, _ := st.GuessSampleCount(context.Background(), 1, 0)
	if n > 100 {
		t.Errorf("%d rows after the prune (cap 100, retention 1 h)", n)
	}
	if _, pruned := rec.Stats(); pruned < 200 {
		t.Errorf("pruned %d", pruned)
	}
	rec.Record(in) // closed: ignored
	if wr, _ := rec.Stats(); wr != 300 {
		t.Errorf("written %d after close", wr)
	}
}

func TestGuessRecorderTrackerHook(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec := NewGuessRecorder(st, GuessRecorderOptions{})
	hook := rec.TrackerHook()
	hook([]tracker.GuessSample{{T: 1_000, Station: 3, ICAO: 0xabc, Lat: 50.5, Lon: 8.25, AltFt: 12000, RSSI: -22.5, Channel: 2, OnGround: false},
		{T: 2_000, Station: 3, ICAO: 0xabc, Lat: 50.5, Lon: 8.25, AltFt: math.NaN(), RSSI: math.NaN(), Channel: -1, OnGround: true}})
	rec.Close()
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	var got []store.GuessSample
	if err := st.ScanGuessSamples(context.Background(), 3, 0, 0, func(g store.GuessSample) error {
		got = append(got, g)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RSSI != -22.5 || got[0].Ch != 2 || got[0].AltFt != 12000 || !got[1].Ground || isNum(got[1].AltFt) {
		t.Fatalf("%+v", got)
	}
}

func TestL2TimingCollector(t *testing.T) {
	var c L2TimingCollector
	c.MaxAircraft, c.MaxPoints = 4, 30
	o := latency.NewObs()
	o.IsPosition, o.TFlag, o.Lat, o.Lon, o.AltFt = true, true, 50, 8, 35000
	// arrival 420 µs after an even epoch (F = 0), one every 2.4 s
	for i := 0; i < 40; i++ {
		o.ICAO = 1
		o.CPROdd = false
		o.GPSNs = int64(i)*12*l2EpochNs + 420_000 // epochs 0, 4, 8: even
		c.Observe(&o)
		o.CPROdd = true // odd: arrives 0.05 s before the odd epoch
		o.GPSNs = int64(i)*12*l2EpochNs + 3*l2EpochNs - 50_000_000
		o.ICAO = 2
		c.Observe(&o)
	}
	tr := c.Tracks()
	if len(tr) != 2 || len(tr[0].Points) != 30 {
		t.Fatalf("tracks %+v", tr)
	}
	for _, p := range tr[0].Points {
		if p.MNs != 420_000 {
			t.Fatalf("even: M %v", p.MNs)
		}
	}
	for _, p := range tr[1].Points {
		if p.MNs != -50_000_000 {
			t.Fatalf("odd: M %v", p.MNs)
		}
	}
	// rejected observations
	for _, f := range []func(*latency.Obs){
		func(o *latency.Obs) { o.TFlag = false },
		func(o *latency.Obs) { o.MLAT = true },
		func(o *latency.Obs) { o.GPSNs = -1 },
		func(o *latency.Obs) { o.AltFt = math.NaN() },
		func(o *latency.Obs) { o.IsPosition = false },
	} {
		x := o
		x.ICAO = 99
		x.GPSNs = 1e12
		f(&x)
		c.Observe(&x)
	}
	c.mu.Lock()
	_, bad := c.acs[99]
	c.mu.Unlock()
	if bad {
		t.Error("invalid observation recorded")
	}
	// the aircraft cap evicts
	for k := uint32(10); k < 20; k++ {
		x := o
		x.ICAO, x.GPSNs = k, 1e12+int64(k)*1e9
		c.Observe(&x)
	}
	c.mu.Lock()
	n := len(c.acs)
	c.mu.Unlock()
	if n > 4 {
		t.Errorf("%d aircraft kept (cap 4)", n)
	}
}

func TestL2Keep(t *testing.T) {
	all := l2Keep(100)
	if !all(1, 2) {
		t.Fatal("small sets are not thinned")
	}
	k := l2Keep(4 * l2LoadMax)
	n := 0
	for i := 0; i < 100_000; i++ {
		if k(uint32(i%500), int64(i)*1000) {
			n++
		}
	}
	if n < 20_000 || n > 30_000 {
		t.Errorf("kept %d of 100000, want ~25000", n)
	}
	// whole 10 s buckets go together
	if k(5, 10_000) != k(5, 19_999) {
		t.Error("bucket split")
	}
}
