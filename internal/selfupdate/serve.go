package selfupdate

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReleaseHandler serves a release directory written by the release tool
// (deploy/release, "avrelease publish") over HTTP, for an aggregator's
// /feeder/ routes (mount with http.StripPrefix("/feeder", h)):
//
//	GET /manifest.json            stable manifest
//	GET /manifest.json.sig        its signature
//	GET /dl/<target>              stable artifacts
//	GET /beta/manifest.json(.sig) beta channel
//	GET /beta/dl/<target>         beta artifacts
//
// Directory layout: <dir>/manifest.json, <dir>/manifest.json.sig,
// <dir>/dl/<target>, and the same below <dir>/beta/. Only these names are
// served (targets must match ValidTarget), so nothing else in the
// directory is reachable. Replies carry Cache-Control: no-cache; artifacts
// support Range requests (resumable downloads).
//
// The handler does not verify signatures (clients do); it only refuses to
// serve a manifest without its .sig.
type ReleaseHandler struct {
	Dir string
}

// ServeHTTP implements http.Handler.
func (h ReleaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel, ctype, ok := releasePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(h.Dir, filepath.FromSlash(rel))
	if strings.HasSuffix(rel, "manifest.json") {
		if _, err := os.Stat(full + ".sig"); err != nil {
			http.Error(w, "release not published", http.StatusNotFound)
			return
		}
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(ctype, "application/octet-stream") {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(full)))
	}
	http.ServeContent(w, r, "", info.ModTime().Truncate(time.Second), f)
}

// releasePath maps a request path to a file below the release directory.
func releasePath(p string) (rel, ctype string, ok bool) {
	p = strings.TrimPrefix(p, "/")
	prefix := ""
	if rest, found := strings.CutPrefix(p, ChannelBeta+"/"); found {
		prefix, p = ChannelBeta+"/", rest
	}
	switch {
	case p == "manifest.json":
		return prefix + p, "application/json", true
	case p == "manifest.json.sig":
		return prefix + p, "text/plain; charset=utf-8", true
	case strings.HasPrefix(p, "dl/") && ValidTarget(p[3:]):
		return prefix + p, "application/octet-stream", true
	}
	return "", "", false
}
