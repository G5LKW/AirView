package plan

import (
	"testing"

	"airview/internal/mlat/geo"
)

// TestSurveyTargetSyncSet: a survey target's syncSet is filled up to
// SyncSetMax with reference aircraft spread over a larger area than the
// clock-sync choice, and switching it off restores the normal choice.
func TestSurveyTargetSyncSet(t *testing.T) {
	w := newNetWorld(t, 5, 40, 400, 250)
	pos := posMap{}
	for _, a := range w.ac {
		pos[a.ICAO] = struct {
			p geo.Vec3
			s float64
		}{w.apos(a), 50}
	}
	spread := func(p *Planner, keys []uint32) float64 {
		var sum float64
		var n int
		for i := range keys {
			for j := i + 1; j < len(keys); j++ {
				sum += pos[keys[i]].p.Dist(pos[keys[j]].p)
				n++
			}
		}
		if n == 0 {
			return 0
		}
		return sum / float64(n)
	}
	run := func(target bool) ([]uint32, Summary) {
		p := newPlanner(t, w.sts, func(o *Options) { o.Positions = pos })
		st := w.ids[0]
		p.SetSurveyTarget(st, target)
		now := t0 + 2e9
		for round := 0; round < 3; round++ {
			w.feed(p, now, 100) // 99 % have ADS-B: sync candidates
			p.Replan(now)
			now += 5e9
		}
		ss, _ := p.Sets(st)
		return ss, p.Summary()
	}
	base, s0 := run(false)
	surv, s1 := run(true)
	cfg := DefaultConfig().Normalize()
	if s0.SurveyTargets != 0 || s1.SurveyTargets != 1 {
		t.Fatalf("summary targets %d / %d", s0.SurveyTargets, s1.SurveyTargets)
	}
	if len(surv) < len(base) || len(surv) > cfg.SyncSetMax {
		t.Fatalf("survey sync set %d (normal %d, max %d)", len(surv), len(base), cfg.SyncSetMax)
	}
	sb, ss := spread(nil, base), spread(nil, surv)
	t.Logf("sync set: normal %d aircraft, mean separation %.0f km; survey %d, %.0f km", len(base), sb/1e3, len(surv), ss/1e3)
	if ss <= sb {
		t.Fatalf("survey set not more spread out: %.0f <= %.0f m", ss, sb)
	}
	p := newPlanner(t, w.sts, nil)
	p.SetSurveyTarget(w.ids[0], true)
	p.SetSurveyTarget(w.ids[0], false)
	if p.SurveyTargets() != 0 {
		t.Fatal("target not removed")
	}
}
