package refdata

// Registration derivation from the 24-bit address.
//
// Some states do not hand out addresses one by one but tie them arithmetically
// to the registration mark, so the mark can be computed from the address
// without any database. The mappings were worked out by the community from the
// allocation patterns; most of them are described publicly in the
// registrations.js of the dump1090 / tar1090 web interfaces (US, Japan, South
// Korea, France, Germany, Belgium, Denmark, Finland, Greece, Portugal, Romania,
// Turkey, Jordan, Pakistan, Singapore, Syria, Canada C-F / C-G, Argentina,
// Russia, Cuba).
//
// Every rule enabled here was additionally measured against the tar1090-db /
// Mictronics aircraft database (645,762 airframes, November 2026): of the
// database airframes whose address lies in a rule's range, at least 92 % (for
// most rules over 99 %, United States 99.9 %) carry exactly the registration
// the rule computes. The remainder is database noise, re-used addresses and
// state aircraft with a different mark (Russian RF-, Argentine LQ-). The rules that tar1090
// also has were compared address by address with its registrations.js: all
// 1.56 million derived marks are identical.
//
// Rules that scored poorly were dropped or narrowed:
//   - South Africa ZS-: 78 %, the pattern is no longer followed (tar1090
//     dropped it too).
//   - The military sub-ranges of Jordan (JY-M), Romania (YR-M), Singapore
//     (9V-X..Z), Pakistan (AP-D..Z) and Tunisia (TS-M..Z) are excluded.
//   - Swiss numeric marks stop at HB-5999: the addresses above are used by
//     airport ground vehicles.
//
// TestRegistrationAgainstDatabase repeats the measurement on demand.
//
// They are still heuristics. A computed mark exists for every address in a
// range, including addresses that were never issued, and a state can break its
// own pattern at any time. When the imported aircraft database has a record,
// prefer it.

// limitedAlphabet is A-Z without I and O (used by US and Japanese marks).
const limitedAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ"

// strideMap describes marks of the form PREFIX + three letters where
// address = base + c1*s1 + c2*s2 + c3 (c = 0..25 for A..Z).
type strideMap struct {
	lo, hi uint32 // inclusive address range the mapping applies to
	base   uint32 // address of suffix "AAA" (may be below lo)
	s1, s2 uint32
	prefix string
}

// stride builds a strideMap covering suffixes first..last ("" = AAA / ZZZ),
// where start is the address of the first suffix.
func stride(start, s1, s2 uint32, prefix, first, last string) strideMap {
	off := func(s string, def uint32) uint32 {
		if s == "" {
			return def
		}
		return uint32(s[0]-'A')*s1 + uint32(s[1]-'A')*s2 + uint32(s[2]-'A')
	}
	fo := off(first, 0)
	lo := off(last, 25*s1+25*s2+25)
	return strideMap{lo: start, hi: start - fo + lo, base: start - fo, s1: s1, s2: s2, prefix: prefix}
}

// strideMaps is sorted by lo and non-overlapping (checked by the tests).
var strideMaps = []strideMap{
	stride(0x028000, 1024, 32, "TS-", "", "LZZ"),  // Tunisia TS-A..L; M..Z is largely military
	stride(0x380000, 1024, 32, "F-B", "", ""),     // France
	stride(0x388000, 1024, 32, "F-I", "", ""),     //
	stride(0x390000, 1024, 32, "F-G", "", ""),     //
	stride(0x398000, 1024, 32, "F-H", "", ""),     //
	stride(0x3A0000, 1024, 32, "F-O", "", ""),     // French overseas
	stride(0x3C0001, 26*26, 26, "D-A", "PAA", ""), // Germany D-APAA .. D-AZZZ
	stride(0x3C2001, 26*26, 26, "D-B", "PAA", ""), //         D-BPAA .. D-BZZZ
	stride(0x3C4421, 1024, 32, "D-A", "", "OZZ"),  //         D-AAAA .. D-AOZZ
	stride(0x3C8421, 1024, 32, "D-B", "", "OZZ"),  //         D-BAAA .. D-BOZZ
	stride(0x3CC000, 26*26, 26, "D-C", "", ""),    //
	stride(0x3D04A8, 26*26, 26, "D-E", "", ""),    //
	stride(0x3D4950, 26*26, 26, "D-F", "", ""),    //
	stride(0x3D8DF8, 26*26, 26, "D-G", "", ""),    //
	stride(0x3DD2A0, 26*26, 26, "D-H", "", ""),    //
	stride(0x3E1748, 26*26, 26, "D-I", "", ""),    //
	stride(0x448421, 1024, 32, "OO-", "", ""),     // Belgium
	stride(0x458421, 1024, 32, "OY-", "", ""),     // Denmark
	stride(0x460000, 26*26, 26, "OH-", "", ""),    // Finland
	stride(0x468421, 1024, 32, "SX-", "", ""),     // Greece
	stride(0x470000, 26*26, 26, "HA-", "", ""),    // Hungary
	stride(0x490421, 1024, 32, "CS-", "", ""),     // Portugal
	stride(0x4A0421, 1024, 32, "YR-", "", "LZZ"),  // Romania YR-A..L
	stride(0x4A3821, 1024, 32, "YR-", "NAA", ""),  //         YR-N..Z; YR-M is military
	stride(0x4A8421, 1024, 32, "SE-", "", ""),     // Sweden
	stride(0x4B0000, 26*26, 26, "HB-", "", ""),    // Switzerland and Liechtenstein
	stride(0x4B8421, 1024, 32, "TC-", "", ""),     // Turkey
	stride(0x4C0000, 1024, 32, "YU-", "", ""),     // Serbia
	stride(0x728421, 1024, 32, "YI-", "", ""),     // Iraq
	stride(0x730421, 1024, 32, "EP-", "", ""),     // Iran
	stride(0x740421, 1024, 32, "JY-", "", "LZZ"),  // Jordan JY-A..L
	stride(0x743821, 1024, 32, "JY-", "NAA", ""),  //        JY-N..Z; JY-M is military
	stride(0x760421, 1024, 32, "AP-", "", "CZZ"),  // Pakistan AP-A..C; later letters are mostly military
	stride(0x768421, 1024, 32, "9V-", "", "WZZ"),  // Singapore 9V-A..W; X..Z is military / ground
	stride(0x778421, 1024, 32, "YK-", "", ""),     // Syria
	stride(0x7C0000, 36*36, 36, "VH-", "", ""),    // Australia
	stride(0x880421, 1024, 32, "HS-", "", ""),     // Thailand
	stride(0xC00001, 26*26, 26, "C-F", "", ""),    // Canada
	stride(0xC044A9, 26*26, 26, "C-G", "", ""),    //
	stride(0xC08951, 26*26, 26, "C-I", "", ""),    // Canada, ultralights and amateur-built
	stride(0xE01041, 4096, 64, "LV-", "", ""),     // Argentina
}

// numericMap describes marks of the form PREFIX + decimal number where
// number = address - base.
type numericMap struct {
	lo, hi uint32 // inclusive address range
	base   uint32
	prefix string
	width  int // zero-pad the number to this many digits; 0 = no padding
}

// numericMaps is sorted by lo and non-overlapping (checked by the tests).
var numericMaps = []numericMap{
	{0x0B03E8, 0x0B07CF, 0x0B0000, "CU-T", 4}, // Cuba CU-T1000 .. CU-T1999
	{0x140000, 0x15869F, 0x140000, "RA-", 5},  // Russia RA-00000 .. RA-99999
	{0x3EE098, 0x3F07A6, 0x3EE097, "D-", 4},   // Germany gliders D-0001 .. D-9999
	{0x464831, 0x464C17, 0x464830, "OH-U", 3}, // Finland ultralights OH-U001 .. OH-U999
	{0x4B44A8, 0x4B5C16, 0x4B44A7, "HB-", 0},  // Switzerland gliders / balloons HB-1 .. HB-5999
}

// Address ranges of the rules that have their own code.
const (
	usFirst, usLast = 0xA00001, 0xADF7C7 // N1 .. N99999
	jaFirst, jaLast = 0x840000, 0x840000 + 229840 - 1
	hlFirst, hlLast = 0x71BA00, 0x71C299
)

// Registration returns the registration mark that the address encodes for the
// states that allocate addresses arithmetically (United States, Canada,
// Germany, France, Switzerland, Belgium, Denmark, Finland, Sweden, Greece,
// Portugal, Romania, Hungary, Serbia, Turkey, Russia, Japan, South Korea,
// Australia, Singapore, Pakistan, Thailand, Jordan, Iran, Iraq, Syria,
// Tunisia, Argentina, Cuba), or "" when no rule applies.
//
// The result is a heuristic: see the comment at the top of this file.
// Addresses inside a military block never yield a registration.
// It performs at most one small allocation (the returned string).
func Registration(icao uint32) string {
	var buf [12]byte
	b := AppendRegistration(buf[:0], icao)
	if len(b) == 0 {
		return ""
	}
	return string(b)
}

// AppendRegistration appends the derived registration (see Registration) to
// dst and returns the extended slice; dst is returned unchanged when no rule
// applies. It does not allocate when dst has room for 8 more bytes.
func AppendRegistration(dst []byte, icao uint32) []byte {
	if icao == 0 || icao > 0xFFFFFF || IsMilitary(icao) {
		return dst
	}
	switch {
	case icao >= usFirst && icao <= usLast:
		return appendNNumber(dst, icao-usFirst)
	case icao >= jaFirst && icao <= jaLast:
		return appendJA(dst, icao-jaFirst)
	case icao >= hlFirst && icao <= hlLast:
		return appendHL(dst, icao)
	}
	// Numeric marks.
	lo, hi := 0, len(numericMaps)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if numericMaps[mid].hi < icao {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(numericMaps) && numericMaps[lo].lo <= icao {
		m := &numericMaps[lo]
		dst = append(dst, m.prefix...)
		return appendDecimal(dst, icao-m.base, m.width)
	}
	// Three-letter suffix marks.
	if m := findStride(icao); m != nil {
		off := icao - m.base
		i1 := off / m.s1
		r := off % m.s1
		i2 := r / m.s2
		i3 := r % m.s2
		if i1 < 26 && i2 < 26 && i3 < 26 {
			dst = append(dst, m.prefix...)
			return append(dst, byte('A'+i1), byte('A'+i2), byte('A'+i3))
		}
	}
	return dst
}

// findStride returns the stride mapping whose range contains icao, or nil.
func findStride(icao uint32) *strideMap {
	lo, hi := 0, len(strideMaps)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if strideMaps[mid].hi < icao {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(strideMaps) && strideMaps[lo].lo <= icao {
		return &strideMaps[lo]
	}
	return nil
}

// appendDecimal appends v in decimal, zero-padded to width digits.
func appendDecimal(dst []byte, v uint32, width int) []byte {
	var tmp [10]byte
	i := len(tmp)
	for {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	for len(tmp)-i < width {
		i--
		tmp[i] = '0'
	}
	return append(dst, tmp[i:]...)
}

// US N-numbers. The civil block A00001..ADF7C7 enumerates every valid mark in
// lexical order: N1, N1A, N1AA, N1AB .. N1ZZ, N10, N10A .. N99999.
//
// Bucket sizes: a digit position that can still be followed by 3 / 2 / 1 / 0
// more characters spans 101711 / 10111 / 951 / 35 addresses; the first 601 of
// a bucket (25 of the last one) are the letter suffixes.
const (
	nBucket1 = 101711 // per first digit
	nBucket2 = 10111
	nBucket3 = 951
	nBucket4 = 35
	nLetters = 601 // "" + 24 single letters + 24*24 letter pairs
)

func appendNNumber(dst []byte, off uint32) []byte {
	dst = append(dst, 'N', byte('1'+off/nBucket1))
	r := off % nBucket1
	if r < nLetters {
		return appendNLetters(dst, r)
	}
	r -= nLetters
	dst = append(dst, byte('0'+r/nBucket2))
	r %= nBucket2
	if r < nLetters {
		return appendNLetters(dst, r)
	}
	r -= nLetters
	dst = append(dst, byte('0'+r/nBucket3))
	r %= nBucket3
	if r < nLetters {
		return appendNLetters(dst, r)
	}
	r -= nLetters
	dst = append(dst, byte('0'+r/nBucket4))
	r %= nBucket4
	if r < 25 {
		if r > 0 {
			dst = append(dst, limitedAlphabet[r-1])
		}
		return dst
	}
	return append(dst, byte('0'+r-25))
}

// appendNLetters appends the 0, 1 or 2 letter suffix number r (0..600).
func appendNLetters(dst []byte, r uint32) []byte {
	if r == 0 {
		return dst
	}
	r--
	dst = append(dst, limitedAlphabet[r/25])
	if r%25 > 0 {
		dst = append(dst, limitedAlphabet[r%25-1])
	}
	return dst
}

// Japan: JA + four characters: dddd, dddL or ddLL (letters without I and O).
func appendJA(dst []byte, off uint32) []byte {
	d1 := off / 22984
	r := off % 22984
	d2 := r / 916
	r %= 916
	if d1 > 9 || d2 > 9 {
		return dst
	}
	dst = append(dst, 'J', 'A', byte('0'+d1), byte('0'+d2))
	if r < 340 {
		dst = append(dst, byte('0'+r/34))
		r %= 34
		if r < 10 {
			return append(dst, byte('0'+r))
		}
		return append(dst, limitedAlphabet[r-10])
	}
	r -= 340
	return append(dst, limitedAlphabet[r/24], limitedAlphabet[r%24])
}

// South Korea: HL + four digits, where the digits are the hexadecimal nibbles
// of the address offset (binary-coded decimal).
func appendHL(dst []byte, icao uint32) []byte {
	var v uint32
	switch {
	case icao >= 0x71BA00 && icao <= 0x71BF99:
		v = icao - 0x71BA00 + 0x7200
	case icao >= 0x71C000 && icao <= 0x71C099:
		v = icao - 0x71C000 + 0x8000
	case icao >= 0x71C200 && icao <= 0x71C299:
		v = icao - 0x71C200 + 0x8200
	default:
		return dst
	}
	n := len(dst)
	dst = append(dst, 'H', 'L')
	for shift := 12; shift >= 0; shift -= 4 {
		d := (v >> uint(shift)) & 0xF
		if d > 9 {
			return dst[:n]
		}
		dst = append(dst, byte('0'+d))
	}
	return dst
}
