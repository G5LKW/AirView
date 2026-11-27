package mlat

import (
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

type notes []PositionChange

func (n *notes) PositionChanged(c PositionChange) { *n = append(*n, c) }

// TestSurveyOverrideAndNotifier: the phase-3 seam (SurveySink) and the
// owner notification of DESIGN §19.3. An auto-used survey is reported with
// AutoSurvey and its position; the owner's OverrideSurvey rejection takes
// the station back to its declared position, stays in force for later
// survey runs and can be lifted.
func TestSurveyOverrideAndNotifier(t *testing.T) {
	var got notes
	e := testEngine(t, func(o *Options) {
		o.Config.AutoUseSurvey = true
		o.Config.AutoUseMaxSigmaM = 50
		o.Notifier = &got
	})
	var sink SurveySink = e
	p := geo.ToECEF(51, 9, 100)
	q := geo.ToECEF(51.001, 9, 100)
	st := model.StationIdx(5)
	if sink.OverrideSurvey(st, true) {
		t.Fatal("override of an unknown station must report false")
	}
	e.SetStation(st, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: p}})
	sv := Survey{Valid: true, ECEF: q, SigmaHM: 20, SigmaVM: 30, Stable: true}
	sink.SetSurvey(st, sv)
	if len(got) != 2 || got[1].New != PosSurvey || !got[1].AutoSurvey || got[1].Old != PosDeclared ||
		got[1].ECEF != q || got[1].Reason != "survey" || !(got[1].SigmaM > 0) {
		t.Fatalf("auto-use notification: %+v", got)
	}
	if !sink.OverrideSurvey(st, true) || e.PositionSource(st) != PosDeclared {
		t.Fatalf("override: %v", e.PositionSource(st))
	}
	if c := got[len(got)-1]; c.Reason != "override" || c.New != PosDeclared || c.AutoSurvey {
		t.Fatalf("override notification %+v", c)
	}
	// a new survey run does not undo the owner's decision
	sink.SetSurvey(st, Survey{Valid: true, ECEF: q, SigmaHM: 10, SigmaVM: 20, Stable: true})
	if e.PositionSource(st) != PosDeclared {
		t.Fatal("survey used despite the owner's rejection")
	}
	if s, ok := e.SurveyOf(st); !ok || !s.OwnerRejected || s.SigmaHM != 10 {
		t.Fatalf("SurveyOf %+v %v", s, ok)
	}
	n := len(got)
	sink.OverrideSurvey(st, false)
	if e.PositionSource(st) != PosSurvey || len(got) != n+1 || !got[n].AutoSurvey {
		t.Fatalf("override lifted: %v %+v", e.PositionSource(st), got)
	}
	// no notification without a source change
	n = len(got)
	sink.SetSurvey(st, Survey{Valid: true, ECEF: q, SigmaHM: 12, SigmaVM: 20, Stable: true})
	if len(got) != n {
		t.Fatalf("notified without a change: %+v", got[n:])
	}
}
