package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const piMountinfo = `22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
23 1 0:22 / /sys rw,nosuid,nodev,noexec,relatime shared:2 - sysfs sysfs rw
25 1 179:2 / / rw,noatime shared:1 - ext4 /dev/mmcblk0p2 rw
26 25 179:1 / /boot/firmware rw,relatime shared:13 - vfat /dev/mmcblk0p1 rw,fmask=0022
40 25 8:1 / /mnt/usb\040disk rw,relatime shared:30 - ext4 /dev/sda1 rw
41 25 0:33 / /var/lib/btr rw,relatime shared:31 - btrfs /dev/mmcblk0p3 rw
`

func plat(goos, goarch string, intSize int, mem int64, mountinfo string) platform {
	return platform{goos: goos, goarch: goarch, intSize: intSize, memBytes: mem, mountinfo: mountinfo,
		evalPath: func(s string) string { return s }}
}

func TestResolveProfileBranches(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name   string
		req    string
		kind   Kind
		dir    string
		pf     platform
		want   string
		reason string
	}{
		{"windows desktop", "auto", KindMain, `C:\ProgramData\AirView`, plat("windows", "amd64", 64, 32*gib, ""), ProfileStandard, "default"},
		{"linux x86 server disk", "", KindMain, "/var/lib/airview", plat("linux", "amd64", 64, 16*gib, "25 1 8:2 / / rw - ext4 /dev/sda2 rw"), ProfileStandard, "default"},
		{"pi 4 on SD card", "auto", KindMain, "/var/lib/airview", plat("linux", "arm64", 64, 4*gib, piMountinfo), ProfileLowWear, "SD/MMC"},
		{"pi on USB SSD", "auto", KindMain, "/mnt/usb disk/airview", plat("linux", "arm64", 64, 4*gib, piMountinfo), ProfileStandard, "default"},
		{"pi zero 2 small RAM on USB", "auto", KindMain, "/mnt/usb disk/airview", plat("linux", "arm", 32, 512<<20, piMountinfo), ProfileLowWear, "MiB RAM"},
		{"btrfs on mmc reports major 0", "auto", KindMain, "/var/lib/btr/data", plat("linux", "arm64", 64, 8*gib, piMountinfo), ProfileLowWear, "mmcblk0p3"},
		{"x86 with 1 GiB is not arm", "auto", KindMain, "/data", plat("linux", "amd64", 64, gib, "25 1 8:2 / / rw - ext4 /dev/sda2 rw"), ProfileStandard, "default"},
		{"stations.db is server", "auto", KindStations, "/var/lib/airview", plat("linux", "amd64", 64, 16*gib, ""), ProfileServer, "stations.db"},
		{"explicit low-wear on windows", "low-wear", KindMain, `C:\x`, plat("windows", "amd64", 64, 32*gib, ""), ProfileLowWear, ""},
		{"explicit server", "Server", KindMain, "/x", plat("linux", "amd64", 64, 32*gib, ""), ProfileServer, ""},
		{"unknown name falls back to auto", "turbo", KindMain, "/x", plat("linux", "amd64", 64, 32*gib, ""), ProfileStandard, "unknown profile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := resolveProfile(c.req, c.kind, c.dir, c.pf)
			if p.Name != c.want {
				t.Fatalf("profile %q (reason %q), want %q", p.Name, p.Reason, c.want)
			}
			if c.reason != "" && !strings.Contains(p.Reason, c.reason) {
				t.Fatalf("reason %q does not mention %q", p.Reason, c.reason)
			}
			if c.reason == "" && p.Requested != p.Name {
				t.Fatalf("requested %q", p.Requested)
			}
		})
	}
}

func TestProfileValues(t *testing.T) {
	const gib = int64(1) << 30
	std := profileValues(ProfileStandard, plat("linux", "amd64", 64, 16*gib, ""))
	if std.FlushInterval != 10*time.Second || std.CheckpointSoftBytes != 40<<20 || std.CheckpointInterval != 5*time.Minute ||
		std.WALHardCapPages != 16384 || std.JournalSizeLimit != 64<<20 || std.WriterCacheKiB != 16<<10 ||
		std.ReaderCacheKiB != 8<<10 || std.ReadConns != 4 || std.MmapBytes != 256<<20 || std.ReaderIdleClose != 10*time.Minute {
		t.Fatalf("standard: %+v", std)
	}
	lw := profileValues(ProfileLowWear, plat("linux", "arm64", 64, gib, ""))
	if lw.FlushInterval != 30*time.Second || lw.CheckpointSoftBytes != 48<<20 || lw.CheckpointInterval != 10*time.Minute ||
		lw.WriterCacheKiB != 8<<10 || lw.ReaderCacheKiB != 4<<10 || lw.ReadConns != 2 || lw.MmapBytes != 64<<20 ||
		lw.ReaderIdleClose != 2*time.Minute || lw.HeatFlush != 900*time.Second || lw.IncFlush != 300*time.Second ||
		lw.MigrationStep != 100*time.Millisecond || lw.TileCacheMB != 128 {
		t.Fatalf("low-wear: %+v", lw)
	}
	// Pages caches of a Pi: <= 16 MiB on 64-bit, <= 10 MiB on 32-bit (§1).
	if kib := lw.WriterCacheKiB + lw.ReaderCacheKiB*lw.ReadConns; kib > 16<<10 {
		t.Fatalf("low-wear 64-bit caches %d KiB", kib)
	}
	lw32 := profileValues(ProfileLowWear, plat("linux", "arm", 32, gib, ""))
	if lw32.MmapBytes != 0 || lw32.WriterCacheKiB != 4<<10 {
		t.Fatalf("low-wear 32-bit: %+v", lw32)
	}
	if kib := lw32.WriterCacheKiB + lw32.ReaderCacheKiB*lw32.ReadConns; kib > 12<<10 {
		t.Fatalf("low-wear 32-bit caches %d KiB", kib)
	}
	if big := profileValues(ProfileLowWear, plat("linux", "arm64", 64, 4*gib, "")); big.ReadConns != 3 {
		t.Fatalf("low-wear with 4 GiB: %d read conns", big.ReadConns)
	}
	srv := profileValues(ProfileServer, plat("linux", "amd64", 64, 32*gib, ""))
	if srv.FlushInterval != 5*time.Second || srv.WALHardCapPages != 131072 || srv.JournalSizeLimit != 128<<20 ||
		srv.CheckpointSoftBytes != 64<<20 || srv.CheckpointInterval != time.Minute || srv.MmapBytes != 1<<30 ||
		srv.ReadConns != 8 || srv.ReaderIdleClose != 0 || srv.TrailFlush != 0 {
		t.Fatalf("server: %+v", srv)
	}
	if win := profileValues(ProfileStandard, plat("windows", "amd64", 64, 16*gib, "")); win.MmapBytes != 0 {
		t.Fatalf("windows mmap %d (blocks the file shrink)", win.MmapBytes)
	}
	if std32 := profileValues(ProfileStandard, plat("linux", "386", 32, gib, "")); std32.MmapBytes != 0 {
		t.Fatalf("standard 32-bit mmap %d", std32.MmapBytes)
	}
}

func TestProfileFillAndJSON(t *testing.T) {
	pf := plat("linux", "arm64", 64, 1<<30, "")
	p := Profile{Name: ProfileLowWear, FlushInterval: 45 * time.Second}.fill(pf)
	if p.FlushInterval != 45*time.Second || p.CheckpointSoftBytes != 48<<20 || p.BackupBeforeMigrate != "auto" {
		t.Fatalf("fill: %+v", p)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v["name"] != "low-wear" || v["flushIntervalMs"].(float64) != 45000 {
		t.Fatalf("json %s", b)
	}
}

func TestOpenUsesProfile(t *testing.T) {
	p := ResolveProfile(ProfileLowWear, KindMain, t.TempDir())
	s := openTest(t, Options{Profile: p})
	if s.opts.FlushInterval != 30*time.Second || s.Profile().Name != ProfileLowWear {
		t.Fatalf("flush %v profile %q", s.opts.FlushInterval, s.Profile().Name)
	}
	if got := scalar[int64](t, s, `PRAGMA cache_size`); got != -int64(p.ReaderCacheKiB) {
		t.Fatalf("reader cache_size %d", got)
	}
	// An explicit FlushInterval wins over the profile.
	s2 := openTest(t, Options{Profile: p, FlushInterval: 3 * time.Second})
	if s2.opts.FlushInterval != 3*time.Second || s2.Profile().FlushInterval != 3*time.Second {
		t.Fatalf("explicit flush interval lost: %v", s2.opts.FlushInterval)
	}
}

func TestMountDeviceLongestPrefix(t *testing.T) {
	d, ok := mountDevice(piMountinfo, "/boot/firmware/config")
	if !ok || d.point != "/boot/firmware" || d.major != 179 || d.minor != 1 {
		t.Fatalf("%+v %v", d, ok)
	}
	d, ok = mountDevice(piMountinfo, "/bootx")
	if !ok || d.point != "/" {
		t.Fatalf("%+v %v", d, ok)
	}
	if _, ok := mountDevice("", "/x"); ok {
		t.Fatal("empty mountinfo matched")
	}
}

// readStmt prepares a hot read once per database generation.
func TestReadStmtCache(t *testing.T) {
	s := openTest(t, manual)
	if err := s.KVSet(bg, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	q := `SELECT value FROM kv WHERE key = ?`
	a, err := s.readStmt(bg, q)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.readStmt(bg, q)
	if a != b {
		t.Fatal("statement prepared twice")
	}
	var v string
	if err := a.QueryRowContext(bg, "k").Scan(&v); err != nil || v != "v1" {
		t.Fatalf("%q %v", v, err)
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	c, err := s.readStmt(bg, q)
	if err != nil || c == a {
		t.Fatalf("not re-prepared after Reset: %v", err)
	}
	if err := c.QueryRowContext(bg, "k").Scan(&v); err == nil {
		t.Fatal("old row after Reset")
	}
}

// The early-flush threshold scales with the group-commit interval (1.0.0:
// 4,000 rows per 2 s), so a busy low-wear station is not flushed early.
func TestFlushRowsFollowsInterval(t *testing.T) {
	for _, c := range []struct {
		o    Options
		want int
	}{
		{Options{Profile: ResolveProfile(ProfileStandard, KindMain, "")}, 20000},
		{Options{Profile: ResolveProfile(ProfileLowWear, KindMain, "")}, 60000},
		{Options{Profile: ResolveProfile(ProfileServer, KindMain, "")}, 10000},
		{Options{FlushInterval: 2 * time.Second}, 4000},
		{Options{FlushInterval: 10 * time.Millisecond}, 4000},
		{Options{FlushInterval: time.Hour}, maxDefaultFlushRows},
		{Options{FlushInterval: time.Hour, FlushRows: 50}, 50},
		{Options{Kind: KindStations, Profile: ResolveProfile(ProfileServer, KindStations, "")}, 20000},
	} {
		o := c.o.withDefaults()
		if o.FlushRows != c.want {
			t.Errorf("%+v: FlushRows %d, want %d", c.o.FlushInterval, o.FlushRows, c.want)
		}
		if o.MaxQueueRows < 3*o.FlushRows {
			t.Errorf("FlushRows %d: queue bound %d holds fewer than three flushes", o.FlushRows, o.MaxQueueRows)
		}
	}
}
