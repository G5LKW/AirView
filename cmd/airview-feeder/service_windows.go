package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	svcdebug "golang.org/x/sys/windows/svc/debug"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// The Windows service (DESIGN §12.3):
//
//	airview-feeder service install [-name AirViewFeeder] [-config f] [-data d] [-account virtual|localsystem|localservice|networkservice]
//	                               [-start auto|delayed|manual] [-user-control=true] [-acl=true] [-log-dir d] [-log-max-mb n] [-log-keep n] [-now]
//	airview-feeder service remove  [-name ...]
//	airview-feeder service start|stop|pause|continue|restart|status [-name ...] [-wait 60s] [-json]
//	airview-feeder service run     [-name ...] [-config f] [-data d] [-log-...]   (started by the SCM; -console to try it in a console)
//	airview-feeder service sid     [-name ...]                                       (the virtual account's SID)
//
// install is idempotent: on an existing service it updates the command
// line, account, start type, recovery actions and permissions (installer
// upgrades run it again). The service runs as the virtual account
// NT SERVICE\<name> by default; Pause stops feeding (inputs, uplinks and
// the status page) until Continue, a restart or a reboot.

const (
	exitStartFailed = 1 // the configuration could not be loaded / the feeder failed to start
	exitLocked      = 2 // another feeder holds the data directory
	exitLogFailed   = 3 // the data or log directory cannot be created
)

func serviceUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: airview-feeder service <command> [options]

Commands:
  install    register (or update) the Windows service; needs administrator rights
  remove     stop and unregister the service (keeps configuration and identity)
  start      start the service
  stop       stop the service
  pause      stop feeding until "continue" (the service keeps running)
  continue   resume feeding after "pause"
  restart    stop and start the service
  status     show the service state (-json for JSON)
  run        the service body, started by the service control manager (-console to try it here)
  sid        print the SID of the service's virtual account (for icacls)

Run "airview-feeder service <command> -h" for the options.
`)
}

func cmdService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			serviceUsage(stdout)
			return 0
		}
		serviceUsage(stderr)
		return 2
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "install":
		return svcInstall(args, stdout, stderr)
	case "remove", "uninstall", "delete":
		return svcRemove(args, stdout, stderr)
	case "start", "stop", "pause", "continue", "resume", "restart":
		return svcControl(sub, args, stdout, stderr)
	case "status", "query":
		return svcStatus(args, stdout, stderr)
	case "run":
		return svcRun(args, stdout, stderr)
	case "sid":
		fs := newFlagSet("service sid", stderr)
		name := fs.String("name", defaultServiceName, "service `name`")
		if code := parseFlags(fs, args); code >= 0 {
			return code
		}
		fmt.Fprintln(stdout, serviceSID(*name))
		return 0
	case "help":
		serviceUsage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "airview-feeder service: unknown command %q\n\n", sub)
	serviceUsage(stderr)
	return 2
}

// ---- install ----------------------------------------------------------------

type installOptions struct {
	name, display, account, start string
	c                             common
	logs                          logSettings
	userControl, acl, now, elog   bool
	exe                           string // default: the console feeder next to this program
}

// serviceArgs is the command line the SCM runs (after the executable).
func (o installOptions) serviceArgs() []string {
	a := []string{"service", "run", "-name", o.name}
	if o.c.config != "" {
		a = append(a, "-config", absPath(o.c.config))
	}
	if o.c.data != "" {
		a = append(a, "-data", absPath(o.c.data))
	}
	if o.logs.Dir != "" {
		a = append(a, "-log-dir", absPath(o.logs.Dir))
	}
	if o.logs.MaxMB > 0 {
		a = append(a, "-log-max-mb", fmt.Sprint(o.logs.MaxMB))
	}
	if o.logs.Keep > 0 {
		a = append(a, "-log-keep", fmt.Sprint(o.logs.Keep))
	}
	return a
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// accountFor maps -account to the SCM start name and the SID that gets
// access to the data directory ("" = SYSTEM, already covered).
func accountFor(kind, name string) (startName, sid string, err error) {
	switch strings.ToLower(kind) {
	case "", "virtual":
		return `NT SERVICE\` + name, serviceSID(name), nil
	case "localsystem", "system":
		return "LocalSystem", "", nil
	case "localservice":
		return `NT AUTHORITY\LocalService`, "S-1-5-19", nil
	case "networkservice":
		return `NT AUTHORITY\NetworkService`, "S-1-5-20", nil
	}
	return "", "", fmt.Errorf("unknown -account %q (virtual, localsystem, localservice or networkservice)", kind)
}

func startTypeFor(s string) (uint32, bool, error) {
	switch strings.ToLower(s) {
	case "", "auto", "automatic":
		return mgr.StartAutomatic, false, nil
	case "delayed", "delayed-auto":
		return mgr.StartAutomatic, true, nil
	case "manual", "demand":
		return mgr.StartManual, false, nil
	}
	return 0, false, fmt.Errorf("unknown -start %q (auto, delayed or manual)", s)
}

// serviceConfig builds the SCM configuration.
func (o installOptions) serviceConfig() (mgr.Config, string, error) {
	startName, sid, err := accountFor(o.account, o.name)
	if err != nil {
		return mgr.Config{}, "", err
	}
	st, delayed, err := startTypeFor(o.start)
	if err != nil {
		return mgr.Config{}, "", err
	}
	cfg := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        st,
		DelayedAutoStart: delayed,
		ErrorControl:     mgr.ErrorNormal,
		DisplayName:      o.display,
		Description:      serviceDescription,
		ServiceStartName: startName,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
		BinaryPathName:   windows.ComposeCommandLine(append([]string{o.exe}, o.serviceArgs()...)),
	}
	return cfg, sid, nil
}

func svcInstall(args []string, stdout, stderr io.Writer) int {
	var o installOptions
	fs := newFlagSet("service install", stderr)
	o.c.register(fs)
	fs.StringVar(&o.name, "name", defaultServiceName, "service `name` (one per feeder instance)")
	fs.StringVar(&o.display, "display", "", "display `name` in services.msc (default \"AirView Feeder\", or with the service name)")
	fs.StringVar(&o.account, "account", "virtual", "service `account`: virtual (NT SERVICE\\<name>), localsystem, localservice or networkservice")
	fs.StringVar(&o.start, "start", "auto", "start `type`: auto, delayed or manual")
	fs.BoolVar(&o.userControl, "user-control", true, "let interactive users pause, resume and restart the service (the tray needs no UAC prompt)")
	fs.BoolVar(&o.acl, "acl", true, "restrict the data directory to SYSTEM, Administrators and the service account")
	fs.BoolVar(&o.elog, "eventlog", true, "register the Windows Event Log source")
	fs.BoolVar(&o.now, "now", false, "start the service after installing it")
	fs.StringVar(&o.exe, "exe", "", "feeder `executable` the service runs (default: airview-feeder.exe next to this program)")
	registerLogFlags(fs, &o.logs)
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	if o.display == "" {
		o.display = defaultServiceDisplay
		if !strings.EqualFold(o.name, defaultServiceName) {
			o.display += " (" + o.name + ")"
		}
	}
	if o.exe == "" {
		o.exe = siblingExe()
	}
	o.exe = absPath(o.exe)
	if _, err := os.Stat(o.exe); err != nil {
		fmt.Fprintf(stderr, "airview-feeder: %v\n", err)
		return 1
	}
	if err := installService(o, stdout); err != nil {
		fmt.Fprintf(stderr, "airview-feeder: %v\n", err)
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			fmt.Fprintln(stderr, "Run this command in an administrator console (or use the installer).")
		}
		return 1
	}
	return 0
}

func installService(o installOptions, stdout io.Writer) error {
	cfg, sid, err := o.serviceConfig()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service control manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(o.name)
	verb := "installed"
	if err == nil {
		verb = "updated"
		if err := s.UpdateConfig(cfg); err != nil {
			s.Close()
			return fmt.Errorf("update service %s: %w", o.name, err)
		}
	} else {
		s, err = m.CreateService(o.name, o.exe, cfg, o.serviceArgs()...)
		if err != nil {
			return fmt.Errorf("create service %s: %w", o.name, err)
		}
	}
	defer s.Close()

	// Restart after 5 s, 5 s, then every 60 s; forget failures after a day.
	// Also on a non-crash failure (the service stops with an error code,
	// e.g. a broken configuration it may recover from after an edit).
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400); err != nil {
		fmt.Fprintf(stdout, "warning: recovery actions: %v\n", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		fmt.Fprintf(stdout, "warning: recovery on non-crash failures: %v\n", err)
	}
	if err := setServiceDACL(s.Handle, o.userControl); err != nil {
		fmt.Fprintf(stdout, "warning: service permissions: %v\n", err)
	}
	if o.elog {
		if err := eventlog.InstallAsEventCreate(o.name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil && !strings.Contains(err.Error(), "exists") {
			fmt.Fprintf(stdout, "warning: event log source: %v\n", err)
		}
	}

	data := o.c.data
	if data == "" {
		data = defaultDataDir()
	}
	logs := o.logs
	logs.resolve(data, os.Getenv)
	if o.acl {
		if err := secureDataDir(data, logs.Dir, sid); err != nil {
			fmt.Fprintf(stdout, "warning: data directory permissions not changed: %v\n", err)
		}
	} else if err := os.MkdirAll(logs.Dir, 0o750); err != nil {
		fmt.Fprintf(stdout, "warning: %v\n", err)
	}
	conf := o.c.config
	if conf == "" {
		conf = defaultConfigFile(data)
	}
	if !isParentDir(absPath(data), absPath(conf)) {
		fmt.Fprintf(stdout, "note: the configuration file %s is outside the data directory; make sure %s can read it.\n", conf, cfg.ServiceStartName)
	}

	fmt.Fprintf(stdout, "Service %s %s.\n  account: %s\n  command: %s\n  config:  %s\n  data:    %s\n  logs:    %s\n",
		o.name, verb, cfg.ServiceStartName, cfg.BinaryPathName, conf, data, logs.Dir)
	if o.now {
		st, err := s.Query()
		if err == nil && st.State == svc.Running {
			fmt.Fprintln(stdout, "The service is already running; restart it to use the new settings.")
			return nil
		}
		if err := s.Start(); err != nil {
			return fmt.Errorf("start %s: %w", o.name, err)
		}
		if _, err := waitState(s, svc.Running, 60*time.Second); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Service started.")
	}
	return nil
}

func registerLogFlags(fs *flag.FlagSet, l *logSettings) {
	fs.StringVar(&l.Dir, "log-dir", "", "log `directory` (default: $"+envLogDir+", else <data>\\logs)")
	fs.IntVar(&l.MaxMB, "log-max-mb", 0, "rotate feeder.log at this `size` in MB (default: $"+envLogMaxMB+", else 10)")
	fs.IntVar(&l.Keep, "log-keep", 0, "old log `files` to keep (default: $"+envLogKeep+", else 5)")
}

// ---- remove -----------------------------------------------------------------

func svcRemove(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("service remove", stderr)
	name := fs.String("name", defaultServiceName, "service `name`")
	keepLog := fs.Bool("keep-eventlog", false, "keep the Event Log source registration")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	if err := removeService(*name, !*keepLog, stdout); err != nil {
		fmt.Fprintf(stderr, "airview-feeder: %v\n", err)
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			fmt.Fprintln(stderr, "Run this command in an administrator console.")
		}
		return 1
	}
	// The uninstaller removes the service last: the release trust anchors
	// (SEC-R1, HKLM) go with it.
	if err := removeAdminTrust(); err != nil {
		fmt.Fprintf(stdout, "warning: removing HKLM\\%s: %v\n", trustKeyPath, err)
	}
	return 0
}

func removeService(name string, removeEventLog bool, stdout io.Writer) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service control manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			fmt.Fprintf(stdout, "Service %s is not installed.\n", name)
			if removeEventLog {
				_ = eventlog.Remove(name)
			}
			return nil
		}
		return fmt.Errorf("open service %s: %w", name, err)
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err == nil || errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			if _, err := waitState(s, svc.Stopped, 60*time.Second); err != nil {
				fmt.Fprintf(stdout, "warning: %v; removing anyway (it is deleted when it stops)\n", err)
			}
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service %s: %w", name, err)
	}
	if removeEventLog {
		_ = eventlog.Remove(name)
	}
	fmt.Fprintf(stdout, "Service %s removed. The configuration, identity and logs are kept (\"airview-feeder uninstall-helper\" lists them).\n", name)
	return nil
}

// ---- control ----------------------------------------------------------------

// openServiceAccess opens a service with only the rights needed, so
// non-administrators can query it (and control it when the service DACL
// allows: install -user-control).
func openServiceAccess(name string, access uint32) (*mgr.Service, func(), error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, nil, fmt.Errorf("service control manager: %w", err)
	}
	h, err := windows.OpenService(scm, windows.StringToUTF16Ptr(name), access)
	if err != nil {
		windows.CloseServiceHandle(scm)
		return nil, nil, err
	}
	s := &mgr.Service{Name: name, Handle: h}
	return s, func() {
		windows.CloseServiceHandle(h)
		windows.CloseServiceHandle(scm)
	}, nil
}

// controlService performs start/stop/pause/continue/restart and waits up to
// wait for the final state.
func controlService(name, action string, wait time.Duration) (svc.Status, error) {
	access := uint32(windows.SERVICE_QUERY_STATUS)
	switch action {
	case "start":
		access |= windows.SERVICE_START
	case "stop":
		access |= windows.SERVICE_STOP
	case "pause", "continue", "resume":
		access |= windows.SERVICE_PAUSE_CONTINUE
	case "restart":
		access |= windows.SERVICE_START | windows.SERVICE_STOP
	default:
		return svc.Status{}, fmt.Errorf("unknown action %q", action)
	}
	s, done, err := openServiceAccess(name, access)
	if err != nil {
		return svc.Status{}, err
	}
	defer done()
	cur, err := s.Query()
	if err != nil {
		return svc.Status{}, err
	}
	switch action {
	case "start":
		if cur.State == svc.Running {
			return cur, nil
		}
		if cur.State == svc.Paused {
			return svc.Status{}, errors.New("the service is paused; use continue")
		}
		if err := s.Start(); err != nil {
			return svc.Status{}, err
		}
		return waitState(s, svc.Running, wait)
	case "stop":
		if cur.State == svc.Stopped {
			return cur, nil
		}
		if _, err := s.Control(svc.Stop); err != nil {
			return svc.Status{}, err
		}
		return waitState(s, svc.Stopped, wait)
	case "pause":
		if cur.State == svc.Paused {
			return cur, nil
		}
		if _, err := s.Control(svc.Pause); err != nil {
			return svc.Status{}, err
		}
		return waitState(s, svc.Paused, wait)
	case "continue", "resume":
		if cur.State == svc.Running {
			return cur, nil
		}
		if _, err := s.Control(svc.Continue); err != nil {
			return svc.Status{}, err
		}
		return waitState(s, svc.Running, wait)
	}
	// restart
	if cur.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return svc.Status{}, err
		}
		if _, err := waitState(s, svc.Stopped, wait); err != nil {
			return svc.Status{}, err
		}
	}
	if err := s.Start(); err != nil {
		return svc.Status{}, err
	}
	return waitState(s, svc.Running, wait)
}

// waitState polls until the service reaches want.
func waitState(s *mgr.Service, want svc.State, timeout time.Duration) (svc.Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil {
			return st, err
		}
		if st.State == want {
			return st, nil
		}
		if want == svc.Running && st.State == svc.Stopped {
			return st, fmt.Errorf("service %s stopped (exit code %d, service code %d); see the log", s.Name, st.Win32ExitCode, st.ServiceSpecificExitCode)
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("service %s is %s, not %s, after %v", s.Name, stateName(st.State), stateName(want), timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "resuming"
	case svc.PausePending:
		return "pausing"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state %d", s)
}

func svcControl(action string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("service "+action, stderr)
	name := fs.String("name", defaultServiceName, "service `name`")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for the new state")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	st, err := controlService(*name, action, *wait)
	if err != nil {
		fmt.Fprintf(stderr, "airview-feeder: %s %s: %v\n", action, *name, err)
		switch {
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			fmt.Fprintln(stderr, "Run this command in an administrator console.")
		case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
			fmt.Fprintln(stderr, "The service is not installed (airview-feeder service install).")
		}
		return 1
	}
	fmt.Fprintf(stdout, "Service %s is %s.\n", *name, stateName(st.State))
	return 0
}

// ---- status -----------------------------------------------------------------

// serviceInfo is what "service status" and the tray know about the service.
type serviceInfo struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	State     string `json:"state,omitempty"`
	PID       uint32 `json:"pid,omitempty"`
	ExitCode  uint32 `json:"exitCode,omitempty"`
	SvcCode   uint32 `json:"serviceExitCode,omitempty"`
	Account   string `json:"account,omitempty"`
	StartType string `json:"startType,omitempty"`
	Command   string `json:"command,omitempty"`
	Config    string `json:"config,omitempty"`
	DataDir   string `json:"dataDir,omitempty"`
	LogDir    string `json:"logDir,omitempty"`
	Error     string `json:"error,omitempty"`

	state svc.State
}

// queryService returns the service's state and, when readable, its
// configuration (with the -config / -data / -log-dir it runs with).
func queryService(name string) serviceInfo {
	si := serviceInfo{Name: name}
	s, done, err := openServiceAccess(name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			si.Error = err.Error()
		}
		return si
	}
	defer done()
	si.Installed = true
	st, err := s.Query()
	if err != nil {
		si.Error = err.Error()
		return si
	}
	si.state, si.State, si.PID = st.State, stateName(st.State), st.ProcessId
	si.ExitCode, si.SvcCode = st.Win32ExitCode, st.ServiceSpecificExitCode
	if cfg, err := s.Config(); err == nil {
		si.Account, si.Command = cfg.ServiceStartName, cfg.BinaryPathName
		switch {
		case cfg.StartType == mgr.StartAutomatic && cfg.DelayedAutoStart:
			si.StartType = "delayed"
		case cfg.StartType == mgr.StartAutomatic:
			si.StartType = "auto"
		case cfg.StartType == mgr.StartManual:
			si.StartType = "manual"
		case cfg.StartType == mgr.StartDisabled:
			si.StartType = "disabled"
		}
		c, logs := serviceRunPaths(cfg.BinaryPathName)
		si.Config, si.DataDir, si.LogDir = c.config, c.data, logs.Dir
	}
	return si
}

// serviceRunPaths extracts -config, -data and the log settings from a
// service command line and resolves the defaults the service would use.
func serviceRunPaths(cmdline string) (common, logSettings) {
	var c common
	var logs logSettings
	args := splitCommandLine(cmdline)
	for i := 0; i+1 < len(args); i++ {
		v := args[i+1]
		switch strings.TrimLeft(args[i], "-") {
		case "config":
			c.config = v
		case "data":
			c.data = v
		case "log-dir":
			logs.Dir = v
		default:
			continue
		}
		i++
	}
	c.resolve()
	logs.resolve(c.data, func(string) string { return "" })
	return c, logs
}

// splitCommandLine splits a Windows command line with CommandLineToArgvW.
func splitCommandLine(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var argc int32
	argv, err := windows.CommandLineToArgv(windows.StringToUTF16Ptr(s), &argc)
	if err != nil {
		return strings.Fields(s)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(argv)))
	out := make([]string, argc)
	for i := range out {
		out[i] = windows.UTF16PtrToString(&argv[i][0])
	}
	return out
}

func svcStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("service status", stderr)
	name := fs.String("name", defaultServiceName, "service `name`")
	asJSON := fs.Bool("json", false, "print JSON")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	si := queryService(*name)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(si)
	} else if !si.Installed {
		fmt.Fprintf(stdout, "Service %s is not installed.\n", *name)
		if si.Error != "" {
			fmt.Fprintf(stdout, "  (%s)\n", si.Error)
		}
	} else {
		fmt.Fprintf(stdout, "Service %s: %s", si.Name, si.State)
		if si.PID != 0 {
			fmt.Fprintf(stdout, " (pid %d)", si.PID)
		}
		fmt.Fprintln(stdout)
		if si.state == svc.Stopped && (si.ExitCode != 0 || si.SvcCode != 0) {
			fmt.Fprintf(stdout, "  last exit: code %d, service code %d\n", si.ExitCode, si.SvcCode)
		}
		for _, kv := range [][2]string{{"start", si.StartType}, {"account", si.Account}, {"command", si.Command},
			{"config", si.Config}, {"data", si.DataDir}, {"logs", si.LogDir}, {"error", si.Error}} {
			if kv[1] != "" {
				fmt.Fprintf(stdout, "  %-8s %s\n", kv[0]+":", kv[1])
			}
		}
	}
	if !si.Installed {
		return 3
	}
	return 0
}

// ---- run (the service body) -------------------------------------------------

// svcRunner is the part of *feeder the service drives.
type svcRunner interface {
	stop()
	reload()
	statusURL() string
}

// feederService implements svc.Handler.
type feederService struct {
	start  func() (svcRunner, error)
	logf   func(format string, args ...any)
	event  func(kind string, msg string) // Event Log: "info", "warning", "error"
	onURL  func(url string)
	tick   time.Duration // checkpoint interval of pending states
	stopTO time.Duration // wait hint of stop / pause
}

const acceptRunning = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPauseAndContinue | svc.AcceptParamChange

// Execute implements svc.Handler.
func (h *feederService) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	if h.tick <= 0 {
		h.tick = 2 * time.Second
	}
	if h.stopTO <= 0 {
		h.stopTO = 30 * time.Second
	}
	var f svcRunner
	var err error
	h.pending(s, svc.StartPending, 30*time.Second, func() { f, err = h.start() })
	if err != nil {
		h.logf("service: start failed: %v", err)
		h.event("error", "AirView Feeder could not start: "+err.Error())
		return true, exitStartFailed
	}
	h.started(f)
	h.event("info", "AirView Feeder started.")
	s <- svc.Status{State: svc.Running, Accepts: acceptRunning}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			s <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			h.logf("service: %s requested", map[svc.Cmd]string{svc.Stop: "stop", svc.Shutdown: "shutdown"}[c.Cmd])
			if f != nil {
				h.pending(s, svc.StopPending, h.stopTO, f.stop)
			}
			h.event("info", "AirView Feeder stopped.")
			return false, 0
		case svc.Pause:
			if f == nil {
				s <- svc.Status{State: svc.Paused, Accepts: acceptRunning}
				continue
			}
			h.logf("service: pause requested; feeding stops until the service is resumed")
			h.pending(s, svc.PausePending, h.stopTO, f.stop)
			f = nil
			h.event("info", "AirView Feeder paused.")
			s <- svc.Status{State: svc.Paused, Accepts: acceptRunning}
		case svc.Continue:
			if f != nil {
				s <- svc.Status{State: svc.Running, Accepts: acceptRunning}
				continue
			}
			h.logf("service: resuming")
			h.pending(s, svc.ContinuePending, 30*time.Second, func() { f, err = h.start() })
			if err != nil {
				h.logf("service: resume failed: %v", err)
				h.event("error", "AirView Feeder could not resume: "+err.Error())
				return true, exitStartFailed
			}
			h.started(f)
			h.event("info", "AirView Feeder resumed.")
			s <- svc.Status{State: svc.Running, Accepts: acceptRunning}
		case svc.ParamChange:
			if f != nil {
				f.reload()
				h.started(f)
			}
			s <- c.CurrentStatus
		default:
			h.logf("service: unexpected control request %d", c.Cmd)
		}
	}
	// The channel closed (the package never does this in practice).
	if f != nil {
		f.stop()
	}
	return false, 0
}

func (h *feederService) started(f svcRunner) {
	if h.onURL != nil {
		h.onURL(f.statusURL())
	}
}

// pending reports state with a rising checkpoint while fn runs, so the SCM
// does not give up on a slow start or a long spool flush.
func (h *feederService) pending(s chan<- svc.Status, state svc.State, hint time.Duration, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	cp := uint32(1)
	s <- svc.Status{State: state, CheckPoint: cp, WaitHint: uint32(hint / time.Millisecond)}
	t := time.NewTicker(h.tick)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			cp++
			s <- svc.Status{State: state, CheckPoint: cp, WaitHint: uint32(hint / time.Millisecond)}
		}
	}
}

func svcRun(args []string, stdout, stderr io.Writer) int {
	var c common
	var logs logSettings
	var console bool
	fs := newFlagSet("service run", stderr)
	c.register(fs)
	name := fs.String("name", defaultServiceName, "service `name`")
	fs.BoolVar(&console, "console", false, "run the service body in this console (Ctrl+C stops it), for testing")
	registerLogFlags(fs, &logs)
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	c.resolve()
	logs.resolve(c.data, os.Getenv)
	if !console && !isWindowsService() {
		fmt.Fprintln(stderr, "airview-feeder: \"service run\" is started by the service control manager.\nUse \"airview-feeder service start\", or \"airview-feeder run\" to run the feeder in this console (\"service run -console\" tries the service body).")
		return 2
	}

	var elog *eventlog.Log
	if !console {
		elog, _ = eventlog.Open(*name) // nil when the source is not registered
		if elog != nil {
			defer elog.Close()
		}
	}
	event := func(kind, msg string) {
		if elog == nil {
			return
		}
		switch kind {
		case "error":
			_ = elog.Error(1, msg)
		case "warning":
			_ = elog.Warning(2, msg)
		default:
			_ = elog.Info(3, msg)
		}
	}

	if err := os.MkdirAll(c.data, 0o750); err != nil {
		event("error", fmt.Sprintf("AirView Feeder: data directory %s: %v", c.data, err))
		fmt.Fprintf(stderr, "data directory %s: %v\n", c.data, err)
		return exitLogFailed
	}
	var out io.Writer = stderr
	if !console {
		rl, err := openRotatingLog(logs.Dir, int64(logs.MaxMB)<<20, logs.Keep)
		if err != nil {
			event("error", fmt.Sprintf("AirView Feeder: log directory %s: %v", logs.Dir, err))
			out = io.Discard
		} else {
			defer rl.Close()
			out = rl
			if cf, err := os.OpenFile(filepath.Join(logs.Dir, crashFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640); err == nil {
				_ = debug.SetCrashOutput(cf, debug.CrashOptions{})
				cf.Close() // SetCrashOutput keeps its own duplicate
			}
		}
	} else {
		_ = os.MkdirAll(logs.Dir, 0o750)
	}
	lg := log.New(out, "", log.LstdFlags|log.Lmicroseconds)
	logf := func(format string, a ...any) { lg.Printf(format, a...) }
	slogger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))

	release, err := lockDataDirWin(c.data)
	if err != nil {
		logf("service: %v (%s)", err, c.data)
		event("error", fmt.Sprintf("AirView Feeder: %v (%s). Stop the other feeder (tray or console) first.", err, c.data))
		if console {
			return exitLocked
		}
		// Report the failure through the SCM so recovery retries later.
		_ = svc.Run(*name, failingService{code: exitLocked})
		return exitLocked
	}
	defer release()

	h := &feederService{
		start: func() (svcRunner, error) {
			return startFeeder(runOptions{ConfigFile: c.config, DataDir: c.data, Environ: os.Environ(), Logf: logf, Logger: slogger})
		},
		logf:  logf,
		event: event,
		onURL: func(u string) { writeStatusURL(logs.Dir, u) },
	}
	logf("service %s: starting (config %s, data %s, logs %s)", *name, c.config, c.data, logs.Dir)
	if console {
		err = svcdebug.Run(*name, h)
	} else {
		err = svc.Run(*name, h)
	}
	if err != nil {
		logf("service %s: %v", *name, err)
		event("error", fmt.Sprintf("AirView Feeder service failed: %v", err))
		return 1
	}
	logf("service %s: exited", *name)
	return 0
}

// failingService reports a start failure to the SCM.
type failingService struct{ code uint32 }

func (f failingService) Execute(_ []string, _ <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StopPending}
	return true, f.code
}
