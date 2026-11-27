package feed

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
)

// NonICAOFlag is set in DecodedUpdate.ICAO (bit 24) when the source marked
// the address as not being a real 24-bit ICAO address ("~" prefix in readsb
// / dump1090 output: TIS-B track files, anonymous addresses, ...). The lower
// 24 bits still hold the address as received.
const NonICAOFlag uint32 = 1 << 24

// ParseSBSLine parses one BaseStation ("port 30003") line into a
// DecodedUpdate. It reports false for lines that carry no aircraft update
// (other record types, malformed lines, unsupported MSG subtypes).
//
// Field layout (comma separated, 22 fields, trailing ones may be missing):
//
//	0 MSG  1 subtype  2 session  3 aircraft-id  4 hex  5 flight-id
//	6 date-generated  7 time-generated  8 date-logged  9 time-logged
//	10 callsign  11 altitude(ft)  12 ground-speed(kt)  13 track(deg)
//	14 lat  15 lon  16 vertical-rate(ft/min)  17 squawk
//	18 alert  19 emergency  20 SPI  21 on-ground
//
// Subtypes 1..8 are the classic BaseStation transmission types. Subtype 10
// is the Radarcape's MLAT position message and sets DecodedUpdate.MLAT.
// Subtype 9 (Radarcape FLARM, not a Mode S aircraft) is rejected.
//
// Every field that is present is used regardless of the subtype, which
// makes the parser tolerant of the many dialects in the wild. The caller
// sets Recv and FeedID.
func ParseSBSLine(line []byte) (model.DecodedUpdate, bool) {
	var u model.DecodedUpdate
	line = bytes.TrimSpace(line)
	if len(line) < 6 || !bytes.HasPrefix(line, []byte("MSG,")) {
		return u, false
	}
	var fields [22][]byte
	n := 0
	rest := line
	for n < len(fields) {
		i := bytes.IndexByte(rest, ',')
		if i < 0 {
			fields[n] = bytes.TrimSpace(rest)
			n++
			break
		}
		fields[n] = bytes.TrimSpace(rest[:i])
		n++
		rest = rest[i+1:]
	}
	if n < 5 {
		return u, false
	}

	subtype, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return u, false
	}
	switch {
	case subtype >= 1 && subtype <= 8:
	case subtype == 10:
		u.MLAT = true
	default:
		return u, false
	}

	icao, ok := parseHexAddr(string(fields[4]))
	if !ok {
		return u, false
	}
	u.ICAO = icao
	u.Msgs = 1

	if cs := strings.TrimSpace(string(fields[10])); cs != "" && cs != "@@@@@@@@" {
		u.Callsign = &cs
	}
	u.AltBaro = sbsFloat(fields[11], -5000, 150000)
	u.GS = sbsFloat(fields[12], 0, 10000)
	if u.Track = sbsFloat(fields[13], -360, 360); u.Track != nil && *u.Track < 0 {
		*u.Track += 360
	}
	lat := sbsFloat(fields[14], -90, 90)
	lon := sbsFloat(fields[15], -180, 180)
	if lat != nil && lon != nil && !(*lat == 0 && *lon == 0) {
		u.Lat, u.Lon = lat, lon
	}
	u.VertRate = sbsFloat(fields[16], -100000, 100000)
	if sq := string(fields[17]); isSquawk(sq) {
		if sq = normSquawk(sq); sq != "0000" || subtype == 6 {
			u.Squawk = &sq
		}
	}
	switch string(fields[21]) {
	case "-1", "1":
		u.OnGround = model.B(true)
	case "0":
		u.OnGround = model.B(false)
	}
	return u, true
}

// parseHexAddr parses a 24-bit address written as up to 6 hex digits, with
// an optional "~" prefix marking a non-ICAO address.
func parseHexAddr(s string) (uint32, bool) {
	s = strings.TrimSpace(s)
	var flag uint32
	if strings.HasPrefix(s, "~") {
		flag = NonICAOFlag
		s = s[1:]
	}
	if len(s) == 0 || len(s) > 6 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil || (v == 0 && flag == 0) {
		return 0, false
	}
	return uint32(v) | flag, true
}

func sbsFloat(b []byte, lo, hi float64) *float64 {
	if len(b) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil || math.IsNaN(v) || v < lo || v > hi {
		return nil
	}
	return &v
}

// isSquawk reports whether s is 1..4 octal digits.
func isSquawk(s string) bool {
	if len(s) == 0 || len(s) > 4 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

// normSquawk left-pads a squawk to four digits.
func normSquawk(s string) string {
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

// lineSplitter cuts a byte stream into lines without allocating per line.
// Lines longer than the buffer are discarded.
type lineSplitter struct {
	buf      [1024]byte
	n        int
	overflow bool
	Overlong uint64
}

// Write consumes a chunk and calls fn for every complete line (without the
// line terminator). The slice passed to fn is only valid during the call.
func (s *lineSplitter) Write(b []byte, fn func(line []byte)) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			s.add(b)
			return
		}
		if s.n == 0 && !s.overflow {
			if i > len(s.buf) {
				s.Overlong++
			} else {
				fn(trimCR(b[:i])) // whole line inside this chunk: no copy
			}
		} else {
			s.add(b[:i])
			if !s.overflow {
				fn(trimCR(s.buf[:s.n]))
			}
			s.n, s.overflow = 0, false
		}
		b = b[i+1:]
	}
}

func (s *lineSplitter) add(b []byte) {
	if s.overflow {
		return
	}
	if s.n+len(b) > len(s.buf) {
		s.overflow = true
		s.n = 0
		s.Overlong++
		return
	}
	s.n += copy(s.buf[s.n:], b)
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

// SBSDecoder turns a BaseStation text stream into DecodedUpdates.
type SBSDecoder struct {
	Sink     model.FrameSink
	FeedID   string
	MLATOnly bool
	Recv     time.Time // set once per socket read

	split   lineSplitter
	Updates uint64 // updates delivered
	Ignored uint64 // lines that were not usable aircraft updates
}

// Write consumes the next chunk of the stream.
func (d *SBSDecoder) Write(b []byte) {
	d.split.Write(b, d.line)
}

func (d *SBSDecoder) line(line []byte) {
	if len(line) == 0 {
		return
	}
	u, ok := ParseSBSLine(line)
	if !ok {
		d.Ignored++
		return
	}
	u.Recv = d.Recv
	u.FeedID = d.FeedID
	if d.MLATOnly {
		u.MLAT = true
	}
	d.Updates++
	d.Sink.HandleUpdate(u)
}
