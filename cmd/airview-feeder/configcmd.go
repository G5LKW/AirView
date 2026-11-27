package main

import (
	"airview/internal/selfupdate"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"

	"airview/internal/config"
	"airview/internal/model"
)

// cmdConfig implements "airview-feeder config check|print|get|set|env|example|path".
func cmdConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "Usage: airview-feeder config check|print|get <path>|set <path=value>...|env|example|path [-config file] [-data dir]")
		return 2
	}
	sub, rest := args[0], args[1:]
	var c common
	var showSecrets bool
	fs := newFlagSet("config "+sub, stderr)
	c.register(fs)
	if sub == "print" {
		fs.BoolVar(&showSecrets, "show-secrets", false, "print enrol tokens and the password hash")
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	pos := fs.Args()
	c.resolve()
	switch sub {
	case "check":
		return configCheck(c, stdout)
	case "print":
		lc := loadForCLI(c, stderr)
		cfg := lc.Eff
		if !showSecrets {
			cfg = redactConfig(cfg, "(redacted)")
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(cfg)
		return 0
	case "get":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "Usage: airview-feeder config get <path>")
			return 2
		}
		p, _, err := settingPath(pos[0])
		if err != nil {
			fmt.Fprintln(stderr, "airview-feeder:", err)
			return 2
		}
		lc := loadForCLI(c, stderr)
		doc, _ := toDoc(redactConfig(lc.Eff, "(redacted)"))
		var cur any = doc
		for _, s := range p {
			switch v := cur.(type) {
			case map[string]any:
				cur = v[s.key]
			case []any:
				if s.idx >= len(v) {
					fmt.Fprintf(stderr, "airview-feeder: %s: no such element\n", pos[0])
					return 1
				}
				cur = v[s.idx]
			default:
				cur = nil
			}
		}
		if s, ok := cur.(string); ok {
			fmt.Fprintln(stdout, s)
		} else {
			b, _ := json.MarshalIndent(cur, "", "  ")
			fmt.Fprintln(stdout, string(b))
		}
		return 0
	case "set":
		if len(pos) == 0 {
			fmt.Fprintln(stderr, "Usage: airview-feeder config set <path=value>...")
			return 2
		}
		return configSet(c, pos, stdout, stderr)
	case "env":
		configEnv(stdout)
		return 0
	case "example":
		fmt.Fprint(stdout, exampleConfig())
		return 0
	case "path":
		fmt.Fprintf(stdout, "config file: %s\ndata dir:    %s\n", c.config, c.data)
		return 0
	}
	fmt.Fprintf(stderr, "airview-feeder: unknown config command %q\n", sub)
	return 2
}

// configCheck validates the configuration without changing anything. Exit
// code 1 when there are errors.
func configCheck(c common, stdout io.Writer) int {
	lc, _ := loadConfig(loadOptions{File: c.config, Environ: os.Environ()})
	if !lc.Exists {
		fmt.Fprintf(stdout, "%s does not exist: the defaults are used (no aggregator; run airview-feeder setup)\n", c.config)
	} else if lc.Comment {
		fmt.Fprintf(stdout, "note: %s contains comments or trailing commas; setup, config set and the status page rewrite it without them\n", c.config)
	}
	for _, is := range lc.Issues {
		fmt.Fprintln(stdout, is)
	}
	for _, n := range lc.Notes {
		fmt.Fprintf(stdout, "warning: %s\n", n)
	}
	warn := semanticWarnings(lc.Eff)
	for _, w := range warn {
		fmt.Fprintf(stdout, "warning: %s\n", w)
	}
	errs := lc.errorCount()
	warns := len(lc.Issues) - errs + len(lc.Notes) + len(warn)
	if errs > 0 {
		fmt.Fprintf(stdout, "%s: %d error(s), %d warning(s)\n", c.config, errs, warns)
		return 1
	}
	fmt.Fprintf(stdout, "%s: OK (%d warning(s)); %d receiver(s), %d aggregator(s)\n", c.config, warns, len(lc.Eff.Receivers), len(lc.Eff.Uplinks))
	return 0
}

// semanticWarnings flags settings that are valid but probably not intended.
func semanticWarnings(c model.FeederConfig) []string {
	var out []string
	if len(c.Uplinks) == 0 {
		out = append(out, "no aggregator (uplinks) is configured: nothing is fed")
	}
	for i, u := range c.Uplinks {
		p := fmt.Sprintf("uplinks[%d]", i)
		if u.Host == "" {
			out = append(out, p+".host is empty")
		}
		if u.IsBeast() {
			continue // a Beast output has no TLS and no AirView privacy settings
		}
		if u.ServerPin == "" {
			if _, err := netip.ParseAddr(u.Host); err == nil {
				out = append(out, p+": no serverPin for an IP address; the server certificate must then be signed by a public CA for that IP (use the pin from the aggregator's install command)")
			}
		} else if !strings.HasPrefix(u.ServerPin, "sha256/") {
			out = append(out, p+".serverPin should look like sha256/<base64>")
		}
		if !u.Timestamps {
			out = append(out, p+": timestamps are off; this station cannot take part in MLAT")
		}
	}
	for i, r := range c.Receivers {
		if !r.Located {
			out = append(out, fmt.Sprintf("receivers[%d]: no declared position; the aggregator has to locate the station (consent needed)", i))
		}
	}
	if host, _, err := net.SplitHostPort(c.UI.Addr); err == nil {
		if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
			out = append(out, "ui.addr "+c.UI.Addr+" makes the status page reachable from the network (password protected)")
		}
	}
	us := selfupdate.SettingsFromModel(c.Update)
	us.Enabled = false // autoUpdate only switches on with a URL and a key
	if err := us.Check(); err != nil {
		out = append(out, strings.Split(err.Error(), "\n")...)
	}
	if c.AutoUpdate && len(c.Update.Keys) == 0 {
		out = append(out, "autoUpdate is on but update.keys is not set: no automatic updates (the installers write it)")
	} else if c.AutoUpdate && c.Update.ManifestURL == "" && derivedManifestURL(c.Uplinks) == "" {
		out = append(out, "autoUpdate is on but update.manifestUrl is not set and there is no aggregator uplink to derive it from (https://<host>/feeder/manifest.json): no automatic updates")
	}
	return out
}

// configSet edits the file layer.
func configSet(c common, assigns []string, stdout, stderr io.Writer) int {
	lc, err := loadConfig(loadOptions{File: c.config})
	if err != nil && lc.Exists {
		fmt.Fprintf(stderr, "airview-feeder: %v (fix the file first)\n", err)
		return 1
	}
	doc, err := toDoc(lc.FileCfg)
	if err != nil {
		fmt.Fprintln(stderr, "airview-feeder:", err)
		return 1
	}
	for _, a := range assigns {
		path, raw, ok := strings.Cut(a, "=")
		if !ok {
			fmt.Fprintf(stderr, "airview-feeder: %q: want path=value\n", a)
			return 2
		}
		p, t, err := settingPath(path)
		if err != nil {
			fmt.Fprintln(stderr, "airview-feeder:", err)
			return 2
		}
		if pathString(p) == "ui.passwordHash" && raw != "" && !config.IsPasswordHash(raw) {
			raw = hashPassword(raw)
		}
		v, err := parseTextValue(t, raw)
		if err != nil {
			fmt.Fprintf(stderr, "airview-feeder: %s: %v\n", path, err)
			return 2
		}
		if doc, err = setPath(doc, p, v); err != nil {
			fmt.Fprintf(stderr, "airview-feeder: %s: %v\n", path, err)
			return 2
		}
	}
	b, _ := json.Marshal(doc)
	cfg, err := decodeFeederConfig(b)
	if err != nil {
		fmt.Fprintln(stderr, "airview-feeder:", err)
		return 2
	}
	for _, n := range normalizeFeeder(&cfg) {
		fmt.Fprintf(stdout, "note: %s\n", n)
	}
	if err := saveConfig(c.config, cfg); err != nil {
		fmt.Fprintf(stderr, "airview-feeder: cannot write %s: %v\n", c.config, err)
		return 1
	}
	fmt.Fprintf(stdout, "saved %s; restart the feeder (or send it SIGHUP) to apply\n", c.config)
	return 0
}

// configEnv lists the environment variables.
func configEnv(w io.Writer) {
	fmt.Fprintln(w, "airview-feeder reads these environment variables (they override feeder.json).")
	fmt.Fprintln(w, "List elements are numbered from 0: AIRVIEW_FEEDER_UPLINKS_0_HOST. <NAME>_FILE reads the value from a file.")
	fmt.Fprintln(w, "Lists of strings take \"a,b\" or JSON; lists of objects take JSON.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-48s %s\n", envConfigFile, "configuration file (like -config)")
	fmt.Fprintf(w, "%-48s %s\n", envDataDir, "data directory (like -data)")
	for _, p := range leafPaths() {
		segs, _ := parsePath(strings.ReplaceAll(p, "[]", "[0]"))
		name := strings.ReplaceAll(envName(segs), "_0_", "_<n>_")
		if strings.HasSuffix(name, "_0") {
			name = strings.TrimSuffix(name, "_0") + "_<n>"
		}
		fmt.Fprintf(w, "%-48s %s\n", name, strings.ReplaceAll(p, "[]", "[n]"))
	}
}
