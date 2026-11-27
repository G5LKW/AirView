package plan

import (
	"fmt"
	"slices"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

type acRec struct{ log []string }

func (r *acRec) ModeAC(st model.StationIdx, on bool) {
	r.log = append(r.log, fmt.Sprintf("%d:%v", st, on))
}

type fakeMLATC struct{ calls map[model.StationIdx]bool }

func (f *fakeMLATC) SetModeACAll(st model.StationIdx, on bool) bool { f.calls[st] = on; return true }

// ModeACOut hears only changes of the Mode A/C station set, in station
// order, and everything is switched off when Mode A/C MLAT is disabled.
func TestModeACOut(t *testing.T) {
	o := geo.ToECEF(52, 0, 0)
	f := geo.NewFrameECEF(o)
	sts := stMap{}
	for i, d := range [][2]float64{{0, 0}, {50e3, 0}, {0, 60e3}, {-40e3, -40e3}, {400e3, 0}} {
		sts[model.StationIdx(i+2)] = StationInfo{Pos: f.FromENU(geo.Vec3{X: d[0], Y: d[1]}), Located: true, Synced: true, Anchor: true}
	}
	rec := &acRec{}
	mc := &fakeMLATC{calls: map[model.StationIdx]bool{}}
	p := newPlanner(t, sts, func(o *Options) {
		o.Config.ModeAC.Enabled = true
		o.ModeACOut = ModeACFanout{rec, MLATCModeAC{C: mc}}
	})
	for st := range sts {
		p.NoteHearer(1, st, t0)
	}
	p.Replan(t0)
	if want := []string{"2:true", "3:true", "4:true", "5:true"}; !slices.Equal(rec.log, want) {
		t.Fatalf("first replan %v want %v", rec.log, want)
	}
	if !mc.calls[2] || len(mc.calls) != 4 {
		t.Fatalf("mlatc adapter %v", mc.calls)
	}
	if got := p.ModeACStations(); !slices.Equal(got, []model.StationIdx{2, 3, 4, 5}) {
		t.Fatalf("stations %v", got)
	}
	rec.log = nil
	p.Replan(t0 + 1e9)
	if len(rec.log) != 0 {
		t.Fatalf("unchanged set notified: %v", rec.log)
	}
	// station 3 loses its anchor status: it is switched off, and so are
	// the stations left with fewer than MinPeers anchor peers; nobody is
	// switched on
	s := sts[3]
	s.Anchor = false
	sts[3] = s
	p.Replan(t0 + 2e9)
	for _, l := range rec.log {
		if l[len(l)-4:] == "true" {
			t.Fatalf("station switched on: %v", rec.log)
		}
	}
	if !slices.Contains(rec.log, "3:false") {
		t.Fatalf("station 3 not switched off: %v", rec.log)
	}
	for _, st := range p.ModeACStations() {
		if st == 3 {
			t.Fatal("non-anchor still listed")
		}
	}
	// disabled: everything off
	p2 := newPlanner(t, sts, func(o *Options) { o.ModeACOut = rec })
	rec.log = nil
	p2.Replan(t0)
	if len(rec.log) != 0 || p2.ModeACStations() != nil {
		t.Fatalf("disabled planner notified %v", rec.log)
	}
	var fn ModeACFunc = func(model.StationIdx, bool) {}
	fn.ModeAC(2, true)
}
