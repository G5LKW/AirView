// Package store is AirView's SQLite persistence layer (modernc.org/sqlite,
// pure Go): one database file "airview.db" in the data directory, opened in
// WAL mode with exactly one writing connection and a pool of read-only
// connections.
//
// # Design
//
//   - Every write runs on ONE goroutine (the writer). Callers either queue
//     typed row operations that are flushed together in one transaction every
//     group-commit interval (Submit and the Store.Add*/Merge*/Upsert*
//     shortcuts - they never block on the disk), or run a synchronous
//     transaction on the writer (Write, Apply, KVSet, ImportACDB, RollupHours,
//     Prune ...). The interval comes from the storage Profile (profile.go):
//     10 s "standard", 30 s "low-wear" (Raspberry Pi on an SD card), 5 s
//     "server" (aggregator); 1.0.0 used 2 s everywhere.
//   - Checkpoints never run inside a commit (checkpointer.go): the writer's
//     wal_autocheckpoint is only a hard cap; a checkpointer goroutine runs a
//     PASSIVE checkpoint on its own connection in the writer's idle slot when
//     the WAL passes the profile's soft size or interval, and the hourly job
//     (HourlyMaintenance) truncates the WAL. Bulk work (data-migration steps,
//     the incremental-vacuum steps of Prune / Maintain) checkpoints between
//     its transactions as soon as the WAL holds the soft size, so the WAL
//     stays below the 64 MiB hard cap on stations.
//   - Data whose layout changes between releases is converted in the
//     background by registered data migrations (datamig.go): small resumable
//     transactions with their progress in the same transaction, a free-space
//     check and an optional pre-migration backup (premigrate.go). 1.0.0
//     tables whose layout changed keep their name for exports and the parity
//     harness through the logical-table registry (logical.go).
//   - The queue is bounded (Options.MaxQueueRows). If the disk cannot keep up,
//     the oldest low-value rows (histogram / heat-map increments, raw met
//     observations) are dropped first, then trail positions; Stats reports
//     the drop counters.
//   - A flush that fails because of the database itself (disk full, I/O
//     error) loses nothing: the rows stay queued and are retried with the
//     next timer tick. An operation that fails because of its own data is
//     dropped alone (and counted in Stats.FailedOps); the rest of the batch
//     is written.
//   - Reads go through DB(), a read-only (query_only) pool that other packages
//     use for their own SELECTs. Readers never block the writer and vice
//     versa (WAL). Queued rows become visible after the next flush.
//   - Other packages may keep tables of their own in the same file: they
//     register DDL with RegisterMigration and write through Write.
//
// # Usage
//
//	st, err := store.Open(dataDir)
//	defer st.Close()
//
//	// Tracker: accumulate in memory, flush periodically.
//	b := store.NewBatch()
//	b.UpsertAircraft(seen)        // []store.AircraftSeen (deltas)
//	b.UpsertFlights(changed)      // []store.Flight, IDs from st.NextFlightID()
//	b.AddPositions(trail)         // []store.Position
//	b.AddMinuteStats(minuteRows)  // []store.StatsRow
//	b.MergeCoverage(cov)          // increments, merged with UPSERT
//	b.MergeHeat(heat)
//	st.Submit(b)                  // queued; b is empty again and reusable
//
//	// Housekeeping (engine): hourly, then retention once a day.
//	st.HourlyMaintenance(ctx) // RollupHours, hourly jobs, optimize, TRUNCATE checkpoint
//	st.Prune(ctx, cfg.Storage)
//
//	// Server.
//	row, ok, err := st.AircraftByICAO(ctx, 0x4CA2D1)
//	rows, total, err := st.ListAircraft(ctx, store.AircraftQuery{Search: "ryr", Desc: true, Limit: 50})
//	flights, err := st.FlightsForAircraft(ctx, 0x4CA2D1, 20, 0)
//	trail, err := st.Trail(ctx, flights[0].ID)       // []model.TrailPoint
//	hours, err := st.RangeHours(ctx, "yagi", from, to) // BLOBs decoded
//	err = st.DB().QueryRowContext(ctx, `SELECT MAX(max_km) FROM range_hour WHERE ant = ?`, "yagi").Scan(&km)
//
//	// Another package with tables of its own.
//	func init() { store.RegisterMigration("predict", 1, `CREATE TABLE predict_model(...)`) }
//	err = st.Write(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, `INSERT INTO predict_model ...`); return err })
//
// Row types use Unix seconds and NaN for "unknown" (stored as NULL); the
// types returned by the read API implement json.Marshaler and encode to the
// HTTP API conventions (Unix milliseconds, hex ICAO, unknown values omitted).
//
// # Schema reference
//
// Conventions: times are Unix SECONDS (UTC) unless noted; "day" is the Unix
// day number (t/86400, see DayOf); "ant" is the antenna ID, the empty string
// meaning "all antennas combined"; icao is the 24-bit address as an INTEGER;
// text columns are NOT NULL and use the empty string for "unknown"; nullable
// numeric columns use NULL for "unknown". Altitude ft, speed kt, distance km, signal dBFS.
// The bin encodings (alt_band, brg_bin, dist_bin, rssi_bin, elev_bin, cell,
// level) are defined by the functions in bins.go - use them, do not
// re-derive them.
//
//	schema_migrations(name TEXT, version INT, applied INT, PK(name, version))
//	    applied migrations ("core" = this package).
//
//	kv(key TEXT PK, value TEXT, updated INT)
//	    misc state. Keys starting with "store." are reserved for this package.
//
//	aircraft(icao INT PK, first_seen, last_seen, callsign, reg, type, operator,
//	         country, category, msgs, flights, positions, max_dist_km REAL,
//	         max_alt INT NULL, src_mask INT, flags INT)
//	    every airframe ever seen. msgs / flights / positions are lifetime
//	    totals; max_dist_km is 0 when never positioned; src_mask = OR of Mask*
//	    bits; flags = OR of Flag* bits. Index: aircraft_last_seen(last_seen).
//	    Since 1.1 the tracker writes a new airframe at its next 15-s row
//	    check but refreshes the rows of active airframes only every
//	    storage profile AircraftRefresh (300 / 900 s); readers of these rows
//	    (aircraft list / detail / export, overview totals) first call
//	    tracker.FlushAircraftRows, which writes rows older than 60 s.
//
//	flights(id INT PK, icao, callsign, squawk, start, "end", msgs, positions,
//	        src_mask, max_dist_km, max_dist_brg, min_dist_km, min_alt, max_alt,
//	        max_gs, rssi_min, rssi_max, rssi_avg, first_lat, first_lon,
//	        last_lat, last_lon, emergency)
//	    one row per contact session (a gap > 30 min starts a new one). "end"
//	    is a keyword: quote it. Distance / altitude / speed / rssi / lat / lon
//	    columns are NULL when the flight had no position (or no such value).
//	    Indexes: flights_icao(icao, start), flights_start(start), flights_end("end");
//	    since 1.1 (migration flights 2) partial flights_dep / flights_arr
//	    (WHERE dep / arr IS NOT NULL), no flights_unresolved; events_icao(icao, id).
//
//	positions(flight_id, t, lat_e5 INT, lon_e5 INT, alt INT NULL, gs INT NULL,
//	          track INT NULL, rssi_x10 INT NULL, src INT,
//	          PK(flight_id, t)) WITHOUT ROWID
//	    down-sampled trail points. lat/lon = value/1e5 degrees, rssi =
//	    rssi_x10/10 dBFS, src = Src* code. There is NO index on t: select by
//	    flight_id (join flights on a time range, see Store.ForEachPosition).
//	    Since 1.1 this is the LOGICAL trail table: new points go to
//	    trail_tail (one varint-coded row per flight and tracker flush) and
//	    are sealed into trail_chunk(id, flight_id, t0, t1, n, data BLOB,
//	    UNIQUE(flight_id, t0)) - delta/varint (+deflate) columns, lossless
//	    (trail_codec.go). Data migration trail 1 moves the 1.0.0 rows into
//	    chunks and leaves the physical positions table empty. Read trails
//	    with Positions / Trail / ForEachPosition / ForEachTrailRow, never
//	    with SQL on positions; exports read it through the logical table.
//
//	stats_minute(t, ant, frames, df0, df4, df5, df11, df16, df17, df18, df20,
//	             df21, df_other, crc_bad, positions, mlat_positions,
//	             aircraft, aircraft_pos, adsb, mlat, modes,
//	             max_km REAL NULL, rssi_avg REAL NULL, rssi_min REAL NULL,
//	             rssi_max REAL NULL, secs REAL NULL, PK(t, ant)) WITHOUT ROWID
//	    per-minute receiver statistics; t = start of the minute. frames ..
//	    mlat_positions are counters, aircraft to modes are gauges (distinct
//	    aircraft in the minute). secs (core migration 3) = seconds of the
//	    minute that were covered (< 60 for the first minute after a start or
//	    reset and the last before a stop; NULL = unknown, a full minute).
//	    Kept for Storage.MinuteRetentionDays.
//
//	stats_hour(... same columns ..., aircraft_avg REAL, minutes INT, secs REAL,
//	           PK(t, ant)) WITHOUT ROWID
//	    hourly rollup of stats_minute (RollupHours), kept forever. Counters
//	    are sums, gauges and max_km / rssi_max maxima, rssi_min the minimum,
//	    rssi_avg the frame-weighted mean, aircraft_avg the mean of the
//	    per-minute aircraft gauge, minutes the number of minute rows (60 =
//	    receiver up the whole hour), secs the sum of the minutes' covered
//	    seconds (NULL minutes count 60). Daily figures: GROUP BY t/86400.
//
//	range_hour(t, ant, max_km, p99_km, p95_km, p90_km, p50_km, positions,
//	           aircraft, far_count, resid_db, resid_n, dist_hist BLOB,
//	           sector_max BLOB, PK(t, ant))
//	    hourly range metrics. dist_hist = 120 little-endian uint32 counts of
//	    5 km distance bins (DecodeDistHist); sector_max = 36 little-endian
//	    float32 max km per 10 degrees of bearing (DecodeSectorMax); resid_db =
//	    mean of rssi + 20*log10(slant_km) over resid_n samples. Since 1.1
//	    (migration range 2) WITHOUT ROWID with a compact dist_hist on
//	    airview.db too: read BLOBs only through the Decode* functions.
//
//	coverage(day, ant, brg_bin, alt_band, max_km, count, rssi_sum, rssi_n,
//	         PK(day, ant, brg_bin, alt_band)) WITHOUT ROWID
//	    polar coverage. brg_bin = 2 degree bins 0..179, alt_band 0..6
//	    (<1000, 1-5k, 5-10k, 10-20k, 20-30k, 30-40k, >=40k ft). Mean rssi =
//	    rssi_sum / rssi_n.
//
//	signal_hist(day, ant, dist_bin, rssi_bin, count,
//	            PK(day, ant, dist_bin, rssi_bin)) WITHOUT ROWID
//	    RSSI vs distance. dist_bin = 10 km bins 0..99, rssi_bin = floor(dBFS)
//	    in -60..-1 (RSSIBin; the tracker writes geo.RSSIBinStored of its
//	    in-memory geo.RSSIBin index, the same value). Databases written
//	    before this convention was pinned down hold the same encoding
//	    (the tracker always stored -(index+1)); only levels at exact whole
//	    dB moved by one bin, so nothing is migrated.
//
//	elev_hist(day, ant, elev_bin, count, rssi_sum, resid_sum,
//	          PK(day, ant, elev_bin)) WITHOUT ROWID
//	    elevation-angle pattern. elev_bin = 0.5 degree bins from -2 degrees
//	    (0..183); means = rssi_sum / count, resid_sum / count.
//
//	heat(cell, ant, count, rssi_sum, rssi_n, min_alt INT NULL, max_alt INT NULL,
//	     last_t, PK(cell, ant)) WITHOUT ROWID
//	    all-time position grid, 0.05 degree cells; cell = latIdx<<16 | lonIdx
//	    (HeatCell / HeatCellCenter). Mean rssi = rssi_sum / rssi_n.
//	    There is deliberately no day dimension: a per-day copy would
//	    multiply the table (tens of thousands of active cells times every
//	    day kept) and the SD-card writes of every flush, for a view nobody
//	    reads yet. A time-windowed heat map ("last 7 / 30 days") is
//	    computed on demand from positions + flights (flights_end narrows
//	    the window), within the positions retention. Since 1.1 (migration
//	    heat 2) airview.db keys it (tile, cell, ant) with tile = a 4 x 4
//	    block of cells (heatTileSQL), so a track touches fewer pages; while
//	    the migration runs the 1.0.0 rows wait in heat_v1 and readers merge
//	    them (the table count stays logical); the export keeps the 1.0.0
//	    columns (logical table). Closed days of coverage / signal_hist /
//	    elev_hist may be sealed into hist_day blobs (hist_seal.go); their
//	    exports are logical tables too.
//
//	weather_obs(t INT PK, temp_c, dewpoint_c, rh, pressure_msl,
//	            surface_pressure, wind_kmh, wind_dir, gust_kmh, cloud,
//	            cloud_low, cloud_mid, cloud_high, precip_mm, rain_mm, snow_cm,
//	            visibility_m, cape, weather_code INT, is_day INT, n_surface,
//	            dn_dh, k_factor, duct, profile_json TEXT)
//	    hourly observations / analysis; every measurement is nullable.
//
//	weather_fc(t INT PK, issued, ... same measurement columns ..., profile_json)
//	    latest forecast per target hour. Since 1.1 a poll rewrites only the
//	    rows whose values changed; the issue time of the newest forecast's
//	    window is kv store.weather_fc.issue, applied by WeatherForecast.
//
//	weather_fc_log(t, lead_h, issued, ... same measurement columns ...,
//	               PK(t, lead_h)) WITHOUT ROWID
//	    what was forecast for hour t, lead_h hours ahead (backtesting).
//
//	ntp_samples(t INT PK, server, offset_ms, delay_ms, stratum)
//	    offset_ms = true UTC - system clock. Kept for MinuteRetentionDays.
//
//	met_obs(t, icao, lat, lon, alt INT, wind_dir, wind_kt, oat_c, src INT)
//	    aircraft-derived wind / temperature observations: the LOGICAL 1.0.0
//	    table since 1.1 (export, parity). Stored as met_sample(k INTEGER
//	    PRIMARY KEY = t << 25 | icao, lat_e5, lon_e5, alt_d = alt - 20000,
//	    w = packed wind, o = oat_x100 * 4 + src; VIRTUAL t, icao, alt,
//	    wdir_x10, wkt_x10, oat_x100, src), kept for
//	    Storage.TrailRetentionDays (pruner met_sample); data migration met 2
//	    converts the 1.0.0 rows. See met_store.go. kv: count.met_sample,
//	    count.guess_samples.<stn>, store.weather_fc.issue.
//
//	met_profile(hour, level, u_sum, v_sum, t_sum, n_wind, n_temp,
//	            PK(hour, level)) WITHOUT ROWID
//	    hourly vertical profile; hour = start of the hour (Unix seconds),
//	    level = floor(alt_ft/1000) (MetLevel). Means: u_sum/n_wind,
//	    v_sum/n_wind, t_sum/n_temp.
//
//	records(key TEXT PK, value REAL, icao INT NULL, t, detail_json TEXT NULL)
//	    all-time records (farthest, highest, fastest ...).
//
//	events(id INT PK, t, kind TEXT, icao INT NULL, detail_json TEXT NULL)
//	    emergencies, new records, feed up/down, notable catches.
//	    Index: events_t(t).
//
//	acdb(icao INT PK, reg, type, "desc", operator, flags INT, year INT)
//	    imported aircraft database. "desc" is a keyword: quote it.
//
//	receiver_status(t, feed, json, PK(t, feed)) WITHOUT ROWID
//	    receiver status frames (GPS lock etc.). Kept for MinuteRetentionDays.
//
//	flight_station(flight_id, sid, icao, first, last, msgs, positions,
//	               max_dist_km REAL NULL, min_dist_km REAL NULL, rssi_min,
//	               rssi_max, rssi_avg REAL NULL, PK(flight_id, sid)) WITHOUT ROWID
//	    (core migration 4) what one station heard of one flight (aggregator
//	    mode; Batch.UpsertFlightStation). Index flight_station_sid(sid, first).
//	    Deleted together with its flight by Prune.
//
//	mlat_minute(t, sid, frames, sync_frames, groups, fixes, outliers, peers,
//	            synced_secs, sigma_ns, resid_rms_ns, drift_ppm REAL NULL,
//	            PK(t, sid)) WITHOUT ROWID
//	    (core 4) per-minute MLAT contribution of a station, sid '' = network.
//
//	mlat_quality_hour(t, cell1deg, altband, n, p50, p95, p99 REAL NULL,
//	                  err_hist BLOB, PK(t, cell1deg, altband)) WITHOUT ROWID
//	    (core 4) hold-out MLAT error per hour, 1° cell (Cell1Deg) and AltBand;
//	    err_hist = MLATErrBins little-endian uint32 counts (MLATErrBin).
//
//	stats_day(... stats_hour columns ..., hours INT, PK(t, ant)) WITHOUT ROWID
//	    (core 4; also stations.db) UTC-day rollup of stats_hour (RollupDays),
//	    the tier that outlives the hourly statistics (Maintain, TierPolicy).
//
// # stations.db (aggregator mode)
//
// OpenStations opens the second database kind (see Kind): the per-antenna
// statistics tables above with namespaced keys ant = 's<idx>' /
// 's<idx>/<ch>' (AntKey), receiver_status (feed = 'st/<sid>/<rx>'),
// stats_day, heat keyed (ant, cell), and
//
//	station_minute(t, sid, frames, timed, late, dropped, bytes, up_secs,
//	               rtt_ms, PK(t, sid)) WITHOUT ROWID       link health
//	station_clock_minute(t, sid, synced_secs, sigma_ns, drift_ppm,
//	               beta_short_ns, resid_rms_ns, outliers, peers,
//	               epoch_changes, PK(t, sid)) WITHOUT ROWID clock state
//
// The registry tables of internal/registry share the file. Per-station
// reads use SeriesIn; Maintain applies the tiered retention; WIRING.md
// describes the aggregator wiring.
//
// # Trail tiers (SO-6)
//
// Stored position trails pass through three tiers: every point while the
// flight ended less than TierPolicy.TrailFullDays ago (default 2), then one
// point per TrailThinSec (default 30 s: the first point of every bucket of
// Unix time, plus the flight's last point; ThinTrails, run by Maintain on
// airview.db), then only the flights row once the trail retention deletes
// the positions (Prune). downsample.go derives the error bound (the
// sagitta of a turn between two kept points, about 1 km) and gives the
// measured sizes: thinned trails take 15 % of the bytes, the steady state
// of 7 trail days shrinks by 60 %.
//
// # Analytical queries
//
// Write them against DB() with a context. Useful facts: every time-series
// table is keyed (or indexed) by time first, so range scans on t / day / hour
// are cheap; the per-day histogram tables are small enough to aggregate
// over months with SUM/MAX ... GROUP BY; a query may run for seconds without
// disturbing the writer, but a reader that stays open for minutes keeps the
// WAL from being checkpointed - page long results instead.
package store
