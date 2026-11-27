// Package mlatc is the aggregator's compat server for the mlat-client
// protocol on TCP 31090 (docs/aggregator/DESIGN.md §5.2; the protocol is
// described in AirView's own words in docs/aggregator/MLATC.md).
//
// Unmodified mlat-client installations (piaware-style feeders, readsb +
// mlat-client, Radarcape scripts) connect here and deliver timestamped
// Mode S receptions for AirView's own MLAT engine; they receive MLAT
// results for the aircraft they helped to locate. The package is a
// clean-room implementation written from the protocol's observable
// behaviour; no mlat-client or mlat-server code was used (provenance in
// docs/aggregator/CLEANROOM.md).
//
// # Data flow
//
//	client ──handshake──▶ Registry.Attach (station, admission)
//	       ──sync/mlat──▶ tick normalisation ──▶ Sink.Submit (internal/ingest)
//	       ──seen/lost──▶ selection ──▶ start_sending / stop_sending
//	       ◀──result──── Server.SendResult (MLAT engine, per contributor)
//
// Each sync message carries two frames (the even and odd DF17 airborne
// positions of one aircraft), each mlat or ssync message one frame. Every
// frame becomes one model.RawFrame with the station, a clock epoch, the
// clock class and receiver ticks in the AirView convention, so the
// station-aware ingest groups mlat-client copies with AVF/1 copies of the
// same transmission and the clock filter treats them alike.
//
// # Timestamp normalisation (the precision-critical part)
//
// The reference client does not send raw receiver stamps. Writing t for
// the value on the wire, s for the receiver's own stamp (the value an
// AirView feeder would put into AVF/1 ticks), and n for the payload length:
//
//	12 MHz clocks (dump1090, beast, radarcape_12mhz):
//	    t = s − Δ12(n),  Δ12 = 768 ticks (64 µs) for Mode S, 244 for Mode A/C
//	    s = (t + Δ12(n)) mod 2^48, then unwrapped: the value ≡ s (mod 2^48)
//	    nearest to the epoch's largest tick so far (model.Unwrap48).
//	GPS clock (radarcape_gps; 1 GHz, nanoseconds of the UTC day):
//	    t = (s_day − Δg(n)) mod 86 400 e9,  Δg = 64 000 ns (56-bit),
//	        120 000 ns (112-bit), 20 300 ns (Mode A/C)
//	    s_day = (t + Δg(n)) mod 86 400 e9
//	    s = d·86 400 e9 + s_day with the day index d chosen so that
//	        |s − t_server| ≤ 12 h (model.GPSTicks), i.e. Unix nanoseconds.
//	SBS (20 MHz, 24-bit counter widened by the client):
//	    t = W(s + Δs(n)),  Δs = 1 120 ticks (short), 0 (long), 1 920 (A/C),
//	        W the client's widening (borrow the previous value's upper bits)
//	    s = t − Δs(n)
//
// The offsets Δ are the frame-start corrections the client applies; adding
// them back makes a 31090 tick and an AVF/1 tick of the same receiver
// identical, so the clock filter's single FrameRef(class, kind) table
// applies to both, and any residual short/long difference stays in the
// per-station β_short state (DESIGN §9.2). TestSimnetRealClientConvention
// proves the inversion against simnet truth for every device type: the
// recovered ticks equal the receivers' stamps exactly (12 MHz modulo 2^48,
// SBS modulo 2^24, GPS exactly), and every epoch change coincides with a
// truth epoch change.
//
// Options.RawTicks accepts AirView's own convention instead (unwrapped
// counters, absolute GPS nanoseconds); it exists for AirView's simulator
// and test clients.
//
// # Clock epochs
//
// Ticks of one epoch are comparable; a new epoch tells the ingest and the
// clock filter to start a new mapping (ingest.ClockEvent, announced before
// the first frame of the new epoch). A new epoch starts on:
//
//   - every connection (a reconnecting client may have restarted its
//     receiver), numbered after the station's previous epochs;
//   - clock_reset with a decoder mode or frequency (class change, e.g. a
//     Radarcape losing GPS lock switches to the emulated 1 GHz mode,
//     model.ClockGPSEmul), and clock_reset for any other reason, except the
//     day rollover of a GPS clock, which is not a discontinuity in Unix
//     nanoseconds;
//   - clock_jump;
//   - input_connected after input_disconnected (the client reconnected to
//     its receiver);
//   - a server-side plausibility break: a tick more than 30 s behind the
//     epoch's maximum (sync pairs legitimately carry the first half up to
//     the 5 s pairing window late), or more than (elapsed server time +
//     90 s) ahead of it (model.EpochStepLimit).
//
// # Duplicates
//
// The reference client may send the same position message in two
// consecutive sync pairs (it pairs each new message with the latest one of
// the other parity). A 256-entry table of recent (ticks, payload hash)
// drops the repeat, so each reception reaches the ingest once.
//
// # Split sync (single "ssync" messages) and paired sync
//
// With pairs, the first half of every pair reaches the server only when the
// second half was received (up to the client's 5 s pairing window later),
// so it misses its transmission's grouping window; split sync sends each
// position as it arrives. Options.SplitSync defaults to SplitSyncAuto: the
// handshake reply offers split_sync, a client that ignores the flag keeps
// sending pairs (still accepted), and a client whose single syncs turn out
// unusable (splitBadMin malformed "ssync", or no sync data at all for
// splitSilentAfter while unknown messages arrive) is disconnected once and
// answered without split_sync for splitFallbackFor (Server.
// SplitSyncFallbacks, Stats.SplitFallbacks). The decision and its evidence
// are in docs/aggregator/PROGRESS.md (H-mlatc): on simnet split sync
// delivers 99.98 % of multi-station sync transmissions complete to the
// grouper against 86 % with pairs, halves the sync traffic of a client that
// pairs overlapping messages, and costs nothing in hold-out quality.
//
// For the pairs that still arrive, the older half's HostNs is set to its
// reception time — the arrival time minus the tick difference to the newer
// half at the class's nominal rate (backdateSyncPair) — so the ingest's
// host-time consumers see an old copy rather than a fresh one with odd
// ticks.
//
// # Selection (start_sending / stop_sending)
//
// The client sends nothing for an aircraft until the server asks for it. In
// the ingest's full traffic mode every aircraft the client reports as seen
// is requested (up to Policy.MaxRequested); in selective mode (more online
// stations than aggregator.limits.selectiveAbove) only the MLAT planner's
// interest sets (ingest.SetInterest) are requested, plus, for a station in
// timing quarantine (Options.Trust, internal/registry/trust), its
// Options.ProbeAircraft highest-rate aircraft from its last rate_report, so
// the quarantine has sync frames to judge it by. The server implements
// ingest.Controller for this; Server.StartSending / StopSending force
// entries.
//
// rate_report (the client's ADS-B position rate per aircraft) is kept per
// client: LinkStats.ADSBAircraft / ADSBRate, Server.ADSBRate and
// Server.ADSBRates for the planner, and Hooks.OnRates.
//
// # Offline sessions
//
// Server.Offline runs a client session without a network connection: the
// same handshake, line processing, selection and results, with the lines
// and their arrival times supplied by the caller. It serves the replay of
// recorded client streams (DESIGN §13.3) and the virtual-time simulation
// in mlatcsim, which drives mlat-client stations, AVF stations, the ingest
// and the MLAT engine through one simnet world faster than real time.
//
// # UDP transport (Options.UDP, aggregator.mlat.udp)
//
// A client whose handshake offers udp_transport 2 is answered with
// [host, port, key] (host: Options.UDPHost or the local address of its TCP
// connection; port: Options.UDPPort or the bound UDP port, by default the
// TCP port number) and then sends its timed frames as datagrams, while
// seen/lost, rate_report, clock events and heartbeats stay on TCP. The
// datagram format (14-byte header u32 key, u16 sequence, u64 base
// timestamp; messages 1 sync, 2/3 mlat short/long, 4 ssync, 5 rebase, 6
// absolute sync; timestamps = base + signed 32-bit delta) is described in
// udp.go and in the "UDP transport" section of docs/aggregator/MLATC.md.
// The timestamps are the JSON values, so the tick normalisation above
// applies unchanged; TestUDPInterop proves that a station sending every
// message type as datagrams delivers exactly the frames of a station
// sending the same receptions as JSON lines.
//
// Attribution and safety: the key is random (crypto/rand) and becomes live
// only after the handshake's epoch start; a datagram must come from the
// TCP connection's address (Options.UDPAnySource lifts this); a duplicate
// sequence number within the last 64 drops the datagram; sequence gaps
// estimate the loss (LinkStats.UDPLost); the server never answers a
// datagram (no amplification). The frame rate and kB/s limits count
// datagram traffic like TCP traffic, and mostly malformed datagrams
// disconnect the client like garbage lines. Since a UDP client may stay
// silent on TCP, its idle timeout is checked by the writer against the last
// data on either path instead of a TCP read deadline.
//
// Concurrency: datagrams are read by Options.UDPReaders goroutines; the
// processing of one client (TCP lines and datagrams) is serialised by the
// connection's proc lock, so epochs, de-duplication and batches see one
// sequence of frames. Datagrams of one client may be processed out of
// order (several readers, or reordering in the network); the ingest
// accepts out-of-order frames within an epoch as it does for paired sync.
// Cost: ≈ 3.5 µs per 1,214-byte datagram of 100 mlat messages (≈ 35 ns
// per message against ≈ 97 ns per JSON mlat line, 0 allocations;
// BenchmarkUDPDatagram, development machine).
//
// # Mode A/C (DESIGN §9.9)
//
// Mode A/C replies arrive as mlat messages with a 4-digit "m" (TCP) or as a
// long frame padded with 12 zero bytes (UDP) and reach the ingest as
// model.FrameModeAC frames (12 MHz offset 244 ticks, GPS 20.3 µs, SBS 1 920
// ticks added back). Requests (Server.SetModeACAll, StartSendingModeAC,
// StopSendingModeAC) are an AirView extension of the protocol:
// start_sending_modeac / stop_sending_modeac with 4-hex-digit reply values
// or "*", sent only to clients that announced "modeac": true in their
// handshake (or to all with Options.ModeACAllClients); see modeac.go.
//
// # Results
//
// Server.SendResult delivers one result to one station's client, in the
// format it asked for; the engine calls it once per contributing station
// (DESIGN §8.4). The ECEF format carries the position in metres, the
// upper-triangular ECEF covariance in m² (the client's error estimate is
// √(Σxx + Σyy + Σzz), the 3-D one-sigma radius), the station count and,
// when known, velocity (east/north in knots, vertical rate in ft/min). The
// legacy format carries latitude/longitude (WGS-84), altitude in feet above
// the ellipsoid and gdop = √trace(Σ)/300, because the client turns gdop
// into an error estimate of gdop × 300 m.
//
// # Limits
//
// Per address: connections, unauthenticated connections, stations of
// UUID-less clients; per /24 or /48: connections; overall: clients. Per
// client: frames/s, received kB/s and lines/s (a small compressed chunk can
// inflate into many lines), 64 KiB per line, malformed-line share, seen and
// requested set sizes, output queue (results are dropped first). The kB/s
// limit is a compressed budget: a client without compression counts a
// quarter of its bytes. Repeated over-limit seconds disconnect the client
// with a cool-down (logged with the measured rates). A second live
// client with the same UUID is refused unless the first has been silent for
// 30 s (then it replaces it: a half-open connection of the same feeder).
//
// # Concurrency and cost
//
// One reader goroutine per client parses, converts and delivers; one
// writer goroutine sends queued output, heartbeats (every 30 s; the client
// gives up after 60 s without data), stats and selection updates. The
// per-line path allocates nothing (TestHotPathZeroAlloc): ≈ 90 ns per mlat
// line, ≈ 230 ns per sync line on the development machine; 1,000 clients
// at 300 frames/s each cost about 0.2 CPU cores and 80 MB of heap
// (cmd/mlatc-load, server and clients in separate processes on loopback). The server
// direction of zlib2 is written as stored deflate blocks, so a client
// costs no compressor state.
package mlatc
