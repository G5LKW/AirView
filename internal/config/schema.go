package config

import (
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"airview/internal/model"
)

// This file describes every setting of model.Config by reflection: its
// dotted path, JSON type, environment variable and documentation key. The
// environment overrides, "airview config check", the example file and
// docs/CONFIG.md are all derived from it, so a new field in model.Config is
// picked up everywhere automatically.

// EnvPrefix starts every AirView environment variable.
const EnvPrefix = "AIRVIEW_"

// Environment variables that are not settings themselves.
const (
	// EnvConfigFile names the operator config file (like the -config flag).
	EnvConfigFile = "AIRVIEW_CONFIG"
	// EnvDataDir names the data directory (like the -data flag).
	EnvDataDir = "AIRVIEW_DATA"
)

// knownNonSettingEnv are AIRVIEW_* variables used elsewhere (program
// switches and test helpers); they are neither settings nor typos.
var knownNonSettingEnv = map[string]bool{
	EnvConfigFile:             true,
	EnvDataDir:                true,
	"AIRVIEW_REMOTE":          true,
	"AIRVIEW_DEBUG":           true,
	"AIRVIEW_TILECACHE_MB":    true,
	"AIRVIEW_LIVE_TESTS":      true,
	"AIRVIEW_LIVE_TERRAIN":    true,
	"AIRVIEW_GEN_API_DOC":     true,
	"AIRVIEW_REFDATA_DB":      true,
	"AIRVIEW_REFDATA_LIVE":    true,
	"AIRVIEW_CHECK_GENERATED": true,
	// First-start admin password of the Docker image (SEC-R2).
	"AIRVIEW_GENERATE_ADMIN_PASSWORD": true,
	"AIRVIEW_GEN_SERVER_WIRING":       true,
	"AIRVIEW_GROWTH":                  true,
}

// FieldKind is the value type of a setting.
type FieldKind uint8

// Field kinds.
const (
	KindString FieldKind = iota
	KindBool
	KindInt
	KindFloat
	KindIntList    // list of integers
	KindStringList // list of strings
	KindStructList // list of objects (antennas, feeds, uplinks)
)

var fieldKindNames = [...]string{"string", "boolean", "integer", "number", "list of integers", "list of strings", "list of objects"}

func (k FieldKind) String() string { return fieldKindNames[k] }

// FieldInfo describes one setting.
type FieldInfo struct {
	// Path is the dotted JSON path ("http.port"). Fields of list elements use
	// "[]" for the index ("feeds[].host").
	Path string
	// Env is the environment variable ("AIRVIEW_HTTP_PORT"); for fields of
	// list elements it contains "<n>" for the index ("AIRVIEW_FEEDS_<n>_HOST").
	Env  string
	Kind FieldKind
	Type reflect.Type
	// DocKey is "<Go type>.<Go field>" of the field in internal/model.
	DocKey string
	// Section is the dotted path of the enclosing object ("" = top level).
	Section string
	// InList is true for fields of list elements.
	InList bool
	// Secret marks values that are redacted and never printed.
	Secret bool
}

// secretPaths are the secret settings (see Redact).
var secretPaths = map[string]bool{
	"http.adminPasswordHash": true,
	"hub.password":           true,
	"remote.password":        true,
	"uplinks[].enrolToken":   true,
	"aggregator.outputToken": true,
	"aggregator.tlsKey":      true,
}

var (
	fieldsOnce sync.Once
	allFields  []FieldInfo
	fieldByEnv map[string]*FieldInfo // non-list leaves and list fields
	fieldByPth map[string]*FieldInfo
)

// Fields returns every setting in file order (the "version" stamp is not a
// setting and is not listed). List fields (KindStructList) are followed by
// the fields of their elements.
func Fields() []FieldInfo {
	fieldsOnce.Do(buildFields)
	return allFields
}

// FieldByPath returns the setting at a path ("http.port", "feeds[].host").
// Index numbers are accepted and ignored ("feeds[2].host").
func FieldByPath(path string) (FieldInfo, bool) {
	fieldsOnce.Do(buildFields)
	f, ok := fieldByPth[genericPath(path)]
	if !ok {
		return FieldInfo{}, false
	}
	return *f, true
}

// genericPath replaces list indices by "[]".
func genericPath(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] == '[' {
			j := strings.IndexByte(path[i:], ']')
			if j > 0 {
				b.WriteString("[]")
				i += j
				continue
			}
		}
		b.WriteByte(path[i])
	}
	return b.String()
}

func buildFields() {
	fieldByEnv = map[string]*FieldInfo{}
	fieldByPth = map[string]*FieldInfo{}
	walkStruct(reflect.TypeFor[model.Config](), "", EnvPrefix[:len(EnvPrefix)-1], false)
	for i := range allFields {
		f := &allFields[i]
		fieldByPth[f.Path] = f
		if !f.InList {
			fieldByEnv[f.Env] = f
		}
	}
}

func walkStruct(t reflect.Type, prefix, envPrefix string, inList bool) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		name, ok := jsonName(sf)
		if !ok || (prefix == "" && name == "version") {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		env := envPrefix + "_" + envSegment(name)
		info := FieldInfo{
			Path: path, Env: env, Type: sf.Type, DocKey: t.Name() + "." + sf.Name,
			Section: prefix, InList: inList, Secret: secretPaths[path],
		}
		ft := sf.Type
		switch ft.Kind() {
		case reflect.Struct:
			walkStruct(ft, path, env, inList)
			continue
		case reflect.Slice:
			switch ft.Elem().Kind() {
			case reflect.Struct:
				info.Kind = KindStructList
				allFields = append(allFields, info)
				walkStruct(ft.Elem(), path+"[]", env+"_<n>", true)
				continue
			case reflect.String:
				info.Kind = KindStringList
			default:
				info.Kind = KindIntList
			}
		case reflect.String:
			info.Kind = KindString
		case reflect.Bool:
			info.Kind = KindBool
		case reflect.Float32, reflect.Float64:
			info.Kind = KindFloat
		default:
			info.Kind = KindInt
		}
		allFields = append(allFields, info)
	}
}

// jsonName returns the JSON member name of a struct field.
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

// envSegment converts a JSON member name to the upper-case snake form used in
// environment variables: "bindAddr" -> "BIND_ADDR", "maxKBpsPerStation" ->
// "MAX_K_BPS_PER_STATION", "aircraftTtlS" -> "AIRCRAFT_TTL_S".
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

// seg is one step of a concrete path: a member name or a list index.
type seg struct {
	key string
	idx int // >= 0 for a list index, -1 for a member
}

func (s seg) isIndex() bool { return s.idx >= 0 }

func segsString(segs []seg) string {
	var b strings.Builder
	for i, s := range segs {
		if s.isIndex() {
			b.WriteString("[" + strconv.Itoa(s.idx) + "]")
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.key)
	}
	return b.String()
}

// parsePath parses "a.b[2].c" into segments.
func parsePath(p string) ([]seg, bool) {
	var out []seg
	p = strings.TrimSpace(p)
	if p == "" {
		return nil, false
	}
	for _, part := range strings.Split(p, ".") {
		name := part
		rest := ""
		if i := strings.IndexByte(part, '['); i >= 0 {
			name, rest = part[:i], part[i:]
		}
		if name == "" {
			return nil, false
		}
		out = append(out, seg{key: name, idx: -1})
		for rest != "" {
			j := strings.IndexByte(rest, ']')
			if rest[0] != '[' || j < 2 {
				return nil, false
			}
			n, err := strconv.Atoi(rest[1:j])
			if err != nil || n < 0 || n > 9999 {
				return nil, false
			}
			out = append(out, seg{idx: n})
			rest = rest[j+1:]
		}
	}
	return out, true
}

// typeAt resolves the Go type at a concrete path below model.Config. It
// fails for unknown members.
func typeAt(segs []seg) (reflect.Type, bool) {
	t := reflect.TypeFor[model.Config]()
	for _, s := range segs {
		if s.isIndex() {
			if t.Kind() != reflect.Slice {
				return nil, false
			}
			t = t.Elem()
			continue
		}
		if t.Kind() != reflect.Struct {
			return nil, false
		}
		sf, ok := fieldByJSON(t, s.key)
		if !ok {
			return nil, false
		}
		t = sf.Type
	}
	return t, true
}

// fieldByJSON finds the struct field with the exact JSON name.
func fieldByJSON(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if n, ok := jsonName(sf); ok && n == name {
			return sf, true
		}
	}
	return reflect.StructField{}, false
}

// resolveEnv maps an environment variable name to a concrete path. List
// element fields are addressed with their index: AIRVIEW_UPLINKS_0_HOST is
// uplinks[0].host and AIRVIEW_UPLINKS_0 is the whole element (JSON object).
func resolveEnv(name string) ([]seg, reflect.Type, bool) {
	if !strings.HasPrefix(name, EnvPrefix) {
		return nil, nil, false
	}
	return resolveEnvIn(reflect.TypeFor[model.Config](), strings.TrimPrefix(name, EnvPrefix), nil, true)
}

func resolveEnvIn(t reflect.Type, rest string, segs []seg, top bool) ([]seg, reflect.Type, bool) {
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			name, ok := jsonName(sf)
			if !ok || (top && name == "version") {
				continue
			}
			s := envSegment(name)
			next := append(segs[:len(segs):len(segs)], seg{key: name, idx: -1})
			if rest == s {
				if sf.Type.Kind() == reflect.Struct {
					continue // whole sections are not set from one variable
				}
				return next, sf.Type, true
			}
			if r, ok := strings.CutPrefix(rest, s+"_"); ok {
				if p, ft, ok := resolveEnvIn(sf.Type, r, next, false); ok {
					return p, ft, true
				}
			}
		}
	case reflect.Slice:
		if t.Elem().Kind() != reflect.Struct {
			return nil, nil, false
		}
		num, r, _ := strings.Cut(rest, "_")
		n, err := strconv.Atoi(num)
		if err != nil || n < 0 || n > 9999 || strconv.Itoa(n) != num {
			return nil, nil, false
		}
		next := append(segs[:len(segs):len(segs)], seg{idx: n})
		if r == "" && !strings.Contains(rest, "_") {
			return next, t.Elem(), true
		}
		return resolveEnvIn(t.Elem(), r, next, false)
	}
	return nil, nil, false
}
