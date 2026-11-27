package feederui

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"airview/internal/auth"
	"airview/internal/config"
	"airview/internal/model"
	"airview/internal/uplink"
)

//go:embed static
var staticFS embed.FS

// Options configure a Server.
type Options struct {
	Backend Backend
	// PasswordHash returns the current page password hash
	// (FeederUI.PasswordHash, config.HashPassword format). When it is
	// non-empty, every request from a non-loopback client needs HTTP Basic
	// authentication with that password (any user name). Loopback clients
	// never need it. nil = no password.
	PasswordHash func() string
	// Logf receives log lines (never secrets). nil = silent.
	Logf func(format string, args ...any)
	// ClaimTimeout bounds a claim request (default 20 s).
	ClaimTimeout time.Duration
}

// Server is the local status page and its JSON API (DESIGN §12.1):
//
//	GET  /                 the page (embedded HTML, CSS, JS)
//	GET  /api/status       Snapshot
//	GET  /api/health       {"ok":bool,"uplinks":n,"connected":n} (200, or 503 when no uplink is connected)
//	POST /api/claim        {"id":"<uplink>","rxNo":0} -> {"code","expiresMs"}
//	POST /api/retry        {"id":"<uplink>"}
//	GET  /api/settings     SettingsDoc
//	PUT  /api/settings     model.FeederConfig -> {"notes":[...]}
//
// Protection, because the page usually listens on 127.0.0.1 where any web
// site the user visits could otherwise reach it:
//   - the Host header must be an IP literal or "localhost" (defeats DNS
//     rebinding);
//   - every mutating request needs the header "X-AirView-Feeder: 1" (a
//     custom header forces a CORS preflight, which this server never
//     approves), and a cross-site Origin is refused;
//   - non-loopback clients need the password when one is set, and are
//     refused when none is set;
//   - responses are never cached; claim codes are never logged.
type Server struct {
	o   Options
	mux *http.ServeMux

	// Remote Basic auth (SEC-08): failures are throttled per client, and
	// successful checks are remembered for a while, because the page
	// polls and the hash is a slow KDF.
	fails  *auth.Throttle
	okMu   sync.Mutex
	okSeen map[[32]byte]time.Time
}

// authCacheTTL is how long a verified (hash, password) pair is trusted
// without running the KDF again.
const authCacheTTL = 10 * time.Minute

// CSRFHeader must be present (value "1") on every mutating request.
const CSRFHeader = "X-AirView-Feeder"

// New returns the page handler.
func New(o Options) *Server {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.ClaimTimeout <= 0 {
		o.ClaimTimeout = 20 * time.Second
	}
	s := &Server{o: o, mux: http.NewServeMux(), fails: auth.NewThrottle(5, 15*time.Minute, 5*time.Minute, 1024), okSeen: map[[32]byte]time.Time{}}
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(sub))
	s.mux.Handle("GET /", files)
	s.mux.HandleFunc("GET /api/status", s.status)
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("POST /api/claim", s.claim)
	s.mux.HandleFunc("POST /api/retry", s.retry)
	s.mux.HandleFunc("GET /api/settings", s.getSettings)
	s.mux.HandleFunc("PUT /api/settings", s.putSettings)
	return s
}

// ServeHTTP implements http.Handler with the checks described on Server.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	if !hostAllowed(r.Host) {
		http.Error(w, "unknown host name; open the page by IP address or as localhost", http.StatusMisdirectedRequest)
		return
	}
	if !isLoopback(r.RemoteAddr) {
		hash := ""
		if s.o.PasswordHash != nil {
			hash = s.o.PasswordHash()
		}
		if hash == "" {
			http.Error(w, "this page is available on this computer only", http.StatusForbidden)
			return
		}
		client := clientKey(r.RemoteAddr)
		if d := s.fails.Blocked(client); d > 0 {
			h.Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
			http.Error(w, "too many wrong passwords; try again later", http.StatusTooManyRequests)
			return
		}
		_, pw, ok := r.BasicAuth()
		if !ok || !s.checkPassword(hash, pw) {
			if ok {
				s.fails.Fail(client)
			}
			h.Set("WWW-Authenticate", `Basic realm="AirView Feeder", charset="UTF-8"`)
			http.Error(w, "password required", http.StatusUnauthorized)
			return
		}
		s.fails.Success(client)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get(CSRFHeader) != "1" {
			jsonErr(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
			jsonErr(w, http.StatusForbidden, "cross-site request refused")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// checkPassword verifies pw against hash, remembering successes for
// authCacheTTL (keyed by a digest of both, never the password itself).
func (s *Server) checkPassword(hash, pw string) bool {
	k := sha256.Sum256([]byte(hash + "\x00" + pw))
	now := time.Now()
	s.okMu.Lock()
	t, hit := s.okSeen[k]
	s.okMu.Unlock()
	if hit && now.Sub(t) < authCacheTTL {
		return true
	}
	if !config.CheckPassword(hash, pw) {
		return false
	}
	s.okMu.Lock()
	if len(s.okSeen) > 64 {
		clear(s.okSeen)
	}
	s.okSeen[k] = now
	s.okMu.Unlock()
	return true
}

// clientKey is the throttle key of a remote address (IPv4 address or IPv6
// /64).
func clientKey(remote string) string {
	h, _, err := net.SplitHostPort(remote)
	if err != nil {
		h = remote
	}
	return auth.ClientKeyString(h)
}

// hostAllowed accepts IP literals, localhost names and this machine's own
// host name (also with ".local", as mDNS/avahi advertises it, e.g. the Pi
// image's airview-feeder.local), with or without a port. Anything else
// could be a rebinding attacker's domain.
func hostAllowed(host string) bool {
	if host == "" {
		return true // HTTP/1.0 without Host: only reachable directly
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	return ownHostName(host)
}

// osHostname is os.Hostname (tests replace it).
var osHostname = os.Hostname

// ownHostName reports whether host (lower case) is this machine's host
// name, or that name + ".local". Read at request time: a renamed machine
// works without a restart.
func ownHostName(host string) bool {
	name, err := osHostname()
	if err != nil {
		return false
	}
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if name == "" || name == "localhost" {
		return false
	}
	short, _, _ := strings.Cut(name, ".")
	return host == name || host == short || host == short+".local"
}

func isLoopback(remote string) bool {
	h, _, err := net.SplitHostPort(remote)
	if err != nil {
		h = remote
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.Unmap().IsLoopback()
}

func sameOrigin(origin, host string) bool {
	o := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	return o == host
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.o.Backend.Snapshot())
}

// Health is the GET /api/health body.
type Health struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version"`
	FeederID  string `json:"feederId"`
	Uplinks   int    `json:"uplinks"`
	Connected int    `json:"connected"`
	Receivers int    `json:"receivers"`
	Inputs    int    `json:"inputsConnected"`
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	snap := s.o.Backend.Snapshot()
	h := Health{Version: snap.Version, FeederID: snap.FeederID, Uplinks: len(snap.Uplinks), Receivers: len(snap.Receivers)}
	for _, u := range snap.Uplinks {
		if u.State == uplink.StateConnected {
			h.Connected++
		}
	}
	for _, r := range snap.Receivers {
		if r.Connected {
			h.Inputs++
		}
	}
	h.OK = h.Uplinks > 0 && h.Connected == h.Uplinks
	code := http.StatusOK
	if !h.OK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, h)
}

// ClaimRequest is the POST /api/claim body.
type ClaimRequest struct {
	ID   string `json:"id"`
	RxNo int    `json:"rxNo"`
}

// ClaimResponse is the POST /api/claim answer.
type ClaimResponse struct {
	Code      string `json:"code"`
	ExpiresMs int64  `json:"expiresMs"`
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if !readJSON(w, r, &req, 4096) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.o.ClaimTimeout)
	defer cancel()
	code, exp, err := s.o.Backend.Claim(ctx, req.ID, req.RxNo)
	if err != nil {
		st := http.StatusConflict
		switch {
		case errors.Is(err, uplink.ErrNoUplink):
			st = http.StatusNotFound
		case errors.Is(err, context.DeadlineExceeded):
			st = http.StatusGatewayTimeout
		}
		jsonErr(w, st, err.Error())
		return
	}
	s.o.Logf("feederui: claim code issued for uplink %q receiver %d", req.ID, req.RxNo) // never the code
	writeJSON(w, http.StatusOK, ClaimResponse{Code: code, ExpiresMs: exp.UnixMilli()})
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if !readJSON(w, r, &req, 4096) {
		return
	}
	if err := s.o.Backend.Retry(req.ID); err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.o.Backend.Settings())
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	cfg := model.DefaultFeederConfig()
	cfg.Receivers, cfg.Uplinks = nil, nil
	if !readJSON(w, r, &cfg, 256<<10) {
		return
	}
	notes, err := s.o.Backend.SaveSettings(cfg)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.o.Logf("feederui: settings saved from the status page")
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		jsonErr(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(v); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
