package server

import (
	"net/http"
	"time"
)

// Param documents one query or path parameter.
type Param struct {
	Name string
	Desc string
}

// Route is one API endpoint. The table below is the single place where
// endpoints are declared: the mux is built from it, the tests iterate over
// it and docs/API.md is generated from it.
type Route struct {
	Method string
	Path   string // net/http pattern path, e.g. /api/aircraft/{icao}
	Group  string // section in docs/API.md

	Summary string  // one line
	Notes   string  // further documentation (Markdown)
	Params  []Param // query parameters (path parameters are explained in Notes)
	Body    string  // documentation of the request body ("" = none)
	Returns string  // response when it is not a JSON document produced by API

	// Admin: mutating / maintenance endpoint, needs the admin login for
	// non-loopback clients when a password is set.
	Admin bool
	// Access is who may read the route in aggregator mode (access.go,
	// docs/aggregator/DESIGN.md §8.2/§8.4). Every route must declare it
	// (TestRouteAccessLint); there is no default. Standalone mode ignores it.
	Access Access
	// Station is how the route selects a station (StationNone when it does
	// not). Routes with a {sid} path value use StationPath, routes taking
	// ?station= use StationQuery; the server verifies ownership before the
	// handler runs.
	Station StationSel
	// NetOnly: the route exists only in aggregator mode (404 in standalone).
	NetOnly bool
	// AccountLimited: owner requests are rate-limited per account
	// (acctRate), on top of the public per-address limit. Admins are
	// exempt.
	AccountLimited bool
	// Credentials: the request carries a password, TOTP code or setup
	// token. In aggregator mode it is refused from remote clients over
	// plain HTTP unless http.allowInsecureLogin is set (SEC-01).
	Credentials bool
	// LongBody: the request body may take longer than bodyReadTimeout to
	// arrive (large uploads with their own bounds).
	LongBody bool
	// Exclusive: runs alone, after all requests in flight (factory reset).
	Exclusive bool
	// Cache > 0: the encoded response is shared between identical requests
	// for this long, and concurrent identical requests are computed once.
	Cache time.Duration

	API         func(s *Server, r *http.Request) (any, error)           // JSON endpoint
	Raw         func(s *Server, w http.ResponseWriter, r *http.Request) // streaming / special endpoint
	ContentType string                                                  // "" = application/json

	// Sample is an example request (path and query) used by the tests and
	// for the example in the docs; "" = Path. SampleBody is its JSON body.
	Sample     string
	SampleBody string
	// NoAuto: not called by the generic every-route test (destructive,
	// streaming, or needs the internet); such routes have their own tests.
	NoAuto bool
}

// Routes returns the route table (read-only).
func Routes() []Route { return routes }

const (
	grpCore     = "Core"
	grpStats    = "Statistics"
	grpAntenna  = "Antenna & coverage"
	grpWeather  = "Weather"
	grpMet      = "Aircraft-derived meteorology"
	grpForecast = "Forecasts"
	grpTiming   = "Timing"
	grpGuess    = "Location guesser"
	grpHistory  = "History"
	grpDB       = "Database"
	grpGeo      = "Geo, astronomy & reference data"
	grpSystem   = "System"
)

var (
	pRange = []Param{
		{"from, to", "time range in Unix ms (`to` defaults to now)"},
		{"hours", "alternative: the last N hours (ending at `to` or now); fractions allowed"},
		{"days", "alternative: the last N days"},
	}
	pAnt  = Param{"ant", "antenna ID (`config.antennas[].id`); empty, `all` or `combined` = all antennas combined. Per-antenna data exists only when feeds are attributed to an antenna (`feed.antennaId`) or in demo mode"}
	pBand = Param{"band", "altitude band 0..6 (0 <1000 ft, 1 1-5k, 2 5-10k, 3 10-20k, 4 20-30k, 5 30-40k, 6 >=40k)"}
)

func params(groups ...any) []Param {
	var out []Param
	for _, g := range groups {
		switch v := g.(type) {
		case Param:
			out = append(out, v)
		case []Param:
			out = append(out, v...)
		}
	}
	return out
}

const (
	ttlLive  = 2 * time.Second  // live-ish data: only de-duplicates
	ttlShort = 10 * time.Second // minute statistics
	ttlMed   = 60 * time.Second // hourly statistics
	ttlLong  = 5 * time.Minute  // heavy analyses (the analyzer caches them for 5-15 min itself)
)

var routes = []Route{
	// ------------------------------------------------------------------ core
	{Method: "GET", Path: "/api/status", Group: grpCore, Access: AccessViewer, API: apiStatus,
		Summary: "Application status (`model.Status`).",
		Notes:   "`adminLocked` is computed per client: true when this client has to log in before using an admin endpoint. `located`, `mode` and `demo` are the *effective* values (in demo mode `located` is true although `GET /api/config` may still say `receiver.located=false`). `tileCacheHosts` is present only while the tile cache is enabled. `station` is the effective station position every analysis uses: `{lat, lon, source}` with source `config` (located receiver; in demo mode the simulator site), `gps` (no located receiver, but the receiver reports its own GPS position, Beast type-5 frames; same as `receiverGps`) or `none` (lat / lon null)."},
	{Method: "GET", Path: "/api/config", Group: grpCore, Access: AccessViewer, API: apiGetConfig,
		Summary: "The stored configuration (`model.Config`) with secrets blanked.",
		Notes:   "`http.adminPasswordHash`, `hub.password` and `remote.password` are `\"\"` (not set) or `\"set\"`. For the admin, settings fixed by the operator config file, an `AIRVIEW_*` environment variable or a `-set` flag are listed in an extra member `\"locked\":[{\"path\":\"http.port\",\"origin\":\"env AIRVIEW_HTTP_PORT\"}]` (present only when there are any; values are never shown). Changes to locked settings are saved but the override stays in force."},
	{Method: "PUT", Path: "/api/config", Group: grpCore, Access: AccessAdmin, API: apiPutConfig, Admin: true, NoAuto: true,
		Summary: "Validate, save and apply the configuration live; returns the stored configuration (redacted).",
		Body:    "`model.Config` (the object returned by `GET /api/config`, edited). Top-level sections that are missing keep their stored value; a section that is present replaces the stored one completely (send whole `antennas` / `feeds` arrays). Secrets: `\"\"` or `\"set\"` keeps the stored secret, `\"clear\"` removes it, any other value is the new secret; for `http.adminPasswordHash` send the new plain password as `\"plain:<password>\"`.",
		Notes:   "Invalid values are corrected, not rejected (the corrections arrive as `notice` events with level `warn`). A changed `http.port` / `http.bindAddr` is saved but needs a restart: see `restartRequired` in `GET /api/system`. Changing the admin password ends all login sessions."},
	{Method: "POST", Path: "/api/factory-reset", Group: grpCore, Access: AccessAdmin, API: apiFactoryReset, Admin: true, Exclusive: true, NoAuto: true,
		Summary: "Wipe the database and restore the default configuration, then restart the engine.",
		Body:    "`{\"confirm\":\"RESET\"}`",
		Notes:   "Answers `{\"ok\":true}` when the engine is running again (can take a few seconds; other requests wait meanwhile). The tile cache is kept. All login sessions end."},
	{Method: "POST", Path: "/api/login", Group: grpCore, Access: AccessPublic, Raw: rawLogin, NoAuto: true, Credentials: true,
		Summary: "Admin login.", Body: "`{\"password\":\"...\"}`",
		Returns: "`sessionInfo` JSON and `Set-Cookie: airview_session=...; HttpOnly; SameSite=Lax`.",
		Notes:   "`401 {\"error\":\"wrong password\"}` on failure. After 5 failures from one address further attempts are refused with `429` and a `Retry-After` header (2 s, doubling up to 5 min). Without a configured password it answers `200` with `admin:true` and sets no cookie. Aggregator mode: the body is `{\"username\":\"...\",\"password\":\"...\",\"code\":\"123456 or a recovery code\"}`; a `401` with `codeRequired:true` asks for the code; the cookie is `SameSite=Strict` (plus `Secure` over TLS)."},
	{Method: "POST", Path: "/api/logout", Group: grpCore, Access: AccessPublic, Raw: rawLogout, NoAuto: true,
		Summary: "End the admin session.", Returns: "`sessionInfo` JSON; the cookie is cleared."},
	{Method: "GET", Path: "/api/session", Group: grpCore, Access: AccessPublic, API: apiSession,
		Summary: "Whether this client is admin and whether a password is configured."},
	{Method: "GET", Path: "/api/aircraft", Group: grpCore, Access: AccessViewer, Station: StationQuery, API: apiAircraft, Params: []Param{pSID},
		Summary: "Live aircraft table (`model.LiveSnapshot`), rebuilt once per second.",
		Notes:   "Optional fields are omitted when unknown. Units: ft, kt, ft/min, km, degrees true, dBFS, Unix ms."},
	{Method: "GET", Path: "/api/aircraft/{icao}", Group: grpCore, Access: AccessViewer, API: apiAircraftDetail, Sample: "/api/aircraft/{live}",
		Summary: "One aircraft: live state, live trail, reference data, aircraft-database record, all-time totals and recent flights.",
		Params:  []Param{{"smooth", "`1` returns the live trail smoothed by the tracking filter (RTS smoother; useful for noisy MLAT trails)"}},
		Notes:   "`{icao}` is 6 hex digits (`~` prefix for non-ICAO addresses). Always `200` for a valid address: `aircraft` is null when it is not tracked right now, `seen` null when it was never stored. `watch` is `{list, note}` when the address is on an alert watch-list, else null. `trail` points are `[tMs, lat, lon, altFt|null, gsKt|null, rssi|null, src]` with src 0 ADS-B, 1 MLAT, 2 TIS-B/ADS-R, 3 other. `flights` holds the newest 20."},
	{Method: "GET", Path: "/api/trails", Group: grpCore, Access: AccessViewer, API: apiTrails, Cache: ttlLive, Sample: "/api/trails?minutes=5",
		Summary: "In-memory trails of all live aircraft.",
		Params: []Param{{"minutes", "trail length, 1..240 (default `config.ui.trailMinutes`)"},
			{"bbox", "aggregator mode below admin: `south,west,north,east`, only aircraft whose newest point is inside"}},
		Notes: "`trails` maps the ICAO address to an array of `[tMs, lat, lon, altFt|null, gsKt|null, rssi|null, src]`, oldest first. In aggregator mode below admin scope the answer is the public one (`GET /api/public/trails`: at most 10 minutes and 120 points per aircraft)."},
	{Method: "GET", Path: "/api/events", Group: grpCore, Access: AccessViewer, Station: StationQuery, Raw: rawEvents, NoAuto: true, Params: []Param{pSID, {Name: "bbox", Desc: "viewport stream, as for `GET /api/public/events`"}, {Name: "view", Desc: "`1`: whole-world viewport stream (see `POST /api/events/view`)"}, {Name: "replace", Desc: "stream id of a previous stream of this client to close first"}},
		Summary: "Live stream (Server-Sent Events).",
		Returns: "`text/event-stream`. Events: `aircraft` (data = `model.LiveSnapshot`, every second), `status` (data = `model.Status`, every 5 s and right after a configuration change), `notice` (data = `{level, text, t, kind?, icao?, key?, value?}`; level `info` | `warn` | `alert`; kind `emergency` | `military` | `interesting` | `record` | `feed_up` | `feed_down` | `alert` or absent; `key` / `value` = record key and new value for kind `record`), `alert` (data = `alerts.Alert`, whenever an alert rule fires: `{id, t, ruleId, ruleName, severity, kind: aircraft|system, event?, icao?, callsign?, reg?, type?, squawk?, lat?, lon?, altFt?, distKm?, value?, text, notify, sound, highlight?}`).",
		Notes:   "Alerts: show a toast for `alert` events with `notify` true, coloured by `severity` (`critical` red, `warning` amber, `info` blue), play a sound when `sound` is true (false whenever `config.alerts.sound` is off), clicking selects `icao`. Alerts of user rules and system alerts (feed down, range collapse, NTP) are additionally sent as `notice` with kind `alert`; the built-in emergency, military and record rules are not, because the tracker already sends notices for those. The first `status` and `aircraft` events are sent immediately on connect, preceded by `retry: 2000`. A comment line (`: hb`) is sent every 15 s. A client that does not read is disconnected after about 20 s of backlog; at most 64 streams are served (`503` beyond). Usable cross-origin (no credentials)."},
	{Method: "GET", Path: "/api/notices", Group: grpCore, Access: AccessViewer, API: apiNotices,
		Summary: "The most recent notices (at most 50, oldest first) for clients that connect late."},
	{Method: "GET", Path: "/api/feeds", Group: grpCore, Access: AccessAdmin, API: apiFeeds,
		Summary: "Feed connections, per-feed and per-antenna counters, and the data port (hub) with its clients."},
	{Method: "GET", Path: "/api/tilecache", Group: grpCore, Access: AccessAdmin, API: apiTileCacheStats,
		Summary: "Tile cache size and counters."},
	{Method: "POST", Path: "/api/tilecache/clear", Group: grpCore, Access: AccessAdmin, API: apiTileCacheClear, Admin: true, NoAuto: true,
		Summary: "Delete all cached map tiles.", Returns: "`{\"ok\":true}`"},

	// ------------------------------------------------------------ statistics
	{Method: "GET", Path: "/api/stats/overview", Group: grpStats, Access: AccessAdmin, API: apiStatsOverview, Cache: ttlShort,
		Summary: "Everything a statistics landing page needs: last 24 h totals, sources, downlink formats, all-time totals, today, busiest day/hour, top lists, records, recent events.",
		Params:  params(pAnt)},
	{Method: "GET", Path: "/api/stats/rates", Group: grpStats, Access: AccessAdmin, API: apiStatsRates, Cache: ttlShort, Sample: "/api/stats/rates?hours=6",
		Summary: "Traffic over time: message rate, positions, aircraft counts, range and signal as parallel arrays, plus totals.",
		Params:  params(pRange, pAnt),
		Notes:   "Default 24 h. `resolution` is `minute` for spans up to 12 h, else `hour`. Every array in `series` has one value per `series.t` entry; null = no data."},
	{Method: "GET", Path: "/api/stats/df", Group: grpStats, Access: AccessAdmin, API: apiStatsDF, Cache: ttlShort,
		Summary: "Downlink-format and source mix over a range, plus the live DF / Comm-B register counters since start.",
		Params:  params(pRange, pAnt)},
	{Method: "GET", Path: "/api/stats/heatmap", Group: grpStats, Access: AccessAdmin, API: apiStatsHeatmap, Cache: ttlMed,
		Summary: "Day-of-week x hour-of-day heat map (7 x 24, Monday first, local time) of aircraft and message rate.",
		Params:  params(pRange, pAnt), Notes: "Default 28 days."},
	{Method: "GET", Path: "/api/stats/daily", Group: grpStats, Access: AccessAdmin, API: apiStatsDaily, Cache: ttlMed,
		Summary: "Daily totals (local days) with the busiest day and hour.",
		Params:  params(pRange, pAnt), Notes: "Default 30 days."},
	{Method: "GET", Path: "/api/stats/top", Group: grpStats, Access: AccessAdmin, API: apiStatsTop, Cache: ttlMed, Sample: "/api/stats/top?limit=5",
		Summary: "Top operators, countries, aircraft types and airframes.",
		Params:  params(pRange, Param{"limit", "rows per list, 1..200 (default 15)"}), Notes: "Default 30 days."},
	{Method: "GET", Path: "/api/stats/altitudes", Group: grpStats, Access: AccessAdmin, API: apiStatsAltitudes, Cache: ttlMed,
		Summary: "Altitude distribution: positions per altitude band and a histogram of flight maximum altitudes.",
		Params:  params(pRange, pAnt)},
	{Method: "GET", Path: "/api/stats/records", Group: grpStats, Access: AccessAdmin, API: apiStatsRecords, Cache: ttlShort,
		Summary: "All-time records.",
		Notes:   "Keys: `max_dist_km`, `max_alt_ft`, `max_gs_kt`, `min_rssi_pos_dbfs`, `max_aircraft`, `max_msg_rate`, `max_flight_s`. `detail` (when present) has callsign, reg, type, lat, lon, altFt, distKm, bearing, gsKt, rssi."},
	{Method: "GET", Path: "/api/stats/events", Group: grpStats, Access: AccessAdmin, API: apiStatsEvents, Sample: "/api/stats/events?limit=5",
		Summary: "Event log, newest first: emergencies, military / interesting catches, new records, feed up / down.",
		Params: []Param{{"limit", "1..1000 (default 100)"}, {"kind", "`emergency` | `military` | `interesting` | `record` | `feed_up` | `feed_down`"},
			{"icao", "only this aircraft"}, {"since", "only events at or after this time (Unix ms)"}, {"before", "paging: only events with an id below this one"}}},
	{Method: "GET", Path: "/api/stats/counters", Group: grpStats, Access: AccessAdmin, API: apiStatsCounters,
		Summary: "Decoder counters since the program started (`tracker.Counters`): frames, CRC failures, duplicates, positions, per-DF, per-antenna and per-feed totals."},
	{Method: "GET", Path: "/api/stats/range", Group: grpStats, Access: AccessAdmin, API: apiStatsRange, Cache: ttlMed, Sample: "/api/stats/range?hours=48",
		Summary: "Hourly range metrics (`range_hour`): maximum and percentile distances, positions, aircraft, far-reception count, signal residual.",
		Params:  params(pRange, pAnt, Param{"hist", "`1` adds `distHist` (120 counts of 5 km bins) and `sectorMax` (36 maxima per 10 degrees) to every hour"}),
		Notes:   "Default 7 days."},
	{Method: "GET", Path: "/api/stats/daynight", Group: grpStats, Access: AccessAdmin, API: apiDayNight, Cache: ttlLong,
		Summary: "Day / twilight / night comparison of range and signal, by local hour, with a significance test.",
		Params:  params(pRange, pAnt), Notes: "Default 30 days. Also available as `/api/antenna/daynight`."},

	// --------------------------------------------------------------- antenna
	{Method: "GET", Path: "/api/antenna/coverage", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaCoverage, Cache: ttlLong,
		Summary: "Polar coverage: maximum, robust and typical range per 2 degree bearing bin, overall and per altitude band, against the radio horizon; measured skyline and obstructions.",
		Params:  params(pRange, pAnt, pBand), Notes: "Default 30 days. With `band` only that band is returned in `bands`. All per-bearing arrays have 180 entries matching `bearingDeg`."},
	{Method: "GET", Path: "/api/antenna/coverage.geojson", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaCoverageGeoJSON, Cache: ttlLong, ContentType: "application/geo+json",
		Summary: "Coverage outlines as GeoJSON polygons for the map.",
		Params: params(pRange, pAnt, pBand, Param{"kind", "`max` or `robust` (default both)"},
			Param{"altBand", "`all` = one outline per altitude band with data (no overall), `-1` = all altitudes together only, `0`..`6` = that band only; takes precedence over `band`"}),
		Notes: "FeatureCollection of Polygon features (closed ring of `[lon, lat]`), properties `kind` (`max` | `robust`), `band` (-1 = all altitudes), `label`, `altFt`, `loFt`, `hiFt` (band limits in ft, null = open), `ant`. Without `band` / `altBand`: the overall outline plus every band. Empty collection until the receiver is located and data exists."},
	{Method: "GET", Path: "/api/antenna/horizon", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaHorizon, Cache: ttlLong,
		Summary: "Radio line-of-sight table per antenna and altitude band, and the achieved range against it.",
		Params:  params(pRange, pAnt, Param{"k", "effective earth radius factor for the tables (default 1.3333)"})},
	{Method: "GET", Path: "/api/antenna/skyline", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaSkyline, Cache: ttlLong,
		Summary: "Measured skyline (lowest elevation angle with reception per bearing) and detected obstructions.",
		Params:  params(pRange, pAnt)},
	{Method: "GET", Path: "/api/antenna/heat", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaHeat, Cache: ttlLong, Sample: "/api/antenna/heat?maxCells=2000",
		Summary: "All-time position heat grid as parallel arrays (cell centre, count, mean RSSI, lowest and highest altitude).",
		Params: params(pAnt, Param{"minCount", "drop cells with fewer positions (default 1)"},
			Param{"maxCells", "cells are merged n x n until at most this many remain, 100..60000 (default 20000)"},
			Param{"bbox", "`west,south,east,north` in degrees: only this area"}),
		Notes: "`minAlt` is the coverage floor (lowest altitude ever received in the cell, ft)."},
	{Method: "GET", Path: "/api/antenna/signal", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaSignal, Cache: ttlLong,
		Summary: "RSSI against distance: 2-D histogram cells, per-distance percentiles and the path-loss fit.",
		Params:  params(pRange, pAnt)},
	{Method: "GET", Path: "/api/antenna/azimuth", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaAzimuth, Cache: ttlLong,
		Summary: "Measured azimuth gain pattern (relative dB per 2 degree bearing) and the directional lobe fitted to it.",
		Params:  params(pRange, pAnt)},
	{Method: "GET", Path: "/api/antenna/elevation", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaElevation, Cache: ttlLong,
		Summary: "Measured elevation pattern: counts, mean RSSI and distance-corrected residual per 0.5 degree of elevation.",
		Params:  params(pRange, pAnt)},
	{Method: "GET", Path: "/api/antenna/fit", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaFit, Cache: ttlLong,
		Summary: "Yagi lobe fit: inferred boresight, beamwidth and gain over the omni level, compared with the configured yagi.",
		Params:  params(pRange, pAnt)},
	{Method: "GET", Path: "/api/antenna/linkbudget", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaLinkBudget, Cache: ttlLong,
		Summary: "Predicted (link budget) against achieved range per altitude band.",
		Params:  params(pRange, pAnt)},
	{Method: "GET", Path: "/api/antenna/pattern", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaPattern, Cache: ttlLong,
		Summary: "Configured antennas with their modelled azimuth / elevation patterns on the analysis grid, next to the measured pattern.",
		Params:  params(pRange, pAnt),
		Notes:   "`azGainDbi` / `azRelDb` have 180 entries (2 degree bearing bins, at 3 degrees elevation), `elGainDbi` / `elRelDb` 184 entries (0.5 degree bins from -2 degrees). For free-form cuts use `/api/radio/pattern`."},
	{Method: "GET", Path: "/api/antenna/score", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaScore, Cache: ttlLong,
		Summary: "Station score 0..100 with sub-scores and plain-language findings.",
		Params:  params(pRange, pAnt)},
	{Method: "GET", Path: "/api/antenna/report", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaReport, Cache: ttlLong,
		Summary: "The complete antenna analysis in one document (signal, azimuth, elevation, antennas, yagi, linkBudget, notes).",
		Params:  params(pRange, pAnt), Notes: "The endpoints `signal`, `azimuth`, `elevation`, `fit`, `linkbudget` and `pattern` are parts of this report; they share one computation."},
	{Method: "GET", Path: "/api/antenna/daynight", Group: grpAntenna, Access: AccessAdmin, API: apiDayNight, Cache: ttlLong,
		Summary: "Same as `/api/stats/daynight`.", Params: params(pRange, pAnt)},
	{Method: "GET", Path: "/api/antenna/budget", Group: grpAntenna, Access: AccessAdmin, API: apiAntennaBudget, Sample: "/api/antenna/budget?ant=vertical&km=250&altFt=38000",
		Summary: "Link budget for one aircraft position: path loss, antenna gain, received power, noise floor, SNR, margin.",
		Params: []Param{{"ant", "antenna ID (default: the first antenna)"}, {"az", "bearing from the station, degrees (default: the antenna's boresight)"},
			{"km", "ground distance (default 200)"}, {"altFt", "aircraft altitude (default 35000)"}},
		Notes: "Power levels are dBm at the antenna terminals (not dBFS). Assumes a 250 W transponder and a 6 dB receiver noise figure."},
	{Method: "GET", Path: "/api/radio/pattern", Group: grpAntenna, Access: AccessAdmin, API: apiRadioPattern, Cache: ttlMed, Sample: "/api/radio/pattern?ant=vertical&step=10",
		Summary: "Modelled antenna pattern cuts: azimuth cut at a given elevation, elevation cut at a given azimuth, predicted range around the compass.",
		Params: []Param{{"ant", "antenna ID (default: all antennas)"}, {"elev", "elevation of the azimuth cut, degrees (default 3)"},
			{"az", "azimuth of the elevation cut (default: boresight)"}, {"step", "angular step, 0.5..30 degrees (default 2)"},
			{"altFt", "altitude of the range prediction (default 35000)"}},
		Notes: "The elevation cut runs from -10 to +90 degrees."},
	{Method: "GET", Path: "/api/radio/transponders", Group: grpAntenna, Access: AccessAdmin, API: apiRadioTransponders,
		Summary: "Transponder power classes used by the link budget."},

	// --------------------------------------------------------------- weather
	{Method: "GET", Path: "/api/weather/current", Group: grpWeather, Access: AccessAdmin, API: apiWeatherCurrent, Cache: ttlShort,
		Summary: "Current conditions, poller status, the stored row of the current hour and the radio-propagation state derived from it.",
		Notes:   "`current` is null until Open-Meteo was fetched (weather disabled, receiver not located, offline). Units: degC, hPa, km/h, mm, %, m. Show `attribution`."},
	{Method: "GET", Path: "/api/weather/hourly", Group: grpWeather, Access: AccessAdmin, API: apiWeatherHourly, Cache: ttlMed, Sample: "/api/weather/hourly?hours=6&ahead=6",
		Summary: "Hourly weather rows from the database: observed hours up to now and forecast hours from the current hour on.",
		Params: []Param{{"hours", "hours back from now (default 48)"}, {"ahead", "hours ahead of now (default 72, max 384)"}, {"from, to", "explicit range in Unix ms instead"},
			{"profile", "`1` includes the pressure-level document `profile` in every row"}},
		Notes: "Row fields are omitted when unknown. `nSurface` (N-units), `dnDh` (N/km), `kFactor`, `duct` (M-units of the strongest duct able to trap 1090 MHz, 0 = none) are the refractivity columns. Forecast rows carry `issued` and `leadH`."},
	{Method: "GET", Path: "/api/weather/profile", Group: grpWeather, Access: AccessAdmin, API: apiWeatherProfile, Cache: ttlMed,
		Summary: "Pressure-level profile of the weather model for one hour with its refractivity analysis (N / M profile, layers, ducts).",
		Params:  []Param{{"t", "hour, Unix ms (default: now)"}},
		Notes:   "`levels[]`: `hpa`, `t` (degC), `rh` (%), `z` (geopotential m AMSL), `ws` (km/h), `wd` (deg). `t` = 0 and empty arrays when no weather data exists for the hour."},
	{Method: "GET", Path: "/api/weather/radar", Group: grpWeather, Access: AccessAdmin, API: apiWeatherRadar,
		Summary: "RainViewer precipitation radar frame index for the map overlay.",
		Notes:   "`past[].url` is a ready XYZ tile template (`{z}/{x}/{y}`), usable as a MapLibre raster source with `maxzoom` 7 and routable through `/tilecache/`. `available` is false until the index was fetched (needs `config.weather.radarOverlay`)."},
	{Method: "GET", Path: "/api/weather/impact", Group: grpWeather, Access: AccessAdmin, API: apiWeatherImpact, Cache: ttlLong,
		Summary: "Weather impact on reception: correlations, response curves, ridge regression and ducting episodes.",
		Params:  params(pRange, pAnt), Notes: "Default 60 days. Needs 48 h of overlapping range and weather data (`enough`); until then `conclusions` says how much is missing."},
	{Method: "GET", Path: "/api/weather/ducting", Group: grpWeather, Access: AccessAdmin, API: apiWeatherDucting, Cache: ttlMed, Sample: "/api/weather/ducting?hours=6&ahead=6",
		Summary: "Refractivity / ducting time line: per hour the gradient, k-factor, 0..100 propagation index, class and ducts; plus the inversions aircraft measure right now.",
		Params:  []Param{{"hours", "hours back (default 48)"}, {"ahead", "hours ahead (default 72)"}, {"from, to", "explicit range in Unix ms instead"}},
		Notes:   "`index`: 50 = standard atmosphere, 85+ = ducting likely. `class`: `sub-refraction` | `standard` | `super-refraction` | `ducting`."},
	{Method: "POST", Path: "/api/weather/refresh", Group: grpWeather, Access: AccessAdmin, API: apiWeatherRefresh, Admin: true, NoAuto: true,
		Summary: "Fetch the weather now instead of waiting for the next poll.",
		Returns: "`{\"ok\":true,\"fetched\":<ms>,\"status\":{...}}`; `409` when weather is disabled or the receiver is not located, `502` when the download failed."},

	// ------------------------------------------------------------------- met
	{Method: "GET", Path: "/api/met/profile", Group: grpMet, Access: AccessAdmin, API: apiMetProfile, Cache: ttlShort,
		Summary: "Vertical profile of wind and temperature above the station derived from aircraft (Mode S EHS / MRAR), in 1000 ft layers, with its analysis.",
		Params:  []Param{{"t", "Unix ms: the analysed profile of that clock hour instead of the current one"}},
		Notes:   "Units: kt, ft (pressure altitude), degrees true (direction the wind blows FROM), degC. Optional values are omitted when unknown."},
	{Method: "GET", Path: "/api/met/history", Group: grpMet, Access: AccessAdmin, API: apiMetHistory, Cache: ttlMed,
		Summary: "Hourly time-height history of the aircraft-derived profiles.",
		Params:  pRange, Notes: "Default 24 h. `cells` are ordered by hour, then level."},
	{Method: "GET", Path: "/api/met/observations", Group: grpMet, Access: AccessAdmin, API: apiMetObservations, Cache: ttlLive, Sample: "/api/met/observations?limit=5",
		Summary: "Recent single wind / temperature observations for a map layer.",
		Params: []Param{{"maxAge", "seconds, 10..7200 (default 900)"}, {"limit", "1..5000 (default 1000)"},
			{"perAircraft", "`1` (default) = only the newest observation per aircraft, `0` = all"}}},
	{Method: "GET", Path: "/api/met/winds", Group: grpMet, Access: AccessAdmin, API: apiMetWinds, Cache: ttlShort,
		Summary: "Winds-aloft table at standard flight levels, jet maximum, strongest shear, freezing level and tropopause from the current profile."},
	{Method: "GET", Path: "/api/met/compare", Group: grpMet, Access: AccessAdmin, API: apiMetCompare, Cache: ttlMed,
		Summary: "Aircraft-derived profile against the weather model (differences are aircraft minus model) and the radio refractivity of measured inversions."},
	{Method: "GET", Path: "/api/met/stats", Group: grpMet, Access: AccessAdmin, API: apiMetStats,
		Summary: "Collector counters and the learned per-airframe heading corrections.",
		Params:  []Param{{"limit", "heading corrections returned, 1..500 (default 50)"}}},

	// -------------------------------------------------------------- forecast
	{Method: "GET", Path: "/api/forecast/traffic", Group: grpForecast, Access: AccessAdmin, API: apiForecastTraffic, Cache: ttlMed,
		Summary: "Traffic forecast for the next 72 h: aircraft count and message rate with 80 % intervals, busiest and quietest hours."},
	{Method: "GET", Path: "/api/forecast/reception", Group: grpForecast, Access: AccessAdmin, API: apiForecastReception, Cache: ttlMed,
		Summary: "Reception forecast for the next 72 h: p95 / max range, 0..100 index, far-reception probability, atmosphere and drivers per hour."},
	{Method: "GET", Path: "/api/forecast/weather", Group: grpForecast, Access: AccessAdmin, API: apiForecastWeather, Cache: ttlMed,
		Summary: "Local weather outlook: bias-corrected hourly forecast, Zambretti, detected patterns (fronts, fog, inversion, convection), analogue forecast."},
	{Method: "GET", Path: "/api/forecast/outlook", Group: grpForecast, Access: AccessAdmin, API: apiForecastOutlook, Cache: ttlMed,
		Summary: "Digest of all forecast products for a home page: nowcast, the next 12 h of each product, highlights."},
	{Method: "GET", Path: "/api/forecast/verification", Group: grpForecast, Access: AccessAdmin, API: apiForecastVerification, Cache: ttlMed,
		Summary: "How good the forecasts are: backtest skill per model, forecast-versus-actual series, what data is still missing."},
	{Method: "GET", Path: "/api/forecast/nowcast", Group: grpForecast, Access: AccessAdmin, API: apiForecastNowcast, Cache: ttlShort,
		Summary: "One-line reception nowcast for a top bar: index, label, trend, sentence."},
	{Method: "GET", Path: "/api/forecast/status", Group: grpForecast, Access: AccessAdmin, API: apiForecastStatus,
		Summary: "State of the forecaster (last fit, last build, hours of history, last error)."},
	{Method: "POST", Path: "/api/forecast/refit", Group: grpForecast, Access: AccessAdmin, API: apiForecastRefit, Admin: true, NoAuto: true,
		Summary: "Refit all forecast models now (normally hourly).", Returns: "`{\"ok\":true,\"status\":{...}}`"},

	// ---------------------------------------------------------------- timing
	{Method: "GET", Path: "/api/timing/ntp", Group: grpTiming, Access: AccessAdmin, API: apiTimingNTP,
		Summary: "NTP reference: configuration, status, newest sample and statistics (last hour, 24 h, everything in memory).",
		Notes:   "Offsets are true UTC minus host clock, in ms. `driftPpm` is null with fewer than 4 samples."},
	{Method: "GET", Path: "/api/timing/history", Group: grpTiming, Access: AccessAdmin, API: apiTimingHistory, Cache: ttlShort,
		Summary: "NTP offset / delay history.",
		Params:  params(pRange, Param{"limit", "maximum number of samples, evenly thinned, 10..20000 (default 3000)"}),
		Notes:   "Default 24 h. Ranges older than what is in memory (about 6 days) come from the database with fewer fields filled."},
	{Method: "GET", Path: "/api/timing/latency", Group: grpTiming, Access: AccessAdmin, API: apiTimingLatency,
		Summary: "Per-feed latency between the receiver's GPS frame timestamps and the (NTP-corrected) host clock: percentiles, histogram, per-minute history; plus the latency-chain summary and stage histograms.",
		Notes: "Only feeds with GPS timestamps (Radarcape in GPS mode, the demo feed) appear in `feeds`. " +
			"`chain` (`latency.Summary`, null when unavailable): `now, windowSec, receiverSet, clockSynced, clockOffsetMs, aircraft, aircraftT1, shareT1, msgShareT1, stages[], counts{}, feeds[], findings[]`; " +
			"`stages[]`: `{key: propagation|receiver|processing|transponder|ageDelta|airToSystem|endToEnd, label, description, count, minMs, p50Ms, p95Ms, p99Ms, maxMs, meanMs, uncertaintyMs, exact}`; " +
			"`feeds[]`: `{feedId, samples, negative, absurd, lastMs, floorMs, jitterMs, driftUsPerMin, intervals, status: ok|warn|bad|unknown, issues[]}`; `findings[]`: `{level: info|warn|bad, key, text}`. " +
			"`histograms[]`: `{key, label, unit:\"ms\", scale:\"log\"|\"linear\", count, bins:[{lo, hi, n}]}` (log bins are signed; extra keys `aircraftTransponder`, `aircraftAgeJitter`). " +
			"Every number of `chain` / `histograms` is null instead of NaN. The station position used for propagation is `station` of `/api/status` (configured receiver, else the receiver's GPS position)."},
	{Method: "GET", Path: "/api/timing/latency/aircraft", Group: grpTiming, Access: AccessAdmin, API: apiTimingLatencyAircraft,
		Summary: "Per-aircraft latency table (aircraft with positions, most positions first): transponder latency of T=1 positions, estimated position age, age jitter, propagation, flags.",
		Notes: "`{aircraft: [AircraftRow]}`; `AircraftRow`: `icao, firstSeen, lastSeen, n, positions, t1, t0, tFlag, lastTc, xpdrN, xpdrMeanMs, xpdrStdMs, xpdrMinMs, xpdrMaxMs, agePairs, ageJitterMs, ageModelMs, ageCheckMs, positionAgeMs, ageExact, propMeanUs, propLastUs, slantKm, flags[]`. " +
			"Flags: `t1-not-synchronised`, `t1-out-of-tolerance`, `t1-outliers`, `t1-unexpected-tc`, `age-over-0.6s-est`, `age-jitter-high`, `stale-positions`. Unknown numbers are null."},
	{Method: "GET", Path: "/api/timing/latency/aircraft/{icao}", Group: grpTiming, Access: AccessAdmin, API: apiTimingLatencyAircraftOne, Sample: "/api/timing/latency/aircraft/{live}",
		Summary: "One aircraft's latency row and the stage waterfall of its latest position message.",
		Notes: "`{row: AircraftRow, waterfall: Waterfall}`. `Waterfall`: `icao, t, df, tc, tFlag, cprOdd, feed, exact, slantKm, elevDeg, events[], stages[], totalMs, totalUncMs, notes[]`; " +
			"`events[]`: `{key: applicability|transmit|arrival|received|handled, label, utc (RFC 3339 ns), atMs (relative to applicability), uncMs}`; " +
			"`stages[]`: `{key: positionAge|propagation|receiver|processing, label, ns, ms, uncMs, method: exact|estimated|assumed|measured|gated|computed|unavailable}`. " +
			"`{icao}` is 6 hex digits. `400` for an invalid address, `404 {\"error\":\"unknown aircraft\"}` when the aircraft is not in the latency table."},
	{Method: "GET", Path: "/api/timing/latency/feeds/{id}", Group: grpTiming, Access: AccessAdmin, API: apiTimingLatencyFeed, Sample: "/api/timing/latency/feeds/demo",
		Summary: "Receiver-delay minute series of one feed (up to 24 h, restored from the database after a restart).",
		Params:  []Param{{"since", "only minutes at or after this time (Unix ms; default all)"}},
		Notes: "`{feed, feeds: [every tracked feed id], series: [FeedMinute]}`, oldest first; `FeedMinute`: `{t (Unix ms), n, minMs, p50Ms, p95Ms, maxMs, meanMs, floorMs, jitterMs, neg, absurd}` (null = unknown). " +
			"`404 {\"error\":\"unknown feed\"}` for a feed without latency data."},
	{Method: "GET", Path: "/api/timing/ntpcheck", Group: grpTiming, Access: AccessAdmin, API: apiTimingNTPCheck,
		Summary: "NTP / receiver GPS clock consistency: NTP server quality, offset uncertainty, per-feed receiver-delay clock filter (10 s minima, floor, jitter, drift), issues and an explanation.",
		Notes: "`{ntp: {enabled, synced, server, stratum, refId, leap, offsetMs, delayMs, jitterMs, rootDelayMs, rootDispMs, lastOk, lastError}, quality: good|degraded|bad|unknown, offsetUncertaintyMs, issues[], " +
			"feeds: [{feedId, samples, negative, absurd, lastMs, floorMs, jitterMs, driftUsPerMin, intervals, status: ok|warn|bad|unknown, issues[], minima: [{t, ms}]}], explanation}`. " +
			"The NTP reference is exactly `config.ntp.server` (the same sampler corrects the host clock for every latency statistic)."},
	{Method: "GET", Path: "/api/timing/receiver", Group: grpTiming, Access: AccessAdmin, API: apiTimingReceiver,
		Summary: "Receiver timing state: GPS-timestamp mode and status text per feed, receiver GPS position, stored status frames.",
		Params:  []Param{{"limit", "stored status rows, 1..500 (default 20)"}}},
	{Method: "POST", Path: "/api/timing/ntp/sample", Group: grpTiming, Access: AccessAdmin, API: apiTimingSample, Admin: true, NoAuto: true,
		Summary: "Take an NTP measurement now with the configured server, or test another server once without saving it.",
		Params:  []Param{{"server", "optional `host`, `host:port`, IPv4 or IPv6 literal (`[v6]:port`): query this server once instead of `config.ntp.server`"}},
		Body:    "Optional `{\"server\":\"host[:port]\"}` (same as `?server=`). Without a server the configured one is sampled.",
		Returns: "Without a server: `ntp.Sample` of the configured server; `502` when it did not answer. " +
			"With a server: one query (timeout 3 s), nothing is stored or applied: `{ok, server (normalised host:port), addr?, t, stratum, refId, leap, offsetMs, delayMs, rootDelayMs, rootDispMs, error?, saved:false}` " +
			"(always `200`; `ok:false` with `error` and null numbers when the server did not answer); `400` for an invalid host.",
		Notes: "To use the server, save it as `config.ntp.server` (with `ntp.enabled`) through `PUT /api/config`: the sampler and the latency clock switch to it at once."},

	// ----------------------------------------------------------------- guess
	{Method: "GET", Path: "/api/guess", Group: grpGuess, Access: AccessAdmin, API: apiGuess, Cache: ttlMed,
		Summary: "Receiver location estimated from the received positions alone (four methods and their fusion). Cached result.",
		Params:  params(pAnt), Notes: "The configured position is never an input; `errorKm` compares against it when it is set. `surface` is a 33 x 33 cost grid (row-major from the south-west corner) for a heat-map display."},
	{Method: "POST", Path: "/api/guess", Group: grpGuess, Access: AccessAdmin, API: apiGuessRun, Admin: true, NoAuto: true,
		Summary: "Run the location guesser now on the newest data (flushes the statistics first) and return the result.",
		Params:  params(pAnt), Returns: "Same document as `GET /api/guess`."},

	// --------------------------------------------------------------- history
	{Method: "GET", Path: "/api/history/aircraft", Group: grpHistory, Access: AccessAdmin, API: apiHistoryAircraft, Sample: "/api/history/aircraft?limit=3&sort=msgs",
		Summary: "Every airframe ever seen: search, sort, paging.",
		Params: []Param{{"q", "case-insensitive substring of ICAO address, registration, callsign, type, type description or operator"},
			{"sort", "`last_seen` (default) | `first_seen` | `icao` | `callsign` | `reg` | `type` | `operator` | `country` | `msgs` | `flights` | `positions` | `max_dist_km` | `max_alt`"},
			{"desc", "`1` / `0` (default: descending for times and numbers, ascending for text)"},
			{"limit", "1..1000 (default 100)"}, {"offset", "rows to skip"},
			{"since, until", "restrict `lastSeen` (Unix ms)"}}},
	{Method: "GET", Path: "/api/history/aircraft/{icao}", Group: grpHistory, Access: AccessAdmin, API: apiHistoryAircraftOne, Sample: "/api/history/aircraft/{seen}",
		Summary: "One airframe: totals, reference data, its flights (newest first) and its events.",
		Params:  []Param{{"limit", "flights returned, 1..1000 (default 100)"}},
		Notes:   "Same document as `/api/aircraft/{icao}` plus `events`. `404` when the address was never seen."},
	{Method: "GET", Path: "/api/history/flights", Group: grpHistory, Access: AccessAdmin, API: apiHistoryFlights, Sample: "/api/history/flights?limit=3",
		Summary: "Flights (contact sessions), newest first.",
		Params:  params(Param{"limit", "1..1000 (default 100)"}, Param{"offset", "rows to skip"}, Param{"icao", "only this aircraft"}, pRange),
		Notes:   "With a time range: flights that overlap it. A flight is one contact session; a gap of more than 30 min starts a new one."},
	{Method: "GET", Path: "/api/history/flights/{id}", Group: grpHistory, Access: AccessAdmin, API: apiHistoryFlight, Sample: "/api/history/flights/{flight}",
		Summary: "One flight with its stored trail.",
		Notes:   "`trail` points are `[tMs, lat, lon, altFt|null, gsKt|null, rssi|null, src]`, one every `config.storage.trailIntervalSec` seconds. `airports` holds the guessed departure / arrival airports (see `/api/history/flights/{id}/trace`). `404` for an unknown id."},

	// -------------------------------------------------------------------- db
	{Method: "GET", Path: "/api/db/stats", Group: grpDB, Access: AccessAdmin, API: apiDBStats,
		Summary: "Database size, writer counters, row counts per table, retention settings, aircraft database and tile cache state."},
	{Method: "POST", Path: "/api/db/vacuum", Group: grpDB, Access: AccessAdmin, API: apiDBVacuum, Admin: true, NoAuto: true,
		Summary: "Compact the database file (blocks writes while it runs; can take minutes on a Raspberry Pi).", Returns: "`{ok, elapsedMs, beforeBytes, afterBytes}`"},
	{Method: "POST", Path: "/api/db/prune", Group: grpDB, Access: AccessAdmin, API: apiDBPrune, Admin: true, NoAuto: true,
		Summary: "Apply the retention settings now.", Returns: "`{ok, elapsedMs, beforeBytes, afterBytes, total, pruned:{flights, positions, minuteStats, metObs, ntpSamples, receiverStatus, freedPages}}`"},
	{Method: "POST", Path: "/api/db/checkpoint", Group: grpDB, Access: AccessAdmin, API: apiDBCheckpoint, Admin: true, NoAuto: true,
		Summary: "Flush the in-memory statistics and checkpoint the write-ahead log.", Returns: "`{ok, elapsedMs, beforeBytes, afterBytes}`"},
	{Method: "POST", Path: "/api/db/rollup", Group: grpDB, Access: AccessAdmin, API: apiDBRollup, Admin: true, NoAuto: true,
		Summary: "Roll minute statistics up into hours now (normally 3 min after each hour).", Returns: "`{ok, elapsedMs, beforeBytes, afterBytes, hours}`"},
	{Method: "GET", Path: "/api/db/acdb/sources", Group: grpDB, Access: AccessAdmin, API: apiACDBSources,
		Summary: "Downloadable aircraft databases (with licence and attribution), what is installed, and the import state."},
	{Method: "POST", Path: "/api/db/acdb/import", Group: grpDB, Access: AccessAdmin, API: apiACDBImport, Admin: true, NoAuto: true,
		Summary: "Start downloading and importing an aircraft database in the background.",
		Body:    "`{\"source\":\"tar1090-db\"|\"opensky\", \"url\":\"optional mirror\"}` (empty body = tar1090-db)",
		Returns: "`engine.ImportStatus` (see `/api/db/acdb/progress`); `409` when an import is already running.",
		Notes:   "Poll `/api/db/acdb/progress`; the outcome also arrives as a `notice` event. A failed download leaves the installed database untouched."},
	{Method: "POST", Path: "/api/db/acdb/upload", Group: grpDB, Access: AccessAdmin, Raw: rawACDBUpload, Admin: true, NoAuto: true, LongBody: true,
		Summary: "Import an aircraft database file sent as the raw request body (tar1090-db or OpenSky CSV, optionally gzip; at most 600 MB).",
		Params:  []Param{{"name", "original file name (informational)"}},
		Returns: "`engine.ImportStatus` when the import finished (the request blocks until then)."},
	{Method: "POST", Path: "/api/db/acdb/cancel", Group: grpDB, Access: AccessAdmin, API: apiACDBCancel, Admin: true, NoAuto: true,
		Summary: "Cancel a running import.", Returns: "`engine.ImportStatus`"},
	{Method: "GET", Path: "/api/db/acdb/progress", Group: grpDB, Access: AccessAdmin, API: apiACDBProgress,
		Summary: "State of the current or last aircraft database import.",
		Notes:   "`stage`: `\"\"` (never ran) | `download` | `store` | `done` | `error` | `cancelled`. `progress.bytesTotal` is -1 when unknown."},
	{Method: "GET", Path: "/api/db/acdb/lookup", Group: grpDB, Access: AccessAdmin, API: apiACDBLookup, Sample: "/api/db/acdb/lookup?icao=3C6444",
		Summary: "Look one address up in the aircraft database and the built-in tables.",
		Params:  []Param{{"icao", "6 hex digits (required)"}}},
	{Method: "GET", Path: "/api/db/export", Group: grpDB, Access: AccessAdmin, API: apiDBExportList,
		Summary: "Tables available for CSV export with their row counts."},
	{Method: "GET", Path: "/api/db/export/{file}", Group: grpDB, Access: AccessAdmin, Raw: rawDBExport, Sample: "/api/db/export/records.csv",
		Summary: "Download one table as CSV (`{file}` = `<table>.csv`), streamed.",
		Params:  params(pRange, Param{"limit", "maximum number of rows"}),
		Returns: "`text/csv` with a header row; database columns as stored (times in Unix seconds, `day` columns in days since 1970, BLOBs as hex, NULL as empty field).",
		Notes:   "The time range filters on the table's time column (see `/api/db/export`); tables without one ignore it. At most two exports run at a time (`429` beyond)."},

	// ------------------------------------------------------------------- geo
	{Method: "GET", Path: "/api/geo/parse", Group: grpGeo, Access: AccessPublic, API: apiGeoParse, Sample: "/api/geo/parse?q=50%C2%B002%2700.0%22N+8%C2%B034%2713.4%22E",
		Summary: "Parse pasted coordinates in almost any notation into latitude / longitude.",
		Params:  []Param{{"q", "the text: decimal degrees, DMS, degrees + decimal minutes, Maidenhead locator, Plus Code, `geo:` URI, Google / Bing / OSM map URL ..."}},
		Notes:   "Always `200`: `ok:false` with a user-readable `error` when the text is not understood (the box is still being typed into). `lat` / `lon` are null then."},
	{Method: "GET", Path: "/api/geo/format", Group: grpGeo, Access: AccessPublic, API: apiGeoFormat, Sample: "/api/geo/format?lat=50.05&lon=8.6",
		Summary: "Format a position in the common notations.", Params: []Param{{"lat, lon", "degrees (required)"}}},
	{Method: "GET", Path: "/api/geo/elevation", Group: grpGeo, Access: AccessAdmin, API: apiGeoElevation, Cache: 24 * time.Hour, Sample: "/api/geo/elevation?lat=50.05&lon=8.6",
		Summary: "Terrain elevation at a position (Open-Meteo elevation service; needs the internet).",
		Params:  []Param{{"lat, lon", "degrees (required)"}}, Notes: "`502` when the look-up fails. Results are cached for a day."},
	{Method: "GET", Path: "/api/geo/magvar", Group: grpGeo, Access: AccessPublic, API: apiGeoMagVar, Sample: "/api/geo/magvar?lat=50.05&lon=8.6",
		Summary: "Magnetic declination and field (World Magnetic Model 2025) at a position, now.",
		Params:  []Param{{"lat, lon", "degrees (required)"}, {"altKm", "height above the ellipsoid (default 0)"}}},
	{Method: "GET", Path: "/api/astro/now", Group: grpGeo, Access: AccessAdmin, API: apiAstroNow, Cache: ttlShort,
		Summary: "Sun position, day phase and today's sun events at the receiver, the day/night terminator rings and the moon phase.",
		Params:  []Param{{"t", "Unix ms (default now)"}},
		Notes:   "`nightSide.sunset|civil|nautical|astronomical` are closed rings of `[lon, lat]` enclosing the area where the sun is below -0.833 / -6 / -12 / -18 degrees. `position`, `phase` and `events` are absent until the receiver is located. Event times are Unix ms, 0 = does not occur."},
	{Method: "GET", Path: "/api/astro/terminator.geojson", Group: grpGeo, Access: AccessAdmin, API: apiAstroTerminator, Cache: ttlMed, ContentType: "application/geo+json", Sample: "/api/astro/terminator.geojson?step=10",
		Summary: "The night side of the earth as a GeoJSON FeatureCollection of four polygons (properties `kind`, `sunAlt`).",
		Params:  []Param{{"t", "Unix ms (default now)"}, {"step", "ring resolution, 0.5..10 degrees (default 2)"}}},
	{Method: "GET", Path: "/api/astro/days", Group: grpGeo, Access: AccessAdmin, API: apiAstroDays, Sample: "/api/astro/days?days=2",
		Summary: "Sunrise, sunset and twilight times for the coming days.",
		Params:  []Param{{"days", "1..366 (default 7)"}, {"lat, lon", "another position instead of the receiver"}}},
	{Method: "GET", Path: "/api/refdata/icao/{icao}", Group: grpGeo, Access: AccessPublic, API: apiRefICAO, Sample: "/api/refdata/icao/3C6444",
		Summary: "What the built-in tables know about an ICAO address: state of registry, allocation block, derived registration, military heuristic."},
	{Method: "GET", Path: "/api/refdata/callsign/{callsign}", Group: grpGeo, Access: AccessPublic, API: apiRefCallsign, Sample: "/api/refdata/callsign/DLH4AB",
		Summary: "Interpret a callsign: operator, telephony name, flight number or registration."},
	{Method: "GET", Path: "/api/refdata/type/{code}", Group: grpGeo, Access: AccessPublic, API: apiRefType, Sample: "/api/refdata/type/A20N",
		Summary: "ICAO aircraft type designator: manufacturer, model, class, wake category."},
	{Method: "GET", Path: "/api/refdata/squawk/{code}", Group: grpGeo, Access: AccessPublic, API: apiRefSquawk, Sample: "/api/refdata/squawk/7700",
		Summary: "Conventional meaning of a Mode A code."},

	// ---------------------------------------------------------------- system
	{Method: "GET", Path: "/api/system", Group: grpSystem, Access: AccessAdmin, API: apiSystem,
		Summary: "Process and host information: version, platform, uptime, goroutines, memory, data directory, restart-required flag."},
}
