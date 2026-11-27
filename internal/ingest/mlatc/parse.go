package mlatc

import (
	"errors"
	"math"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// The client sends one JSON object per line. The hot messages (mlat, sync)
// arrive at hundreds per second per client, so they are parsed by this
// small validating scanner without allocations instead of encoding/json.
// It accepts exactly RFC 8259 JSON (no comments, no trailing commas, no
// NaN), with any amount of whitespace; a line that is not valid JSON is
// rejected as a whole. Unknown keys and unknown messages are skipped.

type msgKind uint8

const (
	kUnknown msgKind = iota
	kMLAT
	kSync
	kSSync
	kSeen
	kLost
	kRate
	kClockReset
	kClockJump
	kInputConnected
	kInputDisconnected
	kHeartbeat
	kPosition
)

var errSyntax = errors.New("mlatc: invalid JSON")

// maxDepth bounds nesting in skipped values.
const maxDepth = 32

// maxListLen bounds the addresses / rates kept from one message.
const maxListLen = 8192

// message is the decoded content of one top-level key.
type message struct {
	kind msgKind
	// mlat / ssync: one frame; sync: two (even, odd).
	n   int
	t   [2]uint64
	tok [2]bool // t present and a valid non-negative number
	m   [2][]byte
	fb  [2][14]byte
	// seen / lost (24-bit addresses; entries that are not addresses are
	// counted in skipped).
	addrs   []uint32
	rates   []AircraftRate
	skipped int
	// clock_reset
	reason, mode, epoch string
	freq                float64
	// position_update
	lat, lon, alt float64
	hasPos        bool
	// heartbeat
	clientTime float64
}

// parser scans one line. Reuse it: the slices in msg are kept.
type parser struct {
	b   []byte
	i   int
	msg message
	sb  []byte // unescape scratch
	top bool   // inside the top-level object
	cnt int    // keys seen at top level
}

func isWS(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (p *parser) ws() {
	for p.i < len(p.b) && isWS(p.b[p.i]) {
		p.i++
	}
}

func (p *parser) peek() byte {
	if p.i < len(p.b) {
		return p.b[p.i]
	}
	return 0
}

func (p *parser) expect(c byte) error {
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == c {
		p.i++
		return nil
	}
	return errSyntax
}

// begin starts a line: it must hold one JSON object.
func (p *parser) begin(line []byte) error {
	p.b, p.i, p.top, p.cnt = line, 0, false, 0
	if err := p.expect('{'); err != nil {
		return err
	}
	p.top = true
	return nil
}

// next decodes the next top-level key into p.msg. It returns false after
// the closing brace, which must be followed only by whitespace.
func (p *parser) next() (bool, error) {
	if !p.top {
		return false, nil
	}
	p.ws()
	if p.peek() == '}' {
		p.i++
		p.top = false
		p.ws()
		if p.i != len(p.b) {
			return false, errSyntax
		}
		return false, nil
	}
	if p.cnt > 0 {
		if err := p.expect(','); err != nil {
			return false, err
		}
		p.ws()
	}
	p.cnt++
	key, err := p.str()
	if err != nil {
		return false, err
	}
	if err := p.expect(':'); err != nil {
		return false, err
	}
	p.ws()
	m := &p.msg
	m.kind, m.n, m.tok, m.skipped = kUnknown, 0, [2]bool{}, 0
	m.addrs, m.rates = m.addrs[:0], m.rates[:0]
	switch string(key) {
	case "mlat":
		m.kind = kMLAT
		return true, p.frames(false)
	case "ssync":
		m.kind = kSSync
		return true, p.frames(false)
	case "sync":
		m.kind = kSync
		return true, p.frames(true)
	case "seen":
		m.kind = kSeen
		return true, p.addrList()
	case "lost":
		m.kind = kLost
		return true, p.addrList()
	case "rate_report":
		m.kind = kRate
		return true, p.rateMap()
	case "clock_reset":
		m.kind = kClockReset
		return true, p.clockReset()
	case "clock_jump":
		m.kind = kClockJump
	case "input_connected":
		m.kind = kInputConnected
	case "input_disconnected":
		m.kind = kInputDisconnected
	case "heartbeat":
		m.kind = kHeartbeat
		return true, p.heartbeat()
	case "position_update":
		m.kind = kPosition
		return true, p.position()
	}
	return true, p.skip(0)
}

// str reads a string and returns its content (unescaped; the slice is
// valid until the next call).
func (p *parser) str() ([]byte, error) {
	if p.peek() != '"' {
		return nil, errSyntax
	}
	p.i++
	start := p.i
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch {
		case c == '"':
			s := p.b[start:p.i]
			p.i++
			return s, nil
		case c == '\\':
			return p.strEscaped(start)
		case c < 0x20:
			return nil, errSyntax
		}
		p.i++
	}
	return nil, errSyntax
}

func (p *parser) strEscaped(start int) ([]byte, error) {
	sb := append(p.sb[:0], p.b[start:p.i]...)
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			p.sb = sb
			return sb, nil
		case c < 0x20:
			return nil, errSyntax
		case c != '\\':
			sb = append(sb, c)
			p.i++
			continue
		}
		if p.i+1 >= len(p.b) {
			return nil, errSyntax
		}
		e := p.b[p.i+1]
		p.i += 2
		switch e {
		case '"', '\\', '/':
			sb = append(sb, e)
		case 'b':
			sb = append(sb, '\b')
		case 'f':
			sb = append(sb, '\f')
		case 'n':
			sb = append(sb, '\n')
		case 'r':
			sb = append(sb, '\r')
		case 't':
			sb = append(sb, '\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return nil, errSyntax
			}
			if utf16.IsSurrogate(r) {
				r2 := utf8.RuneError
				if p.i+1 < len(p.b) && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
					save := p.i
					p.i += 2
					if x, ok := p.hex4(); ok {
						if d := utf16.DecodeRune(r, x); d != utf8.RuneError {
							r2 = d
						} else {
							p.i = save
						}
					} else {
						return nil, errSyntax
					}
				}
				r = r2
			}
			sb = utf8.AppendRune(sb, r)
		default:
			return nil, errSyntax
		}
	}
	return nil, errSyntax
}

func (p *parser) hex4() (rune, bool) {
	if p.i+4 > len(p.b) {
		return 0, false
	}
	var r rune
	for _, c := range p.b[p.i : p.i+4] {
		v, ok := hexVal(c)
		if !ok {
			return 0, false
		}
		r = r<<4 | rune(v)
	}
	p.i += 4
	return r, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// num reads a JSON number. u holds its exact value when it is a
// non-negative integer that fits in 64 bits (isUint); f always holds the
// float value.
func (p *parser) num() (u uint64, f float64, isUint bool, err error) {
	start := p.i
	neg := false
	if p.peek() == '-' {
		neg = true
		p.i++
	}
	ds := p.i
	if p.peek() == '0' {
		p.i++
	} else {
		if c := p.peek(); c < '1' || c > '9' {
			return 0, 0, false, errSyntax
		}
		for c := p.peek(); c >= '0' && c <= '9'; c = p.peek() {
			p.i++
		}
	}
	intEnd := p.i
	frac := false
	if p.peek() == '.' {
		p.i++
		if c := p.peek(); c < '0' || c > '9' {
			return 0, 0, false, errSyntax
		}
		for c := p.peek(); c >= '0' && c <= '9'; c = p.peek() {
			p.i++
		}
		frac = true
	}
	if c := p.peek(); c == 'e' || c == 'E' {
		p.i++
		if c := p.peek(); c == '+' || c == '-' {
			p.i++
		}
		if c := p.peek(); c < '0' || c > '9' {
			return 0, 0, false, errSyntax
		}
		for c := p.peek(); c >= '0' && c <= '9'; c = p.peek() {
			p.i++
		}
		frac = true
	}
	if !frac && !neg && intEnd-ds <= 20 {
		exact := true
		for _, c := range p.b[ds:intEnd] {
			d := uint64(c - '0')
			if u > (math.MaxUint64-d)/10 {
				exact = false
				break
			}
			u = u*10 + d
		}
		if exact {
			return u, float64(u), true, nil
		}
		u = 0
	}
	f, perr := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if perr != nil && !errors.Is(perr, strconv.ErrRange) {
		return 0, 0, false, errSyntax
	}
	if !neg && f >= 0 && f < 1<<64 && f == math.Trunc(f) {
		return uint64(f), f, true, nil
	}
	return 0, f, false, nil
}

// lit consumes a literal (true, false, null).
func (p *parser) lit(s string) error {
	if len(p.b)-p.i >= len(s) && string(p.b[p.i:p.i+len(s)]) == s {
		p.i += len(s)
		return nil
	}
	return errSyntax
}

// skip validates and skips one value.
func (p *parser) skip(depth int) error {
	if depth > maxDepth {
		return errSyntax
	}
	p.ws()
	switch c := p.peek(); {
	case c == '"':
		_, err := p.str()
		return err
	case c == '{':
		p.i++
		p.ws()
		if p.peek() == '}' {
			p.i++
			return nil
		}
		for {
			p.ws()
			if _, err := p.str(); err != nil {
				return err
			}
			if err := p.expect(':'); err != nil {
				return err
			}
			if err := p.skip(depth + 1); err != nil {
				return err
			}
			p.ws()
			switch p.peek() {
			case ',':
				p.i++
			case '}':
				p.i++
				return nil
			default:
				return errSyntax
			}
		}
	case c == '[':
		p.i++
		p.ws()
		if p.peek() == ']' {
			p.i++
			return nil
		}
		for {
			if err := p.skip(depth + 1); err != nil {
				return err
			}
			p.ws()
			switch p.peek() {
			case ',':
				p.i++
			case ']':
				p.i++
				return nil
			default:
				return errSyntax
			}
		}
	case c == 't':
		return p.lit("true")
	case c == 'f':
		return p.lit("false")
	case c == 'n':
		return p.lit("null")
	case c == '-' || (c >= '0' && c <= '9'):
		_, _, _, err := p.num()
		return err
	}
	return errSyntax
}

// object iterates the members of an object value; fn is called with the
// parser positioned at each member's value and must consume it. A value
// that is not an object is skipped (tolerated) and reported as notObj.
func (p *parser) object(fn func(key []byte) error) (notObj bool, err error) {
	p.ws()
	if p.peek() != '{' {
		return true, p.skip(0)
	}
	p.i++
	p.ws()
	if p.peek() == '}' {
		p.i++
		return false, nil
	}
	for {
		p.ws()
		k, err := p.str()
		if err != nil {
			return false, err
		}
		if err := p.expect(':'); err != nil {
			return false, err
		}
		p.ws()
		if err := fn(k); err != nil {
			return false, err
		}
		p.ws()
		switch p.peek() {
		case ',':
			p.i++
		case '}':
			p.i++
			return false, nil
		default:
			return false, errSyntax
		}
	}
}

// frames decodes {"t":…,"m":…} or {"et":…,"em":…,"ot":…,"om":…}. The
// member loop is written out (no closure) to keep the hot path free of
// allocations.
func (p *parser) frames(pair bool) error {
	m := &p.msg
	if pair {
		m.n = 2
	} else {
		m.n = 1
	}
	m.m[0], m.m[1] = nil, nil
	p.ws()
	if p.peek() != '{' {
		m.n = 0
		return p.skip(0)
	}
	p.i++
	p.ws()
	if p.peek() == '}' {
		p.i++
		return nil
	}
	for {
		p.ws()
		k, err := p.str()
		if err != nil {
			return err
		}
		if err := p.expect(':'); err != nil {
			return err
		}
		p.ws()
		slot, isT := -1, false
		switch {
		case !pair && len(k) == 1 && k[0] == 't':
			slot, isT = 0, true
		case !pair && len(k) == 1 && k[0] == 'm':
			slot = 0
		case pair && len(k) == 2 && k[0] == 'e' && k[1] == 't':
			slot, isT = 0, true
		case pair && len(k) == 2 && k[0] == 'e' && k[1] == 'm':
			slot = 0
		case pair && len(k) == 2 && k[0] == 'o' && k[1] == 't':
			slot, isT = 1, true
		case pair && len(k) == 2 && k[0] == 'o' && k[1] == 'm':
			slot = 1
		}
		switch {
		case slot < 0:
			if err := p.skip(1); err != nil {
				return err
			}
		case isT:
			c := p.peek()
			if c == '-' || (c >= '0' && c <= '9') {
				u, _, ok, err := p.num()
				if err != nil {
					return err
				}
				m.t[slot], m.tok[slot] = u, ok
			} else if err := p.skip(1); err != nil {
				return err
			}
		default:
			if p.peek() == '"' {
				s, err := p.str()
				if err != nil {
					return err
				}
				m.m[slot] = decodeFrame(s, m.fb[slot][:0])
			} else if err := p.skip(1); err != nil {
				return err
			}
		}
		p.ws()
		switch p.peek() {
		case ',':
			p.i++
		case '}':
			p.i++
			return nil
		default:
			return errSyntax
		}
	}
}

// decodeFrame decodes a hex Mode S / Mode A/C frame (4, 14 or 28 hex
// digits) into dst; nil for anything else.
func decodeFrame(s, dst []byte) []byte {
	switch len(s) {
	case 4, 14, 28:
	default:
		return nil
	}
	for i := 0; i < len(s); i += 2 {
		hi, ok1 := hexVal(s[i])
		lo, ok2 := hexVal(s[i+1])
		if !ok1 || !ok2 {
			return nil
		}
		dst = append(dst, hi<<4|lo)
	}
	return dst
}

// parseAddr parses a 24-bit address of up to 6 hex digits.
func parseAddr(s []byte) (uint32, bool) {
	if len(s) == 0 || len(s) > 6 {
		return 0, false
	}
	var v uint32
	for _, c := range s {
		h, ok := hexVal(c)
		if !ok {
			return 0, false
		}
		v = v<<4 | uint32(h)
	}
	return v, true
}

func (p *parser) addrList() error {
	m := &p.msg
	p.ws()
	if p.peek() != '[' {
		m.skipped++
		return p.skip(0)
	}
	p.i++
	p.ws()
	if p.peek() == ']' {
		p.i++
		return nil
	}
	for {
		p.ws()
		switch c := p.peek(); {
		case c == '"':
			s, err := p.str()
			if err != nil {
				return err
			}
			if a, ok := parseAddr(s); ok && len(m.addrs) < maxListLen {
				m.addrs = append(m.addrs, a)
			} else {
				m.skipped++
			}
		case c == '-' || (c >= '0' && c <= '9'):
			u, _, ok, err := p.num()
			if err != nil {
				return err
			}
			if ok && u <= 0xffffff && len(m.addrs) < maxListLen {
				m.addrs = append(m.addrs, uint32(u))
			} else {
				m.skipped++
			}
		default:
			if err := p.skip(1); err != nil {
				return err
			}
			m.skipped++
		}
		p.ws()
		switch p.peek() {
		case ',':
			p.i++
		case ']':
			p.i++
			return nil
		default:
			return errSyntax
		}
	}
}

func (p *parser) rateMap() error {
	m := &p.msg
	notObj, err := p.object(func(k []byte) error {
		a, aok := parseAddr(k)
		c := p.peek()
		if c != '-' && (c < '0' || c > '9') {
			m.skipped++
			return p.skip(1)
		}
		_, f, _, err := p.num()
		if err != nil {
			return err
		}
		if !aok || !(f >= 0) || f > 1e6 || len(m.rates) >= maxListLen {
			m.skipped++
			return nil
		}
		m.rates = append(m.rates, AircraftRate{Addr: a, Rate: float32(f)})
		return nil
	})
	if notObj {
		m.skipped++
	}
	return err
}

// strOrNum reads a string or number member as text (for clock_reset's
// "epoch", which is a name in the reference client).
func (p *parser) strOrNum() (string, error) {
	switch c := p.peek(); {
	case c == '"':
		s, err := p.str()
		return string(s), err
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.i
		_, _, _, err := p.num()
		return string(p.b[start:p.i]), err
	}
	return "", p.skip(1)
}

func (p *parser) clockReset() error {
	m := &p.msg
	m.reason, m.mode, m.epoch, m.freq = "", "", "", 0
	_, err := p.object(func(k []byte) error {
		var err error
		switch string(k) {
		case "reason":
			m.reason, err = p.strOrNum()
		case "mode":
			m.mode, err = p.strOrNum()
		case "epoch":
			m.epoch, err = p.strOrNum()
		case "frequency":
			c := p.peek()
			if c == '-' || (c >= '0' && c <= '9') {
				_, m.freq, _, err = p.num()
			} else {
				err = p.skip(1)
			}
		default:
			err = p.skip(1)
		}
		return err
	})
	return err
}

func (p *parser) heartbeat() error {
	m := &p.msg
	m.clientTime = 0
	_, err := p.object(func(k []byte) error {
		c := p.peek()
		if string(k) == "client_time" && (c == '-' || (c >= '0' && c <= '9')) {
			_, f, _, err := p.num()
			m.clientTime = f
			return err
		}
		return p.skip(1)
	})
	return err
}

func (p *parser) position() error {
	m := &p.msg
	m.lat, m.lon, m.alt = math.NaN(), math.NaN(), math.NaN()
	_, err := p.object(func(k []byte) error {
		c := p.peek()
		if c != '-' && (c < '0' || c > '9') {
			return p.skip(1)
		}
		_, f, _, err := p.num()
		switch string(k) {
		case "lat":
			m.lat = f
		case "lon":
			m.lon = f
		case "alt":
			m.alt = f
		}
		return err
	})
	m.hasPos = err == nil && m.lat >= -90 && m.lat <= 90 && m.lon >= -180 && m.lon <= 180
	return err
}
