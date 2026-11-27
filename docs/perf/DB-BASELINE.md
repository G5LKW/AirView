# Station database baseline (AirView 1.0.0, before the efficiency work)

Measured on 2026-11-15 against the 1.0.0 code (a copy of `dist\airview.exe`
and an unmodified copy of the source tree), before any database change. It
gives every later change a "before" number for a station on a Raspberry Pi
with an SD card, and ranks where the bytes, the writes, the CPU and the
waiting go. Every number was measured. Projections (extrapolated in time or
scaled to the Pi) are marked, and the text says how they were made.

Units: MB = 10^6 bytes. "Per hour" means one wall-clock hour of a running
station.

## 1. Summary

| What | Baseline |
|---|---|
| Database after 7 days, 250 aircraft at the daily peak (826 msg/s average, 1,221 msg/s peak hour) | **553 MB**, of which `positions` is 530 MB (96 %) |
| Database after 7 days, 450 aircraft at the daily peak (1,181 msg/s average, 1,601 msg/s peak hour) | **941 MB**, of which `positions` is 909 MB |
| Growth per day | `positions` 76 MB (250 peak) to 130 MB (450 peak), `met_obs` +14 to 24 MB, everything else +1 to 2 MB |
| Steady state with the default retention (trails 30 d, flights 730 d, minute statistics 90 d, the rest forever), 250 peak | **~2.9 GB after 30 days, ~3.6 GB after 1 year, ~4.3 GB after 2 years** (projection, section 4.3); ~5.8 GB after 1 year at 450 peak |
| Bytes written by the process, busy station (170-250 live aircraft, 900-1,100 msg/s) | **450-500 MB/h** on a fresh database (still rising after 14 min), **747 MB/h** with 7 days of history in steady state: **11-18 GB per day** |
| of which WAL appends | 328-350 MB/h fresh, **469 MB/h** with history (80-85 k and 114 k frames of 4 KB per hour); the rest (~120-280 MB/h) is checkpoint back-fill into the database file |
| Commits | ~1,900 per hour (one every 2 s). About 90 % of the WAL bytes come from the tracker's flush every 15 s (~400 pages = 1.6 MB per flush) |
| WAL checkpoints | 80-86 per hour fresh, 93 per hour with history. They run inside the writer's commit: ~160-185 fsyncs per hour |
| Write amplification | ~100-150x (bytes written / bytes of data kept, ~5-6 MB/h) |
| CPU, whole station process (including the demo simulator) | 3.5-3.8 % of one i9-13900K core in steady state. Estimate for a Pi 4: 17-27 % of one core |
| Store share of the CPU | small: the writer goroutine took 1.2 % of the profile samples (2.5 s in 20 min, 80 % of it the checkpoint fsync); the cost of the store on a Pi is I/O and stalls, not CPU |
| Memory | working set 85-140 MiB, private bytes 110-126 MiB in normal operation; **210 MiB working set / 233 MiB private after one pass over the heavy pages**, and it stays there |
| Slowest read APIs on the 7-day database (i9, first request after a start, partly cold cache) | `/api/db/stats` **16.7 s** cold / 0.45 s warm (COUNT(*) of positions), `/api/guess` 3.9 s (CPU), `/api/replay?hours=24` 1.2 s (truncated at its 400 k point budget), `/api/stats/overview` 0.63 s. Everything else is under 0.25 s. On a Pi 4 expect 5-7x for CPU, and SD-card speed for cold pages |

Top opportunities (section 10 has the evidence, the estimates and the
risks):

1. **Checkpoint policy.** Move the checkpoint out of the commit path and run
   it less often. Today the database file receives ~120-280 MB/h of random
   4 KB page writes from 80-93 checkpoints per hour; each one also costs two
   fsyncs and stalls the writer. Measured (exp1): `wal_autocheckpoint`
   10,000 pages alone cut checkpoints from 93 to 12 per hour and the
   database-file writes from 254 to 94 MB/h (-63 %; all writes -20 %).
2. **Fewer pages per tracker flush.** Flush trail points and increments less
   often, and stop rewriting unchanged indexed columns of active flights and
   aircraft every minute. positions + heat are ~67 % of all WAL frames,
   aircraft + flights_icao another ~11 %. Measured (exp2): exp1 plus a
   60-s tracker flush and a 10-s store flush cut all writes from 702 to
   274 MB/h (-61 %; WAL -50 %, database file -80 %, commits 1,913 -> 528
   per hour), and the rows written per day by 45 %.
3. **Lossless trail packing.** positions is 96 % of the file at 38 B/point.
   A per-flight delta/varint chunk measures 10.2 B/point (3.2x smaller),
   6.7-6.9 B/point with deflate (4.8x): 2.27 GB -> ~0.5-0.7 GB at the
   30-day plateau.
4. **Second copies of trail data.** `met_obs` (~420 MB at its 30-day
   plateau, 82 B/row) and `guess_samples` (~75 MB, 1.1 M inserts per day)
   are both written on a station whose location is already known.
5. **Tiering of the long-lived station tables** (flights for 730 days,
   daily histograms and range_hour forever): ~150-250 MB per year.
6. **Read paths**: cheap table counts for `/api/db/stats` (a full walk of
   positions: 16.5 s with a cold cache even on NVMe, tens of seconds to
   minutes on an SD card), replay reading at its frame step (24 h replay is
   truncated today), a daily rollup for the overview, an index for events
   by aircraft.

## 2. Method

Host: Windows 11, i9-13900K (24 cores, 32 threads), 64 GB RAM, NVMe SSD.
Every run used a Pi-like Go runtime profile: `GOMAXPROCS=4`,
`GOMEMLIMIT=600MiB`.

| Run | What | Scratch dir |
|---|---|---|
| gen7 | 7 days of data generated at data time through the real write path: `sim` (`Advance`, virtual clock) -> `tracker.Tracker` -> `store.Store` (default Options). The engine scheduler is emulated: RollupHours + Checkpoint every data hour, Prune every data day. 250 aircraft at the diurnal peak (simulator default site 50.05 N 8.60 E, two antennas), seed 1. 499 M frames = 826 msg/s on average, 1,221 msg/s in the peak hour. 8 minutes of wall time. | `dbeff\gen7` |
| gen7busy | Same, with 450 aircraft at the peak, seed 2: 714 M frames = 1,181 msg/s on average, 1,601 msg/s in the peak hour | `dbeff\gen7busy` |
| gen31 | Same as gen7 for 31 days: the plateau of the 30-day trail retention, and the first Prune that deletes a whole day | `dbeff\gen31` |
| gen1trace | 1 day as gen7, through an SQL-tracing driver wrapper: exact statement counts per table | `dbeff\gen1trace` |
| rt-fresh | The unmodified 1.0.0 binary (a copy of `dist\airview.exe`), `-headless -demo -set demo.aircraft=1300`, empty data directory, real time. At night the simulator's diurnal curve scales 1,300 down to 190-250 live aircraft at 1,000-1,100 msg/s (two-antenna, Radarcape-like demo feed). Every 60 s: process I/O counters (Win32_Process), CPU and working set. The WAL index (`-shm`) was polled every 20 ms for commits, WAL frames, WAL resets and checkpoints. | `dbeff\rt-busy` |
| rt-7d | The same build plus an SQL-tracing `database/sql` driver wrapper and a CPU/heap profile (`AIRVIEW_SQLTRACE`, `AIRVIEW_CPUPROFILE`; scratch-only instrumentation). Demo 1,300 on a **copy of the gen7 database** (a station with 7 days of history) in real time, 00:39-01:05, with the read-API sweep (section 7) at 00:48:30 against the same running instance. WAL snapshots were attributed to tables page by page. | `dbeff\rt-7d` |
| exp1, exp2 | Two what-if runs, each 16 minutes on a fresh copy of the gen7 database with demo 1,300, built from the scratch source copy with environment overrides (scratch only): exp1 `wal_autocheckpoint(10000)`; exp2 the same plus the tracker flush 15 s -> 60 s and the store FlushInterval 2 s -> 10 s (section 10.1) | `dbeff\rt-exp`, `dbeff\rt-exp2` |

The generator writes everything the tracker writes: aircraft, flights,
positions, statistics, range, coverage, histograms, heat, records, events
and receiver status. The tables written by other engine components
(met_obs / met_profile, guess_samples, latency_minute, rfstat, weather,
predict, alerts, navdata) were measured in the real-time runs and projected
linearly from their ratio to positions rows.

Caveats:
- The simulator invents a new airframe for most flights (29,111 airframes
  in 7 days). A real station sees many repeat visitors, so the `aircraft`
  table is overstated here; it is small either way.
- Simulated trails are smoother than real ones, so the trail-packing
  estimate is probably 10-20 % optimistic.
- I/O and CPU were measured on an NVMe/i9 host. The Pi 4 projections
  assume one Cortex-A72 core is ~5-7x slower than one i9-13900K core for
  this kind of Go code, and an A1/A2 SD card does ~1-3 k random 4 KB
  reads/s, ~0.3-1 k random 4 KB writes/s and 20-90 MB/s sequential.
- Windows has no fsync counter, so fsyncs are derived from the checkpoint
  count. WAL mode with `synchronous=NORMAL` syncs only in checkpoints: the
  WAL before the back-fill and the database after it, plus the hourly
  `wal_checkpoint(TRUNCATE)`. The CPU profile confirms that the sync cost
  sits in `FlushFileBuffers` under `walCheckpoint`.
- On Windows the Go profiler also counts threads blocked in a syscall
  (`FlushFileBuffers`, `ReadFile`), so "CPU" samples below include I/O
  waiting. On Linux that time would show as wall time, not CPU.

## 3. Configuration as found

### 3.1 Connections and pragmas (internal/store/store.go)

| Setting | Writer (1 connection) | Readers (pool of 4, `DB()`) | Stored in the file |
|---|---|---|---|
| page_size | | | 4096 (SQLite default) |
| journal_mode | WAL | (WAL) | wal |
| synchronous | NORMAL | default | |
| auto_vacuum | INCREMENTAL (new files only) | | 2 = incremental |
| cache_size | -16384 (16 MiB) | -8192 (8 MiB each, 32 MiB for the pool) | |
| mmap_size | 64 MiB (0 on 32-bit) | 64 MiB | |
| temp_store | MEMORY | MEMORY | |
| wal_autocheckpoint | 1000 pages (~4 MiB) | | |
| journal_size_limit | 32 MiB | | |
| busy_timeout | 10 s (1 s during Checkpoint) | 10 s | |
| foreign_keys | on (the schema has no foreign keys) | on | |
| query_only | | 1 | |
| _txlock | immediate | | |

The writer is one goroutine. Asynchronous rows are queued and committed in
ONE transaction every `FlushInterval` = 2 s whenever anything is queued
(`FlushRows` 4,000 triggers earlier, `MaxQueueRows` 200,000 bounds the
queue). The tracker hands over increments and trail points every 15 s,
flight and aircraft rows every 60 s, minute rows every minute and partial
range_hour rows every 5 min. The other components (guess samples, met
observations, latency, alerts, NTP, weather) queue their own rows in
between, so in practice there is a commit every 2 s (measured: ~1,900
commits per hour).

Maintenance on a **station** (engine/sched.go): `RollupHours` and
`Checkpoint` (`wal_checkpoint(TRUNCATE)`) 3 minutes after every hour,
`Prune` once a day (and 5 minutes after the start), latency minutes once a
minute. `PRAGMA optimize` runs only in `Close`, so a station that is never
restarted never refreshes its planner statistics. `Maintain` (tiered
retention, trail thinning, histogram month buckets, incremental vacuum,
periodic optimize) runs only in aggregator mode. Incremental vacuum runs
only when more than a quarter of the file is free.

Retention defaults: trails (`positions`, plus `met_obs`) 30 days, flights
730 days, minute statistics / NTP samples / receiver status 90 days.
stats_hour, stats_day, range_hour, coverage, signal_hist, elev_hist, heat,
aircraft, events, records, weather and met_profile are kept forever.
guess_samples: 7 days, at most 2,000,000 rows per station (pruned every
10 minutes).

## 4. Disk size

### 4.1 Per table after 7 days (gen7: 250 aircraft at the peak, 826 msg/s on average)

Sizes come from `dbstat` (pages, including free space inside pages). File =
553,299,968 bytes, no free-list pages, WAL truncated. "Unused" = free bytes
inside the table's own pages.

| Table (+ its indexes) | Rows | Table | Indexes | B/row incl. indexes | Unused in pages | Growth / day |
|---|---:|---:|---:|---:|---:|---:|
| positions (PK flight_id, t; WITHOUT ROWID) | 13,873,520 | 530.1 MB | - | 38.2 | 74.2 MB (14 %) | **75.7 MB** |
| flights (rowid + 6 indexes) | 31,187 | 5.15 MB | 2.79 MB | 254.5 | 0.5 MB | 1.13 MB |
| heat (all-time grid) | 95,424 | 4.44 MB | - | 46.6 | 0.56 MB | 0.63 MB, saturating |
| stats_minute | 30,240 | 3.58 MB | - | 118.4 | 0.41 MB | 0.51 MB |
| aircraft (+ aircraft_last_seen) | 29,111 | 2.58 MB | 0.47 MB | 104.5 | 0.28 MB | 0.43 MB (the sim invents a new airframe per flight) |
| coverage | 26,460 | 1.20 MB | - | 45.4 | 0.14 MB | 0.17 MB |
| range_hour (rowid + autoindex) | 504 | 0.69 MB | 0.02 MB | 1,406 | **0.33 MB (48 %)** | 0.10 MB |
| receiver_status | 10,012 | 0.55 MB | - | 55.2 | 0.07 MB | 0.08 MB |
| signal_hist | 22,942 | 0.47 MB | - | 20.5 | 0.05 MB | 0.07 MB |
| events (+ events_t) | 2,018 | 0.20 MB | 0.03 MB | 113.7 | | 0.03 MB |
| elev_hist | 3,789 | 0.15 MB | - | 38.9 | | 0.02 MB |
| stats_hour | 504 | 0.07 MB | - | 146.3 | | 0.01 MB |
| everything else (schema, kv, records, planner statistics) | | 0.07 MB | | | | |

range_hour stores its 480-byte `dist_hist` and 144-byte `sector_max` BLOBs
raw on a station (stations.db already packs them as varints), and its
partial row is replaced every 5 minutes. That explains the 48 % of free
space in its pages.

Tables the generator does not write were measured in the real-time runs
(~200 aircraft, fresh database) and scaled by their ratio to positions rows:

| Table | Rows/h (real time) | B/row incl. indexes | Projected growth / day (gen7 traffic) | Retention -> plateau |
|---|---:|---:|---:|---|
| met_obs (rowid + met_obs_t) | 8,100-8,700 | 82.4 (66.3 + 16.1 index) | 171 k rows = **14.1 MB** | 30 days (trail retention) -> **~420 MB** |
| guess_samples | 46,000-49,000 | 37.3 | 0.97 M rows = 36 MB | 7 days, capped at 2 M rows -> **~75 MB** |
| met_profile | ~7,500 UPSERTs (~180 new rows) | ~40 | ~0.04 MB | forever |
| latency_minute, rfstat_hour, weather_*, predict_forecast_log, alerts_* | | | < 0.3 MB together | |

Busy profile (gen7busy: 450 aircraft at the peak, 1,181 msg/s on average,
1,601 msg/s in the peak hour): the file is 941 MB after 7 days. positions
has 23,504,251 rows = 909.2 MB (130 MB/day, 127.4 MB = 14 % unused), flights
56,357 rows = 14.4 MB, aircraft 49,893 rows = 5.3 MB; the statistics tables
are the same size as in gen7.

### 4.2 What a trail point costs

The positions payload is 29.8 B/row (record header ~10 B plus 9 columns;
t and flight_id are repeated in every row), plus ~2.5 B of cell overhead,
plus 14 % free space in pages. Trail points are inserted in the middle of
the B-tree (one open tail per active flight, ~250 at a time), so pages
split half full and only fill to ~86 %. A `VACUUM INTO` of the 7-day file
writes 478 MB instead of 553 MB (-13.5 %, 4.3 s on the i9); new trails
fragment the same way again.

A per-flight chunk encoding was computed over all 13.9 M points with the
scratch tool `trailpack`: time-ordered points, every column delta- and
zig-zag-varint-encoded, 256 points per row keyed (flight_id, t0), lossless,
NULLs kept. Result: **10.15 B/point = ~163 MB instead of 530 MB (3.2x)**,
and **6.70 B/point = ~108 MB (4.9x)** with deflate (BestSpeed) per chunk.
The busy database gives the same ratios (10.19 and 6.93 B/point: 278 and
189 MB instead of 909 MB), and so does the 30-day plateau of gen31 (59.4 M
points, 10.16 and 6.71 B/point: **~700 MB or ~464 MB instead of 2,325 MB**).

### 4.3 Steady state with the default retention (projection)

Made from the measured growth per day (gen7) and the measured real-time
ratios. positions reach their 30-day plateau after a month; flights keep
growing for two years. aircraft is shown with the simulator's airframe
count; a real station's repeat visitors make it 5-10x smaller.

| | 30 days | 1 year | 2 years |
|---|---:|---:|---:|
| positions (30 d) | 2,272 MB | 2,272 MB | 2,272 MB |
| met_obs (30 d) | 422 MB | 422 MB | 422 MB |
| flights (730 d) | 34 MB | 414 MB | 828 MB |
| guess_samples (cap) | 75 MB | 75 MB | 75 MB |
| stats_minute (90 d) | 15 MB | 46 MB | 46 MB |
| coverage + signal_hist + elev_hist (forever) | 8 MB | 95 MB | 190 MB |
| range_hour (forever) | 3 MB | 37 MB | 74 MB |
| aircraft (forever; simulator upper bound) | 13 MB | 159 MB | 317 MB |
| events, stats_hour, heat, receiver_status, met_profile, the rest | ~20 MB | ~55 MB | ~75 MB |
| **total** | **~2.9 GB** | **~3.6 GB** | **~4.3 GB** |

At 450 aircraft at the peak: positions 3.9 GB, met_obs ~0.7 GB and flights
0.75 GB per year, so ~5.8 GB after one year.

Check of the plateau (gen31: 31 days generated, Prune with the default
30-day trail retention): file **2.41 GB** (2,411,667,456 bytes, 7 free-list
pages: Prune frees pages and the next inserts reuse them). positions holds
59,409,507 rows = **2,325 MB** (the projection said 2,272 MB; 325 MB of it
is free space inside pages), flights 138,449 rows = 22.9 MB, stats_minute
15.9 MB, aircraft 9.7 MB, coverage 5.3 MB, heat 4.7 MB (saturating: 100 k
cells after 31 days against 95 k after 7), range_hour 3.1 MB (1.45 MB of it
free space).

## 5. Write volume and SD-card wear

### 5.1 Per hour, real time

| | rt-fresh (empty DB, pristine 1.0.0) | rt-7d (7-day DB copy, traced build) |
|---|---:|---:|
| Window (steady state) | 00:30:32-00:38:33, minutes 6-14 after the start (still rising: 501 MB/h in the last 4 min) | 00:50-01:01, minutes 11-22 (after the ramp-up and after the read sweep) |
| Live aircraft / message rate | 190-250 / ~1,100 msg/s | 170-200 (127 with position) / 900-1,000 msg/s |
| Bytes written by the process (Win32 WriteTransferCount) | **450 MB/h** | **747 MB/h** |
| Write calls | 191 k/h | 290 k/h |
| Write transactions (WAL index iChange) | 1,888/h | 1,925/h |
| WAL frames appended (4,096 + 24 B each) | 79.6 k/h = **328 MB/h** | 113.9 k/h = **469 MB/h** |
| Frames per transaction (mean) | 42.1 | 59.2 |
| WAL checkpoints (automatic at 1,000 frames, plus the hourly TRUNCATE) | 80/h | 93/h |
| fsync-equivalents (2 per checkpoint) | ~160/h | ~185/h |
| Writes to the database file (process writes minus WAL; the log is < 0.1 MB/h) | ~122 MB/h | **~278 MB/h** |
| Bytes read by the process | 170 MB/h (mostly checkpoints reading WAL frames back) | 451 MB/h (checkpoints, plus cold pages of the 553 MB file) |
| New data actually kept | ~5-6 MB/h | ~5-6 MB/h |

That is **11-18 GB written per day to keep ~120-150 MB/day**: a write
amplification of ~100-150x. Both runs ramp up for 10-15 minutes after a
start (the tracker fills up with flights), so the 7-day steady state is
the representative number. The database file itself receives 120-280 MB/h
of random 4 KB page writes (the checkpoint back-fill), which is the worst
pattern for an SD card: erase blocks of 4 MB and more, and the card's own
write amplification on top.

### 5.2 Where the WAL frames come from

Per-commit histogram (rt-7d; the WAL index was polled every 5 ms and 577
commits in 20 minutes could be attributed one by one):

| Frames per commit | Commits | Frames | Share of frames |
|---|---:|---:|---:|
| 1-4 | 350 | 912 | 2.6 % |
| 5-16 | 149 | 1,235 | 3.6 % |
| 65-256 | 3 | 729 | 2.1 % |
| 257-1,024 (mean 421 = 1.7 MB) | 75 | 31,608 | **91.7 %** |

The 2-second commits of the small writers (guess samples, met
observations, latency, events) are cheap, ~3 frames each. About 90 % of the
WAL volume is the tracker's 15-second flush: ~400 distinct 4 KB pages every
15 s.

Which tables those pages belong to: 8 copies of the live WAL taken 41 s
apart (6,084 frames) were mapped page by page to their owners with
`dbstat`:

| Object | WAL frames | Share | Distinct pages (summed over the 8 snapshots) | Why |
|---|---:|---:|---:|---|
| positions | 2,133 | **35.1 %** | 1,137 | one open tail page per active flight (~250), rewritten at every flush |
| heat | 1,950 | **32.1 %** | 1,169 | UPSERTs of random cells all over the all-time grid (key = cell) |
| coverage | 389 | 6.4 % | 180 | today's bins |
| guess_samples | 387 | 6.4 % | 62 | one insert stream, small commits every 2 s |
| aircraft | 368 | 6.0 % | 361 | UPSERT of every active airframe every 60 s; rowid = ICAO, so random pages |
| flights_icao (index) | 285 | 4.7 % | 265 | the flight UPSERT assigns `icao` and `start` (and `"end"`, dep, arr) on every 60-s refresh; far more flights_icao pages change than the ~3 new flights per minute could touch, so the (icao, start) entries of the active flights are rewritten on random pages although their values do not change |
| signal_hist | 182 | 3.0 % | 84 | today's bins |
| met_obs + met_obs_t | 76 | 1.3 % | 46 | |
| elev_hist, flights, aircraft_last_seen, met_profile, flights_arr, schema, rest | 314 | 5.2 % | | |

Check of the flights_icao effect on a scratch table (20,000 rows, index on
(icao, start)): an UPSERT of 500 random rows that assigns `icao` and `start`
their unchanged values dirtied 81 table pages **and 80 index pages**; the
same UPSERT assigning only a non-indexed column dirtied 91 table pages and
no index page. SQLite rewrites the index entry whenever an indexed column is
assigned, whether or not the value changes.

Within one WAL generation (~45 s) a page appears 1.76 times on average:
the same hot pages are appended again by each flush, and only the
checkpoint de-duplicates them.

### 5.3 Rows written per day (gen1trace, exact statement counts)

| Statement | Rows / day | Share | Writer time (i9) |
|---|---:|---:|---:|
| UPSERT signal_hist | 4,817,136 | 37 % | 10.9 s |
| UPSERT heat | 2,453,133 | 19 % | 7.5 s |
| UPSERT coverage | 1,805,936 | 14 % | 5.3 s |
| INSERT OR REPLACE positions | 1,755,129 | 13 % | 4.6 s |
| UPSERT elev_hist | 360,974 | 3 % | 0.8 s |
| UPSERT flights (every 60 s per active flight) | 200,325 | 2 % | 1.8 s |
| UPSERT aircraft (every 60 s per active airframe) | 196,481 | 2 % | 1.0 s |
| stats_minute, range_hour, records, events, receiver_status, kv | ~7,000 | < 0.1 % | 0.05 s |
| (real time only) guess_samples | ~1.1-1.2 M | | |
| (real time only) met_obs inserts + met_profile UPSERTs | ~0.4 M | | |

In total the store writes 13.3 M rows per day at 250 aircraft peak (gen7:
93.3 M rows in 7 days) and 19.9 M rows per day at 450 peak (gen7busy:
139.0 M rows), plus ~1.5 M guess-sample and met rows in real time. 72 % of
all rows written are histogram / grid increments. signal_hist alone
receives ~1,460 UPSERTs per stored row and day (4.8 M increments for
~3,300 rows per day). On the 7-day database a heat UPSERT costs ~14 µs
(random page reads) against ~2 µs for a signal_hist or positions write: in
real time heat used 1.9 s of writer time per hour, more than everything
else together.

## 6. CPU and memory

### 6.1 Whole process, real time

| | rt-fresh (pristine 1.0.0) | rt-7d (traced build, 7-day DB) |
|---|---:|---:|
| CPU | 137 s/h = **3.8 %** of one i9-13900K core | 125 s/h = **3.5 %** (steady state; the read sweep minute took 21.6 s) |
| Working set (MiB) | 135-142 | 84-101 before the sweep, 205-210 after it |
| Private bytes (MiB) | 110-118 | 110-126 before the sweep, 225-233 after it |
| Pi 4 estimate (x5-7 per core) | ~19-27 % of one core | ~17-24 % of one core |

This includes the demo simulator (on a real station the Beast decoder
replaces it) and everything else the station does.

### 6.2 Where the CPU goes (pprof)

Real-time profile (rt-7d, 20 minutes 00:45-01:05, including the read sweep
at 00:48:30): 202 s of samples. On Windows that is more than the process
CPU time measured by the OS in the same window (~60 s), because threads
waiting in system calls are sampled too, so read the shares, not the
seconds.

| Part | Samples | Share |
|---|---:|---:|
| demo simulator (`sim.loop`; replaced by the Beast decoder on a real station) | 58.5 s | 29.0 % |
| tracker (`HandleFrame`) | 31.7 s | 15.7 % |
| HTTP API (the sweep): `TableCounts` 16.6 s (of which `winRead` 17.9 s cumulative: cold page reads), Locate Me posterior (`l2Problem.eval`) 11.8 s, `ForEachPosition` 2.5 s | 22.0 s | 10.9 % |
| **store writer goroutine** (`Store.run`) | **2.5 s** | **1.2 %** |
| of which checkpoint `FlushFileBuffers` inside `Commit` | 1.35 s | 0.7 % |
| of which statements (signal_hist 0.26 s, heat 0.19 s, positions 0.09 s, coverage 0.09 s, flights / aircraft / elev 0.12 s) | ~0.8 s | 0.4 % |

So the steady write path costs ~2.5 s per 20 minutes on the i9 (estimate
for a Pi 4: 1-2 % of one core), and most of that is the fsync of the
checkpoint. What the store costs a Pi is SD-card I/O and the writer
stalls while a checkpoint syncs ~1,000 random pages, not CPU.

Generator profile (gen7: 7 days of data written in 8 minutes, 928 s of
samples): the store's writer goroutine took 453 s = 48.8 % of all samples.
Of that, the WAL auto-checkpoint that runs inside `Commit` took 171 s
(18.4 % of all samples; 143 s = 15.4 % blocked in `FlushFileBuffers`),
`winWrite` 55 s and the statements themselves (`sqlite3VdbeExec`) 209 s.
By operation (cumulative under the flush): MergeSignalHist 81.7 s,
MergeHeat 51.9 s, MergeCoverage 38.9 s, AddPositions 34.1 s, UpsertFlights
13.9 s, UpsertAircraft 7.2 s, MergeElevHist 6.9 s. The simulator and the
tracker took the other half; tracker.HandleFrame needed 123.5 s for 499 M
frames = 0.25 µs per frame.

Maintenance jobs (generator, per data hour or day, i9): RollupHours 42 ms
on average (max 194 ms), Checkpoint(TRUNCATE) 13 ms on average (max
123 ms), the daily Prune 0.5-2 ms while nothing is old enough to delete.
The first Prune that deletes a whole day of trails (gen31: ~4,500 flights,
~2 M positions in chunks of at most 20,000 rows per transaction) took
1,209 ms on the i9 (estimate for a Pi: 6-10 s, plus ~75 MB of freed pages
through the WAL once a day). Over 31 days the hourly RollupHours averaged
76 ms and Checkpoint(TRUNCATE) 13 ms (max 56 ms).

### 6.3 Memory

Go heap in use at 01:05 (after the sweep): 76.7 MB, of which terrain tiles
and panorama 21 MB, the analytics caches the sweep filled (heat grid
7.7 MB, top lists 10.7 MB), trajectory 7.7 MB, SQLite column text 3 MB. The
process private bytes grew from 126 to 233 MiB during the one-minute sweep
and stayed there; under `GOMEMLIMIT=600MiB` the Go runtime has no reason to
give the memory back quickly.

SQLite page caches: writer 16 MiB + 4 readers x 8 MiB = up to 48 MiB of
cache once warm, allocated by the modernc libc allocator outside the Go
heap (it shows in private bytes, not in the heap profile). The 64 MiB mmap per connection maps the same file region (backed
by the OS page cache, not private memory).

## 7. Read latency on the 7-day database

All read APIs listed in `docs/API.md` for a station (75 URLs), 3 requests
each, sequentially with curl against rt-7d (7-day database plus live demo
traffic, `GOMAXPROCS=4`), 00:48:30-00:48:58. r1 is the first request, with
the server's response cache empty. Most of the database was not in the OS
file cache (the copy had bypassed it, see the `/api/db/stats` note), so r1
includes reading cold pages from the NVMe, as the first request after a
reboot on a Pi reads them from the SD card. Most analytics endpoints are cached
by the server for a while, so their r2/r3 (~1 ms) only show the cache;
endpoints without a cache repeat their full cost. SQL times come from the
tracing driver (statement start to rows closed). The Pi 4 column is r1 x
5-7 (CPU); a cold SD card adds to it.

| Endpoint | r1 (ms) | r2 / r3 (ms) | Response | Dominant SQL (time, rows) | Pi 4 est. |
|---|---:|---:|---:|---|---:|
| `/api/db/stats` | **16,672** (cold cache); 454 warm | 1 / 1 (count cache) | 1.8 KB | `SELECT COUNT(*) FROM "positions"` 16,491 ms cold (13.9 M rows, 575 MB read), 423 ms warm | cold: 6-27 s (7 d), 25-110 s (30 d); warm 2-3 s |
| `/api/guess` (Locate Me) | **3,936** | 0.7 / 0.7 | 9.8 KB | only 6 ms of SQL (7,338 guess samples); the rest is the posterior computation (CPU) | 20-30 s |
| `/api/replay?hours=24` | **1,185** | 0.8 / 18 | 858 KB | ForEachPosition (flights JOIN positions) 1,159 ms; stops at the 400,000-point budget, reply `truncated: true` (877 aircraft) | 6-9 s |
| `/api/replay?hours=6` | 759 | 1.7 / 1.9 | 1,973 KB | ForEachPosition, 281,227 points | 4-6 s |
| `/api/stats/overview` | 634 | 0.7 / 0.7 | 11 KB | `SELECT (start+?)/86400, COUNT(DISTINCT icao), COUNT(*) FROM flights ... GROUP BY 1` 377 ms (temp B-trees); `SELECT COUNT(*), ... COUNT(DISTINCT ...) FROM aircraft` 86 ms; top-list join 78 ms | 3-5 s |
| `/api/reports.csv?hours=168` | 229 | 188 / 186 | 6.1 MB | flights LEFT JOIN aircraft LEFT JOIN acdb, 31 k rows | 1.2-1.6 s |
| `/api/export/aircraft.csv` | 111 | 106 / 97 | 3.2 MB | aircraft LEFT JOIN acdb | 0.6-0.8 s |
| `/api/antenna/signal` | 107 | 0.7 / 0.7 | 19 KB | signal_hist GROUP BY 18 ms + fit | 0.5-0.8 s |
| `/api/antenna/coverage` | 91 | 0.7 / 1.1 | 66 KB | coverage 10-39 ms + outline computation | 0.5-0.7 s |
| `/api/stats/top?limit=10` | 87 | 0.6 / 0.8 | 3 KB | flights JOIN aircraft JOIN acdb 77 ms, 31 k rows | 0.4-0.6 s |
| `/api/stats/altitudes` | 55 | 0.6 / 0.6 | 0.8 KB | flights GROUP BY altitude band 13 ms + coverage 39 ms | 0.3-0.4 s |
| `/api/reports?hours=168&limit=100` | 34 | 32 / 29 | 45 KB | COUNT(*) over the joined report 18-33 ms, run for every page | 0.2 s |
| `/api/stats/rates?hours=168` | 31 | 0.7 / 0.6 | 12 KB | stats_hour / stats_minute | 0.2 s |
| `/api/history/aircraft?limit=50&sort=msgs` | 20 | 21 / 20 | 13 KB | full sort of aircraft (no index on msgs) | 0.1-0.15 s |
| `/api/history/aircraft?limit=50&q=dlh` | 17 | 16 / 17 | 13 KB | 9-way LIKE over aircraft LEFT JOIN acdb, COUNT + page | 0.1 s |
| `/api/replay?hours=0.5&step=60` | 20 | 0.8 / 0.9 | 87 KB | ForEachPosition | 0.1-0.15 s |
| `/api/trails?minutes=30` | 14 | 1.2 / 1.1 | 1.1 MB | in memory | 0.1 s |
| `/api/antenna/heat` (all cells) | 8.9 | 0.9 / 1.1 | 302 KB | `SELECT ... FROM heat WHERE ant = ?`: full scan of every antenna's cells (95 k rows, 73 ms on the first call) | 0.05-0.5 s |
| the other 57 URLs | < 40 (most < 5) | | | | < 0.25 s |

Notes:
- The trail endpoints (`/api/history/flights/{id}`, `/trace`,
  `/api/export/flight/{id}.geojson`) take 3.5-4.2 ms for a 13,726-point
  flight: the (flight_id, t) primary key serves them well.
- `ForEachPosition` (replay, the Locate Me fallback) is bounded by the
  flights "end" index and the positions primary key. Its cost is
  proportional to the rows read and dominated by Go-side scanning
  (~1.7 µs per row on the i9).
- `/api/db/stats` counts every table on a read connection; for positions
  that is a full B-tree walk (~130 k pages, 530 MB). The result is then
  cached for 50x its cost (here 13 minutes), so the first visit to
  Settings -> Database after a start pays it. The 16.5 s were a **cold
  page cache**: the database had been copied with Windows CopyFile, which
  bypasses the file cache for large files, so this first walk read 575 MB
  from the NVMe with 4 KB page reads (~35 MB/s; the process read counter
  jumped by 575 MB in that minute). The same count in the same process 14
  minutes later took 423 ms (`/api/db/stats` 454 ms), and 350-410 ms from a
  separate process; on the 30-day database (gen31, 59.4 M rows) the warm
  count takes 1.54 s. On a Pi after a reboot the cold case is the normal
  one: 530 MB at 20-90 MB/s is 6-27 s for 7 days of trails, and 25-110 s
  at the 2.3 GB plateau.
- The other r1 values in the table were also measured on a partly cold
  cache (the replay and overview queries read pages nobody had touched
  since the copy); they are the "first request after a restart" numbers.
- The heavy pages raise memory: working set 101 -> 209 MiB and private
  bytes 126 -> 233 MiB during the sweep minute, and they stayed there.

## 8. Indexes and query plans

### 8.1 Index list (station airview.db)

| Index | Table | Size after 7 d | Used by (traced statements: sweep + 25 min of writes) |
|---|---|---:|---|
| PK (flight_id, t) | positions (WITHOUT ROWID) | (the table) | trail inserts, `Trail`, `ForEachPosition` (replay, Locate Me), Prune |
| flights_icao (icao, start) | flights | 561 KB | FlightsForAircraft (aircraft history page), covering scan for per-day counts |
| flights_start (start) | flights | 430 KB | reports, statistics ranges, history list paging |
| flights_end ("end") | flights | 430 KB | ForEachPosition, Prune (positions watermark, flights) |
| flights_unresolved ("end") WHERE dep IS NULL | flights | 430 KB | departure / arrival backfill only (needs navigation data). **Without navdata every flight stays unresolved, so it is a full copy of flights_end.** |
| flights_dep (dep, start), flights_arr (arr, start) | flights | 467 KB each | the report filter by airport only; not used in the sweep. Every flight UPSERT (every 60 s per active flight) also sets dep = arr = NULL |
| aircraft_last_seen | aircraft | 467 KB | history list (default sort), "since" filters |
| events_t | events | 33 KB | event list by time. **There is no index on events.icao**: the per-aircraft event list (`/api/history/aircraft/{icao}`) is a full scan of events (7.4 ms at 2 k events; events grow forever, ~290 per day here) |
| met_obs_t | met_obs | ~16 B/row (~100 MB at the 30-day plateau) | met reads, Prune. met_obs is a rowid table inserted in time order, so rowid order is already t order |
| sqlite_autoindex_range_hour_1 (t, ant) | range_hour (rowid table) | 16 KB | every range_hour read and the 5-minute partial REPLACE; a WITHOUT ROWID table would not need it |
| flight_station_sid | flight_station | 1 page | aggregator only |
| navdata_* (8 indexes) | navdata tables | empty until an import | airport / navaid lookups |

There is no redundant (prefix-duplicate) index. Unused on a station
without navigation data: flights_unresolved (a duplicate of flights_end),
flights_dep and flights_arr. Together they take ~1.4 MB per 7 days (~70 MB
at the two-year flights plateau), plus their maintenance on every flight
UPSERT.

### 8.2 Plans of the heavy statements (EXPLAIN QUERY PLAN with real values)

| Statement (caller) | Plan | Note |
|---|---|---|
| ForEachPosition: `flights f JOIN positions p ... WHERE f."end" >= ? AND f.start <= ? AND p.t BETWEEN ? AND ? ORDER BY f."end", f.id, p.t` (replay, Locate Me fallback) | SEARCH f USING INDEX flights_end (end>?); SEARCH p USING PRIMARY KEY (flight_id=? AND t>? AND t<?) | The access path is right; the cost is the rows. Replay reads every stored point (5 s trail) even at a 144 s frame step, and stops at the 400,000-point budget |
| `SELECT COUNT(*) FROM "positions"` (`/api/db/stats`) | SCAN positions | full B-tree walk |
| `SELECT (start+?)/86400, COUNT(DISTINCT icao), COUNT(*) FROM flights WHERE start >= ? AND start < ? GROUP BY 1` (stats overview / daily) | SEARCH flights_start (or a covering SCAN of flights_icao); TEMP B-TREE for GROUP BY and for COUNT(DISTINCT) | 377 ms for 7 days in the station (cold pages); 116 ms warm for 30 days of flights (138 k rows, gen31) from a separate process; grows with the 730-day flights retention |
| `SELECT COUNT(*), SUM(msgs), ... COUNT(DISTINCT NULLIF(country,'')) FROM aircraft` (overview, all time) | SCAN aircraft + TEMP B-TREE | 86 ms; grows forever |
| `SELECT ... FROM heat WHERE ant = ? AND count > 0` (antenna heat) | SCAN heat | the key is (cell, ant), so it reads every antenna's cells (3x the rows it needs) |
| aircraft list `ORDER BY msgs` / 9-way `LIKE` search | SCAN aircraft + TEMP B-TREE | 16-20 ms per page, run for COUNT(*) and again for the page |
| `events WHERE icao = ? ORDER BY id DESC` | SCAN events | see 8.1 |
| report COUNT(*) (flights LEFT JOIN aircraft LEFT JOIN acdb) | SEARCH flights_start | 18-33 ms, computed for every page |
| writes: UPSERT heat / coverage / signal_hist / elev_hist / aircraft / flights, INSERT positions | primary-key point lookups | the cost is page I/O, not the plan |

The live station has `sqlite_stat1/4` only after a clean shutdown
(`PRAGMA optimize` runs in `Close` only). A Pi that is never stopped
cleanly plans without statistics.

## 9. Other disk writers on a station

| Writer | Measured | Notes |
|---|---|---|
| Log (`airview.log`, rotated at 4 MiB; also stderr / journald when run as a service) | 4 KB in the first 14 minutes, ~20 KB/h steady (mostly alert INFO lines for military aircraft and new records) | negligible next to the database. Under systemd the same lines also reach journald, a second copy on the SD card unless journald is volatile |
| Terrain (`data\terrain`) | 42.3 MB once at the first start: 293 elevation tiles (36.1 MB, zoom 9 + 12) and a 6.2 MB gzip polar analysis cache; nothing afterwards | provider cap `MaxDiskMB` 1024; downloaded again only for a new site or radius |
| Map tile cache (`data\tilecache`) | 0 in these runs (no browser attached) | proxy cache for the web map, default cap 512 MB (`tileDefaultMaxMB`); every map pan by a user writes tiles plus a 512-byte header per tile |
| Navigation data import | not triggered (manual, from Settings) | imports into `navdata_*_stage` tables inside airview.db, then swaps them in: the whole dataset (tens of MB) goes through the WAL once per import |
| Config (`config.json`, `airview.run`) | 12.7 KB at the start and on every settings change | negligible |
| Weather / forecast | `weather_fc`: 167 rows (~1,250 B/row) rewritten per poll; `weather_fc_log`, `weather_obs`; default poll every 15 minutes | ~0.2 MB of rows per poll |

## 10. Opportunities, ranked by impact

The ranking weighs SD-card wear, disk size and Pi CPU / latency, with every
feature kept. The estimates come from the measurements above; section 10.1
has the two quick experiments.

| # | Opportunity | Evidence | Estimated gain | Feature risk |
|---|---|---|---|---|
| 1 | **Checkpoint policy**: take the checkpoint out of the writer's commit path and run it less often, e.g. a large or disabled `wal_autocheckpoint` plus a timed PASSIVE checkpoint every few minutes besides the existing hourly TRUNCATE, with `journal_size_limit` to match | 80-93 checkpoints/h; ~280 MB/h of random page writes to the database file on the 7-day DB; checkpoint + FlushFileBuffers = 18 % / 15 % of all samples in the generator profile; each checkpoint stalls the single writer (on a Pi: an SD-card burst of up to ~1,000 random page writes) | measured (exp1, one pragma): checkpoints 93 -> 12 /h (fsyncs ~186 -> ~24), database-file writes 254 -> 94 MB/h (-63 %), all writes 702 -> 563 MB/h (-20 %); the WAL grows to 40 MB. A timed checkpoint outside the commit path also removes the writer stalls | none: the WAL size stays bounded by the timer, and crash safety is unchanged with synchronous=NORMAL |
| 2 | **Fewer page rewrites per tracker flush**: flush trail points and increments less often (15 s -> 60 s or more; the aggregator lanes already use 5 min); refresh aircraft / flight rows less often and only with the columns that changed (never re-set `icao`, `start`, `dep`, `arr` of an active flight) | about 90 % of WAL frames come from the 15-s tracker flush (~400 pages each): positions 35 %, heat 32 %, aircraft 6 %, flights_icao 5 %. 72 % of the rows written are increments | measured (exp2 = exp1 + 60-s tracker flush + 10-s store flush): all writes 702 -> 274 MB/h (-61 %), WAL 448 -> 222 MB/h, commits 1,913 -> 528 /h; rows written per day -45 % (histogram / grid increments -38 to -65 %). Rewriting only changed columns (flights_icao, aircraft) and longer increment intervals would add to this | live statistics pages lag the database by up to the new interval (in-memory partials can be merged on read); a crash loses up to that interval of increments (15 s today) |
| 3 | **Pack trails per flight**: lossless delta + varint chunks; keep the points of open flights as rows and pack them when the flight closes (or append chunks) | positions = 96 % of the file at 38.2 B/point | 530 -> ~163 MB per 7 days (3.2x), ~108 MB with deflate (4.9x); the 30-day plateau 2.27 GB -> ~0.5-0.7 GB; fewer pages per flush (one chunk per flight); replay and Locate Me read 3-5x fewer pages | none if lossless; trail reads need a cheap decode step |
| 4 | **No second, row-per-sample copy of trail data**: `met_obs` (30 days, 82 B/row with its index, REAL columns for values with 3-4 significant digits) and `guess_samples` (37 B/row, 2 M rows) on a located station | ~420 MB + ~75 MB at steady state; ~1.3 M extra row inserts per day (plus ~0.2 M met_profile UPSERTs); every 10 min the guess-sample pruner also counts the table with a full scan (`SELECT stn, COUNT(*) FROM guess_samples GROUP BY stn`, ~75 MB at the cap) before its DELETE; ~8 % of the WAL frames | ~450 MB at steady state; ~8 % of the WAL frames | met_obs: quantise (lat/lon e5, wind / temperature x10 integers) and drop met_obs_t (rowid is already time order): ~3x smaller at the resolution of the sources. guess_samples: record only while the location is unknown or a Locate Me run is wanted, or derive the samples from positions (the documented fallback) |
| 5 | **Tier the long-lived station tables** the way `Maintain` already does for aggregators: daily histograms -> month buckets after N days; range_hour BLOBs varint-packed (as in stations.db) and WITHOUT ROWID | flights 1.13 MB/day for 730 days (~830 MB); histograms ~95 MB per year; range_hour 37 MB per year with 48 % free space in its pages | ~150-250 MB per year of operation | month buckets lose the per-day resolution of old days (the trade-off the aggregator already made); packing is lossless |
| 6 | **Cheap table counts** for `/api/db/stats`: sqlite_stat1, max(rowid) for rowid tables, or counters kept by the store | COUNT(*) of positions: 16.5 s with a cold page cache, 0.42 s warm (i9/NVMe); on a Pi after a reboot tens of seconds to minutes, because it reads the whole 0.5-2.3 GB table | the slowest endpoint becomes instant | approximate counts for the huge tables (label them as such) |
| 7 | **Replay reads at the frame step**, not every stored point | `/api/replay?hours=24` hits the 400 k point budget (truncated) after 1.2 s; it needs one point per aircraft per 144 s step but reads one per 5 s | the 24 h replay becomes complete instead of truncated, ~10-30x fewer rows read | none (it is a fix) |
| 8 | **Statistics overview queries**: keep per-day distinct-aircraft and flight counts in a rollup (stats_day already exists) instead of COUNT(DISTINCT) over flights; add an index on events(icao) | overview 634 ms (377 ms in one COUNT DISTINCT), growing with 730 days of flights; events by aircraft is a full scan | overview < 50 ms; aircraft page O(log n) | none |
| 9 | **Cold-cache reads and pragmas for the Pi**: every full walk of positions reads the whole table (530 MB at 7 days, 2.3 GB at the plateau) with 4 KB page reads - 16.5 s cold even on NVMe; options 3, 6 and 7 remove most such walks. A larger reader `mmap_size` on 64-bit would cut the per-page syscalls of warm reads (the 64 MiB map covers only the front of the file; the hot statistics tables are not at the front). Run `PRAGMA optimize` periodically, not only in Close. page_size 4096 matches ext4 and the SD card; temp_store=MEMORY is right | section 7: db/stats 16.5 s cold vs 0.42 s warm | first-request latency after a reboot; fewer syscalls | none |
| 10 | Other writers: the log volume is tiny; terrain 42 MB once; the tile cache is capped at 512 MB (a smaller default could suit a Pi) | section 9 | up to ~0.5 GB of disk on a Pi | none |

### 10.1 Quick experiments (scratch builds only)

Real time, each on a fresh copy of the 7-day database, demo 1,300, compared
over the same elapsed minutes 6-16 after the start (the baseline window
includes the read sweep, which reads but hardly writes):

| Run | All writes /h | WAL appends /h | Database-file writes /h | Checkpoints /h (fsyncs) | Commits /h | Largest WAL |
|---|---:|---:|---:|---:|---:|---:|
| baseline (rt-7d) | 702 MB | 448 MB | 254 MB | 93 (~186) | 1,913 | 6.3 MB |
| exp1: `wal_autocheckpoint(10000)` | 563 MB (-20 %) | 470 MB | **94 MB (-63 %)** | **12 (~24)** | 1,920 | 40 MB |
| exp2: exp1 + tracker flush 15 s -> 60 s + store FlushInterval 2 s -> 10 s | **274 MB (-61 %)** | **222 MB (-50 %)** | **51 MB (-80 %)** | **6 (~12)** | 528 | 38 MB |

The same 60-s tracker flush at data time (gen1trace repeated, 1 day): rows
written 13.19 M -> 7.24 M per day (-45 %): signal_hist 4.82 M -> 1.70 M,
heat 2.45 M -> 1.53 M, coverage 1.81 M -> 0.82 M, elev_hist 0.36 M ->
0.15 M; positions, flights and aircraft unchanged; the database the same
size (82.8 vs 81.8 MB). A 300-s setting gave exactly the same numbers: the
tracker flushes everything anyway when the minute rolls over, so going
beyond 60 s needs the increments decoupled from the minute rows.

Neither experiment changes what is stored. exp1 only moves when pages are
copied from the WAL into the database file. exp2 additionally lets the
statistics pages and trails in the database lag the live state by up to a
minute instead of 15 s, the rows of the small writers become visible after
up to 10 s instead of 2 s, and a crash would lose up to a minute of
increments instead of 15 s (the minute rows already behave that way).

## 11. Reproducing these numbers

The measurement tools are scratch programs outside the product. They were
built from a copy of the source tree; nothing in the repository was
changed. To repeat a measurement after a change, rebuild them against the
changed tree:

| Tool | What it does |
|---|---|
| `dbgen -dir D -days N -aircraft A -seed S [-cpuprofile F] [-end-ago 0]` | sim (`Advance`, virtual clock) -> `tracker` -> `store` at data time; RollupHours + Checkpoint per data hour, Prune per data day; writes `dbgen-report.json` (frames, flushes, rows, sizes and maintenance times per hour). 7 days at 250 aircraft take ~8 minutes on the i9 |
| `dbinspect -db COPY [-analyze]` | pragmas, DDL, per-object dbstat (bytes, payload, unused), rows, time span |
| `trailpack -db COPY [-chunk 256]` | lossless delta+varint (and deflate) size of the positions table |
| `walwatch -shm DB-shm -out CSV [-report 60s] [-every 20ms]` | polls the WAL index: commits (iChange), WAL frames, WAL resets, checkpoints, back-filled frames, per-commit frame histogram |
| `walpages -wal WAL-COPY -db DB` | maps WAL frames to the tables and indexes that own the pages (dbstat) |
| `sample.ps1` / `rates.ps1` | Win32_Process I/O counters, CPU, working set and data-dir sizes every 60 s / rates between two samples |
| `sweep.ps1` | times the read APIs with curl |
| store `zz_sqltrace.go` (`AIRVIEW_SQLTRACE=file`, `AIRVIEW_CPUPROFILE=file`) | database/sql driver wrapper: per-statement count, rows, total and max time, transactions; CPU and heap profile of a running station |
| `eqptrace -db COPY -trace JSON` | EXPLAIN QUERY PLAN of every traced statement: full scans, temp B-trees, index usage |

On the Pi itself, `/proc/<pid>/io` (`write_bytes`) replaces the I/O
counters, `/sys/block/mmcblk0/stat` or `iostat` shows what reaches the
card, and `strace -c -e trace=fsync,fdatasync` counts the syncs.
