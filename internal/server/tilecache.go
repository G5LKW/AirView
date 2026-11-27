package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The tile cache: GET /tilecache/{host}/{path...} fetches
// https://{host}/{path} for a fixed list of free map hosts and keeps the
// answer on disk under <data>/tilecache, so a Raspberry Pi station (or a
// desktop without a permanent connection) serves the map from its own disk
// and the tile providers see one polite client.
//
// It is not an open proxy: only the hosts in tileHosts are ever contacted,
// only with GET, without cookies or client headers, and only image / vector
// tile / font / JSON content is passed on.
//
// On disk every object is one file <dir>/<host>/<2 hex>/<sha256 of the URL>:
// a fixed 512-byte JSON header (content type, encoding, validators, expiry)
// followed by the body exactly as the upstream sent it (possibly gzip).

// tileHosts is the whitelist (exact host names).
var tileHosts = []string{"tiles.openfreemap.org", "tile.openstreetmap.org", "tilecache.rainviewer.com"}

// tileQueryParams lists, per host, the query parameters that are part of
// the upstream URL. Every other client parameter (cache busters such as
// "?_=1700000000" or "?v=3" added by a browser, a map library or a stale
// link) is dropped from both the upstream request and the cache key, so the
// same tile is stored and fetched once. None of the whitelisted hosts uses
// a query today (their tile, style, TileJSON, glyph and sprite URLs are pure
// paths); a host that needs one gets its parameter names here.
var tileQueryParams = map[string][]string{}

// tileQuery returns the normalised query of a tile request: only the
// parameters of tileQueryParams[host], sorted by name and value, encoded.
func tileQuery(host, raw string) string {
	keep := tileQueryParams[host]
	if raw == "" || len(keep) == 0 {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}
	out := url.Values{}
	for _, k := range keep {
		if v, ok := vals[k]; ok {
			v = append([]string(nil), v...)
			sort.Strings(v)
			out[k] = v
		}
	}
	return out.Encode() // sorted by key
}

const (
	tileHeaderSize    = 512
	tileMaxObject     = 8 << 20 // largest body cached or passed on
	tileDefaultMaxMB  = 512     // default size of the disk cache (the storage profile may lower it)
	tileMinTTL        = 24 * time.Hour
	tileMinTTLJSON    = 10 * time.Minute // style / TileJSON documents change more often
	tileMaxTTL        = 30 * 24 * time.Hour
	tileFetchTimeout  = 25 * time.Second
	tileConcurrency   = 8
	tileTouchAfter    = 12 * time.Hour // a hit refreshes mtime (the eviction order) at most this often
	tileEvictTo       = 0.85           // eviction target, fraction of the cap
	tileScanInterval  = 30 * time.Minute
	tileMaxPathLen    = 1024
	tileMaxQueryLen   = 512
	tileNegativeTTL   = 2 * time.Minute // upstream errors are remembered this long
	tileNegativeLimit = 2048
	tileStaleRetry    = 5 * time.Minute // an expired object served during an outage is retried after this
)

// tileMeta is the header of a cache file.
type tileMeta struct {
	CT      string `json:"ct"`             // Content-Type
	Enc     string `json:"enc,omitempty"`  // Content-Encoding of the stored body ("" or "gzip")
	ETag    string `json:"etag,omitempty"` // upstream validator
	LastMod string `json:"lm,omitempty"`   // upstream Last-Modified
	Exp     int64  `json:"exp"`            // fresh until (Unix s)
	Fetched int64  `json:"t"`              // downloaded / last validated (Unix s)
	Size    int64  `json:"n"`              // body bytes
}

type tileCall struct {
	done   chan struct{}
	status int    // 0 = the object is on disk now
	msg    string // error text for status != 0
	how    string // MISS | REVALIDATED | STALE
}

type tileNeg struct {
	status int
	msg    string
	until  time.Time
}

type tileCache struct {
	s        *Server
	dir      string
	maxBytes int64
	client   *http.Client
	// upstream maps a whitelisted host to its base URL (tests point it at a
	// fake server).
	upstream func(host string) string
	sem      chan struct{}

	mu       sync.Mutex
	inflight map[string]*tileCall
	negative map[string]tileNeg

	bytes, files                              atomic.Int64
	scanned                                   atomic.Bool
	hits, misses, revalidated, stale, errorsN atomic.Uint64
	evictions                                 atomic.Uint64
	evictC                                    chan struct{}
}

func newTileCache(s *Server) *tileCache {
	maxMB := tileCacheMaxMB(s.eng.TileCacheDefaultMB(), os.Getenv("AIRVIEW_TILECACHE_MB"))
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = tileConcurrency
	tr.ResponseHeaderTimeout = 15 * time.Second
	tr.DisableCompression = true // the body is stored as sent
	t := &tileCache{
		s:        s,
		dir:      filepath.Join(s.eng.DataDir(), "tilecache"),
		maxBytes: maxMB << 20,
		upstream: func(host string) string { return "https://" + host },
		sem:      make(chan struct{}, tileConcurrency),
		inflight: map[string]*tileCall{},
		negative: map[string]tileNeg{},
		evictC:   make(chan struct{}, 1),
	}
	t.client = &http.Client{
		Transport: tr,
		Timeout:   tileFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Never leave the whitelisted host.
			if len(via) >= 3 || req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	return t
}

func (t *tileCache) hosts() []string { return append([]string(nil), tileHosts...) }

func tileHostAllowed(host string) bool {
	for _, h := range tileHosts {
		if h == host {
			return true
		}
	}
	return false
}

// tileStats is GET /api/tilecache.
type tileStats struct {
	Enabled     bool     `json:"enabled"`
	Hosts       []string `json:"hosts"`
	Dir         string   `json:"dir"`
	Bytes       int64    `json:"bytes"`    // size on disk
	Files       int64    `json:"files"`    //
	MaxBytes    int64    `json:"maxBytes"` // eviction starts above this
	Scanned     bool     `json:"scanned"`  // false until the start-up scan of the directory finished
	Hits        uint64   `json:"hits"`     // served from disk since start
	Misses      uint64   `json:"misses"`   // downloaded
	Revalidated uint64   `json:"revalidated"`
	Stale       uint64   `json:"stale"`  // served although expired because the upstream failed
	Errors      uint64   `json:"errors"` // upstream failures
	Evictions   uint64   `json:"evictions"`
}

func (t *tileCache) stats() tileStats {
	return tileStats{
		Enabled: t.enabled(), Hosts: t.hosts(), Dir: t.dir,
		Bytes: t.bytes.Load(), Files: t.files.Load(), MaxBytes: t.maxBytes, Scanned: t.scanned.Load(),
		Hits: t.hits.Load(), Misses: t.misses.Load(), Revalidated: t.revalidated.Load(),
		Stale: t.stale.Load(), Errors: t.errorsN.Load(), Evictions: t.evictions.Load(),
	}
}

func (t *tileCache) enabled() bool {
	h := t.s.eng.Config()
	return h != nil && h.Get().HTTP.TileCache
}

// run scans the cache directory once and evicts when the cap is exceeded.
func (t *tileCache) run(ctx context.Context) {
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-t.evictC:
		}
		t.sweep(ctx)
		timer.Reset(tileScanInterval)
	}
}

type tileFile struct {
	path  string
	size  int64
	mtime time.Time
}

// sweep walks the directory, corrects the size counters and deletes the
// least recently used files while the cache is over its cap.
func (t *tileCache) sweep(ctx context.Context) {
	var list []tileFile
	var total int64
	now := time.Now()
	_ = filepath.WalkDir(t.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if strings.HasSuffix(p, ".tmp") {
			if now.Sub(info.ModTime()) > 10*time.Minute {
				_ = os.Remove(p) // left over from a crash
			}
			return nil
		}
		list = append(list, tileFile{p, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if ctx.Err() != nil {
		return
	}
	if total > t.maxBytes {
		sort.Slice(list, func(i, j int) bool { return list[i].mtime.Before(list[j].mtime) })
		target := int64(float64(t.maxBytes) * tileEvictTo)
		removed := 0
		for removed < len(list) && total > target {
			f := list[removed]
			if os.Remove(f.path) == nil {
				total -= f.size
				t.evictions.Add(1)
			}
			removed++
		}
		list = list[removed:]
		t.s.log.Info("tile cache trimmed", "removed", removed, "bytes", total)
	}
	t.bytes.Store(total)
	t.files.Store(int64(len(list)))
	t.scanned.Store(true)
}

// clear deletes the whole cache.
func (t *tileCache) clear() error {
	err := os.RemoveAll(t.dir)
	t.bytes.Store(0)
	t.files.Store(0)
	t.mu.Lock()
	clear(t.negative)
	t.mu.Unlock()
	return err
}

func tileError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeError(w, status, msg)
}

// validTilePath checks the escaped upstream path.
func validTilePath(p string) bool {
	if p == "" || len(p) > tileMaxPathLen || strings.HasPrefix(p, "/") {
		return false
	}
	dec, err := url.PathUnescape(p)
	if err != nil {
		return false
	}
	lower := strings.ToLower(p)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%00") {
		return false
	}
	for _, r := range dec {
		// '@' is allowed: HiDPI sprites are "ofm@2x.json". The path always
		// follows "https://<host>/", so it cannot form a userinfo part.
		if r < 0x20 || r == 0x7f || r == '\\' {
			return false
		}
	}
	for _, seg := range strings.Split(dec, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// serve handles /tilecache/{host}/{path...}.
func (t *tileCache) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		tileError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !t.enabled() {
		tileError(w, http.StatusNotFound, "tile cache is disabled")
		return
	}
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/tilecache/")
	host, p, ok := strings.Cut(rest, "/")
	if !ok || host == "" {
		tileError(w, http.StatusBadRequest, "use /tilecache/<host>/<path>")
		return
	}
	if !tileHostAllowed(host) {
		tileError(w, http.StatusForbidden, "host is not on the tile cache whitelist")
		return
	}
	raw := r.URL.RawQuery
	if !validTilePath(p) || len(raw) > tileMaxQueryLen || strings.ContainsAny(raw, "\r\n #") {
		tileError(w, http.StatusBadRequest, "invalid tile path")
		return
	}
	query := tileQuery(host, raw)
	target := t.upstream(host) + "/" + p
	ident := host + "/" + p
	if query != "" {
		target += "?" + query
		ident += "?" + query
	}
	sum := sha256.Sum256([]byte(ident))
	key := hex.EncodeToString(sum[:])
	file := filepath.Join(t.dir, host, key[:2], key)

	meta, haveFile := readTileMeta(file)
	now := time.Now()
	how := "HIT"
	if !haveFile || now.Unix() >= meta.Exp {
		call := t.fetch(r.Context(), key, file, target, meta, haveFile)
		if call.status != 0 {
			tileError(w, call.status, call.msg)
			return
		}
		how = call.how
		meta, haveFile = readTileMeta(file)
		if !haveFile {
			tileError(w, http.StatusBadGateway, "tile cache: object vanished")
			return
		}
	} else {
		t.hits.Add(1)
	}
	t.send(w, r, file, meta, how)
}

// send writes a cached object to the client.
func (t *tileCache) send(w http.ResponseWriter, r *http.Request, file string, meta tileMeta, how string) {
	f, err := os.Open(file)
	if err != nil {
		tileError(w, http.StatusBadGateway, "tile cache: "+err.Error())
		return
	}
	defer f.Close()
	if how == "HIT" {
		if info, err := f.Stat(); err == nil && time.Since(info.ModTime()) > tileTouchAfter {
			now := time.Now()
			_ = os.Chtimes(file, now, now)
		}
	}
	etag := meta.ETag
	if etag == "" {
		etag = fmt.Sprintf(`W/"av-%d-%d"`, meta.Size, meta.Fetched)
	}
	h := w.Header()
	h.Set("Content-Type", meta.CT)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("ETag", etag)
	if meta.LastMod != "" {
		h.Set("Last-Modified", meta.LastMod)
	}
	// The browser may keep it as long as we consider it fresh, at least an
	// hour (a stale copy served during an upstream outage included).
	maxAge := max(meta.Exp-time.Now().Unix(), 3600)
	h.Set("Cache-Control", "public, max-age="+strconv.FormatInt(maxAge, 10))
	h.Set("X-AirView-Cache", how)
	h.Add("Vary", "Accept-Encoding")

	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagMatch(inm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	} else if ims := r.Header.Get("If-Modified-Since"); ims != "" && meta.LastMod != "" {
		if a, err1 := http.ParseTime(ims); err1 == nil {
			if b, err2 := http.ParseTime(meta.LastMod); err2 == nil && !b.After(a) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	body := io.NewSectionReader(f, tileHeaderSize, meta.Size)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(60 * time.Second))
	if meta.Enc == "gzip" && !acceptsGzip(r) {
		zr, err := gzip.NewReader(body)
		if err != nil {
			tileError(w, http.StatusBadGateway, "tile cache: corrupt object")
			return
		}
		defer zr.Close()
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, io.LimitReader(zr, 4*tileMaxObject))
		}
		return
	}
	if meta.Enc != "" {
		h.Set("Content-Encoding", meta.Enc)
	}
	h.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, body)
	}
}

func etagMatch(header, etag string) bool {
	strip := func(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "W/") }
	want := strip(etag)
	for _, part := range strings.Split(header, ",") {
		if p := strings.TrimSpace(part); p == "*" || strip(p) == want {
			return true
		}
	}
	return false
}

// readTileMeta reads the header of a cache file.
func readTileMeta(file string) (tileMeta, bool) {
	var m tileMeta
	f, err := os.Open(file)
	if err != nil {
		return m, false
	}
	defer f.Close()
	var hdr [tileHeaderSize]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return m, false
	}
	if json.Unmarshal(bytes.TrimSpace(hdr[:]), &m) != nil || m.CT == "" || m.Size < 0 {
		return m, false
	}
	if info, err := f.Stat(); err != nil || info.Size() != tileHeaderSize+m.Size {
		return m, false // truncated
	}
	return m, true
}

func encodeTileMeta(m tileMeta) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(b) > tileHeaderSize-1 {
		// Oversized validators: drop them rather than fail.
		m.ETag, m.LastMod = "", ""
		if b, err = json.Marshal(m); err != nil || len(b) > tileHeaderSize-1 {
			return nil, errors.New("tile header too large")
		}
	}
	hdr := bytes.Repeat([]byte{' '}, tileHeaderSize)
	copy(hdr, b)
	hdr[tileHeaderSize-1] = '\n'
	return hdr, nil
}

// rewriteTileMeta replaces the header of an existing cache file in place.
func rewriteTileMeta(file string, m tileMeta) {
	hdr, err := encodeTileMeta(m)
	if err != nil {
		return
	}
	if f, err := os.OpenFile(file, os.O_WRONLY, 0); err == nil {
		_, _ = f.WriteAt(hdr, 0)
		_ = f.Close()
	}
}

// fetch downloads or revalidates one object; concurrent requests for the
// same object share one upstream request.
func (t *tileCache) fetch(ctx context.Context, key, file, target string, old tileMeta, haveOld bool) *tileCall {
	t.mu.Lock()
	if n, ok := t.negative[key]; ok && !haveOld {
		if time.Now().Before(n.until) {
			t.mu.Unlock()
			return &tileCall{status: n.status, msg: n.msg}
		}
		delete(t.negative, key)
	}
	if c, ok := t.inflight[key]; ok {
		t.mu.Unlock()
		select {
		case <-c.done:
			return c
		case <-ctx.Done():
			return &tileCall{status: 499, msg: "request cancelled"}
		}
	}
	c := &tileCall{done: make(chan struct{})}
	t.inflight[key] = c
	t.mu.Unlock()

	// Detached from the first client: the others still want the result.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tileFetchTimeout)
	go func() {
		defer cancel()
		defer func() {
			if v := recover(); v != nil {
				c.status, c.msg = http.StatusBadGateway, "tile cache: internal error"
			}
			t.mu.Lock()
			delete(t.inflight, key)
			if c.status != 0 && c.status != 499 {
				if len(t.negative) >= tileNegativeLimit {
					clear(t.negative)
				}
				t.negative[key] = tileNeg{c.status, c.msg, time.Now().Add(tileNegativeTTL)}
			}
			t.mu.Unlock()
			close(c.done)
		}()
		t.download(fctx, c, file, target, old, haveOld)
	}()
	select {
	case <-c.done:
		return c
	case <-ctx.Done():
		return &tileCall{status: 499, msg: "request cancelled"}
	}
}

// download performs the upstream request and fills c.
func (t *tileCache) download(ctx context.Context, c *tileCall, file, target string, old tileMeta, haveOld bool) {
	fail := func(status int, msg string) {
		t.errorsN.Add(1)
		if haveOld {
			// Better an expired tile than a hole in the map. The copy is
			// declared fresh for a few minutes so that an outage does not
			// cost one upstream attempt per tile request.
			c.how = "STALE"
			t.stale.Add(1)
			old.Exp = time.Now().Add(tileStaleRetry).Unix()
			rewriteTileMeta(file, old)
			return
		}
		c.status, c.msg = status, msg
	}
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		fail(http.StatusServiceUnavailable, "tile cache is busy")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		fail(http.StatusBadRequest, "invalid tile path")
		return
	}
	req.Header.Set("User-Agent", "AirView/"+t.s.opts.Version)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "gzip")
	if haveOld {
		if old.ETag != "" {
			req.Header.Set("If-None-Match", old.ETag)
		} else if old.LastMod != "" {
			req.Header.Set("If-Modified-Since", old.LastMod)
		}
	}
	resp, err := t.client.Do(req)
	if err != nil {
		fail(http.StatusBadGateway, "tile upstream unreachable")
		return
	}
	defer resp.Body.Close()
	now := time.Now()

	switch {
	case resp.StatusCode == http.StatusNotModified && haveOld:
		old.Exp = now.Add(tileTTL(resp.Header, old.CT, now)).Unix()
		old.Fetched = now.Unix()
		rewriteTileMeta(file, old)
		c.how = "REVALIDATED"
		t.revalidated.Add(1)
		return
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusGone:
		// Not an outage: the object does not exist (e.g. a tile outside the data set).
		c.status, c.msg = http.StatusNotFound, "tile not found upstream"
		return
	case resp.StatusCode != http.StatusOK:
		fail(http.StatusBadGateway, fmt.Sprintf("tile upstream answered %d", resp.StatusCode))
		return
	}
	ct := resp.Header.Get("Content-Type")
	if !tileContentAllowed(ct) {
		fail(http.StatusBadGateway, "tile upstream sent an unexpected content type")
		return
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "identity" {
		enc = ""
	}
	if enc != "" && enc != "gzip" {
		fail(http.StatusBadGateway, "tile upstream sent an unsupported encoding")
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, tileMaxObject+1))
	if err != nil {
		fail(http.StatusBadGateway, "tile download failed")
		return
	}
	if len(body) > tileMaxObject {
		fail(http.StatusBadGateway, "tile object is too large")
		return
	}
	meta := tileMeta{
		CT: ct, Enc: enc, ETag: resp.Header.Get("ETag"), LastMod: resp.Header.Get("Last-Modified"),
		Exp: now.Add(tileTTL(resp.Header, ct, now)).Unix(), Fetched: now.Unix(), Size: int64(len(body)),
	}
	if err := t.store(file, meta, body); err != nil {
		t.s.log.Warn("tile cache write failed", "err", err)
		fail(http.StatusInsufficientStorage, "tile cache cannot write to disk")
		return
	}
	c.how = "MISS"
	t.misses.Add(1)
}

// store writes a cache file atomically and keeps the size counters.
func (t *tileCache) store(file string, meta tileMeta, body []byte) error {
	hdr, err := encodeTileMeta(meta)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	var oldSize int64 = -1
	if info, err := os.Stat(file); err == nil {
		oldSize = info.Size()
	}
	tmp := file + "." + strconv.FormatInt(time.Now().UnixNano(), 36) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(hdr)
	if err == nil {
		_, err = f.Write(body)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, file)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	size := int64(len(hdr) + len(body))
	if oldSize >= 0 {
		t.bytes.Add(size - oldSize)
	} else {
		t.bytes.Add(size)
		t.files.Add(1)
	}
	if t.scanned.Load() && t.bytes.Load() > t.maxBytes {
		select {
		case t.evictC <- struct{}{}:
		default:
		}
	}
	return nil
}

// tileContentAllowed keeps active content (HTML, scripts, SVG) from ever
// being served from our origin.
func tileContentAllowed(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch ct {
	case "image/png", "image/jpeg", "image/webp", "image/gif", "image/avif",
		"application/x-protobuf", "application/vnd.mapbox-vector-tile", "application/protobuf",
		"application/octet-stream", "application/json", "application/geo+json", "text/plain",
		"application/font-sdf", "application/x-font-ttf", "application/font-woff", "application/pbf":
		return true
	}
	return strings.HasPrefix(ct, "font/")
}

// tileTTL derives the cache lifetime from the upstream headers, at least a
// day for tiles, glyphs and sprites.
func tileTTL(h http.Header, ct string, now time.Time) time.Duration {
	ttl := time.Duration(0)
	for _, d := range strings.Split(h.Get("Cache-Control"), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(d), "=")
		if strings.EqualFold(k, "max-age") {
			if n, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64); err == nil && n > 0 {
				ttl = time.Duration(min(n, int64(tileMaxTTL/time.Second))) * time.Second
			}
		}
	}
	if ttl == 0 {
		if exp, err := http.ParseTime(h.Get("Expires")); err == nil && exp.After(now) {
			ttl = exp.Sub(now)
		}
	}
	lo := tileMinTTL
	if strings.HasPrefix(strings.ToLower(ct), "application/json") {
		lo = tileMinTTLJSON
	}
	return min(max(ttl, lo), tileMaxTTL)
}

// tileCacheMaxMB is the cap of the disk cache: AIRVIEW_TILECACHE_MB
// (16..1048576) when set, else the storage profile's default (128 MB on
// low-wear, 512 MB elsewhere), else tileDefaultMaxMB.
func tileCacheMaxMB(profileMB int, env string) int64 {
	if v, err := strconv.ParseInt(env, 10, 64); err == nil && v >= 16 && v <= 1<<20 {
		return v
	}
	if profileMB >= 16 {
		return int64(profileMB)
	}
	return tileDefaultMaxMB
}
