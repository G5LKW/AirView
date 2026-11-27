package xz

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Test vectors were produced with xz-utils 5.x (see testdata/README.md); the
// expected outputs are recorded as SHA-256 and length.
var vectors = []struct {
	file string
	sum  string
	size int64
}{
	{"empty.xz", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", 0},
	{"one.xz", "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", 1},
	{"text.crc64.xz", "54711b284e848827733c19578aaeaa3fdde10c5b2538079a9249e9262f868b2a", 1260503},
	{"text.none.xz", "54711b284e848827733c19578aaeaa3fdde10c5b2538079a9249e9262f868b2a", 1260503},
	{"text.dict4k.xz", "54711b284e848827733c19578aaeaa3fdde10c5b2538079a9249e9262f868b2a", 1260503},
	{"mixed.crc32.xz", "ab447ffbdd4109b41fb47cb5b57f61f2d94b9bf270003e86a4f6761b82bb331b", 1300000},
	{"mixed.blocks.sha256.xz", "ab447ffbdd4109b41fb47cb5b57f61f2d94b9bf270003e86a4f6761b82bb331b", 1300000},
	{"mixed.lp2.xz", "ab447ffbdd4109b41fb47cb5b57f61f2d94b9bf270003e86a4f6761b82bb331b", 1300000},
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeVectors(t *testing.T) {
	for _, v := range vectors {
		t.Run(v.file, func(t *testing.T) {
			var out bytes.Buffer
			n, err := Decode(&out, bytes.NewReader(read(t, v.file)))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(out.Bytes())
			if n != v.size || int64(out.Len()) != v.size || hex.EncodeToString(sum[:]) != v.sum {
				t.Fatalf("got %d bytes sha256 %x, want %d %s", n, sum, v.size, v.sum)
			}
		})
	}
}

// memWA is an in-memory io.WriterAt.
type memWA struct {
	mu sync.Mutex
	b  []byte
}

func (m *memWA) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if need := int(off) + len(p); need > len(m.b) {
		m.b = append(m.b, make([]byte, need-len(m.b))...)
	}
	copy(m.b[off:], p)
	return len(p), nil
}

func TestDecodeFileParallel(t *testing.T) {
	for _, v := range vectors {
		for _, workers := range []int{1, 3, 0} {
			in := read(t, v.file)
			var m memWA
			n, err := DecodeFile(&m, bytes.NewReader(in), int64(len(in)), workers)
			if err != nil {
				t.Fatalf("%s workers=%d: %v", v.file, workers, err)
			}
			sum := sha256.Sum256(m.b)
			if n != v.size || hex.EncodeToString(sum[:]) != v.sum {
				t.Fatalf("%s workers=%d: got %d bytes %x", v.file, workers, n, sum)
			}
		}
	}
	p, err := planBlocks(bytes.NewReader(read(t, "mixed.blocks.sha256.xz")), int64(len(read(t, "mixed.blocks.sha256.xz"))))
	if err != nil || len(p.blocks) != 5 {
		t.Fatalf("plan: %v, %d blocks (want 5)", err, len(p.blocks))
	}
}

func TestMultiStreamAndPadding(t *testing.T) {
	in := read(t, "multi.xz")
	var out bytes.Buffer
	if _, err := Decode(&out, bytes.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 1+1260503 || out.Bytes()[0] != 'x' {
		t.Fatalf("multi-stream: %d bytes", out.Len())
	}
	// Stream padding (4 zero bytes) between and after streams is legal.
	one := read(t, "one.xz")
	padded := append(append(append([]byte{}, one...), 0, 0, 0, 0), one...)
	padded = append(padded, 0, 0, 0, 0)
	out.Reset()
	if _, err := Decode(&out, bytes.NewReader(padded)); err != nil || out.String() != "xx" {
		t.Fatalf("padded: %q %v", out.String(), err)
	}
	var m memWA
	if n, err := DecodeFile(&m, bytes.NewReader(padded), int64(len(padded)), 4); err != nil || n != 2 {
		t.Fatalf("padded parallel: %d %v", n, err)
	}
	// Padding that is not a multiple of four is not.
	bad := append(append([]byte{}, one...), 0, 0)
	if _, err := Decode(&out, bytes.NewReader(bad)); err == nil {
		t.Fatal("odd stream padding accepted")
	}
}

func TestCorruptionDetected(t *testing.T) {
	for _, name := range []string{"text.crc64.xz", "mixed.crc32.xz", "mixed.blocks.sha256.xz", "text.none.xz"} {
		in := read(t, name)
		// Flip bytes across the whole file; every flip must be reported (or,
		// for "none" checks, at least never panic).
		for _, pos := range []int{0, 7, 13, 40, len(in) / 3, len(in) / 2, len(in) - 30, len(in) - 5, len(in) - 1} {
			c := append([]byte{}, in...)
			c[pos] ^= 0x55
			var out bytes.Buffer
			_, err := Decode(&out, bytes.NewReader(c))
			if err == nil && name != "text.none.xz" {
				t.Errorf("%s: flip at %d not detected", name, pos)
			}
			var m memWA
			_, err2 := DecodeFile(&m, bytes.NewReader(c), int64(len(c)), 4)
			if err2 == nil && name != "text.none.xz" {
				t.Errorf("%s: parallel: flip at %d not detected", name, pos)
			}
		}
		// Truncation.
		for _, cut := range []int{1, 12, len(in) / 2, len(in) - 1} {
			var out bytes.Buffer
			if _, err := Decode(&out, bytes.NewReader(in[:cut])); err == nil {
				t.Errorf("%s: truncation at %d not detected", name, cut)
			}
		}
	}
	if _, err := Decode(new(bytes.Buffer), bytes.NewReader([]byte("not an xz file at all"))); !errors.Is(err, ErrFormat) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestVarint(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		v    uint64
		n    int
		fail bool
	}{
		{[]byte{0}, 0, 1, false},
		{[]byte{0x7f}, 127, 1, false},
		{[]byte{0x80, 0x01}, 128, 2, false},
		{[]byte{0x80, 0x00}, 0, 0, true}, // non-minimal
		{[]byte{0x80}, 0, 0, true},
	} {
		v, n, err := readVarint(c.in)
		if (err != nil) != c.fail || (!c.fail && (v != c.v || n != c.n)) {
			t.Errorf("%x: %d %d %v", c.in, v, n, err)
		}
	}
}

// TestRealImage decodes a real Raspberry Pi OS image when AIRVIEW_XZ_TEST
// names one (with AIRVIEW_XZ_SHA256 the expected decompressed SHA-256).
func TestRealImage(t *testing.T) {
	p := os.Getenv("AIRVIEW_XZ_TEST")
	if p == "" {
		t.Skip("set AIRVIEW_XZ_TEST=<file.img.xz> [AIRVIEW_XZ_SHA256=<hex>]")
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	out, err := os.CreateTemp(t.TempDir(), "img")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	n, err := DecodeFile(out, f, st.Size(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decoded %d bytes", n)
	if want := os.Getenv("AIRVIEW_XZ_SHA256"); want != "" {
		h := sha256.New()
		out.Seek(0, 0)
		if _, err := out.WriteTo(h); err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			t.Fatalf("sha256 %s, want %s", got, want)
		}
	}
}
