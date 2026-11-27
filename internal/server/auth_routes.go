package server

import (
	"context"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	avauth "airview/internal/auth"
	"airview/internal/model"
)

// Account endpoints of aggregator mode (docs/aggregator/DESIGN.md §7).
//
// They are appended to the route table in init. Every handler answers 404
// in standalone mode, so registering them changes nothing there. None is
// Admin: each checks the session it needs itself, and the CSRF token is
// enforced for all of them by checkOrigin (routeHandler).

const grpAccounts = "Accounts (aggregator mode)"

// AuthRoutes returns the account routes for the route table.
func AuthRoutes() []Route { return authRoutes }

func init() {
	routes = append(routes, authRoutes...)
}

var authRoutes = []Route{
	{Method: "GET", Path: "/api/auth/setup", Group: grpAccounts, Access: AccessSession, Raw: rawAuthSetupState, NoAuto: true,
		Summary: "Whether the first admin account still has to be set up.",
		Returns: "`{\"required\":bool,\"tokenPending\":bool,\"tokenFile\":\"<path, loopback clients only>\",\"os\":\"linux\"}`"},
	{Method: "POST", Path: "/api/auth/setup/begin", Group: grpAccounts, Access: AccessSession, Raw: rawAuthSetupBegin, NoAuto: true, Credentials: true,
		Summary: "First admin setup, step 1: console token, user name and password; returns the authenticator secret.",
		Body:    "`{\"token\":\"<from the setup-token file in the data directory>\",\"username\":\"...\",\"password\":\"<12+ characters>\"}`",
		Returns: "`{\"secret\":\"BASE32\",\"uri\":\"otpauth://...\"}`"},
	{Method: "POST", Path: "/api/auth/setup/finish", Group: grpAccounts, Access: AccessSession, Raw: rawAuthSetupFinish, NoAuto: true, Credentials: true,
		Summary: "First admin setup, step 2: confirm the authenticator with a code; returns the recovery codes once.",
		Body:    "`{\"token\":\"...\",\"username\":\"...\",\"code\":\"123456\"}`",
		Returns: "`{\"recoveryCodes\":[\"XXXXX-XXXXX\", ...]}`"},
	{Method: "POST", Path: "/api/auth/register", Group: grpAccounts, Access: AccessSession, Raw: rawAuthRegister, NoAuto: true, Credentials: true,
		Summary: "Create an owner account (no e-mail) and log in.",
		Body:    "`{\"username\":\"3-32 of a-z 0-9 . _ -\",\"password\":\"<8+ characters>\"}`",
		Returns: "`sessionInfo` JSON and the session cookie. `409` when the name is taken, `429` after 5 sign-ups per hour from one address."},
	{Method: "POST", Path: "/api/auth/password", Group: grpAccounts, Access: AccessSession, Raw: rawAuthPassword, NoAuto: true, Credentials: true,
		Summary: "Change the own password; other sessions of the account end.",
		Body:    "`{\"current\":\"...\",\"new\":\"...\"}`"},
	{Method: "POST", Path: "/api/auth/totp/begin", Group: grpAccounts, Access: AccessSession, Raw: rawAuthTOTPBegin, NoAuto: true, Credentials: true,
		Summary: "Start authenticator (TOTP) enrolment; returns the secret and otpauth URI.",
		Body:    "`{\"password\":\"...\"}`"},
	{Method: "POST", Path: "/api/auth/totp/confirm", Group: grpAccounts, Access: AccessSession, Raw: rawAuthTOTPConfirm, NoAuto: true, Credentials: true,
		Summary: "Confirm the authenticator with a first code; returns 10 single-use recovery codes (shown once).",
		Body:    "`{\"code\":\"123456\"}`"},
	{Method: "POST", Path: "/api/auth/totp/disable", Group: grpAccounts, Access: AccessSession, Raw: rawAuthTOTPDisable, NoAuto: true, Credentials: true,
		Summary: "Turn the authenticator off (owners only; admins cannot).",
		Body:    "`{\"password\":\"...\",\"code\":\"123456 or a recovery code\"}`"},
	{Method: "POST", Path: "/api/auth/recovery-codes", Group: grpAccounts, Access: AccessSession, Raw: rawAuthRecovery, NoAuto: true, Credentials: true,
		Summary: "Replace all recovery codes.",
		Body:    "`{\"password\":\"...\",\"code\":\"123456\"}`"},
	{Method: "GET", Path: "/api/auth/sessions", Group: grpAccounts, Access: AccessSession, Raw: rawAuthSessions, NoAuto: true,
		Summary: "The own login sessions (no addresses are stored)."},
	{Method: "POST", Path: "/api/auth/logout-others", Group: grpAccounts, Access: AccessSession, Raw: rawAuthLogoutOthers, NoAuto: true,
		Summary: "End every other session of the own account.", Returns: "`{\"ended\":n}`"},
}

// netOnly answers 404 outside aggregator mode.
func (s *Server) netOnly(w http.ResponseWriter) bool {
	if !s.aggregatorMode() {
		writeError(w, http.StatusNotFound, "only available in aggregator mode")
		return false
	}
	return true
}

// netRequire returns the request's session or answers 401.
func (s *Server) netRequire(w http.ResponseWriter, r *http.Request) (avauth.Session, avauth.Account, bool) {
	if !s.netOnly(w) {
		return avauth.Session{}, avauth.Account{}, false
	}
	sess, acct, ok := s.netSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "login required")
	}
	return sess, acct, ok
}

func authCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), loginTimeout)
}

func rawAuthSetupState(s *Server, w http.ResponseWriter, r *http.Request) {
	if !s.netOnly(w) {
		return
	}
	svc := s.AccountService()
	ready, err := svc.AdminReady(r.Context())
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	out := setupState{Required: !ready, TokenPending: svc.BootstrapPending(), OS: runtime.GOOS}
	// UX-03: the token is kept in a file in the data directory (it is
	// printed only on an interactive console). The machine itself is told
	// where; remote clients only that it is "setup-token" in the data
	// directory.
	if !ready && out.TokenPending && isLoopback(r) && s.eng != nil && s.eng.DataDir() != "" {
		out.TokenFile = filepath.Join(s.eng.DataDir(), setupTokenFileName)
	}
	writeJSON(w, r, http.StatusOK, out)
}

// setupTokenFileName is the token file in the data directory (written by
// cmd/airview, accounts.go).
const setupTokenFileName = "setup-token"

// setupState is GET /api/auth/setup.
type setupState struct {
	Required     bool   `json:"required"`
	TokenPending bool   `json:"tokenPending"`
	TokenFile    string `json:"tokenFile,omitempty"` // loopback clients only
	OS           string `json:"os"`                  // server OS (wording of the hints)
}

func rawAuthSetupBegin(s *Server, w http.ResponseWriter, r *http.Request) {
	if !s.netOnly(w) {
		return
	}
	var in struct{ Token, Username, Password string }
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := authCtx(r)
	defer cancel()
	setup, err := s.AccountService().BootstrapBegin(ctx, in.Token, s.remoteIP(r), in.Username, in.Password)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, setup)
}

func rawAuthSetupFinish(s *Server, w http.ResponseWriter, r *http.Request) {
	if !s.netOnly(w) {
		return
	}
	var in struct{ Token, Username, Code string }
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	codes, err := s.AccountService().BootstrapFinish(r.Context(), in.Token, s.remoteIP(r), in.Username, in.Code)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	s.log.Warn("aggregator admin account set up", "user", in.Username)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

func rawAuthRegister(s *Server, w http.ResponseWriter, r *http.Request) {
	if !s.netOnly(w) {
		return
	}
	var in struct{ Username, Password string }
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := authCtx(r)
	defer cancel()
	cl := s.client(r)
	svc := s.AccountService()
	acct, err := svc.Register(ctx, in.Username, in.Password, cl.String())
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	res, err := svc.Login(ctx, avauth.LoginRequest{Username: acct.Username, Password: in.Password, Client: cl.String()})
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	http.SetCookie(w, netCookie(res.Token, max(int(time.Until(res.Session.Expires).Seconds()), 1), cl.Secure))
	s.log.Info("owner account created", "user", acct.Username)
	writeJSON(w, r, http.StatusOK, sessionInfo{
		OK: true, Aggregator: true, PasswordSet: true, LoggedIn: true,
		Scope: res.Session.Scope.String(), Username: acct.Username, CSRFToken: res.Session.CSRF,
	})
}

func rawAuthPassword(s *Server, w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.netRequire(w, r)
	if !ok {
		return
	}
	var in struct{ Current, New string }
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := authCtx(r)
	defer cancel()
	if err := s.AccountService().ChangePassword(ctx, &sess, in.Current, in.New); err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]bool{"ok": true})
}

func rawAuthTOTPBegin(s *Server, w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.netRequire(w, r)
	if !ok {
		return
	}
	var in struct{ Password string }
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := authCtx(r)
	defer cancel()
	setup, err := s.AccountService().BeginTOTP(ctx, &sess, in.Password)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, setup)
}

func rawAuthTOTPConfirm(s *Server, w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.netRequire(w, r)
	if !ok {
		return
	}
	var in struct{ Code string }
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	codes, err := s.AccountService().ConfirmTOTP(r.Context(), &sess, in.Code)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

func rawAuthTOTPDisable(s *Server, w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.netRequire(w, r)
	if !ok {
		return
	}
	var in struct{ Password, Code string }
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := authCtx(r)
	defer cancel()
	if err := s.AccountService().DisableTOTP(ctx, &sess, in.Password, in.Code); err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]bool{"ok": true})
}

func rawAuthRecovery(s *Server, w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.netRequire(w, r)
	if !ok {
		return
	}
	var in struct{ Password, Code string }
	if err := readJSON(w, r, &in, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := authCtx(r)
	defer cancel()
	codes, err := s.AccountService().RegenerateRecovery(ctx, &sess, in.Password, in.Code)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

// sessionBrief is one entry of GET /api/auth/sessions.
type sessionBrief struct {
	ID       string `json:"id"` // first 12 hex digits of the session hash
	Scope    string `json:"scope"`
	Created  int64  `json:"created"`  // Unix ms
	LastSeen int64  `json:"lastSeen"` // Unix ms (updated at most once a minute)
	Expires  int64  `json:"expires"`  // Unix ms
	Current  bool   `json:"current"`  // the session making this request
}

func rawAuthSessions(s *Server, w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.netRequire(w, r)
	if !ok {
		return
	}
	list, err := s.AccountService().Sessions().List(r.Context(), sess.AccountID)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	now := time.Now()
	out := make([]sessionBrief, 0, len(list))
	for _, x := range list {
		if !x.Valid(now) {
			continue
		}
		out = append(out, sessionBrief{
			ID: x.ID[:12], Scope: x.Scope.String(), Created: x.Created.UnixMilli(),
			LastSeen: x.LastSeen.UnixMilli(), Expires: x.Expires.UnixMilli(), Current: x.ID == sess.ID,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, map[string]any{"sessions": out})
}

func rawAuthLogoutOthers(s *Server, w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.netRequire(w, r)
	if !ok {
		return
	}
	n, err := s.AccountService().LogoutOthers(r.Context(), sess.AccountID, sess.ID)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]int{"ended": n})
}

// requireScope reports whether the request's viewer has at least need; it
// answers 401 (no session) or 403 (insufficient scope) otherwise. For
// WP-H's scoped routes in aggregator mode.
func (s *Server) requireScope(w http.ResponseWriter, r *http.Request, need model.Scope) (model.Viewer, bool) {
	v := s.viewer(r)
	if v.Scope.Allows(need) {
		return v, true
	}
	if v.Scope == model.ScopePublic {
		writeError(w, http.StatusUnauthorized, "login required")
	} else {
		writeError(w, http.StatusForbidden, "not allowed for this account")
	}
	return v, false
}
