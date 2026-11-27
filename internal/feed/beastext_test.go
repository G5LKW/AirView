package feed

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

type escRec struct {
	typ  byte
	body []byte // nil for unknown types
}

func collectEscapes(chunks ...[]byte) ([]parsed, []escRec, *BeastParser) {
	var frames []parsed
	var escs []escRec
	p := NewBeastParser(func(f *BeastFrame) {
		frames = append(frames, parsed{f.Type, f.Timestamp, f.Signal, append([]byte(nil), f.Payload...)})
	})
	p.OnEscape = func(typ byte, body []byte) {
		r := escRec{typ: typ}
		if body != nil {
			r.body = append([]byte{}, body...)
		}
		escs = append(escs, r)
	}
	for _, c := range chunks {
		p.Write(c)
	}
	return frames, escs, p
}

func TestBeastParserEscapeCallback(t *testing.T) {
	uuid := "0f8c1e2a-7d3b-4c55-9a1a-1a2b3c4d5e6f" // contains no 0x1A, but the ping and ID do
	var s []byte
	s = AppendBeastUUID(s, uuid)
	s = AppendBeastOption(s, BeastOptPing)
	s = AppendBeastReceiverID(s, 0x1A00_0000_DEAD_BE1A)
	s = AppendBeastFrame(s, BeastModeSLong, 1000, 10, long1)
	s = AppendBeastPing(s, 0x1A1A1A)
	s = append(s, 0x1A, 0xE5, 'x', 'y') // unknown extension type
	s = AppendBeastFrame(s, BeastModeSShort, 2000, 20, short1)
	s = append(s, 0x1A, 0xE5)
	s = AppendBeastFrame(s, BeastModeSLong, 3000, 30, long2)

	wantFrames := []parsed{
		{BeastModeSLong, 1000, 10, long1},
		{BeastModeSShort, 2000, 20, short1},
		{BeastModeSLong, 3000, 30, long2},
	}
	idBody := []byte{0x1A, 0, 0, 0, 0xDE, 0xAD, 0xBE, 0x1A}
	wantEsc := []escRec{
		{BeastUUID, []byte(uuid)},
		{BeastOption, []byte{'O'}},
		{BeastReceiverID, idBody},
		{BeastPing, []byte{0x1A, 0x1A, 0x1A}},
		{0xE5, nil},
		{0xE5, nil},
	}
	for i := 0; i <= len(s); i++ {
		frames, escs, p := collectEscapes(s[:i], s[i:])
		checkParsed(t, fmt.Sprintf("split %d", i), frames, wantFrames)
		if len(escs) != len(wantEsc) {
			t.Fatalf("split %d: escapes %v", i, escs)
		}
		for j, w := range wantEsc {
			if escs[j].typ != w.typ || !bytes.Equal(escs[j].body, w.body) || (w.body == nil) != (escs[j].body == nil) {
				t.Errorf("split %d escape %d = %x %q, want %x %q", i, j, escs[j].typ, escs[j].body, w.typ, w.body)
			}
		}
		if p.Skipped != 4 || p.Unknown != 2 || p.Truncated != 0 || p.Frames != 3 {
			t.Errorf("split %d counters: skipped %d unknown %d truncated %d frames %d", i, p.Skipped, p.Unknown, p.Truncated, p.Frames)
		}
	}

	// Body helpers.
	if id, ok := ParseBeastReceiverID(idBody); !ok || id != 0x1A00_0000_DEAD_BE1A {
		t.Errorf("receiver ID %x %v", id, ok)
	}
	if v, ok := ParseBeastPing([]byte{1, 2, 3}); !ok || v != 0x010203 {
		t.Errorf("ping %x", v)
	}
	if _, ok := ParseBeastPing([]byte{1, 2}); ok {
		t.Error("short ping accepted")
	}
	if _, ok := ParseBeastReceiverID([]byte{1}); ok {
		t.Error("short ID accepted")
	}
	for _, typ := range []byte{BeastReceiverID, BeastUUID, BeastPing, BeastOption} {
		if !IsBeastEscapeType(typ) {
			t.Errorf("%x not an escape type", typ)
		}
	}
	if IsBeastEscapeType(BeastModeSLong) {
		t.Error("frame type reported as escape")
	}
}

func TestBeastParserNoHandlerStillSkips(t *testing.T) {
	var s []byte
	s = AppendBeastUUID(s, "abc")
	s = AppendBeastOption(s, BeastOptSlowDown)
	s = AppendBeastFrame(s, BeastModeSShort, 2000, 20, short1)
	frames, p := collectBeast(s)
	if len(frames) != 1 || p.Skipped != 2 || p.Garbage != 0 {
		t.Errorf("frames %d skipped %d garbage %d", len(frames), p.Skipped, p.Garbage)
	}
}

func TestNormalizeUUID(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"0F8C1E2A-7D3B-4C55-9A1A-1A2B3C4D5E6F", "0f8c1e2a7d3b4c559a1a1a2b3c4d5e6f", true},
		{"0f8c1e2a-7d3b-4c55-9a1a-1a2b3cffffff\x00", "0f8c1e2a7d3b4c559a1a1a2b3cffffff", true},
		{"0f8c1e2a7d3b4c559a1a1a2b3c4d5e6fffff", "0f8c1e2a7d3b4c559a1a1a2b3c4d5e6fffff", true}, // 36 hex, padded
		{"0f8c1e2a7d3b4c559a1a1a2b", "", false},                                                // 24 hex: too short
		{"00000000-0000-0000-0000-000000000000", "", false},                                    // null UUID
		{"0f8c1e2a-7d3b-4c55-9a1a-1a2b3c4d5e6g", "", false},                                    // not hex
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeUUID(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeUUID(%q) = %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
	var s []byte
	s = AppendBeastUUID(s, "0f8c1e2a-7d3b-4c55-9a1a-1a2b3c4d5e6f")
	_, escs, _ := collectEscapes(s)
	if u, ok := ParseBeastUUID(escs[0].body); !ok || u != "0f8c1e2a7d3b4c559a1a1a2b3c4d5e6f" {
		t.Errorf("ParseBeastUUID = %q %v", u, ok)
	}
	// Short UUIDs are padded with 'f' to the fixed length.
	if b := AppendBeastUUID(nil, "abc"); len(b) != 2+BeastUUIDLen || b[2+3] != 'f' {
		t.Errorf("padding: %q", b)
	}
}

func TestBeastPingRTT(t *testing.T) {
	sent := time.UnixMilli(1<<24*3 - 100) // 100 ms before the counter wraps
	v := BeastPingValue(sent)
	if rtt, ok := BeastPingRTT(v, sent.Add(250*time.Millisecond)); !ok || rtt != 250*time.Millisecond {
		t.Errorf("RTT across the wrap = %v %v", rtt, ok)
	}
	if _, ok := BeastPingRTT(v, sent.Add(2*time.Minute)); ok {
		t.Error("implausible RTT accepted")
	}
	// Echo of a received ping is the same three bytes.
	var s []byte
	s = AppendBeastPing(s, v)
	_, escs, _ := collectEscapes(s)
	echo := AppendBeastEscape(nil, BeastPing, escs[0].body)
	if !bytes.Equal(echo, s) {
		t.Errorf("echo %x != %x", echo, s)
	}
}
