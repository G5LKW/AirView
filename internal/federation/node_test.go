package federation

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	mgeo "airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

// TestTwoPeers: two in-process federated networks exchange aircraft state
// and MLAT results in both directions over AVFED/1.
func TestTwoPeers(t *testing.T) {
	ida, idb := newID(t), newID(t)
	a := startNode(t, ida, "net-a", "auto", Config{Peers: []PeerConfig{{Name: "b", ID: idb.FeederID(),
		Send: Policy{MLAT: true}}}}, Options{})
	b := startNode(t, idb, "net-b", "", Config{Peers: []PeerConfig{{Name: "a", ID: ida.FeederID(),
		Addr: a.Addr().String()}}}, Options{})

	mlatAC := plane("3C6DD1", "MLT01", 51.2, 7.1, 9000)
	mlatAC.Source, mlatAC.MLATGrade, mlatAC.MLATSigmaM = model.SrcMLAT, "B", f64(88)
	noPos := model.Aircraft{ICAO: "~A1B2C3", Source: model.SrcModeS, AltBaro: f64(12000), Squawk: "7000"}
	a.pic.set(plane("4840D6", "KLM1023", 52.3104567, 4.7623412, 3500), mlatAC, noPos)
	b.pic.set(plane("ABCDEF", "UAL9", 40.1, -74.2, 31000))

	waitFor(t, 5*time.Second, "B has A's aircraft", func() bool { return hasAll(b.Node, "4840D6", "3C6DD1", "~A1B2C3") })
	waitFor(t, 5*time.Second, "A has B's aircraft", func() bool { return hasAll(a.Node, "ABCDEF") })

	var got publicdto.Aircraft
	for _, x := range b.RemoteAircraft() {
		if x.ICAO == "4840D6" {
			got = x
		}
	}
	if got.Callsign != "KLM1023" || got.Squawk != "4521" || got.Category != "A3" || got.Source != model.SrcADSB {
		t.Fatalf("identity fields: %+v", got)
	}
	if math.Abs(*got.Lat-52.3104567) > 1e-7 || math.Abs(*got.Lon-4.7623412) > 1e-7 {
		t.Fatalf("position %v %v", *got.Lat, *got.Lon)
	}
	if *got.AltBaro != 3500 || *got.GS != 420.5 || *got.Track != 271.25 || *got.VertRate != -640 || *got.OnGround {
		t.Fatalf("kinematics: alt %v gs %v trk %v vr %v", *got.AltBaro, *got.GS, *got.Track, *got.VertRate)
	}
	if age := time.Now().UnixMilli() - got.LastSeen; age < 0 || age > 3000 {
		t.Fatalf("lastSeen age %d ms", age)
	}
	for _, x := range b.RemoteAircraft() {
		if x.ICAO == "3C6DD1" && (x.Source != model.SrcMLAT || x.MLATGrade != "B" || x.MLATSigmaM == nil || *x.MLATSigmaM != 88) {
			t.Fatalf("mlat aircraft %+v", x)
		}
		if x.ICAO == "~A1B2C3" && (x.Lat != nil || *x.AltBaro != 12000 || x.Squawk != "7000") {
			t.Fatalf("no-position aircraft %+v", x)
		}
	}

	// Sink delivery with the federation feed ID.
	waitFor(t, 2*time.Second, "sink updates", func() bool { return len(b.sink.all()) >= 3 })
	for _, u := range b.sink.all() {
		if u.FeedID != FeedPrefix+ida.FeederID() || u.Station != 0 {
			t.Fatalf("sink update feed %q", u.FeedID)
		}
		if u.ICAO == 0x3C6DD1 && (!u.MLAT || u.Src != SrcMLATFed || u.Grade != model.GradeB) {
			t.Fatalf("mlat update %+v", u)
		}
		if u.ICAO == 0xA1B2C3|1<<24 && u.Lat != nil {
			t.Fatalf("non-ICAO update %+v", u)
		}
	}

	// MLAT results: A shares, B does not (default policy).
	ecef := mgeo.ToECEF(50.5, 6.25, 10_000)
	fix := model.MLATFix{ICAO: 0x3C6DD1, TUnixNs: time.Now().UnixNano(), ECEF: [3]float64{ecef.X, ecef.Y, ecef.Z},
		Cov: [6]float32{900, 0, 0, 900, 0, 900}, Grade: model.GradeB, NBucket: model.NBucket5to8, Flags: model.FixFlagTracked}
	a.PublishMLAT([]model.MLATFix{fix})
	waitFor(t, 3*time.Second, "B has the fix", func() bool { return len(b.RemoteFixes()) == 1 })
	rf := b.RemoteFixes()[0]
	if math.Abs(rf.Lat-50.5) > 1e-6 || math.Abs(rf.Lon-6.25) > 1e-6 || math.Abs(rf.AltM-10_000) > 0.1 || rf.NBucket != model.NBucket5to8 {
		t.Fatalf("fix %+v", rf)
	}
	b.PublishMLAT([]model.MLATFix{fix})
	time.Sleep(300 * time.Millisecond)
	if len(a.RemoteFixes()) != 0 {
		t.Fatal("B shared MLAT although its policy says no")
	}

	sa, sb := peerState(a.Node, "b"), peerState(b.Node, "a")
	if sa.State != StateConnected || sb.State != StateConnected || sa.Direction != "in" || sb.Direction != "out" {
		t.Fatalf("states %+v / %+v", sa, sb)
	}
	if sb.PeerInfo == nil || !sb.PeerInfo.MLAT || sa.PeerInfo == nil || sa.PeerInfo.MLAT {
		t.Fatalf("peer info %+v / %+v", sb.PeerInfo, sa.PeerInfo)
	}
	if sb.Remote != 3 || sa.Remote != 1 || sb.MLATIn != 1 {
		t.Fatalf("counters: b.remote %d a.remote %d mlatIn %d", sb.Remote, sa.Remote, sb.MLATIn)
	}

	// Removal: an aircraft that disappears from A expires on B after the TTL
	// (exercised with the store directly; the TTL is minutes in production).
	a.pic.set(plane("4840D6", "KLM1023", 52.31, 4.76, 3600))
	time.Sleep(300 * time.Millisecond)
	if n := b.store.expire(time.Now().UnixMilli() + int64(DefaultTTLS+1)*1000); n != 3 {
		t.Fatalf("expired %d", n)
	}

	// Shutdown: B sees the goodbye and backs off.
	a.Close()
	waitFor(t, 3*time.Second, "B notices", func() bool { return peerState(b.Node, "a").State != StateConnected })
	if e := peerState(b.Node, "a").LastError; !strings.Contains(e, ByeShutdown) {
		t.Fatalf("last error %q", e)
	}
}

// TestLoopThree: three networks in a triangle, transit on everywhere. Every
// aircraft reaches every network, nothing ever comes back to its origin,
// the direct path wins, and traffic stays bounded.
func TestLoopThree(t *testing.T) {
	ida, idb, idc := newID(t), newID(t), newID(t)
	tr := Policy{Transit: true}
	a := startNode(t, ida, "a", "auto", Config{Transit: true, Peers: []PeerConfig{
		{Name: "b", ID: idb.FeederID(), Send: tr}, {Name: "c", ID: idc.FeederID(), Send: tr}}}, Options{})
	b := startNode(t, idb, "b", "auto", Config{Transit: true, Peers: []PeerConfig{
		{Name: "a", ID: ida.FeederID(), Addr: a.Addr().String(), Send: tr}, {Name: "c", ID: idc.FeederID(), Send: tr}}}, Options{})
	c := startNode(t, idc, "c", "", Config{Transit: true, Peers: []PeerConfig{
		{Name: "a", ID: ida.FeederID(), Addr: a.Addr().String(), Send: tr},
		{Name: "b", ID: idb.FeederID(), Addr: b.Addr().String(), Send: tr}}}, Options{})
	a.pic.set(plane("AAAAA1", "A1", 50, 1, 30000))
	b.pic.set(plane("BBBBB1", "B1", 51, 2, 31000))
	c.pic.set(plane("CCCCC1", "C1", 52, 3, 32000))
	waitFor(t, 5*time.Second, "full mesh", func() bool {
		return hasAll(a.Node, "BBBBB1", "CCCCC1") && hasAll(b.Node, "AAAAA1", "CCCCC1") && hasAll(c.Node, "AAAAA1", "BBBBB1")
	})
	time.Sleep(1500 * time.Millisecond) // let transit copies circulate
	own := map[*testNode]string{a: "AAAAA1", b: "BBBBB1", c: "CCCCC1"}
	for n, mine := range own {
		if remoteByICAO(n.Node)[mine] {
			t.Fatalf("%s got its own aircraft back", n.cfg.Name)
		}
		n.store.mu.RLock()
		for _, e := range n.store.m {
			if slices.Contains(e.path, n.id) {
				t.Fatalf("%s holds an entry whose path contains itself: %v", n.cfg.Name, e.path)
			}
			if len(e.path) != 1 {
				t.Fatalf("%s: direct path must win, got %v", n.cfg.Name, e.path)
			}
		}
		n.store.mu.RUnlock()
		if st := n.Status().Remote; st.Spoofed != 0 || st.ProtoErrors != 0 {
			t.Fatalf("%s: %+v", n.cfg.Name, st)
		}
	}
	// Bounded traffic: per second each link carries at most the aircraft
	// of the picture (local + transit), not an echo cascade.
	before := totalOut(a, b, c)
	time.Sleep(2 * time.Second)
	rate := float64(totalOut(a, b, c)-before) / 2
	// 6 directed links × (1 own + 1 transit) aircraft × 10 exports/s = 120/s max
	// with delta suppression far less (unchanged LastSeen is not re-sent),
	// plus full refreshes.
	if rate > 130 {
		t.Fatalf("aircraft out rate %.0f/s: echo cascade?", rate)
	}

}

func totalOut(ns ...*testNode) uint64 {
	var s uint64
	for _, n := range ns {
		for _, p := range n.Status().Peers {
			s += p.AircraftOut
		}
	}
	return s
}

// TestTransitLine: A - B - C with no direct A-C link.
func TestTransitLine(t *testing.T) {
	for _, transit := range []bool{true, false} {
		t.Run(map[bool]string{true: "transit", false: "no-transit"}[transit], func(t *testing.T) {
			ida, idb, idc := newID(t), newID(t), newID(t)
			pol := Policy{Transit: true}
			a := startNode(t, ida, "a", "auto", Config{Peers: []PeerConfig{{Name: "b", ID: idb.FeederID(), Send: pol}}}, Options{})
			b := startNode(t, idb, "b", "auto", Config{Transit: transit, MaxHops: 2, Peers: []PeerConfig{
				{Name: "a", ID: ida.FeederID(), Addr: a.Addr().String(), Send: pol},
				{Name: "c", ID: idc.FeederID(), Send: pol}}}, Options{})
			c := startNode(t, idc, "c", "", Config{Transit: true, Peers: []PeerConfig{
				{Name: "b", ID: idb.FeederID(), Addr: b.Addr().String(), Send: pol}}}, Options{})
			a.pic.set(plane("AAAAA1", "A1", 50, 1, 30000))
			c.pic.set(plane("CCCCC1", "C1", 52, 3, 32000))
			ecef := mgeo.ToECEF(50.5, 6.25, 10_000)
			if transit {
				waitFor(t, 5*time.Second, "C gets A's aircraft via B", func() bool { return hasAll(c.Node, "AAAAA1") })
				waitFor(t, 5*time.Second, "A gets C's aircraft via B", func() bool { return hasAll(a.Node, "CCCCC1") })
				c.store.mu.RLock()
				for _, e := range c.store.m {
					if !slices.Equal(e.path, []string{ida.FeederID(), idb.FeederID()}) {
						t.Errorf("path %v", e.path)
					}
				}
				c.store.mu.RUnlock()
				// A's own picture never comes back from B (split horizon).
				if remoteByICAO(a.Node)["AAAAA1"] {
					t.Fatal("echo")
				}
				return
			}
			_ = ecef
			waitFor(t, 5*time.Second, "B has both", func() bool { return hasAll(b.Node, "AAAAA1", "CCCCC1") })
			time.Sleep(500 * time.Millisecond)
			if hasAll(c.Node, "AAAAA1") || hasAll(a.Node, "CCCCC1") {
				t.Fatal("transit happened although B has transit off")
			}
		})
	}
}

// TestReceiverLoopGuards feeds forged records straight into the receive
// path: a path through ourselves, a path that does not end with the
// sending peer, and an over-long path are all dropped and counted.
func TestReceiverLoopGuards(t *testing.T) {
	self, peerID, other := newID(t), newID(t), newID(t)
	n, err := New(Config{Enabled: true, MaxHops: 3, Peers: []PeerConfig{{Name: "p", ID: peerID.FeederID()}}}, Options{Identity: self})
	if err != nil {
		t.Fatal(err)
	}
	l := &link{n: n, p: n.peers[peerID.FeederID()], info: PeerInfo{NodeID: peerID.FeederID()}}
	ac := []publicdto.Aircraft{{ICAO: "123456", Source: model.SrcADSB, LastSeen: 1000}}
	send := func(path ...string) {
		p, _ := AppendState(nil, 1000, false, path, ac)
		if err := n.onState(l, p); err != nil {
			t.Fatal(err)
		}
	}
	send(self.FeederID(), peerID.FeederID())                                            // loop through us
	send(other.FeederID())                                                              // spoofed sender
	send(other.FeederID(), newID(t).FeederID(), newID(t).FeederID(), peerID.FeederID()) // 4 > MaxHops 3
	send(peerID.FeederID(), other.FeederID(), peerID.FeederID())                        // repeated node
	st := n.Status().Remote
	if st.Loops != 2 || st.Spoofed != 1 || st.HopDrops != 1 || st.Aircraft != 0 {
		t.Fatalf("guards: %+v", st)
	}
	send(other.FeederID(), peerID.FeederID())
	if n.Status().Remote.Aircraft != 1 {
		t.Fatal("valid transit record rejected")
	}
	fx := []Fix{{ICAO: 1, Lat: 1, Lon: 1, Grade: model.GradeA}}
	p, _ := AppendMLAT(nil, 1000, []string{self.FeederID(), peerID.FeederID()}, fx)
	if err := n.onMLAT(l, p); err != nil {
		t.Fatal(err)
	}
	if n.Status().Remote.Loops != 3 || len(n.RemoteFixes()) != 0 {
		t.Fatal("MLAT loop not dropped")
	}
}

// TestDuplicateLinks: both sides dial each other; exactly one link
// survives and data flows.
func TestDuplicateLinks(t *testing.T) {
	ida, idb := newID(t), newID(t)
	// A's address must be known before B starts and vice versa: bind A's
	// port once, release it and reuse it.
	a0 := startNode(t, ida, "a", "auto", Config{Peers: []PeerConfig{{Name: "b", ID: idb.FeederID()}}}, Options{})
	addrA := a0.Addr().String()
	a0.Close()
	b := startNode(t, idb, "b", "auto", Config{Peers: []PeerConfig{{Name: "a", ID: ida.FeederID(), Addr: addrA}}}, Options{})
	a := startNode(t, ida, "a", addrA, Config{Peers: []PeerConfig{{Name: "b", ID: idb.FeederID(), Addr: b.Addr().String()}}}, Options{})
	a.pic.set(plane("AAAAA1", "A1", 50, 1, 30000))
	b.pic.set(plane("BBBBB1", "B1", 51, 2, 31000))
	waitFor(t, 8*time.Second, "both directions", func() bool { return hasAll(a.Node, "BBBBB1") && hasAll(b.Node, "AAAAA1") })
	time.Sleep(time.Second)
	sa, sb := peerState(a.Node, "b"), peerState(b.Node, "a")
	if sa.State != StateConnected || sb.State != StateConnected {
		t.Fatalf("states %s %s", sa.State, sb.State)
	}
	// Both sides hold the same single link (same dialler).
	la, lb := a.peers[idb.FeederID()].current(), b.peers[ida.FeederID()].current()
	if la == nil || lb == nil || la.dialer != lb.dialer || la.outbound == lb.outbound {
		t.Fatal("the two sides disagree about the surviving link")
	}
}
