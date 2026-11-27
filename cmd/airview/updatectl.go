package main

// updateCtl is the self-update as the web page and the automatic loop see
// it (internal/selfupdate; WP-W3-wire). It reads the update block of the
// live configuration on every use, so switching automatic updates or the
// channel on the settings page takes effect without a restart.
//
//   - Status and "Check now" work everywhere (desktop window included).
//   - Installing (the "Install" button and the automatic loop) needs a
//     headless run whose program file is writable by the running user:
//     the new version is swapped in, the program restarts itself
//     (selfupdate.SelfRestarter: execve on POSIX, a new process on
//     Windows), and the new process confirms itself after its health
//     window or the previous version is restored (attachUpdate / Attach).
//     A desktop window or a read-only program directory (the systemd
//     service user under /opt) gets the reason and the command to run
//     instead ("sudo airview update").

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"

	"airview/internal/model"
	"airview/internal/selfupdate"
	"airview/internal/server"
)

type updateCtl struct {
	dir      string
	log      *slog.Logger
	settings func() model.UpdateSettings // the live update block
	// install is false for the desktop window (it never replaces itself).
	install bool
	// restarter restarts into the new version (headless only).
	restarter selfupdate.Restarter
	exe       string
	// exitReq asks the main loop to end (Windows: the new process runs).
	exitReq func()
	// writable reports whether the program file can be replaced
	// (selfupdate.CheckWritable; replaced in tests).
	writable func(exe string) error

	mu      sync.Mutex
	busy    bool
	result  *selfupdate.Result
	lastErr string
}

func (c *updateCtl) updater() (*selfupdate.Updater, error) {
	s := selfupdate.SettingsFromModel(c.settings())
	return selfupdate.New(selfupdate.Options{
		Product: "airview", CurrentVersion: version, Settings: s, StateDir: c.dir, Exe: c.exe,
		Restarter: c.restarter,
		Logf:      func(f string, a ...any) { c.log.Info(fmt.Sprintf(f, a...)) },
	})
}

// canInstall reports whether this run may install, and if not, why.
func (c *updateCtl) canInstall(u *selfupdate.Updater) (bool, string) {
	switch {
	case !u.Configured():
		return false, "Set the release manifest URL and the release key (update.manifestUrl, update.keys) first."
	case !c.install:
		return false, "The desktop app does not replace itself while its window is open. Close AirView and run \"airview update\" with the rights to replace the program, or install the new version by hand."
	case c.restarter == nil:
		return false, "This run cannot restart itself."
	}
	w := c.writable
	if w == nil {
		w = selfupdate.CheckWritable
	}
	if err := w(c.exe); err != nil {
		cmd := "airview update"
		if runtime.GOOS != "windows" {
			cmd = "sudo airview update"
		}
		return false, fmt.Sprintf("The program file cannot be replaced by this user (%v). Run \"%s\" instead (it restarts the service and rolls back on failure).", err, cmd)
	}
	return true, ""
}

func (c *updateCtl) state(u *selfupdate.Updater) server.UpdateState {
	st := server.UpdateState{Supported: true}
	if u == nil {
		st.Status = selfupdate.Status{Product: "airview", Version: version, Target: selfupdate.CurrentTarget()}
		st.InstallNote = "The update settings are not valid (see update.* in \"airview config check\")."
	} else {
		st.Status = u.Status()
		st.Configured = u.Configured()
		st.CanInstall, st.InstallNote = c.canInstall(u)
	}
	c.mu.Lock()
	st.Busy, st.Result, st.ActionError = c.busy, c.result, c.lastErr
	c.mu.Unlock()
	return st
}

// UpdateState implements server.UpdateController.
func (c *updateCtl) UpdateState() server.UpdateState {
	u, _ := c.updater()
	return c.state(u)
}

// CheckUpdate implements server.UpdateController.
func (c *updateCtl) CheckUpdate(ctx context.Context) (server.UpdateState, error) {
	u, err := c.updater()
	if err != nil {
		return c.state(nil), err
	}
	if !u.Configured() {
		return c.state(u), selfupdate.ErrNotConfigured
	}
	_, err = u.Check(ctx)
	return c.state(u), err
}

// StartInstall implements server.UpdateController: the install runs in
// the background; the program restarts when it is done.
func (c *updateCtl) StartInstall() (server.UpdateState, error) {
	u, err := c.updater()
	if err != nil {
		return c.state(nil), err
	}
	if ok, why := c.canInstall(u); !ok {
		return c.state(u), fmt.Errorf("%w: %s", server.ErrUpdateNotPossible, why)
	}
	c.mu.Lock()
	if c.busy {
		c.mu.Unlock()
		return c.state(u), server.ErrUpdateBusy
	}
	c.busy, c.lastErr, c.result = true, "", nil
	c.mu.Unlock()
	// A quick check first, so "nothing newer" is answered at once.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	rel, err := u.Check(ctx)
	cancel()
	if err == nil && !rel.Newer {
		err = server.ErrUpToDate
	}
	if err != nil {
		c.finish(nil, err)
		return c.state(u), err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res, err := u.Update(ctx, selfupdate.UpdateOptions{})
		c.finish(&res, err)
	}()
	return c.state(u), nil
}

// finish records the outcome of an install (also of the automatic loop).
func (c *updateCtl) finish(res *selfupdate.Result, err error) {
	c.mu.Lock()
	c.busy, c.result = false, res
	c.lastErr = ""
	if err != nil && !errors.Is(err, selfupdate.ErrExitRequested) {
		c.lastErr = err.Error()
	}
	c.mu.Unlock()
	switch {
	case errors.Is(err, selfupdate.ErrExitRequested):
		c.log.Info("self-update: the new version runs; exiting")
		if c.exitReq != nil {
			c.exitReq()
		}
	case err != nil:
		c.log.Warn("self-update", "err", err)
	case res != nil && res.Outcome != selfupdate.OutcomeUpToDate:
		c.log.Info("self-update", "outcome", res.Outcome, "from", res.From, "to", res.To)
	}
}

// loop runs the automatic updates while update.enabled is on (checked on
// every round, so the switch on the settings page works at once): a first
// round 2-15 minutes after the start, then every checkIntervalMin ±25 %.
func (c *updateCtl) loop(ctx context.Context) {
	delay := 2*time.Minute + time.Duration(rand.Int64N(int64(13*time.Minute)))
	for {
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		s := selfupdate.SettingsFromModel(c.settings())
		iv := s.Normalized().CheckInterval()
		delay = iv*3/4 + time.Duration(rand.Int64N(int64(iv/2)+1))
		if !s.Enabled {
			continue
		}
		u, err := c.updater()
		if err != nil || !u.Configured() {
			continue
		}
		if ok, why := c.canInstall(u); !ok {
			// Still check, so the page and the log show what is available.
			if rel, err := u.Check(ctx); err == nil && rel.Newer {
				c.log.Info("self-update: a newer version is available but cannot be installed automatically", "version", rel.Manifest.Version, "why", why)
			}
			continue
		}
		c.mu.Lock()
		if c.busy {
			c.mu.Unlock()
			continue
		}
		c.busy = true
		c.mu.Unlock()
		res, err := u.Update(ctx, selfupdate.UpdateOptions{})
		c.finish(&res, err)
		if errors.Is(err, selfupdate.ErrExitRequested) {
			return
		}
	}
}
