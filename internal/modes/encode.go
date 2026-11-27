package modes

// Field encoders: the inverses of the field decoders. Together with
// CPREncode, Checksum and SetParity they are what a traffic simulator or a
// test needs to build valid frames.

// EncodeGillham returns the Mode C (Gillham) reply for a pressure altitude,
// in Mode A layout (one octal digit per hex nibble, see GillhamAltitude).
// The altitude is rounded to the nearest 100 ft. ok is false outside
// -1200..126700 ft.
func EncodeGillham(altFt int32) (modeA uint16, ok bool) {
	if altFt < -1250 || altFt > 126749 {
		return 0, false
	}
	// h = 500*N500 + 100*N100 - 1300 with N100 in 1..5.
	n := (altFt + 1250) / 100 // 0 for -1200 ft
	n500 := uint32(n / 5)
	n100 := n%5 + 1
	if n500&1 != 0 {
		n100 = 6 - n100 // the 100 ft digit runs backwards on odd 500 ft steps
	}
	// N500 as an 8-bit reflected Gray code on D2 D4 A1 A2 A4 B1 B2 B4.
	gray := n500 ^ n500>>1
	for i, mask := range [8]uint16{0x0002, 0x0004, 0x1000, 0x2000, 0x4000, 0x0100, 0x0200, 0x0400} {
		if gray&(0x80>>uint(i)) != 0 {
			modeA |= mask
		}
	}
	// N100 on C1 C2 C4: 1 = C4, 2 = C2+C4, 3 = C2, 4 = C1+C2, 5 = C1.
	modeA |= [6]uint16{0, 0x0040, 0x0060, 0x0020, 0x0030, 0x0010}[n100]
	return modeA, true
}

// EncodeID13 converts a Mode A code in hex-digit layout (0x7700, as held in
// Message.Squawk) or a Gillham code from EncodeGillham to the 13-bit field
// transmitted in DF5/21 (ID), DF0/4/16/20 (AC with Q=0) and the extended
// squitter status messages: C1 A1 C2 A2 C4 A4 X B1 D1 B2 D2 B4 D4.
// It is the inverse of the decoder used for Message.Squawk.
func EncodeID13(modeA uint16) uint16 {
	var f uint16
	for _, p := range [...][2]uint16{
		{0x0010, 0x1000}, {0x1000, 0x0800}, {0x0020, 0x0400}, {0x2000, 0x0200},
		{0x0040, 0x0100}, {0x4000, 0x0080}, {0x0100, 0x0020}, {0x0001, 0x0010},
		{0x0200, 0x0008}, {0x0002, 0x0004}, {0x0400, 0x0002}, {0x0004, 0x0001},
	} {
		if modeA&p[0] != 0 {
			f |= p[1]
		}
	}
	return f
}

// EncodeAC13 returns the 13-bit altitude code (AC field of DF0/4/16/20) of a
// barometric altitude in feet. Altitudes up to 50175 ft use the 25 ft
// encoding (Q=1), rounded to the nearest 25 ft; higher ones the 100 ft
// Gillham encoding. ok is false outside -1000..126700 ft.
func EncodeAC13(altFt int32) (code uint16, ok bool) {
	if altFt < -1012 {
		return 0, false
	}
	if altFt <= 50187 {
		n := uint16((altFt + 1012) / 25) // 11 bits
		if n > 0x7FF {
			n = 0x7FF
		}
		// N with the M bit (0) inserted after bit 5 and the Q bit (1) after bit 4.
		return (n&0x7E0)<<2 | (n&0x010)<<1 | 0x0010 | n&0x00F, true
	}
	g, ok := EncodeGillham(altFt)
	if !ok {
		return 0, false
	}
	return EncodeID13(g), true
}

// EncodeAC12 returns the 12-bit altitude field of an extended squitter
// airborne position (ME bits 9-20) for an altitude in feet: EncodeAC13
// without the M bit.
func EncodeAC12(altFt int32) (code uint16, ok bool) {
	ac, ok := EncodeAC13(altFt)
	if !ok {
		return 0, false
	}
	return (ac&0x1F80)>>1 | ac&0x003F, true
}

// EncodeCallsign packs an aircraft identification into the 48-bit field of
// an identification squitter (ME bits 9-56) or BDS 2,0 register (MB bits
// 9-56). The text is upper-cased and padded with spaces to eight characters.
// ok is false when it is longer than eight characters or contains anything
// but letters, digits and spaces.
func EncodeCallsign(s string) (v uint64, ok bool) {
	if len(s) > 8 {
		return 0, false
	}
	for i := 0; i < 8; i++ {
		c := byte(' ')
		if i < len(s) {
			c = s[i]
		}
		var code uint64
		switch {
		case c >= 'A' && c <= 'Z':
			code = uint64(c-'A') + 1
		case c >= 'a' && c <= 'z':
			code = uint64(c-'a') + 1
		case c >= '0' && c <= '9':
			code = uint64(c-'0') + 48
		case c == ' ':
			code = 32
		default:
			return 0, false
		}
		v = v<<6 | code
	}
	return v, true
}
