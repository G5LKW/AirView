# AirView HTTP API

> Generated from the route table of `internal/server` (routes.go) and from real responses of a demo instance
> with 60 hours of simulated traffic and synthetic weather. Do not edit by hand: change the route table or the
> result types and regenerate (see `internal/server/docgen_test.go`). A test fails when this file and the
> route table disagree.

Base URL: the origin the UI was loaded from (`http://<host>[:port]`). In the web UI use `ctx.api.get(path, {params, signal})`
(see `web/static/js/README.md`).

## Conventions

- **JSON, UTF-8.** Every endpoint answers `application/json` unless stated otherwise. Responses are never cached by the
  browser (`Cache-Control: no-store`) and are gzip-compressed when the client accepts it.
- **Errors:** HTTP status plus `{"error":"message"}`. `400` bad parameter, `401` admin login needed, `403` cross-origin
  change refused, `404` unknown endpoint / object, `409` not possible in the current state, `429` throttled,
  `502` an upstream service (Open-Meteo, NTP, tile host) failed, `503` engine restarting (factory reset), `504` timed out.
- **Units:** timestamps Unix **milliseconds** UTC; altitude ft; speed kt; vertical rate ft/min; distance km; angles
  degrees true, clockwise from north; signal dBFS (negative, 0 = full scale); weather in degC, hPa, km/h, mm, %, m.
  Wind directions are where the wind blows **from**.
- **Unknown values:** numbers that are not known are `null` (types written `number | null`) or the field is omitted
  (marked *optional*). NaN never appears. Arrays are never `null`: an empty result is `[]`.
- **Time range parameters** (wherever "from, to / hours / days" is listed): `from` and `to` in Unix ms, or `hours=N` /
  `days=N` for the span ending now (or at `to`). Without any of them each endpoint uses its documented default span.
- **`ant` parameter:** an antenna ID from `config.antennas[].id`; empty (or `all` / `combined`) means all antennas
  combined. Statistics per antenna exist only for feeds attributed to an antenna (`feed.antennaId`) and in demo mode
  (`vertical`, `yagi`); an antenna without data yields empty results, not an error.
- **Altitude bands** (`band` 0..6): <1000 ft, 1-5k, 5-10k, 10-20k, 20-30k, 30-40k, >=40k ft.
- **Per-bearing arrays** have 180 entries (2 degree bins, centre = `bearingDeg[i]` = 1, 3, 5 ... 359 degrees).
- **Position source codes** (`src` in trail points): 0 ADS-B, 1 MLAT, 2 TIS-B / ADS-R, 3 other. `srcMask` in history rows
  is a bit set: 1 ADS-B, 2 MLAT, 4 TIS-B, 8 other, 16 Mode S without position.
- **Examples** below are real responses, shortened: `/* … N more */` marks omitted array elements.

## Access control

- **Read-only endpoints (all `GET`) are open** to every client that can reach the server, and may be fetched
  cross-origin (`Access-Control-Allow-Origin: *`, no credentials) - the remote shell page and other AirView
  instances use this.
- **Admin endpoints** (everything that changes something; marked *admin*): requests from the machine itself
  (loopback) are always admin. Other clients are admin while no password is configured
  (`config.http.adminPasswordHash` empty); once one is set they must `POST /api/login` first, otherwise the
  endpoint answers `401 {"error":"admin password required"}`. `status.adminLocked` (and `GET /api/session`)
  tell the UI whether this client has to log in.
- **Same-origin only for changes:** a `PUT` / `POST` whose `Origin` header names another host, or that a browser
  marks as cross-site, is refused with `403`. Changes addressed to a public DNS name (not an IP address, a
  single-label or `.local`-style name) are accepted only with a login session (protection against DNS rebinding).
- The session cookie is `airview_session` (HttpOnly, SameSite=Lax, 30 days); sessions live in memory and end on
  restart, logout, factory reset or password change.

## Contents

**Core**

- `GET /api/status` - Application status (`model.Status`)
- `GET /api/config` - The stored configuration (`model.Config`) with secrets blanked
- `PUT /api/config` *(admin)* - Validate, save and apply the configuration live; returns the stored configuration (redacted)
- `POST /api/factory-reset` *(admin)* - Wipe the database and restore the default configuration, then restart the engine
- `POST /api/login` - Admin login
- `POST /api/logout` - End the admin session
- `GET /api/session` - Whether this client is admin and whether a password is configured
- `GET /api/aircraft` - Live aircraft table (`model.LiveSnapshot`), rebuilt once per second
- `GET /api/aircraft/{icao}` - One aircraft: live state, live trail, reference data, aircraft-database record, all-time totals and recent flights
- `GET /api/trails` - In-memory trails of all live aircraft
- `GET /api/events` - Live stream (Server-Sent Events)
- `GET /api/notices` - The most recent notices (at most 50, oldest first) for clients that connect late
- `GET /api/feeds` - Feed connections, per-feed and per-antenna counters, and the data port (hub) with its clients
- `GET /api/tilecache` - Tile cache size and counters
- `POST /api/tilecache/clear` *(admin)* - Delete all cached map tiles
- `GET /api/acas/events` - ACAS (TCAS II) resolution advisories reported by aircraft (DF16, BDS 3,0, TC28), newest first: when, where, which advisories ("Climb", "Do not descend", ...), the threat when reported, and whether the aircraft reported the RA as ended
- `GET /api/acas/status` - Counters of the resolution advisory recorder (events since the start, open events, reports dropped by the flood guard)
- `GET /tilecache/{host}/{path...}` - caching proxy for the whitelisted map tile hosts

**Statistics**

- `GET /api/stats/overview` - Everything a statistics landing page needs: last 24 h totals, sources, downlink formats, all-time totals, today, busiest day/hour, top lists, records, recent events
- `GET /api/stats/rates` - Traffic over time: message rate, positions, aircraft counts, range and signal as parallel arrays, plus totals
- `GET /api/stats/df` - Downlink-format and source mix over a range, plus the live DF / Comm-B register counters since start
- `GET /api/stats/heatmap` - Day-of-week x hour-of-day heat map (7 x 24, Monday first, local time) of aircraft and message rate
- `GET /api/stats/daily` - Daily totals (local days) with the busiest day and hour
- `GET /api/stats/top` - Top operators, countries, aircraft types and airframes
- `GET /api/stats/altitudes` - Altitude distribution: positions per altitude band and a histogram of flight maximum altitudes
- `GET /api/stats/records` - All-time records
- `GET /api/stats/events` - Event log, newest first: emergencies, military / interesting catches, new records, feed up / down
- `GET /api/stats/counters` - Decoder counters since the program started (`tracker.Counters`): frames, CRC failures, duplicates, positions, per-DF, per-antenna and per-feed totals
- `GET /api/stats/range` - Hourly range metrics (`range_hour`): maximum and percentile distances, positions, aircraft, far-reception count, signal residual
- `GET /api/stats/daynight` - Day / twilight / night comparison of range and signal, by local hour, with a significance test

**Antenna & coverage**

- `GET /api/antenna/coverage` - Polar coverage: maximum, robust and typical range per 2 degree bearing bin, overall and per altitude band, against the radio horizon; measured skyline and obstructions
- `GET /api/antenna/coverage.geojson` - Coverage outlines as GeoJSON polygons for the map
- `GET /api/antenna/horizon` - Radio line-of-sight table per antenna and altitude band, and the achieved range against it
- `GET /api/antenna/skyline` - Measured skyline (lowest elevation angle with reception per bearing) and detected obstructions
- `GET /api/antenna/heat` - All-time position heat grid as parallel arrays (cell centre, count, mean RSSI, lowest and highest altitude)
- `GET /api/antenna/signal` - RSSI against distance: 2-D histogram cells, per-distance percentiles and the path-loss fit
- `GET /api/antenna/azimuth` - Measured azimuth gain pattern (relative dB per 2 degree bearing) and the directional lobe fitted to it
- `GET /api/antenna/elevation` - Measured elevation pattern: counts, mean RSSI and distance-corrected residual per 0.5 degree of elevation
- `GET /api/antenna/fit` - Yagi lobe fit: inferred boresight, beamwidth and gain over the omni level, compared with the configured yagi
- `GET /api/antenna/linkbudget` - Predicted (link budget) against achieved range per altitude band
- `GET /api/antenna/pattern` - Configured antennas with their modelled azimuth / elevation patterns on the analysis grid, next to the measured pattern
- `GET /api/antenna/score` - Station score 0..100 with sub-scores and plain-language findings
- `GET /api/antenna/report` - The complete antenna analysis in one document (signal, azimuth, elevation, antennas, yagi, linkBudget, notes)
- `GET /api/antenna/daynight` - Same as `/api/stats/daynight`
- `GET /api/antenna/budget` - Link budget for one aircraft position: path loss, antenna gain, received power, noise floor, SNR, margin
- `GET /api/radio/pattern` - Modelled antenna pattern cuts: azimuth cut at a given elevation, elevation cut at a given azimuth, predicted range around the compass
- `GET /api/radio/transponders` - Transponder power classes used by the link budget
- `GET /api/antenna/heat.jpg` - The all-time position heat map as a JPEG, by default 7680 x 4320 (8K UHD): heat in the Live Map's colours on a plain background with a graticule, the range rings, the receiver, a scale bar and a caption

**Weather**

- `GET /api/weather/current` - Current conditions, poller status, the stored row of the current hour and the radio-propagation state derived from it
- `GET /api/weather/hourly` - Hourly weather rows from the database: observed hours up to now and forecast hours from the current hour on
- `GET /api/weather/profile` - Pressure-level profile of the weather model for one hour with its refractivity analysis (N / M profile, layers, ducts)
- `GET /api/weather/radar` - RainViewer precipitation radar frame index for the map overlay
- `GET /api/weather/impact` - Weather impact on reception: correlations, response curves, ridge regression and ducting episodes
- `GET /api/weather/ducting` - Refractivity / ducting time line: per hour the gradient, k-factor, 0..100 propagation index, class and ducts; plus the inversions aircraft measure right now
- `POST /api/weather/refresh` *(admin)* - Fetch the weather now instead of waiting for the next poll

**Aircraft-derived meteorology**

- `GET /api/met/profile` - Vertical profile of wind and temperature above the station derived from aircraft (Mode S EHS / MRAR), in 1000 ft layers, with its analysis
- `GET /api/met/history` - Hourly time-height history of the aircraft-derived profiles
- `GET /api/met/observations` - Recent single wind / temperature observations for a map layer
- `GET /api/met/winds` - Winds-aloft table at standard flight levels, jet maximum, strongest shear, freezing level and tropopause from the current profile
- `GET /api/met/compare` - Aircraft-derived profile against the weather model (differences are aircraft minus model) and the radio refractivity of measured inversions
- `GET /api/met/stats` - Collector counters and the learned per-airframe heading corrections
- `GET /api/met/grid` - Winds and temperatures aloft on a latitude/longitude grid at one flight level, from the aircraft observations of the last minutes (Mode S EHS BDS 5,0/6,0 derivation and BDS 4,4)
- `GET /api/met/hazards` - BDS 4,5 meteorological hazard reports (turbulence, wind shear, microburst, icing, wake vortex: 0 nil .

**Forecasts**

- `GET /api/forecast/traffic` - Traffic forecast for the next 72 h: aircraft count and message rate with 80 % intervals, busiest and quietest hours
- `GET /api/forecast/reception` - Reception forecast for the next 72 h: p95 / max range, 0..100 index, far-reception probability, atmosphere and drivers per hour
- `GET /api/forecast/weather` - Local weather outlook: bias-corrected hourly forecast, Zambretti, detected patterns (fronts, fog, inversion, convection), analogue forecast
- `GET /api/forecast/outlook` - Digest of all forecast products for a home page: nowcast, the next 12 h of each product, highlights
- `GET /api/forecast/verification` - How good the forecasts are: backtest skill per model, forecast-versus-actual series, what data is still missing
- `GET /api/forecast/nowcast` - One-line reception nowcast for a top bar: index, label, trend, sentence
- `GET /api/forecast/status` - State of the forecaster (last fit, last build, hours of history, last error)
- `POST /api/forecast/refit` *(admin)* - Refit all forecast models now (normally hourly)

**Timing**

- `GET /api/timing/ntp` - NTP reference: configuration, status, newest sample and statistics (last hour, 24 h, everything in memory)
- `GET /api/timing/history` - NTP offset / delay history
- `GET /api/timing/latency` - Per-feed latency between the receiver's GPS frame timestamps and the (NTP-corrected) host clock: percentiles, histogram, per-minute history; plus the latency-chain summary and stage histograms
- `GET /api/timing/latency/aircraft` - Per-aircraft latency table (aircraft with positions, most positions first): transponder latency of T=1 positions, estimated position age, age jitter, propagation, flags
- `GET /api/timing/latency/aircraft/{icao}` - One aircraft's latency row and the stage waterfall of its latest position message
- `GET /api/timing/latency/feeds/{id}` - Receiver-delay minute series of one feed (up to 24 h, restored from the database after a restart)
- `GET /api/timing/ntpcheck` - NTP / receiver GPS clock consistency: NTP server quality, offset uncertainty, per-feed receiver-delay clock filter (10 s minima, floor, jitter, drift), issues and an explanation
- `GET /api/timing/receiver` - Receiver timing state: GPS-timestamp mode and status text per feed, receiver GPS position, stored status frames
- `POST /api/timing/ntp/sample` *(admin)* - Take an NTP measurement now with the configured server, or test another server once without saving it

**Location guesser**

- `GET /api/guess` - Receiver location estimated from the received positions alone (four methods and their fusion)
- `POST /api/guess` *(admin)* - Run the location guesser now on the newest data (flushes the statistics first) and return the result

**History**

- `GET /api/history/aircraft` - Every airframe ever seen: search, sort, paging
- `GET /api/history/aircraft/{icao}` - One airframe: totals, reference data, its flights (newest first) and its events
- `GET /api/history/flights` - Flights (contact sessions), newest first
- `GET /api/history/flights/{id}` - One flight with its stored trail

**Database**

- `GET /api/db/stats` - Database size, writer counters, row counts per table, retention settings, aircraft database and tile cache state
- `POST /api/db/vacuum` *(admin)* - Compact the database file (blocks writes while it runs; can take minutes on a Raspberry Pi)
- `POST /api/db/prune` *(admin)* - Apply the retention settings now
- `POST /api/db/checkpoint` *(admin)* - Flush the in-memory statistics and checkpoint the write-ahead log
- `POST /api/db/rollup` *(admin)* - Roll minute statistics up into hours now (normally 3 min after each hour)
- `GET /api/db/acdb/sources` - Downloadable aircraft databases (with licence and attribution), what is installed, and the import state
- `POST /api/db/acdb/import` *(admin)* - Start downloading and importing an aircraft database in the background
- `POST /api/db/acdb/upload` *(admin)* - Import an aircraft database file sent as the raw request body (tar1090-db or OpenSky CSV, optionally gzip; at most 600 MB)
- `POST /api/db/acdb/cancel` *(admin)* - Cancel a running import
- `GET /api/db/acdb/progress` - State of the current or last aircraft database import
- `GET /api/db/acdb/lookup` - Look one address up in the aircraft database and the built-in tables
- `GET /api/db/export` - Tables available for CSV export with their row counts
- `GET /api/db/export/{file}` - Download one table as CSV (`{file}` = `<table>.csv`), streamed

**Geo, astronomy & reference data**

- `GET /api/geo/parse` - Parse pasted coordinates in almost any notation into latitude / longitude
- `GET /api/geo/format` - Format a position in the common notations
- `GET /api/geo/elevation` - Terrain elevation at a position (Open-Meteo elevation service; needs the internet)
- `GET /api/geo/magvar` - Magnetic declination and field (World Magnetic Model 2025) at a position, now
- `GET /api/astro/now` - Sun position, day phase and today's sun events at the receiver, the day/night terminator rings and the moon phase
- `GET /api/astro/terminator.geojson` - The night side of the earth as a GeoJSON FeatureCollection of four polygons (properties `kind`, `sunAlt`)
- `GET /api/astro/days` - Sunrise, sunset and twilight times for the coming days
- `GET /api/refdata/icao/{icao}` - What the built-in tables know about an ICAO address: state of registry, allocation block, derived registration, military heuristic
- `GET /api/refdata/callsign/{callsign}` - Interpret a callsign: operator, telephony name, flight number or registration
- `GET /api/refdata/type/{code}` - ICAO aircraft type designator: manufacturer, model, class, wake category
- `GET /api/refdata/squawk/{code}` - Conventional meaning of a Mode A code

**System**

- `GET /api/system` - Process and host information: version, platform, uptime, goroutines, memory, data directory, restart-required flag
- `GET /api/ssr` - EXPERIMENTAL: state of the SSR self-location (enabled, feed, counters, estimator statistics incl
- `POST /api/ssr/solve` *(admin)* - EXPERIMENTAL: run an SSR self-location solve now (about 0.3 s; poll `GET /api/ssr`)
- `GET /api/update` - Self-update state: running version, channel, automatic updates on/off, last check and its error, the newer version available, the last update transaction, and whether this installation can install updates itself
- `POST /api/update/check` *(admin)* - Check the release manifest now (signature verified; nothing is installed)
- `POST /api/update/install` *(admin)* - Download, verify and install the newer version, then restart into it

**Public network (aggregator mode)**

- `GET /feeder/install.sh` - The Raspberry Pi feeder installer with this network's presets (feeder host and port, AVF certificate pin, web address, release key)
- `GET /feeder/manifest.json` - The signed airview-feeder release manifest from `aggregator.feederReleaseDir` (404 when none is configured)
- `GET /feeder/manifest.json.sig` - The Ed25519 signature of manifest.json (base64)
- `GET /feeder/dl/{target}` - One airview-feeder release file (`dl/<target>` in the release directory): `linux-arm64`, `linux-armv7`, `linux-amd64`, `windows-amd64`, `windows-arm64`, ..
- `GET /feeder/beta/manifest.json` - The beta channel's signed manifest (`beta/` in the release directory)
- `GET /feeder/beta/manifest.json.sig` - The beta manifest's signature
- `GET /feeder/beta/dl/{target}` - One beta release file
- `GET /api/share/{token}` - What a share link shows: label, scope, number of stations, expiry and the network name (no station data)
- `GET /api/share/{token}/aircraft` - The aircraft the link's stations heard in the last 60 s (`publicdto.Snapshot`: no receiver data; low aircraft on the island-guard grid when the link says so)
- `GET /api/share/{token}/stats` - Non-locating statistics of the link's stations (scope `map+stats` only)
- `GET /healthz` - Health check for load balancers, service managers and monitors
- `GET /api/public/status` - Public application status: version, mode, network name, live counters and which public surfaces are enabled
- `GET /api/public/aircraft` - The merged live picture without receiver data (`publicdto.Snapshot`)
- `GET /api/public/aircraft/{icao}` - One aircraft of the public picture: live state, live trail without signal levels, reference data
- `GET /api/public/trails` - Live trails of the public picture (signal column null)
- `GET /api/public/events` - The public live stream (Server-Sent Events): `aircraft` (`publicdto.Snapshot`), `status` (`publicdto.Status`) and public `notice` events
- `POST /api/events/view` - Change the viewport of a live stream opened with `bbox` or `view=1` (no new keyframe)
- `GET /api/public/stations` - Stations that opted in to a public pin or public statistics (`publicdto.StationList`)
- `GET /api/public/summary` - The network landing page in numbers (`publicdto.Summary`): station counts, aircraft, MLAT aircraft, message rate
- `GET /api/public/stats` - The network in numbers: approved and online stations and the live counters
- `GET /api/public/mlat` - The network's empirical MLAT accuracy over the last hour (hold-out aircraft against their ADS-B reference) and its 1° cells (`publicdto.MLATCoverage`)
- `GET /api/public/join` - What a new feeder needs to join this network: host name, open listener ports (AVF/1, Beast, mlat-client), admission and registration policy, the backup retention named in the deletion notice and the privacy-notice link
- `GET /data/aircraft.json` - The public picture in the tar1090 / readsb `aircraft.json` shape
- `GET /data/receiver.json` - tar1090 `receiver.json`

**My stations (aggregator mode)**

- `POST /api/me/stations/{sid}/location/pending/confirm` - Apply the position the station's feeder announced and the aggregator holds (a large move waits for the survey or this confirmation; finding PRIV-1)
- `PUT /api/me/stations/{sid}/mlat/survey` - Allow or reject the automatic use of the station's survey position for MLAT (DESIGN §19.3)
- `GET /api/me/stations/{sid}/notices` - Location notices of the station (last 30 days, newest first): finished surveys, declared-position checks, the automatic use of the survey position for MLAT and its owner override, confirmations
- `GET /api/me/notices` - Location notices of all the viewer's stations (last 14 days, at most 50, newest first)
- `GET /api/me/stations/{sid}/minutes` - The station's last 16 minutes of feeder statistics (AVF/1 MINUTE_STATS merged with what the server received): exact, received, merged and shed counts, selective mode
- `GET /api/me/shares` - The account's share links (no secrets): label, stations, scope, state, expiry, last use and use count, plus the location warning
- `POST /api/me/shares` - Create a share link for own stations (read-only live view)
- `DELETE /api/me/shares/{id}` - Revoke an own share link (it stops working at once)
- `GET /api/me/stations/{sid}/survey` - The station's reverse-MLAT survey: the running job, the last results (position, σ, 95 % ellipse, stable/suspect, declared-position check, MSL/HAE verdict) and the next scheduled check
- `POST /api/me/stations/{sid}/survey` - Ask for a survey of the station (at most one per hour and station)
- `GET /api/me/stations/{sid}/locate` - Locate Me v2 for a station without a location: the approximate position estimated from what the station receives (radio horizon, skyline, signal level), with its 95 % circle and the error against the last survey
- `GET /api/me` - The logged-in account and its stations (exact data)
- `GET /api/me/stations` - The own stations with exact location, privacy settings, admission and live state
- `GET /api/me/stations/{sid}` - One own station: exact data, location estimates, the public pin preview (exactly as others see it), live links and statistics
- `POST /api/me/stations/{sid}` - Change an own station's name, privacy (pin precision, show name, public statistics) or declared position
- `PUT /api/me/stations/{sid}/privacy` - Set an own station's public pin precision, name display and public statistics
- `PUT /api/me/stations/{sid}/name` - Rename an own station
- `PUT /api/me/stations/{sid}/location` - Declare an own station's antenna position (cross-checked by the survey); `{"clear":true}` removes it
- `POST /api/me/stations/{sid}/location/confirm` - Use a location estimate (survey, GNSS) as the station's position for MLAT (owner confirmation, DESIGN §10.1)
- `POST /api/me/stations/{sid}/unclaim` - Release an own station (it keeps feeding; it can be claimed again)
- `GET /api/me/stations/{sid}/stats` - Live statistics of an own station: frames, positions, aircraft this minute, late frames, live links, last seen
- `GET /api/me/stations/{sid}/mlat` - Timing and MLAT of an own station: position source used for MLAT, clock state (class, anchor state, σθ, drift, short-frame offset, jitter, steps, events), sync peers, MLAT contributions and residuals, timed frames per minute and result eligibility
- `GET /api/me/stations/{sid}/mlat/longterm` - History of an own station's MLAT participation over days, one point per hour: timed frames, groups, fixes and outliers per minute, synced peers, synced share, σθ, residual RMS and drift
- `GET /api/me/stations/{sid}/connections` - Connection log of an own station (newest first; addresses only as daily-salted hashes)
- `GET /api/me/aircraft` - The live picture for an owner: `mine` and `stations` (own SIDs) per aircraft, geometry for the selected own station
- `GET /data/me/aircraft.json` - tar1090 `aircraft.json` of the aircraft the own stations heard in the last 60 s (session-scoped)
- `GET /api/me/check` - Feed Check: the live feeder links coming from the visitor's own address (exact IPv4, or the same IPv6 /64)
- `POST /api/me/claim` - Claim a station: a native feeder with its claim code, a compat feeder with its full UUID from the feeder's own address
- `GET /api/me/export` - Export the account's data (JSON files in a zip)
- `DELETE /api/me/account` - Delete the own account and its stations' data now; the session ends

**Network administration (aggregator mode)**

- `POST /api/admin/stations/{sid}/timing` *(admin)* - Override a station's timing quarantine: `trusted` (joins MLAT now), `excluded` (never), `quarantine` (judge again), `auto` (clear overrides)
- `GET /api/admin/mlat/quarantine` - Stations in timing quarantine with the gate's progress per station, and the gate counters
- `GET /api/admin/mlat/crlb/improve` - Where one more station would improve MLAT most: candidate sites ranked by the CRLB gain (area-weighted log improvement of σ_h), with newly covered and newly grade-A/B cells, plus the gain of every candidate cell for the map
- `GET /api/admin/mlat/surveys` - Survey console: every approved station's location status, MLAT position source, running survey and last result (no coordinates), plus the network's location quality summary
- `GET /api/admin/stations/{sid}/minutes` - A station's last 16 minutes of feeder statistics (as the owner sees them)
- `GET /api/admin/shares` - Every share link of the network (no secrets) with its owner, stations and use
- `DELETE /api/admin/shares/{id}` *(admin)* - Revoke any share link
- `GET /api/admin/stations/{sid}/survey` - A station's reverse-MLAT survey status (as the owner sees it)
- `POST /api/admin/stations/{sid}/survey` *(admin)* - Start a survey of a station (no rate limit)
- `DELETE /api/admin/stations/{sid}/survey` *(admin)* - Cancel a station's running or queued survey
- `GET /api/admin/mlat/survey` - Survey counters: stations known, running and queued jobs, finished jobs by outcome, tap counters
- `GET /api/admin/mlat/verification` - ADS-B position verification: counters (confirmed, contradicted, suspects, contradictions charged to a station, contradictions muted by the network-wide breaker, recovered clock-filter and engine panics), the aircraft whose broadcast positions are suspect now, and the last 200 transitions
- `GET /api/admin/federation` - Federation: node ID, TLS pin, peers (state, direction, RTT, counters, last error), received aircraft and out-feeds
- `GET /api/admin/mlat/modeac` - Mode A/C multilateration: counters and the shown targets (synthetic id, squawk, Mode C altitude, position, grade, σ)
- `GET /api/admin/stations` - The station registry with exact positions and owners: `{"stations":[...],"total":n,"counts":{...}}`
- `GET /api/admin/stations/{sid}` - One station with exact data, estimates, live links (with addresses) and statistics
- `POST /api/admin/stations/{sid}` *(admin)* - Change a station: admission, owner, name, trust, anchor, never-anchor flag
- `PUT /api/admin/stations/{sid}` *(admin)* - Edit a station (same body as the POST)
- `POST /api/admin/stations/{sid}/admission` *(admin)* - Approve, hold, deny or revoke a station
- `POST /api/admin/stations/{sid}/reveal` *(admin)* - A station's exact position and current address (audited `station.reveal`)
- `DELETE /api/admin/stations/{sid}` *(admin)* - Delete a station and its data (queued; survives restarts)
- `POST /api/admin/feeders/{id}/revoke` *(admin)* - Revoke a native feeder key (all its stations)
- `GET /api/admin/tokens` - Enrol tokens (without secrets)
- `POST /api/admin/tokens` *(admin)* - Mint an enrol token; the secret is shown once
- `DELETE /api/admin/tokens/{id}` *(admin)* - Revoke an enrol token
- `GET /api/admin/audit` - Audit log, newest first
- `POST /api/admin/retention` *(admin)* - Run the retention jobs now; returns their report
- `GET /api/admin/links` - Every live feeder link of every listener, with addresses
- `GET /api/admin/ingest/beast` - The Beast listener (30004): totals and every link
- `POST /api/admin/ingest/beast/{link}/disconnect` *(admin)* - Close one Beast link
- `GET /api/admin/network` - Operator view of the aggregator: listeners (ports, addresses, errors), ingest statistics, tracker lock statistics, lanes, flight_station rows
- `GET /api/admin/mlat` - MLAT engine counters: every stage (groups, clusters, solves, publishes, drops), clock filter, reference store, tracker, planner, k_atm
- `GET /api/admin/mlat/quality` - MLAT self-validation: hold-out error (fix and track), σ calibration, NEES, week-on-week alarm
- `GET /api/admin/mlat/history` - MLAT console history, one sample per minute for up to 24 h: hold-out error (fix and track N, P50, P95, cross-track P95, share within 1σ over the preceding 10 minutes), stage rates per minute (groups, clusters, solves, accepted, rejected, published, results, drops), clock filter (stations, synced, components, epoch cost), worker load
- `GET /api/admin/mlat/longterm` - MLAT network history over days, one point per hour from the persisted minute tables: frames, groups, fixes and outliers per minute, synced stations, and the hold-out error per hour (N, P50, P95 from the summed error histograms of every cell)
- `GET /api/admin/mlat/stations` - Per-station clock quality and MLAT residuals for every station the MLAT engine knows (class, anchor state, synced, σθ, drift, β short, jitter, steps, peers, contributions, used / excluded / outlier %, residual bias, σ scale, bias flag, timed frames per minute)
- `GET /api/admin/mlat/sync` - Clock-sync matrix: per station and peer (SIDs) co-observations in the last 30 s, RMS residual, relative frequency, same clock domain
- `GET /api/admin/mlat/crlb` - Expected horizontal MLAT σ (Cramér-Rao bound) at 3000 ft, FL100 and FL300 on a 0.25° grid from the located stations and their timing
- `GET /api/admin/stations/{sid}/mlat` - Timing and MLAT of any station (as the owner view, peers by SID)
- `GET /api/admin/aircraft/{icao}` - One aircraft with the full receiver list (every station that heard it in the last 60 s)

**Routes**

- `GET /api/routes/{callsign}` - Full route view of a callsign: every hypothesis with its sources, fused probability and live score, the shown route (active leg, state, confidence, progress, ETA), the great-circle path and the learned history
- `GET /api/routes/status` - Resolver counters, the online budget (used / daily cap / back-off), invalid rows of the overrides file and the attribution lines
- `GET /api/routes/overrides` - Every route override (`locked` rows come from `routes.overrides.csv` in the data directory and can only be changed there)
- `PUT /api/routes/overrides/{callsign}` *(admin)* - Create or replace the override of a callsign
- `DELETE /api/routes/overrides/{callsign}` *(admin)* - Delete the override of a callsign
- `GET /api/routes/overrides-log` - Audit log of override changes, newest first
- `POST /api/routes/refresh` *(admin)* - Check the reference data (airports, routes, ...) for updates now (conditional GET: unchanged files are not downloaded)
- `GET /api/routes/export/{file}` - Exports: `learned.csv` (learned routes with P >= 0.9 seen on >= 3 days, in the VRS standing-data format, released CC0 for submission to the VRS standing data), `overrides.csv` (round-trips with `routes.overrides.csv`), `legs.csv` (observed legs; `from`/`to` Unix s)
- `GET /api/routes/board/{airport}` - Arrivals, departures and recently landed flights of an airport, built from the live picture and the routes resolved for it (not a schedule)
- `GET /api/routes/airports` - Airport references (ICAO / IATA code, name, position, elevation, size) for client-side route drawing and the IATA/ICAO alias map
- `GET /api/routes/batch` - Route shorts of the aircraft in the caller's live picture with the given callsigns (for lists and filters)
- `GET /api/routes/stats` - Statistics of the legs this station observed: busiest `routes` (observed departure-arrival pairs), `airports` (departures + arrivals), `airlines` (callsign prefix), or `coverage` (legs by the source of the route shown, and the share with both ends observed)
- `GET /api/routes/legs` - Search the observed legs, newest first
- `GET /api/routes/providers` - Online route providers: enabled, licence class, budget used today, daily cap, back-off and what is sent

**Accounts (aggregator mode)**

- `GET /api/auth/setup` - Whether the first admin account still has to be set up
- `POST /api/auth/setup/begin` - First admin setup, step 1: console token, user name and password; returns the authenticator secret
- `POST /api/auth/setup/finish` - First admin setup, step 2: confirm the authenticator with a code; returns the recovery codes once
- `POST /api/auth/register` - Create an owner account (no e-mail) and log in
- `POST /api/auth/password` - Change the own password; other sessions of the account end
- `POST /api/auth/totp/begin` - Start authenticator (TOTP) enrolment; returns the secret and otpauth URI
- `POST /api/auth/totp/confirm` - Confirm the authenticator with a first code; returns 10 single-use recovery codes (shown once)
- `POST /api/auth/totp/disable` - Turn the authenticator off (owners only; admins cannot)
- `POST /api/auth/recovery-codes` - Replace all recovery codes
- `GET /api/auth/sessions` - The own login sessions (no addresses are stored)
- `POST /api/auth/logout-others` - End every other session of the own account

**Prediction**

- `GET /api/prediction/live` - Predicted position now of every aircraft the tracking filter follows (IMM filter, wind aware)
- `GET /api/prediction/stats` - Tracking filter counters and self-scored prediction quality per horizon and flight phase
- `GET /api/prediction/path/{icao}` - Predicted path of one aircraft with growing uncertainty ellipses, and when terrain will hide it
- `GET /api/prediction/learned` - The learned traffic-flow predictor: learned cells, aircraft followed, and its self-check against dead reckoning at 30 and 60 s (mean errors, share of predictions where it was closer, the blend weight it corrected itself to)
- `GET /api/prediction/intent/{icao}` - Intent analysis of one aircraft: phase, holding, approach, closest approach to the station, level-off
- `GET /api/prediction/proximity` - Predicted close approaches between airborne aircraft (informational only, not an ATC/TCAS function)

**Terrain**

- `GET /api/terrain/status` - Terrain analysis job (download / sampling / coverage progress), result availability, tile cache
- `POST /api/terrain/prefetch` *(admin)* - Download the elevation tiles and run the terrain analysis now (also when automatic prefetching is off)
- `POST /api/terrain/recompute` *(admin)* - Re-run the last terrain analysis ignoring caches (also resets the download back-off)
- `POST /api/terrain/cancel` *(admin)* - Cancel a running terrain job (the previous result stays)
- `GET /api/terrain/coverage` - The complete terrain analysis: per antenna the horizon panorama, line-of-sight and radio coverage rings per altitude, advice
- `GET /api/terrain/coverage.geojson` - Theoretical coverage per altitude as GeoJSON polygons for the map
- `GET /api/terrain/horizon` - Horizon panorama of one antenna: terrain mask angle, distance and height of the skyline per bearing
- `GET /api/terrain/panorama` - Same as `/api/terrain/horizon`
- `GET /api/terrain/advice` - Antenna site advice: coverage gain of a higher mast, worst sectors with the limiting ridge, ready-made sentences
- `GET /api/terrain/floor` - Coverage floor grid: lowest altitude visible from the antenna per cell
- `GET /api/terrain/floor-image` - Coverage floor as a map image: URL, corner coordinates and legend for a MapLibre `image` source
- `GET /api/terrain/floor.png` - The coverage floor PNG (Web Mercator rows, transparent where nothing is visible)
- `GET /api/terrain/visible` - Is a point above the terrain radio horizon? (~100 ns, from the panorama.)
- `GET /api/terrain/los` - Line of sight from the antenna to one point: obstruction, Fresnel clearance, diffraction loss and the terrain profile
- `GET /api/terrain/profile` - Same as `/api/terrain/los` with `alt` as the altitude parameter, for the path profile chart
- `GET /api/terrain/elevation` - Terrain elevation at a point from the elevation tiles (downloads the tile when missing)

**RF statistics**

- `GET /api/rf/summary` - Headline RF numbers: health score, frame rate, channel occupancy, noise / decode floor, detection range, radars, GNSS index, advice (`advice[]` sentences and `adviceItems[]` `{code,text,data}`)
- `GET /api/rf/report` - Everything below in one document (`rfstat.Report`)
- `GET /api/rf/signal` - Signal levels: RSSI histogram, decode floor, noise-floor estimate, overload share, per antenna / range ring / elevation, fading, multipath, aspect
- `GET /api/rf/detection` - Reception probability of position / velocity / identification squitters versus distance, elevation, bearing and RSSI; 50 % / 90 % range; update gaps
- `GET /api/rf/intervals` - Position update intervals per range band: percentiles and the probability that a position is at most T seconds old
- `GET /api/rf/channel` - 1090 MHz channel: frames per DF, occupancy, CRC failures, garble model, load curve, bursts, last 5 minutes per second
- `GET /api/rf/interrogators` - Interrogator census from DF11 replies (II / SI codes, rates, lock-out), surveillance reply rates, Comm-B mix, ACAS activity
- `GET /api/rf/timing` - Squitter timing from the receiver's GPS timestamps: interval histograms per type, even/odd alternation, non-compliant transmitters
- `GET /api/rf/fleet` - Fleet / equipage census: ADS-B versions, NIC / NACp / SIL, poor integrity share, emitter categories, TC28/29/31 use, GNSS vs baro altitude, GNSS interference
- `GET /api/rf/gnss` - GNSS interference index: share of ADS-B aircraft with degraded navigation integrity, per minute (24 h) and per polar map cell
- `GET /api/rf/health` - RF health: score 0..100 (-1 = no data), scored items with plain-language text, blocked sectors, advice
- `GET /api/rf/history` - Hourly RF summaries (frame rate, occupancy, CRC failures, Pd, 90 % range, health ...), oldest first

**Navigation data**

- `GET /api/navdata/status` - Imported reference datasets (rows, import time, source, licence), downloadable sources, import state, options and attribution
- `POST /api/navdata/import` *(admin)* - Start downloading and importing reference datasets in the background
- `GET /api/navdata/progress` - State of the current or last import (`navdata.ImportStatus`): `running`, `progress{dataset, stage, bytesRead, bytesTotal, records, index, count}`, `fraction` 0..1, `result`, `error`
- `POST /api/navdata/cancel` *(admin)* - Cancel a running import
- `POST /api/navdata/route-cache/clear` *(admin)* - Forget the cached online route look-ups
- `GET /api/navdata/airports` - Airports in a box or around a point, largest first; optionally with runways or as a GeoJSON map overlay
- `GET /api/navdata/search` - Search airports by code or name
- `GET /api/navdata/navaids` - Radio navigation aids (VOR, DME, NDB, TACAN) in a box
- `GET /api/navdata/airport/{code}` - One airport by ICAO, IATA, ident or GPS code, with its runways and usable landing directions
- `GET /api/navdata/route/{callsign}` - Route of a callsign (standing data, then the opt-in online look-up) with airports, great-circle path and a plausibility check
- `GET /api/navdata/photo/{icao}` - Aircraft photo metadata from planespotters.net (opt-in): thumbnail, large image, link and photographer

**Alerts**

- `GET /api/alerts/rules` - All alert rules (built-in defaults and user rules), sorted by id
- `POST /api/alerts/rules` *(admin)* - Create a rule
- `GET /api/alerts/rules/{id}` - One rule
- `PUT /api/alerts/rules/{id}` *(admin)* - Replace a rule (built-in rules may be edited; reset-defaults restores them)
- `DELETE /api/alerts/rules/{id}` *(admin)* - Delete a rule
- `POST /api/alerts/rules/{id}/enabled` *(admin)* - Switch a rule on or off
- `POST /api/alerts/rules/reset-defaults` *(admin)* - Restore the built-in rules to their shipped state (user rules are kept)
- `POST /api/alerts/test` *(admin)* - Test a rule against the current live picture without firing anything
- `GET /api/alerts/recent` - Recently fired alerts, newest first (in-memory ring of 500; logged ones are also in `/api/stats/events?kind=alert`)
- `GET /api/alerts/highlights` - Aircraft currently highlighted by a rule (also in the live picture as `aircraft[].highlight`)
- `GET /api/alerts/stats` - Alert engine counters: rules, evaluations, last evaluation time, fired / suppressed, webhook sent / failed / dropped, range baseline
- `GET /api/alerts/placeholders` - Help text listing the message template placeholders (`{ident}`, `{alt}`, `{dist}` ...)
- `POST /api/alerts/webhook-test` *(admin)* - Send a test POST to a webhook URL
- `GET /api/alerts/export` - All rules as a JSON array (download)
- `POST /api/alerts/import` *(admin)* - Import rules (all-or-nothing validation; rules with an existing id are replaced)
- `GET /api/alerts/watchlists` - Watch-lists with their number of entries (`[{name, entries}]`)
- `GET /api/alerts/watchlists/{name}` - The entries of one watch-list, or a download
- `POST /api/alerts/watchlists/{name}/import` *(admin)* - Import ICAO addresses into a watch-list (created if needed)
- `PUT /api/alerts/watchlists/{name}/{icao}` *(admin)* - Add (or update the note of) one address
- `DELETE /api/alerts/watchlists/{name}/{icao}` *(admin)* - Remove one address
- `DELETE /api/alerts/watchlists/{name}` *(admin)* - Delete a watch-list

**Replay, exports & reports**

- `GET /api/replay` - History replay: time-sliced pictures of all stored positions in a window, in compact arrays (tar1090 / VRS style)
- `GET /api/history/flights/{id}/trace` - Full stored trail of one flight with track angles, and the guessed departure / arrival airports
- `GET /api/export/flight/{file}` - Download one flight: `{id}.kml` (Google Earth, absolute altitudes), `{id}.geojson` (LineString [lon, lat, altM] with `properties.times`) or `{id}.csv`
- `GET /api/export/live.kml` - The current picture with trails as KML
- `GET /api/export/live.geojson` - The current picture with trails as GeoJSON
- `GET /api/export/aircraft.csv` - Every airframe ever seen as CSV (streamed)
- `GET /api/reports` - Report query (VirtualRadar style): flights in a time range filtered by callsign, registration, address, type, operator, country, military, distance, altitude, departure / arrival airport; paged and sorted
- `GET /api/reports.csv` - The same report as a CSV download (streamed)

**Uplinks**

- `GET /api/uplinks` - Uplinks of this station to aggregators: feeder ID, UUID and per-uplink state (no secrets)
- `POST /api/uplinks` *(admin)* - Uplink actions: `{"action":"claim","id":"...","rxNo":0}` returns a claim code (shown once), `{"action":"retry","id":"..."}` restarts a stopped uplink


## Core

### `GET /api/status`

Application status (`model.Status`).

`adminLocked` is computed per client: true when this client has to log in before using an admin endpoint. `located`, `mode` and `demo` are the *effective* values (in demo mode `located` is true although `GET /api/config` may still say `receiver.located=false`). `tileCacheHosts` is present only while the tile cache is enabled. `station` is the effective station position every analysis uses: `{lat, lon, source}` with source `config` (located receiver; in demo mode the simulator site), `gps` (no located receiver, but the receiver reports its own GPS position, Beast type-5 frames; same as `receiverGps`) or `none` (lat / lon null).

Response: [model.Status](#t-model-Status)

Example: `GET /api/status`

```jsonc
{
  "app": "AirView",
  "version": "test",
  "platform": "linux/amd64",
  "gui": false,
  "now": 1795821896536,
  "started": 1795821863046,
  "mode": "standalone",
  "httpPort": 42025,
  "located": true,
  "demo": true,
  "tileCache": true,
  "feeds": [
    {
      "id": "beast",
      "name": "Local Beast (readsb/dump1090 :30005)",
      "type": "beast",
      "addr": "localhost:30005",
      "enabled": false,
      "connected": false,
      "since": 1795821863059,
      "frames": 0,
      "bytes": 0,
      "msgRate": 0,
      "lastFrame": 0,
      "gpsTime": false
    },
    /* … 1 more */
  ],
  "counts": {
    "aircraft": 12,
    "withPos": 11,
    "adsb": 9,
    "mlat": 3,
    "tisb": 0,
    "modes": 0,
    "msgRate": 54.7,
    "posRate": 10.7,
    "maxRangeKm": 340.3,
    "predicted": 0
  },
  "dbBytes": 28024832,
  "dataDir": "/var/lib/airview",
  "ntp": {
    "enabled": false,
    "server": "",
    "synced": false,
    "offsetMs": 0,
    "delayMs": 0,
    "jitterMs": 0,
    "stratum": 0,
    "lastOk": 0
  },
  "hub": {
    "enabled": false,
    "port": 30154,
    "clients": 0
  },
  "adminLocked": false,
  "station": {
    "lat": 50.05,
    "lon": 8.6,
    "source": "config"
  },
  "tileCacheHosts": ["tiles.openfreemap.org", "tile.openstreetmap.org", "tilecache.rainviewer.com"]
}
```

### `GET /api/config`

The stored configuration (`model.Config`) with secrets blanked.

`http.adminPasswordHash`, `hub.password` and `remote.password` are `""` (not set) or `"set"`. For the admin, settings fixed by the operator config file, an `AIRVIEW_*` environment variable or a `-set` flag are listed in an extra member `"locked":[{"path":"http.port","origin":"env AIRVIEW_HTTP_PORT"}]` (present only when there are any; values are never shown). Changes to locked settings are saved but the override stays in force.

Response: [model.Config](#t-model-Config)

Example: `GET /api/config`

```jsonc
{
  "version": 1,
  "mode": "standalone",
  "http": {
    "port": 80,
    "bindAddr": "",
    "adminPasswordHash": "",
    "tileCache": true
  },
  "receiver": {
    "name": "My Station",
    "model": "",
    "located": false,
    "lat": 0,
    "lon": 0,
    "groundElevM": 0
  },
  "antennas": [
    {
      "id": "vertical",
      "name": "Vertical",
      "type": "vertical",
      "channel": 0,
      "gainDbi": 5,
      "heightAglM": 8,
      "azimuthDeg": 0,
      "tiltDeg": 0,
      "hBeamwidthDeg": 0,
      "vBeamwidthDeg": 30,
      "frontToBackDb": 0,
      "elements": 0,
      "polarization": "vertical",
      "cableLossDb": 0,
      "lnaGainDb": 0,
      "lnaNoiseFigDb": 0,
      "filterLossDb": 0,
      "notes": "",
      "color": "#38bdf8"
    }
  ],
  "feeds": [
    {
      "id": "beast",
      "name": "Local Beast (readsb/dump1090 :30005)",
      "enabled": true,
      "type": "beast",
      "host": "localhost",
      "port": 30005,
      "url": "",
      "antennaId": "",
      "mlatOnly": false,
      "notes": ""
    }
  ],
  "ntp": {
    "enabled": false,
    "server": "",
    "intervalSec": 64
  },
  "weather": {
    "enabled": true,
    "intervalMin": 15,
    "radarOverlay": false
  },
  "hub": {
    "enabled": false,
    "port": 30154,
    "password": ""
  },
  "remote": {
    "host": "",
    "dataPort": 30154,
    "httpPort": 80,
    "password": "",
    "useHostReceiver": true
  },
  "storage": {
    "trailIntervalSec": 5,
    "trailRetentionDays": 30,
    "flightRetentionDays": 730,
    "minuteRetentionDays": 90
  },
  "ui": {
    "units": "aviation",
    "trailMinutes": 15,
    "rangeRingsKm": [100, 200, 300, 400],
    "aircraftTtlS": 60,
    "mapStyle": "airview-dark",
    "colorTrailsBy": "altitude"
  },
  "demo": {
    "enabled": false,
    "aircraft": 60
  },
  "prediction": {
    "enabled": true,
    "coastSec": 60,
    "pathSec": 120,
    "showPaths": true
  },
  "terrain": {
    "enabled": true,
    "radiusKm": 450,
    "zoom": 0,
    "kFactor": 1.3333333333333333,
    "autoPrefetch": true
  },
  "navData": {
    "airportsOverlay": true,
    "autoRefresh": true,
    "photos": false,
    "photoContact": "",
    "routes": {
      "enabled": true,
      "online": true,
      "dailyCap": 1000,
      "stableSec": 45,
      "contact": "",
      "learn": true,
      "eta": true
    }
  },
  "alerts": {
    "enabled": true,
    "sound": true,
    "webhooks": true
  },
  "rfStat": {
    "enabled": true
  },
  "uplinkTuning": {
    "ringSec": 600,
    "ringMb": 12,
    "backfillQueueMb": 24,
    "resumeWindowSec": 30,
    "backfillMinKBps": 64,
    "backoffMinSec": 1,
    "backoffMaxSec": 60,
    "livenessSec": 30,
    "spoolSegmentMb": 4
  },
  "aggregator": {
    "name": "AirView Network",
    "publicHost": "",
    "privacyNoticeUrl": "",
    "avfPort": 30160,
    "beastPort": 30004,
    "mlatPort": 31090,
    "sbsInLocalPort": 0,
    "beastOutPort": 0,
    "sbsOutPort": 0,
    "outputToken": "",
    "admission": "invite",
    "tlsCert": "",
    "tlsKey": "",
    "publicMap": true,
    "publicStats": true,
    "tarJson": true,
    "defaultPin": "hidden",
    "islandGuard": false,
    "listenAddr": "",
    "feederReleaseDir": "",
    "feederManifestKey": "",
    "mlat": {
      "enabled": true,
      "returnResults": true,
      "holdout": 0.1,
      "autoUseSurvey": true,
      "autoUseMaxSigmaM": 50,
      "epochMs": 500,
      "maxComponentStations": 200,
      "modeAc": false,
      "udp": false,
      "surveyIntervalH": 6,
      "refractivityN0": 315,
      "refractivityScaleHeightM": 7000,
      "ref": {
        "window": 0,
        "qHorizontal": 0,
        "qVertical": 0,
        "minNacp": 0,
        "minNic": 0,
        "maxPosAgeS": 0,
        "latencySigmaS": 0,
        "gateChi2": 0,
        "gateFloorM": 0,
        "probationS": 0,
        "suspendS": 0,
        "trackTimeoutS": 0,
        "altReplyMinS": 0,
        "pairBaroS": 0,
        "noAltReplies": false,
        "field": {
          "cellKm": 0,
          "bandFt": 0,
          "driftMPerH": 0,
          "obsSigmaM": 0,
          "profileSigmaM": 0,
          "profileSigmaTK": 0
        }
      },
      "group": {
        "nSigma": 0,
        "slackM": 0,
        "hintSlackM": 0,
        "hintMaxSigmaM": 0,
        "colocatedM": 0,
        "minSites": 0,
        "minSitesAlt": 0,
        "maxParts": 0,
        "lateHoldMs": 0,
        "lateSlots": 0
      },
      "plan": {
        "syncSetMin": 0,
        "syncSetMax": 0,
        "mlatSetMax": 0,
        "stationFpsBudget": 0,
        "stationsPerAircraft": 0,
        "minStationsPerAircraft": 0,
        "targetSigmaM": 0,
        "heardTtlS": 0,
        "adsbGoodAgeS": 0,
        "minDwellS": 0,
        "maxChurn": 0,
        "replanEveryS": 0,
        "acquireHz": 0,
        "trackedHz": 0,
        "holdOutHz": 0,
        "suspectHz": 0,
        "globalSolveHz": 0
      },
      "solve": {
        "maxIter": 0,
        "huberK": 0,
        "pfa": 0,
        "maxSigmaHM": 0,
        "altRejectSigma": 0,
        "priorSeedMaxSigmaM": 0,
        "gridStepM": 0,
        "gridHalfM": 0,
        "maxSpeedMps": 0,
        "machFactor": 0,
        "hostTolS": 0,
        "horizonSlackM": 0,
        "minHaeM": 0,
        "maxHaeM": 0,
        "maxRangeM": 0,
        "relinM": 0,
        "ransacMinStations": 0,
        "ransacMaxSubsets": 0,
        "alwaysAlternates": false
      },
      "track": {
        "qCv": 0,
        "qCt": 0,
        "qVert": 0,
        "qOmega": 0,
        "omegaTauS": 0,
        "sojournCvS": 0,
        "sojournCtS": 0,
        "initPCt": 0,
        "initSigmaVelH": 0,
        "initSigmaVelU": 0,
        "initSigmaOmega": 0,
        "gateProb": 0,
        "iterations": 0,
        "confirmWindowS": 0,
        "confirmUpdates": 0,
        "coastS": 0,
        "maxSigmaHM": 0,
        "maxSpeedMS": 0,
        "hypLR": 0,
        "maxHyp": 0,
        "respawnMisses": 0,
        "losMarginM": 0,
        "maxMembers": 0,
        "maxExclude": 0,
        "maxLagS": 0,
        "excessRefreshM": 0,
        "recentreM": 0,
        "syncTauS": 0
      },
      "engine": {
        "workers": 0,
        "queueLen": 0,
        "publishIntervalMs": 0,
        "maxSolveSites": 0,
        "contribWindowSec": 0,
        "minTimedPerMin": 0,
        "holdoutRotateMin": 0,
        "latencyFeedback": false,
        "jitterFeedback": false,
        "applyKAtm": false,
        "verifyEverySec": 0,
        "syncWarmupSec": 0,
        "ingestGate": false,
        "modeAcCodeFormat": "",
        "modeAcJitterNs": 0
      },
      "clock": {
        "hostSigmaS": 0,
        "ppsCorrelatedNs": 0,
        "resetThetaSigmaNs": 0,
        "maxMembers": 0,
        "syncRatePerStation": 0,
        "maxObsAgeS": 0,
        "stationTimeoutS": 0,
        "coObsWindowS": 0,
        "probationS": 0,
        "anchorFreqMaxPpb": 0,
        "anchorOffsetMaxNs": 0,
        "anchorDriftMaxNsPerH": 0,
        "anchorHostMaxS": 0,
        "gateProb": 0,
        "wTestMax": 0,
        "stepSigma": 0,
        "stepCount": 0,
        "stepRunCount": 0,
        "stepRunWindowS": 0,
        "blacklistS": 0,
        "blacklistMaxFrac": 0,
        "stationBadOutlier": 0,
        "syncedMinObs": 0,
        "syncedWindowS": 0,
        "workers": 0,
        "noJitterLearning": false,
        "reoriginS": 0,
        "relGaugeBetaSigmaNs": 0,
        "externalInflate": 0,
        "partitionStations": 0,
        "externalStates": 0,
        "partitionDriftNs": 0
      },
      "quarantine": {
        "disabled": false,
        "minRefs": 0,
        "windowSec": 0,
        "maxSigmaNs": 0,
        "goodWindows": 0,
        "maxBadFrac": 0,
        "minAgeSec": 0
      },
      "survey": {
        "disabled": false,
        "maxJobs": 0,
        "maxCheckFrac": 0,
        "minDurationSec": 0,
        "evalEverySec": 0,
        "maxDurationMin": 0,
        "noDataAfterMin": 0,
        "retryAfterMin": 0,
        "ownerMinIntervalMin": 0,
        "maxObs": 0,
        "minIntervalMs": 0,
        "minPeers": 0,
        "maxPeerSigmaNs": 0,
        "maskDeg": 0,
        "heightSigmaM": 0,
        "defaultAglM": 0,
        "searchRadiusKm": 0,
        "stableSigmaGpsM": 0,
        "stableSigmaM": 0,
        "minAzSpreadDeg": 0,
        "minAircraft": 0,
        "suspectMinM": 0,
        "suspectK": 0,
        "bundle": {
          "disabled": false,
          "everyMin": 0,
          "spanMin": 0,
          "maxStations": 0,
          "windowS": 0,
          "thinIntervalS": 0,
          "maxFrames": 0,
          "minStationRecs": 0,
          "outlierMinM": 0,
          "outlierK": 0,
          "declaredSigmaM": 0,
          "anchorSigmaM": 0,
          "maxReleases": 0,
          "workers": 0
        }
      },
      "latencyCal": {
        "enabled": false,
        "minGain": 0,
        "zEnable": 0,
        "zDisable": 0,
        "treatFraction": 0,
        "minCalSamples": 0,
        "minArmSamples": 0,
        "minArmAircraft": 0,
        "windowH": 0,
        "priorSigmaS": 0,
        "calibratedSigmaS": 0
      }
    },
    "retention": {
      "connLogDays": 30,
      "minuteDays": 30,
      "hourMonths": 13,
      "offlinePurgeDays": 90,
      "auditMonths": 24,
      "estimatesPerMethod": 5,
      "backupDays": 30
    },
    "limits": {
      "maxStations": 2000,
      "selectiveAbove": 100,
      "perIpStations": 8,
      "maxFpsPerStation": 6000,
      "maxKBpsPerStation": 200,
      "maxUnauthPerIp": 4,
      "maxConnsPerIp": 16,
      "maxConnsPerSubnet": 32,
      "handshakeTimeoutSec": 10,
      "ingestFps": 3000000,
      "maxAircraft": 30000,
      "trackerShards": 0,
      "publicRps": 1,
      "publicBurst": 10,
      "maxPublicSse": 2000,
      "maxHttpConns": 20000,
      "maxHttpConnsPerIp": 64
    },
    "auth": {
      "ownerSessionDays": 30,
      "ownerIdleDays": 14,
      "adminSessionHours": 12,
      "adminIdleMinutes": 120,
      "registration": "open",
      "loginFreeFailures": 5
    },
    "beast": {
      "identifyWaitMs": 2000,
      "idleTimeoutSec": 180,
      "pingIntervalSec": 5,
      "slowDownEverySec": 10,
      "queueBatches": 64,
      "maxLinks": 4096,
      "acceptMlatResults": false,
      "stripClock": false,
      "overLimitSeconds": 5,
      "coolDownSec": 300
    },
    "lanes": {
      "flushSec": 10,
      "slowFlushSec": 300,
      "evictHours": 24,
      "maxRangeKm": 800
    },
    "storage": {
      "minuteDays": 0,
      "hourDays": 0,
      "dayDays": 0,
      "histDailyDays": 35,
      "histDays": 395,
      "heatIdleDays": 395,
      "flightStationDays": 90,
      "trailDays": 7,
      "trailFullDays": 2,
      "trailThinSec": 30,
      "freePct": 10,
      "walCheckpointMb": 64,
      "optimizeHours": 1,
      "queueRows": 2000000
    },
    "ingest": {
      "windowMs": 1000,
      "flushAfterMs": 400,
      "retainMs": 1000,
      "maxMatchMs": 300,
      "minMatchMs": 10,
      "uniqueSkewMs": 2500,
      "lateAfterMs": 0,
      "shards": 64,
      "reduceMs": 500,
      "flowHoldSec": 5,
      "flowGraceSec": 10,
      "avfQueueRecords": 64,
      "avfResumeGraceSec": 30,
      "avfMaxSessions": 10000,
      "clockGateFloorUs": 0,
      "flushSampleEvery": 0,
      "avfHostSlackMs": 0,
      "adaptiveFlush": false
    },
    "mlatc": {
      "splitSync": "",
      "maxRequested": 0,
      "maxSeen": 0,
      "maxBadLines": 0,
      "idleTimeoutSec": 0,
      "heartbeatSec": 0,
      "statsSec": 0,
      "overLimitSeconds": 0,
      "coolDownSec": 0,
      "maxClients": 0,
      "maxOutQueueKB": 0,
      "udpHost": "",
      "udpPort": 0,
      "udpListen": "",
      "udpAnySource": false,
      "udpReaders": 0,
      "modeAcAllClients": false
    },
    "positions": {
      "jitterM": 0,
      "minIntervalHours": 0,
      "holdBeyondM": 0,
      "holdMaxDays": 0,
      "surveyMatchM": 0
    },
    "federation": {
      "enabled": false
    },
    "shareLinks": {
      "disabled": false,
      "maxPerOwner": 0,
      "maxDays": 0,
      "defaultDays": 0,
      "exactLowDefault": false
    }
  },
  "update": {
    "enabled": false,
    "channel": "stable",
    "manifestUrl": "",
    "keys": [],
    "checkIntervalMin": 1440,
    "healthWindowS": 60,
    "maxDownloadMB": 256,
    "healthUrl": ""
  },
  "locate": {
    "enabled": true,
    "retentionDays": 7,
    "maxRowsPerStation": 2000000,
    "maxRowsTotal": 10000000
  }
}
```

### `PUT /api/config`

Validate, save and apply the configuration live; returns the stored configuration (redacted).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `model.Config` (the object returned by `GET /api/config`, edited). Top-level sections that are missing keep their stored value; a section that is present replaces the stored one completely (send whole `antennas` / `feeds` arrays). Secrets: `""` or `"set"` keeps the stored secret, `"clear"` removes it, any other value is the new secret; for `http.adminPasswordHash` send the new plain password as `"plain:<password>"`.

Invalid values are corrected, not rejected (the corrections arrive as `notice` events with level `warn`). A changed `http.port` / `http.bindAddr` is saved but needs a restart: see `restartRequired` in `GET /api/system`. Changing the admin password ends all login sessions.

Example: `PUT /api/config` with body `{"ui":{"units":"metric","trailMinutes":20,"rangeRingsKm":[100,200,300],"aircraftTtlS":60,"mapStyle":"airview-dark","colorTrailsBy":"altitude"}}` -> `200`

```jsonc
{
  "version": 1,
  "mode": "standalone",
  "http": {
    "port": 80,
    "bindAddr": "",
    "adminPasswordHash": "",
    "tileCache": true
  },
  "receiver": {
    "name": "My Station",
    "model": "",
    "located": false,
    "lat": 0,
    "lon": 0,
    "groundElevM": 0
  },
  "antennas": [
    {
      "id": "vertical",
      "name": "Vertical",
      "type": "vertical",
      "channel": 0,
      "gainDbi": 5,
      "heightAglM": 8,
      "azimuthDeg": 0,
      "tiltDeg": 0,
      "hBeamwidthDeg": 0,
      "vBeamwidthDeg": 30,
      "frontToBackDb": 0,
      "elements": 0,
      "polarization": "vertical",
      "cableLossDb": 0,
      "lnaGainDb": 0,
      "lnaNoiseFigDb": 0,
      "filterLossDb": 0,
      "notes": "",
      "color": "#38bdf8"
    }
  ],
  "feeds": [
    {
      "id": "beast",
      "name": "Local Beast (readsb/dump1090 :30005)",
      "enabled": true,
      "type": "beast",
      "host": "localhost",
      "port": 30005,
      "url": "",
      "antennaId": "",
      "mlatOnly": false,
      "notes": ""
    }
  ],
  "ntp": {
    "enabled": false,
    "server": "",
    "intervalSec": 64
  },
  "weather": {
    "enabled": true,
    "intervalMin": 15,
    "radarOverlay": false
  },
  "hub": {
    "enabled": false,
    "port": 30154,
    "password": ""
  },
  "remote": {
    "host": "",
    "dataPort": 30154,
    "httpPort": 80,
    "password": "",
    "useHostReceiver": true
  },
  "storage": {
    "trailIntervalSec": 5,
    "trailRetentionDays": 30,
    "flightRetentionDays": 730,
    "minuteRetentionDays": 90
  },
  "ui": {
    "units": "metric",
    "trailMinutes": 20,
    "rangeRingsKm": [100, 200, 300],
    "aircraftTtlS": 60,
    "mapStyle": "airview-dark",
    "colorTrailsBy": "altitude"
  },
  "demo": {
    "enabled": false,
    "aircraft": 60
  },
  "prediction": {
    "enabled": true,
    "coastSec": 60,
    "pathSec": 120,
    "showPaths": true
  },
  "terrain": {
    "enabled": true,
    "radiusKm": 450,
    "zoom": 0,
    "kFactor": 1.3333333333333333,
    "autoPrefetch": true
  },
  "navData": {
    "airportsOverlay": true,
    "autoRefresh": true,
    "photos": false,
    "photoContact": "",
    "routes": {
      "enabled": true,
      "online": true,
      "dailyCap": 1000,
      "stableSec": 45,
      "contact": "",
      "learn": true,
      "eta": true
    }
  },
  "alerts": {
    "enabled": true,
    "sound": true,
    "webhooks": true
  },
  "rfStat": {
    "enabled": true
  },
  "uplinkTuning": {
    "ringSec": 600,
    "ringMb": 12,
    "backfillQueueMb": 24,
    "resumeWindowSec": 30,
    "backfillMinKBps": 64,
    "backoffMinSec": 1,
    "backoffMaxSec": 60,
    "livenessSec": 30,
    "spoolSegmentMb": 4
  },
  "aggregator": {
    "name": "AirView Network",
    "publicHost": "",
    "privacyNoticeUrl": "",
    "avfPort": 30160,
    "beastPort": 30004,
    "mlatPort": 31090,
    "sbsInLocalPort": 0,
    "beastOutPort": 0,
    "sbsOutPort": 0,
    "outputToken": "",
    "admission": "invite",
    "tlsCert": "",
    "tlsKey": "",
    "publicMap": true,
    "publicStats": true,
    "tarJson": true,
    "defaultPin": "hidden",
    "islandGuard": false,
    "listenAddr": "",
    "feederReleaseDir": "",
    "feederManifestKey": "",
    "mlat": {
      "enabled": true,
      "returnResults": true,
      "holdout": 0.1,
      "autoUseSurvey": true,
      "autoUseMaxSigmaM": 50,
      "epochMs": 500,
      "maxComponentStations": 200,
      "modeAc": false,
      "udp": false,
      "surveyIntervalH": 6,
      "refractivityN0": 315,
      "refractivityScaleHeightM": 7000,
      "ref": {
        "window": 0,
        "qHorizontal": 0,
        "qVertical": 0,
        "minNacp": 0,
        "minNic": 0,
        "maxPosAgeS": 0,
        "latencySigmaS": 0,
        "gateChi2": 0,
        "gateFloorM": 0,
        "probationS": 0,
        "suspendS": 0,
        "trackTimeoutS": 0,
        "altReplyMinS": 0,
        "pairBaroS": 0,
        "noAltReplies": false,
        "field": {
          "cellKm": 0,
          "bandFt": 0,
          "driftMPerH": 0,
          "obsSigmaM": 0,
          "profileSigmaM": 0,
          "profileSigmaTK": 0
        }
      },
      "group": {
        "nSigma": 0,
        "slackM": 0,
        "hintSlackM": 0,
        "hintMaxSigmaM": 0,
        "colocatedM": 0,
        "minSites": 0,
        "minSitesAlt": 0,
        "maxParts": 0,
        "lateHoldMs": 0,
        "lateSlots": 0
      },
      "plan": {
        "syncSetMin": 0,
        "syncSetMax": 0,
        "mlatSetMax": 0,
        "stationFpsBudget": 0,
        "stationsPerAircraft": 0,
        "minStationsPerAircraft": 0,
        "targetSigmaM": 0,
        "heardTtlS": 0,
        "adsbGoodAgeS": 0,
        "minDwellS": 0,
        "maxChurn": 0,
        "replanEveryS": 0,
        "acquireHz": 0,
        "trackedHz": 0,
        "holdOutHz": 0,
        "suspectHz": 0,
        "globalSolveHz": 0
      },
      "solve": {
        "maxIter": 0,
        "huberK": 0,
        "pfa": 0,
        "maxSigmaHM": 0,
        "altRejectSigma": 0,
        "priorSeedMaxSigmaM": 0,
        "gridStepM": 0,
        "gridHalfM": 0,
        "maxSpeedMps": 0,
        "machFactor": 0,
        "hostTolS": 0,
        "horizonSlackM": 0,
        "minHaeM": 0,
        "maxHaeM": 0,
        "maxRangeM": 0,
        "relinM": 0,
        "ransacMinStations": 0,
        "ransacMaxSubsets": 0,
        "alwaysAlternates": false
      },
      "track": {
        "qCv": 0,
        "qCt": 0,
        "qVert": 0,
        "qOmega": 0,
        "omegaTauS": 0,
        "sojournCvS": 0,
        "sojournCtS": 0,
        "initPCt": 0,
        "initSigmaVelH": 0,
        "initSigmaVelU": 0,
        "initSigmaOmega": 0,
        "gateProb": 0,
        "iterations": 0,
        "confirmWindowS": 0,
        "confirmUpdates": 0,
        "coastS": 0,
        "maxSigmaHM": 0,
        "maxSpeedMS": 0,
        "hypLR": 0,
        "maxHyp": 0,
        "respawnMisses": 0,
        "losMarginM": 0,
        "maxMembers": 0,
        "maxExclude": 0,
        "maxLagS": 0,
        "excessRefreshM": 0,
        "recentreM": 0,
        "syncTauS": 0
      },
      "engine": {
        "workers": 0,
        "queueLen": 0,
        "publishIntervalMs": 0,
        "maxSolveSites": 0,
        "contribWindowSec": 0,
        "minTimedPerMin": 0,
        "holdoutRotateMin": 0,
        "latencyFeedback": false,
        "jitterFeedback": false,
        "applyKAtm": false,
        "verifyEverySec": 0,
        "syncWarmupSec": 0,
        "ingestGate": false,
        "modeAcCodeFormat": "",
        "modeAcJitterNs": 0
      },
      "clock": {
        "hostSigmaS": 0,
        "ppsCorrelatedNs": 0,
        "resetThetaSigmaNs": 0,
        "maxMembers": 0,
        "syncRatePerStation": 0,
        "maxObsAgeS": 0,
        "stationTimeoutS": 0,
        "coObsWindowS": 0,
        "probationS": 0,
        "anchorFreqMaxPpb": 0,
        "anchorOffsetMaxNs": 0,
        "anchorDriftMaxNsPerH": 0,
        "anchorHostMaxS": 0,
        "gateProb": 0,
        "wTestMax": 0,
        "stepSigma": 0,
        "stepCount": 0,
        "stepRunCount": 0,
        "stepRunWindowS": 0,
        "blacklistS": 0,
        "blacklistMaxFrac": 0,
        "stationBadOutlier": 0,
        "syncedMinObs": 0,
        "syncedWindowS": 0,
        "workers": 0,
        "noJitterLearning": false,
        "reoriginS": 0,
        "relGaugeBetaSigmaNs": 0,
        "externalInflate": 0,
        "partitionStations": 0,
        "externalStates": 0,
        "partitionDriftNs": 0
      },
      "quarantine": {
        "disabled": false,
        "minRefs": 0,
        "windowSec": 0,
        "maxSigmaNs": 0,
        "goodWindows": 0,
        "maxBadFrac": 0,
        "minAgeSec": 0
      },
      "survey": {
        "disabled": false,
        "maxJobs": 0,
        "maxCheckFrac": 0,
        "minDurationSec": 0,
        "evalEverySec": 0,
        "maxDurationMin": 0,
        "noDataAfterMin": 0,
        "retryAfterMin": 0,
        "ownerMinIntervalMin": 0,
        "maxObs": 0,
        "minIntervalMs": 0,
        "minPeers": 0,
        "maxPeerSigmaNs": 0,
        "maskDeg": 0,
        "heightSigmaM": 0,
        "defaultAglM": 0,
        "searchRadiusKm": 0,
        "stableSigmaGpsM": 0,
        "stableSigmaM": 0,
        "minAzSpreadDeg": 0,
        "minAircraft": 0,
        "suspectMinM": 0,
        "suspectK": 0,
        "bundle": {
          "disabled": false,
          "everyMin": 0,
          "spanMin": 0,
          "maxStations": 0,
          "windowS": 0,
          "thinIntervalS": 0,
          "maxFrames": 0,
          "minStationRecs": 0,
          "outlierMinM": 0,
          "outlierK": 0,
          "declaredSigmaM": 0,
          "anchorSigmaM": 0,
          "maxReleases": 0,
          "workers": 0
        }
      },
      "latencyCal": {
        "enabled": false,
        "minGain": 0,
        "zEnable": 0,
        "zDisable": 0,
        "treatFraction": 0,
        "minCalSamples": 0,
        "minArmSamples": 0,
        "minArmAircraft": 0,
        "windowH": 0,
        "priorSigmaS": 0,
        "calibratedSigmaS": 0
      }
    },
    "retention": {
      "connLogDays": 30,
      "minuteDays": 30,
      "hourMonths": 13,
      "offlinePurgeDays": 90,
      "auditMonths": 24,
      "estimatesPerMethod": 5,
      "backupDays": 30
    },
    "limits": {
      "maxStations": 2000,
      "selectiveAbove": 100,
      "perIpStations": 8,
      "maxFpsPerStation": 6000,
      "maxKBpsPerStation": 200,
      "maxUnauthPerIp": 4,
      "maxConnsPerIp": 16,
      "maxConnsPerSubnet": 32,
      "handshakeTimeoutSec": 10,
      "ingestFps": 3000000,
      "maxAircraft": 30000,
      "trackerShards": 0,
      "publicRps": 1,
      "publicBurst": 10,
      "maxPublicSse": 2000,
      "maxHttpConns": 20000,
      "maxHttpConnsPerIp": 64
    },
    "auth": {
      "ownerSessionDays": 30,
      "ownerIdleDays": 14,
      "adminSessionHours": 12,
      "adminIdleMinutes": 120,
      "registration": "open",
      "loginFreeFailures": 5
    },
    "beast": {
      "identifyWaitMs": 2000,
      "idleTimeoutSec": 180,
      "pingIntervalSec": 5,
      "slowDownEverySec": 10,
      "queueBatches": 64,
      "maxLinks": 4096,
      "acceptMlatResults": false,
      "stripClock": false,
      "overLimitSeconds": 5,
      "coolDownSec": 300
    },
    "lanes": {
      "flushSec": 10,
      "slowFlushSec": 300,
      "evictHours": 24,
      "maxRangeKm": 800
    },
    "storage": {
      "minuteDays": 0,
      "hourDays": 0,
      "dayDays": 0,
      "histDailyDays": 35,
      "histDays": 395,
      "heatIdleDays": 395,
      "flightStationDays": 90,
      "trailDays": 7,
      "trailFullDays": 2,
      "trailThinSec": 30,
      "freePct": 10,
      "walCheckpointMb": 64,
      "optimizeHours": 1,
      "queueRows": 2000000
    },
    "ingest": {
      "windowMs": 1000,
      "flushAfterMs": 400,
      "retainMs": 1000,
      "maxMatchMs": 300,
      "minMatchMs": 10,
      "uniqueSkewMs": 2500,
      "lateAfterMs": 0,
      "shards": 64,
      "reduceMs": 500,
      "flowHoldSec": 5,
      "flowGraceSec": 10,
      "avfQueueRecords": 64,
      "avfResumeGraceSec": 30,
      "avfMaxSessions": 10000,
      "clockGateFloorUs": 0,
      "flushSampleEvery": 0,
      "avfHostSlackMs": 0,
      "adaptiveFlush": false
    },
    "mlatc": {
      "splitSync": "",
      "maxRequested": 0,
      "maxSeen": 0,
      "maxBadLines": 0,
      "idleTimeoutSec": 0,
      "heartbeatSec": 0,
      "statsSec": 0,
      "overLimitSeconds": 0,
      "coolDownSec": 0,
      "maxClients": 0,
      "maxOutQueueKB": 0,
      "udpHost": "",
      "udpPort": 0,
      "udpListen": "",
      "udpAnySource": false,
      "udpReaders": 0,
      "modeAcAllClients": false
    },
    "positions": {
      "jitterM": 0,
      "minIntervalHours": 0,
      "holdBeyondM": 0,
      "holdMaxDays": 0,
      "surveyMatchM": 0
    },
    "federation": {
      "enabled": false
    },
    "shareLinks": {
      "disabled": false,
      "maxPerOwner": 0,
      "maxDays": 0,
      "defaultDays": 0,
      "exactLowDefault": false
    }
  },
  "update": {
    "enabled": false,
    "channel": "stable",
    "manifestUrl": "",
    "keys": [],
    "checkIntervalMin": 1440,
    "healthWindowS": 60,
    "maxDownloadMB": 256,
    "healthUrl": ""
  },
  "locate": {
    "enabled": true,
    "retentionDays": 7,
    "maxRowsPerStation": 2000000,
    "maxRowsTotal": 10000000
  }
}
```

### `POST /api/factory-reset`

Wipe the database and restore the default configuration, then restart the engine.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"confirm":"RESET"}`

Answers `{"ok":true}` when the engine is running again (can take a few seconds; other requests wait meanwhile). The tile cache is kept. All login sessions end.

Example: `POST /api/factory-reset` with body `{"confirm":"no"}` -> `400`

```jsonc
{
  "error": "confirmation text must be RESET"
}
```

### `POST /api/login`

Admin login.

Request body: `{"password":"..."}`

`401 {"error":"wrong password"}` on failure. After 5 failures from one address further attempts are refused with `429` and a `Retry-After` header (2 s, doubling up to 5 min). Without a configured password it answers `200` with `admin:true` and sets no cookie. Aggregator mode: the body is `{"username":"...","password":"...","code":"123456 or a recovery code"}`; a `401` with `codeRequired:true` asks for the code; the cookie is `SameSite=Strict` (plus `Secure` over TLS).

Response: `sessionInfo` JSON and `Set-Cookie: airview_session=...; HttpOnly; SameSite=Lax`.

Example: `POST /api/login` with body `{"password":"secret"}` -> `200`

```jsonc
{
  "ok": true,
  "admin": true,
  "loopback": true,
  "passwordSet": false,
  "loggedIn": false
}
```

### `POST /api/logout`

End the admin session.

Response: `sessionInfo` JSON; the cookie is cleared.

Example: `POST /api/logout` -> `200`

```jsonc
{
  "ok": true,
  "admin": true,
  "loopback": true,
  "passwordSet": false,
  "loggedIn": false
}
```

### `GET /api/session`

Whether this client is admin and whether a password is configured.

Response: [server.sessionInfo](#t-server-sessionInfo)

Example: `GET /api/session`

```jsonc
{
  "ok": true,
  "admin": true,
  "loopback": true,
  "passwordSet": false,
  "loggedIn": false
}
```

### `GET /api/aircraft`

Live aircraft table (`model.LiveSnapshot`), rebuilt once per second.

| Parameter | Meaning |
|---|---|
| `station` | SID of one of the viewer's own stations (admins: any station); the station's geometry is added |

Optional fields are omitted when unknown. Units: ft, kt, ft/min, km, degrees true, dBFS, Unix ms.

Response: [model.LiveSnapshot](#t-model-LiveSnapshot)

Example: `GET /api/aircraft`

```jsonc
{
  "now": 1795821896534,
  "aircraft": [
    {
      "icao": "3C473C",
      "addrType": "icao",
      "callsign": "DLH81AS",
      "squawk": "2017",
      "category": "A5",
      "operator": "Lufthansa",
      "country": "Germany",
      "countryCode": "DE",
      "lat": 51.67144775390625,
      "lon": 4.9039459228515625,
      "source": "adsb",
      "posAge": 1,
      "onGround": false,
      "altBaro": 32000,
      "altGeom": 32350,
      "vertRate": 0,
      "selAlt": 32000,
      "qnh": 1013.2,
      "gs": 413.2807762284619,
      "track": 284.0026191880347,
      "ias": 273,
      "tas": 444,
      "mach": 0.756,
      "magHeading": 280.01953125,
      "roll": 0,
      "trackRate": 0,
      "selHeading": 279.84375,
      "windDir": 267.20233714741534,
      "windSpd": 31.988267981898133,
      "oat": -46.00175623862748,
      "adsbVersion": 2,
      "nic": 10,
      "nacp": 10,
      "nacv": 2,
      "sil": 3,
      "rcM": 25,
      "navModes": ["althold"],
      "distKm": 316.57088706962315,
      "bearing": 306.1513316589171,
      "elevation": 0.6745640280992499,
      "slantKm": 316.9299703341622,
      "rssi": -33.6,
      "rssiAvg": -32.6,
      "msgs": 151,
      "msgRate": 3.5,
      "antennas": ["vertical"],
      "feeds": ["demo"],
      "firstSeen": 1795821863140,
      "lastSeen": 1795821896218,
      "flightId": 2052,
      "cpaKm": 315.77,
      "cpaInS": 0,
      "terrainVisible": true,
      "losMarginDeg": 0.78,
      "route": {
        "from": "EDDF",
        "to": "EGLL",
        "fromIata": "FRA",
        "toIata": "LHR",
        "fromName": "Frankfurt am Main",
        "toName": "London",
        "plausible": true,
        "source": "standing-data",
        "checked": true,
        "state": "fits",
        "conf": 0.9,
        "reason": "standing data · fits the track",
        "along": 45.5,
        "distRem": 371.4,
        "eta": 1795824330,
        "etaSigma": 150,
        "lic": "open"
      }
    },
    /* … 11 more */
  ],
  "counts": {
    "aircraft": 12,
    "withPos": 11,
    "adsb": 9,
    "mlat": 3,
    "tisb": 0,
    "modes": 0,
    "msgRate": 54.7,
    "posRate": 10.7,
    "maxRangeKm": 340.3,
    "predicted": 0
  }
}
```

Example of an aircraft with a `route` from the offline standing data (`plausible`: the position fits the great circle between the airports; `checked` false when that could not be tested):

```jsonc
{
  "icao": "3C473C",
  "addrType": "icao",
  "callsign": "DLH81AS",
  "squawk": "2017",
  "category": "A5",
  "operator": "Lufthansa",
  "country": "Germany",
  "countryCode": "DE",
  "lat": 51.67144775390625,
  "lon": 4.9039459228515625,
  "source": "adsb",
  "posAge": 1,
  "onGround": false,
  "altBaro": 32000,
  "altGeom": 32350,
  "vertRate": 0,
  "selAlt": 32000,
  "qnh": 1013.2,
  "gs": 413.2807762284619,
  "track": 284.0026191880347,
  "ias": 273,
  "tas": 444,
  "mach": 0.756,
  "magHeading": 280.01953125,
  "roll": 0,
  "trackRate": 0,
  "selHeading": 279.84375,
  "windDir": 267.20233714741534,
  "windSpd": 31.988267981898133,
  "oat": -46.00175623862748,
  "adsbVersion": 2,
  "nic": 10,
  "nacp": 10,
  "nacv": 2,
  "sil": 3,
  "rcM": 25,
  "navModes": ["althold"],
  "distKm": 316.57088706962315,
  "bearing": 306.1513316589171,
  "elevation": 0.6745640280992499,
  "slantKm": 316.9299703341622,
  "rssi": -33.6,
  "rssiAvg": -32.6,
  "msgs": 151,
  "msgRate": 3.5,
  "antennas": ["vertical"],
  "feeds": ["demo"],
  "firstSeen": 1795821863140,
  "lastSeen": 1795821896218,
  "flightId": 2052,
  "cpaKm": 315.77,
  "cpaInS": 0,
  "terrainVisible": true,
  "losMarginDeg": 0.78,
  "route": {
    "from": "EDDF",
    "to": "EGLL",
    "fromIata": "FRA",
    "toIata": "LHR",
    "fromName": "Frankfurt am Main",
    "toName": "London",
    "plausible": true,
    "source": "standing-data",
    "checked": true,
    "state": "fits",
    "conf": 0.9,
    "reason": "standing data · fits the track",
    "along": 45.5,
    "distRem": 371.4,
    "eta": 1795824330,
    "etaSigma": 150,
    "lic": "open"
  }
}
```

Example of a coasting aircraft (no position received for 9 s; `predicted:true`, the position is the tracking filter's prediction, `lastRealLat` / `lastRealLon` the last received one):

```jsonc
{
  "icao": "4CACEA",
  "addrType": "icao",
  "callsign": "RYR1649",
  "squawk": "1534",
  "operator": "Ryanair",
  "country": "Ireland",
  "countryCode": "IE",
  "lat": 49.627377787746056,
  "lon": 5.1444485252761485,
  "source": "adsb",
  "posAge": 8.7,
  "onGround": false,
  "altBaro": 35000,
  "altGeom": 35375,
  "vertRate": 0,
  "selAlt": 35008,
  "qnh": 1013.6,
  "gs": 485.51930960570456,
  "track": 81.3525306518372,
  "ias": 263,
  "tas": 450,
  "mach": 0.776,
  "magHeading": 78.046875,
  "roll": 0,
  "trackRate": 0,
  "selHeading": 78.046875,
  "windDir": 267.60421267334056,
  "windSpd": 35.74874111096344,
  "oat": -51.69340449226743,
  "adsbVersion": 2,
  "nic": 8,
  "nacp": 9,
  "nacv": 2,
  "sil": 3,
  "rcM": 185.2,
  "navModes": ["autopilot", "althold", "lnav", "tcas"],
  "distKm": 252.95730074691758,
  "bearing": 260.6154259468423,
  "elevation": 1.5334547118125155,
  "slantKm": 253.3744464985017,
  "rssi": -32.9,
  "rssiAvg": -32.7,
  "msgs": 132,
  "msgRate": 2,
  "antennas": ["vertical"],
  "feeds": ["demo"],
  "firstSeen": 1795821863125,
  "lastSeen": 1795821903803,
  "flightId": 2053,
  "predicted": true,
  "coastS": 8.7,
  "predSigmaM": 21,
  "lastRealLat": 49.62446594238281,
  "lastRealLon": 5.114713969983552,
  "cpaKm": 15.25,
  "cpaInS": 1010,
  "terrainVisible": true,
  "losMarginDeg": 1.65
}
```

### `GET /api/aircraft/{icao}`

One aircraft: live state, live trail, reference data, aircraft-database record, all-time totals and recent flights.

| Parameter | Meaning |
|---|---|
| `smooth` | `1` returns the live trail smoothed by the tracking filter (RTS smoother; useful for noisy MLAT trails) |

`{icao}` is 6 hex digits (`~` prefix for non-ICAO addresses). Always `200` for a valid address: `aircraft` is null when it is not tracked right now, `seen` null when it was never stored. `watch` is `{list, note}` when the address is on an alert watch-list, else null. `trail` points are `[tMs, lat, lon, altFt|null, gsKt|null, rssi|null, src]` with src 0 ADS-B, 1 MLAT, 2 TIS-B/ADS-R, 3 other. `flights` holds the newest 20.

Response: [server.aircraftDetail](#t-server-aircraftDetail)

Example: `GET /api/aircraft/3C473C`

```jsonc
{
  "aircraft": {
    "icao": "3C473C",
    "addrType": "icao",
    "callsign": "DLH81AS",
    "squawk": "2017",
    "category": "A5",
    "operator": "Lufthansa",
    "country": "Germany",
    "countryCode": "DE",
    "lat": 51.67543029785156,
    "lon": 4.878226615287162,
    "source": "adsb",
    "posAge": 1,
    "onGround": false,
    "altBaro": 32000,
    "altGeom": 32350,
    "vertRate": 0,
    "selAlt": 32000,
    "qnh": 1013.2,
    "gs": 413.2807762284619,
    "track": 284.0026191880347,
    "ias": 273,
    "tas": 444,
    "mach": 0.756,
    "magHeading": 280.01953125,
    "roll": 0,
    "trackRate": 0,
    "selHeading": 279.84375,
    "windDir": 267.20233714741534,
    "windSpd": 31.988267981898133,
    "oat": -46.00175623862748,
    "adsbVersion": 2,
    "nic": 10,
    "nacp": 10,
    "nacv": 2,
    "sil": 3,
    "rcM": 25,
    "navModes": ["althold"],
    "distKm": 318.3020602876175,
    "bearing": 306.04226329401286,
    "elevation": 0.659253978067017,
    "slantKm": 318.6611503417474,
    "rssi": -33.6,
    "rssiAvg": -33.3,
    "msgs": 163,
    "msgRate": 1.7,
    "antennas": ["vertical"],
    "feeds": ["demo"],
    "firstSeen": 1795821863140,
    "lastSeen": 1795821904091,
    "flightId": 2052
  },
  "trail": [
    [1795821865687, 51.65762, 4.99306, 32000, 413.3, -31.8, 0],
    [1795821869019, 51.65920, 4.98283, 32000, 413.3, -32.8, 0],
    /* … 9 more */
  ],
  "info": {
    "icao": "3C473C",
    "country": "Germany",
    "countryCode": "DE",
    "state": "Germany",
    "stateCode": "DE",
    "alloc": "state",
    "operator": "Lufthansa",
    "categoryDesc": "Heavy (above 300,000 lb / 136 t)",
    "callsign": {
      "callsign": "DLH81AS",
      "kind": "flight",
      "designator": "DLH",
      "flightNumber": "81AS",
      "operator": "Lufthansa",
      "operatorType": "airline",
      "telephony": "LUFTHANSA",
      "country": "Germany",
      "countryCode": "DE"
    },
    "spoken": "LUFTHANSA 81AS"
  },
  "acdb": null,
  "seen": {
    "icao": "3C473C",
    "firstSeen": 1795821863000,
    "lastSeen": 1795821894000,
    "callsign": "DLH81AS",
    "operator": "Lufthansa",
    "country": "Germany",
    "category": "A5",
    "msgs": 148,
    "flights": 1,
    "positions": 24,
    "maxDistKm": 315.80720434198395,
    "maxAlt": 32000,
    "srcMask": 1,
    "flags": 0
  },
  "flights": [
    {
      "id": 2052,
      "icao": "3C473C",
      "callsign": "DLH81AS",
      "squawk": "2017",
      "start": 1795821863000,
      "end": 1795821894000,
      "msgs": 148,
      "positions": 24,
      "srcMask": 1,
      "maxDistKm": 315.80720434198395,
      "maxDistBrg": 306.199225095921,
      "minDistKm": 310.58058439366135,
      "minAlt": 32000,
      "maxAlt": 32000,
      "maxGs": 413.2807762284619,
      "rssiMin": -35.4,
      "rssiMax": -29.5,
      "rssiAvg": -31.99452054794521,
      "firstLat": 51.657623291015625,
      "firstLon": 4.993063436972128,
      "lastLat": 51.66966247558594,
      "lastLon": 4.915268356735642,
      "operator": "Lufthansa",
      "country": "Germany"
    }
  ],
  "watch": null
}
```

### `GET /api/trails`

In-memory trails of all live aircraft.

| Parameter | Meaning |
|---|---|
| `minutes` | trail length, 1..240 (default `config.ui.trailMinutes`) |
| `bbox` | aggregator mode below admin: `south,west,north,east`, only aircraft whose newest point is inside |

`trails` maps the ICAO address to an array of `[tMs, lat, lon, altFt|null, gsKt|null, rssi|null, src]`, oldest first. In aggregator mode below admin scope the answer is the public one (`GET /api/public/trails`: at most 10 minutes and 120 points per aircraft).

Server-side cache: 2s (identical requests share one computation).

Response: [server.trailsReply](#t-server-trailsReply)

Example: `GET /api/trails?minutes=5`

```jsonc
{
  "now": 1795821905058,
  "minutes": 5,
  "trails": {
    "3C473C": [
      [1795821865687, 51.65762, 4.99306, 32000, 413.3, -31.8, 0],
      [1795821869019, 51.65920, 4.98283, 32000, 413.3, -32.8, 0],
      /* … 9 more */
    ],
    "3DD35E": [
      [1795821864244, 49.69913, 7.30880, 3050, 108.5, -31.2, 0],
      [1795821867354, 49.69812, 7.31061, 3050, 108.5, -33.1, 0],
      /* … 11 more */
    ],
    "4074E1": [
      [1795821864467, 51.08121, 9.92362, 32000, 428.8, -26.6, 0],
      [1795821867468, 51.07622, 9.91843, 32000, 428.8, -29.9, 0],
      /* … 11 more */
    ],
    "4436CA": [
      [1795821868499, 47.77881, 5.55105, 41000, 447.0, null, 1],
      [1795821873035, 47.77826, 5.56430, 41000, 446.3, null, 1],
      /* … 6 more */
    ],
    "4852DE": [
      [1795821869138, 50.50589, 8.27776, 23400, 432.2, null, 1],
      [1795821873603, 50.51184, 8.28319, 23200, 437.2, null, 1],
      /* … 6 more */
    ],
    "485FD9": [
      [1795821864289, 49.46928, 9.00778, 39000, 462.6, -18.5, 0],
      [1795821867630, 49.46897, 9.01874, 39000, 462.6, -18.4, 0],
      /* … 11 more */
    ],
    "4AAAF7": [
      [1795821866505, 50.35776, 7.06023, 11700, 343.1, null, 1],
      [1795821871003, 50.35204, 7.06912, 11550, 350.0, null, 1],
      /* … 7 more */
    ],
    "4CA19A": [
      [1795821864442, 49.27560, 7.79494, 30000, 458.6, -22.1, 0],
      [1795821867553, 49.27776, 7.78543, 30000, 458.6, -29.1, 0],
      /* … 11 more */
    ],
    "4CA2AB": [
      [1795821874102, 49.15139, 13.16791, 32000, 466.4, -32.2, 0],
      [1795821879440, 49.15590, 13.15176, 32000, 466.0, -32.9, 0],
      /* … 3 more */
    ],
    "4CACEA": [
      [1795821869752, 49.61545, 5.02357, 35000, 485.7, -32.4, 0],
      [1795821873285, 49.61663, 5.03567, 35000, 485.7, -32.6, 0],
      /* … 5 more */
    ],
    "AC9FDA": [
      [1795821874853, 52.24199, 10.38420, 33000, 437.8, -33.3, 0],
      [1795821879223, 52.23354, 10.38002, 33000, 437.8, -30.4, 0],
      /* … 6 more */
    ]
  }
}
```

### `GET /api/events`

Live stream (Server-Sent Events).

| Parameter | Meaning |
|---|---|
| `station` | SID of one of the viewer's own stations (admins: any station); the station's geometry is added |
| `bbox` | viewport stream, as for `GET /api/public/events` |
| `view` | `1`: whole-world viewport stream (see `POST /api/events/view`) |
| `replace` | stream id of a previous stream of this client to close first |

Alerts: show a toast for `alert` events with `notify` true, coloured by `severity` (`critical` red, `warning` amber, `info` blue), play a sound when `sound` is true (false whenever `config.alerts.sound` is off), clicking selects `icao`. Alerts of user rules and system alerts (feed down, range collapse, NTP) are additionally sent as `notice` with kind `alert`; the built-in emergency, military and record rules are not, because the tracker already sends notices for those. The first `status` and `aircraft` events are sent immediately on connect, preceded by `retry: 2000`. A comment line (`: hb`) is sent every 15 s. A client that does not read is disconnected after about 20 s of backlog; at most 64 streams are served (`503` beyond). Usable cross-origin (no credentials).

Response: `text/event-stream`. Events: `aircraft` (data = `model.LiveSnapshot`, every second), `status` (data = `model.Status`, every 5 s and right after a configuration change), `notice` (data = `{level, text, t, kind?, icao?, key?, value?}`; level `info` | `warn` | `alert`; kind `emergency` | `military` | `interesting` | `record` | `feed_up` | `feed_down` | `alert` or absent; `key` / `value` = record key and new value for kind `record`), `alert` (data = `alerts.Alert`, whenever an alert rule fires: `{id, t, ruleId, ruleName, severity, kind: aircraft|system, event?, icao?, callsign?, reg?, type?, squawk?, lat?, lon?, altFt?, distKm?, value?, text, notify, sound, highlight?}`).

Example stream (data shortened):

```
retry: 2000

event: status
data: {"app":"AirView","version":"test","platform":"linux/amd64","gui":false,"now":1795821905061,"started":1795821863046,"mode":"standalone","httpPort":42025,"located":true,"demo":true,"tileCache":true,"feeds":[{"id":"beast","name":"Loc ...

event: aircraft
data: {"now":1795821905047,"aircraft":[{"icao":"3C473C","addrType":"icao","callsign":"DLH81AS","squawk":"2017","category":"A5","operator":"Lufthansa","country":"Germany","countryCode":"DE","lat":51.67543029785156,"lon":4.878226615287162 ...

event: notice
data: {"level":"alert","text":"Emergency: DLH4AB squawks 7700 (general emergency)","t":1795821906261}

event: alert
data: {"id":1,"t":1795821907020,"ruleId":"r-25b1899d9e91","ruleName":"Example rule","severity":"warning","kind":"aircraft","icao":"3C473C","callsign":"DLH81AS","operator":"Lufthansa","squawk":"2017","lat":51.67543029785156,"lon":4.878226615287162,"altFt":32000,"distKm":318.3020602876175,"gsKt":413.2807762284619,"vrFpm":0,"trackDeg":284.0026191880347,"brgDeg":306.04226329401286,"rssi":-33.3,"text":"Example rule: DLH81AS at 32000 ft, 318.3 km","notify":true,"sound":true,"highlight":"#ffd60a"}

: hb

```

### `GET /api/notices`

The most recent notices (at most 50, oldest first) for clients that connect late.

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/notices`

```jsonc
{
  "notices": [
    {
      "level": "info",
      "text": "Feed \"Simulator (demo)\" connected (simulator)",
      "t": 1795821864059,
      "kind": "feed_up"
    },
    /* … 3 more */
  ]
}
```

### `GET /api/feeds`

Feed connections, per-feed and per-antenna counters, and the data port (hub) with its clients.

Response: [server.feedsReply](#t-server-feedsReply)

Example: `GET /api/feeds`

```jsonc
{
  "mode": "standalone",
  "demo": true,
  "feeds": [
    {
      "id": "beast",
      "name": "Local Beast (readsb/dump1090 :30005)",
      "type": "beast",
      "addr": "localhost:30005",
      "enabled": false,
      "connected": false,
      "since": 1795821863059,
      "frames": 0,
      "bytes": 0,
      "msgRate": 0,
      "lastFrame": 0,
      "gpsTime": false
    },
    /* … 1 more */
  ],
  "counters": [
    {
      "id": "beast",
      "frames": 0,
      "updates": 0,
      "updateMsgs": 0,
      "msgRate": 0,
      "lastFrame": 0
    },
    /* … 1 more */
  ],
  "antennas": [
    {
      "id": "",
      "frames": 2401,
      "positions": 460,
      "msgRate": 51.4
    },
    /* … 1 more */
  ],
  "hub": {
    "enabled": false,
    "port": 30154,
    "clients": 0,
    "clientList": [],
    "droppedClients": 0
  },
  "receiverGps": null
}
```

### `GET /api/tilecache`

Tile cache size and counters.

Response: [server.tileStats](#t-server-tileStats)

Example: `GET /api/tilecache`

```jsonc
{
  "enabled": true,
  "hosts": ["tiles.openfreemap.org", "tile.openstreetmap.org", "tilecache.rainviewer.com"],
  "dir": "/var/lib/airview/tilecache",
  "bytes": 0,
  "files": 0,
  "maxBytes": 536870912,
  "scanned": true,
  "hits": 0,
  "misses": 0,
  "revalidated": 0,
  "stale": 0,
  "errors": 0,
  "evictions": 0
}
```

### `POST /api/tilecache/clear`

Delete all cached map tiles.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`


### `GET /api/acas/events`

ACAS (TCAS II) resolution advisories reported by aircraft (DF16, BDS 3,0, TC28), newest first: when, where, which advisories ("Climb", "Do not descend", ...), the threat when reported, and whether the aircraft reported the RA as ended.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `icao` | only this aircraft (6 hex digits) |
| `limit` | 1..5000 (default 500) |

Default range: the last 7 days. Open events (`open:true`, `id` 0) are included. A report is the aircraft's own statement that its ACAS issued an advisory; it says nothing about the crew's response, and a missing report does not mean there was none (BDS 3,0 is only sent when a ground station asks for it).

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/acas/events?hours=24`

```jsonc
{
  "events": []
}
```

### `GET /api/acas/status`

Counters of the resolution advisory recorder (events since the start, open events, reports dropped by the flood guard).

Response: [acas.Stats](#t-acas-Stats)

Example: `GET /api/acas/status`

```jsonc
{
  "events": 0,
  "open": 0,
  "dropped": 0
}
```

### `GET /tilecache/{host}/{path...}`

Caching proxy for map tiles, glyphs, sprites and style documents: fetches `https://{host}/{path}?{query}` once and
serves it from `<data>/tilecache` afterwards. Enabled by `config.http.tileCache` (`status.tileCache`); `404` when
disabled.

- **Whitelist** (exact host names, also in `status.tileCacheHosts`): `tiles.openfreemap.org`,
  `tile.openstreetmap.org`, `tilecache.rainviewer.com`. Any other host: `403`. It is not an open proxy: only
  `GET` / `HEAD`, no cookies or client headers are forwarded, redirects to other hosts are not followed, and only
  image, vector-tile, font and JSON content is passed on.
- The path is passed through as written (`%20` stays `%20`), the query string too. Paths with `..`, encoded slashes
  or backslashes, control characters or more than 1024 characters are refused with `400`.
- Upstream requests carry `User-Agent: AirView/<version>`; at most 8 run at a time; identical concurrent requests
  share one download; timeout 25 s.
- **Freshness:** the upstream `Cache-Control: max-age` / `Expires` is honoured with a minimum of 1 day (10 min for
  JSON documents) and a maximum of 30 days. Expired objects are revalidated (`If-None-Match` /
  `If-Modified-Since`); if the upstream is unreachable the stale copy is served.
- **Response headers:** the upstream `Content-Type`, `ETag` / `Last-Modified`, `Cache-Control: public, max-age=<s>`,
  `Access-Control-Allow-Origin: *` and `X-AirView-Cache: HIT | MISS | REVALIDATED | STALE`. Conditional requests
  from the browser are answered with `304`.
- **Errors:** `404` tile does not exist upstream, `502` upstream failure or unexpected content (remembered for
  2 min), `503` proxy busy.
- **Disk cap:** 512 MB by default (environment variable `AIRVIEW_TILECACHE_MB`); above it the least recently used
  files are deleted. See `GET /api/tilecache` and `POST /api/tilecache/clear`.

In MapLibre, rewrite whitelisted URLs in `transformRequest`:
`https://tiles.openfreemap.org/planet/...` -> `/tilecache/tiles.openfreemap.org/planet/...`.

## Statistics

### `GET /api/stats/overview`

Everything a statistics landing page needs: last 24 h totals, sources, downlink formats, all-time totals, today, busiest day/hour, top lists, records, recent events.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 10s (identical requests share one computation).

Response: [analytics.Overview](#t-analytics-Overview)

Example: `GET /api/stats/overview`

```jsonc
{
  "now": 1795821907063,
  "ant": "",
  "last24h": {
    "frames": 11634808,
    "positions": 2578554,
    "mlatPositions": 100851,
    "crcBad": 691,
    "decodePct": 99.9941,
    "framesPerSec": 132.5422,
    "peakAircraft": 45,
    "peakAircraftT": 1795762800000,
    "maxKm": 475.2521,
    "rssiAvg": -26.737,
    "uptimePct": 99.8636,
    "uniqueAircraft": 825,
    "flights": 826,
    "minutesWithData": 1464
  },
  "sources": {
    "adsbPct": 80.1242,
    "mlatPct": 11.0559,
    "modesPct": 8.8199,
    "adsbPositions": 2477703,
    "mlatPositions": 100851
  },
  "df": [
    {
      "df": "DF0",
      "name": "Short air-air surveillance",
      "count": 822846,
      "pct": 7.0727
    },
    /* … 9 more */
  ],
  "allTime": {
    "airframes": 2047,
    "flights": 2058,
    "msgs": 29605879,
    "positions": 6594911,
    "since": 1795605805000,
    "maxKm": 475.2946,
    "countries": 27
  },
  "today": {
    "t": 1795737600000,
    "date": "2026-11-27",
    "frames": 11497380,
    "positions": 2550138,
    "peakAircraft": 45,
    "uniqueAircraft": 806,
    "flights": 807,
    "newAirframes": 798,
    "maxKm": 475.2521,
    "uptimeMin": 1404,
    "busiestHour": {
      "t": 1795762800000,
      "aircraft": 45,
      "frames": 712387
    },
    "farthest": {
      "flightId": 1270,
      "icao": "442D4E",
      "callsign": "AUA5681",
      "km": 475.2521,
      "t": 1795744958000
    }
  },
  "busiest": {
    "from": 1793145600000,
    "to": 1795824000000,
    "ant": "",
    "utcOffsetSec": 0,
    "days": [
      {
        "t": 1795564800000,
        "date": "2026-11-25",
        "frames": 6310157,
        "positions": 1403656,
        "peakAircraft": 43,
        "uniqueAircraft": 463,
        "flights": 463,
        "newAirframes": 463,
        "maxKm": 475.2485,
        "uptimeMin": 757,
        "busiestHour": {
          "t": 1795626000000,
          "aircraft": 43,
          "frames": 711139
        },
        "farthest": {
          "flightId": 391,
          "icao": "3474CA",
          "callsign": "VLG3JB",
          "km": 475.2485,
          "t": 1795634710000
        }
      },
      /* … 2 more */
    ],
    "busiestDay": {
      "t": 1795737600000,
      "date": "2026-11-27",
      "frames": 11497380,
      "positions": 2550138,
      "peakAircraft": 45,
      "uniqueAircraft": 806,
      "flights": 807,
      "newAirframes": 798,
      "maxKm": 475.2521,
      "uptimeMin": 1404,
      "busiestHour": {
        "t": 1795762800000,
        "aircraft": 45,
        "frames": 712387
      },
      "farthest": {
        "flightId": 1270,
        "icao": "442D4E",
        "callsign": "AUA5681",
        "km": 475.2521,
        "t": 1795744958000
      }
    },
    "busiestHour": {
      "t": 1795762800000,
      "aircraft": 45,
      "frames": 712387
    }
  },
  "top": {
    "from": 1793228400000,
    "to": 1795824000000,
    "flights": 2058,
    "aircraft": 2047,
    "operators": [
      {
        "key": "Ryanair",
        "flights": 165,
        "aircraft": 162,
        "msgs": 2262735,
        "pct": 8.0175,
        "maxKm": null
      },
      /* … 4 more */
    ],
    "countries": [
      {
        "key": "United Kingdom",
        "flights": 408,
        "aircraft": 405,
        "msgs": 6004809,
        "pct": 19.8251,
        "maxKm": null
      },
      /* … 4 more */
    ],
    "types": [],
    "airframes": [
      {
        "key": "4051D4",
        "label": "G-JZUT",
        "flights": 2,
        "aircraft": 1,
        "msgs": 53771,
        "pct": 0.0972,
        "operator": "TUI Airways",
        "country": "United Kingdom",
        "maxKm": 390.5081
      },
      /* … 4 more */
    ]
  },
  "records": [
    {
      "key": "max_aircraft",
      "value": 46,
      "t": 1795765228000
    },
    /* … 6 more */
  ],
  "events": [
    {
      "id": 227,
      "t": 1795821864000,
      "kind": "feed_up",
      "detail": {
        "addr": "simulator",
        "error": "",
        "feed": "demo",
        "name": "Simulator (demo)",
        "type": "demo"
      }
    },
    /* … 19 more */
  ],
  "located": true,
  "receiver": {
    "lat": 50.05,
    "lon": 8.6
  }
}
```

### `GET /api/stats/rates`

Traffic over time: message rate, positions, aircraft counts, range and signal as parallel arrays, plus totals.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Default 24 h. `resolution` is `minute` for spans up to 12 h, else `hour`. Every array in `series` has one value per `series.t` entry; null = no data.

Server-side cache: 10s (identical requests share one computation).

Response: [analytics.Traffic](#t-analytics-Traffic)

Example: `GET /api/stats/rates?hours=6`

```jsonc
{
  "from": 1795800300000,
  "to": 1795821960000,
  "ant": "",
  "resolution": "minute",
  "stepSec": 60,
  "series": {
    "t": [1795800300000, 1795800360000, 1795800420000, 1795800480000, 1795800540000, 1795800600000, 1795800660000, 1795800720000, /* … 351 more */],
    "framesPerSec": [212.5, 214.1, 208, 208.6, 215.25, 205.583, 208.033, 208.117, /* … 351 more */],
    "positionsPerSec": [48.483, 49.25, 47.467, 48.6, 50.667, 48.467, 49.167, 48.017, /* … 351 more */],
    "aircraft": [37, 37, 37, 37, 38, 39, 38, 38, /* … 351 more */],
    "aircraftPos": [34, 34, 34, 35, 36, 36, 35, 35, /* … 351 more */],
    "adsb": [32, 32, 32, 32, 33, 34, 34, 33, /* … 351 more */],
    "mlat": [3, 3, 3, 3, 3, 3, 2, 2, /* … 351 more */],
    "modes": [2, 2, 2, 2, 2, 2, 2, 3, /* … 351 more */],
    "maxKm": [425.386, 413.458, 411.813, 414.836, 422.927, 426.999, 437.965, 438.025, /* … 351 more */],
    "rssiAvg": [-27.016, -26.465, -25.89, -25.579, -25.649, -25.576, -26.272, -26.116, /* … 351 more */]
  },
  "totals": {
    "frames": 2444655,
    "positions": 555616,
    "mlatPositions": 17306,
    "crcBad": 118,
    "decodePct": 99.9952,
    "framesPerSec": 113.8006,
    "peakAircraft": 41,
    "peakAircraftT": 1795801080000,
    "maxKm": 474.5366,
    "rssiAvg": -26.2725,
    "uptimePct": 99.446,
    "uniqueAircraft": 190,
    "flights": 190,
    "minutesWithData": 359
  },
  "df": [
    {
      "df": "DF0",
      "name": "Short air-air surveillance",
      "count": 150725,
      "pct": 6.1658
    },
    /* … 9 more */
  ],
  "sources": {
    "adsbPct": 85.4061,
    "mlatPct": 7.1735,
    "modesPct": 7.4204,
    "adsbPositions": 538310,
    "mlatPositions": 17306
  }
}
```

### `GET /api/stats/df`

Downlink-format and source mix over a range, plus the live DF / Comm-B register counters since start.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 10s (identical requests share one computation).

Response: [server.dfReply](#t-server-dfReply)

Example: `GET /api/stats/df`

```jsonc
{
  "from": 1795734000000,
  "to": 1795824000000,
  "ant": "",
  "df": [
    {
      "df": "DF0",
      "name": "Short air-air surveillance",
      "count": 822846,
      "pct": 7.0727
    },
    /* … 9 more */
  ],
  "sources": {
    "adsbPct": 80.1242,
    "mlatPct": 11.0559,
    "modesPct": 8.8199,
    "adsbPositions": 2477703,
    "mlatPositions": 100851
  },
  "frames": 11634808,
  "crcBad": 691,
  "liveDf": {
    "df0": 241,
    "df11": 320,
    "df16": 0,
    "df17": 1184,
    "df18": 108,
    "df20": 196,
    "df21": 57,
    "df4": 225,
    "df5": 66,
    "df_other": 0
  },
  "liveBds": {
    "4,0": 86,
    "4,4": 1,
    "4,5": 0,
    "5,0": 69,
    "6,0": 82
  }
}
```

### `GET /api/stats/heatmap`

Day-of-week x hour-of-day heat map (7 x 24, Monday first, local time) of aircraft and message rate.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Default 28 days.

Server-side cache: 1m0s (identical requests share one computation).

Response: [analytics.WeekHeatmap](#t-analytics-WeekHeatmap)

Example: `GET /api/stats/heatmap`

```jsonc
{
  "from": 1793401200000,
  "to": 1795824000000,
  "ant": "",
  "utcOffsetSec": 0,
  "aircraft": [
    [null, null, null, null, null, null, null, null, /* … 16 more */],
    [null, null, null, null, null, null, null, null, /* … 16 more */],
    /* … 5 more */
  ],
  "framesPerSec": [
    [null, null, null, null, null, null, null, null, /* … 16 more */],
    [null, null, null, null, null, null, null, null, /* … 16 more */],
    /* … 5 more */
  ],
  "hours": [
    [0, 0, 0, 0, 0, 0, 0, 0, /* … 16 more */],
    [0, 0, 0, 0, 0, 0, 0, 0, /* … 16 more */],
    /* … 5 more */
  ],
  "days": ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"],
  "busiestDay": 4,
  "busiestHour": 8,
  "hourOfDay": [8.025, 8.742, 10.717, 14.3, 20.8, 28.633, 35.817, 39.267, /* … 16 more */]
}
```

### `GET /api/stats/daily`

Daily totals (local days) with the busiest day and hour.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Default 30 days.

Server-side cache: 1m0s (identical requests share one computation).

Response: [analytics.Daily](#t-analytics-Daily)

Example: `GET /api/stats/daily`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "utcOffsetSec": 0,
  "days": [
    {
      "t": 1795564800000,
      "date": "2026-11-25",
      "frames": 6310157,
      "positions": 1403656,
      "peakAircraft": 43,
      "uniqueAircraft": 463,
      "flights": 463,
      "newAirframes": 463,
      "maxKm": 475.2485,
      "uptimeMin": 757,
      "busiestHour": {
        "t": 1795626000000,
        "aircraft": 43,
        "frames": 711139
      },
      "farthest": {
        "flightId": 391,
        "icao": "3474CA",
        "callsign": "VLG3JB",
        "km": 475.2485,
        "t": 1795634710000
      }
    },
    /* … 2 more */
  ],
  "busiestDay": {
    "t": 1795737600000,
    "date": "2026-11-27",
    "frames": 11497380,
    "positions": 2550138,
    "peakAircraft": 45,
    "uniqueAircraft": 806,
    "flights": 807,
    "newAirframes": 798,
    "maxKm": 475.2521,
    "uptimeMin": 1404,
    "busiestHour": {
      "t": 1795762800000,
      "aircraft": 45,
      "frames": 712387
    },
    "farthest": {
      "flightId": 1270,
      "icao": "442D4E",
      "callsign": "AUA5681",
      "km": 475.2521,
      "t": 1795744958000
    }
  },
  "busiestHour": {
    "t": 1795762800000,
    "aircraft": 45,
    "frames": 712387
  }
}
```

### `GET /api/stats/top`

Top operators, countries, aircraft types and airframes.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `limit` | rows per list, 1..200 (default 15) |

Default 30 days.

Server-side cache: 1m0s (identical requests share one computation).

Response: [analytics.Top](#t-analytics-Top)

Example: `GET /api/stats/top?limit=5`

```jsonc
{
  "from": 1793228400000,
  "to": 1795824000000,
  "flights": 2058,
  "aircraft": 2047,
  "operators": [
    {
      "key": "Ryanair",
      "flights": 165,
      "aircraft": 162,
      "msgs": 2262735,
      "pct": 8.0175,
      "maxKm": null
    },
    /* … 4 more */
  ],
  "countries": [
    {
      "key": "United Kingdom",
      "flights": 408,
      "aircraft": 405,
      "msgs": 6004809,
      "pct": 19.8251,
      "maxKm": null
    },
    /* … 4 more */
  ],
  "types": [],
  "airframes": [
    {
      "key": "4051D4",
      "label": "G-JZUT",
      "flights": 2,
      "aircraft": 1,
      "msgs": 53771,
      "pct": 0.0972,
      "operator": "TUI Airways",
      "country": "United Kingdom",
      "maxKm": 390.5081
    },
    /* … 4 more */
  ]
}
```

### `GET /api/stats/altitudes`

Altitude distribution: positions per altitude band and a histogram of flight maximum altitudes.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 1m0s (identical requests share one computation).

Response: [analytics.AltitudeDistribution](#t-analytics-AltitudeDistribution)

Example: `GET /api/stats/altitudes`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "bands": [
    {
      "band": 0,
      "label": "\u003c1k",
      "positions": 118733,
      "pct": 1.873,
      "maxKm": 76.9716
    },
    /* … 6 more */
  ],
  "binFt": 2000,
  "flightAlt": [62, 98, 77, 32, 0, 2, 4, 3, /* … 22 more */],
  "flightsN": 2058,
  "medianFt": 34486.2745,
  "lowestFt": 350,
  "highestFt": 41000,
  "cruiseBand": "30-40k"
}
```

### `GET /api/stats/records`

All-time records.

Keys: `max_dist_km`, `max_alt_ft`, `max_gs_kt`, `min_rssi_pos_dbfs`, `max_aircraft`, `max_msg_rate`, `max_flight_s`. `detail` (when present) has callsign, reg, type, lat, lon, altFt, distKm, bearing, gsKt, rssi.

Server-side cache: 10s (identical requests share one computation).

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/stats/records`

```jsonc
{
  "records": [
    {
      "key": "max_aircraft",
      "value": 46,
      "t": 1795765228000
    },
    /* … 6 more */
  ]
}
```

### `GET /api/stats/events`

Event log, newest first: emergencies, military / interesting catches, new records, feed up / down.

| Parameter | Meaning |
|---|---|
| `limit` | 1..1000 (default 100) |
| `kind` | `emergency` \| `military` \| `interesting` \| `record` \| `feed_up` \| `feed_down` |
| `icao` | only this aircraft |
| `since` | only events at or after this time (Unix ms) |
| `before` | paging: only events with an id below this one |

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/stats/events?limit=5`

```jsonc
{
  "events": [
    {
      "id": 227,
      "t": 1795821864000,
      "kind": "feed_up",
      "detail": {
        "addr": "simulator",
        "error": "",
        "feed": "demo",
        "name": "Simulator (demo)",
        "type": "demo"
      }
    },
    /* … 4 more */
  ]
}
```

### `GET /api/stats/counters`

Decoder counters since the program started (`tracker.Counters`): frames, CRC failures, duplicates, positions, per-DF, per-antenna and per-feed totals.

Response: [tracker.Counters](#t-tracker-Counters)

Example: `GET /api/stats/counters`

```jsonc
{
  "now": 1795821907086,
  "started": 1795821863056,
  "uptimeS": 44,
  "frames": 2404,
  "accepted": 2398,
  "duplicates": 0,
  "crcBad": 0,
  "fixed": 0,
  "rejected": 6,
  "unsupported": 0,
  "badLength": 0,
  "modeAC": 0,
  "status": 44,
  "updates": 0,
  "updateMsgs": 0,
  "positions": 461,
  "mlatPositions": 66,
  "posRejected": 0,
  "posPending": 12,
  "posJumps": 0,
  "df": {
    "df0": 241,
    "df11": 320,
    "df16": 0,
    "df17": 1185,
    "df18": 108,
    "df20": 196,
    "df21": 57,
    "df4": 225,
    "df5": 66,
    "df_other": 0
  },
  "bds": {
    "4,0": 86,
    "4,4": 1,
    "4,5": 0,
    "5,0": 69,
    "6,0": 82
  },
  "flights": 12,
  "emergencies": 0,
  "msgRate": 51.4,
  "posRate": 10.6,
  "aircraft": 12,
  "tracked": 12,
  "antennas": [
    {
      "id": "",
      "frames": 2404,
      "positions": 461,
      "msgRate": 51.4
    },
    /* … 1 more */
  ],
  "feeds": [
    {
      "id": "beast",
      "frames": 0,
      "updates": 0,
      "updateMsgs": 0,
      "msgRate": 0,
      "lastFrame": 0
    },
    /* … 1 more */
  ]
}
```

### `GET /api/stats/range`

Hourly range metrics (`range_hour`): maximum and percentile distances, positions, aircraft, far-reception count, signal residual.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |
| `hist` | `1` adds `distHist` (120 counts of 5 km bins) and `sectorMax` (36 maxima per 10 degrees) to every hour |

Default 7 days.

Server-side cache: 1m0s (identical requests share one computation).

Response: [server.rangeReply](#t-server-rangeReply)

Example: `GET /api/stats/range?hours=48`

```jsonc
{
  "from": 1795649107088,
  "to": 1795821907088,
  "ant": "",
  "farKm": 300,
  "hours": [
    {
      "t": 1795651200000,
      "ant": "",
      "maxKm": 436.7617596830755,
      "p99Km": 409.26381578947377,
      "p95Km": 368.44075144508673,
      "p90Km": 334.927135678392,
      "p50Km": 127.46408045977012,
      "positions": 28419,
      "aircraft": 16,
      "farCount": 5092,
      "residDb": 15.008460974646606,
      "residN": 28419
    },
    /* … 47 more */
  ]
}
```

### `GET /api/stats/daynight`

Day / twilight / night comparison of range and signal, by local hour, with a significance test.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Default 30 days. Also available as `/api/antenna/daynight`.

Server-side cache: 5m0s (identical requests share one computation).

Response: [analytics.DayNight](#t-analytics-DayNight)

Example: `GET /api/stats/daynight`

```jsonc
{
  "from": 1793228400000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "utcOffsetSec": 0,
  "hours": 61,
  "phases": [
    {
      "phase": "day",
      "hour": 0,
      "hours": 21,
      "p95Km": 381.4317,
      "maxKm": 465.0208,
      "residDb": 14.5461,
      "aircraft": 69.5714,
      "positions": 139438.3333,
      "p95NormKm": 386.4678
    },
    /* … 2 more */
  ],
  "byHour": [
    {
      "hour": 0,
      "hours": 2,
      "p95Km": 383.6342,
      "maxKm": 455.2541,
      "residDb": 15.0445,
      "aircraft": 16,
      "positions": 27355.5,
      "p95NormKm": 371.5275
    },
    /* … 23 more */
  ],
  "trafficFit": {
    "n": 61,
    "slopeKm": -12.0633,
    "r2": 0.1506,
    "kmPerDoubling": -8.3617
  },
  "nightVsDay": {
    "nA": 21,
    "nB": 31,
    "diffKm": -0.4863,
    "rawDiffKm": 9.3006,
    "diffResidDb": 0.4265,
    "t": -0.1176,
    "p": 0.9069,
    "significant": false
  },
  "conclusions": ["Traffic volume explains 15 % of the hour-to-hour variation in range (-8 km per doubling of the aircraft count); the comparisons below are co…", "No reliable day/night difference in range once traffic is accounted for (-0 km at night, 31 night vs 21 day hours, p = 0.91).", "Hourly values are correlated from one hour to the next, so the p-values are optimistic."]
}
```

## Antenna & coverage

### `GET /api/antenna/coverage`

Polar coverage: maximum, robust and typical range per 2 degree bearing bin, overall and per altitude band, against the radio horizon; measured skyline and obstructions.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |
| `band` | altitude band 0..6 (0 <1000 ft, 1 1-5k, 2 5-10k, 3 10-20k, 4 20-30k, 5 30-40k, 6 >=40k) |

Default 30 days. With `band` only that band is returned in `bands`. All per-bearing arrays have 180 entries matching `bearingDeg`.

Server-side cache: 5m0s (identical requests share one computation).

Response: [analytics.Coverage](#t-analytics-Coverage)

Example: `GET /api/antenna/coverage`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "days": 3,
  "positions": 6339291,
  "located": true,
  "receiver": {
    "lat": 50.05,
    "lon": 8.6
  },
  "antennaAmslM": 118,
  "binDeg": 2,
  "bearingDeg": [1, 3, 5, 7, 9, 11, 13, 15, /* … 172 more */],
  "overall": {
    "band": -1,
    "label": "all",
    "altFt": 43000,
    "maxKm": [474.225, 466.445, 474.256, 474.289, 474.22, 471.735, 474.306, 474.327, /* … 172 more */],
    "robustKm": [472.64, 466.203, 474.247, 474.124, 474.178, 471.517, 474.303, 473.352, /* … 172 more */],
    "typicalKm": [458.374, 464.019, 474.172, 472.641, 473.792, 469.549, 474.271, 464.573, /* … 172 more */],
    "count": [50880, 56209, 54281, 54931, 51081, 48016, 48515, 52904, /* … 172 more */],
    "rssi": [-27.09, -27.24, -27.421, -27.047, -27.055, -27.236, -27.186, -27.332, /* … 172 more */],
    "positions": 6339291,
    "bearings": 180,
    "bestKm": 475.2946,
    "bestBrg": 279,
    "horizonKm": 516.3495,
    "horizonTypKm": 516.3495,
    "pctOfHorizon": [91.535, 90.288, 91.846, 91.822, 91.833, 91.317, 91.857, 91.673, /* … 172 more */],
    "meanPct": 90.5252
  },
  "bands": [
    {
      "band": 0,
      "label": "\u003c1k",
      "altFt": 500,
      "maxKm": [23.854, 55.758, 59.313, 28.012, 65.329, 62.501, 74.794, 45.706, /* … 172 more */],
      "robustKm": [23.346, 53.826, 57.149, 27.251, 62.334, 61.114, 67.903, 45.618, /* … 172 more */],
      "typicalKm": [18.772, 36.436, 37.664, 20.402, 35.378, 48.628, 69.463, 44.828, /* … 172 more */],
      "count": [97, 218, 247, 121, 144, 125, 172, 212, /* … 172 more */],
      "rssi": [-15.079, -18.539, -17.071, -12.905, -16.087, -21.929, -20.415, -18.925, /* … 172 more */],
      "positions": 118733,
      "bearings": 180,
      "bestKm": 76.9716,
      "bestBrg": 343,
      "horizonKm": 116.7338,
      "horizonTypKm": 95.6577,
      "pctOfHorizon": [19.999, 46.11, 48.956, 23.344, 53.398, 52.353, 58.169, 39.079, /* … 172 more */],
      "meanPct": 44.3425
    },
    /* … 6 more */
  ],
  "skyline": {
    "source": "heat",
    "elevDeg": [-0.185, -0.188, -0.218, -0.19, -0.207, -0.161, -0.184, -0.187, /* … 172 more */],
    "medianDeg": -0.1521,
    "bearings": 180
  },
  "obstructions": []
}
```

### `GET /api/antenna/coverage.geojson`

Coverage outlines as GeoJSON polygons for the map.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |
| `band` | altitude band 0..6 (0 <1000 ft, 1 1-5k, 2 5-10k, 3 10-20k, 4 20-30k, 5 30-40k, 6 >=40k) |
| `kind` | `max` or `robust` (default both) |
| `altBand` | `all` = one outline per altitude band with data (no overall), `-1` = all altitudes together only, `0`..`6` = that band only; takes precedence over `band` |

FeatureCollection of Polygon features (closed ring of `[lon, lat]`), properties `kind` (`max` | `robust`), `band` (-1 = all altitudes), `label`, `altFt`, `loFt`, `hiFt` (band limits in ft, null = open), `ant`. Without `band` / `altBand`: the overall outline plus every band. Empty collection until the receiver is located and data exists.

Server-side cache: 5m0s (identical requests share one computation).

Response: [analytics.FeatureCollection](#t-analytics-FeatureCollection)

Example: `GET /api/antenna/coverage.geojson`

```jsonc
{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "properties": {
        "altFt": 43000,
        "ant": "",
        "band": -1,
        "hiFt": null,
        "kind": "max",
        "label": "all",
        "loFt": 0
      },
      "geometry": {
        "type": "Polygon",
        "coordinates": [
          [
            [8.47446, 54.25518],
            [8.218, 54.30612],
            /* … 179 more */
          ]
        ]
      }
    },
    /* … 15 more */
  ]
}
```

### `GET /api/antenna/horizon`

Radio line-of-sight table per antenna and altitude band, and the achieved range against it.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |
| `k` | effective earth radius factor for the tables (default 1.3333) |

Server-side cache: 5m0s (identical requests share one computation).

Response: [server.horizonReply](#t-server-horizonReply)

Example: `GET /api/antenna/horizon`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "k": 1.3333333333333333,
  "groundElevM": 110,
  "antennaAmslM": 118,
  "antennas": [
    {
      "id": "vertical",
      "name": "Vertical",
      "heightAglM": 8,
      "amslM": 118,
      "table": [
        {
          "band": 0,
          "label": "\u003c1k",
          "altFt": 500,
          "losKm": 95.65769510274018,
          "losLoKm": 44.774109661882854,
          "losHiKm": 116.73382838688104
        },
        /* … 6 more */
      ]
    }
  ],
  "bands": [
    {
      "band": 0,
      "label": "\u003c1k",
      "altFt": 500,
      "horizonKm": 116.7338,
      "horizonTypKm": 95.6577,
      "bestKm": 76.9716,
      "bestBrg": 343,
      "meanPct": 44.3425
    },
    /* … 6 more */
  ]
}
```

### `GET /api/antenna/skyline`

Measured skyline (lowest elevation angle with reception per bearing) and detected obstructions.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 5m0s (identical requests share one computation).

Response: [server.skylineReply](#t-server-skylineReply)

Example: `GET /api/antenna/skyline`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "binDeg": 2,
  "bearingDeg": [1, 3, 5, 7, 9, 11, 13, 15, /* … 172 more */],
  "skyline": {
    "source": "heat",
    "elevDeg": [-0.185, -0.188, -0.218, -0.19, -0.207, -0.161, -0.184, -0.187, /* … 172 more */],
    "medianDeg": -0.1521,
    "bearings": 180
  },
  "obstructions": []
}
```

### `GET /api/antenna/heat`

All-time position heat grid as parallel arrays (cell centre, count, mean RSSI, lowest and highest altitude).

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |
| `minCount` | drop cells with fewer positions (default 1) |
| `maxCells` | cells are merged n x n until at most this many remain, 100..60000 (default 20000) |
| `bbox` | `west,south,east,north` in degrees: only this area |

`minAlt` is the coverage floor (lowest altitude ever received in the cell, ft).

Server-side cache: 5m0s (identical requests share one computation).

Response: [analytics.HeatGrid](#t-analytics-HeatGrid)

Example: `GET /api/antenna/heat?maxCells=2000`

```jsonc
{
  "ant": "",
  "cellDeg": 0.3,
  "step": 6,
  "cells": 1026,
  "totalCells": 33009,
  "positions": 6339300,
  "maxCount": 238112,
  "lat": [45.75, 45.75, 45.75, 45.75, 45.75, 45.75, 45.75, 45.75, /* … 1018 more */],
  "lon": [8.85, 7.65, 7.95, 8.25, 8.55, 7.35, 9.45, 9.75, /* … 1018 more */],
  "count": [11, 11, 14, 14, 17, 1, 28, 1, /* … 1018 more */],
  "rssi": [-33.3, -32.4, -32, -33.9, -33.3, -36, -32.3, -34.3, /* … 1018 more */],
  "minAlt": [38000, 33000, 41000, 39000, 37000, 39000, 37000, 40075, /* … 1018 more */],
  "maxAlt": [41000, 41000, 41000, 41000, 41000, 39000, 39950, 40075, /* … 1018 more */]
}
```

### `GET /api/antenna/signal`

RSSI against distance: 2-D histogram cells, per-distance percentiles and the path-loss fit.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 5m0s (identical requests share one computation).

Response: [server.signalReply](#t-server-signalReply)

Example: `GET /api/antenna/signal`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "signal": {
    "distBinKm": 10,
    "cells": [
      [0, -35, 9],
      [0, -34, 45],
      /* … 1088 more */
    ],
    "total": 6339300,
    "maxCount": 39792,
    "distKm": [5, 15, 25, 35, 45, 55, 65, 75, /* … 40 more */],
    "n": [107278, 306403, 344788, 334425, 305932, 290291, 260920, 247766, /* … 40 more */],
    "median": [-7.844, -13.23, -17.701, -20.427, -22.582, -24.041, -25.184, -25.813, /* … 40 more */],
    "p10": [-14.051, -19.11, -22.63, -24.942, -26.925, -28.266, -29.385, -29.822, /* … 40 more */],
    "p90": [-2.251, -7.579, -13.105, -15.864, -17.887, -19.079, -19.991, -20.638, /* … 40 more */],
    "rssiP01": -34.6392,
    "rssiP50": -28.0339,
    "rssiP99": -6.6279,
    "strongPct": 0.2826,
    "fit": {
      "bins": 34,
      "minKm": 25,
      "maxKm": 355,
      "slopeDbPerDecade": 11.2078,
      "freeSpaceSlope": 20,
      "interceptDb": -3.8986,
      "r2": 0.9719,
      "systemOffsetDb": 14.8762,
      "lineKm": [25, 27.922, 31.186, 34.832, 38.903, 43.451, 48.53, 54.203, /* … 17 more */],
      "measuredDb": [-19.566, -20.105, -20.643, -21.181, -21.719, -22.257, -22.795, -23.333, /* … 17 more */],
      "freeSpaceDb": [-13.083, -14.043, -15.003, -15.963, -16.924, -17.884, -18.844, -19.804, /* … 17 more */],
      "text": "Signal falls by only 11 dB per decade of distance (free space: 20): weak distant signals are missing from the statistics, so the receiver is…"
    }
  }
}
```

### `GET /api/antenna/azimuth`

Measured azimuth gain pattern (relative dB per 2 degree bearing) and the directional lobe fitted to it.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 5m0s (identical requests share one computation).

Response: [server.azimuthReply](#t-server-azimuthReply)

Example: `GET /api/antenna/azimuth`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "azimuth": {
    "method": "heat-residual",
    "bearingDeg": [1, 3, 5, 7, 9, 11, 13, 15, /* … 172 more */],
    "relDb": [0.983, 0.982, 0.914, 0.864, 0.856, 0.825, 0.699, 0.527, /* … 172 more */],
    "rawDb": [0.98, 1.035, 0.871, 0.871, 0.841, 0.869, 0.723, 0.473, /* … 172 more */],
    "cells": [82, 82, 81, 81, 81, 83, 82, 77, /* … 172 more */],
    "bearings": 180,
    "samples": 4444945,
    "peakDb": 0.9825,
    "peakBrg": 1,
    "minDb": -0.2927,
    "minBrg": 99,
    "spreadDb": 1.0323,
    "distSlopeDb": 9.689
  },
  "yagi": {
    "ok": false,
    "reason": "no clear directional lobe in the measured azimuth pattern",
    "azimuthDeg": 1.1014,
    "beamwidthDeg": 81.0043,
    "gainOverOmniDb": 0.954,
    "confidence": 0.1513,
    "confidenceLabel": "low",
    "explainedPct": 86.8932,
    "rmsDb": 0.0973,
    "fitDb": [0.936, 0.93, 0.91, 0.877, 0.831, 0.771, 0.697, 0.609, /* … 172 more */],
    "pointingErrorDeg": null,
    "beamwidthErrorDeg": null,
    "gainErrorDb": null,
    "text": ""
  }
}
```

### `GET /api/antenna/elevation`

Measured elevation pattern: counts, mean RSSI and distance-corrected residual per 0.5 degree of elevation.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 5m0s (identical requests share one computation).

Response: [server.elevationReply](#t-server-elevationReply)

Example: `GET /api/antenna/elevation`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "elevation": {
    "elevDeg": [-1.75, -1.25, -0.75, -0.25, 0.25, 0.75, 1.25, 1.75, /* … 176 more */],
    "count": [0, 0, 0, 177754, 1084877, 1137067, 889894, 643350, /* … 176 more */],
    "rssi": [null, null, null, -24.37, -28.333, -27.637, -27.235, -26.807, /* … 176 more */],
    "residDb": [null, null, null, 15.185, 15.556, 15.174, 15.137, 14.998, /* … 176 more */],
    "relDb": [null, null, null, 0, -0.052, -0.201, -0.341, -0.464, /* … 176 more */],
    "total": 6339300,
    "peakDeg": -0.25,
    "lowestDeg": -0.4108
  }
}
```

### `GET /api/antenna/fit`

Yagi lobe fit: inferred boresight, beamwidth and gain over the omni level, compared with the configured yagi.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 5m0s (identical requests share one computation).

Response: [server.fitReply](#t-server-fitReply)

Example: `GET /api/antenna/fit`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "yagi": {
    "ok": false,
    "reason": "no clear directional lobe in the measured azimuth pattern",
    "azimuthDeg": 1.1014,
    "beamwidthDeg": 81.0043,
    "gainOverOmniDb": 0.954,
    "confidence": 0.1513,
    "confidenceLabel": "low",
    "explainedPct": 86.8932,
    "rmsDb": 0.0973,
    "fitDb": [0.936, 0.93, 0.91, 0.877, 0.831, 0.771, 0.697, 0.609, /* … 172 more */],
    "pointingErrorDeg": null,
    "beamwidthErrorDeg": null,
    "gainErrorDb": null,
    "text": ""
  },
  "notes": []
}
```

### `GET /api/antenna/linkbudget`

Predicted (link budget) against achieved range per altitude band.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 5m0s (identical requests share one computation).

Response: [server.linkBudgetReply](#t-server-linkBudgetReply)

Example: `GET /api/antenna/linkbudget`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "linkBudget": [
    {
      "band": 0,
      "label": "\u003c1k",
      "altFt": 500,
      "horizonKm": 95.6577,
      "predictedKm": 95.6577,
      "limit": "horizon",
      "marginDb": 21.1556,
      "antenna": "vertical",
      "achievedMaxKm": 76.9716,
      "achievedRobustKm": 74.3825,
      "achievedTypicalKm": 53.7654,
      "pctOfPredicted": 77.759
    },
    /* … 6 more */
  ]
}
```

### `GET /api/antenna/pattern`

Configured antennas with their modelled azimuth / elevation patterns on the analysis grid, next to the measured pattern.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

`azGainDbi` / `azRelDb` have 180 entries (2 degree bearing bins, at 3 degrees elevation), `elGainDbi` / `elRelDb` 184 entries (0.5 degree bins from -2 degrees). For free-form cuts use `/api/radio/pattern`.

Server-side cache: 5m0s (identical requests share one computation).

Response: [server.patternReply](#t-server-patternReply)

Example: `GET /api/antenna/pattern`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "antennas": [
    {
      "id": "vertical",
      "name": "Vertical",
      "type": "vertical",
      "color": "#38bdf8",
      "info": {
        "kind": "vertical",
        "directional": false,
        "peakGainDbi": 5,
        "azimuthDeg": 0,
        "tiltDeg": 0,
        "hBeamwidthDeg": 360,
        "vBeamwidthDeg": 30,
        "frontToBackDb": 0,
        "sidelobeDb": 13.3,
        "floorDb": 30,
        "estimated": []
      },
      "azGainDbi": [4.881, 4.881, 4.881, 4.881, 4.881, 4.881, 4.881, 4.881, /* … 172 more */],
      "azRelDb": [0, 0, 0, 0, 0, 0, 0, 0, /* … 172 more */],
      "elGainDbi": [4.959, 4.979, 4.993, 4.999, 4.999, 4.993, 4.979, 4.959, /* … 176 more */],
      "elRelDb": [-0.04, -0.02, -0.007, 0, 0, -0.007, -0.02, -0.04, /* … 176 more */],
      "measured": {
        "method": "heat-residual",
        "bearingDeg": [1, 3, 5, 7, 9, 11, 13, 15, /* … 172 more */],
        "relDb": [-0.007, 0.023, -0.022, -0.036, 0.027, 0.103, 0.072, -0.02, /* … 172 more */],
        "rawDb": [-0.03, 0.105, -0.096, -0.016, -0.016, 0.151, 0.113, -0.086, /* … 172 more */],
        "cells": [82, 82, 81, 81, 81, 83, 82, 77, /* … 172 more */],
        "bearings": 180,
        "samples": 4176562,
        "peakDb": 0.2501,
        "peakBrg": 267,
        "minDb": -0.2908,
        "minBrg": 99,
        "spreadDb": 0.2853,
        "distSlopeDb": 9.3797
      },
      "compare": {
        "rmsDb": 0.0926,
        "r": 0,
        "n": 180
      }
    }
  ],
  "azimuth": {
    "method": "heat-residual",
    "bearingDeg": [1, 3, 5, 7, 9, 11, 13, 15, /* … 172 more */],
    "relDb": [0.983, 0.982, 0.914, 0.864, 0.856, 0.825, 0.699, 0.527, /* … 172 more */],
    "rawDb": [0.98, 1.035, 0.871, 0.871, 0.841, 0.869, 0.723, 0.473, /* … 172 more */],
    "cells": [82, 82, 81, 81, 81, 83, 82, 77, /* … 172 more */],
    "bearings": 180,
    "samples": 4444945,
    "peakDb": 0.9825,
    "peakBrg": 1,
    "minDb": -0.2927,
    "minBrg": 99,
    "spreadDb": 1.0323,
    "distSlopeDb": 9.689
  }
}
```

### `GET /api/antenna/score`

Station score 0..100 with sub-scores and plain-language findings.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 5m0s (identical requests share one computation).

Response: [analytics.StationScore](#t-analytics-StationScore)

Example: `GET /api/antenna/score`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "score": 95.4,
  "grade": "excellent",
  "subs": [
    {
      "key": "range",
      "label": "Range vs radio horizon",
      "score": 92.8776,
      "weight": 35,
      "available": true,
      "detail": "on average 93 % of the 500 km radio horizon (30-40k ft) around the compass"
    },
    /* … 4 more */
  ],
  "findings": [
    {
      "level": "good",
      "code": "range",
      "text": "Range is excellent: on average 93 % of the 500 km radio horizon (30-40k ft) around the compass."
    },
    /* … 2 more */
  ]
}
```

### `GET /api/antenna/report`

The complete antenna analysis in one document (signal, azimuth, elevation, antennas, yagi, linkBudget, notes).

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

The endpoints `signal`, `azimuth`, `elevation`, `fit`, `linkbudget` and `pattern` are parts of this report; they share one computation.

Server-side cache: 5m0s (identical requests share one computation).

Response: [analytics.AntennaReport](#t-analytics-AntennaReport)

Example: `GET /api/antenna/report`

```jsonc
{
  "from": 1793145600000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "signal": {
    "distBinKm": 10,
    "cells": [
      [0, -35, 9],
      [0, -34, 45],
      /* … 1088 more */
    ],
    "total": 6339300,
    "maxCount": 39792,
    "distKm": [5, 15, 25, 35, 45, 55, 65, 75, /* … 40 more */],
    "n": [107278, 306403, 344788, 334425, 305932, 290291, 260920, 247766, /* … 40 more */],
    "median": [-7.844, -13.23, -17.701, -20.427, -22.582, -24.041, -25.184, -25.813, /* … 40 more */],
    "p10": [-14.051, -19.11, -22.63, -24.942, -26.925, -28.266, -29.385, -29.822, /* … 40 more */],
    "p90": [-2.251, -7.579, -13.105, -15.864, -17.887, -19.079, -19.991, -20.638, /* … 40 more */],
    "rssiP01": -34.6392,
    "rssiP50": -28.0339,
    "rssiP99": -6.6279,
    "strongPct": 0.2826,
    "fit": {
      "bins": 34,
      "minKm": 25,
      "maxKm": 355,
      "slopeDbPerDecade": 11.2078,
      "freeSpaceSlope": 20,
      "interceptDb": -3.8986,
      "r2": 0.9719,
      "systemOffsetDb": 14.8762,
      "lineKm": [25, 27.922, 31.186, 34.832, 38.903, 43.451, 48.53, 54.203, /* … 17 more */],
      "measuredDb": [-19.566, -20.105, -20.643, -21.181, -21.719, -22.257, -22.795, -23.333, /* … 17 more */],
      "freeSpaceDb": [-13.083, -14.043, -15.003, -15.963, -16.924, -17.884, -18.844, -19.804, /* … 17 more */],
      "text": "Signal falls by only 11 dB per decade of distance (free space: 20): weak distant signals are missing from the statistics, so the receiver is…"
    }
  },
  "azimuth": {
    "method": "heat-residual",
    "bearingDeg": [1, 3, 5, 7, 9, 11, 13, 15, /* … 172 more */],
    "relDb": [0.983, 0.982, 0.914, 0.864, 0.856, 0.825, 0.699, 0.527, /* … 172 more */],
    "rawDb": [0.98, 1.035, 0.871, 0.871, 0.841, 0.869, 0.723, 0.473, /* … 172 more */],
    "cells": [82, 82, 81, 81, 81, 83, 82, 77, /* … 172 more */],
    "bearings": 180,
    "samples": 4444945,
    "peakDb": 0.9825,
    "peakBrg": 1,
    "minDb": -0.2927,
    "minBrg": 99,
    "spreadDb": 1.0323,
    "distSlopeDb": 9.689
  },
  "elevation": {
    "elevDeg": [-1.75, -1.25, -0.75, -0.25, 0.25, 0.75, 1.25, 1.75, /* … 176 more */],
    "count": [0, 0, 0, 177754, 1084877, 1137067, 889894, 643350, /* … 176 more */],
    "rssi": [null, null, null, -24.37, -28.333, -27.637, -27.235, -26.807, /* … 176 more */],
    "residDb": [null, null, null, 15.185, 15.556, 15.174, 15.137, 14.998, /* … 176 more */],
    "relDb": [null, null, null, 0, -0.052, -0.201, -0.341, -0.464, /* … 176 more */],
    "total": 6339300,
    "peakDeg": -0.25,
    "lowestDeg": -0.4108
  },
  "antennas": [
    {
      "id": "vertical",
      "name": "Vertical",
      "type": "vertical",
      "color": "#38bdf8",
      "info": {
        "kind": "vertical",
        "directional": false,
        "peakGainDbi": 5,
        "azimuthDeg": 0,
        "tiltDeg": 0,
        "hBeamwidthDeg": 360,
        "vBeamwidthDeg": 30,
        "frontToBackDb": 0,
        "sidelobeDb": 13.3,
        "floorDb": 30,
        "estimated": []
      },
      "azGainDbi": [4.881, 4.881, 4.881, 4.881, 4.881, 4.881, 4.881, 4.881, /* … 172 more */],
      "azRelDb": [0, 0, 0, 0, 0, 0, 0, 0, /* … 172 more */],
      "elGainDbi": [4.959, 4.979, 4.993, 4.999, 4.999, 4.993, 4.979, 4.959, /* … 176 more */],
      "elRelDb": [-0.04, -0.02, -0.007, 0, 0, -0.007, -0.02, -0.04, /* … 176 more */],
      "measured": {
        "method": "heat-residual",
        "bearingDeg": [1, 3, 5, 7, 9, 11, 13, 15, /* … 172 more */],
        "relDb": [-0.007, 0.023, -0.022, -0.036, 0.027, 0.103, 0.072, -0.02, /* … 172 more */],
        "rawDb": [-0.03, 0.105, -0.096, -0.016, -0.016, 0.151, 0.113, -0.086, /* … 172 more */],
        "cells": [82, 82, 81, 81, 81, 83, 82, 77, /* … 172 more */],
        "bearings": 180,
        "samples": 4176562,
        "peakDb": 0.2501,
        "peakBrg": 267,
        "minDb": -0.2908,
        "minBrg": 99,
        "spreadDb": 0.2853,
        "distSlopeDb": 9.3797
      },
      "compare": {
        "rmsDb": 0.0926,
        "r": 0,
        "n": 180
      }
    }
  ],
  "yagi": {
    "ok": false,
    "reason": "no clear directional lobe in the measured azimuth pattern",
    "azimuthDeg": 1.1014,
    "beamwidthDeg": 81.0043,
    "gainOverOmniDb": 0.954,
    "confidence": 0.1513,
    "confidenceLabel": "low",
    "explainedPct": 86.8932,
    "rmsDb": 0.0973,
    "fitDb": [0.936, 0.93, 0.91, 0.877, 0.831, 0.771, 0.697, 0.609, /* … 172 more */],
    "pointingErrorDeg": null,
    "beamwidthErrorDeg": null,
    "gainErrorDb": null,
    "text": ""
  },
  "linkBudget": [
    {
      "band": 0,
      "label": "\u003c1k",
      "altFt": 500,
      "horizonKm": 95.6577,
      "predictedKm": 95.6577,
      "limit": "horizon",
      "marginDb": 21.1556,
      "antenna": "vertical",
      "achievedMaxKm": 76.9716,
      "achievedRobustKm": 74.3825,
      "achievedTypicalKm": 53.7654,
      "pctOfPredicted": 77.759
    },
    /* … 6 more */
  ],
  "notes": []
}
```

### `GET /api/antenna/daynight`

Same as `/api/stats/daynight`.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Server-side cache: 5m0s (identical requests share one computation).

Response: [analytics.DayNight](#t-analytics-DayNight)

Example: `GET /api/antenna/daynight`

```jsonc
{
  "from": 1793228400000,
  "to": 1795824000000,
  "ant": "",
  "located": true,
  "utcOffsetSec": 0,
  "hours": 61,
  "phases": [
    {
      "phase": "day",
      "hour": 0,
      "hours": 21,
      "p95Km": 381.4317,
      "maxKm": 465.0208,
      "residDb": 14.5461,
      "aircraft": 69.5714,
      "positions": 139438.3333,
      "p95NormKm": 386.4678
    },
    /* … 2 more */
  ],
  "byHour": [
    {
      "hour": 0,
      "hours": 2,
      "p95Km": 383.6342,
      "maxKm": 455.2541,
      "residDb": 15.0445,
      "aircraft": 16,
      "positions": 27355.5,
      "p95NormKm": 371.5275
    },
    /* … 23 more */
  ],
  "trafficFit": {
    "n": 61,
    "slopeKm": -12.0633,
    "r2": 0.1506,
    "kmPerDoubling": -8.3617
  },
  "nightVsDay": {
    "nA": 21,
    "nB": 31,
    "diffKm": -0.4863,
    "rawDiffKm": 9.3006,
    "diffResidDb": 0.4265,
    "t": -0.1176,
    "p": 0.9069,
    "significant": false
  },
  "conclusions": ["Traffic volume explains 15 % of the hour-to-hour variation in range (-8 km per doubling of the aircraft count); the comparisons below are co…", "No reliable day/night difference in range once traffic is accounted for (-0 km at night, 31 night vs 21 day hours, p = 0.91).", "Hourly values are correlated from one hour to the next, so the p-values are optimistic."]
}
```

### `GET /api/antenna/budget`

Link budget for one aircraft position: path loss, antenna gain, received power, noise floor, SNR, margin.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (default: the first antenna) |
| `az` | bearing from the station, degrees (default: the antenna's boresight) |
| `km` | ground distance (default 200) |
| `altFt` | aircraft altitude (default 35000) |

Power levels are dBm at the antenna terminals (not dBFS). Assumes a 250 W transponder and a 6 dB receiver noise figure.

Response: [server.budgetReply](#t-server-budgetReply)

Example: `GET /api/antenna/budget?ant=vertical&km=250&altFt=38000`

```jsonc
{
  "antenna": "vertical",
  "altFt": 38000,
  "budget": {
    "azimuthDeg": 0,
    "groundKm": 250,
    "altM": 11582.400000000001,
    "slantKm": 250.47591130149382,
    "elevationDeg": 1.7805010701533288,
    "horizonKm": 488.11844467444797,
    "lineOfSight": true,
    "txPowerDbm": 53.979400086720375,
    "eirpDbm": 53.979400086720375,
    "fsplDb": 141.17163248800458,
    "gasLossDb": 0.6226389246196883,
    "polarizationLossDb": 0,
    "miscLossDb": 0,
    "rxAntennaGainDbi": 4.958066877183561,
    "receivedDbm": -82.85680444872034,
    "frontEndLossDb": 0,
    "lnaGainDb": 0,
    "rxInputDbm": -82.85680444872034,
    "systemTempK": 1014.5107946051418,
    "systemNfDb": 5.999999999999998,
    "noiseFloorDbm": -105.52630049280529,
    "sensitivityDbm": -95.52630049280529,
    "snrDb": 22.66949604408495,
    "requiredSnrDb": 10,
    "marginDb": 12.669496044084951,
    "decodable": true
  },
  "systemTempK": 1014.5,
  "systemNfDb": 6,
  "noiseFloorDbm": -105.53,
  "sensitivityDbm": -95.53,
  "maxRangeKm": 488.1,
  "limit": "horizon"
}
```

### `GET /api/radio/pattern`

Modelled antenna pattern cuts: azimuth cut at a given elevation, elevation cut at a given azimuth, predicted range around the compass.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (default: all antennas) |
| `elev` | elevation of the azimuth cut, degrees (default 3) |
| `az` | azimuth of the elevation cut (default: boresight) |
| `step` | angular step, 0.5..30 degrees (default 2) |
| `altFt` | altitude of the range prediction (default 35000) |

The elevation cut runs from -10 to +90 degrees.

Server-side cache: 1m0s (identical requests share one computation).

Response: [server.radioPatternReply](#t-server-radioPatternReply)

Example: `GET /api/radio/pattern?ant=vertical&step=10`

```jsonc
{
  "stepDeg": 10,
  "antennas": [
    {
      "id": "vertical",
      "name": "Vertical",
      "type": "vertical",
      "color": "#38bdf8",
      "info": {
        "kind": "vertical",
        "directional": false,
        "peakGainDbi": 5,
        "azimuthDeg": 0,
        "tiltDeg": 0,
        "hBeamwidthDeg": 360,
        "vBeamwidthDeg": 30,
        "frontToBackDb": 0,
        "sidelobeDb": 13.3,
        "floorDb": 30,
        "estimated": []
      },
      "amslM": 118,
      "elevDeg": 3,
      "azDeg": 0,
      "azimuth": {
        "angleDeg": [0, 10, 20, 30, 40, 50, 60, 70, /* … 28 more */],
        "gainDbi": [4.88, 4.88, 4.88, 4.88, 4.88, 4.88, 4.88, 4.88, /* … 28 more */],
        "relDb": [0, 0, 0, 0, 0, 0, 0, 0, /* … 28 more */],
        "peakDeg": 0,
        "peakDbi": 4.88,
        "beamwidthDeg": null
      },
      "elevation": {
        "angleDeg": [-10, -9, -8, -7, -6, -5, -4, -3, /* … 93 more */],
        "gainDbi": [3.67, 3.92, 4.15, 4.35, 4.52, 4.67, 4.79, 4.88, /* … 93 more */],
        "relDb": [-1.33, -1.08, -0.85, -0.65, -0.48, -0.33, -0.21, -0.12, /* … 93 more */],
        "peakDeg": 0,
        "peakDbi": 5,
        "beamwidthDeg": null
      },
      "rangeAltFt": 35000,
      "rangeKm": [470.3, 470.3, 470.3, 470.3, 470.3, 470.3, 470.3, 470.3, /* … 28 more */],
      "rangeAzDeg": [0, 10, 20, 30, 40, 50, 60, 70, /* … 28 more */],
      "horizonKm": 470.3,
      "rangeTable": [
        {
          "band": 0,
          "label": "\u003c1k",
          "altFt": 500,
          "horizonKm": 95.65769510274018,
          "rangeKm": 95.65769510274018,
          "limit": "horizon",
          "marginDb": 21.15564405066329
        },
        /* … 6 more */
      ],
      "systemNfDb": 6,
      "sensitivityDbm": -95.53,
      "frontEndLossDb": 0
    }
  ]
}
```

### `GET /api/radio/transponders`

Transponder power classes used by the link budget.

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/radio/transponders`

```jsonc
{
  "classes": [
    {
      "name": "70 W",
      "powerW": 70,
      "powerDbm": 48.45098040014257,
      "note": "minimum for aircraft that stay below 15 000 ft (18.5 dBW): light aircraft, gliders"
    },
    /* … 3 more */
  ],
  "freqMHz": 1090
}
```

### `GET /api/antenna/heat.jpg`

The all-time position heat map as a JPEG, by default 7680 x 4320 (8K UHD): heat in the Live Map's colours on a plain background with a graticule, the range rings, the receiver, a scale bar and a caption. No map tiles are fetched.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |
| `w`, `h` | size in pixels, 640..16384 (default 7680 x 4320) |
| `q` | JPEG quality 1..100 (default 90) |
| `light` | `1` = light background |
| `download` | `1` = as an attachment |

Response: `image/jpeg` (8K: about 1-3 MB, a few seconds, about 35 MB of memory); `404` when no position was stored yet; `429` while another export runs


## Weather

### `GET /api/weather/current`

Current conditions, poller status, the stored row of the current hour and the radio-propagation state derived from it.

`current` is null until Open-Meteo was fetched (weather disabled, receiver not located, offline). Units: degC, hPa, km/h, mm, %, m. Show `attribution`.

Server-side cache: 10s (identical requests share one computation).

Response: [server.weatherCurrentReply](#t-server-weatherCurrentReply)

Example: `GET /api/weather/current`

```jsonc
{
  "enabled": true,
  "located": true,
  "status": {
    "enabled": false,
    "located": true,
    "intervalMin": 15,
    "lastAttempt": 0,
    "lastOk": 0,
    "failures": 0,
    "nextPoll": 0,
    "radarEnabled": false,
    "radarLastOk": 0,
    "usage": {
      "minute": 0,
      "hour": 0,
      "day": 0,
      "budget": {
        "PerMinute": 300,
        "PerHour": 2500,
        "PerDay": 5000
      },
      "serverRemaining": -1
    }
  },
  "attribution": "Weather data by Open-Meteo.com (CC BY 4.0)",
  "attributionUrl": "https://open-meteo.com/",
  "fetched": 0,
  "model": "",
  "elevationM": null,
  "current": null,
  "text": "Partly cloudy",
  "hour": {
    "t": 1795820400000,
    "tempC": 8.000000000000002,
    "dewpointC": 4.200000000000002,
    "rh": 81,
    "pressureMsl": 1009.6168587287167,
    "surfacePressure": 996.6168587287167,
    "windKmh": 15.20555813538548,
    "windDir": 263.5547947044711,
    "gustKmh": 26.27407751384731,
    "cloud": 79.19388472502861,
    "cloudLow": 48.24617795314288,
    "cloudMid": 20,
    "cloudHigh": 35,
    "precipMm": 0,
    "rainMm": 0,
    "snowCm": 0,
    "visibilityM": 19500,
    "cape": 0,
    "weatherCode": 2,
    "isDay": 0,
    "nSurface": 316.3,
    "dnDh": -42,
    "kFactor": 1.365,
    "duct": 0
  },
  "hourSource": "observation",
  "atmosphere": {
    "hasProfile": true,
    "nSurface": 316.26848066146584,
    "dNdh": -41.954503732130604,
    "minGradient": -48.70836555983323,
    "minGradientM": 403.3,
    "k": 1.3646774979738856,
    "horizonFactor": 1.0116857829782993,
    "index": 51,
    "class": "standard",
    "ducts": 0,
    "ductStrengthM": 0,
    "ductBaseM": 0,
    "ductTopM": 0,
    "ducting1090": false,
    "inversionC": -1.4,
    "summary": "Standard refraction (dN/dh -42 N/km, k = 1.36): normal radio horizon."
  }
}
```

### `GET /api/weather/hourly`

Hourly weather rows from the database: observed hours up to now and forecast hours from the current hour on.

| Parameter | Meaning |
|---|---|
| `hours` | hours back from now (default 48) |
| `ahead` | hours ahead of now (default 72, max 384) |
| `from`, `to` | explicit range in Unix ms instead |
| `profile` | `1` includes the pressure-level document `profile` in every row |

Row fields are omitted when unknown. `nSurface` (N-units), `dnDh` (N/km), `kFactor`, `duct` (M-units of the strongest duct able to trap 1090 MHz, 0 = none) are the refractivity columns. Forecast rows carry `issued` and `leadH`.

Server-side cache: 1m0s (identical requests share one computation).

Response: [server.weatherHourlyReply](#t-server-weatherHourlyReply)

Example: `GET /api/weather/hourly?hours=6&ahead=6`

```jsonc
{
  "from": 1795800307313,
  "to": 1795843507313,
  "now": 1795821907313,
  "attribution": "Weather data by Open-Meteo.com (CC BY 4.0)",
  "attributionUrl": "https://open-meteo.com/",
  "obs": [
    {
      "t": 1795802400000,
      "tempC": 15.242640687119286,
      "dewpointC": 7.097056274847715,
      "rh": 59.27207793864214,
      "pressureMsl": 1009.3084417028159,
      "surfacePressure": 996.3084417028159,
      "windKmh": 17.398476355045048,
      "windDir": 255.8019188524952,
      "gustKmh": 29.1979684733934,
      "cloud": 66.80136419113303,
      "cloudLow": 40.50085261945814,
      "cloudMid": 20,
      "cloudHigh": 35,
      "precipMm": 0,
      "rainMm": 0,
      "snowCm": 0,
      "visibilityM": 24000,
      "cape": 212.13203435596427,
      "weatherCode": 2,
      "isDay": 0,
      "nSurface": 314.3,
      "dnDh": -40.8,
      "kFactor": 1.351,
      "duct": 0
    },
    /* … 5 more */
  ],
  "forecast": [
    {
      "t": 1795820400000,
      "issued": 1795818600000,
      "leadH": 1,
      "tempC": 8.000000000000002,
      "dewpointC": 4.200000000000002,
      "rh": 81,
      "pressureMsl": 1009.6168587287167,
      "surfacePressure": 996.6168587287167,
      "windKmh": 15.20555813538548,
      "windDir": 263.5547947044711,
      "gustKmh": 26.27407751384731,
      "cloud": 79.19388472502861,
      "cloudLow": 48.24617795314288,
      "cloudMid": 20,
      "cloudHigh": 35,
      "precipMm": 0,
      "rainMm": 0,
      "snowCm": 0,
      "visibilityM": 19500,
      "cape": 0,
      "weatherCode": 2,
      "isDay": 0,
      "nSurface": 316.3,
      "dnDh": -42,
      "kFactor": 1.365,
      "duct": 0
    },
    /* … 6 more */
  ]
}
```

### `GET /api/weather/profile`

Pressure-level profile of the weather model for one hour with its refractivity analysis (N / M profile, layers, ducts).

| Parameter | Meaning |
|---|---|
| `t` | hour, Unix ms (default: now) |

`levels[]`: `hpa`, `t` (degC), `rh` (%), `z` (geopotential m AMSL), `ws` (km/h), `wd` (deg). `t` = 0 and empty arrays when no weather data exists for the hour.

Server-side cache: 1m0s (identical requests share one computation).

Response: [server.weatherProfileReply](#t-server-weatherProfileReply)

Example: `GET /api/weather/profile`

```jsonc
{
  "t": 1795820400000,
  "source": "observation",
  "levels": [
    {
      "hpa": 1000,
      "t": 7.62,
      "rh": 78.9,
      "z": 190.7,
      "ws": 20.5,
      "wd": 240
    },
    /* … 13 more */
  ],
  "surface": {
    "t": 1795820400000,
    "tempC": 8.000000000000002,
    "dewpointC": 4.200000000000002,
    "rh": 81,
    "pressureMsl": 1009.6168587287167,
    "surfacePressure": 996.6168587287167,
    "windKmh": 15.20555813538548,
    "windDir": 263.5547947044711,
    "gustKmh": 26.27407751384731,
    "cloud": 79.19388472502861,
    "cloudLow": 48.24617795314288,
    "cloudMid": 20,
    "cloudHigh": 35,
    "precipMm": 0,
    "rainMm": 0,
    "snowCm": 0,
    "visibilityM": 19500,
    "cape": 0,
    "weatherCode": 2,
    "isDay": 0,
    "nSurface": 316.3,
    "dnDh": -42,
    "kFactor": 1.365,
    "duct": 0
  },
  "atmosphere": {
    "hasProfile": true,
    "nSurface": 316.26848066146584,
    "dNdh": -41.954503732130604,
    "minGradient": -48.70836555983323,
    "minGradientM": 403.3,
    "k": 1.3646774979738856,
    "horizonFactor": 1.0116857829782993,
    "index": 51,
    "class": "standard",
    "ducts": 0,
    "ductStrengthM": 0,
    "ductBaseM": 0,
    "ductTopM": 0,
    "ducting1090": false,
    "inversionC": -1.4,
    "summary": "Standard refraction (dN/dh -42 N/km, k = 1.36): normal radio horizon."
  },
  "points": [
    {
      "heightM": 191,
      "n": 315.6,
      "m": 345.5
    },
    /* … 13 more */
  ],
  "layers": [
    {
      "baseM": 191,
      "topM": 403,
      "dNdh": -51.1,
      "dMdh": 105.9,
      "class": "standard"
    },
    /* … 12 more */
  ],
  "ducts": [],
  "summary": "Standard refraction (dN/dh -42 N/km, k = 1.36): normal radio horizon."
}
```

### `GET /api/weather/radar`

RainViewer precipitation radar frame index for the map overlay.

`past[].url` is a ready XYZ tile template (`{z}/{x}/{y}`), usable as a MapLibre raster source with `maxzoom` 7 and routable through `/tilecache/`. `available` is false until the index was fetched (needs `config.weather.radarOverlay`).

Response: [server.radarReply](#t-server-radarReply)

Example: `GET /api/weather/radar`

```jsonc
{
  "enabled": false,
  "available": false,
  "fetched": 0,
  "generated": 0,
  "host": "",
  "past": [],
  "nowcast": [],
  "tileSize": 256,
  "maxZoom": 7,
  "attribution": "Radar data by RainViewer",
  "attributionUrl": "https://www.rainviewer.com/"
}
```

### `GET /api/weather/impact`

Weather impact on reception: correlations, response curves, ridge regression and ducting episodes.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Default 60 days. Needs 48 h of overlapping range and weather data (`enough`); until then `conclusions` says how much is missing.

Server-side cache: 5m0s (identical requests share one computation).

Response: [analytics.WeatherImpact](#t-analytics-WeatherImpact)

Example: `GET /api/weather/impact`

```jsonc
{
  "from": 1790636400000,
  "to": 1795824000000,
  "ant": "",
  "hours": 61,
  "rangeHours": 61,
  "weatherHours": 62,
  "minHours": 48,
  "enough": true,
  "vars": [
    {
      "key": "temp_c",
      "label": "Temperature",
      "unit": "°C",
      "n": 61,
      "mean": 11.647,
      "std": 4.21
    },
    /* … 15 more */
  ],
  "metrics": ["p95_km", "max_km", "far_count", "resid_db"],
  "correlations": [
    {
      "var": "temp_c",
      "metric": "p95_km",
      "n": 61,
      "pearson": -0.4395,
      "pearsonP": 0.0004,
      "spearman": -0.4308,
      "spearmanP": 0.0005,
      "partial": -0.2675,
      "partialP": 0.0388,
      "bestLagH": 0,
      "bestLagR": -0.4395,
      "significant": false
    },
    /* … 55 more */
  ],
  "responses": [
    {
      "var": "temp_c",
      "metric": "p95_km",
      "bins": [
        {
          "lo": 5,
          "hi": 5.803847577293368,
          "n": 7,
          "xMean": 5.231661106622247,
          "yMean": 403.8061560910849,
          "yStd": 21.34373575998781,
          "yq": [379.27696923755445, 411.1434511434511, 424.4755980018998]
        },
        /* … 7 more */
      ]
    },
    /* … 15 more */
  ],
  "model": {
    "ok": false,
    "reason": "not enough data yet: 59 complete hours, 76 needed for the model",
    "target": "p95_km",
    "n": 59,
    "r2": null,
    "cvR2": null,
    "baseR2": null,
    "weatherR2": null,
    "lambda": null,
    "residStdKm": null,
    "factors": []
  },
  "episodes": [],
  "conclusions": ["Based on 61 hours with both range and weather data.", "No weather variable shows a statistically reliable link with the 95th percentile range once the traffic level is held constant.", "Regression model: not enough data yet: 59 complete hours, 76 needed for the model.", "Less than two weeks of data: seasonal effects and rare conditions are not covered yet. Hourly values are also correlated in time, which make…"]
}
```

### `GET /api/weather/ducting`

Refractivity / ducting time line: per hour the gradient, k-factor, 0..100 propagation index, class and ducts; plus the inversions aircraft measure right now.

| Parameter | Meaning |
|---|---|
| `hours` | hours back (default 48) |
| `ahead` | hours ahead (default 72) |
| `from`, `to` | explicit range in Unix ms instead |

`index`: 50 = standard atmosphere, 85+ = ducting likely. `class`: `sub-refraction` | `standard` | `super-refraction` | `ducting`.

Server-side cache: 1m0s (identical requests share one computation).

Response: [server.ductingReply](#t-server-ductingReply)

Example: `GET /api/weather/ducting?hours=6&ahead=6`

```jsonc
{
  "from": 1795800307323,
  "to": 1795843507323,
  "now": 1795821907323,
  "stationElevM": 110,
  "current": {
    "hasProfile": true,
    "nSurface": 316.26848066146584,
    "dNdh": -41.954503732130604,
    "minGradient": -48.70836555983323,
    "minGradientM": 403.3,
    "k": 1.3646774979738856,
    "horizonFactor": 1.0116857829782993,
    "index": 51,
    "class": "standard",
    "ducts": 0,
    "ductStrengthM": 0,
    "ductBaseM": 0,
    "ductTopM": 0,
    "ducting1090": false,
    "inversionC": -1.4,
    "summary": "Standard refraction (dN/dh -42 N/km, k = 1.36): normal radio horizon."
  },
  "hours": [
    {
      "t": 1795802400000,
      "source": "observation",
      "hasProfile": true,
      "nSurface": 314.34254433314055,
      "dNdh": -40.79209248730842,
      "minGradient": -45.19833982167273,
      "minGradientM": 400.8,
      "k": 1.3510268221880972,
      "horizonFactor": 1.006613191171799,
      "index": 50,
      "class": "standard",
      "ducts": 0,
      "ductStrengthM": 0,
      "ductBaseM": 0,
      "ductTopM": 0,
      "ducting1090": false,
      "inversionC": -1.9,
      "summary": "Standard refraction (dN/dh -41 N/km, k = 1.35): normal radio horizon."
    },
    /* … 11 more */
  ],
  "aircraft": {
    "layers": [],
    "strongest": -1,
    "class": "",
    "note": "No temperature inversion aloft in the aircraft-derived profile."
  },
  "ductHours": 0
}
```

### `POST /api/weather/refresh`

Fetch the weather now instead of waiting for the next poll.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true,"fetched":<ms>,"status":{...}}`; `409` when weather is disabled or the receiver is not located, `502` when the download failed.

Example: `POST /api/weather/refresh` -> `409`

```jsonc
{
  "error": "weather is disabled in the settings"
}
```

(The demo instance used for these examples runs offline.)

## Aircraft-derived meteorology

### `GET /api/met/profile`

Vertical profile of wind and temperature above the station derived from aircraft (Mode S EHS / MRAR), in 1000 ft layers, with its analysis.

| Parameter | Meaning |
|---|---|
| `t` | Unix ms: the analysed profile of that clock hour instead of the current one |

Units: kt, ft (pressure altitude), degrees true (direction the wind blows FROM), degC. Optional values are omitted when unknown.

Server-side cache: 10s (identical requests share one computation).

Response: [met.Profile](#t-met-Profile)

Example: `GET /api/met/profile`

```jsonc
{
  "t": 1795821903984,
  "from": 1795817400000,
  "windowMin": 75.1,
  "nWind": 630,
  "nTemp": 503,
  "levels": [
    {
      "level": 0,
      "altFt": 694,
      "hpa": 988.1,
      "windDir": 250.7,
      "windKt": 6.9,
      "u": 6.51,
      "v": 2.28,
      "nWind": 4,
      "nTemp": 0,
      "windSpread": 2.8
    },
    /* … 35 more */
  ],
  "freezingLevelFt": 7999,
  "tropopause": {
    "altFt": 37022,
    "tempC": -59.3,
    "confirmed": false,
    "depthFt": 3978
  },
  "maxWind": {
    "altFt": 40000,
    "windKt": 62,
    "windDir": 285,
    "jet": true
  },
  "maxShear": {
    "baseFt": 35085,
    "topFt": 36039,
    "ktPer1000Ft": 12.4
  },
  "lapse": [
    {
      "baseFt": 2405,
      "topFt": 3455,
      "lapse": -26.69,
      "class": "inversion"
    },
    /* … 31 more */
  ],
  "inversions": [],
  "windsAloft": [
    {
      "fl": 30,
      "altFt": 3000,
      "windDir": 266,
      "windKt": 10.4,
      "tempC": 7.5,
      "isaDev": -1.5,
      "coded": "2710+08"
    },
    /* … 8 more */
  ]
}
```

### `GET /api/met/history`

Hourly time-height history of the aircraft-derived profiles.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |

Default 24 h. `cells` are ordered by hour, then level.

Server-side cache: 1m0s (identical requests share one computation).

Response: [met.TimeHeight](#t-met-TimeHeight)

Example: `GET /api/met/history`

```jsonc
{
  "from": 1795735507325,
  "to": 1795821907325,
  "hours": [1795734000000, 1795737600000, 1795741200000, 1795744800000, 1795748400000, 1795752000000, 1795755600000, 1795759200000, /* … 17 more */],
  "cells": [
    {
      "t": 1795734000000,
      "level": 2,
      "altFt": 2500,
      "windDir": 282.6,
      "windKt": 8.9,
      "tempC": 8.34,
      "isaDev": -1.7,
      "nWind": 7,
      "nTemp": 12
    },
    /* … 1040 more */
  ]
}
```

### `GET /api/met/observations`

Recent single wind / temperature observations for a map layer.

| Parameter | Meaning |
|---|---|
| `maxAge` | seconds, 10..7200 (default 900) |
| `limit` | 1..5000 (default 1000) |
| `perAircraft` | `1` (default) = only the newest observation per aircraft, `0` = all |

Server-side cache: 2s (identical requests share one computation).

Response: [server.metObsReply](#t-server-metObsReply)

Example: `GET /api/met/observations?limit=5`

```jsonc
{
  "now": 1795821907330,
  "maxAgeS": 900,
  "count": 5,
  "observations": [
    {
      "t": 1795821902247,
      "icao": "4852DE",
      "lat": 50.5493,
      "lon": 8.3397,
      "altFt": 21975,
      "windDir": 260.9,
      "windKt": 16.2,
      "oat": -21.5,
      "q": 0.85
    },
    /* … 4 more */
  ]
}
```

### `GET /api/met/winds`

Winds-aloft table at standard flight levels, jet maximum, strongest shear, freezing level and tropopause from the current profile.

Server-side cache: 10s (identical requests share one computation).

Response: [server.metWindsReply](#t-server-metWindsReply)

Example: `GET /api/met/winds`

```jsonc
{
  "t": 1795821903984,
  "from": 1795817400000,
  "windowMin": 75.1,
  "nWind": 630,
  "nTemp": 503,
  "windsAloft": [
    {
      "fl": 30,
      "altFt": 3000,
      "windDir": 266,
      "windKt": 10.4,
      "tempC": 7.5,
      "isaDev": -1.5,
      "coded": "2710+08"
    },
    /* … 8 more */
  ],
  "maxWind": {
    "altFt": 40000,
    "windKt": 62,
    "windDir": 285,
    "jet": true
  },
  "maxShear": {
    "baseFt": 35085,
    "topFt": 36039,
    "ktPer1000Ft": 12.4
  },
  "freezingLevelFt": 7999,
  "tropopause": {
    "altFt": 37022,
    "tempC": -59.3,
    "confirmed": false,
    "depthFt": 3978
  }
}
```

### `GET /api/met/compare`

Aircraft-derived profile against the weather model (differences are aircraft minus model) and the radio refractivity of measured inversions.

Server-side cache: 1m0s (identical requests share one computation).

Response: [server.metCompareReply](#t-server-metCompareReply)

Example: `GET /api/met/compare`

```jsonc
{
  "t": 1795821907331,
  "modelAvailable": true,
  "modelT": 1795820400000,
  "attribution": "Weather data by Open-Meteo.com (CC BY 4.0)",
  "comparison": {
    "levels": [
      {
        "hpa": 1000,
        "altFt": 364,
        "obsWindDir": 250.7,
        "obsWindKt": 6.9,
        "modelWindDir": 240,
        "modelWindKt": 11.1,
        "dSpeedKt": -4.2,
        "vecDiffKt": 4.5
      },
      /* … 13 more */
    ],
    "nTemp": 12,
    "tempBiasC": 4.38,
    "tempRmsC": 5.41,
    "nWind": 14,
    "speedBiasKt": -3.8,
    "speedRmsKt": 4.7,
    "dirBiasDeg": 6.4,
    "uBiasKt": -2.7,
    "vBiasKt": -1.5,
    "rmsvdKt": 6.9
  },
  "radio": {
    "layers": [],
    "strongest": -1,
    "class": "",
    "note": "No temperature inversion aloft in the aircraft-derived profile."
  }
}
```

### `GET /api/met/stats`

Collector counters and the learned per-airframe heading corrections.

| Parameter | Meaning |
|---|---|
| `limit` | heading corrections returned, 1..500 (default 50) |

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/met/stats`

```jsonc
{
  "biases": [
    {
      "icao": "06A047",
      "meanDeg": -3.65,
      "appliedDeg": -3.52,
      "stdDeg": 0,
      "n": 161,
      "updated": 1795703526000
    },
    /* … 49 more */
  ],
  "stats": {
    "calls": 76,
    "derived": 49,
    "wind": 40,
    "temp": 33,
    "mrarWind": 1,
    "mrarTemp": 1,
    "lowQuality": 0,
    "rejected": {
      "noHeading": 9
    },
    "aircraft": 10,
    "biases": 1677,
    "biasesActive": 1172,
    "lastObs": 1795821903984,
    "dropped": 0
  }
}
```

### `GET /api/met/grid`

Winds and temperatures aloft on a latitude/longitude grid at one flight level, from the aircraft observations of the last minutes (Mode S EHS BDS 5,0/6,0 derivation and BDS 4,4).

| Parameter | Meaning |
|---|---|
| `level` | flight level, e.g. `300` (default 300) |
| `band` | half width of the altitude band in ft, 500..5000 (default 2000) |
| `minutes` | time window, 10..360 (default 60) |
| `cell` | cell size in latitude degrees, 0.1..5 (default 0.5; longitude cells are widened with latitude) |
| `minObs` | observations a cell needs, 1..50 (default 2) |

Each cell: vector-mean wind (`windDir` FROM, `windKt`), mean static air temperature reduced to the level with the standard lapse rate (`tempC`) and its deviation from the ICAO standard atmosphere (`isaDevC`), counts. Cells without observations are absent: nothing is interpolated.

Server-side cache: 10s (identical requests share one computation).

Response: [met.Grid](#t-met-Grid)

Example: `GET /api/met/grid?level=300`

```jsonc
{
  "from": 1795818307334,
  "to": 1795821907334,
  "levelFt": 30000,
  "bandFt": 2000,
  "cellDeg": 0.5,
  "samples": 48,
  "cells": [
    {
      "lat": 47.25,
      "lon": 4.79,
      "windDir": 272,
      "windKt": 40.1,
      "nWind": 2,
      "nTemp": 1,
      "nAc": 1,
      "altFt": 30000
    },
    /* … 12 more */
  ]
}
```

### `GET /api/met/hazards`

BDS 4,5 meteorological hazard reports (turbulence, wind shear, microburst, icing, wake vortex: 0 nil .. 3 severe; static air temperature, pressure, radio height), newest first, at most one per aircraft per minute.

| Parameter | Meaning |
|---|---|
| `hours` | the last N hours, up to 48 (default 6) |

In memory only (the last 2000). BDS 4,5 has no identifying bits, so a reply is only taken as 4,5 when register inference picks it: rare, and less certain than the other registers. The reports never enter the profiles or the grid.

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/met/hazards?hours=6`

```jsonc
{
  "hazards": []
}
```

## Forecasts

### `GET /api/forecast/traffic`

Traffic forecast for the next 72 h: aircraft count and message rate with 80 % intervals, busiest and quietest hours.

Server-side cache: 1m0s (identical requests share one computation).

Response: [predict.TrafficForecast](#t-predict-TrafficForecast)

Example: `GET /api/forecast/traffic`

```jsonc
{
  "product": "traffic",
  "generated": 1795821895014,
  "method": "Hour-of-day profile (weighted median, 14-day half-life) plus the recent anomaly continued by an AR(3) model; intervals from the spread of th…",
  "level": "blend",
  "confidence": "low",
  "confidenceValue": 0.15,
  "dataHours": 60,
  "notes": ["needs 6 more days of data for a weekly (weekday/weekend) profile"],
  "utcOffsetSec": 3600,
  "hours": [
    {
      "t": 1795820400000,
      "aircraft": {
        "value": 9.2,
        "lo": 7.2,
        "hi": 11.2,
        "confidence": 0.15
      },
      "msgRate": {
        "value": 39.1,
        "lo": 34.3,
        "hi": 43.9,
        "confidence": 0.15
      }
    },
    /* … 71 more */
  ],
  "busiest": [
    {
      "t": 1796029200000,
      "aircraft": 45,
      "msgRate": 190.1,
      "inHours": 58
    },
    /* … 4 more */
  ],
  "quietest": {
    "t": 1795820400000,
    "aircraft": 9.2,
    "msgRate": 39.1,
    "inHours": 0
  },
  "recent": [
    {
      "t": 1795647600000,
      "aircraft": 10,
      "msgRate": 38
    },
    /* … 47 more */
  ]
}
```

### `GET /api/forecast/reception`

Reception forecast for the next 72 h: p95 / max range, 0..100 index, far-reception probability, atmosphere and drivers per hour.

Server-side cache: 1m0s (identical requests share one computation).

Response: [predict.ReceptionForecast](#t-predict-ReceptionForecast)

Example: `GET /api/forecast/reception`

```jsonc
{
  "product": "reception",
  "generated": 1795821895014,
  "method": "Physics only: Open-Meteo forecast profile -\u003e refractivity N, gradient dN/dh, k-factor and ducts (ITU-R P.453/P.834); range = line of sight f…",
  "level": "physics",
  "confidence": "low",
  "confidenceValue": 0.3,
  "dataHours": 60,
  "notes": ["needs 12 more hours of data for a learned range model (hours with both range and weather data)", "far-reception probability is physics-only until enough far-reception hours have been logged"],
  "hours": [
    {
      "t": 1795820400000,
      "leadH": 0,
      "source": "forecast",
      "p95Km": {
        "value": 366,
        "lo": 282,
        "hi": 451,
        "confidence": 0.3
      },
      "maxKm": {
        "value": 430,
        "lo": 320,
        "hi": 540,
        "confidence": 0.3
      },
      "index": 51,
      "label": "normal",
      "farProbability": 0.008,
      "confidence": 0.3,
      "aircraft": 9.2,
      "atmosphere": {
        "hasProfile": true,
        "nSurface": 316.3,
        "dNdh": -42,
        "minGradient": -48.7,
        "minGradientM": 403,
        "k": 1.365,
        "horizonFactor": 1.012,
        "index": 51,
        "class": "standard",
        "ducts": 0,
        "ductStrengthM": 0,
        "ductBaseM": 0,
        "ductTopM": 0,
        "ducting1090": false,
        "inversionC": -1.4
      },
      "factors": [
        {
          "key": "traffic",
          "text": "little traffic expected (about 9 aircraft): fewer distant targets",
          "effectKm": -19.3
        }
      ],
      "summary": "Normal conditions: p95 range about 366 km; little traffic expected (about 9 aircraft): fewer distant targets."
    },
    /* … 71 more */
  ],
  "farKm": 545,
  "typicalP95Km": 385,
  "typicalMaxKm": 473,
  "antennaAmslM": 118,
  "bands": [
    {
      "band": 0,
      "label": "\u003c1k",
      "altFt": 500,
      "standardKm": 95.7,
      "nowKm": 96.8,
      "factor": 1.012
    },
    /* … 6 more */
  ],
  "best": {
    "t": 1796011200000,
    "leadH": 53,
    "source": "forecast",
    "p95Km": {
      "value": 389,
      "lo": 273,
      "hi": 505,
      "confidence": 0.23
    },
    "maxKm": {
      "value": 480,
      "lo": 321,
      "hi": 639,
      "confidence": 0.23
    },
    "index": 55,
    "label": "normal",
    "farProbability": 0.02,
    "confidence": 0.23,
    "aircraft": 24,
    "atmosphere": {
      "hasProfile": true,
      "nSurface": 318.2,
      "dNdh": -52,
      "minGradient": -70.6,
      "minGradientM": 466,
      "k": 1.496,
      "horizonFactor": 1.059,
      "index": 55,
      "class": "standard",
      "ducts": 0,
      "ductStrengthM": 0,
      "ductBaseM": 0,
      "ductTopM": 0,
      "ducting1090": false,
      "inversionC": 0.8
    },
    "factors": [
      {
        "key": "refraction",
        "text": "stronger downward bending of radio waves than usual (dN/dh -52 N/km, k = 1.50)",
        "effectKm": 4.4
      }
    ],
    "summary": "Normal conditions: p95 range about 389 km; stronger downward bending of radio waves than usual (dN/dh -52 N/km, k = 1.50)."
  },
  "recent": [
    {
      "t": 1795647600000,
      "p95Km": 409.6,
      "maxKm": 466.3,
      "index": 50
    },
    /* … 47 more */
  ],
  "weatherHours": 72
}
```

### `GET /api/forecast/weather`

Local weather outlook: bias-corrected hourly forecast, Zambretti, detected patterns (fronts, fog, inversion, convection), analogue forecast.

Server-side cache: 1m0s (identical requests share one computation).

Response: [predict.WeatherOutlook](#t-predict-WeatherOutlook)

Example: `GET /api/forecast/weather`

```jsonc
{
  "product": "weather",
  "generated": 1795821895014,
  "method": "Open-Meteo forecast as issued (no verified local bias yet), plus an independent Zambretti barometric forecast with a Kalman-filtered pressur…",
  "level": "physics",
  "confidence": "medium",
  "confidenceValue": 0.55,
  "dataHours": 61,
  "notes": ["bias correction needs about 24 logged forecasts per lead-time class that verifiably improve on the raw forecast", "needs 8 more days of data for the analogue forecast"],
  "attribution": "Weather data by Open-Meteo.com (CC BY 4.0)",
  "attributionUrl": "https://open-meteo.com/",
  "current": {
    "t": 1795820400000,
    "tempC": 8,
    "dewPointC": 4.2,
    "rh": 81,
    "pressureMsl": 1009.6,
    "windKmh": 15.2,
    "windDir": 264,
    "wind": "W",
    "cloud": 79,
    "visibilityM": 19500,
    "text": "Partly cloudy",
    "change3h": 0.2,
    "tendency": "steady",
    "pressure6h": {
      "value": 1010,
      "lo": 1008.4,
      "hi": 1011.6,
      "confidence": 0.6
    }
  },
  "hours": [
    {
      "t": 1795820400000,
      "leadH": 0,
      "tempC": 8,
      "tempRawC": 8,
      "dewPointC": 4.2,
      "rh": 81,
      "pressureMsl": 1009.6,
      "windKmh": 15.2,
      "windDir": 264,
      "gustKmh": 26.3,
      "cloud": 79,
      "cloudLow": 48,
      "precipMm": 0,
      "precipProb": 20,
      "visibilityM": 19500,
      "cape": 0,
      "weatherCode": 2,
      "text": "Partly cloudy",
      "isDay": false,
      "fogRisk": 0,
      "inversionRisk": 0,
      "convectiveRisk": 0,
      "corrected": false
    },
    /* … 72 more */
  ],
  "bias": [],
  "zambretti": {
    "number": 10,
    "letter": "K",
    "text": "Fairly fine, showers likely",
    "trend": "steady",
    "exceptional": false,
    "pressureMsl": 1009.6,
    "change3h": 0.2,
    "tendency": "steady",
    "windDir": 264,
    "wind": "W",
    "season": "winter",
    "hemisphere": "north",
    "outlook": "Barometer 1010 hPa, steady, wind W: fairly fine, showers likely over the next 12-24 hours.",
    "confidence": 0.55
  },
  "patterns": [
    {
      "kind": "pressure",
      "severity": 0,
      "probability": null,
      "title": "Pressure steady",
      "detail": "Kalman-filtered tendency +0.2 hPa per 3 hours."
    },
    /* … 1 more */
  ]
}
```

### `GET /api/forecast/outlook`

Digest of all forecast products for a home page: nowcast, the next 12 h of each product, highlights.

Server-side cache: 1m0s (identical requests share one computation).

Response: [server.outlookReply](#t-server-outlookReply)

Example: `GET /api/forecast/outlook`

```jsonc
{
  "generated": 1795821907340,
  "status": {
    "fitted": 1795821895000,
    "built": 1795821895014,
    "dataHours": 60
  },
  "nowcast": {
    "generated": 1795821895014,
    "t": 1795820400000,
    "index": 51,
    "label": "normal",
    "trend": "steady",
    "trendDelta": 0,
    "next6h": [
      {
        "t": 1795820400000,
        "index": 51,
        "label": "normal"
      },
      /* … 6 more */
    ],
    "p95Km": {
      "value": 366,
      "lo": 282,
      "hi": 451,
      "confidence": 0.3
    },
    "farProbability": 0.011,
    "aircraft": 9.2,
    "sentence": "Reception normal (index 51), steady for the next 6 h; expected range about 366 km.",
    "confidence": "low",
    "level": "physics"
  },
  "traffic": {
    "level": "blend",
    "confidence": "low",
    "busiest": [
      {
        "t": 1796029200000,
        "aircraft": 45,
        "msgRate": 190.1,
        "inHours": 58
      },
      /* … 4 more */
    ],
    "quietest": {
      "t": 1795820400000,
      "aircraft": 9.2,
      "msgRate": 39.1,
      "inHours": 0
    },
    "next": [
      {
        "t": 1795820400000,
        "aircraft": {
          "value": 9.2,
          "lo": 7.2,
          "hi": 11.2,
          "confidence": 0.15
        },
        "msgRate": {
          "value": 39.1,
          "lo": 34.3,
          "hi": 43.9,
          "confidence": 0.15
        }
      },
      /* … 11 more */
    ]
  },
  "reception": {
    "level": "physics",
    "confidence": "low",
    "typicalP95Km": 385,
    "farKm": 545,
    "best": {
      "t": 1796011200000,
      "leadH": 53,
      "source": "forecast",
      "p95Km": {
        "value": 389,
        "lo": 273,
        "hi": 505,
        "confidence": 0.23
      },
      "maxKm": {
        "value": 480,
        "lo": 321,
        "hi": 639,
        "confidence": 0.23
      },
      "index": 55,
      "label": "normal",
      "farProbability": 0.02,
      "confidence": 0.23,
      "aircraft": 24,
      "atmosphere": {
        "hasProfile": true,
        "nSurface": 318.2,
        "dNdh": -52,
        "minGradient": -70.6,
        "minGradientM": 466,
        "k": 1.496,
        "horizonFactor": 1.059,
        "index": 55,
        "class": "standard",
        "ducts": 0,
        "ductStrengthM": 0,
        "ductBaseM": 0,
        "ductTopM": 0,
        "ducting1090": false,
        "inversionC": 0.8
      },
      "factors": [
        {
          "key": "refraction",
          "text": "stronger downward bending of radio waves than usual (dN/dh -52 N/km, k = 1.50)",
          "effectKm": 4.4
        }
      ],
      "summary": "Normal conditions: p95 range about 389 km; stronger downward bending of radio waves than usual (dN/dh -52 N/km, k = 1.50)."
    },
    "bands": [
      {
        "band": 0,
        "label": "\u003c1k",
        "altFt": 500,
        "standardKm": 95.7,
        "nowKm": 96.8,
        "factor": 1.012
      },
      /* … 6 more */
    ],
    "next": [
      {
        "t": 1795820400000,
        "leadH": 0,
        "source": "forecast",
        "p95Km": {
          "value": 366,
          "lo": 282,
          "hi": 451,
          "confidence": 0.3
        },
        "maxKm": {
          "value": 430,
          "lo": 320,
          "hi": 540,
          "confidence": 0.3
        },
        "index": 51,
        "label": "normal",
        "farProbability": 0.008,
        "confidence": 0.3,
        "aircraft": 9.2,
        "atmosphere": {
          "hasProfile": true,
          "nSurface": 316.3,
          "dNdh": -42,
          "minGradient": -48.7,
          "minGradientM": 403,
          "k": 1.365,
          "horizonFactor": 1.012,
          "index": 51,
          "class": "standard",
          "ducts": 0,
          "ductStrengthM": 0,
          "ductBaseM": 0,
          "ductTopM": 0,
          "ducting1090": false,
          "inversionC": -1.4
        },
        "factors": [
          {
            "key": "traffic",
            "text": "little traffic expected (about 9 aircraft): fewer distant targets",
            "effectKm": -19.3
          }
        ],
        "summary": "Normal conditions: p95 range about 366 km; little traffic expected (about 9 aircraft): fewer distant targets."
      },
      /* … 11 more */
    ]
  },
  "weather": {
    "level": "physics",
    "confidence": "medium",
    "current": {
      "t": 1795820400000,
      "tempC": 8,
      "dewPointC": 4.2,
      "rh": 81,
      "pressureMsl": 1009.6,
      "windKmh": 15.2,
      "windDir": 264,
      "wind": "W",
      "cloud": 79,
      "visibilityM": 19500,
      "text": "Partly cloudy",
      "change3h": 0.2,
      "tendency": "steady",
      "pressure6h": {
        "value": 1010,
        "lo": 1008.4,
        "hi": 1011.6,
        "confidence": 0.6
      }
    },
    "zambretti": {
      "number": 10,
      "letter": "K",
      "text": "Fairly fine, showers likely",
      "trend": "steady",
      "exceptional": false,
      "pressureMsl": 1009.6,
      "change3h": 0.2,
      "tendency": "steady",
      "windDir": 264,
      "wind": "W",
      "season": "winter",
      "hemisphere": "north",
      "outlook": "Barometer 1010 hPa, steady, wind W: fairly fine, showers likely over the next 12-24 hours.",
      "confidence": 0.55
    },
    "patterns": [
      {
        "kind": "pressure",
        "severity": 0,
        "probability": null,
        "title": "Pressure steady",
        "detail": "Kalman-filtered tendency +0.2 hPa per 3 hours."
      },
      /* … 1 more */
    ],
    "next": [
      {
        "t": 1795820400000,
        "leadH": 0,
        "tempC": 8,
        "tempRawC": 8,
        "dewPointC": 4.2,
        "rh": 81,
        "pressureMsl": 1009.6,
        "windKmh": 15.2,
        "windDir": 264,
        "gustKmh": 26.3,
        "cloud": 79,
        "cloudLow": 48,
        "precipMm": 0,
        "precipProb": 20,
        "visibilityM": 19500,
        "cape": 0,
        "weatherCode": 2,
        "text": "Partly cloudy",
        "isDay": false,
        "fogRisk": 0,
        "inversionRisk": 0,
        "convectiveRisk": 0,
        "corrected": false
      },
      /* … 11 more */
    ]
  }
}
```

### `GET /api/forecast/verification`

How good the forecasts are: backtest skill per model, forecast-versus-actual series, what data is still missing.

Server-side cache: 1m0s (identical requests share one computation).

Response: [predict.Verification](#t-predict-Verification)

Example: `GET /api/forecast/verification`

```jsonc
{
  "generated": 1795821895014,
  "fitted": 1795821895000,
  "summary": "0 of 6 models fully trained, 0 learning, 6 waiting for data.",
  "models": [
    {
      "product": "traffic",
      "target": "aircraft",
      "title": "Aircraft count",
      "status": "insufficient",
      "dataHours": 60,
      "needHours": 672,
      "needs": "needs 36 more hours of data for a first backtest",
      "method": "Hour-of-week profile with exponential forgetting + AR(3) anomaly correction; rolling-origin backtest over 72 h horizons, refitted at every o…"
    },
    /* … 5 more */
  ],
  "bias": [],
  "analogue": [],
  "live": []
}
```

### `GET /api/forecast/nowcast`

One-line reception nowcast for a top bar: index, label, trend, sentence.

Server-side cache: 10s (identical requests share one computation).

Response: [predict.Nowcast](#t-predict-Nowcast)

Example: `GET /api/forecast/nowcast`

```jsonc
{
  "generated": 1795821895014,
  "t": 1795820400000,
  "index": 51,
  "label": "normal",
  "trend": "steady",
  "trendDelta": 0,
  "next6h": [
    {
      "t": 1795820400000,
      "index": 51,
      "label": "normal"
    },
    /* … 6 more */
  ],
  "p95Km": {
    "value": 366,
    "lo": 282,
    "hi": 451,
    "confidence": 0.3
  },
  "farProbability": 0.011,
  "aircraft": 9.2,
  "sentence": "Reception normal (index 51), steady for the next 6 h; expected range about 366 km.",
  "confidence": "low",
  "level": "physics"
}
```

### `GET /api/forecast/status`

State of the forecaster (last fit, last build, hours of history, last error).

Response: [predict.Status](#t-predict-Status)

Example: `GET /api/forecast/status`

```jsonc
{
  "fitted": 1795821895000,
  "built": 1795821895014,
  "dataHours": 60
}
```

### `POST /api/forecast/refit`

Refit all forecast models now (normally hourly).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true,"status":{...}}`

Example: `POST /api/forecast/refit` -> `200`

```jsonc
{
  "ok": true,
  "status": {
    "fitted": 1795821907000,
    "built": 1795821907341,
    "dataHours": 60
  }
}
```

## Timing

### `GET /api/timing/ntp`

NTP reference: configuration, status, newest sample and statistics (last hour, 24 h, everything in memory).

Offsets are true UTC minus host clock, in ms. `driftPpm` is null with fewer than 4 samples.

Response: [server.ntpReply](#t-server-ntpReply)

Example: `GET /api/timing/ntp`

```jsonc
{
  "config": {
    "enabled": false,
    "server": "",
    "intervalSec": 64
  },
  "status": {
    "enabled": false,
    "server": "",
    "synced": false,
    "offsetMs": 0,
    "delayMs": 0,
    "jitterMs": 0,
    "stratum": 0,
    "lastOk": 0
  },
  "latest": null,
  "hour": {
    "count": 54,
    "spanSec": 3392,
    "offsetMeanMs": 3.253705091700066,
    "offsetMinMs": 2.4814062525512335,
    "offsetMaxMs": 3.986369084308231,
    "offsetStdMs": 0.4196363799252475,
    "delayMeanMs": 20.4453841094778,
    "delayMinMs": 18.27972027715973,
    "delayMaxMs": 21.98835035104505,
    "driftPpm": -0.3910614939360421,
    "polls": 0,
    "failures": 0,
    "steps": 0
  },
  "day": {
    "count": 1348,
    "spanSec": 86208,
    "offsetMeanMs": 3.0535949962015025,
    "offsetMinMs": 1.3002518555333267,
    "offsetMaxMs": 4.699800843843744,
    "offsetStdMs": 1.0648916994972648,
    "delayMeanMs": 20.548172841970555,
    "delayMinMs": 18.003296833161713,
    "delayMaxMs": 21.99999944118751,
    "driftPpm": 0.004846100758394489,
    "polls": 0,
    "failures": 0,
    "steps": 0
  },
  "all": {
    "count": 3375,
    "spanSec": 215936,
    "offsetMeanMs": 3.0489760213600183,
    "offsetMinMs": 1.3002117520355485,
    "offsetMaxMs": 4.699800843843744,
    "offsetStdMs": 1.0665107769499695,
    "delayMeanMs": 20.5455752660646,
    "delayMinMs": 18,
    "delayMaxMs": 21.99999944118751,
    "driftPpm": -0.00003988130727190057,
    "polls": 0,
    "failures": 0,
    "steps": 0
  }
}
```

### `GET /api/timing/history`

NTP offset / delay history.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `limit` | maximum number of samples, evenly thinned, 10..20000 (default 3000) |

Default 24 h. Ranges older than what is in memory (about 6 days) come from the database with fewer fields filled.

Server-side cache: 10s (identical requests share one computation).

Response: [server.ntpHistoryReply](#t-server-ntpHistoryReply)

Example: `GET /api/timing/history`

```jsonc
{
  "from": 1795735507353,
  "to": 1795821907353,
  "source": "memory",
  "total": 1348,
  "samples": [
    {
      "t": 1795735533000,
      "server": "pool.ntp.org",
      "offsetMs": 1.6980673543500509,
      "delayMs": 20.092238106600732,
      "stratum": 2,
      "leap": 0,
      "rootDelayMs": 0,
      "rootDispMs": 0,
      "smoothedMs": 1.6980673543500509,
      "jitterMs": 0,
      "sent": 1,
      "replies": 1
    },
    /* … 1347 more */
  ]
}
```

### `GET /api/timing/latency`

Per-feed latency between the receiver's GPS frame timestamps and the (NTP-corrected) host clock: percentiles, histogram, per-minute history; plus the latency-chain summary and stage histograms.

Only feeds with GPS timestamps (Radarcape in GPS mode, the demo feed) appear in `feeds`. `chain` (`latency.Summary`, null when unavailable): `now, windowSec, receiverSet, clockSynced, clockOffsetMs, aircraft, aircraftT1, shareT1, msgShareT1, stages[], counts{}, feeds[], findings[]`; `stages[]`: `{key: propagation|receiver|processing|transponder|ageDelta|airToSystem|endToEnd, label, description, count, minMs, p50Ms, p95Ms, p99Ms, maxMs, meanMs, uncertaintyMs, exact}`; `feeds[]`: `{feedId, samples, negative, absurd, lastMs, floorMs, jitterMs, driftUsPerMin, intervals, status: ok|warn|bad|unknown, issues[]}`; `findings[]`: `{level: info|warn|bad, key, text}`. `histograms[]`: `{key, label, unit:"ms", scale:"log"|"linear", count, bins:[{lo, hi, n}]}` (log bins are signed; extra keys `aircraftTransponder`, `aircraftAgeJitter`). Every number of `chain` / `histograms` is null instead of NaN. The station position used for propagation is `station` of `/api/status` (configured receiver, else the receiver's GPS position).

Response: [server.latencyReply](#t-server-latencyReply)

Example: `GET /api/timing/latency`

```jsonc
{
  "now": 1795821907360,
  "clockOffsetMs": 0,
  "clockSynced": false,
  "feeds": [
    {
      "feedId": "demo",
      "total": 2306,
      "samples": 756,
      "windowS": 44.2,
      "lastMs": 1.744,
      "meanMs": 2.552,
      "stdMs": 1.904,
      "minMs": 1.665,
      "maxMs": 27.485,
      "p01Ms": 1.679,
      "p05Ms": 1.736,
      "p25Ms": 1.905,
      "p50Ms": 2.204,
      "p75Ms": 2.697,
      "p95Ms": 3.71,
      "p99Ms": 8.328,
      "hist": [
        {
          "loMs": 1.673,
          "hiMs": 2.026,
          "count": 280
        },
        /* … 39 more */
      ],
      "history": [
        {
          "t": 1795821840000,
          "samples": 552,
          "minMs": 1.668,
          "p05Ms": 1.738,
          "p50Ms": 2.186,
          "p95Ms": 3.653
        },
        /* … 1 more */
      ],
      "clockOffsetMs": 0,
      "clockSynced": false,
      "ref": "startOfFrame"
    }
  ],
  "chain": {
    "now": 1795821907335,
    "windowSec": 900,
    "receiverSet": true,
    "clockSynced": false,
    "clockOffsetMs": 0,
    "hostOffsetMs": 1.6644,
    "hostOffsetCorrected": false,
    "aircraft": 9,
    "aircraftT1": 1,
    "shareT1": 0.1111,
    "msgShareT1": 0.0718,
    "stages": [
      {
        "key": "propagation",
        "label": "Propagation",
        "description": "Slant range from the antenna to the aircraft divided by the radio speed (c / (1+N·1e-6), tropospheric refractivity included).",
        "count": 397,
        "minMs": 0.2402,
        "p50Ms": 0.3528,
        "p95Ms": 1.0482,
        "p99Ms": 1.152,
        "maxMs": 1.152,
        "meanMs": 0.4942,
        "uncertaintyMs": 0.002,
        "exact": true
      },
      /* … 6 more */
    ],
    "counts": {
      "frames": 2408,
      "gpsFrames": 2300,
      "noGps": 0,
      "mlatExcluded": 108,
      "positions": 418,
      "positionsRanged": 397,
      "positionsT1": 30,
      "positionsT0": 388,
      "negativeDelay": 0,
      "absurdDelay": 0,
      "oneSecondAhead": 0,
      "gpsTimeNotUtc": 0,
      "legacyRange": 0,
      "processingGated": 30,
      "rangeGated": 0,
      "t1OutOfSpec": 0,
      "t1UnexpectedTc": 0,
      "agePairs": 377,
      "ageTurnsSkipped": 0,
      "ageStaleRepeats": 0,
      "agePairsGated": 0,
      "frameEndCorrUs": 64,
      "receiverDelayMinMs": -500,
      "receiverDelayMaxMs": 5000
    },
    "feeds": [
      {
        "feedId": "demo",
        "samples": 2281,
        "negative": 0,
        "absurd": 0,
        "lastMs": 1.8792,
        "floorMs": 1.6644,
        "jitterMs": 0.0016,
        "driftUsPerMin": null,
        "intervals": 5,
        "status": "ok",
        "issues": [],
        "minima": [],
        "ref": "startOfFrame"
      }
    ],
    "findings": [
      {
        "level": "info",
        "key": "propagation",
        "text": "Radio propagation takes 353 µs at the median (240–1152 µs): light needs 3.34 µs per km."
      },
      /* … 7 more */
    ]
  },
  "histograms": [
    {
      "key": "propagation",
      "label": "Propagation",
      "unit": "ms",
      "scale": "log",
      "count": 397,
      "bins": [
        {
          "lo": 0.2371,
          "hi": 0.2512,
          "n": 56
        },
        /* … 27 more */
      ]
    },
    /* … 8 more */
  ]
}
```

### `GET /api/timing/latency/aircraft`

Per-aircraft latency table (aircraft with positions, most positions first): transponder latency of T=1 positions, estimated position age, age jitter, propagation, flags.

`{aircraft: [AircraftRow]}`; `AircraftRow`: `icao, firstSeen, lastSeen, n, positions, t1, t0, tFlag, lastTc, xpdrN, xpdrMeanMs, xpdrStdMs, xpdrMinMs, xpdrMaxMs, agePairs, ageJitterMs, ageModelMs, ageCheckMs, positionAgeMs, ageExact, propMeanUs, propLastUs, slantKm, flags[]`. Flags: `t1-not-synchronised`, `t1-out-of-tolerance`, `t1-outliers`, `t1-unexpected-tc`, `age-over-0.6s-est`, `age-jitter-high`, `stale-positions`. Unknown numbers are null.

Response: [server.latencyAircraftReply](#t-server-latencyAircraftReply)

Example: `GET /api/timing/latency/aircraft`

```jsonc
{
  "aircraft": [
    {
      "icao": "485FD9",
      "firstSeen": 1795821863225,
      "lastSeen": 1795821907362,
      "n": 236,
      "positions": 84,
      "t1": 0,
      "t0": 84,
      "tFlag": false,
      "lastTc": 12,
      "xpdrN": 0,
      "xpdrMeanMs": null,
      "xpdrStdMs": null,
      "xpdrMinMs": null,
      "xpdrMaxMs": null,
      "agePairs": 81,
      "ageJitterMs": 1.7825,
      "ageModelMs": 3.0874,
      "ageCheckMs": null,
      "positionAgeMs": 3.0874,
      "ageExact": false,
      "propMeanUs": 248.2447,
      "propLastUs": 257.4179,
      "slantKm": 77.1602,
      "flags": []
    },
    /* … 8 more */
  ]
}
```

### `GET /api/timing/latency/aircraft/{icao}`

One aircraft's latency row and the stage waterfall of its latest position message.

`{row: AircraftRow, waterfall: Waterfall}`. `Waterfall`: `icao, t, df, tc, tFlag, cprOdd, feed, exact, slantKm, elevDeg, events[], stages[], totalMs, totalUncMs, notes[]`; `events[]`: `{key: applicability|transmit|arrival|received|handled, label, utc (RFC 3339 ns), atMs (relative to applicability), uncMs}`; `stages[]`: `{key: positionAge|propagation|receiver|processing, label, ns, ms, uncMs, method: exact|estimated|assumed|measured|gated|computed|unavailable}`. `{icao}` is 6 hex digits. `400` for an invalid address, `404 {"error":"unknown aircraft"}` when the aircraft is not in the latency table.

Response: [server.latencyAircraftDetail](#t-server-latencyAircraftDetail)

Example: `GET /api/timing/latency/aircraft/3C473C`

```jsonc
{
  "row": {
    "icao": "3C473C",
    "firstSeen": 1795821863140,
    "lastSeen": 1795821906338,
    "n": 73,
    "positions": 30,
    "t1": 30,
    "t0": 0,
    "tFlag": true,
    "lastTc": 10,
    "xpdrN": 28,
    "xpdrMeanMs": 49.8722,
    "xpdrStdMs": 53.357,
    "xpdrMinMs": null,
    "xpdrMaxMs": null,
    "agePairs": 23,
    "ageJitterMs": 58.7709,
    "ageModelMs": 101.7941,
    "ageCheckMs": 11.8504,
    "positionAgeMs": 49.8722,
    "ageExact": true,
    "propMeanUs": 1047.5696,
    "propLastUs": 1063.1503,
    "slantKm": 318.6609,
    "flags": []
  },
  "waterfall": {
    "icao": "3C473C",
    "t": 1795821904091,
    "df": 17,
    "tc": 10,
    "tFlag": true,
    "cprOdd": false,
    "feed": "demo",
    "exact": true,
    "slantKm": 318.6609,
    "elevDeg": 0.3041,
    "events": [
      {
        "key": "applicability",
        "label": "Time of applicability",
        "utc": "2026-11-27T23:25:04.000000000Z",
        "atMs": 0,
        "uncMs": 0
      },
      /* … 3 more */
    ],
    "stages": [
      {
        "key": "positionAge",
        "label": "Position age at transmit",
        "ns": 88142313,
        "ms": 88.1423,
        "uncMs": 0,
        "method": "exact"
      },
      /* … 2 more */
    ],
    "totalMs": null,
    "totalUncMs": null,
    "notes": ["host clock not NTP-corrected: receiver→host delay contains the host clock error"]
  }
}
```

### `GET /api/timing/latency/feeds/{id}`

Receiver-delay minute series of one feed (up to 24 h, restored from the database after a restart).

| Parameter | Meaning |
|---|---|
| `since` | only minutes at or after this time (Unix ms; default all) |

`{feed, feeds: [every tracked feed id], series: [FeedMinute]}`, oldest first; `FeedMinute`: `{t (Unix ms), n, minMs, p50Ms, p95Ms, maxMs, meanMs, floorMs, jitterMs, neg, absurd}` (null = unknown). `404 {"error":"unknown feed"}` for a feed without latency data.

Response: [server.latencyFeedReply](#t-server-latencyFeedReply)

Example: `GET /api/timing/latency/feeds/demo`

```jsonc
{
  "feed": "demo",
  "feeds": ["demo"],
  "series": [
    {
      "t": 1795821840000,
      "n": 1929,
      "minMs": 1.6644,
      "p50Ms": 2.2122,
      "p95Ms": 3.8121,
      "maxMs": 27.4851,
      "meanMs": 2.5438,
      "floorMs": 1.6644,
      "jitterMs": 0.0016,
      "neg": 0,
      "absurd": 0
    }
  ]
}
```

### `GET /api/timing/ntpcheck`

NTP / receiver GPS clock consistency: NTP server quality, offset uncertainty, per-feed receiver-delay clock filter (10 s minima, floor, jitter, drift), issues and an explanation.

`{ntp: {enabled, synced, server, stratum, refId, leap, offsetMs, delayMs, jitterMs, rootDelayMs, rootDispMs, lastOk, lastError}, quality: good|degraded|bad|unknown, offsetUncertaintyMs, issues[], feeds: [{feedId, samples, negative, absurd, lastMs, floorMs, jitterMs, driftUsPerMin, intervals, status: ok|warn|bad|unknown, issues[], minima: [{t, ms}]}], explanation}`. The NTP reference is exactly `config.ntp.server` (the same sampler corrects the host clock for every latency statistic).

Response: [latency.NTPCheck](#t-latency-NTPCheck)

Example: `GET /api/timing/ntpcheck`

```jsonc
{
  "ntp": {
    "enabled": false,
    "synced": false,
    "server": "",
    "stratum": 0,
    "refId": "",
    "leap": 0,
    "offsetMs": 0,
    "delayMs": 0,
    "jitterMs": 0,
    "rootDelayMs": null,
    "rootDispMs": null,
    "lastOk": 0
  },
  "quality": "unknown",
  "offsetUncertaintyMs": null,
  "issues": ["no NTP server configured (Settings → NTP): the receiver→host delay includes the host clock error"],
  "feeds": [
    {
      "feedId": "demo",
      "samples": 2283,
      "negative": 0,
      "absurd": 0,
      "lastMs": 18.0216,
      "floorMs": 1.6644,
      "jitterMs": 0.0016,
      "driftUsPerMin": null,
      "intervals": 5,
      "status": "ok",
      "issues": [],
      "minima": [
        {
          "t": 1795821860000,
          "ms": 1.6665
        },
        /* … 4 more */
      ],
      "ref": "startOfFrame"
    }
  ],
  "explanation": "The Radarcape time-stamps every frame from its GPS receiver and the NTP server is GPS-locked too, so (host receive time + NTP offset) - GPS …"
}
```

### `GET /api/timing/receiver`

Receiver timing state: GPS-timestamp mode and status text per feed, receiver GPS position, stored status frames.

| Parameter | Meaning |
|---|---|
| `limit` | stored status rows, 1..500 (default 20) |

Response: [server.receiverReply](#t-server-receiverReply)

Example: `GET /api/timing/receiver`

```jsonc
{
  "now": 1795821907365,
  "model": "",
  "feeds": [
    {
      "id": "beast",
      "name": "Local Beast (readsb/dump1090 :30005)",
      "type": "beast",
      "connected": false,
      "gpsTime": false,
      "info": "",
      "lastFrame": 0
    },
    /* … 1 more */
  ],
  "receiverGps": null,
  "statusFrames": 44,
  "history": [
    {
      "t": 1795821864000,
      "feed": "demo",
      "status": [17, 1, 159, 0, 0, 0, 0, 0, /* … 6 more */]
    },
    /* … 19 more */
  ]
}
```

### `POST /api/timing/ntp/sample`

Take an NTP measurement now with the configured server, or test another server once without saving it.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

| Parameter | Meaning |
|---|---|
| `server` | optional `host`, `host:port`, IPv4 or IPv6 literal (`[v6]:port`): query this server once instead of `config.ntp.server` |

Request body: Optional `{"server":"host[:port]"}` (same as `?server=`). Without a server the configured one is sampled.

To use the server, save it as `config.ntp.server` (with `ntp.enabled`) through `PUT /api/config`: the sampler and the latency clock switch to it at once.

Response: Without a server: `ntp.Sample` of the configured server; `502` when it did not answer. With a server: one query (timeout 3 s), nothing is stored or applied: `{ok, server (normalised host:port), addr?, t, stratum, refId, leap, offsetMs, delayMs, rootDelayMs, rootDispMs, error?, saved:false}` (always `200`; `ok:false` with `error` and null numbers when the server did not answer); `400` for an invalid host.

Example: `POST /api/timing/ntp/sample` -> `502`

```jsonc
{
  "error": "NTP query failed: ntp: no server configured"
}
```

(The demo instance used for these examples runs offline.)

## Location guesser

### `GET /api/guess`

Receiver location estimated from the received positions alone (four methods and their fusion). Cached result.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

The configured position is never an input; `errorKm` compares against it when it is set. `surface` is a 33 x 33 cost grid (row-major from the south-west corner) for a heat-map display.

Server-side cache: 1m0s (identical requests share one computation).

Response: [analytics.Guess](#t-analytics-Guess)

Example: `GET /api/guess`

```jsonc
{
  "ant": "",
  "cells": 33009,
  "positions": 6339300,
  "ok": true,
  "estimates": [
    {
      "method": "centroid",
      "label": "Centroid of all positions",
      "ok": true,
      "lat": 50.1973,
      "lon": 8.62173,
      "uncertaintyKm": 94.6001,
      "errorKm": 16.4521,
      "weight": 0.0005,
      "detail": "2260 cells, r.m.s. radius 315 km; biased towards busy airways and away from blocked sectors"
    },
    /* … 4 more */
  ],
  "fused": {
    "method": "fused",
    "label": "Best estimate (Locate Me v2)",
    "ok": true,
    "lat": 50.03521,
    "lon": 8.59351,
    "uncertaintyKm": 2.087,
    "errorKm": 1.709,
    "weight": 1,
    "r95Km": 5.24,
    "detail": "Locate Me v2: 399730 stored trail positions of 1782 aircraft (1826 passes); terms: horizon, endpoints, signal; smooth earth; 95 % radius 5.1…"
  },
  "located": true,
  "configured": {
    "lat": 50.05,
    "lon": 8.6
  },
  "formatted": "50.0352, 8.5935",
  "maidenhead": "JO40ha",
  "surface": {
    "method": "locate2",
    "south": 49.85064,
    "west": 8.30787,
    "dLat": 0.011455,
    "dLon": 0.017834,
    "rows": 33,
    "cols": 33,
    "cost": [1, 1, 1, 1, 1, 1, 1, 1, /* … 1081 more */],
    "minLat": 50.03392,
    "minLon": 8.59321
  },
  "notes": ["Uncertainty radii are heuristic: they describe how sharply each method pins the position down, not a guaranteed error bound.", "Weights: Locate Me v2 97.7 %, classic methods 2.3 %. The weights follow the uncertainties (1/σ²): v2's σ of 2.1 km is much smaller than t…", "The accuracy circle (radius 5.2 km) holds the station with about 95 % probability; it includes an allowance for model errors on a real site …"],
  "locate2": {
    "ok": true,
    "lat": 50.03392,
    "lon": 8.59321,
    "mapLat": 50.03527,
    "mapLon": 8.59321,
    "sigmaKm": 2.082,
    "r95Km": 5.095,
    "majorKm": 2.082,
    "minorKm": 2.082,
    "axisDeg": 135,
    "antennaM": 200,
    "samples": 399730,
    "sessions": 1826,
    "aircraft": 1782,
    "terrain": false,
    "source": "positions",
    "terms": [
      {
        "name": "horizon",
        "used": true,
        "n": 30031,
        "detail": "aircraft must be above the radio horizon / terrain panorama; surface positions draw airport line-of-sight discs",
        "deltaLL": 0
      },
      /* … 3 more */
    ],
    "levels": [
      {
        "stepKm": 8,
        "nodes": 1961,
        "halfKm": 200,
        "horizon": 60000,
        "signal": 9557,
        "endpoints": 1070
      },
      /* … 3 more */
    ],
    "surface": {
      "method": "locate2",
      "south": 49.85064,
      "west": 8.30787,
      "dLat": 0.011455,
      "dLon": 0.017834,
      "rows": 33,
      "cols": 33,
      "cost": [1, 1, 1, 1, 1, 1, 1, 1, /* … 1081 more */],
      "minLat": 50.03392,
      "minLon": 8.59321
    },
    "elapsedMs": 7699,
    "notes": [],
    "errorKm": 1.853,
    "skylineRaised": 0.139,
    "refSource": "configured"
  }
}
```

### `POST /api/guess`

Run the location guesser now on the newest data (flushes the statistics first) and return the result.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode |

Response: Same document as `GET /api/guess`.

Example: `POST /api/guess` -> `200`

```jsonc
{
  "ant": "",
  "cells": 33009,
  "positions": 6339390,
  "ok": true,
  "estimates": [
    {
      "method": "centroid",
      "label": "Centroid of all positions",
      "ok": true,
      "lat": 50.19729,
      "lon": 8.62174,
      "uncertaintyKm": 94.6001,
      "errorKm": 16.4511,
      "weight": 0.0005,
      "detail": "2260 cells, r.m.s. radius 315 km; biased towards busy airways and away from blocked sectors"
    },
    /* … 4 more */
  ],
  "fused": {
    "method": "fused",
    "label": "Best estimate (Locate Me v2)",
    "ok": true,
    "lat": 50.03521,
    "lon": 8.59351,
    "uncertaintyKm": 2.087,
    "errorKm": 1.709,
    "weight": 1,
    "r95Km": 5.24,
    "detail": "Locate Me v2: 399730 stored trail positions of 1782 aircraft (1826 passes); terms: horizon, endpoints, signal; smooth earth; 95 % radius 5.1…"
  },
  "located": true,
  "configured": {
    "lat": 50.05,
    "lon": 8.6
  },
  "formatted": "50.0352, 8.5935",
  "maidenhead": "JO40ha",
  "surface": {
    "method": "locate2",
    "south": 49.85064,
    "west": 8.30787,
    "dLat": 0.011455,
    "dLon": 0.017834,
    "rows": 33,
    "cols": 33,
    "cost": [1, 1, 1, 1, 1, 1, 1, 1, /* … 1081 more */],
    "minLat": 50.04538,
    "minLon": 8.57538
  },
  "notes": ["Uncertainty radii are heuristic: they describe how sharply each method pins the position down, not a guaranteed error bound.", "Weights: Locate Me v2 97.7 %, classic methods 2.3 %. The weights follow the uncertainties (1/σ²): v2's σ of 2.1 km is much smaller than t…", "The accuracy circle (radius 5.2 km) holds the station with about 95 % probability; it includes an allowance for model errors on a real site …"],
  "locate2": {
    "ok": true,
    "lat": 50.03392,
    "lon": 8.59321,
    "mapLat": 50.04381,
    "mapLon": 8.57779,
    "sigmaKm": 2.082,
    "r95Km": 5.095,
    "majorKm": 2.082,
    "minorKm": 2.082,
    "axisDeg": 26.6,
    "antennaM": 200,
    "samples": 399730,
    "sessions": 1826,
    "aircraft": 1782,
    "terrain": false,
    "source": "positions",
    "terms": [
      {
        "name": "horizon",
        "used": true,
        "n": 30032,
        "detail": "aircraft must be above the radio horizon / terrain panorama; surface positions draw airport line-of-sight discs",
        "deltaLL": 0
      },
      /* … 3 more */
    ],
    "levels": [
      {
        "stepKm": 8,
        "nodes": 1961,
        "halfKm": 200,
        "horizon": 60000,
        "signal": 9537,
        "endpoints": 1070
      },
      /* … 3 more */
    ],
    "surface": {
      "method": "locate2",
      "south": 49.85064,
      "west": 8.30787,
      "dLat": 0.011455,
      "dLon": 0.017834,
      "rows": 33,
      "cols": 33,
      "cost": [1, 1, 1, 1, 1, 1, 1, 1, /* … 1081 more */],
      "minLat": 50.04538,
      "minLon": 8.57538
    },
    "elapsedMs": 6686,
    "notes": [],
    "errorKm": 1.853,
    "skylineRaised": 0.167,
    "refSource": "configured"
  }
}
```

## History

### `GET /api/history/aircraft`

Every airframe ever seen: search, sort, paging.

| Parameter | Meaning |
|---|---|
| `q` | case-insensitive substring of ICAO address, registration, callsign, type, type description or operator |
| `sort` | `last_seen` (default) \| `first_seen` \| `icao` \| `callsign` \| `reg` \| `type` \| `operator` \| `country` \| `msgs` \| `flights` \| `positions` \| `max_dist_km` \| `max_alt` |
| `desc` | `1` / `0` (default: descending for times and numbers, ascending for text) |
| `limit` | 1..1000 (default 100) |
| `offset` | rows to skip |
| `since`, `until` | restrict `lastSeen` (Unix ms) |

Response: [server.aircraftListReply](#t-server-aircraftListReply)

Example: `GET /api/history/aircraft?limit=3&sort=msgs`

```jsonc
{
  "total": 2047,
  "limit": 3,
  "offset": 0,
  "sort": "msgs",
  "desc": true,
  "rows": [
    {
      "icao": "4051D4",
      "firstSeen": 1795739011000,
      "lastSeen": 1795810292000,
      "callsign": "TOM7354",
      "reg": "G-JZUT",
      "operator": "TUI Airways",
      "country": "United Kingdom",
      "category": "A3",
      "msgs": 53771,
      "flights": 2,
      "positions": 14320,
      "maxDistKm": 390.5080721229996,
      "maxAlt": 31000,
      "srcMask": 17,
      "flags": 0
    },
    /* … 2 more */
  ]
}
```

### `GET /api/history/aircraft/{icao}`

One airframe: totals, reference data, its flights (newest first) and its events.

| Parameter | Meaning |
|---|---|
| `limit` | flights returned, 1..1000 (default 100) |

Same document as `/api/aircraft/{icao}` plus `events`. `404` when the address was never seen.

Response: [server.historyAircraftReply](#t-server-historyAircraftReply)

Example: `GET /api/history/aircraft/4436CA`

```jsonc
{
  "aircraft": {
    "icao": "4436CA",
    "addrType": "icao",
    "squawk": "4477",
    "country": "Austria",
    "countryCode": "AT",
    "lat": 47.76380635924258,
    "lon": 5.71148212139423,
    "source": "mlat",
    "posAge": 2.8,
    "onGround": false,
    "altBaro": 41000,
    "vertRate": 0,
    "gs": 447.0447404902556,
    "track": 97.71321539639175,
    "ias": 205,
    "tas": 406,
    "mach": 0.704,
    "magHeading": 95.80078125,
    "roll": 0,
    "trackRate": 0,
    "windDir": 271.1526535071407,
    "windSpd": 41.51347892501752,
    "nic": 0,
    "nacv": 0,
    "distKm": 330.8218437651352,
    "bearing": 220.88648675716732,
    "elevation": 1.0253730271842325,
    "slantKm": 331.3433176335282,
    "rssi": -32.5,
    "rssiAvg": -33.5,
    "msgs": 111,
    "msgRate": 1.9,
    "antennas": ["vertical"],
    "feeds": ["demo"],
    "firstSeen": 1795821865867,
    "lastSeen": 1795821924794,
    "flightId": 2057
  },
  "trail": [
    [1795821868499, 47.77881, 5.55105, 41000, 447.0, null, 1],
    [1795821873035, 47.77826, 5.56430, 41000, 446.3, null, 1],
    /* … 10 more */
  ],
  "info": {
    "icao": "4436CA",
    "country": "Austria",
    "countryCode": "AT",
    "state": "Austria",
    "stateCode": "AT",
    "alloc": "state"
  },
  "acdb": null,
  "seen": {
    "icao": "4436CA",
    "firstSeen": 1795821865000,
    "lastSeen": 1795821915000,
    "country": "Austria",
    "msgs": 94,
    "flights": 1,
    "positions": 28,
    "maxDistKm": 337.1916806412576,
    "maxAlt": 41000,
    "srcMask": 2,
    "flags": 0
  },
  "flights": [
    {
      "id": 2057,
      "icao": "4436CA",
      "squawk": "4477",
      "start": 1795821865000,
      "end": 1795821915000,
      "msgs": 94,
      "positions": 28,
      "srcMask": 2,
      "maxDistKm": 337.1916806412576,
      "maxDistBrg": 222.6602187321935,
      "minDistKm": 331.4189745331907,
      "minAlt": 41000,
      "maxAlt": 41000,
      "maxGs": 454.2510319195764,
      "rssiMin": -35.1,
      "rssiMax": -30.4,
      "rssiAvg": -32.679591836734694,
      "firstLat": 47.77880859375,
      "firstLon": 5.551048278808594,
      "lastLat": 47.767949508408364,
      "lastLon": 5.691129244290865,
      "country": "Austria"
    }
  ],
  "watch": null,
  "events": []
}
```

### `GET /api/history/flights`

Flights (contact sessions), newest first.

| Parameter | Meaning |
|---|---|
| `limit` | 1..1000 (default 100) |
| `offset` | rows to skip |
| `icao` | only this aircraft |
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |

With a time range: flights that overlap it. A flight is one contact session; a gap of more than 30 min starts a new one.

Response: [server.flightsReply](#t-server-flightsReply)

Example: `GET /api/history/flights?limit=3`

```jsonc
{
  "limit": 3,
  "offset": 0,
  "flights": [
    {
      "id": 2058,
      "icao": "44A0DF",
      "callsign": "BEL902",
      "squawk": "3706",
      "start": 1795821867000,
      "end": 1795821915000,
      "msgs": 27,
      "positions": 2,
      "srcMask": 1,
      "maxDistKm": 403.65868697239443,
      "maxDistBrg": 137.44296539874924,
      "minDistKm": 403.3822351308781,
      "minAlt": 33000,
      "maxAlt": 33000,
      "maxGs": 438.21455931997514,
      "rssiMin": -36.1,
      "rssiMax": -32.2,
      "rssiAvg": -34.16538461538462,
      "firstLat": 47.317657470703125,
      "firstLon": 12.210960388183594,
      "lastLat": 47.31909283136917,
      "lastLon": 12.207782451923077,
      "operator": "Brussels Airlines",
      "country": "Belgium"
    },
    /* … 2 more */
  ]
}
```

### `GET /api/history/flights/{id}`

One flight with its stored trail.

`trail` points are `[tMs, lat, lon, altFt|null, gsKt|null, rssi|null, src]`, one every `config.storage.trailIntervalSec` seconds. `airports` holds the guessed departure / arrival airports (see `/api/history/flights/{id}/trace`). `404` for an unknown id.

Response: [server.flightReply](#t-server-flightReply)

Example: `GET /api/history/flights/2057`

```jsonc
{
  "flight": {
    "id": 2057,
    "icao": "4436CA",
    "squawk": "4477",
    "start": 1795821865000,
    "end": 1795821915000,
    "msgs": 94,
    "positions": 28,
    "srcMask": 2,
    "maxDistKm": 337.1916806412576,
    "maxDistBrg": 222.6602187321935,
    "minDistKm": 331.4189745331907,
    "minAlt": 41000,
    "maxAlt": 41000,
    "maxGs": 454.2510319195764,
    "rssiMin": -35.1,
    "rssiMax": -30.4,
    "rssiAvg": -32.679591836734694,
    "firstLat": 47.77880859375,
    "firstLon": 5.551048278808594,
    "lastLat": 47.767949508408364,
    "lastLon": 5.691129244290865,
    "country": "Austria"
  },
  "trail": [
    [1795821868000, 47.77881, 5.55105, 41000, 447.0, null, 1],
    [1795821873000, 47.77826, 5.56430, 41000, 446.0, null, 1],
    /* … 7 more */
  ],
  "info": {
    "icao": "4436CA",
    "country": "Austria",
    "countryCode": "AT",
    "state": "Austria",
    "stateCode": "AT",
    "alloc": "state"
  },
  "live": true,
  "airports": {
    "departure": null,
    "arrival": null
  }
}
```

## Database

### `GET /api/db/stats`

Database size, writer counters, row counts per table, retention settings, aircraft database and tile cache state.

Response: [server.dbStatsReply](#t-server-dbStatsReply)

Example: `GET /api/db/stats`

```jsonc
{
  "stats": {
    "path": "/var/lib/airview/airview.db",
    "dbBytes": 28024832,
    "walBytes": 556232,
    "pageSize": 4096,
    "pageCount": 6842,
    "freelistPages": 3082,
    "queuedRows": 1,
    "queuedOps": 1,
    "flushes": 19,
    "flushedRows": 659,
    "flushErrors": 0,
    "failedOps": 0,
    "combinedCommits": 11,
    "droppedLow": 0,
    "droppedMid": 0,
    "droppedHigh": 0,
    "lastFlush": 1795821924187,
    "lastFlushMs": 1.162399,
    "checkpoints": 0,
    "checkpointRestarts": 0,
    "checkpointTruncates": 2,
    "checkpointsIncomplete": 0,
    "lastCheckpoint": 1795821895034,
    "lastCheckpointMs": 0,
    "walPendingBytes": 552960,
    "vacuumedPages": 0
  },
  "tables": {
    "acas_event": 0,
    "acdb": 0,
    "aircraft": 2047,
    "alerts_rule": 11,
    "alerts_watch": 0,
    "coverage": 11079,
    "elev_hist": 1554,
    "events": 227,
    "flight_station": 0,
    "flights": 2058,
    "flights_hour": 58,
    "guess_samples": 44,
    "heat": 82084,
    "hist_day": 0,
    "kv": 12,
    "latency_minute": 2,
    "met_bias": 1673,
    "met_obs": 63494,
    "met_profile": 2555,
    "met_sample": 63494,
    "mlat_minute": 0,
    "mlat_quality_hour": 0,
    "navdata_airport": 6,
    "navdata_airport_stage": 0,
    "navdata_country": 0,
    "navdata_country_stage": 0,
    "navdata_meta": 4,
    "navdata_navaid": 1,
    "navdata_navaid_stage": 0,
    "navdata_photo": 0,
    "navdata_route": 3,
    "navdata_route_cache": 7,
    "navdata_route_stage": 0,
    "navdata_runway": 2,
    "navdata_runway_stage": 0,
    "ntp_samples": 3375,
    "positions": 940300,
    "predict_forecast_log": 28,
    "range_hour": 183,
    "receiver_status": 3578,
    "records": 7,
    "rfstat_hour": 0,
    "rfstat_iid_hour": 0,
    "routes_learned": 0,
    "routes_leg": 24,
    "routes_meta": 0,
    "routes_obs": 0,
    "routes_override": 0,
    "routes_override_log": 0,
    "schema_data_migrations": 6,
    "schema_migrations": 20,
    "signal_hist": 9088,
    "stats_day": 0,
    "stats_hour": 183,
    "stats_minute": 10804,
    "trail_chunk": 4651,
    "trail_tail": 12,
    "weather_fc": 73,
    "weather_fc_log": 0,
    "weather_obs": 62
  },
  "storage": {
    "trailIntervalSec": 5,
    "trailRetentionDays": 30,
    "flightRetentionDays": 730,
    "minuteRetentionDays": 90
  },
  "acdb": {
    "installed": false,
    "rows": 0,
    "info": null
  },
  "tileCache": {
    "enabled": true,
    "hosts": ["tiles.openfreemap.org", "tile.openstreetmap.org", "tilecache.rainviewer.com"],
    "dir": "/var/lib/airview/tilecache",
    "bytes": 0,
    "files": 0,
    "maxBytes": 536870912,
    "scanned": true,
    "hits": 0,
    "misses": 0,
    "revalidated": 0,
    "stale": 0,
    "errors": 0,
    "evictions": 0
  },
  "profile": {
    "name": "standard",
    "requested": "auto",
    "reason": "default",
    "flushIntervalMs": 10000,
    "trailFlushMs": 60000,
    "incFlushMs": 60000,
    "heatFlushMs": 300000,
    "rangePartialMs": 900000,
    "aircraftRefreshMs": 300000,
    "sealIntervalMs": 300000,
    "checkpointSoftBytes": 41943040,
    "checkpointIntervalMs": 300000,
    "walHardCapPages": 16384,
    "journalSizeLimit": 67108864,
    "writerCacheKiB": 16384,
    "readerCacheKiB": 8192,
    "readConns": 4,
    "readerIdleCloseMs": 600000,
    "mmapBytes": 268435456,
    "migrationStepMs": 200,
    "backupBeforeMigrate": "auto",
    "tileCacheMB": 512
  },
  "migration": {
    "state": "done",
    "migrations": [
      {
        "name": "flights_hour",
        "version": 1,
        "state": "done",
        "rowsLeft": 0,
        "rowsDone": 10,
        "steps": 0,
        "started": 1795821820000,
        "updated": 1795821820000,
        "eta": 0
      },
      /* … 5 more */
    ],
    "backupMode": "auto",
    "vacuumTail": false
  }
}
```

### `POST /api/db/vacuum`

Compact the database file (blocks writes while it runs; can take minutes on a Raspberry Pi).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{ok, elapsedMs, beforeBytes, afterBytes}`

Example: `POST /api/db/vacuum` -> `200`

```jsonc
{
  "ok": true,
  "elapsedMs": 172,
  "beforeBytes": 28581064,
  "afterBytes": 14581760
}
```

### `POST /api/db/prune`

Apply the retention settings now.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{ok, elapsedMs, beforeBytes, afterBytes, total, pruned:{flights, positions, minuteStats, metObs, ntpSamples, receiverStatus, freedPages}}`

Example: `POST /api/db/prune` -> `200`

```jsonc
{
  "ok": true,
  "elapsedMs": 3,
  "beforeBytes": 14581760,
  "afterBytes": 14614752,
  "pruned": {
    "flights": 0,
    "positions": 0,
    "minuteStats": 0,
    "metObs": 0,
    "ntpSamples": 0,
    "receiverStatus": 0,
    "freedPages": 0,
    "extra": {
      "flights_hour": 1
    }
  },
  "total": 1
}
```

### `POST /api/db/checkpoint`

Flush the in-memory statistics and checkpoint the write-ahead log.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{ok, elapsedMs, beforeBytes, afterBytes}`

Example: `POST /api/db/checkpoint` -> `200`

```jsonc
{
  "ok": true,
  "elapsedMs": 5,
  "beforeBytes": 14614752,
  "afterBytes": 14585856
}
```

### `POST /api/db/rollup`

Roll minute statistics up into hours now (normally 3 min after each hour).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{ok, elapsedMs, beforeBytes, afterBytes, hours}`

Example: `POST /api/db/rollup` -> `200`

```jsonc
{
  "ok": true,
  "elapsedMs": 0,
  "beforeBytes": 14585856,
  "afterBytes": 14590008,
  "hours": 3
}
```

### `GET /api/db/acdb/sources`

Downloadable aircraft databases (with licence and attribution), what is installed, and the import state.

Response: [server.acdbSourcesReply](#t-server-acdbSourcesReply)

Example: `GET /api/db/acdb/sources`

```jsonc
{
  "sources": [
    {
      "id": "tar1090-db",
      "name": "tar1090-db (Mictronics aircraft database)",
      "url": "https://github.com/wiedehopf/tar1090-db/raw/refs/heads/csv/aircraft.csv.gz",
      "homepage": "https://github.com/wiedehopf/tar1090-db",
      "format": "tar1090",
      "licence": "Open Data Commons Attribution License (ODC-By) v1.0",
      "licenceUrl": "https://opendatacommons.org/licenses/by/1-0/",
      "attribution": "Aircraft data: tar1090-db by wiedehopf, built from the Mictronics aircraft database (www.mictronics.de/aircraft-database, ODC-By 1.0) with a…",
      "notes": "Recommended. Updated about weekly; includes military / interesting / PIA / LADD flags. The tar1090-db repository itself carries no licence f…",
      "approxBytes": 9437184,
      "approxRecords": 650000
    },
    /* … 1 more */
  ],
  "installed": {
    "installed": false,
    "rows": 0,
    "info": null
  },
  "status": {
    "running": false,
    "stage": "",
    "stored": 0
  }
}
```

### `POST /api/db/acdb/import`

Start downloading and importing an aircraft database in the background.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"source":"tar1090-db"|"opensky", "url":"optional mirror"}` (empty body = tar1090-db)

Poll `/api/db/acdb/progress`; the outcome also arrives as a `notice` event. A failed download leaves the installed database untouched.

Response: `engine.ImportStatus` (see `/api/db/acdb/progress`); `409` when an import is already running.


### `POST /api/db/acdb/upload`

Import an aircraft database file sent as the raw request body (tar1090-db or OpenSky CSV, optionally gzip; at most 600 MB).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

| Parameter | Meaning |
|---|---|
| `name` | original file name (informational) |

Response: `engine.ImportStatus` when the import finished (the request blocks until then).

Example: `POST /api/db/acdb/upload?name=sample.csv` with a two-line tar1090-db file as body -> `200`

```jsonc
{
  "running": false,
  "stage": "done",
  "source": "sample.csv",
  "started": 1795821925267,
  "finished": 1795821925267,
  "progress": {
    "source": "",
    "url": "",
    "stage": "done",
    "bytesRead": 100,
    "bytesTotal": 100,
    "records": 2,
    "skipped": 0,
    "empty": 0
  },
  "stored": 2
}
```

### `POST /api/db/acdb/cancel`

Cancel a running import.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `engine.ImportStatus`

Example: `POST /api/db/acdb/cancel` -> `200`

```jsonc
{
  "running": false,
  "stage": "done",
  "source": "sample.csv",
  "started": 1795821925267,
  "finished": 1795821925267,
  "progress": {
    "source": "",
    "url": "",
    "stage": "done",
    "bytesRead": 100,
    "bytesTotal": 100,
    "records": 2,
    "skipped": 0,
    "empty": 0
  },
  "stored": 2
}
```

### `GET /api/db/acdb/progress`

State of the current or last aircraft database import.

`stage`: `""` (never ran) | `download` | `store` | `done` | `error` | `cancelled`. `progress.bytesTotal` is -1 when unknown.

Response: [engine.ImportStatus](#t-engine-ImportStatus)

Example: `GET /api/db/acdb/progress`

```jsonc
{
  "running": false,
  "stage": "done",
  "source": "sample.csv",
  "started": 1795821925267,
  "finished": 1795821925267,
  "progress": {
    "source": "",
    "url": "",
    "stage": "done",
    "bytesRead": 100,
    "bytesTotal": 100,
    "records": 2,
    "skipped": 0,
    "empty": 0
  },
  "stored": 2
}
```

### `GET /api/db/acdb/lookup`

Look one address up in the aircraft database and the built-in tables.

| Parameter | Meaning |
|---|---|
| `icao` | 6 hex digits (required) |

Response: [server.acdbLookupReply](#t-server-acdbLookupReply)

Example: `GET /api/db/acdb/lookup?icao=3C6444`

```jsonc
{
  "icao": "3C6444",
  "found": true,
  "record": {
    "icao": "3C6444",
    "reg": "D-AIBD",
    "type": "A319",
    "typeDesc": "AIRBUS A-319",
    "operator": "Lufthansa",
    "flags": 0,
    "year": 2010
  },
  "info": {
    "icao": "3C6444",
    "country": "Germany",
    "countryCode": "DE",
    "state": "Germany",
    "stateCode": "DE",
    "alloc": "state",
    "reg": "D-AIBD",
    "type": "A319",
    "typeDesc": "AIRBUS A-319",
    "typeClass": "Landplane, 2 jet engines",
    "typeInfo": {
      "designator": "A319",
      "manufacturer": "Airbus",
      "model": "A319",
      "name": "Airbus A319",
      "description": "L2J",
      "wtc": "M",
      "role": "airliner"
    },
    "wtc": "Medium (7,000 to 136,000 kg)",
    "operator": "Lufthansa",
    "year": 2010
  }
}
```

### `GET /api/db/export`

Tables available for CSV export with their row counts.

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/db/export`

```jsonc
{
  "tables": [
    {
      "table": "acdb",
      "url": "/api/db/export/acdb.csv",
      "rows": 2
    },
    /* … 19 more */
  ]
}
```

### `GET /api/db/export/{file}`

Download one table as CSV (`{file}` = `<table>.csv`), streamed.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |
| `limit` | maximum number of rows |

The time range filters on the table's time column (see `/api/db/export`); tables without one ignore it. At most two exports run at a time (`429` beyond).

Response: `text/csv` with a header row; database columns as stored (times in Unix seconds, `day` columns in days since 1970, BLOBs as hex, NULL as empty field).

Example: `GET /api/db/export/records.csv`

```
key,value,icao,t,detail_json
max_aircraft,46,,1795765228,
max_alt_ft,41000,10986873,1795605807,"{""callsign"":""UAL819"",""reg"":""N592GP"",""lat"":53.60448,""lon"":7.29218,""altFt"":41000,""distKm"":405.61,""bearing"":347.7,""gsKt"":463,""rssi"":-27.7}"
...
```

## Geo, astronomy & reference data

### `GET /api/geo/parse`

Parse pasted coordinates in almost any notation into latitude / longitude.

| Parameter | Meaning |
|---|---|
| `q` | the text: decimal degrees, DMS, degrees + decimal minutes, Maidenhead locator, Plus Code, `geo:` URI, Google / Bing / OSM map URL ... |

Always `200`: `ok:false` with a user-readable `error` when the text is not understood (the box is still being typed into). `lat` / `lon` are null then.

Response: [server.geoParseReply](#t-server-geoParseReply)

Example: `GET /api/geo/parse?q=50%C2%B002%2700.0%22N+8%C2%B034%2713.4%22E`

```jsonc
{
  "ok": true,
  "input": "50°02'00.0\"N 8°34'13.4\"E",
  "lat": 50.0333333,
  "lon": 8.5703889,
  "format": "degrees, minutes, seconds",
  "formatted": {
    "decimal": "50.03333, 8.57039",
    "dms": "50°02'00.0\"N 8°34'13.4\"E",
    "ddm": "N50 02.000 E008 34.223",
    "maidenhead": "JO40ga",
    "maidenhead8": "JO40ga88"
  },
  "distKm": 2.82,
  "bearing": 228.9
}
```

### `GET /api/geo/format`

Format a position in the common notations.

| Parameter | Meaning |
|---|---|
| `lat`, `lon` | degrees (required) |

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/geo/format?lat=50.05&lon=8.6`

```jsonc
{
  "formatted": {
    "decimal": "50.05000, 8.60000",
    "dms": "50°03'00.0\"N 8°36'00.0\"E",
    "ddm": "N50 03.000 E008 36.000",
    "maidenhead": "JO40hb",
    "maidenhead8": "JO40hb12"
  },
  "lat": 50.05,
  "lon": 8.6
}
```

### `GET /api/geo/elevation`

Terrain elevation at a position (Open-Meteo elevation service; needs the internet).

| Parameter | Meaning |
|---|---|
| `lat`, `lon` | degrees (required) |

`502` when the look-up fails. Results are cached for a day.

Server-side cache: 24h0m0s (identical requests share one computation).

Response: [server.elevationReply2](#t-server-elevationReply2)

Example: `GET /api/geo/elevation?lat=50.05&lon=8.6`

```jsonc
{
  "lat": 50.05,
  "lon": 8.6,
  "elevationM": 123.4,
  "source": "open-meteo",
  "attribution": "Elevation data by Open-Meteo.com (Copernicus DEM GLO-90)"
}
```

### `GET /api/geo/magvar`

Magnetic declination and field (World Magnetic Model 2025) at a position, now.

| Parameter | Meaning |
|---|---|
| `lat`, `lon` | degrees (required) |
| `altKm` | height above the ellipsoid (default 0) |

Response: [server.magvarReply](#t-server-magvarReply)

Example: `GET /api/geo/magvar?lat=50.05&lon=8.6`

```jsonc
{
  "lat": 50.05,
  "lon": 8.6,
  "altKm": 0,
  "t": 1795821925278,
  "magVarDeg": 3.778,
  "inclDeg": 65.859,
  "totalNt": 49171.5,
  "horizNt": 20110.1,
  "model": "WMM2025",
  "decimalYear": 2026.907
}
```

### `GET /api/astro/now`

Sun position, day phase and today's sun events at the receiver, the day/night terminator rings and the moon phase.

| Parameter | Meaning |
|---|---|
| `t` | Unix ms (default now) |

`nightSide.sunset|civil|nautical|astronomical` are closed rings of `[lon, lat]` enclosing the area where the sun is below -0.833 / -6 / -12 / -18 degrees. `position`, `phase` and `events` are absent until the receiver is located. Event times are Unix ms, 0 = does not occur.

Server-side cache: 10s (identical requests share one computation).

Response: [analytics.SunInfo](#t-analytics-SunInfo)

Example: `GET /api/astro/now`

```jsonc
{
  "t": 1795821900000,
  "located": true,
  "receiver": {
    "lat": 50.05,
    "lon": 8.6
  },
  "position": {
    "azimuth": 5.617788923299656,
    "elevation": -61.11549137659126,
    "trueElevation": -61.11867444089247,
    "declination": -21.260226033692927,
    "hourAngle": -177.0918898929424,
    "eqOfTimeMin": 12.232440428230374,
    "distanceAu": 0.9866808525893745
  },
  "phase": "night",
  "isDay": false,
  "events": {
    "solarNoon": 1795864412097,
    "noonElevation": 18.603209262009674,
    "midnightElevation": -61.38306815051731,
    "sunrise": 1795849097065,
    "sunset": 1795879704565,
    "civilDawn": 1795846868281,
    "civilDusk": 1795881932724,
    "nauticalDawn": 1795844434827,
    "nauticalDusk": 1795884365249,
    "astroDawn": 1795842104823,
    "astroDusk": 1795886694031,
    "dayLengthSec": 30607.499607591,
    "polarDay": false,
    "polarNight": false
  },
  "nightSide": {
    "t": 1795821900000,
    "subsolarLat": -21.260226033692927,
    "subsolarLon": -174.3081101070576,
    "sunset": [
      [-180, 69.48048403340155],
      [-178, 69.53402477117406],
      /* … 182 more */
    ],
    "civil": [
      [-180, 74.66979273125948],
      [-178, 74.7103930762615],
      /* … 182 more */
    ],
    "nautical": [
      [-180, 80.69626511141237],
      [-178, 80.72150896250518],
      /* … 182 more */
    ],
    "astronomical": [
      [-180, 86.72395575958957],
      [-178, 86.73313435261967],
      /* … 182 more */
    ]
  },
  "moon": {
    "illumination": 0.8444518290970646,
    "phase": 0.6290461487441159,
    "ageDays": 18.57610318812557,
    "waxing": false,
    "name": "Waning Gibbous"
  }
}
```

### `GET /api/astro/terminator.geojson`

The night side of the earth as a GeoJSON FeatureCollection of four polygons (properties `kind`, `sunAlt`).

| Parameter | Meaning |
|---|---|
| `t` | Unix ms (default now) |
| `step` | ring resolution, 0.5..10 degrees (default 2) |

Server-side cache: 1m0s (identical requests share one computation).

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/astro/terminator.geojson?step=10`

```jsonc
{
  "features": [
    {
      "geometry": {
        "coordinates": [
          [
            [-180, 69.48382665628336],
            [-170, 69.5173118768357],
            /* … 38 more */
          ]
        ],
        "type": "Polygon"
      },
      "properties": {
        "kind": "sunset",
        "sunAlt": -0.833
      },
      "type": "Feature"
    },
    /* … 3 more */
  ],
  "type": "FeatureCollection"
}
```

### `GET /api/astro/days`

Sunrise, sunset and twilight times for the coming days.

| Parameter | Meaning |
|---|---|
| `days` | 1..366 (default 7) |
| `lat`, `lon` | another position instead of the receiver |

Response: [server.astroDayReply](#t-server-astroDayReply)

Example: `GET /api/astro/days?days=2`

```jsonc
{
  "located": true,
  "receiver": {
    "lat": 50.05,
    "lon": 8.6
  },
  "days": [
    {
      "solarNoon": 1795864412097,
      "noonElevation": 18.603209262009674,
      "midnightElevation": -61.38306815051731,
      "sunrise": 1795849097065,
      "sunset": 1795879704565,
      "civilDawn": 1795846868281,
      "civilDusk": 1795881932724,
      "nauticalDawn": 1795844434827,
      "nauticalDusk": 1795884365249,
      "astroDawn": 1795842104823,
      "astroDusk": 1795886694031,
      "dayLengthSec": 30607.499607591,
      "polarDay": false,
      "polarNight": false
    },
    /* … 1 more */
  ]
}
```

### `GET /api/refdata/icao/{icao}`

What the built-in tables know about an ICAO address: state of registry, allocation block, derived registration, military heuristic.

Response: [server.aircraftInfo](#t-server-aircraftInfo)

Example: `GET /api/refdata/icao/3C6444`

```jsonc
{
  "icao": "3C6444",
  "country": "Germany",
  "countryCode": "DE",
  "state": "Germany",
  "stateCode": "DE",
  "alloc": "state",
  "reg": "D-AIBD"
}
```

### `GET /api/refdata/callsign/{callsign}`

Interpret a callsign: operator, telephony name, flight number or registration.

Response: [server.callsignReply](#t-server-callsignReply)

Example: `GET /api/refdata/callsign/DLH4AB`

```jsonc
{
  "callsign": "DLH4AB",
  "kind": "flight",
  "designator": "DLH",
  "flightNumber": "4AB",
  "operator": "Lufthansa",
  "operatorType": "airline",
  "telephony": "LUFTHANSA",
  "country": "Germany",
  "countryCode": "DE",
  "spoken": "LUFTHANSA 4AB"
}
```

### `GET /api/refdata/type/{code}`

ICAO aircraft type designator: manufacturer, model, class, wake category.

Response: [server.typeReply](#t-server-typeReply)

Example: `GET /api/refdata/type/A20N`

```jsonc
{
  "found": true,
  "designator": "A20N",
  "manufacturer": "Airbus",
  "model": "A320neo",
  "name": "Airbus A320neo",
  "description": "L2J",
  "wtc": "M",
  "role": "airliner",
  "classText": "Landplane, 2 jet engines",
  "wtcText": "Medium (7,000 to 136,000 kg)",
  "engines": 2
}
```

### `GET /api/refdata/squawk/{code}`

Conventional meaning of a Mode A code.

Response: [server.squawkReply](#t-server-squawkReply)

Example: `GET /api/refdata/squawk/7700`

```jsonc
{
  "found": true,
  "valid": true,
  "code": "7700",
  "meaning": "General emergency",
  "region": "Worldwide",
  "emergency": "general",
  "kind": "emergency"
}
```

## System

### `GET /api/system`

Process and host information: version, platform, uptime, goroutines, memory, data directory, restart-required flag.

Response: [server.systemReply](#t-server-systemReply)

Example: `GET /api/system`

```jsonc
{
  "app": "AirView",
  "version": "test",
  "platform": "linux/amd64",
  "goVersion": "go1.27.3",
  "hostname": "airview",
  "pid": 1389,
  "cpus": 4,
  "goroutines": 38,
  "memory": {
    "allocBytes": 20952624,
    "heapInuseBytes": 24084480,
    "sysBytes": 407189864,
    "stackBytes": 1572864,
    "numGc": 543,
    "gcPauseMs": 0.038011
  },
  "now": 1795821925280,
  "started": 1795821863046,
  "uptimeS": 62,
  "timeZone": "UTC",
  "utcOffsetSec": 0,
  "dataDir": "/var/lib/airview",
  "dbBytes": 14627088,
  "gui": false,
  "mode": "standalone",
  "demo": true,
  "located": true,
  "httpAddr": "0.0.0.0:42025",
  "httpPort": 42025,
  "restartRequired": false,
  "liveClients": 0,
  "sessions": 0,
  "cacheEntries": 3,
  "forecaster": {
    "fitted": 1795821907000,
    "built": 1795821920014,
    "dataHours": 60
  },
  "tracker": {
    "frames": 3390,
    "msgRate": 58.6,
    "aircraft": 12,
    "tracked": 12
  }
}
```

### `GET /api/ssr`

EXPERIMENTAL: state of the SSR self-location (enabled, feed, counters, estimator statistics incl. `pairMatches`, the last solve with the interrogators it used). Advisory only: the result is never applied to a position.

Response: `SSRStatus` (`experimental`, `enabled`, ...); `enabled:false` when `ssr.enabled` is off


### `POST /api/ssr/solve`

EXPERIMENTAL: run an SSR self-location solve now (about 0.3 s; poll `GET /api/ssr`).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `202` `{"queued":true}`; `409` when `ssr.enabled` is off or a solve is already queued


### `GET /api/update`

Self-update state: running version, channel, automatic updates on/off, last check and its error, the newer version available, the last update transaction, and whether this installation can install updates itself.

Response: `UpdateState` (`supported`, `configured`, `canInstall`, `installNote`, `busy`, `product`, `version`, `channel`, `enabled`, `lastCheck`, `lastError`, `available`, `last`, `result`)


### `POST /api/update/check`

Check the release manifest now (signature verified; nothing is installed).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `UpdateState`; `409` when update.manifestUrl / update.keys are not set


### `POST /api/update/install`

Download, verify and install the newer version, then restart into it. The new version confirms itself after its health window (update.healthWindowS) or the previous one is restored.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Poll `GET /api/update` and `/healthz` (its `version`) to follow it. Audited in the log.

Response: `202` with `UpdateState` (`busy`: the install runs; the program restarts when it is done); `409` when this installation cannot install itself (`installNote` says why) or nothing newer exists


## Public network (aggregator mode)

### `GET /feeder/install.sh`

The Raspberry Pi feeder installer with this network's presets (feeder host and port, AVF certificate pin, web address, release key).

Presets come from `aggregator.publicHost`, `aggregator.avfPort`, the AVF certificate and `aggregator.feederManifestKey`; a preset that is not configured stays empty and the installer asks for it. Never cached.


### `GET /feeder/manifest.json`

The signed airview-feeder release manifest from `aggregator.feederReleaseDir` (404 when none is configured).


### `GET /feeder/manifest.json.sig`

The Ed25519 signature of manifest.json (base64).


### `GET /feeder/dl/{target}`

One airview-feeder release file (`dl/<target>` in the release directory): `linux-arm64`, `linux-armv7`, `linux-amd64`, `windows-amd64`, `windows-arm64`, ...

Range requests are supported (resumed downloads).


### `GET /feeder/beta/manifest.json`

The beta channel's signed manifest (`beta/` in the release directory).


### `GET /feeder/beta/manifest.json.sig`

The beta manifest's signature.


### `GET /feeder/beta/dl/{target}`

One beta release file.


### `GET /api/share/{token}`

What a share link shows: label, scope, number of stations, expiry and the network name (no station data).

| Parameter | Meaning |
|---|---|
| `token` | the secret of a share link (path; 32 characters) |

Response: `{"label","scope","stationCount":n,"expires":ms,"coarseLow":bool,"network":"..."}`; `404` for an unknown, revoked or expired link


### `GET /api/share/{token}/aircraft`

The aircraft the link's stations heard in the last 60 s (`publicdto.Snapshot`: no receiver data; low aircraft on the island-guard grid when the link says so).

| Parameter | Meaning |
|---|---|
| `token` | the secret of a share link (path; 32 characters) |

Response: `publicdto.Snapshot`; `404` for an unknown, revoked or expired link


### `GET /api/share/{token}/stats`

Non-locating statistics of the link's stations (scope `map+stats` only).

| Parameter | Meaning |
|---|---|
| `token` | the secret of a share link (path; 32 characters) |

Response: `{"entries":[{"name","online","stats":StationStats}]}` (named `entries`: a public payload never has a `stations` member); `404` when the link has another scope


### `GET /healthz`

Health check for load balancers, service managers and monitors.

Never rate-limited or cached; no database query; no station data.

Response: `{"ok":bool,"status":"ok|degraded|unavailable","mode","version","uptimeS","checks":{"engine":"ok","registry":"ok","admin"?:"setup pending"},"adminReady"?:bool}`; `200` while the engine runs (`degraded` when an aggregator service is missing), `503` otherwise.

Example: `GET /healthz`

```jsonc
{
  "ok": true,
  "status": "ok",
  "mode": "standalone",
  "version": "test",
  "uptimeS": 62,
  "checks": {
    "engine": "ok"
  }
}
```

### `GET /api/public/status`

Public application status: version, mode, network name, live counters and which public surfaces are enabled.

No feeds, receiver position, data port, NTP or paths. `GET /api/status` answers this shape to public viewers in aggregator mode.

Response: [publicdto.Status](#t-publicdto-Status)

Example: `GET /api/public/status`

```jsonc
{
  "app": "AirView",
  "version": "test",
  "mode": "standalone",
  "now": 1795821925281,
  "started": 1795821863046,
  "tileCache": true,
  "counts": {
    "aircraft": 12,
    "withPos": 10,
    "adsb": 9,
    "mlat": 3,
    "tisb": 0,
    "modes": 0,
    "msgRate": 58.6,
    "posRate": 10.9,
    "predicted": 2
  },
  "adminLocked": true,
  "located": false,
  "publicMap": false,
  "publicStats": false,
  "tarJson": false,
  "tileCacheHosts": ["tiles.openfreemap.org", "tile.openstreetmap.org", "tilecache.rainviewer.com"]
}
```

### `GET /api/public/aircraft`

The merged live picture without receiver data (`publicdto.Snapshot`).

No distance, bearing, elevation, signal level, receiver list or station flag; MLAT positions carry `mlatGrade` / `mlatSigmaM`. With `aggregator.islandGuard` on, low aircraft heard only by hidden-pin stations are rounded to 0.05° (`coarse:true`). `403` when `aggregator.publicMap` is off (public viewers).

Server-side cache: 2s (identical requests share one computation).

Response: [publicdto.Snapshot](#t-publicdto-Snapshot)

Example: `GET /api/public/aircraft`

```jsonc
{
  "now": 1795821925277,
  "aircraft": [
    {
      "icao": "3C473C",
      "addrType": "icao",
      "callsign": "DLH81AS",
      "squawk": "2017",
      "category": "A5",
      "operator": "Lufthansa",
      "country": "Germany",
      "countryCode": "DE",
      "lat": 51.68476169392214,
      "lon": 4.8177337646484375,
      "source": "adsb",
      "posAge": 1,
      "onGround": false,
      "altBaro": 32000,
      "altGeom": 32350,
      "vertRate": 0,
      "selAlt": 32000,
      "qnh": 1013.6,
      "gs": 413.2807762284619,
      "track": 284.0026191880347,
      "ias": 273,
      "tas": 444,
      "mach": 0.756,
      "magHeading": 280.01953125,
      "roll": 0,
      "trackRate": 0,
      "selHeading": 279.84375,
      "windDir": 267.20233714741534,
      "windSpd": 31.988267981898133,
      "oat": -46.00175623862748,
      "adsbVersion": 2,
      "nic": 10,
      "nacp": 10,
      "nacv": 2,
      "sil": 3,
      "rcM": 25,
      "navModes": ["autopilot", "althold", "lnav", "tcas"],
      "firstSeen": 1795821863140,
      "lastSeen": 1795821925277,
      "route": {
        "from": "EDDF",
        "to": "EGLL",
        "fromIata": "FRA",
        "toIata": "LHR",
        "fromName": "Frankfurt am Main",
        "toName": "London",
        "plausible": true,
        "source": "standing-data",
        "checked": true,
        "state": "fits",
        "conf": 0.9,
        "reason": "standing data · fits the track",
        "along": 46.4,
        "distRem": 365.5,
        "eta": 1795824330,
        "etaSigma": 150,
        "lic": "open"
      }
    },
    /* … 11 more */
  ],
  "counts": {
    "aircraft": 12,
    "withPos": 10,
    "adsb": 9,
    "mlat": 3,
    "tisb": 0,
    "modes": 0,
    "msgRate": 58.6,
    "posRate": 10.9,
    "predicted": 2
  }
}
```

### `GET /api/public/aircraft/{icao}`

One aircraft of the public picture: live state, live trail without signal levels, reference data.

| Parameter | Meaning |
|---|---|
| `smooth` | `1` returns the trail smoothed by the tracking filter |

Trail points are `[tMs, lat, lon, altFt|null, gsKt|null, null, src]` (the signal column is always null).

Response: [server.publicAircraftDetail](#t-server-publicAircraftDetail)

Example: `GET /api/public/aircraft/3C473C`

```jsonc
{
  "aircraft": {
    "icao": "3C473C",
    "addrType": "icao",
    "callsign": "DLH81AS",
    "squawk": "2017",
    "category": "A5",
    "operator": "Lufthansa",
    "country": "Germany",
    "countryCode": "DE",
    "lat": 51.68476169392214,
    "lon": 4.8177337646484375,
    "source": "adsb",
    "posAge": 1,
    "onGround": false,
    "altBaro": 32000,
    "altGeom": 32350,
    "vertRate": 0,
    "selAlt": 32000,
    "qnh": 1013.6,
    "gs": 413.2807762284619,
    "track": 284.0026191880347,
    "ias": 273,
    "tas": 444,
    "mach": 0.756,
    "magHeading": 280.01953125,
    "roll": 0,
    "trackRate": 0,
    "selHeading": 279.84375,
    "windDir": 267.20233714741534,
    "windSpd": 31.988267981898133,
    "oat": -46.00175623862748,
    "adsbVersion": 2,
    "nic": 10,
    "nacp": 10,
    "nacv": 2,
    "sil": 3,
    "rcM": 25,
    "navModes": ["autopilot", "althold", "lnav", "tcas"],
    "firstSeen": 1795821863140,
    "lastSeen": 1795821925277
  },
  "trail": [
    [1795821865687, 51.65762, 4.99306, 32000, 413, null, 0],
    [1795821869019, 51.65920, 4.98283, 32000, 413, null, 0],
    /* … 13 more */
  ],
  "info": {
    "icao": "3C473C",
    "country": "Germany",
    "countryCode": "DE",
    "state": "Germany",
    "stateCode": "DE",
    "alloc": "state",
    "operator": "Lufthansa",
    "categoryDesc": "Heavy (above 300,000 lb / 136 t)",
    "callsign": {
      "callsign": "DLH81AS",
      "kind": "flight",
      "designator": "DLH",
      "flightNumber": "81AS",
      "operator": "Lufthansa",
      "operatorType": "airline",
      "telephony": "LUFTHANSA",
      "country": "Germany",
      "countryCode": "DE"
    },
    "spoken": "LUFTHANSA 81AS"
  },
  "acdb": null
}
```

### `GET /api/public/trails`

Live trails of the public picture (signal column null).

| Parameter | Meaning |
|---|---|
| `minutes` | trail length, 1..240 (default `config.ui.trailMinutes`); public answers cover at most the last 10 minutes |
| `bbox` | `south,west,north,east`: only aircraft whose newest point is inside (west > east crosses the antimeridian) |

At most 120 points per aircraft (evenly thinned, first and last kept). Full trails of one aircraft: `GET /api/public/aircraft/{icao}`.

Server-side cache: 2s (identical requests share one computation).

Response: [publicdto.Trails](#t-publicdto-Trails)

Example: `GET /api/public/trails?minutes=5`

```jsonc
{
  "now": 1795821925282,
  "minutes": 5,
  "trails": {
    "3C473C": [
      [1795821865687, 51.65762, 4.99306, 32000, 413, null, 0],
      [1795821869019, 51.65920, 4.98283, 32000, 413, null, 0],
      /* … 13 more */
    ],
    "3DD35E": [
      [1795821864244, 49.69913, 7.30880, 3050, 108, null, 0],
      [1795821867354, 49.69812, 7.31061, 3050, 108, null, 0],
      /* … 17 more */
    ],
    "4074E1": [
      [1795821864467, 51.08121, 9.92362, 32000, 429, null, 0],
      [1795821867468, 51.07622, 9.91843, 32000, 429, null, 0],
      /* … 17 more */
    ],
    "4436CA": [
      [1795821868499, 47.77881, 5.55105, 41000, 447, null, 1],
      [1795821873035, 47.77826, 5.56430, 41000, 446, null, 1],
      /* … 10 more */
    ],
    "44A0DF": [
      [1795821908716, 47.31766, 12.21096, 33000, 438, null, 0],
      [1795821918972, 47.32919, 12.18553, 33000, 438, null, 0],
      /* … 1 more */
    ],
    "4852DE": [
      [1795821869138, 50.50589, 8.27776, 23400, 432, null, 1],
      [1795821873603, 50.51184, 8.28319, 23200, 437, null, 1],
      /* … 10 more */
    ],
    "485FD9": [
      [1795821864289, 49.46928, 9.00778, 39000, 463, null, 0],
      [1795821867630, 49.46897, 9.01874, 39000, 463, null, 0],
      /* … 17 more */
    ],
    "4AAAF7": [
      [1795821866505, 50.35776, 7.06023, 11700, 343, null, 1],
      [1795821871003, 50.35204, 7.06912, 11550, 350, null, 1],
      /* … 10 more */
    ],
    "4CA19A": [
      [1795821864442, 49.27560, 7.79494, 30000, 459, null, 0],
      [1795821867553, 49.27776, 7.78543, 30000, 459, null, 0],
      /* … 17 more */
    ],
    "4CA2AB": [
      [1795821874102, 49.15139, 13.16791, 32000, 466, null, 0],
      [1795821879440, 49.15590, 13.15176, 32000, 466, null, 0],
      /* … 8 more */
    ],
    "4CACEA": [
      [1795821869752, 49.61545, 5.02357, 35000, 486, null, 0],
      [1795821873285, 49.61663, 5.03567, 35000, 486, null, 0],
      /* … 5 more */
    ],
    "AC9FDA": [
      [1795821874853, 52.24199, 10.38420, 33000, 438, null, 0],
      [1795821879223, 52.23354, 10.38002, 33000, 438, null, 0],
      /* … 10 more */
    ]
  }
}
```

### `GET /api/public/events`

The public live stream (Server-Sent Events): `aircraft` (`publicdto.Snapshot`), `status` (`publicdto.Status`) and public `notice` events.

| Parameter | Meaning |
|---|---|
| `delta` | `1`: send one keyframe (`aircraft` with `seq`), then `delta` events `{seq, base, now, counts, upd, del}`: `upd` holds new aircraft in full and only the changed members of the others (a removed member is `null`), `del` the aircraft that left. Values are quantised (positions 1e-5°, altitudes 25 ft, `lastSeen` 5 s). After a lost frame the server sends a new keyframe; a client that sees `base` differ from its last `seq` reconnects. |
| `bbox` | `south,west,north,east` (west > east crosses the antimeridian): a viewport stream. The first event is `stream` `{id, bbox}`; then only aircraft inside the box are sent. Delta streams send aircraft entering the view in full in `upd` and aircraft leaving it (outside the box grown by 10 %) in `del`; `counts` stay global. Aircraft without a position are never sent. |
| `view` | `1`: a viewport stream of the whole world, narrowed later with `POST /api/events/view` |
| `replace` | stream id of a previous stream of this client: it is closed first, so a reconnect with a new viewport is not refused by the one-public-stream-per-address limit |

Same framing as `GET /api/events`. In aggregator mode one public stream per client address (IPv4 address or IPv6 /64) and at most `aggregator.limits.maxPublicSse` in total. Every picture is encoded once and shared by all streams, and the same picture is never sent twice. Public viewers of `GET /api/events` may use `delta=1` too (the web UI does).


### `POST /api/events/view`

Change the viewport of a live stream opened with `bbox` or `view=1` (no new keyframe).

| Parameter | Meaning |
|---|---|
| `id` | stream token from the stream's `stream` event |
| `bbox` | `south,west,north,east`; empty = the whole world |

The token is the only credential (only the stream's reader knows it). The next delta brings the aircraft that entered the new view in full and removes those that left it. Clients debounce map moves (about 500 ms).

Response: `204`; `404` unknown token; `400` bad box.


### `GET /api/public/stations`

Stations that opted in to a public pin or public statistics (`publicdto.StationList`).

A pin is the stored keyed point inside a 0.5° (`region`) or 0.1° (`cell01`) cell, never the exact position. Stations with a hidden pin appear only with opted-in statistics and then without SID. `approved` / `online` count every approved station. Empty in standalone mode; `403` when `aggregator.publicStats` is off (public viewers).

Server-side cache: 10s (identical requests share one computation).

Response: [publicdto.StationList](#t-publicdto-StationList)

Example: `GET /api/public/stations`

```jsonc
{
  "stations": [],
  "approved": 0,
  "online": 0
}
```

### `GET /api/public/summary`

The network landing page in numbers (`publicdto.Summary`): station counts, aircraft, MLAT aircraft, message rate.

`stationCounts` counts approved stations: `total`, `online` (data in the last 2 minutes) and `public` (with a public pin). `mlat` (network P50/P95/P99 error) is null until the MLAT engine publishes it. `403` when `aggregator.publicStats` is off (public viewers).

Server-side cache: 2s (identical requests share one computation).

Response: [publicdto.Summary](#t-publicdto-Summary)

Example: `GET /api/public/summary`

```jsonc
{
  "stationCounts": {
    "total": 0,
    "online": 0,
    "public": 0
  },
  "aircraft": 12,
  "mlatAircraft": 3,
  "messagesPerSec": 58.6,
  "mlat": null,
  "updated": 1795821925277
}
```

### `GET /api/public/stats`

The network in numbers: approved and online stations and the live counters.

`403` when `aggregator.publicStats` is off (public viewers).

Server-side cache: 2s (identical requests share one computation).

Response: [publicdto.NetworkStats](#t-publicdto-NetworkStats)

Example: `GET /api/public/stats`

```jsonc
{
  "now": 1795821925282,
  "approved": 0,
  "online": 0,
  "counts": {
    "aircraft": 12,
    "withPos": 10,
    "adsb": 9,
    "mlat": 3,
    "tisb": 0,
    "modes": 0,
    "msgRate": 58.6,
    "posRate": 10.9,
    "predicted": 2
  }
}
```

### `GET /api/public/mlat`

The network's empirical MLAT accuracy over the last hour (hold-out aircraft against their ADS-B reference) and its 1° cells (`publicdto.MLATCoverage`).

`quality` is null below 20 samples. A cell is listed only with at least 20 samples from at least 5 contributing stations; no station identity. Empty when the MLAT engine does not run. `403` when `aggregator.publicStats` is off (public viewers).

Server-side cache: 10s (identical requests share one computation).

Response: [publicdto.MLATCoverage](#t-publicdto-MLATCoverage)

Example: `GET /api/public/mlat`

```jsonc
{
  "quality": null,
  "samples": 0,
  "cells": [],
  "updated": 1795821925282
}
```

### `GET /api/public/join`

What a new feeder needs to join this network: host name, open listener ports (AVF/1, Beast, mlat-client), admission and registration policy, the backup retention named in the deletion notice and the privacy-notice link.

Operator facts only, no station data. `host` is `aggregator.publicHost` (`""` = use the host name the page was loaded from; the server never echoes a request header). In standalone mode `{"network":false}` with every other field empty.

Server-side cache: 10s (identical requests share one computation).

Response: `{network, name?, host, avfPort, beastPort, mlatPort, mlat, admission, registration, backupDays, privacyNotice}`; a closed listener is `0` (`mlatPort` is also `0` while the MLAT engine is off).

Example: `GET /api/public/join`

```jsonc
{
  "network": false,
  "host": "",
  "avfPort": 0,
  "beastPort": 0,
  "mlatPort": 0,
  "mlat": false,
  "admission": "",
  "registration": "",
  "backupDays": 0,
  "privacyNotice": ""
}
```

### `GET /data/aircraft.json`

The public picture in the tar1090 / readsb `aircraft.json` shape.

No `rssi`, no per-aircraft message counts, no receiver fields. Predicted (coasting) aircraft are exported at their last real position. `404` when `aggregator.tarJson` (or `publicMap`) is off (public viewers in aggregator mode).

Server-side cache: 2s (identical requests share one computation).

Response: [publicdto.TarAircraftFile](#t-publicdto-TarAircraftFile)

Example: `GET /data/aircraft.json`

```jsonc
{
  "now": 1795821925.3,
  "messages": 3390,
  "aircraft": [
    {
      "hex": "3c473c",
      "type": "adsb_icao",
      "flight": "DLH81AS ",
      "alt_baro": 32000,
      "alt_geom": 32350,
      "gs": 413.3,
      "ias": 273,
      "tas": 444,
      "mach": 0.756,
      "track": 284,
      "track_rate": 0,
      "roll": 0,
      "mag_heading": 280.02,
      "baro_rate": 0,
      "squawk": "2017",
      "emergency": "none",
      "category": "A5",
      "nav_qnh": 1013.6,
      "nav_altitude_mcp": 32000,
      "nav_heading": 279.84,
      "nav_modes": ["autopilot", "althold", "lnav", "tcas"],
      "lat": 51.684762,
      "lon": 4.817734,
      "nic": 10,
      "rc": 25,
      "seen_pos": 1,
      "version": 2,
      "nac_p": 10,
      "nac_v": 2,
      "sil": 3,
      "alert": 0,
      "spi": 0,
      "mlat": [],
      "tisb": [],
      "seen": 0
    },
    /* … 11 more */
  ]
}
```

### `GET /data/receiver.json`

tar1090 `receiver.json`.

Aggregator mode: no position, or the centroid of the public pins (1 decimal) when at least 3 stations have one. Standalone mode: the configured receiver position.

Response: [publicdto.TarReceiver](#t-publicdto-TarReceiver)

Example: `GET /data/receiver.json`

```jsonc
{
  "version": "AirView test",
  "refresh": 1000,
  "history": 0,
  "lat": 50.05,
  "lon": 8.6
}
```

## My stations (aggregator mode)

### `POST /api/me/stations/{sid}/location/pending/confirm`

Apply the position the station's feeder announced and the aggregator holds (a large move waits for the survey or this confirmation; finding PRIV-1).

Response: the station (OwnerStation); `404` when no position is pending


### `PUT /api/me/stations/{sid}/mlat/survey`

Allow or reject the automatic use of the station's survey position for MLAT (DESIGN §19.3).

Request body: `{"reject":bool}`

The choice is stored with the station and stands until it is changed.

Response: `{"positionSource":"...","surveyRejected":bool}`


### `GET /api/me/stations/{sid}/notices`

Location notices of the station (last 30 days, newest first): finished surveys, declared-position checks, the automatic use of the survey position for MLAT and its owner override, confirmations.

Built from the audit log, which never holds coordinates.

Response: `{"notices":[{"t":ms,"kind":"survey|declared|mlat|located|owner","level":"info|ok|warn","text":"..."}]}`


### `GET /api/me/notices`

Location notices of all the viewer's stations (last 14 days, at most 50, newest first).

Response: `{"notices":[{"sid","name","t","kind","level","text"}]}`


### `GET /api/me/stations/{sid}/minutes`

The station's last 16 minutes of feeder statistics (AVF/1 MINUTE_STATS merged with what the server received): exact, received, merged and shed counts, selective mode.

Response: `{"minutes":[{minute, exact, received, receivedKnown, merged, shed, selective, records, late}]}`


### `GET /api/me/shares`

The account's share links (no secrets): label, stations, scope, state, expiry, last use and use count, plus the location warning.

Response: `{"links":[ShareLink],"max":n,"warning":"..."}`


### `POST /api/me/shares`

Create a share link for own stations (read-only live view). The secret is returned once.

Request body: `{"stations":[sid],"label":"...","scope":"map|map+stats","days":1..365,"coarseLow":true,"acknowledge":true}` (`acknowledge` confirms the location warning; required)

Audited (`share.create`).

Response: `{"link":ShareLink,"token":"...","path":"/#/share/<token>"}`; `400` with the warning without `acknowledge`, `429` over the limit of 20 active links


### `DELETE /api/me/shares/{id}`

Revoke an own share link (it stops working at once).

Audited (`share.revoke`).


### `GET /api/me/stations/{sid}/survey`

The station's reverse-MLAT survey: the running job, the last results (position, σ, 95 % ellipse, stable/suspect, declared-position check, MSL/HAE verdict) and the next scheduled check.

Response: `{"enabled":bool,"running":bool,...,"last":{...},"history":[...],"nextCheck":ms}`


### `POST /api/me/stations/{sid}/survey`

Ask for a survey of the station (at most one per hour and station).

Audited (`station.survey.start`).

Response: `{"job":n,"queued":bool}`; `202` when queued behind other surveys, `409` with the consent text when the station sends no timestamps, `429` when a survey ran recently


### `GET /api/me/stations/{sid}/locate`

Locate Me v2 for a station without a location: the approximate position estimated from what the station receives (radio horizon, skyline, signal level), with its 95 % circle and the error against the last survey.

Owner/admin only (DESIGN §8.2). Uses the station's guess samples of the last 7 days, recorded only while the station has no location (`locate.*`).

Server-side cache: 1m0s (identical requests share one computation).

Response: Locate2Result (`lat`, `lon`, `r95Km`, `sigmaKm`, `terms[]`, `errorKm`, `refSource`, `notes[]`); `404` without stations.db


### `GET /api/me`

The logged-in account and its stations (exact data).


### `GET /api/me/stations`

The own stations with exact location, privacy settings, admission and live state.


### `GET /api/me/stations/{sid}`

One own station: exact data, location estimates, the public pin preview (exactly as others see it), live links and statistics.

`404` for unknown stations and for stations of other accounts.


### `POST /api/me/stations/{sid}`

Change an own station's name, privacy (pin precision, show name, public statistics) or declared position.

Request body: `{"name"?:"...","pin"?:"hidden|region|cell01","showName"?:bool,"publicStats"?:bool,"declared"?:{"lat","lon","alt","altRef":"msl|hae"} | null}` (members that are missing keep their value; `declared:null` clears it)


### `PUT /api/me/stations/{sid}/privacy`

Set an own station's public pin precision, name display and public statistics.

Request body: `{"pin"?:"hidden|region|cell01","showName"?:bool,"publicStats"?:bool}`

Response: the station (OwnerStation)


### `PUT /api/me/stations/{sid}/name`

Rename an own station.

Request body: `{"name":"..."}` (at most 64 characters)

Response: the station (OwnerStation)


### `PUT /api/me/stations/{sid}/location`

Declare an own station's antenna position (cross-checked by the survey); `{"clear":true}` removes it.

Request body: `{"lat":..,"lon":..,"alt":metres,"altRef":"msl|hae"}`

Response: the station (OwnerStation)


### `POST /api/me/stations/{sid}/location/confirm`

Use a location estimate (survey, GNSS) as the station's position for MLAT (owner confirmation, DESIGN §10.1).

Request body: `{"estimate":"<id from location.estimates>"}`

Response: the station (OwnerStation)


### `POST /api/me/stations/{sid}/unclaim`

Release an own station (it keeps feeding; it can be claimed again).

Response: `{"ok":true}`


### `GET /api/me/stations/{sid}/stats`

Live statistics of an own station: frames, positions, aircraft this minute, late frames, live links, last seen.


### `GET /api/me/stations/{sid}/mlat`

Timing and MLAT of an own station: position source used for MLAT, clock state (class, anchor state, σθ, drift, short-frame offset, jitter, steps, events), sync peers, MLAT contributions and residuals, timed frames per minute and result eligibility.

Peers are weekly tags unique to the viewing station (no SIDs) with a distance band (`near` < 50 km, `mid` 50-150 km, `far` > 150 km); never a name, bearing, position or clock figure. The band is computed from the viewing station's position frozen at its first request of the ISO week (the tag period), so moving the station does not change it. `driftPpm` is rounded to whole ppm in owner views. Owners are limited to 30 requests in a burst and one every 2 s on average (429 with Retry-After). `summary`: peers, synced peers, contributions per hour, timed frames per minute, result eligibility and results received (null when the listener does not report it). `history`: the station's last 2 h at one sample per minute (σθ, drift, clock outliers, residual bias, contributions). Event times are Unix ms. `running:false` when the MLAT engine does not run.


### `GET /api/me/stations/{sid}/mlat/longterm`

History of an own station's MLAT participation over days, one point per hour: timed frames, groups, fixes and outliers per minute, synced peers, synced share, σθ, residual RMS and drift.

| Parameter | Meaning |
|---|---|
| `days` | 1..90 (default 7) |

Only the station's own figures; no peer, no network or hold-out data.


### `GET /api/me/stations/{sid}/connections`

Connection log of an own station (newest first; addresses only as daily-salted hashes).

| Parameter | Meaning |
|---|---|
| `days` | 1..30 (default 7) |
| `limit` | 1..1000 (default 200) |


### `GET /api/me/aircraft`

The live picture for an owner: `mine` and `stations` (own SIDs) per aircraft, geometry for the selected own station.

| Parameter | Meaning |
|---|---|
| `station` | SID of one of the viewer's own stations (admins: any station); the station's geometry is added |
| `only` | `mine` = only aircraft heard by the own stations |


### `GET /data/me/aircraft.json`

tar1090 `aircraft.json` of the aircraft the own stations heard in the last 60 s (session-scoped).


### `GET /api/me/check`

Feed Check: the live feeder links coming from the visitor's own address (exact IPv4, or the same IPv6 /64).

No login needed; only non-locating link data: protocol, the last 6 characters of the UUID, warnings (no UUID, two UUIDs on one address), connected since, msg/s, pos/s, kB/s, RTT, clock class, MLAT sync, and whether the station can be claimed. Never cached.


### `POST /api/me/claim`

Claim a station: a native feeder with its claim code, a compat feeder with its full UUID from the feeder's own address.

Request body: `{"code":"ABCD-EFGH"}` or `{"uuid":"..."}`

`429` with `Retry-After` after 5 attempts per 10 minutes per address or 20 per day per account. Error messages are safe to show.


### `GET /api/me/export`

Export the account's data (JSON files in a zip).

Response: `application/zip`


### `DELETE /api/me/account`

Delete the own account and its stations' data now; the session ends.

Request body: none, or `{"confirm":"DELETE"}`

Response: `{"ok":true}`


## Network administration (aggregator mode)

### `POST /api/admin/stations/{sid}/timing`

Override a station's timing quarantine: `trusted` (joins MLAT now), `excluded` (never), `quarantine` (judge again), `auto` (clear overrides).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"mode":"trusted|excluded|quarantine|auto"}`

Audited (`station.timing`); the MLAT gate applies it at once.

Response: `{"timing":"...","override":"..."}`


### `GET /api/admin/mlat/quarantine`

Stations in timing quarantine with the gate's progress per station, and the gate counters.

Response: `{"enabled":bool,"stats":{...},"stations":[{"sid","name","since","report"}]}`


### `GET /api/admin/mlat/crlb/improve`

Where one more station would improve MLAT most: candidate sites ranked by the CRLB gain (area-weighted log improvement of σ_h), with newly covered and newly grade-A/B cells, plus the gain of every candidate cell for the map.

| Parameter | Meaning |
|---|---|
| `alt` | target height in m (default 3048 = FL100; 300..15000) |
| `step` | grid step in degrees (0.25..2, default 0.5) |
| `sigmaNs` | timing σ of the hypothetical station in ns (5..1000, default 30 = GPS) |
| `refresh` | 1 = recompute now |

Admin only (derived from exact station positions). Cached for 10 minutes per parameter set; a computation takes up to a few seconds for 1,000 stations.

Response: `{"running":true,"altM","stepDeg","sigmaNs","stations","candidates","baseline":{cells,p50M,p90M},"sites":[{lat,lon,gain,score,newCells,newA,newB,better,medianBeforeM,medianAfterM}],"cells":[{lat,lon,score}]}`


### `GET /api/admin/mlat/surveys`

Survey console: every approved station's location status, MLAT position source, running survey and last result (no coordinates), plus the network's location quality summary.

Response: `{"enabled":bool,"autoUse":bool,"autoUseMaxSigmaM":n,"stats":{...},"quality":{...},"stations":[...]}`


### `GET /api/admin/stations/{sid}/minutes`

A station's last 16 minutes of feeder statistics (as the owner sees them).


### `GET /api/admin/shares`

Every share link of the network (no secrets) with its owner, stations and use.

| Parameter | Meaning |
|---|---|
| `q` | search in label, owner and station SIDs |
| `state` | `active`, `revoked` or `expired` |
| `offset`, `limit` | paging (limit 1..500, default 100) |

Response: `{"links":[ShareLink],"total":n,"counts":{"active":n,"revoked":n,"expired":n}}`


### `DELETE /api/admin/shares/{id}`

Revoke any share link.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Audited (`share.revoke`).


### `GET /api/admin/stations/{sid}/survey`

A station's reverse-MLAT survey status (as the owner sees it).


### `POST /api/admin/stations/{sid}/survey`

Start a survey of a station (no rate limit).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Audited (`station.survey.start`).

Response: `{"job":n,"queued":bool}`


### `DELETE /api/admin/stations/{sid}/survey`

Cancel a station's running or queued survey.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"cancelled":bool}`


### `GET /api/admin/mlat/survey`

Survey counters: stations known, running and queued jobs, finished jobs by outcome, tap counters.

Response: `{"enabled":bool,"stats":{...},"bundle":{"stats":{...},"last":{...}}}` (bundle: the network bundle adjustment, when on)


### `GET /api/admin/mlat/verification`

ADS-B position verification: counters (confirmed, contradicted, suspects, contradictions charged to a station, contradictions muted by the network-wide breaker, recovered clock-filter and engine panics), the aircraft whose broadcast positions are suspect now, and the last 200 transitions.

Admin only (an internal quality signal, never public). The list follows the planner's `suspect` class (10 minutes after the last contradiction).

Response: `{"running":bool,"counters":{...},"current":[{icao,callsign,since,lat,lon,altFt,source,nacp,mlat}],"events":[{t,kind,icao,...}],"newSinceStart":n}`


### `GET /api/admin/federation`

Federation: node ID, TLS pin, peers (state, direction, RTT, counters, last error), received aircraft and out-feeds.

Response: `{"enabled":bool,"running":bool,"error":string,"status":{nodeId,peers[],received,outFeeds[]}}`


### `GET /api/admin/mlat/modeac`

Mode A/C multilateration: counters and the shown targets (synthetic id, squawk, Mode C altitude, position, grade, σ).

Response: `{"enabled":bool,"stats":{...},"targets":[ModeACTarget]}`


### `GET /api/admin/stations`

The station registry with exact positions and owners: `{"stations":[...],"total":n,"counts":{...}}`.

| Parameter | Meaning |
|---|---|
| `q` | matches SID, name, feeder ID or UUID |
| `admission` | `approved` \| `pending` \| `denied` \| `revoked` |
| `kind` | `avf` \| `beast` \| `mlatc` \| `host` |
| `unclaimed` | `1` = without owner |
| `online` | `1` / `0`: only stations with / without data in the last 2 minutes |
| `after` | paging: station index |
| `offset` | paging: rows to skip |
| `limit` | 1..5000 (default 500) |
| `sort` | order before paging: `name` \| `sid` \| `kind` \| `admission` \| `online` \| `lastSeen` \| `trust` \| `created` \| `owner` (default: registry order) |
| `dir` | `asc` (default) \| `desc` |

Audited (`station.list`).


### `GET /api/admin/stations/{sid}`

One station with exact data, estimates, live links (with addresses) and statistics.

Every call is written to the audit log (`station.reveal`).


### `POST /api/admin/stations/{sid}`

Change a station: admission, owner, name, trust, anchor, never-anchor flag.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"admission"?:"approved|pending|denied|revoked","ownerId"?:n (0 = none),"name"?:"...","trustDelta"?:n,"anchor"?:bool,"neverAnchor"?:bool}` (`neverAnchor`: the MLAT clock filter never uses the station as a time anchor and demotes it if it is one; a negative trust has the same effect)


### `PUT /api/admin/stations/{sid}`

Edit a station (same body as the POST).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"name"?,"trust"?:n (absolute),"owner"?:id or user name}`


### `POST /api/admin/stations/{sid}/admission`

Approve, hold, deny or revoke a station.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"admission":"approved|pending|denied|revoked"}`


### `POST /api/admin/stations/{sid}/reveal`

A station's exact position and current address (audited `station.reveal`).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"lat","lon","alt","source","sigmaM","ip"?}`


### `DELETE /api/admin/stations/{sid}`

Delete a station and its data (queued; survives restarts).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"reason":"..."}` (optional)

Response: `{"ok":true}`


### `POST /api/admin/feeders/{id}/revoke`

Revoke a native feeder key (all its stations).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`


### `GET /api/admin/tokens`

Enrol tokens (without secrets).


### `POST /api/admin/tokens`

Mint an enrol token; the secret is shown once.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"label":"...","maxUses":1,"days":7}`

Response: the token fields plus `token` (the secret, shown once)


### `DELETE /api/admin/tokens/{id}`

Revoke an enrol token.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`


### `GET /api/admin/audit`

Audit log, newest first.

| Parameter | Meaning |
|---|---|
| `sid` | only this station |
| `action` | only actions starting with this |
| `before` | paging: entries with a smaller id |
| `limit` | 1..1000 (default 200) |


### `POST /api/admin/retention`

Run the retention jobs now; returns their report.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.


### `GET /api/admin/links`

Every live feeder link of every listener, with addresses.

Audited (`links.view`).


### `GET /api/admin/ingest/beast`

The Beast listener (30004): totals and every link.


### `POST /api/admin/ingest/beast/{link}/disconnect`

Close one Beast link.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`


### `GET /api/admin/network`

Operator view of the aggregator: listeners (ports, addresses, errors), ingest statistics, tracker lock statistics, lanes, flight_station rows.


### `GET /api/admin/mlat`

MLAT engine counters: every stage (groups, clusters, solves, publishes, drops), clock filter, reference store, tracker, planner, k_atm.

Every `/api/admin/mlat*` route answers `{"running":false}` when the MLAT engine does not run.


### `GET /api/admin/mlat/quality`

MLAT self-validation: hold-out error (fix and track), σ calibration, NEES, week-on-week alarm.

| Parameter | Meaning |
|---|---|
| `window` | minutes 1..10080 (default 60) |
| `cells` | `1` adds the error map |


### `GET /api/admin/mlat/history`

MLAT console history, one sample per minute for up to 24 h: hold-out error (fix and track N, P50, P95, cross-track P95, share within 1σ over the preceding 10 minutes), stage rates per minute (groups, clusters, solves, accepted, rejected, published, results, drops), clock filter (stations, synced, components, epoch cost), worker load.

| Parameter | Meaning |
|---|---|
| `minutes` | 1..1440 (default 360) |

Columns of equal length, oldest first. Rates are `null` for the first sample and after the engine restarted. Kept in memory only (lost on restart).


### `GET /api/admin/mlat/longterm`

MLAT network history over days, one point per hour from the persisted minute tables: frames, groups, fixes and outliers per minute, synced stations, and the hold-out error per hour (N, P50, P95 from the summed error histograms of every cell).

| Parameter | Meaning |
|---|---|
| `days` | 1..90 (default 7) |


### `GET /api/admin/mlat/stations`

Per-station clock quality and MLAT residuals for every station the MLAT engine knows (class, anchor state, synced, σθ, drift, β short, jitter, steps, peers, contributions, used / excluded / outlier %, residual bias, σ scale, bias flag, timed frames per minute). No positions.


### `GET /api/admin/mlat/sync`

Clock-sync matrix: per station and peer (SIDs) co-observations in the last 30 s, RMS residual, relative frequency, same clock domain.

`stations` lists every SID of the matrix with name, clock class, anchor state and sync state (the heat-map axis).


### `GET /api/admin/mlat/crlb`

Expected horizontal MLAT σ (Cramér-Rao bound) at 3000 ft, FL100 and FL300 on a 0.25° grid from the located stations and their timing.

| Parameter | Meaning |
|---|---|
| `refresh` | `1` recomputes now (otherwise cached for 10 minutes; seconds of CPU for a large network) |

`summary` per target height: cells, P50 / P90 / minimum σ_h and the area (km²) per grade (A ≤ 50 m, B ≤ 150 m, C ≤ 500 m, D ≤ 2 km).


### `GET /api/admin/stations/{sid}/mlat`

Timing and MLAT of any station (as the owner view, peers by SID).


### `GET /api/admin/aircraft/{icao}`

One aircraft with the full receiver list (every station that heard it in the last 60 s).


## Routes

### `GET /api/routes/{callsign}`

Full route view of a callsign: every hypothesis with its sources, fused probability and live score, the shown route (active leg, state, confidence, progress, ETA), the great-circle path and the learned history.

| Parameter | Meaning |
|---|---|
| `icao` | ICAO address of the aircraft: scores the hypotheses against its current position |
| `online` | `1`: look the callsign up online (adsbdb) when nothing offline is known (admin only; subject to the daily budget) |

Reads the offline data (override, learned, standing data) and the online cache. Answers from online providers (`lic: restricted`) are only shown to the station's own users; public viewers of an aggregator get `hidden` instead.

Response: [routes.Detail](#t-routes-Detail)

Example: `GET /api/routes/BAW123`

```jsonc
{
  "callsign": "BAW123",
  "lookupable": true,
  "hypotheses": []
}
```

### `GET /api/routes/status`

Resolver counters, the online budget (used / daily cap / back-off), invalid rows of the overrides file and the attribution lines.

Response: [server.routeStatusDTO](#t-server-routeStatusDTO)

Example: `GET /api/routes/status`

```jsonc
{
  "stats": {
    "entries": 10,
    "resolved": 18,
    "onlineQueries": 7,
    "onlineFound": 5,
    "onlineErrors": 0,
    "legsRecorded": 24,
    "obsAdded": 0,
    "learnedUpdates": 0
  },
  "online": true,
  "onlineUsed": 7,
  "onlineDaily": 1000,
  "attribution": ["Routes: Virtual Radar Server standing data (CC0)", "Learned routes: AirView (CC0)", "Online routes: adsbdb.com (shown to this station's users only)"],
  "config": {
    "enabled": true,
    "online": true,
    "dailyCap": 1000,
    "stableSec": 45,
    "contact": "",
    "learn": true,
    "eta": true
  }
}
```

### `GET /api/routes/overrides`

Every route override (`locked` rows come from `routes.overrides.csv` in the data directory and can only be changed there).

Response: [routes.Override](#t-routes-Override)[]

Example: `GET /api/routes/overrides`

```jsonc
[]
```

### `PUT /api/routes/overrides/{callsign}`

Create or replace the override of a callsign.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"airports":"EGLL-KJFK","mode":"fixed|verify|block","note":"...","expires":0}`

Response: the stored override; `400` when invalid, `409` for a locked (file) row


### `DELETE /api/routes/overrides/{callsign}`

Delete the override of a callsign.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`; `404` when none, `409` for a locked (file) row


### `GET /api/routes/overrides-log`

Audit log of override changes, newest first.

| Parameter | Meaning |
|---|---|
| `limit` | 1..1000 (default 200) |

Response: [routes.OverrideLog](#t-routes-OverrideLog)[]

Example: `GET /api/routes/overrides-log`

```jsonc
[]
```

### `POST /api/routes/refresh`

Check the reference data (airports, routes, ...) for updates now (conditional GET: unchanged files are not downloaded).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"started":true}`; `409` when an import is running


### `GET /api/routes/export/{file}`

Exports: `learned.csv` (learned routes with P >= 0.9 seen on >= 3 days, in the VRS standing-data format, released CC0 for submission to the VRS standing data), `overrides.csv` (round-trips with `routes.overrides.csv`), `legs.csv` (observed legs; `from`/`to` Unix s).

| Parameter | Meaning |
|---|---|
| `from`, `to` | legs.csv only: time range in Unix s |

Example: `GET /api/routes/export/learned.csv`

```
Callsign,Code,Number,AirlineCode,AirportCodes

...
```

### `GET /api/routes/board/{airport}`

Arrivals, departures and recently landed flights of an airport, built from the live picture and the routes resolved for it (not a schedule).

Arrivals: route ends here with confidence >= 50 %, sorted by ETA. Departures: route starts here, climbing, within 300 km. Landed: legs this station saw end here in the last 60 minutes (station users only on an aggregator). Public viewers of an aggregator see the public picture (no online-provider routes).

Response: `{airport, arrivals[], departures[], landed[], generated, note}`; `404` for an unknown airport


### `GET /api/routes/airports`

Airport references (ICAO / IATA code, name, position, elevation, size) for client-side route drawing and the IATA/ICAO alias map.

| Parameter | Meaning |
|---|---|
| `codes` | comma-separated ICAO, IATA or ident codes (at most 200); unknown codes are left out |

Response: [routes.AirportRef](#t-routes-AirportRef)[]

Example: `GET /api/routes/airports?codes=EGLL,JFK`

```jsonc
[
  {
    "code": "EGLL",
    "iata": "LHR",
    "name": "London",
    "lat": 51.4706,
    "lon": -0.461941,
    "elevFt": 83,
    "size": 3
  },
  /* … 1 more */
]
```

### `GET /api/routes/batch`

Route shorts of the aircraft in the caller's live picture with the given callsigns (for lists and filters). Never triggers an online look-up.

| Parameter | Meaning |
|---|---|
| `callsigns` | comma-separated callsigns (at most 200) |

Response: `{"routes":{"BAW123":{route short},...}}`; callsigns not in the live picture or without a route are left out

Example: `GET /api/routes/batch?callsigns=BAW123,EZY45`

```jsonc
{
  "routes": {}
}
```

### `GET /api/routes/stats`

Statistics of the legs this station observed: busiest `routes` (observed departure-arrival pairs), `airports` (departures + arrivals), `airlines` (callsign prefix), or `coverage` (legs by the source of the route shown, and the share with both ends observed).

| Parameter | Meaning |
|---|---|
| `kind` | routes \| airports \| airlines \| coverage |
| `from`, `to` | time range of the leg start, Unix s (default: the last 7 days) |
| `limit` | 1..1000 (default 50) |

Only ends observed with confidence >= 50 % count for routes and airports. Online-provider answers are never stored in legs. On an aggregator only owners and admins may read them.

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/routes/stats?kind=routes`

```jsonc
{
  "from": 1795217125,
  "kind": "routes",
  "rows": [],
  "to": 0
}
```

### `GET /api/routes/legs`

Search the observed legs, newest first.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range of the leg start, Unix s |
| `callsign` | exact callsign |
| `airline` | 3-letter ICAO airline prefix |
| `dep`, `arr` | observed departure / arrival airport (as stored, normally ICAO) |
| `route` | route shown when the flight ended, e.g. `EGLL-KJFK` |
| `limit` | 1..5000 (default 500) |

Response: [routes.Leg](#t-routes-Leg)[]

Example: `GET /api/routes/legs?limit=20`

```jsonc
[
  {
    "flight": 2023,
    "callsign": "LOT60EN",
    "start": 1795815520,
    "end": 1795817953,
    "arr": "EDDF",
    "arrConf": 0.17,
    "arrTier": "L5"
  },
  /* … 19 more */
]
```

### `GET /api/routes/providers`

Online route providers: enabled, licence class, budget used today, daily cap, back-off and what is sent.

Response: [server.routeProviderDTO](#t-server-routeProviderDTO)[]

Example: `GET /api/routes/providers`

```jsonc
[
  {
    "name": "adsbdb",
    "enabled": true,
    "lic": "restricted",
    "used": 7,
    "daily": 1000,
    "sends": "the callsign of an aircraft with no offline route, after it was received for 45 s with a position; never the receiver location",
    "homepage": "https://www.adsbdb.com/"
  }
]
```

## Accounts (aggregator mode)

### `GET /api/auth/setup`

Whether the first admin account still has to be set up.

Response: `{"required":bool,"tokenPending":bool,"tokenFile":"<path, loopback clients only>","os":"linux"}`


### `POST /api/auth/setup/begin`

First admin setup, step 1: console token, user name and password; returns the authenticator secret.

Request body: `{"token":"<from the setup-token file in the data directory>","username":"...","password":"<12+ characters>"}`

Response: `{"secret":"BASE32","uri":"otpauth://..."}`


### `POST /api/auth/setup/finish`

First admin setup, step 2: confirm the authenticator with a code; returns the recovery codes once.

Request body: `{"token":"...","username":"...","code":"123456"}`

Response: `{"recoveryCodes":["XXXXX-XXXXX", ...]}`


### `POST /api/auth/register`

Create an owner account (no e-mail) and log in.

Request body: `{"username":"3-32 of a-z 0-9 . _ -","password":"<8+ characters>"}`

Response: `sessionInfo` JSON and the session cookie. `409` when the name is taken, `429` after 5 sign-ups per hour from one address.


### `POST /api/auth/password`

Change the own password; other sessions of the account end.

Request body: `{"current":"...","new":"..."}`


### `POST /api/auth/totp/begin`

Start authenticator (TOTP) enrolment; returns the secret and otpauth URI.

Request body: `{"password":"..."}`


### `POST /api/auth/totp/confirm`

Confirm the authenticator with a first code; returns 10 single-use recovery codes (shown once).

Request body: `{"code":"123456"}`


### `POST /api/auth/totp/disable`

Turn the authenticator off (owners only; admins cannot).

Request body: `{"password":"...","code":"123456 or a recovery code"}`


### `POST /api/auth/recovery-codes`

Replace all recovery codes.

Request body: `{"password":"...","code":"123456"}`


### `GET /api/auth/sessions`

The own login sessions (no addresses are stored).


### `POST /api/auth/logout-others`

End every other session of the own account.

Response: `{"ended":n}`


## Prediction

### `GET /api/prediction/live`

Predicted position now of every aircraft the tracking filter follows (IMM filter, wind aware).

`coast` is true when the last real position is older than 4 s; such aircraft are also in `/api/aircraft` with `predicted:true` until `config.prediction.coastSec`. `conf` (0..1) decays with the age (about e^(-age/30)), `sigH` is the 1-sigma horizontal uncertainty in m. Only aircraft whose last position is at most `coastSec` old are listed. `409` when `config.prediction.enabled` is off.

Server-side cache: 2s (identical requests share one computation).

Response: [server.predictionLive](#t-server-predictionLive)

Example: `GET /api/prediction/live`

```jsonc
{
  "now": 1795821925285,
  "coastSec": 60,
  "aircraft": [
    {
      "icao": "3C473C",
      "lat": 51.68519499343489,
      "lon": 4.814766356602799,
      "alt": 32000,
      "track": 284.0240899995058,
      "gs": 413.9996345913002,
      "vr": 0,
      "sigH": 9.112368343852882,
      "age": 0.9927365779876709,
      "coast": false,
      "conf": 0.9631779099434096
    },
    /* … 11 more */
  ]
}
```

### `GET /api/prediction/stats`

Tracking filter counters and self-scored prediction quality per horizon and flight phase.

`horizons` has one row per horizon (5/15/30/60 s), predictor (`imm` = the filter, `cv` = constant velocity, `dr` = dead reckoning from the last report) and phase (`all`, `straight`, `turn`, `vertical`): median / p90 / p95 error in km, `calib95` (share of real positions inside the predicted 95 % ellipse, ideal 0.95). `skill[].skillPct` = how much better than dead reckoning (%). Only positions with 1-sigma <= 60 m are scored (MLAT excluded).

Server-side cache: 5s (identical requests share one computation).

Response: [trajectory.Stats](#t-trajectory-Stats)

Example: `GET /api/prediction/stats`

```jsonc
{
  "now": 1795821925286,
  "tracks": 12,
  "ready": 12,
  "coasting": 2,
  "counters": {
    "observations": 2023,
    "positions": 660,
    "rejected": 0,
    "velRejected": 0,
    "altRejected": 0,
    "outOfOrder": 0,
    "oooDropped": 0,
    "duplicates": 2,
    "resets": 0,
    "inits": 12,
    "overflow": 0
  },
  "horizons": [
    {
      "horizon": 5,
      "predictor": "imm",
      "phase": "all",
      "n": 60,
      "total": 60,
      "medianKm": 0.006796354293823242,
      "p90Km": 0.019781702995300295,
      "p95Km": 0.024906286811828608,
      "meanKm": 0.00938714080254237,
      "rmsKm": 0.012041146352872114,
      "alongBiasKm": 0.0070408581974644524,
      "alongMedAbsKm": 0.00498663330078125,
      "crossBiasKm": 0.0006849240948756535,
      "crossMedAbsKm": 0.002160812497138977,
      "vertN": 60,
      "vertMedAbsFt": 0,
      "vertP90Ft": 0,
      "vertBiasFt": 0,
      "calib95": 1
    },
    /* … 23 more */
  ],
  "skill": [
    {
      "horizon": 5,
      "phase": "all",
      "n": 60,
      "immMedianKm": 0.006796354293823242,
      "drMedianKm": 0.0036867220401763916,
      "skillPct": -84.34680509567436
    },
    /* … 7 more */
  ]
}
```

### `GET /api/prediction/path/{icao}`

Predicted path of one aircraft with growing uncertainty ellipses, and when terrain will hide it.

| Parameter | Meaning |
|---|---|
| `horizon` | seconds ahead, 10..900 (default `config.prediction.pathSec`) |
| `step` | seconds between points, 1..300 (default 10; at most 301 points) |

`learned` (when the aircraft has a current position, track and speed) is the path predicted from the recent surrounding traffic (`/api/prediction/learned`). `points[].dt` seconds after `t0`; `ell` is the 1-sigma error ellipse (`a`, `b` in m, `dir` = major axis deg true). `plan`: `decay` (turn rolls out), `capture` (turning to the selected heading), `hold` (racetrack), `ground`. `terrain` (null until the terrain analysis exists): `lostAtMs` / `acquiredAtMs` when the path crosses the terrain radio horizon (0 = not within the path). `404` when the aircraft is unknown or has had no position for longer than 120 s.

Response: [server.predictedPath](#t-server-predictedPath)

Example: `GET /api/prediction/path/3C473C?horizon=60&step=10`

```jsonc
{
  "icao": "3C473C",
  "t0": 1795821925286,
  "altOk": true,
  "plan": "decay",
  "points": [
    {
      "dt": 0,
      "lat": 51.685195403023556,
      "lon": 4.814763690817204,
      "alt": 32000,
      "sh": 9.112756112530626,
      "shf": 8.871854897357698,
      "sv": 6.043287069701994,
      "ell": {
        "a": 6.452667501320517,
        "b": 6.434703263077974,
        "dir": 12.687387540906116
      }
    },
    /* … 6 more */
  ],
  "terrain": {
    "known": true,
    "visibleNow": true,
    "marginDeg": 0.706914431435452,
    "lostAtMs": 0,
    "acquiredAtMs": 0
  },
  "learned": {
    "points": [
      {
        "dt": 0,
        "lat": 51.68476169392214,
        "lon": 4.8177337646484375,
        "track": 284.0026191880347,
        "conf": 0
      },
      /* … 6 more */
    ],
    "weight": 0.49,
    "learned": false
  }
}
```

### `GET /api/prediction/learned`

The learned traffic-flow predictor: learned cells, aircraft followed, and its self-check against dead reckoning at 30 and 60 s (mean errors, share of predictions where it was closer, the blend weight it corrected itself to).

The learner records, per 0.1-degree cell, altitude band and 30-degree heading sector, how the traffic that passed turned over the next 30 / 60 s (time-decayed, half-life 3 h), and predicts by applying those turn rates (`learned` in `/api/prediction/path/{icao}`). Every 15 s per aircraft it scores a learned and a straight prediction against the real position and moves the blend `weight` towards the closer one. Memory only: it relearns within hours after a restart.

Response: [flowlearn.Stats](#t-flowlearn-Stats)

Example: `GET /api/prediction/learned`

```jsonc
{
  "cells": 15,
  "aircraft": 12,
  "samples": 96,
  "skill": [
    {
      "horizonS": 30,
      "n": 18,
      "learnedM": 225,
      "straightM": 225,
      "weight": 0.53,
      "betterPct": 72.2,
      "improvement": 0.002
    },
    /* … 1 more */
  ]
}
```

### `GET /api/prediction/intent/{icao}`

Intent analysis of one aircraft: phase, holding, approach, closest approach to the station, level-off.

`phase`: `ground` | `climb` | `cruise` | `descent` | `approach` | `holding` | `turning` | `unknown`. `cpa`: closest approach to the receiver within 30 min (`inSec` 0 = now / moving away). `approach` needs imported runway data (`/api/navdata/import`). `404` as for `path`.

Response: [trajectory.Intent](#t-trajectory-Intent)

Example: `GET /api/prediction/intent/3C473C`

```jsonc
{
  "icao": "3C473C",
  "t": 1795821925287,
  "phase": "cruise",
  "plan": "decay",
  "selTrack": 283.83536474839264,
  "cpa": {
    "inSec": 0,
    "t": 1795821925287,
    "distKm": 321.84020517466524,
    "lat": 51.68519554210845,
    "lon": 4.814762785590533,
    "alt": 32000
  }
}
```

### `GET /api/prediction/proximity`

Predicted close approaches between airborne aircraft (informational only, not an ATC/TCAS function).

| Parameter | Meaning |
|---|---|
| `km` | only pairs currently closer than this, 1..200 km (default 40) |
| `horizon` | look-ahead seconds, 10..600 (default 120) |
| `hkm` | horizontal threshold km (default 9.26 = 5 NM) |
| `vft` | vertical threshold ft (default 1000) |
| `max` | most pairs returned, 1..1000 (default 200) |

`pairs` sorted by `severity` = max(hDist/hkm, vSep/vft) at the closest point (`tcpa` s from now); <= 1 means both thresholds are violated.

Server-side cache: 5s (identical requests share one computation).

Response: [server.proximityReply](#t-server-proximityReply)

Example: `GET /api/prediction/proximity`

```jsonc
{
  "now": 1795821925286,
  "note": "Predicted from received data only; informational, not an ATC or collision-avoidance function.",
  "pairs": []
}
```

## Terrain

### `GET /api/terrain/status`

Terrain analysis job (download / sampling / coverage progress), result availability, tile cache.

Poll every second while `job.state` is `running` (`job.phase` `tiles` -> `terrain` -> `coverage`, `job.progress` 0..1), else rarely. `dataComplete=false`: some elevation tiles were missing (read as sea level). Show `attribution` with a link to `attributionUrl` wherever terrain-derived layers are visible. The analysis runs automatically when the receiver is located and `config.terrain.autoPrefetch` is on (about 30-40 MB of tiles for 450 km, downloaded once), and again after a change of position, antenna heights, `kFactor` or `radiusKm`.

Response: [server.terrainStatus](#t-server-terrainStatus)

Example: `GET /api/terrain/status`

```jsonc
{
  "job": {
    "state": "done",
    "phase": "coverage",
    "progress": 1,
    "phaseProgress": 1,
    "tiles": {
      "zoom": 0,
      "total": 0,
      "done": 0,
      "cached": 0,
      "downloaded": 0,
      "noData": 0,
      "failed": 0,
      "bytes": 0,
      "offline": false
    },
    "message": "terrain analysis complete",
    "error": "",
    "key": "ac29bd2a6be9fff2",
    "startedMs": 1795821863060,
    "finishedMs": 1795821863756,
    "fromCache": false
  },
  "available": true,
  "key": "ac29bd2a6be9fff2",
  "computedMs": 1795821863756,
  "dataComplete": true,
  "provider": null,
  "attribution": "Terrain: Mapzen Terrain Tiles on AWS Open Data (SRTM, GMTED2010, 3DEP courtesy USGS; ETOPO1 NOAA; EU-DEM Copernicus; and others)",
  "attributionUrl": "https://github.com/tilezen/joerd/blob/master/docs/attribution.md",
  "enabled": true,
  "autoPrefetch": true,
  "located": true
}
```

### `POST /api/terrain/prefetch`

Download the elevation tiles and run the terrain analysis now (also when automatic prefetching is off).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

| Parameter | Meaning |
|---|---|
| `force` | `1` re-runs it ignoring the result cache |

Response: `{"started":bool,"job":JobStatus}`; `409` when terrain is disabled or the receiver is not located.

Example: `POST /api/terrain/prefetch` -> `200`

```jsonc
{
  "job": {
    "state": "done",
    "phase": "coverage",
    "progress": 1,
    "phaseProgress": 1,
    "tiles": {
      "zoom": 0,
      "total": 0,
      "done": 0,
      "cached": 0,
      "downloaded": 0,
      "noData": 0,
      "failed": 0,
      "bytes": 0,
      "offline": false
    },
    "message": "terrain analysis complete",
    "error": "",
    "key": "ac29bd2a6be9fff2",
    "startedMs": 1795821863060,
    "finishedMs": 1795821863756,
    "fromCache": false
  },
  "started": false
}
```

### `POST /api/terrain/recompute`

Re-run the last terrain analysis ignoring caches (also resets the download back-off).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `202 {"started":bool,"job":JobStatus}`; `409` when nothing was submitted yet.

Example: `POST /api/terrain/recompute` -> `202`

```jsonc
{
  "job": {
    "state": "done",
    "phase": "coverage",
    "progress": 1,
    "phaseProgress": 1,
    "tiles": {
      "zoom": 0,
      "total": 0,
      "done": 0,
      "cached": 0,
      "downloaded": 0,
      "noData": 0,
      "failed": 0,
      "bytes": 0,
      "offline": false
    },
    "message": "terrain analysis complete",
    "error": "",
    "key": "ac29bd2a6be9fff2",
    "startedMs": 1795821863060,
    "finishedMs": 1795821863756,
    "fromCache": false
  },
  "started": true
}
```

### `POST /api/terrain/cancel`

Cancel a running terrain job (the previous result stays).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `JobStatus`

Example: `POST /api/terrain/cancel` -> `200`

```jsonc
{
  "state": "done",
  "phase": "coverage",
  "progress": 1,
  "phaseProgress": 1,
  "tiles": {
    "zoom": 0,
    "total": 0,
    "done": 0,
    "cached": 0,
    "downloaded": 0,
    "noData": 0,
    "failed": 0,
    "bytes": 0,
    "offline": false
  },
  "message": "terrain analysis complete",
  "error": "",
  "key": "ac29bd2a6be9fff2",
  "startedMs": 1795821863060,
  "finishedMs": 1795821863756,
  "fromCache": false
}
```

### `GET /api/terrain/coverage`

The complete terrain analysis: per antenna the horizon panorama, line-of-sight and radio coverage rings per altitude, advice.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID whose site advice is returned in `advice` (default: the first antenna); `404` for an unknown antenna |

Response: `terrain.Analysis` JSON (0.3-1 MB: fetch once per `key`, see `/api/terrain/status`). Per antenna: `horizon`, `los[]` and `radio[]` (`altFt`, `rangeKm[]` per bearing step, `minKm`, `maxKm`, `meanKm`, `areaKm2`), `advice`. `503` until the first analysis finished.

Example: `GET /api/terrain/coverage`

```jsonc
{
  "key": "ac29bd2a6be9fff2",
  "computedMs": 1795821863756,
  "computeMs": 696,
  "lat": 50.05,
  "lon": 8.6,
  "groundM": 110,
  "groundSource": "dem",
  "demGroundM": 110,
  "demGroundOk": true,
  "k": 1.3333333333333333,
  "radiusKm": 450,
  "stepDeg": 0.5,
  "sampleKm": 0.1125,
  "zoom": 9,
  "noDataFraction": 0,
  "dataComplete": true,
  "altitudesFt": [1000, 3000, 5000, 10000, 20000, 30000, 40000],
  "antennas": [
    {
      "id": "vertical",
      "name": "Vertical",
      "heightAglM": 8,
      "antennaM": 118,
      "horizon": {
        "antennaId": "vertical",
        "antennaM": 118,
        "k": 1.3333,
        "stepDeg": 0.5,
        "radiusKm": 450,
        "minObstacleKm": 0.3,
        "smoothDipDeg": -0.302,
        "smoothHorizonKm": 44.77,
        "meanSkylineDeg": 0.0607,
        "maxSkylineDeg": 1.0685,
        "maxSkylineBrgDeg": 41.5,
        "bearingDeg": [0, 0.5, 1, 1.5, 2, 2.5, 3, 3.5, /* … 712 more */],
        "elevationDeg": [-0.0786, -0.0786, -0.068, -0.0522, -0.0364, -0.0207, -0.005, 0.0107, /* … 712 more */],
        "distanceKm": [11.7, 11.7, 26.775, 26.775, 26.775, 26.775, 26.888, 26.888, /* … 712 more */],
        "terrainM": [110, 110, 128.4, 135.8, 143.2, 150.5, 158.2, 165.6, /* … 712 more */],
        "nearestObstacleKm": [-1, -1, -1, -1, -1, -1, -1, 24.413, /* … 712 more */],
        "nearestObstacleDeg": [0, 0, 0, 0, 0, 0, 0, 0.0006, /* … 712 more */],
        "noDataFraction": 0
      },
      "los": [
        {
          "altFt": 1000,
          "altM": 304.8,
          "smoothKm": 116.734,
          "rangeKm": [69.185, 69.185, 67.303, 64.601, 61.995, 59.486, 57.074, 54.762, /* … 712 more */],
          "capped": 0,
          "minKm": 18.501,
          "maxKm": 69.185,
          "meanKm": 59.694,
          "areaKm2": 12267.7
        },
        /* … 6 more */
      ],
      "radio": [
        {
          "altFt": 1000,
          "altM": 304.8,
          "smoothKm": 116.734,
          "rangeKm": [139.466, 139.466, 116.996, 114.464, 111.975, 109.529, 107.018, 104.658, /* … 712 more */],
          "capped": 0,
          "minKm": 23.063,
          "maxKm": 139.466,
          "meanKm": 119.146,
          "areaKm2": 49640.7
        },
        /* … 6 more */
      ],
      "sensitivityDbm": -95.53
    }
  ],
  "advice": {
    "antennaId": "vertical",
    "altitudesFt": [1000, 3000, 5000, 10000, 20000, 30000, 40000],
    "refAltFt": 10000,
    "heights": [
      {
        "extraM": 0,
        "antennaAglM": 8,
        "areaKm2": [12267.7, 43848.1, 75186.2, 153722.6, 311724.5, 470451.5, 597194.3],
        "meanRangeKm": [59.694, 114.739, 151.526, 218.47, 312.769, 385.033, 434.642],
        "areaGainPct": [0, 0, 0, 0, 0, 0, 0]
      },
      /* … 3 more */
    ],
    "km2PerMetre": [205.7, 383.2, 500.3, 714.2, 1016.7, 1249.1, 109.2],
    "sectors": [
      {
        "sector": 0,
        "fromDeg": 0,
        "toDeg": 10,
        "maxSkylineDeg": 0.198,
        "meanSkylineDeg": 0.051,
        "limitDistKm": 27.225,
        "limitLat": 50.29147,
        "limitLon": 8.66325,
        "limitTerrainM": 255.6,
        "rangeKm": 216.03,
        "smoothKm": 272.3,
        "blockedPct": 20.7,
        "nearestObstacleKm": 18,
        "nearestObstacleDeg": 0.004,
        "nearestObstacleBrg": 9.5,
        "nearestObstacleLat": 50.20965,
        "nearestObstacleLon": 8.64175,
        "nearestObstacleTerrainM": 138.2
      },
      /* … 35 more */
    ],
    "worstSectors": [4, 3, 5, 2, 6],
    "summary": ["Raising the antenna by 10 m adds 16.8 % coverage area at 1,000 ft (+2057 km², about 206 km² per metre).", "Raising the antenna by 10 m adds 4.7 % coverage area at 10,000 ft (+7142 km², about 714 km² per metre).", "Sector 040°–050°: skyline up to 1.07° at 37.1 km; range at 10,000 ft 56 % below the smooth-earth horizon. Nearest obstacle above the ho…", "Sector 030°–040°: skyline up to 1.02° at 35.9 km; range at 10,000 ft 54 % below the smooth-earth horizon. Nearest obstacle above the ho…", "Sector 050°–060°: skyline up to 0.88° at 32.9 km; range at 10,000 ft 49 % below the smooth-earth horizon. Nearest obstacle above the ho…"],
    "summaryItems": [
      {
        "code": "terrain.height-gain",
        "text": "Raising the antenna by 10 m adds 16.8 % coverage area at 1,000 ft (+2057 km², about 206 km² per metre).",
        "data": {
          "altFt": 1000,
          "areaKm2": 2057.3,
          "heightM": 10,
          "km2PerM": 205.7,
          "pct": 16.77
        }
      },
      /* … 4 more */
    ]
  },
  "attribution": "Terrain: Mapzen Terrain Tiles on AWS Open Data (SRTM, GMTED2010, 3DEP courtesy USGS; ETOPO1 NOAA; EU-DEM Copernicus; and others)",
  "attributionUrl": "https://github.com/tilezen/joerd/blob/master/docs/attribution.md"
}
```

### `GET /api/terrain/coverage.geojson`

Theoretical coverage per altitude as GeoJSON polygons for the map.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |
| `kind` | `los` (geometric line of sight with refraction; default) or `radio` (link budget incl. diffraction) |

Response: FeatureCollection, one Polygon per altitude (highest first), properties `kind`, `antennaId`, `altFt`, `altM`, `areaKm2`, `meanKm`, `maxKm`, `minKm`, `smoothKm`, `capped`, `color`. `503` before the first analysis, `404` unknown antenna.

Example: `GET /api/terrain/coverage.geojson?kind=radio`

```jsonc
{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "geometry": {
        "type": "Polygon",
        "coordinates": [
          [
            [8.6, 54.09694],
            [8.66017, 54.09677],
            /* … 719 more */
          ]
        ]
      },
      "properties": {
        "altFt": 40000,
        "altM": 12192,
        "antennaId": "vertical",
        "areaKm2": 599196.6,
        "capped": 570,
        "color": "#f87171",
        "kind": "radio",
        "maxKm": 450,
        "meanKm": 435.503,
        "minKm": 326.18,
        "smoothKm": 499.622
      }
    },
    /* … 6 more */
  ]
}
```

### `GET /api/terrain/horizon`

Horizon panorama of one antenna: terrain mask angle, distance and height of the skyline per bearing.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |

Response: `terrain.Horizon` JSON: `bearingDeg[]`, `elevationDeg[]`, `distanceKm[]`, `terrainM[]`, `nearestObstacleKm[]`, `smoothDipDeg` (smooth-earth horizon dip), `meanSkylineDeg`, `maxSkylineDeg`, `noDataFraction`.

Example: `GET /api/terrain/horizon`

```jsonc
{
  "antennaId": "vertical",
  "antennaM": 118,
  "k": 1.3333,
  "stepDeg": 0.5,
  "radiusKm": 450,
  "minObstacleKm": 0.3,
  "smoothDipDeg": -0.302,
  "smoothHorizonKm": 44.77,
  "meanSkylineDeg": 0.0607,
  "maxSkylineDeg": 1.0685,
  "maxSkylineBrgDeg": 41.5,
  "bearingDeg": [0, 0.5, 1, 1.5, 2, 2.5, 3, 3.5, /* … 712 more */],
  "elevationDeg": [-0.0786, -0.0786, -0.068, -0.0522, -0.0364, -0.0207, -0.005, 0.0107, /* … 712 more */],
  "distanceKm": [11.7, 11.7, 26.775, 26.775, 26.775, 26.775, 26.888, 26.888, /* … 712 more */],
  "terrainM": [110, 110, 128.4, 135.8, 143.2, 150.5, 158.2, 165.6, /* … 712 more */],
  "nearestObstacleKm": [-1, -1, -1, -1, -1, -1, -1, 24.413, /* … 712 more */],
  "nearestObstacleDeg": [0, 0, 0, 0, 0, 0, 0, 0.0006, /* … 712 more */],
  "noDataFraction": 0
}
```

### `GET /api/terrain/panorama`

Same as `/api/terrain/horizon`.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |

Response: `terrain.Horizon` JSON (see `/api/terrain/horizon`).

Example: `GET /api/terrain/panorama`

```jsonc
{
  "antennaId": "vertical",
  "antennaM": 118,
  "k": 1.3333,
  "stepDeg": 0.5,
  "radiusKm": 450,
  "minObstacleKm": 0.3,
  "smoothDipDeg": -0.302,
  "smoothHorizonKm": 44.77,
  "meanSkylineDeg": 0.0607,
  "maxSkylineDeg": 1.0685,
  "maxSkylineBrgDeg": 41.5,
  "bearingDeg": [0, 0.5, 1, 1.5, 2, 2.5, 3, 3.5, /* … 712 more */],
  "elevationDeg": [-0.0786, -0.0786, -0.068, -0.0522, -0.0364, -0.0207, -0.005, 0.0107, /* … 712 more */],
  "distanceKm": [11.7, 11.7, 26.775, 26.775, 26.775, 26.775, 26.888, 26.888, /* … 712 more */],
  "terrainM": [110, 110, 128.4, 135.8, 143.2, 150.5, 158.2, 165.6, /* … 712 more */],
  "nearestObstacleKm": [-1, -1, -1, -1, -1, -1, -1, 24.413, /* … 712 more */],
  "nearestObstacleDeg": [0, 0, 0, 0, 0, 0, 0, 0.0006, /* … 712 more */],
  "noDataFraction": 0
}
```

### `GET /api/terrain/advice`

Antenna site advice: coverage gain of a higher mast, worst sectors with the limiting ridge, ready-made sentences.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |

Response: `terrain.SiteAdvice` JSON: `summary[]` sentences, `heights[]` (`extraM`, `areaKm2[]` per altitude, `areaGainPct[]`), `km2PerMetre[]`, `sectors[36]` and `worstSectors[]` (`blockedPct`, `limitDistKm`, `limitLat`, `limitLon`); `antennaId` says which antenna it is for. `503` before the first analysis, `404` unknown antenna.

Example: `GET /api/terrain/advice`

```jsonc
{
  "antennaId": "vertical",
  "altitudesFt": [1000, 3000, 5000, 10000, 20000, 30000, 40000],
  "refAltFt": 10000,
  "heights": [
    {
      "extraM": 0,
      "antennaAglM": 8,
      "areaKm2": [12267.7, 43848.1, 75186.2, 153722.6, 311724.5, 470451.5, 597194.3],
      "meanRangeKm": [59.694, 114.739, 151.526, 218.47, 312.769, 385.033, 434.642],
      "areaGainPct": [0, 0, 0, 0, 0, 0, 0]
    },
    /* … 3 more */
  ],
  "km2PerMetre": [205.7, 383.2, 500.3, 714.2, 1016.7, 1249.1, 109.2],
  "sectors": [
    {
      "sector": 0,
      "fromDeg": 0,
      "toDeg": 10,
      "maxSkylineDeg": 0.198,
      "meanSkylineDeg": 0.051,
      "limitDistKm": 27.225,
      "limitLat": 50.29147,
      "limitLon": 8.66325,
      "limitTerrainM": 255.6,
      "rangeKm": 216.03,
      "smoothKm": 272.3,
      "blockedPct": 20.7,
      "nearestObstacleKm": 18,
      "nearestObstacleDeg": 0.004,
      "nearestObstacleBrg": 9.5,
      "nearestObstacleLat": 50.20965,
      "nearestObstacleLon": 8.64175,
      "nearestObstacleTerrainM": 138.2
    },
    /* … 35 more */
  ],
  "worstSectors": [4, 3, 5, 2, 6],
  "summary": ["Raising the antenna by 10 m adds 16.8 % coverage area at 1,000 ft (+2057 km², about 206 km² per metre).", "Raising the antenna by 10 m adds 4.7 % coverage area at 10,000 ft (+7142 km², about 714 km² per metre).", "Sector 040°–050°: skyline up to 1.07° at 37.1 km; range at 10,000 ft 56 % below the smooth-earth horizon. Nearest obstacle above the ho…", "Sector 030°–040°: skyline up to 1.02° at 35.9 km; range at 10,000 ft 54 % below the smooth-earth horizon. Nearest obstacle above the ho…", "Sector 050°–060°: skyline up to 0.88° at 32.9 km; range at 10,000 ft 49 % below the smooth-earth horizon. Nearest obstacle above the ho…"],
  "summaryItems": [
    {
      "code": "terrain.height-gain",
      "text": "Raising the antenna by 10 m adds 16.8 % coverage area at 1,000 ft (+2057 km², about 206 km² per metre).",
      "data": {
        "altFt": 1000,
        "areaKm2": 2057.3,
        "heightM": 10,
        "km2PerM": 205.7,
        "pct": 16.77
      }
    },
    /* … 4 more */
  ]
}
```

### `GET /api/terrain/floor`

Coverage floor grid: lowest altitude visible from the antenna per cell.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |
| `cell` | cell size in degrees (default 0.05 = the heat-map cells) |

Response: `{cellDeg, south, west, north, east, rows, cols, floorFt[] (row 0 = south, -1 = not visible), heatAligned}`

Example: `GET /api/terrain/floor?cell=0.5`

```jsonc
{
  "antennaId": "",
  "cellDeg": 0.5,
  "south": 46,
  "west": 1.5,
  "north": 54.5,
  "east": 16,
  "rows": 17,
  "cols": 29,
  "floorFt": [-1, -1, -1, -1, -1, -1, -1, -1, /* … 485 more */],
  "heatAligned": false
}
```

### `GET /api/terrain/floor-image`

Coverage floor as a map image: URL, corner coordinates and legend for a MapLibre `image` source.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |
| `w` | image width px (default 1024, max 2048) |

Response: `{url, width, height, coordinates[4][lon,lat] (TL, TR, BR, BL), legend[{maxFt, color, label}], key}`

Example: `GET /api/terrain/floor-image?w=256`

```jsonc
{
  "url": "/api/terrain/floor.png?k=ac29bd2a6be9fff2\u0026w=256",
  "width": 256,
  "height": 235,
  "coordinates": [
    [1.69884, 54.096947],
    [15.50116, 54.096947],
    /* … 2 more */
  ],
  "legend": [
    {
      "maxFt": 1000,
      "color": "#22d3ee",
      "label": "\u003c 1 000 ft"
    },
    /* … 7 more */
  ],
  "key": "ac29bd2a6be9fff2"
}
```

### `GET /api/terrain/floor.png`

The coverage floor PNG (Web Mercator rows, transparent where nothing is visible). Use the `url` from `/api/terrain/floor-image`.

| Parameter | Meaning |
|---|---|
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |
| `w` | width px |
| `k` | cache buster (analysis key) |

Response: `image/png`

Example: `GET /api/terrain/floor.png?k=ac29bd2a6be9fff2&w=256` -> `200`, `image/png`, 5137 bytes.

### `GET /api/terrain/visible`

Is a point above the terrain radio horizon? (~100 ns, from the panorama.)

| Parameter | Meaning |
|---|---|
| `lat`, `lon` | degrees (required) |
| `altFt` | altitude ft AMSL (required) |
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |

Response: `{known, visible, marginDeg, elevationDeg, skylineDeg, floorFt, distKm, bearingDeg, beyondRange, antennaId}`

Example: `GET /api/terrain/visible?lat=50.4&lon=9.1&altFt=3000`

```jsonc
{
  "known": true,
  "visible": false,
  "marginDeg": -0.3757332653734009,
  "elevationDeg": 0.6875262013612644,
  "skylineDeg": 1.0632594667346653,
  "floorFt": 4134.895671432403,
  "distKm": 52.7240293569901,
  "bearingDeg": 42.23425407562048,
  "beyondRange": false,
  "antennaId": "vertical"
}
```

### `GET /api/terrain/los`

Line of sight from the antenna to one point: obstruction, Fresnel clearance, diffraction loss and the terrain profile.

| Parameter | Meaning |
|---|---|
| `lat`, `lon` | degrees (required) |
| `altFt` | altitude ft AMSL (required) |
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |
| `profile` | `0` omits the profile array |
| `k` | effective earth radius factor for this path, clamped to 0.5..10 (default: the analysis' `k`, `config.terrain.kFactor`); the result's `k` is the value used |

Response: `{clear, groundKm, slantKm, bearingDeg, elevationDeg, obstruction|null, minClearanceM, fresnelRatio, fresnel60Clear, diffractionDb, edges[], fsplDb, gasDb, totalLossDb, noDataSamples, profile[{distKm, lat, lon, terrainM, bulgeM, rayM, fresnelM}]}`. Downloads the path tiles on demand (up to 60 s).

Example: `GET /api/terrain/los?lat=50.5&lon=9.2&altFt=10000&profile=0`

```jsonc
{
  "clear": true,
  "rxLat": 50.05,
  "rxLon": 8.6,
  "rxGroundM": 110,
  "rxAntennaM": 118,
  "tgtLat": 50.5,
  "tgtLon": 9.2,
  "tgtAltM": 3048,
  "k": 1.3333333333333333,
  "groundKm": 65.74049629345481,
  "slantKm": 65.82178287261038,
  "bearingDeg": 40.20516921610062,
  "elevationDeg": 2.3297428753986176,
  "obstruction": null,
  "minClearanceM": 24.27454624329299,
  "minClearanceDistKm": 0.39963827534015084,
  "fresnelRatio": 2.32244026617215,
  "fresnel60Clear": true,
  "diffractionDb": 0,
  "edges": [],
  "fsplDb": 129.56370601219663,
  "gasDb": 0.2802036052983412,
  "totalLossDb": 129.84390961749497,
  "samples": 659,
  "noDataSamples": 0
}
```

### `GET /api/terrain/profile`

Same as `/api/terrain/los` with `alt` as the altitude parameter, for the path profile chart.

| Parameter | Meaning |
|---|---|
| `lat`, `lon` | degrees (required) |
| `alt` | altitude ft AMSL (required; `altFt` also accepted) |
| `ant` | antenna ID (default: the first antenna; `visible`/`floor`: empty = best of all antennas) |
| `k` | effective earth radius factor for this path, clamped to 0.5..10 (default: the analysis' `k`, `config.terrain.kFactor`); the result's `k` is the value used |

Response: Same as `/api/terrain/los`, always with `profile`.

Example: `GET /api/terrain/profile?lat=50.5&lon=9.2&alt=10000`

```jsonc
{
  "clear": true,
  "rxLat": 50.05,
  "rxLon": 8.6,
  "rxGroundM": 110,
  "rxAntennaM": 118,
  "tgtLat": 50.5,
  "tgtLon": 9.2,
  "tgtAltM": 3048,
  "k": 1.3333333333333333,
  "groundKm": 65.74049629345481,
  "slantKm": 65.82178287261038,
  "bearingDeg": 40.20516921610062,
  "elevationDeg": 2.3297428753986176,
  "obstruction": null,
  "minClearanceM": 24.27454624329299,
  "minClearanceDistKm": 0.39963827534015084,
  "fresnelRatio": 2.32244026617215,
  "fresnel60Clear": true,
  "diffractionDb": 0,
  "edges": [],
  "fsplDb": 129.56370601219663,
  "gasDb": 0.2802036052983412,
  "totalLossDb": 129.84390961749497,
  "samples": 659,
  "noDataSamples": 0,
  "profile": [
    {
      "distKm": 0,
      "lat": 50.05,
      "lon": 8.6,
      "terrainM": 110,
      "bulgeM": 0,
      "rayM": 118,
      "fresnelM": 0
    },
    /* … 658 more */
  ]
}
```

### `GET /api/terrain/elevation`

Terrain elevation at a point from the elevation tiles (downloads the tile when missing).

| Parameter | Meaning |
|---|---|
| `lat`, `lon` | degrees (required) |
| `z` | tile zoom (default 12) |

Response: `{lat, lon, elevM, ok, zoom}` (`ok=false`: no data, sea level assumed).

Example: `GET /api/terrain/elevation?lat=50.1&lon=8.7`

```jsonc
{
  "lat": 50.1,
  "lon": 8.7,
  "elevM": 110,
  "ok": true,
  "zoom": 12
}
```

## RF statistics

### `GET /api/rf/summary`

Headline RF numbers: health score, frame rate, channel occupancy, noise / decode floor, detection range, radars, GNSS index, advice (`advice[]` sentences and `adviceItems[]` `{code,text,data}`).

All `/api/rf/*` sections come from one sliding ~1 h statistics window (`windowSec`) shared between the endpoints (recomputed at most every 2 s). Fractions are 0..1, `*Pct` percent, unknown values `null`. `409` when `config.rfStat.enabled` is off.

Response: [server.rfSummary](#t-server-rfSummary)

Example: `GET /api/rf/summary`

```jsonc
{
  "t": 1795821925000,
  "windowSec": 62,
  "health": 80,
  "healthStatus": "ok",
  "advice": ["Gain may be too low: increase it by 2-3 dB or check the LNA / cable losses."],
  "adviceItems": [
    {
      "code": "advice.gain-low",
      "text": "Gain may be too low: increase it by 2-3 dB or check the LNA / cable losses.",
      "data": {
        "stepMaxDb": 3,
        "stepMinDb": 2
      }
    }
  ],
  "fps": 52.3226,
  "occupancyPct": 0.5185,
  "peakOccupancyPct": 0.6152,
  "crcBadFrac": 0,
  "failFrac": 0.0018,
  "decodeFloorDbfs": -34.7197,
  "noiseFloorEstDbfs": -44.7197,
  "p50Dbfs": -29.054,
  "strongFrac": 0,
  "snrMarginDb": 5.6657,
  "pdPos": 0.5341,
  "range50Km": 188.3333,
  "range90Km": 109.902,
  "activeRadars": 0,
  "adsbAircraft": 9,
  "gnssIndexNow": 0,
  "timingAvailable": true
}
```

### `GET /api/rf/report`

Everything below in one document (`rfstat.Report`).

Response: [rfstat.Report](#t-rfstat-Report)

Example: `GET /api/rf/report`

```jsonc
{
  "t": 1795821925000,
  "windowSec": 62,
  "since": 1795821863000,
  "live": true,
  "signal": {
    "frames": 3238,
    "all": {
      "label": "all",
      "lo": null,
      "hi": null,
      "n": 3238,
      "p05": -33.5225,
      "p50": -29.054,
      "p95": -20.0775,
      "mean": -28.1779,
      "strongFrac": 0,
      "hist": {
        "lo": -36.5,
        "step": 0.5,
        "counts": [2, 2, 11, 31, /* … 38 more */]
      }
    },
    "perAntenna": [
      {
        "label": "vertical",
        "lo": null,
        "hi": null,
        "n": 3238,
        "p05": -33.5225,
        "p50": -29.054,
        "p95": -20.0775,
        "mean": -28.1779,
        "strongFrac": 0,
        "hist": {
          "lo": -36.5,
          "step": 0.5,
          "counts": [2, 2, 11, 31, /* … 38 more */]
        }
      }
    ],
    "perRing": [
      {
        "label": "50 km",
        "lo": 50,
        "hi": 75,
        "n": 487,
        "p05": -28.7825,
        "p50": -22.3375,
        "p95": -17.7625,
        "mean": -22.9225,
        "strongFrac": 0
      },
      /* … 7 more */
    ],
    "perElev": [
      {
        "label": "0–0.5°",
        "lo": 0,
        "hi": 0.5,
        "n": 468,
        "p05": -33.7538,
        "p50": -30.6754,
        "p95": -28.01,
        "mean": -30.7468,
        "strongFrac": 0
      },
      /* … 4 more */
    ],
    "decodeFloorDbfs": -34.7197,
    "noiseFloorEstDbfs": -44.7197,
    "assumedDecodeSnrDb": 10,
    "p50Dbfs": -29.054,
    "p999Dbfs": -16.2797,
    "dynamicRangeDb": 19.4108,
    "snrMarginDb": 5.6657,
    "snrEstDb": 15.6657,
    "strongFrac": 0,
    "nearClipFrac": 0,
    "fading": {
      "windows": 57,
      "kMedianDb": 11.8333,
      "stdMedianDb": 1.4821,
      "kHist": {
        "lo": -10,
        "step": 1,
        "counts": [0, 0, 0, 0, /* … 36 more */]
      },
      "stdHist": {
        "lo": 0,
        "step": 0.25,
        "counts": [0, 0, 3, 5, /* … 36 more */]
      },
      "kByElev": [
        {
          "label": "0–0.5°",
          "kDb": 14.2897,
          "n": 9
        },
        /* … 4 more */
      ],
      "worst": [
        {
          "icao": "485FD9",
          "kDb": 4.7678,
          "stdDb": 3.3527,
          "windows": 6,
          "distKm": 78.657,
          "elevDeg": 8.2386
        },
        /* … 9 more */
      ]
    },
    "multipath": {
      "elevDeg": [0.125, 0.475, 0.525, 0.625, /* … 43 more */],
      "devDb": [0.6054, -0.1581, 0.3389, -0.2728, /* … 43 more */],
      "rippleDb": 0.7484,
      "reflectorHeightM": 6.9,
      "periodDeg": 1.1431,
      "strength": 0.2555,
      "detected": false,
      "bins": 47
    },
    "aspect": {
      "fromDeg": [0, 10, 20, 30, /* … 14 more */],
      "meanDb": [-0.1053, null, 0.4231, -0.0558, /* … 14 more */],
      "stdDb": [1.7754, null, 1.8244, 1.8805, /* … 14 more */],
      "n": [916, 7, 68, 106, /* … 14 more */],
      "noseDb": -0.0696,
      "beamDb": -0.1327,
      "tailDb": -0.2533,
      "noseTailDb": 0.1838
    }
  },
  "detection": {
    "types": [
      {
        "type": "position",
        "nominalRate": 2,
        "expected": 880,
        "decoded": 470,
        "pd": 0.5341
      },
      /* … 2 more */
    ],
    "vsDistance": {
      "lo": [0, 10, 20, 30, /* … 46 more */],
      "hi": [10, 20, 30, 40, /* … 46 more */],
      "pos": [null, null, null, null, /* … 46 more */],
      "vel": [null, null, null, null, /* … 46 more */],
      "ident": [null, null, null, null, /* … 46 more */],
      "expected": [0, 0, 0, 0, /* … 46 more */]
    },
    "vsElev": {
      "lo": [null, -1, -0.5, 0, /* … 15 more */],
      "hi": [-1, -0.5, 0, 0.25, /* … 15 more */],
      "pos": [null, null, null, 0.75, /* … 15 more */],
      "vel": [null, null, null, 0.7083, /* … 15 more */],
      "ident": [null, null, null, 0.9167, /* … 15 more */],
      "expected": [0, 0, 0, 120, /* … 15 more */]
    },
    "vsBearing": {
      "lo": [0, 10, 20, 30, /* … 32 more */],
      "hi": [10, 20, 30, 40, /* … 32 more */],
      "pos": [null, null, 0.41, 0.87, /* … 32 more */],
      "vel": [null, null, 0.39, 0.84, /* … 32 more */],
      "ident": [null, null, 0.6, 0.9, /* … 32 more */],
      "expected": [0, 0, 100, 100, /* … 32 more */]
    },
    "vsRssi": {
      "lo": [-50, -48, -46, -44, /* … 21 more */],
      "hi": [-48, -46, -44, -42, /* … 21 more */],
      "pos": [null, null, null, null, /* … 21 more */],
      "vel": [null, null, null, null, /* … 21 more */],
      "ident": [null, null, null, null, /* … 21 more */],
      "expected": [0, 0, 0, 0, /* … 21 more */]
    },
    "range50Km": 188.3333,
    "range90Km": 109.902,
    "gaps": [
      {
        "loKm": 0,
        "hiKm": 50,
        "n": 0,
        "p50": null,
        "p90": null,
        "p95": null,
        "p99": null,
        "mean": null,
        "tSec": [2, 4, 5, 8, /* … 1 more */],
        "pUpdate": [null, null, null, null, /* … 1 more */]
      },
      /* … 5 more */
    ],
    "aircraft": [
      {
        "icao": "485FD9",
        "distKm": 78.657,
        "pdPos": 0.95,
        "pdVel": 0.94,
        "pdIdent": 0.9,
        "rssi": -23.7613
      },
      /* … 8 more */
    ]
  },
  "channel": {
    "seconds": 62,
    "frames": 3244,
    "fps": 52.3226,
    "byDf": [
      {
        "df": 0,
        "label": "DF0 Short air-air surveillance (ACAS)",
        "count": 346,
        "rate": 5.5806
      },
      /* … 6 more */
    ],
    "byCrc": {
      "addrAccepted": 1109,
      "addrRejected": 6,
      "bad": 0,
      "fixed": 0,
      "ok": 2129,
      "unknown": 0
    },
    "occupancyPct": 0.5185,
    "peakOccupancyPct": 0.6152,
    "meanReplyUs": 99.0949,
    "crcBadFrac": 0,
    "fixedFrac": 0,
    "rejectedFrac": 0.0054,
    "failFrac": 0.0018,
    "overlapProb": 0.0103,
    "impliedRate": 9.3409,
    "excessFail": -0.0085,
    "vsLoad": [
      {
        "loFps": 0,
        "hiFps": 100,
        "seconds": 62,
        "meanFps": 52.1613,
        "failFrac": 0.0019,
        "modelFailFrac": 0.0103
      }
    ],
    "recent": {
      "t0": 1795821625000,
      "frames": [0, 0, 0, 0, /* … 296 more */],
      "bad": [0, 0, 0, 0, /* … 296 more */],
      "occPct": [0, 0, 0, 0, /* … 296 more */]
    },
    "bursts": null,
    "burstCount": 0
  },
  "interrogation": {
    "codes": [
      {
        "code": "II 0",
        "iid": 0,
        "replies": 453,
        "rate": 7.3065,
        "last10m": 453,
        "aircraft": 11,
        "replyingFrac": 0.9167,
        "lockoutLikely": false,
        "timeline": [0, 0, 0, 0, /* … 56 more */],
        "acqSquitter": true
      }
    ],
    "activeRadars": 0,
    "survRateP50": 1.2813,
    "survRateP90": 1.6922,
    "survRateMax": 1.6959,
    "topSurv": [
      {
        "icao": "AC9FDA",
        "rate": 1.6959,
        "replies": 104
      },
      /* … 9 more */
    ],
    "commB": [
      {
        "bds": "2,0",
        "count": 25,
        "frac": 0.0706
      },
      /* … 4 more */
    ],
    "df0Rate": 5.5806,
    "df16Rate": 0,
    "raReports": 0,
    "acasAircraft": 11,
    "activeAircraft": 12,
    "adsbAndModeS": 9,
    "adsbOnly": 0,
    "modeSOnly": 3,
    "radarShare": 1
  },
  "timing": {
    "available": true,
    "types": [
      {
        "type": "position",
        "intervals": 559,
        "badFrac": 0,
        "hist": {
          "lo": 0.4,
          "step": 0.005,
          "counts": [8, 12, 8, 7, /* … 484 more */]
        },
        "nominalLo": 0.4,
        "nominalHi": 0.6,
        "mean": 0.501,
        "std": 0.0563,
        "chi2": 7.3814,
        "uniformOk": true
      },
      /* … 2 more */
    ],
    "altPairs": 451,
    "altRegularity": 0.9778,
    "alt2Pairs": 62,
    "alt2Same": 0.9194,
    "nonCompliant": null,
    "note": "Single receiver: GPS timestamps allow interval statistics only; time-of-arrival ranging or position verification needs several synchronised …"
  },
  "census": {
    "aircraft": 12,
    "adsb": 9,
    "versions": [
      {
        "label": "v2",
        "count": 8,
        "share": 0.8889
      },
      /* … 1 more */
    ],
    "nic": [
      {
        "label": "7",
        "count": 4,
        "share": 0.4444
      },
      /* … 2 more */
    ],
    "nacp": [
      {
        "label": "8",
        "count": 4,
        "share": 0.4444
      },
      /* … 2 more */
    ],
    "sil": [
      {
        "label": "3",
        "count": 9,
        "share": 1
      }
    ],
    "poorIntegrityShare": 0,
    "categories": [
      {
        "label": "A1",
        "count": 1,
        "share": 0.1111
      },
      /* … 4 more */
    ],
    "tc28Share": 0.6667,
    "tc29Share": 1,
    "tc31Share": 0.8889,
    "gnssAltShare": 1,
    "gnssDelta": {
      "lo": 0,
      "step": 25,
      "counts": [102, 0, 0, 0, /* … 14 more */]
    },
    "gnssDeltaMeanFt": 302.125,
    "gnssDeltaByAlt": [
      {
        "band": "\u003c1000",
        "meanFt": 295,
        "stdFt": 150.333,
        "n": 5
      },
      /* … 2 more */
    ],
    "jumpAircraft": 0,
    "degradedNow": 0,
    "degradedList": null,
    "gnssIndexNow": 0,
    "gnssSeries": [
      {
        "t": 1795821840000,
        "index": 0,
        "tot": 9,
        "deg": 0,
        "jumps": 0
      }
    ],
    "gnssCells": [
      {
        "sector": 2,
        "ring": 5,
        "lat": 52.2793,
        "lon": 10.3081,
        "tot": 1,
        "deg": 0,
        "index": 0
      },
      /* … 7 more */
    ],
    "droppedFrames": 0
  },
  "health": {
    "score": 80,
    "status": "ok",
    "items": [
      {
        "key": "gain",
        "label": "Receiver gain",
        "score": 60,
        "status": "warn",
        "text": "Median -29.1 dBFS, decode floor -34.7 dBFS, 0.0 % of frames within 3 dB of full scale. Gain may be too low: most signals sit close to the de…",
        "code": "gain.low",
        "data": {
          "floorDbfs": -34.7197,
          "nearDb": 3,
          "p50Dbfs": -29.054,
          "strongPct": 0
        }
      },
      /* … 6 more */
    ],
    "blockedSectors": null,
    "advice": ["Gain may be too low: increase it by 2-3 dB or check the LNA / cable losses."],
    "adviceItems": [
      {
        "code": "advice.gain-low",
        "text": "Gain may be too low: increase it by 2-3 dB or check the LNA / cable losses.",
        "data": {
          "stepMaxDb": 3,
          "stepMinDb": 2
        }
      }
    ]
  }
}
```

### `GET /api/rf/signal`

Signal levels: RSSI histogram, decode floor, noise-floor estimate, overload share, per antenna / range ring / elevation, fading, multipath, aspect.

`noiseFloorEstDbfs` is an estimate (decode floor minus an assumed decode SNR, `assumedDecodeSnrDb`); label it as such. Strong = within 3 dB of full scale.

Response: [rfstat.SignalStats](#t-rfstat-SignalStats)

Example: `GET /api/rf/signal`

```jsonc
{
  "frames": 3238,
  "all": {
    "label": "all",
    "lo": null,
    "hi": null,
    "n": 3238,
    "p05": -33.5225,
    "p50": -29.054,
    "p95": -20.0775,
    "mean": -28.1779,
    "strongFrac": 0,
    "hist": {
      "lo": -36.5,
      "step": 0.5,
      "counts": [2, 2, 11, 31, 50, 69, 130, 169, /* … 34 more */]
    }
  },
  "perAntenna": [
    {
      "label": "vertical",
      "lo": null,
      "hi": null,
      "n": 3238,
      "p05": -33.5225,
      "p50": -29.054,
      "p95": -20.0775,
      "mean": -28.1779,
      "strongFrac": 0,
      "hist": {
        "lo": -36.5,
        "step": 0.5,
        "counts": [2, 2, 11, 31, 50, 69, 130, 169, /* … 34 more */]
      }
    }
  ],
  "perRing": [
    {
      "label": "50 km",
      "lo": 50,
      "hi": 75,
      "n": 487,
      "p05": -28.7825,
      "p50": -22.3375,
      "p95": -17.7625,
      "mean": -22.9225,
      "strongFrac": 0
    },
    /* … 7 more */
  ],
  "perElev": [
    {
      "label": "0–0.5°",
      "lo": 0,
      "hi": 0.5,
      "n": 468,
      "p05": -33.7538,
      "p50": -30.6754,
      "p95": -28.01,
      "mean": -30.7468,
      "strongFrac": 0
    },
    /* … 4 more */
  ],
  "decodeFloorDbfs": -34.7197,
  "noiseFloorEstDbfs": -44.7197,
  "assumedDecodeSnrDb": 10,
  "p50Dbfs": -29.054,
  "p999Dbfs": -16.2797,
  "dynamicRangeDb": 19.4108,
  "snrMarginDb": 5.6657,
  "snrEstDb": 15.6657,
  "strongFrac": 0,
  "nearClipFrac": 0,
  "fading": {
    "windows": 57,
    "kMedianDb": 11.8333,
    "stdMedianDb": 1.4821,
    "kHist": {
      "lo": -10,
      "step": 1,
      "counts": [0, 0, 0, 0, 0, 0, 0, 0, /* … 32 more */]
    },
    "stdHist": {
      "lo": 0,
      "step": 0.25,
      "counts": [0, 0, 3, 5, 14, 7, 1, 4, /* … 32 more */]
    },
    "kByElev": [
      {
        "label": "0–0.5°",
        "kDb": 14.2897,
        "n": 9
      },
      /* … 4 more */
    ],
    "worst": [
      {
        "icao": "485FD9",
        "kDb": 4.7678,
        "stdDb": 3.3527,
        "windows": 6,
        "distKm": 78.657,
        "elevDeg": 8.2386
      },
      /* … 9 more */
    ]
  },
  "multipath": {
    "elevDeg": [0.125, 0.475, 0.525, 0.625, 0.675, 0.725, 1.025, 1.175, /* … 39 more */],
    "devDb": [0.6054, -0.1581, 0.3389, -0.2728, -0.1245, -0.3125, -0.8607, -0.0576, /* … 39 more */],
    "rippleDb": 0.7484,
    "reflectorHeightM": 6.9,
    "periodDeg": 1.1431,
    "strength": 0.2555,
    "detected": false,
    "bins": 47
  },
  "aspect": {
    "fromDeg": [0, 10, 20, 30, 40, 50, 60, 70, /* … 10 more */],
    "meanDb": [-0.1053, null, 0.4231, -0.0558, null, -0.4315, 0.4292, null, /* … 10 more */],
    "stdDb": [1.7754, null, 1.8244, 1.8805, null, 0.9953, 1.2824, null, /* … 10 more */],
    "n": [916, 7, 68, 106, 0, 39, 345, 0, /* … 10 more */],
    "noseDb": -0.0696,
    "beamDb": -0.1327,
    "tailDb": -0.2533,
    "noseTailDb": 0.1838
  }
}
```

### `GET /api/rf/detection`

Reception probability of position / velocity / identification squitters versus distance, elevation, bearing and RSSI; 50 % / 90 % range; update gaps.

Pd = received / expected squitters per aircraft in 10 s windows (nominal rates 2/s, 2/s, 0.2/s). `vsDistance` etc. are parallel arrays per bin (`lo`, `hi`, `pos`, `vel`, `ident`, `expected`).

Response: [rfstat.DetectionStats](#t-rfstat-DetectionStats)

Example: `GET /api/rf/detection`

```jsonc
{
  "types": [
    {
      "type": "position",
      "nominalRate": 2,
      "expected": 880,
      "decoded": 470,
      "pd": 0.5341
    },
    /* … 2 more */
  ],
  "vsDistance": {
    "lo": [0, 10, 20, 30, 40, 50, 60, 70, /* … 42 more */],
    "hi": [10, 20, 30, 40, 50, 60, 70, 80, /* … 42 more */],
    "pos": [null, null, null, null, null, null, null, 0.95, /* … 42 more */],
    "vel": [null, null, null, null, null, null, null, 0.94, /* … 42 more */],
    "ident": [null, null, null, null, null, null, null, 0.9, /* … 42 more */],
    "expected": [0, 0, 0, 0, 0, 0, 0, 100, /* … 42 more */]
  },
  "vsElev": {
    "lo": [null, -1, -0.5, 0, 0.25, 0.5, 0.75, 1, /* … 11 more */],
    "hi": [-1, -0.5, 0, 0.25, 0.5, 0.75, 1, 1.5, /* … 11 more */],
    "pos": [null, null, null, 0.75, null, 0.25, null, 0.3929, /* … 11 more */],
    "vel": [null, null, null, 0.7083, null, 0.2643, null, 0.3786, /* … 11 more */],
    "ident": [null, null, null, 0.9167, null, 0.2857, null, 0.6, /* … 11 more */],
    "expected": [0, 0, 0, 120, 60, 140, 0, 140, /* … 11 more */]
  },
  "vsBearing": {
    "lo": [0, 10, 20, 30, 40, 50, 60, 70, /* … 28 more */],
    "hi": [10, 20, 30, 40, 50, 60, 70, 80, /* … 28 more */],
    "pos": [null, null, 0.41, 0.87, null, null, null, null, /* … 28 more */],
    "vel": [null, null, 0.39, 0.84, null, null, null, null, /* … 28 more */],
    "ident": [null, null, 0.6, 0.9, null, null, null, null, /* … 28 more */],
    "expected": [0, 0, 100, 100, 0, 0, 0, 0, /* … 28 more */]
  },
  "vsRssi": {
    "lo": [-50, -48, -46, -44, -42, -40, -38, -36, /* … 17 more */],
    "hi": [-48, -46, -44, -42, -40, -38, -36, -34, /* … 17 more */],
    "pos": [null, null, null, null, null, null, null, null, /* … 17 more */],
    "vel": [null, null, null, null, null, null, null, null, /* … 17 more */],
    "ident": [null, null, null, null, null, null, null, null, /* … 17 more */],
    "expected": [0, 0, 0, 0, 0, 0, 0, 60, /* … 17 more */]
  },
  "range50Km": 188.3333,
  "range90Km": 109.902,
  "gaps": [
    {
      "loKm": 0,
      "hiKm": 50,
      "n": 0,
      "p50": null,
      "p90": null,
      "p95": null,
      "p99": null,
      "mean": null,
      "tSec": [2, 4, 5, 8, 12],
      "pUpdate": [null, null, null, null, null]
    },
    /* … 5 more */
  ],
  "aircraft": [
    {
      "icao": "485FD9",
      "distKm": 78.657,
      "pdPos": 0.95,
      "pdVel": 0.94,
      "pdIdent": 0.9,
      "rssi": -23.7613
    },
    /* … 8 more */
  ]
}
```

### `GET /api/rf/intervals`

Position update intervals per range band: percentiles and the probability that a position is at most T seconds old.

`gaps[].pUpdate[i]` belongs to `tSec[i]` (2, 4, 5, 8, 12 s).

Response: [server.rfIntervals](#t-server-rfIntervals)

Example: `GET /api/rf/intervals`

```jsonc
{
  "gaps": [
    {
      "loKm": 0,
      "hiKm": 50,
      "n": 0,
      "p50": null,
      "p90": null,
      "p95": null,
      "p99": null,
      "mean": null,
      "tSec": [2, 4, 5, 8, 12],
      "pUpdate": [null, null, null, null, null]
    },
    /* … 5 more */
  ],
  "types": [
    {
      "type": "position",
      "nominalRate": 2,
      "expected": 880,
      "decoded": 470,
      "pd": 0.5341
    },
    /* … 2 more */
  ],
  "range50Km": 188.3333,
  "range90Km": 109.902
}
```

### `GET /api/rf/channel`

1090 MHz channel: frames per DF, occupancy, CRC failures, garble model, load curve, bursts, last 5 minutes per second.

`overlapProb` is the Poisson probability that another reply overlaps one at the measured rate; `excessFail` = failures beyond that (interference / noise); `impliedRate` = channel rate that would explain the failures (FRUIT).

Response: [rfstat.ChannelStats](#t-rfstat-ChannelStats)

Example: `GET /api/rf/channel`

```jsonc
{
  "seconds": 62,
  "frames": 3244,
  "fps": 52.3226,
  "byDf": [
    {
      "df": 0,
      "label": "DF0 Short air-air surveillance (ACAS)",
      "count": 346,
      "rate": 5.5806
    },
    /* … 6 more */
  ],
  "byCrc": {
    "addrAccepted": 1109,
    "addrRejected": 6,
    "bad": 0,
    "fixed": 0,
    "ok": 2129,
    "unknown": 0
  },
  "occupancyPct": 0.5185,
  "peakOccupancyPct": 0.6152,
  "meanReplyUs": 99.0949,
  "crcBadFrac": 0,
  "fixedFrac": 0,
  "rejectedFrac": 0.0054,
  "failFrac": 0.0018,
  "overlapProb": 0.0103,
  "impliedRate": 9.3409,
  "excessFail": -0.0085,
  "vsLoad": [
    {
      "loFps": 0,
      "hiFps": 100,
      "seconds": 62,
      "meanFps": 52.1613,
      "failFrac": 0.0019,
      "modelFailFrac": 0.0103
    }
  ],
  "recent": {
    "t0": 1795821625000,
    "frames": [0, 0, 0, 0, 0, 0, 0, 0, /* … 292 more */],
    "bad": [0, 0, 0, 0, 0, 0, 0, 0, /* … 292 more */],
    "occPct": [0, 0, 0, 0, 0, 0, 0, 0, /* … 292 more */]
  },
  "bursts": null,
  "burstCount": 0
}
```

### `GET /api/rf/interrogators`

Interrogator census from DF11 replies (II / SI codes, rates, lock-out), surveillance reply rates, Comm-B mix, ACAS activity.

Reply `{t, interrogators}`: `t` is the time of the RF report (Unix ms; the end of every per-minute `timeline`), `interrogators` the census object (before this version it was returned bare, without `t`). II 0 includes DF11 acquisition squitters (`acqSquitter`).

Response: [server.rfInterrogators](#t-server-rfInterrogators)

Example: `GET /api/rf/interrogators`

```jsonc
{
  "t": 1795821925000,
  "interrogators": {
    "codes": [
      {
        "code": "II 0",
        "iid": 0,
        "replies": 453,
        "rate": 7.3065,
        "last10m": 453,
        "aircraft": 11,
        "replyingFrac": 0.9167,
        "lockoutLikely": false,
        "timeline": [0, 0, 0, 0, 0, 0, 0, 0, /* … 52 more */],
        "acqSquitter": true
      }
    ],
    "activeRadars": 0,
    "survRateP50": 1.2813,
    "survRateP90": 1.6922,
    "survRateMax": 1.6959,
    "topSurv": [
      {
        "icao": "AC9FDA",
        "rate": 1.6959,
        "replies": 104
      },
      /* … 9 more */
    ],
    "commB": [
      {
        "bds": "2,0",
        "count": 25,
        "frac": 0.0706
      },
      /* … 4 more */
    ],
    "df0Rate": 5.5806,
    "df16Rate": 0,
    "raReports": 0,
    "acasAircraft": 11,
    "activeAircraft": 12,
    "adsbAndModeS": 9,
    "adsbOnly": 0,
    "modeSOnly": 3,
    "radarShare": 1
  }
}
```

### `GET /api/rf/timing`

Squitter timing from the receiver's GPS timestamps: interval histograms per type, even/odd alternation, non-compliant transmitters.

`available` is false without GPS timestamps. One receiver cannot do TOA ranging (`note`).

Response: [rfstat.TimingStats](#t-rfstat-TimingStats)

Example: `GET /api/rf/timing`

```jsonc
{
  "available": true,
  "types": [
    {
      "type": "position",
      "intervals": 559,
      "badFrac": 0,
      "hist": {
        "lo": 0.4,
        "step": 0.005,
        "counts": [8, 12, 8, 7, 15, 7, 9, 13, /* … 480 more */]
      },
      "nominalLo": 0.4,
      "nominalHi": 0.6,
      "mean": 0.501,
      "std": 0.0563,
      "chi2": 7.3814,
      "uniformOk": true
    },
    /* … 2 more */
  ],
  "altPairs": 451,
  "altRegularity": 0.9778,
  "alt2Pairs": 62,
  "alt2Same": 0.9194,
  "nonCompliant": null,
  "note": "Single receiver: GPS timestamps allow interval statistics only; time-of-arrival ranging or position verification needs several synchronised …"
}
```

### `GET /api/rf/fleet`

Fleet / equipage census: ADS-B versions, NIC / NACp / SIL, poor integrity share, emitter categories, TC28/29/31 use, GNSS vs baro altitude, GNSS interference.

Poor integrity = NACp < 8 or NIC < 7 or SIL < 3.

Response: [rfstat.CensusStats](#t-rfstat-CensusStats)

Example: `GET /api/rf/fleet`

```jsonc
{
  "aircraft": 12,
  "adsb": 9,
  "versions": [
    {
      "label": "v2",
      "count": 8,
      "share": 0.8889
    },
    /* … 1 more */
  ],
  "nic": [
    {
      "label": "7",
      "count": 4,
      "share": 0.4444
    },
    /* … 2 more */
  ],
  "nacp": [
    {
      "label": "8",
      "count": 4,
      "share": 0.4444
    },
    /* … 2 more */
  ],
  "sil": [
    {
      "label": "3",
      "count": 9,
      "share": 1
    }
  ],
  "poorIntegrityShare": 0,
  "categories": [
    {
      "label": "A1",
      "count": 1,
      "share": 0.1111
    },
    /* … 4 more */
  ],
  "tc28Share": 0.6667,
  "tc29Share": 1,
  "tc31Share": 0.8889,
  "gnssAltShare": 1,
  "gnssDelta": {
    "lo": 0,
    "step": 25,
    "counts": [102, 0, 0, 0, 0, 0, 0, 0, /* … 10 more */]
  },
  "gnssDeltaMeanFt": 302.125,
  "gnssDeltaByAlt": [
    {
      "band": "\u003c1000",
      "meanFt": 295,
      "stdFt": 150.333,
      "n": 5
    },
    /* … 2 more */
  ],
  "jumpAircraft": 0,
  "degradedNow": 0,
  "degradedList": null,
  "gnssIndexNow": 0,
  "gnssSeries": [
    {
      "t": 1795821840000,
      "index": 0,
      "tot": 9,
      "deg": 0,
      "jumps": 0
    }
  ],
  "gnssCells": [
    {
      "sector": 2,
      "ring": 5,
      "lat": 52.2793,
      "lon": 10.3081,
      "tot": 1,
      "deg": 0,
      "index": 0
    },
    /* … 7 more */
  ],
  "droppedFrames": 0
}
```

### `GET /api/rf/gnss`

GNSS interference index: share of ADS-B aircraft with degraded navigation integrity, per minute (24 h) and per polar map cell.

Degraded = NACp < 5 after having reported >= 8, or NIC < 5 after >= 7. `cells`: 10 degree x 50 km sectors around the station with centre `lat` / `lon`, colour by `index` (%).

Response: [server.rfGNSS](#t-server-rfGNSS)

Example: `GET /api/rf/gnss`

```jsonc
{
  "indexNow": 0,
  "series": [
    {
      "t": 1795821840000,
      "index": 0,
      "tot": 9,
      "deg": 0,
      "jumps": 0
    }
  ],
  "cells": [
    {
      "sector": 2,
      "ring": 5,
      "lat": 52.2793,
      "lon": 10.3081,
      "tot": 1,
      "deg": 0,
      "index": 0
    },
    /* … 7 more */
  ],
  "degradedNow": 0,
  "degradedList": null,
  "jumpAircraft": 0,
  "gnssDelta": {
    "lo": 0,
    "step": 25,
    "counts": [102, 0, 0, 0, 0, 0, 0, 0, /* … 10 more */]
  },
  "gnssDeltaMeanFt": 302.125,
  "gnssDeltaByAlt": [
    {
      "band": "\u003c1000",
      "meanFt": 295,
      "stdFt": 150.333,
      "n": 5
    },
    /* … 2 more */
  ]
}
```

### `GET /api/rf/health`

RF health: score 0..100 (-1 = no data), scored items with plain-language text, blocked sectors, advice.

Response: [rfstat.Health](#t-rfstat-Health)

Example: `GET /api/rf/health`

```jsonc
{
  "score": 80,
  "status": "ok",
  "items": [
    {
      "key": "gain",
      "label": "Receiver gain",
      "score": 60,
      "status": "warn",
      "text": "Median -29.1 dBFS, decode floor -34.7 dBFS, 0.0 % of frames within 3 dB of full scale. Gain may be too low: most signals sit close to the de…",
      "code": "gain.low",
      "data": {
        "floorDbfs": -34.7197,
        "nearDb": 3,
        "p50Dbfs": -29.054,
        "strongPct": 0
      }
    },
    /* … 6 more */
  ],
  "blockedSectors": null,
  "advice": ["Gain may be too low: increase it by 2-3 dB or check the LNA / cable losses."],
  "adviceItems": [
    {
      "code": "advice.gain-low",
      "text": "Gain may be too low: increase it by 2-3 dB or check the LNA / cable losses.",
      "data": {
        "stepMaxDb": 3,
        "stepMinDb": 2
      }
    }
  ]
}
```

### `GET /api/rf/history`

Hourly RF summaries (frame rate, occupancy, CRC failures, Pd, 90 % range, health ...), oldest first.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms (`to` defaults to now) |
| `hours` | alternative: the last N hours (ending at `to` or now); fractions allowed |
| `days` | alternative: the last N days |

Default 7 days. Closed clock hours only (the current hour appears when it is over).

Server-side cache: 1m0s (identical requests share one computation).

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/rf/history`

```jsonc
{
  "hours": []
}
```

## Navigation data

### `GET /api/navdata/status`

Imported reference datasets (rows, import time, source, licence), downloadable sources, import state, options and attribution.

Nothing is downloaded automatically. Airports, runways, navaids and countries come from OurAirports (public domain), routes from the Virtual Radar Server standing data (CC0). Show `attribution`.

Response: [server.navStatus](#t-server-navStatus)

Example: `GET /api/navdata/status`

```jsonc
{
  "datasets": [
    {
      "dataset": "countries",
      "rows": 0,
      "records": 0,
      "skipped": 0,
      "bytes": 0,
      "source": {
        "dataset": "countries",
        "name": "OurAirports countries.csv",
        "url": "https://davidmegginson.github.io/ourairports-data/countries.csv",
        "homepage": "https://ourairports.com/data/",
        "licence": "Public domain (OurAirports data dumps; repository licence: The Unlicense)",
        "licenceUrl": "https://github.com/davidmegginson/ourairports-data",
        "attribution": "Airport data: OurAirports (ourairports.com)",
        "approxBytes": 25000,
        "format": "csv"
      }
    },
    /* … 4 more */
  ],
  "sources": [
    {
      "dataset": "countries",
      "name": "OurAirports countries.csv",
      "url": "https://davidmegginson.github.io/ourairports-data/countries.csv",
      "homepage": "https://ourairports.com/data/",
      "licence": "Public domain (OurAirports data dumps; repository licence: The Unlicense)",
      "licenceUrl": "https://github.com/davidmegginson/ourairports-data",
      "attribution": "Airport data: OurAirports (ourairports.com)",
      "approxBytes": 25000,
      "format": "csv"
    },
    /* … 4 more */
  ],
  "import": {
    "running": false,
    "fraction": 0
  },
  "regionAirports": 5,
  "routesOnline": true,
  "autoRefresh": true,
  "photos": false,
  "airportsOverlay": true,
  "attribution": ["Airport data: OurAirports (ourairports.com)", "Routes: Virtual Radar Server standing data", "Online routes: adsbdb.com (shown to this station's users only)"]
}
```

### `POST /api/navdata/import`

Start downloading and importing reference datasets in the background.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"datasets":["countries","airports","runways","navaids","routes"],"force":false}` (empty body or empty list = all; about 25 MB in total)

Poll `/api/navdata/progress`. A failed or cancelled dataset keeps its previous contents. A notice is sent when the import finished.

Response: `{"started":true,"import":ImportStatus}`; `409` when an import is running.

Example: `POST /api/navdata/import` with body `{"datasets":["nosuch"]}` -> `200`

```jsonc
{
  "import": {
    "running": true,
    "fraction": 0,
    "started": 1795821925341
  },
  "started": true
}
```

(The documentation build runs offline, so this example names an unknown dataset; it starts and fails at once - `GET /api/navdata/progress` then reports the error. A real import reports `progress` per dataset.)

### `GET /api/navdata/progress`

State of the current or last import (`navdata.ImportStatus`): `running`, `progress{dataset, stage, bytesRead, bytesTotal, records, index, count}`, `fraction` 0..1, `result`, `error`.

Response: [navdata.ImportStatus](#t-navdata-ImportStatus)

Example: `GET /api/navdata/progress`

```jsonc
{
  "running": false,
  "fraction": 0,
  "result": {
    "datasets": [
      {
        "dataset": "nosuch",
        "records": 0,
        "skipped": 0,
        "bytes": 0,
        "elapsedMs": 0,
        "error": "navdata: invalid input: unknown dataset \"nosuch\""
      }
    ],
    "started": 1795821925341,
    "finished": 1795821925341
  },
  "error": "navdata: invalid input: unknown dataset \"nosuch\"",
  "started": 1795821925341
}
```

### `POST /api/navdata/cancel`

Cancel a running import.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"cancelled":true,"import":ImportStatus}`

Example: `POST /api/navdata/cancel` -> `200`

```jsonc
{
  "cancelled": true,
  "import": {
    "running": false,
    "fraction": 0,
    "result": {
      "datasets": [
        {
          "dataset": "nosuch",
          "records": 0,
          "skipped": 0,
          "bytes": 0,
          "elapsedMs": 0,
          "error": "navdata: invalid input: unknown dataset \"nosuch\""
        }
      ],
      "started": 1795821925341,
      "finished": 1795821925341
    },
    "error": "navdata: invalid input: unknown dataset \"nosuch\"",
    "started": 1795821925341
  }
}
```

### `POST /api/navdata/route-cache/clear`

Forget the cached online route look-ups.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`

Example: `POST /api/navdata/route-cache/clear` -> `200`

```jsonc
{
  "ok": true
}
```

### `GET /api/navdata/airports`

Airports in a box or around a point, largest first; optionally with runways or as a GeoJSON map overlay.

| Parameter | Meaning |
|---|---|
| `bbox` | `S,W,N,E` degrees (W > E crosses the antimeridian) |
| `lat`, `lon` | centre instead of `bbox` (default: the receiver) |
| `radiusKm` | radius around lat, lon, up to 5000 (default 100) |
| `minSize` | `large` \| `medium` \| `small` (default) \| `other` \| `all` |
| `limit` | 1..5000 (default 500) |
| `runways` | `1` attaches the runways |
| `format` | `geojson` = FeatureCollection with `kind` `airport` points (label `code`), `runway` polygons, `centerline` lines (needs `runways=1`) |

Map overlay: fetch on `moveend` with the view as `bbox`, `runways=1&format=geojson`, `minSize` by zoom (<= 6 large, <= 8 medium, else small). Empty until the airports are imported.

Server-side cache: 10s (identical requests share one computation).

Response: [navdata.Airport](#t-navdata-Airport)[]

Example: `GET /api/navdata/airports?lat=50.05&lon=8.6&radiusKm=300&minSize=medium`

```jsonc
[
  {
    "id": 2212,
    "ident": "EDDF",
    "type": "large_airport",
    "size": 3,
    "name": "Frankfurt am Main Airport",
    "lat": 50.030241,
    "lon": 8.561096,
    "elevFt": 364,
    "continent": "EU",
    "country": "DE",
    "region": "DE-HE",
    "municipality": "Frankfurt am Main",
    "scheduled": true,
    "icao": "EDDF",
    "iata": "FRA",
    "gps": "EDDF",
    "distKm": 3.54
  },
  /* … 3 more */
]
```

### `GET /api/navdata/search`

Search airports by code or name.

| Parameter | Meaning |
|---|---|
| `q` | text (required) |
| `limit` | 1..200 (default 20) |

Response: [navdata.Airport](#t-navdata-Airport)[]

Example: `GET /api/navdata/search?q=frankfurt`

```jsonc
[
  {
    "id": 2212,
    "ident": "EDDF",
    "type": "large_airport",
    "size": 3,
    "name": "Frankfurt am Main Airport",
    "lat": 50.030241,
    "lon": 8.561096,
    "elevFt": 364,
    "continent": "EU",
    "country": "DE",
    "region": "DE-HE",
    "municipality": "Frankfurt am Main",
    "scheduled": true,
    "icao": "EDDF",
    "iata": "FRA",
    "gps": "EDDF"
  },
  /* … 1 more */
]
```

### `GET /api/navdata/navaids`

Radio navigation aids (VOR, DME, NDB, TACAN) in a box.

| Parameter | Meaning |
|---|---|
| `bbox` | `S,W,N,E` degrees (required) |
| `types` | comma separated, e.g. `VOR,VOR-DME,NDB` |
| `limit` | 1..5000 (default 500) |

Server-side cache: 10s (identical requests share one computation).

Response: [navdata.Navaid](#t-navdata-Navaid)[]

Example: `GET /api/navdata/navaids?bbox=49,7,51,10`

```jsonc
[
  {
    "id": 85001,
    "ident": "FFM",
    "name": "Frankfurt",
    "type": "VOR-DME",
    "freqKHz": 114200,
    "lat": 50.053611,
    "lon": 8.637222,
    "elevFt": 328,
    "country": "DE",
    "dmeChannel": "89X",
    "magVarDeg": 2.5,
    "usage": "BOTH",
    "power": "HIGH",
    "airport": "EDDF"
  }
]
```

### `GET /api/navdata/airport/{code}`

One airport by ICAO, IATA, ident or GPS code, with its runways and usable landing directions.

`404` when unknown (or nothing imported).

Response: [server.navAirportReply](#t-server-navAirportReply)

Example: `GET /api/navdata/airport/EDDF`

```jsonc
{
  "airport": {
    "id": 2212,
    "ident": "EDDF",
    "type": "large_airport",
    "size": 3,
    "name": "Frankfurt am Main Airport",
    "lat": 50.030241,
    "lon": 8.561096,
    "elevFt": 364,
    "continent": "EU",
    "country": "DE",
    "region": "DE-HE",
    "municipality": "Frankfurt am Main",
    "scheduled": true,
    "icao": "EDDF",
    "iata": "FRA",
    "gps": "EDDF",
    "runways": [
      {
        "id": 240001,
        "airportId": 2212,
        "airportIdent": "EDDF",
        "lengthFt": 13123,
        "widthFt": 148,
        "surface": "CON",
        "lighted": true,
        "le": {
          "ident": "07C",
          "lat": 50.032692,
          "lon": 8.534564,
          "elevFt": 328,
          "headingT": 69.9
        },
        "he": {
          "ident": "25C",
          "lat": 50.04503,
          "lon": 8.586981,
          "elevFt": 364,
          "headingT": 249.9
        }
      },
      /* … 1 more */
    ]
  },
  "ends": [
    {
      "airportId": 2212,
      "airportIdent": "EDDF",
      "airportCode": "EDDF",
      "runway": "07C",
      "lat": 50.032692,
      "lon": 8.534564,
      "endLat": 50.032692,
      "endLon": 8.534564,
      "headingT": 69.9,
      "headingSrc": "published",
      "elevFt": 328,
      "lengthFt": 13123,
      "widthFt": 148
    },
    /* … 3 more */
  ]
}
```

### `GET /api/navdata/route/{callsign}`

Route of a callsign (standing data, then the opt-in online look-up) with airports, great-circle path and a plausibility check.

| Parameter | Meaning |
|---|---|
| `lat`, `lon` | current position of the aircraft: enables `check` (plausible / doubtful) |

Call lazily (when the user opens an aircraft), not for every aircraft: the live picture already carries `route` from the offline data. The online look-up (adsbdb.com) only runs for an admin caller with `config.navData.routes.online`; other callers get the standing data and an already cached answer and is cached (24 h, unknown 12 h) and paced (2 s, 1000/day; `429` when exhausted). `found:false` when unknown. `path` is `[lon, lat]` with unwrapped longitudes.

Response: [navdata.RouteInfo](#t-navdata-RouteInfo)

Example: `GET /api/navdata/route/DLH400?lat=50.2&lon=7.9`

```jsonc
{
  "callsign": "DLH400",
  "found": true,
  "source": "standing-data",
  "airline": "DLH",
  "airports": [
    {
      "code": "EDDF",
      "resolved": true,
      "airport": {
        "id": 2212,
        "ident": "EDDF",
        "type": "large_airport",
        "size": 3,
        "name": "Frankfurt am Main Airport",
        "lat": 50.030241,
        "lon": 8.561096,
        "elevFt": 364,
        "continent": "EU",
        "country": "DE",
        "region": "DE-HE",
        "municipality": "Frankfurt am Main",
        "scheduled": true,
        "icao": "EDDF",
        "iata": "FRA",
        "gps": "EDDF"
      }
    },
    /* … 1 more */
  ],
  "path": [
    [8.561096, 50.030241],
    [7.922217, 50.213795],
    /* … 123 more */
  ],
  "distanceKm": 6189,
  "check": {
    "plausible": true,
    "doubtful": false,
    "leg": 0,
    "crossTrackKm": -2,
    "alongPct": 0.8,
    "toleranceKm": 400,
    "reason": "inside the route corridor"
  }
}
```

### `GET /api/navdata/photo/{icao}`

Aircraft photo metadata from planespotters.net (opt-in): thumbnail, large image, link and photographer.

| Parameter | Meaning |
|---|---|
| `reg` | registration (improves the match) |

`409` unless `config.navData.photos` is on and `config.navData.photoContact` is set (planespotters.net requires a contact in the User-Agent). Show the thumbnail only together with `attribution` and link it to `link`; never when `found` is false. Cached 7 days (2 days when none).

Example: `GET /api/navdata/photo/3C6444` -> `409`

```jsonc
{
  "error": "navdata: online lookup is disabled"
}
```

(Photos are off in the documentation instance. With `config.navData.photos` on and a contact set the answer is `navdata.Photo`: `{icao, found, thumbnail:{src,width,height}, large:{...}, link, photographer, attribution, cached}`.)

## Alerts

### `GET /api/alerts/rules`

All alert rules (built-in defaults and user rules), sorted by id.

Rule JSON: `{id, name, description?, enabled, builtin?, severity (info|warning|critical), match: Cond, cooldownSec, ruleCooldownSec?, includePredicted?, message?, actions: {notify, sound, log, webhook?, highlight?}, created, updated}`. `includePredicted` (default false): rules with an `altitude`, `distance`, `geofence` or `bearing` leaf ignore aircraft shown at a predicted (coasting) position unless it is true. Cond: a group `{op: and|or, conds: [...], negate?}` or a leaf `{type, ...}`; leaf types `icao`, `reg`, `callsign`, `operator`, `actype`, `category`, `country`, `squawk` (`values` globs), `military` / `onGround` / `newAirframe` (`value`), `emergency` (`values`), `altitude` / `speed` / `vrate` / `distance` / `adsbVersion` / `nic` / `rssi` (`min`, `max`), `bearing` (`from`, `to`), `geofence` (`lat`, `lon`, `radiusKm` or `polygon` [[lon,lat]...], `inside`), `source`, `watchlist` (`values` list names), `system` (`values` event kinds: `record`, `feed_down`, `feed_up`, `ntp_unsync`, `ntp_sync`, `range_collapse`, `range_restored`, `custom`, `new_airframe`, or `kind:KEY*`). Message placeholders: `/api/alerts/placeholders`.

Response: [alerts.Rule](#t-alerts-Rule)[]

Example: `GET /api/alerts/rules`

```jsonc
[
  {
    "id": "builtin-emergency",
    "name": "Emergency",
    "description": "Emergency squawk 7500/7600/7700 or an ADS-B emergency state",
    "enabled": true,
    "builtin": true,
    "severity": "critical",
    "match": {
      "op": "or",
      "conds": [
        {
          "type": "squawk",
          "values": ["7500", "7600", "7700"]
        },
        /* … 1 more */
      ]
    },
    "cooldownSec": 1800,
    "message": "{rule}: {ident} squawk {squawk} at {alt} ft, {dist} km",
    "actions": {
      "notify": true,
      "sound": true,
      "log": true,
      "highlight": "#ff3b30"
    }
  },
  /* … 10 more */
]
```

### `POST /api/alerts/rules`

Create a rule.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: a Rule without `id` (unknown fields are rejected, at most 64 kB)

Response: the stored Rule (id `r-...`); `400` with a readable message when invalid, `409` beyond 500 rules.

Example: `POST /api/alerts/rules` with body `{"name":"Heavies over the house","enabled":true,"severity":"info","match":{"op":"and","conds":[{"type":"category","values":["A5"]},{"type":"distance","max":25},{"type":"altitude","max":10000}]},"cooldownSec":900,"message":"{ident} ({type}) at {alt} ft, {dist} km","actions":{"notify":true,"sound":false,"log":true,"highlight":"#0a84ff"}}` -> `200`

```jsonc
{
  "id": "r-b89465471a94",
  "name": "Heavies over the house",
  "enabled": true,
  "severity": "info",
  "match": {
    "op": "and",
    "conds": [
      {
        "type": "category",
        "values": ["A5"]
      },
      /* … 2 more */
    ]
  },
  "cooldownSec": 900,
  "message": "{ident} ({type}) at {alt} ft, {dist} km",
  "actions": {
    "notify": true,
    "sound": false,
    "log": true,
    "highlight": "#0a84ff"
  },
  "created": 1795821925646,
  "updated": 1795821925646
}
```

### `GET /api/alerts/rules/{id}`

One rule.

`404` when unknown.

Response: [alerts.Rule](#t-alerts-Rule)

Example: `GET /api/alerts/rules/builtin-military`

```jsonc
{
  "id": "builtin-military",
  "name": "Military aircraft",
  "enabled": true,
  "builtin": true,
  "severity": "info",
  "match": {
    "type": "military"
  },
  "cooldownSec": 3600,
  "actions": {
    "notify": true,
    "sound": false,
    "log": true,
    "highlight": "#ffb020"
  }
}
```

### `PUT /api/alerts/rules/{id}`

Replace a rule (built-in rules may be edited; reset-defaults restores them).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: a Rule

Response: the stored Rule; `404` when unknown.

Example: `PUT /api/alerts/rules/r-b89465471a94` with body `{"name":"Heavies over the house","enabled":true,"severity":"warning","match":{"op":"and","conds":[{"type":"category","values":["A5"]},{"type":"distance","max":30}]},"cooldownSec":900,"actions":{"notify":true,"sound":true,"log":true,"highlight":"#0a84ff"}}` -> `200`

```jsonc
{
  "id": "r-b89465471a94",
  "name": "Heavies over the house",
  "enabled": true,
  "severity": "warning",
  "match": {
    "op": "and",
    "conds": [
      {
        "type": "category",
        "values": ["A5"]
      },
      /* … 1 more */
    ]
  },
  "cooldownSec": 900,
  "actions": {
    "notify": true,
    "sound": true,
    "log": true,
    "highlight": "#0a84ff"
  },
  "created": 1795821925646,
  "updated": 1795821925647
}
```

### `DELETE /api/alerts/rules/{id}`

Delete a rule.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`; `404` when unknown.

Example: `DELETE /api/alerts/rules/r-84b154b640ca` -> `200`

```jsonc
{
  "ok": true
}
```

### `POST /api/alerts/rules/{id}/enabled`

Switch a rule on or off.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"enabled":true|false}`

Response: the stored Rule

Example: `POST /api/alerts/rules/builtin-new-airframe/enabled` with body `{"enabled":true}` -> `200`

```jsonc
{
  "id": "builtin-new-airframe",
  "name": "New airframe",
  "description": "An aircraft never seen before by this station",
  "enabled": true,
  "builtin": true,
  "severity": "info",
  "match": {
    "type": "newAirframe"
  },
  "cooldownSec": 86400,
  "actions": {
    "notify": true,
    "sound": false,
    "log": true,
    "highlight": "#34c759"
  },
  "created": 1795821925647,
  "updated": 1795821925647
}
```

### `POST /api/alerts/rules/reset-defaults`

Restore the built-in rules to their shipped state (user rules are kept).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`

Example: `POST /api/alerts/rules/reset-defaults` -> `200`

```jsonc
{
  "ok": true
}
```

### `POST /api/alerts/test`

Test a rule against the current live picture without firing anything.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"rule": Rule, "event": {"kind":"record","key":"max_dist_km","text":"..."}?}` (the event only for system rules)

Response: `{"aircraft": n tested, "matches": [{icao, callsign, reg, type, text}]}` (at most 500); `400` when the rule is invalid.

Example: `POST /api/alerts/test` with body `{"rule":{"name":"Above FL300","enabled":true,"severity":"info","match":{"type":"altitude","min":30000},"actions":{}}}` -> `200`

```jsonc
{
  "aircraft": 12,
  "matches": [
    {
      "icao": "3C473C",
      "callsign": "DLH81AS",
      "text": "Above FL300: DLH81AS [3C473C] sq 2017 32000 ft 322.4 km"
    },
    /* … 6 more */
  ]
}
```

### `GET /api/alerts/recent`

Recently fired alerts, newest first (in-memory ring of 500; logged ones are also in `/api/stats/events?kind=alert`).

| Parameter | Meaning |
|---|---|
| `limit` | 1..500 (default 100) |
| `after` | only alerts with an id above this |

The same objects arrive live as SSE `event: alert`.

Response: [alerts.Alert](#t-alerts-Alert)[]

Example: `GET /api/alerts/recent?limit=5`

```jsonc
[
  {
    "id": 1,
    "t": 1795821907020,
    "ruleId": "r-25b1899d9e91",
    "ruleName": "Example rule",
    "severity": "warning",
    "kind": "aircraft",
    "icao": "3C473C",
    "callsign": "DLH81AS",
    "operator": "Lufthansa",
    "squawk": "2017",
    "lat": 51.67543029785156,
    "lon": 4.878226615287162,
    "altFt": 32000,
    "distKm": 318.3020602876175,
    "gsKt": 413.2807762284619,
    "vrFpm": 0,
    "trackDeg": 284.0026191880347,
    "brgDeg": 306.04226329401286,
    "rssi": -33.3,
    "text": "Example rule: DLH81AS at 32000 ft, 318.3 km",
    "notify": true,
    "sound": true,
    "highlight": "#ffd60a"
  }
]
```

### `GET /api/alerts/highlights`

Aircraft currently highlighted by a rule (also in the live picture as `aircraft[].highlight`).

`until` = Unix ms the highlight lasts without a new match (15 s hold).

Response: [alerts.Highlight](#t-alerts-Highlight)[]

Example: `GET /api/alerts/highlights`

```jsonc
[
  {
    "icao": "3C473C",
    "color": "#ffd60a",
    "ruleId": "r-25b1899d9e91",
    "severity": "warning",
    "until": 1795821940277
  },
  /* … 9 more */
]
```

### `GET /api/alerts/stats`

Alert engine counters: rules, evaluations, last evaluation time, fired / suppressed, webhook sent / failed / dropped, range baseline.

Response: [alerts.Stats](#t-alerts-Stats)

Example: `GET /api/alerts/stats`

```jsonc
{
  "rules": 12,
  "enabledRules": 5,
  "evaluations": 65,
  "lastEvalMicros": 6,
  "lastEvalNanos": 6843,
  "lastEvalAircraft": 12,
  "fired": 1,
  "suppressed": 9,
  "states": 10,
  "webhookSent": 0,
  "webhookFailed": 0,
  "webhookDropped": 0,
  "subscribers": 2,
  "rangeBaselineKm": 0,
  "rangeRecentKm": 403.4
}
```

### `GET /api/alerts/placeholders`

Help text listing the message template placeholders (`{ident}`, `{alt}`, `{dist}` ...).

Response: { [key: string]: any JSON (see the example) }

Example: `GET /api/alerts/placeholders`

```jsonc
{
  "text": "{rule} {severity} {icao} {callsign} {reg} {type} {operator} {squawk} {alt} {dist} {ident} {event} {key} {value} {text} {watchlist} {note}"
}
```

### `POST /api/alerts/webhook-test`

Send a test POST to a webhook URL.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"url":"https://..."}`

Webhook payload: `{"app":"AirView","version":"...","type":"alert"|"test","alert":Alert}`, `Content-Type: application/json`, 5 s timeout, no retries, no redirects, at most 30 per minute.

Response: `{"ok":true}`; `400` invalid URL, `409` webhooks switched off (`config.alerts.webhooks`), `502` with the error when the POST failed.

Example: `POST /api/alerts/webhook-test` with body `{"url":"http://127.0.0.1:<port>/hook"}` (a local test receiver) -> `200`

```jsonc
{
  "ok": true
}
```

### `GET /api/alerts/export`

All rules as a JSON array (download).

Response: JSON array of Rule, `Content-Disposition: attachment`.

Example: `GET /api/alerts/export`

```jsonc
[
  {
    "id": "builtin-emergency",
    "name": "Emergency",
    "description": "Emergency squawk 7500/7600/7700 or an ADS-B emergency state",
    "enabled": true,
    "builtin": true,
    "severity": "critical",
    "match": {
      "op": "or",
      "conds": [
        {
          "type": "squawk",
          "values": ["7500", "7600", "7700"]
        },
        /* … 1 more */
      ]
    },
    "cooldownSec": 1800,
    "message": "{rule}: {ident} squawk {squawk} at {alt} ft, {dist} km",
    "actions": {
      "notify": true,
      "sound": true,
      "log": true,
      "highlight": "#ff3b30"
    }
  },
  /* … 11 more */
]
```

### `POST /api/alerts/import`

Import rules (all-or-nothing validation; rules with an existing id are replaced).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: JSON array of Rule (at most 4 MB)

Response: `{"imported":n}`

Example: `POST /api/alerts/import` with body `[{"name":"Imported: police helicopters","enabled":true,"severity":"info","match":{"op":"and","conds":[{"type":"category","values":["A7"]},{"type":"operator","values":["*POLIZEI*","*POLICE*"]}]},"cooldownSec":1800,"actions":{"notify":true,"highlight":"#bf5af2"}}]` -> `200`

```jsonc
{
  "imported": 1
}
```

### `GET /api/alerts/watchlists`

Watch-lists with their number of entries (`[{name, entries}]`).

Response: [alerts.WatchlistInfo](#t-alerts-WatchlistInfo)[]

Example: `GET /api/alerts/watchlists`

```jsonc
[]
```

### `GET /api/alerts/watchlists/{name}`

The entries of one watch-list, or a download.

| Parameter | Meaning |
|---|---|
| `format` | `json` (default: `[{icao, note, added}]`) \| `txt` \| `csv` (download) |

Response: JSON array of `{icao, note, added}` or a text / CSV file; `404` unknown list.

Example: `GET /api/alerts/watchlists/demo`

```jsonc
[
  {
    "icao": "3C6444",
    "note": "Lufthansa A319 (test entry)",
    "added": 1795821925000
  }
]
```

### `POST /api/alerts/watchlists/{name}/import`

Import ICAO addresses into a watch-list (created if needed).

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

| Parameter | Meaning |
|---|---|
| `replace` | `1` replaces the list instead of adding |

Request body: plain text or CSV (at most 16 MB): one address per line, optional note after a comma, semicolon or space; `#` comments

Response: `{"imported":n,"rejected":k}`

Example: `POST /api/alerts/watchlists/vips/import` with the text body `3C6444 Lufthansa A319` / `# comment` / `4CA2D1,Ryanair` / `not-hex` (four lines) -> `200`

```jsonc
{
  "imported": 2,
  "rejected": 1
}
```

### `PUT /api/alerts/watchlists/{name}/{icao}`

Add (or update the note of) one address.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Request body: `{"note":"..."}` (optional)

Response: `{"ok":true}`

Example: `PUT /api/alerts/watchlists/vips/400A0B` with body `{"note":"British Airways"}` -> `200`

```jsonc
{
  "ok": true
}
```

### `DELETE /api/alerts/watchlists/{name}/{icao}`

Remove one address.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`; `404` when not on the list.

Example: `DELETE /api/alerts/watchlists/vips/400A0B` -> `200`

```jsonc
{
  "ok": true
}
```

### `DELETE /api/alerts/watchlists/{name}`

Delete a watch-list.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.

Response: `{"ok":true}`; `404` when unknown.

Example: `DELETE /api/alerts/watchlists/vips` -> `200`

```jsonc
{
  "ok": true
}
```

## Replay, exports & reports

### `GET /api/replay`

History replay: time-sliced pictures of all stored positions in a window, in compact arrays (tar1090 / VRS style).

| Parameter | Meaning |
|---|---|
| `from`, `to` | window in Unix ms (default: the last hour; at most 24 h) |
| `hours` | alternative: the last N hours |
| `days` | alternative: the last N days |
| `step` | seconds between frames, 1..3600 (default: span/600, at least 5); at most 2000 frames |

`aircraft[]` lists each flight once (`icao`, `flightId`, `callsign`, `reg`, `type`, `operator`, `category` = ADS-B emitter category such as `A3` (omitted when unknown), `military`); every frame `p` row is `[aircraftIndex, lat, lon, altFt|null, gsKt|null, trackDeg|null, src, ageS]` - the newest stored point at or before the frame time if at most `staleSec` old (`ageS` = its age). Stored trails have one point per `config.storage.trailIntervalSec` and are kept `config.storage.trailRetentionDays`; in aggregator mode they keep full resolution for `aggregator.storage.trailFullDays` (2) days after the flight ended, then one point per `aggregator.storage.trailThinSec` (30 s), and `staleSec` is widened to at least trailThinSec + trailIntervalSec + 5 for windows starting before then. At most 400 000 points are read (`truncated`). Predicted positions are never stored, so they never appear here.

Server-side cache: 10s (identical requests share one computation).

Response: [server.replayReply](#t-server-replayReply)

Example: `GET /api/replay?hours=0.5&step=60`

```jsonc
{
  "from": 1795820125000,
  "to": 1795821925000,
  "step": 60,
  "staleSec": 120,
  "aircraft": [
    {
      "icao": "AC7378",
      "flightId": 2032,
      "callsign": "DAL1871",
      "reg": "N901FZ",
      "operator": "Delta Air Lines",
      "category": "A3"
    },
    /* … 27 more */
  ],
  "frames": [
    {
      "t": 1795820125000,
      "p": [
        [0, 47.86853, 3.24021, 37000, 391, 233, 0, 13],
        [1, 46.51081, 7.73177, 39000, 467, 181, 0, 16],
        /* … 9 more */
      ]
    },
    /* … 30 more */
  ],
  "points": 2507,
  "truncated": false
}
```

### `GET /api/history/flights/{id}/trace`

Full stored trail of one flight with track angles, and the guessed departure / arrival airports.

`points`: `[tMs, lat, lon, altFt|null, gsKt|null, trackDeg|null, rssi|null, src]`. `airports.departure` / `arrival` (`navdata.AirportGuess`: `airport`, `runway`, `distKm`, `heightAglFt`, `confidence` 0..1, `reason`) are null when the flight was not seen low near an airport or no airport data is imported. `404` unknown flight.

Response: [server.traceReply](#t-server-traceReply)

Example: `GET /api/history/flights/2057/trace`

```jsonc
{
  "flight": {
    "id": 2057,
    "icao": "4436CA",
    "squawk": "4477",
    "start": 1795821865000,
    "end": 1795821924000,
    "msgs": 111,
    "positions": 32,
    "srcMask": 2,
    "maxDistKm": 337.1916806412576,
    "maxDistBrg": 222.6602187321935,
    "minDistKm": 330.8205174620613,
    "minAlt": 41000,
    "maxAlt": 41000,
    "maxGs": 454.2510319195764,
    "rssiMin": -35.1,
    "rssiMax": -30.4,
    "rssiAvg": -32.88333333333334,
    "firstLat": 47.77880859375,
    "firstLon": 5.551048278808594,
    "lastLat": 47.76380635924258,
    "lastLon": 5.71148212139423,
    "country": "Austria"
  },
  "points": [
    [1795821868000, 47.77881, 5.55105, 41000, 447, 98, null, 1],
    [1795821873000, 47.77826, 5.56430, 41000, 446, 98, null, 1],
    /* … 8 more */
  ],
  "airports": {
    "departure": null,
    "arrival": null
  }
}
```

### `GET /api/export/flight/{file}`

Download one flight: `{id}.kml` (Google Earth, absolute altitudes), `{id}.geojson` (LineString [lon, lat, altM] with `properties.times`) or `{id}.csv`.

Response: `application/vnd.google-earth.kml+xml`, `application/geo+json` or `text/csv` with `Content-Disposition: attachment`; `404` unknown flight or format.

Example: `GET /api/export/flight/2057.geojson`

```jsonc
{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "geometry": {
        "type": "LineString",
        "coordinates": [
          [5.55105, 47.77881, 12497],
          [5.5643, 47.77826, 12497],
          /* … 8 more */
        ]
      },
      "properties": {
        "callsign": "",
        "end": 1795821924000,
        "flightId": 2057,
        "icao": "4436CA",
        "operator": "",
        "positions": 10,
        "reg": "",
        "start": 1795821865000,
        "times": [1795821868000, 1795821873000, 1795821878000, 1795821885000, 1795821890000, 1795821895000, 1795821901000, 1795821909000, /* … 2 more */],
        "type": ""
      }
    }
  ]
}
```

### `GET /api/export/live.kml`

The current picture with trails as KML.

| Parameter | Meaning |
|---|---|
| `minutes` | trail length 0..240 (default `config.ui.trailMinutes`; 0 = no trails) |

Response: KML document: one folder per aircraft with a point (absolute altitude) and its trail. Coasting aircraft are exported at their last real position.

Example: `GET /api/export/live.kml?minutes=5` (shortened)

```xml
<?xml version="1.0" encoding="UTF-8"?>
<kml xmlns="http://www.opengis.net/kml/2.2">
<Document>
<name>AirView live 20261127-232525</name>
<Style id="ac">
<IconStyle>
<scale>0.8</scale>
</IconStyle>
</Style>
<Style id="trk">
<LineStyle>
<color>ccffb000</color>
<width>2</width>
</LineStyle>
</Style>
<Folder>
<name>DLH81AS (3C473C)</name>
<Placemark>
<name>DLH81AS (3C473C)</name>
<description>32000 ft, 413 kt</description>
<styleUrl>#ac</styleUrl>
<Point>
<altitudeMode>absolute</altitudeMode>
<coordinates>4.81773,51.68476,9754</coordinates>
</Point>
</Placemark>
<Placemark>
<name>trail</name>
<styleUrl>#trk</styleUrl>
<LineString>
<tessellate>1</tessellate>
<altitudeMode>absolute</altitudeMode>
<coordinates>4.99306,51.65762,9754 4.98283,51.65920,9754 4.97330,51.66069,9754 4.96254,51.66237,9754 4.94698,51.66479,9754 4.93680,51.66637,9754 4.92484,51.66820,9754 4.91527,51.66966,9754 4.90395,51.67145,9754 4.89197,51.67331,975 ...
```

### `GET /api/export/live.geojson`

The current picture with trails as GeoJSON.

| Parameter | Meaning |
|---|---|
| `minutes` | trail length 0..240 (default `config.ui.trailMinutes`; 0 = no trails) |

Response: FeatureCollection: `kind=aircraft` points (icao, callsign, reg, type, operator, altFt, gsKt, track, source, predicted, lastSeen) and `kind=trail` LineStrings ([lon, lat, altM], `times`).

Example: `GET /api/export/live.geojson?minutes=5`

```jsonc
{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "geometry": {
        "type": "Point",
        "coordinates": [4.81773, 51.68476]
      },
      "properties": {
        "altFt": 32000,
        "callsign": "DLH81AS",
        "gsKt": 413,
        "icao": "3C473C",
        "kind": "aircraft",
        "lastSeen": 1795821925277,
        "operator": "Lufthansa",
        "predicted": false,
        "reg": "",
        "source": "adsb",
        "track": 284,
        "type": ""
      }
    },
    /* … 23 more */
  ]
}
```

### `GET /api/export/aircraft.csv`

Every airframe ever seen as CSV (streamed).

Response: `text/csv`: icao, reg, type, type_desc, operator, country, category, military, first_seen, last_seen (RFC 3339 UTC), msgs, flights, positions, max_dist_km, max_alt_ft, year.

Example: `GET /api/export/aircraft.csv`

```
icao,reg,type,type_desc,operator,country,category,military,first_seen,last_seen,msgs,flights,positions,max_dist_km,max_alt_ft,year
06A021,,,,Qatar Airways,Qatar,A5,false,2026-11-27T05:34:28Z,2026-11-27T06:39:42Z,29952,1,6624,392.3,31000,
06A023,,,,Qatar Airways,Qatar,A5,false,2026-11-27T16:12:17Z,2026-11-27T16:56:03Z,18944,1,4189,421.7,32000,
...
```

### `GET /api/reports`

Report query (VirtualRadar style): flights in a time range filtered by callsign, registration, address, type, operator, country, military, distance, altitude, departure / arrival airport; paged and sorted.

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms: flights that overlap it (default: the last 24 h) |
| `hours` | alternative: the last N hours |
| `days` | alternative: the last N days |
| `callsign` | callsign, `*` / `?` wildcards (e.g. `DLH*`) |
| `reg` | registration, wildcards allowed |
| `icao` | ICAO address (6 hex digits) or a hex pattern with wildcards (`3C*`) |
| `type` | ICAO type designator, wildcards allowed (`A32?`) |
| `operator` | operator: substring, or wildcards |
| `country` | country of registration (name): substring, or wildcards |
| `squawk` | Mode A code, wildcards allowed (`77*`) |
| `military` | `1` only military, `0` no military |
| `emergency` | `1` only flights that declared an emergency |
| `minDistKm` | farthest distance of the flight at least this |
| `maxDistKm` | closest distance of the flight at most this |
| `minAlt` | highest altitude of the flight at least this (ft) |
| `maxAlt` | lowest altitude of the flight at most this (ft) |
| `minPositions` | at least this many positions |
| `dep` | departure airport: exact ICAO or IATA code, case-insensitive (`EDDF`, `fra`) |
| `arr` | arrival airport: exact ICAO or IATA code, case-insensitive |
| `sort` | `start` (default) \| `end` \| `callsign` \| `icao` \| `reg` \| `type` \| `operator` \| `country` \| `maxDistKm` \| `minDistKm` \| `maxAlt` \| `minAlt` \| `maxGs` \| `msgs` \| `positions` \| `duration` \| `rssiMax` \| `dep` \| `arr` |
| `desc` | `1` / `0` (default: descending for times and numbers, ascending for text) |
| `limit` | 1..1000 (default 100) |
| `offset` | rows to skip |

`total` counts all matching flights. Distances km, altitudes ft, speeds kt, signal dBFS, times Unix ms; unknown values null. Reference data comes from the aircraft table and the imported aircraft database. `dep` / `arr` are the departure / arrival airport codes (ICAO, else GPS code / ident) the engine stores once a flight is closed (about 35 min after its last message), guessed from the first / last low positions near an airport of the imported airport data; they are absent while the flight is open, not resolved yet (older flights are back-filled slowly in the background) or when no airport fits.

Response: [server.reportReply](#t-server-reportReply)

Example: `GET /api/reports?hours=6&limit=3`

```jsonc
{
  "from": 1795800325670,
  "to": 1795821925670,
  "total": 190,
  "limit": 3,
  "offset": 0,
  "sort": "start",
  "desc": true,
  "rows": [
    {
      "flightId": 2058,
      "icao": "44A0DF",
      "callsign": "BEL902",
      "squawk": "3706",
      "operator": "Brussels Airlines",
      "country": "Belgium",
      "category": "A3",
      "start": 1795821867000,
      "end": 1795821924000,
      "durationS": 57,
      "msgs": 46,
      "positions": 4,
      "srcMask": 1,
      "minDistKm": 400.59,
      "maxDistKm": 403.66,
      "maxDistBrg": 137.4,
      "minAlt": 33000,
      "maxAlt": 33000,
      "maxGs": 438,
      "rssiMax": -31.3,
      "rssiAvg": -33.7,
      "firstLat": 47.31766,
      "firstLon": 12.21096,
      "lastLat": 47.33362,
      "lastLon": 12.17567,
      "dep": "",
      "arr": ""
    },
    /* … 2 more */
  ]
}
```

### `GET /api/reports.csv`

The same report as a CSV download (streamed).

| Parameter | Meaning |
|---|---|
| `from`, `to` | time range in Unix ms: flights that overlap it (default: the last 24 h) |
| `hours` | alternative: the last N hours |
| `days` | alternative: the last N days |
| `callsign` | callsign, `*` / `?` wildcards (e.g. `DLH*`) |
| `reg` | registration, wildcards allowed |
| `icao` | ICAO address (6 hex digits) or a hex pattern with wildcards (`3C*`) |
| `type` | ICAO type designator, wildcards allowed (`A32?`) |
| `operator` | operator: substring, or wildcards |
| `country` | country of registration (name): substring, or wildcards |
| `squawk` | Mode A code, wildcards allowed (`77*`) |
| `military` | `1` only military, `0` no military |
| `emergency` | `1` only flights that declared an emergency |
| `minDistKm` | farthest distance of the flight at least this |
| `maxDistKm` | closest distance of the flight at most this |
| `minAlt` | highest altitude of the flight at least this (ft) |
| `maxAlt` | lowest altitude of the flight at most this (ft) |
| `minPositions` | at least this many positions |
| `dep` | departure airport: exact ICAO or IATA code, case-insensitive (`EDDF`, `fra`) |
| `arr` | arrival airport: exact ICAO or IATA code, case-insensitive |
| `sort` | `start` (default) \| `end` \| `callsign` \| `icao` \| `reg` \| `type` \| `operator` \| `country` \| `maxDistKm` \| `minDistKm` \| `maxAlt` \| `minAlt` \| `maxGs` \| `msgs` \| `positions` \| `duration` \| `rssiMax` \| `dep` \| `arr` |
| `desc` | `1` / `0` (default: descending for times and numbers, ascending for text) |
| `limit` | 1..1000000 (default 100000) |

Response: `text/csv` with the columns of `/api/reports` rows (times RFC 3339 UTC), ending with `dep`, `arr`.

Example: `GET /api/reports.csv?hours=6&limit=5`

```
flight_id,icao,callsign,squawk,reg,type,type_desc,operator,country,category,military,start,end,duration_s,msgs,positions,min_dist_km,max_dist_km,max_dist_brg,min_alt_ft,max_alt_ft,max_gs_kt,rssi_max,rssi_avg,first_lat,first_lon,last_lat,last_lon,emergency,dep,arr
2058,44A0DF,BEL902,3706,,,,Brussels Airlines,Belgium,A3,false,2026-11-27T23:24:27Z,2026-11-27T23:25:24Z,57,46,4,400.59,403.66,137.4,33000,33000,438,-31.3,-33.7,47.31766,12.21096,47.33362,12.17567,,,
2057,4436CA,,4477,,,,,Austria,,false,2026-11-27T23:24:25Z,2026-11-27T23:25:24Z,59,111,32,330.82,337.19,222.7,41000,41000,454,-30.4,-32.9,47.77881,5.55105,47.76381,5.71148,,,
...
```

## Uplinks

### `GET /api/uplinks`

Uplinks of this station to aggregators: feeder ID, UUID and per-uplink state (no secrets).

`503` when the uplink manager is not running.


### `POST /api/uplinks`

Uplink actions: `{"action":"claim","id":"...","rxNo":0}` returns a claim code (shown once), `{"action":"retry","id":"..."}` restarts a stopped uplink.

**Admin endpoint**: `401 {"error":"admin password required"}` for a non-loopback client that is not logged in while a password is set; same-origin only.


## Type reference

Field tables of the response types named above, in alphabetical order (`package.Type` as in the Go source).
The *Notes* column is the comment of the field in the source. A type written `T | null` can be null; fields
marked *optional* are left out when empty / unknown. `model.Config`, `model.Status`, `model.Aircraft` and
`model.LiveSnapshot` are the shared contract types of `internal/model`.

<a id="t-acas-Stats"></a>

#### acas.Stats

Stats are the recorder's counters.

| Field | Type | Notes |
|---|---|---|
| `events` | integer | events started since the start |
| `open` | integer | events in progress |
| `dropped` | integer | reports dropped by the flood guard |

<a id="t-alerts-Actions"></a>

#### alerts.Actions

Actions say what happens when a rule fires.

| Field | Type | Notes |
|---|---|---|
| `notify` | boolean | UI toast |
| `sound` | boolean | UI plays a sound with the toast |
| `log` | boolean | store event (kind "alert") |
| `webhook` | string | *optional* - POST JSON to this http(s) URL |
| `highlight` | string | *optional* - map highlight colour "#rrggbb" while matching |

<a id="t-alerts-Alert"></a>

#### alerts.Alert

Alert is one fired alert (SSE "alert" event, history, webhook payload).

| Field | Type | Notes |
|---|---|---|
| `id` | integer |  |
| `t` | integer | Unix ms |
| `ruleId` | string |  |
| `ruleName` | string |  |
| `severity` | string |  |
| `kind` | string | "aircraft" \| "system" |
| `event` | string | *optional* - system event kind |
| `eventKey` | string | *optional* |
| `icao` | string | *optional* |
| `callsign` | string | *optional* |
| `reg` | string | *optional* |
| `type` | string | *optional* |
| `operator` | string | *optional* |
| `squawk` | string | *optional* |
| `lat` | number | *optional* |
| `lon` | number | *optional* |
| `altFt` | number | *optional* |
| `distKm` | number | *optional* |
| `gsKt` | number | *optional* - ground speed |
| `vrFpm` | number | *optional* - vertical rate, ft/min |
| `trackDeg` | number | *optional* - true track |
| `brgDeg` | number | *optional* - bearing from the station |
| `rssi` | number | *optional* - dBFS |
| `value` | number | *optional* - system event value (e.g. record km), in Unit |
| `unit` | string | *optional* - Unit is the unit of Value and RefValue: "km", "ft", "kt", "dBFS", "msg/s", "s" or "" (a count). |
| `refValue` | number | *optional* - comparison value (range collapse: the typical range) |
| `text` | string |  |
| `auto` | boolean | *optional* - Auto is true when Text comes from the built-in default template (the rule has no message of its own). Its altitude / distance / record value are then in fixed server units (ft, km, the record key's unit), and a client may rebuild the text from the structured fields in its own units. |
| `notify` | boolean |  |
| `sound` | boolean |  |
| `highlight` | string | *optional* |
| `webhook` | boolean | *optional* - a webhook POST was queued |
| `logged` | boolean | *optional* |

<a id="t-alerts-Cond"></a>

#### alerts.Cond

Cond is one node of a rule's condition tree: either a group (Op with Conds) or a leaf (Type with its parameters). Negate inverts any node.

| Field | Type | Notes |
|---|---|---|
| `op` | string | *optional* - "and" \| "or" (groups only) |
| `conds` | [alerts.Cond](#t-alerts-Cond)[] | *optional* |
| `negate` | boolean | *optional* |
| `type` | string | *optional* |
| `values` | string[] | *optional* |
| `min` | number | *optional* |
| `max` | number | *optional* |
| `value` | boolean | *optional* |
| `lat` | number | *optional* |
| `lon` | number | *optional* |
| `radiusKm` | number | *optional* |
| `polygon` | number[][] | *optional* - [lon, lat] (GeoJSON order), open or closed ring |
| `inside` | boolean | *optional* |
| `from` | number | *optional* |
| `to` | number | *optional* |

<a id="t-alerts-Highlight"></a>

#### alerts.Highlight

Highlight is a map highlight for an aircraft currently matching a rule with a highlight colour.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `color` | string |  |
| `ruleId` | string |  |
| `severity` | string |  |
| `until` | integer | Unix ms |

<a id="t-alerts-Rule"></a>

#### alerts.Rule

Rule is one alert rule (the JSON stored and exchanged with the UI).

| Field | Type | Notes |
|---|---|---|
| `id` | string |  |
| `name` | string |  |
| `description` | string | *optional* |
| `enabled` | boolean |  |
| `builtin` | boolean | *optional* - shipped default (may be edited/disabled; ResetDefaults restores) |
| `severity` | string |  |
| `match` | [alerts.Cond](#t-alerts-Cond) |  |
| `cooldownSec` | integer | per aircraft (or per system event key); default 900 |
| `ruleCooldownSec` | integer | *optional* - minimum gap between any two alerts of this rule |
| `message` | string | *optional* - template, see Expand |
| `actions` | [alerts.Actions](#t-alerts-Actions) |  |
| `includePredicted` | boolean | *optional* - IncludePredicted lets the position-dependent conditions (altitude, distance, geofence, bearing) match aircraft shown at a predicted (coasting) position. Default false: such a rule ignores those aircraft until a real position is received again. |
| `created` | integer | *optional* - Unix ms |
| `updated` | integer | *optional* |

<a id="t-alerts-Stats"></a>

#### alerts.Stats

Stats are engine counters.

| Field | Type | Notes |
|---|---|---|
| `rules` | integer |  |
| `enabledRules` | integer |  |
| `evaluations` | integer |  |
| `lastEvalMicros` | integer |  |
| `lastEvalNanos` | integer | same, in ns (the rule pass usually takes < 1 µs per aircraft) |
| `lastEvalAircraft` | integer |  |
| `fired` | integer |  |
| `suppressed` | integer | cooldown / storm guard |
| `states` | integer |  |
| `webhookSent` | integer |  |
| `webhookFailed` | integer |  |
| `webhookDropped` | integer |  |
| `lastWebhookError` | string | *optional* |
| `subscribers` | integer |  |
| `rangeBaselineKm` | number |  |
| `rangeRecentKm` | number |  |

<a id="t-alerts-WatchlistInfo"></a>

#### alerts.WatchlistInfo

WatchlistInfo summarises one list.

| Field | Type | Notes |
|---|---|---|
| `name` | string |  |
| `entries` | integer |  |

<a id="t-analytics-AllTime"></a>

#### analytics.AllTime

AllTime are lifetime totals of the database.

| Field | Type | Notes |
|---|---|---|
| `airframes` | integer |  |
| `flights` | integer |  |
| `msgs` | integer |  |
| `positions` | integer |  |
| `since` | integer | first sighting, Unix ms; 0 = empty database |
| `maxKm` | number \| null |  |
| `countries` | integer |  |

<a id="t-analytics-AltBandCount"></a>

#### analytics.AltBandCount

AltBandCount is the number of positions received in one altitude band.

| Field | Type | Notes |
|---|---|---|
| `band` | integer |  |
| `label` | string |  |
| `positions` | integer |  |
| `pct` | number \| null |  |
| `maxKm` | number \| null |  |

<a id="t-analytics-AltitudeDistribution"></a>

#### analytics.AltitudeDistribution

AltitudeDistribution is the result of Analyzer.AltitudeDistribution.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `bands` | [analytics.AltBandCount](#t-analytics-AltBandCount)[] | positions per altitude band (coverage table) |
| `binFt` | integer | Histogram of the maximum altitude of the flights in range. |
| `flightAlt` | integer[] | count per BinFt bin starting at 0 ft |
| `flightsN` | integer |  |
| `medianFt` | number \| null | median flight maximum altitude |
| `lowestFt` | number \| null | lowest altitude seen on any flight |
| `highestFt` | number \| null |  |
| `cruiseBand` | string | label of the busiest band |

<a id="t-analytics-AntennaModel"></a>

#### analytics.AntennaModel

AntennaModel is one configured antenna: its modelled patterns and, when antenna-specific statistics exist (one feed per antenna), its measured azimuth pattern.

| Field | Type | Notes |
|---|---|---|
| `id` | string |  |
| `name` | string |  |
| `type` | string |  |
| `color` | string | *optional* |
| `info` | [radio.PatternInfo](#t-radio-PatternInfo) |  |
| `azGainDbi` | (number \| null)[] | AzGainDBi is the modelled gain per bearing bin at 3 degrees elevation; AzRelDB the same relative to its peak. |
| `azRelDb` | (number \| null)[] |  |
| `elGainDbi` | (number \| null)[] | ElGainDBi is the modelled gain per elevation bin (on boresight for a directional antenna); ElRelDB relative to its peak. |
| `elRelDb` | (number \| null)[] |  |
| `measured` | [analytics.AzimuthPattern](#t-analytics-AzimuthPattern) | *optional* |
| `compare` | [analytics.PatternCompare](#t-analytics-PatternCompare) | *optional* |
| `fit` | [analytics.YagiFit](#t-analytics-YagiFit) | *optional* - directional antennas with their own data |

<a id="t-analytics-AntennaReport"></a>

#### analytics.AntennaReport

AntennaReport is the result of Analyzer.Antenna.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean |  |
| `signal` | [analytics.SignalDistance](#t-analytics-SignalDistance) \| null | Signal is the RSSI-vs-distance histogram with the path-loss fit. |
| `azimuth` | [analytics.AzimuthPattern](#t-analytics-AzimuthPattern) \| null | Azimuth is the measured azimuth gain pattern of the selected antenna ("" = everything the receiver heard); nil until there is enough data. |
| `elevation` | [analytics.ElevationPattern](#t-analytics-ElevationPattern) \| null |  |
| `antennas` | [analytics.AntennaModel](#t-analytics-AntennaModel)[] | Antennas are the configured antennas with their modelled patterns. |
| `yagi` | [analytics.YagiFit](#t-analytics-YagiFit) \| null | Yagi is the directional lobe inferred from Azimuth. |
| `linkBudget` | [analytics.LinkRow](#t-analytics-LinkRow)[] |  |
| `notes` | string[] |  |

<a id="t-analytics-AzimuthPattern"></a>

#### analytics.AzimuthPattern

AzimuthPattern is the measured azimuth gain pattern.

| Field | Type | Notes |
|---|---|---|
| `method` | string | Method: "heat-residual" (mean of rssi + 20*log10(slant) per bearing over the all-time heat grid, 30-300 km, elevation 0.3-12 degrees, with the common distance trend removed) or "coverage-rssi" (mean RSSI per bearing at 10-40 kft relative to the all-round mean; used while the receiver position is unknown; less accurate). |
| `bearingDeg` | (number \| null)[] |  |
| `relDb` | (number \| null)[] | smoothed, relative to the all-round median; null = no data |
| `rawDb` | (number \| null)[] | unsmoothed |
| `cells` | integer[] | samples (heat cells / coverage rows) per bearing |
| `bearings` | integer |  |
| `samples` | integer |  |
| `peakDb` | number \| null |  |
| `peakBrg` | number \| null |  |
| `minDb` | number \| null |  |
| `minBrg` | number \| null |  |
| `spreadDb` | number \| null | 95th - 5th percentile over bearings |
| `distSlopeDb` | number \| null | DistSlopeDB is the residual distance trend that was removed, dB per decade (0 = free-space law holds exactly). |

<a id="t-analytics-BusiestHour"></a>

#### analytics.BusiestHour

BusiestHour is the hour with the most aircraft.

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `aircraft` | integer |  |
| `frames` | integer |  |

<a id="t-analytics-ConfiguredYagi"></a>

#### analytics.ConfiguredYagi

ConfiguredYagi is the directional antenna from the configuration that the measured lobe is compared with.

| Field | Type | Notes |
|---|---|---|
| `antennaId` | string |  |
| `name` | string |  |
| `azimuthDeg` | number |  |
| `hBeamwidthDeg` | number |  |
| `gainOverOmniDb` | number \| null | modelled advantage over the omni at low elevation, net of feed losses |

<a id="t-analytics-Contrast"></a>

#### analytics.Contrast

Contrast is a two-sample comparison (Welch t-test) of the traffic-normalised range.

| Field | Type | Notes |
|---|---|---|
| `nA` | integer |  |
| `nB` | integer |  |
| `diffKm` | number \| null | mean(B) - mean(A), traffic-normalised |
| `rawDiffKm` | number \| null | without normalisation |
| `diffResidDb` | number \| null |  |
| `t` | number \| null |  |
| `p` | number \| null |  |
| `significant` | boolean | p < 0.01 with at least 24 hours on each side |

<a id="t-analytics-CostSurface"></a>

#### analytics.CostSurface

CostSurface is a coarse grid of one method's cost function around the fused estimate, for a map visualisation. Cost[row*Cols+col] belongs to latitude South + row*DLat and longitude West + col*DLon and is normalised to 0 (best) .. 1 (worst on the grid).

| Field | Type | Notes |
|---|---|---|
| `method` | string |  |
| `south` | number |  |
| `west` | number |  |
| `dLat` | number |  |
| `dLon` | number |  |
| `rows` | integer |  |
| `cols` | integer |  |
| `cost` | (number \| null)[] |  |
| `minLat` | number | grid node with the lowest cost |
| `minLon` | number |  |

<a id="t-analytics-Coverage"></a>

#### analytics.Coverage

Coverage is the result of Analyzer.Coverage.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `days` | integer | days with data |
| `positions` | integer |  |
| `located` | boolean |  |
| `receiver` | [analytics.LatLon](#t-analytics-LatLon) | *optional* |
| `antennaAmslM` | number |  |
| `binDeg` | number |  |
| `bearingDeg` | (number \| null)[] | bin centres |
| `overall` | [analytics.PolarBand](#t-analytics-PolarBand) |  |
| `bands` | [analytics.PolarBand](#t-analytics-PolarBand)[] |  |
| `skyline` | [analytics.Skyline](#t-analytics-Skyline) |  |
| `obstructions` | [analytics.Obstruction](#t-analytics-Obstruction)[] |  |

<a id="t-analytics-Daily"></a>

#### analytics.Daily

Daily is the result of Analyzer.Daily.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `utcOffsetSec` | integer |  |
| `days` | [analytics.DayTotals](#t-analytics-DayTotals)[] |  |
| `busiestDay` | [analytics.DayTotals](#t-analytics-DayTotals) | *optional* - most unique aircraft (frames when no flights) |
| `busiestHour` | [analytics.BusiestHour](#t-analytics-BusiestHour) | *optional* - most aircraft in view |

<a id="t-analytics-DayNight"></a>

#### analytics.DayNight

DayNight is the result of Analyzer.DayNight.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean | sun phases need the receiver position |
| `utcOffsetSec` | integer |  |
| `hours` | integer |  |
| `phases` | [analytics.PhaseStats](#t-analytics-PhaseStats)[] |  |
| `byHour` | [analytics.PhaseStats](#t-analytics-PhaseStats)[] |  |
| `trafficFit` | [analytics.TrafficFit](#t-analytics-TrafficFit) | *optional* |
| `nightVsDay` | [analytics.Contrast](#t-analytics-Contrast) | *optional* - NightVsDay compares night against day (positive = better at night). |
| `conclusions` | string[] |  |

<a id="t-analytics-DayTotals"></a>

#### analytics.DayTotals

DayTotals is one local calendar day.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | local midnight, Unix ms |
| `date` | string | YYYY-MM-DD (local) |
| `frames` | integer |  |
| `positions` | integer |  |
| `peakAircraft` | integer |  |
| `uniqueAircraft` | integer | distinct airframes that started a flight |
| `flights` | integer |  |
| `newAirframes` | integer | never seen before that day |
| `maxKm` | number \| null |  |
| `uptimeMin` | integer |  |
| `busiestHour` | [analytics.BusiestHour](#t-analytics-BusiestHour) \| null | BusiestHour is the hour of this day with the most aircraft in view (null when no hour had aircraft). |
| `farthest` | [analytics.FarthestFlight](#t-analytics-FarthestFlight) \| null | Farthest is the flight that started this day with the largest maximum distance (null when no flight had a position). |

<a id="t-analytics-DFCount"></a>

#### analytics.DFCount

DFCount is the share of one downlink format.

| Field | Type | Notes |
|---|---|---|
| `df` | string | "DF17", ..., "other" |
| `name` | string |  |
| `count` | integer |  |
| `pct` | number \| null |  |

<a id="t-analytics-DuctEpisode"></a>

#### analytics.DuctEpisode

DuctEpisode is a run of hours with a duct flag or anomalously long range.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `hours` | integer |  |
| `peakP95Km` | number \| null |  |
| `peakMaxKm` | number \| null |  |
| `peakZ` | number \| null | robust z-score of the range anomaly |
| `ductFlag` | boolean | weather analysis flagged ducting |
| `anomalous` | boolean |  |
| `minDnDh` | number \| null |  |
| `reason` | string |  |

<a id="t-analytics-ElevationPattern"></a>

#### analytics.ElevationPattern

ElevationPattern is the measured elevation pattern from elev_hist.

| Field | Type | Notes |
|---|---|---|
| `elevDeg` | (number \| null)[] | bin centres, 0.5 degree bins from -2 |
| `count` | integer[] |  |
| `rssi` | (number \| null)[] | mean dBFS |
| `residDb` | (number \| null)[] | mean of rssi + 20*log10(slant) |
| `relDb` | (number \| null)[] | RelDB is the smoothed residual relative to its maximum: the measured elevation gain pattern (null where there are too few samples). Near the horizon it reads high because only strong signals are decoded. |
| `total` | integer |  |
| `peakDeg` | number \| null |  |
| `lowestDeg` | number \| null | LowestDeg is the lowest elevation with regular reception (0.5 % of the samples lie below). |

<a id="t-analytics-FarthestFlight"></a>

#### analytics.FarthestFlight

FarthestFlight is the flight with the largest maximum distance of a day.

| Field | Type | Notes |
|---|---|---|
| `flightId` | integer | flights.id (history / replay endpoints) |
| `icao` | string | hex, e.g. "3C6444" |
| `callsign` | string | "" when unknown |
| `km` | number \| null | maximum distance from the receiver |
| `t` | integer | flight start, Unix ms |

<a id="t-analytics-Feature"></a>

#### analytics.Feature

Feature is a GeoJSON Polygon feature.

| Field | Type | Notes |
|---|---|---|
| `type` | string |  |
| `properties` | { [key: string]: any JSON (see the example) } |  |
| `geometry` | [analytics.Geometry](#t-analytics-Geometry) |  |

<a id="t-analytics-FeatureCollection"></a>

#### analytics.FeatureCollection

FeatureCollection is a GeoJSON FeatureCollection of Polygons.

| Field | Type | Notes |
|---|---|---|
| `type` | string |  |
| `features` | [analytics.Feature](#t-analytics-Feature)[] |  |

<a id="t-analytics-Finding"></a>

#### analytics.Finding

Finding is a plain-language observation or piece of advice.

| Field | Type | Notes |
|---|---|---|
| `level` | string | "good" \| "info" \| "warn" |
| `code` | string | stable identifier, e.g. "obstruction", "yagi-pointing" |
| `text` | string |  |

<a id="t-analytics-Geometry"></a>

#### analytics.Geometry

Geometry is a GeoJSON Polygon; rings are closed [lon, lat] lists.

| Field | Type | Notes |
|---|---|---|
| `type` | string |  |
| `coordinates` | number[][][] |  |

<a id="t-analytics-Guess"></a>

#### analytics.Guess

Guess is the result of Analyzer.GuessLocation.

| Field | Type | Notes |
|---|---|---|
| `ant` | string |  |
| `cells` | integer | heat cells available |
| `positions` | integer |  |
| `ok` | boolean |  |
| `reason` | string | *optional* |
| `estimates` | [analytics.GuessEstimate](#t-analytics-GuessEstimate)[] |  |
| `fused` | [analytics.GuessEstimate](#t-analytics-GuessEstimate) | *optional* |
| `located` | boolean |  |
| `configured` | [analytics.LatLon](#t-analytics-LatLon) | *optional* |
| `formatted` | string | *optional* - fused position, decimal degrees |
| `maidenhead` | string | *optional* - 6-character locator |
| `surface` | [analytics.CostSurface](#t-analytics-CostSurface) | *optional* |
| `notes` | string[] |  |
| `locate2` | [analytics.Locate2Result](#t-analytics-Locate2Result) | *optional* - Locate2 is the full Locate Me v2 result when it produced the fused estimate (terms, levels, error against the receiver's GNSS position). |

<a id="t-analytics-GuessEstimate"></a>

#### analytics.GuessEstimate

GuessEstimate is one estimate of the receiver position.

| Field | Type | Notes |
|---|---|---|
| `method` | string | Method: "centroid" \| "signal" \| "floor" \| "symmetry" \| "fused". |
| `label` | string |  |
| `ok` | boolean |  |
| `reason` | string | *optional* |
| `lat` | number |  |
| `lon` | number |  |
| `uncertaintyKm` | number \| null | UncertaintyKm is a heuristic 1-sigma radius (from the sharpness of the method's cost minimum or the scatter of its input). |
| `errorKm` | number \| null | ErrorKm is the distance to the configured receiver position (null when none is configured). |
| `weight` | number \| null | share in the fused estimate, 0..1 |
| `r95Km` | number | *optional* - R95Km is the radius of the 95 % accuracy circle (Locate Me v2 and the fused estimate it makes; null for the classic heuristic methods). |
| `detail` | string |  |

<a id="t-analytics-HeatGrid"></a>

#### analytics.HeatGrid

HeatGrid is the all-time position grid as compact parallel arrays.

| Field | Type | Notes |
|---|---|---|
| `ant` | string |  |
| `cellDeg` | number | size of the exported cells (0.05 x Step) |
| `step` | integer |  |
| `cells` | integer |  |
| `totalCells` | integer | cells in the table for this antenna |
| `positions` | integer |  |
| `maxCount` | integer |  |
| `lat` | number[] | cell centres |
| `lon` | number[] |  |
| `count` | integer[] |  |
| `rssi` | (number \| null)[] | mean dBFS |
| `minAlt` | (number \| null)[] | lowest altitude seen, ft = coverage floor |
| `maxAlt` | (number \| null)[] |  |

<a id="t-analytics-L2Level"></a>

#### analytics.L2Level

L2Level is one level of the grid search.

| Field | Type | Notes |
|---|---|---|
| `stepKm` | number |  |
| `nodes` | integer |  |
| `halfKm` | number | half width of the searched box |
| `horizon` | integer |  |
| `signal` | integer |  |
| `endpoints` | integer |  |

<a id="t-analytics-L2Term"></a>

#### analytics.L2Term

L2Term describes one likelihood term of a Locate Me v2 result.

| Field | Type | Notes |
|---|---|---|
| `name` | string | horizon, endpoints, signal, timing |
| `used` | boolean |  |
| `n` | integer | samples / sessions / aircraft it used at the final level |
| `detail` | string |  |
| `deltaLL` | number \| null | DeltaLL is the term's log-likelihood at the estimate minus its value at the search centre: how strongly it pulled the estimate. |

<a id="t-analytics-LatLon"></a>

#### analytics.LatLon

LatLon is a WGS84 position.

| Field | Type | Notes |
|---|---|---|
| `lat` | number |  |
| `lon` | number |  |

<a id="t-analytics-LinkRow"></a>

#### analytics.LinkRow

LinkRow compares the predicted and the achieved range in one altitude band.

| Field | Type | Notes |
|---|---|---|
| `band` | integer |  |
| `label` | string |  |
| `altFt` | number |  |
| `horizonKm` | number \| null | smooth-earth line of sight at AltFt |
| `predictedKm` | number \| null | link-budget maximum range (best antenna, on boresight) |
| `limit` | string | "horizon" \| "power" \| "none" |
| `marginDb` | number \| null |  |
| `antenna` | string | antenna giving the prediction |
| `achievedMaxKm` | number \| null | Achieved: the absolute maximum, the best bearing of the robust envelope, and the median over all bearings of the robust envelope. |
| `achievedRobustKm` | number \| null |  |
| `achievedTypicalKm` | number \| null |  |
| `pctOfPredicted` | number \| null | AchievedRobustKm / PredictedKm |

<a id="t-analytics-Locate2Result"></a>

#### analytics.Locate2Result

Locate2Result is a Locate Me v2 position estimate.

| Field | Type | Notes |
|---|---|---|
| `ok` | boolean |  |
| `reason` | string | *optional* |
| `lat` | number | posterior mean |
| `lon` | number |  |
| `mapLat` | number | highest posterior node |
| `mapLon` | number |  |
| `sigmaKm` | number \| null | SigmaKm is the 1-sigma radial uncertainty sqrt((aÂ²+bÂ²)/2) of the posterior; R95Km the radius of the accuracy circle (95 % of the posterior mass for its major axis); MajorKm/MinorKm/AxisDeg the 1-sigma ellipse (axis = bearing of the major axis). |
| `r95Km` | number \| null |  |
| `majorKm` | number \| null |  |
| `minorKm` | number \| null |  |
| `axisDeg` | number \| null |  |
| `antennaM` | number \| null | AntennaM is the antenna height that explains the data best: above ground with terrain data, above the surrounding terrain without. |
| `samples` | integer |  |
| `sessions` | integer |  |
| `aircraft` | integer |  |
| `terrain` | boolean |  |
| `source` | string | *optional* - Source names where the samples came from ("guess_samples" or "positions"); set by the Analyzer. |
| `terms` | [analytics.L2Term](#t-analytics-L2Term)[] |  |
| `levels` | [analytics.L2Level](#t-analytics-L2Level)[] |  |
| `surface` | [analytics.CostSurface](#t-analytics-CostSurface) | *optional* |
| `elapsedMs` | integer |  |
| `notes` | string[] |  |
| `errorKm` | number \| null | ErrorKm is the distance to the reference position (GNSS or configured; NaN = none), set by the Analyzer. |
| `skylineRaised` | number \| null | SkylineRaised is the share of directions (10° sectors with session ends) whose skyline the fit moved off the model horizon: terrain or obstructions the model does not know (null = too few sectors). |
| `refSource` | string | *optional* - RefSource names the reference of ErrorKm (gnss, configured, survey). |

<a id="t-analytics-Obstruction"></a>

#### analytics.Obstruction

Obstruction is a sector where low-altitude coverage is persistently short.

| Field | Type | Notes |
|---|---|---|
| `fromDeg` | number |  |
| `toDeg` | number | clockwise from FromDeg (may wrap through north) |
| `centerDeg` | number |  |
| `widthDeg` | number |  |
| `compass` | string |  |
| `elevDeg` | number \| null | mean skyline elevation in the sector |
| `excessDeg` | number \| null | above the median skyline |
| `rangePct` | number \| null | low-altitude range relative to the all-round median, % |
| `severity` | string | "minor" \| "moderate" \| "severe" |
| `text` | string |  |

<a id="t-analytics-Overview"></a>

#### analytics.Overview

Overview bundles what a statistics landing page needs in one call.

| Field | Type | Notes |
|---|---|---|
| `now` | integer |  |
| `ant` | string |  |
| `last24h` | [analytics.TrafficTotals](#t-analytics-TrafficTotals) |  |
| `sources` | [analytics.SourceShare](#t-analytics-SourceShare) |  |
| `df` | [analytics.DFCount](#t-analytics-DFCount)[] |  |
| `allTime` | [analytics.AllTime](#t-analytics-AllTime) |  |
| `today` | [analytics.DayTotals](#t-analytics-DayTotals) | *optional* |
| `busiest` | [analytics.Daily](#t-analytics-Daily) | *optional* - 30-day daily totals with busiest day / hour |
| `top` | [analytics.Top](#t-analytics-Top) | *optional* - 30 days, 5 rows each |
| `records` | [store.Record](#store-rows)[] |  |
| `events` | [store.Event](#store-rows)[] |  |
| `located` | boolean |  |
| `receiver` | [analytics.LatLon](#t-analytics-LatLon) | *optional* |

<a id="t-analytics-PathLossFit"></a>

#### analytics.PathLossFit

PathLossFit is the fitted signal-vs-distance law rssi = InterceptDB - SlopeDBPerDecade * log10(d_km).

| Field | Type | Notes |
|---|---|---|
| `bins` | integer | distance bins used |
| `minKm` | number | fitted distance range |
| `maxKm` | number |  |
| `slopeDbPerDecade` | number \| null | measured loss per decade of distance |
| `freeSpaceSlope` | number | 20 dB / decade |
| `interceptDb` | number \| null | extrapolated level at 1 km, dBFS |
| `r2` | number \| null |  |
| `systemOffsetDb` | number \| null | SystemOffsetDB is the mean of rssi + 20*log10(d): the level a free-space link would give at 1 km (transmit power + antenna and receiver gains in dBFS terms). It moves with receiver gain, cable loss and antenna gain. |
| `lineKm` | (number \| null)[] | sample distances for plotting |
| `measuredDb` | (number \| null)[] | fitted line |
| `freeSpaceDb` | (number \| null)[] | 20 dB / decade line through SystemOffsetDB |
| `text` | string |  |

<a id="t-analytics-PatternCompare"></a>

#### analytics.PatternCompare

PatternCompare compares a measured pattern with a modelled one (both centred on their means over the bearings with data).

| Field | Type | Notes |
|---|---|---|
| `rmsDb` | number \| null |  |
| `r` | number \| null | correlation of the shapes |
| `n` | integer |  |

<a id="t-analytics-PhaseStats"></a>

#### analytics.PhaseStats

PhaseStats are the mean hourly metrics of one sun phase or hour of day.

| Field | Type | Notes |
|---|---|---|
| `phase` | string | *optional* - "day" \| "twilight" \| "night" |
| `hour` | integer | local hour of day (ByHour only) |
| `hours` | integer | hours of data |
| `p95Km` | number \| null |  |
| `maxKm` | number \| null |  |
| `residDb` | number \| null |  |
| `aircraft` | number \| null |  |
| `positions` | number \| null |  |
| `p95NormKm` | number \| null | P95NormKm is the 95th percentile range with the traffic-volume effect removed (what the range would be at the average traffic level). |

<a id="t-analytics-PolarBand"></a>

#### analytics.PolarBand

PolarBand is the polar range plot of one altitude band (or of all altitudes: Band = -1). Every slice has one value per 2 degree bearing bin (see Coverage.BearingDeg); null = nothing received on that bearing.

| Field | Type | Notes |
|---|---|---|
| `band` | integer |  |
| `label` | string |  |
| `altFt` | number | representative altitude |
| `maxKm` | (number \| null)[] | MaxKm is the absolute maximum over the range; RobustKm the 95th percentile of the daily maxima with isolated spikes clipped to their neighbourhood (what the station reliably reaches); TypicalKm the median daily maximum. |
| `robustKm` | (number \| null)[] |  |
| `typicalKm` | (number \| null)[] |  |
| `count` | integer[] |  |
| `rssi` | (number \| null)[] | mean dBFS |
| `positions` | integer |  |
| `bearings` | integer | bearing bins with data |
| `bestKm` | number \| null |  |
| `bestBrg` | number \| null |  |
| `horizonKm` | number \| null | Radio horizon comparison (smooth earth, k = 4/3). line of sight at the top of the band |
| `horizonTypKm` | number \| null | at the representative altitude |
| `pctOfHorizon` | (number \| null)[] | 100 * RobustKm / HorizonKm per bearing |
| `meanPct` | number \| null | mean over all 180 bearings (no data = 0 %) |

<a id="t-analytics-RidgeFactor"></a>

#### analytics.RidgeFactor

RidgeFactor is one term of the range model.

| Field | Type | Notes |
|---|---|---|
| `name` | string |  |
| `label` | string |  |
| `group` | string | "weather" \| "traffic" \| "time" |
| `beta` | number \| null | Beta is the standardised coefficient: standard deviations of range per standard deviation of the factor. |
| `perUnit` | number \| null | PerUnit is the coefficient in km per unit of the factor. |

<a id="t-analytics-SignalDistance"></a>

#### analytics.SignalDistance

SignalDistance is the RSSI-vs-distance 2-D histogram.

| Field | Type | Notes |
|---|---|---|
| `distBinKm` | integer |  |
| `cells` | integer[][] | Cells are the non-empty bins as [distance bin, RSSI dB (floor), count]. |
| `total` | integer |  |
| `maxCount` | integer |  |
| `distKm` | (number \| null)[] | Per distance bin (index = bin, centre = (i + 0.5) * DistBinKm). |
| `n` | integer[] |  |
| `median` | (number \| null)[] |  |
| `p10` | (number \| null)[] |  |
| `p90` | (number \| null)[] |  |
| `rssiP01` | number \| null | Overall distribution. |
| `rssiP50` | number \| null |  |
| `rssiP99` | number \| null |  |
| `strongPct` | number \| null | StrongPct is the share of samples above -3 dBFS (overload indicator). |
| `fit` | [analytics.PathLossFit](#t-analytics-PathLossFit) | *optional* |

<a id="t-analytics-Skyline"></a>

#### analytics.Skyline

Skyline is the measured horizon: per bearing the lowest elevation angle at which aircraft are still received.

| Field | Type | Notes |
|---|---|---|
| `source` | string | Source is "heat" (lowest aircraft per heat-grid cell, needs a located receiver) or "coverage" (derived from the range per altitude band). |
| `elevDeg` | (number \| null)[] | per bearing bin; null = no data |
| `medianDeg` | number \| null |  |
| `bearings` | integer |  |

<a id="t-analytics-SourceShare"></a>

#### analytics.SourceShare

SourceShare splits traffic into ADS-B / MLAT / Mode-S-only.

| Field | Type | Notes |
|---|---|---|
| `adsbPct` | number \| null | Aircraft shares, weighted by time on air (sum of the per-interval aircraft gauges). |
| `mlatPct` | number \| null |  |
| `modesPct` | number \| null |  |
| `adsbPositions` | integer | Position counts. |
| `mlatPositions` | integer |  |

<a id="t-analytics-StationScore"></a>

#### analytics.StationScore

StationScore is the result of Analyzer.Score.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `score` | number \| null | 0..100, null without any data |
| `grade` | string | "excellent" \| "good" \| "fair" \| "poor" \| "no data" |
| `subs` | [analytics.SubScore](#t-analytics-SubScore)[] |  |
| `findings` | [analytics.Finding](#t-analytics-Finding)[] |  |

<a id="t-analytics-SubScore"></a>

#### analytics.SubScore

SubScore is one component of the station score.

| Field | Type | Notes |
|---|---|---|
| `key` | string | "range" \| "uniformity" \| "sensitivity" \| "decode" \| "uptime" |
| `label` | string |  |
| `score` | number \| null | 0..100, null when not available |
| `weight` | number |  |
| `available` | boolean |  |
| `detail` | string |  |

<a id="t-analytics-SunInfo"></a>

#### analytics.SunInfo

SunInfo is the sun / day-night state for the UI.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms the values are valid for |
| `located` | boolean |  |
| `receiver` | [analytics.LatLon](#t-analytics-LatLon) | *optional* |
| `position` | [astro.SunPosition](#t-astro-SunPosition) | *optional* - Position, Phase and Events are for the receiver position and absent until it is set. |
| `phase` | string | *optional* - "day" \| "civil" \| "nautical" \| "astronomical" \| "night" |
| `isDay` | boolean |  |
| `events` | [astro.EventsMs](#t-astro-EventsMs) | *optional* - today's sunrise / sunset / twilights |
| `nightSide` | [astro.NightSide](#t-astro-NightSide) | NightSide holds the terminator: four nested rings ([lon, lat]) of the region where the sun is below -0.833 / -6 / -12 / -18 degrees, and the sub-solar point. |
| `moon` | [astro.Moon](#t-astro-Moon) |  |

<a id="t-analytics-TimeSeries"></a>

#### analytics.TimeSeries

TimeSeries is a columnar time series (all slices have the length of T).

| Field | Type | Notes |
|---|---|---|
| `t` | integer[] | interval start, Unix ms |
| `framesPerSec` | (number \| null)[] | message rate |
| `positionsPerSec` | (number \| null)[] | position rate |
| `aircraft` | integer[] | distinct aircraft heard (hour: peak minute) |
| `aircraftPos` | integer[] | with a position |
| `adsb` | integer[] |  |
| `mlat` | integer[] |  |
| `modes` | integer[] |  |
| `maxKm` | (number \| null)[] |  |
| `rssiAvg` | (number \| null)[] |  |

<a id="t-analytics-Top"></a>

#### analytics.Top

Top is the result of Analyzer.Top.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `flights` | integer |  |
| `aircraft` | integer |  |
| `operators` | [analytics.TopEntry](#t-analytics-TopEntry)[] |  |
| `countries` | [analytics.TopEntry](#t-analytics-TopEntry)[] |  |
| `types` | [analytics.TopEntry](#t-analytics-TopEntry)[] |  |
| `airframes` | [analytics.TopEntry](#t-analytics-TopEntry)[] |  |

<a id="t-analytics-TopEntry"></a>

#### analytics.TopEntry

TopEntry is one row of a top list.

| Field | Type | Notes |
|---|---|---|
| `key` | string | operator / country / type code / ICAO hex |
| `label` | string | *optional* - type description, registration ... |
| `flights` | integer |  |
| `aircraft` | integer | distinct airframes |
| `msgs` | integer |  |
| `pct` | number \| null | share of all flights in range |
| `type` | string | *optional* - airframes only |
| `operator` | string | *optional* - airframes only |
| `country` | string | *optional* - airframes only |
| `maxKm` | number \| null | airframes only |

<a id="t-analytics-Traffic"></a>

#### analytics.Traffic

Traffic is the result of Analyzer.Traffic.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `resolution` | string | "minute" \| "hour" |
| `stepSec` | integer |  |
| `series` | [analytics.TimeSeries](#t-analytics-TimeSeries) |  |
| `totals` | [analytics.TrafficTotals](#t-analytics-TrafficTotals) |  |
| `df` | [analytics.DFCount](#t-analytics-DFCount)[] |  |
| `sources` | [analytics.SourceShare](#t-analytics-SourceShare) |  |

<a id="t-analytics-TrafficFit"></a>

#### analytics.TrafficFit

TrafficFit is the dependence of the hourly range on traffic volume: p95_km = a + SlopeKm * ln(1 + aircraft).

| Field | Type | Notes |
|---|---|---|
| `n` | integer |  |
| `slopeKm` | number \| null | km per e-fold of traffic |
| `r2` | number \| null |  |
| `kmPerDoubling` | number \| null | KmPerDoubling is the range gained when traffic doubles. |

<a id="t-analytics-TrafficTotals"></a>

#### analytics.TrafficTotals

TrafficTotals summarises a range.

| Field | Type | Notes |
|---|---|---|
| `frames` | integer |  |
| `positions` | integer |  |
| `mlatPositions` | integer |  |
| `crcBad` | integer |  |
| `decodePct` | number \| null | frames that passed CRC, % |
| `framesPerSec` | number \| null | mean over the time the receiver was up |
| `peakAircraft` | integer |  |
| `peakAircraftT` | integer | Unix ms, 0 = none |
| `maxKm` | number \| null |  |
| `rssiAvg` | number \| null | frame-weighted |
| `uptimePct` | number \| null | minutes with data / minutes in range |
| `uniqueAircraft` | integer | distinct airframes with a flight in range |
| `flights` | integer |  |
| `minutesWithData` | integer |  |

<a id="t-analytics-WeatherCorr"></a>

#### analytics.WeatherCorr

WeatherCorr is the association of one range metric with one variable.

| Field | Type | Notes |
|---|---|---|
| `var` | string |  |
| `metric` | string |  |
| `n` | integer |  |
| `pearson` | number \| null |  |
| `pearsonP` | number \| null |  |
| `spearman` | number \| null |  |
| `spearmanP` | number \| null |  |
| `partial` | number \| null | Partial is the correlation with the traffic level (aircraft count) held constant. |
| `partialP` | number \| null |  |
| `bestLagH` | integer | BestLagH is the lag in hours (-6..+6) with the strongest correlation; positive = the range follows the weather by that many hours. |
| `bestLagR` | number \| null |  |
| `significant` | boolean | Significant: partial correlation p-value below the Bonferroni threshold for the number of variables tested, with enough hours. |

<a id="t-analytics-WeatherImpact"></a>

#### analytics.WeatherImpact

WeatherImpact is the result of Analyzer.WeatherImpact.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `hours` | integer | hours with both range and weather data |
| `rangeHours` | integer |  |
| `weatherHours` | integer |  |
| `minHours` | integer |  |
| `enough` | boolean |  |
| `vars` | [analytics.WeatherVarInfo](#t-analytics-WeatherVarInfo)[] |  |
| `metrics` | string[] |  |
| `correlations` | [analytics.WeatherCorr](#t-analytics-WeatherCorr)[] |  |
| `responses` | [analytics.WeatherResponse](#t-analytics-WeatherResponse)[] |  |
| `model` | [analytics.WeatherModel](#t-analytics-WeatherModel) \| null |  |
| `episodes` | [analytics.DuctEpisode](#t-analytics-DuctEpisode)[] |  |
| `conclusions` | string[] |  |

<a id="t-analytics-WeatherModel"></a>

#### analytics.WeatherModel

WeatherModel is the ridge regression explaining the hourly 95th percentile range from weather, traffic and hour-of-day terms.

| Field | Type | Notes |
|---|---|---|
| `ok` | boolean |  |
| `reason` | string | *optional* |
| `target` | string |  |
| `n` | integer |  |
| `r2` | number \| null | in sample |
| `cvR2` | number \| null | blocked 5-fold cross-validation |
| `baseR2` | number \| null | traffic + time-of-day terms only |
| `weatherR2` | number \| null | R2 - BaseR2: what weather adds |
| `lambda` | number \| null |  |
| `residStdKm` | number \| null |  |
| `factors` | [analytics.RidgeFactor](#t-analytics-RidgeFactor)[] | sorted by \|Beta\| |

<a id="t-analytics-WeatherResponse"></a>

#### analytics.WeatherResponse

WeatherResponse is a binned response curve of the 95th percentile range.

| Field | Type | Notes |
|---|---|---|
| `var` | string |  |
| `metric` | string |  |
| `bins` | [stat.ResponseBin](#t-stat-ResponseBin)[] |  |

<a id="t-analytics-WeatherVarInfo"></a>

#### analytics.WeatherVarInfo

WeatherVarInfo describes one weather variable of the analysis.

| Field | Type | Notes |
|---|---|---|
| `key` | string |  |
| `label` | string |  |
| `unit` | string |  |
| `n` | integer | hours with this variable and a range value |
| `mean` | number \| null |  |
| `std` | number \| null |  |

<a id="t-analytics-WeekHeatmap"></a>

#### analytics.WeekHeatmap

WeekHeatmap is traffic by hour of day and weekday (local time).

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `utcOffsetSec` | integer |  |
| `aircraft` | ((number \| null)[])[] | Rows are weekdays Monday..Sunday, columns hours 0..23; null = no data. mean aircraft in view |
| `framesPerSec` | ((number \| null)[])[] | mean message rate |
| `hours` | integer[][] | hours of data behind each cell |
| `days` | string[] |  |
| `busiestDay` | integer | Busiest slot of the week. 0 = Monday, -1 = no data |
| `busiestHour` | integer | 0..23 |
| `hourOfDay` | (number \| null)[] | HourOfDay is the mean aircraft count per hour of day over all weekdays. |

<a id="t-analytics-YagiFit"></a>

#### analytics.YagiFit

YagiFit is the directional lobe inferred from the measured azimuth pattern by fitting level + max(omni, yagi lobe) (a diversity receiver uses whichever antenna hears the frame better): m(az) = C + max(0, G + 10*n*log10(cos(az - az0))), cos^n(BW/2) = 1/2

| Field | Type | Notes |
|---|---|---|
| `ok` | boolean |  |
| `reason` | string | *optional* |
| `azimuthDeg` | number \| null | measured boresight |
| `beamwidthDeg` | number \| null | measured -3 dB beamwidth of the lobe |
| `gainOverOmniDb` | number \| null | lobe peak above the omni level |
| `confidence` | number \| null | Confidence 0..1: how much of the azimuth variation the lobe explains, how far it stands above the omni level and how complete the data is. |
| `confidenceLabel` | string | "low" \| "medium" \| "high" |
| `explainedPct` | number \| null |  |
| `rmsDb` | number \| null |  |
| `fitDb` | (number \| null)[] | fitted curve per bearing bin, same reference as AzimuthPattern.RelDB |
| `configured` | [analytics.ConfiguredYagi](#t-analytics-ConfiguredYagi) | *optional* |
| `pointingErrorDeg` | number \| null | measured - configured; positive = clockwise (right) |
| `beamwidthErrorDeg` | number \| null | measured - configured |
| `gainErrorDb` | number \| null | measured - modelled advantage |
| `text` | string |  |

<a id="t-astro-EventsMs"></a>

#### astro.EventsMs

EventsMs is Events with Unix-millisecond timestamps (0 = does not occur), ready for the JSON API.

| Field | Type | Notes |
|---|---|---|
| `solarNoon` | integer |  |
| `noonElevation` | number |  |
| `midnightElevation` | number |  |
| `sunrise` | integer |  |
| `sunset` | integer |  |
| `civilDawn` | integer |  |
| `civilDusk` | integer |  |
| `nauticalDawn` | integer |  |
| `nauticalDusk` | integer |  |
| `astroDawn` | integer |  |
| `astroDusk` | integer |  |
| `dayLengthSec` | number |  |
| `polarDay` | boolean |  |
| `polarNight` | boolean |  |

<a id="t-astro-Moon"></a>

#### astro.Moon

Moon is the moon's phase at an instant.

| Field | Type | Notes |
|---|---|---|
| `illumination` | number | illuminated fraction of the disc, 0..1 |
| `phase` | number | 0 = new, 0.25 = first quarter, 0.5 = full, 0.75 = last quarter |
| `ageDays` | number | approximate days since new moon |
| `waxing` | boolean |  |
| `name` | string | "New Moon", "Waxing Crescent", "First Quarter", … |

<a id="t-astro-NightSide"></a>

#### astro.NightSide

NightSide describes the dark side of the earth at one instant as nested rings.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms the rings are valid for |
| `subsolarLat` | number | where the sun is overhead |
| `subsolarLon` | number |  |
| `sunset` | number[][] | sun below -0.833°: not day |
| `civil` | number[][] | below -6°: civil twilight over |
| `nautical` | number[][] | below -12° |
| `astronomical` | number[][] | below -18°: full night |

<a id="t-astro-SunPosition"></a>

#### astro.SunPosition

SunPosition is the sun's position for an observer.

| Field | Type | Notes |
|---|---|---|
| `azimuth` | number | degrees true, clockwise from north |
| `elevation` | number | apparent elevation (refraction corrected) |
| `trueElevation` | number | geometric elevation of the sun's centre |
| `declination` | number | apparent declination |
| `hourAngle` | number | local hour angle, negative before solar noon |
| `eqOfTimeMin` | number | equation of time, minutes |
| `distanceAu` | number | earth–sun distance |

<a id="t-engine-ACDBImportInfo"></a>

#### engine.ACDBImportInfo

ACDBImportInfo describes the aircraft database currently installed.

| Field | Type | Notes |
|---|---|---|
| `source` | string |  |
| `url` | string | *optional* |
| `records` | integer |  |
| `t` | integer | Unix ms of the import |
| `lastModified` | integer | *optional* - Unix ms, server's Last-Modified |
| `attribution` | string | *optional* |
| `licence` | string | *optional* |
| `format` | string | *optional* - Format is the detected file format of an uploaded file ("tar1090", "opensky"); empty for a download, whose Source names it. |

<a id="t-engine-ImportStatus"></a>

#### engine.ImportStatus

ImportStatus is the state of the current or last aircraft database import.

| Field | Type | Notes |
|---|---|---|
| `running` | boolean |  |
| `stage` | string | Stage: "" (never ran), "download" (fetching and parsing into the staging file), "store" (loading the staging file into the database), "done", "error", "cancelled". |
| `source` | string | *optional* |
| `started` | integer | *optional* - Unix ms |
| `finished` | integer | *optional* - Unix ms |
| `progress` | [refdata.Progress](#t-refdata-Progress) | *optional* - download / parse progress |
| `stored` | integer | rows written to the database |
| `error` | string | *optional* |

<a id="t-flowlearn-Path"></a>

#### flowlearn.Path

Path is a learned prediction.

| Field | Type | Notes |
|---|---|---|
| `points` | [flowlearn.Point](#t-flowlearn-Point)[] |  |
| `weight` | number | blend weight used |
| `learned` | boolean | Learned is true when at least one step used learned traffic. |

<a id="t-flowlearn-Point"></a>

#### flowlearn.Point

Point is one point of a predicted path.

| Field | Type | Notes |
|---|---|---|
| `dt` | number | seconds after the start |
| `lat` | number |  |
| `lon` | number |  |
| `track` | number |  |
| `conf` | number | Conf is the confidence of the learned turn applied in this step (0 = straight). |

<a id="t-flowlearn-Skill"></a>

#### flowlearn.Skill

Skill is the self-check of one horizon.

| Field | Type | Notes |
|---|---|---|
| `horizonS` | integer |  |
| `n` | integer | scored predictions |
| `learnedM` | number | mean error of the learned prediction, m (EWMA) |
| `straightM` | number | mean error of dead reckoning, m (EWMA) |
| `weight` | number | current blend weight of the learned turn rates (0..1) |
| `betterPct` | number | share of predictions where learned was closer |
| `improvement` | number | 1 - learned/straight (positive = learned better) |

<a id="t-flowlearn-Stats"></a>

#### flowlearn.Stats

Stats describes the learner.

| Field | Type | Notes |
|---|---|---|
| `cells` | integer |  |
| `aircraft` | integer |  |
| `samples` | integer | turn observations learned |
| `skill` | [flowlearn.Skill](#t-flowlearn-Skill)[] |  |

<a id="t-hub-ClientInfo"></a>

#### hub.ClientInfo

ClientInfo describes one logged-in client (Server.Clients).

| Field | Type | Notes |
|---|---|---|
| `addr` | string |  |
| `since` | integer | Unix ms of login |
| `bytesSent` | integer | bytes written to the socket |
| `backlog` | integer | bytes waiting in the client's buffer |

<a id="t-latency-AircraftRow"></a>

#### latency.AircraftRow

AircraftRow is one line of the per-aircraft latency table.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `firstSeen` | integer | Unix ms (host) |
| `lastSeen` | integer |  |
| `n` | integer | ADS-B messages |
| `positions` | integer |  |
| `t1` | integer | positions with T=1 |
| `t0` | integer |  |
| `tFlag` | boolean | latest position's T bit |
| `lastTc` | integer |  |
| `xpdrN` | integer | Transponder latency (transmit - time of applicability), T=1 only: |
| `xpdrMeanMs` | number |  |
| `xpdrStdMs` | number |  |
| `xpdrMinMs` | number |  |
| `xpdrMaxMs` | number |  |
| `agePairs` | integer | Kinematic position-age estimate (all aircraft with enough pairs): |
| `ageJitterMs` | number | sigma of the position age |
| `ageModelMs` | number | mean age, uniform-register model |
| `ageCheckMs` | number | T=1: RMS(pair method - exact) |
| `positionAgeMs` | number | best estimate: exact mean (T=1) else model |
| `ageExact` | boolean |  |
| `propMeanUs` | number |  |
| `propLastUs` | number |  |
| `slantKm` | number |  |
| `flags` | string[] |  |

<a id="t-latency-Bin"></a>

#### latency.Bin

Bin is one histogram bin.

| Field | Type | Notes |
|---|---|---|
| `lo` | number |  |
| `hi` | number |  |
| `n` | integer |  |

<a id="t-latency-Counts"></a>

#### latency.Counts

Counts are the message and gate counters of a Summary (since start).

| Field | Type | Notes |
|---|---|---|
| `frames` | integer |  |
| `gpsFrames` | integer |  |
| `noGps` | integer |  |
| `mlatExcluded` | integer |  |
| `positions` | integer |  |
| `positionsRanged` | integer |  |
| `positionsT1` | integer |  |
| `positionsT0` | integer |  |
| `negativeDelay` | integer |  |
| `absurdDelay` | integer |  |
| `oneSecondAhead` | integer | receiver delay ~ -1 s (legacy day-second range) |
| `gpsTimeNotUtc` | integer | receiver delay ~ -18 s (GPS time stamps) |
| `legacyRange` | integer | GPS ns >= 86400 s seen |
| `processingGated` | integer | Handled before Recv or > 10 s later |
| `rangeGated` | integer | slant range > 1000 km |
| `t1OutOfSpec` | integer | \|transmit - ToA\| > 150 ms |
| `t1UnexpectedTc` | integer | T=1 outside TC 5, 6, 9, 10, 20, 21 |
| `agePairs` | integer | position pairs used by the age estimator |
| `ageTurnsSkipped` | integer |  |
| `ageStaleRepeats` | integer | identical position while moving |
| `agePairsGated` | integer | \|delta age\| > 1 s |
| `frameEndCorrUs` | number | 64/120 us, or 0 when disabled |
| `receiverDelayMinMs` | number | gate |
| `receiverDelayMaxMs` | number | gate |

<a id="t-latency-Event"></a>

#### latency.Event

Event is one instant of a Waterfall.

| Field | Type | Notes |
|---|---|---|
| `key` | string |  |
| `label` | string |  |
| `utc` | string | RFC 3339 with ns |
| `atMs` | number | relative to the time of applicability |
| `uncMs` | number | +- uncertainty of this instant |

<a id="t-latency-FeedClock"></a>

#### latency.FeedClock

FeedClock is the NTP/GPS consistency state of one feed.

| Field | Type | Notes |
|---|---|---|
| `feedId` | string |  |
| `samples` | integer |  |
| `negative` | integer |  |
| `absurd` | integer |  |
| `lastMs` | number |  |
| `floorMs` | number | min of the last 8 x 10 s minima |
| `jitterMs` | number | RMS of successive 10 s minima |
| `driftUsPerMin` | number | LS slope of the 10 s minima (~10 min) |
| `intervals` | integer |  |
| `status` | string | "ok", "warn", "bad", "unknown" |
| `issues` | string[] |  |
| `minima` | [latency.FilterPoint](#t-latency-FilterPoint)[] | the 10 s minima, oldest first |
| `ref` | string | receiver-delay reference: "startOfFrame" / "endOfFrame" |

<a id="t-latency-FeedMinute"></a>

#### latency.FeedMinute

FeedMinute is one point of a feed's receiver-delay time series.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms, minute start (host clock) |
| `n` | integer |  |
| `minMs` | number |  |
| `p50Ms` | number |  |
| `p95Ms` | number |  |
| `maxMs` | number |  |
| `meanMs` | number |  |
| `floorMs` | number | clock-filter floor at the end of the minute |
| `jitterMs` | number | RMS of successive 10 s minima |
| `neg` | integer |  |
| `absurd` | integer |  |

<a id="t-latency-FilterPoint"></a>

#### latency.FilterPoint

FilterPoint is one 10 s receiver-delay minimum.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms (true UTC), interval start |
| `ms` | number |  |

<a id="t-latency-Finding"></a>

#### latency.Finding

Finding is a plain-language observation.

| Field | Type | Notes |
|---|---|---|
| `level` | string | "info", "warn", "bad" |
| `key` | string |  |
| `text` | string |  |

<a id="t-latency-Histogram"></a>

#### latency.Histogram

Histogram is a chart-ready distribution.

| Field | Type | Notes |
|---|---|---|
| `key` | string |  |
| `label` | string |  |
| `unit` | string | "ms" |
| `scale` | string | "log" (signed log bins) or "linear" |
| `count` | integer |  |
| `bins` | [latency.Bin](#t-latency-Bin)[] |  |

<a id="t-latency-NTPCheck"></a>

#### latency.NTPCheck

NTPCheck is the payload of GET /api/timing/ntpcheck.

| Field | Type | Notes |
|---|---|---|
| `ntp` | [latency.NTPInfo](#t-latency-NTPInfo) |  |
| `quality` | string | Quality: "good" (synced GNSS/PPS stratum 1), "degraded", "bad", "unknown" (no NTP configured). |
| `offsetUncertaintyMs` | number | OffsetUncertaintyMs bounds the error of the host->UTC correction: delay/2 + root delay/2 + root dispersion + jitter. |
| `issues` | string[] |  |
| `feeds` | [latency.FeedClock](#t-latency-FeedClock)[] |  |
| `explanation` | string |  |

<a id="t-latency-NTPInfo"></a>

#### latency.NTPInfo

NTPInfo is the quality of the configured NTP reference.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean |  |
| `synced` | boolean |  |
| `server` | string |  |
| `stratum` | integer |  |
| `refId` | string |  |
| `leap` | integer |  |
| `offsetMs` | number |  |
| `delayMs` | number |  |
| `jitterMs` | number |  |
| `rootDelayMs` | number |  |
| `rootDispMs` | number |  |
| `lastOk` | integer | Unix ms |
| `lastError` | string | *optional* |

<a id="t-latency-StageStats"></a>

#### latency.StageStats

StageStats summarises one stage over the summary window.

| Field | Type | Notes |
|---|---|---|
| `key` | string |  |
| `label` | string |  |
| `description` | string |  |
| `count` | integer |  |
| `minMs` | number |  |
| `p50Ms` | number |  |
| `p95Ms` | number |  |
| `p99Ms` | number |  |
| `maxMs` | number |  |
| `meanMs` | number |  |
| `uncertaintyMs` | number | UncertaintyMs is the measurement uncertainty of a single value of this stage (not its spread). |
| `exact` | boolean | measured, not estimated |
| `ref` | string | *optional* - Ref is the reference instant of the GPS time of arrival for stages that use it ("receiver"): RefStartOfFrame (default) or RefEndOfFrame (Options.NoFrameEndCorrection). Empty for the other stages. |

<a id="t-latency-Summary"></a>

#### latency.Summary

Summary is the payload of GET /api/timing/latency.

| Field | Type | Notes |
|---|---|---|
| `now` | integer | Unix ms (host clock of the latest message) |
| `windowSec` | integer |  |
| `receiverSet` | boolean |  |
| `clockSynced` | boolean |  |
| `clockOffsetMs` | number |  |
| `hostOffsetMs` | number | HostOffsetMs is the host clock error estimated from the receiver's GPS time stamps while the host clock is not NTP-synchronised (host minus GPS UTC, including the receiver -> host delay floor of a few ms); null when synced or not yet estimated. HostOffsetCorrected: it is large (> 100 ms) and the host-side stages (receiver, airToSystem, endToEnd) are corrected by it, i.e. relative (their floor is ~0). |
| `hostOffsetCorrected` | boolean |  |
| `aircraft` | integer | ADS-B aircraft with positions in the table |
| `aircraftT1` | integer | ... whose latest position had T=1 |
| `shareT1` | number | AircraftT1 / Aircraft |
| `msgShareT1` | number | share of positions (since start) with T=1 |
| `stages` | [latency.StageStats](#t-latency-StageStats)[] |  |
| `counts` | [latency.Counts](#t-latency-Counts) |  |
| `feeds` | [latency.FeedClock](#t-latency-FeedClock)[] |  |
| `findings` | [latency.Finding](#t-latency-Finding)[] |  |

<a id="t-latency-Waterfall"></a>

#### latency.Waterfall

Waterfall breaks the latest position message of one aircraft into stages.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `t` | integer | Unix ms, true UTC of host reception |
| `df` | integer |  |
| `tc` | integer |  |
| `tFlag` | boolean |  |
| `cprOdd` | boolean |  |
| `feed` | string |  |
| `exact` | boolean | time of applicability known exactly (T=1) |
| `slantKm` | number |  |
| `elevDeg` | number |  |
| `events` | [latency.Event](#t-latency-Event)[] |  |
| `stages` | [latency.WStage](#t-latency-WStage)[] |  |
| `totalMs` | number | applicability -> handled |
| `totalUncMs` | number |  |
| `notes` | string[] |  |

<a id="t-latency-WStage"></a>

#### latency.WStage

WStage is one stage (interval) of a Waterfall.

| Field | Type | Notes |
|---|---|---|
| `key` | string |  |
| `label` | string |  |
| `ns` | integer |  |
| `ms` | number |  |
| `uncMs` | number |  |
| `method` | string | "exact", "measured", "estimated", "unavailable" |

<a id="t-met-Comparison"></a>

#### met.Comparison

Comparison is the observed profile against a model profile.

| Field | Type | Notes |
|---|---|---|
| `levels` | [met.ModelDiff](#t-met-ModelDiff)[] | one per model level inside the observed range, ascending |
| `nTemp` | integer |  |
| `tempBiasC` | number | *optional* - mean (obs - model) |
| `tempRmsC` | number | *optional* |
| `nWind` | integer |  |
| `speedBiasKt` | number | *optional* |
| `speedRmsKt` | number | *optional* |
| `dirBiasDeg` | number | *optional* - mean signed direction difference (levels with >= 10 kt) |
| `uBiasKt` | number | *optional* |
| `vBiasKt` | number | *optional* |
| `rmsvdKt` | number | *optional* - RMS vector difference, the standard wind verification score |

<a id="t-met-Grid"></a>

#### met.Grid

Grid is the result.

| Field | Type | Notes |
|---|---|---|
| `from` | integer | Unix ms |
| `to` | integer |  |
| `levelFt` | number |  |
| `bandFt` | number |  |
| `cellDeg` | number |  |
| `samples` | integer | observations in the band and window |
| `cells` | [met.GridCell](#t-met-GridCell)[] |  |

<a id="t-met-GridCell"></a>

#### met.GridCell

GridCell is one cell of the grid.

| Field | Type | Notes |
|---|---|---|
| `lat` | number | cell centre |
| `lon` | number |  |
| `windDir` | number | *optional* - deg true, FROM |
| `windKt` | number | *optional* |
| `tempC` | number | *optional* - mean static air temperature |
| `isaDevC` | number | *optional* - tempC minus the ISA temperature at the band's level |
| `nWind` | integer |  |
| `nTemp` | integer |  |
| `nAc` | integer | distinct aircraft |
| `altFt` | number |  |

<a id="t-met-HistCell"></a>

#### met.HistCell

HistCell is one hour x level cell of the time-height history.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms, start of the hour |
| `level` | integer | 1000 ft layer |
| `altFt` | number | layer centre |
| `windDir` | number | *optional* |
| `windKt` | number | *optional* |
| `tempC` | number | *optional* |
| `isaDev` | number | *optional* |
| `nWind` | integer |  |
| `nTemp` | integer |  |

<a id="t-met-Inversion"></a>

#### met.Inversion

Inversion is a layer in which temperature rises with height.

| Field | Type | Notes |
|---|---|---|
| `baseFt` | number | pressure altitude |
| `topFt` | number |  |
| `baseTempC` | number |  |
| `topTempC` | number |  |
| `strengthC` | number | TopTempC - BaseTempC |
| `thicknessM` | number | geometric |
| `grad` | number | dT/dz, positive |

<a id="t-met-LapseLayer"></a>

#### met.LapseLayer

LapseLayer is the temperature gradient between two adjacent levels.

| Field | Type | Notes |
|---|---|---|
| `baseFt` | number |  |
| `topFt` | number |  |
| `lapse` | number | positive = cooling with height; geometric height |
| `class` | string |  |

<a id="t-met-Level"></a>

#### met.Level

Level is one 1000 ft layer of a vertical profile. Absent values are nil (never NaN), so the type marshals safely.

| Field | Type | Notes |
|---|---|---|
| `level` | integer | layer index: covers [level*1000, level*1000+1000) ft |
| `altFt` | number | mean pressure altitude of the samples (layer centre when unknown) |
| `hpa` | number | standard-atmosphere pressure at AltFt |
| `windDir` | number | *optional* - vector-mean wind, degrees true FROM |
| `windKt` | number | *optional* |
| `u` | number | *optional* - eastward component, kt |
| `v` | number | *optional* - northward component, kt |
| `tempC` | number | *optional* - mean static air temperature |
| `isaDev` | number | *optional* - TempC minus the standard atmosphere |
| `nWind` | integer |  |
| `nTemp` | integer |  |
| `windSpread` | number | *optional* - RMS vector deviation from the mean, kt |
| `tempSpread` | number | *optional* - standard deviation, degC |

<a id="t-met-MaxWind"></a>

#### met.MaxWind

MaxWind is the level of the strongest vector-mean wind.

| Field | Type | Notes |
|---|---|---|
| `altFt` | number |  |
| `windKt` | number |  |
| `windDir` | number |  |
| `jet` | boolean | >= 60 kt at or above 20 000 ft: a jet stream |

<a id="t-met-ModelDiff"></a>

#### met.ModelDiff

ModelDiff compares the observed profile with one model pressure level. Differences are observed minus model.

| Field | Type | Notes |
|---|---|---|
| `hpa` | number |  |
| `altFt` | number | pressure altitude of the level |
| `obsTempC` | number | *optional* |
| `modelTempC` | number | *optional* |
| `dTempC` | number | *optional* |
| `obsWindDir` | number | *optional* |
| `obsWindKt` | number | *optional* |
| `modelWindDir` | number | *optional* |
| `modelWindKt` | number | *optional* |
| `dSpeedKt` | number | *optional* |
| `dDirDeg` | number | *optional* - signed, -180..180 |
| `vecDiffKt` | number | *optional* - magnitude of the vector difference |

<a id="t-met-Obs"></a>

#### met.Obs

Obs is one recent observation for the map layer.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms |
| `icao` | string |  |
| `lat` | number |  |
| `lon` | number |  |
| `altFt` | number |  |
| `windDir` | number | *optional* |
| `windKt` | number | *optional* |
| `oat` | number | *optional* |
| `q` | number |  |
| `mrar` | boolean | *optional* - contains a direct BDS 4,4 value |

<a id="t-met-Profile"></a>

#### met.Profile

Profile is a vertical profile above the station with its analysis.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms the profile is valid for (end of the window) |
| `from` | integer | Unix ms, start of the averaging window |
| `windowMin` | number | length of the window, minutes |
| `nWind` | integer | observations in the window (all levels) |
| `nTemp` | integer |  |
| `levels` | [met.Level](#t-met-Level)[] | ascending, only levels with data |
| `freezingLevelFt` | number | *optional* - lowest 0 degC crossing, pressure altitude |
| `tropopause` | [met.Tropopause](#t-met-Tropopause) | *optional* |
| `maxWind` | [met.MaxWind](#t-met-MaxWind) | *optional* - jet-stream maximum |
| `maxShear` | [met.Shear](#t-met-Shear) | *optional* - strongest vertical wind shear |
| `lapse` | [met.LapseLayer](#t-met-LapseLayer)[] | layer by layer |
| `inversions` | [met.Inversion](#t-met-Inversion)[] | temperature inversions below the tropopause |
| `windsAloft` | [met.WindAloft](#t-met-WindAloft)[] | at the standard flight levels |

<a id="t-met-RadioInversions"></a>

#### met.RadioInversions

RadioInversions is the propagation product of a profile.

| Field | Type | Notes |
|---|---|---|
| `layers` | [met.RadioLayer](#t-met-RadioLayer)[] | ascending |
| `strongest` | integer | Strongest is the index into Layers of the most negative refractivity gradient (-1 when there is none). |
| `class` | string | Class is the regime of the strongest layer ("" without layers). |
| `lowestBaseFt` | number | *optional* - LowestBaseFt is the base of the lowest inversion (nil without one): the most relevant one for signals arriving near the horizon. |
| `note` | string | Note is a one-line plain-language summary. |

<a id="t-met-RadioLayer"></a>

#### met.RadioLayer

RadioLayer is a temperature inversion aloft seen by aircraft, with the refractivity gradient it implies for 1090 MHz propagation. A temperature inversion alone bends rays only a little more than standard; it becomes super-refractive or ducting when the humidity also drops through it, which is why the model's humidity is combined with the observed temperatures here.

| Field | Type | Notes |
|---|---|---|
| `baseFt` | number | pressure altitude |
| `topFt` | number |  |
| `baseM` | number | the same in metres (standard atmosphere) |
| `topM` | number |  |
| `thicknessM` | number | geometric |
| `strengthC` | number | temperature rise through the layer |
| `grad` | number | dT/dz |
| `dndh` | number | *optional* - DNDH is the mean refractivity gradient through the layer, N-units per km (standard about -40; <= -79 super-refraction; <= -157 ducting). nil when it could not be computed. |
| `k` | number | *optional* - effective earth radius factor for that gradient |
| `class` | string | *optional* - radio.Class* of DNDH |
| `humidity` | string | Humidity is "model" when the model's relative humidity was available at both ends of the layer, "dry" when the gradient is the dry-air term only (a lower bound of the bending). |
| `rhBase` | number | *optional* |
| `rhTop` | number | *optional* |

<a id="t-met-Shear"></a>

#### met.Shear

Shear is a vertical wind shear between two adjacent levels.

| Field | Type | Notes |
|---|---|---|
| `baseFt` | number |  |
| `topFt` | number |  |
| `ktPer1000Ft` | number | magnitude of the vector difference per 1000 ft |

<a id="t-met-TimeHeight"></a>

#### met.TimeHeight

TimeHeight is the hourly time-height history of the profiles.

| Field | Type | Notes |
|---|---|---|
| `from` | integer | Unix ms |
| `to` | integer | Unix ms (exclusive) |
| `hours` | integer[] | Unix ms of the hours that have data, ascending |
| `cells` | [met.HistCell](#t-met-HistCell)[] | ordered by hour, then level |

<a id="t-met-Tropopause"></a>

#### met.Tropopause

Tropopause is the thermal (WMO lapse-rate) tropopause estimate.

| Field | Type | Notes |
|---|---|---|
| `altFt` | number |  |
| `tempC` | number |  |
| `confirmed` | boolean | Confirmed: the profile reaches 2 km above, so the WMO criterion (mean lapse rate <= 2 K/km over the next 2 km) was checked in full. Otherwise it held over the available depth only. |
| `depthFt` | number | depth of data above that the criterion was checked over |

<a id="t-met-WindAloft"></a>

#### met.WindAloft

WindAloft is one row of the winds-aloft table.

| Field | Type | Notes |
|---|---|---|
| `fl` | integer |  |
| `altFt` | number |  |
| `windDir` | number | *optional* |
| `windKt` | number | *optional* |
| `tempC` | number | *optional* |
| `isaDev` | number | *optional* |
| `coded` | string | *optional* - Coded is the classic winds-aloft (FD/FB) group "DDSS+TT", e.g. "2745-32" = from 270 degrees at 45 kt, -32 degC ("9900" = light and variable; speeds of 100 kt and more add 50 to the direction digits). |

<a id="t-model-ACASMark"></a>

#### model.ACASMark

ACASMark is an active ACAS resolution advisory reported by the aircraft.

| Field | Type | Notes |
|---|---|---|
| `advisory` | string | e.g. "Climb", "Do not descend" |
| `sense` | string | *optional* - "up" \| "down" |
| `since` | integer | Unix ms of the first report |

<a id="t-model-AggAuth"></a>

#### model.AggAuth

AggAuth holds the aggregator account and session policy (DESIGN §7, §20.3).

| Field | Type | Notes |
|---|---|---|
| `ownerSessionDays` | integer | OwnerSessionDays is the absolute lifetime of a station-owner session (default 30, 1..365 days). |
| `ownerIdleDays` | integer | OwnerIdleDays ends an owner session without activity (default 14, 1..365 days). |
| `adminSessionHours` | integer | AdminSessionHours is the absolute lifetime of an admin session (default 12, 1..168 hours). |
| `adminIdleMinutes` | integer | AdminIdleMinutes ends an admin session without activity (default 120, 5..1440 minutes). |
| `registration` | string | Registration is "open" (default: anyone may create an owner account) or "closed" (accounts are created by the admin only). |
| `loginFreeFailures` | integer | LoginFreeFailures is the number of failed logins per client and user name before the login throttle starts delaying (default 5, 1..100). |

<a id="t-model-AggBeast"></a>

#### model.AggBeast

AggBeast holds the tunables of the Beast compat listener on BeastPort (DESIGN §5.1, §20.3). The rate and connection limits are in AggLimits.

| Field | Type | Notes |
|---|---|---|
| `identifyWaitMs` | integer | IdentifyWaitMs is how long a link may send Beast data before its UUID arrives; after it the link is attached as unidentified (default 2000, 100..60000 ms). |
| `idleTimeoutSec` | integer | IdleTimeoutSec closes links that are silent this long (default 180, 10..3600 s). |
| `pingIntervalSec` | integer | PingIntervalSec is the period of the server's own pings once the feeder asked for them (default 5, 1..300 s). |
| `slowDownEverySec` | integer | SlowDownEverySec is the minimum interval between two slow-down requests on one link (default 10, 1..600 s). |
| `queueBatches` | integer | QueueBatches is the per-link delivery queue length in batches (default 64, 4..65536). |
| `maxLinks` | integer | MaxLinks caps concurrent Beast links (default 4096, 1..100000). |
| `acceptMlatResults` | boolean | AcceptMlatResults passes frames carrying the MLAT magic timestamp (default false: they are dropped and counted). |
| `stripClock` | boolean | StripClock clears the receiver timestamps of every Beast frame (default false; Beast frames never enter MLAT either way). |
| `overLimitSeconds` | integer | OverLimitSeconds consecutive seconds above the rate limits disconnect a link (default 5, 1..300). |
| `coolDownSec` | integer | CoolDownSec is how long a station disconnected for its rate is refused (default 300, 0..86400 s; 0 = no cool-down). |

<a id="t-model-AggFedBox"></a>

#### model.AggFedBox

AggFedBox is a latitude/longitude rectangle in degrees (West > East crosses the antimeridian).

| Field | Type | Notes |
|---|---|---|
| `south` | number | South is the southern edge. |
| `west` | number | West is the western edge. |
| `north` | number | North is the northern edge. |
| `east` | number | East is the eastern edge. |

<a id="t-model-AggFederation"></a>

#### model.AggFederation

AggFederation is aggregator.federation: links to other AirView networks over AVFED/1 and federation-safe outbound feeds (DESIGN §12.1 phase 5). It has exactly the JSON shape of internal/federation.Config (a test keeps the two in step). Zero = off / default.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | Enabled switches federation on. Off, nothing listens or dials. |
| `name` | string | *optional* - Name is this network's display name sent to peers (optional). |
| `listen` | string | *optional* - Listen is the address of the inbound federation listener ("" = accept no inbound peers, e.g. ":30170"). |
| `identityFile` | string | *optional* - IdentityFile is the node's Ed25519 key file ("" = federation.key in the data directory). The node ID shown to peer operators derives from it. |
| `tlsCert` | string | *optional* - TLSCert is an operator certificate for the listener ("" = a self-signed certificate kept in <data>/fed-tls). |
| `tlsKey` | string | *optional* - TLSKey is the key of TLSCert (a secret path). |
| `intervalMs` | integer | *optional* - IntervalMs is the aircraft-state export period (default 1000, 100..60000). |
| `mlatIntervalMs` | integer | *optional* - MLATIntervalMs is the MLAT-result flush period (default 250, 50..10000). |
| `fullRefreshS` | integer | *optional* - FullRefreshS: every this many seconds a peer gets every aircraft, not only the changed ones (default 30, 1..3600). |
| `ttlS` | integer | *optional* - TTLS is how long a remote aircraft stays without an update (default 60, 5..3600 s). |
| `transit` | boolean | *optional* - Transit allows forwarding data received from one peer to other peers (each peer must also allow it in send.transit; default off). |
| `maxHops` | integer | *optional* - MaxHops bounds the path length of forwarded data, origin included (default 3, 1..8). |
| `maxRemoteAircraft` | integer | *optional* - MaxRemoteAircraft bounds the remote aircraft store (default 50000, 100..1000000). |
| `maxInflateBps` | integer | *optional* - MaxInflateBps bounds the decompressed inbound rate per peer in bytes per second (default 16000000, 100000..1000000000). |
| `queueRecords` | integer | *optional* - QueueRecords bounds the per-peer send queue (default 256, 8..100000). |
| `hideRemote` | boolean | *optional* - HideRemote keeps received aircraft off the map (default false: they are drawn as an overlay). |
| `inject` | boolean | *optional* - Inject hands received aircraft to the tracker (default false; keep it off: remote data would enter statistics and records). |
| `peers` | [model.AggFedPeer](#t-model-AggFedPeer)[] | *optional* - Peers are the federated networks. |
| `feeds` | [model.AggFedFeed](#t-model-AggFedFeed)[] | *optional* - Feeds are federation-safe outbound feeds to other networks. |

<a id="t-model-AggFedFeed"></a>

#### model.AggFedFeed

AggFedFeed is one federation-safe outbound feed (aggregator.federation.feeds[]).

| Field | Type | Notes |
|---|---|---|
| `name` | string | Name is the label (status, logs). |
| `format` | string | Format is "beast" (raw Mode S, timestamps and signal zeroed) or "json" (one aircraft file per line). |
| `mode` | string | Mode is "connect" (dial Addr and push) or "listen" (serve on Addr). |
| `addr` | string | Addr is "host:port". |
| `allow` | string[] | *optional* - Allow lists the client networks of a listen feed (default loopback only). |
| `maxClients` | integer | *optional* - MaxClients bounds the clients of a listen feed (default 16). |
| `intervalMs` | integer | *optional* - IntervalMs is the JSON emission period (default 1000). |
| `modeAC` | boolean | *optional* - ModeAC includes Mode A/C replies in a Beast feed (default off). |
| `disabled` | boolean | *optional* - Disabled keeps the entry without running it. |
| `policy` | [model.AggFedPolicy](#t-model-AggFedPolicy) | Policy filters the feed (mlat, transit and delayS do not apply). |

<a id="t-model-AggFedPeer"></a>

#### model.AggFedPeer

AggFedPeer is one federated AirView network (aggregator.federation.peers[]).

| Field | Type | Notes |
|---|---|---|
| `name` | string | Name is the local label of the peer. |
| `id` | string | ID is the peer's node ID (shown by the peer's `airview federation id`); mandatory. |
| `addr` | string | *optional* - Addr is "host:port" to dial ("" = only accept this peer). |
| `pin` | string | *optional* - Pin pins the peer listener's TLS key ("sha256/<b64>") or requires a WebPKI certificate ("webpki"); "" relies on the mutual identity check. |
| `disabled` | boolean | *optional* - Disabled keeps the entry but neither dials nor accepts it. |
| `send` | [model.AggFedPolicy](#t-model-AggFedPolicy) | Send is what this network shares with the peer. |
| `receive` | [model.AggFedReceive](#t-model-AggFedReceive) | Receive is what this network accepts from the peer. |

<a id="t-model-AggFedPolicy"></a>

#### model.AggFedPolicy

AggFedPolicy says what is shared with one peer or out-feed. Feeder identities, station positions, receiver lists, signal levels and IP addresses are never shared.

| Field | Type | Notes |
|---|---|---|
| `aircraft` | string | *optional* - Aircraft is "all" (default), "nomlat" (MLAT positions removed) or "none". |
| `mlat` | boolean | *optional* - MLAT shares MLAT result records (peers only; default off). |
| `transit` | boolean | *optional* - Transit forwards data of other peers to this peer (needs federation.transit too). |
| `island` | string | *optional* - Island is what happens to island-guard aircraft: "coarsen" (default) or "drop". |
| `minAltFt` | integer | *optional* - MinAltFt drops aircraft below this altitude and on the ground (0 = no floor). |
| `noGround` | boolean | *optional* - NoGround drops aircraft on the ground. |
| `noMilitary` | boolean | *optional* - NoMilitary drops aircraft the reference data marks military. |
| `boxes` | [model.AggFedBox](#t-model-AggFedBox)[] | *optional* - Boxes limits sharing to positions inside any box (empty = anywhere). |
| `excludeBoxes` | [model.AggFedBox](#t-model-AggFedBox)[] | *optional* - ExcludeBoxes drops positions inside any of these boxes. |
| `delayS` | integer | *optional* - DelayS delays everything sent to this peer (0..600 s; peers only). |
| `maxAircraft` | integer | *optional* - MaxAircraft caps the aircraft per export (0 = no cap). |

<a id="t-model-AggFedReceive"></a>

#### model.AggFedReceive

AggFedReceive says what is accepted from a peer.

| Field | Type | Notes |
|---|---|---|
| `aircraft` | string | *optional* - Aircraft is "all" (default) or "none". |
| `mlat` | string | *optional* - MLAT is "all" (default) or "none". |
| `boxes` | [model.AggFedBox](#t-model-AggFedBox)[] | *optional* - Boxes keeps only positions inside any box (empty = anywhere). |
| `maxAircraft` | integer | *optional* - MaxAircraft bounds the aircraft held from this peer (default 20000). |

<a id="t-model-AggIngest"></a>

#### model.AggIngest

AggIngest holds the tunables of the station-aware ingest and the AVF/1 listener (internal/ingest, internal/ingest/avfsrv; DESIGN §6.2, §20.3).

| Field | Type | Notes |
|---|---|---|
| `windowMs` | integer | WindowMs: a group accepts copies this long after its first copy (default 1000, 200..5000). |
| `flushAfterMs` | integer | FlushAfterMs: a group goes to MLAT this long after its first copy (default 400, 100..windowMs). |
| `retainMs` | integer | RetainMs keeps a closed group for de-duplication (default 1000, 100..10000). |
| `maxMatchMs` | integer | MaxMatchMs caps the time tolerance of a match (default 300, 20..700). |
| `minMatchMs` | integer | MinMatchMs is the smallest tolerance (default 10, 1..maxMatchMs). |
| `uniqueSkewMs` | integer | UniqueSkewMs is the largest skew at which a non-repeating payload joins a group before the station's time correction is calibrated (default 2500, 100..10000). |
| `lateAfterMs` | integer | LateAfterMs routes older frames to the statistics lanes only (0 = windowMs + retainMs + 500, else 500..60000). |
| `shards` | integer | Shards is the number of grouper shards (default 64, 1..4096; rounded up to a power of two). |
| `reduceMs` | integer | ReduceMs is the reduce interval of selective mode (default 500, 100..5000). |
| `flowHoldSec` | integer | FlowHoldSec tunes the FLOW rules (default 5, 1..60). |
| `flowGraceSec` | integer | FlowGraceSec tunes the FLOW rules (default 10, 1..300). |
| `avfQueueRecords` | integer | AVFQueueRecords is the per-connection queue of the AVF listener in record batches (default 64, 4..4096). |
| `avfResumeGraceSec` | integer | AVFResumeGraceSec keeps a station online after its connection ended, for a resume (default 30, 1..300). |
| `avfMaxSessions` | integer | AVFMaxSessions caps concurrent AVF connections (default 10000, 1..100000). |
| `clockGateFloorUs` | integer | ClockGateFloorUs is the smallest tolerance of the synced-station network-time gate in µs (0 = default 50; -1 = no floor). |
| `flushSampleEvery` | integer | FlushSampleEvery feeds every n-th group's delays to the adaptive flush estimator (0 = default 4). |
| `avfHostSlackMs` | integer | AVFHostSlackMs bounds how much earlier than physically plausible an AVF feeder's host times may place its frames before the host-clock offset is corrected (0 = default 300; -1 = off). |
| `adaptiveFlush` | boolean | AdaptiveFlush replaces the fixed flushAfterMs of MLAT groups by a per-aircraft delay from the delivery latency of the stations that hear it (150-600 ms; default false). |

<a id="t-model-AggLanes"></a>

#### model.AggLanes

AggLanes holds the per-station statistics lane settings of the network tracker (DESIGN §6.3-6.4, §20.3).

| Field | Type | Notes |
|---|---|---|
| `flushSec` | integer | FlushSec is the interval of the per-station minute statistics rows (default 10, 2..60 s). |
| `slowFlushSec` | integer | SlowFlushSec is the interval of the range, coverage, signal, elevation and heat increments (default 300, 60..3600 s). |
| `evictHours` | integer | EvictHours drops the in-memory lane of a station silent this long (default 24, 1..720 h). |
| `maxRangeKm` | number | MaxRangeKm is the range gate: positions farther than this from a station never enter its range and coverage products, and are refused as implausible for it (default 800, 100..1500 km). |

<a id="t-model-AggLimits"></a>

#### model.AggLimits

AggLimits holds the aggregator's admission and load limits (DESIGN §4.6), sized for the publication target of 1,000+ feeders and 10k+ concurrent aircraft on a mid-size VPS (DESIGN §20.2).

| Field | Type | Notes |
|---|---|---|
| `maxStations` | integer | MaxStations is the maximum number of registered, approved stations (default 2000, 1..100000). |
| `selectiveAbove` | integer | SelectiveAbove switches stations to selective traffic when more than this many approved stations are online (default 100, 0..100000; 0 = always selective). |
| `perIpStations` | integer | PerIPStations is the maximum number of stations behind one IP address unless the address is in AllowCIDRs (default 8, 1..1000). |
| `maxFpsPerStation` | integer | MaxFPSPerStation: above this frame rate the station gets FLOW, then is disconnected with a cool-down (default 6000, 100..100000). |
| `maxKBpsPerStation` | integer | MaxKBpsPerStation is the compressed byte-rate limit per station in kB/s (default 200, 10..10000). |
| `maxUnauthPerIp` | integer | MaxUnauthPerIP is the number of not yet authenticated connections per IP (default 4, 1..1000). |
| `maxConnsPerIp` | integer | MaxConnsPerIP is the number of connections per IP (default 16, 1..10000). |
| `maxConnsPerSubnet` | integer | MaxConnsPerSubnet is the number of connections per IPv4 /24 or IPv6 /48 (default 32, 1..100000). |
| `handshakeTimeoutSec` | integer | HandshakeTimeoutSec bounds the TLS + AVF handshake (default 10, 2..120). |
| `ingestFps` | integer | IngestFPS is the global ingest budget in frames per second across all stations; selective-eligible traffic is shed first above it (default 3,000,000, 10,000..100,000,000). |
| `maxAircraft` | integer | MaxAircraft is the maximum number of concurrently tracked aircraft (default 30000, 1000..1,000,000). |
| `trackerShards` | integer | TrackerShards is the number of ICAO shards of the global tracker (default 0 = automatic from GOMAXPROCS, else 1..256). |
| `publicRps` | number | PublicRPS / PublicBurst rate-limit the public API per client IP (defaults 1 and 10). |
| `publicBurst` | integer |  |
| `maxPublicSse` | integer | MaxPublicSSE is the total number of public SSE streams (one per IP; default 2000, 1..100000). |
| `maxHttpConns` | integer | MaxHTTPConns is the total number of open web (HTTP) connections (default 20000, 100..1,000,000); further connections wait in the kernel backlog. |
| `maxHttpConnsPerIp` | integer | MaxHTTPConnsPerIP is the number of open web connections per client IPv4 address or IPv6 /64 (default 64, 1..100000). Loopback and aggregator.trustedProxies are exempt. |
| `allowCidrs` | string[] | *optional* - AllowCIDRs are exempt from PerIPStations and the per-IP / per-subnet connection limits (e.g. a club's relay host). |

<a id="t-model-AggMLAT"></a>

#### model.AggMLAT

AggMLAT holds the MLAT engine settings of an aggregator (DESIGN §9, §19).

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | Enabled runs the own MLAT engine (default true). |
| `returnResults` | boolean | ReturnResults sends results to contributing stations (default true; the minimum-contribution rule of DESIGN §8.4 always applies). |
| `holdout` | number | Holdout is the fraction of good ADS-B aircraft solved as Mode S-only for self-validation (default 0.10, 0..0.5). |
| `autoUseSurvey` | boolean | AutoUseSurvey uses a reverse-MLAT station position for MLAT without owner confirmation when its horizontal sigma is at most AutoUseMaxSigmaM and it passes the suspect checks (default true, user decision DESIGN §19.3). |
| `autoUseMaxSigmaM` | number | AutoUseMaxSigmaM is that sigma threshold in metres (default 50, 5..500). |
| `epochMs` | integer | EpochMs is the clock filter epoch (default 500, 100..2000). |
| `maxComponentStations` | integer | MaxComponentStations is the station count above which a clock-sync component is partitioned (default 200, 20..2000; DESIGN §20.2). |
| `modeAc` | boolean | ModeAC enables Mode A/C multilateration among anchors (default false). |
| `udp` | boolean | UDP accepts the mlat-client UDP transport on MLATPort (default false). |
| `surveyIntervalH` | integer | SurveyIntervalH is how often declared positions are re-surveyed (default 6, 1..168 hours). |
| `refractivityN0` | number | RefractivityN0 is the surface radio refractivity in N-units used for the propagation delay (default 315, 200..450; 0 = derive it from METAR when available, else 315). |
| `refractivityScaleHeightM` | number | RefractivityScaleHeightM is the scale height of the exponential refractivity profile in metres (default 7000, 4000..12000). |
| `geoidFile` | string | *optional* - GeoidFile is a geoid grid replacing the embedded EGM96 1-degree grid (NGA WW15MGH.GRD, GeographicLib .pgm or AirView .avg; "" = embedded). |
| `ref` | [model.AggMLATRef](#t-model-AggMLATRef) | Ref, Group, Plan, Solve, Track and Engine tune the MLAT engine's parts (zero = the package default; see each block). |
| `group` | [model.AggMLATGroup](#t-model-AggMLATGroup) |  |
| `plan` | [model.AggMLATPlan](#t-model-AggMLATPlan) |  |
| `solve` | [model.AggMLATSolve](#t-model-AggMLATSolve) |  |
| `track` | [model.AggMLATTrack](#t-model-AggMLATTrack) |  |
| `engine` | [model.AggMLATEngine](#t-model-AggMLATEngine) | Engine tunes the MLAT engine root (internal/mlat): workers, queues, result routing and the quality feedback switches (zero = default). |
| `clock` | [model.AggMLATClock](#t-model-AggMLATClock) | Clock tunes the joint clock filter (internal/mlat/clock): gates, step detection, anchor probation and blacklisting (zero = default). |
| `quarantine` | [model.AggMLATQuarantine](#t-model-AggMLATQuarantine) | Quarantine is the timing quarantine of new stations (internal/registry/trust; zero = default). |
| `survey` | [model.AggMLATSurvey](#t-model-AggMLATSurvey) | Survey tunes the reverse-MLAT station survey (internal/mlat/survey; zero = default). |
| `latencyCal` | [model.AggMLATLatencyCal](#t-model-AggMLATLatencyCal) | LatencyCal is the per-airframe ADS-B latency calibration of the reference tracks with its A/B gate (default off). |

<a id="t-model-AggMLATBundle"></a>

#### model.AggMLATBundle

AggMLATBundle tunes the network bundle adjustment (aggregator.mlat.survey.bundle; internal/mlat/survey Bundler): station positions and clocks of up to maxStations stations refined together, wrong declarations released and flagged. Zero = default.

| Field | Type | Notes |
|---|---|---|
| `disabled` | boolean | Disabled turns the bundle adjustment off (default false). |
| `everyMin` | integer | EveryMin is the adjustment period (default 60, 1..1440 min). |
| `spanMin` | integer | SpanMin is the data span of one adjustment (default 60, 5..360 min). |
| `maxStations` | integer | MaxStations bounds one adjustment (default 64, 2..512). |
| `windowS` | number | WindowS is the clock polynomial window (default 10, 2..120 s). |
| `thinIntervalS` | integer | ThinIntervalS keeps one transmission per aircraft per interval (default 8, 1..60 s). |
| `maxFrames` | integer | MaxFrames bounds the stored transmissions (default 200000, 1000..5000000). |
| `minStationRecs` | integer | MinStationRecs: a station with fewer usable receptions is left out (default 100, 10..1000000). |
| `outlierMinM` | number | OutlierMinM: a prior position is contradicted beyond max(outlierMinM, outlierK x sigma) (default 100, 1..100000 m). |
| `outlierK` | number | OutlierK is that sigma factor (default 3, 1..100). |
| `declaredSigmaM` | number | DeclaredSigmaM is the prior 1-sigma of a declared position that passed the test (default 30, 1..10000 m). |
| `anchorSigmaM` | number | AnchorSigmaM is the horizontal prior 1-sigma of an anchor (GNSS or confirmed position; default 5, 0.1..1000 m). |
| `maxReleases` | integer | MaxReleases bounds the priors released in one adjustment (default 16, 1..512). |
| `workers` | integer | Workers is the number of goroutines of one adjustment (default min(GOMAXPROCS, 8), up to 256). |

<a id="t-model-AggMLATC"></a>

#### model.AggMLATC

AggMLATC tunes the mlat-client compatible listener on MLATPort (internal/ingest/mlatc, DESIGN §5.2). The rate and connection limits are in AggLimits. Zero = default.

| Field | Type | Notes |
|---|---|---|
| `splitSync` | string | SplitSync is the split_sync policy: "auto" (default, "" = auto: offered, with a per-client fallback to sync pairs), "on" (always offered) or "off" (never; the reference protocol's default). The old boolean form is accepted: true = "on", false = "auto" (DESIGN §5.2). |
| `maxRequested` | integer | MaxRequested caps the aircraft one client is asked to send (default 2000). |
| `maxSeen` | integer | MaxSeen caps a client's remembered seen set (default 10000). |
| `maxBadLines` | integer | MaxBadLines disconnects a client after this many malformed lines (default 200). |
| `idleTimeoutSec` | integer | IdleTimeoutSec drops a silent client (default 150 s). |
| `heartbeatSec` | integer | HeartbeatSec is the server heartbeat period (default 30 s). |
| `statsSec` | integer | StatsSec is the period of stats messages (default 60 s). |
| `overLimitSeconds` | integer | OverLimitSeconds above the rate limit disconnect a client (default 5). |
| `coolDownSec` | integer | CoolDownSec refuses a disconnected rate offender this long (default 300 s). |
| `maxClients` | integer | MaxClients caps concurrent connections (default 4096). |
| `maxOutQueueKB` | integer | MaxOutQueueKB is the per-client output queue (default 256 KiB). |
| `udpHost` | string | UDPHost is the host told to clients for the UDP transport ("" = the local address of the client's TCP connection; set it behind NAT or in a port-mapped container). Needs aggregator.mlat.udp. |
| `udpPort` | integer | UDPPort is the externally reachable UDP port told to clients (0 = the bound UDP port, 1..65535). |
| `udpListen` | string | UDPListen is the datagram listen address ("" = the TCP listener's address and port). |
| `udpAnySource` | boolean | UDPAnySource accepts datagrams from any source address (default false: only from the address of the client's TCP connection; needed behind carrier-grade NAT). |
| `udpReaders` | integer | UDPReaders is the number of goroutines reading the datagram socket (default min(4, GOMAXPROCS), up to 64). |
| `modeAcAllClients` | boolean | ModeACAllClients sends Mode A/C requests also to clients that did not announce "modeac" in their handshake (default false). |

<a id="t-model-AggMLATClock"></a>

#### model.AggMLATClock

AggMLATClock tunes the joint clock filter (internal/mlat/clock, DESIGN §9.2). The epoch and the component partition size stay in aggregator.mlat (epochMs, maxComponentStations). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `hostSigmaS` | number | HostSigmaS is the 1σ error of server-side host times used to start a new clock epoch (default 0.05 s). |
| `ppsCorrelatedNs` | number | PPSCorrelatedNs is the part of a GPS-class station's timestamp error that is common to one second (1PPS discipline error; default 30 ns, negative = off). |
| `resetThetaSigmaNs` | number | ResetThetaSigmaNs is the smallest clock-offset 1σ after a detected step (default 1000000 ns = 1 ms). |
| `maxMembers` | integer | MaxMembers caps the stations used from one sync group (default 32, 3..256). |
| `syncRatePerStation` | number | SyncRatePerStation is the target number of sync groups per station and second (default 6, up to 100). |
| `maxObsAgeS` | number | MaxObsAgeS drops sync groups older than this at their epoch (default 5 s). |
| `stationTimeoutS` | number | StationTimeoutS removes a station from its component after this long without an accepted observation (default 120 s). |
| `coObsWindowS` | number | CoObsWindowS is the co-observation window that defines the connected clock components (default 60 s). |
| `probationS` | number | ProbationS is the anchor probation of a GPS station (default 600 s). |
| `anchorFreqMaxPpb` | number | AnchorFreqMaxPPB is the frequency limit for anchor promotion (default 2 ppb). |
| `anchorOffsetMaxNs` | number | AnchorOffsetMaxNs is the offset limit against the other anchors for promotion (default 200 ns). |
| `anchorDriftMaxNsPerH` | number | AnchorDriftMaxNsPerH demotes an anchor that drifts faster (default 100 ns/h). |
| `anchorHostMaxS` | number | AnchorHostMaxS is how far a GPS station's clock may be from the server's corrected time before it can become an anchor (default 0.4 s). |
| `gateProb` | number | GateProb is the probability of the sync innovation gate (default 0.999, below 1). |
| `wTestMax` | number | WTestMax is the largest studentised innovation of an accepted sync member (default 4.5). |
| `stepSigma` | number | StepSigma is the outlier size, in σ, that counts towards a step (default 4). |
| `stepCount` | integer | StepCount consecutive same-sign outliers declare a clock step (default 5). |
| `stepRunCount` | integer | StepRunCount same-sign steps of a free clock within StepRunWindowS also reset its frequency (default 3; -1 = off). |
| `stepRunWindowS` | number | StepRunWindowS is the window of StepRunCount (default 60 s). |
| `blacklistS` | number | BlacklistS is how long an aircraft that keeps failing sync is not used as a reference (default 600 s). |
| `blacklistMaxFrac` | number | BlacklistMaxFrac caps the blacklisted share of the sync aircraft (default 0.2, up to 1). |
| `stationBadOutlier` | number | StationBadOutlier: a member whose outlier share is at least this does not count towards an aircraft's failure (default 0.25, up to 1). |
| `syncedMinObs` | integer | SyncedMinObs is the accepted observations in SyncedWindowS a station needs to count as synced (default 5). |
| `syncedWindowS` | number | SyncedWindowS is the window of SyncedMinObs (default 30 s). |
| `workers` | integer | Workers is the number of goroutines that update clock components in parallel (default GOMAXPROCS, up to 64). |
| `noJitterLearning` | boolean | NoJitterLearning turns off the on-line learning of receiver jitter (default false). |
| `reoriginS` | number | ReoriginS moves the filter's internal time origin forward after this many seconds (default 86400, at least 10). |
| `relGaugeBetaSigmaNs` | number | RelGaugeBetaSigmaNs is the β_short gauge of relative components without GPS-class clocks (default 10 ns; negative = no β gauge). |
| `externalInflate` | number | ExternalInflate scales the variance of a member from another partition (default 2, at least 1). |
| `partitionStations` | integer | PartitionStations is the largest partition of a partitioned time domain (default min(100, maxComponentStations); 4..2000). |
| `externalStates` | integer | ExternalStates caps the cross-partition members per partition and epoch that are estimated as shared states (default 64, up to 1024; negative = off). |
| `partitionDriftNs` | number | PartitionDriftNs is the common-mode random walk (ns/sqrt(s)) of the partitions that do not carry the gauge (default 1, up to 100; negative = off). |

<a id="t-model-AggMLATEngine"></a>

#### model.AggMLATEngine

AggMLATEngine tunes the MLAT engine root (internal/mlat, DESIGN §9, §20.3). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `workers` | integer | Workers is the number of processing goroutines (default GOMAXPROCS, 1..64). |
| `queueLen` | integer | QueueLen is the per-worker queue length in groups (default 4096); a full queue drops the group and counts it. |
| `publishIntervalMs` | integer | PublishIntervalMs is the shortest interval between two map updates of one aircraft (default 500 ms). |
| `maxSolveSites` | integer | MaxSolveSites caps the stations used in one solve (default 16, 4..64). |
| `contribWindowSec` | integer | ContribWindowSec: a station receives an aircraft's results only when it contributed to its track within this window (default 60 s, DESIGN §8.4). |
| `minTimedPerMin` | integer | MinTimedPerMin is the timed frames per minute a synced station must deliver to receive results (default 10). |
| `holdoutRotateMin` | integer | HoldoutRotateMin rotates the hold-out set (default 10 minutes). |
| `latencyFeedback` | boolean | LatencyFeedback feeds hold-out fixes into the reference store's position-latency estimate (default false; DESIGN §9.6 excludes it). |
| `jitterFeedback` | boolean | JitterFeedback passes the jitter learned from solver residuals to the clock filter (default false). |
| `applyKAtm` | boolean | ApplyKAtm applies the hourly k_atm estimate to the solver and the clock filter (default false: advisory only). |
| `verifyEverySec` | integer | VerifyEverySec is the per-aircraft interval of the ADS-B TDOA verification (default 5 s; -1 = off). |
| `syncWarmupSec` | integer | SyncWarmupSec holds sync observations back after the start (default 60 s; -1 = none). |
| `ingestGate` | boolean | IngestGate gives the ingest the clock filter's gate view: synced copies are checked against their network time (with a 50 µs floor) and sync candidates are flushed early and re-delivered with every copy (default false). |
| `modeAcCodeFormat` | string | ModeACCodeFormat is the layout of the 2-byte Mode A/C payload delivered by the feeders: "pulses" (default, "" = pulses: the reply pulse order of the Beast description) or "nibbles" (4 octal digits). |
| `modeAcJitterNs` | number | ModeACJitterNs is the extra timing noise of Mode A/C replies (default 50 ns, up to 1000; negative = none). |

<a id="t-model-AggMLATField"></a>

#### model.AggMLATField

AggMLATField tunes the regional GNSS−baro field (DESIGN §9.6). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `cellKm` | number | CellKm is the horizontal cell size (default 50 km). |
| `bandFt` | number | BandFt is the altitude band of a cell (default 2000 ft). |
| `driftMPerH` | number | DriftMPerH is the random-walk rate of a cell in m per √h (default 20). |
| `obsSigmaM` | number | ObsSigmaM is the 1σ of one per-aircraft observation (default 12 m). |
| `profileSigmaM` | number | ProfileSigmaM is the 1σ of the weather-profile fallback used where no cell has been learned (default 25 m). |
| `profileSigmaTK` | number | ProfileSigmaTK is the 1σ temperature error of that profile (default 1.5 K). |

<a id="t-model-AggMLATGroup"></a>

#### model.AggMLATGroup

AggMLATGroup tunes the cluster assembler (internal/mlat/group, DESIGN §9.3). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `nSigma` | number | NSigma is the gate width in σ of the timing noise (default 4, 2..8). |
| `slackM` | number | SlackM is the geometric slack of the baseline gate (default 1000 m). |
| `hintSlackM` | number | HintSlackM is the geometric slack of the predicted-position gate (default 300 m). |
| `hintMaxSigmaM` | number | HintMaxSigmaM ignores position hints with a larger 1σ (default 5000 m). |
| `colocatedM` | number | ColocatedM: stations closer than this are one site (default 1000 m). |
| `minSites` | integer | MinSites is the number of sites needed to solve without an altitude (default 4, 3..8). |
| `minSitesAlt` | integer | MinSitesAlt is the number of sites needed with an altitude (default 3). |
| `maxParts` | integer | MaxParts is the most clusters taken from one ingest group (default 4, 1..8). |
| `lateHoldMs` | integer | LateHoldMs keeps emitted clusters this long for late copies (default 2000, ..5000). |
| `lateSlots` | integer | LateSlots is the size of the late-copy ring per MLAT worker (default 16384, about 1 kB per slot); above 4 workers the rings share 4x this size (at least 2048 slots each). |

<a id="t-model-AggMLATLatencyCal"></a>

#### model.AggMLATLatencyCal

AggMLATLatencyCal configures aggregator.mlat.latencyCal: the per-airframe ADS-B position-latency calibration of the reference tracks with its own A/B gate (internal/mlat/quality LatencyCal, DESIGN §9.6). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | Enabled runs the calibration (default false). Even when on, the built-in A/B test applies learned latencies only after they measurably reduce the along-track error of the treated half of the airframes. |
| `minGain` | number | MinGain is the relative error reduction the A/B test needs before the calibration is applied (default 0.05, 0.001..1). |
| `zEnable` | number | ZEnable is the significance needed to enable it (default 3, 0.5..20). |
| `zDisable` | number | ZDisable is the significance below which an enabled calibration is switched off again (default 2, 0.1..20). |
| `treatFraction` | number | TreatFraction is the share of calibrated airframes that get their learned latency (the rest are the control arm; default 0.5, 0.05..0.95). |
| `minCalSamples` | integer | MinCalSamples: an airframe counts as calibrated after this many samples (default 20, 1..100000). |
| `minArmSamples` | integer | MinArmSamples is the samples each A/B arm needs (default 500, 10..10000000). |
| `minArmAircraft` | integer | MinArmAircraft is the airframes each A/B arm needs (default 20, 2..100000). |
| `windowH` | integer | WindowH is the A/B window in hours (default 6, 1..168). |
| `priorSigmaS` | number | PriorSigmaS is the prior 1-sigma of an airframe latency in seconds (default 0.15, 0.001..2). |
| `calibratedSigmaS` | number | CalibratedSigmaS: an airframe counts as calibrated below this posterior 1-sigma in seconds (default 0.04, 0.001..1). |

<a id="t-model-AggMLATPlan"></a>

#### model.AggMLATPlan

AggMLATPlan tunes the MLAT planner (internal/mlat/plan, DESIGN §4.4, §9.3). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `syncSetMin` | integer | SyncSetMin is the smallest syncSet per station (default 15). |
| `syncSetMax` | integer | SyncSetMax is the largest syncSet per station (default 30). |
| `mlatSetMax` | integer | MLATSetMax caps a station's mlatSet in aircraft (default 400). |
| `stationFpsBudget` | number | StationFPSBudget caps the frame rate a selective station is asked for (default 400 frames/s). |
| `stationsPerAircraft` | integer | StationsPerAircraft is the most stations asked for one MLAT aircraft (default 12, 4..64). |
| `minStationsPerAircraft` | integer | MinStationsPerAircraft is the number always asked for when available (default 6). |
| `targetSigmaM` | number | TargetSigmaM: above the minimum, stations are added while the expected horizontal σ is above this (default 30 m). |
| `heardTtlS` | integer | HeardTTLS: a station counts as hearing an aircraft this long after its last copy (default 60 s). |
| `adsbGoodAgeS` | integer | ADSBGoodAgeS: an aircraft with a good ADS-B position this recent is no MLAT candidate (default 10 s). |
| `minDwellS` | integer | MinDwellS keeps a set member at least this long (default 30 s). |
| `maxChurn` | integer | MaxChurn is the most set changes per station and replan (default 10). |
| `replanEveryS` | integer | ReplanEveryS is the replan period (default 5 s). |
| `acquireHz` | number | AcquireHz is the solve rate of aircraft being acquired (default 4). |
| `trackedHz` | number | TrackedHz is the solve rate of tracked aircraft (default 2). |
| `holdOutHz` | number | HoldOutHz is the solve rate of hold-out ADS-B aircraft (default 1). |
| `suspectHz` | number | SuspectHz is the solve rate of suspect ADS-B aircraft (default 0.5). |
| `globalSolveHz` | number | GlobalSolveHz is the solver budget of the whole network (default 20000 solves/s). |

<a id="t-model-AggMLATQuarantine"></a>

#### model.AggMLATQuarantine

AggMLATQuarantine is the timing quarantine of new stations (internal/registry/trust, H-mlatc): a new station's copies reach MLAT only after its timing agreed with trusted references. Zero = default.

| Field | Type | Notes |
|---|---|---|
| `disabled` | boolean | Disabled admits new stations to MLAT without proving their timing (default false: new stations start in timing quarantine). |
| `minRefs` | integer | MinRefs is the number of distinct reference stations a station's timing must agree with (default 2). |
| `windowSec` | integer | WindowSec is the length of one fit window per station pair (default 30 s). |
| `maxSigmaNs` | number | MaxSigmaNs is the floor of the per-pair residual threshold (default 1500 ns). |
| `goodWindows` | integer | GoodWindows good windows prove the timing (default 8). |
| `maxBadFrac` | number | MaxBadFrac is the largest share of judged references with as many bad windows as good ones (default 0.2, up to 1). |
| `minAgeSec` | integer | MinAgeSec is the time from the first comparison before a proof (default 180 s). |

<a id="t-model-AggMLATRef"></a>

#### model.AggMLATRef

AggMLATRef tunes the ADS-B reference tracks (internal/mlat/ref, DESIGN §9.2.2, §9.6). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `window` | integer | Window is the number of measurement nodes kept per track (default 16, 6..64). |
| `qHorizontal` | number | QHorizontal is the horizontal white-jerk PSD in m²/s⁵ (default 4). |
| `qVertical` | number | QVertical is the vertical white-jerk PSD in m²/s⁵ (default 1). |
| `minNacp` | integer | MinNACp is the sync-reference NACp gate (default 8, 1..11). |
| `minNic` | integer | MinNIC is the sync-reference NIC gate (default 7, 1..11). |
| `maxPosAgeS` | number | MaxPosAgeS is the largest time from a queried instant to the nearest position report of a usable reference (default 1 s). |
| `latencySigmaS` | number | LatencySigmaS is the prior 1σ position latency of T=0 transmitters (default 0.15 s). |
| `gateChi2` | number | GateChi2 is the position innovation gate (2 degrees of freedom, default 30). |
| `gateFloorM` | number | GateFloorM is the innovation always accepted (default 25 m). |
| `probationS` | number | ProbationS keeps a track unusable after a jump (default 30 s). |
| `suspendS` | number | SuspendS suspends an unstable (duplicate or spoofed) address (default 600 s). |
| `trackTimeoutS` | number | TrackTimeoutS drops tracks not updated for this long (default 120 s). |
| `altReplyMinS` | number | AltReplyMinS: the barometric altitude of a Mode S reply (DF0/4/16/20) updates a reference track's vertical channel at most once per this interval (default 2 s). |
| `pairBaroS` | number | PairBaroS: a GNSS height (TC 20-22) and a pressure altitude reported within this interval form one GNSS−baro sample (default 1.5 s). |
| `noAltReplies` | boolean | NoAltReplies ignores the altitude of Mode S replies in the reference tracks (default false). |
| `field` | [model.AggMLATField](#t-model-AggMLATField) | Field tunes the GNSS−baro field. |

<a id="t-model-AggMLATSolve"></a>

#### model.AggMLATSolve

AggMLATSolve tunes the position solver (internal/mlat/solve, DESIGN §9.4). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `maxIter` | integer | MaxIter bounds the Levenberg-Marquardt iterations per start (default 10). |
| `huberK` | number | HuberK is the Huber threshold on the w-test (default 1.345). |
| `pfa` | number | PFA is the false-alarm probability of the RAIM test (default 0.001). |
| `maxSigmaHM` | number | MaxSigmaHM rejects fixes with a larger horizontal σ (default 2000 m). |
| `altRejectSigma` | number | AltRejectSigma rejects fixes whose height disagrees with the altitude by more σ (default 4). |
| `priorSeedMaxSigmaM` | number | PriorSeedMaxSigmaM: a track prior with a smaller horizontal σ seeds the solve (default 2000 m). |
| `gridStepM` | number | GridStepM is the step of the 3+altitude start grid (default 5000 m). |
| `gridHalfM` | number | GridHalfM is the half width of that grid (default 400000 m). |
| `maxSpeedMps` | number | MaxSpeedMps is the implied-speed limit (default 308.7 m/s = 600 kt). |
| `machFactor` | number | MachFactor scales a reported Mach number (default 1.25). |
| `hostTolS` | number | HostTolS is the transmit-time tolerance against the host arrival time (default 0.05 s). |
| `horizonSlackM` | number | HorizonSlackM is the accepted clearance below the radio horizon (default 300 m). |
| `minHaeM` | number | MinHAEM is the lowest plausible height above the ellipsoid (default -500 m; may be negative). |
| `maxHaeM` | number | MaxHAEM is the highest plausible height (default 25000 m). |
| `maxRangeM` | number | MaxRangeM bounds the distance from the nearest station (default 800000 m). |
| `relinM` | number | RelinM re-linearises the refraction excess after this movement (default 5000 m). |
| `ransacMinStations` | integer | RANSACMinStations enables RANSAC from this many stations (default 6). |
| `ransacMaxSubsets` | integer | RANSACMaxSubsets bounds the RANSAC subsets (default 64). |
| `alwaysAlternates` | boolean | AlwaysAlternates always searches for the second solution branch (default false). |

<a id="t-model-AggMLATSurvey"></a>

#### model.AggMLATSurvey

AggMLATSurvey tunes the reverse-MLAT station survey (internal/mlat/survey, DESIGN §10.2-10.3). Zero = the package default.

| Field | Type | Notes |
|---|---|---|
| `disabled` | boolean | Disabled turns the survey off (default false). |
| `maxJobs` | integer | MaxJobs bounds the concurrently collecting surveys (default 64, 1..4096). |
| `maxCheckFrac` | number | MaxCheckFrac bounds the periodic checks of located stations to this share of the stations (default 0.1, 0.001..1). |
| `minDurationSec` | integer | MinDurationSec is the collection time before the first evaluation (default 180, 30..21600 s). |
| `evalEverySec` | integer | EvalEverySec is the evaluation period after that (default 60, 5..3600 s). |
| `maxDurationMin` | integer | MaxDurationMin ends a survey (default 30, 1..1440 min). |
| `noDataAfterMin` | integer | NoDataAfterMin ends a survey that collected almost nothing (default 10, 1..1440 min). |
| `retryAfterMin` | integer | RetryAfterMin is the pause after an unsuccessful survey of an unlocated station (default 30, 1..43200 min; doubles up to surveyIntervalH). |
| `ownerMinIntervalMin` | integer | OwnerMinIntervalMin rate-limits owner-started surveys per station (default 60, up to 10080 min; negative = no limit). |
| `maxObs` | integer | MaxObs bounds a survey's observations (default 15000, 500..200000). |
| `minIntervalMs` | integer | MinIntervalMs thins observations to one per aircraft per interval (default 1000, 100..60000 ms). |
| `minPeers` | integer | MinPeers is the number of synced, located peers that fix a transmission's emission time (default 2, 1..16). |
| `maxPeerSigmaNs` | number | MaxPeerSigmaNs drops peers with a noisier network time (default 600, 20..10000 ns). |
| `maskDeg` | number | MaskDeg is the elevation mask (default 2.5, up to 15 degrees). |
| `heightSigmaM` | number | HeightSigmaM is the 1-sigma of the terrain + antenna height prior (default 20, 1..1000 m). |
| `defaultAglM` | number | DefaultAglM is the antenna height above ground when none is declared (default 10, up to 500 m). |
| `searchRadiusKm` | number | SearchRadiusKm is the seed grid's radius around the declared or Locate Me position (default 50, 1..1000 km). |
| `stableSigmaGpsM` | number | StableSigmaGpsM is the horizontal sigma a GPS-timed station's survey must reach to be stable (default 30, 1..1000 m). |
| `stableSigmaM` | number | StableSigmaM is the same limit for other clock classes (default 200, 1..5000 m). |
| `minAzSpreadDeg` | number | MinAzSpreadDeg is the azimuth spread of reference aircraft a stable survey needs (default 180, up to 360 degrees). |
| `minAircraft` | integer | MinAircraft is the number of distinct reference aircraft a stable survey needs (default 10, 2..10000). |
| `suspectMinM` | number | SuspectMinM: a declared position is suspect when it is more than max(suspectMinM, suspectK x sigma) from the survey (default 100, 1..100000 m). |
| `suspectK` | number | SuspectK is that sigma factor (default 3, 1..20). |
| `bundle` | [model.AggMLATBundle](#t-model-AggMLATBundle) | Bundle tunes the hourly network bundle adjustment (zero = default). |

<a id="t-model-AggMLATTrack"></a>

#### model.AggMLATTrack

AggMLATTrack tunes the MLAT tracker (internal/mlat/track, DESIGN §9.5). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `qCv` | number | QCv is the horizontal acceleration PSD of the constant-velocity model (default 0.3 m²/s³). |
| `qCt` | number | QCt is the horizontal acceleration PSD of the turn model (default 0.5). |
| `qVert` | number | QVert is the vertical acceleration PSD (default 1). |
| `qOmega` | number | QOmega is the turn-rate acceleration PSD (default 3e-5 rad²/s³). |
| `omegaTauS` | number | OmegaTauS is the correlation time of the turn rate (default 20 s). |
| `sojournCvS` | number | SojournCvS is the mean time in the straight-flight mode (default 300 s). |
| `sojournCtS` | number | SojournCtS is the mean time in the turn mode (default 20 s). |
| `initPCt` | number | InitPCt is the turn-mode probability of a new track (default 0.1). |
| `initSigmaVelH` | number | InitSigmaVelH is the prior 1σ horizontal speed of a new track (default 150 m/s). |
| `initSigmaVelU` | number | InitSigmaVelU is the prior 1σ vertical rate (default 15 m/s). |
| `initSigmaOmega` | number | InitSigmaOmega is the prior 1σ turn rate (default 0.03 rad/s). |
| `gateProb` | number | GateProb is the probability mass of the gate (default 0.999). |
| `iterations` | integer | Iterations is the iterated-EKF relinearisations per update (default 3). |
| `confirmWindowS` | number | ConfirmWindowS is the confirmation window of a new track (default 10 s). |
| `confirmUpdates` | integer | ConfirmUpdates is the number of updates that confirm it (default 2). |
| `coastS` | number | CoastS deletes a track without updates for this long (default 60 s). |
| `maxSigmaHM` | number | MaxSigmaHM deletes a track with a larger horizontal σ (default 5000 m). |
| `maxSpeedMS` | number | MaxSpeedMS is the speed ceiling of a hypothesis (default 360 m/s). |
| `hypLR` | number | HypLR drops a competing hypothesis at this likelihood ratio (default 100). |
| `maxHyp` | integer | MaxHyp is the number of hypotheses kept per address (default 4). |
| `respawnMisses` | integer | RespawnMisses starts a competing hypothesis after this many non-gating fixes (default 3). |
| `losMarginM` | number | LOSMarginM penalises a hypothesis hidden below a hearer's radio horizon by more than this (default 3000 m). |
| `maxMembers` | integer | MaxMembers caps the stations used from one cluster (default 24, 2..32). |
| `maxExclude` | integer | MaxExclude is the number of outlier members that may be excluded from a cluster (default 2, negative = none, ..4). |
| `maxLagS` | number | MaxLagS drops measurements older than the track by more (default 2.5 s). |
| `excessRefreshM` | number | ExcessRefreshM recomputes a cached refraction excess after this movement (default 2000 m). |
| `recentreM` | number | RecentreM moves the local frame after this offset (default 5000 m). |
| `syncTauS` | number | SyncTauS is the correlation time of clock-sync errors (default 15 s). |

<a id="t-model-AggPositions"></a>

#### model.AggPositions

AggPositions rate-limits and holds the station positions that feeders announce themselves (AVF RECEIVER records, mlat-client handshakes; finding PRIV-1). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `jitterM` | number | JitterM: changes smaller than this are ignored (default 30 m). |
| `minIntervalHours` | number | MinIntervalHours: at most one applied change per station per this interval (default 24 h). |
| `holdBeyondM` | number | HoldBeyondM: moves at least this large wait for the survey, an owner confirmation or holdMaxDays (default 1000 m). |
| `holdMaxDays` | number | HoldMaxDays releases a held move announced continuously this long (default 7; negative = never). |
| `surveyMatchM` | number | SurveyMatchM: a survey estimate within max(surveyMatchM, 3σ) of a held position confirms it (default 250 m). |

<a id="t-model-Aggregator"></a>

#### model.Aggregator

Aggregator is the configuration of aggregator mode (DESIGN §1). It is used only when Config.Mode is ModeAggregator. Registry data, keys, owners, claims and enrol tokens are mutable secrets that live in stations.db, not here. Missing JSON fields take their default (see UnmarshalJSON), so a config file needs to list only the settings it changes.

| Field | Type | Notes |
|---|---|---|
| `name` | string | Name is the network's display name. |
| `publicHost` | string | PublicHost is the host name feeders use to reach this aggregator (embedded in install one-liners). "" = taken from the HTTP request. |
| `privacyNoticeUrl` | string | PrivacyNoticeURL is the address of the operator's published privacy notice (template: docs/aggregator/PRIVACY-NOTICE.md), linked from the registration form, the feeder page and the consent texts: an http(s) URL or a path on this server starting with "/" ("" = no link). |
| `avfPort` | integer | Listener ports; 0 disables a listener. AVF/1 (default 30160) |
| `beastPort` | integer | Beast compat ingest (default 30004) |
| `mlatPort` | integer | mlat-client compat ingest (default 31090) |
| `sbsInLocalPort` | integer | 127.0.0.1-only SBS bridge input (default 0) |
| `beastOutPort` | integer | merged Beast output (default 0) |
| `sbsOutPort` | integer | merged SBS output (default 0) |
| `outputToken` | string | OutputToken protects BeastOutPort / SBSOutPort ("" = outputs refuse all clients). It is a secret. |
| `admission` | string | Admission is AdmissionOpen, AdmissionApprove or AdmissionInvite (default). |
| `tlsCert` | string | TLSCert / TLSKey are PEM files of an operator certificate; empty = self-signed ECDSA P-256 certificate in <data>/avf-tls/. The key path is treated as a secret by config redaction. |
| `tlsKey` | string |  |
| `trustedProxies` | string[] | *optional* - TrustedProxies are CIDRs whose X-Forwarded-For header is honoured. |
| `publicMap` | boolean | PublicMap / PublicStats / TarJSON enable the public merged map, the public network statistics and the tar1090-shaped /data/*.json (defaults true, true, true). |
| `publicStats` | boolean |  |
| `tarJson` | boolean |  |
| `defaultPin` | string | DefaultPin is the pin precision of new stations: PinHidden (default), PinRegion or PinCell01. |
| `islandGuard` | boolean | IslandGuard coarsens low single-station aircraft heard only by hidden stations (DESIGN §8.6; default false). |
| `trustedRelays` | string[] | *optional* - TrustedRelays are the Beast relays whose 0xE3 receiver IDs are honoured (each becomes a sub-station): "uuid@cidr" binds the relay's station UUID to the addresses it connects from; a plain "uuid" is honoured only from limits.allowCidrs. The UUID alone is no secret. |
| `listenAddr` | string | ListenAddr is the bind address of the feeder listeners (AVF, Beast, mlat-client); "" = every interface. The -listen-host flag and $AIRVIEW_LISTEN_HOST take precedence. |
| `feederReleaseDir` | string | FeederReleaseDir is a directory with signed airview-feeder releases served to the Pi installer: manifest.json, manifest.json.sig and one file per target (linux-arm64, linux-armv7, linux-amd64, ...) at /feeder/manifest.json, /feeder/manifest.json.sig and /feeder/dl/<target> ("" = not served; the installer then needs a local binary). |
| `feederManifestKey` | string | FeederManifestKey is the base64 Ed25519 public key (32 bytes) that signs manifest.json; /feeder/install.sh carries it as its preset ("" = none). |
| `mlat` | [model.AggMLAT](#t-model-AggMLAT) |  |
| `retention` | [model.AggRetention](#t-model-AggRetention) |  |
| `limits` | [model.AggLimits](#t-model-AggLimits) |  |
| `auth` | [model.AggAuth](#t-model-AggAuth) |  |
| `beast` | [model.AggBeast](#t-model-AggBeast) |  |
| `lanes` | [model.AggLanes](#t-model-AggLanes) |  |
| `storage` | [model.AggStorage](#t-model-AggStorage) |  |
| `ingest` | [model.AggIngest](#t-model-AggIngest) |  |
| `mlatc` | [model.AggMLATC](#t-model-AggMLATC) |  |
| `positions` | [model.AggPositions](#t-model-AggPositions) | Positions rate-limits and holds station positions announced by the feeders themselves (zero = default). |
| `federation` | [model.AggFederation](#t-model-AggFederation) | Federation links this network with other AirView networks and runs federation-safe outbound feeds (default off). |
| `shareLinks` | [model.AggShareLinks](#t-model-AggShareLinks) | ShareLinks limits the read-only share links of station owners (default on). |

<a id="t-model-AggRetention"></a>

#### model.AggRetention

AggRetention holds the aggregator's retention schedule (DESIGN §8.5, §20.2).

| Field | Type | Notes |
|---|---|---|
| `connLogDays` | integer | connection log (default 30, 1..365) |
| `minuteDays` | integer | per-station minute stats (default 30, 1..365) |
| `hourMonths` | integer | per-station hourly stats (default 13, 1..120) |
| `offlinePurgeDays` | integer | unclaimed + offline stations are purged after (default 90, 7..3650) |
| `auditMonths` | integer | audit log (default 24, 1..120) |
| `estimatesPerMethod` | integer | EstimatesPerMethod is how many location estimates are kept per station and method (default 5, 1..100). |
| `backupDays` | integer | BackupDays is how long the operator's backups keep deleted data (default 30, 1..3650). AirView makes no backups by itself; the value is what owners are told when they delete their data (consent text C8, privacy notice), so set it to the retention of your backup rotation. |

<a id="t-model-AggShareLinks"></a>

#### model.AggShareLinks

AggShareLinks configures aggregator.shareLinks: the read-only share links station owners create for their own stations (internal/server api_share.go). Zero = default; the links are on by default.

| Field | Type | Notes |
|---|---|---|
| `disabled` | boolean | Disabled switches share links off (default false). The share routes then answer 404, existing links stop working and the web UI hides the feature; the stored links are kept. |
| `maxPerOwner` | integer | MaxPerOwner is the number of active links one account may hold (default 20, 1..1000). |
| `maxDays` | integer | MaxDays is the longest validity of a link in days (default 365, 1..3650). |
| `defaultDays` | integer | DefaultDays is the validity of a new link when the owner does not choose one (default 30, 1..maxDays). |
| `exactLowDefault` | boolean | ExactLowDefault makes new links show low aircraft (below 1000 ft AGL and on the ground) at their exact position unless the owner chooses otherwise (default false: new links round them to the island-guard grid). The owner can still choose per link. |

<a id="t-model-AggStorage"></a>

#### model.AggStorage

AggStorage holds the tiered retention and maintenance settings of the aggregator's databases (airview.db and stations.db; DESIGN §20.2, §20.3). They map onto store.TierPolicy.

| Field | Type | Notes |
|---|---|---|
| `minuteDays` | integer | MinuteDays keeps minute statistics this long (0 = retention.minuteDays, else 1..3650). |
| `hourDays` | integer | HourDays keeps hourly statistics this long (0 = 30 x retention.hourMonths, else 7..3650). |
| `dayDays` | integer | DayDays keeps the daily totals this long (default 0 = forever, else 1..36500). |
| `histDailyDays` | integer | HistDailyDays keeps histograms per day this long before they are merged into month buckets (default 35, 1..3650). |
| `histDays` | integer | HistDays deletes histogram rows older than this (default 395, 0 = forever). |
| `heatIdleDays` | integer | HeatIdleDays deletes heat cells not updated for this long (default 395, 0 = forever). |
| `flightStationDays` | integer | FlightStationDays deletes the station links of flights that ended longer ago (default 90; 0 = keep them as long as the flights). |
| `trailDays` | integer | TrailDays keeps the stored position trails (the positions table of airview.db, the largest table of a busy aggregator: about 0.6 GB per 1000 aircraft and day) of flights that ended this long ago; it replaces storage.trailRetentionDays in aggregator mode (default 7, 1..3650). |
| `trailFullDays` | integer | TrailFullDays keeps the stored trails of flights at full resolution for this many days after the flight ended, then thins them to one point per TrailThinSec (default 2, 0..3650; 0 = never thin). |
| `trailThinSec` | integer | TrailThinSec is the point interval of thinned trails in seconds (default 30, 0 or 2..3600; 0 = never thin). |
| `freePct` | integer | FreePct releases free pages when more than this share of a file is free (default 10, 1..90 %). |
| `walCheckpointMb` | integer | WALCheckpointMB truncates a write-ahead log larger than this (default 64, 4..4096 MiB). |
| `optimizeHours` | integer | OptimizeHours is the interval of PRAGMA optimize (default 1, 1..720 h; bounded by analysis_limit, so an hourly run costs milliseconds). |
| `queueRows` | integer | QueueRows bounds the write queue of stations.db in rows (default 2000000, 10000..50000000). |

<a id="t-model-Aircraft"></a>

#### model.Aircraft

Aircraft is the live state DTO for one tracked aircraft (GET /api/aircraft, SSE "aircraft" event). Nil / omitted = unknown.

| Field | Type | Notes |
|---|---|---|
| `icao` | string | 6 upper-case hex digits |
| `addrType` | string | *optional* - "icao" \| "non-icao" \| "tisb-track" \| "anon" \| "modeac" (Mode A/C MLAT target, ICAO "#xxxxxx") |
| `callsign` | string | *optional* |
| `squawk` | string | *optional* - 4 octal digits |
| `category` | string | *optional* - ADS-B emitter category e.g. "A3" |
| `reg` | string | *optional* - Reference data (from the optional aircraft database / ICAO allocation tables). |
| `type` | string | *optional* - ICAO type designator e.g. "A20N" |
| `typeDesc` | string | *optional* - e.g. "Airbus A320neo" |
| `operator` | string | *optional* |
| `country` | string | *optional* - state of registration by ICAO address block |
| `countryCode` | string | *optional* - ISO 3166-1 alpha-2 |
| `military` | boolean | *optional* |
| `lat` | number | *optional* - Position. |
| `lon` | number | *optional* |
| `source` | string | Src* constant: best current position/data source |
| `posAge` | number | *optional* - seconds since last position |
| `onGround` | boolean | *optional* |
| `altBaro` | number | *optional* - Vertical. ft |
| `altGeom` | number | *optional* - ft (GNSS) |
| `vertRate` | number | *optional* - ft/min |
| `selAlt` | number | *optional* - selected altitude MCP/FCU or FMS, ft |
| `qnh` | number | *optional* - hPa |
| `gs` | number | *optional* - Horizontal. ground speed kt |
| `track` | number | *optional* - true track deg |
| `ias` | number | *optional* - kt |
| `tas` | number | *optional* - kt |
| `mach` | number | *optional* |
| `magHeading` | number | *optional* - deg magnetic |
| `roll` | number | *optional* - deg, + = right wing down |
| `trackRate` | number | *optional* - deg/s |
| `selHeading` | number | *optional* - deg |
| `windDir` | number | *optional* - Derived meteorology (Mode S EHS / MRAR). deg true, direction wind blows FROM |
| `windSpd` | number | *optional* - kt |
| `oat` | number | *optional* - static air temperature °C |
| `adsbVersion` | integer | *optional* - Integrity / status. |
| `nic` | integer | *optional* |
| `nacp` | integer | *optional* |
| `nacv` | integer | *optional* |
| `sil` | integer | *optional* |
| `rcM` | number | *optional* - containment radius metres |
| `emergency` | string | *optional* - "", "general", "medical", "minfuel", "nordo", "unlawful", "downed" |
| `alert` | boolean | *optional* |
| `spi` | boolean | *optional* |
| `navModes` | string[] | *optional* - "autopilot","vnav","althold","approach","lnav","tcas" |
| `distKm` | number | *optional* - Geometry relative to the receiver (only when receiver is located and aircraft has a position). great-circle ground distance |
| `bearing` | number | *optional* - deg true from receiver to aircraft |
| `elevation` | number | *optional* - deg above the receiver's horizon (4/3 earth) |
| `slantKm` | number | *optional* |
| `rssi` | number | *optional* - Reception. last frame dBFS |
| `rssiAvg` | number | *optional* - smoothed dBFS |
| `msgs` | integer | frames received this session |
| `msgRate` | number | frames/s (smoothed) |
| `antennas` | string[] | *optional* - antenna IDs that heard it recently |
| `feeds` | string[] | *optional* - feed IDs that heard it recently |
| `firstSeen` | integer | Unix ms |
| `lastSeen` | integer | Unix ms |
| `flightId` | integer | *optional* |
| `predicted` | boolean | *optional* - Prediction (internal/trajectory). When Predicted is true, Lat/Lon (and DistKm/Bearing/Elevation/SlantKm) are the position the tracking filter predicts for now, because no real position arrived for more than PredictAfterSec seconds; the aircraft is shown like this for at most config.prediction.coastSec seconds after its last real position. Predicted positions are never stored, never enter trails, statistics, coverage or records. |
| `coastS` | number | *optional* - seconds since the last real position (only while predicted) |
| `predSigmaM` | number | *optional* - 1-sigma horizontal uncertainty of the shown position, m (only while predicted) |
| `lastRealLat` | number | *optional* - last real (received) position, only while predicted |
| `lastRealLon` | number | *optional* |
| `cpaKm` | number | *optional* - predicted closest approach to the receiver, km (within 30 min) |
| `cpaInS` | number | *optional* - seconds until that closest approach (0 = now / moving away) |
| `terrainVisible` | boolean | *optional* - Terrain line of sight from the receiver (internal/terrain), refreshed at 1 Hz. above the terrain radio horizon of the best antenna; omitted until the terrain analysis exists |
| `losMarginDeg` | number | *optional* - elevation above (+) or below (-) the terrain skyline, deg |
| `route` | [model.RouteShort](#t-model-RouteShort) | *optional* - Route from the offline standing data (or a cached online lookup). |
| `acas` | [model.ACASMark](#t-model-ACASMark) | *optional* - ACAS is set while the aircraft reports an active ACAS (TCAS) resolution advisory (internal/acas). |
| `highlight` | [model.AlertMark](#t-model-AlertMark) | *optional* - Highlight is set while an alert rule with a highlight colour matches this aircraft (the JSON name "alert" is the Mode S alert flag above). |
| `mlatGrade` | string | *optional* - MLATGrade is the grade "A".."C" of the current own-MLAT position. |
| `mlatSigmaM` | number | *optional* - MLATSigmaM is the 1-sigma horizontal uncertainty of an MLAT position, m. |
| `mine` | boolean | *optional* - Mine is set for an owner when one of the owner's stations heard the aircraft in the last 60 s (owner/admin scope only). |
| `stations` | string[] | *optional* - Stations lists the SIDs of stations that heard the aircraft recently: only the viewer's own stations for an owner, all for an admin, never in public scope. |

<a id="t-model-AlertMark"></a>

#### model.AlertMark

AlertMark marks an aircraft that matches an alert rule with a highlight.

| Field | Type | Notes |
|---|---|---|
| `ruleId` | string |  |
| `color` | string | CSS colour of the highlight |
| `label` | string | rule name |
| `severity` | string | "info" \| "warning" \| "critical" |

<a id="t-model-AlertsConfig"></a>

#### model.AlertsConfig

AlertsConfig controls the alert rules engine (internal/alerts).

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | evaluate rules (default true) |
| `sound` | boolean | alerts may play a sound in the UI (default true) |
| `webhooks` | boolean | Webhooks allows rules to POST to their webhook URL (default true; off = no webhook is sent). |

<a id="t-model-Antenna"></a>

#### model.Antenna

Antenna describes one physical antenna and its RF chain.

| Field | Type | Notes |
|---|---|---|
| `id` | string | stable slug, unique |
| `name` | string |  |
| `type` | string | Ant* constants |
| `channel` | integer | receiver RF channel index this antenna is plugged into (0-based), -1 = n/a |
| `gainDbi` | number | peak gain |
| `heightAglM` | number | feed-point height above ground, metres |
| `azimuthDeg` | number | Directional parameters (yagi/patch). Azimuth is the boresight in degrees true. |
| `tiltDeg` | number | boresight elevation above horizon |
| `hBeamwidthDeg` | number | -3 dB horizontal beamwidth |
| `vBeamwidthDeg` | number | -3 dB vertical beamwidth |
| `frontToBackDb` | number |  |
| `elements` | integer | yagi element count (informational / used to estimate beamwidth when 0) |
| `polarization` | string | "vertical" (ADS-B) \| "horizontal" |
| `cableLossDb` | number | feedline loss before the first amplifier |
| `lnaGainDb` | number |  |
| `lnaNoiseFigDb` | number |  |
| `filterLossDb` | number |  |
| `notes` | string |  |
| `color` | string | UI colour for this antenna's overlays (hex) |

<a id="t-model-Config"></a>

#### model.Config

Config is the complete persisted configuration (config.json in the data dir). GET/PUT /api/config exchange exactly this shape.

| Field | Type | Notes |
|---|---|---|
| `version` | integer |  |
| `mode` | string |  |
| `http` | [model.HTTPConfig](#t-model-HTTPConfig) |  |
| `receiver` | [model.Receiver](#t-model-Receiver) |  |
| `antennas` | [model.Antenna](#t-model-Antenna)[] |  |
| `feeds` | [model.Feed](#t-model-Feed)[] |  |
| `ntp` | [model.NTPConfig](#t-model-NTPConfig) |  |
| `weather` | [model.WeatherConfig](#t-model-WeatherConfig) |  |
| `hub` | [model.HubConfig](#t-model-HubConfig) |  |
| `remote` | [model.RemoteConfig](#t-model-RemoteConfig) |  |
| `storage` | [model.StorageConfig](#t-model-StorageConfig) |  |
| `ui` | [model.UIConfig](#t-model-UIConfig) |  |
| `demo` | [model.DemoConfig](#t-model-DemoConfig) |  |
| `prediction` | [model.PredictionConfig](#t-model-PredictionConfig) |  |
| `terrain` | [model.TerrainConfig](#t-model-TerrainConfig) |  |
| `navData` | [model.NavDataConfig](#t-model-NavDataConfig) |  |
| `alerts` | [model.AlertsConfig](#t-model-AlertsConfig) |  |
| `rfStat` | [model.RFStatConfig](#t-model-RFStatConfig) |  |
| `uplinks` | [model.Uplink](#t-model-Uplink)[] | *optional* - Uplinks feed this station to AirView aggregators over AVF/1 (standalone and aggregator modes; omitted from config.json when empty). |
| `uplinkTuning` | [model.UplinkTuning](#t-model-UplinkTuning) | UplinkTuning holds the global tunables of the uplink client (ring, backfill, backoff, spool segments); they apply to every uplink. |
| `aggregator` | [model.Aggregator](#t-model-Aggregator) | Aggregator holds the aggregator-mode settings (used only when Mode is ModeAggregator). Stand-alone configs carry the defaults; a document without this key decodes to the defaults when decoded over DefaultConfig(), and a zero block is restored by Aggregator.Normalize. |
| `update` | [model.UpdateSettings](#t-model-UpdateSettings) | Update is the signed self-update of this program (opt-in; see internal/selfupdate). |
| `locate` | [model.LocateConfig](#t-model-LocateConfig) | Locate controls Locate Me v2: the guess samples recorded to estimate the receiver position from what it hears. |
| `ssr` | [model.SSRConfig](#t-model-SSRConfig) | SSR is the experimental bistatic SSR self-location of this receiver (off by default; owner/admin only, never applied automatically). |
| `process` | [model.ProcessConfig](#t-model-ProcessConfig) | Process holds the log file and memory settings of the server process (applied at start). |

<a id="t-model-DemoConfig"></a>

#### model.DemoConfig

DemoConfig enables the built-in traffic simulator (no receiver needed).

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean |  |
| `aircraft` | integer | simulated aircraft count, default 60 |

<a id="t-model-Feed"></a>

#### model.Feed

Feed is one data input.

| Field | Type | Notes |
|---|---|---|
| `id` | string | stable slug, unique |
| `name` | string |  |
| `enabled` | boolean |  |
| `type` | string | Feed* constants |
| `host` | string |  |
| `port` | integer |  |
| `url` | string | for FeedJSON |
| `antennaId` | string | AntennaID is the antenna this feed's frames are attributed to when the protocol does not carry a channel number. "" = unknown/combined. |
| `mlatOnly` | boolean | MLATOnly marks a feed that only carries MLAT results. |
| `notes` | string |  |
| `noForward` | boolean | *optional* - NoForward keeps this feed's data off every uplink (DESIGN §1). Feeds with MLATOnly are never forwarded regardless of this flag. |
| `clockClass` | string | *optional* - ClockClass overrides the detected receiver clock class for uplinks (ClockClass.String() names; "" = detect). |
| `sdr` | [model.FeedSDR](#t-model-FeedSDR) | SDR tunes a feed of type "rtltcp" (an RTL-SDR dongle through rtl_tcp; ignored for the other types). |

<a id="t-model-FeedSDR"></a>

#### model.FeedSDR

FeedSDR tunes an rtltcp feed (Feed.SDR). Zero = default.

| Field | Type | Notes |
|---|---|---|
| `gain` | string | *optional* - Gain is "max" (default: the highest manual gain step), "auto" (the tuner's AGC, not recommended for Mode S) or a manual gain in dB such as "40.2". |
| `ppm` | integer | *optional* - PPM is the oscillator frequency correction (-500..500). |
| `biasTee` | boolean | *optional* - BiasTee switches on the antenna supply of dongles that have one. |
| `sampleRate` | integer | *optional* - SampleRate is the complex sample rate in samples per second (default 2400000, 2000000..3200000). |
| `spawn` | boolean | *optional* - Spawn starts the user's own rtl_tcp as a child process (found on PATH or next to the program, or at RTLTCPPath). AirView never ships or downloads rtl_tcp. |
| `rtltcpPath` | string | *optional* - RTLTCPPath is the rtl_tcp executable for Spawn ("" = search). |
| `device` | integer | *optional* - Device is the dongle index passed to a spawned rtl_tcp (0..31). |
| `acceptUnverifiedAp` | boolean | *optional* - AcceptUnverifiedAP passes address/parity replies whose address was not confirmed by a recent all-call or ADS-B message (default false). |

<a id="t-model-FeedStatus"></a>

#### model.FeedStatus

FeedStatus is the runtime state of one feed.

| Field | Type | Notes |
|---|---|---|
| `id` | string |  |
| `name` | string |  |
| `type` | string |  |
| `addr` | string |  |
| `enabled` | boolean |  |
| `connected` | boolean |  |
| `since` | integer | Unix ms of last state change |
| `frames` | integer |  |
| `bytes` | integer |  |
| `msgRate` | number |  |
| `lastFrame` | integer | Unix ms |
| `lastError` | string | *optional* |
| `gpsTime` | boolean | GPS timestamp mode detected (Radarcape) and last receiver status text, if any. |
| `info` | string | *optional* |

<a id="t-model-HTTPConfig"></a>

#### model.HTTPConfig

HTTPConfig configures the built-in web server.

| Field | Type | Notes |
|---|---|---|
| `port` | integer | default 80; if busy the server falls back to 8080, 8081.. then an ephemeral port |
| `bindAddr` | string | BindAddr "" = all interfaces. "127.0.0.1" = local only. |
| `adminPasswordHash` | string | AdminPasswordHash protects mutating endpoints (config, factory reset) for non-loopback clients when non-empty. Format: "pbkdf2-sha256$<iter>$<salt>$<key>" (the former "sha256:<hex salt>:<hex hash>" is still accepted). |
| `tileCache` | boolean | cache map tiles on disk via /tilecache |
| `allowInsecureLogin` | boolean | *optional* - AllowInsecureLogin (aggregator mode) accepts passwords, TOTP codes and the setup token from remote clients over plain HTTP. Off by default: credentials are accepted only over HTTPS (a trusted reverse proxy that sets X-Forwarded-Proto) or from the machine itself. |
| `allowOpenAdmin` | boolean | *optional* - AllowOpenAdmin (standalone mode, no admin password) makes every client admin, also those outside the private networks. Off by default: without a password only this machine and private-network (RFC 1918, IPv6 ULA, link-local) clients are admin. |
| `sseGzipMax` | integer | *optional* - SSEGzipMax caps the live streams (/api/live) that are gzip-compressed at once; each holds a compressor of about 800 KiB (0..100000; 0 = automatic: 12 in standalone mode, every stream in aggregator mode). Streams above the cap are sent uncompressed. |

<a id="t-model-HubConfig"></a>

#### model.HubConfig

HubConfig configures the data port that other AirView instances log in to.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean |  |
| `port` | integer | default 30154 |
| `password` | string | shared secret for the HMAC challenge login; "" = open |

<a id="t-model-HubStatus"></a>

#### model.HubStatus

HubStatus is the runtime state of the data port.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean |  |
| `port` | integer |  |
| `clients` | integer |  |

<a id="t-model-LiveCounts"></a>

#### model.LiveCounts

LiveCounts summarises the live picture.

| Field | Type | Notes |
|---|---|---|
| `aircraft` | integer |  |
| `withPos` | integer |  |
| `adsb` | integer |  |
| `mlat` | integer |  |
| `tisb` | integer |  |
| `modes` | integer |  |
| `msgRate` | number | frames/s all feeds |
| `posRate` | number | decoded positions/s |
| `maxRangeKm` | number | farthest live aircraft (real positions only) |
| `predicted` | integer | aircraft shown at a predicted position (not counted in withPos) |

<a id="t-model-LiveSnapshot"></a>

#### model.LiveSnapshot

LiveSnapshot is the payload of GET /api/aircraft and the SSE "aircraft" event.

| Field | Type | Notes |
|---|---|---|
| `now` | integer | Unix ms |
| `aircraft` | [model.Aircraft](#t-model-Aircraft)[] |  |
| `counts` | [model.LiveCounts](#t-model-LiveCounts) |  |

<a id="t-model-LocateConfig"></a>

#### model.LocateConfig

LocateConfig controls Locate Me v2 (internal/analytics locate2): the guess samples the tracker records so the receiver position can be estimated from what it hears.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | Enabled records guess samples and runs Locate Me v2 (default true). |
| `retentionDays` | integer | RetentionDays keeps guess samples this long (default 7, 1..30). |
| `maxRowsPerStation` | integer | MaxRowsPerStation caps one station's guess samples (default 2000000, 10000..20000000). |
| `maxRowsTotal` | integer | MaxRowsTotal caps all guess samples of a database (default 10000000, 100000..200000000). |

<a id="t-model-NavDataConfig"></a>

#### model.NavDataConfig

NavDataConfig controls aviation reference data (internal/navdata). Nothing is downloaded automatically: imports run only on request.

| Field | Type | Notes |
|---|---|---|
| `airportsOverlay` | boolean | airports layer offered on the map (default true) |
| `autoRefresh` | boolean | AutoRefresh downloads the reference data (airports, runways, navaids, countries, VRS standing-data routes; CC0 / public domain, about 25 MB, no user data sent) at first start and checks it daily at a per-install random time with a conditional GET (default true; off for air-gapped sites). |
| `photos` | boolean | Photos shows aircraft photos from planespotters.net (sends the ICAO address; default false). Requires PhotoContact. |
| `photoContact` | string | contact URL or e-mail sent in the User-Agent (required by planespotters.net) |
| `routes` | [model.RoutesConfig](#t-model-RoutesConfig) | route look-ups, learning and overrides |

<a id="t-model-NTPConfig"></a>

#### model.NTPConfig

NTPConfig configures the SNTP reference used for timing analysis.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean |  |
| `server` | string | host or host:port; "" = disabled |
| `intervalSec` | integer | sampling interval, default 64 |

<a id="t-model-NTPStatus"></a>

#### model.NTPStatus

NTPStatus is the latest NTP measurement summary.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean |  |
| `server` | string |  |
| `synced` | boolean |  |
| `offsetMs` | number | true UTC - system clock |
| `delayMs` | number | round-trip |
| `jitterMs` | number |  |
| `stratum` | integer |  |
| `refId` | string | *optional* |
| `lastOk` | integer | Unix ms |
| `lastError` | string | *optional* |

<a id="t-model-PredictionConfig"></a>

#### model.PredictionConfig

PredictionConfig controls the trajectory filter (internal/trajectory): coasting aircraft on the live map and predicted paths.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | run the tracking filter (default true) |
| `coastSec` | integer | CoastSec is how long an aircraft without new positions is shown at its predicted position (default 60, 5..120). Predicted positions start PredictAfterSec (4 s) after the last real position. |
| `pathSec` | integer | PathSec is the default horizon of predicted paths (default 120, 10..900). |
| `showPaths` | boolean | ShowPaths: the map draws the predicted path of the selected aircraft (default true). |

<a id="t-model-ProcessConfig"></a>

#### model.ProcessConfig

ProcessConfig ("process") holds process-level settings of the airview server: the log file and the soft memory limit (DESIGN §20.3: every setting file-configurable). They are applied when the server starts.

| Field | Type | Notes |
|---|---|---|
| `logLevel` | string | *optional* - LogLevel is the lowest level written to the log: debug, info (default), warn or error. AIRVIEW_DEBUG=1 still forces debug. |
| `logMaxMb` | integer | *optional* - LogMaxMB is the size (MiB) at which airview.log is rotated (1..4096; 0 = automatic: 4 in standalone mode, 64 in aggregator mode). |
| `logFiles` | integer | *optional* - LogFiles is how many rotated files (airview.log.1, .2, ...) are kept (1..100; 0 = automatic: 1 in standalone mode, 10 in aggregator mode). |
| `memoryLimitMb` | integer | *optional* - MemoryLimitMB is the soft Go heap limit in MiB (64..1048576; 0 = automatic: 384 on Raspberry Pi class Linux devices in standalone mode; in aggregator mode 85 % of a cgroup memory cap, else 80 % of the physical memory; -1 = none). GOMEMLIMIT wins over it. |
| `journalLevel` | string | *optional* - JournalLevel is the lowest level copied to standard error, which systemd stores in its journal: debug, info, warn or error ("" = automatic: warn when started by systemd, so the SD card does not get every line twice, else the same as logLevel). airview.log is not affected. |

<a id="t-model-Receiver"></a>

#### model.Receiver

Receiver describes the receiving station.

| Field | Type | Notes |
|---|---|---|
| `name` | string |  |
| `model` | string | e.g. "Radarcape EVO 2CH" |
| `located` | boolean | false until the user (or the guesser) sets coordinates |
| `lat` | number | WGS84 degrees |
| `lon` | number |  |
| `groundElevM` | number | GroundElevM is the terrain elevation at the site, metres above mean sea level. |

<a id="t-model-ReceiverGPS"></a>

#### model.ReceiverGPS

ReceiverGPS is a receiver-reported station position.

| Field | Type | Notes |
|---|---|---|
| `feedId` | string |  |
| `lat` | number |  |
| `lon` | number |  |
| `altM` | number | metres (as reported by the receiver) |
| `t` | integer | Unix ms of the report |

<a id="t-model-RemoteConfig"></a>

#### model.RemoteConfig

RemoteConfig is used by the desktop app in remote-data / remote-web modes.

| Field | Type | Notes |
|---|---|---|
| `host` | string | AirView host (e.g. the Raspberry Pi) |
| `dataPort` | integer | host's hub port, default 30154 |
| `httpPort` | integer | host's website port, default 80 |
| `password` | string | hub password |
| `useHostReceiver` | boolean | adopt the host's receiver/antenna settings |

<a id="t-model-RFStatConfig"></a>

#### model.RFStatConfig

RFStatConfig controls the RF / protocol statistics (internal/rfstat).

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | default true |

<a id="t-model-RoutesConfig"></a>

#### model.RoutesConfig

RoutesConfig controls route resolution (internal/routes).

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | master switch of the route feature (default true) |
| `online` | boolean | Online looks callsigns up at adsbdb.com when no offline route (override, learned, standing data) exists (default true). It sends the callsign, never the receiver location; answers are only cached, never merged into AirView's own tables, and shown only to the station's own users. |
| `dailyCap` | integer | online look-ups per 24 h (default 1000) |
| `stableSec` | integer | a callsign must be received this long before it is looked up online (default 45) |
| `contact` | string | contact URL or e-mail in the User-Agent of online look-ups (default: the project URL) |
| `learn` | boolean | learn routes from observed take-offs and landings (default true) |
| `eta` | boolean | estimate the arrival time (default true) |

<a id="t-model-RouteShort"></a>

#### model.RouteShort

RouteShort is the compact route of a flight in the live picture. The airport fields describe the ACTIVE leg of a multi-stop route.

| Field | Type | Notes |
|---|---|---|
| `from` | string | departure airport code of the active leg (ICAO, else IATA / ident) |
| `to` | string | destination airport code of the active leg |
| `fromIata` | string | *optional* |
| `toIata` | string | *optional* |
| `fromName` | string | *optional* - short airport name / city |
| `toName` | string | *optional* |
| `via` | integer | *optional* - number of intermediate stops of the whole route |
| `plausible` | boolean | the current position fits the route (state fits or uncertain) |
| `source` | string | "override" \| "learned" \| "standing-data" \| "adsbdb" |
| `checked` | boolean | *optional* - plausibility could be checked (position and both airports known) |
| `state` | string | *optional* - fits \| uncertain \| doubtful \| reversed \| unchecked |
| `conf` | number | *optional* - fused confidence 0..1 (2 dp) |
| `reason` | string | *optional* - one line, no station identifiers |
| `stops` | string[] | *optional* - full ordered route codes when more than 2 airports |
| `leg` | integer | *optional* - 1-based active leg (multi-stop routes) |
| `legs` | integer | *optional* - number of legs (multi-stop routes) |
| `along` | number | *optional* - progress along the active leg, 0..100 |
| `distRem` | number | *optional* - remaining distance on the active leg, km |
| `eta` | integer | *optional* - estimated arrival at the active leg's end, Unix s (rounded to 30 s) |
| `etaSigma` | integer | *optional* - 1-sigma ETA uncertainty, s |
| `kind` | string | *optional* - "" \| "positioning" \| "charter" |
| `lic` | string | *optional* - licence class: open \| own \| restricted (restricted never leaves the station's own scopes) |
| `flags` | string[] | *optional* - "variant", "stale-standing" |

<a id="t-model-SSRConfig"></a>

#### model.SSRConfig

SSRConfig configures the EXPERIMENTAL bistatic SSR self-location of this receiver (top-level "ssr" block; internal/mlat/ssr, docs/aggregator/SSR-RESEARCH.md). Off by default. Its result is shown to the station owner / admin only and is never used to set, confirm or replace a station position (DESIGN §8, §19.3); at most it may seed a survey search. Every numeric field: 0 = the package default. The Go field names equal those of ssr.Config; the engine maps them by name (internal/engine/ssrwire.go, tested).

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | Enabled turns the estimator on (default false, experimental). |
| `feedId` | string | *optional* - FeedID is the local feed whose receiver timestamps are used ("" = the first feed that delivers timestamped frames). One receiver clock only: replies of other feeds are ignored. |
| `solveEveryMin` | integer | SolveEveryMin is how often the position is solved in the background (default 10, 1..1440 minutes; a solve takes about 0.3 s). |
| `sites` | [model.SSRSite](#t-model-SSRSite)[] | *optional* - Sites are known interrogator (radar) positions. Empty = learn them from the data. |
| `conspicuityCodes` | string[] | *optional* - ConspicuityCodes are Mode A codes (4 octal digits) shared by many aircraft and never used to identify a Mode A/C reply (default 7000, 2000, 1200, 0000, 7777, 1000). |
| `modeAcCodeFormat` | string | *optional* - ModeACCodeFormat is the layout of the receiver's 2-byte Mode A/C payloads: "" or "pulses" (reply pulses in transmission order, the Beast default) or "nibbles" (one octal digit per nibble). |
| `dwellGapS` | number | DwellGapS is the silence that ends a reply burst (default 0.05, 0.001..1 s). |
| `processLagS` | number | ProcessLagS: replies are processed this far behind the newest input (default 2, 0.1..30 s). |
| `maxDwellS` | number | MaxDwellS caps a reply burst (default 0.5, 0.01..5 s). |
| `matchTolNs` | number | MatchTolNs is the half width of the reply-train match window (default 400, 10..5000 ns). |
| `minMatches` | integer | MinMatches is the minimum common interrogations of a pair dwell (default 4, 2..100). |
| `ambiguityRatio` | number | AmbiguityRatio rejects a pair whose alias peak reaches this fraction of the best peak (default 0.6, 0.01..1). |
| `maxPairSepM` | number | MaxPairSepM bounds the separation of two paired aircraft (default 250000, 1000..1000000 m). |
| `timingSigmaACNs` | number | TimingSigmaACNs is the per-reply timing noise of Mode A/C replies (default 120, 1..5000 ns; about 180 for RTL-SDR receivers). |
| `timingSigmaSNs` | number | TimingSigmaSNs is the per-reply timing noise of Mode S replies (default 60, 1..5000 ns). |
| `trackSigmaM` | number | TrackSigmaM is the position noise of a reference aircraft without NACp (default 20, 0.1..10000 m). |
| `floorSigmaM` | number | FloorSigmaM is added to every observation (default 5, 0.01..1000 m). |
| `biasSigmaACNs` | number | BiasSigmaACNs is the prior of a Mode A/C reply-delay offset (default 300, 1..10000 ns). |
| `biasSigmaSNs` | number | BiasSigmaSNs is the prior of a Mode S reply-delay offset (default 150, 1..10000 ns). |
| `maxObs` | integer | MaxObs bounds the stored pair observations (default 60000, 1000..10000000). |
| `maxAircraft` | integer | MaxAircraft bounds the solved airframe biases (default 600, 10..100000). |
| `minObs` | integer | MinObs gates a result: minimum observations (default 50, 1..1000000). |
| `minAircraft` | integer | MinAircraft gates a result: minimum airframes (default 10, 1..10000). |
| `minRadarObs` | integer | MinRadarObs gates a result: observations per interrogator (default 20, 1..1000000). |
| `radarRegionM` | number | RadarRegionM is the half size of the interrogator search square (default 500000, 10000..2000000 m). |
| `radarCellM` | number | RadarCellM is the Hough cell of the site learning (default 4000, 100..100000 m). |
| `minLineSepM` | number | MinLineSepM is the minimum pair separation for a bearing line (default 15000, 100..1000000 m). |
| `minRadarLines` | integer | MinRadarLines is the minimum bearing lines of a learned interrogator (default 25, 2..100000). |
| `minRadarSpreadDeg` | number | MinRadarSpreadDeg is the minimum azimuth spread of those lines (default 10, 0.1..180 degrees). |
| `maxRadars` | integer | MaxRadars bounds the learned interrogators (default 16, 1..256). |
| `radarNmsM` | number | RadarNMSM is the peak suppression radius (default 30000, 100..1000000 m). |
| `beamwidthDeg` | number | BeamwidthDeg is the assumed beam width incl. margin (default 3, 0.1..20 degrees). |
| `assignMinTolM` | number | AssignMinTolM is the minimum assignment tolerance (default 3000, 1..1000000 m). |
| `radarHeightM` | number | RadarHeightM is the assumed height of a learned interrogator above the ellipsoid (default 60, -500..9000 m). |
| `radarHeightSigmaM` | number | RadarHeightSigmaM is its prior 1-sigma (default 300, 1..10000 m). |
| `learnedSiteMinSigmaM` | number | LearnedSiteMinSigmaM floors a learned site's horizontal prior (default 1500, 1..1000000 m). |
| `maxLineSigmaM` | number | MaxLineSigmaM: bearing lines vote only below this expected miss (default 10000, 10..1000000 m). |
| `rotWindowS` | number | RotWindowS is the rotation-check phase window (default 60, 1..3600 s). |
| `minPeriodS` | number | MinPeriodS is the shortest radar rotation period searched (default 2, 0.5..60 s). |
| `maxPeriodS` | number | MaxPeriodS is the longest one (default 15, 1..120 s). |
| `minCoherence` | number | MinCoherence is the rotation coherence a learned interrogator needs (default 0.8, 0.01..1). |
| `phaseTolDeg` | number | PhaseTolDeg is the beam-phase allowance (default 2, 0.01..90 degrees). |
| `latencySigmaS` | number | LatencySigmaS is the prior of a per-airframe ADS-B latency (default 0.3, 0.001..10 s). |
| `noLatency` | boolean | NoLatency disables the per-airframe latency parameters. |
| `searchRadiusM` | number | SearchRadiusM is the seed grid radius without a hint (default 300000, 1000..2000000 m). |
| `seedMaxObs` | integer | SeedMaxObs is the observations used by the seed grid (default 4000, 100..1000000). |
| `heightSigmaM` | number | HeightSigmaM is the height prior without a hint (default 1000, 1..100000 m). |
| `huberK` | number | HuberK is the robust-solve threshold (default 2.5, 0.1..100). |
| `outlierZ` | number | OutlierZ rejects observations beyond this normalised residual (default 5, 1..100). |
| `maxIter` | integer | MaxIter bounds the solver iterations (default 25, 1..1000). |
| `maxSigmaHM` | number | MaxSigmaHM: a solution with a larger horizontal 1-sigma is reported but not OK (default 500, 1..1000000 m). |

<a id="t-model-SSRSite"></a>

#### model.SSRSite

SSRSite is one known interrogator (ssr.sites[]).

| Field | Type | Notes |
|---|---|---|
| `name` | string | Name is a label. |
| `lat` | number | Lat is the WGS-84 latitude in degrees. |
| `lon` | number | Lon is the WGS-84 longitude in degrees. |
| `heightM` | number | HeightM is the antenna height above the ellipsoid (-500..9000 m). |
| `sigmaHM` | number | *optional* - SigmaHM is the horizontal 1-sigma of the position (0 = 50 m). |
| `sigmaUM` | number | *optional* - SigmaUM is the vertical 1-sigma (0 = radarHeightSigmaM). |

<a id="t-model-StationInfo"></a>

#### model.StationInfo

StationInfo is the effective station position reported in Status.

| Field | Type | Notes |
|---|---|---|
| `lat` | number \| null |  |
| `lon` | number \| null |  |
| `source` | string | "config" \| "gps" \| "none" |

<a id="t-model-Status"></a>

#### model.Status

Status is the payload of GET /api/status and the SSE "status" event.

| Field | Type | Notes |
|---|---|---|
| `app` | string |  |
| `version` | string |  |
| `platform` | string | GOOS/GOARCH |
| `gui` | boolean | running inside the desktop window |
| `now` | integer |  |
| `started` | integer |  |
| `mode` | string |  |
| `httpPort` | integer | actual bound port |
| `located` | boolean |  |
| `demo` | boolean |  |
| `tileCache` | boolean |  |
| `feeds` | [model.FeedStatus](#t-model-FeedStatus)[] |  |
| `counts` | [model.LiveCounts](#t-model-LiveCounts) |  |
| `dbBytes` | integer |  |
| `dataDir` | string |  |
| `ntp` | [model.NTPStatus](#t-model-NTPStatus) |  |
| `hub` | [model.HubStatus](#t-model-HubStatus) |  |
| `adminLocked` | boolean | AdminLocked is true when this client must authenticate before changing settings. |
| `receiverGps` | [model.ReceiverGPS](#t-model-ReceiverGPS) | *optional* - ReceiverGPS is the receiver's own GPS position when a feed reports one (Radarcape Beast type '5' frames). The UI offers it as "use receiver GPS position". |
| `station` | [model.StationInfo](#t-model-StationInfo) | Station is the effective station position every analysis uses: the located receiver (source "config"; in demo mode the simulator site), else the receiver's own GPS position ("gps"), else none ("none", lat / lon null). |
| `tileCacheHosts` | string[] | *optional* - TileCacheHosts lists the upstream map hosts the /tilecache/ proxy accepts. |

<a id="t-model-StorageConfig"></a>

#### model.StorageConfig

StorageConfig controls database retention and how the database writes to disk (the storage profile, see storage.go).

| Field | Type | Notes |
|---|---|---|
| `trailIntervalSec` | integer | min seconds between stored trail points per aircraft (default 5) |
| `trailRetentionDays` | integer | stored position trails (default 30) |
| `flightRetentionDays` | integer | flight/session rows (default 730, 0 = forever) |
| `minuteRetentionDays` | integer | per-minute stats (default 90); hourly/daily rollups are kept forever |
| `profile` | string | *optional* - Profile selects how often the database writes to disk and how much memory its caches use: "auto" ("" is the same; server in aggregator mode, low-wear when the data directory is on an SD/MMC card or on a small ARM board, else standard), "standard", "low-wear" (fewest disk writes, for SD cards) or "server". No profile changes what is stored or shown; the power-cut loss windows are listed in docs/CONFIG.md. |
| `backupBeforeMigrate` | string | *optional* - BackupBeforeMigrate controls the backup taken before the database is converted to a new storage layout: "auto" ("" is the same; a backup when the disk has room for it, else migrate without one), "always" (no migration without a backup) or "never". |
| `tuning` | [model.StorageTuning](#t-model-StorageTuning) | Tuning overrides single values of the storage profile (advanced; 0 = the profile's value). |

<a id="t-model-StorageTuning"></a>

#### model.StorageTuning

StorageTuning ("storage.tuning") overrides single values of the storage profile. Every value 0 keeps the profile's value; the defaults suit every installation, these exist for measurements and unusual hardware.

| Field | Type | Notes |
|---|---|---|
| `flushSec` | integer | *optional* - FlushSec is the group-commit interval of the database writer in seconds (1..600; 0 = profile: standard 10, low-wear 30, server 5). |
| `incFlushSec` | integer | *optional* - IncFlushSec is how often the coverage, signal and elevation statistics of the current day are written (15..3600 s; 0 = profile: standard 60, low-wear 300). Pages always show the unwritten part. |
| `heatFlushSec` | integer | *optional* - HeatFlushSec is how often the antenna heat map increments are written (15..3600 s; 0 = profile: standard 300, low-wear 900). |
| `aircraftFlushSec` | integer | *optional* - AircraftFlushSec is how often the all-time aircraft rows (messages, positions, last seen) of the aircraft in view are rewritten (60..3600 s; 0 = profile: standard 300, low-wear 900). A new aircraft is written within 15 s, and the aircraft list, the aircraft details and the export write what is pending first, so they always show rows at most a minute old. |
| `checkpointSec` | integer | *optional* - CheckpointSec is the longest time between two WAL checkpoints (10..3600 s; 0 = profile: standard 300, low-wear 600, server 60). |
| `walSoftMB` | integer | *optional* - WalSoftMB is the WAL size in MiB that triggers a checkpoint before checkpointSec has passed (1..1024; 0 = profile: standard 40, low-wear 48, server 64). |
| `cacheMB` | integer | *optional* - CacheMB is the page cache of the database writer in MiB (1..4096; 0 = profile: standard 16, low-wear 8 (4 on 32-bit), server 64). |
| `readConns` | integer | *optional* - ReadConns is the number of database reader connections (1..64; 0 = profile: standard 4, low-wear 2 (3 with 2 GiB RAM or more), server 8). |
| `mmapMB` | integer | *optional* - MmapMB is the memory-mapped I/O size of the database in MiB (1..65536, -1 = off; 0 = profile: standard 256, low-wear 64, server 1024; off on 32-bit systems, and by default on Windows, where a mapping keeps the file from shrinking). |

<a id="t-model-TerrainConfig"></a>

#### model.TerrainConfig

TerrainConfig controls the terrain line-of-sight analysis (internal/terrain).

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | default true |
| `radiusKm` | integer | RadiusKm of the analysis around the receiver (default 450, 50..1000). |
| `zoom` | integer | Zoom of the elevation tiles: 0 = automatic (default), else 6..12. |
| `kFactor` | number | KFactor is the effective earth radius factor for radio line of sight (default 4/3, 0.5..10). |
| `autoPrefetch` | boolean | AutoPrefetch downloads the elevation tiles (AWS Terrain Tiles, about 30-40 MB for 450 km) and runs the analysis automatically once the receiver is located (default true). Off = only on request. |

<a id="t-model-UIConfig"></a>

#### model.UIConfig

UIConfig holds shared UI defaults (per-browser preferences live in localStorage).

| Field | Type | Notes |
|---|---|---|
| `units` | string | "aviation" (ft/kt/nm) \| "metric" \| "imperial" |
| `trailMinutes` | integer | live map trail length, default 15 |
| `rangeRingsKm` | integer[] | e.g. [100,200,300,400] |
| `aircraftTtlS` | integer | seconds until a silent aircraft leaves the live list (default 60) |
| `mapStyle` | string | "airview-dark" default |
| `colorTrailsBy` | string | "altitude" \| "speed" \| "signal" \| "source" |

<a id="t-model-UpdateSettings"></a>

#### model.UpdateSettings

UpdateSettings is the signed self-update block ("update") of airview and airview-feeder. It mirrors internal/selfupdate.Settings field for field (model is a leaf package; the wiring converts with a field copy). Self-update is opt-in for airview; airview-feeder maps its autoUpdate switch onto Enabled when a manifest URL and a key are set.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | Enabled turns on the periodic automatic check-and-install (default false for airview; the manual "update" command works whenever a manifest URL and a key are configured). |
| `channel` | string | Channel is "stable" (default) or "beta". |
| `manifestUrl` | string | ManifestURL is the URL of the signed release manifest, e.g. https://aggregator.example/feeder/manifest.json ("" = no updates; "{channel}" is replaced by the channel). |
| `keys` | string[] | Keys are the trusted Ed25519 release public keys (base64 of the raw 32 bytes; several allow key rotation). Public keys, not secrets. |
| `checkIntervalMin` | integer | CheckIntervalMin is the automatic check period in minutes (default 1440, 15..44640; jittered by ±25 %). |
| `healthWindowS` | integer | HealthWindowS is how long a new version must stay healthy after the restart before it is kept (default 60, 10..900 s). |
| `maxDownloadMB` | integer | MaxDownloadMB bounds a release download (default 256, 1..4096). |
| `healthUrl` | string | HealthURL is polled during the health window ("" = the program's own health endpoint). |

<a id="t-model-Uplink"></a>

#### model.Uplink

Uplink is one connection from this AirView (standalone or aggregator mode) or from airview-feeder to an AirView aggregator over AVF/1. Config.Uplinks / FeederConfig.Uplinks hold them. Missing JSON fields take the DefaultUplink values (see UnmarshalJSON), so a hand-written entry needs only "id" and "host".

| Field | Type | Notes |
|---|---|---|
| `id` | string | ID is a stable slug, unique among the uplinks. |
| `host` | string | Host is the aggregator's host name or IP address. |
| `port` | integer | Port is the aggregator's AVF/1 port (default 30160). |
| `serverPin` | string | ServerPin is the expected server key: "sha256/<base64 SHA-256 of the certificate's SubjectPublicKeyInfo>". "" verifies the certificate against the system roots instead. |
| `enrolToken` | string | EnrolToken is the admin-minted enrol token for an "invite" aggregator. It is a secret: config redaction treats it like the hub password. |
| `inputs` | string[] | *optional* - Inputs lists the local feed IDs (Config.Feeds[].ID) forwarded on this uplink. Empty = every enabled feed that delivers timed frames. |
| `sendLocation` | boolean | SendLocation sends the receiver position in the RECEIVER record (default true). Off = the aggregator must locate the station itself. |
| `shareGnss` | boolean | ShareGNSS forwards the receiver's own GNSS position (Radarcape type '5' frames) as GNSS_POS records (default false). |
| `timestamps` | boolean | Timestamps sends receiver timestamps (default true). Off = no MLAT and no timing-based self-location: only untimed data is sent. |
| `returnMlat` | boolean | ReturnMLAT asks for MLAT results for aircraft this station helped to locate (default true). |
| `spoolMb` | integer | SpoolMB is the size of the on-disk spool written while disconnected. 0 = RAM ring only; SpoolAuto (-1, default) = 0 when the root filesystem is an SD card, 256 on other Linux hosts, 1024 on Windows. |
| `pin` | string | Pin is the requested public map pin precision (PinHidden default, PinRegion, PinCell01). |
| `publicStats` | boolean | PublicStats publishes non-locating statistics of this station (default false). |
| `showName` | boolean | ShowName shows the station name publicly (default false). |
| `federate` | boolean | *optional* - Federate is used by an aggregator only (phase 5 federation): besides the host station, forward every approved network station as its own receiver. Default false. |
| `disabled` | boolean | Disabled keeps the entry but does not connect. |
| `type` | string | *optional* - Type is UplinkAVF ("avf", default when empty: an AirView aggregator over AVF/1) or UplinkBeast ("beast": "also feed others", a Beast output to a third-party aggregator such as readsb on port 30004; DESIGN §12.1 phase 5). For a Beast entry the port defaults to 30004, timestamps applies, and serverPin, enrolToken, spoolMb, shareGnss, returnMlat, sendLocation, pin, publicStats, showName and federate do not apply. |
| `format` | string | *optional* - Format is the Beast output format (Beast only): beast_reduce_plus_out (default), beast_reduce_out or beast_out. |
| `uuid` | string | *optional* - UUID is the station UUID sent to a Beast aggregator (Beast only; "" = a random UUID generated for this uplink). An identifier at that aggregator, not a password: shown to the owner, never public. |
| `reduceMs` | integer | *optional* - ReduceMs is the Beast reduce interval (Beast only; default 250, 50..5000 ms; ignored for beast_out). |
| `user` | string | *optional* - User is the display name sent to an mlat-server (MLAT only; default "airview"). An MLAT uplink also uses UUID (as for Beast), Inputs, Disabled and Pin: "hidden" (the default) asks the network to keep the station off its public maps. |

<a id="t-model-UplinkTuning"></a>

#### model.UplinkTuning

UplinkTuning holds the global tunables of the AVF/1 uplink client (internal/uplink; DESIGN §5.2, §20.3). They apply to every uplink.

| Field | Type | Notes |
|---|---|---|
| `ringSec` | integer | RingSec is the span of the in-memory ring of unacknowledged data (default 600, 60..3600 s). |
| `ringMb` | integer | RingMB is the size of that ring (default 12, 1..512 MiB). |
| `backfillQueueMb` | integer | BackfillQueueMB is the in-memory backfill queue (default 24, 1..1024 MiB). |
| `resumeWindowSec` | integer | ResumeWindowSec: unacknowledged data younger than this is resent live after a reconnect, older data as backfill (default 30, 5..300 s). |
| `backfillMinKBps` | integer | BackfillMinKBps is the backfill rate floor (default 64, 1..100000 KiB/s). |
| `backoffMinSec` | integer | BackoffMinSec is the first reconnect delay (default 1, 1..60 s). |
| `backoffMaxSec` | integer | BackoffMaxSec is the longest reconnect delay (default 60, 5..3600 s). |
| `livenessSec` | integer | LivenessSec reconnects after this long without PING or ACK (default 30, 5..600 s). |
| `spoolSegmentMb` | integer | SpoolSegmentMB is the size of one disk spool segment (default 4, 1..64 MiB). |

<a id="t-model-WeatherConfig"></a>

#### model.WeatherConfig

WeatherConfig configures free weather data ingestion (Open-Meteo, no API key).

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean |  |
| `intervalMin` | integer | refresh period, default 15 |
| `radarOverlay` | boolean | RainViewer precipitation radar layer on the map |

<a id="t-navdata-Airport"></a>

#### navdata.Airport

Airport is one OurAirports airport row (plus its runways when requested).

| Field | Type | Notes |
|---|---|---|
| `id` | integer |  |
| `ident` | string |  |
| `type` | string | OurAirports type, e.g. "large_airport" |
| `size` | integer (3 large, 2 medium, 1 small, 0 other, -1 closed) |  |
| `name` | string |  |
| `lat` | number |  |
| `lon` | number |  |
| `elevFt` | integer | *optional* |
| `continent` | string | *optional* |
| `country` | string | *optional* - ISO 3166-1 alpha-2 |
| `countryName` | string | *optional* |
| `region` | string | *optional* - ISO 3166-2 |
| `municipality` | string | *optional* |
| `scheduled` | boolean | *optional* |
| `icao` | string | *optional* |
| `iata` | string | *optional* |
| `gps` | string | *optional* |
| `local` | string | *optional* |
| `distKm` | number | *optional* - set by radius / nearest queries |
| `runways` | [navdata.Runway](#t-navdata-Runway)[] | *optional* |

<a id="t-navdata-AirportGuess"></a>

#### navdata.AirportGuess

AirportGuess is a likely departure or arrival airport.

| Field | Type | Notes |
|---|---|---|
| `airport` | [navdata.Airport](#t-navdata-Airport) |  |
| `runway` | string | *optional* - best aligned runway end, when one fits |
| `distKm` | number | from the reference track point |
| `heightAglFt` | number | *optional* |
| `confidence` | number | 0..1 |
| `reason` | string |  |
| `pointT` | integer | time of the track point used (Unix ms) |

<a id="t-navdata-DatasetInfo"></a>

#### navdata.DatasetInfo

DatasetInfo describes what is currently stored for one dataset.

| Field | Type | Notes |
|---|---|---|
| `dataset` | string (`countries` \| `airports` \| `runways` \| `navaids` \| `routes`) |  |
| `rows` | integer |  |
| `url` | string | *optional* |
| `imported` | integer | *optional* - Unix ms of the last import, 0 = never |
| `records` | integer |  |
| `skipped` | integer |  |
| `bytes` | integer |  |
| `lastModified` | string | *optional* |
| `source` | [navdata.Source](#t-navdata-Source) |  |

<a id="t-navdata-DatasetResult"></a>

#### navdata.DatasetResult

DatasetResult is the outcome of one dataset import.

| Field | Type | Notes |
|---|---|---|
| `dataset` | string (`countries` \| `airports` \| `runways` \| `navaids` \| `routes`) |  |
| `url` | string | *optional* |
| `records` | integer |  |
| `skipped` | integer |  |
| `bytes` | integer |  |
| `unchanged` | boolean | *optional* |
| `lastModified` | string | *optional* |
| `etag` | string | *optional* |
| `elapsedMs` | integer |  |
| `error` | string | *optional* |

<a id="t-navdata-ImportResult"></a>

#### navdata.ImportResult

ImportResult is the outcome of an Import call.

| Field | Type | Notes |
|---|---|---|
| `datasets` | [navdata.DatasetResult](#t-navdata-DatasetResult)[] |  |
| `started` | integer | Unix ms |
| `finished` | integer |  |
| `cancelled` | boolean | *optional* |

<a id="t-navdata-ImportStatus"></a>

#### navdata.ImportStatus

ImportStatus is the state of the background import job.

| Field | Type | Notes |
|---|---|---|
| `running` | boolean |  |
| `progress` | [navdata.Progress](#t-navdata-Progress) | *optional* |
| `fraction` | number |  |
| `result` | [navdata.ImportResult](#t-navdata-ImportResult) | *optional* - last finished run |
| `error` | string | *optional* - last run's error |
| `started` | integer | *optional* |

<a id="t-navdata-Navaid"></a>

#### navdata.Navaid

Navaid is one OurAirports navaid row.

| Field | Type | Notes |
|---|---|---|
| `id` | integer |  |
| `ident` | string |  |
| `name` | string |  |
| `type` | string | VOR, VOR-DME, VORTAC, DME, NDB, NDB-DME, TACAN |
| `freqKHz` | integer | *optional* |
| `lat` | number |  |
| `lon` | number |  |
| `elevFt` | integer | *optional* |
| `country` | string | *optional* |
| `dmeChannel` | string | *optional* |
| `magVarDeg` | number | *optional* |
| `usage` | string | *optional* - HI, LO, BOTH, TERMINAL, RNAV |
| `power` | string | *optional* |
| `airport` | string | *optional* - associated airport ident |

<a id="t-navdata-Progress"></a>

#### navdata.Progress

Progress reports the state of a running import.

| Field | Type | Notes |
|---|---|---|
| `dataset` | string (`countries` \| `airports` \| `runways` \| `navaids` \| `routes`) |  |
| `stage` | string | "download" \| "import" \| "unchanged" \| "done" \| "error" |
| `bytesRead` | integer |  |
| `bytesTotal` | integer | -1 unknown |
| `records` | integer |  |
| `skipped` | integer |  |
| `index` | integer | 1-based dataset number |
| `count` | integer | datasets in this run |
| `error` | string | *optional* |

<a id="t-navdata-RouteAirport"></a>

#### navdata.RouteAirport

RouteAirport is one airport of a route (resolved from the airports table; Resolved=false when the code is not in the imported data).

| Field | Type | Notes |
|---|---|---|
| `code` | string |  |
| `resolved` | boolean |  |
| `airport` | [navdata.Airport](#t-navdata-Airport) | *optional* |

<a id="t-navdata-RouteCheck"></a>

#### navdata.RouteCheck

RouteCheck is the plausibility of a route against the aircraft position.

| Field | Type | Notes |
|---|---|---|
| `plausible` | boolean |  |
| `doubtful` | boolean | = !Plausible, for UI convenience |
| `leg` | integer | 0-based index of the best fitting leg |
| `crossTrackKm` | number |  |
| `alongPct` | number | progress along that leg, 0..100 (may exceed slightly) |
| `toleranceKm` | number |  |
| `reason` | string |  |

<a id="t-navdata-RouteInfo"></a>

#### navdata.RouteInfo

RouteInfo is the answer for one callsign.

| Field | Type | Notes |
|---|---|---|
| `callsign` | string |  |
| `found` | boolean |  |
| `source` | string | *optional* - "standing-data" \| "adsbdb" |
| `airline` | string | *optional* |
| `airports` | [navdata.RouteAirport](#t-navdata-RouteAirport)[] | *optional* - in flight order |
| `path` | number[][] | *optional* - great-circle [lon,lat] points over all resolved legs (lon unwrapped) |
| `distanceKm` | number | *optional* |
| `check` | [navdata.RouteCheck](#t-navdata-RouteCheck) | *optional* - when a position was given and >= 2 airports resolved |
| `cached` | boolean | *optional* |

<a id="t-navdata-Runway"></a>

#### navdata.Runway

Runway is one OurAirports runway row.

| Field | Type | Notes |
|---|---|---|
| `id` | integer |  |
| `airportId` | integer |  |
| `airportIdent` | string |  |
| `lengthFt` | integer | *optional* |
| `widthFt` | integer | *optional* |
| `surface` | string | *optional* |
| `lighted` | boolean | *optional* |
| `closed` | boolean | *optional* |
| `le` | [navdata.RunwayEnd](#t-navdata-RunwayEnd) |  |
| `he` | [navdata.RunwayEnd](#t-navdata-RunwayEnd) |  |

<a id="t-navdata-RunwayEnd"></a>

#### navdata.RunwayEnd

RunwayEnd is one end ("le" = low end, "he" = high end) of a runway.

| Field | Type | Notes |
|---|---|---|
| `ident` | string | *optional* - e.g. "09L" |
| `lat` | number | *optional* |
| `lon` | number | *optional* |
| `elevFt` | integer | *optional* |
| `headingT` | number | *optional* - degrees true (as published) |
| `displacedFt` | integer | *optional* |

<a id="t-navdata-RunwayEndInfo"></a>

#### navdata.RunwayEndInfo

RunwayEndInfo is one usable landing/take-off direction of a runway, in a flat, hot-path friendly form (all values finite).

| Field | Type | Notes |
|---|---|---|
| `airportId` | integer |  |
| `airportIdent` | string |  |
| `airportCode` | string | ICAO if known |
| `runway` | string | end ident, e.g. "27L" |
| `lat` | number | landing threshold (displaced threshold applied) |
| `lon` | number |  |
| `endLat` | number | physical start of this direction's pavement |
| `endLon` | number |  |
| `headingT` | number | degrees true, direction of travel when landing on it |
| `headingSrc` | string |  |
| `elevFt` | number | threshold elevation (airport elevation fallback, 0 unknown) |
| `lengthFt` | number |  |
| `widthFt` | number |  |

<a id="t-navdata-Source"></a>

#### navdata.Source

Source describes where a dataset comes from (show Licence + Attribution in the UI).

| Field | Type | Notes |
|---|---|---|
| `dataset` | string (`countries` \| `airports` \| `runways` \| `navaids` \| `routes`) |  |
| `name` | string |  |
| `url` | string |  |
| `homepage` | string |  |
| `licence` | string |  |
| `licenceUrl` | string |  |
| `attribution` | string |  |
| `approxBytes` | integer |  |
| `format` | string | "csv" \| "zip" |

<a id="t-ntp-Sample"></a>

#### ntp.Sample

Sample is one accepted measurement: the minimum-delay reply of a burst. It is the unit handed to OnSample (for persistence in ntp_samples) and kept in the in-memory history ring.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms (host clock) of the measurement |
| `server` | string | configured server string |
| `addr` | string | *optional* - "ip:port" of the server that answered |
| `offsetMs` | number | true UTC - host clock, this sample |
| `delayMs` | number | round-trip delay of this sample |
| `stratum` | integer |  |
| `refId` | string | *optional* |
| `leap` | integer | Leap* constant announced by the server |
| `rootDelayMs` | number |  |
| `rootDispMs` | number |  |
| `smoothedMs` | number | robust smoothed offset after this sample |
| `jitterMs` | number | RMS jitter after this sample |
| `sent` | integer | queries sent in the burst |
| `replies` | integer | valid replies in the burst |

<a id="t-ntp-Stats"></a>

#### ntp.Stats

Stats summarises the history ring over a time window.

| Field | Type | Notes |
|---|---|---|
| `count` | integer |  |
| `spanSec` | number | time between first and last sample used |
| `offsetMeanMs` | number |  |
| `offsetMinMs` | number |  |
| `offsetMaxMs` | number |  |
| `offsetStdMs` | number |  |
| `delayMeanMs` | number |  |
| `delayMinMs` | number |  |
| `delayMaxMs` | number |  |
| `driftPpm` | number \| null | DriftPPM is the least-squares slope of offset against time in parts per million. Positive = the host clock is running slow relative to the server. Nil when fewer than 4 samples or less than 2 minutes are covered. |
| `polls` | integer | bursts attempted since start |
| `failures` | integer | bursts without any valid reply |
| `steps` | integer | host clock steps detected |

<a id="t-predict-AnalogueForecast"></a>

#### predict.AnalogueForecast

AnalogueForecast is product 3d: what followed the K past states that most resembled the present one.

| Field | Type | Notes |
|---|---|---|
| `method` | string |  |
| `k` | integer |  |
| `candidates` | integer | past states searched |
| `baseT` | integer | Unix ms of the state the forecast starts from |
| `hours` | [predict.AnalogueHour](#t-predict-AnalogueHour)[] |  |
| `analogues` | [predict.AnalogueMatch](#t-predict-AnalogueMatch)[] |  |
| `summary` | string |  |
| `verification` | [predict.AnalogueSkill](#t-predict-AnalogueSkill)[] |  |

<a id="t-predict-AnalogueHour"></a>

#### predict.AnalogueHour

AnalogueHour is one hour of the analogue forecast.

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `leadH` | integer |  |
| `tempC` | [predict.Est](#t-predict-Est) |  |
| `pressureMsl` | [predict.Est](#t-predict-Est) |  |
| `windKmh` | [predict.Est](#t-predict-Est) |  |
| `cloud` | number \| null | % |
| `precipProb` | number \| null | 0..1: share of the analogues with precipitation in that hour |
| `persistTempC` | number \| null | Persistence is the "no change" temperature / pressure for comparison. |

<a id="t-predict-AnalogueMatch"></a>

#### predict.AnalogueMatch

AnalogueMatch is one past state that resembled the present.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms of the past state |
| `distance` | number | standardised distance (0 = identical) |
| `tempChangeC` | number \| null | What followed in the next 24 hours. |
| `pressureChange` | number \| null |  |
| `precipMm` | number \| null |  |

<a id="t-predict-AnalogueSkill"></a>

#### predict.AnalogueSkill

AnalogueSkill verifies the analogue method at one lead time.

| Field | Type | Notes |
|---|---|---|
| `variable` | string | tempC \| pressureMsl \| windKmh |
| `leadH` | integer |  |
| `n` | integer |  |
| `analogueMae` | number \| null |  |
| `persistenceMae` | number \| null |  |
| `vsPersistence` | number \| null | 1 - MAE(analogue)/MAE(persistence) |
| `openMeteoMae` | number \| null | OpenMeteoMAE is the error of the logged Open-Meteo forecast at the same lead over the NOpenMeteo of those hours that have one. |
| `nOpenMeteo` | integer |  |

<a id="t-predict-Atmos"></a>

#### predict.Atmos

Atmos is the radio-propagation state of the lower atmosphere derived from one hourly weather row (observation or forecast): the features of the reception model. All numbers are finite when Valid.

| Field | Type | Notes |
|---|---|---|
| `hasProfile` | boolean | HasProfile is true when a pressure-level profile was analysed; false when the gradient comes from the stored columns or, as a last resort, from the climatological relation with surface refractivity. |
| `estimated` | boolean | *optional* - Estimated is true when dN/dh had to be estimated from surface refractivity alone (dN = -7.32 exp(0.005577 Ns), CRPL exponential reference atmosphere). |
| `nSurface` | number | surface refractivity, N-units |
| `dNdh` | number | gradient over the lowest km, N/km |
| `minGradient` | number | most negative layer gradient below 3 km, N/km |
| `minGradientM` | number | base height of that layer, m AMSL |
| `k` | number | effective earth radius factor (radio.KMax = trapping) |
| `horizonFactor` | number | radio horizon relative to standard (capped at 3) |
| `index` | integer | radio.Assess index, 50 = standard, 85+ = ducting |
| `class` | string | radio.Class* |
| `ducts` | integer | ducts found in the profile |
| `ductStrengthM` | number | strongest duct, M-units (0 = none) |
| `ductKind` | string | *optional* - radio.Duct* of the strongest duct |
| `ductBaseM` | number | m AMSL |
| `ductTopM` | number | m AMSL |
| `ducting1090` | boolean | a duct able to trap 1090 MHz is present |
| `inversionC` | number \| null | InversionC is the temperature excess of the warmest level in the lowest 1.5 km over the 2 m temperature (positive = inversion), NaN without a profile. |
| `summary` | string | *optional* |

<a id="t-predict-BandOutlook"></a>

#### predict.BandOutlook

BandOutlook is the smooth-earth line-of-sight range of one altitude band under the standard atmosphere and under the current refraction.

| Field | Type | Notes |
|---|---|---|
| `band` | integer |  |
| `label` | string |  |
| `altFt` | number |  |
| `standardKm` | number |  |
| `nowKm` | number |  |
| `factor` | number | NowKm / StandardKm (capped) |

<a id="t-predict-BiasRow"></a>

#### predict.BiasRow

BiasRow is the verification of the Open-Meteo forecast against the station's observation record for one variable and lead-time class.

| Field | Type | Notes |
|---|---|---|
| `variable` | string |  |
| `leadFrom` | integer | hours |
| `leadTo` | integer |  |
| `n` | integer | forecast/observation pairs |
| `bias` | number \| null | mean of forecast - observation |
| `mae` | number \| null | mean absolute error of the raw forecast |
| `maeCorrected` | number \| null | MAECorrected is the error after subtracting the bias, measured on the newest 30 % of the pairs with a bias learned from the older 70 %. |
| `maeHoldout` | number \| null | raw forecast on the same 30 % |
| `applied` | boolean | Applied: the correction verifiably helps and is applied to the outlook. |

<a id="t-predict-BusyHour"></a>

#### predict.BusyHour

BusyHour is one of the busiest upcoming hours.

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `aircraft` | number \| null |  |
| `msgRate` | number \| null |  |
| `inHours` | integer | hours from now |

<a id="t-predict-Coefficient"></a>

#### predict.Coefficient

Coefficient describes one feature of the learned range model.

| Field | Type | Notes |
|---|---|---|
| `feature` | string |  |
| `perUnit` | number \| null | PerUnit is the change of the p95 range in km per unit of the feature, PerSD per standard deviation of the feature in the training data. |
| `perSd` | number \| null |  |

<a id="t-predict-Est"></a>

#### predict.Est

Est is one estimate: the expected value, an 80 % interval and a confidence between 0 and 1.

| Field | Type | Notes |
|---|---|---|
| `value` | number \| null |  |
| `lo` | number \| null |  |
| `hi` | number \| null |  |
| `confidence` | number |  |

<a id="t-predict-Factor"></a>

#### predict.Factor

Factor is one contribution to an hour's expected range.

| Field | Type | Notes |
|---|---|---|
| `key` | string | refraction \| duct \| traffic \| timeOfDay \| humidity \| wind \| pressure |
| `text` | string | plain-language explanation |
| `effectKm` | number \| null | contribution to the expected p95 range relative to a typical hour |

<a id="t-predict-LeadError"></a>

#### predict.LeadError

LeadError is the backtested error at one lead time.

| Field | Type | Notes |
|---|---|---|
| `leadH` | integer |  |
| `rmse` | number \| null |  |

<a id="t-predict-LiveSkill"></a>

#### predict.LiveSkill

LiveSkill verifies the forecasts that were actually issued (logged in predict_forecast_log) against what was measured afterwards. Unlike the backtests it includes the error of the weather forecast itself.

| Field | Type | Notes |
|---|---|---|
| `product` | string | traffic.aircraft \| traffic.msgRate \| reception.p95 \| reception.max |
| `leadH` | integer |  |
| `n` | integer |  |
| `mae` | number \| null |  |
| `rmse` | number \| null |  |
| `bias` | number \| null | mean of forecast - actual |
| `coverage` | number \| null | Coverage is the share of the actuals inside the 80 % interval. |

<a id="t-predict-ModelReport"></a>

#### predict.ModelReport

ModelReport is the verification of one learned model.

| Field | Type | Notes |
|---|---|---|
| `product` | string | traffic \| reception \| weather |
| `target` | string | aircraft \| msgRate \| p95Km \| farProbability \| bias \| analogue |
| `title` | string |  |
| `status` | string |  |
| `dataHours` | integer | DataHours of NeedHours usable history hours; Needs says in words what is missing ("needs 5 more days ..."), empty when nothing is. |
| `needHours` | integer |  |
| `needs` | string | *optional* |
| `method` | string |  |
| `skill` | [predict.Skill](#t-predict-Skill) | *optional* - Skill: rolling-origin backtest against persistence and seasonal-naive. |
| `byLead` | [predict.LeadError](#t-predict-LeadError)[] | *optional* |
| `series` | [predict.VerifyPoint](#t-predict-VerifyPoint)[] | *optional* - Series is forecast versus actual for charts. |

<a id="t-predict-Nowcast"></a>

#### predict.Nowcast

Nowcast is product 5: the compact summary for the top bar / home tiles.

| Field | Type | Notes |
|---|---|---|
| `generated` | integer |  |
| `t` | integer | hour the index refers to, Unix ms |
| `index` | integer | 0-100 reception index now |
| `label` | string | poor \| normal \| enhanced \| ducting likely |
| `trend` | string | Trend over the next six hours and the index change behind it. |
| `trendDelta` | integer |  |
| `next6h` | [predict.NowcastPoint](#t-predict-NowcastPoint)[] |  |
| `p95Km` | [predict.Est](#t-predict-Est) | expected p95 range this hour |
| `farProbability` | number \| null | highest far-reception probability of the next 6 h |
| `aircraft` | number \| null | expected aircraft this hour |
| `sentence` | string |  |
| `confidence` | string |  |
| `level` | string |  |

<a id="t-predict-NowcastPoint"></a>

#### predict.NowcastPoint

NowcastPoint is the reception index of one of the next hours.

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `index` | integer |  |
| `label` | string |  |

<a id="t-predict-Pattern"></a>

#### predict.Pattern

Pattern is one detected or expected weather pattern.

| Field | Type | Notes |
|---|---|---|
| `kind` | string |  |
| `severity` | integer | Severity 0 = information, 1 = low, 2 = moderate, 3 = high. |
| `probability` | number \| null | 0..1 where meaningful |
| `title` | string |  |
| `detail` | string |  |
| `from` | integer | *optional* - Unix ms |
| `to` | integer | *optional* |

<a id="t-predict-ReceptionActual"></a>

#### predict.ReceptionActual

ReceptionActual is one observed hour for the chart.

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `p95Km` | number \| null |  |
| `maxKm` | number \| null |  |
| `index` | integer | *optional* - propagation index of the observed atmosphere |

<a id="t-predict-ReceptionForecast"></a>

#### predict.ReceptionForecast

ReceptionForecast is product 2: reception conditions for the next 72 hours.

| Field | Type | Notes |
|---|---|---|
| `product` | string |  |
| `generated` | integer | Unix ms |
| `method` | string | Method is a short description of how the numbers were produced. |
| `level` | string | Level is LevelNone \| LevelPhysics \| LevelBlend \| LevelLearned. |
| `confidence` | string | Confidence is ConfLow \| ConfMedium \| ConfHigh; ConfidenceValue the underlying 0..1 figure. |
| `confidenceValue` | number |  |
| `skill` | [predict.Skill](#t-predict-Skill) | *optional* - Skill is the backtest result, nil while there is too little history. |
| `dataHours` | integer | DataHours is the number of history hours the product could use. |
| `notes` | string[] | *optional* |
| `hours` | [predict.ReceptionHour](#t-predict-ReceptionHour)[] |  |
| `farKm` | number \| null | threshold of a far-reception hour |
| `typicalP95Km` | number \| null |  |
| `typicalMaxKm` | number \| null |  |
| `antennaAmslM` | number |  |
| `bands` | [predict.BandOutlook](#t-predict-BandOutlook)[] |  |
| `best` | [predict.ReceptionHour](#t-predict-ReceptionHour) | *optional* - the most promising hour |
| `model` | [predict.ReceptionModelInfo](#t-predict-ReceptionModelInfo) | *optional* |
| `farSkill` | [predict.Skill](#t-predict-Skill) | *optional* |
| `recent` | [predict.ReceptionActual](#t-predict-ReceptionActual)[] |  |
| `weatherHours` | integer | forecast hours backed by an Open-Meteo forecast |

<a id="t-predict-ReceptionHour"></a>

#### predict.ReceptionHour

ReceptionHour is one hour of the reception-conditions forecast.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | start of the hour, Unix ms |
| `leadH` | integer |  |
| `source` | string | Source is "forecast" (Open-Meteo forecast for the hour), "observation" (current analysis) or "climatology" (no weather data: standard atmosphere). |
| `p95Km` | [predict.Est](#t-predict-Est) | expected 95th-percentile range |
| `maxKm` | [predict.Est](#t-predict-Est) | expected maximum range |
| `index` | integer | Index is the 0-100 reception index (50 = standard atmosphere) and Label its class: poor \| normal \| enhanced \| ducting likely. |
| `label` | string |  |
| `farProbability` | number \| null | FarProbability is the probability (0..1) of anomalous long-range reception: an hourly maximum beyond ReceptionForecast.FarKm. |
| `confidence` | number |  |
| `aircraft` | number \| null | expected traffic level used |
| `atmosphere` | [predict.Atmos](#t-predict-Atmos) |  |
| `factors` | [predict.Factor](#t-predict-Factor)[] |  |
| `summary` | string |  |

<a id="t-predict-ReceptionModelInfo"></a>

#### predict.ReceptionModelInfo

ReceptionModelInfo documents the learned part of the forecast.

| Field | Type | Notes |
|---|---|---|
| `trainingHours` | integer |  |
| `farEvents` | integer |  |
| `weight` | number | share of the learned model in the blend, 0..1 |
| `farWeight` | number | share of the logistic model in the far probability |
| `r2` | number \| null |  |
| `residStdKm` | number \| null |  |
| `lambda` | number \| null |  |
| `gradientEffectKm` | number \| null | GradientEffectKm is the learned change of the p95 range when the low-level refractivity gradient steepens from -40 to -120 N/km (super-refraction), all else equal. Positive = the station's range grows with refraction, as physics expects. |
| `coefficients` | [predict.Coefficient](#t-predict-Coefficient)[] |  |

<a id="t-predict-Skill"></a>

#### predict.Skill

Skill is the outcome of a rolling-origin backtest of one model against the naive baselines. Errors are RMSE unless Metric says otherwise; the Vs* scores are 1 - error(model)/error(baseline): positive = better than the baseline, 0 = no better, negative = worse.

| Field | Type | Notes |
|---|---|---|
| `metric` | string | "rmse" \| "brier" |
| `unit` | string | unit of the errors |
| `model` | number \| null | error of the model |
| `persistence` | number \| null | error of the persistence forecast |
| `seasonalNaive` | number \| null | error of the seasonal-naive forecast |
| `vsPersistence` | number \| null | skill score against persistence |
| `vsSeasonalNaive` | number \| null | skill score against seasonal-naive |
| `n` | integer | forecast points scored |
| `origins` | integer | forecast origins |
| `horizonH` | integer | forecast horizon of the backtest, hours |
| `note` | string | *optional* |

<a id="t-predict-Status"></a>

#### predict.Status

Status describes the state of the forecaster.

| Field | Type | Notes |
|---|---|---|
| `fitted` | integer | Unix ms of the last model fit, 0 = never |
| `built` | integer | Unix ms of the last product build |
| `dataHours` | integer | hours of traffic history |
| `lastError` | string | *optional* |

<a id="t-predict-TrafficActual"></a>

#### predict.TrafficActual

TrafficActual is one observed hour, for drawing the recent past next to the forecast.

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `aircraft` | number \| null |  |
| `msgRate` | number \| null |  |

<a id="t-predict-TrafficForecast"></a>

#### predict.TrafficForecast

TrafficForecast is product 1: expected traffic for the next 72 hours.

| Field | Type | Notes |
|---|---|---|
| `product` | string |  |
| `generated` | integer | Unix ms |
| `method` | string | Method is a short description of how the numbers were produced. |
| `level` | string | Level is LevelNone \| LevelPhysics \| LevelBlend \| LevelLearned. |
| `confidence` | string | Confidence is ConfLow \| ConfMedium \| ConfHigh; ConfidenceValue the underlying 0..1 figure. |
| `confidenceValue` | number |  |
| `skill` | [predict.Skill](#t-predict-Skill) | *optional* - Skill is the backtest result, nil while there is too little history. |
| `dataHours` | integer | DataHours is the number of history hours the product could use. |
| `notes` | string[] | *optional* |
| `utcOffsetSec` | integer | UTCOffsetSec is the local-time offset used for the hour-of-week profile at the first forecast hour. |
| `hours` | [predict.TrafficHour](#t-predict-TrafficHour)[] |  |
| `busiest` | [predict.BusyHour](#t-predict-BusyHour)[] |  |
| `quietest` | [predict.BusyHour](#t-predict-BusyHour) | *optional* |
| `recent` | [predict.TrafficActual](#t-predict-TrafficActual)[] | last 48 observed hours |
| `msgRateSkill` | [predict.Skill](#t-predict-Skill) | *optional* - MsgRateSkill is the backtest of the message-rate model (Meta.Skill is the aircraft-count model). |

<a id="t-predict-TrafficHour"></a>

#### predict.TrafficHour

TrafficHour is one forecast hour.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | start of the hour, Unix ms |
| `aircraft` | [predict.Est](#t-predict-Est) | peak number of aircraft tracked at once |
| `msgRate` | [predict.Est](#t-predict-Est) | frames per second |

<a id="t-predict-Verification"></a>

#### predict.Verification

Verification is product 4: how good the forecasts are.

| Field | Type | Notes |
|---|---|---|
| `generated` | integer |  |
| `fitted` | integer | Unix ms of the model fit the report belongs to |
| `summary` | string |  |
| `models` | [predict.ModelReport](#t-predict-ModelReport)[] |  |
| `bias` | [predict.BiasRow](#t-predict-BiasRow)[] | Bias is the Open-Meteo forecast verification per variable and lead. |
| `analogue` | [predict.AnalogueSkill](#t-predict-AnalogueSkill)[] | Analogue compares the analogue weather forecast with persistence and the logged Open-Meteo forecasts. |
| `live` | [predict.LiveSkill](#t-predict-LiveSkill)[] | Live verifies the forecasts that were really issued. |

<a id="t-predict-VerifyPoint"></a>

#### predict.VerifyPoint

VerifyPoint is one backtest forecast next to what really happened. The forecast was made LeadH hours ahead: from data before T - (LeadH-1) h only.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | start of the target hour, Unix ms |
| `leadH` | integer |  |
| `actual` | number \| null |  |
| `forecast` | number \| null |  |

<a id="t-predict-WeatherHour"></a>

#### predict.WeatherHour

WeatherHour is one hour of the bias-corrected Open-Meteo forecast.

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `leadH` | integer |  |
| `tempC` | number \| null | bias-corrected |
| `tempRawC` | number \| null | as forecast |
| `dewPointC` | number \| null |  |
| `rh` | number \| null |  |
| `pressureMsl` | number \| null |  |
| `windKmh` | number \| null |  |
| `windDir` | number \| null |  |
| `gustKmh` | number \| null |  |
| `cloud` | number \| null |  |
| `cloudLow` | number \| null |  |
| `precipMm` | number \| null |  |
| `precipProb` | number \| null |  |
| `visibilityM` | number \| null |  |
| `cape` | number \| null |  |
| `weatherCode` | integer | *optional* |
| `text` | string | *optional* |
| `isDay` | boolean | *optional* |
| `fogRisk` | number | Risks, 0..1. |
| `inversionRisk` | number |  |
| `convectiveRisk` | number |  |
| `corrected` | boolean | a learned bias was applied to at least one variable |

<a id="t-predict-WeatherNow"></a>

#### predict.WeatherNow

WeatherNow is the latest observation with the Kalman-filtered pressure tendency of the station's own record.

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `tempC` | number \| null |  |
| `dewPointC` | number \| null |  |
| `rh` | number \| null |  |
| `pressureMsl` | number \| null |  |
| `windKmh` | number \| null |  |
| `windDir` | number \| null |  |
| `wind` | string | *optional* - compass point |
| `cloud` | number \| null |  |
| `visibilityM` | number \| null |  |
| `text` | string | *optional* |
| `change3h` | number \| null | Change3h is the smoothed pressure tendency in hPa per 3 hours and Tendency its class (ClassifyTendency). |
| `tendency` | string |  |
| `pressure6h` | [predict.Est](#t-predict-Est) | Pressure6h is the Kalman extrapolation of the pressure six hours ahead. |

<a id="t-predict-WeatherOutlook"></a>

#### predict.WeatherOutlook

WeatherOutlook is product 3: the local weather outlook.

| Field | Type | Notes |
|---|---|---|
| `product` | string |  |
| `generated` | integer | Unix ms |
| `method` | string | Method is a short description of how the numbers were produced. |
| `level` | string | Level is LevelNone \| LevelPhysics \| LevelBlend \| LevelLearned. |
| `confidence` | string | Confidence is ConfLow \| ConfMedium \| ConfHigh; ConfidenceValue the underlying 0..1 figure. |
| `confidenceValue` | number |  |
| `skill` | [predict.Skill](#t-predict-Skill) | *optional* - Skill is the backtest result, nil while there is too little history. |
| `dataHours` | integer | DataHours is the number of history hours the product could use. |
| `notes` | string[] | *optional* |
| `attribution` | string |  |
| `attributionUrl` | string |  |
| `current` | [predict.WeatherNow](#t-predict-WeatherNow) | *optional* |
| `hours` | [predict.WeatherHour](#t-predict-WeatherHour)[] |  |
| `bias` | [predict.BiasRow](#t-predict-BiasRow)[] |  |
| `zambretti` | [predict.ZambrettiOutlook](#t-predict-ZambrettiOutlook) | *optional* |
| `patterns` | [predict.Pattern](#t-predict-Pattern)[] |  |
| `analogue` | [predict.AnalogueForecast](#t-predict-AnalogueForecast) | *optional* |

<a id="t-predict-ZambrettiOutlook"></a>

#### predict.ZambrettiOutlook

ZambrettiOutlook is the independent barometric forecast.

| Field | Type | Notes |
|---|---|---|
| `number` | integer | 0..25 |
| `letter` | string | "A".."Z" |
| `text` | string |  |
| `trend` | string |  |
| `exceptional` | boolean | adjusted pressure outside the 950-1050 hPa scale |
| `pressureMsl` | number \| null |  |
| `change3h` | number \| null | Kalman-smoothed, hPa / 3 h |
| `tendency` | string |  |
| `windDir` | number \| null |  |
| `wind` | string | *optional* |
| `season` | string | "summer" \| "winter" |
| `hemisphere` | string | "north" \| "south" |
| `outlook` | string | plain-language 12-24 h outlook |
| `confidence` | number |  |

<a id="t-publicdto-Aircraft"></a>

#### publicdto.Aircraft

Aircraft is one aircraft of the public merged picture.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `addrType` | string | *optional* |
| `callsign` | string | *optional* |
| `squawk` | string | *optional* |
| `category` | string | *optional* |
| `reg` | string | *optional* |
| `type` | string | *optional* |
| `typeDesc` | string | *optional* |
| `operator` | string | *optional* |
| `country` | string | *optional* |
| `countryCode` | string | *optional* |
| `military` | boolean | *optional* |
| `lat` | number | *optional* |
| `lon` | number | *optional* |
| `source` | string |  |
| `posAge` | number | *optional* |
| `onGround` | boolean | *optional* |
| `coarse` | boolean | *optional* - Coarse is set when the position was rounded by the island guard (DESIGN §8.6). |
| `altBaro` | number | *optional* |
| `altGeom` | number | *optional* |
| `vertRate` | number | *optional* |
| `selAlt` | number | *optional* |
| `qnh` | number | *optional* |
| `gs` | number | *optional* |
| `track` | number | *optional* |
| `ias` | number | *optional* |
| `tas` | number | *optional* |
| `mach` | number | *optional* |
| `magHeading` | number | *optional* |
| `roll` | number | *optional* |
| `trackRate` | number | *optional* |
| `selHeading` | number | *optional* |
| `windDir` | number | *optional* |
| `windSpd` | number | *optional* |
| `oat` | number | *optional* |
| `adsbVersion` | integer | *optional* |
| `nic` | integer | *optional* |
| `nacp` | integer | *optional* |
| `nacv` | integer | *optional* |
| `sil` | integer | *optional* |
| `rcM` | number | *optional* |
| `emergency` | string | *optional* |
| `alert` | boolean | *optional* |
| `spi` | boolean | *optional* |
| `navModes` | string[] | *optional* |
| `firstSeen` | integer | Unix ms |
| `lastSeen` | integer | Unix ms |
| `predicted` | boolean | *optional* |
| `coastS` | number | *optional* |
| `predSigmaM` | number | *optional* |
| `lastRealLat` | number | *optional* |
| `lastRealLon` | number | *optional* |
| `route` | [model.RouteShort](#t-model-RouteShort) | *optional* |
| `mlatGrade` | string | *optional* |
| `mlatSigmaM` | number | *optional* |

<a id="t-publicdto-Counts"></a>

#### publicdto.Counts

Counts summarises the public picture. It has no maximum range (a receiver product).

| Field | Type | Notes |
|---|---|---|
| `aircraft` | integer |  |
| `withPos` | integer |  |
| `adsb` | integer |  |
| `mlat` | integer |  |
| `tisb` | integer |  |
| `modes` | integer |  |
| `msgRate` | number | merged network frames/s |
| `posRate` | number |  |
| `predicted` | integer |  |

<a id="t-publicdto-MLATCell"></a>

#### publicdto.MLATCell

MLATCell is one 1° cell of MLATCoverage (south-west corner; Band is the altitude band of the engine's quality map).

| Field | Type | Notes |
|---|---|---|
| `lat` | integer |  |
| `lon` | integer |  |
| `band` | integer |  |
| `n` | integer |  |
| `p50M` | number |  |
| `p95M` | number |  |

<a id="t-publicdto-MLATCoverage"></a>

#### publicdto.MLATCoverage

MLATCoverage is GET /api/public/mlat: the network's empirical MLAT accuracy over the last hour and its 1° cells (DESIGN §8.2). A cell is listed only with at least 20 hold-out samples from at least 5 different contributing stations; no station identity or count is published.

| Field | Type | Notes |
|---|---|---|
| `quality` | [publicdto.MLATQuality](#t-publicdto-MLATQuality) \| null | Quality is null while the last hour has fewer than 20 hold-out samples. |
| `samples` | integer |  |
| `cells` | [publicdto.MLATCell](#t-publicdto-MLATCell)[] |  |
| `updated` | integer | Unix ms |

<a id="t-publicdto-MLATQuality"></a>

#### publicdto.MLATQuality

MLATQuality is the network-wide empirical MLAT accuracy (DESIGN §8.2).

| Field | Type | Notes |
|---|---|---|
| `p50M` | number | P50M/P95M/P99M: horizontal error of the hold-out tracks against the ADS-B reference; it includes the reference's airframe position latency, so it overstates the MLAT error. |
| `p95M` | number |  |
| `p99M` | number |  |
| `estP50M` | number | EstP50M/EstP95M: the horizontal error estimated from the cross-track errors alone (free of that latency): the headline figure (0 = not available). |
| `estP95M` | number |  |
| `fixesPerMin` | number |  |

<a id="t-publicdto-NetworkStats"></a>

#### publicdto.NetworkStats

NetworkStats is GET /api/public/stats: the merged network in numbers.

| Field | Type | Notes |
|---|---|---|
| `now` | integer |  |
| `name` | string | *optional* |
| `approved` | integer | approved stations |
| `online` | integer | approved stations with data in the last 2 minutes |
| `counts` | [publicdto.Counts](#t-publicdto-Counts) |  |

<a id="t-publicdto-Pin"></a>

#### publicdto.Pin

Pin is a station's stored public pin point (DESIGN §8.3): never the exact position, always the keyed point inside its cell.

| Field | Type | Notes |
|---|---|---|
| `lat` | number |  |
| `lon` | number |  |
| `precision` | string | "region" \| "cell01" (after the rural guard) |

<a id="t-publicdto-Snapshot"></a>

#### publicdto.Snapshot

Snapshot is the public live picture (GET /api/public/aircraft, the public "aircraft" stream event, and GET /api/aircraft for public viewers in aggregator mode).

| Field | Type | Notes |
|---|---|---|
| `now` | integer | Unix ms |
| `aircraft` | [publicdto.Aircraft](#t-publicdto-Aircraft)[] |  |
| `counts` | [publicdto.Counts](#t-publicdto-Counts) |  |

<a id="t-publicdto-StationCounts"></a>

#### publicdto.StationCounts

StationCounts counts approved stations: all, online and with a public pin.

| Field | Type | Notes |
|---|---|---|
| `total` | integer |  |
| `online` | integer |  |
| `public` | integer |  |

<a id="t-publicdto-StationEntry"></a>

#### publicdto.StationEntry

StationEntry is one entry of the public station list. Stations with a hidden pin are listed only when their owner opted in to public statistics, and then without ID and pin (their SID would link them across views).

| Field | Type | Notes |
|---|---|---|
| `sid` | string | *optional* - only for stations with a public pin |
| `name` | string | *optional* - only when the owner opted in |
| `online` | boolean |  |
| `pin` | [publicdto.Pin](#t-publicdto-Pin) \| null | null = no public pin |
| `stats` | [publicdto.StationStats](#t-publicdto-StationStats) | *optional* |

<a id="t-publicdto-StationList"></a>

#### publicdto.StationList

StationList is GET /api/public/stations.

| Field | Type | Notes |
|---|---|---|
| `stations` | [publicdto.StationEntry](#t-publicdto-StationEntry)[] |  |
| `approved` | integer | Approved and Online count every approved station (not only listed). |
| `online` | integer |  |

<a id="t-publicdto-StationStats"></a>

#### publicdto.StationStats

StationStats are the opt-in, non-locating statistics of a station.

| Field | Type | Notes |
|---|---|---|
| `msgRate` | number | MsgRate is the recent frame rate (frames/s, 1 decimal). |
| `posRate` | number | PosRate is the recent position rate (positions/s, 1 decimal). |
| `aircraft` | integer | Aircraft heard in the current minute. |
| `positions` | integer | Positions decoded since the station's lane started (0 for entries without SID, see Unlinkable). |
| `uptimePct` | number | *optional* - UptimePct is the share of the last 24 h with data (when known). |
| `aircraftDay` | integer | *optional* - AircraftDay is the number of aircraft heard today (when known). |
| `mlatContrib` | integer | *optional* - MLATContrib is the number of MLAT fixes the station contributed to in the last hour (when the MLAT engine runs). |

<a id="t-publicdto-Status"></a>

#### publicdto.Status

Status is the public application status (GET /api/public/status and GET /api/status for public viewers in aggregator mode). It carries no feed, receiver, data-port, NTP or path information.

| Field | Type | Notes |
|---|---|---|
| `app` | string |  |
| `version` | string |  |
| `mode` | string |  |
| `network` | string | *optional* - aggregator display name |
| `now` | integer |  |
| `started` | integer |  |
| `tileCache` | boolean |  |
| `counts` | [publicdto.Counts](#t-publicdto-Counts) |  |
| `adminLocked` | boolean | AdminLocked is always true for a public viewer. |
| `located` | boolean | Located is always false: the public scope has no station position. |
| `publicMap` | boolean | PublicMap / PublicStats / TarJSON tell the UI which public surfaces exist. |
| `publicStats` | boolean |  |
| `tarJson` | boolean |  |
| `tileCacheHosts` | string[] | *optional* - TileCacheHosts lists the map hosts of the tile proxy. |

<a id="t-publicdto-Summary"></a>

#### publicdto.Summary

Summary is GET /api/public/summary: the network landing page.

| Field | Type | Notes |
|---|---|---|
| `name` | string | *optional* |
| `stationCounts` | [publicdto.StationCounts](#t-publicdto-StationCounts) |  |
| `aircraft` | integer |  |
| `mlatAircraft` | integer |  |
| `messagesPerSec` | number |  |
| `mlat` | [publicdto.MLATQuality](#t-publicdto-MLATQuality) \| null | MLAT is the empirical network MLAT quality (null until the MLAT engine publishes it). |
| `updated` | integer | Unix ms |

<a id="t-publicdto-TarAircraft"></a>

#### publicdto.TarAircraft

TarAircraft is one entry of aircraft.json.

| Field | Type | Notes |
|---|---|---|
| `hex` | string |  |
| `type` | string |  |
| `flight` | string | *optional* |
| `r` | string | *optional* |
| `t` | string | *optional* |
| `desc` | string | *optional* |
| `alt_baro` | any JSON (see the example) | *optional* - ft, or "ground" |
| `alt_geom` | integer | *optional* |
| `gs` | number | *optional* |
| `ias` | integer | *optional* |
| `tas` | integer | *optional* |
| `mach` | number | *optional* |
| `track` | number | *optional* |
| `track_rate` | number | *optional* |
| `roll` | number | *optional* |
| `mag_heading` | number | *optional* |
| `baro_rate` | integer | *optional* |
| `squawk` | string | *optional* |
| `emergency` | string | *optional* |
| `category` | string | *optional* |
| `nav_qnh` | number | *optional* |
| `nav_altitude_mcp` | integer | *optional* |
| `nav_heading` | number | *optional* |
| `nav_modes` | string[] | *optional* |
| `lat` | number | *optional* |
| `lon` | number | *optional* |
| `nic` | integer | *optional* |
| `rc` | integer | *optional* |
| `seen_pos` | number | *optional* |
| `version` | integer | *optional* |
| `nac_p` | integer | *optional* |
| `nac_v` | integer | *optional* |
| `sil` | integer | *optional* |
| `alert` | integer |  |
| `spi` | integer |  |
| `mlat` | string[] |  |
| `tisb` | string[] |  |
| `seen` | number |  |
| `dbFlags` | integer | *optional* - 1 = military |

<a id="t-publicdto-TarAircraftFile"></a>

#### publicdto.TarAircraftFile

TarAircraftFile is /data/aircraft.json.

| Field | Type | Notes |
|---|---|---|
| `now` | number | Unix seconds |
| `messages` | integer | merged frames since start (network total) |
| `aircraft` | [publicdto.TarAircraft](#t-publicdto-TarAircraft)[] |  |

<a id="t-publicdto-TarReceiver"></a>

#### publicdto.TarReceiver

TarReceiver is /data/receiver.json. Lat/Lon are present only in standalone mode (the local receiver, for its own operator) or, in aggregator mode, as the rounded centroid of the public pins.

| Field | Type | Notes |
|---|---|---|
| `version` | string |  |
| `refresh` | integer | ms between aircraft.json updates |
| `history` | integer | number of history files (none) |
| `lat` | number | *optional* |
| `lon` | number | *optional* |

<a id="t-publicdto-TrailPoint"></a>

#### publicdto.TrailPoint

TrailPoint is one public trail point. It encodes as the compact array [tMs, lat, lon, altFt|null, gsKt|null, null, src] - the layout of the standalone trails with the signal column always null, so existing clients parse it unchanged.

| Field | Type | Notes |
|---|---|---|
| `T` | integer |  |
| `Lat` | number |  |
| `Lon` | number |  |
| `Alt` | number | NaN = unknown |
| `GS` | number | NaN = unknown |
| `Src` | integer |  |

<a id="t-publicdto-Trails"></a>

#### publicdto.Trails

Trails is the public trails reply.

| Field | Type | Notes |
|---|---|---|
| `now` | integer |  |
| `minutes` | integer |  |
| `trails` | { [key: string]: [publicdto.TrailPoint](#t-publicdto-TrailPoint)[] } |  |

<a id="t-radio-BandPrediction"></a>

#### radio.BandPrediction

BandPrediction is the predicted maximum range for one altitude band.

| Field | Type | Notes |
|---|---|---|
| `band` | integer | 0..6 (geo.AltBand) |
| `label` | string |  |
| `altFt` | number | representative altitude of the band |
| `horizonKm` | number | smooth-earth line-of-sight limit |
| `rangeKm` | number | predicted maximum decoding range |
| `limit` | string | Limit* constant |
| `marginDb` | number | MarginDB is the link margin left at RangeKm: the spare signal at the horizon when horizon-limited, 0 when power-limited. |

<a id="t-radio-BandRange"></a>

#### radio.BandRange

BandRange is the radio line-of-sight range for one of the standard altitude bands (geo.AltBand).

| Field | Type | Notes |
|---|---|---|
| `band` | integer | 0..6 |
| `label` | string | "<1k", "1-5k", … |
| `altFt` | number | representative altitude of the band (geo.AltBandTypicalFt) |
| `losKm` | number | line-of-sight range at AltFt |
| `losLoKm` | number | at the band's lower edge (0 ft for the lowest band) |
| `losHiKm` | number | at the band's upper edge (50 000 ft for the highest band) |

<a id="t-radio-Budget"></a>

#### radio.Budget

Budget is the complete link budget for one aircraft position.

| Field | Type | Notes |
|---|---|---|
| `azimuthDeg` | number | bearing from the station |
| `groundKm` | number | ground distance |
| `altM` | number | aircraft altitude AMSL |
| `slantKm` | number | straight-line path length |
| `elevationDeg` | number | elevation of the aircraft at the antenna |
| `horizonKm` | number | line-of-sight limit for this altitude |
| `lineOfSight` | boolean | GroundKm <= HorizonKm |
| `txPowerDbm` | number |  |
| `eirpDbm` | number |  |
| `fsplDb` | number |  |
| `gasLossDb` | number |  |
| `polarizationLossDb` | number |  |
| `miscLossDb` | number |  |
| `rxAntennaGainDbi` | number | pattern gain towards the aircraft |
| `receivedDbm` | number | signal at the antenna terminals |
| `frontEndLossDb` | number | cable + filter |
| `lnaGainDb` | number |  |
| `rxInputDbm` | number | signal at the receiver input |
| `systemTempK` | number |  |
| `systemNfDb` | number |  |
| `noiseFloorDbm` | number | at the antenna terminals |
| `sensitivityDbm` | number | noise floor + required SNR |
| `snrDb` | number |  |
| `requiredSnrDb` | number |  |
| `marginDb` | number | SNR − required SNR |
| `decodable` | boolean | LineOfSight && MarginDB >= 0 |

<a id="t-radio-Duct"></a>

#### radio.Duct

Duct is a tropospheric duct found in a profile.

| Field | Type | Notes |
|---|---|---|
| `kind` | string | Duct* constant |
| `baseM` | number | bottom of the duct, m AMSL |
| `topM` | number | top of the duct = top of the trapping layer |
| `trapBaseM` | number | bottom of the trapping layer (dM/dh < 0) |
| `thicknessM` | number | TopM − BaseM |
| `strengthM` | number | ΔM: M deficit across the trapping layer, M-units |
| `maxWavelengthM` | number | MaxWavelengthM is the longest wavelength the duct can trap and CutoffMHz the corresponding lowest frequency. |
| `cutoffMHz` | number |  |
| `traps1090` | boolean | 1090 MHz is above the cut-off |

<a id="t-radio-PatternInfo"></a>

#### radio.PatternInfo

PatternInfo is the fully resolved parameter set of a pattern model: what the user configured plus everything that had to be estimated.

| Field | Type | Notes |
|---|---|---|
| `kind` | string | model.Ant* constant |
| `directional` | boolean | single-beam antenna (yagi, patch); false = omni |
| `peakGainDbi` | number |  |
| `azimuthDeg` | number | boresight azimuth (0 for an omni) |
| `tiltDeg` | number | boresight / beam-peak elevation |
| `hBeamwidthDeg` | number | 360 for an omni |
| `vBeamwidthDeg` | number |  |
| `frontToBackDb` | number | 0 for an omni |
| `sidelobeDb` | number | sidelobe envelope below the peak; 0 = none |
| `floorDb` | number | omni: deepest null below the peak |
| `estimated` | string[] | Estimated lists the model.Antenna JSON field names whose values were not configured and had to be derived ("gainDbi", "hBeamwidthDeg", "vBeamwidthDeg", "frontToBackDb"). |

<a id="t-refdata-AircraftType"></a>

#### refdata.AircraftType

AircraftType is one entry of the embedded ICAO type designator table (ICAO Doc 8643).

| Field | Type | Notes |
|---|---|---|
| `designator` | string | e.g. "A20N" |
| `manufacturer` | string | e.g. "Airbus"; "" for generic designators |
| `model` | string | e.g. "A320neo" |
| `name` | string | manufacturer + model, e.g. "Airbus A320neo" |
| `description` | string | *optional* - Description is the ICAO description code: airframe class letter, engine count, engine type (e.g. "L2J"); "" when the designator has none. |
| `wtc` | string | *optional* - WTC is the ICAO wake turbulence category: "L", "M", "H", "J" (super) or "L/M" (depends on the variant); "" when not applicable. |
| `role` | string |  |

<a id="t-refdata-CallsignInfo"></a>

#### refdata.CallsignInfo

CallsignInfo is the interpretation of a flight identification.

| Field | Type | Notes |
|---|---|---|
| `callsign` | string | normalised: trimmed, upper case |
| `kind` | string (`unknown` \| `flight` \| `registration` \| `military` \| `other`) |  |
| `designator` | string | *optional* - Flight callsigns: the three-letter designator and the rest. Designator is set for every callsign of the form LLL + digit..., even when the operator is not in the embedded table (then Operator is ""). |
| `flightNumber` | string | *optional* |
| `operator` | string | *optional* - operator name |
| `operatorType` | string | *optional* - Operator* constant |
| `telephony` | string | *optional* - radiotelephony callsign |
| `country` | string | *optional* - operator / registration country |
| `countryCode` | string | *optional* - ISO 3166-1 alpha-2 |
| `military` | boolean | *optional* - military operator or military word callsign |
| `reg` | string | *optional* - Registration is the formatted mark ("G-BYTE", "N123AB") when Kind is CallsignRegistration. |

<a id="t-refdata-Progress"></a>

#### refdata.Progress

Progress is a snapshot of a running import or parse.

| Field | Type | Notes |
|---|---|---|
| `source` | string | source ID ("" for a bare Parse) |
| `url` | string | URL being downloaded ("" for a bare Parse) |
| `stage` | string | Stage: "connect" (before the first byte), "download" (records are flowing) or "done" (final call of a successful run). |
| `bytesRead` | integer | BytesRead counts bytes consumed from the input (compressed bytes for a gzip file); BytesTotal is the expected total, -1 when unknown. |
| `bytesTotal` | integer |  |
| `records` | integer | records delivered to the sink |
| `skipped` | integer | malformed lines ignored |
| `empty` | integer | well-formed records without any information, not delivered |

<a id="t-refdata-Source"></a>

#### refdata.Source

Source describes a downloadable aircraft database, including what the UI has to show as attribution.

| Field | Type | Notes |
|---|---|---|
| `id` | string |  |
| `name` | string |  |
| `url` | string | download URL (for OpenSky: the newest snapshot known at build time) |
| `homepage` | string | where a human can read about the data |
| `format` | string |  |
| `licence` | string |  |
| `licenceUrl` | string |  |
| `attribution` | string | text to show next to the data |
| `notes` | string |  |
| `approxBytes` | integer | ApproxBytes is the approximate download size and ApproxRecords the approximate number of airframes (as of late 2026), for the UI. |
| `approxRecords` | integer |  |

<a id="t-refdata-SquawkInfo"></a>

#### refdata.SquawkInfo

SquawkInfo describes a Mode A code with a conventional meaning.

| Field | Type | Notes |
|---|---|---|
| `code` | string | four octal digits |
| `meaning` | string | short label |
| `region` | string | Region says where the meaning applies: "Worldwide" (ICAO) or a list of states / regions. The same code can mean something else elsewhere. |
| `emergency` | string | *optional* - Emergency is the model.Aircraft.Emergency state the code implies ("general", "nordo", "unlawful"), "" for non-emergency codes. |
| `kind` | string | Kind is "emergency", "conspicuity" (general VFR / IFR codes that many aircraft set at the same time) or "special" (a particular activity). |

<a id="t-rfstat-AdviceItem"></a>

#### rfstat.AdviceItem

AdviceItem is one piece of advice with its stable code and numbers.

| Field | Type | Notes |
|---|---|---|
| `code` | string |  |
| `text` | string | the same sentence as in Health.Advice |
| `data` | { [key: string]: number \| null } |  |

<a id="t-rfstat-AircraftFad"></a>

#### rfstat.AircraftFad

AircraftFad is the fading state of one aircraft.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `kDb` | number \| null |  |
| `stdDb` | number \| null |  |
| `windows` | integer |  |
| `distKm` | number \| null |  |
| `elevDeg` | number \| null |  |

<a id="t-rfstat-AircraftPd"></a>

#### rfstat.AircraftPd

AircraftPd is the lifetime reception probability of one active aircraft.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `distKm` | number \| null |  |
| `pdPos` | number \| null |  |
| `pdVel` | number \| null |  |
| `pdIdent` | number \| null |  |
| `rssi` | number \| null |  |

<a id="t-rfstat-AircraftRate"></a>

#### rfstat.AircraftRate

AircraftRate is a per-aircraft surveillance reply rate.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `rate` | number \| null |  |
| `replies` | integer |  |

<a id="t-rfstat-AltBandDelta"></a>

#### rfstat.AltBandDelta

AltBandDelta is the GNSS-minus-baro difference in one altitude band.

| Field | Type | Notes |
|---|---|---|
| `band` | string |  |
| `meanFt` | number \| null |  |
| `stdFt` | number \| null |  |
| `n` | integer |  |

<a id="t-rfstat-AspectStats"></a>

#### rfstat.AspectStats

AspectStats: RSSI versus aspect (0 = nose-on, 180 = tail-on), relative to each aircraft's own mean, i.e. airframe shadowing of the transponder antenna.

| Field | Type | Notes |
|---|---|---|
| `fromDeg` | (number \| null)[] |  |
| `meanDb` | (number \| null)[] |  |
| `stdDb` | (number \| null)[] |  |
| `n` | integer[] |  |
| `noseDb` | number \| null | 0-30 |
| `beamDb` | number \| null | 60-120 |
| `tailDb` | number \| null | 150-180 |
| `noseTailDb` | number \| null |  |

<a id="t-rfstat-BDSCount"></a>

#### rfstat.BDSCount

BDSCount is the Comm-B register mix.

| Field | Type | Notes |
|---|---|---|
| `bds` | string |  |
| `count` | integer |  |
| `frac` | number \| null |  |

<a id="t-rfstat-Burst"></a>

#### rfstat.Burst

Burst is a second with an abnormally high frame or error rate.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms |
| `frames` | integer |  |
| `expected` | number \| null | EWMA rate before the burst |
| `badFrac` | number \| null |  |

<a id="t-rfstat-CensusStats"></a>

#### rfstat.CensusStats

CensusStats is section 6.

| Field | Type | Notes |
|---|---|---|
| `aircraft` | integer |  |
| `adsb` | integer |  |
| `versions` | [rfstat.Count](#t-rfstat-Count)[] |  |
| `nic` | [rfstat.Count](#t-rfstat-Count)[] |  |
| `nacp` | [rfstat.Count](#t-rfstat-Count)[] |  |
| `sil` | [rfstat.Count](#t-rfstat-Count)[] |  |
| `poorIntegrityShare` | number \| null |  |
| `categories` | [rfstat.Count](#t-rfstat-Count)[] |  |
| `tc28Share` | number \| null |  |
| `tc29Share` | number \| null |  |
| `tc31Share` | number \| null |  |
| `gnssAltShare` | number \| null |  |
| `gnssDelta` | [rfstat.Hist](#t-rfstat-Hist) |  |
| `gnssDeltaMeanFt` | number \| null |  |
| `gnssDeltaByAlt` | [rfstat.AltBandDelta](#t-rfstat-AltBandDelta)[] |  |
| `jumpAircraft` | integer |  |
| `degradedNow` | integer |  |
| `degradedList` | string[] |  |
| `gnssIndexNow` | number \| null |  |
| `gnssSeries` | [rfstat.GNSSPoint](#t-rfstat-GNSSPoint)[] |  |
| `gnssCells` | [rfstat.GNSSCell](#t-rfstat-GNSSCell)[] |  |
| `droppedFrames` | integer | aircraft table full |

<a id="t-rfstat-ChannelStats"></a>

#### rfstat.ChannelStats

ChannelStats is section 3.

| Field | Type | Notes |
|---|---|---|
| `seconds` | number \| null |  |
| `frames` | integer |  |
| `fps` | number \| null |  |
| `byDf` | [rfstat.DFRate](#t-rfstat-DFRate)[] |  |
| `byCrc` | { [key: string]: integer } |  |
| `occupancyPct` | number \| null | air time of received replies / time |
| `peakOccupancyPct` | number \| null | max over the last 10 min (1 s) |
| `meanReplyUs` | number \| null |  |
| `crcBadFrac` | number \| null |  |
| `fixedFrac` | number \| null |  |
| `rejectedFrac` | number \| null | rejected / all address-parity frames |
| `failFrac` | number \| null | (bad + rejected) / frames |
| `overlapProb` | number \| null | Poisson model: probability that another reply overlaps a given one at the measured rate: 1 - exp(-2 * lambda * tau). |
| `impliedRate` | number \| null | ImpliedRate: channel rate (replies/s) that would explain the observed failure fraction by overlaps alone. >> FPS means many undecoded replies (FRUIT). |
| `excessFail` | number \| null | ExcessFail = FailFrac - OverlapProb (> 0: noise/interference beyond garble). |
| `vsLoad` | [rfstat.LoadBin](#t-rfstat-LoadBin)[] |  |
| `recent` | [rfstat.Series](#t-rfstat-Series) |  |
| `bursts` | [rfstat.Burst](#t-rfstat-Burst)[] |  |
| `burstCount` | integer |  |

<a id="t-rfstat-Count"></a>

#### rfstat.Count

Count is a labelled count with share.

| Field | Type | Notes |
|---|---|---|
| `label` | string |  |
| `count` | integer |  |
| `share` | number \| null |  |

<a id="t-rfstat-DetectionStats"></a>

#### rfstat.DetectionStats

DetectionStats is section 2.

| Field | Type | Notes |
|---|---|---|
| `types` | [rfstat.PdType](#t-rfstat-PdType)[] |  |
| `vsDistance` | [rfstat.PdAxis](#t-rfstat-PdAxis) |  |
| `vsElev` | [rfstat.PdAxis](#t-rfstat-PdAxis) |  |
| `vsBearing` | [rfstat.PdAxis](#t-rfstat-PdAxis) |  |
| `vsRssi` | [rfstat.PdAxis](#t-rfstat-PdAxis) |  |
| `range50Km` | number \| null | position Pd crosses 50 % |
| `range90Km` | number \| null | position Pd crosses 90 % |
| `gaps` | [rfstat.GapBand](#t-rfstat-GapBand)[] |  |
| `aircraft` | [rfstat.AircraftPd](#t-rfstat-AircraftPd)[] |  |

<a id="t-rfstat-DFRate"></a>

#### rfstat.DFRate

DFRate is the count/rate of one downlink format.

| Field | Type | Notes |
|---|---|---|
| `df` | integer |  |
| `label` | string |  |
| `count` | integer |  |
| `rate` | number \| null | per second |

<a id="t-rfstat-ElevK"></a>

#### rfstat.ElevK

ElevK is the mean K factor in one elevation band.

| Field | Type | Notes |
|---|---|---|
| `label` | string |  |
| `kDb` | number \| null |  |
| `n` | integer |  |

<a id="t-rfstat-FadingStats"></a>

#### rfstat.FadingStats

FadingStats: short-window (FadeWindow) fading of range-normalised RSSI.

| Field | Type | Notes |
|---|---|---|
| `windows` | integer |  |
| `kMedianDb` | number \| null | Rician K (dB), moment method |
| `stdMedianDb` | number \| null |  |
| `kHist` | [rfstat.Hist](#t-rfstat-Hist) | K in dB |
| `stdHist` | [rfstat.Hist](#t-rfstat-Hist) | RSSI std dev in dB |
| `kByElev` | [rfstat.ElevK](#t-rfstat-ElevK)[] |  |
| `worst` | [rfstat.AircraftFad](#t-rfstat-AircraftFad)[] | strongest fading among active aircraft |

<a id="t-rfstat-GapBand"></a>

#### rfstat.GapBand

GapBand: position update interval statistics in one range band.

| Field | Type | Notes |
|---|---|---|
| `loKm` | number \| null |  |
| `hiKm` | number \| null | null = open |
| `n` | integer |  |
| `p50` | number \| null |  |
| `p90` | number \| null |  |
| `p95` | number \| null |  |
| `p99` | number \| null |  |
| `mean` | number \| null |  |
| `tSec` | (number \| null)[] | PUpdate[i] = time-weighted probability that the latest position is not older than TSec[i] seconds (sum(min(gap,T))/sum(gap)). |
| `pUpdate` | (number \| null)[] |  |

<a id="t-rfstat-GNSSCell"></a>

#### rfstat.GNSSCell

GNSSCell is one polar map cell (10 deg x 50 km) of the interference map.

| Field | Type | Notes |
|---|---|---|
| `sector` | integer | 0..35 (x10 deg) |
| `ring` | integer | 0..9 (x50 km) |
| `lat` | number \| null |  |
| `lon` | number \| null |  |
| `tot` | integer | aircraft-minutes |
| `deg` | integer |  |
| `index` | number \| null |  |

<a id="t-rfstat-GNSSPoint"></a>

#### rfstat.GNSSPoint

GNSSPoint is one minute of the GNSS interference index.

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `index` | number \| null | % of ADS-B aircraft with degraded integrity |
| `tot` | integer |  |
| `deg` | integer |  |
| `jumps` | integer |  |

<a id="t-rfstat-Health"></a>

#### rfstat.Health

Health is section 7: headline scores with plain-language advice.

| Field | Type | Notes |
|---|---|---|
| `score` | integer | 0..100 overall, -1 = no data |
| `status` | string |  |
| `items` | [rfstat.HealthItem](#t-rfstat-HealthItem)[] |  |
| `blockedSectors` | [rfstat.Sector](#t-rfstat-Sector)[] |  |
| `advice` | string[] |  |
| `adviceItems` | [rfstat.AdviceItem](#t-rfstat-AdviceItem)[] | AdviceItems are the Advice sentences, in the same order, with their codes and numbers. |

<a id="t-rfstat-HealthItem"></a>

#### rfstat.HealthItem

HealthItem is one scored aspect of RF health.

| Field | Type | Notes |
|---|---|---|
| `key` | string |  |
| `label` | string |  |
| `score` | integer | 0..100, -1 = not enough data |
| `status` | string | ok \| warn \| bad \| unknown |
| `text` | string |  |
| `code` | string | Code is the stable id of the message kind ("gain.too-high", ...) and Data the numbers printed in Text under unit-suffixed names, so a UI can re-format them in the viewer's units (codes and fields: WIRING.md section 6). Text is unchanged. |
| `data` | { [key: string]: number \| null } |  |

<a id="t-rfstat-Hist"></a>

#### rfstat.Hist

Hist is a histogram with equal bins: bin i covers [Lo+i*Step, Lo+(i+1)*Step).

| Field | Type | Notes |
|---|---|---|
| `lo` | number \| null |  |
| `step` | number \| null |  |
| `counts` | (number \| null)[] |  |

<a id="t-rfstat-InterrogationStats"></a>

#### rfstat.InterrogationStats

InterrogationStats is section 4.

| Field | Type | Notes |
|---|---|---|
| `codes` | [rfstat.Interrogator](#t-rfstat-Interrogator)[] |  |
| `activeRadars` | integer | codes != II0 with >= 10 replies in 10 min |
| `survRateP50` | number \| null |  |
| `survRateP90` | number \| null |  |
| `survRateMax` | number \| null |  |
| `topSurv` | [rfstat.AircraftRate](#t-rfstat-AircraftRate)[] |  |
| `commB` | [rfstat.BDSCount](#t-rfstat-BDSCount)[] |  |
| `df0Rate` | number \| null |  |
| `df16Rate` | number \| null |  |
| `raReports` | integer |  |
| `acasAircraft` | integer |  |
| `activeAircraft` | integer | heard within 2 min |
| `adsbAndModeS` | integer |  |
| `adsbOnly` | integer |  |
| `modeSOnly` | integer |  |
| `radarShare` | number \| null | share of ADS-B aircraft also under Mode S surveillance |

<a id="t-rfstat-Interrogator"></a>

#### rfstat.Interrogator

Interrogator is one interrogator code seen in DF11 replies.

| Field | Type | Notes |
|---|---|---|
| `code` | string | "II 3" or "SI 12" |
| `iid` | integer |  |
| `replies` | integer |  |
| `rate` | number \| null | per second over the window |
| `last10m` | integer | replies in the last 10 minutes |
| `aircraft` | integer | aircraft that replied within 2 min |
| `replyingFrac` | number \| null | ReplyingFrac = Aircraft / aircraft under Mode S surveillance (2 min). |
| `lockoutLikely` | boolean |  |
| `timeline` | integer[] | Timeline: replies per minute, oldest first, last 60 minutes. |
| `acqSquitter` | boolean | *optional* - AcqSquitter: II 0 also contains spontaneous DF11 acquisition squitters. |

<a id="t-rfstat-LoadBin"></a>

#### rfstat.LoadBin

LoadBin: failure rate versus the per-second frame rate.

| Field | Type | Notes |
|---|---|---|
| `loFps` | number \| null |  |
| `hiFps` | number \| null |  |
| `seconds` | integer |  |
| `meanFps` | number \| null |  |
| `failFrac` | number \| null |  |
| `modelFailFrac` | number \| null | Poisson overlap prediction |

<a id="t-rfstat-MultipathStats"></a>

#### rfstat.MultipathStats

MultipathStats: periodic RSSI ripple versus elevation angle (two-ray / Lloyd's mirror lobing from a ground reflection).

| Field | Type | Notes |
|---|---|---|
| `elevDeg` | (number \| null)[] | bin centres with data |
| `devDb` | (number \| null)[] | detrended mean deviation per bin |
| `rippleDb` | number \| null | RMS of the detrended deviation |
| `reflectorHeightM` | number \| null | ReflectorHeightM = lambda / (2 * period in sin(elev)): height of the antenna above the dominant reflecting surface. |
| `periodDeg` | number \| null | lobe spacing near 2 deg elevation |
| `strength` | number \| null | 0..1 periodogram peak (1 = pure sinusoid) |
| `detected` | boolean |  |
| `bins` | integer |  |

<a id="t-rfstat-NonCompliant"></a>

#### rfstat.NonCompliant

NonCompliant is an aircraft whose squitter timing looks non-compliant.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `intervals` | integer |  |
| `badFrac` | number \| null |  |
| `mean` | number \| null |  |
| `std` | number \| null |  |
| `reason` | string |  |

<a id="t-rfstat-PdAxis"></a>

#### rfstat.PdAxis

PdAxis is probability of detection along one axis. Index i of every slice is the same bin; Pos/Vel/Ident are null where too few squitters were expected.

| Field | Type | Notes |
|---|---|---|
| `lo` | (number \| null)[] |  |
| `hi` | (number \| null)[] |  |
| `pos` | (number \| null)[] |  |
| `vel` | (number \| null)[] |  |
| `ident` | (number \| null)[] |  |
| `expected` | (number \| null)[] | expected position squitters per bin |

<a id="t-rfstat-PdType"></a>

#### rfstat.PdType

PdType is the total reception probability of one squitter type.

| Field | Type | Notes |
|---|---|---|
| `type` | string |  |
| `nominalRate` | number \| null | per second |
| `expected` | number \| null |  |
| `decoded` | number \| null |  |
| `pd` | number \| null |  |

<a id="t-rfstat-Report"></a>

#### rfstat.Report

Report is the full snapshot served by GET /api/rf/report.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | snapshot time (ms) |
| `windowSec` | number \| null | effective length of the sliding window (s) |
| `since` | integer | engine start (ms) |
| `live` | boolean | sliding ~1 h window (false = one closed hour) |
| `signal` | [rfstat.SignalStats](#t-rfstat-SignalStats) |  |
| `detection` | [rfstat.DetectionStats](#t-rfstat-DetectionStats) |  |
| `channel` | [rfstat.ChannelStats](#t-rfstat-ChannelStats) |  |
| `interrogation` | [rfstat.InterrogationStats](#t-rfstat-InterrogationStats) |  |
| `timing` | [rfstat.TimingStats](#t-rfstat-TimingStats) |  |
| `census` | [rfstat.CensusStats](#t-rfstat-CensusStats) |  |
| `health` | [rfstat.Health](#t-rfstat-Health) |  |

<a id="t-rfstat-Sector"></a>

#### rfstat.Sector

Sector is a bearing sector with low detection probability.

| Field | Type | Notes |
|---|---|---|
| `fromDeg` | integer |  |
| `toDeg` | integer |  |
| `pd` | number \| null |  |
| `medianPd` | number \| null |  |

<a id="t-rfstat-Series"></a>

#### rfstat.Series

Series is a per-second time series (index i = T0 + i seconds).

| Field | Type | Notes |
|---|---|---|
| `t0` | integer |  |
| `frames` | integer[] |  |
| `bad` | integer[] |  |
| `occPct` | (number \| null)[] |  |

<a id="t-rfstat-SignalBand"></a>

#### rfstat.SignalBand

SignalBand summarises the RSSI distribution of one antenna/ring/elevation band.

| Field | Type | Notes |
|---|---|---|
| `label` | string |  |
| `lo` | number \| null | band lower bound (km or deg), null if open |
| `hi` | number \| null |  |
| `n` | integer |  |
| `p05` | number \| null |  |
| `p50` | number \| null |  |
| `p95` | number \| null |  |
| `mean` | number \| null |  |
| `strongFrac` | number \| null | StrongFrac = share within 3 dB of full scale. |
| `hist` | [rfstat.Hist](#t-rfstat-Hist) | *optional* |

<a id="t-rfstat-SignalStats"></a>

#### rfstat.SignalStats

SignalStats is section 1.

| Field | Type | Notes |
|---|---|---|
| `frames` | integer | frames with a signal level |
| `all` | [rfstat.SignalBand](#t-rfstat-SignalBand) |  |
| `perAntenna` | [rfstat.SignalBand](#t-rfstat-SignalBand)[] |  |
| `perRing` | [rfstat.SignalBand](#t-rfstat-SignalBand)[] | 25 km rings |
| `perElev` | [rfstat.SignalBand](#t-rfstat-SignalBand)[] |  |
| `decodeFloorDbfs` | number \| null | 1st percentile = weakest reliably decodable level |
| `noiseFloorEstDbfs` | number \| null | DecodeFloor - assumed decode SNR |
| `assumedDecodeSnrDb` | number \| null |  |
| `p50Dbfs` | number \| null |  |
| `p999Dbfs` | number \| null |  |
| `dynamicRangeDb` | number \| null | p99.9 - p0.1 |
| `snrMarginDb` | number \| null | p50 - DecodeFloor |
| `snrEstDb` | number \| null | p50 - NoiseFloorEst |
| `strongFrac` | number \| null | >= -3 dBFS |
| `nearClipFrac` | number \| null | >= -1 dBFS |
| `fading` | [rfstat.FadingStats](#t-rfstat-FadingStats) |  |
| `multipath` | [rfstat.MultipathStats](#t-rfstat-MultipathStats) |  |
| `aspect` | [rfstat.AspectStats](#t-rfstat-AspectStats) |  |

<a id="t-rfstat-TimingStats"></a>

#### rfstat.TimingStats

TimingStats is section 5.

| Field | Type | Notes |
|---|---|---|
| `available` | boolean |  |
| `types` | [rfstat.TimingType](#t-rfstat-TimingType)[] |  |
| `altPairs` | integer | consecutive position pairs (no loss) |
| `altRegularity` | number \| null | share that alternate even/odd |
| `alt2Pairs` | integer | pairs with one lost squitter |
| `alt2Same` | number \| null | share with equal parity |
| `nonCompliant` | [rfstat.NonCompliant](#t-rfstat-NonCompliant)[] |  |
| `note` | string |  |

<a id="t-rfstat-TimingType"></a>

#### rfstat.TimingType

TimingType: GPS-timestamp interval statistics for one squitter type.

| Field | Type | Notes |
|---|---|---|
| `type` | string |  |
| `intervals` | integer |  |
| `badFrac` | number \| null | intervals impossible for a compliant transmitter |
| `hist` | [rfstat.Hist](#t-rfstat-Hist) |  |
| `nominalLo` | number \| null | Single-interval (no loss) statistics inside the nominal [lo,hi] range. |
| `nominalHi` | number \| null |  |
| `mean` | number \| null |  |
| `std` | number \| null |  |
| `chi2` | number \| null | 10 sub-bins vs uniform, df = 9 |
| `uniformOk` | boolean |  |

<a id="t-routes-AirportRef"></a>

#### routes.AirportRef

AirportRef is an airport for drawing and aliasing (IATA <-> ICAO).

| Field | Type | Notes |
|---|---|---|
| `code` | string | ICAO (else ident) |
| `iata` | string | *optional* |
| `name` | string | *optional* |
| `lat` | number |  |
| `lon` | number |  |
| `elevFt` | number | *optional* |
| `size` | integer | *optional* |

<a id="t-routes-Claim"></a>

#### routes.Claim

Claim is one source asserting a route.

| Field | Type | Notes |
|---|---|---|
| `Source` | string |  |
| `Logit` | number |  |
| `Why` | string | short evidence text, e.g. "learned 9 days" |
| `Variant` | boolean | claimed for a variant spelling of the callsign |

<a id="t-routes-Detail"></a>

#### routes.Detail

Detail is the full route view of a callsign.

| Field | Type | Notes |
|---|---|---|
| `callsign` | string |  |
| `lookupable` | boolean |  |
| `blocked` | boolean | *optional* |
| `override` | [routes.Override](#t-routes-Override) | *optional* |
| `route` | [model.RouteShort](#t-model-RouteShort) | *optional* |
| `hypotheses` | [routes.HypView](#t-routes-HypView)[] |  |
| `path` | number[][] | *optional* - great circle [lon,lat] of the shown route (lon unwrapped) |
| `hidden` | integer | *optional* |
| `learned` | [routes.Learned](#t-routes-Learned)[] | *optional* |
| `online` | string | *optional* - "", "queried", "not found", error text |

<a id="t-routes-HypView"></a>

#### routes.HypView

HypView is one hypothesis in the detail view.

| Field | Type | Notes |
|---|---|---|
| `airports` | string |  |
| `stops` | [routes.Stop](#t-routes-Stop)[] |  |
| `sources` | [routes.Claim](#t-routes-Claim)[] |  |
| `logit` | number |  |
| `p` | number |  |
| `score` | [routes.Score](#t-routes-Score) |  |
| `lic` | string |  |

<a id="t-routes-Learned"></a>

#### routes.Learned

Learned is a promoted route of a callsign.

| Field | Type | Notes |
|---|---|---|
| `callsign` | string |  |
| `rank` | integer |  |
| `airports` | string |  |
| `p` | number |  |
| `days` | integer |  |
| `lastT` | integer |  |
| `todMin` | integer | typical departure time of day, minutes UTC |
| `staleStanding` | boolean | *optional* |

<a id="t-routes-Leg"></a>

#### routes.Leg

Leg is one observed flight leg handed to the learner when a flight closes.

| Field | Type | Notes |
|---|---|---|
| `flight` | integer |  |
| `callsign` | string |  |
| `start` | integer | Unix s |
| `end` | integer |  |
| `dep` | string | *optional* |
| `depConf` | number | *optional* |
| `depTier` | string | *optional* - T1 \| T2 \| T3 |
| `arr` | string | *optional* |
| `arrConf` | number | *optional* |
| `arrTier` | string | *optional* - L1 \| L2 \| L5 |
| `route` | string | *optional* - route shown at close (open/own classes only) |
| `routeSrc` | string | *optional* |

<a id="t-routes-Override"></a>

#### routes.Override

Override is one operator correction.

| Field | Type | Notes |
|---|---|---|
| `callsign` | string |  |
| `airports` | string | codes in flight order joined with "-" ("" for block) |
| `mode` | string |  |
| `note` | string | *optional* |
| `author` | string | *optional* |
| `created` | integer | *optional* - Unix s |
| `expires` | integer | *optional* - Unix s, 0 = never |
| `locked` | boolean | *optional* - from the overrides file |

<a id="t-routes-OverrideLog"></a>

#### routes.OverrideLog

OverrideLog is one audit entry.

| Field | Type | Notes |
|---|---|---|
| `id` | integer |  |
| `callsign` | string |  |
| `before` | string |  |
| `after` | string |  |
| `author` | string |  |
| `t` | integer |  |

<a id="t-routes-Score"></a>

#### routes.Score

Score is the evaluation of one route at one position.

| Field | Type | Notes |
|---|---|---|
| `state` | string |  |
| `score` | number | smaller = better; fits <= 1.5 < uncertain < 4 <= doubtful |
| `z` | number | detour (ellipse) term of the active leg |
| `leg` | integer | 0-based active leg |
| `along` | number | 0..100 along the active leg |
| `distRem` | number | remaining great-circle distance to the active leg's end |
| `reason` | string |  |

<a id="t-routes-Stats"></a>

#### routes.Stats

Stats are counters for the status endpoint.

| Field | Type | Notes |
|---|---|---|
| `entries` | integer |  |
| `resolved` | integer |  |
| `onlineQueries` | integer |  |
| `onlineFound` | integer |  |
| `onlineErrors` | integer |  |
| `lastOnlineError` | string | *optional* |
| `legsRecorded` | integer |  |
| `obsAdded` | integer |  |
| `learnedUpdates` | integer |  |

<a id="t-routes-Stop"></a>

#### routes.Stop

Stop is one airport of a route.

| Field | Type | Notes |
|---|---|---|
| `code` | string |  |
| `iata` | string | *optional* |
| `name` | string | *optional* |
| `lat` | number |  |
| `lon` | number |  |
| `elevFt` | number | NaN = unknown |
| `size` | integer | navdata.SizeClass: 3 large, 2 medium, 1 small |
| `known` | boolean | resolved from the airport data |

<a id="t-server-acdbInstalled"></a>

#### server.acdbInstalled

| Field | Type | Notes |
|---|---|---|
| `installed` | boolean | an import was recorded |
| `rows` | integer | rows in the aircraft database |
| `info` | [engine.ACDBImportInfo](#t-engine-ACDBImportInfo) \| null | what was imported, null = nothing yet |

<a id="t-server-acdbLookupReply"></a>

#### server.acdbLookupReply

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `found` | boolean |  |
| `record` | [store.ACDBEntry](#store-rows) \| null |  |
| `info` | [server.aircraftInfo](#t-server-aircraftInfo) |  |

<a id="t-server-acdbSourcesReply"></a>

#### server.acdbSourcesReply

| Field | Type | Notes |
|---|---|---|
| `sources` | [refdata.Source](#t-refdata-Source)[] |  |
| `installed` | [server.acdbInstalled](#t-server-acdbInstalled) |  |
| `status` | [engine.ImportStatus](#t-engine-ImportStatus) |  |

<a id="t-server-aircraftDetail"></a>

#### server.aircraftDetail

| Field | Type | Notes |
|---|---|---|
| `aircraft` | [model.Aircraft](#t-model-Aircraft) \| null | live state, null when not currently tracked |
| `trail` | TrailPoint[] (compact arrays `[tMs, lat, lon, altFt\|null, gsKt\|null, rssi\|null, src]`) | live in-memory trail |
| `info` | [server.aircraftInfo](#t-server-aircraftInfo) |  |
| `acdb` | [store.ACDBEntry](#store-rows) \| null | record of the imported aircraft database, null = none |
| `seen` | [store.AircraftRow](#store-rows) \| null | all-time totals of this airframe, null = never stored |
| `flights` | [store.Flight](#store-rows)[] | newest first |
| `watch` | [server.watchInfo](#t-server-watchInfo) \| null | alert watch-list membership, null = none |

<a id="t-server-aircraftInfo"></a>

#### server.aircraftInfo

aircraftInfo is the reference data of one address: allocation tables, aircraft database, type table and the interpretation of the callsign.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `country` | string | *optional* - Country / CountryCode: state (or territory, see AllocTerritory) the address block is allocated to. Empty when Alloc is not "state" or "territory". |
| `countryCode` | string | *optional* - ISO 3166-1 alpha-2 |
| `state` | string | *optional* - State / StateCode: the ICAO contracting state the block was allocated to. Differs from Country only for territory sub-blocks (Bermuda -> United Kingdom, Hong Kong -> China). |
| `stateCode` | string | *optional* |
| `alloc` | string (`state` \| `territory` \| `region` \| `reserved` \| `none`) | Alloc classifies the address block; Region names the ICAO regional pool or reserved range for addresses outside the directly allocated blocks. |
| `region` | string | *optional* |
| `military` | boolean | *optional* - Military is the heuristic military-block flag (see IsMilitary). |
| `reg` | string | *optional* - Registration is the mark derived arithmetically from the address (see Registration); "" when the state does not allocate that way. It is a heuristic: a record from the imported aircraft database takes precedence. |
| `type` | string | *optional* - ICAO type designator |
| `typeDesc` | string | *optional* - e.g. "Airbus A320neo" |
| `typeClass` | string | *optional* - e.g. "Landplane, 2 jet engines" |
| `typeInfo` | [refdata.AircraftType](#t-refdata-AircraftType) | *optional* |
| `wtc` | string | *optional* - wake turbulence category text |
| `operator` | string | *optional* |
| `year` | integer | *optional* - year built |
| `categoryDesc` | string | *optional* - ADS-B emitter category in words |
| `callsign` | [refdata.CallsignInfo](#t-refdata-CallsignInfo) | *optional* |
| `spoken` | string | *optional* - radiotelephony form of the callsign |
| `squawk` | [refdata.SquawkInfo](#t-refdata-SquawkInfo) | *optional* |
| `interesting` | boolean | *optional* - aircraft database flag |
| `pia` | boolean | *optional* |
| `ladd` | boolean | *optional* |

<a id="t-server-aircraftListReply"></a>

#### server.aircraftListReply

| Field | Type | Notes |
|---|---|---|
| `total` | integer | rows matching the search |
| `limit` | integer |  |
| `offset` | integer |  |
| `sort` | string |  |
| `desc` | boolean |  |
| `rows` | [store.AircraftRow](#store-rows)[] |  |

<a id="t-server-astroDayReply"></a>

#### server.astroDayReply

| Field | Type | Notes |
|---|---|---|
| `located` | boolean |  |
| `receiver` | [analytics.LatLon](#t-analytics-LatLon) \| null |  |
| `days` | [astro.EventsMs](#t-astro-EventsMs)[] | one entry per day, starting today |

<a id="t-server-azimuthReply"></a>

#### server.azimuthReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean |  |
| `azimuth` | [analytics.AzimuthPattern](#t-analytics-AzimuthPattern) \| null | measured pattern, null until there is enough data |
| `yagi` | [analytics.YagiFit](#t-analytics-YagiFit) \| null | lobe fitted to it |

<a id="t-server-budgetReply"></a>

#### server.budgetReply

| Field | Type | Notes |
|---|---|---|
| `antenna` | string |  |
| `altFt` | number |  |
| `budget` | [radio.Budget](#t-radio-Budget) |  |
| `systemTempK` | number \| null | The fixed part of the chain. |
| `systemNfDb` | number \| null |  |
| `noiseFloorDbm` | number \| null |  |
| `sensitivityDbm` | number \| null |  |
| `maxRangeKm` | number \| null | predicted maximum range on this bearing and altitude |
| `limit` | string | "horizon" \| "power" \| "none" |

<a id="t-server-callsignReply"></a>

#### server.callsignReply

| Field | Type | Notes |
|---|---|---|
| `callsign` | string | normalised: trimmed, upper case |
| `kind` | string (`unknown` \| `flight` \| `registration` \| `military` \| `other`) |  |
| `designator` | string | *optional* - Flight callsigns: the three-letter designator and the rest. Designator is set for every callsign of the form LLL + digit..., even when the operator is not in the embedded table (then Operator is ""). |
| `flightNumber` | string | *optional* |
| `operator` | string | *optional* - operator name |
| `operatorType` | string | *optional* - Operator* constant |
| `telephony` | string | *optional* - radiotelephony callsign |
| `country` | string | *optional* - operator / registration country |
| `countryCode` | string | *optional* - ISO 3166-1 alpha-2 |
| `military` | boolean | *optional* - military operator or military word callsign |
| `reg` | string | *optional* - Registration is the formatted mark ("G-BYTE", "N123AB") when Kind is CallsignRegistration. |
| `spoken` | string | *optional* |

<a id="t-server-cutJSON"></a>

#### server.cutJSON

| Field | Type | Notes |
|---|---|---|
| `angleDeg` | (number \| null)[] |  |
| `gainDbi` | (number \| null)[] |  |
| `relDb` | (number \| null)[] | relative to the peak of the cut |
| `peakDeg` | number \| null |  |
| `peakDbi` | number \| null |  |
| `beamwidthDeg` | number \| null | BeamwidthDeg is the -3 dB width of the cut, null when it has none. |

<a id="t-server-dbStatsReply"></a>

#### server.dbStatsReply

| Field | Type | Notes |
|---|---|---|
| `stats` | [store.Stats](#t-store-Stats) |  |
| `tables` | { [key: string]: integer } | row count per table |
| `storage` | [model.StorageConfig](#t-model-StorageConfig) | retention settings in force |
| `acdb` | [server.acdbInstalled](#t-server-acdbInstalled) |  |
| `tileCache` | [server.tileStats](#t-server-tileStats) |  |
| `profile` | [store.ProfileView](#t-store-ProfileView) | Profile is the resolved storage profile (storage.profile) and Migration the state of the background data migrations. |
| `migration` | [store.MigrationInfo](#t-store-MigrationInfo) |  |

<a id="t-server-dfReply"></a>

#### server.dfReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `df` | [analytics.DFCount](#t-analytics-DFCount)[] | stored counts over the range |
| `sources` | [analytics.SourceShare](#t-analytics-SourceShare) |  |
| `frames` | integer | frames in the range |
| `crcBad` | integer |  |
| `liveDf` | { [key: string]: integer } | Live counters since the program started (all antennas). accepted frames per downlink format: "df0" .. "df_other" |
| `liveBds` | { [key: string]: integer } | identified Comm-B registers: "4,0", "5,0", "6,0", "4,4" |

<a id="t-server-ductHour"></a>

#### server.ductHour

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `source` | string | "observation" \| "forecast" |
| `hasProfile` | boolean | HasProfile is true when a pressure-level profile was analysed; false when the gradient comes from the stored columns or, as a last resort, from the climatological relation with surface refractivity. |
| `estimated` | boolean | *optional* - Estimated is true when dN/dh had to be estimated from surface refractivity alone (dN = -7.32 exp(0.005577 Ns), CRPL exponential reference atmosphere). |
| `nSurface` | number | surface refractivity, N-units |
| `dNdh` | number | gradient over the lowest km, N/km |
| `minGradient` | number | most negative layer gradient below 3 km, N/km |
| `minGradientM` | number | base height of that layer, m AMSL |
| `k` | number | effective earth radius factor (radio.KMax = trapping) |
| `horizonFactor` | number | radio horizon relative to standard (capped at 3) |
| `index` | integer | radio.Assess index, 50 = standard, 85+ = ducting |
| `class` | string | radio.Class* |
| `ducts` | integer | ducts found in the profile |
| `ductStrengthM` | number | strongest duct, M-units (0 = none) |
| `ductKind` | string | *optional* - radio.Duct* of the strongest duct |
| `ductBaseM` | number | m AMSL |
| `ductTopM` | number | m AMSL |
| `ducting1090` | boolean | a duct able to trap 1090 MHz is present |
| `inversionC` | number \| null | InversionC is the temperature excess of the warmest level in the lowest 1.5 km over the 2 m temperature (positive = inversion), NaN without a profile. |
| `summary` | string | *optional* |

<a id="t-server-ductingReply"></a>

#### server.ductingReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `now` | integer |  |
| `stationElevM` | number |  |
| `current` | [predict.Atmos](#t-predict-Atmos) \| null | state of the current hour, null without weather data |
| `hours` | [server.ductHour](#t-server-ductHour)[] | observed hours, then forecast hours |
| `aircraft` | [met.RadioInversions](#t-met-RadioInversions) | inversions measured by aircraft right now |
| `ductHours` | integer |  |

<a id="t-server-elevationReply"></a>

#### server.elevationReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean |  |
| `elevation` | [analytics.ElevationPattern](#t-analytics-ElevationPattern) \| null |  |

<a id="t-server-elevationReply2"></a>

#### server.elevationReply2

| Field | Type | Notes |
|---|---|---|
| `lat` | number |  |
| `lon` | number |  |
| `elevationM` | number \| null | terrain height above mean sea level |
| `source` | string |  |
| `attribution` | string |  |

<a id="t-server-feedsReply"></a>

#### server.feedsReply

| Field | Type | Notes |
|---|---|---|
| `mode` | string | effective run mode |
| `demo` | boolean |  |
| `feeds` | [model.FeedStatus](#t-model-FeedStatus)[] | connection state per feed |
| `counters` | [tracker.FeedCounters](#t-tracker-FeedCounters)[] | decoded totals per feed |
| `antennas` | [tracker.AntennaCounters](#t-tracker-AntennaCounters)[] | totals per antenna, index 0 = "" (combined) |
| `hub` | [server.hubReply](#t-server-hubReply) |  |
| `receiverGps` | [model.ReceiverGPS](#t-model-ReceiverGPS) \| null |  |

<a id="t-server-fitReply"></a>

#### server.fitReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean |  |
| `yagi` | [analytics.YagiFit](#t-analytics-YagiFit) \| null |  |
| `notes` | string[] |  |

<a id="t-server-flightAirports"></a>

#### server.flightAirports

flightAirports are the departure / arrival airports guessed from the first / last low positions of a flight (needs imported airport data).

| Field | Type | Notes |
|---|---|---|
| `departure` | [navdata.AirportGuess](#t-navdata-AirportGuess) \| null | null = not near an airport when first seen |
| `arrival` | [navdata.AirportGuess](#t-navdata-AirportGuess) \| null | null = not near an airport when last seen |

<a id="t-server-flightReply"></a>

#### server.flightReply

| Field | Type | Notes |
|---|---|---|
| `flight` | [store.Flight](#store-rows) |  |
| `trail` | TrailPoint[] (compact arrays `[tMs, lat, lon, altFt\|null, gsKt\|null, rssi\|null, src]`) | stored (down-sampled) trail |
| `info` | [server.aircraftInfo](#t-server-aircraftInfo) |  |
| `live` | boolean | the flight is still being tracked |
| `airports` | [server.flightAirports](#t-server-flightAirports) | guessed departure / arrival airports |

<a id="t-server-flightsReply"></a>

#### server.flightsReply

| Field | Type | Notes |
|---|---|---|
| `limit` | integer |  |
| `offset` | integer |  |
| `flights` | [store.Flight](#store-rows)[] | newest first |

<a id="t-server-geoFormats"></a>

#### server.geoFormats

| Field | Type | Notes |
|---|---|---|
| `decimal` | string | "50.05000, 8.60000" |
| `dms` | string | degrees, minutes, seconds |
| `ddm` | string | degrees and decimal minutes |
| `maidenhead` | string | 6-character locator |
| `maidenhead8` | string | 8-character locator |

<a id="t-server-geoParseReply"></a>

#### server.geoParseReply

| Field | Type | Notes |
|---|---|---|
| `ok` | boolean |  |
| `error` | string | *optional* - why the text was not understood (ok = false) |
| `input` | string |  |
| `lat` | number \| null |  |
| `lon` | number \| null |  |
| `format` | string | *optional* - detected notation, e.g. "decimal degrees" |
| `formatted` | [server.geoFormats](#t-server-geoFormats) | *optional* |
| `distKm` | number \| null | Distance and bearing from the configured receiver (when located). |
| `bearing` | number \| null |  |

<a id="t-server-historyAircraftReply"></a>

#### server.historyAircraftReply

| Field | Type | Notes |
|---|---|---|
| `aircraft` | [model.Aircraft](#t-model-Aircraft) \| null | live state, null when not currently tracked |
| `trail` | TrailPoint[] (compact arrays `[tMs, lat, lon, altFt\|null, gsKt\|null, rssi\|null, src]`) | live in-memory trail |
| `info` | [server.aircraftInfo](#t-server-aircraftInfo) |  |
| `acdb` | [store.ACDBEntry](#store-rows) \| null | record of the imported aircraft database, null = none |
| `seen` | [store.AircraftRow](#store-rows) \| null | all-time totals of this airframe, null = never stored |
| `flights` | [store.Flight](#store-rows)[] | newest first |
| `watch` | [server.watchInfo](#t-server-watchInfo) \| null | alert watch-list membership, null = none |
| `events` | [store.Event](#store-rows)[] | emergencies, records ... of this airframe, newest first |

<a id="t-server-horizonAntenna"></a>

#### server.horizonAntenna

| Field | Type | Notes |
|---|---|---|
| `id` | string |  |
| `name` | string |  |
| `heightAglM` | number |  |
| `amslM` | number | feed point above mean sea level |
| `table` | [radio.BandRange](#t-radio-BandRange)[] | line of sight per altitude band |

<a id="t-server-horizonBand"></a>

#### server.horizonBand

| Field | Type | Notes |
|---|---|---|
| `band` | integer |  |
| `label` | string |  |
| `altFt` | number |  |
| `horizonKm` | number \| null | line of sight at the top of the band |
| `horizonTypKm` | number \| null | at the representative altitude |
| `bestKm` | number \| null | farthest reception in the band |
| `bestBrg` | number \| null |  |
| `meanPct` | number \| null | robust range as % of the horizon, mean over all bearings |

<a id="t-server-horizonReply"></a>

#### server.horizonReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean |  |
| `k` | number | effective earth radius factor used for the tables |
| `groundElevM` | number |  |
| `antennaAmslM` | number | height used for the measured comparison |
| `antennas` | [server.horizonAntenna](#t-server-horizonAntenna)[] |  |
| `bands` | [server.horizonBand](#t-server-horizonBand)[] | measured against the horizon |

<a id="t-server-hubReply"></a>

#### server.hubReply

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean |  |
| `port` | integer |  |
| `clients` | integer |  |
| `error` | string | *optional* - why the data port could not start |
| `clientList` | [hub.ClientInfo](#t-hub-ClientInfo)[] |  |
| `droppedClients` | integer | clients cut off for being too slow |

<a id="t-server-latencyAircraftDetail"></a>

#### server.latencyAircraftDetail

| Field | Type | Notes |
|---|---|---|
| `row` | [latency.AircraftRow](#t-latency-AircraftRow) |  |
| `waterfall` | [latency.Waterfall](#t-latency-Waterfall) |  |

<a id="t-server-latencyAircraftReply"></a>

#### server.latencyAircraftReply

| Field | Type | Notes |
|---|---|---|
| `aircraft` | [latency.AircraftRow](#t-latency-AircraftRow)[] |  |

<a id="t-server-latencyFeedReply"></a>

#### server.latencyFeedReply

| Field | Type | Notes |
|---|---|---|
| `feed` | string |  |
| `feeds` | string[] | every tracked feed ID |
| `series` | [latency.FeedMinute](#t-latency-FeedMinute)[] |  |

<a id="t-server-latencyReply"></a>

#### server.latencyReply

| Field | Type | Notes |
|---|---|---|
| `now` | integer |  |
| `clockOffsetMs` | number | NTP correction in use (true UTC - host clock) |
| `clockSynced` | boolean |  |
| `feeds` | [tracker.FeedLatency](#t-tracker-FeedLatency)[] | only feeds with GPS timestamps |
| `chain` | [latency.Summary](#t-latency-Summary) \| null | Chain is the latency-chain summary (internal/latency): stages, counters, per-feed clock filter, findings. Null when unavailable. |
| `histograms` | [latency.Histogram](#t-latency-Histogram)[] | Histograms are the stage histograms of the summary window. |

<a id="t-server-linkBudgetReply"></a>

#### server.linkBudgetReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean |  |
| `linkBudget` | [analytics.LinkRow](#t-analytics-LinkRow)[] |  |

<a id="t-server-magvarReply"></a>

#### server.magvarReply

| Field | Type | Notes |
|---|---|---|
| `lat` | number |  |
| `lon` | number |  |
| `altKm` | number |  |
| `t` | integer |  |
| `magVarDeg` | number \| null | declination, degrees east of true north |
| `inclDeg` | number \| null | inclination, degrees down |
| `totalNt` | number \| null | total field intensity |
| `horizNt` | number \| null | horizontal intensity |
| `model` | string |  |
| `decimalYear` | number \| null |  |

<a id="t-server-memoryInfo"></a>

#### server.memoryInfo

| Field | Type | Notes |
|---|---|---|
| `allocBytes` | integer | live heap objects |
| `heapInuseBytes` | integer |  |
| `sysBytes` | integer | obtained from the OS |
| `stackBytes` | integer |  |
| `numGc` | integer |  |
| `gcPauseMs` | number | most recent pause |

<a id="t-server-metCompareReply"></a>

#### server.metCompareReply

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `modelAvailable` | boolean | a weather-model profile exists for this hour |
| `modelT` | integer | hour of the model profile, Unix ms |
| `attribution` | string |  |
| `comparison` | [met.Comparison](#t-met-Comparison) | aircraft minus model |
| `radio` | [met.RadioInversions](#t-met-RadioInversions) |  |

<a id="t-server-metObsReply"></a>

#### server.metObsReply

| Field | Type | Notes |
|---|---|---|
| `now` | integer |  |
| `maxAgeS` | integer |  |
| `count` | integer |  |
| `observations` | [met.Obs](#t-met-Obs)[] |  |

<a id="t-server-metWindsReply"></a>

#### server.metWindsReply

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `from` | integer |  |
| `windowMin` | number |  |
| `nWind` | integer |  |
| `nTemp` | integer |  |
| `windsAloft` | [met.WindAloft](#t-met-WindAloft)[] | standard flight levels with data |
| `maxWind` | [met.MaxWind](#t-met-MaxWind) \| null |  |
| `maxShear` | [met.Shear](#t-met-Shear) \| null |  |
| `freezingLevelFt` | number \| null |  |
| `tropopause` | [met.Tropopause](#t-met-Tropopause) \| null |  |

<a id="t-server-navAirportReply"></a>

#### server.navAirportReply

navAirportReply is GET /api/navdata/airport/{code}.

| Field | Type | Notes |
|---|---|---|
| `airport` | [navdata.Airport](#t-navdata-Airport) | with runways |
| `ends` | [navdata.RunwayEndInfo](#t-navdata-RunwayEndInfo)[] | usable landing directions |

<a id="t-server-navStatus"></a>

#### server.navStatus

navStatus is GET /api/navdata/status.

| Field | Type | Notes |
|---|---|---|
| `datasets` | [navdata.DatasetInfo](#t-navdata-DatasetInfo)[] | imported datasets (rows, time, source with licence) |
| `sources` | [navdata.Source](#t-navdata-Source)[] | downloadable datasets |
| `import` | [navdata.ImportStatus](#t-navdata-ImportStatus) | current / last import |
| `regionAirports` | integer | airports within 800 km of the receiver loaded for the live picture |
| `routesOnline` | boolean | config.navData.routes.online (and routes enabled) |
| `autoRefresh` | boolean | config.navData.autoRefresh |
| `photos` | boolean | config.navData.photos (and a contact is set) |
| `airportsOverlay` | boolean |  |
| `attribution` | string[] | lines to show where the data is used |

<a id="t-server-ntpHistoryReply"></a>

#### server.ntpHistoryReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `source` | string | "memory" (full samples) \| "database" (offset, delay, stratum only) |
| `total` | integer | samples in the range before thinning |
| `samples` | [ntp.Sample](#t-ntp-Sample)[] |  |

<a id="t-server-ntpReply"></a>

#### server.ntpReply

| Field | Type | Notes |
|---|---|---|
| `config` | [model.NTPConfig](#t-model-NTPConfig) |  |
| `status` | [model.NTPStatus](#t-model-NTPStatus) |  |
| `latest` | [ntp.Sample](#t-ntp-Sample) \| null | newest measurement, null = none yet |
| `hour` | [ntp.Stats](#t-ntp-Stats) | statistics of the last hour |
| `day` | [ntp.Stats](#t-ntp-Stats) | last 24 hours |
| `all` | [ntp.Stats](#t-ntp-Stats) | everything in memory (about 6 days) |

<a id="t-server-outlookReception"></a>

#### server.outlookReception

| Field | Type | Notes |
|---|---|---|
| `level` | string |  |
| `confidence` | string |  |
| `typicalP95Km` | number \| null |  |
| `farKm` | number \| null |  |
| `best` | [predict.ReceptionHour](#t-predict-ReceptionHour) \| null |  |
| `bands` | [predict.BandOutlook](#t-predict-BandOutlook)[] |  |
| `next` | [predict.ReceptionHour](#t-predict-ReceptionHour)[] | the next 12 hours |

<a id="t-server-outlookReply"></a>

#### server.outlookReply

| Field | Type | Notes |
|---|---|---|
| `generated` | integer |  |
| `status` | [predict.Status](#t-predict-Status) |  |
| `nowcast` | [predict.Nowcast](#t-predict-Nowcast) \| null |  |
| `traffic` | [server.outlookTraffic](#t-server-outlookTraffic) |  |
| `reception` | [server.outlookReception](#t-server-outlookReception) |  |
| `weather` | [server.outlookWeather](#t-server-outlookWeather) |  |

<a id="t-server-outlookTraffic"></a>

#### server.outlookTraffic

| Field | Type | Notes |
|---|---|---|
| `level` | string |  |
| `confidence` | string |  |
| `busiest` | [predict.BusyHour](#t-predict-BusyHour)[] |  |
| `quietest` | [predict.BusyHour](#t-predict-BusyHour) \| null |  |
| `next` | [predict.TrafficHour](#t-predict-TrafficHour)[] | the next 12 hours |

<a id="t-server-outlookWeather"></a>

#### server.outlookWeather

| Field | Type | Notes |
|---|---|---|
| `level` | string |  |
| `confidence` | string |  |
| `current` | [predict.WeatherNow](#t-predict-WeatherNow) \| null |  |
| `zambretti` | [predict.ZambrettiOutlook](#t-predict-ZambrettiOutlook) \| null |  |
| `patterns` | [predict.Pattern](#t-predict-Pattern)[] |  |
| `next` | [predict.WeatherHour](#t-predict-WeatherHour)[] | the next 12 hours |

<a id="t-server-patternReply"></a>

#### server.patternReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean |  |
| `antennas` | [analytics.AntennaModel](#t-analytics-AntennaModel)[] | modelled (and, with one feed per antenna, measured) patterns |
| `azimuth` | [analytics.AzimuthPattern](#t-analytics-AzimuthPattern) \| null | measured pattern of the selected antenna / combined |

<a id="t-server-predictedPath"></a>

#### server.predictedPath

predictedPath is GET /api/prediction/path/{icao}.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `t0` | integer | Unix ms of the first point |
| `altOk` | boolean |  |
| `plan` | string | "decay" \| "capture" \| "hold" \| "ground" |
| `points` | [trajectory.PathPoint](#t-trajectory-PathPoint)[] |  |
| `terrain` | [terrain.VisibilityForecast](#t-terrain-VisibilityForecast) \| null | Terrain tells when the aircraft is expected to drop below (or rise above) the terrain radio horizon along the path; null until the terrain analysis exists. |
| `learned` | [flowlearn.Path](#t-flowlearn-Path) | *optional* - Learned is the path predicted from how the surrounding traffic flew through the same airspace recently (internal/flowlearn); omitted when the aircraft has no current position, track or speed. |

<a id="t-server-predictionLive"></a>

#### server.predictionLive

predictionLive is GET /api/prediction/live.

| Field | Type | Notes |
|---|---|---|
| `now` | integer | Unix ms |
| `coastSec` | integer | config.prediction.coastSec |
| `aircraft` | [trajectory.Predicted](#t-trajectory-Predicted)[] | every aircraft with a usable estimate; coast=true when its last position is older than 4 s |

<a id="t-server-proximityReply"></a>

#### server.proximityReply

proximityReply is GET /api/prediction/proximity.

| Field | Type | Notes |
|---|---|---|
| `now` | integer |  |
| `note` | string |  |
| `pairs` | [trajectory.ProximityPair](#t-trajectory-ProximityPair)[] | sorted by severity (<= 1: both thresholds violated) |

<a id="t-server-publicAircraftDetail"></a>

#### server.publicAircraftDetail

publicAircraftDetail is GET /api/aircraft/{icao} (and /api/public/aircraft/{icao}) below admin scope in aggregator mode: the live state, the live trail without signal levels, and reference data. Stored totals and flights (with distances) are not included.

| Field | Type | Notes |
|---|---|---|
| `aircraft` | [publicdto.Aircraft](#t-publicdto-Aircraft) \| null |  |
| `trail` | [publicdto.TrailPoint](#t-publicdto-TrailPoint)[] |  |
| `info` | [server.aircraftInfo](#t-server-aircraftInfo) |  |
| `acdb` | [store.ACDBEntry](#store-rows) \| null |  |

<a id="t-server-radarReply"></a>

#### server.radarReply

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | weather.radarOverlay is on in the settings |
| `available` | boolean | a frame index has been downloaded |
| `fetched` | integer | Unix ms of the download |
| `generated` | integer | Unix ms the index was generated upstream |
| `host` | string | tile host, e.g. "https://tilecache.rainviewer.com" |
| `past` | [weather.RadarFrame](#t-weather-RadarFrame)[] | observed frames, oldest first (about 2 h in 10-minute steps) |
| `nowcast` | [weather.RadarFrame](#t-weather-RadarFrame)[] | forecast frames (currently empty on the free API) |
| `tileSize` | integer |  |
| `maxZoom` | integer |  |
| `attribution` | string |  |
| `attributionUrl` | string |  |

<a id="t-server-radioPattern"></a>

#### server.radioPattern

| Field | Type | Notes |
|---|---|---|
| `id` | string |  |
| `name` | string |  |
| `type` | string |  |
| `color` | string | *optional* |
| `info` | [radio.PatternInfo](#t-radio-PatternInfo) | resolved model parameters, "estimated" lists what was derived |
| `amslM` | number | feed point height above mean sea level |
| `elevDeg` | number | elevation of the azimuth cut |
| `azDeg` | number | azimuth of the elevation cut |
| `azimuth` | [server.cutJSON](#t-server-cutJSON) |  |
| `elevation` | [server.cutJSON](#t-server-cutJSON) |  |
| `rangeAltFt` | number | altitude of the range prediction |
| `rangeKm` | (number \| null)[] | predicted maximum range per azimuth step |
| `rangeAzDeg` | (number \| null)[] |  |
| `horizonKm` | number \| null | line of sight at rangeAltFt |
| `rangeTable` | [radio.BandPrediction](#t-radio-BandPrediction)[] | per altitude band, on boresight |
| `systemNfDb` | number \| null |  |
| `sensitivityDbm` | number \| null |  |
| `frontEndLossDb` | number \| null |  |

<a id="t-server-radioPatternReply"></a>

#### server.radioPatternReply

| Field | Type | Notes |
|---|---|---|
| `stepDeg` | number |  |
| `antennas` | [server.radioPattern](#t-server-radioPattern)[] |  |

<a id="t-server-rangeReply"></a>

#### server.rangeReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `farKm` | number | threshold behind farCount |
| `hours` | [store.RangeHour](#store-rows)[] |  |

<a id="t-server-receiverFeed"></a>

#### server.receiverFeed

| Field | Type | Notes |
|---|---|---|
| `id` | string |  |
| `name` | string |  |
| `type` | string |  |
| `connected` | boolean |  |
| `gpsTime` | boolean | the feed delivers GPS timestamps |
| `info` | string | last receiver status text |
| `lastFrame` | integer |  |

<a id="t-server-receiverReply"></a>

#### server.receiverReply

| Field | Type | Notes |
|---|---|---|
| `now` | integer |  |
| `model` | string | configured receiver model |
| `feeds` | [server.receiverFeed](#t-server-receiverFeed)[] |  |
| `receiverGps` | [model.ReceiverGPS](#t-model-ReceiverGPS) \| null | position reported by the receiver, null = none |
| `statusFrames` | integer | receiver status frames seen since start |
| `history` | [server.receiverStatusRow](#t-server-receiverStatusRow)[] | stored status frames, newest first |

<a id="t-server-receiverStatusRow"></a>

#### server.receiverStatusRow

| Field | Type | Notes |
|---|---|---|
| `t` | integer |  |
| `feed` | string |  |
| `status` | integer[] | raw receiver status payload bytes (Radarcape type '4'); sampled once per feed and minute |

<a id="t-server-refractLayer"></a>

#### server.refractLayer

| Field | Type | Notes |
|---|---|---|
| `baseM` | number \| null |  |
| `topM` | number \| null |  |
| `dNdh` | number \| null | N-units per km |
| `dMdh` | number \| null | M-units per km, negative = trapping layer |
| `class` | string |  |

<a id="t-server-refractPoint"></a>

#### server.refractPoint

| Field | Type | Notes |
|---|---|---|
| `heightM` | number \| null | m AMSL |
| `n` | number \| null | refractivity, N-units |
| `m` | number \| null | modified refractivity, M-units |

<a id="t-server-replayAircraft"></a>

#### server.replayAircraft

replayAircraft identifies one flight of a replay (index = position in replayReply.Aircraft).

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `flightId` | integer |  |
| `callsign` | string | *optional* |
| `reg` | string | *optional* |
| `type` | string | *optional* |
| `operator` | string | *optional* |
| `category` | string | *optional* - ADS-B emitter category ("A3", "B1", ...) |
| `military` | boolean | *optional* |

<a id="t-server-replayFrame"></a>

#### server.replayFrame

replayFrame is the picture at one instant: rows [aircraftIndex, lat, lon, altFt|null, gsKt|null, trackDeg|null, src, ageS].

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms |
| `p` | array of `[aircraftIndex, lat, lon, altFt\|null, gsKt\|null, trackDeg\|null, src, ageS]` |  |

<a id="t-server-replayReply"></a>

#### server.replayReply

replayReply is GET /api/replay.

| Field | Type | Notes |
|---|---|---|
| `from` | integer | Unix ms of the first frame |
| `to` | integer | Unix ms (end of the window) |
| `step` | integer | seconds between frames |
| `staleSec` | integer | a stored point is shown for at most this long after its time |
| `aircraft` | [server.replayAircraft](#t-server-replayAircraft)[] | indexed by the first element of every frame row |
| `frames` | [server.replayFrame](#t-server-replayFrame)[] | oldest first, one every step seconds |
| `points` | integer | stored positions read |
| `truncated` | boolean | the point budget (points the frames use) was exhausted: later frames are incomplete |

<a id="t-server-reportReply"></a>

#### server.reportReply

reportReply is GET /api/reports.

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `total` | integer | matching flights |
| `limit` | integer |  |
| `offset` | integer |  |
| `sort` | string |  |
| `desc` | boolean |  |
| `rows` | [server.reportRow](#t-server-reportRow)[] |  |

<a id="t-server-reportRow"></a>

#### server.reportRow

reportRow is one flight of a report with the airframe's reference data.

| Field | Type | Notes |
|---|---|---|
| `flightId` | integer |  |
| `icao` | string |  |
| `callsign` | string | *optional* |
| `squawk` | string | *optional* |
| `reg` | string | *optional* |
| `type` | string | *optional* |
| `typeDesc` | string | *optional* |
| `operator` | string | *optional* |
| `country` | string | *optional* |
| `category` | string | *optional* |
| `military` | boolean | *optional* |
| `interesting` | boolean | *optional* |
| `start` | integer | Unix ms |
| `end` | integer |  |
| `durationS` | integer |  |
| `msgs` | integer |  |
| `positions` | integer |  |
| `srcMask` | integer |  |
| `minDistKm` | number \| null |  |
| `maxDistKm` | number \| null |  |
| `maxDistBrg` | number \| null |  |
| `minAlt` | number \| null | ft |
| `maxAlt` | number \| null |  |
| `maxGs` | number \| null | kt |
| `rssiMax` | number \| null |  |
| `rssiAvg` | number \| null |  |
| `firstLat` | number \| null |  |
| `firstLon` | number \| null |  |
| `lastLat` | number \| null |  |
| `lastLon` | number \| null |  |
| `emergency` | string | *optional* |
| `dep` | string | departure airport code (stored once the flight is closed; "" = unknown / not yet resolved) |
| `arr` | string | arrival airport code ("" = unknown) |

<a id="t-server-rfGNSS"></a>

#### server.rfGNSS

rfGNSS is GET /api/rf/gnss: the GNSS interference part of the census.

| Field | Type | Notes |
|---|---|---|
| `indexNow` | number \| null |  |
| `series` | [rfstat.GNSSPoint](#t-rfstat-GNSSPoint)[] | per minute, 24 h |
| `cells` | [rfstat.GNSSCell](#t-rfstat-GNSSCell)[] | polar map cells around the station |
| `degradedNow` | integer |  |
| `degradedList` | string[] |  |
| `jumpAircraft` | integer |  |
| `gnssDelta` | [rfstat.Hist](#t-rfstat-Hist) |  |
| `gnssDeltaMeanFt` | number \| null |  |
| `gnssDeltaByAlt` | [rfstat.AltBandDelta](#t-rfstat-AltBandDelta)[] |  |

<a id="t-server-rfInterrogators"></a>

#### server.rfInterrogators

rfInterrogators is GET /api/rf/interrogators: the interrogation part of the RF report with the report time, so the UI can place the per-minute timelines on the server's clock.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | RF report time (Unix ms) |
| `interrogators` | [rfstat.InterrogationStats](#t-rfstat-InterrogationStats) \| null |  |

<a id="t-server-rfIntervals"></a>

#### server.rfIntervals

rfIntervals is GET /api/rf/intervals: position update intervals.

| Field | Type | Notes |
|---|---|---|
| `gaps` | [rfstat.GapBand](#t-rfstat-GapBand)[] | per range band |
| `types` | [rfstat.PdType](#t-rfstat-PdType)[] |  |
| `range50Km` | number \| null |  |
| `range90Km` | number \| null |  |

<a id="t-server-rfSummary"></a>

#### server.rfSummary

rfSummary is GET /api/rf/summary: the headline numbers of every section.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms |
| `windowSec` | number \| null | sliding window of the statistics (about 1 h) |
| `health` | integer | 0..100, -1 = no data |
| `healthStatus` | string |  |
| `advice` | string[] |  |
| `adviceItems` | [rfstat.AdviceItem](#t-rfstat-AdviceItem)[] | AdviceItems are the Advice sentences with their code and numbers (rfstat.AdviceItem), so a UI can re-format them in its own units. |
| `fps` | number \| null | frames per second |
| `occupancyPct` | number \| null | 1090 MHz channel occupancy by received replies |
| `peakOccupancyPct` | number \| null | 1 s peak, last 10 min |
| `crcBadFrac` | number \| null |  |
| `failFrac` | number \| null |  |
| `decodeFloorDbfs` | number \| null |  |
| `noiseFloorEstDbfs` | number \| null |  |
| `p50Dbfs` | number \| null |  |
| `strongFrac` | number \| null |  |
| `snrMarginDb` | number \| null |  |
| `pdPos` | number \| null | position squitter reception probability |
| `range50Km` | number \| null |  |
| `range90Km` | number \| null |  |
| `activeRadars` | integer |  |
| `adsbAircraft` | integer |  |
| `gnssIndexNow` | number \| null | % of ADS-B aircraft with degraded GNSS integrity |
| `timingAvailable` | boolean |  |

<a id="t-server-routeProviderDTO"></a>

#### server.routeProviderDTO

| Field | Type | Notes |
|---|---|---|
| `name` | string |  |
| `enabled` | boolean |  |
| `lic` | string |  |
| `used` | integer |  |
| `daily` | integer |  |
| `backoffUntil` | integer | *optional* |
| `sends` | string |  |
| `homepage` | string |  |

<a id="t-server-routeStatusDTO"></a>

#### server.routeStatusDTO

| Field | Type | Notes |
|---|---|---|
| `stats` | [routes.Stats](#t-routes-Stats) |  |
| `online` | boolean |  |
| `onlineUsed` | integer | online look-ups in the current 24-h window |
| `onlineDaily` | integer | daily cap |
| `backoffUntil` | integer | *optional* |
| `fileProblems` | string[] | *optional* |
| `attribution` | string[] |  |
| `config` | [model.RoutesConfig](#t-model-RoutesConfig) |  |

<a id="t-server-sessionInfo"></a>

#### server.sessionInfo

sessionInfo is the reply of the login, logout and session endpoints. The fields after LoggedIn are additions for aggregator mode and for the CSRF token; they are omitted when empty so standalone clients see the same document as before.

| Field | Type | Notes |
|---|---|---|
| `ok` | boolean |  |
| `admin` | boolean | this client may change settings now |
| `loopback` | boolean | the request came from the machine itself (never in aggregator mode) |
| `passwordSet` | boolean | an admin password is configured (aggregator: always true) |
| `loggedIn` | boolean | a valid session cookie was presented |
| `csrfToken` | string | *optional* - CSRFToken must be sent as the X-CSRF-Token header on every non-GET request made with this session (required in aggregator mode, checked when present in standalone mode). Only present while logged in. |
| `aggregator` | boolean | *optional* - Aggregator is true in aggregator mode. |
| `scope` | string | *optional* - Scope is "public", "owner" or "admin" (aggregator mode). |
| `username` | string | *optional* - Username of the logged-in account (aggregator mode). |
| `totp` | boolean | *optional* - TOTP is true when the account has an authenticator (aggregator mode). |
| `codeRequired` | boolean | *optional* - CodeRequired: the password was right, now send it again with `code` (TOTP or recovery code). Only in a 401 login reply. |
| `setupRequired` | boolean | *optional* - SetupRequired: aggregator mode has no admin with an authenticator yet; finish setup with `airview admin init` or the console token. |
| `recoveryLeft` | integer | *optional* - RecoveryLeft is the number of unused recovery codes, sent after a login that used one. |

<a id="t-server-signalReply"></a>

#### server.signalReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean |  |
| `signal` | [analytics.SignalDistance](#t-analytics-SignalDistance) \| null | null until there is data |

<a id="t-server-skylineReply"></a>

#### server.skylineReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `ant` | string |  |
| `located` | boolean |  |
| `binDeg` | number |  |
| `bearingDeg` | (number \| null)[] |  |
| `skyline` | [analytics.Skyline](#t-analytics-Skyline) |  |
| `obstructions` | [analytics.Obstruction](#t-analytics-Obstruction)[] |  |

<a id="t-server-squawkReply"></a>

#### server.squawkReply

| Field | Type | Notes |
|---|---|---|
| `found` | boolean |  |
| `valid` | boolean | four octal digits |
| `code` | string | four octal digits |
| `meaning` | string | short label |
| `region` | string | Region says where the meaning applies: "Worldwide" (ICAO) or a list of states / regions. The same code can mean something else elsewhere. |
| `emergency` | string | *optional* - Emergency is the model.Aircraft.Emergency state the code implies ("general", "nordo", "unlawful"), "" for non-emergency codes. |
| `kind` | string | Kind is "emergency", "conspicuity" (general VFR / IFR codes that many aircraft set at the same time) or "special" (a particular activity). |

<a id="t-server-systemReply"></a>

#### server.systemReply

| Field | Type | Notes |
|---|---|---|
| `app` | string |  |
| `version` | string |  |
| `platform` | string | GOOS/GOARCH |
| `goVersion` | string |  |
| `hostname` | string |  |
| `pid` | integer |  |
| `cpus` | integer |  |
| `goroutines` | integer |  |
| `memory` | [server.memoryInfo](#t-server-memoryInfo) |  |
| `now` | integer |  |
| `started` | integer |  |
| `uptimeS` | integer |  |
| `timeZone` | string | of the host |
| `utcOffsetSec` | integer | of the host, now |
| `dataDir` | string |  |
| `dbBytes` | integer |  |
| `gui` | boolean |  |
| `mode` | string |  |
| `demo` | boolean |  |
| `located` | boolean |  |
| `httpAddr` | string |  |
| `httpPort` | integer |  |
| `restartRequired` | boolean | a saved setting (HTTP port / bind address) needs a restart |
| `liveClients` | integer | open /api/events streams |
| `sessions` | integer | admin logins |
| `cacheEntries` | integer | cached API responses |
| `forecaster` | [predict.Status](#t-predict-Status) |  |
| `tracker` | [server.trackerBrief](#t-server-trackerBrief) |  |

<a id="t-server-terrainStatus"></a>

#### server.terrainStatus

terrainStatus is GET /api/terrain/status.

| Field | Type | Notes |
|---|---|---|
| `job` | [terrain.JobStatus](#t-terrain-JobStatus) |  |
| `available` | boolean | an analysis result exists |
| `key` | string | its key |
| `computedMs` | integer |  |
| `dataComplete` | boolean |  |
| `provider` | [terrain.ProviderStats](#t-terrain-ProviderStats) \| null |  |
| `attribution` | string |  |
| `attributionUrl` | string |  |
| `enabled` | boolean | config.terrain.enabled |
| `autoPrefetch` | boolean | config.terrain.autoPrefetch |
| `located` | boolean | the (effective) receiver position is known |

<a id="t-server-tileStats"></a>

#### server.tileStats

tileStats is GET /api/tilecache.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean |  |
| `hosts` | string[] |  |
| `dir` | string |  |
| `bytes` | integer | size on disk |
| `files` | integer |  |
| `maxBytes` | integer | eviction starts above this |
| `scanned` | boolean | false until the start-up scan of the directory finished |
| `hits` | integer | served from disk since start |
| `misses` | integer | downloaded |
| `revalidated` | integer |  |
| `stale` | integer | served although expired because the upstream failed |
| `errors` | integer | upstream failures |
| `evictions` | integer |  |

<a id="t-server-traceReply"></a>

#### server.traceReply

traceReply is GET /api/history/flights/{id}/trace.

| Field | Type | Notes |
|---|---|---|
| `flight` | [store.Flight](#store-rows) |  |
| `points` | array of `[tMs, lat, lon, altFt\|null, gsKt\|null, trackDeg\|null, rssi\|null, src]` | every stored position: [tMs, lat, lon, altFt\|null, gsKt\|null, trackDeg\|null, rssi\|null, src] |
| `airports` | [server.flightAirports](#t-server-flightAirports) |  |

<a id="t-server-trackerBrief"></a>

#### server.trackerBrief

| Field | Type | Notes |
|---|---|---|
| `frames` | integer |  |
| `msgRate` | number |  |
| `aircraft` | integer |  |
| `tracked` | integer |  |

<a id="t-server-trailsReply"></a>

#### server.trailsReply

| Field | Type | Notes |
|---|---|---|
| `now` | integer |  |
| `minutes` | integer |  |
| `trails` | { [key: string]: TrailPoint[] (compact arrays `[tMs, lat, lon, altFt\|null, gsKt\|null, rssi\|null, src]`) } |  |

<a id="t-server-typeReply"></a>

#### server.typeReply

| Field | Type | Notes |
|---|---|---|
| `found` | boolean |  |
| `designator` | string | e.g. "A20N" |
| `manufacturer` | string | e.g. "Airbus"; "" for generic designators |
| `model` | string | e.g. "A320neo" |
| `name` | string | manufacturer + model, e.g. "Airbus A320neo" |
| `description` | string | *optional* - Description is the ICAO description code: airframe class letter, engine count, engine type (e.g. "L2J"); "" when the designator has none. |
| `wtc` | string | *optional* - WTC is the ICAO wake turbulence category: "L", "M", "H", "J" (super) or "L/M" (depends on the variant); "" when not applicable. |
| `role` | string |  |
| `classText` | string | *optional* - "Landplane, 2 jet engines" |
| `wtcText` | string | *optional* |
| `engines` | integer | *optional* |

<a id="t-server-watchInfo"></a>

#### server.watchInfo

watchInfo tells on which alert watch-list an address is.

| Field | Type | Notes |
|---|---|---|
| `list` | string |  |
| `note` | string | *optional* |

<a id="t-server-weatherCurrentReply"></a>

#### server.weatherCurrentReply

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | weather is switched on in the settings |
| `located` | boolean | needs the receiver position |
| `status` | [weather.PollerStatus](#t-weather-PollerStatus) |  |
| `attribution` | string |  |
| `attributionUrl` | string |  |
| `fetched` | integer | Unix ms of the newest download, 0 = none |
| `model` | string | weather model |
| `elevationM` | number \| null | terrain height used by the weather model |
| `current` | [weather.Current](#t-weather-Current) \| null | 15-minute current conditions, null until fetched |
| `text` | string | current weather in words |
| `hour` | [store.Weather](#store-rows) \| null | stored row of the current hour (with refractivity columns), null = none |
| `hourSource` | string | "observation" \| "forecast" \| "" |
| `atmosphere` | [predict.Atmos](#t-predict-Atmos) \| null | radio propagation state derived from hour |

<a id="t-server-weatherHourlyReply"></a>

#### server.weatherHourlyReply

| Field | Type | Notes |
|---|---|---|
| `from` | integer |  |
| `to` | integer |  |
| `now` | integer |  |
| `attribution` | string |  |
| `attributionUrl` | string |  |
| `obs` | [store.Weather](#store-rows)[] | observed / analysed hours up to now |
| `forecast` | [store.Weather](#store-rows)[] | forecast hours from the current hour on |

<a id="t-server-weatherProfileReply"></a>

#### server.weatherProfileReply

| Field | Type | Notes |
|---|---|---|
| `t` | integer | hour of the profile, Unix ms (0 = none found) |
| `source` | string | "observation" \| "forecast" \| "" |
| `levels` | [weather.Level](#t-weather-Level)[] | pressure levels, ground up |
| `surface` | [store.Weather](#store-rows) \| null |  |
| `atmosphere` | [predict.Atmos](#t-predict-Atmos) \| null |  |
| `points` | [server.refractPoint](#t-server-refractPoint)[] | Refractivity analysis of the levels (null without a profile). |
| `layers` | [server.refractLayer](#t-server-refractLayer)[] |  |
| `ducts` | [radio.Duct](#t-radio-Duct)[] |  |
| `summary` | string |  |

<a id="t-stat-ResponseBin"></a>

#### stat.ResponseBin

ResponseBin describes y for the observations whose x fell into one bin.

| Field | Type | Notes |
|---|---|---|
| `lo` | number | lower x edge (equal-count bins: smallest x) |
| `hi` | number | upper x edge (equal-count bins: largest x) |
| `n` | integer | observations in the bin |
| `xMean` | number | mean x of the bin |
| `yMean` | number | mean y |
| `yStd` | number | sample std of y (0 when N < 2) |
| `yq` | number[] | y quantiles, parallel to BinOptions.Quantiles |

<a id="t-store-MigrationInfo"></a>

#### store.MigrationInfo

MigrationInfo is the data-migration block of /api/db/stats.

| Field | Type | Notes |
|---|---|---|
| `state` | string | State: "none" (nothing registered), "pending", "running", "waiting_for_disk", "waiting_for_backup", "failed" or "done". |
| `migrations` | [store.MigrationState](#t-store-MigrationState)[] |  |
| `backupMode` | string |  |
| `backup` | string | *optional* - pre-migration backup file (path) |
| `vacuumTail` | boolean | free pages still being returned |
| `hint` | string | *optional* |

<a id="t-store-MigrationState"></a>

#### store.MigrationState

MigrationState is the progress of one data migration.

| Field | Type | Notes |
|---|---|---|
| `name` | string |  |
| `version` | integer |  |
| `state` | string |  |
| `rowsLeft` | integer | estimate |
| `rowsDone` | integer | approximate (SQLite change counter / 2) |
| `steps` | integer | steps run by this process |
| `started` | integer | Unix ms, 0 = not started |
| `updated` | integer | Unix ms of the last step |
| `eta` | integer | Unix ms, 0 = unknown |
| `lastError` | string | *optional* |

<a id="t-store-ProfileView"></a>

#### store.ProfileView

ProfileView is Profile with durations in milliseconds, for JSON replies.

| Field | Type | Notes |
|---|---|---|
| `name` | string |  |
| `requested` | string |  |
| `reason` | string | *optional* |
| `flushIntervalMs` | integer |  |
| `trailFlushMs` | integer |  |
| `incFlushMs` | integer |  |
| `heatFlushMs` | integer |  |
| `rangePartialMs` | integer |  |
| `aircraftRefreshMs` | integer |  |
| `sealIntervalMs` | integer |  |
| `checkpointSoftBytes` | integer |  |
| `checkpointIntervalMs` | integer |  |
| `walHardCapPages` | integer |  |
| `journalSizeLimit` | integer |  |
| `writerCacheKiB` | integer |  |
| `readerCacheKiB` | integer |  |
| `readConns` | integer |  |
| `readerIdleCloseMs` | integer |  |
| `mmapBytes` | integer |  |
| `migrationStepMs` | integer |  |
| `backupBeforeMigrate` | string |  |
| `tileCacheMB` | integer |  |

<a id="t-store-Stats"></a>

#### store.Stats

Stats is a snapshot of the store's size and writer counters.

| Field | Type | Notes |
|---|---|---|
| `path` | string |  |
| `dbBytes` | integer | size of airview.db |
| `walBytes` | integer | size of airview.db-wal |
| `pageSize` | integer |  |
| `pageCount` | integer |  |
| `freelistPages` | integer | unused pages inside the file |
| `queuedRows` | integer | rows waiting for the next flush |
| `queuedOps` | integer |  |
| `flushes` | integer | successful batch transactions |
| `flushedRows` | integer | rows written by them |
| `flushErrors` | integer | failed flushes (rows were kept and retried) |
| `failedOps` | integer | operations dropped because their data could not be written |
| `combinedCommits` | integer | CombinedCommits: synchronous writes (Write, Apply) that shared the commit of the queued rows instead of adding one of their own. |
| `droppedLow` | integer | Rows dropped because the queue overflowed (disk too slow or full), by importance: Low = histogram / heat-map increments and raw met observations, Mid = trail positions, NTP samples and receiver status, High = everything else (only when nothing less important was left). |
| `droppedMid` | integer |  |
| `droppedHigh` | integer |  |
| `lastFlush` | integer | Unix ms of the last successful flush, 0 = never |
| `lastFlushMs` | number | its duration |
| `lastError` | string | *optional* |
| `checkpoints` | integer | Checkpointer (§3.1): PASSIVE checkpoints run outside the commits, escalations to RESTART, TRUNCATE checkpoints (hourly / Checkpoint), PASSIVE runs a reader kept from finishing, and the WAL content not yet copied into the database file. |
| `checkpointRestarts` | integer |  |
| `checkpointTruncates` | integer |  |
| `checkpointsIncomplete` | integer |  |
| `lastCheckpoint` | integer | Unix ms, 0 = none since start |
| `lastCheckpointMs` | number | duration of the last PASSIVE / RESTART |
| `walPendingBytes` | integer |  |
| `vacuumedPages` | integer | released by the post-migration vacuum tail |

<a id="t-terrain-JobStatus"></a>

#### terrain.JobStatus

JobStatus is the background job state (JSON-ready).

| Field | Type | Notes |
|---|---|---|
| `state` | string |  |
| `phase` | string |  |
| `progress` | number | 0..1 overall |
| `phaseProgress` | number | 0..1 within the phase |
| `tiles` | [terrain.PrefetchProgress](#t-terrain-PrefetchProgress) |  |
| `message` | string |  |
| `error` | string |  |
| `key` | string |  |
| `startedMs` | integer |  |
| `finishedMs` | integer |  |
| `fromCache` | boolean | polar terrain loaded from the disk cache |

<a id="t-terrain-PrefetchProgress"></a>

#### terrain.PrefetchProgress

PrefetchProgress reports the progress of a Prefetch.

| Field | Type | Notes |
|---|---|---|
| `zoom` | integer |  |
| `total` | integer |  |
| `done` | integer |  |
| `cached` | integer |  |
| `downloaded` | integer |  |
| `noData` | integer |  |
| `failed` | integer |  |
| `bytes` | integer |  |
| `offline` | boolean |  |

<a id="t-terrain-ProviderStats"></a>

#### terrain.ProviderStats

ProviderStats is a snapshot of the provider's caches and counters.

| Field | Type | Notes |
|---|---|---|
| `url` | string |  |
| `dir` | string |  |
| `zoom` | integer |  |
| `memTiles` | integer |  |
| `memCapacity` | integer |  |
| `memBytes` | integer |  |
| `diskTiles` | integer |  |
| `diskBytes` | integer |  |
| `diskCapBytes` | integer |  |
| `downloads` | integer |  |
| `downloadBytes` | integer |  |
| `failures` | integer |  |
| `noData` | integer |  |
| `diskLoads` | integer |  |
| `missingReads` | integer |  |
| `offline` | boolean |  |
| `offlineUntilMs` | integer |  |

<a id="t-terrain-VisibilityForecast"></a>

#### terrain.VisibilityForecast

VisibilityForecast tells when an aircraft is expected to disappear behind or rise above the terrain horizon along a predicted path.

| Field | Type | Notes |
|---|---|---|
| `known` | boolean |  |
| `visibleNow` | boolean | at the first path point |
| `marginDeg` | number | at the first path point |
| `lostAtMs` | integer | LostAtMs is the first predicted time the aircraft drops below the terrain horizon (0 = not within the path). |
| `acquiredAtMs` | integer | AcquiredAtMs is the first predicted time it rises above it (0 = not within the path). |

<a id="t-tracker-AntennaCounters"></a>

#### tracker.AntennaCounters

AntennaCounters are the totals of one antenna ("" = all combined).

| Field | Type | Notes |
|---|---|---|
| `id` | string |  |
| `frames` | integer |  |
| `positions` | integer |  |
| `msgRate` | number |  |

<a id="t-tracker-Counters"></a>

#### tracker.Counters

Counters is the ingest summary for the statistics page. All counts are totals since the tracker was created or reset.

| Field | Type | Notes |
|---|---|---|
| `now` | integer | data time, Unix ms (0 before the first frame) |
| `started` | integer | wall clock, Unix ms |
| `uptimeS` | number |  |
| `frames` | integer | Mode S frames received (every copy) |
| `accepted` | integer | decoded and applied (duplicates excluded) |
| `duplicates` | integer | second copies from another antenna / feed |
| `crcBad` | integer | parity failures |
| `fixed` | integer | accepted after a 1-bit repair |
| `rejected` | integer | parity-address frames of unconfirmed addresses |
| `unsupported` | integer | downlink formats without a decoder |
| `badLength` | integer |  |
| `modeAC` | integer |  |
| `status` | integer | receiver status frames |
| `updates` | integer |  |
| `updateMsgs` | integer |  |
| `positions` | integer |  |
| `mlatPositions` | integer |  |
| `posRejected` | integer | failed the range / speed filters |
| `posPending` | integer | first fixes that waited for confirmation |
| `posJumps` | integer | tracks restarted by two consistent outliers |
| `df` | { [key: string]: integer } | accepted frames per downlink format ("df17", "df_other" ...) |
| `bds` | { [key: string]: integer } | identified Comm-B registers ("4,0", "5,0", "6,0", "4,4") |
| `flights` | integer |  |
| `emergencies` | integer |  |
| `msgRate` | number | frames/s, smoothed |
| `posRate` | number |  |
| `aircraft` | integer | shown in the live list |
| `tracked` | integer | in the table, including dormant flights |
| `antennas` | [tracker.AntennaCounters](#t-tracker-AntennaCounters)[] |  |
| `feeds` | [tracker.FeedCounters](#t-tracker-FeedCounters)[] |  |

<a id="t-tracker-FeedCounters"></a>

#### tracker.FeedCounters

FeedCounters are the totals of one feed.

| Field | Type | Notes |
|---|---|---|
| `id` | string |  |
| `antennaId` | string | *optional* |
| `frames` | integer | raw Mode S frames |
| `updates` | integer | pre-decoded updates (SBS / JSON) |
| `updateMsgs` | integer |  |
| `msgRate` | number |  |
| `lastFrame` | integer | Unix ms, 0 = never |

<a id="t-tracker-FeedLatency"></a>

#### tracker.FeedLatency

FeedLatency describes the delay between a feed's GPS frame timestamps and the (NTP-corrected) host clock at reception: latency = (Recv + clock offset) time of day - GPS time of day It contains the receiver's processing time, the network transit and any error of the host clock. Only feeds that deliver GPS timestamps (Radarcape in GPS mode, the simulator) have samples. The statistics cover the rolling sample window (about the last 80 s of traffic); History has one point per minute.

| Field | Type | Notes |
|---|---|---|
| `feedId` | string |  |
| `total` | integer | GPS-stamped frames seen |
| `samples` | integer | samples in the rolling window |
| `windowS` | number | time span of the window |
| `lastMs` | number |  |
| `meanMs` | number |  |
| `stdMs` | number |  |
| `minMs` | number |  |
| `maxMs` | number |  |
| `p01Ms` | number |  |
| `p05Ms` | number |  |
| `p25Ms` | number |  |
| `p50Ms` | number |  |
| `p75Ms` | number |  |
| `p95Ms` | number |  |
| `p99Ms` | number |  |
| `hist` | [tracker.LatencyBin](#t-tracker-LatencyBin)[] |  |
| `history` | [tracker.LatencyPoint](#t-tracker-LatencyPoint)[] |  |
| `clockOffsetMs` | number | NTP correction applied |
| `clockSynced` | boolean |  |
| `ref` | string | Ref is the instant the delays are measured from: always "startOfFrame" (latency.RefStartOfFrame) - the GPS stamp (end of the frame) minus the frame duration, the same reference as the receiver stage of the latency analysis. |

<a id="t-tracker-LatencyBin"></a>

#### tracker.LatencyBin

LatencyBin is one bar of the latency histogram.

| Field | Type | Notes |
|---|---|---|
| `loMs` | number |  |
| `hiMs` | number |  |
| `count` | integer |  |

<a id="t-tracker-LatencyPoint"></a>

#### tracker.LatencyPoint

LatencyPoint is the latency summary of one minute.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | start of the minute, Unix ms |
| `samples` | integer |  |
| `minMs` | number |  |
| `p05Ms` | number |  |
| `p50Ms` | number |  |
| `p95Ms` | number |  |

<a id="t-trajectory-ApproachInfo"></a>

#### trajectory.ApproachInfo

ApproachInfo describes an aircraft established on approach.

| Field | Type | Notes |
|---|---|---|
| `airport` | string | *optional* |
| `runway` | string | *optional* |
| `distKm` | number | along-track distance to the threshold |
| `etaSec` | number | time to the threshold at the current ground speed |
| `crossTrackM` | number | + = right of the extended centre line |
| `glideDevFt` | number | altitude minus the 3 degree glide path |
| `heightAglFt` | number | above threshold elevation |
| `decelerating` | boolean |  |
| `confidence` | number | 0..1 |

<a id="t-trajectory-Counters"></a>

#### trajectory.Counters

Counters are cumulative ingest counters.

| Field | Type | Notes |
|---|---|---|
| `observations` | integer |  |
| `positions` | integer |  |
| `rejected` | integer | positions failing the Mahalanobis gate |
| `velRejected` | integer | velocities failing the gate |
| `altRejected` | integer | altitudes failing the gate |
| `outOfOrder` | integer | late observations used (shifted) |
| `oooDropped` | integer | too late, dropped |
| `duplicates` | integer |  |
| `resets` | integer | tracks re-initialised after repeated rejects |
| `inits` | integer |  |
| `overflow` | integer | observations ignored because MaxTracks was reached |

<a id="t-trajectory-CPA"></a>

#### trajectory.CPA

CPA is the closest point of approach of an aircraft to the receiver.

| Field | Type | Notes |
|---|---|---|
| `inSec` | number | 0 = now (receding) |
| `t` | integer | Unix ms |
| `distKm` | number |  |
| `lat` | number |  |
| `lon` | number |  |
| `alt` | number |  |

<a id="t-trajectory-Ellipse"></a>

#### trajectory.Ellipse

Ellipse is a 1-sigma horizontal error ellipse.

| Field | Type | Notes |
|---|---|---|
| `a` | number | metres |
| `b` | number | metres |
| `dir` | number | direction of the major axis, deg true [0,180) |

<a id="t-trajectory-HoldInfo"></a>

#### trajectory.HoldInfo

HoldInfo describes a detected holding pattern or orbit.

| Field | Type | Notes |
|---|---|---|
| `kind` | string | "hold" (racetrack) \| "orbit" |
| `direction` | string | "right" \| "left" |
| `centerLat` | number |  |
| `centerLon` | number |  |
| `radiusKm` | number |  |
| `legCourse` | number | *optional* - course of the latest straight leg, deg true |
| `legSec` | number | *optional* |
| `turnDps` | number |  |
| `laps` | number |  |

<a id="t-trajectory-HorizonStats"></a>

#### trajectory.HorizonStats

HorizonStats is the rolling error summary for one predictor, horizon and flight phase.

| Field | Type | Notes |
|---|---|---|
| `horizon` | integer |  |
| `predictor` | string | "imm" \| "cv" \| "dr" |
| `phase` | string | "all" \| "straight" \| "turn" \| "vertical" |
| `n` | integer | samples in the rolling window |
| `total` | integer | samples since start |
| `medianKm` | number |  |
| `p90Km` | number |  |
| `p95Km` | number |  |
| `meanKm` | number |  |
| `rmsKm` | number | all-time |
| `alongBiasKm` | number | mean along-track error (+ = real aircraft ahead of prediction) |
| `alongMedAbsKm` | number |  |
| `crossBiasKm` | number | mean cross-track error (+ = right of predicted track) |
| `crossMedAbsKm` | number |  |
| `vertN` | integer |  |
| `vertMedAbsFt` | number |  |
| `vertP90Ft` | number |  |
| `vertBiasFt` | number | mean (real - predicted) |
| `calib95` | number | Calib95 (imm only): fraction of real positions inside the predicted 95 % ellipse (ideal 0.95; lower = over-confident). -1 when unavailable. |

<a id="t-trajectory-Intent"></a>

#### trajectory.Intent

Intent is the intent-aware analysis of one aircraft.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `t` | integer |  |
| `phase` | string | "ground","takeoff","climb","cruise","descent","approach","holding","turning","unknown" |
| `plan` | string | turn plan used by the predictor |
| `selTrack` | number | *optional* - SelTrackDeg is the selected heading converted to a true-track target (when known). |
| `holding` | [trajectory.HoldInfo](#t-trajectory-HoldInfo) | *optional* |
| `approach` | [trajectory.ApproachInfo](#t-trajectory-ApproachInfo) | *optional* |
| `cpa` | [trajectory.CPA](#t-trajectory-CPA) | *optional* |
| `exitSec` | number | *optional* - ExitSec is the predicted time until the aircraft leaves coverage (needs a RangeFunc). |
| `levelOff` | [trajectory.LevelOff](#t-trajectory-LevelOff) | *optional* |
| `descentPending` | boolean | *optional* - DescentPending: level, and the selected altitude is well below: top of descent is near. |
| `climbPending` | boolean | *optional* |

<a id="t-trajectory-LevelOff"></a>

#### trajectory.LevelOff

LevelOff is the predicted end of the current climb or descent.

| Field | Type | Notes |
|---|---|---|
| `inSec` | number |  |
| `alt` | number |  |
| `target` | string | "selected" \| "ceiling" \| "ground" |

<a id="t-trajectory-PathPoint"></a>

#### trajectory.PathPoint

PathPoint is one point of a predicted path.

| Field | Type | Notes |
|---|---|---|
| `dt` | number | seconds after Path.T0 |
| `lat` | number |  |
| `lon` | number |  |
| `alt` | number | only meaningful when Path.AltOK |
| `sh` | number | 1-sigma horizontal, m (includes the turn hypothesis: grows to km after a minute) |
| `shf` | number | 1-sigma horizontal of the track filter alone, m (no turn hypothesis; for path tick marks) |
| `sv` | number | 1-sigma vertical, ft |
| `ell` | [trajectory.Ellipse](#t-trajectory-Ellipse) |  |

<a id="t-trajectory-Predicted"></a>

#### trajectory.Predicted

Predicted is the compact per-aircraft entry of Manager.Predicted.

| Field | Type | Notes |
|---|---|---|
| `icao` | string |  |
| `lat` | number |  |
| `lon` | number |  |
| `alt` | number | *optional* - omitted when unknown |
| `track` | number |  |
| `gs` | number |  |
| `vr` | number |  |
| `sigH` | number |  |
| `age` | number |  |
| `coast` | boolean |  |
| `conf` | number |  |

<a id="t-trajectory-ProximityPair"></a>

#### trajectory.ProximityPair

ProximityPair is a predicted close approach between two aircraft.

| Field | Type | Notes |
|---|---|---|
| `a` | string |  |
| `b` | string |  |
| `tcpa` | number | seconds from now to the closest point of approach |
| `hDistKm` | number | horizontal separation at CPA |
| `vSepFt` | number | vertical separation at CPA |
| `nowHDistKm` | number |  |
| `nowVSepFt` | number |  |
| `lat` | number | midpoint at CPA |
| `lon` | number |  |
| `altA` | number | ft at CPA |
| `altB` | number |  |
| `severity` | number | Severity = max(hDist/hThresh, vSep/vThresh) at CPA; <= 1 means both thresholds are violated. |

<a id="t-trajectory-Skill"></a>

#### trajectory.Skill

Skill compares the full predictor against naive dead reckoning.

| Field | Type | Notes |
|---|---|---|
| `horizon` | integer |  |
| `phase` | string |  |
| `n` | integer |  |
| `immMedianKm` | number |  |
| `drMedianKm` | number |  |
| `skillPct` | number | 100*(1 - imm/dr); > 0 = better than dead reckoning |

<a id="t-trajectory-Stats"></a>

#### trajectory.Stats

Stats is the payload of the statistics endpoint.

| Field | Type | Notes |
|---|---|---|
| `now` | integer |  |
| `tracks` | integer |  |
| `ready` | integer | tracks with a usable estimate |
| `coasting` | integer | tracks currently coasting (age > CoastAfter, <= MaxCoast) |
| `counters` | [trajectory.Counters](#t-trajectory-Counters) |  |
| `horizons` | [trajectory.HorizonStats](#t-trajectory-HorizonStats)[] |  |
| `skill` | [trajectory.Skill](#t-trajectory-Skill)[] |  |

<a id="t-weather-Budget"></a>

#### weather.Budget

Budget is the client's self-imposed ceiling on weighted Open-Meteo calls. Zero fields take the defaults (half of the free tier).

| Field | Type | Notes |
|---|---|---|
| `PerMinute` | number |  |
| `PerHour` | number |  |
| `PerDay` | number |  |

<a id="t-weather-Current"></a>

#### weather.Current

Current is the latest analysed conditions (Open-Meteo "current", a 15-minute value interpolated from the best model for the location).

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms of the value |
| `intervalSec` | integer | aggregation interval of the value (900) |
| `tempC` | number \| null | air temperature at 2 m |
| `rh` | number \| null | relative humidity at 2 m, % |
| `dewPointC` | number \| null | dew point at 2 m |
| `apparentC` | number \| null | feels-like temperature |
| `precipMm` | number \| null | total precipitation in the preceding hour (rain + showers + snow water) |
| `rainMm` | number \| null | large-scale rain |
| `showersMm` | number \| null | convective rain |
| `snowCm` | number \| null | snowfall, cm |
| `weatherCode` | number \| null | WMO weather interpretation code, see WeatherCodeText |
| `cloud` | number \| null | total cloud cover, % |
| `cloudLow` | number \| null | cloud below 2 km, % |
| `cloudMid` | number \| null | cloud 2–6 km, % |
| `cloudHigh` | number \| null | cloud above 6 km, % |
| `pressureMsl` | number \| null | sea-level pressure, hPa |
| `surfacePressure` | number \| null | pressure at the surface, hPa |
| `windKmh` | number \| null | wind speed at 10 m |
| `windDir` | number \| null | wind direction at 10 m, degrees the wind blows FROM |
| `gustKmh` | number \| null | gusts at 10 m |
| `visibilityM` | number \| null | horizontal visibility, m |
| `isDay` | number \| null | 1 = daylight, 0 = night |
| `cape` | number \| null | convective available potential energy, J/kg |

<a id="t-weather-Level"></a>

#### weather.Level

Level is one isobaric level of the vertical profile.

| Field | Type | Notes |
|---|---|---|
| `hpa` | number | pressure level |
| `t` | number \| null | air temperature |
| `rh` | number \| null | relative humidity, % |
| `z` | number \| null | geopotential height, metres above mean sea level |
| `ws` | number \| null | wind speed |
| `wd` | number \| null | direction the wind blows FROM, degrees |

<a id="t-weather-PollerStatus"></a>

#### weather.PollerStatus

PollerStatus is a snapshot of the poller state for the UI.

| Field | Type | Notes |
|---|---|---|
| `enabled` | boolean | WeatherConfig.Enabled |
| `located` | boolean | station has valid coordinates |
| `intervalMin` | integer | effective refresh period |
| `lastAttempt` | integer | Unix ms, 0 = never |
| `lastOk` | integer | Unix ms, 0 = never |
| `lastError` | string | *optional* |
| `failures` | integer | consecutive failed polls |
| `nextPoll` | integer | Unix ms of the next scheduled poll, 0 = none |
| `radarEnabled` | boolean |  |
| `radarLastOk` | integer |  |
| `radarError` | string | *optional* |
| `usage` | [weather.Usage](#t-weather-Usage) | Open-Meteo call accounting |

<a id="t-weather-RadarFrame"></a>

#### weather.RadarFrame

RadarFrame is one radar mosaic in time.

| Field | Type | Notes |
|---|---|---|
| `t` | integer | Unix ms the frame was generated |
| `path` | string | frame path on the tile host, e.g. "/v2/radar/1a2b3c4d5e6f" |
| `url` | string | URL is a ready-to-use XYZ raster tile template with {z}/{x}/{y} placeholders: 256 px, Universal Blue, smoothed, snow shown separately. |

<a id="t-weather-Usage"></a>

#### weather.Usage

Usage reports the weighted Open-Meteo calls made by this client.

| Field | Type | Notes |
|---|---|---|
| `minute` | number | last 60 s |
| `hour` | number | last 60 min |
| `day` | number | last 24 h |
| `budget` | [weather.Budget](#t-weather-Budget) |  |
| `serverRemaining` | number | ServerRemaining is the server-reported remaining allowance in the current rate window (X-RateLimit-Remaining); -1 when unknown. |

<a id="store-rows"></a>

#### Database row types (store.*)

These encode themselves; unknown values are omitted. Times are Unix ms.

| Type | Fields |
|---|---|
| `store.AircraftRow` | `icao`, `firstSeen`, `lastSeen`, `callsign?`, `reg?`, `type?`, `typeDesc?`, `operator?`, `country?`, `category?`, `msgs`, `flights`, `positions` (lifetime totals), `maxDistKm?`, `maxAlt?` (ft), `srcMask`, `flags` (1 military, 2 interesting, 4 PIA, 8 LADD), `military?`, `year?` |
| `store.Flight` | `id`, `icao`, `callsign?`, `squawk?`, `start`, `end`, `msgs`, `positions`, `srcMask`, `maxDistKm?`, `maxDistBrg?`, `minDistKm?`, `minAlt?`, `maxAlt?` (ft), `maxGs?` (kt), `rssiMin?`, `rssiMax?`, `rssiAvg?` (dBFS), `firstLat?`, `firstLon?`, `lastLat?`, `lastLon?`, `emergency?`, `reg?`, `type?`, `operator?`, `country?` |
| `store.Event` | `id`, `t`, `kind` (`emergency` \| `military` \| `interesting` \| `record` \| `feed_up` \| `feed_down`), `icao?`, `detail?` (object: callsign, squawk, emergency, reg, type, operator, country, lat, lon, altFt, distKm, bearing, flightId; record events: key, value, previous; feed events: feed, name, error) |
| `store.Record` | `key`, `value` (number \| null), `icao?`, `t`, `detail?` (object: callsign, reg, type, lat, lon, altFt, distKm, bearing, gsKt, rssi) |
| `store.RangeHour` | `t` (start of the hour), `ant`, `maxKm?`, `p99Km?`, `p95Km?`, `p90Km?`, `p50Km?`, `positions`, `aircraft`, `farCount` (positions beyond 300 km), `residDb?` (mean of rssi + 20 log10(slant km)), `residN`, `distHist?` (120 counts, 5 km bins), `sectorMax?` (36 maxima in km, 10 degree sectors) |
| `store.Weather` | `t` (start of the hour), `issued?`, `leadH?` (forecast rows), `tempC?`, `dewpointC?`, `rh?`, `pressureMsl?`, `surfacePressure?`, `windKmh?`, `windDir?`, `gustKmh?`, `cloud?`, `cloudLow?`, `cloudMid?`, `cloudHigh?`, `precipMm?`, `rainMm?`, `snowCm?`, `visibilityM?`, `cape?`, `weatherCode?` (WMO), `isDay?` (1 / 0), `nSurface?` (N-units), `dnDh?` (N/km), `kFactor?`, `duct?` (M-units, 0 = none), `profile?` (`{levels:[{hpa,t,rh,z,ws,wd}], liftedIndex, cin, boundaryLayerM, precipProb, freezingLevelM}`, only when asked for) |
| `store.NTPSample` | `t`, `server?`, `offsetMs?`, `delayMs?`, `stratum` |
| `store.StatsRow` | `t`, `ant`, `frames`, `df0`, `df4`, `df5`, `df11`, `df16`, `df17`, `df18`, `df20`, `df21`, `dfOther`, `crcBad`, `positions`, `mlatPositions`, `aircraft`, `aircraftPos`, `adsb`, `mlat`, `modes`, `maxKm?`, `rssiAvg?`, `rssiMin?`, `rssiMax?`, `aircraftAvg?`, `minutes?` |
| `store.ACDBEntry` | `icao`, `reg?`, `type?`, `typeDesc?`, `operator?`, `flags`, `military?`, `year?` |
