# internal/ingest: wiring notes (WP-I, DESIGN §4.4-4.6, §6.2)

The station-aware front end of the aggregator's global tracker. Aggregator mode only; standalone keeps
the tracker's own de-duplication. Protocol neutral: `internal/ingest/avfsrv` (AVF/1, 30160),
`internal/ingest/beastin` (Beast 30004), the host station's feeds and bridges all deliver here.

## 1. Construction (engine, WP-E)

```go
io := ingest.OptionsFromConfig(cfg.Aggregator)  // Limits (IngestFPS, SelectiveAbove, MaxFPS/KBps per station)
io.Handler  = tr                 // tracker.NetHandler (*tracker.Tracker after tr.SetNetwork(reg.Resolver(), lanes))
io.Resolver = reg.Resolver()     // admission per frame: pending -> own lane only, denied/revoked -> dropped
io.Now      = ntp.Now
io.Logger   = log.With("component", "ingest")
// phase 2: io.MLAT = mlatEngine (ingest.MLATSink [+ LateObserver]); io.Clock = clockFilter (ingest.ClockView);
//          io.ClockObserver = clockFilter (ingest.ClockObserver)
ing, err := ingest.New(io)
go ing.Run(ctx)                  // sweeper (10 ms) + session tick (1 s); flushes on ctx done
ing.SetLimits(next.Aggregator.Limits)   // on ApplyConfig
```

Feeding it:

| Source | Call |
|---|---|
| host station feeds (feed manager sink in aggregator mode) | `ing.HandleFrame(f)` / `ing.HandleUpdate(u)` (`Station` 0 = `model.HostStation`) |
| Beast 30004 (`beastin`) | pass `ing` as the `beastin.Sink`, `ing.Budget()` as `beastin.Options.Budget` |
| AVF 30160 | `avfsrv.New(opts, reg, ing)` (see `avfsrv/WIRING.md`) |
| SBS-in bridge / own MLAT results | `ing.HandleUpdate(u)` with `Src` = `mlat-ext` / `mlat-av` (go straight to the tracker) |
| any other link | `ing.Submit(frames, mapOnly)` + `ing.OpenSession(...)` for FLOW/INTEREST |

## 2. What happens to a frame

1. `Station` 0 becomes `HostStation`; MLAT-result and receiver-status frames go straight to `Handle`.
2. Admission (resolver): pending -> `Lanes.Backfill` (own lane counters only, never map/MLAT); denied, revoked
   or unknown -> dropped.
3. `Late` (backfill) -> `Handle` with `Late` (lanes only).
4. Corrected time (station.go): ticks mapped to the feeder's host time by the lower envelope of (host − tick)
   per clock epoch (removes USB-block / batching jitter), minus the feeder's host-clock offset (AVF PING/PONG,
   else the arrival envelope), minus the station's learned relative latency (from shared position squitters).
   Frames older than `LateAfter` (default 2.5 s) are routed as late.
5. Grouper: 64-bit payload hash + payload bytes, sharded 64 ways. First copy -> `Handle` (outside the shard
   lock); later copies -> `Lanes().Credit` with the cached `Result` (credits owed while `Handle` runs are paid
   when it returns). Same-station repeat rule: a station already in the group starts a new group unless its
   ticks are within 32 µs (other channel of a diversity receiver; untimed: other channel within 1 ms of host
   time). Match tolerance 5 ms + 3·(spread₁+spread₂), clamped to 10-300 ms; position squitters (unique
   payloads) also join up to 2.5 s off, so uncalibrated stations calibrate. With a `ClockView`, synced
   stations must also agree in network time within d/c + 5σ.
   Per-copy cost is O(1) expected at any group size up to `MaxCopies` (64; dense 1,000-feeder networks
   average ~63 copies per transmission): "is this station already in the group" is answered by a
   1024-bit blocked Bloom filter (1.6 % false positives at 64 stations, `TestStationFilterFalsePositives`)
   and, once a group holds more than 8 copies, by a pooled 128-slot station index (`TestStationIndexMatchesScan`).
   `BenchmarkGrouperDenseParallel/64` went from ~400 to ~75 ns per copy (WP-SCALE-tracker).
6. Groups: flushed to `MLATSink.Observe` 400 ms after the first copy's corrected time when they have ≥ 2
   timed copies from different stations (or 1 copy of a sync candidate from a synced station); copies that
   arrive later within the 1 s window go to `LateObserver.ObserveLate`; the group is kept 1 s more for
   de-duplication. Beast 30004 copies, copies of stations with `timestamps:false` and the second channel of
   a station are never timed. The sink is called from the sweeper goroutine only; `*Group` is valid during
   the call (`Clone` to keep it).

7. Adaptive flush (H-clock, DESIGN §9.3): `io.Flush = group.NewFlushEstimator()` (any `ingest.FlushPolicy`)
   replaces the fixed 400 ms by the aircraft's delay (`Delay(key)`, clamped to 100 ms..`Window`). The sweeper
   feeds it the delivery latency (server arrival − corrected antenna time) of every copy of one group in
   `FlushSampleEvery` (default 4) and, through `HearerNoter`, the stations that heard the aircraft; nothing else
   needs to call it. Cost: one map lookup + one mutex per new group; 0 allocations per copy.
8. Synced-station gate (H-clock): tolerance `d/c + max(5σ, ClockGateFloor)` (default 50 µs), so it separates
   identical transmissions (≥ 64 µs apart) but never the copies of a clock that drifted by a few σ (HOOKS
   WP-M-engine -> internal/ingest item 1). A `ClockView` that also implements `ClockKindView`
   (`clock.GateView`) gets the frame kind and answers with σ including the receiver jitter.
9. Re-delivery (H-clock): a group flushed with one timed copy (sync candidate of a synced station) that later gets
   timed copies of other stations within the window is delivered again through `Observe` with every copy and
   `Group.Redelivery = true` (no late delivery for it); `Stats.Redelivered` counts them. Sinks should skip
   per-message side effects (reference tracks) for re-deliveries.

Deviation from DESIGN §6.2: the repeat-rule threshold for timed copies is 32 µs, not 1 ms: the simulator
(and real ACAS / DF4-20 interrogation pairs) produces identical replies 0.74 ms apart, and a reply lasts at
least 64 µs, so one clock cannot stamp two transmissions closer than that.

## 3. Sessions, FLOW, INTEREST, budget

```go
s := ing.OpenSession(ingest.SessionInfo{Proto: "avf", Label: feederID, Controller: ctl, Selective: true})
mode := s.AddStation(idx)        // start mode: "full" or "selective"
s.SetHostOffset(off, rtt)        // PING/PONG (feeder minus server)
s.AddBytes(n); s.SetQueueFill(f) // once a second
s.Close(30 * time.Second)        // resume grace: stays "online" for the mode policy
```

Once a second (`Tick`): approved online stations (open sessions, sessions within their resume grace, and
session-less stations such as Beast 30004 links that delivered frames in the last minute) >
`Limits.SelectiveAbove` -> every selective-capable
station gets `INTEREST{mode: selective, reset, its sets}`; back below -> `full`. Per link
(`avf.FlowMonitor`): rate above `MaxFPSPerStation` / `MaxKBpsPerStation` -> FLOW with a slow factor, still
above after `FlowGrace` (10 s) -> `Disconnect(rate_limited, 300)` (avfsrv cools the feeder down 5 min); queue
> 50 % for 5 s -> FLOW, 5 s more -> forced selective; calm for 30 s -> FLOW lifted. The MLAT planner sends
interest-set deltas with `ing.SetInterest(st, InterestCmd{SyncAdd, MLATDel, ...})`; they are kept and sent
whenever the station is in selective mode.

`ing.Budget()` is the global `IngestFPS` semaphore: Beast 30004 (sheddable) up to 70 %, full-mode AVF
(normal) up to 90 %, selective-mode AVF / host (essential) up to 100 % per second.

## 4. Observability (admin scope)

`ing.Stats()` (copies, unique groups, joins, repeats, dups, late, too old, pending, flushed, budget),
`ing.Sessions()` (per link: queue fill, bytes/s, FLOW, per-station mode and fps; no addresses),
`ing.Timing(st)` (mapping, offset source, learned latency/spread), `ing.Online(st)`, `ing.DesiredMode()`.

## 5. Tests and numbers

```
go test ./internal/ingest                     # unit tests + DESIGN §13.2 gates (simnet) + budget check
go test ./internal/ingest -run '^$' -bench .  # per-copy cost
INGEST_LOAD=1 go test ./internal/ingest -run Simnet1000 -v [-cpu 4]   # 1,000-station replay
```

Measured on the dev machine (i9-13900K, Windows 11, Go 1.27.3):

| Check | Result |
|---|---|
| Gate "DF11 repeat every 0.8-1.2 s, 100 stations, 60 s" | truth 108,475 unique (14,507 DF11) -> grouper 108,476 (14,507 DF11): +0.001 %; gate ±0.1 % |
| Same with 20 % untimed stations, 20 % host clocks off up to ±3 s, 10 % Beast map-only | +0.12 % overall (bound 0.3 %), DF11 +0.08 % |
| 60 SDR/Airspy stations | −0.014 % |
| MLAT groups mixing two transmissions | 0.3-0.5 % (identical replies 0.74 ms apart; MLAT clustering splits them) |
| `BenchmarkGrouperCopy` (normaliser + estimator + grouper + amortised sweep; no-op tracker) | 149 ns/copy, 0 allocs (budget 300 ns, 0 allocs; re-measured by H-clock with the flush policy and gate changes) |
| Same, 32 goroutines | 72 ns/copy throughput |
| 1,000-station simnet replay (1,500 aircraft, 129 copies per transmission, 1.47 M copies/s) | 32 threads: 20.7 M copies/s (no-op tracker), 16.3 M copies/s with the real tracker + lanes; `-cpu 4`: 9.5 M / 6.4 M copies/s = 4.3x real time; unique count exact |
| Live: `avfsrv-load`, 1,000 TLS feeders, 300 aircraft, separate processes on loopback | 520 k frames/s, 2.3 k unique/s, ~1 core server CPU, 0 drops, 0 too old (see avfsrv/WIRING.md) |

No race detector run: the toolchain on the dev machine has no C compiler (`-race` needs cgo).
