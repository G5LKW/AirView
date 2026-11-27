package modes

import (
	"errors"

	"airview/internal/model"
)

// Errors returned by Decode. When ErrBadCRC or ErrUnsupportedDF is returned,
// Message.DF, Message.Raw, Message.Len and Message.Residual are still set so
// the caller can keep per-format statistics.
var (
	ErrFrameLength   = errors.New("modes: frame is not 7 or 14 bytes, or its length does not match its downlink format")
	ErrBadCRC        = errors.New("modes: parity check failed")
	ErrUnsupportedDF = errors.New("modes: unsupported downlink format")
)

// CRCStatus says how far the parity of a frame can be trusted.
type CRCStatus uint8

const (
	CRCUnknown       CRCStatus = iota // not evaluated (length error, unsupported DF)
	CRCOK                             // residual zero: DF17, DF18, DF11 acquisition squitter
	CRCInterrogator                   // DF11 with residual 1..79 (II/SI code overlay): plausible, not certain
	CRCAddressParity                  // DF0/4/5/16/20/21/24: address overlaid on parity, unverifiable from the frame alone
	CRCFixed1                         // DF17/DF18 accepted after repairing one bit
	CRCFixed2                         // DF17 accepted after repairing two bits
	CRCBad                            // parity failure
)

// String returns a short label for logs and diagnostics.
func (c CRCStatus) String() string {
	switch c {
	case CRCOK:
		return "ok"
	case CRCInterrogator:
		return "interrogator"
	case CRCAddressParity:
		return "address-parity"
	case CRCFixed1:
		return "fixed-1bit"
	case CRCFixed2:
		return "fixed-2bit"
	case CRCBad:
		return "bad"
	}
	return "unknown"
}

// AddrType classifies what the 24-bit address of a message identifies.
type AddrType uint8

const (
	AddrUnknown       AddrType = iota // DF18 with a control field we cannot interpret
	AddrModeS                         // ICAO address from a Mode S surveillance/Comm-B/all-call reply
	AddrADSBICAO                      // DF17: ADS-B from a transponder, ICAO address
	AddrADSBICAONT                    // DF18 CF=0: ADS-B from a non-transponder device, ICAO address
	AddrADSBOther                     // DF18 CF=1: ADS-B, non-ICAO address (anonymous, vehicle, obstruction)
	AddrTISBICAO                      // DF18 CF=2/3, IMF=0: TIS-B target identified by ICAO address
	AddrTISBTrackFile                 // DF18 CF=2/3, IMF=1: TIS-B track file number / Mode A code, not an ICAO address
	AddrTISBOther                     // DF18 CF=5: TIS-B relay of ADS-B that uses an anonymous address
	AddrADSRICAO                      // DF18 CF=6, IMF=0: ADS-R rebroadcast, ICAO address
	AddrADSROther                     // DF18 CF=6, IMF=1: ADS-R rebroadcast, non-ICAO address
)

// IsICAO reports whether the address is a real ICAO 24-bit aircraft address.
func (a AddrType) IsICAO() bool {
	switch a {
	case AddrModeS, AddrADSBICAO, AddrADSBICAONT, AddrTISBICAO, AddrADSRICAO:
		return true
	}
	return false
}

// Label returns the value used in model.Aircraft.AddrType:
// "icao", "non-icao", "tisb-track" or "anon".
func (a AddrType) Label() string {
	switch a {
	case AddrModeS, AddrADSBICAO, AddrADSBICAONT, AddrTISBICAO, AddrADSRICAO:
		return "icao"
	case AddrTISBTrackFile:
		return "tisb-track"
	case AddrTISBOther:
		return "anon"
	}
	return "non-icao"
}

// String returns a detailed label (dump1090/readsb style).
func (a AddrType) String() string {
	switch a {
	case AddrModeS:
		return "mode_s"
	case AddrADSBICAO:
		return "adsb_icao"
	case AddrADSBICAONT:
		return "adsb_icao_nt"
	case AddrADSBOther:
		return "adsb_other"
	case AddrTISBICAO:
		return "tisb_icao"
	case AddrTISBTrackFile:
		return "tisb_trackfile"
	case AddrTISBOther:
		return "tisb_other"
	case AddrADSRICAO:
		return "adsr_icao"
	case AddrADSROther:
		return "adsr_other"
	}
	return "unknown"
}

// Source is the surveillance service a message belongs to.
type Source uint8

const (
	SourceUnknown Source = iota
	SourceModeS          // DF0/4/5/11/16/20/21/24
	SourceADSB           // DF17, DF18 CF=0/1
	SourceTISB           // DF18 CF=2/3/5
	SourceADSR           // DF18 CF=6
)

// Label returns the matching model.Src* constant.
func (s Source) Label() string {
	switch s {
	case SourceModeS:
		return model.SrcModeS
	case SourceADSB:
		return model.SrcADSB
	case SourceTISB:
		return model.SrcTISB
	case SourceADSR:
		return model.SrcADSR
	}
	return model.SrcOther
}

// AirGround is the air/ground state claimed by a message.
type AirGround uint8

const (
	// AGUnknown: the message says nothing about air/ground state.
	AGUnknown AirGround = iota
	// AGGround: the transponder positively reports being on the ground
	// (CA=4, FS=1/3, VS=1, surface position message).
	AGGround
	// AGAirborne: the transponder positively reports being airborne (CA=5).
	AGAirborne
	// AGUncertain: an "airborne" indication from a field that is also used
	// when the transponder cannot tell (FS=0/2, VS=0, airborne-format
	// squitter without CA=5). Treat as airborne only with corroboration.
	AGUncertain
)

// NavModes is a bit set of autopilot / navigation modes (TC29, BDS 4,0).
type NavModes uint8

const (
	NavAutopilot NavModes = 1 << iota
	NavVNAV
	NavAltHold
	NavApproach
	NavLNAV
	NavTCAS
)

var navModeNames = [...]string{"autopilot", "vnav", "althold", "approach", "lnav", "tcas"}

// AppendNames appends the model.Aircraft.NavModes names of the set bits to dst.
func (n NavModes) AppendNames(dst []string) []string {
	for i, name := range navModeNames {
		if n&(1<<uint(i)) != 0 {
			dst = append(dst, name)
		}
	}
	return dst
}

// Fields is a bit mask naming the optional fields of a Message.
type Fields uint64

// Validity bits for Message.Valid. A field must only be read when its bit is
// set.
const (
	FCapability     Fields = 1 << iota // CA (DF11, DF17)
	FFlightStatus                      // FS, DR, UM (DF4/5/20/21)
	FAlert                             // Alert (from FS or the ES surveillance status)
	FSPI                               // SPI (from FS or the ES surveillance status)
	FIID                               // IID (DF11)
	FACAS                              // VS, CC, SL, RI (DF0, DF16)
	FTypeCode                          // TC, Subtype (DF17/18)
	FCallsign                          // Callsign (TC1-4, or a DF20/21 reply recognised as BDS 2,0)
	FCategory                          // Category
	FSquawk                            // Squawk
	FAltBaro                           // AltBaro, AltBaroGillham
	FAltMetric                         // AltMetric (M bit set; reserved encoding)
	FAltGNSS                           // AltGNSS
	FGNSSDelta                         // GNSSDelta
	FCPR                               // CPRLat, CPRLon, CPROdd, CPRSurface, CPRTime
	FSurvStatus                        // SurvStatus
	FNICSuppB                          // NICSuppB
	FGroundSpeed                       // GS
	FTrack                             // Track
	FHeading                           // Heading
	FAirspeed                          // Airspeed, AirspeedTAS
	FVertRate                          // VertRate, VertRateBaro
	FNACv                              // NACv
	FEmergency                         // Emergency
	FSelAlt                            // SelAlt, SelAltFMS
	FBaroSetting                       // BaroSetting
	FSelHeading                        // SelHeading, SelHeadingIsTrack
	FNavModes                          // NavModes
	FNACp                              // NACp
	FSIL                               // SIL, SILPerSample (when FSILType is also set)
	FSILType                           // SILPerSample
	FNICBaro                           // NICBaro
	FVersion                           // Version
	FNICSuppA                          // NICSuppA
	FNICSuppC                          // NICSuppC
	FGVA                               // GVA
	FSDA                               // SDA
	FHRD                               // HRDMagnetic
	FTrackIsHeading                    // TrackIsHeading (surface opstatus TAH bit)
	FLengthWidth                       // LengthWidth
	FOpStatus                          // CapClass, OpMode
	FACASRA                            // an ACAS resolution advisory is carried; see Message.ACASRA
	FCommB                             // MB holds a non-empty Comm-B payload; see Message.BDS

	// FAlertSPI is set when both Alert and SPI are known. An airborne
	// position with a surveillance status of "alert" sets FAlert only: the
	// alert states take precedence over SPI, so the SPI state is unknown.
	FAlertSPI = FAlert | FSPI
)

// Emergency / priority status values (TC28 subtype 1, TC29 subtype 0).
const (
	EmergencyNone     = 0
	EmergencyGeneral  = 1
	EmergencyMedical  = 2
	EmergencyMinFuel  = 3
	EmergencyNoComms  = 4
	EmergencyUnlawful = 5
	EmergencyDowned   = 6
	EmergencyReserved = 7
)

// Message is one decoded Mode S frame. The zero value is an empty message.
// It contains no pointers, so reusing one Message for every frame costs no
// allocations.
type Message struct {
	// Raw is the frame as decoded, after any error correction. Only the first
	// Len bytes are meaningful.
	Raw [14]byte
	Len uint8 // 7 or 14
	DF  uint8 // downlink format; DF24..31 (Comm-D) are all reported as 24

	// Valid names the optional fields that were decoded.
	Valid Fields

	// ICAO is the 24-bit address: the AA field for DF11/17/18, otherwise the
	// address recovered from the parity field (AddressFromParity is set).
	ICAO     uint32
	AddrType AddrType
	Source   Source
	// IMF is the ICAO/Mode A flag of TIS-B and ADS-R messages: set when the
	// address is not an ICAO address.
	IMF bool
	// AddressFromParity is set for DF0/4/5/16/20/21/24. Such an address is only
	// trustworthy if the caller already tracks that aircraft from a
	// self-checking format. For DF20/21 a transponder using the BDS overlay
	// (data parity) yields address XOR (BDS<<16); see OverlayAddress.
	AddressFromParity bool

	CRC CRCStatus
	// Residual is the parity residual of the frame as received (before any
	// correction).
	Residual uint32
	// FixedBits is the number of corrected bits (0..2); FixedBit holds their
	// positions (0 = first bit of the frame).
	FixedBits uint8
	FixedBit  [2]uint8

	// IID is the DF11 interrogator code overlay: 0 = acquisition squitter,
	// 1..15 = II code, 16..79 = SI code + 16. See Interrogator.
	IID uint8

	CA uint8 // capability (DF11/17), 0..7
	CF uint8 // control field (DF18), 0..7
	FS uint8 // flight status (DF4/5/20/21), 0..7
	DR uint8 // downlink request (DF4/5/20/21)
	UM uint8 // utility message (DF4/5/20/21): IIS<<2 | IDS

	VS bool  // DF0/16 vertical status: true = on the ground
	CC bool  // DF0 cross-link capability
	SL uint8 // DF0/16 ACAS sensitivity level
	RI uint8 // DF0/16 reply information (ACAS capability / max airspeed)

	// AirGround is the air/ground state claimed by this message.
	AirGround AirGround
	Alert     bool // alert condition (FS 2/3/4, or ES surveillance status 1/2); valid with FAlert
	SPI       bool // special position identification (FS 4/5, or ES surveillance status 3); valid with FSPI

	// Extended squitter.
	TC      uint8 // ME type code 0..31
	Subtype uint8 // ME subtype (TC19/23/28/29/31) or category number (TC1-4)

	// Callsign is the 8 character identification, space padded.
	Callsign [8]byte
	// Category is the emitter category as a hex pair: 0xA3 = "A3".
	Category uint8
	// Squawk is the Mode A code with one octal digit per hex nibble (0x7500).
	Squawk uint16

	AltBaro        int32 // barometric altitude, ft
	AltBaroGillham bool  // true: 100 ft Gillham encoding; false: 25 ft
	AltMetric      int32 // raw 12-bit code of an M=1 altitude (reserved, scale undefined)
	AltGNSS        int32 // GNSS height above the ellipsoid, ft (TC20-22)
	GNSSDelta      int32 // GNSS height minus barometric altitude, ft (TC19)

	// Compact position report.
	CPRLat     uint32 // 17 bits
	CPRLon     uint32 // 17 bits
	CPROdd     bool   // CPR format: false = even, true = odd
	CPRSurface bool   // surface encoding (TC5-8)
	CPRTime    bool   // T bit: position is synchronised to a 0.2 s UTC epoch
	// SurvStatus is the airborne position surveillance status: 0 none,
	// 1 permanent alert, 2 temporary alert, 3 SPI.
	SurvStatus uint8
	// NICSuppB is the NIC supplement B bit of airborne positions (ADS-B v2;
	// in v0/v1 the same bit is the single antenna flag).
	NICSuppB bool

	GS    float64 // ground speed, kt (TC19 subtype 1/2, surface movement)
	Track float64 // true track over ground, degrees (TC19 subtype 1/2, surface)
	// Heading is the TC19 subtype 3/4 heading, degrees. It is magnetic unless
	// the aircraft's operational status says otherwise (HRDMagnetic).
	Heading      float64
	Airspeed     float64 // kt (TC19 subtype 3/4)
	AirspeedTAS  bool    // true: Airspeed is TAS; false: IAS
	VertRate     int32   // ft/min, positive up
	VertRateBaro bool    // true: barometric rate; false: GNSS/INS (geometric)
	NACv         uint8   // TC19 (NUCr in ADS-B v0), surface opstatus in v2
	IntentChange bool    // TC19 intent change flag (not for TIS-B/ADS-R)

	// Emergency is the emergency/priority status, Emergency* constants.
	Emergency uint8

	// Target state (TC29).
	SelAlt            int32   // selected altitude, ft
	SelAltFMS         bool    // true: FMS/RNAV source; false: MCP/FCU
	BaroSetting       float64 // QNH/QFE setting, hPa
	SelHeading        float64 // selected heading, degrees
	SelHeadingIsTrack bool    // TC29 subtype 0 only: the angle is a track, not a heading
	NavModes          NavModes

	// Accuracy / integrity (TC29, TC31).
	NACp         uint8
	SIL          uint8
	SILPerSample bool // SIL probability is per sample (true) or per hour (false)
	NICBaro      bool
	Version      uint8 // ADS-B version (0 = DO-260, 1 = DO-260A, 2 = DO-260B, 3 = DO-260C)
	NICSuppA     bool
	NICSuppC     bool // surface only
	GVA          uint8
	SDA          uint8
	// HRDMagnetic: heading reference direction is magnetic north (true) or
	// true north (false).
	HRDMagnetic bool
	// TrackIsHeading: surface position "track" values are heading (true) or
	// ground track (false).
	TrackIsHeading bool
	LengthWidth    uint8  // surface aircraft length/width code
	CapClass       uint16 // raw capability class codes (TC31 ME bits 9-24)
	OpMode         uint16 // raw operational mode codes (TC31 ME bits 25-40)

	// BDS holds the Comm-B register candidates of a DF20/21 reply, best
	// first. Only the first NumBDS entries are valid.
	BDS    [MaxBDS]BDSCandidate
	NumBDS uint8
}

// Has reports whether all fields in f are valid.
func (m *Message) Has(f Fields) bool { return m.Valid&f == f }

// Long reports whether the message is a 112-bit frame.
func (m *Message) Long() bool { return m.Len == 14 }

// payload returns frame bits 33-88 (ME, MB or MV field) as a 56-bit integer.
func (m *Message) payload() uint64 {
	r := &m.Raw
	return uint64(r[4])<<48 | uint64(r[5])<<40 | uint64(r[6])<<32 | uint64(r[7])<<24 |
		uint64(r[8])<<16 | uint64(r[9])<<8 | uint64(r[10])
}

// ME returns the 56-bit extended squitter message field (DF17/18), bit 1 in
// bit 55 of the result. It is 0 for short frames.
func (m *Message) ME() uint64 {
	if m.Len != 14 {
		return 0
	}
	return m.payload()
}

// MB returns the 56-bit Comm-B message field (DF20/21) or the DF16 MV field,
// bit 1 in bit 55 of the result. It is 0 for short frames.
func (m *Message) MB() uint64 { return m.ME() }

// Interrogator decodes the DF11 interrogator code. si is true for a
// surveillance identifier (SI 1..63), false for an interrogator identifier
// (II 0..15). ok is false when the message is not a DF11.
func (m *Message) Interrogator() (code uint8, si bool, ok bool) {
	if m.Valid&FIID == 0 {
		return 0, false, false
	}
	if m.IID < 16 {
		return m.IID, false, true
	}
	return m.IID - 16, true, true
}

// AddrKey returns a map key that keeps non-ICAO addresses apart from real
// ICAO addresses with the same 24 bits: ICAO | NonICAOFlag when the address
// type is not an ICAO address.
func (m *Message) AddrKey() uint32 {
	if m.AddrType.IsICAO() {
		return m.ICAO
	}
	return m.ICAO | NonICAOFlag
}

// Options tunes DecodeOpts.
type Options struct {
	// FixErrors enables parity error correction for extended squitters:
	// 0 = none, 1 = repair single bit errors (DF17 and DF18), 2 = additionally
	// repair two bit errors (DF17 only). Errors in the five DF bits are never
	// repaired. Corrected frames are flagged CRCFixed1 / CRCFixed2; a caller
	// should accept CRCFixed2 frames only for aircraft it already tracks.
	FixErrors int
	// NoCommB skips Comm-B register inference for DF20/21.
	NoCommB bool
}

// Decode decodes a 7 or 14 byte Mode S frame into m without error
// correction. m is completely overwritten; frame is not retained.
func Decode(frame []byte, m *Message) error {
	return DecodeOpts(frame, m, Options{})
}

// DecodeFrame is a convenience wrapper around Decode returning the message
// by value.
func DecodeFrame(frame []byte) (Message, error) {
	var m Message
	err := Decode(frame, &m)
	return m, err
}

// DecodeHex decodes a frame given as 14 or 28 hexadecimal digits.
func DecodeHex(s string) (Message, error) {
	var (
		m   Message
		buf [14]byte
	)
	n, ok := parseHexFrame(s, &buf)
	if !ok {
		return m, ErrFrameLength
	}
	err := Decode(buf[:n], &m)
	return m, err
}

func parseHexFrame(s string, buf *[14]byte) (n int, ok bool) {
	if len(s) != 14 && len(s) != 28 {
		return 0, false
	}
	for i := 0; i < len(s); i += 2 {
		hi, ok1 := hexVal(s[i])
		lo, ok2 := hexVal(s[i+1])
		if !ok1 || !ok2 {
			return 0, false
		}
		buf[i/2] = hi<<4 | lo
	}
	return len(s) / 2, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// DecodeOpts decodes a 7 or 14 byte Mode S frame into m. m is completely
// overwritten; frame is not retained or modified.
func DecodeOpts(frame []byte, m *Message, o Options) error {
	*m = Message{}
	n := len(frame)
	if n != 7 && n != 14 {
		return ErrFrameLength
	}
	df := frame[0] >> 3
	if df >= 24 {
		df = 24
	}
	m.DF = df
	if (df >= 16) != (n == 14) {
		return ErrFrameLength
	}
	copy(m.Raw[:], frame)
	m.Len = uint8(n)
	res := Residual(m.Raw[:n])
	m.Residual = res
	r := &m.Raw

	switch df {
	case 0: // short air-air surveillance
		m.addressFromParity(res)
		m.decodeAirAir()

	case 4, 20: // surveillance / Comm-B altitude reply
		m.addressFromParity(res)
		m.decodeFlightStatus()
		m.setAC13(uint16(r[2])<<8 | uint16(r[3]))
		if df == 20 {
			m.decodeCommB(o)
		}

	case 5, 21: // surveillance / Comm-B identity reply
		m.addressFromParity(res)
		m.decodeFlightStatus()
		m.Squawk = decodeID13((uint16(r[2])<<8 | uint16(r[3])) & 0x1FFF)
		m.Valid |= FSquawk
		if df == 21 {
			m.decodeCommB(o)
		}

	case 11: // all-call reply / acquisition squitter
		// The parity is overlaid with CL (3 bits) and IC (4 bits) only; CL
		// values above 4 are not assigned.
		if res > 79 {
			m.CRC = CRCBad
			return ErrBadCRC
		}
		if res == 0 {
			m.CRC = CRCOK
		} else {
			m.CRC = CRCInterrogator
		}
		m.IID = uint8(res)
		m.ICAO = uint32(r[1])<<16 | uint32(r[2])<<8 | uint32(r[3])
		m.AddrType = AddrModeS
		m.Source = SourceModeS
		m.Valid |= FIID
		m.decodeCapability()

	case 16: // long air-air surveillance
		m.addressFromParity(res)
		m.decodeAirAir()
		// MV field: an ACAS coordination reply with VDS = 0x30 carries the
		// same resolution advisory layout as BDS 3,0.
		if r[4] == 0x30 {
			m.Valid |= FACASRA
		}

	case 17, 18: // extended squitter
		if res == 0 {
			m.CRC = CRCOK
		} else if !m.fixErrors(res, o.FixErrors) {
			m.CRC = CRCBad
			return ErrBadCRC
		}
		m.ICAO = uint32(r[1])<<16 | uint32(r[2])<<8 | uint32(r[3])
		if df == 17 {
			m.AddrType = AddrADSBICAO
			m.Source = SourceADSB
			m.decodeCapability()
			m.decodeES(m.payload(), false)
		} else {
			m.decodeDF18()
		}

	case 24: // Comm-D extended length message
		m.addressFromParity(res)

	default:
		return ErrUnsupportedDF
	}
	return nil
}

// fixErrors tries to repair a DF17/DF18 frame with a non-zero residual.
func (m *Message) fixErrors(res uint32, maxBits int) bool {
	if maxBits > 2 {
		maxBits = 2
	}
	if m.DF != 17 && maxBits > 1 {
		maxBits = 1
	}
	b0, b1, ok := lookupSyndrome(res, maxBits)
	if !ok {
		return false
	}
	m.Raw[b0>>3] ^= 0x80 >> (b0 & 7)
	m.FixedBit[0] = b0
	m.FixedBits = 1
	m.CRC = CRCFixed1
	if b1 != noBit {
		m.Raw[b1>>3] ^= 0x80 >> (b1 & 7)
		m.FixedBit[1] = b1
		m.FixedBits = 2
		m.CRC = CRCFixed2
	}
	return true
}

// AddressCorrected reports whether error correction changed a bit of the
// 24-bit address field. Such a frame should only be accepted for an address
// that is already being tracked: the repair may have produced a wrong
// address that happens to satisfy the parity.
func (m *Message) AddressCorrected() bool {
	for i := uint8(0); i < m.FixedBits && i < 2; i++ {
		if b := m.FixedBit[i]; b >= 8 && b < 32 {
			return true
		}
	}
	return false
}

func (m *Message) addressFromParity(res uint32) {
	m.ICAO = res
	m.AddressFromParity = true
	m.CRC = CRCAddressParity
	m.AddrType = AddrModeS
	m.Source = SourceModeS
}

// decodeCapability handles the CA field of DF11 and DF17.
func (m *Message) decodeCapability() {
	m.CA = m.Raw[0] & 7
	m.Valid |= FCapability
	switch m.CA {
	case 4:
		m.AirGround = AGGround
	case 5:
		m.AirGround = AGAirborne
	}
}

// decodeFlightStatus handles FS, DR and UM of DF4/5/20/21.
func (m *Message) decodeFlightStatus() {
	r := &m.Raw
	m.FS = r[0] & 7
	m.DR = r[1] >> 3
	m.UM = (r[1]&7)<<3 | r[2]>>5
	m.Valid |= FFlightStatus
	switch m.FS {
	case 0:
		m.AirGround = AGUncertain
	case 1:
		m.AirGround = AGGround
	case 2:
		m.Alert = true
		m.AirGround = AGUncertain
	case 3:
		m.Alert = true
		m.AirGround = AGGround
	case 4:
		m.Alert = true
		m.SPI = true
	case 5:
		m.SPI = true
	default:
		return // 6, 7: reserved
	}
	m.Valid |= FAlertSPI
}

// decodeAirAir handles the fields shared by DF0 and DF16.
func (m *Message) decodeAirAir() {
	r := &m.Raw
	m.VS = r[0]&0x04 != 0
	m.CC = m.DF == 0 && r[0]&0x02 != 0
	m.SL = r[1] >> 5
	m.RI = (r[1]&7)<<1 | r[2]>>7
	m.Valid |= FACAS
	if m.VS {
		m.AirGround = AGGround
	} else {
		m.AirGround = AGUncertain
	}
	m.setAC13(uint16(r[2])<<8 | uint16(r[3]))
}

// setAC13 stores a 13-bit altitude code.
func (m *Message) setAC13(f uint16) {
	v, kind := decodeAC13(f)
	switch kind {
	case altQ25:
		m.AltBaro = v
		m.Valid |= FAltBaro
	case altGillham:
		m.AltBaro = v
		m.AltBaroGillham = true
		m.Valid |= FAltBaro
	case altMetric:
		m.AltMetric = v
		m.Valid |= FAltMetric
	}
}

// decodeCommB runs BDS inference on the MB field of a DF20/21 reply.
func (m *Message) decodeCommB(o Options) {
	mb := m.payload()
	if mb == 0 {
		return
	}
	m.Valid |= FCommB
	if o.NoCommB {
		return
	}
	alt := unknown
	if m.Valid&FAltBaro != 0 {
		alt = float64(m.AltBaro)
	}
	m.NumBDS = uint8(inferBDS(mb, alt, &m.BDS))
	if m.NumBDS == 0 {
		return
	}
	switch best := m.BDS[0]; best.BDS {
	case BDS30:
		m.Valid |= FACASRA
	case BDS20:
		// The register number in the first byte plus eight legal characters
		// make BDS 2,0 unmistakable, so the callsign is surfaced like the one
		// of an identification squitter.
		if cs, ok := DecodeBDS20(mb); ok && best.Confidence >= 0.9 {
			m.Callsign = cs
			m.Valid |= FCallsign
		}
	}
}

// decodeDF18 interprets the control field of a DF18 squitter and decodes the
// ME field where its format is known.
func (m *Message) decodeDF18() {
	m.CF = m.Raw[0] & 7
	me := m.payload()
	switch m.CF {
	case 0: // ADS-B from a non-transponder device, ICAO address
		m.AddrType = AddrADSBICAONT
		m.Source = SourceADSB
		m.decodeES(me, false)
	case 1: // ADS-B from a non-transponder device, other address
		m.AddrType = AddrADSBOther
		m.Source = SourceADSB
		m.decodeES(me, false)
	case 2: // fine format TIS-B
		m.AddrType = AddrTISBICAO
		m.Source = SourceTISB
		m.decodeES(me, true)
	case 3: // coarse format TIS-B: only the IMF bit (ME bit 1) is interpreted
		m.AddrType = AddrTISBICAO
		m.Source = SourceTISB
		if me>>55 != 0 {
			m.setIMF()
		}
	case 5: // TIS-B relay of ADS-B using anonymous addresses
		m.AddrType = AddrTISBOther
		m.Source = SourceTISB
		m.decodeES(me, false)
	case 6: // ADS-B rebroadcast (ADS-R)
		m.AddrType = AddrADSRICAO
		m.Source = SourceADSR
		m.decodeES(me, true)
	default: // 4: TIS-B/ADS-R management, 7: reserved
		m.AddrType = AddrUnknown
		m.Source = SourceUnknown
	}
}

// setIMF records that the AA field does not hold an ICAO address.
func (m *Message) setIMF() {
	m.IMF = true
	switch m.AddrType {
	case AddrTISBICAO:
		m.AddrType = AddrTISBTrackFile
	case AddrADSRICAO:
		m.AddrType = AddrADSROther
	case AddrADSBICAO, AddrADSBICAONT:
		m.AddrType = AddrADSBOther
	}
}
