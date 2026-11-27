package avf

import (
	"bufio"
	"encoding/binary"
	"io"
)

// Record is one decoded record. Payload aliases the reader's buffer and is
// valid only until the next call to Next.
type Record struct {
	Type    RecordType
	Payload []byte
}

// AppendRecord appends "u8 type | uvarint len | payload" to dst. It refuses
// reserved types and payloads above MaxRecordLen.
func AppendRecord(dst []byte, t RecordType, payload []byte) ([]byte, error) {
	if t.Reserved() {
		return dst, protoErr("reserved record type %s", t)
	}
	if len(payload) > MaxRecordLen {
		return dst, protoErr("%s payload of %d bytes exceeds %d", t, len(payload), MaxRecordLen)
	}
	dst = append(dst, byte(t))
	dst = binary.AppendUvarint(dst, uint64(len(payload)))
	return append(dst, payload...), nil
}

// RecordReader reads records from a decompressed stream. It enforces the
// framing rules of AVF.md §4.2 for records travelling in direction dir:
// reserved types and known types of the other direction are protocol
// errors; unknown types are skipped using their length.
type RecordReader struct {
	r   *bufio.Reader
	dir Direction
	buf []byte

	// Skipped counts unknown records that were skipped.
	Skipped uint64
	// Records counts records returned by Next.
	Records uint64
	// Bytes counts uncompressed record bytes consumed (headers included).
	Bytes uint64
}

// NewRecordReader returns a reader of records travelling in direction dir.
// r is wrapped in a bufio.Reader unless it already is one.
func NewRecordReader(r io.Reader, dir Direction) *RecordReader {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 32<<10)
	}
	return &RecordReader{r: br, dir: dir}
}

// Next returns the next known record. It returns io.EOF at a clean end of
// stream (between records) and io.ErrUnexpectedEOF inside a record.
func (rr *RecordReader) Next() (Record, error) {
	for {
		tb, err := rr.r.ReadByte()
		if err != nil {
			return Record{}, err
		}
		t := RecordType(tb)
		if t.Reserved() {
			return Record{}, protoErr("reserved record type %s", t)
		}
		n, err := readUvarint(rr.r)
		if err != nil {
			return Record{}, err
		}
		if n > MaxRecordLen {
			return Record{}, protoErr("%s length %d exceeds %d", t, n, MaxRecordLen)
		}
		rr.Bytes += 1 + uint64(uvarintLen(n)) + n
		if !t.Known() {
			if _, err := rr.r.Discard(int(n)); err != nil {
				return Record{}, unexpected(err)
			}
			rr.Skipped++
			continue
		}
		if t.Direction() != rr.dir {
			return Record{}, protoErr("%s received in the wrong direction", t)
		}
		if cap(rr.buf) < int(n) {
			rr.buf = make([]byte, n, max(int(n), 4096))
		}
		rr.buf = rr.buf[:n]
		if _, err := io.ReadFull(rr.r, rr.buf); err != nil {
			return Record{}, unexpected(err)
		}
		rr.Records++
		return Record{Type: t, Payload: rr.buf}, nil
	}
}

// readUvarint reads an LEB128 value of at most 10 bytes; overflow is a
// protocol error and EOF inside the value is io.ErrUnexpectedEOF.
func readUvarint(r io.ByteReader) (uint64, error) {
	var v uint64
	var s uint
	for i := 0; i < binary.MaxVarintLen64; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, unexpected(err)
		}
		if b < 0x80 {
			if i == binary.MaxVarintLen64-1 && b > 1 {
				return 0, protoErr("varint overflows 64 bits")
			}
			return v | uint64(b)<<s, nil
		}
		v |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, protoErr("varint longer than 10 bytes")
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}
