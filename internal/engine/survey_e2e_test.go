package engine

import (
	"context"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"airview/internal/engine/simfeed"
	"airview/internal/geoid"
	"airview/internal/mlat"
	"airview/internal/model"
	"airview/internal/registry"
	"airview/internal/sim/simnet"
)

// TestSurveyAutoUseEndToEnd is the WP-W3-wire acceptance test of the
// survey wiring (DESIGN §10.1-10.3, §14 phase 3, §19.3 AutoUseSurvey): one
// simnet world over real sockets into a real aggregator Engine,
//
//   - 6 located native feeders (GPS Radarcapes) declaring their true
//     positions: the reference network;
//   - 1 native feeder with GPS timing that declares no position at all.
//
// The aggregator must schedule a survey of the unlocated feeder on its own,
// the survey must finish stable within AutoUseMaxSigmaM, the MLAT engine
// must use it (PositionSource "survey"), and the registry must follow
// (located, source/status "estimated", within 3 σ + 30 m of the truth), with
// the owner's notice in the audit log.
//
// Real time 3-10 minutes, so it only runs with AIRVIEW_E2E_SURVEY=1:
//
//	AIRVIEW_E2E_SURVEY=1 go test ./internal/engine -run TestSurveyAutoUseEndToEnd -v -timeout 20m
func TestSurveyAutoUseEndToEnd(t *testing.T) {
	if os.Getenv("AIRVIEW_E2E_SURVEY") != "1" {
		t.Skip("set AIRVIEW_E2E_SURVEY=1 to run the survey end-to-end test (3-10 minutes real time)")
	}
	maxDur := 10 * time.Minute
	if s := os.Getenv("SURVEY_E2E_MAX"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			maxDur = d
		}
	}

	aggDir := t.TempDir()
	avfPort, beastPort, mlatPort := freePort(t), freePort(t), freePort(t)
	agg := New(aggDir, Options{Offline: true, Logger: quietLogger(), ListenHost: "127.0.0.1", ConfigSets: []string{
		"mode=aggregator", "ntp.enabled=false", "aggregator.admission=open",
		"aggregator.beastPort=" + strconv.Itoa(beastPort), "aggregator.avfPort=" + strconv.Itoa(avfPort),
		"aggregator.mlatPort=" + strconv.Itoa(mlatPort), "aggregator.limits.perIpStations=64",
		"aggregator.mlat.engine.syncWarmupSec=20",
		"aggregator.mlat.quarantine.minAgeSec=30", "aggregator.mlat.quarantine.windowSec=10",
		"aggregator.mlat.quarantine.goodWindows=4",
		// Shorter survey phases so the test ends in minutes (the defaults
		// are 180 s / 60 s).
		"aggregator.mlat.survey.minDurationSec=60", "aggregator.mlat.survey.evalEverySec=20",
		"aggregator.mlat.autoUseSurvey=true", "aggregator.mlat.autoUseMaxSigmaM=50",
	}})
	if err := agg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer agg.Stop()
	makeAdmin(t, agg.Accounts())
	waitFor(t, "admin ready", 10*time.Second, agg.AdminReady)
	pin := agg.NetworkStatus().AVFPin
	if agg.Survey() == nil {
		t.Fatal("the survey is not wired in aggregator mode")
	}

	const (
		cLat, cLon = 50.0, 9.0
		nRef       = 6
	)
	und, _ := geoid.Undulation(cLat, cLon)
	devs := make([]simnet.Device, nRef+1)
	for i := range devs {
		devs[i] = simnet.DeviceRadarcapeGPS
	}
	const rxTarget = nRef
	specs := simnet.RandomReceivers(33, len(devs), simnet.Placement{CenterLat: cLat, CenterLon: cLon, RadiusKm: 90})
	rr := rand.New(rand.NewPCG(33, 5))
	for i, d := range devs {
		specs[i].Clock = simnet.DefaultClockSpec(d, rr)
		specs[i].Reception = simnet.DefaultReception(d, rr)
	}
	n, err := simnet.New(simnet.Config{Seed: 33, Start: time.Now().UTC(), Aircraft: 220, Receivers: specs,
		ModeSOnlyFraction: 0.2, CenterLat: cLat, CenterLon: cLon, GeoidUndulationM: und})
	if err != nil {
		t.Fatal(err)
	}
	truth := n.Receiver(rxTarget)

	addr := "127.0.0.1:" + strconv.Itoa(avfPort)
	for i := 0; i < nRef; i++ {
		f, err := simfeed.NewAVFFeeder(n, i, simfeed.AVFOptions{Addr: addr, Pin: pin})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
	}
	tf, err := simfeed.NewAVFFeeder(n, rxTarget, simfeed.AVFOptions{Addr: addr, Pin: pin, Unlocated: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tf.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = simfeed.RealTime(ctx, n, 100*time.Millisecond, nil)
	}()

	reg := agg.Registry()
	target := func() (registry.Station, bool) {
		// The only station that declares no position.
		for _, s := range reg.List(registry.Filter{Kind: model.StationKindAVF}) {
			if !s.Declared.Valid {
				return s, true
			}
		}
		return registry.Station{}, false
	}

	start := time.Now()
	var st registry.Station
	sawEstimating, sawRunning := false, false
	for time.Since(start) < maxDur {
		time.Sleep(3 * time.Second)
		s, ok := target()
		if !ok {
			continue
		}
		st = s
		if s.LocStatus == model.LocEstimating {
			sawEstimating = true
		}
		if stt, ok := agg.Survey().Status(s.Idx); ok && stt.Running {
			sawRunning = true
		}
		if s.Located && s.LocStatus == model.LocEstimated && agg.MLAT().PositionSource(s.Idx) == mlat.PosSurvey {
			break
		}
	}
	elapsed := time.Since(start).Round(time.Second)
	stt, _ := agg.Survey().Status(st.Idx)
	t.Logf("after %s: station %s located=%v status=%q source=%q, MLAT source %s, survey running=%v observed=%d stats %+v",
		elapsed, st.SID, st.Located, st.LocStatus, st.Effective.Source, agg.MLAT().PositionSource(st.Idx), stt.Running, stt.Observed, agg.Survey().Stats())
	if stt.Last != nil {
		l := stt.Last
		t.Logf("last survey: state %s valid %v stable %v suspect %v (%v) autoUse %v σh %.1f m, %d aircraft, %d observations",
			l.State, l.Valid, l.Stable, l.Suspect, l.SuspectWhy, l.AutoUse, l.Solution.SigmaHM, l.Solution.Aircraft, l.Observations)
	}
	if st.SID == "" {
		t.Fatal("the unlocated feeder never registered")
	}
	if !sawRunning && !sawEstimating && !st.Located {
		t.Error("no survey of the unlocated feeder was started automatically")
	}
	if !st.Located || st.LocStatus != model.LocEstimated || st.Effective.Source != model.LocSrcEstimated {
		t.Fatalf("the unlocated feeder was not located by the survey within %s", maxDur)
	}
	if src := agg.MLAT().PositionSource(st.Idx); src != mlat.PosSurvey {
		t.Errorf("MLAT position source %s, want survey (AutoUseSurvey)", src)
	}
	errH := horizM(truth.Lat, truth.Lon, st.Effective.Lat, st.Effective.Lon)
	t.Logf("surveyed position vs truth: %.1f m horizontal (σh %.1f m), height error %.1f m", errH, st.Effective.SigmaM, st.Effective.HAE-truth.HAE)
	if lim := 3*st.Effective.SigmaM + 30; errH > lim {
		t.Errorf("surveyed position %.1f m from the truth (limit 3σ+30 = %.0f m)", errH, lim)
	}
	if st.Effective.SigmaM > 50 {
		t.Errorf("auto-used survey σh %.1f m above autoUseMaxSigmaM 50", st.Effective.SigmaM)
	}
	// The owner notice: the audit row that the server turns into
	// "The network located this station".
	rows, err := reg.AuditLog(context.Background(), registry.AuditFilter{SID: st.SID, ActionPrefix: "station.", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range rows {
		found[r.Action] = true
	}
	t.Logf("audit actions: %v", found)
	if !found["station.location.survey"] {
		t.Error("no station.location.survey audit row (owner notice)")
	}
	if !found["station.survey"] {
		t.Error("no station.survey audit row (survey finished notice)")
	}
}
