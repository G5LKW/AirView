package trust

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/ingest"
	"airview/internal/model"
)

type rec struct {
	groups []ingest.Group
	late   []ingest.Group
}

func (r *rec) Observe(g *ingest.Group)     { r.groups = append(r.groups, *g.Clone()) }
func (r *rec) ObserveLate(g *ingest.Group) { r.late = append(r.late, *g.Clone()) }

type src map[model.StationIdx]StationInfo

func (s src) TrustInfo(st model.StationIdx) (StationInfo, bool) {
	in, ok := s[st]
	return in, ok
}

func copyOf(st model.StationIdx) ingest.Copy {
	return ingest.Copy{Station: st, ClockCls: model.ClockHW12, Ticks: 1000, Timed: true}
}

func TestGateFiltersUntrustedCopies(t *testing.T) {
	s := src{2: {State: Trusted}, 3: {State: Trusted}, 4: {State: Quarantined}, 5: {State: Excluded}}
	r := &rec{}
	g := New(r, s, Config{}, nil)
	grp := &ingest.Group{FirstNs: 1e18, Data: []byte{0x5d, 1, 2, 3, 4, 5, 6},
		Copies: []ingest.Copy{copyOf(2), copyOf(4), copyOf(3), copyOf(5)}, Timed: 4}
	g.Observe(grp)
	if len(r.groups) != 1 || len(r.groups[0].Copies) != 2 || r.groups[0].Timed != 2 {
		t.Fatalf("forwarded %+v", r.groups)
	}
	for _, c := range r.groups[0].Copies {
		if c.Station != 2 && c.Station != 3 {
			t.Fatalf("untrusted copy forwarded: %d", c.Station)
		}
	}
	if len(grp.Copies) != 4 {
		t.Fatal("the input group was modified")
	}
	// only untrusted copies: nothing forwarded
	g.Observe(&ingest.Group{FirstNs: 1e18, Copies: []ingest.Copy{copyOf(4), copyOf(5)}})
	if len(r.groups) != 1 || g.Stats().Dropped != 1 {
		t.Fatalf("groups %d stats %+v", len(r.groups), g.Stats())
	}
	// late copies are filtered the same way
	g.ObserveLate(&ingest.Group{FirstNs: 1e18, Copies: []ingest.Copy{copyOf(4), copyOf(3)}})
	if len(r.late) != 1 || len(r.late[0].Copies) != 1 || r.late[0].Copies[0].Station != 3 {
		t.Fatalf("late %+v", r.late)
	}
	// unknown stations pass (the gate never drops what it cannot place)
	g.Observe(&ingest.Group{FirstNs: 1e18, Copies: []ingest.Copy{copyOf(9), copyOf(2)}})
	if len(r.groups) != 2 || len(r.groups[1].Copies) != 2 {
		t.Fatalf("unknown station dropped: %+v", r.groups[1])
	}
	if !g.Quarantined(4) || !g.Quarantined(5) || g.Quarantined(2) || g.Quarantined(9) {
		t.Fatal("Quarantined")
	}
	// disabled: everything passes
	r2 := &rec{}
	g2 := New(r2, s, Config{Disabled: true}, nil)
	g2.Observe(grp)
	if len(r2.groups[0].Copies) != 4 || g2.Quarantined(4) {
		t.Fatal("disabled gate filtered")
	}
}

func TestGateRefreshAndProvenGrace(t *testing.T) {
	s := src{2: {State: Trusted}, 4: {State: Quarantined}}
	r := &rec{}
	var promoted []model.StationIdx
	g := New(r, s, Config{}, func(st model.StationIdx, _ Report) { promoted = append(promoted, st) })
	t0 := int64(1e18)
	g.Observe(&ingest.Group{FirstNs: t0, Copies: []ingest.Copy{copyOf(2), copyOf(4)}})
	// the admin trusts station 4: picked up at the next refresh
	s[4] = StationInfo{State: Trusted}
	g.Observe(&ingest.Group{FirstNs: t0 + 1e9, Copies: []ingest.Copy{copyOf(2), copyOf(4)}})
	if len(r.groups[1].Copies) != 1 {
		t.Fatal("refresh before RefreshEvery")
	}
	g.Observe(&ingest.Group{FirstNs: t0 + 11e9, Copies: []ingest.Copy{copyOf(2), copyOf(4)}})
	if len(r.groups[2].Copies) != 2 {
		t.Fatal("admin override not picked up after RefreshEvery")
	}
	// Refresh applies a change at once
	s[4] = StationInfo{State: Excluded}
	g.Refresh()
	g.Observe(&ingest.Group{FirstNs: t0 + 12e9, Copies: []ingest.Copy{copyOf(2), copyOf(4)}})
	if len(r.groups[3].Copies) != 1 {
		t.Fatal("Refresh did not apply the exclusion")
	}
	// a station the gate proved stays trusted while the source catches up
	e := g.ent[4]
	e.provenAt = t0 + 12e9
	s[4] = StationInfo{State: Quarantined}
	g.Refresh()
	g.Observe(&ingest.Group{FirstNs: t0 + 13e9, Copies: []ingest.Copy{copyOf(2), copyOf(4)}})
	if len(r.groups[4].Copies) != 2 {
		t.Fatal("proven station re-quarantined inside the grace")
	}
	g.Refresh()
	g.Observe(&ingest.Group{FirstNs: t0 + 13e9 + 3*60e9, Copies: []ingest.Copy{copyOf(2), copyOf(4)}})
	if len(r.groups[5].Copies) != 1 {
		t.Fatal("source's quarantine ignored after the grace (admin re-quarantine)")
	}
}

func TestJudgeWindowRobust(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	mk := func(sigma float64, outliers float64) ([]float64, []float64) {
		var x, y []float64
		for i := 0; i < 64; i++ {
			xi := float64(i) * 0.4
			yi := 123456 + 37e3*xi + rng.NormFloat64()*sigma // 37 ppm relative rate
			if rng.Float64() < outliers {
				yi += (rng.Float64()*2 - 1) * 500e3
			}
			x, y = append(x, xi), append(y, yi)
		}
		return x, y
	}
	x, y := mk(300, 0.12)
	if sig, good := judgeWindow(x, y, 1500); !good || sig > 500 {
		t.Fatalf("honest SDR pair with 12 %% gross errors: σ %.0f good %v", sig, good)
	}
	x, y = mk(29e3, 0) // ±50 µs uniform-ish
	if sig, good := judgeWindow(x, y, 1500); good || sig < 10e3 {
		t.Fatalf("random timing judged good: σ %.0f", sig)
	}
	x, y = mk(2500, 0)
	if _, good := judgeWindow(x, y, 1500); good {
		t.Fatal("2.5 µs noise judged good")
	}
	if s := median([]float64{3, 1, 2}); s != 2 {
		t.Fatal("median")
	}
	if !math.IsInf(mad(nil, nil), 1) {
		t.Fatal("mad of nothing")
	}
}

// BenchmarkGateTrustedGroup is the hot path: a group without untrusted
// copies (the normal case once a network has settled).
func BenchmarkGateTrustedGroup(b *testing.B) {
	s := src{}
	for i := model.StationIdx(2); i < 2000; i++ {
		s[i] = StationInfo{State: Trusted}
	}
	g := New(nopSink{}, s, Config{}, nil)
	grp := &ingest.Group{FirstNs: 1e18, Copies: []ingest.Copy{copyOf(2), copyOf(300), copyOf(1999), copyOf(55), copyOf(77)}}
	g.Observe(grp)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Observe(grp)
	}
}

// BenchmarkGateFilteredGroup: a group with one quarantined copy (not a
// position message, so no judging).
func BenchmarkGateFilteredGroup(b *testing.B) {
	s := src{2: {State: Trusted}, 3: {State: Trusted}, 4: {State: Quarantined}, 5: {State: Trusted}}
	g := New(nopSink{}, s, Config{}, nil)
	grp := &ingest.Group{FirstNs: 1e18, Data: []byte{0x5d, 1, 2, 3, 4, 5, 6},
		Copies: []ingest.Copy{copyOf(2), copyOf(3), copyOf(4), copyOf(5)}}
	g.Observe(grp)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Observe(grp)
	}
}

type nopSink struct{}

func (nopSink) Observe(*ingest.Group) {}
