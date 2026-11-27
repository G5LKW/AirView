package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airview/internal/fatfs"
	"airview/internal/model"
)

// fakeELF writes a minimal static linux/arm64 ELF header (no sections).
func fakeELF(t *testing.T, dir, name string, arch uint16, pad int) string {
	t.Helper()
	h := make([]byte, 64+pad)
	copy(h, "\x7fELF")
	h[4], h[5], h[6] = 2, 1, 1                  // 64-bit, little endian, version
	binary.LittleEndian.PutUint16(h[16:], 2)    // ET_EXEC
	binary.LittleEndian.PutUint16(h[18:], arch) // e_machine
	binary.LittleEndian.PutUint32(h[20:], 1)
	binary.LittleEndian.PutUint16(h[52:], 64) // e_ehsize
	rand.New(rand.NewSource(int64(pad))).Read(h[64:])
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, h, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeBase writes a Raspberry Pi OS-like image: MBR, a FAT32 boot partition
// with the cloud-init seed, and a "root" partition full of random bytes.
func fakeBase(t *testing.T, dir string, cloudInit bool) string {
	t.Helper()
	const bootStart, bootSec, rootSec = 8192, 80 * 2048, 4096
	size := int64(bootStart+bootSec+rootSec) * 512
	p := filepath.Join(dir, "base.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	mbr := make([]byte, 512)
	binary.LittleEndian.PutUint32(mbr[440:], 0x27b94958)
	put := func(i int, typ byte, start, n uint32) {
		e := mbr[446+16*i:]
		e[4] = typ
		binary.LittleEndian.PutUint32(e[8:], start)
		binary.LittleEndian.PutUint32(e[12:], n)
	}
	put(0, 0x0C, bootStart, bootSec)
	put(1, 0x83, bootStart+bootSec, rootSec)
	mbr[510], mbr[511] = 0x55, 0xAA
	f.WriteAt(mbr, 0)
	root := make([]byte, rootSec*512)
	rand.New(rand.NewSource(9)).Read(root)
	f.WriteAt(root, int64(bootStart+bootSec)*512)
	if err := fatfs.Format(f, bootStart*512, bootSec*512, fatfs.FormatOptions{Label: "bootfs", SectorsPerCluster: 1}); err != nil {
		t.Fatal(err)
	}
	fs, err := fatfs.Open(f, bootStart*512, bootSec*512, fatfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fs.WriteFile("cmdline.txt", []byte("console=tty1 root=PARTUUID=27b94958-02 rootfstype=ext4 rootwait resize\n"))
	fs.WriteFile("config.txt", []byte("arm_64bit=1\n"))
	fs.MkdirAll("overlays")
	fs.WriteFile("overlays/README", bytes.Repeat([]byte("x"), 5000))
	if cloudInit {
		fs.WriteFile("user-data", []byte("#cloud-config\n"))
		fs.WriteFile("meta-data", []byte("instance_id: rpios-image\n"))
		fs.WriteFile("network-config", []byte("# none\n"))
	}
	return p
}

type bins struct{ airview, feeder, pisetup string }

func fakeBins(t *testing.T, dir string) bins {
	return bins{
		airview: fakeELF(t, dir, "airview", 183, 3<<20),
		feeder:  fakeELF(t, dir, "airview-feeder", 183, 1<<20),
		pisetup: fakeELF(t, dir, "airview-pisetup", 183, 512<<10),
	}
}

func repoRoot(t *testing.T) string {
	r, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func runTool(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return out.String() + errb.String(), code
}

func TestBuildBothImagesAndVerify(t *testing.T) {
	dir := t.TempDir()
	base := fakeBase(t, dir, true)
	b := fakeBins(t, dir)
	out := filepath.Join(dir, "out")
	stamp := "2026-11-14T10:00:00Z"
	log, code := runTool(t, "build", "-base", base, "-out", out, "-version", "1.2.3", "-airview", b.airview,
		"-feeder", b.feeder, "-pisetup", b.pisetup, "-repo", repoRoot(t), "-time", stamp, "-manifest-key", testKey)
	if code != 0 {
		t.Fatalf("build failed:\n%s", log)
	}
	for _, want := range []string{"VERIFIED: AirView-Station-Pi-1.2.3.img", "VERIFIED: AirView-Feeder-Pi-1.2.3.img",
		"MBR: identical to the base image", "Outside the boot partition (MBR, gap, root file system): identical",
		"/airview/airview-pisetup", "/airview/install-feeder.sh", "/airview/airview-linux-arm64", "cloud-init NoCloud"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	for _, f := range []string{"AirView-Station-Pi-1.2.3.img", "AirView-Station-Pi-1.2.3.img.zip", "AirView-Station-Pi-1.2.3.img.zip.sha256",
		"AirView-Feeder-Pi-1.2.3.img", "AirView-Feeder-Pi-1.2.3.img.zip", "AirView-Feeder-Pi-1.2.3.verify.txt", "SHA256SUMS"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	// The zip holds exactly the image.
	zr, err := zip.OpenReader(filepath.Join(out, "AirView-Feeder-Pi-1.2.3.img.zip"))
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := zr.File[0].Open()
	zb, _ := io.ReadAll(rc)
	rc.Close()
	zr.Close()
	ib, _ := os.ReadFile(filepath.Join(out, "AirView-Feeder-Pi-1.2.3.img"))
	if !bytes.Equal(zb, ib) {
		t.Fatal("zip content differs from the image")
	}
	// Read the feeder image back.
	v, err := openImage(filepath.Join(out, "AirView-Feeder-Pi-1.2.3.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	conf, _ := v.fs.ReadFile("airview/image.conf")
	for _, want := range []string{"AIRVIEW_IMAGE_KIND=feeder", "AIRVIEW_IMAGE_VERSION=1.2.3", "AIRVIEW_HOSTNAME=airview-feeder", "AIRVIEW_IMAGE_BUILT=" + stamp} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("image.conf lacks %q:\n%s", want, conf)
		}
	}
	sums, _ := v.fs.ReadFile("airview/SHA256SUMS")
	if strings.Contains(string(sums), "feeder-setup.txt") || !strings.Contains(string(sums), "  airview-pisetup\n") {
		t.Errorf("SHA256SUMS:\n%s", sums)
	}
	// DLV-05: licence texts ship in the payload and are listed in SHA256SUMS
	// (so firstboot copies them to /opt/airview-image for the installers).
	for _, f := range []string{"LICENSE.txt", "THIRD-PARTY-NOTICES.txt", "update.conf"} {
		if !strings.Contains(string(sums), "  "+f+"\n") {
			t.Errorf("%s not in the feeder image's SHA256SUMS:\n%s", f, sums)
		}
	}
	if n, _ := v.fs.ReadFile("airview/THIRD-PARTY-NOTICES.txt"); !bytes.Contains(n, []byte("The Go programming language")) {
		t.Errorf("feeder notices lack the Go licence:\n%.300s", n)
	}
	// DLV-03: the release key reaches airview-pisetup.
	if u, _ := v.fs.ReadFile("airview/update.conf"); !strings.Contains(string(u), "MANIFEST_KEY="+testKey+"\n") {
		t.Errorf("update.conf:\n%s", u)
	}
	readme, _ := v.fs.ReadFile("airview/README.txt")
	if !strings.Contains(string(readme), "http://airview-feeder.local:8099/") || strings.Contains(string(readme), "{{") {
		t.Errorf("README:\n%s", readme)
	}
	e, _ := v.fs.Stat("airview/firstboot.sh")
	if !e.ModTime.Equal(time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("mtime %v", e.ModTime)
	}
	fb, _ := v.fs.ReadFile("airview/firstboot.sh")
	if bytes.Contains(fb, []byte("\r")) {
		t.Error("CR in firstboot.sh")
	}
	// inspect, with and without the base.
	if log, code := runTool(t, "inspect", filepath.Join(out, "AirView-Station-Pi-1.2.3.img")); code != 0 || !strings.Contains(log, "27b94958-02") || !strings.Contains(log, "/airview/install.sh") {
		t.Fatalf("inspect:\n%s", log)
	}
	if log, code := runTool(t, "inspect", "-base", base, filepath.Join(out, "AirView-Station-Pi-1.2.3.img")); code != 0 || !strings.Contains(log, "VERIFIED") {
		t.Fatalf("inspect -base:\n%s", log)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	base := fakeBase(t, dir, true)
	b := fakeBins(t, dir)
	out := filepath.Join(dir, "out")
	if log, code := runTool(t, "build", "-base", base, "-out", out, "-kinds", "station", "-zip=false", "-airview", b.airview, "-repo", repoRoot(t)); code != 0 {
		t.Fatalf("build:\n%s", log)
	}
	img := filepath.Join(out, "AirView-Station-Pi-"+model.Version+".img")
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	f.WriteAt([]byte{0xAA}, st.Size()-100) // a byte in the root partition
	f.Close()
	log, code := runTool(t, "inspect", "-base", base, img)
	if code == 0 || !strings.Contains(log, "outside the boot partition differ") {
		t.Fatalf("tampering not detected:\n%s", log)
	}
}

func TestBuildRefusals(t *testing.T) {
	dir := t.TempDir()
	b := fakeBins(t, dir)
	repo := repoRoot(t)
	// Bookworm-like base without the cloud-init seed.
	noCI := fakeBase(t, filepath.Join(dir), false)
	if log, code := runTool(t, "build", "-base", noCI, "-out", filepath.Join(dir, "o1"), "-kinds", "station", "-airview", b.airview, "-repo", repo); code == 0 || !strings.Contains(log, "cloud-init") {
		t.Fatalf("base without cloud-init accepted:\n%s", log)
	}
	// Not an arm64 binary.
	amd := fakeELF(t, dir, "amd64", 62, 0)
	d2 := filepath.Join(dir, "d2")
	os.MkdirAll(d2, 0o755)
	base := fakeBase(t, d2, true)
	if log, code := runTool(t, "build", "-base", base, "-out", filepath.Join(dir, "o2"), "-kinds", "station", "-airview", amd, "-repo", repo); code == 0 || !strings.Contains(log, "not a linux/arm64") {
		t.Fatalf("amd64 binary accepted:\n%s", log)
	}
	// Payload too large for the boot partition.
	huge := fakeELF(t, dir, "huge", 183, 90<<20)
	if log, code := runTool(t, "build", "-base", base, "-out", filepath.Join(dir, "o3"), "-kinds", "station", "-airview", huge, "-repo", repo); code == 0 || !strings.Contains(log, "MiB free") {
		t.Fatalf("oversized payload accepted:\n%s", log)
	}
	// Somebody else's vendor-data.
	f, _ := os.OpenFile(base, os.O_RDWR, 0)
	m, _ := fatfs.ReadMBR(f, 0)
	bp, _ := m.FirstFAT()
	fs, _ := fatfs.Open(f, bp.Offset(), bp.Size(), fatfs.Options{})
	fs.WriteFile("vendor-data", []byte("#cloud-config\n"))
	f.Close()
	if log, code := runTool(t, "build", "-base", base, "-out", filepath.Join(dir, "o4"), "-kinds", "station", "-airview", b.airview, "-repo", repo); code == 0 || !strings.Contains(log, "vendor-data") {
		t.Fatalf("foreign vendor-data replaced:\n%s", log)
	}
	if _, code := runTool(t, "build", "-base", base); code == 0 {
		t.Fatal("missing -out accepted")
	}
	if _, code := runTool(t, "build", "-base", base, "-out", dir, "-kinds", "toaster"); code == 0 {
		t.Fatal("unknown kind accepted")
	}
}

func TestPayloadCommand(t *testing.T) {
	dir := t.TempDir()
	b := fakeBins(t, dir)
	out := filepath.Join(dir, "payload")
	if log, code := runTool(t, "payload", "-kind", "feeder", "-out", out, "-feeder", b.feeder, "-pisetup", b.pisetup, "-repo", repoRoot(t)); code != 0 {
		t.Fatalf("payload:\n%s", log)
	}
	for _, f := range []string{"vendor-data", "airview/firstboot.sh", "airview/SHA256SUMS", "airview/airview-pisetup", "airview/install-feeder.sh",
		"airview/LICENSE.txt", "airview/THIRD-PARTY-NOTICES.txt", "airview/update.conf"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	// The station payload carries the /opt/airview tools and VERSION of a
	// bundle install (WP-BUILD), all listed in SHA256SUMS.
	out = filepath.Join(dir, "station")
	if log, code := runTool(t, "payload", "-kind", "station", "-out", out, "-airview", b.airview, "-version", "9.8.7", "-repo", repoRoot(t)); code != 0 {
		t.Fatalf("station payload:\n%s", log)
	}
	sums, err := os.ReadFile(filepath.Join(out, "airview", "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"install.sh", "airview.service", "update.sh", "uninstall.sh", "VERSION", "LICENSE.txt", "THIRD-PARTY-NOTICES.txt"} {
		if _, err := os.Stat(filepath.Join(out, "airview", f)); err != nil {
			t.Errorf("station payload lacks %s", f)
		}
		if !bytes.Contains(sums, []byte(" "+f+"\n")) && !bytes.Contains(sums, []byte("*"+f+"\n")) {
			t.Errorf("%s not in SHA256SUMS:\n%s", f, sums)
		}
	}
	if v, _ := os.ReadFile(filepath.Join(out, "airview", "VERSION")); string(v) != "9.8.7\n" {
		t.Errorf("VERSION %q", v)
	}
}
