// Command airview-feeder is the headless AirView feeder (DESIGN §12.1): it
// reads one or more local receivers (readsb / dump1090 / Radarcape / any
// Beast, AVR or SBS output), forwards their frames with receiver timestamps
// to one or more AirView aggregators over AVF/1 (internal/uplink), delivers
// the MLAT results it gets back to local programs, and serves a small
// status page on 127.0.0.1:8099.
//
//	airview-feeder [run] [-config file] [-data dir] [-set path=value]...
//	airview-feeder setup [options]          write feeder.json (interactive or --non-interactive)
//	airview-feeder status [-json]           state of the running feeder
//	airview-feeder claim [-uplink id] [-rx n]  print a claim code
//	airview-feeder config check|print|get|set|env|example|path
//	airview-feeder detect [-host h] [-port p]  look for receivers
//	airview-feeder new-identity             new feeder key (keeps the UUID)
//	airview-feeder uninstall-helper [-json] [-purge -yes]
//	airview-feeder version
//
// See cmd/airview-feeder/README.md for the full reference.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"airview/internal/model"
)

// version can be overridden at build time: -ldflags "-X main.version=1.2.3".
var version = model.Version

// command is one subcommand. Run returns the process exit code.
type command struct {
	Summary string
	Run     func(args []string, stdout, stderr io.Writer) int
}

// commands holds the subcommands. Platform files (the Windows service and
// tray of WP-WF, *_windows.go) add theirs from init functions.
var commands = map[string]command{}

func init() {
	commands["run"] = command{"run the feeder (default)", cmdRun}
	commands["setup"] = command{"create or change feeder.json (interactive, or --non-interactive)", cmdSetup}
	commands["status"] = command{"show the state of the running feeder", cmdStatus}
	commands["claim"] = command{"get a claim code to link this feeder to your account", cmdClaim}
	commands["config"] = command{"check|print|get|set|env|example|path: work with feeder.json", cmdConfig}
	commands["detect"] = command{"look for receivers on this computer or a given host", cmdDetect}
	commands["new-identity"] = command{"create a new feeder key (after cloning an SD card); keeps the UUID", cmdNewIdentity}
	commands["uninstall-helper"] = command{"list (or with -purge -yes delete) the feeder's own files", cmdUninstallHelper}
	commands["update"] = command{"check for and install a signed update published by the aggregator", cmdUpdate}
	commands["version"] = command{"print the version", cmdVersion}
	commands["help"] = command{"show this help", nil}
}

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr))
}

func runMain(args []string, stdout, stderr io.Writer) int {
	name := "run"
	if len(args) > 0 {
		switch args[0] {
		case "-h", "-help", "--help", "-?", "/?":
			// UX-13: the top-level help is the command overview (the
			// options of "run" are under "airview-feeder run -h").
			usage(stdout)
			return 0
		}
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	switch name {
	case "-version", "--version":
		return cmdVersion(nil, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	}
	if len(args) > 0 && (args[0] == "--version" || args[0] == "-version") && name == "run" {
		return cmdVersion(nil, stdout, stderr)
	}
	c, ok := commands[name]
	if !ok || c.Run == nil {
		fmt.Fprintf(stderr, "airview-feeder: unknown command %q\n\n", name)
		usage(stderr)
		return 2
	}
	return c.Run(args, stdout, stderr)
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "airview-feeder %s - feeds local ADS-B / Mode S receivers to AirView aggregators\n\nUsage: airview-feeder <command> [options]\n\nCommands:\n", version)
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-17s %s\n", n, commands[n].Summary)
	}
	fmt.Fprintf(w, "\nCommon options: -config <file> (default %s), -data <dir> (default %s)\nRun \"airview-feeder <command> -h\" for the options of a command.\n",
		defaultConfigFile(""), defaultDataDir())
}

func cmdVersion(_ []string, stdout, _ io.Writer) int {
	fmt.Fprintf(stdout, "airview-feeder %s\n", version)
	return 0
}

// common holds the options every command takes.
type common struct {
	config string
	data   string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.config, "config", "", "configuration `file` (default: $AIRVIEW_FEEDER_CONFIG, else the platform default)")
	fs.StringVar(&c.data, "data", "", "data `directory` for the identity, state and spool (default: $AIRVIEW_FEEDER_DATA, else the platform default)")
}

// resolve fills in the defaults.
func (c *common) resolve() {
	if c.data == "" {
		c.data = defaultDataDir()
	}
	if c.config == "" {
		c.config = defaultConfigFile(c.data)
	}
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("airview-feeder "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags parses; it returns -1 to continue, else an exit code.
func parseFlags(fs *flag.FlagSet, args []string) int {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	return -1
}

// ---- run ---------------------------------------------------------------------

func cmdRun(args []string, stdout, stderr io.Writer) int {
	var c common
	var sets []string
	var uiAddr string
	fs := newFlagSet("run", stderr)
	c.register(fs)
	fs.Func("set", "override one setting, `path=value` (repeatable), e.g. -set ui.addr=127.0.0.1:8100", func(v string) error {
		if !strings.Contains(v, "=") {
			return fmt.Errorf("want path=value")
		}
		sets = append(sets, v)
		return nil
	})
	fs.StringVar(&uiAddr, "ui", "", "status page listen `address` (overrides ui.addr for this run)")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	c.resolve()

	lg := log.New(stderr, "", log.LstdFlags)
	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("JOURNAL_STREAM") != "" {
		lg.SetFlags(0) // journald adds its own timestamps
	}
	logf := func(format string, a ...any) { lg.Printf(format, a...) }
	slogger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := os.MkdirAll(c.data, 0o750); err != nil {
		logf("data directory %s: %v", c.data, err)
		return 1
	}
	release, err := lockDataDir(c.data)
	if err != nil {
		logf("%v (%s)", err, c.data)
		return 1
	}
	defer release()

	f, err := startFeeder(runOptions{ConfigFile: c.config, DataDir: c.data, Environ: os.Environ(), Sets: sets,
		Logf: logf, Logger: slogger, UIAddr: uiAddr})
	if err != nil {
		logf("%v", err)
		return 1
	}
	sdNotify("READY=1\nSTATUS=feeding")

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	var wd <-chan time.Time
	if iv := sdWatchdogInterval(); iv > 0 {
		t := time.NewTicker(iv)
		defer t.Stop()
		wd = t.C
	}
	for {
		select {
		case s := <-sig:
			if s == syscall.SIGHUP {
				f.reload()
				continue
			}
			logf("received %v, stopping", s)
			sdNotify("STOPPING=1")
			f.stop()
			return 0
		case <-wd:
			sdNotify("WATCHDOG=1")
		case code := <-f.exitReq:
			sdNotify("STOPPING=1")
			f.stop()
			return code
		}
	}
}
