package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"airview/internal/config"
	"airview/internal/federation"
)

// runFederation implements "airview federation id": print this network's
// federation node ID (and, with a listener, its TLS pin) for the operators
// of peer networks, who enter it in their aggregator.federation.peers[].
// The identity is created on first use (<data>/federation.key).
func runFederation(args []string, stdout, stderr io.Writer) int {
	usage := "Usage: airview federation id [-data dir] [-config file]"
	if len(args) == 0 || args[0] != "id" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			fmt.Fprintln(stdout, usage)
			return 0
		}
		fmt.Fprintln(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("airview federation id", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "data `directory` (default: $AIRVIEW_DATA, else the default data directory)")
	file := fs.String("config", "", "operator config `file` (default: $AIRVIEW_CONFIG, else <data>/airview.jsonc)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	dir, err := dataDirFromFlag(*data)
	if err != nil {
		fmt.Fprintf(stderr, "data directory: %v\n", err)
		return 1
	}
	r := config.Check(config.LoadOptions{Dir: dir, File: *file, Environ: os.Environ()})
	a := r.Effective.Aggregator
	fc := federation.FromModel(a.Federation)
	fc.Normalize()
	var hosts []string
	if a.PublicHost != "" {
		hosts = []string{a.PublicHost}
	}
	m, err := federation.LoadMaterial(dir, &fc, hosts)
	if err != nil {
		fmt.Fprintf(stderr, "federation identity: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "node ID: %s\n", m.Identity.FeederID())
	if m.Created {
		fmt.Fprintln(stdout, "(a new federation identity was created)")
	}
	if fc.Listen != "" {
		fmt.Fprintf(stdout, "listen:  %s (open %d/tcp inbound for peers that dial in)\n", fc.Listen, federation.DefaultPort)
		if m.Pin != "" {
			fmt.Fprintf(stdout, "pin:     %s\n", m.Pin)
		}
	}
	if !fc.Enabled {
		fmt.Fprintln(stdout, "note: aggregator.federation.enabled is false")
	}
	return 0
}
