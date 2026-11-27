package terrain

import (
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
)

// TileSize is the edge length of a Terrarium tile in pixels.
const TileSize = 256

// maxMercatorLat is the latitude limit of the Web-Mercator tile pyramid.
const maxMercatorLat = 85.0511287798066

// earthCircumferenceM is the WGS84 equatorial circumference (Web Mercator).
const earthCircumferenceM = 40075016.68557849

// Source is anything that returns terrain elevation in metres above mean sea
// level for a WGS84 position. ok is false when no data is available for the
// point; the returned elevation is then 0 (sea level). Implementations must be
// safe for concurrent use.
type Source interface {
	ElevationM(lat, lon float64) (elevM float64, ok bool)
}

// Resolver is optionally implemented by a Source to report its horizontal
// resolution (metres per sample) near a latitude. Sampling steps are derived
// from it; sources without it are assumed to have 100 m resolution.
type Resolver interface {
	ResolutionM(lat float64) float64
}

// sourceResolution returns the resolution of src at lat (default 100 m).
func sourceResolution(src Source, lat float64) float64 {
	if r, ok := src.(Resolver); ok {
		if v := r.ResolutionM(lat); v > 0 && !math.IsInf(v, 0) {
			return v
		}
	}
	return 100
}

// TileXY is a Web-Mercator tile address.
type TileXY struct {
	Z int `json:"z"`
	X int `json:"x"`
	Y int `json:"y"`
}

func (t TileXY) String() string { return fmt.Sprintf("%d/%d/%d", t.Z, t.X, t.Y) }

// Valid reports whether the tile address exists in the pyramid.
func (t TileXY) Valid() bool {
	if t.Z < 0 || t.Z > 30 {
		return false
	}
	n := 1 << t.Z
	return t.X >= 0 && t.X < n && t.Y >= 0 && t.Y < n
}

// MercatorX returns the Web-Mercator x coordinate of lon in [0,1).
func MercatorX(lon float64) float64 {
	x := (lon + 180) / 360
	x -= math.Floor(x)
	return x
}

// MercatorY returns the Web-Mercator y coordinate of lat in [0,1]
// (0 = north edge). Latitudes are clamped to ±85.0511°.
func MercatorY(lat float64) float64 {
	if lat > maxMercatorLat {
		lat = maxMercatorLat
	} else if lat < -maxMercatorLat {
		lat = -maxMercatorLat
	}
	s := math.Sin(lat * math.Pi / 180)
	return 0.5 - math.Log((1+s)/(1-s))/(4*math.Pi)
}

// MercatorLat is the inverse of MercatorY.
func MercatorLat(y float64) float64 {
	return math.Atan(math.Sinh(math.Pi*(1-2*y))) * 180 / math.Pi
}

// TileAt returns the tile containing (lat, lon) at zoom z.
func TileAt(lat, lon float64, z int) TileXY {
	n := float64(int(1) << z)
	x := int(MercatorX(lon) * n)
	y := int(MercatorY(lat) * n)
	maxI := int(n) - 1
	if x > maxI {
		x = maxI
	}
	if y > maxI {
		y = maxI
	}
	if y < 0 {
		y = 0
	}
	return TileXY{Z: z, X: x, Y: y}
}

// TileBounds returns the WGS84 bounds of a tile.
func TileBounds(t TileXY) (south, west, north, east float64) {
	n := float64(int(1) << t.Z)
	west = float64(t.X)/n*360 - 180
	east = float64(t.X+1)/n*360 - 180
	north = MercatorLat(float64(t.Y) / n)
	south = MercatorLat(float64(t.Y+1) / n)
	return
}

// PixelSizeM returns the ground size of one Terrarium pixel in metres at
// latitude lat and zoom z.
func PixelSizeM(lat float64, z int) float64 {
	return earthCircumferenceM * math.Cos(lat*math.Pi/180) / (TileSize * math.Exp2(float64(z)))
}

// TileWidthKm returns the ground width of a tile in km at latitude lat.
func TileWidthKm(lat float64, z int) float64 { return PixelSizeM(lat, z) * TileSize / 1000 }

// ChooseZoom picks the highest zoom level in [minZoom, maxZoom] (defaults
// 8..12) at which a disc of radiusKm around latitude lat needs at most
// maxTiles tiles (default 300, about 30-45 MB of Terrarium PNGs). For a
// 450 km radius at 50° N this gives z9 (~290 tiles), 200 km gives z10,
// 100 km gives z11.
func ChooseZoom(radiusKm, lat float64, maxTiles, minZoom, maxZoom int) int {
	if maxTiles <= 0 {
		maxTiles = 300
	}
	if minZoom <= 0 {
		minZoom = 8
	}
	if maxZoom <= 0 || maxZoom > MaxTerrariumZoom {
		maxZoom = 12
	}
	if minZoom > maxZoom {
		minZoom = maxZoom
	}
	if !(radiusKm > 0) {
		return maxZoom
	}
	for z := maxZoom; z > minZoom; z-- {
		w := TileWidthKm(lat, z)
		if w <= 0 {
			continue
		}
		// Disc area over tile area, plus the ring of tiles cut by the edge:
		// skips the zoom levels that are clearly too fine before counting.
		est := math.Pi*radiusKm*radiusKm/(w*w) + 2*math.Pi*radiusKm/w
		if est > 4*float64(maxTiles) {
			continue
		}
		if len(TilesInRadius(lat, 0, radiusKm, z)) <= maxTiles {
			return z
		}
	}
	return minZoom
}

// TerrariumElevation decodes one Terrarium pixel to metres:
// (R·256 + G + B/256) − 32768.
func TerrariumElevation(r, g, b uint8) float64 {
	return float64(r)*256 + float64(g) + float64(b)/256 - 32768
}

// TerrariumEncode is the inverse of TerrariumElevation (used by tests and
// synthetic tile servers). Values are clamped to the representable range.
func TerrariumEncode(elevM float64) (r, g, b uint8) {
	v := elevM + 32768
	if v < 0 {
		v = 0
	}
	if v > 65535.996 {
		v = 65535.996
	}
	i := math.Floor(v)
	f := math.Round((v - i) * 256)
	if f >= 256 {
		f = 255
	}
	iv := int(i)
	return uint8(iv >> 8), uint8(iv & 0xff), uint8(f)
}

// errTileSize reports a PNG of unexpected dimensions.
var errTileSize = errors.New("terrain: terrarium tile is not 256x256")

// DecodeTerrarium decodes a Terrarium PNG into dst (row-major, 256×256
// float32, metres). dst must have length TileSize*TileSize.
func DecodeTerrarium(r io.Reader, dst []float32) error {
	if len(dst) < TileSize*TileSize {
		return errors.New("terrain: decode buffer too small")
	}
	img, err := png.Decode(r)
	if err != nil {
		return fmt.Errorf("terrain: decode terrarium png: %w", err)
	}
	b := img.Bounds()
	if b.Dx() != TileSize || b.Dy() != TileSize {
		return errTileSize
	}
	switch m := img.(type) {
	case *image.RGBA:
		for y := 0; y < TileSize; y++ {
			row := m.Pix[(y)*m.Stride:]
			o := y * TileSize
			for x := 0; x < TileSize; x++ {
				p := row[x*4 : x*4+3 : x*4+3]
				dst[o+x] = float32(TerrariumElevation(p[0], p[1], p[2]))
			}
		}
	case *image.NRGBA:
		for y := 0; y < TileSize; y++ {
			row := m.Pix[(y)*m.Stride:]
			o := y * TileSize
			for x := 0; x < TileSize; x++ {
				p := row[x*4 : x*4+3 : x*4+3]
				dst[o+x] = float32(TerrariumElevation(p[0], p[1], p[2]))
			}
		}
	default:
		for y := 0; y < TileSize; y++ {
			for x := 0; x < TileSize; x++ {
				cr, cg, cb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				dst[y*TileSize+x] = float32(TerrariumElevation(uint8(cr>>8), uint8(cg>>8), uint8(cb>>8)))
			}
		}
	}
	return nil
}

// EncodeTerrarium writes a 256×256 Terrarium PNG from row-major elevations
// (used by tests, the synthetic tile server and offline tools).
func EncodeTerrarium(w io.Writer, elev []float32) error {
	if len(elev) < TileSize*TileSize {
		return errors.New("terrain: encode buffer too small")
	}
	img := image.NewRGBA(image.Rect(0, 0, TileSize, TileSize))
	for i := 0; i < TileSize*TileSize; i++ {
		r, g, b := TerrariumEncode(float64(elev[i]))
		img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2], img.Pix[i*4+3] = r, g, b, 255
	}
	return png.Encode(w, img)
}

// RenderTerrariumTile renders the tile t of any Source as Terrarium
// elevations (pixel centres), e.g. to serve a synthetic terrain in tests.
func RenderTerrariumTile(src Source, t TileXY, dst []float32) {
	n := float64(int(1)<<t.Z) * TileSize
	for py := 0; py < TileSize; py++ {
		lat := MercatorLat((float64(t.Y*TileSize+py) + 0.5) / n)
		for px := 0; px < TileSize; px++ {
			lon := (float64(t.X*TileSize+px)+0.5)/n*360 - 180
			e, _ := src.ElevationM(lat, lon)
			dst[py*TileSize+px] = float32(e)
		}
	}
}
