package main

// Self-update on the local status page (WP-W3-wire): the routes below sit
// in front of internal/feederui and give the page (and scripts on this
// machine) the update state, "Check now" and, where the feeder may replace
// itself, "Install now":
//
//	GET  /api/update          state (version, channel, automatic on/off, last check, available, last transaction)
//	POST /api/update/check    fetch and verify the manifest now
//	POST /api/update/install  install the newer version and restart into it
//	GET  /update              the "Software updates" page (updatepage/)
//
// The channel and the automatic switch are ordinary settings (autoUpdate,
// update.channel) saved through PUT /api/settings.
//
// Protection: stricter than the status page itself. Only loopback clients
// (the update routes never accept the LAN password), the Host header must be
// an IP literal or localhost (DNS rebinding), and the POSTs need the
// "X-AirView-Feeder: 1" header with no cross-site Origin (CSRF).
//
// Where installs run: Linux / macOS "airview-feeder run" with a writable
// program file. The Pi installer's service (AIRVIEW_FEEDER_UPDATER=external,
// read-only /opt) installs from its root timer or "sudo airview-feeder-setup
// --update"; the Windows service and tray from the scheduled task or the
// tray's "Check for updates" (administrator rights). The page says so.

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"airview/internal/selfupdate"
)

// updateState is the JSON of GET /api/update.
type updateState struct {
	selfupdate.Status
	Configured  bool               `json:"configured"`
	AutoUpdate  bool               `json:"autoUpdate"`
	CanInstall  bool               `json:"canInstall"`
	InstallNote string             `json:"installNote,omitempty"`
	Busy        bool               `json:"busy"`
	Result      *selfupdate.Result `json:"result,omitempty"`
	ActionError string             `json:"actionError,omitempty"`
}

// updateAPI wraps the status page handler.
type updateAPI struct {
	f    *feeder
	next http.Handler

	mu      sync.Mutex
	busy    bool
	result  *selfupdate.Result
	lastErr string
	// writable replaces selfupdate.CheckWritable (tests).
	writable func(string) error
	// goos replaces runtime.GOOS (tests).
	goos string
}

func newUpdateAPI(f *feeder, next http.Handler) *updateAPI {
	return &updateAPI{f: f, next: next, goos: runtime.GOOS}
}

func (a *updateAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	page := r.URL.Path == "/update" || strings.HasPrefix(r.URL.Path, "/update/")
	if !page && r.URL.Path != "/api/update" && !strings.HasPrefix(r.URL.Path, "/api/update/") {
		a.next.ServeHTTP(w, r)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if page {
		a.servePage(w, r)
		return
	}
	if !updHostAllowed(r.Host) {
		updJSONErr(w, http.StatusMisdirectedRequest, "unknown host name; open the page by IP address or as localhost")
		return
	}
	if !updLoopback(r.RemoteAddr) {
		updJSONErr(w, http.StatusForbidden, "updates can be managed on this computer only")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("X-AirView-Feeder") != "1" {
			updJSONErr(w, http.StatusForbidden, "missing X-AirView-Feeder header")
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://") != r.Host {
				updJSONErr(w, http.StatusForbidden, "cross-site request refused")
				return
			}
		}
	}
	switch {
	case r.URL.Path == "/api/update" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		updJSON(w, http.StatusOK, a.state(a.updater()))
	case r.URL.Path == "/api/update/check" && r.Method == http.MethodPost:
		u, err := a.updater()
		if err == nil && !u.Configured() {
			err = selfupdate.ErrNotConfigured
		}
		if err != nil {
			updJSONErr(w, http.StatusConflict, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		_, cerr := u.Check(ctx)
		cancel()
		st := a.state(u, nil)
		if cerr != nil {
			st.ActionError = cerr.Error()
		}
		updJSON(w, http.StatusOK, st)
	case r.URL.Path == "/api/update/install" && r.Method == http.MethodPost:
		a.install(w)
	default:
		updJSONErr(w, http.StatusNotFound, "not found")
	}
}

// updatePageFS is the "Software updates" page (GET /update): version,
// last check, Check now / Install now, and the autoUpdate and
// update.channel settings (saved through the status page's PUT
// /api/settings). Same protection as the update routes: loopback clients
// and an IP-literal or localhost Host only.
//
//go:embed updatepage
var updatePageFS embed.FS

func (a *updateAPI) servePage(w http.ResponseWriter, r *http.Request) {
	if !updHostAllowed(r.Host) {
		http.Error(w, "unknown host name; open the page by IP address or as localhost", http.StatusMisdirectedRequest)
		return
	}
	if !updLoopback(r.RemoteAddr) {
		http.Error(w, "updates can be managed on this computer only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var name, ctype string
	switch r.URL.Path {
	case "/update", "/update/":
		name, ctype = "updatepage/index.html", "text/html; charset=utf-8"
	case "/update/page.js":
		name, ctype = "updatepage/page.js", "text/javascript; charset=utf-8"
	case "/update/page.css":
		name, ctype = "updatepage/page.css", "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	b, err := updatePageFS.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(b)
}

// updater builds an updater from the current configuration (so a changed
// channel applies at once). Its restarter is the in-process one.
func (a *updateAPI) updater() (*selfupdate.Updater, error) {
	f := a.f
	f.cfgMu.Lock()
	eff := f.lc.Eff
	f.cfgMu.Unlock()
	defaultManifestURL(&eff)
	o := selfupdate.Options{
		Product: "airview-feeder", CurrentVersion: version, StateDir: f.o.DataDir,
		Settings: selfupdate.FeederSettings(eff.AutoUpdate, eff.Update), Logf: f.o.Logf,
	}
	if exe, err := selfupdate.ExecutablePath(); err == nil {
		o.Exe = exe
		o.Restarter = selfupdate.SelfRestarter{Exe: exe, Before: f.stop, Exit: func() { f.requestExit(0) }}
	}
	return selfupdate.New(o)
}

// canInstall reports whether this process may install, and if not, why.
func (a *updateAPI) canInstall(u *selfupdate.Updater) (bool, string) {
	if !u.Configured() {
		return false, "Updates are not set up: the installer writes update.manifestUrl and update.keys when it knows the aggregator."
	}
	switch {
	case a.goos == "windows":
		return false, "On Windows the daily \"AirView Feeder update\" task installs updates; use \"Check for updates\" in the tray menu to install now (it asks for administrator rights)."
	case os.Getenv("AIRVIEW_FEEDER_UPDATER") == "external":
		return false, "This feeder is updated by its installer's daily timer. To install now run: sudo airview-feeder-setup --update"
	}
	w := a.writable
	if w == nil {
		w = selfupdate.CheckWritable
	}
	exe, err := selfupdate.ExecutablePath()
	if err == nil {
		err = w(exe)
	}
	if err != nil {
		return false, fmt.Sprintf("The program file cannot be replaced by this user (%v). Run \"sudo airview-feeder update\" instead.", err)
	}
	return true, ""
}

func (a *updateAPI) state(u *selfupdate.Updater, err error) updateState {
	f := a.f
	f.cfgMu.Lock()
	auto := f.lc.Eff.AutoUpdate
	f.cfgMu.Unlock()
	st := updateState{AutoUpdate: auto}
	if err != nil || u == nil {
		st.Status = selfupdate.Status{Product: "airview-feeder", Version: version, Target: selfupdate.CurrentTarget()}
		if err != nil {
			st.InstallNote = "The update settings are not valid: " + err.Error()
		}
	} else {
		st.Status = u.Status()
		st.Configured = u.Configured()
		st.CanInstall, st.InstallNote = a.canInstall(u)
	}
	a.mu.Lock()
	st.Busy, st.Result, st.ActionError = a.busy, a.result, a.lastErr
	a.mu.Unlock()
	return st
}

func (a *updateAPI) install(w http.ResponseWriter) {
	u, err := a.updater()
	if err != nil {
		updJSONErr(w, http.StatusConflict, err.Error())
		return
	}
	if ok, why := a.canInstall(u); !ok {
		updJSONErr(w, http.StatusConflict, why)
		return
	}
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		updJSONErr(w, http.StatusConflict, "an update is already running")
		return
	}
	a.busy, a.result, a.lastErr = true, nil, ""
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	rel, err := u.Check(ctx)
	cancel()
	if err == nil && !rel.Newer {
		err = errors.New("no newer version is available")
	}
	if err != nil {
		a.finish(nil, err)
		updJSONErr(w, http.StatusConflict, err.Error())
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res, err := u.Update(ctx, selfupdate.UpdateOptions{})
		a.finish(&res, err)
	}()
	a.f.o.Logf("self-update: install of %s started from the status page", rel.Manifest.Version)
	updJSON(w, http.StatusAccepted, a.state(u, nil))
}

func (a *updateAPI) finish(res *selfupdate.Result, err error) {
	a.mu.Lock()
	a.busy, a.result, a.lastErr = false, res, ""
	if err != nil && !errors.Is(err, selfupdate.ErrExitRequested) {
		a.lastErr = err.Error()
	}
	a.mu.Unlock()
	if err != nil && !errors.Is(err, selfupdate.ErrExitRequested) {
		a.f.o.Logf("self-update: %v", err)
	}
}

func updHostAllowed(host string) bool {
	if host == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

func updLoopback(remote string) bool {
	h, _, err := net.SplitHostPort(remote)
	if err != nil {
		h = remote
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.Unmap().IsLoopback()
}

func updJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func updJSONErr(w http.ResponseWriter, code int, msg string) {
	updJSON(w, code, map[string]string{"error": msg})
}
