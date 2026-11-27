package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/config"
)

// Operator file, environment and -set overrides are applied at start, stay
// in force across ApplyConfig and FactoryReset, and never reach config.json
// (DESIGN §20.3; HOOKS WP-CF-configfiles -> internal/engine).
func TestConfigLayers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "airview.jsonc"), []byte("{\n  // operator file\n  \"ntp\": {\"intervalSec\": 128,},\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(dir, Options{
		Demo: true, Offline: true, DemoSeed: 42, Logger: quietLogger(),
		Environ:    []string{"AIRVIEW_HUB_PASSWORD=env-secret", "PATH=/bin"},
		ConfigSets: []string{"ui.units=imperial"},
	})
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()
	check := func(when string) {
		t.Helper()
		cfg := e.Config().Get()
		if cfg.NTP.IntervalSec != 128 || cfg.Hub.Password != "env-secret" || cfg.UI.Units != "imperial" {
			t.Fatalf("%s: ntp.intervalSec %d hub.password %q ui.units %q", when, cfg.NTP.IntervalSec, cfg.Hub.Password, cfg.UI.Units)
		}
		for _, p := range []string{"ntp.intervalSec", "hub.password", "ui.units"} {
			if !e.Layers().Locked(p) {
				t.Errorf("%s: %s not locked", when, p)
			}
		}
		b, err := os.ReadFile(filepath.Join(dir, config.FileName))
		if err == nil && strings.Contains(string(b), "env-secret") {
			t.Fatalf("%s: the environment secret reached config.json", when)
		}
	}
	check("start")

	cfg := e.Config().Get()
	cfg.NTP.IntervalSec = 256
	cfg.Hub.Password = ""
	cfg.Weather.Enabled = !cfg.Weather.Enabled
	if _, _, err := e.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	check("apply")
	if got := e.Config().Get().Weather.Enabled; got != cfg.Weather.Enabled {
		t.Errorf("unlocked change lost: weather.enabled %v", got)
	}
	if err := e.FactoryReset(); err != nil {
		t.Fatalf("FactoryReset: %v", err)
	}
	check("factory reset")
}
