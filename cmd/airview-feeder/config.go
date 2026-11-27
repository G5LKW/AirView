package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"airview/internal/config"
	"airview/internal/model"
)

// Configuration of airview-feeder (DESIGN §12.1, §20.3).
//
// The effective configuration is built from, in increasing priority:
//
//  1. the defaults (model.DefaultFeederConfig);
//  2. the configuration file feeder.json (JSON; comments and trailing commas
//     are accepted when reading but dropped when the feeder rewrites the
//     file from "setup", "config set" or the status page);
//  3. AIRVIEW_FEEDER_* environment variables, one per setting, list
//     elements by index (AIRVIEW_FEEDER_UPLINKS_0_HOST); a variable named
//     <NAME>_FILE reads the value from a file (for secrets);
//  4. "-set path=value" command-line options (run).
//
// Layers 3 and 4 are "locked": they are in force, but saving from the
// status page or "config set" writes only the file layer, so a secret given
// by environment variable never lands on disk.

// Environment variables that are not settings.
const (
	envPrefix     = "AIRVIEW_FEEDER_"
	envConfigFile = "AIRVIEW_FEEDER_CONFIG" // like -config
	envDataDir    = "AIRVIEW_FEEDER_DATA"   // like -data
	// AIRVIEW_CONFIG / AIRVIEW_DATA (the main app's names) are honoured when
	// the AIRVIEW_FEEDER_* forms are not set.
	envConfigFileMain = "AIRVIEW_CONFIG"
	envDataDirMain    = "AIRVIEW_DATA"
)

var nonSettingEnv = map[string]bool{envConfigFile: true, envDataDir: true}

// maxConfigSize bounds feeder.json and *_FILE values.
const maxConfigSize = 1 << 20

// issue is one problem found while loading.
type issue struct {
	Error  bool   // false = warning
	Source string // file:line:col, "env NAME" or "-set path"
	Path   string // setting path, if known
	Msg    string
}

func (i issue) String() string {
	sev := "warning"
	if i.Error {
		sev = "error"
	}
	var b strings.Builder
	b.WriteString(sev)
	if i.Source != "" {
		b.WriteString(": " + i.Source)
	}
	if i.Path != "" {
		b.WriteString(": " + i.Path)
	}
	b.WriteString(": " + i.Msg)
	return b.String()
}

// loadedConfig is the result of loading the layers.
type loadedConfig struct {
	File    string             // configuration file path
	Exists  bool               // the file exists
	Comment bool               // the file has comments or trailing commas (lost on rewrite)
	FileCfg model.FeederConfig // file layer over defaults, normalised
	Eff     model.FeederConfig // effective configuration, normalised
	Notes   []string           // normalisation notes of the effective configuration
	Issues  []issue            // syntax, schema, environment and -set problems
	Locked  []string           // paths set by environment or -set
	ops     []override
}

type override struct {
	path   []pseg
	value  any
	origin string
}

// loadOptions selects the sources.
type loadOptions struct {
	File    string   // configuration file (must be resolved by the caller)
	Environ []string // nil = no environment overrides
	Sets    []string // "path=value"
}

// errorCount returns the number of error issues.
func (l *loadedConfig) errorCount() int {
	n := 0
	for _, i := range l.Issues {
		if i.Error {
			n++
		}
	}
	return n
}

// loadConfig reads and layers the configuration. A missing file is not an
// error (defaults); an unreadable or syntactically broken file is, but a
// usable configuration (defaults + overrides) is still returned.
func loadConfig(o loadOptions) (*loadedConfig, error) {
	l := &loadedConfig{File: o.File}
	var fileErr error
	base := model.DefaultFeederConfig()
	data, err := readLimited(o.File)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		fileErr = err
		l.Issues = append(l.Issues, issue{Error: true, Source: o.File, Msg: err.Error()})
	default:
		l.Exists = true
		cfg, issues, comment, err := parseFeederConfig(o.File, data)
		l.Issues = append(l.Issues, issues...)
		l.Comment = comment
		if err != nil {
			fileErr = err
		} else {
			base = cfg
		}
	}
	l.FileCfg = base
	normalizeFeeder(&l.FileCfg)

	l.loadEnv(o.Environ)
	l.loadSets(o.Sets)
	l.Eff, l.Notes = l.apply(base)
	return l, fileErr
}

// apply returns base with every override applied, normalised, plus notes.
func (l *loadedConfig) apply(base model.FeederConfig) (model.FeederConfig, []string) {
	out := base
	if len(l.ops) > 0 {
		doc, err := toDoc(base)
		if err == nil {
			for _, op := range l.ops {
				doc, err = setPath(doc, op.path, op.value)
				if err != nil {
					l.Issues = append(l.Issues, issue{Error: true, Source: op.origin, Path: pathString(op.path), Msg: err.Error()})
				}
			}
			b, _ := json.Marshal(doc)
			if cfg, err := decodeFeederConfig(b); err == nil {
				out = cfg
			} else {
				l.Issues = append(l.Issues, issue{Error: true, Source: "overrides", Msg: err.Error()})
			}
		}
	}
	notes := normalizeFeeder(&out)
	return out, notes
}

// reapply recomputes the effective configuration from a new file layer
// (after the status page saved settings).
func (l *loadedConfig) reapply(fileCfg model.FeederConfig) {
	l.FileCfg = fileCfg
	normalizeFeeder(&l.FileCfg)
	l.Eff, l.Notes = l.apply(l.FileCfg)
}

// parseFeederConfig parses feeder.json (JSONC accepted) and checks it
// against the schema. Unknown keys are warnings, wrong types errors.
func parseFeederConfig(name string, data []byte) (model.FeederConfig, []issue, bool, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	root, err := config.ParseJSONC(data)
	if err != nil {
		var se *config.SyntaxError
		src := name
		if errors.As(err, &se) {
			src = fmt.Sprintf("%s:%d:%d", name, se.Pos.Line, se.Pos.Col)
			err = errors.New(se.Msg)
		}
		return model.FeederConfig{}, []issue{{Error: true, Source: src, Msg: "not valid JSON: " + err.Error()}}, false, fmt.Errorf("%s: %w", src, err)
	}
	var issues []issue
	if root.Kind != config.ObjectNode {
		return model.FeederConfig{}, []issue{{Error: true, Source: name, Msg: "the file must contain one JSON object"}}, false, errors.New(name + ": not a JSON object")
	}
	checkNode(name, reflect.TypeFor[model.FeederConfig](), root, "", &issues)
	plain := root.JSON()
	comment := !bytes.Equal(bytes.TrimSpace(compactJSON(data)), bytes.TrimSpace(compactJSON(plain)))
	cfg, err := decodeFeederConfig(plain)
	if err != nil {
		issues = append(issues, issue{Error: true, Source: name, Msg: err.Error()})
		return model.FeederConfig{}, issues, comment, err
	}
	for _, is := range issues {
		if is.Error {
			return cfg, issues, comment, errors.New(is.String())
		}
	}
	return cfg, issues, comment, nil
}

func compactJSON(b []byte) []byte {
	var out bytes.Buffer
	if json.Compact(&out, b) != nil {
		return nil
	}
	return out.Bytes()
}

// checkNode reports unknown members (warning) and type mismatches (error).
func checkNode(file string, t reflect.Type, n *config.Node, path string, out *[]issue) {
	at := func(p config.Pos) string { return fmt.Sprintf("%s:%d:%d", file, p.Line, p.Col) }
	want := func(k config.NodeKind) {
		if n.Kind != k && n.Kind != config.NullNode {
			*out = append(*out, issue{Error: true, Source: at(n.Pos), Path: path, Msg: fmt.Sprintf("must be %s, not %s", k, n.Kind)})
		}
	}
	switch t.Kind() {
	case reflect.Struct:
		want(config.ObjectNode)
		if n.Kind != config.ObjectNode {
			return
		}
		fields := jsonFields(t)
		for _, m := range n.Members {
			sf, ok := fields[m.Key]
			p := joinPath(path, m.Key)
			if !ok {
				msg := "unknown setting; ignored"
				if s := suggestField(fields, m.Key); s != "" {
					msg += fmt.Sprintf(" (did you mean %q?)", s)
				}
				*out = append(*out, issue{Source: at(m.KeyPos), Path: p, Msg: msg})
				continue
			}
			checkNode(file, sf.Type, m.Value, p, out)
		}
	case reflect.Slice:
		want(config.ArrayNode)
		for i, e := range n.Elems {
			checkNode(file, t.Elem(), e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	case reflect.String:
		want(config.StringNode)
	case reflect.Bool:
		want(config.BoolNode)
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		want(config.NumberNode)
		if n.Kind == config.NumberNode {
			if _, err := strconv.ParseInt(n.Raw, 10, 64); err != nil {
				*out = append(*out, issue{Error: true, Source: at(n.Pos), Path: path, Msg: n.Raw + " is not a whole number"})
			}
		}
	case reflect.Float64, reflect.Float32:
		want(config.NumberNode)
	}
}

func jsonFields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if name, ok := jsonName(sf); ok {
			out[name] = sf
		}
	}
	return out
}

func jsonName(sf reflect.StructField) (string, bool) {
	if !sf.IsExported() {
		return "", false
	}
	tag := sf.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		name = sf.Name
	}
	return name, true
}

func suggestField(fields map[string]reflect.StructField, key string) string {
	best, bestD := "", 3
	for name := range fields {
		if strings.EqualFold(name, key) {
			return name
		}
		if d := editDistance(strings.ToLower(name), strings.ToLower(key)); d < bestD {
			best, bestD = name, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// decodeFeederConfig decodes plain JSON on top of the defaults. Receivers
// are decoded one by one on top of defaultReceiver (encoding/json would
// otherwise start them from zero values: a hand-written receiver would be
// disabled and have no rxNo).
func decodeFeederConfig(b []byte) (model.FeederConfig, error) {
	cfg := model.DefaultFeederConfig()
	var aux struct {
		Receivers *[]json.RawMessage `json:"receivers"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return cfg, err
	}
	keep := cfg.Receivers
	if err := json.Unmarshal(b, &cfg); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return cfg, err
		}
		return cfg, fmt.Errorf("%s: wrong type (%s, want %s)", te.Field, te.Value, te.Type)
	}
	cfg.Receivers = keep
	if aux.Receivers != nil {
		cfg.Receivers = make([]model.FeederReceiver, 0, len(*aux.Receivers))
		for i, raw := range *aux.Receivers {
			r := defaultReceiver(i + 1)
			if err := json.Unmarshal(raw, &r); err != nil {
				return cfg, fmt.Errorf("receivers[%d]: %v", i, err)
			}
			cfg.Receivers = append(cfg.Receivers, r)
		}
	}
	return cfg, nil
}

// defaultReceiver is the starting point of a configured receiver.
func defaultReceiver(rxNo int) model.FeederReceiver {
	return model.FeederReceiver{
		RxNo: rxNo, Name: "receiver",
		Input:  model.Feed{Enabled: true, Type: model.FeedAuto},
		AltRef: model.AltRefMSL,
	}
}

// Default ports of the input types (used when a host is given without a
// port).
var defaultInputPort = map[string]int{
	model.FeedBeast: 30005,
	model.FeedAVR:   30002,
	model.FeedSBS:   30003,
	// RTL-SDR dongle through the user's own rtl_tcp.
	model.FeedRTLTCP: model.DefaultRTLTCPPort,
}

// normalizeFeeder runs model's Normalize plus the feeder-specific input
// checks and returns all notes.
func normalizeFeeder(c *model.FeederConfig) []string {
	notes := c.Normalize()
	seen := map[string]bool{}
	for i := range c.Receivers {
		r := &c.Receivers[i]
		p := fmt.Sprintf("receivers[%d]", i)
		in := &r.Input
		switch in.Type {
		case model.FeedAuto, model.FeedBeast, model.FeedAVR, model.FeedSBS, model.FeedJSON, model.FeedRTLTCP:
		case "":
			in.Type = model.FeedAuto
		default:
			notes = append(notes, fmt.Sprintf("%s.input.type %q is not one of auto/beast/avr/sbs/json/rtltcp; auto is used", p, in.Type))
			in.Type = model.FeedAuto
		}
		if in.Type == model.FeedRTLTCP {
			notes = append(notes, in.SDR.Normalize(p+".input.sdr")...)
		} else {
			in.SDR = model.FeedSDR{}
		}
		if in.Type == model.FeedJSON && in.URL == "" {
			notes = append(notes, fmt.Sprintf("%s.input: type json needs a url; auto is used", p))
			in.Type = model.FeedAuto
		}
		if in.Port < 0 || in.Port > 65535 {
			notes = append(notes, fmt.Sprintf("%s.input.port %d is out of range; the default is used", p, in.Port))
			in.Port = 0
		}
		if in.Type != model.FeedAuto && in.Type != model.FeedJSON {
			if in.Host == "" {
				in.Host = "127.0.0.1"
			}
			if in.Port == 0 {
				in.Port = defaultInputPort[in.Type]
			}
		}
		id := strings.ToLower(in.ID)
		if !model.ValidSlug(id) || seen[id] {
			id = fmt.Sprintf("rx%d", r.RxNo)
			for k := 2; seen[id]; k++ {
				id = fmt.Sprintf("rx%d-%d", r.RxNo, k)
			}
			if in.ID != "" {
				notes = append(notes, fmt.Sprintf("%s.input.id %q is invalid or duplicate; %q is used", p, in.ID, id))
			}
		}
		in.ID = id
		seen[id] = true
		if in.Name == "" {
			in.Name = r.Name
		}
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" {
			r.Name = "receiver"
		}
		if len(r.Name) > 32 {
			notes = append(notes, fmt.Sprintf("%s.name is longer than 32 characters; it was shortened", p))
			r.Name = r.Name[:32]
		}
		// The uplink applies Feed.ClockClass; keep both in step.
		in.ClockClass = r.ClockClass
		in.MLATOnly, in.NoForward = false, false
	}
	if len(c.Receivers) == 0 {
		notes = append(notes, "no receiver is configured; nothing will be fed")
	}
	if c.Results.ReadsbAddr != "" {
		if _, _, err := splitHostPortDefault(c.Results.ReadsbAddr, 30104); err != nil {
			notes = append(notes, fmt.Sprintf("results.readsbAddr %q is not host:port; it is ignored", c.Results.ReadsbAddr))
			c.Results.ReadsbAddr = ""
		}
	}
	return notes
}

// readLimited reads a file of at most maxConfigSize bytes.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxConfigSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxConfigSize)
	}
	return b, nil
}

// saveConfig writes the file layer atomically (0640: it may hold the enrol
// token and the page password hash).
func saveConfig(path string, cfg model.FeederConfig) error {
	cfg.Version, cfg.Mode = model.ConfigVersion, model.ModeFeeder
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".feeder-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0o640)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm() // keep the installer's mode and group
		keepOwner(name, st)     // and its owner (root:airview-feeder on a Pi)
	}
	_ = os.Chmod(name, mode)
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return nil
}

// redactConfig hides secrets (enrol tokens, the page password hash).
func redactConfig(c model.FeederConfig, mark string) model.FeederConfig {
	c.Uplinks = model.CloneUplinks(c.Uplinks)
	c.Receivers = append([]model.FeederReceiver(nil), c.Receivers...)
	for i := range c.Uplinks {
		if c.Uplinks[i].EnrolToken != "" {
			c.Uplinks[i].EnrolToken = mark
		}
	}
	if c.UI.PasswordHash != "" {
		c.UI.PasswordHash = mark
	}
	return c
}

// mergeSecrets replaces redaction marks in in with the values from cur
// (uplinks matched by ID). A plain password in ui.passwordHash (anything not
// in hash form) is hashed.
func mergeSecrets(in, cur model.FeederConfig, mark string) model.FeederConfig {
	for i := range in.Uplinks {
		if in.Uplinks[i].EnrolToken != mark {
			continue
		}
		in.Uplinks[i].EnrolToken = ""
		for _, u := range cur.Uplinks {
			// Only to the same server (host, port, pin): SEC-11.
			if u.ID == in.Uplinks[i].ID && config.SameUplinkTarget(u, in.Uplinks[i]) {
				in.Uplinks[i].EnrolToken = u.EnrolToken
			}
		}
	}
	switch h := in.UI.PasswordHash; {
	case h == mark:
		in.UI.PasswordHash = cur.UI.PasswordHash
	case h != "" && !config.IsPasswordHash(h):
		in.UI.PasswordHash = config.HashPassword(h)
	}
	return in
}

// ---- paths, environment and -set ----------------------------------------

// pseg is one step of a setting path: a member name or a list index.
type pseg struct {
	key string
	idx int // >= 0: list index
}

func (s pseg) isIndex() bool { return s.idx >= 0 }

func pathString(p []pseg) string {
	var b strings.Builder
	for i, s := range p {
		if s.isIndex() {
			fmt.Fprintf(&b, "[%d]", s.idx)
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.key)
	}
	return b.String()
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// parsePath parses "uplinks[0].host" (also "uplinks.0.host").
func parsePath(s string) ([]pseg, bool) {
	var out []pseg
	for _, part := range strings.Split(strings.TrimSpace(s), ".") {
		if part == "" {
			return nil, false
		}
		for part != "" {
			if part[0] == '[' {
				end := strings.IndexByte(part, ']')
				if end < 0 {
					return nil, false
				}
				n, err := strconv.Atoi(part[1:end])
				if err != nil || n < 0 {
					return nil, false
				}
				out = append(out, pseg{idx: n})
				part = part[end+1:]
				continue
			}
			key := part
			if i := strings.IndexByte(part, '['); i >= 0 {
				key, part = part[:i], part[i:]
			} else {
				part = ""
			}
			if n, err := strconv.Atoi(key); err == nil && n >= 0 {
				out = append(out, pseg{idx: n})
			} else {
				out = append(out, pseg{key: key, idx: -1})
			}
		}
	}
	return out, len(out) > 0
}

// typeAt returns the Go type at a path of FeederConfig.
func typeAt(p []pseg) (reflect.Type, bool) {
	t := reflect.TypeFor[model.FeederConfig]()
	for _, s := range p {
		switch {
		case s.isIndex():
			if t.Kind() != reflect.Slice {
				return nil, false
			}
			t = t.Elem()
		case t.Kind() == reflect.Struct:
			sf, ok := jsonFields(t)[s.key]
			if !ok {
				return nil, false
			}
			t = sf.Type
		default:
			return nil, false
		}
	}
	return t, true
}

// envSegment turns a JSON name into its environment form
// ("uplinkTuning" -> "UPLINK_TUNING"; the same rule as airview's own
// AIRVIEW_* variables).
func envSegment(name string) string {
	rs := []rune(name)
	var b strings.Builder
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// envName returns the variable of a concrete path.
func envName(p []pseg) string {
	var b strings.Builder
	b.WriteString(strings.TrimSuffix(envPrefix, "_"))
	for _, s := range p {
		b.WriteByte('_')
		if s.isIndex() {
			b.WriteString(strconv.Itoa(s.idx))
		} else {
			b.WriteString(envSegment(s.key))
		}
	}
	return b.String()
}

// resolveEnv maps a variable name (without the prefix) to a path.
func resolveEnv(rest string) ([]pseg, reflect.Type, bool) {
	t := reflect.TypeFor[model.FeederConfig]()
	var p []pseg
	for rest != "" {
		switch t.Kind() {
		case reflect.Slice:
			i := strings.IndexByte(rest, '_')
			tok := rest
			if i >= 0 {
				tok = rest[:i]
			}
			n, err := strconv.Atoi(tok)
			if err != nil || n < 0 || n > 255 {
				return nil, nil, false
			}
			p = append(p, pseg{idx: n})
			t = t.Elem()
			if i < 0 {
				rest = ""
			} else {
				rest = rest[i+1:]
			}
		case reflect.Struct:
			// longest matching field name first
			var best, bestName string
			var bestT reflect.Type
			for name, sf := range jsonFields(t) {
				seg := envSegment(name)
				if (rest == seg || strings.HasPrefix(rest, seg+"_")) && len(seg) > len(best) {
					best, bestName, bestT = seg, name, sf.Type
				}
			}
			if best == "" {
				return nil, nil, false
			}
			p = append(p, pseg{key: bestName, idx: -1})
			t = bestT
			rest = strings.TrimPrefix(strings.TrimPrefix(rest, best), "_")
		default:
			return nil, nil, false
		}
	}
	if len(p) == 0 || (len(p) == 1 && (p[0].key == "version" || p[0].key == "mode")) {
		return nil, nil, false
	}
	return p, t, true
}

func (l *loadedConfig) loadEnv(environ []string) {
	vals := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, envPrefix) && !nonSettingEnv[k] {
			vals[k] = v
		}
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	var ops []override
	for _, name := range names {
		raw := vals[name]
		src := "env " + name
		p, t, ok := resolveEnv(strings.TrimPrefix(name, envPrefix))
		if !ok {
			base, isFile := strings.CutSuffix(name, "_FILE")
			if isFile {
				p, t, ok = resolveEnv(strings.TrimPrefix(base, envPrefix))
			}
			if !ok {
				l.Issues = append(l.Issues, issue{Source: src, Msg: "not a known airview-feeder setting; ignored (see airview-feeder config env)"})
				continue
			}
			data, err := readLimited(strings.TrimSpace(raw))
			if err != nil {
				l.Issues = append(l.Issues, issue{Error: true, Source: src, Path: pathString(p), Msg: "cannot read the value file: " + err.Error()})
				continue
			}
			raw = strings.TrimRight(string(data), "\r\n")
		}
		v, err := parseTextValue(t, raw)
		if err != nil {
			l.Issues = append(l.Issues, issue{Error: true, Source: src, Path: pathString(p), Msg: err.Error()})
			continue
		}
		ops = append(ops, override{path: p, value: v, origin: src})
	}
	// A list is replaced before its elements are edited; indices ascend
	// numerically (so _0_ is created before _1_).
	sort.SliceStable(ops, func(i, j int) bool {
		a, b := ops[i].path, ops[j].path
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
	for _, op := range ops {
		l.ops = append(l.ops, op)
		l.Locked = append(l.Locked, pathString(op.path))
	}
}

func (l *loadedConfig) loadSets(sets []string) {
	for _, s := range sets {
		path, raw, ok := strings.Cut(s, "=")
		src := "-set " + strings.TrimSpace(path)
		if !ok {
			l.Issues = append(l.Issues, issue{Error: true, Source: "-set " + s, Msg: "expected path=value"})
			continue
		}
		p, t, err := settingPath(path)
		if err != nil {
			l.Issues = append(l.Issues, issue{Error: true, Source: src, Msg: err.Error()})
			continue
		}
		v, err := parseTextValue(t, raw)
		if err != nil {
			l.Issues = append(l.Issues, issue{Error: true, Source: src, Path: pathString(p), Msg: err.Error()})
			continue
		}
		l.ops = append(l.ops, override{path: p, value: v, origin: src})
		l.Locked = append(l.Locked, pathString(p))
	}
}

// settingPath parses and checks a user-given setting path.
func settingPath(path string) ([]pseg, reflect.Type, error) {
	p, ok := parsePath(path)
	if !ok {
		return nil, nil, fmt.Errorf("%q is not a setting path (for example uplinks[0].host)", path)
	}
	t, ok := typeAt(p)
	if !ok || (len(p) == 1 && (p[0].key == "version" || p[0].key == "mode")) {
		return nil, nil, fmt.Errorf("unknown setting %q (see airview-feeder config env)", path)
	}
	return p, t, nil
}

// parseTextValue converts text to a generic JSON value of type t: plain
// text for strings, true/false/yes/no/on/off/1/0 for booleans, numbers,
// comma-separated or JSON lists, JSON for objects and lists of objects.
func parseTextValue(t reflect.Type, raw string) (any, error) {
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
		return nil, fmt.Errorf("%q is not a boolean (use true or false)", s)
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a whole number", s)
		}
		return json.Number(strconv.FormatInt(n, 10)), nil
	case reflect.Float64, reflect.Float32:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != f {
			return nil, fmt.Errorf("%q is not a number", s)
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), nil
	case reflect.Slice:
		if t.Elem().Kind() != reflect.Struct && !strings.HasPrefix(s, "[") {
			out := []any{}
			if s == "" {
				return out, nil
			}
			for _, part := range strings.Split(s, ",") {
				v, err := parseTextValue(t.Elem(), strings.TrimSpace(part))
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}
	}
	root, err := config.ParseJSONC([]byte(s))
	if err != nil {
		return nil, fmt.Errorf("not valid JSON: %v", err)
	}
	var issues []issue
	checkNode("value", t, root, "", &issues)
	for _, is := range issues {
		if is.Error {
			return nil, fmt.Errorf("%s %s", is.Path, is.Msg)
		}
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(root.JSON()))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// toDoc converts a configuration to a generic JSON document.
func toDoc(c model.FeederConfig) (map[string]any, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	err = dec.Decode(&doc)
	return doc, err
}

// newElement returns the generic default element of a list (used when a
// path addresses the element just past the end, which appends it).
func newElement(p []pseg) any {
	switch pathString(p) {
	case "uplinks":
		u := model.DefaultUplink()
		u.ID = "uplink"
		return mustDoc(u)
	case "receivers":
		return mustDoc(defaultReceiver(0))
	}
	return map[string]any{}
}

func mustDoc(v any) any {
	b, _ := json.Marshal(v)
	var out any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

// setPath sets v at path p in doc. An index equal to the list length
// appends a default element.
func setPath(doc map[string]any, p []pseg, v any) (map[string]any, error) {
	var set func(cur any, i int) (any, error)
	set = func(cur any, i int) (any, error) {
		if i == len(p) {
			return v, nil
		}
		s := p[i]
		if s.isIndex() {
			list, _ := cur.([]any)
			switch {
			case s.idx < len(list):
			case s.idx == len(list):
				list = append(list, newElement(p[:i]))
			default:
				return nil, fmt.Errorf("list index %d is past the end (the list has %d elements)", s.idx, len(list))
			}
			nv, err := set(list[s.idx], i+1)
			if err != nil {
				return nil, err
			}
			list[s.idx] = nv
			return list, nil
		}
		m, _ := cur.(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		nv, err := set(m[s.key], i+1)
		if err != nil {
			return nil, err
		}
		m[s.key] = nv
		return m, nil
	}
	out, err := set(doc, 0)
	if err != nil {
		return doc, err
	}
	return out.(map[string]any), nil
}

// leafPaths lists every setting of FeederConfig with its env variable
// pattern ("[]" for list indices), for "config env".
func leafPaths() []string {
	var out []string
	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		switch t.Kind() {
		case reflect.Struct:
			names := make([]string, 0)
			fields := jsonFields(t)
			for i := 0; i < t.NumField(); i++ {
				if n, ok := jsonName(t.Field(i)); ok {
					names = append(names, n)
				}
			}
			for _, n := range names {
				if path == "" && (n == "version" || n == "mode") {
					continue
				}
				walk(fields[n].Type, joinPath(path, n))
			}
		case reflect.Slice:
			if t.Elem().Kind() == reflect.Struct {
				out = append(out, path) // whole list as JSON
				walk(t.Elem(), path+"[]")
				return
			}
			out = append(out, path)
		default:
			out = append(out, path)
		}
	}
	walk(reflect.TypeFor[model.FeederConfig](), "")
	return out
}
