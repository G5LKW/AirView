// Package privacy holds the pieces of aggregator-mode privacy enforcement
// (docs/aggregator/DESIGN.md §8) that are not tied to the HTTP server:
//
//   - publicdto: the public data-transfer types. They have no fields for
//     station coordinates, distances, bearings, receiver lists, signal
//     levels, IP addresses or raw timestamps, so a public handler cannot
//     leak them by accident: it can only copy what the type can hold.
//   - Strip* helpers that remove receiver-related fields from the full
//     model types where a model type has to be served to a non-admin
//     viewer (defence in depth on top of the tracker's projection).
//   - Fixture / Scan: the leak detector used by the golden privacy tests.
//     It looks for fixture coordinates (3 decimals and common scaled
//     integer forms), any IPv4 address, the fixture IPv6 addresses, SIDs of
//     hidden stations, ns-precision timestamps and the forbidden JSON keys
//     in a response body.
//   - IslandGuard: the optional coarsening of low aircraft heard only by
//     hidden-pin stations (§8.6).
//
// Nothing in this package knows about sessions or routes; the server
// decides the viewer and calls into here.
package privacy
