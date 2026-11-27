package config

import (
	"airview/internal/federation"
	"airview/internal/selfupdate"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"airview/internal/model"
)

// Report is the result of Check ("airview config check").
type Report struct {
	// ConfigJSON is the path of config.json; ConfigJSONFound tells whether
	// it exists.
	ConfigJSON      string
	ConfigJSONFound bool
	// OperatorFile is the operator file that was read ("" = none).
	OperatorFile string
	// Issues in source order: config.json, operator file, environment,
	// -set, then problems of the merged configuration.
	Issues []Issue
	// Effective is the configuration AirView would run with (not redacted).
	Effective model.Config
	// Overrides are the settings locked by the operator file, environment
	// and -set.
	Overrides []Override
}

// Errors returns the number of error issues.
func (r Report) Errors() int { return r.count(SevError) }

// Warnings returns the number of warning issues.
func (r Report) Warnings() int { return r.count(SevWarning) }

func (r Report) count(s Severity) int {
	n := 0
	for _, is := range r.Issues {
		if is.Severity == s {
			n++
		}
	}
	return n
}

// Check loads every configuration layer exactly like LoadLayered, without
// creating, rewriting or moving any file, and reports every problem with
// its source, position and setting path:
//
//   - syntax errors (file, line, column);
//   - unknown settings (errors in the operator file; warnings in
//     config.json, which a newer AirView may have written) with a "did you
//     mean" hint;
//   - wrong value types and duplicate settings;
//   - unusable environment variables and -set values, unknown AIRVIEW_*
//     variables (warning);
//   - every value AirView would correct when starting (out of range,
//     unknown enum value, inconsistent combination): an error located at the
//     source that supplied the value.
func Check(opts LoadOptions) Report {
	opts.ReadOnly = true
	path := filepath.Join(opts.Dir, FileName)
	r := Report{ConfigJSON: path}

	base := model.DefaultConfig()
	var root *Node
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		r.Issues = append(r.Issues, Issue{Severity: SevError, Source: path, Msg: "cannot be read: " + err.Error()})
	default:
		r.ConfigJSONFound = true
		n, perr := ParseJSONC(data)
		if perr != nil {
			is := Issue{Severity: SevError, Source: path, Msg: perr.Error()}
			var se *SyntaxError
			if errors.As(perr, &se) {
				is.Pos, is.Msg = se.Pos, se.Msg
			}
			is.Msg += " (AirView would move this file aside and start with the defaults)"
			r.Issues = append(r.Issues, is)
			break
		}
		root = n
		if n.Kind != ObjectNode {
			r.Issues = append(r.Issues, Issue{Severity: SevError, Source: path, Pos: n.Pos,
				Msg: "the file must contain one object {...}, found " + n.Kind.String() + " (AirView would move this file aside and start with the defaults)"})
			root = nil
			break
		}
		r.Issues = append(r.Issues, checkDocument(path, n, SevWarning)...)
		cfg, warns, _, perr := parse(data)
		if perr != nil {
			r.Issues = append(r.Issues, Issue{Severity: SevError, Source: path, Msg: perr.Error()})
			break
		}
		for _, w := range warns {
			if strings.Contains(w, "has the wrong type") {
				continue // reported precisely by checkDocument
			}
			r.Issues = append(r.Issues, Issue{Severity: SevWarning, Source: path, Msg: w})
		}
		base = cfg
	}

	l := &Layers{base: Clone(base)}
	_ = l.loadOperatorFile(opts) // problems are in l.issues
	l.loadEnv(opts.Environ)
	l.loadSets(opts.Sets)
	r.Issues = append(r.Issues, l.issues...)
	r.OperatorFile = l.file

	eff, notes := l.Apply(base)
	for _, note := range notes {
		r.Issues = append(r.Issues, l.locate(note, path, root))
	}
	r.Issues = append(r.Issues, semanticIssues(eff)...)
	r.Effective = eff
	r.Overrides = l.Overrides()
	return r
}

// semanticIssues are warnings about combinations of valid settings that
// do not work as an operator would expect.
func semanticIssues(cfg model.Config) []Issue {
	var out []Issue
	if cfg.Mode == model.ModeAggregator && cfg.Hub.Enabled && cfg.Hub.Password == "" {
		out = append(out, Issue{Severity: SevWarning, Path: "hub.password",
			Msg: "aggregator mode: the data port only accepts the admin with a password; with none set every login is refused"})
	}
	if cfg.Mode == model.ModeAggregator && len(cfg.Aggregator.Limits.AllowCIDRs) == 0 {
		for _, r := range cfg.Aggregator.TrustedRelays {
			if !strings.Contains(r, "@") {
				out = append(out, Issue{Severity: SevWarning, Path: "aggregator.trustedRelays",
					Msg: "relay " + r + " is bound to no address and aggregator.limits.allowCidrs is empty, so it is never trusted; write it as uuid@cidr"})
			}
		}
	}
	if cfg.Mode == model.ModeAggregator && !loopbackBind(cfg.HTTP.BindAddr) {
		switch {
		case cfg.HTTP.AllowInsecureLogin:
			out = append(out, Issue{Severity: SevWarning, Path: "http.allowInsecureLogin",
				Msg: "passwords, TOTP codes and session cookies are accepted over plain HTTP from the network: anyone on the path can take over an admin session; put an HTTPS reverse proxy in front instead"})
		case len(cfg.Aggregator.TrustedProxies) == 0:
			out = append(out, Issue{Severity: SevWarning, Path: "http.bindAddr",
				Msg: "aggregator mode on plain HTTP without a trusted reverse proxy: logins are accepted only from this machine (" + adminInitHint() + "); put an HTTPS reverse proxy in front (aggregator.trustedProxies) for remote logins"})
		}
	}
	if err := selfupdate.SettingsFromModel(cfg.Update).Check(); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			path := "update"
			if i := strings.IndexAny(line, ": "); i > 0 && strings.HasPrefix(line, "update.") {
				path = line[:i]
			}
			out = append(out, Issue{Severity: SevError, Path: path, Msg: line})
		}
	}
	if f := cfg.Aggregator.Federation; f.Enabled || len(f.Peers) > 0 || len(f.Feeds) > 0 {
		fc := federation.FromModel(f)
		fc.Normalize()
		for _, line := range fc.Check("aggregator.federation") {
			path := "aggregator.federation"
			if i := strings.Index(line, ":"); i > 0 && strings.HasPrefix(line, "aggregator.federation") {
				path = line[:i]
			}
			sev := SevError
			if !f.Enabled {
				sev = SevWarning
			}
			out = append(out, Issue{Severity: sev, Path: path, Msg: line})
		}
	}
	return append(out, mlatIssues(cfg)...)
}

// mlatIssues are warnings about the MLAT settings of an aggregator (DESIGN
// §1, §5.2, §5.3, §9.6): combinations that are valid but do not do what an
// operator would expect.
func mlatIssues(cfg model.Config) []Issue {
	if cfg.Mode != model.ModeAggregator {
		return nil
	}
	a, m := cfg.Aggregator, cfg.Aggregator.MLAT
	var out []Issue
	warn := func(path, msg string) { out = append(out, Issue{Severity: SevWarning, Path: path, Msg: msg}) }
	if !m.Enabled {
		if a.MLATPort > 0 {
			warn("aggregator.mlatPort", "the mlat-client listener only runs with the MLAT engine (aggregator.mlat.enabled is false), so it stays closed")
		}
		if m.ReturnResults {
			warn("aggregator.mlat.returnResults", "has no effect while aggregator.mlat.enabled is false")
		}
		return out
	}
	if a.AVFPort == 0 && a.MLATPort == 0 {
		warn("aggregator.mlat.enabled", "the MLAT engine has no timed input: both aggregator.avfPort and aggregator.mlatPort are 0 (the Beast port 30004 is never used for MLAT), so only the host station's own feeds can contribute")
	}
	if a.SBSInLocalPort > 0 {
		warn("aggregator.sbsInLocalPort", "the SBS-in bridge (external mlat-server) runs beside the own MLAT engine: aircraft may get positions from both (mlat-ext and mlat-av); set it to 0 once the own MLAT is trusted")
	}
	if m.Holdout == 0 {
		warn("aggregator.mlat.holdout", "0 turns the hold-out self-validation off: no live accuracy figures (public MLAT quality, mlat_quality_hour) are produced")
	}
	return out
}

// loopbackBind reports whether a bind address only listens on loopback.
func loopbackBind(addr string) bool {
	switch strings.TrimSpace(addr) {
	case "127.0.0.1", "::1", "[::1]", "localhost":
		return true
	}
	return strings.HasPrefix(addr, "127.")
}

var notePathRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]*(\[[0-9]+\])?(\.[a-zA-Z][a-zA-Z0-9]*(\[[0-9]+\])?)*`)

// locate turns a Validate note into an Issue at the source that supplied
// the setting the note is about.
func (l *Layers) locate(note, cfgPath string, cfgRoot *Node) Issue {
	is := Issue{Severity: SevError, Msg: note}
	p := notePathRE.FindString(note)
	segs, ok := parsePath(p)
	if ok {
		_, ok = typeAt(segs)
	}
	if !ok {
		return is
	}
	is.Path = p
	if msg := strings.TrimSpace(strings.TrimPrefix(note, p)); msg != "" {
		is.Msg = strings.TrimSpace(strings.TrimPrefix(msg, ":"))
	}
	// Filling an empty value with its default (an uplink without an id
	// gets a generated one) is not a mistake of the operator (C5).
	if strings.HasPrefix(is.Msg, `"" was changed to`) {
		is.Severity = SevWarning
	}
	// The most specific override covering the path wins (the last applied).
	// An override of another member of the same list element counts too:
	// AIRVIEW_UPLINKS_0_HOST creates uplinks[0], whose filled-in id is
	// then the environment's doing, not config.json's (C5).
	for i := len(l.ops) - 1; i >= 0; i-- {
		o := l.ops[i]
		if hasPrefixSegs(segs, o.segs) || hasPrefixSegs(o.segs, segs) || sameListElement(segs, o.segs) {
			is.Source = o.source
			if l.fileAt != nil && o.source == l.file {
				if _, kp := nodeAt(l.fileAt, segs); kp.IsValid() {
					is.Pos = kp
				} else {
					is.Pos = o.pos
				}
			}
			return is
		}
	}
	if cfgRoot != nil {
		// Only blame config.json when it holds the setting (or the list
		// element / object it belongs to).
		for n := len(segs); n > 0; n-- {
			if _, kp := nodeAt(cfgRoot, segs[:n]); kp.IsValid() {
				is.Source = cfgPath
				if n == len(segs) {
					is.Pos = kp
				}
				break
			}
		}
	}
	return is
}

// sameListElement reports whether a and b are paths inside the same list
// element (their common prefix ends with a list index).
func sameListElement(a, b []seg) bool {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n > 0 && a[n-1].isIndex()
}

// adminInitHint is how the admin account is created on this OS (UX-03: no
// sudo on Windows).
func adminInitHint() string {
	if runtime.GOOS == "windows" {
		return `"airview admin init" in an administrator command prompt`
	}
	return "sudo airview admin init"
}
