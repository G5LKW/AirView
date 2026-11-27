package alerts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var t0 = time.Unix(1_800_000_000, 0)

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

func ip(v int) *int { return &v }

// base returns a civil airliner near Frankfurt with every field set.
func base(icao string) model.Aircraft {
	return model.Aircraft{
		ICAO: icao, Callsign: "DLH4AB  ", Squawk: "1000", Category: "A3",
		Registration: "D-AIBA", TypeCode: "A319", Operator: "Lufthansa", Country: "Germany", CountryCode: "DE",
		Lat: fp(50.0), Lon: fp(8.5), Source: model.SrcADSB, OnGround: bp(false),
		AltBaro: fp(35000), VertRate: fp(-1200), GS: fp(450),
		ADSBVersion: ip(2), NIC: ip(8), RSSIAvg: fp(-20), RSSI: fp(-18),
		DistKm: fp(50), Bearing: fp(45),
	}
}

func with(a model.Aircraft, mods ...func(*model.Aircraft)) model.Aircraft {
	for _, m := range mods {
		m(&a)
	}
	return a
}

func evalCond(t *testing.T, c Cond, a model.Aircraft, ws *watchSet, newAF bool) bool {
	t.Helper()
	r := Rule{Name: "t", Match: c}
	if err := r.Normalize(); err != nil {
		t.Fatalf("normalize %+v: %v", c, err)
	}
	cr := compileRule(r)
	v := view{watch: ws}
	v.set(&a)
	v.newAF = newAF
	return cr.root.eval(&v)
}

func TestConditions(t *testing.T) {
	ws := &watchSet{lists: map[string]map[uint32]watchEntry{"spot": {0x3C6444: {note: "special"}}}}
	nilPos := func(a *model.Aircraft) { a.Lat, a.Lon = nil, nil }
	cases := []struct {
		name string
		c    Cond
		mod  func(*model.Aircraft)
		want bool
	}{
		{"icao exact", Cond{Type: CondICAO, Values: []string{"3C6444"}}, nil, true},
		{"icao lower", Cond{Type: CondICAO, Values: []string{"3c6444"}}, nil, true},
		{"icao tilde", Cond{Type: CondICAO, Values: []string{"~3C6444"}}, nil, true},
		{"icao glob", Cond{Type: CondICAO, Values: []string{"3C*"}}, nil, true},
		{"icao glob ?", Cond{Type: CondICAO, Values: []string{"3C64?4"}}, nil, true},
		{"icao glob miss", Cond{Type: CondICAO, Values: []string{"3D*"}}, nil, false},
		{"icao range", Cond{Type: CondICAO, Values: []string{"3C0000-3FFFFF"}}, nil, true},
		{"icao range miss", Cond{Type: CondICAO, Values: []string{"400000-43FFFF", "AE0000-AFFFFF"}}, nil, false},
		{"icao unparsable aircraft", Cond{Type: CondICAO, Values: []string{"*"}}, func(a *model.Aircraft) { a.ICAO = "XYZ" }, false},
		{"reg glob", Cond{Type: CondReg, Values: []string{"D-AI*"}}, nil, true},
		{"reg case", Cond{Type: CondReg, Values: []string{"d-aiba"}}, nil, true},
		{"reg miss", Cond{Type: CondReg, Values: []string{"G-*"}}, nil, false},
		{"reg unknown never matches *", Cond{Type: CondReg, Values: []string{"*"}}, func(a *model.Aircraft) { a.Registration = "" }, false},
		{"callsign trimmed", Cond{Type: CondCallsign, Values: []string{"DLH4AB"}}, nil, true},
		{"callsign glob", Cond{Type: CondCallsign, Values: []string{"BAW*", "DLH*"}}, nil, true},
		{"callsign miss", Cond{Type: CondCallsign, Values: []string{"BAW*"}}, nil, false},
		{"operator", Cond{Type: CondOperator, Values: []string{"luft*"}}, nil, true},
		{"actype", Cond{Type: CondAircraft, Values: []string{"A31?"}}, nil, true},
		{"actype miss", Cond{Type: CondAircraft, Values: []string{"B74*"}}, nil, false},
		{"category", Cond{Type: CondCategory, Values: []string{"A3"}}, nil, true},
		{"category miss", Cond{Type: CondCategory, Values: []string{"B*"}}, nil, false},
		{"military civil", Cond{Type: CondMilitary}, nil, false},
		{"military yes", Cond{Type: CondMilitary}, func(a *model.Aircraft) { a.Military = true }, true},
		{"military false", Cond{Type: CondMilitary, Value: bp(false)}, nil, true},
		{"country iso", Cond{Type: CondCountry, Values: []string{"DE"}}, nil, true},
		{"country name", Cond{Type: CondCountry, Values: []string{"germ*"}}, nil, true},
		{"country miss", Cond{Type: CondCountry, Values: []string{"FR"}}, nil, false},
		{"squawk miss", Cond{Type: CondSquawk, Values: []string{"7700"}}, nil, false},
		{"squawk 7700", Cond{Type: CondSquawk, Values: []string{"7500", "7600", "7700"}}, func(a *model.Aircraft) { a.Squawk = "7700" }, true},
		{"squawk glob", Cond{Type: CondSquawk, Values: []string{"76*"}}, func(a *model.Aircraft) { a.Squawk = "7600" }, true},
		{"emergency none", Cond{Type: CondEmergency}, nil, false},
		{"emergency 'none'", Cond{Type: CondEmergency}, func(a *model.Aircraft) { a.Emergency = "none" }, false},
		{"emergency any", Cond{Type: CondEmergency}, func(a *model.Aircraft) { a.Emergency = "general" }, true},
		{"emergency any value", Cond{Type: CondEmergency, Values: []string{"unlawful", "any"}}, func(a *model.Aircraft) { a.Emergency = "medical" }, true},
		{"emergency specific", Cond{Type: CondEmergency, Values: []string{"unlawful"}}, func(a *model.Aircraft) { a.Emergency = "unlawful" }, true},
		{"emergency specific miss", Cond{Type: CondEmergency, Values: []string{"unlawful"}}, func(a *model.Aircraft) { a.Emergency = "general" }, false},
		{"alt max", Cond{Type: CondAltitude, Max: fp(30000)}, nil, false},
		{"alt min", Cond{Type: CondAltitude, Min: fp(30000)}, nil, true},
		{"alt band inclusive", Cond{Type: CondAltitude, Min: fp(35000), Max: fp(35000)}, nil, true},
		{"alt geom fallback", Cond{Type: CondAltitude, Max: fp(2000)}, func(a *model.Aircraft) { a.AltBaro, a.AltGeom = nil, fp(1500) }, true},
		{"alt unknown", Cond{Type: CondAltitude, Max: fp(100000)}, func(a *model.Aircraft) { a.AltBaro = nil }, false},
		{"speed", Cond{Type: CondSpeed, Min: fp(400), Max: fp(500)}, nil, true},
		{"speed unknown", Cond{Type: CondSpeed, Min: fp(0)}, func(a *model.Aircraft) { a.GS = nil }, false},
		{"vrate descending", Cond{Type: CondVRate, Max: fp(-1000)}, nil, true},
		{"vrate climbing", Cond{Type: CondVRate, Min: fp(1000)}, nil, false},
		{"distance max", Cond{Type: CondDistance, Max: fp(40)}, nil, false},
		{"distance min", Cond{Type: CondDistance, Min: fp(40)}, nil, true},
		{"distance unknown", Cond{Type: CondDistance, Min: fp(0)}, func(a *model.Aircraft) { a.DistKm = nil }, false},
		{"bearing", Cond{Type: CondBearing, From: fp(0), To: fp(90)}, nil, true},
		{"bearing wrap in", Cond{Type: CondBearing, From: fp(350), To: fp(10)}, func(a *model.Aircraft) { a.Bearing = fp(5) }, true},
		{"bearing wrap in2", Cond{Type: CondBearing, From: fp(350), To: fp(10)}, func(a *model.Aircraft) { a.Bearing = fp(355) }, true},
		{"bearing wrap out", Cond{Type: CondBearing, From: fp(350), To: fp(10)}, nil, false},
		{"bearing full circle", Cond{Type: CondBearing, From: fp(0), To: fp(360)}, func(a *model.Aircraft) { a.Bearing = fp(359.9) }, true},
		{"bearing unknown", Cond{Type: CondBearing, From: fp(0), To: fp(360)}, func(a *model.Aircraft) { a.Bearing = nil }, false},
		{"adsb version", Cond{Type: CondADSBVersion, Min: fp(2)}, nil, true},
		{"adsb version old", Cond{Type: CondADSBVersion, Max: fp(1)}, nil, false},
		{"adsb version unknown", Cond{Type: CondADSBVersion, Max: fp(7)}, func(a *model.Aircraft) { a.ADSBVersion = nil }, false},
		{"nic", Cond{Type: CondNIC, Min: fp(9)}, nil, false},
		{"nic low", Cond{Type: CondNIC, Max: fp(8)}, nil, true},
		{"rssi avg", Cond{Type: CondRSSI, Max: fp(-15)}, nil, true},
		{"rssi fallback", Cond{Type: CondRSSI, Max: fp(-15)}, func(a *model.Aircraft) { a.RSSIAvg, a.RSSI = nil, fp(-10) }, false},
		{"source adsb", Cond{Type: CondSource, Values: []string{"ADSB"}}, nil, true},
		{"source mlat", Cond{Type: CondSource, Values: []string{"mlat"}}, nil, false},
		{"on ground default", Cond{Type: CondOnGround}, nil, false},
		{"airborne", Cond{Type: CondOnGround, Value: bp(false)}, nil, true},
		{"ground unknown", Cond{Type: CondOnGround, Value: bp(false)}, func(a *model.Aircraft) { a.OnGround = nil }, false},
		{"watchlist named", Cond{Type: CondWatchlist, Values: []string{"spot"}}, nil, true},
		{"watchlist other", Cond{Type: CondWatchlist, Values: []string{"other"}}, nil, false},
		{"watchlist any", Cond{Type: CondWatchlist}, nil, true},
		{"watchlist not on", Cond{Type: CondWatchlist}, func(a *model.Aircraft) { a.ICAO = "3C6445" }, false},
		// 50.0,8.5 is ~7.2 km west of 50.0,8.6.
		{"circle inside", Cond{Type: CondGeofence, Lat: fp(50), Lon: fp(8.6), RadiusKm: fp(10)}, nil, true},
		{"circle too small", Cond{Type: CondGeofence, Lat: fp(50), Lon: fp(8.6), RadiusKm: fp(5)}, nil, false},
		{"circle outside flag", Cond{Type: CondGeofence, Lat: fp(50), Lon: fp(8.6), RadiusKm: fp(5), Inside: bp(false)}, nil, true},
		{"circle no position", Cond{Type: CondGeofence, Lat: fp(50), Lon: fp(8.6), RadiusKm: fp(10)}, nilPos, false},
		{"circle no position outside", Cond{Type: CondGeofence, Lat: fp(50), Lon: fp(8.6), RadiusKm: fp(10), Inside: bp(false)}, nilPos, false},
		{"polygon", Cond{Type: CondGeofence, Polygon: [][2]float64{{8, 49}, {9, 49}, {9, 51}, {8, 51}}}, nil, true},
		{"polygon outside", Cond{Type: CondGeofence, Polygon: [][2]float64{{9, 49}, {10, 49}, {10, 51}, {9, 51}}}, nil, false},
		{"negate", Cond{Type: CondSquawk, Values: []string{"7700"}, Negate: true}, nil, true},
		{"and", Cond{Op: "and", Conds: []Cond{{Type: CondCallsign, Values: []string{"DLH*"}}, {Type: CondAltitude, Min: fp(30000)}}}, nil, true},
		{"and fails", Cond{Op: "AND", Conds: []Cond{{Type: CondCallsign, Values: []string{"DLH*"}}, {Type: CondMilitary}}}, nil, false},
		{"or", Cond{Op: "or", Conds: []Cond{{Type: CondMilitary}, {Type: CondReg, Values: []string{"D-*"}}}}, nil, true},
		{"negated group", Cond{Op: "or", Negate: true, Conds: []Cond{{Type: CondMilitary}, {Type: CondSquawk, Values: []string{"7*"}}}}, nil, true},
		{"nested", Cond{Op: "and", Conds: []Cond{
			{Op: "or", Conds: []Cond{{Type: CondCountry, Values: []string{"FR"}}, {Type: CondCountry, Values: []string{"DE"}}}},
			{Type: CondDistance, Max: fp(100)},
		}}, nil, true},
		{"system leaf without event", Cond{Type: CondSystem, Values: []string{"record"}}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base("3C6444")
			if tc.mod != nil {
				tc.mod(&a)
			}
			if got := evalCond(t, tc.c, a, ws, false); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	// New airframe depends on the engine's note.
	if !evalCond(t, Cond{Type: CondNewAirframe}, base("3C6444"), ws, true) ||
		evalCond(t, Cond{Type: CondNewAirframe}, base("3C6444"), ws, false) ||
		!evalCond(t, Cond{Type: CondNewAirframe, Value: bp(false)}, base("3C6444"), ws, false) {
		t.Fatal("newAirframe")
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		p, s string
		want bool
	}{
		{"A*B?C", "axxbyc", true}, {"A*B?C", "abyc", true}, {"A*B?C", "abc", false},
		{"*", "", true}, {"*", "anything", true}, {"?", "", false}, {"", "", true}, {"", "a", false},
		{"*AB", "xxaab", true}, {"*AB*", "zzabzz", true}, {"A**", "a", true}, {"DLH*", "DLH", true},
		{"*X", "abc", false}, {"A?", "a", false}, {"7?00", "7700", true}, {"7?00", "770", false},
	}
	for _, c := range cases {
		if got := globMatch(c.p, c.s); got != c.want {
			t.Errorf("globMatch(%q,%q)=%v", c.p, c.s, got)
		}
		var b [8]byte
		if len(c.s) <= 8 {
			n := copy(b[:], c.s)
			if got := globMatchArr(c.p, &b, n); got != c.want {
				t.Errorf("globMatchArr(%q,%q)=%v", c.p, c.s, got)
			}
		}
	}
}

func TestPolygonAndSector(t *testing.T) {
	sq := newPolygon([][2]float64{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}) // closed ring
	if len(sq.xs) != 4 {
		t.Fatalf("closing point not dropped: %d", len(sq.xs))
	}
	for _, c := range []struct {
		lon, lat float64
		want     bool
	}{{5, 5, true}, {0.001, 9.999, true}, {11, 5, false}, {-1, 5, false}, {5, -0.1, false}, {5, 10.1, false}} {
		if got := sq.contains(c.lon, c.lat); got != c.want {
			t.Errorf("square contains(%v,%v)=%v", c.lon, c.lat, got)
		}
	}
	// Concave "U": the notch (4..6 x 4..10) is outside.
	u := newPolygon([][2]float64{{0, 0}, {10, 0}, {10, 10}, {6, 10}, {6, 4}, {4, 4}, {4, 10}, {0, 10}})
	for _, c := range []struct {
		lon, lat float64
		want     bool
	}{{5, 2, true}, {5, 6, false}, {2, 8, true}, {8, 8, true}, {5, 9.9, false}} {
		if got := u.contains(c.lon, c.lat); got != c.want {
			t.Errorf("U contains(%v,%v)=%v", c.lon, c.lat, got)
		}
	}
	// Across the antimeridian: 170E .. 170W.
	am := newPolygon([][2]float64{{170, -10}, {-170, -10}, {-170, 10}, {170, 10}})
	for _, c := range []struct {
		lon, lat float64
		want     bool
	}{{179, 0, true}, {-179, 0, true}, {180, 0, true}, {-180, 0, true}, {0, 0, false}, {160, 0, false}, {-160, 0, false}, {175, 11, false}} {
		if got := am.contains(c.lon, c.lat); got != c.want {
			t.Errorf("antimeridian contains(%v,%v)=%v", c.lon, c.lat, got)
		}
	}
	for _, c := range []struct {
		b, from, to float64
		want        bool
	}{{45, 0, 90, true}, {90, 0, 90, true}, {91, 0, 90, false}, {5, 350, 10, true}, {-5, 350, 10, true},
		{365, 0, 10, true}, {180, 350, 10, false}, {123, 0, 360, true}} {
		if got := inSector(c.b, c.from, c.to); got != c.want {
			t.Errorf("inSector(%v,%v,%v)=%v", c.b, c.from, c.to, got)
		}
	}
}

func TestParseRuleValid(t *testing.T) {
	r, err := ParseRule([]byte(`{"name":"  Emergencies near me ","match":{"op":"AND","conds":[
		{"type":"squawk","values":[" 7700 "]},{"type":"distance","max":100}]},
		"actions":{"notify":true,"highlight":"#F00","webhook":"https://example.com/hook?x=1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "Emergencies near me" || r.Severity != SeverityInfo || r.CooldownSec != 900 || r.Match.Op != "and" ||
		r.Match.Conds[0].Values[0] != "7700" || r.ID != "" {
		t.Fatalf("normalised: %+v", r)
	}
	for _, d := range DefaultRules() {
		if err := d.Normalize(); err != nil {
			t.Fatalf("default %s: %v", d.ID, err)
		}
		want := d.ID == "builtin-emergency" || d.ID == "builtin-military" || d.ID == "builtin-record"
		if d.Enabled != want || !d.Builtin {
			t.Fatalf("default %s enabled=%v", d.ID, d.Enabled)
		}
	}
}

func TestParseRuleHostile(t *testing.T) {
	deep := strings.Repeat(`{"op":"and","conds":[`, 10) + `{"type":"military"}` + strings.Repeat(`]}`, 10)
	var many strings.Builder
	many.WriteString(`{"op":"or","conds":[`)
	for i := 0; i < MaxNodes+5; i++ {
		if i > 0 {
			many.WriteByte(',')
		}
		many.WriteString(`{"type":"military"}`)
	}
	many.WriteString(`]}`)
	vals := make([]string, MaxValues+1)
	for i := range vals {
		vals[i] = `"A"`
	}
	leaf := func(s string) string { return `{"name":"x","match":` + s + `}` }
	cases := map[string]string{
		"not json":           `{"name":`,
		"array":              `[]`,
		"unknown field":      `{"name":"x","match":{"type":"military"},"evil":1}`,
		"unknown cond field": leaf(`{"type":"military","foo":1}`),
		"trailing":           `{"name":"x","match":{"type":"military"}} {"name":"y"}`,
		"no name":            `{"match":{"type":"military"}}`,
		"blank name":         `{"name":"   ","match":{"type":"military"}}`,
		"control in name":    `{"name":"a\u0007b","match":{"type":"military"}}`,
		"long name":          `{"name":"` + strings.Repeat("x", MaxNameLen+1) + `","match":{"type":"military"}}`,
		"bad id":             `{"id":"../etc","name":"x","match":{"type":"military"}}`,
		"bad severity":       `{"name":"x","severity":"panic","match":{"type":"military"}}`,
		"neg cooldown":       `{"name":"x","cooldownSec":-1,"match":{"type":"military"}}`,
		"huge cooldown":      `{"name":"x","cooldownSec":99999999,"match":{"type":"military"}}`,
		"bad colour":         `{"name":"x","match":{"type":"military"},"actions":{"highlight":"red;background:url(x)"}}`,
		"js webhook":         `{"name":"x","match":{"type":"military"},"actions":{"webhook":"javascript:alert(1)"}}`,
		"ftp webhook":        `{"name":"x","match":{"type":"military"},"actions":{"webhook":"ftp://host/x"}}`,
		"hostless webhook":   `{"name":"x","match":{"type":"military"},"actions":{"webhook":"http:///x"}}`,
		"relative webhook":   `{"name":"x","match":{"type":"military"},"actions":{"webhook":"/api/x"}}`,
		"space webhook":      `{"name":"x","match":{"type":"military"},"actions":{"webhook":"http://a.b/ x"}}`,
		"empty match":        `{"name":"x","match":{}}`,
		"empty group":        leaf(`{"op":"and","conds":[]}`),
		"bad op":             leaf(`{"op":"xor","conds":[{"type":"military"}]}`),
		"op and type":        leaf(`{"op":"and","type":"military","conds":[{"type":"military"}]}`),
		"unknown type":       leaf(`{"type":"exec"}`),
		"too deep":           leaf(deep),
		"too many nodes":     leaf(many.String()),
		"too many values":    leaf(`{"type":"reg","values":[` + strings.Join(vals, ",") + `]}`),
		"empty value":        leaf(`{"type":"reg","values":["  "]}`),
		"long value":         leaf(`{"type":"reg","values":["` + strings.Repeat("A", MaxStringLen+1) + `"]}`),
		"no values":          leaf(`{"type":"callsign"}`),
		"bad icao":           leaf(`{"type":"icao","values":["GGGGGG"]}`),
		"long icao":          leaf(`{"type":"icao","values":["1234567"]}`),
		"bad icao range":     leaf(`{"type":"icao","values":["400000-3C0000"]}`),
		"bad icao glob":      leaf(`{"type":"icao","values":["3C*Z"]}`),
		"bad source":         leaf(`{"type":"source","values":["radar"]}`),
		"bad emergency":      leaf(`{"type":"emergency","values":["fire"]}`),
		"bad list name":      leaf(`{"type":"watchlist","values":["a/b"]}`),
		"bad system":         leaf(`{"type":"system","values":["reboot"]}`),
		"no range":           leaf(`{"type":"altitude"}`),
		"min>max":            leaf(`{"type":"altitude","min":5000,"max":1000}`),
		"alt out of range":   leaf(`{"type":"altitude","max":1e9}`),
		"nic out of range":   leaf(`{"type":"nic","min":12}`),
		"bearing missing":    leaf(`{"type":"bearing","from":10}`),
		"bearing range":      leaf(`{"type":"bearing","from":10,"to":400}`),
		"fence both":         leaf(`{"type":"geofence","lat":1,"lon":2,"radiusKm":3,"polygon":[[0,0],[1,0],[1,1]]}`),
		"fence none":         leaf(`{"type":"geofence"}`),
		"fence half circle":  leaf(`{"type":"geofence","lat":1,"radiusKm":3}`),
		"fence zero radius":  leaf(`{"type":"geofence","lat":1,"lon":2,"radiusKm":0}`),
		"fence bad lat":      leaf(`{"type":"geofence","lat":91,"lon":2,"radiusKm":3}`),
		"fence 2 points":     leaf(`{"type":"geofence","polygon":[[0,0],[1,0]]}`),
		"fence bad point":    leaf(`{"type":"geofence","polygon":[[0,0],[1,0],[200,1]]}`),
		"huge":               `{"name":"` + strings.Repeat("x", MaxRuleBytes) + `"}`,
	}
	for name, js := range cases {
		if _, err := ParseRule([]byte(js)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func FuzzParseRule(f *testing.F) {
	for _, s := range []string{
		`{"name":"x","match":{"type":"military"}}`,
		`{"name":"x","match":{"op":"or","conds":[{"type":"icao","values":["3C*","400000-40FFFF"]},{"type":"geofence","polygon":[[0,0],[1,0],[1,1]]}]}}`,
		`{"name":"x","match":{"type":"bearing","from":350,"to":10,"negate":true}}`,
		`{"name":"x","match":{"type":"system","values":["record:*"]}}`,
	} {
		f.Add([]byte(s))
	}
	a := base("3C6444")
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := ParseRule(data)
		if err != nil {
			return
		}
		c := compileRule(r)
		v := view{}
		v.set(&a)
		_ = c.root.eval(&v)
		if _, err := json.Marshal(r); err != nil {
			t.Fatal(err)
		}
	})
}

// newTestEngine returns an engine holding only the given rules.
func newTestEngine(t *testing.T, o Options, rules ...Rule) *Engine {
	t.Helper()
	if o.Logger == nil {
		o.Logger = quiet
	}
	e := New(o)
	ctx := context.Background()
	for _, r := range e.Rules() {
		if err := e.DeleteRule(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rules {
		if _, err := e.PutRule(ctx, r); err != nil {
			t.Fatalf("put %s: %v", r.Name, err)
		}
	}
	return e
}

func sq(icao, squawk string) model.Aircraft {
	return with(base(icao), func(a *model.Aircraft) { a.Squawk = squawk })
}

func squawkRule(id string, cooldown int) Rule {
	return Rule{ID: id, Name: "Squawk " + id, Enabled: true, Severity: SeverityCritical, CooldownSec: cooldown,
		Match: Cond{Type: CondSquawk, Values: []string{"7700"}}, Actions: Actions{Notify: true}}
}

// TestPredictedPositions: position-dependent rules ignore aircraft at a
// predicted (coasting) position unless the rule sets includePredicted;
// other rules are unaffected.
func TestPredictedPositions(t *testing.T) {
	fence := func(id string, incl bool) Rule {
		return Rule{ID: id, Name: "Fence " + id, Enabled: true, CooldownSec: 60, IncludePredicted: incl,
			Match: Cond{Op: "and", Conds: []Cond{
				{Type: CondCallsign, Values: []string{"DLH*"}},
				{Type: CondGeofence, Lat: fp(50), Lon: fp(8.5), RadiusKm: fp(10)},
			}}}
	}
	alt := Rule{ID: "alt", Name: "Low", Enabled: true, CooldownSec: 60, Match: Cond{Type: CondAltitude, Max: fp(40000)}}
	e := newTestEngine(t, Options{}, fence("strict", false), fence("incl", true), alt, squawkRule("sq", 60))
	pred := with(sq("3C6444", "7700"), func(a *model.Aircraft) { a.Predicted = true })
	e.Evaluate([]model.Aircraft{pred}, at(0))
	got := map[string]int{}
	for _, al := range e.Recent(0, 0) {
		got[al.RuleID]++
	}
	if got["strict"] != 0 || got["alt"] != 0 {
		t.Errorf("position rules fired on a predicted position: %v", got)
	}
	if got["incl"] != 1 || got["sq"] != 1 {
		t.Errorf("includePredicted / non-positional rules must fire: %v", got)
	}
	// A real position fires the strict rule.
	e.Evaluate([]model.Aircraft{sq("3C6444", "7700")}, at(1))
	got = map[string]int{}
	for _, al := range e.Recent(0, 0) {
		got[al.RuleID]++
	}
	if got["strict"] != 1 || got["alt"] != 1 {
		t.Errorf("real position: %v", got)
	}
	// TestRule follows the same logic, and the flag round-trips through JSON.
	m, err := e.TestRule(fence("x", false), []model.Aircraft{pred}, nil, at(2))
	if err != nil || len(m) != 0 {
		t.Errorf("TestRule on a predicted position: %v %v", m, err)
	}
	b, _ := json.Marshal(fence("y", true))
	r, err := ParseRule(b)
	if err != nil || !r.IncludePredicted || !strings.Contains(string(b), `"includePredicted":true`) {
		t.Errorf("includePredicted JSON: %s %v", b, err)
	}
}

func TestCooldownAndDedup(t *testing.T) {
	e := newTestEngine(t, Options{}, squawkRule("sq", 60))
	a := []model.Aircraft{sq("3C6444", "7700")}
	fired := func() uint64 { return e.Stats().Fired }
	e.Evaluate(a, at(0))
	if fired() != 1 {
		t.Fatalf("first match must fire: %d", fired())
	}
	for s := 1; s <= 5; s++ { // same episode
		e.Evaluate(a, at(float64(s)))
	}
	if fired() != 1 {
		t.Fatalf("continuing match must be de-duplicated: %d", fired())
	}
	e.Evaluate(nil, at(10))
	e.Evaluate(a, at(20)) // 15 s gap > StaleAfter: new episode, but inside the 60 s cooldown
	if fired() != 1 || e.Stats().Suppressed != 1 {
		t.Fatalf("cooldown: fired %d suppressed %d", fired(), e.Stats().Suppressed)
	}
	for s := 21; s <= 70; s++ { // continuous: the episode never ends, nothing new
		e.Evaluate(a, at(float64(s)))
	}
	if fired() != 1 {
		t.Fatalf("continuous episode re-fired: %d", fired())
	}
	e.Evaluate(a, at(90)) // gap of 20 s and 90 s since the first alert
	if fired() != 2 {
		t.Fatalf("after cooldown: %d", fired())
	}
	e.Evaluate([]model.Aircraft{sq("3C6444", "7700"), sq("4CA87C", "7700")}, at(91))
	if fired() != 3 {
		t.Fatalf("a second aircraft is independent: %d", fired())
	}
	// Non-ICAO / unparsable addresses still alert (shared de-dup slot).
	e.Evaluate([]model.Aircraft{sq("~1", "7700"), sq("bad!", "7700")}, at(92))
	if fired() != 5 {
		t.Fatalf("non-icao: %d", fired())
	}
}

func TestRuleCooldownStormGuardAndStateLimit(t *testing.T) {
	r := squawkRule("sq", 60)
	r.RuleCooldownSec = 30
	e := newTestEngine(t, Options{}, r)
	e.Evaluate([]model.Aircraft{sq("000001", "7700"), sq("000002", "7700")}, at(0))
	if e.Stats().Fired != 1 {
		t.Fatalf("rule cooldown: %d", e.Stats().Fired)
	}
	e.Evaluate([]model.Aircraft{sq("000003", "7700")}, at(31))
	if e.Stats().Fired != 2 {
		t.Fatalf("after rule cooldown: %d", e.Stats().Fired)
	}

	e = newTestEngine(t, Options{MaxAlertsPerMinute: 3}, squawkRule("sq", 60))
	var many []model.Aircraft
	for i := 1; i <= 5; i++ {
		many = append(many, sq(fmt.Sprintf("%06X", i), "7700"))
	}
	e.Evaluate(many, at(0))
	if s := e.Stats(); s.Fired != 3 || s.Suppressed != 2 {
		t.Fatalf("storm guard: %+v", s)
	}
	e.Evaluate([]model.Aircraft{sq("0000AA", "7700")}, at(61))
	if e.Stats().Fired != 4 {
		t.Fatal("storm window must reset after a minute")
	}

	e = newTestEngine(t, Options{MaxStates: 2}, squawkRule("sq", 60))
	e.Evaluate(many[:3], at(0))
	if s := e.Stats(); s.Fired != 2 || s.States != 2 {
		t.Fatalf("state limit: %+v", s)
	}
	// After the sweep (episode over and cooldown passed) room is available again.
	e.Evaluate(nil, at(100))
	e.Evaluate(many[2:3], at(101))
	if s := e.Stats(); s.Fired != 3 {
		t.Fatalf("state limit after sweep: %+v", s)
	}
}

func TestHighlightsSubscribeRecent(t *testing.T) {
	warn := Rule{ID: "a-warn", Name: "Low", Enabled: true, Severity: SeverityWarning,
		Match: Cond{Type: CondAltitude, Max: fp(40000)}, Actions: Actions{Highlight: "#00ff00"}}
	crit := squawkRule("b-crit", 60)
	crit.Actions.Highlight = "#ff0000"
	e := newTestEngine(t, Options{}, warn, crit)
	ch, cancel := e.Subscribe(10)
	e.Evaluate([]model.Aircraft{sq("3C6444", "7700"), base("4CA87C")}, at(0))
	hs := e.Highlights(at(1))
	if len(hs) != 2 || hs[0].ICAO != "3C6444" || hs[0].Color != "#ff0000" || hs[0].RuleID != "b-crit" ||
		hs[1].ICAO != "4CA87C" || hs[1].Color != "#00ff00" {
		t.Fatalf("highlights: %+v", hs)
	}
	if hs[0].Until != at(15).UnixMilli() {
		t.Fatalf("until %d", hs[0].Until)
	}
	if len(e.Highlights(at(16))) != 0 {
		t.Fatal("highlights must expire")
	}
	var got []Alert
	for len(got) < 3 {
		select {
		case al := <-ch:
			got = append(got, al)
		case <-time.After(time.Second):
			t.Fatalf("only %d alerts received", len(got))
		}
	}
	if got[0].ID != 1 || got[2].ID != 3 || got[0].T != t0.UnixMilli() || got[0].Kind != "aircraft" {
		t.Fatalf("alerts: %+v", got)
	}
	rec := e.Recent(2, 0)
	if len(rec) != 2 || rec[0].ID != 3 || rec[1].ID != 2 {
		t.Fatalf("recent: %+v", rec)
	}
	if rec := e.Recent(0, 1); len(rec) != 2 {
		t.Fatalf("recent after 1: %+v", rec)
	}
	cancel()
	cancel() // idempotent
	if e.Stats().Subscribers != 0 {
		t.Fatal("unsubscribe")
	}
	// Disabled rules are not evaluated.
	if _, err := e.SetEnabled(context.Background(), "a-warn", false); err != nil {
		t.Fatal(err)
	}
	e.Evaluate([]model.Aircraft{base("AAAAAA")}, at(100))
	if e.Stats().Fired != 3 {
		t.Fatal("disabled rule fired")
	}
	// History ring wraps.
	e2 := newTestEngine(t, Options{HistorySize: 3, MaxAlertsPerMinute: 1000}, squawkRule("sq", 60))
	for i := 1; i <= 7; i++ {
		e2.Evaluate([]model.Aircraft{sq(fmt.Sprintf("%06X", i), "7700")}, at(float64(i)))
	}
	if rec := e2.Recent(100, 0); len(rec) != 3 || rec[0].ID != 7 || rec[2].ID != 5 {
		t.Fatalf("ring: %+v", rec)
	}
}

func TestAlertContentAndTemplate(t *testing.T) {
	r := squawkRule("sq", 60)
	r.Message = "{ident}/{reg}/{type}/{icao} sq {squawk} at {alt} ft {dist} km {unknown} {operator} {event} {note}"
	e := newTestEngine(t, Options{}, r)
	a := sq("3c6444", "7700")
	a.DistKm = fp(math.NaN())
	a.Lat = fp(math.Inf(1))
	e.Evaluate([]model.Aircraft{a}, at(0))
	al := e.Recent(1, 0)[0]
	want := "DLH4AB/D-AIBA/A319/3C6444 sq 7700 at 35000 ft ? km {unknown} Lufthansa ? ?"
	if al.Text != want {
		t.Fatalf("text %q\nwant %q", al.Text, want)
	}
	if al.DistKm != nil || al.Lat != nil || al.Lon != nil || al.AltFt == nil || *al.AltFt != 35000 || al.ICAO != "3C6444" {
		t.Fatalf("alert fields: %+v", al)
	}
	for k, want := range map[string]string{"max_dist_km": "km", "max_alt_ft": "ft", "max_gs_kt": "kt",
		"min_rssi_pos_dbfs": "dBFS", "max_aircraft": "", "max_msg_rate": "msg/s", "max_flight_s": "s"} {
		if got := RecordUnit(k); got != want {
			t.Errorf("RecordUnit(%s) = %q, want %q", k, got, want)
		}
	}
	if _, err := json.Marshal(e.Recent(0, 0)); err != nil {
		t.Fatalf("alerts must be JSON-safe: %v", err)
	}
	// Default template.
	e = newTestEngine(t, Options{}, squawkRule("sq", 60))
	e.Evaluate([]model.Aircraft{sq("3C6444", "7700")}, at(0))
	if got := e.Recent(1, 0)[0].Text; got != "Squawk sq: DLH4AB (A319) [3C6444] sq 7700 35000 ft 50.0 km" {
		t.Fatalf("default text %q", got)
	}
	if al := e.Recent(1, 0)[0]; al.GSKt == nil || *al.GSKt != 450 || al.VRFpm == nil || *al.VRFpm != -1200 ||
		al.BrgDeg == nil || *al.BrgDeg != 45 || al.RSSI == nil || *al.RSSI != -18 || al.TrackDeg != nil || !al.Auto {
		t.Fatalf("numeric fields: %+v", al)
	}
	if !strings.Contains(PlaceholderHelp(), "{ident}") {
		t.Fatal("help")
	}
}

func TestSystemEvents(t *testing.T) {
	rec := Rule{ID: "rec", Name: "Record", Enabled: true, CooldownSec: 600,
		Match: Cond{Type: CondSystem, Values: []string{"record:FARTHEST*"}}, Actions: Actions{Notify: true}}
	milRec := Rule{ID: "milrec", Name: "Military record", Enabled: true,
		Match: Cond{Op: "and", Conds: []Cond{{Type: CondSystem, Values: []string{"record"}}, {Type: CondMilitary}}}}
	e := newTestEngine(t, Options{}, rec, milRec)
	e.HandleSystem(SystemEvent{Kind: "record", Key: "farthest_km", Text: "new farthest 412 km", Value: fp(412.5), T: at(0)})
	al := e.Recent(1, 0)
	if len(al) != 1 || al[0].Kind != "system" || al[0].Event != "record" || al[0].EventKey != "farthest_km" ||
		al[0].Value == nil || *al[0].Value != 412.5 || al[0].Text != "Record: new farthest 412 km" || al[0].Unit != "km" {
		t.Fatalf("record alert: %+v", al)
	}
	e.HandleSystem(SystemEvent{Kind: "RECORD", Key: "farthest_km", T: at(10)}) // per-key cooldown
	e.HandleSystem(SystemEvent{Kind: "record", Key: "highest", T: at(11)})     // pattern does not match
	if e.Stats().Fired != 1 {
		t.Fatalf("fired %d", e.Stats().Fired)
	}
	e.HandleSystem(SystemEvent{Kind: "record", Key: "farthest_mlat", T: at(12)}) // other key: own cooldown
	if e.Stats().Fired != 2 {
		t.Fatalf("other key: %d", e.Stats().Fired)
	}
	mil := with(base("AE1234"), func(a *model.Aircraft) { a.Military = true })
	e.HandleSystem(SystemEvent{Kind: "record", Key: "fastest", Aircraft: &mil, Value: fp(math.NaN()), T: at(13)})
	al = e.Recent(1, 0)
	if e.Stats().Fired != 3 || al[0].RuleID != "milrec" || al[0].ICAO != "AE1234" || al[0].Value != nil {
		t.Fatalf("aircraft condition in a system rule: %+v", al)
	}
	// System rules are never evaluated per aircraft.
	e.Evaluate([]model.Aircraft{mil}, at(20))
	if e.Stats().Fired != 3 {
		t.Fatal("system rule fired from Evaluate")
	}
}

func TestDefaultRulesEngine(t *testing.T) {
	e := New(Options{Logger: quiet})
	mil := with(base("AE1234"), func(a *model.Aircraft) { a.Military = true; a.Callsign = "RCH123" })
	emerg := with(base("3C6444"), func(a *model.Aircraft) { a.Emergency = "general" })
	e.Evaluate([]model.Aircraft{sq("4CA87C", "7700"), mil, emerg, base("400001")}, at(0))
	byRule := map[string]int{}
	for _, al := range e.Recent(0, 0) {
		byRule[al.RuleID]++
	}
	if byRule["builtin-emergency"] != 2 || byRule["builtin-military"] != 1 || len(byRule) != 2 {
		t.Fatalf("default rule alerts: %v", byRule)
	}
	hl := map[string]string{}
	for _, h := range e.Highlights(at(0)) {
		hl[h.ICAO] = h.Color
	}
	if hl["4CA87C"] != "#ff3b30" || hl["AE1234"] != "#ffb020" || len(hl) != 3 {
		t.Fatalf("highlights %v", hl)
	}
	e.HandleSystem(SystemEvent{Kind: SysRecord, Key: "farthest", T: at(1)})
	e.HandleSystem(SystemEvent{Kind: SysFeedDown, Key: "rc", T: at(1)})
	if n := len(e.Recent(0, 0)); n != 4 {
		t.Fatalf("record enabled, feed-down disabled: %d alerts", n)
	}
}

func TestNewAirframe(t *testing.T) {
	e := New(Options{Logger: quiet})
	if _, err := e.SetEnabled(context.Background(), "builtin-new-airframe", true); err != nil {
		t.Fatal(err)
	}
	e.NoteNewAirframe(0x3C6444, at(0))
	e.NoteNewAirframe(0x4CA87C, at(-20*60)) // older than the 15 min window
	e.Evaluate([]model.Aircraft{base("3C6444"), base("4CA87C"), base("400001")}, at(1))
	rec := e.Recent(0, 0)
	if len(rec) != 1 || rec[0].ICAO != "3C6444" || rec[0].RuleID != "builtin-new-airframe" {
		t.Fatalf("new airframe: %+v", rec)
	}
}

func TestEvaluateAllocFree(t *testing.T) {
	e := New(Options{Logger: quiet})
	ctx := context.Background()
	for _, id := range []string{"builtin-new-airframe", "builtin-watchlist", "builtin-overhead", "builtin-unusual"} {
		if _, err := e.SetEnabled(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	extra := []Rule{
		{Name: "fence", Enabled: true, Match: Cond{Op: "and", Conds: []Cond{
			{Type: CondGeofence, Polygon: [][2]float64{{0, 0}, {1, 0}, {1, 1}, {0, 1}}},
			{Type: CondAltitude, Max: fp(5000)}}}},
		{Name: "icao", Enabled: true, Match: Cond{Type: CondICAO, Values: []string{"AE*", "43C000-43CFFF"}}},
		{Name: "calls", Enabled: true, Match: Cond{Type: CondCallsign, Values: []string{"RCH*", "*TEST*", "N?123"}}},
		{Name: "circle", Enabled: true, Match: Cond{Type: CondGeofence, Lat: fp(10), Lon: fp(10), RadiusKm: fp(20)}},
	}
	for _, r := range extra {
		if _, err := e.PutRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.AddWatch(ctx, "spot", 0x111111, ""); err != nil {
		t.Fatal(err)
	}
	acs := make([]model.Aircraft, 300)
	for i := range acs {
		acs[i] = base(fmt.Sprintf("%06X", 0x300000+i))
		acs[i].Lat, acs[i].Lon = fp(50+float64(i)/100), fp(8)
	}
	acs[7].Military = true // continuing match after the first alert
	now := t0
	e.Evaluate(acs, now)
	firedAfterWarmup := e.Stats().Fired
	if firedAfterWarmup != 1 {
		t.Fatalf("warm-up fired %d", firedAfterWarmup)
	}
	allocs := testing.AllocsPerRun(100, func() {
		now = now.Add(time.Second)
		e.Evaluate(acs, now)
	})
	if allocs != 0 {
		t.Fatalf("Evaluate allocates %.1f times per call in steady state", allocs)
	}
	if e.Stats().Fired != firedAfterWarmup {
		t.Fatal("steady state fired")
	}
}

func BenchmarkEvaluate300(b *testing.B) {
	e := New(Options{Logger: quiet})
	acs := make([]model.Aircraft, 300)
	for i := range acs {
		acs[i] = base(fmt.Sprintf("%06X", 0x300000+i))
	}
	now := t0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now = now.Add(time.Second)
		e.Evaluate(acs, now)
	}
}

func TestCheckStatus(t *testing.T) {
	e := New(Options{Logger: quiet})
	ctx := context.Background()
	for _, id := range []string{"builtin-feed-down", "builtin-ntp", "builtin-range-collapse"} {
		if _, err := e.SetEnabled(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	feed := func(conn bool) []model.FeedStatus {
		return []model.FeedStatus{{ID: "rc", Name: "Radarcape", Enabled: true, Connected: conn, LastError: "connection refused"},
			{ID: "off", Enabled: false}}
	}
	st := model.Status{Feeds: feed(true), NTP: model.NTPStatus{Enabled: true, Synced: true, Server: "pool.ntp.org"}}
	e.CheckStatus(st, at(0))
	st.Feeds = feed(false)
	e.CheckStatus(st, at(10))
	e.CheckStatus(st, at(30))
	if e.Stats().Fired != 0 {
		t.Fatal("feed_down before the grace period")
	}
	e.CheckStatus(st, at(71))
	rec := e.Recent(0, 0)
	if len(rec) != 1 || rec[0].Event != SysFeedDown || rec[0].EventKey != "rc" || !strings.Contains(rec[0].Text, "Radarcape down for 61 s: connection refused") ||
		rec[0].Value == nil || *rec[0].Value != 61 || rec[0].Unit != "s" {
		t.Fatalf("feed down: %+v", rec)
	}
	e.CheckStatus(st, at(80))
	st.Feeds = feed(true)
	e.CheckStatus(st, at(90))
	rec = e.Recent(0, 0)
	if len(rec) != 2 || rec[0].Event != SysFeedUp {
		t.Fatalf("feed up: %+v", rec)
	}
	st.NTP.Synced = false
	e.CheckStatus(st, at(100))
	e.CheckStatus(st, at(105))
	rec = e.Recent(0, 0)
	if len(rec) != 3 || rec[0].Event != SysNTPUnsync {
		t.Fatalf("ntp: %+v", rec)
	}
	st.NTP.Synced = true

	// Range: 7 h at 300 km, then 60 km.
	st.Counts = model.LiveCounts{MaxRangeKm: 300, WithPos: 20}
	start := 3600.0
	m := 0
	for ; m < 7*60; m++ {
		e.CheckStatus(st, at(start+float64(m)*60))
	}
	if s := e.Stats(); s.RangeBaselineKm != 300 || s.RangeRecentKm != 300 {
		t.Fatalf("baseline %+v", s)
	}
	st.Counts.MaxRangeKm = 60
	collapsedAt := -1
	for k := 0; k < 20; k++ {
		e.CheckStatus(st, at(start+float64(m)*60))
		m++
		if r := e.Recent(1, 0)[0]; collapsedAt < 0 && r.Event == SysRangeCollapse {
			collapsedAt = k
			if r.Unit != "km" || r.Value == nil || *r.Value != 60 || r.RefValue == nil || *r.RefValue != 300 {
				t.Fatalf("collapse numbers: %+v", r)
			}
		}
	}
	if collapsedAt < 14 || collapsedAt > 15 {
		t.Fatalf("range collapse detected after %d min", collapsedAt)
	}
	st.Counts.MaxRangeKm = 280
	e.CheckStatus(st, at(start+float64(m)*60))
	if r := e.Recent(1, 0)[0]; r.Event != SysRangeRestored || r.Value == nil || *r.Value != 280 {
		t.Fatalf("restored: %+v", r)
	}
	if n := len(e.Recent(0, 0)); n != 5 {
		t.Fatalf("total %d", n)
	}
}

func TestWebhook(t *testing.T) {
	type got struct {
		body []byte
		hdr  http.Header
	}
	bodies := make(chan got, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		bodies <- got{b, r.Header.Clone()}
	}))
	defer srv.Close()
	r := squawkRule("sq", 60)
	r.Actions.Webhook = srv.URL + "/hook?token=secret"
	e := newTestEngine(t, Options{WebhookTimeout: 2 * time.Second}, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	e.Evaluate([]model.Aircraft{sq("3C6444", "7700")}, at(0))
	var g got
	select {
	case g = <-bodies:
	case <-time.After(5 * time.Second):
		t.Fatal("webhook not delivered")
	}
	var p WebhookPayload
	if err := json.Unmarshal(g.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.App != model.AppName || p.Type != "alert" || p.Alert.ICAO != "3C6444" || p.Alert.RuleID != "sq" || p.Alert.Text == "" {
		t.Fatalf("payload %+v", p)
	}
	if g.hdr.Get("Content-Type") != "application/json" || !strings.HasPrefix(g.hdr.Get("User-Agent"), model.AppName+"/") {
		t.Fatalf("headers %v", g.hdr)
	}
	waitFor(t, func() bool { return e.Stats().WebhookSent == 1 })
	if err := e.TestWebhook(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	if g = <-bodies; !bytes.Contains(g.body, []byte(`"type":"test"`)) {
		t.Fatalf("test payload %s", g.body)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWebhookFailureNoRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	r := squawkRule("sq", 60)
	r.Actions.Webhook = srv.URL + "/path/secret-token"
	e := newTestEngine(t, Options{}, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	e.Evaluate([]model.Aircraft{sq("3C6444", "7700")}, at(0))
	waitFor(t, func() bool { return e.Stats().WebhookFailed == 1 })
	time.Sleep(200 * time.Millisecond)
	s := e.Stats()
	if hits.Load() != 1 || s.WebhookSent != 0 || !strings.Contains(s.LastWebhookError, "500") ||
		strings.Contains(s.LastWebhookError, "secret") {
		t.Fatalf("hits %d stats %+v", hits.Load(), s)
	}
}

func TestWebhookBudgetQueueTimeoutRedirect(t *testing.T) {
	r := squawkRule("sq", 60)
	r.Actions.Webhook = "http://127.0.0.1:1/never"
	var acs []model.Aircraft
	for i := 1; i <= 5; i++ {
		acs = append(acs, sq(fmt.Sprintf("%06X", i), "7700"))
	}
	// Budget: 2 per minute (no Run: posts only queue).
	e := newTestEngine(t, Options{WebhookPerMinute: 2}, r)
	e.Evaluate(acs, at(0))
	queued := 0
	for _, al := range e.Recent(0, 0) {
		if al.Webhook {
			queued++
		}
	}
	if queued != 2 || e.Stats().WebhookDropped != 3 || e.Stats().Fired != 5 {
		t.Fatalf("budget: queued %d stats %+v", queued, e.Stats())
	}
	// Queue overflow.
	e = newTestEngine(t, Options{WebhookQueue: 1, WebhookPerMinute: 100}, r)
	e.Evaluate(acs[:3], at(0))
	if e.Stats().WebhookDropped != 2 {
		t.Fatalf("queue: %+v", e.Stats())
	}
	// Timeout.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	e = newTestEngine(t, Options{WebhookTimeout: 200 * time.Millisecond})
	start := time.Now()
	if err := e.TestWebhook(context.Background(), slow.URL); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("timeout: err %v after %v", err, time.Since(start))
	}
	// Redirects are not followed.
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Store(true) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	if err := e.TestWebhook(context.Background(), redir.URL); err == nil || followed.Load() {
		t.Fatalf("redirect: err %v followed %v", err, followed.Load())
	}
	if err := e.TestWebhook(context.Background(), "file:///etc/passwd"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad url: %v", err)
	}
	if got := redactURL("https://hooks.example.com/services/T0/B0/xyz?k=1"); got != "https://hooks.example.com/..." {
		t.Fatalf("redact %q", got)
	}
	if got := redactURL("https://user:pw@hooks.example.com?k=1"); got != "https://hooks.example.com/..." {
		t.Fatalf("redact userinfo %q", got)
	}
	// Transport errors (*url.Error) repeat the full URL: redacted too, both
	// in the returned error and in Stats().LastWebhookError.
	secret := "http://127.0.0.1:1/hooks/TOKEN-abcdef0123456789?key=QUERYSECRET"
	err := e.TestWebhook(context.Background(), secret)
	if err == nil || strings.Contains(err.Error(), "TOKEN") || strings.Contains(err.Error(), "QUERYSECRET") ||
		!strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("transport error not redacted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	e.mu.Lock()
	e.enqueueWebhook(secret, Alert{RuleID: "x"}, at(0).UnixMilli())
	e.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for e.Stats().WebhookFailed == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if st := e.Stats(); st.WebhookFailed == 0 || strings.Contains(st.LastWebhookError, "TOKEN") || strings.Contains(st.LastWebhookError, "QUERYSECRET") {
		t.Fatalf("lastWebhookError: %+v", st)
	}
}

// Reset (factory reset) forgets history, cooldowns, highlights and counters.
func TestEngineReset(t *testing.T) {
	r := squawkRule("b-crit", 600)
	r.Actions.Highlight = "#ff0000"
	e := newTestEngine(t, Options{}, r)
	e.Evaluate([]model.Aircraft{sq("3C6444", "7700")}, at(0))
	if len(e.Recent(0, 0)) != 1 || len(e.Highlights(at(1))) != 1 || e.Stats().Fired != 1 {
		t.Fatalf("setup: %+v", e.Stats())
	}
	e.Reset()
	if len(e.Recent(0, 0)) != 0 || len(e.Highlights(at(1))) != 0 || e.Stats().Fired != 0 {
		t.Fatalf("after reset: recent %d highlights %d stats %+v", len(e.Recent(0, 0)), len(e.Highlights(at(1))), e.Stats())
	}
	// The cooldown from before the reset no longer suppresses the rule.
	e.Evaluate([]model.Aircraft{sq("3C6444", "7700")}, at(10))
	if rec := e.Recent(0, 0); len(rec) != 1 || rec[0].ID <= 1 {
		t.Fatalf("rule suppressed after reset (or id reused): %+v", rec)
	}
}

func TestParseWriteWatchlist(t *testing.T) {
	in := "\xef\xbb\xbficao,note\r\n" +
		"3C6444,\"Lufthansa, special livery\"\n" +
		"~4ca87c; ITA\n" +
		"# comment\n" +
		"; another comment\n" +
		"\n" +
		"zzz bad\n" +
		"000000\n" +
		"1234567 too long\n" +
		"AE1234\tUS Army \x01 C-17\n" +
		"3c6444 duplicate later\n"
	m, order, bad, err := ParseWatchlist(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if bad != 3 || len(order) != 3 || order[0] != 0x3C6444 || order[1] != 0x4CA87C || order[2] != 0xAE1234 {
		t.Fatalf("order %X bad %d", order, bad)
	}
	if m[0x3C6444] != "duplicate later" || m[0x4CA87C] != "ITA" || m[0xAE1234] != "US Army   C-17" {
		t.Fatalf("notes %v", m)
	}
	_, _, _, err = ParseWatchlist(strings.NewReader("3C6444,\"Lufthansa, x\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m2, _, _, _ := ParseWatchlist(strings.NewReader("3C6444,\"Lufthansa, x\"\n")); m2[0x3C6444] != "Lufthansa, x" {
		t.Fatalf("quoted note %q", m2[0x3C6444])
	}
	entries := []WatchEntry{{ICAO: "3C6444", Note: "a, \"quoted\" note"}, {ICAO: "4CA87C"}}
	for _, f := range []string{"csv", "txt"} {
		var buf bytes.Buffer
		if err := WriteWatchlist(&buf, "spot", entries, f); err != nil {
			t.Fatal(err)
		}
		m, order, bad, err := ParseWatchlist(&buf)
		if err != nil || bad != 0 || len(order) != 2 || m[0x3C6444] != entries[0].Note {
			t.Fatalf("%s roundtrip: %v %d %v\n%s", f, m, bad, err, buf.String())
		}
	}
	if err := WriteWatchlist(io.Discard, "x", nil, "xml"); !errors.Is(err, ErrInvalid) {
		t.Fatal("unknown format")
	}
	long := cleanNote(strings.Repeat("Ã©", 300))
	if len([]rune(long)) != MaxNoteLen {
		t.Fatalf("note length %d", len([]rune(long)))
	}
}

func TestWatchlistEngine(t *testing.T) {
	r := Rule{ID: "w", Name: "Watched", Enabled: true, Match: Cond{Type: CondWatchlist, Values: []string{"spot"}},
		Message: "{ident} {watchlist}: {note}"}
	e := newTestEngine(t, Options{}, r)
	ctx := context.Background()
	if err := e.AddWatch(ctx, "spot", 0x3C6444, "Retro livery"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddWatch(ctx, "bad/name", 1, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad name accepted")
	}
	if err := e.AddWatch(ctx, "spot", 0x1000000, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad icao accepted")
	}
	e.Evaluate([]model.Aircraft{base("3C6444"), base("4CA87C")}, at(0))
	rec := e.Recent(0, 0)
	if len(rec) != 1 || rec[0].Text != "DLH4AB spot: Retro livery" {
		t.Fatalf("watch alert %+v", rec)
	}
	if l, n, ok := e.WatchNote(0x3C6444); !ok || l != "spot" || n != "Retro livery" {
		t.Fatal("WatchNote")
	}
	n, bad, err := e.ImportWatchlist(ctx, "spot", strings.NewReader("4CA87C ITA\nAE1234\nnope\n"), false)
	if err != nil || n != 2 || bad != 1 {
		t.Fatalf("import merge: %d %d %v", n, bad, err)
	}
	if l, _ := e.Watchlist("spot"); len(l) != 3 || l[0].ICAO != "3C6444" || l[2].ICAO != "AE1234" {
		t.Fatalf("list %+v", l)
	}
	if _, _, err := e.ImportWatchlist(ctx, "spot", strings.NewReader("400001\n"), true); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.Watchlist("spot"); len(l) != 1 || l[0].ICAO != "400001" {
		t.Fatalf("replace %+v", l)
	}
	if infos := e.Watchlists(); len(infos) != 1 || infos[0].Name != "spot" || infos[0].Entries != 1 {
		t.Fatalf("infos %+v", infos)
	}
	var buf bytes.Buffer
	if err := e.ExportWatchlist(&buf, "spot", "csv"); err != nil || buf.String() != "icao,note\n400001,\n" {
		t.Fatalf("export %q %v", buf.String(), err)
	}
	if err := e.RemoveWatch(ctx, "spot", 0x3C6444); !errors.Is(err, ErrNotFound) {
		t.Fatal("remove missing")
	}
	if err := e.RemoveWatch(ctx, "spot", 0x400001); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteWatchlist(ctx, "spot"); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteWatchlist(ctx, "spot"); !errors.Is(err, ErrNotFound) {
		t.Fatal("delete twice")
	}
	for i := 0; i < MaxLists; i++ {
		if err := e.AddWatch(ctx, fmt.Sprintf("l%d", i), 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.AddWatch(ctx, "one-too-many", 1, ""); !errors.Is(err, ErrTooMany) {
		t.Fatalf("list limit: %v", err)
	}
}

func TestTestRuleAndImportExport(t *testing.T) {
	e := New(Options{Logger: quiet})
	pic := []model.Aircraft{sq("3C6444", "7700"), sq("4CA87C", "7600"), base("400001")}
	m, err := e.TestRule(Rule{Name: "x", Match: Cond{Type: CondSquawk, Values: []string{"7*"}}}, pic, nil, at(0))
	if err != nil || len(m) != 2 || m[0].ICAO != "3C6444" || m[1].ICAO != "4CA87C" || m[0].Text == "" {
		t.Fatalf("test rule: %+v %v", m, err)
	}
	if e.Stats().Fired != 0 {
		t.Fatal("TestRule must not fire")
	}
	if _, err := e.TestRule(Rule{Name: "x", Match: Cond{Type: "bogus"}}, pic, nil, at(0)); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid test rule")
	}
	m, err = e.TestRule(Rule{Name: "sys", Match: Cond{Type: CondSystem, Values: []string{"feed_down"}}}, pic,
		&SystemEvent{Kind: "feed_down", Key: "rc", Text: "down"}, at(0))
	if err != nil || len(m) != 1 || m[0].Text != "sys: down" {
		t.Fatalf("system test: %+v %v", m, err)
	}

	ctx := context.Background()
	if _, err := e.PutRule(ctx, Rule{ID: "mine", Name: "Mine", Match: Cond{Type: CondReg, Values: []string{"D-*"}}}); err != nil {
		t.Fatal(err)
	}
	data, err := e.ExportRules()
	if err != nil {
		t.Fatal(err)
	}
	e2 := newTestEngine(t, Options{})
	n, err := e2.ImportRules(ctx, data)
	if err != nil || n != len(DefaultRules())+1 || len(e2.Rules()) != n {
		t.Fatalf("import %d %v", n, err)
	}
	r, ok := e2.Rule("mine")
	if !ok || r.Builtin || r.Match.Values[0] != "D-*" {
		t.Fatalf("imported %+v", r)
	}
	if r, _ := e2.Rule("builtin-emergency"); !r.Builtin || !r.Enabled {
		t.Fatal("builtin flag")
	}
	e3 := newTestEngine(t, Options{})
	if _, err := e3.ImportRules(ctx, []byte(`[{"name":"ok","match":{"type":"military"}},{"name":"bad","match":{"type":"nope"}}]`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid import: %v", err)
	}
	if len(e3.Rules()) != 0 {
		t.Fatal("partial import")
	}
	if _, err := e3.ImportRules(ctx, []byte(`{"not":"an array"}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("object import")
	}
	// Rules() returns copies.
	rs := e2.Rules()
	rs[0].Match.Conds = nil
	rs[0].Name = "changed"
	if r, _ := e2.Rule(rs[0].ID); r.Name == "changed" {
		t.Fatal("Rules must return copies")
	}
	if err := e2.DeleteRule(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal("delete missing")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	e := New(Options{Store: st, Logger: quiet})
	if err := e.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.Rules()) != len(DefaultRules()) {
		t.Fatalf("seeded %d", len(e.Rules()))
	}
	created, err := e.PutRule(ctx, Rule{Name: "Mine", Enabled: true, Match: Cond{Type: CondCallsign, Values: []string{"DLH*"}}})
	if err != nil || !strings.HasPrefix(created.ID, "r-") || created.Created == 0 {
		t.Fatalf("create %+v %v", created, err)
	}
	if err := e.DeleteRule(ctx, "builtin-military"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddWatch(ctx, "spot", 0x3C6444, "note"); err != nil {
		t.Fatal(err)
	}
	// Log action writes a store event.
	e.Evaluate([]model.Aircraft{sq("3C6444", "7700")}, time.Now())
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	evs, err := st.Events(ctx, store.EventQuery{Kind: "alert"})
	if err != nil || len(evs) != 1 || evs[0].ICAO != 0x3C6444 || !strings.Contains(evs[0].Detail, `"ruleId":"builtin-emergency"`) {
		t.Fatalf("events %+v %v", evs, err)
	}

	e2 := New(Options{Store: st, Logger: quiet})
	if err := e2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := e2.Rule("builtin-military"); ok {
		t.Fatal("deleted default came back")
	}
	if r, ok := e2.Rule(created.ID); !ok || r.Created != created.Created {
		t.Fatalf("custom rule lost: %+v", r)
	}
	if _, n, ok := e2.WatchNote(0x3C6444); !ok || n != "note" {
		t.Fatal("watch-list not persisted")
	}
	if err := e2.ResetDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e2.Rules()) != len(DefaultRules())+1 {
		t.Fatalf("after reset %d", len(e2.Rules()))
	}
	// A corrupt stored rule is skipped, not fatal.
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO alerts_rule(id, json, updated) VALUES('zz', '{"name":', 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e3 := New(Options{Store: st, Logger: quiet})
	if err := e3.Load(ctx); err != nil || len(e3.Rules()) != len(DefaultRules())+1 {
		t.Fatalf("corrupt row: %v %d", err, len(e3.Rules()))
	}
}

func TestConcurrentUse(t *testing.T) {
	e := New(Options{Logger: quiet})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			e.Evaluate([]model.Aircraft{sq(fmt.Sprintf("%06X", i+1), "7700")}, at(float64(i)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_, _ = e.PutRule(ctx, Rule{ID: "c", Name: "c", Enabled: i%2 == 0, Match: Cond{Type: CondMilitary}})
			_ = e.AddWatch(ctx, "x", uint32(i+1), "")
		}
	}()
	go func() {
		defer wg.Done()
		ch, unsub := e.Subscribe(1)
		defer unsub()
		for i := 0; i < 100; i++ {
			_ = e.Recent(10, 0)
			_ = e.Highlights(at(float64(i)))
			_ = e.Stats()
			select {
			case <-ch:
			default:
			}
		}
	}()
	wg.Wait()
}
