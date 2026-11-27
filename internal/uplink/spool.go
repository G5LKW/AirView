package uplink

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"airview/internal/avf"
)

// The disk spool (AVF §9.1, DESIGN §4.5) keeps data that no longer fits the
// RAM ring while the uplink is disconnected. It is a directory of segment
// files of at most SegmentBytes each, written only while disconnected
// (protecting SD cards) and read back as backfill once connected.
//
// Segment file: the magic line "AVFSPOOL1\n", then records
//
//	u32 n | u8 type | u8 flags | i64 hostNs | body | u32 crc32c(type..body)
//
// with n = the byte count after the length field. A record that fails its
// checksum or is truncated (power loss) ends the segment.
//
// Read progress is the position of the last acknowledged record. It is kept
// in memory and saved to "progress" at most every progressEvery and on close,
// so a crash replays at most that much data twice; segments wholly
// acknowledged are deleted at once.

const (
	spoolMagic    = "AVFSPOOL1\n"
	segSuffix     = ".seg"
	progressFile  = "progress"
	progressEvery = 5 * time.Second
	recHeader     = 4 + 1 + 1 + 8
	recTrailer    = 4
	maxSpoolBody  = 1 << 17
)

var crcTab = crc32.MakeTable(crc32.Castagnoli)

// spoolPos is a position in the spool: segment id and byte offset.
type spoolPos struct {
	Seg uint64 `json:"seg"`
	Off int64  `json:"off"`
}

func (a spoolPos) less(b spoolPos) bool { return a.Seg < b.Seg || (a.Seg == b.Seg && a.Off < b.Off) }

type segment struct {
	id   uint64
	size int64
}

type spool struct {
	dir      string
	maxBytes int64
	segMax   int64

	segs  []segment // ascending ids
	total int64

	w     *os.File
	wb    *bufio.Writer
	wseg  uint64 // id of the open write segment (0 = none)
	wsize int64

	rf     *os.File
	rb     *bufio.Reader
	rseg   uint64
	cursor spoolPos // next record to read
	ack    spoolPos // end of the last acknowledged record
	saved  time.Time
	dirty  bool

	// counters
	written, read, droppedBytes int64
	err                         error
}

// openSpool opens (and creates) a spool directory. maxBytes <= 0 is an
// error; callers do not create a spool when SpoolMB is 0.
func openSpool(dir string, maxBytes, segMax int64) (*spool, error) {
	if maxBytes <= 0 {
		return nil, errors.New("spool: size is 0")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &spool{dir: dir, maxBytes: maxBytes, segMax: min(segMax, maxBytes)}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".tmp-") {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if !strings.HasSuffix(name, segSuffix) {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSuffix(name, segSuffix), 16, 64)
		if err != nil || id == 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		s.segs = append(s.segs, segment{id: id, size: info.Size()})
		s.total += info.Size()
	}
	slices.SortFunc(s.segs, func(a, b segment) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})
	if b, err := os.ReadFile(filepath.Join(dir, progressFile)); err == nil {
		var p spoolPos
		if json.Unmarshal(b, &p) == nil {
			s.ack = p
		}
	}
	// drop segments wholly before the saved progress
	for len(s.segs) > 0 && s.segs[0].id < s.ack.Seg {
		s.removeSeg(0)
	}
	if len(s.segs) == 0 || s.ack.Seg != s.segs[0].id {
		s.ack = spoolPos{}
	}
	s.cursor = s.ack
	s.enforce()
	return s, nil
}

func (s *spool) segPath(id uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%016x%s", id, segSuffix))
}

func (s *spool) removeSeg(i int) {
	sg := s.segs[i]
	if sg.id == s.rseg {
		s.closeReader()
	}
	_ = os.Remove(s.segPath(sg.id))
	s.total -= sg.size
	s.segs = slices.Delete(s.segs, i, i+1)
}

// Bytes returns the bytes stored (written and not yet deleted).
func (s *spool) Bytes() int64 { return s.total }

// Empty reports whether nothing is left to read.
func (s *spool) Empty() bool {
	if len(s.segs) == 0 {
		return true
	}
	last := s.segs[len(s.segs)-1]
	if s.wseg == last.id {
		return false // being written: not readable, but not empty
	}
	c := s.cursor
	if c.Seg == 0 {
		return false
	}
	return c.Seg == last.id && c.Off >= last.size
}

// Append writes one record (disconnected state only).
func (s *spool) Append(r *rec) error {
	if len(r.body) > maxSpoolBody {
		return errors.New("spool: record too large")
	}
	need := int64(recHeader + len(r.body) + recTrailer)
	if s.w == nil || s.wsize+need > s.segMax {
		if err := s.rotate(); err != nil {
			s.err = err
			return err
		}
	}
	var hdr [recHeader]byte
	binary.BigEndian.PutUint32(hdr[0:], uint32(need-4))
	hdr[4] = byte(r.typ)
	if r.late {
		hdr[5] = 1
	}
	binary.BigEndian.PutUint64(hdr[6:], uint64(r.hostNs))
	crc := crc32.Update(0, crcTab, hdr[4:])
	crc = crc32.Update(crc, crcTab, r.body)
	var tr [4]byte
	binary.BigEndian.PutUint32(tr[:], crc)
	_, _ = s.wb.Write(hdr[:])
	_, _ = s.wb.Write(r.body)
	if _, err := s.wb.Write(tr[:]); err != nil {
		s.err = err
		return err
	}
	s.wsize += need
	s.segs[len(s.segs)-1].size = s.wsize
	s.total += need
	s.written += need
	s.enforce()
	return nil
}

func (s *spool) rotate() error {
	if err := s.CloseWriter(); err != nil {
		return err
	}
	id := uint64(time.Now().UnixNano())
	if n := len(s.segs); n > 0 && id <= s.segs[n-1].id {
		id = s.segs[n-1].id + 1
	}
	f, err := os.OpenFile(s.segPath(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	s.w, s.wb, s.wseg = f, bufio.NewWriterSize(f, 64<<10), id
	if _, err := s.wb.WriteString(spoolMagic); err != nil {
		return err
	}
	s.wsize = int64(len(spoolMagic))
	first := len(s.segs) == 0
	s.segs = append(s.segs, segment{id: id, size: s.wsize})
	s.total += s.wsize
	if first {
		s.cursor = spoolPos{Seg: id}
		s.ack = s.cursor
	}
	return nil
}

// CloseWriter flushes, syncs and closes the open segment (on reconnect and
// close).
func (s *spool) CloseWriter() error {
	if s.w == nil {
		return nil
	}
	err := s.wb.Flush()
	if serr := s.w.Sync(); err == nil {
		err = serr
	}
	if cerr := s.w.Close(); err == nil {
		err = cerr
	}
	s.w, s.wb, s.wseg = nil, nil, 0
	return err
}

// enforce deletes the oldest segments while the spool is over its size.
func (s *spool) enforce() {
	for s.total > s.maxBytes && len(s.segs) > 1 {
		sg := s.segs[0]
		if sg.id == s.wseg {
			break
		}
		s.droppedBytes += sg.size
		s.removeSeg(0)
		if len(s.segs) > 0 {
			first := s.segs[0].id
			if s.cursor.Seg < first {
				s.cursor = spoolPos{Seg: first}
			}
			if s.ack.Seg < first {
				s.ack = spoolPos{Seg: first}
				s.dirty = true
			}
		}
	}
}

func (s *spool) closeReader() {
	if s.rf != nil {
		_ = s.rf.Close()
	}
	s.rf, s.rb, s.rseg = nil, nil, 0
}

// Next reads the record at the cursor. ok is false when nothing is readable
// now (empty, or only the segment being written is left).
func (s *spool) Next() (*rec, bool) {
	for {
		if len(s.segs) == 0 {
			return nil, false
		}
		if s.cursor.Seg == 0 {
			s.cursor = spoolPos{Seg: s.segs[0].id}
		}
		i := slices.IndexFunc(s.segs, func(g segment) bool { return g.id >= s.cursor.Seg })
		if i < 0 {
			return nil, false
		}
		sg := s.segs[i]
		if sg.id != s.cursor.Seg {
			s.cursor = spoolPos{Seg: sg.id}
		}
		if sg.id == s.wseg {
			return nil, false
		}
		if s.cursor.Off < int64(len(spoolMagic)) {
			s.cursor.Off = int64(len(spoolMagic))
		}
		if s.cursor.Off < sg.size {
			if r, ok := s.readAt(sg); ok {
				return r, true
			}
			// corrupt or truncated tail: treat the segment as finished
			s.droppedBytes += sg.size - s.cursor.Off
			s.segs[i].size = s.cursor.Off
		}
		if i+1 >= len(s.segs) {
			return nil, false
		}
		s.cursor = spoolPos{Seg: s.segs[i+1].id}
	}
}

func (s *spool) readAt(sg segment) (*rec, bool) {
	if s.rseg != sg.id || s.rf == nil {
		s.closeReader()
		f, err := os.Open(s.segPath(sg.id))
		if err != nil {
			return nil, false
		}
		s.rf, s.rseg = f, sg.id
		s.rb = bufio.NewReaderSize(f, 64<<10)
		var magic [len(spoolMagic)]byte
		if _, err := io.ReadFull(s.rb, magic[:]); err != nil || string(magic[:]) != spoolMagic {
			return nil, false
		}
		if _, err := s.rb.Discard(int(s.cursor.Off) - len(spoolMagic)); err != nil {
			return nil, false
		}
	}
	var hdr [recHeader]byte
	if _, err := io.ReadFull(s.rb, hdr[:]); err != nil {
		s.closeReader()
		return nil, false
	}
	n := int64(binary.BigEndian.Uint32(hdr[:4]))
	bl := n - (recHeader - 4) - recTrailer
	if bl < 0 || bl > maxSpoolBody || s.cursor.Off+4+n > sg.size {
		s.closeReader()
		return nil, false
	}
	body := make([]byte, bl)
	var tr [4]byte
	if _, err := io.ReadFull(s.rb, body); err != nil {
		s.closeReader()
		return nil, false
	}
	if _, err := io.ReadFull(s.rb, tr[:]); err != nil {
		s.closeReader()
		return nil, false
	}
	crc := crc32.Update(0, crcTab, hdr[4:])
	crc = crc32.Update(crc, crcTab, body)
	if crc != binary.BigEndian.Uint32(tr[:]) {
		s.closeReader()
		return nil, false
	}
	s.cursor.Off += 4 + n
	s.read += 4 + n
	return &rec{typ: avf.RecordType(hdr[4]), late: hdr[5]&1 != 0, hostNs: int64(binary.BigEndian.Uint64(hdr[6:])), body: body,
		fromSpool: true, spoolEnd: s.cursor}, true
}

// Ack marks everything up to end (a record's spoolEnd) acknowledged and
// deletes segments that are wholly acknowledged.
func (s *spool) Ack(end spoolPos, now time.Time) {
	if !s.ack.less(end) {
		return
	}
	s.ack = end
	s.dirty = true
	for len(s.segs) > 0 {
		sg := s.segs[0]
		if sg.id == s.wseg || sg.id > s.ack.Seg || (sg.id == s.ack.Seg && s.ack.Off < sg.size) {
			break
		}
		s.removeSeg(0)
		if len(s.segs) > 0 && s.ack.Seg < s.segs[0].id {
			s.ack = spoolPos{Seg: s.segs[0].id}
		}
	}
	if len(s.segs) == 0 {
		s.ack, s.cursor = spoolPos{}, spoolPos{}
	}
	if now.Sub(s.saved) >= progressEvery {
		s.SaveProgress(now)
	}
}

// Rewind moves the read cursor back to the acknowledged position (records
// read but not acknowledged are read again).
func (s *spool) Rewind() {
	s.closeReader()
	s.cursor = s.ack
}

// SaveProgress writes the acknowledged position.
func (s *spool) SaveProgress(now time.Time) {
	s.saved = now
	if !s.dirty {
		return
	}
	b, _ := json.Marshal(s.ack)
	if err := writeFileAtomic(filepath.Join(s.dir, progressFile), b, 0o600); err == nil {
		s.dirty = false
	}
}

// Close flushes the writer and saves the progress.
func (s *spool) Close(now time.Time) error {
	err := s.CloseWriter()
	s.closeReader()
	s.dirty = true
	s.SaveProgress(now)
	return err
}
