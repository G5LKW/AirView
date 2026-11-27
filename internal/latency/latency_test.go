package latency

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/modes"
	"airview/internal/store"
)

type fixedClock struct {
	off time.Duration
	ok  bool
}

func (c fixedClock) Offset() time.Duration { return c.off }
func (c fixedClock) Synced() bool          { return c.ok }

const (
	rxLat, rxLon, rxH = 48.0, 11.0, 500.0
	icaoA             = 0x3C6DD2
)

// day0 is 2026-11-12 00:00:00 UTC in Unix ns.
var day0 = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC).UnixNano()

func ns(d time.Duration) int64 { return int64(d) }

// msg describes one synthetic transmission with known truth.
type msg struct {
	icao     uint32
	feed     string
	tx       int64 // true UTC transmit time (start of preamble leaves the antenna)
	tflag    bool
	odd      bool
	lat, lon float64
	altFt    float64
	gs, trk  float64
	recvDly  int64         // antenna TOA -> host socket read (true)
	proc     int64         // socket read -> handled
	offset   time.Duration // true UTC - host clock
	df       uint8
	mlat     bool
}

// build returns the Obs for m and the expected propagation (ns, unrounded).
func build(t testing.TB, rx *receiverGeo, m msg) (Obs, float64) {
	t.Helper()
	dist, _ := rx.slant(m.lat, m.lon, m.altFt*geo.MetresPerFoot)
	prop := propagationNs(dist, rx.hM, m.altFt*geo.MetresPerFoot)
	toa := m.tx + int64(math.Round(prop)) // start of frame at the antenna
	df := m.df
	if df == 0 {
		df = 17
	}
	end := toa + frameDurationNs(df) // Radarcape stamps the end of the frame
	gps := ((end-day0)%nsPerDay + nsPerDay) % nsPerDay
	hostUTC := toa + m.recvDly
	recvHost := hostUTC - int64(m.offset)
	o := NewObs()
	o.RecvHost = time.Unix(0, recvHost)
	o.Handled = time.Unix(0, recvHost+m.proc)
	o.GPSNs = gps
	o.FeedID = m.feed
	if o.FeedID == "" {
		o.FeedID = "radarcape"
	}
	o.ICAO = m.icao
	o.DF, o.TC = df, 11
	o.TFlag, o.CPROdd = m.tflag, m.odd
	if m.tflag {
		o.TC = 9
	}
	o.IsPosition = true
	o.Lat, o.Lon, o.AltFt, o.GS, o.Track = m.lat, m.lon, m.altFt, m.gs, m.trk
	o.MLAT = m.mlat
	return o, prop
}

func newTestAnalyzer(off time.Duration) *Analyzer {
	a := New(Options{})
	a.SetReceiver(rxLat, rxLon, rxH)
	a.SetClock(fixedClock{off: off, ok: true}, nil)
	return a
}

func TestNearestEpochParity(t *testing.T) {
	sec := func(s float64) int64 { return day0 + int64(math.Round(s*1e9)) }
	cases := []struct {
		recv float64
		odd  bool
		want float64
	}{
		{12.05, false, 12.0}, {12.05, true, 12.2}, {12.11, true, 12.2}, {12.29, false, 12.4},
		{13.01, true, 13.0}, // 13.0 s is an ODD epoch (5 intervals after 12.0)
		{13.01, false, 13.2}, {12.79, false, 12.8}, {12.79, true, 12.6}, {0.05, true, 0.2}, {0.95, true, 1.0},
		{13.6, false, 13.6}, {13.4, true, 13.4}, {12.0, false, 12.0},
	}
	for _, c := range cases {
		got := NearestEpoch(sec(c.recv), c.odd)
		if got != sec(c.want) {
			t.Errorf("NearestEpoch(%.2f s, odd=%v) = %.3f s, want %.1f", c.recv, c.odd, float64(got-day0)/1e9, c.want)
		}
	}
}

func TestGPSToUnixMidnight(t *testing.T) {
	// TOA 23:59:59.950 on day -1, host already 00:00:00.010.
	toa, legacy, ok := GPSToUnixNs(nsPerDay-ns(50*time.Millisecond), day0+ns(10*time.Millisecond))
	if !ok || legacy || toa != day0-ns(50*time.Millisecond) {
		t.Fatalf("wrap back: %v %v %d", ok, legacy, toa-day0)
	}
	// TOA 00:00:00.020, host still 23:59:59.970.
	toa, _, _ = GPSToUnixNs(ns(20*time.Millisecond), day0-ns(30*time.Millisecond))
	if toa != day0+ns(20*time.Millisecond) {
		t.Fatalf("wrap forward: %d", toa-day0)
	}
	// Legacy 1..86400 range: 86400.5 s is the last second of the day.
	toa, legacy, ok = GPSToUnixNs(nsPerDay+ns(500*time.Millisecond), day0+ns(time.Hour))
	if !ok || !legacy || toa != day0-ns(500*time.Millisecond) {
		t.Fatalf("legacy: %v %v %d", ok, legacy, toa-day0)
	}
	if _, _, ok := GPSToUnixNs(-1, day0); ok {
		t.Fatal("negative accepted")
	}
	if _, _, ok := GPSToUnixNs(nsPerDay+nsPerSec, day0); ok {
		t.Fatal("out of range accepted")
	}
}

func TestSlantGeometry(t *testing.T) {
	r := newReceiverGeo(rxLat, rxLon, rxH)
	d, el := r.slant(rxLat, rxLon, 10000)
	if math.Abs(d-9500) > 1e-6 || math.Abs(el-90) > 1e-6 {
		t.Fatalf("overhead: %v m, %v deg", d, el)
	}
	lat, lon, _ := geo.VincentyDirect(rxLat, rxLon, 75, 200000)
	d, el = r.slant(lat, lon, 10000)
	ref := geo.SlantRangeKm(geo.DistanceKm(rxLat, rxLon, lat, lon), rxH, 10000) * 1000
	if math.Abs(d-ref)/ref > 0.002 || el < -1 || el > 3 {
		t.Fatalf("200 km: ecef %.1f m vs geo %.1f m, elev %.3f", d, ref, el)
	}
	// Refractivity adds roughly 0.02..0.03 % of the path.
	p := propagationNs(d, rxH, 10000)
	vac := d / SpeedOfLight * 1e9
	if ex := p - vac; ex < 0.1*vac*1e-4 || ex > 3.2*vac*1e-4 {
		t.Fatalf("refractive excess %.3f ns of %.1f", ex, vac)
	}
}

// TestExactStagesT1 checks every stage of a UTC-coupled message against the
// constructed truth to better than 1 us (in fact to 1 ns).
func TestExactStagesT1(t *testing.T) {
	lat, lon, _ := geo.VincentyDirect(rxLat, rxLon, 75, 200000)
	cases := []struct {
		name string
		sec  float64 // epoch, seconds of day
		odd  bool
		l    time.Duration
		off  time.Duration
	}{
		{"even +37ms", 45296.4, false, 37123456 * time.Nanosecond, 3500 * time.Microsecond},
		{"odd +80ms", 45296.6, true, 80 * time.Millisecond, -12 * time.Millisecond},
		{"odd 13.0 s style", 45297.0, true, 5 * time.Millisecond, 0},
		{"even -120ms (sent before epoch)", 45298.8, false, -120 * time.Millisecond, 250 * time.Millisecond},
		{"even +190ms", 45300.0, false, 190 * time.Millisecond, 1 * time.Millisecond},
	}
	for i, c := range cases {
		a := newTestAnalyzer(c.off)
		e := day0 + int64(math.Round(c.sec*1e9))
		m := msg{icao: icaoA + uint32(i), tx: e + int64(c.l), tflag: true, odd: c.odd, lat: lat, lon: lon,
			altFt: 35000, gs: 450, trk: 75, recvDly: 4321 * 1000, proc: 250 * 1000, offset: c.off}
		o, prop := build(t, &a.rx, m)
		a.Observe(&o)
		_, w, ok := a.AircraftDetail(m.icao)
		if !ok || !w.Exact {
			t.Fatalf("%s: no exact waterfall: %+v", c.name, w)
		}
		want := map[string]int64{
			"positionAge": int64(c.l),
			"propagation": int64(math.Round(prop)),
			"receiver":    4321 * 1000,
			"processing":  250 * 1000,
		}
		for _, s := range w.Stages {
			if wv, ok := want[s.Key]; ok {
				if d := s.Ns - wv; d > 1000 || d < -1000 {
					t.Errorf("%s: stage %s = %d ns, want %d", c.name, s.Key, s.Ns, wv)
				} else if d != 0 {
					t.Logf("%s: stage %s off by %d ns", c.name, s.Key, d)
				}
				delete(want, s.Key)
			}
		}
		if len(want) > 0 {
			t.Errorf("%s: missing stages %v", c.name, want)
		}
		total := int64(c.l) + int64(math.Round(prop)) + 4321*1000 + 250*1000
		if got := int64(math.Round(float64(w.TotalMs) * 1e6)); math.Abs(float64(got-total)) > 1000 {
			t.Errorf("%s: total %d want %d", c.name, got, total)
		}
		if w.Events[0].Key != "applicability" || w.Events[0].UTC != fmtUTC(e) {
			t.Errorf("%s: applicability event %+v, want %s", c.name, w.Events[0], fmtUTC(e))
		}
		sum := a.Summary()
		if st := sum.Stages[stXpdr]; st.Count != 1 || math.Abs(float64(st.MinMs)-float64(c.l)/1e6) > 1e-3 {
			t.Errorf("%s: transponder stage %+v", c.name, st)
		}
		if st := sum.Stages[stRecv]; math.Abs(float64(st.MinMs)-4.321) > 1e-3 {
			t.Errorf("%s: receiver stage %+v", c.name, st)
		}
	}
}

func TestEpochAmbiguity(t *testing.T) {
	// +210 ms after an even epoch is nearer to the NEXT even epoch: the
	// recovered latency wraps to -190 ms (the ±200 ms ambiguity window).
	a := newTestAnalyzer(0)
	e := day0 + 1000*nsPerSec
	m := msg{icao: icaoA, tx: e + ns(210*time.Millisecond), tflag: true, lat: rxLat + 0.5, lon: rxLon, altFt: 30000, recvDly: ns(3 * time.Millisecond)}
	o, _ := build(t, &a.rx, m)
	a.Observe(&o)
	_, w, _ := a.AircraftDetail(icaoA)
	if w.Stages[0].Ns != -ns(190*time.Millisecond) {
		t.Fatalf("got %d", w.Stages[0].Ns)
	}
}

func TestMidnightPipeline(t *testing.T) {
	// Transmission 30 ms before midnight, host reads it 45 ms later with a
	// host clock 20 ms slow (offset +20 ms): host clock still shows 23:59:59.995.
	a := newTestAnalyzer(20 * time.Millisecond)
	m := msg{icao: icaoA, tx: day0 - ns(30*time.Millisecond), lat: rxLat, lon: rxLon + 1, altFt: 20000,
		recvDly: ns(45 * time.Millisecond), proc: ns(time.Millisecond), offset: 20 * time.Millisecond}
	o, _ := build(t, &a.rx, m)
	if o.GPSNs < nsPerDay-ns(time.Second) {
		t.Fatalf("test setup: GPSNs %d should be just before midnight", o.GPSNs)
	}
	a.Observe(&o)
	_, w, _ := a.AircraftDetail(icaoA)
	for _, s := range w.Stages {
		if s.Key == "receiver" && s.Ns != ns(45*time.Millisecond) {
			t.Fatalf("receiver stage %d", s.Ns)
		}
	}
	// And the other way: TOA just after midnight, host clock fast.
	a = newTestAnalyzer(-200 * time.Millisecond)
	m = msg{icao: icaoA, tx: day0 + ns(10*time.Millisecond), lat: rxLat, lon: rxLon + 1, altFt: 20000,
		recvDly: ns(2 * time.Millisecond), offset: -200 * time.Millisecond}
	o, _ = build(t, &a.rx, m)
	a.Observe(&o)
	if st := a.Summary().Stages[stRecv]; st.Count != 1 || math.Abs(float64(st.MinMs)-2) > 1e-3 {
		t.Fatalf("after midnight: %+v", st)
	}
}

func TestGating(t *testing.T) {
	a := newTestAnalyzer(0)
	base := msg{icao: icaoA, tx: day0 + 3600*nsPerSec, lat: rxLat + 1, lon: rxLon, altFt: 30000, recvDly: ns(5 * time.Millisecond), proc: 1000}
	obs := func(mut func(*msg), omut func(*Obs)) {
		m := base
		base.tx += ns(500 * time.Millisecond)
		if mut != nil {
			mut(&m)
		}
		o, _ := build(t, &a.rx, m)
		if omut != nil {
			omut(&o)
		}
		a.Observe(&o)
	}
	obs(func(m *msg) { m.recvDly = -ns(5 * time.Millisecond) }, nil)          // negative: counted, kept
	obs(func(m *msg) { m.recvDly = -ns(time.Hour) }, nil)                     // absurd (host 1 h behind; a host time jumping ahead would move the window)
	obs(func(m *msg) { m.recvDly = -nsPerSec + ns(4*time.Millisecond) }, nil) // legacy 1 s ahead
	obs(func(m *msg) { m.recvDly = -18*nsPerSec + ns(4*time.Millisecond) }, nil)
	obs(nil, func(o *Obs) { o.GPSNs = -1 })
	obs(nil, func(o *Obs) { o.MLAT = true })
	obs(nil, func(o *Obs) { o.Handled = o.RecvHost.Add(-time.Millisecond) })
	obs(nil, func(o *Obs) { o.Lat = math.NaN() })
	obs(nil, func(o *Obs) { o.AltFt = math.NaN() })
	obs(nil, func(o *Obs) { o.Lat, o.Lon = 10, 100 }) // > 1000 km
	a.Observe(nil)
	var zero Obs
	a.Observe(&zero)

	c := a.Summary().Counts
	if c.NegativeDelay != 1 || c.AbsurdDelay != 1 || c.OneSecondAhead != 1 || c.GPSTimeNotUTC != 1 ||
		c.NoGPS != 1 || c.MLATExcluded != 1 || c.ProcessingGated != 1 || c.RangeGated != 1 {
		t.Fatalf("counts %+v", c)
	}
	st := a.Summary().Stages
	if st[stRecv].Count != 5 { // all with valid GPS and a plausible delay
		t.Fatalf("receiver count %d", st[stRecv].Count)
	}
	if st[stProp].Count != 6 {
		t.Fatalf("prop count %d", st[stProp].Count)
	}
	chk := a.NTPCheck()
	if chk.Quality != "bad" {
		t.Fatalf("ntpcheck should be bad with 1 s / 18 s offsets: %+v", chk)
	}
}

// straightTrack generates n positions of a T=0 aircraft flying straight at
// gs kt on track trk with position ages uniform in [0, ageMax] and Gaussian
// position noise sigma (m).
func straightTrack(a *Analyzer, rng *rand.Rand, icao uint32, n int, ageMax, noiseM float64, t1 bool) {
	const gs, trk = 450.0, 60.0
	v := gs * ktToMS
	lat0, lon0 := rxLat+0.8, rxLon-0.5
	t0 := day0 + 7200*nsPerSec
	tx := t0
	sn, cs := math.Sincos(trk * math.Pi / 180)
	for i := 0; i < n; i++ {
		tx += int64((0.4 + 0.2*rng.Float64()) * 1e9)
		var age float64
		var odd bool
		txi := tx
		if t1 {
			// Position valid at an epoch, transmitted 20..120 ms later.
			k := floorDiv(tx, EpochNs)
			odd = k&1 == 1
			age = 0.02 + 0.1*rng.Float64()
			txi = k*EpochNs + int64(age*1e9)
		} else {
			age = ageMax * rng.Float64()
		}
		valid := float64(txi-t0)/1e9 - age
		dist := v * valid
		nN := dist*cs + noiseM*rng.NormFloat64()
		nE := dist*sn + noiseM*rng.NormFloat64()
		r := geo.EarthRadiusKm * 1000
		lat := lat0 + nN/r*180/math.Pi
		lon := lon0 + nE/(r*math.Cos(lat0*math.Pi/180))*180/math.Pi
		m := msg{icao: icao, tx: txi, tflag: t1, odd: odd, lat: lat, lon: lon, altFt: 36000, gs: gs, trk: trk,
			recvDly: ns(3*time.Millisecond) + int64(rng.ExpFloat64()*1e6), proc: 100000}
		o, _ := build(nil2{}, &a.rx, m)
		a.Observe(&o)
	}
}

// nil2 is a minimal testing.TB stand-in for build's Helper call.
type nil2 struct{ testing.TB }

func (nil2) Helper() {}

func TestPositionAgeEstimate(t *testing.T) {
	a := newTestAnalyzer(0)
	rng := rand.New(rand.NewPCG(1, 2))
	straightTrack(a, rng, icaoA, 1500, 0.2, 5, false)
	row, w, ok := a.AircraftDetail(icaoA)
	if !ok {
		t.Fatal("aircraft missing")
	}
	wantSigma := 200 / math.Sqrt(12) // 57.7 ms
	if math.Abs(float64(row.AgeJitterMs)-wantSigma) > 0.12*wantSigma {
		t.Errorf("age jitter %.1f ms, want %.1f ±12%%", float64(row.AgeJitterMs), wantSigma)
	}
	if math.Abs(float64(row.AgeModelMs)-100) > 15 {
		t.Errorf("model mean age %.1f ms, want 100 ±15", float64(row.AgeModelMs))
	}
	if row.AgeExact || w.Exact {
		t.Error("T=0 must not be exact")
	}
	if w.Stages[0].Method != "estimated" {
		t.Errorf("waterfall method %q", w.Stages[0].Method)
	}
	t.Logf("pairs %d jitter %.1f ms model %.1f ms single-msg age %.1f ±%.1f ms", row.AgePairs,
		float64(row.AgeJitterMs), float64(row.AgeModelMs), float64(w.Stages[0].Ms), float64(w.Stages[0].UncMs))
	// Single-message estimates: correlation with truth is what matters; the
	// aggregate is checked above. Here only sanity: within [-100, 300] ms.
	if ms := float64(w.Stages[0].Ms); ms < -100 || ms > 300 {
		t.Errorf("single-message estimate %.1f ms", ms)
	}
}

func TestPairMethodAgainstT1(t *testing.T) {
	a := newTestAnalyzer(0)
	rng := rand.New(rand.NewPCG(3, 4))
	straightTrack(a, rng, icaoA, 600, 0, 0.3, true)
	row, _, _ := a.AircraftDetail(icaoA)
	if row.XpdrN < 500 || !row.AgeExact {
		t.Fatalf("T=1 not analysed: %+v", row)
	}
	if math.Abs(float64(row.XpdrMeanMs)-70) > 5 {
		t.Errorf("mean transponder latency %.2f ms, want 70 ±5", float64(row.XpdrMeanMs))
	}
	if !(float64(row.AgeCheckMs) < 5) {
		t.Errorf("pair method vs exact RMS %.2f ms", float64(row.AgeCheckMs))
	}
	for _, f := range row.Flags {
		if strings.HasPrefix(f, "t1-") {
			t.Errorf("unexpected flag %s", f)
		}
	}
}

func TestT1NotSynchronisedFlag(t *testing.T) {
	a := newTestAnalyzer(0)
	rng := rand.New(rand.NewPCG(5, 6))
	tx := day0 + 100*nsPerSec
	for i := 0; i < 50; i++ {
		tx += int64((0.4 + 0.2*rng.Float64()) * 1e9)
		m := msg{icao: icaoA, tx: tx, tflag: true, odd: rng.IntN(2) == 1, lat: rxLat + 1, lon: rxLon, altFt: 35000, recvDly: 3e6}
		o, _ := build(t, &a.rx, m)
		a.Observe(&o)
	}
	row, _, _ := a.AircraftDetail(icaoA)
	found := false
	for _, f := range row.Flags {
		found = found || f == "t1-not-synchronised"
	}
	if !found {
		t.Fatalf("flags %v (std %.1f)", row.Flags, float64(row.XpdrStdMs))
	}
}

func TestClockFilter(t *testing.T) {
	a := newTestAnalyzer(0)
	rng := rand.New(rand.NewPCG(7, 8))
	start := day0 + 10000*nsPerSec
	for i := 0; i < 6000; i++ { // 10 minutes at 10 msg/s
		tx := start + int64(i)*100*1e6
		minutes := float64(tx-start) / 60e9
		d := 3e6 + 100e3*minutes + rng.ExpFloat64()*0.3e6
		m := msg{icao: icaoA + uint32(i%20), tx: tx, lat: rxLat + 0.5, lon: rxLon, altFt: 20000, recvDly: int64(d), proc: 50000}
		o, _ := build(t, &a.rx, m)
		a.Observe(&o)
	}
	chk := a.NTPCheck()
	if len(chk.Feeds) != 1 {
		t.Fatalf("feeds %+v", chk.Feeds)
	}
	f := chk.Feeds[0]
	wantFloor := 3 + 0.1*(10-80.0/60) // floor over the last 80 s
	if math.Abs(float64(f.FloorMs)-wantFloor) > 0.1 {
		t.Errorf("floor %.3f ms, want ~%.3f", float64(f.FloorMs), wantFloor)
	}
	if math.Abs(float64(f.DriftUsPerMin)-100) > 15 {
		t.Errorf("drift %.1f us/min, want 100", float64(f.DriftUsPerMin))
	}
	if f.Status != "warn" {
		t.Errorf("status %s, issues %v", f.Status, f.Issues)
	}
	series, ok := a.FeedSeries("radarcape", 0)
	if !ok || len(series) < 9 {
		t.Fatalf("series %d", len(series))
	}
	rows := a.DrainMinutes()
	var all, feed int
	for _, r := range rows {
		if r.Feed == "" {
			all++
		} else {
			feed++
		}
	}
	if all < 9 || feed < 9 {
		t.Fatalf("minute rows all=%d feed=%d", all, feed)
	}
	if a.DrainMinutes() != nil {
		t.Fatal("drain not emptied")
	}
}

func TestNTPQuality(t *testing.T) {
	a := newTestAnalyzer(0)
	info := NTPInfo{Enabled: true, Synced: true, Server: "gps.lan", Stratum: 1, RefID: "PPS",
		OffsetMs: 0.1, DelayMs: 0.4, JitterMs: 0.02, RootDelayMs: 0, RootDispMs: 0.01}
	a.SetClock(fixedClock{ok: true}, func() NTPInfo { return info })
	if q := a.NTPCheck(); q.Quality != "good" || math.Abs(float64(q.OffsetUncertaintyMs)-0.23) > 1e-9 {
		t.Fatalf("good server: %+v", q)
	}
	info.Stratum, info.RefID = 2, "192.168.1.1"
	if q := a.NTPCheck(); q.Quality != "degraded" {
		t.Fatalf("stratum 2: %+v", q)
	}
	info.Stratum, info.RefID = 1, "LOCL"
	if q := a.NTPCheck(); q.Quality != "degraded" {
		t.Fatalf("refid LOCL: %+v", q)
	}
	info.Synced = false
	if q := a.NTPCheck(); q.Quality != "bad" {
		t.Fatalf("unsynced: %+v", q)
	}
	info.Enabled, info.Server = false, ""
	if q := a.NTPCheck(); q.Quality != "unknown" {
		t.Fatalf("disabled: %+v", q)
	}
}

func TestJSONNeverNaN(t *testing.T) {
	check := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(b), "NaN") || strings.Contains(string(b), "Inf") {
			t.Fatalf("%s: %s", name, b)
		}
	}
	a := New(Options{})
	check("empty summary", a.Summary())
	check("empty hist", a.Histograms())
	check("empty aircraft", a.Aircraft())
	check("empty ntp", a.NTPCheck())
	a = newTestAnalyzer(time.Millisecond)
	rng := rand.New(rand.NewPCG(9, 9))
	straightTrack(a, rng, icaoA, 200, 0.2, 5, false)
	straightTrack(a, rng, icaoA+1, 200, 0, 1, true)
	check("summary", a.Summary())
	check("hist", a.Histograms())
	check("aircraft", a.Aircraft())
	check("ntp", a.NTPCheck())
	r, w, _ := a.AircraftDetail(icaoA)
	check("row", r)
	check("waterfall", w)
	s, _ := a.FeedSeries("radarcape", 0)
	check("series", s)
	var n Num
	if err := json.Unmarshal([]byte("null"), &n); err != nil || n.Valid() {
		t.Fatal("Num null")
	}
}

func TestBounded(t *testing.T) {
	a := New(Options{MaxAircraft: 50, MaxFeeds: 2})
	a.SetReceiver(rxLat, rxLon, rxH)
	for i := 0; i < 1000; i++ {
		m := msg{icao: uint32(i + 1), feed: string(rune('a' + i%5)), tx: day0 + int64(i)*1e6, lat: rxLat + 0.2, lon: rxLon, altFt: 10000, recvDly: 1e6}
		o, _ := build(t, &a.rx, m)
		a.Observe(&o)
	}
	if len(a.ac) > 50 || len(a.acIdx) > 50 {
		t.Fatalf("aircraft table %d / %d", len(a.ac), len(a.acIdx))
	}
	if ids := a.FeedIDs(); len(ids) != 3 {
		t.Fatalf("feeds %v", ids)
	}
	// Index consistency after evictions.
	for icao, i := range a.acIdx {
		if a.ac[i].icao != icao {
			t.Fatalf("index broken for %06X", icao)
		}
	}
}

func TestObserveNoAlloc(t *testing.T) {
	a := newTestAnalyzer(time.Millisecond)
	m := msg{icao: icaoA, tx: day0 + 5000*nsPerSec, tflag: true, lat: rxLat + 1, lon: rxLon, altFt: 35000, gs: 450, trk: 0, recvDly: 3e6, proc: 1e5}
	o, _ := build(t, &a.rx, m)
	a.Observe(&o) // create feed + aircraft
	i := 0
	allocs := testing.AllocsPerRun(2000, func() {
		i++
		o.RecvHost = o.RecvHost.Add(time.Millisecond)
		o.Handled = o.RecvHost.Add(100 * time.Microsecond)
		o.GPSNs += 1e6
		o.Lat += 1e-5
		a.Observe(&o)
	})
	if allocs != 0 {
		t.Fatalf("Observe allocates %.2f per call", allocs)
	}
}

func TestMessageHelpers(t *testing.T) {
	// DF17 airborne position, TC 11, T=1, F=1, from the modes test encoder layout.
	me := uint64(11)<<51 | 1<<(56-21) | 1<<(56-22) | uint64(1234)<<17 | 5678
	if !TFlagFromME(me) {
		t.Fatal("T bit not read")
	}
	if TFlagFromME(uint64(19)<<51 | 1<<(56-21)) {
		t.Fatal("T bit read from a velocity message")
	}
	var frame [14]byte
	frame[0] = 17<<3 | 5
	frame[1], frame[2], frame[3] = 0x3C, 0x6D, 0xD2
	for i := 0; i < 7; i++ {
		frame[4+i] = byte(me >> (48 - 8*i))
	}
	modes.SetParity(frame[:], 0)
	var m modes.Message
	if err := modes.Decode(frame[:], &m); err != nil {
		t.Fatal(err)
	}
	o := NewObs()
	o.SetMessage(&m)
	if !o.IsPosition || !o.TFlag || !o.CPROdd || o.ICAO != 0x3C6DD2 || o.DF != 17 || o.TC != 11 {
		t.Fatalf("SetMessage: %+v", o)
	}
	if TFlagFromME(m.ME()) != m.CPRTime {
		t.Fatal("helper disagrees with modes.CPRTime")
	}
}

func TestPersist(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Skipf("store unavailable: %v", err)
	}
	defer st.Close()
	rows := []MinuteSummary{{T: 1794398400, Feed: "radarcape", N: 10, MinUs: 2900, P50Us: 3100}, {T: 1794398400, Feed: "", N: 10, T1N: 2, T1P50Us: 41000}}
	ctx := context.Background()
	if err := Persist(ctx, st, rows, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.Flush(); err != nil { // queued for the group commit
		t.Fatal(err)
	}
	got, err := LoadMinutes(ctx, st.DB(), 0, nil, 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("load: %v %+v", err, got)
	}
	a := New(Options{})
	a.Seed(got)
	if s, ok := a.FeedSeries("radarcape", 0); !ok || len(s) != 1 || float64(s[0].MinMs) != 2.9 {
		t.Fatalf("seed %+v", s)
	}
}

func BenchmarkObserve(b *testing.B) {
	a := newTestAnalyzer(time.Millisecond)
	m := msg{icao: icaoA, tx: day0 + 5000*nsPerSec, tflag: true, lat: rxLat + 1, lon: rxLon, altFt: 35000, gs: 450, trk: 0, recvDly: 3e6, proc: 1e5}
	o, _ := build(b, &a.rx, m)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o.RecvHost = o.RecvHost.Add(time.Millisecond)
		o.Handled = o.RecvHost.Add(100 * time.Microsecond)
		o.GPSNs = (o.GPSNs + 1e6) % nsPerDay
		o.ICAO = icaoA + uint32(i&255)
		o.Lat += 1e-6
		a.Observe(&o)
	}
}
