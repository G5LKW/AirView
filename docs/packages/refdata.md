# refdata - API notes (from the package author)

import "airview/internal/refdata" (depends only on stdlib + airview/internal/model). All lookups are safe for concurrent use (tables immutable after init) and allocation-free unless noted.

ADDRESS
- `Lookup(icao uint32) Info` - at most 1 alloc (the derived registration string). `Info{ICAO uint32 (json "-"); Country, CountryCode (ISO2); State, StateCode; Alloc AllocKind; Region string; Military bool; Registration string}`. Country differs from State only for territory sub-blocks (Bermuda/Cayman/Isle of Man/Guernsey -> GB, Hong Kong -> CN). JSON keys: country, countryCode, state, stateCode, alloc ("state"|"territory"|"region"|"reserved"|"none"), region, military, reg - this is the "info" object for GET /api/aircraft/{icao}.
- `Info.Hex() string`; `Info.Apply(*model.Aircraft)` sets Country/CountryCode, sets Military (never clears), sets Registration only if empty.
- `CountryOf(icao) (Country{Name, ISO2}, ok)` (territory-aware); `StateOf(icao)` (ICAO state only); `CountryName(iso2) string`.
- `IsMilitary(icao) bool` - HEURISTIC: true = very likely military, false = nothing.
- `Registration(icao) string` ("" when no rule; never for military blocks); `AppendRegistration(dst []byte, icao) []byte` (0 allocs).
- `FormatICAO(uint32) string` (6 upper-case hex); `ParseICAO(s) (uint32, ok)` (1-6 hex digits, optional leading '~').
- `AllocKind`: AllocNone, AllocState, AllocTerritory, AllocRegion, AllocReserved (String/MarshalText).

CALLSIGN
- `LookupCallsign(cs string) CallsignInfo`; `LookupCallsignFor(cs, icao)` additionally rejects a registration-style callsign whose nationality mark does not fit the address block (use this when the address is known; icao 0 = no check).
- `CallsignInfo{Callsign (trimmed, upper); Kind; Designator; FlightNumber; Operator; OperatorType; Telephony; Country; CountryCode; Military bool; Registration}`. Kind: CallsignUnknown ("" input), CallsignFlight (LLL+digit..., Designator set even if operator unknown), CallsignRegistration (Registration formatted, e.g. "G-BYTE"), CallsignMilitary (word callsign such as NATO01), CallsignOther. JSON kind = "unknown|flight|registration|military|other".
- `CallsignInfo.Spoken() string` ("SPEEDBIRD 123A"); `CallsignInfo.Apply(*model.Aircraft)` fills Operator and Registration only if empty, sets Military.
- `LookupAirline(designator) (Airline{Designator, Name, Telephony, CountryCode, Type}, ok)`; `Airline.Country()`, `Airline.Military()`; `AirlineCount()`; `NormalizeCallsign(s)`.
- Operator type constants: OperatorAirline, OperatorCargo, OperatorBusiness, OperatorMilitary, OperatorGovernment, OperatorOther.

TYPES
- `LookupType(designator) (AircraftType{Designator, Manufacturer, Model, Name, Description ("L2J"), WTC ("L|M|H|J|L/M"), Role}, ok)`; methods Engines(), EngineType(), AirframeClass(), ClassText(), WTCText().
- `TypeName(designator) string`; `TypeCount()`; `TypeEngines(desc)`, `TypeEngineType(desc)`, `TypeAirframeClass(desc)`, `TypeClassText(desc)` ("Landplane, 2 jet engines"), `WTCText(wtc)`.
- TypeRole: RoleAirliner, RoleRegional, RoleFreighter, RoleBusiness, RoleUtility, RoleGA, RoleLight, RoleHelicopter, RoleMilitary, RoleUAV, RoleOther (string values, usable for map icons).

CODES
- `EmitterCategory("A3") string`; `EmitterCategoryCode(typeCode 1..4, category 0..7) string` (TC4=A, 3=B, 2=C, 1=D); `EmitterCategoryKnown(code) bool`.
- `Squawk(code) (SquawkInfo{Code, Meaning, Region, Emergency, Kind}, ok)`; `SquawkMeaning(code)`; `SquawkEmergency(code)` ("unlawful"/"nordo"/"general"/""); `ValidSquawk(code)`. Kinds: SquawkKindEmergency, SquawkKindConspicuity, SquawkKindSpecial.
- `EmergencyState(code 0..7) string` (ADS-B TC28 field -> model.Aircraft.Emergency identifier); `EmergencyLabel(state) string`; constants EmergencyNone/General/Medical/MinFuel/NoRadio/Unlawful/Downed.
- `ADSBVersionLabel(v)`; `NICText(n)`, `NICRadiusM(n) (metres, ok)`; `NACpText(n)`, `NACpAccuracyM(n) (metres, ok)`; `NACvText`, `SILText`, `SDAText`, `GVAText`.

AIRCRAFT DATABASE
- `type ACRecord struct{ICAO uint32; Reg, Type, Desc, Operator string; Flags uint32; Year int}` - maps 1:1 to the store's acdb table. Flags: ACMilitary=1, ACInteresting=2, ACPIA=4, ACLADD=8 (tar1090-db only). `ACRecord.Military()`; `ACRecord.Apply(*model.Aircraft)` overwrites Registration/TypeCode/TypeDesc/Operator when non-empty (TypeDesc falls back to the embedded type name), sets Military.
- `type Sink interface{ Put(ACRecord) error }`; `SinkFunc`. Put is called from one goroutine in file order; an error aborts. Records already delivered stay delivered on failure or cancel, so the sink should stage and swap.
- `Sources() []Source`, `SourceByID(id)`; `Source{ID, Name, URL, Homepage, Format, Licence, LicenceURL, Attribution, Notes, ApproxBytes, ApproxRecords}` (JSON-tagged, show Attribution and Licence in the UI). IDs: SourceTar1090 ("tar1090-db", default and recommended, about 9 MB gz, 646k records), SourceOpenSky ("opensky", 108 MB, 587k records).
- `Import(ctx, ImportOptions{Source, URL, Format, Client, Progress, ProgressInterval, IdleTimeout (default 60 s), MaxBytes (default 2 GiB), UserAgent}, sink) (ImportResult{Source, URL, Format, Records, Skipped, Empty, Bytes, LastModified, ETag, Elapsed}, error)` - blocking; run in a goroutine. Returns ctx's error on cancel, ErrImportIdle on a stalled download; counts are valid even on error.
- `Parse(ctx, io.Reader, sink, ParseOptions{Format, Progress, ProgressInterval, BytesTotal, MaxBytes}) (ParseResult{Format, Gzip, Records, Skipped, Empty, BytesRead}, error)` - for a user-supplied local file; detects gzip and format.
- `Progress{Source, URL, Stage ("connect"|"download"|"done"), BytesRead, BytesTotal (-1 unknown), Records, Skipped, Empty}`; `Progress.Fraction()` (0..1, or -1 when unknown).
- Formats: FormatAuto, FormatTar1090, FormatOpenSky. Errors: ErrUnknownFormat, ErrTooLarge, ErrNoRecords, ErrImportIdle.

USAGE (tracker, when an aircraft appears and when its callsign changes):
```go
refdata.Lookup(icao).Apply(&ac)                      // country, military, derived registration
if rec, ok := acdbLookup(icao); ok { rec.Apply(&ac) } // store's acdb row as refdata.ACRecord
refdata.LookupCallsignFor(cs, icao).Apply(&ac)       // operator from the callsign
```
Import (engine/server):
```go
res, err := refdata.Import(ctx, refdata.ImportOptions{Source: refdata.SourceTar1090,
    Progress: func(p refdata.Progress) { /* push SSE notice */ }}, sink)
```
