// Package xz is a pure-Go decoder for the .xz container (format specification
// 1.2.1) with the LZMA2 filter, the only filter used by Raspberry Pi OS images.
//
// It provides a streaming Reader and DecodeFile, which decodes the blocks of a
// multi-block file (as written by "xz -T") in parallel straight into an
// io.WriterAt. Every block's check (CRC32, CRC64 or SHA-256) and the stream
// index are verified. BCJ and delta filters are not supported and are
// reported as errors.
//
// Written from the public specifications; see docs/aggregator/CLEANROOM.md.
package xz

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
)

var (
	headerMagic = []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}
	footerMagic = []byte{'Y', 'Z'}
	crc64Table  = crc64.MakeTable(crc64.ECMA)
)

// ErrFormat reports a file that is not a valid .xz stream.
var ErrFormat = errors.New("xz: invalid .xz data")

const (
	checkNone   = 0x00
	checkCRC32  = 0x01
	checkCRC64  = 0x04
	checkSHA256 = 0x0A
	filterLZMA2 = 0x21
	maxDict     = 1 << 30 // refuse absurd dictionaries (1.5 GiB+ is legal but never used for images)
)

func checkSize(t byte) (int, error) {
	switch t {
	case checkNone:
		return 0, nil
	case checkCRC32:
		return 4, nil
	case checkCRC64:
		return 8, nil
	case checkSHA256:
		return 32, nil
	}
	if t <= 0x0F {
		return 0, fmt.Errorf("xz: unsupported check type %#x", t)
	}
	return 0, ErrFormat
}

func newCheck(t byte) hash.Hash {
	switch t {
	case checkCRC32:
		return crc32.NewIEEE()
	case checkCRC64:
		return crc64.New(crc64Table)
	case checkSHA256:
		return sha256.New()
	}
	return nil
}

func parseStreamFlags(b []byte) (byte, error) {
	if b[0] != 0 || b[1]&0xF0 != 0 {
		return 0, ErrFormat
	}
	if _, err := checkSize(b[1]); err != nil {
		return 0, err
	}
	return b[1], nil
}

// readVarint decodes an xz multibyte integer (at most 9 bytes).
func readVarint(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < len(b) && i < 9; i++ {
		v |= uint64(b[i]&0x7F) << (7 * i)
		if b[i]&0x80 == 0 {
			if i > 0 && b[i] == 0 {
				return 0, 0, ErrFormat
			}
			return v, i + 1, nil
		}
	}
	return 0, 0, ErrFormat
}

// blockHeader is a parsed block header.
type blockHeader struct {
	size         int    // header size in bytes
	compressed   int64  // -1 when absent
	uncompressed int64  // -1 when absent
	dictSize     uint32 // LZMA2 dictionary size
}

func parseBlockHeader(h []byte) (blockHeader, error) {
	bh := blockHeader{size: len(h), compressed: -1, uncompressed: -1}
	if crc32.ChecksumIEEE(h[:len(h)-4]) != binary.LittleEndian.Uint32(h[len(h)-4:]) {
		return bh, errors.New("xz: block header CRC32 mismatch")
	}
	flags := h[1]
	if flags&0x3C != 0 {
		return bh, ErrFormat
	}
	nf := int(flags&3) + 1
	p := h[2 : len(h)-4]
	if flags&0x40 != 0 {
		v, n, err := readVarint(p)
		if err != nil || v == 0 || v > 1<<62 {
			return bh, ErrFormat
		}
		bh.compressed, p = int64(v), p[n:]
	}
	if flags&0x80 != 0 {
		v, n, err := readVarint(p)
		if err != nil || v > 1<<62 {
			return bh, ErrFormat
		}
		bh.uncompressed, p = int64(v), p[n:]
	}
	for i := 0; i < nf; i++ {
		id, n, err := readVarint(p)
		if err != nil {
			return bh, ErrFormat
		}
		p = p[n:]
		ps, n, err := readVarint(p)
		if err != nil || ps > uint64(len(p)-n) {
			return bh, ErrFormat
		}
		p = p[n:]
		props := p[:ps]
		p = p[ps:]
		if i != nf-1 || id != filterLZMA2 {
			return bh, fmt.Errorf("xz: unsupported filter %#x (only a single LZMA2 filter is supported)", id)
		}
		if len(props) != 1 || props[0] > 40 {
			return bh, ErrFormat
		}
		d := props[0]
		if d == 40 {
			bh.dictSize = 0xFFFFFFFF
		} else {
			bh.dictSize = (2 | uint32(d&1)) << (d/2 + 11)
		}
		if bh.dictSize > maxDict {
			return bh, fmt.Errorf("xz: dictionary of %d bytes is too large", bh.dictSize)
		}
	}
	for _, b := range p {
		if b != 0 {
			return bh, ErrFormat
		}
	}
	return bh, nil
}

// lzma2Dec decodes one LZMA2 stream (one xz block).
type lzma2Dec struct {
	in      *bufio.Reader
	st      lzmaState
	w       window
	needDic bool
	needSt  bool
	packed  []byte
	out     []byte // pending output
	done    bool
	inCount int64 // compressed bytes consumed
}

func newLZMA2(in *bufio.Reader, dict uint32) *lzma2Dec {
	if dict < 4096 {
		dict = 4096
	}
	d := &lzma2Dec{in: in, needDic: true, needSt: true}
	d.w.limit = dict
	d.w.buf = make([]byte, int(dict)+(1<<21)+16)
	return d
}

func (d *lzma2Dec) readFull(b []byte) error {
	n, err := io.ReadFull(d.in, b)
	d.inCount += int64(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// chunk decodes the next LZMA2 chunk into the window and sets d.out to the
// produced bytes (which stay valid until the next call).
func (d *lzma2Dec) chunk() error {
	var c [1]byte
	if err := d.readFull(c[:]); err != nil {
		return err
	}
	ctrl := c[0]
	if ctrl == 0 {
		d.done = true
		d.out = nil
		return nil
	}
	start := d.w.pos
	var n int
	switch {
	case ctrl == 1 || ctrl == 2:
		var s [2]byte
		if err := d.readFull(s[:]); err != nil {
			return err
		}
		if ctrl == 1 {
			d.resetDict()
			start = d.w.pos
		} else if d.needDic {
			return errCorrupt
		}
		n = int(binary.BigEndian.Uint16(s[:])) + 1
		if err := d.copyRaw(n); err != nil {
			return err
		}
	case ctrl >= 0x80:
		var s [4]byte
		if err := d.readFull(s[:]); err != nil {
			return err
		}
		n = int(ctrl&0x1F)<<16 + int(binary.BigEndian.Uint16(s[:2])) + 1
		packed := int(binary.BigEndian.Uint16(s[2:])) + 1
		reset := (ctrl >> 5) & 3
		if reset == 3 {
			d.resetDict()
			start = d.w.pos
		} else if d.needDic {
			return errCorrupt
		}
		if reset >= 2 {
			var p [1]byte
			if err := d.readFull(p[:]); err != nil {
				return err
			}
			if err := d.st.setProps(p[0]); err != nil {
				return err
			}
		} else if d.needSt {
			return errCorrupt
		}
		if reset >= 1 {
			d.st.reset()
			d.needSt = false
		} else if d.needSt {
			return errCorrupt
		}
		if cap(d.packed) < packed {
			d.packed = make([]byte, packed, 1<<16)
		}
		d.packed = d.packed[:packed]
		if err := d.readFull(d.packed); err != nil {
			return err
		}
		var rd rangeDec
		if err := rd.init(d.packed); err != nil {
			return err
		}
		if err := d.st.decode(&rd, &d.w, n); err != nil {
			return err
		}
		if rd.pos != len(rd.buf) {
			return errCorrupt
		}
	default:
		return errCorrupt
	}
	// Hand out [start, start+n) of the circular window.
	if start+n <= len(d.w.buf) {
		d.out = d.w.buf[start : start+n]
	} else {
		if cap(d.packed) < n {
			d.packed = make([]byte, 0, n)
		}
		tmp := make([]byte, n)
		k := copy(tmp, d.w.buf[start:])
		copy(tmp[k:], d.w.buf[:n-k])
		d.out = tmp
	}
	return nil
}

func (d *lzma2Dec) resetDict() {
	d.w.pos, d.w.full, d.w.total = 0, 0, 0
	d.needDic = false
	d.needSt = true
}

func (d *lzma2Dec) copyRaw(n int) error {
	for n > 0 {
		k := n
		if room := len(d.w.buf) - d.w.pos; k > room {
			k = room
		}
		if err := d.readFull(d.w.buf[d.w.pos : d.w.pos+k]); err != nil {
			return err
		}
		d.w.pos += k
		if d.w.pos == len(d.w.buf) {
			d.w.pos = 0
		}
		d.w.full += uint64(k)
		d.w.total += uint64(k)
		n -= k
	}
	return nil
}

// decodeBlock decodes one block whose header starts at the reader, writing the
// uncompressed bytes to out. It returns the unpadded and uncompressed sizes.
func decodeBlock(br *bufio.Reader, first byte, check byte, out io.Writer) (unpadded, uncompressed int64, err error) {
	hsize := (int(first) + 1) * 4
	h := make([]byte, hsize)
	h[0] = first
	if _, err := io.ReadFull(br, h[1:]); err != nil {
		return 0, 0, unexpected(err)
	}
	bh, err := parseBlockHeader(h)
	if err != nil {
		return 0, 0, err
	}
	dec := newLZMA2(br, bh.dictSize)
	hc := newCheck(check)
	var total int64
	for !dec.done {
		if err := dec.chunk(); err != nil {
			return 0, 0, unexpected(err)
		}
		if len(dec.out) > 0 {
			total += int64(len(dec.out))
			if bh.uncompressed >= 0 && total > bh.uncompressed {
				return 0, 0, errors.New("xz: block larger than its declared size")
			}
			if hc != nil {
				hc.Write(dec.out)
			}
			if _, err := out.Write(dec.out); err != nil {
				return 0, 0, err
			}
		}
	}
	if bh.uncompressed >= 0 && total != bh.uncompressed {
		return 0, 0, errors.New("xz: block uncompressed size mismatch")
	}
	if bh.compressed >= 0 && dec.inCount != bh.compressed {
		return 0, 0, errors.New("xz: block compressed size mismatch")
	}
	// Block padding.
	pad := (4 - dec.inCount%4) % 4
	var pb [4]byte
	if _, err := io.ReadFull(br, pb[:pad]); err != nil {
		return 0, 0, unexpected(err)
	}
	for _, b := range pb[:pad] {
		if b != 0 {
			return 0, 0, ErrFormat
		}
	}
	cs, _ := checkSize(check)
	cb := make([]byte, cs)
	if _, err := io.ReadFull(br, cb); err != nil {
		return 0, 0, unexpected(err)
	}
	if hc != nil {
		sum := hc.Sum(nil)
		if check == checkCRC32 || check == checkCRC64 {
			// hash/crc* Sum is big-endian; the file stores little-endian.
			for i, j := 0, len(sum)-1; i < j; i, j = i+1, j-1 {
				sum[i], sum[j] = sum[j], sum[i]
			}
		}
		if !bytes.Equal(sum, cb) {
			return 0, 0, errors.New("xz: block check mismatch (data corrupt)")
		}
	}
	return int64(hsize) + dec.inCount + int64(cs), total, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

type indexRecord struct{ unpadded, uncompressed int64 }

// readIndex parses the index after its indicator byte (already consumed) and
// returns the records and the index size in bytes.
func readIndex(br *bufio.Reader) ([]indexRecord, int64, error) {
	var raw bytes.Buffer
	raw.WriteByte(0)
	rv := func() (uint64, error) {
		var b [9]byte
		for i := 0; i < 9; i++ {
			c, err := br.ReadByte()
			if err != nil {
				return 0, unexpected(err)
			}
			raw.WriteByte(c)
			b[i] = c
			if c&0x80 == 0 {
				v, _, err := readVarint(b[:i+1])
				return v, err
			}
		}
		return 0, ErrFormat
	}
	n, err := rv()
	if err != nil {
		return nil, 0, err
	}
	if n > 1<<24 {
		return nil, 0, ErrFormat
	}
	recs := make([]indexRecord, 0, n)
	for i := uint64(0); i < n; i++ {
		u, err := rv()
		if err != nil {
			return nil, 0, err
		}
		c, err := rv()
		if err != nil {
			return nil, 0, err
		}
		recs = append(recs, indexRecord{int64(u), int64(c)})
	}
	for raw.Len()%4 != 0 {
		c, err := br.ReadByte()
		if err != nil {
			return nil, 0, unexpected(err)
		}
		if c != 0 {
			return nil, 0, ErrFormat
		}
		raw.WriteByte(c)
	}
	var cb [4]byte
	if _, err := io.ReadFull(br, cb[:]); err != nil {
		return nil, 0, unexpected(err)
	}
	if crc32.ChecksumIEEE(raw.Bytes()) != binary.LittleEndian.Uint32(cb[:]) {
		return nil, 0, errors.New("xz: index CRC32 mismatch")
	}
	return recs, int64(raw.Len()) + 4, nil
}

// Decode decompresses all streams of an .xz file from r to w, verifying every
// check, index and footer. It returns the number of bytes written.
func Decode(w io.Writer, r io.Reader) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var total int64
	streams := 0
	for {
		// Stream padding between streams (multiples of 4 zero bytes).
		if streams > 0 {
			pad := 0
			for {
				b, err := br.Peek(1)
				if err == io.EOF || len(b) == 0 {
					if pad%4 != 0 {
						return total, ErrFormat
					}
					return total, nil
				}
				if b[0] != 0 {
					break
				}
				br.ReadByte()
				pad++
			}
			if pad%4 != 0 {
				return total, ErrFormat
			}
		}
		n, err := decodeStream(br, w)
		total += n
		if err != nil {
			return total, err
		}
		streams++
	}
}

func decodeStream(br *bufio.Reader, w io.Writer) (int64, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, unexpected(err)
	}
	if !bytes.Equal(hdr[:6], headerMagic) {
		return 0, ErrFormat
	}
	if crc32.ChecksumIEEE(hdr[6:8]) != binary.LittleEndian.Uint32(hdr[8:]) {
		return 0, errors.New("xz: stream header CRC32 mismatch")
	}
	check, err := parseStreamFlags(hdr[6:8])
	if err != nil {
		return 0, err
	}
	var blocks []indexRecord
	var total int64
	for {
		first, err := br.ReadByte()
		if err != nil {
			return total, unexpected(err)
		}
		if first == 0 {
			break
		}
		u, c, err := decodeBlock(br, first, check, w)
		total += c
		if err != nil {
			return total, err
		}
		blocks = append(blocks, indexRecord{u, c})
	}
	recs, isize, err := readIndex(br)
	if err != nil {
		return total, err
	}
	if len(recs) != len(blocks) {
		return total, errors.New("xz: index does not match the blocks")
	}
	for i := range recs {
		if recs[i] != blocks[i] {
			return total, errors.New("xz: index does not match the blocks")
		}
	}
	var ft [12]byte
	if _, err := io.ReadFull(br, ft[:]); err != nil {
		return total, unexpected(err)
	}
	if !bytes.Equal(ft[10:], footerMagic) || crc32.ChecksumIEEE(ft[4:10]) != binary.LittleEndian.Uint32(ft[:4]) {
		return total, errors.New("xz: bad stream footer")
	}
	if !bytes.Equal(ft[8:10], hdr[6:8]) {
		return total, errors.New("xz: stream footer flags differ from the header")
	}
	if (int64(binary.LittleEndian.Uint32(ft[4:8]))+1)*4 != isize {
		return total, errors.New("xz: backward size mismatch")
	}
	return total, nil
}
