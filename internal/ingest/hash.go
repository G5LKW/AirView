package ingest

import (
	"encoding/binary"
	"math/bits"
)

// payloadHash returns a 64-bit hash of a Mode A/C or Mode S payload (at most
// 14 bytes; longer inputs are hashed over their first 16 bytes and length).
// For DF20/21 the address is part of the payload (AP field), so equal
// payloads mean equal aircraft too. The hash only selects candidates: the
// grouper always compares the payload bytes as well.
func payloadHash(d []byte) uint64 {
	var a, b uint64
	switch n := len(d); {
	case n >= 16:
		a = binary.LittleEndian.Uint64(d)
		b = binary.LittleEndian.Uint64(d[8:])
	case n > 8:
		a = binary.LittleEndian.Uint64(d)
		var t [8]byte
		copy(t[:], d[8:])
		b = binary.LittleEndian.Uint64(t[:])
	default:
		var t [8]byte
		copy(t[:], d)
		a = binary.LittleEndian.Uint64(t[:])
	}
	return mix64(a^0x9E3779B97F4A7C15, b^0xC2B2AE3D27D4EB4F^uint64(len(d))<<56)
}

// mix64 is a 128->64 bit multiply-xorshift mixer.
func mix64(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	h := hi ^ lo
	h ^= h >> 29
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 32
	return h
}

// uniquePayload reports whether a payload practically never repeats
// identically within seconds: DF17/18 surface and airborne positions (the
// CPR coordinates and the odd/even bit change with every message). Such a
// payload identifies the transmission on its own, so a copy may join its
// group even when the station's time correction is not calibrated yet.
func uniquePayload(d []byte) bool {
	if len(d) != 14 {
		return false
	}
	df := d[0] >> 3
	if df != 17 && df != 18 {
		return false
	}
	if df == 18 && d[0]&7 > 2 { // DF18 CF 3+ (TIS-B management, ADS-R): keep the strict rule
		return false
	}
	tc := d[4] >> 3
	return (tc >= 5 && tc <= 18) || (tc >= 20 && tc <= 22)
}
