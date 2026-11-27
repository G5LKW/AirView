package track

import "testing"

func TestACSquawk(t *testing.T) {
	seen := map[uint16]bool{}
	for sq := uint16(0); sq <= 0o7777; sq++ {
		raw := ACFromSquawk(sq)
		if raw&^ACCodeMask != 0 {
			t.Fatalf("squawk %04o sets the X bit: %04x", sq, raw)
		}
		if seen[raw] {
			t.Fatalf("squawk %04o: raw %04x not unique", sq, raw)
		}
		seen[raw] = true
		if got := ACSquawk(raw); got != sq {
			t.Fatalf("squawk %04o -> %04x -> %04o", sq, raw, got)
		}
		if got := ACSquawk(raw | acX); got != sq {
			t.Fatal("X bit changes the squawk")
		}
	}
	// pulse positions: 7000 = A4 A2 A1; 0001 = D1; 0010 = C1; 0100 = B1
	if ACFromSquawk(0o7000) != acA4|acA2|acA1 || ACFromSquawk(0o0001) != acD1 || ACFromSquawk(0o0010) != acC1 || ACFromSquawk(0o0100) != acB1 {
		t.Fatal("pulse layout")
	}
	if ACSquawkString(ACFromSquawk(0o7500)) != "7500" {
		t.Fatal("string")
	}
}

func TestACGillham(t *testing.T) {
	seen := map[uint16]int{}
	for ft := -1200; ft <= 126700; ft += 100 {
		raw, ok := ACFromAltitudeFt(ft)
		if !ok {
			t.Fatalf("%d ft not encodable", ft)
		}
		if raw&(acX|acD1) != 0 {
			t.Fatalf("%d ft uses X or D1", ft)
		}
		if o, dup := seen[raw]; dup {
			t.Fatalf("%d ft and %d ft share code %04x", ft, o, raw)
		}
		seen[raw] = ft
		got, ok := ACAltitudeFt(raw)
		if !ok || got != ft {
			t.Fatalf("%d ft -> %04x -> %d %v", ft, raw, got, ok)
		}
	}
	// Gray code property: adjacent 100 ft steps differ in exactly one pulse
	for ft := -1200; ft < 126700; ft += 100 {
		a, _ := ACFromAltitudeFt(ft)
		b, _ := ACFromAltitudeFt(ft + 100)
		if d := a ^ b; d == 0 || d&(d-1) != 0 {
			t.Fatalf("%d -> %d ft changes pulses %04x", ft, ft+100, d)
		}
	}
	// rounding and range
	if r1, _ := ACFromAltitudeFt(3449); r1 != mustAlt(3400) {
		t.Fatal("rounding down")
	}
	if r1, _ := ACFromAltitudeFt(3451); r1 != mustAlt(3500) {
		t.Fatal("rounding up")
	}
	for _, ft := range []int{-1300, -1251, 126751, 200000} {
		if _, ok := ACFromAltitudeFt(ft); ok {
			t.Fatalf("%d ft encoded", ft)
		}
	}
	// illegal 100 ft groups: C1 C2 C4 = 000, 101, 111
	for _, c := range []uint16{0, acC1 | acC4, acC1 | acC2 | acC4} {
		if _, ok := ACAltitudeFt(c | acB4); ok {
			t.Fatalf("C group %04x accepted", c)
		}
	}
	// a known anchor point, from the definition: 0 ft = n500 2 (B2 B4),
	// n100 3 (C2)
	if r, _ := ACFromAltitudeFt(0); r != acB2|acB4|acC2 {
		t.Fatalf("0 ft = %04x", r)
	}
}

func mustAlt(ft int) uint16 {
	r, ok := ACFromAltitudeFt(ft)
	if !ok {
		panic(ft)
	}
	return r
}

func TestACNibbles(t *testing.T) {
	for sq := uint16(0); sq <= 0o7777; sq++ {
		raw := ACFromSquawk(sq)
		n := ACNibblesFromRaw(raw)
		back, ok := ACRawFromNibbles(n)
		if !ok || back != raw {
			t.Fatalf("%04o: nibbles %04x back %04x", sq, n, back)
		}
	}
	if n := ACNibblesFromRaw(ACFromSquawk(0o7500)); n != 0x7500 {
		t.Fatalf("7500 -> %04x", n)
	}
	if _, ok := ACRawFromNibbles(0x7800); ok {
		t.Fatal("digit 8 accepted")
	}
}
