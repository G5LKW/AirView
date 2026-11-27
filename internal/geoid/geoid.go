// Package geoid converts between heights above mean sea level (the geoid)
// and heights above the WGS-84 ellipsoid (HAE), which the MLAT engine works
// in (DESIGN §9.1, §9.6; receiver heights entered as MSL must become HAE).
//
//	h_HAE = h_MSL + N(lat, lon)
//
// N, the geoid undulation, comes from the EGM96 geoid model (NGA, public
// domain), embedded as a 1° global grid of 181×360 int16 values in
// centimetres (≈130 KB) and bilinearly interpolated. The 1° grid
// reproduces the full EGM96 model to typically < 0.5 m and to a few metres
// in the steepest regions (trenches, high mountains); for 1090 MHz MLAT
// that is far below other error sources, and the Grid type accepts a finer
// operator-supplied grid (Load / SetDefault) where needed.
//
// The embedded grid is produced by cmd/geoidgen from NGA's WW15MGH.GRD (or
// GeographicLib's egm96-15.pgm, which carries the same NGA values); see
// data/README.md. When the data file is absent from the build, Available
// reports false and the conversion functions report ok = false, so callers
// can fall back (DESIGN: treat the height as HAE and flag it).
package geoid

import (
	"bytes"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"sync"
	"sync/atomic"
)

// EmbeddedFile is the path of the embedded grid inside the data directory.
const EmbeddedFile = "data/egm96-1deg.avg"

//go:embed data
var dataFS embed.FS

// Errors.
var (
	ErrNoData = errors.New("geoid: no geoid grid is embedded in this build (see internal/geoid/data/README.md)")
	ErrFormat = errors.New("geoid: unrecognised or corrupt grid file")
)

// Grid is a regular latitude/longitude grid of geoid undulations. Rows run
// from north to south starting at Lat0; columns run east from Lon0. Values
// are stored as int16 v and decode to Offset + Scale·v metres. A Grid is
// immutable after construction and safe for concurrent use.
type Grid struct {
	NLat, NLon int
	Lat0, Lon0 float64 // north edge latitude and west edge longitude, degrees
	DLat, DLon float64 // spacing, degrees (positive)
	Scale      float64 // metres per unit
	Offset     float64 // metres
	Data       []int16 // NLat×NLon, row-major

	wrap bool // longitudes cover the full circle
}

// NewGrid validates the fields and finishes the grid.
func NewGrid(g Grid) (*Grid, error) {
	if g.NLat < 2 || g.NLon < 2 || !(g.DLat > 0) || !(g.DLon > 0) || !(g.Scale > 0) ||
		len(g.Data) != g.NLat*g.NLon || math.IsNaN(g.Lat0) || math.IsNaN(g.Lon0) || math.IsNaN(g.Offset) {
		return nil, fmt.Errorf("%w: bad grid geometry %d×%d", ErrFormat, g.NLat, g.NLon)
	}
	if g.Lat0-float64(g.NLat-1)*g.DLat < -90-1e-9 || g.Lat0 > 90+1e-9 {
		return nil, fmt.Errorf("%w: latitude range outside ±90°", ErrFormat)
	}
	g.wrap = math.Abs(float64(g.NLon)*g.DLon-360) < 1e-9
	return &g, nil
}

// Global reports whether the grid wraps around in longitude.
func (g *Grid) Global() bool { return g.wrap }

// value returns the undulation at node (i, j) in metres.
func (g *Grid) value(i, j int) float64 {
	return g.Offset + g.Scale*float64(g.Data[i*g.NLon+j])
}

// Undulation returns the geoid height N above the ellipsoid (metres) at a
// WGS-84 latitude/longitude in degrees, by bilinear interpolation. Points
// outside a regional grid are clamped to its edge. Any longitude is
// accepted; latitudes are clamped to ±90°.
func (g *Grid) Undulation(latDeg, lonDeg float64) float64 {
	if math.IsNaN(latDeg) || math.IsNaN(lonDeg) || math.IsInf(lonDeg, 0) {
		return math.NaN()
	}
	latDeg = math.Max(-90, math.Min(90, latDeg))
	y := (g.Lat0 - latDeg) / g.DLat
	maxY := float64(g.NLat - 1)
	if y < 0 {
		y = 0
	} else if y > maxY {
		y = maxY
	}
	i0 := int(y)
	if i0 >= g.NLat-1 {
		i0 = g.NLat - 2
	}
	fy := y - float64(i0)

	x := (lonDeg - g.Lon0) / g.DLon
	var j0, j1 int
	var fx float64
	if g.wrap {
		n := float64(g.NLon)
		x = math.Mod(x, n)
		if x < 0 {
			x += n
		}
		j0 = int(x)
		if j0 >= g.NLon {
			j0 = 0
		}
		fx = x - float64(j0)
		j1 = j0 + 1
		if j1 == g.NLon {
			j1 = 0
		}
	} else {
		// Regional grid: bring the longitude into [Lon0, Lon0+360), then
		// clamp to the nearer edge if it falls outside.
		maxX := float64(g.NLon - 1)
		full := 360 / g.DLon
		x = math.Mod(x, full)
		if x < 0 {
			x += full
		}
		if x > maxX {
			if x-maxX < full-x {
				x = maxX
			} else {
				x = 0
			}
		}
		j0 = int(x)
		if j0 >= g.NLon-1 {
			j0 = g.NLon - 2
		}
		fx = x - float64(j0)
		j1 = j0 + 1
	}
	v00, v01 := g.value(i0, j0), g.value(i0, j1)
	v10, v11 := g.value(i0+1, j0), g.value(i0+1, j1)
	return (1-fy)*((1-fx)*v00+fx*v01) + fy*((1-fx)*v10+fx*v11)
}

// Resample returns the sub-grid of every step-th node in both directions
// (node values are copied exactly, so a 15′ grid resampled with step 4
// gives the exact 1° nodes).
func (g *Grid) Resample(step int) (*Grid, error) {
	if step < 1 {
		return nil, fmt.Errorf("geoid: bad resample step %d", step)
	}
	if (g.NLat-1)%step != 0 || (g.wrap && g.NLon%step != 0) || (!g.wrap && (g.NLon-1)%step != 0) {
		return nil, fmt.Errorf("geoid: %d×%d grid cannot be resampled by %d", g.NLat, g.NLon, step)
	}
	nlat := (g.NLat-1)/step + 1
	nlon := g.NLon / step
	if !g.wrap {
		nlon = (g.NLon-1)/step + 1
	}
	out := Grid{NLat: nlat, NLon: nlon, Lat0: g.Lat0, Lon0: g.Lon0, DLat: g.DLat * float64(step),
		DLon: g.DLon * float64(step), Scale: g.Scale, Offset: g.Offset, Data: make([]int16, nlat*nlon)}
	for i := 0; i < nlat; i++ {
		for j := 0; j < nlon; j++ {
			out.Data[i*nlon+j] = g.Data[i*step*g.NLon+j*step]
		}
	}
	return NewGrid(out)
}

// Binary format ("AVGEOID1"), all little-endian:
//
//	[8]byte  magic "AVGEOID1"
//	int32    NLat, NLon
//	float64  Lat0, Lon0, DLat, DLon, Scale, Offset
//	uint32   CRC-32 (IEEE) of the data bytes
//	int16    Data[NLat·NLon]
const (
	magic     = "AVGEOID1"
	headerLen = 8 + 4 + 4 + 6*8 + 4
)

// MarshalBinary encodes the grid in the AVGEOID1 format.
func (g *Grid) MarshalBinary() ([]byte, error) {
	data := make([]byte, 2*len(g.Data))
	for i, v := range g.Data {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(v))
	}
	var b bytes.Buffer
	b.Grow(headerLen + len(data))
	b.WriteString(magic)
	le := binary.LittleEndian
	_ = binary.Write(&b, le, int32(g.NLat))
	_ = binary.Write(&b, le, int32(g.NLon))
	for _, f := range []float64{g.Lat0, g.Lon0, g.DLat, g.DLon, g.Scale, g.Offset} {
		_ = binary.Write(&b, le, f)
	}
	_ = binary.Write(&b, le, crc32.ChecksumIEEE(data))
	b.Write(data)
	return b.Bytes(), nil
}

// ParseBinary decodes an AVGEOID1 grid.
func ParseBinary(b []byte) (*Grid, error) {
	if len(b) < headerLen || string(b[:8]) != magic {
		return nil, ErrFormat
	}
	le := binary.LittleEndian
	g := Grid{
		NLat: int(int32(le.Uint32(b[8:]))),
		NLon: int(int32(le.Uint32(b[12:]))),
	}
	f := func(off int) float64 { return math.Float64frombits(le.Uint64(b[off:])) }
	g.Lat0, g.Lon0, g.DLat, g.DLon, g.Scale, g.Offset = f(16), f(24), f(32), f(40), f(48), f(56)
	crc := le.Uint32(b[64:])
	if g.NLat <= 0 || g.NLon <= 0 || g.NLat > 1<<14 || g.NLon > 1<<15 {
		return nil, ErrFormat
	}
	data := b[headerLen:]
	if len(data) != 2*g.NLat*g.NLon {
		return nil, fmt.Errorf("%w: %d data bytes for %d×%d", ErrFormat, len(data), g.NLat, g.NLon)
	}
	if crc32.ChecksumIEEE(data) != crc {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrFormat)
	}
	g.Data = make([]int16, g.NLat*g.NLon)
	for i := range g.Data {
		g.Data[i] = int16(le.Uint16(data[2*i:]))
	}
	return NewGrid(g)
}

// Parse decodes a grid in any supported format: AVGEOID1, NGA ASCII
// (WW15MGH.GRD layout) or GeographicLib PGM.
func Parse(b []byte) (*Grid, error) {
	switch {
	case bytes.HasPrefix(b, []byte(magic)):
		return ParseBinary(b)
	case bytes.HasPrefix(b, []byte("P5")):
		return ParsePGM(bytes.NewReader(b))
	default:
		return ParseNGA(bytes.NewReader(b))
	}
}

// Load reads and parses a grid file (any supported format).
func Load(path string) (*Grid, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

var (
	override atomic.Pointer[Grid] // set by SetDefault
	loadOnce sync.Once
	embedded *Grid
	loadErr  error
)

func loadEmbedded() {
	b, err := dataFS.ReadFile(EmbeddedFile)
	if err != nil {
		loadErr = ErrNoData
		return
	}
	embedded, err = ParseBinary(b)
	if err != nil {
		loadErr = fmt.Errorf("geoid: embedded grid: %w", err)
	}
}

// Default returns the grid used by the package-level functions: the one
// set with SetDefault, else the embedded EGM96 1° grid (decoded once, on
// first use).
func Default() (*Grid, error) {
	if g := override.Load(); g != nil {
		return g, nil
	}
	loadOnce.Do(loadEmbedded)
	return embedded, loadErr
}

// SetDefault replaces the grid used by the package-level functions (for
// example a finer grid named in the configuration). nil restores the
// embedded grid. Safe for concurrent use.
func SetDefault(g *Grid) { override.Store(g) }

// Available reports whether a geoid grid is available.
func Available() bool {
	_, err := Default()
	return err == nil
}

// Undulation returns N (metres, geoid above ellipsoid) from the default
// grid; ok is false when no grid is available.
func Undulation(latDeg, lonDeg float64) (n float64, ok bool) {
	g, err := Default()
	if err != nil {
		return 0, false
	}
	return g.Undulation(latDeg, lonDeg), true
}

// MSLToHAE converts a height above mean sea level to a height above the
// WGS-84 ellipsoid. With no grid it returns hMSL unchanged and ok = false.
func MSLToHAE(latDeg, lonDeg, hMSL float64) (hHAE float64, ok bool) {
	n, ok := Undulation(latDeg, lonDeg)
	return hMSL + n, ok
}

// HAEToMSL converts a height above the ellipsoid to a height above mean
// sea level. With no grid it returns hHAE unchanged and ok = false.
func HAEToMSL(latDeg, lonDeg, hHAE float64) (hMSL float64, ok bool) {
	n, ok := Undulation(latDeg, lonDeg)
	return hHAE - n, ok
}
