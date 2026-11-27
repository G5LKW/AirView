package server

import (
	"net/http/httptest"
	"testing"

	"airview/internal/model"
)

// SO-7: public trails are bounded: thinned to publicTrailMaxPoints with the
// first and last point kept, and filtered by ?bbox=.
func TestThinTrailAndBBox(t *testing.T) {
	pts := make([]model.TrailPoint, 1500)
	for i := range pts {
		pts[i] = model.TrailPoint{T: int64(i), Lat: 50, Lon: 5}
	}
	th := thinTrail(pts, publicTrailMaxPoints)
	if len(th) != publicTrailMaxPoints || th[0].T != 0 || th[len(th)-1].T != 1499 {
		t.Fatalf("thinned %d first %d last %d", len(th), th[0].T, th[len(th)-1].T)
	}
	if len(thinTrail(pts[:10], publicTrailMaxPoints)) != 10 {
		t.Fatal("short trail changed")
	}
	r := httptest.NewRequest("GET", "/api/public/trails?bbox=49,4,51,6", nil)
	b, err := parseTrailBox(r)
	if err != nil || !b.contains(50, 5) || b.contains(52, 5) || b.contains(50, 7) {
		t.Fatalf("bbox %+v %v", b, err)
	}
	am, _ := parseTrailBox(httptest.NewRequest("GET", "/x?bbox=-10,170,10,-170", nil))
	if !am.contains(0, 179) || !am.contains(0, -179) || am.contains(0, 0) {
		t.Fatal("antimeridian box")
	}
	for _, bad := range []string{"1,2,3", "a,b,c,d", "10,0,5,1", "0,0,95,1"} {
		if _, err := parseTrailBox(httptest.NewRequest("GET", "/x?bbox="+bad, nil)); err == nil {
			t.Errorf("bbox %q accepted", bad)
		}
	}
}

func TestPublicTrailsBounded(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.waitTraffic(3)
	tr := f.srv.publicTrails(240, nil)
	if tr.Minutes != publicTrailMaxMinutes {
		t.Fatalf("minutes %d", tr.Minutes)
	}
	for k, pts := range tr.Trails {
		if len(pts) > publicTrailMaxPoints {
			t.Fatalf("%s: %d points", k, len(pts))
		}
	}
	none := f.srv.publicTrails(10, &geoBox{s: -1, w: -1, n: 1, e: 1})
	if len(none.Trails) != 0 {
		t.Fatalf("bbox off the traffic returned %d trails", len(none.Trails))
	}
}
