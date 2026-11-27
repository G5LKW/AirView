package main

// Signed self-update (internal/selfupdate, WIRING.md §4). "airview update"
// checks, installs and health-checks a release published for the "airview"
// product; with update.enabled (opt-in, off by default) a headless
// "airview serve" also finishes and confirms installs at start-up and runs
// the periodic check. The desktop window never updates itself.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"airview/internal/config"
	"airview/internal/model"
	"airview/internal/selfupdate"
)

// splitDataConfig removes -data / -config (with their values) from args.
func splitDataConfig(args []string) (data, cfg string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || (name != "data" && name != "config") {
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

// loadUpdateConfig resolves the data directory and reads the effective
// configuration without writing anything.
func loadUpdateConfig(dataFlag, cfgFlag string) (string, model.Config, error) {
	if dataFlag == "" {
		dataFlag = os.Getenv(config.EnvDataDir)
	}
	dir, err := config.ResolveDataDir(dataFlag)
	if err != nil {
		return "", model.Config{}, err
	}
	rep := config.Check(config.LoadOptions{Dir: dir, File: cfgFlag, Environ: os.Environ()})
	return dir, rep.Effective, nil
}

func defaultService() string {
	if runtime.GOOS == "linux" {
		return "airview" // systemd unit of deploy/server
	}
	return ""
}

func healthURL(cfg model.Config) string {
	port := cfg.HTTP.Port
	if port <= 0 {
		port = 80
	}
	return fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
}

// runUpdate implements "airview update [-data DIR] [-config FILE] [update flags]".
func runUpdate(args []string, stdout, stderr io.Writer) int {
	dataFlag, cfgFlag, rest := splitDataConfig(args)
	dir, cfg, err := loadUpdateConfig(dataFlag, cfgFlag)
	if err != nil {
		fmt.Fprintf(stderr, "airview update: data directory: %v\n", err)
		return 1
	}
	return selfupdate.RunCommand(rest, stdout, stderr, selfupdate.CommandOptions{
		Product: "airview", Name: "airview", Version: version,
		Settings:  selfupdate.SettingsFromModel(cfg.Update),
		StateDir:  dir,
		Service:   defaultService(),
		HealthURL: healthURL(cfg),
	})
}

// attachUpdate runs the in-process part of the self-update for a headless
// run whose update block names a manifest and a key: it finishes a pending
// install (a crash-looping new version is rolled back: rolledBack is then
// true and the program must exit non-zero at once) and returns the
// attachment whose Run confirms the new version. The automatic
// check-and-install loop is not the attachment's: updateCtl runs it with
// the live configuration (update.enabled / update.channel can change while
// the program runs). exit asks the main loop to end (Windows: the new
// process was started); before runs before an in-place restart (POSIX
// execve).
func attachUpdate(dir, cfgFile string, log *slog.Logger, exit func(), before func()) (att *selfupdate.Attachment, rolledBack bool) {
	_, cfg, err := loadUpdateConfig(dir, cfgFile)
	if err != nil {
		return nil, false
	}
	s := selfupdate.SettingsFromModel(cfg.Update)
	if s.ManifestURL == "" || len(s.Keys) == 0 {
		return nil, false // not set up: nothing can be pending
	}
	s.Enabled = false // the automatic loop is updateCtl's
	exe, err := selfupdate.ExecutablePath()
	if err != nil {
		log.Warn("self-update unavailable", "err", err)
		return nil, false
	}
	att, err = selfupdate.Attach(selfupdate.Options{
		Product: "airview", CurrentVersion: version,
		Settings:  s,
		StateDir:  dir,
		Restarter: selfupdate.SelfRestarter{Exe: exe, Before: before, Exit: exit},
		Logf:      func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) },
	})
	switch {
	case errors.Is(err, selfupdate.ErrRolledBack):
		log.Error("the new version failed its trial and was rolled back; exiting so the previous version starts", "err", err)
		return nil, true
	case err != nil:
		log.Warn("self-update unavailable", "err", err)
		return nil, false
	}
	return att, false
}

// e2eFault is empty in every release build. The self-update end-to-end
// test (TestSelfUpdateE2E) links a deliberately broken "new version" with
// -ldflags "-X main.e2eFault=unhealthy": its trial self check fails, so the
// rollback runs against a real airview binary.
var e2eFault string

// healthCheck is the new version's self check during its trial: the
// local web server answers /healthz.
func healthCheck(url string) func(context.Context) error {
	return func(ctx context.Context) error {
		if e2eFault == "unhealthy" {
			return errors.New("health check: this build is broken on purpose (e2e test)")
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("health check: HTTP %d", resp.StatusCode)
		}
		return nil
	}
}
