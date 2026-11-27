// Package modes is a pure, allocation-free decoder for Mode S downlink
// formats and 1090 MHz extended squitter (ADS-B, TIS-B, ADS-R).
//
// It has no I/O, no goroutines and no global mutable state apart from two
// lazily built read-only lookup tables, so every function is safe for
// concurrent use.
//
// # Decoding
//
//	var m modes.Message
//	if err := modes.Decode(frame, &m); err != nil {
//		// errors.Is(err, modes.ErrBadCRC) -> count as crc_bad; m.DF is still set
//		return
//	}
//	if m.Has(modes.FAltBaro) { use(m.AltBaro) }
//
// Decode fills a caller supplied Message; every optional field is guarded by
// a bit in Message.Valid. The 7 or 14 byte frame is copied into Message.Raw,
// so the caller's buffer may be reused immediately.
//
// # Trust levels
//
//   - DF17/DF18: parity must be zero (CRCOK) or repairable when error
//     correction is requested via DecodeOpts (CRCFixed1 / CRCFixed2).
//   - DF11: parity residual is the interrogator code. Zero is certain (CRCOK),
//     1..79 is plausible (CRCInterrogator) and must be confirmed by the caller
//     against already known aircraft.
//   - DF0/4/5/16/20/21/24: the address is overlaid on the parity, so *any*
//     bit pattern decodes to some address. Message.AddressFromParity is set and
//     the caller must only accept addresses it has already seen in a
//     self-checking format (DF11 with zero residual, DF17, DF18).
//
// # Positions
//
// Position messages carry raw CPR values only. Use CPRAirborneGlobal,
// CPRAirborneLocal, CPRSurfaceGlobal and CPRSurfaceLocal to resolve them.
//
// # Comm-B
//
// DF20/DF21 replies do not say which BDS register they carry. Decode fills
// Message.BDS with scored candidates (InferBDS); DisambiguateBDS re-scores them
// against known aircraft state. The register contents are obtained on demand
// with DecodeBDS40, DecodeBDS50, DecodeBDS60 and friends.
//
// Bit numbering in comments follows the ICAO convention: bit 1 is the most
// significant bit of the frame, of the 56-bit ME field or of the 56-bit MB
// field, as stated.
package modes
