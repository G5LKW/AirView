package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"airview/internal/auth"
	"airview/internal/config"
	"airview/internal/engine"
	"airview/internal/model"
)

// runAdmin implements "airview admin init|reset" (DESIGN §7, §20.1):
//
//   - init creates the first aggregator admin (password + authenticator)
//     and prints the recovery codes. It refuses when an admin is set up.
//   - reset is the break-glass path for whoever has access to the data
//     directory: it sets a new password on an admin account, replaces its
//     authenticator, ends its sessions and prints new recovery codes.
//
// Both open <data>/stations.db directly; SQLite WAL lets this run beside a
// live server, which reads accounts from the database on every login.
// Passwords are read from the terminal without echo (or one per line from
// standard input when it is not a terminal) and never logged.
func runAdmin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintf(stderr, "Usage: airview admin init|reset [-data dir] [-config file] [-user name]\n\n"+
			"  init   create the first aggregator admin account (password + authenticator)\n"+
			"  reset  break-glass: new password and authenticator for an admin account\n")
	}
	if len(args) == 0 || (args[0] != "init" && args[0] != "reset") {
		usage()
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("airview admin "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "data `directory` (default: $AIRVIEW_DATA, else the default data directory)")
	cfgFile := fs.String("config", "", "operator config `file` (default: $AIRVIEW_CONFIG)")
	user := fs.String("user", "admin", "admin user `name`")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *data == "" {
		*data = os.Getenv(config.EnvDataDir)
	}
	dir, err := config.ResolveDataDir(*data)
	if err != nil {
		fmt.Fprintf(stderr, "data directory: %v\n", err)
		return 1
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	cfg, _, _, err := config.LoadLayered(config.LoadOptions{Dir: dir, File: *cfgFile, Environ: os.Environ(), ReadOnly: true})
	if err != nil {
		fmt.Fprintf(stderr, "configuration: %v\n", err)
		return 1
	}
	if cfg.Mode != model.ModeAggregator {
		fmt.Fprintf(stderr, "note: this data directory is not configured for aggregator mode (mode %q); the account is stored anyway\n", cfg.Mode)
	}
	reg, err := engine.OpenRegistry(dir, cfg.Aggregator, nil)
	if err != nil {
		fmt.Fprintf(stderr, "station registry %s: %v\n", filepath.Join(dir, "stations.db"), err)
		return 1
	}
	defer reg.Close()
	svc := auth.NewService(reg.Accounts(), reg.Sessions(), engine.AccountOptions(cfg.Aggregator))
	return adminFlow(context.Background(), svc, sub, *user, newSecretReader(stdin, stdout), stdout, stderr)
}

// adminFlow runs the interactive part of init/reset against svc.
func adminFlow(ctx context.Context, svc *auth.Service, sub, user string, in *secretReader, stdout, stderr io.Writer) int {
	pw, err := in.secret("New password for " + user + ": ")
	if err != nil {
		fmt.Fprintf(stderr, "password: %v\n", err)
		return 1
	}
	pw2, err := in.secret("Repeat the password: ")
	if err != nil {
		fmt.Fprintf(stderr, "password: %v\n", err)
		return 1
	}
	if pw != pw2 {
		fmt.Fprintln(stderr, "the passwords differ")
		return 1
	}
	var setup auth.TOTPSetup
	if sub == "init" {
		setup, err = svc.InitAdmin(ctx, user, pw)
	} else {
		setup, err = svc.ResetAdmin(ctx, user, pw)
	}
	if err != nil {
		if errors.Is(err, auth.ErrAdminExists) {
			fmt.Fprintln(stderr, "an admin account is already set up; use \"airview admin reset\" to replace its password and authenticator")
		} else {
			fmt.Fprintf(stderr, "%s: %v\n", sub, err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "\nAdd this account to your authenticator app:\n  secret: %s\n  URI:    %s\n\n", setup.Secret, setup.URI)
	for try := 0; try < 3; try++ {
		code, err := in.line("Enter the 6-digit code from the authenticator: ")
		if err != nil {
			fmt.Fprintf(stderr, "code: %v\n", err)
			return 1
		}
		codes, err := svc.ConfirmAdminTOTP(ctx, user, strings.TrimSpace(code))
		if err != nil {
			fmt.Fprintf(stderr, "the code was not accepted: %v\n", err)
			continue
		}
		fmt.Fprintf(stdout, "\nAdmin %q is ready. Recovery codes (each works once; store them offline):\n", user)
		for _, c := range codes {
			fmt.Fprintf(stdout, "  %s\n", c)
		}
		return 0
	}
	fmt.Fprintf(stderr, "no valid code; run \"airview admin %s\" again\n", sub)
	return 1
}

// secretReader reads lines and passwords from the console or a pipe.
type secretReader struct {
	r   *bufio.Reader
	f   *os.File // non-nil when stdin is a terminal file (echo can be switched off)
	out io.Writer
}

func newSecretReader(stdin io.Reader, out io.Writer) *secretReader {
	s := &secretReader{r: bufio.NewReader(stdin), out: out}
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		s.f = f
	}
	return s
}

func (s *secretReader) line(prompt string) (string, error) {
	fmt.Fprint(s.out, prompt)
	l, err := s.r.ReadString('\n')
	if err != nil && (err != io.EOF || l == "") {
		return "", err
	}
	return strings.TrimRight(l, "\r\n"), nil
}

// secret reads one line with echo switched off when stdin is a terminal.
func (s *secretReader) secret(prompt string) (string, error) {
	if s.f != nil {
		if restore, err := noEcho(s.f); err == nil {
			defer func() {
				restore()
				fmt.Fprintln(s.out)
			}()
		}
	}
	return s.line(prompt)
}
