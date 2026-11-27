# internal/tracker: wiring guide (aggregator mode)

Standalone mode is unchanged: feeds call `HandleFrame` / `HandleUpdate` (`model.FrameSink`), the
tracker de-duplicates copies itself and keeps per-antenna statistics. Everything below is opt-in
through `SetNetwork` (docs/aggregator/DESIGN.md §6.2-6.4).

## Persistence cadence (both modes, docs/perf/DB-EFFICIENCY-DESIGN.md §3.3)

The tracker writes each stream on its own data-time clock, taken from the storage profile
(`tracker.Options{TrailFlush, IncFlush, HeatFlush, RangePartial}`, 0 = profile `standard`):

| Stream | `standard` | `low-wear` |
|---|---:|---:|
| trail points -> `trail_tail` | 60 s | 60 s |
| coverage / signal_hist / elev_hist increments | 60 s | 300 s |
| heat increments | 300 s | 900 s |
| partial range_hour rows | 15 min | 30 min |
| stats_minute rows | every minute | every minute |
| flight / aircraft rows | checked every 15 s, saved once a minute | same |

A UTC day roll writes the old day's increments before the first position of the new day is
credited; `Flush` writes everything (shutdown, factory reset, antenna change). What is not written
yet is served to readers by `(*Tracker).PendingIncs()` (`store.PendingIncs`), which the engine hands
to analytics as its overlay. The aggregator lanes keep their own cadence (`LaneOptions`).

## Set-up (engine, WP-E)

```go
lanes := tracker.NewLanes(tracker.LaneOptions{
    Resolver:        registry,        // model.StationResolver: Geo for located stations, Info for SIDs
    Store:           stationsDB,      // *store.Store of stations.db (anything with Submit(*store.Batch))
    OnFlightStation: writeFlightStation, // []tracker.FlightStation -> airview.db flight_station (see HOOKS.md)
    // MaxRangeKm 800, FlushEvery 10 s, SlowFlushEvery 5 min, GeoRefresh 30 s, EvictAfter 24 h by default
})
tr := tracker.New(tracker.Options{Store: airviewDB, Config: &cfg, ...})
tr.SetNetwork(registry, lanes) // before frames arrive
go tr.Run(ctx)                 // also drives the lanes' 1 Hz Tick
...
tr.Flush(ctx)                  // shutdown: tracker rows + lanes.FlushAll
```

In aggregator mode the tracker stores **no receiver geometry** (no distKm/bearing/elevation/slant
per aircraft, no distance records, flights' station-relative columns stay NULL), keeps only the
combined statistics slot (station-free counters), and ignores `cfg.Antennas`.

## Ingest (grouper, WP-I)

Code against `tracker.NetHandler` (implemented by `*Tracker`, sharded or not - the ICAO-sharded
tracker is the same `*Tracker`, see Scaling):

```go
r := h.Handle(firstCopy)              // decode + apply; also credits firstCopy.Station's lane
group.result = r                      // cache with the group (plain value, ~100 bytes)
h.Lanes().Credit(st, ch, sig, &group.result) // every later copy; never takes the tracker mutex
```

- `Handle` does **not** de-duplicate: the grouper decides what a transmission is.
- `Result.Status`: `ResultOK` (aircraft `Key`, `Pos` with `Lat/Lon/AltFt`, `FlightID`), `ResultBadCRC`,
  `ResultRejected`, `ResultUnsupported`, `ResultNone` (Mode A/C, status), `ResultLate`.
- Late (backfilled) frames: `Handle` with `RawFrame.Late` set, or `Lanes().Backfill(f)` directly;
  they reach the lanes at their original time (`HostNs`, else `Recv`) and never the live picture.
  Late updates: `HandleUpdate` with `Late` set (or `Lanes().BackfillUpdate`).
- Updates from a station (`DecodedUpdate.Station != 0`, not MLAT) are credited to its lane by
  `HandleUpdate`. Own MLAT results (`Src = mlat-av`, Station 0) credit no lane.
- Do not call `HandleFrame` in aggregator mode (it de-duplicates and credits no lane).
- The `Result` is valid for about a second (the grouper window).

## Reading (server, WP-H)

In aggregator mode **never serve `Snapshot()` / `Aircraft()` directly**: their `feeds` hold
namespaced FeedIDs (`st/<sid>/<rx>`, i.e. SIDs) and `rssi` is the first copy's station.
Use the per-viewer projection:

```go
v := tracker.View{Scope: viewer.Scope, Station: selectedIdx, Own: ownerStationIdxs}
s := tr.SnapshotView(v)           // copy of the 1 Hz snapshot, projected
a, ok := tr.AircraftView(icao, v)
```

| Scope | Gets |
|---|---|
| public | merged picture only; no distKm, bearing, elevation, slantKm, cpa, terrain, rssi, feeds, antennas, mine, stations |
| owner | `mine` + `stations` (own SIDs that heard it in 60 s); geometry for `View.Station` only if it is in `Own` |
| admin | everything, all SIDs of the last 60 s, geometry for any located station |

Terrain line of sight and trajectory CPA from the enrichment hook (relative to the configured
receiver) are kept only for `View.Station == model.HostStation`; for other stations CPA is a
straight-line prediction (≤ 30 min). In standalone mode `SnapshotView` returns `Snapshot()`.

Other read APIs: `tr.HeardBy(icao)` (stations of the last 60 s, operator use and the MLAT
result-return rule), `lanes.Info(idx)` / `lanes.List()` (status pages), `lanes.HeardICAOs(idx,
sinceMs, nil)` ("aircraft my station heard"), `tr.LockStats()` (see Scaling).

## Lanes (per-station statistics, §6.3)

- One lane per station, sharded 256 ways by index (one mutex per shard), created lazily, evicted
  (after a final flush) when silent for 24 h.
- Each lane has the station's combined stats (`ant = "s<idx>"`) and per channel 0..7
  (`"s<idx>/<ch>"`, created on first use) - the same accumulators as a standalone antenna.
- Its own per-minute / per-hour aircraft sets (stamped membership in an open-addressing table)
  give the stats_minute gauges (aircraft, with position, ADS-B / MLAT / Mode S) and
  range_hour.aircraft.
- Counters are always credited; range, coverage, heat, signal and elevation only while the
  station is located (`Resolver.Geo`, re-read every 30 s; HAE converted to MSL with the geoid).
  Accepted positions are buffered and folded in batches (at minute roll, flush, flight close).
- Rows: stats_minute every 10 s per lane (staggered round-robin; counters are deltas, gauges
  cumulative, so the store's merge rules give exact minutes), range_hour and the day tables every
  5 min, the day tables of a finished day at the day roll, late minutes at the next flush.
- `flight_station`: per (flight, station) msgs, positions, first/last, min/max distance (located
  stations), RSSI min/max/avg; emitted through `OnFlightStation` when the tracker closes the
  flight, when the aircraft's flight id changes, or when the lane has not heard the aircraft for
  35 min.

## Locate Me v2 samples (§10.4, WP-L2)

`tr.SetGuessSampleHook(func([]tracker.GuessSample))`: ADS-B positions received off-air (no MLAT,
TIS-B or decoded feeds), at most one per aircraft per 10 s of data time, with RSSI, channel,
antenna ID, altitude, on-ground and station. Delivered about once per second without locks; the
slice is reused after the call.

## Scaling (§6.4, §20.2): measurements and the ICAO-sharding seam

Measured on the dev machine (i9-13900K, Go 1.27.3, 150 simulated aircraft, 100 located stations;
`go test ./internal/tracker -bench 'Network|Lane|HandleFrame'`):

| Path | ns/op | allocs | budget |
|---|---|---|---|
| `HandleFrame` standalone | ~127 | 0 | unchanged |
| `Handle` (first copy + its lane credit) | ~350-370 | 0 (2 B/op amortised) | 700 |
| `Lanes.Credit` (incl. amortised 1 Hz tick: fold, minute rows, day tables) | ~107-113 | 0 (0 B/op) | 120 |
| `Lanes.Credit`, 32 goroutines | ~76 (throughput) | 0 | |
| `Handle`, 1 / 2 / 4 / 8 goroutines | 223 / 377 / 331 / 338 | 0 | |

Where a credit's time goes (CPU profile of `BenchmarkLaneCredit`): ~37 % position fold (WGS84
distance/bearing, elevation, range/coverage/heat accumulators), ~15 % heardBy marks (the benchmark
rotates copies over 100 stations, so every mark evicts from the 16-slot set: the worst case),
~7 % shard mutex. Kept off the per-copy path: the minute gauges are maintained incrementally as
an aircraft's minute bits are first set (a flush never scans the aircraft table), a lane remembers
its station's heardBy slot (re-mark = one load + one CAS), and the day-table rows are built in
per-shard scratch buffers (the store copies them), so steady state allocates nothing.

`TestNetworkBudgets` re-checks both budgets (with a 3x margin for slow CI) and 0 allocs.

**Mutex profile** (`-bench NetworkHandleParallel -cpu 8 -mutexprofile`): 100 % of the contention
is `Tracker.mu` in `Handle`; the lane shard locks contribute ~10 % of the wait. `Handle` does not
scale with goroutines: one tracker saturates at about 3-4.5 M unique transmissions/s on this CPU
(~1 M/s on a 2-4 vCPU VPS estimate). The design load is 30-60 k/s for a large UK network and
roughly 100-200 k/s at 1,000+ feeders, i.e. a mutex utilisation of a few percent to ~20 %.

**Gate.** `tr.LockStats()` times every 1024th handling (`HoldNs`) and reports
`Utilization = HoldNs x msg rate` (fraction of one CPU spent inside the tracker mutex), and for a
sharded tracker `Shards` and `MaxShardUtilization` (the busiest shard). Raise the shard count when
`MaxShardUtilization` stays above 0.5 for minutes on the production host.

### ICAO sharding (phase 5, implemented: `shard.go`, WP-SCALE-tracker)

No new type and no wiring change: `SetNetwork` on a **fresh** aggregator tracker splits it into
N shards and the `*Tracker` the engine holds becomes the front (every exported method delegates).

- **Shard count:** `Options.Shards`, else `aggregator.limits.trackerShards` (file / `AIRVIEW_*` env
  through the config package), else `AutoShards()` = GOMAXPROCS/2 clamped to 1..8 (1 below 4
  CPUs). `MaxShards` = 256. `Options.Shards = 1` forces one tracker. Standalone trackers are never
  sharded (`HandleFrame` pays one atomic pointer load; measured A/B within noise, ~3 %).
- **Routing:** frames by `ShardOf(RouteKey(f.Data), N)` (address from the clear field of
  DF11/17/18, from the CRC residual of DF0/4/5/16/20/21; no address -> shard 0; a frame with bit
  errors routes somewhere and is rejected there, as before); updates and look-ups by ICAO.
  Identical data always reaches the same shard, so standalone-style de-duplication stays exact.
- **Shared:** the `*Lanes` (only shard 0 drives its 1 Hz Tick), the all-time records (one table,
  own mutex, lock-free reads of current values), the flight-id source, trajectories, RF
  statistics and latency analysis. Each shard gets `MaxAircraft/N` + 25 % headroom.
- **Merged on read:** snapshots (shards build in parallel; k-way merge in ICAO order, counts
  summed, enrichment hook once, cached per tick), counters, feed latency (each feed sampled by
  one shard), lock statistics, trails.
- **Merged on write:** the combined slot's `stats_minute` rows (`minuteMerger`: one row per minute,
  counters and disjoint gauges summed, RSSI/range combined; a minute stalled 2 min of data time
  is written as is), so stats_minute matches an unsharded tracker (`TestShardedMatchesUnsharded`).
  Records of totals (most aircraft at once, peak message rate) from all shards' latest values.
- **Hooks** that promise "never concurrently with itself" (EHS, new airframe, guess samples) are
  serialised across shards. `SetNetwork(nil, nil)` flushes and drops the shards.

**Measurements** (i9-13900K, 32 threads; the final runs shared the machine with other build
agents at ~57 % CPU load, so absolute numbers are 10-40 % above the quiet-machine baseline;
compare rows of one run):

| Benchmark | before (quiet) | after (loaded) |
|---|---|---|
| `HandleFrame/base` standalone | 126 ns | 179-187 ns; A/B same load with/without the shard check: 184 vs 178 ns |
| `NetworkHandle` (1 shard) | 359 ns | 480-495 ns |
| `NetworkHandleParallel` 1 shard / 8 shards (auto) | 344 ns / - | 505-543 ns / 168-170 ns |
| `ShardedHandleParallel` 2000 aircraft, 1/4/8/16 shards | - | 860-910 / 280-288 / 168-176 / 117-130 ns |
| `ShardedSnapshot` 10k aircraft, 1/8/16 shards | 7.5 ms (1) | 5.7-6.0 / 3.1-3.3 / 2.8-3.2 ms |
| `ShardedTick` 10k aircraft, longest shard hold 1/8/16 | - | 480-530 / 34-47 / 17-30 µs |

1,000-feeder simnet (`INGEST_LOAD=1 INGEST_LOAD_AIRCRAFT=10000 go test ./internal/ingest -run
Simnet1000 -v`; 2 x 5,000-aircraft networks, 27.2 M copies of 433 k transmissions = 4.70 M
copies/s, 75 k unique/s, 62.7 copies per transmission; grouper vs truth -0.019 %):

| Run | unpaced throughput | paced CPU (real time) | snapshot | busiest shard |
|---|---|---|---|---|
| no-op tracker (grouper only) | 14.6 M copies/s | 3.3 cores | - | - |
| real tracker + lanes, 1 shard | 10.5 M copies/s (2.2x real time) | 5.9 cores (1,251 ns CPU/copy) | 9.3-10.6 ms | 16-18 % of a core |
| real tracker + lanes, 8 shards | 11.2 M copies/s (2.4x real time) | 5.2 cores (1,106 ns CPU/copy) | 5.9-7.5 ms | 3-7 % of a core |

Total CPU is the same within run-to-run noise (an earlier, quieter paced run: 4.8 cores 1 shard,
5.2 cores 8 shards); budget ~5-6 cores of this CPU for 1,000 feeders x 10k aircraft at full
copy fan-out. The tracker mutex is not the bottleneck at this size (18 % of one core unsharded); sharding
buys headroom (5x lower per-shard utilisation), halves snapshot and 1 Hz tick holds, and lifts
contended `Handle` throughput 3x. Lane credits: the CPU profile shows `Lanes.Credit` mutex wait
at 0.1 s of 61 s (256 lane shards), so batched credits (`CreditBatch`) are **not** wired; the
remaining contention (~6 % of samples) is the grouper shard mutex, taken by all ~63 copies of one
transmission, which no shard count removes.

## Integrator additions (after Wave 1A)

- `tracker.LaneOptionsFromConfig(cfg.Aggregator)` maps the `aggregator.lanes` block (flushSec, slowFlushSec, evictHours, maxRangeKm) onto `LaneOptions`; set Resolver/Store/OnFlightStation on the result and pass the same `MaxRangeKm` as `Options.MaxRangeKm`.
