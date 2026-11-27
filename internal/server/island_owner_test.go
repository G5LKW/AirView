package server

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/privacy"
)

// onGrid reports whether v lies on the island-guard grid.
func onGrid(v float64) bool {
	return math.Abs(v-privacy.RoundGrid(v)) < 1e-9
}

// PRIV-1: an owner without stations (anyone can self-register) must not see
// the exact position of an island aircraft on /api/aircraft,
// /api/me/aircraft or the owner live stream: only the public grid.
func TestOwnerIslandGuardCoarse(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.waitTraffic(3)
	f.srv.testGuard = func(a *model.Aircraft) bool { return true } // every aircraft is an island
	mallory := f.register(t, "mallory", "198.51.100.200:5000")

	check := func(name string, body []byte) {
		t.Helper()
		var snap struct {
			Aircraft []map[string]any `json:"aircraft"`
		}
		if err := json.Unmarshal(body, &snap); err != nil {
			t.Fatalf("%s: %v %.200s", name, err, body)
		}
		n := 0
		for _, a := range snap.Aircraft {
			for _, k := range []string{"lat", "lon", "lastRealLat", "lastRealLon"} {
				if v, ok := a[k].(float64); ok {
					n++
					if !onGrid(v) {
						t.Errorf("%s: %v %s=%v is exact", name, a["icao"], k, v)
					}
				}
			}
			for _, k := range []string{"distKm", "bearing", "elevation", "slantKm"} {
				if _, ok := a[k]; ok {
					t.Errorf("%s: %v has %s", name, a["icao"], k)
				}
			}
		}
		if n == 0 {
			t.Fatalf("%s: no positions to check", name)
		}
	}
	rec := f.as(mallory, "GET", "/api/aircraft", "")
	must(t, rec, 200, "/api/aircraft")
	check("/api/aircraft", rec.Body.Bytes())
	rec = f.as(mallory, "GET", "/api/me/aircraft", "")
	must(t, rec, 200, "/api/me/aircraft")
	check("/api/me/aircraft", rec.Body.Bytes())

	body := f.readSSE(t, "/api/events", mallory.addr, 1500*time.Millisecond, "Cookie", mallory.cookie)
	frames := 0
	for _, blk := range strings.Split(string(body), "\n\n") {
		if !strings.Contains(blk, "event: aircraft") {
			continue
		}
		for _, ln := range strings.Split(blk, "\n") {
			if d, ok := strings.CutPrefix(ln, "data: "); ok {
				frames++
				check("/api/events", []byte(d))
			}
		}
	}
	if frames == 0 {
		t.Fatalf("no aircraft frame on the owner stream: %.300s", body)
	}

	// The guard off: the owner projection is unchanged (exact positions).
	f.srv.testGuard = func(a *model.Aircraft) bool { return false }
	rec = f.as(mallory, "GET", "/api/aircraft", "")
	var snap struct {
		Aircraft []model.Aircraft `json:"aircraft"`
	}
	jsonOf(t, rec, &snap)
	exact := false
	for _, a := range snap.Aircraft {
		if a.Lat != nil && !onGrid(*a.Lat) {
			exact = true
		}
	}
	if !exact {
		t.Errorf("guard off: no exact position left")
	}
}

// Mine aircraft keep their exact position for their owner.
func TestGuardOwnerSnapshotKeepsMine(t *testing.T) {
	s := &Server{testGuard: func(*model.Aircraft) bool { return true }}
	lat, lon := 52.03412, 9.25721
	snap := model.LiveSnapshot{Aircraft: []model.Aircraft{
		{ICAO: "a24b38", Lat: &lat, Lon: &lon, Mine: true},
		{ICAO: "a24b39", Lat: &lat, Lon: &lon},
	}}
	s.guardOwnerSnapshot(&snap)
	if *snap.Aircraft[0].Lat != lat {
		t.Errorf("own aircraft coarsened")
	}
	if *snap.Aircraft[1].Lat != 52.05 || *snap.Aircraft[1].Lon != 9.25 {
		t.Errorf("foreign island aircraft: %v %v", *snap.Aircraft[1].Lat, *snap.Aircraft[1].Lon)
	}
	if lat != 52.03412 {
		t.Errorf("coarsening wrote through the shared pointer")
	}
}

// PRIV-A5 (was PRIV-3): the ground estimate never uses station antenna
// heights (the public coarse flag would leak them); with unknown terrain it
// is the conservative constant.
func TestIslandGroundFt(t *testing.T) {
	s := &Server{}
	if g := s.islandGroundFt(47, 11, false); g != privacy.IslandUnknownGroundFt {
		t.Errorf("unknown ground: %v", g)
	}
	if g := s.islandGroundFt(47, 11, true); g != privacy.IslandUnknownGroundFt {
		t.Errorf("no terrain model: %v", g)
	}
	// 2,500 ft baro over unknown ground (5,000 ft assumed) is low.
	alt := 2500.0
	if !privacy.IslandLowAGL(nil, &alt, nil, privacy.IslandUnknownGroundFt) {
		t.Errorf("2500 ft MSL over unknown terrain is not low")
	}
}

type fakeResolver map[model.StationIdx]model.StationGeo

func (f fakeResolver) Info(st model.StationIdx) (model.StationMeta, bool) {
	_, ok := f[st]
	return model.StationMeta{Idx: st, Pin: model.PinHidden}, ok
}

func (f fakeResolver) Geo(st model.StationIdx) (model.StationGeo, bool) {
	g, ok := f[st]
	return g, ok
}
