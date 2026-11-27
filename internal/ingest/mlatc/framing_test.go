package mlatc

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

// clientChunks compresses lines the way the reference client does (a
// zlib-wrapped deflate stream, level 1, one sync flush per batch, the
// 00 00 FF FF tail stripped, a 2-byte big-endian length in front).
type clientChunks struct {
	buf bytes.Buffer
	zw  io.Writer
	fl  interface{ Flush() error }
	out bytes.Buffer
}

func newClientChunks(raw bool) *clientChunks {
	c := &clientChunks{}
	if raw {
		w, _ := flate.NewWriter(&c.buf, 1)
		c.zw, c.fl = w, w
	} else {
		w, _ := zlib.NewWriterLevel(&c.buf, 1)
		c.zw, c.fl = w, w
	}
	return c
}

func (c *clientChunks) batch(lines ...string) {
	for _, l := range lines {
		io.WriteString(c.zw, l)
	}
	c.fl.Flush()
	b := c.buf.Bytes()
	if !bytes.HasSuffix(b, syncTail[:]) {
		panic("no sync tail")
	}
	b = b[:len(b)-4]
	for len(b) > 0 { // the client keeps chunks below 64 KiB
		n := min(len(b), 60000)
		binary.Write(&c.out, binary.BigEndian, uint16(n))
		c.out.Write(b[:n])
		b = b[n:]
	}
	c.buf.Reset()
}

func readAllLines(t *testing.T, r io.Reader) []string {
	t.Helper()
	br := bufio.NewReaderSize(r, maxLine)
	var out []string
	for {
		l, err := br.ReadString('\n')
		if l != "" {
			out = append(out, l)
		}
		if err != nil {
			return out
		}
	}
}

func TestInflateClientChunks(t *testing.T) {
	for _, raw := range []bool{false, true} {
		cc := newClientChunks(raw)
		var want []string
		r := rand.New(rand.NewPCG(1, 2))
		for b := 0; b < 200; b++ {
			var lines []string
			for i := 0; i < 1+r.IntN(30); i++ {
				l := fmt.Sprintf(`{"mlat":{"t":%d,"m":"5d4840d6c0ffee"}}`+"\n", r.Uint64()>>16)
				lines = append(lines, l)
			}
			if b == 100 { // one huge batch, split into several chunks
				big := `{"seen":[` + strings.Repeat(`"abcdef",`, 15000) + `"000001"]}` + "\n"
				lines = append(lines, big)
			}
			want = append(want, lines...)
			cc.batch(lines...)
		}
		got := readAllLines(t, &lazyInflater{cr: newChunkReader(&cc.out)})
		if len(got) != len(want) {
			t.Fatalf("raw=%v: %d lines, want %d", raw, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("raw=%v line %d differs", raw, i)
			}
		}
	}
}

// TestInflateDoesNotReadAhead: after a chunk the inflater must hand out
// its lines without waiting for the next chunk (the client waits for our
// answers, so blocking would deadlock).
func TestInflateDoesNotReadAhead(t *testing.T) {
	pr, pw := io.Pipe()
	lines := bufio.NewReader(&lazyInflater{cr: newChunkReader(pr)})
	cc := newClientChunks(false)
	go func() {
		for i := 0; i < 5; i++ {
			cc.batch(fmt.Sprintf(`{"heartbeat":{"n":%d}}`+"\n", i))
			pw.Write(cc.out.Bytes())
			cc.out.Reset()
		}
	}()
	for i := 0; i < 5; i++ {
		l, err := lines.ReadString('\n')
		if err != nil || !strings.Contains(l, fmt.Sprintf(`"n":%d`, i)) {
			t.Fatalf("line %d: %q %v", i, l, err)
		}
	}
	pw.Close()
}

// TestStoredWriterStream decodes the server direction the way the client
// does: re-append the tail to every chunk and inflate with zlib.
func TestStoredWriterStream(t *testing.T) {
	var z storedWriter
	var wire bytes.Buffer
	var want bytes.Buffer
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 50; i++ {
		var p []byte
		n := r.IntN(3000)
		if i == 25 {
			n = 200000 // several chunks
		}
		for len(p) < n {
			p = append(p, fmt.Sprintf(`{"result":{"addr":"%06x"}}`+"\n", r.Uint32()&0xffffff)...)
		}
		want.Write(p)
		wire.Write(z.appendFrames(nil, p))
	}
	// reassemble like the client
	var stream bytes.Buffer
	b := wire.Bytes()
	chunks := 0
	for len(b) > 0 {
		n := int(binary.BigEndian.Uint16(b))
		if n == 0 || n > maxChunk {
			t.Fatalf("chunk length %d", n)
		}
		stream.Write(b[2 : 2+n])
		stream.Write(syncTail[:])
		b = b[2+n:]
		chunks++
	}
	zr, err := zlib.NewReader(&stream)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != io.ErrUnexpectedEOF && err != nil { // the stream never ends
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("decoded %d bytes, want %d", len(got), want.Len())
	}
	t.Logf("%d chunks", chunks)
}

func TestChunkReaderEmptyChunks(t *testing.T) {
	cr := newChunkReader(bytes.NewReader(make([]byte, 2*100)))
	if _, err := cr.Read(make([]byte, 10)); err != errEmptyChunk {
		t.Fatalf("err %v", err)
	}
}

// FuzzZlib2 feeds arbitrary bytes to the inbound framing: it must fail
// cleanly, never panic or hang.
func FuzzZlib2(f *testing.F) {
	cc := newClientChunks(false)
	cc.batch(`{"mlat":{"t":1,"m":"5d4840d6c0ffee"}}` + "\n")
	f.Add(cc.out.Bytes())
	f.Add([]byte{0, 2, 0x78, 0x01})
	f.Add([]byte{0, 5, 1, 2, 3, 4, 5})
	f.Fuzz(func(t *testing.T, b []byte) {
		br := newLineReader(&lazyInflater{cr: newChunkReader(bytes.NewReader(b))})
		for i := 0; i < 1000; i++ {
			if _, _, err := br.readLine(); err != nil {
				return
			}
		}
	})
}

func TestLineReader(t *testing.T) {
	long := strings.Repeat("x", maxLine+100)
	mid := strings.Repeat("y", 20000) // grows the buffer
	in := "a\n" + long + "\n" + mid + "\nb\n" + strings.Repeat("z", 3*maxLine) + "\nc\ntail"
	// tiny reads exercise every refill path
	lr := newLineReader(iotest1(strings.NewReader(in)))
	var got []string
	toos := 0
	for {
		l, too, err := lr.readLine()
		if err != nil {
			break
		}
		if too {
			toos++
			continue
		}
		got = append(got, string(l))
	}
	want := []string{"a\n", mid + "\n", "b\n", "c\n"}
	if toos != 2 || len(got) != len(want) {
		t.Fatalf("lines %d (too long %d)", len(got), toos)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: %.20q", i, got[i])
		}
	}
	if len(lr.buf) > maxLine {
		t.Fatalf("buffer grew to %d", len(lr.buf))
	}
	// handover: the bytes after a line are readable as a stream
	lr = newLineReader(strings.NewReader("hello\nrest of stream"))
	lr.readLine()
	lr.release()
	rest, _ := io.ReadAll(lr)
	if string(rest) != "rest of stream" {
		t.Fatalf("rest %q", rest)
	}
}

// iotest1 returns a reader that reads at most 7 bytes at a time.
func iotest1(r io.Reader) io.Reader { return &smallReader{r} }

type smallReader struct{ r io.Reader }

func (s *smallReader) Read(p []byte) (int, error) { return s.r.Read(p[:min(len(p), 7)]) }
