package modes

import (
	"errors"
)

// NonICAOFlag is set in address keys (Message.AddrKey, ParseICAO with a "~"
// prefix) for addresses that are not ICAO aircraft addresses, so that they
// cannot collide with real ones.
const NonICAOFlag = 1 << 24

const hexDigits = "0123456789ABCDEF"

// FormatICAO formats a 24-bit address as six upper-case hex digits. An
// address key carrying NonICAOFlag is prefixed with "~".
func FormatICAO(addr uint32) string {
	var buf [7]byte
	return string(AppendICAO(buf[:0], addr))
}

// AppendICAO appends the FormatICAO representation of addr to dst.
func AppendICAO(dst []byte, addr uint32) []byte {
	if addr&NonICAOFlag != 0 {
		dst = append(dst, '~')
	}
	for shift := 20; shift >= 0; shift -= 4 {
		dst = append(dst, hexDigits[(addr>>uint(shift))&15])
	}
	return dst
}

// ErrBadICAO is returned by ParseICAO for malformed input.
var ErrBadICAO = errors.New("modes: ICAO address must be six hex digits")

// ParseICAO parses six hex digits (either case) into a 24-bit address. A
// leading "~" marks a non-ICAO address and sets NonICAOFlag in the result.
func ParseICAO(s string) (uint32, error) {
	var flag uint32
	if len(s) == 7 && s[0] == '~' {
		flag = NonICAOFlag
		s = s[1:]
	}
	if len(s) != 6 {
		return 0, ErrBadICAO
	}
	var v uint32
	for i := 0; i < 6; i++ {
		d, ok := hexVal(s[i])
		if !ok {
			return 0, ErrBadICAO
		}
		v = v<<4 | uint32(d)
	}
	return v | flag, nil
}

// ICAOString returns the message address as six upper-case hex digits.
func (m *Message) ICAOString() string { return FormatICAO(m.ICAO) }

// CallsignString returns the callsign without trailing spaces, or "" when
// the message carries none.
func (m *Message) CallsignString() string {
	if m.Valid&FCallsign == 0 {
		return ""
	}
	return TrimCallsign(m.Callsign)
}

// TrimCallsign converts a space padded 8 character callsign to a string
// without trailing spaces.
func TrimCallsign(cs [8]byte) string {
	n := 8
	for n > 0 && (cs[n-1] == ' ' || cs[n-1] == 0) {
		n--
	}
	return string(cs[:n])
}

// FormatSquawk formats a Mode A code held as one octal digit per hex nibble
// (0x7500) as four digits.
func FormatSquawk(squawk uint16) string {
	var buf [4]byte
	return string(AppendSquawk(buf[:0], squawk))
}

// AppendSquawk appends the four digit representation of squawk to dst.
func AppendSquawk(dst []byte, squawk uint16) []byte {
	return append(dst,
		'0'+byte(squawk>>12&7), '0'+byte(squawk>>8&7), '0'+byte(squawk>>4&7), '0'+byte(squawk&7))
}

// SquawkString returns the Mode A code as four octal digits, or "" when the
// message carries none.
func (m *Message) SquawkString() string {
	if m.Valid&FSquawk == 0 {
		return ""
	}
	return FormatSquawk(m.Squawk)
}

var categoryNames = buildCategoryNames()

func buildCategoryNames() (t [4][8]string) {
	for set := 0; set < 4; set++ {
		for n := 0; n < 8; n++ {
			t[set][n] = string([]byte{'A' + byte(set), '0' + byte(n)})
		}
	}
	return t
}

// CategoryName formats an emitter category byte (0xA3) as "A3". It returns ""
// for values outside A0..D7.
func CategoryName(category uint8) string {
	set := int(category>>4) - 0xA
	if set < 0 || set > 3 || category&0x08 != 0 {
		return ""
	}
	return categoryNames[set][category&7]
}

// CategoryString returns the emitter category ("A3"), or "" when the message
// carries none.
func (m *Message) CategoryString() string {
	if m.Valid&FCategory == 0 {
		return ""
	}
	return CategoryName(m.Category)
}

var emergencyNames = [8]string{"", "general", "medical", "minfuel", "nordo", "unlawful", "downed", ""}

// EmergencyName maps an emergency/priority status (Message.Emergency) to the
// values of model.Aircraft.Emergency: "", "general", "medical", "minfuel",
// "nordo", "unlawful", "downed". Unknown and reserved values give "".
func EmergencyName(state uint8) string {
	if state > 7 {
		return ""
	}
	return emergencyNames[state]
}

// SquawkEmergency maps the emergency Mode A codes to model.Aircraft.Emergency
// values: 7500 "unlawful", 7600 "nordo", 7700 "general", anything else "".
func SquawkEmergency(squawk uint16) string {
	switch squawk {
	case 0x7500:
		return "unlawful"
	case 0x7600:
		return "nordo"
	case 0x7700:
		return "general"
	}
	return ""
}

// EmergencyString returns the model.Aircraft.Emergency value implied by the
// message: its emergency/priority status if present and non-zero, otherwise
// an emergency squawk, otherwise "".
func (m *Message) EmergencyString() string {
	if m.Valid&FEmergency != 0 {
		if s := EmergencyName(m.Emergency); s != "" {
			return s
		}
	}
	if m.Valid&FSquawk != 0 {
		return SquawkEmergency(m.Squawk)
	}
	return ""
}

// DFName returns a human readable name of a downlink format.
func DFName(df uint8) string {
	switch df {
	case 0:
		return "Short air-air surveillance (ACAS)"
	case 4:
		return "Surveillance, altitude reply"
	case 5:
		return "Surveillance, identity reply"
	case 11:
		return "All-call reply"
	case 16:
		return "Long air-air surveillance (ACAS)"
	case 17:
		return "Extended squitter"
	case 18:
		return "Extended squitter, non-transponder"
	case 19:
		return "Military extended squitter"
	case 20:
		return "Comm-B, altitude reply"
	case 21:
		return "Comm-B, identity reply"
	}
	if df >= 24 && df <= 31 {
		return "Comm-D extended length message"
	}
	return "Unassigned"
}

// FrameDF returns the downlink format of a raw frame without decoding it
// (DF24..31 are reported as 24). It returns 0xFF for an empty frame.
func FrameDF(frame []byte) uint8 {
	if len(frame) == 0 {
		return 0xFF
	}
	df := frame[0] >> 3
	if df >= 24 {
		df = 24
	}
	return df
}

// FrameLen returns the frame length in bytes implied by a downlink format:
// 7 for DF0..15, 14 for DF16..31.
func FrameLen(df uint8) int {
	if df >= 16 {
		return 14
	}
	return 7
}

// Stable per-downlink-format indexes for statistics counters. The order
// matches the df* columns of the stats tables.
const (
	DFIndex0 = iota
	DFIndex4
	DFIndex5
	DFIndex11
	DFIndex16
	DFIndex17
	DFIndex18
	DFIndex20
	DFIndex21
	DFIndexOther
	// NumDFIndex is the number of counters needed.
	NumDFIndex
)

var dfIndexLabels = [NumDFIndex]string{
	"df0", "df4", "df5", "df11", "df16", "df17", "df18", "df20", "df21", "df_other",
}

// DFIndex maps a downlink format to its statistics counter index
// (0..NumDFIndex-1). Formats without their own counter share DFIndexOther.
func DFIndex(df uint8) int {
	switch df {
	case 0:
		return DFIndex0
	case 4:
		return DFIndex4
	case 5:
		return DFIndex5
	case 11:
		return DFIndex11
	case 16:
		return DFIndex16
	case 17:
		return DFIndex17
	case 18:
		return DFIndex18
	case 20:
		return DFIndex20
	case 21:
		return DFIndex21
	}
	return DFIndexOther
}

// DFIndexLabel returns the column-style label ("df17", "df_other") of a
// statistics counter index, or "" when out of range.
func DFIndexLabel(i int) string {
	if i < 0 || i >= NumDFIndex {
		return ""
	}
	return dfIndexLabels[i]
}
