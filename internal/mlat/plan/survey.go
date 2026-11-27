package plan

import (
	"cmp"
	"math"
	"slices"
	"sync"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Survey targets (DESIGN §10.2, internal/mlat/survey): a station under
// reverse-MLAT survey needs the frames of reference aircraft that it and
// its synced, located peers hear, spread over as many directions and
// distances as possible. Selective AVF feeders and mlat-clients send only
// what the planner asks for, so while a station is a survey target its
// syncSet is chosen for spread instead of for clock-sync coverage.

type surveySet struct {
	mu sync.Mutex
	m  map[model.StationIdx]struct{}
}

func (s *surveySet) set(st model.StationIdx, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		if s.m == nil {
			s.m = map[model.StationIdx]struct{}{}
		}
		s.m[st] = struct{}{}
	} else {
		delete(s.m, st)
	}
}

func (s *surveySet) has(st model.StationIdx) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[st]
	return ok
}

func (s *surveySet) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// SetSurveyTarget marks station st as a survey target (on) or ends that.
// While on, the station's syncSet is every usable reference aircraft it
// hears that a located peer hears too, chosen for spatial spread (greedy
// farthest point from the best-covered aircraft), bounded by SyncSetMax and
// half the station's frame budget. The change takes effect at the next
// Replan. Safe from any goroutine; never blocks on a Replan.
func (p *Planner) SetSurveyTarget(st model.StationIdx, on bool) { p.survey.set(st, on) }

// SurveyTargets returns the number of current survey targets.
func (p *Planner) SurveyTargets() int { return p.survey.count() }

// selectSurveySync appends survey target si's syncSet to w.chosen.
func (w *workspace) selectSurveySync(si int32, cfg *Config, pos Positions, fpsCap float64) (fps float64) {
	s := &w.st[si]
	type cand struct {
		ai   int32
		gain float64
		p    geo.Vec3
		ok   bool
		dmin float64
	}
	cs := make([]cand, 0, s.sc1-s.sc0)
	for _, ai := range w.scand[s.sc0:s.sc1] {
		g := w.syncGain(ai, si)
		if g <= 0 {
			continue // no located peer hears it: useless for the survey
		}
		c := cand{ai: ai, gain: g, dmin: math.Inf(1)}
		if pos != nil {
			c.p, _, c.ok = pos.Approx(w.ac[ai].key)
		}
		cs = append(cs, c)
	}
	// Best covered first; ties by key for reproducibility.
	slices.SortFunc(cs, func(a, b cand) int {
		if c := cmp.Compare(b.gain, a.gain); c != 0 {
			return c
		}
		return cmp.Compare(w.ac[a.ai].key, w.ac[b.ai].key)
	})
	n := 0
	for len(cs) > 0 && n < cfg.SyncSetMax {
		// Pick the candidate farthest from those chosen (positions known);
		// without positions, the best covered one.
		best := 0
		for i := range cs {
			if cs[i].ok && (!cs[best].ok || cs[i].dmin > cs[best].dmin) {
				best = i
			}
		}
		c := cs[best]
		cs = slices.Delete(cs, best, best+1)
		r := float64(w.ac[c.ai].rate)
		if fps+r > fpsCap && n > 0 {
			continue
		}
		fps += r
		n++
		w.chosen = append(w.chosen, w.ac[c.ai].key)
		if c.ok {
			for i := range cs {
				if cs[i].ok {
					cs[i].dmin = math.Min(cs[i].dmin, cs[i].p.Dist(c.p))
				}
			}
		}
	}
	return fps
}
