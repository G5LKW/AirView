package feed

import (
	"strings"
	"time"
)

// Beast extension messages (escape types BeastReceiverID, BeastUUID,
// BeastPing, BeastOption) as spoken on readsb-style aggregator connections
// (beast_reduce_plus_out). Written from the observable wire behaviour:
//
//   - 0x1A 0xE4 + 36 ASCII bytes: the feeder UUID, once after connecting.
//     Shorter UUIDs are padded (commonly with 'f').
//   - 0x1A 0xE3 + 8 bytes: big-endian receiver ID; it applies to the frames
//     that follow until the next 0xE3 (relays merging several receivers).
//   - 0x1A 'W' + 1 byte: an option or request; 'O' asks for pings, 'S' asks
//     the sender to slow down.
//   - 0x1A 'P' + 3 bytes: a 24-bit millisecond counter; the receiving side
//     echoes it so the originator can measure the round trip.
//
// The usual Beast escaping (a literal 0x1A doubled) applies to the bodies.

// Beast option bytes carried by BeastOption.
const (
	BeastOptPing     = 'O' // request pings
	BeastOptSlowDown = 'S' // request the sender to reduce its rate
)

// AppendBeastEscape appends one extension message with the given type and
// body (escaped) to dst.
func AppendBeastEscape(dst []byte, typ byte, body []byte) []byte {
	dst = append(dst, beastEsc, typ)
	for _, b := range body {
		dst = append(dst, b)
		if b == beastEsc {
			dst = append(dst, beastEsc)
		}
	}
	return dst
}

// AppendBeastPing appends a 'P' message carrying the low 24 bits of v.
func AppendBeastPing(dst []byte, v uint32) []byte {
	return AppendBeastEscape(dst, BeastPing, []byte{byte(v >> 16), byte(v >> 8), byte(v)})
}

// AppendBeastOption appends a 'W' message with option byte opt.
func AppendBeastOption(dst []byte, opt byte) []byte {
	return AppendBeastEscape(dst, BeastOption, []byte{opt})
}

// AppendBeastReceiverID appends a 0xE3 receiver-ID message.
func AppendBeastReceiverID(dst []byte, id uint64) []byte {
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(id >> (56 - 8*i))
	}
	return AppendBeastEscape(dst, BeastReceiverID, b[:])
}

// AppendBeastUUID appends a 0xE4 UUID message. uuid is truncated or padded
// with 'f' to BeastUUIDLen bytes.
func AppendBeastUUID(dst []byte, uuid string) []byte {
	var b [BeastUUIDLen]byte
	n := copy(b[:], uuid)
	for i := n; i < len(b); i++ {
		b[i] = 'f'
	}
	return AppendBeastEscape(dst, BeastUUID, b[:])
}

// ParseBeastPing returns the 24-bit value of a 'P' body.
func ParseBeastPing(body []byte) (uint32, bool) {
	if len(body) != 3 {
		return 0, false
	}
	return uint32(body[0])<<16 | uint32(body[1])<<8 | uint32(body[2]), true
}

// ParseBeastReceiverID returns the receiver ID of a 0xE3 body.
func ParseBeastReceiverID(body []byte) (uint64, bool) {
	if len(body) != 8 {
		return 0, false
	}
	var id uint64
	for _, b := range body {
		id = id<<8 | uint64(b)
	}
	return id, true
}

// ParseBeastUUID normalises the body of a 0xE4 message (DESIGN §3): NUL and
// space padding and dashes are removed and the rest lower-cased; the result
// must be 28..36 hexadecimal characters and not all zero (a "null UUID").
func ParseBeastUUID(body []byte) (string, bool) {
	return NormalizeUUID(string(body))
}

// NormalizeUUID applies the UUID normalisation of ParseBeastUUID to a
// string (also used for UUIDs from other sources, e.g. mlat-client).
func NormalizeUUID(s string) (string, bool) {
	s = strings.TrimRight(s, "\x00 \r\n\t")
	s = strings.TrimLeft(s, " \t")
	var b strings.Builder
	b.Grow(len(s))
	zero := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-':
			continue
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
			c += 'a' - 'A'
		default:
			return "", false
		}
		if c != '0' {
			zero = false
		}
		b.WriteByte(c)
	}
	if b.Len() < 28 || b.Len() > BeastUUIDLen || zero {
		return "", false
	}
	return b.String(), true
}

// BeastPingValue returns the 24-bit millisecond counter for a ping sent at
// t (milliseconds since the Unix epoch, modulo 2^24).
func BeastPingValue(t time.Time) uint32 {
	return uint32(t.UnixMilli()) & (1<<24 - 1)
}

// BeastPingRTT returns the round-trip time of an echoed ping value v
// received back at now (computed modulo the 4.66 h counter period). It
// reports false when the result is implausible (negative or above 60 s),
// for instance for an echo of a ping we did not send.
func BeastPingRTT(v uint32, now time.Time) (time.Duration, bool) {
	d := (BeastPingValue(now) - v) & (1<<24 - 1)
	if d > 60_000 {
		return 0, false
	}
	return time.Duration(d) * time.Millisecond, true
}
