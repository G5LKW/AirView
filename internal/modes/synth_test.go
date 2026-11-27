package modes

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Test vector provenance.
//
// Every frame in this package's tests is one of two kinds:
//
//  1. Self-generated. The field values are made up in the test, placed into
//     the bit layout of ICAO Annex 10 Vol. IV / ICAO Doc 9871 / RTCA DO-260B
//     with this package's field encoders (encode.go, CPREncode) or plain bit
//     placement (setBits, the builders below), and closed with SetParity.
//     The expected decoded values are those inputs, or follow from them by
//     the arithmetic written out next to the test.
//  2. A worked example of "The 1090 Megahertz Riddle" (Junzi Sun, 2nd ed.,
//     TU Delft OPEN Publishing, 2021; https://mode-s.org/1090mhz/). These are
//     the book* constants below; the chapter is cited where they are used.
//
// No frame or expected value is taken from another decoder's test suite or
// sample data.

// Worked examples of "The 1090 Megahertz Riddle".
const (
	bookIdent     = "8D4840D6202CC371C32CE0576098" // ADS-B identification chapter: KLM1023, category A0
	bookPosEven   = "8D40621D58C382D690C8AC2863A7" // airborne position chapter: even frame, 38000 ft
	bookPosOdd    = "8D40621D58C386435CC412692AD6" // airborne position chapter: odd frame
	bookVelGS     = "8D485020994409940838175B284F" // airborne velocity chapter, message A (subtype 1)
	bookVelAS     = "8DA05F219B06B6AF189400CBC33F" // airborne velocity chapter, message B (subtype 3)
	bookSurf      = "8C4841753A9A153237AEF0F275BE" // surface position chapter: 17 kt, track 92.8125
	bookSurfEven  = "8C4841753AAB238733C8CD4020B1" // surface position chapter: even frame
	bookSurfOdd   = "8C4841753A8A35323FAEBDAC702D" // surface position chapter: odd frame
	bookAllCall   = "5D484FDEA248F5"               // all-call reply chapter: 484FDE, CA 5, SI code 6
	bookDF4       = "2000171806A983"               // surveillance reply chapter: altitude reply, 36000 ft
	bookDF5       = "2A00516D492B80"               // surveillance reply chapter: identity reply, squawk 0356
	bookParity    = "A0001838CA380031440000F24177" // Mode S basics: address 3C6DD0 recovered from the parity
	bookBDS17     = "A0000638FA81C10000000081A92F" // ELS chapter: BDS 1,7 capability report
	bookBDS20     = "A000083E202CC371C31DE0AA1CCF" // EHS / ELS chapter: BDS 2,0, KLM1017
	bookBDS40     = "A8001EBCAEE57730A80106DE1344" // EHS chapter: BDS 4,0, 24000 / 24000 ft, 1013.2 hPa
	bookBDS44     = "A0001692185BD5CF400000DFC696" // meteorological chapter: BDS 4,4
	bookBDS45     = "A00016920001CF400000008BD87B" // meteorological chapter: BDS 4,5
	bookBDS50     = "A80006ACF9363D3BBF9CE98F1E1D" // EHS chapter: BDS 5,0
	bookBDS60     = "A80004AAA74A072BFDEFC1D5CB4F" // EHS chapter: BDS 6,0
	bookInference = "A0001838E519F33160240142D7FA" // BDS inference chapter: "should be BDS60"
)

// fhex formats a frame as upper case hex.
func fhex(f []byte) string { return strings.ToUpper(hex.EncodeToString(f)) }

// survFrame builds a 56-bit reply from its first 32 bits with overlay (the
// address, an interrogator code or 0) on the parity.
func survFrame(header uint32, overlay uint32) []byte {
	f := []byte{byte(header >> 24), byte(header >> 16), byte(header >> 8), byte(header), 0, 0, 0}
	SetParity(f, overlay)
	return f
}

// df0Frame: DF0 short air-air surveillance. VS bit 6, CC bit 7, SL bits
// 9-11, RI bits 14-17, AC bits 20-32 (Annex 10 Vol. IV 3.1.2.8.2).
func df0Frame(vs, cc bool, sl, ri uint8, ac13 uint16, icao uint32) []byte {
	return survFrame(uint32(b2u(vs))<<26|uint32(b2u(cc))<<25|uint32(sl)<<21|uint32(ri)<<15|uint32(ac13), icao)
}

// df4Frame / df5Frame: surveillance replies, FS bits 6-8, DR 9-13, UM 14-19,
// AC / ID bits 20-32.
func df4Frame(fs, dr, um uint8, ac13 uint16, icao uint32) []byte {
	return survFrame(4<<27|uint32(fs)<<24|uint32(dr)<<19|uint32(um)<<13|uint32(ac13), icao)
}

func df5Frame(fs, dr, um uint8, squawk uint16, icao uint32) []byte {
	return survFrame(5<<27|uint32(fs)<<24|uint32(dr)<<19|uint32(um)<<13|uint32(EncodeID13(squawk)), icao)
}

// df11Frame: all-call reply, CA bits 6-8, AA bits 9-32, parity overlaid with
// the interrogator identifier.
func df11Frame(ca uint8, icao, iid uint32) []byte {
	return survFrame(11<<27|uint32(ca)<<24|icao, iid)
}

// df16Frame: long air-air surveillance with an MV field.
func df16Frame(vs bool, sl, ri uint8, ac13 uint16, mv uint64, icao uint32) []byte {
	f := make([]byte, 14)
	h := uint32(16)<<27 | uint32(b2u(vs))<<26 | uint32(sl)<<21 | uint32(ri)<<15 | uint32(ac13)
	f[0], f[1], f[2], f[3] = byte(h>>24), byte(h>>16), byte(h>>8), byte(h)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(mv >> (48 - 8*uint(i)))
	}
	SetParity(f, icao)
	return f
}

func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// identME builds an identification ME field (TC1-4).
func identME(tc, cat uint8, cs string) uint64 {
	v, ok := EncodeCallsign(cs)
	if !ok {
		panic("bad callsign " + cs)
	}
	return uint64(tc)<<51 | uint64(cat)<<48 | v
}

// posME builds an airborne position ME field from an altitude in feet and a
// position, with the 25 ft altitude code and the CPR encoder.
func posME(tc uint8, altFt int32, odd bool, lat, lon float64) uint64 {
	ac12, ok := EncodeAC12(altFt)
	if !ok {
		panic("altitude out of range")
	}
	clat, clon := CPREncode(lat, lon, odd, false)
	return airbornePosME(tc, 0, false, ac12, false, odd, clat, clon)
}

// surfME builds a surface position ME field (TC5-8): movement code, track
// status and 7-bit track, CPR of a position.
func surfME(tc uint8, mov uint8, trkOK bool, trk uint8, odd bool, lat, lon float64) uint64 {
	clat, clon := CPREncode(lat, lon, odd, true)
	me := uint64(tc)<<51 | uint64(mov)<<44 | uint64(trk&0x7F)<<36 | uint64(clat)<<17 | uint64(clon)
	if trkOK {
		me |= 1 << 43
	}
	if odd {
		me |= 1 << 34
	}
	return me
}

// bds10MB builds a BDS 1,0 payload (ICAO Doc 9871 Table A-2-16) from its
// fields, bit by bit.
func bds10MB(c DataLinkCapability) uint64 {
	mb := uint64(0x10) << 48
	set := func(bit uint, on bool) {
		if on {
			mb = setBits(mb, bit, bit, 1)
		}
	}
	set(9, c.Continuation)
	set(15, c.OverlayCommand)
	set(16, c.ACASOperational)
	mb = setBits(mb, 17, 23, uint64(c.SubnetworkVersion))
	set(24, c.EnhancedProtocol)
	set(25, c.SpecificServices)
	mb = setBits(mb, 26, 28, uint64(c.UplinkELM))
	mb = setBits(mb, 29, 32, uint64(c.DownlinkELM))
	set(33, c.AircraftIDCapable)
	set(34, c.SquitterCapable)
	set(35, c.SICapable)
	set(36, c.GICBCapabilityRep)
	set(37, c.ACASHybridSurv)
	set(38, c.ACASRACapable)
	mb = setBits(mb, 39, 40, uint64(c.ACASVersion))
	return setBits(mb, 41, 56, uint64(c.DTE))
}

// bds20MB builds a BDS 2,0 payload.
func bds20MB(cs string) uint64 {
	v, ok := EncodeCallsign(cs)
	if !ok {
		panic("bad callsign " + cs)
	}
	return 0x20<<48 | v
}

// Self-generated BDS 1,0 contents used by several tests.
var (
	ownBDS10a = DataLinkCapability{ACASOperational: true, SubnetworkVersion: 3, SpecificServices: true, AircraftIDCapable: true,
		SquitterCapable: true, SICapable: true, GICBCapabilityRep: true, ACASRACapable: true, ACASVersion: 1}
	ownBDS10b = DataLinkCapability{OverlayCommand: true, ACASOperational: true, SubnetworkVersion: 5, SpecificServices: true,
		AircraftIDCapable: true, SquitterCapable: true, SICapable: true, GICBCapabilityRep: true, ACASRACapable: true, ACASVersion: 2}
)

// Self-generated Comm-B replies used by several tests. Payload values are
// on the field grids, so they decode exactly.
var (
	ownBDS10   = fhex(commBFrame(21, EncodeID13(0x6000), 0x4D2B3C, bds10MB(ownBDS10a)))
	ownBDS10v5 = fhex(commBFrame(20, ac13For(39000), 0x4D2B3C, bds10MB(ownBDS10b)))
	ownBDS20   = fhex(commBFrame(20, ac13For(34000), 0x4B3A21, bds20MB("SWR93KE")))
	ownBDS20b  = fhex(commBFrame(21, EncodeID13(0x4372), 0x89A1C3, bds20MB("AFR1745")))
	ownBDS20c  = fhex(commBFrame(20, ac13For(3000), 0x406E55, bds20MB("EIN6B")))
	ownBDS40   = fhex(commBFrame(20, ac13For(36975), 0x400A77, bds40MB(37008, -1, 1021.6)))
	ownBDS40b  = fhex(commBFrame(20, ac13For(2950), 0x406E55, bds40MB(4000, 4000, 1019.2)))
	ownBDS50   = fhex(commBFrame(20, ac13For(33000), 0x3C5EF1, bds50MB(-4.21875, 287.578125, 446, -0.3125, 452)))
	ownBDS60   = fhex(commBFrame(20, ac13For(5000), 0x48C1D0, bds60MB(118.4765625, 258, 0.428, -1600, -1632)))
)

// Self-generated BDS 5,0 payloads that also pass every BDS 6,0 rule when
// no altitude is known (found by a seeded search over bds50MB inputs; the
// inputs are the expected 5,0 values).
var (
	// GS 336 kt, track 240.5; as 6,0: heading 1.2, IAS 344, Mach 0.672.
	ownAmbig1 = bds50MB(0.52734375, 240.46875, 336, -0.59375, 372)
	// GS 350 kt, track 221.0; as 6,0: heading 3.7, IAS 233, Mach 0.700.
	ownAmbig2 = bds50MB(1.7578125, 220.95703125, 350, 0.46875, 310)
	// GS 220 kt, track 216.2, TAS 228; as 6,0: heading 355.25, IAS 206,
	// Mach 0.44, baro rate +256 next to inertial rate +3648.
	ownAmbig3 = bds50MB(-2.4609375, 216.2109375, 220, 0.25, 228)
)

// ownTSSME is a TC29 subtype 1 target state message (DO-260B
// 2.2.3.2.7.1): selected altitude field 751 -> 750*32 = 24000 ft, QNH field
// 267 -> 800+266*0.8 = 1012.8 hPa, heading field 95 -> 95*180/256 =
// 66.796875, NACp 9, NICbaro, SIL 3, modes autopilot, VNAV, TCAS, LNAV.
var ownTSSME = func() uint64 {
	me := uint64(29)<<51 | 1<<49
	me = setBits(me, 10, 20, 751)
	me = setBits(me, 21, 29, 267)
	me = setBits(me, 30, 30, 1)
	me = setBits(me, 31, 39, 95)
	me = setBits(me, 40, 43, 9)
	me = setBits(me, 44, 44, 1)
	me = setBits(me, 45, 46, 3)
	for _, b := range []uint{47, 48, 49, 53, 54} {
		me = setBits(me, b, b, 1)
	}
	return me
}()

var (
	ownTSS      = esHex(0x8D, 0xA05A29, ownTSSME)
	ownSurfDF18 = esHex(0x90, 0x3A2B1C, surfME(7, 30, true, 20, true, 43.62650, 1.37480))
	ownDF0      = fhex(df0Frame(false, true, 7, 3, ac13For(45000), 0x0D1A7E))
	// Airborne operational status, version 2 (DO-260B 2.2.3.2.7.2):
	// capability class bit 11 = TCAS operational, SDA 3 (bits 31-32), NIC
	// supplement A, NACp 10, GVA 2, SIL 3 per hour, NICbaro, true north.
	ownOpStatus = esHex(0x8D, 0x0D1A7E, func() uint64 {
		me := uint64(31) << 51
		me = setBits(me, 11, 11, 1)
		me = setBits(me, 31, 32, 3)
		me = setBits(me, 41, 43, 2)
		me = setBits(me, 44, 44, 1)
		me = setBits(me, 45, 48, 10)
		me = setBits(me, 49, 50, 2)
		me = setBits(me, 51, 52, 3)
		return setBits(me, 53, 53, 1)
	}())
)

// More self-generated frames for the benchmarks and allocation tests.
var (
	ownDF0Gillham = fhex(df0Frame(false, false, 0, 0, EncodeID13(gillhamEncode(10600)), 0xAB12C3))
	ownDF16       = fhex(df16Frame(false, 7, 3, ac13For(35000), 0x58A1C3D2E4F506, 0xAC12E9))
	ownAmbigDF21  = fhex(commBFrame(21, EncodeID13(0x5402), 0x3C7A90, ownAmbig1))
	ownStatus     = esHex(0x8D, 0xA2C1B7, 28<<51|1<<48|uint64(EncodeID13(0x3415))<<32)
	ownADSRIdent  = esHex(0x96, 0xA0821B, identME(4, 1, "N905XT"))
	ownDF24       = func() string {
		f := []byte{0xDA, 0xCA, 0x7F, 0x82, 0x61, 0x3C, 0x2D, 0xB1, 0x4A, 0x49, 0xC5, 0, 0, 0}
		SetParity(f, 0x5C3D11)
		return fhex(f)
	}()
)

// esHex is esFrame as a hex string.
func esHex(first byte, icao uint32, me uint64) string { return fhex(esFrame(first, icao, me)) }

// TestSynthBuilders checks the builders against hand-placed bits, so that a
// mistake in a builder cannot hide behind a matching mistake in the decoder.
func TestSynthBuilders(t *testing.T) {
	// DF11, CA 5, address 3A5C71, II 0: first byte 01011 101 = 0x5D.
	if f := df11Frame(5, 0x3A5C71, 0); f[0] != 0x5D || f[1] != 0x3A || f[2] != 0x5C || f[3] != 0x71 || Residual(f) != 0 {
		t.Errorf("df11Frame: % X", f)
	}
	// DF4, FS 1, DR 0, UM 0, AC 0x1A2B: 00100 001 | 00000 000 | 000 1 1010 | 0010 1011.
	if f := df4Frame(1, 0, 0, 0x1A2B&0x1FFF, 0x123456); f[0] != 0x21 || f[1] != 0x00 || f[2] != 0x1A || f[3] != 0x2B || Residual(f) != 0x123456 {
		t.Errorf("df4Frame: % X", f)
	}
	// DF0, VS 1, CC 1, SL 5, RI 9, AC 0: 00000 1 1 0 | 101 00 100 | 1 000 0000 | 0.
	if f := df0Frame(true, true, 5, 9, 0, 1); f[0] != 0x06 || f[1] != 0xA4 || f[2] != 0x80 || f[3] != 0x00 {
		t.Errorf("df0Frame: % X", f)
	}
	// identME: TC 4, category 3 -> ME byte 0 = 00100 011.
	if me := identME(4, 3, "AB"); me>>48 != 0x23 || me&0xFFFFFFFFFFFF != 1<<42|2<<36|0x820820820&0xFFFFFFFFF {
		t.Errorf("identME: %014X", me)
	}
}
