package federation

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

// A published DF17 example (address 4840D6, CRC residual zero).
const df17Vector = "8D4840D6202CC371C32CE0576098"

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// withParity returns body plus the 24-bit parity field crc(body) ^ ap.
func withParity(body []byte, ap uint32) []byte {
	p := crc24(body) ^ ap
	return append(append([]byte(nil), body...), byte(p>>16), byte(p>>8), byte(p))
}

func TestCRCAndAddress(t *testing.T) {
	v := mustHex(df17Vector)
	if crc24(v[:11]) != uint32(v[11])<<16|uint32(v[12])<<8|uint32(v[13]) {
		t.Fatal("CRC of the published DF17 example")
	}
	if a, ok := frameAddr(v); !ok || a != 0x4840D6 {
		t.Fatalf("DF17 %06X %v", a, ok)
	}
	bad := append([]byte(nil), v...)
	bad[5] ^= 1
	if _, ok := frameAddr(bad); ok {
		t.Fatal("corrupted DF17 accepted")
	}
	df4 := withParity([]byte{4 << 3, 0x00, 0x0F, 0x12}, 0x4840D6) // DF4, AP = address
	if a, ok := frameAddr(df4); !ok || a != 0x4840D6 {
		t.Fatalf("DF4 %06X", a)
	}
	df11 := withParity([]byte{11<<3 | 5, 0x48, 0x40, 0xD6}, 0x000003) // II code in the low bits
	if a, ok := frameAddr(df11); !ok || a != 0x4840D6 {
		t.Fatalf("DF11 %06X %v", a, ok)
	}
	if _, ok := frameAddr(withParity([]byte{11 << 3, 0x48, 0x40, 0xD6}, 0x001000)); ok {
		t.Fatal("DF11 with a large residual accepted")
	}
	if _, ok := frameAddr([]byte{0xC8, 1, 2, 3, 4, 5, 6}); ok { // DF25, short: not forwarded
		t.Fatal("DF25 accepted")
	}
}

type beastMsg struct {
	typ  byte
	ts   [6]byte
	sig  byte
	data []byte
}

// parseBeast decodes a Beast stream written by appendBeast (test helper).
func parseBeast(t *testing.T, b []byte) []beastMsg {
	t.Helper()
	var out []beastMsg
	un := func(i int) (byte, int) {
		if b[i] == beastEsc {
			if i+1 >= len(b) || b[i+1] != beastEsc {
				t.Fatalf("bad escape at %d", i)
			}
			return beastEsc, i + 2
		}
		return b[i], i + 1
	}
	for i := 0; i < len(b); {
		if b[i] != beastEsc {
			t.Fatalf("no sync at %d", i)
		}
		m := beastMsg{typ: b[i+1]}
		i += 2
		n := map[byte]int{beastModeAC: 2, beastShort: 7, beastLong: 14}[m.typ]
		if n == 0 {
			t.Fatalf("type %q", m.typ)
		}
		for k := range 6 {
			m.ts[k], i = un(i)
		}
		m.sig, i = un(i)
		for range n {
			var c byte
			c, i = un(i)
			m.data = append(m.data, c)
		}
		out = append(out, m)
	}
	return out
}

func TestBeastOutFeed(t *testing.T) {
	id := newID(t)
	low := plane("400001", "LOW", 53.48, -2.24, 500)
	a := startNode(t, id, "a", "", Config{Feeds: []FeedConfig{{Name: "bx", Format: FormatBeast, Mode: ModeListen, Addr: "127.0.0.1:0", ModeAC: true}}},
		Options{Guard: func(a *model.Aircraft) bool { return a.ICAO == "400001" }})
	a.pic.set(plane("4840D6", "KLM", 52, 4, 3000), low)
	waitFor(t, 3*time.Second, "picture", func() bool { return a.Status().Feeds[0].Aircraft == 1 })
	c, err := net.Dial("tcp", a.FeedAddr("bx").String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, 3*time.Second, "client", func() bool { return a.Status().Feeds[0].Clients == 1 })

	v := mustHex(df17Vector)
	df4 := withParity([]byte{4 << 3, 0x00, 0x0F, 0x1A}, 0x4840D6) // contains 0x1A: escaped
	unknown := withParity([]byte{4 << 3, 0, 0, 0}, 0x123456)
	island := withParity([]byte{4 << 3, 0, 0, 0}, 0x400001)
	now := time.Now()
	frames := []model.RawFrame{
		{Kind: model.FrameModeSLong, Data: v, Clock12MHz: 123456789, Signal: -3, Recv: now},
		{Kind: model.FrameModeSLong, Data: v, Clock12MHz: 123456999, Signal: -9, Station: 7, Recv: now}, // duplicate (another station)
		{Kind: model.FrameModeSShort, Data: df4, GPSNs: 5_000_000_000, Recv: now},
		{Kind: model.FrameModeSShort, Data: unknown, Recv: now},
		{Kind: model.FrameModeSShort, Data: island, Recv: now},
		{Kind: model.FrameModeSLong, Data: v[:13], Recv: now},
		{Kind: model.FrameModeSLong, Data: append(append([]byte(nil), v[:10]...), 0, 0, 0, 0), MLAT: true, Recv: now},
		{Kind: model.FrameModeSLong, Data: v, Late: true, Recv: now},
		{Kind: model.FrameStatus, Data: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, Recv: now},
		{Kind: model.FrameModeAC, Data: []byte{0x12, 0x34}, Recv: now},
	}
	for _, f := range frames {
		a.HandleFrame(f)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for buf.Len() < 3*9+14+7+2+1 {
		n, err := c.Read(tmp)
		if err != nil {
			t.Fatalf("read: %v (%d bytes)", err, buf.Len())
		}
		buf.Write(tmp[:n])
	}
	msgs := parseBeast(t, buf.Bytes())
	if len(msgs) != 3 {
		t.Fatalf("%d frames out", len(msgs))
	}
	want := [][]byte{v, df4, {0x12, 0x34}}
	for i, m := range msgs {
		if !bytes.Equal(m.data, want[i]) || m.ts != [6]byte{} || m.sig != 0 {
			t.Fatalf("frame %d: %+v", i, m)
		}
	}
	st := a.Status().Feeds[0]
	if st.FramesIn != uint64(len(frames)) || st.FramesOut != 3 || st.Dupes != 1 || st.Filtered != 6 {
		t.Fatalf("counters %+v", st)
	}
	// Non-loopback clients are refused by the default allow list: checked
	// on the predicate (the test cannot dial from another address).
	f := a.feeds[0]
	if f.permitted(&net.TCPAddr{IP: net.ParseIP("192.0.2.9"), Port: 1}) || !f.permitted(&net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.1"), Port: 1}) {
		t.Fatal("allow list")
	}
}

func TestJSONOutFeedConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	id := newID(t)
	a := startNode(t, id, "a", "", Config{Feeds: []FeedConfig{{Name: "js", Format: FormatJSON, Mode: ModeConnect,
		Addr: ln.Addr().String(), IntervalMs: 100, Policy: Policy{MinAltFt: 1000}}}}, Options{})
	a.pic.set(leakyLocal()...)
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReader(c)
	var file publicdto.TarAircraftFile
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		fx := fixture()
		if leaks := fx.Scan(line); len(leaks) > 0 {
			t.Fatalf("leaks %v in %s", leaks, line)
		}
		if err := json.Unmarshal(line, &file); err != nil {
			t.Fatal(err)
		}
		if len(file.Aircraft) > 0 {
			break
		}
	}
	got := map[string]bool{}
	for _, x := range file.Aircraft {
		got[x.Hex] = true
	}
	// 40abcd is at 600 ft (below the floor), aaaaaa is federation-only.
	if !got["4ca123"] || !got["4ca999"] || got["40abcd"] || got["aaaaaa"] || len(got) != 2 {
		t.Fatalf("aircraft %v", got)
	}
	if !a.Status().Feeds[0].Connected {
		t.Fatal("status not connected")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 0))
}
