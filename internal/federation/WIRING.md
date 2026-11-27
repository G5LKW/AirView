# internal/federation — wiring notes (WP-FED-lib)

Federation between AirView aggregators (DESIGN.md §14 phase 5, §19.4) plus federation-safe out-feeds.
Wire format: PROTOCOL.md (AVFED/1). Imports only the standard library, `internal/avf`, `internal/model`,
`internal/mlat/geo` and `internal/privacy` (+ `publicdto`); it must never import engine, server or
config (they import it). Status of the wiring requests: docs/aggregator/HOOKS.md ("WP-FED-lib -> ...").

## 1. Configuration (`aggregator.federation`)

`federation.Config` is the block as it appears in the config file (JSON tags are the file keys). The
model WP mirrors it as `model.AggFederation` with identical JSON (engine converts by JSON round trip or
field copy; a test should compare the two key sets). Every key is optional; `Normalize` fills defaults
and `Check("aggregator.federation")` returns one `path: problem` line per error for `airview config check`.

```jsonc
"aggregator": { "federation": {
  "enabled": false,
  "name": "",                    // network name sent to peers
  "listen": "",                  // e.g. ":30170"; "" = only dial out
  "identityFile": "",            // default <data>/federation.key (0600)
  "tlsCert": "", "tlsKey": "",   // default self-signed in <data>/fed-tls
  "intervalMs": 1000,            // state export period (100..60000)
  "mlatIntervalMs": 250,         // MLAT flush period (50..10000)
  "fullRefreshS": 30,            // full snapshot period (1..3600)
  "ttlS": 60,                    // remote aircraft lifetime (5..3600)
  "transit": false,              // forward other peers' data (also needs peers[].send.transit)
  "maxHops": 3,                  // path length incl. origin (1..8)
  "maxRemoteAircraft": 50000,    // store bound (100..1e6)
  "maxInflateBps": 16000000,     // per-peer decompressed rate guard
  "queueRecords": 256,           // per-peer send queue (8..100000)
  "hideRemote": false,           // keep received aircraft off the map
  "inject": false,               // also hand received data to the tracker ("fed:<peer>" feeds)
  "peers": [{
    "name": "neighbour-net",
    "id": "abcdefghijklmnop",    // the peer's node ID (`airview federation id` on their side)
    "addr": "fed.example.net:30170",   // "" = accept only
    "pin": "",                   // "" (identity check only) | "webpki" | "sha256/<b64>"
    "disabled": false,
    "send":    { "aircraft": "all|nomlat|none", "mlat": false, "transit": false,
                 "island": "coarsen|drop", "minAltFt": 0, "noGround": false, "noMilitary": false,
                 "boxes": [{"south":49,"west":-11,"north":61,"east":2}], "excludeBoxes": [],
                 "delayS": 0, "maxAircraft": 0 },
    "receive": { "aircraft": "all|none", "mlat": "all|none", "boxes": [], "maxAircraft": 20000 }
  }],
  "feeds": [{
    "name": "to-other-network", "format": "beast|json", "mode": "connect|listen",
    "addr": "feed.example.org:30004", "allow": ["127.0.0.0/8", "::1/128"], "maxClients": 16,
    "intervalMs": 1000, "modeAC": false, "disabled": false,
    "policy": { "aircraft": "all|nomlat|none", "island": "coarsen|drop", "minAltFt": 0, "noGround": false,
                "noMilitary": false, "boxes": [], "excludeBoxes": [], "maxAircraft": 0 }
  }]
}}
```

Environment (generic layer of internal/config): `AIRVIEW_AGGREGATOR_FEDERATION_ENABLED`, `..._LISTEN`,
`..._NAME`, `..._TRANSIT`, `..._MAX_HOPS`, ... and indexed peers/feeds (`..._PEERS_0_ID`,
`..._PEERS_0_ADDR`, `..._PEERS_0_SEND_MLAT`, `..._FEEDS_0_ADDR`, ...), like the other list blocks.

## 2. Engine (aggregator mode only)

```go
fc := federation.Config(cfg.Aggregator.Federation)       // or JSON round trip
if fc.Enabled {
    m, err := federation.LoadMaterial(dataDir, &fc, []string{cfg.Aggregator.PublicHost})
    // log m.Identity.FeederID() (node ID) and m.Pin once; never log peer data
    opts := federation.Options{
        Identity: m.Identity, Certificate: m.Certificate, Version: version.String(),
        Source: tracker.Snapshot,                // UNPROJECTED snapshot; the package projects it itself
        Guard:  islandGuard,                     // the same predicate the server uses for the public map (nil when off)
        Logf:   logger.Printf,
    }
    if fc.Inject { opts.Sink = tracker }          // remote data as DecodedUpdate, FeedID "fed:<peer>", Station 0
    node, err := federation.New(fc, opts)
    err = node.Start(ctx)                         // listener bound when it returns
    defer node.Close()                            // GOODBYE shutting_down to every peer
}
```

- **MLAT**: wherever the engine hands own MLAT results to feeders (the `WriteMLATResults` path), also call
  `node.PublishMLAT(fixes)` (non-blocking; only grades A-C, no hold-out fixes leave).
- **Beast out-feeds**: if `node.WantsFrames()`, tee every merged ingest frame into `node.HandleFrame(f)`
  (≈20 ns per frame on the reject path; it de-duplicates across stations itself). `Node` also implements
  `model.FrameSink` (HandleUpdate is a no-op), so it can sit in an existing tee.
- **Inject mode**: the tracker must keep `Aircraft.Feeds` populated with the `fed:` FeedIDs so `prepare`
  can drop federation-only aircraft from the export (loop prevention). Station 0 updates must not count as
  the host station's reception in stats/coverage; until the tracker distinguishes them, keep `inject` off.
- **Config reload**: rebuild the node (Close + New + Start); links re-establish within seconds.

## 3. Server

- `GET /api/admin/federation` → `node.StatusHandler()` (admin scope: it lists peer addresses).
- Public and owner map (aggregator mode, `!hideRemote`): after building the `publicdto.Snapshot`, call
  `node.Overlay(&snap, enrich)` (enrich fills registration/type from the local reference data; may be nil).
  Overlaid aircraft are already public-safe; the island guard does not apply to them (the origin applied
  its own).
- Optional polling endpoint for partner networks: `node.AircraftJSONHandler(policy)` serves the tar1090
  `aircraft.json` shape of the local picture through a policy (public scope; the public rate limit applies).

## 4. CLI

`airview federation id [-data DIR] [-config FILE]`: prints the node ID and, when `listen` is set, the TLS
pin (`federation.LoadMaterial`). Operators exchange node IDs (and optionally pins) out of band.

## 5. Behaviour and numbers

- Export: changed aircraft every interval, everything every `fullRefreshS` and after a dropped batch;
  a full per-peer send queue drops the batch (counted as `dropped`) and schedules a full refresh.
- Delay peers: records wait in a per-link FIFO (bounded to 64 MiB, oldest dropped).
- Links: PING every 5 s both ways (RTT in the status), 30 s idle timeout, back-off 1 s → 60 s (±20 %),
  5 min after a denial.
- Measured (i9-13900K, `go test -bench . ./internal/federation`): 10k aircraft encode 0.76 ms
  (58 B/aircraft before DEFLATE, 0 allocs), decode 3.4 ms, one full export cycle to one peer 2.3 ms; a live
  link carries 10k aircraft changing every second in ≈133 kB/s of wire bytes, all 10k delivered within one
  export cycle (`TestScale10k`). Beast out-feed: ≈17 ns per rejected frame, no allocations.
- Out-feeds carry only this network's own aircraft, never data received from peers.
