package sim

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"airview/internal/model"
)

func TestGPSTimestamp(t *testing.T) {
	// worked example from the Jetvision wiki (Radarcape: Firmware Versions):
	// 0x244bbb9ac9f0 = second 37166 of the day (10:19:26), 999999984 ns
	if got := gpsTimestamp(37166*1e9 + 999999984); got != 0x244bbb9ac9f0 {
		t.Errorf("gpsTimestamp = %012x, want 244bbb9ac9f0", got)
	}
	if got := gpsTimestamp(0); got != 0 {
		t.Errorf("midnight = %x", got)
	}
	last := gpsTimestamp(86400*1e9 - 1)
	if last>>30 != 86399 || last&(1<<30-1) != 999999999 || last >= 1<<48 {
		t.Errorf("last nanosecond of the day = %012x", last)
	}
}

func TestSignalByte(t *testing.T) {
	for _, c := range []struct {
		dbfs float64
		want byte
	}{
		{0, 255}, {3, 255}, {-10, 81}, {-30, 8}, {-40, 3}, {-60, 1}, {-200, 1}, {math.NaN(), 0},
	} {
		if got := signalByte(c.dbfs); got != c.want {
			t.Errorf("signalByte(%v) = %d, want %d", c.dbfs, got, c.want)
		}
	}
	// the byte reads back as 20*log10(byte/255), the dump1090 / readsb convention
	for db := -35.0; db <= 0; db += 0.7 {
		back := 20 * math.Log10(float64(signalByte(db))/255)
		if math.Abs(back-db) > 1.3 {
			t.Errorf("%v dBFS reads back as %.2f", db, back)
		}
	}
}

func TestBeastEscaping(t *testing.T) {
	// A frame in which the timestamp, the signal byte and the data all
	// contain 0x1A: second 104 of the day and 0x1A1A ns give the timestamp
	// 0x001A00001A1A; -19.83 dBFS is signal byte 26 = 0x1A.
	data := []byte{0x8D, 0x1A, 0x1A, 0x00, 0x1A, 0x58, 0x1A, 0x1A, 0x1A, 0x33, 0x1A, 0x44, 0x55, 0x1A}
	gps := int64(104*1e9 + 0x1A1A)
	f := model.RawFrame{Kind: model.FrameModeSLong, Data: data, GPSNs: gps, Signal: -19.83, Channel: 0}
	if signalByte(f.Signal) != 0x1A {
		t.Fatalf("test setup: signal byte %#x", signalByte(f.Signal))
	}
	ts := gpsTimestamp(gps)
	if ts != 0x001A00001A1A {
		t.Fatalf("test setup: timestamp %012x", ts)
	}
	esc := 3 + 1 // timestamp + signal
	esc += bytes.Count(data, []byte{0x1A})

	out := AppendBeastFrame([]byte{0xEE}, &f)
	if out[0] != 0xEE {
		t.Fatal("AppendBeastFrame does not append")
	}
	out = out[1:]
	if want := 2 + 6 + 1 + 14 + esc; len(out) != want {
		t.Errorf("encoded length %d, want %d (%d escapes)", len(out), want, esc)
	}
	if out[0] != 0x1A || out[1] != '3' {
		t.Errorf("frame start % X", out[:2])
	}
	// after the frame start every 0x1A must come in pairs
	for i := 2; i < len(out); i++ {
		if out[i] == 0x1A {
			if i+1 >= len(out) || out[i+1] != 0x1A {
				t.Fatalf("lone 0x1A at offset %d: % X", i, out)
			}
			i++
		}
	}
	frames, rest, bad := refParseBeast(out)
	if len(frames) != 1 || len(rest) != 0 || bad != 0 {
		t.Fatalf("parsed %d frames, %d bytes left, %d errors", len(frames), len(rest), bad)
	}
	if g := frames[0]; g.typ != '3' || g.ts != ts || g.sig != 0x1A || !bytes.Equal(g.data, data) {
		t.Errorf("round trip: %+v", g)
	}

	// short frame, status frame, MLAT result, unknown kinds
	short := model.RawFrame{Kind: model.FrameModeSShort, Data: data[:7], GPSNs: gps, Signal: -3}
	status := model.RawFrame{Kind: model.FrameStatus, Data: data, GPSNs: 26e9, Signal: math.NaN()}
	mlat := model.RawFrame{Kind: model.FrameModeSLong, Data: data, GPSNs: -1, Signal: math.NaN(), MLAT: true}
	legacy := model.RawFrame{Kind: model.FrameModeSShort, Data: data[:7], GPSNs: -1, Clock12MHz: 0x0102031A0506, Signal: -3}
	ac := model.RawFrame{Kind: model.FrameModeAC, Data: []byte{0x1A, 0x20}, GPSNs: gps, Signal: -3}
	var buf []byte
	for _, fr := range []*model.RawFrame{&short, &status, &mlat, &legacy, &ac} {
		buf = AppendBeastFrame(buf, fr)
	}
	n := len(buf)
	buf = AppendBeastFrame(buf, &model.RawFrame{Kind: model.FrameModeSLong, Data: data[:7]}) // wrong length
	buf = AppendBeastFrame(buf, &model.RawFrame{Kind: 99, Data: data})
	if len(buf) != n {
		t.Error("malformed frames must be skipped")
	}
	frames, rest, bad = refParseBeast(buf)
	if len(frames) != 5 || len(rest) != 0 || bad != 0 {
		t.Fatalf("parsed %d frames, %d bytes left, %d errors", len(frames), len(rest), bad)
	}
	if g := frames[0]; g.typ != '2' || g.ts != ts || !bytes.Equal(g.data, data[:7]) {
		t.Errorf("short frame: %+v", g)
	}
	if g := frames[1]; g.typ != '4' || g.ts != 26<<30 || g.sig != 0 || !bytes.Equal(g.data, data) {
		t.Errorf("status frame: %+v", g)
	}
	if g := frames[2]; g.typ != '3' || g.ts != 0xFF004D4C4154 || g.sig != 0 {
		t.Errorf("MLAT frame: %+v", g)
	}
	if g := frames[3]; g.typ != '2' || g.ts != 0x0102031A0506 {
		t.Errorf("12 MHz timestamp frame: %+v", g)
	}
	if g := frames[4]; g.typ != '1' || !bytes.Equal(g.data, []byte{0x1A, 0x20}) {
		t.Errorf("Mode A/C frame: %+v", g)
	}
}

// TestBeastStreamRoundTrip encodes a few minutes of simulated traffic and
// parses it back with the independent parser.
func TestBeastStreamRoundTrip(t *testing.T) {
	s := New(Config{Aircraft: 60, Seed: 31, Start: testStart, NoDiurnal: true})
	var all []model.RawFrame
	var buf []byte
	wantLen, escaped := 0, 0
	sink := funcSink(func(f model.RawFrame) {
		all = append(all, f)
		before := len(buf)
		buf = AppendBeastFrame(buf, &f)
		ts := gpsTimestamp(f.GPSNs)
		sig := signalByte(f.Signal)
		if f.MLAT {
			ts, sig = 0xFF004D4C4154, 0
		}
		if f.Kind == model.FrameStatus {
			sig = 0
		}
		e := bytes.Count(f.Data, []byte{0x1A})
		if sig == 0x1A {
			e++
		}
		for i := 0; i < 6; i++ {
			if byte(ts>>(8*i)) == 0x1A {
				e++
			}
		}
		if e > 0 {
			escaped++
		}
		wantLen += 2 + 6 + 1 + len(f.Data) + e
		if len(buf)-before != 2+6+1+len(f.Data)+e {
			t.Fatalf("frame encoded in %d bytes, want %d", len(buf)-before, 2+6+1+len(f.Data)+e)
		}
	})
	if err := s.Advance(context.Background(), 3*time.Minute, sink); err != nil {
		t.Fatal(err)
	}
	frames, rest, bad := refParseBeast(buf)
	t.Logf("%d frames, %d bytes, %d frames needed escaping", len(all), len(buf), escaped)
	if len(frames) != len(all) || len(rest) != 0 || bad != 0 || len(buf) != wantLen {
		t.Fatalf("parsed %d of %d frames, %d bytes left, %d framing errors", len(frames), len(all), len(rest), bad)
	}
	if escaped < 100 {
		t.Errorf("only %d frames exercised the escaping", escaped)
	}
	for i, g := range frames {
		f := all[i]
		if !bytes.Equal(g.data, f.Data) {
			t.Fatalf("frame %d: data %X, want %X", i, g.data, f.Data)
		}
		switch {
		case f.Kind == model.FrameStatus:
			if g.typ != '4' || g.ts != gpsTimestamp(f.GPSNs) || g.ts&(1<<30-1) != 0 {
				t.Fatalf("frame %d: status %+v", i, g)
			}
		case f.MLAT:
			if g.typ != '3' || g.ts != 0xFF004D4C4154 {
				t.Fatalf("frame %d: MLAT %+v", i, g)
			}
		default:
			wantTyp := byte('2')
			if len(f.Data) == 14 {
				wantTyp = '3'
			}
			sod, ns := g.ts>>30, g.ts&(1<<30-1)
			if g.typ != wantTyp || int64(sod)*1e9+int64(ns) != f.GPSNs || ns >= 1e9 || sod >= 86400 {
				t.Fatalf("frame %d: type %c timestamp %012x, want GPSNs %d", i, g.typ, g.ts, f.GPSNs)
			}
			if db := 20 * math.Log10(float64(g.sig)/255); g.sig == 0 || g.sig != signalByte(f.Signal) || math.Abs(db-f.Signal) > 2.5 {
				t.Fatalf("frame %d: signal byte %d (%.1f dBFS), want %.1f dBFS", i, g.sig, db, f.Signal)
			}
		}
	}
}

func TestBeastStreamSelection(t *testing.T) {
	d1 := []byte{0x5D, 1, 2, 3, 4, 5, 6}
	d2 := []byte{0x8D, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
	d3 := []byte{0x90, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
	st := make([]byte, 14)
	batch := []model.RawFrame{
		{Kind: model.FrameStatus, Data: st, GPSNs: 30e9, Channel: -1},                       // 00:00:30: followed by a position frame
		{Kind: model.FrameModeSShort, Data: d1, GPSNs: 30e9 + 100, Signal: -20, Channel: 0}, // heard on both channels,
		{Kind: model.FrameModeSShort, Data: d1, GPSNs: 30e9 + 100, Signal: -12, Channel: 1}, // better on channel 1
		{Kind: model.FrameModeSLong, Data: d2, GPSNs: 30e9 + 200, Signal: -25, Channel: 0},  // channel 0 only
		{Kind: model.FrameModeSLong, Data: d3, GPSNs: -1, Signal: math.NaN(), Channel: -1, MLAT: true},
		// the same bytes transmitted again: a separate reception with its
		// own backing array, which must not be merged with the earlier one
		{Kind: model.FrameModeSLong, Data: append([]byte(nil), d2...), GPSNs: 30e9 + 900, Signal: -9, Channel: 1},
		{Kind: model.FrameStatus, Data: st, GPSNs: 31e9, Channel: -1}, // no position frame this second
	}

	parse := func(s beastStream) []refBeastFrame {
		t.Helper()
		frames, rest, bad := refParseBeast(s.appendBatch(nil, batch))
		if len(rest) != 0 || bad != 0 {
			t.Fatalf("framing: %d bytes left, %d errors", len(rest), bad)
		}
		return frames
	}
	types := func(fr []refBeastFrame) string {
		var b []byte
		for _, f := range fr {
			b = append(b, f.typ)
		}
		return string(b)
	}

	var pos [21]byte
	binary.LittleEndian.PutUint32(pos[4:], math.Float32bits(50.05))
	combined := parse(beastStream{channel: -1, pos: pos})
	if got := types(combined); got != "4523334" {
		t.Fatalf("combined stream frame types %q, want 4523334", got)
	}
	if combined[2].sig != signalByte(-12) {
		t.Errorf("combined stream did not pick the stronger channel: signal byte %d", combined[2].sig)
	}
	if lat := math.Float32frombits(binary.LittleEndian.Uint32(combined[1].data[4:])); lat != 50.05 {
		t.Errorf("position frame latitude %v", lat)
	}
	if got := types(parse(beastStream{channel: 0, noPosition: true})); got != "42334" {
		t.Errorf("channel 0 stream %q, want 42334", got)
	}
	ch1 := parse(beastStream{channel: 1, noPosition: true, noMLAT: true})
	if got := types(ch1); got != "4234" {
		t.Errorf("channel 1 stream without MLAT %q, want 4234", got)
	}
	if ch1[1].sig != signalByte(-12) || ch1[2].sig != signalByte(-9) {
		t.Errorf("channel 1 signal bytes %d %d", ch1[1].sig, ch1[2].sig)
	}
}

func readBeast(t *testing.T, conn net.Conn, minBytes int, timeout time.Duration) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 0, minBytes+65536)
	tmp := make([]byte, 32768)
	for len(buf) < minBytes {
		n, err := conn.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			t.Fatalf("read after %d bytes: %v", len(buf), err)
		}
	}
	return buf
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestServeBeastTCP connects to the Beast server like a feed client would.
func TestServeBeastTCP(t *testing.T) {
	s := New(Config{Aircraft: 60, Seed: 77, Start: testStart, Speed: 60, NoDiurnal: true})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.ServeBeastOn(ctx, ln, BeastOptions{}) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// a Radarcape accepts option commands on this port: they must be ignored
	conn.Write([]byte{0x1A, '1', 'C', 0x1A, '1', 'G'})
	waitFor(t, "client registration", func() bool { return s.Stats().BeastClients == 1 })

	buf := readBeast(t, conn, 300_000, 10*time.Second)
	frames, _, bad := refParseBeast(buf)
	if bad != 0 {
		t.Fatalf("%d framing errors in %d bytes", bad, len(buf))
	}
	count := map[byte]int{}
	var lastTS uint64
	var mlat int
	st := s.Station()
	for i, f := range frames {
		count[f.typ]++
		switch f.typ {
		case '2', '3':
			if f.ts == 0xFF004D4C4154 {
				mlat++
				if refDF(f.data) != 18 || refRemainder(f.data) != 0 {
					t.Fatalf("frame %d: bad MLAT result %X", i, f.data)
				}
				continue
			}
			sod, ns := f.ts>>30, f.ts&(1<<30-1)
			if sod >= 86400 || ns >= 1e9 || f.ts < lastTS {
				t.Fatalf("frame %d: timestamp %012x after %012x", i, f.ts, lastTS)
			}
			lastTS = f.ts
			if f.sig == 0 {
				t.Fatalf("frame %d: signal byte 0", i)
			}
			df := refDF(f.data)
			if (df >= 16) != (f.typ == '3') {
				t.Fatalf("frame %d: DF%d in a type %c frame", i, df, f.typ)
			}
			if (df == 11 || df == 17) && refRemainder(f.data) != 0 {
				t.Fatalf("frame %d: DF%d fails the CRC: %X", i, df, f.data)
			}
		case '4':
			if f.data[0]&0x10 == 0 || f.ts&(1<<30-1) != 0 || f.ts>>30 >= 86400 {
				t.Fatalf("frame %d: status %X at %012x", i, f.data, f.ts)
			}
		case '5':
			lat := math.Float32frombits(binary.LittleEndian.Uint32(f.data[4:]))
			lon := math.Float32frombits(binary.LittleEndian.Uint32(f.data[8:]))
			alt := math.Float32frombits(binary.LittleEndian.Uint32(f.data[12:]))
			if math.Abs(float64(lat)-st.Lat) > 1e-4 || math.Abs(float64(lon)-st.Lon) > 1e-4 || math.Abs(float64(alt)-st.GroundElevM-8) > 0.01 {
				t.Fatalf("frame %d: receiver position %v %v %v", i, lat, lon, alt)
			}
		default:
			t.Fatalf("frame %d: unexpected type %c", i, f.typ)
		}
	}
	t.Logf("%d bytes, frames by type %v, %d MLAT results", len(buf), map[string]int{
		"short": count['2'], "long": count['3'], "status": count['4'], "position": count['5']}, mlat)
	if count['2'] < 500 || count['3'] < 500 || count['4'] < 5 || count['5'] < 1 {
		t.Errorf("frame mix: %v", count)
	}
	// virtual time of the stream agrees with the simulator clock
	if sod := int64(lastTS >> 30); sod < 9*3600+30*60 || sod > int64(s.Now().Sub(s.Now().Truncate(24*time.Hour))/time.Second)+1 {
		t.Errorf("last timestamp second %d, simulator time %v", sod, s.Now())
	}

	// a second client gets the stream too; closing it is noticed
	conn2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second client", func() bool { return s.Stats().BeastClients == 2 })
	if b := readBeast(t, conn2, 2000, 5*time.Second); b[0] != 0x1A {
		t.Errorf("second client stream does not start at a frame boundary: % X", b[:4])
	}
	conn2.Close()
	waitFor(t, "second client to be dropped", func() bool { return s.Stats().BeastClients == 1 })

	// shutdown: the server returns nil and closes the connection
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeBeastOn returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeBeastOn did not return after cancellation")
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		// a reset is as good as a clean EOF here
		t.Logf("connection closed with: %v", err)
	}
	if n := s.Stats().BeastClients; n != 0 {
		t.Errorf("%d clients left after shutdown", n)
	}
	before := s.Now()
	time.Sleep(50 * time.Millisecond)
	if !s.Now().Equal(before) {
		t.Error("simulation keeps running after the server stopped")
	}
}

func TestServeBeastOptionsAndErrors(t *testing.T) {
	s := New(Config{Aircraft: 40, Seed: 78, Start: testStart, Speed: 60, NoDiurnal: true})

	// unknown antenna
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	if err := s.ServeBeastOn(context.Background(), ln, BeastOptions{AntennaID: "nope"}); err == nil {
		t.Error("unknown antenna ID accepted")
	}
	if _, err := ln.Accept(); err == nil {
		t.Error("listener left open after an error")
	}

	// address that cannot be bound
	if err := s.ServeBeast(context.Background(), "256.0.0.1:99999"); err == nil {
		t.Error("ServeBeast on an invalid address returned nil")
	}

	// ServeBeast with a context: returns nil when cancelled
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ServeBeast(ctx, "127.0.0.1:0") }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeBeast returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeBeast did not return after cancellation")
	}

	// one stream per antenna, without MLAT and position frames
	ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- s.ServeBeastOn(ctx, ln, BeastOptions{AntennaID: "yagi", NoMLAT: true, NoPosition: true})
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frames, _, bad := refParseBeast(readBeast(t, conn, 60_000, 10*time.Second))
	if bad != 0 || len(frames) < 500 {
		t.Fatalf("%d frames, %d framing errors", len(frames), bad)
	}
	for i, f := range frames {
		if f.typ == '5' || f.ts == 0xFF004D4C4154 {
			t.Fatalf("frame %d: type %c timestamp %012x in a stream without MLAT and position", i, f.typ, f.ts)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("ServeBeastOn returned %v", err)
	}
}
