package feed

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

type avrGot struct {
	prefix byte
	hasTS  bool
	ts     uint64
	hasSig bool
	sig    byte
	data   []byte
}

func collectAVR(chunks ...string) ([]avrGot, *AVRParser) {
	var out []avrGot
	p := NewAVRParser(func(f *AVRFrame) {
		out = append(out, avrGot{f.Prefix, f.HasTimestamp, f.Timestamp, f.HasSignal, f.Signal, append([]byte(nil), f.Data...)})
	})
	for _, c := range chunks {
		p.Write([]byte(c))
	}
	return out, p
}

// avrSample mixes every AVR dialect, as seen on dump1090 port 30002, a
// Mode-S Beast in AVR mode (examples from the jetvision wiki) and readsb's
// extended format, with junk in between.
const avrSample = "*8D4840D6202CC371C32CE0576098;\r\n" +
	"*02E99619FACDAE;\n" +
	"*7700;\n" +
	"@016CE3671C7423FFE7AB7BFCAB;\r\n" +
	"%00000A1B2C3D8D40621D58C382D690C8AC2863A7;\n" +
	"<FF004D4C4154A08D4840D6202CC371C32CE0576098;\n" +
	":5d4840d6202cc3;" + // lower case, no line break
	"*8D4840D6202CC371C32CE05760;\n" + // wrong length
	"*8D4840D6202CC371\n" + // never terminated
	"hello world\r\n" +
	"@016CE3671C74;\n" + // timestamp only
	"*5D4840D6202CC3;\n"

func TestAVRParser(t *testing.T) {
	want := []avrGot{
		{'*', false, 0, false, 0, long1},
		{'*', false, 0, false, 0, []byte{0x02, 0xE9, 0x96, 0x19, 0xFA, 0xCD, 0xAE}},
		{'*', false, 0, false, 0, []byte{0x77, 0x00}},
		{'@', true, 0x016CE3671C74, false, 0, []byte{0x23, 0xFF, 0xE7, 0xAB, 0x7B, 0xFC, 0xAB}},
		{'%', true, 0x00000A1B2C3D, false, 0, []byte{0x8D, 0x40, 0x62, 0x1D, 0x58, 0xC3, 0x82, 0xD6, 0x90, 0xC8, 0xAC, 0x28, 0x63, 0xA7}},
		{'<', true, 0xFF004D4C4154, true, 0xA0, long1},
		{':', false, 0, false, 0, short1},
		{'*', false, 0, false, 0, short1},
	}
	check := func(label string, got []avrGot) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %d frames, want %d", label, len(got), len(want))
		}
		for i, w := range want {
			g := got[i]
			if g.prefix != w.prefix || g.hasTS != w.hasTS || g.ts != w.ts || g.hasSig != w.hasSig || g.sig != w.sig || !bytes.Equal(g.data, w.data) {
				t.Fatalf("%s: frame %d = %+v, want %+v", label, i, g, w)
			}
		}
	}
	got, p := collectAVR(avrSample)
	check("whole", got)
	if p.Garbage != 3 {
		t.Errorf("garbage = %d, want 3", p.Garbage)
	}
	for i := 0; i <= len(avrSample); i++ {
		got, _ = collectAVR(avrSample[:i], avrSample[i:])
		check("split", got)
	}
	n := 0
	q := NewAVRParser(func(*AVRFrame) { n++ })
	b := []byte(avrSample)
	if a := testing.AllocsPerRun(100, func() { q.Write(b) }); a != 0 {
		t.Errorf("AVR parser allocates %v times per pass", a)
	}
}

func TestAVRDecoder(t *testing.T) {
	out := &sink{}
	d := &AVRDecoder{Sink: out, FeedID: "avr"}
	d.Recv = time.Unix(1_800_000_000, 0)
	NewAVRParser(d.Handle).Write([]byte(avrSample))
	if len(out.frames) != 8 {
		t.Fatalf("got %d frames", len(out.frames))
	}
	kinds := []model.FrameKind{model.FrameModeSLong, model.FrameModeSShort, model.FrameModeAC, model.FrameModeSShort,
		model.FrameModeSLong, model.FrameModeSLong, model.FrameModeSShort, model.FrameModeSShort}
	for i, f := range out.frames {
		if f.Kind != kinds[i] || f.FeedID != "avr" || f.Channel != -1 || !f.Recv.Equal(d.Recv) {
			t.Errorf("frame %d = %+v", i, f)
		}
		// Timestamp mode is not established by a handful of frames.
		if f.GPSNs != -1 || f.Clock12MHz != 0 {
			t.Errorf("frame %d carries a timestamp before detection: %+v", i, f)
		}
	}
	mlat := out.frames[5]
	if !mlat.MLAT || math.Abs(mlat.Signal-20*math.Log10(160.0/255)) > 1e-9 {
		t.Errorf("'<' frame: MLAT %v signal %v", mlat.MLAT, mlat.Signal)
	}
	if out.frames[0].MLAT || !math.IsNaN(out.frames[0].Signal) {
		t.Errorf("'*' frame: %+v", out.frames[0])
	}

	// With a steady stream the 12 MHz counter is recognised.
	out2 := &sink{}
	d2 := &AVRDecoder{Sink: out2}
	p2 := NewAVRParser(d2.Handle)
	t0 := time.Unix(1_800_000_000, 0)
	var last uint64
	for i := 0; i < 400; i++ {
		el := time.Duration(i) * 20 * time.Millisecond
		d2.Recv = t0.Add(el)
		last = 0x000012345678 + uint64(el.Seconds()*12e6)
		p2.Write([]byte("@" + strings.ToUpper(hex12(last)) + "5D4840D6202CC3;\n"))
	}
	if d2.TS.Mode() != Timestamp12MHz {
		t.Fatalf("mode = %v", d2.TS.Mode())
	}
	if f := out2.frames[len(out2.frames)-1]; f.Clock12MHz != last || f.GPSNs != -1 {
		t.Errorf("last frame = %+v, want clock %#x", f, last)
	}
}

func hex12(v uint64) string { return fmt.Sprintf("%012x", v) }

func TestParseSBS(t *testing.T) {
	type exp struct {
		icao             uint32
		mlat             bool
		callsign, squawk string
		lat, lon         float64
		alt, gs, trk, vr float64
		ground           int // -1 absent, 0 false, 1 true
	}
	nan := math.NaN()
	cases := []struct {
		line string
		ok   bool
		want exp
	}{
		{"MSG,1,1,1,4840D6,1,2026/11/11,12:00:00.412,2026/11/11,12:00:00.440,KLM1023 ,,,,,,,,,,,0", true,
			exp{0x4840D6, false, "KLM1023", "", nan, nan, nan, nan, nan, nan, 0}},
		{"MSG,3,1,1,4840D6,1,2026/11/11,12:00:01.123,2026/11/11,12:00:01.150,,38000,,,52.25720,3.91937,,,0,,0,0", true,
			exp{0x4840D6, false, "", "", 52.2572, 3.91937, 38000, nan, nan, nan, 0}},
		{"MSG,4,1,1,4840D6,1,2026/11/11,12:00:01.523,2026/11/11,12:00:01.560,,,447,271,,,-64,,,,,0", true,
			exp{0x4840D6, false, "", "", nan, nan, nan, 447, 271, -64, 0}},
		{"MSG,6,1,1,4840D6,1,2026/11/11,12:00:02.001,2026/11/11,12:00:02.050,,38000,,,,,,7000,0,0,0,0", true,
			exp{0x4840D6, false, "", "7000", nan, nan, 38000, nan, nan, nan, 0}},
		{"MSG,5,1,1,3c6dd2,1,2026/11/11,12:00:02.101,2026/11/11,12:00:02.150,DLH4AB,11025,,,,,,,0,,0,0\r", true,
			exp{0x3C6DD2, false, "DLH4AB", "", nan, nan, 11025, nan, nan, nan, 0}},
		{"MSG,2,1,1,400F2B,1,2026/11/11,12:00:03.000,2026/11/11,12:00:03.010,,0,12.5,93.2,51.47012,-0.45432,,,,,,-1", true,
			exp{0x400F2B, false, "", "", 51.47012, -0.45432, 0, 12.5, 93.2, nan, 1}},
		{"MSG,8,1,1,A1B2C3,1,2026/11/11,12:00:03.100,2026/11/11,12:00:03.110,,,,,,,,,,,,0", true,
			exp{0xA1B2C3, false, "", "", nan, nan, nan, nan, nan, nan, 0}},
		{"MSG,7,1,1,A1B2C3,1,2026/11/11,12:00:03.100,2026/11/11,12:00:03.110,,35000,,,,,,,,,,", true,
			exp{0xA1B2C3, false, "", "", nan, nan, 35000, nan, nan, nan, -1}},
		// Radarcape MLAT position message.
		{"MSG,10,1,1,3C4AD1,1,2026/11/11,12:00:04.000,2026/11/11,12:00:04.020,,24000,310,45,48.35310,11.78610,0,,,,,0", true,
			exp{0x3C4AD1, true, "", "", 48.3531, 11.7861, 24000, 310, 45, 0, 0}},
		// readsb non-ICAO address, negative track normalised.
		{"MSG,3,1,1,~2A1B3C,1,2026/11/11,12:00:04.000,2026/11/11,12:00:04.020,,1200,,-10,40.0,-74.0,,,,,,0", true,
			exp{0x2A1B3C | NonICAOFlag, false, "", "", 40, -74, 1200, nan, 350, nan, 0}},
		// Short line (trailing fields missing) is still usable.
		{"MSG,1,1,1,4840D6,1,,,,,TEST", true, exp{0x4840D6, false, "TEST", "", nan, nan, nan, nan, nan, nan, -1}},
		// 0,0 position and out-of-range values are dropped, the rest kept.
		{"MSG,3,1,1,4840D6,1,,,,,,999999,,,0.0,0.0,,,,,,0", true, exp{0x4840D6, false, "", "", nan, nan, nan, nan, nan, nan, 0}},

		{"MSG,9,1,1,DD1234,1,2026/11/11,12:00:04.000,2026/11/11,12:00:04.020,,1500,60,180,47.9,11.2,,,,,,0", false, exp{}}, // FLARM
		{"STA,,5,179,400AE7,10103,2008/11/28,14:58:51.153,2008/11/28,14:58:51.153,RM", false, exp{}},
		{"SEL,,496,2286,4CA4E5,27215,2010/02/19,18:06:07.710,2010/02/19,18:06:07.710,RYR1427", false, exp{}},
		{"MSG,3,1,1,XYZ,1,,,,,,,,,,,,,,,,", false, exp{}},
		{"MSG,3,1,1,000000,1,,,,,,,,,,,,,,,,", false, exp{}},
		{"MSG,x,1,1,4840D6", false, exp{}},
		{"MSG,3", false, exp{}},
		{"", false, exp{}},
	}
	fEq := func(p *float64, w float64) bool {
		if math.IsNaN(w) {
			return p == nil
		}
		return p != nil && math.Abs(*p-w) < 1e-9
	}
	sEq := func(p *string, w string) bool {
		if w == "" {
			return p == nil
		}
		return p != nil && *p == w
	}
	for _, c := range cases {
		u, ok := ParseSBSLine([]byte(c.line))
		if ok != c.ok {
			t.Errorf("%q: ok = %v, want %v", c.line, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		w := c.want
		ground := -1
		if u.OnGround != nil {
			ground = 0
			if *u.OnGround {
				ground = 1
			}
		}
		if u.ICAO != w.icao || u.MLAT != w.mlat || u.Msgs != 1 || !sEq(u.Callsign, w.callsign) || !sEq(u.Squawk, w.squawk) ||
			!fEq(u.Lat, w.lat) || !fEq(u.Lon, w.lon) || !fEq(u.AltBaro, w.alt) || !fEq(u.GS, w.gs) ||
			!fEq(u.Track, w.trk) || !fEq(u.VertRate, w.vr) || ground != w.ground {
			t.Errorf("%q:\n got %s\nwant %+v", c.line, dumpUpdate(u), w)
		}
	}
}

func dumpUpdate(u model.DecodedUpdate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{icao=%06X msgs=%d", u.ICAO, u.Msgs)
	if u.MLAT {
		b.WriteString(" mlat")
	}
	str := func(n string, p *string) {
		if p != nil {
			fmt.Fprintf(&b, " %s=%q", n, *p)
		}
	}
	num := func(n string, p *float64) {
		if p != nil {
			fmt.Fprintf(&b, " %s=%g", n, *p)
		}
	}
	str("cs", u.Callsign)
	str("sq", u.Squawk)
	num("lat", u.Lat)
	num("lon", u.Lon)
	num("alt", u.AltBaro)
	num("altg", u.AltGeom)
	num("gs", u.GS)
	num("trk", u.Track)
	num("vr", u.VertRate)
	num("sig", u.Signal)
	if u.OnGround != nil {
		fmt.Fprintf(&b, " ground=%v", *u.OnGround)
	}
	b.WriteString("}")
	return b.String()
}

func TestSBSDecoderStream(t *testing.T) {
	stream := "MSG,3,1,1,4840D6,1,2026/11/11,12:00:01.123,2026/11/11,12:00:01.150,,38000,,,52.25720,3.91937,,,0,,0,0\r\n" +
		"\r\n" +
		"CLK,,496,-1,,-1,2010/02/19,18:18:19.036,2010/02/19,18:18:19.036\r\n" +
		"MSG,1,1,1,4840D6,1,2026/11/11,12:00:00.412,2026/11/11,12:00:00.440,KLM1023 ,,,,,,,,,,,0\n" +
		"MSG,3,1,1,4840D6,1," + strings.Repeat("x", 2000) + "\n" + // overlong line is discarded
		"MSG,4,1,1,3C6DD2,1,2026/11/11,12:00:01.523,2026/11/11,12:00:01.560,,,447,271,,,-64,,,,,0\n"
	recv := time.Unix(1_800_000_000, 0)
	for split := 0; split <= len(stream); split += 7 {
		out := &sink{}
		d := &SBSDecoder{Sink: out, FeedID: "sbs", MLATOnly: true, Recv: recv}
		d.Write([]byte(stream[:split]))
		d.Write([]byte(stream[split:]))
		if len(out.updates) != 3 || d.Updates != 3 {
			t.Fatalf("split %d: %d updates, want 3", split, len(out.updates))
		}
		if d.Ignored != 1 || d.split.Overlong != 1 {
			t.Fatalf("split %d: ignored %d overlong %d", split, d.Ignored, d.split.Overlong)
		}
		u := out.updates[2]
		if u.ICAO != 0x3C6DD2 || u.FeedID != "sbs" || !u.MLAT || !u.Recv.Equal(recv) || u.GS == nil || *u.GS != 447 {
			t.Fatalf("split %d: last update %s", split, dumpUpdate(u))
		}
	}
}
