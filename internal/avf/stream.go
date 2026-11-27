package avf

import (
	"bufio"
	"compress/flate"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// StreamWriter writes records into one raw DEFLATE stream (RFC 1951, level
// 1) and performs the sync flushes of AVF.md §4.1: batch interval after the
// first unflushed record, 32 KiB of pending uncompressed data, or right
// after a control record. It is safe for concurrent use.
type StreamWriter struct {
	mu      sync.Mutex
	cw      countWriter
	bw      *bufio.Writer
	fw      deflater
	batch   time.Duration
	pending int
	timer   *time.Timer
	armed   bool
	err     error
	closed  bool
	hdr     [1 + binary.MaxVarintLen64]byte

	rawBytes uint64
	records  uint64
	flushes  uint64
}

// deflater is the compressor of a StreamWriter: *flate.Writer, or
// storedWriter (no compression).
type deflater interface {
	io.Writer
	Flush() error
}

type countWriter struct {
	w io.Writer
	n uint64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += uint64(n)
	return n, err
}

// NewStreamWriter returns a writer that compresses into w. batch is the
// flush interval (0 = flush after every record).
func NewStreamWriter(w io.Writer, batch time.Duration) *StreamWriter {
	return newStreamWriterLevel(w, batch, flate.BestSpeed)
}

// NewStreamWriterHuffman is NewStreamWriter with Huffman-only compression
// (no LZ77 matching): still a valid raw DEFLATE stream for every reader,
// but the writer needs about 0.3 MB instead of 0.8 MB. Servers use it for
// their low-volume direction (PING, ACK, INTEREST, results), so that
// thousands of sessions stay affordable (DESIGN §20.2).
func NewStreamWriterHuffman(w io.Writer, batch time.Duration) *StreamWriter {
	return newStreamWriterLevel(w, batch, flate.HuffmanOnly)
}

func newStreamWriterLevel(w io.Writer, batch time.Duration, level int) *StreamWriter {
	s := &StreamWriter{batch: batch}
	s.cw.w = w
	s.bw = bufio.NewWriterSize(&s.cw, 16<<10)
	fw, err := flate.NewWriter(s.bw, level)
	if err != nil { // only for an invalid level
		panic(err)
	}
	s.fw = fw
	return s
}

// SetBatch changes the flush interval (FLOW batchMs, WELCOME batchMs). It
// takes effect for the next unflushed record.
func (s *StreamWriter) SetBatch(d time.Duration) {
	s.mu.Lock()
	s.batch = d
	s.mu.Unlock()
}

// Batch returns the current flush interval.
func (s *StreamWriter) Batch() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batch
}

// WriteRecord frames and compresses one record and applies the flush
// policy. The payload is consumed before WriteRecord returns.
func (s *StreamWriter) WriteRecord(t RecordType, payload []byte) error {
	if t.Reserved() {
		return protoErr("reserved record type %s", t)
	}
	if len(payload) > MaxRecordLen {
		return protoErr("%s payload of %d bytes exceeds %d", t, len(payload), MaxRecordLen)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return io.ErrClosedPipe
	}
	s.hdr[0] = byte(t)
	h := binary.PutUvarint(s.hdr[1:], uint64(len(payload))) + 1
	if _, err := s.fw.Write(s.hdr[:h]); err != nil {
		return s.setErr(err)
	}
	if _, err := s.fw.Write(payload); err != nil {
		return s.setErr(err)
	}
	s.records++
	s.rawBytes += uint64(h + len(payload))
	s.pending += h + len(payload)
	if t.Control() || s.pending >= FlushThreshold || s.batch <= 0 {
		return s.flushLocked()
	}
	if !s.armed {
		s.armed = true
		if s.timer == nil {
			s.timer = time.AfterFunc(s.batch, s.onTimer)
		} else {
			s.timer.Reset(s.batch)
		}
	}
	return nil
}

func (s *StreamWriter) onTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed && !s.closed && s.err == nil {
		_ = s.flushLocked()
	}
}

func (s *StreamWriter) setErr(err error) error {
	if s.err == nil {
		s.err = err
	}
	return s.err
}

func (s *StreamWriter) flushLocked() error {
	s.armed = false
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.pending == 0 {
		return nil
	}
	s.pending = 0
	s.flushes++
	if err := s.fw.Flush(); err != nil {
		return s.setErr(err)
	}
	if err := s.bw.Flush(); err != nil {
		return s.setErr(err)
	}
	return nil
}

// Flush forces a sync flush of pending records.
func (s *StreamWriter) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return s.flushLocked()
}

// Close flushes pending records and stops the batch timer. It does not
// close the underlying writer and does not terminate the DEFLATE stream (a
// session ends by closing the connection).
func (s *StreamWriter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.err
	}
	var err error
	if s.err == nil {
		err = s.flushLocked()
	}
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
	}
	return err
}

// Err returns the first write error (sticky), if any.
func (s *StreamWriter) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// WriterStats are cumulative counters of a StreamWriter.
type WriterStats struct {
	Records   uint64 // records written
	RawBytes  uint64 // uncompressed record bytes
	WireBytes uint64 // compressed bytes handed to the underlying writer
	Flushes   uint64 // sync flushes
}

// Stats returns the writer's counters.
func (s *StreamWriter) Stats() WriterStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriterStats{Records: s.records, RawBytes: s.rawBytes, WireBytes: s.cw.n, Flushes: s.flushes}
}

// ---------------------------------------------------------------- reader

// Guard holds the decompression guards of AVF.md §4.1.
type Guard struct {
	// MaxRate is the maximum inflated output in bytes per second, averaged
	// over RateWindow (default 2 MB/s over 10 s). 0 disables the rate check.
	MaxRate    int64
	RateWindow time.Duration
	// MaxRatio is the maximum inflated/wire ratio over every RatioBlock
	// bytes of output (default 100 over 1 MiB). 0 disables the ratio check.
	MaxRatio   int64
	RatioBlock int64
	// Now is the clock (default time.Now); tests replace it.
	Now func() time.Time
}

// DefaultGuard returns the server-side guard of AVF.md §4.1.
func DefaultGuard() Guard {
	return Guard{MaxRate: 2_000_000, RateWindow: 10 * time.Second, MaxRatio: 100, RatioBlock: 1 << 20}
}

// ErrGuard is wrapped by decompression-guard violations (it also wraps
// ErrProtocol).
var ErrGuard = protoErr("decompression guard")

// StreamReader inflates one raw DEFLATE stream and enforces a Guard. It is
// an io.Reader for a RecordReader. It is not safe for concurrent use.
type StreamReader struct {
	cr  countReader
	fr  io.ReadCloser
	g   Guard
	out atomic.Uint64

	// rate: per-second buckets over the window
	buckets  []int64
	bucketAt int64 // unix second of buckets[cur]
	cur      int
	sum      int64

	// ratio: current block
	blockOut  int64
	blockWire uint64
	err       error
}

type countReader struct {
	r io.Reader
	n atomic.Uint64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(uint64(n))
	return n, err
}

// NewStreamReader returns a reader that inflates r under guard g. Zero
// fields of g disable the respective check; RateWindow and RatioBlock get
// defaults when their limits are set.
func NewStreamReader(r io.Reader, g Guard) *StreamReader {
	if g.RateWindow <= 0 {
		g.RateWindow = 10 * time.Second
	}
	if g.RatioBlock <= 0 {
		g.RatioBlock = 1 << 20
	}
	if g.Now == nil {
		g.Now = time.Now
	}
	s := &StreamReader{g: g}
	s.cr.r = r
	// flate wraps a non-ByteReader in its own small bufio; the wire counter
	// therefore runs at most a few KiB ahead, which the ratio check
	// tolerates.
	s.fr = flate.NewReader(&s.cr)
	n := int(g.RateWindow / time.Second)
	if n < 1 {
		n = 1
	}
	s.buckets = make([]int64, n)
	return s
}

// Read inflates into p.
func (s *StreamReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.fr.Read(p)
	if n > 0 {
		s.out.Add(uint64(n))
		if gerr := s.account(int64(n)); gerr != nil {
			s.err = gerr
			return n, gerr
		}
	}
	if err != nil {
		s.err = err
	}
	return n, err
}

func (s *StreamReader) account(n int64) error {
	if s.g.MaxRate > 0 {
		now := s.g.Now().Unix()
		if s.bucketAt == 0 {
			s.bucketAt = now
		}
		for s.bucketAt < now { // advance, clearing expired buckets
			s.bucketAt++
			s.cur = (s.cur + 1) % len(s.buckets)
			s.sum -= s.buckets[s.cur]
			s.buckets[s.cur] = 0
			if now-s.bucketAt > int64(len(s.buckets)) { // long idle: reset
				for i := range s.buckets {
					s.buckets[i] = 0
				}
				s.sum = 0
				s.bucketAt = now
			}
		}
		s.buckets[s.cur] += n
		s.sum += n
		if s.sum > s.g.MaxRate*int64(len(s.buckets)) {
			return protoErrWrap(ErrGuard, "inflated output above %d B/s over %v", s.g.MaxRate, s.g.RateWindow)
		}
	}
	if s.g.MaxRatio > 0 {
		s.blockOut += n
		if s.blockOut >= s.g.RatioBlock {
			wire := s.cr.n.Load() - s.blockWire
			if wire == 0 || uint64(s.blockOut) > uint64(s.g.MaxRatio)*wire {
				return protoErrWrap(ErrGuard, "inflate ratio %d:%d above %d:1", s.blockOut, wire, s.g.MaxRatio)
			}
			s.blockOut = 0
			s.blockWire = s.cr.n.Load()
		}
	}
	return nil
}

// ReaderStats are cumulative counters of a StreamReader.
type ReaderStats struct {
	RawBytes  uint64 // inflated bytes
	WireBytes uint64 // compressed bytes read from the underlying reader
}

// Stats returns the reader's counters. It may be called concurrently with
// Read.
func (s *StreamReader) Stats() ReaderStats {
	return ReaderStats{RawBytes: s.out.Load(), WireBytes: s.cr.n.Load()}
}
