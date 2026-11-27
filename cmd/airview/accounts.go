package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"airview/internal/auth"
)

// Aggregator account wiring (DESIGN §7).
//
// The engine owns the station registry (<data>/stations.db) and the account
// service in aggregator mode (internal/engine/network.go): persistent
// accounts and sessions, the session janitor, the registry jobs and the
// admin gate of the feeder listeners. This file installs the service in the
// HTTP server and, while no admin with an authenticator exists, keeps a
// one-time setup token in <data>/setup-token (mode 0600, readable only by
// the service account and root). The token itself goes to the console only
// when stdout is an interactive terminal: under systemd stdout is the
// journal, which every member of adm / systemd-journal can read (SEC-09).
// The token expires after auth.BootstrapTTL and is renewed while no admin
// exists; the file is removed once the admin is set up.
func (a *app) wireAccounts(ctx context.Context) {
	svc := a.eng.Accounts()
	if svc == nil {
		return
	}
	a.srv.SetAccountService(svc)

	ready, err := svc.AdminReady(ctx)
	if err != nil {
		return
	}
	path := filepath.Join(a.dir, setupTokenFile)
	if ready {
		_ = os.Remove(path)
		return
	}
	if !a.mintSetupToken(ctx, svc, path, true) {
		return
	}
	go a.renewSetupToken(ctx, svc, path)
}

// setupTokenFile holds the console setup token (in the data directory).
const setupTokenFile = "setup-token"

// mintSetupToken creates a token, writes it to path and announces it.
func (a *app) mintSetupToken(ctx context.Context, svc *auth.Service, path string, announce bool) bool {
	tok, err := svc.MintBootstrapToken(ctx)
	if err != nil {
		a.log.Error("admin setup token", "err", err)
		return false
	}
	if err := writeSecretFile(path, tok+"\n"); err != nil {
		a.log.Error("admin setup token: cannot write the token file", "path", path, "err", err)
	}
	if !announce {
		return true
	}
	a.log.Warn("no admin account yet: the feeder listeners stay closed; the one-time setup token is in the token file (or run \"airview admin init\")", "file", path)
	who := "root and the service account"
	if runtime.GOOS == "windows" {
		who = "administrators and the service account"
	}
	shown := "in the file " + path + " (readable by " + who + " only)"
	if isTerminal(os.Stdout) {
		shown = tok + " (also in " + path + ")"
	}
	fmt.Fprintf(os.Stdout, "\nAirView aggregator: no admin account exists yet, so the feeder listeners stay closed.\n"+
		"One-time setup token: %s\n"+
		"Set up the first admin (password + authenticator) with POST %sapi/auth/setup/begin and %sapi/auth/setup/finish\n"+
		"(see docs/API.md, \"Accounts\"), or run \"airview admin init\" on this host.\n"+
		"The token is valid for %s (renewed in the same file while no admin exists) or until it is used.\n\n",
		shown, a.url, a.url, auth.BootstrapTTL)
	return true
}

// renewSetupToken re-mints the token shortly before it expires while no
// admin exists, and removes the token file once one does.
func (a *app) renewSetupToken(ctx context.Context, svc *auth.Service, path string) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if ready, err := svc.AdminReady(ctx); err == nil && ready {
			_ = os.Remove(path)
			return
		}
		if !svc.BootstrapPending() {
			a.mintSetupToken(ctx, svc, path, false)
			a.log.Info("admin setup token renewed", "file", path)
		}
	}
}

// writeSecretFile writes data to path with mode 0600 (atomically).
func writeSecretFile(path, data string) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	return os.Rename(tmp, path)
}
