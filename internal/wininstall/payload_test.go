package wininstall

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePE is a minimal PE32+ header (enough for overlayEnd) padded to n bytes.
func fakePE(n int) []byte {
	b := make([]byte, n)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x80)
	copy(b[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[0x80+24:], 0x20b)
	return b
}

// sign appends a fake certificate table and records it in the security
// directory, as signtool does.
func sign(img []byte) []byte {
	cert := bytes.Repeat([]byte{0xCE}, 64)
	off := len(img)
	dd := 0x80 + 24 + 112 + 4*8
	binary.LittleEndian.PutUint32(img[dd:], uint32(off))
	binary.LittleEndian.PutUint32(img[dd+4:], uint32(len(cert)))
	return append(img, cert...)
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for n, c := range files {
		p := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// buildTestInstaller writes a stub and an installer with the given files.
func buildTestInstaller(t *testing.T, dir string, p *Product, files map[string]string) string {
	t.Helper()
	stub := filepath.Join(dir, "stub.exe")
	if err := os.WriteFile(stub, fakePE(4093), 0o644); err != nil { // odd size: alignment padding
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	writeTree(t, src, files)
	var specs []FileSpec
	for n := range files {
		specs = append(specs, FileSpec{Src: filepath.Join(src, filepath.FromSlash(n)), Dest: n})
	}
	out := filepath.Join(dir, "setup.exe")
	if err := Build(BuildOptions{Stub: stub, Product: p, Files: specs, Out: out}); err != nil {
		t.Fatal(err)
	}
	return out
}

func testFiles() map[string]string {
	return map[string]string{
		"airview.exe":         "MZ program v1",
		"tray.exe":            "MZ tray v1",
		"LICENSE.txt":         "licence text",
		"docs/GUIDE.md":       "# guide",
		"docs/old/REMOVED.md": "only in v1",
	}
}

func TestBuildAndReadPayload(t *testing.T) {
	dir := t.TempDir()
	out := buildTestInstaller(t, dir, validProduct(), testFiles())
	pl, err := OpenPayload(out)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()
	if pl.StubSize != 4093 {
		t.Errorf("stub size %d", pl.StubSize)
	}
	if pl.Product.ID != "AirViewTest" || pl.Product.Version != "1.2.3" || len(pl.Files) != 5 {
		t.Fatalf("payload %+v, %d files", pl.Product, len(pl.Files))
	}
	rc, err := pl.Open("docs/GUIDE.md")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "# guide" {
		t.Errorf("content %q", b)
	}
	st, _ := os.Stat(out)
	if st.Size()%8 != 0 {
		t.Errorf("installer size %d not 8-byte aligned", st.Size())
	}
	if n, err := StubLength(out); err != nil || n != 4093 {
		t.Errorf("StubLength %d %v", n, err)
	}

	// Deterministic: the same inputs give the same bytes.
	first, _ := os.ReadFile(out)
	out2 := buildTestInstaller(t, t.TempDir(), validProduct(), testFiles())
	second, _ := os.ReadFile(out2)
	if !bytes.Equal(first, second) {
		t.Error("two builds differ")
	}

	// A signed installer still opens.
	signed := sign(append([]byte(nil), first...))
	pl2, err := ReadPayload(bytes.NewReader(signed), int64(len(signed)))
	if err != nil {
		t.Fatalf("signed: %v", err)
	}
	if pl2.StubSize != 4093 {
		t.Errorf("signed stub size %d", pl2.StubSize)
	}
}

func TestReadPayloadErrors(t *testing.T) {
	stub := fakePE(4096)
	if _, err := ReadPayload(bytes.NewReader(stub), int64(len(stub))); !errors.Is(err, ErrNoPayload) {
		t.Errorf("bare stub: %v", err)
	}
	// Corrupt trailer length.
	bad := append(append([]byte(nil), stub...), make([]byte, 16)...)
	binary.LittleEndian.PutUint32(bad[len(bad)-16:], 1<<30)
	copy(bad[len(bad)-8:], trailerMagic)
	if _, err := ReadPayload(bytes.NewReader(bad), int64(len(bad))); err == nil || errors.Is(err, ErrNoPayload) {
		t.Errorf("corrupt trailer: %v", err)
	}
	// Zip slip.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create(ProductEntry)
	pj := `{"id":"A","name":"A","version":"1.0","mainExe":"a.exe","defaultDir":"{pf}\\A"}`
	_, _ = w.Write([]byte(pj))
	w, _ = zw.Create("files/../evil.exe")
	_, _ = w.Write([]byte("x"))
	zw.Close()
	var img bytes.Buffer
	img.Write(stub)
	if err := WritePayload(&img, zb.Bytes(), int64(len(stub))); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPayload(bytes.NewReader(img.Bytes()), int64(img.Len())); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Errorf("zip slip: %v", err)
	}
}

func TestBuildRefusals(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.exe")
	_ = os.WriteFile(stub, fakePE(1024), 0o644)
	src := filepath.Join(dir, "a.exe")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	lic := filepath.Join(dir, "LICENSE.txt")
	_ = os.WriteFile(lic, []byte("x"), 0o644)
	p := validProduct()
	base := []FileSpec{{Src: src, Dest: "airview.exe"}, {Src: lic, Dest: "LICENSE.txt"}}
	cases := map[string][]FileSpec{
		"no main exe":  {{Src: lic, Dest: "LICENSE.txt"}},
		"no licence":   {{Src: src, Dest: "airview.exe"}},
		"duplicate":    append(append([]FileSpec(nil), base...), FileSpec{Src: src, Dest: "AIRVIEW.EXE"}),
		"dotdot":       append(append([]FileSpec(nil), base...), FileSpec{Src: src, Dest: "../x"}),
		"absolute":     append(append([]FileSpec(nil), base...), FileSpec{Src: src, Dest: "C:/x"}),
		"uninstaller":  append(append([]FileSpec(nil), base...), FileSpec{Src: src, Dest: "uninstall.exe"}),
		"missing file": append(append([]FileSpec(nil), base...), FileSpec{Src: filepath.Join(dir, "nope"), Dest: "y"}),
	}
	for name, files := range cases {
		if err := Build(BuildOptions{Stub: stub, Product: p, Files: files, Out: filepath.Join(dir, "o.exe")}); err == nil {
			t.Errorf("%s: built", name)
		}
	}
	if err := Build(BuildOptions{Stub: stub, Product: p, Files: base, Out: filepath.Join(dir, "ok.exe")}); err != nil {
		t.Fatal(err)
	}
	// A stub that already has a payload is refused.
	if err := Build(BuildOptions{Stub: filepath.Join(dir, "ok.exe"), Product: p, Files: base, Out: filepath.Join(dir, "o2.exe")}); err == nil {
		t.Error("stub with payload accepted")
	}
	// Not a PE.
	_ = os.WriteFile(filepath.Join(dir, "txt"), []byte("hello"), 0o644)
	if err := Build(BuildOptions{Stub: filepath.Join(dir, "txt"), Product: p, Files: base, Out: filepath.Join(dir, "o3.exe")}); err == nil {
		t.Error("non-PE stub accepted")
	}
}

func BenchmarkOpenPayload(b *testing.B) {
	dir := b.TempDir()
	t := &testing.T{}
	_ = t
	stub := filepath.Join(dir, "stub.exe")
	_ = os.WriteFile(stub, fakePE(1<<20), 0o644)
	big := filepath.Join(dir, "big.bin")
	_ = os.WriteFile(big, bytes.Repeat([]byte("airview "), 4<<20), 0o644)
	lic := filepath.Join(dir, "LICENSE.txt")
	_ = os.WriteFile(lic, []byte("x"), 0o644)
	out := filepath.Join(dir, "setup.exe")
	if err := Build(BuildOptions{Stub: stub, Product: validProduct(), Files: []FileSpec{{Src: big, Dest: "airview.exe"}, {Src: lic, Dest: "LICENSE.txt"}}, Out: out, Level: 1}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pl, err := OpenPayload(out)
		if err != nil {
			b.Fatal(err)
		}
		pl.Close()
	}
}
