// Package cli implements AirView's command-line subcommands that do not
// start the station, starting with "airview config ...". The functions take
// their arguments, output streams and environment explicitly so that
// cmd/airview only has to dispatch to them and tests can run them in-process.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"airview/internal/config"
	"airview/internal/model"
)

// Exit statuses of the config subcommands.
const (
	ExitOK    = 0 // success; for check: no errors (warnings allowed)
	ExitError = 1 // check found errors, or an I/O error occurred
	ExitUsage = 2 // bad command line
)

// Env is the process context a subcommand runs in.
type Env struct {
	Stdout, Stderr io.Writer
	// Environ is the process environment ("KEY=value"), e.g. os.Environ().
	Environ []string
	// ExeDir is the directory of the executable, used to find the default
	// data directory ("data" next to the executable). "" = os.Executable.
	ExeDir string
	// UserConfigDir is the per-user configuration directory. "" =
	// os.UserConfigDir.
	UserConfigDir string
}

// OSEnv returns the Env of the running process.
func OSEnv() Env {
	return Env{Stdout: os.Stdout, Stderr: os.Stderr, Environ: os.Environ()}
}

// IsConfigCommand reports whether args (os.Args[1:]) start the config
// subcommand ("airview config ..."). cmd/airview calls RunConfig then.
func IsConfigCommand(args []string) bool { return len(args) > 0 && args[0] == "config" }

const configUsage = `Usage: airview config <command> [options]

Commands:
  check     validate config.json, the operator file, AIRVIEW_* variables and
            -set values; exit status 0 = valid, 1 = errors, 2 = usage
  print     print the effective configuration (secrets redacted)
  example   print the fully commented example configuration
  env       list the environment variable of every setting

Common options (check, print):
  -data dir       data directory holding config.json (default: $AIRVIEW_DATA,
                  else "data" next to the executable, else the user config dir)
  -config file    operator config file (default: $AIRVIEW_CONFIG, else
                  airview.jsonc in the data directory when present)
  -no-config      ignore the operator file
  -no-env         ignore AIRVIEW_* environment variables
  -set path=value override one setting (repeatable), e.g. -set http.port=8080

Reference: docs/CONFIG.md
`

// RunConfig runs "airview config <args...>" and returns the exit status.
// args excludes the word "config".
func RunConfig(args []string, env Env) int {
	if env.Stdout == nil {
		env.Stdout = io.Discard
	}
	if env.Stderr == nil {
		env.Stderr = io.Discard
	}
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, configUsage)
		return ExitUsage
	}
	switch args[0] {
	case "check":
		return runCheck(args[1:], env)
	case "print":
		return runPrint(args[1:], env)
	case "example":
		return runExample(args[1:], env)
	case "env":
		return runEnv(args[1:], env)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(env.Stdout, configUsage)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "airview config: unknown command %q\n\n%s", args[0], configUsage)
	return ExitUsage
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// sourceFlags are the options shared by check and print.
type sourceFlags struct {
	data, file     string
	noFile, noEnv  bool
	sets           multiFlag
	jsonOut, quiet bool
	sources        bool
}

func newFlagSet(name string, env Env, sf *sourceFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("airview config "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.StringVar(&sf.data, "data", "", "data `directory` holding config.json")
	fs.StringVar(&sf.file, "config", "", "operator config `file` (JSON with comments)")
	fs.BoolVar(&sf.noFile, "no-config", false, "ignore the operator config file")
	fs.BoolVar(&sf.noEnv, "no-env", false, "ignore AIRVIEW_* environment variables")
	fs.Var(&sf.sets, "set", "override a setting: `path=value` (repeatable)")
	fs.BoolVar(&sf.jsonOut, "json", false, "machine-readable JSON output")
	return fs
}

func parseArgs(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, false
		}
		return ExitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return ExitUsage, false
	}
	return 0, true
}

// loadOptions resolves the data directory (without creating anything) and
// builds the config.LoadOptions.
func (sf *sourceFlags) loadOptions(env Env) config.LoadOptions {
	o := config.LoadOptions{
		Dir:    findDataDir(sf.data, env),
		File:   sf.file,
		NoFile: sf.noFile,
		Sets:   sf.sets,
	}
	if !sf.noEnv {
		o.Environ = env.Environ
	}
	return o
}

// findDataDir mirrors config.ResolveDataDir without creating directories:
// -data, $AIRVIEW_DATA, "data" next to the executable if it exists, the
// user config directory if it exists, else "data" next to the executable.
func findDataDir(flagValue string, env Env) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return absPath(v)
	}
	if v, ok := lookup(env.Environ, config.EnvDataDir); ok && strings.TrimSpace(v) != "" {
		return absPath(strings.TrimSpace(v))
	}
	exeDir := env.ExeDir
	if exeDir == "" {
		if exe, err := os.Executable(); err == nil {
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				exe = real
			}
			exeDir = filepath.Dir(exe)
		}
	}
	next := filepath.Join(exeDir, "data")
	if isDir(next) {
		return next
	}
	ucd := env.UserConfigDir
	if ucd == "" {
		ucd, _ = os.UserConfigDir()
	}
	if ucd != "" {
		if d := filepath.Join(ucd, model.AppName); isDir(d) {
			return d
		}
	}
	return next
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func lookup(environ []string, key string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(environ[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// --- check -------------------------------------------------------------------

type jsonIssue struct {
	Severity string `json:"severity"`
	Source   string `json:"source,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

func runCheck(args []string, env Env) int {
	var sf sourceFlags
	fs := newFlagSet("check", env, &sf)
	fs.BoolVar(&sf.quiet, "q", false, "print errors only")
	if code, ok := parseArgs(fs, args); !ok {
		return code
	}
	opts := sf.loadOptions(env)
	rep := config.Check(opts)

	if sf.jsonOut {
		out := struct {
			Valid        bool              `json:"valid"`
			Errors       int               `json:"errors"`
			Warnings     int               `json:"warnings"`
			DataDir      string            `json:"dataDir"`
			ConfigJSON   string            `json:"configJson"`
			Found        bool              `json:"configJsonFound"`
			OperatorFile string            `json:"operatorFile,omitempty"`
			Overrides    []config.Override `json:"overrides"`
			Issues       []jsonIssue       `json:"issues"`
		}{
			Valid: rep.Errors() == 0, Errors: rep.Errors(), Warnings: rep.Warnings(),
			DataDir: opts.Dir, ConfigJSON: rep.ConfigJSON, Found: rep.ConfigJSONFound,
			OperatorFile: rep.OperatorFile, Overrides: rep.Overrides, Issues: []jsonIssue{},
		}
		if out.Overrides == nil {
			out.Overrides = []config.Override{}
		}
		for _, is := range rep.Issues {
			out.Issues = append(out.Issues, jsonIssue{
				Severity: is.Severity.String(), Source: is.Source, Line: is.Pos.Line, Column: is.Pos.Col,
				Path: is.Path, Message: is.Msg,
			})
		}
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(out)
	} else {
		for _, is := range rep.Issues {
			if sf.quiet && is.Severity != config.SevError {
				continue
			}
			fmt.Fprintln(env.Stdout, is.String())
		}
		if !sf.quiet {
			src := rep.ConfigJSON
			if !rep.ConfigJSONFound {
				src += " (not found: defaults)"
			}
			if rep.OperatorFile != "" {
				src += ", " + rep.OperatorFile
			}
			if n := len(rep.Overrides); n > 0 {
				src += fmt.Sprintf(", %d override(s)", n)
			}
			verdict := "configuration is valid"
			if rep.Errors() > 0 {
				verdict = "configuration has errors"
			}
			fmt.Fprintf(env.Stdout, "%s: %d error(s), %d warning(s) [%s]\n", verdict, rep.Errors(), rep.Warnings(), src)
		}
	}
	if rep.Errors() > 0 {
		return ExitError
	}
	return ExitOK
}

// --- print -------------------------------------------------------------------

func runPrint(args []string, env Env) int {
	var sf sourceFlags
	fs := newFlagSet("print", env, &sf)
	fs.BoolVar(&sf.sources, "sources", false, "annotate overridden settings with their origin (JSON with comments)")
	if code, ok := parseArgs(fs, args); !ok {
		return code
	}
	opts := sf.loadOptions(env)
	opts.ReadOnly = true
	cfg, layers, _, err := config.LoadLayered(opts)
	if err != nil {
		fmt.Fprintln(env.Stderr, "airview config print:", err)
	}
	red := config.Redact(cfg)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if e := enc.Encode(red); e != nil {
		fmt.Fprintln(env.Stderr, "airview config print:", e)
		return ExitError
	}
	if !sf.sources || sf.jsonOut {
		_, _ = env.Stdout.Write(b.Bytes())
	} else {
		fmt.Fprint(env.Stdout, annotate(red, layers.Overrides()))
	}
	if err != nil {
		return ExitError
	}
	return ExitOK
}

// annotate renders cfg as indented JSON with a "// from ..." comment above
// every overridden setting.
func annotate(cfg model.Config, ovs []config.Override) string {
	origins := map[string][]string{}
	for _, o := range ovs {
		origins[o.Path] = append(origins[o.Path], o.Origin)
	}
	var b strings.Builder
	b.WriteString("// Effective AirView configuration (secrets redacted).\n")
	if len(ovs) == 0 {
		b.WriteString("// No setting is overridden: everything comes from config.json or the defaults.\n")
	}
	var doc any
	raw, _ := json.Marshal(cfg)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&doc)
	writeAnnotated(&b, doc, "", 0, origins)
	b.WriteByte('\n')
	return b.String()
}

func writeAnnotated(b *strings.Builder, v any, path string, indent int, origins map[string][]string) {
	pad := strings.Repeat("  ", indent)
	switch x := v.(type) {
	case map[string]any:
		// A map loses the field order; restore the order of model.Config.
		keys := orderedKeys(x, path)
		b.WriteString("{\n")
		for i, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			for _, o := range origins[p] {
				b.WriteString(pad + "  // from " + o + "\n")
			}
			kb, _ := json.Marshal(k)
			b.WriteString(pad + "  " + string(kb) + ": ")
			writeAnnotated(b, x[k], p, indent+1, origins)
			if i < len(keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(pad + "}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range x {
			p := fmt.Sprintf("%s[%d]", path, i)
			for _, o := range origins[p] {
				b.WriteString(pad + "  // from " + o + "\n")
			}
			b.WriteString(pad + "  ")
			writeAnnotated(b, e, p, indent+1, origins)
			if i < len(x)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(pad + "]")
	default:
		var sb strings.Builder
		enc := json.NewEncoder(&sb)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(x)
		b.WriteString(strings.TrimSpace(sb.String()))
	}
}

// orderedKeys returns the keys of an object in model field order (unknown
// keys last, sorted).
func orderedKeys(m map[string]any, path string) []string {
	t := reflect.TypeFor[model.Config]()
	if path != "" {
		if f, ok := config.FieldByPath(path); ok && f.Kind == config.KindStructList {
			t = f.Type.Elem()
		} else if st, ok := structTypeAt(path); ok {
			t = st
		} else {
			t = nil
		}
	}
	var keys []string
	seen := map[string]bool{}
	if t != nil {
		for i := 0; i < t.NumField(); i++ {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			if _, ok := m[name]; ok && !seen[name] {
				keys = append(keys, name)
				seen[name] = true
			}
		}
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// structTypeAt returns the struct type of a section path ("aggregator.mlat",
// "uplinks[0]").
func structTypeAt(path string) (reflect.Type, bool) {
	t := reflect.TypeFor[model.Config]()
	for _, part := range strings.Split(path, ".") {
		name, _, hasIdx := strings.Cut(part, "[")
		if t.Kind() != reflect.Struct {
			return nil, false
		}
		found := false
		for i := 0; i < t.NumField(); i++ {
			n, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			if n == name {
				t = t.Field(i).Type
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
		if hasIdx {
			if t.Kind() != reflect.Slice {
				return nil, false
			}
			t = t.Elem()
		}
	}
	return t, t.Kind() == reflect.Struct
}

// --- example and env -----------------------------------------------------------

func runExample(args []string, env Env) int {
	fs := flag.NewFlagSet("airview config example", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	out := fs.String("o", "", "write to `file` instead of standard output (refuses to overwrite)")
	if code, ok := parseArgs(fs, args); !ok {
		return code
	}
	text := config.ExampleJSONC()
	if *out == "" {
		fmt.Fprint(env.Stdout, text)
		return ExitOK
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintln(env.Stderr, "airview config example:", err)
		return ExitError
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		fmt.Fprintln(env.Stderr, "airview config example:", err)
		return ExitError
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(env.Stderr, "airview config example:", err)
		return ExitError
	}
	fmt.Fprintln(env.Stdout, "wrote", *out)
	return ExitOK
}

func runEnv(args []string, env Env) int {
	fs := flag.NewFlagSet("airview config env", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	if code, ok := parseArgs(fs, args); !ok {
		return code
	}
	type row struct {
		Env    string `json:"env"`
		Path   string `json:"path"`
		Type   string `json:"type"`
		Secret bool   `json:"secret,omitempty"`
	}
	var rows []row
	for _, f := range config.Fields() {
		rows = append(rows, row{Env: f.Env, Path: f.Path, Type: f.Kind.String(), Secret: f.Secret})
	}
	if *jsonOut {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rows)
		return ExitOK
	}
	w := 0
	for _, r := range rows {
		w = max(w, len(r.Env))
	}
	fmt.Fprintf(env.Stdout, "%-*s  %s\n", w, "VARIABLE", "SETTING (TYPE)")
	for _, r := range rows {
		s := ""
		if r.Secret {
			s = ", secret"
		}
		fmt.Fprintf(env.Stdout, "%-*s  %s (%s%s)\n", w, r.Env, r.Path, r.Type, s)
	}
	fmt.Fprintf(env.Stdout, "\n<n> is a list index from 0. Append _FILE to any variable to read its value from a file.\n%s names the operator config file, %s the data directory.\n", config.EnvConfigFile, config.EnvDataDir)
	return ExitOK
}
