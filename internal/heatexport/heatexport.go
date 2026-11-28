// Package heatexport renders the all-time position heat map as a large
// JPEG (default 7680 x 4320, "8K UHD") for printing and sharing.
//
// It never holds a full-resolution RGBA picture: the heat is accumulated on
// a grid at a quarter of the resolution, the thin overlays (graticule, range
// rings, receiver, labels) are one bit per pixel, and the pixels are
// computed while the standard library's JPEG encoder reads them. An 8K
// export needs about 15 MB of memory, so it also works on a Raspberry Pi.
//
// The picture is a Web Mercator map without a base map: the background is
// a plain colour (no third-party tiles are fetched), with a graticule, the
// configured range rings around the receiver, a scale bar and a caption.
package heatexport

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"sort"
	"strings"

	"airview/internal/geo"
)

// Cell is one cell of the heat grid.
type Cell struct {
	Lat, Lon float64
	Count    int64
}

// Options configure a rendering.
type Options struct {
	Width, Height int     // pixels (default 7680 x 4320; 640..16384 each)
	Quality       int     // JPEG quality 1..100 (default 90)
	CellDeg       float64 // size of the input cells in degrees (splat size; default 0.05)
	Light         bool    // light background instead of dark
	RxOK          bool    // the receiver position is known
	RxLat, RxLon  float64 //
	RingsKm       []int   // range rings around the receiver
	Title         string  // first caption line
	Caption       string  // second caption line
}

// ErrNoData is returned when there is nothing to draw.
var ErrNoData = errors.New("heatexport: no positions in the heat grid yet")

const (
	maxSide = 16384
	minSide = 640
	ds      = 4 // density grid downscale
)

type ramp struct {
	at      float64
	r, g, b float64
	a       float64
}

// The same colours as the Live Map's heat layer.
var heatRamp = []ramp{
	{0, 10, 30, 90, 0}, {0.15, 47, 125, 255, 0.45}, {0.4, 34, 211, 238, 0.65},
	{0.65, 250, 204, 21, 0.75}, {0.9, 255, 77, 94, 0.85}, {1, 255, 240, 240, 0.9},
}

func rampAt(heatRamp []ramp, v float64) (r, g, b, a float64) {
	if v <= 0 {
		return 0, 0, 0, 0
	}
	if v >= 1 {
		s := heatRamp[len(heatRamp)-1]
		return s.r, s.g, s.b, s.a
	}
	for i := 1; i < len(heatRamp); i++ {
		if v <= heatRamp[i].at {
			p, q := heatRamp[i-1], heatRamp[i]
			f := (v - p.at) / (q.at - p.at)
			return p.r + (q.r-p.r)*f, p.g + (q.g-p.g)*f, p.b + (q.b-p.b)*f, p.a + (q.a-p.a)*f
		}
	}
	return 0, 0, 0, 0
}

// mercY is the Web Mercator y of a latitude (radians-based, unscaled).
func mercY(lat float64) float64 {
	lat = math.Max(-85, math.Min(85, lat))
	r := lat * math.Pi / 180
	return math.Log(math.Tan(math.Pi/4 + r/2))
}

// view maps lat/lon to pixels.
type view struct {
	w, h         int
	x0, y1       float64 // mercator x (radians) of the left edge, y of the top edge
	scale        float64 // pixels per mercator unit
	west, east   float64
	south, north float64
}

func (v *view) px(lat, lon float64) (float64, float64) {
	return (lon*math.Pi/180 - v.x0) * v.scale, (v.y1 - mercY(lat)) * v.scale
}

func (v *view) latAt(py float64) float64 {
	y := v.y1 - py/v.scale
	return (2*math.Atan(math.Exp(y)) - math.Pi/2) * 180 / math.Pi
}

// fit chooses a view that contains the bounds, padded, at the picture's aspect.
func fit(w, h int, west, south, east, north float64) *view {
	padX, padY := (east-west)*0.06+0.2, (north-south)*0.06+0.2
	west, east, south, north = west-padX, east+padX, math.Max(-84, south-padY), math.Min(84, north+padY)
	x0, x1 := west*math.Pi/180, east*math.Pi/180
	yb, yt := mercY(south), mercY(north)
	scale := math.Min(float64(w)/(x1-x0), float64(h)/(yt-yb))
	cx, cy := (x0+x1)/2, (yb+yt)/2
	v := &view{w: w, h: h, scale: scale, x0: cx - float64(w)/2/scale, y1: cy + float64(h)/2/scale}
	v.west, v.east = v.x0*180/math.Pi, (v.x0+float64(w)/scale)*180/math.Pi
	v.north, v.south = v.latAt(0), v.latAt(float64(h))
	return v
}

// picture is the image the JPEG encoder reads.
type picture struct {
	w, h     int
	gw, gh   int
	dens     []float32 // normalised 0..1 heat at 1/ds resolution
	line     []uint64  // overlay bit masks (full resolution)
	text     []uint64
	mark     []uint64
	bg       [3]float64
	lineCol  [3]float64
	textCol  [3]float64
	markCol  [3]float64
	lineA    float64
	stride64 int
	ramp     []ramp
	c        color.RGBA // the colour At returns (reused: see At)
}

func (p *picture) ColorModel() color.Model { return color.RGBAModel }
func (p *picture) Bounds() image.Rectangle { return image.Rect(0, 0, p.w, p.h) }

func (p *picture) bit(m []uint64, x, y int) bool {
	i := y*p.stride64*64 + x
	return m[i>>6]&(1<<(uint(i)&63)) != 0
}

func (p *picture) set(m []uint64, x, y int) {
	if x < 0 || y < 0 || x >= p.w || y >= p.h {
		return
	}
	i := y*p.stride64*64 + x
	m[i>>6] |= 1 << (uint(i) & 63)
}

// heat is the bilinear density at a full-resolution pixel.
func (p *picture) heat(x, y int) float64 {
	fx := (float64(x)+0.5)/ds - 0.5
	fy := (float64(y)+0.5)/ds - 0.5
	ix, iy := int(math.Floor(fx)), int(math.Floor(fy))
	tx, ty := fx-float64(ix), fy-float64(iy)
	get := func(i, j int) float64 {
		if i < 0 || j < 0 || i >= p.gw || j >= p.gh {
			return 0
		}
		return float64(p.dens[j*p.gw+i])
	}
	a := get(ix, iy)*(1-tx) + get(ix+1, iy)*tx
	b := get(ix, iy+1)*(1-tx) + get(ix+1, iy+1)*tx
	return a*(1-ty) + b*ty
}

func (p *picture) At(x, y int) color.Color {
	r, g, b := p.bg[0], p.bg[1], p.bg[2]
	if p.bit(p.line, x, y) {
		r, g, b = blend(r, g, b, p.lineCol, p.lineA)
	}
	if v := p.heat(x, y); v > 0 {
		hr, hg, hb, ha := rampAt(p.ramp, v)
		r, g, b = blend(r, g, b, [3]float64{hr, hg, hb}, ha)
	}
	if p.bit(p.mark, x, y) {
		r, g, b = p.markCol[0], p.markCol[1], p.markCol[2]
	}
	if p.bit(p.text, x, y) {
		r, g, b = p.textCol[0], p.textCol[1], p.textCol[2]
	}
	// The encoder reads each colour before it asks for the next pixel, so
	// one reused value avoids an allocation per pixel (33 million at 8K).
	p.c = color.RGBA{uint8(r + 0.5), uint8(g + 0.5), uint8(b + 0.5), 255}
	return &p.c
}

func blend(r, g, b float64, c [3]float64, a float64) (float64, float64, float64) {
	return r + (c[0]-r)*a, g + (c[1]-g)*a, b + (c[2]-b)*a
}

// Render draws the heat map and writes it as a JPEG.
func Render(w io.Writer, cells []Cell, o Options) error {
	if o.Width == 0 {
		o.Width = 7680
	}
	if o.Height == 0 {
		o.Height = 4320
	}
	if o.Width < minSide || o.Height < minSide/2 || o.Width > maxSide || o.Height > maxSide {
		return fmt.Errorf("heatexport: size %dx%d outside %d..%d", o.Width, o.Height, minSide, maxSide)
	}
	if o.Quality <= 0 || o.Quality > 100 {
		o.Quality = 90
	}
	if o.CellDeg <= 0 {
		o.CellDeg = 0.05
	}
	var maxC int64
	west, south, east, north := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, c := range cells {
		if c.Count <= 0 || !geo.ValidLatLon(c.Lat, c.Lon) {
			continue
		}
		maxC = max(maxC, c.Count)
		west, east = math.Min(west, c.Lon), math.Max(east, c.Lon)
		south, north = math.Min(south, c.Lat), math.Max(north, c.Lat)
	}
	if maxC == 0 {
		return ErrNoData
	}
	if o.RxOK {
		west, east = math.Min(west, o.RxLon), math.Max(east, o.RxLon)
		south, north = math.Min(south, o.RxLat), math.Max(north, o.RxLat)
	}
	if east-west > 180 { // antimeridian-crossing coverage is not handled: show it all
		west, east = -180, 180
	}
	v := fit(o.Width, o.Height, west, south, east, north)
	p := &picture{w: o.Width, h: o.Height, gw: (o.Width + ds - 1) / ds, gh: (o.Height + ds - 1) / ds, lineA: 0.35}
	p.stride64 = (o.Width + 63) / 64
	words := p.stride64 * o.Height
	p.line, p.text, p.mark = make([]uint64, words), make([]uint64, words), make([]uint64, words)
	p.dens = make([]float32, p.gw*p.gh)
	p.ramp = append([]ramp(nil), heatRamp...)
	if o.Light {
		p.bg, p.lineCol, p.textCol, p.markCol = [3]float64{244, 246, 250}, [3]float64{60, 80, 120}, [3]float64{15, 23, 42}, [3]float64{29, 78, 216}
		p.ramp[0] = ramp{0, 255, 255, 255, 0}
	} else {
		p.bg, p.lineCol, p.textCol, p.markCol = [3]float64{4, 10, 26}, [3]float64{120, 168, 255}, [3]float64{227, 237, 255}, [3]float64{255, 255, 255}
	}

	// Heat: a Gaussian splat per cell (log-scaled weight), normalised so
	// that the 99.5th percentile of the covered area is the top of the ramp.
	lmax := math.Log1p(float64(maxC))
	for _, c := range cells {
		if c.Count <= 0 || !geo.ValidLatLon(c.Lat, c.Lon) {
			continue
		}
		x, y := v.px(c.Lat, c.Lon)
		_, y2 := v.px(c.Lat+o.CellDeg, c.Lon)
		sig := math.Max(1.2, math.Abs(y-y2)/ds*0.6)
		gx, gy := x/ds, y/ds
		rad := int(math.Ceil(sig * 2.5))
		wt := math.Log1p(float64(c.Count)) / lmax
		ix, iy := int(gx), int(gy)
		for j := iy - rad; j <= iy+rad; j++ {
			if j < 0 || j >= p.gh {
				continue
			}
			for i := ix - rad; i <= ix+rad; i++ {
				if i < 0 || i >= p.gw {
					continue
				}
				dx, dy := float64(i)+0.5-gx, float64(j)+0.5-gy
				p.dens[j*p.gw+i] += float32(wt * math.Exp(-(dx*dx+dy*dy)/(2*sig*sig)))
			}
		}
	}
	var nz []float32
	for _, d := range p.dens {
		if d > 0.01 {
			nz = append(nz, d)
		}
	}
	if len(nz) > 0 {
		sort.Slice(nz, func(i, j int) bool { return nz[i] < nz[j] })
		top := float64(nz[min(len(nz)-1, int(float64(len(nz))*0.995))])
		for i, d := range p.dens {
			// gamma < 1 lifts the faint fringe of the coverage
			p.dens[i] = float32(math.Pow(math.Min(1, float64(d)/top), 0.7))
		}
	}

	th := max(1, o.Width/1920)
	// Graticule.
	step := niceStep(math.Max(v.east-v.west, v.north-v.south) / 10)
	for lon := math.Ceil(v.west/step) * step; lon <= v.east; lon += step {
		x, _ := v.px(0, lon)
		p.vline(p.line, int(x), th)
	}
	for lat := math.Ceil(v.south/step) * step; lat <= v.north; lat += step {
		_, y := v.px(lat, 0)
		p.hline(p.line, int(y), th)
	}
	scale := max(2, o.Width/640) // font pixel size
	// Range rings and the receiver.
	if o.RxOK {
		for _, km := range o.RingsKm {
			if km <= 0 {
				continue
			}
			var px0, py0 float64
			for a := 0; a <= 360; a++ {
				la, lo := geo.Destination(o.RxLat, o.RxLon, float64(a), float64(km))
				x, y := v.px(la, lo)
				if a > 0 {
					p.segment(p.mark, px0, py0, x, y, th)
				}
				px0, py0 = x, y
			}
			la, lo := geo.Destination(o.RxLat, o.RxLon, 0, float64(km))
			x, y := v.px(la, lo)
			p.text5x7(fmt.Sprintf("%d KM", km), int(x)+scale*2, int(y)+scale*2, scale)
		}
		x, y := v.px(o.RxLat, o.RxLon)
		p.disc(p.mark, int(x), int(y), 4*th)
	}
	// Caption, scale bar, attribution.
	m := scale * 6
	if o.Title != "" {
		p.text5x7(o.Title, m, m, scale*2)
	}
	if o.Caption != "" {
		p.text5x7(o.Caption, m, m+scale*2*9, scale)
	}
	p.text5x7(fmt.Sprintf("GRID %s°  ·  MADE WITH AIRVIEW", trimFloat(step)), m, o.Height-m-scale*7, scale)
	p.scaleBar(v, scale, th)
	return jpeg.Encode(w, p, &jpeg.Options{Quality: o.Quality})
}

// scaleBar draws a scale bar in the bottom right corner at the map's centre latitude.
func (p *picture) scaleBar(v *view, scale, th int) {
	midLat := v.latAt(float64(p.h) / 2)
	kmPerPx := 2 * math.Pi * 6371.0088 * math.Cos(midLat*math.Pi/180) / (2 * math.Pi * v.scale)
	km := niceStep(kmPerPx * float64(p.w) / 6)
	length := int(km / kmPerPx)
	m := scale * 6
	x1, y := p.w-m, p.h-m-scale*3
	x0 := x1 - length
	for t := 0; t < 2*th; t++ {
		for x := x0; x <= x1; x++ {
			p.set(p.text, x, y+t)
		}
	}
	for t := 0; t < scale*3; t++ {
		for k := 0; k < 2*th; k++ {
			p.set(p.text, x0+k, y-t)
			p.set(p.text, x1-k, y-t)
		}
	}
	p.text5x7(trimFloat(km)+" KM", x0, y-scale*12, scale)
}

func niceStep(x float64) float64 {
	if x <= 0 {
		return 1
	}
	e := math.Pow(10, math.Floor(math.Log10(x)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*e >= x {
			return m * e
		}
	}
	return 10 * e
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

func (p *picture) vline(m []uint64, x, th int) {
	for y := 0; y < p.h; y++ {
		for t := 0; t < th; t++ {
			p.set(m, x+t, y)
		}
	}
}

func (p *picture) hline(m []uint64, y, th int) {
	for x := 0; x < p.w; x++ {
		for t := 0; t < th; t++ {
			p.set(m, x, y+t)
		}
	}
}

func (p *picture) disc(m []uint64, cx, cy, r int) {
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r {
				p.set(m, cx+x, cy+y)
			}
		}
	}
}

// segment draws a line of thickness th (skipped when it leaves the picture far).
func (p *picture) segment(m []uint64, x0, y0, x1, y1 float64, th int) {
	if math.Max(x0, x1) < -10 || math.Min(x0, x1) > float64(p.w)+10 || math.Max(y0, y1) < -10 || math.Min(y0, y1) > float64(p.h)+10 {
		return
	}
	n := int(math.Ceil(math.Max(math.Abs(x1-x0), math.Abs(y1-y0)))) + 1
	if n > 4*maxSide {
		return
	}
	for i := 0; i <= n; i++ {
		f := float64(i) / float64(n)
		x, y := int(x0+(x1-x0)*f), int(y0+(y1-y0)*f)
		for a := 0; a < th; a++ {
			for b := 0; b < th; b++ {
				p.set(m, x+a, y+b)
			}
		}
	}
}

// text5x7 draws s with the bitmap font, each font pixel s x s picture pixels.
func (p *picture) text5x7(s string, x, y, sc int) {
	for _, r := range s {
		g := glyph(r)
		for row := 0; row < 7; row++ {
			for col := 0; col < 5; col++ {
				if g[row]&(0x10>>col) == 0 {
					continue
				}
				for a := 0; a < sc; a++ {
					for b := 0; b < sc; b++ {
						p.set(p.text, x+col*sc+a, y+row*sc+b)
					}
				}
			}
		}
		x += 6 * sc
	}
}
