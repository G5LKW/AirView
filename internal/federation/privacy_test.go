package federation

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/privacy"
	"airview/internal/privacy/publicdto"
)

// Fixture secrets of network A: exact station positions, SIDs, feeder
// IDs, a UUID and IPs. None of them may reach a peer or an out-feed.
var (
	stationA   = privacy.Point{Lat: 53.48123, Lon: -2.24456, Strict: true, Label: "hidden station"}
	stationB   = privacy.Point{Lat: 52.20517, Lon: 0.11931, Strict: true, Label: "public station"}
	secretSIDs = []string{"k3m2q7xw4vb5ny6a", "p9t8r7e6w5q4z3y2"}
	secretFeed = "avf:" + "ab4cd5ef6gh7ij2k"
	secretUUID = "6f1c2a7e-3b4d-4e5f-9a8b-7c6d5e4f3a2b"
)

func leakyLocal() []model.Aircraft {
	low := plane("40ABCD", "GABCD", 53.4801, -2.2399, 600)
	high := plane("4CA123", "RYR12", 52.9, -1.5, 37000)
	for _, a := range []*model.Aircraft{&low, &high} {
		a.DistKm, a.Bearing, a.Elevation, a.SlantKm = f64(1.234), f64(271.5), f64(3.25), f64(1.9)
		a.CPAKm, a.CPAInS, a.LOSMarginDeg = f64(0.8), f64(31), f64(1.5)
		tv := true
		a.TerrainVisible = &tv
		a.RSSI, a.RSSIAvg = f64(-12.3), f64(-14.1)
		a.Feeds = []string{secretFeed, "beast-" + secretUUID}
		a.Antennas = []string{"ant-" + secretSIDs[0]}
		a.Stations = secretSIDs
		a.Mine = true
		a.Registration, a.Operator = "G-ABCD", "Owner Ltd"
		a.Msgs, a.MsgRate = 1234, 5.5
	}
	pred := plane("4CA999", "EIN5", 0, 0, 25000)
	pred.Predicted, pred.LastRealLat, pred.LastRealLon, pred.CoastS = true, f64(52.1), f64(-0.9), f64(6)
	pred.Lat, pred.Lon = f64(52.12), f64(-0.95)
	// Heard only through federation: must never be exported again.
	echo := plane("AAAAAA", "ECHO1", 50, 0, 20000)
	echo.Feeds = []string{FeedPrefix + "zzzzzzzzzzzzzzzz"}
	return []model.Aircraft{low, high, pred, echo}
}

func fixture() privacy.Fixture {
	return privacy.Fixture{Points: []privacy.Point{stationA, stationB}, SIDs: secretSIDs,
		Secrets: []string{secretFeed, secretUUID, "ab4cd5ef6gh7ij2k"}, IPs: []string{"192.0.2.44", "2001:db8::44"}}
}

func scan(t *testing.T, what string, f privacy.Fixture, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if leaks := f.Scan(b); len(leaks) > 0 {
		t.Fatalf("%s leaks: %v\n%s", what, leaks, b)
	}
}

// TestPrivacy: what a peer receives from network A (state, sink updates,
// INFO, status) and what A's JSON out-feed emits carries no receiver data,
// island aircraft are coarsened (or dropped), predicted positions are not
// shared and federation-only aircraft are not re-exported.
func TestPrivacy(t *testing.T) {
	ida, idb, idc := newID(t), newID(t), newID(t)
	guard := func(a *model.Aircraft) bool { return a.ICAO == "40ABCD" }
	a := startNode(t, ida, "net-a", "auto", Config{Peers: []PeerConfig{
		{Name: "b", ID: idb.FeederID(), Send: Policy{MLAT: true}},
		{Name: "c", ID: idc.FeederID(), Send: Policy{Island: IslandDrop, NoMilitary: true}}}},
		Options{Guard: guard})
	b := startNode(t, idb, "net-b", "", Config{Peers: []PeerConfig{{Name: "a", ID: ida.FeederID(), Addr: a.Addr().String()}}}, Options{})
	c := startNode(t, idc, "net-c", "", Config{Peers: []PeerConfig{{Name: "a", ID: ida.FeederID(), Addr: a.Addr().String()}}}, Options{})
	a.pic.set(leakyLocal()...)
	waitFor(t, 5*time.Second, "B and C receive", func() bool {
		return hasAll(b.Node, "40ABCD", "4CA123", "4CA999") && hasAll(c.Node, "4CA123", "4CA999")
	})
	time.Sleep(300 * time.Millisecond)
	f := fixture()

	rb := b.RemoteAircraft()
	scan(t, "B remote aircraft", f, rb)
	scan(t, "B sink updates", f, b.sink.all())
	scan(t, "C remote aircraft", f, c.RemoteAircraft())
	for _, x := range rb {
		switch x.ICAO {
		case "40ABCD":
			if !x.Coarse || !onGrid(*x.Lat) || !onGrid(*x.Lon) {
				t.Fatalf("island aircraft not coarsened: %v %v coarse=%v", *x.Lat, *x.Lon, x.Coarse)
			}
		case "4CA999":
			if *x.Lat != 52.1 || *x.Lon != -0.9 || x.Predicted || x.LastRealLat != nil {
				t.Fatalf("predicted position shared: %+v", x)
			}
		case "AAAAAA":
			t.Fatal("federation-only aircraft re-exported")
		}
		if x.Registration != "" || x.Operator != "" {
			t.Fatal("reference data exchanged")
		}
	}
	if remoteByICAO(c.Node)["40ABCD"] {
		t.Fatal("island aircraft sent to a peer whose policy drops it")
	}
	// The status of B (admin scope) names its own peer address only.
	sf := f
	sf.AllowIPs = []string{"127.0.0.1"}
	scan(t, "B status", sf, b.Status())
	// Peer INFO as received by B.
	scan(t, "INFO", f, peerState(b.Node, "a").PeerInfo)

	// The JSON out-feed rendering (tar1090 shape), default policy and drop.
	for _, p := range []Policy{{}, {Island: IslandDrop}} {
		p.normalize()
		var file publicdto.TarAircraftFile
		raw := a.AircraftJSON(&p)
		if leaks := f.Scan(raw); len(leaks) > 0 {
			t.Fatalf("out-feed JSON leaks: %v\n%s", leaks, raw)
		}
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}
		for _, x := range file.Aircraft {
			if x.Hex == "40abcd" && p.Island == IslandDrop {
				t.Fatal("island aircraft in a dropping out-feed")
			}
			if x.Hex == "aaaaaa" {
				t.Fatal("federation-only aircraft in the out-feed")
			}
		}
	}
}

func onGrid(v float64) bool {
	k := v / privacy.IslandGridDeg
	return math.Abs(k-math.Round(k)) < 1e-6
}

// TestWireHasNoReceiverFields: the encoder works on publicdto.Aircraft,
// whose type cannot hold a receiver field; and every field it encodes is a
// field of that public type.
func TestWireHasNoReceiverFields(t *testing.T) {
	ty := reflect.TypeFor[publicdto.Aircraft]()
	for i := range ty.NumField() {
		tag := strings.Split(ty.Field(i).Tag.Get("json"), ",")[0]
		for _, k := range privacy.ForbiddenPublicKeys {
			if strings.EqualFold(tag, k) {
				t.Fatalf("publicdto.Aircraft has forbidden field %s", tag)
			}
		}
	}
	for _, ty := range []reflect.Type{reflect.TypeFor[Fix](), reflect.TypeFor[Info](), reflect.TypeFor[Hello](), reflect.TypeFor[AuthBody]()} {
		for i := range ty.NumField() {
			name := strings.ToLower(ty.Field(i).Name)
			for _, bad := range []string{"station", "feeder", "receiver", "rssi", "dist", "bearing", "ip", "uuid", "sid"} {
				if name == bad || strings.HasPrefix(name, bad) {
					t.Fatalf("%s.%s looks like receiver data", ty.Name(), ty.Field(i).Name)
				}
			}
		}
	}
}
