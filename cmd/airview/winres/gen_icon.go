//go:build ignore

// gen_icon draws the AirView application icon (the artwork of
// web/static/favicon.svg, re-created procedurally because no SVG rasteriser
// is available in a pure-Go toolchain) and writes icon.png (256 px) plus a
// hand-tuned small size next to this file:
//
//	go run gen_icon.go
//
// go-winres packs the PNGs into the EXE's icon resource (see winres.json).
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

type rgba struct{ r, g, b, a float64 } // straight alpha, 0..1

func hex(s string, a float64) rgba {
	v := func(i int) float64 {
		var n int
		for _, c := range s[i : i+2] {
			n *= 16
			switch {
			case c >= '0' && c <= '9':
				n += int(c - '0')
			case c >= 'a' && c <= 'f':
				n += int(c-'a') + 10
			}
		}
		return float64(n) / 255
	}
	return rgba{v(1), v(3), v(5), a}
}

func mix(a, b rgba, t float64) rgba {
	return rgba{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t, a.a + (b.a-a.a)*t}
}

type stop struct {
	at float64
	c  rgba
}

func ramp(stops []stop, t float64) rgba {
	if t <= stops[0].at {
		return stops[0].c
	}
	for i := 1; i < len(stops); i++ {
		if t <= stops[i].at {
			return mix(stops[i-1].c, stops[i].c, (t-stops[i-1].at)/(stops[i].at-stops[i-1].at))
		}
	}
	return stops[len(stops)-1].c
}

// linear returns the gradient parameter of (x, y) along (x1,y1)->(x2,y2).
func linear(x, y, x1, y1, x2, y2 float64) float64 {
	dx, dy := x2-x1, y2-y1
	return ((x-x1)*dx + (y-y1)*dy) / (dx*dx + dy*dy)
}

var (
	gradG = []stop{{0, hex("#8ccbff", 1)}, {0.55, hex("#2f7dff", 1)}, {1, hex("#0b3bff", 1)}}
	gradB = []stop{{0, hex("#12409c", 1)}, {0.55, hex("#071a45", 1)}, {1, hex("#02040a", 1)}}
)

// inRoundRect: rectangle [x0,x1] x [y0,y1] with corner radius r.
func inRoundRect(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Max(x0+r, math.Min(x, x1-r))
	cy := math.Max(y0+r, math.Min(y, y1-r))
	return math.Hypot(x-cx, y-cy) <= r
}

func inPoly(x, y float64, p [][2]float64) bool {
	in := false
	for i, j := 0, len(p)-1; i < len(p); j, i = i, i+1 {
		xi, yi, xj, yj := p[i][0], p[i][1], p[j][0], p[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

// distSeg is the distance from (x, y) to the segment a-b.
func distSeg(x, y, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := ((x-ax)*dx + (y-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(x-ax-t*dx, y-ay-t*dy)
}

func over(dst *rgba, src rgba) {
	a := src.a + dst.a*(1-src.a)
	if a <= 0 {
		*dst = rgba{}
		return
	}
	dst.r = (src.r*src.a + dst.r*dst.a*(1-src.a)) / a
	dst.g = (src.g*src.a + dst.g*dst.a*(1-src.a)) / a
	dst.b = (src.b*src.a + dst.b*dst.a*(1-src.a)) / a
	dst.a = a
}

// sample evaluates the artwork at (x, y) in the 64 x 64 design space. bold
// thickens the strokes for the small icon sizes.
func sample(x, y, bold float64) rgba {
	var c rgba
	// Background tile.
	if !inRoundRect(x, y, 0, 0, 64, 64, 15) {
		return c
	}
	over(&c, ramp(gradB, math.Hypot(x-40, y-52)/56))
	g := ramp(gradG, linear(x, y, 8, 6, 56, 60))

	// Border.
	bw := 1.5 * bold
	if !inRoundRect(x, y, bw, bw, 64-bw, 64-bw, 15-bw) {
		b := g
		b.a = 0.6
		over(&c, b)
	}
	d := math.Hypot(x-32, y-32)
	// Radar sweep: 0..60 degrees clockwise from north.
	if d <= 22 {
		ang := math.Atan2(x-32, 32-y) * 180 / math.Pi
		if ang >= 0 && ang <= 60 {
			t := math.Max(0, math.Min(1, linear(x, y, 32, 32, 48, 12)))
			s := hex("#22d3ee", 0.6*t)
			over(&c, s)
		}
	}
	// Outer ring.
	if math.Abs(d-22) <= 1.1*bold {
		over(&c, g)
	}
	// Inner ring.
	if math.Abs(d-13) <= 0.65*bold {
		over(&c, hex("#5aa2ff", 0.45))
	}
	// Ticks.
	ticks := [][4]float64{{32, 6.5, 32, 11.5}, {32, 52.5, 32, 57.5}, {6.5, 32, 11.5, 32}, {52.5, 32, 57.5, 32}}
	for _, t := range ticks {
		if distSeg(x, y, t[0], t[1], t[2], t[3]) <= 0.8*bold {
			over(&c, hex("#5aa2ff", 0.7))
			break
		}
	}
	// Aircraft / pointer.
	if inPoly(x, y, [][2]float64{{45, 19}, {36.8, 43.6}, {32.3, 33.7}, {22.4, 29.2}}) {
		over(&c, hex("#ffffff", 1))
	}
	if inPoly(x, y, [][2]float64{{45, 19}, {32.3, 33.7}, {36.8, 43.6}}) {
		over(&c, hex("#b9dcff", 1))
	}
	return c
}

func render(size int, bold float64) *image.NRGBA {
	const ss = 6 // supersampling per axis
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := 64 / float64(size)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * scale
					y := (float64(py) + (float64(sy)+0.5)/ss) * scale
					c := sample(x, y, bold)
					r += c.r * c.a
					g += c.g * c.a
					b += c.b * c.a
					a += c.a
				}
			}
			if a > 0 {
				r, g, b = r/a, g/a, b/a
			}
			a /= ss * ss
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(math.Round(r * 255)), G: uint8(math.Round(g * 255)),
				B: uint8(math.Round(b * 255)), A: uint8(math.Round(a * 255)),
			})
		}
	}
	return img
}

func write(name string, img image.Image) {
	f, err := os.Create(name)
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}

func main() {
	write("icon.png", render(256, 1))
	write("icon48.png", render(48, 1.15))
	write("icon32.png", render(32, 1.3))
	write("icon16.png", render(16, 1.6))
}
