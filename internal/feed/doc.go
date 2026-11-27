// Package feed contains the input clients that deliver receiver data into a
// model.FrameSink, and the Manager that runs them according to the
// configured []model.Feed.
//
// Feed types handled here:
//
//   - "beast": Beast binary over TCP (Radarcape 10003/10004/10005, dump1090 /
//     readsb 30005). Produces model.RawFrame.
//   - "avr":   AVR / raw hex text over TCP (dump1090 30002, Beast serial
//     format). Produces model.RawFrame.
//   - "sbs":   BaseStation CSV over TCP (30003). Produces model.DecodedUpdate.
//   - "json":  HTTP polling of readsb / dump1090 aircraft.json or the
//     Radarcape aircraftlist.json (auto-detected). Produces model.DecodedUpdate.
//   - "hub":   an AirView host's data port (package hub). Produces whatever
//     the host relays.
//
// Further types (the engine's "demo" simulator) are plugged in with
// Manager.RegisterType.
//
// # What is known about the Radarcape (EVO 2CH) data interfaces
//
// Verified against the jetvision wiki (Radarcape:Software_Features,
// Radarcape:User_Services_and_Interfaces, Radarcape:Version_History,
// Radarcape:Firmware_Versions, Mode-S_Beast:Data_Output_Formats) and the
// reference decoders mlat-client (modes_reader.c) and dump1090-fa / readsb
// (net_io.c):
//
//   - TCP 10002: raw FPGA output including broken frames (binary or AVR,
//     depending on the FPGA switch); 10003: binary, all Mode S formats, CRC
//     pre-checked (plus Mode A/C when enabled); 10004: binary, DF11/17/18
//     only; 10005: binary, only frames of aircraft without a known position
//     (input for MLAT clients - it does NOT carry MLAT results); 30003:
//     BaseStation CSV. The same streams exist as UDP copies. Port 10006 is
//     only mentioned as part of the range "raw data ports 10002-10006"; its
//     content is undocumented.
//   - Beast binary framing: 0x1A, type, 6-byte big-endian timestamp, 1-byte
//     signal level, payload; a literal 0x1A inside a frame is doubled.
//     Types: '1' Mode A/C (2 bytes), '2' Mode S short (7), '3' Mode S long
//     (14), '4' Radarcape status (14), '5' Radarcape position (21 bytes after
//     the type, no timestamp/signal; little-endian float32 latitude,
//     longitude and altitude at offsets 4, 8 and 12).
//   - GPS timestamp mode: the upper 18 bits of the 48-bit timestamp are the
//     second of the day, the lower 30 bits nanoseconds. Legacy mode: a
//     free-running 12 MHz counter.
//   - Status frame (type '4', sent on every 1PPS in GPS mode): byte 0 mirrors
//     the FPGA switches (bit 4 = GPS timestamps), byte 1 is the signed time
//     stamp error at the 1PPS edge ("int8_t, 1 tick is 15ns", typically
//     +-3 ticks, Jetvision "Mode-S Beast:Data Output Formats"), byte 2
//     is the GPS status (bit 0 antenna OK, bit 1 tracking >= 1 satellite,
//     bit 2 tracking >= 3 satellites, bit 3 UTC time base, bit 7 second-of-day
//     range is 0..86399 rather than the legacy 1..86400). mlat-client
//     additionally reads bit 4 as "FPGA synchronised" and bit 5 as
//     "timestamp not usable / emulated", and trusts the detail bits only
//     when bit 7 is set; an all-zero GPS byte is therefore shown as "GPS
//     state not reported" rather than as an antenna fault. Bit 5 set means
//     the Radarcape itself marks its timestamps as emulated (mlat-client
//     switches to its RADARCAPE_EMULATED decoder): AirView then shows "GPS
//     time not valid" and class gps_emul; that is the receiver's verdict,
//     not a decoding problem (BeastStatus.Advice says what to check). The signal byte
//     of a status frame is zero / reserved.
//   - Signal level: the wiki does not give a scale for the signal byte.
//     dump1090-fa and readsb read it as amplitude (power = (byte/255)^2) and
//     write it back as round(sqrt(power)*255), in Beast binary as well as in
//     the '<' AVR format; this package follows them.
//   - AVR text: "*hex;" (the Beast's plain format; dump1090 also accepts
//     ':'), "@" + 12 hex digits timestamp + hex (Beast "MLAT" format), "%"
//     likewise (CRC-checked variant, dump1090 / readsb), "<" + 12 hex digits
//     timestamp + 2 hex digits signal + hex (dump1090 / readsb).
//   - MLAT results computed by mlat-client style software come back as
//     synthetic DF18 Beast frames whose timestamp is 0xFF004D4C4154
//     ("\xFF\0MLAT") .. +10. The Radarcape's own MLAT results are only
//     documented on port 30003 (as MSG,3 or MSG,10) and in aircraftlist.json
//     (src "M"); no raw port is documented to carry them.
//   - The 2CH model has two antenna inputs feeding a single receiver
//     ("antenna diversity" / "dual antenna systems" in the release notes,
//     "two antenna inputs that feed a single receiver" in the product
//     description); no per-frame channel indication is documented for any
//     port, and there are no per-antenna ports.
//   - aircraftlist.json (http://radarcape/aircraftlist.json): fields uti, ns,
//     hex, fli, src (A = ADS-B, M = jetvision MLAT, F = FlightAware MLAT,
//     others = FLARM/OGN etc.), lat, lon, alt (ft), altg, spd (kt), trk, vrt
//     (ft/min), gda (A/G, lower case for TIS-B), squ, dbm (approximate dBm
//     at the antenna connector), lla (position age, s), tru (count of
//     decoded frames); absent values are missing or null.
//   - readsb / dump1090-fa aircraft.json: {"now","messages","aircraft":[...]}
//     with hex ("~" prefix = non-ICAO), type, flight, alt_baro (feet or
//     "ground"), alt_geom, gs, track, baro_rate, geom_rate, squawk, lat, lon,
//     seen, seen_pos, rssi (dBFS), messages, mlat[] (README-json.md).
//
// Not documented anywhere that could be found, and therefore handled
// tolerantly rather than assumed:
//
//   - The scale of the Beast signal-level byte. This package uses the
//     convention shared by dump1090, readsb and every Beast consumer:
//     amplitude = byte/255, i.e. dBFS = 20*log10(byte/255); 0 means unknown.
//   - Whether a given stream uses GPS or 12 MHz timestamps when no status
//     frame is seen (other Beast sources): detected statistically, see
//     Timestamper.
//   - A channel flag for the 2CH: RawFrame.Channel is always -1 for Beast,
//     AVR and SBS input; attribute frames with Feed.AntennaID.
//   - The top-level shape of aircraftlist.json: both a bare array and an
//     object wrapping an array are accepted.
//
// # Network additions (docs/aggregator/DESIGN.md §1, §4.3, §5.1)
//
//   - Beast extension messages of readsb-style feeders (0xE3 receiver ID,
//     0xE4 UUID, 'P' ping, 'W' option) are passed to BeastParser.OnEscape;
//     unknown escape types are skipped without ever failing the stream.
//   - Every Beast/AVR frame carries HostNs and, when it has a usable
//     receiver time, the clock class, epoch and unwrapped 64-bit ticks
//     (Timestamper, ClockTracker). New epochs are reported as ClockEvent
//     values (Manager.OnClockEvent) before the first frame of the epoch.
//   - Manager.Forwardable and ForwardableFrame implement the uplink
//     no-forward rule.
//
// See WIRING.md in this directory for the integration contract.
//
// # Ownership of RawFrame.Data
//
// Every RawFrame delivered by this package owns a freshly allocated,
// exactly sized Data slice. The sink may retain it indefinitely; the feed
// never touches it again. That one small allocation is the only per-frame
// allocation on the Beast and AVR paths.
package feed
