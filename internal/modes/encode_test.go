package modes

import (
	"bytes"
	"testing"
)

func TestEncodeGillham(t *testing.T) {
	// Against the independent test encoder and through the decoder.
	for alt := int32(-1200); alt <= 126700; alt += 100 {
		code, ok := EncodeGillham(alt)
		if !ok || code != gillhamEncode(alt) {
			t.Fatalf("EncodeGillham(%d) = %04X, %v; want %04X", alt, code, ok, gillhamEncode(alt))
		}
		if got, ok := GillhamAltitude(code); !ok || got != alt {
			t.Fatalf("GillhamAltitude(EncodeGillham(%d)) = %d, %v", alt, got, ok)
		}
	}
	// Rounding to the nearest 100 ft and range limits.
	for _, c := range []struct{ in, want int32 }{{-1250, -1200}, {-1151, -1200}, {-1150, -1100}, {49, 0}, {50, 100}, {-49, 0}, {-51, -100}, {126749, 126700}} {
		code, ok := EncodeGillham(c.in)
		if got, _ := GillhamAltitude(code); !ok || got != c.want {
			t.Errorf("EncodeGillham(%d) decodes to %d, want %d", c.in, got, c.want)
		}
	}
	for _, alt := range []int32{-1251, -5000, 126750, 200000} {
		if _, ok := EncodeGillham(alt); ok {
			t.Errorf("EncodeGillham(%d) accepted", alt)
		}
	}
	// Hand derivation (see TestGillhamKnownAnswers): 35100 ft = D2 D4 A1 A2 A4 B1 B2 B4 C1 C2 C4 "01101100110".
	if code, _ := EncodeGillham(35100); code != grayStringToModeA(t, "01101100110") {
		t.Errorf("EncodeGillham(35100) = %04X", code)
	}
}

func TestEncodeID13(t *testing.T) {
	for squawk := 0; squawk <= 0xFFFF; squawk++ {
		s := uint16(squawk)
		f := EncodeID13(s)
		if f != id13FromModeA(s) || f&0x0040 != 0 || f > 0x1FFF {
			t.Fatalf("EncodeID13(%04X) = %04X", s, f)
		}
		if got := decodeID13(f); got != s&0x7777 {
			t.Fatalf("decodeID13(EncodeID13(%04X)) = %04X", s, got)
		}
	}
}

func TestEncodeAltitude(t *testing.T) {
	for alt := int32(-1000); alt <= 126700; alt += 25 {
		want := alt
		gillham := false
		if alt > 50175 {
			if alt%100 != 0 {
				continue
			}
			gillham = true
		}
		ac, ok := EncodeAC13(alt)
		if !ok || ac > 0x1FFF {
			t.Fatalf("EncodeAC13(%d) = %04X, %v", alt, ac, ok)
		}
		v, kind := decodeAC13(ac)
		if v != want || (kind == altGillham) != gillham || (kind != altGillham && kind != altQ25) {
			t.Fatalf("decodeAC13(EncodeAC13(%d)) = %d kind %d", alt, v, kind)
		}
		if !gillham && ac != ac13For(alt) {
			t.Fatalf("EncodeAC13(%d) = %04X, independent encoder says %04X", alt, ac, ac13For(alt))
		}
		ac12, ok := EncodeAC12(alt)
		if !ok || ac12 > 0x0FFF {
			t.Fatalf("EncodeAC12(%d) = %03X, %v", alt, ac12, ok)
		}
		v, kind = decodeAC12(ac12)
		if v != want || (kind == altGillham) != gillham {
			t.Fatalf("decodeAC12(EncodeAC12(%d)) = %d kind %d", alt, v, kind)
		}
		if !gillham && ac12 != ac12For(alt) {
			t.Fatalf("EncodeAC12(%d) = %03X, independent encoder says %03X", alt, ac12, ac12For(alt))
		}
	}
	// Rounding to the nearest 25 ft.
	for _, c := range []struct{ in, want int32 }{{-1012, -1000}, {-988, -1000}, {-987, -975}, {12, 0}, {13, 25}, {37990, 38000}, {50187, 50175}, {50188, 50200}} {
		ac, ok := EncodeAC13(c.in)
		if got, _ := decodeAC13(ac); !ok || got != c.want {
			t.Errorf("EncodeAC13(%d) decodes to %d, want %d", c.in, got, c.want)
		}
	}
	for _, alt := range []int32{-1013, -2000, 126750} {
		if _, ok := EncodeAC13(alt); ok {
			t.Errorf("EncodeAC13(%d) accepted", alt)
		}
		if _, ok := EncodeAC12(alt); ok {
			t.Errorf("EncodeAC12(%d) accepted", alt)
		}
	}
}

func TestEncodeCallsign(t *testing.T) {
	for _, s := range []string{"KLM1023", "SWR93KE", "N905XT", "A", "", "ABCDEFGH", "01234567", "D AIBC"} {
		v, ok := EncodeCallsign(s)
		if !ok || v>>48 != 0 {
			t.Errorf("EncodeCallsign(%q) = %X, %v", s, v, ok)
			continue
		}
		var cs [8]byte
		if !decodeCallsign(v, &cs) || TrimCallsign(cs) != s {
			t.Errorf("EncodeCallsign(%q) decodes to %q", s, cs[:])
		}
	}
	if v, ok := EncodeCallsign("klm1023"); !ok || v != 0x2CC371C32CE0 {
		t.Errorf("lower case: %X, %v", v, ok)
	}
	for _, s := range []string{"TOOLONG12", "AB-CD", "ÄB", "A_B", "A\x00"} {
		if _, ok := EncodeCallsign(s); ok {
			t.Errorf("EncodeCallsign(%q) accepted", s)
		}
	}
}

// TestRebuildPublishedFrames reconstructs the worked examples of "The 1090
// Megahertz Riddle" bit for bit from their decoded content using the encoders.
func TestRebuildPublishedFrames(t *testing.T) {
	// 8D4840D6202CC371C32CE0576098: DF17 CA5, TC4 category 0, KLM1023.
	cs, _ := EncodeCallsign("KLM1023")
	got := esFrame(0x8D, 0x4840D6, 4<<51|cs)
	if want := hexFrame(t, bookIdent); !bytes.Equal(got, want) {
		t.Errorf("identification: % X, want % X", got, want)
	}

	// 8D40621D58C382D690C8AC2863A7: TC11, 38000 ft, even CPR of
	// 52.2572021484375 N 3.91937255859375 E.
	ac12, _ := EncodeAC12(38000)
	lat, lon := CPREncode(52.2572021484375, 3.91937255859375, false, false)
	got = esFrame(0x8D, 0x40621D, airbornePosME(11, 0, false, ac12, false, false, lat, lon))
	if want := hexFrame(t, bookPosEven); !bytes.Equal(got, want) {
		t.Errorf("airborne position: % X, want % X", got, want)
	}

	// 8C4841753A9A153237AEF0F275BE: surface position, movement 41, track
	// 33*360/128, odd CPR of 52.32056051997815 N 4.735735212053571 E.
	lat, lon = CPREncode(52.32056051997815, 4.735735212053571, true, true)
	me := uint64(7)<<51 | 41<<44 | 1<<43 | 33<<36 | 1<<34 | uint64(lat)<<17 | uint64(lon)
	got = esFrame(0x8C, 0x484175, me)
	if want := hexFrame(t, bookSurf); !bytes.Equal(got, want) {
		t.Errorf("surface position: % X, want % X", got, want)
	}

	// 2A00516D492B80: DF5, FS 2, squawk 0356 (this transponder sets the X
	// bit) and UM = 2 (IDS = 2: a Comm-C reservation report), address 510AF9
	// overlaid on the parity.
	f := make([]byte, 7)
	f[0] = 5<<3 | 2
	id := EncodeID13(0x0356) | 0x0040
	f[2], f[3] = byte(id>>8)|2<<5, byte(id)
	SetParity(f, 0x510AF9)
	if want := hexFrame(t, bookDF5); !bytes.Equal(f, want) {
		t.Errorf("identity reply: % X, want % X", f, want)
	}

	// 2000171806A983 (1090 MHz Riddle, surveillance replies): DF4, 36000 ft,
	// address 4CA7E8 on the parity.
	f = make([]byte, 7)
	f[0] = 4 << 3
	ac, _ := EncodeAC13(36000)
	f[2], f[3] = byte(ac>>8), byte(ac)
	SetParity(f, 0x4CA7E8)
	if want := hexFrame(t, bookDF4); !bytes.Equal(f, want) {
		t.Errorf("altitude reply: % X, want % X", f, want)
	}

	// A000083E202CC371C31DE0AA1CCF: DF20, 12550 ft, BDS 2,0 KLM1017,
	// address 484163.
	cs, _ = EncodeCallsign("KLM1017")
	ac, _ = EncodeAC13(12550)
	got = commBFrame(20, ac, 0x484163, 0x20<<48|cs)
	if want := hexFrame(t, bookBDS20); !bytes.Equal(got, want) {
		t.Errorf("Comm-B identification: % X, want % X", got, want)
	}
}
