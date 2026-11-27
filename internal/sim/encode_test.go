package sim

import (
	"bytes"
	"encoding/hex"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cs8(s string) *[8]byte {
	var c [8]byte
	copy(c[:], s+"        ")
	return &c
}

func me56(v uint64) []byte {
	b := make([]byte, 7)
	put56(b, v)
	return b
}

// Published messages from "The 1090 Megahertz Riddle" (mode-s.org).
const (
	kaIdent   = "8D4840D6202CC371C32CE0576098" // KLM1023, TC 4, CA 0
	kaPosEven = "8D40621D58C382D690C8AC2863A7" // 38000 ft, CPR 93000 / 51372
	kaPosOdd  = "8D40621D58C386435CC412692AD6" // CPR 74158 / 50194
	kaVel     = "8D485020994409940838175B284F" // 159 kt, 182.88 deg, -832 ft/min
	kaSurfE   = "8C4841753AAB238733C8CD4020B1" // surface, even
	kaSurfO   = "8C4841753A8A35323FAEBDAC702D" // surface, odd (most recent)
	kaDF5     = "2A00516D492B80"               // squawk 0356
)

// The reference implementations must themselves agree with the published
// worked examples, otherwise they prove nothing about the encoder.
func TestReferenceDecoderAgainstPublishedExamples(t *testing.T) {
	for _, h := range []string{kaIdent, kaPosEven, kaPosOdd, kaVel, kaSurfE, kaSurfO} {
		if r := refRemainder(mustHex(t, h)); r != 0 {
			t.Errorf("%s: CRC remainder %06X, want 0", h, r)
		}
	}
	// a single flipped bit must be detected
	m := mustHex(t, kaIdent)
	m[5] ^= 0x04
	if refRemainder(m) == 0 {
		t.Error("corrupted message passes the reference CRC")
	}

	if cs := refCallsign(mustHex(t, kaIdent)[4:], 9); cs != "KLM1023" {
		t.Errorf("callsign %q", cs)
	}

	e, o := mustHex(t, kaPosEven)[4:11], mustHex(t, kaPosOdd)[4:11]
	if rb(e, 23, 39) != 93000 || rb(e, 40, 56) != 51372 || rb(o, 23, 39) != 74158 || rb(o, 40, 56) != 50194 {
		t.Fatal("CPR fields of the published position pair")
	}
	lat, lon, ok := refCPRAirborne(rb(e, 23, 39), rb(e, 40, 56), rb(o, 23, 39), rb(o, 40, 56), false)
	if !ok || math.Abs(lat-52.2572021484375) > 1e-9 || math.Abs(lon-3.91937255859375) > 1e-9 {
		t.Errorf("airborne CPR: %v %v %v", lat, lon, ok)
	}
	if alt, ok := refAlt12(rb(e, 9, 20)); !ok || alt != 38000 {
		t.Errorf("altitude %v %v", alt, ok)
	}

	v, ok := refVelocity(mustHex(t, kaVel)[4:11])
	if !ok || v.ew != -8 || v.ns != -159 || v.vr != -832 || v.vrBaro || !v.hasDelta || v.delta != 550 {
		t.Errorf("velocity %+v %v", v, ok)
	}

	se, so := mustHex(t, kaSurfE)[4:11], mustHex(t, kaSurfO)[4:11]
	lat, lon, ok = refCPRSurface(rb(se, 23, 39), rb(se, 40, 56), rb(so, 23, 39), rb(so, 40, 56), true, 51.990, 4.375)
	if !ok || math.Abs(lat-52.320607) > 2e-6 || math.Abs(lon-4.734735) > 2e-6 {
		t.Errorf("surface CPR: %v %v %v", lat, lon, ok)
	}

	if sq := refSquawk(rb(mustHex(t, kaDF5), 20, 32)); sq != "0356" {
		t.Errorf("squawk %q", sq)
	}
}

// The encoder must reproduce the published messages bit for bit, parity
// included.
func TestEncoderReproducesPublishedMessages(t *testing.T) {
	var msg [14]byte

	putES(msg[:], 17, 5, 0x4840D6, meIdent(4, 0, cs8("KLM1023")))
	if want := mustHex(t, kaIdent); !bytes.Equal(msg[:], want) {
		t.Errorf("identification\n got %X\nwant %X", msg, want)
	}

	yz, xz := cprEncodeAirborne(52.2572021484375, 3.91937255859375, false)
	if yz != 93000 || xz != 51372 {
		t.Errorf("even CPR = %d / %d, want 93000 / 51372", yz, xz)
	}
	putES(msg[:], 17, 5, 0x40621D, meAirbornePos(11, 0, false, encodeAC12(38000), false, false, yz, xz))
	if want := mustHex(t, kaPosEven); !bytes.Equal(msg[:], want) {
		t.Errorf("airborne position\n got %X\nwant %X", msg, want)
	}
	if yz, _ := cprEncodeAirborne(52.26578017412606, 3.93, true); yz != 74158 {
		t.Errorf("odd CPR latitude = %d, want 74158", yz)
	}

	// The published velocity message has the IFR capability flag (ME bit 10)
	// set, which the simulator never does: add it before comparing.
	me := meVelocity(0, -8, -159, false, -832, 550) | 1<<46
	putES(msg[:], 17, 5, 0x485020, me)
	if want := mustHex(t, kaVel); !bytes.Equal(msg[:], want) {
		t.Errorf("velocity\n got %X\nwant %X", msg, want)
	}

	// squawk 0356: identical to the published DF5 identity field apart from
	// the unused X bit, which that transponder happens to set
	if got, want := encodeID13(0x0356), uint16(rb(mustHex(t, kaDF5), 20, 32))&^0x40; got != want {
		t.Errorf("identity code %013b, want %013b", got, want)
	}
}

func TestCRCParity(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		icao := r.Uint32N(1 << 24)
		var long [14]byte
		var short [7]byte
		for i := range long {
			long[i] = byte(r.UintN(256))
		}
		copy(short[:], long[:])

		// PI parity (II = 0): the whole message divides evenly
		seal(long[:], 0)
		seal(short[:], 0)
		if refRemainder(long[:]) != 0 || refRemainder(short[:]) != 0 {
			t.Fatalf("PI parity wrong: %X / %X", long, short)
		}
		// the table CRC equals the bit-serial one
		if got, want := crc24(long[:11]), uint32(rb(long[:], 89, 112)); got != want {
			t.Fatalf("crc24 %06X != transmitted %06X", got, want)
		}
		// AP parity: the remainder is the address
		seal(long[:], icao)
		seal(short[:], icao)
		if a, b := refRemainder(long[:]), refRemainder(short[:]); a != icao || b != icao {
			t.Fatalf("AP parity: recovered %06X / %06X, want %06X", a, b, icao)
		}
		// any single bit error is detected
		bit := r.IntN(112)
		long[bit/8] ^= 0x80 >> (bit % 8)
		if refRemainder(long[:]) == icao {
			t.Fatalf("bit error %d not detected", bit)
		}
	}
}

func TestSurveillanceReplies(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 3000 {
		icao := 1 + r.Uint32N(1<<24-1)
		alt := float64(r.IntN(2048))*25 - 1000
		sq := uint16(r.IntN(8))<<12 | uint16(r.IntN(8))<<8 | uint16(r.IntN(8))<<4 | uint16(r.IntN(8))
		fs := uint8(r.IntN(6))
		mb := r.Uint64() & (1<<56 - 1)

		var m7 [7]byte
		putSurvShort(m7[:], 4, fs, encodeAC13(alt), icao)
		if refDF(m7[:]) != 4 || refRemainder(m7[:]) != icao || rb(m7[:], 6, 8) != uint64(fs) || rb(m7[:], 9, 19) != 0 {
			t.Fatalf("DF4 header/parity: %X", m7)
		}
		if got, ok := refAlt13(rb(m7[:], 20, 32)); !ok || got != alt {
			t.Fatalf("DF4 altitude %v, want %v", got, alt)
		}

		putSurvShort(m7[:], 5, fs, encodeID13(sq), icao)
		if refDF(m7[:]) != 5 || refRemainder(m7[:]) != icao {
			t.Fatalf("DF5 header/parity: %X", m7)
		}
		if got := refSquawk(rb(m7[:], 20, 32)); got != squawkString(sq) {
			t.Fatalf("DF5 squawk %s, want %s", got, squawkString(sq))
		}

		var m14 [14]byte
		putSurvLong(m14[:], 20, fs, encodeAC13(alt), mb, icao)
		if refDF(m14[:]) != 20 || refRemainder(m14[:]) != icao || rb(m14[:], 33, 88) != mb {
			t.Fatalf("DF20: %X", m14)
		}
		if got, ok := refAlt13(rb(m14[:], 20, 32)); !ok || got != alt {
			t.Fatalf("DF20 altitude %v, want %v", got, alt)
		}
		putSurvLong(m14[:], 21, fs, encodeID13(sq), mb, icao)
		if refDF(m14[:]) != 21 || refRemainder(m14[:]) != icao || rb(m14[:], 33, 88) != mb ||
			refSquawk(rb(m14[:], 20, 32)) != squawkString(sq) {
			t.Fatalf("DF21: %X", m14)
		}

		vs, sl, ri := r.IntN(2) == 1, uint8(r.IntN(8)), uint8(r.IntN(16))
		putDF0(m7[:], vs, true, sl, ri, encodeAC13(alt), icao)
		if refDF(m7[:]) != 0 || refRemainder(m7[:]) != icao || (rb(m7[:], 6, 6) == 1) != vs || rb(m7[:], 7, 7) != 1 ||
			rb(m7[:], 9, 11) != uint64(sl) || rb(m7[:], 14, 17) != uint64(ri) ||
			rb(m7[:], 8, 8) != 0 || rb(m7[:], 12, 13) != 0 || rb(m7[:], 18, 19) != 0 {
			t.Fatalf("DF0: %X", m7)
		}
		if got, ok := refAlt13(rb(m7[:], 20, 32)); !ok || got != alt {
			t.Fatalf("DF0 altitude %v, want %v", got, alt)
		}

		putDF11(m7[:], 5, icao)
		if refDF(m7[:]) != 11 || rb(m7[:], 6, 8) != 5 || refICAO(m7[:]) != icao || refRemainder(m7[:]) != 0 {
			t.Fatalf("DF11: %X", m7)
		}
	}
}

func TestAltitudeAndIdentityCodes(t *testing.T) {
	for n := 0; n < 2048; n++ {
		ft := float64(n)*25 - 1000
		// any altitude within half a step encodes to the same code
		for _, d := range []float64{-12.4, 0, 12.4} {
			if got, ok := refAlt12(uint64(encodeAC12(ft + d))); !ok || got != ft {
				t.Fatalf("AC12(%v) decodes to %v", ft+d, got)
			}
			if got, ok := refAlt13(uint64(encodeAC13(ft + d))); !ok || got != ft {
				t.Fatalf("AC13(%v) decodes to %v", ft+d, got)
			}
		}
	}
	if encodeAC12(38000) != 0xC38 {
		t.Errorf("AC12(38000) = %03X, want C38", encodeAC12(38000))
	}
	// out of range altitudes saturate instead of wrapping
	if a, _ := refAlt12(uint64(encodeAC12(-5000))); a != -1000 {
		t.Errorf("low clamp %v", a)
	}
	if a, _ := refAlt12(uint64(encodeAC12(99000))); a != 50175 {
		t.Errorf("high clamp %v", a)
	}
	for a := 0; a < 8; a++ {
		for b := 0; b < 8; b++ {
			for c := 0; c < 8; c++ {
				for d := 0; d < 8; d++ {
					sq := uint16(a<<12 | b<<8 | c<<4 | d)
					id := encodeID13(sq)
					want := string([]byte{'0' + byte(a), '0' + byte(b), '0' + byte(c), '0' + byte(d)})
					if id&0x40 != 0 || id > 0x1FFF || refSquawk(uint64(id)) != want {
						t.Fatalf("squawk %s -> %013b -> %s", want, id, refSquawk(uint64(id)))
					}
				}
			}
		}
	}
}

func TestIdentificationMessage(t *testing.T) {
	for _, cs := range []string{"BAW117", "EZY82KM", "N512PA", "DEKLM", "A", "RYR4TK", "NATO01", "LIFE7"} {
		me := me56(meIdent(4, 3, cs8(cs)))
		if rb(me, 1, 5) != 4 || rb(me, 6, 8) != 3 || refCallsign(me, 9) != cs {
			t.Errorf("TC4 %q -> %q", cs, refCallsign(me, 9))
		}
		mb := me56(mbBDS20(cs8(cs)))
		if rb(mb, 1, 8) != 0x20 || refCallsign(mb, 9) != cs {
			t.Errorf("BDS 2,0 %q -> %q", cs, refCallsign(mb, 9))
		}
	}
	// characters outside the alphabet become spaces, never garbage
	if got := refCallsign(me56(meIdent(4, 1, cs8("ab-1"))), 9); strings.ContainsRune(got, '#') {
		t.Errorf("invalid characters leak into the callsign: %q", got)
	}
}

func TestVelocityMessage(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for range 5000 {
		ew := (r.Float64()*2 - 1) * 600
		ns := (r.Float64()*2 - 1) * 600
		vr := (r.Float64()*2 - 1) * 6000
		delta := (r.Float64()*2 - 1) * 1500
		baro := r.IntN(2) == 1
		nacv := uint8(r.IntN(5))
		me := me56(meVelocity(nacv, ew, ns, baro, vr, delta))
		v, ok := refVelocity(me)
		if !ok {
			t.Fatalf("not a TC19/1 message: %X", me)
		}
		if math.Abs(v.ew-ew) > 0.5 || math.Abs(v.ns-ns) > 0.5 || math.Abs(v.vr-vr) > 32 ||
			!v.hasDelta || math.Abs(v.delta-delta) > 12.5 || v.vrBaro != baro || v.nacv != int(nacv) {
			t.Fatalf("velocity (%.1f %.1f %.0f %.0f) decoded as %+v", ew, ns, vr, delta, v)
		}
		if rb(me, 9, 10) != 0 || rb(me, 47, 48) != 0 {
			t.Fatalf("reserved bits set: %X", me)
		}
	}
	// zero velocity is "0 kt", not "no information"
	v, ok := refVelocity(me56(meVelocity(2, 0, 0, true, 0, math.NaN())))
	if !ok || v.ew != 0 || v.ns != 0 || v.vr != 0 || v.hasDelta {
		t.Errorf("zero velocity: %+v %v", v, ok)
	}
	// saturation instead of overflow
	v, _ = refVelocity(me56(meVelocity(2, 5000, -5000, true, 99999, 9999)))
	if v.ew != 1022 || v.ns != -1022 || v.vr != 510*64 || v.delta != 126*25 {
		t.Errorf("saturation: %+v", v)
	}
}

func TestPositionMessages(t *testing.T) {
	me := me56(meAirbornePos(13, 2, true, encodeAC12(12350), false, true, 0x1ABCD, 0x05432))
	if rb(me, 1, 5) != 13 || rb(me, 6, 7) != 2 || rb(me, 8, 8) != 1 || rb(me, 21, 21) != 0 || rb(me, 22, 22) != 1 ||
		rb(me, 23, 39) != 0x1ABCD || rb(me, 40, 56) != 0x05432 {
		t.Errorf("airborne position fields: %X", me)
	}
	if alt, ok := refAlt12(rb(me, 9, 20)); !ok || alt != 12350 {
		t.Errorf("altitude %v", alt)
	}

	for _, trk := range []float64{0, 1.3, 92.8125, 179.9, 270, 359.9} {
		for _, gs := range []float64{0, 0.1, 0.5, 1.4, 7.3, 17, 44.4, 85, 133, 174.9, 180, 250} {
			me := me56(meSurfacePos(7, encodeMovement(gs), true, trk, false, 1, 2))
			if rb(me, 1, 5) != 7 || rb(me, 13, 13) != 1 || rb(me, 21, 22) != 0 || rb(me, 23, 39) != 1 || rb(me, 40, 56) != 2 {
				t.Fatalf("surface position fields: %X", me)
			}
			lo, hi, ok := refMovement(rb(me, 6, 12))
			if !ok || gs < lo || gs >= hi {
				t.Errorf("movement code %d for %v kt covers [%v, %v)", rb(me, 6, 12), gs, lo, hi)
			}
			if got := float64(rb(me, 14, 20)) * 360 / 128; angDiff(got, trk) > 360.0/128/2+1e-9 {
				t.Errorf("ground track %v decoded as %v", trk, got)
			}
		}
	}
	// the published example: movement 41 = 17 kt, track 33 = 92.8125 deg
	if encodeMovement(17) != 41 {
		t.Errorf("movement(17 kt) = %d, want 41", encodeMovement(17))
	}
	if me := me56(meSurfacePos(7, 41, true, 92.8125, false, 0, 0)); rb(me, 14, 20) != 33 {
		t.Errorf("track 92.8125 encoded as %d, want 33", rb(me, 14, 20))
	}
}

func TestCPRRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	var worstAir, worstSurf float64
	skipped := 0
	const n = 200000
	for i := 0; i < n; i++ {
		lat := (r.Float64()*2 - 1) * 86.9
		lon := (r.Float64()*2 - 1) * 180
		switch i % 50 { // exercise the awkward spots too
		case 0:
			lat = 0
		case 1:
			lon = -180
		case 2:
			lat, lon = -lat, 179.99999
		case 3:
			lon = 0
		}
		ye, xe := cprEncodeAirborne(lat, lon, false)
		yo, xo := cprEncodeAirborne(lat, lon, true)
		if ye > 0x1FFFF || xe > 0x1FFFF || yo > 0x1FFFF || xo > 0x1FFFF {
			t.Fatalf("CPR value exceeds 17 bits at %v %v", lat, lon)
		}
		for _, odd := range []bool{false, true} {
			dl, dn, ok := refCPRAirborne(uint64(ye), uint64(xe), uint64(yo), uint64(xo), odd)
			if !ok {
				skipped++ // the two formats quantise to different sides of a zone boundary
				continue
			}
			d := refDistM(lat, lon, dl, dn)
			if d > worstAir {
				worstAir = d
			}
			if d > 10 {
				t.Fatalf("airborne CPR %v,%v decodes %.1f m away (%v,%v)", lat, lon, d, dl, dn)
			}
		}

		// surface: the receiver is within a few hundred km of the aircraft
		refLat := clamp(lat+(r.Float64()*2-1)*3, -89, 89)
		refLon := lon + (r.Float64()*2-1)*3
		ye, xe = cprEncodeSurface(lat, lon, false)
		yo, xo = cprEncodeSurface(lat, lon, true)
		if ye > 0x1FFFF || xe > 0x1FFFF || yo > 0x1FFFF || xo > 0x1FFFF {
			t.Fatalf("surface CPR value exceeds 17 bits at %v %v", lat, lon)
		}
		for _, odd := range []bool{false, true} {
			dl, dn, ok := refCPRSurface(uint64(ye), uint64(xe), uint64(yo), uint64(xo), odd, refLat, refLon)
			if !ok {
				skipped++
				continue
			}
			d := refDistM(lat, lon, dl, dn)
			if d > worstSurf {
				worstSurf = d
			}
			if d > 3 {
				t.Fatalf("surface CPR %v,%v (ref %v,%v) decodes %.1f m away (%v,%v)", lat, lon, refLat, refLon, d, dl, dn)
			}
		}
	}
	if skipped > n/100 {
		t.Errorf("%d of %d pairs undecodable", skipped, 4*n)
	}
	t.Logf("worst CPR error: airborne %.2f m, surface %.2f m; %d zone-boundary pairs skipped", worstAir, worstSurf, skipped)
}

func TestCommBRegisters(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	for range 5000 {
		roll := (r.Float64()*2 - 1) * 60
		trk := r.Float64() * 360
		gs := r.Float64() * 600
		rate := (r.Float64()*2 - 1) * 15
		tas := r.Float64() * 600
		d50, ok := refDecode50(me56(mbBDS50(roll, trk, gs, rate, tas)))
		if !ok || math.Abs(d50.roll-roll) > 45.0/512+1e-9 || angDiff(d50.track, trk) > 90.0/1024+1e-9 ||
			math.Abs(d50.gs-gs) > 1 || math.Abs(d50.trackRate-rate) > 8.0/512+1e-9 || math.Abs(d50.tas-tas) > 1 {
			t.Fatalf("BDS 5,0 (%v %v %v %v %v) -> %+v %v", roll, trk, gs, rate, tas, d50, ok)
		}

		hdg := r.Float64() * 360
		ias := r.Float64() * 400
		mach := r.Float64() * 0.95
		vr := (r.Float64()*2 - 1) * 8000
		d60, ok := refDecode60(me56(mbBDS60(hdg, ias, mach, vr, vr+100)))
		if !ok || angDiff(d60.hdg, hdg) > 90.0/1024+1e-9 || math.Abs(d60.ias-ias) > 0.5 || math.Abs(d60.mach-mach) > 0.002+1e-9 ||
			math.Abs(d60.baroRate-vr) > 16 || math.Abs(d60.inertialRate-(vr+100)) > 16 {
			t.Fatalf("BDS 6,0 (%v %v %v %v) -> %+v %v", hdg, ias, mach, vr, d60, ok)
		}

		sel := 100 * math.Round(r.Float64()*450)
		baro := 950 + r.Float64()*100
		vnav, hold, app := r.IntN(2) == 1, r.IntN(2) == 1, r.IntN(2) == 1
		d40, ok := refDecode40(me56(mbBDS40(sel, 0, baro, vnav, hold, app)))
		if sel > 0 {
			if !ok || math.Abs(d40.mcpAlt-sel) > 8 || math.Abs(d40.baro-baro) > 0.05+1e-9 ||
				d40.vnav != vnav || d40.altHold != hold || d40.approach != app || d40.source != 2 {
				t.Fatalf("BDS 4,0 (%v %v) -> %+v %v", sel, baro, d40, ok)
			}
		}

		wind := r.Float64() * 150
		dir := r.Float64() * 360
		sat := -70 + r.Float64()*100
		press := 150 + r.Float64()*870
		d44, ok := refDecode44(me56(mbBDS44(wind, dir, sat, press)))
		if !ok || math.Abs(d44.windKt-wind) > 0.5 || angDiff(d44.windDir, dir) > 180.0/512+1e-9 ||
			math.Abs(d44.sat-sat) > 0.125+1e-9 || math.Abs(d44.press-press) > 0.5 {
			t.Fatalf("BDS 4,4 (%v %v %v %v) -> %+v %v", wind, dir, sat, press, d44, ok)
		}
	}
	// the four registers must be distinguishable by their leading bits
	if m := me56(mbBDS20(cs8("TEST"))); m[0] != 0x20 {
		t.Errorf("BDS 2,0 does not start with 0x20")
	}
	if m := me56(mbBDS44(10, 20, -50, 300)); m[0]>>4 != 1 {
		t.Errorf("BDS 4,4 FOM/source nibble = %d", m[0]>>4)
	}
}

func TestTargetStateMessage(t *testing.T) {
	f := tssFields{
		selAltFt: 36000, baroHPa: 1013.2, selHdgDeg: 271.4, nacp: 9, sil: 3, nicBaro: true,
		modeBits: true, autopilot: true, vnav: false, altHold: true, approach: false, tcas: true, lnav: true,
	}
	me := me56(meTargetState(&f))
	// field positions as decoded by dump1090 (decodeESTargetStatus, version 2)
	if rb(me, 1, 5) != 29 || rb(me, 6, 7) != 1 || rb(me, 8, 8) != 0 || rb(me, 9, 9) != 0 {
		t.Fatalf("header: %X", me)
	}
	if alt := float64(rb(me, 10, 20)-1) * 32; math.Abs(alt-36000) > 16 {
		t.Errorf("selected altitude %v", alt)
	}
	if baro := 800 + float64(rb(me, 21, 29)-1)*0.8; math.Abs(baro-1013.2) > 0.4+1e-9 {
		t.Errorf("baro setting %v", baro)
	}
	if rb(me, 30, 30) != 1 {
		t.Errorf("heading status")
	}
	if hdg := float64(rb(me, 31, 39)) * 180 / 256; angDiff(hdg, 271.4) > 180.0/512+1e-9 {
		t.Errorf("selected heading %v", hdg)
	}
	if rb(me, 40, 43) != 9 || rb(me, 44, 44) != 1 || rb(me, 45, 46) != 3 {
		t.Errorf("NACp / NICbaro / SIL: %X", me)
	}
	// mode status, autopilot, VNAV, altitude hold, IMF, approach, TCAS, LNAV
	if got := rb(me, 47, 54); got != 0b11010011 {
		t.Errorf("mode bits %08b", got)
	}
	if rb(me, 55, 56) != 0 {
		t.Errorf("reserved bits")
	}

	// "not available" encodings and saturation
	me = me56(meTargetState(&tssFields{selHdgDeg: math.NaN()}))
	if rb(me, 10, 20) != 0 || rb(me, 21, 29) != 0 || rb(me, 30, 39) != 0 || rb(me, 47, 54) != 0 {
		t.Errorf("empty target state: %X", me)
	}
	me = me56(meTargetState(&tssFields{selAltFt: 1e6, baroHPa: 5000, selHdgDeg: 359.99}))
	if rb(me, 10, 20) != 2047 || rb(me, 21, 29) != 511 || rb(me, 31, 39) != 0 {
		t.Errorf("saturation: alt %d baro %d hdg %d", rb(me, 10, 20), rb(me, 21, 29), rb(me, 31, 39))
	}
}

func TestOperationalStatusMessage(t *testing.T) {
	air := me56(meOpStatus(&opFields{acas: true, sda: 2, nacp: 10, nacv: 2, gva: 2, sil: 3, nicBaro: true}))
	// field positions as decoded by dump1090 (decodeESOperationalStatus, version 2)
	if rb(air, 1, 5) != 31 || rb(air, 6, 8) != 0 {
		t.Fatalf("header: %X", air)
	}
	if rb(air, 9, 10) != 0 || rb(air, 11, 11) != 1 || rb(air, 12, 12) != 0 || rb(air, 15, 15) != 1 || rb(air, 16, 16) != 1 {
		t.Errorf("airborne capability class: %016b", rb(air, 9, 24))
	}
	if rb(air, 25, 26) != 0 || rb(air, 30, 30) != 0 || rb(air, 31, 32) != 2 {
		t.Errorf("operational mode: %016b", rb(air, 25, 40))
	}
	if rb(air, 41, 43) != 2 || rb(air, 44, 44) != 0 || rb(air, 45, 48) != 10 || rb(air, 49, 50) != 2 ||
		rb(air, 51, 52) != 3 || rb(air, 53, 53) != 1 || rb(air, 54, 56) != 0 {
		t.Errorf("version / accuracy fields: %X", air)
	}

	surf := me56(meOpStatus(&opFields{surface: true, singleAntenna: true, sda: 1, nacp: 8, nacv: 1, sil: 2, lenWidth: 12}))
	if rb(surf, 1, 5) != 31 || rb(surf, 6, 8) != 1 {
		t.Fatalf("header: %X", surf)
	}
	if rb(surf, 9, 10) != 0 || rb(surf, 17, 19) != 1 || rb(surf, 20, 20) != 0 || rb(surf, 21, 24) != 12 {
		t.Errorf("surface capability class: %016b", rb(surf, 9, 24))
	}
	if rb(surf, 30, 30) != 1 || rb(surf, 31, 32) != 1 || rb(surf, 41, 43) != 2 || rb(surf, 45, 48) != 8 ||
		rb(surf, 49, 50) != 0 || rb(surf, 51, 52) != 2 {
		t.Errorf("surface status fields: %X", surf)
	}
}

func TestEmergencyMessage(t *testing.T) {
	me := me56(meEmergency(1, encodeID13(0x7700)))
	if rb(me, 1, 5) != 28 || rb(me, 6, 8) != 1 || rb(me, 9, 11) != 1 || refSquawk(rb(me, 12, 24)) != "7700" || rb(me, 25, 56) != 0 {
		t.Errorf("TC28: %X", me)
	}
}
