package avf

import (
	"bytes"
	"testing"
	"time"
)

// TestStreamHuffmanRoundTrip checks that the low-memory server writer
// produces a stream every reader decodes.
func TestStreamHuffmanRoundTrip(t *testing.T) {
	var buf lockedBuf
	w := NewStreamWriterHuffman(&buf, time.Hour)
	var want [][]byte
	for i := 0; i < 200; i++ {
		p := bytes.Repeat([]byte{byte(i), 'a'}, i%150)
		want = append(want, p)
		if err := w.WriteRecord(TFrames, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	rr := NewRecordReader(NewStreamReader(bytes.NewReader(buf.Bytes()), DefaultGuard()), FromClient)
	for i, p := range want {
		r, err := rr.Next()
		if err != nil || !bytes.Equal(r.Payload, p) {
			t.Fatalf("record %d: %v", i, err)
		}
	}
}

func BenchmarkNewStreamWriter(b *testing.B) {
	for _, c := range []struct {
		name string
		mk   func() *StreamWriter
	}{
		{"bestSpeed", func() *StreamWriter { return NewStreamWriter(&bytes.Buffer{}, 0) }},
		{"huffman", func() *StreamWriter { return NewStreamWriterHuffman(&bytes.Buffer{}, 0) }},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = c.mk()
			}
		})
	}
}
