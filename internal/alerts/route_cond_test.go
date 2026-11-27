package alerts

import (
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

func routed(icao string, r model.RouteShort) model.Aircraft {
	return with(base(icao), func(a *model.Aircraft) { a.Route = &r })
}

func TestRouteConditions(t *testing.T) {
	r := model.RouteShort{From: "EGLL", To: "KJFK", FromIATA: "LHR", ToIATA: "JFK", State: "fits", Kind: "charter",
		Stops: []string{"EGLL", "EINN", "KJFK"}}
	a := routed("400001", r)
	none := base("400002")
	cases := []struct {
		c    Cond
		want bool
	}{
		{Cond{Type: CondRouteTo, Values: []string{"KJFK"}}, true},
		{Cond{Type: CondRouteTo, Values: []string{"jfk"}}, true},
		{Cond{Type: CondRouteTo, Values: []string{"EG*"}}, false},
		{Cond{Type: CondRouteFrom, Values: []string{"LHR"}}, true},
		{Cond{Type: CondRouteVia, Values: []string{"EINN"}}, true},
		{Cond{Type: CondRouteVia, Values: []string{"EGLL"}}, false}, // an end, not a stop
		{Cond{Type: CondRouteAirport, Values: []string{"EINN"}}, true},
		{Cond{Type: CondRouteAirport, Values: []string{"JFK"}}, true},
		{Cond{Type: CondRoute, Values: []string{"EGLL-KJFK"}}, true},
		{Cond{Type: CondRoute, Values: []string{"LHR-*"}}, true},
		{Cond{Type: CondRoute, Values: []string{"KJFK-EGLL"}}, false},
		{Cond{Type: CondRouteState, Values: []string{"Fits", "uncertain"}}, true},
		{Cond{Type: CondRouteState, Values: []string{"doubtful"}}, false},
		{Cond{Type: CondRouteKind, Values: []string{"charter"}}, true},
	}
	for _, c := range cases {
		if got := evalCond(t, c.c, a, nil, false); got != c.want {
			t.Errorf("%s %v: %v, want %v", c.c.Type, c.c.Values, got, c.want)
		}
		if evalCond(t, c.c, none, nil, false) {
			t.Errorf("%s %v matches an aircraft without a route", c.c.Type, c.c.Values)
		}
	}
	for _, bad := range []Cond{
		{Type: CondRoute, Values: []string{"EGLL"}},
		{Type: CondRoute, Values: []string{"A-B-C"}},
		{Type: CondRouteState, Values: []string{"maybe"}},
		{Type: CondRouteKind, Values: []string{"cargo"}},
		{Type: CondETAWithin},
		{Type: CondETAWithin, Max: fp(5000)},
		{Type: CondRouteTo},
	} {
		r := Rule{Name: "x", Match: bad}
		if err := r.Normalize(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestETAWithinAndACASRA(t *testing.T) {
	now := at(0)
	soon := routed("400001", model.RouteShort{From: "EGLL", To: "EDDF", ETA: now.Unix() + 10*60})
	late := routed("400002", model.RouteShort{From: "EGLL", To: "EDDF", ETA: now.Unix() + 40*60})
	noETA := routed("400003", model.RouteShort{From: "EGLL", To: "EDDF"})
	ra := with(base("400004"), func(a *model.Aircraft) { a.ACAS = &model.ACASMark{Advisory: "Climb", Sense: "up"} })
	rules := []Rule{
		{ID: "eta", Name: "Arrivals", Enabled: true, Match: Cond{Op: "and", Conds: []Cond{
			{Type: CondRouteTo, Values: []string{"EDDF"}}, {Type: CondETAWithin, Max: fp(20)}}},
			Message: "{callsign} {route} eta {eta} src {routeSource}"},
		{ID: "ra", Name: "RA", Enabled: true, Severity: SeverityWarning, Match: Cond{Type: CondACASRA}, Message: "RA {ra}"},
		{ID: "radown", Name: "RA down", Enabled: true, Match: Cond{Type: CondACASRA, Values: []string{"down"}}},
	}
	e := newTestEngine(t, Options{}, rules...)
	ch, cancel := e.Subscribe(10)
	defer cancel()
	e.Evaluate([]model.Aircraft{soon, late, noETA, ra}, now)
	var got []Alert
	timeout := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case al := <-ch:
			got = append(got, al)
		case <-timeout:
			t.Fatalf("alerts: %+v", got)
		}
	}
	select {
	case al := <-ch:
		t.Fatalf("unexpected alert %+v", al)
	case <-time.After(100 * time.Millisecond):
	}
	byRule := map[string]Alert{}
	for _, al := range got {
		byRule[al.RuleID] = al
	}
	if al := byRule["eta"]; al.ICAO != "400001" || !strings.Contains(al.Text, "EGLL-EDDF eta ") || !strings.Contains(al.Text, " UTC src ?") {
		t.Fatalf("eta alert %+v", al)
	}
	if al := byRule["ra"]; al.ICAO != "400004" || al.Text != "RA Climb" {
		t.Fatalf("ra alert %+v", al)
	}
	// Value false: aircraft without an RA
	if !evalCond(t, Cond{Type: CondACASRA, Value: bp(false)}, base("1"), nil, false) || evalCond(t, Cond{Type: CondACASRA, Value: bp(false)}, ra, nil, false) {
		t.Fatal("acasRA value=false")
	}
}
