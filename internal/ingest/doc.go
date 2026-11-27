// Package ingest is the station-aware front end of the aggregator's global
// tracker (docs/aggregator/DESIGN.md §2, §4.4-4.6, §6.2).
//
// Every frame a station delivers - from an AVF/1 feeder (internal/ingest/avfsrv),
// a Beast compat link (internal/ingest/beastin), the in-process host station
// or a bridge - goes through one *Ingest:
//
//   - Normaliser and host-offset estimator (station.go): the frame's receiver
//     ticks and the feeder's host time are turned into one "corrected" time in
//     the server's clock domain. Per station and clock epoch, the lower
//     envelope of (host time - tick time) maps ticks to a jitter-free host
//     time; the feeder's host-clock offset comes from AVF PING/PONG (or, until
//     a PONG arrived, from the arrival envelope); a relative latency per
//     station is learned from the groups it shares with other stations. The
//     corrected time is what the grouper compares.
//   - Grouper (grouper.go): copies of one transmission heard by several
//     stations are joined by a 64-bit hash of the payload plus the payload
//     bytes. The first copy goes to tracker.NetHandler.Handle; every later copy
//     is credited with the cached result through Lanes.Credit without the
//     tracker mutex. The same-station repeat rule separates repeated identical
//     transmissions (DF11 acquisition squitters, DF4/5 replies, steady DF17):
//     a station that already has a copy in a group starts a new group unless
//     its ticks are within 1 ms of its first copy (the other channel of a
//     diversity receiver). Groups with timed copies are handed to an MLATSink
//     after FlushAfter (or the per-aircraft delay of a FlushPolicy, DESIGN
//     §9.3: the p99 delivery latency of the stations that hear it, which the
//     ingest teaches the policy from a sample of the groups), copies that
//     arrive later until the end of Window as a late update; the group is
//     remembered for Retain more for de-duplication. A group flushed early
//     with a single synced copy of a sync candidate is delivered once more,
//     with every copy, when other stations' copies join it (Redelivery).
//     With a ClockView the synced-station gate keeps copies whose network
//     times differ by more than d/c + max(5σ, ClockGateFloor) apart; with a
//     ClockKindView it uses the frame kind and σ including receiver jitter.
//   - Routing: late (backfilled) frames and frames too old to group go to the
//     lanes only (tracker Handle with Late set); stations held pending feed
//     their own lane only (Lanes.Backfill), never the map or MLAT; Beast
//     30004 frames (MapStatsOnly) are grouped and mapped but never counted as
//     timed copies; synthetic MLAT frames and receiver status frames bypass
//     the grouper.
//   - Sessions (session.go): the protocol servers register their links;
//     once a second the ingest measures per-station rates and queue fill,
//     applies the FLOW rules of DESIGN §4.4/§4.6 (FLOW, selective, then
//     disconnect), the full/selective traffic-mode policy (selective above
//     Limits.SelectiveAbove approved online stations) and forwards INTEREST
//     set changes from the MLAT planner.
//   - Budget (budget.go): the global ingest budget (Limits.IngestFPS) with
//     three priority classes; selective-eligible traffic is shed first.
//
// Standalone mode never uses this package: there the tracker keeps its own
// de-duplication (DESIGN §6.2).
//
// The package is protocol neutral. Wiring notes are in WIRING.md.
package ingest
