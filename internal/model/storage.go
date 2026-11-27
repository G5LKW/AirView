package model

import "strings"

// Storage profiles (StorageConfig.Profile). The store resolves "auto"
// (store.ResolveProfile); the values of each profile are listed in
// docs/CONFIG.md and docs/perf/DB-EFFICIENCY-DESIGN.md Â§4.1.
const (
	StorageProfileAuto     = "auto"
	StorageProfileStandard = "standard"
	StorageProfileLowWear  = "low-wear"
	StorageProfileServer   = "server"
)

// Pre-migration backup policies (StorageConfig.BackupBeforeMigrate).
const (
	BackupAuto   = "auto"
	BackupAlways = "always"
	BackupNever  = "never"
)

// StorageTuning ("storage.tuning") overrides single values of the storage
// profile. Every value 0 keeps the profile's value; the defaults suit
// every installation, these exist for measurements and unusual hardware.
type StorageTuning struct {
	// FlushSec is the group-commit interval of the database writer in
	// seconds (1..600; 0 = profile: standard 10, low-wear 30, server 5).
	FlushSec int `json:"flushSec,omitempty"`
	// IncFlushSec is how often the coverage, signal and elevation
	// statistics of the current day are written (15..3600 s; 0 = profile:
	// standard 60, low-wear 300). Pages always show the unwritten part.
	IncFlushSec int `json:"incFlushSec,omitempty"`
	// HeatFlushSec is how often the antenna heat map increments are
	// written (15..3600 s; 0 = profile: standard 300, low-wear 900).
	HeatFlushSec int `json:"heatFlushSec,omitempty"`
	// AircraftFlushSec is how often the all-time aircraft rows (messages,
	// positions, last seen) of the aircraft in view are rewritten (60..3600
	// s; 0 = profile: standard 300, low-wear 900). A new aircraft is written
	// within 15 s, and the aircraft list, the aircraft details and the
	// export write what is pending first, so they always show rows at most
	// a minute old.
	AircraftFlushSec int `json:"aircraftFlushSec,omitempty"`
	// CheckpointSec is the longest time between two WAL checkpoints
	// (10..3600 s; 0 = profile: standard 300, low-wear 600, server 60).
	CheckpointSec int `json:"checkpointSec,omitempty"`
	// WalSoftMB is the WAL size in MiB that triggers a checkpoint before
	// checkpointSec has passed (1..1024; 0 = profile: standard 40,
	// low-wear 48, server 64).
	WalSoftMB int `json:"walSoftMB,omitempty"`
	// CacheMB is the page cache of the database writer in MiB (1..4096;
	// 0 = profile: standard 16, low-wear 8 (4 on 32-bit), server 64).
	CacheMB int `json:"cacheMB,omitempty"`
	// ReadConns is the number of database reader connections (1..64; 0 =
	// profile: standard 4, low-wear 2 (3 with 2 GiB RAM or more), server 8).
	ReadConns int `json:"readConns,omitempty"`
	// MmapMB is the memory-mapped I/O size of the database in MiB
	// (1..65536, -1 = off; 0 = profile: standard 256, low-wear 64, server
	// 1024; off on 32-bit systems, and by default on Windows, where a
	// mapping keeps the file from shrinking).
	MmapMB int `json:"mmapMB,omitempty"`
}

// IsZero reports whether no tuning value is set.
func (t StorageTuning) IsZero() bool { return t == StorageTuning{} }

// Normalize clamps the storage profile settings into their ranges and
// returns notes. Retention values are checked by the config validator.
func (s *StorageConfig) Normalize() []string {
	var n notes
	switch p := strings.ToLower(strings.TrimSpace(s.Profile)); p {
	case "", StorageProfileAuto:
		s.Profile = ""
	case StorageProfileStandard, StorageProfileLowWear, StorageProfileServer:
		s.Profile = p
	case "lowwear", "low_wear":
		s.Profile = StorageProfileLowWear
	default:
		n.addf("storage.profile %q is not auto, standard, low-wear or server; auto is used", s.Profile)
		s.Profile = ""
	}
	switch b := strings.ToLower(strings.TrimSpace(s.BackupBeforeMigrate)); b {
	case "", BackupAuto:
		s.BackupBeforeMigrate = ""
	case BackupAlways, BackupNever:
		s.BackupBeforeMigrate = b
	default:
		n.addf("storage.backupBeforeMigrate %q is not auto, always or never; auto is used", s.BackupBeforeMigrate)
		s.BackupBeforeMigrate = ""
	}
	t := &s.Tuning
	zeroOrRangeI(&n, "storage.tuning.flushSec", &t.FlushSec, 1, 600)
	zeroOrRangeI(&n, "storage.tuning.incFlushSec", &t.IncFlushSec, 15, 3600)
	zeroOrRangeI(&n, "storage.tuning.heatFlushSec", &t.HeatFlushSec, 15, 3600)
	zeroOrRangeI(&n, "storage.tuning.aircraftFlushSec", &t.AircraftFlushSec, 60, 3600)
	zeroOrRangeI(&n, "storage.tuning.checkpointSec", &t.CheckpointSec, 10, 3600)
	zeroOrRangeI(&n, "storage.tuning.walSoftMB", &t.WalSoftMB, 1, 1024)
	zeroOrRangeI(&n, "storage.tuning.cacheMB", &t.CacheMB, 1, 4096)
	zeroOrRangeI(&n, "storage.tuning.readConns", &t.ReadConns, 1, 64)
	if t.MmapMB != -1 {
		zeroOrRangeI(&n, "storage.tuning.mmapMB", &t.MmapMB, 1, 65536)
	}
	return n
}

// ProfileName returns the configured profile with "" spelled "auto".
func (s StorageConfig) ProfileName() string {
	if s.Profile == "" {
		return StorageProfileAuto
	}
	return s.Profile
}

// BackupPolicy returns the configured backup policy with "" spelled "auto".
func (s StorageConfig) BackupPolicy() string {
	if s.BackupBeforeMigrate == "" {
		return BackupAuto
	}
	return s.BackupBeforeMigrate
}
