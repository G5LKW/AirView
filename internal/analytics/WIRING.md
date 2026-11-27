# internal/analytics — wiring notes (aggregator mode, WP-D)

Standalone: unchanged, `analytics.New(airviewDB, cfg.Get)`.

Aggregator, one Analyzer per station (DESIGN §6.5, §6.6):

```go
a := analytics.New(stationsDB, stationCfg,              // stationCfg: Receiver = the station's location
    analytics.WithAntPrefix(store.StationAntPrefix(idx)), // "s<idx>": ant "" -> s<idx>, "1" -> s<idx>/1
    analytics.WithNetwork(airviewDB, sid))                // flights/aircraft/weather from airview.db,
                                                          // flight figures scoped to flight_station of sid
```

- Every public method keeps its signature; `ant` stays the unprefixed channel ID and results report it
  unprefixed. With prefix "" the behaviour is byte-for-byte the standalone one
  (`TestPrefixMatchesStandalone` compares all analyses of a standalone database with the same data
  stored as station `s7` between other stations).
- With a SID, Traffic/Daily/Top/AltitudeDistribution/Overview count only the station's flights, with
  its message counts and distances (`TestStationSIDScopesFlights`). Records and events stay network-wide.
- An Analyzer on stations.db **needs** `WithNetwork` (stations.db has no flights/aircraft/weather).
- Per-station day-range queries use `store.SeriesIn` (one key probe per day) so cost does not grow
  with the number of stations.
- `Daily` fills days older than the hourly data from `stats_day` (the daily tier kept by
  `store.Maintain`).
- Privacy: every result of a station Analyzer is owner/admin data (§8.2). Never serve it on public routes.

## Locate Me v2 (locate2*.go)

Package documentation: `locate2_doc.go`. The engine wires it in
`internal/engine/locatewire.go`: a `GuessRecorder` on the tracker's guess-sample
hook (standalone: airview.db; aggregator: stations.db, only stations without a
location), `SetLocateSources` (cached terrain tiles; standalone also the
receiver GPS and the `L2TimingCollector` fed through `latency.Analyzer.SetTap`).
`GuessLocation` merges the v2 estimate; a station Analyzer's `LocateMe` serves
`GET /api/me/stations/{sid}/locate` (owner/admin). Settings: `locate.*`.

## Pending increments (DB efficiency, 1.1)

`analytics.WithPending(tracker.PendingIncs)` (or `WithOptions(Options{Pending: ...})`) on the standalone
Analyzer: coverage, outline, signal, elevation, altitudes, heat grid and `Analyzer.RangeHours`
(`/api/stats/range`, day/night, weather impact) merge the increments the tracker has not written yet with
the writer's rules (`store.Overlay*`, `store.MergeRangeHour`), so the longer tracker flush intervals of
the storage profiles never show on a page (`TestPendingOverlayEqualsWritten`). Station Analyzers on
stations.db get no Pending. Daily totals read distinct aircraft per day from the `flights_hour` rollup
(`store.FlightDays`, equal to the 1.0.0 `COUNT(DISTINCT icao)` for every UTC offset); `Top` looks up
aircraft / acdb attributes once per airframe (`TestTopEqualsLegacy`).
