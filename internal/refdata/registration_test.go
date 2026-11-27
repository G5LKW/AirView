package refdata

import (
	"strings"
	"testing"
)

// Real airframes: address and registration as recorded in the tar1090-db
// aircraft database (November 2026), several per rule.
func TestRegistrationRealAircraft(t *testing.T) {
	tests := []struct {
		icao uint32
		reg  string
	}{
		// United States.
		{0xA00001, "N1"},     // FAA Citation
		{0xA00002, "N1A"},    // Goodyear airship
		{0xA000E8, "N1KE"},   // Nike Gulfstream
		{0xA004B3, "N100"},   //
		{0xA061D9, "N12345"}, //
		{0xA12239, "N172SP"}, //
		{0xA44360, "N374HK"}, //
		{0xA835AF, "N628TS"}, //
		{0xAAAAAA, "N787BK"}, //
		{0xAB1644, "N813UA"}, // United A319
		{0xAC82EC, "N905NA"}, // NASA 747
		{0xACC006, "N920VA"}, //
		{0xACC007, "N920VB"}, //
		{0xA8F1C3, "N6758H"}, //
		{0xADF7C7, "N99999"}, // last civil address
		// Canada.
		{0xC0172E, "C-FIUF"}, // Air Canada 777
		{0xC0440B, "C-FZTY"},
		{0xC06156, "C-GKWJ"},
		{0xC07C7A, "C-GVDP"}, // Air Canada A220
		{0xC08951, "C-IAAA"},
		// Germany.
		{0x3C6444, "D-AIBD"}, // Lufthansa A319
		{0x3C6589, "D-AILI"},
		{0x3C4B26, "D-ABYF"}, // Lufthansa 747-8
		{0x3C60B2, "D-AHER"},
		{0x3CC000, "D-CAAA"},
		{0x3D2B9C, "D-EOTO"},
		{0x3D66C6, "D-FLEC"},
		{0x3E0F90, "D-HXCA"},
		{0x3E1748, "D-IAAA"},
		{0x3EE0BC, "D-0037"}, // gliders
		{0x3EE9D8, "D-2369"},
		{0x3EFBCA, "D-6963"},
		// France.
		{0x39BDA4, "F-HPNE"}, // Air France A220
		// Switzerland.
		{0x4B17E5, "HB-JBH"}, // Swiss A220
		{0x4B1801, "HB-JCJ"},
		{0x4B4B8D, "HB-1766"}, // glider
		// Belgium, Denmark, Finland, Greece, Hungary, Portugal, Romania,
		// Sweden, Turkey, Serbia.
		{0x44CDC1, "OO-SNA"},
		{0x44CE61, "OO-SSA"},
		{0x45AC45, "OY-KBE"},
		{0x461F36, "OH-LVI"},
		{0x461E14, "OH-LKE"},
		{0x4649CB, "OH-U411"}, // ultralight
		{0x464AD2, "OH-U674"},
		{0x46B8A4, "SX-NED"},
		{0x4692C7, "SX-DVG"},
		{0x471F62, "HA-LXA"},
		{0x471F82, "HA-LYG"},
		{0x4952A6, "CS-TUF"},
		{0x4A0661, "YR-ASA"},
		{0x4A08E7, "YR-BGG"},
		{0x4AC9E1, "SE-ROA"},
		{0x4ACA76, "SE-RSV"},
		{0x4BA9C1, "TC-JNA"},
		{0x4BA945, "TC-JJE"},
		{0x4C01E4, "YU-APE"},
		// Russia, Cuba.
		{0x151DA6, "RA-73126"},
		{0x15407F, "RA-82047"}, // An-124
		{0x155BA9, "RA-89001"},
		{0x0B04E2, "CU-T1250"},
		// Middle East, Asia, Oceania.
		{0x02A180, "TS-IMA"},
		{0x02A1A0, "TS-INA"},
		{0x728639, "YI-AQY"},
		{0x728661, "YI-ASA"},
		{0x732441, "EP-IBA"},
		{0x740733, "JY-AYS"},
		{0x740828, "JY-BAH"},
		{0x7608EA, "AP-BGJ"},
		{0x760916, "AP-BHV"},
		{0x76CCE4, "9V-SGD"},
		{0x76BCC8, "9V-OFH"},
		{0x778564, "YK-AKD"},
		{0x71BE31, "HL7631"},
		{0x71C001, "HL8001"},
		{0x71C226, "HL8226"},
		{0x8406E8, "JA01XJ"},
		{0x868338, "JA743A"},
		{0x86E7BC, "JA873A"},
		{0x88044E, "HS-ABN"},
		{0x7C0000, "VH-AAA"},
		{0x7C4920, "VH-OQA"}, // Qantas A380
		{0x7C6B2D, "VH-VGF"},
		{0x7C8064, "VH-ZNA"},
		// Argentina.
		{0xE06541, "LV-FUA"},
		{0xE072CF, "LV-GKO"},
	}
	for _, tc := range tests {
		if got := Registration(tc.icao); got != tc.reg {
			t.Errorf("Registration(%06X) = %q, want %q", tc.icao, got, tc.reg)
		}
		if got := Lookup(tc.icao).Registration; got != tc.reg {
			t.Errorf("Lookup(%06X).Registration = %q, want %q", tc.icao, got, tc.reg)
		}
	}
}

func TestRegistrationNone(t *testing.T) {
	none := map[uint32]string{
		0x000000:     "zero",
		0x1A00001:    "not a 24-bit address",
		0x406A9F:     "G-ZBJH: the United Kingdom does not allocate arithmetically",
		0x484161:     "PH-BXH: neither do the Netherlands",
		0x4CA242:     "EI-DCL: nor Ireland",
		0x780A3F:     "B-LJJ: nor China",
		0xA00000:     "one before N1",
		0xADF7C8:     "one after N99999 (US military block)",
		0xAE01CE:     "USAF RC-135: military block",
		0x3F4567:     "German military block",
		0x3EA12C:     "Luftwaffe A350: military block",
		0x4B7123:     "Swiss military block",
		0x4A3421:     "YR-M: Romanian military sub-range",
		0x743421:     "JY-M: Jordanian military sub-range",
		0x76E021:     "9V-X: Singapore military sub-range",
		0x761021:     "AP-D: Pakistani sub-range that does not follow the pattern",
		0x71BA0A:     "South Korea: address offset is not binary-coded decimal",
		0x71C100:     "South Korea: gap between the HL80xx and HL82xx ranges",
		0x3C443B:     "Germany: gap in the 32-wide stride (third letter index 26)",
		0x4B5C18:     "Swiss airport ground vehicles above HB-5999",
		0x3EE097:     "one before D-0001",
		0x0B03E7:     "one before CU-T1000",
		0x15869F + 1: "one after RA-99999",
	}
	for icao, why := range none {
		if got := Registration(icao); got != "" {
			t.Errorf("Registration(%06X) = %q, want none (%s)", icao, got, why)
		}
	}
}

// First and last mark of every range.
func TestRegistrationRangeEdges(t *testing.T) {
	tests := []struct {
		icao uint32
		reg  string
	}{
		{0x380000, "F-BAAA"}, {0x386739, "F-BZZZ"}, {0x388000, "F-IAAA"}, {0x390000, "F-GAAA"},
		{0x398000, "F-HAAA"}, {0x3A0000, "F-OAAA"},
		{0x3C0001, "D-APAA"}, {0x3C1D0C, "D-AZZZ"}, {0x3C2001, "D-BPAA"},
		{0x3C4421, "D-AAAA"}, {0x3C7F5A, "D-AOZZ"}, {0x3C8421, "D-BAAA"},
		{0x3D04A8, "D-EAAA"}, {0x3D4950, "D-FAAA"}, {0x3D8DF8, "D-GAAA"}, {0x3DD2A0, "D-HAAA"},
		{0x3E1748 + 17575, "D-IZZZ"},
		{0x3EE098, "D-0001"}, {0x3F07A6, "D-9999"},
		{0x448421, "OO-AAA"}, {0x458421, "OY-AAA"}, {0x460000, "OH-AAA"}, {0x460000 + 17575, "OH-ZZZ"},
		{0x464831, "OH-U001"}, {0x464C17, "OH-U999"},
		{0x468421, "SX-AAA"}, {0x470000, "HA-AAA"}, {0x490421, "CS-AAA"},
		{0x4A0421, "YR-AAA"}, {0x4A3821, "YR-NAA"}, {0x4A8421, "SE-AAA"},
		{0x4B0000, "HB-AAA"}, {0x4B44A8, "HB-1"}, {0x4B5C16, "HB-5999"},
		{0x4B8421, "TC-AAA"}, {0x4C0000, "YU-AAA"},
		{0x028000, "TS-AAA"}, {0x728421, "YI-AAA"}, {0x730421, "EP-AAA"},
		{0x740421, "JY-AAA"}, {0x743821, "JY-NAA"}, {0x760421, "AP-AAA"}, {0x768421, "9V-AAA"},
		{0x778421, "YK-AAA"}, {0x7C0000 + 25*1296 + 25*36 + 25, "VH-ZZZ"}, {0x880421, "HS-AAA"},
		{0xC00001, "C-FAAA"}, {0xC044A8, "C-FZZZ"}, {0xC044A9, "C-GAAA"}, {0xC08950, "C-GZZZ"},
		{0xC08951 + 17575, "C-IZZZ"},
		{0xE01041, "LV-AAA"},
		{0x0B03E8, "CU-T1000"}, {0x0B07CF, "CU-T1999"},
		{0x140000, "RA-00000"}, {0x15869F, "RA-99999"},
		{0x840000, "JA0000"}, {0x840000 + 9*22984 + 9*916 + 915, "JA99ZZ"},
		{0x71BA00, "HL7200"}, {0x71BF99, "HL7799"}, {0x71C000, "HL8000"}, {0x71C299, "HL8299"},
	}
	for _, tc := range tests {
		if got := Registration(tc.icao); got != tc.reg {
			t.Errorf("Registration(%06X) = %q, want %q", tc.icao, got, tc.reg)
		}
	}
}

// nNumberAddress is an independent encoder (registration -> address) for US
// N-numbers, used to cross-check the decoder.
func nNumberAddress(reg string) (uint32, bool) {
	if !isNNumber(reg) {
		return 0, false
	}
	letter := func(c byte) uint32 { return uint32(strings.IndexByte(limitedAlphabet, c)) }
	suffix := func(s string) uint32 { // "", "A", "AB" -> 0..600
		switch len(s) {
		case 0:
			return 0
		case 1:
			return 1 + letter(s[0])*25
		}
		return 1 + letter(s[0])*25 + 1 + letter(s[1])
	}
	s := reg[1:]
	off := uint32(s[0]-'1') * 101711
	s = s[1:]
	for _, size := range []uint32{10111, 951, 35} {
		if len(s) == 0 || !isDigit(s[0]) {
			if len(s) > 2 {
				return 0, false
			}
			return 0xA00001 + off + suffix(s), true
		}
		off += 601 + uint32(s[0]-'0')*size
		s = s[1:]
	}
	// Four digits so far: nothing, one letter or one digit may follow.
	switch {
	case len(s) == 0:
	case len(s) == 1 && isDigit(s[0]):
		off += 25 + uint32(s[0]-'0')
	case len(s) == 1:
		off += 1 + letter(s[0])
	default:
		return 0, false
	}
	return 0xA00001 + off, true
}

// Every address of the US civil block decodes to a syntactically valid
// N-number that encodes back to the same address: the mapping is a bijection
// between A00001..ADF7C7 and the set of valid marks.
func TestNNumberRoundTrip(t *testing.T) {
	if usLast-usFirst+1 != 915399 {
		t.Fatalf("US block has %d addresses, want 915399", usLast-usFirst+1)
	}
	var buf []byte
	prev := ""
	for icao := uint32(usFirst); icao <= usLast; icao++ {
		buf = AppendRegistration(buf[:0], icao)
		reg := string(buf)
		back, ok := nNumberAddress(reg)
		if !ok || back != icao {
			t.Fatalf("%06X -> %q -> %06X (ok=%v)", icao, reg, back, ok)
		}
		if reg == prev {
			t.Fatalf("%06X repeats %q", icao, reg)
		}
		prev = reg
	}
	// The sequence starts N1, N1A, N1AA, N1AB ... and ends N99999.
	first := []string{"N1", "N1A", "N1AA", "N1AB", "N1AC"}
	for i, want := range first {
		if got := Registration(usFirst + uint32(i)); got != want {
			t.Errorf("Registration(%06X) = %q, want %q", usFirst+uint32(i), got, want)
		}
	}
	for reg, icao := range map[string]uint32{
		"N1": 0xA00001, "N1AZ": 0xA00001 + 25, "N1B": 0xA00001 + 26, "N1ZZ": 0xA00001 + 600,
		"N10": 0xA00001 + 601, "N2": 0xA00001 + 101711, "N9": 0xA00001 + 8*101711,
		"N12345": 0xA061D9, "N99999": 0xADF7C7,
	} {
		if got, ok := nNumberAddress(reg); !ok || got != icao {
			t.Errorf("nNumberAddress(%s) = %06X, %v; want %06X", reg, got, ok, icao)
		}
		if got := Registration(icao); got != reg {
			t.Errorf("Registration(%06X) = %q, want %q", icao, got, reg)
		}
	}
}

// The Japanese block gives distinct, well-formed marks. Each first digit owns
// 22,984 addresses of which the first 10 x 916 are used (JAdddd, JAdddL,
// JAddLL); the rest of each bucket is unused and must give no mark.
func TestJapanMarks(t *testing.T) {
	seen := make(map[string]bool, 92000)
	for icao := uint32(jaFirst); icao <= jaLast; icao++ {
		reg := Registration(icao)
		if used := (icao-jaFirst)%22984 < 9160; !used {
			if reg != "" {
				t.Fatalf("Registration(%06X) = %q in the unused part of the block", icao, reg)
			}
			continue
		}
		if len(reg) != 6 || reg[:2] != "JA" || !isDigit(reg[2]) || !isDigit(reg[3]) {
			t.Fatalf("Registration(%06X) = %q", icao, reg)
		}
		for _, c := range []byte(reg[4:]) {
			if !isDigit(c) && strings.IndexByte(limitedAlphabet, c) < 0 {
				t.Fatalf("Registration(%06X) = %q: bad character", icao, reg)
			}
		}
		if isUpper(reg[4]) && isDigit(reg[5]) {
			t.Fatalf("Registration(%06X) = %q: letter followed by digit", icao, reg)
		}
		if seen[reg] {
			t.Fatalf("Registration(%06X) = %q: duplicate", icao, reg)
		}
		seen[reg] = true
	}
	if len(seen) != 10*10*916 {
		t.Errorf("%d distinct Japanese marks, want %d", len(seen), 10*10*916)
	}
}

// The rule tables must be sorted and must not overlap each other, the special
// ranges or (for their valid part) produce malformed marks.
func TestRegistrationTables(t *testing.T) {
	type rng struct {
		lo, hi uint32
		name   string
	}
	var all []rng
	for i, m := range strideMaps {
		if m.lo > m.hi || m.base > m.lo || m.s1 < 26*m.s2 || m.s2 < 26 {
			t.Errorf("strideMaps[%d] %s: inconsistent %+v", i, m.prefix, m)
		}
		if i > 0 && strideMaps[i-1].hi >= m.lo {
			t.Errorf("strideMaps[%d] %s @%06X overlaps or is not sorted after @%06X", i, m.prefix, m.lo, strideMaps[i-1].lo)
		}
		all = append(all, rng{m.lo, m.hi, m.prefix})
		// The first and last address of the range are valid marks.
		for _, a := range []uint32{m.lo, m.hi} {
			reg := Registration(a)
			if IsMilitary(a) {
				continue
			}
			if len(reg) != len(m.prefix)+3 || !strings.HasPrefix(reg, m.prefix) {
				t.Errorf("strideMaps[%d]: Registration(%06X) = %q, want %sxxx", i, a, reg, m.prefix)
			}
		}
	}
	for i, m := range numericMaps {
		if m.lo > m.hi || m.base > m.lo {
			t.Errorf("numericMaps[%d] %s: inconsistent", i, m.prefix)
		}
		if i > 0 && numericMaps[i-1].hi >= m.lo {
			t.Errorf("numericMaps[%d] overlaps or is not sorted", i)
		}
		all = append(all, rng{m.lo, m.hi, m.prefix})
	}
	all = append(all, rng{usFirst, usLast, "N"}, rng{jaFirst, jaLast, "JA"}, rng{hlFirst, hlLast, "HL"})
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[i].lo <= all[j].hi && all[j].lo <= all[i].hi {
				t.Errorf("rules %s @%06X and %s @%06X overlap", all[i].name, all[i].lo, all[j].name, all[j].lo)
			}
		}
		// A rule must sit inside one state's address block.
		a, b := findBlock(stateBlocks, all[i].lo), findBlock(stateBlocks, all[i].hi)
		if a < 0 || a != b {
			t.Errorf("rule %s @%06X-%06X is not inside one state block", all[i].name, all[i].lo, all[i].hi)
		}
	}
}

// Nationality mark of every derived registration matches the state the
// address is allocated to.
func TestRegistrationMarkMatchesCountry(t *testing.T) {
	mark := map[string]string{
		"N": "US", "C-": "CA", "D-": "DE", "F-": "FR", "HB-": "CH", "OO-": "BE", "OY-": "DK",
		"OH-": "FI", "SE-": "SE", "SX-": "GR", "CS-": "PT", "YR-": "RO", "HA-": "HU", "YU-": "RS",
		"TC-": "TR", "RA-": "RU", "JA": "JP", "HL": "KR", "VH-": "AU", "9V-": "SG", "AP-": "PK",
		"HS-": "TH", "JY-": "JO", "EP-": "IR", "YI-": "IQ", "YK-": "SY", "TS-": "TN", "LV-": "AR",
		"CU-": "CU",
	}
	for icao := uint32(1); icao <= 0xFFFFFF; icao += 37 {
		reg := Registration(icao)
		if reg == "" {
			continue
		}
		var iso string
		for n := 3; n >= 1 && iso == ""; n-- {
			if len(reg) > n {
				iso = mark[reg[:n]]
			}
		}
		c, ok := StateOf(icao)
		if !ok || iso == "" || c.ISO2 != iso {
			t.Fatalf("Registration(%06X) = %q (mark of %q) but the address belongs to %+v", icao, reg, iso, c)
		}
	}
}

func TestAppendRegistration(t *testing.T) {
	got := AppendRegistration([]byte("reg="), 0x3C6444)
	if string(got) != "reg=D-AIBD" {
		t.Errorf("AppendRegistration = %q", got)
	}
	got = AppendRegistration([]byte("reg="), 0x406A9F)
	if string(got) != "reg=" {
		t.Errorf("AppendRegistration without rule = %q", got)
	}
	// A rejected South Korean address must not leave a partial mark behind.
	got = AppendRegistration([]byte("x"), 0x71BA0A)
	if string(got) != "x" {
		t.Errorf("AppendRegistration(71BA0A) = %q", got)
	}

	buf := make([]byte, 0, 16)
	addrs := []uint32{0x3C6444, 0xA061D9, 0x8406E8, 0x71BE31, 0x151DA6, 0x7C4920, 0x406A9F, 0xAE01CE, 0x4B4B8D}
	var sink int
	n := testing.AllocsPerRun(200, func() {
		for _, a := range addrs {
			sink += len(AppendRegistration(buf[:0], a))
		}
	})
	if n != 0 {
		t.Errorf("AppendRegistration allocates %.1f times, want 0", n)
	}
	n = testing.AllocsPerRun(200, func() { sink += len(Registration(0x406A9F)) + len(Registration(0xAE01CE)) })
	if n != 0 {
		t.Errorf("Registration without a rule allocates %.1f times, want 0", n)
	}
	_ = sink
}

func TestAppendDecimal(t *testing.T) {
	tests := []struct {
		v     uint32
		width int
		want  string
	}{
		{0, 0, "0"}, {7, 0, "7"}, {7, 3, "007"}, {1234, 4, "1234"}, {1234, 2, "1234"},
		{99999, 5, "99999"}, {5, 5, "00005"}, {4294967295, 0, "4294967295"},
	}
	for _, tc := range tests {
		if got := string(appendDecimal(nil, tc.v, tc.width)); got != tc.want {
			t.Errorf("appendDecimal(%d, %d) = %q, want %q", tc.v, tc.width, got, tc.want)
		}
	}
}

func BenchmarkRegistration(b *testing.B) {
	b.ReportAllocs()
	buf := make([]byte, 0, 16)
	var n int
	for i := 0; i < b.N; i++ {
		n += len(AppendRegistration(buf[:0], 0xA00001+uint32(i%900000)))
	}
	_ = n
}
