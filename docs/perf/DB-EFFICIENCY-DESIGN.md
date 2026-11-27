# Station database efficiency: design (AirView 1.1 storage)

Status: design, 2026-11-15. Input: `docs/perf/DB-BASELINE.md` (all "baseline"
numbers below come from it). Nothing in this document has been implemented
yet; the numbers marked *target* are acceptance criteria, the numbers marked
*est.* are estimates derived from the baseline measurements.

Goal: make the station database as cheap as possible on a Raspberry Pi with
an SD card - disk size, bytes written and fsyncs per hour (card wear), CPU,
RAM and read latency - **without removing or degrading any feature**, and
apply the same gains on every platform (Windows station, Pi 32/64-bit,
Linux VPS aggregator with `stations.db`).

## 0. Ground rules every change must keep

1. **Feature parity.** Every API answer, CSV/GeoJSON export and table export
   (`/api/db/export/<table>.csv`) of the new build equals the 1.0.0 answer on
   the same data. The only allowed differences are the ones listed in §9
   (each is a strict improvement, e.g. a 24 h replay that is no longer
   truncated) and they get a named tolerance in `tools/dbparity`.
2. **Lossless storage.** Trail points, histograms and grid cells are stored
   bit-exact (re-encoded, never rounded). Where a REAL column is re-encoded
   as a scaled integer (met observations only, §3.9) the scale is finer than
   the source resolution and the API output is unchanged at its printed
   precision.
3. **Crash safety.** `journal_mode=WAL` + `synchronous=NORMAL` stays the only
   durability mode. Never `synchronous=OFF`, never `journal_mode=MEMORY/OFF`,
   never `locking_mode=EXCLUSIVE` with WAL disabled. An application crash, a
   kill -9, a power cut or a full disk never corrupts the file. What a power
   cut may lose is bounded and documented per profile (§4.3).
4. **Old data preserved.** Migration is automatic, in the background,
   resumable, non-destructive per step (every step is one transaction that
   writes the new form and removes the old form of the same rows), with a
   disk-space check and an optional pre-migration backup (§5). The station
   stays fully usable while it migrates: every reader understands both
   layouts until the migration is marked done.
5. Pure Go, `CGO_ENABLED=0`, `modernc.org/sqlite` v1.62.0, no new module.
   Deflate comes from `compress/flate` (standard library).

## 1. Targets

Workload for every write/latency target: the baseline "rt-7d" run (copy of
the 7-day gen7 database, `-demo` with `demo.aircraft=1300`, 170-250 live
aircraft, 900-1,100 msg/s, `GOMAXPROCS=4`, `GOMEMLIMIT=600MiB`, steady state
minutes 11-22), on the i9/NVMe host. Disk targets use the same generators
(gen7, gen7busy, gen31) as the baseline.

| Metric | Baseline (1.0.0) | Target, profile `standard` (Windows, x86 Linux) | Target, profile `low-wear` (Pi, SD card) |
|---|---:|---:|---:|
| 7-day database, 250 peak (gen7) | 553 MB | **<= 150 MB** | same |
| 7-day database, 450 peak (gen7busy) | 941 MB | **<= 240 MB** | same |
| 30-day plateau (gen31 + met/guess projection) | 2.41 GB file (2.9 GB projected with met_obs/guess) | **<= 0.85 GB** | same |
| 1 year / 2 years (projection, method of baseline §4.3) | 3.6 GB / 4.3 GB | **<= 1.45 GB / <= 1.9 GB** | same |
| All bytes written by the process | 747 MB/h (~18 GB/day) | **<= 160 MB/h** (-79 %) | **<= 80 MB/h** (-89 %, <= 1.9 GB/day) |
| of which writes into the database file (checkpoint back-fill) | ~278 MB/h | <= 40 MB/h | <= 20 MB/h |
| Commits | 1,925 /h | <= 450 /h | <= 150 /h |
| Checkpoints (fsync pairs) | 93 /h (~185 fsyncs) | <= 13 /h (<= 26 fsyncs) | <= 7 /h (<= 14 fsyncs) |
| Rows written per day (store statements) | 13.2 M | <= 4 M | <= 2 M |
| Store writer CPU (pprof share, rt-7d) | 1.2 % | <= 0.6 % | <= 0.5 % |
| SQLite page caches when warm | 48 MiB (16 + 4x8) | 48 MiB (unchanged on desktop) | <= 16 MiB (Pi 64-bit), <= 10 MiB (32-bit) |
| Private bytes after the 75-URL read sweep | 233 MiB | <= 233 MiB | <= 170 MiB |
| `/api/db/stats` r1 cold / warm | 16,672 / 454 ms | **<= 100 / <= 20 ms** | same |
| `/api/replay?hours=24` | 1,185 ms, truncated at 400 k points | **complete, <= 800 ms** | same |
| `/api/replay?hours=6` | 759 ms | <= 400 ms | same |
| `/api/stats/overview` r1 | 634 ms | **<= 80 ms** (and flat with 730 days of flights) | same |
| `/api/antenna/heat` first DB read | 73 ms (full scan of every antenna) | <= 15 ms | same |
| `/api/history/aircraft/{icao}` events | full scan of events | index search | same |
| Trail of a 13.7 k-point flight | 3.5-4.2 ms | <= 6 ms | same |
| Every other read URL | baseline | <= baseline + 10 % | same |

Pi projections are the i9 numbers x5-7 for CPU, as in the baseline; the
`low-wear` write targets are measured on the i9 with the low-wear profile
forced (`storage.profile = "low-wear"`), because bytes and commits do not
depend on the CPU.

## 2. Where the cost is, and the four levers

From the baseline: 96 % of the file is `positions` at 38.2 B/point; 92 % of
the WAL bytes are the tracker's 15-s flush, and inside it positions (35 %)
and heat (32 %) dominate; every 4 MiB of WAL triggers a checkpoint inside the
writer's commit (93/h, two fsyncs each, 278 MB/h of random 4 KB writes into
the database file); the slow reads are full walks of the big table.

The design pulls four levers, in this order of payoff:

| Lever | What | Items |
|---|---|---|
| L1 Write less often | group commit, decoupled and longer in-RAM accumulation of increments, checkpoint off the commit path and on a timer | §3.1-3.4 |
| L2 Write fewer pages per write | append-only trail tail, tile-ordered heat keys, no rewrite of unchanged indexed columns, sorted UPSERT batches | §3.5-3.7 |
| L3 Store fewer bytes | packed trail chunks, compact met observations, partial/removed indexes, WITHOUT ROWID where it pays, sealed day histograms | §3.8-3.12 |
| L4 Read fewer pages | O(1) counts, step-aware replay over chunks, hourly ICAO-set rollup for the overview, key orders that match the reads, periodic `PRAGMA optimize`, per-platform caches | §3.13-3.18 |

## 3. Design items

Each item: what changes, expected saving (est. unless measured), feature
risk, owner group (§7). "WAL share" refers to baseline §5.2.

### 3.1 Checkpoint off the commit path, on a timer (owner A)

- Writer connection: `wal_autocheckpoint` = the profile's **hard cap** in
  pages (16,384 = 64 MiB on station profiles, 131,072 = 512 MiB on `server`).
  It is only a safety net; normally it never fires.
- A **checkpointer goroutine** owned by the store, with its own dedicated
  connection (not `query_only`, `busy_timeout` 1 s), runs
  `PRAGMA wal_checkpoint(PASSIVE)` when the WAL exceeds the profile's soft
  size (`standard` 16 MiB, `low-wear` 32 MiB, `server` 64 MiB) or the
  profile's interval has passed since the last one (`standard` 5 min,
  `low-wear` 10 min, `server` 60 s), whichever comes first, and only while
  the writer is idle (it takes the writer's "between transactions" slot via a
  channel so no checkpoint ever runs inside a commit). Once an hour (the
  existing hourly job) it runs `TRUNCATE` with the existing 1 s busy timeout.
  If a PASSIVE checkpoint cannot back-fill everything for 3 consecutive runs
  (a long reader holds an old snapshot) and the WAL is above twice the soft
  size, it escalates to `RESTART` with a 1 s busy timeout.
- `journal_size_limit` = 32 MiB (`server`: 128 MiB), so a WAL that grew
  during a migration or a long export is truncated after the next reset.
- WAL appends stay sequential (good for flash); back-fill happens a few times
  per hour, and pages written repeatedly between two checkpoints (heat,
  today's histogram bins, the trail tail) are written to the database file
  once instead of once per 4 MiB of WAL.
- Expected (measured, exp1 with only the pragma): database-file writes
  254 -> 94 MB/h (-63 %), checkpoints 93 -> 12/h, all writes -20 %. With the
  timer and the smaller WAL volume of 3.2-3.6: database-file writes <= 40
  (`standard`) / <= 20 MB/h (`low-wear`), 6-13 checkpoints/h. Removes the
  writer stalls (checkpoint `FlushFileBuffers` was 80 % of the writer's CPU
  samples).
- Risk: none for features. The WAL is larger between checkpoints (<= 64 MiB
  hard cap), so a cold start replays a bigger WAL index (milliseconds). A
  power cut can lose commits made since the OS last wrote the WAL back
  (§4.3), as today.
- `stations.db` (aggregator): the registry opens the same file with its own
  pool (`internal/registry/registry.go`). It gets the same hard cap pragma
  and no checkpointer of its own; the store's checkpointer serves the file
  (owner D for the registry DSN).

### 3.2 Group commit interval per profile (owner A, values owner E)

- `Options.FlushInterval`: `standard` 10 s, `low-wear` 30 s, `server` 5 s
  (was 2 s everywhere). `FlushRows` / `MaxQueueRows` unchanged. Synchronous
  calls (`Write`, `Apply`, `Flush`, maintenance) still commit at once, after
  the queue (order is kept).
- Expected: commits 1,925 -> <= 450/h (`standard`), <= 150/h (`low-wear`).
  The small writers (guess samples, met, latency, events: 6-8 % of WAL frames,
  ~3 frames per commit) re-append their tail pages 5-15x less often:
  -25 to -30 MB/h WAL.
- Risk: rows of the small writers become visible in the database up to 10 s /
  30 s later; all of them are also shown live from memory (the in-memory
  views are unchanged). Loss window on power cut grows by the same amount
  (§4.3).

### 3.3 Tracker flush cadence decoupled from the minute (owner B)

Today `flushEveryMs = 15_000` and the minute roll-over flushes everything
(baseline §10.1: 60 s and 300 s gave identical numbers because of that).

- Split the tracker's persistence into four independent clocks, all taken
  from `tracker.Options` (filled by the engine from the profile, §4):

  | Stream | 1.0.0 | `standard` | `low-wear` |
  |---|---:|---:|---:|
  | Trail points -> trail tail (§3.5) | 15 s | 60 s | 60 s |
  | Day increments: coverage, signal_hist, elev_hist | 15 s + every minute roll | 60 s | 300 s |
  | Heat increments (all-time grid) | 15 s + every minute roll | 300 s | 900 s |
  | range_hour partial rows | 5 min | 15 min | 30 min |
  | flights / aircraft rows of active aircraft | 60 s | 60 s | 60 s |
  | stats_minute rows | every minute | every minute | every minute |

- The minute roll writes only the minute rows (and whatever stream is due).
  A day roll-over (UTC) forces the day increments of the old day out first,
  so increments are never attributed to the wrong day. `flush(final)` on
  shutdown and on `Reset` writes everything, as today.
- **Read overlay, so no page shows the lag.** The tracker exposes a snapshot
  of what it has not yet written:

  ```go
  // PendingIncs returns copies of the coverage / signal / elevation / heat
  // increments and the partial range_hour rows accumulated since their last
  // flush (nil slices when none). Safe for concurrent use; O(dirty bins).
  func (t *Tracker) PendingIncs() store.PendingIncs
  ```

  with `store.PendingIncs{Coverage []CoverageInc; Signal []SignalHistInc;
  Elev []ElevHistInc; Heat []HeatInc; Range []RangeHour}` (type defined by B
  in `internal/store/pending.go`). Analytics (owner C) merges it into every
  answer that covers "now" (antenna signal/coverage/heat/elevation, range,
  outline), so a page answers exactly what it would answer if the increments
  had been written. The engine (owner E) wires the function into
  `analytics.Options`.
- Expected (measured exp2 for 60 s): rows written per day 13.19 M -> 7.24 M
  (-45 %); with heat at 300 s and day increments at 300 s (`low-wear`) est.
  <= 2 M/day. WAL: heat 32 % share est. -70 % (`standard`) / -90 %
  (`low-wear`); coverage/signal/elev ~10 % share est. -50 % / -85 %.
- Risk: in-RAM increments are lost on a power cut (bounded, §4.3); the
  overlay must be exact (dbparity live phase checks it). RAM: the heat delta
  map holds at most the cells touched in 15 min (est. 10-20 k cells, <= 1 MB).

### 3.4 Flight / aircraft UPSERT without unchanged indexed columns (owner B)

- `UpsertFlights`: first write of a flight is an INSERT of all columns
  (`INSERT ... ON CONFLICT(id) DO NOTHING` semantic kept for re-sends);
  refreshes are `UPDATE flights SET callsign=?, squawk=?, "end"=?, msgs=?,
  ... WHERE id=?` that never assign `icao`, `start`, `dep`, `arr`. Baseline
  §5.2 proved SQLite rewrites an index entry whenever an indexed column is
  assigned: this removes the flights_icao rewrites (4.7 % of WAL frames) and
  the flights_dep/arr/unresolved rewrites. `"end"` is still updated
  (flights_end is needed by replay and prune), once per minute per active
  flight as today.
- `UpsertAircraft`: same pattern; `first_seen` is never re-assigned.
  `last_seen` (indexed) only when it moved by >= 60 s.
- Expected: WAL -5 to -6 % (-25 MB/h at baseline volume), aircraft/flight
  writer time -30 %.
- Risk: none (same final row content; dbparity rowcheck proves it).

### 3.5 Trail tail: append-only, then sealed into chunks (owner B)

New tables (registered migration `"trail"` v1, KindMain only):

```sql
-- recent trail points, append-only; no index (a few minutes of points)
CREATE TABLE trail_tail(
  flight_id INTEGER NOT NULL, t INTEGER NOT NULL,
  lat_e5 INTEGER NOT NULL, lon_e5 INTEGER NOT NULL,
  alt INTEGER, gs INTEGER, track INTEGER, rssi_x10 INTEGER,
  src INTEGER NOT NULL DEFAULT 0);

-- sealed, packed trail chunks (rowid table: rows are 0.6-2.6 KB, too big
-- for a WITHOUT ROWID B-tree, which wants rows < ~1/20 page)
CREATE TABLE trail_chunk(
  id INTEGER PRIMARY KEY,
  flight_id INTEGER NOT NULL,
  t0 INTEGER NOT NULL,         -- first point (s)
  t1 INTEGER NOT NULL,         -- last point (s)
  n  INTEGER NOT NULL,         -- points
  data BLOB NOT NULL);         -- codec byte + payload, §3.8
CREATE UNIQUE INDEX trail_chunk_flight ON trail_chunk(flight_id, t0);
```

- The tracker's trail stream appends to `trail_tail` (rowid order = arrival
  order). 250 active flights x 12 points per minute land on ~30 consecutive
  pages per 60-s flush, instead of ~250 scattered "open tail" pages of the
  big `positions` B-tree (positions = 35 % of the WAL frames today).
- **Seal** (store, runs from the checkpointer's slot every seal interval:
  `standard` 5 min, `low-wear` 10 min, and before every TRUNCATE): in one
  transaction, read `trail_tail` up to its current max rowid, group by flight,
  de-duplicate by `(flight_id, t)` with the last row winning (the
  `INSERT OR REPLACE` semantic of 1.0.0), append one chunk per flight, delete
  the sealed tail rows by rowid range. A point whose `t` falls inside an
  existing chunk's `[t0, t1]` re-encodes that chunk (merge, last wins), so
  late points behave exactly as before.
- **Compact** (hourly job): flights that ended more than 1 h ago and own
  more than one chunk of < 256 points are rewritten into 256-point chunks
  (deflate). Every point is therefore written at most three times in its life
  (tail, seal, compact) instead of being re-appended with its page every
  15 s.
- Readers (`Positions`, `Trail`, `ForEachPosition`, export, thinning,
  prune) read chunks by `(flight_id, t0)` / `t1` range, decode, merge the
  tail rows of that flight (tail scan is bounded by the seal interval:
  <= ~30 k rows), and, while the migration of §5 is running, the legacy
  `positions` rows (by primary key). Order and duplicate semantics are
  identical to 1.0.0.
- Expected WAL: positions share 165 MB/h -> est. <= 15 MB/h.
- Risk: correctness of merge/dedupe (covered by property tests and
  dbparity); `Trail` of an active flight scans the tail (target <= 6 ms on
  13.7 k points). Fallback if the tail scan misses its target: an index
  `trail_tail(flight_id)` (est. +3 MB/h WAL).

### 3.6 Tile-ordered heat key (owner C)

- Heat cells are row-major (`cell = lat_row << 16 | lon_col`, 0.05 deg); a
  page holds ~90 cells of one latitude row, so a track crossing rows touches a
  new page every 5.5 km. Rebuild `heat` as
  `PRIMARY KEY(ant, tile, cell) WITHOUT ROWID` with
  `tile = (lat_row >> 4) << 12 | (lon_col >> 4)` (16x16-cell tiles, ~88 km).
  The `cell` column and its values are unchanged (API and export identical;
  the export CSV lists the columns of 1.0.0 in their order via the logical
  table, §3.17).
- Reads by antenna become a range scan (today `SELECT ... WHERE ant=?` scans
  every antenna's cells: 3x the rows).
- Expected (est., to verify with `walpages`): distinct heat pages per flush
  -60 to -75 %; combined with 3.3: heat WAL 150 MB/h -> <= 25 (`standard`)
  / <= 10 MB/h (`low-wear`).
- Risk: none (same rows). `ON CONFLICT(cell, ant)` keeps working with any
  primary-key column order; the UPSERT gains the `tile` column.

### 3.7 Sorted UPSERT batches (owner B, in ops.go)

- Every `Merge*` / `AddPositions` / `UpsertAircraft` op sorts its rows by the
  table's primary key before executing (the increments come out of Go maps in
  random order). Sequential B-tree access keeps the same leaf in cache and
  cuts page reads of the writer (heat UPSERT 14 us vs 2 us for sequential
  tables in baseline §5.3).
- Expected: writer statement time -30 to -50 % for heat/coverage; no change
  in bytes.

### 3.8 Trail chunk codec (owner B)

- Payload: columnar, per column zig-zag delta varints over the chunk's
  points in `t` order: `t` (first value relative to `t0`), `lat_e5`, `lon_e5`,
  then `alt`, `gs`, `track`, `rssi_x10` each with a run-length-encoded NULL
  bitmap followed by deltas of the non-NULL values, then `src` run-length
  encoded. Byte 0 is the codec: `1` = varint, `2` = varint + raw deflate
  (`compress/flate`, `BestSpeed` at seal, level 6 at compaction).
- Measured (baseline §4.2, 256-point chunks over 13.9 M and 59.4 M real
  generator points): 10.15 B/point varint (3.2x), 6.70 B/point with deflate
  (4.9x). 30-day positions 2,325 MB -> est. <= 500 MB incl. index and page
  slack; 7 days 530 -> <= 120 MB. Simulated trails are smoother than real
  ones: expect 10-20 % more on a real station.
- Decode cost: est. 40-80 ns/point on the i9 (0.3-0.6 us on a Pi 4); a
  24 h replay decodes ~1.7 M points in < 150 ms on the i9.
- Risk: codec bugs = data loss. Mitigation: encoder output is decoded and
  compared before the tail rows are deleted (cheap; on by default, test
  hook to disable in benchmarks), fuzz test of the codec, migration keeps
  legacy rows until the chunk round-trips.

### 3.9 Compact meteorological observations (owner D)

- `met_obs` (82.4 B/row incl. `met_obs_t`; ~420 MB at its 30-day plateau)
  becomes `met_sample(t INTEGER, icao INTEGER, lat_e5 INTEGER, lon_e5
  INTEGER, alt INTEGER, wdir_x10 INTEGER, wkt_x10 INTEGER, oat_x100
  INTEGER, src INTEGER, PRIMARY KEY(t, icao)) WITHOUT ROWID`. No secondary
  index (the key is time-ordered; reads are by time). Scales: lat/lon 1e-5
  deg (as trails), wind direction 0.1 deg, wind 0.1 kt, OAT 0.01 C - all
  finer than the BDS 4,4 / 5,0 / 6,0 source resolution (1 kt, ~0.7 deg,
  0.25 C) and than the API's printed precision; NULL stays NULL.
- `met_profile` UPSERTs (~7,500/h, ~0.2 M/day) are accumulated per
  (hour, level) in the collector and written once per hour and at shutdown
  (the open hour is served from memory as today).
- Expected: 82 -> est. 28-32 B/row: plateau 420 -> <= 170 MB; met WAL -50 %.
- Risk: the scaled integers are rounded at the 5th decimal of the source
  REAL; dbparity compares met answers at printed precision (tolerance
  documented in tools/dbparity/README.md).

### 3.10 Flights indexes (owner B)

- Drop `flights_unresolved` (on a station without navigation data it is a
  full copy of `flights_end`). The departure/arrival backfill
  (`Store.UnresolvedFlights(ctx, endBefore, limit)`, unchanged signature)
  walks `flights_end` backwards from `endBefore` with `dep IS NULL` as a
  filter; the engine already passes the cursor. Without navdata the engine
  does not call it (unchanged).
- Replace `flights_dep` / `flights_arr` by partial indexes
  `... ON flights(dep, start) WHERE dep IS NOT NULL` (same for arr): SQLite
  proves `dep IS NOT NULL` from the report filter `dep = ?`, so the report
  plan is unchanged, and unresolved flights (all of them without navdata)
  cost nothing.
- Add `CREATE INDEX events_icao ON events(icao, id)` (per-aircraft event
  list was a full scan; events grow forever, ~290/day).
- Expected: flights 254 -> ~205 B/row (-49 B/row: 3 index entries of
  ~14-17 B); 2-year flights plateau 828 -> ~670 MB (station without
  navdata). Plus 3.4 removes their write cost.
- Risk: none; plans checked with `eqptrace` (A's harness).

### 3.11 range_hour (owner C; encoding line in ops.go owner B)

- Rebuild as `WITHOUT ROWID` (drops `sqlite_autoindex_range_hour_1`) and
  write the compact BLOB encoding that `stations.db` already uses
  (`compact.go`; decode already accepts both, `types.go:465`) on KindMain
  too. Partial rows every 15/30 min (3.3) instead of 5.
- Expected: 1,406 -> ~600 B/row; 37 -> ~14 MB per year; 48 % free space in
  its pages disappears with fewer REPLACEs.
- Risk: none (lossless; decode is shared).

### 3.12 Sealed day histograms (owner C, priority P2)

- Closed days of `coverage`, `signal_hist`, `elev_hist` (one row per bin:
  ~3,300 + 3,800 + 540 rows/day at 20-45 B) are packed into one row per
  (day, ant, table): `hist_day(day INTEGER, ant TEXT, tbl INTEGER, data BLOB,
  PRIMARY KEY(day, ant, tbl))`, delta-varint bins, lossless. Sealing runs in
  the hourly job for days older than 2 days (KindMain only; the aggregator
  keeps its month buckets in `tiered.go`).
- Readers (analytics) read sealed blobs plus live rows; the three tables'
  CSV exports come from the logical-table registry (§3.17) and are
  identical.
- Expected: ~95 -> ~25 MB per year.
- Risk: medium complexity in the analytics readers; strictly lossless.

### 3.13 O(1) table counts (owner B, framework A)

- `/api/db/stats` and the export list call `TableCounts`, which today runs
  `COUNT(*)` on every table: a full walk of 0.5-2.3 GB of positions (16.7 s
  cold on NVMe, 25-110 s on a cold SD card).
- New: exact counters for the big logical tables kept in `kv` in the same
  transactions that change them (`positions` = sum of chunk `n` + tail +
  legacy rows during migration; `met_obs` by D; `guess_samples` by D), a
  `COUNT(*)` for the small tables, and the logical name in the reply
  (`positions` keeps its name). A daily verify job recounts one table per day
  from the chunk index (`n` is read from `trail_chunk` rows, not decoded) and
  logs/fixes drift.
- Expected: 16.7 s cold -> <= 100 ms; 454 ms warm -> <= 20 ms.

### 3.14 Step-aware replay over chunks (owner B)

- `ForEachPositionStep(ctx, from, to, step, stale int64, fn)` decodes chunks
  and hands `fn` only the points the replay can use (for each aircraft the
  last point at or before each frame time within `stale`, plus the first
  point after a gap), so the 400,000-point budget counts points used, not
  points read. A 24 h replay at a 144 s step is complete instead of
  truncated (the one intended difference, tolerance `replay-untruncated`).
  For replays that were not truncated in 1.0.0 the reply is identical.
- Expected: rows touched -95 %, pages read 3-5x fewer (chunks), 24 h replay
  <= 800 ms and complete.

### 3.15 Overview without COUNT(DISTINCT) over all flights (owner C)

- New rollup `flights_hour(hour INTEGER PRIMARY KEY, flights INTEGER,
  icaos BLOB)` (sorted delta-varint ICAO set of the hour, ~1 KB), written for
  every closed hour by the hourly job, backfilled by a data migration.
  Per-day distinct aircraft for any whole-hour time-zone offset is the union
  of 24 hour sets; the current day and a half-hour offset fall back to the
  live query over at most one/two days of flights. `aircraft` totals
  (`COUNT(*), SUM(msgs), ...`) use a cached per-day aggregate invalidated by
  the writer counter.
- Expected: overview 634 ms -> <= 80 ms on 7 days and flat with 730 days of
  flights (the 1.0.0 query grows linearly: est. 30-40 s at 2 years on the
  i9). Cost ~8 MB/year.

### 3.16 Planner statistics, prepared statements (owner A)

- `PRAGMA optimize(0x10002)` with `analysis_limit=400` in the hourly job on
  every kind (today only in `Close` on stations; a Pi that is never cleanly
  stopped plans without statistics) and once after every completed data
  migration.
- Readers: the hot read statements (`Trail`, `AircraftByICAO`,
  `FlightByID`, chunk lookup, KV) are prepared once on the read pool
  (`database/sql` re-prepares per connection lazily). Est. -20-60 us per call
  on the Pi.

### 3.17 Logical tables for exports and the parity harness (owner A, providers B/C/D)

- `RegisterLogicalTable(LogicalTable{Name, Columns, TimeCol, Unit, Scan,
  Count})` in the store; `/api/db/export/<table>.csv` (api_data.go) and the
  export list use it for every table whose physical layout changed
  (`positions` - B, `heat` column order and `coverage`/`signal_hist`/
  `elev_hist` sealed days - C, `met_obs` - D) and fall back to the physical
  table otherwise. Column names, order, NULL handling and row order equal
  1.0.0.
- `tools/dbparity` rowcheck compares logical rows through the same registry.

### 3.18 Per-platform caches, mmap, read pool (owner A, values E)

- See the profile table §4.1. Reader connections close after an idle time on
  the Pi (`SetConnMaxIdleTime`), which returns their page cache (the
  baseline's +107 MiB after one sweep is mostly Go caches of analytics plus
  32 MiB of reader page cache).
- mmap stays modest on SD (64 MiB on Pi 64-bit, 0 on 32-bit): an SD read
  error through a mapping faults the process instead of returning an error.
  Desktop and server: 256 MiB / 1 GiB.
- `temp_store=MEMORY` stays (Vacuum keeps switching to FILE).
  `page_size` stays 4096 (matches ext4, NTFS clusters and SD pages; changing
  it needs a full VACUUM - not worth it).

### 3.19 guess_samples (owner D)

- The 10-minute pruner runs `SELECT stn, COUNT(*) FROM guess_samples GROUP BY
  stn` (a full read of up to ~75 MB every 10 min: ~10 GB/day of SD reads)
  before deleting. New: per-station counters (kv, same transaction as inserts
  and deletes) and a time-based cut (`t < cutoff`) first, the row cap only
  when the counter exceeds it (`OFFSET` lookup on the primary key, unchanged).
- The recorder's rows ride the low-tier queue, so they commit with the group
  commit (3.2) instead of their own 2-s commits.
- Expected: removes the 10-min full scan; guess WAL share (6.4 %) -80 %.
- Storage format unchanged (already WITHOUT ROWID, 37 B/row); recording
  rules unchanged (Locate Me keeps its inputs).

### 3.20 Other SD writers (owner E; weather owner D)

| Writer | Change | Expected |
|---|---|---|
| `weather_fc` (167 rows ~1.25 KB rewritten per poll, every 15 min: ~20 MB/day through the WAL) | the poller writes only rows whose values changed (`internal/engine/weather.go`, D) | -80-95 % of the weather writes |
| Log | unchanged level (info) and rotation; under systemd (`JOURNAL_STREAM` set) the stderr copy carries warn+ only by default (`process.journalLevel`, default `warn` under systemd, `info` elsewhere) so the SD card does not get every INFO line twice | ~20 KB/h -> ~10 KB/h; negligible either way |
| Tile cache | default cap 128 MB on `low-wear` (512 MB elsewhere); tiles are only written when a user pans | up to 384 MB less disk on a Pi |
| Terrain | unchanged (42 MB once) | - |
| navdata import | stage tables are created in airview.db as today; after the swap the dropped stage pages are returned with incremental vacuum in 2,000-page steps, and a TRUNCATE checkpoint follows the import | no lasting free space; bounded WAL |
| journald / mount options | documented in deploy/pi/README.md as optional operator steps (`Storage=volatile` drop-in, `noatime`); never changed by the installer without an explicit flag (`install.sh --low-wear-journal`) | operator choice |

### 3.21 Rejected or deferred (and why)

| Option | Decision |
|---|---|
| `synchronous=OFF` or no WAL on the Pi | rejected: corruption risk on power cut |
| `page_size` 8 or 16 KB | rejected: needs VACUUM of the whole file; WAL frames get bigger for the small hot writes |
| Rounding REAL columns of `flights` (lat/lon, rssi, distances) to scaled integers | rejected: API prints full precision in several places; saving ~25 B/row is not worth a parity risk |
| Removing `guess_samples` on a located station | rejected: Locate Me remains a feature on a located station |
| Shorter default retention / month buckets for station histograms | rejected: changes what the user sees; sealing (3.12) is lossless instead |
| Holding open trails only in RAM (no tail table) | rejected: loses up to the seal interval of trail on a power cut; the tail costs ~7 MB/h |
| A virtual table (`modernc.org/sqlite/vtab`) for `positions` | deferred: the logical-table registry covers exports and parity; a vtab can be added later for ad-hoc SQL |
| `/api/guess` 3.9 s | out of scope (CPU of the posterior, 6 ms of SQL); C may cache, not required |

## 4. Platform profiles

### 4.1 Profile values

`storage.profile`: `auto` (default) | `standard` | `low-wear` | `server`.
`auto` resolves to `server` in aggregator mode for `stations.db` (and the
aggregator's airview.db), else to `low-wear` when the data directory lives on
an MMC/SD block device (Linux: device major 179 in `/proc/self/mountinfo`)
or the host is Linux/arm or arm64 with <= 1 GiB RAM, else to `standard`.
Each value can be overridden individually by an advanced setting (E: config
keys under `storage.tuning.*`, 0 = profile value).

| Setting | `standard` (Windows; Linux x86; Pi on SSD) | `low-wear` (Pi 64-bit / 32-bit on SD) | `server` (aggregator, stations.db + registry) |
|---|---|---|---|
| store FlushInterval (group commit) | 10 s | 30 s | 5 s |
| trail tail flush | 60 s | 60 s | n/a (lanes, unchanged) |
| day increments flush | 60 s | 300 s | lanes `SlowFlushSec` (300 s, unchanged) |
| heat increments flush | 300 s | 900 s | lanes, unchanged |
| range_hour partial | 15 min | 30 min | unchanged |
| trail seal / compaction | 5 min / hourly | 10 min / hourly | n/a |
| checkpoint soft size / interval | 16 MiB / 5 min | 32 MiB / 10 min | 64 MiB / 60 s |
| wal_autocheckpoint (hard cap) | 16,384 pages | 16,384 pages | 131,072 pages |
| journal_size_limit | 32 MiB | 32 MiB | 128 MiB |
| synchronous | NORMAL | NORMAL | NORMAL |
| writer cache_size | 16 MiB | 8 MiB (32-bit: 4 MiB) | 64 MiB |
| reader cache_size x connections | 8 MiB x 4 | 4 MiB x 2 (>= 2 GiB RAM: x 3) | 16 MiB x 8 |
| reader idle close | 10 min | 2 min | never |
| mmap_size | 256 MiB (64-bit), 0 (32-bit) | 64 MiB (64-bit), 0 (32-bit) | 1 GiB |
| PRAGMA optimize | hourly | hourly | hourly (Maintain) |
| trail codec | deflate | deflate | deflate |
| tile cache default cap | 512 MB | 128 MB | 512 MB |
| pre-migration backup default (§5.3) | auto | auto | auto |

The resolved profile is logged at start, shown in `/api/db/stats`
(`profile` object) and in `airview config report`.

### 4.2 Expected writes per profile (est., rt-7d workload)

WAL MB/h by source, baseline -> `standard` / `low-wear`:

| Source (baseline WAL share) | Baseline | standard | low-wear |
|---|---:|---:|---:|
| positions (35 %) | 165 | 15 | 12 |
| heat (32 %) | 150 | 25 | 10 |
| coverage + signal + elev (~10 %) | 49 | 25 | 6 |
| guess_samples (6.4 %) | 30 | 6 | 3 |
| aircraft (6 %) | 28 | 20 | 20 |
| flights_icao + flights indexes (~5 %) | 24 | 1 | 1 |
| met (1.3 %) | 6 | 3 | 2 |
| rest (5 %) | 24 | 12 | 8 |
| **WAL total** | **469** | **~107** | **~62** |
| database-file back-fill | ~278 | <= 40 | <= 20 |
| **all writes** | **747** | **<= 160 (target)** | **<= 80 (target)** |

### 4.3 Power-cut loss window (documented in docs/CONFIG.md)

An application crash or kill loses only what is held in RAM; a clean stop
(SIGTERM, service stop, Windows shutdown) writes everything. A power cut
additionally loses commits the OS has not written back (WAL with
`synchronous=NORMAL`; Linux writes dirty pages back within ~30 s, Windows
within seconds). The file is never corrupted.

| Data | standard | low-wear |
|---|---|---|
| trail points | <= 60 s + OS write-back | <= 60 s + OS write-back |
| coverage / signal / elevation increments | <= 60 s + write-back | <= 5 min + write-back |
| heat increments | <= 5 min + write-back | <= 15 min + write-back |
| range_hour partial hour (max / percentiles of the open hour) | <= 15 min + write-back | <= 30 min + write-back |
| small rows (guess, met, events, latency, NTP) | <= 10 s + write-back | <= 30 s + write-back |
| flights / aircraft rows | <= 60 s + write-back (as 1.0.0) | same |
| 1.0.0 for comparison | 15 s increments/trails, 2 s small rows, 5 min range_hour | |

## 5. Migration

### 5.1 Framework (owner A, `internal/store/datamig.go`)

DDL migrations keep the existing `RegisterMigration` path. New, for data:

```go
// DataMigration converts existing rows in the background, in small
// transactions, after the DDL migrations ran.
type DataMigration struct {
    Name    string    // owner name, e.g. "trail"; unique with Version
    Version int
    Kinds   Kind
    After   []string  // names that must be done first (e.g. "trail" after its DDL)
    // Estimate returns the rows left and the extra disk bytes the rest of
    // the migration needs at its peak (used by the disk-space check).
    Estimate func(ctx context.Context, q Querier) (rows, peakBytes int64, err error)
    // Step converts at most budget rows inside tx and returns the new
    // cursor; done=true marks the migration complete. A step must be
    // idempotent for a given cursor (it may be re-run after a crash).
    Step func(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) (next []byte, done bool, err error)
    // Finish runs once after done (e.g. DROP the emptied legacy table).
    Finish func(ctx context.Context, tx *sql.Tx) error
}
func registerDataMigration(m DataMigration) // from init() of the owner file
func (s *Store) MigrationStatus() []MigrationState // name, version, state, rows left, started, eta, last error
```

- Progress lives in `schema_data_migrations(name, version, cursor BLOB,
  done INTEGER, started, updated, rows_done, last_error)`, written in the same
  transaction as the step, so a kill at any point resumes exactly after the
  last committed step (never twice, never skipped).
- Scheduling: steps run in the writer goroutine between flushes (never
  inside one), each bounded by `budget` rows and ~200 ms (`low-wear`:
  100 ms) of work, with a pause of the same length between steps; paused
  while the queue holds more than `FlushRows`, while a checkpoint runs, or
  while free disk is below the threshold. Throughput est. on the i9: 7-day DB
  ~1-2 min, 30-day ~8-10 min; Pi 4 est. 30-90 min (the station works
  throughout).
- **Disk-space check** before start and before every 100th step: free bytes
  on the data volume >= `peakBytes` + max(256 MiB, 10 % of the file). If
  not, the migration pauses (status `waiting_for_disk`, warning in the log
  and in `/api/db/stats`, re-check every 10 min). Platform free-space call:
  `golang.org/x/sys/windows.GetDiskFreeSpaceEx` / `unix.Statfs` (x/sys is
  already a dependency).
- After all data migrations: `PRAGMA optimize`, then return free pages with
  `incremental_vacuum` in 2,000-page steps spread over the next hours (one
  step per checkpoint), so the file shrinks without a write burst. Files
  created before `auto_vacuum=INCREMENTAL` was the default (auto_vacuum=0)
  keep their free pages for reuse and get a one-line hint to run
  `airview db vacuum` (existing Vacuum: needs free space = file size).

### 5.2 The data migrations

| Name / ver | Owner | What | Per step | Notes |
|---|---|---|---|---|
| `trail` 1 | B | legacy `positions` -> `trail_chunk` (256-point deflate chunks), oldest flight first | <= 64 flights or 50 k points: encode, verify round-trip, insert chunks, delete those legacy rows, add to the kv counter | readers union legacy + chunks + tail until done; Finish keeps an empty `positions` table (schema stability for old tools) |
| `flights` 2 (DDL only) | B | drop flights_unresolved; partial flights_dep/arr; events_icao | one transaction (flights ~0.14 M rows/month: < 2 s on a Pi) | |
| `heat` 2 | C | rebuild heat with `(ant, tile, cell)` key | one antenna per step | 95-100 k rows |
| `range` 2 | C | range_hour WITHOUT ROWID, compact BLOBs | 1,000 rows per step | |
| `flights_hour` 1 | C | backfill hour ICAO sets from flights | 7 days per step | |
| `hist_day` 1 (P2) | C | seal closed days | 1 day per step | |
| `met` 2 | D | `met_obs` -> `met_sample` | 20 k rows per step (time order) | old table dropped in Finish |
| `guess` 2 | D | per-station counters from one `COUNT(*)` | one step | |

### 5.3 Pre-migration backup

`storage.backupBeforeMigrate`: `auto` (default) | `always` | `never`.

- `auto`: before the first data step, if free space >= file size x 1.3 +
  512 MiB, the store writes `data/backup/airview-pre-1.1-<UTC date>.db` with
  `VACUUM INTO` on a read connection (consistent snapshot, writer not
  blocked, est. 478 MB / 4.3 s for the 7-day file on the i9). Otherwise it
  logs that no backup was made and migrates anyway - each step is
  non-destructive, so a crash never loses data.
- `always`: no backup -> no migration (the station keeps running on the
  legacy layout, which every reader still supports; status
  `waiting_for_backup`).
- `never`: no backup.
- The backup is kept for 14 days after the migration completed, then
  deleted by the store (only files it created, matching its own name
  pattern). `airview db migrate --status | --backup | --now` (E) exposes the
  same from the command line for headless installs.
- Downgrade to 1.0.0 is only supported by restoring that backup (1.0.0 would
  not see sealed trails); the release notes say so.

### 5.4 Migration acceptance (A, with B/C/D providers)

- `go run ./tools/dbparity` extended phase "migrate": old build records the
  3-session database, the new build migrates it with the station running,
  and is killed (`taskkill /F` / SIGKILL) at 5 random points; after the last
  restart the migration completes and logical rows of every table equal the
  old build's rows; API answers equal (tolerances §9).
- gen7 copy: migration completes in <= 3 min on the i9 with live demo
  traffic, `Stats.Dropped == 0`, no API error, WAL <= 64 MiB, peak file size
  <= original + 15 %, final file after the incremental-vacuum tail <= 150 MB.
- gen31 copy: completes in <= 12 min on the i9, final file <= 0.65 GB
  (trails + 30 days of everything else; met not in the generator).
- A disk-full simulation (free-space function stubbed) pauses and resumes.

## 6. Schema summary (KindMain)

| Table | 1.0.0 | 1.1 |
|---|---|---|
| positions | 13.9 M rows (7 d), 38.2 B/pt | empty after migration (kept); trail_tail (minutes) + trail_chunk (6.7-10 B/pt) |
| flights | 6 indexes | 3 full + 2 partial indexes; UPDATE refreshes |
| events | events_t | + events_icao |
| heat | PK(cell, ant) | PK(ant, tile, cell) |
| range_hour | rowid + autoindex, raw BLOBs | WITHOUT ROWID, compact BLOBs |
| coverage / signal_hist / elev_hist | rows forever | rows for the last 2 days + hist_day sealed blobs |
| met_obs | rowid + met_obs_t, REAL | met_sample WITHOUT ROWID, scaled INTEGER |
| new | | flights_hour, schema_data_migrations |

`stations.db` keeps its schema; it gains the checkpoint policy, the `server`
profile, the sorted batches and the guess counters.

## 7. Implementation groups

Five groups with strictly disjoint file ownership. A file not listed is not
edited by anyone. New files are listed with their exact names; a group may
add `*_test.go` files only with its own prefix as listed. Ports: each group
runs its builds on its own port (`-set http.port=<port>`) and keeps binaries
and data in
`C:\Users\chris\AppData\Local\Temp\claude\7cf8c843ccba07dc\559b9eaf-9013-47b7-bf37-8e45b676bc65\scratchpad\dbeff\<group>\`.
Reference databases (read-only, copy before use): `dbeff\gen7\data\airview.db`,
`dbeff\gen7busy`, `dbeff\gen31`; measurement tools: `dbeff\measure\` (dbgen,
walwatch, walpages, dbinspect, trailpack, eqptrace, sample.ps1, rates.ps1,
sweep.ps1; sources in `dbeff\measure\src`, rebuild against the new tree).

### 7.0 Contracts (land first, as stubs with 1.0.0 behaviour)

| Provider | Contract | Consumer |
|---|---|---|
| A | `store.Profile` struct + `store.ResolveProfile(name string, kind Kind, dataDir string) Profile`; `Options.Profile` | E (engine/config), B (tracker cadence values read from the Profile passed by E) |
| A | `registerDataMigration`, `registerPruner(name, fn func(ctx, *Store, model.StorageConfig, now int64, *PruneResult) error)`, `registerHourly(name, fn func(ctx, *Store) error)`, `RegisterLogicalTable`, `(*Store).HourlyMaintenance(ctx) (HourlyReport, error)`, `(*Store).MigrationStatus()`; `PruneResult.Extra map[string]int64` | B, C, D (in their own store files), E (sched.go calls HourlyMaintenance instead of RollupHours + Checkpoint) |
| A | `tryOpenHandles` calls `maxStoredFlightID(ctx, q)` | B defines it in `trail_store.go` |
| B | `store.PendingIncs` type (`internal/store/pending.go`); `(*tracker.Tracker).PendingIncs()`; `tracker.Options{TrailFlush, IncFlush, HeatFlush, RangePartial time.Duration}` | C (analytics merge), E (wiring) |
| B | `(*Store).ForEachTrailRow(ctx, from, to, limit, fn)`; `(*Store).ForEachPositionStep(...)` | A (export, dbparity), B (replay) |
| C | `analytics.Options.Pending func() store.PendingIncs` | E (wiring) |
| D | `(*Store).AddMetSamples`, `(*Store).MetSamplesSince` (in `met_store.go`) | D (met collector) |

Every group: gofmt/vet clean, `go test ./...` passes (budget tests
`clock.TestEpochCostGate` / `tracker.TestNetworkBudgets` may be re-run
alone), and `go run ./tools/dbparity -new <its build>` exits 0 before it
reports done. Never stop or touch `dist\`, `dist\data` or the user's
instance; stop only own PIDs.

### Group A - store core, migration framework, parity harness (port 18921)

Owns:
`internal/store/store.go`, `internal/store/writer.go`,
`internal/store/schema.go`, `internal/store/kind.go`,
`internal/store/maint.go`, `internal/store/tiered.go`,
`internal/store/doc.go`, `internal/store/WIRING.md`,
`internal/store/antkey.go`, `internal/store/export.go`,
`internal/store/store_test.go`, `internal/store/growth_test.go`,
`internal/store/tierconfig_test.go`, `internal/store/stations_test.go`,
`internal/store/stations_bench_test.go`;
new: `internal/store/profile.go`, `internal/store/profile_test.go`,
`internal/store/checkpointer.go`, `internal/store/checkpointer_test.go`,
`internal/store/datamig.go`, `internal/store/datamig_test.go`,
`internal/store/hooks.go`, `internal/store/logical.go`,
`internal/store/logical_test.go`, `internal/store/premigrate.go`,
`internal/store/premigrate_test.go`, `internal/store/diskfree_windows.go`,
`internal/store/diskfree_unix.go`; `internal/server/api_data.go`;
`tools/dbparity/**`.

Tasks:
1. Contracts of 7.0 (stubs first).
2. Profiles (3.18, 4.1): `ResolveProfile` incl. MMC detection and RAM;
   DSNs built from the profile; reader idle close. Accept: unit tests for
   each platform branch; `/api/db/stats` shows the resolved profile.
3. Checkpointer (3.1): accept on rt-7d workload with only this change:
   checkpoints <= 13/h (`standard`), <= 7/h (`low-wear`); database-file
   writes <= 94 MB/h (exp1 level) before the other groups' gains; no
   checkpoint inside `Commit` in a CPU profile; WAL never > 64 MiB.
4. Group commit values (3.2): accept, on the full tree, commits <= 450/h
   (`standard`) and <= 150/h (`low-wear`) on the rt-7d workload.
5. Data-migration framework (5.1), backup (5.3), disk-space check; Prune
   via pruner registry (legacy positions/met_obs pruning stays in maint.go
   and no-ops on empty tables); `HourlyMaintenance` = RollupHours + hourly
   jobs + optimize + TRUNCATE. Accept: datamig tests with a fake migration
   killed between every step (in-process, by closing the store mid-step and
   by a child process killed with os.Process.Kill); disk-full pause/resume
   test.
6. Logical tables (3.17) in `api_data.go` export + export list +
   `/api/db/stats` migration status. Accept: CSV of every table byte-equal to
   1.0.0 on the parity data set.
7. dbparity: new "migrate" phase with 5 kills (5.4), logical rowcheck,
   tolerances `replay-untruncated`, `met-precision`; README updated. Accept:
   `-new old` self-test and the full tree both exit 0.
8. Integration measurement of §1 targets on rt-7d (both profiles) and gen7 /
   gen31 sizes, results handed to E for `docs/perf/DB-RESULTS.md`.

### Group B - trails, flights, aircraft, tracker persistence, readers (port 18922)

Owns:
`internal/store/ops.go`, `internal/store/read.go`,
`internal/store/types.go`, `internal/store/json.go`,
`internal/store/airports.go`, `internal/store/airports_test.go`,
`internal/store/downsample.go`, `internal/store/downsample_test.go`;
new: `internal/store/pending.go`, `internal/store/trail_codec.go`,
`internal/store/trail_codec_test.go`, `internal/store/trail_store.go`,
`internal/store/trail_store_test.go`, `internal/store/trail_migrate.go`,
`internal/store/trail_migrate_test.go`, `internal/store/flights_migrate.go`,
`internal/store/counts.go`, `internal/store/counts_test.go`,
`internal/store/ops_sort_test.go`; `internal/tracker/**` (all files);
`internal/server/api_replay.go`.

Tasks:
1. Contracts of 7.0 (`PendingIncs`, tracker cadence options,
   `ForEachTrailRow`, `ForEachPositionStep`, `maxStoredFlightID`).
2. Trail codec (3.8) with fuzz and round-trip tests. Accept on all 13.9 M
   gen7 points: <= 10.3 B/pt varint, <= 7.0 B/pt deflate, encode+decode
   <= 150 ns/pt on the i9.
3. Tail + seal + compact + readers + pruner + thinning (3.5); migration
   `trail` 1 (5.2). Accept: gen7 positions bytes 530 MB -> <= 120 MB after
   migration + compaction; trail of a 13.7 k-point flight <= 6 ms; rowcheck
   equal; positions WAL share <= 15 MB/h on rt-7d.
4. Tracker cadence split + overlay (3.3): accept rows written per day on
   gen1trace <= 7.3 M (`standard`) and <= 2 M (`low-wear`); live-phase
   dbparity equal (overlay exact).
5. Flights/aircraft UPDATE path (3.4) and flights index migration (3.10):
   accept zero flights_icao/dep/arr/unresolved pages in a `walpages` run of
   10 min; `eqptrace` plans of report/airport filters unchanged; flights
   B/row <= 210 on gen7.
6. Sorted batches (3.7) incl. range_hour compact encoding on KindMain
   (one-line change in `AddRangeHours`, requested by C): accept writer
   statement time (pprof `sqlite3VdbeExec` under flush) -30 % on gen1trace.
7. O(1) counts (3.13): `/api/db/stats` <= 100 ms cold (after a reboot-like
   cache flush: run on a freshly copied file), <= 20 ms warm.
8. Step-aware replay (3.14): 24 h replay complete, <= 800 ms; 6 h <= 400 ms;
   non-truncated replays byte-equal to 1.0.0.

### Group C - statistics, heat, coverage, rollups, analytics (port 18923)

Owns:
`internal/store/bins.go`, `internal/store/compact.go`,
`internal/store/series.go`, `internal/store/secs_test.go`;
new: `internal/store/stats_migrate.go`, `internal/store/stats_migrate_test.go`,
`internal/store/flights_hour.go`, `internal/store/flights_hour_test.go`,
`internal/store/hist_seal.go`, `internal/store/hist_seal_test.go`,
`internal/store/stats_read.go`, `internal/store/stats_read_test.go`;
`internal/analytics/**` (all files); `internal/server/api_stats.go`,
`internal/server/api_location.go`.

Tasks:
1. `analytics.Options.Pending` and the overlay merge into every analytics
   answer that includes the open day / open hour (signal, coverage, outline,
   elevation, heat, range). Accept: dbparity live phase equal with
   `low-wear` cadence forced.
2. Heat rebuild with tile key (3.6), migration `heat` 2; antenna heat reads
   by range. Accept: heat distinct WAL pages per 60 s flush -60 % (walpages,
   rt-7d), `/api/antenna/heat` first DB read <= 15 ms.
3. range_hour rebuild (3.11), migration `range` 2. Accept: <= 650 B/row on
   gen31, parity equal.
4. flights_hour rollup + overview (3.15), migration `flights_hour` 1.
   Accept: overview r1 <= 80 ms on gen7 and on a synthetic 2-year flights
   table (C builds it with dbgen in its scratch dir), answers equal for
   offsets 0, +1 h, +5.5 h, -8 h.
5. (P2) Sealed day histograms (3.12), migration `hist_day` 1, logical
   tables for coverage/signal_hist/elev_hist. Accept: <= 30 % of the bytes of
   the three tables on gen31, parity equal.

### Group D - tables of other packages (port 18924)

Owns:
`internal/store/guess_samples.go`, `internal/store/ops_net.go`,
`internal/store/read_net.go`; new: `internal/store/met_store.go`,
`internal/store/met_store_test.go`, `internal/store/met_migrate.go`,
`internal/store/guess_counts_test.go`; `internal/met/**`,
`internal/latency/**`, `internal/rfstat/**`, `internal/weather/**`,
`internal/predict/**`, `internal/navdata/**`, `internal/alerts/**`,
`internal/registry/**`, `internal/mlat/**`;
`internal/engine/weather.go`; `internal/server/mlat_history_store.go`,
`internal/server/api_weather.go`.

Tasks:
1. met_sample + migration `met` 2 + collector reads/writes via
   `met_store.go`; met_profile hourly accumulation (3.9). Accept: <= 32 B/row
   incl. key on a 2-hour real-time demo run; met answers equal at printed
   precision; met WAL <= 3 MB/h.
2. guess_samples counters, low-tier queue, no full scan in the pruner
   (3.19). Accept: no `GROUP BY stn` scan in an SQL trace of 30 min; Locate Me
   answers equal.
3. weather_fc diff writes (3.20). Accept: weather rows written per poll
   <= 20 % of 1.0.0 with an unchanged forecast; weather APIs equal.
4. registry DSN for stations.db with the `server` profile hard cap and
   caches; navdata import followed by stepwise incremental vacuum + TRUNCATE;
   review latency/rfstat/alerts/predict/mlat persistence for per-row
   commits (move them to the batch queue where they commit alone). Accept:
   registry and mlat tests pass; aggregator scale tests
   (`internal/registry/scale_test.go`, `internal/store/stations_bench_test.go`
   run by A) not slower than 1.0.0 +5 %.

### Group E - platform profiles, engine, config, commands, deploy, docs (port 18925)

Owns:
`internal/engine/**` except `internal/engine/weather.go`;
`internal/config/**`; `internal/model/**`; `cmd/**`;
`deploy/pi/**`, `deploy/feeder/pi/**`, `deploy/feeder/pi-image/**`,
`deploy/server/**`; `internal/server/tilecache.go`;
`docs/ARCHITECTURE.md`, `docs/CONFIG.md`, `docs/API.md`,
`docs/aggregator/OPERATOR-GUIDE.md`, `docs/RELEASE-NOTES.md`,
new `docs/perf/DB-RESULTS.md`.

Tasks:
1. Config: `storage.profile`, `storage.backupBeforeMigrate`,
   `storage.tuning.*` (flushSec, incFlushSec, heatFlushSec, checkpointSec,
   walSoftMB, cacheMB, readConns, mmapMB; 0 = profile), validation,
   generated field docs, example jsonc. Accept: config tests, `airview
   config check` on old configs unchanged.
2. Engine: resolve the profile, pass `Options.Profile` to both stores and
   the cadence to the tracker; `sched.go` calls `HourlyMaintenance`; wire
   `tracker.PendingIncs` into analytics; graceful shutdown flushes all
   streams before `Close` within the service stop timeout. Accept: engine
   tests; SIGTERM during `low-wear` loses nothing (test: stop, restart,
   compare counts).
3. `airview db migrate --status|--backup|--now` and `airview db profile`.
4. Logging under systemd (3.20), tile-cache cap per profile, deploy/pi
   README low-wear notes, `install.sh --low-wear-journal` (opt-in), server
   install with `server` profile defaults. Accept: deploy script tests pass.
5. Docs: CONFIG (profiles, loss windows §4.3), ARCHITECTURE (storage
   section), OPERATOR-GUIDE (stations.db profile), API (`/api/db/stats`
   additions), RELEASE-NOTES (migration, downgrade note), and
   `docs/perf/DB-RESULTS.md` with the before/after table of §1 measured by A
   and the groups.

## 8. Order of work and integration

1. All groups: contracts (7.0) as stubs with 1.0.0 behaviour, tree builds.
2. In parallel: A 2-5, B 2-5, C 1-3, D 1-3, E 1-2.
3. Then: A 6-7, B 6-8, C 4-5, D 4, E 3-5.
4. A runs the integrated measurement (§1 targets, both profiles) and
   dbparity; E writes DB-RESULTS.md. Any missed target goes back to its
   owner with the measured number.

## 9. Intended differences (dbparity tolerances)

| Tolerance | Where | Why |
|---|---|---|
| `replay-untruncated` | `/api/replay` when 1.0.0 replied `truncated: true` | the budget counts points used (3.14) |
| `met-precision` | met answers | scaled integers, compared at printed precision (3.9) |
| `db-stats-shape` | `/api/db/stats` | new `profile` and `migration` objects; table counts equal |
| `visible-after` | live phase, database-only reads of small writers | group commit 10/30 s (3.2); pages that merge the overlay must be equal |
