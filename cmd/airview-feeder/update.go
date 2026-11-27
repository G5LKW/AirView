package main

// Signed self-update (internal/selfupdate; WIRING.md §3 there). The
// "update" command installs a release published by the aggregator
// (update.manifestUrl, update.keys in feeder.json; written by the
// installers), restarts the service and rolls back when the new version
// does not come up healthy. In "run" and the Windows service / tray the
// feeder finishes and confirms a pending install itself; on Linux it also
// runs the automatic check when autoUpdate is on and the program directory
// is writable (otherwise the root update timer installs).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"time"

	"airview/internal/model"
	"airview/internal/selfupdate"
)

// updateService is the service the update command restarts.
func updateService() string {
	switch runtime.GOOS {
	case "windows":
		return "AirViewFeeder"
	case "linux":
		return "airview-feeder"
	}
	return ""
}

func feederHealthURL(eff model.FeederConfig) string {
	addr := eff.UI.Addr
	if addr == "" {
		addr = model.DefaultFeederUIAddr
	}
	return "http://" + addr + "/api/health"
}

// cmdUpdate implements "airview-feeder update [-config F] [-data D] [update flags]".
func cmdUpdate(args []string, stdout, stderr io.Writer) int {
	data, cfgFile, rest := splitDataConfig(args)
	c := common{config: cfgFile, data: data}
	if c.data == "" {
		c.data = os.Getenv(envDataDir)
	}
	if c.config == "" {
		c.config = os.Getenv(envConfigFile)
	}
	c.resolve()
	lc, _ := loadConfig(loadOptions{File: c.config, Environ: os.Environ()})
	eff := lc.Eff
	retrust, seal := false, false
	rest = slices.DeleteFunc(rest, func(a string) bool {
		n, _, _ := cutFlag(a)
		switch n {
		case "retrust":
			retrust = true
			return true
		case "seal":
			seal = true
			return true
		}
		return false
	})
	// SEC-R1: on Windows the manifest URL, release keys and channel come
	// from an administrator-only place (HKLM), never from the
	// service-writable feeder.json alone.
	// Derive the manifest URL before sealing, so the HKLM copy (SEC-R1)
	// holds it and a later edit of feeder.json cannot redirect it.
	defaultManifestURL(&eff)
	if seal {
		// "-seal" (the machine-scope installer, elevated): store the
		// anchors in HKLM now, so no trust-on-first-use window remains.
		// Nothing to seal is not an error; nothing is checked or installed.
		if eff.Update.ManifestURL == "" || len(eff.Update.Keys) == 0 {
			fmt.Fprintln(stdout, "No update.manifestUrl / update.keys to seal.")
			return 0
		}
		msg, err := applyAdminTrust(&eff, true)
		if err != nil {
			fmt.Fprintf(stderr, "airview-feeder update -seal: %v\n", err)
			return 1
		}
		if msg != "" {
			fmt.Fprintln(stdout, msg)
		}
		return 0
	}
	if msg, err := applyAdminTrust(&eff, retrust); err != nil {
		fmt.Fprintf(stderr, "airview-feeder update: %v\n", err)
		return 1
	} else if msg != "" {
		fmt.Fprintln(stdout, msg)
	}
	code := selfupdate.RunCommand(rest, stdout, stderr, selfupdate.CommandOptions{
		Product: "airview-feeder", Name: "airview-feeder", Version: version,
		Settings:     selfupdate.FeederSettings(eff.AutoUpdate, eff.Update),
		StateDir:     c.data,
		Service:      updateService(),
		HealthURL:    feederHealthURL(eff),
		HealthAccept: []int{200, 503},
		HealthHeader: http.Header{"X-AirView-Feeder": {"1"}},
	})
	if code == 0 && runtime.GOOS == "windows" {
		// The tray is the same program with the GUI subsystem: bring it
		// to the installed version (a no-op when it is current).
		if exe, err := selfupdate.ExecutablePath(); err == nil {
			if wrote, err := refreshTray(exe, "airview-feeder-tray.exe"); err != nil {
				fmt.Fprintf(stderr, "airview-feeder update: tray program not refreshed: %v\n", err)
			} else if wrote {
				fmt.Fprintln(stdout, "airview-feeder-tray.exe refreshed")
			}
		}
	}
	return code
}

// splitDataConfig removes -data / -config (with their values) from args.
func splitDataConfig(args []string) (data, cfg string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := cutFlag(a)
		if name != "data" && name != "config" {
			rest = append(rest, a)
			continue
		}
		if !hasVal && i+1 < len(args) {
			i++
			val = args[i]
		}
		if name == "data" {
			data = val
		} else {
			cfg = val
		}
	}
	return data, cfg, rest
}

// cutFlag splits "-name=value" / "--name" into its parts.
func cutFlag(a string) (name, val string, hasVal bool) {
	if len(a) < 2 || a[0] != '-' {
		return "", "", false
	}
	a = a[1:]
	if a != "" && a[0] == '-' {
		a = a[1:]
	}
	for i := 0; i < len(a); i++ {
		if a[i] == '=' {
			return a[:i], a[i+1:], true
		}
	}
	return a, "", false
}

// attachUpdate is the in-process part (startFeeder): a crash-looping new
// version is rolled back here (error wrapping ErrRolledBack: exit non-zero
// at once). Windows leaves the automatic installs to the scheduled task
// (the service cannot restart itself through the SCM).
func (f *feeder) attachUpdate(eff model.FeederConfig) error {
	defaultManifestURL(&eff)
	s := selfupdate.FeederSettings(eff.AutoUpdate, eff.Update)
	if s.ManifestURL == "" || len(s.Keys) == 0 {
		return nil // not set up: nothing to confirm or install
	}
	if os.Getenv("AIRVIEW_FEEDER_UPDATER") == "external" {
		// Pi installer: the root timer runs "airview-feeder update", which
		// restarts, supervises and rolls back; the service cannot write
		// the program directory anyway.
		return nil
	}
	if runtime.GOOS == "windows" {
		s.Enabled = false
	}
	exe, err := selfupdate.ExecutablePath()
	if err != nil {
		f.o.Logf("self-update unavailable: %v", err)
		return nil
	}
	att, err := selfupdate.Attach(selfupdate.Options{
		Product: "airview-feeder", CurrentVersion: version, Settings: s, StateDir: f.o.DataDir,
		Restarter: selfupdate.SelfRestarter{Exe: exe, Before: f.stop, Exit: func() { f.requestExit(0) }},
		Logf:      f.o.Logf,
	})
	switch {
	case errors.Is(err, selfupdate.ErrRolledBack):
		return fmt.Errorf("the new version failed its trial and was rolled back: %w", err)
	case err != nil:
		f.o.Logf("self-update unavailable: %v", err)
		return nil
	}
	f.att = att
	return nil
}

// runUpdate confirms a version on trial and runs the automatic loop.
func (f *feeder) runUpdate() {
	if f.att == nil {
		return
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		err := f.att.Run(f.ctx, f.selfCheck)
		if errors.Is(err, selfupdate.ErrRolledBack) || errors.Is(err, selfupdate.ErrExitRequested) {
			f.o.Logf("self-update: exiting for the restart (%v)", err)
			f.requestExit(1)
		}
	}()
}

// selfCheck fails only when the program itself is broken: the status page
// does not answer. An unreachable aggregator is no reason to roll back.
func (f *feeder) selfCheck(ctx context.Context) error {
	f.uiMu.Lock()
	addr := ""
	if f.uiLn != nil {
		addr = f.uiLn.Addr().String()
	}
	f.uiMu.Unlock()
	if addr == "" {
		return nil // status page disabled
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-AirView-Feeder", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return fmt.Errorf("status page: HTTP %d", resp.StatusCode)
	}
	return nil
}

// requestExit asks the run loop to end with code; when no run loop takes
// it (Windows service and tray bodies), the process exits after 30 s so
// its supervisor starts the program file again.
func (f *feeder) requestExit(code int) {
	select {
	case f.exitReq <- code:
	default:
		return
	}
	go func() {
		time.Sleep(30 * time.Second)
		if len(f.exitReq) > 0 {
			f.o.Logf("self-update: no run loop took the exit request; exiting")
			os.Exit(max(code, 1))
		}
	}()
}

// UpdateStatus is the self-update state (status page, tray).
func (f *feeder) UpdateStatus() selfupdate.Status { return f.att.Status() }
