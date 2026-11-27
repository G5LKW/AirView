package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeELF returns a minimal static ELF header for the machine (enough for
// debug/elf: header only, no program headers).
func fakeELF(t *testing.T, machine uint16, class32 bool) []byte {
	t.Helper()
	var b bytes.Buffer
	ident := [16]byte{0x7f, 'E', 'L', 'F', 2, 1, 1}
	if class32 {
		ident[4] = 1
	}
	b.Write(ident[:])
	le := binary.LittleEndian
	w16 := func(v uint16) { binary.Write(&b, le, v) }
	w32 := func(v uint32) { binary.Write(&b, le, v) }
	w64 := func(v uint64) { binary.Write(&b, le, v) }
	w16(2) // ET_EXEC
	w16(machine)
	w32(1)
	if class32 {
		w32(0)
		w32(0)
		w32(0)
		w32(0)
		w16(52)
		w16(32)
		w16(0)
		w16(40)
		w16(0)
		w16(0)
	} else {
		w64(0)
		w64(0)
		w64(0)
		w32(0)
		w16(64)
		w16(56)
		w16(0)
		w16(64)
		w16(0)
		w16(0)
	}
	b.Write(make([]byte, 64))
	return b.Bytes()
}

func write(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

type tarEnt struct {
	name  string
	mode  int64
	typ   byte
	uid   int
	uname string
	body  []byte
}

func readTar(t *testing.T, p string) []tarEnt {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Errorf("gzip header carries name %q / time %v", gz.Name, gz.ModTime)
	}
	tr := tar.NewReader(gz)
	var out []tarEnt
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out = append(out, tarEnt{h.Name, h.Mode, h.Typeflag, h.Uid, h.Uname, b})
	}
}

// stagePi builds a staging tree like the Pi bundle.
func stagePi(t *testing.T, root, name string) string {
	stage := filepath.Join(root, name)
	write(t, filepath.Join(stage, "airview-linux-arm64"), fakeELF(t, 183, false))
	write(t, filepath.Join(stage, "airview-linux-armv7"), fakeELF(t, 40, true))
	write(t, filepath.Join(stage, "install.sh"), []byte("#!/bin/sh\necho hi\n"))
	write(t, filepath.Join(stage, "README.md"), []byte("# readme\n"))
	write(t, filepath.Join(stage, "VERSION"), []byte("1.2.3\n"))
	write(t, filepath.Join(stage, "proxy", "nginx.conf"), []byte("x\n"))
	write(t, filepath.Join(stage, "LICENSE.txt"), []byte("licence\n"))
	write(t, filepath.Join(stage, "THIRD-PARTY-NOTICES.txt"), []byte("notices\n"))
	return stage
}

func TestPackModesOwnersAndOrder(t *testing.T) {
	root := t.TempDir()
	stage := stagePi(t, root, "airview-pi-1.2.3")
	out := filepath.Join(root, "out", "airview-pi-1.2.3.tar.gz")
	when := time.Unix(1700000000, 0)
	n, err := Pack(stage, out, when)
	if err != nil {
		t.Fatal(err)
	}
	ents := readTar(t, out)
	if n != len(ents) || n != 10 {
		t.Fatalf("entries %d / %d, want 10", n, len(ents))
	}
	want := map[string]int64{
		"airview-pi-1.2.3/":                        0o755,
		"airview-pi-1.2.3/LICENSE.txt":             0o644,
		"airview-pi-1.2.3/THIRD-PARTY-NOTICES.txt": 0o644,
		"airview-pi-1.2.3/README.md":               0o644,
		"airview-pi-1.2.3/VERSION":                 0o644,
		"airview-pi-1.2.3/airview-linux-arm64":     0o755,
		"airview-pi-1.2.3/airview-linux-armv7":     0o755,
		"airview-pi-1.2.3/install.sh":              0o755,
		"airview-pi-1.2.3/proxy/":                  0o755,
		"airview-pi-1.2.3/proxy/nginx.conf":        0o644,
	}
	prev := ""
	for _, e := range ents {
		if e.name < prev {
			t.Errorf("entries not sorted: %s after %s", e.name, prev)
		}
		prev = e.name
		m, ok := want[e.name]
		if !ok {
			t.Errorf("unexpected entry %s", e.name)
			continue
		}
		if e.mode != m || e.uid != 0 || e.uname != "root" {
			t.Errorf("%s: mode %o uid %d uname %q", e.name, e.mode, e.uid, e.uname)
		}
	}
	// Reproducible: packing again gives the same bytes.
	a, _ := os.ReadFile(out)
	out2 := filepath.Join(root, "out2", "airview-pi-1.2.3.tar.gz")
	if _, err := Pack(stage, out2, when); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out2)
	if !bytes.Equal(a, b) {
		t.Error("pack is not reproducible")
	}
	// And the check accepts it.
	rep, err := Check(filepath.Dir(out), CheckOptions{Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("problems: %v", rep.Problems)
	}
}

func TestPackRejectsBadNames(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "bundle")
	write(t, filepath.Join(stage, "Install service.cmd"), []byte("x"))
	if _, err := Pack(stage, filepath.Join(root, "b.tar.gz"), time.Unix(0, 0)); err == nil {
		t.Fatal("a name with a space was packed")
	}
}

func TestSumsAndCheck(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rel")
	stage := stagePi(t, root, "airview-pi-1.2.3")
	if _, err := Pack(stage, filepath.Join(dir, "airview-pi-1.2.3.tar.gz"), time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "airview-linux-amd64"), fakeELF(t, 62, false))
	write(t, filepath.Join(dir, "airview-server-install.sh"), []byte("#!/bin/sh\nexit 0\n"))
	write(t, filepath.Join(dir, "unrelated.txt"), []byte("not an artifact"))
	write(t, filepath.Join(dir, "airview.exe.old"), []byte("old"))
	var out bytes.Buffer
	if rc := run([]string{"sums", "-dir", dir}, &out, &out); rc != 0 {
		t.Fatalf("sums: %d %s", rc, out.String())
	}
	s, _ := os.ReadFile(filepath.Join(dir, SumsName))
	if strings.Contains(string(s), "unrelated") || strings.Contains(string(s), ".old") || strings.Contains(string(s), "\r") {
		t.Fatalf("SHA256SUMS:\n%s", s)
	}
	if strings.Count(string(s), "\n") != 3 {
		t.Fatalf("want 3 lines:\n%s", s)
	}
	out.Reset()
	if rc := run([]string{"check", "-dir", dir, "-version", "1.2.3", "-require", "airview-pi-1.2.3.tar.gz,airview-linux-amd64"}, &out, &out); rc != 0 {
		t.Fatalf("check: %d\n%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "airview-pi-1.2.3.tar.gz") || !strings.Contains(out.String(), "OK: 4 artifact(s)") {
		t.Fatalf("listing:\n%s", out.String())
	}

	// Each of these breaks the check.
	cases := []struct {
		name   string
		break_ func()
		want   string
	}{
		{"missing required", func() {}, "required artifact missing"},
		{"tampered", func() { write(t, filepath.Join(dir, "airview-linux-amd64"), fakeELF(t, 62, false)[:70]) }, "does not match"},
		{"unlisted", func() { write(t, filepath.Join(dir, "airview-new.bin"), []byte("x")) }, "is not listed"},
		{"wrong arch", func() { write(t, filepath.Join(dir, "airview-linux-arm64"), fakeELF(t, 62, false)) }, "ELF machine"},
		{"crlf script", func() { write(t, filepath.Join(dir, "airview-x.sh"), []byte("#!/bin/sh\r\nexit 0\r\n")) }, "CR characters"},
		{"wrong version", func() {}, `version "1.2.3", want "9.9.9"`},
	}
	for _, c := range cases {
		c.break_()
		var req []string
		if c.name == "missing required" {
			req = []string{"airview-missing.zip"}
		}
		rep, err := Check(dir, CheckOptions{Version: "9.9.9", Require: req, Sums: true})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(rep.Problems, "\n"), c.want) {
			t.Errorf("%s: problem %q not reported: %v", c.name, c.want, rep.Problems)
		}
	}

	// Scoped to this build's artifacts: files left from an earlier build are
	// listed as extra, not checked and not required in SHA256SUMS.
	dir2 := filepath.Join(root, "rel2")
	write(t, filepath.Join(dir2, "airview-linux-amd64"), fakeELF(t, 62, false))
	write(t, filepath.Join(dir2, "airview-pi.tar.gz"), []byte("stale, not even gzip"))
	out.Reset()
	if rc := run([]string{"sums", "-dir", dir2, "-files", "airview-linux-amd64"}, &out, &out); rc != 0 {
		t.Fatalf("sums -files: %s", out.String())
	}
	rep, err := Check(dir2, CheckOptions{Require: []string{"airview-linux-amd64"}, Sums: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("scoped check: %v", rep.Problems)
	}
	var b bytes.Buffer
	rep.Print(&b)
	if !strings.Contains(b.String(), "not part of this build") {
		t.Fatalf("stale file not marked:\n%s", b.String())
	}
}

// writeTar writes a hand-made archive to test the mode and owner rules.
func writeTar(t *testing.T, p string, ents []tarEnt) {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Uid: e.uid, Uname: e.uname, Gname: e.uname, Size: int64(len(e.body))}
		if e.typ == tar.TypeDir {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write(e.body)
	}
	tw.Close()
	gz.Close()
	write(t, p, b.Bytes())
}

func TestCheckTarRules(t *testing.T) {
	elf := fakeELF(t, 183, false)
	cases := []struct {
		name string
		ents []tarEnt
		want string
	}{
		{"script not exec", []tarEnt{
			{"b/", 0o755, tar.TypeDir, 0, "root", nil},
			{"b/install.sh", 0o644, tar.TypeReg, 0, "root", []byte("#!/bin/sh\n")}}, "mode 0644, want 0755"},
		{"binary not exec", []tarEnt{
			{"b/", 0o755, tar.TypeDir, 0, "root", nil},
			{"b/airview-linux-arm64", 0o644, tar.TypeReg, 0, "root", elf}}, "mode 0644, want 0755"},
		{"text exec", []tarEnt{
			{"b/", 0o755, tar.TypeDir, 0, "root", nil},
			{"b/README.md", 0o755, tar.TypeReg, 0, "root", []byte("x")}}, "mode 0755, want 0644"},
		{"user owned", []tarEnt{
			{"b/", 0o755, tar.TypeDir, 1000, "chris", nil}}, "want root"},
		{"dir mode", []tarEnt{
			{"b/", 0o777, tar.TypeDir, 0, "root", nil}}, "directory mode 0777"},
		{"outside", []tarEnt{
			{"b/", 0o755, tar.TypeDir, 0, "root", nil},
			{"c/x", 0o644, tar.TypeReg, 0, "root", []byte("x")}}, "outside the top-level"},
		{"symlink", []tarEnt{
			{"b/", 0o755, tar.TypeDir, 0, "root", nil},
			{"b/l", 0o777, tar.TypeSymlink, 0, "root", nil}}, "entry type"},
		{"no top", []tarEnt{
			{"b/x", 0o644, tar.TypeReg, 0, "root", []byte("x")}}, "no top-level directory"},
		{"crlf library", []tarEnt{
			{"b/", 0o755, tar.TypeDir, 0, "root", nil},
			{"b/lib.sh", 0o644, tar.TypeReg, 0, "root", []byte("x=1\r\n")}}, "CRLF line ends"},
		{"no arch", []tarEnt{
			{"b/", 0o755, tar.TypeDir, 0, "root", nil},
			{"b/airview", 0o755, tar.TypeReg, 0, "root", elf}}, "CPU architecture not named"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		writeTar(t, filepath.Join(dir, "b.tar.gz"), c.ents)
		rep := &Report{}
		checkTarGz(rep, filepath.Join(dir, "b.tar.gz"), "")
		if !strings.Contains(strings.Join(rep.Problems, "\n"), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, rep.Problems)
		}
	}
	// The server bundle names the CPU in the archive name only.
	dir := t.TempDir()
	writeTar(t, filepath.Join(dir, "airview-server-linux-arm64.tar.gz"), []tarEnt{
		{"airview-server-linux-arm64/", 0o755, tar.TypeDir, 0, "root", nil},
		{"airview-server-linux-arm64/airview", 0o755, tar.TypeReg, 0, "root", elf},
		{"airview-server-linux-arm64/LICENSE.txt", 0o644, tar.TypeReg, 0, "root", []byte("l")},
		{"airview-server-linux-arm64/THIRD-PARTY-NOTICES.txt", 0o644, tar.TypeReg, 0, "root", []byte("n")}})
	rep := &Report{}
	checkTarGz(rep, filepath.Join(dir, "airview-server-linux-arm64.tar.gz"), "")
	if len(rep.Problems) != 0 {
		t.Errorf("server bundle: %v", rep.Problems)
	}
	// DLV-05: a bundle with programs but without the licence texts fails.
	writeTar(t, filepath.Join(dir, "airview-feeder-pi-1.0.0.tar.gz"), []tarEnt{
		{"airview-feeder-pi-1.0.0/", 0o755, tar.TypeDir, 0, "root", nil},
		{"airview-feeder-pi-1.0.0/airview-feeder-linux-arm64", 0o755, tar.TypeReg, 0, "root", elf},
		{"airview-feeder-pi-1.0.0/LICENSE.txt", 0o644, tar.TypeReg, 0, "root", []byte("l")}})
	rep = &Report{}
	checkTarGz(rep, filepath.Join(dir, "airview-feeder-pi-1.0.0.tar.gz"), "")
	if p := strings.Join(rep.Problems, "\n"); !strings.Contains(p, "THIRD-PARTY-NOTICES.txt") || strings.Contains(p, "without airview-feeder-pi-1.0.0/LICENSE.txt") {
		t.Errorf("missing notices: %v", rep.Problems)
	}
}

// DLV-05: a ZIP with Windows programs must carry LICENSE.txt and
// THIRD-PARTY-NOTICES.txt (top level or its single folder).
func TestCheckZipNotices(t *testing.T) {
	mk := func(files map[string][]byte) string {
		p := filepath.Join(t.TempDir(), "AirView-Feeder-1.0.0-windows-amd64.zip")
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		for n, d := range files {
			w, _ := zw.Create(n)
			w.Write(d)
		}
		zw.Close()
		write(t, p, b.Bytes())
		return p
	}
	notices := func(problems []string) (n int) {
		for _, p := range problems {
			if strings.Contains(p, "licence notices") {
				n++
			}
		}
		return n
	}
	dir := "AirView-Feeder-1.0.0-windows-amd64/"
	exe := []byte("MZ not really a program")
	rep := &Report{}
	checkZip(rep, mk(map[string][]byte{dir + "airview-feeder.exe": exe, dir + "README.txt": []byte("r")}))
	if notices(rep.Problems) != 2 {
		t.Errorf("ZIP without notices: %v", rep.Problems)
	}
	rep = &Report{}
	checkZip(rep, mk(map[string][]byte{dir + "airview-feeder.exe": exe, dir + "LICENSE.txt": []byte("l"), dir + "THIRD-PARTY-NOTICES.txt": []byte("n")}))
	if notices(rep.Problems) != 0 {
		t.Errorf("good ZIP: %v", rep.Problems)
	}
	// A ZIP without programs (the Pi image's .img.zip) needs none.
	rep = &Report{}
	checkZip(rep, mk(map[string][]byte{"AirView-Feeder-Pi-1.0.0.img": []byte("img")}))
	if notices(rep.Problems) != 0 {
		t.Errorf("image ZIP: %v", rep.Problems)
	}
}

func TestCheckZipCRC(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "airview-x.zip")
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "a.txt", Method: zip.Store})
	w.Write([]byte("hello world"))
	zw.Close()
	raw := b.Bytes()
	write(t, p, raw)
	rep := &Report{}
	checkZip(rep, p)
	if len(rep.Problems) != 0 {
		t.Fatalf("good zip: %v", rep.Problems)
	}
	i := bytes.Index(raw, []byte("hello"))
	raw[i] = 'j'
	write(t, p, raw)
	rep = &Report{}
	checkZip(rep, p)
	if len(rep.Problems) == 0 {
		t.Fatal("damaged zip member not detected")
	}
}

func TestArchOf(t *testing.T) {
	for in, want := range map[string]string{
		"airview-linux-arm64":                  "arm64",
		"airview-feeder-linux-armv7":           "armv7",
		"airview-server-linux-amd64.tar.gz":    "amd64",
		"AirView-Feeder-Setup-1.0.0-arm64.exe": "arm64",
		"AirView-Setup-1.0.0.exe":              "",
		"airview-pi-1.0.0.tar.gz":              "",
		"AirView-Feeder-1.0.0-windows-arm64":   "arm64",
	} {
		if got := archOf(in); got != want {
			t.Errorf("archOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoticesStdlibOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	mods, err := linkedModules("./", "linux", "arm64")
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	if len(mods) != 0 {
		t.Fatalf("relkit links third-party modules: %v", mods)
	}
	text, err := Notices(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "The Go programming language") || strings.Contains(text, "\r") {
		t.Fatalf("notices:\n%.300s", text)
	}
	if _, err := Notices([]module{{Path: "example.com/x", Version: "v1", Dir: t.TempDir()}}, ""); err == nil {
		t.Fatal("a module without a licence was accepted")
	}
}

func TestCheckSidecarAndLibrary(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "airview-img.zip"), mustZip(t))
	h, _ := fileSHA256(filepath.Join(dir, "airview-img.zip"))
	write(t, filepath.Join(dir, "airview-img.zip.sha256"), []byte(h+"  airview-img.zip\n"))
	writeTar(t, filepath.Join(dir, "airview-lib.tar.gz"), []tarEnt{
		{"airview-lib/", 0o755, tar.TypeDir, 0, "root", nil},
		{"airview-lib/lib.sh", 0o644, tar.TypeReg, 0, "root", []byte("x=1\n")}})
	rep, err := Check(dir, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("problems: %v", rep.Problems)
	}
	write(t, filepath.Join(dir, "airview-img.zip.sha256"), []byte(strings.Repeat("0", 64)+"  airview-img.zip\n"))
	rep, _ = Check(dir, CheckOptions{})
	if !strings.Contains(strings.Join(rep.Problems, "\n"), "does not match airview-img.zip") {
		t.Fatalf("bad sidecar not reported: %v", rep.Problems)
	}
}

func mustZip(t *testing.T) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("hello"))
	zw.Close()
	return b.Bytes()
}
