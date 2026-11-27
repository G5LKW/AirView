package modes

import (
	"sort"
	"sync"
)

// crcPoly is the Mode S CRC-24 generator polynomial
// x^24+x^23+...+x^12+x^10+x^3+1 (0x1FFF409) without its leading term.
const crcPoly = 0xFFF409

var crcTable = buildCRCTable()

func buildCRCTable() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 16
		for j := 0; j < 8; j++ {
			if c&0x800000 != 0 {
				c = (c << 1) ^ crcPoly
			} else {
				c <<= 1
			}
		}
		t[i] = c & 0xFFFFFF
	}
	return t
}

// Checksum returns the Mode S CRC-24 of data (the remainder of
// data(x)·x^24 divided by the generator). For a frame without its last three
// bytes this is the parity a transmitter would append.
func Checksum(data []byte) uint32 {
	var c uint32
	for _, b := range data {
		c = ((c << 8) ^ crcTable[byte(c>>16)^b]) & 0xFFFFFF
	}
	return c
}

// Residual returns the parity residual (syndrome) of a complete 7 or 14 byte
// frame: the CRC of everything but the last three bytes, XORed with those
// three bytes.
//
//   - DF17/DF18: 0 for an undamaged frame.
//   - DF11: the interrogator code (0 for an acquisition squitter).
//   - DF0/4/5/16/20/21/24: the transponder's 24-bit address.
//
// Frames shorter than 4 bytes yield 0xFFFFFFFF, which can never be a valid
// residual.
func Residual(frame []byte) uint32 {
	n := len(frame)
	if n < 4 {
		return 0xFFFFFFFF
	}
	return Checksum(frame[:n-3]) ^ (uint32(frame[n-3])<<16 | uint32(frame[n-2])<<8 | uint32(frame[n-1]))
}

// SetParity overwrites the last three bytes of frame with the CRC of the
// preceding bytes XORed with overlay. Use overlay 0 for DF17/DF18 and
// acquisition squitters, the interrogator code for DF11 replies and the
// 24-bit address for address/parity formats (DF0/4/5/16/20/21).
// Frames shorter than 4 bytes are left untouched.
func SetParity(frame []byte, overlay uint32) {
	n := len(frame)
	if n < 4 {
		return
	}
	p := Checksum(frame[:n-3]) ^ (overlay & 0xFFFFFF)
	frame[n-3] = byte(p >> 16)
	frame[n-2] = byte(p >> 8)
	frame[n-1] = byte(p)
}

// noBit marks an unused bit slot in a syndrome table entry.
const noBit = 0xFF

// syndromeEntry maps a parity residual to the one or two bit positions
// (0 = first transmitted bit of a 112-bit frame) whose inversion causes it.
type syndromeEntry struct {
	syn    uint32
	b0, b1 uint8
}

var (
	syndromeOnce  sync.Once
	syndromeTable []syndromeEntry // sorted by syn
)

// bitSyndromes returns the residual produced by inverting each single bit of
// a 112-bit frame. Bit i corresponds to x^(111-i), so the residuals are the
// successive powers of x modulo the generator.
func bitSyndromes() (s [112]uint32) {
	v := uint32(1)
	for i := 111; i >= 0; i-- {
		s[i] = v
		v <<= 1
		if v&0x1000000 != 0 {
			v ^= 0x1000000 | crcPoly
		}
	}
	return s
}

// buildSyndromeTable creates the lookup table for one and two bit errors in
// 112-bit frames. Errors touching the five DF bits are left out: "repairing"
// them would turn the frame into a different downlink format.
func buildSyndromeTable() {
	single := bitSyndromes()
	const first = 5
	n := 112 - first
	t := make([]syndromeEntry, 0, n+n*(n-1)/2)
	for i := first; i < 112; i++ {
		t = append(t, syndromeEntry{single[i], uint8(i), noBit})
	}
	for i := first; i < 112; i++ {
		for j := i + 1; j < 112; j++ {
			t = append(t, syndromeEntry{single[i] ^ single[j], uint8(i), uint8(j)})
		}
	}
	sort.Slice(t, func(a, b int) bool { return t[a].syn < t[b].syn })
	syndromeTable = t
}

// lookupSyndrome finds the bit error pattern for a residual of a 112-bit
// frame. maxBits limits the search to one or two bit patterns.
func lookupSyndrome(syn uint32, maxBits int) (b0, b1 uint8, ok bool) {
	if maxBits < 1 || syn == 0 {
		return 0, 0, false
	}
	syndromeOnce.Do(buildSyndromeTable)
	t := syndromeTable
	lo, hi := 0, len(t)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if t[mid].syn < syn {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= len(t) || t[lo].syn != syn {
		return 0, 0, false
	}
	e := t[lo]
	if e.b1 != noBit && maxBits < 2 {
		return 0, 0, false
	}
	return e.b0, e.b1, true
}
