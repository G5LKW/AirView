package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"airview/internal/model"
)

// Configuration layers (docs/CONFIG.md, DESIGN §20.3). The effective
// configuration is built from, in increasing priority:
//
//  1. the factory defaults (model.DefaultConfig);
//  2. config.json in the data directory: written by AirView (the web UI's
//     settings page saves here);
//  3. the operator file, a JSON-with-comments file that AirView only reads
//     (-config <file>, else $AIRVIEW_CONFIG, else <data>/airview.jsonc when
//     present). Every setting it lists overrides config.json;
//  4. AIRVIEW_* environment variables (one per setting, see Fields);
//  5. "-set path=value" command-line overrides.
//
// Settings coming from layers 3-5 are "locked": they are in force, the UI
// shows them, but saving from the UI never writes them into config.json
// (see Layers.Persistable), so a secret given by environment variable never
// lands on disk and removing the variable restores the config.json value.

// OperatorFileName is the default name of the operator config file in the
// data directory.
const OperatorFileName = "airview.jsonc"

// maxOperatorFileSize bounds the operator file and *_FILE secrets.
const maxOperatorFileSize = 4 << 20

// LoadOptions selects the configuration sources for LoadLayered and Check.
type LoadOptions struct {
	// Dir is the data directory holding config.json.
	Dir string
	// File is the operator config file. "" = $AIRVIEW_CONFIG (from Environ),
	// else <Dir>/airview.jsonc if it exists. An explicitly named file must
	// exist.
	File string
	// NoFile disables the operator file even when <Dir>/airview.jsonc exists.
	NoFile bool
	// Environ is the process environment ("KEY=value" strings, as from
	// os.Environ). nil means no environment overrides.
	Environ []string
	// Sets are "path=value" overrides from the command line (-set).
	Sets []string
	// ReadOnly never creates, rewrites or moves config.json (used by
	// "airview config check/print"); a missing or damaged config.json is
	// then simply treated as the defaults.
	ReadOnly bool
}

// Override is one setting locked by the operator file, the environment or
// the command line.
type Override struct {
	Path   string // concrete path, e.g. "http.port" or "uplinks[0].enrolToken"
	Origin string // e.g. "/etc/airview/airview.jsonc:14:5", "env AIRVIEW_HTTP_PORT", "-set http.port"
}

type override struct {
	segs   []seg
	value  any // generic JSON value (map[string]any, []any, json.Number, string, bool, nil)
	origin string
	pos    Pos // in the operator file
	source string
}

// Layers holds the override layers above config.json. A nil *Layers is
// valid and means "no overrides". It is safe for concurrent use.
type Layers struct {
	mu     sync.Mutex
	file   string // operator file in use ("" = none)
	fileAt *Node  // its syntax tree
	ops    []override
	issues []Issue
	base   model.Config // config.json as last loaded or saved (validated)
}

// File returns the operator config file in use ("" when there is none).
func (l *Layers) File() string {
	if l == nil {
		return ""
	}
	return l.file
}

// Issues returns the problems found in the override sources (operator file
// syntax and types, unusable environment variables and -set values).
func (l *Layers) Issues() []Issue {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.issues)
}

// Overrides lists the locked settings in the order they are applied.
func (l *Layers) Overrides() []Override {
	if l == nil {
		return nil
	}
	out := make([]Override, len(l.ops))
	for i, o := range l.ops {
		out[i] = Override{Path: segsString(o.segs), Origin: o.origin}
	}
	return out
}

// Locked reports whether a setting (concrete path such as "http.port" or
// "uplinks[0].host") is overridden by a layer above config.json, either
// directly or through an enclosing list.
func (l *Layers) Locked(path string) bool {
	if l == nil {
		return false
	}
	segs, ok := parsePath(path)
	if !ok {
		return false
	}
	for _, o := range l.ops {
		if hasPrefixSegs(segs, o.segs) {
			return true
		}
	}
	return false
}

func hasPrefixSegs(s, prefix []seg) bool {
	return len(prefix) <= len(s) && slices.Equal(s[:len(prefix)], prefix)
}

// Base returns config.json as last loaded or saved through these layers.
func (l *Layers) Base() model.Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Clone(l.base)
}

// LoadLayered loads config.json (as Load does, unless opts.ReadOnly) and
// applies the operator file, environment and -set layers on top. It returns
// the validated effective configuration, the layers (for Commit, Locked,
// Overrides) and warnings: config.json notes, issues of the override
// sources and the corrections Validate made to the merged result.
//
// The returned configuration is always usable. err is non-nil when
// config.json could not be read or written, or when the operator file is
// missing (only if named explicitly) or not valid JSONC; in the latter case
// the operator file is ignored and only the other layers are applied.
func LoadLayered(opts LoadOptions) (model.Config, *Layers, []string, error) {
	var (
		base     model.Config
		warnings []string
		err      error
	)
	if opts.ReadOnly {
		base, warnings, _, _ = readBase(opts.Dir)
	} else {
		base, warnings, err = LoadWithWarnings(opts.Dir)
	}
	l := &Layers{base: Clone(base)}
	ferr := l.loadOperatorFile(opts)
	l.loadEnv(opts.Environ)
	l.loadSets(opts.Sets)
	for _, is := range l.issues {
		warnings = append(warnings, is.String())
	}
	eff, notes := l.Apply(base)
	warnings = append(warnings, notes...)
	return eff, l, warnings, errors.Join(err, ferr)
}

// readBase reads config.json without changing anything on disk. It returns
// the validated base config, the parse/validate warnings, the syntax tree
// (nil when the file is missing or unparsable) and the problems found.
func readBase(dir string) (model.Config, []string, *Node, []Issue) {
	path := filepath.Join(dir, FileName)
	def, _ := Validate(model.DefaultConfig())
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return def, nil, nil, nil
	}
	if err != nil {
		is := Issue{Severity: SevError, Source: path, Msg: "cannot be read: " + err.Error()}
		return def, []string{is.String()}, nil, []Issue{is}
	}
	root, perr := ParseJSONC(data)
	if perr != nil {
		is := Issue{Severity: SevError, Source: path, Msg: perr.Error() + " (AirView would move this file aside and start with the defaults)"}
		var se *SyntaxError
		if errors.As(perr, &se) {
			is.Pos, is.Msg = se.Pos, se.Msg+" (AirView would move this file aside and start with the defaults)"
		}
		return def, []string{is.String()}, nil, []Issue{is}
	}
	cfg, warnings, _, perr := parse(data)
	if perr != nil {
		is := Issue{Severity: SevError, Source: path, Msg: perr.Error() + " (AirView would move this file aside and start with the defaults)"}
		return def, []string{is.String()}, root, []Issue{is}
	}
	cfg, notes := Validate(cfg)
	return cfg, append(warnings, notes...), root, nil
}

// operatorFile decides which operator file to use and whether it was named
// explicitly.
func operatorFile(opts LoadOptions) (string, bool) {
	if opts.NoFile {
		return "", false
	}
	if opts.File != "" {
		return opts.File, true
	}
	if v, ok := lookupEnv(opts.Environ, EnvConfigFile); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), true
	}
	if opts.Dir != "" {
		p := filepath.Join(opts.Dir, OperatorFileName)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, false
		}
	}
	return "", false
}

func lookupEnv(environ []string, key string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- { // last one wins, like the OS
		if k, v, ok := strings.Cut(environ[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func (l *Layers) loadOperatorFile(opts LoadOptions) error {
	path, explicit := operatorFile(opts)
	if path == "" {
		return nil
	}
	data, err := readLimited(path)
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		l.issues = append(l.issues, Issue{Severity: SevError, Source: path, Msg: "cannot be read: " + err.Error()})
		return fmt.Errorf("config: operator file %s: %w", path, err)
	}
	root, err := ParseJSONC(data)
	if err != nil {
		is := Issue{Severity: SevError, Source: path, Msg: err.Error()}
		var se *SyntaxError
		if errors.As(err, &se) {
			is.Pos, is.Msg = se.Pos, se.Msg
		}
		l.issues = append(l.issues, is)
		return fmt.Errorf("config: operator file %s: %w (the file is ignored)", path, err)
	}
	l.file, l.fileAt = path, root
	c := &schemaChecker{source: path, unknownSev: SevError}
	if root.Kind != ObjectNode {
		c.add(SevError, root.Pos, nil, "the file must contain one object {...}, found %s", root.Kind)
	} else {
		l.fileOps(c, reflect.TypeFor[model.Config](), root, nil, path)
	}
	l.issues = append(l.issues, c.issues...)
	return nil
}

// fileOps turns an operator-file object into overrides: objects are entered,
// every other value (scalars and whole lists) becomes one override.
// Unknown or ill-typed members are reported and skipped.
func (l *Layers) fileOps(c *schemaChecker, t reflect.Type, n *Node, segs []seg, path string) {
	seen := map[string]Pos{}
	for _, m := range n.Members {
		msegs := append(segs[:len(segs):len(segs)], seg{key: m.Key, idx: -1})
		if prev, dup := seen[m.Key]; dup {
			c.add(SevError, m.KeyPos, msegs, "duplicate setting (first given at line %d); only the last one is used", prev.Line)
		}
		seen[m.Key] = m.KeyPos
		if len(segs) == 0 && m.Key == "version" {
			continue // informational in the operator file
		}
		sf, ok := fieldByJSON(t, m.Key)
		if ok && sf.Type.Kind() == reflect.Struct && m.Value.Kind == ObjectNode {
			l.fileOps(c, sf.Type, m.Value, msegs, path)
			continue
		}
		if !c.member(t, m, msegs, false) {
			continue
		}
		v := m.Value.Value()
		if v == nil && sf.Type.Kind() == reflect.Slice {
			v = []any{} // "list": null means an empty list
		}
		l.addOp(override{
			segs: msegs, value: v, pos: m.KeyPos, source: path,
			origin: fmt.Sprintf("%s:%d:%d", path, m.KeyPos.Line, m.KeyPos.Col),
		})
	}
}

// addOp records an override; a plain admin password is hashed once here so
// the effective hash is stable for the life of the layers.
func (l *Layers) addOp(o override) {
	if segsString(o.segs) == "http.adminPasswordHash" {
		if s, ok := o.value.(string); ok && s != "" && !validHash(s) {
			o.value = HashPassword(strings.TrimPrefix(s, PlainPrefix))
		}
	}
	l.ops = append(l.ops, o)
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b bytes.Buffer
	n, err := b.ReadFrom(io.LimitReader(f, maxOperatorFileSize+1))
	if err != nil {
		return nil, err
	}
	if n > maxOperatorFileSize {
		return nil, fmt.Errorf("file is larger than %d bytes", maxOperatorFileSize)
	}
	return b.Bytes(), nil
}

// loadEnv turns AIRVIEW_* variables into overrides.
func (l *Layers) loadEnv(environ []string) {
	fieldsOnce.Do(buildFields)
	vals := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, EnvPrefix) {
			continue
		}
		vals[k] = v // last one wins
	}
	var ops []override
	for name, raw := range vals {
		if knownNonSettingEnv[name] || strings.HasPrefix(name, "AIRVIEW_LIVE_") {
			continue
		}
		src := "env " + name
		segs, typ, ok := resolveEnv(name)
		if !ok {
			base, isFile := strings.CutSuffix(name, "_FILE")
			if isFile {
				segs, typ, ok = resolveEnv(base)
			}
			if !ok {
				l.issues = append(l.issues, Issue{Severity: SevWarning, Source: src, Msg: "not a known AirView setting; ignored (see airview config env)"})
				continue
			}
			data, err := readLimited(strings.TrimSpace(raw))
			if err != nil {
				l.issues = append(l.issues, Issue{Severity: SevError, Source: src, Path: segsString(segs), Msg: "cannot read the value file: " + err.Error()})
				continue
			}
			raw = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		}
		v, err := parseTextValue(typ, raw, segs)
		if err != nil {
			l.issues = append(l.issues, Issue{Severity: SevError, Source: src, Path: segsString(segs), Msg: err.Error()})
			continue
		}
		ops = append(ops, override{segs: segs, value: v, origin: src, source: src})
	}
	sortOps(ops)
	for _, o := range ops {
		l.addOp(o)
	}
}

// loadSets turns "-set path=value" arguments into overrides.
func (l *Layers) loadSets(sets []string) {
	for _, s := range sets {
		path, raw, ok := strings.Cut(s, "=")
		path = strings.TrimSpace(path)
		src := "-set " + path
		if !ok {
			l.issues = append(l.issues, Issue{Severity: SevError, Source: "-set " + s, Msg: "expected path=value"})
			continue
		}
		segs, ok := parsePath(path)
		var typ reflect.Type
		if ok {
			typ, ok = typeAt(segs)
		}
		if !ok || (len(segs) == 1 && segs[0].key == "version") {
			l.issues = append(l.issues, Issue{Severity: SevError, Source: src, Msg: "unknown setting (see airview config env for the names)"})
			continue
		}
		if typ.Kind() == reflect.Struct && !segs[len(segs)-1].isIndex() {
			l.issues = append(l.issues, Issue{Severity: SevError, Source: src, Msg: "is a section; set its individual settings"})
			continue
		}
		v, err := parseTextValue(typ, raw, segs)
		if err != nil {
			l.issues = append(l.issues, Issue{Severity: SevError, Source: src, Path: path, Msg: err.Error()})
			continue
		}
		l.addOp(override{segs: segs, value: v, origin: src, source: src})
	}
}

// sortOps orders overrides so that a list is replaced before its elements
// are edited and element indices ascend numerically.
func sortOps(ops []override) {
	sort.SliceStable(ops, func(i, j int) bool {
		a, b := ops[i].segs, ops[j].segs
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] == b[k] {
				continue
			}
			if a[k].isIndex() && b[k].isIndex() {
				return a[k].idx < b[k].idx
			}
			return a[k].key < b[k].key
		}
		return len(a) < len(b)
	})
}

// parseTextValue converts the text of an environment variable or -set value
// to a generic JSON value of type t. Lists take JSON ("[1,2]") or comma
// separated values ("1,2"); lists of objects and single list elements take
// JSON (comments allowed).
func parseTextValue(t reflect.Type, raw string, segs []seg) (any, error) {
	s := strings.TrimSpace(raw)
	switch t.Kind() {
	case reflect.String:
		return raw, nil
	case reflect.Bool:
		switch strings.ToLower(s) {
		case "1", "true", "yes", "on":
			return true, nil
		case "0", "false", "no", "off":
			return false, nil
		}
		return nil, fmt.Errorf("%q is not a boolean (use true or false)", clip(raw))
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%q is not a number", clip(raw))
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", clip(raw))
		}
		return json.Number(strconv.FormatInt(n, 10)), nil
	case reflect.Slice:
		if t.Elem().Kind() != reflect.Struct && !strings.HasPrefix(s, "[") {
			out := []any{}
			if s == "" {
				return out, nil
			}
			for i, part := range strings.Split(s, ",") {
				v, err := parseTextValue(t.Elem(), strings.TrimSpace(part), segs)
				if err != nil {
					return nil, fmt.Errorf("item %d: %v", i+1, err)
				}
				out = append(out, v)
			}
			return out, nil
		}
	}
	// JSON(C) value: lists of objects, a single list element, or a JSON list.
	root, err := ParseJSONC([]byte(s))
	if err != nil {
		return nil, fmt.Errorf("not valid JSON: %v", err)
	}
	c := &schemaChecker{unknownSev: SevError}
	ok := c.check(t, root, segs)
	for _, is := range c.issues {
		if is.Severity == SevError {
			if is.Path != segsString(segs) {
				return nil, fmt.Errorf("%s: %s (at %s of the value)", is.Path, is.Msg, is.Pos)
			}
			return nil, fmt.Errorf("%s (at %s of the value)", is.Msg, is.Pos)
		}
	}
	if !ok {
		return nil, errors.New("value is not usable")
	}
	v := root.Value()
	if v == nil && t.Kind() == reflect.Slice {
		v = []any{}
	}
	return v, nil
}

// Apply returns base with every override applied, validated, plus the notes
// of Validate. With no overrides it is Validate(base).
func (l *Layers) Apply(base model.Config) (model.Config, []string) {
	if l == nil || len(l.ops) == 0 {
		return Validate(base)
	}
	doc, err := toDoc(base)
	if err != nil {
		return Validate(base)
	}
	var notes []string
	for _, o := range l.ops {
		nd, err := setIn(doc, o.segs, o.value)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: %s: %v; ignored", o.origin, segsString(o.segs), err))
			continue
		}
		doc = nd.(map[string]any)
	}
	cfg, err := fromDoc(doc)
	if err != nil {
		notes = append(notes, "applying overrides failed: "+err.Error())
		return Validate(base)
	}
	cfg, vnotes := Validate(cfg)
	return cfg, append(notes, vnotes...)
}

// Persistable returns cfg with every overridden setting put back to its
// config.json value: what may be written to config.json. Overrides that
// added list elements remove those elements again.
func (l *Layers) Persistable(cfg model.Config) model.Config {
	if l == nil || len(l.ops) == 0 {
		return cfg
	}
	l.mu.Lock()
	base := Clone(l.base)
	l.mu.Unlock()
	doc, err1 := toDoc(cfg)
	bdoc, err2 := toDoc(base)
	if err1 != nil || err2 != nil {
		return cfg
	}
	for _, o := range l.ops {
		doc = restoreIn(doc, bdoc, o.segs).(map[string]any)
	}
	out, err := fromDoc(doc)
	if err != nil {
		return base
	}
	out, _ = Validate(out)
	return out
}

// Commit saves cfg (typically the result of ApplyUpdate) to config.json in
// dir without the overridden settings, remembers it as the new base and
// returns the new effective configuration (overrides re-applied, so locked
// settings keep their override value). With nil layers it is Save + return.
func (l *Layers) Commit(dir string, cfg model.Config) (model.Config, error) {
	if l == nil {
		cfg, _ = Validate(cfg)
		return cfg, Save(dir, cfg)
	}
	base, _ := Validate(l.Persistable(cfg))
	if err := Save(dir, base); err != nil {
		return cfg, err
	}
	l.mu.Lock()
	l.base = Clone(base)
	l.mu.Unlock()
	eff, _ := l.Apply(base)
	return eff, nil
}

// FactoryReset resets config.json to the defaults (see the package function)
// and returns the effective configuration with the overrides re-applied.
func (l *Layers) FactoryReset(dir string) (model.Config, error) {
	cfg, err := FactoryReset(dir)
	if l == nil {
		return cfg, err
	}
	l.mu.Lock()
	l.base = Clone(cfg)
	l.mu.Unlock()
	eff, _ := l.Apply(cfg)
	return eff, err
}

// toDoc converts a config into the generic JSON representation.
func toDoc(cfg model.Config) (map[string]any, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	return m, dec.Decode(&m)
}

// fromDoc decodes a generic document like config.json is decoded (on top of
// the defaults, lists replaced as a whole).
func fromDoc(doc map[string]any) (model.Config, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return model.Config{}, err
	}
	cfg, _, _, err := parse(b)
	return cfg, err
}

// setIn sets v at segs below c and returns the updated container. An index
// equal to the list length appends a new element; larger indices fail.
func setIn(c any, segs []seg, v any) (any, error) {
	if len(segs) == 0 {
		return v, nil
	}
	s := segs[0]
	if s.isIndex() {
		arr, _ := c.([]any)
		switch {
		case s.idx > len(arr):
			return c, fmt.Errorf("index %d skips elements (the list has %d)", s.idx, len(arr))
		case s.idx == len(arr):
			arr = append(arr, map[string]any{})
		}
		nv, err := setIn(arr[s.idx], segs[1:], v)
		if err != nil {
			return c, err
		}
		arr[s.idx] = nv
		return arr, nil
	}
	m, ok := c.(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	nv, err := setIn(m[s.key], segs[1:], v)
	if err != nil {
		return c, err
	}
	m[s.key] = nv
	return m, nil
}

// restoreIn puts the base value at segs back into c (removing what the base
// does not have) and returns the updated container.
func restoreIn(c, b any, segs []seg) any {
	if len(segs) == 0 {
		return b
	}
	s := segs[0]
	if s.isIndex() {
		arr, _ := c.([]any)
		barr, _ := b.([]any)
		if s.idx >= len(barr) {
			if len(arr) > len(barr) {
				arr = arr[:len(barr)]
			}
			return arr
		}
		if s.idx >= len(arr) {
			return arr
		}
		arr[s.idx] = restoreIn(arr[s.idx], barr[s.idx], segs[1:])
		return arr
	}
	m, ok := c.(map[string]any)
	if !ok {
		return c
	}
	bm, _ := b.(map[string]any)
	bv, has := bm[s.key]
	if !has {
		delete(m, s.key)
		return m
	}
	m[s.key] = restoreIn(m[s.key], bv, segs[1:])
	return m
}
