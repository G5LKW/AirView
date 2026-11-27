package feed

import (
	"math"
	"testing"
	"time"

	"airview/internal/model"
)

const readsbDoc1 = `{ "now" : 1794398400.1,
  "messages" : 123456,
  "aircraft" : [
    {"hex":"4840d6","type":"adsb_icao","flight":"KLM1023 ","alt_baro":38000,"alt_geom":38450,"gs":447.5,"track":271.3,"baro_rate":-64,"squawk":"7000","category":"A3","lat":52.257202,"lon":3.919373,"nic":8,"rc":186,"seen_pos":0.3,"version":2,"mlat":[],"tisb":[],"messages":812,"seen":0.1,"rssi":-18.4},
    {"hex":"3c6dd2","type":"mode_s","alt_baro":"ground","squawk":"1000","mlat":[],"tisb":[],"messages":25,"seen":0.9,"rssi":-27.1},
    {"hex":"~2a1b3c","type":"tisb_trackfile","alt_baro":1200,"lat":40.1,"lon":-74.2,"seen_pos":20.5,"messages":10,"seen":0.4,"rssi":-30.0},
    {"hex":"400f2b","type":"mlat","flight":"BAW12   ","alt_baro":12000,"gs":300,"track":90,"lat":51.5,"lon":-0.5,"seen_pos":0.8,"mlat":["lat","lon","gs","track"],"tisb":[],"messages":300,"seen":0.2,"rssi":-22.0},
    {"hex":"a1b2c3","alt_baro":30000,"messages":5,"seen":45.0,"rssi":-33.0},
    {"hex":"zzzzzz","messages":1,"seen":0.1},
    {"flight":"NOHEX"}
  ]}`

const readsbDoc2 = `{"now":1794398401.1,"messages":123500,"aircraft":[
    {"hex":"4840d6","type":"adsb_icao","flight":"KLM1023 ","alt_baro":37975,"gs":447.0,"track":271.4,"geom_rate":-128,"squawk":"7000","lat":52.2573,"lon":3.9170,"seen_pos":4.0,"messages":820,"seen":0.2,"rssi":-18.9},
    {"hex":"3c6dd2","type":"mode_s","alt_baro":"ground","squawk":"1000","messages":25,"seen":1.9,"rssi":-27.1},
    {"hex":"a1b2c3","alt_baro":30100,"messages":6,"seen":0.5,"rssi":-32.0}
  ]}`

// Older dump1090-mutability field names.
const mutabilityDoc = `{"now":1794398402,"aircraft":[
    {"hex":"4ca4e5","squawk":"2254","flight":"RYR1427 ","lat":53.1,"lon":-6.2,"seen_pos":0.4,"altitude":36000,"vert_rate":64,"track":180,"speed":420,"messages":44,"seen":0.1,"rssi":-12.5},
    {"hex":"4ca4e6","altitude":"ground","messages":3,"seen":0.6,"rssi":-3.1}
  ]}`

const radarcapeDoc1 = `[{"uti":1794398400,"ns":120000000,"hex":"3C6DD2","fli":"DLH4AB","src":"A","ava":"A","lat":48.3531,"lon":11.7861,"alt":36000,"altg":36475,"gda":"A","spd":452,"trk":123.4,"vrt":0,"squ":"1000","cat":"A3","dbm":-72,"lla":0,"tru":1503,"cou":"Germany","dis":12.3},
 {"uti":1794398399,"ns":900000000,"hex":"4B1803","fli":null,"src":"M","ava":"M","lat":47.9,"lon":11.2,"alt":12000,"gda":"A","spd":250,"trk":10,"vrt":-800,"squ":4521,"dbm":-80,"lla":1,"tru":88},
 {"uti":1794398390,"ns":0,"hex":"DD1234","src":"S","lat":47.8,"lon":11.1,"alt":1500,"tru":0},
 {"uti":1794398398,"ns":0,"hex":"400F2B","src":"A","lat":null,"lon":null,"alt":null,"gda":"G","spd":12,"trk":93,"squ":"0420","dbm":-65.5,"lla":30,"tru":40}]`

const radarcapeDoc2 = `[{"uti":1794398401,"ns":220000000,"hex":"3C6DD2","fli":"DLH4AB","src":"A","lat":48.3540,"lon":11.7880,"alt":36000,"gda":"A","spd":452,"trk":123.4,"vrt":0,"squ":"1000","dbm":-71,"lla":0,"tru":1510},
 {"uti":1794398399,"ns":900000000,"hex":"4B1803","src":"M","lat":47.9,"lon":11.2,"alt":12000,"gda":"A","lla":2,"tru":88}]`

func byICAO(us []model.DecodedUpdate) map[uint32]model.DecodedUpdate {
	m := make(map[uint32]model.DecodedUpdate, len(us))
	for _, u := range us {
		m[u.ICAO] = u
	}
	return m
}

func val(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func TestJSONDecoderReadsb(t *testing.T) {
	out := &sink{}
	d := &JSONDecoder{Sink: out, FeedID: "json", Interval: time.Second}
	now := time.Unix(1794398400, 100_000_000)
	if err := d.Decode([]byte(readsbDoc1), now); err != nil {
		t.Fatal(err)
	}
	if d.Format != JSONReadsb || d.Aircraft != 7 {
		t.Errorf("format %q aircraft %d", d.Format, d.Aircraft)
	}
	if len(out.updates) != 4 {
		for _, u := range out.updates {
			t.Log(dumpUpdate(u))
		}
		t.Fatalf("first poll: %d updates, want 4", len(out.updates))
	}
	m := byICAO(out.updates)

	klm := m[0x4840D6]
	if klm.Callsign == nil || *klm.Callsign != "KLM1023" || klm.Squawk == nil || *klm.Squawk != "7000" ||
		val(klm.Lat) != 52.257202 || val(klm.Lon) != 3.919373 || val(klm.AltBaro) != 38000 || val(klm.AltGeom) != 38450 ||
		val(klm.GS) != 447.5 || val(klm.Track) != 271.3 || val(klm.VertRate) != -64 || val(klm.Signal) != -18.4 ||
		klm.OnGround == nil || *klm.OnGround || klm.MLAT || klm.Msgs != 1 || klm.FeedID != "json" {
		t.Errorf("KLM: %s", dumpUpdate(klm))
	}
	if want := now.Add(-100 * time.Millisecond); !klm.Recv.Equal(want) {
		t.Errorf("KLM Recv = %v, want now-seen = %v", klm.Recv, want)
	}
	if g := m[0x3C6DD2]; g.OnGround == nil || !*g.OnGround || g.AltBaro != nil || g.Lat != nil {
		t.Errorf("ground aircraft: %s", dumpUpdate(g))
	}
	if tis, ok := m[0x2A1B3C|NonICAOFlag]; !ok || tis.Lat != nil || val(tis.AltBaro) != 1200 {
		t.Errorf("non-ICAO aircraft with a stale position: %s (present %v)", dumpUpdate(tis), ok)
	}
	if ml := m[0x400F2B]; !ml.MLAT || val(ml.Lat) != 51.5 || ml.Callsign == nil || *ml.Callsign != "BAW12" {
		t.Errorf("MLAT aircraft: %s", dumpUpdate(ml))
	}
	if _, ok := m[0xA1B2C3]; ok {
		t.Error("aircraft last heard 45 s ago was reported as new")
	}

	// Second poll: only aircraft with new messages produce updates.
	out.updates = nil
	if err := d.Decode([]byte(readsbDoc2), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(out.updates) != 2 {
		t.Fatalf("second poll: %d updates, want 2", len(out.updates))
	}
	m = byICAO(out.updates)
	klm = m[0x4840D6]
	if klm.Msgs != 8 || klm.Lat != nil || val(klm.AltBaro) != 37975 || val(klm.VertRate) != -128 {
		t.Errorf("KLM second poll (8 new messages, position 4 s old): %s", dumpUpdate(klm))
	}
	if a := m[0xA1B2C3]; a.Msgs != 1 || val(a.AltBaro) != 30100 {
		t.Errorf("A1B2C3: %s", dumpUpdate(a))
	}
	if len(d.seen) != 3 {
		t.Errorf("state map holds %d aircraft, want 3 (departed ones must be forgotten)", len(d.seen))
	}

	// An unchanged document produces nothing.
	out.updates = nil
	d.Decode([]byte(readsbDoc2), now.Add(2*time.Second))
	if len(out.updates) != 0 {
		t.Errorf("unchanged poll produced %d updates", len(out.updates))
	}
}

func TestJSONDecoderOldDump1090(t *testing.T) {
	out := &sink{}
	d := &JSONDecoder{Sink: out, MLATOnly: true}
	if err := d.Decode([]byte(mutabilityDoc), time.Now()); err != nil {
		t.Fatal(err)
	}
	m := byICAO(out.updates)
	r := m[0x4CA4E5]
	if val(r.AltBaro) != 36000 || val(r.GS) != 420 || val(r.VertRate) != 64 || val(r.Track) != 180 ||
		r.Squawk == nil || *r.Squawk != "2254" || val(r.Lat) != 53.1 || !r.MLAT {
		t.Errorf("old-style fields: %s", dumpUpdate(r))
	}
	if g := m[0x4CA4E6]; g.OnGround == nil || !*g.OnGround {
		t.Errorf("old-style ground: %s", dumpUpdate(g))
	}
}

func TestJSONDecoderRadarcape(t *testing.T) {
	out := &sink{}
	d := &JSONDecoder{Sink: out, FeedID: "rc-json"}
	now := time.Unix(1794398400, 500_000_000)
	if err := d.Decode([]byte(radarcapeDoc1), now); err != nil {
		t.Fatal(err)
	}
	if d.Format != JSONRadarcape || d.Aircraft != 4 {
		t.Errorf("format %q aircraft %d", d.Format, d.Aircraft)
	}
	if len(out.updates) != 3 {
		t.Fatalf("first poll: %d updates, want 3 (FLARM entry skipped)", len(out.updates))
	}
	m := byICAO(out.updates)
	dlh := m[0x3C6DD2]
	if dlh.Callsign == nil || *dlh.Callsign != "DLH4AB" || dlh.Squawk == nil || *dlh.Squawk != "1000" ||
		val(dlh.Lat) != 48.3531 || val(dlh.Lon) != 11.7861 || val(dlh.AltBaro) != 36000 || val(dlh.AltGeom) != 36475 ||
		val(dlh.GS) != 452 || val(dlh.Track) != 123.4 || val(dlh.VertRate) != 0 || val(dlh.Signal) != -72 ||
		dlh.OnGround == nil || *dlh.OnGround || dlh.MLAT || !dlh.Recv.Equal(now) || dlh.FeedID != "rc-json" {
		t.Errorf("DLH: %s", dumpUpdate(dlh))
	}
	ml := m[0x4B1803]
	if !ml.MLAT || ml.Callsign != nil || ml.Squawk == nil || *ml.Squawk != "4521" || val(ml.Lat) != 47.9 || val(ml.VertRate) != -800 {
		t.Errorf("MLAT aircraft: %s", dumpUpdate(ml))
	}
	g := m[0x400F2B]
	if g.OnGround == nil || !*g.OnGround || g.Lat != nil || g.AltBaro != nil || g.Squawk == nil || *g.Squawk != "0420" || val(g.Signal) != -65.5 {
		t.Errorf("ground aircraft with nulls: %s", dumpUpdate(g))
	}
	if _, ok := m[0xDD1234]; ok {
		t.Error("FLARM target reported as Mode S aircraft")
	}

	out.updates = nil
	if err := d.Decode([]byte(radarcapeDoc2), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(out.updates) != 1 || out.updates[0].ICAO != 0x3C6DD2 || out.updates[0].Msgs != 7 {
		t.Fatalf("second poll: %d updates", len(out.updates))
	}

	// The same list wrapped in an object is understood too.
	out.updates = nil
	d2 := &JSONDecoder{Sink: out}
	if err := d2.Decode([]byte(`{"aircraftlist":`+radarcapeDoc1+`}`), now); err != nil {
		t.Fatal(err)
	}
	if len(out.updates) != 3 || d2.Format != JSONRadarcape {
		t.Errorf("wrapped list: %d updates, format %q", len(out.updates), d2.Format)
	}
}

func TestJSONDecoderErrors(t *testing.T) {
	d := &JSONDecoder{Sink: &sink{}}
	for _, doc := range []string{"", "   ", "<html>404</html>", `{"now":1}`, `{"aircraft":`, `[{"hex":`} {
		if err := d.Decode([]byte(doc), time.Now()); err == nil {
			t.Errorf("Decode(%q) succeeded", doc)
		}
	}
	// Empty lists are fine.
	if err := d.Decode([]byte(`[]`), time.Now()); err != nil || d.Format != JSONRadarcape {
		t.Errorf("empty array: %v, format %q", err, d.Format)
	}
	d = &JSONDecoder{Sink: &sink{}}
	if err := d.Decode([]byte("\xEF\xBB\xBF"+`{"now":1,"aircraft":[]}`), time.Now()); err != nil || d.Format != JSONReadsb {
		t.Errorf("empty readsb list: %v, format %q", err, d.Format)
	}
}

func TestJSONCandidates(t *testing.T) {
	cases := []struct {
		cfg   model.Feed
		first string
		n     int
		bad   bool
	}{
		{model.Feed{URL: "http://pi/tar1090/data/aircraft.json"}, "http://pi/tar1090/data/aircraft.json", 1, false},
		{model.Feed{URL: "radarcape/aircraftlist.json"}, "http://radarcape/aircraftlist.json", 1, false},
		{model.Feed{URL: "http://radarcape/"}, "http://radarcape/aircraftlist.json", len(jsonProbePaths), false},
		{model.Feed{Host: "radarcape"}, "http://radarcape/aircraftlist.json", len(jsonProbePaths), false},
		{model.Feed{Host: "10.0.0.5", Port: 8080}, "http://10.0.0.5:8080/aircraftlist.json", len(jsonProbePaths), false},
		{model.Feed{Host: "fe80::1", Port: 80}, "http://[fe80::1]/aircraftlist.json", len(jsonProbePaths), false},
		{model.Feed{URL: "http://pi/list?format=json"}, "http://pi/list?format=json", 1, false},
		{model.Feed{}, "", 0, true},
		{model.Feed{URL: "ftp://pi/aircraft.json"}, "", 0, true},
	}
	for _, c := range cases {
		got, err := jsonCandidates(c.cfg)
		if c.bad {
			if err == nil {
				t.Errorf("%+v: expected an error, got %v", c.cfg, got)
			}
			continue
		}
		if err != nil || len(got) != c.n || got[0] != c.first {
			t.Errorf("%+v: got %v (%v), want first %q of %d", c.cfg, got, err, c.first, c.n)
		}
	}
}
