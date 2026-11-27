# AirView — architecture & build contract

AirView is an advanced ADS-B / Mode S statistics and mapping station.
One Go codebase produces:

- `airview.exe` — Windows 11 desktop app: a WebView2 window + embedded HTTP server (standalone EXE).
- `airview` (linux/arm64, linux/armv7) — headless server for Raspberry Pi 4, same web UI in a browser.

Primary receiver: **Radarcape EVO 2CH** (GPS-timestamped Beast binary over TCP) with a yagi and a
vertical antenna. Other inputs (dump1090/readsb Beast, AVR, SBS, aircraft.json) are also supported.

## Hard rules

- **Pure Go, CGO_ENABLED=0.** No C toolchain exists. SQLite = `modernc.org/sqlite` (driver name `"sqlite"`).
  Windows window = `github.com/jchv/go-webview2` (only imported from `cmd/airview/*_windows.go`).
- Dependencies are already in `go.mod`. **Do not run `go get` or `go mod tidy`** and do not add
  third-party modules; use the standard library. (Integration tidies at the end.)
- Everything except the GUI file must compile for `windows/amd64`, `linux/arm64`, `linux/arm`.
  No OS-specific code outside `_windows.go` / `_linux.go` / build-tagged files.
- `internal/model` is the shared contract. **Do not edit it** unless you own it; report needed
  changes in your result instead.
- Stay inside the directories you own. Build/test only your own packages
  (`go build ./internal/<pkg>/...`, `go vet ./internal/<pkg>/...`, `go test ./internal/<pkg>/...`) —
  other packages are being written concurrently and may be mid-edit.
- Efficiency matters: a Radarcape can deliver 1,000–3,000 frames/s, and the Pi 4 target has an SD card.
  No per-frame allocations you can avoid, no per-frame DB writes, no unbounded maps/slices.
- Every long-running component takes a `context.Context` and stops cleanly when it is cancelled.
- Logging: `log/slog` default logger. No `fmt.Print` in libraries.
- Units & timestamps in APIs: see the header comment of `internal/model/model.go`.
- Run `gofmt -w` on everything you write.

## Package map (owner = the one agent allowed to write there)

| Package | Purpose |
|---|---|
| `internal/model` | Shared types: `Config`, `RawFrame`, `DecodedUpdate`, `Aircraft` DTO, `Status`, interfaces `FrameSink`, `Clock`. |
| `internal/modes` | Mode S / ADS-B decoder: CRC, DF0/4/5/11/16/17/18/20/21, all ADS-B type codes, CPR, Comm-B BDS inference. Pure functions. |
| `internal/feed` | Input clients producing `model.RawFrame` / `model.DecodedUpdate` into a `model.FrameSink`: Beast, AVR, SBS, JSON polling; reconnect/backoff; per-feed status. |
| `internal/hub` | The AirView "data port": authenticated TCP server that relays the live frame stream + station metadata to remote AirView desktop clients, and the matching client (used by feed type `hub`). |
| `internal/geo` | Great-circle maths, coordinate parsing/formatting (decimal, DMS, Maidenhead). |
| `internal/astro` | Sun position, sunrise/sunset/twilight, day/night classification, terminator polygon. |
| `internal/radio` | 1090 MHz propagation: radio horizon, FSPL, link budget, antenna pattern models, refractivity/k-factor/ducting. |
| `internal/store` | SQLite: open/pragmas/migrations, single-writer batching, schema below, retention, typed write API. |
| `internal/config` | Load/save/validate `config.json`, data-dir resolution, factory reset. |
| `internal/weather` | Open-Meteo client (current, hourly forecast, pressure-level profile, history backfill), RainViewer radar frames. |
| `internal/ntp` | SNTP client + periodic sampler implementing `model.Clock`. |
| `internal/sim` | Traffic simulator: generates valid Mode S/ADS-B frames and serves them as a Radarcape-style Beast TCP stream (demo mode + end-to-end tests). |
| `internal/refdata` | ICAO address → country, registration-from-hex algorithms, airline/operator from callsign, emitter categories, squawk meanings, optional aircraft database import. |
| `internal/stat` | Numerical toolbox (stdlib only): descriptive/circular statistics, correlation, OLS/ridge/logistic regression, Holt-Winters, Kalman, Nelder-Mead, backtesting metrics. |
| `internal/tracker` | Live aircraft table: decode → state, CPR pairing, trails, geometry vs receiver, per-antenna stats accumulation, flight sessions; writes to `store`. |
| `internal/analytics` | Read-side analysis: coverage/polar plots, antenna pattern estimation, RSSI-vs-distance, heatmaps, records, weather correlation, receiver-location guesser. |
| `internal/met` | Aircraft-derived meteorology (wind/temperature aloft from Mode S EHS/MRAR) and vertical profiles. |
| `internal/predict` | Forecasting: traffic levels, reception conditions, local weather tendency; backtesting. |
| `internal/trajectory` | IMM tracking filter per aircraft (fed by the tracker), coasting / predicted positions, predicted paths, intent (holding, approach, closest approach to the station), proximity pairs, self-scored prediction skill. In memory only. |
| `internal/terrain` | Elevation tiles (AWS Terrain Tiles, disk cache `<data>/terrain`), radio line of sight / diffraction, horizon panorama, theoretical coverage per altitude, coverage floor, per-aircraft visibility, site advice, HTTP handlers. |
| `internal/rfstat` | Online RF / protocol statistics from every frame: signal / noise floor / overload, detection probability, update intervals, channel occupancy and garble, interrogator census, squitter timing, fleet equipage, GNSS interference, RF health. Hourly rows `rfstat_hour`, `rfstat_iid_hour`. |
| `internal/navdata` | OurAirports airports / runways / navaids / countries and VRS standing-data routes (imported on request into `navdata_*` tables), airport region index, approach matching, airport association, opt-in online route and photo look-ups (cached, paced). |
| `internal/latency` | Latency chain from GPS time stamps: per-stage statistics (propagation, receiver → host, processing, T=1 transponder latency, T=0 position-age change, totals), histograms, per-aircraft rows and stage waterfall, per-feed receiver-delay clock filter and NTP / GPS consistency check, plain-language findings. 15 min window in memory, per-feed minutes in `latency_minute` (24 h restored at start). See `internal/latency/WIRING.md`. |
| `internal/alerts` | Alert rules engine (AND/OR/NOT trees, geofences, watch-lists, system events), cooldowns, highlights, webhooks; tables `alerts_rule`, `alerts_watch`. |
| `internal/engine` | Wires everything together and owns lifecycles; applies config changes live. |
| `internal/server` | HTTP API, SSE live stream, embedded UI, tile cache proxy, admin auth. |
| `web` | `package web` — `//go:embed static` exposing `web.FS`. All front-end assets (no build step, ES modules, vendored libs). |
| `cmd/airview` | `main`: flags, data dir, engine + server start, WebView2 window on Windows, headless elsewhere. |
| `deploy/` | Raspberry Pi install script + systemd unit, build scripts. |

Dependency direction (no cycles): `model` ← `geo`/`astro`/`radio`/`modes`/`refdata` ← `store`/`config`/`feed`/`hub`/`weather`/`ntp`/`sim`
← `trajectory`/`rfstat`/`terrain`/`navdata`/`alerts` ← `tracker`/`analytics`/`met`/`predict` ← `engine` ← `server` ← `cmd/airview`.
(`trajectory` imports only `model`; `terrain` `model`, `geo`, `radio`; `rfstat` `geo`, `modes`, `store`; `navdata` and
`alerts` `model`, `geo`, `store`. The tracker imports `trajectory` and `rfstat` for its frame-path hooks.)

## Extensions: prediction, terrain, RF statistics, navigation data, alerts

Wiring (engine `ext.go`, tracker `extend.go`; each library's `WIRING.md` has the details):

- **Frame path (tracker, under its lock, allocation-free):** every Mode S / Mode A/C frame that is not a duplicate or
  an MLAT result goes to `rfstat.Stats.Observe` (also CRC failures and rejected frames); every applied position,
  velocity, altitude (altitude-only replies throttled to 1/s per aircraft), vertical rate and Comm-B register goes to
  `trajectory.Manager.Observe` (time = GPS timestamp when available, wind from the met collector). Benchmark
  `BenchmarkHandleFrame` 119 ns/frame without, `BenchmarkHandleFrameFull` ~590 ns/frame with both (desktop i9), 0 allocs.
- **Coasting:** from `model.PredictAfterSec` (4 s) after the last real position until `config.prediction.coastSec`
  (default 60, max 120) the live picture shows the filter's predicted position with `predicted:true` (plus `coastS`,
  `predSigmaM`, `lastRealLat/Lon`), and keeps such an aircraft listed even when it is otherwise silent. Predicted
  positions exist only in the snapshot DTO: never in the aircraft state, trails, the database, statistics, coverage,
  heat, range or records. `counts.withPos` / `maxRangeKm` count real positions only (`counts.predicted` separately).
- **1 Hz enrichment** (`Tracker.SetEnricher`, run once per new snapshot without the tracker lock; an engine loop
  builds the snapshot every second even without clients): terrain visibility (`terrainVisible`, `losMarginDeg`),
  closest approach to the station (`cpaKm`, `cpaInS`, recomputed per aircraft every 5 s, at most 80 per second),
  `route` from the offline standing data (look-ups queued to a background worker, cached 1 h / 10 min negative),
  then the alert rules (`alerts.Evaluate`) and their map highlights (`highlight`).
- **Alerts:** system events from the 5 s status check (`alerts.CheckStatus`: feed down/up, NTP, range collapse),
  new records (tracker notices of kind `record`) and new airframes (first sighting ever, checked against the
  `aircraft` table in the tracker's background look-up). Fired alerts go out as SSE `event: alert`; user-rule and
  system alerts also as `notice` (kind `alert`). `config.alerts.webhooks=false` stops all webhook POSTs.
- **Terrain:** analysed in the background when the (effective) receiver is located and `terrain.autoPrefetch` is on,
  again after a change of position, antenna heights, `kFactor`, `radiusKm` or `zoom` (no-op when unchanged). Offline
  engines (tests) use a synthetic terrain source instead of downloading tiles.
- **Navigation data:** nothing is downloaded automatically; `POST /api/navdata/import` runs the import bound to the
  engine runtime. The airport region (800 km) is reloaded at start, after a receiver move and after each import;
  it feeds approach detection in the filter and the departure / arrival guesses of stored flights. The trace
  page guesses on read; the engine also stores the guess in `flights.dep` / `flights.arr` (core migration 2)
  once a flight is closed (35 min after its last message), newest first, in small paced batches that also
  back-fill older flights (`engine/airports.go`; flights that found no airport are re-queued after an import).
  `/api/reports` filters and sorts on them. Online routes (adsbdb) and photos (planespotters) are opt-in and only on request.
- **RF statistics:** one `rfstat.Stats` per antenna set / site (replaced on change), hourly summaries persisted.
- **Latency chain:** the tracker hands every handled, non-duplicate frame to `latency.Analyzer.Observe` (~210 ns,
  0 allocs, own mutex) after it is fully processed, with the position decoded from that frame; MLAT results are
  counted and excluded. The analyzer uses the NTP sampler as its clock (same correction as the tracker's per-feed
  `sampleLatency`, which however measures to the end of the frame, so it reads ~0.06-0.12 ms higher) and the
  effective station position (`Engine.Station()`: the located receiver, else the receiver's own GPS position). The engine drains per-feed minutes into `latency_minute` once a minute
  (minute retention) and seeds the last 24 h at start. Endpoints: `GET /api/timing/latency` (`chain`,
  `histograms`), `/api/timing/latency/aircraft[/{icao}]`, `/api/timing/latency/feeds/{id}`, `/api/timing/ntpcheck`;
  `POST /api/timing/ntp/sample?server=` tests any NTP server once without saving it.
- **Demo mode:** the demo feed drops the airborne position squitters of about a quarter of the aircraft for 15-45 s
  every four minutes (`engine/dropout.go`) so coasting is visible; the built-in military rule fires on the simulated
  military traffic.
- **History replay / reports:** `/api/replay` reads stored positions through the flights overlapping the window
  (`flights("end")` index + the positions primary key) - no index on `positions.t` was added (it would cost about
  as much as the positions table's key itself on an SD card for a query that the flight index already bounds).

Configuration sections (all optional in `config.json`; missing sections take the defaults, `PUT /api/config` keeps
sections that are not sent): `prediction {enabled, coastSec, pathSec, showPaths}`, `terrain {enabled, radiusKm, zoom,
kFactor, autoPrefetch}`, `navData {airportsOverlay, routesOnline, photos, photoContact}`, `alerts {enabled, sound,
webhooks}`, `rfStat {enabled}`. None holds a secret (`navData.photoContact` is the contact the photo service requires
in the User-Agent).

## Runtime modes (`Config.Mode`)

- `standalone` — feeds → engine → DB → UI, all local.
- `remote-data` — the desktop app logs in to an AirView host's **data port** (`internal/hub`, default TCP 30154),
  receives the host's live frame stream and station settings, and runs its own local engine/DB/UI on that data.
- `remote-web` — the desktop window shows the host's hosted website (`http://host:port/`) inside a thin local shell page
  (`/remote.html`) that keeps a "disconnect / change connection" bar.

The Pi build always behaves as `standalone` (and can enable the hub for desktop clients).

## Command line (`cmd/airview`)

`-data <dir>` data directory (default: `data` next to the executable; falls back to the OS user-config dir if not writable) ·
`-port <n>` override HTTP port · `-headless` no window (Windows) · `-demo` force the simulator feed ·
`-open` open the default browser (headless) · `-version` ·
`-config <file>` operator config file (JSONC; default `$AIRVIEW_CONFIG`, else `airview.jsonc` in the data directory; an explicitly named file must be valid or a headless start fails) ·
`-set path=value` override one setting (repeatable). `-data` defaults to `$AIRVIEW_DATA`. Precedence: defaults < `config.json` <
operator file < `AIRVIEW_*` environment < `-set`; overridden settings are never written to `config.json` (docs/CONFIG.md).
`airview config check|print|example|env` validates / prints / documents the configuration without starting the app. `airview admin init|reset [-data dir] [-user name]` creates the first aggregator admin (password + authenticator, prints recovery codes) or, as the break-glass path, replaces an admin's password and authenticator; it writes `<data>/stations.db` and may run beside a live server.

Subcommands (the first argument; anything starting with `-` is the classic flag form above):

| Command | What it does |
|---|---|
| `airview serve [options]` | run headless (servers, systemd, Docker); same options as the flag form. Unit: `ExecStart=/opt/airview/airview serve` |
| `airview config check\|print\|example\|env` | validate / show / generate the configuration |
| `airview admin init\|reset` | aggregator admin bootstrap / break-glass reset |
| `airview backup [-data dir] [-o file.zip]` | consistent online backup (VACUUM INTO of airview.db and stations.db, config.json, an `airview.jsonc` inside the data directory, `uplink/` without the spool, `avf-tls/`), `MANIFEST.json` with SHA-256 |
| `airview restore [-data dir] [-force] file.zip` | refuses while AirView runs on the directory, verifies every checksum first, moves replaced files to `<data>/pre-restore-<UTC time>/` |
| `airview db snapshot [-data dir] -o outdir` | consistent copies of every `*.db` (VACUUM INTO) for external backup scripts (`deploy/server/backup.sh --online`) |
| `airview db profile [-data dir] [-config file] [-set p=v] [-json]` | the storage profile each database runs with (`storage.profile`, why `auto` chose it, values after `storage.tuning.*`); changes nothing |
| `airview db migrate [-data dir] --status \| --backup \| --now` | storage-layout migration: progress (from the running server's `/api/db/stats`, else read-only from the file), a backup of every database to `<data>/backup/` (safe while running), or the pending migrations run to completion (server stopped) |
| `airview db vacuum [-data dir] [-config file]` | VACUUM of every database with the server stopped (needs free space of about the file size); returns the pages a migration freed in a file created without incremental auto-vacuum |
| `airview version [-json]`, `airview help` | version / usage |

`-listen-host <addr>` (or `$AIRVIEW_LISTEN_HOST`, else the setting `aggregator.listenAddr`) is the bind address of the aggregator's feeder listeners (AVF, Beast, mlat-client; default every interface). SIGINT/SIGTERM (Windows: Ctrl+C, console close, logoff) shut down gracefully within 90 s: HTTP, feeds, then every tracker stream held in memory (trail points, day statistics, heat increments, partial range rows) is written, then the store commits its queue and closes (the Pi unit allows `TimeoutStopSec=120`). Logging: `airview.log` gets `process.logLevel`; the stderr copy gets `process.journalLevel` (default `warn` when systemd's `JOURNAL_STREAM` is set, so an SD card does not store every line twice).

Deployment kits: `deploy/pi/` (Raspberry Pi station), `deploy/server/` (Linux server / VPS aggregator: install, upgrade, backup, restore, hardened systemd unit, reverse-proxy examples; guide in `deploy/server/README.md`), `Dockerfile` + `docker-compose.example.yml` (health check `/healthz`), `deploy/feeder/pi/install.sh` (airview-feeder on a Pi), `deploy/feeder/windows` and `deploy/installer` (Windows installers `AirView-Setup-<v>.exe`, `AirView-Feeder-Setup-<v>.exe`), `cmd/airview-piimage` (Pi SD-card images), `deploy/docker` (release Docker context), `deploy/relkit` (bundle packing and the final artifact check), `deploy/build.ps1 -OutDir/-Version/-SkipFeeder/-PiImages/-SignKey` (one run builds and verifies every deliverable: `airview-pi-<v>.tar.gz` + `airview-pi-install.sh`, `airview-feeder-pi-<v>.tar.gz` + `airview-feeder-install.sh`, server kits + `airview-server-install.sh`, `airview-docker-<v>.tar.gz`, `SHA256SUMS`; full list in its header). Operator documentation: `docs/aggregator/README.md`.

On linux/arm and linux/arm64 (Raspberry Pi) `main` sets a soft Go memory limit of 384 MiB
(`debug.SetMemoryLimit`) unless `GOMEMLIMIT` is set (in aggregator mode the limit is lifted again, or set to
85 % of the cgroup `memory.max` when the process runs under one); after the big background jobs (navigation data import,
terrain analysis, aircraft database import) the engine / navdata call `debug.FreeOSMemory` so the resident set
shrinks back after the peak.

HTTP port: default **80**. If the configured port cannot be bound, fall back to 8080, 8081…8089, then an ephemeral
port, log it, and report the real port in `/api/status.httpPort`.

## Core HTTP API (v1 — fixed now; the UI shell is built against it)

All JSON, UTF-8. Errors: HTTP status + `{"error":"message"}`.

| Method & path | Result |
|---|---|
| `GET /api/status` | `model.Status` |
| `GET /api/config` | `model.Config` (secrets blanked: `hub.password`, `remote.password`, `http.adminPasswordHash` → `""`/`"set"`) |
| `PUT /api/config` | body `model.Config` → validated, saved, applied live → returns the stored `model.Config` |
| `POST /api/factory-reset` | body `{"confirm":"RESET"}` → wipes DB + config back to `model.DefaultConfig()`, restarts engine |
| `GET /api/aircraft` | `model.LiveSnapshot` |
| `GET /api/aircraft/{icao}` | `{ "aircraft": model.Aircraft \| null, "trail": [TrailPoint…], "info": {...refdata}, "flights": [...] }` |
| `GET /api/trails?minutes=N` | `{ "now": ms, "trails": { "<ICAO>": [[tMs,lat,lon,altFt,gsKt,rssi,src],…] } }` (live in-memory trails) |
| `GET /api/events` | Server-Sent Events: `event: aircraft` (`model.LiveSnapshot`, 1 Hz), `event: status` (`model.Status`, every 5 s), `event: notice` (`{level,text,t}`) |
| `GET /tilecache/{host}/{path…}` | caching proxy for whitelisted free map hosts (only when `status.tileCache`) |

`TrailPoint` JSON form is the compact array `[tMs, lat, lon, altFt|null, gsKt|null, rssi|null, src]`.

**Serving the UI:** mount `/api/…` and `/tilecache/…` first, then `mux.Handle("/", web.Handler())` (it sets JavaScript
MIME types — MapLibre 6 is ESM-only and browsers refuse module scripts served as `text/plain`). Never send
`X-Frame-Options` or a `frame-ancestors` CSP: `remote.html` iframes another AirView host.

**Tile cache whitelist** (exact hosts; reported in `status.tileCacheHosts`): `tiles.openfreemap.org`,
`tile.openstreetmap.org`, `tilecache.rainviewer.com`. Paths may contain `%20`; pass the query string and
`Content-Type` through; send the identifying User-Agent `AirView/<version>`; honour upstream cache headers
(min 1 day for tiles/glyphs/sprites); cap the on-disk cache size.

**Admin auth:** requests from loopback are always admin. If `http.adminPasswordHash` is set, non-loopback clients must
`POST /api/login {"password":"…"}` → `Set-Cookie: airview_session=<random>; HttpOnly; SameSite=Lax` before any
mutating endpoint (`PUT /api/config`, `POST /api/factory-reset`, `/api/db/*` maintenance, aircraft-DB import…);
otherwise those answer `401 {"error":"admin password required"}`. `POST /api/logout` clears it.
`status.adminLocked` tells the UI whether this client still has to log in. Login attempts are throttled per IP.

**Secrets in `PUT /api/config`** (implemented by `config.ApplyUpdate`): `""` or `"set"` = keep the stored secret,
`"clear"` = remove it, `"plain:<password>"` in `http.adminPasswordHash` = set a new admin password (hashed
server-side); `hub.password` / `remote.password` take the new plain value directly.

**Antennas vs. channels:** no Radarcape output carries a per-frame antenna/channel flag (the EVO 2CH is antenna
diversity into one stream), so `RawFrame.Channel` is `-1` for real feeds. Per-antenna statistics are keyed by
`Feed.AntennaID` (when the user runs one feed per antenna/receiver) and always also under `ant=''` (combined).
The antenna analysis must therefore *infer* each antenna's contribution from the combined data (measured azimuth
gain pattern vs. the modelled yagi + vertical patterns).

Further endpoints (`/api/weather/radar` = RainViewer frame index, `/api/stats/*`, `/api/antenna/*`, `/api/weather/*`, `/api/forecast/*`, `/api/timing/*`,
`/api/guess`, `/api/history/*`, `/api/db/*`, `/api/prediction/*`, `/api/terrain/*`, `/api/rf/*`, `/api/navdata/*`,
`/api/alerts/*`, `/api/replay`, `/api/export/*`, `/api/reports`) are defined by the server and documented in `docs/API.md`.
The SSE stream additionally carries `event: alert` (`alerts.Alert`: besides `text`, the numbers `altFt`, `distKm`,
`gsKt`, `vrFpm`, `trackDeg`, `brgDeg`, `rssi`, and for system events `value` / `refValue` with their `unit`, so the UI
formats them in the viewer's units). The stream is gzip-encoded (flushed per event) when the client accepts gzip;
alert / notice events have their own per-client queue, so a burst of alerts neither displaces the aircraft picture
nor disconnects the client.

## Database (SQLite, WAL) — `internal/store`

One file `airview.db` in the data dir (aggregator: plus `stations.db`). One writer goroutine (group commit every
10 s / 30 s / 5 s by profile), a separate read pool, and a checkpointer goroutine. WAL with `synchronous=NORMAL`
on every profile. Times are Unix **seconds** in the DB unless noted.

Storage layout 1.1 (design and measurements: `docs/perf/DB-EFFICIENCY-DESIGN.md`, `docs/perf/DB-RESULTS.md`):

- **Profiles.** `store.ResolveProfile(storage.profile, kind, dataDir)` picks `standard`, `low-wear` (data on an
  SD/MMC card, or a Linux ARM board with <= 1 GiB) or `server` (aggregator); the engine applies
  `storage.tuning.*` (`engine.StorageProfile`) and passes the result as `store.Options.Profile` to `airview.db`
  and `stations.db`, and the cadences (trail / day increments / heat / range partial) to the tracker. The
  profile sets group commit, checkpoint policy, caches, read pool, mmap, the tile-cache default and the
  migration pace (table in docs/CONFIG.md).
- **Tracker streams.** Trail points, day increments (coverage, signal, elevation), heat increments and
  `range_hour` partial rows have independent flush clocks; the minute roll writes only the minute rows. What
  is not yet written is exposed by `Tracker.PendingIncs()` and merged by analytics into every answer that
  covers "now" (`analytics.Options.Pending`), so pages never show the lag. A clean stop flushes everything.
- **Checkpointer.** `wal_autocheckpoint` is only a hard cap (16,384 pages; server 131,072); a dedicated
  connection runs PASSIVE checkpoints between writer transactions when the WAL passes the profile's soft
  size or interval (escalating to RESTART when readers keep it from finishing), and the hourly job a
  TRUNCATE. `journal_size_limit` bounds the WAL file after a reset. Periodic store jobs (trail seal) run in
  its slot.
- **Trails.** Each tracker trail flush appends one varint-coded row per flight to `trail_tail`
  (`flight_id, t0, t1, n, data`); the seal moves them into per-flight `trail_chunk` rows (columnar delta varints, deflate; decoded and compared before the tail rows are deleted);
  the hourly job compacts finished flights into 256-point chunks. Readers merge chunks, tail and (during the
  migration) legacy `positions` rows with 1.0 order and duplicate semantics. Table counts are kept as exact
  counters, so `/api/db/stats` does not walk the big tables.
- **Hourly maintenance.** The engine's scheduler calls `Store.HourlyMaintenance` 3 min after each hour:
  `RollupHours`, the registered hourly jobs (trail compaction, sealed histogram days, `flights_hour` ICAO
  sets, backup expiry), `PRAGMA optimize` (analysis_limit 400) and a TRUNCATE checkpoint.
- **Migration.** DDL migrations run at open (`RegisterMigration`); data migrations (`registerDataMigration`:
  trail, heat, range, flights_hour, hist_day, met, guess) run in the background in small, resumable
  transactions with progress in `schema_data_migrations`, a free-disk check and an optional pre-migration
  backup (`storage.backupBeforeMigrate`, `<data>/backup/<db>-pre-1.1-<date>.db`, deleted 14 days after
  completion). Every reader understands both layouts until a migration is done; afterwards free pages are
  returned with stepwise incremental vacuum. Status: `Store.MigrationInfo()` in `/api/db/stats`
  (`migration`), `airview db migrate --status`.
- **Compact statistics.** `heat` is keyed `(tile, cell, ant)` WITHOUT ROWID (4x4-cell tiles, so one flush
  touches fewer leaves), `range_hour` is WITHOUT ROWID with the lossless compact histogram, closed days of
  `coverage` / `signal_hist` / `elev_hist` older than 2 days are sealed into `hist_day` blobs, met
  observations live in `met_sample` (scaled integers in a time-ordered key), and `weather_fc` is rewritten
  only for rows that changed.
- **Logical tables.** Exports (`/api/db/export/<table>.csv`) and `tools/dbparity` read tables whose physical
  layout changed (`positions`, `heat`, `coverage` / `signal_hist` / `elev_hist`, `met_obs`) through
  `RegisterLogicalTable`, with the 1.0 columns, order and NULL handling.

The core schema below is the logical (1.0) view; physical tables added by 1.1: `trail_tail`, `trail_chunk`,
`hist_day`, `flights_hour`, `met_sample`, `schema_data_migrations`. `ant` = antenna ID, with `''` meaning "all antennas combined".
Altitude bands (ft): `0:<1000 1:1–5k 2:5–10k 3:10–20k 4:20–30k 5:30–40k 6:>40k`.

Core schema (store owns the DDL; add columns you find necessary, keep it compact):

- `kv(key PK, value, updated)` — misc state.
- `aircraft(icao INTEGER PK, first_seen, last_seen, callsign, reg, type, operator, country, category, msgs, flights, max_dist_km, …)` — every airframe ever seen.
- `flights(id PK, icao, callsign, squawk, start, end, msgs, positions, src_mask, max_dist_km, max_dist_brg, min_dist_km, min_alt, max_alt, max_gs, rssi_min, rssi_max, rssi_avg, first_lat, first_lon, last_lat, last_lon, emergency, dep, arr)` — one row per contact session (gap > 30 min = new flight); `dep` / `arr` = guessed airport codes, NULL until resolved after the flight closed, `''` when no airport fits (core migration 2).
- `positions(flight_id, t, lat_e5 INT, lon_e5 INT, alt, gs, track, rssi_x10, src) WITHOUT ROWID, PK(flight_id,t)` — down-sampled trail points.
- `stats_minute(t, ant, frames, df0,df4,df5,df11,df16,df17,df18,df20,df21, df_other, crc_bad, positions, mlat_positions, aircraft, aircraft_pos, adsb, mlat, modes, max_km, rssi_avg, rssi_min, rssi_max, PK(t,ant))` and `stats_hour` with the same columns (rolled up; hourly kept forever).
- `range_hour(t, ant, max_km, p99_km, p95_km, p90_km, p50_km, positions, aircraft, far_count, resid_db, dist_hist BLOB, sector_max BLOB, PK(t,ant))` — hourly range metrics (5 km distance histogram ×120 bins as little-endian uint32; `sector_max` = 36 × 10° float32 max km; `resid_db` = mean of `rssi + 20·log10(slant_km)`), the dependent variable for weather correlation and forecasting.
- `coverage(day, ant, brg_bin, alt_band, max_km, count, rssi_sum, rssi_n, PK(day,ant,brg_bin,alt_band))` — polar coverage, `brg_bin` = 2° bins (0..179), `day` = Unix day number.
- `signal_hist(day, ant, dist_bin, rssi_bin, count, PK(...))` — RSSI vs distance (10 km × 1 dB bins; `rssi_bin` = floor(dBFS) in −60..−1, `store.RSSIBin` = `geo.RSSIBinStored(geo.RSSIBin(x))`).
- `elev_hist(day, ant, elev_bin, count, rssi_sum, resid_sum, PK(...))` — elevation-angle pattern (0.5° bins from −2° to +90°).
- `heat(cell, ant, count, rssi_sum, min_alt, max_alt, last_t, PK(cell,ant))` — all-time position grid, 0.05° cells (`cell = (latIdx<<16)|lonIdx`), for heatmap, coverage-floor map and the location guesser.
- `weather_obs(t PK, temp_c, dewpoint_c, rh, pressure_msl, surface_pressure, wind_kmh, wind_dir, gust_kmh, cloud, cloud_low, cloud_mid, cloud_high, precip_mm, rain_mm, snow_cm, visibility_m, cape, weather_code, is_day, n_surface, dn_dh, k_factor, duct, profile_json)` — hourly observations/analysis.
- `weather_fc(t PK, issued, …same columns…)` — latest forecast per target hour; `weather_fc_log` keeps what was forecast `lead_h` hours ahead for backtesting.
- `ntp_samples(t, server, offset_ms, delay_ms, stratum, PK(t))`.
- `met_obs(t, icao, lat, lon, alt, wind_dir, wind_kt, oat_c, src)` + `met_profile(hour, level, u_sum, v_sum, t_sum, n_wind, n_temp, PK(hour,level))` (1000 ft levels).
- `records(key PK, value, icao, t, detail_json)` — all-time records (farthest, highest, fastest, …).
- `events(id PK, t, kind, icao, detail_json)` — emergencies, new records, feed up/down, notable catches.
- `acdb(icao INTEGER PK, reg, type, desc, operator, flags, year)` — imported aircraft database.
- `receiver_status(t PK, feed, json)` — receiver status frames (GPS lock etc.).

Other packages read via `store.DB()` (read pool) and may register their own extra tables through the store's
migration hook; all writes go through the store's single writer.

## Front-end (`web/static`)

No bundler. `index.html` + ES modules. Vendored libraries live in `web/static/vendor/` (MapLibre GL JS, Apache ECharts).
Look: **striking dark black → deep electric-blue gradient**, glassy panels, crisp tabular numerics; must feel like a
modern pro instrument, not a default-bootstrap dashboard.

- `js/app.js` — shell, hash router, lazy `import('./pages/<id>.js')`. Page ids (fixed, sidebar order):
  `map`, `aircraft`, `reports` (Reports & Replay), `alerts`, `stats`, `antenna`, `rf` (RF Lab), `weather`, `forecast`,
  `timing`, `guess`, `settings`.
  A missing page module renders a "module not installed" placeholder.
- Page module contract: `export default { id, title, mount(container, ctx), unmount() }`; `ctx` gives `api`, `live`
  (shared SSE store), `units`, `ui`, `charts`, `config`.
- `js/api.js`, `js/live.js`, `js/units.js`, `js/ui.js`, `js/charts.js`, `js/config.js`, `js/geo.js`, `js/icons.js` —
  shared helpers pages must reuse. **The authoritative reference for page authors is `web/static/js/README.md`**
  (page contract incl. `fullBleed`, `onRoute`, the full `ctx`, every helper signature).
- Map tiles: MapLibre GL + OpenFreeMap vector tiles (free, no key) with a custom dark-blue style; when
  `status.tileCache` is true, `transformRequest` rewrites whitelisted upstream URLs to `/tilecache/<host>/<path>`.

## Hub data port (summary — details live in `internal/hub`)

Line/length-framed TCP protocol, version tagged. Login = server nonce → client `HMAC-SHA256(password, nonce)`; the
password never crosses the wire. After login the server streams: station metadata (`model.Receiver` + antennas, JSON),
then every `RawFrame`/`DecodedUpdate` with its original receiver timestamps, signal level, feed/channel attribution,
plus heartbeats. Slow clients are dropped, never allowed to back-pressure the engine.
