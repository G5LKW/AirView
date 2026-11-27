// Package beastin is the aggregator's compat Beast ingest on TCP 30004
// (docs/aggregator/DESIGN.md §5.1).
//
// Unmodified readsb / dump1090-fa / Radarcape feeders push a Beast binary
// stream to the aggregator: plain Beast (beast_out), beast_reduce_out or
// beast_reduce_plus_out. The last one adds extension messages:
//
//   - 0x1A 0xE4 + 36 ASCII bytes: the feeder UUID, sent once after
//     connecting. It identifies the station.
//   - 0x1A 0xE3 + 8 bytes: a receiver ID that applies to the frames that
//     follow. It is honoured only on links whose UUID is listed in
//     Policy.TrustedRelays; each receiver ID then becomes a sub-station.
//     From anyone else it is counted and ignored.
//   - 0x1A 'W' + 1 byte: an option. 'O' asks for pings.
//   - 0x1A 'P' + 3 bytes: a 24-bit millisecond ping counter.
//
// Escapes with any other type byte are counted and skipped; nothing a peer
// sends makes the link fail (the parser is feed.BeastParser).
//
// # What the server does with a link
//
//   - Connection admission: an optional gate (closed until the admin is set
//     up), per-IP, per-IP-unidentified and per-subnet (IPv4 /24, IPv6 /48)
//     connection limits with an allow list, a global link cap, and
//     cool-downs for identities that were refused or disconnected for
//     exceeding their limits.
//   - Identity: the UUID from 0xE4 (normalised to the 28..32 hex digits of
//     model.NormalizeUUID), or, when the link sends Beast data but no UUID
//     within Options.IdentifyWait, an "unidentified" IP-keyed identity
//     "ip:" + base32(HMAC-SHA256(serverKey, ip ‖ listen port)[:10]). A UUID
//     arriving later upgrades the link. The Registry maps identities to
//     stations and decides admission (approved / pending / denied).
//   - Ping: a ping the client originates is echoed verbatim. After 'W' 'O'
//     the server also originates its own ping every Options.PingInterval and
//     measures the round trip from the echo. An incoming ping that matches
//     one of our outstanding pings is an echo, never echoed back, so the
//     behaviour is correct whichever side starts pinging.
//   - Limits: per station frames per second and per link bytes per second.
//     Frames above the frame limit in a one-second window are shed; the
//     first over-limit second sends 'W' 'S' (slow down); OverLimitSeconds
//     consecutive over-limit seconds disconnect the link and put its
//     identity into a cool-down. 'W' 'S' is also sent when the link's
//     delivery queue is more than half full.
//   - Statistics: per link (Server.Links) and server-wide (Server.Stats).
//
// # Never MLAT
//
// Frames from 30004 are for the map and statistics only. beast_reduce thins
// copies and readsb may re-stamp, so their timestamps must never reach clock
// synchronisation, MLAT or the survey. Every Batch carries MapStatsOnly =
// true; the ingest must honour it (latency statistics may still use the
// ticks). With Options.StripClock the clock fields are cleared before
// delivery as a belt-and-braces measure. MLAT result frames (the magic
// timestamp) are dropped unless Options.AcceptMLATResults is set, so a
// feeder cannot inject positions computed elsewhere as receptions.
//
// # Threading
//
// One goroutine reads each link (parsing, identity, pings and limits happen
// there); a fixed pool of Options.Workers goroutines delivers the per-link
// batch queues to the Sink, keeping per-link order. 1,000 links therefore
// cost about 1,000 goroutines plus the pool.
//
// See WIRING.md for how the engine connects the Registry and the Sink.
package beastin
