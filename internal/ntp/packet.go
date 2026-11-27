// Package ntp provides AirView's timing reference: an SNTP/NTPv4 client
// (RFC 5905 packet format, RFC 4330 client behaviour) and a periodic Sampler
// that tracks the offset between the host system clock and the configured
// NTP server. The Sampler implements model.Clock.
//
// Sign convention everywhere in this package:
//
//	offset = true UTC (as told by the server) - host system clock
//
// so that  true UTC ≈ time.Now().Add(offset).
package ntp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// PacketSize is the size of an NTP header without extension fields or MAC.
const PacketSize = 48

// DefaultPort is the NTP UDP port used when the server string carries none.
const DefaultPort = "123"

// Association modes (RFC 5905 figure 10).
const (
	ModeSymmetricActive  = 1
	ModeSymmetricPassive = 2
	ModeClient           = 3
	ModeServer           = 4
	ModeBroadcast        = 5
)

// Leap indicator values.
const (
	LeapNone         = 0 // no warning
	LeapAddSecond    = 1 // last minute of the day has 61 seconds
	LeapDelSecond    = 2 // last minute of the day has 59 seconds
	LeapNotInSync    = 3 // clock unsynchronised (alarm condition)
	maxStratum       = 15
	ntpEpochOffset   = 2208988800 // seconds from 1900-01-01 (NTP era 0) to 1970-01-01 (Unix)
	eraSeconds       = int64(1) << 32
	maxRootDistance  = 16 * time.Second // RFC 5905 MAXDISP
	currentVersion   = 4
	minServerVersion = 1
)

// Timestamp is a 64-bit NTP timestamp: 32 bits of seconds since the start of
// the NTP era and 32 bits of binary fraction. The zero value means "unknown".
type Timestamp uint64

// TimestampFromTime converts t to an NTP timestamp (era information is lost,
// as on the wire).
func TimestampFromTime(t time.Time) Timestamp {
	secs := uint64(uint32(t.Unix() + ntpEpochOffset))
	frac := (uint64(t.Nanosecond()) << 32) / 1e9
	return Timestamp(secs<<32 | frac)
}

// Time converts the timestamp to a time.Time. The 32-bit seconds field wraps
// every ~136 years (first on 2036-02-07); the era is chosen so that the result
// lies within ±68 years of pivot (normally the local system time).
func (ts Timestamp) Time(pivot time.Time) time.Time {
	secs := int64(uint32(ts >> 32))
	frac := int64(uint32(ts))
	u := secs - ntpEpochOffset
	k := math.Round(float64(pivot.Unix()-u) / float64(eraSeconds))
	u += int64(k) * eraSeconds
	nsec := (frac*1e9 + 1<<31) >> 32
	return time.Unix(u, nsec).UTC()
}

// Short is the 32-bit NTP short format (16.16 fixed-point seconds) used for
// root delay and root dispersion.
type Short uint32

// Duration converts the short-format value to a time.Duration.
func (s Short) Duration() time.Duration {
	return time.Duration((int64(s)*1e9 + 1<<15) >> 16)
}

// ShortFromDuration converts d (clamped to 0 .. ~65536 s) to NTP short format.
func ShortFromDuration(d time.Duration) Short {
	if d <= 0 {
		return 0
	}
	v := (int64(d)<<16 + 5e8) / 1e9
	if v > math.MaxUint32 {
		v = math.MaxUint32
	}
	return Short(v)
}

// Packet is a decoded NTP header.
type Packet struct {
	Leap           uint8 // 2 bits
	Version        uint8 // 3 bits
	Mode           uint8 // 3 bits
	Stratum        uint8
	Poll           int8 // log2 seconds
	Precision      int8 // log2 seconds
	RootDelay      Short
	RootDispersion Short
	RefID          uint32
	Reference      Timestamp
	Origin         Timestamp
	Receive        Timestamp
	Transmit       Timestamp
}

// Marshal encodes the header into its 48-byte wire form.
func (p *Packet) Marshal() [PacketSize]byte {
	var b [PacketSize]byte
	b[0] = p.Leap<<6 | (p.Version&7)<<3 | p.Mode&7
	b[1] = p.Stratum
	b[2] = byte(p.Poll)
	b[3] = byte(p.Precision)
	binary.BigEndian.PutUint32(b[4:], uint32(p.RootDelay))
	binary.BigEndian.PutUint32(b[8:], uint32(p.RootDispersion))
	binary.BigEndian.PutUint32(b[12:], p.RefID)
	binary.BigEndian.PutUint64(b[16:], uint64(p.Reference))
	binary.BigEndian.PutUint64(b[24:], uint64(p.Origin))
	binary.BigEndian.PutUint64(b[32:], uint64(p.Receive))
	binary.BigEndian.PutUint64(b[40:], uint64(p.Transmit))
	return b
}

// ErrShortPacket is returned when a datagram is smaller than an NTP header.
var ErrShortPacket = errors.New("ntp: packet shorter than 48 bytes")

// ParsePacket decodes the first 48 bytes of b. Extension fields and MACs that
// may follow the header are ignored.
func ParsePacket(b []byte) (Packet, error) {
	if len(b) < PacketSize {
		return Packet{}, fmt.Errorf("%w (got %d)", ErrShortPacket, len(b))
	}
	return Packet{
		Leap:           b[0] >> 6,
		Version:        b[0] >> 3 & 7,
		Mode:           b[0] & 7,
		Stratum:        b[1],
		Poll:           int8(b[2]),
		Precision:      int8(b[3]),
		RootDelay:      Short(binary.BigEndian.Uint32(b[4:])),
		RootDispersion: Short(binary.BigEndian.Uint32(b[8:])),
		RefID:          binary.BigEndian.Uint32(b[12:]),
		Reference:      Timestamp(binary.BigEndian.Uint64(b[16:])),
		Origin:         Timestamp(binary.BigEndian.Uint64(b[24:])),
		Receive:        Timestamp(binary.BigEndian.Uint64(b[32:])),
		Transmit:       Timestamp(binary.BigEndian.Uint64(b[40:])),
	}, nil
}

// log2Duration converts an NTP log2-seconds exponent (poll, precision) to a
// duration, clamped to a sane range.
func log2Duration(exp int8) time.Duration {
	e := int(exp)
	if e > 36 {
		e = 36
	}
	if e < -40 {
		return 0
	}
	return time.Duration(math.Ldexp(1e9, e))
}

// FormatRefID renders a reference identifier. For stratum 0 (kiss code) and
// stratum 1 (reference clock name such as "GPS", "PPS", "DCF") the four bytes
// are ASCII text. For stratum 2 and above they are the IPv4 address of the
// upstream server (or, for an IPv6 upstream, the first four bytes of the MD5
// hash of its address) and are shown in dotted-quad form.
func FormatRefID(stratum int, id uint32) string {
	b := [4]byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	if stratum <= 1 {
		out := make([]byte, 0, 4)
		for _, c := range b {
			if c == 0 {
				break
			}
			if c < 0x20 || c > 0x7e {
				// Not printable: fall back to hex so nothing odd reaches the UI.
				return fmt.Sprintf("0x%08X", id)
			}
			out = append(out, c)
		}
		// Trim trailing padding spaces.
		for len(out) > 0 && out[len(out)-1] == ' ' {
			out = out[:len(out)-1]
		}
		return string(out)
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
}

// Now returns the host system (wall) clock with the best resolution the
// platform offers and without a monotonic reading. It is the clock every
// offset in this package refers to. On Linux it equals time.Now(); on Windows
// time.Now() only advances once per timer tick (0.5–15.6 ms), so Now reads
// GetSystemTimePreciseAsFileTime instead.
func Now() time.Time { return preciseNow() }

// offsetDelay applies the on-wire protocol equations (RFC 5905 §8):
//
//	t1 = client transmit (client clock)   t2 = server receive (server clock)
//	t3 = server transmit (server clock)   t4 = client receive (client clock)
//
//	offset = ((t2 - t1) + (t3 - t4)) / 2
//	delay  =  (t4 - t1) - (t3 - t2)
//
// The times must not carry monotonic clock readings (use Round(0)) so that
// all subtractions are wall-clock subtractions.
func offsetDelay(t1, t2, t3, t4 time.Time) (offset, delay time.Duration) {
	offset = (t2.Sub(t1) + t3.Sub(t4)) / 2
	delay = t4.Sub(t1) - t3.Sub(t2)
	return offset, delay
}
