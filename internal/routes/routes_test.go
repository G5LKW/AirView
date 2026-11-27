package routes

import (
	"bytes"
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/navdata"
	"airview/internal/store"
)

// ---------------------------------------------------------------- fixtures

var testAirports = map[string]navdata.Airport{
	"EGLL": {Ident: "EGLL", ICAO: "EGLL", IATA: "LHR", Name: "London Heathrow", Municipality: "London", Lat: 51.4706, Lon: -0.4619, Size: navdata.SizeLarge, ElevFt: ip(83)},
	"KJFK": {Ident: "KJFK", ICAO: "KJFK", IATA: "JFK", Name: "John F Kennedy", Municipality: "New York", Lat: 40.6398, Lon: -73.7789, Size: navdata.SizeLarge, ElevFt: ip(13)},
	"EGPH": {Ident: "EGPH", ICAO: "EGPH", IATA: "EDI", Name: "Edinburgh", Municipality: "Edinburgh", Lat: 55.95, Lon: -3.3725, Size: navdata.SizeLarge, ElevFt: ip(135)},
	"LIRF": {Ident: "LIRF", ICAO: "LIRF", IATA: "FCO", Name: "Fiumicino", Municipality: "Rome", Lat: 41.8003, Lon: 12.2389, Size: navdata.SizeLarge, ElevFt: ip(13)},
	"EDDF": {Ident: "EDDF", ICAO: "EDDF", IATA: "FRA", Name: "Frankfurt", Municipality: "Frankfurt", Lat: 50.0333, Lon: 8.5706, Size: navdata.SizeLarge, ElevFt: ip(364)},
	"YSSY": {Ident: "YSSY", ICAO: "YSSY", IATA: "SYD", Name: "Sydney", Municipality: "Sydney", Lat: -33.9461, Lon: 151.177, Size: navdata.SizeLarge, ElevFt: ip(21)},
	"WSSS": {Ident: "WSSS", ICAO: "WSSS", IATA: "SIN", Name: "Changi", Municipality: "Singapore", Lat: 1.3502, Lon: 103.994, Size: navdata.SizeLarge, ElevFt: ip(22)},
	"EHAM": {Ident: "EHAM", ICAO: "EHAM", IATA: "AMS", Name: "Schiphol", Municipality: "Amsterdam", Lat: 52.3086, Lon: 4.7639, Size: navdata.SizeLarge, ElevFt: ip(-11)},
}

func ip(v int) *int { return &v }

func stop(code string) Stop {
	a := testAirports[code]
	return StopOf(a)
}

func stopsOf(codes ...string) []Stop {
	var out []Stop
	for _, c := range codes {
		out = append(out, stop(c))
	}
	return out
}

// fakeNav serves fixed standing / cached / online routes.
type fakeNav struct {
	mu       sync.Mutex
	standing map[string][]string
	cached   map[string][]string // cached online answers
	online   map[string][]string // what an online request returns
	requests []string
}

func (f *fakeNav) StoredRoute(_ context.Context, cs string) (navdata.RouteRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.standing[cs]; ok {
		return navdata.RouteRecord{Callsign: cs, Codes: c}, true, nil
	}
	return navdata.RouteRecord{}, false, nil
}

func (f *fakeNav) CachedRoute(_ context.Context, cs string) (navdata.RouteRecord, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cached[cs]; ok {
		return navdata.RouteRecord{Callsign: cs, Codes: c}, len(c) >= 2, true, nil
	}
	return navdata.RouteRecord{}, false, false, nil
}

func (f *fakeNav) OnlineRoute(_ context.Context, cs string) (navdata.RouteRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, cs)
	c, ok := f.online[cs]
	if f.cached == nil {
		f.cached = map[string][]string{}
	}
	f.cached[cs] = c
	return navdata.RouteRecord{Callsign: cs, Codes: c}, ok, nil
}

func (f *fakeNav) AirportByCode(_ context.Context, code string, _ bool) (navdata.Airport, bool, error) {
	a, ok := testAirports[code]
	if !ok {
		for _, x := range testAirports {
			if x.IATA == code {
				return x, true, nil
			}
		}
	}
	return a, ok, nil
}

func (f *fakeNav) reqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
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

func f64(v float64) *float64 { return &v }
func bp(v bool) *bool        { return &v }

func aircraft(icao, cs string, lat, lon, alt, track, gs, vr float64) *model.Aircraft {
	return &model.Aircraft{ICAO: icao, Callsign: cs, Lat: f64(lat), Lon: f64(lon), AltBaro: f64(alt), Track: f64(track), GS: f64(gs), VertRate: f64(vr), Category: "A3"}
}

// ---------------------------------------------------------------- callsign

func TestCallsignHygiene(t *testing.T) {
	for cs, want := range map[string]bool{
		"BAW123": true, "EZY12AB": true, "RYR1234": true, "DLH400": true, "QFA1": true,
		"": false, "GABCD": false, "N123AB": false, "TEST1": false, "TESTX12": false, "BAW": false, "BA123": false, "BAWX123": false,
	} {
		if got := Lookupable(cs, ""); got != want {
			t.Errorf("Lookupable(%q) = %v, want %v", cs, got, want)
		}
	}
	if Lookupable("DLH400", "D-LH400") || Lookupable("ABC1234", "ABC1234") {
		t.Error("a callsign equal to the registration must not be looked up")
	}
	if Normalize(" baw123 ") != "BAW123" || Normalize("BAW-12") != "" {
		t.Error("Normalize")
	}
	check := func(cs string, want ...string) {
		t.Helper()
		got := Variants(cs)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("Variants(%s) = %v, want %v", cs, got, want)
		}
	}
	check("BAW0123", "BAW123")
	check("BAW101P", "BAW101")
	check("EZY12AB") // alphanumeric suffix: a different route, never stripped
	check("RYR1234")
	check("EZY85TQ")
}

// ---------------------------------------------------------------- scoring

func TestDetourZ(t *testing.T) {
	// A point on the great circle is 0; the tolerance ellipse edge is 1.
	for _, d := range []float64{150, 300, 1000, 6000} {
		k := math.Max(0.10, math.Min(0.60, 0.10+60/d))
		if z := detourZ(d/2, d/2, d); z != 0 {
			t.Errorf("D=%v on track: z=%v", d, z)
		}
		edge := k*d + 20
		if z := detourZ(d/2+edge/2, d/2+edge/2, d); math.Abs(z-1) > 1e-9 {
			t.Errorf("D=%v edge: z=%v", d, z)
		}
	}
	// Short legs get a wider relative tolerance than long ones.
	if detourZ(100, 100, 150) <= detourZ(4000, 2050, 6000)*0 {
		t.Error("unexpected")
	}
}

func TestEvaluateCruiseFits(t *testing.T) {
	// Mid-Atlantic on LHR-JFK, heading west at FL370.
	o := Obs{Lat: 52.5, Lon: -30, AltFt: 37000, VR: 0, Track: 265, GS: 480}
	sc := Evaluate(stopsOf("EGLL", "KJFK"), o)
	if sc.State != StateFits || sc.AlongPct < 30 || sc.AlongPct > 60 {
		t.Fatalf("%+v", sc)
	}
	// Heading back east on the same route is reversed.
	o.Track = 85
	if sc := Evaluate(stopsOf("EGLL", "KJFK"), o); sc.State != StateReversed {
		t.Fatalf("reversed: %+v", sc)
	}
	// Far off the route (over Spain).
	if sc := Evaluate(stopsOf("EGLL", "KJFK"), Obs{Lat: 40, Lon: -4, AltFt: 37000, VR: 0, Track: 265, GS: 480}); sc.State != StateDoubtful {
		t.Fatalf("off route: %+v", sc)
	}
}

// The old "within 40 km of any airport" rule passed hub overflights: an
// airliner cruising at FL380 over Heathrow is not arriving there.
func TestEvaluateHubOverflight(t *testing.T) {
	o := Obs{Lat: 51.47, Lon: -0.45, AltFt: 38000, VR: 0, Track: 330, GS: 470}
	if sc := Evaluate(stopsOf("LIRF", "EGLL"), o); sc.State == StateFits || sc.State == StateUncertain {
		t.Fatalf("FL380 over the destination fits: %+v", sc)
	}
	// Descending into it is fine.
	o.AltFt, o.VR, o.Track = 9000, -1200, 270
	if sc := Evaluate(stopsOf("LIRF", "EGLL"), o); sc.State != StateFits {
		t.Fatalf("descending into the destination: %+v", sc)
	}
	// Climbing out of the destination at 5000 ft does not fit that route.
	o.AltFt, o.VR = 5000, 2000
	if sc := Evaluate(stopsOf("LIRF", "EGLL"), o); sc.State == StateFits {
		t.Fatalf("climbing out of the destination: %+v", sc)
	}
	// ... but fits the reverse route.
	if sc := Evaluate(stopsOf("EGLL", "LIRF"), o); sc.State != StateFits {
		t.Fatalf("climbing out of the origin: %+v", sc)
	}
}

func TestEvaluateMultiLegActiveLeg(t *testing.T) {
	// QFA1 YSSY-WSSS-EGLL over the Bay of Bengal: second leg.
	o := Obs{Lat: 15, Lon: 88, AltFt: 38000, VR: 0, Track: 300, GS: 500}
	sc := Evaluate(stopsOf("YSSY", "WSSS", "EGLL"), o)
	if sc.Leg != 1 || sc.State != StateFits {
		t.Fatalf("%+v", sc)
	}
	// Over Western Australia: first leg.
	o = Obs{Lat: -25, Lon: 125, AltFt: 38000, VR: 0, Track: 300, GS: 500}
	if sc := Evaluate(stopsOf("YSSY", "WSSS", "EGLL"), o); sc.Leg != 0 {
		t.Fatalf("%+v", sc)
	}
	// Unresolved airports: unchecked.
	st := stopsOf("YSSY")
	st = append(st, Stop{Code: "ZZZZ", ElevFt: math.NaN()})
	if sc := Evaluate(st, o); sc.State != StateUnchecked {
		t.Fatalf("%+v", sc)
	}
}

func TestETA(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// 200 NM out at FL350, 450 kt: roughly 30-35 minutes.
	dest := stop("EGLL")
	lat, lon := 51.47, -0.46-200*1.852/(111.32*math.Cos(51.47*math.Pi/180))
	eta, sig, ok := ETA(dest, Obs{Lat: lat, Lon: lon, AltFt: 35000, GS: 450, Category: "A3"}, now)
	if !ok {
		t.Fatal("no eta")
	}
	m := eta.Sub(now).Minutes()
	if m < 28 || m > 45 || sig < 90*time.Second || sig > 3*time.Minute {
		t.Fatalf("eta %.1f min sigma %v", m, sig)
	}
	if _, _, ok := ETA(dest, Obs{Lat: lat, Lon: lon, AltFt: 35000, GS: math.NaN()}, now); ok {
		t.Fatal("eta without ground speed")
	}
}

// ---------------------------------------------------------------- fusion

func TestFuse(t *testing.T) {
	o := Obs{Lat: 52.5, Lon: -30, AltFt: 37000, VR: 0, Track: 265, GS: 480}
	hs := []Hyp{
		{Key: "EGLL-KJFK", Stops: stopsOf("EGLL", "KJFK"), Claims: []Claim{{Source: SrcStanding, Logit: lStanding}}},
		{Key: "EGLL-EDDF", Stops: stopsOf("EGLL", "EDDF"), Claims: []Claim{{Source: SrcAdsbdb, Logit: lOnline}}},
	}
	f := Fuse(hs, o)
	if f.Best != 0 || f.P < 0.7 {
		t.Fatalf("%+v", f)
	}
	sum := math.Exp(0)
	for _, l := range f.Logits {
		sum += math.Exp(l)
	}
	tot := 1 / sum
	for i := range hs {
		tot += probOf(f, i)
	}
	if math.Abs(tot-1) > 1e-9 {
		t.Fatalf("probabilities sum to %v", tot)
	}
	// A standing route that fits shows normally (P >= 0.8).
	if f := Fuse(hs[:1], o); f.P < 0.8 {
		t.Fatalf("standing fits: P=%v", f.P)
	}
	// Learned prior grows with days and decays with age.
	if !(LearnedLogit(1, 0) < LearnedLogit(5, 0) && LearnedLogit(5, 0) <= 4 && LearnedLogit(5, 30*24*time.Hour) < LearnedLogit(5, 0)) {
		t.Fatal("learned logit")
	}
}

// ---------------------------------------------------------------- service

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newSvc(t *testing.T, nav *fakeNav, st *store.Store, cfg model.RoutesConfig) (*Service, *clock) {
	t.Helper()
	ck := &clock{t: time.Unix(1_800_000_000, 0)}
	dir := t.TempDir()
	s := New(Options{Store: st, Nav: nav, Config: func() model.RoutesConfig { return cfg }, Now: ck.now,
		OverridesFile: filepath.Join(dir, "routes.overrides.csv")})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return s, ck
}

func defCfg() model.RoutesConfig { return model.DefaultConfig().NavData.Routes }

// live polls Live until it returns non-nil (the resolver is asynchronous).
func live(t *testing.T, s *Service, a *model.Aircraft, now func() time.Time) *model.RouteShort {
	t.Helper()
	for i := 0; i < 400; i++ {
		if r := s.Live(a, now()); r != nil {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

func TestServiceStandingRoute(t *testing.T) {
	nav := &fakeNav{standing: map[string][]string{"BAW117": {"EGLL", "KJFK"}}}
	s, ck := newSvc(t, nav, nil, defCfg())
	// Over Newfoundland, 650 NM out: within ETA range (<= 1500 NM).
	a := aircraft("400001", "BAW117", 47, -60, 37000, 245, 480, 0)
	r := live(t, s, a, ck.now)
	if r == nil {
		t.Fatal("no route")
	}
	if r.From != "EGLL" || r.To != "KJFK" || r.FromIATA != "LHR" || r.Source != SrcStanding || r.Class != ClassOpen ||
		r.State != StateFits || r.Conf < 0.8 || !r.Plausible || r.ETA == 0 || r.ETA%30 != 0 || r.AlongPct == 0 {
		t.Fatalf("%+v", r)
	}
	if len(nav.reqs()) != 0 {
		t.Fatal("online request for a callsign with standing data")
	}
	// Not an airline callsign: no route, no look-up.
	if r := s.Live(aircraft("400002", "GABCD", 52, 0, 3000, 90, 120, 0), ck.now()); r != nil {
		t.Fatal(r)
	}
}

func TestServiceMultiStopActiveLeg(t *testing.T) {
	nav := &fakeNav{standing: map[string][]string{"QFA1": {"YSSY", "WSSS", "EGLL"}}}
	s, ck := newSvc(t, nav, nil, defCfg())
	r := live(t, s, aircraft("7C0001", "QFA1", 15, 88, 38000, 300, 500, 0), ck.now)
	if r == nil || r.From != "WSSS" || r.To != "EGLL" || r.Leg != 2 || r.Legs != 2 || r.Via != 1 || len(r.Stops) != 3 {
		t.Fatalf("%+v", r)
	}
}

// Hysteresis: a single contrary evaluation does not change the shown state.
func TestServiceStateHysteresis(t *testing.T) {
	nav := &fakeNav{standing: map[string][]string{"BAW117": {"EGLL", "KJFK"}}}
	s, ck := newSvc(t, nav, nil, defCfg())
	a := aircraft("400001", "BAW117", 52.5, -30, 37000, 265, 480, 0)
	if r := live(t, s, a, ck.now); r == nil || r.State != StateFits {
		t.Fatalf("%+v", r)
	}
	a.Track = f64(85) // reversed
	ck.add(31 * time.Second)
	if r := s.Live(a, ck.now()); r.State != StateFits {
		t.Fatalf("switched after one evaluation: %+v", r)
	}
	ck.add(31 * time.Second)
	if r := s.Live(a, ck.now()); r.State != StateReversed {
		t.Fatalf("not switched after two evaluations: %+v", r)
	}
}

// Online look-ups: only after the callsign was stable for stableSec, only
// when nothing offline exists, and the answer is restricted class.
func TestServiceOnline(t *testing.T) {
	nav := &fakeNav{online: map[string][]string{"EZY12AB": {"EGPH", "LIRF"}}}
	cfg := defCfg()
	s, ck := newSvc(t, nav, nil, cfg)
	a := aircraft("400003", "EZY12AB", 50, 2, 37000, 140, 450, 0)
	for i := 0; i < 50; i++ {
		_ = s.Live(a, ck.now())
		time.Sleep(2 * time.Millisecond)
	}
	if n := len(nav.reqs()); n != 0 {
		t.Fatalf("%d online requests before the callsign was stable", n)
	}
	ck.add(time.Duration(cfg.StableSec) * time.Second)
	r := live(t, s, a, ck.now)
	if r == nil || r.Source != SrcAdsbdb || r.Class != ClassRestricted {
		t.Fatalf("%+v", r)
	}
	if q := nav.reqs(); len(q) != 1 || q[0] != "EZY12AB" {
		t.Fatalf("requests %v", q)
	}
	// Online off: never.
	cfg.Online = false
	nav2 := &fakeNav{online: map[string][]string{"EZY12AB": {"EGPH", "LIRF"}}}
	s2, ck2 := newSvc(t, nav2, nil, cfg)
	ck2.add(time.Hour)
	for i := 0; i < 50; i++ {
		_ = s2.Live(a, ck2.now())
		ck2.add(time.Minute)
		time.Sleep(2 * time.Millisecond)
	}
	if n := len(nav2.reqs()); n != 0 {
		t.Fatalf("%d online requests with online look-ups off", n)
	}
}

// Restricted answers are hidden from the detail view of non-own users.
func TestDetailRestricted(t *testing.T) {
	nav := &fakeNav{cached: map[string][]string{"EZY12AB": {"EGPH", "LIRF"}}, standing: map[string][]string{"BAW117": {"EGLL", "KJFK"}}}
	s, _ := newSvc(t, nav, nil, defCfg())
	ctx := context.Background()
	d, err := s.Detail(ctx, "EZY12AB", DetailOptions{IncludeRestricted: false})
	if err != nil || len(d.Hyps) != 0 || d.Hidden != 1 || d.Route != nil {
		t.Fatalf("%+v %v", d, err)
	}
	d, err = s.Detail(ctx, "EZY12AB", DetailOptions{IncludeRestricted: true})
	if err != nil || len(d.Hyps) != 1 || d.Route == nil || d.Route.Class != ClassRestricted {
		t.Fatalf("%+v %v", d, err)
	}
	d, err = s.Detail(ctx, "BAW117", DetailOptions{})
	if err != nil || d.Route == nil || len(d.Path) < 10 {
		t.Fatalf("%+v %v", d, err)
	}
	// Public detail never goes online.
	if _, err := s.Detail(ctx, "DLH400", DetailOptions{}); err != nil || len(nav.reqs()) != 0 {
		t.Fatalf("online request from a public detail: %v %v", nav.reqs(), err)
	}
}

func TestOverrides(t *testing.T) {
	st := openStore(t)
	nav := &fakeNav{standing: map[string][]string{"BAW117": {"EGLL", "KJFK"}, "DLH400": {"EDDF", "KJFK"}}}
	s, ck := newSvc(t, nav, st, defCfg())
	ctx := context.Background()
	if _, err := s.PutOverride(ctx, Override{Callsign: "BAW117", Airports: "egll eham", Mode: "fixed", Note: "seasonal"}, "tester"); err != nil {
		t.Fatal(err)
	}
	a := aircraft("400001", "BAW117", 52.0, 2.0, 20000, 80, 400, -500)
	r := live(t, s, a, ck.now)
	if r == nil || r.To != "EHAM" || r.Source != SrcOverride || r.Conf != 1 {
		t.Fatalf("%+v", r)
	}
	if _, err := s.PutOverride(ctx, Override{Callsign: "DLH400", Mode: "block"}, "tester"); err != nil {
		t.Fatal(err)
	}
	b := aircraft("3C0001", "DLH400", 50, 8, 30000, 270, 450, 0)
	for i := 0; i < 100; i++ {
		if r := s.Live(b, ck.now()); r != nil {
			t.Fatalf("blocked callsign shows %+v", r)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := s.PutOverride(ctx, Override{Callsign: "BAW1", Airports: "EGLL", Mode: "fixed"}, "tester"); err == nil {
		t.Fatal("one-airport override accepted")
	}
	if err := s.DeleteOverride(ctx, "DLH400", "tester"); err != nil {
		t.Fatal(err)
	}
	log, err := s.OverrideLog(ctx, 10)
	if err != nil || len(log) != 3 || log[0].Callsign != "DLH400" || log[0].Before != "block" || log[2].After != "fixed EGLL-EHAM" {
		t.Fatalf("%+v %v", log, err)
	}
	// Export round-trips.
	var buf bytes.Buffer
	if err := s.Export(ctx, &buf, ExportOverrides, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, bad := ParseOverridesCSV(&buf)
	if len(bad) != 0 || len(got) != 1 || got[0].Airports != "EGLL-EHAM" || got[0].Note != "seasonal" {
		t.Fatalf("%+v %v", got, bad)
	}
	// File rows are locked and win.
	if err := os.WriteFile(s.o.OverridesFile, []byte("callsign,airports,mode,note\nBAW117,EGLL-LIRF,fixed,from file\nXX,bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.reloadFile()
	if _, err := s.PutOverride(ctx, Override{Callsign: "BAW117", Airports: "EGLL-EDDF"}, "tester"); err != ErrLocked {
		t.Fatalf("locked row edited: %v", err)
	}
	if len(s.FileProblems()) != 1 {
		t.Fatalf("problems %v", s.FileProblems())
	}
	ck.add(2 * time.Hour)
	if r := live(t, s, a, ck.now); r == nil || r.To != "LIRF" {
		t.Fatalf("%+v", r)
	}
}

// ---------------------------------------------------------------- learner

func TestLearner(t *testing.T) {
	st := openStore(t)
	nav := &fakeNav{standing: map[string][]string{"BAW117": {"EGLL", "KJFK"}}}
	s, ck := newSvc(t, nav, st, defCfg())
	ctx := context.Background()
	day := int64(86400)
	t0 := ck.now().Unix() - 10*day
	var flight int64
	leg := func(cs, dep, arr string, start int64, depConf, arrConf float64) Leg {
		flight++
		return Leg{Flight: flight, Callsign: cs, Start: start, End: start + 3600, Dep: dep, DepConf: depConf, DepTier: "T1", Arr: arr, ArrConf: arrConf, ArrTier: "L1"}
	}
	// One day: nothing promoted yet.
	if err := s.CloseLeg(ctx, leg("EZY85TQ", "EGPH", "LIRF", t0, 0.95, 0.95)); err != nil {
		t.Fatal(err)
	}
	if ls, _ := learnedFor(ctx, st.DB(), "EZY85TQ"); len(ls) != 0 {
		t.Fatalf("promoted after one day: %+v", ls)
	}
	// Second day: promoted (no standing route).
	if err := s.CloseLeg(ctx, leg("EZY85TQ", "EGPH", "LIRF", t0+day, 0.95, 0.95)); err != nil {
		t.Fatal(err)
	}
	ls, _ := learnedFor(ctx, st.DB(), "EZY85TQ")
	if len(ls) != 1 || ls[0].Airports != "EGPH-LIRF" || ls[0].Days != 2 || ls[0].P < 0.8 {
		t.Fatalf("%+v", ls)
	}
	// Low-confidence legs do not teach anything.
	if err := s.CloseLeg(ctx, leg("RYR1234", "EGPH", "LIRF", t0, 0.5, 0.4)); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseLeg(ctx, leg("RYR1234", "EGPH", "LIRF", t0+day, 0.5, 0.4)); err != nil {
		t.Fatal(err)
	}
	if ls, _ := learnedFor(ctx, st.DB(), "RYR1234"); len(ls) != 0 {
		t.Fatalf("learned from weak events: %+v", ls)
	}
	// A route contradicting the standing data needs much more evidence
	// (prior 0.8 on the standing route) and then marks it stale.
	for i := int64(0); i < 20; i++ {
		if err := s.CloseLeg(ctx, leg("BAW117", "EGLL", "EHAM", t0+i*day/2+i, 0.95, 0.95)); err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			if ls, _ := learnedFor(ctx, st.DB(), "BAW117"); len(ls) != 0 {
				t.Fatalf("standing overridden after 2 days: %+v", ls)
			}
		}
	}
	ls, _ = learnedFor(ctx, st.DB(), "BAW117")
	if len(ls) != 1 || ls[0].Airports != "EGLL-EHAM" {
		t.Fatalf("%+v", ls)
	}
	if !ls[0].StaleStanding {
		t.Fatalf("standing not marked stale: %+v", ls[0])
	}
	// The learned route wins over the stale standing route live.
	a := aircraft("400001", "BAW117", 52.0, 2.0, 20000, 80, 400, -500)
	r := live(t, s, a, ck.now)
	if r == nil || r.To != "EHAM" || r.Source != SrcLearned {
		t.Fatalf("%+v", r)
	}
	// Export: only P >= 0.9 and >= 3 days, VRS format with the CC0 header.
	var buf bytes.Buffer
	if err := s.Export(ctx, &buf, ExportLearned, 0, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "Callsign,") || !strings.Contains(out, "Callsign,Code,Number,AirlineCode,AirportCodes") || strings.Contains(out, "EZY85TQ,") {
		t.Fatalf("export:\n%s", out)
	}
	buf.Reset()
	if err := s.Export(ctx, &buf, ExportLegs, 0, 0); err != nil || strings.Count(buf.String(), "\n") != 25 {
		t.Fatalf("legs export: %v\n%s", err, buf.String())
	}
	// Pruning after 120 days removes the observations.
	ck.add(130 * 24 * time.Hour)
	if err := st.Write(ctx, func(tx *sql.Tx) error { _, err := pruneObs(ctx, tx, ck.now()); return err }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM routes_obs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d observations after prune (%v)", n, err)
	}
}

func TestSeasonChange(t *testing.T) {
	got := lastSeasonChange(time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("%v want %v", got, want)
	}
	got = lastSeasonChange(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2025, 10, 26, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("%v want %v", got, want)
	}
}

// The 1 Hz hit path does no I/O and allocates only the DTO.
func BenchmarkLiveHit(b *testing.B) {
	nav := &fakeNav{standing: map[string][]string{"BAW117": {"EGLL", "KJFK"}}}
	cfg := defCfg()
	s := New(Options{Nav: nav, Config: func() model.RoutesConfig { return cfg }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	a := aircraft("400001", "BAW117", 52.5, -30, 37000, 265, 480, 0)
	for s.Live(a, time.Now()) == nil {
		time.Sleep(time.Millisecond)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Live(a, time.Now())
	}
}

var _ = bp
