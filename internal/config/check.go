package config

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	"airview/internal/model"
)

// Severity of an Issue.
type Severity uint8

// Severities.
const (
	SevError Severity = iota
	SevWarning
)

func (s Severity) String() string {
	if s == SevWarning {
		return "warning"
	}
	return "error"
}

// Issue is one problem found in a configuration source.
type Issue struct {
	Severity Severity
	// Source is where the problem is: a file path, "env AIRVIEW_X" or
	// "flag -set ...". Empty for problems of the merged configuration.
	Source string
	// Pos is the position in Source when it is a file (zero otherwise).
	Pos Pos
	// Path is the setting ("http.port", "feeds[1].host"); may be empty.
	Path string
	Msg  string
}

// String formats the issue as "error: file:12:5: http.port: message".
func (i Issue) String() string {
	var b strings.Builder
	b.WriteString(i.Severity.String())
	b.WriteString(": ")
	if i.Source != "" {
		b.WriteString(i.Source)
		if i.Pos.IsValid() {
			b.WriteString(":" + i.Pos.String())
		}
		b.WriteString(": ")
	}
	if i.Path != "" {
		b.WriteString(i.Path)
		b.WriteString(": ")
	}
	b.WriteString(i.Msg)
	return b.String()
}

// schemaChecker type-checks a parsed JSONC document against model.Config.
type schemaChecker struct {
	source string
	// unknownSev is the severity of unknown members (an error in the
	// operator file, a warning in config.json, which a newer AirView may have
	// written).
	unknownSev Severity
	issues     []Issue
}

func (c *schemaChecker) add(sev Severity, pos Pos, segs []seg, format string, a ...any) {
	c.issues = append(c.issues, Issue{Severity: sev, Source: c.source, Pos: pos, Path: segsString(segs), Msg: fmt.Sprintf(format, a...)})
}

// check reports whether n is usable as a value of type t, adding issues for
// everything that is not. For objects it checks every member and returns
// true when the object itself is usable (bad members are reported, and
// usableMember tells callers which ones to skip).
func (c *schemaChecker) check(t reflect.Type, n *Node, segs []seg) bool {
	if n.Kind == NullNode {
		if t.Kind() == reflect.Slice {
			return true
		}
		c.add(SevWarning, n.Pos, segs, "null is ignored; the current or default value is kept")
		return false
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != ObjectNode {
			c.add(SevError, n.Pos, segs, "expected an object, found %s", n.Kind)
			return false
		}
		seen := map[string]Pos{}
		for _, m := range n.Members {
			msegs := append(segs[:len(segs):len(segs)], seg{key: m.Key, idx: -1})
			if prev, dup := seen[m.Key]; dup {
				c.add(SevError, m.KeyPos, msegs, "duplicate setting (first given at line %d); only the last one is used", prev.Line)
			}
			seen[m.Key] = m.KeyPos
			c.member(t, m, msegs, len(segs) == 0)
		}
		return true
	case reflect.Slice:
		if n.Kind != ArrayNode {
			c.add(SevError, n.Pos, segs, "expected a list [...], found %s", n.Kind)
			return false
		}
		ok := true
		for i, e := range n.Elems {
			esegs := append(segs[:len(segs):len(segs)], seg{idx: i})
			if !c.check(t.Elem(), e, esegs) {
				ok = false
			}
		}
		return ok
	case reflect.String:
		if n.Kind == BoolNode && t == reflect.TypeOf(model.SplitSyncSetting("")) {
			c.add(SevWarning, n.Pos, segs, "the boolean form is deprecated; use \"auto\", \"on\" or \"off\" (true = \"on\", false = \"auto\")")
			return true
		}
		if n.Kind != StringNode {
			c.add(SevError, n.Pos, segs, "expected a string in double quotes, found %s", n.Kind)
			return false
		}
		return true
	case reflect.Bool:
		if n.Kind != BoolNode {
			hint := ""
			if n.Kind == StringNode && (n.Str == "true" || n.Str == "false") {
				hint = " (write true or false without quotes)"
			}
			c.add(SevError, n.Pos, segs, "expected true or false, found %s%s", n.Kind, hint)
			return false
		}
		return true
	case reflect.Float32, reflect.Float64:
		if n.Kind != NumberNode {
			c.add(SevError, n.Pos, segs, "expected a number, found %s%s", n.Kind, quotedNumberHint(n))
			return false
		}
		if f, err := strconv.ParseFloat(n.Raw, 64); err != nil || math.IsInf(f, 0) {
			c.add(SevError, n.Pos, segs, "number %s is out of range", n.Raw)
			return false
		}
		return true
	default: // integers
		if n.Kind != NumberNode {
			c.add(SevError, n.Pos, segs, "expected an integer, found %s%s", n.Kind, quotedNumberHint(n))
			return false
		}
		if _, err := strconv.ParseInt(n.Raw, 10, 64); err != nil {
			if f, ferr := strconv.ParseFloat(n.Raw, 64); ferr == nil && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
				return true // 1e3 or 30.0 decode fine
			}
			c.add(SevError, n.Pos, segs, "expected an integer, found %s", n.Raw)
			return false
		}
		return true
	}
}

func quotedNumberHint(n *Node) string {
	if n.Kind == StringNode {
		if _, err := strconv.ParseFloat(strings.TrimSpace(n.Str), 64); err == nil {
			return " (remove the quotes)"
		}
	}
	return ""
}

// member checks one object member against struct type t and reports whether
// it is a known, well-typed setting.
func (c *schemaChecker) member(t reflect.Type, m Member, segs []seg, top bool) bool {
	if top && m.Key == "version" {
		if m.Value.Kind != NumberNode {
			c.add(SevError, m.Value.Pos, segs, "expected an integer, found %s", m.Value.Kind)
			return false
		}
		return true
	}
	sf, ok := fieldByJSON(t, m.Key)
	if !ok {
		hint := ""
		for i := 0; i < t.NumField(); i++ {
			if n, ok := jsonName(t.Field(i)); ok && strings.EqualFold(n, m.Key) {
				hint = fmt.Sprintf(" (did you mean %q? names are case-sensitive)", n)
				break
			}
		}
		if hint == "" {
			if s := suggest(t, m.Key); s != "" {
				hint = fmt.Sprintf(" (did you mean %q?)", s)
			}
		}
		c.add(c.unknownSev, m.KeyPos, segs, "unknown setting%s", hint)
		return false
	}
	return c.check(sf.Type, m.Value, segs)
}

// suggest returns the member name of t closest to key (edit distance <= 2).
func suggest(t reflect.Type, key string) string {
	best, bestD := "", 3
	for i := 0; i < t.NumField(); i++ {
		n, ok := jsonName(t.Field(i))
		if !ok {
			continue
		}
		if d := editDistance(strings.ToLower(n), strings.ToLower(key)); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	if len(a) > 64 || len(b) > 64 {
		return 99
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// nodeAt finds the node at a concrete path in a document (nil if absent).
// For a member it returns the value; keyPos is the member name's position.
func nodeAt(root *Node, segs []seg) (n *Node, keyPos Pos) {
	n = root
	for _, s := range segs {
		if n == nil {
			return nil, Pos{}
		}
		if s.isIndex() {
			if n.Kind != ArrayNode || s.idx >= len(n.Elems) {
				return nil, Pos{}
			}
			n = n.Elems[s.idx]
			keyPos = n.Pos
			continue
		}
		var next *Node
		if n.Kind == ObjectNode {
			for _, m := range n.Members {
				if m.Key == s.key {
					next, keyPos = m.Value, m.KeyPos // last one wins
				}
			}
		}
		n = next
	}
	return n, keyPos
}

// checkDocument reports the issues of a whole document.
func checkDocument(source string, root *Node, unknownSev Severity) []Issue {
	c := &schemaChecker{source: source, unknownSev: unknownSev}
	c.check(reflect.TypeFor[model.Config](), root, nil)
	return c.issues
}
