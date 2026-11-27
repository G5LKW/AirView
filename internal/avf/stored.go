package avf

import (
	"bufio"
	"encoding/binary"
	"io"
	"time"
)

// storedWriter writes a raw DEFLATE stream (RFC 1951) of stored, i.e.
// uncompressed, blocks. Every reader of the protocol decodes it like any
// other DEFLATE stream, but the writer needs no compression state at all:
// only the bytes of the current batch. Servers use it for the direction
// that carries a few small records per second (PING, ACK, INTEREST, FLOW,
// results): a flate writer per session cost about 0.3 MB even Huffman-only,
// some 3 GB at 10,000 feeders (SO-9).
type storedWriter struct {
	w   io.Writer
	buf []byte
}

// maxStored is the largest stored block (RFC 1951 §3.2.4).
const maxStored = 65535

func (s *storedWriter) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for len(s.buf) >= maxStored {
		if err := s.block(s.buf[:maxStored]); err != nil {
			return 0, err
		}
		s.buf = append(s.buf[:0], s.buf[maxStored:]...)
	}
	return len(p), nil
}

// block writes one non-final stored block.
func (s *storedWriter) block(p []byte) error {
	var h [5]byte // BFINAL=0, BTYPE=00, aligned; LEN, NLEN
	binary.LittleEndian.PutUint16(h[1:], uint16(len(p)))
	binary.LittleEndian.PutUint16(h[3:], ^uint16(len(p)))
	if _, err := s.w.Write(h[:]); err != nil {
		return err
	}
	if len(p) == 0 {
		return nil
	}
	_, err := s.w.Write(p)
	return err
}

// Flush writes the pending bytes and, like a flate sync flush, an empty
// stored block (00 00 00 FF FF).
func (s *storedWriter) Flush() error {
	if len(s.buf) > 0 {
		if err := s.block(s.buf); err != nil {
			return err
		}
		s.buf = s.buf[:0]
		if cap(s.buf) > 64<<10 {
			s.buf = nil // do not keep a large batch's buffer for an idle session
		}
	}
	return s.block(nil)
}

// NewStreamWriterStored is NewStreamWriter without compression (stored
// DEFLATE blocks): a valid stream for every reader, with no per-session
// compressor memory.
func NewStreamWriterStored(w io.Writer, batch time.Duration) *StreamWriter {
	s := &StreamWriter{batch: batch}
	s.cw.w = w
	s.bw = bufio.NewWriterSize(&s.cw, 4<<10)
	s.fw = &storedWriter{w: s.bw}
	return s
}
