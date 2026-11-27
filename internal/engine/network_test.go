package engine

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"testing"
	"time"

	"airview/internal/auth"
	"airview/internal/avf"
	"airview/internal/config"
	"airview/internal/model"
	"airview/internal/registry"
	"airview/internal/sim/simnet"
	"airview/internal/uplink"
	"airview/internal/uplink/avftest"
)

// freePort returns a currently unused loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// simStreams renders d of simnet traffic as one Beast byte stream per
// receiver (receiver 0 plain Beast for the host feed, the others readsb
// beast_reduce_plus with their UUID).
func simStreams(t *testing.T, nrx int, d time.Duration) ([]simnet.ReceiverSpec, [][]byte) {
	t.Helper()
	specs := simnet.RandomReceivers(3, nrx, simnet.Placement{CenterLat: 50, CenterLon: 9, RadiusKm: 120})
	n, err := simnet.New(simnet.Config{Seed: 3, Start: time.Now().UTC().Add(-d), Aircraft: 80, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	bufs := make([]*bytes.Buffer, nrx)
	for i := range bufs {
		bufs[i] = &bytes.Buffer{}
		opt := simnet.BeastOptions{}
		if i > 0 {
			opt.Format = simnet.BeastReducePlus
		}
		n.NewBeastEmitter(i, &simnet.WriterSink{W: bufs[i]}, opt)
	}
	if err := n.Advance(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, nrx)
	for i, b := range bufs {
		out[i] = b.Bytes()
	}
	return specs, out
}

// serveOnce serves data to the first client and then keeps the connection
// open until the test ends.
func serveOnce(t *testing.T, data []byte) int {
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
		_, _ = c.Write(data)
		<-done
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// makeAdmin creates the admin account with a confirmed authenticator.
func makeAdmin(t *testing.T, svc *auth.Service) {
	t.Helper()
	ctx := context.Background()
	setup, err := svc.InitAdmin(ctx, "root", "correct horse battery staple 42")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := auth.ParseTOTPSecret(setup.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmAdminTOTP(ctx, "root", sec.Code(time.Now())); err != nil {
		t.Fatal(err)
	}
}

func TestAggregatorNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	dir := t.TempDir()
	before := runtime.NumGoroutine()
	specs, streams := simStreams(t, 4, 40*time.Second)

	beastPort, sbsPort := freePort(t), freePort(t)
	cfg := model.DefaultConfig()
	cfg.Mode = model.ModeAggregator
	cfg.NTP.Enabled = false
	cfg.Receiver = model.Receiver{Name: "Club EVO", Located: true, Lat: specs[0].Lat, Lon: specs[0].Lon, GroundElevM: 100}
	cfg.Feeds = []model.Feed{{ID: "evo", Name: "EVO", Type: model.FeedBeast, Host: "127.0.0.1",
		Port: serveOnce(t, streams[0]), Enabled: true}}
	cfg.Aggregator.Admission = model.AdmissionOpen
	cfg.Aggregator.AVFPort, cfg.Aggregator.MLATPort = 0, 0
	cfg.Aggregator.BeastPort, cfg.Aggregator.SBSInLocalPort = beastPort, sbsPort
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}

	e := New(dir, Options{Offline: true, Logger: quietLogger(), ListenHost: "127.0.0.1"})
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			e.Stop()
		}
	}()
	if e.Mode() != model.ModeAggregator || !e.Aggregator() || e.Registry() == nil || e.Accounts() == nil {
		t.Fatalf("mode %q aggregator=%v", e.Mode(), e.Aggregator())
	}
	if sdb := e.StationsDB(); sdb == nil || !sdb.HasTable("stats_minute") || sdb.HasTable("flights") {
		t.Fatal("stations.db statistics store not opened")
	}
	if e.HostSID() == "" || e.Status().Mode != model.ModeAggregator {
		t.Fatalf("host sid %q status mode %q", e.HostSID(), e.Status().Mode)
	}
	host, ok := e.Registry().Station(model.HostStation)
	if !ok || host.Kind != model.StationKindHost || !host.Located || host.Name != "Club EVO" {
		t.Fatalf("host station %+v", host)
	}
	if a := e.StationAnalyzer(host.SID); a == nil || e.StationAnalyzer("nosuchstation") != nil {
		t.Fatal("per-station analyzer not available")
	}

	// The host feed reaches the map as station #1 before any admin exists.
	waitFor(t, "host station frames", 20*time.Second, func() bool {
		return e.NetworkStatus().HostFrames > 100 && withPositions(e) > 0
	})

	// Feeder listeners stay closed until the admin exists (DESIGN §7).
	ns := e.NetworkStatus()
	if !ns.Beast.Listening || ns.AdminReady {
		t.Fatalf("listener %+v adminReady %v", ns.Beast, ns.AdminReady)
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(beastPort))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write(streams[1][:256])
	waitFor(t, "gate rejection", 10*time.Second, func() bool {
		bs := e.NetworkStatus().BeastStats
		return bs != nil && bs.Rejected["not ready"] > 0
	})
	c.Close()

	makeAdmin(t, e.Accounts())
	waitFor(t, "admin ready", 10*time.Second, func() bool { return e.AdminReady() })
	waitFor(t, "host owner", 10*time.Second, func() bool {
		h, _ := e.Registry().Station(model.HostStation)
		return h.OwnerID != 0
	})

	// Three compat Beast feeders (readsb beast_reduce_plus with UUIDs).
	var conns []net.Conn
	for i := 1; i < len(streams); i++ {
		c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(beastPort))
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		go func(c net.Conn, b []byte) { _, _ = c.Write(b) }(c, streams[i])
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	waitFor(t, "four stations with lanes", 20*time.Second, func() bool {
		s := e.NetworkStatus()
		return s.Stations[model.AdmApproved] >= 4 && s.Lanes >= 4 && s.Core != nil && s.Core.Joined > 0
	})
	s := e.NetworkStatus()
	if s.Core.Groups == 0 || s.Core.Copies <= s.Core.Groups {
		t.Fatalf("ingest %+v", *s.Core)
	}
	for _, l := range e.Lanes().List() {
		if l.Frames == 0 {
			t.Fatalf("lane without frames: %+v", l)
		}
	}

	// SBS-in bridge: an external mlat-server's position enters as mlat-ext.
	bc, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(sbsPort))
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("MSG,3,1,1,ABCDEF,1,2026/11/13,12:00:00.000,2026/11/13,12:00:00.000,,35000,,,%.5f,%.5f,,,0,0,0,0\r\n", specs[0].Lat+0.2, specs[0].Lon+0.1)
	for i := 0; i < 3; i++ {
		_, _ = bc.Write([]byte(line))
		time.Sleep(50 * time.Millisecond)
	}
	waitFor(t, "bridge aircraft", 10*time.Second, func() bool {
		a, ok := e.Tracker().Aircraft(0xABCDEF)
		return ok && a.Lat != nil
	})
	bc.Close()

	// Port changes follow ApplyConfig; 0 switches a listener off.
	cur := e.Config().Get()
	next := config.Clone(cur)
	next.Aggregator.BeastPort = 0
	if _, _, err := e.ApplyConfig(next); err != nil {
		t.Fatal(err)
	}
	if st := e.NetworkStatus(); st.Beast.Listening {
		t.Fatalf("Beast listener still running: %+v", st.Beast)
	}
	if e.RestartRequired() {
		t.Fatal("restart required for a port change")
	}
	// Leaving aggregator mode needs a restart.
	next = config.Clone(e.Config().Get())
	next.Mode = model.ModeStandalone
	if _, _, err := e.ApplyConfig(next); err != nil {
		t.Fatal(err)
	}
	if !e.RestartRequired() || e.Mode() != model.ModeAggregator {
		t.Fatalf("restartRequired=%v mode=%q", e.RestartRequired(), e.Mode())
	}

	e.Stop()
	stopped = true
	if n := settledGoroutines(before + 5); n > before+5 {
		buf := make([]byte, 1<<20)
		t.Fatalf("goroutines: %d before, %d after\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}

	// The registry survives a restart: the host keeps its SID.
	sid := host.SID
	e2 := New(dir, Options{Offline: true, Logger: quietLogger(), ListenHost: "127.0.0.1",
		ConfigSets: []string{"mode=aggregator", "aggregator.beastPort=0", "aggregator.sbsInLocalPort=0"}})
	if err := e2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e2.Stop()
	if e2.HostSID() != sid || !e2.AdminReady() {
		t.Fatalf("after restart: sid %q (was %q) adminReady %v", e2.HostSID(), sid, e2.AdminReady())
	}
}

func TestStandaloneHasNoNetwork(t *testing.T) {
	dir := t.TempDir()
	e := New(dir, Options{Demo: true, Offline: true, DemoSeed: 7, Logger: quietLogger(),
		ConfigSets: []string{"mode=aggregator"}})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	// -demo forces standalone even when the file says aggregator.
	if e.Aggregator() || e.Mode() != model.ModeStandalone || e.Registry() != nil || e.Lanes() != nil {
		t.Fatalf("aggregator=%v mode=%q", e.Aggregator(), e.Mode())
	}
	if !e.AdminReady() || e.HostSID() != "" || e.NetworkStatus().Aggregator {
		t.Fatal("network state in standalone mode")
	}
}

func TestUplinkTee(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	stub := avftest.Start(t, avftest.Options{})
	_, streams := simStreams(t, 1, 20*time.Second)
	dir := t.TempDir()
	cfg := model.DefaultConfig()
	cfg.NTP.Enabled = false
	cfg.Receiver = model.Receiver{Name: "rx", Located: true, Lat: 50, Lon: 9}
	cfg.Feeds = []model.Feed{
		{ID: "evo", Name: "EVO", Type: model.FeedBeast, Host: "127.0.0.1", Port: serveOnce(t, streams[0]), Enabled: true},
	}
	cfg.Uplinks = []model.Uplink{stub.Uplink("club")}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	e := New(dir, Options{Offline: true, Logger: quietLogger()})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	if e.Uplinks() == nil {
		t.Fatal("no uplink manager")
	}
	waitFor(t, "frames at the aggregator", 20*time.Second, func() bool { return stub.FrameCount() > 50 })
	if withPositions(e) == 0 {
		t.Fatal("local picture lost its aircraft")
	}
	if errs := stub.Errors(); len(errs) > 0 {
		t.Fatalf("protocol errors at the stub: %v", errs)
	}
	// Removing the uplink stops it; the manager stays (identity kept).
	next := config.Clone(e.Config().Get())
	next.Uplinks = nil
	if _, _, err := e.ApplyConfig(next); err != nil {
		t.Fatal(err)
	}
	if m := e.Uplinks(); m == nil || m.Active() {
		t.Fatalf("manager after removal: %v", m)
	}
}

// avfTestFeed hands a simnet receiver's observations to an uplink manager.
type avfTestFeed struct {
	n  *simnet.Network
	m  *uplink.Manager
	id string
}

func (r *avfTestFeed) Observe(o *simnet.Observation) {
	f := r.n.RawFrame(o)
	f.FeedID, f.Station = r.id, 0
	r.m.HandleFrame(f)
}
func (r *avfTestFeed) Advance(float64) {}

func TestAggregatorAVF(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	dir := t.TempDir()
	avfPort := freePort(t)
	e := New(dir, Options{Offline: true, Logger: quietLogger(), ListenHost: "127.0.0.1", ConfigSets: []string{
		"mode=aggregator", "ntp.enabled=false", "aggregator.admission=open", "aggregator.beastPort=0",
		"aggregator.mlatPort=0", "aggregator.avfPort=" + strconv.Itoa(avfPort),
	}})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	ns := e.NetworkStatus()
	if !ns.AVF.Listening || ns.AVFPin == "" {
		t.Fatalf("AVF listener %+v pin %q", ns.AVF, ns.AVFPin)
	}
	makeAdmin(t, e.Accounts())
	waitFor(t, "admin ready", 10*time.Second, func() bool { return e.AdminReady() })

	specs := simnet.RandomReceivers(5, 3, simnet.Placement{CenterLat: 50, CenterLon: 9, RadiusKm: 100})
	n, err := simnet.New(simnet.Config{Seed: 5, Start: time.Now().UTC(), Aircraft: 60, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	var mgrs []*uplink.Manager
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
		u.ID, u.Host, u.Port, u.ServerPin, u.SpoolMB = "agg", "127.0.0.1", avfPort, ns.AVFPin, 0
		m.Apply([]model.Uplink{u})
		defer m.Close()
		mgrs = append(mgrs, m)
		n.Attach(i, &avfTestFeed{n: n, m: m, id: id})
	}
	start := time.Now()
	sim := time.Duration(0)
	deadline := start.Add(30 * time.Second)
	ok := func() bool {
		s := e.NetworkStatus()
		located := 0
		for _, st := range e.Registry().List(registry.Filter{}) {
			if st.Kind == model.StationKindAVF && st.Located {
				located++
			}
		}
		return s.AVFStats != nil && s.AVFStats.Frames > 500 && located == 3 && s.Lanes >= 3 && withPositions(e) > 0
	}
	for !ok() {
		if time.Now().After(deadline) {
			s := e.NetworkStatus()
			t.Fatalf("AVF feeders did not reach the map: avf %+v core %+v lanes %d", s.AVFStats, s.Core, s.Lanes)
		}
		if err := n.Advance(context.Background(), 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		sim += 100 * time.Millisecond
		if d := time.Until(start.Add(sim)); d > 0 {
			time.Sleep(d)
		}
	}
}
