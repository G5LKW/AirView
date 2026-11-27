package modes

import (
	"reflect"
	"testing"
)

func TestBDS20(t *testing.T) {
	for frame, want := range map[string]string{
		bookBDS20: "KLM1017", // The 1090 MHz Riddle
		ownBDS20:  "SWR93KE", // self-generated
		ownBDS20b: "AFR1745",
		ownBDS20c: "EIN6B",
	} {
		cs, ok := DecodeBDS20(mbOf(t, frame))
		if !ok || TrimCallsign(cs) != want {
			t.Errorf("DecodeBDS20(%s) = %q, %v; want %q", frame, cs[:], ok, want)
		}
		// Decode surfaces an unmistakable BDS 2,0 callsign on the message.
		m := mustDecode(t, frame)
		if !m.Has(FCallsign|FCommB) || m.CallsignString() != want || m.Has(FCategory) {
			t.Errorf("%s: message callsign %q valid=%v", frame, m.CallsignString(), m.Has(FCallsign))
		}
	}
	if _, ok := DecodeBDS20(0x212CC371C31DE0); ok {
		t.Error("register code 2,1 accepted as 2,0")
	}
	if _, ok := DecodeBDS20(0x20000000000000); ok {
		t.Error("characters outside A-Z 0-9 space accepted")
	}
}

func TestBDS40(t *testing.T) {
	// Self-generated: MCP 4000 ft, FMS 4000 ft, 1019.2 hPa.
	v := DecodeBDS40(mbOf(t, ownBDS40b))
	if !v.MCPAltOK || v.MCPAlt != 4000 || !v.FMSAltOK || v.FMSAlt != 4000 || !v.BaroOK || !near(v.Baro, 1019.2, 1e-9) {
		t.Errorf("%+v", v)
	}
	if v.ModesOK || v.SourceOK {
		t.Errorf("mode/source bits are not populated in this message: %+v", v)
	}
	// The 1090 MHz Riddle, EHS chapter: 24000 / 24000 / 1013.2.
	v = DecodeBDS40(mbOf(t, bookBDS40))
	if v.MCPAlt != 24000 || v.FMSAlt != 24000 || !near(v.Baro, 1013.2, 1e-9) || !v.MCPAltOK || !v.FMSAltOK || !v.BaroOK {
		t.Errorf("%+v", v)
	}
	if !v.ModesOK || v.VNAV || v.AltHold || v.Approach || !v.SourceOK || v.Source != 2 || v.NavModes() != 0 {
		t.Errorf("modes/source: %+v", v)
	}
	// Self-generated: MCP 37008 ft, 1021.6 hPa, no FMS.
	v = DecodeBDS40(mbOf(t, ownBDS40))
	if v.MCPAlt != 37008 || v.FMSAltOK || !near(v.Baro, 1021.6, 1e-9) {
		t.Errorf("%+v", v)
	}

	// Mode bits and target altitude source.
	mb := bds40MB(35008, -1, 1013.2)
	mb = setBits(mb, 48, 51, 0b1101) // status, VNAV, approach
	mb = setBits(mb, 54, 56, 0b111)  // status, source 3 (FMS)
	v = DecodeBDS40(mb)
	if !v.ModesOK || !v.VNAV || v.AltHold || !v.Approach || v.NavModes() != NavVNAV|NavApproach || !v.SourceOK || v.Source != 3 {
		t.Errorf("%+v", v)
	}
	if v.MCPAlt != 35008 || v.FMSAltOK || v.FMSAlt != 0 {
		t.Errorf("%+v", v)
	}
	// Cleared status bits hide the data.
	if v := DecodeBDS40(0x7FFBFFDFFFFEFB); v.MCPAltOK || v.FMSAltOK || v.BaroOK || v.ModesOK || v.SourceOK {
		t.Errorf("status bits ignored: %+v", v)
	}
}

func TestBDS50(t *testing.T) {
	// Self-generated.
	v := DecodeBDS50(mbOf(t, ownBDS50))
	if !v.RollOK || !v.TrackOK || !v.GSOK || !v.TrackRateOK || !v.TASOK {
		t.Fatalf("%+v", v)
	}
	if !near(v.Roll, -4.21875, 1e-9) || !near(v.Track, 287.578125, 1e-9) || v.GS != 446 || v.TrackRate != -0.3125 || v.TAS != 452 {
		t.Errorf("%+v", v)
	}
	// Signed roll angle: field 0x3FE = -2 steps of 45/256.
	if v := DecodeBDS50(setBits(setBits(0, 1, 1, 1), 2, 11, 0x3FE)); !near(v.Roll, -0.3515625, 1e-9) {
		t.Errorf("roll = %v, want -0.3515625", v.Roll)
	}
	// The 1090 MHz Riddle, EHS chapter: roll -9.7, track 140.273,
	// track rate -0.406, GS 476, TAS 466.
	v = DecodeBDS50(mbOf(t, bookBDS50))
	if !near(v.Roll, -9.7, 0.05) || !near(v.Track, 140.273, 0.001) || !near(v.TrackRate, -0.406, 0.001) || v.GS != 476 || v.TAS != 466 {
		t.Errorf("%+v", v)
	}
	// Tracks west of north come back in [180, 360).
	v = DecodeBDS50(bds50MB(-25, 250.3125, 320, -1.5, 334))
	if !near(v.Roll, -25, 0.1) || v.Track != 250.3125 || v.GS != 320 || v.TrackRate != -1.5 || v.TAS != 334 {
		t.Errorf("%+v", v)
	}
	// Nothing populated.
	if v := DecodeBDS50(0x7FEFFFFFDFFBFF &^ (1<<32 | 1<<21 | 1<<10)); v.RollOK || v.TrackOK || v.GSOK || v.TrackRateOK || v.TASOK {
		t.Errorf("status bits ignored: %+v", v)
	}
}

func TestBDS60(t *testing.T) {
	// Self-generated.
	v := DecodeBDS60(mbOf(t, ownBDS60))
	if !v.HeadingOK || !v.IASOK || !v.MachOK || !v.BaroRateOK || !v.InertialRateOK {
		t.Fatalf("%+v", v)
	}
	if !near(v.MagHeading, 118.4765625, 1e-9) || v.IAS != 258 || !near(v.Mach, 0.428, 1e-9) || v.BaroRate != -1600 || v.InertialRate != -1632 {
		t.Errorf("%+v", v)
	}
	// The 1090 MHz Riddle, EHS chapter.
	v = DecodeBDS60(mbOf(t, bookBDS60))
	if !near(v.MagHeading, 110.391, 0.001) || v.IAS != 259 || !near(v.Mach, 0.7, 1e-9) || v.BaroRate != -2144 || v.InertialRate != -2016 {
		t.Errorf("%+v", v)
	}
	// Headings west of north, positive rates.
	v = DecodeBDS60(bds60MB(271.0546875, 301, 0.78, 1504, 1600))
	if v.MagHeading != 271.0546875 || v.IAS != 301 || !near(v.Mach, 0.78, 1e-9) || v.BaroRate != 1504 || v.InertialRate != 1600 {
		t.Errorf("%+v", v)
	}
	// Plain two's complement of the 10-bit field (sign bit first, Doc 9871
	// Table A-2-96): 0x3FF is -32 ft/min, not level flight.
	for raw, want := range map[uint32]int32{0x000: 0, 0x200: -16384, 0x1FF: 16352, 0x3FF: -32, 0x001: 32, 0x3FE: -64, 0x201: -16352} {
		if got := bds60Rate(raw); got != want {
			t.Errorf("bds60Rate(%03X) = %d, want %d", raw, got, want)
		}
	}
}

func TestBDS44(t *testing.T) {
	// The 1090 MHz Riddle, meteorological chapter: FOM 1 (INS), wind 22 kt
	// from 344.5, temperature -48.75 C (-24.375 with the alternative
	// scaling), no pressure, no humidity.
	v := DecodeBDS44(mbOf(t, bookBDS44))
	if v.FOM != 1 || !v.WindOK || v.WindSpeed != 22 || !near(v.WindDir, 344.53, 0.01) {
		t.Errorf("%+v", v)
	}
	if v.Temp != -48.75 || v.TempAlt != -24.375 || v.PressureOK || v.HumidityOK || v.TurbulenceOK {
		t.Errorf("%+v", v)
	}
	// All fields.
	var mb uint64
	mb = setBits(mb, 1, 4, 2)
	mb = setBits(mb, 5, 5, 1)
	mb = setBits(mb, 6, 14, 85)
	mb = setBits(mb, 15, 23, 128) // 90 deg
	mb = setBits(mb, 24, 34, 60)  // +15 C
	mb = setBits(mb, 35, 35, 1)
	mb = setBits(mb, 36, 46, 850)
	mb = setBits(mb, 47, 47, 1)
	mb = setBits(mb, 48, 49, 2)
	mb = setBits(mb, 50, 50, 1)
	mb = setBits(mb, 51, 56, 32)
	v = DecodeBDS44(mb)
	want := MeteoRoutine{FOM: 2, WindOK: true, WindSpeed: 85, WindDir: 90, Temp: 15, TempAlt: 7.5,
		PressureOK: true, Pressure: 850, TurbulenceOK: true, Turbulence: 2, HumidityOK: true, Humidity: 50}
	if v != want {
		t.Errorf("got %+v\nwant %+v", v, want)
	}
}

func TestBDS45(t *testing.T) {
	// The 1090 MHz Riddle, meteorological chapter (figure "BDS 4,5 decoding
	// example"): only the static air temperature is present, -48.75 C.
	v := DecodeBDS45(mbOf(t, bookBDS45))
	want := MeteoHazard{TempOK: true, Temp: -48.75}
	if v != want {
		t.Errorf("got %+v\nwant %+v", v, want)
	}
	var mb uint64
	mb = setBits(mb, 1, 3, 0b111)   // turbulence severe
	mb = setBits(mb, 4, 6, 0b101)   // wind shear light
	mb = setBits(mb, 7, 9, 0b110)   // microburst moderate
	mb = setBits(mb, 10, 12, 0b100) // icing nil
	mb = setBits(mb, 13, 15, 0b101) // wake vortex light
	mb = setBits(mb, 16, 16, 1)
	mb = setBits(mb, 17, 26, 40) // +10 C
	mb = setBits(mb, 27, 27, 1)
	mb = setBits(mb, 28, 38, 1013)
	mb = setBits(mb, 39, 39, 1)
	mb = setBits(mb, 40, 51, 125) // 2000 ft
	v = DecodeBDS45(mb)
	want = MeteoHazard{TurbulenceOK: true, Turbulence: 3, WindShearOK: true, WindShear: 1, MicroburstOK: true, Microburst: 2,
		IcingOK: true, Icing: 0, WakeVortexOK: true, WakeVortex: 1, TempOK: true, Temp: 10,
		PressureOK: true, Pressure: 1013, RadioHeightOK: true, RadioHeight: 2000}
	if v != want {
		t.Errorf("got %+v\nwant %+v", v, want)
	}
}

func TestBDS10(t *testing.T) {
	// Self-generated BDS 1,0 data link capability reports.
	for _, want := range []DataLinkCapability{ownBDS10a, ownBDS10b} {
		if v := DecodeBDS10(bds10MB(want)); v != want {
			t.Errorf("got %+v\nwant %+v", v, want)
		}
	}
	if v := DecodeBDS10(mbOf(t, ownBDS10v5)); v != ownBDS10b {
		t.Errorf("through DF20: %+v", v)
	}
	var want DataLinkCapability
	var v DataLinkCapability
	// Remaining fields.
	var mb uint64 = 0x10 << 48
	mb = setBits(mb, 9, 9, 1)
	mb = setBits(mb, 24, 24, 1)
	mb = setBits(mb, 26, 28, 5)
	mb = setBits(mb, 29, 32, 9)
	mb = setBits(mb, 37, 37, 1)
	mb = setBits(mb, 41, 56, 0xBEEF)
	v = DecodeBDS10(mb)
	want = DataLinkCapability{Continuation: true, EnhancedProtocol: true, UplinkELM: 5, DownlinkELM: 9, ACASHybridSurv: true, DTE: 0xBEEF}
	if v != want {
		t.Errorf("got %+v\nwant %+v", v, want)
	}
}

func TestBDS17(t *testing.T) {
	// The 1090 MHz Riddle, ELS chapter: bits 1,2,3,4,5,7,9,16,17,18,24 =
	// BDS 0,5 0,6 0,7 0,8 0,9 2,0 4,0 5,0 5,1 5,2 6,0.
	g := DecodeBDS17(mbOf(t, bookBDS17))
	want := []BDS{0x05, 0x06, 0x07, 0x08, 0x09, 0x20, 0x40, 0x50, 0x51, 0x52, 0x60}
	if got := g.AppendRegisters(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("registers = %v, want %v", got, want)
	}
	for _, b := range want {
		if !g.Supports(b) {
			t.Errorf("Supports(%v) = false", b)
		}
	}
	for _, b := range []BDS{0x0A, 0x21, 0x44, 0x45, 0x5F, 0x10, 0x17, 0x30, 0x00} {
		if g.Supports(b) {
			t.Errorf("Supports(%v) = true", b)
		}
	}
	// All 24 capability bits map to distinct registers.
	all := GICBCapability(0xFFFFFF).AppendRegisters(nil)
	seen := map[BDS]bool{}
	for _, b := range all {
		seen[b] = true
	}
	if len(all) != 24 || len(seen) != 24 || !seen[BDS44] || !seen[BDS45] || !seen[0x0A] || !seen[0x5F] {
		t.Errorf("all registers: %v", all)
	}
}

func TestBDS30(t *testing.T) {
	// Threat identified by address.
	var mb uint64 = 0x30 << 48
	mb = setBits(mb, 9, 22, 0b11001010000000) // RA, corrective, upward, reversal, positive
	mb = setBits(mb, 23, 26, 0b1010)          // do not pass below, do not turn left
	mb = setBits(mb, 27, 27, 1)               // terminated
	mb = setBits(mb, 29, 30, 1)
	mb = setBits(mb, 31, 54, 0x4840D6)
	ra := DecodeACASRA(mb)
	if !ra.Active() || !ra.Corrective() || ra.Downward() || ra.IncreasedRate() || !ra.SenseReversal() || ra.AltitudeCrossing() || !ra.Positive() {
		t.Errorf("ARA flags: %014b", ra.ARA)
	}
	if ra.RAC != 0b1010 || !ra.Terminated || ra.MultipleThreat || ra.ThreatType != 1 || ra.ThreatICAO != 0x4840D6 {
		t.Errorf("%+v", ra)
	}
	if ra.ThreatAltOK || ra.ThreatRangeOK || ra.ThreatBrgOK {
		t.Errorf("altitude/range/bearing without threat type 2: %+v", ra)
	}

	// Threat identified by altitude, range and bearing.
	mb = 0x30 << 48
	mb = setBits(mb, 9, 22, 0b10110100000000) // RA, preventive, downward, increased rate, crossing
	mb = setBits(mb, 28, 28, 1)
	mb = setBits(mb, 29, 30, 2)
	mb = setBits(mb, 31, 43, uint64(ac13For(12300)))
	mb = setBits(mb, 44, 50, 36) // 3.5 NM
	mb = setBits(mb, 51, 56, 16) // 90..96 degrees
	ra = DecodeACASRA(mb)
	if !ra.Active() || ra.Corrective() || !ra.Downward() || !ra.IncreasedRate() || ra.SenseReversal() || !ra.AltitudeCrossing() || ra.Positive() {
		t.Errorf("ARA flags: %014b", ra.ARA)
	}
	if !ra.MultipleThreat || ra.ThreatType != 2 || !ra.ThreatAltOK || ra.ThreatAlt != 12300 ||
		!ra.ThreatRangeOK || !near(ra.ThreatRangeNM, 3.5, 1e-9) || !ra.ThreatBrgOK || ra.ThreatBearing != 93 || ra.ThreatICAO != 0 {
		t.Errorf("%+v", ra)
	}
	// Range and bearing special values.
	mb = setBits(mb, 44, 50, 127)
	mb = setBits(mb, 51, 56, 61)
	mb = setBits(mb, 31, 43, 0)
	ra = DecodeACASRA(mb)
	if ra.ThreatRangeNM != 12.7 || !ra.ThreatRangeOK || ra.ThreatBrgOK || ra.ThreatAltOK {
		t.Errorf("%+v", ra)
	}
	mb = setBits(mb, 44, 56, 0)
	if ra = DecodeACASRA(mb); ra.ThreatRangeOK || ra.ThreatBrgOK {
		t.Errorf("%+v", ra)
	}

	// No RA: first ARA bit and MTE clear.
	if ra := DecodeACASRA(0x30 << 48); ra.Active() {
		t.Error("empty report is active")
	}

	// Through a DF20 reply: recognised as BDS 3,0 and exposed by ACASRA().
	mb = 0x30 << 48
	mb = setBits(mb, 9, 22, 0b11000000000000)
	mb = setBits(mb, 29, 30, 1)
	mb = setBits(mb, 31, 54, 0x3C6DD2)
	var m Message
	if err := Decode(commBFrame(20, ac13For(24000), 0x4840D6, mb), &m); err != nil {
		t.Fatal(err)
	}
	got, ok := m.ACASRA()
	if !ok || !m.Has(FACASRA) || got.ThreatICAO != 0x3C6DD2 || !got.Corrective() || m.ICAO != 0x4840D6 || m.AltBaro != 24000 {
		t.Errorf("DF20 RA: ok=%v %+v", ok, got)
	}
	if b, _, ok := m.BestBDS(); !ok || b != BDS30 {
		t.Errorf("BestBDS = %v, %v", b, ok)
	}
	// A message without an RA.
	plain := mustDecode(t, bookBDS20)
	if _, ok := plain.ACASRA(); ok {
		t.Error("ACASRA on a BDS 2,0 reply")
	}
}

func TestOverlayAddress(t *testing.T) {
	// A transponder using the BDS overlay XORs the register number into the
	// top byte of the address/parity field.
	const icao = 0x4840D6
	mb := bds50MB(1, 100, 420, 0, 430)
	f := commBFrame(20, ac13For(35000), icao^0x50<<16, mb)
	var m Message
	if err := Decode(f, &m); err != nil {
		t.Fatal(err)
	}
	if m.ICAO == icao {
		t.Fatal("overlay had no effect on the parity address")
	}
	b, _, ok := m.BestBDS()
	if !ok || b != BDS50 || OverlayAddress(m.ICAO, b) != icao {
		t.Errorf("BestBDS=%v ok=%v overlay address %06X", b, ok, OverlayAddress(m.ICAO, b))
	}
	if OverlayAddress(0xFFFFFF, BDS60) != 0x9FFFFF || OverlayAddress(icao, BDSNone) != icao {
		t.Error("OverlayAddress arithmetic")
	}
}

func TestSignedField(t *testing.T) {
	for _, c := range []struct {
		raw   uint32
		width uint
		want  int32
	}{
		{0, 10, 0}, {1, 10, 1}, {511, 10, 511}, {512, 10, -512}, {1023, 10, -1},
		{1024, 11, -1024}, {2047, 11, -1}, {1023, 11, 1023},
	} {
		if got := signedField(c.raw, c.width); got != c.want {
			t.Errorf("signedField(%d, %d) = %d, want %d", c.raw, c.width, got, c.want)
		}
	}
	if field(0x80000000000001, 1, 1) != 1 || field(0x80000000000001, 56, 56) != 1 || field(0x80000000000001, 2, 55) != 0 {
		t.Error("field bit numbering")
	}
	if !flag(0x80000000000000, 1) || flag(0x80000000000000, 2) || !flag(1, 56) {
		t.Error("flag bit numbering")
	}
	if field(0x00FFFFFF000000, 9, 32) != 0xFFFFFF {
		t.Error("24-bit field")
	}
}
