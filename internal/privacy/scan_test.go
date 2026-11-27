package privacy

import (
	"math"
	"strings"
	"testing"

	"airview/internal/model"
)

func kinds(ls []Leak) string {
	var s []string
	for _, l := range ls {
		s = append(s, l.Kind+":"+l.Detail)
	}
	return strings.Join(s, " | ")
}

func TestScanCoordinates(t *testing.T) {
	f := &Fixture{Points: []Point{
		{Lat: -41.28713, Lon: 174.77621, Strict: true, Label: "strict"},
		{Lat: 50.03312, Lon: 8.57046, Label: "pair"},
	}}
	cases := []struct {
		body string
		leak bool
	}{
		{`{"lat":-41.287}`, true},            // strict: one coordinate is enough
		{`{"x":174.7762}`, true},             // strict longitude
		{`{"lat":-41.29}`, false},            // 2 decimals cannot locate
		{`{"lat":-412871300}`, true},         // 1e7 integer form
		{`{"lat":50.033,"y":1}`, false},      // pair point: latitude alone
		{`{"lat":50.033,"lon":8.570}`, true}, // pair point: both
		{`{"lat":50.0331,"other":"` + strings.Repeat("x", 300) + `","lon":8.5704}`, false}, // too far apart
		{`{"lat":50.1,"lon":8.6}`, false},
		{`{"id":"a50.033b8.570"}`, false}, // digits inside an identifier
	}
	for _, c := range cases {
		got := f.Scan([]byte(c.body))
		has := false
		for _, l := range got {
			if l.Kind == "coordinate" {
				has = true
			}
		}
		if has != c.leak {
			t.Errorf("%s: leak=%v, want %v (%s)", c.body, has, c.leak, kinds(got))
		}
	}
}

func TestScanOther(t *testing.T) {
	f := &Fixture{
		IPs:     []string{"2001:db8:aa:bb::17", "203.0.113.5"},
		SIDs:    []string{"k3x9qa7m2pz4tb6wy8cd"},
		Secrets: []string{"0123456789ABCDEF0123456789abcdef"},
	}
	cases := map[string]string{
		`{"a":"10.1.2.3"}`:                             "ip",
		`{"a":"2001:db8:aa:bb:1:2:3:4"}`:               "ip", // same /64
		`{"a":"2001:DB8:AA:BB::17"}`:                   "ip",
		`{"s":"k3x9qa7m2pz4tb6wy8cd"}`:                 "sid",
		`{"u":"0123456789abcdef0123456789ABCDEF"}`:     "secret",
		`{"t":1731500000123456789}`:                    "ns-timestamp",
		`{"distKm":12}`:                                "key",
		"event: aircraft\ndata: {\"feeds\":[\"x\"]}\n": "key",
	}
	for body, kind := range cases {
		got := f.Scan([]byte(body))
		found := false
		for _, l := range got {
			if l.Kind == kind {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want a %s leak, got %s", body, kind, kinds(got))
		}
	}
	clean := `{"now":1731500000123,"version":"1.2","lat":50.12345,"t":"12:30:45","msgRate":12.5,"ver":"v1.2.3"}`
	if got := f.Scan([]byte(clean)); len(got) != 0 {
		t.Errorf("clean body: %s", kinds(got))
	}
	// AllowKeys and NoKeyCheck.
	if got := (&Fixture{AllowKeys: []string{"distKm"}}).Scan([]byte(`{"distKm":1}`)); len(got) != 0 {
		t.Errorf("allowed key reported: %s", kinds(got))
	}
	if got := (&Fixture{NoKeyCheck: true}).Scan([]byte(`{"rssi":1}`)); len(got) != 0 {
		t.Errorf("NoKeyCheck: %s", kinds(got))
	}
}

func fp(v float64) *float64 { return &v }

func TestStripAircraft(t *testing.T) {
	b := true
	a := model.Aircraft{ICAO: "3C6444", Lat: fp(50), Lon: fp(8), DistKm: fp(1), Bearing: fp(2), Elevation: fp(3),
		SlantKm: fp(4), CPAKm: fp(5), CPAInS: fp(6), TerrainVisible: &b, LOSMarginDeg: fp(7), RSSI: fp(-3),
		RSSIAvg: fp(-4), Feeds: []string{"st/x/0"}, Antennas: []string{"yagi"}, Mine: true, Stations: []string{"sid"}}
	c := a
	StripAircraft(&c, true)
	if c.DistKm != nil || c.Bearing != nil || c.Elevation != nil || c.SlantKm != nil || c.CPAKm != nil || c.CPAInS != nil ||
		c.TerrainVisible != nil || c.LOSMarginDeg != nil || c.RSSI != nil || c.RSSIAvg != nil || c.Feeds != nil || c.Antennas != nil {
		t.Errorf("not stripped: %+v", c)
	}
	if !c.Mine || len(c.Stations) != 1 || c.Lat == nil {
		t.Errorf("keepOwn lost own fields: %+v", c)
	}
	StripAircraft(&c, false)
	if c.Mine || c.Stations != nil {
		t.Errorf("own fields kept: %+v", c)
	}
	s := StripSnapshot(model.LiveSnapshot{Aircraft: []model.Aircraft{a}, Counts: model.LiveCounts{MaxRangeKm: 300}}, false)
	if s.Counts.MaxRangeKm != 0 || s.Aircraft[0].DistKm != nil || a.DistKm == nil {
		t.Errorf("StripSnapshot: %+v (source changed: %v)", s, a.DistKm == nil)
	}
}

type fakeRes map[model.StationIdx]string

func (f fakeRes) Info(i model.StationIdx) (model.StationMeta, bool) {
	p, ok := f[i]
	return model.StationMeta{Idx: i, Pin: p}, ok
}
func (f fakeRes) Geo(model.StationIdx) (model.StationGeo, bool) { return model.StationGeo{}, false }

func TestIslandGuardHelpers(t *testing.T) {
	g, air := true, false
	if !IslandLow(&g, fp(5000), nil) || IslandLow(&air, fp(5000), nil) || !IslandLow(nil, fp(900), nil) ||
		IslandLow(nil, nil, fp(1500)) || !IslandLow(nil, nil, nil) {
		t.Error("IslandLow")
	}
	res := fakeRes{1: model.PinHidden, 2: model.PinRegion, 3: ""}
	if !OnlyHidden([]model.StationIdx{1, 3, 9}, res) || OnlyHidden([]model.StationIdx{1, 2}, res) || OnlyHidden(nil, res) {
		t.Error("OnlyHidden")
	}
	if v := RoundGrid(50.0331); v != 50.05 {
		t.Errorf("RoundGrid 50.0331 = %v", v)
	}
	if v := RoundGrid(8.524); v != 8.5 {
		t.Errorf("RoundGrid 8.524 = %v", v)
	}
}

func TestScanRoundCoordinates(t *testing.T) {
	f := &Fixture{Points: []Point{{Lat: 50.05, Lon: 8.6, Label: "demo"}}}
	if got := f.Scan([]byte(`{"lat":50.05,"lon":8.6}`)); len(got) != 1 {
		t.Errorf("exact round position not found: %s", kinds(got))
	}
	if got := f.Scan([]byte(`{"lat":50.1,"lon":8.6}`)); len(got) != 0 {
		t.Errorf("rounded latitude reported: %s", kinds(got))
	}
}

func TestScanAllowIPs(t *testing.T) {
	f := &Fixture{IPs: []string{"203.0.113.5", "2001:db8::1"}, AllowIPs: []string{"203.0.113.5", "2001:db8::1"}}
	if got := f.Scan([]byte(`{"ip":"203.0.113.5","v6":"2001:db8::1"}`)); len(got) != 1 || got[0].Kind != "key" {
		t.Errorf("allowed addresses reported: %s", kinds(got))
	}
	if got := f.Scan([]byte(`{"a":"203.0.113.6"}`)); len(got) != 1 {
		t.Errorf("other address not reported: %s", kinds(got))
	}
}

// PRIV-3: the island guard measures above the ground (DESIGN §8.6).
func TestIslandLowAGL(t *testing.T) {
	fp := func(v float64) *float64 { return &v }
	if !IslandLowAGL(nil, fp(2500), nil, 1800) || IslandLowAGL(nil, fp(2900), nil, 1800) {
		t.Error("AGL over 1800 ft terrain")
	}
	if !IslandLowAGL(nil, nil, nil, 0) || !AltLowAGL(math.NaN(), 0) {
		t.Error("unknown altitude is low")
	}
	if IslandLowAGL(nil, fp(1500), nil, -50) {
		t.Error("negative ground counts as sea level")
	}
}
