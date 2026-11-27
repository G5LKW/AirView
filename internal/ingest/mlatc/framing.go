package mlatc

import (
	"encoding/binary"
	"errors"
	"io"
)

// Compression modes negotiated in the handshake (MLATC.md §3).
const (
	compressNone  = "none"
	compressZlib  = "zlib"  // client -> server compressed, server -> client plain
	compressZlib2 = "zlib2" // both directions compressed
)

// uncompressedByteFactor is the deflate ratio assumed for plain JSON
// lines: an uncompressed client's bytes count at 1/4 against the
// per-client byte limit (Policy.MaxKBps is a compressed budget).
const uncompressedByteFactor = 4

// syncTail is the 4-byte end of a deflate sync flush (the LEN/NLEN of an
// empty stored block). The sender strips it from every chunk; the receiver
// puts it back before inflating.
var syncTail = [4]byte{0x00, 0x00, 0xff, 0xff}

// maxChunk is the largest compressed chunk a client may send (the 2-byte
// length field allows 65535).
const maxChunk = 1<<16 - 1

var errEmptyChunk = errors.New("mlatc: too many empty chunks")

// chunkReader turns the length-prefixed chunk framing of the zlib and zlib2
// modes back into one continuous deflate stream: every chunk is returned
// followed by the sync-flush tail. It reads a new chunk from the
// connection only when the previous one has been consumed, so the inflater
// never blocks while decompressed data is still owed to the caller.
type chunkReader struct {
	r      io.Reader
	buf    []byte
	pos    int
	chunks uint64 // chunks read
	wire   uint64 // compressed wire bytes (incl. length prefixes)
	empty  int    // consecutive empty chunks
}

func newChunkReader(r io.Reader) *chunkReader { return &chunkReader{r: r} }

func (c *chunkReader) Read(p []byte) (int, error) {
	for c.pos >= len(c.buf) {
		var hdr [2]byte
		if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
			return 0, err
		}
		n := int(binary.BigEndian.Uint16(hdr[:]))
		c.wire += 2
		if n == 0 {
			// Not produced by the reference client; tolerated, but a
			// stream of nothing but empty chunks is a broken peer.
			if c.empty++; c.empty > 64 {
				return 0, errEmptyChunk
			}
			continue
		}
		c.empty = 0
		if cap(c.buf) < n+len(syncTail) {
			// grow to the largest chunk seen (typically a few kB)
			c.buf = make([]byte, n+len(syncTail), max(n+len(syncTail), 2*cap(c.buf)))
		}
		c.buf = c.buf[:n+len(syncTail)]
		if _, err := io.ReadFull(c.r, c.buf[:n]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		copy(c.buf[n:], syncTail[:])
		c.pos = 0
		c.chunks++
		c.wire += uint64(n)
	}
	k := copy(p, c.buf[c.pos:])
	c.pos += k
	return k, nil
}

// ReadByte lets the inflater read without an extra bufio layer (it would
// otherwise read ahead into the next chunk and block on the network).
func (c *chunkReader) ReadByte() (byte, error) {
	var b [1]byte
	if _, err := c.Read(b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// storedChunkMax is the payload carried per outgoing chunk. With the zlib
// header (2), one stored-block header (5) and the kept first byte of the
// flush marker (1) a chunk stays well inside the 16-bit length.
const storedChunkMax = 32 << 10

// storedWriter produces the server -> client side of zlib2: a valid zlib
// stream made of stored (uncompressed) deflate blocks, cut into chunks
// exactly as a sync-flushing compressor would be. Server traffic is small
// (results, start/stop lists, heartbeats), and stored blocks need no
// compressor state, which keeps a thousand sessions cheap (a flate writer
// costs several hundred kB each). Any inflater, including zlib's, accepts
// it.
//
// Layout of the stream:
//
//	78 01                         zlib header (CM 8, CINFO 7, FLEVEL 0; FCHECK ok)
//	per chunk:
//	  00 LEN NLEN <LEN bytes>     a non-final stored block (byte aligned)
//	  00 [00 00 FF FF]            an empty stored block = the sync-flush
//	                              marker; the bracketed 4 bytes are stripped
//
// The adler-32 trailer is never written: the stream does not end.
type storedWriter struct {
	started bool
}

// appendFrames appends the length-prefixed chunks carrying payload to dst.
func (z *storedWriter) appendFrames(dst, payload []byte) []byte {
	for len(payload) > 0 {
		n := min(len(payload), storedChunkMax)
		size := 5 + n + 1
		if !z.started {
			size += 2
		}
		dst = binary.BigEndian.AppendUint16(dst, uint16(size))
		if !z.started {
			z.started = true
			dst = append(dst, 0x78, 0x01)
		}
		dst = append(dst, 0x00, byte(n), byte(n>>8), ^byte(n), ^byte(n>>8))
		dst = append(dst, payload[:n]...)
		dst = append(dst, 0x00)
		payload = payload[n:]
	}
	return dst
}
