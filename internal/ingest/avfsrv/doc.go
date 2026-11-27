// Package avfsrv is the AVF/1 server of aggregator mode: the native feeder
// listener on TCP 30160 (docs/aggregator/DESIGN.md §4, docs/aggregator/AVF.md).
//
// For every connection it
//
//   - applies the per-IP / per-subnet connection limits and the admin gate
//     before any TLS work (AVF.md §8),
//   - runs the TLS 1.3 + HELLO handshake of internal/avf (the Ed25519
//     signature bound to the TLS exporter),
//   - applies the clone rule (registry.CloneGuard: another IP is refused with
//     cloned_feeder, the same IP replaces the old session) and the feeder's
//     admission (registry: revoked feeders, invite tokens, approve/open),
//   - admits every receiver of a RECEIVER record as a station with
//     registry.Admit, writes the connection log and attaches the station to
//     an ingest session,
//   - decodes the record stream with seq/ACK (avf.Sequencer, a shared
//     avf.WatermarkStore so a reconnect resumes at the server's watermark),
//     normalises FRAMES into model.RawFrame (feeder host time, receiver
//     ticks, clock class and epoch, late bit inside BACKFILL) and hands them
//     through a bounded per-connection queue to internal/ingest,
//   - forwards CLOCK_EVENT / CLOCK_PAIR / CLOCK_STATUS to the ingest's clock
//     observer, DECODED updates to the tracker (map only), INPUT_STATUS,
//     GNSS_POS, MINUTE_STATS and RECEIVER details to the engine's hooks,
//     CLAIM to registry.MintClaim (CLAIM_CODE),
//   - feeds the PING/PONG host-clock offset, the wire byte rate and the queue
//     fill to the ingest session, which decides FLOW, selective mode and
//     rate-limit disconnects; INTEREST and FLOW records are sent from there,
//   - sends MLAT_RESULTS, FEEDBACK and NOTICE records on request of the MLAT
//     engine and the admin (Server.SendResults, SendFeedback, Notice).
//
// Pending stations stay connected; their data reaches their own lane only
// (the ingest checks the registry's admission state per frame). A NOTICE
// "approved" is sent when the admin approves them.
//
// Raw IP addresses stay in memory (limits, clone guard, admin session list);
// the registry's connection log stores only a salted hash.
//
// Host-time plausibility (hostguard.go, finding ROB-1): when even the
// fastest live frame of the last 20-30 s claims to be older than RTT/2 +
// Options.HostLatencySlack, the feeder's host times are earlier than the
// link allows, and the host-clock offset handed to the ingest is corrected
// by the excess (SessionInfo.HostClampMs, Stats.HostClamped).
//
// Selective-mode MINUTE_STATS merge (minutes.go, phase 4 WP-I2): with
// Hooks.OnStationMinute set, the server counts per station and minute (of
// feeder host time) the frames it handed to the ingest, and merges each
// MINUTE_STATS record with them into a StationMinute: the feeder's exact
// counters, the received counts, Shed (what the feeder filtered out in
// selective mode or under FLOW) and Merged. Several records for one minute
// (a feeder restart or reconnect inside the minute) are added up and
// re-published (upsert by station and minute).
//
// MLAT results are counted per receiver (SessionStation.Results) and per
// connection (SessionInfo.Results) for the owner's "results received".
package avfsrv
