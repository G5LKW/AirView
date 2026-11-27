package engine

import (
	"slices"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

func TestStorageProfileResolution(t *testing.T) {
	dir := t.TempDir()
	cfg := model.DefaultConfig()

	// Fixed names are taken as they are.
	for _, name := range []string{model.StorageProfileStandard, model.StorageProfileLowWear, model.StorageProfileServer} {
		cfg.Storage.Profile = name
		p, over := StorageProfile(cfg, store.KindMain, dir, false)
		if p.Name != name || len(over) != 0 {
			t.Errorf("%s: got %q, overrides %v", name, p.Name, over)
		}
	}
	// auto: the aggregator's airview.db and stations.db run the server profile.
	cfg.Storage.Profile = ""
	if p, _ := StorageProfile(cfg, store.KindMain, dir, true); p.Name != store.ProfileServer || p.Requested != store.ProfileAuto {
		t.Errorf("aggregator airview.db: %q requested %q", p.Name, p.Requested)
	}
	if p, _ := StorageProfile(cfg, store.KindStations, dir, true); p.Name != store.ProfileServer {
		t.Errorf("stations.db: %q", p.Name)
	}
	// A station resolves by its platform; it is never "server".
	if p, _ := StorageProfile(cfg, store.KindMain, dir, false); p.Name == store.ProfileServer || p.Name == "" {
		t.Errorf("station auto: %q", p.Name)
	}
}

func TestStorageTuningOverrides(t *testing.T) {
	cfg := model.DefaultConfig()
	cfg.Storage.Profile = model.StorageProfileLowWear
	cfg.Storage.BackupBeforeMigrate = model.BackupAlways
	cfg.Storage.Tuning = model.StorageTuning{FlushSec: 45, IncFlushSec: 120, HeatFlushSec: 600, CheckpointSec: 900,
		WalSoftMB: 128, CacheMB: 3, ReadConns: 1, MmapMB: -1}
	base, _ := StorageProfile(model.Config{Storage: model.StorageConfig{Profile: model.StorageProfileLowWear}}, store.KindMain, t.TempDir(), false)
	p, over := StorageProfile(cfg, store.KindMain, t.TempDir(), false)
	if p.FlushInterval != 45*time.Second || p.IncFlush != 2*time.Minute || p.HeatFlush != 10*time.Minute ||
		p.CheckpointInterval != 15*time.Minute || p.CheckpointSoftBytes != 128<<20 || p.WriterCacheKiB != 3<<10 ||
		p.ReadConns != 1 || p.MmapBytes != 0 || p.BackupBeforeMigrate != model.BackupAlways {
		t.Fatalf("profile %+v", p.View())
	}
	// Values without an override keep the profile's.
	if p.TrailFlush != base.TrailFlush || p.ReaderCacheKiB != base.ReaderCacheKiB || p.SealInterval != base.SealInterval {
		t.Fatalf("untouched values changed: %+v vs %+v", p.View(), base.View())
	}
	// The hard cap stays above a raised soft size.
	if int64(p.WALHardCapPages)*4096 < 4*p.CheckpointSoftBytes || p.JournalSizeLimit < 2*p.CheckpointSoftBytes {
		t.Fatalf("hard cap %d pages / journal limit %d below soft %d", p.WALHardCapPages, p.JournalSizeLimit, p.CheckpointSoftBytes)
	}
	want := []string{"storage.backupBeforeMigrate", "storage.tuning.cacheMB", "storage.tuning.checkpointSec",
		"storage.tuning.flushSec", "storage.tuning.heatFlushSec", "storage.tuning.incFlushSec",
		"storage.tuning.mmapMB", "storage.tuning.readConns", "storage.tuning.walSoftMB"}
	if !slices.Equal(over, want) {
		t.Fatalf("overrides %v", over)
	}

	// mmapMB > 0 is used on 64-bit hosts only.
	cfg.Storage.Tuning = model.StorageTuning{MmapMB: 32}
	p, _ = StorageProfile(cfg, store.KindMain, t.TempDir(), false)
	if is64bit && p.MmapBytes != 32<<20 || !is64bit && p.MmapBytes != 0 {
		t.Fatalf("mmap %d (64-bit %v)", p.MmapBytes, is64bit)
	}
}
