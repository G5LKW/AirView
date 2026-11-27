package server

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	avauth "airview/internal/auth"
	"airview/internal/config"
)

// Authentication has two regimes (see docs/ARCHITECTURE.md and
// docs/aggregator/DESIGN.md §7):
//
// Standalone (and remote-*) mode - unchanged:
//
//   - requests from this machine (loopback peer, not proxied) are admin;
//   - without http.adminPasswordHash everybody on the LAN is admin;
//   - with it, other clients need a session cookie from POST /api/login.
//
// Sessions live in memory only: a restart logs everybody out. A session is
// bound to the password hash it was created under, so changing or clearing
// the password ends all sessions.
//
// Aggregator mode (auth_net.go): accounts with scoped sessions (owner /
// admin), admin needs password + TOTP, loopback is never admin,
// X-Forwarded-For only from trusted proxies, CSRF token on every mutating
// cookie request.
//
// In both regimes a request that carries forwarding headers is not "from
// this machine", even when the TCP peer is loopback: a reverse proxy on the
// same host would otherwise make every internet client admin.

const (
	sessionCookie  = "airview_session"
	sessionTTL     = 30 * 24 * time.Hour
	maxSessions    = 512
	loginFree      = 5                // failures before the throttle starts
	loginWindow    = 15 * time.Minute // failures are forgotten after this
	loginMaxDelay  = 5 * time.Minute
	maxThrottleIPs = 2048
	// loginGlobalFree is the number of failed admin logins from all
	// addresses together within loginWindow before every address waits
	// (SEC-R7: a /64 holds 2^64 addresses, the password is only one).
	loginGlobalFree = 50
)

type session struct {
	hash    string // password hash the session was created under
	csrf    string // CSRF token (checked when the client sends one)
	expires time.Time
}

type throttle struct {
	fails int
	last  time.Time
	until time.Time // no attempt accepted before this
}

type auth struct {
	mu       sync.Mutex
	sessions map[string]session
	throttle map[string]*throttle
	// global counts the failed logins of all clients (SEC-R7).
	global throttle
	// failDelay slows down password guessing (shortened in tests).
	failDelay time.Duration

	// Aggregator mode (auth_net.go).
	net netAuth
}

func newAuth() *auth {
	return &auth{
		sessions: map[string]session{}, throttle: map[string]*throttle{},
		failDelay: 300 * time.Millisecond,
	}
}

// clientIP is the peer address. It does not honour X-Forwarded-For (that
// would let anybody claim to be loopback); in aggregator mode use
// s.remoteIP, which honours it from configured trusted proxies only.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLoopback reports whether the request comes from this machine: the peer
// is a loopback address and the request was not relayed by a proxy (no
// X-Forwarded-For / Forwarded / X-Real-IP / X-Forwarded-Host header).
func isLoopback(r *http.Request) bool {
	ip := net.ParseIP(clientIP(r))
	return ip != nil && ip.IsLoopback() && !avauth.HasForwardingHeaders(r)
}

func (s *Server) passwordHash() string {
	h := s.eng.Config()
	if h == nil {
		return ""
	}
	return h.Get().HTTP.AdminPasswordHash
}

// isAdmin reports whether the client may use the mutating endpoints.
//
// Standalone mode:
//   - a request sent by a page of another site (Origin naming another host,
//     Sec-Fetch-Site cross-site / same-site) is never admin, so a foreign
//     page cannot read admin-scope data through this machine's loopback or
//     password-less LAN access (SEC-R3);
//   - loopback (not proxied) is admin;
//   - without an admin password only private-network peers (RFC 1918,
//     IPv6 ULA, link-local; not CGNAT) that were not relayed by a proxy
//     are admin, unless http.allowOpenAdmin is set: a server exposed to
//     the internet without a password must not hand configuration,
//     factory reset and the database to everybody (SEC-R2);
//   - otherwise a valid login session is needed.
func (s *Server) isAdmin(r *http.Request) bool {
	if s.aggregatorMode() {
		return s.netIsAdmin(r)
	}
	if crossOrigin(r) {
		return false
	}
	if isLoopback(r) {
		return true
	}
	hash := s.passwordHash()
	if hash == "" {
		return s.openAdmin(r)
	}
	return s.auth.valid(r, hash)
}

// openAdmin reports whether a client is admin on a standalone server
// without an admin password (SEC-R2).
func (s *Server) openAdmin(r *http.Request) bool {
	if s.eng != nil {
		if h := s.eng.Config(); h != nil && h.Get().HTTP.AllowOpenAdmin {
			return true
		}
	}
	if avauth.HasForwardingHeaders(r) {
		return false
	}
	return privatePeer(clientIP(r))
}

// privatePeer reports whether ip is a loopback, private (RFC 1918, IPv6
// ULA) or link-local address.
func privatePeer(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()
}

func (a *auth) lookup(r *http.Request, hash string) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	se, ok := a.sessions[c.Value]
	if !ok {
		return session{}, false
	}
	if se.hash != hash || time.Now().After(se.expires) {
		delete(a.sessions, c.Value)
		return session{}, false
	}
	return se, true
}

func (a *auth) valid(r *http.Request, hash string) bool {
	_, ok := a.lookup(r, hash)
	return ok
}

func (a *auth) create(hash string) string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sessions) >= maxSessions {
		var oldest string
		var oldestT time.Time
		for k, se := range a.sessions {
			if now.After(se.expires) {
				delete(a.sessions, k)
			} else if oldest == "" || se.expires.Before(oldestT) {
				oldest, oldestT = k, se.expires
			}
		}
		if len(a.sessions) >= maxSessions {
			delete(a.sessions, oldest)
		}
	}
	a.sessions[tok] = session{hash: hash, csrf: avauth.NewCSRFToken(), expires: now.Add(sessionTTL)}
	return tok
}

func (a *auth) drop(tok string) {
	a.mu.Lock()
	delete(a.sessions, tok)
	a.mu.Unlock()
}

// dropAll ends every session (factory reset), including aggregator ones.
func (a *auth) dropAll() {
	a.mu.Lock()
	clear(a.sessions)
	a.mu.Unlock()
	a.net.dropAll()
}

// count returns the number of login sessions (standalone + aggregator).
func (a *auth) count() int {
	a.mu.Lock()
	n := len(a.sessions)
	a.mu.Unlock()
	return n + a.net.count()
}

// blocked returns how long ip still has to wait before the next attempt.
func (a *auth) blocked(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	var g time.Duration
	if a.global.fails > 0 && now.Sub(a.global.last) <= loginWindow {
		g = max(0, a.global.until.Sub(now))
	}
	t := a.throttle[ip]
	if t == nil {
		return g
	}
	if now.Sub(t.last) > loginWindow {
		delete(a.throttle, ip)
		return g
	}
	return max(g, t.until.Sub(now))
}

// fail records a wrong password and returns the wait imposed (0 while the
// free attempts last): 2 s, 4 s, 8 s ... capped at loginMaxDelay.
func (a *auth) fail(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	t := a.throttle[ip]
	if t == nil || now.Sub(t.last) > loginWindow {
		if len(a.throttle) >= maxThrottleIPs {
			for k, v := range a.throttle {
				if now.Sub(v.last) > loginWindow {
					delete(a.throttle, k)
				}
			}
			// SEC-R7: flooded from many addresses: drop the oldest entries,
			// never the whole table (that reset every client's wait).
			for len(a.throttle) >= maxThrottleIPs {
				oldest, at := "", now
				for k, v := range a.throttle {
					if oldest == "" || v.last.Before(at) {
						oldest, at = k, v.last
					}
				}
				delete(a.throttle, oldest)
			}
		}
		t = &throttle{}
		a.throttle[ip] = t
	}
	d := t.fail(now, loginFree)
	// SEC-R7: one admin password, one budget: every failure from any
	// address also counts against a global allowance.
	if a.global.fails > 0 && now.Sub(a.global.last) > loginWindow {
		a.global = throttle{}
	}
	return max(d, a.global.fail(now, loginGlobalFree))
}

// fail counts one failure and returns the wait imposed once free failures
// were used up: 2 s, 4 s, 8 s ... capped at loginMaxDelay.
func (t *throttle) fail(now time.Time, free int) time.Duration {
	t.fails++
	t.last = now
	if t.fails < free {
		return 0
	}
	d := time.Second << min(t.fails-free+1, 12)
	d = min(d, loginMaxDelay)
	t.until = now.Add(d)
	return d
}

func (a *auth) success(ip string) {
	a.mu.Lock()
	delete(a.throttle, ip)
	a.mu.Unlock()
}

// sessionInfo is the reply of the login, logout and session endpoints.
// The fields after LoggedIn are additions for aggregator mode and for the
// CSRF token; they are omitted when empty so standalone clients see the
// same document as before.
type sessionInfo struct {
	OK          bool `json:"ok"`
	Admin       bool `json:"admin"`       // this client may change settings now
	Loopback    bool `json:"loopback"`    // the request came from the machine itself (never in aggregator mode)
	PasswordSet bool `json:"passwordSet"` // an admin password is configured (aggregator: always true)
	LoggedIn    bool `json:"loggedIn"`    // a valid session cookie was presented

	// CSRFToken must be sent as the X-CSRF-Token header on every
	// non-GET request made with this session (required in aggregator mode,
	// checked when present in standalone mode). Only present while logged in.
	CSRFToken string `json:"csrfToken,omitempty"`
	// Aggregator is true in aggregator mode.
	Aggregator bool `json:"aggregator,omitempty"`
	// Scope is "public", "owner" or "admin" (aggregator mode).
	Scope string `json:"scope,omitempty"`
	// Username of the logged-in account (aggregator mode).
	Username string `json:"username,omitempty"`
	// TOTP is true when the account has an authenticator (aggregator mode).
	TOTP bool `json:"totp,omitempty"`
	// CodeRequired: the password was right, now send it again with `code`
	// (TOTP or recovery code). Only in a 401 login reply.
	CodeRequired bool `json:"codeRequired,omitempty"`
	// SetupRequired: aggregator mode has no admin with an authenticator
	// yet; finish setup with `airview admin init` or the console token.
	SetupRequired bool `json:"setupRequired,omitempty"`
	// RecoveryLeft is the number of unused recovery codes, sent after a
	// login that used one.
	RecoveryLeft *int `json:"recoveryLeft,omitempty"`
}

func (s *Server) sessionInfo(r *http.Request) sessionInfo {
	if s.aggregatorMode() {
		return s.netSessionInfo(r)
	}
	hash := s.passwordHash()
	info := sessionInfo{
		OK: true, Admin: s.isAdmin(r), Loopback: isLoopback(r),
		PasswordSet: hash != "",
	}
	if hash != "" {
		if se, ok := s.auth.lookup(r, hash); ok {
			info.LoggedIn, info.CSRFToken = true, se.csrf
		}
	}
	return info
}

func apiSession(s *Server, r *http.Request) (any, error) {
	return s.sessionInfo(r), nil
}

func rawLogin(s *Server, w http.ResponseWriter, r *http.Request) {
	if s.aggregatorMode() {
		s.netLogin(w, r)
		return
	}
	ip := clientIP(r)
	// SEC-R7: one IPv6 /64 is one client (an attacker holds 2^64 addresses).
	key := avauth.ClientKeyString(ip)
	if d := s.auth.blocked(key); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed logins, try again later")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash := s.passwordHash()
	if hash == "" {
		// Nothing to log in to: private-network clients are admin, the
		// others are not (SEC-R2; sessionInfo says which).
		writeJSON(w, r, http.StatusOK, s.sessionInfo(r))
		return
	}
	if !config.CheckPassword(hash, in.Password) {
		d := s.auth.fail(key)
		s.log.Warn("admin login failed", "client", ip)
		time.Sleep(s.auth.failDelay) // slows guessing
		if d > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		}
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	s.auth.success(key)
	tok := s.auth.create(hash)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
	s.log.Info("admin login", "client", ip)
	info := s.sessionInfo(r)
	info.Admin, info.LoggedIn = true, true
	s.auth.mu.Lock()
	info.CSRFToken = s.auth.sessions[tok].csrf
	s.auth.mu.Unlock()
	writeJSON(w, r, http.StatusOK, info)
}

func rawLogout(s *Server, w http.ResponseWriter, r *http.Request) {
	if s.aggregatorMode() {
		s.netLogout(w, r)
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.auth.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	info := s.sessionInfo(r)
	info.LoggedIn = false
	info.CSRFToken = ""
	info.Admin = info.Loopback || !info.PasswordSet
	writeJSON(w, r, http.StatusOK, info)
}

// localSuffixes are host-name endings that cannot be public DNS names.
var localSuffixes = []string{".local", ".lan", ".home", ".home.arpa", ".internal", ".localdomain", ".localhost", ".fritz.box", ".test"}

// localHostName reports whether host (no port) can only name a machine on
// the local network: an IP literal, a single-label name, a local suffix or
// this machine's own host name.
func localHostName(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return true
	}
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suf := range localSuffixes {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	if hn, err := os.Hostname(); err == nil && strings.EqualFold(hn, host) {
		return true
	}
	return false
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// crossOrigin applies the browser-origin rules shared by both regimes: a
// cross-origin Origin header, or Sec-Fetch-Site cross-site / same-site,
// refuses the request. Requests without Origin (curl, scripts) pass.
func crossOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if o == "null" || err != nil || !strings.EqualFold(u.Host, r.Host) {
			return true
		}
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return true
	}
	return false
}

// checkOrigin guards the mutating endpoints against requests forged by
// other web pages. It returns "" when the request is acceptable.
//
//   - Cross-site request forgery: a browser sends Origin with every
//     cross-origin POST / PUT; it must name the host the request was sent to.
//     Sec-Fetch-Site is checked as well.
//   - CSRF token: in aggregator mode every request authenticated by a
//     session cookie must carry the session's token in X-CSRF-Token
//     (auth_net.go). In standalone mode a token, when sent, must match.
//   - DNS rebinding (standalone): a public name that resolves to this
//     machine makes a foreign page same-origin with us, and loopback or
//     password-less LAN access would make it admin. The Host header must
//     therefore be an IP literal or a name that can only exist locally -
//     unless the client presents a valid login session, which a rebinding
//     page cannot have. Aggregator mode has no such ambient authority
//     (loopback is not admin, everything needs a session), so the rule does
//     not apply there.
//
// Requests without Origin (curl, scripts) are not browser page requests and
// only have to pass the Host rule.
func (s *Server) checkOrigin(r *http.Request) string {
	const refused = "cross-origin request refused"
	if crossOrigin(r) {
		return refused
	}
	if s.aggregatorMode() {
		return s.netCheckCSRF(r)
	}
	hash := s.passwordHash()
	se, loggedIn := session{}, false
	if hash != "" {
		se, loggedIn = s.auth.lookup(r, hash)
	}
	if loggedIn && r.Header.Get(avauth.CSRFHeader) != "" && !avauth.CheckCSRF(r, se.csrf) {
		return "CSRF token does not match the session"
	}
	if !localHostName(hostOnly(r.Host)) && !loggedIn {
		return "this host name is not accepted for changes: use the IP address, or set an admin password and log in"
	}
	return ""
}
