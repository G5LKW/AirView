package engine

import (
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/registry"
)

func TestRestoredSigmaHSemiMajor(t *testing.T) {
	// MLAT-R5: an isotropic 40 m survey must restore as 40 m, not 56.6 m.
	if got := restoredSigmaH(&registry.Estimate{SigmaE: 40, SigmaN: 40}); got != 40 {
		t.Fatalf("sigmaH = %v, want 40", got)
	}
	if got := restoredSigmaH(&registry.Estimate{SigmaE: 10, SigmaN: 30}); got != 30 {
		t.Fatalf("sigmaH = %v, want 30", got)
	}
}

func TestPickRestoreSurvey(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := registry.Station{Declared: registry.Position{Lat: 50, Lon: 8, Valid: true}, LocStatus: model.LocDeclared}
	near := registry.Estimate{Method: "survey", Status: "stable", T: t0, Lat: 50.0001, Lon: 8, SigmaE: 20, SigmaN: 20}
	unstable := registry.Estimate{Method: "survey", Status: "unstable", T: t0.Add(6 * time.Hour), Lat: 50.0002, Lon: 8, SigmaE: 200, SigmaN: 200}
	// MLAT-R3: the later unstable re-check must not hide the stable one.
	if b := pickRestoreSurvey([]registry.Estimate{near, unstable}, st, 100); b == nil || b.Status != "stable" {
		t.Fatalf("got %+v, want the stable estimate", b)
	}
	// MLAT-R2: a survey 5 km from the current declared position is skipped.
	far := registry.Estimate{Method: "survey", Status: "stable", T: t0.Add(time.Hour), Lat: 50.045, Lon: 8, SigmaE: 20, SigmaN: 20}
	if b := pickRestoreSurvey([]registry.Estimate{near, far}, st, 100); b == nil || b.Lat != near.Lat {
		t.Fatalf("got %+v, want the near estimate", b)
	}
	if b := pickRestoreSurvey([]registry.Estimate{far}, st, 100); b != nil {
		t.Fatalf("got %+v, want none", b)
	}
	// A suspect declaration keeps the far survey (that is the point of it).
	st.LocStatus = model.LocSuspect
	if b := pickRestoreSurvey([]registry.Estimate{near, far}, st, 100); b == nil || b.Lat != far.Lat {
		t.Fatalf("got %+v, want the far estimate", b)
	}
	// Non-survey estimates are ignored.
	if b := pickRestoreSurvey([]registry.Estimate{{Method: "locate", Status: "stable", T: t0}}, st, 100); b != nil {
		t.Fatalf("got %+v, want none", b)
	}
}
