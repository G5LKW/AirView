package modes

import "testing"

// grayStringToModeA converts an 11 character Gillham bit string (pulse order
// D2 D4 A1 A2 A4 B1 B2 B4 C1 C2 C4) to the Mode A layout used here.
func grayStringToModeA(t *testing.T, s string) uint16 {
	t.Helper()
	if len(s) != 11 {
		t.Fatalf("bad gray string %q", s)
	}
	masks := [11]uint16{0x0002, 0x0004, 0x1000, 0x2000, 0x4000, 0x0100, 0x0200, 0x0400, 0x0010, 0x0020, 0x0040}
	var v uint16
	for i, m := range masks {
		if s[i] == '1' {
			v |= m
		}
	}
	return v
}

func TestGillhamKnownAnswers(t *testing.T) {
	// Worked out by hand from h = 500*N500 + 100*N100 - 1300: N500 as a
	// reflected Gray code on D2..B4, N100 on C1 C2 C4 (1=001 2=011 3=010
	// 4=110 5=100), counted backwards when N500 is odd.
	for _, c := range []struct {
		gray string
		want int32
	}{
		{"00000000011", -1100},  // N500 0, N100 2
		{"00000001001", -300},   // N500 1 (Gray 1), N100 5 -> 1
		{"00000010010", 500},    // N500 3 (Gray 2), N100 3 -> 3
		{"00000100100", 2300},   // N500 7 (Gray 4), N100 1 -> 5
		{"00011111001", 9700},   // N500 21 (Gray 31), N100 5 -> 1
		{"00110101010", 18000},  // N500 38 (Gray 53), N100 3
		{"01101100110", 35100},  // N500 72 (Gray 108), N100 4
		{"01010011001", 47800},  // N500 98 (Gray 83), N100 1
		{"11000100011", 66600},  // N500 135 (Gray 196), N100 4 -> 2
		{"10101010010", 101000}, // N500 204 (Gray 170), N100 3
		{"10000000011", 126600}, // N500 255 (Gray 128), N100 4 -> 2
	} {
		got, ok := GillhamAltitude(grayStringToModeA(t, c.gray))
		if !ok || got != c.want {
			t.Errorf("GillhamAltitude(%s) = %d, %v; want %d", c.gray, got, ok, c.want)
		}
	}
}

// gillhamEncode is an independent Gillham encoder built from the textbook
// definition h = 500*N500 + 100*N100 - 1300: N500 is an 8-bit reflected Gray
// code on D2 D4 A1 A2 A4 B1 B2 B4, N100 (1..5) a 3-bit code on C1 C2 C4 that
// runs backwards on odd 500 ft steps.
func gillhamEncode(alt int32) uint16 {
	n := (alt + 1300 - 100) / 100
	n500 := uint32(n / 5)
	n100 := n%5 + 1
	if n500&1 != 0 {
		n100 = 6 - n100
	}
	gray := n500 ^ n500>>1
	var v uint16
	masks := [8]uint16{0x0002, 0x0004, 0x1000, 0x2000, 0x4000, 0x0100, 0x0200, 0x0400} // MSB first
	for i, m := range masks {
		if gray&(0x80>>uint(i)) != 0 {
			v |= m
		}
	}
	switch n100 {
	case 1:
		v |= 0x0040 // C4
	case 2:
		v |= 0x0060 // C2 C4
	case 3:
		v |= 0x0020 // C2
	case 4:
		v |= 0x0030 // C1 C2
	case 5:
		v |= 0x0010 // C1
	}
	return v
}

// id13FromModeA is the inverse of decodeID13.
func id13FromModeA(a uint16) uint16 {
	pairs := [...][2]uint16{
		{0x0010, 0x1000}, {0x1000, 0x0800}, {0x0020, 0x0400}, {0x2000, 0x0200},
		{0x0040, 0x0100}, {0x4000, 0x0080}, {0x0100, 0x0020}, {0x0001, 0x0010},
		{0x0200, 0x0008}, {0x0002, 0x0004}, {0x0400, 0x0002}, {0x0004, 0x0001},
	}
	var f uint16
	for _, p := range pairs {
		if a&p[0] != 0 {
			f |= p[1]
		}
	}
	return f
}

func TestGillhamAllAltitudes(t *testing.T) {
	seen := map[uint16]bool{}
	for alt := int32(-1200); alt <= 126700; alt += 100 {
		code := gillhamEncode(alt)
		if seen[code] {
			t.Fatalf("encoder produced code %04X twice (alt %d)", code, alt)
		}
		seen[code] = true
		got, ok := GillhamAltitude(code)
		if !ok || got != alt {
			t.Fatalf("GillhamAltitude(%04X) = %d, %v; want %d", code, got, ok, alt)
		}
		// Through the 13-bit AC field of a surveillance reply (Q=0, M=0).
		ac := id13FromModeA(code)
		if ac&0x0050 != 0 {
			t.Fatalf("alt %d: AC code %04X has M or Q set", alt, ac)
		}
		v, kind := decodeAC13(ac)
		if kind != altGillham || v != alt {
			t.Fatalf("decodeAC13(%04X) = %d kind %d; want %d Gillham", ac, v, kind, alt)
		}
		// Through the 12-bit field of an airborne position (no M bit).
		ac12 := (ac&0x1F80)>>1 | ac&0x003F
		v, kind = decodeAC12(ac12)
		if kind != altGillham || v != alt {
			t.Fatalf("decodeAC12(%03X) = %d kind %d; want %d Gillham", ac12, v, kind, alt)
		}
	}
}

func TestGillhamRejectsIllegalCodes(t *testing.T) {
	for _, code := range []uint16{
		0x0000, // no C bits
		0x1000, // no C bits
		0x0050, // C1+C4 = 5 -> 7, illegal
		0x0070, // C1+C2+C4, illegal
		0x0011, // D1 set
		0x8010, // bit outside the code
		0x0810, // bit outside the code
	} {
		if alt, ok := GillhamAltitude(code); ok {
			t.Errorf("GillhamAltitude(%04X) = %d, want rejection", code, alt)
		}
	}
	// Exhaustive: every accepted code must be one the encoder can produce.
	valid := map[uint16]int32{}
	for alt := int32(-1200); alt <= 126700; alt += 100 {
		valid[gillhamEncode(alt)] = alt
	}
	for code := 0; code < 0x10000; code++ {
		alt, ok := GillhamAltitude(uint16(code))
		want, legal := valid[uint16(code)]
		if ok != legal || (ok && alt != want) {
			t.Fatalf("GillhamAltitude(%04X) = %d, %v; want %d, %v", code, alt, ok, want, legal)
		}
	}
}

func TestAC13QBit(t *testing.T) {
	// 25 ft encoding: N*25 - 1000 with the M and Q bits removed.
	for _, alt := range []int32{-1000, -975, -25, 0, 25, 1000, 36000, 38000, 45000, 50175} {
		n := uint16((alt + 1000) / 25)
		ac := (n&0x7E0)<<2 | (n&0x010)<<1 | 0x0010 | n&0x00F
		v, kind := decodeAC13(ac)
		if kind != altQ25 || v != alt {
			t.Errorf("decodeAC13(%04X) = %d kind %d; want %d", ac, v, kind, alt)
		}
		ac12 := (ac&0x1F80)>>1 | ac&0x003F
		v, kind = decodeAC12(ac12)
		if kind != altQ25 || v != alt {
			t.Errorf("decodeAC12(%03X) = %d kind %d; want %d", ac12, v, kind, alt)
		}
	}
	if _, kind := decodeAC13(0); kind != altNone {
		t.Errorf("all-zero AC must be 'not available', got kind %d", kind)
	}
	if _, kind := decodeAC12(0); kind != altNone {
		t.Errorf("all-zero AC12 must be 'not available', got kind %d", kind)
	}
}

func TestAC13Metric(t *testing.T) {
	// M=1: reserved metric encoding. The raw 12-bit number is exposed
	// separately and never reported as a barometric altitude in feet.
	v, kind := decodeAC13(0x0040 | 0x1F80 | 0x003F)
	if kind != altMetric || v != 0xFFF {
		t.Fatalf("got %d kind %d", v, kind)
	}
	f := make([]byte, 7)
	f[0] = 4 << 3
	f[2], f[3] = 0x02, 0x40|0x15 // M=1 plus some data bits
	SetParity(f, 0x123456)
	var m Message
	if err := Decode(f, &m); err != nil {
		t.Fatal(err)
	}
	if m.Has(FAltBaro) || !m.Has(FAltMetric) || m.AltMetric != 0x115 {
		t.Fatalf("valid=%b AltMetric=%X", m.Valid, m.AltMetric)
	}
}

func TestSquawkLayout(t *testing.T) {
	// Every squawk 0000..7777 survives the ID13 bit shuffle.
	for a := uint16(0); a < 8; a++ {
		for b := uint16(0); b < 8; b++ {
			for c := uint16(0); c < 8; c++ {
				for d := uint16(0); d < 8; d++ {
					want := a<<12 | b<<8 | c<<4 | d
					if got := decodeID13(id13FromModeA(want)); got != want {
						t.Fatalf("squawk %04X came back as %04X", want, got)
					}
				}
			}
		}
	}
	// The X bit is not part of the code.
	if got := decodeID13(0x0040); got != 0 {
		t.Errorf("X bit leaked into the code: %04X", got)
	}
}
