package survey

import (
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/ref"
	"airview/internal/model"
)

type nilClock struct{}

func (nilClock) Snapshot() *clock.Snapshot { return nil }

type nilRefs struct{}

func (nilRefs) TransmitInstant(uint32, int64, []ref.Arrival, *geo.Atmosphere, *ref.State) (float64, float64, bool) {
	return 0, 0, false
}

// recorder implements Sink, DeclaredFlagger and Notifier.
type recorder struct {
	mu      sync.Mutex
	surveys map[model.StationIdx]mlat.Survey
	flags   map[model.StationIdx]bool
	results []Result
}

func newRecorder() *recorder {
	return &recorder{surveys: map[model.StationIdx]mlat.Survey{}, flags: map[model.StationIdx]bool{}}
}
func (r *recorder) SetSurvey(st model.StationIdx, sv mlat.Survey) {
	r.mu.Lock()
	r.surveys[st] = sv
	r.mu.Unlock()
}
func (r *recorder) SetDeclaredSuspect(st model.StationIdx, s bool, _ float64) {
	r.mu.Lock()
	r.flags[st] = s
	r.mu.Unlock()
}
func (r *recorder) SurveyFinished(res Result) {
	r.mu.Lock()
	r.results = append(r.results, res)
	r.mu.Unlock()
}

type fakeNow struct{ t time.Time }

func (f *fakeNow) now() time.Time { return f.t }

func newAPI(t testing.TB, cfg Config) (*Surveyor, *recorder, *fakeNow) {
	t.Helper()
	clk := &fakeNow{t: time.Unix(0, unixBase)}
	rec := newRecorder()
	cfg.AutoUse = true
	s, err := New(Options{Config: cfg, Clock: nilClock{}, Refs: nilRefs{}, Sink: rec, Declared: rec, Notifier: rec,
		Now: clk.now, Synchronous: true, Undulation: func(float64, float64) (float64, bool) { return 47, true },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s, rec, clk
}

func TestNewRequiresSources(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without Clock/Refs must fail")
	}
}

func TestConfigNormalize(t *testing.T) {
	c := DefaultConfig()
	d := c
	d.Normalize()
	if c != d {
		t.Fatal("Normalize is not idempotent")
	}
	if !c.AutoUse || c.AutoUseMaxSigmaM != 50 || c.CheckEvery != 6*time.Hour || c.SuspectMinM != 100 || c.SuspectK != 3 ||
		c.StableSigmaGPSM != 30 || c.StableSigmaM != 200 || c.HeightSigmaM != 20 || c.DefaultAGLM != 10 {
		t.Fatalf("defaults %+v", c)
	}
	bad := Config{AutoUseMaxSigmaM: math.NaN(), MaskDeg: 99, HuberK: 9, RejectK: 3, MinDuration: time.Hour, MaxDuration: time.Minute}
	bad.Normalize()
	if bad.AutoUseMaxSigmaM != 50 || bad.MaskDeg != 15 || bad.RejectK < bad.HuberK || bad.MaxDuration < bad.MinDuration {
		t.Fatalf("clamping %+v", bad)
	}
	m := model.DefaultAggregator().MLAT
	m.SurveyIntervalH = 12
	m.AutoUseSurvey = false
	mc := ConfigFromModel(m)
	if mc.AutoUse || mc.CheckEvery != 12*time.Hour || mc.AutoUseMaxSigmaM != 50 {
		t.Fatalf("ConfigFromModel %+v", mc)
	}
}

// TestConsentGate: no survey without timestamps (DESIGN §10.5), neither
// requested nor scheduled; withdrawing consent cancels a running job.
func TestConsentGate(t *testing.T) {
	s, rec, _ := newAPI(t, Config{})
	st := model.StationIdx(7)
	if _, err := s.Start(st, ReasonOwner); !errors.Is(err, ErrUnknownStation) {
		t.Fatalf("unknown station: %v", err)
	}
	s.SetStation(st, Station{Class: model.ClockSDR12, Timestamps: false})
	if _, err := s.Start(st, ReasonAdmin); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("no consent: %v", err)
	}
	s.RequestSurvey(st, "test")
	s.Tick()
	if st2, _ := s.Status(st); st2.Running {
		t.Fatal("a station without consent was surveyed")
	}
	s.SetStation(st, Station{Class: model.ClockNone, Timestamps: true})
	if _, err := s.Start(st, ReasonAdmin); !errors.Is(err, ErrNotTimed) {
		t.Fatalf("untimed class: %v", err)
	}
	s.SetStation(st, Station{Class: model.ClockSDR12, Timestamps: true})
	s.Tick() // unlocated: scheduled at once
	stt, _ := s.Status(st)
	if !stt.Running || stt.Reason != ReasonUnlocated {
		t.Fatalf("unlocated station not scheduled: %+v", stt)
	}
	s.SetStation(st, Station{Class: model.ClockSDR12, Timestamps: false})
	stt, _ = s.Status(st)
	if stt.Running || s.Stats().Cancelled != 1 || len(rec.results) != 0 {
		t.Fatalf("consent withdrawal did not cancel: %+v %+v", stt, s.Stats())
	}
	if s.active.Load().has(st) {
		t.Fatal("cancelled job still a tap target")
	}
}

func TestSchedulerReasons(t *testing.T) {
	s, _, _ := newAPI(t, Config{MaxCheckFrac: 1})
	s.SetStation(1, Station{Class: model.ClockGPSUTC, Timestamps: true, GNSS: true})
	s.SetStation(2, Station{Class: model.ClockHW12, Timestamps: true, Confirmed: true})
	s.SetStation(3, Station{Class: model.ClockHW12, Timestamps: true, Declared: &Declared{Lat: 51, Lon: 9, Alt: 100, AltRef: model.AltRefMSL}})
	s.SetStation(4, Station{Class: model.ClockHW12, Timestamps: true, Declared: &Declared{Lat: 51, Lon: 9, Alt: 100, AltRef: model.AltRefAmbiguous}})
	s.SetStation(5, Station{Class: model.ClockHW12, Timestamps: true})
	s.Tick()
	want := map[model.StationIdx]Reason{3: ReasonCheck, 4: ReasonAltRef, 5: ReasonUnlocated}
	for st := model.StationIdx(1); st <= 5; st++ {
		stt, _ := s.Status(st)
		if w, ok := want[st]; ok != stt.Running || stt.Reason != w {
			t.Errorf("station %d: running %v reason %q, want %v %q", st, stt.Running, stt.Reason, ok, w)
		}
	}
	// a GNSS station can still be surveyed on request
	if _, err := s.Start(1, ReasonAdmin); err != nil {
		t.Fatal(err)
	}
}

func TestCheckFractionAndBusy(t *testing.T) {
	s, _, clk := newAPI(t, Config{MaxJobs: 3})
	for st := model.StationIdx(1); st <= 20; st++ {
		s.SetStation(st, Station{Class: model.ClockHW12, Timestamps: true, Declared: &Declared{Lat: 51, Lon: 9, AltRef: model.AltRefHAE}})
	}
	s.Tick()
	if n := s.Stats().Running; n != 2 { // 10 % of 20 stations
		t.Fatalf("running checks %d, want 2", n)
	}
	s.SetStation(30, Station{Class: model.ClockHW12, Timestamps: true}) // unlocated: not capped by the fraction
	s.Tick()
	if n := s.Stats().Running; n != 3 {
		t.Fatalf("running %d, want 3", n)
	}
	s.SetStation(31, Station{Class: model.ClockHW12, Timestamps: true})
	if _, err := s.Start(31, ReasonAdmin); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	if s.Stats().Queued != 1 {
		t.Fatal("busy request not queued")
	}
	// the jobs end without data; the queued request is started first
	clk.t = clk.t.Add(11 * time.Minute)
	s.Tick()
	s.Tick()
	stt, _ := s.Status(31)
	if !stt.Running || stt.Reason != ReasonAdmin {
		t.Fatalf("queued request not started: %+v", stt)
	}
	if s.Stats().NoData < 3 {
		t.Fatalf("no-data jobs: %+v", s.Stats())
	}
}

func TestOwnerRateLimitAndCancel(t *testing.T) {
	s, _, clk := newAPI(t, Config{})
	s.SetStation(9, Station{Class: model.ClockHW12, Timestamps: true, Confirmed: true})
	id, err := s.Start(9, ReasonOwner)
	if err != nil || id == 0 {
		t.Fatal(err)
	}
	if id2, err := s.Start(9, ReasonOwner); err != nil || id2 != id {
		t.Fatalf("a running job must be reused: %d %v", id2, err)
	}
	if !s.Cancel(9) || s.Cancel(9) {
		t.Fatal("cancel")
	}
	if _, err := s.Start(9, ReasonOwner); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate limit: %v", err)
	}
	if _, err := s.Start(9, ReasonAdmin); err != nil {
		t.Fatalf("admins are not rate limited: %v", err)
	}
	s.Cancel(9)
	clk.t = clk.t.Add(61 * time.Minute)
	if _, err := s.Start(9, ReasonOwner); err != nil {
		t.Fatal(err)
	}
	s.RemoveStation(9)
	if _, ok := s.Status(9); ok || s.Stats().Running != 0 {
		t.Fatal("removed station kept")
	}
}

// feed puts synthetic observations into a running job.
func feed(t *testing.T, s *Surveyor, st model.StationIdx, obs []Obs) {
	t.Helper()
	s.mu.Lock()
	j := s.jobs[st]
	s.mu.Unlock()
	if j == nil {
		t.Fatal("no job")
	}
	for i := range obs {
		j.col.add(&obs[i], nil)
	}
}

// TestJobDeclaredSuspect: a full job on synthetic observations — the
// survey is published to the sink, a declaration 300 m off is flagged,
// and corrected by the owner the flag is cleared.
func TestJobDeclaredSuspect(t *testing.T) {
	s, rec, clk := newAPI(t, Config{MinInterval: 100 * time.Millisecond})
	sy := gpsSynth(3)
	fr := geo.NewFrame(sy.truth)
	off := geo.FromECEF(fr.FromENU(geo.Vec3{X: 212, Y: 212}))
	st := model.StationIdx(4)
	s.SetStation(st, Station{Class: model.ClockGPSUTC, Timestamps: true, AGLM: 7,
		Declared: &Declared{Lat: off.Lat, Lon: off.Lon, Alt: sy.truth.H, AltRef: model.AltRefHAE}})
	s.Tick()
	feed(t, s, st, sy.gen())
	clk.t = clk.t.Add(3 * time.Minute)
	s.Tick()
	if len(rec.results) != 1 {
		t.Fatalf("results %d (stats %+v)", len(rec.results), s.Stats())
	}
	r := rec.results[0]
	if r.State != StateDone || !r.Valid || !r.Stable || r.Suspect || !r.AutoUse {
		t.Fatalf("result %+v", r)
	}
	if !r.DeclaredChecked || !r.DeclaredSuspect || math.Abs(r.DeclaredDistM-300) > 20 || !rec.flags[st] {
		t.Fatalf("declared check: dist %.0f suspect %v flag %v", r.DeclaredDistM, r.DeclaredSuspect, rec.flags[st])
	}
	sv := rec.surveys[st]
	if !sv.Valid || !sv.Stable || sv.Suspect || sv.ECEF.Dist(sy.truth.ECEF()) > 15 {
		t.Fatalf("sink survey %+v", sv)
	}
	if !s.excluded.Load().has(st) {
		t.Fatal("a suspect station must not be a survey peer")
	}
	stt, _ := s.Status(st)
	if stt.Last == nil || stt.Last.Job != r.Job || !stt.NextCheck.Equal(clk.t.Add(6*time.Hour)) {
		t.Fatalf("status %+v", stt)
	}
	// the owner corrects the position: the verdict is withdrawn and a new
	// check scheduled at once
	s.SetStation(st, Station{Class: model.ClockGPSUTC, Timestamps: true, Declared: &Declared{Lat: sy.truth.Lat, Lon: sy.truth.Lon, Alt: sy.truth.H, AltRef: model.AltRefHAE}})
	if rec.flags[st] || s.excluded.Load().has(st) {
		t.Fatal("correction did not clear the suspect flag")
	}
	s.Tick()
	if stt, _ := s.Status(st); !stt.Running || stt.Reason != ReasonCheck {
		t.Fatalf("no re-check after the correction: %+v", stt)
	}
}

// TestJobUnstableKeepsFlag: a weak (unstable) result never clears a
// suspect flag; MaxDuration ends the job as unstable.
func TestJobUnstableKeepsFlag(t *testing.T) {
	s, rec, clk := newAPI(t, Config{MinInterval: 100 * time.Millisecond, MaxDuration: 5 * time.Minute})
	sy := sdrSynth(5)
	sy.durS, sy.aircraft, sy.sector = 240, 8, 90 // few aircraft in one sector: never stable
	st := model.StationIdx(6)
	s.SetStation(st, Station{Class: model.ClockSDR12, Timestamps: true,
		Declared: &Declared{Lat: sy.truth.Lat, Lon: sy.truth.Lon, Alt: sy.truth.H, AltRef: model.AltRefHAE}})
	s.mu.Lock()
	s.stations[st].suspect = true
	s.mu.Unlock()
	s.Tick()
	feed(t, s, st, sy.gen())
	for i := 0; i < 6; i++ {
		clk.t = clk.t.Add(time.Minute)
		s.Tick()
	}
	if len(rec.results) != 1 {
		t.Fatalf("results %d", len(rec.results))
	}
	r := rec.results[0]
	if r.State != StateUnstable || r.Stable || r.AutoUse {
		t.Fatalf("result state %s stable %v autoUse %v σ %.0f az %.0f", r.State, r.Stable, r.AutoUse, r.Solution.SigmaHM, r.Solution.AzSpreadDeg)
	}
	s.mu.Lock()
	still := s.stations[st].suspect
	s.mu.Unlock()
	if !still {
		t.Fatal("an unstable result cleared the suspect flag")
	}
}

func TestRestore(t *testing.T) {
	s, _, clk := newAPI(t, Config{})
	st := model.StationIdx(3)
	s.SetStation(st, Station{Class: model.ClockHW12, Timestamps: true, Declared: &Declared{Lat: 51, Lon: 9, AltRef: model.AltRefHAE}})
	s.Restore(st, Result{State: StateDone, Finished: clk.t.Add(-time.Hour), DeclaredSuspect: true})
	s.Tick()
	stt, _ := s.Status(st)
	if stt.Running || !stt.NextCheck.Equal(clk.t.Add(5*time.Hour)) || stt.Last == nil {
		t.Fatalf("restore: %+v", stt)
	}
	if !s.excluded.Load().has(st) {
		t.Fatal("restored suspect flag not applied to the peer set")
	}
}

// TestTapIgnoresOthers: groups without a survey target, untimed or
// map-only target copies and late deliveries never reach a job.
func TestTapIgnoresOthers(t *testing.T) {
	s, _, _ := newAPI(t, Config{})
	s.SetStation(5, Station{Class: model.ClockHW12, Timestamps: true})
	s.Tick()
	g := &ingest.Group{Kind: model.FrameModeSLong, Key: 0x4840d6, Copies: []ingest.Copy{
		{Station: 2, Timed: true}, {Station: 3, Timed: true}, {Station: 5, Timed: false}, {Station: 5, Timed: true, MapOnly: true}}}
	s.Observe(g)
	if st := s.Stats(); st.Matched != 0 || st.Groups != 1 {
		t.Fatalf("stats %+v", st)
	}
	g.Copies[2].Timed = true
	s.Observe(g) // target present; no clock snapshot: nothing collected
	if st := s.Stats(); st.Matched != 1 || st.Accepted != 0 {
		t.Fatalf("stats %+v", st)
	}
	g.Kind = model.FrameModeSShort
	s.Observe(g)
	if s.Stats().Groups != 2 {
		t.Fatal("short frames must not be used")
	}
	var nx countSink
	tee := Tee{Next: &nx, S: s}
	tee.Observe(g)
	tee.ObserveLate(g)
	if nx.n != 1 || nx.late != 1 {
		t.Fatalf("tee forwarding %+v", nx)
	}
}

type countSink struct{ n, late int }

func (c *countSink) Observe(*ingest.Group)     { c.n++ }
func (c *countSink) ObserveLate(*ingest.Group) { c.late++ }

func TestCollectorThinning(t *testing.T) {
	c := newCollector(int64(time.Second), 100)
	o := Obs{Key: 1, TauNs: unixBase}
	if !c.add(&o, nil) || c.add(&o, nil) {
		t.Fatal("thinning")
	}
	o.TauNs += 2e9
	if !c.wants(1, o.TauNs) || !c.add(&o, nil) {
		t.Fatal("second observation")
	}
	for i := 0; i < 300; i++ {
		o.Key, o.TauNs = uint32(10+i), unixBase+int64(i)*1e6
		c.add(&o, []geo.Vec3{{X: 6.4e6}})
	}
	if n := c.count(); n > 100 || n < 50 || c.interval < 4e9 || c.dropped == 0 {
		t.Fatalf("decimation: n %d interval %d", n, c.interval)
	}
	_, cen, ok, ph := c.snapshot()
	if !ok || math.Abs(cen.X-6.4e6) > 1e-6 || len(ph) == 0 {
		t.Fatal("peer centroid")
	}
}
