package config

import (
	"os"
	"path/filepath"
	"testing"

	"airview/internal/model"
)

// A config.json written by an earlier release (two antennas, Radarcape feed
// on radarcape:10003) keeps its antennas and feed after the generic factory
// defaults; a factory reset then gives the new defaults.
func TestUpgradeKeepsOldStationAndResetGivesGenericDefaults(t *testing.T) {
	dir := t.TempDir()
	old := `{
  "version": 1,
  "receiver": {"name": "My Station", "model": "Radarcape EVO 2CH"},
  "antennas": [
    {"id": "vertical", "name": "Vertical", "type": "vertical", "channel": 0, "gainDbi": 5, "heightAglM": 8},
    {"id": "yagi", "name": "Yagi", "type": "yagi", "channel": 1, "gainDbi": 12, "heightAglM": 8, "hBeamwidthDeg": 40}
  ],
  "feeds": [
    {"id": "radarcape", "name": "Radarcape EVO (Beast 10003)", "enabled": true, "type": "beast", "host": "radarcape", "port": 10003}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Antennas) != 2 || cfg.Antennas[1].ID != "yagi" {
		t.Fatalf("antennas changed on upgrade: %+v", cfg.Antennas)
	}
	if len(cfg.Feeds) != 1 || cfg.Feeds[0].Host != "radarcape" || cfg.Feeds[0].Port != 10003 || cfg.Feeds[0].ID != "radarcape" {
		t.Fatalf("feeds changed on upgrade: %+v", cfg.Feeds)
	}
	if cfg.Receiver.Model != "Radarcape EVO 2CH" {
		t.Fatalf("receiver model changed: %q", cfg.Receiver.Model)
	}
	// Loading again (the file was re-saved in canonical form) is stable.
	again, err := Load(dir)
	if err != nil || !Equal(again, cfg) {
		t.Fatalf("second load differs: %v", err)
	}

	reset, err := FactoryReset(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reset.Antennas) != 1 || reset.Feeds[0].Host != model.DefaultFeedHost || reset.Feeds[0].Port != model.DefaultFeedPort {
		t.Fatalf("factory reset: %+v %+v", reset.Antennas, reset.Feeds)
	}
	loaded, err := Load(dir)
	if err != nil || !Equal(loaded, model.DefaultConfig()) {
		t.Fatalf("after reset: %v", err)
	}
}
