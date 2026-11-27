package avf

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// lockedBuf is a goroutine-safe bytes.Buffer.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
	n  int // writes
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return l.b.Write(p)
}

func (l *lockedBuf) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Len()
}

func (l *lockedBuf) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.b.Bytes()...)
}

func TestStreamRoundTrip(t *testing.T) {
	var buf lockedBuf
	w := NewStreamWriter(&buf, time.Hour)
	var want [][]byte
	for i := 0; i < 500; i++ {
		p := bytes.Repeat([]byte{byte(i)}, i%300)
		want = append(want, p)
		if err := w.WriteRecord(TFrames, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	st := w.Stats()
	if st.Records != 500 || st.WireBytes == 0 || st.WireBytes >= st.RawBytes {
		t.Fatalf("%+v", st)
	}
	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), DefaultGuard())
	rr := NewRecordReader(sr, FromClient)
	for i, p := range want {
		r, err := rr.Next()
		if err != nil || r.Type != TFrames || !bytes.Equal(r.Payload, p) {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	// The stream is not terminated: the reader reports an unexpected EOF.
	if _, err := rr.Next(); err == nil {
		t.Fatal("expected end of data")
	}
	if rs := sr.Stats(); rs.RawBytes != st.RawBytes {
		t.Fatalf("raw in %d != out %d", rs.RawBytes, st.RawBytes)
	}
}

func TestStreamFlushPolicy(t *testing.T) {
	var buf lockedBuf
	w := NewStreamWriter(&buf, 40*time.Millisecond)
	if err := w.WriteRecord(TFrames, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatal("data record flushed immediately")
	}
	// Control record flushes immediately.
	if err := w.WriteRecord(TPong, nil); err != nil {
		t.Fatal(err)
	}
	n1 := buf.Len()
	if n1 == 0 {
		t.Fatal("control record not flushed")
	}
	// Batch timer flushes.
	if err := w.WriteRecord(TFrames, []byte("more")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for buf.Len() == n1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if buf.Len() == n1 {
		t.Fatal("batch timer did not flush")
	}
	// 32 KiB of pending data flushes without the timer.
	w.SetBatch(time.Hour)
	n2 := buf.Len()
	for i := 0; i < 40; i++ {
		if err := w.WriteRecord(TFrames, bytes.Repeat([]byte{byte(i * 7)}, 1000)); err != nil {
			t.Fatal(err)
		}
	}
	if buf.Len() == n2 {
		t.Fatal("32 KiB threshold did not flush")
	}
	if w.Stats().Flushes < 3 {
		t.Fatal(w.Stats())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRecord(TFrames, nil); err == nil {
		t.Fatal("write after close")
	}
	// All records decode.
	rr := NewRecordReader(NewStreamReader(bytes.NewReader(buf.Bytes()), Guard{}), FromClient)
	n := 0
	for {
		if _, err := rr.Next(); err != nil {
			break
		}
		n++
	}
	if n != 43 {
		t.Fatalf("decoded %d records", n)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestStreamWriterStickyError(t *testing.T) {
	w := NewStreamWriter(errWriter{}, 0)
	if err := w.WriteRecord(TPong, nil); err == nil {
		t.Fatal("expected error")
	}
	if err := w.WriteRecord(TPong, nil); err == nil || w.Err() == nil {
		t.Fatal("error not sticky")
	}
}

// A compression bomb (1 KiB of wire inflating to many MiB) trips the ratio
// guard after at most one ratio block.
func TestGuardRatio(t *testing.T) {
	var wire bytes.Buffer
	fw, _ := flate.NewWriter(&wire, flate.BestCompression)
	zero := make([]byte, 1<<20)
	for i := 0; i < 8; i++ {
		fw.Write(zero)
	}
	fw.Flush()
	if wire.Len() > 80<<10 {
		t.Fatalf("bomb is %d bytes", wire.Len())
	}
	g := Guard{MaxRatio: 100, RatioBlock: 1 << 20}
	sr := NewStreamReader(bytes.NewReader(wire.Bytes()), g)
	n, err := io.Copy(io.Discard, sr)
	if !errors.Is(err, ErrGuard) || !errors.Is(err, ErrProtocol) {
		t.Fatalf("ratio guard: %v after %d bytes", err, n)
	}
	if n > 2<<20 {
		t.Fatalf("read %d bytes before the guard tripped", n)
	}
	// Ordinary record traffic passes.
	var buf lockedBuf
	w := NewStreamWriter(&buf, 0)
	p := make([]byte, 200)
	for i := 0; i < 20000; i++ {
		p[i%200] = byte(i)
		p[(i*7)%200] = byte(i >> 3)
		w.WriteRecord(TFrames, p)
	}
	if _, err := io.Copy(io.Discard, NewStreamReader(bytes.NewReader(buf.Bytes()), DefaultGuard())); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("normal traffic tripped the guard: %v", err)
	}
}

func TestGuardRate(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	var buf lockedBuf
	w := NewStreamWriter(&buf, 0)
	rnd := make([]byte, 4096)
	for i := range rnd {
		rnd[i] = byte(i*131 + i/7)
	}
	for i := 0; i < 1000; i++ {
		rnd[i%4096] ^= byte(i)
		w.WriteRecord(TFrames, rnd) // ~4 MB
	}
	g := Guard{MaxRate: 100_000, RateWindow: 10 * time.Second, Now: clock}
	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), g)
	p := make([]byte, 50_000)
	// 50 kB/s for 30 s is fine.
	for i := 0; i < 30; i++ {
		if _, err := io.ReadFull(sr, p); err != nil {
			t.Fatalf("second %d: %v", i, err)
		}
		now = now.Add(time.Second)
	}
	// A burst above 1 MB within the 10 s window trips.
	var err error
	for i := 0; i < 30 && err == nil; i++ {
		_, err = io.ReadFull(sr, p)
	}
	if !errors.Is(err, ErrGuard) {
		t.Fatalf("rate guard: %v", err)
	}
}
