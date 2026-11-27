package modes

// altKind describes how a 13-bit altitude code was encoded.
type altKind uint8

const (
	altNone    altKind = iota // all zero: altitude not available
	altQ25                    // Q=1: 25 ft increments
	altGillham                // Q=0: Gillham (Gray) code, 100 ft increments
	altMetric                 // M=1: metric encoding (reserved by Annex 10)
	altInvalid                // illegal Gillham code
)

// decodeAC13 decodes the 13-bit AC altitude code of DF0/4/16/20 (frame bits
// 20-32). Bit 0x0040 is the M bit, bit 0x0010 the Q bit.
//
// For altMetric the returned value is the raw 12-bit number with the M bit
// removed; Annex 10 reserves this encoding without defining a scale.
func decodeAC13(f uint16) (v int32, kind altKind) {
	f &= 0x1FFF
	if f == 0 {
		return 0, altNone
	}
	if f&0x0040 != 0 {
		return int32(((f & 0x1F80) >> 1) | (f & 0x003F)), altMetric
	}
	if f&0x0010 != 0 {
		n := int32(((f & 0x1F80) >> 2) | ((f & 0x0020) >> 1) | (f & 0x000F))
		return n*25 - 1000, altQ25
	}
	ft, ok := GillhamAltitude(decodeID13(f))
	if !ok {
		return 0, altInvalid
	}
	return ft, altGillham
}

// decodeAC12 decodes the 12-bit altitude field of an extended squitter
// airborne position (ME bits 9-20), which is the AC13 code without the M bit.
func decodeAC12(f uint16) (v int32, kind altKind) {
	f &= 0x0FFF
	return decodeAC13(((f & 0x0FC0) << 1) | (f & 0x003F))
}

// decodeID13 rearranges the 13-bit identity / Gillham field
// (C1 A1 C2 A2 C4 A4 X B1 D1 B2 D2 B4 D4, most significant first) into the
// conventional Mode A layout where each hex digit is one octal squawk digit:
// 0xABCD with A4 A2 A1 | B4 B2 B1 | C4 C2 C1 | D4 D2 D1.
func decodeID13(f uint16) uint16 {
	var g uint16
	if f&0x1000 != 0 { // C1
		g |= 0x0010
	}
	if f&0x0800 != 0 { // A1
		g |= 0x1000
	}
	if f&0x0400 != 0 { // C2
		g |= 0x0020
	}
	if f&0x0200 != 0 { // A2
		g |= 0x2000
	}
	if f&0x0100 != 0 { // C4
		g |= 0x0040
	}
	if f&0x0080 != 0 { // A4
		g |= 0x4000
	}
	// 0x0040 is the X (Mode A) or M (altitude) bit and is not part of the code.
	if f&0x0020 != 0 { // B1
		g |= 0x0100
	}
	if f&0x0010 != 0 { // D1 (or Q)
		g |= 0x0001
	}
	if f&0x0008 != 0 { // B2
		g |= 0x0200
	}
	if f&0x0004 != 0 { // D2
		g |= 0x0002
	}
	if f&0x0002 != 0 { // B4
		g |= 0x0400
	}
	if f&0x0001 != 0 { // D4
		g |= 0x0004
	}
	return g
}

// GillhamAltitude converts a Mode C reply in Mode A layout (hex digits =
// octal digits ABCD, as produced for squawks) to a pressure altitude in feet.
// It also serves raw Mode A/C receivers. ok is false for codes that are not a
// legal Gillham altitude.
func GillhamAltitude(modeA uint16) (ft int32, ok bool) {
	// D1 is never used for altitude; C1..C4 can never all be zero.
	if modeA&0x8889 != 0 || modeA&0x00F0 == 0 {
		return 0, false
	}
	var one, five uint32
	if modeA&0x0010 != 0 { // C1
		one ^= 0x007
	}
	if modeA&0x0020 != 0 { // C2
		one ^= 0x003
	}
	if modeA&0x0040 != 0 { // C4
		one ^= 0x001
	}
	// Remove 7s from the 100 ft digit (7 -> 5, 5 -> 7).
	if one&5 == 5 {
		one ^= 2
	}
	if one > 5 {
		return 0, false
	}
	if modeA&0x0002 != 0 { // D2
		five ^= 0x0FF
	}
	if modeA&0x0004 != 0 { // D4
		five ^= 0x07F
	}
	if modeA&0x1000 != 0 { // A1
		five ^= 0x03F
	}
	if modeA&0x2000 != 0 { // A2
		five ^= 0x01F
	}
	if modeA&0x4000 != 0 { // A4
		five ^= 0x00F
	}
	if modeA&0x0100 != 0 { // B1
		five ^= 0x007
	}
	if modeA&0x0200 != 0 { // B2
		five ^= 0x003
	}
	if modeA&0x0400 != 0 { // B4
		five ^= 0x001
	}
	// The 100 ft digit runs backwards on every other 500 ft step.
	if five&1 != 0 {
		one = 6 - one
	}
	n := int32(five*5+one) - 13
	if n < -12 {
		return 0, false
	}
	return n * 100, true
}
