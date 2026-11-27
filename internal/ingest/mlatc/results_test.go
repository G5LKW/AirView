package mlatc

import (
	"encoding/json"
	"math"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

func TestResultECEF(t *testing.T) {
	p := geo.ToECEF(51.4775, -0.4614, 3048) // 10,000 ft HAE
	vel := [3]float32{100, -50, 5}          // m/s E, N, U
	r := Result{MLATFix: model.MLATFix{ICAO: 0x4840d6, TUnixNs: 1_700_000_000_123_456_789,
		ECEF: [3]float64{p.X, p.Y, p.Z}, Cov: [6]float32{400, 10, -5, 900, 3, 2500}, Vel: &vel,
		NBucket: model.NBucket5to8}, Callsign: "klm12a ", Squawk: "7000"}
	line := appendResult(nil, &r, true)
	var m struct {
		Result struct {
			At       float64    `json:"@"`
			Addr     string     `json:"addr"`
			ECEF     [3]float64 `json:"ecef"`
			Cov      [6]float64 `json:"cov"`
			ND       int        `json:"nd"`
			EW, NS   float64
			VRate    float64 `json:"vrate"`
			Callsign string  `json:"callsign"`
			Squawk   string  `json:"squawk"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	var raw map[string]map[string]json.RawMessage
	json.Unmarshal(line, &raw)
	json.Unmarshal(raw["result"]["ewvel"], &m.Result.EW)
	json.Unmarshal(raw["result"]["nsvel"], &m.Result.NS)
	res := m.Result
	if res.Addr != "4840d6" || math.Abs(res.At-1_700_000_000.123) > 1e-6 || res.ND != 5 ||
		res.Callsign != "KLM12A" || res.Squawk != "7000" {
		t.Fatalf("fields: %s", line)
	}
	for i := 0; i < 3; i++ {
		if math.Abs(res.ECEF[i]-r.ECEF[i]) > 0.051 {
			t.Fatalf("ecef %d: %v vs %v", i, res.ECEF[i], r.ECEF[i])
		}
	}
	for i := range res.Cov {
		if math.Abs(res.Cov[i]-float64(r.Cov[i])) > 0.006 {
			t.Fatalf("cov %d", i)
		}
	}
	// the client's position (ECEF -> LLH) and error estimate
	llh := geo.FromECEF(geo.Vec3{X: res.ECEF[0], Y: res.ECEF[1], Z: res.ECEF[2]})
	if math.Abs(llh.Lat-51.4775) > 1e-6 || math.Abs(llh.Lon+0.4614) > 1e-6 || math.Abs(llh.H-3048) > 0.1 {
		t.Fatalf("llh %+v", llh)
	}
	if errM := math.Sqrt(res.Cov[0] + res.Cov[3] + res.Cov[5]); math.Abs(errM-math.Sqrt(3800)) > 0.01 {
		t.Fatal("error estimate")
	}
	// knots and ft/min
	if math.Abs(res.EW-194.4) > 0.06 || math.Abs(res.NS+97.2) > 0.06 || math.Abs(res.VRate-984) > 0.6 {
		t.Fatalf("velocity %v %v %v", res.EW, res.NS, res.VRate)
	}
}

func TestResultLegacy(t *testing.T) {
	p := geo.ToECEF(-33.9, 151.2, 1000)
	r := Result{MLATFix: model.MLATFix{ICAO: 0xabc, TUnixNs: 1e18, ECEF: [3]float64{p.X, p.Y, p.Z},
		Cov: [6]float32{900, 0, 0, 900, 0, 900}}, NStations: 7}
	line := appendResult(nil, &r, false)
	var m struct {
		Result struct {
			Addr      string  `json:"addr"`
			Lat       float64 `json:"lat"`
			Lon       float64 `json:"lon"`
			Alt       float64 `json:"alt"`
			GDOP      float64 `json:"gdop"`
			NStations int     `json:"nstations"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	var raw map[string]map[string]json.RawMessage
	json.Unmarshal(line, &raw)
	res := m.Result
	if res.Addr != "000abc" || math.Abs(res.Lat+33.9) > 1e-6 || math.Abs(res.Lon-151.2) > 1e-6 ||
		math.Abs(res.Alt-1000/0.3048) > 0.6 || res.NStations != 7 {
		t.Fatalf("%s", line)
	}
	// the client's error estimate gdop × 300 equals the 3-D sigma
	if math.Abs(res.GDOP*300-math.Sqrt(2700)) > 0.2 {
		t.Fatalf("gdop %v", res.GDOP)
	}
	if _, ok := raw["result"]["ecef"]; ok {
		t.Fatal("ecef in legacy")
	}
	if _, ok := raw["result"]["callsign"]; ok {
		t.Fatal("empty callsign sent")
	}
}

func TestResultNonFinite(t *testing.T) {
	r := Result{MLATFix: model.MLATFix{ECEF: [3]float64{math.NaN(), math.Inf(1), 0}, Cov: [6]float32{float32(math.NaN())}}}
	for _, e := range []bool{true, false} {
		if line := appendResult(nil, &r, e); !json.Valid(line) {
			t.Fatalf("invalid JSON %s", line)
		}
	}
	if s := cleanIdent("\"\x00ab c\n", 8); s != "AB C" {
		t.Fatalf("cleanIdent %q", s)
	}
}

func TestControlMessages(t *testing.T) {
	for _, b := range [][]byte{
		appendAddrList(nil, "start_sending", []uint32{0x4840d6, 1}),
		appendStats(nil, ClientStats{PeerCount: 12, OutlierPercent: 1.5, BadSyncTimeout: 0}),
		appendHeartbeat(nil, 1_700_000_000_500_000_000),
	} {
		if !json.Valid(b) || b[len(b)-1] != '\n' {
			t.Fatalf("%q", b)
		}
	}
	if s := string(appendAddrList(nil, "stop_sending", []uint32{0x4840d6, 1})); s != `{"stop_sending":["4840d6","000001"]}`+"\n" {
		t.Fatal(s)
	}
}
