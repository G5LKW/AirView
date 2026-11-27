package engine

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/quality"
	"airview/internal/mlat/ref"
	"airview/internal/model"
	"airview/internal/registry"
	"airview/internal/store"
)

// hourSum emulates the quality table: per-minute slots, the report at a
// time in minute k sums the slots k-59..k (the current one partially).
type hourSum struct{ c map[int64]float64 }

func (h *hourSum) at(k int64) float64 {
	s := 0.0
	for j := k - 59; j <= k; j++ {
		s += h.c[j]
	}
	return s
}

// The hour-ring recovery reproduces the counts between two samples: the
// sample runs δ into minute k (here 1 s of 60), the error of one sample's
// value is a random walk with one step of (e_{k−59} − e_{k−60}) per hour
// (package documentation, "MLAT metrics"), and the total is exact up to that error.
func TestHourRingRecoversMinuteCounts(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3} {
		rng := rand.New(rand.NewPCG(seed, 2))
		h := &hourSum{c: map[int64]float64{}}
		var ring hourRing
		const m0 = int64(29_000_000)
		const minutes = 600
		rest := 0.0 // counts of the previous minute after its sample
		var got, want float64
		var maxErr, cumErr float64
		for k := m0; k < m0+minutes; k++ {
			full := float64(rng.IntN(60))
			if k > m0+120 && k < m0+140 {
				full = 0 // a quiet stretch
			}
			early := 0.0
			for i := 0; i < int(full); i++ {
				if rng.IntN(60) == 0 {
					early++
				}
			}
			h.c[k] += early
			d := early + rest // true counts since the previous sample
			inc := float64(ring.sample(k, h.at(k)))
			got += inc
			want += d
			cumErr = got - want
			maxErr = math.Max(maxErr, math.Abs(inc-d))
			h.c[k] += full - early
			rest = full - early
		}
		if math.Abs(cumErr) > 12 || maxErr > 16 { // 59 walks of ten ±1..2 steps
			t.Fatalf("seed %d: cumulative error %v, largest per-sample error %v", seed, cumErr, maxErr)
		}
		t.Logf("seed %d: %v counts over %d min, cumulative error %v, largest per-sample error %v", seed, want, minutes, cumErr, maxErr)
	}
}

func TestHourCounts(t *testing.T) {
	rep := func(used int, usedPct, exclPct, outPct float64) mlat.StationReport {
		return mlat.StationReport{MLAT: &quality.StationReport{ContribPerHour: used, UsedPct: usedPct, ExcludedPct: exclPct, OutlierPct: outPct}}
	}
	// 37 used, 3 excluded, 5 outliers (3 excluded + 2 |w|>4) of 40.
	u, o, ok := hourCounts(rep(37, 100*37.0/40, 100*3.0/40, 100*5.0/40))
	if !ok || u != 37 || o != 5 {
		t.Fatalf("%v %v %v", u, o, ok)
	}
	if _, _, ok := hourCounts(rep(0, 0, 100, 100)); ok {
		t.Fatal("only excluded rows cannot be recovered")
	}
	if u, o, ok := hourCounts(rep(0, 0, 0, 0)); !ok || u != 0 || o != 0 {
		t.Fatal("empty")
	}
}

type sinkCount struct{ obs, late int }

func (s *sinkCount) Observe(*ingest.Group)     { s.obs++ }
func (s *sinkCount) ObserveLate(*ingest.Group) { s.late++ }

func TestMLATTapCounts(t *testing.T) {
	next := &sinkCount{}
	tap := newMLATTap(next)
	g := &ingest.Group{Copies: []ingest.Copy{
		{Station: 2, Timed: true}, {Station: 2, Timed: true, SameStation: true}, {Station: 3, Timed: true},
		{Station: 900, Timed: false}, {Station: mlatTapMaxStation + 5, Timed: true},
	}}
	tap.Observe(g)
	tap.ObserveLate(&ingest.Group{Copies: []ingest.Copy{{Station: 70, Timed: true}}})
	if next.obs != 1 || next.late != 1 {
		t.Fatalf("not passed on: %+v", next)
	}
	for _, c := range []struct {
		st     model.StationIdx
		fr, gr uint64
	}{{2, 2, 1}, {3, 1, 1}, {900, 0, 0}, {70, 1, 1}, {1, 0, 0}} {
		if fr, gr := tap.station(c.st); fr != c.fr || gr != c.gr {
			t.Errorf("station %d: %d/%d want %d/%d", c.st, fr, gr, c.fr, c.gr)
		}
	}
	if tap.frames.Load() != 5 || tap.groups.Load() != 2 {
		t.Fatalf("network %d/%d", tap.frames.Load(), tap.groups.Load())
	}
	// Without a late path nothing is counted for late groups.
	t2 := newMLATTap(struct{ ingest.MLATSink }{next})
	t2.ObserveLate(g)
	if t2.frames.Load() != 0 {
		t.Fatal("late counted without a late observer")
	}
	if a := testing.AllocsPerRun(1000, func() { tap.Observe(g) }); a != 0 {
		t.Fatalf("%v allocs per group", a)
	}
}

// fakeMetrics is an mlatMetricsSource.
type fakeMetrics struct {
	reports map[model.StationIdx]mlat.StationReport
	clocks  []*clock.StationClock
	rej     uint64
	ref     ref.State
	refOK   bool
}

func (f *fakeMetrics) Station(st model.StationIdx) (mlat.StationReport, bool) {
	r, ok := f.reports[st]
	return r, ok
}
func (f *fakeMetrics) Stats() mlat.Stats                    { return mlat.Stats{Rejected: f.rej} }
func (f *fakeMetrics) ClockStations() []*clock.StationClock { return f.clocks }
func (f *fakeMetrics) RefState(_ uint32, tNs int64, st *ref.State) bool {
	*st = f.ref
	st.TNs = tNs
	return f.refOK
}

func TestMLATRecorderWritesRows(t *testing.T) {
	dir := t.TempDir()
	mainDB, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer mainDB.Close()
	stDB, err := store.OpenStations(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer stDB.Close()

	clk := time.Date(2026, 11, 14, 10, 0, 2, 0, time.UTC)
	now := func() time.Time { return clk }
	src := &fakeMetrics{reports: map[model.StationIdx]mlat.StationReport{
		2: {MLAT: &quality.StationReport{ContribPerHour: 12, UsedPct: 100}},
	}}
	sc := &clock.StationClock{Station: 2, Synced: true, SigmaThetaNs: 40, Phi: 2500, Beta: 12, Accepted30s: 30,
		Peers:  []clock.PeerStat{{Peer: 3, Count: 10, RMSNs: 30, SameDom: true}, {Peer: 4, Count: 30, RMSNs: 50, SameDom: true}},
		Events: []clock.EventRecord{{T: 5, Reason: "epoch"}, {T: 6, Reason: "step"}}}
	src.clocks = []*clock.StationClock{sc}
	tap := newMLATTap(&sinkCount{})
	for i := 0; i < 7; i++ {
		tap.Observe(&ingest.Group{Copies: []ingest.Copy{{Station: 2, Timed: true}, {Station: 3, Timed: true}}})
	}
	fixes := make(chan model.MLATFix, 16)
	regs := []registry.Station{{Idx: 2, SID: "AAAA"}, {Idx: 3, SID: "BBBB"}, {Idx: 4, SID: "CCCC"}}
	r := newMLATRecorder(src, tap, fixes, mainDB, stDB, func() []registry.Station { return regs }, now)

	// Six clock samples (two read Accepted30s), three published fixes, one
	// scored hold-out fix 300 m east of its reference.
	for s := 5; s < 60; s += 10 {
		r.sample(time.Date(2026, 11, 14, 10, 0, s, 0, time.UTC))
	}
	src.ref = ref.State{Lat: 51, Lon: 9, HAE: 10000 * 0.3048, Flags: ref.FlagConfirmed}
	src.ref.Pos = geo.ToECEF(51, 9, src.ref.HAE)
	src.refOK = true
	fr := geo.NewFrame(geo.LLH{Lat: 51, Lon: 9, H: src.ref.HAE})
	p := fr.FromENU(geo.Vec3{X: 300})
	for i := 0; i < 3; i++ {
		r.onFix(&model.MLATFix{ICAO: 0xabc, Grade: model.GradeA})
	}
	r.onFix(&model.MLATFix{ICAO: 0xabd, TUnixNs: clk.UnixNano(), ECEF: [3]float64{p.X, p.Y, p.Z}, Grade: model.GradeB, Flags: model.FixFlagHoldout})
	r.onFix(&model.MLATFix{ICAO: 0xabe, Grade: model.GradeD, Flags: model.FixFlagHoldout}) // grade D: not scored
	src.rej = 5                                                                            // first reading is the baseline
	clk = clk.Add(time.Minute)
	r.flush(clk.Unix()/60 - 1)
	if err := mainDB.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := stDB.Flush(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t0 := (clk.Unix()/60 - 1) * 60
	got, err := mainDB.MLATMinutes(ctx, "AAAA", t0, t0)
	if err != nil || len(got) != 1 {
		t.Fatalf("AAAA rows %v %v", got, err)
	}
	a := got[0]
	wantRMS := math.Sqrt((10*30*30 + 30*50*50) / 40.0)
	if a.Frames != 7 || a.Groups != 7 || a.SyncFrames != 60 || a.Fixes != 12 || a.Peers != 2 || a.SyncedSecs != 60 ||
		a.SigmaNs != 40 || math.Abs(a.DriftPPM-2.5) > 1e-9 || math.Abs(a.ResidRMSNs-wantRMS) > 1e-6 {
		t.Fatalf("AAAA %+v", a)
	}
	if b, _ := mainDB.MLATMinutes(ctx, "BBBB", t0, t0); len(b) != 1 || b[0].Frames != 7 || !math.IsNaN(b[0].SyncedSecs) {
		t.Fatalf("BBBB %+v", b)
	}
	if c, _ := mainDB.MLATMinutes(ctx, "CCCC", t0, t0); len(c) != 0 {
		t.Fatalf("idle station written: %+v", c)
	}
	n, _ := mainDB.MLATMinutes(ctx, "", t0, t0)
	if len(n) != 1 || n[0].Frames != 14 || n[0].Groups != 7 || n[0].Fixes != 3 || n[0].Peers != 1 || n[0].SyncFrames != 60 {
		t.Fatalf("network %+v", n)
	}
	q, err := mainDB.MLATQualityHours(ctx, t0-3600, t0+3600, -1)
	if err != nil || len(q) != 1 || q[0].N != 1 || q[0].AltBand != store.AltBand(10000) || q[0].Cell != store.Cell1Deg(51, 9) ||
		q[0].P50 < 250 || q[0].P50 > 360 {
		t.Fatalf("quality %+v %v", q, err)
	}
	ck, err := stDB.StationClockMinutes(ctx, "AAAA", t0, t0)
	if err != nil || len(ck) != 1 || ck[0].BetaShortNs != 12 || ck[0].EpochChanges != 1 || ck[0].Peers != 2 {
		t.Fatalf("clock %+v %v", ck, err)
	}

	// Next minute: the hourly sum grows to 20, the rejections by 4, the
	// same epoch event is not counted again.
	src.reports[2] = mlat.StationReport{MLAT: &quality.StationReport{ContribPerHour: 20, UsedPct: 80, ExcludedPct: 20, OutlierPct: 40}}
	src.rej = 9
	r.sample(clk.Add(5 * time.Second))
	clk = clk.Add(time.Minute)
	r.flush(clk.Unix()/60 - 1)
	_ = mainDB.Flush()
	_ = stDB.Flush()
	got, _ = mainDB.MLATMinutes(ctx, "AAAA", t0+60, t0+60)
	if len(got) != 1 || got[0].Fixes != 8 || got[0].Outliers != 10 || got[0].Frames != 0 || got[0].SyncedSecs != 10 {
		t.Fatalf("minute 2 %+v", got)
	}
	n, _ = mainDB.MLATMinutes(ctx, "", t0+60, t0+60)
	if len(n) != 1 || n[0].Outliers != 4 {
		t.Fatalf("network minute 2 %+v", n)
	}
	ck, _ = stDB.StationClockMinutes(ctx, "AAAA", t0+60, t0+60)
	if len(ck) != 1 || ck[0].EpochChanges != 0 {
		t.Fatalf("clock minute 2 %+v", ck)
	}
	// A station that left the registry is forgotten.
	regs = regs[1:]
	r.flush(clk.Unix() / 60)
	if _, ok := r.st[2]; ok {
		t.Fatal("removed station kept")
	}
}

// The retention pass of the aggregator prunes the MLAT tables.
func TestMLATTablesRetention(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := time.Now().Add(-400 * 24 * time.Hour).Unix()
	cur := time.Now().Unix()
	db.AddMLATMinutes([]store.MLATMinute{{T: old, SID: "X", Frames: 1}, {T: cur, SID: "X", Frames: 2}})
	h := make([]uint32, store.MLATErrBins)
	h[20] = 3
	db.MergeMLATQuality([]store.MLATQualityInc{{T: old, Cell: 100, Hist: h}, {T: cur, Cell: 100, Hist: h}})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	pol := store.TierPolicyFromConfig(model.DefaultAggregator())
	if _, err := db.Maintain(context.Background(), pol); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rows, _ := db.MLATMinutes(ctx, "X", 0, cur+60)
	q, _ := db.MLATQualityHours(ctx, 0, cur+3600, -1)
	if len(rows) != 1 || rows[0].Frames != 2 || len(q) != 1 {
		t.Fatalf("after retention: minutes %+v quality %d", rows, len(q))
	}
}
