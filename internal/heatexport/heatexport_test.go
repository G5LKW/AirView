package heatexport

import (
	"bytes"
	"image/jpeg"
	"math"
	"runtime"
	"testing"
	"time"
)

// cells is a synthetic coverage: a disc of 400 km around 50N 8E with the
// count falling off with distance.
func cells() []Cell {
	var out []Cell
	for lat := 46.0; lat <= 54; lat += 0.05 {
		for lon := 2.0; lon <= 14; lon += 0.05 {
			d := math.Hypot(lat-50, (lon-8)*math.Cos(50*math.Pi/180)) * 111
			if d > 400 {
				continue
			}
			out = append(out, Cell{Lat: lat, Lon: lon, Count: int64(1 + 5000*math.Exp(-d/80))})
		}
	}
	return out
}

func TestRenderSmall(t *testing.T) {
	var buf bytes.Buffer
	err := Render(&buf, cells(), Options{Width: 1280, Height: 720, RxOK: true, RxLat: 50, RxLon: 8, RingsKm: []int{100, 200},
		Title: "AirView test", Caption: "1234 positions"})
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1280 || b.Dy() != 720 {
		t.Fatalf("bounds %v", b)
	}
	// Near the receiver the heat is hot (red/white), far away it is background.
	r, g, b, _ := img.At(660, 360).RGBA()
	if r>>8 < 150 {
		t.Errorf("centre pixel %d %d %d is not hot", r>>8, g>>8, b>>8)
	}
	r, g, b, _ = img.At(5, 360).RGBA()
	if r>>8 > 40 || g>>8 > 60 {
		t.Errorf("edge pixel %d %d %d is not background", r>>8, g>>8, b>>8)
	}
	if err := Render(&buf, nil, Options{}); err != ErrNoData {
		t.Fatalf("no data: %v", err)
	}
	if err := Render(&buf, cells(), Options{Width: 100, Height: 100}); err == nil {
		t.Fatal("tiny size accepted")
	}
	buf.Reset()
	if err := Render(&buf, cells(), Options{Width: 1280, Height: 720, Light: true}); err != nil {
		t.Fatal(err)
	}
}

// The 8K export stays small in memory (no full-resolution RGBA buffer).
func TestRender8KMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("8K encode takes a few seconds")
	}
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	var n countWriter
	t0 := time.Now()
	if err := Render(&n, cells(), Options{RxOK: true, RxLat: 50, RxLon: 8, RingsKm: []int{100, 200, 300, 400}, Title: "AirView"}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m1)
	alloc := (m1.TotalAlloc - m0.TotalAlloc) >> 20
	t.Logf("8K: %d kB JPEG in %v, %d MB allocated", n/1024, time.Since(t0).Round(time.Millisecond), alloc)
	// An RGBA buffer alone would be 126 MB.
	if alloc > 60 {
		t.Errorf("allocated %d MB", alloc)
	}
	if n < 100_000 {
		t.Errorf("JPEG only %d bytes", n)
	}
}

type countWriter int

func (c *countWriter) Write(b []byte) (int, error) { *c += countWriter(len(b)); return len(b), nil }
