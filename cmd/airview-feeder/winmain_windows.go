package main

// Windows entry glue of the Windows feeder (DESIGN §12.3, WP-WF): the
// "service" and "tray" subcommands, and two ways the program is started
// without a subcommand:
//
//   - airview-feeder-tray.exe (the same program linked with -H windowsgui,
//     so no console window appears) starts the tray when it is run without a
//     command;
//   - the service control manager may start a service whose command line
//     names no subcommand (registered by hand with sc.exe): that runs the
//     service.

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/svc"
)

const (
	// defaultServiceName is the SCM name of the feeder service; its
	// virtual account is NT SERVICE\AirViewFeeder.
	defaultServiceName = "AirViewFeeder"
	// defaultServiceDisplay is the name shown in services.msc.
	defaultServiceDisplay = "AirView Feeder"
	serviceDescription    = "Feeds local ADS-B / Mode S receivers to AirView aggregators (status page http://127.0.0.1:8099/)."

	// Environment variables of the Windows log file (not feeder.json
	// settings: the service's log location must be known before the
	// configuration is read).
	envLogDir   = "AIRVIEW_FEEDER_LOG_DIR"
	envLogMaxMB = "AIRVIEW_FEEDER_LOG_MAX_MB"
	envLogKeep  = "AIRVIEW_FEEDER_LOG_KEEP"
)

func init() {
	commands["service"] = command{"install|remove|start|stop|pause|continue|restart|status|run: the Windows service", cmdService}
	commands["tray"] = command{"the Windows notification-area icon (status, pause, logs, settings)", cmdTray}
	ownFiles = append(ownFiles, "logs")
	for _, k := range []string{envLogDir, envLogMaxMB, envLogKeep} {
		nonSettingEnv[k] = true
	}
	os.Args = windowsDefaultArgs(os.Args, isWindowsService)
}

// isWindowsService is svc.IsWindowsService (a variable for tests).
var isWindowsService = func() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// windowsDefaultArgs inserts the implied subcommand: "tray" for an
// executable whose name contains "-tray", "service run" when the SCM
// started the program without one.
func windowsDefaultArgs(args []string, isService func() bool) []string {
	if len(args) == 0 {
		return args
	}
	hasCmd := len(args) > 1 && !strings.HasPrefix(args[1], "-")
	if hasCmd {
		return args
	}
	base := strings.ToLower(filepath.Base(args[0]))
	switch {
	case strings.Contains(base, "-tray"):
		return append([]string{args[0], "tray"}, args[1:]...)
	case isService():
		return append([]string{args[0], "service", "run"}, args[1:]...)
	}
	return args
}

// exePath is the absolute path of the running program.
func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// siblingExe returns the console feeder next to the tray (or the program
// itself): airview-feeder-tray.exe -> airview-feeder.exe.
func siblingExe() string {
	self := exePath()
	dir, base := filepath.Split(self)
	if i := strings.Index(strings.ToLower(base), "-tray"); i >= 0 {
		cand := filepath.Join(dir, base[:i]+base[i+len("-tray"):])
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return self
}
