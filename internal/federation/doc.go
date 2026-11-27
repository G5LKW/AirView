// Package federation links AirView aggregators ("networks") so that each
// can show the others' aircraft (DESIGN.md §14 phase 5, "Federation
// between aggregators"), and provides federation-safe outbound feeds for
// other networks.
//
// # Model
//
// A Node is one network's federation service. It has an Ed25519 identity
// (avf.Identity; its FeederID is the node ID) and a list of configured
// peers, each named by the node ID the peer must prove. A peer link is one
// AVFED/1 session (PROTOCOL.md), dialled by either side; when both dial,
// the link dialled by the lower node ID survives on both sides.
//
// Once per export interval (default 1 s) the node takes the local merged
// picture (Options.Source, the tracker's unprojected snapshot), converts it
// with publicdto.FromAircraft - a whitelist, so no receiver field can pass
// - and sends every peer the aircraft that changed since the last export
// (all of them at every full refresh), shaped by the peer's send Policy.
// Own MLAT results (PublishMLAT) are flushed every 250 ms to peers whose
// policy shares MLAT. Received aircraft go into a bounded store (the best
// report per address, rebased to the local clock, expiring after the TTL)
// that the server can overlay on the map (Overlay, RemoteAircraft) and,
// optionally, to a model.FrameSink as DecodedUpdates with FeedID
// "fed:<peer node ID>".
//
// # Loop prevention
//
// Every STATE and MLAT record carries its path: the origin node first, the
// sending node last. A receiver drops records whose last hop is not the
// authenticated peer (spoofing), whose path contains itself or a repeated
// node (loop), or that are longer than MaxHops. A sender only forwards
// other networks' data when Config.Transit and the peer's send.transit are
// both on (default off), never to a node already on the path (split
// horizon), never beyond MaxHops, and never for an aircraft its own
// stations hear. Aircraft whose every feed is a "fed:" feed (remote data
// injected into the tracker) are never exported as own data. Among reports
// of about the same age the shorter path wins, so direct data is preferred
// to forwarded data.
//
// # Privacy (DESIGN.md §8)
//
// What a peer receives is at most what the public map shows: the wire
// format encodes publicdto.Aircraft (no station coordinates, distances,
// bearings, receiver lists, signal levels, feeder IDs, SIDs or IPs exist in
// that type), reference data and routes are not sent (the receiver
// enriches itself), predicted positions are replaced by the last real one,
// times have millisecond precision, MLAT fixes carry only the station
// count bucket. Island-guard aircraft (Options.Guard, §8.6) are coarsened
// to the public 0.05° grid or dropped (Policy.Island). Per peer the
// operator can further restrict sharing (no MLAT positions, altitude
// floor, no ground traffic, no military, boxes, exclusion boxes, a delay).
// The handshake and INFO carry only the network name and software version.
//
// # Outbound feeds
//
// FeedConfig adds Beast or JSON feeds for other networks (listen or
// connect). They carry only this network's own aircraft. Beast frames are
// de-duplicated across stations, have a zero timestamp and signal byte, and
// only frames whose CRC resolves to an allowed aircraft of the local
// picture leave; receiver status/position frames, MLAT results and
// backfilled frames never do. JSON is the tar1090 aircraft.json shape of
// publicdto, one file per line.
//
// # Files
//
//   - config.go: Config, PeerConfig, Policy, FeedConfig, Normalize, Check.
//   - handshake.go: TLS configs (ALPN avfed/1) and the mutual handshake.
//   - wire.go: record framing and the STATE / MLAT / PING codecs.
//   - link.go: one peer session (reader, writer, delay FIFO, keepalive).
//   - node.go: Node (listener, dialers, export, receive, MLAT, views).
//   - store.go: the remote aircraft store.
//   - policy.go: picture preparation and the send / receive policies.
//   - outfeed.go: Beast and JSON out-feeds, Mode S CRC.
//   - status.go: Status, the HTTP handlers and LoadMaterial.
//
// Wiring into the engine, server and configuration is described in
// WIRING.md; the wire format in PROTOCOL.md. Everything here is AirView's
// own code from public specifications (see docs/aggregator/CLEANROOM.md).
package federation
