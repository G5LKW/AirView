package federation

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"airview/internal/avf"
	mgeo "airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

func ip(v int) *int { return &v }

func fullAircraft() publicdto.Aircraft {
	g := true
	return publicdto.Aircraft{
		ICAO: "4840D6", AddrType: "icao", Callsign: "KLM1023", Squawk: "7700", Category: "A3",
		Lat: f64(-33.9461234), Lon: f64(151.1772345), Source: model.SrcADSR, PosAge: f64(1.234), OnGround: &g,
		AltBaro: f64(-125), AltGeom: f64(37025), VertRate: f64(-2112), SelAlt: f64(36000), QNH: f64(1013.2),
		GS: f64(451.3), Track: f64(359.99), IAS: f64(280), TAS: f64(460), Mach: f64(0.792), MagHeading: f64(12.25),
		Roll: f64(-12.5), TrackRate: f64(-0.31), SelHeading: f64(270.5),
		WindDir: f64(245.5), WindSpd: f64(61.2), OAT: f64(-56.4),
		ADSBVersion: ip(2), NIC: ip(8), NACp: ip(9), NACv: ip(1), SIL: ip(3), RcM: f64(186),
		Emergency: "general", Alert: true, SPI: true, Military: true, Coarse: true,
		NavModes:  []string{"autopilot", "tcas", "unknown-mode"},
		FirstSeen: 1_700_000_000_000 - 3_600_000, LastSeen: 1_700_000_000_000 - 1500,
		MLATGrade: "A", MLATSigmaM: f64(37),
	}
}

func TestStateRoundTrip(t *testing.T) {
	tMs := int64(1_700_000_000_000)
	in := fullAircraft()
	path := []string{newID(t).FeederID(), newID(t).FeederID()}
	p, n := AppendState(nil, tMs, true, path, []publicdto.Aircraft{in, {ICAO: "~00ABCD", Source: model.SrcModeS, LastSeen: tMs}})
	if n != 2 {
		t.Fatal(n)
	}
	sb, err := DecodeState(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sb.TMs != tMs || !sb.Full || !reflect.DeepEqual(sb.Path, path) || len(sb.Aircraft) != 2 {
		t.Fatalf("%+v", sb)
	}
	want := in
	want.NavModes = []string{"autopilot", "tcas"}
	want.FirstSeen = in.LastSeen - (in.LastSeen-in.FirstSeen)/1000*1000 // whole seconds before LastSeen
	got := sb.Aircraft[0]
	wj, _ := json.Marshal(want)
	gj, _ := json.Marshal(got)
	if !bytes.Equal(wj, gj) {
		t.Fatalf("round trip\nwant %s\n got %s", wj, gj)
	}
	if sb.Aircraft[1].ICAO != "~00ABCD" || sb.Aircraft[1].Lat != nil {
		t.Fatalf("%+v", sb.Aircraft[1])
	}
}

func TestStateSplitsLargeBatches(t *testing.T) {
	ac := make([]publicdto.Aircraft, 10_000)
	for i := range ac {
		ac[i] = fullAircraft()
		ac[i].ICAO = avf.FormatICAO(uint32(i), false)
	}
	path := []string{newID(t).FeederID()}
	total := 0
	for rest := ac; len(rest) > 0; {
		p, n := AppendState(nil, 1, false, path, rest)
		if len(p) > MaxRecordLen || n == 0 {
			t.Fatalf("payload %d bytes, %d items", len(p), n)
		}
		sb, err := DecodeState(p, nil)
		if err != nil || len(sb.Aircraft) != n {
			t.Fatal(err)
		}
		total += n
		rest = rest[n:]
	}
	if total != len(ac) {
		t.Fatal(total)
	}
}

func TestDecodeHostile(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	good, _ := AppendState(nil, 5, false, []string{newID(t).FeederID()}, []publicdto.Aircraft{fullAircraft()})
	goodM, _ := AppendMLAT(nil, 5, []string{newID(t).FeederID()}, []Fix{{ICAO: 1, Vel: &[3]float32{1, 2, 3}}})
	for i := 0; i < 20000; i++ {
		var b []byte
		switch i % 3 {
		case 0:
			b = make([]byte, r.IntN(200))
			for j := range b {
				b[j] = byte(r.Uint32())
			}
		case 1:
			b = append([]byte(nil), good[:r.IntN(len(good))]...)
		default:
			b = append([]byte(nil), good...)
			b[r.IntN(len(b))] ^= byte(1 + r.IntN(255))
		}
		_, _ = DecodeState(b, nil) // must not panic
		_, _ = DecodeMLAT(b, nil)
		if i%2 == 0 {
			m := append([]byte(nil), goodM...)
			m[r.IntN(len(m))] ^= 0xFF
			_, _ = DecodeMLAT(m, nil)
		}
	}
	// Truncations are errors.
	for k := 0; k < len(good)-1; k++ {
		if _, err := DecodeState(good[:k], nil); err == nil {
			t.Fatalf("truncated at %d accepted", k)
		}
	}
}

func TestRecordReaderSkipsUnknown(t *testing.T) {
	var b []byte
	b = appendRecord(b, 0x77, []byte("future"))
	b = appendRecord(b, TPing, appendPing(nil, 9, 42))
	rr := newRecordReader(bytes.NewReader(b))
	ty, p, err := rr.next()
	if err != nil || ty != TPing || rr.skipped != 1 {
		t.Fatal(ty, err)
	}
	id, ns, _ := decodePing(p)
	if id != 9 || ns != 42 {
		t.Fatal(id, ns)
	}
	if _, _, err := rr.next(); err != io.EOF {
		t.Fatal(err)
	}
	over := []byte{byte(TState), 0xFF, 0xFF, 0x7F}
	if _, _, err := newRecordReader(bytes.NewReader(over)).next(); err == nil {
		t.Fatal("oversized record accepted")
	}
}

func TestMLATRoundTripAndConversion(t *testing.T) {
	lat, lon, h := 47.123456, -122.654321, 3048.0
	e := mgeo.ToECEF(lat, lon, h)
	// Covariance: 30 m east, 40 m north, 60 m up, rotated to ECEF.
	sl, cl := math.Sincos(lat * math.Pi / 180)
	so, co := math.Sincos(lon * math.Pi / 180)
	R := [3][3]float64{{-so, -sl * co, cl * co}, {co, -sl * so, cl * so}, {0, cl, sl}} // columns e, n, u
	d := [3]float64{30 * 30, 40 * 40, 60 * 60}
	var C [3][3]float64
	for i := range 3 {
		for j := range 3 {
			for k := range 3 {
				C[i][j] += R[i][k] * d[k] * R[j][k]
			}
		}
	}
	m := model.MLATFix{ICAO: 0xABC123, TUnixNs: 1_700_000_000_123_456_789, ECEF: [3]float64{e.X, e.Y, e.Z},
		Cov: [6]float32{float32(C[0][0]), float32(C[0][1]), float32(C[0][2]), float32(C[1][1]), float32(C[1][2]), float32(C[2][2])},
		Vel: &[3]float32{100, 0, -5}, Grade: model.GradeA, NBucket: model.NBucket9Plus, Flags: model.FixFlagTracked}
	f, ok := FixFromModel(&m)
	if !ok {
		t.Fatal("not converted")
	}
	if math.Abs(f.Lat-lat) > 1e-9 || math.Abs(f.Lon-lon) > 1e-9 || math.Abs(f.AltM-h) > 1e-3 {
		t.Fatalf("llh %+v", f)
	}
	if math.Abs(f.SigmaHM-50) > 0.05 || math.Abs(f.SigmaVM-60) > 0.05 || f.TMs != 1_700_000_000_123 {
		t.Fatalf("sigma %v %v t %d", f.SigmaHM, f.SigmaVM, f.TMs)
	}
	for _, bad := range []model.MLATFix{{Grade: model.GradeD}, {Grade: model.GradeA, Flags: model.FixFlagHoldout}} {
		if _, ok := FixFromModel(&bad); ok {
			t.Fatal("undisplayable / hold-out fix shared")
		}
	}
	path := []string{newID(t).FeederID()}
	p, n := AppendMLAT(nil, 1_700_000_000_000, path, []Fix{f})
	mb, err := DecodeMLAT(p, nil)
	if err != nil || n != 1 || len(mb.Fixes) != 1 {
		t.Fatal(err)
	}
	g := mb.Fixes[0]
	if g.TMs != f.TMs || math.Abs(g.Lat-f.Lat) > 1e-7 || math.Abs(g.AltM-f.AltM) > 0.05 || math.Abs(g.SigmaHM-f.SigmaHM) > 0.05 ||
		g.Grade != f.Grade || g.NBucket != f.NBucket || g.Flags != f.Flags || g.Vel == nil || g.Vel[0] != 100 || g.Vel[2] != -5 {
		t.Fatalf("%+v vs %+v", g, f)
	}
	u := updateFromFix(&g, "x")
	if math.Abs(*u.GS-194.38) > 0.01 || math.Abs(*u.Track-90) > 1e-9 || math.Abs(*u.VertRate+984.25) > 0.01 {
		t.Fatalf("velocity %v %v %v", *u.GS, *u.Track, *u.VertRate)
	}
}
