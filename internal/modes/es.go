package modes

import "math"

// field returns bits first..last (1-based, bit 1 = most significant) of a
// 56-bit ME/MB value.
func field(v uint64, first, last uint) uint32 {
	return uint32(v>>(56-last)) & (1<<(last-first+1) - 1)
}

// flag returns bit n (1-based) of a 56-bit ME/MB value.
func flag(v uint64, n uint) bool {
	return v>>(56-n)&1 != 0
}

// aisCharset is the 6-bit character set of aircraft identification fields.
const aisCharset = "@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_ !\"#$%&'()*+,-./0123456789:;<=>?"

// decodeCallsign unpacks eight 6-bit characters from the low 48 bits of v.
// ok is false if any character is outside A-Z, 0-9 and space.
func decodeCallsign(v uint64, out *[8]byte) (ok bool) {
	ok = true
	for i := uint(0); i < 8; i++ {
		c := aisCharset[(v>>(42-6*i))&0x3F]
		out[i] = c
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ' ') {
			ok = false
		}
	}
	return ok
}

// decodeES decodes the 56-bit ME field of an extended squitter. checkIMF is
// set for TIS-B and ADS-R, where one bit of most formats is the ICAO/Mode A
// flag instead of its ADS-B meaning.
func (m *Message) decodeES(me uint64, checkIMF bool) {
	tc := uint8(me >> 51)
	m.TC = tc
	m.Valid |= FTypeCode
	switch {
	case tc == 0:
		// "No position information": altitude may still be present.
		m.decodeAirbornePosition(me, checkIMF)
	case tc <= 4:
		m.decodeIdent(me)
	case tc <= 8:
		m.decodeSurfacePosition(me, checkIMF)
	case tc <= 18:
		m.decodeAirbornePosition(me, checkIMF)
	case tc == 19:
		m.decodeVelocity(me, checkIMF)
	case tc <= 22:
		m.decodeAirbornePosition(me, checkIMF)
	case tc == 23:
		m.decodeTest(me)
	case tc == 28:
		m.decodeAircraftStatus(me, checkIMF)
	case tc == 29:
		m.decodeTargetState(me, checkIMF)
	case tc == 31:
		m.decodeOperationalStatus(me, checkIMF)
	}
}

// decodeIdent handles TC1-4: identification and emitter category.
func (m *Message) decodeIdent(me uint64) {
	cat := uint8(field(me, 6, 8))
	m.Subtype = cat
	// TC4 = set A, TC3 = set B, TC2 = set C, TC1 = set D.
	m.Category = (0x0E-m.TC)<<4 | cat
	m.Valid |= FCategory
	if decodeCallsign(me&0xFFFFFFFFFFFF, &m.Callsign) && m.Callsign != [8]byte{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '} {
		m.Valid |= FCallsign
	}
}

// movementSpeed converts the surface movement code (1..124) to knots. Each
// code is a speed band; the lower edge of the band is returned.
func movementSpeed(mov uint32) float64 {
	switch {
	case mov <= 1:
		return 0 // stopped (< 0.125 kt)
	case mov <= 8:
		return 0.125 * float64(mov-1)
	case mov <= 12:
		return 1 + 0.25*float64(mov-9)
	case mov <= 38:
		return 2 + 0.5*float64(mov-13)
	case mov <= 93:
		return 15 + float64(mov-39)
	case mov <= 108:
		return 70 + 2*float64(mov-94)
	case mov <= 123:
		return 100 + 5*float64(mov-109)
	}
	return 175 // 124: >= 175 kt
}

// decodeSurfacePosition handles TC5-8.
func (m *Message) decodeSurfacePosition(me uint64, checkIMF bool) {
	m.AirGround = AGGround
	if mov := field(me, 6, 12); mov >= 1 && mov <= 124 {
		m.GS = movementSpeed(mov)
		m.Valid |= FGroundSpeed
	}
	if flag(me, 13) {
		m.Track = float64(field(me, 14, 20)) * (360.0 / 128.0)
		m.Valid |= FTrack
	}
	if checkIMF {
		// TIS-B / ADS-R: bit 21 is the IMF flag instead of the T bit.
		if flag(me, 21) {
			m.setIMF()
		}
	} else {
		m.CPRTime = flag(me, 21)
	}
	m.CPROdd = flag(me, 22)
	m.CPRLat = field(me, 23, 39)
	m.CPRLon = field(me, 40, 56)
	m.CPRSurface = true
	m.Valid |= FCPR
}

// decodeAirbornePosition handles TC0, TC9-18 (barometric altitude) and
// TC20-22 (GNSS height).
func (m *Message) decodeAirbornePosition(me uint64, checkIMF bool) {
	// A transponder that declares itself on the ground (CA=4) while sending
	// an airborne format is contradicting itself: its altitude is not used.
	declaredGround := m.AirGround == AGGround
	if m.AirGround == AGUnknown {
		m.AirGround = AGUncertain
	}
	ss := uint8(field(me, 6, 7))
	m.SurvStatus = ss
	m.Valid |= FSurvStatus
	switch ss {
	case 0:
		m.Valid |= FAlert | FSPI
	case 1, 2:
		// The alert states take precedence over SPI, so SPI is unknown here.
		m.Alert = true
		m.Valid |= FAlert
	case 3:
		m.SPI = true
		m.Valid |= FAlert | FSPI
	}

	if checkIMF {
		// TIS-B / ADS-R: bit 8 is the IMF flag.
		if flag(me, 8) {
			m.setIMF()
		}
	} else {
		m.NICSuppB = flag(me, 8)
		m.Valid |= FNICSuppB
	}

	// The 12-bit altitude field uses the Mode S altitude code without its M
	// bit, both for barometric altitude and for GNSS height (HAE): Q=1 is
	// N*25-1000 ft, Q=0 the 100 ft Gillham code. DO-260B 2.2.3.2.3.4.3 and
	// ICAO Doc 9871 (airborne position format, TYPE 20-22) carry GNSS height
	// in the same ALTITUDE subfield and coding as TYPE 9-18, and readsb /
	// dump1090-fa decode it with the same AC12 routine, in feet. Reading the
	// raw 12 bits as metres (as pyModeS does) cannot be right: it would cap
	// the field at 4095 m (13,435 ft), below normal cruise heights, and would
	// ignore the Q bit that transmitters set.
	ac12 := uint16(field(me, 9, 20))
	if !declaredGround {
		v, kind := decodeAC12(ac12)
		if kind == altQ25 || kind == altGillham {
			if m.TC >= 20 {
				m.AltGNSS = v
				m.Valid |= FAltGNSS
			} else {
				m.AltBaro = v
				m.AltBaroGillham = kind == altGillham
				m.Valid |= FAltBaro
			}
		}
	}

	if m.TC == 0 {
		return // no position available
	}
	m.CPRTime = flag(me, 21)
	m.CPROdd = flag(me, 22)
	m.CPRLat = field(me, 23, 39)
	m.CPRLon = field(me, 40, 56)
	// Known transponder fault (seen by dump1090 on several airframes): type
	// 15 with no altitude, a zero longitude and zeros in the low latitude
	// bits, alternating with good type 14 reports. Never use it as a position.
	if m.TC == 15 && ac12 == 0 && m.CPRLon == 0 && m.CPRLat&0x0FFF == 0 {
		return
	}
	m.Valid |= FCPR
}

// decodeVelocity handles TC19: airborne velocity.
func (m *Message) decodeVelocity(me uint64, checkIMF bool) {
	st := uint8(field(me, 6, 8))
	m.Subtype = st
	if st < 1 || st > 4 {
		return
	}
	if m.AirGround == AGUnknown {
		m.AirGround = AGUncertain
	}
	if checkIMF {
		// TIS-B / ADS-R: bit 9 is the IMF flag instead of intent change.
		if flag(me, 9) {
			m.setIMF()
		}
	} else {
		m.IntentChange = flag(me, 9)
	}
	m.NACv = uint8(field(me, 11, 13))
	m.Valid |= FNACv

	// Subtypes 2 and 4 are the supersonic variants with 4 kt resolution.
	scale := 1.0
	if st == 2 || st == 4 {
		scale = 4
	}

	if st <= 2 {
		// Velocity over ground as east-west / north-south components.
		ewRaw := field(me, 15, 24)
		nsRaw := field(me, 26, 35)
		if ewRaw != 0 && nsRaw != 0 {
			ew := float64(ewRaw-1) * scale
			ns := float64(nsRaw-1) * scale
			if flag(me, 14) { // 1 = flying west
				ew = -ew
			}
			if flag(me, 25) { // 1 = flying south
				ns = -ns
			}
			m.GS = math.Sqrt(ew*ew + ns*ns)
			m.Valid |= FGroundSpeed
			if ew != 0 || ns != 0 {
				trk := math.Atan2(ew, ns) * (180 / math.Pi)
				if trk < 0 {
					trk += 360
				}
				m.Track = trk
				m.Valid |= FTrack
			}
		}
	} else {
		// Airspeed and heading.
		if flag(me, 14) {
			m.Heading = float64(field(me, 15, 24)) * (360.0 / 1024.0)
			m.Valid |= FHeading
		}
		if as := field(me, 26, 35); as != 0 {
			m.Airspeed = float64(as-1) * scale
			m.AirspeedTAS = flag(me, 25)
			m.Valid |= FAirspeed
		}
	}

	if vr := field(me, 38, 46); vr != 0 {
		rate := int32(vr-1) * 64
		if flag(me, 37) { // 1 = descending
			rate = -rate
		}
		m.VertRate = rate
		m.VertRateBaro = flag(me, 36)
		m.Valid |= FVertRate
	}
	if d := field(me, 50, 56); d != 0 {
		delta := int32(d-1) * 25
		if flag(me, 49) { // 1 = GNSS height below barometric altitude
			delta = -delta
		}
		m.GNSSDelta = delta
		m.Valid |= FGNSSDelta
	}
}

// decodeTest handles TC23. Subtype 7 carries the Mode A code of some
// transponders (see 1090-WP-15-20).
func (m *Message) decodeTest(me uint64) {
	m.Subtype = uint8(field(me, 6, 8))
	if m.Subtype == 7 {
		if id := uint16(field(me, 9, 21)); id != 0 {
			m.Squawk = decodeID13(id)
			m.Valid |= FSquawk
		}
	}
}

// decodeAircraftStatus handles TC28: emergency/priority status with Mode A
// code (subtype 1) and the ACAS RA broadcast (subtype 2).
func (m *Message) decodeAircraftStatus(me uint64, checkIMF bool) {
	m.Subtype = uint8(field(me, 6, 8))
	switch m.Subtype {
	case 1:
		m.Emergency = uint8(field(me, 9, 11))
		m.Valid |= FEmergency
		if id := uint16(field(me, 12, 24)); id != 0 {
			m.Squawk = decodeID13(id)
			m.Valid |= FSquawk
		}
		if checkIMF && flag(me, 56) {
			m.setIMF()
		}
	case 2:
		m.Valid |= FACASRA
	}
}

// decodeTargetState handles TC29: target state and status.
func (m *Message) decodeTargetState(me uint64, checkIMF bool) {
	m.Subtype = uint8(field(me, 6, 7))
	if checkIMF && flag(me, 51) {
		m.setIMF()
	}
	switch m.Subtype {
	case 0: // DO-260A (ADS-B version 1) layout
		if flag(me, 11) {
			return // not backward compatible: layout unknown
		}
		vsrc := field(me, 8, 9) // 0 none, 1 MCP/FCU, 2 holding altitude, 3 FMS/RNAV
		if alt := field(me, 16, 25); vsrc != 0 && alt <= 1010 {
			m.SelAlt = int32(alt)*100 - 1000
			m.SelAltFMS = vsrc == 3
			m.Valid |= FSelAlt
		}
		if hsrc := field(me, 26, 27); hsrc != 0 {
			if hdg := field(me, 28, 36); hdg < 360 {
				m.SelHeading = float64(hdg)
				m.SelHeadingIsTrack = flag(me, 37)
				m.Valid |= FSelHeading
			}
		}
		m.NACp = uint8(field(me, 40, 43))
		m.NICBaro = flag(me, 44)
		m.SIL = uint8(field(me, 45, 46))
		m.Valid |= FNACp | FNICBaro | FSIL
		m.Emergency = uint8(field(me, 54, 56))
		m.Valid |= FEmergency

	case 1: // DO-260B (ADS-B version 2) layout
		m.SILPerSample = flag(me, 8)
		m.Valid |= FSILType
		if alt := field(me, 10, 20); alt != 0 {
			m.SelAlt = int32(alt-1) * 32
			m.SelAltFMS = flag(me, 9)
			m.Valid |= FSelAlt
		}
		if qnh := field(me, 21, 29); qnh != 0 {
			m.BaroSetting = 800 + float64(qnh-1)*0.8
			m.Valid |= FBaroSetting
		}
		if flag(me, 30) {
			// Sign bit and 8 magnitude bits form a 9-bit value of 180/256 degrees.
			m.SelHeading = float64(field(me, 31, 39)) * (180.0 / 256.0)
			m.Valid |= FSelHeading
		}
		m.NACp = uint8(field(me, 40, 43))
		m.NICBaro = flag(me, 44)
		m.SIL = uint8(field(me, 45, 46))
		m.Valid |= FNACp | FNICBaro | FSIL
		if flag(me, 47) { // mode bits are populated
			var nm NavModes
			if flag(me, 48) {
				nm |= NavAutopilot
			}
			if flag(me, 49) {
				nm |= NavVNAV
			}
			if flag(me, 50) {
				nm |= NavAltHold
			}
			if flag(me, 52) {
				nm |= NavApproach
			}
			if flag(me, 54) {
				nm |= NavLNAV
			}
			if flag(me, 53) {
				nm |= NavTCAS
			}
			m.NavModes = nm
			m.Valid |= FNavModes
		}
	}
}

// decodeOperationalStatus handles TC31: aircraft operational status.
func (m *Message) decodeOperationalStatus(me uint64, checkIMF bool) {
	st := uint8(field(me, 6, 8))
	m.Subtype = st
	if st > 1 {
		return // only airborne (0) and surface (1) are defined
	}
	if checkIMF && flag(me, 56) {
		m.setIMF()
	}
	if st == 1 {
		m.AirGround = AGGround
	}
	m.Version = uint8(field(me, 41, 43))
	m.Valid |= FVersion
	if m.Version == 0 {
		// DO-260 operational status has a different, rarely transmitted
		// layout without integrity fields.
		return
	}

	m.CapClass = uint16(field(me, 9, 24))
	m.OpMode = uint16(field(me, 25, 40))
	m.Valid |= FOpStatus

	m.NICSuppA = flag(me, 44)
	m.NACp = uint8(field(me, 45, 48))
	m.SIL = uint8(field(me, 51, 52))
	m.HRDMagnetic = flag(me, 54)
	m.Valid |= FNICSuppA | FNACp | FSIL | FHRD
	if st == 0 {
		m.NICBaro = flag(me, 53)
		m.Valid |= FNICBaro
	} else {
		m.TrackIsHeading = flag(me, 53)
		m.Valid |= FTrackIsHeading
	}

	surfaceCC := st == 1 && field(me, 9, 10) == 0
	if m.Version == 1 {
		if surfaceCC && field(me, 13, 14) == 0 {
			m.LengthWidth = uint8(field(me, 21, 24))
			m.Valid |= FLengthWidth
		}
		return
	}

	// Version 2 and later.
	m.SILPerSample = flag(me, 55)
	m.Valid |= FSILType
	if field(me, 25, 26) == 0 { // operational mode format 0
		m.SDA = uint8(field(me, 31, 32))
		m.Valid |= FSDA
	}
	if st == 0 {
		m.GVA = uint8(field(me, 49, 50))
		m.Valid |= FGVA
	} else if surfaceCC {
		m.NACv = uint8(field(me, 17, 19))
		m.NICSuppC = flag(me, 20)
		m.LengthWidth = uint8(field(me, 21, 24))
		m.Valid |= FNACv | FNICSuppC | FLengthWidth
	}
}

// TCASOperational reports the "TCAS/ACAS operational" bit of a version 2
// target state and status message (TC29 subtype 1) or of an airborne
// operational status message (TC31 subtype 0; the bit is inverted in version
// 1). ok is false when the message does not carry it.
//
// In a target state message the bit is valid on its own, whereas
// Message.NavModes (which also includes NavTCAS) is only filled when the
// transponder flags its mode bits as populated.
func (m *Message) TCASOperational() (operational, ok bool) {
	if m.Valid&FTypeCode == 0 {
		return false, false
	}
	if m.TC == 29 {
		if m.Subtype != 1 {
			return false, false
		}
		return flag(m.payload(), 53), true
	}
	if m.TC != 31 || m.Valid&FOpStatus == 0 || m.Subtype != 0 || m.CapClass&0xC000 != 0 {
		return false, false
	}
	bit := m.CapClass&0x2000 != 0 // ME bit 11
	if m.Version == 1 {
		return !bit, true
	}
	return bit, true
}

// TCASRAActive reports the "TCAS resolution advisory active" operational mode
// bit of an operational status message.
func (m *Message) TCASRAActive() (active, ok bool) {
	if m.Valid&FOpStatus == 0 || m.OpMode&0xC000 != 0 {
		return false, false
	}
	return m.OpMode&0x2000 != 0, true // ME bit 27
}

// IdentActive reports the "IDENT switch active" operational mode bit of an
// operational status message.
func (m *Message) IdentActive() (active, ok bool) {
	if m.Valid&FOpStatus == 0 || m.OpMode&0xC000 != 0 {
		return false, false
	}
	return m.OpMode&0x1000 != 0, true // ME bit 28
}
