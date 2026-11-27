package federation

import (
	"strings"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

func pub(icao string, lat, lon, alt float64) publicdto.Aircraft {
	m := plane(icao, "X", lat, lon, alt)
	return publicdto.FromAircraft(&m)
}

func TestPolicyApply(t *testing.T) {
	norm := func(p Policy) Policy { p.normalize(); return p }
	var out publicdto.Aircraft
	a := pub("111111", 51.5, -0.1, 3000)
	cases := []struct {
		name   string
		p      Policy
		src    publicdto.Aircraft
		island bool
		ok     bool
	}{
		{"default", norm(Policy{}), a, false, true},
		{"none", norm(Policy{Aircraft: ShareNone}), a, false, false},
		{"island drop", norm(Policy{Island: IslandDrop}), a, true, false},
		{"island coarsen", norm(Policy{}), a, true, true},
		{"min alt below", norm(Policy{MinAltFt: 5000}), a, false, false},
		{"min alt above", norm(Policy{MinAltFt: 2000}), a, false, true},
		{"box in", norm(Policy{Boxes: []Box{{50, -2, 52, 1}}}), a, false, true},
		{"box out", norm(Policy{Boxes: []Box{{40, -2, 45, 1}}}), a, false, false},
		{"exclude", norm(Policy{ExcludeBoxes: []Box{{51, -1, 52, 0}}}), a, false, false},
		{"antimeridian in", norm(Policy{Boxes: []Box{{-50, 170, -30, -170}}}), pub("222222", -40, 179.5, 30000), false, true},
		{"antimeridian in west", norm(Policy{Boxes: []Box{{-50, 170, -30, -170}}}), pub("222222", -40, -175, 30000), false, true},
		{"antimeridian out", norm(Policy{Boxes: []Box{{-50, 170, -30, -170}}}), pub("222222", -40, 160, 30000), false, false},
	}
	for _, c := range cases {
		if got := c.p.apply(&c.src, c.island, &out); got != c.ok {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	// Ground / military / no position with boxes.
	g := pub("333333", 51, 0, 0)
	tr := true
	g.OnGround = &tr
	if p := norm(Policy{NoGround: true}); p.apply(&g, false, &out) {
		t.Error("ground shared")
	}
	if p := norm(Policy{MinAltFt: 1}); p.apply(&g, false, &out) {
		t.Error("ground passed the altitude floor")
	}
	mil := a
	mil.Military = true
	if p := norm(Policy{NoMilitary: true}); p.apply(&mil, false, &out) {
		t.Error("military shared")
	}
	nopos := publicdto.Aircraft{ICAO: "444444"}
	if p := norm(Policy{Boxes: []Box{{-90, -180, 90, 180}}}); p.apply(&nopos, false, &out) {
		t.Error("no-position aircraft passed a box filter")
	}
	// nomlat strips the MLAT position but keeps the aircraft.
	ml := a
	ml.Source, ml.MLATGrade = model.SrcMLAT, "A"
	if p := norm(Policy{Aircraft: ShareNoMLAT}); !p.apply(&ml, false, &out) || out.Lat != nil || out.MLATGrade != "" || out.Source != model.SrcModeS {
		t.Errorf("nomlat: %+v", out)
	}
	if p := norm(Policy{Aircraft: ShareNoMLAT}); p.fixAllowed(&Fix{Lat: 1, Lon: 1}) {
		t.Error("nomlat shares fixes")
	}
	// Coarsening rounds and marks; the source is not modified.
	if p := norm(Policy{}); !p.apply(&a, true, &out) || !out.Coarse || *out.Lat != 51.5 || *out.Lon != -0.1 || a.Coarse {
		t.Errorf("coarsen: %+v", out)
	}
	r := ReceivePolicy{Aircraft: ShareAll, Boxes: []Box{{50, -2, 52, 1}}}
	if !r.accept(&a) || r.accept(&nopos) {
		t.Error("receive boxes")
	}
}

func TestConfigCheck(t *testing.T) {
	id := newID(t).FeederID()
	c := Config{Enabled: true, Listen: "bad", TLSCert: "x",
		Peers: []PeerConfig{{ID: "nope"}, {ID: id, Addr: "host", Pin: "md5/x", Send: Policy{Aircraft: "some", Island: "blur", DelayS: 9999}},
			{ID: id}},
		Feeds: []FeedConfig{{Name: "f", Format: "avr", Mode: "push", Addr: "x", Allow: []string{"300.1.1.1"}}, {Name: "f", Format: "json", Mode: "listen", Addr: ":1"}}}
	c.Normalize()
	probs := strings.Join(c.Check("aggregator.federation"), "\n")
	for _, want := range []string{"listen", "tlsCert", "peers[0].id", "peers[1].addr", "peers[1].pin", "peers[1].send.aircraft",
		"peers[1].send.island", "peers[1].send.delayS", "peers[2].id: duplicate", "feeds[0].format", "feeds[0].mode",
		"feeds[0].addr", "feeds[0].allow[0]", "feeds[1].name"} {
		if !strings.Contains(probs, "aggregator.federation."+want) {
			t.Errorf("missing %q in\n%s", want, probs)
		}
	}
	ok := Config{Enabled: true, Listen: ":30170", Peers: []PeerConfig{{Name: "x", ID: id, Addr: "fed.example.net:30170"}},
		Feeds: []FeedConfig{{Format: "beast", Mode: "connect", Addr: "feed.example.org:30004"}}}
	ok.Normalize()
	if p := ok.Check("f"); len(p) != 0 {
		t.Fatal(p)
	}
	if ok.Peers[0].Send.Aircraft != ShareAll || ok.Peers[0].Receive.MaxAircraft != 20_000 || ok.Feeds[0].Allow[0] != "127.0.0.0/8" || ok.IntervalMs != 1000 {
		t.Fatalf("defaults %+v", ok)
	}
	if _, err := New(Config{Enabled: true, Peers: []PeerConfig{{ID: "x"}}}, Options{Identity: newID(t)}); err == nil {
		t.Fatal("New accepted an invalid config")
	}
	self := newID(t)
	if _, err := New(Config{Enabled: true, Peers: []PeerConfig{{ID: self.FeederID()}}}, Options{Identity: self}); err == nil {
		t.Fatal("New accepted itself as a peer")
	}
}

// TestDelayAndReceivePolicy: a delayed peer gets data no earlier than the
// delay; a receive policy of "none" ignores everything.
func TestDelayAndReceivePolicy(t *testing.T) {
	ida, idb, idc := newID(t), newID(t), newID(t)
	a := startNode(t, ida, "a", "auto", Config{Peers: []PeerConfig{
		{Name: "b", ID: idb.FeederID(), Send: Policy{DelayS: 2}}, {Name: "c", ID: idc.FeederID()}}}, Options{})
	b := startNode(t, idb, "b", "", Config{Peers: []PeerConfig{{Name: "a", ID: ida.FeederID(), Addr: a.Addr().String()}}}, Options{})
	c := startNode(t, idc, "c", "", Config{Peers: []PeerConfig{{Name: "a", ID: ida.FeederID(), Addr: a.Addr().String(),
		Receive: ReceivePolicy{Aircraft: ShareNone}}}}, Options{})
	waitFor(t, 5*time.Second, "connected", func() bool {
		return peerState(b.Node, "a").State == StateConnected && peerState(c.Node, "a").State == StateConnected
	})
	start := time.Now()
	a.pic.set(plane("ABC123", "D1", 50, 1, 30000))
	waitFor(t, 6*time.Second, "delayed arrival", func() bool { return hasAll(b.Node, "ABC123") })
	if el := time.Since(start); el < 1900*time.Millisecond {
		t.Fatalf("arrived after %v, delay is 2 s", el)
	}
	if len(c.RemoteAircraft()) != 0 || peerState(c.Node, "a").Rejected == 0 {
		t.Fatal("receive policy none accepted data")
	}
}
