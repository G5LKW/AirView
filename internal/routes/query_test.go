package routes

import (
	"context"
	"testing"
	"time"

	"airview/internal/model"
)

func TestBuildBoard(t *testing.T) {
	ap := refOf(StopOf(testAirports["EGLL"]))
	now := time.Unix(1_800_000_000, 0)
	ac := []model.Aircraft{
		// inbound, later ETA
		{ICAO: "400001", Callsign: "BAW2", Lat: f64(51), Lon: f64(-2), Route: &model.RouteShort{From: "KJFK", To: "EGLL", ToIATA: "LHR", Conf: 0.9, State: "fits", ETA: now.Unix() + 1800}},
		// inbound by IATA only, earlier ETA
		{ICAO: "400002", Callsign: "BAW1", Lat: f64(51.2), Lon: f64(-1), Route: &model.RouteShort{From: "EDDF", To: "LHR", Conf: 0.8, State: "fits", ETA: now.Unix() + 600}},
		// inbound but too unsure
		{ICAO: "400003", Callsign: "BAW3", Lat: f64(51), Lon: f64(-1), Route: &model.RouteShort{From: "EDDF", To: "EGLL", Conf: 0.3, State: "uncertain"}},
		// inbound but reversed
		{ICAO: "400004", Callsign: "BAW4", Lat: f64(51), Lon: f64(-1), Route: &model.RouteShort{From: "EDDF", To: "EGLL", Conf: 0.9, State: "reversed"}},
		// departing, climbing, near
		{ICAO: "400005", Callsign: "BAW5", Lat: f64(51.6), Lon: f64(-0.8), VertRate: f64(2000), OnGround: bp(false), Route: &model.RouteShort{From: "EGLL", To: "KJFK", Conf: 0.9, State: "fits"}},
		// from EGLL but level in cruise far away: not a departure
		{ICAO: "400006", Callsign: "BAW6", Lat: f64(55), Lon: f64(-20), VertRate: f64(0), Route: &model.RouteShort{From: "EGLL", To: "KJFK", Conf: 0.9, State: "fits"}},
		// no route
		{ICAO: "400007", Callsign: "XYZ1", Lat: f64(51), Lon: f64(-1)},
	}
	landed := []Leg{
		{Callsign: "EZY1", Dep: "EGPH", Arr: "EGLL", End: now.Unix() - 600, ArrConf: 0.9},
		{Callsign: "EZY2", Dep: "EGPH", Arr: "EGLL", End: now.Unix() - 7200, ArrConf: 0.9}, // too old
		{Callsign: "EZY3", Dep: "EGPH", Arr: "EGLL", End: now.Unix() - 60, ArrConf: 0.4},   // unsure
		{Callsign: "EZY4", Dep: "EGPH", Arr: "EGLL", End: now.Unix() - 120, ArrConf: 0.8},
	}
	b := BuildBoard(ap, ac, landed, now)
	if len(b.Arrivals) != 2 || b.Arrivals[0].Callsign != "BAW1" || b.Arrivals[1].Callsign != "BAW2" {
		t.Fatalf("arrivals %+v", b.Arrivals)
	}
	if b.Arrivals[0].DistKm <= 0 {
		t.Fatalf("distance %+v", b.Arrivals[0])
	}
	if len(b.Departures) != 1 || b.Departures[0].Callsign != "BAW5" {
		t.Fatalf("departures %+v", b.Departures)
	}
	if len(b.Landed) != 2 || b.Landed[0].Callsign != "EZY4" || b.Landed[1].Callsign != "EZY1" {
		t.Fatalf("landed %+v", b.Landed)
	}
	if b.Note == "" || b.Airport.IATA != "LHR" {
		t.Fatalf("board %+v", b)
	}
	// empty input: empty lists, never null
	e := BuildBoard(ap, nil, nil, now)
	if e.Arrivals == nil || e.Departures == nil || e.Landed == nil {
		t.Fatal("nil lists")
	}
}

func TestAirportsAndLegQueries(t *testing.T) {
	st := openStore(t)
	s, _ := newSvc(t, &fakeNav{}, st, defCfg())
	ctx := context.Background()

	refs, err := s.Airports(ctx, []string{"lhr", "EGLL", "JFK", "NOPE", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Code != "EGLL" || refs[1].Code != "KJFK" || refs[0].ElevFt == nil || *refs[0].ElevFt != 83 {
		t.Fatalf("airports %+v", refs)
	}
	if _, ok, _ := s.Airport(ctx, "ZZZZ"); ok {
		t.Fatal("unknown airport resolved")
	}

	t0 := int64(1_800_000_000)
	legs := []Leg{
		{Flight: 1, Callsign: "BAW1", Start: t0, End: t0 + 3600, Dep: "EGLL", DepConf: 0.9, DepTier: "T1", Arr: "KJFK", ArrConf: 0.9, ArrTier: "L1", Route: "EGLL-KJFK", RouteSrc: "standing-data"},
		{Flight: 2, Callsign: "BAW1", Start: t0 + 86400, End: t0 + 90000, Dep: "EGLL", DepConf: 0.9, DepTier: "T2", Arr: "KJFK", ArrConf: 0.9, ArrTier: "L2", Route: "EGLL-KJFK", RouteSrc: "learned"},
		{Flight: 3, Callsign: "EZY9", Start: t0 + 100, End: t0 + 3000, Dep: "EGPH", DepConf: 0.9, DepTier: "T1", Arr: "EGLL", ArrConf: 0.3, ArrTier: "L5"},
	}
	for _, l := range legs {
		if err := s.CloseLeg(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Legs(ctx, LegQuery{Airline: "baw"})
	if err != nil || len(got) != 2 || got[0].Flight != 2 {
		t.Fatalf("legs by airline %+v %v", got, err)
	}
	if got, _ := s.Legs(ctx, LegQuery{Dep: "EGPH"}); len(got) != 1 || got[0].Callsign != "EZY9" {
		t.Fatalf("legs by dep %+v", got)
	}
	if got, _ := s.Legs(ctx, LegQuery{From: t0 + 50, To: t0 + 200}); len(got) != 1 {
		t.Fatalf("legs by time %+v", got)
	}

	rows, err := s.LegStats(ctx, StatRoutes, 0, 0, 10)
	if err != nil || len(rows) != 1 || rows[0].Key != "EGLL-KJFK" || rows[0].N != 2 {
		t.Fatalf("routes %+v %v", rows, err)
	}
	rows, _ = s.LegStats(ctx, StatAirports, 0, 0, 10)
	if len(rows) != 3 || rows[0].Key != "EGLL" || rows[0].Dep != 2 || rows[0].Arr != 0 {
		t.Fatalf("airports %+v", rows) // EZY9's arrival is below 0.5 and does not count
	}
	rows, _ = s.LegStats(ctx, StatAirlines, 0, 0, 10)
	if len(rows) != 2 || rows[0].Key != "BAW" || rows[0].N != 2 {
		t.Fatalf("airlines %+v", rows)
	}
	rows, _ = s.LegStats(ctx, StatCoverage, 0, 0, 10)
	byKey := map[string]StatRow{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	if byKey["total"].N != 3 || byKey["both-ends-observed"].N != 2 || byKey["none"].N != 1 || byKey["learned"].N != 1 {
		t.Fatalf("coverage %+v", rows)
	}
	if _, err := s.LegStats(ctx, "bogus", 0, 0, 10); err == nil {
		t.Fatal("unknown kind accepted")
	}
	landed, err := s.LandedAt(ctx, []string{"KJFK"}, t0+86400)
	if err != nil || len(landed) != 1 || landed[0].Flight != 2 {
		t.Fatalf("landed %+v %v", landed, err)
	}
}
