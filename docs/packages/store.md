# store - API notes (from the package author)

## package store ("airview/internal/store") — full table/column reference is the package doc in doc.go

Conventions: DB times are Unix SECONDS; unknown float = NaN (stored NULL, read back NaN); ICAO is uint32 (0 = none); Ant "" = all antennas combined. Row types returned by the read API implement json.Marshaler in API form (ms timestamps, 6-hex ICAO, unknowns omitted) — pass them straight to encoding/json.

Lifecycle
- Open(dir) (*Store, error); OpenWithOptions(dir, Options{FlushInterval=2s, FlushRows=4000, MaxQueueRows=200000, ReadConns=4, Logger}).
- File "airview.db" (const FileName). A file that is not an SQLite DB is moved to airview.db.corrupt-<n>.
- (*Store).DB() *sql.DB — read-only pool (query_only). Call DB() at each query; do not cache it across Reset.
- Dir(), Path(), Close() error (flush + checkpoint, idempotent), Reset() error (deletes db/-wal/-shm, reopens empty and migrated; same *Store, new DB() pool, flight IDs restart at 1; discards queued rows; stop readers/writers first). ErrClosed.
- RegisterMigration(name string, version int, ddl string) error — from init() or later (applied at once to open stores). "core" is reserved; DDL runs once in one transaction; prefix tables with the owner name.

Writing (all on one writer goroutine)
- Write(ctx, func(tx *sql.Tx) error) error — synchronous, own transaction, runs after everything queued earlier. fn must not call synchronous Store methods.
- b := NewBatch(); fill with typed methods; Submit(b) queues (never blocks on disk; flushed every ~2 s or at FlushRows); Apply(ctx, b) writes synchronously all-or-nothing. Both empty b. Flush() error commits the queue.
- Batch methods (Store has same-named async shortcuts taking the same args):
  - UpsertAircraft([]AircraftSeen) — Msgs/Flights/Positions are DELTAS (added); first=min, last=max; max dist/alt; masks OR-ed; non-empty text replaces.
  - UpsertFlights([]Flight) — full row by ID: open, update and close are the same call. ID from st.NextFlightID(); build with NewFlight(id, icao, start).
  - AddPositions([]Position) — PK (FlightID, T). PositionFromTrail(flightID, model.TrailPoint, track); Position.TrailPoint().
  - AddMinuteStats([]StatsRow) — merges into (minute, ant).
  - AddRangeHours([]RangeHour) — merges histograms/sectors, recomputes percentiles.
  - MergeCoverage([]CoverageInc), MergeSignalHist([]SignalHistInc), MergeElevHist([]ElevHistInc), MergeHeat([]HeatInc), MergeMetProfile([]MetProfileInc) — UPSERT: sums added, max of max, min of min.
  - AddWeatherObs([]Weather) (column-wise merge, NaN keeps stored), SetWeatherForecast([]Weather), AddWeatherForecastLog([]Weather); NewWeather(t).
  - AddNTPSamples([]NTPSample), AddMetObs([]MetObs), AddReceiverStatus([]ReceiverStatus), AddEvents([]Event), AddACDB([]ACDBEntry).
  - SetRecord(Record, RecordMax|RecordMin|RecordAlways); SetKV(key, value) (Store shortcut is SetKVAsync).
- Synchronous: KVSet(ctx,k,v), KVDelete(ctx,k), ImportACDB(ctx, replace bool, iter.Seq[ACDBEntry]) (int, error) (50k-row transactions), RollupHours(ctx) (int, error) (idempotent stats_minute -> stats_hour).
- Overflow: oldest rows dropped by tier — low (coverage/hist/heat/met), then mid (positions/ntp/receiver_status), high last. Counters in Stats.

Reading
- AircraftByICAO(ctx, icao) (AircraftRow, ok, err) — joined with acdb.
- ListAircraft(ctx, AircraftQuery{Search, Since, Until, Sort, Desc, Limit, Offset}) (rows, total, err). Sort: last_seen (default) | first_seen | icao | callsign | reg | type | operator | country | msgs | flights | positions | max_dist_km | max_alt. Default order is ASCENDING: set Desc for newest first. Limit default 100, max 5000.
- FlightsForAircraft(ctx, icao, limit, offset), RecentFlights(ctx, limit, offset), FlightsBetween(ctx, from, to, limit), FlightByID(ctx, id), LatestFlight(ctx, icao).
- Positions(ctx, flightID, from, to) []Position; Trail(ctx, flightID) []model.TrailPoint; ForEachPosition(ctx, from, to, fn(icao, Position) error).
- Events(ctx, EventQuery{Kind, ICAO, Since, BeforeID, Limit}) newest first; Records(ctx); RecordByKey(ctx, key); KVGet(ctx, key) (value, ok, err).
- ACDBLookup(ctx, icao), ACDBCount(ctx).
- WeatherObs / WeatherForecast / WeatherForecastLog(ctx, from, to); MinuteStats / HourStats(ctx, ant, from, to); RangeHours(ctx, ant, from, to); NTPSamples(ctx, from, to, limit) (newest `limit`, returned oldest first).
- DBSize() int64 (db + wal; cheap, use for status.dbBytes); TableCounts(ctx) map[string]int64 (cached); Stats() Stats (sizes, pages, queue, flush/drop counters, LastError).

Maintenance
- Prune(ctx, model.StorageConfig) (PruneResult, error) — chunked; a period <= 0 keeps forever. Trail retention: positions + met_obs; flight retention: flights with positions; minute retention: stats_minute (only after rollup), ntp_samples, receiver_status.
- Checkpoint(ctx), Vacuum(ctx) (blocks the writer; user-requested only).

Bin helpers (writer and reader must both use these): DayOf/DayStart, AltBand/AltBandRange (AltBands=7), BrgBin (BrgBins=180), DistBin (10 km), RSSIBin (floor dBFS, -80..0), ElevBin/ElevBinLow (ElevBins=184, 0.5 deg from -2), HeatCell/HeatCellCenter (0.05 deg), MetLevel; EncodeDistHist/DecodeDistHist, EncodeSectorMax/DecodeSectorMax (DistHistBins=120 x 5 km, SectorBins=36 x 10 deg), HistPercentile, HexICAO. Constants: Src{ADSB,MLAT,TISB,Other}=0..3 (same as model.TrailPoint.Src), Mask* (1<<Src, MaskModeS=16), Flag{Military=1,Interesting=2,PIA=4,LADD=8}.

Example
  st, _ := store.Open(dir); defer st.Close()
  b := store.NewBatch()
  id := st.NextFlightID(); f := store.NewFlight(id, icao, now)
  b.UpsertFlights([]store.Flight{f}); b.AddPositions(ps)
  b.UpsertAircraft([]store.AircraftSeen{{ICAO: icao, LastSeen: now, Msgs: 12, Flights: 1}})
  st.Submit(b)
  st.RollupHours(ctx); st.Prune(ctx, cfg.Storage)
  rows, total, _ := st.ListAircraft(ctx, store.AircraftQuery{Search: "ryr", Desc: true, Limit: 50})

## package config ("airview/internal/config")

- ResolveDataDir(flag string) (string, error): flag, else <exe dir>/data if writable, else os.UserConfigDir()/AirView; creates it.
- Load(dir) (model.Config, error): logs warnings. LoadWithWarnings(dir) (model.Config, []string, error) returns them. Missing file: defaults saved. Older/version-less: migrated and re-saved. Newer: known fields kept, original backed up as config.json.v<N>.bak. Corrupt: renamed config.json.bad-<n>, defaults used, reported as a warning. The returned config is always validated and usable, also when err != nil.
- Save(dir, cfg) error: validates, then temp file + fsync + rename. FileName = "config.json".
- Validate(cfg) (model.Config, []string): never panics, idempotent, does not alias its input. Limits: MaxAntennas=16, MaxFeeds=32, MaxRangeRings=12. Slug(s) string is the ID sanitiser.
- HashPassword(pw) string ("sha256:<hex salt>:<hex sha256(salt||pw)>", "" for empty pw); CheckPassword(hash, pw) bool (constant time; empty/malformed hash matches nothing — the caller decides that an empty hash means "open").
- Redact(cfg) model.Config: http.adminPasswordHash, hub.password, remote.password become "" or "set".
- MergeSecrets(incoming, current) model.Config, wire convention for the three secret fields: "" or "set" keeps the stored secret; "clear" removes it; "plain:<text>" sets <text> ("plain:" alone removes); any other value is the new secret. For adminPasswordHash the new secret is the PLAIN admin password and is hashed server-side (a well-formed hash is stored verbatim). Constants SecretSet, SecretClear, PlainPrefix.
- ApplyUpdate(incoming, current) (model.Config, []string) = Validate(MergeSecrets(...)) — the PUT /api/config pipeline; then Save, holder.Set, reply Redact(next).
- FactoryReset(dir) (model.Config, error): writes defaults; the caller does store.Reset().
- Clone(cfg), Equal(a, b).
- Holder: NewHolder(cfg); Get() (deep copy); Set(cfg); Update(func(*model.Config)) model.Config; Subscribe(func(old, cur model.Config)) (unsubscribe func()). Callbacks run synchronously in order on the Set caller's goroutine, also when nothing changed; a callback must not call Set/Update on the same Holder.
