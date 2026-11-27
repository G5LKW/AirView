# AirView

AirView is an ADS-B / Mode S statistics and mapping station for a Radarcape (or any Beast, AVR, SBS or
JSON-producing receiver). It is a single self-contained program: it decodes the receiver's frame stream,
tracks aircraft, stores everything in a local SQLite database and serves a web interface with a live map
(with trajectory prediction for aircraft that drop out), long-term statistics, antenna and terrain
line-of-sight analysis, an RF lab, alert rules, flight reports and replay, weather and radio-propagation
views, forecasts, timing diagnostics and a "locate me" game.

- **Windows 11**: `airview.exe` is a desktop app. It opens its own window (WebView2) and serves the same
  interface to every browser on your network.
- **Raspberry Pi** (64-bit or 32-bit Raspberry Pi OS): a headless service, used from any browser.
- No runtime, no external database. Map, chart libraries and fonts are built into the binary.

Current version: **1.0.0**. What is new, the download list and known limitations are in
[docs/RELEASE-NOTES.md](docs/RELEASE-NOTES.md). Check downloads against `SHA256SUMS`.

## Quick start on Windows

1. Run `AirView-Setup-1.0.0.exe` (Start menu entry, uninstaller, optional desktop icon), or copy the portable
   `airview.exe` to a folder you can write to, for example `C:\AirView\`. The installer is not code-signed:
   if SmartScreen warns, compare the file's SHA-256 with `SHA256SUMS`, then "More info" > "Run anyway".
2. Start AirView. A window opens on the Live Map.
3. Go to **Settings** and work through the [first-run checklist](#first-run-checklist).

No receiver yet? Start it with `airview.exe -demo`, or switch on **Settings → Demo mode**. The built-in
simulator then flies up to 60 aircraft around the station. While demo mode is on, it replaces your
configured feeds.

### Command-line flags

| Flag | Meaning |
|---|---|
| `-data <dir>` | Data directory (database, `config.json`, log, tile cache). |
| `-port <n>` | Use this HTTP port instead of the one in the configuration. |
| `-headless` | No window. Logs go to the console. This is always the case outside Windows. |
| `-demo` | Force the built-in traffic simulator. |
| `-open` | Headless only: open the default browser on the web interface. |
| `-version` | Print the version and exit. |

The environment variable `AIRVIEW_DEBUG=1` turns on debug logging.

### Where your data lives

By default the data directory is `data\` **next to `airview.exe`**, so the folder is portable. If that folder
is not writable (for example under `C:\Program Files`), AirView uses `%AppData%\AirView` instead. It holds
these files:

| File | Contents |
|---|---|
| `airview.db` (+ `-wal`, `-shm`) | All history and statistics (SQLite, WAL mode) |
| `config.json` | All settings. They are edited from **Settings**, which applies changes live. |
| `airview.log`, `airview.log.1` | Log, rotated at 4 MB |
| `tilecache\` | Cached map tiles, so the map also works when you are offline later |
| `terrain\` | Cached elevation tiles for the terrain analysis (about 35-40 MB for the default 450 km radius) |
| `webview2\`, `window.json` | Window browser profile and window position |

Only one instance can run per data directory. Starting `airview.exe` a second time brings the running
window to the front.

### Ports

The web server listens on **port 80** by default, so other devices can open `http://<pc-name>/`. If port 80
is already taken (often by IIS, Skype or another web server), AirView tries 8080, 8081 … 8089 and then any
free port. The window opens on the right port automatically. Other devices need the actual port, which is
shown in the log, in **Settings → Web server** and in `GET /api/status` (`httpPort`). You can set a fixed port
in **Settings → Web server**. Windows Firewall asks once whether to allow AirView on your network.

From the PC itself you always have admin rights. If you set an admin password under **Settings → Web server**,
other devices can view everything but need the password to change settings.

## First-run checklist

1. **Receiver feed**: **Settings → Receiver feeds**. The default feed is a Radarcape on host `radarcape`, Beast
   port 10003. Enter your Radarcape's host name or IP address. Presets cover the Radarcape SBS port 30003,
   dump1090/readsb Beast 30005, AVR, SBS and JSON. Beast from a Radarcape is the best input: it carries GPS
   timestamps, signal levels and both antenna channels.
2. **Station location**: **Settings → Station location**. Paste coordinates in any common format (decimal,
   degrees-minutes-seconds, Maidenhead locator, Plus Code, Google or OSM link), or pick the spot on the map. The
   ground elevation is filled in from a terrain model. Range, coverage, horizon, antenna and weather analyses
   all need the location. The **Locate Me** page can estimate it from the traffic if you do not know it exactly.
3. **Antennas**: **Settings → Antennas**. Set the type (vertical, collinear, Yagi, patch), gain, height above
   ground, cable/LNA/filter figures, and for directional antennas the azimuth and beamwidths. Map each antenna to
   its receiver channel. The defaults are a vertical on channel 0 and a Yagi on channel 1.
4. **NTP server**: **Settings → Time (NTP)**. This is off by default. Enter `pool.ntp.org` or your router, so
   the Timing page can measure the PC clock error and the true feed latency. **Test now** queries the typed
   server once (host, `host:port`, IPv4 or `[IPv6]:port`, UDP port 123 by default) and shows offset, delay,
   stratum and reference with a "GPS-locked stratum 1" badge when it qualifies, without saving anything;
   **Save** makes it the reference at once. For latency work prefer a GPS-locked stratum-1
   server on your own network (for example the Radarcape's own NTP service): every absolute latency figure is
   only as good as this server. AirView only measures the clock; it never sets it.
5. Optional: weather (on by default; it uses Open-Meteo once the location is set), the rain-radar overlay
   (off by default), retention periods, units and map style.
6. Optional: **Settings → Aviation data**: import airports, runways, navaids and callsign routes (about 25 MB
   download, once) for the map's airport layer and the route shown for each flight. **Settings → Terrain**
   downloads elevation tiles on its own once the location is set (see the terrain limitation below).
7. Optional: import an aircraft database (**Aircraft → Aircraft database**, or Settings) for registrations and
   types. See [Data sources](#data-sources-attributions-and-licences).

## Raspberry Pi install

The Pi bundle `airview-pi-<version>.tar.gz` contains both Linux builds (arm64, armv7), the installer, `update.sh`,
`uninstall.sh` and a systemd unit.

```sh
scp airview-pi-<version>.tar.gz pi@raspberrypi:
ssh pi@raspberrypi
tar xzf airview-pi-<version>.tar.gz
cd airview-pi-<version>
sudo ./install.sh
```

Where the release files are published on a web server, `airview-pi-install.sh` does the same in one line
(`curl -fsSL <site>/airview-pi-install.sh | sudo sh -s -- --base-url <site>`); `/opt/airview/update.sh` later
upgrades from the same place. A ready SD-card image, `AirView-Station-Pi-<version>.img.zip`, needs no
installer at all: write it with Raspberry Pi Imager and boot.

For 1.0.0 the files are `airview-pi-1.0.0.tar.gz` and `AirView-Station-Pi-1.0.0.img.zip`; a feeder-only Pi uses
`airview-feeder-pi-1.0.0.tar.gz` or `AirView-Feeder-Pi-1.0.0.img.zip` (see
[docs/aggregator/FEEDER-GUIDE.md](docs/aggregator/FEEDER-GUIDE.md)).

The installer chooses the arm64 or armv7 binary for the board. It installs the binary to
`/opt/airview/airview`, keeps data in `/var/lib/airview` and runs the service as the unprivileged user
`airview`, which may bind port 80. Open `http://<pi-address>/` in a browser and go through the checklist
above. Logs: `journalctl -u airview -f`. Running `install.sh` again upgrades in place and keeps the data.
`sudo ./uninstall.sh` removes the service; `--purge` also deletes the data. More detail is in
[`deploy/pi/README.md`](deploy/pi/README.md).

## The three connection modes

Set under **Settings → Connection / remote**:

| Mode | What happens | Typical use |
|---|---|---|
| **Standalone** (default) | Feeds → decoder → database → web UI, all on this machine. | The PC or Pi that is connected to the receiver. |
| **Remote data** | The desktop app logs in to another AirView's **data port** (hub, TCP 30154; challenge-response login, so the password never crosses the network). It receives that station's live frame stream, position and antennas, and runs its own decoder, statistics and database locally. | A Windows PC elsewhere in the house, with full functionality, using the Pi's receiver. |
| **Remote website** | The desktop window shows the host's own web interface inside a thin local frame with a "disconnect / change connection" bar. Nothing is stored locally. | Just looking at the Pi from the PC. |

To offer the data port, enable **Data port (hub) of this station** on the host and set a password. A Pi always
runs standalone. It can serve desktop clients through the data port. The environment variable
`AIRVIEW_REMOTE=1` lets a headless instance use remote-data mode.

## Feature tour

- **Live Map** (`#/map`): GPU-rendered aircraft with altitude-coloured trails, a sortable and filterable
  aircraft list (position, ADS-B, MLAT, military, emergency), a detail panel for each aircraft (identity,
  accuracy figures, Comm-B data such as selected altitude, wind, roll and Mach, signal history), receiver
  overlays (range rings, antenna beams, coverage), day/night terminator, optional RainViewer radar, and a
  choice of base maps. Also:
  - **Prediction.** When an aircraft's positions stop arriving, it keeps moving along the tracking filter's
    prediction for up to `prediction.coastSec` (60 s by default). It glows in its own colour, shows a 1-sigma
    uncertainty circle and a dotted link to its last real position, and is marked PRED in the list and the
    detail panel. Predicted positions are display-only: they never enter trails, the database or any
    statistic. Predicted paths (+30 s / +60 s ticks, growing uncertainty) for the selected aircraft or all
    of them, smooth motion between the 1 Hz updates, flight phase, closest approach and a terrain
    line-of-sight forecast in the detail panel.
  - **Overlays:** measured range outline, theoretical terrain coverage rings, airports with runways and
    extended centrelines, all-time heat map, predicted close approaches (informational only).
  - **Route** line and airports for the selected flight from the imported standing data, with a
    plausibility badge. "Full trace" of today's stored flight, KML / GeoJSON / CSV export.
  - Filter panel (altitude and speed ranges, callsign, type, operator, category, source, military, ground,
    alerting, predicted) with removable chips, configurable list columns, multi-select (Shift / Ctrl click),
    a measuring tool with a terrain profile, keyboard shortcuts (`?`), shareable URLs
    (`#/map/<ICAO>?zoom=&lat=&lon=&filter=`), a kiosk mode, and alert-rule highlight rings and toasts with
    an optional beep.
- **Aircraft** (`#/aircraft`): every airframe ever received, with filters (country, type, military, with
  registration), recent flights, a log of events and records, and the aircraft-database import. Each airframe
  has a drawer with reference data, a trail map and altitude/speed and signal/distance charts.
- **Statistics** (`#/stats`): live tiles, then tabs. Overview: totals, message and position rates, aircraft by
  source, range and signal. Patterns: weekday × hour heat map, average day, daily totals, distance and altitude
  distributions. Messages: downlink formats, ADS-B/MLAT/Mode S share, Comm-B registers. Fleet: top operators,
  countries, types and airframes. Records & events. Health: decode quality, feeds, process and database.
- **Antenna Lab** (`#/antenna`): station score, coverage polar plots per altitude band and antenna, radio
  horizon versus measured range, terrain skyline and obstructions, signal versus distance, elevation response,
  a fitted Yagi lobe, link budget and what-if calculator, day/night comparison, position heat map, and a "show
  on live map" overlay. The **Terrain & line of sight** tab (`#/antenna/terrain`) compares the terrain
  skyline with the measured one (sectors where something near the antenna blocks the view), theoretical
  against measured coverage per altitude, a coverage map, mast-height advice (+2 / +5 / +10 m) and an
  interactive line-of-sight profile with Fresnel zone, diffraction loss and a k-factor slider.
- **RF Lab** (`#/rf`): health score of the receive chain with advice, signal level histograms per antenna,
  decode and estimated noise floor, detection probability against distance, elevation, bearing and level,
  update intervals, channel occupancy and the overlap (garble) model, interrogators (II/SI codes, Comm-B,
  ACAS), squitter timing compliance, ADS-B fleet quality (version, NIC/NACp/SIL), a GNSS-interference index
  and map, and the prediction-error scores of the tracking filter.
- **Alerts** (`#/alerts`): rules with nested ALL / ANY / NOT conditions over 24 condition types (callsign,
  squawk, type, operator, altitude, speed, distance, geofence circle or polygon, watch-lists, system events
  such as feed down, range collapse, NTP loss, new records, new airframes ...), actions (notice, sound, log,
  map highlight colour, webhook), cooldowns, message templates, a dry run against the live picture, import
  and export. Watch-lists can be imported from CSV / text. Built-in rules cover emergencies, military,
  records, feed loss and more; most are off by default.
- **Reports & Replay** (`#/reports`): a VirtualRadar-style flight report (time range, callsign,
  registration, ICAO, type, operator, country, squawk, distance and altitude limits ...) with CSV export, a
  flight page with trace map, profile charts and KML / GeoJSON / CSV downloads, a daily summary, and a
  replay of up to 24 h of stored traffic at 1x to 600x (`#/reports/replay`).
- **Weather** (`#/weather`): current conditions (Open-Meteo), refractivity and ducting analysis with a duct
  diagram, time–height profiles, the sun and moon, winds and temperatures aloft measured by aircraft (Mode S
  EHS / Comm-B), a comparison of those with the weather model, and the effect of weather on reception once
  enough history exists.
- **Forecast** (`#/forecast`): reception now-cast and 72-hour outlook, traffic forecast, local weather forecast
  with optional learned bias correction and an analogue forecast, plus verification of past forecasts. It starts
  physics-only and becomes data-driven as history accumulates.
- **Timing** (`#/timing`): system clock against NTP, clock-offset history, receiver GPS time, receiver status
  frames and latency for each feed. **Measure now** takes an NTP sample on demand (admin). The **Time difference
  of arrival** section breaks the delay of a position down into stages, using the Radarcape's GPS time stamps
  against the NTP-corrected host clock:
  - a **stage waterfall** for one aircraft (pick it from the list; default the one with the most positions):
    position age at transmit, radio propagation, receiver → host, AirView processing, each with its ±
    uncertainty; estimated parts are hatched;
  - **stage cards** (median, min, p95, p99) and **distributions** for propagation, receiver → host,
    processing, transponder latency (T=1), position-age change and the totals, over a rolling 15-minute window;
  - a **receiver-delay chart** per feed for the last 24 h (median, min…p95 band and the clock-filter floor) and
    an **NTP / GPS consistency** check (server quality, offset uncertainty, 10 s delay minima, drift, issues);
  - a per-aircraft table and plain-language **findings**.

  What is exact and what is estimated: propagation comes from geometry alone. ADS-B positions with the T bit
  set (T=1, about 1 % of aircraft) apply to an exact 0.2 s UTC epoch, so their transponder latency is measured
  exactly. For the usual T=0 positions only the *variation* of the position age is observable; the mean age
  shown is a model estimate marked "est.". Receiver → host and the end-to-end figures need a configured NTP
  server; without one they include the full PC clock error and the page says so. MLAT results are excluded
  because their time stamps are synthetic. Only feeds with GPS time stamps (Radarcape Beast in GPS mode, the
  demo simulator) produce latency data. In demo mode about 10 % of the simulated ADS-B aircraft are UTC-coupled
  (T=1), so the transponder-latency cards and the TDOA section fill within a few minutes; the simulator hands each
  frame over when the wall clock reaches its receive time, so "AirView processing" shows real processing time.
- **Locate Me** (`#/guess`): estimates the station position from the received traffic alone with four
  independent methods and a fused result, compares it with the configured position, and can adopt it.
- **Settings** (`#/settings`): all configuration, with validation, live preview of antenna patterns, admin
  login, database maintenance and factory reset. The Time section shows the NTP reference with "Test now"
  (one query to the typed server, nothing saved) and "Save" buttons. Prediction (on/off, coast time, path length), Terrain (radius, zoom, k-factor, download and
  re-run), Aviation data (import with progress, opt-in online route look-up and aircraft photos), Alerts
  (sound, webhooks) and RF statistics have their own sections with live status panels.

The HTTP API behind all of this is documented in [`docs/API.md`](docs/API.md).

## Data sources, attributions and licences

Data fetched at runtime (an internet connection is needed for these; map tiles go through the local cache):

| Source | Used for | Terms |
|---|---|---|
| [OpenFreeMap](https://openfreemap.org/) / [OpenMapTiles](https://openmaptiles.org/) | Vector base map, fonts, sprites | Free, no key. Map data © [OpenStreetMap contributors](https://www.openstreetmap.org/copyright) (ODbL). The attribution is shown on the map. |
| [OpenStreetMap](https://www.openstreetmap.org/copyright) tile servers | Optional raster base map | OSMF tile usage policy (interactive use only, attribution, identifying User-Agent). |
| [Open-Meteo](https://open-meteo.com/) | Weather forecast, weather history, pressure-level profiles, ground elevation (Copernicus DEM GLO-90) | Free for non-commercial use, data under CC BY 4.0. "Weather data by Open-Meteo.com" is shown on the weather pages. |
| [RainViewer](https://www.rainviewer.com/api.html) | Optional rain-radar overlay | Free for personal/educational use, with attribution and link. |
| NTP server you configure (for example [pool.ntp.org](https://www.ntppool.org/)) | Clock reference | Use of the NTP Pool is subject to its terms. |
| [Mapzen Terrain Tiles on AWS Open Data](https://registry.opendata.aws/terrain-tiles/) (terrarium PNG) | Terrain line of sight, coverage, horizon, mast advice | Open data; SRTM, GMTED2010, 3DEP (USGS), ETOPO1 (NOAA), EU-DEM (Copernicus) and others, see the [attribution list](https://github.com/tilezen/joerd/blob/master/docs/attribution.md). Shown on the terrain panels. |
| [OurAirports](https://ourairports.com/data/) | Airports, runways, navaids, countries | Public domain (repository licence: The Unlicense). Downloaded only when you import it. |
| [Virtual Radar Server standing data](https://github.com/vradarserver/standing-data) | Callsign routes | CC0 1.0. Downloaded only when you import it. |
| [adsbdb.com](https://www.adsbdb.com/) | Optional online route look-up for callsigns missing from the standing data | Off by default. Sends callsigns from the AirView host. |
| [planespotters.net](https://www.planespotters.net/) | Optional aircraft photos | Off by default, needs a contact in `navData.photoContact`. Sends ICAO address / registration; the photographer credit is shown with each photo. |

An aircraft database is optional. It is downloaded only when you ask for it and is not bundled:

| Source | Licence |
|---|---|
| [tar1090-db](https://github.com/wiedehopf/tar1090-db) (built from the Mictronics database) | Open Data Commons Attribution License (ODC-By) v1.0 (the licence of the Mictronics data; the repository has no licence file). Attribution is shown in the UI. |
| [OpenSky Network aircraft database](https://opensky-network.org/data/aircraft) | Unlicensed, offered "as is" by OpenSky. Attribution is shown in the UI. |

Built into the binary:

| Component | Licence |
|---|---|
| [MapLibre GL JS](https://maplibre.org/) 6.18.0 | BSD-3-Clause |
| [Apache ECharts](https://echarts.apache.org/) 6.1.0 (includes zrender, BSD-3-Clause) | Apache-2.0 |
| [Inter](https://rsms.me/inter/) and [JetBrains Mono](https://www.jetbrains.com/lp/mono/) fonts | SIL Open Font License 1.1 |
| ICAO address allocations, airline designators, aircraft type designators, squawk tables | Compiled from public ICAO documents and public references |
| World Magnetic Model coefficients (NOAA/NCEI) | Public domain |
| Go modules (see `go.mod`; for example `modernc.org/sqlite`, `go-webview2`) | Their respective BSD/MIT-style licences |

The licence files are in `web/static/vendor/` (see `web/static/vendor/VERSIONS.md`).

**Test data:** the Mode S decoder tests (`internal/modes`) use only self-generated frames (field values made
up in the test and encoded with the package's own encoders, see the header of `internal/modes/synth_test.go`)
and the worked examples of *The 1090 Megahertz Riddle* (Junzi Sun, TU Delft OPEN Publishing). Files under
`internal/*/testdata` are small samples of third-party formats. These files are **test vectors only**: Go
never compiles `testdata` into the binaries.

## Network aggregator and Linux servers

AirView can also run as a network aggregator that merges many feeders (setting `mode: "aggregator"`).
The Linux server kit is in [`deploy/server/`](deploy/server/README.md) (installer, upgrades, backups,
hardened systemd unit, reverse-proxy examples), with a [`Dockerfile`](Dockerfile) and
[`docker-compose.example.yml`](docker-compose.example.yml). Feeder stations run `airview-feeder`
(Pi installer: [`deploy/feeder/pi/`](deploy/feeder/pi/)). The operator guide, feeder guide and privacy
templates are in [`docs/aggregator/`](docs/aggregator/README.md); every setting is listed in
[`docs/CONFIG.md`](docs/CONFIG.md).

## Building from source

Requires Go (see `go.mod`); no cgo is needed.

```powershell
powershell -ExecutionPolicy Bypass -File deploy\build.ps1            # vet + test + all targets
powershell -ExecutionPolicy Bypass -File deploy\build.ps1 -SkipTests # just build
```

This produces, among others, `dist\airview.exe` (Windows GUI, with icon and version resources from
`cmd\airview\rsrc_windows_*.syso`), the Windows installers `AirView-Setup-<version>.exe` and
`AirView-Feeder-Setup-<version>.exe`, the headless Linux binaries, the Pi station bundle
`airview-pi-<version>.tar.gz` with its one-line installer `airview-pi-install.sh`, the feeder bundle
`airview-feeder-pi-<version>.tar.gz` with `airview-feeder-install.sh`, the server kits
`airview-server-linux-{amd64,arm64}.tar.gz` with `airview-server-install.sh`, the Docker build context
`airview-docker-<version>.tar.gz` and `SHA256SUMS`. `-PiImages <Raspberry Pi OS Lite .img.xz>` adds the SD-card
images `AirView-{Station,Feeder}-Pi-<version>.img.zip`; `-SignKey <key>` adds signed update manifests. Every
artifact is verified at the end and the run fails when anything is wrong. If `dist\airview.exe` is still
running, the script renames it to `airview.exe.old` first. `-Resources` regenerates the Windows resources,
which needs network access the first time. `-OutDir <dir>` writes the artifacts elsewhere. The full list is in
the header of `deploy/build.ps1` and in `deploy/WIRING.md`.

## Known limitations

- **Young stations look empty.** Many analyses need history: hourly range rows appear after the first full
  hour, the day/night comparison needs 48 h or more, the weather-impact study needs 48 h, the learned
  reception model needs about 3 days, and the analogue weather forecast and bias correction need weeks. Until
  then, each panel says what it is waiting for.
- **Demo mode replaces your feeds.** It does not run alongside them. The simulator follows a day/night traffic
  curve, so at night there are far fewer than 60 aircraft. With weather enabled, demo mode still polls
  Open-Meteo for the simulated site.
- **Raspberry Pi builds and SD-card images were cross-compiled, packaged and checked offline, but not booted
  on real Pi hardware** for this release (1.0.0).
- **Remote-data mode** was tested at configuration level and with the hub's own client/server tests. A full
  desktop-to-Pi session was not exercised end to end. Settings adopted from the host are kept in memory only.
  Switching an existing station to remote-data mixes both stations' statistics in one database unless you
  factory-reset first.
- **Admin sessions are in memory.** Restarting AirView logs out every remote admin. Loopback (the PC itself) is
  always admin.
- **Windows without WebView2:** the app keeps running headless, with no window and no tray icon, and must be
  ended in Task Manager (a message box says so). WebView2 ships with Windows 11.
- **Factory reset** briefly interrupts live streams (SSE) and raw downloads while the engine restarts, and
  cancels an aircraft database upload that is still being sent.
- Texts generated by the server (forecast notes, the Zambretti outlook, analysis conclusions) are always in
  hPa and km, whatever display units you choose.
- On Linux, nothing stops two processes from using the same data directory. Do not start a second copy by hand
  while the service runs.
- The tile cache and weather need internet access. Without it, the map shows only previously cached areas.
- **Terrain downloads by itself.** `terrain.autoPrefetch` is on by default: once the station is located,
  AirView fetches about 35-40 MB of elevation tiles (450 km radius) and runs a 10-15 s analysis. Turn it off
  under **Settings → Terrain** on a metered connection. Offline, tiles that are not cached read as sea level;
  the terrain panels then warn that the terrain data is incomplete and the tile server is unreachable, and
  "Download & analyse again" fills the gaps later.
- **Prediction is a display aid.** The coasting position and its uncertainty circle come from the tracking
  filter. The circle is a deliberately pessimistic 1-sigma area that assumes the aircraft may have turned
  (standard-rate turn hypothesis) since the data stopped: about 250 m after 10 s, about 1 km after 20 s and
  several km after 40 s for a jet, much less while velocity reports still arrive. It is not a guarantee: an
  aircraft can be outside it. On the forecast path of the selected aircraft, tick marks whose 1-sigma
  exceeds 5 km are drawn without a circle. Alert rules are evaluated on the live picture, so a geofence or
  distance rule can fire on a predicted position. The coast time cannot be 0: switch prediction off to hide
  aircraft as soon as positions stop.
- **Alerts in the browser:** toasts (one per alert, with *Show* jumping to the aircraft on the map) and the
  chime appear on every page, controlled by `config.alerts.sound` and a per-browser mute (Alerts page
  *Sound* switch). Your own message templates use ft and km whatever display units you choose; built-in
  alert texts carry the numbers as separate fields (altitude, distance, speed, the event value and its unit),
  so the UI can show them in your units, and the RF / antenna advice is rewritten in your units by the UI.
  Webhooks are sent from the AirView host.
- **Flight airports are guesses.** Departure / arrival are inferred from the first / last low positions near
  an airport of the imported airport data and stored about 35 minutes after a flight ends (older flights are
  filled in slowly in the background, newest first); without imported airport data they stay empty. Reports
  can filter and sort by them. The replay window is at most 24 h and gets coarser for long windows.
- **Daily report "farthest flight"** shows the flight's start time, not the moment of the maximum distance,
  which is not stored per flight.
- **`navData.photoContact`** is visible to everyone who can open the web interface (`GET /api/config`).
- The RF Lab's hourly history needs the first full clock hour after a start; the RF statistics window is
  kept in memory and restarts with the program.
- Low-power (ARM) mode switches off the map's smooth motion and the animated prediction glow; the glow is
  then static.
