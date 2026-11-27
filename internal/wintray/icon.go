package wintray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Level is the state shown by the badge in the icon's lower right corner.
type Level int

const (
	// LevelNone draws no badge (application icon).
	LevelNone Level = iota
	// LevelOK is green: feeding.
	LevelOK
	// LevelWarn is amber: running, but something needs attention
	// (no aggregator configured, receiver or uplink not connected yet).
	LevelWarn
	// LevelError is red: the service failed or an uplink was refused.
	LevelError
	// LevelIdle is grey: paused, stopped or not installed.
	LevelIdle
)

// String returns the level name.
func (l Level) String() string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	case LevelIdle:
		return "idle"
	}
	return "none"
}

var (
	tileColor   = color.NRGBA{0x0E, 0x2A, 0x4F, 0xFF} // AirView navy
	rimColor    = color.NRGBA{0x3A, 0x6E, 0xA8, 0xFF}
	glyphColor  = color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}
	badgeColors = map[Level]color.NRGBA{
		LevelOK:    {0x2E, 0xCC, 0x71, 0xFF},
		LevelWarn:  {0xF5, 0xA6, 0x23, 0xFF},
		LevelError: {0xE5, 0x48, 0x4D, 0xFF},
		LevelIdle:  {0x8A, 0x96, 0xA3, 0xFF},
	}
)

// BadgeColor returns the badge colour of a level (zero for LevelNone).
func BadgeColor(l Level) color.NRGBA { return badgeColors[l] }

// DrawIcon renders the feeder icon at size×size pixels (size ≥ 8): a
// rounded navy tile with a white "broadcast" glyph (a dot and two quarter
// rings) and, unless level is LevelNone, a status badge. It uses 4×4
// supersampling, so small sizes stay smooth.
func DrawIcon(size int, level Level) *image.NRGBA {
	if size < 8 {
		size = 8
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	badge, hasBadge := badgeColors[level]
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64 // premultiplied sums
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					v := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					c, ok := iconSample(u, v, hasBadge, badge)
					if !ok {
						continue
					}
					r += float64(c.R)
					g += float64(c.G)
					b += float64(c.B)
					a++
				}
			}
			if a == 0 {
				continue
			}
			n := float64(ss * ss)
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(math.Round(r / a)), G: uint8(math.Round(g / a)), B: uint8(math.Round(b / a)),
				A: uint8(math.Round(255 * a / n)),
			})
		}
	}
	return img
}

// iconSample returns the colour at (u, v) in [0,1]² (v downwards) and
// whether the point is covered at all.
func iconSample(u, v float64, hasBadge bool, badge color.NRGBA) (color.NRGBA, bool) {
	const (
		margin = 0.03
		radius = 0.20
		rim    = 0.035
	)
	var c color.NRGBA
	covered := false
	if d := roundedRectSDF(u, v, margin, radius); d <= 0 {
		covered = true
		c = tileColor
		if d > -rim {
			c = rimColor
		}
		// Glyph: a dot at P and two quarter rings opening up and to the right.
		const px, py = 0.30, 0.70
		dx, dy := u-px, v-py
		dist := math.Hypot(dx, dy)
		switch {
		case dist <= 0.085:
			c = glyphColor
		case dx >= -0.01 && dy <= 0.01 && (math.Abs(dist-0.27) <= 0.05 || math.Abs(dist-0.45) <= 0.05):
			c = glyphColor
		}
	}
	if hasBadge {
		const bx, by = 0.76, 0.76
		d := math.Hypot(u-bx, v-by)
		switch {
		case d <= 0.18:
			return badge, true
		case d <= 0.235:
			return tileColor, true
		}
	}
	return c, covered
}

// roundedRectSDF is the signed distance from (u, v) to a square inset by
// margin with corner radius r (negative inside).
func roundedRectSDF(u, v, margin, r float64) float64 {
	half := 0.5 - margin
	qx := math.Abs(u-0.5) - (half - r)
	qy := math.Abs(v-0.5) - (half - r)
	ox, oy := math.Max(qx, 0), math.Max(qy, 0)
	return math.Hypot(ox, oy) + math.Min(math.Max(qx, qy), 0) - r
}

// PNG returns DrawIcon(size, level) encoded as PNG.
func PNG(size int, level Level) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, DrawIcon(size, level)) // cannot fail on a bytes.Buffer
	return buf.Bytes()
}

// ICO returns a Windows icon file with one PNG-compressed image per size
// (Windows Vista and later read PNG icon images). Sizes above 256 are
// clamped to 256.
func ICO(sizes []int, level Level) []byte {
	type entry struct {
		size int
		data []byte
	}
	var es []entry
	for _, s := range sizes {
		if s > 256 {
			s = 256
		}
		es = append(es, entry{s, PNG(s, level)})
	}
	var buf bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&buf, le, [3]uint16{0, 1, uint16(len(es))})
	off := 6 + 16*len(es)
	for _, e := range es {
		dim := uint8(e.size)
		if e.size >= 256 {
			dim = 0 // 0 means 256
		}
		_ = binary.Write(&buf, le, struct {
			W, H, Colors, Reserved uint8
			Planes, BPP            uint16
			Size, Offset           uint32
		}{dim, dim, 0, 0, 1, 32, uint32(len(e.data)), uint32(off)})
		off += len(e.data)
	}
	for _, e := range es {
		buf.Write(e.data)
	}
	return buf.Bytes()
}
