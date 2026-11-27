package avf

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"testing"

	"airview/internal/model"
)

func seedVectors(f *testing.F) {
	for _, s := range []string{
		"023301010700000000000f424018776f28c30380000212000080" + "8d4840d6202cc371c32ce0576098" + "01e05df303405d4840d6a1b2c3",
		"8202ac02",
		"854001" + "4840d618776f28ca5f4d15414e5d7040000000c0c38820000000004152f5843000000043c8000041200000c0a0000043c80000404000004461000001010200",
		"0a03010203",
		"00",
		"02ffffffffffffffffff7f",
	} {
		b, _ := hex.DecodeString(s)
		f.Add(b)
	}
}

// FuzzRecordReader: the framing never panics, never returns a payload above
// the limit and never returns a record of the wrong direction.
func FuzzRecordReader(f *testing.F) {
	seedVectors(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, dir := range []Direction{FromClient, FromServer} {
			rr := NewRecordReader(bytes.NewReader(data), dir)
			for i := 0; i < 1000; i++ {
				r, err := rr.Next()
				if err != nil {
					break
				}
				if len(r.Payload) > MaxRecordLen || r.Type.Direction() != dir || !r.Type.Known() {
					t.Fatalf("bad record %s len %d", r.Type, len(r.Payload))
				}
				decodeAny(t, r)
			}
		}
	})
}

// decodeAny runs the payload decoder of a record and checks that a decoded
// binary payload re-encodes and decodes to the same value.
func decodeAny(t *testing.T, r Record) {
	switch r.Type {
	case TFrames:
		var f Frames
		if f.Decode(r.Payload) == nil {
			p, err := AppendFrames(nil, &f)
			if err != nil {
				t.Fatalf("decoded FRAMES does not re-encode: %v", err)
			}
			var g Frames
			if err := g.Decode(p); err != nil || !reflect.DeepEqual(f, g) {
				t.Fatalf("FRAMES round trip: %v", err)
			}
		}
	case TMLATResults:
		if fx, err := DecodeMLATResults(nil, r.Payload); err == nil {
			p, err := AppendMLATResults(nil, fx)
			if err != nil {
				t.Fatal(err)
			}
			gx, err := DecodeMLATResults(nil, p)
			if err != nil || len(gx) != len(fx) {
				t.Fatal("MLAT round trip", err)
			}
		}
	case TClockStatus:
		var c ClockStatus
		if c.Decode(r.Payload) == nil {
			p, err := AppendClockStatus(nil, &c)
			var d ClockStatus
			if err != nil || d.Decode(p) != nil || !reflect.DeepEqual(c, d) {
				t.Fatal("CLOCK_STATUS round trip", err)
			}
		}
	case TClockPair:
		var c ClockPair
		if c.Decode(r.Payload) == nil {
			var d ClockPair
			if d.Decode(AppendClockPair(nil, &c)) != nil || c != d {
				t.Fatal("CLOCK_PAIR round trip")
			}
		}
	case TPing:
		var p Ping
		_ = p.Decode(r.Payload)
	case TPong:
		var p Pong
		_ = p.Decode(r.Payload)
	case TAck:
		_, _ = DecodeAck(r.Payload)
	case TDecoded:
		_, _, _, _ = DecodeDecoded(r.Payload)
	case TReceiver:
		var v Receiver
		if _, err := DecodeSeqJSON(r.Payload, &v); err == nil {
			_ = v.Validate()
			_ = v.Class()
		}
	case TInterest:
		var v Interest
		if DecodeJSON(r.Payload, &v) == nil {
			_ = v.Validate()
		}
	case TFlow:
		var v Flow
		if DecodeJSON(r.Payload, &v) == nil {
			_ = v.Validate()
		}
	default:
		var v map[string]any
		if r.Type.HasSeq() {
			_, _ = DecodeSeqJSON(r.Payload, &v)
		} else {
			_ = DecodeJSON(r.Payload, &v)
		}
	}
}

// FuzzFramesRoundTrip builds FRAMES from fuzzed fields and checks the
// encode/decode round trip.
func FuzzFramesRoundTrip(f *testing.F) {
	f.Add(uint64(1), uint8(1), uint32(7), uint64(1000), int64(1763000000000000000), []byte{0x12, 0x00, 0x80, 0x01, 0x40, 0x22}, uint16(12000))
	f.Fuzz(func(t *testing.T, seq uint64, rx uint8, ep uint32, base uint64, host int64, spec []byte, step uint16) {
		if rx == 0 || len(spec) == 0 {
			return
		}
		host -= host % 1000
		r := &Frames{Seq: seq, RxNo: rx, EpochID: ep, BaseTicks: base, BaseHostNs: host}
		ticks := base
		for i, b := range spec {
			if len(r.Frames) == MaxFramesPerRecord {
				break
			}
			kind := int(b & 3 % 3)
			fr := Frame{Data: make([]byte, kindLen[kind]), Channel: b >> 4, Late: b&4 != 0, TsInvalid: b&8 != 0, Sig: b}
			fr.Data[0] = b
			fr.HostNs = host + int64(int8(b))*1000*int64(i)
			if !fr.TsInvalid {
				if ticks+uint64(step) < ticks {
					return
				}
				ticks += uint64(step) * uint64(b&1)
				fr.Ticks = ticks
			}
			r.Frames = append(r.Frames, fr)
		}
		p, err := AppendFrames(nil, r)
		if err != nil {
			t.Fatal(err)
		}
		var d Frames
		if err := d.Decode(p); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(&d, r) {
			t.Fatalf("round trip\n got %+v\nwant %+v", d, *r)
		}
	})
}

// FuzzMLATRoundTrip checks the MLAT_RESULTS codec on fuzzed fixes.
func FuzzMLATRoundTrip(f *testing.F) {
	f.Add(uint32(0x4840D6), int64(1763000000123456789), 3980000.5, -10000.25, float32(400), uint8(1), true)
	f.Fuzz(func(t *testing.T, icao uint32, ts int64, x, y float64, c float32, nb uint8, vel bool) {
		fix := model.MLATFix{ICAO: icao & 0xFFFFFF, TUnixNs: ts, ECEF: [3]float64{x, y, x - y},
			Cov: [6]float32{c, c, c, c, c, c}, NBucket: nb, Grade: nb, Flags: nb}
		if vel {
			fix.Vel = &[3]float32{c, -c, 1}
		}
		p, err := AppendMLATResults(nil, []model.MLATFix{fix, fix})
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeMLATResults(nil, p)
		if err != nil || len(got) != 2 {
			t.Fatal(err)
		}
		// Compare bit patterns (NaN != NaN).
		p2, _ := AppendMLATResults(nil, got)
		if !bytes.Equal(p, p2) {
			t.Fatal("re-encoding differs")
		}
	})
}

// FuzzHandshakeMessage: the handshake reader and JSON decoders never panic.
func FuzzHandshakeMessage(f *testing.F) {
	f.Add([]byte("\x00\x00\x00\x02{}"))
	f.Add([]byte("\x00\x00\x00\x30{\"body\":\"e30=\",\"sig\":\"AAAA\",\"x\":1}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		var hs HelloS
		_ = ReadMessage(r, &hs)
		var hc HelloC
		if ReadMessage(bytes.NewReader(data), &hc) == nil {
			var b HelloBody
			_ = DecodeJSON(hc.Body, &b)
			_ = checkBody(&b)
		}
		var w Welcome
		_ = ReadMessage(bytes.NewReader(data), &w)
	})
}

// FuzzStreamReader: arbitrary compressed input never panics the inflater,
// the guards or the record reader.
func FuzzStreamReader(f *testing.F) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, 0)
	w.WriteRecord(TPing, AppendPing(nil, Ping{ID: 1, ServerSendNs: 2}))
	w.WriteRecord(TAck, AppendAck(nil, 3))
	f.Add(buf.Bytes())
	f.Add([]byte{0x00, 0x00, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		rr := NewRecordReader(NewStreamReader(bytes.NewReader(data), DefaultGuard()), FromServer)
		for i := 0; i < 1000; i++ {
			r, err := rr.Next()
			if err != nil {
				return
			}
			decodeAny(t, r)
		}
	})
}
