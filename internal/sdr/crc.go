package sdr

import (
	"sort"
	"sync"

	"airview/internal/modes"
)

// Parity checking and single-bit repair for demodulated frames.
//
// The Mode S parity field is a CRC-24 (ICAO Annex 10 Vol IV, generator
// 0x1FFF409); internal/modes computes it. Because the code is linear and the
// register starts at zero, the residual of a frame with one inverted bit is
// the residual of the undamaged frame XOR the residual of an all-zero frame
// with only that bit set. This file tabulates those single-bit residuals for
// both frame lengths so that a damaged DF11/17/18 frame can be repaired by a
// table lookup.

type bitSyn struct {
	syn uint32
	bit uint8
}

var (
	synOnce  sync.Once
	synShort []bitSyn // 56-bit frames, sorted by syn
	synLong  []bitSyn // 112-bit frames, sorted by syn
)

func buildSyn(nbits int) []bitSyn {
	buf := make([]byte, nbits/8)
	// The five DF bits are never repaired: changing them would turn the
	// frame into a different format with a different parity rule.
	t := make([]bitSyn, 0, nbits-5)
	for i := 5; i < nbits; i++ {
		for j := range buf {
			buf[j] = 0
		}
		buf[i>>3] = 0x80 >> uint(i&7)
		t = append(t, bitSyn{modes.Residual(buf), uint8(i)})
	}
	sort.Slice(t, func(a, b int) bool { return t[a].syn < t[b].syn })
	return t
}

// syndromeBit returns the bit index whose inversion produces residual syn in
// a frame of nbits (56 or 112) bits.
func syndromeBit(syn uint32, nbits int) (int, bool) {
	synOnce.Do(func() {
		synShort = buildSyn(56)
		synLong = buildSyn(112)
	})
	t := synShort
	if nbits == 112 {
		t = synLong
	}
	i := sort.Search(len(t), func(i int) bool { return t[i].syn >= syn })
	if i < len(t) && t[i].syn == syn {
		return int(t[i].bit), true
	}
	return 0, false
}

// frameBits returns the length in bits of a frame with downlink format df
// (0 when df is not a defined Mode S format).
func frameBits(df int) int {
	switch {
	case df == 0, df == 4, df == 5, df == 11:
		return 56
	case df >= 16 && df <= 21, df >= 24:
		return 112
	}
	return 0
}

// parity rules
const (
	parityPI     = iota // DF11/17/18: residual 0 (DF11: interrogator code)
	parityAP            // address/parity: residual is the 24-bit address
	parityNone          // DF19 military, DF22: cannot be checked
	parityUnused        // reserved formats
)

func parityRule(df int) int {
	switch df {
	case 11, 17, 18:
		return parityPI
	case 0, 4, 5, 16, 20, 21, 24, 25, 26, 27, 28, 29, 30, 31:
		return parityAP
	case 19, 22:
		return parityNone
	}
	return parityUnused
}

// addrTable remembers the 24-bit addresses recently confirmed by a frame with
// a checkable parity (DF11 acquisition / all-call replies and DF17/18 with an
// ICAO address), so that address/parity replies can be accepted only when
// their residual names an aircraft that is really there. Times are in 12 MHz
// stream ticks so that the table behaves the same at any replay speed.
type addrTable struct {
	ttl  uint64 // ticks
	seen map[uint32]uint64
	n    int // inserts since the last sweep
}

func newAddrTable(ttlTicks uint64) *addrTable {
	return &addrTable{ttl: ttlTicks, seen: make(map[uint32]uint64)}
}

func (a *addrTable) add(addr uint32, now uint64) {
	a.seen[addr&0xFFFFFF] = now
	a.n++
	if a.n >= 4096 {
		a.sweep(now)
	}
}

func (a *addrTable) known(addr uint32, now uint64) bool {
	t, ok := a.seen[addr&0xFFFFFF]
	return ok && now-t <= a.ttl
}

func (a *addrTable) sweep(now uint64) {
	a.n = 0
	for k, t := range a.seen {
		if now-t > a.ttl {
			delete(a.seen, k)
		}
	}
}

func (a *addrTable) reset() {
	a.seen = make(map[uint32]uint64)
	a.n = 0
}
