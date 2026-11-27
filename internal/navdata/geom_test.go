package navdata

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"airview/internal/geo"
)

func TestRegionAndApproach(t *testing.T) {
	nd, _ := imported(t)
	reg, err := nd.LoadRegion(context.Background(), 51.5, -0.3, 300, SizeSmall)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Len() != 4 || len(reg.Ends()) != 6 {
		t.Fatalf("region %d airports %d ends", reg.Len(), len(reg.Ends()))
	}
	var idx RunwayIndex = reg
	if a, ok := idx.AirportByCode("lhr"); !ok || a.Ident != "EGLL" {
		t.Fatal("AirportByCode")
	}
	a, d, ok := idx.NearestAirport(51.5074, -0.1278, SizeSmall, 100)
	if !ok || a.Ident != "EGLL" || d < 22 || d > 25 {
		t.Fatalf("nearest %s %f", a.Ident, d)
	}
	if _, _, ok := idx.NearestAirport(0, 0, SizeSmall, 100); ok {
		t.Fatal("nearest in the Atlantic")
	}
	ends := idx.RunwaysNear(51.47, -0.45, 10, nil)
	if len(ends) != 4 {
		t.Fatalf("runways near %d", len(ends))
	}

	// Aircraft on a 3 deg final for 27L (threshold 51.4649,-0.434115 displaced 1007 ft west, heading 269.6):
	// 10 km east of the threshold on the extended centreline, at threshold elev + tan(3)*10 km.
	thr := ends[3]
	if thr.Runway != "27L" {
		for _, e := range ends {
			if e.Runway == "27L" {
				thr = e
			}
		}
	}
	lat, lon := geo.Destination(thr.Lat, thr.Lon, thr.HeadingT+180, 10)
	alt := thr.ElevFt + geo.MetresToFeet(math.Tan(geo.Rad(3))*10000)
	fit, ok := MatchApproach(ends, lat, lon, alt, 269)
	if !ok || fit.End.Runway != "27L" || math.Abs(fit.DistKm-10) > 0.05 || math.Abs(fit.GlideDeg-3) > 0.05 ||
		math.Abs(fit.LateralKm) > 0.05 || math.Abs(fit.TrackDiffDeg+0.6) > 0.01 {
		t.Fatalf("fit %+v %v", fit, ok)
	}
	// 500 m right of the centreline (seen from the aircraft = north for a westbound approach).
	lat2, lon2 := geo.Destination(lat, lon, thr.HeadingT+90, 0.5)
	fit, ok = MatchApproach(ends, lat2, lon2, alt, 269.6)
	if !ok || fit.End.Runway != "27L" || math.Abs(fit.LateralKm-0.5) > 0.02 {
		t.Fatalf("lateral %+v", fit)
	}
	// Flying the wrong way, too high, or beyond the threshold: no match.
	if _, ok := MatchApproach(ends, lat, lon, alt, 89); ok {
		t.Fatal("opposite track matched")
	}
	if _, ok := MatchApproach(ends, lat, lon, 15000, 269); ok {
		t.Fatal("15000 ft matched")
	}
	if _, ok := MatchApproach(ends, 51.46, -0.6, 1000, 269); ok {
		t.Fatal("west of the airport matched 27L")
	}
	// Unknown altitude/track still matches geometrically.
	if _, ok := MatchApproach(ends, lat, lon, math.NaN(), math.NaN()); !ok {
		t.Fatal("NaN inputs")
	}
	if n := testing.AllocsPerRun(100, func() { MatchApproach(ends, lat, lon, alt, 269) }); n != 0 {
		t.Fatalf("MatchApproach allocates %v", n)
	}
	buf := make([]RunwayEndInfo, 0, 16)
	if n := testing.AllocsPerRun(100, func() { buf = reg.RunwaysNear(51.47, -0.45, 10, buf[:0]) }); n != 0 {
		t.Fatalf("RunwaysNear allocates %v", n)
	}

	// Arrival: last points descending onto 27L at Heathrow.
	var pts []TrackPoint
	for i := 0; i < 8; i++ {
		dist := 12 - float64(i)*1.5
		la, lo := geo.Destination(thr.Lat, thr.Lon, thr.HeadingT+180, dist)
		pts = append(pts, TrackPoint{T: int64(i) * 30000, Lat: la, Lon: lo, AltFt: 80 + dist*318, GS: 140, Track: 269.6})
	}
	g, ok := GuessArrival(pts, reg)
	if !ok || g.Airport.Ident != "EGLL" || g.Runway != "27L" || g.Confidence < 0.6 || !strings.HasPrefix(g.Reason, "arrival") {
		t.Fatalf("arrival %+v", g)
	}
	// Departure from Gatwick 26L.
	pts = pts[:0]
	for i := 0; i < 5; i++ {
		la, lo := geo.Destination(51.1449, -0.2095, 77.5, float64(i)*2)
		pts = append(pts, TrackPoint{Lat: la, Lon: lo, AltFt: 300 + float64(i)*900, Track: 77.5, GS: 170})
	}
	g, ok = GuessDeparture(pts, reg)
	if !ok || g.Airport.Ident != "EGKK" || g.Runway != "08R" {
		t.Fatalf("departure %+v", g)
	}
	// First seen at cruise level: no guess.
	if _, ok := GuessDeparture([]TrackPoint{{Lat: 51.5, Lon: -0.4, AltFt: 35000, Track: 90}}, reg); ok {
		t.Fatal("cruise guessed")
	}
	b, err := json.Marshal(g)
	if err != nil || strings.Contains(string(b), "NaN") {
		t.Fatalf("json %s %v", b, err)
	}
}

func TestOverlayAndGeometry(t *testing.T) {
	nd, _ := imported(t)
	a, _, _ := nd.AirportByCode(context.Background(), "EGLL", true)
	fc := AirportOverlay([]Airport{a}, OverlayOptions{})
	kinds := map[string]int{}
	for _, f := range fc.Features {
		kinds[f.Properties["kind"].(string)]++
	}
	if kinds["airport"] != 1 || kinds["runway"] != 2 || kinds["centerline"] != 4 {
		t.Fatalf("features %v", kinds)
	}
	ring := RunwayOutline(&a.Runways[0])
	if len(ring) != 5 || ring[0] != ring[4] {
		t.Fatalf("ring %v", ring)
	}
	// Width 164 ft = 50 m: corners 25 m either side of the low end.
	w := geo.HaversineKm(ring[0][1], ring[0][0], ring[3][1], ring[3][0]) * 1000
	if math.Abs(w-50) > 0.5 {
		t.Fatalf("width %f m", w)
	}
	if _, err := json.Marshal(fc); err != nil {
		t.Fatal(err)
	}

	// Great circle: endpoints exact, midpoint on the sphere midpoint.
	gc := GreatCircle(51.4706, -0.461941, 40.639447, -73.779317, 100)
	mla, mlo := geo.Midpoint(51.4706, -0.461941, 40.639447, -73.779317)
	mid := gc[len(gc)/2]
	if len(gc) < 50 || math.Abs(mid[1]-mla) > 0.5 || math.Abs(mid[0]-mlo) > 1 {
		t.Fatalf("gc %d mid %v want %f,%f", len(gc), mid, mlo, mla)
	}
	if len(GreatCircle(1, 1, 1, 1, 10)) != 2 {
		t.Fatal("degenerate gc")
	}
	pts := [][2]float64{{179, 0}, {-179, 0}, {-177, 0}}
	unwrapLon(pts)
	if pts[1][0] != 181 || pts[2][0] != 183 {
		t.Fatalf("unwrap %v", pts)
	}
	if h, ok := headingFromIdent("27L"); !ok || h != 270 {
		t.Fatal("ident 27L")
	}
	if h, ok := headingFromIdent("36"); !ok || h != 0 {
		t.Fatal("ident 36")
	}
	if _, ok := headingFromIdent("H1"); ok {
		t.Fatal("ident H1")
	}
	b := BoxAround(51.5, 179.5, 100)
	if !(b.W > b.E) || !b.Valid() {
		t.Fatalf("antimeridian box %+v", b)
	}
	if b := BoxAround(89.5, 0, 200); b.W != -180 || b.E != 180 || b.N != 90 {
		t.Fatalf("polar box %+v", b)
	}
}

func TestClassifyPhase(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		alt, agl, vr, gs float64
		gnd, app         bool
		want             Phase
	}{
		{0, nan, nan, 10, true, false, PhaseGround},
		{800, 700, 2000, 160, false, false, PhaseTakeoff},
		{12000, nan, 2500, 300, false, false, PhaseClimb},
		{37000, nan, 0, 450, false, false, PhaseCruise},
		{20000, nan, -1800, 400, false, false, PhaseDescent},
		{2000, 1900, -700, 150, false, false, PhaseApproach},
		{2500, 2400, -100, 150, false, true, PhaseApproach},
		{nan, nan, nan, nan, false, false, PhaseUnknown},
	}
	for i, c := range cases {
		if got := ClassifyPhase(c.alt, c.agl, c.vr, c.gs, c.gnd, c.app); got != c.want {
			t.Errorf("case %d: %s want %s", i, got, c.want)
		}
	}
	if s, ok := ParseSize("Medium"); !ok || s != SizeMedium || s.String() != "medium" {
		t.Fatal("ParseSize")
	}
}
