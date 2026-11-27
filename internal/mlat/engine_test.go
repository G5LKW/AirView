package mlat

import (
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/group"
	"airview/internal/mlat/linalg"
	"airview/internal/model"
)

func testEngine(t testing.TB, mut func(o *Options)) *Engine {
	cfg := model.DefaultAggregator().MLAT
	o := Options{Config: cfg, Workers: -1, OriginUnixNs: 1.8e18,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return time.Unix(0, 1.8e18) }}
	if mut != nil {
		mut(&o)
	}
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

// DESIGN §10.1 resolution order and the AutoUseSurvey rule of §19.3.
func TestStationPositionResolution(t *testing.T) {
	var got []PosSource
	e := testEngine(t, func(o *Options) {
		o.Config.AutoUseSurvey = true
		o.Config.AutoUseMaxSigmaM = 50
		o.OnPosition = func(_ model.StationIdx, s PosSource) { got = append(got, s) }
	})
	p := geo.ToECEF(51, 9, 100)
	q := geo.ToECEF(51.001, 9, 100)
	st := model.StationIdx(7)
	e.SetStation(st, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: p}})
	if s := e.PositionSource(st); s != PosDeclared {
		t.Fatalf("declared: %v", s)
	}
	sv := Survey{Valid: true, ECEF: q, SigmaHM: 30, SigmaVM: 20, Stable: true}
	e.SetSurvey(st, sv)
	if s := e.PositionSource(st); s != PosSurvey {
		t.Fatalf("auto-used survey: %v", s)
	}
	pos, sig, ok := (&sitesAdapter{tab: e.st.load()}).Site(st)
	if !ok || pos != q || sig > 30 {
		t.Fatalf("site %v %.1f %v", pos, sig, ok)
	}
	for _, bad := range []Survey{
		{Valid: true, ECEF: q, SigmaHM: 80, Stable: true},                      // σ too large
		{Valid: true, ECEF: q, SigmaHM: 30, Stable: false},                     // not stable
		{Valid: true, ECEF: q, SigmaHM: 30, Stable: true, Suspect: true},       // failed checks
		{Valid: true, ECEF: q, SigmaHM: 30, Stable: true, OwnerRejected: true}, // owner override
	} {
		e.SetSurvey(st, bad)
		if s := e.PositionSource(st); s != PosDeclared {
			t.Fatalf("survey %+v used: %v", bad, s)
		}
	}
	e.SetSurvey(st, sv)
	e.SetStation(st, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: p}, GNSS: Position{Valid: true, ECEF: p}})
	if s := e.PositionSource(st); s != PosGNSS {
		t.Fatalf("GNSS first: %v", s)
	}
	want := []PosSource{PosDeclared, PosSurvey, PosDeclared, PosSurvey, PosGNSS}
	if len(got) != len(want) {
		t.Fatalf("notifications %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("notifications %v want %v", got, want)
		}
	}
	// a suspect declared position gets its σ inflated by the discrepancy
	e.SetStation(9, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: p, SigmaM: 10}, Suspect: true, DiscrepancyM: 400})
	if _, sig, _ := (&sitesAdapter{tab: e.st.load()}).Site(9); sig < 400 {
		t.Fatalf("suspect σ %.0f", sig)
	}
	// AutoUseSurvey off: the survey is never used automatically
	e2 := testEngine(t, func(o *Options) { o.Config.AutoUseSurvey = false })
	e2.SetStation(st, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: p}})
	e2.SetSurvey(st, sv)
	if s := e2.PositionSource(st); s != PosDeclared {
		t.Fatalf("AutoUseSurvey off: %v", s)
	}
	e2.RemoveStation(st)
	if s := e2.PositionSource(st); s != PosNone {
		t.Fatalf("removed: %v", s)
	}
}

type fakeSender struct {
	ok   map[model.StationIdx]bool
	sent map[model.StationIdx][]model.MLATFix
}

func (f *fakeSender) SendResults(st model.StationIdx, fx []model.MLATFix) bool {
	if !f.ok[st] {
		return false
	}
	for _, x := range fx {
		if x.Vel != nil {
			v := *x.Vel
			x.Vel = &v
		}
		f.sent[st] = append(f.sent[st], x)
	}
	return true
}

// DESIGN §8.4: results only to stations that contributed within 60 s and
// delivered >= 10 timed frames per minute while synced; never to others.
func TestResultRoutingContributionRule(t *testing.T) {
	avf := &fakeSender{ok: map[model.StationIdx]bool{2: true, 3: true, 4: true}, sent: map[model.StationIdx][]model.MLATFix{}}
	mc := &fakeSender{ok: map[model.StationIdx]bool{5: true}, sent: map[model.StationIdx][]model.MLATFix{}}
	e := testEngine(t, func(o *Options) { o.Results = []ResultSender{avf, mc} })
	w := e.workers[0]
	now := int64(1.8e18)
	v := [3]float32{100, 50, 0}
	a := &acState{lastFix: model.MLATFix{ICAO: 0x4840d6, Grade: model.GradeA, Vel: &v}}
	a.addContrib(2, now-10e9) // recent, busy
	a.addContrib(3, now-90e9) // contributed too long ago
	a.addContrib(4, now-5e9)  // recent but too few timed frames
	a.addContrib(5, now-1e9)  // recent, busy, mlat-client
	e.rates.add(2, 30)
	e.rates.add(4, 5)
	e.rates.add(5, 12)
	e.rates.rotate()
	w.route(a, now)
	e.route.flush()
	if len(avf.sent[2]) != 1 || len(avf.sent[3]) != 0 || len(avf.sent[4]) != 0 || len(mc.sent[5]) != 1 || len(avf.sent[5]) != 0 {
		t.Fatalf("sent avf %v mlatc %v", avf.sent, mc.sent)
	}
	if f := avf.sent[2][0]; f.ICAO != 0x4840d6 || f.Vel == nil || f.Vel[0] != 100 {
		t.Fatalf("fix %+v", f)
	}
	// the router copied the velocity: changing the state does not change
	// what was queued
	v[0] = 7
	w.route(a, now)
	v[0] = 9
	e.route.flush()
	if got := avf.sent[2][1].Vel[0]; got != 7 {
		t.Fatalf("velocity %v", got)
	}
	// results off: nothing is routed
	e.cfg.ReturnResults = false
	w.route(a, now)
	e.route.flush()
	if len(avf.sent[2]) != 2 {
		t.Fatal("routed with ReturnResults off")
	}
	st := e.Stats()
	if st.ResultsSent != 4 || st.ResultsRefused != 2 {
		t.Fatalf("sent %d refused %d", st.ResultsSent, st.ResultsRefused)
	}
}

func TestContribRing(t *testing.T) {
	var a acState
	for i := 0; i < 40; i++ {
		a.addContrib(model.StationIdx(i), int64(i))
	}
	if a.nContrib != maxContrib {
		t.Fatal(a.nContrib)
	}
	for i := 0; i < maxContrib; i++ {
		if a.contrib[i].st < 8 {
			t.Fatalf("oldest not replaced: %+v", a.contrib[i])
		}
	}
	a.addContrib(39, 100)
	n := 0
	for i := 0; i < a.nContrib; i++ {
		if a.contrib[i].st == 39 {
			n++
			if a.contrib[i].tNs != 100 {
				t.Fatal("not refreshed")
			}
		}
	}
	if n != 1 {
		t.Fatal("duplicate")
	}
}

// Hold-out aircraft are hidden from the clock filter's reference source.
func TestHoldoutHiddenFromSync(t *testing.T) {
	e := testEngine(t, func(o *Options) { o.Config.Holdout = 0.5 })
	now := int64(1.8e18)
	e.nowNs.Store(now)
	var in, out uint32
	for k := uint32(0x400000); in == 0 || out == 0; k++ {
		if e.hold.Selected(k, now) && in == 0 {
			in = k
		} else if !e.hold.Selected(k, now) && out == 0 {
			out = k
		}
	}
	e.hold.NoteNACp(in, 10, now)
	e.hold.NoteNACp(out, 10, now)
	if !e.hold.In(in, now) || e.hold.In(out, now) {
		t.Fatal("selection")
	}
	// no reference tracks exist, so both are unusable; the point is that
	// the hold-out one never reaches the store
	if _, _, ok := e.href.RefPosition(in, 17, now); ok {
		t.Fatal("hold-out reference offered")
	}
	if e.href.Usable(in) {
		t.Fatal("hold-out usable")
	}
	// the ingest view never gates by network time nor flushes single copies
	v := e.ClockView()
	if _, _, ok := v.NetTime(2, 1, 1000); ok || v.SyncCandidate(out, 17) {
		t.Fatal("ingest view")
	}
}

func TestMahal3(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	for k := 0; k < 100; k++ {
		var A [9]float64
		for i := range A {
			A[i] = r.NormFloat64()
		}
		var S [9]float64
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				for m := 0; m < 3; m++ {
					S[i*3+j] += A[i*3+m] * A[j*3+m]
				}
			}
			S[i*3+i] += 0.1
		}
		d := geo.Vec3{X: r.NormFloat64(), Y: r.NormFloat64(), Z: r.NormFloat64()}
		got, ok := mahal3(&S, d)
		L := S
		if err := linalg.Cholesky(L[:], 3); err != nil {
			t.Fatal(err)
		}
		want := linalg.MahalanobisSq(L[:], 3, []float64{d.X, d.Y, d.Z}, nil)
		if !ok || math.Abs(got-want) > 1e-9*math.Max(1, want) {
			t.Fatalf("mahal3 %g want %g", got, want)
		}
	}
}

// The Mode A/C seam: disabled unless configured; a consistent cluster of
// 5 anchors is solved.
func TestModeACSeam(t *testing.T) {
	e := testEngine(t, nil)
	if err := e.EnableModeAC(nil); err != ErrModeACDisabled {
		t.Fatalf("enabled without config: %v", err)
	}
	var fixes []ModeACFix
	e = testEngine(t, func(o *Options) { o.Config.ModeAC = true })
	if err := e.EnableModeAC(func(f ModeACFix) { fixes = append(fixes, f) }); err != nil {
		t.Fatal(err)
	}
	ac := geo.ToECEF(51.1, 9.2, 3000)
	atm := e.Atmosphere()
	c := group.ACCluster{Code: 0x1234}
	t0 := group.MakeTime(1.8e18, 0)
	for i := 0; i < 5; i++ {
		a := float64(i) / 5 * 2 * math.Pi
		p := geo.ToECEF(51+0.4*math.Cos(a), 9+0.6*math.Sin(a), 150)
		c.Members = append(c.Members, group.ACMember{Station: model.StationIdx(i + 2), Pos: p, Var: 30 * 30,
			T: t0.Add(atm.PropagationNs(p, ac))})
	}
	st := e.modeAC.Load()
	e.solveModeAC(st, &c)
	if len(fixes) != 1 || fixes[0].Code != 0x1234 || fixes[0].NStations != 5 {
		t.Fatalf("fixes %+v", fixes)
	}
	f := fixes[0]
	if d := ac.Dist(geo.Vec3{X: f.ECEF[0], Y: f.ECEF[1], Z: f.ECEF[2]}); d > 50 {
		t.Fatalf("Mode A/C fix %.0f m off", d)
	}
	// replies of unsynced stations are dropped at the door
	e.ObserveModeAC([]ModeACReply{{Station: 2, Epoch: 1, Ticks: 5, Code: 0x1234}})
	if len(st.buf) != 0 {
		t.Fatal("unsynced reply buffered")
	}
}

// Backpressure: a full worker queue drops (and counts) groups instead of
// blocking the ingest; Drain and Close work.
func TestQueueBackpressure(t *testing.T) {
	e := testEngine(t, func(o *Options) { o.Workers = 2; o.QueueLen = 4 })
	for _, w := range e.workers {
		close(w.quit) // stop the consumers so the queues fill up
	}
	e.wg.Wait()
	g := ingest.Group{Key: 0x400001, Data: make([]byte, 7), Kind: model.FrameModeSShort, FirstNs: 1.8e18,
		Copies: []ingest.Copy{{Station: 2, Timed: true}, {Station: 3, Timed: true}}}
	for i := 0; i < 100; i++ {
		g.Key = uint32(0x400000 + i)
		e.Observe(&g)
	}
	st := e.Stats()
	if st.Groups != 100 || st.Dropped < 90 || st.Dropped > 92 {
		t.Fatalf("groups %d dropped %d", st.Groups, st.Dropped)
	}
	e.closed.Store(true)
}

func TestDrainAndClose(t *testing.T) {
	e := testEngine(t, func(o *Options) { o.Workers = 3 })
	g := ingest.Group{Key: 0x400001, Data: make([]byte, 7), Kind: model.FrameModeSShort, FirstNs: 1.8e18,
		Copies: []ingest.Copy{{Station: 2, Timed: true}, {Station: 3, Timed: true}}}
	for i := 0; i < 1000; i++ {
		g.Key = uint32(0x400000 + i)
		e.Observe(&g)
	}
	ctx := t.Context()
	if err := e.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.Stats(); st.Groups != 1000 || st.Dropped != 0 || st.QueueFill != 0 {
		t.Fatalf("%+v", st)
	}
	e.Advance(1.8e18 + 2e9) // ticks reach the workers
	e.Close()
	e.Close()
	e.Observe(&g) // ignored after Close
}
