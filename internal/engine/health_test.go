package engine

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"airview/internal/config"
	"airview/internal/model"
)

// SO-2: a configured feeder port that cannot be bound fails the health
// check.
func TestHealthChecksListenerFailed(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, ps, _ := net.SplitHostPort(busy.Addr().String())
	port, _ := strconv.Atoi(ps)
	dir := t.TempDir()
	cfg := model.DefaultConfig()
	cfg.Mode = model.ModeAggregator
	cfg.NTP.Enabled = false
	cfg.Aggregator.AVFPort, cfg.Aggregator.MLATPort, cfg.Aggregator.SBSInLocalPort = 0, 0, 0
	cfg.Aggregator.BeastPort = port
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	e := New(dir, Options{Offline: true, Logger: quietLogger(), ListenHost: "127.0.0.1"})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	hc := e.HealthChecks()
	if c := hc["listener.beast"]; !c.Fail || !strings.HasPrefix(c.State, "failed") {
		t.Fatalf("busy beast port: %+v (all %+v)", c, hc)
	}
	if c := hc["listener.avf"]; c.Fail || c.State != "off" {
		t.Fatalf("avf off: %+v", c)
	}
	if c := hc["store.main"]; c.Fail {
		t.Fatalf("store: %+v", c)
	}
}

// SO-6: in aggregator mode the trails follow aggregator.storage.trailDays.
func TestPruneStorageAggregatorTrails(t *testing.T) {
	cfg := model.DefaultConfig()
	if got := pruneStorage(cfg).TrailRetentionDays; got != cfg.Storage.TrailRetentionDays {
		t.Fatalf("standalone: %d", got)
	}
	cfg.Mode = model.ModeAggregator
	if got := pruneStorage(cfg).TrailRetentionDays; got != 7 {
		t.Fatalf("aggregator default: %d", got)
	}
	cfg.Aggregator.Storage.TrailDays = 2
	if got := pruneStorage(cfg).TrailRetentionDays; got != 2 {
		t.Fatalf("aggregator: %d", got)
	}
}
