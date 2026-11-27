package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type usageError struct{ error }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmds := map[string]func([]string, io.Writer, io.Writer) error{
		"pack": cmdPack, "sums": cmdSums, "notices": cmdNotices, "check": cmdCheck,
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return 0
	}
	c, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "relkit: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
	if err := c(args[1:], stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "relkit %s: %v\n", args[0], err)
		var ue usageError
		if errors.As(err, &ue) {
			return 2
		}
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `AirView release kit

  pack    -dir STAGE -o NAME.tar.gz [-mtime UNIX]     root-owned, mode-exact, reproducible tar.gz
  sums    -dir DIR [-o FILE] [-match REGEXP]          SHA256SUMS of the release artifacts
  notices -pkg PKG -goos OS -goarch ARCH [-web DIR] -o FILE
                                                     THIRD-PARTY-NOTICES.txt of a program
  check   -dir DIR [-version V] [-require a,b] [-sums=false]
                                                     verify and list every artifact
`)
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("relkit "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	return nil
}
