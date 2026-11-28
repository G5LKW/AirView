// Package uplink is the feeding side of AirView Network: it sends a
// station's receiver data to one or more aggregators over AVF/1
// (docs/aggregator/AVF.md, DESIGN §4, §12) and brings MLAT results back.
//
// It is shared by the main airview binary (standalone and aggregator modes,
// Config.Uplinks) and by airview-feeder. The wire format, TLS, handshake and
// record stream are internal/avf; this package decides what is sent, when,
// and what happens to data the aggregator has not acknowledged.
//
// # Pieces
//
//   - Manager (manager.go): runs every configured uplink, is the uplink
//     branch of the frame tee (model.FrameSink) and serves the status API
//     for UIs (ServeHTTP: GET status, POST claim / retry).
//   - Client (client.go, session.go): one uplink. It encodes local frames
//     into FRAMES records per receiver and clock epoch, announces receivers
//     (RECEIVER), clock epochs (CLOCK_EVENT), clock pairs, input status,
//     Radarcape status and optional GNSS positions, and runs the connection
//     loop: dial, handshake (Ed25519 identity, enrol token, resume token),
//     exponential backoff 1 s..60 s ±20 % honouring reconnectIn, stop codes
//     (enrol_required, revoked, cloned_feeder, ...), liveness.
//   - queue (queue.go) and spool (spool.go): the RAM ring of 10 minutes and
//     the optional disk spool of segment files (Uplink.SpoolMB, written only
//     while disconnected). After a reconnect, records the server's
//     WELCOME.ackSeq does not cover are renumbered above it: those younger
//     than the 30 s resume window are resent live, older ones are backfilled
//     inside BACKFILL begin/end with the late bit on every frame, at most a
//     quarter of the link rate, after live data.
//   - selFilter (filter.go): selective traffic (INTEREST sync/mlat sets,
//     reduce-style for everything else) and FLOW slowFactor; minuteStats
//     (stats.go) keeps MINUTE_STATS exact before any filtering. A minute's
//     frames are sent before its MINUTE_STATS, and the minute in progress
//     is sent when the uplink closes; the server adds parts of one minute
//     up (selective-mode merge, internal/ingest/avfsrv minutes.go).
//   - Beast outputs (beast.go, beast_reduce.go, manager_beast.go): "also
//     feed others" (DESIGN §12.1 phase 5). Manager.ApplyBeast runs Beast
//     TCP clients next to the AVF/1 uplinks, compatible with readsb's
//     beast_reduce_plus_out (0xE4 UUID, 'W' 'O', reduce filter, ping echo,
//     'W' 'S' slow-down), beast_reduce_out and beast_out. They are live only
//     (no buffering, no backfill) and apply the same never-forwarded rules.
//     An external mlat-client runs alongside unchanged: it reads the same
//     receiver, and its results (magic timestamp) are never forwarded.
//   - MLAT outputs (mlatout.go, manager_mlat.go): the client side of the
//     mlat-client protocol (docs/aggregator/MLATC.md), so the station can
//     take part in a community network's multilateration next to a Beast
//     output to the same network ("uplinks[]" entries of type "mlat").
//     12 MHz receiver clocks only; results are injected as external MLAT
//     positions (FeedID "uplink/mlat/<id>") and never forwarded.
//   - FixUpdate (results.go): MLAT_RESULTS become model.DecodedUpdates with
//     FeedID "uplink/<id>" and are injected into the local tracker; they are
//     never forwarded upstream.
//
// # Never forwarded
//
// Whatever the configuration, an uplink never sends: frames carrying the
// MLAT magic timestamp or the MLAT flag; frames of feeds that are not
// Source.Forwardable (MLATOnly, NoForward, demo, hub); injected results;
// late (backfilled) input frames; and, as timed frames, frames with a zero
// or missing receiver timestamp (they go out with tsInvalid, or not at all
// for MLAT updates). With Uplink.Timestamps off every frame is untimed and
// no clock records are sent.
//
// See WIRING.md for the integration with internal/engine, the server and
// airview-feeder. The test server stub is internal/uplink/avftest.
package uplink
