package mlat

import (
	"math"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

func autoUseEngine(t *testing.T) *Engine {
	return testEngine(t, func(o *Options) {
		o.Config.AutoUseSurvey = true
		o.Config.AutoUseMaxSigmaM = 50
	})
}

// MLAT-R2: the owner moves the receiver 50 km and updates the declared
// position; the old auto-used survey must stop being used. A small
// correction of the declaration (inside the survey's own limits) keeps it.
func TestMovedStationDropsOldSurvey(t *testing.T) {
	e := autoUseEngine(t)
	p := geo.ToECEF(51, 9, 100)
	st := model.StationIdx(5)
	e.SetStation(st, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: p}})
	e.SetSurvey(st, Survey{Valid: true, ECEF: p, SigmaHM: 20, SigmaVM: 30, Stable: true})
	if src := e.PositionSource(st); src != PosSurvey {
		t.Fatalf("premise: source %v, want survey", src)
	}
	// a 40 m correction: within max(100 m, 3σ_h) — survey kept
	fr := geo.NewFrameECEF(p)
	near := fr.FromENU(geo.Vec3{X: 40})
	e.SetStation(st, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: near}})
	if src := e.PositionSource(st); src != PosSurvey {
		t.Errorf("a 40 m declaration correction dropped the survey: source %v", src)
	}
	moved := geo.ToECEF(51.45, 9, 100)
	e.SetStation(st, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: moved}})
	eff, _ := e.st.load().get(st)
	if e.PositionSource(st) != PosDeclared || eff.pos.Dist(moved) > 1 {
		t.Errorf("after a 50 km move: source %v, %.0f m from the new declared position", e.PositionSource(st), eff.pos.Dist(moved))
	}
	if sv, _ := e.SurveyOf(st); sv.Valid {
		t.Error("the stale survey is still stored")
	}
	// a new stable survey at the new place is used again
	e.SetSurvey(st, Survey{Valid: true, ECEF: moved, SigmaHM: 15, SigmaVM: 30, Stable: true})
	if src := e.PositionSource(st); src != PosSurvey {
		t.Errorf("new survey not used: source %v", src)
	}
}

// MLAT-R6: a survey result for a station removed in the meantime (or never
// registered) must not re-create it.
func TestSurveyDoesNotRecreateRemovedStation(t *testing.T) {
	e := autoUseEngine(t)
	st := model.StationIdx(9)
	p := geo.ToECEF(51, 9, 100)
	e.SetStation(st, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: p}})
	e.RemoveStation(st)
	e.SetSurvey(st, Survey{Valid: true, ECEF: p, SigmaHM: 20, SigmaVM: 30, Stable: true})
	if src := e.PositionSource(st); src != PosNone {
		t.Errorf("removed station re-created by a late survey: source %v", src)
	}
	if _, ok := e.SurveyOf(st); ok {
		t.Error("removed station has a stored survey")
	}
	e.SetSurvey(77, Survey{Valid: true, ECEF: p, SigmaHM: 20, Stable: true})
	if src := e.PositionSource(77); src != PosNone {
		t.Errorf("unknown station created by a survey: source %v", src)
	}
}

// MLAT-R3: an inconclusive (valid, unstable, non-suspect) re-check keeps a
// stable auto-used survey; a suspect result replaces it; a stable one
// replaces it too.
func TestUnstableRecheckKeepsStableSurvey(t *testing.T) {
	e := autoUseEngine(t)
	p := geo.ToECEF(51, 9, 100)
	far := geo.ToECEF(51.05, 9, 100) // declared 5.5 km off and suspect
	st := model.StationIdx(5)
	e.SetStation(st, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: far}, Suspect: true, DiscrepancyM: 5500})
	e.SetSurvey(st, Survey{Valid: true, ECEF: p, SigmaHM: 20, SigmaVM: 30, Stable: true})
	if src := e.PositionSource(st); src != PosSurvey {
		t.Fatalf("premise: source %v", src)
	}
	e.SetSurvey(st, Survey{Valid: true, ECEF: p, SigmaHM: 400, SigmaVM: 300, Stable: false})
	if src := e.PositionSource(st); src != PosSurvey {
		t.Errorf("an inconclusive re-check dropped the stable survey: source %v", src)
	}
	if sv, _ := e.SurveyOf(st); sv.SigmaHM != 20 {
		t.Errorf("stored survey σ_h %v, want the stable 20 m", sv.SigmaHM)
	}
	fr := geo.NewFrameECEF(p)
	p2 := fr.FromENU(geo.Vec3{Y: 10})
	e.SetSurvey(st, Survey{Valid: true, ECEF: p2, SigmaHM: 12, SigmaVM: 20, Stable: true})
	if eff, _ := e.st.load().get(st); eff.pos.Dist(p2) > 1e-6 {
		t.Error("a newer stable survey did not replace the old one")
	}
	e.SetSurvey(st, Survey{Valid: true, ECEF: p2, SigmaHM: 12, SigmaVM: 20, Stable: false, Suspect: true})
	if src := e.PositionSource(st); src == PosSurvey {
		t.Error("a suspect result did not demote the survey")
	}
}

// MLAT-R4: the clock filter gets the station's position σ, inflated for a
// suspect declared position.
func TestClockInfoCarriesPositionSigma(t *testing.T) {
	e := autoUseEngine(t)
	p := geo.ToECEF(51, 9, 100)
	e.SetStation(3, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: p}})
	ci, _ := e.st.clockInfoOf(3)
	if ci.PosSigmaM != 30 {
		t.Errorf("declared PosSigmaM %v, want 30", ci.PosSigmaM)
	}
	e.SetStation(3, StationInfo{Class: model.ClockSDR12, Declared: Position{Valid: true, ECEF: p}, Suspect: true, DiscrepancyM: 3000})
	ci, _ = e.st.clockInfoOf(3)
	if want := math.Hypot(30, 3000); math.Abs(ci.PosSigmaM-want) > 1e-9 {
		t.Errorf("suspect PosSigmaM %v, want %v", ci.PosSigmaM, want)
	}
}
