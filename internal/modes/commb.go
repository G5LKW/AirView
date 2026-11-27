package modes

// BDS identifies a Comm-B data selector register as a hex pair:
// 0x40 is BDS 4,0.
type BDS uint8

// Registers this package can recognise and decode.
const (
	BDSNone BDS = 0
	BDS10   BDS = 0x10 // data link capability report
	BDS17   BDS = 0x17 // common usage GICB capability report
	BDS20   BDS = 0x20 // aircraft identification
	BDS30   BDS = 0x30 // ACAS active resolution advisory
	BDS40   BDS = 0x40 // selected vertical intention
	BDS44   BDS = 0x44 // meteorological routine air report
	BDS45   BDS = 0x45 // meteorological hazard report
	BDS50   BDS = 0x50 // track and turn report
	BDS60   BDS = 0x60 // heading and speed report
)

// String formats the register as "4,0".
func (b BDS) String() string {
	const hex = "0123456789ABCDEF"
	return string([]byte{hex[b>>4], ',', hex[b&15]})
}

// MaxBDS is the capacity of Message.BDS.
const MaxBDS = 6

// BDSCandidate is one possible interpretation of a Comm-B payload.
type BDSCandidate struct {
	BDS BDS
	// Confidence is a heuristic score in (0, 1]: >= 0.8 strong, 0.5..0.8
	// plausible, below 0.5 weak (do not use without corroboration).
	Confidence float32
}

// signedField interprets a field of the given width (sign bit first) as a
// two's complement number.
func signedField(raw uint32, width uint) int32 {
	if raw&(1<<(width-1)) != 0 {
		return int32(raw) - int32(1)<<width
	}
	return int32(raw)
}

// wrongStatus reports a status bit that is clear while its data bits
// first..last are not all zero - the signature of a payload that is not the
// register being tested.
func wrongStatus(mb uint64, status, first, last uint) bool {
	return !flag(mb, status) && field(mb, first, last) != 0
}

// ---------------------------------------------------------------- BDS 1,0

// DataLinkCapability is the content of BDS 1,0.
type DataLinkCapability struct {
	Continuation      bool   // bit 9: report continues in registers 1,1..1,6
	OverlayCommand    bool   // bit 15: overlay command capability (data parity)
	ACASOperational   bool   // bit 16
	SubnetworkVersion uint8  // bits 17-23: Mode S subnetwork version number
	EnhancedProtocol  bool   // bit 24: level 5 transponder
	SpecificServices  bool   // bit 25: Mode S specific services capability
	UplinkELM         uint8  // bits 26-28
	DownlinkELM       uint8  // bits 29-32
	AircraftIDCapable bool   // bit 33: BDS 2,0 available
	SquitterCapable   bool   // bit 34
	SICapable         bool   // bit 35: surveillance identifier code capability
	GICBCapabilityRep bool   // bit 36: BDS 1,7 has changed / is available
	ACASHybridSurv    bool   // bit 37
	ACASRACapable     bool   // bit 38: TA and RA generation
	ACASVersion       uint8  // bits 39-40: 0 DO-185, 1 DO-185A, 2 DO-185B, 3 reserved
	DTE               uint16 // bits 41-56: data terminal equipment status
}

// DecodeBDS10 decodes a BDS 1,0 payload (Message.MB).
func DecodeBDS10(mb uint64) DataLinkCapability {
	return DataLinkCapability{
		Continuation:      flag(mb, 9),
		OverlayCommand:    flag(mb, 15),
		ACASOperational:   flag(mb, 16),
		SubnetworkVersion: uint8(field(mb, 17, 23)),
		EnhancedProtocol:  flag(mb, 24),
		SpecificServices:  flag(mb, 25),
		UplinkELM:         uint8(field(mb, 26, 28)),
		DownlinkELM:       uint8(field(mb, 29, 32)),
		AircraftIDCapable: flag(mb, 33),
		SquitterCapable:   flag(mb, 34),
		SICapable:         flag(mb, 35),
		GICBCapabilityRep: flag(mb, 36),
		ACASHybridSurv:    flag(mb, 37),
		ACASRACapable:     flag(mb, 38),
		ACASVersion:       uint8(field(mb, 39, 40)),
		DTE:               uint16(field(mb, 41, 56)),
	}
}

// ---------------------------------------------------------------- BDS 1,7

// GICBCapability is the content of BDS 1,7: one bit per commonly used
// register, MB bit 1 in bit 23.
type GICBCapability uint32

// gicbRegisters lists the register announced by each of MB bits 1..24.
var gicbRegisters = [24]BDS{
	0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x20, 0x21,
	0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x48, 0x50,
	0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x5F, 0x60,
}

// DecodeBDS17 decodes a BDS 1,7 payload (Message.MB).
func DecodeBDS17(mb uint64) GICBCapability {
	return GICBCapability(field(mb, 1, 24))
}

// Supports reports whether the transponder announces register b.
func (g GICBCapability) Supports(b BDS) bool {
	for i, r := range gicbRegisters {
		if r == b {
			return g&(1<<uint(23-i)) != 0
		}
	}
	return false
}

// AppendRegisters appends every announced register to dst.
func (g GICBCapability) AppendRegisters(dst []BDS) []BDS {
	for i, r := range gicbRegisters {
		if g&(1<<uint(23-i)) != 0 {
			dst = append(dst, r)
		}
	}
	return dst
}

// ---------------------------------------------------------------- BDS 2,0

// DecodeBDS20 decodes the aircraft identification of a BDS 2,0 payload. The
// callsign is space padded; ok is false when it contains characters other
// than A-Z, 0-9 and space or when the register code is not 2,0.
func DecodeBDS20(mb uint64) (callsign [8]byte, ok bool) {
	ok = decodeCallsign(mb&0xFFFFFFFFFFFF, &callsign)
	return callsign, ok && mb>>48 == 0x20
}

// ---------------------------------------------------------------- BDS 3,0

// ACASRA is an ACAS resolution advisory report, as carried by BDS 3,0, the
// TC28 subtype 2 extended squitter and the DF16 MV field.
type ACASRA struct {
	// ARA is the 14-bit active resolution advisory field, MB bit 9 in bit 13.
	ARA uint16
	// RAC is the resolution advisory complement: bit 3 = do not pass below,
	// bit 2 = do not pass above, bit 1 = do not turn left, bit 0 = do not
	// turn right.
	RAC            uint8
	Terminated     bool // RAT: the advisory has ended (sent for 18 s afterwards)
	MultipleThreat bool // MTE

	// ThreatType: 0 no identity data, 1 ThreatICAO is valid, 2 the
	// altitude/range/bearing fields may be valid.
	ThreatType    uint8
	ThreatICAO    uint32
	ThreatAlt     int32   // ft
	ThreatAltOK   bool    //
	ThreatRangeNM float64 // NM; 12.7 means "more than 12.55 NM"
	ThreatRangeOK bool    //
	ThreatBearing float64 // degrees relative to own heading, centre of a 6 degree sector
	ThreatBrgOK   bool    //
}

// Active reports whether at least one advisory is in force (first ARA bit, or
// the multiple threat flag).
func (r *ACASRA) Active() bool { return r.ARA&0x2000 != 0 || r.MultipleThreat }

// Corrective reports a corrective (as opposed to preventive) advisory. Only
// meaningful when the first ARA bit is set.
func (r *ACASRA) Corrective() bool { return r.ARA&0x3000 == 0x3000 }

// Downward reports a downward sense advisory ("descend" / "do not climb").
// Only meaningful when the first ARA bit is set.
func (r *ACASRA) Downward() bool { return r.ARA&0x2800 == 0x2800 }

// IncreasedRate reports an increased rate advisory.
func (r *ACASRA) IncreasedRate() bool { return r.ARA&0x2400 == 0x2400 }

// SenseReversal reports a sense reversal advisory.
func (r *ACASRA) SenseReversal() bool { return r.ARA&0x2200 == 0x2200 }

// AltitudeCrossing reports an altitude crossing advisory.
func (r *ACASRA) AltitudeCrossing() bool { return r.ARA&0x2100 == 0x2100 }

// Positive reports a positive advisory ("climb"/"descend" rather than a
// vertical speed limit).
func (r *ACASRA) Positive() bool { return r.ARA&0x2080 == 0x2080 }

// DecodeACASRA decodes bits 9-56 of a resolution advisory payload: a BDS 3,0
// MB field, a TC28 subtype 2 ME field or a DF16 MV field.
func DecodeACASRA(payload uint64) ACASRA {
	r := ACASRA{
		ARA:            uint16(field(payload, 9, 22)),
		RAC:            uint8(field(payload, 23, 26)),
		Terminated:     flag(payload, 27),
		MultipleThreat: flag(payload, 28),
		ThreatType:     uint8(field(payload, 29, 30)),
	}
	switch r.ThreatType {
	case 1:
		r.ThreatICAO = field(payload, 31, 54)
	case 2:
		if v, kind := decodeAC13(uint16(field(payload, 31, 43))); kind == altQ25 || kind == altGillham {
			r.ThreatAlt = v
			r.ThreatAltOK = true
		}
		if rng := field(payload, 44, 50); rng != 0 {
			r.ThreatRangeNM = float64(rng-1) / 10
			if rng == 127 {
				r.ThreatRangeNM = 12.7
			}
			r.ThreatRangeOK = true
		}
		if brg := field(payload, 51, 56); brg >= 1 && brg <= 60 {
			r.ThreatBearing = float64(brg-1)*6 + 3
			r.ThreatBrgOK = true
		}
	}
	return r
}

// ACASRA returns the resolution advisory carried by the message: a DF20/21
// reply whose best register candidate is BDS 3,0, a DF17/18 TC28 subtype 2
// squitter or a DF16 reply with an RA in its MV field.
func (m *Message) ACASRA() (ra ACASRA, ok bool) {
	if m.Valid&FACASRA == 0 {
		return ra, false
	}
	return DecodeACASRA(m.payload()), true
}

// ---------------------------------------------------------------- BDS 4,0

// SelectedVerticalIntention is the content of BDS 4,0.
type SelectedVerticalIntention struct {
	MCPAltOK bool
	MCPAlt   int32 // MCP/FCU selected altitude, ft (16 ft resolution)
	FMSAltOK bool
	FMSAlt   int32 // FMS selected altitude, ft
	BaroOK   bool
	Baro     float64 // barometric pressure setting, hPa
	ModesOK  bool    // the three mode flags are populated
	VNAV     bool
	AltHold  bool
	Approach bool
	SourceOK bool
	// Source of the target altitude: 0 unknown, 1 aircraft altitude,
	// 2 MCP/FCU selected altitude, 3 FMS selected altitude.
	Source uint8
}

// DecodeBDS40 decodes a BDS 4,0 payload (Message.MB).
func DecodeBDS40(mb uint64) SelectedVerticalIntention {
	var v SelectedVerticalIntention
	if flag(mb, 1) {
		v.MCPAltOK = true
		v.MCPAlt = int32(field(mb, 2, 13)) * 16
	}
	if flag(mb, 14) {
		v.FMSAltOK = true
		v.FMSAlt = int32(field(mb, 15, 26)) * 16
	}
	if flag(mb, 27) {
		v.BaroOK = true
		v.Baro = 800 + float64(field(mb, 28, 39))*0.1
	}
	if flag(mb, 48) {
		v.ModesOK = true
		v.VNAV = flag(mb, 49)
		v.AltHold = flag(mb, 50)
		v.Approach = flag(mb, 51)
	}
	if flag(mb, 54) {
		v.SourceOK = true
		v.Source = uint8(field(mb, 55, 56))
	}
	return v
}

// NavModes returns the mode flags as a NavModes set (empty unless ModesOK).
func (v *SelectedVerticalIntention) NavModes() NavModes {
	var n NavModes
	if v.VNAV {
		n |= NavVNAV
	}
	if v.AltHold {
		n |= NavAltHold
	}
	if v.Approach {
		n |= NavApproach
	}
	return n
}

// ---------------------------------------------------------------- BDS 4,4

// MeteoRoutine is the content of BDS 4,4 (meteorological routine air report).
type MeteoRoutine struct {
	// FOM is the figure of merit / source: 0 invalid, 1 INS, 2 GNSS,
	// 3 DME/DME, 4 VOR/DME.
	FOM       uint8
	WindOK    bool
	WindSpeed float64 // kt
	WindDir   float64 // degrees true, direction the wind blows from
	// Temp is the static air temperature in °C using the 0.25 °C scaling
	// that matches observed transponders. The field has no status bit.
	Temp float64
	// TempAlt is the same field with the 0.125 °C scaling that follows from
	// the "MSB = 64 °C" reading of ICAO Doc 9871 table A-2-68.
	TempAlt      float64
	PressureOK   bool
	Pressure     float64 // average static pressure, hPa
	TurbulenceOK bool
	Turbulence   uint8 // 0 nil, 1 light, 2 moderate, 3 severe
	HumidityOK   bool
	Humidity     float64 // relative humidity, %
}

// DecodeBDS44 decodes a BDS 4,4 payload (Message.MB).
func DecodeBDS44(mb uint64) MeteoRoutine {
	v := MeteoRoutine{FOM: uint8(field(mb, 1, 4))}
	if flag(mb, 5) {
		v.WindOK = true
		v.WindSpeed = float64(field(mb, 6, 14))
		v.WindDir = float64(field(mb, 15, 23)) * (180.0 / 256.0)
	}
	t := float64(signedField(field(mb, 24, 34), 11))
	v.Temp = t * 0.25
	v.TempAlt = t * 0.125
	if flag(mb, 35) {
		v.PressureOK = true
		v.Pressure = float64(field(mb, 36, 46))
	}
	if flag(mb, 47) {
		v.TurbulenceOK = true
		v.Turbulence = uint8(field(mb, 48, 49))
	}
	if flag(mb, 50) {
		v.HumidityOK = true
		v.Humidity = float64(field(mb, 51, 56)) * (100.0 / 64.0)
	}
	return v
}

// ---------------------------------------------------------------- BDS 4,5

// MeteoHazard is the content of BDS 4,5 (meteorological hazard report).
// Hazard levels are 0 nil, 1 light, 2 moderate, 3 severe.
type MeteoHazard struct {
	TurbulenceOK  bool
	Turbulence    uint8
	WindShearOK   bool
	WindShear     uint8
	MicroburstOK  bool
	Microburst    uint8
	IcingOK       bool
	Icing         uint8
	WakeVortexOK  bool
	WakeVortex    uint8
	TempOK        bool
	Temp          float64 // static air temperature, °C
	PressureOK    bool
	Pressure      float64 // average static pressure, hPa
	RadioHeightOK bool
	RadioHeight   int32 // ft
}

// DecodeBDS45 decodes a BDS 4,5 payload (Message.MB).
func DecodeBDS45(mb uint64) MeteoHazard {
	var v MeteoHazard
	if flag(mb, 1) {
		v.TurbulenceOK = true
		v.Turbulence = uint8(field(mb, 2, 3))
	}
	if flag(mb, 4) {
		v.WindShearOK = true
		v.WindShear = uint8(field(mb, 5, 6))
	}
	if flag(mb, 7) {
		v.MicroburstOK = true
		v.Microburst = uint8(field(mb, 8, 9))
	}
	if flag(mb, 10) {
		v.IcingOK = true
		v.Icing = uint8(field(mb, 11, 12))
	}
	if flag(mb, 13) {
		v.WakeVortexOK = true
		v.WakeVortex = uint8(field(mb, 14, 15))
	}
	if flag(mb, 16) {
		v.TempOK = true
		v.Temp = float64(signedField(field(mb, 17, 26), 10)) * 0.25
	}
	if flag(mb, 27) {
		v.PressureOK = true
		v.Pressure = float64(field(mb, 28, 38))
	}
	if flag(mb, 39) {
		v.RadioHeightOK = true
		v.RadioHeight = int32(field(mb, 40, 51)) * 16
	}
	return v
}

// ---------------------------------------------------------------- BDS 5,0

// TrackAndTurn is the content of BDS 5,0.
type TrackAndTurn struct {
	RollOK      bool
	Roll        float64 // degrees, positive = right wing down
	TrackOK     bool
	Track       float64 // true track angle, degrees [0, 360)
	GSOK        bool
	GS          float64 // ground speed, kt
	TrackRateOK bool
	TrackRate   float64 // degrees per second, positive = turning right
	TASOK       bool
	TAS         float64 // true airspeed, kt
}

// DecodeBDS50 decodes a BDS 5,0 payload (Message.MB).
func DecodeBDS50(mb uint64) TrackAndTurn {
	var v TrackAndTurn
	if flag(mb, 1) {
		v.RollOK = true
		v.Roll = float64(signedField(field(mb, 2, 11), 10)) * (45.0 / 256.0)
	}
	if flag(mb, 12) {
		v.TrackOK = true
		t := float64(signedField(field(mb, 13, 23), 11)) * (90.0 / 512.0)
		if t < 0 {
			t += 360
		}
		v.Track = t
	}
	if flag(mb, 24) {
		v.GSOK = true
		v.GS = float64(field(mb, 25, 34)) * 2
	}
	if flag(mb, 35) {
		v.TrackRateOK = true
		v.TrackRate = float64(signedField(field(mb, 36, 45), 10)) * (8.0 / 256.0)
	}
	if flag(mb, 46) {
		v.TASOK = true
		v.TAS = float64(field(mb, 47, 56)) * 2
	}
	return v
}

// ---------------------------------------------------------------- BDS 6,0

// HeadingAndSpeed is the content of BDS 6,0.
type HeadingAndSpeed struct {
	HeadingOK      bool
	MagHeading     float64 // magnetic heading, degrees [0, 360)
	IASOK          bool
	IAS            float64 // indicated airspeed, kt
	MachOK         bool
	Mach           float64
	BaroRateOK     bool
	BaroRate       int32 // barometric altitude rate, ft/min
	InertialRateOK bool
	InertialRate   int32 // inertial vertical velocity, ft/min
}

// DecodeBDS60 decodes a BDS 6,0 payload (Message.MB).
func DecodeBDS60(mb uint64) HeadingAndSpeed {
	var v HeadingAndSpeed
	if flag(mb, 1) {
		v.HeadingOK = true
		h := float64(signedField(field(mb, 2, 12), 11)) * (90.0 / 512.0)
		if h < 0 {
			h += 360
		}
		v.MagHeading = h
	}
	if flag(mb, 13) {
		v.IASOK = true
		v.IAS = float64(field(mb, 14, 23))
	}
	if flag(mb, 24) {
		v.MachOK = true
		v.Mach = float64(field(mb, 25, 34)) * (2.048 / 512.0)
	}
	if flag(mb, 35) {
		v.BaroRateOK = true
		v.BaroRate = bds60Rate(field(mb, 36, 45))
	}
	if flag(mb, 46) {
		v.InertialRateOK = true
		v.InertialRate = bds60Rate(field(mb, 47, 56))
	}
	return v
}

// bds60Rate converts a 10-bit BDS 6,0 vertical rate: sign bit first, two's
// complement, 32 ft/min per bit (ICAO Doc 9871 table A-2-96).
//
// 0x3FF is therefore -32 ft/min, which is what transponders in level flight
// send about as often as 0. The out-of-range patterns 0x200 (-16384 ft/min)
// and 0x1FF (+16352 ft/min) are decoded as they stand; the register inference
// rejects any payload with a rate beyond +-6000 ft/min.
func bds60Rate(raw uint32) int32 {
	return signedField(raw&0x3FF, 10) * 32
}

// OverlayAddress undoes the BDS overlay ("data parity") some transponders
// apply to DF20/21 replies: the register number is XORed into the top byte
// of the address/parity field. Given the address recovered from parity and a
// register candidate it returns the address the aircraft would have if the
// overlay was in use.
func OverlayAddress(parityAddress uint32, b BDS) uint32 {
	return (parityAddress ^ uint32(b)<<16) & 0xFFFFFF
}
