package federation

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
)

func newID(t testing.TB) avf.Identity {
	t.Helper()
	id, err := avf.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// picture is a mutable local picture for a node's Source.
type picture struct {
	mu sync.Mutex
	ac []model.Aircraft
}

func (p *picture) set(ac ...model.Aircraft) {
	p.mu.Lock()
	p.ac = ac
	p.mu.Unlock()
}

func (p *picture) source() model.LiveSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixMilli()
	out := make([]model.Aircraft, len(p.ac))
	for i, a := range p.ac {
		a.LastSeen = now - 200
		a.FirstSeen = now - 60_000
		out[i] = a
	}
	return model.LiveSnapshot{Now: now, Aircraft: out}
}

func f64(v float64) *float64 { return &v }

// plane builds a local aircraft with a position.
func plane(icao, call string, lat, lon, alt float64) model.Aircraft {
	g := false
	return model.Aircraft{ICAO: icao, Callsign: call, Lat: f64(lat), Lon: f64(lon), AltBaro: f64(alt),
		GS: f64(420.5), Track: f64(271.25), VertRate: f64(-640), OnGround: &g, Source: model.SrcADSB,
		PosAge: f64(0.4), Squawk: "4521", Category: "A3"}
}

// sinkRec records updates delivered to a Sink.
type sinkRec struct {
	mu sync.Mutex
	u  []model.DecodedUpdate
}

func (s *sinkRec) HandleFrame(model.RawFrame) {}
func (s *sinkRec) HandleUpdate(u model.DecodedUpdate) {
	s.mu.Lock()
	s.u = append(s.u, u)
	s.mu.Unlock()
}
func (s *sinkRec) all() []model.DecodedUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.DecodedUpdate(nil), s.u...)
}

type testNode struct {
	*Node
	pic  *picture
	sink *sinkRec
}

// startNode builds and starts a node; peers reference other nodes' IDs.
// listen is "" (no listener), "auto" (127.0.0.1:0) or an address.
func startNode(t *testing.T, id avf.Identity, name string, listen string, cfg Config, opts Options) *testNode {
	t.Helper()
	cfg.Enabled = true
	cfg.Name = name
	switch listen {
	case "auto":
		cfg.Listen = "127.0.0.1:0"
	default:
		cfg.Listen = listen
	}
	if cfg.IntervalMs == 0 {
		cfg.IntervalMs = 100
	}
	if cfg.MLATIntervalMs == 0 {
		cfg.MLATIntervalMs = 50
	}
	tn := &testNode{pic: &picture{}, sink: &sinkRec{}}
	opts.Identity = id
	if opts.Source == nil {
		opts.Source = tn.pic.source
	}
	if opts.Sink == nil {
		opts.Sink = tn.sink
	}
	opts.Version = "test"
	n, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	tn.Node = n
	return tn
}

func peerCfg(name string, id avf.Identity, addr string) PeerConfig {
	return PeerConfig{Name: name, ID: id.FeederID(), Addr: addr}
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func remoteByICAO(n *Node) map[string]bool {
	m := map[string]bool{}
	for _, a := range n.RemoteAircraft() {
		m[a.ICAO] = true
	}
	return m
}

func hasAll(n *Node, icaos ...string) bool {
	m := remoteByICAO(n)
	for _, i := range icaos {
		if !m[i] {
			return false
		}
	}
	return true
}

func peerState(n *Node, name string) PeerStatus {
	for _, p := range n.Status().Peers {
		if p.Name == name {
			return p
		}
	}
	panic(fmt.Sprintf("no peer %s", name))
}
