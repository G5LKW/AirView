# modes - API notes (from the package author)

import "airview/internal/modes" - pure functions, safe for concurrent use, no allocation in Decode / DecodeOpts / DisambiguateBDS / CPR functions (asserted by TestIndepNoAllocs). Signatures are unchanged from the author's report; behaviour changes are marked [changed].

DECODE
- func Decode(frame []byte, m *Message) error - 7 or 14 bytes; m fully overwritten; frame copied into m.Raw. No error correction.
- func DecodeOpts(frame, m, Options{FixErrors int /*0 none, 1 = 1-bit DF17+DF18, 2 = also 2-bit DF17 only; DF bits never repaired; <0 and >2 are clamped*/, NoCommB bool}) error
- func DecodeFrame(frame []byte) (Message, error); func DecodeHex(s string) (Message, error)
- Errors: ErrFrameLength, ErrBadCRC, ErrUnsupportedDF. On ErrBadCRC / ErrUnsupportedDF m.DF, m.Raw, m.Len, m.Residual are set and m.Valid is 0. DF24..31 are reported as DF 24. A frame whose DF bits are damaged decodes as that other format (e.g. DF17 with bit 2 flipped is accepted as Comm-D with a parity address).

MESSAGE (no pointers; read an optional field only if its bit is in m.Valid; m.Has(f) tests all bits of f)
- Always: Raw [14]byte, Len, DF, ICAO uint32, AddrType, Source, IMF, AddressFromParity, CRC CRCStatus, Residual (of the frame as received), FixedBits, FixedBit [2]uint8, AirGround.
- CRCStatus: CRCUnknown, CRCOK, CRCInterrogator (DF11 residual 1..79), CRCAddressParity (DF0/4/5/16/20/21/24), CRCFixed1, CRCFixed2, CRCBad. m.AddressCorrected().
- AddrType: AddrUnknown, AddrModeS, AddrADSBICAO, AddrADSBICAONT, AddrADSBOther, AddrTISBICAO, AddrTISBTrackFile, AddrTISBOther, AddrADSRICAO, AddrADSROther; .IsICAO(), .Label() ("icao"|"non-icao"|"tisb-track"|"anon"), .String().
- Source: SourceUnknown/ModeS/ADSB/TISB/ADSR; .Label() gives model.Src*.
- AirGround: AGUnknown, AGGround, AGAirborne (CA=5 only), AGUncertain.
- m.AddrKey() = ICAO | NonICAOFlag (1<<24) for non-ICAO addresses.
- Fields bits: FCapability(CA) FFlightStatus(FS,DR,UM) FAlert FSPI FIID(IID; m.Interrogator() -> code, si, ok) FACAS(VS,CC,SL,RI) FTypeCode(TC,Subtype) FCallsign(Callsign [8]byte) FCategory(0xA3) FSquawk(0x7500) FAltBaro(AltBaro ft, AltBaroGillham) FAltMetric FAltGNSS(ft, encoding unverified) FGNSSDelta(ft, GNSS minus baro) FCPR(CPRLat,CPRLon 17-bit, CPROdd, CPRSurface, CPRTime) FSurvStatus FNICSuppB FGroundSpeed(GS kt) FTrack FHeading FAirspeed(Airspeed, AirspeedTAS) FVertRate(VertRate fpm, VertRateBaro) FNACv FEmergency(0..7) FSelAlt(SelAlt, SelAltFMS) FBaroSetting(hPa) FSelHeading(SelHeading, SelHeadingIsTrack) FNavModes(NavAutopilot|NavVNAV|NavAltHold|NavApproach|NavLNAV|NavTCAS; .AppendNames(dst)) FNACp FSIL FSILType(SILPerSample) FNICBaro FVersion FNICSuppA FNICSuppC FGVA FSDA FHRD(HRDMagnetic) FTrackIsHeading FLengthWidth FOpStatus(CapClass,OpMode) FACASRA FCommB. FAlertSPI = FAlert|FSPI.
- Methods: ME()/MB() uint64 (56-bit payload, bit 1 = bit 55), Long(), ICAOString(), CallsignString(), SquawkString(), CategoryString(), EmergencyString(), NICRc(version, nicA, nicC), TCASOperational(), TCASRAActive(), IdentActive(), ACASRA() (ACASRA, ok), BestBDS().
- Emergency constants EmergencyNone..EmergencyReserved (0..7).

CRC
- Checksum(data) uint32; Residual(frame) uint32 (0 for good DF17/18, IID for DF11, address for parity-overlay DFs; 0xFFFFFFFF for frames under 4 bytes); SetParity(frame, overlay).

INTEGRITY
- NICRc(tc uint8, version int, nicA, nicB, nicC bool) (nic int, rcM float64, ok bool) - rcM 0 = unknown; pass version 0 when unknown. NUCp(tc) (nucp, ok); NACpEPU(nacp) metres; NACvHFOM(nacv) m/s.

CPR (raw 17-bit values in; degrees out, lon in [-180,180))
- CPRAirborneGlobal(evenLat, evenLon, oddLat, oddLon uint32, latestOdd bool) (lat, lon, ok) - pair <= 10 s old, same aircraft. ok=false on NL mismatch, latitude out of range, [changed] or any raw value > 0x1FFFF.
- CPRAirborneLocal(refLat, refLon, cprLat, cprLon, odd) - reference within 180 NM. ok=false when the reference is not a real position, the result is more than half a zone away, [changed] or a raw value > 0x1FFFF. [changed] references on a zone boundary now decode.
- CPRSurfaceGlobal(refLat, refLon, evenLat, evenLon, oddLat, oddLon, latestOdd) - the reference picks the quadrant (nearest solution; latitude has only two candidates, so it may be more than 45 degrees from a bad reference). [changed] ok=false for NaN / Inf / out-of-range reference or raw values > 0x1FFFF; an all-zero latitude resolves to +-90 when the reference is beyond +-45.
- CPRSurfaceLocal(refLat, refLon, cprLat, cprLon, odd) - reference within 45 NM; same validation as CPRAirborneLocal.
- NL(lat) int (1 for NaN / Inf / beyond 87); CPREncode(lat, lon, odd, surface) (cprLat, cprLon) [changed: (0,0) for NaN / Inf].

COMM-B
- type BDS uint8 (0x40 = BDS 4,0): BDSNone, BDS10, BDS17, BDS20, BDS30, BDS40, BDS44, BDS45, BDS50, BDS60; .String() "4,0".
- Decode fills m.BDS [MaxBDS=6]BDSCandidate{BDS, Confidence float32} best first, m.NumBDS. A BDS 2,0 with confidence >= 0.9 also sets Callsign / FCallsign; a best candidate of BDS 3,0 sets FACASRA.
- InferBDS(mb uint64, altFt float64 /*NaN unknown*/) (c [MaxBDS]BDSCandidate, n int)
- DisambiguateBDS(m *Message, gs, track, altFt float64 /*NaN = unknown*/) (b BDS, confidence float32, ok bool) - ok=false means ignore the reply (winner < 0.5 or lead < 0.15). RankBDS(m, gs, track, altFt) returns all re-scored candidates. m.BestBDS() is the same decision from the message alone.
- Register decoders on m.MB(): DecodeBDS10 -> DataLinkCapability; DecodeBDS17 -> GICBCapability (.Supports(b), .AppendRegisters(dst)); DecodeBDS20 -> ([8]byte, ok); DecodeACASRA -> ACASRA (.Active/.Corrective/.Downward/.IncreasedRate/.SenseReversal/.AltitudeCrossing/.Positive); DecodeBDS40 -> SelectedVerticalIntention{MCPAltOK, MCPAlt, FMSAltOK, FMSAlt, BaroOK, Baro, ModesOK, VNAV, AltHold, Approach, SourceOK, Source} (.NavModes()); DecodeBDS44 -> MeteoRoutine{FOM, WindOK, WindSpeed, WindDir, Temp (0.25 C), TempAlt (0.125 C), PressureOK, Pressure, TurbulenceOK, Turbulence, HumidityOK, Humidity}; DecodeBDS45 -> MeteoHazard; DecodeBDS50 -> TrackAndTurn{RollOK, Roll, TrackOK, Track, GSOK, GS, TrackRateOK, TrackRate, TASOK, TAS}; DecodeBDS60 -> HeadingAndSpeed{HeadingOK, MagHeading, IASOK, IAS, MachOK, Mach, BaroRateOK, BaroRate, InertialRateOK, InertialRate} [changed: rates are plain two's complement x 32 ft/min, 0x3FF = -32].
- OverlayAddress(parityAddr, bds).

HELPERS
- FormatICAO(addr) ("~" prefix when NonICAOFlag), AppendICAO(dst, addr), ParseICAO(s) (uint32, error; ErrBadICAO), TrimCallsign([8]byte), FormatSquawk / AppendSquawk, CategoryName(0xA3) "A3", EmergencyName(state), SquawkEmergency(squawk), DFName(df), FrameDF(frame), FrameLen(df), GillhamAltitude(modeA) (ft, ok).
- Stats: DFIndex(df) int in 0..NumDFIndex-1 (DFIndex0, 4, 5, 11, 16, 17, 18, 20, 21, DFIndexOther); DFIndexLabel(i) "df17" / "df_other".
- Encoders for sim / tests: EncodeGillham, EncodeID13, EncodeAC13, EncodeAC12, EncodeCallsign, CPREncode, SetParity.

FILES TOUCHED (all under D:\AirView\internal\modes)
- cpr.go, commb.go (fixes); commb_test.go (one wrong expectation corrected)
- new: vectors_independent_test.go, synth_test.go (the earlier pyModeS-derived golden corpus, golden_pymodes_test.go and testdata\pymodes_golden.txt, was deleted; see the provenance note at the top of synth_test.go)

VERIFICATION RESULT
- go test ./internal/modes/... and go vet ./internal/modes/... pass: 108 tests, 0 failures; benchmarks still 0 allocs/op.
- Passed first time (the frames are self-generated with the package's encoders, plus the worked examples of "The 1090 Megahertz Riddle"): DF, ICAO (incl. parity recovery for DF20/21), type code, CRC, altitude, CPR format and raw fields, squawk, callsign, category, TC19 ground speed / track / vertical rate, BDS 2,0 / 4,0 / 5,0 fields and register choice (363 / 363); CRC residuals; Gillham table; 25 ft and negative altitudes; DF4/5/11 fields incl. SI code; all four TC19 subtypes and sign cases; surface movement table; TC28 / TC29 / TC31 fields; NIC / Rc / NUCp / NACp / NACv tables; NL table at every boundary; self-generated and book CPR vectors for airborne global / local and surface quadrants; 1- and 2-bit repair; never-panic sweep over about 700,000 random, truncated and structured frames.
- Failed first time and fixed: BDS 6,0 rates, the local-CPR zone-boundary vector, surface-CPR pole vectors, NaN / Inf / out-of-range reference and over-wide raw CPR values.
