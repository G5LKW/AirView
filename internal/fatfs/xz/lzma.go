package xz

import (
	"errors"
	"fmt"
)

// This file is an LZMA decoder written from the published format description
// (the LZMA specification text in the public-domain LZMA SDK, "lzma-specification.txt",
// and the .xz file format specification 1.2.1). No code was copied; see
// docs/aggregator/CLEANROOM.md (WP-PIIMG).

const (
	numStates       = 12
	posStatesMax    = 1 << 4
	lenLowBits      = 3
	lenMidBits      = 3
	lenHighBits     = 8
	lenLowSymbols   = 1 << lenLowBits
	lenMidSymbols   = 1 << lenMidBits
	matchMinLen     = 2
	numLenToPos     = 4
	numPosSlotBits  = 6
	startPosModel   = 4
	endPosModel     = 14
	numFullDists    = 1 << (endPosModel >> 1) // 128
	numAlignBits    = 4
	probInit        = 1024
	bitModelTotal   = 1 << 11
	numMoveBits     = 5
	topValue        = 1 << 24
	literalCoderLen = 0x300
)

var errCorrupt = errors.New("xz: corrupt LZMA data")

// rangeDec decodes from an in-memory packed chunk.
type rangeDec struct {
	buf   []byte
	pos   int
	rng   uint32
	code  uint32
	extra bool // read past the end of buf
}

func (r *rangeDec) init(buf []byte) error {
	r.buf, r.pos, r.extra = buf, 0, false
	if len(buf) < 5 || buf[0] != 0 {
		return errCorrupt
	}
	r.rng = 0xFFFFFFFF
	r.code = uint32(buf[1])<<24 | uint32(buf[2])<<16 | uint32(buf[3])<<8 | uint32(buf[4])
	r.pos = 5
	if r.code == r.rng {
		return errCorrupt
	}
	return nil
}

func (r *rangeDec) next() uint32 {
	if r.pos < len(r.buf) {
		b := r.buf[r.pos]
		r.pos++
		return uint32(b)
	}
	r.extra = true
	return 0
}

func (r *rangeDec) bit(p *uint16) uint32 {
	bound := (r.rng >> 11) * uint32(*p)
	var b uint32
	if r.code < bound {
		r.rng = bound
		*p += (bitModelTotal - *p) >> numMoveBits
	} else {
		r.rng -= bound
		r.code -= bound
		*p -= *p >> numMoveBits
		b = 1
	}
	if r.rng < topValue {
		r.rng <<= 8
		r.code = r.code<<8 | r.next()
	}
	return b
}

func (r *rangeDec) direct(n uint32) uint32 {
	var res uint32
	for ; n > 0; n-- {
		r.rng >>= 1
		var b uint32
		if r.code >= r.rng {
			r.code -= r.rng
			b = 1
		}
		res = res<<1 | b
		if r.rng < topValue {
			r.rng <<= 8
			r.code = r.code<<8 | r.next()
		}
	}
	return res
}

// tree decodes numBits bits MSB first with probs[1:1<<numBits].
func (r *rangeDec) tree(probs []uint16, numBits uint32) uint32 {
	m := uint32(1)
	for i := uint32(0); i < numBits; i++ {
		m = m<<1 | r.bit(&probs[m])
	}
	return m - (1 << numBits)
}

// reverse decodes numBits bits LSB first with probs[1:].
func (r *rangeDec) reverse(probs []uint16, numBits uint32) uint32 {
	m, sym := uint32(1), uint32(0)
	for i := uint32(0); i < numBits; i++ {
		b := r.bit(&probs[m])
		m = m<<1 | b
		sym |= b << i
	}
	return sym
}

type lenDec struct {
	choice, choice2 uint16
	low             [posStatesMax][lenLowSymbols]uint16
	mid             [posStatesMax][lenMidSymbols]uint16
	high            [1 << lenHighBits]uint16
}

func (l *lenDec) reset() {
	l.choice, l.choice2 = probInit, probInit
	for i := range l.low {
		for j := range l.low[i] {
			l.low[i][j] = probInit
			l.mid[i][j] = probInit
		}
	}
	for i := range l.high {
		l.high[i] = probInit
	}
}

func (l *lenDec) decode(r *rangeDec, posState uint32) uint32 {
	if r.bit(&l.choice) == 0 {
		return r.tree(l.low[posState][:], lenLowBits)
	}
	if r.bit(&l.choice2) == 0 {
		return lenLowSymbols + r.tree(l.mid[posState][:], lenMidBits)
	}
	return lenLowSymbols + lenMidSymbols + r.tree(l.high[:], lenHighBits)
}

// lzmaState is the adaptive model of an LZMA decoder (reset by LZMA2 state resets).
type lzmaState struct {
	lc, lp, pb uint32
	literal    []uint16
	isMatch    [numStates << 4]uint16
	isRep      [numStates]uint16
	isRepG0    [numStates]uint16
	isRepG1    [numStates]uint16
	isRepG2    [numStates]uint16
	isRep0Long [numStates << 4]uint16
	posSlot    [numLenToPos][1 << numPosSlotBits]uint16
	posSpec    [1 + numFullDists - endPosModel]uint16
	align      [1 << numAlignBits]uint16
	lenDec     lenDec
	repLenDec  lenDec
	state      uint32
	rep        [4]uint32
}

func (s *lzmaState) setProps(props byte) error {
	if props >= 9*5*5 {
		return fmt.Errorf("xz: invalid LZMA properties byte %#x", props)
	}
	p := uint32(props)
	s.lc = p % 9
	p /= 9
	s.lp = p % 5
	s.pb = p / 5
	if s.lc+s.lp > 4 {
		return fmt.Errorf("xz: LZMA2 requires lc+lp <= 4 (lc=%d lp=%d)", s.lc, s.lp)
	}
	n := literalCoderLen << (s.lc + s.lp)
	if cap(s.literal) >= n {
		s.literal = s.literal[:n]
	} else {
		s.literal = make([]uint16, n)
	}
	return nil
}

func fill(p []uint16) {
	for i := range p {
		p[i] = probInit
	}
}

func (s *lzmaState) reset() {
	fill(s.literal)
	fill(s.isMatch[:])
	fill(s.isRep[:])
	fill(s.isRepG0[:])
	fill(s.isRepG1[:])
	fill(s.isRepG2[:])
	fill(s.isRep0Long[:])
	for i := range s.posSlot {
		fill(s.posSlot[i][:])
	}
	fill(s.posSpec[:])
	fill(s.align[:])
	s.lenDec.reset()
	s.repLenDec.reset()
	s.state = 0
	s.rep = [4]uint32{}
}

// window is the decoder's history: a circular buffer that also holds the
// bytes produced since the last flush.
type window struct {
	buf    []byte
	pos    int    // next write index
	full   uint64 // bytes written since the dictionary reset (capped use)
	total  uint64 // position since the dictionary reset (LZMA posState / lp)
	flushd int    // index up to which buf has been handed out
	limit  uint32 // dictionary size from the block header
}

func (w *window) put(b byte) {
	w.buf[w.pos] = b
	w.pos++
	if w.pos == len(w.buf) {
		w.pos = 0
	}
	w.full++
	w.total++
}

func (w *window) back(dist uint32) byte { // dist 1 = previous byte
	i := w.pos - int(dist)
	if i < 0 {
		i += len(w.buf)
	}
	return w.buf[i]
}

// decodeLZMA decodes exactly outLen bytes of one LZMA2 chunk into w.
func (s *lzmaState) decode(r *rangeDec, w *window, outLen int) error {
	pbMask := uint32(1)<<s.pb - 1
	lpMask := uint32(1)<<s.lp - 1
	lc := s.lc
	for outLen > 0 {
		posState := uint32(w.total) & pbMask
		st := s.state
		if r.bit(&s.isMatch[st<<4+posState]) == 0 {
			var prev uint32
			if w.full > 0 {
				prev = uint32(w.back(1))
			}
			litState := ((uint32(w.total) & lpMask) << lc) + (prev >> (8 - lc))
			probs := s.literal[literalCoderLen*litState : literalCoderLen*(litState+1)]
			sym := uint32(1)
			if st >= 7 {
				if uint64(s.rep[0]) >= w.full {
					return errCorrupt
				}
				match := uint32(w.back(s.rep[0] + 1))
				for sym < 0x100 {
					mb := (match >> 7) & 1
					match <<= 1
					b := r.bit(&probs[((1+mb)<<8)+sym])
					sym = sym<<1 | b
					if mb != b {
						break
					}
				}
			}
			for sym < 0x100 {
				sym = sym<<1 | r.bit(&probs[sym])
			}
			w.put(byte(sym))
			outLen--
			switch {
			case st < 4:
				s.state = 0
			case st < 10:
				s.state = st - 3
			default:
				s.state = st - 6
			}
			continue
		}
		var length uint32
		if r.bit(&s.isRep[st]) != 0 {
			if w.full == 0 {
				return errCorrupt
			}
			if r.bit(&s.isRepG0[st]) == 0 {
				if r.bit(&s.isRep0Long[st<<4+posState]) == 0 {
					// Short rep: one byte at rep0.
					if uint64(s.rep[0]) >= w.full {
						return errCorrupt
					}
					if st < 7 {
						s.state = 9
					} else {
						s.state = 11
					}
					w.put(w.back(s.rep[0] + 1))
					outLen--
					continue
				}
			} else {
				var dist uint32
				if r.bit(&s.isRepG1[st]) == 0 {
					dist = s.rep[1]
				} else {
					if r.bit(&s.isRepG2[st]) == 0 {
						dist = s.rep[2]
					} else {
						dist = s.rep[3]
						s.rep[3] = s.rep[2]
					}
					s.rep[2] = s.rep[1]
				}
				s.rep[1] = s.rep[0]
				s.rep[0] = dist
			}
			length = s.repLenDec.decode(r, posState)
			if st < 7 {
				s.state = 8
			} else {
				s.state = 11
			}
		} else {
			s.rep[3], s.rep[2], s.rep[1] = s.rep[2], s.rep[1], s.rep[0]
			length = s.lenDec.decode(r, posState)
			if st < 7 {
				s.state = 7
			} else {
				s.state = 10
			}
			lenState := length
			if lenState > numLenToPos-1 {
				lenState = numLenToPos - 1
			}
			slot := r.tree(s.posSlot[lenState][:], numPosSlotBits)
			var dist uint32
			if slot < startPosModel {
				dist = slot
			} else {
				nd := (slot >> 1) - 1
				dist = (2 | (slot & 1)) << nd
				if slot < endPosModel {
					dist += r.reverse(s.posSpec[dist-slot:], nd)
				} else {
					dist += r.direct(nd-numAlignBits) << numAlignBits
					dist += r.reverse(s.align[:], numAlignBits)
				}
			}
			if dist == 0xFFFFFFFF {
				return errors.New("xz: unexpected LZMA end marker inside an LZMA2 chunk")
			}
			s.rep[0] = dist
		}
		length += matchMinLen
		dist := s.rep[0] + 1
		if uint64(dist) > w.full || dist > w.limit || int(dist) > len(w.buf) {
			return errCorrupt
		}
		if int(length) > outLen {
			return errCorrupt
		}
		outLen -= int(length)
		// Copy with overlap semantics.
		src := w.pos - int(dist)
		if src < 0 {
			src += len(w.buf)
		}
		n := len(w.buf)
		if src+int(length) <= n && w.pos+int(length) <= n && (dist >= length) {
			copy(w.buf[w.pos:w.pos+int(length)], w.buf[src:src+int(length)])
			w.pos += int(length)
			if w.pos == n {
				w.pos = 0
			}
			w.full += uint64(length)
			w.total += uint64(length)
		} else {
			for i := uint32(0); i < length; i++ {
				b := w.buf[src]
				src++
				if src == n {
					src = 0
				}
				w.put(b)
			}
		}
		if r.extra {
			return errCorrupt
		}
	}
	if r.extra {
		return errCorrupt
	}
	return nil
}
