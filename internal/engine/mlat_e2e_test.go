package engine

import (
	"context"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"airview/internal/config"
	"airview/internal/engine/simfeed"
	"airview/internal/geoid"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/registry"
	"airview/internal/sim/simnet"
)

// TestMLATEndToEnd is the WP-E2 acceptance test (DESIGN §14 phase 2,
// §13.2): one simnet world heard by
//
//   - 4 native AirView feeders on AVF/1 (GPS Radarcapes) and 1 more that
//     withholds its timestamps (uplink "timestamps": false);
//   - 3 mlat-clients on 31090 (2 GPS Radarcapes, 1 Beast 12 MHz) speaking
//     the reference client's timestamp convention;
//   - 2 readsb beast_reduce_plus links on the Beast port (map only);
//   - 1 standalone AirView (its own Engine, Beast feed from the simulated
//     receiver) uplinked to the aggregator over AVF/1;
//
// all over real sockets into a real aggregator Engine. It checks that MLAT
// positions of Mode S-only aircraft reach the global map with grades and
// σ, measures them against the simulator's truth, that results reach the
// timed contributors (AVF, mlat-client and the standalone station, whose
// local map shows them) and only aircraft each of them heard, that the
// station without timestamps and the Beast links get none, and that the
// metrics land in mlat_minute / station_clock_minute.
//
// Real time: 1-4 minutes (it ends as soon as everything was seen).
func TestMLATEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time end-to-end test")
	}
	maxDur := 4 * time.Minute
	if s := os.Getenv("MLAT_E2E_MAX"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			maxDur = d
		}
	}

	// --- the aggregator
	aggDir := t.TempDir()
	avfPort, beastPort, mlatPort := freePort(t), freePort(t), freePort(t)
	agg := New(aggDir, Options{Offline: true, Logger: quietLogger(), ListenHost: "127.0.0.1", ConfigSets: []string{
		"mode=aggregator", "ntp.enabled=false", "aggregator.admission=open",
		"aggregator.beastPort=" + strconv.Itoa(beastPort), "aggregator.avfPort=" + strconv.Itoa(avfPort),
		"aggregator.mlatPort=" + strconv.Itoa(mlatPort), "aggregator.limits.perIpStations=64",
		"aggregator.mlat.engine.syncWarmupSec=20", "aggregator.mlat.holdout=0.1",
		// The timing quarantine is on (new stations prove their timing
		// first); shortened so a 4-minute run can prove every station.
		"aggregator.mlat.quarantine.minAgeSec=30", "aggregator.mlat.quarantine.windowSec=10",
		"aggregator.mlat.quarantine.goodWindows=4",
	}})
	if err := agg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer agg.Stop()
	makeAdmin(t, agg.Accounts())
	waitFor(t, "admin ready", 10*time.Second, agg.AdminReady)
	pin := agg.NetworkStatus().AVFPin

	// --- the simulated world
	const (
		cLat, cLon = 50.0, 9.0
		nAVF       = 4
		nMLATC     = 3
		nBeast     = 2
	)
	und, _ := geoid.Undulation(cLat, cLon)
	devs := []simnet.Device{}
	for i := 0; i < nAVF; i++ {
		devs = append(devs, simnet.DeviceRadarcapeGPS)
	}
	devs = append(devs, simnet.DeviceRadarcapeGPS, simnet.DeviceRadarcapeGPS, simnet.DeviceBeast) // mlat-clients
	// Diagnostics: MLAT_E2E_SDR=1 makes every timed AVF and mlat-client
	// receiver an RTL-SDR; MLAT_E2E_MLATC_AS_AVF=1 connects the mlat-client
	// receivers over AVF/1 instead (same world, other transport).
	if os.Getenv("MLAT_E2E_SDR") == "1" {
		for i := 0; i < nAVF+nMLATC; i++ {
			devs[i] = simnet.DeviceSDR
		}
	}
	mlatcAsAVF := os.Getenv("MLAT_E2E_MLATC_AS_AVF") == "1"
	devs = append(devs, simnet.DeviceSDR, simnet.DeviceSDR) // Beast compat
	devs = append(devs, simnet.DeviceRadarcapeGPS)          // standalone
	devs = append(devs, simnet.DeviceRadarcapeGPS)          // AVF without timestamps
	const rxStandalone, rxNoTS = nAVF + nMLATC + nBeast, nAVF + nMLATC + nBeast + 1
	specs := simnet.RandomReceivers(21, len(devs), simnet.Placement{CenterLat: cLat, CenterLon: cLon, RadiusKm: 110})
	rr := rand.New(rand.NewPCG(21, 3))
	for i, d := range devs {
		specs[i].Clock = simnet.DefaultClockSpec(d, rr)
		specs[i].Reception = simnet.DefaultReception(d, rr)
	}
	n, err := simnet.New(simnet.Config{Seed: 21, Start: time.Now().UTC(), Aircraft: 160, Receivers: specs,
		ModeSOnlyFraction: 0.3, CenterLat: cLat, CenterLon: cLon, GeoidUndulationM: und})
	if err != nil {
		t.Fatal(err)
	}
	heard := simfeed.AttachHeard(n)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()

	var avfs []*simfeed.AVFFeeder
	for i := 0; i < nAVF; i++ {
		f, err := simfeed.NewAVFFeeder(n, i, simfeed.AVFOptions{Addr: "127.0.0.1:" + strconv.Itoa(avfPort), Pin: pin})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		avfs = append(avfs, f)
	}
	noTS, err := simfeed.NewAVFFeeder(n, rxNoTS, simfeed.AVFOptions{Addr: "127.0.0.1:" + strconv.Itoa(avfPort), Pin: pin, NoTimestamps: true})
	if err != nil {
		t.Fatal(err)
	}
	defer noTS.Close()
	var clients []*simfeed.MLATClient
	for i := 0; i < nMLATC; i++ {
		if mlatcAsAVF {
			f, err := simfeed.NewAVFFeeder(n, nAVF+i, simfeed.AVFOptions{Addr: "127.0.0.1:" + strconv.Itoa(avfPort), Pin: pin})
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			avfs = append(avfs, f)
			continue
		}
		c, err := simfeed.NewMLATClient(n, nAVF+i, simfeed.MLATClientOptions{Addr: "127.0.0.1:" + strconv.Itoa(mlatPort)})
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
		wg.Add(1)
		go func() { defer wg.Done(); c.Run(ctx) }()
	}
	for i := 0; i < nBeast; i++ {
		b := simfeed.NewBeastFeeder(n, nAVF+nMLATC+i, "127.0.0.1:"+strconv.Itoa(beastPort))
		wg.Add(1)
		go func() { defer wg.Done(); b.Run(ctx) }()
	}

	// --- the standalone station: its own Engine, a Beast feed served from
	// the simulated receiver, one uplink to the aggregator.
	attach := make(chan func(), 4)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		ch := make(chan []byte, 8192)
		attach <- func() {
			n.NewBeastEmitter(rxStandalone, chanSink(ch), simnet.BeastOptions{})
		}
		defer c.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-ch:
				_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err := c.Write(p); err != nil {
					return
				}
			}
		}
	}()
	sp := n.Receiver(rxStandalone)
	stDir := t.TempDir()
	scfg := model.DefaultConfig()
	scfg.NTP.Enabled = false
	scfg.Receiver = model.Receiver{Name: "standalone", Located: true, Lat: sp.Lat, Lon: sp.Lon, GroundElevM: sp.HAE - und}
	scfg.Feeds = []model.Feed{{ID: "evo", Name: "EVO", Type: model.FeedBeast, Host: "127.0.0.1",
		Port: ln.Addr().(*net.TCPAddr).Port, Enabled: true}}
	up := model.DefaultUplink()
	up.ID, up.Host, up.Port, up.ServerPin, up.SpoolMB = "agg", "127.0.0.1", avfPort, pin, 0
	scfg.Uplinks = []model.Uplink{up}
	if err := config.Save(stDir, scfg); err != nil {
		t.Fatal(err)
	}
	std := New(stDir, Options{Offline: true, Logger: quietLogger()})
	if err := std.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer std.Stop()

	// --- run in real time
	start := time.Now()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = simfeed.RealTime(ctx, n, 100*time.Millisecond, func() {
			select {
			case fn := <-attach:
				fn()
			default:
			}
		})
	}()

	type mapFix struct {
		icao       uint32
		errH, sigH float64
		grade      string
	}
	var fixes []mapFix
	mlatAC := map[uint32]bool{}
	gradesSeen := map[string]int{}
	evalMap := func() {
		now := time.Now()
		type q struct {
			a    model.Aircraft
			icao uint32
			ts   float64
			tr   simnet.AircraftTruth
			ok   bool
		}
		var qs []q
		for _, a := range agg.Tracker().Snapshot().Aircraft {
			if a.MLATGrade == "" || a.Lat == nil || a.Lon == nil || a.PosAge == nil {
				continue
			}
			icao, err := strconv.ParseUint(a.ICAO, 16, 32)
			if err != nil {
				continue
			}
			qs = append(qs, q{a: a, icao: uint32(icao), ts: n.Seconds(now.Add(-time.Duration(*a.PosAge * float64(time.Second))))})
		}
		// The simulator is not safe for concurrent use: read the truth on
		// its own goroutine (between steps).
		done := make(chan struct{})
		attach <- func() {
			for i := range qs {
				qs[i].tr, qs[i].ok = truthAt(n, qs[i].icao, qs[i].ts)
			}
			close(done)
		}
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("simulator goroutine did not answer")
		}
		for _, x := range qs {
			a, icao, tr, ok := x.a, x.icao, x.tr, x.ok
			if !ok {
				continue
			}
			sig := math.NaN()
			if a.MLATSigmaM != nil {
				sig = *a.MLATSigmaM
			}
			e := horizM(*a.Lat, *a.Lon, tr.Lat, tr.Lon)
			fixes = append(fixes, mapFix{uint32(icao), e, sig, a.MLATGrade})
			mlatAC[uint32(icao)] = true
			gradesSeen[a.MLATGrade]++
		}
	}
	standaloneMLAT := func() int {
		k := 0
		for _, a := range std.Tracker().Snapshot().Aircraft {
			if a.MLATGrade != "" && a.Lat != nil {
				k++
			}
		}
		return k
	}
	done := func() bool {
		if len(mlatAC) < 5 || standaloneMLAT() == 0 {
			return false
		}
		for _, f := range avfs {
			if f.Results.Count() == 0 {
				return false
			}
		}
		got := 0
		for _, c := range clients {
			if c.Results.Count() > 0 {
				got++
			}
		}
		// metrics: at least one full minute with MLAT published
		return (got >= 2 || len(clients) == 0) && time.Since(start) > 130*time.Second
	}
	stdSeen := 0
	for time.Since(start) < maxDur {
		time.Sleep(2 * time.Second)
		evalMap()
		stdSeen = max(stdSeen, standaloneMLAT())
		if done() {
			break
		}
	}
	elapsed := time.Since(start)
	st := agg.NetworkStatus()
	if st.MLAT == nil {
		t.Fatal("no MLAT stats")
	}
	m := st.MLAT
	t.Logf("after %s: groups %d, clusters %d, solves %d ok %d, published %d, grades %v, results sent %d refused %d, clock steps %d, stations %d",
		elapsed.Round(time.Second), m.Groups, m.Clusters, m.Solves, m.SolveOK, m.Published, m.FixGrades, m.ResultsSent,
		m.ResultsRefused, m.Clock.Steps, m.Clock.Stations)

	// 1. MLAT on the global map, with grades and σ, close to the truth.
	if len(mlatAC) == 0 {
		t.Fatal("no MLAT aircraft on the global map")
	}
	var errs []float64
	in1, nsig := 0, 0
	for _, f := range fixes {
		errs = append(errs, f.errH)
		if !math.IsNaN(f.sigH) {
			nsig++
			if f.errH <= f.sigH*1.5 { // horizontal σ is the RMS of east and north: 1.5 σ_h ≈ 68 % for 2-D
				in1++
			}
		}
		if f.grade != "A" && f.grade != "B" && f.grade != "C" {
			t.Errorf("grade %q on the map", f.grade)
		}
	}
	slices.Sort(errs)
	p50, p95 := errs[len(errs)/2], errs[len(errs)*95/100]
	t.Logf("map: %d MLAT aircraft, %d position samples vs truth: P50 %.0f m, P95 %.0f m, within 1.5σ_h %.0f %% (σ on %d), grades %v",
		len(mlatAC), len(errs), p50, p95, 100*float64(in1)/float64(max(nsig, 1)), nsig, gradesSeen)
	if p50 > 250 {
		t.Errorf("map P50 %.0f m vs truth (gate 250 m in this short, relative-time run)", p50)
	}
	if nsig < len(fixes)*9/10 {
		t.Errorf("σ missing on %d of %d map positions", len(fixes)-nsig, len(fixes))
	}

	// 2. Results reach the timed contributors, and only aircraft they heard.
	checkHeard := func(who string, rx int, r *simfeed.Results) {
		for icao := range r.Aircraft() {
			if _, ok := heard.LastHeard(rx, icao); !ok {
				t.Errorf("%s (rx %d) got a result for %06x it never heard", who, rx, icao)
			}
		}
	}
	for _, f := range avfs {
		t.Logf("AVF rx%d: %d results for %d aircraft", f.Rx, f.Results.Count(), len(f.Results.Aircraft()))
		if f.Results.Count() == 0 {
			t.Errorf("AVF feeder rx%d got no results", f.Rx)
		}
		checkHeard("AVF", f.Rx, f.Results)
		for g := range f.Results.Grades() {
			if !model.Displayable(g) {
				t.Errorf("AVF rx%d got a result of grade %d", f.Rx, g)
			}
		}
	}
	got := 0
	for _, c := range clients {
		t.Logf("mlat-client rx%d: %d results for %d aircraft, %d requested, denied %q", c.Rx, c.Results.Count(),
			len(c.Results.Aircraft()), c.Requested(), c.Denied())
		if c.Results.Count() > 0 {
			got++
		}
		checkHeard("mlat-client", c.Rx, c.Results)
	}
	if got < 2 && len(clients) > 0 {
		t.Errorf("only %d of %d mlat-clients got results", got, len(clients))
	}
	if noTS.Results.Count() != 0 {
		t.Errorf("the station without timestamps got %d results", noTS.Results.Count())
	}
	if !noTS.Connected() {
		t.Error("the station without timestamps is not connected (it still feeds the map)")
	}
	// The standalone station shows the returned results on its own map;
	// they never travel back up (uplink results are injected into the
	// tracker, not into the feed tee).
	t.Logf("standalone: up to %d MLAT aircraft on its local map", stdSeen)
	if stdSeen == 0 {
		t.Error("the standalone uplink feeder shows no MLAT results")
	}
	if ups := std.Uplinks(); ups != nil {
		for _, s := range ups.Status() {
			t.Logf("standalone uplink: state %s, results in %d injected %d", s.State, s.Counters.ResultsIn, s.Counters.ResultsInjected)
			if s.Counters.ResultsIn == 0 {
				t.Error("standalone uplink received no results")
			}
		}
	}

	// 3. The Beast links are map-only.
	for _, s := range agg.Registry().List(registry.Filter{Kind: model.StationKindBeast}) {
		if rep, ok := agg.MLAT().Station(s.Idx); ok && rep.MLAT != nil && rep.MLAT.ContribPerHour > 0 {
			t.Errorf("Beast station %s contributed to MLAT", s.SID)
		}
	}

	// 4. Metrics rows.
	if err := agg.Store().Flush(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	netRows, err := agg.Store().MLATMinutes(context.Background(), "", now-600, now)
	if err != nil {
		t.Fatal(err)
	}
	var fr, fx int64
	for _, r := range netRows {
		fr += r.Frames
		fx += r.Fixes
	}
	t.Logf("mlat_minute network rows: %d, frames %d, fixes %d", len(netRows), fr, fx)
	if g := agg.TrustGate(); g == nil {
		t.Error("timing quarantine not wired")
	} else {
		t.Logf("timing quarantine: %+v", g.Stats())
	}
	if len(netRows) == 0 || fr == 0 {
		t.Errorf("no network mlat_minute rows: %+v", netRows)
	}
	stRows := 0
	for _, s := range agg.Registry().List(registry.Filter{Kind: model.StationKindAVF}) {
		rows, _ := agg.Store().MLATMinutes(context.Background(), s.SID, now-600, now)
		stRows += len(rows)
	}
	if stRows == 0 {
		t.Error("no per-station mlat_minute rows for the AVF stations")
	}
	if sdb := agg.StationsDB(); sdb != nil {
		_ = sdb.Flush()
		k := 0
		for _, s := range agg.Registry().List(registry.Filter{Kind: model.StationKindAVF}) {
			rows, _ := sdb.StationClockMinutes(context.Background(), s.SID, now-600, now)
			k += len(rows)
		}
		if k == 0 {
			t.Error("no station_clock_minute rows")
		}
	}
}

// chanSink is a simnet.ByteSink into a channel (a slow reader loses data).
type chanSink chan []byte

func (c chanSink) Write(_ int64, p []byte) {
	select {
	case c <- append([]byte(nil), p...):
	default:
	}
}

// truthAt returns the simulator's truth for icao at t (s since the start);
// simnet.AircraftAt may panic on a snapshot boundary (HOOKS WP-M -> simnet).
func truthAt(n *simnet.Network, icao uint32, t float64) (at simnet.AircraftTruth, ok bool) {
	defer func() {
		if recover() != nil {
			at, ok = simnet.AircraftTruth{}, false
		}
	}()
	return n.AircraftAt(icao, t)
}

// horizM is the horizontal distance between two positions in metres (local
// east-north plane of the first).
func horizM(lat1, lon1, lat2, lon2 float64) float64 {
	fr := geo.NewFrame(geo.LLH{Lat: lat1, Lon: lon1})
	d := fr.ToENU(geo.ToECEF(lat2, lon2, 0))
	return math.Hypot(d.X, d.Y)
}
