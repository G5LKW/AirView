package engine

import (
	"sort"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

// Storage profiles (docs/perf/DB-EFFICIENCY-DESIGN.md §4): the engine
// resolves storage.profile for each database it opens, applies the
// operator's storage.tuning overrides and passes the result to the store
// (Options.Profile) and its cadences to the tracker.

// StorageProfile resolves the storage profile of a database of kind in
// dataDir for cfg: storage.profile ("auto" in aggregator mode is "server"
// for both databases), then the storage.tuning overrides and
// storage.backupBeforeMigrate. aggregator is the engine's network mode
// (a demo run never is). It returns the profile and the overridden
// setting names (sorted).
func StorageProfile(cfg model.Config, kind store.Kind, dataDir string, aggregator bool) (store.Profile, []string) {
	name := cfg.Storage.ProfileName()
	var p store.Profile
	if name == model.StorageProfileAuto && aggregator && kind != store.KindStations {
		p = store.ResolveProfile(store.ProfileServer, kind, dataDir)
		p.Requested, p.Reason = store.ProfileAuto, "aggregator mode"
	} else {
		p = store.ResolveProfile(name, kind, dataDir)
	}
	over := applyStorageTuning(&p, cfg.Storage)
	return p, over
}

// applyStorageTuning writes the non-zero storage.tuning values and a fixed
// backup policy over p and returns the names of the settings it changed.
func applyStorageTuning(p *store.Profile, s model.StorageConfig) []string {
	t := s.Tuning
	var over []string
	sec := func(name string, v int, d *time.Duration) {
		if v > 0 {
			*d = time.Duration(v) * time.Second
			over = append(over, name)
		}
	}
	sec("storage.tuning.flushSec", t.FlushSec, &p.FlushInterval)
	sec("storage.tuning.incFlushSec", t.IncFlushSec, &p.IncFlush)
	sec("storage.tuning.heatFlushSec", t.HeatFlushSec, &p.HeatFlush)
	sec("storage.tuning.aircraftFlushSec", t.AircraftFlushSec, &p.AircraftRefresh)
	sec("storage.tuning.checkpointSec", t.CheckpointSec, &p.CheckpointInterval)
	if t.WalSoftMB > 0 {
		p.CheckpointSoftBytes = int64(t.WalSoftMB) << 20
		// The hard cap (wal_autocheckpoint) stays a safety net above the
		// soft size: at least four times it.
		if minPages := int(p.CheckpointSoftBytes*4/4096) + 1; p.WALHardCapPages > 0 && p.WALHardCapPages < minPages {
			p.WALHardCapPages = minPages
		}
		if lim := p.CheckpointSoftBytes * 2; p.JournalSizeLimit > 0 && p.JournalSizeLimit < lim {
			p.JournalSizeLimit = lim
		}
		over = append(over, "storage.tuning.walSoftMB")
	}
	if t.CacheMB > 0 {
		p.WriterCacheKiB = t.CacheMB << 10
		over = append(over, "storage.tuning.cacheMB")
	}
	if t.ReadConns > 0 {
		p.ReadConns = t.ReadConns
		over = append(over, "storage.tuning.readConns")
	}
	switch {
	case t.MmapMB == -1:
		p.MmapBytes = 0
		over = append(over, "storage.tuning.mmapMB")
	case t.MmapMB > 0:
		if is64bit {
			p.MmapBytes = int64(t.MmapMB) << 20
		}
		over = append(over, "storage.tuning.mmapMB")
	}
	if s.BackupBeforeMigrate != "" {
		p.BackupBeforeMigrate = s.BackupBeforeMigrate
		over = append(over, "storage.backupBeforeMigrate")
	}
	sort.Strings(over)
	return over
}

// is64bit: memory-mapped I/O is never used in a 32-bit address space.
const is64bit = ^uint(0)>>63 == 1

// mainStoreOptions returns the store options of airview.db for cfg.
func (e *Engine) mainStoreOptions(cfg model.Config) store.Options {
	p, over := StorageProfile(cfg, store.KindMain, e.dir, e.wantNetwork(cfg))
	if len(over) > 0 {
		e.log.Info("storage profile overrides", "db", "airview.db", "settings", over)
	}
	return store.Options{Logger: e.log, Profile: p}
}

// StorageProfile returns the profile airview.db runs with (zero before
// Start).
func (e *Engine) StorageProfile() store.Profile {
	if e.st == nil {
		return store.Profile{}
	}
	return e.st.Profile()
}

// TileCacheDefaultMB is the default cap of the map tile cache: the
// profile's value (128 MB on low-wear, 512 MB elsewhere).
func (e *Engine) TileCacheDefaultMB() int {
	if mb := e.StorageProfile().TileCacheMB; mb > 0 {
		return mb
	}
	return 512
}

// storageProfileChanged reports whether the profile settings differ from
// the ones the databases were opened with (they apply at the next start).
func storageProfileChanged(boot, cur model.StorageConfig) bool {
	return boot.Profile != cur.Profile || boot.BackupBeforeMigrate != cur.BackupBeforeMigrate || boot.Tuning != cur.Tuning
}
