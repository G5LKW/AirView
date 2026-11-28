package routes

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/navdata"
)

// navWithAirlines is a fakeNav that also serves the airline list.
type navWithAirlines struct {
	*fakeNav
	airlines []navdata.Airline
}

func (n *navWithAirlines) Airlines(context.Context) ([]navdata.Airline, error) {
	return n.airlines, nil
}

func TestFlightKind(t *testing.T) {
	nav := &navWithAirlines{fakeNav: &fakeNav{standing: map[string][]string{
		"BAW9123": {"EGLL", "EGPH"}, "BAW4512": {"EGLL", "LIRF"}, "BAW117": {"EGLL", "KJFK"}, "EZY85TQ": {"EGPH", "LIRF"}}},
		airlines: []navdata.Airline{
			{Code: "BAW", ICAO: "BAW", Name: "British Airways", Positioning: `9\d\d\d`, Charter: `^(45|85|86)\d\d$`},
			{Code: "EZY", ICAO: "EZY", Name: "easyJet", Charter: `(?<=8)5`}, // .NET look-behind: RE2 cannot compile it
		}}
	ck := &clock{t: time.Unix(1_800_000_000, 0)}
	cfg := defCfg()
	s := New(Options{Store: openStore(t), Nav: nav, Config: func() model.RoutesConfig { return cfg }, Now: ck.now,
		OverridesFile: filepath.Join(t.TempDir(), "routes.overrides.csv")})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	s.loadAirlines(context.Background()) // Run loads them too; make sure before the checks
	for cs, want := range map[string]string{"BAW9123": KindPositioning, "BAW4512": KindCharter, "BAW117": "", "EZY85TQ": "", "DLH1": ""} {
		if got := s.kindOf(cs); got != want {
			t.Errorf("kindOf(%s) = %q, want %q", cs, got, want)
		}
	}
	a := aircraft("400001", "BAW9123", 53.0, -1.5, 30000, 340, 450, 0)
	r := live(t, s, a, ck.now)
	if r == nil || r.Kind != KindPositioning {
		t.Fatalf("live route %+v", r)
	}
	d, err := s.Detail(context.Background(), "BAW4512", DetailOptions{})
	if err != nil || d.Route == nil || d.Route.Kind != KindCharter {
		t.Fatalf("detail %+v %v", d, err)
	}
}
