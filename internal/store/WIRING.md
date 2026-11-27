# internal/store — wiring notes (aggregator mode, WP-D)

Owner: WP-D. Design: `docs/aggregator/DESIGN.md` §6.4, §6.6, §8.5, §20.2.
Standalone mode is unchanged: `store.Open(dataDir)` opens `airview.db` exactly as before
(plus the new, empty network tables of core migration 4).

## Two databases

| File | Open with | Holds |
|---|---|---|
| `airview.db` | `store.Open` / `OpenWithOptions` (Kind `KindMain`) | merged network data: aircraft, flights, positions, events, records, weather …, plus `flight_station`, `mlat_minute`, `mlat_quality_hour`, `stats_day` |
| `stations.db` | `store.OpenStations(dir, opts)` (Kind `KindStations`) | per-station statistics: `kv`, `stats_minute`, `stats_hour`, `stats_day`, `range_hour`, `coverage`, `signal_hist`, `elev_hist`, `heat`, `receiver_status`, `station_minute`, `station_clock_minute` (+ `latency_minute` from internal/latency) |

- internal/registry opens the same `stations.db` with its own writer connection for its tables
  (`registry_meta, registry_keys, feeders, stations, …`). Both writers use `BEGIN IMMEDIATE` with a
  10 s busy timeout; this package never creates or touches registry tables.
- `Store.Reset` on stations.db returns `ErrResetUnsupported` (it would delete the registry).
- Registered migrations go to airview.db only, except the names in `stationShared` (`latency`).
  Use `store.RegisterMigrationFor(store.KindStations|store.KindMain, name, v, ddl)` to choose.
- Batch operations for tables a kind lacks (flights on stations.db …) fail alone and are counted
  in `Stats().FailedOps`; never send them there.

## Keys (DESIGN §6.6)

`ant = "s<idx>"` (station combined) or `"s<idx>/<ch>"` (channel): `store.AntKey(prefix, ant)`,
`store.StationAntPrefix(idx)` (= `model.StationAntKey(idx, -1)`), `SplitAntKey`, `AntKeyWhere(col, prefix)`
(`ant = 's7' OR ant in ['s7/', 's70')`, so `s70` never matches). FeedIDs `st/<sid>/<rx>`:
`StationFeedRange(sid)`. SID-keyed tables: `sid` column.

Per-station reads on stations.db (`MinuteStats`, `HourStats`, `DayStats`, `RangeHours`) probe the
`(t, ant)` key once per time step (`store.SeriesIn`) instead of scanning every station's rows:
30 days of one station's hours among 1,000 stations: 3.5 ms instead of 62 ms
(`BenchmarkStationHourStats30d`). Use `SeriesIn` in your own per-station SQL too. `heat` in
stations.db is keyed `(ant, cell)`.

## Writers

| Writer | Batch method | Database |
|---|---|---|
| tracker lanes (`LaneOptions.Store`) | `AddMinuteStats`, `AddRangeHours`, `MergeCoverage` … (namespaced ant) | stations.db |
| `LaneOptions.OnFlightStation` | `UpsertFlightStation([]store.FlightStation)` (merge: msgs/positions add, first min, last max, distances/RSSI min/max, rssi_avg msgs-weighted, NaN = NULL) | airview.db |
| ingest (per-station link health) | `AddStationMinutes` | stations.db |
| MLAT clock filter | `AddStationClockMinutes` | stations.db |
| MLAT engine | `AddMLATMinutes` (sid `""` = network), `MergeMLATQuality` (hold-out error histogram, `MLATErrBin`, `Cell1Deg`, percentiles recomputed) | airview.db |

Conversion for the tracker hook (WP-E):

```go
OnFlightStation: func(rows []tracker.FlightStation) {
    out := make([]store.FlightStation, len(rows))
    for i, r := range rows {
        out[i] = store.FlightStation{FlightID: r.FlightID, SID: r.SID, ICAO: r.ICAO, First: r.First, Last: r.Last,
            Msgs: r.Msgs, Positions: r.Positions, MaxDistKm: r.MaxDistKm, MinDistKm: r.MinDistKm,
            RSSIMin: r.RSSIMin, RSSIMax: r.RSSIMax, RSSIAvg: r.RSSIAvg}
    }
    airviewDB.UpsertFlightStation(out)
}
```

Readers: `FlightStations(flightID)` (admin only: receivers per aircraft), `StationFlights(sid, from, to, limit)`,
`MLATMinutes(sid, …)`, `MLATQualityHours(from, to, cell)`, `StationMinutes`, `StationClockMinutes`,
`StationAntKeys(prefix)`.

## Large data (DESIGN §20.2)

- **Tiered retention / downsampling:** `st.Maintain(ctx, store.TierPolicy{...})` on both stores, hourly.
  It runs `RollupHours`, `RollupDays` (stats_hour → stats_day, UTC days), the minute tier (MinuteDays),
  hour tier (HourDays; hours are deleted only after their day is rolled up), day tier (DayDays, 0 = forever),
  histogram month buckets (per-day rows older than HistDailyDays merged into the 1st of their UTC month;
  sums preserved), histogram retention (HistDays), idle heat cells (HeatIdleDays, walk at most daily),
  flight_station rows of flights ended more than FlightStationDays ago (airview.db, default 90),
  incremental vacuum above FreePct free pages, WAL checkpoint above WALCheckpointBytes, `PRAGMA optimize`
  every OptimizeEvery. All steps are chunked (≤ 5,000 rows / one month × 64 keys per transaction),
  idempotent and resumable. `store.DefaultTierPolicy()` = 30 d / 395 d / forever / 35 d per day /
  395 d histograms / 395 d heat. Standalone keeps calling `Prune(cfg.Storage)` as before (Maintain is
  optional there). `Prune` also deletes the `flight_station` rows of pruned flights.
- **Batched writes:** unchanged single-writer queue; stations.db defaults to `FlushRows` 20,000 and
  `MaxQueueRows` 2,000,000 (lane bursts). ~250,000 minute rows/s on NVMe
  (`BenchmarkStationsMinuteFlush`).
- **Compact BLOBs:** stations.db stores `range_hour.dist_hist` / `sector_max` varint-packed
  (`EncodeDistHistCompact`, magic byte 0xC7, length never a multiple of 4); `DecodeDistHist` /
  `DecodeSectorMax` read both forms, airview.db keeps the raw arrays. Read BLOBs only through them.
- **Sizes:** `st.TableSizes(ctx)` (dbstat; reads every page — status pages only).
- **Growth measurement:** `AIRVIEW_GROWTH=1 go test -run TestGrowth ./internal/store/` (see growth_test.go).

## Station deletion and export

- `st.DeleteStationData(ctx, store.StationKeys{Prefix: "s7", SID: sid})` on **both** stores (registry's
  deletion job covers stations.db only; airview.db holds `flight_station` / `mlat_minute` by sid).
- Owner export: `st.ExportStationData(ctx, store.ExportSpec{Keys: …, Minutes: false}, add)` on both stores
  inside `registry.Options.ExportExtra`:

```go
ExportExtra: func(ctx context.Context, ownerID int64, sts []registry.Station, add func(string, any) error) error {
    for _, s := range sts {
        k := store.StationKeys{Prefix: store.StationAntPrefix(s.Idx), SID: s.SID}
        pre := "stations/" + s.SID + "/"
        sub := func(name string, v any) error { return add(pre+name, v) }
        if err := stationsDB.ExportStationData(ctx, store.ExportSpec{Keys: k}, sub); err != nil { return err }
        if err := airviewDB.ExportStationData(ctx, store.ExportSpec{Keys: k}, sub); err != nil { return err }
    }
    return nil
}
```

## Storage profiles, checkpointer, data migrations (1.1, docs/perf/DB-EFFICIENCY-DESIGN.md)

- **Profiles** (`profile.go`): `store.ResolveProfile(name, kind, dataDir)` -> `Options.Profile`.
  `auto` = `server` for stations.db (KindStations; the engine passes `"server"` for the aggregator's
  airview.db), `low-wear` when the data directory is on an MMC/SD device (Linux major 179) or on Linux
  arm/arm64 with <= 1 GiB RAM, else `standard`. The profile sets the group commit (10 s / 30 s / 5 s),
  page caches, read pool, reader idle close, mmap (none on 32-bit), WAL hard cap and journal size limit,
  the checkpointer's soft size / interval, the migration step time, the backup mode and the tracker
  cadences the engine passes on. `/api/db/stats` shows it (`profile`).
- **Checkpointer** (`checkpointer.go`): `wal_autocheckpoint` = hard cap only (16,384 pages; 131,072 on
  `server`); PASSIVE checkpoints on a dedicated connection in the writer's idle slot (soft size 40/48/64 MiB
  or 5/10/1 min; 16/32 MiB of the design measured too small, see profile.go); RESTART after three incomplete runs above twice the soft size; `Checkpoint` (TRUNCATE)
  first runs the registered before-truncate jobs (trail seal). stations.db: the registry's own pool keeps
  its DSN (hard cap set by its owner); this checkpointer serves the file. Bulk work (data-migration
  steps, Prune / Maintain incremental-vacuum steps) checkpoints between its transactions once the WAL
  holds the soft size, so the WAL stays under the hard cap. After the migrations the vacuum tail returns
  2,000 free pages per checkpointer tick (10 s; gen7 in ~9 min, gen31 in ~40 min).
- **Group commit / early flush**: `Options.FlushRows` defaults to 2,000 rows per second of the profile's
  FlushInterval (4,000..60,000: standard 20,000, low-wear 60,000), the 1.0.0 rate, so a busy station
  is not flushed before its interval.
- **Exports**: `/api/db/export` and `/api/db/export/<table>.csv` first write what is held in memory
  (`Tracker.Flush`, and `FlushOpenHour(ctx) error` of the met collector when it implements it), so an
  export holds everything collected so far.
- **Hourly job**: `st.HourlyMaintenance(ctx)` = RollupHours + registered hourly jobs + `PRAGMA optimize`
  (analysis_limit 400) + TRUNCATE checkpoint. The aggregator keeps calling `Maintain` on both stores.
- **Data migrations** (`datamig.go`, `premigrate.go`): registered with `registerDataMigration` by the owner
  files; run in the background 15 s after Open (`Options.MigrationDelay`), progress in
  `schema_data_migrations`, `st.MigrationStatus()` / `MigrationInfo()` / `MigrateNow(ctx)` / `BackupNow(ctx)`.
  Backup `<data>/backup/<db>-pre-1.1-<date>.db` (mode `auto|always|never`), deleted 14 days after completion.
- **Hooks** (`hooks.go`): `registerPruner` (Prune, before the legacy steps), `registerHourly`,
  `registerPeriodic` (checkpointer goroutine, optionally before every TRUNCATE).
- **Logical tables** (`logical.go`): `RegisterLogicalTable`; `st.ScanTable` / `store.ScanTableDB` serve
  `/api/db/export/<table>.csv` and `tools/dbparity` with the 1.0.0 columns and rows.
