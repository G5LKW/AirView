package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStorageNormalize(t *testing.T) {
	s := StorageConfig{Profile: " Low-Wear ", BackupBeforeMigrate: "ALWAYS",
		Tuning: StorageTuning{FlushSec: 30, IncFlushSec: 5, HeatFlushSec: 900, CheckpointSec: 99999,
			WalSoftMB: 32, CacheMB: -2, ReadConns: 3, MmapMB: -1}}
	notes := s.Normalize()
	if s.Profile != StorageProfileLowWear || s.BackupBeforeMigrate != BackupAlways {
		t.Fatalf("enums: %+v", s)
	}
	want := StorageTuning{FlushSec: 30, HeatFlushSec: 900, WalSoftMB: 32, ReadConns: 3, MmapMB: -1}
	if s.Tuning != want {
		t.Fatalf("tuning %+v, want %+v", s.Tuning, want)
	}
	joined := strings.Join(notes, "\n")
	for _, p := range []string{"storage.tuning.incFlushSec", "storage.tuning.checkpointSec", "storage.tuning.cacheMB"} {
		if !strings.Contains(joined, p) {
			t.Errorf("no note for %s in %q", p, joined)
		}
	}
	if len(notes) != 3 {
		t.Errorf("notes %q", notes)
	}

	s = StorageConfig{Profile: "turbo", BackupBeforeMigrate: "sometimes"}
	if n := s.Normalize(); len(n) != 2 || s.Profile != "" || s.BackupBeforeMigrate != "" {
		t.Fatalf("bad enums kept: %+v %q", s, n)
	}
	if s.ProfileName() != StorageProfileAuto || s.BackupPolicy() != BackupAuto {
		t.Fatalf("names %q %q", s.ProfileName(), s.BackupPolicy())
	}
	s = StorageConfig{Profile: "auto", BackupBeforeMigrate: "auto"}
	if n := s.Normalize(); len(n) != 0 || s.Profile != "" || s.BackupBeforeMigrate != "" {
		t.Fatalf("auto not folded: %+v %q", s, n)
	}
}

// TestStorageJSONUnchanged: a default storage block serialises exactly as in
// 1.0.0 (the new keys are omitted), so config.json files do not change.
func TestStorageJSONUnchanged(t *testing.T) {
	b, err := json.Marshal(DefaultConfig().Storage)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"trailIntervalSec":5,"trailRetentionDays":30,"flightRetentionDays":730,"minuteRetentionDays":90}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
	b, _ = json.Marshal(StorageConfig{Profile: "low-wear", Tuning: StorageTuning{CacheMB: 4}})
	if !strings.Contains(string(b), `"profile":"low-wear"`) || !strings.Contains(string(b), `"tuning":{"cacheMB":4}`) {
		t.Fatalf("got %s", b)
	}
}

func TestEffectiveJournalLevel(t *testing.T) {
	cases := []struct {
		p       ProcessConfig
		systemd bool
		want    string
	}{
		{ProcessConfig{}, false, LogInfo},
		{ProcessConfig{}, true, LogWarn},
		{ProcessConfig{LogLevel: LogDebug}, false, LogDebug},
		{ProcessConfig{LogLevel: LogDebug}, true, LogWarn},
		{ProcessConfig{JournalLevel: LogInfo}, true, LogInfo},
		{ProcessConfig{JournalLevel: LogError}, false, LogError},
	}
	for _, c := range cases {
		if got := c.p.EffectiveJournalLevel(c.systemd); got != c.want {
			t.Errorf("%+v systemd=%v: %q, want %q", c.p, c.systemd, got, c.want)
		}
	}
	p := ProcessConfig{JournalLevel: "LOUD"}
	if n := p.Normalize(); len(n) != 1 || p.JournalLevel != "" {
		t.Fatalf("bad journal level kept: %+v %q", p, n)
	}
}
