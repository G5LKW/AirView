package modes

import (
	"math"
	"testing"
)

func TestIdentification(t *testing.T) {
	for _, c := range []struct {
		frame    string
		icao     uint32
		callsign string
		category string
		src      Source
		addr     AddrType
	}{
		// The 1090 MHz Riddle, identification chapter.
		{bookIdent, 0x4840D6, "KLM1023", "A0", SourceADSB, AddrADSBICAO},
		// Self-generated: DF17, and DF18 CF 6 (ADS-R of an ICAO address).
		{esHex(0x8D, 0x406C21, identME(4, 0, "EZY64TB")), 0x406C21, "EZY64TB", "A0", SourceADSB, AddrADSBICAO},
		{esHex(0x8D, 0xA3F1C2, identME(4, 1, "N4417Q")), 0xA3F1C2, "N4417Q", "A1", SourceADSB, AddrADSBICAO},
		{esHex(0x96, 0xA0821B, identME(4, 1, "N905XT")), 0xA0821B, "N905XT", "A1", SourceADSR, AddrADSRICAO},
	} {
		m := mustDecode(t, c.frame)
		if !m.Has(FTypeCode|FCallsign|FCategory) || m.TC != 4 {
			t.Errorf("%s: TC=%d valid=%b", c.frame, m.TC, m.Valid)
			continue
		}
		if m.ICAO != c.icao || m.CallsignString() != c.callsign || m.CategoryString() != c.category {
			t.Errorf("%s: icao=%06X callsign=%q category=%q", c.frame, m.ICAO, m.CallsignString(), m.CategoryString())
		}
		if m.Source != c.src || m.AddrType != c.addr || m.CRC != CRCOK || m.AddressFromParity {
			t.Errorf("%s: src=%v addr=%v crc=%v", c.frame, m.Source, m.AddrType, m.CRC)
		}
		if string(m.Callsign[:]) != (c.callsign + "        ")[:8] {
			t.Errorf("%s: raw callsign %q is not space padded", c.frame, m.Callsign[:])
		}
	}

	// Category sets: TC4 = A, TC3 = B, TC2 = C, TC1 = D.
	for tc, set := range map[uint64]string{4: "A", 3: "B", 2: "C", 1: "D"} {
		for cat := uint64(0); cat < 8; cat++ {
			me := tc<<51 | cat<<48 | 0x202CC371C32CE0&0xFFFFFFFFFFFF
			m, err := DecodeFrame(esFrame(0x8D, 0x123456, me))
			if err != nil {
				t.Fatal(err)
			}
			want := set + string(rune('0'+cat))
			if m.CategoryString() != want || m.Subtype != uint8(cat) || m.CallsignString() != "KLM1023" {
				t.Errorf("TC%d CA%d: category %q, want %q", tc, cat, m.CategoryString(), want)
			}
		}
	}

	// Illegal characters or an empty callsign leave the callsign invalid but
	// keep the category.
	for _, chars := range []uint64{0 /* all '@' */, 0x820820820820 /* all spaces */, 0x04104104105B /* AAAAAAA[ */} {
		m, err := DecodeFrame(esFrame(0x8D, 0x123456, 4<<51|3<<48|chars))
		if err != nil {
			t.Fatal(err)
		}
		if m.Has(FCallsign) || m.CallsignString() != "" || m.CategoryString() != "A3" {
			t.Errorf("chars %012X: callsign valid=%v %q category=%q", chars, m.Has(FCallsign), m.CallsignString(), m.CategoryString())
		}
	}
}

func TestAirbornePosition(t *testing.T) {
	for _, c := range []struct {
		frame    string
		tc       uint8
		alt      int32
		odd      bool
		lat, lon uint32
	}{
		// The 1090 MHz Riddle, airborne position chapter.
		{bookPosEven, 11, 38000, false, 93000, 51372},
		{bookPosOdd, 11, 38000, true, 74158, 50194},
		// Self-generated, raw CPR values chosen directly.
		{esHex(0x8D, 0x40058C, airbornePosME(11, 0, false, ac12For(39000), false, false, 80123, 12345)), 11, 39000, false, 80123, 12345},
		{esHex(0x8D, 0x40058C, airbornePosME(11, 0, false, ac12For(39000), false, true, 61234, 11111)), 11, 39000, true, 61234, 11111},
		{esHex(0x8D, 0xA2C1B0, airbornePosME(11, 0, false, ac12For(23650), false, false, 87001, 71002)), 11, 23650, false, 87001, 71002},
		// A Gillham (100 ft, Q=0) altitude: 10600 ft.
		{esHex(0x8D, 0xAB9201, airbornePosME(11, 0, false, gillhamAC12(10600), false, true, 78003, 19004)), 11, 10600, true, 78003, 19004},
	} {
		m := mustDecode(t, c.frame)
		if !m.Has(FTypeCode|FAltBaro|FCPR|FSurvStatus|FNICSuppB|FAlertSPI) || m.TC != c.tc {
			t.Errorf("%s: TC=%d valid=%b", c.frame, m.TC, m.Valid)
			continue
		}
		wantGillham := c.alt == 10600
		if m.AltBaro != c.alt || m.AltBaroGillham != wantGillham || m.CPROdd != c.odd || m.CPRSurface {
			t.Errorf("%s: alt=%d gillham=%v odd=%v surface=%v", c.frame, m.AltBaro, m.AltBaroGillham, m.CPROdd, m.CPRSurface)
		}
		if c.lat != 0 && (m.CPRLat != c.lat || m.CPRLon != c.lon) {
			t.Errorf("%s: cpr=%d,%d want %d,%d", c.frame, m.CPRLat, m.CPRLon, c.lat, c.lon)
		}
		if m.Has(FAltGNSS) || m.Has(FGroundSpeed) || m.Alert || m.SPI || m.SurvStatus != 0 {
			t.Errorf("%s: unexpected fields", c.frame)
		}
		if m.AirGround != AGAirborne { // all of these have CA=5
			t.Errorf("%s: air/ground=%v", c.frame, m.AirGround)
		}
	}
}

// airbornePosME builds an airborne position ME field.
func airbornePosME(tc uint8, ss uint8, nicb bool, ac12 uint16, tbit, odd bool, lat, lon uint32) uint64 {
	me := uint64(tc)<<51 | uint64(ss)<<49 | uint64(ac12)<<36 | uint64(lat)<<17 | uint64(lon)
	if nicb {
		me |= 1 << 48
	}
	if tbit {
		me |= 1 << 35
	}
	if odd {
		me |= 1 << 34
	}
	return me
}

// gillhamAC12 returns the 12-bit Gillham (Q=0) altitude code of an altitude,
// from the independent test encoder.
func gillhamAC12(alt int32) uint16 {
	g := id13FromModeA(gillhamEncode(alt))
	return (g&0x1F80)>>1 | g&0x3F
}

// ac12For returns the 12-bit Q=1 altitude code of an altitude.
func ac12For(alt int32) uint16 {
	n := uint16((alt + 1000) / 25)
	return (n&0x7F0)<<1 | 0x010 | n&0x00F
}

func TestAirbornePositionFields(t *testing.T) {
	decode := func(first byte, me uint64) Message {
		m, err := DecodeFrame(esFrame(first, 0xABCDEF, me))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	// Surveillance status. The alert states hide the SPI state.
	for ss, want := range map[uint8]struct {
		alert, spi bool
		valid      Fields
	}{
		0: {false, false, FAlert | FSPI},
		1: {true, false, FAlert},
		2: {true, false, FAlert},
		3: {false, true, FAlert | FSPI},
	} {
		m := decode(0x8D, airbornePosME(11, ss, false, ac12For(12000), true, true, 1, 2))
		if m.SurvStatus != ss || m.Alert != want.alert || m.SPI != want.spi || m.Valid&FAlertSPI != want.valid {
			t.Errorf("SS=%d: alert=%v spi=%v valid=%b", ss, m.Alert, m.SPI, m.Valid&FAlertSPI)
		}
		if !m.CPRTime || !m.CPROdd || m.CPRLat != 1 || m.CPRLon != 2 || m.AltBaro != 12000 {
			t.Errorf("SS=%d: t=%v odd=%v cpr=%d,%d alt=%d", ss, m.CPRTime, m.CPROdd, m.CPRLat, m.CPRLon, m.AltBaro)
		}
	}

	// NIC supplement B.
	if m := decode(0x8D, airbornePosME(13, 0, true, ac12For(500), false, false, 0x1FFFF, 0x1FFFF)); !m.NICSuppB || !m.Has(FNICSuppB) ||
		m.CPRLat != 0x1FFFF || m.CPRLon != 0x1FFFF || m.CPRTime || m.CPROdd {
		t.Errorf("NICb: %+v", m)
	}

	// GNSS height (TC20-22) goes to AltGNSS, never to AltBaro. It uses the
	// same 12-bit altitude code as TC9-18, in feet (DO-260B 2.2.3.2.3.4.3;
	// readsb and dump1090-fa decode it the same way): Q=1 is 25 ft steps,
	// Q=0 the Gillham code. Reading the raw bits as metres would give
	// nonsense: 35125 ft is raw field 0xB55 (Q=1), i.e. 2901 "m" = 9518 ft.
	for tc := uint8(20); tc <= 22; tc++ {
		m := decode(0x8D, airbornePosME(tc, 0, false, ac12For(35125), false, false, 100, 200))
		if !m.Has(FAltGNSS|FCPR) || m.Has(FAltBaro) || m.AltGNSS != 35125 {
			t.Errorf("TC%d: gnss=%d valid=%b", tc, m.AltGNSS, m.Valid)
		}
	}
	for _, c := range []struct {
		ac12 uint16
		want int32
	}{
		{0xB55, 35125},              // hand-built: N = 1445 = 0x5A5, Q inserted after bit 4
		{ac12For(-1000), -1000},     // lowest 25 ft value (N = 0, Q = 1)
		{ac12For(50175), 50175},     // highest 25 ft value
		{gillhamAC12(62300), 62300}, // above 50175 ft only the 100 ft code is left
		{gillhamAC12(1300), 1300},
	} {
		m := decode(0x8D, airbornePosME(21, 0, false, c.ac12, false, true, 300, 400))
		if !m.Has(FAltGNSS) || m.AltGNSS != c.want || m.Has(FAltBaro) {
			t.Errorf("TC21 field %03X: gnss=%d valid=%b, want %d ft", c.ac12, m.AltGNSS, m.Valid, c.want)
		}
	}
	// Field 0 means no height is available.
	if m := decode(0x8D, airbornePosME(20, 0, false, 0, false, false, 100, 200)); m.Has(FAltGNSS) || !m.Has(FCPR) {
		t.Errorf("TC20 without height: valid=%b", m.Valid)
	}

	// Altitude not available.
	if m := decode(0x8D, airbornePosME(11, 0, false, 0, false, false, 100, 200)); m.Has(FAltBaro) || !m.Has(FCPR) {
		t.Errorf("zero altitude: valid=%b", m.Valid)
	}

	// Gillham coded altitude (Q=0) in a position message.
	g := id13FromModeA(gillhamEncode(73200))
	if m := decode(0x8D, airbornePosME(12, 0, false, (g&0x1F80)>>1|g&0x3F, false, false, 100, 200)); !m.Has(FAltBaro) || m.AltBaro != 73200 || !m.AltBaroGillham {
		t.Errorf("Gillham: alt=%d gillham=%v", m.AltBaro, m.AltBaroGillham)
	}

	// TC0: no position, altitude may still be there.
	if m := decode(0x8D, airbornePosME(0, 0, false, ac12For(3100), false, false, 100, 200)); m.Has(FCPR) || !m.Has(FAltBaro) || m.AltBaro != 3100 {
		t.Errorf("TC0: valid=%b alt=%d", m.Valid, m.AltBaro)
	}

	// CA=4 (on the ground) with an airborne format: altitude is not used.
	if m := decode(0x8C, airbornePosME(11, 0, false, ac12For(3100), false, false, 100, 200)); m.Has(FAltBaro) || m.AirGround != AGGround || !m.Has(FCPR) {
		t.Errorf("CA4: valid=%b ag=%v", m.Valid, m.AirGround)
	}

	// CA=0 airborne format: air/ground uncertain.
	if m := decode(0x88, airbornePosME(11, 0, false, ac12For(3100), false, false, 100, 200)); m.AirGround != AGUncertain {
		t.Errorf("CA0: ag=%v", m.AirGround)
	}

	// Known transponder fault: TC15, no altitude, zero longitude and zeros
	// in the low latitude bits. It must not be offered as a position.
	m := decode(0x8D, airbornePosME(15, 0, false, 0, false, false, 0x0A000, 0))
	if m.TC != 15 || m.Has(FCPR) || m.Has(FAltBaro) {
		t.Errorf("TC15 fault frame: TC=%d cpr valid=%v", m.TC, m.Has(FCPR))
	}
	// The same type code with real data is fine.
	if m := decode(0x8D, airbornePosME(15, 0, false, ac12For(3100), false, false, 0x12000, 0)); !m.Has(FCPR) {
		t.Error("TC15 with altitude must keep its position")
	}
}

func TestSurfacePosition(t *testing.T) {
	// The 1090 MHz Riddle, surface position chapter: 17 kt, track 92.8125.
	m := mustDecode(t, bookSurf)
	if m.TC != 7 || !m.Has(FCPR|FGroundSpeed|FTrack) || !m.CPRSurface || !m.CPROdd {
		t.Fatalf("TC=%d valid=%b", m.TC, m.Valid)
	}
	if m.GS != 17 || m.Track != 92.8125 || m.CPRLat != 39195 || m.CPRLon != 110320 {
		t.Fatalf("gs=%v trk=%v cpr=%d,%d", m.GS, m.Track, m.CPRLat, m.CPRLon)
	}
	if m.AirGround != AGGround || m.Has(FAltBaro) || m.CA != 4 {
		t.Fatalf("ag=%v ca=%d", m.AirGround, m.CA)
	}
	for _, c := range []struct {
		frame    string
		odd      bool
		lat, lon uint32
	}{
		{bookSurfEven, false, 115609, 116941},
		{bookSurfOdd, true, 39199, 110269},
	} {
		m := mustDecode(t, c.frame)
		if m.CPROdd != c.odd || m.CPRLat != c.lat || m.CPRLon != c.lon || !m.CPRSurface {
			t.Errorf("%s: odd=%v cpr=%d,%d", c.frame, m.CPROdd, m.CPRLat, m.CPRLon)
		}
	}

	// Self-generated DF17 CA 7: movement 43 -> 19 kt, track 15 -> 42.1875.
	m = mustDecode(t, esHex(0x8F, 0xC8F00A, surfME(7, 43, true, 15, true, -43.49, 172.54)))
	if m.GS != 19 || !near(m.Track, 42.1875, 1e-9) || !m.CPROdd || m.ICAO != 0xC8F00A {
		t.Fatalf("gs=%v trk=%v odd=%v", m.GS, m.Track, m.CPROdd)
	}
}

func TestMovementSpeed(t *testing.T) {
	// Band edges from DO-260B / The 1090 MHz Riddle table.
	for _, c := range []struct {
		mov  uint32
		want float64
	}{
		{1, 0}, {2, 0.125}, {8, 0.875}, {9, 1}, {12, 1.75}, {13, 2}, {38, 14.5}, {39, 15}, {41, 17},
		{93, 69}, {94, 70}, {108, 98}, {109, 100}, {123, 170}, {124, 175},
	} {
		if got := movementSpeed(c.mov); got != c.want {
			t.Errorf("movementSpeed(%d) = %v, want %v", c.mov, got, c.want)
		}
	}
	prev := -1.0
	for mov := uint32(1); mov <= 124; mov++ {
		v := movementSpeed(mov)
		if v <= prev {
			t.Fatalf("movementSpeed is not increasing at %d", mov)
		}
		prev = v
	}
	// Codes 0 and 125-127 carry no speed; a cleared status bit no track.
	for _, mov := range []uint64{0, 125, 126, 127} {
		me := uint64(6)<<51 | mov<<44 | 0x7F<<36 // track bits set, status clear
		m, err := DecodeFrame(esFrame(0x8C, 0x123456, me))
		if err != nil {
			t.Fatal(err)
		}
		if m.Has(FGroundSpeed) || m.Has(FTrack) || !m.Has(FCPR) || !m.CPRSurface {
			t.Errorf("mov %d: valid=%b", mov, m.Valid)
		}
	}
}

func TestVelocity(t *testing.T) {
	// The 1090 MHz Riddle, airborne velocity chapter, message A (subtype 1):
	// 159.20 kt, 182.88 deg, -832 ft/min from GNSS, GNSS 550 ft above baro.
	m := mustDecode(t, bookVelGS)
	if m.TC != 19 || m.Subtype != 1 || !m.Has(FGroundSpeed|FTrack|FVertRate|FGNSSDelta|FNACv) {
		t.Fatalf("TC=%d st=%d valid=%b", m.TC, m.Subtype, m.Valid)
	}
	if !near(m.GS, 159.20, 0.005) || !near(m.Track, 182.88, 0.005) || m.VertRate != -832 || m.VertRateBaro || m.GNSSDelta != 550 {
		t.Fatalf("gs=%v trk=%v vr=%d baro=%v delta=%d", m.GS, m.Track, m.VertRate, m.VertRateBaro, m.GNSSDelta)
	}
	if m.Has(FAirspeed) || m.Has(FHeading) || m.NACv != 0 || m.IntentChange {
		t.Fatalf("unexpected airspeed/heading, NACv=%d", m.NACv)
	}

	// Message B (subtype 3): TAS 375 kt, heading 243.98, -2304 ft/min baro.
	m = mustDecode(t, bookVelAS)
	if m.Subtype != 3 || !m.Has(FAirspeed|FHeading|FVertRate) || m.Has(FGroundSpeed) || m.Has(FTrack) || m.Has(FGNSSDelta) {
		t.Fatalf("st=%d valid=%b", m.Subtype, m.Valid)
	}
	if m.Airspeed != 375 || !m.AirspeedTAS || !near(m.Heading, 243.98, 0.005) || m.VertRate != -2304 || !m.VertRateBaro {
		t.Fatalf("as=%v tas=%v hdg=%v vr=%d baro=%v", m.Airspeed, m.AirspeedTAS, m.Heading, m.VertRate, m.VertRateBaro)
	}

	// Self-generated ground velocities, also through DF18 (ADS-R, TIS-B).
	// GS and track follow from the components: hypot and atan2(east, north).
	for _, c := range []struct {
		first          byte
		west           bool
		ew             uint32 // kt
		south          bool
		ns             uint32
		vrBaro, vrDown bool
		vrField        uint32
		dDown          bool
		dField         uint32
	}{
		{0x8D, true, 120, true, 50, false, true, 5, false, 7},   // 130 kt, -256 GNSS, +150
		{0x96, false, 0, false, 100, true, false, 6, true, 7},   // due north, +320 baro, -150
		{0x92, false, 33, false, 56, true, false, 1, false, 32}, // 65 kt, level, +775
		{0x8D, true, 360, false, 105, false, false, 41, false, 0},
	} {
		s := esHex(c.first, 0xA3D4E5, velocityME(1, false, 0, c.west, c.ew+1, c.south, c.ns+1, c.vrBaro, c.vrDown, c.vrField, c.dDown, c.dField))
		m := mustDecode(t, s)
		vx, vy := float64(c.ew), float64(c.ns)
		if c.west {
			vx = -vx
		}
		if c.south {
			vy = -vy
		}
		wantTrk := math.Mod(math.Atan2(vx, vy)*180/math.Pi+360, 360)
		if !m.Has(FGroundSpeed|FTrack|FVertRate) || !near(m.GS, math.Hypot(vx, vy), 1e-9) || !near(m.Track, wantTrk, 1e-9) {
			t.Errorf("%s: gs=%v trk=%v, want %v %v", s, m.GS, m.Track, math.Hypot(vx, vy), wantTrk)
		}
		wantVR := int32(c.vrField-1) * 64
		if c.vrDown {
			wantVR = -wantVR
		}
		if m.VertRate != wantVR || m.VertRateBaro != c.vrBaro {
			t.Errorf("%s: vr=%d baro=%v, want %d %v", s, m.VertRate, m.VertRateBaro, wantVR, c.vrBaro)
		}
		if c.dField != 0 {
			wantD := int32(c.dField-1) * 25
			if c.dDown {
				wantD = -wantD
			}
			if !m.Has(FGNSSDelta) || m.GNSSDelta != wantD {
				t.Errorf("%s: delta=%d valid=%v, want %d", s, m.GNSSDelta, m.Has(FGNSSDelta), wantD)
			}
		} else if m.Has(FGNSSDelta) {
			t.Errorf("%s: delta without data", s)
		}
	}

	// Subtype 3: IAS 292 kt, baro rate +128 ft/min, heading not available.
	m = mustDecode(t, esHex(0x8D, 0xA35E01, velocityME(3, false, 0, false, 0, false, 293, true, false, 3, false, 0)))
	if m.Subtype != 3 || !m.Has(FAirspeed|FVertRate) || m.Has(FHeading) || m.Airspeed != 292 || m.AirspeedTAS ||
		m.VertRate != 128 || !m.VertRateBaro || m.NACv != 0 {
		t.Fatalf("as=%v tas=%v vr=%d hdgValid=%v", m.Airspeed, m.AirspeedTAS, m.VertRate, m.Has(FHeading))
	}
	// Subtype 3: TAS 437 kt, -1856 ft/min, heading status clear.
	m = mustDecode(t, esHex(0x8D, 0x4400C1, velocityME(3, false, 0, false, 0, true, 438, true, true, 30, false, 0)))
	if m.Airspeed != 437 || !m.AirspeedTAS || m.VertRate != -1856 || m.Has(FHeading) {
		t.Fatalf("as=%v vr=%d", m.Airspeed, m.VertRate)
	}
}

// velocityME builds a TC19 ME field.
func velocityME(st uint8, ic bool, nacv uint8, dir1 bool, v1 uint32, dir2 bool, v2 uint32, vrBaro, vrDown bool, vr uint32, dDown bool, d uint32) uint64 {
	me := uint64(19)<<51 | uint64(st)<<48 | uint64(nacv)<<43 | uint64(v1)<<32 | uint64(v2)<<21 | uint64(vr)<<10 | uint64(d)
	for _, b := range []struct {
		on  bool
		bit uint
	}{{ic, 9}, {dir1, 14}, {dir2, 25}, {vrBaro, 36}, {vrDown, 37}, {dDown, 49}} {
		if b.on {
			me |= 1 << (56 - b.bit)
		}
	}
	return me
}

func TestVelocitySubtypes(t *testing.T) {
	decode := func(me uint64) Message {
		m, err := DecodeFrame(esFrame(0x8D, 0x123456, me))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Subtype 1: 300 kt east, 400 kt north -> 500 kt, track 36.87.
	m := decode(velocityME(1, true, 3, false, 301, false, 401, true, false, 11, true, 5))
	if !near(m.GS, 500, 1e-9) || !near(m.Track, 36.8699, 1e-3) || m.VertRate != 640 || !m.VertRateBaro || m.GNSSDelta != -100 || m.NACv != 3 || !m.IntentChange {
		t.Errorf("st1: gs=%v trk=%v vr=%d delta=%d nacv=%d ic=%v", m.GS, m.Track, m.VertRate, m.GNSSDelta, m.NACv, m.IntentChange)
	}
	// Subtype 2 (supersonic): 4 kt resolution. 300 west, 400 south.
	m = decode(velocityME(2, false, 0, true, 301, true, 401, false, true, 2, false, 0))
	if !near(m.GS, 2000, 1e-9) || !near(m.Track, 216.8699, 1e-3) || m.VertRate != -64 || m.VertRateBaro || m.Has(FGNSSDelta) {
		t.Errorf("st2: gs=%v trk=%v vr=%d", m.GS, m.Track, m.VertRate)
	}
	// All four quadrants.
	for _, c := range []struct {
		west, south bool
		want        float64
	}{{false, false, 45}, {false, true, 135}, {true, true, 225}, {true, false, 315}} {
		m := decode(velocityME(1, false, 0, c.west, 101, c.south, 101, false, false, 1, false, 0))
		if !near(m.Track, c.want, 1e-9) || !near(m.GS, 100*math.Sqrt2, 1e-9) || !m.Has(FVertRate) || m.VertRate != 0 {
			t.Errorf("west=%v south=%v: trk=%v gs=%v", c.west, c.south, m.Track, m.GS)
		}
	}
	// Zero velocity: speed 0 is valid, the track is not.
	m = decode(velocityME(1, false, 0, false, 1, false, 1, false, false, 0, false, 0))
	if !m.Has(FGroundSpeed) || m.GS != 0 || m.Has(FTrack) || m.Has(FVertRate) {
		t.Errorf("zero velocity: valid=%b", m.Valid)
	}
	// A missing component means no ground velocity at all.
	m = decode(velocityME(1, false, 0, false, 0, false, 200, false, false, 0, false, 0))
	if m.Has(FGroundSpeed) || m.Has(FTrack) {
		t.Errorf("missing E/W component: valid=%b", m.Valid)
	}
	// Subtype 3: IAS, heading 90 deg. Subtype 4: 4 kt resolution TAS.
	m = decode(velocityME(3, false, 2, true, 256, false, 251, false, false, 0, false, 0))
	if m.Heading != 90 || m.Airspeed != 250 || m.AirspeedTAS || !m.Has(FHeading|FAirspeed) || m.Has(FGroundSpeed) {
		t.Errorf("st3: hdg=%v as=%v tas=%v", m.Heading, m.Airspeed, m.AirspeedTAS)
	}
	m = decode(velocityME(4, false, 2, true, 512, true, 301, false, false, 0, false, 0))
	if m.Heading != 180 || m.Airspeed != 1200 || !m.AirspeedTAS {
		t.Errorf("st4: hdg=%v as=%v tas=%v", m.Heading, m.Airspeed, m.AirspeedTAS)
	}
	// Airspeed not available.
	m = decode(velocityME(3, false, 0, true, 256, false, 0, false, false, 0, false, 0))
	if m.Has(FAirspeed) || !m.Has(FHeading) {
		t.Errorf("st3 without airspeed: valid=%b", m.Valid)
	}
	// Reserved subtypes decode nothing.
	for _, st := range []uint8{0, 5, 6, 7} {
		m = decode(velocityME(st, false, 3, false, 301, false, 401, true, false, 11, true, 5))
		if m.Subtype != st || m.Valid != FTypeCode|FCapability {
			t.Errorf("st%d: valid=%b", st, m.Valid)
		}
	}
	r := mustDecode(t, esHex(0x91, 0x1C05A0, velocityME(0, false, 0, false, 0x5A4, false, 0x2CF, true, false, 0x109, false, 0x64)))
	if r.TC != 19 || r.Subtype != 0 || r.Has(FGroundSpeed) || r.AddrType != AddrADSBOther {
		t.Errorf("reserved velocity subtype: %+v", r)
	}
}

func TestAircraftStatus(t *testing.T) {
	// Self-generated: no emergency, squawks 3415 and 1052.
	for frame, squawk := range map[string]string{
		esHex(0x8D, 0xA2C1B7, 28<<51|1<<48|uint64(id13FromModeA(0x3415))<<32): "3415",
		esHex(0x8D, 0xC06811, 28<<51|1<<48|uint64(id13FromModeA(0x1052))<<32): "1052",
	} {
		m := mustDecode(t, frame)
		if m.TC != 28 || m.Subtype != 1 || !m.Has(FEmergency|FSquawk) || m.Emergency != EmergencyNone ||
			m.SquawkString() != squawk || m.EmergencyString() != "" {
			t.Errorf("%s: st=%d emergency=%d squawk=%s", frame, m.Subtype, m.Emergency, m.SquawkString())
		}
	}
	// Every emergency state with a matching squawk.
	wantNames := []string{"", "general", "medical", "minfuel", "nordo", "unlawful", "downed", ""}
	for state := uint64(0); state < 8; state++ {
		me := uint64(28)<<51 | 1<<48 | state<<45 | uint64(id13FromModeA(0x7700))<<32
		m, err := DecodeFrame(esFrame(0x8D, 0x123456, me))
		if err != nil {
			t.Fatal(err)
		}
		if m.Emergency != uint8(state) || m.Squawk != 0x7700 {
			t.Errorf("state %d: emergency=%d squawk=%04X", state, m.Emergency, m.Squawk)
		}
		want := wantNames[state]
		if want == "" {
			want = "general" // falls back to the 7700 squawk
		}
		if got := m.EmergencyString(); got != want {
			t.Errorf("state %d: EmergencyString = %q, want %q", state, got, want)
		}
	}
	// Subtype 2: ACAS RA broadcast, same layout as BDS 3,0.
	me := uint64(28)<<51 | 2<<48 | uint64(0x3000)<<34 | 1<<26 | uint64(0x4840D6)<<2
	m, err := DecodeFrame(esFrame(0x8D, 0x123456, me))
	if err != nil {
		t.Fatal(err)
	}
	ra, ok := m.ACASRA()
	if !ok || !ra.Active() || !ra.Corrective() || ra.Downward() || ra.ThreatType != 1 || ra.ThreatICAO != 0x4840D6 {
		t.Errorf("RA broadcast: ok=%v %+v", ok, ra)
	}
	if m.Has(FEmergency) || m.Has(FSquawk) {
		t.Error("RA broadcast must not report emergency/squawk")
	}
}

func TestTargetStateV2(t *testing.T) {
	type want struct {
		alt       int32
		qnh       float64
		hdg       float64 // < 0: not present
		modes     NavModes
		modesOK   bool
		tcas      bool
		nacp, sil uint8
	}
	// Self-generated TC29 subtype 1 messages: altitude field n -> (n-1)*32
	// ft, QNH field q -> 800+(q-1)*0.8 hPa, heading field h -> h*180/256.
	tss := func(altField, qnhField uint64, hdg int, modeBits []uint, nacp uint64) string {
		me := uint64(29)<<51 | 1<<49
		me = setBits(me, 10, 20, altField)
		me = setBits(me, 21, 29, qnhField)
		if hdg >= 0 {
			me = setBits(me, 30, 30, 1)
			me = setBits(me, 31, 39, uint64(hdg))
		}
		me = setBits(me, 40, 43, nacp)
		me = setBits(me, 44, 44, 1)
		me = setBits(me, 45, 46, 3)
		for _, b := range modeBits {
			me = setBits(me, b, b, 1)
		}
		return esHex(0x8D, 0xA08F11, me)
	}
	for frame, w := range map[string]want{
		ownTSS:                            {24000, 1012.8, 66.796875, NavAutopilot | NavVNAV | NavLNAV | NavTCAS, true, true, 9, 3},
		tss(439, 267, 327, []uint{53}, 9): {14016, 1012.8, 229.921875, 0, false, true, 9, 3},
		tss(1158, 268, -1, []uint{47, 48, 50, 53}, 10): {37024, 1013.6, -1, NavAutopilot | NavAltHold | NavTCAS, true, true, 10, 3},
		tss(720, 267, -1, nil, 10):                     {23008, 1012.8, -1, 0, false, false, 10, 3},
		tss(1126, 268, 448, []uint{53}, 8):             {36000, 1013.6, 315, 0, false, true, 8, 3},
		tss(939, 268, -1, []uint{47, 48, 49, 53}, 10):  {30016, 1013.6, -1, NavAutopilot | NavVNAV | NavTCAS, true, true, 10, 3},
	} {
		m := mustDecode(t, frame)
		if m.TC != 29 || m.Subtype != 1 || !m.Has(FSelAlt|FBaroSetting|FNACp|FSIL|FNICBaro|FSILType) {
			t.Errorf("%s: TC=%d st=%d valid=%b", frame, m.TC, m.Subtype, m.Valid)
			continue
		}
		if m.SelAlt != w.alt || m.SelAltFMS || !near(m.BaroSetting, w.qnh, 1e-6) {
			t.Errorf("%s: selalt=%d fms=%v qnh=%v", frame, m.SelAlt, m.SelAltFMS, m.BaroSetting)
		}
		if w.hdg < 0 {
			if m.Has(FSelHeading) {
				t.Errorf("%s: unexpected selected heading %v", frame, m.SelHeading)
			}
		} else if !m.Has(FSelHeading) || !near(m.SelHeading, w.hdg, 0.05) || m.SelHeadingIsTrack {
			t.Errorf("%s: selhdg=%v valid=%v", frame, m.SelHeading, m.Has(FSelHeading))
		}
		if m.Has(FNavModes) != w.modesOK || m.NavModes != w.modes {
			t.Errorf("%s: modes=%06b valid=%v", frame, m.NavModes, m.Has(FNavModes))
		}
		if tcas, ok := m.TCASOperational(); !ok || tcas != w.tcas {
			t.Errorf("%s: TCAS operational=%v ok=%v", frame, tcas, ok)
		}
		if m.NACp != w.nacp || m.SIL != w.sil || !m.NICBaro {
			t.Errorf("%s: nacp=%d sil=%d nicbaro=%v", frame, m.NACp, m.SIL, m.NICBaro)
		}
		if m.Has(FEmergency) || m.Has(FAltBaro) {
			t.Errorf("%s: unexpected fields", frame)
		}
	}

	// Synthetic: FMS source, approach + LNAV, negative-sign heading, no QNH.
	var me uint64 = 29<<51 | 1<<49
	me = setBits(me, 8, 8, 1)        // SIL per sample
	me = setBits(me, 9, 9, 1)        // FMS
	me = setBits(me, 10, 20, 1+1250) // 40000 ft
	me = setBits(me, 30, 30, 1)      // heading valid
	me = setBits(me, 31, 39, 0x180)  // 270 deg
	me = setBits(me, 40, 43, 11)     // NACp
	me = setBits(me, 45, 46, 2)      // SIL
	me = setBits(me, 47, 47, 1)      // modes populated
	me = setBits(me, 52, 52, 1)      // approach
	me = setBits(me, 54, 54, 1)      // LNAV
	m, err := DecodeFrame(esFrame(0x8D, 0x123456, me))
	if err != nil {
		t.Fatal(err)
	}
	if m.SelAlt != 40000 || !m.SelAltFMS || m.Has(FBaroSetting) || m.SelHeading != 270 || m.NACp != 11 || m.SIL != 2 ||
		m.NICBaro || !m.SILPerSample || m.NavModes != NavApproach|NavLNAV || !m.Has(FNavModes) {
		t.Errorf("synthetic v2: %+v", m)
	}
	// Altitude not available.
	m, _ = DecodeFrame(esFrame(0x8D, 0x123456, 29<<51|1<<49))
	if m.Has(FSelAlt) || m.Has(FBaroSetting) || m.Has(FSelHeading) || m.Has(FNavModes) {
		t.Errorf("empty v2: valid=%b", m.Valid)
	}
}

func TestTargetStateV1(t *testing.T) {
	var me uint64 = 29 << 51      // subtype 0
	me = setBits(me, 8, 9, 3)     // vertical source FMS/RNAV
	me = setBits(me, 16, 25, 360) // 35000 ft
	me = setBits(me, 26, 27, 1)   // horizontal source MCP/FCU
	me = setBits(me, 28, 36, 275) // target angle
	me = setBits(me, 37, 37, 1)   // it is a track angle
	me = setBits(me, 40, 43, 7)   // NACp
	me = setBits(me, 44, 44, 1)   // NICbaro
	me = setBits(me, 45, 46, 2)   // SIL
	me = setBits(me, 54, 56, 3)   // minimum fuel
	m, err := DecodeFrame(esFrame(0x8D, 0x123456, me))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subtype != 0 || !m.Has(FSelAlt|FSelHeading|FNACp|FSIL|FNICBaro|FEmergency) {
		t.Fatalf("valid=%b", m.Valid)
	}
	if m.SelAlt != 35000 || !m.SelAltFMS || m.SelHeading != 275 || !m.SelHeadingIsTrack || m.NACp != 7 || m.SIL != 2 ||
		!m.NICBaro || m.Emergency != EmergencyMinFuel || m.EmergencyString() != "minfuel" {
		t.Fatalf("%+v", m)
	}
	if _, ok := m.TCASOperational(); ok {
		t.Error("version 1 target state has no single TCAS bit")
	}
	// Target altitude out of range, heading out of range, no sources.
	me = setBits(me, 16, 25, 1011)
	me = setBits(me, 28, 36, 360)
	m, _ = DecodeFrame(esFrame(0x8D, 0x123456, me))
	if m.Has(FSelAlt) || m.Has(FSelHeading) {
		t.Errorf("out of range values accepted: valid=%b", m.Valid)
	}
	// Backward compatibility flag set: unknown layout, nothing decoded.
	m, _ = DecodeFrame(esFrame(0x8D, 0x123456, setBits(me, 11, 11, 1)))
	if m.Valid != FTypeCode|FCapability {
		t.Errorf("bit 11 set: valid=%b", m.Valid)
	}
	// Reserved subtypes 2 and 3.
	m, _ = DecodeFrame(esFrame(0x8D, 0x123456, 29<<51|2<<49|0xFFFFFFFFFF))
	if m.Subtype != 2 || m.Valid != FTypeCode|FCapability {
		t.Errorf("subtype 2: valid=%b", m.Valid)
	}
}

func TestOperationalStatusV2(t *testing.T) {
	// Self-generated airborne version 2 status messages (DO-260B
	// 2.2.3.2.7.2): CC bit 11 = TCAS operational, SDA bits 31-32, NIC-A 44,
	// NACp 45-48, GVA 49-50, SIL 51-52 (3, per hour), NICbaro 53, true north.
	ops := func(nicA bool, nacp, sda, gva uint64) string {
		me := uint64(31) << 51
		me = setBits(me, 11, 11, 1)
		me = setBits(me, 31, 32, sda)
		me = setBits(me, 41, 43, 2)
		if nicA {
			me = setBits(me, 44, 44, 1)
		}
		me = setBits(me, 45, 48, nacp)
		me = setBits(me, 49, 50, gva)
		me = setBits(me, 51, 52, 3)
		return esHex(0x8D, 0xA1A8D1, setBits(me, 53, 53, 1))
	}
	for _, c := range []struct {
		frame          string
		nicA           bool
		nacp, sda, gva uint8
	}{
		{ownOpStatus, true, 10, 3, 2},
		{ops(false, 9, 2, 2), false, 9, 2, 2},
		{ops(false, 10, 2, 1), false, 10, 2, 1},
	} {
		m := mustDecode(t, c.frame)
		if m.TC != 31 || m.Subtype != 0 || !m.Has(FVersion|FNICSuppA|FNACp|FSIL|FSILType|FNICBaro|FGVA|FSDA|FHRD|FOpStatus) {
			t.Errorf("%s: TC=%d st=%d valid=%b", c.frame, m.TC, m.Subtype, m.Valid)
			continue
		}
		if m.Version != 2 || m.NICSuppA != c.nicA || m.NACp != c.nacp || m.SDA != c.sda || m.GVA != c.gva {
			t.Errorf("%s: v=%d nicA=%v nacp=%d sda=%d gva=%d", c.frame, m.Version, m.NICSuppA, m.NACp, m.SDA, m.GVA)
		}
		// SIL 3 per hour, NICbaro 1, heading reference true north.
		if m.SIL != 3 || m.SILPerSample || !m.NICBaro || m.HRDMagnetic {
			t.Errorf("%s: sil=%d perSample=%v nicbaro=%v hrdMag=%v", c.frame, m.SIL, m.SILPerSample, m.NICBaro, m.HRDMagnetic)
		}
		// Capability classes include ACAS.
		if tcas, ok := m.TCASOperational(); !ok || !tcas {
			t.Errorf("%s: TCAS operational=%v ok=%v", c.frame, tcas, ok)
		}
		if ra, ok := m.TCASRAActive(); !ok || ra {
			t.Errorf("%s: RA active=%v ok=%v", c.frame, ra, ok)
		}
		if id, ok := m.IdentActive(); !ok || id {
			t.Errorf("%s: ident=%v ok=%v", c.frame, id, ok)
		}
		if m.Has(FNICSuppC) || m.Has(FLengthWidth) || m.Has(FTrackIsHeading) || m.AirGround != AGAirborne {
			t.Errorf("%s: surface fields on an airborne status", c.frame)
		}
	}

	// Self-generated surface status relayed by ADS-R. Version 2, NIC-A 0,
	// NIC-C 0, NACv 1, L/W 1, SDA 2, NACp 9, SIL 3 per hour, true north.
	sme := uint64(31)<<51 | 1<<48
	sme = setBits(sme, 17, 19, 1)
	sme = setBits(sme, 21, 24, 1)
	sme = setBits(sme, 31, 32, 2)
	sme = setBits(sme, 41, 43, 2)
	sme = setBits(sme, 45, 48, 9)
	sme = setBits(sme, 51, 52, 3)
	m := mustDecode(t, esHex(0x96, 0xA4D02E, sme))
	if m.Subtype != 1 || m.Version != 2 || !m.Has(FNACv|FNICSuppC|FLengthWidth|FTrackIsHeading|FSDA) || m.Has(FGVA) || m.Has(FNICBaro) {
		t.Fatalf("st=%d v=%d valid=%b", m.Subtype, m.Version, m.Valid)
	}
	if m.NICSuppA || m.NICSuppC || m.NACv != 1 || m.LengthWidth != 1 || m.SDA != 2 || m.NACp != 9 || m.SIL != 3 || m.SILPerSample || m.HRDMagnetic {
		t.Fatalf("%+v", m)
	}
	if m.AirGround != AGGround || m.Source != SourceADSR || m.AddrType != AddrADSRICAO {
		t.Fatalf("ag=%v src=%v addr=%v", m.AirGround, m.Source, m.AddrType)
	}
	if _, ok := m.TCASOperational(); ok {
		t.Error("surface status has no TCAS capability bit")
	}
	// Reserved subtype: nothing decoded.
	r := mustDecode(t, esHex(0x92, 0x479250, 31<<51|4<<48|0xB22E16FBDC3B))
	if r.TC != 31 || r.Subtype != 4 || r.Has(FVersion) {
		t.Fatalf("reserved subtype: st=%d valid=%b", r.Subtype, r.Valid)
	}
}

func TestOperationalStatusSynthetic(t *testing.T) {
	build := func(st, version uint8, edit func(me uint64) uint64) Message {
		var me uint64 = 31<<51 | uint64(st)<<48
		me = setBits(me, 41, 43, uint64(version))
		me = edit(me)
		m, err := DecodeFrame(esFrame(0x8D, 0x123456, me))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Version 1 airborne: "Not-TCAS" bit is inverted, no GVA/SDA/SIL type.
	m := build(0, 1, func(me uint64) uint64 {
		me = setBits(me, 11, 11, 1) // Not-TCAS
		me = setBits(me, 44, 44, 1) // NIC supplement
		me = setBits(me, 45, 48, 8)
		me = setBits(me, 51, 52, 2)
		me = setBits(me, 53, 53, 1) // NICbaro
		return setBits(me, 54, 54, 1)
	})
	if m.Version != 1 || !m.NICSuppA || m.NACp != 8 || m.SIL != 2 || !m.NICBaro || !m.HRDMagnetic {
		t.Errorf("v1 airborne: %+v", m)
	}
	if m.Has(FGVA) || m.Has(FSDA) || m.Has(FSILType) || m.Has(FNICSuppC) {
		t.Errorf("v1 airborne has v2 fields: valid=%b", m.Valid)
	}
	if tcas, ok := m.TCASOperational(); !ok || tcas {
		t.Errorf("v1 Not-TCAS=1: operational=%v ok=%v", tcas, ok)
	}
	// Version 1 surface: length/width, track/heading flag, no NACv.
	m = build(1, 1, func(me uint64) uint64 {
		me = setBits(me, 21, 24, 9)
		return setBits(me, 53, 53, 1)
	})
	if !m.Has(FLengthWidth|FTrackIsHeading) || m.LengthWidth != 9 || !m.TrackIsHeading || m.Has(FNACv) || m.Has(FNICBaro) || m.AirGround != AGGround {
		t.Errorf("v1 surface: %+v", m)
	}
	// Version 2 surface: NACv, NIC supplement C, SIL per sample, RA + ident.
	m = build(1, 2, func(me uint64) uint64 {
		me = setBits(me, 17, 19, 4)
		me = setBits(me, 20, 20, 1)
		me = setBits(me, 21, 24, 15)
		me = setBits(me, 27, 28, 3)
		me = setBits(me, 31, 32, 1)
		return setBits(me, 55, 55, 1)
	})
	if m.NACv != 4 || !m.NICSuppC || m.LengthWidth != 15 || m.SDA != 1 || !m.SILPerSample || m.TrackIsHeading {
		t.Errorf("v2 surface: %+v", m)
	}
	if ra, ok := m.TCASRAActive(); !ok || !ra {
		t.Errorf("RA active=%v ok=%v", ra, ok)
	}
	if id, ok := m.IdentActive(); !ok || !id {
		t.Errorf("ident=%v ok=%v", id, ok)
	}
	// Version 0 (DO-260): only the version is reported.
	m = build(0, 0, func(me uint64) uint64 { return setBits(me, 45, 48, 9) })
	if !m.Has(FVersion) || m.Version != 0 || m.Has(FNACp) || m.Has(FOpStatus) {
		t.Errorf("v0: valid=%b", m.Valid)
	}
	// Unknown capability class / operational mode formats are not interpreted.
	m = build(0, 2, func(me uint64) uint64 {
		me = setBits(me, 9, 10, 1)
		return setBits(me, 25, 26, 2)
	})
	if _, ok := m.TCASOperational(); ok || m.Has(FSDA) {
		t.Errorf("unknown CC/OM format interpreted: valid=%b", m.Valid)
	}
	if _, ok := m.TCASRAActive(); ok {
		t.Error("unknown OM format interpreted")
	}
}

func TestDF18ControlField(t *testing.T) {
	// Self-generated DF18 frames, one per control field value.
	cf5Pos := esHex(0x95, 0x298FD0, airbornePosME(13, 0, false, ac12For(700), false, true, 70001, 30002))
	cf6IMF := esHex(0x96, 0x130DA1, airbornePosME(18, 0, true, ac12For(2000), false, true, 68003, 31004))
	cf2TC0 := esHex(0x92, 0xEF92C0, airbornePosME(0, 0, true, ac12For(3100), false, false, 0, 0))
	for _, c := range []struct {
		frame string
		cf    uint8
		addr  AddrType
		src   Source
		label string
		imf   bool
	}{
		{fhex(esFrame(0x90, 0x3A33F1, 31<<51|2<<13)), 0, AddrADSBICAONT, SourceADSB, "icao", false},
		{esHex(0x91, 0xADF9C1, identME(4, 1, "NONICAO")), 1, AddrADSBOther, SourceADSB, "non-icao", false},
		{esHex(0x91, 0x43E8E1, posME(11, 5000, false, 52.1, 5.2)), 1, AddrADSBOther, SourceADSB, "non-icao", false},
		{esHex(0x92, 0xA24531, velocityME(1, false, 0, true, 50, true, 70, true, false, 1, false, 32)), 2, AddrTISBICAO, SourceTISB, "icao", false},
		{cf2TC0, 2, AddrTISBTrackFile, SourceTISB, "tisb-track", true},
		{cf5Pos, 5, AddrTISBOther, SourceTISB, "anon", false},
		{esHex(0x96, 0xA0821B, identME(4, 1, "N905XT")), 6, AddrADSRICAO, SourceADSR, "icao", false},
		{cf6IMF, 6, AddrADSROther, SourceADSR, "non-icao", true},
		{esHex(0x97, 0xCAEEF1, 0x37FB1341BF58DF), 7, AddrUnknown, SourceUnknown, "non-icao", false},
		{esHex(0x97, 0x2AE8D1, 0xD73E298FCAA6BE), 7, AddrUnknown, SourceUnknown, "non-icao", false},
	} {
		m := mustDecode(t, c.frame)
		if m.DF != 18 || m.CF != c.cf || m.AddrType != c.addr || m.Source != c.src || m.IMF != c.imf {
			t.Errorf("%s: cf=%d addr=%v src=%v imf=%v", c.frame, m.CF, m.AddrType, m.Source, m.IMF)
		}
		if m.AddrType.Label() != c.label || m.AddrType.IsICAO() != (c.label == "icao") {
			t.Errorf("%s: label=%q", c.frame, m.AddrType.Label())
		}
		if m.Has(FCapability) || m.CRC != CRCOK || m.AddressFromParity {
			t.Errorf("%s: capability on DF18 or bad CRC state", c.frame)
		}
		if c.cf == 7 && m.Valid != 0 {
			t.Errorf("%s: reserved CF must not be decoded (valid=%b)", c.frame, m.Valid)
		}
	}

	// TIS-B relay (CF5) airborne position, 700 ft, odd, 70001/30002.
	m := mustDecode(t, cf5Pos)
	if m.TC != 13 || m.AltBaro != 700 || !m.CPROdd || m.CPRLat != 70001 || m.CPRLon != 30002 || m.AirGround != AGUncertain {
		t.Errorf("CF5 position: %+v", m)
	}
	// ADS-R with IMF, 2000 ft, odd, 68003/31004. Bit 8 is the IMF flag here,
	// not NIC supplement B.
	m = mustDecode(t, cf6IMF)
	if m.TC != 18 || m.AltBaro != 2000 || !m.CPROdd || m.CPRLat != 68003 || m.CPRLon != 31004 || m.Has(FNICSuppB) || m.ICAO != 0x130DA1 {
		t.Errorf("ADS-R position: %+v", m)
	}
	// TIS-B TC0 with IMF: altitude 3100 ft but no position.
	m = mustDecode(t, cf2TC0)
	if m.TC != 0 || m.Has(FCPR) || m.AltBaro != 3100 {
		t.Errorf("TIS-B TC0: %+v", m)
	}

	// IMF bit positions for every format, on fine TIS-B (CF2) and ADS-R (CF6).
	surface := uint64(7)<<51 | 41<<44
	tss := uint64(29)<<51 | 1<<49
	for name, c := range map[string]struct {
		me  uint64
		bit uint
	}{
		"airborne position": {airbornePosME(11, 0, false, ac12For(5000), false, false, 1, 2), 8},
		"surface position":  {surface, 21},
		"velocity":          {velocityME(1, false, 0, false, 101, false, 101, false, false, 1, false, 0), 9},
		"aircraft status":   {uint64(28)<<51 | 1<<48, 56},
		"target state":      {tss, 51},
		"operational":       {uint64(31)<<51 | 2<<13, 56},
	} {
		for _, first := range []byte{0x92, 0x96} {
			plain, err := DecodeFrame(esFrame(first, 0x123456, c.me))
			if err != nil {
				t.Fatal(err)
			}
			flagged, err := DecodeFrame(esFrame(first, 0x123456, setBits(c.me, c.bit, c.bit, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if plain.IMF || !plain.AddrType.IsICAO() {
				t.Errorf("%s first=%02X: IMF set without the bit", name, first)
			}
			wantType := AddrTISBTrackFile
			if first == 0x96 {
				wantType = AddrADSROther
			}
			if !flagged.IMF || flagged.AddrType != wantType || flagged.AddrKey() != 0x123456|NonICAOFlag {
				t.Errorf("%s first=%02X: IMF=%v addr=%v", name, first, flagged.IMF, flagged.AddrType)
			}
		}
		// The same bit in plain ADS-B (DF17) is not an IMF flag.
		adsb, err := DecodeFrame(esFrame(0x8D, 0x123456, setBits(c.me, c.bit, c.bit, 1)))
		if err != nil {
			t.Fatal(err)
		}
		if adsb.IMF || adsb.AddrType != AddrADSBICAO {
			t.Errorf("%s: DF17 treated bit %d as IMF", name, c.bit)
		}
	}
	// In TIS-B the surface "T" bit and the velocity "intent change" bit do
	// not exist.
	s, _ := DecodeFrame(esFrame(0x92, 0x123456, setBits(surface, 21, 21, 1)))
	v, _ := DecodeFrame(esFrame(0x92, 0x123456, setBits(velocityME(1, false, 0, false, 101, false, 101, false, false, 1, false, 0), 9, 9, 1)))
	if s.CPRTime || v.IntentChange {
		t.Error("IMF bit leaked into CPRTime / IntentChange")
	}

	// Coarse TIS-B (CF3): only the IMF bit is interpreted.
	c0, _ := DecodeFrame(esFrame(0x93, 0x123456, 0x7FFFFFFFFFFFFF))
	c1, _ := DecodeFrame(esFrame(0x93, 0x123456, 0x80000000000000))
	if c0.AddrType != AddrTISBICAO || c0.IMF || c0.Valid != 0 || c0.Source != SourceTISB {
		t.Errorf("coarse TIS-B without IMF: %+v", c0)
	}
	if c1.AddrType != AddrTISBTrackFile || !c1.IMF || c1.Valid != 0 {
		t.Errorf("coarse TIS-B with IMF: %+v", c1)
	}
	// CF4: TIS-B / ADS-R management, not decoded.
	c4, _ := DecodeFrame(esFrame(0x94, 0x123456, 0x202CC371C32CE0))
	if c4.AddrType != AddrUnknown || c4.Valid != 0 {
		t.Errorf("CF4: %+v", c4)
	}
}

func TestOtherTypeCodes(t *testing.T) {
	// TC0 "no position information", all zero.
	m := mustDecode(t, esHex(0x8D, 0xA90B11, 0))
	if m.TC != 0 || m.Has(FCPR) || m.Has(FAltBaro) || !m.Has(FTypeCode) {
		t.Errorf("TC0: valid=%b", m.Valid)
	}
	// TC23 (test message) subtype 6: nothing to decode.
	m = mustDecode(t, esHex(0x8D, 0x85D7A0, 23<<51|6<<48|0xAF5654B710D8))
	if m.TC != 23 || m.Has(FSquawk) {
		t.Errorf("TC23: TC=%d valid=%b", m.TC, m.Valid)
	}
	// TC23 subtype 7 carries a Mode A code.
	me := uint64(23)<<51 | 7<<48 | uint64(id13FromModeA(0x1200))<<35
	m, err := DecodeFrame(esFrame(0x8D, 0x123456, me))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Has(FSquawk) || m.SquawkString() != "1200" {
		t.Errorf("TC23/7: squawk %q", m.SquawkString())
	}
	// Reserved type codes only report the type code.
	for _, tc := range []uint64{24, 25, 26, 27, 30} {
		m, _ := DecodeFrame(esFrame(0x8D, 0x123456, tc<<51|0x7FFFFFFFFFFFF))
		if m.TC != uint8(tc) || m.Valid != FTypeCode|FCapability {
			t.Errorf("TC%d: valid=%b", tc, m.Valid)
		}
	}
}
