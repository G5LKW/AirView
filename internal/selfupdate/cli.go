package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

// Exit codes of RunCommand.
const (
	ExitOK            = 0  // updated, or already up to date
	ExitError         = 1  // check, download, verification or install failed (nothing changed)
	ExitUsage         = 2  // bad option
	ExitNotConfigured = 3  // no manifest URL / key
	ExitRolledBack    = 4  // the new version was unhealthy; the previous one runs again
	ExitAvailable     = 10 // -check: a newer version exists
)

// CommandOptions are what a program supplies to RunCommand: its identity,
// the "update" settings from its configuration and its service defaults.
type CommandOptions struct {
	Product  string
	Version  string
	Settings Settings
	// Keys compiled into the program (optional), trusted in addition to
	// Settings.Keys.
	Keys     []PublicKey
	StateDir string
	// Exe defaults to the running program.
	Exe string
	// Service is the default service to restart: the systemd unit on
	// Linux ("airview-feeder", "airview"), the service name on Windows
	// ("AirViewFeeder"). -service overrides it.
	Service string
	// HealthURL is the default health URL (-health-url overrides,
	// Settings.HealthURL comes first when set).
	HealthURL string
	// HealthAccept are the HTTP codes that count as alive (default 200).
	HealthAccept []int
	// HealthHeader is sent with health requests.
	HealthHeader http.Header
	// Name is the program name used in messages ("airview-feeder").
	Name string
	// Restarter and Probe, when set, replace the service control chosen by
	// -service (programs with their own supervision; tests).
	Restarter Restarter
	Probe     Probe
}

// RunCommand implements the "update" subcommand shared by airview and
// airview-feeder:
//
//	update [-check] [-status] [-json] [-force] [-yes] [-channel stable|beta]
//	       [-manifest-url URL] [-key BASE64]... [-service NAME]
//	       [-health-url URL] [-health-window 60s]
//
// It must run with the rights to replace the program file and to restart
// its service (root / administrator). See the package documentation.
func RunCommand(args []string, stdout, stderr io.Writer, o CommandOptions) int {
	name := o.Name
	if name == "" {
		name = o.Product
	}
	fs := flag.NewFlagSet(name+" update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		checkOnly = fs.Bool("check", false, "only check whether a newer version exists (exit 10 if so)")
		status    = fs.Bool("status", false, "print the state of the last check and update")
		asJSON    = fs.Bool("json", false, "machine-readable output")
		force     = fs.Bool("force", false, "install the channel's version even if it is not newer (reinstall / downgrade)")
		channel   = fs.String("channel", "", "release channel: stable or beta (default: update.channel)")
		murl      = fs.String("manifest-url", "", "manifest URL (default: update.manifestUrl)")
		service   = fs.String("service", o.Service, "service to restart and supervise (systemd unit on Linux, service name on Windows)")
		hurl      = fs.String("health-url", "", "health URL polled after the restart")
		window    = fs.Duration("health-window", 0, "how long the new version must stay healthy (default: update.healthWindowS)")
		keys      multiFlag
	)
	fs.Var(&keys, "key", "trusted release public key (base64; repeatable; default: update.keys)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s update: unexpected argument %q\n", name, fs.Arg(0))
		return ExitUsage
	}
	s := o.Settings
	if *channel != "" {
		s.Channel = *channel
	}
	if *murl != "" {
		s.ManifestURL = *murl
	}
	if len(keys) > 0 {
		s.Keys = keys
	}
	if *hurl != "" {
		s.HealthURL = *hurl
	}
	if s.HealthURL == "" {
		s.HealthURL = o.HealthURL
	}
	s.Enabled = false // the command never depends on the automatic switch
	if err := s.Check(); err != nil {
		fmt.Fprintf(stderr, "%s update: %v\n", name, err)
		return ExitUsage
	}
	uo := Options{Product: o.Product, CurrentVersion: o.Version, Settings: s, Keys: o.Keys, Exe: o.Exe, StateDir: o.StateDir,
		Logf: func(f string, a ...any) {
			if !*asJSON {
				fmt.Fprintf(stdout, strings.TrimPrefix(f, "selfupdate: ")+"\n", a...)
			}
		}}
	if *window > 0 {
		uo.Health.Window = *window
	}
	if !*status && !*checkOnly && o.Restarter != nil {
		uo.Restarter, uo.Probe = o.Restarter, o.Probe
		if s.HealthURL != "" {
			hp := HTTPProbe{URL: s.HealthURL, Accept: o.HealthAccept, Header: o.HealthHeader}
			if o.Probe == nil {
				uo.Probe = hp
			} else {
				uo.Probe = AllProbes{o.Probe, hp}
			}
		}
	} else if !*status && !*checkOnly {
		if *service == "" {
			fmt.Fprintf(stderr, "%s update: -service is required (the service that runs %s)\n", name, name)
			return ExitUsage
		}
		r, p := serviceControl(*service)
		uo.Restarter = r
		if s.HealthURL != "" {
			p = AllProbes{p, HTTPProbe{URL: s.HealthURL, Accept: o.HealthAccept, Header: o.HealthHeader}}
		}
		uo.Probe = p
	}
	u, err := New(uo)
	if err != nil {
		fmt.Fprintf(stderr, "%s update: %v\n", name, err)
		return ExitError
	}
	if *status {
		st := u.Status()
		if *asJSON {
			writeJSON(stdout, st)
		} else {
			fmt.Fprintf(stdout, "%s %s (%s), channel %s, automatic updates %s\n", st.Product, st.Version, st.Target, st.Channel, onOff(o.Settings.Enabled))
			if st.LastCheck != "" {
				fmt.Fprintf(stdout, "last check: %s", st.LastCheck)
				if st.Available != "" {
					fmt.Fprintf(stdout, ", %s available", st.Available)
				}
				if st.LastError != "" {
					fmt.Fprintf(stdout, ", error: %s", st.LastError)
				}
				fmt.Fprintln(stdout)
			}
			if st.Last.Phase != "" {
				fmt.Fprintf(stdout, "last update: %s -> %s %s %s\n", st.Last.From, st.Last.To, st.Last.Phase, st.Last.Result)
			}
		}
		return ExitOK
	}
	if !u.Configured() {
		fmt.Fprintf(stderr, "%s update: %v\n", name, ErrNotConfigured)
		return ExitNotConfigured
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	res, err := u.Update(ctx, UpdateOptions{CheckOnly: *checkOnly, Force: *force})
	if *asJSON {
		out := struct {
			Result
			Error string `json:"error,omitempty"`
		}{Result: res}
		if err != nil {
			out.Error = err.Error()
		}
		writeJSON(stdout, out)
	}
	switch {
	case errors.Is(err, ErrRolledBack):
		if !*asJSON {
			fmt.Fprintf(stderr, "%s update: %v\n", name, err)
		}
		return ExitRolledBack
	case errors.Is(err, ErrNotConfigured):
		return ExitNotConfigured
	case err != nil:
		if !*asJSON {
			fmt.Fprintf(stderr, "%s update: %v\n", name, err)
		}
		return ExitError
	}
	if !*asJSON {
		switch res.Outcome {
		case OutcomeUpToDate:
			fmt.Fprintf(stdout, "%s %s is up to date\n", name, res.From)
		case OutcomeAvailable:
			fmt.Fprintf(stdout, "%s %s is available (installed: %s)\n", name, res.To, res.From)
		case OutcomeUpdated:
			fmt.Fprintf(stdout, "%s updated %s -> %s and healthy\n", name, res.From, res.To)
		default:
			fmt.Fprintf(stdout, "%s: %s %s -> %s\n", name, res.Outcome, res.From, res.To)
		}
	}
	if res.Outcome == OutcomeAvailable {
		return ExitAvailable
	}
	return ExitOK
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
