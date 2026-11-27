package server

// Self-update status and actions (internal/selfupdate; WP-W3-wire). The
// program (cmd/airview) supplies an UpdateController through
// Options.Updater; the settings themselves (update.enabled = automatic,
// update.channel, update.manifestUrl, update.keys) are ordinary
// configuration, edited through PUT /api/config like every other block.
//
// Admin only: the version and the last error say nothing secret, but an
// install restarts the program.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"airview/internal/selfupdate"
)

func init() {
	routes = append(routes, updateRoutes...)
}

var updateRoutes = []Route{
	{Method: "GET", Path: "/api/update", Group: grpSystem, Access: AccessAdmin, API: apiUpdateStatus, NoAuto: true,
		Summary: "Self-update state: running version, channel, automatic updates on/off, last check and its error, the newer version available, the last update transaction, and whether this installation can install updates itself.",
		Returns: "`UpdateState` (`supported`, `configured`, `canInstall`, `installNote`, `busy`, `product`, `version`, `channel`, `enabled`, `lastCheck`, `lastError`, `available`, `last`, `result`)"},
	{Method: "POST", Path: "/api/update/check", Group: grpSystem, Access: AccessAdmin, Admin: true, API: apiUpdateCheck, NoAuto: true,
		Summary: "Check the release manifest now (signature verified; nothing is installed).",
		Returns: "`UpdateState`; `409` when update.manifestUrl / update.keys are not set"},
	{Method: "POST", Path: "/api/update/install", Group: grpSystem, Access: AccessAdmin, Admin: true, API: apiUpdateInstall, NoAuto: true,
		Summary: "Download, verify and install the newer version, then restart into it. The new version confirms itself after its health window (update.healthWindowS) or the previous one is restored.",
		Returns: "`202` with `UpdateState` (`busy`: the install runs; the program restarts when it is done); `409` when this installation cannot install itself (`installNote` says why) or nothing newer exists",
		Notes:   "Poll `GET /api/update` and `/healthz` (its `version`) to follow it. Audited in the log."},
}

// UpdateState is the self-update state shown by the settings page.
type UpdateState struct {
	selfupdate.Status
	// Supported: this program has a self-update (false: no controller).
	Supported bool `json:"supported"`
	// Configured: update.manifestUrl and update.keys are set.
	Configured bool `json:"configured"`
	// CanInstall: this installation may replace its own program file and
	// restart (headless, writable program directory). InstallNote says
	// why not, and what to do instead.
	CanInstall  bool   `json:"canInstall"`
	InstallNote string `json:"installNote,omitempty"`
	// Busy: a check or an install started from here is running.
	Busy bool `json:"busy"`
	// Result is the outcome of the last action started from here.
	Result *selfupdate.Result `json:"result,omitempty"`
	// ActionError is the error of the last action started from here.
	ActionError string `json:"actionError,omitempty"`
}

// UpdateController is the program's self-update, as the server needs it.
type UpdateController interface {
	// UpdateState returns the current state (cheap: files in the data
	// directory, no network).
	UpdateState() UpdateState
	// CheckUpdate fetches and verifies the manifest.
	CheckUpdate(ctx context.Context) (UpdateState, error)
	// StartInstall starts the install in the background; it returns
	// ErrUpdateNotPossible (wrapped, with the reason) when this
	// installation cannot install, or ErrUpToDate.
	StartInstall() (UpdateState, error)
}

// Errors of UpdateController.StartInstall.
var (
	ErrUpdateNotPossible = errors.New("this installation cannot install updates itself")
	ErrUpToDate          = errors.New("no newer version is available")
	ErrUpdateBusy        = errors.New("an update is already running")
)

func (s *Server) updater() UpdateController { return s.opts.Updater }

func apiUpdateStatus(s *Server, r *http.Request) (any, error) {
	u := s.updater()
	if u == nil {
		return UpdateState{Status: selfupdate.Status{Product: "airview", Version: s.opts.Version}}, nil
	}
	st := u.UpdateState()
	st.Supported = true
	return st, nil
}

func apiUpdateCheck(s *Server, r *http.Request) (any, error) {
	u := s.updater()
	if u == nil {
		return nil, errStatus(http.StatusConflict, "this program has no self-update")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	st, err := u.CheckUpdate(ctx)
	st.Supported = true
	switch {
	case errors.Is(err, selfupdate.ErrNotConfigured):
		return nil, errStatus(http.StatusConflict, "%v", err)
	case err != nil:
		// The failure is part of the state (lastError); the page shows it.
		st.ActionError = err.Error()
	}
	return st, nil
}

func apiUpdateInstall(s *Server, r *http.Request) (any, error) {
	u := s.updater()
	if u == nil {
		return nil, errStatus(http.StatusConflict, "this program has no self-update")
	}
	st, err := u.StartInstall()
	st.Supported = true
	switch {
	case errors.Is(err, ErrUpdateNotPossible), errors.Is(err, ErrUpToDate), errors.Is(err, ErrUpdateBusy),
		errors.Is(err, selfupdate.ErrNotConfigured):
		return nil, errStatus(http.StatusConflict, "%v", err)
	case err != nil:
		return nil, err
	}
	s.log.Info("self-update install started from the web page", "from", st.Version, "to", st.Available)
	return accepted(st), nil
}
