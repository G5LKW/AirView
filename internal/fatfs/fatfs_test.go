package fatfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// memDev is an in-memory disk.
type memDev struct{ b []byte }

func (m *memDev) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > int64(len(m.b)) {
		return 0, fmt.Errorf("read past end: %d+%d > %d", off, len(p), len(m.b))
	}
	return copy(p, m.b[off:]), nil
}

func (m *memDev) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > int64(len(m.b)) {
		return 0, fmt.Errorf("write past end: %d+%d > %d", off, len(p), len(m.b))
	}
	return copy(m.b[off:], p), nil
}

var fixed = time.Date(2026, 11, 14, 12, 34, 56, 0, time.UTC)

// newDisk builds an MBR disk with a FAT32 partition at 1 MiB and a fake Linux
// partition after it.
func newDisk(t *testing.T, fatMiB int, spc uint32) (*memDev, Partition) {
	t.Helper()
	size := int64(fatMiB+2) << 20
	d := &memDev{b: make([]byte, size)}
	p := Partition{Index: 1, Type: 0x0C, StartLBA: 2048, Sectors: uint32(fatMiB << 11)}
	mbr := d.b[:512]
	mbr[440], mbr[441], mbr[442], mbr[443] = 0x58, 0x49, 0xb9, 0x27
	put := func(i int, typ byte, start, n uint32) {
		e := mbr[446+16*i:]
		e[4] = typ
		for k := 0; k < 4; k++ {
			e[8+k] = byte(start >> (8 * k))
			e[12+k] = byte(n >> (8 * k))
		}
	}
	put(0, p.Type, p.StartLBA, p.Sectors)
	put(1, 0x83, p.StartLBA+p.Sectors, 2048)
	mbr[510], mbr[511] = 0x55, 0xAA
	if err := Format(d, p.Offset(), p.Size(), FormatOptions{Label: "bootfs", VolumeID: 0x1234ABCD, SectorsPerCluster: spc}); err != nil {
		t.Fatal(err)
	}
	return d, p
}

func open(t *testing.T, d *memDev, p Partition) *FS {
	t.Helper()
	f, err := Open(d, p.Offset(), p.Size(), Options{Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mustClean(t *testing.T, f *FS) {
	t.Helper()
	for _, p := range f.Check() {
		t.Errorf("check: %s", p)
	}
}

func TestMBR(t *testing.T) {
	d, p := newDisk(t, 40, 1)
	m, err := ReadMBR(d, int64(len(d.b)))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Partitions) != 2 || m.Partitions[0] != p || m.Partitions[1].Type != 0x83 {
		t.Fatalf("partitions %+v", m.Partitions)
	}
	if m.PartUUID(2) != "27b94958-02" {
		t.Fatalf("partuuid %s", m.PartUUID(2))
	}
	if fp, ok := m.FirstFAT(); !ok || fp.Index != 1 {
		t.Fatal("FirstFAT")
	}
	if _, err := ReadMBR(d, int64(len(d.b))-4096); err == nil {
		t.Fatal("partition past the end accepted")
	}
	b := append([]byte{}, d.b[:512]...)
	b[446+16+9] = 0x08 // partition 2 now starts at 2048+... inside partition 1
	b[446+16+8] = 0x00
	b[446+16+10] = 0x00
	if _, err := ReadMBR(&memDev{b: append(b, make([]byte, len(d.b)-512)...)}, int64(len(d.b))); err == nil {
		t.Fatal("overlap accepted")
	}
	if _, err := ReadMBR(&memDev{b: make([]byte, 1024)}, 1024); !errors.Is(err, ErrNoMBR) {
		t.Fatalf("no signature: %v", err)
	}
}

func TestFormatOpenEmpty(t *testing.T) {
	d, p := newDisk(t, 40, 1)
	f := open(t, d, p)
	if f.Label() != "bootfs" || f.VolumeID() != 0x1234ABCD || f.ClusterSize() != 512 {
		t.Fatalf("label %q id %x cs %d", f.Label(), f.VolumeID(), f.ClusterSize())
	}
	if es, err := f.ReadDir("/"); err != nil || len(es) != 0 {
		t.Fatalf("root: %v %v", es, err)
	}
	if f.FreeClusters() != f.TotalClusters()-1 {
		t.Fatalf("free %d of %d", f.FreeClusters(), f.TotalClusters())
	}
	mustClean(t, f)
	if _, err := Open(d, 0, p.Size(), Options{}); err == nil {
		t.Fatal("MBR sector accepted as FAT32")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	d, p := newDisk(t, 40, 1)
	f := open(t, d, p)
	cs := int(f.ClusterSize())
	rng := rand.New(rand.NewSource(1))
	files := map[string][]byte{}
	sizes := []int{0, 1, cs - 1, cs, cs + 1, 3*cs + 7, 1 << 20}
	names := []string{"EMPTY", "one.txt", "Mixed Case Name.Json", "cmdline.txt", "CONFIG.TXT",
		"a very long file name that needs several long name entries - 0123456789.bin", "\u00fcn\u00efc\u00f8d\u00e9-\u2708.txt"}
	if err := f.MkdirAll("airview/sub dir/deeper"); err != nil {
		t.Fatal(err)
	}
	for i, n := range names {
		for _, dir := range []string{"", "airview/", "airview/sub dir/deeper/"} {
			b := make([]byte, sizes[i])
			rng.Read(b)
			if err := f.WriteFile(dir+n, b); err != nil {
				t.Fatalf("%s: %v", dir+n, err)
			}
			files[dir+n] = b
		}
	}
	mustClean(t, f)
	f2 := open(t, d, p) // fresh view from disk
	for n, want := range files {
		got, err := f2.ReadFile(n)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %d bytes, %v", n, len(got), err)
		}
		if _, err := f2.Stat(strings.ToUpper(n)); err != nil {
			t.Fatalf("upper %s: %v", n, err)
		}
	}
	es, _ := f2.ReadDir("airview")
	var got []string
	for _, e := range es {
		got = append(got, e.Name)
	}
	if len(es) != len(names)+1 || !strings.Contains(strings.Join(got, "|"), "Mixed Case Name.Json") {
		t.Fatalf("names %q", got)
	}
	for _, e := range es {
		if e.Name == "cmdline.txt" && e.ShortName != "CMDLINE.TXT" {
			t.Fatalf("short name %q", e.ShortName)
		}
		if !e.IsDir && !e.ModTime.Equal(fixed) {
			t.Fatalf("mtime %v", e.ModTime)
		}
	}
	if f2.fsInfoFree != f2.FreeClusters() {
		t.Fatalf("FSInfo free %d, FAT %d", f2.fsInfoFree, f2.FreeClusters())
	}
	mustClean(t, f2)
}

func TestReplaceRemoveAndSpace(t *testing.T) {
	d, p := newDisk(t, 40, 2)
	f := open(t, d, p)
	free0 := f.FreeClusters()
	big := bytes.Repeat([]byte("x"), 100000)
	if err := f.WriteFile("file.bin", big); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("FILE.BIN", []byte("small")); err != nil { // replace, other case
		t.Fatal(err)
	}
	if b, _ := f.ReadFile("file.bin"); string(b) != "small" {
		t.Fatalf("replaced: %q", b)
	}
	if es, _ := f.ReadDir("/"); len(es) != 1 {
		t.Fatalf("replace duplicated the entry: %v", es)
	}
	if f.FreeClusters() != free0-1 {
		t.Fatalf("free %d, want %d", f.FreeClusters(), free0-1)
	}
	if err := f.WriteFile("file.bin", nil); err != nil {
		t.Fatal(err)
	}
	if f.FreeClusters() != free0 {
		t.Fatal("empty file holds clusters")
	}
	if err := f.MkdirAll("d/e"); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove("d"); err == nil {
		t.Fatal("non-empty directory removed")
	}
	if err := f.WriteFile("d/e/x", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := f.RemoveAll("d"); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove("file.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stat("file.bin"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat removed: %v", err)
	}
	if f.FreeClusters() != free0 {
		t.Fatalf("leak: free %d, want %d", f.FreeClusters(), free0)
	}
	mustClean(t, f)
	huge := make([]byte, int(f.FreeBytes())+1)
	if err := f.WriteFile("huge", huge); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("huge: %v", err)
	}
	if f.FreeClusters() != free0 {
		t.Fatal("failed write leaked clusters")
	}
	if err := f.WriteFile("nodir/x", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing parent: %v", err)
	}
	for _, bad := range []string{"a:b", "x?", "trail.", "con\x01"} {
		if err := f.WriteFile(bad, nil); err == nil {
			t.Errorf("invalid name %q accepted", bad)
		}
	}
	mustClean(t, f)
}

func TestDirectoryGrowthAndShortNameTails(t *testing.T) {
	d, p := newDisk(t, 40, 1) // 512-byte clusters: 16 entries each
	f := open(t, d, p)
	if err := f.Mkdir("many"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if err := f.WriteFile(fmt.Sprintf("many/long file name number %03d.data", i), []byte{byte(i)}); err != nil {
			t.Fatal(i, err)
		}
	}
	es, err := f.ReadDir("many")
	if err != nil || len(es) != 300 {
		t.Fatalf("%d entries, %v", len(es), err)
	}
	shorts := map[string]bool{}
	for i, e := range es {
		if e.Name != fmt.Sprintf("long file name number %03d.data", i) {
			t.Fatalf("entry %d: %q", i, e.Name)
		}
		if shorts[e.ShortName] {
			t.Fatalf("duplicate short name %s", e.ShortName)
		}
		shorts[e.ShortName] = true
	}
	if !shorts["LONGFI~1.DAT"] || !shorts["LONGF~10.DAT"] || !shorts["LONG~100.DAT"] {
		t.Fatalf("numeric tails missing: %v", shorts)
	}
	if err := f.Remove("many/long file name number 000.data"); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("many/x", nil); err != nil {
		t.Fatal(err)
	}
	mustClean(t, f)
}

func TestFragmentedAllocation(t *testing.T) {
	d, p := newDisk(t, 16, 1)
	f := open(t, d, p)
	free := int(f.FreeClusters())
	var names []string
	for i := 0; i < free/32-20; i++ {
		n := fmt.Sprintf("f%05d", i)
		if err := f.WriteFile(n, make([]byte, 8192)); err != nil {
			t.Fatal(i, err)
		}
		names = append(names, n)
	}
	for i := 0; i < len(names); i += 2 {
		if err := f.Remove(names[i]); err != nil {
			t.Fatal(err)
		}
	}
	want := make([]byte, int(f.FreeBytes())-64*1024)
	rand.New(rand.NewSource(3)).Read(want)
	if err := f.WriteFile("scattered", want); err != nil {
		t.Fatal(err)
	}
	got, err := open(t, d, p).ReadFile("scattered")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("scattered: %v", err)
	}
	mustClean(t, f)
}

func TestCheckFindsCorruption(t *testing.T) {
	d, p := newDisk(t, 40, 1)
	f := open(t, d, p)
	f.WriteFile("a", make([]byte, 2000))
	f.WriteFile("b", make([]byte, 2000))
	a, _ := f.Stat("a")
	b, _ := f.Stat("b")
	f.setEntry(a.Cluster, b.Cluster) // cross-link a into b
	f.Sync()
	found := false
	for _, p := range f.Check() {
		if p.Severe && strings.Contains(p.Text, "cross-linked") {
			found = true
		}
	}
	if !found {
		t.Fatal("cross-link not found")
	}
}

func TestNames(t *testing.T) {
	if _, fl, ok := exactShort("cmdline.txt"); !ok || fl != 0x18 {
		t.Fatal("cmdline.txt")
	}
	if _, _, ok := exactShort("Cmdline.txt"); ok {
		t.Fatal("mixed case is not a plain short name")
	}
	b, e, lossy := basisName(".hidden file.tar.gz")
	if b != "HIDDENFI" || e != "GZ" || !lossy {
		t.Fatalf("basis %q %q %v", b, e, lossy)
	}
	var sn shortName
	copy(sn[:], "FIRSTB~1SH ")
	if sn.String() != "FIRSTB~1.SH" {
		t.Fatal(sn.String())
	}
	if len(lfnEntries(strings.Repeat("a", 26), 0)) != 2 || len(lfnEntries(strings.Repeat("a", 27), 0)) != 3 {
		t.Fatal("lfn entry count")
	}
}

// TestSevenZipReadsOurImage cross-checks the writer with an independent FAT
// reader (7-Zip) when it is installed.
func TestSevenZipReadsOurImage(t *testing.T) {
	z := `C:\Program Files\7-Zip\7z.exe`
	if _, err := os.Stat(z); err != nil {
		if z, err = exec.LookPath("7z"); err != nil {
			t.Skip("7-Zip not installed")
		}
	}
	d, p := newDisk(t, 40, 1)
	f := open(t, d, p)
	f.MkdirAll("airview/Sub Dir")
	f.WriteFile("vendor-data", []byte("#!/bin/sh\n"))
	f.WriteFile("airview/Sub Dir/A Long Name.txt", []byte("hello"))
	for i := 0; i < 40; i++ {
		f.WriteFile(fmt.Sprintf("airview/Sub Dir/file number %d.txt", i), bytes.Repeat([]byte{'a'}, i*100))
	}
	img := filepath.Join(t.TempDir(), "fat.img")
	if err := os.WriteFile(img, d.b[p.Offset():p.Offset()+p.Size()], 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(z, "l", "-slt", img).CombinedOutput()
	if err != nil {
		t.Fatalf("7z: %v\n%s", err, out)
	}
	s := strings.ReplaceAll(string(out), "\\", "/")
	for _, want := range []string{"Path = vendor-data", "Path = airview/Sub Dir/A Long Name.txt", "Size = 5",
		"Path = airview/Sub Dir/file number 39.txt", "Size = 3900"} {
		if !strings.Contains(s, want) {
			t.Fatalf("7z listing lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Errors") || strings.Contains(s, "ERROR") || strings.Contains(s, "Warnings") {
		t.Fatalf("7z reports problems:\n%s", s)
	}
	// Extract and compare one file.
	dir := t.TempDir()
	if out, err := exec.Command(z, "x", "-y", "-o"+dir, img, "airview/Sub Dir/file number 39.txt").CombinedOutput(); err != nil {
		t.Fatalf("7z x: %v %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "airview", "Sub Dir", "file number 39.txt"))
	if err != nil || !bytes.Equal(b, bytes.Repeat([]byte{'a'}, 3900)) {
		t.Fatalf("7z extracted %d bytes, %v", len(b), err)
	}
}

// TestRealBootPartition reads a Raspberry Pi OS image named by
// AIRVIEW_PIIMG_TEST (read-only).
func TestRealBootPartition(t *testing.T) {
	img := os.Getenv("AIRVIEW_PIIMG_TEST")
	if img == "" {
		t.Skip("set AIRVIEW_PIIMG_TEST=<raspios.img>")
	}
	fh, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	st, _ := fh.Stat()
	m, err := ReadMBR(fh, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	bp, ok := m.FirstFAT()
	if !ok {
		t.Fatal("no FAT partition")
	}
	f, err := Open(readOnly{fh}, bp.Offset(), bp.Size(), Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := f.ReadFile("cmdline.txt")
	if err != nil || !strings.Contains(string(cmd), "root=PARTUUID="+m.PartUUID(2)) {
		t.Fatalf("cmdline.txt: %q %v", cmd, err)
	}
	n := 0
	f.Walk("/", func(string, DirEntry) error { n++; return nil })
	t.Logf("label %q, %d entries, %d MiB free, cluster %d", f.Label(), n, f.FreeBytes()>>20, f.ClusterSize())
	for _, p := range f.Check() {
		t.Logf("check: %s", p)
		if p.Severe {
			t.Error(p)
		}
	}
}

type readOnly struct{ f *os.File }

func (r readOnly) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }
func (r readOnly) WriteAt([]byte, int64) (int, error)      { return 0, ErrReadOnly }
