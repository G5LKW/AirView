// Package web embeds the AirView front-end (web/static) into the binary.
//
// The UI is plain static files: index.html, hand-written ES modules, CSS and
// vendored libraries (MapLibre GL JS, Apache ECharts, two OFL fonts). There is
// no build step.
//
// Use [Static] for an fs.FS rooted at the site root, or [Handler] for a ready
// http.Handler that serves it with correct MIME types (ES modules and module
// workers are refused by browsers unless they carry a JavaScript content type,
// and the platform MIME tables - notably the Windows registry - are not
// reliable for .js/.mjs), ETags and gzip.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// FS holds the embedded front-end; every asset lives under the "static/" prefix.
//
//go:embed all:static
var FS embed.FS

// Static returns the front-end file system rooted at the site root, so that
// "index.html", "js/app.js", "vendor/..." are top-level names.
func Static() fs.FS {
	sub, err := fs.Sub(FS, "static")
	if err != nil {
		// Unreachable: "static" is a compile-time embedded directory.
		panic("web: embedded static directory missing: " + err.Error())
	}
	return sub
}

// contentTypes maps the file extensions used by the front-end to MIME types.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".pbf":   "application/x-protobuf",
}

// ContentType returns the MIME type for a front-end asset name based on its
// extension, or "application/octet-stream" when the extension is unknown.
// Extension-less files (LICENSE, NOTICE) are served as plain text.
func ContentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return "text/plain; charset=utf-8"
	}
	if ct, ok := contentTypes[ext]; ok {
		return ct
	}
	return "application/octet-stream"
}

func compressible(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".css", ".js", ".mjs", ".json", ".svg", ".txt", ".md", ".map", "":
		return true
	}
	return false
}

// asset is one embedded file prepared for serving.
type asset struct {
	data []byte
	gz   []byte // nil when not compressible or not worth it
	etag string
	ct   string
}

type handler struct {
	root  fs.FS
	mu    sync.Mutex
	cache map[string]*asset // bounded by the number of embedded files
}

// Handler returns an http.Handler serving the embedded front-end at "/".
//
// "/" serves index.html. Only GET and HEAD are allowed. Responses carry a
// strong ETag (content hash) and "Cache-Control: no-cache" so browsers
// revalidate cheaply and pick up a new build immediately; text assets are
// gzip-compressed once and cached in memory. Unknown paths return 404 (the UI
// uses hash routing, so no history fallback is needed).
func Handler() http.Handler {
	return &handler{root: Static(), cache: make(map[string]*asset)}
}

func (h *handler) load(name string) (*asset, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a, ok := h.cache[name]; ok {
		return a, nil
	}
	data, err := fs.ReadFile(h.root, name)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	a := &asset{
		data: data,
		etag: `"` + hex.EncodeToString(sum[:12]) + `"`,
		ct:   ContentType(name),
	}
	if compressible(name) && len(data) > 1024 {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write(data); err == nil && zw.Close() == nil && buf.Len() < len(data)*9/10 {
			a.gz = buf.Bytes()
		}
	}
	h.cache[name] = a
	return a, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	a, err := h.load(name)
	if err != nil {
		// Missing file or a directory: no listings.
		http.NotFound(w, r)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", a.ct)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("ETag", a.etag)
	hdr.Add("Vary", "Accept-Encoding")
	if etagMatch(r.Header.Get("If-None-Match"), a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.data
	if a.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		hdr.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func etagMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		if part == etag || part == "*" {
			return true
		}
	}
	return false
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			continue
		}
		q := strings.TrimSpace(params)
		if strings.HasPrefix(q, "q=") {
			if v, err := strconv.ParseFloat(strings.TrimPrefix(q, "q="), 64); err == nil && v == 0 {
				return false
			}
		}
		return true
	}
	return false
}
