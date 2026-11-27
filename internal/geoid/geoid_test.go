package geoid

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// field is a smooth synthetic undulation field (metres).
func field(lat, lon float64) float64 {
	return 30*math.Sin(lat*math.Pi/90) + 20*math.Cos(lon*math.Pi/180) + 5*math.Sin(lon*math.Pi/45)*math.Cos(lat*math.Pi/180)
}

// synthGrid samples field on a global grid of the given spacing.
func synthGrid(t testing.TB, step float64) *Grid {
	nlat := int(math.Round(180/step)) + 1
	nlon := int(math.Round(360 / step))
	data := make([]int16, nlat*nlon)
	for i := 0; i < nlat; i++ {
		for j := 0; j < nlon; j++ {
			c, err := toCM(field(90-float64(i)*step, float64(j)*step))
			if err != nil {
				t.Fatal(err)
			}
			data[i*nlon+j] = c
		}
	}
	g, err := NewGrid(Grid{NLat: nlat, NLon: nlon, Lat0: 90, Lon0: 0, DLat: step, DLon: step, Scale: cmScale, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestBilinear(t *testing.T) {
	g := synthGrid(t, 1)
	if !g.Global() {
		t.Fatal("global")
	}
	// Exact at nodes (to the 1 cm storage resolution).
	for _, p := range [][2]float64{{0, 0}, {45, 10}, {-30, 359}, {90, 0}, {-90, 123}, {12, 180}} {
		if d := g.Undulation(p[0], p[1]) - field(p[0], p[1]); math.Abs(d) > 0.005+1e-9 {
			t.Errorf("node %v: off by %v", p, d)
		}
	}
	// Bilinear between nodes: the midpoint of a cell is the mean of its
	// four corners.
	mid := g.Undulation(10.5, 20.5)
	want := (g.Undulation(10, 20) + g.Undulation(10, 21) + g.Undulation(11, 20) + g.Undulation(11, 21)) / 4
	if math.Abs(mid-want) > 1e-9 {
		t.Errorf("cell centre %v, want %v", mid, want)
	}
	// Linear along an edge.
	a, b := g.Undulation(10, 20), g.Undulation(10, 21)
	if v := g.Undulation(10, 20.25); math.Abs(v-(0.75*a+0.25*b)) > 1e-9 {
		t.Errorf("edge interpolation %v", v)
	}
	// Close to the smooth field everywhere.
	var worst float64
	for lat := -89.7; lat < 90; lat += 3.1 {
		for lon := -180.0; lon < 360; lon += 7.3 {
			worst = math.Max(worst, math.Abs(g.Undulation(lat, lon)-field(lat, lon)))
		}
	}
	if worst > 0.1 {
		t.Errorf("worst interpolation error %v m", worst)
	}
}

func TestLongitudeWrap(t *testing.T) {
	g := synthGrid(t, 1)
	for _, lon := range []float64{0.3, 120, 359.7} {
		v := g.Undulation(33.3, lon)
		for _, alt := range []float64{lon - 360, lon + 360, lon - 720} {
			if w := g.Undulation(33.3, alt); math.Abs(w-v) > 1e-9 {
				t.Errorf("lon %v vs %v: %v vs %v", lon, alt, v, w)
			}
		}
	}
	// Across the 0/360 seam the interpolation uses columns 359 and 0.
	a, b := g.Undulation(20, 359), g.Undulation(20, 0)
	if v := g.Undulation(20, 359.5); math.Abs(v-(a+b)/2) > 1e-9 {
		t.Errorf("seam %v, want %v", v, (a+b)/2)
	}
	// Latitude clamped beyond the poles; NaN propagates.
	if g.Undulation(95, 10) != g.Undulation(90, 10) {
		t.Error("lat clamp")
	}
	if !math.IsNaN(g.Undulation(math.NaN(), 0)) {
		t.Error("NaN")
	}
}

func TestRegionalGrid(t *testing.T) {
	// 3×3 grid over 50..52°N, -1..1°E with values = 100·row + col.
	data := []int16{0, 1, 2, 100, 101, 102, 200, 201, 202}
	g, err := NewGrid(Grid{NLat: 3, NLon: 3, Lat0: 52, Lon0: -1, DLat: 1, DLon: 1, Scale: 1, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if g.Global() {
		t.Fatal("regional grid reported global")
	}
	if v := g.Undulation(51.5, 0.5); math.Abs(v-51.5) > 1e-9 {
		t.Errorf("inside %v", v)
	}
	if v := g.Undulation(51, 359.5); math.Abs(v-100.5) > 1e-9 { // = -0.5°E
		t.Errorf("wrapped longitude %v", v)
	}
	if v := g.Undulation(60, 5); v != 2 { // clamped to the NE corner
		t.Errorf("clamp NE %v", v)
	}
	if v := g.Undulation(40, -5); v != 200 { // clamped to the SW corner
		t.Errorf("clamp SW %v", v)
	}
	if _, err := NewGrid(Grid{NLat: 1, NLon: 3, DLat: 1, DLon: 1, Scale: 1, Data: []int16{1, 2, 3}}); err == nil {
		t.Error("1-row grid accepted")
	}
	if _, err := NewGrid(Grid{NLat: 2, NLon: 2, Lat0: 90, DLat: 1, DLon: 1, Scale: 1, Data: []int16{1}}); err == nil {
		t.Error("short data accepted")
	}
}

func TestBinaryRoundTrip(t *testing.T) {
	g := synthGrid(t, 1)
	b, err := g.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != headerLen+2*181*360 {
		t.Errorf("1° grid is %d bytes", len(b))
	}
	h, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if h.NLat != g.NLat || h.NLon != g.NLon || h.DLat != g.DLat || !h.Global() {
		t.Fatalf("header mismatch %+v", h)
	}
	for i := range g.Data {
		if g.Data[i] != h.Data[i] {
			t.Fatal("data mismatch")
		}
	}
	// Corruption is detected.
	c := append([]byte(nil), b...)
	c[len(c)-1] ^= 1
	if _, err := ParseBinary(c); !errors.Is(err, ErrFormat) {
		t.Errorf("corrupt data: %v", err)
	}
	if _, err := ParseBinary(b[:len(b)-2]); !errors.Is(err, ErrFormat) {
		t.Errorf("truncated: %v", err)
	}
	if _, err := ParseBinary([]byte("nonsense")); !errors.Is(err, ErrFormat) {
		t.Errorf("garbage: %v", err)
	}
}

// ngaText renders a grid in the NGA ASCII layout (with the duplicated
// 360° column, eight values per line like WW15MGH.GRD).
func ngaText(step float64) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, " %f %f %f %f %f %f\n\n", -90.0, 90.0, 0.0, 360.0, step, step)
	nlat := int(math.Round(180/step)) + 1
	nlon := int(math.Round(360/step)) + 1
	for i := 0; i < nlat; i++ {
		for j := 0; j < nlon; j++ {
			fmt.Fprintf(&sb, " %9.3f", field(90-float64(i)*step, float64(j)*step))
			if j%8 == 7 {
				sb.WriteByte('\n')
			}
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}

func TestParseNGAAndResample(t *testing.T) {
	src, err := Parse([]byte(ngaText(0.25)))
	if err != nil {
		t.Fatal(err)
	}
	if src.NLat != 721 || src.NLon != 1440 || !src.Global() {
		t.Fatalf("15' grid parsed as %d×%d", src.NLat, src.NLon)
	}
	g, err := src.Resample(4)
	if err != nil {
		t.Fatal(err)
	}
	if g.NLat != 181 || g.NLon != 360 || g.DLat != 1 {
		t.Fatalf("resampled to %d×%d @ %v", g.NLat, g.NLon, g.DLat)
	}
	// Resampled nodes are exact source nodes.
	for _, p := range [][2]float64{{0, 0}, {37, 211}, {-89, 359}} {
		if g.Undulation(p[0], p[1]) != src.Undulation(p[0], p[1]) {
			t.Errorf("node %v differs after resampling", p)
		}
	}
	if _, err := src.Resample(7); err == nil {
		t.Error("non-dividing step accepted")
	}
	// Malformed inputs.
	for _, bad := range []string{"", "1 2 3", "-90 90 0 360 1 1\n1 2 3", "-90 90 0 360 0 1\n", "-90 90 0 360 90 180\n1 2 3 4 5 6 x"} {
		if _, err := ParseNGA(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// Trailing data is rejected.
	if _, err := ParseNGA(strings.NewReader("-90 90 0 360 90 180\n1 2 3 4 5 6 7 8 9 10")); err == nil {
		t.Error("trailing data accepted")
	}
	// A minimal valid grid (3 lat × 3 lon incl. duplicate column -> 3×2).
	g2, err := ParseNGA(strings.NewReader("-90 90 0 360 90 180\n1 2 1  3 4 3  5 6 5"))
	if err != nil {
		t.Fatal(err)
	}
	if g2.NLat != 3 || g2.NLon != 2 || g2.Undulation(90, 0) != 1 || g2.Undulation(0, 180) != 4 {
		t.Errorf("tiny grid %+v", g2)
	}
}

func TestParsePGM(t *testing.T) {
	const w, h = 8, 5 // 45° × 45°
	var b bytes.Buffer
	fmt.Fprintf(&b, "P5\n# Geoid file in PGM format\n# Offset -108\n# Scale 0.003\n%d %d\n65535\n", w, h)
	want := make([]float64, w*h)
	for i := 0; i < h; i++ {
		for j := 0; j < w; j++ {
			raw := uint16((i*w + j) * 1000)
			want[i*w+j] = -108 + 0.003*float64(raw)
			_ = binary.Write(&b, binary.BigEndian, raw)
		}
	}
	g, err := Parse(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if g.NLat != h || g.NLon != w || g.DLat != 45 || g.DLon != 45 || !g.Global() {
		t.Fatalf("PGM grid %+v", g)
	}
	if v := g.Undulation(45, 90); math.Abs(v-want[1*w+2]) > 0.005 {
		t.Errorf("PGM node %v want %v", v, want[1*w+2])
	}
	if _, err := ParsePGM(strings.NewReader("P5\n8 5\n65535\n")); err == nil {
		t.Error("PGM without Offset/Scale accepted")
	}
	if _, err := ParsePGM(strings.NewReader("P5\n# Offset -108\n# Scale 0.003\n8 5\n65535\nshort")); err == nil {
		t.Error("short PGM accepted")
	}
}

func TestPackageFunctions(t *testing.T) {
	defer SetDefault(nil)
	g := synthGrid(t, 1)
	SetDefault(g)
	if !Available() {
		t.Fatal("not available after SetDefault")
	}
	n, ok := Undulation(10, 20)
	if !ok || n != g.Undulation(10, 20) {
		t.Fatal("Undulation")
	}
	hae, ok := MSLToHAE(10, 20, 100)
	if !ok || math.Abs(hae-(100+n)) > 1e-12 {
		t.Error("MSLToHAE")
	}
	msl, ok := HAEToMSL(10, 20, hae)
	if !ok || math.Abs(msl-100) > 1e-12 {
		t.Error("HAEToMSL")
	}
	SetDefault(nil)
	_, err := Default()
	if err == nil {
		t.Log("embedded EGM96 grid present")
	} else if !errors.Is(err, ErrNoData) {
		t.Fatalf("embedded grid failed to load: %v", err)
	} else {
		if _, ok := MSLToHAE(10, 20, 100); ok {
			t.Error("ok without data")
		}
		if h, _ := MSLToHAE(10, 20, 100); h != 100 {
			t.Error("MSLToHAE without data must return the input")
		}
	}
}

// TestEGM96Reference is the acceptance check of DESIGN §9.1: the embedded
// grid must reproduce NGA's published EGM96 test points within 2 m. It is
// skipped while the grid is not part of the build (see data/README.md).
func TestEGM96Reference(t *testing.T) {
	SetDefault(nil)
	g, err := Default()
	if err != nil {
		t.Skipf("embedded EGM96 grid not present: %v", err)
	}
	if g.NLat != 181 || g.NLon != 360 || g.DLat != 1 || !g.Global() {
		t.Errorf("embedded grid is %d×%d @ %v°", g.NLat, g.NLon, g.DLat)
	}
	for _, p := range NGATestPoints {
		got := g.Undulation(p.Lat, p.Lon)
		t.Logf("%10.5f %11.5f: published %8.3f m, grid %8.3f m (Δ %+.3f)", p.Lat, p.Lon, p.N, got, got-p.N)
		if math.Abs(got-p.N) > 2 {
			t.Errorf("%+v: grid gives %.3f m (> 2 m off)", p, got)
		}
	}
	for _, p := range Extremes {
		if got := g.Undulation(p.Lat, p.Lon); math.Abs(got-p.N) > 5 {
			t.Errorf("extreme %+v: grid gives %.2f m", p, got)
		}
	}
	// UK sanity (RESEARCH: the UK geoid is about +45…+55 m).
	if n := g.Undulation(52, -1); n < 44 || n > 56 {
		t.Errorf("UK undulation %.1f m", n)
	}
}

func BenchmarkUndulation(b *testing.B) {
	g := synthGrid(b, 1)
	b.ReportAllocs()
	var s float64
	for i := 0; i < b.N; i++ {
		s += g.Undulation(51.5+float64(i&31)*0.01, -0.12)
	}
	_ = s
}
