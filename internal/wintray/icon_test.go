package wintray

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestDrawIconLayers(t *testing.T) {
	for _, size := range []int{16, 20, 24, 32, 48, 256} {
		img := DrawIcon(size, LevelOK)
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Fatalf("size %d: bounds %v", size, b)
		}
		// Rounded corner: the very corner pixel is transparent.
		if a := img.NRGBAAt(0, 0).A; a != 0 {
			t.Errorf("size %d: corner alpha %d, want 0", size, a)
		}
		// Tile centre-top is opaque navy (no glyph there).
		c := img.NRGBAAt(size/2, size/6)
		if c.A != 255 || c.B < c.R {
			t.Errorf("size %d: tile pixel %v, want opaque navy", size, c)
		}
		// Badge centre carries the level colour.
		bc := img.NRGBAAt(size*76/100, size*76/100)
		want := BadgeColor(LevelOK)
		if absDiff(bc.R, want.R) > 8 || absDiff(bc.G, want.G) > 8 || absDiff(bc.B, want.B) > 8 {
			t.Errorf("size %d: badge %v, want %v", size, bc, want)
		}
		// Glyph dot is white.
		if g := img.NRGBAAt(size*30/100, size*70/100); g.R < 200 || g.G < 200 || g.B < 200 {
			t.Errorf("size %d: glyph dot %v, want white", size, g)
		}
	}
}

func TestLevelsDiffer(t *testing.T) {
	seen := map[string]Level{}
	for _, l := range []Level{LevelNone, LevelOK, LevelWarn, LevelError, LevelIdle} {
		s := string(PNG(32, l))
		if prev, ok := seen[s]; ok {
			t.Fatalf("levels %v and %v draw the same icon", prev, l)
		}
		seen[s] = l
		if l.String() == "" {
			t.Fatal("empty level name")
		}
	}
	// No badge: the badge area shows the tile/glyph, not a badge colour.
	img := DrawIcon(32, LevelNone)
	if c := img.NRGBAAt(24, 24); c == BadgeColor(LevelOK) {
		t.Fatal("LevelNone drew a badge")
	}
}

func TestICO(t *testing.T) {
	sizes := []int{16, 32, 48, 300}
	ico := ICO(sizes, LevelNone)
	le := binary.LittleEndian
	if le.Uint16(ico[0:]) != 0 || le.Uint16(ico[2:]) != 1 || int(le.Uint16(ico[4:])) != len(sizes) {
		t.Fatalf("bad ICONDIR % x", ico[:6])
	}
	for i, s := range sizes {
		e := ico[6+16*i:]
		w := int(e[0])
		if s >= 256 {
			if w != 0 {
				t.Fatalf("entry %d: width byte %d, want 0 (=256)", i, w)
			}
			s = 256
		} else if w != s {
			t.Fatalf("entry %d: width %d, want %d", i, w, s)
		}
		if le.Uint16(e[6:]) != 32 {
			t.Fatalf("entry %d: bpp %d", i, le.Uint16(e[6:]))
		}
		n, off := le.Uint32(e[8:]), le.Uint32(e[12:])
		if int(off+n) > len(ico) {
			t.Fatalf("entry %d out of range", i)
		}
		img, err := png.Decode(bytes.NewReader(ico[off : off+n]))
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if img.Bounds().Dx() != s {
			t.Fatalf("entry %d: image %d px, want %d", i, img.Bounds().Dx(), s)
		}
	}
}

func BenchmarkPNG16(b *testing.B) {
	for i := 0; i < b.N; i++ {
		PNG(16, LevelWarn)
	}
}

func absDiff(a, b uint8) uint8 {
	if a > b {
		return a - b
	}
	return b - a
}
