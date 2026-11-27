// Command airview is the AirView ADS-B / Mode S statistics and mapping
// station and network aggregator: an embedded HTTP server with the web UI,
// shown in a WebView2 window on Windows and served headless everywhere else
// (and on Windows with -headless or "airview serve").
//
//	airview [-data dir] [-config file] [-set path=value]... [-port n] [-headless] [-demo] [-open]
//	airview serve [same options]           headless server (systemd, Docker, Windows service wrappers)
//	airview config check|print|example|env [options]
//	airview admin init|reset [-data dir] [-user name]
//	airview backup [-data dir] [-o file.zip]
//	airview restore [-data dir] [-force] file.zip
//	airview update [-data dir] [-config file] [update options]
//	airview federation id [-data dir] [-config file]
//	airview version [-json]
package main

import (
	"airview/internal/selfupdate"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"airview/internal/cli"
	"airview/internal/config"
	"airview/internal/engine"
	"airview/internal/model"
	"airview/internal/server"
)

// version can be overridden at build time:
// -ldflags "-X main.version=1.2.3".
var version = model.Version

// flags are the command-line options of the server (see
// docs/ARCHITECTURE.md).
type flags struct {
	data       string
	config     string   // operator config file (-config)
	sets       []string // -set path=value overrides
	port       int
	headless   bool
	demo       bool
	open       bool
	version    bool
	listenHost string // bind address of the aggregator's feeder listeners
}

// subcommands lists the first-argument commands; anything else (or a first
// argument starting with "-") runs the server with the classic flags.
var subcommands = []string{"serve", "config", "admin", "backup", "restore", "db", "update", "federation", "version", "help"}

func usageText() string {
	return fmt.Sprintf(`%s %s - ADS-B / Mode S statistics, mapping station and network aggregator

Usage:
  airview [options]                 run the station (desktop window on Windows unless -headless)
  airview serve [options]           run headless (servers, systemd, Docker); same options
  airview config check|print|example|env [options]
                                    validate, show or generate the configuration
  airview admin init|reset [-data dir] [-config file] [-user name]
                                    create or reset the aggregator admin account
  airview backup [-data dir] [-o file.zip]
                                    consistent backup of databases, settings and identities
  airview restore [-data dir] [-force] file.zip
                                    restore a backup (the server must be stopped)
  airview db snapshot [-data dir] -o outdir
                                    consistent copies of the databases (online)
  airview update [-data dir] [-config file] [-check] ...
                                    check for and install a signed release
  airview federation id [-data dir] [-config file]
                                    print this aggregator's federation identity
  airview version [-json]           print the version

Run "airview <command> -h" for the options of a command.

Options:
`, model.AppName, version)
}

func parseFlags(args []string, out io.Writer) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("airview", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&f.data, "data", "", "data `directory` (default: $AIRVIEW_DATA, else \"data\" next to the executable, else the user config directory)")
	fs.StringVar(&f.config, "config", "", "operator config `file` (JSONC; default: $AIRVIEW_CONFIG, else airview.jsonc in the data directory when present)")
	fs.Func("set", "override one setting, `path=value` (repeatable), e.g. -set http.port=8080", func(v string) error {
		if !strings.Contains(v, "=") {
			return fmt.Errorf("-set %q: want path=value", v)
		}
		f.sets = append(f.sets, v)
		return nil
	})
	fs.IntVar(&f.port, "port", 0, "override the HTTP `port` from the configuration")
	fs.BoolVar(&f.headless, "headless", false, "run without a window (always the case outside Windows)")
	fs.BoolVar(&f.demo, "demo", false, "force the built-in traffic simulator feed (standalone)")
	fs.BoolVar(&f.open, "open", false, "open the default browser on the web interface (headless)")
	fs.BoolVar(&f.version, "version", false, "print the version and exit")
	fs.StringVar(&f.listenHost, "listen-host", os.Getenv("AIRVIEW_LISTEN_HOST"), "aggregator mode: `address` the feeder listeners bind to (default: $AIRVIEW_LISTEN_HOST, else every interface)")
	fs.Usage = func() {
		fmt.Fprint(out, usageText())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		return f, fmt.Errorf("unexpected argument %q (commands: %s)", fs.Arg(0), strings.Join(subcommands, ", "))
	}
	if f.port < 0 || f.port > 65535 {
		return f, fmt.Errorf("invalid -port %d", f.port)
	}
	return f, nil
}

// app is the running application: engine + HTTP server.
type app struct {
	flags flags
	dir   string
	log   *slog.Logger
	eng   *engine.Engine
	srv   *server.Server
	url   string // local URL of the web interface
	port  int
	// exitReq ends a headless run with the given exit code (self-update:
	// rolled back or restarted into the new version).
	exitReq chan int
}

func main() {
	os.Exit(run())
}

// splitCommand returns the subcommand ("" for the classic flag form) and
// its arguments.
func splitCommand(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

func run() int {
	cmd, rest := splitCommand(os.Args[1:])
	switch cmd {
	case "config":
		// "airview config ..." runs before any window, lock or log file.
		platformInit(true)
		return cli.RunConfig(rest, cli.OSEnv())
	case "admin":
		platformInit(true)
		return runAdmin(rest, os.Stdin, os.Stdout, os.Stderr)
	case "backup":
		platformInit(true)
		return runBackup(rest, os.Stdout, os.Stderr)
	case "restore":
		platformInit(true)
		return runRestore(rest, os.Stdout, os.Stderr)
	case "db":
		platformInit(true)
		return runDB(rest, os.Stdout, os.Stderr)
	case "update":
		platformInit(true)
		return runUpdate(rest, os.Stdout, os.Stderr)
	case "federation":
		platformInit(true)
		return runFederation(rest, os.Stdout, os.Stderr)
	case "version":
		platformInit(true)
		return runVersion(rest, os.Stdout, os.Stderr)
	case "help":
		platformInit(true)
		_, _ = parseFlags([]string{"-h"}, os.Stdout)
		return 0
	case "serve":
		platformInit(true)
		return serve(rest, true)
	case "":
		wantConsole := false
		for _, a := range rest {
			switch a {
			case "-headless", "--headless", "-version", "--version", "-h", "--help", "-help":
				wantConsole = true
			}
		}
		platformInit(wantConsole)
		return serve(rest, false)
	default:
		platformInit(true)
		fmt.Fprintf(os.Stderr, "airview: unknown command %q (commands: %s; run \"airview help\")\n", cmd, strings.Join(subcommands, ", "))
		return 2
	}
}

// runVersion implements "airview version [-json]".
func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("airview version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	info := versionInfo()
	if *asJSON {
		b, _ := json.MarshalIndent(info, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "%s %s (%s/%s, %s)\n", model.AppName, info.Version, info.OS, info.Arch, info.Go)
	return 0
}

type versionJSON struct {
	App     string `json:"app"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Go      string `json:"go"`
	GUI     bool   `json:"gui"` // this build has the desktop window
}

func versionInfo() versionJSON {
	return versionJSON{App: model.AppName, Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Go: runtime.Version(), GUI: guiAvailable}
}

// serve runs the station / aggregator until the window closes or a
// termination signal arrives. forceHeadless is set by "airview serve".
func serve(args []string, forceHeadless bool) int {
	f, err := parseFlags(args, os.Stderr)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		fatalMessage(fmt.Sprintf("Invalid command line: %v", err), forceHeadless || f.headless || !guiAvailable)
		return 2
	}
	if forceHeadless {
		f.headless = true
	}
	if f.version {
		return runVersion(nil, os.Stdout, os.Stderr)
	}
	gui := guiAvailable && !f.headless

	if f.data == "" {
		f.data = os.Getenv(config.EnvDataDir)
	}
	dir, err := config.ResolveDataDir(f.data)
	if err != nil {
		fatalMessage(fmt.Sprintf("The data directory cannot be used: %v", err), !gui)
		return 1
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}

	logFile := openLogFile(filepath.Join(dir, logFileName))
	defer logFile.Close()
	logger := newLogger(os.Stderr, logFile)
	slog.SetDefault(logger)
	applyMemoryLimit(logger)

	a := &app{flags: f, dir: dir, log: logger, exitReq: make(chan int, 1)}
	// Self-update (opt-in, headless only): finish or roll back a pending
	// install before the instance lock and any listener (on Windows the new
	// version waits here for the previous process to exit).
	var att *selfupdate.Attachment
	if !gui {
		var rolledBack bool
		att, rolledBack = attachUpdate(dir, f.config, logger,
			func() { a.requestExit(0) },
			func() { a.shutdown() })
		if rolledBack {
			return 75
		}
	}

	// One instance per data directory.
	release, already := acquireInstance(dir)
	if already {
		logger.Info("another instance is already running with this data directory", "dataDir", dir)
		activateRunningInstance(dir, gui || f.open)
		return 0
	}
	defer release()

	logger.Info("starting", "app", model.AppName, "version", version,
		"platform", runtime.GOOS+"/"+runtime.GOARCH, "dataDir", dir, "gui", gui, "demo", f.demo)

	// An operator file named explicitly (-config / $AIRVIEW_CONFIG) must be
	// usable: a headless service does not start without it (DESIGN §20.3).
	if err := checkOperatorFile(dir, f); err != nil {
		logger.Error("operator configuration file", "err", err)
		if !gui {
			fatalMessage(fmt.Sprintf("The configuration file cannot be used: %v (run \"airview config check\")", err), true)
			return 1
		}
	}

	a.eng = engine.New(dir, engine.Options{
		Demo:            f.demo,
		AllowRemoteData: remoteDataAllowed(),
		AllowRemoteWeb:  gui,
		Version:         version,
		Logger:          logger,
		ConfigFile:      f.config,
		ConfigSets:      f.sets,
		Environ:         os.Environ(),
		ListenHost:      f.listenHost,
	})
	startCtx, cancelStart := context.WithTimeout(context.Background(), 2*time.Minute)
	err = a.eng.Start(startCtx)
	cancelStart()
	if err != nil {
		logger.Error("engine start failed", "err", err)
		fatalMessage(fmt.Sprintf("AirView could not start:\n\n%v", err), !gui)
		return 1
	}
	applyProcessConfig(logger, logFile, a.eng.EffectiveConfig().Process, a.eng.Aggregator())
	if a.eng.Aggregator() {
		liftMemoryLimit(logger, a.eng.EffectiveConfig().Process.MemoryLimitMB)
	} else if !gui {
		genAdminPassword(logger, a.eng, dir)
		warnOpenAdmin(logger, a.eng.EffectiveConfig().HTTP)
	}

	// A headless aggregator keeps its configured port or fails (systemd and
	// the reverse proxy expect it there); the desktop app may fall back.
	// The self-update as the settings page sees it (updatectl.go): status
	// and "Check now" everywhere, installs only in a headless run.
	ctl := &updateCtl{dir: dir, log: logger, install: !gui, exitReq: func() { a.requestExit(0) },
		settings: func() model.UpdateSettings { return a.eng.EffectiveConfig().Update }}
	if exe, err := selfupdate.ExecutablePath(); err == nil {
		ctl.exe = exe
		if !gui {
			ctl.restarter = selfupdate.SelfRestarter{Exe: exe, Before: a.shutdown, Exit: func() { a.requestExit(0) }}
		}
	} else {
		ctl.install = false
	}
	a.srv = server.New(a.eng, server.Options{PortOverride: f.port, GUI: gui, Version: version, StrictPort: !gui && a.eng.Aggregator(),
		Updater: ctl})
	_, port, err := a.srv.Start(context.Background())
	if err != nil {
		logger.Error("HTTP server start failed", "err", err)
		a.eng.Stop()
		fatalMessage(fmt.Sprintf("AirView could not start its web server:\n\n%v", err), !gui)
		return 1
	}
	a.port = port
	a.url = a.srv.LocalURL()
	accountsCtx, stopAccounts := context.WithCancel(context.Background())
	defer stopAccounts()
	a.wireAccounts(accountsCtx)
	a.wireNetwork()
	writeInstanceInfo(dir, instanceInfo{PID: os.Getpid(), URL: a.url, Port: port, GUI: gui, Started: time.Now().UnixMilli()})
	defer removeInstanceInfo(dir)

	updCtx, stopUpd := context.WithCancel(context.Background())
	defer stopUpd()
	if att != nil {
		go func() {
			err := att.Run(updCtx, healthCheck(fmt.Sprintf("http://127.0.0.1:%d/healthz", port)))
			switch {
			case errors.Is(err, selfupdate.ErrRolledBack):
				// This version failed its trial and the previous program
				// file is back in place: start it (execve on POSIX, a new
				// process on Windows). Without a usable restart, exit
				// non-zero so a supervisor (systemd Restart=) starts it.
				logger.Warn("self-update: this version failed its trial; restarting into the restored previous version", "reason", err)
				if r := ctl.restarter; r != nil {
					if rerr := r.Restart(context.Background()); rerr != nil && !errors.Is(rerr, selfupdate.ErrExitRequested) {
						logger.Error("self-update: restart into the previous version failed; exiting", "err", rerr)
					}
				}
				a.requestExit(1)
			case errors.Is(err, selfupdate.ErrExitRequested):
				logger.Info("self-update: exiting for the restart", "reason", err)
				a.requestExit(1)
			}
		}()
	}
	if !gui {
		go ctl.loop(updCtx) // automatic updates while update.enabled is on
	}

	// Blocks until the window is closed / a termination signal arrives.
	code := runUI(a, gui)
	a.shutdown()
	return code
}

// requestExit ends a headless run (non-blocking; the first code wins).
func (a *app) requestExit(code int) {
	select {
	case a.exitReq <- code:
	default:
	}
}

// shutdownGrace bounds the whole graceful shutdown; after it the process
// exits anyway (a stuck disk must not keep a service from stopping).
const shutdownGrace = 90 * time.Second

// shutdown stops the HTTP server, then the engine (listeners, feeds,
// uplinks, tracker and lane flush, registry, database), in that order.
func (a *app) shutdown() {
	a.log.Info("shutting down")
	watchdog := time.AfterFunc(shutdownGrace, func() {
		a.log.Error("shutdown did not finish in time; exiting", "grace", shutdownGrace.String())
		os.Exit(1)
	})
	defer watchdog.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := a.srv.Shutdown(ctx); err != nil {
		a.log.Debug("HTTP shutdown", "err", err)
	}
	cancel()
	a.eng.Stop()
	a.log.Info("stopped")
}

// liftMemoryLimit removes the soft heap limit of small devices for an
// aggregator (it holds many stations' state; a 384 MiB target would make
// the collector run constantly). GOMEMLIMIT still wins.
func liftMemoryLimit(log *slog.Logger, cfgMB int) {
	if os.Getenv("GOMEMLIMIT") != "" || cfgMB != 0 {
		return // GOMEMLIMIT, or process.memoryLimitMb applied by applyProcessConfig
	}
	// Inside a container or a systemd unit with MemoryMax, aim the soft
	// limit at 85 % of that cap instead of none.
	if lim := cgroupSoftLimit(readCgroupMax()); lim > 0 {
		debug.SetMemoryLimit(lim)
		log.Info("aggregator mode: soft memory limit from the cgroup", "MiB", lim>>20, "hint", "set GOMEMLIMIT to override")
		return
	}
	// SCALE-02: without a cgroup cap, aim at 80 % of the physical memory
	// so the collector works harder before the host starts swapping.
	if lim := physSoftLimit(readMemTotal()); lim > 0 {
		debug.SetMemoryLimit(lim)
		log.Info("aggregator mode: soft memory limit at 80 % of the physical memory", "MiB", lim>>20,
			"hint", "set process.memoryLimitMb or GOMEMLIMIT to override")
		return
	}
	if prev := debug.SetMemoryLimit(math.MaxInt64); prev != math.MaxInt64 {
		log.Info("aggregator mode: soft memory limit removed", "hint", "set GOMEMLIMIT to cap the heap")
	}
}

// checkOperatorFile returns an error when an explicitly named operator
// config file (-config or $AIRVIEW_CONFIG) is missing or not valid JSONC.
// The implicit <data>/airview.jsonc only produces warnings.
func checkOperatorFile(dir string, f flags) error {
	if f.config == "" && os.Getenv(config.EnvConfigFile) == "" {
		return nil
	}
	_, _, _, err := config.LoadLayered(config.LoadOptions{
		Dir: dir, File: f.config, Environ: os.Environ(), Sets: f.sets, ReadOnly: true,
	})
	return err
}

// startURL is the page the window (or browser) opens: the local UI, or the
// remote shell page pointing at the host in remote-web mode.
func (a *app) startURL() string {
	if a.eng.Mode() == model.ModeRemoteWeb {
		cfg := a.eng.Config().Get()
		if cfg.Remote.Host != "" {
			host := cfg.Remote.Host
			if p := cfg.Remote.HTTPPort; p > 0 && p != 80 {
				host = hostPort(host, p)
			} else if isBareIPv6(host) {
				host = "[" + host + "]"
			}
			return a.url + "remote.html?url=" + url.QueryEscape("http://"+host+"/")
		}
	}
	return a.url
}

func isBareIPv6(host string) bool {
	for i := 0; i < len(host); i++ {
		if host[i] == ':' {
			return host[0] != '['
		}
	}
	return false
}

func hostPort(host string, port int) string {
	if isBareIPv6(host) {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(port)
}

// remoteDataAllowed: the desktop build honours Config.Mode "remote-data";
// the headless Pi build is a standalone station unless AIRVIEW_REMOTE=1.
func remoteDataAllowed() bool {
	if guiAvailable {
		return true
	}
	v := os.Getenv("AIRVIEW_REMOTE")
	return v == "1" || v == "true"
}

// instanceInfo is written to the data directory while the application runs,
// so that a second launch can find the first one.
type instanceInfo struct {
	PID     int    `json:"pid"`
	URL     string `json:"url"`
	Port    int    `json:"port"`
	GUI     bool   `json:"gui"`
	Started int64  `json:"started"`
}

const instanceFileName = "airview.run"

func writeInstanceInfo(dir string, info instanceInfo) {
	b, err := json.Marshal(info)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, instanceFileName), b, 0o644)
}

func readInstanceInfo(dir string) (instanceInfo, bool) {
	var info instanceInfo
	b, err := os.ReadFile(filepath.Join(dir, instanceFileName))
	if err != nil || json.Unmarshal(b, &info) != nil || info.URL == "" {
		return info, false
	}
	return info, true
}

func removeInstanceInfo(dir string) {
	_ = os.Remove(filepath.Join(dir, instanceFileName))
}

// warnOpenAdmin explains a headless standalone server without an admin
// password (SEC-R2): only this machine and private-network clients are
// admin then; everybody else can only read.
func warnOpenAdmin(logger *slog.Logger, h model.HTTPConfig) {
	if h.AdminPasswordHash != "" {
		return
	}
	if h.AllowOpenAdmin {
		logger.Warn("http.allowOpenAdmin is set and there is no admin password: EVERY client that reaches this server can change its settings, reset it and read its database")
		return
	}
	logger.Warn("no admin password: only this machine and private-network clients (RFC 1918, IPv6 ULA, link-local) may change settings; " +
		"set one in Settings (from this machine or the LAN) or http.adminPasswordHash in the operator configuration file; http.allowOpenAdmin=true restores open access")
}
