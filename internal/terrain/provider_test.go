package terrain

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
)

// ---------------------------------------------------------------------------
// Terrarium encoding

func TestTerrariumKnownValues(t *testing.T) {
	// Mapzen/Tilezen: height = (R*256 + G + B/256) - 32768.
	cases := []struct {
		r, g, b uint8
		want    float64
	}{
		{128, 0, 0, 0},
		{0, 0, 0, -32768},
		{255, 255, 255, 32767 + 255.0/256},
		{128, 100, 128, 100.5},
		{131, 232, 0, 1000},     // 3*256+232 = 1000
		{162, 144, 64, 8848.25}, // Everest-ish: 34*256+144 = 8848
		{127, 255, 0, -1},
		{126, 0, 0, -512},
	}
	for _, c := range cases {
		near(t, "terrarium "+strconv.Itoa(int(c.r))+","+strconv.Itoa(int(c.g))+","+strconv.Itoa(int(c.b)),
			TerrariumElevation(c.r, c.g, c.b), c.want, 0)
		if c.want > -32768 && c.want < 32767 {
			r, g, b := TerrariumEncode(c.want)
			if r != c.r || g != c.g || b != c.b {
				t.Errorf("encode(%g) = %d,%d,%d want %d,%d,%d", c.want, r, g, b, c.r, c.g, c.b)
			}
		}
	}
	// Round trip within 1/512 m.
	for _, v := range []float64{-10994, -0.3, 0.001, 123.456, 4807.9, 8848.86} {
		r, g, b := TerrariumEncode(v)
		near(t, "roundtrip", TerrariumElevation(r, g, b), v, 1.0/512+1e-9)
	}
}

// makePNG builds a 256x256 PNG of the given image type with pixel colours
// from f(x, y).
func makePNG(t *testing.T, kind string, f func(x, y int) (uint8, uint8, uint8)) []byte {
	t.Helper()
	rect := image.Rect(0, 0, TileSize, TileSize)
	var img image.Image
	switch kind {
	case "rgba":
		m := image.NewRGBA(rect)
		for y := 0; y < TileSize; y++ {
			for x := 0; x < TileSize; x++ {
				r, g, b := f(x, y)
				m.SetRGBA(x, y, color.RGBA{r, g, b, 255})
			}
		}
		img = m
	case "nrgba":
		m := image.NewNRGBA(rect)
		for y := 0; y < TileSize; y++ {
			for x := 0; x < TileSize; x++ {
				r, g, b := f(x, y)
				m.SetNRGBA(x, y, color.NRGBA{r, g, b, 255})
			}
		}
		img = m
	case "paletted":
		pal := color.Palette{}
		idx := map[[3]uint8]uint8{}
		m := image.NewPaletted(rect, nil)
		for y := 0; y < TileSize; y++ {
			for x := 0; x < TileSize; x++ {
				r, g, b := f(x, y)
				k := [3]uint8{r, g, b}
				i, ok := idx[k]
				if !ok {
					if len(pal) >= 256 {
						t.Fatal("palette overflow")
					}
					i = uint8(len(pal))
					idx[k] = i
					pal = append(pal, color.RGBA{r, g, b, 255})
				}
				m.Pix[y*m.Stride+x] = i
			}
		}
		m.Palette = pal
		img = m
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecodeTerrariumKnownPixels(t *testing.T) {
	f := func(x, y int) (uint8, uint8, uint8) {
		switch {
		case x == 0 && y == 0:
			return 128, 0, 0 // 0 m
		case x == 255 && y == 0:
			return 131, 232, 0 // 1000 m
		case x == 0 && y == 255:
			return 127, 255, 0 // -1 m
		case x == 255 && y == 255:
			return 128, 100, 128 // 100.5 m
		case x == 17 && y == 42:
			return 162, 144, 64 // 8848.25 m
		}
		return 128, uint8(x % 8), 0 // small values, keeps the palette small
	}
	want := map[[2]int]float32{{0, 0}: 0, {255, 0}: 1000, {0, 255}: -1, {255, 255}: 100.5, {17, 42}: 8848.25, {5, 9}: 5}
	for _, kind := range []string{"rgba", "nrgba", "paletted"} {
		data := makePNG(t, kind, f)
		dst := make([]float32, TileSize*TileSize)
		if err := DecodeTerrarium(bytes.NewReader(data), dst); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		for p, w := range want {
			if got := dst[p[1]*TileSize+p[0]]; got != w {
				t.Errorf("%s pixel %v = %g, want %g", kind, p, got, w)
			}
		}
	}
	// Wrong size and garbage are rejected.
	small := image.NewRGBA(image.Rect(0, 0, 16, 16))
	var buf bytes.Buffer
	_ = png.Encode(&buf, small)
	if err := DecodeTerrarium(&buf, make([]float32, TileSize*TileSize)); !errors.Is(err, errTileSize) {
		t.Errorf("16x16 tile: %v", err)
	}
	if err := DecodeTerrarium(strings.NewReader("not a png"), make([]float32, TileSize*TileSize)); err == nil {
		t.Error("garbage decoded")
	}
	if err := DecodeTerrarium(bytes.NewReader(nil), make([]float32, 10)); err == nil {
		t.Error("small buffer accepted")
	}
}

func TestTileMath(t *testing.T) {
	// Slippy-map tile of Frankfurt (50.11 N, 8.68 E) at z10 is 536/346.
	if got := TileAt(50.11, 8.68, 10); got != (TileXY{10, 536, 346}) {
		t.Errorf("TileAt = %v", got)
	}
	s, w, n, e := TileBounds(TileXY{1, 0, 0})
	near(t, "z1 west", w, -180, 1e-9)
	near(t, "z1 east", e, 0, 1e-9)
	near(t, "z1 north", n, maxMercatorLat, 1e-9)
	near(t, "z1 south", s, 0, 1e-9)
	near(t, "mercator inverse", MercatorLat(MercatorY(47.3)), 47.3, 1e-9)
	// Pixel size at the equator, z0: 40075016.686/256 = 156543.03 m.
	near(t, "pixel z0", PixelSizeM(0, 0), 156543.0339, 1e-3)
	near(t, "pixel z10 @60", PixelSizeM(60, 10), 156543.0339/1024/2, 1e-3)

	// Zoom choice: the 450 km disc must stay in the tens of MB.
	cases := []struct {
		r    float64
		want int
	}{{450, 9}, {200, 10}, {100, 11}}
	for _, c := range cases {
		z := ChooseZoom(c.r, 50, 0, 0, 0)
		if z != c.want {
			t.Errorf("ChooseZoom(%g km) = %d, want %d", c.r, z, c.want)
		}
		n := len(TilesInRadius(50, 10, c.r, z))
		if n > 300 || n < 20 {
			t.Errorf("%g km at z%d: %d tiles", c.r, z, n)
		}
	}
	// TilesInRadius: nearest first, includes the centre tile, wraps at 180Ã‚Â°.
	ts := TilesInRadius(50, 10, 50, 10)
	if ts[0] != TileAt(50, 10, 10) {
		t.Errorf("first tile %v", ts[0])
	}
	wrap := TilesInRadius(0, 179.9, 100, 6)
	var east, west bool
	for _, tt := range wrap {
		if tt.X == 63 {
			east = true
		}
		if tt.X == 0 {
			west = true
		}
	}
	if !east || !west {
		t.Errorf("antimeridian wrap missing: %v", wrap)
	}
}

// ---------------------------------------------------------------------------
// Synthetic tile server

type tileServer struct {
	srv    *httptest.Server
	hits   atomic.Int64
	mu     sync.Mutex
	agents map[string]int
	// tile returns the elevations of a tile and the HTTP status.
	tile func(z, x, y int) ([]float32, int)
}

func newTileServer(t *testing.T, tile func(z, x, y int) ([]float32, int)) *tileServer {
	ts := &tileServer{tile: tile, agents: map[string]int{}}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.hits.Add(1)
		ts.mu.Lock()
		ts.agents[r.UserAgent()]++
		ts.mu.Unlock()
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".png"), "/")
		if len(parts) != 3 {
			http.NotFound(w, r)
			return
		}
		z, _ := strconv.Atoi(parts[0])
		x, _ := strconv.Atoi(parts[1])
		y, _ := strconv.Atoi(parts[2])
		elev, status := ts.tile(z, x, y)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		var buf bytes.Buffer
		if err := EncodeTerrarium(&buf, elev); err != nil {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tileServer) url() string { return ts.srv.URL + "/{z}/{x}/{y}.png" }

// gradientTiles: elevation = (gx - gx0) + 0.5*(gy - gy0) in global pixel
// coordinates of the zoom level (exactly bilinear), relative to an origin
// pixel so that values stay small.
func gradientTiles(gx0, gy0 int) func(z, x, y int) ([]float32, int) {
	return func(z, x, y int) ([]float32, int) {
		e := make([]float32, tilePixels)
		for py := 0; py < TileSize; py++ {
			for px := 0; px < TileSize; px++ {
				gx, gy := x*TileSize+px, y*TileSize+py
				e[py*TileSize+px] = float32(float64(gx-gx0) + 0.5*float64(gy-gy0))
			}
		}
		return e, http.StatusOK
	}
}

func TestProviderDownloadCacheAndBilinear(t *testing.T) {
	const z = 10
	lat, lon := 50.0, 10.0
	c := TileAt(lat, lon, z)
	gx0, gy0 := c.X*TileSize, c.Y*TileSize
	ts := newTileServer(t, gradientTiles(gx0, gy0))
	dir := t.TempDir()

	p := NewProvider(ProviderOptions{Dir: dir, URLTemplate: ts.url(), Zoom: z, Concurrency: 3})
	// Nothing cached yet: sea level, ok=false, and no network access.
	if e, ok := p.ElevationM(lat, lon); ok || e != 0 {
		t.Fatalf("uncached read = %g, %v", e, ok)
	}
	if ts.hits.Load() != 0 {
		t.Fatal("ElevationM touched the network")
	}
	var calls int
	pr, err := p.Prefetch(context.Background(), lat, lon, 20, z, func(pp PrefetchProgress) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if pr.Total == 0 || pr.Downloaded != pr.Total || pr.Failed != 0 || calls != pr.Total || pr.Bytes <= 0 {
		t.Fatalf("prefetch %+v (calls %d)", pr, calls)
	}
	if got := ts.hits.Load(); got != int64(pr.Total) {
		t.Errorf("server hits %d, tiles %d", got, pr.Total)
	}
	ts.mu.Lock()
	for ua := range ts.agents {
		if ua != model.AppName+"/"+model.Version || !strings.HasPrefix(ua, "AirView/") {
			t.Errorf("User-Agent %q", ua)
		}
	}
	ts.mu.Unlock()

	// Bilinear interpolation between pixel centres is exact for a planar field.
	n := float64(int(1)<<z) * TileSize
	check := func(la, lo float64) {
		t.Helper()
		gx := MercatorX(lo)*n - 0.5
		gy := MercatorY(la)*n - 0.5
		want := (gx - float64(gx0)) + 0.5*(gy-float64(gy0))
		got, ok := p.ElevationM(la, lo)
		if !ok {
			t.Fatalf("no data at %g,%g", la, lo)
		}
		near(t, "bilinear", got, math.Max(want, 0), 0.01)
	}
	check(lat, lon)
	// Points straddling the tile edges (pixel 255.7 Ã¢â€ â€™ neighbour tile).
	s, w, nn, e := TileBounds(c)
	check((s+nn)/2, w+1e-6)
	check((s+nn)/2, e-1e-6)
	check(nn-1e-6, (w+e)/2)
	check(s+1e-6, (w+e)/2)
	check(s+0.0123, w+0.0456)

	// A second provider on the same directory never re-downloads.
	hits := ts.hits.Load()
	p2 := NewProvider(ProviderOptions{Dir: dir, URLTemplate: ts.url(), Zoom: z})
	pr2, err := p2.Prefetch(context.Background(), lat, lon, 20, z, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pr2.Cached != pr2.Total || pr2.Downloaded != 0 || ts.hits.Load() != hits {
		t.Errorf("re-prefetch %+v, hits %d Ã¢â€ â€™ %d", pr2, hits, ts.hits.Load())
	}
	st := p2.Stats()
	if st.DiskTiles != pr.Total || st.DiskBytes != pr.Bytes {
		t.Errorf("stats %+v vs prefetch %+v", st, pr)
	}
	// And it can read the data offline.
	ts.srv.Close()
	p3 := NewProvider(ProviderOptions{Dir: dir, URLTemplate: ts.url(), Zoom: z, Offline: true})
	if v, ok := p3.ElevationM(lat, lon); !ok || math.IsNaN(v) {
		t.Errorf("offline cached read %g %v", v, ok)
	}
	// A lower-zoom-only cache still answers (fallback) for a higher zoom.
	if _, ok := p3.ElevationAtZoom(z+2, lat, lon); !ok {
		t.Error("no fallback from z+2 to cached z")
	}
	if _, ok := p3.ElevationAtZoom(z+5, lat, lon); ok {
		t.Error("fallback beyond 4 levels")
	}
}

func TestProviderOffline(t *testing.T) {
	ts := newTileServer(t, gradientTiles(0, 0))
	// No cache dir: always offline, never any request.
	p := NewProvider(ProviderOptions{URLTemplate: ts.url()})
	if !p.Offline() {
		t.Error("provider without dir should be offline")
	}
	if _, _, err := p.Fetch(context.Background(), TileXY{10, 1, 1}); !errors.Is(err, ErrOffline) {
		t.Errorf("fetch without dir: %v", err)
	}
	// Explicitly offline.
	p = NewProvider(ProviderOptions{Dir: t.TempDir(), URLTemplate: ts.url(), Offline: true})
	pr, err := p.Prefetch(context.Background(), 50, 10, 30, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Failed != pr.Total || !pr.Offline || pr.Downloaded != 0 {
		t.Errorf("offline prefetch %+v", pr)
	}
	if e, ok := p.ElevationM(50, 10); ok || e != 0 {
		t.Errorf("offline read %g %v", e, ok)
	}
	if ts.hits.Load() != 0 {
		t.Errorf("offline provider made %d requests", ts.hits.Load())
	}
	if p.Stats().MissingReads == 0 {
		t.Error("missing reads not counted")
	}
	// Back online.
	p.SetOffline(false)
	if res, _, err := p.Fetch(context.Background(), TileAt(50, 10, 10)); res != FetchDownloaded || err != nil {
		t.Errorf("online fetch %s %v", res, err)
	}
}

func TestProviderUnreachableBacksOff(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := NewProvider(ProviderOptions{Dir: t.TempDir(), URLTemplate: srv.URL + "/{z}/{x}/{y}.png", Concurrency: 1, RetryAfter: time.Hour})
	p.retryBase = time.Millisecond
	pr, err := p.Prefetch(context.Background(), 50, 10, 60, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Downloaded != 0 || pr.Failed != pr.Total {
		t.Errorf("prefetch %+v", pr)
	}
	if !p.Offline() || p.Stats().OfflineUntilMs == 0 {
		t.Error("provider did not switch to offline mode")
	}
	// 6 failing tiles Ãƒâ€” 3 attempts, then no more requests.
	if got := hits.Load(); got != offlineAfterErrs*3 {
		t.Errorf("%d requests, want %d", got, offlineAfterErrs*3)
	}
	p.ResetBackoff()
	if p.Offline() {
		t.Error("still offline after ResetBackoff")
	}
	// Nothing was cached: reads are sea level with ok=false.
	if e, ok := p.ElevationM(50, 10); ok || e != 0 {
		t.Errorf("read %g %v", e, ok)
	}
}

func TestProviderNoDataTile(t *testing.T) {
	ts := newTileServer(t, func(z, x, y int) ([]float32, int) { return nil, http.StatusNotFound })
	dir := t.TempDir()
	p := NewProvider(ProviderOptions{Dir: dir, URLTemplate: ts.url()})
	tt := TileAt(50, 10, 10)
	res, _, err := p.Fetch(context.Background(), tt)
	if res != FetchNoData || err != nil {
		t.Fatalf("404: %s %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "terrarium", "10", strconv.Itoa(tt.X), strconv.Itoa(tt.Y)+".none")); err != nil {
		t.Errorf("no-data marker: %v", err)
	}
	// Remembered: no second request, also by a new provider.
	h := ts.hits.Load()
	p2 := NewProvider(ProviderOptions{Dir: dir, URLTemplate: ts.url()})
	if res, _, _ := p2.Fetch(context.Background(), tt); res != FetchNoData || ts.hits.Load() != h {
		t.Errorf("re-fetch %s, hits %dÃ¢â€ â€™%d", res, h, ts.hits.Load())
	}
	if _, ok := p2.ElevationM(50, 10); ok {
		t.Error("no-data tile returned data")
	}
}

func TestProviderNegativeAndBathymetry(t *testing.T) {
	ts := newTileServer(t, func(z, x, y int) ([]float32, int) {
		e := make([]float32, tilePixels)
		for i := range e {
			e[i] = -42.5
		}
		return e, http.StatusOK
	})
	dir := t.TempDir()
	p := NewProvider(ProviderOptions{Dir: dir, URLTemplate: ts.url()})
	if _, _, err := p.Fetch(context.Background(), TileAt(50, 10, 10)); err != nil {
		t.Fatal(err)
	}
	if e, ok := p.ElevationM(50, 10); !ok || e != 0 {
		t.Errorf("sea floor clamped: %g %v", e, ok)
	}
	pb := NewProvider(ProviderOptions{Dir: dir, URLTemplate: ts.url(), KeepBathymetry: true})
	if e, ok := pb.ElevationM(50, 10); !ok || e != -42.5 {
		t.Errorf("bathymetry kept: %g %v", e, ok)
	}
}

func TestProviderCorruptTileRemoved(t *testing.T) {
	dir := t.TempDir()
	tt := TileAt(50, 10, 10)
	path := filepath.Join(dir, "terrarium", "10", strconv.Itoa(tt.X), strconv.Itoa(tt.Y)+".png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Leftover temp files are cleaned up by the index scan.
	if err := os.WriteFile(path+".tmp", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewProvider(ProviderOptions{Dir: dir, Offline: true})
	if _, ok := p.ElevationM(50, 10); ok {
		t.Error("corrupt tile returned data")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("corrupt tile not removed")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file not removed")
	}
	if p.HasTile(tt) {
		t.Error("corrupt tile still indexed")
	}
}

func noiseTiles(z, x, y int) ([]float32, int) {
	r := rand.New(rand.NewPCG(uint64(x), uint64(y)))
	e := make([]float32, tilePixels)
	for i := range e {
		e[i] = float32(r.Float64() * 4000)
	}
	return e, http.StatusOK
}

func TestProviderDiskCap(t *testing.T) {
	ts := newTileServer(t, noiseTiles)
	dir := t.TempDir()
	p := NewProvider(ProviderOptions{Dir: dir, URLTemplate: ts.url(), MaxDiskMB: 1})
	var tiles []TileXY
	for i := 0; i < 8; i++ {
		tt := TileXY{10, 500 + i, 300}
		tiles = append(tiles, tt)
		if res, _, err := p.Fetch(context.Background(), tt); res != FetchDownloaded || err != nil {
			t.Fatalf("fetch %v: %s %v", tt, res, err)
		}
		time.Sleep(2 * time.Millisecond) // distinct LRU stamps
	}
	before := p.Stats()
	if before.DiskBytes <= 1<<20 {
		t.Fatalf("noise tiles too small to test the cap: %d bytes", before.DiskBytes)
	}
	// Reading the oldest tile makes it recently used.
	la0, lo0 := tileCentre(tiles[0])
	if _, ok := p.ElevationAtZoom(10, la0, lo0); !ok {
		t.Fatal("tile 0 unreadable")
	}
	p.enforceCap(map[uint64]bool{tileKey(10, tiles[7].X, tiles[7].Y): true})
	after := p.Stats()
	if after.DiskBytes > (1<<20)*9/10 {
		t.Errorf("cache %d bytes after trim", after.DiskBytes)
	}
	if !p.HasTile(tiles[7]) || !p.HasTile(tiles[0]) {
		t.Error("protected / recently used tile deleted")
	}
	if p.HasTile(tiles[1]) {
		t.Error("least recently used tile kept")
	}
	// The index matches the files on disk.
	var files int
	var size int64
	_ = filepath.Walk(filepath.Join(dir, "terrarium"), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files++
			size += info.Size()
		}
		return nil
	})
	if files != after.DiskTiles || size != after.DiskBytes {
		t.Errorf("disk %d files/%d bytes, index %d/%d", files, size, after.DiskTiles, after.DiskBytes)
	}
}

func tileCentre(t TileXY) (float64, float64) {
	s, w, n, e := TileBounds(t)
	return (s + n) / 2, (w + e) / 2
}

func TestProviderMemoryLRU(t *testing.T) {
	ts := newTileServer(t, gradientTiles(500*TileSize, 300*TileSize))
	p := NewProvider(ProviderOptions{Dir: t.TempDir(), URLTemplate: ts.url(), MemTiles: 4})
	var tiles []TileXY
	for i := 0; i < 10; i++ {
		tt := TileXY{10, 500 + i, 300}
		tiles = append(tiles, tt)
		if _, _, err := p.Fetch(context.Background(), tt); err != nil {
			t.Fatal(err)
		}
	}
	for round := 0; round < 3; round++ {
		for _, tt := range tiles {
			la, lo := tileCentre(tt)
			if _, ok := p.ElevationAtZoom(10, la, lo); !ok {
				t.Fatalf("tile %v unreadable", tt)
			}
			if st := p.Stats(); st.MemTiles > 4 || st.MemBytes > 4*tilePixels*4 {
				t.Fatalf("memory cache %d tiles / %d bytes", st.MemTiles, st.MemBytes)
			}
		}
	}
	// A hot tile stays in memory (no disk load).
	la, lo := tileCentre(tiles[0])
	_, _ = p.ElevationAtZoom(10, la, lo)
	loads := p.Stats().DiskLoads
	for i := 0; i < 100; i++ {
		_, _ = p.ElevationAtZoom(10, la, lo)
	}
	if p.Stats().DiskLoads != loads {
		t.Error("hot tile reloaded from disk")
	}
	// Concurrent readers.
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				la, lo := tileCentre(tiles[(g+i)%len(tiles)])
				if _, ok := p.ElevationAtZoom(10, la, lo); !ok {
					t.Error("concurrent read failed")
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestProviderElevationNoAlloc(t *testing.T) {
	ts := newTileServer(t, gradientTiles(0, 0))
	p := NewProvider(ProviderOptions{Dir: t.TempDir(), URLTemplate: ts.url()})
	if _, _, err := p.Fetch(context.Background(), TileAt(50, 10, 10)); err != nil {
		t.Fatal(err)
	}
	_, _ = p.ElevationM(50, 10)
	if a := testing.AllocsPerRun(1000, func() { _, _ = p.ElevationM(50.0001, 10.0001) }); a != 0 {
		t.Errorf("ElevationM allocates %.1f", a)
	}
}

func TestProviderPrefetchCancel(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	defer close(block)
	p := NewProvider(ProviderOptions{Dir: t.TempDir(), URLTemplate: srv.URL + "/{z}/{x}/{y}.png"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := p.Prefetch(ctx, 50, 10, 100, 10, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v", err)
	}
	if time.Since(t0) > 5*time.Second {
		t.Error("cancellation too slow")
	}
}

func TestProviderPrefetchPath(t *testing.T) {
	ts := newTileServer(t, gradientTiles(0, 0))
	p := NewProvider(ProviderOptions{Dir: t.TempDir(), URLTemplate: ts.url()})
	pr, err := p.PrefetchPath(context.Background(), 50, 10, 50, 12, 10)
	if err != nil {
		t.Fatal(err)
	}
	// 2Ã‚Â° of longitude at z10 = 2/360*1024 Ã¢â€°Ë† 5.7 tiles Ã¢â€ â€™ 6 or 7 tiles on one row.
	if pr.Total < 6 || pr.Total > 8 || pr.Downloaded != pr.Total {
		t.Errorf("path prefetch %+v", pr)
	}
}

// TestProviderAsSource runs an LOS over a provider serving a synthetic ridge
// (rendered into Terrarium tiles) and checks it agrees with the analytic
// source.
func TestProviderAsSource(t *testing.T) {
	src := ridgeSource()
	ts := newTileServer(t, func(z, x, y int) ([]float32, int) {
		e := make([]float32, tilePixels)
		RenderTerrariumTile(src, TileXY{z, x, y}, e)
		return e, http.StatusOK
	})
	p := NewProvider(ProviderOptions{Dir: t.TempDir(), URLTemplate: ts.url(), Zoom: 12, MemTiles: 128})
	o := newOrigin(stLat, stLon)
	la, lo := o.destKm(0, 60)
	if _, err := p.PrefetchPath(context.Background(), stLat, stLon, la, lo, 12); err != nil {
		t.Fatal(err)
	}
	in := LOSInput{RxLat: stLat, RxLon: stLon, RxAGLM: 10, TgtLat: la, TgtLon: lo, TgtAltM: 1000, Samples: 1201}
	ra, rp := LOS(src, in), LOS(p, in)
	if ra.Clear != rp.Clear || rp.NoDataSamples != 0 {
		t.Fatalf("analytic clear=%v, tiles clear=%v nodata=%d", ra.Clear, rp.Clear, rp.NoDataSamples)
	}
	if ra.Obstruction != nil && rp.Obstruction != nil {
		near(t, "obstruction distance", rp.Obstruction.DistKm, ra.Obstruction.DistKm, 0.3)
	}
	near(t, "diffraction", rp.DiffractionDB, ra.DiffractionDB, 1.5)
}
