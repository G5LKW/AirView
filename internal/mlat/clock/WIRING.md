# internal/mlat/clock: wiring guide

`import "airview/internal/mlat/clock"`. The joint clock filter of DESIGN §9.2; the maths is in `doc.go`.

## Create and run

```go
f, _ := clock.New(clock.Config{
    OriginUnixNs: start.Truncate(time.Second).UnixNano(), // network seconds are relative to this
    EpochS:       float64(cfg.Aggregator.MLAT.EpochMs) / 1000,
    MaxComponentStations: cfg.Aggregator.MLAT.MaxComponentStations,
    Atmosphere:   geo.Atmosphere{N0: n0, H: h, K: kAtm}.Normalize(),
    OnBlacklist:  refStore.Suspend,            // mlat/ref: stop offering blacklisted aircraft
    Logf:         logf,
})
go f.Run(ctx, ntpNowUnixNs, time.Second)       // one Step per epoch, 1 s behind real time
```

Stations (registry): `f.SetStation(idx, clock.StationInfo{Class, Pos /*ECEF, HAE*/, Located, NoStatus /*mlatc radarcape_gps*/, Flagged /*admin*/, JitterNs /*quality*/})`, `f.RemoveStation(idx)`. Call again on any change.

## Ingest

- `ingest.Options.Clock = clock.View{F: f, Ref: refStore}` (ingest.ClockView). `View.NetTime` returns **ns since `OriginUnixNs`**, not Unix ns.
- `ingest.Options.ClockObserver = f` (CLOCK_EVENT, CLOCK_STATUS; MapOnly events are ignored).
- In `MLATSink.Observe(g)`: `f.ObserveGroup(g, refStore)` for every group (it filters DF, tracker result, Timed copies and the reference itself; zero allocations when it rejects).
- `refStore` implements `clock.RefSource`: `RefPosition(key, df, tUnixNs) (ECEF, cov upper triangle m², ok)` and `Usable(key)`.

### Gate view (H-clock)

`clock.GateView{F: f, Ref: refStore}` is a second `ingest.ClockView` that also implements `ingest.ClockKindView`: the ingest's synced-station gate then compares network times of the frame's own kind (frame reference and β_short) with σ including each receiver's per-frame jitter (`Snapshot.NetTimeTotal`). Together with `ingest.Options.ClockGateFloor` (default 50 µs) the gate only separates identical transmissions and no longer removes the copies of a clock that drifted by a few σ. `View` is unchanged because `internal/mlat` embeds it and overrides `NetTime`; switching to `GateView` (and letting `SyncCandidate` answer, now that single-copy flushes are re-delivered with every copy, `ingest.Group.Redelivery`) is the engine's choice (HOOKS H-clock -> internal/mlat).

## Readers (lock-free)

```go
snap := f.Snapshot()
snap.Convert(queries, t, cov, ok)                         // solver: network seconds + full m×m covariance (ns²), 0 allocs
ns, frac, clockVar, jitterVar, ok := snap.NetTimeUnix(q)  // group.Clock.NetTime
snap.CrossCov(a, b); snap.Domain(st)                      // group.CrossCov / group.Domains
sc := snap.Station(st)                                    // owner/admin stats: Class, AnchorState/Reason, SigmaThetaNs,
                                                          // Phi (ppb), Beta (ns), OutlierPct, Events, Peers, SyncedPeers()
```

Use one time domain per cluster (`StationClock.Domain`); set `model.FixFlagRelativeTime` when `StationClock.RelativeTime`.

Times stay "seconds since `OriginUnixNs`" in the API; internally the filter moves its origin every `ReoriginS` (1 day) so its own float64 times never lose resolution.

## Statistics

`f.Stats()`: `Steps`, `FreqSteps` (steps whose residual run showed a frequency error; φ corrected too), `FreqResets`, `LastEpochNs` / `MeanEpochNs` (precise system clock: non-zero on Windows), `UpdateFailures`, admission and blacklist counters.

## Short frames in anchor-less networks

Relative components now carry a β_short gauge (mean β = const), so short-frame sync groups (DF0/4/5/11 of ADS-B aircraft) can be used in SDR-only networks too; the engine still feeds them only to anchored components (HOOKS H-clock -> internal/mlat: measure before switching).

## Cost and scale (phase 5)

Epoch (Step) at one dense component: 2 ms at 50 stations, 9.5 ms at 100, 61-64 ms at 200 (732 states) on an i9-13900K core. Above `MaxComponentStations` (200) a time domain is split into compact partitions of at most `PartitionStations` (100); the cost then grows linearly, about 10 ms per partition on one core, and partitions run in parallel on `Workers` goroutines (default GOMAXPROCS).

| Stations (one domain, simnet) | Partitions | Epoch median (1 core) | Synced | RMS GPS / hw12 / sdr12 |
|---|---|---|---|---|
| 200 | 1 | 61 ms | 200 | 7 / 8 / 73 ns |
| 500 | 5 | 51 ms | 500 | 10 / 13 / 73 ns |
| 1,000 | 10 | 104 ms | 1,000 | 16 / 18 / 73 ns |

With `Workers` 8 the 1,000-station epoch takes 29 ms (median).

Reproduce: `AIRVIEW_CLOCK_SCALE=1 go test -run TestSimnetScaleCurve -v ./internal/mlat/clock/` (about 4 minutes); `TestSimnetScaleExplore` takes `AIRVIEW_CLOCK_SCALE_N`, `_S`, `_MAX`, `_PART`, `_WORKERS`, `_EXT`, `_INFL`, `_DRIFT`, `_PROB`, `_TRACE`.

Partition tunables in `clock.Config` (zero = default): `PartitionStations` (100), `ExternalStates` (64; negative = off), `ExternalInflate` (2), `PartitionDriftNs` (1 ns/√s; negative = off). Only `MaxComponentStations` and `ExternalInflate` are in the config file today (HOOKS WP-SCALE-clock -> internal/model + internal/config). `Stats().ExternalStates` / `ExternalFailures` report the cross-partition states per epoch.
