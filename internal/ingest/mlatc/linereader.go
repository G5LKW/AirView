package mlatc

import (
	"bytes"
	"io"
)

// lineReader reads newline-terminated lines with a buffer that starts small
// and grows only for long lines, up to maxLine (a thousand idle clients
// should not each hold 64 KiB). Lines longer than maxLine are discarded and
// reported as too long. It is also an io.Reader of the unread bytes, so a
// compressed stream can take over after the handshake.
type lineReader struct {
	r    io.Reader
	buf  []byte
	r0   int // start of unread data
	w    int // end of unread data
	err  error
	skip bool // discarding the rest of an over-long line
}

const lineInitial = 4 << 10

func newLineReader(r io.Reader) *lineReader { return &lineReader{r: r} }

// Buffered returns the number of unread bytes held.
func (l *lineReader) Buffered() int { return l.w - l.r0 }

// fill reads more data, compacting or growing the buffer as needed.
func (l *lineReader) fill() {
	if l.buf == nil {
		l.buf = make([]byte, lineInitial)
	}
	if l.r0 > 0 {
		copy(l.buf, l.buf[l.r0:l.w])
		l.w -= l.r0
		l.r0 = 0
	}
	if l.w == len(l.buf) {
		if len(l.buf) >= maxLine {
			return
		}
		nb := make([]byte, min(2*len(l.buf), maxLine))
		copy(nb, l.buf[:l.w])
		l.buf = nb
	}
	n, err := l.r.Read(l.buf[l.w:])
	l.w += n
	if err != nil {
		l.err = err
	}
}

// readLine returns the next line including its '\n' (valid until the next
// call). too is set for a line that was longer than maxLine (its content
// is dropped). At the end of input a final unterminated line is not
// returned; err is the read error.
func (l *lineReader) readLine() (line []byte, too bool, err error) {
	for {
		if i := bytes.IndexByte(l.buf[l.r0:l.w], '\n'); i >= 0 {
			line = l.buf[l.r0 : l.r0+i+1]
			l.r0 += i + 1
			if l.skip {
				l.skip = false
				return nil, true, nil
			}
			return line, false, nil
		}
		if l.w-l.r0 >= maxLine || (l.skip && l.w > l.r0) {
			// no newline within maxLine: drop what we have and keep
			// discarding up to the next newline
			l.skip = true
			l.r0, l.w = 0, 0
		}
		if l.err != nil {
			return nil, false, l.err
		}
		l.fill()
	}
}

// Read implements io.Reader over the unread bytes, then the source.
func (l *lineReader) Read(p []byte) (int, error) {
	if l.r0 < l.w {
		n := copy(p, l.buf[l.r0:l.w])
		l.r0 += n
		return n, nil
	}
	if l.err != nil {
		return 0, l.err
	}
	return l.r.Read(p)
}

// release drops the buffer once the reader is only used as an io.Reader
// and nothing is buffered.
func (l *lineReader) release() {
	if l.r0 == l.w {
		l.buf, l.r0, l.w = nil, 0, 0
	}
}
