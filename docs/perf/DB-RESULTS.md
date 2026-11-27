# Station database efficiency: results (AirView 1.1 storage)

Status: measured 2026-11-15 on the full tree (all five implementation groups
merged), i9 / NVMe host, Windows 11, `CGO_ENABLED=0`. Design and targets:
`docs/perf/DB-EFFICIENCY-DESIGN.md` (§1); "before" numbers: 1.0.0
(`docs/perf/DB-BASELINE.md`, or re-measured side by side with the 1.0.0
binary where marked). Every number below was measured unless it says
*projection* or *est.*

## 1. Summary against the §1 targets

| Metric | 1.0.0 | Target `standard` | 1.1 `standard` | Target `low-wear` | 1.1 `low-wear` | |
|---|---:|---:|---:|---:|---:|---|
| 7-day database, 250 peak (gen7) | 553 MB | <= 150 MB | **101 MB** | same | 101 MB | met |
| 7-day database, 450 peak (gen7busy) | 941 MB | <= 240 MB | **172 MB** | same | 172 MB | met |
| 30-day plateau (gen31; + met/guess projection) | 2.41 GB (2.9 GB) | <= 0.85 GB | **0.42 GB** (0.66 GB) | same | same | met |
| 1 year / 2 years (*projection*, §2.2) | 3.6 / 4.3 GB | <= 1.45 / 1.9 GB | **1.24 / 1.80 GB** | same | same | met |
| All bytes written by the process | 747 MB/h | <= 160 MB/h | 345 MB/h (-54 %) | <= 80 MB/h | 266 MB/h (-64 %) | **not met**, §4 |
| of which database-file back-fill | ~278 MB/h | <= 40 MB/h | <= 268 MB/h (upper bound) | <= 20 MB/h | <= 218 MB/h (upper bound) | not met, §4 |
| Commits | 1,925 /h | <= 450 /h | **438 /h** | <= 150 /h | 216 /h | standard met; low-wear not |
| Checkpoints (fsync pairs) | 93 /h | <= 13 /h | **12 /h** | <= 7 /h | **6 /h** | met |
| Store statement rows per day (gen1trace) | 13.2 M | <= 4 M | 4.9 M (group B) | <= 2 M | not measured separately | open |
| SQLite page caches, configured maximum | 48 MiB | 48 MiB | 48 MiB (0 mmap on Windows) | <= 16 / 10 MiB | 16 MiB (20 with >= 2 GiB RAM); 32-bit 12 MiB | see §4 |
| Private bytes after the read sweep | 233 MiB (325 re-measured, static) | <= 233 MiB | 445 MiB (static) | <= 170 MiB | 184 MiB in the rt-7d run without a sweep (not swept) | not met, §3 |
| `/api/db/stats` r1 / warm | 16,672 / 454 ms (811 / 2 re-measured) | <= 100 / 20 ms | **31 / 1.8 ms** | same | same | met |
| `/api/replay?hours=24` | 1,185 ms, truncated (639 ms re-measured) | complete, <= 800 ms | **421 ms, complete** (3.7 MB) | same | same | met |
| `/api/replay?hours=6` | 759 ms | <= 400 ms | 7 ms (static data, see §3) | same | same | met (static) |
| `/api/stats/overview` r1 | 634 ms (262 re-measured) | <= 80 ms, flat | 255 ms; daily part 3-7 ms, flat over 2 years | same | same | partly: flat yes, 80 ms no |
| `/api/antenna/heat` first DB read | 73 ms | <= 15 ms | 27-32 ms warm (group C) | same | same | not met |
| `/api/history/aircraft/{icao}` events | full scan | index | `events_icao` (2.7 -> 1.9 ms) | same | same | met |
| Trail of one flight | 3.5-4.2 ms (13.7 k points) | <= 6 ms | 2.7-4.1 ms (1.5 k points; was 4.3-5.6) | same | same | met |
| Every other read URL | baseline | <= +10 % | sum of 75 r1: 19.2 s -> 10.6 s; no URL > +10 % and > 2 ms except §3 | same | same | met |
| Log written (`airview.log`) | ~20 KB/h | <= 25 KB/h | **4.8 KB/h** | same; journal warn+ only under systemd | 7.7 KB/h | met |

"Re-measured" = the 1.0.0 binary (`dist\airview.exe`, copied) on a fresh
copy of the same database in the same sweep as the 1.1 build (§3); the
design's baseline was taken with live demo traffic and a cold OS cache.

## 2. Disk size

### 2.1 Reference databases, migrated

Copies of the baseline databases, migrated with `airview db migrate --now`
(profile `standard`, no backup), then a compact copy with `airview db
snapshot` (= the file after the background incremental-vacuum tail).

| Database | 1.0.0 file | 1.1 file | Migration (`--now`, i9) | trail_chunk | B / trail point |
|---|---:|---:|---:|---:|---:|
| gen7 (7 days, 13.9 M points) | 553.3 MB | **101.3 MB** (-82 %) | 33 s | 82.5 MB | 5.9 |
| gen7busy (7 days, 450 peak, 23.9 M points) | 941.9 MB | **172.4 MB** (-82 %) | 61 s | 146.3 MB | 6.1 |
| gen31 (31 days, 59.4 M points) | 2,411.7 MB | **419.1 MB** (-83 %) | 175 s | 354.6 MB | 6.0 |

Largest tables after the migration (gen31): trail_chunk 354.6 MB, flights
27.0 MB (incl. indexes, 138 k rows = 195 B/row), stats_minute 14.2 MB,
aircraft 10.1 MB, heat 4.6 MB, hist_day 2.5 MB, receiver_status 2.1 MB,
range_hour 1.0 MB, events 1.0 MB, flights_hour 0.3 MB.

The migrated file keeps its old size until the free pages are returned:
the running server does it in the background (incremental vacuum after the
migration, measured on the rt-7d copy: 555 -> 424 MB in the first 8
minutes, about 65 MB every ~3 minutes); a file created without incremental
auto-vacuum keeps the pages for new rows and logs a hint to run the new
`airview db vacuum` (AirView stopped).

### 2.2 Projection with the default retention (method of baseline §4.3)

| | 30 days | 1 year | 2 years | basis |
|---|---:|---:|---:|---|
| trails (30 d) | 355 MB | 355 MB | 355 MB | gen31 measured (1.0.0: 2,272) |
| met observations (30 d) | ~164 MB | ~164 MB | ~164 MB | *est.* 32 B/row (group D target) x the baseline row count (1.0.0: 422) |
| flights (730 d) | 27 MB | ~328 MB | ~656 MB | gen31 measured, x 0.79 of 1.0.0 (1.0.0: 34 / 414 / 828) |
| guess_samples (cap) | 75 MB | 75 MB | 75 MB | unchanged |
| stats_minute (90 d) | 15 MB | 46 MB | 46 MB | unchanged |
| coverage + signal + elevation | 3 MB | ~36 MB | ~72 MB | sealed days 38 % of 1.0.0 (group C, gen31) |
| range_hour | 1 MB | ~13 MB | ~25 MB | 479 vs 1,406 B/row (group C) |
| aircraft (simulator upper bound) | 13 MB | 159 MB | 317 MB | unchanged |
| the rest (+ flights_hour ~4.7 MB/year) | ~20 MB | ~60 MB | ~85 MB | |
| **total** | **~0.67 GB** | **~1.24 GB** | **~1.80 GB** | 1.0.0: 2.9 / 3.6 / 4.3 GB |

Real trails are less smooth than the generator's: expect 10-20 % more
trail bytes (design §3.8), still inside every target.

## 3. Read latency (static sweep)

All 75 read URLs of `measure\sweep.ps1`, 3 requests each, on a fresh copy
of gen7 without live traffic (`-set feeds=[]`, `GOMAXPROCS=4`,
`GOMEMLIMIT=600MiB`): the 1.0.0 binary on the 1.0.0 file, then the 1.1
binary on the migrated file, minutes apart. r1 = first request.

| Endpoint | 1.0.0 r1 / r3 (ms) | 1.1 r1 / r3 (ms) | Note |
|---|---:|---:|---|
| `/api/db/stats` | 811 / 2 | **31 / 1.8** | O(1) counters (group B) |
| `/api/guess` | 16,266 / 1.1 | 8,687 / 1.2 | CPU of the posterior, out of scope |
| `/api/replay?hours=24` | 639 / 1.5 (truncated, 866 KB) | **421 / 3.3 (complete, 3.7 MB)** | step-aware replay (tolerance `replay-untruncated`) |
| `/api/replay?hours=6` | 17.8 / 1.5 | 7.0 / 1.2 | the generated data ends at its generation time, so the 6 h window is mostly empty here |
| `/api/stats/overview` | 262 / 1.4 | 255 / 1.2 | `Top` dominates (70-80 ms, every generated flight is a new airframe; group C) |
| `/api/stats/top?limit=10` | 156 / 1.1 | 159 / 1.3 | same |
| `/api/antenna/signal` | 120 / 1.0 | 129 / 1.4 | +7 %, CPU of the fit |
| `/api/antenna/heat?maxCells=2000` | 69 / 1.2 | 80 / 1.5 | +16 %: per-row scan of the tiled key (group C, §3.6 note) |
| `/api/antenna/heat` | 11.4 / 1.2 | 8.4 / 1.4 | |
| `/api/history/flights/19717` (1,468 points) | 5.6 / 5.7 | 3.2 / 2.8 | trail from chunks |
| `/api/export/flight/19717.geojson` | 4.8 / 4.6 | 2.7 / 2.5 | |
| `/api/history/aircraft/4074FC` | 2.7 / 2.0 | 1.9 / 1.7 | `events_icao` |
| `/api/reports.csv?hours=168` | 286 / 295 | 289 / 278 | |
| `/api/export/aircraft.csv` | 170 / 144 | 173 / 166 | |
| sum of all r1 | 19,204 | **10,610** | |

Other URLs moved by less than 2 ms. Response sizes are equal except where
the time window moved between the two runs (`rates?hours=6`, `replay?hours=6`,
`reports.csv`) and the untruncated 24 h replay; byte-for-byte parity is
checked by `tools/dbparity` (group A).

Memory: private bytes after the sweep 325 MiB (1.0.0) / 445 MiB (1.1).
Attribution, measured separately on the same copies: three 24 h replays
add 34 MiB (1.0.0, truncated) / 58 MiB (1.1, complete); one `/api/guess`
adds ~270-330 MiB in both builds (posterior grids; out of scope of this
work, design §3.21). The 233 MiB of the baseline was measured with a
different request order; this target is **not met** and is dominated by
`/api/guess`, not by the database.

## 4. Writes on the rt-7d workload

Two runs of the rt-7d workload on the full tree (build of 05:47 UTC), one
after the other on port 18925: the migrated gen7 copy compacted with
`airview db snapshot` (the state of a station after the migration and its
vacuum tail), `-demo -set demo.aircraft=1300`, `GOMAXPROCS=4`,
`GOMEMLIMIT=600MiB`, steady state minutes 11-22, then a graceful stop
(Ctrl+Break; 1.5-1.6 s, nothing dropped: `droppedLow/Mid/High` 0).
Process counters from `sample.ps1` (Win32 WriteTransferCount), commits and
checkpoints from `walwatch` (WAL index in the -shm file).

| Per hour | 1.0.0 (baseline rt-7d) | 1.1 `standard` | 1.1 `low-wear` |
|---|---:|---:|---:|
| bytes written by the process | 747 MB | **345 MB** (-54 %) | **266 MB** (-64 %) |
| WAL appended | 469 MB | 267 MB | 221 MB |
| commits | 1,925 | 438 | 216 |
| WAL frames per commit | 59 | 148 | 248 |
| checkpoints | 93 (~185 fsyncs) | 12 (~24) | 6 (~12) |
| database-file back-fill (upper bound: frames checkpointed) | ~278 MB | <= 268 MB | <= 218 MB |
| CPU of the whole process (one core) | - | 3.5 % | 5.2 % |
| private bytes (end of window) | - | 188 MiB | 184 MiB |
| `airview.log` | ~20 KB | 4.8 KB | 7.7 KB |

Checkpoints and commits reach their targets (low-wear commits are 216/h
instead of <= 150: synchronous `Write`/`Apply` calls commit on their own).
Bytes written do **not** reach the 160 / 80 MB/h targets. Where the
remaining WAL comes from (`walpages` on a WAL copy of the `standard` run,
06:39 UTC, 15 commits, 1,872 frames):

| Table / index | WAL frames | distinct pages |
|---|---:|---:|
| `aircraft` | **64.7 %** | 473 |
| `flights` | 8.9 % | 42 |
| `guess_samples` | 6.1 % | 59 |
| `coverage` | 4.1 % | 38 |
| `trail_tail` | 2.3 % | 41 |
| `aircraft_last_seen` | 2.2 % | 10 |
| `flights_end` / `flights_icao` | 1.9 % / 1.8 % | 4 / 32 |
| `signal_hist`, `met_sample`, `elev_hist`, the rest | < 2 % each | |

The trail, heat and statistics streams (together 67 % of the 1.0.0 WAL)
are now a few percent. What is left is the `aircraft` row refresh: with
1,300 simulated airframes keyed by random ICAO, every commit rewrites about
half of the table's ~1,000 leaf pages (~80 frames per commit). That is the
next lever (design §3.4: write an aircraft row only when its counters
moved enough, or accumulate per minute like the flights), handed to group B
in docs/aggregator/HOOKS.md, as are the remaining `flights_icao` writes
(target zero). With the aircraft share removed the same runs would write
about 90 (`standard`) / 80 MB/h (`low-wear`) of WAL (est., same table shares assumed for low-wear). A real station has far
fewer distinct airframes per hour than the 1,300-aircraft simulator, so its
aircraft share is smaller.

Earlier runs, for comparison: on the **unvacuumed** migrated file the
background incremental vacuum dominates the first ~20 minutes (555 -> 424 MB
in 8 minutes, about 65 MB moved every ~3 minutes), and an integration run of
group A that migrated the 1.0.0 copy during the run measured 322 / 259 MB/h
in minutes 11-22 with the migration's vacuum tail inside the window.

## 5. Profiles in force

Resolved by `store.ResolveProfile` and printed by `airview db profile`
(values after A's measurements, docs/CONFIG.md "Storage profiles"):

| | standard | low-wear | server |
|---|---|---|---|
| group commit | 10 s | 30 s | 5 s |
| trail / day stats / heat / range partial | 60 s / 60 s / 300 s / 15 min | 60 s / 300 s / 900 s / 30 min | aggregator lanes |
| checkpoint | 40 MiB or 5 min | 48 MiB or 10 min | 64 MiB or 60 s |
| hard cap / journal_size_limit | 64 / 64 MiB | 64 / 64 MiB | 512 / 128 MiB |
| caches (writer + readers) | 16 + 4 x 8 MiB | 8 + 2 x 4 MiB (3 readers with >= 2 GiB; 32-bit writer 4 MiB) | 64 + 8 x 16 MiB |
| mmap | 256 MiB (0 on Windows) | 64 MiB (0 on 32-bit) | 1 GiB |
| tile cache default | 512 MB | 128 MB | 512 MB |

Changes against the design: checkpoint soft size 40 / 48 MiB instead of
16 / 32 MiB (16 MiB gave 30 checkpoints/h, group A), journal_size_limit
64 MiB, and no mmap on Windows (a mapping keeps the incremental vacuum from
shrinking the file). The page-cache target "<= 16 MiB (Pi 64-bit), <= 10 MiB
(32-bit)" is the configured maximum of a 1 GiB board (8 + 2 x 4 MiB); a Pi
with >= 2 GiB gets a third reader (20 MiB) and 32-bit gets 4 + 2 x 4 = 12 MiB.
Idle readers close after 2 minutes, which returns their caches.

## 6. End-to-end checks

- **Old-format database, background migration, low-wear.** The 1.0.0
  binary (copy of `dist\airview.exe`) recorded 9 minutes of demo traffic
  into a scratch directory; the 1.1 build then started on it with
  `storage.profile=low-wear`. `airview db migrate --status` before the
  start: "not started"; 15 s after the start every data migration (trail,
  heat, range, met, flights_hour, hist_day) was done, the backup
  `backup/airview-pre-1.1-<date>.db` existed, and every logical count
  equalled the 1.0.0 file (positions 16,644, flights 271, heat 12,469,
  coverage 2,401, range_hour 3). Found: `/api/db/stats` reported `heat` 0
  rows during the ~15 s before the heat step (HOOKS, to group C).
- **Graceful stop loses nothing (low-wear).** After 7 minutes, Ctrl+Break
  (the Windows equivalent of SIGTERM for a console program): stopped in
  58 ms, WAL truncated. Counts in the database right before the stop vs
  after a restart: heat rows 12,469 -> 21,422 (7 minutes of the 15-minute
  heat stream were held in memory and written at the stop), coverage
  3,025 -> 3,155, signal 2,617 -> 2,665, elevation 407 -> 420, positions
  30,812 -> 33,205. `engine.TestLowWearStopLosesNothing` checks the same
  with sums of the counts (stored + pending before the stop <= stored
  after the restart).
- **Journal level.** With `JOURNAL_STREAM` set (systemd), standard error
  carried one line (the admin-password warning) in 7 minutes while
  `airview.log` had every INFO line.
- **Commands.** `airview db profile`, `db migrate --status | --backup |
  --now` and the new `db vacuum` are covered by `cmd/airview/dbcmd_test.go`
  on `testdata/airview-1.0.0.db` (recorded by the 1.0.0 build).

## 7. Reproducing

Scratch tools in `scratchpad\dbeff\measure` (sample.ps1, walwatch, rates.ps1,
sweep.ps1, dbinspect); scripts of this report in `scratchpad\dbeff\E`
(sizes.ps1, sweeprun.ps1, rtrun.ps1, ctrlrun). The rt-7d runs use the
migrated gen7 copy compacted with `airview db snapshot`, `-demo -set
demo.aircraft=1300`, `GOMAXPROCS=4`, `GOMEMLIMIT=600MiB`, minutes 11-22.

## 8. Integration pass (15 Nov, tree of 07:41 UTC)

Changes after sections 1-6: the aircraft rows of active airframes are
refreshed every `Profile.AircraftRefresh` (standard 300 s, low-wear 900 s;
`storage.tuning.aircraftFlushSec`) instead of every 60 s, with readers of
aircraft rows forcing a refresh of rows older than 60 s first
(`Tracker.FlushAircraftRows`); met writes go to `met_sample` only; the
forecast issue overlay is applied by `WeatherForecast`; the met collector
writes its open hour before an export (`FlushOpenHour`).

rt-7d, same method as A's integrated measurement (copy of the 1.0.0 gen7
file, data migration in the first ~2 min, `-demo -set demo.aircraft=1300`,
`GOMAXPROCS=4`, `GOMEMLIMIT=600MiB`, 28 min, steady state minutes 14-27,
both profiles in parallel on the i9; nobody browsing, so no reader-forced
aircraft refresh):

| Metric | 1.0.0 | standard before | standard now | target | low-wear before | low-wear now | target |
|---|---:|---:|---:|---:|---:|---:|---:|
| all bytes written by the process | 747 MB/h | 321 MB/h | **229 MB/h** (-69 % vs 1.0.0) | <= 160 | 247 MB/h | **120 MB/h** (-84 %) | <= 80 |
| WAL bytes | 469 MB/h | 250 MB/h | 159 MB/h | | 195 MB/h | 83 MB/h | |
| database-file writes (process - WAL) | ~278 MB/h | ~70 MB/h | ~70 MB/h | <= 40 | ~52 MB/h | ~37 MB/h | <= 20 |
| commits (walwatch tx) | 1,925 /h | 388 /h | 402 /h | <= 450 | 125 /h | 125 /h | <= 150 |
| checkpoints | 93 /h | 9.2 /h | 9.2 /h | <= 13 | 4.6 /h | 4.6 /h | <= 7 |
| largest WAL file | - | 46.6 MB | 46.7 MB | <= 64 MiB | 57.3 MB | 57.5 MB | <= 64 MiB |
| peak private bytes | - | 184 MiB | 185 MiB | - | 190 MiB | 188 MiB | - |
| Stats.Dropped / flush errors | - | 0 | 0 / 0 | 0 | 0 | 0 / 0 | 0 |

The byte targets are still missed, by less: what remains in the WAL is
mostly the heat, flights and statistics increments the design table
schedules (the page-level write amplification of a 4 KiB page per touched
leaf). Database sizes are unchanged (gen7 553 MB -> 108 MB after the
migration and its vacuum tail).

Parity: `tools/dbparity` (fixture e91a36db1119, old = 1.0.0, new = this
tree, ports 18930/18931) passes migrate, crash and live, live with 0 of 246
calls differing (aircraft rows, met_profile export, records and alerts
equal). Aggregator start-up: a 1.0.0 aggregator data directory is migrated
automatically by the new build (airview.db data migrations 15 s after
Open; stations.db has DDL migrations only, reported as `none` by
`airview db migrate --status`).

Scratch: `scratchpad\dbeff\integrator` (runrt.ps1, rt\std, rt\low,
parity, agg).
