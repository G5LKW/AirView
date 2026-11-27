package engine

// End-to-end runs of the phase 4-5 wiring (WP-WIRE-45) over real sockets:
// defaults unchanged, Beast outputs ("also feed others"), RTL-SDR station
// feed through rtl_tcp, the experimental SSR estimator, the sharded
// tracker selection, federation between two aggregators with a Beast
// out-feed, and Mode A/C MLAT on/off.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/config"
	"airview/internal/federation"
	"airview/internal/mlat/ssr/ssrsim"
	"airview/internal/mlat/track"
	"airview/internal/model"
	"airview/internal/sdr"
	"airview/internal/sim/simnet"
	"airview/internal/uplink"
)

// beastSink is a third-party Beast aggregator: it counts the Mode S frames
// of every client (escape-aware) and keeps the first bytes.
type beastSink struct {
	ln     net.Listener
	frames atomic.Int64
	mu     sync.Mutex
	head   []byte
}

func newBeastSink(t *testing.T) *beastSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &beastSink{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.read(c)
		}
	}()
	return s
}

func (s *beastSink) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *beastSink) read(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 64<<10)
	esc := false
	for {
		n, err := c.Read(buf)
		s.mu.Lock()
		if len(s.head) < 4096 {
			s.head = append(s.head, buf[:min(n, 4096-len(s.head))]...)
		}
		s.mu.Unlock()
		for _, b := range buf[:n] {
			if esc {
				esc = false
				if b == '2' || b == '3' {
					s.frames.Add(1)
				}
				continue
			}
			if b == 0x1a {
				esc = true
			}
		}
		if err != nil {
			return
		}
	}
}

func startEngine(t *testing.T, dir string, cfg model.Config, o Options) *Engine {
	t.Helper()
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if o.Logger == nil {
		o.Logger = quietLogger()
	}
	o.Offline = true
	e := New(dir, o)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Stop)
	return e
}

// TestWire45StandaloneDefaults: a default standalone station runs none of
// the phase 4-5 features and its config.json does not grow the new blocks.
func TestWire45StandaloneDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := model.DefaultConfig()
	cfg.NTP.Enabled = false
	cfg.Feeds = nil
	e := startEngine(t, dir, cfg, Options{})
	if e.Aggregator() || e.Federation() != nil || e.MLAT() != nil {
		t.Fatal("standalone runs network parts")
	}
	if e.Uplinks() != nil {
		t.Fatal("uplink manager without an uplink")
	}
	if s := e.SSRStatus(); s.Enabled || e.SSRSolveNow() {
		t.Fatalf("SSR running by default: %+v", s)
	}
	if ls := e.Tracker().LockStats(); ls.Shards > 1 {
		t.Fatalf("standalone tracker sharded: %d", ls.Shards)
	}
	eff := e.EffectiveConfig()
	if eff.SSR.Enabled || eff.Aggregator.MLAT.LatencyCal.Enabled || eff.Aggregator.Federation.Enabled || eff.Aggregator.MLAT.ModeAC {
		t.Fatal("a phase 4-5 feature is on by default")
	}
	b, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"ssr"`)) {
		t.Fatal(`config.json carries an "ssr" block by default`)
	}
}

// TestWire45BeastOutput: a standalone station with a Beast feed and a
// Beast output ("also feed others") delivers frames to a third-party
// Beast aggregator; removing the entry stops it.
func TestWire45BeastOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	sink := newBeastSink(t)
	_, streams := simStreams(t, 1, 20*time.Second)
	cfg := model.DefaultConfig()
	cfg.NTP.Enabled = false
	cfg.Receiver = model.Receiver{Name: "rx", Located: true, Lat: 50, Lon: 9}
	cfg.Feeds = []model.Feed{{ID: "evo", Name: "EVO", Type: model.FeedBeast, Host: "127.0.0.1", Port: serveOnce(t, streams[0]), Enabled: true}}
	up := model.DefaultUplink()
	up.ID, up.Type, up.Host, up.Port, up.Format = "other", model.UplinkBeast, "127.0.0.1", sink.port(), "beast_out"
	cfg.Uplinks = []model.Uplink{up}
	e := startEngine(t, t.TempDir(), cfg, Options{})
	waitFor(t, "Beast frames at the third-party aggregator", 20*time.Second, func() bool { return sink.frames.Load() > 100 })
	m := e.Uplinks()
	if m == nil {
		t.Fatal("no uplink manager")
	}
	bs := m.BeastStatus()
	if len(bs) != 1 || bs[0].State != "connected" || bs[0].FramesSent == 0 || bs[0].Format != "beast_out" {
		t.Fatalf("beast status %+v", bs)
	}
	if len(m.Status()) != 0 {
		t.Fatalf("an AVF uplink appeared: %+v", m.Status())
	}
	if withPositions(e) == 0 {
		t.Fatal("local map lost its aircraft")
	}
	next := config.Clone(e.Config().Get())
	next.Uplinks = nil
	if _, _, err := e.ApplyConfig(next); err != nil {
		t.Fatal(err)
	}
	if bs := e.Uplinks().BeastStatus(); len(bs) != 0 {
		t.Fatalf("Beast output still running: %+v", bs)
	}
}

// fakeRTLTCP starts the sdr package's fake rtl_tcp server streaming
// synthesised I/Q with the given frames (repeating).
func fakeRTLTCP(t *testing.T, frames [][]byte, every, count int) int {
	t.Helper()
	var sf []sdr.SynthFrame
	for i := 0; i < count; i++ {
		sf = append(sf, sdr.SynthFrame{Data: frames[i%len(frames)], Start: float64(1000 + i*every), SNRDB: 25, Phase: 0.3})
	}
	iq := sdr.Synthesize(sdr.SynthConfig{Seed: 3}, 1000+count*every+2000, sf)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	var mu sync.Mutex
	pos := 0
	fake := &sdr.FakeRTLTCP{Tuner: sdr.TunerR820T, GainCount: 29, Rate: 2.4e6, Source: func(n int) []byte {
		mu.Lock()
		defer mu.Unlock()
		out := make([]byte, 0, 2*n)
		for len(out) < 2*n {
			k := min(2*n-len(out), len(iq)-pos)
			out = append(out, iq[pos:pos+k]...)
			pos = (pos + k) % len(iq)
		}
		return out
	}}
	go fake.Serve(ctx, ln)
	return ln.Addr().(*net.TCPAddr).Port
}

// TestWire45RTLTCPStationFeed: an AirView station reads an RTL-SDR through
// rtl_tcp (feed type rtltcp): the pure-Go demodulator's frames reach the
// tracker, the aircraft gets a CPR position and the feed shows connected.
func TestWire45RTLTCPStationFeed(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	// A hovering target (same position in both CPR formats), encoded with
	// the test encoder of ssrwire_test.go.
	even := posFrame(0x40621D, 52.2572, 3.9194, 35000, false)
	odd := posFrame(0x40621D, 52.2572, 3.9194, 35000, true)
	port := fakeRTLTCP(t, [][]byte{even, odd}, 300000, 20)
	cfg := model.DefaultConfig()
	cfg.NTP.Enabled = false
	cfg.Receiver = model.Receiver{Name: "rx", Located: true, Lat: 52.1, Lon: 4.1}
	cfg.Feeds = []model.Feed{{ID: "dongle", Name: "RTL-SDR", Type: model.FeedRTLTCP, Host: "127.0.0.1", Port: port, Enabled: true,
		SDR: model.FeedSDR{Gain: "40.2", PPM: 3}}}
	e := startEngine(t, t.TempDir(), cfg, Options{})
	waitFor(t, "RTL-SDR aircraft with a position", 30*time.Second, func() bool {
		a, ok := e.Tracker().Aircraft(0x40621D)
		return ok && a.Lat != nil && math.Abs(*a.Lat-52.2572) < 0.01 && math.Abs(*a.Lon-3.9194) < 0.01
	})
	waitFor(t, "rtltcp feed shown connected", 20*time.Second, func() bool {
		for _, s := range e.FeedStatus() {
			if s.ID == "dongle" && s.Connected && s.Frames > 5 {
				return true
			}
		}
		return false
	})
}

// ssrBeastStream renders an ssrsim scenario as one Beast byte stream.
func ssrBeastStream(sc ssrsim.Scenario) ([]byte, ssrsim.Truth) {
	var b []byte
	tr := ssrFrames(sc, func(f model.RawFrame) { b = sdr.AppendBeast(b, f) })
	return b, tr
}

// TestWire45SSREngine: ssr.enabled switches the estimator on at run time;
// the station's Beast feed (a simulated receiver hearing three rotating
// SSRs) reaches it, a solve runs on request, and switching it off stops
// it. The result never touches the receiver position.
func TestWire45SSREngine(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	sc := ssrsim.Baseline(22)
	sc.DurS = 300
	stream, _ := ssrBeastStream(sc)
	cfg := model.DefaultConfig()
	cfg.NTP.Enabled = false
	rxHint := ssrsim.Offset(sc.Receiver, 120, 10e3, 0)
	cfg.Receiver = model.Receiver{Name: "rx", Located: true, Lat: rxHint.Lat, Lon: rxHint.Lon}
	cfg.Feeds = nil
	e := startEngine(t, t.TempDir(), cfg, Options{})
	if e.SSRStatus().Enabled {
		t.Fatal("SSR on before it was enabled")
	}
	next := config.Clone(e.Config().Get())
	next.SSR.Enabled = true
	next.SSR.SolveEveryMin = 60
	next.Feeds = []model.Feed{{ID: "evo", Name: "EVO", Type: model.FeedBeast, Host: "127.0.0.1", Port: servePaced(t, stream, 100, 250*time.Millisecond), Enabled: true}}
	if _, _, err := e.ApplyConfig(next); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "SSR estimator fed", 60*time.Second, func() bool {
		s := e.SSRStatus()
		return s.Enabled && s.Estimator.PairMatches > 200 && s.Positions > 1000
	})
	// Let the 1 Hz Process loop drain the stream, then solve on request.
	waitFor(t, "stream drained", 60*time.Second, func() bool {
		a := e.SSRStatus().Estimator.PairMatches
		time.Sleep(1500 * time.Millisecond)
		return e.SSRStatus().Estimator.PairMatches == a
	})
	if !e.SSRSolveNow() {
		t.Fatal("SSRSolveNow refused")
	}
	waitFor(t, "SSR solve", 60*time.Second, func() bool { s := e.SSRStatus(); return s.SolveRuns > 0 && !s.Solving })
	s := e.SSRStatus()
	if s.Last == nil || s.FeedID != "evo" {
		t.Fatalf("status %+v", s)
	}
	errH := math.NaN()
	if s.Last.OK {
		d := geoDistM(s.Last.Lat, s.Last.Lon, sc.Receiver.Lat, sc.Receiver.Lon)
		errH = d
	}
	t.Logf("SSR engine: frames %d positions %d replies %d pairMatches %d; solve ok=%v %q err_h %.1f m σ_h %.1f m radars %d",
		s.Frames, s.Positions, s.Replies, s.Estimator.PairMatches, s.Last.OK, s.Last.Reason, errH, s.Last.SigmaHM, s.Last.NRadars)
	if !s.Last.OK || errH > 60 {
		t.Fatalf("SSR solve through the engine: ok=%v %q err_h %.1f m", s.Last.OK, s.Last.Reason, errH)
	}
	if r := e.EffectiveConfig().Receiver; r.Lat != rxHint.Lat || r.Lon != rxHint.Lon {
		t.Fatal("the SSR result changed the receiver position")
	}
	next = config.Clone(e.Config().Get())
	next.SSR.Enabled = false
	if _, _, err := e.ApplyConfig(next); err != nil {
		t.Fatal(err)
	}
	if e.SSRStatus().Enabled {
		t.Fatal("SSR still running after it was switched off")
	}
}

// servePaced serves data to the first client in n chunks, one every
// interval: the Beast feed decides between GPS and 12 MHz timestamps only
// after 5 s of wall time, so a stream sent at once would arrive untimed.
func servePaced(t *testing.T, data []byte, n int, interval time.Duration) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		step := (len(data) + n - 1) / n
		for off := 0; off < len(data); off += step {
			if _, err := c.Write(data[off:min(off+step, len(data))]); err != nil {
				return
			}
			select {
			case <-done:
				return
			case <-time.After(interval):
			}
		}
		<-done
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func geoDistM(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371e3
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := p2-p1, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// TestWire45TrackerShards: aggregator.limits.trackerShards selects the
// global tracker's ICAO shards; a standalone tracker ignores it.
func TestWire45TrackerShards(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	sets := []string{"mode=aggregator", "ntp.enabled=false", "aggregator.beastPort=0", "aggregator.avfPort=0",
		"aggregator.mlatPort=0", "aggregator.sbsInLocalPort=0", "aggregator.limits.trackerShards=4"}
	cfg := model.DefaultConfig()
	cfg.Feeds = nil
	agg := startEngine(t, t.TempDir(), cfg, Options{ListenHost: "127.0.0.1", ConfigSets: sets})
	if n := agg.Tracker().LockStats().Shards; n != 4 {
		t.Fatalf("aggregator tracker shards %d, want 4", n)
	}
	sa := startEngine(t, t.TempDir(), cfg, Options{ConfigSets: []string{"ntp.enabled=false", "aggregator.limits.trackerShards=4"}})
	if n := sa.Tracker().LockStats().Shards; n > 1 {
		t.Fatalf("standalone tracker sharded: %d", n)
	}
}

// fedNodeID creates the federation identity of a data directory (what
// `airview federation id` does) and returns the node ID.
func fedNodeID(t *testing.T, dir string) string {
	t.Helper()
	m, err := federation.LoadMaterial(dir, &federation.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m.Identity.FeederID()
}

// TestWire45Federation: two aggregators federated over AVFED/1 on
// loopback. Network A has a host receiver; network B has none. B receives
// A's public picture, and A's Beast out-feed serves its own aircraft to a
// third party.
func TestWire45Federation(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	idA, idB := fedNodeID(t, dirA), fedNodeID(t, dirB)
	fedA, fedB, outFeed := freePort(t), freePort(t), freePort(t)
	specs, streams := simStreams(t, 1, 40*time.Second)

	base := func(name string) model.Config {
		c := model.DefaultConfig()
		c.Mode = model.ModeAggregator
		c.NTP.Enabled = false
		c.Feeds = nil
		c.Aggregator.Name = name
		c.Aggregator.Admission = model.AdmissionOpen
		c.Aggregator.AVFPort, c.Aggregator.MLATPort, c.Aggregator.BeastPort, c.Aggregator.SBSInLocalPort = 0, 0, 0, 0
		c.Aggregator.Federation.Enabled = true
		c.Aggregator.Federation.IntervalMs = 200
		return c
	}
	ca := base("Net A")
	ca.Receiver = model.Receiver{Name: "A-host", Located: true, Lat: specs[0].Lat, Lon: specs[0].Lon, GroundElevM: 100}
	ca.Feeds = []model.Feed{{ID: "evo", Name: "EVO", Type: model.FeedBeast, Host: "127.0.0.1", Port: servePaced(t, streams[0], 160, 250*time.Millisecond), Enabled: true}}
	ca.Aggregator.Federation.Name = "net-a"
	ca.Aggregator.Federation.Listen = "127.0.0.1:" + strconv.Itoa(fedA)
	ca.Aggregator.Federation.Peers = []model.AggFedPeer{{Name: "b", ID: idB, Addr: "127.0.0.1:" + strconv.Itoa(fedB)}}
	ca.Aggregator.Federation.Feeds = []model.AggFedFeed{{Name: "out", Format: "beast", Mode: "listen",
		Addr: "127.0.0.1:" + strconv.Itoa(outFeed), Allow: []string{"127.0.0.0/8"}}}
	cb := base("Net B")
	cb.Aggregator.Federation.Name = "net-b"
	cb.Aggregator.Federation.Listen = "127.0.0.1:" + strconv.Itoa(fedB)
	cb.Aggregator.Federation.Peers = []model.AggFedPeer{{Name: "a", ID: idA}} // accept only

	a := startEngine(t, dirA, ca, Options{ListenHost: "127.0.0.1"})
	b := startEngine(t, dirB, cb, Options{ListenHost: "127.0.0.1"})
	makeAdmin(t, a.Accounts())
	makeAdmin(t, b.Accounts())

	waitFor(t, "federation nodes", 20*time.Second, func() bool { return a.Federation() != nil && b.Federation() != nil })
	var outBytes atomic.Int64
	oc, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(outFeed), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer oc.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := oc.Read(buf)
			outBytes.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	if a.Federation().ID() != idA || b.Federation().ID() != idB {
		t.Fatalf("node IDs %s/%s, want %s/%s", a.Federation().ID(), b.Federation().ID(), idA, idB)
	}
	waitFor(t, "A's aircraft at B", 60*time.Second, func() bool {
		return b.Federation().Status().Remote.Aircraft > 0 && withPositions(a) > 0
	})
	sa, sb := a.Federation().Status(), b.Federation().Status()
	if len(sa.Peers) != 1 || sa.Peers[0].State != "connected" || sa.Peers[0].AircraftOut == 0 {
		t.Fatalf("A peers %+v", sa.Peers)
	}
	if len(sb.Peers) != 1 || sb.Peers[0].AircraftIn == 0 {
		t.Fatalf("B peers %+v", sb.Peers)
	}
	// B's own tracker stays empty (inject is off): federation data is an
	// overlay only.
	if withPositions(b) != 0 {
		t.Fatalf("remote aircraft entered B's tracker: %d", withPositions(b))
	}
	t.Logf("federation: A exports %d aircraft, B holds %d remote aircraft", sa.Peers[0].AircraftOut, sb.Remote.Aircraft)

	// A's Beast out-feed (listen mode) serves its own aircraft.
	waitFor(t, "bytes from A's Beast out-feed", 20*time.Second, func() bool { return outBytes.Load() > 64 })

	// Switching federation off at run time closes the node.
	next := config.Clone(b.Config().Get())
	next.Aggregator.Federation.Enabled = false
	if _, _, err := b.ApplyConfig(next); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B's node closed", 10*time.Second, func() bool { return b.Federation() == nil })
}

// acTee hands a simnet receiver's frames to an uplink manager and, for
// every 10th timed frame, a Mode A/C reply with the same timing fields.
type acTee struct {
	n    *simnet.Network
	m    *uplink.Manager
	id   string
	k    int
	sent *atomic.Int64
}

func (r *acTee) Observe(o *simnet.Observation) {
	f := r.n.RawFrame(o)
	f.FeedID, f.Station = r.id, 0
	r.m.HandleFrame(f)
	r.k++
	if r.k%10 == 0 && f.ClockCls.Timed() && !f.MLAT {
		raw := track.ACFromSquawk(0o1234)
		ac := f
		ac.Kind, ac.Data = model.FrameModeAC, []byte{byte(raw >> 8), byte(raw)}
		r.m.HandleFrame(ac)
		r.sent.Add(1)
	}
}
func (r *acTee) Advance(float64) {}

// TestWire45ModeACOnOff: with aggregator.mlat.modeAc the timed Mode A/C
// replies of AVF stations reach the MLAT engine's Mode A/C stage; without
// it they are not routed there.
func TestWire45ModeACOnOff(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("modeAc=%v", on), func(t *testing.T) {
			avfPort := freePort(t)
			cfg := model.DefaultConfig()
			cfg.Feeds = nil
			e := startEngine(t, t.TempDir(), cfg, Options{ListenHost: "127.0.0.1", ConfigSets: []string{
				"mode=aggregator", "ntp.enabled=false", "aggregator.admission=open", "aggregator.beastPort=0",
				"aggregator.mlatPort=0", "aggregator.sbsInLocalPort=0", "aggregator.avfPort=" + strconv.Itoa(avfPort),
				"aggregator.mlat.modeAc=" + strconv.FormatBool(on),
			}})
			makeAdmin(t, e.Accounts())
			waitFor(t, "admin ready", 10*time.Second, e.AdminReady)
			pin := e.NetworkStatus().AVFPin
			specs := simnet.RandomReceivers(7, 3, simnet.Placement{CenterLat: 50, CenterLon: 9, RadiusKm: 80})
			n, err := simnet.New(simnet.Config{Seed: 7, Start: time.Now().UTC(), Aircraft: 40, Receivers: specs})
			if err != nil {
				t.Fatal(err)
			}
			var sent atomic.Int64
			for i := range specs {
				sp := n.Receiver(i)
				id := fmt.Sprintf("rx%d", i)
				loc := &avf.Location{Lat: sp.Lat, Lon: sp.Lon, Alt: sp.HAE, AltRef: model.AltRefHAE, Source: model.LocSrcManual}
				m, err := uplink.NewManager(uplink.Options{Software: "test", Logf: func(string, ...any) {},
					Source: uplink.SourceFuncs{ReceiverFn: func(string) uplink.ReceiverInfo {
						return uplink.ReceiverInfo{RxNo: 1, Name: id, Connected: true, InputType: "beast", Location: loc}
					}}})
				if err != nil {
					t.Fatal(err)
				}
				u := model.DefaultUplink()
				u.ID, u.Host, u.Port, u.ServerPin, u.SpoolMB = "agg", "127.0.0.1", avfPort, pin, 0
				m.Apply([]model.Uplink{u})
				defer m.Close()
				n.Attach(i, &acTee{n: n, m: m, id: id, sent: &sent})
			}
			reached := func() uint64 {
				me := e.MLAT()
				if me == nil {
					return 0
				}
				s := me.ModeACStats()
				return s.Replies + s.Unsynced + s.Overflow
			}
			start, simd := time.Now(), time.Duration(0)
			deadline := start.Add(25 * time.Second)
			for time.Now().Before(deadline) && !(on && reached() > 50) {
				if err := n.Advance(context.Background(), 100*time.Millisecond); err != nil {
					t.Fatal(err)
				}
				simd += 100 * time.Millisecond
				if d := time.Until(start.Add(simd)); d > 0 {
					time.Sleep(d)
				}
				if !on && simd > 12*time.Second {
					break
				}
			}
			me := e.MLAT()
			if me == nil {
				t.Fatal("no MLAT engine")
			}
			st := me.ModeACStats()
			t.Logf("modeAc=%v: sent %d Mode A/C replies; engine %+v; avf frames %d", on, sent.Load(), st, e.NetworkStatus().AVFStats.Frames)
			if sent.Load() == 0 {
				t.Fatal("the test sent no Mode A/C replies")
			}
			if on && (!st.Enabled || reached() == 0) {
				t.Fatalf("Mode A/C replies did not reach the engine: %+v", st)
			}
			if !on && (st.Enabled || reached() != 0) {
				t.Fatalf("Mode A/C stage active while off: %+v", st)
			}
		})
	}
}
