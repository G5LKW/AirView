# dbparity: database parity harness

`dbparity` proves that a new AirView build loses no feature compared with an
old one. It is meant for storage and database efficiency work (smaller
files, fewer writes and fsyncs, different table layouts, retention tiers),
where every change must leave every page, export and statistic exactly as
it was.

It runs two real binaries side by side, each with its own scratch data
directory:

- **old**: the reference build. By default this is the released
  `dist/airview.exe`. The harness copies it into the work directory and
  only runs the copy.
- **new**: by default a build of the current tree (`go build ./cmd/airview`),
  or any binary passed with `-new`.

## One command

From the repository root:

```sh
go run ./tools/dbparity                      # new tree vs dist/airview.exe
go run ./tools/dbparity -new old             # self-test: old build vs itself (must pass)
go run ./tools/dbparity -new path/to/airview # any two binaries
```

On Linux (Raspberry Pi, aggregator host), pass the old binary explicitly,
for example `-old dist/airview-linux-arm64`.

Exit code 0 means every phase passed, 1 means a difference was found, and 2
means the harness itself failed. Each run writes the following into the
work directory (`-work`, default `$TMP/airview-dbparity`):

- `report-<time>.txt`: the summary shown at the end;
- `report-<time>.json`: every call, difference and table;
- `dbparity.log`: the progress log;
- `run/<phase>/{old,new}/data`: the data directories of the last run, kept
  for inspection, plus each instance's `*.console.log`.

The first run takes about 20 minutes. Most of that is building the fixture
in real time, which is cached for later runs (see `-refresh`), so a re-run
takes about 8 minutes.

## What it does

| Phase | What happens | What must hold |
|---|---|---|
| `fixture` | The **old** build records `-sessions` (3) sessions of `-session` (3 min) of simulated traffic. Between sessions it is stopped and every time value in its databases is moved back one day (see "History"). The result is an old-format database with about two days of history. It is cached per old-binary hash, seed, sessions, session length and retention. | (built once) |
| `migrate` | Old and new each start on their own copy of the fixture, with no input. The new build migrates the database: the DDL migrations at start, then its background data migrations (the harness waits for `/api/db/stats` `migration.state` = `done`, at most `-migwait`). After `-settle` (20 s) and the migration, every route below is called on both, with strict comparison. Both are then stopped and their databases compared row by row. | Same HTTP status and same JSON or text everywhere; every CSV table export byte-identical (row order included) unless a tolerance applies. The tolerances table lists the only exceptions. Every row up to the fixture cutoff is identical (logical rows for re-encoded tables). Every migration recorded in the old database is still recorded. `PRAGMA integrity_check` returns ok. |
| `crash` | The new build is started on a fresh copy of the fixture and killed (TerminateProcess or SIGKILL) `-kills` (5) times spread over its measured start-up time (at least 2 s: database open, DDL migrations, start-up maintenance), then `-kills` more times at random moments of its background data migration (the window measured in the migrate phase; design §5.4). It is then started normally, the migration completes, and the old build is started on its own copy for a strict comparison. | It becomes healthy, the data migration completes, the file passes `integrity_check`, keeps every old migration, every route answers as the old build (strict), and the rows equal the old build's rows. |
| `live` | Old and new each start on a copy of the fixture and receive **byte-identical** Beast input in real time for `-live` (4 min). Then the simulation stops and every route is called while the connections stay open. | Same answers within the live tolerances. Historic rows (older than one day before the cutoff) are identical. |

### Routes and exports compared

The route list is the station's own route table (`server.Routes()`, the
same table `docs/API.md` is generated from). It includes every `GET` route
except `NetOnly` (aggregator-only) routes and the few in `skipRoutes`
(`routes.go`), which are skipped with a stated reason: the endless SSE
streams, internet-only photos, the terrain PNG, account bootstrap. A new
`NoAuto` GET route is reported in the log rather than silently skipped.

Each route is called with its documented sample. Sample placeholders are
resolved from data both instances have:

- `{live}`: an aircraft with a position that both instances list;
- `{flight}` / `{seen}`: a stored flight with a trail that both instances
  list with the same ID, ICAO and start;
- the airport and route samples use `EDDF` and `DLH400`.

Every route that takes the common time-range parameters is also called with
`days=7` and with `from`/`to` covering the shifted sessions, which reaches
the historic part of the database.

These extra calls are made as well:

- every table of `/api/db/export/{file}` as CSV;
- the oldest flight as JSON, trace, GeoJSON, KML and CSV;
- `history/flights` and `history/aircraft` with `limit=500`;
- `/api/replay?hours=96`, `/api/reports.csv?days=7` and
  `/api/export/live.kml?minutes=60`;
- all stats events.

Together these cover the CSV, KML and GeoJSON exports, replay frames,
flight traces, history and the statistics pages.

### Comparison rules

**JSON** is compared semantically:

- Objects are compared key by key. A key present on only one side is a
  difference.
- Lists of objects are matched by the first identifying field that is
  unique on both sides (`id`, `icao`, `key`, `t`, `day`, and so on; see
  `idKeys`). Otherwise they are compared by position.
- Numbers compare exactly, or within 1e-9 relative for float formatting.
- Strings compare exactly. Sentences with numbers in them ("measured
  boresight 2°") compare word for word, with the numbers given the mode's
  number tolerance.
- `/api/replay` rows reference `aircraft[]` by index, so the index is
  replaced by the flight ID before comparison (see `normalize`).

**CSV, KML and other text** is compared as a multiset of lines, because
export order is not a feature. Lines are matched exactly first, then by
"same words, numbers within tolerance". Unix-time tokens (seconds or
milliseconds) may differ by up to 5 s.

**Non-200 answers**: both builds must return the same status code. A
feature that is off, for example terrain, answers the same way in both.

### Tolerances

Every field that is not compared exactly is listed in `tolerances`
(`compare.go`), with its reason. The groups below summarize them.

**Intended differences of the 1.1 storage (design §9)**

- `db-stats-shape`: `/api/db/stats` gains `profile` and `migration` objects and checkpoint counters in `stats`; file sizes, pages, WAL and the tile cache are ignored, the bookkeeping and new physical tables (`kv`, `schema_*`, `trail_*`, `met_sample`, `hist_day`, `flights_hour`) and the tables an idle instance writes to (`stats_minute`, `receiver_status`) are not compared. The counts of every other 1.0.0 table are compared exactly (`positions`, `met_obs` keep their names).
- `met-precision`: met observations are stored as scaled integers (lat/lon 1e-5 deg, wind direction 0.1 deg, wind 0.1 kt, OAT 0.01 C, all finer than the BDS sources). `/api/met/*` numbers, the `met_obs.csv` export and the logical `met_obs` rows of the row check (`rowAbs` in `rowcheck.go`: lat, lon, wind_dir, wind_kt, oat_c) may differ by up to 0.06.
- `trail-retention`: 1.0.0 prunes the trail of a flight that ended before the trail retention only when the flight row counts positions, so a point that arrived after the flight row's last update is never deleted; the 1.1 trail pruner deletes every point past the retention. The row check does not compare the logical `positions` rows of flights that ended before the old build's own prune watermark (`kv` `store.positions_pruned`), i.e. trails the old build has already pruned except for such leaked points.
- `replay-untruncated`: when the old `/api/replay` answer is `truncated` (point budget exhausted) and the new one is complete, the new frames only have to cover the old frame times (the new build counts the points it uses, design §3.14). Replays that were not truncated are compared strictly.
- `visible-after`: in the live phase the harness waits after the feed stops for the longer of one group-commit interval of the new build (`profile.flushIntervalMs` + 2 s; the small writers' rows - events, met, guess samples - are committed every 10 s / 30 s instead of every 2 s) and 80 s (both builds write the flights / aircraft rows of the aircraft in view every 60 s, not aligned between the two processes, and both log feed_down / feed_up when the idle connection hits the 60-s "no data" timeout; without the wait the self-test was flaky on exactly those rows and events). Pages that merge the tracker's in-memory overlay need no wait. Table exports (and the export list) write the tracker's increments and the met collector's open hour before they read, so they need no wait either; the `/api/db/stats` count of `met_profile` (the open hour is merged when the hour closes) is not compared in the live phase.

**Process and host facts (ignored)**

- `/api/system`;
- `/api/db/stats`: see `db-stats-shape` above; `dbBytes` / `walBytes` (also in `/api/status`) are ignored everywhere;
- uptime and start times;
- data directory paths (`*dir`, `*Dir`, `locked[].origin`);
- ports and URLs: each instance has its own;
- `*version*`: the new build may carry a newer version string;
- `*fitted`: when the forecast model was fitted (within 5 min: the fit is kept in the database, so after the crash phase's kills the new build reports the fit of an earlier start);
- session and CSRF tokens;
- CPU timings (`*Nanos`, `*Micros`);
- the Open-Meteo quota counter (`*.usage.*`).

**Wall-clock fields**

- Ignored: `now`, `*generated*`, `*computed*`, `*serverTime*`,
  `*cachedAt*`, `*elapsed*`, `*durationMs*`, `*tookMs*`. These hold the
  time of the request or computation.
- Within 5000: `*lastMs`, `*finishedMs`. These are last-update or
  job-finish times a few seconds apart between the two processes. Other
  `*Ms` fields are durations and are compared exactly.
- Within 30: `*ago*`, `*age`, `*seen`. These are ages relative to the
  request time, in seconds.
- Within 2: `/api/rf/*` `*seconds` and `windowSec`. The RF statistics
  window is the time since the process started, capped at about one hour;
  two processes started a moment apart report windows a second apart.

**Live mode only**

- Every number may differ by 10 % or 3, whichever is larger. A frame at a
  minute boundary can be counted in the neighbouring minute, an aircraft
  can time out a moment earlier in one instance, and the two snapshots are
  milliseconds apart.
- Keyed lists and text lines may differ in 10 % of their entries or 3
  entries, whichever is larger.
- Rates may differ by 25 %, because they are computed over sliding
  windows that end at the request.
- `/api/timing/latency*` histograms, waterfall and processing delays are
  ignored, because they measure each process's own scheduling.
- `/api/timing/*` receiver-clock drift is ignored, and its offsets may
  differ by 50 % or 5. Both are fitted from the host arrival times of a
  few minutes of frames.
- Trail points (`trails.*` and `trail*` of the live and history routes,
  and `points` of a flight trace) may differ by 25 % or 10. They are taken
  every `trailIntervalSec` from the arrival timing, so the two builds can
  keep neighbouring samples, whose RSSI differs by several dB.
- `/api/rf/*` `expected[]` counts may differ by 25 %. They are derived from
  the receive-time spacing of the squitters.
- In the "longest tracked flight" record, its event, notice and alert,
  the aircraft identity is removed (see `normalize`). Every aircraft that
  was in view since the feed started has the same tracked time, so which
  one holds the record is a tie decided by processing order. The time the
  record was set is removed as well (each process re-checks the tie at its
  own moment). The value is still compared.
- `/api/timing/*` per-feed arrival-delay histograms (`hist[]`), the worst stage delay (`maxMs`) and the findings / issues sentences that quote the fitted drift are ignored: host scheduler timing, which differs on a loaded host.
- `/api/timing/*` `status` (ok / warn) is ignored for the same reason; live `/api/stats/events` and `/api/stats/overview` events are matched without their row `id` (an event one process writes a moment earlier shifts the IDs of all later ones) and ordered by time, then content (several records set in the same second are written in processing order); their `feed_down` / `feed_up` events are left out (the harness keeps the idle connections open after the simulation stops, so both feeds hit the 60-s "no data" timeout during the wait, and whether a build has logged the pair by the comparison is timing, seen both ways between two 1.0.0 builds).
- `/api/db/stats` `tables.met_bias`: the met collector saves its heading
  corrections every 5 min after its own start, which is when the live
  comparison runs; one process may have saved them a moment before the
  other.
- `/api/*/daynight` `byHour[]` / `phases[]`: where the old build answers
  `null` for the open hour, any new value is accepted (`OldNull`). 1.0.0
  shows the open hour after its 5-min range_hour partial write, which the
  live comparison may come before; 1.1 merges the open hour from the
  tracker's overlay at once. Values present on both sides are compared.
- `/api/notices`: the publication time of a notice is each process's own clock (ms apart), so notices are matched by content.
- Strict mode, used by the migrate phase, has none of these.

The astronomical night side (`/api/astro/terminator.geojson`) is computed for the request time; its coordinates may differ by 0.05 deg in every mode (the two requests can be seconds apart).

**Rows**

- Not compared: `schema_migrations`, which is checked separately (every old
  migration must still be recorded), and `kv`, which holds maintenance
  watermarks and last-run times.
- The column `alerts_rule.updated` is not compared, because it is
  re-seeded at start.

### Row-level check (`rowcheck.go`)

For every table of every `*.db`, the rows up to the fixture cutoff are
compared. The cutoff uses the table's time column, with the unit detected
from its value range.

- **Same columns in old and new**: every row and every column is compared.
- **Re-encoded by the new build** (positions, met_obs, heat, sealed
  histograms ...): when the store package of the build under test registers
  a logical table for it (`store.RegisterLogicalTable`, the same registry
  that serves `/api/db/export/<table>.csv`), the harness reads the new
  database through `store.ScanTableDB` and compares those rows, as a sorted
  multiset up to the cutoff, with the 1.0.0 rows of the old database. This
  takes precedence over a same-named physical table (positions stays as an
  empty table after its migration).
- **Changed layout without a logical table**: the new build must provide a
  logical extract in `logicalExtracts`, a pair of queries against the old
  and the new schema that return the same columns. `positions`, `flights`,
  `stats_hour` and `stats_minute` have one, which today is the identity. A table that
  changed or disappeared without either fails the check.

So a change that re-encodes a table must extend the harness in the same
change, by adding its extract.

## History ("N simulated hours")

AirView stamps everything with the wall clock (`RawFrame.Recv = ntp.Now()`),
and the 1.0.0 build has no clock override, so hours of history cannot be
fed in faster than real time. The fixture is built in sessions instead.
After each session except the last, `shiftDays` (`history.go`) moves every
time value back by one whole day:

- Columns: every INTEGER column with a time-like name (`t`, `day`, `hour`,
  `start`, `end`, `first_seen`, `last_t`, `*_at`, and so on).
- Units: Unix s, ms or µs, or a day, hour or minute number. The unit is
  recognised by the value lying in that unit's window (20 years back to 30
  days ahead), so counters and bins are never touched.
- `kv`: watermark values.

Whole days keep day, hour and minute keys aligned. Tables are rewritten
through a temporary copy, so moving a key column never collides.

All rows were written by the old build itself. When the builds under test
start on the fixture, they run the minute → hour → day rollups, the month
buckets and the retention tiers on genuine old-format data.

`-retention short` (the default) configures trails for 2 days and minute
statistics for 1 day, so the pruning tiers run inside the fixture's
history. Use `-retention default` for the shipped defaults.

## Deterministic input

`feed.go` runs the built-in simulator (`internal/sim`) with a fixed `-seed`,
`-aircraft` (60) and constant density, live on the wall clock. It serves
the simulation as Beast TCP streams:

- **Fixture**: one stream per antenna (vertical on channel 0, yagi on
  channel 1), configured as two feeds with `antennaId`, so the per-antenna
  statistics exist as in demo mode.
- **Live phase**: one combined stream (each transmission once, from the
  channel that heard it best). Two TCP connections are not read in a
  deterministic order, so with split feeds the stored RSSI of a
  transmission would depend on which copy arrived first.

The simulation starts only when every instance has connected. Every chunk
is queued to all connections in the same step. A late reconnect or a
stuck reader is reported as diverged input, which fails the phase.

Each instance runs with an operator file (`config.go`) that does the
following:

- binds to 127.0.0.1 on its own port (`-port` 18911 for old, port+1 for
  new);
- sets the receiver position to the simulator's station;
- turns off everything that needs the internet: weather, terrain download,
  NTP, the tile cache, routes and photos, and self-update.

The harness clears `AIRVIEW_*` and `GOMEMLIMIT` from the instances'
environment.

## Stopping instances

- **Linux**: SIGTERM, a graceful stop.
- **Windows**: `airview.exe` is a GUI-subsystem program that registers its
  console handler before it attaches to a console. A console Ctrl+C or
  Ctrl+Break therefore terminates it instead of stopping it gracefully,
  and there is no SIGTERM. The harness waits 20 s instead (one tracker
  flush period plus the writer commit) and then terminates the process.
  Nothing compared depends on the shutdown path: answers are taken before
  the stop, and rows only up to the cutoff.

The harness only ever stops processes it started, by their PID.

## Options

| Flag | Default | |
|---|---|---|
| `-old` | `dist/airview(.exe)` | reference binary (copied, never run in place) |
| `-new` | `""` | `""` = build `./cmd/airview`; `old` = self-test; or a binary |
| `-work` | `$TMP/airview-dbparity` | work directory |
| `-port` | 18911 | old instance's HTTP port; new = port+1 (or a free port) |
| `-seed`, `-aircraft` | 1, 60 | simulator |
| `-sessions`, `-session` | 3, 3m | fixture: sessions one day apart, length of each |
| `-live` | 4m | live phase length |
| `-settle` | 20s | wait after start before comparing (start-up maintenance) |
| `-retention` | short | `short` or `default` |
| `-phases` | `fixture,migrate,crash,live` | subset to run (the fixture is reused from cache) |
| `-refresh` | false | rebuild the fixture |
| `-kills` | 5 | crash phase kills during the start-up, and again during the data migration |
| `-migwait` | 15m | longest wait for the new build's data migrations |

`go test ./tools/dbparity` checks the harness itself: the history shift,
the comparison rules and the route coverage.

`go run ./tools/dbparity schema <data dir>` lists every table of a data
directory with row counts and integer column ranges, which helps when
adding a logical extract.

## When a check fails

1. Read the `x` lines of the report. Each shows the route, the JSON path
   and the old and new values. Rows show the first differing row and
   column.
2. A real feature loss (missing key, empty list, changed value, different
   status) must be fixed in the new build.
3. A field that legitimately cannot be equal (a new timing field, for
   example) gets a tolerance entry with its reason. Keep that list short:
   every entry is a field nobody checks any more.
4. A re-encoded table gets a logical table in the store (or an extract here).
5. Run `-new old` after changing the harness: the self-test must still
   pass.
