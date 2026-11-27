package track

import "fmt"

// Mode A/C reply codes (DESIGN §9.9). A Mode A/C reply carries 12 code
// pulses between the framing pulses F1 and F2, plus the unused X position:
//
//	F1 C1 A1 C2 A2 C4 A4 X B1 D1 B2 D2 B4 D4 F2 (SPI)
//
// The raw code used throughout internal/mlat is these 13 positions in
// transmission order, C1 in bit 12 down to D4 in bit 0 (X in bit 6). A reply
// to a Mode A interrogation carries the 4-digit octal identity (squawk:
// digit A = A4 A2 A1, B = B4 B2 B1, C = C4 C2 C1, D = D4 D2 D1); a reply to a
// Mode C interrogation carries the pressure altitude in the Gillham code
// (ICAO Annex 10 Vol. IV, 100 ft increments). The reply itself does not say
// which interrogation it answered, so a target's codes are classified by the
// tracker (ACTracker) from their behaviour. Written from the published
// standard; no third-party code consulted.

// ACCodeMask clears the X position, which carries no information.
const ACCodeMask uint16 = 0x1FBF

// Bit positions of the raw code.
const (
	acD4 = 1 << iota
	acB4
	acD2
	acB2
	acD1
	acB1
	acX
	acA4
	acC4
	acA2
	acC2
	acA1
	acC1
)

// ACSquawk returns the 4-digit octal identity of a raw code as a number
// whose octal digits are the squawk (e.g. 0o7000 for squawk 7000).
func ACSquawk(raw uint16) uint16 {
	b := func(m uint16) uint16 {
		if raw&m != 0 {
			return 1
		}
		return 0
	}
	a := b(acA4)<<2 | b(acA2)<<1 | b(acA1)
	bb := b(acB4)<<2 | b(acB2)<<1 | b(acB1)
	c := b(acC4)<<2 | b(acC2)<<1 | b(acC1)
	d := b(acD4)<<2 | b(acD2)<<1 | b(acD1)
	return a<<9 | bb<<6 | c<<3 | d
}

// ACSquawkString returns the squawk of a raw code as 4 octal digits.
func ACSquawkString(raw uint16) string { return fmt.Sprintf("%04o", ACSquawk(raw)) }

// ACFromSquawk returns the raw code of a squawk given as octal digits
// (0o0000..0o7777).
func ACFromSquawk(sq uint16) uint16 {
	var raw uint16
	set := func(v uint16, m uint16) {
		if v != 0 {
			raw |= m
		}
	}
	a, b, c, d := sq>>9&7, sq>>6&7, sq>>3&7, sq&7
	set(a&4, acA4)
	set(a&2, acA2)
	set(a&1, acA1)
	set(b&4, acB4)
	set(b&2, acB2)
	set(b&1, acB1)
	set(c&4, acC4)
	set(c&2, acC2)
	set(c&1, acC1)
	set(d&4, acD4)
	set(d&2, acD2)
	set(d&1, acD1)
	return raw
}

func gray2bin(g uint16) uint16 {
	b := g
	for s := g >> 1; s != 0; s >>= 1 {
		b ^= s
	}
	return b
}

func bin2gray(b uint16) uint16 { return b ^ b>>1 }

// ACAltitudeFt decodes a raw code as a Gillham (Mode C) pressure altitude in
// feet (−1200 .. 126 700 in steps of 100). ok is false for codes that are not
// valid Gillham altitudes (the 100 ft group C1 C2 C4 must be one of its five
// legal values). Note that most squawks are also valid Gillham codes: only
// the tracker can tell which of a target's codes is its altitude.
func ACAltitudeFt(raw uint16) (ft int, ok bool) {
	bit := func(m uint16) uint16 {
		if raw&m != 0 {
			return 1
		}
		return 0
	}
	g500 := bit(acD1)<<8 | bit(acD2)<<7 | bit(acD4)<<6 | bit(acA1)<<5 | bit(acA2)<<4 | bit(acA4)<<3 |
		bit(acB1)<<2 | bit(acB2)<<1 | bit(acB4)
	g100 := bit(acC1)<<2 | bit(acC2)<<1 | bit(acC4)
	n500 := int(gray2bin(g500))
	n100 := int(gray2bin(g100))
	switch n100 {
	case 1, 2, 3, 4:
	case 7:
		n100 = 5
	default:
		return 0, false
	}
	if n500&1 != 0 {
		n100 = 6 - n100
	}
	return n500*500 + n100*100 - 1300, true
}

// ACFromAltitudeFt encodes a pressure altitude (rounded to the nearest
// 100 ft) as a raw Gillham code. ok is false outside −1200 .. 126 700 ft.
func ACFromAltitudeFt(ft int) (raw uint16, ok bool) {
	q := ft + 1300
	if q >= 0 {
		q = (q + 50) / 100
	} else {
		q = -((-q + 50) / 100)
	}
	if q < 1 || q > 5*255+5 { // D1 is not used: 126 700 ft is the top
		return 0, false
	}
	n500 := (q - 1) / 5
	n100 := q - 5*n500
	if n500&1 != 0 {
		n100 = 6 - n100
	}
	v := uint16(n100)
	if v == 5 {
		v = 7
	}
	g100 := bin2gray(v)
	g500 := bin2gray(uint16(n500))
	set := func(cond uint16, m uint16) {
		if cond != 0 {
			raw |= m
		}
	}
	set(g500&0x100, acD1)
	set(g500&0x080, acD2)
	set(g500&0x040, acD4)
	set(g500&0x020, acA1)
	set(g500&0x010, acA2)
	set(g500&0x008, acA4)
	set(g500&0x004, acB1)
	set(g500&0x002, acB2)
	set(g500&0x001, acB4)
	set(g100&4, acC1)
	set(g100&2, acC2)
	set(g100&1, acC4)
	return raw, true
}

// ACRawFromNibbles converts a code written as four octal digits in the four
// nibbles of a 16-bit value (0xABCD, each digit 0-7: the "squawk as hex"
// form some receivers emit for Mode A/C payloads) to the raw pulse layout.
// ok is false when a nibble is above 7.
func ACRawFromNibbles(v uint16) (raw uint16, ok bool) {
	if v&0x8888 != 0 {
		return 0, false
	}
	return ACFromSquawk(v>>12&7<<9 | v>>8&7<<6 | v>>4&7<<3 | v&7), true
}

// ACNibblesFromRaw is the inverse of ACRawFromNibbles.
func ACNibblesFromRaw(raw uint16) uint16 {
	s := ACSquawk(raw)
	return s>>9&7<<12 | s>>6&7<<8 | s>>3&7<<4 | s&7
}
