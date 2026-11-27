package terrain

import (
	"context"
	"net/http"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestProviderMemCap(t *testing.T) {
	p := NewProvider(ProviderOptions{MemTiles: 10000})
	if want := DefaultMaxMemMB << 20 / tileMemBytes; p.opt.MemTiles != want {
		t.Errorf("MemTiles %d, want capped to %d", p.opt.MemTiles, want)
	}
	p = NewProvider(ProviderOptions{MemTiles: 500, MaxMemMB: 8})
	if p.opt.MemTiles != 32 {
		t.Errorf("MemTiles %d with 8 MiB cap, want 32", p.opt.MemTiles)
	}
	if p := NewProvider(ProviderOptions{}); p.opt.MemTiles != 64 {
		t.Errorf("default MemTiles %d", p.opt.MemTiles)
	}
}

func heapInuseMB() float64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapInuse) / (1 << 20)
}

// TestAnalysisHeapRelease runs a synthetic tile-backed analysis twice: once
// keeping every decoded tile after the job (the behaviour before
// KeepMemTiles / TrimMemory existed) and once with the default release, and
// reports the live heap after each. The job hook fires after the release.
func TestAnalysisHeapRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("memory measurement")
	}
	o := newOrigin(stLat, stLon)
	cLa, cLo := o.destKm(90, 20)
	cone := ConeSource{Lat: cLa, Lon: cLo, PeakM: 1200, BaseM: 100, RadiusKm: 6}
	ts := newTileServer(t, func(z, x, y int) ([]float32, int) {
		e := make([]float32, tilePixels)
		RenderTerrariumTile(cone, TileXY{Z: z, X: x, Y: y}, e)
		return e, http.StatusOK
	})
	dir := t.TempDir()
	tiles := filepath.Join(dir, "tiles")
	req := testRequest()
	req.RadiusKm, req.Zoom = 120, 10

	// run analyses once with its own provider and service, measures the
	// live heap while the result is still held, then stops the service.
	run := func(keep int, offline bool) (heapMB float64, memTiles int, hook bool) {
		prov := NewProvider(ProviderOptions{Dir: tiles, URLTemplate: ts.url(), Offline: offline})
		var called atomic.Bool
		s := NewService(ServiceOptions{Provider: prov, KeepMemTiles: keep, OnJobDone: func(JobStatus) { called.Store(true) }})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { _ = s.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		s.Submit(req, false)
		if st := waitJob(t, s, req.Key()); st.State != StateDone {
			t.Fatalf("job %+v", st)
		}
		heapMB = heapInuseMB()
		memTiles = prov.Stats().MemTiles
		runtime.KeepAlive(s.Analysis())
		return heapMB, memTiles, called.Load()
	}
	run(0, false) // warm the disk cache so both measured runs only decode from disk
	base := heapInuseMB()
	keptMB, keptTiles, _ := run(1<<20, true) // old behaviour: everything stays cached
	relMB, relTiles, hook := run(0, true)    // default: released after the job
	t.Logf("heap in use: baseline %.1f MiB, after analysis keeping tiles %.1f MiB (%d tiles), after analysis with release %.1f MiB (%d tiles)",
		base, keptMB, keptTiles, relMB, relTiles)
	if keptTiles == 0 || relTiles != 0 || !hook {
		t.Fatalf("tiles kept %d released %d hook %v", keptTiles, relTiles, hook)
	}
	if relMB >= keptMB {
		t.Errorf("release did not lower the heap: %.1f >= %.1f MiB", relMB, keptMB)
	}
}

func TestTrimMemoryKeepsNewest(t *testing.T) {
	p := NewProvider(ProviderOptions{MemTiles: 8})
	p.mu.Lock()
	for i := 0; i < 12; i++ {
		p.insertLocked(&tile{key: uint64(i), elev: make([]float32, 4)})
	}
	p.mu.Unlock()
	if freed := p.TrimMemory(3); freed != 5*16 {
		t.Errorf("freed %d bytes", freed)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.mem) != 3 || len(p.ring) != 3 || p.mem[11] == nil || p.mem[10] == nil || p.mem[9] == nil {
		t.Errorf("kept %v", p.mem)
	}
	p.insertLocked(&tile{key: 99, elev: make([]float32, 4)})
	if len(p.mem) != 4 {
		t.Errorf("insert after trim: %d", len(p.mem))
	}
}
