package mlat_test

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"airview/internal/auth"
	"airview/internal/engine"
	"airview/internal/engine/simfeed"
	"airview/internal/mlat"
	"airview/internal/sim/simnet"
)

// TestLiveNetsim runs the live scenario of the run-away hold-out tracks
// (HOOKS.md, Wave 2B smoke run) in real time, over real sockets, into a
// real aggregator: `airview-netsim -seed 7 -avf-feeders 12 -beast-feeders 3
// -aircraft 250 -radius 150`. It reports the engine's own hold-out
// statistics (hold-out fixes and tracks against the ADS-B reference) once a
// minute. Opt-in, real time: MLAT_LIVE=1 [MLAT_LIVE_MIN=12]
// [MLAT_LIVE_PORT=28701: AVF, +1 Beast, +2 mlat-client].
func TestLiveNetsim(t *testing.T) {
	if os.Getenv("MLAT_LIVE") == "" {
		t.Skip("set MLAT_LIVE=1 (real-time run)")
	}
	mins := 12
	if v, err := strconv.Atoi(os.Getenv("MLAT_LIVE_MIN")); err == nil && v > 0 {
		mins = v
	}
	port := 28701
	if v, err := strconv.Atoi(os.Getenv("MLAT_LIVE_PORT")); err == nil && v > 0 {
		port = v
	}
	avfPort, beastPort, mlatPort := port, port+1, port+2
	agg := engine.New(t.TempDir(), engine.Options{Offline: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ListenHost: "127.0.0.1", ConfigSets: []string{
			"mode=aggregator", "ntp.enabled=false", "aggregator.admission=open",
			"aggregator.beastPort=" + strconv.Itoa(beastPort), "aggregator.avfPort=" + strconv.Itoa(avfPort),
			"aggregator.mlatPort=" + strconv.Itoa(mlatPort), "aggregator.limits.perIpStations=64",
			"aggregator.mlat.holdout=0.1",
		}})
	if err := agg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer agg.Stop()
	makeAdmin(t, agg.Accounts())
	pin := agg.NetworkStatus().AVFPin

	const seed, nBeast, nAVF = 7, 3, 12
	nrx := 1 + nBeast + nAVF
	specs := simnet.RandomReceivers(seed, nrx, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 150})
	rr := rand.New(rand.NewPCG(seed, 9))
	specs[0].Clock = simnet.DefaultClockSpec(simnet.DeviceRadarcapeGPS, rr)
	specs[0].Reception = simnet.DefaultReception(simnet.DeviceRadarcapeGPS, rr)
	for i, s := range specs {
		t.Logf("rx%d %s", i, s.Clock.Device)
	}
	n, err := simnet.New(simnet.Config{Seed: seed, Start: time.Now().UTC(), Aircraft: 250, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for i := 0; i < nBeast; i++ {
		b := simfeed.NewBeastFeeder(n, 1+i, "127.0.0.1:"+strconv.Itoa(beastPort))
		wg.Add(1)
		go func() { defer wg.Done(); b.Run(ctx) }()
	}
	for i := 0; i < nAVF; i++ {
		f, err := simfeed.NewAVFFeeder(n, 1+nBeast+i, simfeed.AVFOptions{Addr: "127.0.0.1:" + strconv.Itoa(avfPort), Pin: pin})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = simfeed.RealTime(ctx, n, 100*time.Millisecond, func() {})
	}()
	start := time.Now()
	for m := 1; m <= mins; m++ {
		time.Sleep(time.Until(start.Add(time.Duration(m) * time.Minute)))
		e := agg.MLAT()
		if e == nil {
			t.Fatal("no MLAT engine")
		}
		q := e.QualityReport(1, false)
		s := e.Stats()
		t.Logf("min %2d: hold-out FIX n %d P50 %.0f P95 %.0f cross P95 %.0f | TRACK n %d P50 %.0f P95 %.0f P99 %.0f cross P50 %.0f P95 %.0f | synced %d steps %d solves %d ok %d published %d restarts %d",
			m, q.Fix.N, q.Fix.P50, q.Fix.P95, q.Fix.CrossP95, q.Track.N, q.Track.P50, q.Track.P95, q.Track.P99,
			q.Track.CrossP50, q.Track.CrossP95, s.Clock.Stations, s.Clock.Steps, s.Solves, s.SolveOK, s.Published, s.Track.Respawns)
	}
	q := agg.MLAT().QualityReport(60, false)
	t.Logf("total: hold-out FIX n %d P50 %.0f P95 %.0f P99 %.0f cross P95 %.0f | TRACK n %d P50 %.0f P95 %.0f P99 %.0f cross P50 %.0f P95 %.0f 1σ %.2f",
		q.FixTotal.N, q.FixTotal.P50, q.FixTotal.P95, q.FixTotal.P99, q.FixTotal.CrossP95,
		q.TrackTotal.N, q.TrackTotal.P50, q.TrackTotal.P95, q.TrackTotal.P99, q.TrackTotal.CrossP50, q.TrackTotal.CrossP95, q.TrackTotal.Within1Sigma)
	fe, te := mlat.EstimateFromCross(q.FixTotal), mlat.EstimateFromCross(q.TrackTotal)
	t.Logf("cross-track estimate (latency-free): FIX P50 %.0f P95 %.0f | TRACK P50 %.0f P95 %.0f", fe.P50, fe.P95, te.P50, te.P95)
	t.Logf("stats: %+v", agg.MLAT().Stats())
}

// makeAdmin creates the scripted test admin of the throw-away data
// directory (the aggregator admits stations only once an admin exists).
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
