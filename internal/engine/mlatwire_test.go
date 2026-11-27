package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"airview/internal/geoid"
	"airview/internal/mlat"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/registry"
)

func TestMLATStationInfo(t *testing.T) {
	st := registry.Station{ClockCls: model.ClockSDR12,
		Declared: registry.Position{Lat: 50, Lon: 8, Alt: 100, Valid: true}, DeclaredAltRef: model.AltRefMSL}
	in := mlatStationInfo(&st)
	n, _ := geoid.Undulation(50, 8)
	if !in.Declared.Valid || in.Declared.SigmaM != 0 || in.GNSS.Valid || in.Confirmed.Valid || in.Class != model.ClockSDR12 {
		t.Fatalf("declared msl %+v", in)
	}
	if h := geo.FromECEF(in.Declared.ECEF).H; math.Abs(h-(100+n)) > 0.01 {
		t.Fatalf("msl→hae %v want %v", h, 100+n)
	}
	st.DeclaredAltRef = model.AltRefAmbiguous
	if in := mlatStationInfo(&st); math.Abs(in.Declared.SigmaM-math.Hypot(30, math.Abs(n)/2)) > 1e-9 ||
		math.Abs(geo.FromECEF(in.Declared.ECEF).H-(100+n/2)) > 0.01 {
		t.Fatalf("ambiguous σ %v h %v", in.Declared.SigmaM, geo.FromECEF(in.Declared.ECEF).H)
	}
	st.DeclaredAltRef = model.AltRefHAE
	if h := geo.FromECEF(mlatStationInfo(&st).Declared.ECEF).H; math.Abs(h-100) > 0.01 {
		t.Fatalf("hae %v", h)
	}
	// Host station with its own GNSS: Effective with LocGNSS.
	host := registry.Station{Located: true, LocStatus: model.LocGNSS, Effective: model.StationGeo{Lat: 1, Lon: 2, HAE: 3}}
	if in := mlatStationInfo(&host); !in.GNSS.Valid || in.Confirmed.Valid {
		t.Fatalf("gnss %+v", in)
	}
	conf := registry.Station{Located: true, LocStatus: model.LocConfirmed, Effective: model.StationGeo{Lat: 1, Lon: 2, HAE: 3}}
	if in := mlatStationInfo(&conf); !in.Confirmed.Valid || in.GNSS.Valid {
		t.Fatalf("confirmed %+v", in)
	}
	sus := st
	sus.LocStatus = model.LocSuspect
	sus.Estimated = registry.Position{Lat: 50.01, Lon: 8, Alt: 100, Valid: true}
	if in := mlatStationInfo(&sus); !in.Suspect || in.DiscrepancyM < 1000 || in.DiscrepancyM > 1200 {
		t.Fatalf("suspect %+v", in)
	}
}

// Finding ROB-4: NoStatus and Flagged never reached the engine, so
// mlat-client radarcape_gps stations could not anchor and the admin could
// not bar a bad anchor.
func TestMLATStationInfoAnchorFields(t *testing.T) {
	gps := registry.Station{Kind: model.StationKindMLATC, ClockCls: model.ClockGPSUTC}
	if in := mlatStationInfo(&gps); !in.NoStatus || in.Flagged {
		t.Fatalf("mlat-client radarcape_gps: %+v", in)
	}
	for _, st := range []registry.Station{
		{Kind: model.StationKindMLATC, ClockCls: model.ClockHW12},
		{Kind: model.StationKindAVF, ClockCls: model.ClockGPSUTC},
		{Kind: model.StationKindHost, ClockCls: model.ClockGPSUTC},
	} {
		if in := mlatStationInfo(&st); in.NoStatus {
			t.Fatalf("%s %v must keep the status check", st.Kind, st.ClockCls)
		}
	}
	barred := registry.Station{Kind: model.StationKindAVF, ClockCls: model.ClockGPSUTC, Flags: registry.FlagNoAnchor}
	if in := mlatStationInfo(&barred); !in.Flagged {
		t.Fatal("FlagNoAnchor not passed as Flagged")
	}
	distrusted := registry.Station{Kind: model.StationKindAVF, ClockCls: model.ClockGPSUTC, Trust: -5}
	if in := mlatStationInfo(&distrusted); !in.Flagged {
		t.Fatal("negative trust not passed as Flagged")
	}
	// a change of the flag is a change of the description, which the
	// station sync compares to decide whether to push it again
	unbarred := barred
	unbarred.Flags = 0
	if mlatStationInfo(&barred) == mlatStationInfo(&unbarred) {
		t.Fatal("flag change not visible to the sync comparison")
	}
}

type fakeMLATTarget struct {
	set map[model.StationIdx]int
	rm  []model.StationIdx
}

func (f *fakeMLATTarget) SetStation(st model.StationIdx, _ mlat.StationInfo) { f.set[st]++ }
func (f *fakeMLATTarget) RemoveStation(st model.StationIdx)                  { f.rm = append(f.rm, st) }

func TestAggregatorMLAT(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	dir := t.TempDir()
	port := freePort(t)
	e := New(dir, Options{Offline: true, Logger: quietLogger(), ListenHost: "127.0.0.1", ConfigSets: []string{
		"mode=aggregator", "ntp.enabled=false", "aggregator.admission=open", "aggregator.beastPort=0",
		"aggregator.avfPort=0", "aggregator.mlatPort=" + strconv.Itoa(port),
	}})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	eng := e.MLAT()
	if eng == nil {
		t.Fatal("MLAT engine not running")
	}
	ns := e.NetworkStatus()
	if !ns.MLATC.Listening || ns.MLAT == nil || ns.MLATCStats == nil {
		t.Fatalf("mlatc %+v mlat %v", ns.MLATC, ns.MLAT != nil)
	}
	if hc := e.HealthChecks(); hc["mlat"].State != "ok" || hc["listener.mlatc"].Fail {
		t.Fatalf("health %+v", hc)
	}
	makeAdmin(t, e.Accounts())
	waitFor(t, "admin ready", 10*time.Second, func() bool { return e.AdminReady() })

	// An mlat-client handshake creates an mlatc station with the declared
	// position, which the sync loop pushes into the MLAT engine.
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hello, _ := json.Marshal(map[string]any{
		"version": 3, "client_version": "test-0.1", "compress": []string{"none"},
		"selective_traffic": true, "heartbeat": true, "return_results": true, "udp_transport": false,
		"return_result_format": "ecef", "uuid": "0123456789abcdef0123456789abcdef", "user": "tester",
		"lat": 50.1, "lon": 8.6, "alt": 120, "clock_type": "dump1090",
	})
	_, _ = c.Write(append(append(hello, strings.Repeat(" ", 128)...), '\n'))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil || strings.Contains(string(line), "deny") {
		t.Fatalf("handshake reply %q %v", line, err)
	}
	var idx model.StationIdx
	waitFor(t, "mlatc station", 10*time.Second, func() bool {
		for _, st := range e.Registry().List(registry.Filter{Kind: model.StationKindMLATC}) {
			idx = st.Idx
			return st.Declared.Valid
		}
		return false
	})
	waitFor(t, "station in the MLAT engine", 3*mlatStationSyncEvery, func() bool {
		return eng.PositionSource(idx) == mlat.PosDeclared
	})

	// The sync pass removes stations that are no longer approved.
	f := &fakeMLATTarget{set: map[model.StationIdx]int{}}
	last := map[model.StationIdx]mlat.StationInfo{9999: {}}
	syncMLATStations(e.Registry(), f, last)
	if f.set[idx] != 1 || len(f.rm) != 1 || f.rm[0] != 9999 {
		t.Fatalf("sync %+v", f)
	}
	syncMLATStations(e.Registry(), f, last)
	if f.set[idx] != 1 {
		t.Fatal("unchanged station pushed again")
	}
}

// The SBS-in bridge can be switched off at run time while the own MLAT
// engine keeps running; returnResults off applies at once, everything else
// in aggregator.mlat needs a restart (WP-E2).
func TestBridgeOffAndMLATRestartRule(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	dir := t.TempDir()
	sbsPort := freePort(t)
	e := New(dir, Options{Offline: true, Logger: quietLogger(), ListenHost: "127.0.0.1", ConfigSets: []string{
		"mode=aggregator", "ntp.enabled=false", "aggregator.beastPort=0", "aggregator.avfPort=0", "aggregator.mlatPort=0",
	}})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	if e.MLAT() == nil {
		t.Fatal("MLAT engine not running")
	}
	apply := func(fn func(c *model.Config)) {
		t.Helper()
		next := e.Config().Get()
		fn(&next)
		if _, _, err := e.ApplyConfig(next); err != nil {
			t.Fatal(err)
		}
	}
	apply(func(c *model.Config) { c.Aggregator.SBSInLocalPort = sbsPort })
	if s := e.NetworkStatus().SBSIn; !s.Listening {
		t.Fatalf("bridge not listening: %+v", s)
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(sbsPort))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	apply(func(c *model.Config) { c.Aggregator.SBSInLocalPort = 0 })
	if s := e.NetworkStatus().SBSIn; s.Listening {
		t.Fatalf("bridge still listening: %+v", s)
	}
	if hc := e.HealthChecks(); hc["listener.sbsIn"].State != "off" || hc["mlat"].State != "ok" {
		t.Fatalf("health %+v", hc)
	}
	if _, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(sbsPort), time.Second); err == nil {
		t.Fatal("bridge port still accepts connections")
	}
	if e.RestartRequired() {
		t.Fatal("restart required after a bridge change")
	}
	apply(func(c *model.Config) { c.Aggregator.MLAT.ReturnResults = false })
	if e.RestartRequired() || e.MLAT() == nil {
		t.Fatal("returnResults off needs no restart")
	}
	apply(func(c *model.Config) { c.Aggregator.MLAT.ReturnResults = true })
	if e.RestartRequired() {
		t.Fatal("returnResults back on (as built) needs no restart")
	}
	apply(func(c *model.Config) { c.Aggregator.MLAT.Holdout = 0.2 })
	if !e.RestartRequired() {
		t.Fatal("a hold-out change needs a restart")
	}
}

func TestMLATChangedRule(t *testing.T) {
	boot := model.DefaultAggregator().MLAT
	nc := &netCore{mlatBoot: boot}
	cfg := model.DefaultConfig()
	cfg.Mode = model.ModeAggregator
	cfg.Aggregator.MLAT = boot
	if nc.mlatChanged(cfg) {
		t.Fatal("unchanged")
	}
	cfg.Aggregator.MLAT.ReturnResults = false
	if nc.mlatChanged(cfg) {
		t.Fatal("returnResults off is live")
	}
	nc.mlatBoot.ReturnResults = false
	cfg.Aggregator.MLAT.ReturnResults = true
	if !nc.mlatChanged(cfg) {
		t.Fatal("returnResults on after a boot without it needs a restart")
	}
	cfg.Mode = model.ModeStandalone
	if nc.mlatChanged(cfg) || (*netCore)(nil).mlatChanged(cfg) {
		t.Fatal("standalone")
	}
}
