package avf

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"

	"airview/internal/model"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	s = strings.Join(strings.Fields(s), "")
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustHex(t testing.TB, s string) []byte { return unhex(t, s) }

// AVF.md §5.5 test vector.
func TestFramesVector(t *testing.T) {
	want := unhex(t, `02 33 01 01 07 00000000000f4240 18776f28c3038000 02
		12 00 00 80 8d4840d6202cc371c32ce0576098
		01 e05d f303 40 5d4840d6a1b2c3`)
	const base = int64(1_763_000_000_000_000_000)
	r := &Frames{Seq: 1, RxNo: 1, EpochID: 7, BaseTicks: 1_000_000, BaseHostNs: base, Frames: []Frame{
		{Ticks: 1_000_000, HostNs: base, Data: mustHex(t, "8D4840D6202CC371C32CE0576098"), Channel: 1, Sig: 0x80},
		{Ticks: 1_012_000, HostNs: base - 250_000, Data: mustHex(t, "5D4840D6A1B2C3"), Channel: 0, Sig: 0x40},
	}}
	p, err := AppendFrames(nil, r)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := AppendRecord(nil, TFrames, p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec, want) {
		t.Fatalf("FRAMES vector\n got %x\nwant %x", rec, want)
	}
	rr := NewRecordReader(bytes.NewReader(want), FromClient)
	got, err := rr.Next()
	if err != nil || got.Type != TFrames {
		t.Fatal(got.Type, err)
	}
	var d Frames
	if err := d.Decode(got.Payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&d, r) {
		t.Fatalf("decode mismatch\n got %+v\nwant %+v", d, *r)
	}
}

// AVF.md §6.1 test vector.
func TestAckVector(t *testing.T) {
	rec, err := AppendRecord(nil, TAck, AppendAck(nil, 300))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(rec) != "8202ac02" {
		t.Fatalf("ACK vector %x", rec)
	}
	v, err := DecodeAck(rec[2:])
	if err != nil || v != 300 {
		t.Fatal(v, err)
	}
}

// AVF.md §6.3 test vector.
func TestMLATResultsVector(t *testing.T) {
	want := unhex(t, `85 40 01 4840d6 18776f28ca5f4d15
		414e5d7040000000 c0c3882000000000 4152f58430000000
		43c80000 41200000 c0a00000 43c80000 40400000 44610000
		01 01 02 00`)
	fix := model.MLATFix{ICAO: 0x4840D6, TUnixNs: 1_763_000_000_123_456_789,
		ECEF: [3]float64{3980000.5, -10000.25, 4970000.75}, Cov: [6]float32{400, 10, -5, 400, 3, 900},
		NBucket: 1, Grade: 1, Flags: 0x02}
	p, err := AppendMLATResults(nil, []model.MLATFix{fix})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := AppendRecord(nil, TMLATResults, p)
	if !bytes.Equal(rec, want) {
		t.Fatalf("MLAT_RESULTS vector\n got %x\nwant %x", rec, want)
	}
	got, err := DecodeMLATResults(nil, want[2:])
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], fix) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestMLATResultsVelocityRoundTrip(t *testing.T) {
	v1 := [3]float32{100, -50, 2.5}
	fixes := []model.MLATFix{
		{ICAO: 0xABCDEF, TUnixNs: 5, ECEF: [3]float64{1, 2, 3}, Vel: &v1, NBucket: 3, Grade: 4, Flags: 0x1F},
		{ICAO: 1, TUnixNs: -7},
		{ICAO: 2, Vel: &[3]float32{1, 2, 3}},
	}
	p, err := AppendMLATResults(nil, fixes)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 1+75+63+75 { // AVF.md §6.3 says 64/76 per fix; the layout (and its vector) gives 63/75
		t.Fatalf("len %d", len(p))
	}
	got, err := DecodeMLATResults(nil, append(p, 0xEE, 0xFF)) // trailing bytes ignored
	if err != nil || !reflect.DeepEqual(got, fixes) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := AppendMLATResults(nil, nil); err == nil {
		t.Fatal("empty results accepted")
	}
	if _, err := AppendMLATResults(nil, []model.MLATFix{{ICAO: 1 << 24}}); err == nil {
		t.Fatal("25-bit ICAO accepted")
	}
	if _, err := DecodeMLATResults(nil, p[:len(p)-1]); !errors.Is(err, ErrProtocol) {
		t.Fatal("truncated accepted", err)
	}
}

func TestFramesRoundTripAndRules(t *testing.T) {
	base := int64(1_700_000_000_000_000_000)
	r := &Frames{Seq: 1 << 40, RxNo: 255, EpochID: math.MaxUint32, BaseTicks: 500, BaseHostNs: base, Frames: []Frame{
		{Ticks: 500, HostNs: base + 1000, Data: []byte{0x12, 0x34}, Channel: 15, Late: true},
		{HostNs: base + 3000, Data: make([]byte, 7), TsInvalid: true},
		{Ticks: 1 << 50, HostNs: base - 99_000, Data: make([]byte, 14), Sig: 0xFF},
		{Ticks: 1 << 50, HostNs: base - 99_000, Data: make([]byte, 7)},
	}}
	p, err := AppendFrames(nil, r)
	if err != nil {
		t.Fatal(err)
	}
	var d Frames
	if err := d.Decode(append(p, 1, 2, 3)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&d, r) {
		t.Fatalf("got %+v\nwant %+v", d, *r)
	}
	if d.Frames[0].Kind() != KindModeAC || d.Frames[2].Kind() != KindModeSLong {
		t.Fatal("kinds")
	}
	// Reuse keeps storage.
	ptr := &d.Frames[0]
	if err := d.Decode(p); err != nil || &d.Frames[0] != ptr {
		t.Fatal("storage not reused", err)
	}

	bad := []*Frames{
		{RxNo: 1}, // no frames
		{RxNo: 0, Frames: []Frame{{Data: make([]byte, 7)}}},                                              // rx 0
		{RxNo: 1, Frames: []Frame{{Data: make([]byte, 5)}}},                                              // bad length
		{RxNo: 1, BaseTicks: 10, Frames: []Frame{{Ticks: 9, Data: make([]byte, 7)}}},                     // before base
		{RxNo: 1, Frames: []Frame{{Data: make([]byte, 7), Channel: 16}}},                                 // channel field
		{RxNo: 1, Frames: make([]Frame, MaxFramesPerRecord+1)},                                           // too many
		{RxNo: 1, Frames: []Frame{{Ticks: 5, Data: make([]byte, 7)}, {Ticks: 4, Data: make([]byte, 7)}}}, // backwards
	}
	for i, b := range bad {
		if _, err := AppendFrames(nil, b); !errors.Is(err, ErrProtocol) {
			t.Errorf("bad[%d] accepted: %v", i, err)
		}
	}
	// Decoder rejections.
	mutate := func(f func([]byte) []byte) error {
		var x Frames
		return x.Decode(f(append([]byte(nil), p...)))
	}
	if err := mutate(func(b []byte) []byte { return b[:len(b)-1-3] }); err == nil {
		t.Error("truncated accepted")
	}
	// Reserved kind 3 in the vector.
	vec := unhex(t, `01 01 07 00000000000f4240 18776f28c3038000 01 13 00 00 80 8d4840d6202cc371c32ce0576098`)
	var x Frames
	if err := x.Decode(vec); !errors.Is(err, ErrProtocol) {
		t.Error("kind 3 accepted", err)
	}
	// tsInvalid with non-zero dTicks.
	vec2 := unhex(t, `01 01 07 0000000000000000 0000000000000000 01 09 05 00 00 5d4840d6a1b2c3`)
	if err := x.Decode(vec2); !errors.Is(err, ErrProtocol) {
		t.Error("tsInvalid dTicks accepted", err)
	}
	// Huge n with a short body must not allocate n frames.
	vec3 := unhex(t, `01 01 07 0000000000000000 0000000000000000 8020`)
	if err := x.Decode(vec3); !errors.Is(err, ErrProtocol) {
		t.Error("n=4096 short accepted", err)
	}
}

func TestSmallBinaryRecords(t *testing.T) {
	cs := &ClockStatus{Seq: 9, RxNo: 2, HostNs: -5, Settings: 0x31, GPSStatus: 0xC0, PPSOffsetTicks: -1234, Raw: []byte("4abcdef")}
	p, err := AppendClockStatus(nil, cs)
	if err != nil {
		t.Fatal(err)
	}
	var cs2 ClockStatus
	if err := cs2.Decode(p); err != nil || !reflect.DeepEqual(cs, &cs2) {
		t.Fatalf("%+v %v", cs2, err)
	}
	if _, err := AppendClockStatus(nil, &ClockStatus{RxNo: 1, Raw: make([]byte, 65)}); err == nil {
		t.Error("raw 65 accepted")
	}
	if err := cs2.Decode(p[:len(p)-1]); err == nil {
		t.Error("truncated CLOCK_STATUS accepted")
	}

	cp := &ClockPair{Seq: 3, RxNo: 1, EpochID: 77, Ticks: 1 << 62, HostUTCNs: 1_763_000_000_000_000_000, HostErrEstNs: 1500}
	var cp2 ClockPair
	if err := cp2.Decode(AppendClockPair(nil, cp)); err != nil || cp2 != *cp {
		t.Fatalf("%+v %v", cp2, err)
	}
	var pi Ping
	if err := pi.Decode(AppendPing(nil, Ping{ID: 42, ServerSendNs: 7})); err != nil || pi != (Ping{42, 7}) {
		t.Fatal(pi, err)
	}
	var po Pong
	if err := po.Decode(AppendPong(nil, Pong{ID: 1, RxNs: 2, TxNs: 3})); err != nil || po != (Pong{1, 2, 3}) {
		t.Fatal(po, err)
	}
	if err := po.Decode([]byte{1, 2}); !errors.Is(err, ErrProtocol) {
		t.Fatal("short PONG accepted")
	}
	if _, _, err := SplitSeq(nil); err == nil {
		t.Fatal("empty seq accepted")
	}
	if _, _, err := SplitSeq([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01}); err == nil {
		t.Fatal("overlong varint accepted")
	}
}

func TestJSONRecords(t *testing.T) {
	rcv := Receiver{RxNo: 1, Name: "evo-north", ClockClass: "gps_utc", NominalHz: 1e9, EpochID: 17, Channels: 2,
		Decoder: "radarcape-evo 2.10", Location: &Location{Lat: 51.5, Lon: -0.1, Alt: 45, AltRef: "msl", Source: "manual", SigmaM: 10},
		Antennas: []Antenna{{ID: "vertical", Kind: "collinear", AGLM: 8}},
		Privacy:  Privacy{Pin: model.PinHidden}, Timestamps: true}
	p, err := AppendSeqJSON(nil, 5, &rcv)
	if err != nil {
		t.Fatal(err)
	}
	var r2 Receiver
	seq, err := DecodeSeqJSON(p, &r2)
	if err != nil || seq != 5 || !reflect.DeepEqual(rcv, r2) {
		t.Fatalf("%+v %v", r2, err)
	}
	if r2.Class() != model.ClockGPSUTC || r2.Validate() != nil {
		t.Fatal("class/validate")
	}
	r2.ClockClass = "warp"
	if r2.Class() != model.ClockNone {
		t.Fatal("unknown class must be none")
	}
	r2.Channels = 0
	if r2.Validate() == nil {
		t.Fatal("channels 0 valid")
	}
	// Unknown members and trailing bytes are ignored.
	var ev ClockEvent
	if _, err := DecodeSeqJSON(append(AppendSeq(nil, 1), `{"rxNo":1,"oldEpoch":16,"newEpoch":17,"reason":"jump","x":[1],"hostNs":1763000000000000001} trailing`...), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.HostNs != 1763000000000000001 || ev.Reason != model.ClockEvJump {
		t.Fatalf("%+v (64-bit ints must not go through float64)", ev)
	}
	if err := DecodeJSON([]byte("not json"), &ev); !errors.Is(err, ErrProtocol) {
		t.Fatal("bad JSON accepted")
	}
	lat := 0.0
	ups := []DecodedUpdate{{ICAO: "4840D6", TMs: 1, Lat: &lat, N: 2}}
	dp, err := AppendDecoded(nil, 8, 3, ups)
	if err != nil {
		t.Fatal(err)
	}
	s, rx, ups2, err := DecodeDecoded(dp)
	if err != nil || s != 8 || rx != 3 || !reflect.DeepEqual(ups, ups2) {
		t.Fatalf("%v %v %+v %v", s, rx, ups2, err)
	}
	in := Interest{RxNo: 1, Mode: ModeSelective, ReduceMs: 500, SyncAdd: []string{"4840D6"}, MLATDel: []string{"~3c6586"}}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	in.SyncAdd = []string{"XYZ"}
	if in.Validate() == nil {
		t.Fatal("bad ICAO valid")
	}
	if (&Flow{SlowFactor: 2}).Validate() == nil || (&Flow{BatchMs: 200, SlowFactor: 0.5}).Validate() != nil {
		t.Fatal("flow validate")
	}
	for _, c := range []struct {
		s   string
		a   uint32
		non bool
	}{{"4840D6", 0x4840D6, false}, {"~00000A", 10, true}} {
		a, non, ok := ParseICAO(c.s)
		if !ok || a != c.a || non != c.non || FormatICAO(a, non) != strings.ToUpper(c.s) {
			t.Errorf("ICAO %s", c.s)
		}
	}
}

func TestRecordReader(t *testing.T) {
	var s []byte
	s, _ = AppendRecord(s, TFrames, []byte{1, 2, 3})
	s, _ = AppendRecord(s, 0x55, bytes.Repeat([]byte{9}, 300)) // unknown, skipped
	s, _ = AppendRecord(s, 0xC0, nil)                          // unknown server range, skipped
	s, _ = AppendRecord(s, TPong, nil)
	rr := NewRecordReader(bytes.NewReader(s), FromClient)
	r1, err := rr.Next()
	if err != nil || r1.Type != TFrames || !bytes.Equal(r1.Payload, []byte{1, 2, 3}) {
		t.Fatal(r1, err)
	}
	r2, err := rr.Next()
	if err != nil || r2.Type != TPong || len(r2.Payload) != 0 {
		t.Fatal(r2, err)
	}
	if _, err := rr.Next(); err != io.EOF {
		t.Fatal("want EOF", err)
	}
	if rr.Skipped != 2 || rr.Records != 2 {
		t.Fatal(rr.Skipped, rr.Records)
	}

	cases := map[string][]byte{
		"reserved 0x00":   {0x00, 0x00},
		"reserved 0x80":   {0x80, 0x00},
		"reserved 0xFF":   {0xFF, 0x00},
		"wrong direction": {byte(TAck), 0x01, 0x05},
		"too long":        {0x02, 0x81, 0x80, 0x04}, // 65537
		"overflow varint": {0x02, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x7F},
		"11-byte varint":  {0x02, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00},
	}
	for name, b := range cases {
		if _, err := NewRecordReader(bytes.NewReader(b), FromClient).Next(); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := NewRecordReader(bytes.NewReader([]byte{0x02, 0x05, 1}), FromClient).Next(); err != io.ErrUnexpectedEOF {
		t.Errorf("truncated payload: %v", err)
	}
	if _, err := NewRecordReader(bytes.NewReader([]byte{0x02}), FromClient).Next(); err != io.ErrUnexpectedEOF {
		t.Errorf("truncated header: %v", err)
	}
	// Max length is accepted.
	big, _ := AppendRecord(nil, TDecoded, make([]byte, MaxRecordLen))
	if r, err := NewRecordReader(bytes.NewReader(big), FromClient).Next(); err != nil || len(r.Payload) != MaxRecordLen {
		t.Fatal(err)
	}
	if _, err := AppendRecord(nil, TDecoded, make([]byte, MaxRecordLen+1)); err == nil {
		t.Fatal("oversized record encoded")
	}
	if _, err := AppendRecord(nil, 0x80, nil); err == nil {
		t.Fatal("reserved type encoded")
	}
}

func TestTypeProperties(t *testing.T) {
	for typ := range typeNames {
		if typ.Direction() == 0 || typ.Reserved() || !typ.Known() {
			t.Errorf("%s", typ)
		}
	}
	if TGoodbyeC.String() != "GOODBYE" || RecordType(0x5a).String() != "0x5a" {
		t.Error(RecordType(0x5a).String())
	}
	if !TAck.Control() || TFrames.Control() || !TClaim.Control() || TReceiver.Control() {
		t.Error("control set")
	}
	if !TBackfill.HasSeq() || TPong.HasSeq() || TClaim.HasSeq() {
		t.Error("seq set")
	}
	if ActionFor("whatever") != ActionBackoff || ActionFor(CodeReplaced) != ActionNoRetry || ActionFor(CodeRevoked) != ActionStop {
		t.Error("actions")
	}
}
