package server

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	avauth "airview/internal/auth"
	"airview/internal/model"
)

// Aggregator-mode authentication (docs/aggregator/DESIGN.md §7):
//
//   - Accounts (internal/auth): owners (user name + password, TOTP
//     optional) and admins (password + mandatory TOTP). Sessions are
//     scoped: public (no session), owner, admin.
//   - Loopback is never admin; there is no "no password = open" rule.
//   - The client address honours X-Forwarded-For only from
//     Aggregator.TrustedProxies (s.remoteIP); rate limits use it.
//   - Session cookie: HttpOnly; Secure when the request arrived over TLS
//     (directly or per X-Forwarded-Proto from a trusted proxy);
//     SameSite=Strict.
//   - Every non-GET request made with a session cookie must carry the
//     session's CSRF token in X-CSRF-Token (checkOrigin -> netCheckCSRF).
//
// The account and session stores come from SetAccountService (wired by the
// engine to the stations.db registry). Without that call an in-memory
// service is used, which forgets accounts on restart.

// netAuth is the aggregator half of the server's auth state.
type netAuth struct {
	mu  sync.Mutex
	svc *avauth.Service

	proxyKey string
	proxy    *avauth.ProxyTrust

	// Test hooks: force the mode / trusted proxies regardless of config
	// (config validation decides which modes a config may hold).
	testMode    string
	testProxies []string
	// testInsecureOK (tests) accepts credentials over plain HTTP from any
	// client, like http.allowInsecureLogin.
	testInsecureOK bool
}

func (n *netAuth) service() *avauth.Service {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.svc == nil {
		n.svc = avauth.NewService(avauth.NewMemoryAccounts(), avauth.NewMemorySessions(0, 0), avauth.Options{})
	}
	return n.svc
}

func (n *netAuth) current() *avauth.Service {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.svc
}

func (n *netAuth) dropAll() {
	if svc := n.current(); svc != nil {
		_ = svc.Sessions().DeleteAll(context.Background())
	}
}

func (n *netAuth) count() int {
	if svc := n.current(); svc != nil {
		if c, err := svc.Sessions().Count(context.Background()); err == nil {
			return c
		}
	}
	return 0
}

// SetAccountService installs the account / session service used in
// aggregator mode (built by the engine over the registry's persistent
// stores). It may be called before Start or at any time later; existing
// in-memory sessions are dropped.
func (s *Server) SetAccountService(svc *avauth.Service) {
	s.auth.net.mu.Lock()
	s.auth.net.svc = svc
	s.auth.net.mu.Unlock()
}

// AccountService returns the aggregator account service (creating the
// in-memory fallback if none was installed).
func (s *Server) AccountService() *avauth.Service { return s.auth.net.service() }

// aggregatorMode reports whether the aggregator auth rules apply.
func (s *Server) aggregatorMode() bool {
	if m := s.auth.net.testMode; m != "" {
		return m == model.ModeAggregator
	}
	if s.eng == nil {
		return false
	}
	h := s.eng.Config()
	return h != nil && h.Get().Mode == model.ModeAggregator
}

// proxyTrust returns the parsed Aggregator.TrustedProxies (cached until
// the list changes). Standalone mode trusts no proxy.
func (s *Server) proxyTrust() *avauth.ProxyTrust {
	if !s.aggregatorMode() {
		return nil
	}
	list := s.auth.net.testProxies
	if list == nil && s.eng != nil {
		if h := s.eng.Config(); h != nil {
			list = h.Get().Aggregator.TrustedProxies
		}
	}
	key := strings.Join(list, ",")
	n := &s.auth.net
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.proxy == nil || n.proxyKey != key {
		n.proxy, n.proxyKey = avauth.MustTrustedProxies(list), key
	}
	return n.proxy
}

// client resolves the request's origin (trusted-proxy aware in aggregator
// mode; the TCP peer in standalone mode).
func (s *Server) client(r *http.Request) avauth.Client {
	return s.proxyTrust().Resolve(r)
}

// remoteIP is the client address for rate limits, per-IP caps and
// feed-check matching: X-Forwarded-For is honoured only from trusted
// proxies in aggregator mode; otherwise it is the TCP peer (clientIP).
func (s *Server) remoteIP(r *http.Request) string {
	if c := s.client(r); c.IP.IsValid() {
		return c.IP.String()
	}
	return clientIP(r)
}

// logClient is the client address for log lines: the peer address in
// standalone mode; in aggregator mode raw addresses are not logged
// (DESIGN §8.5), only whether the request came from the machine itself.
func (s *Server) logClient(r *http.Request) string {
	if !s.aggregatorMode() {
		return clientIP(r)
	}
	if isLoopback(r) {
		return "local"
	}
	return "remote"
}

// netSession returns the aggregator session of the request's cookie.
func (s *Server) netSession(r *http.Request) (avauth.Session, avauth.Account, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return avauth.Session{}, avauth.Account{}, false
	}
	sess, acct, err := s.auth.net.service().Authenticate(r.Context(), c.Value)
	if err != nil {
		return avauth.Session{}, avauth.Account{}, false
	}
	return sess, acct, true
}

func (s *Server) netIsAdmin(r *http.Request) bool {
	sess, _, ok := s.netSession(r)
	return ok && sess.Scope == model.ScopeAdmin && sess.MFA
}

// viewer returns who a response is produced for (DESIGN §8.4): in
// aggregator mode the session's scope and owner (public without a valid
// session); in standalone mode every client sees all data (only
// mutations are restricted), so the viewer is admin-scoped.
func (s *Server) viewer(r *http.Request) model.Viewer {
	if !s.aggregatorMode() {
		return model.Viewer{Scope: model.ScopeAdmin}
	}
	sess, _, ok := s.netSession(r)
	if !ok {
		return model.PublicViewer
	}
	if sess.Scope == model.ScopeAdmin && !sess.MFA {
		return model.PublicViewer
	}
	return sess.Viewer()
}

// netCheckCSRF enforces the CSRF token for cookie-authenticated mutating
// requests. A request without a (valid) session cookie passes here: it has
// no authority to abuse and is refused later if the route needs a session.
func (s *Server) netCheckCSRF(r *http.Request) string {
	if avauth.SafeMethod(r.Method) {
		return ""
	}
	sess, _, ok := s.netSession(r)
	if !ok {
		return ""
	}
	if !avauth.CheckCSRF(r, sess.CSRF) {
		return "CSRF token missing or wrong: send the csrfToken from GET /api/session as the " + avauth.CSRFHeader + " header"
	}
	return ""
}

func netCookie(tok string, maxAge int, secure bool) *http.Cookie {
	return &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	}
}

func (s *Server) netSessionInfo(r *http.Request) sessionInfo {
	info := sessionInfo{OK: true, Aggregator: true, PasswordSet: true, Scope: model.ScopePublic.String()}
	if sess, acct, ok := s.netSession(r); ok {
		info.LoggedIn = true
		info.Scope, info.Username, info.CSRFToken = sess.Scope.String(), sess.Username, sess.CSRF
		info.Admin = sess.Scope == model.ScopeAdmin && sess.MFA
		info.TOTP = acct.TOTPEnabled()
	}
	if ready, err := s.auth.net.service().AdminReady(r.Context()); err == nil {
		info.SetupRequired = !ready
	}
	return info
}

// writeAuthError maps internal/auth errors to HTTP replies.
func (s *Server) writeAuthError(w http.ResponseWriter, err error) {
	var te *avauth.ThrottledError
	switch {
	case errors.As(err, &te):
		w.Header().Set("Retry-After", strconv.Itoa(int(te.RetryAfter.Seconds())+1))
		status := http.StatusTooManyRequests
		msg := te.Error()
		// A failure that started a throttle still reports the failure.
		if errors.Is(err, avauth.ErrBadCredentials) || errors.Is(err, avauth.ErrBadCode) {
			status = http.StatusUnauthorized
			msg = strings.SplitN(err.Error(), " (", 2)[0]
		}
		writeError(w, status, msg)
	case errors.Is(err, avauth.ErrBadCredentials), errors.Is(err, avauth.ErrBadCode),
		errors.Is(err, avauth.ErrBootstrapToken):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, avauth.ErrAdminNoTOTP), errors.Is(err, avauth.ErrAccountDisabled),
		errors.Is(err, avauth.ErrTOTPMandatory), errors.Is(err, avauth.ErrAdminExists),
		errors.Is(err, avauth.ErrNotAdmin), errors.Is(err, avauth.ErrRegistrationOff):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, avauth.ErrUsernameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, avauth.ErrUsernameInvalid), errors.Is(err, avauth.ErrPasswordTooShort),
		errors.Is(err, avauth.ErrPasswordTooLong), errors.Is(err, avauth.ErrPasswordInvalid),
		errors.Is(err, avauth.ErrNoPendingTOTP), errors.Is(err, avauth.ErrTOTPNotEnabled):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, avauth.ErrBusy):
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, avauth.ErrSessionNotFound):
		writeError(w, http.StatusUnauthorized, "login required")
	default:
		s.log.Error("auth error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// errInsecureLogin is the answer to a credential request refused by
// insecureCredentials.
const errInsecureLogin = "refusing a password over plain HTTP from the network: use HTTPS " +
	"(a reverse proxy listed in aggregator.trustedProxies that sets X-Forwarded-Proto), " +
	"run 'airview admin init' on the server, or set http.allowInsecureLogin"

// insecureCredentials reports whether a credential-bearing request must be
// refused (SEC-01): aggregator mode, the client is not the machine itself
// and the request did not arrive over TLS (directly or per a trusted
// proxy's X-Forwarded-Proto), unless http.allowInsecureLogin is set.
// Without this, passwords, TOTP codes, the setup token and the session
// cookie would cross the internet in clear text.
func (s *Server) insecureCredentials(r *http.Request) bool {
	if !s.aggregatorMode() || s.auth.net.testInsecureOK {
		return false
	}
	if s.eng != nil && s.eng.EffectiveConfig().HTTP.AllowInsecureLogin {
		return false
	}
	cl := s.client(r)
	if cl.Secure {
		return false
	}
	// SEC-R8: only the machine itself (loopback peer, no forwarding
	// headers) counts as local. A request relayed by a local reverse proxy
	// that is not in aggregator.trustedProxies keeps the proxy's loopback
	// address as its IP, but it comes from the network.
	return !cl.Local()
}

// loginTimeout bounds how long a login waits for a PBKDF2 slot.
const loginTimeout = 10 * time.Second

func (s *Server) netLogin(w http.ResponseWriter, r *http.Request) {
	// Login is exempt from checkOrigin (there is no session yet), but a
	// foreign page must not log the browser into an account either.
	if crossOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cl := s.client(r)
	ctx, cancel := context.WithTimeout(r.Context(), loginTimeout)
	defer cancel()
	res, err := s.auth.net.service().Login(ctx, avauth.LoginRequest{
		Username: in.Username, Password: in.Password, Code: in.Code, Client: cl.String(),
	})
	if errors.Is(err, avauth.ErrCodeRequired) {
		info := s.netSessionInfo(r)
		info.CodeRequired = true
		writeJSON(w, r, http.StatusUnauthorized, struct {
			Error string `json:"error"`
			sessionInfo
		}{err.Error(), info})
		return
	}
	if err != nil {
		if errors.Is(err, avauth.ErrBadCredentials) || errors.Is(err, avauth.ErrBadCode) {
			// No raw IPs in logs (DESIGN §8.4).
			s.log.Warn("login failed", "reason", strings.SplitN(err.Error(), " (", 2)[0])
			time.Sleep(s.auth.failDelay)
		}
		s.writeAuthError(w, err)
		return
	}
	maxAge := int(time.Until(res.Session.Expires).Seconds())
	http.SetCookie(w, netCookie(res.Token, max(maxAge, 1), cl.Secure))
	s.log.Info("login", "user", res.Account.Username, "scope", res.Session.Scope.String(), "recovery", res.UsedRecovery)
	info := sessionInfo{
		OK: true, Aggregator: true, PasswordSet: true, LoggedIn: true,
		Admin: res.Session.Scope == model.ScopeAdmin && res.Session.MFA,
		Scope: res.Session.Scope.String(), Username: res.Account.Username,
		CSRFToken: res.Session.CSRF, TOTP: res.Account.TOTPEnabled(),
	}
	if res.UsedRecovery {
		left := res.RecoveryLeft
		info.RecoveryLeft = &left
	}
	writeJSON(w, r, http.StatusOK, info)
}

func (s *Server) netLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.auth.net.service().Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, netCookie("", -1, s.client(r).Secure))
	info := sessionInfo{OK: true, Aggregator: true, PasswordSet: true, Scope: model.ScopePublic.String()}
	writeJSON(w, r, http.StatusOK, info)
}

// isLoopbackBind reports whether a listen address binds loopback only.
func isLoopbackBind(addr string) bool {
	switch strings.TrimSpace(addr) {
	case "localhost", "::1", "[::1]":
		return true
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(addr))
	return err == nil && ip.IsLoopback()
}
