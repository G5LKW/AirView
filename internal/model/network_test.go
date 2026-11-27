package model

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// legacyTopKeys are the config.json keys of the stand-alone layout before
// the network contract was added. They must all still be written, unchanged.
var legacyTopKeys = []string{
	"version", "mode", "http", "receiver", "antennas", "feeds", "ntp", "weather", "hub",
	"remote", "storage", "ui", "demo", "prediction", "terrain", "navData", "alerts", "rfStat",
}

func TestDefaultConfigIsStandalone(t *testing.T) {
	c := DefaultConfig()
	if c.Mode != ModeStandalone || len(c.Uplinks) != 0 {
		t.Fatalf("default config is not plain standalone: mode %q uplinks %d", c.Mode, len(c.Uplinks))
	}
	if !c.Aggregator.IsDefault() {
		t.Fatal("default config does not carry the default aggregator block")
	}
}

func TestDefaultConfigJSONRoundTrip(t *testing.T) {
	data, err := json.Marshal(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var z Config // decoding into a zero value, as PUT handlers and tests do
	if err := json.Unmarshal(data, &z); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(z, DefaultConfig()) {
		t.Fatalf("round trip differs:\n%+v\n%+v", z, DefaultConfig())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range legacyTopKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("legacy key %q missing", k)
		}
	}
	for k := range raw {
		if !slices.Contains(legacyTopKeys, k) && k != "aggregator" && k != "uplinkTuning" && k != "update" && k != "locate" {
			t.Errorf("unexpected new top-level key %q in a standalone config", k)
		}
	}
	// Feeds of a standalone config must not grow new keys.
	if bytes.Contains(raw["feeds"], []byte("noForward")) || bytes.Contains(raw["feeds"], []byte("clockClass")) {
		t.Errorf("default feeds carry network keys: %s", raw["feeds"])
	}
}

func TestLegacyFileDecodesToDefaults(t *testing.T) {
	data, _ := json.Marshal(DefaultConfig())
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	delete(raw, "aggregator")
	legacy, _ := json.Marshal(raw)

	// Over the defaults (config.parse): identical to DefaultConfig.
	c := DefaultConfig()
	if err := json.Unmarshal(legacy, &c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, DefaultConfig()) {
		t.Fatal("legacy file over defaults differs from defaults")
	}
	// Into a zero value: the zero aggregator block is restored by Normalize.
	var z Config
	_ = json.Unmarshal(legacy, &z)
	if notes := z.Aggregator.Normalize(); len(notes) != 0 || !z.Aggregator.IsDefault() {
		t.Fatalf("zero aggregator not restored: %v", notes)
	}
}

func TestAggregatorDefaults(t *testing.T) {
	a := DefaultAggregator()
	switch {
	case !a.MLAT.AutoUseSurvey || a.MLAT.AutoUseMaxSigmaM != 50:
		t.Error("AutoUseSurvey must default to true with 50 m (DESIGN §19.3)")
	case a.DefaultPin != PinHidden:
		t.Error("pins must be hidden by default (DESIGN §19.2)")
	case a.Admission != AdmissionInvite:
		t.Error("admission must default to invite")
	case a.AVFPort != 30160 || a.BeastPort != 30004 || a.MLATPort != 31090 || a.SBSInLocalPort != 0:
		t.Error("wrong default ports")
	case !a.PublicMap || !a.PublicStats || !a.TarJSON || a.IslandGuard:
		t.Error("wrong public defaults")
	case a.Limits.MaxStations < 1000 || a.Limits.MaxAircraft < 10000:
		t.Error("limits not sized for 1,000+ feeders / 10k+ aircraft (DESIGN §20.2)")
	case a.Limits.SelectiveAbove != 100:
		t.Error("selective threshold must default to 100")
	}
	if notes := a.Normalize(); len(notes) != 0 {
		t.Fatalf("defaults are not normalized: %v", notes)
	}
}

func TestAggregatorPartialJSON(t *testing.T) {
	var a Aggregator
	in := `{"name":"Club","mlat":{"holdout":0.2},"limits":{"maxStations":5000},"avfPort":"oops"}`
	if err := json.Unmarshal([]byte(in), &a); err != nil {
		t.Fatal(err)
	}
	want := DefaultAggregator()
	want.Name, want.MLAT.Holdout, want.Limits.MaxStations = "Club", 0.2, 5000
	if !a.Equal(want) {
		t.Fatalf("partial decode:\n got %+v\nwant %+v", a, want)
	}
	// A wrong-typed field inside the block must not abort the outer document.
	var c Config
	doc := `{"aggregator":{"avfPort":"x"},"mode":"aggregator"}`
	if err := json.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeAggregator || c.Aggregator.AVFPort != DefaultAVFPort {
		t.Fatalf("outer decode aborted: mode %q port %d", c.Mode, c.Aggregator.AVFPort)
	}
}

func TestAggregatorRoundTripCustom(t *testing.T) {
	c := DefaultConfig()
	c.Mode = ModeAggregator
	c.Aggregator.Name = "Net"
	c.Aggregator.TrustedProxies = []string{"10.0.0.0/8"}
	c.Aggregator.MLAT.AutoUseSurvey = false
	c.Aggregator.Limits.AllowCIDRs = []string{"192.0.2.7/32"}
	c.Uplinks = []Uplink{{ID: "club", Host: "agg.example", Port: 30160, Timestamps: true, Pin: PinRegion, Inputs: []string{"radarcape"}}}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var z Config
	if err := json.Unmarshal(data, &z); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(z, c) {
		t.Fatalf("custom round trip differs:\n%+v\n%+v", z, c)
	}
}

func TestAggregatorNormalize(t *testing.T) {
	a := DefaultAggregator()
	a.Admission = "bogus"
	a.DefaultPin = "exact"
	a.BeastPort = a.AVFPort // duplicate
	a.MLATPort = 70000
	a.TLSCert = "cert.pem"
	a.TrustedProxies = []string{"10.1.2.3/8", "bad", "192.0.2.1", "10.0.0.0/8"}
	a.TrustedRelays = []string{"0123456789ABCDEF0123456789abcdef", "nope", "01234567-89ab-cdef-0123-456789abcdef"}
	a.MLAT.Holdout = 2
	a.Limits.MaxConnsPerIP = 2
	a.Limits.MaxUnauthPerIP = 4
	notes := a.Normalize()
	if len(notes) < 9 {
		t.Errorf("expected a note per correction, got %d: %v", len(notes), notes)
	}
	switch {
	case a.Admission != AdmissionInvite, a.DefaultPin != PinHidden:
		t.Error("enums not reset")
	case a.BeastPort != 0 || a.MLATPort != 0 || a.AVFPort != DefaultAVFPort:
		t.Errorf("ports: avf %d beast %d mlat %d", a.AVFPort, a.BeastPort, a.MLATPort)
	case a.TLSCert != "" || a.TLSKey != "":
		t.Error("half TLS pair kept")
	case !slices.Equal(a.TrustedProxies, []string{"10.0.0.0/8", "192.0.2.1/32"}):
		t.Errorf("proxies %v", a.TrustedProxies)
	case !slices.Equal(a.TrustedRelays, []string{"0123456789abcdef0123456789abcdef"}):
		t.Errorf("relays %v", a.TrustedRelays)
	case a.MLAT.Holdout != 0.10:
		t.Error("holdout not clamped")
	case a.Limits.MaxConnsPerIP != 4:
		t.Error("conns per IP not raised")
	}
	if again := a.Normalize(); len(again) != 0 {
		t.Errorf("Normalize is not idempotent: %v", again)
	}
}

func TestAggregatorClone(t *testing.T) {
	a := DefaultAggregator()
	a.TrustedProxies = []string{"10.0.0.0/8"}
	b := a.Clone()
	b.TrustedProxies[0] = "x"
	if a.TrustedProxies[0] != "10.0.0.0/8" {
		t.Fatal("Clone shares slices")
	}
	if !DefaultAggregator().Equal(Aggregator(DefaultAggregator().Clone())) {
		t.Fatal("clone not equal")
	}
	e := DefaultAggregator()
	e.TrustedRelays = []string{}
	if !e.IsDefault() {
		t.Fatal("empty and nil lists must compare equal")
	}
}

func TestUplinkDefaultsOnDecode(t *testing.T) {
	var ups []Uplink
	if err := json.Unmarshal([]byte(`[{"id":"club","host":"agg.example"},{"id":"b","host":"h","timestamps":false,"port":"x"}]`), &ups); err != nil {
		t.Fatal(err)
	}
	want := DefaultUplink()
	want.ID, want.Host = "club", "agg.example"
	if !ups[0].Equal(want) {
		t.Fatalf("defaults not applied: %+v", ups[0])
	}
	if ups[1].Timestamps || !ups[1].SendLocation || ups[1].Port != DefaultAVFPort {
		t.Fatalf("second uplink: %+v", ups[1])
	}
	if ups[0].ShareGNSS || ups[0].PublicStats || ups[0].ShowName || ups[0].Pin != PinHidden {
		t.Fatal("privacy defaults must be off / hidden")
	}
}

func TestUplinkNormalize(t *testing.T) {
	ups := []Uplink{
		{ID: "Club", Host: " agg ", Port: 0, ServerPin: "md5/abc", SpoolMB: -5, Pin: "x"},
		{ID: "club", Host: "", Port: 30160, Pin: PinRegion},
		{ID: "bad id!", Host: "h", Port: 1, Pin: PinCell01, ServerPin: "sha256/" + strings.Repeat("A", 43) + "="},
	}
	notes := NormalizeUplinks(ups)
	if len(notes) == 0 {
		t.Fatal("no notes")
	}
	if ups[0].ID != "club" || ups[0].Host != "agg" || ups[0].Port != DefaultAVFPort || ups[0].ServerPin != "" || ups[0].SpoolMB != SpoolAuto || ups[0].Pin != PinHidden {
		t.Errorf("first: %+v", ups[0])
	}
	if ups[1].ID != "club-2" || !ups[1].Disabled {
		t.Errorf("second: %+v", ups[1])
	}
	if ups[2].ID != "uplink" || ups[2].ServerPin == "" {
		t.Errorf("third: %+v", ups[2])
	}
	if again := NormalizeUplinks(ups); len(again) != 0 {
		t.Errorf("not idempotent: %v", again)
	}
	c := CloneUplinks([]Uplink{{Inputs: []string{"a"}}})
	d := CloneUplinks(c)
	d[0].Inputs[0] = "b"
	if c[0].Inputs[0] != "a" || CloneUplinks(nil) != nil {
		t.Error("CloneUplinks")
	}
	if a := (Uplink{Host: "::1", Port: 30160}).Addr(); a != "[::1]:30160" {
		t.Errorf("Addr %q", a)
	}
}

func TestResolveSpoolMB(t *testing.T) {
	cases := []struct {
		v    int
		goos string
		sd   bool
		want int
	}{
		{SpoolAuto, "windows", false, 1024},
		{SpoolAuto, "linux", true, 0},
		{SpoolAuto, "linux", false, 256},
		{0, "windows", false, 0},
		{50, "linux", true, 50},
	}
	for _, c := range cases {
		if got := ResolveSpoolMB(c.v, c.goos, c.sd); got != c.want {
			t.Errorf("ResolveSpoolMB(%d,%s,%v) = %d, want %d", c.v, c.goos, c.sd, got, c.want)
		}
	}
}

func TestFeederConfig(t *testing.T) {
	f := DefaultFeederConfig()
	if notes := f.Normalize(); len(notes) != 0 {
		t.Fatalf("defaults not normalized: %v", notes)
	}
	data, _ := json.Marshal(f)
	var g FeederConfig
	if err := json.Unmarshal(data, &g); err != nil || !reflect.DeepEqual(f, g) {
		t.Fatalf("round trip: %v\n%+v\n%+v", err, f, g)
	}
	f.Receivers = append(f.Receivers, FeederReceiver{RxNo: 1, ClockClass: "warp", AltRef: "x", Located: true, Lat: 99, AGLM: -1})
	f.UI.Addr = "0.0.0.0:8099"
	f.Results.BeastPort = -1
	notes := f.Normalize()
	r := f.Receivers[1]
	if len(notes) < 6 || r.RxNo != 2 || r.ClockClass != "" || r.AltRef != AltRefMSL || r.Located || r.AGLM != 10 ||
		f.UI.Addr != DefaultFeederUIAddr || f.Results.BeastPort != DefaultResultsPort {
		t.Fatalf("normalize: %v %+v %+v", notes, r, f)
	}
	f.UI.Addr, f.UI.PasswordHash = "0.0.0.0:8099", "sha256:aa:bb"
	if notes := f.Normalize(); len(notes) != 0 || f.UI.Addr != "0.0.0.0:8099" {
		t.Fatalf("LAN UI with password refused: %v", notes)
	}
}

func TestFeedForwardable(t *testing.T) {
	if !(Feed{}).Forwardable() || (Feed{MLATOnly: true}).Forwardable() || (Feed{NoForward: true}).Forwardable() {
		t.Fatal("Forwardable")
	}
}

func TestValidServerPin(t *testing.T) {
	good := "sha256/" + strings.Repeat("A", 43) + "="
	for s, want := range map[string]bool{
		"": true, good: true, "sha256/" + strings.Repeat("A", 43): true,
		"sha256/AAAA": false, "sha1/" + strings.Repeat("A", 43) + "=": false, "sha256/!!!": false,
	} {
		if ValidServerPin(s) != want {
			t.Errorf("ValidServerPin(%q) != %v", s, want)
		}
	}
}

func TestTuningBlocksNormalize(t *testing.T) {
	var z UplinkTuning
	if n := z.Normalize("uplinkTuning"); n != nil || z != DefaultUplinkTuning() {
		t.Fatalf("zero tuning: %v %+v", n, z)
	}
	u := DefaultUplinkTuning()
	u.RingMB, u.BackoffMinSec, u.BackoffMaxSec = 9999, 30, 10
	if n := u.Normalize("uplinkTuning"); len(n) != 2 || u.RingMB != 12 || u.BackoffMaxSec != 30 {
		t.Fatalf("tuning notes %v %+v", n, u)
	}
	a := DefaultAggregator()
	a.Beast, a.Lanes = AggBeast{}, AggLanes{}
	if n := a.Normalize(); n != nil || a.Beast != DefaultAggregator().Beast || a.Lanes != DefaultAggregator().Lanes {
		t.Fatalf("zero blocks: %v %+v %+v", n, a.Beast, a.Lanes)
	}
	a.Beast.MaxLinks, a.Lanes.MaxRangeKm = 0, 5000
	if n := a.Normalize(); len(n) != 2 || a.Beast.MaxLinks != 4096 || a.Lanes.MaxRangeKm != 800 {
		t.Fatalf("clamp: %v %+v %+v", n, a.Beast, a.Lanes)
	}
	var b Aggregator
	if err := json.Unmarshal([]byte(`{"beast":{"maxLinks":7}}`), &b); err != nil || b.Beast.MaxLinks != 7 || b.Beast.IdleTimeoutSec != 180 || b.Lanes.FlushSec != 10 {
		t.Fatalf("decode %v %+v", err, b)
	}
}
