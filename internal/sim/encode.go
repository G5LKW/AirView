package sim

import "math"

// This file is the simulator's Mode S / ADS-B *encoder*. It is deliberately
// independent of internal/modes (the decoder) so the two can cross-check each
// other in end-to-end tests. Bit numbering in comments follows the ICAO
// convention: bit 1 is the most significant bit of the field.

// crcTable is the byte-wise table for the Mode S CRC-24, generator polynomial
// x^24+x^23+...  = 0x1FFF409 (the 24 low bits are 0xFFF409).
var crcTable [256]uint32

func init() {
	for i := range crcTable {
		c := uint32(i) << 16
		for range 8 {
			if c&0x800000 != 0 {
				c = (c << 1) ^ 0xFFF409
			} else {
				c <<= 1
			}
		}
		crcTable[i] = c & 0xFFFFFF
	}
}

// crc24 returns the Mode S parity of data (all message bytes except the last
// three). For DF11 (II=0) / DF17 / DF18 this value is transmitted as-is; for
// DF0/4/5/16/20/21 it is XORed with the aircraft address.
func crc24(data []byte) uint32 {
	var c uint32
	for _, b := range data {
		c = ((c << 8) & 0xFFFFFF) ^ crcTable[byte(c>>16)^b]
	}
	return c
}

// seal writes the 24 parity bits into the last three bytes of msg. overlay is
// XORed onto the CRC (0 for PI with II=0, the ICAO address for AP).
func seal(msg []byte, overlay uint32) {
	n := len(msg)
	p := crc24(msg[:n-3]) ^ (overlay & 0xFFFFFF)
	msg[n-3] = byte(p >> 16)
	msg[n-2] = byte(p >> 8)
	msg[n-1] = byte(p)
}

// bits packs fields MSB-first into a 56-bit ME / MB field.
type bits struct{ v uint64 }

func (b *bits) add(n uint, val uint64) {
	b.v = b.v<<n | val&(1<<n-1)
}

func b2u(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

// twos encodes x/lsb as an n-bit two's complement integer (sign bit followed
// by n-1 magnitude bits, which is how Comm-B "sign + value" fields are laid out).
func twos(x, lsb float64, n uint) uint64 {
	v := math.Round(x / lsb)
	lim := float64(int64(1) << (n - 1))
	if v < -lim {
		v = -lim
	}
	if v > lim-1 {
		v = lim - 1
	}
	return uint64(int64(v)) & (1<<n - 1)
}

// angle encodes an angle (degrees) as an n-bit integer with the given LSB.
// The field spans a full circle, so values wrap instead of saturating: this
// serves both the two's complement +-180 degree fields and the unsigned
// 0..360 degree ones.
func angle(deg, lsb float64, n uint) uint64 {
	return uint64(int64(math.Round(deg/lsb))) & (1<<n - 1)
}

// uq encodes x/lsb as an unsigned n-bit integer, clamped.
func uq(x, lsb float64, n uint) uint64 {
	v := math.Round(x / lsb)
	if v < 0 {
		v = 0
	}
	if lim := float64(uint64(1)<<n - 1); v > lim {
		v = lim
	}
	return uint64(v)
}

// putES writes a 112-bit extended squitter (DF17 with CA, or DF18 with CF) and
// its PI parity into msg[0:14].
func putES(msg []byte, df, ca uint8, icao uint32, me uint64) {
	msg[0] = df<<3 | ca&7
	msg[1] = byte(icao >> 16)
	msg[2] = byte(icao >> 8)
	msg[3] = byte(icao)
	put56(msg[4:11], me)
	seal(msg[:14], 0)
}

func put56(dst []byte, v uint64) {
	dst[0] = byte(v >> 48)
	dst[1] = byte(v >> 40)
	dst[2] = byte(v >> 32)
	dst[3] = byte(v >> 24)
	dst[4] = byte(v >> 16)
	dst[5] = byte(v >> 8)
	dst[6] = byte(v)
}

func put32(dst []byte, v uint32) {
	dst[0] = byte(v >> 24)
	dst[1] = byte(v >> 16)
	dst[2] = byte(v >> 8)
	dst[3] = byte(v)
}

// putDF11 writes an all-call reply / acquisition squitter (56 bit) with
// interrogator code 0, so PI is the plain CRC.
func putDF11(msg []byte, ca uint8, icao uint32) {
	msg[0] = 11<<3 | ca&7
	msg[1] = byte(icao >> 16)
	msg[2] = byte(icao >> 8)
	msg[3] = byte(icao)
	seal(msg[:7], 0)
}

// survWord builds the first 32 bits shared by DF4/5/20/21:
// DF(5) FS(3) DR(5) UM(6) AC-or-ID(13).
func survWord(df, fs, dr, um uint8, field13 uint16) uint32 {
	return uint32(df)<<27 | uint32(fs&7)<<24 | uint32(dr&31)<<19 | uint32(um&63)<<13 | uint32(field13&0x1FFF)
}

// putSurvShort writes DF4 (altitude) or DF5 (identity) with address/parity.
func putSurvShort(msg []byte, df, fs uint8, field13 uint16, icao uint32) {
	put32(msg[0:4], survWord(df, fs, 0, 0, field13))
	seal(msg[:7], icao)
}

// putSurvLong writes DF20 (altitude) or DF21 (identity) with a Comm-B MB field.
func putSurvLong(msg []byte, df, fs uint8, field13 uint16, mb uint64, icao uint32) {
	put32(msg[0:4], survWord(df, fs, 0, 0, field13))
	put56(msg[4:11], mb)
	seal(msg[:14], icao)
}

// putDF0 writes a short air-air surveillance (ACAS) reply:
// DF(5) VS(1) CC(1) -(1) SL(3) -(2) RI(4) -(2) AC(13) AP(24).
func putDF0(msg []byte, vs bool, cc bool, sl, ri uint8, ac13 uint16, icao uint32) {
	w := uint32(b2u(vs))<<26 | uint32(b2u(cc))<<25 | uint32(sl&7)<<21 | uint32(ri&15)<<15 | uint32(ac13&0x1FFF)
	put32(msg[0:4], w)
	seal(msg[:7], icao)
}

// altN converts feet to the 11-bit 25 ft code N (altitude = 25*N - 1000).
func altN(ft float64) uint16 {
	n := math.Round((ft + 1000) / 25)
	if n < 0 {
		n = 0
	}
	if n > 2047 {
		n = 2047
	}
	return uint16(n)
}

// encodeAC13 builds the 13-bit altitude code of DF0/4/16/20 (M=0, Q=1).
func encodeAC13(ft float64) uint16 {
	n := altN(ft)
	return (n&0x7E0)<<2 | (n&0x010)<<1 | 0x010 | n&0x00F
}

// encodeAC12 builds the 12-bit altitude code of the extended squitter airborne
// position message (Q=1, 25 ft increments).
func encodeAC12(ft float64) uint16 {
	n := altN(ft)
	return (n&0x7F0)<<1 | 0x010 | n&0x00F
}

// encodeID13 builds the 13-bit identity field C1 A1 C2 A2 C4 A4 X B1 D1 B2 D2 B4 D4
// from a squawk held as four octal digits in nibbles (0xABCD).
func encodeID13(sq uint16) uint16 {
	a := sq >> 12 & 7
	b := sq >> 8 & 7
	c := sq >> 4 & 7
	d := sq & 7
	var v uint16
	v |= (c & 1) << 12      // C1
	v |= (a & 1) << 11      // A1
	v |= (c >> 1 & 1) << 10 // C2
	v |= (a >> 1 & 1) << 9  // A2
	v |= (c >> 2 & 1) << 8  // C4
	v |= (a >> 2 & 1) << 7  // A4
	v |= (b & 1) << 5       // B1
	v |= (d & 1) << 4       // D1
	v |= (b >> 1 & 1) << 3  // B2
	v |= (d >> 1 & 1) << 2  // D2
	v |= (b >> 2 & 1) << 1  // B4
	v |= (d >> 2 & 1)       // D4
	return v
}

// encodeMovement quantises a surface ground speed (kt) to the 7-bit movement
// field of the surface position message.
func encodeMovement(gs float64) uint8 {
	switch {
	case gs < 0.125:
		return 1 // stopped
	case gs < 1:
		return uint8(2 + int((gs-0.125)/0.125))
	case gs < 2:
		return uint8(9 + int((gs-1)/0.25))
	case gs < 15:
		return uint8(13 + int((gs-2)/0.5))
	case gs < 70:
		return uint8(39 + int(gs-15))
	case gs < 100:
		return uint8(94 + int((gs-70)/2))
	case gs < 175:
		return uint8(109 + int((gs-100)/5))
	default:
		return 124
	}
}

// sixBit maps an ASCII character to the 6-bit aircraft identification alphabet.
func sixBit(c byte) uint64 {
	switch {
	case c >= 'A' && c <= 'Z':
		return uint64(c-'A') + 1
	case c >= '0' && c <= '9':
		return uint64(c)
	default:
		return 32 // space
	}
}

// meIdent builds the aircraft identification and category message (TC 1-4).
// tc selects the category set (4 = A, 3 = B, 2 = C, 1 = D), cat the category.
func meIdent(tc, cat uint8, cs *[8]byte) uint64 {
	var b bits
	b.add(5, uint64(tc))
	b.add(3, uint64(cat))
	for _, c := range cs {
		b.add(6, sixBit(c))
	}
	return b.v
}

// meAirbornePos builds an airborne position message (TC 9-18 baro, 20-22 GNSS).
// utc sets the TIME (T) bit: the position applies to the 0.2 s UTC epoch
// whose parity the CPR format bit (odd) gives.
func meAirbornePos(tc, ss uint8, nicSB bool, ac12 uint16, utc, odd bool, latCPR, lonCPR uint32) uint64 {
	var b bits
	b.add(5, uint64(tc))
	b.add(2, uint64(ss))
	b.add(1, b2u(nicSB))
	b.add(12, uint64(ac12))
	b.add(1, b2u(utc)) // T: time of applicability synchronised to UTC
	b.add(1, b2u(odd))
	b.add(17, uint64(latCPR))
	b.add(17, uint64(lonCPR))
	return b.v
}

// meSurfacePos builds a surface position message (TC 5-8).
func meSurfacePos(tc, movement uint8, trkValid bool, trkDeg float64, odd bool, latCPR, lonCPR uint32) uint64 {
	var b bits
	b.add(5, uint64(tc))
	b.add(7, uint64(movement))
	b.add(1, b2u(trkValid))
	b.add(7, angle(wrap360(trkDeg), 360.0/128, 7))
	b.add(1, 0) // T
	b.add(1, b2u(odd))
	b.add(17, uint64(latCPR))
	b.add(17, uint64(lonCPR))
	return b.v
}

// velComp encodes one velocity component of the TC19 subtype 1 message as a
// direction bit plus a 10-bit magnitude (value+1, 0 = not available).
func velComp(v float64) (dir, mag uint64) {
	if v < 0 {
		dir = 1
		v = -v
	}
	m := math.Round(v) + 1
	if m > 1023 {
		m = 1023
	}
	return dir, uint64(m)
}

// meVelocity builds an airborne velocity message, TC19 subtype 1 (ground speed,
// subsonic). ewKt is positive east, nsKt positive north. vrBaro selects the
// vertical-rate source bit. gnssMinusBaroFt may be NaN for "no information".
func meVelocity(nacv uint8, ewKt, nsKt float64, vrBaro bool, vrFpm, gnssMinusBaroFt float64) uint64 {
	var b bits
	b.add(5, 19)
	b.add(3, 1)
	b.add(1, 0) // intent change
	b.add(1, 0) // reserved-A (IFR capability in v0)
	b.add(3, uint64(nacv))
	d, m := velComp(ewKt) // 1 = west
	b.add(1, d)
	b.add(10, m)
	d, m = velComp(nsKt) // 1 = south
	b.add(1, d)
	b.add(10, m)
	b.add(1, b2u(vrBaro))
	var sign uint64
	vr := vrFpm
	if vr < 0 {
		sign = 1
		vr = -vr
	}
	r := math.Round(vr/64) + 1
	if r > 511 {
		r = 511
	}
	b.add(1, sign)
	b.add(9, uint64(r))
	b.add(2, 0)
	if math.IsNaN(gnssMinusBaroFt) {
		b.add(8, 0)
	} else {
		sign = 0
		df := gnssMinusBaroFt
		if df < 0 {
			sign = 1 // GNSS below baro
			df = -df
		}
		q := math.Round(df/25) + 1
		if q > 127 {
			q = 127
		}
		b.add(1, sign)
		b.add(7, uint64(q))
	}
	return b.v
}

// meEmergency builds the aircraft status message TC28 subtype 1
// (emergency / priority status and Mode A code).
func meEmergency(state uint8, id13 uint16) uint64 {
	var b bits
	b.add(5, 28)
	b.add(3, 1)
	b.add(3, uint64(state))
	b.add(13, uint64(id13))
	b.add(32, 0)
	return b.v
}

// tssFields is the content of a target state and status message.
type tssFields struct {
	selAltFt             float64 // MCP/FCU selected altitude; <= 0 = not available
	fms                  bool    // selected altitude is from the FMS
	baroHPa              float64 // <= 0 = not available
	selHdgDeg            float64 // NaN = not available
	nacp, sil            uint8
	nicBaro              bool
	autopilot, vnav      bool
	altHold, approach    bool
	tcas, lnav, modeBits bool
}

// meTargetState builds the target state and status message, TC29 subtype 1
// (ADS-B version 2).
func meTargetState(f *tssFields) uint64 {
	var b bits
	b.add(5, 29)
	b.add(2, 1)
	b.add(1, 0) // SIL supplement: per hour
	b.add(1, b2u(f.fms))
	if f.selAltFt > 0 {
		b.add(11, min(1+uq(f.selAltFt, 32, 11), 2047))
	} else {
		b.add(11, 0)
	}
	if f.baroHPa >= 800 {
		b.add(9, min(1+uq(f.baroHPa-800, 0.8, 9), 511))
	} else {
		b.add(9, 0)
	}
	if math.IsNaN(f.selHdgDeg) {
		b.add(10, 0)
	} else {
		b.add(1, 1)
		b.add(9, angle(wrap360(f.selHdgDeg), 180.0/256, 9))
	}
	b.add(4, uint64(f.nacp))
	b.add(1, b2u(f.nicBaro))
	b.add(2, uint64(f.sil))
	b.add(1, b2u(f.modeBits))
	b.add(1, b2u(f.autopilot))
	b.add(1, b2u(f.vnav))
	b.add(1, b2u(f.altHold))
	b.add(1, 0) // IMF (ADS-R flag)
	b.add(1, b2u(f.approach))
	b.add(1, b2u(f.tcas))
	b.add(1, b2u(f.lnav))
	b.add(2, 0)
	return b.v
}

// opFields is the content of an aircraft operational status message (v2).
type opFields struct {
	surface       bool
	acas, in1090  bool
	singleAntenna bool
	sda           uint8
	nacp, nacv    uint8
	gva, sil      uint8
	nicBaro       bool
	lenWidth      uint8 // surface only: aircraft length/width code
}

// meOpStatus builds the aircraft operational status message, TC31 subtype 0
// (airborne) or 1 (surface), ADS-B version 2.
func meOpStatus(f *opFields) uint64 {
	var cc, om uint64
	om = b2u(f.singleAntenna)<<10 | uint64(f.sda&3)<<8
	if f.surface {
		// CC: 9-10 = 00, 12 = 1090ES IN, 17-19 = NACv, 20 = NIC supp C, 21-24 = L/W
		cc = b2u(f.in1090)<<12 | uint64(f.nacv&7)<<5 | uint64(f.lenWidth&15)
	} else {
		// CC: 9-10 = 00, 11 = ACAS operational, 12 = 1090ES IN, 15 = ARV, 16 = TS, 17-18 = TC
		cc = b2u(f.acas)<<13 | b2u(f.in1090)<<12 | 1<<9 | 1<<8
	}
	var b bits
	b.add(5, 31)
	b.add(3, b2u(f.surface))
	b.add(16, cc)
	b.add(16, om)
	b.add(3, 2) // version 2 (DO-260B)
	b.add(1, 0) // NIC supplement A
	b.add(4, uint64(f.nacp))
	if f.surface {
		b.add(2, 0)
	} else {
		b.add(2, uint64(f.gva))
	}
	b.add(2, uint64(f.sil))
	if f.surface {
		b.add(1, 0) // TRK/HDG: ground track reported
	} else {
		b.add(1, b2u(f.nicBaro))
	}
	b.add(1, 0) // HRD: true north
	b.add(1, 0) // SIL supplement
	b.add(1, 0)
	return b.v
}

// mbBDS20 builds Comm-B register 2,0 (aircraft identification).
func mbBDS20(cs *[8]byte) uint64 {
	var b bits
	b.add(8, 0x20)
	for _, c := range cs {
		b.add(6, sixBit(c))
	}
	return b.v
}

// mbBDS40 builds Comm-B register 4,0 (selected vertical intention).
// Altitudes in ft (<= 0 = not available), baro in hPa (< 800 = not available).
func mbBDS40(mcpAltFt, fmsAltFt, baroHPa float64, vnav, altHold, approach bool) uint64 {
	var b bits
	if mcpAltFt > 0 {
		b.add(1, 1)
		b.add(12, uq(mcpAltFt, 16, 12))
	} else {
		b.add(13, 0)
	}
	if fmsAltFt > 0 {
		b.add(1, 1)
		b.add(12, uq(fmsAltFt, 16, 12))
	} else {
		b.add(13, 0)
	}
	if baroHPa >= 800 {
		b.add(1, 1)
		b.add(12, uq(baroHPa-800, 0.1, 12))
	} else {
		b.add(13, 0)
	}
	b.add(8, 0)
	b.add(1, 1) // status of MCP/FCU mode bits
	b.add(1, b2u(vnav))
	b.add(1, b2u(altHold))
	b.add(1, b2u(approach))
	b.add(2, 0)
	b.add(1, 1) // status of target altitude source
	b.add(2, 2) // FCU/MCP selected altitude
	return b.v
}

// mbBDS50 builds Comm-B register 5,0 (track and turn report).
func mbBDS50(rollDeg, trueTrackDeg, gsKt, trackRateDegS, tasKt float64) uint64 {
	var b bits
	b.add(1, 1)
	b.add(10, twos(rollDeg, 45.0/256, 10))
	b.add(1, 1)
	b.add(11, angle(wrap180(trueTrackDeg), 90.0/512, 11))
	b.add(1, 1)
	b.add(10, uq(gsKt, 2, 10))
	b.add(1, 1)
	b.add(10, twos(trackRateDegS, 8.0/256, 10))
	b.add(1, 1)
	b.add(10, uq(tasKt, 2, 10))
	return b.v
}

// mbBDS60 builds Comm-B register 6,0 (heading and speed report).
func mbBDS60(magHdgDeg, iasKt, mach, baroRateFpm, inertialRateFpm float64) uint64 {
	var b bits
	b.add(1, 1)
	b.add(11, angle(wrap180(magHdgDeg), 90.0/512, 11))
	b.add(1, 1)
	b.add(10, uq(iasKt, 1, 10))
	b.add(1, 1)
	b.add(10, uq(mach, 2.048/512, 10))
	b.add(1, 1)
	b.add(10, twos(baroRateFpm, 32, 10))
	b.add(1, 1)
	b.add(10, twos(inertialRateFpm, 32, 10))
	return b.v
}

// mbBDS44 builds Comm-B register 4,4 (meteorological routine air report):
// FOM/source, wind speed (kt) and direction (deg true, blowing from), static
// air temperature (deg C) and average static pressure (hPa).
func mbBDS44(windKt, windDirDeg, satC, pressHPa float64) uint64 {
	var b bits
	b.add(4, 1) // source: INS
	b.add(1, 1)
	b.add(9, uq(windKt, 1, 9))
	b.add(9, angle(wrap360(windDirDeg), 180.0/256, 9))
	b.add(11, twos(satC, 0.25, 11))
	b.add(1, 1)
	b.add(11, uq(pressHPa, 1, 11))
	b.add(1, 0) // turbulence not available
	b.add(2, 0)
	b.add(1, 0) // humidity not available
	b.add(6, 0)
	return b.v
}

// cprNL is the "number of longitude zones" function of the CPR algorithm
// (NZ = 15).
func cprNL(lat float64) int {
	lat = math.Abs(lat)
	switch {
	case lat < 1e-9:
		return 59
	case lat > 87:
		return 1
	case lat == 87:
		return 2
	}
	const a = 1 - 0.9945218953682733 // 1 - cos(pi / (2*15))
	c := math.Cos(lat * deg2rad)
	return int(math.Floor(2 * math.Pi / math.Acos(1-a/(c*c))))
}

func pmod(x, y float64) float64 {
	r := math.Mod(x, y)
	if r < 0 {
		r += y
	}
	return r
}

// cprEncode implements the CPR position encoding. scale is 2^17 for airborne
// messages and 2^19 for surface messages (of which the low 17 bits are sent).
func cprEncode(lat, lon float64, odd bool, scale float64) (yz, xz uint32) {
	dlat := 360.0 / 60
	if odd {
		dlat = 360.0 / 59
	}
	y := math.Floor(scale*pmod(lat, dlat)/dlat + 0.5)
	rlat := dlat * (y/scale + math.Floor(lat/dlat))
	nl := cprNL(rlat)
	if odd {
		nl--
	}
	if nl < 1 {
		nl = 1
	}
	dlon := 360.0 / float64(nl)
	x := math.Floor(scale*pmod(lon, dlon)/dlon + 0.5)
	return uint32(int64(y)) & 0x1FFFF, uint32(int64(x)) & 0x1FFFF
}

func cprEncodeAirborne(lat, lon float64, odd bool) (uint32, uint32) {
	return cprEncode(lat, lon, odd, 131072)
}

func cprEncodeSurface(lat, lon float64, odd bool) (uint32, uint32) {
	return cprEncode(lat, lon, odd, 524288)
}
