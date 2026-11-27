package avf

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"testing"
)

// SO-9: the stored-block writer produces a raw DEFLATE stream every reader
// decodes, records arriving at each flush, with no compressor state.
func TestStoredStreamRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	w := NewStreamWriterStored(&wire, 0)
	big := bytes.Repeat([]byte("x"), 65000) // with the others, more than one stored block
	payloads := [][]byte{[]byte("hello"), {}, big, big, []byte("end")}
	for _, p := range payloads {
		if err := w.WriteRecord(TPing, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	r := flate.NewReader(bytes.NewReader(wire.Bytes()))
	got, err := io.ReadAll(r)
	if err != nil && err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
	var want bytes.Buffer
	for _, p := range payloads {
		want.WriteByte(byte(TPing))
		var tmp [10]byte
		n := binary.PutUvarint(tmp[:], uint64(len(p)))
		want.Write(tmp[:n])
		want.Write(p)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("decoded %d bytes, want %d", len(got), want.Len())
	}
	if st := w.Stats(); st.Records != 5 || st.WireBytes < st.RawBytes {
		t.Fatalf("stats %+v", st)
	}
}
