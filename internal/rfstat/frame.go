package rfstat

import (
	"math"
	"time"
)

// CRCStatus is the error-check outcome of one frame as seen by the tracker.
type CRCStatus uint8

const (
	CRCUnknown CRCStatus = iota // not checked / not reported
	CRCOK                       // DF11/17/18 parity good (DF11 incl. II/SI overlay)
	CRCFixed                    // accepted after 1- or 2-bit error correction
	CRCBad                      // failed the check, not repairable (counted, otherwise ignored)
	// CRCAddrAccepted: DF0/4/5/16/20/21 address/parity overlay whose recovered
	// address matched an aircraft the tracker already knows (accepted).
	CRCAddrAccepted
	// CRCAddrRejected: address/parity overlay frame whose recovered address
	// is unknown, i.e. most likely a garbled reply (FRUIT/garble indicator).
	CRCAddrRejected
	numCRC
)

var crcLabels = [numCRC]string{"unknown", "ok", "fixed", "bad", "addrAccepted", "addrRejected"}

// String returns the JSON label of the status.
func (c CRCStatus) String() string {
	if c < numCRC {
		return crcLabels[c]
	}
	return "unknown"
}

// Kind classifies the content of a frame.
type Kind uint8

const (
	KindOther          Kind = iota // anything not listed below (TC 23-27/30, DF24 ...)
	KindPosEven                    // airborne position (TC 9-18, 20-22), CPR even
	KindPosOdd                     // airborne position, CPR odd
	KindSurfEven                   // surface position (TC 5-8), even
	KindSurfOdd                    // surface position, odd
	KindVelocity                   // TC 19 airborne velocity
	KindIdent                      // TC 1-4 identification and category
	KindAircraftStatus             // TC 28 (emergency / ACAS RA broadcast)
	KindTargetState                // TC 29 target state and status
	KindOpStatus                   // TC 31 operational status
	KindSurvAlt                    // DF4 surveillance altitude reply
	KindSurvIdent                  // DF5 surveillance identity reply
	KindCommBAlt                   // DF20 Comm-B altitude reply (Frame.BDS)
	KindCommBIdent                 // DF21 Comm-B identity reply (Frame.BDS)
	KindAllCall                    // DF11 all-call reply / acquisition squitter (Frame.IID)
	KindACASShort                  // DF0 short air-air surveillance (ACAS)
	KindACASLong                   // DF16 long air-air surveillance (ACAS, Frame.RA)
	KindModeAC                     // Mode A/C reply (2 bytes, DF = DFModeAC)
	numKind
)

// DFModeAC is the Frame.DF value used for Mode A/C replies.
const DFModeAC = 32

// Frame is one received frame as handed over by the tracker. All fields are
// plain values; unknown floats are NaN, unknown small ints are -1.
//
// Use NewFrame to get a value with every optional field set to "unknown".
type Frame struct {
	Recv  time.Time // host receive time
	GPSNs int64     // receiver GPS timestamp, ns since UTC midnight; -1 = none
	// Signal in dBFS (<= 0). NaN when unknown.
	Signal float64
	// Feed is a small feed index (0..MaxFeeds-1) chosen by the integrator; -1 = unknown.
	Feed int8
	// Antenna is the index into Config.Antennas (0..MaxAntennas-1); -1 = unknown/combined.
	Antenna int8

	DF   uint8 // downlink format 0..24, DFModeAC for Mode A/C
	CRC  CRCStatus
	ICAO uint32 // 24-bit address (bit 24 = non-ICAO flag allowed); 0 = none
	Kind Kind
	TC   uint8 // ADS-B type code (DF17/18), 0 otherwise
	BDS  uint8 // Comm-B register for DF20/21 as 0xXY (0x40 = BDS 4,0); 0 = unknown
	IID  uint8 // DF11 interrogator overlay 0..79 exactly as modes.Message.IID (0-15 = II, 16-79 = SI 0-63)
	RA   bool  // ACAS resolution advisory active (DF16 / BDS 3,0 / TC28 RA broadcast)

	// Geometry relative to the receiver from the tracker's current state.
	DistKm     float64 // ground distance; NaN unknown
	BearingDeg float64 // from receiver to aircraft; NaN unknown
	ElevDeg    float64 // elevation angle seen from the antenna; NaN unknown
	AltFt      float64 // barometric altitude; NaN unknown
	TrackDeg   float64 // aircraft ground track (for aspect); NaN unknown
	Lat, Lon   float64 // last decoded position (for jump detection); NaN unknown

	// Equipage (last known values for the aircraft; -1 = unknown).
	Version  int8  // ADS-B version 0/1/2
	NIC      int8  // 0..11
	NACp     int8  // 0..11
	SIL      int8  // 0..3
	Category uint8 // emitter category 0xA0..0xD7 style (0xA3 = A3); 0 = unknown
	OnGround bool
	// GNSSDeltaFt = GNSS (geometric) height minus baro altitude from TC19; NaN unknown.
	GNSSDeltaFt float64
	// GNSSAlt is true when this frame carried a GNSS altitude (TC 20-22).
	GNSSAlt bool
	// Implausible is set by the tracker when it rejected this frame's position
	// as a jump / implausible (rfstat also runs its own speed check).
	Implausible bool
}

// NewFrame returns a Frame with all optional fields set to unknown.
func NewFrame(recv time.Time) Frame {
	n := math.NaN()
	return Frame{
		Recv: recv, GPSNs: -1, Signal: n, Feed: -1, Antenna: -1,
		DistKm: n, BearingDeg: n, ElevDeg: n, AltFt: n, TrackDeg: n, Lat: n, Lon: n,
		Version: -1, NIC: -1, NACp: -1, SIL: -1, GNSSDeltaFt: n,
	}
}

// longDF reports whether a DF is a 112-bit reply.
func longDF(df uint8) bool { return df >= 16 && df < DFModeAC }

// Reply air times in microseconds: 8 us preamble + 56 or 112 us data block
// at 1 Mbit/s PPM; a Mode A/C reply spans F1..F2 = 20.3 us.
const (
	airShortUs = 64.0
	airLongUs  = 120.0
	airModeAC  = 20.3
)
