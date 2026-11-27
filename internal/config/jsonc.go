package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// This file is a small JSON-with-comments ("JSONC") reader written for
// AirView's hand-edited configuration files. It accepts exactly JSON
// (RFC 8259) plus:
//
//   - line comments    // ... up to the end of the line
//   - block comments   /* ... */ (not nested)
//   - one trailing comma after the last element of an array or object
//   - a leading UTF-8 byte order mark
//
// It produces a syntax tree whose nodes carry their line and column, so
// "airview config check" can point at the exact place of a problem, and it
// converts that tree back to strict JSON for encoding/json.

// Pos is a position in a source file. Line and Col are 1-based; Col counts
// characters (runes), not bytes.
type Pos struct {
	Offset int // byte offset
	Line   int
	Col    int
}

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// IsValid reports whether p was set.
func (p Pos) IsValid() bool { return p.Line > 0 }

// NodeKind is the JSON type of a Node.
type NodeKind uint8

// Node kinds.
const (
	NullNode NodeKind = iota
	BoolNode
	NumberNode
	StringNode
	ArrayNode
	ObjectNode
)

var kindNames = [...]string{"null", "a boolean", "a number", "a string", "an array", "an object"}

func (k NodeKind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "unknown"
}

// Node is one JSON value of a parsed JSONC document.
type Node struct {
	Kind    NodeKind
	Pos     Pos
	Raw     string   // source text of a number, string (with quotes) or literal
	Str     string   // decoded value of a string
	Bool    bool     // value of a boolean
	Elems   []*Node  // array elements
	Members []Member // object members in source order
}

// Member is one "key": value pair of an object.
type Member struct {
	Key    string
	KeyPos Pos
	Value  *Node
}

// Get returns the value of the first member named key, or nil.
func (n *Node) Get(key string) *Node {
	if n == nil || n.Kind != ObjectNode {
		return nil
	}
	for _, m := range n.Members {
		if m.Key == key {
			return m.Value
		}
	}
	return nil
}

// SyntaxError is a JSONC syntax error at a position.
type SyntaxError struct {
	Pos Pos
	Msg string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Pos.Line, e.Pos.Col, e.Msg)
}

// maxDepth bounds nesting so hostile input cannot exhaust the stack.
const maxDepth = 200

// ParseJSONC parses a JSONC document holding exactly one value.
func ParseJSONC(data []byte) (*Node, error) {
	n, _, err := parseJSONC(data)
	return n, err
}

// parseJSONC is ParseJSONC that also reports whether the document contained
// comments or trailing commas (anything strict JSON would reject).
func parseJSONC(data []byte) (*Node, bool, error) {
	p := &jsoncParser{src: bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), line: 1}
	n, err := p.document()
	return n, p.extended, err
}

func (p *jsoncParser) document() (*Node, error) {
	if err := p.skip(); err != nil {
		return nil, err
	}
	if p.off >= len(p.src) {
		return nil, p.errf(p.off, "the file is empty")
	}
	n, err := p.value(0)
	if err != nil {
		return nil, err
	}
	if err := p.skip(); err != nil {
		return nil, err
	}
	if p.off < len(p.src) {
		return nil, p.errf(p.off, "unexpected %s after the end of the document", p.describe(p.off))
	}
	return n, nil
}

// StripJSONC converts a JSONC document into compact strict JSON.
func StripJSONC(data []byte) ([]byte, error) {
	n, err := ParseJSONC(data)
	if err != nil {
		return nil, err
	}
	return n.JSON(), nil
}

// JSON returns the node as compact strict JSON.
func (n *Node) JSON() []byte {
	var b bytes.Buffer
	n.writeJSON(&b)
	return b.Bytes()
}

func (n *Node) writeJSON(b *bytes.Buffer) {
	switch n.Kind {
	case NullNode:
		b.WriteString("null")
	case BoolNode:
		if n.Bool {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case NumberNode, StringNode:
		b.WriteString(n.Raw)
	case ArrayNode:
		b.WriteByte('[')
		for i, e := range n.Elems {
			if i > 0 {
				b.WriteByte(',')
			}
			e.writeJSON(b)
		}
		b.WriteByte(']')
	case ObjectNode:
		b.WriteByte('{')
		for i, m := range n.Members {
			if i > 0 {
				b.WriteByte(',')
			}
			k, _ := json.Marshal(m.Key)
			b.Write(k)
			b.WriteByte(':')
			m.Value.writeJSON(b)
		}
		b.WriteByte('}')
	}
}

// Value converts the node into the generic encoding/json representation
// (map[string]any, []any, json.Number, string, bool, nil). For duplicate
// keys the last one wins, as in encoding/json.
func (n *Node) Value() any {
	switch n.Kind {
	case BoolNode:
		return n.Bool
	case NumberNode:
		return json.Number(n.Raw)
	case StringNode:
		return n.Str
	case ArrayNode:
		out := make([]any, len(n.Elems))
		for i, e := range n.Elems {
			out[i] = e.Value()
		}
		return out
	case ObjectNode:
		out := make(map[string]any, len(n.Members))
		for _, m := range n.Members {
			out[m.Key] = m.Value.Value()
		}
		return out
	}
	return nil
}

type jsoncParser struct {
	src       []byte
	off       int
	line      int
	lineStart int
	extended  bool // a comment or trailing comma was seen
}

func (p *jsoncParser) pos(off int) Pos {
	// off is always at or after lineStart when called for the current token.
	ls := p.lineStart
	line := p.line
	if off < ls { // rare: position on an earlier line; recount
		line, ls = 1, 0
		for i := 0; i < off; i++ {
			if p.src[i] == '\n' {
				line++
				ls = i + 1
			}
		}
	}
	return Pos{Offset: off, Line: line, Col: utf8.RuneCount(p.src[ls:off]) + 1}
}

func (p *jsoncParser) errf(off int, format string, a ...any) error {
	return &SyntaxError{Pos: p.pos(off), Msg: fmt.Sprintf(format, a...)}
}

func (p *jsoncParser) describe(off int) string {
	if off >= len(p.src) {
		return "end of file"
	}
	r, _ := utf8.DecodeRune(p.src[off:])
	if r == utf8.RuneError {
		return "invalid UTF-8"
	}
	return strconv.QuoteRune(r)
}

func (p *jsoncParser) newline(off int) {
	p.line++
	p.lineStart = off + 1
}

// skip passes white space and comments.
func (p *jsoncParser) skip() error {
	for p.off < len(p.src) {
		switch c := p.src[p.off]; c {
		case ' ', '\t', '\r':
			p.off++
		case '\n':
			p.newline(p.off)
			p.off++
		case '/':
			if p.off+1 >= len(p.src) {
				return p.errf(p.off, "unexpected '/'")
			}
			p.extended = true
			switch p.src[p.off+1] {
			case '/':
				for p.off < len(p.src) && p.src[p.off] != '\n' {
					p.off++
				}
			case '*':
				start := p.off
				p.off += 2
				for {
					if p.off+1 >= len(p.src) {
						return p.errf(start, "block comment is not closed with */")
					}
					if p.src[p.off] == '*' && p.src[p.off+1] == '/' {
						p.off += 2
						break
					}
					if p.src[p.off] == '\n' {
						p.newline(p.off)
					}
					p.off++
				}
			default:
				return p.errf(p.off, "unexpected '/' (comments start with // or /*)")
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *jsoncParser) value(depth int) (*Node, error) {
	if depth > maxDepth {
		return nil, p.errf(p.off, "nesting is deeper than %d levels", maxDepth)
	}
	if p.off >= len(p.src) {
		return nil, p.errf(p.off, "unexpected end of file, a value was expected")
	}
	start := p.off
	switch c := p.src[p.off]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		raw, s, err := p.str()
		if err != nil {
			return nil, err
		}
		return &Node{Kind: StringNode, Pos: p.pos(start), Raw: raw, Str: s}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case c >= 'a' && c <= 'z':
		end := p.off
		for end < len(p.src) && p.src[end] >= 'a' && p.src[end] <= 'z' {
			end++
		}
		word := string(p.src[p.off:end])
		n := &Node{Pos: p.pos(start), Raw: word}
		switch word {
		case "true":
			n.Kind, n.Bool = BoolNode, true
		case "false":
			n.Kind = BoolNode
		case "null":
			n.Kind = NullNode
		default:
			return nil, p.errf(start, "unknown word %q (strings must be in double quotes)", word)
		}
		p.off = end
		return n, nil
	case c == '\'':
		return nil, p.errf(start, "strings must use double quotes, not single quotes")
	default:
		return nil, p.errf(start, "unexpected %s, a value was expected", p.describe(start))
	}
}

func (p *jsoncParser) object(depth int) (*Node, error) {
	n := &Node{Kind: ObjectNode, Pos: p.pos(p.off)}
	p.off++ // {
	for {
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.off >= len(p.src) {
			return nil, p.errf(n.Pos.Offset, "object is not closed with }")
		}
		if p.src[p.off] == '}' {
			p.extended = p.extended || len(n.Members) > 0
			p.off++
			return n, nil
		}
		if p.src[p.off] != '"' {
			return nil, p.errf(p.off, "unexpected %s, a quoted member name or } was expected", p.describe(p.off))
		}
		kpos := p.pos(p.off)
		_, key, err := p.str()
		if err != nil {
			return nil, err
		}
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.off >= len(p.src) || p.src[p.off] != ':' {
			return nil, p.errf(p.off, "unexpected %s, ':' was expected after member name %q", p.describe(p.off), key)
		}
		p.off++
		if err := p.skip(); err != nil {
			return nil, err
		}
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		n.Members = append(n.Members, Member{Key: key, KeyPos: kpos, Value: v})
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.off >= len(p.src) {
			return nil, p.errf(n.Pos.Offset, "object is not closed with }")
		}
		switch p.src[p.off] {
		case ',':
			p.off++
		case '}':
			p.off++
			return n, nil
		default:
			return nil, p.errf(p.off, "unexpected %s, ',' or } was expected (is a comma missing?)", p.describe(p.off))
		}
	}
}

func (p *jsoncParser) array(depth int) (*Node, error) {
	n := &Node{Kind: ArrayNode, Pos: p.pos(p.off)}
	p.off++ // [
	for {
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.off >= len(p.src) {
			return nil, p.errf(n.Pos.Offset, "array is not closed with ]")
		}
		if p.src[p.off] == ']' {
			p.extended = p.extended || len(n.Elems) > 0
			p.off++
			return n, nil
		}
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		n.Elems = append(n.Elems, v)
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.off >= len(p.src) {
			return nil, p.errf(n.Pos.Offset, "array is not closed with ]")
		}
		switch p.src[p.off] {
		case ',':
			p.off++
		case ']':
			p.off++
			return n, nil
		default:
			return nil, p.errf(p.off, "unexpected %s, ',' or ] was expected (is a comma missing?)", p.describe(p.off))
		}
	}
}

// str scans a JSON string starting at the opening quote and returns its raw
// text and decoded value.
func (p *jsoncParser) str() (raw, val string, err error) {
	start := p.off
	i := p.off + 1
	for {
		if i >= len(p.src) {
			return "", "", p.errf(start, "string is not closed with \"")
		}
		c := p.src[i]
		switch {
		case c == '"':
			raw = string(p.src[start : i+1])
			if err := json.Unmarshal([]byte(raw), &val); err != nil {
				return "", "", p.errf(start, "invalid string: %v", err)
			}
			p.off = i + 1
			return raw, val, nil
		case c == '\\':
			if i+1 >= len(p.src) {
				return "", "", p.errf(start, "string is not closed with \"")
			}
			switch p.src[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if i+6 > len(p.src) {
					return "", "", p.errf(start, "string is not closed with \"")
				}
				for _, h := range p.src[i+2 : i+6] {
					if !isHex(h) {
						return "", "", p.errf(i, "invalid \\u escape in string")
					}
				}
				i += 6
			default:
				return "", "", p.errf(i, "invalid escape \\%c in string (write \\\\ for a backslash)", p.src[i+1])
			}
		case c < 0x20:
			if c == '\n' {
				return "", "", p.errf(start, "string is not closed with \" before the end of the line")
			}
			return "", "", p.errf(i, "control character in string (use \\n, \\t, ...)")
		case c < utf8.RuneSelf:
			i++
		default:
			r, size := utf8.DecodeRune(p.src[i:])
			if r == utf8.RuneError && size <= 1 {
				return "", "", p.errf(i, "invalid UTF-8 in string")
			}
			i += size
		}
	}
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// number scans a JSON number.
func (p *jsoncParser) number() (*Node, error) {
	start := p.off
	i := p.off
	digits := func() int {
		n := 0
		for i < len(p.src) && p.src[i] >= '0' && p.src[i] <= '9' {
			i++
			n++
		}
		return n
	}
	if p.src[i] == '-' {
		i++
	}
	if i < len(p.src) && p.src[i] == '0' {
		i++
		if i < len(p.src) && p.src[i] >= '0' && p.src[i] <= '9' {
			return nil, p.errf(start, "numbers must not have leading zeros")
		}
	} else if digits() == 0 {
		return nil, p.errf(start, "invalid number")
	}
	if i < len(p.src) && p.src[i] == '.' {
		i++
		if digits() == 0 {
			return nil, p.errf(start, "invalid number: digits must follow the decimal point")
		}
	}
	if i < len(p.src) && (p.src[i] == 'e' || p.src[i] == 'E') {
		i++
		if i < len(p.src) && (p.src[i] == '+' || p.src[i] == '-') {
			i++
		}
		if digits() == 0 {
			return nil, p.errf(start, "invalid number: bad exponent")
		}
	}
	if i < len(p.src) {
		if c := p.src[i]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '.' {
			return nil, p.errf(start, "invalid number")
		}
	}
	n := &Node{Kind: NumberNode, Pos: p.pos(start), Raw: string(p.src[start:i])}
	p.off = i
	return n, nil
}
