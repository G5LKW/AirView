package modes

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeErrors(t *testing.T) {
	var m Message
	for _, n := range []int{0, 1, 6, 8, 13, 15, 28} {
		if err := Decode(make([]byte, n), &m); !errors.Is(err, ErrFrameLength) {
			t.Errorf("len %d: %v, want ErrFrameLength", n, err)
		}
	}
	// A short DF in a long frame and a long DF in a short frame.
	long := make([]byte, 14)
	long[0] = 4 << 3
	if err := Decode(long, &m); !errors.Is(err, ErrFrameLength) || m.DF != 4 {
		t.Errorf("DF4 in 14 bytes: %v (DF %d)", err, m.DF)
	}
	short := make([]byte, 7)
	short[0] = 17 << 3
	if err := Decode(short, &m); !errors.Is(err, ErrFrameLength) || m.DF != 17 {
		t.Errorf("DF17 in 7 bytes: %v (DF %d)", err, m.DF)
	}
	// Formats this package does not decode.
	for _, df := range []uint8{1, 2, 3, 6, 7, 8, 9, 10, 12, 13, 14, 15, 19, 22, 23} {
		f := make([]byte, FrameLen(df))
		f[0] = df << 3
		err := Decode(f, &m)
		if !errors.Is(err, ErrUnsupportedDF) {
			t.Errorf("DF%d: %v, want ErrUnsupportedDF", df, err)
		}
		if m.DF != df || int(m.Len) != len(f) || m.CRC != CRCUnknown || m.Valid != 0 {
			t.Errorf("DF%d: DF=%d Len=%d CRC=%v Valid=%b", df, m.DF, m.Len, m.CRC, m.Valid)
		}
	}
	if _, err := DecodeHex("8D40621D58C382D690C8AC2863"); !errors.Is(err, ErrFrameLength) {
		t.Errorf("DecodeHex(short): %v", err)
	}
	if _, err := DecodeHex("8D40621D58C382D690C8AC2863AZ"); !errors.Is(err, ErrFrameLength) {
		t.Errorf("DecodeHex(non-hex): %v", err)
	}
}

func TestDecodeOverwritesMessage(t *testing.T) {
	// A Message reused for the next frame must not keep anything of the
	// previous one, and the caller's buffer must not be retained or changed.
	var m Message
	buf := hexFrame(t, ownBDS20c) // DF20 with BDS 2,0
	if err := Decode(buf, &m); err != nil {
		t.Fatal(err)
	}
	if m.NumBDS == 0 || !m.Has(FCallsign) {
		t.Fatal("precondition: Comm-B message with callsign")
	}
	copy(buf, df11Frame(5, 0xA61F3C, 0)) // reuse the buffer
	if err := Decode(buf[:7], &m); err != nil {
		t.Fatal(err)
	}
	var want Message
	if err := Decode(df11Frame(5, 0xA61F3C, 0), &want); err != nil {
		t.Fatal(err)
	}
	if m != want {
		t.Fatalf("stale state after reuse:\n got %+v\nwant %+v", m, want)
	}
	if m.NumBDS != 0 || m.Has(FCallsign) || m.Has(FAltBaro) || m.AddressFromParity {
		t.Fatal("fields of the previous frame survived")
	}

	orig := hexFrame(t, bookPosEven)
	in := append([]byte(nil), orig...)
	flipBit(in, 50)
	damaged := append([]byte(nil), in...)
	if err := DecodeOpts(in, &m, Options{FixErrors: 1}); err != nil {
		t.Fatal(err)
	}
	if string(in) != string(damaged) {
		t.Fatal("DecodeOpts modified the caller's frame")
	}
	if string(m.Raw[:14]) != string(orig) {
		t.Fatal("Message.Raw must hold the repaired frame")
	}
}

func TestDecodeFrameWrappers(t *testing.T) {
	a, err := DecodeFrame(hexFrame(t, bookIdent))
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeHex(strings.ToLower(bookIdent)) // lower case is fine
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a.CallsignString() != "KLM1023" {
		t.Fatalf("wrappers disagree or wrong callsign %q", a.CallsignString())
	}
	if !a.Long() || a.ME() != 0x202CC371C32CE0 {
		t.Fatalf("Long=%v ME=%X", a.Long(), a.ME())
	}
}

func TestDF11(t *testing.T) {
	// The 1090 MHz Riddle, all-call reply chapter:
	// 5D484FDEA248F5 = ICAO 484FDE, CA 5, CRC remainder 22 = SI code 6.
	m := mustDecode(t, bookAllCall)
	if m.DF != 11 || m.ICAO != 0x484FDE || m.CA != 5 || !m.Has(FCapability|FIID) {
		t.Fatalf("%+v", m)
	}
	if m.CRC != CRCInterrogator || m.IID != 22 || m.Residual != 22 {
		t.Fatalf("CRC=%v IID=%d residual=%d", m.CRC, m.IID, m.Residual)
	}
	if code, si, ok := m.Interrogator(); !ok || !si || code != 6 {
		t.Fatalf("Interrogator = %d si=%v ok=%v, want SI 6", code, si, ok)
	}
	if m.AddressFromParity || m.AddrType != AddrModeS || m.Source != SourceModeS || m.AirGround != AGAirborne {
		t.Fatalf("addrFromParity=%v type=%v src=%v ag=%v", m.AddressFromParity, m.AddrType, m.Source, m.AirGround)
	}

	// Self-generated all-call replies: II 0 and II 1.
	for _, c := range []struct {
		frame string
		icao  uint32
		iid   uint8
		crc   CRCStatus
	}{
		{fhex(df11Frame(5, 0xA61F3C, 0)), 0xA61F3C, 0, CRCOK},
		{fhex(df11Frame(5, 0xA0B0C0, 0)), 0xA0B0C0, 0, CRCOK},
		{fhex(df11Frame(5, 0x00F00D, 0)), 0x00F00D, 0, CRCOK},
		{fhex(df11Frame(5, 0xAD3E19, 1)), 0xAD3E19, 1, CRCInterrogator},
	} {
		m := mustDecode(t, c.frame)
		if m.ICAO != c.icao || m.IID != c.iid || m.CRC != c.crc || m.CA != 5 {
			t.Errorf("%s: icao=%06X iid=%d crc=%v ca=%d", c.frame, m.ICAO, m.IID, m.CRC, m.CA)
		}
		if code, si, ok := m.Interrogator(); !ok || si || code != c.iid {
			t.Errorf("%s: Interrogator = %d si=%v ok=%v", c.frame, code, si, ok)
		}
	}

	// Every legal interrogator overlay is accepted, everything else is not.
	base := df11Frame(5, 0xA61F3C, 0)
	for overlay := uint32(0); overlay < 256; overlay++ {
		f := append([]byte(nil), base...)
		SetParity(f, overlay)
		var m Message
		err := Decode(f, &m)
		switch {
		case overlay == 0:
			if err != nil || m.CRC != CRCOK {
				t.Fatalf("overlay 0: %v %v", err, m.CRC)
			}
		case overlay <= 79:
			code, si, _ := m.Interrogator()
			wantSI := overlay >= 16
			wantCode := uint8(overlay)
			if wantSI {
				wantCode -= 16
			}
			if err != nil || m.CRC != CRCInterrogator || si != wantSI || code != wantCode {
				t.Fatalf("overlay %d: err=%v crc=%v code=%d si=%v", overlay, err, m.CRC, code, si)
			}
		default:
			if !errors.Is(err, ErrBadCRC) || m.CRC != CRCBad {
				t.Fatalf("overlay %d: err=%v crc=%v, want rejection", overlay, err, m.CRC)
			}
		}
	}
	// CA 4 = on the ground.
	f := append([]byte(nil), base...)
	f[0] = 11<<3 | 4
	SetParity(f, 0)
	var g Message
	if err := Decode(f, &g); err != nil || g.AirGround != AGGround || g.CA != 4 {
		t.Fatalf("CA4: err=%v ag=%v", err, g.AirGround)
	}
	if _, _, ok := mustDecodePtr(t, bookDF4).Interrogator(); ok {
		t.Error("Interrogator must not be available on a DF4")
	}
}

func mustDecodePtr(t testing.TB, s string) *Message {
	m := mustDecode(t, s)
	return &m
}

func TestDF0(t *testing.T) {
	// Self-generated: VS 0, CC 1, SL 7, RI 3, 45000 ft (25 ft code).
	m := mustDecode(t, ownDF0)
	if m.DF != 0 || m.ICAO != 0x0D1A7E || !m.AddressFromParity || m.CRC != CRCAddressParity {
		t.Fatalf("df=%d icao=%06X fromParity=%v crc=%v", m.DF, m.ICAO, m.AddressFromParity, m.CRC)
	}
	if !m.Has(FACAS|FAltBaro) || m.VS || !m.CC || m.SL != 7 || m.RI != 3 || m.AltBaro != 45000 || m.AltBaroGillham {
		t.Fatalf("vs=%v cc=%v sl=%d ri=%d alt=%d", m.VS, m.CC, m.SL, m.RI, m.AltBaro)
	}
	if m.AirGround != AGUncertain {
		t.Fatalf("air/ground = %v, want uncertain (VS=0)", m.AirGround)
	}

	// Gillham (100 ft) altitude code: 10600 ft.
	m = mustDecode(t, fhex(df0Frame(false, false, 0, 0, EncodeID13(gillhamEncode(10600)), 0xAB12C3)))
	if m.ICAO != 0xAB12C3 || m.AltBaro != 10600 || !m.Has(FAltBaro) || !m.AltBaroGillham {
		t.Fatalf("icao=%06X alt=%d gillham=%v", m.ICAO, m.AltBaro, m.AltBaroGillham)
	}

	// On the ground (VS 1), no altitude.
	m = mustDecode(t, fhex(df0Frame(true, false, 0, 0, 0, 0xA3B3C3)))
	if m.ICAO != 0xA3B3C3 || !m.VS || m.AirGround != AGGround || m.Has(FAltBaro) {
		t.Fatalf("icao=%06X vs=%v ag=%v alt valid=%v", m.ICAO, m.VS, m.AirGround, m.Has(FAltBaro))
	}
}

func TestDF16(t *testing.T) {
	// Self-generated: VS 0, SL 7, RI 3, 35000 ft, an MV that is not BDS 3,0.
	m := mustDecode(t, fhex(df16Frame(false, 7, 3, ac13For(35000), 0x58A1C3D2E4F506, 0xAC12E9)))
	if m.DF != 16 || m.ICAO != 0xAC12E9 || !m.AddressFromParity || m.AltBaro != 35000 || !m.Has(FACAS|FAltBaro) {
		t.Fatalf("df=%d icao=%06X alt=%d", m.DF, m.ICAO, m.AltBaro)
	}
	if m.VS || m.SL != 7 || m.RI != 3 || m.CC {
		t.Fatalf("vs=%v sl=%d ri=%d cc=%v", m.VS, m.SL, m.RI, m.CC)
	}
	if m.MB() != 0x58A1C3D2E4F506 {
		t.Fatalf("MV = %X", m.MB())
	}
	if _, ok := m.ACASRA(); ok {
		t.Fatal("MV does not start with VDS 3,0: no RA expected")
	}

	// A coordination reply carrying an RA (VDS = 0x30).
	f := make([]byte, 14)
	f[0] = 16 << 3
	f[4] = 0x30
	f[5] = 0xE0 // ARA: bit 9 (RA active), corrective, downward
	f[7] = 0x10 // MTE
	SetParity(f, 0x400123)
	var r Message
	if err := Decode(f, &r); err != nil {
		t.Fatal(err)
	}
	ra, ok := r.ACASRA()
	if !ok || !r.Has(FACASRA) || !ra.Active() || !ra.Corrective() || !ra.Downward() || !ra.MultipleThreat || r.ICAO != 0x400123 {
		t.Fatalf("ok=%v ra=%+v", ok, ra)
	}
}

func TestDF4(t *testing.T) {
	// The 1090 MHz Riddle (surveillance reply chapter) and self-generated
	// altitude replies.
	for _, c := range []struct {
		frame string
		alt   int32
		icao  uint32
	}{
		{bookDF4, 36000, 0},
		{fhex(df4Frame(0, 0, 0, ac13For(36000), 0x4D0C21)), 36000, 0x4D0C21},
		{fhex(df4Frame(0, 0, 0, ac13For(29000), 0xA3E1C0)), 29000, 0xA3E1C0},
	} {
		m := mustDecode(t, c.frame)
		if m.DF != 4 || !m.Has(FAltBaro|FFlightStatus|FAlertSPI) || m.AltBaro != c.alt || m.AltBaroGillham {
			t.Errorf("%s: df=%d alt=%d valid=%b", c.frame, m.DF, m.AltBaro, m.Valid)
		}
		if !m.AddressFromParity || m.CRC != CRCAddressParity || m.Has(FSquawk) || m.FS != 0 || m.Alert || m.SPI {
			t.Errorf("%s: fromParity=%v crc=%v fs=%d", c.frame, m.AddressFromParity, m.CRC, m.FS)
		}
		if c.icao != 0 && m.ICAO != c.icao {
			t.Errorf("%s: icao = %06X, want %06X", c.frame, m.ICAO, c.icao)
		}
	}

	// UM = IIS 9, IDS 1; DR 1.
	m := mustDecode(t, fhex(df4Frame(0, 1, 9<<2|1, ac13For(15000), 0x4D0C21)))
	if m.UM>>2 != 9 || m.UM&3 != 1 || m.DR != 1 {
		t.Errorf("UM=%d (IIS %d, IDS %d) DR=%d", m.UM, m.UM>>2, m.UM&3, m.DR)
	}
}

func TestDF5(t *testing.T) {
	// The 1090 MHz Riddle, surveillance reply chapter: squawk 0356, FS 2
	// (alert, airborne), DR 0, address 510AF9.
	m := mustDecode(t, bookDF5)
	if m.DF != 5 || !m.Has(FSquawk|FFlightStatus|FAlertSPI) || m.Squawk != 0x0356 || m.SquawkString() != "0356" {
		t.Fatalf("df=%d squawk=%04X", m.DF, m.Squawk)
	}
	if m.FS != 2 || m.DR != 0 || !m.Alert || m.SPI || m.AirGround != AGUncertain || m.ICAO != 0x510AF9 {
		t.Fatalf("fs=%d dr=%d alert=%v spi=%v ag=%v icao=%06X", m.FS, m.DR, m.Alert, m.SPI, m.AirGround, m.ICAO)
	}
	if m.Has(FAltBaro) {
		t.Fatal("identity reply has no altitude")
	}
	// Self-generated: FS 1 (ground), squawk 7123.
	m = mustDecode(t, fhex(df5Frame(1, 0, 0, 0x7123, 0x3C4E5F)))
	if m.SquawkString() != "7123" || m.AirGround != AGGround || m.ICAO != 0x3C4E5F {
		t.Fatalf("squawk=%s ag=%v icao=%06X", m.SquawkString(), m.AirGround, m.ICAO)
	}
}
func TestFlightStatus(t *testing.T) {
	for _, c := range []struct {
		fs         uint8
		alert, spi bool
		ag         AirGround
		valid      bool
	}{
		{0, false, false, AGUncertain, true},
		{1, false, false, AGGround, true},
		{2, true, false, AGUncertain, true},
		{3, true, false, AGGround, true},
		{4, true, true, AGUnknown, true},
		{5, false, true, AGUnknown, true},
		{6, false, false, AGUnknown, false},
		{7, false, false, AGUnknown, false},
	} {
		for _, df := range []uint8{4, 5, 20, 21} {
			f := make([]byte, FrameLen(df))
			f[0] = df<<3 | c.fs
			f[1] = 0x1F<<3 | 5 // DR=31, UM high bits
			f[2] = 0xA0        // UM low bits (101)
			SetParity(f, 0xABCDEF)
			var m Message
			if err := Decode(f, &m); err != nil {
				t.Fatalf("DF%d FS%d: %v", df, c.fs, err)
			}
			if m.FS != c.fs || m.Alert != c.alert || m.SPI != c.spi || m.AirGround != c.ag || m.Has(FAlertSPI) != c.valid {
				t.Errorf("DF%d FS%d: alert=%v spi=%v ag=%v valid=%v", df, c.fs, m.Alert, m.SPI, m.AirGround, m.Has(FAlertSPI))
			}
			if m.DR != 31 || m.UM != 0x2D || m.ICAO != 0xABCDEF || !m.Has(FFlightStatus) {
				t.Errorf("DF%d FS%d: DR=%d UM=%02X icao=%06X", df, c.fs, m.DR, m.UM, m.ICAO)
			}
		}
	}
}

func TestDF20DF21Header(t *testing.T) {
	// Self-generated: altitude code and identity code of Comm-B replies, and
	// the addresses recovered from the parity field.
	m := mustDecode(t, fhex(commBFrame(20, ac13For(32300), 0x4B8E21, 0)))
	if m.DF != 20 || m.AltBaro != 32300 || !m.Has(FAltBaro) || m.Has(FCommB) || m.NumBDS != 0 {
		t.Fatalf("df=%d alt=%d commb=%v n=%d", m.DF, m.AltBaro, m.Has(FCommB), m.NumBDS)
	}
	m = mustDecode(t, fhex(commBFrame(21, EncodeID13(0x2614), 0x3E11A0, bds60MB(300, 250, 0.6, 0, 0))))
	if m.DF != 21 || m.SquawkString() != "2614" || m.Has(FAltBaro) || !m.Has(FCommB) {
		t.Fatalf("df=%d squawk=%s", m.DF, m.SquawkString())
	}
	for _, c := range []struct {
		frame string
		icao  uint32
	}{
		{bookParity, 0x3C6DD0},
		{ownBDS40, 0x400A77},
		{ownBDS50, 0x3C5EF1},
		{fhex(commBFrame(21, EncodeID13(0x6246), 0xA95E01, 0x58A1C3D2E4F506)), 0xA95E01},
		{fhex(commBFrame(20, ac13For(39000), 0xAA6F11, bds20MB("SWA1234"))), 0xAA6F11},
	} {
		m := mustDecode(t, c.frame)
		if m.ICAO != c.icao || !m.AddressFromParity || m.CRC != CRCAddressParity || m.AddrType != AddrModeS {
			t.Errorf("%s: icao=%06X fromParity=%v", c.frame, m.ICAO, m.AddressFromParity)
		}
	}
}

func TestDF24(t *testing.T) {
	// Self-generated Comm-D (DF24..31 all map to 24): first bits 11, address
	// 5C3D11 on the parity.
	f := []byte{0xDA, 0xCA, 0x7F, 0x82, 0x61, 0x3C, 0x2D, 0xB1, 0x4A, 0x49, 0xC5, 0, 0, 0}
	SetParity(f, 0x5C3D11)
	m := mustDecode(t, fhex(f))
	if m.DF != 24 || m.ICAO != 0x5C3D11 || !m.AddressFromParity || m.Source != SourceModeS || m.Valid != 0 {
		t.Fatalf("df=%d icao=%06X valid=%b", m.DF, m.ICAO, m.Valid)
	}
	if FrameDF(m.Raw[:]) != 24 || DFIndex(m.DF) != DFIndexOther {
		t.Fatal("DF24 helpers")
	}
}
func TestAddrKey(t *testing.T) {
	m := mustDecode(t, bookIdent)
	if m.AddrKey() != 0x4840D6 {
		t.Errorf("ICAO key = %X", m.AddrKey())
	}
	// DF18 CF=1: non-ICAO address.
	n, err := DecodeFrame(esFrame(0x91, 0x4840D6, 0x202CC371C32CE0))
	if err != nil {
		t.Fatal(err)
	}
	if n.AddrKey() != 0x4840D6|NonICAOFlag || FormatICAO(n.AddrKey()) != "~4840D6" {
		t.Errorf("non-ICAO key = %X (%s)", n.AddrKey(), FormatICAO(n.AddrKey()))
	}
}
