package group

import (
	"math"

	"airview/internal/modes"
)

// frameAltitude extracts the altitude a Mode S frame carries itself:
// the 13-bit AC field of DF0/4/16/20 (bits 20-32) or the 12-bit altitude of an
// extended squitter airborne position (DF17/18, TC 9-18 barometric, TC 20-22
// GNSS height). It returns the altitude in feet and its source, or
// (NaN, AltNone).
func frameAltitude(data []byte) (ft float64, src uint8) {
	if len(data) < 7 {
		return math.NaN(), AltNone
	}
	switch df := data[0] >> 3; df {
	case 0, 4, 16, 20:
		ac := uint16(data[2]&0x1F)<<8 | uint16(data[3])
		if v, ok := decodeAC13(ac); ok {
			return float64(v), AltFrameBaro
		}
	case 17, 18:
		if len(data) < 14 {
			break
		}
		if df == 18 && data[0]&7 > 1 { // CF >= 2: TIS-B / ADS-R, not the transmitter's own report
			break
		}
		tc := data[4] >> 3
		if (tc < 9 || tc > 18) && (tc < 20 || tc > 22) {
			break
		}
		// The 12-bit field is the AC13 code without the M bit; TC 20-22 carry
		// the GNSS height (HAE) in the same coding (as internal/modes reads it).
		ac12 := uint16(data[5])<<4 | uint16(data[6]>>4)
		ac := (ac12&0x0FC0)<<1 | ac12&0x003F
		if v, ok := decodeAC13(ac); ok {
			if tc >= 20 {
				return float64(v), AltFrameGNSS
			}
			return float64(v), AltFrameBaro
		}
	}
	return math.NaN(), AltNone
}

// decodeAC13 decodes a 13-bit AC altitude code (bit 0x0040 = M, 0x0010 = Q):
// Q=1 gives 25 ft steps from −1000 ft, Q=0 the Gillham (Gray) code in 100 ft
// steps. The metric (M=1) coding is reserved and returns false, as does an
// all-zero field (altitude not available).
func decodeAC13(f uint16) (int32, bool) {
	f &= 0x1FFF
	if f == 0 || f&0x0040 != 0 {
		return 0, false
	}
	if f&0x0010 != 0 {
		n := int32(f&0x1F80)>>2 | int32(f&0x0020)>>1 | int32(f&0x000F)
		return n*25 - 1000, true
	}
	return modes.GillhamAltitude(gillhamOrder(f))
}

// gillhamOrder rearranges the field bits C1 A1 C2 A2 C4 A4 (M) B1 D1 B2 D2 B4
// D4 (most significant first) into the octal-digit layout 0xABCD of a Mode A
// code (each hex digit holds one octal digit, bit 4-2-1) expected by
// modes.GillhamAltitude.
func gillhamOrder(f uint16) uint16 {
	type pair struct{ from, to uint16 }
	var g uint16
	for _, p := range [...]pair{
		{0x1000, 0x0010}, // C1
		{0x0800, 0x1000}, // A1
		{0x0400, 0x0020}, // C2
		{0x0200, 0x2000}, // A2
		{0x0100, 0x0040}, // C4
		{0x0080, 0x4000}, // A4
		{0x0020, 0x0100}, // B1
		{0x0010, 0x0001}, // D1
		{0x0008, 0x0200}, // B2
		{0x0004, 0x0002}, // D2
		{0x0002, 0x0400}, // B4
		{0x0001, 0x0004}, // D4
	} {
		if f&p.from != 0 {
			g |= p.to
		}
	}
	return g
}
