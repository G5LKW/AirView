package terrain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// ProviderOptions configure a Provider. Zero values select the defaults.
type ProviderOptions struct {
	// Dir is the cache directory. Tiles are stored as
	// Dir/terrarium/{z}/{x}/{y}.png (and {y}.none for tiles the server does
	// not have). Empty disables the disk cache and downloads entirely.
	Dir string
	// URLTemplate with {z}, {x}, {y} placeholders. Default DefaultTerrariumURL.
	URLTemplate string
	// UserAgent sent with every request. Default "AirView/<model.Version>".
	UserAgent string
	// Client is the HTTP client. Default: 30 s timeout.
	Client *http.Client
	// MaxDiskMB caps the on-disk tile cache. Default 1024 MB. Least recently
	// used tiles are deleted when the cap is exceeded.
	MaxDiskMB int
	// MemTiles is the number of decoded tiles kept in RAM (256 KiB each).
	// Default 64 (16 MiB) — comfortable on a Raspberry Pi 4. It is capped
	// by MaxMemMB.
	MemTiles int
	// MaxMemMB is a hard byte bound on the decoded-tile memory cache: at
	// most MaxMemMB MiB / 256 KiB tiles are kept, whatever MemTiles says.
	// Default DefaultMaxMemMB (32 MiB, sized for a 1 GB Raspberry Pi).
	MaxMemMB int
	// Concurrency is the maximum number of parallel downloads. Default 4.
	Concurrency int
	// Offline disables all downloads (cache only).
	Offline bool
	// Zoom is the zoom level used by ElevationM. Default 10 (~100 m pixels
	// in mid latitudes).
	Zoom int
	// KeepBathymetry returns negative (sea-floor) elevations as they are in
	// the data. Default false: everything below 0 m reads as sea level, which
	// is what matters for radio propagation.
	KeepBathymetry bool
	// RetryAfter is how long the provider stays in offline mode after
	// repeated network failures. Default 10 minutes.
	RetryAfter time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Fetch outcomes.
const (
	FetchCached     = "cached"     // already on disk, nothing downloaded
	FetchDownloaded = "downloaded" // downloaded now
	FetchNoData     = "nodata"     // the server has no such tile (404); remembered on disk
	FetchFailed     = "failed"     // network/server error or offline; will be retried later
)

// ErrOffline is returned when downloads are disabled or suspended.
var ErrOffline = errors.New("terrain: offline (downloads disabled or suspended after network failures)")

// DefaultMaxMemMB is the default byte bound of the decoded-tile memory
// cache (ProviderOptions.MaxMemMB): 32 MiB = 128 tiles.
const DefaultMaxMemMB = 32

// tileMemBytes is the RAM held by one decoded tile (float32 per pixel).
const tileMemBytes = TileSize * TileSize * 4

const (
	tilePixels       = TileSize * TileSize
	maxTileBytes     = 8 << 20
	maxFallbackZooms = 4
	absentTTL        = 30 * time.Second
	absentMax        = 8192
	offlineAfterErrs = 6
)

// tile is a decoded Terrarium tile; immutable once published (except used).
type tile struct {
	key  uint64
	elev []float32 // nil = known to have no data
	used atomic.Bool
}

type diskEntry struct {
	size int64
	used int64 // unix nano of last use
	none bool
}

// Provider serves terrain elevations from Terrarium tiles with an on-disk
// cache and a small in-memory cache of decoded tiles. It implements Source
// (at its default zoom) and Resolver. Safe for concurrent use.
//
// ElevationM never touches the network: it reads decoded tiles from memory or
// the disk cache, falls back to cached lower-zoom tiles, and otherwise
// returns sea level with ok=false. Downloads only happen through Fetch,
// Prefetch and PrefetchPath.
type Provider struct {
	opt  ProviderOptions
	log  *slog.Logger
	zoom atomic.Int32

	mu       sync.RWMutex
	mem      map[uint64]*tile
	ring     []*tile
	hand     int
	inflight map[uint64]*tileLoad

	absentMu sync.Mutex
	absent   map[uint64]int64 // key → unix nano expiry of a recent "not on disk"

	diskMu     sync.Mutex
	diskLoaded bool
	disk       map[uint64]diskEntry
	diskTotal  int64

	netMu        sync.Mutex
	consecErrs   int
	offlineUntil time.Time
	offline      atomic.Bool

	sem       chan struct{}
	capBytes  int64         // disk cache cap
	retryBase time.Duration // first retry back-off

	nDownloads   atomic.Int64
	nDownBytes   atomic.Int64
	nFailures    atomic.Int64
	nNoData      atomic.Int64
	nDiskLoads   atomic.Int64
	nMemHits     atomic.Int64
	nMissingRead atomic.Int64
}

// NewProvider creates a Provider. The cache directory is created on demand.
func NewProvider(o ProviderOptions) *Provider {
	if o.URLTemplate == "" {
		o.URLTemplate = DefaultTerrariumURL
	}
	if o.UserAgent == "" {
		o.UserAgent = model.AppName + "/" + model.Version
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if o.MaxDiskMB <= 0 {
		o.MaxDiskMB = 1024
	}
	if o.MemTiles <= 0 {
		o.MemTiles = 64
	}
	if o.MaxMemMB <= 0 {
		o.MaxMemMB = DefaultMaxMemMB
	}
	if maxTiles := (o.MaxMemMB << 20) / tileMemBytes; o.MemTiles > maxTiles {
		o.MemTiles = maxTiles
	}
	if o.MemTiles < 4 {
		o.MemTiles = 4
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.Zoom <= 0 || o.Zoom > MaxTerrariumZoom {
		o.Zoom = 10
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = 10 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	p := &Provider{
		opt:      o,
		log:      o.Logger.With("component", "terrain"),
		mem:      make(map[uint64]*tile, o.MemTiles),
		ring:     make([]*tile, 0, o.MemTiles),
		inflight: make(map[uint64]*tileLoad),
		absent:   make(map[uint64]int64),
		disk:     make(map[uint64]diskEntry),
		sem:      make(chan struct{}, o.Concurrency),
	}
	p.capBytes = int64(o.MaxDiskMB) << 20
	p.retryBase = time.Second
	p.zoom.Store(int32(o.Zoom))
	p.offline.Store(o.Offline)
	return p
}

func tileKey(z, x, y int) uint64 { return uint64(z)<<58 | uint64(x)<<29 | uint64(y) }

func keyTile(k uint64) TileXY {
	return TileXY{Z: int(k >> 58), X: int(k >> 29 & (1<<29 - 1)), Y: int(k & (1<<29 - 1))}
}

// Zoom returns the default zoom level used by ElevationM.
func (p *Provider) Zoom() int { return int(p.zoom.Load()) }

// SetZoom changes the default zoom level used by ElevationM.
func (p *Provider) SetZoom(z int) {
	if z < 0 {
		z = 0
	}
	if z > MaxTerrariumZoom {
		z = MaxTerrariumZoom
	}
	p.zoom.Store(int32(z))
}

// SetOffline enables or disables downloads at run time.
func (p *Provider) SetOffline(off bool) {
	p.offline.Store(off)
	if !off {
		p.netMu.Lock()
		p.consecErrs, p.offlineUntil = 0, time.Time{}
		p.netMu.Unlock()
	}
}

// ResetBackoff ends a network back-off (offline after repeated failures)
// so the next fetch tries the server again.
func (p *Provider) ResetBackoff() {
	p.netMu.Lock()
	p.consecErrs, p.offlineUntil = 0, time.Time{}
	p.netMu.Unlock()
}

// ElevationM implements Source at the provider's default zoom.
func (p *Provider) ElevationM(lat, lon float64) (float64, bool) {
	return p.ElevationAtZoom(p.Zoom(), lat, lon)
}

// ResolutionM implements Resolver (pixel size at the default zoom).
func (p *Provider) ResolutionM(lat float64) float64 { return PixelSizeM(lat, p.Zoom()) }

// At returns a view of the provider bound to zoom z (a Source + Resolver).
func (p *Provider) At(z int) Source { return zoomView{p: p, z: z} }

type zoomView struct {
	p *Provider
	z int
}

func (v zoomView) ElevationM(lat, lon float64) (float64, bool) {
	return v.p.ElevationAtZoom(v.z, lat, lon)
}
func (v zoomView) ResolutionM(lat float64) float64 { return PixelSizeM(lat, v.z) }

// ElevationAtZoom returns the bilinearly interpolated elevation at (lat, lon)
// from zoom-z tiles. When the zoom-z tile is not cached, up to four lower
// zoom levels are tried (cache only). Without any data the result is
// (0, false).
func (p *Provider) ElevationAtZoom(z int, lat, lon float64) (float64, bool) {
	if math.IsNaN(lat) || math.IsNaN(lon) {
		return 0, false
	}
	if z > MaxTerrariumZoom {
		z = MaxTerrariumZoom
	}
	for zz := z; zz >= 0 && zz >= z-maxFallbackZooms; zz-- {
		if v, ok := p.sample(zz, lat, lon); ok {
			if !p.opt.KeepBathymetry && v < 0 {
				v = 0
			}
			return v, true
		}
	}
	p.nMissingRead.Add(1)
	return 0, false
}

// sample does the bilinear interpolation between pixel centres at zoom z.
func (p *Provider) sample(z int, lat, lon float64) (float64, bool) {
	n := (1 << z) * TileSize
	gx := MercatorX(lon)*float64(n) - 0.5
	gy := MercatorY(lat)*float64(n) - 0.5
	fx0, fy0 := math.Floor(gx), math.Floor(gy)
	fx, fy := gx-fx0, gy-fy0
	ix0, iy0 := int(fx0), int(fy0)
	ix1, iy1 := ix0+1, iy0+1
	ix0 = ((ix0 % n) + n) % n
	ix1 = ((ix1 % n) + n) % n
	if iy0 < 0 {
		iy0 = 0
	}
	if iy1 < 0 {
		iy1 = 0
	}
	if iy0 > n-1 {
		iy0 = n - 1
	}
	if iy1 > n-1 {
		iy1 = n - 1
	}
	tx0, ty0, tx1, ty1 := ix0>>8, iy0>>8, ix1>>8, iy1>>8
	var v00, v10, v01, v11 float32
	if tx0 == tx1 && ty0 == ty1 {
		t := p.getTile(z, tx0, ty0)
		if t == nil || t.elev == nil {
			return 0, false
		}
		e := t.elev
		x0, x1, y0, y1 := ix0&255, ix1&255, (iy0&255)*TileSize, (iy1&255)*TileSize
		v00, v10, v01, v11 = e[y0+x0], e[y0+x1], e[y1+x0], e[y1+x1]
	} else {
		var ok bool
		if v00, ok = p.pixel(z, ix0, iy0); !ok {
			return 0, false
		}
		if v10, ok = p.pixel(z, ix1, iy0); !ok {
			return 0, false
		}
		if v01, ok = p.pixel(z, ix0, iy1); !ok {
			return 0, false
		}
		if v11, ok = p.pixel(z, ix1, iy1); !ok {
			return 0, false
		}
	}
	top := float64(v00) + (float64(v10)-float64(v00))*fx
	bot := float64(v01) + (float64(v11)-float64(v01))*fx
	return top + (bot-top)*fy, true
}

func (p *Provider) pixel(z, gx, gy int) (float32, bool) {
	t := p.getTile(z, gx>>8, gy>>8)
	if t == nil || t.elev == nil {
		return 0, false
	}
	return t.elev[(gy&255)*TileSize+gx&255], true
}

// getTile returns the decoded tile from memory or disk, or nil.
// tileLoad is one in-flight disk load; t is set before done is closed.
type tileLoad struct {
	done chan struct{}
	t    *tile
}

func (p *Provider) getTile(z, x, y int) *tile {
	k := tileKey(z, x, y)
	for attempt := 0; attempt < 3; attempt++ {
		p.mu.RLock()
		t := p.mem[k]
		p.mu.RUnlock()
		if t != nil {
			t.used.Store(true)
			p.nMemHits.Add(1)
			return t
		}
		if p.isAbsent(k) {
			return nil
		}
		p.mu.Lock()
		if t = p.mem[k]; t != nil {
			p.mu.Unlock()
			t.used.Store(true)
			return t
		}
		if ld, busy := p.inflight[k]; busy {
			p.mu.Unlock()
			<-ld.done
			if ld.t != nil {
				// Use the loader's result directly: with a small memory
				// cache and many concurrent loads the tile may already have
				// been evicted again (it would then read as missing).
				ld.t.used.Store(true)
				return ld.t
			}
			continue
		}
		ld := &tileLoad{done: make(chan struct{})}
		p.inflight[k] = ld
		p.mu.Unlock()

		t = p.loadFromDisk(k)

		p.mu.Lock()
		if t != nil {
			p.insertLocked(t)
		}
		ld.t = t
		delete(p.inflight, k)
		close(ld.done)
		p.mu.Unlock()
		if t == nil {
			p.markAbsent(k)
		}
		return t
	}
	return nil
}

// TrimMemory drops decoded tiles from the memory cache until at most keep
// remain (keep <= 0 drops them all) and returns the bytes released to the
// garbage collector. Tiles are re-read from the disk cache on demand. The
// terrain Service calls it after every analysis job, when the hundreds of
// tiles streamed through the cache are no longer needed.
func (p *Provider) TrimMemory(keep int) int64 {
	if keep < 0 {
		keep = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ring) <= keep {
		return 0
	}
	var freed int64
	// Keep the most recently inserted tiles: the ring slot before the hand
	// was written last.
	kept := make([]*tile, 0, p.opt.MemTiles)
	n := len(p.ring)
	for i := 0; i < n; i++ {
		t := p.ring[(p.hand-1-i+2*n)%n]
		if len(kept) < keep {
			kept = append(kept, t)
			continue
		}
		delete(p.mem, t.key)
		freed += int64(len(t.elev) * 4)
	}
	for i := range p.ring {
		p.ring[i] = nil
	}
	p.ring = append(p.ring[:0], kept...)
	p.hand = 0
	return freed
}

// insertLocked adds t to the memory cache (clock / second-chance eviction).
func (p *Provider) insertLocked(t *tile) {
	t.used.Store(true)
	if len(p.ring) < p.opt.MemTiles {
		p.ring = append(p.ring, t)
		p.mem[t.key] = t
		return
	}
	for {
		old := p.ring[p.hand]
		if old.used.Swap(false) {
			p.hand = (p.hand + 1) % len(p.ring)
			continue
		}
		delete(p.mem, old.key)
		p.ring[p.hand] = t
		p.mem[t.key] = t
		p.hand = (p.hand + 1) % len(p.ring)
		return
	}
}

func (p *Provider) isAbsent(k uint64) bool {
	p.absentMu.Lock()
	exp, ok := p.absent[k]
	p.absentMu.Unlock()
	return ok && time.Now().UnixNano() < exp
}

func (p *Provider) markAbsent(k uint64) {
	p.absentMu.Lock()
	if len(p.absent) >= absentMax {
		clear(p.absent)
	}
	p.absent[k] = time.Now().Add(absentTTL).UnixNano()
	p.absentMu.Unlock()
}

func (p *Provider) clearAbsent(k uint64) {
	p.absentMu.Lock()
	delete(p.absent, k)
	p.absentMu.Unlock()
}

// ---------------------------------------------------------------------------
// Disk cache

func (p *Provider) tilePath(t TileXY, none bool) string {
	ext := ".png"
	if none {
		ext = ".none"
	}
	return filepath.Join(p.opt.Dir, "terrarium", strconv.Itoa(t.Z), strconv.Itoa(t.X), strconv.Itoa(t.Y)+ext)
}

// ensureIndexLocked scans the cache directory once. diskMu must be held.
func (p *Provider) ensureIndexLocked() {
	if p.diskLoaded {
		return
	}
	p.diskLoaded = true
	if p.opt.Dir == "" {
		return
	}
	root := filepath.Join(p.opt.Dir, "terrarium")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(path)
			return nil
		}
		none := strings.HasSuffix(name, ".none")
		if !none && !strings.HasSuffix(name, ".png") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 {
			return nil
		}
		z, e1 := strconv.Atoi(parts[0])
		x, e2 := strconv.Atoi(parts[1])
		y, e3 := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(parts[2], ".png"), ".none"))
		t := TileXY{Z: z, X: x, Y: y}
		if e1 != nil || e2 != nil || e3 != nil || !t.Valid() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		p.disk[tileKey(z, x, y)] = diskEntry{size: info.Size(), used: info.ModTime().UnixNano(), none: none}
		p.diskTotal += info.Size()
		return nil
	})
}

func (p *Provider) diskLookup(k uint64) (diskEntry, bool) {
	p.diskMu.Lock()
	defer p.diskMu.Unlock()
	p.ensureIndexLocked()
	e, ok := p.disk[k]
	return e, ok
}

// loadFromDisk reads and decodes a cached tile; nil when not cached.
func (p *Provider) loadFromDisk(k uint64) *tile {
	if p.opt.Dir == "" {
		return nil
	}
	e, ok := p.diskLookup(k)
	if !ok {
		return nil
	}
	t := keyTile(k)
	if e.none {
		return &tile{key: k}
	}
	path := p.tilePath(t, false)
	data, err := os.ReadFile(path)
	if err != nil {
		p.dropDiskEntry(k)
		return nil
	}
	elev := make([]float32, tilePixels)
	if err := DecodeTerrarium(bytes.NewReader(data), elev); err != nil {
		p.log.Warn("terrain: corrupt cached tile removed", "tile", t.String(), "err", err)
		_ = os.Remove(path)
		p.dropDiskEntry(k)
		return nil
	}
	p.nDiskLoads.Add(1)
	now := time.Now()
	p.diskMu.Lock()
	if de, ok := p.disk[k]; ok {
		de.used = now.UnixNano()
		p.disk[k] = de
	}
	p.diskMu.Unlock()
	_ = os.Chtimes(path, now, now) // persist LRU order across restarts
	return &tile{key: k, elev: elev}
}

func (p *Provider) dropDiskEntry(k uint64) {
	p.diskMu.Lock()
	if e, ok := p.disk[k]; ok {
		p.diskTotal -= e.size
		delete(p.disk, k)
	}
	p.diskMu.Unlock()
}

// HasTile reports whether the tile (or its "no data" marker) is cached on disk.
func (p *Provider) HasTile(t TileXY) bool {
	if p.opt.Dir == "" {
		return false
	}
	_, ok := p.diskLookup(tileKey(t.Z, t.X, t.Y))
	return ok
}

func (p *Provider) storeTile(t TileXY, data []byte, none bool) error {
	path := p.tilePath(t, none)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	k := tileKey(t.Z, t.X, t.Y)
	p.diskMu.Lock()
	p.ensureIndexLocked()
	if old, ok := p.disk[k]; ok {
		p.diskTotal -= old.size
	}
	p.disk[k] = diskEntry{size: int64(len(data)), used: time.Now().UnixNano(), none: none}
	p.diskTotal += int64(len(data))
	p.diskMu.Unlock()
	p.clearAbsent(k)
	return nil
}

// enforceCap deletes least recently used tiles until the cache is below
// 90 % of MaxDiskMB. Tiles in protect are kept if at all possible.
func (p *Provider) enforceCap(protect map[uint64]bool) {
	if p.opt.Dir == "" {
		return
	}
	limit := p.capBytes
	p.diskMu.Lock()
	p.ensureIndexLocked()
	if p.diskTotal <= limit {
		p.diskMu.Unlock()
		return
	}
	type ent struct {
		k uint64
		e diskEntry
	}
	all := make([]ent, 0, len(p.disk))
	for k, e := range p.disk {
		all = append(all, ent{k, e})
	}
	sort.Slice(all, func(i, j int) bool {
		pi, pj := protect[all[i].k], protect[all[j].k]
		if pi != pj {
			return !pi
		}
		return all[i].e.used < all[j].e.used
	})
	target := limit * 9 / 10
	var victims []ent
	for _, a := range all {
		if p.diskTotal <= target {
			break
		}
		victims = append(victims, a)
		p.diskTotal -= a.e.size
		delete(p.disk, a.k)
	}
	p.diskMu.Unlock()
	for _, v := range victims {
		_ = os.Remove(p.tilePath(keyTile(v.k), v.e.none))
	}
	if len(victims) > 0 {
		p.log.Info("terrain: tile cache trimmed", "removed", len(victims), "capMB", p.opt.MaxDiskMB)
	}
}

// ---------------------------------------------------------------------------
// Network

func (p *Provider) tileURL(t TileXY) string {
	return strings.NewReplacer("{z}", strconv.Itoa(t.Z), "{x}", strconv.Itoa(t.X), "{y}", strconv.Itoa(t.Y)).Replace(p.opt.URLTemplate)
}

// Offline reports whether downloads are currently disabled or suspended.
func (p *Provider) Offline() bool {
	if p.offline.Load() || p.opt.Dir == "" {
		return true
	}
	p.netMu.Lock()
	defer p.netMu.Unlock()
	return time.Now().Before(p.offlineUntil)
}

func (p *Provider) noteNet(ok bool) {
	p.netMu.Lock()
	defer p.netMu.Unlock()
	if ok {
		p.consecErrs = 0
		return
	}
	p.consecErrs++
	if p.consecErrs >= offlineAfterErrs && !time.Now().Before(p.offlineUntil) {
		p.offlineUntil = time.Now().Add(p.opt.RetryAfter)
		p.log.Warn("terrain: tile server unreachable, working offline from cache", "retryAfter", p.opt.RetryAfter)
	}
}

// Fetch makes sure tile t is in the disk cache, downloading it if needed.
// Tiles already cached are never downloaded again. It returns one of the
// Fetch* outcomes and the number of bytes downloaded.
func (p *Provider) Fetch(ctx context.Context, t TileXY) (string, int64, error) {
	if !t.Valid() || t.Z > MaxTerrariumZoom {
		return FetchFailed, 0, fmt.Errorf("terrain: invalid tile %s", t)
	}
	k := tileKey(t.Z, t.X, t.Y)
	if p.opt.Dir != "" {
		if e, ok := p.diskLookup(k); ok {
			if e.none {
				return FetchNoData, 0, nil
			}
			return FetchCached, 0, nil
		}
	}
	if p.Offline() {
		return FetchFailed, 0, ErrOffline
	}
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return FetchFailed, 0, ctx.Err()
	}
	defer func() { <-p.sem }()
	// Someone else may have fetched it while we waited.
	if e, ok := p.diskLookup(k); ok {
		if e.none {
			return FetchNoData, 0, nil
		}
		return FetchCached, 0, nil
	}
	var lastErr error
	backoff := p.retryBase
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return FetchFailed, 0, ctx.Err()
			}
			backoff *= 2
		}
		data, status, err := p.download(ctx, t)
		if ctx.Err() != nil {
			return FetchFailed, 0, ctx.Err()
		}
		switch {
		case err == nil && status == http.StatusOK:
			p.noteNet(true)
			if err := p.storeTile(t, data, false); err != nil {
				return FetchFailed, 0, fmt.Errorf("terrain: store tile %s: %w", t, err)
			}
			p.nDownloads.Add(1)
			p.nDownBytes.Add(int64(len(data)))
			return FetchDownloaded, int64(len(data)), nil
		case err == nil && status == http.StatusNotFound:
			p.noteNet(true)
			p.nNoData.Add(1)
			if err := p.storeTile(t, nil, true); err != nil {
				return FetchNoData, 0, nil
			}
			return FetchNoData, 0, nil
		case err == nil && status != http.StatusTooManyRequests && status < 500:
			p.noteNet(true)
			p.nFailures.Add(1)
			return FetchFailed, 0, fmt.Errorf("terrain: tile %s: HTTP %d", t, status)
		default:
			if err == nil {
				err = fmt.Errorf("terrain: tile %s: HTTP %d", t, status)
			}
			lastErr = err
		}
	}
	p.noteNet(false)
	p.nFailures.Add(1)
	return FetchFailed, 0, lastErr
}

func (p *Provider) download(ctx context.Context, t TileXY) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.tileURL(t), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", p.opt.UserAgent)
	resp, err := p.opt.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, resp.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTileBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > maxTileBytes {
		return nil, 0, fmt.Errorf("terrain: tile %s too large", t)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != TileSize || cfg.Height != TileSize {
		return nil, 0, fmt.Errorf("terrain: tile %s is not a 256x256 PNG", t)
	}
	return data, http.StatusOK, nil
}

// PrefetchProgress reports the progress of a Prefetch.
type PrefetchProgress struct {
	Zoom       int   `json:"zoom"`
	Total      int   `json:"total"`
	Done       int   `json:"done"`
	Cached     int   `json:"cached"`
	Downloaded int   `json:"downloaded"`
	NoData     int   `json:"noData"`
	Failed     int   `json:"failed"`
	Bytes      int64 `json:"bytes"`
	Offline    bool  `json:"offline"`
}

// TilesInRadius lists the zoom-z tiles that intersect the disc of radiusKm
// around (lat, lon), nearest first.
func TilesInRadius(lat, lon, radiusKm float64, z int) []TileXY {
	if !(radiusKm > 0) {
		return []TileXY{TileAt(lat, lon, z)}
	}
	const kmPerDeg = 111.19492664455873 // geo.EarthRadiusKm·π/180
	dLat := radiusKm / kmPerDeg
	south := math.Max(lat-dLat, -maxMercatorLat)
	north := math.Min(lat+dLat, maxMercatorLat)
	maxAbs := math.Max(math.Abs(south), math.Abs(north))
	dLon := 180.0
	if c := math.Cos(maxAbs * degToRad); c > 1e-6 {
		dLon = math.Min(180, dLat/c)
	}
	n := 1 << z
	ty0 := TileAt(north, lon, z).Y
	ty1 := TileAt(south, lon, z).Y
	var xs []int
	if dLon >= 180 {
		for x := 0; x < n; x++ {
			xs = append(xs, x)
		}
	} else {
		xw := int(math.Floor((lon - dLon + 180) / 360 * float64(n)))
		xe := int(math.Floor((lon + dLon + 180) / 360 * float64(n)))
		seen := map[int]bool{}
		for x := xw; x <= xe; x++ {
			xx := ((x % n) + n) % n
			if !seen[xx] {
				seen[xx] = true
				xs = append(xs, xx)
			}
		}
	}
	type cand struct {
		t TileXY
		d float64
	}
	var out []cand
	o := newOrigin(lat, lon)
	for y := ty0; y <= ty1; y++ {
		for _, x := range xs {
			t := TileXY{Z: z, X: x, Y: y}
			s, w, nn, e := TileBounds(t)
			cl := math.Max(s, math.Min(nn, lat))
			// Nearest longitude of the tile to lon, wrap-aware.
			cLon := lon
			if !lonInside(lon, w, e) {
				if math.Abs(angDiff(lon, w)) < math.Abs(angDiff(lon, e)) {
					cLon = w
				} else {
					cLon = e
				}
			}
			d, _ := o.inverse(cl, cLon)
			if d <= radiusKm+1 {
				out = append(out, cand{t, d})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].d != out[j].d {
			return out[i].d < out[j].d
		}
		if out[i].t.Y != out[j].t.Y {
			return out[i].t.Y < out[j].t.Y
		}
		return out[i].t.X < out[j].t.X
	})
	res := make([]TileXY, len(out))
	for i := range out {
		res[i] = out[i].t
	}
	return res
}

func lonInside(lon, w, e float64) bool {
	d := math.Mod(lon-w+720, 360)
	return d <= e-w
}

func angDiff(a, b float64) float64 { return math.Mod(a-b+540, 360) - 180 }

// Prefetch downloads (if missing) every zoom-z tile within radiusKm of
// (lat, lon), nearest first, with at most Concurrency parallel requests.
// progress (may be nil) is called after every tile, never concurrently.
// Failed tiles are counted, not returned as errors; the error is non-nil only
// when ctx was cancelled. After the run the disk cache is trimmed to its cap
// (never deleting tiles of this prefetch unless they alone exceed it).
func (p *Provider) Prefetch(ctx context.Context, lat, lon, radiusKm float64, z int, progress func(PrefetchProgress)) (PrefetchProgress, error) {
	return p.prefetchTiles(ctx, TilesInRadius(lat, lon, radiusKm, z), z, progress)
}

// PrefetchPath downloads the zoom-z tiles along the great circle between two
// points (for an on-demand LOS profile).
func (p *Provider) PrefetchPath(ctx context.Context, lat1, lon1, lat2, lon2 float64, z int) (PrefetchProgress, error) {
	o := newOrigin(lat1, lon1)
	dist, brg := o.inverse(lat2, lon2)
	step := TileWidthKm(math.Max(math.Abs(lat1), math.Abs(lat2)), z) / 4
	if !(step > 0.05) {
		step = 0.05
	}
	seen := map[TileXY]bool{}
	var tiles []TileXY
	for d := 0.0; ; d += step {
		if d > dist {
			d = dist
		}
		la, lo := o.destKm(brg, d)
		t := TileAt(la, lo, z)
		if !seen[t] {
			seen[t] = true
			tiles = append(tiles, t)
		}
		if d >= dist {
			break
		}
	}
	return p.prefetchTiles(ctx, tiles, z, nil)
}

func (p *Provider) prefetchTiles(ctx context.Context, tiles []TileXY, z int, progress func(PrefetchProgress)) (PrefetchProgress, error) {
	pr := PrefetchProgress{Zoom: z, Total: len(tiles)}
	if len(tiles) == 0 {
		return pr, nil
	}
	protect := make(map[uint64]bool, len(tiles))
	for _, t := range tiles {
		protect[tileKey(t.Z, t.X, t.Y)] = true
	}
	var mu sync.Mutex
	work := make(chan TileXY)
	var wg sync.WaitGroup
	workers := p.opt.Concurrency
	if workers > len(tiles) {
		workers = len(tiles)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				res, n, _ := p.Fetch(ctx, t)
				mu.Lock()
				pr.Done++
				switch res {
				case FetchCached:
					pr.Cached++
				case FetchDownloaded:
					pr.Downloaded++
					pr.Bytes += n
				case FetchNoData:
					pr.NoData++
				default:
					pr.Failed++
				}
				pr.Offline = p.Offline()
				if progress != nil {
					progress(pr)
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, t := range tiles {
		select {
		case work <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if pr.Downloaded > 0 {
		p.enforceCap(protect)
	}
	if err := ctx.Err(); err != nil {
		return pr, err
	}
	return pr, nil
}

// ProviderStats is a snapshot of the provider's caches and counters.
type ProviderStats struct {
	URL            string `json:"url"`
	Dir            string `json:"dir"`
	Zoom           int    `json:"zoom"`
	MemTiles       int    `json:"memTiles"`
	MemCapacity    int    `json:"memCapacity"`
	MemBytes       int64  `json:"memBytes"`
	DiskTiles      int    `json:"diskTiles"`
	DiskBytes      int64  `json:"diskBytes"`
	DiskCapBytes   int64  `json:"diskCapBytes"`
	Downloads      int64  `json:"downloads"`
	DownloadBytes  int64  `json:"downloadBytes"`
	Failures       int64  `json:"failures"`
	NoData         int64  `json:"noData"`
	DiskLoads      int64  `json:"diskLoads"`
	MissingReads   int64  `json:"missingReads"`
	Offline        bool   `json:"offline"`
	OfflineUntilMs int64  `json:"offlineUntilMs"`
}

// Stats returns cache statistics.
func (p *Provider) Stats() ProviderStats {
	s := ProviderStats{
		URL: p.opt.URLTemplate, Dir: p.opt.Dir, Zoom: p.Zoom(), MemCapacity: p.opt.MemTiles,
		DiskCapBytes: p.capBytes,
		Downloads:    p.nDownloads.Load(), DownloadBytes: p.nDownBytes.Load(), Failures: p.nFailures.Load(),
		NoData: p.nNoData.Load(), DiskLoads: p.nDiskLoads.Load(), MissingReads: p.nMissingRead.Load(),
		Offline: p.Offline(),
	}
	p.mu.RLock()
	s.MemTiles = len(p.mem)
	for _, t := range p.mem {
		s.MemBytes += int64(len(t.elev) * 4)
	}
	p.mu.RUnlock()
	if p.opt.Dir != "" {
		p.diskMu.Lock()
		p.ensureIndexLocked()
		s.DiskTiles, s.DiskBytes = len(p.disk), p.diskTotal
		p.diskMu.Unlock()
	}
	p.netMu.Lock()
	if time.Now().Before(p.offlineUntil) {
		s.OfflineUntilMs = p.offlineUntil.UnixMilli()
	}
	p.netMu.Unlock()
	return s
}
