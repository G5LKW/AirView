package modes

import (
	"math"
	"testing"
)

// hexFrame parses a 14 or 28 digit hex string.
func hexFrame(t testing.TB, s string) []byte {
	t.Helper()
	var buf [14]byte
	n, ok := parseHexFrame(s, &buf)
	if !ok {
		t.Fatalf("bad hex frame %q", s)
	}
	return append([]byte(nil), buf[:n]...)
}

// mustDecode decodes a frame that must be accepted as is.
func mustDecode(t testing.TB, s string) Message {
	t.Helper()
	m, err := DecodeHex(s)
	if err != nil {
		t.Fatalf("Decode(%s): %v (residual %06X)", s, err, m.Residual)
	}
	return m
}

// decodeReparity decodes an extended squitter that was published with a
// placeholder parity field ("...000000") by recomputing the parity first.
func decodeReparity(t testing.TB, s string) Message {
	t.Helper()
	f := hexFrame(t, s)
	SetParity(f, 0)
	var m Message
	if err := Decode(f, &m); err != nil {
		t.Fatalf("Decode(%s) after SetParity: %v", s, err)
	}
	return m
}

// mbOf returns the MB/ME/MV field of a long frame without decoding it.
func mbOf(t testing.TB, s string) uint64 {
	t.Helper()
	f := hexFrame(t, s)
	if len(f) != 14 {
		t.Fatalf("%s is not a long frame", s)
	}
	var v uint64
	for _, b := range f[4:11] {
		v = v<<8 | uint64(b)
	}
	return v
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// setBits writes value into bits first..last (1-based) of a 56-bit field.
func setBits(v uint64, first, last uint, value uint64) uint64 {
	width := last - first + 1
	mask := (uint64(1)<<width - 1) << (56 - last)
	return v&^mask | (value<<(56-last))&mask
}

// signedRaw converts a value to a two's complement field of the given width
// (sign bit first) with the given resolution.
func signedRaw(v, lsb float64, width uint) uint64 {
	n := int64(math.Round(v / lsb))
	return uint64(n) & (1<<width - 1)
}

// bds50MB builds a BDS 5,0 payload with every field populated.
func bds50MB(roll, track, gs, trackRate, tas float64) uint64 {
	if track >= 180 {
		track -= 360
	}
	var mb uint64
	mb = setBits(mb, 1, 1, 1)
	mb = setBits(mb, 2, 11, signedRaw(roll, 45.0/256, 10))
	mb = setBits(mb, 12, 12, 1)
	mb = setBits(mb, 13, 23, signedRaw(track, 90.0/512, 11))
	mb = setBits(mb, 24, 24, 1)
	mb = setBits(mb, 25, 34, uint64(math.Round(gs/2)))
	mb = setBits(mb, 35, 35, 1)
	mb = setBits(mb, 36, 45, signedRaw(trackRate, 8.0/256, 10))
	mb = setBits(mb, 46, 46, 1)
	mb = setBits(mb, 47, 56, uint64(math.Round(tas/2)))
	return mb
}

// bds60MB builds a BDS 6,0 payload with every field populated.
func bds60MB(heading, ias, mach, baroRate, inertialRate float64) uint64 {
	if heading >= 180 {
		heading -= 360
	}
	var mb uint64
	mb = setBits(mb, 1, 1, 1)
	mb = setBits(mb, 2, 12, signedRaw(heading, 90.0/512, 11))
	mb = setBits(mb, 13, 13, 1)
	mb = setBits(mb, 14, 23, uint64(math.Round(ias)))
	mb = setBits(mb, 24, 24, 1)
	mb = setBits(mb, 25, 34, uint64(math.Round(mach/0.004)))
	mb = setBits(mb, 35, 35, 1)
	mb = setBits(mb, 36, 45, signedRaw(baroRate, 32, 10))
	mb = setBits(mb, 46, 46, 1)
	mb = setBits(mb, 47, 56, signedRaw(inertialRate, 32, 10))
	return mb
}

// bds40MB builds a BDS 4,0 payload; negative values leave a field out.
func bds40MB(mcp, fms, baro float64) uint64 {
	var mb uint64
	if mcp >= 0 {
		mb = setBits(mb, 1, 1, 1)
		mb = setBits(mb, 2, 13, uint64(math.Round(mcp/16)))
	}
	if fms >= 0 {
		mb = setBits(mb, 14, 14, 1)
		mb = setBits(mb, 15, 26, uint64(math.Round(fms/16)))
	}
	if baro >= 0 {
		mb = setBits(mb, 27, 27, 1)
		mb = setBits(mb, 28, 39, uint64(math.Round((baro-800)*10)))
	}
	return mb
}

// ac13For returns the 13-bit Q=1 altitude code of an altitude.
func ac13For(alt int32) uint16 {
	n := uint16((alt + 1000) / 25)
	return (n&0x7E0)<<2 | (n&0x010)<<1 | 0x0010 | n&0x00F
}

// esFrame builds a valid DF17/DF18 frame around an ME field.
func esFrame(first byte, icao uint32, me uint64) []byte {
	f := make([]byte, 14)
	f[0] = first
	f[1], f[2], f[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(me >> (48 - 8*uint(i)))
	}
	SetParity(f, 0)
	return f
}

// commBFrame builds a DF20 (altitude) or DF21 (identity) reply around an MB
// field, with the address overlaid on the parity.
func commBFrame(df uint8, code13 uint16, icao uint32, mb uint64) []byte {
	f := make([]byte, 14)
	f[0] = df << 3
	f[2] = byte(code13>>8) & 0x1F
	f[3] = byte(code13)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(mb >> (48 - 8*uint(i)))
	}
	SetParity(f, icao)
	return f
}
