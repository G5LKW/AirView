// Package sim is AirView's traffic simulator: a self-contained world of
// aircraft around the station that transmit bit-exact Mode S / ADS-B frames,
// and a model of the receiver (two antennas, link budget, radio horizon,
// fading, detection) that decides which of them are heard and how strongly.
//
// It serves two purposes: demo mode (no receiver needed) and end-to-end
// testing of the whole pipeline. For the second purpose the encoder in this
// package is deliberately independent of internal/modes (the decoder): the
// package imports nothing but internal/model, so the two implementations
// cross-check each other.
//
// # Usage
//
//	s := sim.New(sim.FromModel(cfg, "demo"))    // live demo, real time
//	go s.Run(ctx, tracker)                      // model.RawFrame straight into a FrameSink
//	go s.ServeBeast(ctx, "127.0.0.1:10003")     // and/or as a Radarcape-style Beast TCP server
//
//	// deterministic: fixed seed + virtual clock, as fast as the CPU allows
//	s := sim.New(sim.Config{Seed: 1, Start: t0, Aircraft: 100})
//	s.Advance(ctx, 6*time.Hour, sink)           // ~4000x real time with 100 aircraft
//	truth := s.Truth()                          // what is really out there
//
// All outputs of one Sim share a single simulation. It advances in 100 ms
// steps while at least one Run / ServeBeast is active (paced by Config.Speed;
// every frame is handed over when the wall clock reaches its Recv, not per
// step) or when Advance / CatchUp is called. A batch holds the frames whose
// Recv falls into its step, so a frame sent late in a step may be delivered
// with the next one (Sim.TruthAt / TruthOf at TransmitTime(frame) are exact
// for it).
//
// # Timing
//
// GPSNs is the Radarcape-style time stamp of the END of the frame at the
// antenna (transmit + propagation + frame duration); Recv adds a receiver ->
// host delay (median ~2.5 ms, rare bursts, monotone). About 10 % of the ADS-B
// aircraft (deterministic from seed and address) are UTC-coupled: their
// airborne positions carry the TIME bit (T=1, TC 9 / 10 / 20 only), apply
// to the 0.2 s UTC epoch whose parity the CPR format bit gives, and are sent
// 50..150 ms (per aircraft) before that epoch at the earliest and 50..150 ms
// after it at the latest - within the DO-260A ±150 ms tolerance - so the
// latency analysis measures their transponder delay exactly.
//
// # World
//
// Config.Aircraft aircraft (scaled by a diurnal traffic curve) fly around the
// station: en-route overflights on great-circle tracks at FL300-FL410,
// arrivals descending to and departures climbing from four synthetic airports
// within 150 km (including taxi, take-off roll, 3 degree final approach,
// rollout and holding patterns), slow low general aviation and helicopters,
// and military aircraft that are Mode S only. Traffic enters and leaves at
// Config.RangeKm. Addresses come from the real ICAO state allocation blocks,
// callsigns are airline designator + flight number or registrations. A
// slowly varying environment supplies wind, temperature, QNH and the
// effective earth radius factor (k) that moves the radio horizon over the
// hours like real tropospheric conditions do.
//
// # Transmissions
//
// ADS-B aircraft send DF17 airborne position (TC 9-18 with the Q-bit
// altitude, or TC 20 with GNSS height for a few airframes; alternating even
// and odd CPR at 2 Hz), surface position (TC 7), airborne velocity (TC 19
// subtype 1, 2 Hz), identification (TC 1-4, every 5 s), and with ADS-B
// version 2 also emergency / priority status (TC 28), target state and status
// (TC 29) and operational status (TC 31). Every aircraft sends DF11
// acquisition squitters (about 1 Hz, PI with II = 0) and answers simulated
// interrogations with DF4 / DF5 / DF20 / DF21 (address overlaid on the
// parity) carrying Comm-B registers 2,0 / 4,0 / 5,0 / 6,0 and occasionally
// 4,4 that are consistent with the aircraft state, and with DF0 short ACAS
// replies. "MLAT" aircraft are Mode S only off-air; while they are being
// received, synthetic DF18 (CF 2) position and velocity frames flagged
// RawFrame.MLAT are added the way an MLAT network returns results.
//
// # Reception
//
// Each transmission is evaluated per antenna (Config.Antennas, default an
// omni vertical on channel 0 and a yagi on channel 1): transmitter EIRP, free
// space loss at 1090 MHz, antenna pattern by azimuth and elevation, a soft
// diffraction edge at the k-factor dependent radio horizon, slow shadowing,
// Rician fading (which becomes Rayleigh-like near the horizon), garbling by
// overlapping transmissions, a noise floor and a logistic detection curve.
// Levels are reported in dBFS: about -1 for an aircraft overhead down to
// about -37 at the noise floor.
//
// # External formats (sources)
//
// Mode S / ADS-B bit layouts follow ICAO Annex 10 Vol IV / Doc 9871 and
// RTCA DO-260B as documented in J. Sun, "The 1090 Megahertz Riddle"
// (https://mode-s.org/1090mhz/): CRC-24 generator 0x1FFF409, CPR with
// NZ = 15, the DF0/4/5/11/17/18/20/21 field layouts, type codes 1-4, 5-8,
// 9-18, 19, 20-22, 28, 31 and BDS 4,0 / 4,4 / 5,0 / 6,0. The TC 29 (version
// 2) and the TC 31 capability / operational mode sub-fields were checked
// against the dump1090 decoder (flightaware/dump1090 mode_s.c). The tests
// reproduce the book's worked examples bit for bit.
//
// Beast binary output: every frame is 0x1A, a type byte, then the body in
// which each 0x1A is doubled. Type '2' and '3' carry a 6-byte big-endian
// timestamp, a signal byte and 7 or 14 bytes of Mode S; type '1' is Mode A/C
// (not generated); type '4' is the Radarcape status message sent with every
// 1PPS: timestamp, one unused byte and 14 bytes of which byte 0 is the
// settings (0x10 = GPS timestamps), byte 1 the signed timestamp offset at
// the last PPS edge in 15 ns units and byte 2 the GPS status flags; type '5'
// is the Radarcape receiver position: 21 raw bytes with latitude, longitude
// and altitude as little-endian float32 at offsets 4, 8 and 12. (Jetvision
// wiki "Mode-S Beast: Data Output Formats"; flightaware/dump1090 net_io.c;
// flightaware/beast-splitter status_writer.cc.) The Radarcape GPS timestamp
// holds the second of the UTC day in the upper 18 bits and nanoseconds in the
// lower 30 bits (Jetvision wiki "Radarcape: Firmware Versions"). The signal
// byte is the amplitude scaled to 0..255, i.e. dBFS = 20*log10(byte/255), as
// written and read by dump1090 / readsb. MLAT results carry the conventional
// synthetic timestamp 0xFF004D4C4154 (readsb MAGIC_MLAT_TIMESTAMP). The Beast
// format has no field for the RF channel; see BeastOptions for how the two
// antennas map onto streams.
package sim
