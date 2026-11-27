package feed

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"airview/internal/model"
)

// Fuzz targets for every parser of the package. Run one with, e.g.
//
//	go test ./internal/feed -run=^$ -fuzz=FuzzBeastParser -fuzztime=30s
//
// Without -fuzz they run their seed corpus as ordinary tests.

type beastEvent struct {
	frame bool
	typ   byte
	ts    uint64
	sig   byte
	body  []byte
}

func runBeast(chunks [][]byte) ([]beastEvent, *BeastParser) {
	var out []beastEvent
	p := NewBeastParser(func(f *BeastFrame) {
		out = append(out, beastEvent{true, f.Type, f.Timestamp, f.Signal, append([]byte{}, f.Payload...)})
	})
	p.OnEscape = func(typ byte, body []byte) {
		var b []byte
		if body != nil {
			b = append([]byte{}, body...)
		}
		out = append(out, beastEvent{typ: typ, body: b})
	}
	for _, c := range chunks {
		p.Write(c)
	}
	return out, p
}

func sameBeast(a, b []beastEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.frame != y.frame || x.typ != y.typ || x.ts != y.ts || x.sig != y.sig || !bytes.Equal(x.body, y.body) || (x.body == nil) != (y.body == nil) {
			return false
		}
	}
	return true
}

func FuzzBeastParser(f *testing.F) {
	stream, _ := testStream()
	f.Add(stream, uint16(7))
	var ext []byte
	ext = AppendBeastUUID(ext, "0f8c1e2a-7d3b-4c55-9a1a-1a2b3c4d5e6f")
	ext = AppendBeastOption(ext, 'O')
	ext = AppendBeastReceiverID(ext, 0x1A1A)
	ext = AppendBeastPing(ext, 0x1A0000)
	ext = append(ext, 0x1A, 0xE5, 0x1A, 0x1A, 0x1A)
	ext = AppendBeastFrame(ext, BeastModeAC, 0x1A, 0x1A, modeAC)
	f.Add(ext, uint16(3))
	f.Add([]byte{0x1A, '3', 0x1A}, uint16(1))
	f.Add([]byte("*8D4840D6202CC371C32CE0576098;"), uint16(0))

	f.Fuzz(func(t *testing.T, data []byte, split uint16) {
		whole, p := runBeast([][]byte{data})
		k := 0
		if len(data) > 0 {
			k = int(split) % (len(data) + 1)
		}
		halves, _ := runBeast([][]byte{data[:k], data[k:]})
		if !sameBeast(whole, halves) {
			t.Fatalf("split at %d changed the result", k)
		}
		bytewise := make([][]byte, len(data))
		for i := range data {
			bytewise[i] = data[i : i+1]
		}
		if one, _ := runBeast(bytewise); !sameBeast(whole, one) {
			t.Fatal("byte-wise parsing changed the result")
		}

		var frames, escs uint64
		var reenc []byte
		for _, e := range whole {
			if !e.frame {
				if e.body != nil {
					escs++
					if len(e.body) != beastBodyLen(e.typ) || !IsBeastEscapeType(e.typ) {
						t.Fatalf("escape %x body length %d", e.typ, len(e.body))
					}
					reenc = AppendBeastEscape(reenc, e.typ, e.body)
				}
				continue
			}
			frames++
			want := map[byte]int{BeastModeAC: 2, BeastModeSShort: 7, BeastModeSLong: 14, BeastStatusType: 14, BeastPosition: 21}[e.typ]
			if want == 0 || len(e.body) != want || e.ts >= 1<<48 {
				t.Fatalf("frame type %x payload %d ts %x", e.typ, len(e.body), e.ts)
			}
			reenc = AppendBeastFrame(reenc, e.typ, e.ts, e.sig, e.body)
			// Decoder helpers never panic on parsed payloads.
			ParseBeastStatus(e.body)
			ParseBeastPosition(e.body)
		}
		if frames != p.Frames || escs != p.Skipped {
			t.Fatalf("counters frames %d/%d skipped %d/%d", frames, p.Frames, escs, p.Skipped)
		}
		// Re-encoding what was understood parses back to the same thing.
		var known []beastEvent
		for _, e := range whole {
			if e.frame || e.body != nil {
				known = append(known, e)
			}
		}
		if again, p2 := runBeast([][]byte{reenc}); !sameBeast(known, again) || p2.Garbage != 0 || p2.Unknown != 0 {
			t.Fatal("re-encoded stream does not round-trip")
		}
	})
}

func FuzzBeastDecoderClock(f *testing.F) {
	stream, _ := testStream()
	f.Add(stream, int64(1_763_000_000_000_000_000), uint8(3))
	var s []byte
	s = AppendBeastFrame(s, BeastStatusType, GPSTimestamp(86399, 0), 0, statusPayload(0x10, 0, gpsUTCLocked))
	s = AppendBeastFrame(s, BeastModeSLong, GPSTimestamp(86399, 999_000_000), 1, long1)
	s = AppendBeastFrame(s, BeastModeSLong, GPSTimestamp(0, 1000), 1, long1)
	s = AppendBeastFrame(s, BeastStatusType, GPSTimestamp(1, 0), 0, statusPayload(0x10, 0, gpsBad))
	s = AppendBeastFrame(s, BeastModeSShort, GPSTimestamp(5000, 1000), 1, short1)
	s = AppendBeastFrame(s, BeastStatusType, 5, 0, statusPayload(0x00, 0, 0))
	s = AppendBeastFrame(s, BeastModeSShort, 1<<48-1, 1, short1)
	s = AppendBeastFrame(s, BeastModeSShort, 3, 1, short1)
	f.Add(s, int64(1_763_078_399_000_000_000), uint8(1))

	f.Fuzz(func(t *testing.T, data []byte, host0 int64, confirm uint8) {
		if host0 < 0 {
			host0 = -host0
		}
		host0 %= 4_000_000_000_000_000_000 // keep within year ~2096
		out := &clockSink{}
		d := &BeastDecoder{Sink: out, FeedID: "fz"}
		d.TS.OnClockEvent = out.event
		d.TS.Clock().ConfirmFrames = int(confirm % 5)
		p := NewBeastParser(d.Handle)
		// Each 16-byte chunk arrives 37 ms after the previous one.
		for i := 0; i < len(data); i += 16 {
			j := min(i+16, len(data))
			d.Recv = time.Unix(0, host0+int64(i/16)*37e6)
			p.Write(data[i:j])
		}
		checkClockInvariants(t, out, d.TS.Clock().backTol())
	})
}

// checkClockInvariants verifies the epoch / tick contract on a decoded
// sequence: events precede their frames, epochs never repeat consecutively
// and are never 0, ticks within an epoch never fall back by more than the
// tolerance below the newest accepted frame.
func checkClockInvariants(t *testing.T, out *clockSink, tolNs int64) {
	t.Helper()
	var cur uint32
	var curCls model.ClockClass
	var maxTicks uint64
	have := false
	ei, fi := 0, 0
	for _, o := range out.order {
		if o == 'e' {
			ev := out.events[ei]
			ei++
			if ev.NewEpoch == 0 || ev.NewEpoch == ev.OldEpoch || ev.OldEpoch != cur || !ev.Reason.Valid() || !ev.NewClass.Timed() {
				t.Fatalf("bad event %+v (current epoch %d)", ev, cur)
			}
			cur, curCls, have, maxTicks = ev.NewEpoch, ev.NewClass, false, 0
			continue
		}
		f := out.frames[fi]
		fi++
		if !f.ClockCls.Timed() {
			if f.Epoch != 0 || f.Ticks != 0 {
				t.Fatalf("untimed frame with clock fields %+v", f)
			}
			continue
		}
		if f.Epoch != cur || f.ClockCls != curCls || f.MLAT {
			t.Fatalf("frame epoch %d class %v, current %d %v", f.Epoch, f.ClockCls, cur, curCls)
		}
		tol := uint64(float64(tolNs) * f.ClockCls.NominalHz() / 1e9)
		if have && f.Ticks+tol < maxTicks {
			t.Fatalf("ticks fell back: %d after %d", f.Ticks, maxTicks)
		}
		if f.Ticks > maxTicks {
			maxTicks = f.Ticks
		}
		have = true
	}
}

func FuzzClockTracker(f *testing.F) {
	f.Add([]byte{4, 0, 0, 0, 0, 0, 0, 1, 0, 10, 0, 0, 0, 0, 0, 2, 0, 10})
	f.Add([]byte{1, 0, 0, 0, 0x27, 0x10, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0, 0, 1})
	f.Add([]byte{7, 0xff, 0xff, 0xf0, 0, 0, 0, 0, 0, 5, 0x00, 0x00, 0x10, 0, 0, 0, 0, 5})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		out := &clockSink{}
		c := &ClockTracker{OnEvent: out.event, ConfirmFrames: int(data[0]>>4) % 4}
		c.SetClass(model.ClockClass(data[0]&7), "")
		host := clockT0
		data = data[1:]
		// Records of 9 bytes: op, 6-byte raw (scaled by class), 2-byte host delta (ms).
		for len(data) >= 9 {
			op, rec := data[0], data[1:9]
			data = data[9:]
			raw := uint64(rec[0])<<40 | uint64(rec[1])<<32 | uint64(rec[2])<<24 | uint64(rec[3])<<16 | uint64(rec[4])<<8 | uint64(rec[5])
			host += int64(binary.BigEndian.Uint16(rec[6:])) * 1e6
			switch op % 16 {
			case 0:
				c.SetClass(model.ClockClass(op>>4&7), ClockDetailClassChange)
			case 1:
				c.SampleDrop(raw)
			case 2:
				c.Restart()
			case 3:
				c.ModeChange(ClockDetailDayRange)
			default:
				if c.Class().GPS() {
					raw = raw * 1000 % uint64(model.NsPerDay)
				}
				ticks, ok := c.Stamp(raw, host)
				f := model.RawFrame{}
				if ok {
					f.ClockCls, f.Epoch, f.Ticks = c.Class(), c.Epoch(), ticks
				}
				out.HandleFrame(f)
			}
		}
		checkClockInvariants(t, out, c.backTol())
	})
}

func FuzzAVRParser(f *testing.F) {
	f.Add([]byte("*8D4840D6202CC371C32CE0576098;\r\n@0123456789AB5D4840D6202CC3;\n<0123456789AB805D4840D6202CC3;%000000000001A1B2;"))
	f.Add([]byte(":1234;*;@;<00;**8D;"))
	f.Fuzz(func(t *testing.T, data []byte) {
		run := func(chunks ...[]byte) ([]AVRFrame, *AVRParser) {
			var out []AVRFrame
			p := NewAVRParser(func(fr *AVRFrame) {
				c := *fr
				c.Data = append([]byte{}, fr.Data...)
				out = append(out, c)
			})
			for _, c := range chunks {
				p.Write(c)
			}
			return out, p
		}
		whole, p := run(data)
		var bytewise [][]byte
		for i := range data {
			bytewise = append(bytewise, data[i:i+1])
		}
		one, _ := run(bytewise...)
		if len(one) != len(whole) {
			t.Fatal("byte-wise parsing changed the result")
		}
		for i, fr := range whole {
			if n := len(fr.Data); n != 2 && n != 7 && n != 14 {
				t.Fatalf("data length %d", n)
			}
			if fr.Timestamp >= 1<<48 || !bytes.Equal(fr.Data, one[i].Data) || fr.Timestamp != one[i].Timestamp {
				t.Fatal("frame mismatch")
			}
		}
		if uint64(len(whole)) != p.Frames {
			t.Fatal("frame counter")
		}
		// The decoder copes with whatever the parser produced.
		d := &AVRDecoder{Sink: &nullSink{}, Recv: time.Unix(1_763_000_000, 0)}
		NewAVRParser(d.Handle).Write(data)
	})
}

func FuzzSBSLine(f *testing.F) {
	f.Add([]byte("MSG,3,1,1,4840D6,1,2026/11/11,12:00:00.000,2026/11/11,12:00:00.000,,37000,,,51.5,-0.1,,,0,0,0,0"))
	f.Add([]byte("MSG,1,,,~ABCDEF,,,,,,KLM123,,,,,,,,,,,"))
	f.Add([]byte("MSG,4,,,4840D6,,,,,,,,450,270,,,-1200,,,,,0"))
	f.Fuzz(func(t *testing.T, line []byte) {
		ParseSBSLine(line)
		d := &SBSDecoder{Sink: &nullSink{}, FeedID: "x", Recv: time.Unix(1_763_000_000, 0)}
		half := len(line) / 2
		d.Write(line[:half])
		d.Write(line[half:])
		d.Write([]byte("\n"))
	})
}

func FuzzJSONDecoder(f *testing.F) {
	f.Add([]byte(`{"now":1763000000.1,"messages":10,"aircraft":[{"hex":"4840d6","flight":"KLM123 ","alt_baro":37000,"gs":450,"track":270,"lat":51.5,"lon":-0.1,"seen":0.1,"seen_pos":0.2,"rssi":-20.5,"messages":10,"mlat":[]}]}`))
	f.Add([]byte(`[{"uti":1763000000,"ns":5,"hex":"4840D6","fli":"KLM123","src":"A","lat":51.5,"lon":-0.1,"alt":37000,"spd":450,"trk":270,"vrt":0,"gda":"A","squ":"1000","dbm":-60,"lla":1,"tru":5}]`))
	f.Add([]byte(`{"aircraft":[{"hex":"~abcdef","alt_baro":"ground"}]}`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		d := &JSONDecoder{Sink: &nullSink{}, FeedID: "j"}
		now := time.Unix(1_763_000_000, 0)
		d.Decode(doc, now)
		d.Decode(doc, now.Add(time.Second))
	})
}

func FuzzNormalizeUUID(f *testing.F) {
	f.Add("0f8c1e2a-7d3b-4c55-9a1a-1a2b3c4d5e6f")
	f.Add("00000000-0000-0000-0000-000000000000")
	f.Fuzz(func(t *testing.T, s string) {
		u, ok := NormalizeUUID(s)
		if !ok {
			return
		}
		if len(u) < 28 || len(u) > BeastUUIDLen {
			t.Fatalf("length %d", len(u))
		}
		if again, ok2 := NormalizeUUID(u); !ok2 || again != u {
			t.Fatal("not idempotent")
		}
	})
}
