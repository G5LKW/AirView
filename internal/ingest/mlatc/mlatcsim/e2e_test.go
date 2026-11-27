package mlatcsim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"airview/internal/auth"
	"airview/internal/engine"
	"airview/internal/engine/simfeed"
	"airview/internal/geoid"
	"airview/internal/registry"
	"airview/internal/sim/simnet"
)

// TestRealTimeDiagnostic (MLATCSIM_E2E=1) reproduces the WP-E2 comparison
// in real time through the real aggregator Engine, the real listeners and
// internal/engine/simfeed's feeders: an RTL-SDR world with one GPS
// station, three receivers either as mlat-clients or on AVF/1. It prints
// per-station clock and MLAT state so the cause of a difference can be
// seen (H-mlatc diagnostics; it asserts nothing).
func TestRealTimeDiagnostic(t *testing.T) {
	if os.Getenv("MLATCSIM_E2E") != "1" {
		t.Skip("set MLATCSIM_E2E=1 (real time, minutes)")
	}
	dur := envDur("MLATCSIM_E2E_DUR", 150*time.Second)
	for _, asAVF := range []bool{false, true} {
		t.Run("mlatcAsAVF="+strconv.FormatBool(asAVF), func(t *testing.T) { realTimeRun(t, asAVF, dur) })
	}
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func realTimeRun(t *testing.T, asAVF bool, dur time.Duration) {
	dir := t.TempDir()
	avfPort, mlatPort := freePort(t), freePort(t)
	agg := engine.New(dir, engine.Options{Offline: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ListenHost: "127.0.0.1", ConfigSets: []string{
			"mode=aggregator", "ntp.enabled=false", "aggregator.admission=open",
			"aggregator.avfPort=" + strconv.Itoa(avfPort), "aggregator.mlatPort=" + strconv.Itoa(mlatPort),
			"aggregator.beastPort=0", "aggregator.limits.perIpStations=64",
			"aggregator.mlat.engine.syncWarmupSec=20", "aggregator.mlat.holdout=0.1",
		}})
	if err := agg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer agg.Stop()
	svc := agg.Accounts()
	ctx := context.Background()
	setup, err := svc.InitAdmin(ctx, "root", "correct horse battery staple 42")
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := auth.ParseTOTPSecret(setup.Secret)
	if _, err := svc.ConfirmAdminTOTP(ctx, "root", sec.Code(time.Now())); err != nil {
		t.Fatal(err)
	}
	for !agg.AdminReady() {
		time.Sleep(50 * time.Millisecond)
	}
	pin := agg.NetworkStatus().AVFPin

	const cLat, cLon = 50.0, 9.0
	und, _ := geoid.Undulation(cLat, cLon)
	devs := []simnet.Device{simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR,
		simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceRadarcapeGPS}
	mlatcRx := map[int]bool{4: true, 5: true, 6: true}
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
	rctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for i := range devs {
		if mlatcRx[i] && !asAVF {
			c, err := simfeed.NewMLATClient(n, i, simfeed.MLATClientOptions{Addr: "127.0.0.1:" + strconv.Itoa(mlatPort)})
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func() { defer wg.Done(); c.Run(rctx) }()
			continue
		}
		f, err := simfeed.NewAVFFeeder(n, i, simfeed.AVFOptions{Addr: "127.0.0.1:" + strconv.Itoa(avfPort), Pin: pin})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
	}
	wg.Add(1)
	go func() { defer wg.Done(); _ = simfeed.RealTime(rctx, n, 100*time.Millisecond, func() {}) }()
	time.Sleep(dur)
	m := agg.MLAT()
	if m == nil {
		t.Fatal("no MLAT engine")
	}
	st := m.Stats()
	q := m.QualityReport(1, false)
	t.Logf("mlatcAsAVF=%v: clusters %d solves ok %d published %d | hold-out (last min) fix %+v track %+v",
		asAVF, st.Clusters, st.SolveOK, st.Published, q.Fix, q.Track)
	var lines []string
	for _, s := range agg.Registry().List(registry.Filter{}) {
		r, ok := m.Station(s.Idx)
		if !ok {
			continue
		}
		line := fmt.Sprintf("  %-6s %-10s pos=%-9s timed/min=%5d", s.Kind, s.Name, r.PositionSource, r.TimedPerMin)
		if c := r.Clock; c != nil {
			line += fmt.Sprintf(" synced=%v σθ=%.0f ns jitter=%.0f outl=%.1f%% steps=%d peers=%d anchor=%s",
				c.Synced, c.SigmaThetaNs, c.JitterNs, c.OutlierPct, c.Steps, c.SyncedPeers, c.AnchorState)
		}
		if mq := r.MLAT; mq != nil {
			line += fmt.Sprintf(" | mlat contrib/h=%d used=%.0f%% outlier=%.1f%% bias=%.0f ns", mq.ContribPerHour, mq.UsedPct, mq.OutlierPct, mq.ResidualBiasNs)
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	for _, l := range lines {
		t.Log(l)
	}
}
