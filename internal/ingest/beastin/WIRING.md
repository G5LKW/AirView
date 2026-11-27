# internal/ingest/beastin: wiring notes (WP-B2, DESIGN §5.1)

Compat Beast ingest on TCP 30004 for unmodified readsb / dump1090-fa / Radarcape feeders
(`beast_out`, `beast_reduce_out`, `beast_reduce_plus_out`). **Map and statistics only, never MLAT.**

## 1. Construction (engine, aggregator mode, `aggregator.beastPort > 0`)

```go
opt := beastin.OptionsFromConfig(cfg.Aggregator)   // addr ":<beastPort>", limits, handshake timeout, trustedRelays
opt.ServerKey = registry.ServerKey()               // persistent HMAC key (unidentified IP keys survive restarts)
opt.Now       = ntp.Now
opt.Logger    = logger.With("component", "beastin") // beastin never logs raw IPs
opt.Gate      = func() bool { return accounts.AdminReady(ctx) } // DESIGN §7: closed until the admin exists
opt.Budget    = ingest.Budget()                     // optional global ingest semaphore
srv, err := beastin.New(opt, regAdapter, ingestSink)  // beastin.Registry, beastin.Sink
go srv.ListenAndServe(ctx)                          // returns nil after ctx cancel / srv.Close()
...
srv.SetPolicy(beastin.PolicyFromConfig(next.Aggregator)) // on ApplyConfig; existing links pick it up
srv.Close()                                         // waits until every frame was delivered and detached
```

Until `internal/ingest` exists, `beastin.FrameSinkAdapter{Sink: trackerOrTee}` feeds frames straight into
a `model.FrameSink`, and `beastin.NewMemRegistry()` gives every identity a station (open admission).

## 2. Registry contract

`Attach(ctx, Identity) (Attachment, error)` / `Detach(Identity, Attachment)`:

| Identity | Key (`Identity.Key()`) | Meaning |
|---|---|---|
| `UUID != ""`, `Relay == false` | `uuid:<32 hex>` | the feeder's station (UUID normalised by `model.NormalizeUUID`; readsb's 36-char padding cut to 32) |
| `Relay == true` | `uuid:<uuid>/rx:<16 hex>` | sub-station of a trusted relay (0xE3 receiver ID) |
| `UUID == ""` | `ip:<base32>` (slot 0) or `ip:<base32>/<slot>` | unidentified; `IPKey` = HMAC-SHA256(serverKey, ip16 ‖ listen port)[:10]; concurrent UUID-less links of one address get slots 1, 2, … |

- `Identity.RemoteIP` is private (claims "UUID + same IP", Feed Check). `Identity.IPKey` lets the registry
  join a UUID-less link to a station on the same address (`link_guess`, DESIGN §3).
- Return `Admission` `approved` or `pending` (frames flow; the ingest keeps pending stations in their own
  lane) or `denied`/`revoked`/an error (the link is closed, the identity cooled down for
  `Policy.RefusedCoolDown`, 1 min). `FeedID` is stamped on frames (`st/<sid>/<rx>`); `EpochBase` continues
  the epoch numbering.
- `Detach` is called exactly once per admitted `Attach`, after that station's last frames were delivered.
- A link that sends a UUID after it was attached unidentified is re-attached (old station detached after its
  frames); a relay's sub-stations are attached lazily on their first 0xE3.

## 3. Sink contract

`HandleBeast(*Batch)` from the delivery pool; per link in order, never concurrently for one link.
`Batch.Frames[i].Station` / `.FeedID` are set; `Data` is owned by the frame; the slice is reused after return.
`Batch.Events` are clock epoch changes (`At` = index of the first frame of the new epoch).

**`Batch.MapStatsOnly` is always true**: these frames must not reach the clock filter, `mlat.Observe` or the
survey. Their `ClockCls`/`Epoch`/`Ticks` may be used for latency statistics. `Options.StripClock` clears them
if a consumer cannot tell. MLAT result frames (magic timestamp) are dropped unless `AcceptMLATResults`.

## 4. Protocol behaviour

| Input | Handling |
|---|---|
| `0x1A 0xE4` + 36 B | UUID → attach (or re-attach) |
| `0x1A 0xE3` + 8 B | trusted relay: switch to that receiver's sub-station; otherwise counted (`relayIgnored`) and ignored |
| `0x1A 'W' 'O'` | start own pings every `PingInterval` (5 s); RTT from the echo (`rttLastMs/minMs/avgMs`) |
| `0x1A 'P'` + 3 B | an echo of one of our 8 outstanding pings → RTT; anything else is echoed verbatim |
| other `0x1A` types | counted (`unknownEscapes`), skipped; the link is never closed for content |
| type `'5'` position | ignored (DESIGN §5.1: no location on 30004) |

Sent to the feeder: ping echoes, own pings, `'W' 'S'` (slow down; at most every 10 s) when the frame or byte
limit is crossed in a second or the delivery queue is more than half full.

## 5. Limits (Policy; defaults from DESIGN §4.6 / `aggregator.limits`)

- Per station 6000 frames/s (frames above it shed), per link 200 kB/s × stations on the link. Over the limit
  for 5 consecutive seconds → disconnect, identity cooled down 5 min.
- Per IP 16 links, 4 links not yet attached, 8 stations (relay sub-stations not counted); per /24 or /48 32
  links; `allowCidrs` exempt. `MaxLinks` 4096 overall.
- Silent links (no byte in `HandshakeTimeout`, 10 s) and idle links (`IdleTimeout`, 3 min) are closed.

## 6. Observability

`srv.Stats()` (server totals, rejections and disconnects by reason) and `srv.Links()` / `srv.Link(id)`
(per link; contains the remote address and UUID: **admin scope only**). `srv.Disconnect(id)` for an admin
"kick".

## 7. Tests and tools

```
go test ./internal/ingest/beastin                         # unit + simnet replay + 1,000-link load test
go test ./internal/ingest/beastin -short                  # without the load test
BEASTIN_LOAD_LINKS=2000 BEASTIN_LOAD_SECONDS=20 go test ./internal/ingest/beastin -run Load -v
go test ./internal/ingest/beastin -run '^$' -bench .      # ns/frame (reader path; loopback end to end)
go test ./internal/ingest/beastin -run '^$' -fuzz FuzzLinkConsume -fuzztime 60s -fuzzminimizetime=50x
go run ./internal/ingest/beastin/cmd/beastin-load -serve 127.0.0.1:18513 -duration 80s   # separate processes:
go run ./internal/ingest/beastin/cmd/beastin-load -load 127.0.0.1:18513 -links 1000 -duration 60s
```

Measured (i9-13900K, Windows 11, loopback, server and load generator in separate processes; simnet
reduce_plus streams, ~790 frames/s and 16.8 kB/s per link): 1,000 links, 790k frames/s, 16.8 MB/s,
server CPU 27-32 % of one core, heap 95-117 MB, Go sys 150-160 MB, 1,036 goroutines, no drops.
Reader path 63 ns/frame with one allocation per frame (`BenchmarkLinkConsume`).

## 8. Open items

- The readsb ping direction is to be confirmed from a capture of the user's feeder (DESIGN §5.1 open item);
  the implementation works either way (echo unknown pings, measure echoes of our own).
- Replay of the user's own `beast_reduce_plus_out` capture (DESIGN §13.3) once one exists: drop it into
  `testdata/` and replay it like `TestSimnetStreams`.

## Integrator additions (after Wave 1A)

- `OptionsFromConfig` now also maps the `aggregator.beast` block (identifyWaitMs, idleTimeoutSec, pingIntervalSec, slowDownEverySec, queueBatches, maxLinks, acceptMlatResults, stripClock); `PolicyFromConfig` maps `overLimitSeconds` and `coolDownSec` (0 = no cool-down).
- The registry adapter is `registry.(*Registry).Beast()`; pass `reg.IngestKey()` as `Options.ServerKey`.
