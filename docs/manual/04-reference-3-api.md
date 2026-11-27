# 4.3 HTTP API overview

Everything the web interface shows comes from AirView's own HTTP API, and every page of the interface is built on the same endpoints you can call yourself: from scripts, dashboards, home-automation systems or another AirView. This chapter is an overview and an index. The complete reference, with every parameter, response type and a real example response for each endpoint, is `docs/API.md` in the AirView distribution; it is generated from the server's route table, and a test fails when the two disagree.

---

## 4.3.1 Basics

- **Base URL:** the address you open the web interface on, e.g. `http://raspberrypi.local:8080`. All paths below are relative to it.
- **Format:** JSON in UTF-8, gzip-compressed when the client accepts it. Responses are never cached by the browser (`Cache-Control: no-store`).
- **Errors:** an HTTP status plus `{"error":"message"}`:

  | Status | Meaning |
  |---|---|
  | 400 | bad parameter |
  | 401 | admin login needed |
  | 403 | cross-origin change refused |
  | 404 | unknown endpoint or object |
  | 409 | not possible in the current state |
  | 429 | throttled (see the `Retry-After` header) |
  | 502 | an upstream service (Open-Meteo, NTP, tile host) failed |
  | 503 | engine restarting (e.g. after a factory reset) |
  | 504 | timed out |

- **Units:** timestamps are Unix **milliseconds** UTC; altitude in ft; speed in kt; vertical rate in ft/min; distance in km; angles in degrees true, clockwise from north; signal in dBFS (negative, 0 = full scale); weather in °C, hPa, km/h, mm, %, m. Wind directions are where the wind blows **from**. (The units chosen in the interface affect only the display.)
- **Unknown values** are `null` or omitted; `NaN` never appears; arrays are never `null` (empty = `[]`).
- **Time ranges:** wherever an endpoint takes a range, use `from` and `to` (Unix ms) or `hours=N` / `days=N` for the span ending now (or at `to`).
- **`ant` parameter:** an antenna id from `antennas[].id`; empty, `all` or `combined` means all antennas combined.
- **Altitude bands** (`band` 0..6): < 1000 ft, 1–5k, 5–10k, 10–20k, 20–30k, 30–40k, ≥ 40k ft.
- **Per-bearing arrays** have 180 entries (2° bins centred on 1°, 3°, … 359°).
- **Position source codes** (`src`): 0 ADS-B, 1 MLAT, 2 TIS-B / ADS-R, 3 other.

## 4.3.2 Access control

| Client | Read (`GET`) | Change (`PUT`/`POST`/`DELETE`) |
|---|---|---|
| The machine itself (loopback) | yes | yes, always admin |
| Standalone, no admin password set | yes | private-network clients only (RFC 1918, IPv6 ULA, link-local), unless `http.allowOpenAdmin` is `true` |
| Standalone, admin password set | yes | after `POST /api/login` |
| Aggregator mode | public endpoints only, unless signed in | per account role (admin, station owner), after `POST /api/login` with password and authenticator code |

- Read-only endpoints may be fetched cross-origin (`Access-Control-Allow-Origin: *`, no credentials).
- **Changes are same-origin only:** a request whose `Origin` names another host, or that the browser marks as cross-site, is refused with `403`. Changes addressed to a public DNS name are accepted only with a login session (DNS-rebinding protection).
- The session cookie is `airview_session` (HttpOnly). In standalone mode sessions last 30 days and end on restart, logout, factory reset or password change. In aggregator mode the cookie is `SameSite=Strict` (plus `Secure` over TLS), and passwords are accepted only over HTTPS through a trusted proxy or from the machine itself, unless `http.allowInsecureLogin` is set.
- After 5 failed logins from one address further attempts get `429` with a growing `Retry-After` (2 s, doubling up to 5 min).
- In aggregator mode public viewers see the public shapes only: `GET /api/status` answers like `GET /api/public/status`, trails are limited to 10 minutes and 120 points per aircraft, and no receiver, feed or station location data is exposed.

### Logging in from a script

```sh
# standalone station with an admin password
curl -c cookies.txt -H 'Content-Type: application/json' \
     -d '{"password":"..."}' http://station.local/api/login
curl -b cookies.txt -X PUT -H 'Content-Type: application/json' \
     --data @config.json http://station.local/api/config
```

From the station itself no login is needed (`curl http://127.0.0.1/api/config`).

## 4.3.3 Endpoints for monitoring and integration

| Endpoint | Use |
|---|---|
| `GET /healthz` | Health check for monitors, load balancers, Docker and systemd. Never rate-limited, no database query, no station data. `200` with `status` `ok` or `degraded`; `503` with `failing` (a feeder port could not be opened, or a database writer is stuck) or `unavailable` (engine not running). |
| `GET /api/status` | Version, mode, uptime, live counters, feed states. |
| `GET /api/aircraft` | The current aircraft list. |
| `GET /api/events` | Server-sent event stream that drives the live map (`delta=1` for incremental updates). |
| `GET /data/aircraft.json`, `GET /data/receiver.json` | tar1090-compatible output, so a separately installed tar1090 or other tools can read AirView. |
| `GET /api/config` / `PUT /api/config` | Read (secrets blanked) or replace the stored configuration; the change is validated, saved and applied live. For the admin, settings locked by the operator file, an environment variable or `-set` (4.1.1) are listed in `locked` with their origin; changes to them are saved, but the override stays in force. |
| `GET /api/update`, `POST /api/update/check`, `POST /api/update/install` | State of the signed self-update, check now, install now. |

Example:

```text
$ curl -s http://127.0.0.1:18783/healthz
{"ok":true,"status":"ok","mode":"standalone","version":"1.0.1","uptimeS":6,"checks":{"engine":"ok"}}
```

On an aggregator `checks` also lists the registry, every feeder listener (`listener.avf`, `listener.beast`, `listener.mlatc`, …) and the databases.

### The feeder's status page API

`airview-feeder` serves a small API on its status page (`ui.addr`, default `127.0.0.1:8099`): `GET /api/status`, `GET /api/health`, `POST /api/claim` (claim code), `POST /api/retry` (reconnect now), `GET /api/settings` and `PUT /api/settings`. `airview-feeder status -json` prints the same status document.

## 4.3.4 Endpoint index

The index lists every endpoint of AirView 1.0.0 by area. *admin* marks endpoints that change something and need admin rights (4.3.2). Endpoints marked *(aggregator mode)* exist only on a network aggregator; there the account role decides who may call them.

<!-- BEGIN GENERATED: endpoints (from docs/API.md, AirView 1.0.0) -->

#### Core

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/status` |  | Application status (`model.Status`) |
| `GET /api/config` |  | The stored configuration (`model.Config`) with secrets blanked |
| `PUT /api/config` | admin | Validate, save and apply the configuration live; returns the stored configuration (redacted) |
| `POST /api/factory-reset` | admin | Wipe the database and restore the default configuration, then restart the engine |
| `POST /api/login` |  | Admin login |
| `POST /api/logout` |  | End the admin session |
| `GET /api/session` |  | Whether this client is admin and whether a password is configured |
| `GET /api/aircraft` |  | Live aircraft table (`model.LiveSnapshot`), rebuilt once per second |
| `GET /api/aircraft/{icao}` |  | One aircraft: live state, live trail, reference data, aircraft-database record, all-time totals and recent flights |
| `GET /api/trails` |  | In-memory trails of all live aircraft |
| `GET /api/events` |  | Live stream (Server-Sent Events) |
| `GET /api/notices` |  | The most recent notices (at most 50, oldest first) for clients that connect late |
| `GET /api/feeds` |  | Feed connections, per-feed and per-antenna counters, and the data port (hub) with its clients |
| `GET /api/tilecache` |  | Tile cache size and counters |
| `POST /api/tilecache/clear` | admin | Delete all cached map tiles |
| `GET /tilecache/{host}/{path...}` |  | caching proxy for the whitelisted map tile hosts |

#### Statistics

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/stats/overview` |  | Everything a statistics landing page needs: last 24 h totals, sources, downlink formats, all-time totals, today, busiest day/hour, top lists, records, recent events |
| `GET /api/stats/rates` |  | Traffic over time: message rate, positions, aircraft counts, range and signal as parallel arrays, plus totals |
| `GET /api/stats/df` |  | Downlink-format and source mix over a range, plus the live DF / Comm-B register counters since start |
| `GET /api/stats/heatmap` |  | Day-of-week x hour-of-day heat map (7 x 24, Monday first, local time) of aircraft and message rate |
| `GET /api/stats/daily` |  | Daily totals (local days) with the busiest day and hour |
| `GET /api/stats/top` |  | Top operators, countries, aircraft types and airframes |
| `GET /api/stats/altitudes` |  | Altitude distribution: positions per altitude band and a histogram of flight maximum altitudes |
| `GET /api/stats/records` |  | All-time records |
| `GET /api/stats/events` |  | Event log, newest first: emergencies, military / interesting catches, new records, feed up / down |
| `GET /api/stats/counters` |  | Decoder counters since the program started (`tracker.Counters`): frames, CRC failures, duplicates, positions, per-DF, per-antenna and per-feed totals |
| `GET /api/stats/range` |  | Hourly range metrics (`range_hour`): maximum and percentile distances, positions, aircraft, far-reception count, signal residual |
| `GET /api/stats/daynight` |  | Day / twilight / night comparison of range and signal, by local hour, with a significance test |

#### Antenna & coverage

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/antenna/coverage` |  | Polar coverage: maximum, robust and typical range per 2 degree bearing bin, overall and per altitude band, against the radio horizon; measured skyline and obstructions |
| `GET /api/antenna/coverage.geojson` |  | Coverage outlines as GeoJSON polygons for the map |
| `GET /api/antenna/horizon` |  | Radio line-of-sight table per antenna and altitude band, and the achieved range against it |
| `GET /api/antenna/skyline` |  | Measured skyline (lowest elevation angle with reception per bearing) and detected obstructions |
| `GET /api/antenna/heat` |  | All-time position heat grid as parallel arrays (cell centre, count, mean RSSI, lowest and highest altitude) |
| `GET /api/antenna/signal` |  | RSSI against distance: 2-D histogram cells, per-distance percentiles and the path-loss fit |
| `GET /api/antenna/azimuth` |  | Measured azimuth gain pattern (relative dB per 2 degree bearing) and the directional lobe fitted to it |
| `GET /api/antenna/elevation` |  | Measured elevation pattern: counts, mean RSSI and distance-corrected residual per 0.5 degree of elevation |
| `GET /api/antenna/fit` |  | Yagi lobe fit: inferred boresight, beamwidth and gain over the omni level, compared with the configured yagi |
| `GET /api/antenna/linkbudget` |  | Predicted (link budget) against achieved range per altitude band |
| `GET /api/antenna/pattern` |  | Configured antennas with their modelled azimuth / elevation patterns on the analysis grid, next to the measured pattern |
| `GET /api/antenna/score` |  | Station score 0..100 with sub-scores and plain-language findings |
| `GET /api/antenna/report` |  | The complete antenna analysis in one document (signal, azimuth, elevation, antennas, yagi, linkBudget, notes) |
| `GET /api/antenna/daynight` |  | Same as `/api/stats/daynight` |
| `GET /api/antenna/budget` |  | Link budget for one aircraft position: path loss, antenna gain, received power, noise floor, SNR, margin |
| `GET /api/radio/pattern` |  | Modelled antenna pattern cuts: azimuth cut at a given elevation, elevation cut at a given azimuth, predicted range around the compass |
| `GET /api/radio/transponders` |  | Transponder power classes used by the link budget |

#### Weather

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/weather/current` |  | Current conditions, poller status, the stored row of the current hour and the radio-propagation state derived from it |
| `GET /api/weather/hourly` |  | Hourly weather rows from the database: observed hours up to now and forecast hours from the current hour on |
| `GET /api/weather/profile` |  | Pressure-level profile of the weather model for one hour with its refractivity analysis (N / M profile, layers, ducts) |
| `GET /api/weather/radar` |  | RainViewer precipitation radar frame index for the map overlay |
| `GET /api/weather/impact` |  | Weather impact on reception: correlations, response curves, ridge regression and ducting episodes |
| `GET /api/weather/ducting` |  | Refractivity / ducting time line: per hour the gradient, k-factor, 0..100 propagation index, class and ducts; plus the inversions aircraft measure right now |
| `POST /api/weather/refresh` | admin | Fetch the weather now instead of waiting for the next poll |

#### Aircraft-derived meteorology

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/met/profile` |  | Vertical profile of wind and temperature above the station derived from aircraft (Mode S EHS / MRAR), in 1000 ft layers, with its analysis |
| `GET /api/met/history` |  | Hourly time-height history of the aircraft-derived profiles |
| `GET /api/met/observations` |  | Recent single wind / temperature observations for a map layer |
| `GET /api/met/winds` |  | Winds-aloft table at standard flight levels, jet maximum, strongest shear, freezing level and tropopause from the current profile |
| `GET /api/met/compare` |  | Aircraft-derived profile against the weather model (differences are aircraft minus model) and the radio refractivity of measured inversions |
| `GET /api/met/stats` |  | Collector counters and the learned per-airframe heading corrections |

#### Forecasts

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/forecast/traffic` |  | Traffic forecast for the next 72 h: aircraft count and message rate with 80 % intervals, busiest and quietest hours |
| `GET /api/forecast/reception` |  | Reception forecast for the next 72 h: p95 / max range, 0..100 index, far-reception probability, atmosphere and drivers per hour |
| `GET /api/forecast/weather` |  | Local weather outlook: bias-corrected hourly forecast, Zambretti, detected patterns (fronts, fog, inversion, convection), analogue forecast |
| `GET /api/forecast/outlook` |  | Digest of all forecast products for a home page: nowcast, the next 12 h of each product, highlights |
| `GET /api/forecast/verification` |  | How good the forecasts are: backtest skill per model, forecast-versus-actual series, what data is still missing |
| `GET /api/forecast/nowcast` |  | One-line reception nowcast for a top bar: index, label, trend, sentence |
| `GET /api/forecast/status` |  | State of the forecaster (last fit, last build, hours of history, last error) |
| `POST /api/forecast/refit` | admin | Refit all forecast models now (normally hourly) |

#### Timing

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/timing/ntp` |  | NTP reference: configuration, status, newest sample and statistics (last hour, 24 h, everything in memory) |
| `GET /api/timing/history` |  | NTP offset / delay history |
| `GET /api/timing/latency` |  | Per-feed latency between the receiver's GPS frame timestamps and the (NTP-corrected) host clock: percentiles, histogram, per-minute history; plus the latency-chain summary and stage histograms |
| `GET /api/timing/latency/aircraft` |  | Per-aircraft latency table (aircraft with positions, most positions first): transponder latency of T=1 positions, estimated position age, age jitter, propagation, flags |
| `GET /api/timing/latency/aircraft/{icao}` |  | One aircraft's latency row and the stage waterfall of its latest position message |
| `GET /api/timing/latency/feeds/{id}` |  | Receiver-delay minute series of one feed (up to 24 h, restored from the database after a restart) |
| `GET /api/timing/ntpcheck` |  | NTP / receiver GPS clock consistency: NTP server quality, offset uncertainty, per-feed receiver-delay clock filter (10 s minima, floor, jitter, drift), issues and an explanation |
| `GET /api/timing/receiver` |  | Receiver timing state: GPS-timestamp mode and status text per feed, receiver GPS position, stored status frames |
| `POST /api/timing/ntp/sample` | admin | Take an NTP measurement now with the configured server, or test another server once without saving it |

#### Location guesser

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/guess` |  | Receiver location estimated from the received positions alone (four methods and their fusion) |
| `POST /api/guess` | admin | Run the location guesser now on the newest data (flushes the statistics first) and return the result |

#### History

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/history/aircraft` |  | Every airframe ever seen: search, sort, paging |
| `GET /api/history/aircraft/{icao}` |  | One airframe: totals, reference data, its flights (newest first) and its events |
| `GET /api/history/flights` |  | Flights (contact sessions), newest first |
| `GET /api/history/flights/{id}` |  | One flight with its stored trail |

#### Database

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/db/stats` |  | Database size, writer counters, row counts per table, retention settings, aircraft database and tile cache state |
| `POST /api/db/vacuum` | admin | Compact the database file (blocks writes while it runs; can take minutes on a Raspberry Pi) |
| `POST /api/db/prune` | admin | Apply the retention settings now |
| `POST /api/db/checkpoint` | admin | Flush the in-memory statistics and checkpoint the write-ahead log |
| `POST /api/db/rollup` | admin | Roll minute statistics up into hours now (normally 3 min after each hour) |
| `GET /api/db/acdb/sources` |  | Downloadable aircraft databases (with licence and attribution), what is installed, and the import state |
| `POST /api/db/acdb/import` | admin | Start downloading and importing an aircraft database in the background |
| `POST /api/db/acdb/upload` | admin | Import an aircraft database file sent as the raw request body (tar1090-db or OpenSky CSV, optionally gzip; at most 600 MB) |
| `POST /api/db/acdb/cancel` | admin | Cancel a running import |
| `GET /api/db/acdb/progress` |  | State of the current or last aircraft database import |
| `GET /api/db/acdb/lookup` |  | Look one address up in the aircraft database and the built-in tables |
| `GET /api/db/export` |  | Tables available for CSV export with their row counts |
| `GET /api/db/export/{file}` |  | Download one table as CSV (`{file}` = `<table>.csv`), streamed |

#### Geo, astronomy & reference data

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/geo/parse` |  | Parse pasted coordinates in almost any notation into latitude / longitude |
| `GET /api/geo/format` |  | Format a position in the common notations |
| `GET /api/geo/elevation` |  | Terrain elevation at a position (Open-Meteo elevation service; needs the internet) |
| `GET /api/geo/magvar` |  | Magnetic declination and field (World Magnetic Model 2025) at a position, now |
| `GET /api/astro/now` |  | Sun position, day phase and today's sun events at the receiver, the day/night terminator rings and the moon phase |
| `GET /api/astro/terminator.geojson` |  | The night side of the earth as a GeoJSON FeatureCollection of four polygons (properties `kind`, `sunAlt`) |
| `GET /api/astro/days` |  | Sunrise, sunset and twilight times for the coming days |
| `GET /api/refdata/icao/{icao}` |  | What the built-in tables know about an ICAO address: state of registry, allocation block, derived registration, military heuristic |
| `GET /api/refdata/callsign/{callsign}` |  | Interpret a callsign: operator, telephony name, flight number or registration |
| `GET /api/refdata/type/{code}` |  | ICAO aircraft type designator: manufacturer, model, class, wake category |
| `GET /api/refdata/squawk/{code}` |  | Conventional meaning of a Mode A code |

#### System

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/system` |  | Process and host information: version, platform, uptime, goroutines, memory, data directory, restart-required flag |
| `GET /api/ssr` |  | EXPERIMENTAL: state of the SSR self-location (enabled, feed, counters, estimator statistics incl |
| `POST /api/ssr/solve` | admin | EXPERIMENTAL: run an SSR self-location solve now (about 0.3 s; poll `GET /api/ssr`) |
| `GET /api/update` |  | Self-update state: running version, channel, automatic updates on/off, last check and its error, the newer version available, the last update transaction, and whether this installation can install updates itself |
| `POST /api/update/check` | admin | Check the release manifest now (signature verified; nothing is installed) |
| `POST /api/update/install` | admin | Download, verify and install the newer version, then restart into it |

#### Public network (aggregator mode)

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /feeder/install.sh` |  | The Raspberry Pi feeder installer with this network's presets (feeder host and port, AVF certificate pin, web address, release key) |
| `GET /feeder/manifest.json` |  | The signed airview-feeder release manifest from `aggregator.feederReleaseDir` (404 when none is configured) |
| `GET /feeder/manifest.json.sig` |  | The Ed25519 signature of manifest.json (base64) |
| `GET /feeder/dl/{target}` |  | One airview-feeder release file (`dl/<target>` in the release directory): `linux-arm64`, `linux-armv7`, `linux-amd64`, `windows-amd64`, `windows-arm64`, .. |
| `GET /feeder/beta/manifest.json` |  | The beta channel's signed manifest (`beta/` in the release directory) |
| `GET /feeder/beta/manifest.json.sig` |  | The beta manifest's signature |
| `GET /feeder/beta/dl/{target}` |  | One beta release file |
| `GET /api/share/{token}` |  | What a share link shows: label, scope, number of stations, expiry and the network name (no station data) |
| `GET /api/share/{token}/aircraft` |  | The aircraft the link's stations heard in the last 60 s (`publicdto.Snapshot`: no receiver data; low aircraft on the island-guard grid when the link says so) |
| `GET /api/share/{token}/stats` |  | Non-locating statistics of the link's stations (scope `map+stats` only) |
| `GET /healthz` |  | Health check for load balancers, service managers and monitors |
| `GET /api/public/status` |  | Public application status: version, mode, network name, live counters and which public surfaces are enabled |
| `GET /api/public/aircraft` |  | The merged live picture without receiver data (`publicdto.Snapshot`) |
| `GET /api/public/aircraft/{icao}` |  | One aircraft of the public picture: live state, live trail without signal levels, reference data |
| `GET /api/public/trails` |  | Live trails of the public picture (signal column null) |
| `GET /api/public/events` |  | The public live stream (Server-Sent Events): `aircraft` (`publicdto.Snapshot`), `status` (`publicdto.Status`) and public `notice` events |
| `POST /api/events/view` |  | Change the viewport of a live stream opened with `bbox` or `view=1` (no new keyframe) |
| `GET /api/public/stations` |  | Stations that opted in to a public pin or public statistics (`publicdto.StationList`) |
| `GET /api/public/summary` |  | The network landing page in numbers (`publicdto.Summary`): station counts, aircraft, MLAT aircraft, message rate |
| `GET /api/public/stats` |  | The network in numbers: approved and online stations and the live counters |
| `GET /api/public/mlat` |  | The network's empirical MLAT accuracy over the last hour (hold-out aircraft against their ADS-B reference) and its 1° cells (`publicdto.MLATCoverage`) |
| `GET /api/public/join` |  | What a new feeder needs to join this network: host name, open listener ports (AVF/1, Beast, mlat-client), admission and registration policy, the backup retention named in the deletion notice and the privacy-notice link |
| `GET /data/aircraft.json` |  | The public picture in the tar1090 / readsb `aircraft.json` shape |
| `GET /data/receiver.json` |  | tar1090 `receiver.json` |

#### My stations (aggregator mode)

| Method and path | Admin | Purpose |
|---|---|---|
| `POST /api/me/stations/{sid}/location/pending/confirm` |  | Apply the position the station's feeder announced and the aggregator holds (a large move waits for the survey or this confirmation; finding PRIV-1) |
| `PUT /api/me/stations/{sid}/mlat/survey` |  | Allow or reject the automatic use of the station's survey position for MLAT (DESIGN §19.3) |
| `GET /api/me/stations/{sid}/notices` |  | Location notices of the station (last 30 days, newest first): finished surveys, declared-position checks, the automatic use of the survey position for MLAT and its owner override, confirmations |
| `GET /api/me/notices` |  | Location notices of all the viewer's stations (last 14 days, at most 50, newest first) |
| `GET /api/me/stations/{sid}/minutes` |  | The station's last 16 minutes of feeder statistics (AVF/1 MINUTE_STATS merged with what the server received): exact, received, merged and shed counts, selective mode |
| `GET /api/me/shares` |  | The account's share links (no secrets): label, stations, scope, state, expiry, last use and use count, plus the location warning |
| `POST /api/me/shares` |  | Create a share link for own stations (read-only live view) |
| `DELETE /api/me/shares/{id}` |  | Revoke an own share link (it stops working at once) |
| `GET /api/me/stations/{sid}/survey` |  | The station's reverse-MLAT survey: the running job, the last results (position, σ, 95 % ellipse, stable/suspect, declared-position check, MSL/HAE verdict) and the next scheduled check |
| `POST /api/me/stations/{sid}/survey` |  | Ask for a survey of the station (at most one per hour and station) |
| `GET /api/me/stations/{sid}/locate` |  | Locate Me v2 for a station without a location: the approximate position estimated from what the station receives (radio horizon, skyline, signal level), with its 95 % circle and the error against the last survey |
| `GET /api/me` |  | The logged-in account and its stations (exact data) |
| `GET /api/me/stations` |  | The own stations with exact location, privacy settings, admission and live state |
| `GET /api/me/stations/{sid}` |  | One own station: exact data, location estimates, the public pin preview (exactly as others see it), live links and statistics |
| `POST /api/me/stations/{sid}` |  | Change an own station's name, privacy (pin precision, show name, public statistics) or declared position |
| `PUT /api/me/stations/{sid}/privacy` |  | Set an own station's public pin precision, name display and public statistics |
| `PUT /api/me/stations/{sid}/name` |  | Rename an own station |
| `PUT /api/me/stations/{sid}/location` |  | Declare an own station's antenna position (cross-checked by the survey); `{"clear":true}` removes it |
| `POST /api/me/stations/{sid}/location/confirm` |  | Use a location estimate (survey, GNSS) as the station's position for MLAT (owner confirmation, DESIGN §10.1) |
| `POST /api/me/stations/{sid}/unclaim` |  | Release an own station (it keeps feeding; it can be claimed again) |
| `GET /api/me/stations/{sid}/stats` |  | Live statistics of an own station: frames, positions, aircraft this minute, late frames, live links, last seen |
| `GET /api/me/stations/{sid}/mlat` |  | Timing and MLAT of an own station: position source used for MLAT, clock state (class, anchor state, σθ, drift, short-frame offset, jitter, steps, events), sync peers, MLAT contributions and residuals, timed frames per minute and result eligibility |
| `GET /api/me/stations/{sid}/mlat/longterm` |  | History of an own station's MLAT participation over days, one point per hour: timed frames, groups, fixes and outliers per minute, synced peers, synced share, σθ, residual RMS and drift |
| `GET /api/me/stations/{sid}/connections` |  | Connection log of an own station (newest first; addresses only as daily-salted hashes) |
| `GET /api/me/aircraft` |  | The live picture for an owner: `mine` and `stations` (own SIDs) per aircraft, geometry for the selected own station |
| `GET /data/me/aircraft.json` |  | tar1090 `aircraft.json` of the aircraft the own stations heard in the last 60 s (session-scoped) |
| `GET /api/me/check` |  | Feed Check: the live feeder links coming from the visitor's own address (exact IPv4, or the same IPv6 /64) |
| `POST /api/me/claim` |  | Claim a station: a native feeder with its claim code, a compat feeder with its full UUID from the feeder's own address |
| `GET /api/me/export` |  | Export the account's data (JSON files in a zip) |
| `DELETE /api/me/account` |  | Delete the own account and its stations' data now; the session ends |

#### Network administration (aggregator mode)

| Method and path | Admin | Purpose |
|---|---|---|
| `POST /api/admin/stations/{sid}/timing` | admin | Override a station's timing quarantine: `trusted` (joins MLAT now), `excluded` (never), `quarantine` (judge again), `auto` (clear overrides) |
| `GET /api/admin/mlat/quarantine` |  | Stations in timing quarantine with the gate's progress per station, and the gate counters |
| `GET /api/admin/mlat/crlb/improve` |  | Where one more station would improve MLAT most: candidate sites ranked by the CRLB gain (area-weighted log improvement of σ_h), with newly covered and newly grade-A/B cells, plus the gain of every candidate cell for the map |
| `GET /api/admin/mlat/surveys` |  | Survey console: every approved station's location status, MLAT position source, running survey and last result (no coordinates), plus the network's location quality summary |
| `GET /api/admin/stations/{sid}/minutes` |  | A station's last 16 minutes of feeder statistics (as the owner sees them) |
| `GET /api/admin/shares` |  | Every share link of the network (no secrets) with its owner, stations and use |
| `DELETE /api/admin/shares/{id}` | admin | Revoke any share link |
| `GET /api/admin/stations/{sid}/survey` |  | A station's reverse-MLAT survey status (as the owner sees it) |
| `POST /api/admin/stations/{sid}/survey` | admin | Start a survey of a station (no rate limit) |
| `DELETE /api/admin/stations/{sid}/survey` | admin | Cancel a station's running or queued survey |
| `GET /api/admin/mlat/survey` |  | Survey counters: stations known, running and queued jobs, finished jobs by outcome, tap counters |
| `GET /api/admin/mlat/verification` |  | ADS-B position verification: counters (confirmed, contradicted, suspects, contradictions charged to a station, contradictions muted by the network-wide breaker, recovered clock-filter and engine panics), the aircraft whose broadcast positions are suspect now, and the last 200 transitions |
| `GET /api/admin/federation` |  | Federation: node ID, TLS pin, peers (state, direction, RTT, counters, last error), received aircraft and out-feeds |
| `GET /api/admin/mlat/modeac` |  | Mode A/C multilateration: counters and the shown targets (synthetic id, squawk, Mode C altitude, position, grade, σ) |
| `GET /api/admin/stations` |  | The station registry with exact positions and owners: `{"stations":[...],"total":n,"counts":{...}}` |
| `GET /api/admin/stations/{sid}` |  | One station with exact data, estimates, live links (with addresses) and statistics |
| `POST /api/admin/stations/{sid}` | admin | Change a station: admission, owner, name, trust, anchor, never-anchor flag |
| `PUT /api/admin/stations/{sid}` | admin | Edit a station (same body as the POST) |
| `POST /api/admin/stations/{sid}/admission` | admin | Approve, hold, deny or revoke a station |
| `POST /api/admin/stations/{sid}/reveal` | admin | A station's exact position and current address (audited `station.reveal`) |
| `DELETE /api/admin/stations/{sid}` | admin | Delete a station and its data (queued; survives restarts) |
| `POST /api/admin/feeders/{id}/revoke` | admin | Revoke a native feeder key (all its stations) |
| `GET /api/admin/tokens` |  | Enrol tokens (without secrets) |
| `POST /api/admin/tokens` | admin | Mint an enrol token; the secret is shown once |
| `DELETE /api/admin/tokens/{id}` | admin | Revoke an enrol token |
| `GET /api/admin/audit` |  | Audit log, newest first |
| `POST /api/admin/retention` | admin | Run the retention jobs now; returns their report |
| `GET /api/admin/links` |  | Every live feeder link of every listener, with addresses |
| `GET /api/admin/ingest/beast` |  | The Beast listener (30004): totals and every link |
| `POST /api/admin/ingest/beast/{link}/disconnect` | admin | Close one Beast link |
| `GET /api/admin/network` |  | Operator view of the aggregator: listeners (ports, addresses, errors), ingest statistics, tracker lock statistics, lanes, flight_station rows |
| `GET /api/admin/mlat` |  | MLAT engine counters: every stage (groups, clusters, solves, publishes, drops), clock filter, reference store, tracker, planner, k_atm |
| `GET /api/admin/mlat/quality` |  | MLAT self-validation: hold-out error (fix and track), σ calibration, NEES, week-on-week alarm |
| `GET /api/admin/mlat/history` |  | MLAT console history, one sample per minute for up to 24 h: hold-out error (fix and track N, P50, P95, cross-track P95, share within 1σ over the preceding 10 minutes), stage rates per minute (groups, clusters, solves, accepted, rejected, published, results, drops), clock filter (stations, synced, components, epoch cost), worker load |
| `GET /api/admin/mlat/longterm` |  | MLAT network history over days, one point per hour from the persisted minute tables: frames, groups, fixes and outliers per minute, synced stations, and the hold-out error per hour (N, P50, P95 from the summed error histograms of every cell) |
| `GET /api/admin/mlat/stations` |  | Per-station clock quality and MLAT residuals for every station the MLAT engine knows (class, anchor state, synced, σθ, drift, β short, jitter, steps, peers, contributions, used / excluded / outlier %, residual bias, σ scale, bias flag, timed frames per minute) |
| `GET /api/admin/mlat/sync` |  | Clock-sync matrix: per station and peer (SIDs) co-observations in the last 30 s, RMS residual, relative frequency, same clock domain |
| `GET /api/admin/mlat/crlb` |  | Expected horizontal MLAT σ (Cramér-Rao bound) at 3000 ft, FL100 and FL300 on a 0.25° grid from the located stations and their timing |
| `GET /api/admin/stations/{sid}/mlat` |  | Timing and MLAT of any station (as the owner view, peers by SID) |
| `GET /api/admin/aircraft/{icao}` |  | One aircraft with the full receiver list (every station that heard it in the last 60 s) |

#### Accounts (aggregator mode)

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/auth/setup` |  | Whether the first admin account still has to be set up |
| `POST /api/auth/setup/begin` |  | First admin setup, step 1: console token, user name and password; returns the authenticator secret |
| `POST /api/auth/setup/finish` |  | First admin setup, step 2: confirm the authenticator with a code; returns the recovery codes once |
| `POST /api/auth/register` |  | Create an owner account (no e-mail) and log in |
| `POST /api/auth/password` |  | Change the own password; other sessions of the account end |
| `POST /api/auth/totp/begin` |  | Start authenticator (TOTP) enrolment; returns the secret and otpauth URI |
| `POST /api/auth/totp/confirm` |  | Confirm the authenticator with a first code; returns 10 single-use recovery codes (shown once) |
| `POST /api/auth/totp/disable` |  | Turn the authenticator off (owners only; admins cannot) |
| `POST /api/auth/recovery-codes` |  | Replace all recovery codes |
| `GET /api/auth/sessions` |  | The own login sessions (no addresses are stored) |
| `POST /api/auth/logout-others` |  | End every other session of the own account |

#### Prediction

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/prediction/live` |  | Predicted position now of every aircraft the tracking filter follows (IMM filter, wind aware) |
| `GET /api/prediction/stats` |  | Tracking filter counters and self-scored prediction quality per horizon and flight phase |
| `GET /api/prediction/path/{icao}` |  | Predicted path of one aircraft with growing uncertainty ellipses, and when terrain will hide it |
| `GET /api/prediction/intent/{icao}` |  | Intent analysis of one aircraft: phase, holding, approach, closest approach to the station, level-off |
| `GET /api/prediction/proximity` |  | Predicted close approaches between airborne aircraft (informational only, not an ATC/TCAS function) |

#### Terrain

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/terrain/status` |  | Terrain analysis job (download / sampling / coverage progress), result availability, tile cache |
| `POST /api/terrain/prefetch` | admin | Download the elevation tiles and run the terrain analysis now (also when automatic prefetching is off) |
| `POST /api/terrain/recompute` | admin | Re-run the last terrain analysis ignoring caches (also resets the download back-off) |
| `POST /api/terrain/cancel` | admin | Cancel a running terrain job (the previous result stays) |
| `GET /api/terrain/coverage` |  | The complete terrain analysis: per antenna the horizon panorama, line-of-sight and radio coverage rings per altitude, advice |
| `GET /api/terrain/coverage.geojson` |  | Theoretical coverage per altitude as GeoJSON polygons for the map |
| `GET /api/terrain/horizon` |  | Horizon panorama of one antenna: terrain mask angle, distance and height of the skyline per bearing |
| `GET /api/terrain/panorama` |  | Same as `/api/terrain/horizon` |
| `GET /api/terrain/advice` |  | Antenna site advice: coverage gain of a higher mast, worst sectors with the limiting ridge, ready-made sentences |
| `GET /api/terrain/floor` |  | Coverage floor grid: lowest altitude visible from the antenna per cell |
| `GET /api/terrain/floor-image` |  | Coverage floor as a map image: URL, corner coordinates and legend for a MapLibre `image` source |
| `GET /api/terrain/floor.png` |  | The coverage floor PNG (Web Mercator rows, transparent where nothing is visible) |
| `GET /api/terrain/visible` |  | Is a point above the terrain radio horizon? (~100 ns, from the panorama.) |
| `GET /api/terrain/los` |  | Line of sight from the antenna to one point: obstruction, Fresnel clearance, diffraction loss and the terrain profile |
| `GET /api/terrain/profile` |  | Same as `/api/terrain/los` with `alt` as the altitude parameter, for the path profile chart |
| `GET /api/terrain/elevation` |  | Terrain elevation at a point from the elevation tiles (downloads the tile when missing) |

#### RF statistics

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/rf/summary` |  | Headline RF numbers: health score, frame rate, channel occupancy, noise / decode floor, detection range, radars, GNSS index, advice (`advice[]` sentences and `adviceItems[]` `{code,text,data}`) |
| `GET /api/rf/report` |  | Everything below in one document (`rfstat.Report`) |
| `GET /api/rf/signal` |  | Signal levels: RSSI histogram, decode floor, noise-floor estimate, overload share, per antenna / range ring / elevation, fading, multipath, aspect |
| `GET /api/rf/detection` |  | Reception probability of position / velocity / identification squitters versus distance, elevation, bearing and RSSI; 50 % / 90 % range; update gaps |
| `GET /api/rf/intervals` |  | Position update intervals per range band: percentiles and the probability that a position is at most T seconds old |
| `GET /api/rf/channel` |  | 1090 MHz channel: frames per DF, occupancy, CRC failures, garble model, load curve, bursts, last 5 minutes per second |
| `GET /api/rf/interrogators` |  | Interrogator census from DF11 replies (II / SI codes, rates, lock-out), surveillance reply rates, Comm-B mix, ACAS activity |
| `GET /api/rf/timing` |  | Squitter timing from the receiver's GPS timestamps: interval histograms per type, even/odd alternation, non-compliant transmitters |
| `GET /api/rf/fleet` |  | Fleet / equipage census: ADS-B versions, NIC / NACp / SIL, poor integrity share, emitter categories, TC28/29/31 use, GNSS vs baro altitude, GNSS interference |
| `GET /api/rf/gnss` |  | GNSS interference index: share of ADS-B aircraft with degraded navigation integrity, per minute (24 h) and per polar map cell |
| `GET /api/rf/health` |  | RF health: score 0..100 (-1 = no data), scored items with plain-language text, blocked sectors, advice |
| `GET /api/rf/history` |  | Hourly RF summaries (frame rate, occupancy, CRC failures, Pd, 90 % range, health ...), oldest first |

#### Navigation data

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/navdata/status` |  | Imported reference datasets (rows, import time, source, licence), downloadable sources, import state, options and attribution |
| `POST /api/navdata/import` | admin | Start downloading and importing reference datasets in the background |
| `GET /api/navdata/progress` |  | State of the current or last import (`navdata.ImportStatus`): `running`, `progress{dataset, stage, bytesRead, bytesTotal, records, index, count}`, `fraction` 0..1, `result`, `error` |
| `POST /api/navdata/cancel` | admin | Cancel a running import |
| `POST /api/navdata/route-cache/clear` | admin | Forget the cached online route look-ups |
| `GET /api/navdata/airports` |  | Airports in a box or around a point, largest first; optionally with runways or as a GeoJSON map overlay |
| `GET /api/navdata/search` |  | Search airports by code or name |
| `GET /api/navdata/navaids` |  | Radio navigation aids (VOR, DME, NDB, TACAN) in a box |
| `GET /api/navdata/airport/{code}` |  | One airport by ICAO, IATA, ident or GPS code, with its runways and usable landing directions |
| `GET /api/navdata/route/{callsign}` |  | Route of a callsign (standing data, then the opt-in online look-up) with airports, great-circle path and a plausibility check |
| `GET /api/navdata/photo/{icao}` |  | Aircraft photo metadata from planespotters.net (opt-in): thumbnail, large image, link and photographer |

#### Alerts

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/alerts/rules` |  | All alert rules (built-in defaults and user rules), sorted by id |
| `POST /api/alerts/rules` | admin | Create a rule |
| `GET /api/alerts/rules/{id}` |  | One rule |
| `PUT /api/alerts/rules/{id}` | admin | Replace a rule (built-in rules may be edited; reset-defaults restores them) |
| `DELETE /api/alerts/rules/{id}` | admin | Delete a rule |
| `POST /api/alerts/rules/{id}/enabled` | admin | Switch a rule on or off |
| `POST /api/alerts/rules/reset-defaults` | admin | Restore the built-in rules to their shipped state (user rules are kept) |
| `POST /api/alerts/test` | admin | Test a rule against the current live picture without firing anything |
| `GET /api/alerts/recent` |  | Recently fired alerts, newest first (in-memory ring of 500; logged ones are also in `/api/stats/events?kind=alert`) |
| `GET /api/alerts/highlights` |  | Aircraft currently highlighted by a rule (also in the live picture as `aircraft[].highlight`) |
| `GET /api/alerts/stats` |  | Alert engine counters: rules, evaluations, last evaluation time, fired / suppressed, webhook sent / failed / dropped, range baseline |
| `GET /api/alerts/placeholders` |  | Help text listing the message template placeholders (`{ident}`, `{alt}`, `{dist}` ...) |
| `POST /api/alerts/webhook-test` | admin | Send a test POST to a webhook URL |
| `GET /api/alerts/export` |  | All rules as a JSON array (download) |
| `POST /api/alerts/import` | admin | Import rules (all-or-nothing validation; rules with an existing id are replaced) |
| `GET /api/alerts/watchlists` |  | Watch-lists with their number of entries (`[{name, entries}]`) |
| `GET /api/alerts/watchlists/{name}` |  | The entries of one watch-list, or a download |
| `POST /api/alerts/watchlists/{name}/import` | admin | Import ICAO addresses into a watch-list (created if needed) |
| `PUT /api/alerts/watchlists/{name}/{icao}` | admin | Add (or update the note of) one address |
| `DELETE /api/alerts/watchlists/{name}/{icao}` | admin | Remove one address |
| `DELETE /api/alerts/watchlists/{name}` | admin | Delete a watch-list |

#### Replay, exports & reports

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/replay` |  | History replay: time-sliced pictures of all stored positions in a window, in compact arrays (tar1090 / VRS style) |
| `GET /api/history/flights/{id}/trace` |  | Full stored trail of one flight with track angles, and the guessed departure / arrival airports |
| `GET /api/export/flight/{file}` |  | Download one flight: `{id}.kml` (Google Earth, absolute altitudes), `{id}.geojson` (LineString [lon, lat, altM] with `properties.times`) or `{id}.csv` |
| `GET /api/export/live.kml` |  | The current picture with trails as KML |
| `GET /api/export/live.geojson` |  | The current picture with trails as GeoJSON |
| `GET /api/export/aircraft.csv` |  | Every airframe ever seen as CSV (streamed) |
| `GET /api/reports` |  | Report query (VirtualRadar style): flights in a time range filtered by callsign, registration, address, type, operator, country, military, distance, altitude, departure / arrival airport; paged and sorted |
| `GET /api/reports.csv` |  | The same report as a CSV download (streamed) |

#### Uplinks

| Method and path | Admin | Purpose |
|---|---|---|
| `GET /api/uplinks` |  | Uplinks of this station to aggregators: feeder ID, UUID and per-uplink state (no secrets) |
| `POST /api/uplinks` | admin | Uplink actions: `{"action":"claim","id":"...","rxNo":0}` returns a claim code (shown once), `{"action":"retry","id":"..."}` restarts a stopped uplink |

<!-- END GENERATED: endpoints -->
