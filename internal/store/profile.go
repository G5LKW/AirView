package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Profile names (storage.profile). "auto" is resolved by ResolveProfile.
const (
	ProfileAuto     = "auto"
	ProfileStandard = "standard"
	ProfileLowWear  = "low-wear"
	ProfileServer   = "server"
)

// Profile holds the platform-dependent storage settings of one database
// (docs/perf/DB-EFFICIENCY-DESIGN.md §4.1). It is resolved once at start by
// ResolveProfile and passed in Options.Profile; the engine may override
// single values (storage.tuning.*) before opening the store. A zero field
// means "the store's built-in default for that value".
//
// The tracker cadences (TrailFlush ... RangePartial) are not used by the
// store itself: the engine copies them into tracker.Options.
type Profile struct {
	// Name is "standard", "low-wear" or "server".
	Name string `json:"name"`
	// Requested is the configured value ("auto" or a fixed name).
	Requested string `json:"requested"`
	// Reason says why auto picked Name (empty for a fixed name).
	Reason string `json:"reason,omitempty"`

	// FlushInterval is the group-commit interval of queued writes.
	FlushInterval time.Duration `json:"flushIntervalMs"`

	// Tracker persistence cadences (0 = the tracker's own default).
	TrailFlush   time.Duration `json:"trailFlushMs"`
	IncFlush     time.Duration `json:"incFlushMs"`
	HeatFlush    time.Duration `json:"heatFlushMs"`
	RangePartial time.Duration `json:"rangePartialMs"`
	// AircraftRefresh: refresh of the active airframes' aircraft rows
	// (readers force a fresher one; tracker.Options.AircraftRefresh).
	AircraftRefresh time.Duration `json:"aircraftRefreshMs"`
	// SealInterval: how often the trail tail is sealed into chunks.
	SealInterval time.Duration `json:"sealIntervalMs"`

	// Checkpointer: a PASSIVE checkpoint runs when the un-checkpointed WAL
	// exceeds CheckpointSoftBytes or CheckpointInterval has passed.
	CheckpointSoftBytes int64         `json:"checkpointSoftBytes"`
	CheckpointInterval  time.Duration `json:"checkpointIntervalMs"`
	// WALHardCapPages is wal_autocheckpoint of the writer: a safety net
	// that normally never fires.
	WALHardCapPages int `json:"walHardCapPages"`
	// JournalSizeLimit truncates the WAL file after a reset.
	JournalSizeLimit int64 `json:"journalSizeLimit"`

	// Page caches (KiB) and the read pool.
	WriterCacheKiB  int           `json:"writerCacheKiB"`
	ReaderCacheKiB  int           `json:"readerCacheKiB"`
	ReadConns       int           `json:"readConns"`
	ReaderIdleClose time.Duration `json:"readerIdleCloseMs"` // 0 = never
	MmapBytes       int64         `json:"mmapBytes"`

	// MigrationStep is the target duration of one data-migration step
	// (the same pause follows each step).
	MigrationStep time.Duration `json:"migrationStepMs"`
	// BackupBeforeMigrate: "auto", "always" or "never" (§5.3).
	BackupBeforeMigrate string `json:"backupBeforeMigrate"`
	// TileCacheMB is the default tile-cache cap (used by the engine).
	TileCacheMB int `json:"tileCacheMB"`
}

// MarshalJSON writes the profile as its ProfileView (durations in ms).
func (p Profile) MarshalJSON() ([]byte, error) { return json.Marshal(p.View()) }

// ProfileView is Profile with durations in milliseconds, for JSON replies.
type ProfileView struct {
	Name                string `json:"name"`
	Requested           string `json:"requested"`
	Reason              string `json:"reason,omitempty"`
	FlushIntervalMs     int64  `json:"flushIntervalMs"`
	TrailFlushMs        int64  `json:"trailFlushMs"`
	IncFlushMs          int64  `json:"incFlushMs"`
	HeatFlushMs         int64  `json:"heatFlushMs"`
	RangePartialMs      int64  `json:"rangePartialMs"`
	AircraftRefreshMs   int64  `json:"aircraftRefreshMs"`
	SealIntervalMs      int64  `json:"sealIntervalMs"`
	CheckpointSoftBytes int64  `json:"checkpointSoftBytes"`
	CheckpointMs        int64  `json:"checkpointIntervalMs"`
	WALHardCapPages     int    `json:"walHardCapPages"`
	JournalSizeLimit    int64  `json:"journalSizeLimit"`
	WriterCacheKiB      int    `json:"writerCacheKiB"`
	ReaderCacheKiB      int    `json:"readerCacheKiB"`
	ReadConns           int    `json:"readConns"`
	ReaderIdleCloseMs   int64  `json:"readerIdleCloseMs"`
	MmapBytes           int64  `json:"mmapBytes"`
	MigrationStepMs     int64  `json:"migrationStepMs"`
	BackupBeforeMigrate string `json:"backupBeforeMigrate"`
	TileCacheMB         int    `json:"tileCacheMB"`
}

// View returns p for a JSON reply.
func (p Profile) View() ProfileView {
	ms := func(d time.Duration) int64 { return d.Milliseconds() }
	return ProfileView{
		Name: p.Name, Requested: p.Requested, Reason: p.Reason,
		FlushIntervalMs: ms(p.FlushInterval), TrailFlushMs: ms(p.TrailFlush), IncFlushMs: ms(p.IncFlush),
		HeatFlushMs: ms(p.HeatFlush), RangePartialMs: ms(p.RangePartial), AircraftRefreshMs: ms(p.AircraftRefresh),
		SealIntervalMs:      ms(p.SealInterval),
		CheckpointSoftBytes: p.CheckpointSoftBytes, CheckpointMs: ms(p.CheckpointInterval),
		WALHardCapPages: p.WALHardCapPages, JournalSizeLimit: p.JournalSizeLimit,
		WriterCacheKiB: p.WriterCacheKiB, ReaderCacheKiB: p.ReaderCacheKiB, ReadConns: p.ReadConns,
		ReaderIdleCloseMs: ms(p.ReaderIdleClose), MmapBytes: p.MmapBytes, MigrationStepMs: ms(p.MigrationStep),
		BackupBeforeMigrate: p.BackupBeforeMigrate, TileCacheMB: p.TileCacheMB,
	}
}

// Profile returns the resolved profile the store was opened with.
func (s *Store) Profile() Profile { return s.opts.Profile }

// platform is what ResolveProfile looks at; tests replace it.
type platform struct {
	goos, goarch string
	intSize      int
	memBytes     int64               // physical RAM, 0 = unknown
	mountinfo    string              // /proc/self/mountinfo (Linux), "" = unknown
	evalPath     func(string) string // absolute, symlink-free data directory
}

func hostPlatform() platform {
	p := platform{goos: runtime.GOOS, goarch: runtime.GOARCH, intSize: strconv.IntSize}
	if p.goos == "linux" {
		if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
			p.mountinfo = string(b)
		}
		p.memBytes = linuxMemTotal()
	}
	p.evalPath = func(dir string) string {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		// The directory may not exist yet: resolve the deepest existing parent.
		probe := dir
		for {
			if r, err := filepath.EvalSymlinks(probe); err == nil {
				rest, _ := filepath.Rel(probe, dir)
				if rest == "." {
					return r
				}
				return filepath.Join(r, rest)
			}
			parent := filepath.Dir(probe)
			if parent == probe {
				return dir
			}
			probe = parent
		}
	}
	return p
}

func linuxMemTotal() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			fields := strings.Fields(v)
			if len(fields) > 0 {
				if kb, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					return kb << 10
				}
			}
		}
	}
	return 0
}

// mmcMajor is the Linux block-device major number of MMC / SD cards.
const mmcMajor = 179

// ResolveProfile resolves a storage.profile value for a database of kind
// in dataDir. name "auto" (or "") picks:
//
//   - "server" for stations.db (KindStations, aggregator mode); the engine
//     passes "server" explicitly for the aggregator's airview.db;
//   - "low-wear" when dataDir lives on an MMC / SD block device (Linux:
//     device major 179 in /proc/self/mountinfo) or the host is Linux on
//     arm / arm64 with at most 1 GiB of RAM;
//   - "standard" otherwise.
//
// Unknown names resolve like "auto". The returned profile carries every
// value of the design's profile table for the platform (32-bit hosts get no
// mmap and smaller caches).
func ResolveProfile(name string, kind Kind, dataDir string) Profile {
	return resolveProfile(name, kind, dataDir, hostPlatform())
}

func resolveProfile(name string, kind Kind, dataDir string, pf platform) Profile {
	req := strings.ToLower(strings.TrimSpace(name))
	if req == "" {
		req = ProfileAuto
	}
	chosen, reason := req, ""
	switch req {
	case ProfileStandard, ProfileLowWear, ProfileServer:
	default:
		chosen, reason = autoProfile(kind, dataDir, pf)
		if req != ProfileAuto {
			reason = "unknown profile " + strconv.Quote(req) + "; " + reason
		}
		req = ProfileAuto
	}
	p := profileValues(chosen, pf)
	p.Requested, p.Reason = req, reason
	return p
}

func autoProfile(kind Kind, dataDir string, pf platform) (name, reason string) {
	if kind == KindStations {
		return ProfileServer, "stations.db (aggregator)"
	}
	if pf.goos == "linux" {
		dir := dataDir
		if pf.evalPath != nil {
			dir = pf.evalPath(dataDir)
		}
		if dev, ok := mountDevice(pf.mountinfo, dir); ok && dev.major == mmcMajor {
			return ProfileLowWear, "data directory on an SD/MMC card (" + dev.source + ")"
		}
		if (pf.goarch == "arm" || pf.goarch == "arm64") && pf.memBytes > 0 && pf.memBytes <= 1<<30 {
			return ProfileLowWear, "ARM host with " + strconv.FormatInt(pf.memBytes>>20, 10) + " MiB RAM"
		}
	}
	return ProfileStandard, "default"
}

type mountDev struct {
	major, minor int
	point        string
	source       string
}

// mountDevice finds the mount of /proc/self/mountinfo whose mount point is
// the longest prefix of dir. A line is
//
//	36 35 179:2 / / rw,noatime shared:1 - ext4 /dev/mmcblk0p2 rw
//
// (ID, parent, major:minor, root, mount point, options, optional fields,
// "-", file system type, source, super options).
func mountDevice(mountinfo, dir string) (mountDev, bool) {
	dir = filepath.ToSlash(filepath.Clean(dir))
	var best mountDev
	found := false
	for _, line := range strings.Split(mountinfo, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		mp := unescapeMount(f[4])
		if !pathHasPrefix(dir, mp) {
			continue
		}
		if found && len(mp) < len(best.point) {
			continue
		}
		maj, min, ok := strings.Cut(f[2], ":")
		if !ok {
			continue
		}
		a, err1 := strconv.Atoi(maj)
		b, err2 := strconv.Atoi(min)
		if err1 != nil || err2 != nil {
			continue
		}
		d := mountDev{major: a, minor: b, point: mp}
		for i := 5; i < len(f); i++ {
			if f[i] == "-" {
				if i+2 < len(f) {
					d.source = f[i+2]
				}
				break
			}
		}
		// Some file systems (btrfs, overlay roots) report major 0; their
		// source still names the card.
		if d.major == 0 && strings.HasPrefix(d.source, "/dev/mmcblk") {
			d.major = mmcMajor
		}
		best, found = d, true
	}
	return best, found
}

func pathHasPrefix(dir, mp string) bool {
	if mp == "/" {
		return strings.HasPrefix(dir, "/")
	}
	return dir == mp || strings.HasPrefix(dir, mp+"/")
}

// unescapeMount decodes the octal escapes (\040 = space) of mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// profileValues returns the table values of §4.1 for name on pf, with two
// measured changes: the checkpointer's soft size is 40 MiB (standard) /
// 48 MiB (low-wear) instead of 16 / 32 MiB - on the 1.0.0 write volume of
// the rt-7d workload 16 MiB still gave 30 checkpoints and ~150 MB of
// database-file writes per hour, 40 MiB 16 / ~90 MB (exp1 level); at the
// 1.1 write volume the interval triggers first anyway - and the
// journal_size_limit equals the 64 MiB hard cap, so a WAL that reaches the
// soft size is not truncated and regrown after every reset.
func profileValues(name string, pf platform) Profile {
	is64 := pf.intSize >= 64
	mmap := func(v int64) int64 {
		if !is64 {
			return 0 // 32-bit address space: no file mappings
		}
		if pf.goos == "windows" {
			// Windows cannot shrink a file while another connection of the
			// process maps it (SetEndOfFile fails on a user-mapped file), so
			// a read connection's mapping would keep the incremental vacuum
			// from ever returning space. Reads go through the OS cache.
			return 0
		}
		return v
	}
	switch name {
	case ProfileLowWear:
		p := Profile{
			Name:          ProfileLowWear,
			FlushInterval: 30 * time.Second,
			TrailFlush:    60 * time.Second, IncFlush: 300 * time.Second, HeatFlush: 900 * time.Second,
			RangePartial: 30 * time.Minute, AircraftRefresh: 900 * time.Second, SealInterval: 10 * time.Minute,
			CheckpointSoftBytes: 48 << 20, CheckpointInterval: 10 * time.Minute,
			WALHardCapPages: 16384, JournalSizeLimit: 64 << 20,
			WriterCacheKiB: 8 << 10, ReaderCacheKiB: 4 << 10, ReadConns: 2,
			ReaderIdleClose: 2 * time.Minute, MmapBytes: mmap(64 << 20),
			MigrationStep: 100 * time.Millisecond, BackupBeforeMigrate: "auto", TileCacheMB: 128,
		}
		if !is64 {
			p.WriterCacheKiB = 4 << 10
		}
		if pf.memBytes >= 2<<30 {
			p.ReadConns = 3
		}
		return p
	case ProfileServer:
		return Profile{
			Name:          ProfileServer,
			FlushInterval: 5 * time.Second,
			// Tracker cadences: the aggregator lanes keep their own (0).
			SealInterval:        5 * time.Minute,
			CheckpointSoftBytes: 64 << 20, CheckpointInterval: 60 * time.Second,
			WALHardCapPages: 131072, JournalSizeLimit: 128 << 20,
			WriterCacheKiB: 64 << 10, ReaderCacheKiB: 16 << 10, ReadConns: 8,
			ReaderIdleClose: 0, MmapBytes: mmap(1 << 30),
			MigrationStep: 200 * time.Millisecond, BackupBeforeMigrate: "auto", TileCacheMB: 512,
		}
	default:
		return Profile{
			Name:          ProfileStandard,
			FlushInterval: 10 * time.Second,
			TrailFlush:    60 * time.Second, IncFlush: 60 * time.Second, HeatFlush: 300 * time.Second,
			RangePartial: 15 * time.Minute, AircraftRefresh: 300 * time.Second, SealInterval: 5 * time.Minute,
			CheckpointSoftBytes: 40 << 20, CheckpointInterval: 5 * time.Minute,
			WALHardCapPages: 16384, JournalSizeLimit: 64 << 20,
			WriterCacheKiB: 16 << 10, ReaderCacheKiB: 8 << 10, ReadConns: 4,
			ReaderIdleClose: 10 * time.Minute, MmapBytes: mmap(256 << 20),
			MigrationStep: 200 * time.Millisecond, BackupBeforeMigrate: "auto", TileCacheMB: 512,
		}
	}
}

// fill completes a profile that was built by hand (zero fields take the
// value of its named profile, or of "standard").
func (p Profile) fill(pf platform) Profile {
	name := p.Name
	switch name {
	case ProfileStandard, ProfileLowWear, ProfileServer:
	default:
		name = ProfileStandard
	}
	d := profileValues(name, pf)
	if p.Name == "" {
		p.Name = d.Name
	}
	if p.Requested == "" {
		p.Requested = p.Name
	}
	set := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&p.FlushInterval, d.FlushInterval)
	set(&p.SealInterval, d.SealInterval)
	set(&p.CheckpointInterval, d.CheckpointInterval)
	set(&p.MigrationStep, d.MigrationStep)
	if p.TrailFlush <= 0 {
		p.TrailFlush = d.TrailFlush
	}
	if p.IncFlush <= 0 {
		p.IncFlush = d.IncFlush
	}
	if p.HeatFlush <= 0 {
		p.HeatFlush = d.HeatFlush
	}
	if p.RangePartial <= 0 {
		p.RangePartial = d.RangePartial
	}
	if p.AircraftRefresh <= 0 {
		p.AircraftRefresh = d.AircraftRefresh
	}
	if p.CheckpointSoftBytes <= 0 {
		p.CheckpointSoftBytes = d.CheckpointSoftBytes
	}
	if p.WALHardCapPages <= 0 {
		p.WALHardCapPages = d.WALHardCapPages
	}
	if p.JournalSizeLimit <= 0 {
		p.JournalSizeLimit = d.JournalSizeLimit
	}
	if p.WriterCacheKiB <= 0 {
		p.WriterCacheKiB = d.WriterCacheKiB
	}
	if p.ReaderCacheKiB <= 0 {
		p.ReaderCacheKiB = d.ReaderCacheKiB
	}
	if p.ReadConns <= 0 {
		p.ReadConns = d.ReadConns
	}
	if p.ReaderIdleClose < 0 {
		p.ReaderIdleClose = 0
	}
	if p.MmapBytes < 0 || pf.intSize < 64 {
		p.MmapBytes = 0
	}
	switch p.BackupBeforeMigrate {
	case "auto", "always", "never":
	default:
		p.BackupBeforeMigrate = d.BackupBeforeMigrate
	}
	if p.TileCacheMB <= 0 {
		p.TileCacheMB = d.TileCacheMB
	}
	return p
}
