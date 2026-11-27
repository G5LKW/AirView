// Package simfeed drives a running AirView aggregator with simulated
// feeders from one internal/sim/simnet world, in real time, over the real
// network protocols (DESIGN §13.1). It is used by the engine's end-to-end
// tests and by cmd airview-netsim:
//
//   - AVFFeeder: a native AirView feeder (one internal/uplink identity per
//     receiver) on AVF/1, with the receiver's unwrapped ticks and clock
//     class; MLAT results coming back are recorded per aircraft.
//   - MLATClient: an mlat-client protocol v3 client (TCP 31090) that sends
//     what the reference client sends for one receiver: the handshake with
//     the declared position, "seen" lists, even/odd DF17 position pairs as
//     "sync" and the other Mode S frames as "mlat", only for aircraft the
//     server requested (start_sending / stop_sending), with the frame-start
//     timestamp convention of the reference client (see below). Results
//     ("result" lines) are recorded per aircraft.
//   - BeastFeeder: a readsb beast_reduce_plus_out link to the compat Beast
//     port (map and statistics only, never MLAT).
//   - Heard: per receiver, the aircraft it heard and when (the simulator's
//     truth), to check that results only went to contributors.
//
// # mlat-client timestamp convention
//
// The reference client reports start-of-frame values; AirView's mlatc
// server adds the offsets back (internal/ingest/mlatc doc.go, MLATC.md §5).
// MLATClient therefore converts the receiver's raw stamp as follows, in
// this package's own implementation of that published convention:
//
//	12 MHz counters (beast, radarcape_12mhz, dump1090):
//	    t = (raw − 768) mod 2^48     Mode S frames (64 µs at 12 MHz)
//	GPS (radarcape_gps), raw = second-of-day << 30 | nanoseconds:
//	    t = ns-of-day − 64 000 (short) / 120 000 (long), wrapped into the
//	        previous day when negative
//
// SBS receivers are not supported (their 24-bit counter needs the client's
// widening) and are refused by NewMLATClient.
//
// Everything here is test tooling: it never runs inside the product.
package simfeed
