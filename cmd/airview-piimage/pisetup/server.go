package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// server is the first-boot setup page. It exists only until the feeder is
// configured, so its security model is narrow:
//
//   - clients must come from loopback, private (RFC 1918, ULA), link-local or
//     CGNAT (100.64/10) addresses;
//   - the Host header must be an IP literal, localhost, the host name, or the
//     host name under .local/.lan/.home/.home.arpa/.localdomain/.internal
//     (defeats DNS rebinding);
//   - a POST needs the per-process form token and, when the browser sends
//     one, a same-origin Origin header (CSRF);
//   - one setup runs at a time.
type server struct {
	a        *app
	token    string
	mu       sync.Mutex // one installation at a time
	finished chan values
	once     sync.Once
}

func newServer(a *app) *server {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return &server{a: a, token: hex.EncodeToString(b[:]), finished: make(chan values, 1)}
}

var localNets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

func clientAllowed(remote string) bool {
	h, _, err := net.SplitHostPort(remote)
	if err != nil {
		h = remote
	}
	ip, err := netip.ParseAddr(strings.Trim(h, "[]"))
	if err != nil {
		return false
	}
	ip = ip.Unmap().WithZone("")
	if ip.IsLoopback() {
		return true
	}
	for _, p := range localNets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

var localSuffixes = []string{"", ".local", ".lan", ".home", ".home.arpa", ".localdomain", ".internal"}

func hostAllowed(host, hostname string) bool {
	if host == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return true
	}
	hostname = strings.ToLower(hostname)
	if hostname == "" {
		return false
	}
	for _, s := range localSuffixes {
		if host == hostname+s {
			return true
		}
	}
	return false
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	if !clientAllowed(r.RemoteAddr) {
		http.Error(w, "The setup page answers only on the local network.", http.StatusForbidden)
		return
	}
	if !hostAllowed(r.Host, s.a.hostname()) {
		http.Error(w, "Unknown host name: open this page as http://"+s.a.hostname()+".local:8099/ or by the Pi's IP address.", http.StatusMisdirectedRequest)
		return
	}
	switch {
	case r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.render(w, http.StatusOK, pageData{V: s.a.initial, Err: s.a.initialErr})
	case r.URL.Path == "/setup" && r.Method == http.MethodPost:
		s.setup(w, r)
	case r.URL.Path == "/healthz":
		w.Write([]byte("setup\n"))
	default:
		http.NotFound(w, r)
	}
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host == host
}

func (s *server) setup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" && !sameOrigin(o, r.Host) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.token)) != 1 {
		http.Error(w, "The form has expired; reload the page.", http.StatusForbidden)
		return
	}
	if !s.mu.TryLock() {
		http.Error(w, "A setup is already running; wait a moment and reload.", http.StatusConflict)
		return
	}
	defer s.mu.Unlock()
	f := r.PostFormValue
	v := values{
		Server: f("server"), Pin: f("pin"), Token: f("token"), Name: f("name"),
		Lat: f("lat"), Lon: f("lon"), Alt: f("alt"), AltRef: f("altRef"), AGL: f("agl"),
		NoLocation: f("location") == "network", Input: f("input"), Privacy: f("privacy"),
		Timestamps: f("timestamps") != "no", AutoUpdate: f("autoUpdate") == "yes",
		Password: f("password"), Password2: f("password2"),
		// Not on the form: the release key and URL of the image (update.conf)
		// or of feeder-setup.txt.
		UpdateURL: s.a.initial.UpdateURL, ManifestKey: s.a.initial.ManifestKey,
	}
	fromFile := false
	if v.Token == "" && s.a.fileToken != "" {
		v.Token, fromFile = s.a.fileToken, true
	}
	if err := v.validate(true); err != nil {
		v.Password, v.Password2 = "", ""
		if fromFile {
			v.Token = ""
		}
		s.render(w, http.StatusUnprocessableEntity, pageData{V: v, Err: err.Error(), Field: fieldOf(err)})
		return
	}
	out, err := s.a.configure(r.Context(), v)
	if fromFile {
		v.Token = "" // never echo the file's token (SEC-R9)
	}
	if err != nil {
		v.Password, v.Password2 = "", ""
		s.render(w, http.StatusInternalServerError, pageData{V: v, Err: err.Error(), Log: out})
		return
	}
	s.render(w, http.StatusOK, pageData{Done: true, V: v, Addrs: s.a.addresses()})
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	s.once.Do(func() { s.finished <- v })
}

func fieldOf(err error) string {
	var fe fieldError
	if errors.As(err, &fe) {
		return fe.Field
	}
	return ""
}

type pageData struct {
	V values
	// TokenKept: feeder-setup.txt has an enrol token (not shown).
	TokenKept bool
	Err       string
	Field     string
	Log       string
	Done      bool
	Addrs     []string
	CSRF      string
	Host      string
	C1        string
	C2        []string
}

func (s *server) render(w http.ResponseWriter, code int, d pageData) {
	d.CSRF = s.token
	d.Host = s.a.hostname()
	d.TokenKept = s.a.fileToken != ""
	d.C1 = consentTimestamps
	d.C2 = consentPinLines
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := pageTmpl.Execute(w, d); err != nil {
		log.Printf("render: %v", err)
	}
}

// serve runs the page until a setup finished (returns its values) or ctx ends.
func (s *server) serve(ctx context.Context, ln net.Listener) (values, error) {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Minute, // the installer runs inside the request
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case v := <-s.finished:
		// Let the success page reach the browser, then free the port.
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return v, nil
	case err := <-errc:
		return values{}, fmt.Errorf("setup page: %w", err)
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return values{}, ctx.Err()
	}
}

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))
