package config

import (
	"os"
	"path/filepath"
	"testing"

	"airview/internal/model"
)

// TestStorageProfileLayers: storage.profile, backupBeforeMigrate and the
// tuning overrides from the operator file, the environment and -set; bad
// values are reported at their setting and fall back to automatic.
func TestStorageProfileLayers(t *testing.T) {
	dir := t.TempDir()
	op := filepath.Join(dir, "op.jsonc")
	if err := os.WriteFile(op, []byte(`{
  // Pi on an SD card
  "storage": {"profile": "low-wear", "tuning": {"cacheMB": 4, "readConns": 2}},
  "process": {"journalLevel": "error"},
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Check(LoadOptions{Dir: dir, File: op,
		Environ: []string{"AIRVIEW_STORAGE_TUNING_FLUSH_SEC=45", "AIRVIEW_STORAGE_BACKUP_BEFORE_MIGRATE=never"},
		Sets:    []string{"storage.tuning.mmapMB=-1"}})
	if r.Errors() != 0 {
		t.Fatalf("errors:\n%s", issuesText(r.Issues))
	}
	s := r.Effective.Storage
	want := model.StorageTuning{FlushSec: 45, CacheMB: 4, ReadConns: 2, MmapMB: -1}
	if s.Profile != model.StorageProfileLowWear || s.BackupBeforeMigrate != model.BackupNever || s.Tuning != want {
		t.Fatalf("storage %+v", s)
	}
	if r.Effective.Process.JournalLevel != model.LogError {
		t.Fatalf("journalLevel %q", r.Effective.Process.JournalLevel)
	}

	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: []string{"storage.profile=fast", "storage.tuning.heatFlushSec=5"}})
	if !issueAt(r, "storage.profile") || !issueAt(r, "storage.tuning.heatFlushSec") {
		t.Fatalf("not reported:\n%s", issuesText(r.Issues))
	}
	if r.Effective.Storage.Profile != "" || r.Effective.Storage.Tuning.HeatFlushSec != 0 {
		t.Fatalf("bad values kept: %+v", r.Effective.Storage)
	}
}

// TestOldConfigCheckUnchanged: a 1.0.0 config.json (no profile keys) checks
// without any issue and keeps the automatic profile.
func TestOldConfigCheckUnchanged(t *testing.T) {
	dir := t.TempDir()
	old := `{"version":1,"mode":"standalone","storage":{"trailIntervalSec":5,"trailRetentionDays":30,"flightRetentionDays":730,"minuteRetentionDays":90},"process":{"logLevel":"info"}}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Check(LoadOptions{Dir: dir, NoFile: true})
	if len(r.Issues) != 0 {
		t.Fatalf("issues:\n%s", issuesText(r.Issues))
	}
	if s := r.Effective.Storage; s.Profile != "" || s.BackupBeforeMigrate != "" || !s.Tuning.IsZero() {
		t.Fatalf("storage %+v", s)
	}
}
