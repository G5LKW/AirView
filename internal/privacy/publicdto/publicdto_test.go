package publicdto

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"airview/internal/model"
	"airview/internal/privacy"
)

// TestNoForbiddenFields is the structural guarantee: no public type has a
// JSON member whose name is a forbidden key, at any depth.
func TestNoForbiddenFields(t *testing.T) {
	forbidden := map[string]bool{}
	for _, k := range privacy.ForbiddenPublicKeys {
		forbidden[k] = true
	}
	seen := map[reflect.Type]bool{}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map || typ.Kind() == reflect.Array {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if forbidden[name] {
				t.Errorf("%s.%s: forbidden public member %q", path, f.Name, name)
			}
			walk(path+"."+f.Name, f.Type)
		}
	}
	for _, v := range []any{Snapshot{}, Trails{}, Status{}, Notice{}, StationList{}, NetworkStats{}, Summary{}, TarAircraftFile{}, TarReceiver{}} {
		walk(reflect.TypeOf(v).Name(), reflect.TypeOf(v))
	}
}

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

func fullAircraft() model.Aircraft {
	b := true
	g := false
	return model.Aircraft{
		ICAO: "3C6444", AddrType: "icao", Callsign: "DLH4AB", Squawk: "1000", Category: "A3",
		Registration: "D-AIBA", TypeCode: "A319", Lat: fp(50.1), Lon: fp(8.6), Source: model.SrcMLAT, PosAge: fp(1.2),
		OnGround: &g, AltBaro: fp(12000), AltGeom: fp(12200), VertRate: fp(-640), GS: fp(310.4), Track: fp(181.25),
		ADSBVersion: ip(2), NIC: ip(8), RcM: fp(186), Emergency: "medical",
		DistKm: fp(12), Bearing: fp(90), Elevation: fp(3), SlantKm: fp(12.4), RSSI: fp(-12), RSSIAvg: fp(-13),
		Antennas: []string{"yagi"}, Feeds: []string{"st/abc/0"}, CPAKm: fp(2), CPAInS: fp(60), TerrainVisible: &b,
		LOSMarginDeg: fp(1), Mine: true, Stations: []string{"abc"}, Msgs: 1234, MsgRate: 5,
		Highlight: &model.AlertMark{RuleID: "near", Label: "within 5 km"}, FirstSeen: 1, LastSeen: 1731500000000,
		MLATGrade: "A", MLATSigmaM: fp(40),
	}
}

func TestFromAircraftDropsReceiverFields(t *testing.T) {
	a := fullAircraft()
	p := FromAircraft(&a)
	b, _ := json.Marshal(p)
	for _, k := range []string{"distKm", "bearing", "rssi", "feeds", "antennas", "mine", "stations", "cpaKm", "highlight", "msgs"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("public aircraft has %q: %s", k, b)
		}
	}
	if p.ICAO != "3C6444" || *p.Lat != 50.1 || p.MLATGrade != "A" || p.Registration != "D-AIBA" {
		t.Errorf("lost public fields: %+v", p)
	}
	p.Coarsen(0.05)
	if *p.Lat != 50.1 || *p.Lon != 8.6 || !p.Coarse {
		t.Errorf("coarsen: %v %v", *p.Lat, *p.Lon)
	}
	q := FromAircraft(&a)
	a.Lat = fp(50.0331)
	q = FromAircraft(&a)
	q.Coarsen(0.05)
	if *q.Lat != 50.05 {
		t.Errorf("coarsen 50.0331: %v", *q.Lat)
	}
}

func TestFromSnapshotGuard(t *testing.T) {
	a := fullAircraft()
	s := model.LiveSnapshot{Now: 5, Aircraft: []model.Aircraft{a, a}, Counts: model.LiveCounts{Aircraft: 2, MaxRangeKm: 321}}
	n := 0
	out := FromSnapshot(&s, func(*model.Aircraft) bool { n++; return n == 1 }, 0.05)
	if len(out.Aircraft) != 2 || !out.Aircraft[0].Coarse || out.Aircraft[1].Coarse || out.Counts.Aircraft != 2 {
		t.Errorf("FromSnapshot: %+v", out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "maxRangeKm") {
		t.Error("maxRangeKm in public counts")
	}
}

func TestTrailPointJSON(t *testing.T) {
	b, _ := json.Marshal([]TrailPoint{FromTrailPoint(model.TrailPoint{T: 1, Lat: 50.123456, Lon: 8.5, Alt: 1000, GS: math.NaN(), RSSI: -12, Src: 1})})
	if string(b) != `[[1,50.12346,8.50000,1000,null,null,1]]` {
		t.Errorf("trail JSON %s", b)
	}
}

func TestTar1090Shape(t *testing.T) {
	a := fullAircraft()
	pub := FromSnapshot(&model.LiveSnapshot{Now: 1731500001000, Aircraft: []model.Aircraft{a}}, nil, 0)
	f := TarFromSnapshot(&pub, 99)
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Now      float64
		Messages int
		Aircraft []map[string]any
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Now != 1731500001 || m.Messages != 99 || len(m.Aircraft) != 1 {
		t.Fatalf("file: %s", b)
	}
	x := m.Aircraft[0]
	want := map[string]any{"hex": "3c6444", "type": "mlat", "flight": "DLH4AB  ", "r": "D-AIBA", "t": "A319",
		"alt_baro": 12000.0, "alt_geom": 12200.0, "baro_rate": -640.0, "gs": 310.4, "track": 181.25,
		"squawk": "1000", "emergency": "lifeguard", "category": "A3", "lat": 50.1, "lon": 8.6, "nic": 8.0,
		"rc": 186.0, "version": 2.0, "seen": 1.0, "seen_pos": 1.2}
	for k, v := range want {
		if x[k] != v {
			t.Errorf("%s = %v (%T), want %v", k, x[k], x[k], v)
		}
	}
	if ml, _ := x["mlat"].([]any); len(ml) < 2 {
		t.Errorf("mlat fields %v", x["mlat"])
	}
	for _, k := range []string{"rssi", "messages"} {
		if _, ok := x[k]; ok {
			t.Errorf("aircraft has %s", k)
		}
	}
	// On the ground.
	g := true
	a.OnGround = &g
	pub = FromSnapshot(&model.LiveSnapshot{Now: 1, Aircraft: []model.Aircraft{a}}, nil, 0)
	if tf := TarFromSnapshot(&pub, 0); tf.Aircraft[0].AltBaro != "ground" {
		t.Errorf("ground: %v", tf.Aircraft[0].AltBaro)
	}
}

// BenchmarkFromSnapshot10k: the public projection of a 10,000-aircraft
// picture (DESIGN §20.2 scale target), built once per second and shared by
// every public viewer.
func BenchmarkFromSnapshot10k(b *testing.B) {
	a := fullAircraft()
	s := model.LiveSnapshot{Now: 1, Aircraft: make([]model.Aircraft, 10000)}
	for i := range s.Aircraft {
		s.Aircraft[i] = a
	}
	b.ReportAllocs()
	for b.Loop() {
		p := FromSnapshot(&s, nil, 0)
		_ = TarFromSnapshot(&p, 0)
	}
}

// The public aircraft types cannot carry a receiver list under any name.
func TestAircraftHasNoReceiverList(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(Aircraft{}), reflect.TypeOf(TarAircraft{})} {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			switch name {
			case "stations", "seenBy", "receivers", "feeds":
				t.Errorf("%s.%s is a receiver list", typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

// PRIV-A7: anonymous station statistics carry no lifetime counter and only
// coarse rates.
func TestStationStatsUnlinkable(t *testing.T) {
	up, day, ml := 87.3, 347, 12
	s := &StationStats{MsgRate: 347.6, PosRate: 0.83, Aircraft: 12, Positions: 1234567, UptimePct: &up, AircraftDay: &day, MLATContrib: &ml}
	s.Unlinkable()
	if s.Positions != 0 || s.MLATContrib != nil {
		t.Errorf("counter kept: %+v", s)
	}
	if s.MsgRate != 300 || s.PosRate != 0.8 || *s.UptimePct != 90 || *s.AircraftDay != 300 {
		t.Errorf("not coarse: msg %v pos %v up %v day %v", s.MsgRate, s.PosRate, *s.UptimePct, *s.AircraftDay)
	}
	var nilStats *StationStats
	nilStats.Unlinkable() // no panic
}
