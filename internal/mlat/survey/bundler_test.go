package survey

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/tracker"
)

// fakeRefs returns the generator's aircraft position for a frame.
type fakeRefs map[[2]int64]geo.Vec3

func (f fakeRefs) RefPosition(key uint32, _ uint8, t int64) (geo.Vec3, [6]float64, bool) {
	p, ok := f[[2]int64{int64(key), t}]
	return p, [6]float64{100, 0, 0, 100, 0, 100}, ok
}

type flagRec struct {
	got map[model.StationIdx]bool
}

func (f *flagRec) SetDeclaredSuspect(st model.StationIdx, s bool, _ float64) { f.got[st] = s }

type notifyRec struct{ n int }

func (n *notifyRec) BundleFinished(BundleResult) { n.n++ }

// groupsOf turns a generated bundle input into ingest groups (one per
// frame) and the reference positions behind them.
func groupsOf(in BundleInput) ([]ingest.Group, fakeRefs) {
	refs := fakeRefs{}
	var gs []ingest.Group
	for _, f := range in.Frames {
		g := ingest.Group{Kind: model.FrameModeSLong, Key: f.Key, DF: 17, HasResult: true,
			Result: tracker.Result{Status: tracker.ResultOK}, FirstNs: f.TNs}
		for _, r := range in.Recs[f.R0 : f.R0+f.NR] {
			s := in.Stations[r.St]
			g.Copies = append(g.Copies, ingest.Copy{Station: s.ID, ClockCls: s.Class, Epoch: r.Epoch, Ticks: r.Ticks, Timed: true})
		}
		refs[[2]int64{int64(f.Key), f.TNs}] = f.P
		gs = append(gs, g)
	}
	return gs, refs
}

func TestBundlerLive(t *testing.T) {
	n := bnetMixed(11)
	n.durS = 900
	in, tr := n.gen()
	gs, refs := groupsOf(in)
	now := time.Unix(0, bnetBaseNs)
	fl := &flagRec{got: map[model.StationIdx]bool{}}
	nt := &notifyRec{}
	bd, err := NewBundler(BundlerOptions{Refs: refs, Now: func() time.Time { return now }, Declared: fl, Notifier: nt,
		Undulation: func(float64, float64) (float64, bool) { return 0, true },
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)), Config: BundleConfig{ThinInterval: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bd.RunOnce(context.Background()); !errors.Is(err, ErrBundleNoSubjects) {
		t.Fatalf("empty run: %v", err)
	}
	for i, s := range in.Stations {
		m := BundleMember{Class: s.Class, Timestamps: true}
		switch s.Prior {
		case PriorAnchor:
			p := s.Pos
			m.Anchor = &p
		case PriorDeclared:
			l := geo.FromECEF(s.Pos)
			m.Declared = &Declared{Lat: l.Lat, Lon: l.Lon, Alt: l.H, AltRef: model.AltRefHAE}
		case PriorHint:
			l := geo.FromECEF(s.Pos)
			m.Hint = &l
		}
		if i == 4 {
			// no consent: never used
			m.Timestamps = false
		}
		bd.SetStation(s.ID, m)
	}
	for i := range gs {
		bd.Observe(&gs[i])
		now = time.Unix(0, gs[i].FirstNs)
	}
	st := bd.Stats()
	if st.Accepted == 0 || st.Subjects != 12 || st.Members != len(in.Stations) {
		t.Fatalf("stats %+v", st)
	}
	res, err := bd.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if nt.n != 1 || bd.Last() == nil || len(bd.Last().Stations) != len(res.Stations) {
		t.Errorf("notifier %d, last %v", nt.n, bd.Last() != nil)
	}
	for _, s := range res.Stations {
		if s.ID == in.Stations[4].ID {
			t.Errorf("station without consent in the bundle")
		}
		i := int(s.ID) - 2
		eh, _ := bnetErr(tr, i, s.ECEF)
		wrong := n.stations[i].prior == PriorDeclared && n.stations[i].errM >= 300
		if wrong != fl.got[s.ID] {
			t.Errorf("station %d: declared error %.0f m, flagged %v", i, n.stations[i].errM, fl.got[s.ID])
		}
		if eh > 150 {
			t.Errorf("station %d: error %.1f m", i, eh)
		}
	}
	// a corrected declaration that the next run confirms clears the flag
	for i, s := range in.Stations {
		if n.stations[i].errM >= 300 && s.Prior == PriorDeclared {
			l := tr.llh[i]
			bd.SetStation(s.ID, BundleMember{Class: s.Class, Timestamps: true, Declared: &Declared{Lat: l.Lat, Lon: l.Lon, Alt: l.H, AltRef: model.AltRefHAE}})
		}
	}
	if _, err := bd.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, f := range fl.got {
		if f {
			t.Errorf("station %d still flagged after a correct declaration", id)
		}
	}
	// consent withdrawn by everyone: nothing is collected any more
	for _, s := range in.Stations {
		bd.SetStation(s.ID, BundleMember{Class: s.Class})
	}
	acc := bd.Stats().Accepted
	bd.Observe(&gs[0])
	if bd.Stats().Accepted != acc {
		t.Error("frame kept without consent")
	}
	// old data is dropped after Span
	now = now.Add(2 * time.Hour)
	if _, err := bd.RunOnce(context.Background()); !errors.Is(err, ErrBundleNoSubjects) {
		t.Errorf("stale run: %v", err)
	}
	if bd.Stats().Frames != 0 {
		t.Errorf("frames kept after Span: %d", bd.Stats().Frames)
	}
}

func TestBundlerThinningAndMemory(t *testing.T) {
	refs := fakeRefs{}
	bd, err := NewBundler(BundlerOptions{Refs: refs, Config: BundleConfig{MaxFrames: 1000, ThinInterval: time.Second},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	p := geo.ToECEF(51, 9, 300)
	bd.SetStation(2, BundleMember{Class: model.ClockSDR12, Timestamps: true})
	bd.SetStation(3, BundleMember{Class: model.ClockGPSUTC, Timestamps: true, Anchor: &p})
	bd.SetStation(4, BundleMember{Class: model.ClockGPSUTC, Timestamps: true, Anchor: &p})
	mk := func(key uint32, t int64, sts ...model.StationIdx) *ingest.Group {
		g := &ingest.Group{Kind: model.FrameModeSLong, Key: key, DF: 17, HasResult: true, Result: tracker.Result{Status: tracker.ResultOK}, FirstNs: t}
		for _, s := range sts {
			g.Copies = append(g.Copies, ingest.Copy{Station: s, ClockCls: model.ClockSDR12, Ticks: uint64(t), Timed: true})
		}
		refs[[2]int64{int64(key), t}] = p
		return g
	}
	t0 := bnetBaseNs
	bd.Observe(mk(1, t0, 2, 3))
	bd.Observe(mk(1, t0+5e8, 2, 3)) // thinned
	bd.Observe(mk(1, t0+2e9, 3, 4)) // no subject
	g := mk(2, t0, 2, 3)
	g.Copies[1].MapOnly = true // Beast 30004: one usable copy only
	bd.Observe(g)
	g = mk(3, t0, 2, 3)
	g.DF = 20 // not a sync format
	bd.Observe(g)
	if s := bd.Stats(); s.Accepted != 1 || s.Thinned != 1 {
		t.Fatalf("stats %+v", s)
	}
	for i := range 5000 {
		bd.Observe(mk(uint32(100+i), t0+int64(i)*1e6, 2, 3, 4))
	}
	s := bd.Stats()
	if s.Frames > 1000 || s.Decimated == 0 {
		t.Errorf("memory bound: %+v", s)
	}
}

func TestBundlerRotation(t *testing.T) {
	now := time.Unix(0, bnetBaseNs)
	bd, err := NewBundler(BundlerOptions{Refs: fakeRefs{}, Config: BundleConfig{MaxStations: 4}, Now: func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	decl := &Declared{Lat: 51, Lon: 9, AltRef: model.AltRefHAE}
	for i := range 4 {
		bd.SetStation(model.StationIdx(10+i), BundleMember{Class: model.ClockSDR12, Timestamps: true, Declared: decl})
	}
	bd.SetStation(20, BundleMember{Class: model.ClockSDR12, Timestamps: true}) // unlocated
	focus := func() []model.StationIdx {
		bd.mu.Lock()
		defer bd.mu.Unlock()
		var f []model.StationIdx
		for st := range bd.focus {
			f = append(f, st)
		}
		return f
	}
	if f := focus(); len(f) != 2 {
		t.Fatalf("focus %v", f)
	}
	seen := map[model.StationIdx]int{}
	for range 4 {
		if _, err := bd.RunOnce(context.Background()); !errors.Is(err, ErrBundleNoSubjects) {
			t.Fatalf("run: %v", err)
		}
		f := focus()
		if len(f) != 2 || !bd.focus[20] {
			t.Fatalf("focus %v: the unlocated station must stay in it", f)
		}
		for _, st := range f {
			seen[st]++
		}
		// pretend the declared ones in focus were adjusted
		bd.mu.Lock()
		for _, st := range f {
			if st != 20 {
				bd.lastDone[st] = now
			}
		}
		bd.mu.Unlock()
		now = now.Add(time.Hour)
	}
	for i := range 4 {
		if seen[model.StationIdx(10+i)] == 0 {
			t.Errorf("declared station %d never in focus: %v", 10+i, seen)
		}
	}
}

func TestMemberOf(t *testing.T) {
	p := geo.ToECEF(51, 9, 300)
	decl := &Declared{Lat: 51, Lon: 9, AltRef: model.AltRefHAE}
	hint := &geo.LLH{Lat: 51, Lon: 9}
	// GNSS: the anchor is used and copied
	m := MemberOf(Station{Class: model.ClockGPSUTC, Timestamps: true, GNSS: true, Declared: decl}, &p, 4)
	if m.Anchor == nil || *m.Anchor != p || m.Anchor == &p || m.AnchorSigmaM != 4 || m.subject() {
		t.Errorf("gnss: %+v", m)
	}
	// declared only: the anchor argument is ignored, the declaration tested
	m = MemberOf(Station{Class: model.ClockSDR12, Timestamps: true, Declared: decl, Hint: hint}, &p, 4)
	if m.Anchor != nil || m.Declared != decl || m.Hint != hint || !m.subject() {
		t.Errorf("declared: %+v", m)
	}
	// no consent: never a subject
	m = MemberOf(Station{Class: model.ClockSDR12, Confirmed: true}, &p, 0)
	if m.subject() || m.Anchor == nil {
		t.Errorf("consent: %+v", m)
	}
}

func BenchmarkBundlerObserveNoSubject(b *testing.B) {
	bd, _ := NewBundler(BundlerOptions{Refs: fakeRefs{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	p := geo.ToECEF(51, 9, 300)
	for i := range 100 {
		bd.SetStation(model.StationIdx(i+2), BundleMember{Class: model.ClockGPSUTC, Timestamps: true, Anchor: &p})
	}
	bd.SetStation(500, BundleMember{Class: model.ClockSDR12, Timestamps: true})
	g := &ingest.Group{Kind: model.FrameModeSLong, Key: 1, DF: 17, HasResult: true, FirstNs: bnetBaseNs}
	for i := range 8 {
		g.Copies = append(g.Copies, ingest.Copy{Station: model.StationIdx(i + 2), ClockCls: model.ClockGPSUTC, Timed: true})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		bd.Observe(g)
	}
}
