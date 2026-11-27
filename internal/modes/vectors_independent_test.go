package modes

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

// Independent verification vectors: self-generated frames (expected values
// are the builder inputs), the worked examples of "The 1090 Megahertz
// Riddle" (book* constants) and tables of the standards themselves. See
// synth_test.go for the provenance rules.

// cprOf extracts F, lat and lon of a position frame by plain bit slicing,
// independently of the decoder.
func cprOf(t testing.TB, s string) (odd bool, lat, lon uint32) {
	t.Helper()
	me := mbOf(t, s)
	return me>>34&1 == 1, uint32(me>>17) & 0x1FFFF, uint32(me) & 0x1FFFF
}

// ---------------------------------------------------------------- CRC

func TestIndepCRC(t *testing.T) {
	// Book frames: the transmitted parity is the CRC of the rest.
	for _, s := range []string{bookIdent, bookPosEven, bookPosOdd, bookVelGS, bookVelAS, bookSurf, bookSurfEven, bookSurfOdd} {
		if r := Residual(hexFrame(t, s)); r != 0 {
			t.Errorf("Residual(%s) = %06X, want 0", s, r)
		}
	}
	// Self-generated frames with every kind of overlay: the residual is the
	// overlay, whatever the payload, and the long division agrees.
	for i, c := range []struct {
		f    []byte
		want uint32
	}{
		{esFrame(0x8D, 0x3A5C71, identME(4, 2, "TST4711")), 0},
		{esFrame(0x90, 0x00A0B1, posME(11, 23475, true, 47.1, 8.6)), 0},
		{df11Frame(5, 0x3A5C71, 0), 0},
		{df11Frame(5, 0x3A5C71, 22), 22},
		{df4Frame(0, 0, 0, ac13For(18000), 0x4D2210), 0x4D2210},
		{df5Frame(2, 0, 0, 0x4521, 0xC0FFEE), 0xC0FFEE},
		{commBFrame(20, ac13For(31000), 0x8A21F0, bds60MB(12, 280, 0.76, 0, 0)), 0x8A21F0},
		{commBFrame(21, EncodeID13(0x7031), 0x0D1E2F, bds50MB(-3, 97, 451, -0.25, 440)), 0x0D1E2F},
	} {
		if r := Residual(c.f); r != c.want {
			t.Errorf("vector %d: Residual = %06X, want %06X", i, r, c.want)
		}
		n := len(c.f)
		if p := crcBitwise(c.f[:n-3]) ^ uint32(c.f[n-3])<<16 ^ uint32(c.f[n-2])<<8 ^ uint32(c.f[n-1]); p != c.want {
			t.Errorf("vector %d: long division gives %06X", i, p)
		}
	}
	// One flipped bit at x^4 (bit 107) leaves residual 2^4 = 16.
	f := esFrame(0x8D, 0x3A5C71, identME(4, 2, "TST4711"))
	flipBit(f, 107)
	if r := Residual(f); r != 16 {
		t.Errorf("Residual with bit 107 flipped = %d, want 16", r)
	}
	// Book identification frame: parity of the first 11 bytes is 576098.
	f = hexFrame(t, bookIdent)
	if c := Checksum(f[:11]); c != 0x576098 {
		t.Errorf("Checksum = %06X, want 576098", c)
	}
	// 1090 MHz Riddle, Mode S basics: remainder CE2CA7, address 3C6DD0.
	f = hexFrame(t, bookParity)
	if c := Checksum(f[:11]); c != 0xCE2CA7 {
		t.Errorf("Checksum = %06X, want CE2CA7", c)
	}
	if m := mustDecode(t, bookParity); m.ICAO != 0x3C6DD0 || !m.AddressFromParity {
		t.Errorf("ICAO %06X", m.ICAO)
	}
	// Addresses: in the AA field (DF17) or recovered from the parity (DF20/21).
	for s, want := range map[string]uint32{
		bookIdent: 0x4840D6,
		esHex(0x8D, 0xE40017, identME(4, 1, "PTB2001")):                                0xE40017,
		fhex(commBFrame(20, ac13For(38000), 0x400941, bds40MB(38000, -1, 1013.2))):     0x400941,
		fhex(commBFrame(21, EncodeID13(0x2000), 0x71C0DE, bds50MB(1, 2, 300, 0, 310))): 0x71C0DE,
	} {
		if m := mustDecode(t, s); m.ICAO != want {
			t.Errorf("%s: ICAO %06X, want %06X", s, m.ICAO, want)
		}
	}
}

// Every one and two bit error pattern must have its own syndrome, otherwise
// the repair could "fix" the wrong bits.
func TestIndepSyndromesUnique(t *testing.T) {
	single := bitSyndromes()
	// Independent derivation of the single bit syndromes.
	for i := 0; i < 112; i++ {
		var f [14]byte
		f[i>>3] = 0x80 >> uint(i&7)
		if r := Residual(f[:]); r != single[i] {
			t.Fatalf("bit %d: syndrome %06X, Residual %06X", i, single[i], r)
		}
	}
	seen := map[uint32][2]int{}
	for i := 5; i < 112; i++ {
		if _, dup := seen[single[i]]; dup || single[i] == 0 {
			t.Fatalf("duplicate single bit syndrome for bit %d", i)
		}
		seen[single[i]] = [2]int{i, -1}
	}
	for i := 5; i < 112; i++ {
		for j := i + 1; j < 112; j++ {
			s := single[i] ^ single[j]
			if p, dup := seen[s]; dup || s == 0 {
				t.Fatalf("syndrome %06X of bits %d,%d also produced by %v", s, i, j, p)
			}
			seen[s] = [2]int{i, j}
		}
	}
}

func TestIndepErrorCorrection(t *testing.T) {
	good := hexFrame(t, bookPosEven)
	var want Message
	if err := Decode(good, &want); err != nil {
		t.Fatal(err)
	}
	var m Message
	for i := 0; i < 112; i++ {
		f := append([]byte(nil), good...)
		f[i>>3] ^= 0x80 >> uint(i&7)
		if err := Decode(f, &m); err == nil && i >= 5 { // a damaged DF field makes it another format
			t.Fatalf("bit %d: damaged frame accepted without correction", i)
		}
		err := DecodeOpts(f, &m, Options{FixErrors: 1})
		if i < 5 {
			if err == nil && m.DF == 17 {
				t.Errorf("bit %d: DF bit repaired", i)
			}
			continue
		}
		if err != nil {
			t.Errorf("bit %d: not repaired: %v", i, err)
			continue
		}
		if m.CRC != CRCFixed1 || m.FixedBits != 1 || int(m.FixedBit[0]) != i || m.Raw != want.Raw ||
			m.ICAO != want.ICAO || m.AltBaro != want.AltBaro || m.CPRLat != want.CPRLat {
			t.Errorf("bit %d: repair gave %v bits %v ICAO %06X", i, m.CRC, m.FixedBit, m.ICAO)
		}
		if m.AddressCorrected() != (i >= 8 && i < 32) {
			t.Errorf("bit %d: AddressCorrected = %v", i, m.AddressCorrected())
		}
		if m.Residual == 0 {
			t.Errorf("bit %d: Residual must be the one of the frame as received", i)
		}
	}
	// Two bit errors: DF17 only, and only on request.
	rng := rand.New(rand.NewSource(7))
	for n := 0; n < 300; n++ {
		i, j := 5+rng.Intn(107), 5+rng.Intn(107)
		if i == j {
			continue
		}
		f := append([]byte(nil), good...)
		f[i>>3] ^= 0x80 >> uint(i&7)
		f[j>>3] ^= 0x80 >> uint(j&7)
		if err := DecodeOpts(f, &m, Options{FixErrors: 1}); err == nil {
			t.Fatalf("bits %d,%d: two bit error accepted with FixErrors=1", i, j)
		}
		if err := DecodeOpts(f, &m, Options{FixErrors: 2}); err != nil || m.CRC != CRCFixed2 || m.Raw != want.Raw {
			t.Fatalf("bits %d,%d: not repaired (%v, %v)", i, j, err, m.CRC)
		}
		// The same damage on a DF18 frame must be refused.
		g := append([]byte(nil), good...)
		g[0] = 0x90
		SetParity(g, 0)
		g[i>>3] ^= 0x80 >> uint(i&7)
		g[j>>3] ^= 0x80 >> uint(j&7)
		if err := DecodeOpts(g, &m, Options{FixErrors: 2}); err == nil {
			t.Fatalf("bits %d,%d: two bit repair applied to DF18", i, j)
		}
	}
	// Address/parity formats can never be repaired.
	c := commBFrame(20, ac13For(30275), 0x5B0E13, bds50MB(2, 114, 438, 0.125, 424))
	c[6] ^= 0x10
	if err := DecodeOpts(c, &m, Options{FixErrors: 2}); err != nil || m.FixedBits != 0 || m.ICAO == 0x5B0E13 {
		t.Errorf("DF20 with a bit error: err %v fixed %d ICAO %06X", err, m.FixedBits, m.ICAO)
	}
}

// ---------------------------------------------------------------- altitude

func TestIndepGillham(t *testing.T) {
	// Bit strings D2 D4 A1 A2 A4 B1 B2 B4 C1 C2 C4 worked out by hand from
	// h = 500*N500 + 100*N100 - 1300 (N500 reflected Gray code on D2..B4,
	// N100 on C1 C2 C4 as 1=C4 2=C2C4 3=C2 4=C1C2 5=C1, reversed when N500 is
	// odd). Example: 1300 ft -> N500 5 (Gray 00000111), N100 1 -> 5 (C1).
	masks := [11]uint16{0x0002, 0x0004, 0x1000, 0x2000, 0x4000, 0x0100, 0x0200, 0x0400, 0x0010, 0x0020, 0x0040}
	for s, want := range map[string]int32{
		"00000000001": -1200,  // N500 0, N100 1
		"00000001100": -700,   // N500 1 (Gray 1), N100 1 reversed -> 5
		"00000011001": -200,   // N500 2 (Gray 3), N100 1
		"00000011010": 0,      // N500 2, N100 3
		"00000111100": 1300,   // N500 5 (Gray 7), N100 1 reversed -> 5
		"00001000110": 6400,   // N500 15 (Gray 8), N100 2 reversed -> 4
		"00010110010": 12500,  // N500 27 (Gray 22), N100 3 reversed -> 3
		"00100001011": 29900,  // N500 62 (Gray 33), N100 2
		"01111111001": 41700,  // N500 85 (Gray 127), N100 5 reversed -> 1
		"11101110001": 88800,  // N500 180 (Gray 238), N100 1
		"10000000001": 126700, // N500 255 (Gray 128), N100 5 reversed -> 1
	} {
		var modeA uint16
		for i, c := range s {
			if c == '1' {
				modeA |= masks[i]
			}
		}
		ft, ok := GillhamAltitude(modeA)
		if !ok || ft != want {
			t.Errorf("GillhamAltitude(%s) = %d, %v; want %d", s, ft, ok, want)
		}
		// The same code inside a 13-bit AC field (Q=0, M=0).
		if v, kind := decodeAC13(EncodeID13(modeA)); kind != altGillham || v != want {
			t.Errorf("decodeAC13(gray %s) = %d kind %d", s, v, kind)
		}
		if enc, ok := EncodeGillham(want); !ok || enc != modeA {
			t.Errorf("EncodeGillham(%d) = %04X, want %04X", want, enc, modeA)
		}
	}
	// C1=1 C2=0 C4=1 is not one of the five legal 100 ft digits.
	if _, kind := decodeAC13(1<<12 | 1<<8); kind != altInvalid {
		t.Errorf("illegal Gillham code accepted (kind %d)", kind)
	}
	if _, kind := decodeAC13(0); kind != altNone {
		t.Errorf("zero altitude code: kind %d", kind)
	}
	// Hand derivation in the 13-bit AC layout.
	if v, kind := decodeAC13(1<<10 | 1<<9 | 1<<5 | 1<<1 | 1<<0); kind != altGillham || v != 50000 {
		// C2=1 only -> n100 = 3; n500 Gray 01010101 = 102 -> 102*500+300-1300.
		t.Errorf("Gillham 102/3 = %d kind %d, want 50000", v, kind)
	}
	if v, kind := decodeAC13(0x1030); kind != altQ25 || v != 25000 {
		t.Errorf("AC 0x1030 = %d", v)
	}
	if v, kind := decodeAC13(0x1010); kind != altQ25 || v != 24600 {
		t.Errorf("AC 0x1010 = %d", v)
	}
}

func TestIndepAltitudes(t *testing.T) {
	// Negative and small altitudes in self-generated position messages,
	// 12-bit field built by hand: N = (alt+1000)/25 with Q=1 after bit 4.
	for _, want := range []int32{-1000, -975, -350, -25, 0, 75, 1225, 4975, 41025} {
		n := uint16((want + 1000) / 25)
		ac12 := (n&0x7F0)<<1 | 0x010 | n&0x00F
		s := esHex(0x8D, 0x4CA7E1, airbornePosME(11, 0, false, ac12, false, false, 1234, 5678))
		m := mustDecode(t, s)
		if !m.Has(FAltBaro) || m.AltBaro != want || m.AltBaroGillham || m.Has(FAltGNSS) {
			t.Errorf("%s: altitude %d (valid %v), want %d", s, m.AltBaro, m.Has(FAltBaro), want)
		}
	}
	// Surveillance replies.
	m := mustDecode(t, bookDF4)
	if m.DF != 4 || !m.Has(FAltBaro) || m.AltBaro != 36000 {
		t.Errorf("DF4 altitude %d", m.AltBaro)
	}
	m = mustDecode(t, fhex(commBFrame(20, ac13For(32300), 0x4B8E21, 0)))
	if m.DF != 20 || m.AltBaro != 32300 || !m.Has(FAltBaro) {
		t.Errorf("DF20 altitude %d", m.AltBaro)
	}
	if m.Has(FCommB) || m.NumBDS != 0 {
		t.Errorf("empty MB must not be a Comm-B payload")
	}
}

// ---------------------------------------------------------------- surveillance

func TestIndepSurveillance(t *testing.T) {
	var m Message
	dec := func(f []byte) *Message {
		t.Helper()
		if err := Decode(f, &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	if dec(survFrame(0x20001030, 0x400940)); m.DF != 4 || m.ICAO != 0x400940 || m.AltBaro != 25000 || !m.Has(FAltBaro) {
		t.Errorf("DF4: %+v", m)
	}
	if dec(survFrame(0x22001030, 0x400940)); m.FS != 2 || !m.Alert || m.SPI || !m.Has(FAlertSPI) || m.AirGround != AGUncertain {
		t.Errorf("DF4 FS=2: FS %d alert %v", m.FS, m.Alert)
	}
	if dec(survFrame(0x20000000, 0x400940)); m.Has(FAltBaro) || m.Has(FAltMetric) {
		t.Errorf("DF4 AC=0 has an altitude")
	}
	if dec(survFrame(0x28000808, 0x400940)); m.DF != 5 || m.SquawkString() != "1200" {
		t.Errorf("DF5 squawk %s", m.SquawkString())
	}
	if dec(survFrame(0x28000AA2, 0x400940)); m.SquawkString() != "7500" || m.EmergencyString() != "unlawful" {
		t.Errorf("DF5 squawk %s emergency %q", m.SquawkString(), m.EmergencyString())
	}
	// Flight status table (Annex 10): 0 airborne, 1 ground, 2 alert airborne,
	// 3 alert ground, 4 alert + SPI, 5 SPI.
	for fs, w := range [6]struct {
		alert, spi bool
		ag         AirGround
	}{
		{false, false, AGUncertain}, {false, false, AGGround}, {true, false, AGUncertain},
		{true, false, AGGround}, {true, true, AGUnknown}, {false, true, AGUnknown},
	} {
		dec(survFrame(0x20001030|uint32(fs)<<24, 0xABCDEF))
		if m.FS != uint8(fs) || m.Alert != w.alert || m.SPI != w.spi || m.AirGround != w.ag || !m.Has(FAlertSPI) {
			t.Errorf("FS %d: alert %v spi %v ag %v", fs, m.Alert, m.SPI, m.AirGround)
		}
	}

	// The book's identity reply, and self-generated DR / UM / DF21 cases.
	r := mustDecode(t, bookDF5)
	if r.DF != 5 || r.FS != 2 || r.DR != 0 || r.SquawkString() != "0356" {
		t.Errorf("%s: %+v", bookDF5, r)
	}
	r = mustDecode(t, fhex(df4Frame(0, 1, 9<<2|1, ac13For(21075), 0x3E11A0)))
	if r.DF != 4 || r.DR != 1 || r.UM != 9<<2|1 || r.AltBaro != 21075 || r.ICAO != 0x3E11A0 {
		t.Errorf("DF4 DR/UM: DR %d UM %d", r.DR, r.UM)
	}
	r = mustDecode(t, fhex(commBFrame(21, EncodeID13(0x2614), 0x3E11A0, bds60MB(300, 250, 0.6, 0, 0))))
	if r.DF != 21 || r.SquawkString() != "2614" {
		t.Errorf("DF21 squawk %s", r.SquawkString())
	}

	// DF0 / DF16 (Annex 10 Vol. IV 3.1.2.8.2: VS bit 6, CC bit 7, SL bits
	// 9-11, RI bits 14-17, AC bits 20-32).
	df0 := func(vs, cc, sl, ri, ac uint32) []byte {
		return survFrame(vs<<26|cc<<25|sl<<21|ri<<15|ac, 0x400940)
	}
	if dec(df0(0, 0, 3, 2, 0x1030)); m.DF != 0 || m.VS || m.CC || m.SL != 3 || m.RI != 2 || m.AltBaro != 25000 ||
		m.ICAO != 0x400940 || !m.Has(FACAS|FAltBaro) || m.AirGround != AGUncertain {
		t.Errorf("DF0: %+v", m)
	}
	if dec(df0(1, 1, 7, 15, 0)); !m.VS || !m.CC || m.SL != 7 || m.RI != 15 || m.AirGround != AGGround || m.Has(FAltBaro) {
		t.Errorf("DF0 ground: %+v", m)
	}
	f16 := make([]byte, 14)
	f16[0], f16[2], f16[3] = 0x80, 0x10, 0x30
	for i := 4; i < 11; i++ {
		f16[i] = 0xFF
	}
	SetParity(f16, 0x400940)
	if dec(f16); m.DF != 16 || m.AltBaro != 25000 || m.ICAO != 0x400940 || m.MB() != 1<<56-1 || m.Has(FACASRA) {
		t.Errorf("DF16: %+v", m)
	}
}

func TestIndepAllCall(t *testing.T) {
	// 1090 MHz Riddle, all-call reply chapter: SI code 6.
	m := mustDecode(t, bookAllCall)
	code, si, ok := m.Interrogator()
	if m.DF != 11 || m.ICAO != 0x484FDE || m.CA != 5 || !ok || !si || code != 6 || m.CRC != CRCInterrogator ||
		m.AddressFromParity || m.AirGround != AGAirborne {
		t.Errorf("DF11: ICAO %06X CA %d IID %d (%d,%v,%v) CRC %v", m.ICAO, m.CA, m.IID, code, si, ok, m.CRC)
	}
	// Capability values.
	for ca := 0; ca < 8; ca++ {
		f := []byte{0x58 | byte(ca), 0x4C, 0xA2, 0xD4, 0, 0, 0}
		SetParity(f, 0)
		var d Message
		if err := Decode(f, &d); err != nil || d.CA != uint8(ca) || d.ICAO != 0x4CA2D4 || d.CRC != CRCOK || d.IID != 0 {
			t.Errorf("CA %d: %v %+v", ca, err, d)
		}
		wantAG := AGUnknown
		switch ca {
		case 4:
			wantAG = AGGround
		case 5:
			wantAG = AGAirborne
		}
		if d.AirGround != wantAG {
			t.Errorf("CA %d: air/ground %v", ca, d.AirGround)
		}
		// Every II and SI code is accepted, nothing beyond.
		for iid := uint32(1); iid < 128; iid++ {
			SetParity(f, iid)
			err := Decode(f, &d)
			if (err == nil) != (iid <= 79) {
				t.Fatalf("IID %d: err %v", iid, err)
			}
			if err == nil {
				c, isSI, _ := d.Interrogator()
				if d.CRC != CRCInterrogator || (iid < 16 && (isSI || uint32(c) != iid)) || (iid >= 16 && (!isSI || uint32(c) != iid-16)) {
					t.Fatalf("IID %d: %d %v", iid, c, isSI)
				}
			} else if d.DF != 11 || d.CRC != CRCBad || d.Residual != iid {
				t.Fatalf("IID %d: stats fields not set on error", iid)
			}
		}
	}
}

// ---------------------------------------------------------------- identification

func TestIndepIdentification(t *testing.T) {
	// Self-generated: TC4 category 0, a callsign with a trailing space.
	m := mustDecode(t, esHex(0x8D, 0x3C0A11, identME(4, 0, "DLH7AB")))
	if m.ICAO != 0x3C0A11 || m.TC != 4 || m.CallsignString() != "DLH7AB" || m.CategoryString() != "A0" || m.Subtype != 0 {
		t.Errorf("%q %s", m.CallsignString(), m.CategoryString())
	}
	m = mustDecode(t, bookIdent)
	if m.ICAO != 0x4840D6 || m.CallsignString() != "KLM1023" || m.CA != 5 || m.Source != SourceADSB || m.AddrType != AddrADSBICAO {
		t.Errorf("%q", m.CallsignString())
	}
}

// ---------------------------------------------------------------- velocity

func TestIndepVelocity(t *testing.T) {
	// 1090 MHz Riddle, airborne velocity chapter: subtype 1.
	m := mustDecode(t, bookVelGS)
	if m.TC != 19 || m.Subtype != 1 || math.Floor(m.GS) != 159 || !near(m.GS, 159.20, 0.01) || !near(m.Track, 182.88, 0.01) ||
		m.VertRate != -832 || m.VertRateBaro || m.GNSSDelta != 550 || !m.Has(FGroundSpeed|FTrack|FVertRate|FGNSSDelta|FNACv) ||
		m.Has(FHeading) || m.Has(FAirspeed) {
		t.Errorf("subtype 1: %+v", m)
	}
	// Subtype 3: TAS 375 kt, heading 243.98, -2304 ft/min (barometric).
	m = mustDecode(t, bookVelAS)
	if m.Subtype != 3 || m.Airspeed != 375 || !m.AirspeedTAS || !near(m.Heading, 243.984375, 1e-9) || m.VertRate != -2304 ||
		!m.VertRateBaro || !m.Has(FAirspeed|FHeading|FVertRate) || m.Has(FGroundSpeed) || m.Has(FTrack) || m.Has(FGNSSDelta) {
		t.Errorf("subtype 3: %+v", m)
	}
	// Self-generated: heading 0 with its status bit set (a zero heading is
	// valid), IAS 100 kt (field 101), +1024 ft/min GNSS (field 17).
	m = mustDecode(t, esHex(0x8D, 0x4D0123, velocityME(3, false, 0, true, 0, false, 101, false, false, 17, false, 0)))
	if m.Subtype != 3 || !m.Has(FHeading) || m.Heading != 0 || m.Airspeed != 100 || m.AirspeedTAS || m.VertRate != 1024 || m.VertRateBaro {
		t.Errorf("heading zero: %+v", m)
	}
	// Vertical rate sign bit and magnitude: rate = (field-1)*64, negative
	// when the sign bit (ME 37) is set.
	for _, c := range []struct {
		down  bool
		field uint32
		want  int32
	}{{false, 2, 64}, {false, 4, 192}, {false, 18, 1088}, {true, 2, -64}, {true, 3, -128}, {true, 64, -4032}, {false, 511, 32640}} {
		s := esHex(0x8D, 0x4D0123, velocityME(1, false, 0, false, 201, true, 51, true, c.down, c.field, false, 0))
		m := mustDecode(t, s)
		if !m.Has(FVertRate) || m.VertRate != c.want {
			t.Errorf("%s: vertical rate %d, want %d", s, m.VertRate, c.want)
		}
	}

	// All four subtypes from one hand-built layout (DO-260B 2.2.3.2.6):
	// west 300 kt / south 400 kt -> 500 kt on track 216.87.
	build := func(st uint64, b14 uint64, f15 uint64, b25 uint64, f26 uint64, vrSrc, vrSign, vr, dSign, d uint64) Message {
		var me uint64
		me = setBits(me, 1, 5, 19)
		me = setBits(me, 6, 8, st)
		me = setBits(me, 11, 13, 2)
		me = setBits(me, 14, 14, b14)
		me = setBits(me, 15, 24, f15)
		me = setBits(me, 25, 25, b25)
		me = setBits(me, 26, 35, f26)
		me = setBits(me, 36, 36, vrSrc)
		me = setBits(me, 37, 37, vrSign)
		me = setBits(me, 38, 46, vr)
		me = setBits(me, 49, 49, dSign)
		me = setBits(me, 50, 56, d)
		var m Message
		if err := Decode(esFrame(0x8D, 0x123456, me), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m = build(1, 1, 301, 1, 401, 1, 0, 11, 1, 5)
	if !near(m.GS, 500, 1e-9) || !near(m.Track, 216.8699, 1e-4) || m.VertRate != 640 || !m.VertRateBaro || m.GNSSDelta != -100 || m.NACv != 2 {
		t.Errorf("subtype 1 synthetic: GS %v track %v VR %d delta %d", m.GS, m.Track, m.VertRate, m.GNSSDelta)
	}
	m = build(2, 0, 301, 0, 401, 0, 1, 2, 0, 2) // supersonic: 4 kt steps, east/north
	if !near(m.GS, 2000, 1e-9) || !near(m.Track, 36.8699, 1e-4) || m.VertRate != -64 || m.VertRateBaro || m.GNSSDelta != 25 {
		t.Errorf("subtype 2 synthetic: GS %v track %v VR %d", m.GS, m.Track, m.VertRate)
	}
	m = build(1, 0, 1, 1, 201, 0, 0, 0, 0, 0) // due south, no vertical rate
	if !near(m.GS, 200, 1e-9) || m.Track != 180 || m.Has(FVertRate) || m.Has(FGNSSDelta) {
		t.Errorf("due south: GS %v track %v", m.GS, m.Track)
	}
	m = build(1, 1, 201, 0, 1, 0, 0, 0, 0, 0) // due west
	if !near(m.GS, 200, 1e-9) || m.Track != 270 {
		t.Errorf("due west: GS %v track %v", m.GS, m.Track)
	}
	m = build(1, 0, 0, 0, 100, 0, 0, 0, 0, 0) // one component not available
	if m.Has(FGroundSpeed) || m.Has(FTrack) {
		t.Errorf("velocity with a missing component accepted")
	}
	m = build(3, 1, 768, 0, 251, 1, 1, 33, 0, 0) // heading 270, IAS 250, -2048 baro
	if m.Heading != 270 || m.Airspeed != 250 || m.AirspeedTAS || m.VertRate != -2048 || !m.VertRateBaro {
		t.Errorf("subtype 3 synthetic: %v %v %d", m.Heading, m.Airspeed, m.VertRate)
	}
	m = build(4, 0, 768, 1, 251, 0, 0, 0, 0, 0) // supersonic airspeed, heading not available
	if m.Has(FHeading) || m.Airspeed != 1000 || !m.AirspeedTAS {
		t.Errorf("subtype 4 synthetic: %v %v", m.Has(FHeading), m.Airspeed)
	}
	for _, st := range []uint64{0, 5, 6, 7} {
		if m = build(st, 0, 100, 0, 100, 0, 0, 10, 0, 10); m.Valid&(FGroundSpeed|FTrack|FHeading|FAirspeed|FVertRate|FGNSSDelta|FNACv) != 0 {
			t.Errorf("subtype %d decoded: %v", st, m.Valid)
		}
	}
}

func TestIndepSurfaceMovement(t *testing.T) {
	// Self-generated surface positions, one per movement band (DO-260B
	// table of the movement field): code -> speed.
	for _, c := range []struct {
		hex string
		gs  float64 // -1: no information
	}{
		{esHex(0x8C, 0x3C66B1, surfME(8, 0, false, 0, false, 50.03, 8.57)), -1},
		{esHex(0x90, 0x3C66B1, surfME(8, 1, true, 10, true, 50.03, 8.57)), 0},
		{esHex(0x8C, 0x3C66B1, surfME(7, 9, true, 64, false, 50.03, 8.57)), 1},
		{esHex(0x8C, 0x3C66B1, surfME(7, 24, true, 65, true, 50.03, 8.57)), 7.5},
		{esHex(0x8C, 0x3C66B1, surfME(7, 25, true, 66, false, 50.03, 8.58)), 8},
		{esHex(0x8C, 0x3C66B1, surfME(7, 39, true, 70, true, 50.03, 8.58)), 15},
		{esHex(0x8C, 0x3C66B1, surfME(6, 94, true, 20, false, 50.04, 8.59)), 70},
		{esHex(0x8C, 0x3C66B1, surfME(6, 109, true, 20, true, 50.05, 8.60)), 100},
		{esHex(0x8D, 0x3C66B1, surfME(5, 124, true, 20, false, 50.06, 8.61)), 175},
	} {
		m := mustDecode(t, c.hex)
		if m.TC < 5 || m.TC > 8 || !m.CPRSurface || !m.Has(FCPR) || m.AirGround != AGGround || m.Has(FAltBaro) {
			t.Errorf("%s: not a surface position: %+v", c.hex, m)
		}
		if c.gs < 0 {
			if m.Has(FGroundSpeed) {
				t.Errorf("%s: GS %v, want none", c.hex, m.GS)
			}
		} else if !m.Has(FGroundSpeed) || m.GS != c.gs {
			t.Errorf("%s: GS %v (valid %v), want %v", c.hex, m.GS, m.Has(FGroundSpeed), c.gs)
		}
	}
	// Every movement code against the DO-260B table A-2-2 quantisation.
	for mov := uint32(0); mov < 128; mov++ {
		var want float64
		switch {
		case mov == 0 || mov > 124:
			want = -1
		case mov == 1:
			want = 0
		case mov <= 8:
			want = 0.125 * float64(mov-1)
		case mov <= 12:
			want = 1 + 0.25*float64(mov-9)
		case mov <= 38:
			want = 2 + 0.5*float64(mov-13)
		case mov <= 93:
			want = 15 + float64(mov-39)
		case mov <= 108:
			want = 70 + 2*float64(mov-94)
		case mov <= 123:
			want = 100 + 5*float64(mov-109)
		default:
			want = 175
		}
		me := setBits(setBits(0, 1, 5, 7), 6, 12, uint64(mov))
		var m Message
		if err := Decode(esFrame(0x8C, 1, me), &m); err != nil {
			t.Fatal(err)
		}
		if (want < 0) == m.Has(FGroundSpeed) || (want >= 0 && m.GS != want) {
			t.Errorf("movement %d: GS %v valid %v, want %v", mov, m.GS, m.Has(FGroundSpeed), want)
		}
	}
	// 1090 MHz Riddle surface example: movement 41 -> 17 kt, track 92.8 for
	// 0100001.
	m := mustDecode(t, bookSurf)
	if !m.Has(FTrack) || !near(m.Track, 92.8125, 1e-9) || !m.Has(FGroundSpeed) || m.GS != 17 || !m.CPROdd {
		t.Errorf("surface example: track %v GS %v", m.Track, m.GS)
	}
	// Self-generated DF18 (CF 7 is reserved, so CF 0: 0x90): movement 43 ->
	// 19 kt, track field 15 -> 15*360/128 = 42.1875.
	m = mustDecode(t, esHex(0x90, 0xC8F00A, surfME(7, 43, true, 15, true, -43.49, 172.54)))
	if !near(m.Track, 42.1875, 1e-9) || m.GS != 19 {
		t.Errorf("southern surface: track %v GS %v", m.Track, m.GS)
	}
}

// ---------------------------------------------------------------- status messages

func TestIndepStatusMessages(t *testing.T) {
	// Self-generated TC28 subtype 1, no emergency, squawk 3415 (ME bits
	// 12-24 hold the 13-bit identity code).
	m := mustDecode(t, esHex(0x8D, 0x4CA2B5, 28<<51|1<<48|uint64(EncodeID13(0x3415))<<32))
	if m.TC != 28 || m.Subtype != 1 || !m.Has(FEmergency) || m.Emergency != EmergencyNone || m.SquawkString() != "3415" || m.EmergencyString() != "" {
		t.Errorf("TC28: %+v", m)
	}
	// Emergency states (DO-260B table 2-78).
	for st, name := range map[uint64]string{1: "general", 2: "medical", 3: "minfuel", 4: "nordo", 5: "unlawful", 6: "downed", 7: "", 0: ""} {
		me := setBits(setBits(setBits(0, 1, 5, 28), 6, 8, 1), 9, 11, st)
		var d Message
		if err := Decode(esFrame(0x8D, 2, me), &d); err != nil || d.Emergency != uint8(st) || d.EmergencyString() != name || d.Has(FSquawk) {
			t.Errorf("emergency %d: %v %q", st, err, d.EmergencyString())
		}
	}

	// Self-generated TC29 subtype 1 (DO-260B 2.2.3.2.7.1): selected altitude
	// field 751 -> 750*32 = 24000 ft, QNH field 267 -> 800+266*0.8 = 1012.8,
	// heading field 95 -> 95*180/256 = 66.796875, NACp 9, NICbaro, SIL 3,
	// modes autopilot, VNAV, TCAS, LNAV.
	tss := ownTSSME
	tssHex := ownTSS
	m = mustDecode(t, tssHex)
	if m.TC != 29 || m.Subtype != 1 || m.SelAlt != 24000 || m.SelAltFMS || !near(m.BaroSetting, 1012.8, 1e-9) ||
		!near(m.SelHeading, 66.796875, 1e-9) || !m.Has(FSelAlt|FBaroSetting|FSelHeading|FNavModes|FNACp|FSIL|FNICBaro) ||
		m.NavModes != NavAutopilot|NavVNAV|NavLNAV|NavTCAS {
		t.Errorf("TC29: alt %d baro %v hdg %v modes %b", m.SelAlt, m.BaroSetting, m.SelHeading, m.NavModes)
	}
	if op, ok := m.TCASOperational(); !ok || !op {
		t.Errorf("TC29 TCAS operational %v %v", op, ok)
	}
	// Independent bit slicing of the integrity fields (ME bits 40-43, 44, 45-46).
	me := mbOf(t, tssHex)
	if m.NACp != uint8(me>>13&0xF) || m.NICBaro != (me>>12&1 == 1) || m.SIL != uint8(me>>10&3) || m.NACp != 9 || m.SIL != 3 {
		t.Errorf("TC29 NACp %d NICbaro %v SIL %d", m.NACp, m.NICBaro, m.SIL)
	}
	// Selected heading with the top bit set: 351 * 180/256.
	m = mustDecode(t, esHex(0x8D, 0xA05A29, setBits(tss, 31, 39, 351)))
	if !m.Has(FSelHeading) || !near(m.SelHeading, 246.796875, 1e-9) {
		t.Errorf("TC29 heading %v", m.SelHeading)
	}

	// TC31 (DO-260B 2.2.3.2.7.2 layout):
	// version bits 41-43, NIC-A 44, NACp 45-48, GVA 49-50, SIL 51-52,
	// NICbaro 53, HRD 54, SIL supplement 55.
	var p uint64 = 31<<51 | 0<<48 | 2<<13 | 1<<12 | 10<<8 | 2<<6 | 3<<4 | 1<<3 | 1<<2 | 1<<1
	var d Message
	if err := Decode(esFrame(0x8D, 0x400000, p), &d); err != nil {
		t.Fatal(err)
	}
	if d.TC != 31 || d.Subtype != 0 || d.Version != 2 || !d.NICSuppA || d.NACp != 10 || d.GVA != 2 || d.SIL != 3 || !d.NICBaro ||
		!d.HRDMagnetic || !d.SILPerSample || !d.Has(FVersion|FNICSuppA|FNACp|FGVA|FSIL|FNICBaro|FHRD|FSILType|FOpStatus) ||
		d.Has(FTrackIsHeading) || d.Has(FLengthWidth) || d.AirGround == AGGround {
		t.Errorf("TC31 airborne v2: %+v", d)
	}
	// Surface, version 2: no NICbaro / GVA, but TAH, NACv, NIC-C and L/W.
	p = 31<<51 | 1<<48 | 2<<13 | 1<<3
	p = setBits(p, 17, 19, 3) // NACv
	p = setBits(p, 20, 20, 1) // NIC supplement C
	p = setBits(p, 21, 24, 9) // length/width
	p = setBits(p, 31, 32, 2) // SDA
	if err := Decode(esFrame(0x8C, 0x400000, p), &d); err != nil {
		t.Fatal(err)
	}
	if d.Subtype != 1 || d.Version != 2 || d.Has(FNICBaro) || d.Has(FGVA) || !d.Has(FTrackIsHeading) || !d.TrackIsHeading ||
		d.NACv != 3 || !d.NICSuppC || d.LengthWidth != 9 || d.SDA != 2 || !d.Has(FNACv|FNICSuppC|FLengthWidth|FSDA) || d.AirGround != AGGround {
		t.Errorf("TC31 surface v2: %+v", d)
	}
	// Version 1: no SIL supplement, GVA, SDA.
	p = 31<<51 | 0<<48 | 1<<13 | 1<<12 | 8<<8 | 2<<4 | 1<<3
	if err := Decode(esFrame(0x8D, 0x400000, p), &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != 1 || !d.NICSuppA || d.NACp != 8 || d.SIL != 2 || !d.NICBaro || d.Has(FSILType) || d.Has(FGVA) || d.Has(FSDA) || d.Has(FNICSuppC) {
		t.Errorf("TC31 v1: %+v", d)
	}
	// Version 0: version only.
	p = 31<<51 | 0xFFFF<<32 | 0<<13 | 0x1FFF
	if err := Decode(esFrame(0x8D, 0x400000, p), &d); err != nil {
		t.Fatal(err)
	}
	if !d.Has(FVersion) || d.Version != 0 || d.Valid&(FNICSuppA|FNACp|FSIL|FNICBaro|FHRD|FOpStatus|FGVA) != 0 {
		t.Errorf("TC31 v0: %v", d.Valid)
	}
	// Reserved subtypes carry nothing.
	p = 31<<51 | 2<<48 | 2<<13 | 0x1FFF
	if err := Decode(esFrame(0x8D, 0x400000, p), &d); err != nil {
		t.Fatal(err)
	}
	if d.Valid&^(FCapability|FTypeCode) != 0 {
		t.Errorf("TC31 subtype 2: %v", d.Valid)
	}
}

// ---------------------------------------------------------------- CPR

// nlTable is the NL transition latitude table published with the CPR
// definition (DO-260B Appendix A, ICAO 1090-WP-9-14), rounded to 8 decimals.
var nlTable = [...]float64{
	10.47047130, 14.82817437, 18.18626357, 21.02939493, 23.54504487, 25.82924707, 27.93898710, 29.91135686,
	31.77209708, 33.53993436, 35.22899598, 36.85025108, 38.41241892, 39.92256684, 41.38651832, 42.80914012,
	44.19454951, 45.54626723, 46.86733252, 48.16039128, 49.42776439, 50.67150166, 51.89342469, 53.09516153,
	54.27817472, 55.44378444, 56.59318756, 57.72747354, 58.84763776, 59.95459277, 61.04917774, 62.13216659,
	63.20427479, 64.26616523, 65.31845310, 66.36171008, 67.39646774, 68.42322022, 69.44242631, 70.45451075,
	71.45986473, 72.45884545, 73.45177442, 74.43893416, 75.42056257, 76.39684391, 77.36789461, 78.33374083,
	79.29428225, 80.24923213, 81.19801349, 82.13956981, 83.07199445, 83.99173563, 84.89166191, 85.75541621,
	86.53536998, 87.00000000,
}

func nlRef(lat float64) int {
	lat = math.Abs(lat)
	for i, lim := range nlTable {
		if lat < lim {
			return 59 - i
		}
	}
	return 1
}

func TestIndepNL(t *testing.T) {
	const eps = 2e-8 // the table is rounded to 8 decimals
	for i, lim := range nlTable[:len(nlTable)-1] {
		for _, sign := range []float64{1, -1} {
			if got := NL(sign * (lim - eps)); got != 59-i {
				t.Errorf("NL(%v) = %d, want %d", sign*(lim-eps), got, 59-i)
			}
			if got := NL(sign * (lim + eps)); got != 58-i {
				t.Errorf("NL(%v) = %d, want %d", sign*(lim+eps), got, 58-i)
			}
		}
	}
	for lat := -90.0; lat <= 90; lat += 0.0137 {
		skip := false
		for _, lim := range nlTable {
			if math.Abs(math.Abs(lat)-lim) < eps {
				skip = true
			}
		}
		if !skip && NL(lat) != nlRef(lat) {
			t.Fatalf("NL(%v) = %d, want %d", lat, NL(lat), nlRef(lat))
		}
	}
	// Spot values of the closed formula.
	for lat, want := range map[float64]int{0: 59, 0.1: 59, -0.1: 59, 45: 42, 80: 10, 87: 2, -87: 2, 87.5: 1, -87.5: 1, 90: 1, -90: 1} {
		if NL(lat) != want {
			t.Errorf("NL(%v) = %d, want %d", lat, NL(lat), want)
		}
	}
	if NL(math.NaN()) != 1 || NL(math.Inf(1)) != 1 || NL(math.Inf(-1)) != 1 {
		t.Errorf("NL of NaN/Inf")
	}
}

func TestIndepCPRAirborne(t *testing.T) {
	// Self-generated pairs: two positions a few seconds apart, encoded with
	// the DO-260B equations of cprEncodeRef (not CPREncode). Each decoded
	// position must be the encoded one to within the quantisation.
	const tolLat, tolLon = 3e-5, 8e-5
	for _, c := range []struct{ lat0, lon0, lat1, lon1 float64 }{
		{51.6866, 0.7002, 51.6868, 0.7013},       // south-east England
		{48.35370, 11.78610, 48.35512, 11.79250}, // Munich
		{-33.94610, 151.17720, -33.95, 151.18},   // Sydney: south, east of 90 E
		{40.6413, -73.7781, 40.6401, -73.7699},   // New York: west
		{-22.8100, -43.2506, -22.8091, -43.2444}, // Rio: south-west
	} {
		elat, elon := cprEncodeRef(c.lat0, c.lon0, false, false)
		olat, olon := cprEncodeRef(c.lat1, c.lon1, true, false)
		lat, lon, ok := CPRAirborneGlobal(elat, elon, olat, olon, false)
		if !ok || !near(lat, c.lat0, tolLat) || !near(lon, c.lon0, tolLon) {
			t.Errorf("global even %v: %v %v %v", c, lat, lon, ok)
		}
		lat, lon, ok = CPRAirborneGlobal(elat, elon, olat, olon, true)
		if !ok || !near(lat, c.lat1, tolLat) || !near(lon, c.lon1, tolLon) {
			t.Errorf("global odd %v: %v %v %v", c, lat, lon, ok)
		}
		// Local decoding from references up to about 2.7 degrees away.
		for _, d := range [][2]float64{{0, 0}, {-2.7, 0}, {2.7, 0}, {0, 2.7}, {0, -2.7}} {
			lat, lon, ok := CPRAirborneLocal(c.lat0+d[0], c.lon0+d[1], elat, elon, false)
			if !ok || !near(lat, c.lat0, tolLat) || !near(lon, c.lon0, tolLon) {
				t.Errorf("local even %v ref offset %v: %v %v %v", c, d, lat, lon, ok)
			}
			lat, lon, ok = CPRAirborneLocal(c.lat1+d[0], c.lon1+d[1], olat, olon, true)
			if !ok || !near(lat, c.lat1, tolLat) || !near(lon, c.lon1, tolLon) {
				t.Errorf("local odd %v ref offset %v: %v %v %v", c, d, lat, lon, ok)
			}
		}
	}

	// 1090 MHz Riddle worked example (exact intermediate values).
	_, elat, elon := cprOf(t, bookPosEven)
	_, olat, olon := cprOf(t, bookPosOdd)
	if elat != 93000 || elon != 51372 || olat != 74158 || olon != 50194 {
		t.Fatalf("CPR fields %d %d %d %d", elat, elon, olat, olon)
	}
	if lat, lon, ok := CPRAirborneGlobal(elat, elon, olat, olon, false); !ok || !near(lat, 52.25720214843750, 1e-12) || !near(lon, 3.91937255859375, 1e-12) {
		t.Errorf("riddle global: %v %v %v", lat, lon, ok)
	}
	if lat, _, ok := CPRAirborneGlobal(elat, elon, olat, olon, true); !ok || !near(lat, 52.26578017412606, 1e-12) {
		t.Errorf("riddle global odd: %v %v", lat, ok)
	}
	if lat, lon, ok := CPRAirborneLocal(52.258, 3.918, elat, elon, false); !ok || !near(lat, 52.25720214843750, 1e-12) || !near(lon, 3.91937255859375, 1e-12) {
		t.Errorf("riddle local: %v %v %v", lat, lon, ok)
	}
	// Reference a hair off a zone boundary: at 30.5085 N the odd zones are
	// 360/(NL-1) = 7.2 degrees wide, so 36 E (+3e-15) sits on the edge
	// between zones 4 and 5. A target at 30.5054 N 33.4479 E must still come
	// out in zone 4.
	clat, clon := cprEncodeRef(30.50540, 33.44787, true, false)
	if lat, lon, ok := CPRAirborneLocal(30.508474576271183, 7.2*5.0+3e-15, clat, clon, true); !ok || !near(lat, 30.50540, tolLat) || !near(lon, 33.44787, tolLon) {
		t.Errorf("zone boundary reference: %v %v %v", lat, lon, ok)
	}
	// Pairs that must be rejected: even and odd frames from latitudes in
	// different NL zones (a stale frame paired with a fresh one), and a
	// latitude index that puts the two solutions in different zones.
	if lat, lon, ok := CPRAirborneGlobal(8192, 0, 114688, 0, false); ok {
		t.Errorf("NL mismatch accepted: %v %v", lat, lon)
	}
	el, eo := cprEncodeRef(10.2, 20.0, false, false)
	ol, oo := cprEncodeRef(55.0, 20.0, true, false)
	for _, latestOdd := range []bool{false, true} {
		if lat, lon, ok := CPRAirborneGlobal(el, eo, ol, oo, latestOdd); ok {
			t.Errorf("inconsistent pair accepted: %v %v", lat, lon)
		}
	}
}

// cprEncodeRef encodes a position with the DO-260B equations, written out
// independently of CPREncode.
func cprEncodeRef(lat, lon float64, odd, surface bool) (uint32, uint32) {
	i := 0.0
	if odd {
		i = 1
	}
	nb := 17.0
	if surface {
		nb = 19
	}
	mod := func(a, b float64) float64 { return a - b*math.Floor(a/b) }
	// The surface format uses a quarter of the zone size; with 19 bits and
	// the airborne zone size the transmitted low 17 bits are the same.
	dlat := 360 / (60 - i)
	yz := math.Floor(math.Exp2(nb)*mod(lat, dlat)/dlat + 0.5)
	rlat := dlat * (yz/math.Exp2(nb) + math.Floor(lat/dlat))
	nl := float64(nlRef(rlat)) - i
	if nl < 1 {
		nl = 1
	}
	dlon := 360 / nl
	xz := math.Floor(math.Exp2(nb)*mod(lon, dlon)/dlon + 0.5)
	return uint32(mod(yz, 131072)), uint32(mod(xz, 131072))
}

func TestIndepCPRSurface(t *testing.T) {
	// Self-generated surface pair (an apron at 47.4581 N 8.5555 E, the odd
	// frame a few metres away), encoded with cprEncodeRef. The reference is
	// moved around the globe to check the quadrant and hemisphere choice: the
	// four longitude solutions are lon + k*90 and the one nearest the
	// reference wins; south of the equator the solution is lat - 90.
	const lat0, lon0, lat1, lon1 = 47.4581, 8.5555, 47.4580, 8.5551
	const tolLat, tolLon = 1.5e-5, 3e-5
	elat, elon := cprEncodeRef(lat0, lon0, false, true)
	olat, olon := cprEncodeRef(lat1, lon1, true, true)
	nearestQuadrant := func(lon, ref float64) float64 {
		v := lon + 90*math.Round((ref-lon)/90)
		for v >= 180 {
			v -= 360
		}
		for v < -180 {
			v += 360
		}
		return v
	}
	for _, ref := range [][2]float64{
		{52, -180}, {52, -140}, {52, -130}, {52, -50}, {52, -40}, {52, -10}, {52, 0}, {52, 10}, {52, 40},
		{52, 60}, {52, 130}, {52, 140}, {52, 179}, {90, 0}, {8, 0}, {44.9, 8},
	} {
		for _, latestOdd := range []bool{false, true} {
			wlat, wlon := lat0, lon0
			if latestOdd {
				wlat, wlon = lat1, lon1
			}
			wlon = nearestQuadrant(wlon, ref[1])
			lat, lon, ok := CPRSurfaceGlobal(ref[0], ref[1], elat, elon, olat, olon, latestOdd)
			if !ok || !near(lat, wlat, tolLat) || lonDiff(lon, wlon) > tolLon {
				t.Errorf("surface latestOdd=%v ref %v: %v %v %v, want %v %v", latestOdd, ref, lat, lon, ok, wlat, wlon)
			}
		}
	}
	// References in the southern hemisphere pick the solution 90 degrees
	// further south; its longitude belongs to a different NL zone count, so
	// only its latitude and quadrant are checked.
	for _, ref := range [][2]float64{{2, 0}, {-52, 0}, {-90, 0}, {-30, 20}} {
		lat, lon, ok := CPRSurfaceGlobal(ref[0], ref[1], elat, elon, olat, olon, false)
		if !ok || !near(lat, lat0-90, tolLat) || lonDiff(lon, ref[1]) > 45.0001 {
			t.Errorf("southern solution ref %v: %v %v %v", ref, lat, lon, ok)
		}
	}
	// Poles and equator all encode to zero.
	for _, c := range []struct{ refLat, lat float64 }{{-46, -90}, {-44, 0}, {44, 0}, {46, 90}} {
		for _, latestOdd := range []bool{false, true} {
			lat, lon, ok := CPRSurfaceGlobal(c.refLat, -180, 0, 0, 0, 0, latestOdd)
			if !ok || lat != c.lat || lon != -180 {
				t.Errorf("zero CPR, ref lat %v: %v %v %v, want %v -180", c.refLat, lat, lon, ok, c.lat)
			}
		}
	}
	// Local surface decoding from references up to 0.7 degrees away.
	for _, d := range [][2]float64{{0, 0}, {-0.7, 0}, {0.7, 0}, {0, 0.7}, {0, -0.7}} {
		if lat, lon, ok := CPRSurfaceLocal(lat0+d[0], lon0+d[1], elat, elon, false); !ok || !near(lat, lat0, tolLat) || !near(lon, lon0, tolLon) {
			t.Errorf("surface local even ref offset %v: %v %v %v", d, lat, lon, ok)
		}
		if lat, lon, ok := CPRSurfaceLocal(lat1+d[0], lon1+d[1], olat, olon, true); !ok || !near(lat, lat1, tolLat) || !near(lon, lon1, tolLon) {
			t.Errorf("surface local odd ref offset %v: %v %v %v", d, lat, lon, ok)
		}
	}

	// Self-generated DF18 surface frames through the decoder, then the pair
	// seen from the reference and from shifted references.
	me := mustDecode(t, esHex(0x90, 0x3A2B1C, surfME(7, 30, true, 20, false, 43.62646, 1.37476)))
	mo := mustDecode(t, esHex(0x90, 0x3A2B1C, surfME(7, 30, true, 20, true, 43.62650, 1.37480)))
	if me.DF != 18 || me.CPROdd || !mo.CPROdd || !me.CPRSurface || !mo.CPRSurface || me.ICAO != 0x3A2B1C {
		t.Fatalf("surface frames: %+v", me)
	}
	chk := func(name string, lat, lon float64, ok bool, wlat, wlon float64) {
		t.Helper()
		if !ok || !near(lat, wlat, tolLat) || lonDiff(lon, wlon) > tolLon {
			t.Errorf("%s: %v %v %v, want %v %v", name, lat, lon, ok, wlat, wlon)
		}
	}
	lat, lon, ok := CPRSurfaceLocal(43.63, 1.37, mo.CPRLat, mo.CPRLon, true)
	chk("local", lat, lon, ok, 43.62650, 1.37480)
	lat, lon, ok = CPRSurfaceGlobal(43.63, 1.37, me.CPRLat, me.CPRLon, mo.CPRLat, mo.CPRLon, true)
	chk("pair odd", lat, lon, ok, 43.62650, 1.37480)
	lat, lon, ok = CPRSurfaceGlobal(43.63, 1.37, me.CPRLat, me.CPRLon, mo.CPRLat, mo.CPRLon, false)
	chk("pair even", lat, lon, ok, 43.62646, 1.37476)
	lat, lon, ok = CPRSurfaceGlobal(43.63, -178.63, me.CPRLat, me.CPRLon, mo.CPRLat, mo.CPRLon, true)
	chk("+180", lat, lon, ok, 43.62650, 1.37480-180)
	lat, lon, ok = CPRSurfaceGlobal(43.63, 91.37, me.CPRLat, me.CPRLon, mo.CPRLat, mo.CPRLon, true)
	chk("+90", lat, lon, ok, 43.62650, 91.37480)

	if lat, lon, ok := CPRSurfaceGlobal(1.0, 179.0, 0, 0, 0, 0, false); !ok || lat != 0 || lon != -180 {
		t.Errorf("dateline: %v %v %v", lat, lon, ok)
	}
	if lat, lon, ok := CPRSurfaceGlobal(0, 0, 0, 0, 0, 0, false); !ok || lat != 0 || lon != 0 {
		t.Errorf("equator: %v %v %v", lat, lon, ok)
	}
	// A target just south of the equator seen from just north of it.
	evenLat := uint32(math.Round((89.9/(90.0/60) - 59) * 131072))
	oddLat := uint32(math.Round((89.9/(90.0/59) - 58) * 131072))
	if lat, _, ok := CPRSurfaceGlobal(0.1, 0, evenLat, 0, oddLat, 0, false); !ok || !near(lat, -0.1, 1e-4) {
		t.Errorf("cross-equator: %v %v", lat, ok)
	}
	if lat, lon, ok := CPRSurfaceGlobal(50, 0, 28672, 0, 122880, 0, false); ok {
		t.Errorf("surface NL mismatch accepted: %v %v", lat, lon)
	}
	// Southern and western hemisphere, and southern hemisphere east of 90 E
	// near the date line: self-generated pairs, global and local.
	for _, c := range []struct{ lat, lon float64 }{{-23.4265, -46.4816}, {-43.48564, 172.53942}, {-33.9461, 151.1772}, {64.13, -21.94}} {
		a, b := cprEncodeRef(c.lat, c.lon, false, true)
		e, f := cprEncodeRef(c.lat, c.lon, true, true)
		if lat, lon, ok := CPRSurfaceGlobal(c.lat+0.01, c.lon-0.02, a, b, e, f, true); !ok || !near(lat, c.lat, tolLat) || lonDiff(lon, c.lon) > tolLon {
			t.Errorf("surface global %v: %v %v %v", c, lat, lon, ok)
		}
		if lat, lon, ok := CPRSurfaceLocal(c.lat+0.3, c.lon-0.3, e, f, true); !ok || !near(lat, c.lat, tolLat) || lonDiff(lon, c.lon) > tolLon {
			t.Errorf("surface local %v: %v %v %v", c, lat, lon, ok)
		}
	}
	// 1090 MHz Riddle surface chapter (Schiphol), exact values.
	_, a, b := cprOf(t, bookSurfEven)
	_, c, d := cprOf(t, bookSurfOdd)
	if a != 115609 || c != 39199 || d != 110269 {
		t.Fatalf("CPR fields %d %d %d", a, c, d)
	}
	if lat, lon, ok := CPRSurfaceGlobal(51.990, 4.375, a, b, c, d, true); !ok || !near(lat, 52.320607072215964, 1e-12) || !near(lon, 4.734734671456474, 1e-12) {
		t.Errorf("riddle surface: %v %v %v", lat, lon, ok)
	}
	_, c, d = cprOf(t, bookSurf)
	if lat, lon, ok := CPRSurfaceLocal(51.990, 4.375, c, d, true); !ok || !near(lat, 52.32056051997815, 1e-12) || !near(lon, 4.735735212053571, 1e-12) {
		t.Errorf("riddle surface local: %v %v %v", lat, lon, ok)
	}
}

// Unusable inputs must give ok=false, never NaN or out-of-range positions.
func TestIndepCPRBadInput(t *testing.T) {
	bad := []float64{math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, x := range bad {
		for _, y := range []float64{0, 50, x} {
			for _, ref := range [][2]float64{{x, y}, {y, x}} {
				if lat, lon, ok := CPRSurfaceGlobal(ref[0], ref[1], 100000, 9000, 30000, 8000, true); ok {
					t.Errorf("CPRSurfaceGlobal(ref %v) = %v %v ok", ref, lat, lon)
				}
				if lat, lon, ok := CPRSurfaceLocal(ref[0], ref[1], 100000, 9000, false); ok {
					t.Errorf("CPRSurfaceLocal(ref %v) = %v %v ok", ref, lat, lon)
				}
				if lat, lon, ok := CPRAirborneLocal(ref[0], ref[1], 80000, 9000, false); ok {
					t.Errorf("CPRAirborneLocal(ref %v) = %v %v ok", ref, lat, lon)
				}
			}
		}
	}
	for _, ref := range [][2]float64{{91, 0}, {-90.5, 0}, {1e300, 0}} {
		if _, _, ok := CPRSurfaceGlobal(ref[0], ref[1], 100000, 9000, 30000, 8000, true); ok {
			t.Errorf("CPRSurfaceGlobal accepted reference latitude %v", ref[0])
		}
		if _, _, ok := CPRAirborneLocal(ref[0], ref[1], 80000, 9000, false); ok {
			t.Errorf("CPRAirborneLocal accepted reference latitude %v", ref[0])
		}
	}
	// Values that are not 17-bit CPR numbers.
	if _, _, ok := CPRAirborneGlobal(1<<17, 0, 0, 0, false); ok {
		t.Errorf("18-bit CPR value accepted")
	}
	if _, _, ok := CPRAirborneLocal(50, 5, 0, 1<<20, false); ok {
		t.Errorf("21-bit CPR value accepted")
	}
	if _, _, ok := CPRSurfaceGlobal(50, 5, 0, 0, 0xFFFFFFFF, 0, false); ok {
		t.Errorf("32-bit CPR value accepted")
	}

	// Whatever the input, an accepted result is a real position.
	rng := rand.New(rand.NewSource(99))
	check := func(name string, lat, lon float64, ok bool) {
		if ok && !(lat >= -90 && lat <= 90 && lon >= -180 && lon < 180) {
			t.Fatalf("%s returned %v %v", name, lat, lon)
		}
	}
	for i := 0; i < 200000; i++ {
		a, b, c, d := rng.Uint32()&0x1FFFF, rng.Uint32()&0x1FFFF, rng.Uint32()&0x1FFFF, rng.Uint32()&0x1FFFF
		rlat, rlon := rng.Float64()*180-90, rng.Float64()*720-360
		odd := i&1 == 1
		lat, lon, ok := CPRAirborneGlobal(a, b, c, d, odd)
		check("CPRAirborneGlobal", lat, lon, ok)
		lat, lon, ok = CPRSurfaceGlobal(rlat, rlon, a, b, c, d, odd)
		check("CPRSurfaceGlobal", lat, lon, ok)
		if ok && lonDiff(lon, rlon) > 45.0001 {
			t.Fatalf("CPRSurfaceGlobal(ref %v %v) = %v %v: not the nearest solution", rlat, rlon, lat, lon)
		}
		lat, lon, ok = CPRAirborneLocal(rlat, rlon, a, b, odd)
		check("CPRAirborneLocal", lat, lon, ok)
		lat, lon, ok = CPRSurfaceLocal(rlat, rlon, a, b, odd)
		check("CPRSurfaceLocal", lat, lon, ok)
	}
}

// Global and local decoding of positions all over the world against an
// encoder written from the DO-260B equations in this test (not CPREncode).
func TestIndepCPRWorld(t *testing.T) {
	encode := cprEncodeRef
	rng := rand.New(rand.NewSource(5))
	var rejected int
	const n = 60000
	for i := 0; i < n; i++ {
		lat := rng.Float64()*178 - 89
		lon := rng.Float64()*360 - 180
		switch i % 8 {
		case 0:
			lat = rng.Float64()*0.2 - 0.1 // equator
		case 1:
			lon = rng.Float64()*0.2 - 0.1 // Greenwich
		case 2:
			lon = 180 - rng.Float64()*0.1 // date line, east side
		case 3:
			lon = -180 + rng.Float64()*0.1
		}
		for _, surface := range []bool{false, true} {
			tol := 6e-5 // half an LSB of 360/2^17 degrees, generously
			if surface {
				tol = 1.5e-5
			}
			elat, elon := encode(lat, lon, false, surface)
			olat, olon := encode(lat, lon, true, surface)
			if x, y := CPREncode(lat, lon, false, surface); x != elat || y != elon {
				t.Fatalf("CPREncode(%v, %v, even, %v) = %d %d, want %d %d", lat, lon, surface, x, y, elat, elon)
			}
			if x, y := CPREncode(lat, lon, true, surface); x != olat || y != olon {
				t.Fatalf("CPREncode(%v, %v, odd, %v) = %d %d, want %d %d", lat, lon, surface, x, y, olat, olon)
			}
			refLat := lat + rng.Float64()*0.6 - 0.3
			refLon := lon + rng.Float64()*0.2 - 0.1
			if refLat > 90 {
				refLat = 90
			}
			for _, latestOdd := range []bool{false, true} {
				var glat, glon float64
				var ok bool
				if surface {
					glat, glon, ok = CPRSurfaceGlobal(refLat, refLon, elat, elon, olat, olon, latestOdd)
				} else {
					glat, glon, ok = CPRAirborneGlobal(elat, elon, olat, olon, latestOdd)
				}
				if !ok {
					rejected++ // even and odd rounding fell on different sides of an NL boundary
					continue
				}
				// Compare longitudes on the circle, scaled to the latitude.
				if !near(glat, lat, tol) || lonDiff(glon, lon)*math.Cos(lat*math.Pi/180) > tol*8 {
					t.Fatalf("global surface=%v latestOdd=%v: %v %v -> %v %v", surface, latestOdd, lat, lon, glat, glon)
				}
				cl, co := elat, elon
				if latestOdd {
					cl, co = olat, olon
				}
				var llat, llon float64
				if surface {
					llat, llon, ok = CPRSurfaceLocal(refLat, refLon, cl, co, latestOdd)
				} else {
					llat, llon, ok = CPRAirborneLocal(refLat, refLon, cl, co, latestOdd)
				}
				if !ok || !near(llat, glat, 1e-9) || lonDiff(llon, glon) > 1e-9 {
					t.Fatalf("local surface=%v latestOdd=%v at %v %v (ref %v %v): %v %v %v, global %v %v",
						surface, latestOdd, lat, lon, refLat, refLon, llat, llon, ok, glat, glon)
				}
			}
		}
	}
	if rejected > n/50 {
		t.Errorf("%d of %d consistent pairs rejected", rejected, 4*n)
	}
}

// ---------------------------------------------------------------- Comm-B

func TestIndepCommBDecoders(t *testing.T) {
	// BDS 1,0, self-generated. Hand placement of ownBDS10a: byte 1 = 10,
	// bit 16 (ACAS) = 01, subnetwork version 3 in bits 17-23 = 06, bit 25 =
	// 80, bits 33-36 + 38 + ACAS version 1 in 39-40 = F5.
	if mb := bds10MB(ownBDS10a); mb != 0x10010680F50000 {
		t.Fatalf("bds10MB = %014X", mb)
	}
	c := DecodeBDS10(mbOf(t, fhex(commBFrame(21, EncodeID13(0x6000), 0x4D2B3C, bds10MB(ownBDS10a)))))
	if c != ownBDS10a {
		t.Errorf("BDS 1,0: %+v", c)
	}
	// BDS 1,7 (1090 MHz Riddle, ELS chapter).
	g := DecodeBDS17(mbOf(t, bookBDS17))
	var regs []string
	for _, b := range g.AppendRegisters(nil) {
		regs = append(regs, b.String())
	}
	if s := strings.Join(regs, " "); s != "0,5 0,6 0,7 0,8 0,9 2,0 4,0 5,0 5,1 5,2 6,0" {
		t.Errorf("BDS 1,7 registers: %s", s)
	}
	if !g.Supports(BDS20) || !g.Supports(BDS60) || g.Supports(BDS44) || g.Supports(BDS45) {
		t.Errorf("BDS 1,7 Supports")
	}
	// BDS 2,0.
	for s, want := range map[string]string{
		bookBDS20: "KLM1017", ownBDS20: "SWR93KE", ownBDS20b: "AFR1745", ownBDS20c: "EIN6B",
	} {
		cs, ok := DecodeBDS20(mbOf(t, s))
		if !ok || TrimCallsign(cs) != want {
			t.Errorf("BDS 2,0 %s: %q %v", s, cs, ok)
		}
		if m := mustDecode(t, s); m.CallsignString() != want || m.BDS[0].BDS != BDS20 {
			t.Errorf("BDS 2,0 %s not surfaced: %q", s, m.CallsignString())
		}
	}
	if _, ok := DecodeBDS20(0x20 << 48); ok {
		t.Errorf("BDS 2,0 with NUL characters accepted")
	}

	// BDS 3,0, payloads placed by hand (Doc 9871 Table A-2-48).
	const ra = uint64(0x30800000000000)
	r := DecodeACASRA(ra)
	if r.ARA != 0x2000 || !r.Active() || r.Corrective() || r.Downward() || r.RAC != 0 || r.Terminated || r.MultipleThreat || r.ThreatType != 0 {
		t.Errorf("BDS 3,0 minimal: %+v", r)
	}
	r = DecodeACASRA(ra | 1<<(55-29) | 0xABCDEF<<2)
	if r.ThreatType != 1 || r.ThreatICAO != 0xABCDEF {
		t.Errorf("BDS 3,0 TTI 1: %+v", r)
	}
	r = DecodeACASRA(ra | 2<<(55-29) | 10<<(55-49) | 3)
	if r.ThreatType != 2 || !r.ThreatRangeOK || !near(r.ThreatRangeNM, 0.9, 1e-9) || !r.ThreatBrgOK || r.ThreatBearing != 15 || r.ThreatAltOK {
		t.Errorf("BDS 3,0 TTI 2: %+v", r)
	}
	r = DecodeACASRA(ra | 2<<(55-29) | 0x1010<<13)
	if !r.ThreatAltOK || r.ThreatAlt != 24600 || r.ThreatRangeOK || r.ThreatBrgOK {
		t.Errorf("BDS 3,0 TTI 2 altitude: %+v", r)
	}
	r = DecodeACASRA(0x30<<48 | 1<<(55-8) | 1<<(55-9) | 1<<(55-12) | 1<<(55-23) | 1<<(55-27))
	if !r.Active() || !r.Corrective() || r.Downward() || r.IncreasedRate() || !r.SenseReversal() || r.AltitudeCrossing() || r.Positive() ||
		r.RAC != 0x4 || r.Terminated || !r.MultipleThreat {
		t.Errorf("BDS 3,0 flags: %+v", r)
	}
	for mb, want := range map[uint64]bool{
		ra:                        true,
		ra | 3<<(55-29):           false, // threat type 3 is not assigned
		ra | 48<<(55-21):          false, // ACAS III reserved bits
		ra | 47<<(55-21):          true,
		0x20<<48 | ra&^(0xFF<<48): false,
	} {
		cands, n := InferBDS(mb, math.NaN())
		got := false
		for i := 0; i < n; i++ {
			got = got || cands[i].BDS == BDS30
		}
		if got != want {
			t.Errorf("BDS 3,0 inference of %014X = %v, want %v", mb, got, want)
		}
	}

	// BDS 4,0.
	v40 := DecodeBDS40(mbOf(t, ownBDS40b))
	if !v40.MCPAltOK || v40.MCPAlt != 4000 || !v40.FMSAltOK || v40.FMSAlt != 4000 || !v40.BaroOK || !near(v40.Baro, 1019.2, 1e-9) {
		t.Errorf("BDS 4,0: %+v", v40)
	}
	v40 = DecodeBDS40(1<<(55-47) | 1<<(55-48) | 1<<(55-50) | 1<<(55-53) | 3)
	if v40.MCPAltOK || v40.FMSAltOK || v40.BaroOK || !v40.ModesOK || !v40.VNAV || v40.AltHold || !v40.Approach || !v40.SourceOK || v40.Source != 3 {
		t.Errorf("BDS 4,0 modes: %+v", v40)
	}
	if v40.NavModes() != NavVNAV|NavApproach {
		t.Errorf("BDS 4,0 NavModes %b", v40.NavModes())
	}

	// BDS 4,4 (1090 MHz Riddle, meteorological chapter).
	v44 := DecodeBDS44(mbOf(t, bookBDS44))
	if v44.FOM != 1 || !v44.WindOK || v44.WindSpeed != 22 || !near(v44.WindDir, 344.53125, 1e-9) || !near(v44.Temp, -48.75, 1e-9) ||
		v44.PressureOK || v44.HumidityOK || v44.TurbulenceOK {
		t.Errorf("BDS 4,4: %+v", v44)
	}
	v44 = DecodeBDS44(1<<(55-3) | 1<<(55-4) | 50<<(55-13) | 256<<(55-22) | 1<<(55-34) | 1013<<(55-45) | 1<<(55-46) | 2<<(55-48) | 1<<(55-49) | 32)
	if v44.FOM != 1 || v44.WindSpeed != 50 || v44.WindDir != 180 || !v44.PressureOK || v44.Pressure != 1013 ||
		!v44.TurbulenceOK || v44.Turbulence != 2 || !v44.HumidityOK || v44.Humidity != 50 || v44.Temp != 0 {
		t.Errorf("BDS 4,4 multi: %+v", v44)
	}
	if v := DecodeBDS44(1<<(55-3) | 1<<(55-4) | 241<<(55-33)); v.Temp != 60.25 {
		t.Errorf("BDS 4,4 temperature %v, want 60.25", v.Temp)
	}
	if v := DecodeBDS44(1<<(55-3) | 1<<(55-4) | 1<<(55-23) | 703<<(55-33)); v.Temp != -80.25 {
		t.Errorf("BDS 4,4 temperature %v, want -80.25", v.Temp)
	}

	// BDS 4,5.
	// Self-generated: temperature only, -7.25 C = -29 * 0.25 in bits 17-26.
	p45 := setBits(setBits(0, 16, 16, 1), 17, 26, uint64(0x400-29))
	v45 := DecodeBDS45(p45)
	if !v45.TempOK || !near(v45.Temp, -7.25, 1e-9) || v45.TurbulenceOK || v45.WindShearOK || v45.MicroburstOK || v45.IcingOK ||
		v45.WakeVortexOK || v45.PressureOK || v45.RadioHeightOK {
		t.Errorf("BDS 4,5: %+v", v45)
	}
	v45 = DecodeBDS45(1<<55 | 2<<(55-2) | 1<<(55-3) | 1<<(55-5) | 1<<(55-6) | 3<<(55-8) | 1<<(55-9) | 2<<(55-11) |
		1<<(55-12) | 1<<(55-14) | 1<<(55-26) | 1013<<(55-37) | 1<<(55-38) | 500<<(55-50))
	if v45.Turbulence != 2 || v45.WindShear != 1 || v45.Microburst != 3 || v45.Icing != 2 || v45.WakeVortex != 1 ||
		!v45.PressureOK || v45.Pressure != 1013 || !v45.RadioHeightOK || v45.RadioHeight != 8000 || v45.TempOK {
		t.Errorf("BDS 4,5 multi: %+v", v45)
	}

	// BDS 5,0.
	v50 := DecodeBDS50(mbOf(t, ownBDS50))
	if !near(v50.Roll, -4.21875, 1e-9) || !near(v50.Track, 287.578125, 1e-9) || v50.GS != 446 || v50.TrackRate != -0.3125 || v50.TAS != 452 {
		t.Errorf("BDS 5,0: %+v", v50)
	}
	// Smallest negative roll step but one: -2 * 45/256.
	if v := DecodeBDS50(bds50MB(-0.3515625, 10, 300, 0, 310)); !near(v.Roll, -0.3515625, 1e-9) {
		t.Errorf("BDS 5,0 signed roll %v", v.Roll)
	}
	if v := DecodeBDS50(1<<(55-11) | 1<<(55-12)); !v.TrackOK || v.Track != 180 {
		t.Errorf("BDS 5,0 track %v", v.Track)
	}
	if v := DecodeBDS50(1<<(55-34) | 1<<(55-35)); !v.TrackRateOK || v.TrackRate != -16 {
		t.Errorf("BDS 5,0 track rate %v", v.TrackRate)
	}
	if v := DecodeBDS50(1<<55 | (1<<9|284)<<(55-10)); !near(v.Roll, -40.078125, 1e-9) {
		t.Errorf("BDS 5,0 roll %v", v.Roll)
	}
	if v := DecodeBDS50(1<<55 | 171<<(55-10)); !near(v.Roll, 30.05859375, 1e-9) {
		t.Errorf("BDS 5,0 roll %v", v.Roll)
	}

	// BDS 6,0.
	v60 := DecodeBDS60(mbOf(t, ownBDS60))
	if !near(v60.MagHeading, 118.4765625, 1e-9) || v60.IAS != 258 || !near(v60.Mach, 0.428, 1e-9) || v60.BaroRate != -1600 || v60.InertialRate != -1632 {
		t.Errorf("BDS 6,0: %+v", v60)
	}
	if v := DecodeBDS60(1<<55 | 1<<54); !v.HeadingOK || v.MagHeading != 180 {
		t.Errorf("BDS 6,0 heading %v", v.MagHeading)
	}
	if v := DecodeBDS60(1<<(55-34) | 1<<(55-35) | 500<<(55-44)); !v.BaroRateOK || v.BaroRate != -384 {
		t.Errorf("BDS 6,0 baro rate %v", v.BaroRate)
	}
	// Two's complement at the edges: 0x3FF = -1 * 32 ft/min.
	if v := DecodeBDS60(1<<(55-34) | 0x3FF<<(55-44) | 1<<(55-45) | 0x3FF); v.BaroRate != -32 || v.InertialRate != -32 {
		t.Errorf("BDS 6,0 rates %d %d, want -32", v.BaroRate, v.InertialRate)
	}
}

func hasBDS(c [MaxBDS]BDSCandidate, n int, b BDS) bool {
	for i := 0; i < n; i++ {
		if c[i].BDS == b {
			return true
		}
	}
	return false
}

func TestIndepBDSInference(t *testing.T) {
	// Book examples and self-generated replies of each register.
	for s, want := range map[string]BDS{
		ownBDS10:  BDS10,
		bookBDS17: BDS17,
		ownBDS20c: BDS20,
		bookBDS20: BDS20,
		ownBDS40:  BDS40,
		ownBDS40b: BDS40,
		bookBDS40: BDS40,
		ownBDS50:  BDS50,
		bookBDS50: BDS50,
		ownBDS60:  BDS60,
		bookBDS60: BDS60,
	} {
		m := mustDecode(t, s)
		b, conf, ok := m.BestBDS()
		if !ok || b != want {
			t.Errorf("%s: BestBDS %v (%.2f, %v), want %v; candidates %v", s, b, conf, ok, want, m.BDS[:m.NumBDS])
		}
	}
	// Extended squitters never get Comm-B candidates.
	for _, s := range []string{bookIdent, bookPosEven, bookVelGS} {
		if m := mustDecode(t, s); m.NumBDS != 0 || m.Has(FCommB) {
			t.Errorf("%s: Comm-B candidates on DF17", s)
		}
	}
	if _, n := InferBDS(0, math.NaN()); n != 0 {
		t.Errorf("all-zero payload has candidates")
	}

	// Validator rules (status bits, reserved bits, value ranges), applied to
	// variations of valid payloads.
	p10 := bds10MB(ownBDS10a)
	p17 := mbOf(t, bookBDS17)
	p20 := mbOf(t, bookBDS20)
	p40 := mbOf(t, ownBDS40b)
	p44 := mbOf(t, bookBDS44)
	p45 := setBits(setBits(0, 16, 16, 1), 17, 26, uint64(0x400-29)) // temperature only, -7.25 C
	p50 := mbOf(t, ownBDS50)
	p60 := mbOf(t, ownBDS60)
	nan := math.NaN()
	for _, c := range []struct {
		name string
		mb   uint64
		b    BDS
		want bool
	}{
		{"1,0 valid", p10, BDS10, true},
		{"1,0 reserved bit", p10 | 1<<(55-9), BDS10, false},
		{"1,0 wrong id", p20, BDS10, false},
		{"1,7 valid", p17, BDS17, true},
		{"1,7 without 2,0", p17 &^ (1 << (55 - 6)), BDS17, false},
		{"1,7 trailing bits", p17 | 1<<(55-28), BDS17, false},
		{"2,0 valid", p20, BDS20, true},
		{"2,0 wrong id", p10, BDS20, false},
		{"2,0 illegal characters", 0x20<<48 | 0x861861861861, BDS20, false}, // eight times code 33 '!'
		{"4,0 valid", p40, BDS40, true},
		{"4,0 reserved 40-47", p40 | 1<<(55-39), BDS40, false},
		{"4,0 reserved 52-53", p40 | 1<<(55-51), BDS40, false},
		{"4,0 MCP status clear", p40 &^ (1 << 55), BDS40, false},
		{"4,0 source without status", 3, BDS40, false},
		{"4,4 valid", p44, BDS44, true},
		{"4,4 FOM 5", p44&^(0xF<<52) | 5<<52, BDS44, false},
		{"4,4 wind 251 kt", 1<<(55-3) | 1<<(55-4) | 251<<(55-13), BDS44, false},
		{"4,4 +60.25 C", 1<<(55-3) | 1<<(55-4) | 50<<(55-13) | 241<<(55-33), BDS44, false},
		{"4,4 -80.25 C", 1<<(55-3) | 1<<(55-4) | 50<<(55-13) | 1<<(55-23) | 703<<(55-33), BDS44, false},
		{"4,4 all zero meteo", 1<<(55-3) | 1<<(55-4), BDS44, false},
		{"4,4 pressure without status", 1<<(55-3) | 1<<(55-4) | 50<<(55-13) | 100<<(55-33) | 1<<(55-45), BDS44, false},
		{"4,5 valid", p45, BDS45, true},
		{"4,5 reserved tail", p45 | 1, BDS45, false},
		{"4,5 +60.25 C", 1<<(55-15) | 241<<(55-25), BDS45, false},
		{"4,5 -80.25 C", 1<<(55-15) | 1<<(55-16) | 191<<(55-25), BDS45, false},
		{"4,5 turbulence without status", 1 << (55 - 2), BDS45, false},
		{"4,5 pressure without status", 1 << (55 - 37), BDS45, false},
		{"4,5 looks like 1,7", 0xFF81C300000000, BDS45, false},
		{"5,0 valid", p50, BDS50, true},
		{"5,0 signed roll", bds50MB(-0.3515625, 330, 410, -0.03125, 420), BDS50, true},
		{"5,0 roll sign without status", 1 << (55 - 1), BDS50, false},
		{"5,0 GS without status", 100 << (55 - 33), BDS50, false},
		{"5,0 GS 602 kt", 1<<(55-23) | 301<<(55-33), BDS50, false},
		{"5,0 TAS-GS 300 kt", 1<<(55-23) | 150<<(55-33) | 1<<(55-45) | 300, BDS50, false},
		{"6,0 valid", p60, BDS60, true},
		{"6,0 heading sign without status", 1 << (55 - 1), BDS60, false},
		{"6,0 IAS 501 kt", 1<<(55-12) | 501<<(55-22), BDS60, false},
		{"6,0 Mach 1.004", 1<<(55-23) | 251<<(55-33), BDS60, false},
		{"6,0 baro rate -16384", p60&^(0x3FF<<(55-44)) | 0x200<<(55-44), BDS60, false},
		{"6,0 inertial rate +16352", p60&^0x3FF | 0x1FF, BDS60, false},
	} {
		cands, n := InferBDS(c.mb, nan)
		if got := hasBDS(cands, n, c.b); got != c.want {
			t.Errorf("%s: BDS %v candidate = %v, want %v (%v)", c.name, c.b, got, c.want, cands[:n])
		}
	}

	// Payloads valid as 5,0 and as 6,0: the ADS-B ground velocity decides.
	for _, c := range []struct {
		mb       uint64
		gs, trk  float64
		altitude float64
	}{{ownAmbig1, 336, 240.5, 14000}, {ownAmbig2, 350, 221, 14000}} {
		m := mustDecode(t, fhex(commBFrame(21, EncodeID13(0x5402), 0x3C7A90, c.mb)))
		if b, conf, ok := DisambiguateBDS(&m, c.gs, c.trk, c.altitude); !ok || b != BDS50 {
			t.Errorf("%014X: DisambiguateBDS %v %.2f %v, want 5,0", c.mb, b, conf, ok)
		}
	}
	m := mustDecode(t, ownBDS20c)
	if b, _, ok := DisambiguateBDS(&m, 0, 0, 0); !ok || b != BDS20 {
		t.Errorf("BDS 2,0 lost in DisambiguateBDS: %v %v", b, ok)
	}
	// 1090 MHz Riddle inference chapter: msg1 is BDS 6,0 (IAS/Mach consistent
	// at its altitude).
	m = mustDecode(t, bookInference)
	if b, conf, ok := m.BestBDS(); !ok || b != BDS60 {
		t.Errorf("riddle msg1: %v %.2f %v", b, conf, ok)
	}

	// A self-generated 5,0 payload that is also a valid 6,0.
	// As 5,0: GS 220, track 216.2, TAS 228; as 6,0: heading 355.25, IAS 206, Mach 0.44.
	amb := ownAmbig3
	if v := DecodeBDS50(amb); v.GS != 220 || !near(v.Track, 216.2109375, 1e-9) || v.TAS != 228 {
		t.Errorf("ambiguous as 5,0: %+v", v)
	}
	if v := DecodeBDS60(amb); !near(v.MagHeading, 355.25390625, 1e-9) || v.IAS != 206 || !near(v.Mach, 0.44, 1e-9) {
		t.Errorf("ambiguous as 6,0: %+v", v)
	}
	f := commBFrame(21, EncodeID13(0x1000), 0x4243D0, amb)
	if err := Decode(f, &m); err != nil {
		t.Fatal(err)
	}
	// Without an altitude both readings survive. 5,0 leads: its five fields
	// agree with each other, while the 6,0 reading has a nearly level
	// barometric rate next to a 3648 ft/min inertial one.
	if !hasBDS(m.BDS, int(m.NumBDS), BDS50) || !hasBDS(m.BDS, int(m.NumBDS), BDS60) || m.BDS[0].BDS != BDS50 {
		t.Errorf("ambiguous payload: candidates %v", m.BDS[:m.NumBDS])
	}
	// In a DF20 at 3000 ft, Mach 0.44 with IAS 206 kt is impossible: 6,0 is out.
	if r := mustDecode(t, fhex(commBFrame(20, ac13For(3000), 0x4243D0, amb))); r.AltBaro != 3000 || r.NumBDS != 1 || r.BDS[0].BDS != BDS50 {
		t.Errorf("ambiguous payload at 3000 ft: %v", r.BDS[:r.NumBDS])
	}
	if b, conf, ok := DisambiguateBDS(&m, 220, 216, nan); !ok || b != BDS50 {
		t.Errorf("ambiguous payload with matching GS: %v %.2f %v", b, conf, ok)
	}
	// An aircraft doing 480 kt cannot be sending GS 220: the reply is 6,0 or nothing.
	if b, _, ok := DisambiguateBDS(&m, 480, 0, nan); ok && b == BDS50 {
		t.Errorf("ambiguous payload accepted as 5,0 against GS 480")
	}
}

// ---------------------------------------------------------------- integrity

func TestIndepNIC(t *testing.T) {
	const nm = 1852.0
	type row struct {
		tc      uint8
		a, b, c bool
		nic     int
		rc      float64
	}
	// 1090 MHz Riddle, uncertainty chapter: version 1 (NIC supplement only).
	for _, r := range []row{
		{0, false, false, false, 0, 0}, {5, false, false, false, 11, 7.5}, {6, false, false, false, 10, 25},
		{7, true, false, false, 9, 75}, {7, false, false, false, 8, 0.1 * nm}, {8, false, false, false, 0, 0},
		{9, false, false, false, 11, 7.5}, {10, false, false, false, 10, 25}, {11, true, false, false, 9, 75},
		{11, false, false, false, 8, 0.1 * nm}, {12, false, false, false, 7, 0.2 * nm}, {13, false, false, false, 6, 0.5 * nm},
		{13, true, false, false, 6, 0.6 * nm}, {14, false, false, false, 5, 1 * nm}, {15, false, false, false, 4, 2 * nm},
		{16, true, false, false, 3, 4 * nm}, {16, false, false, false, 2, 8 * nm}, {17, false, false, false, 1, 20 * nm},
		{18, false, false, false, 0, 0}, {20, false, false, false, 11, 7.5}, {21, false, false, false, 10, 25}, {22, false, false, false, 0, 0},
	} {
		nic, rc, ok := NICRc(r.tc, 1, r.a, true, true) // B and C do not exist in version 1
		if !ok || nic != r.nic || !near(rc, r.rc, 0.01) {
			t.Errorf("v1 TC %d A=%v: NIC %d Rc %v, want %d %v", r.tc, r.a, nic, rc, r.nic, r.rc)
		}
	}
	// Version 2 (NIC supplements A, B, C).
	for _, r := range []row{
		{5, false, false, false, 11, 7.5}, {6, false, false, false, 10, 25}, {7, true, false, false, 9, 75},
		{7, false, false, false, 8, 0.1 * nm}, {8, true, false, true, 7, 0.2 * nm}, {8, true, false, false, 6, 0.3 * nm},
		{8, false, false, true, 6, 0.6 * nm}, {8, false, false, false, 0, 0},
		{9, false, false, false, 11, 7.5}, {10, false, false, false, 10, 25}, {11, true, true, false, 9, 75},
		{11, false, false, false, 8, 0.1 * nm}, {12, false, false, false, 7, 0.2 * nm}, {13, false, true, false, 6, 0.3 * nm},
		{13, false, false, false, 6, 0.5 * nm}, {13, true, true, false, 6, 0.6 * nm}, {14, false, false, false, 5, 1 * nm},
		{15, false, false, false, 4, 2 * nm}, {16, true, true, false, 3, 4 * nm}, {16, false, false, false, 2, 8 * nm},
		{17, false, false, false, 1, 20 * nm}, {18, false, false, false, 0, 0},
		{20, true, true, true, 11, 7.5}, {21, true, true, true, 10, 25}, {22, true, true, true, 0, 0},
	} {
		nic, rc, ok := NICRc(r.tc, 2, r.a, r.b, r.c)
		if !ok || nic != r.nic || !near(rc, r.rc, 0.01) {
			t.Errorf("v2 TC %d A=%v B=%v C=%v: NIC %d Rc %v, want %d %v", r.tc, r.a, r.b, r.c, nic, rc, r.nic, r.rc)
		}
	}
	// Version 0: NUCp and its HPL.
	for tc, w := range map[uint8]struct {
		nuc int
		hpl float64
	}{
		0: {0, 0}, 5: {9, 7.5}, 6: {8, 25}, 7: {7, 0.1 * nm}, 8: {6, 0},
		9: {9, 7.5}, 10: {8, 25}, 11: {7, 0.1 * nm}, 12: {6, 0.2 * nm}, 13: {5, 0.5 * nm}, 14: {4, 1 * nm},
		15: {3, 2 * nm}, 16: {2, 10 * nm}, 17: {1, 20 * nm}, 18: {0, 0}, 20: {9, 7.5}, 21: {8, 25}, 22: {0, 0},
	} {
		nuc, ok := NUCp(tc)
		_, rc, ok2 := NICRc(tc, 0, true, true, true)
		if !ok || !ok2 || nuc != w.nuc || !near(rc, w.hpl, 0.01) {
			t.Errorf("v0 TC %d: NUCp %d Rc %v, want %d %v", tc, nuc, rc, w.nuc, w.hpl)
		}
	}
	for _, tc := range []uint8{1, 2, 3, 4, 19, 23, 28, 29, 31, 32, 255} {
		if _, _, ok := NICRc(tc, 2, false, false, false); ok {
			t.Errorf("NICRc(%d) ok", tc)
		}
		if _, ok := NUCp(tc); ok {
			t.Errorf("NUCp(%d) ok", tc)
		}
	}
	// NUCp of the book's TC11 position frame, and the NACp / NACv tables of the book.
	m := mustDecode(t, bookPosEven)
	if nuc, ok := NUCp(m.TC); !ok || nuc != 7 {
		t.Errorf("NUCp %d", nuc)
	}
	for nacp, epu := range map[uint8]float64{11: 3, 10: 10, 9: 30, 8: 0.05 * nm, 7: 0.1 * nm, 6: 0.3 * nm, 5: 0.5 * nm, 4: 1 * nm, 3: 2 * nm, 2: 4 * nm, 1: 10 * nm, 0: 0, 12: 0, 15: 0} {
		if got := NACpEPU(nacp); !near(got, epu, 0.01) {
			t.Errorf("NACpEPU(%d) = %v, want %v", nacp, got, epu)
		}
	}
	for nacv, v := range map[uint8]float64{0: 0, 1: 10, 2: 3, 3: 1, 4: 0.3, 5: 0, 7: 0} {
		if got := NACvHFOM(nacv); got != v {
			t.Errorf("NACvHFOM(%d) = %v", nacv, got)
		}
	}
}

// ---------------------------------------------------------------- robustness

// exercise calls every accessor of a decoded message; none may panic.
func exercise(m *Message) {
	_ = m.ME()
	_ = m.MB()
	_ = m.Long()
	_ = m.ICAOString()
	_ = m.CallsignString()
	_ = m.SquawkString()
	_ = m.CategoryString()
	_ = m.EmergencyString()
	_ = m.AddrKey()
	_ = m.AddressCorrected()
	_ = m.AddrType.String()
	_ = m.AddrType.Label()
	_ = m.AddrType.IsICAO()
	_ = m.Source.Label()
	_ = m.CRC.String()
	_ = DFName(m.DF)
	_ = DFIndexLabel(DFIndex(m.DF))
	_, _, _ = m.Interrogator()
	_, _, _ = m.NICRc(int(m.Version), m.NICSuppA, m.NICSuppC)
	_, _ = m.TCASOperational()
	_, _ = m.TCASRAActive()
	_, _ = m.IdentActive()
	_, _ = m.ACASRA()
	_, _, _ = m.BestBDS()
	var names [8]string
	_ = m.NavModes.AppendNames(names[:0])
	for _, v := range [][3]float64{{math.NaN(), math.NaN(), math.NaN()}, {0, 0, 0}, {450, 359.9, 41000}, {math.Inf(1), math.Inf(-1), math.Inf(1)}, {-5, 720, -2000}, {1e308, 1e308, 1e308}} {
		_, _, _ = DisambiguateBDS(m, v[0], v[1], v[2])
		c, n := RankBDS(m, v[0], v[1], v[2])
		for i := 0; i < n; i++ {
			if !(c[i].Confidence > 0 && c[i].Confidence <= 1) {
				panic("RankBDS confidence out of range")
			}
		}
	}
	mb := m.MB()
	_ = DecodeBDS10(mb)
	g := DecodeBDS17(mb)
	var regs [24]BDS
	_ = g.AppendRegisters(regs[:0])
	_, _ = DecodeBDS20(mb)
	_ = DecodeACASRA(mb)
	_ = DecodeBDS40(mb)
	_ = DecodeBDS44(mb)
	_ = DecodeBDS45(mb)
	_ = DecodeBDS50(mb)
	_ = DecodeBDS60(mb)
	if m.Has(FCPR) {
		_, _, _ = CPRAirborneGlobal(m.CPRLat, m.CPRLon, m.CPRLon, m.CPRLat, m.CPROdd)
		_, _, _ = CPRSurfaceGlobal(52, 4, m.CPRLat, m.CPRLon, m.CPRLon, m.CPRLat, m.CPROdd)
		_, _, _ = CPRAirborneLocal(52, 4, m.CPRLat, m.CPRLon, m.CPROdd)
		_, _, _ = CPRSurfaceLocal(-33, 151, m.CPRLat, m.CPRLon, m.CPROdd)
	}
}

// checkInvariants verifies what must hold for every decoded message.
func checkInvariants(t *testing.T, frame []byte, m *Message, err error) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("frame %X: "+format, append([]any{frame}, args...)...)
	}
	n := len(frame)
	if n != 7 && n != 14 {
		if err != ErrFrameLength {
			fail("length %d: err %v", n, err)
		}
		if n == 0 || m.Valid != 0 || m.NumBDS != 0 {
			return
		}
		return
	}
	if m.ICAO > 0xFFFFFF {
		fail("ICAO %X", m.ICAO)
	}
	if err != nil {
		if m.Valid != 0 {
			fail("fields valid (%v) despite error %v", m.Valid, err)
		}
		if err == ErrBadCRC && (m.CRC != CRCBad || int(m.Len) != n) {
			fail("ErrBadCRC without stats fields")
		}
		return
	}
	if int(m.Len) != n || m.DF != FrameDF(frame) || m.CRC == CRCUnknown || m.CRC == CRCBad {
		fail("Len %d DF %d CRC %v", m.Len, m.DF, m.CRC)
	}
	if m.Has(FAltBaro) && (m.AltBaro < -1200 || m.AltBaro > 126700) {
		fail("AltBaro %d", m.AltBaro)
	}
	if m.Has(FAltBaro) && !m.AltBaroGillham && (m.AltBaro%25 != 0 || m.AltBaro > 50175 || m.AltBaro < -1000) {
		fail("25 ft altitude %d", m.AltBaro)
	}
	if m.Has(FAltBaro) && m.AltBaroGillham && m.AltBaro%100 != 0 {
		fail("Gillham altitude %d", m.AltBaro)
	}
	if m.Has(FAltGNSS) && (m.AltGNSS < -1200 || m.AltGNSS > 126700) {
		fail("AltGNSS %d", m.AltGNSS)
	}
	if m.Has(FCPR) && (m.CPRLat > 0x1FFFF || m.CPRLon > 0x1FFFF) {
		fail("CPR %d %d", m.CPRLat, m.CPRLon)
	}
	if m.Has(FTrack) && !(m.Track >= 0 && m.Track < 360) {
		fail("Track %v", m.Track)
	}
	if m.Has(FHeading) && !(m.Heading >= 0 && m.Heading < 360) {
		fail("Heading %v", m.Heading)
	}
	if m.Has(FSelHeading) && !(m.SelHeading >= 0 && m.SelHeading < 360) {
		fail("SelHeading %v", m.SelHeading)
	}
	if m.Has(FGroundSpeed) && !(m.GS >= 0 && m.GS <= 5790) {
		fail("GS %v", m.GS)
	}
	if m.Has(FAirspeed) && !(m.Airspeed >= 0 && m.Airspeed <= 4088) {
		fail("Airspeed %v", m.Airspeed)
	}
	if m.Has(FVertRate) && (m.VertRate < -32640 || m.VertRate > 32640 || m.VertRate%64 != 0) {
		fail("VertRate %d", m.VertRate)
	}
	if m.Has(FGNSSDelta) && (m.GNSSDelta < -3150 || m.GNSSDelta > 3150) {
		fail("GNSSDelta %d", m.GNSSDelta)
	}
	if m.Has(FBaroSetting) && !(m.BaroSetting >= 800 && m.BaroSetting <= 1208.4) {
		fail("BaroSetting %v", m.BaroSetting)
	}
	if m.Has(FSelAlt) && (m.SelAlt < -1000 || m.SelAlt > 100000) {
		fail("SelAlt %d", m.SelAlt)
	}
	if m.Has(FSquawk) && m.Squawk&0x8888 != 0 {
		fail("Squawk %04X", m.Squawk)
	}
	if m.Has(FCallsign) {
		for _, c := range m.Callsign {
			if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ' ') {
				fail("Callsign %q", m.Callsign)
			}
		}
	}
	if m.Has(FCategory) && CategoryName(m.Category) == "" {
		fail("Category %02X", m.Category)
	}
	if m.Emergency > 7 || m.NACp > 15 || m.SIL > 3 || m.NACv > 7 || m.GVA > 3 || m.SDA > 3 || m.Version > 7 || m.TC > 31 || m.CA > 7 || m.FS > 7 {
		fail("field range")
	}
	if int(m.NumBDS) > MaxBDS {
		fail("NumBDS %d", m.NumBDS)
	}
	for i := 0; i < int(m.NumBDS); i++ {
		c := m.BDS[i]
		if !(c.Confidence > 0 && c.Confidence <= 1) || c.BDS == BDSNone || (i > 0 && c.Confidence > m.BDS[i-1].Confidence) {
			fail("BDS candidates %v", m.BDS[:m.NumBDS])
		}
	}
	if m.NumBDS > 0 && (!m.Has(FCommB) || (m.DF != 20 && m.DF != 21)) {
		fail("BDS candidates without Comm-B")
	}
	if m.AddressFromParity != (m.CRC == CRCAddressParity) {
		fail("AddressFromParity %v CRC %v", m.AddressFromParity, m.CRC)
	}
	if (m.DF == 17 || m.DF == 18) && Residual(m.Raw[:14]) != 0 {
		fail("accepted extended squitter with non-zero parity")
	}
}

// Random and truncated input must never panic, and whatever is accepted must
// be internally consistent.
func TestIndepNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(20260101))
	var m Message
	run := func(frame []byte) {
		for _, o := range []Options{{}, {FixErrors: 1}, {FixErrors: 2}, {FixErrors: 99, NoCommB: true}, {FixErrors: -3}} {
			err := DecodeOpts(frame, &m, o)
			checkInvariants(t, frame, &m, err)
			exercise(&m)
		}
		_ = Residual(frame)
		_ = Checksum(frame)
		_ = FrameDF(frame)
		c := append([]byte(nil), frame...)
		SetParity(c, rng.Uint32())
	}
	// Every length from nil to oversized.
	run(nil)
	for n := 0; n <= 40; n++ {
		for k := 0; k < 200; k++ {
			f := make([]byte, n)
			rng.Read(f)
			run(f)
		}
	}
	// Truncations of real frames.
	for _, s := range []string{bookPosEven, ownBDS50, bookAllCall, bookDF5} {
		f := hexFrame(t, s)
		for n := 0; n <= len(f); n++ {
			run(f[:n])
		}
	}
	// Random 7 and 14 byte frames of every DF, with and without valid parity
	// so that the field decoders are reached.
	for i := 0; i < 400000; i++ {
		n := 7
		if i&1 == 1 {
			n = 14
		}
		f := make([]byte, n)
		rng.Read(f)
		if i&2 != 0 {
			df := byte(rng.Intn(32))
			if (df >= 16) != (n == 14) {
				df ^= 16
			}
			f[0] = df<<3 | f[0]&7
		}
		switch i % 5 {
		case 0, 1, 2:
			SetParity(f, 0)
		case 3:
			SetParity(f, uint32(rng.Intn(80)))
		}
		if i%7 == 0 && n == 14 { // sparse payloads reach the status-bit logic
			for j := 4; j < 11; j++ {
				if rng.Intn(3) > 0 {
					f[j] = 0
				}
			}
			SetParity(f, 0)
		}
		err := DecodeOpts(f, &m, Options{FixErrors: i % 3})
		checkInvariants(t, f, &m, err)
		if i%16 == 0 {
			exercise(&m)
		}
	}
	// Every type code under every extended squitter header (DF17 with each
	// CA, DF18 with each CF), dense and sparse payloads.
	for first := 0x88; first < 0x98; first++ {
		for tc := uint64(0); tc < 32; tc++ {
			for k := 0; k < 300; k++ {
				me := rng.Uint64() >> 8
				if k%3 == 0 {
					me &= rng.Uint64() & rng.Uint64()
				}
				f := esFrame(byte(first), rng.Uint32()&0xFFFFFF, setBits(me, 1, 5, tc))
				err := Decode(f, &m)
				if err != nil || !m.Has(FTypeCode) && !(m.DF == 18 && (m.CF == 3 || m.CF == 4 || m.CF == 7)) {
					t.Fatalf("frame %X: %v", f, err)
				}
				if m.Has(FTypeCode) && uint64(m.TC) != tc {
					t.Fatalf("frame %X: TC %d", f, m.TC)
				}
				checkInvariants(t, f, &m, err)
				exercise(&m)
			}
		}
	}
	// Comm-B replies with sparse payloads and every register number.
	for k := 0; k < 100000; k++ {
		mb := rng.Uint64() >> 8
		switch k % 4 {
		case 0:
			mb &= rng.Uint64()
		case 1:
			mb = mb&^(0xFF<<48) | uint64(k>>2&0xFF)<<48
		case 2:
			mb &= rng.Uint64() & rng.Uint64() & rng.Uint64()
		}
		f := commBFrame(20+uint8(k&1), uint16(rng.Uint32())&0x1FFF, rng.Uint32()&0xFFFFFF, mb)
		err := Decode(f, &m)
		checkInvariants(t, f, &m, err)
		if k%8 == 0 {
			exercise(&m)
		}
	}
	// Free functions with arbitrary arguments.
	for i := 0; i < 200000; i++ {
		a, b := rng.Uint64(), rng.Uint32()
		alt := []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1e9, 0, 35000, 1e9}[i%7]
		c, n := InferBDS(a, alt)
		if n < 0 || n > MaxBDS {
			t.Fatalf("InferBDS n = %d", n)
		}
		for j := 0; j < n; j++ {
			if !(c[j].Confidence > 0 && c[j].Confidence <= 1) {
				t.Fatalf("InferBDS(%014X) confidence %v", a, c[j].Confidence)
			}
		}
		_, _ = GillhamAltitude(uint16(b))
		_ = EncodeID13(uint16(b))
		_, _ = EncodeAC13(int32(b))
		_, _ = EncodeAC12(int32(b))
		_, _ = EncodeGillham(int32(b))
		_ = CategoryName(uint8(b))
		_ = EmergencyName(uint8(b))
		_ = FormatSquawk(uint16(b))
		_ = FormatICAO(b)
		_ = BDS(b).String()
		_ = DFName(uint8(b))
		_ = DFIndexLabel(int(int32(b)))
		_ = FrameLen(uint8(b))
		_, _, _ = NICRc(uint8(b), int(int32(b>>8)), b&1 != 0, b&2 != 0, b&4 != 0)
		_, _ = NUCp(uint8(b))
		_ = NACpEPU(uint8(b))
		_ = NACvHFOM(uint8(b))
		_ = OverlayAddress(b, BDS(a))
		_ = NL(float64(int32(b)) / 1e6)
		_, _ = CPREncode(float64(int32(b))/1e7, float64(int64(a))/1e16, i&1 == 0, i&2 == 0)
		_, _, _ = CPRAirborneGlobal(b, uint32(a), uint32(a>>32), b>>3, i&1 == 0)
		_, _, _ = CPRSurfaceGlobal(float64(int32(b))/1e7, float64(int64(a))/1e16, b, uint32(a), uint32(a>>32), b>>3, i&1 == 0)
		_, _, _ = CPRAirborneLocal(float64(int32(b))/1e7, float64(int64(a))/1e16, b, uint32(a), i&1 == 0)
	}
	for _, s := range []string{"", "~", "zzzzzz", "~12345", "1234567", "abcdef", "~ABCDEF", strings.Repeat("F", 100)} {
		_, _ = ParseICAO(s)
		_, _ = DecodeHex(s)
		_, _ = EncodeCallsign(s)
	}
}

// The hot path must not allocate, whatever the frame type or option.
func TestIndepNoAllocs(t *testing.T) {
	frames := [][]byte{}
	for _, s := range []string{
		bookPosEven, bookVelGS, bookIdent, ownTSS, ownSurfDF18, ownBDS50,
		ownBDS60, bookBDS44, ownBDS10, bookAllCall, bookDF5, bookDF4, ownDF0,
	} {
		frames = append(frames, hexFrame(t, s))
	}
	damaged := hexFrame(t, bookPosEven)
	damaged[5] ^= 0x04
	damaged[9] ^= 0x40
	frames = append(frames, damaged, make([]byte, 3))
	var m Message
	_ = DecodeOpts(damaged, &m, Options{FixErrors: 2}) // build the syndrome table
	if n := testing.AllocsPerRun(200, func() {
		for _, f := range frames {
			_ = DecodeOpts(f, &m, Options{FixErrors: 2})
			_, _, _ = DisambiguateBDS(&m, 430, 115, 36000)
			_, _, _ = m.BestBDS()
			if m.Has(FCPR) {
				_, _, _ = CPRAirborneGlobal(m.CPRLat, m.CPRLon, 74158, 50194, false)
				_, _, _ = CPRAirborneLocal(52, 4, m.CPRLat, m.CPRLon, m.CPROdd)
				_, _, _ = CPRSurfaceGlobal(52, 4, m.CPRLat, m.CPRLon, 74158, 50194, false)
			}
		}
	}); n != 0 {
		t.Errorf("%v allocations per run", n)
	}
}
