// Package refdata turns bare Mode S identifiers into human-readable reference
// information. Everything except the optional aircraft database importer is
// embedded in the binary and works fully offline.
//
// What is in here:
//
//   - ICAO 24-bit address -> state of registration (Lookup, CountryOf, StateOf),
//     from the ICAO Annex 10 Vol III address allocation plan.
//   - Heuristic military flag from publicly documented military address blocks
//     (IsMilitary).
//   - Registration derived algorithmically from the address for states that
//     allocate addresses sequentially (Registration, AppendRegistration).
//   - Callsign parsing: airline designator + flight number, airline name /
//     country / radiotelephony callsign, registration-as-callsign detection
//     (LookupCallsign, LookupCallsignFor, LookupAirline).
//   - ICAO aircraft type designators (LookupType, TypeName).
//   - Code tables: emitter category, squawk meanings, emergency states, ADS-B
//     version, NIC / NACp / NACv / SIL / SDA / GVA accuracy texts.
//   - An importer that streams a free downloadable aircraft database into a
//     caller supplied Sink (Import, Parse, Sources).
//
// Typical use by the tracker when an aircraft first appears, and again when
// its callsign changes:
//
//	refdata.Lookup(icao).Apply(&ac)                     // country, military, derived registration
//	if rec, ok := acdb[icao]; ok {                      // imported database, kept by the store
//		rec.Apply(&ac)                              // registration, type, operator
//	}
//	refdata.LookupCallsignFor(callsign, icao).Apply(&ac) // operator from the callsign
//
// All lookup functions are safe for concurrent use: the tables are immutable
// after package initialisation. Lookups do not allocate except where a new
// string has to be built (a derived registration, or normalising a callsign
// that was not already upper-case / trimmed).
//
// Accuracy notes. The allocation table gives the state that was *allocated*
// the address block; aircraft occasionally transmit a wrong address. The
// military flag and the address-derived registrations are heuristics that are
// right for the overwhelming majority of airframes but are not authoritative;
// when the imported aircraft database has a record for an address, prefer it.
package refdata
