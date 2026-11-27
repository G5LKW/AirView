# internal/mlat: wiring guide (WP-M, DESIGN §9)

`import "airview/internal/mlat"`. The engine root: it constructs and connects `mlat/ref`, `mlat/clock`,
`mlat/plan`, `mlat/group`, `mlat/solve`, `mlat/track` and `mlat/quality`, sits behind the ingest, and routes
results. Maths and design decisions: `doc.go`. Quality package: `quality/doc.go`.

## 1. Construction (engine, aggregator mode, `aggregator.mlat.enabled`)

```go
eng, err := mlat.New(mlat.Options{
    Config:  cfg.Aggregator.MLAT,                 // every sub-block (ref/group/plan/solve/track) is mapped inside
    Now:     ntp.Now,
    Logger:  log.With("component", "mlat"),
    Updates: ing,                                 // ingest.HandleUpdate: MLAT=true, Src=mlat-av → global tracker
    Results: []mlat.ResultSender{avfSrv, mlatcSrv}, // contributing feeders only (§8.4); first sender that has the station wins
    PlanOut: plan.Fanout{plan.IngestOut{Ingest: ing}, plan.MLATCOut{C: mlatcSrv}},
    Survey:  surveyor,                            // phase 3 (mlat/survey); nil = none
    OnPosition: func(st model.StationIdx, src mlat.PosSource) { /* log */ },
    Notifier:   ownerNotices,                     // mlat.PositionNotifier: PositionChange{Old, New, ECEF, SigmaM, AutoSurvey, Reason} (DESIGN §19.3)
})
// ingest options (before ingest.New):
io.MLAT = eng                 // ingest.MLATSink + LateObserver
io.Clock = eng.ClockView()    // see "Ingest interface" in doc.go
io.ClockObserver = eng        // CLOCK_EVENT / CLOCK_STATUS → clock filter
go eng.Run(ctx)               // Advance every 100 ms; Close on ctx done
```

mlat-client server: `mlatc.Options.Stats = eng` (`mlatc.StatsSource`), `Hooks.OnSeen = eng.NoteSeen`.

Stations (registry → engine; call on every change):

```go
eng.SetStation(idx, mlat.StationInfo{Class, Declared: mlat.Position{Valid, ECEF /*HAE*/, SigmaM}, GNSS, Confirmed,
    Suspect, DiscrepancyM, NoStatus /*mlatc radarcape_gps*/, Flagged /*admin*/})
eng.SetSurvey(idx, mlat.Survey{Valid, ECEF, SigmaHM, SigmaVM, Stable, Suspect, OwnerRejected}) // phase 3
eng.OverrideSurvey(idx, true /*reject*/)  // owner override of the automatic survey use (standing; false lifts it)
eng.SurveyOf(idx)                         // last survey (owner/admin only, never public)
eng.RemoveStation(idx)
```

Phase 3 (`internal/mlat/survey`) and the owner API depend on the interface `mlat.SurveySink` (`SetSurvey`,
`OverrideSurvey`), which `*mlat.Engine` implements; every change of a station's effective position source reaches
`OnPosition` and the `Notifier` synchronously (do not call back into the engine from them).

Position order (DESIGN §10.1, §19.3): GNSS → owner-confirmed → survey (only with `AutoUseSurvey`, stable,
not suspect, not owner-rejected, σ_h ≤ `AutoUseMaxSigmaM`) → declared (σ inflated by the discrepancy when
suspect). `eng.PositionSource(idx)` tells which one is in use.

## 2. Read side (all safe from any goroutine)

| Call | Scope | Content |
|---|---|---|
| `eng.Stats()` | admin | counters of every stage, clock/ref/group/track/plan stats, k_atm |
| `eng.QualityReport(windowMin, cells)` | admin | hold-out error (fix and track), σ calibration, NEES, week-on-week alarm, error map; `FixEst`/`TrackEst`: P50/P95 estimated from the cross-track errors (free of the reference's airframe latency, `EstimateFromCross`) |
| `eng.PublicQuality()` | public | P50/P95/P99 of the last hour vs the ADS-B reference (≥ 20 samples), `crossP50M`/`crossP95M`, `estP50M`/`estP95M` (the latency-free estimate: the honest headline figure), updates/min, 1° cells with ≥ 5 contributing stations; no identities |
| `eng.Station(idx)` | owner (own) / admin | position source, clock report (σ_θ, drift, β_short, events, peers by raw index → map through `reg.PeerTag`), MLAT report (contributions/h, used/excluded/outlier %, residual bias, learned jitter, bias flag), timed frames/min and result eligibility |
| `eng.SyncMatrix()` | admin | per station and peer: co-observations, RMS, relative ppb, same domain |
| `eng.CRLBMap(quality.CRLBParams{})` | admin | expected σ_h at 3000 ft / FL100 / FL300 on a 0.25° grid (seconds of CPU: compute on station changes) |
| `eng.Fixes()` | internal | stream of every published fix (hold-out fixes flagged `FixFlagHoldout`) |
| `eng.Stats()` `Restarts`, `GateSolves` | admin | tracks restarted on contradicting fixes or gate failures of redundant clusters; clusters solved only to check a gated track |

## 3. Tunables (`Options`, defaults in brackets; file: `aggregator.mlat.engine`, zero = default, explicit `Options` win)

`Workers` [GOMAXPROCS], `QueueLen` [4096], `PublishInterval` [500 ms], `MaxSolveSites` [16],
`ContribWindow` [60 s], `MinTimedPerMin` [10], `HoldoutRotate` [10 min], `LatencyFeedback` [off],
`JitterFeedback` [off], `ApplyKAtm` [off], `VerifyEvery` [5 s], `SyncWarmup` [60 s], `ClockLag` [1 s].
`Config.Holdout` [0.10], `Config.AutoUseSurvey` [true], `Config.AutoUseMaxSigmaM` [50], `Config.ReturnResults`,
`Config.ModeAC` (phase 5, see §6), `ModeACCodeFormat` [`pulses`], `ModeACJitterNs` [50], `ModeACOut` [none].

## 4. Tests, tools and numbers

```
go test ./internal/mlat/...                                   # unit + §13.2 rows 1-3 (≈ 2.5 min)
go test ./internal/mlat -short                                # unit only
MLAT_HTRACK=1 [MLAT_HTRACK_LIVE=1] [MLAT_HTRACK_MIN=20] go test ./internal/mlat -run HoldoutTrackNetsim -v  # netsim seed 7 hold-out tracks
MLAT_LIVE=1 [MLAT_LIVE_MIN=12] [MLAT_LIVE_PORT=28701] go test ./internal/mlat -run LiveNetsim -v   # same, real time, real sockets
MLAT_THROUGHPUT=1 go test ./internal/mlat -run Throughput -v   # 100 / 500 stations
MLAT_SEEDS="13 14 15 16" go test ./internal/mlat -run SeedSweep -v
go run ./internal/mlat/cmd/mlatsim -rx 100 -aircraft 600 -dur 14m -listen 127.0.0.1:18611
```

Measured on the dev machine (i9-13900K, Windows 11, Go 1.27.3), simnet through the real ingest with
20-80 ms network latency, default simnet airframe position latency 0-300 ms, published (tracker) positions
inside the network radius against the simulator's truth, last 4 of 16 simulated minutes:

| §13.2 row | Result | Gate |
|---|---|---|
| 1: 8 GPS anchors, 150 km (seed 11) | P50 5.5 m, P95 17.9 m, P99 42.9 m; within 1σ 63.1 %, 2σ 92.7 %; 12,989 positions; 8/8 anchors | P50 < 20, P95 < 60, 1σ 60-75 % — **pass** |
| 2: 4 GPS + 20 SDR (seed 12) | P50 11.6 m, P95 106.7 m; within 1σ 64.5 %; Mode S-only aircraft heard by ≥ 4 stations (≥ 20 transmissions): 61 of 62 got fixes (98.4 %); by ≥ 4 anchors: 35/35 | P50 < 80, P95 < 250, ≥ 90 % — **pass** |
| 3: 20 SDR (seed 13) | P50 58-92 m, P95 300-338 m, 1σ 21-47 % (two runs; the clock filter degrades after 11 min on this seed) | P50 < 200 — **pass**, but see below |

Runs with the synchronous engine (`Workers < 0`) are bit-reproducible (`TestReproducible`: the same fixes, bit for bit and
in the same order, twice over 12 simulated minutes); asynchronous workers keep the order per aircraft only.

Row 3 across seeds (`TestSeedSweepSDR`, 20 RTL-SDRs / 150 km, hold-out 10 %, 16 minutes, last 4 evaluated):

| Seed | False clock steps per minute | Published P50 / P95 | within 1σ |
|---|---|---|---|
| 14, 15, 16, 18, 19, 20 | 0 (seed 16: 5 in minute 3) | 23-33 m / 104-162 m | 82-90 % |
| 13 | 0 until minute 11, then 15-150 | 58 m / 300 m | 47 % |
| 17 | 41-249 from minute 2 | 118 m / 2.9 km | 12 % |

So 6 of 8 SDR-only seeds are clean and 2 are not. The cause is inside the clock filter: a few SDR clocks end up
with φ 30-50 ppm off, get "stepped" every few seconds (a step resets θ but keeps φ), pull their peers and get
reference aircraft blacklisted. Evidence: through the full pipeline, seeds 16 and 18 also stepped 80-160 times
per minute with the simulator's *true* aircraft positions as references; feeding the filter directly from
simnet (`TestDiagDirectClock`, true positions, no ingest) gave 0-1 steps for seeds 15 and 18; the engine-side
changes below (sync warm-up, emission-window filter and anchored-only use of short frames, no ingest
network-time gate) moved the failures from seeds 13, 16, 18, 19 to seeds 13 and 17. Filed in HOOKS.md with the
reproduction.

Hold-out (live numbers the engine publishes vs the latency-corrected ADS-B reference, row 1): track
P50 39 m, cross-track P50 12 m; against the truth the same hold-out tracks inside the radius have P50 6.8 m,
P95 18.6 m. The difference is the reference's unknown airframe position latency (simnet: 0-300 ms), which the
engine does not learn from the hold-out by default (`LatencyFeedback`).

Throughput (`TestThroughput`, simnet DefaultMix, hold-out 10 %, synchronous engine, worker time only; the clock
filter's epoch cost is separate):

| Network | Groups/s | Clusters/s | Solves/s | Engine µs/group | Engine CPU | Clock epoch (every 0.5 s) |
|---|---|---|---|---|---|---|
| 100 stations, 600 aircraft, 300 km | 3,941 | 834 | 325 | 11.4 | 0.045 cores | 9.5 ms (0.019 cores) |
| 500 stations, 1,200 aircraft, 500 km | 7,987 | 2,154 | 708 | 21.0 | 0.168 cores | 74.5 ms (3 components; 0.15 cores) |

With 8 asynchronous workers (500 stations) no group was dropped; see PROGRESS.md for the wall-clock figures.

## 5. Integrator after the hardening wave

- `aggregator.mlat.clock` is mapped onto `clock.Config` in `New` (`clock.ConfigFromModel`, test `TestConfigFromModel`), including `reoriginS`, `relGaugeBetaSigmaNs`, `externalInflate`; `stepRunCount: -1` now really switches the frequency reset off.
- `aggregator.mlat.ref` also carries `altReplyMinS`, `pairBaroS`, `noAltReplies`, `field.profileSigmaM`, `field.profileSigmaTK`.
- Mode S altitude replies (DF0/4/16/20) of aircraft with a reference track are passed to `refs.Observe` (vertical channel, TC 20-22 pairing; counted in `ref.Stats.AltReplies`).
- `Options.IngestGate` / `aggregator.mlat.engine.ingestGate` (default off): `ClockView` returns `clock.GateView`; re-delivered single-copy groups (`ingest.Group.Redelivery`) go only to the clock filter and the assembler. Measured on the 8-seed SDR sweep: no difference within noise (MLAT-RESULTS.md §0c), so it stays off.
- `Options.Weather` is set by internal/engine (`ref.BundleWeather` from the host's weather poller).

## 6. Mode A/C MLAT (phase 5, DESIGN §9.9, `aggregator.mlat.modeAc`, off by default)

With `Config.ModeAC` set, `New` builds the Mode A/C path (`modeac.go`); nothing runs otherwise.

```go
mlat.New(mlat.Options{..., ModeACOut: plan.ModeACFanout{plan.MLATCModeAC{C: mlatcSrv}}}) // ask Mode A/C stations for every A/C reply
eng.ObserveModeACFrames(frames)  // links: model.FrameModeAC frames with ticks (others ignored); any goroutine
eng.ObserveModeAC([]mlat.ModeACReply{{Station, Epoch, Ticks, Code}}) // or already decoded (raw pulse layout)
eng.EnableModeAC(func(f mlat.ModeACFix) { ... })  // optional: every accepted fix (no address; Target = synthetic id)
eng.ModeACTargets()   // public: shown targets, republished once a second ([]mlat.ModeACTarget, JSON-ready)
eng.ModeACAircraft()  // the same as model.Aircraft: ICAO "#xxxxxx", AddrType "modeac", Source mlat
eng.ModeACStats()     // admin counters (replies, unsynced, clusters, solves, tracker stats, stations, shown)
```

Pipeline: network time (clock snapshot) → hold 600 ms → `group.ModeAC` (code + time + geometry among the planner's
Mode A/C stations) → ≥ 4 stations: solver (prior = matching target's prediction) → `track.ACTracker.Fix`; 2-3
stations: `track.ACTracker.Cluster` (updates shown targets only) → 1 Hz publication of grade A-C targets.

`ModeACTarget` (public, no station identities): `id` ("#" + 6 hex; never a valid ICAO string), `key`, `squawk`,
`altFt` (Mode C), `lat`/`lon`/`altGeomM`, `gs`/`track`/`vertRate`, `sigmaHM`, `grade`, `nBucket`, `fixes`,
`updates`, `ageS`, `seenS`, `crowded`, `t`. The map side (server) and the link side (engine/ingest routing of
`FrameModeAC`) are integrator hooks (HOOKS.md, WP-MODEAC).

Tests and numbers (i9-13900K, Go 1.27.3; aircraft are Mode A/C only, replies generated per radar sweep — dwell
of 12 replies 4 ms apart alternating A and C — propagated with simnet refraction and stamped by the true simnet
receiver clocks; the 8 GPS anchors are synced by simnet's Mode S traffic):

```
go test ./internal/mlat -run ModeACNetsim -v                          # acceptance, ≈ 35 s
MLAT_MODEAC_HARD=1 go test ./internal/mlat -run ModeACNetsimHard -v   # stress, ≈ 30 s
MLAT_MODEAC_DEBUG=1 ...                                               # per-target dump
```

| Scenario | Availability | Shown error P50 / P95 | Ids/aircraft | Squawk correct | False targets |
|---|---|---|---|---|---|
| 40 aircraft (¼ on 7000), 4 radars, 75 % reception, +50 ns, 100 FRUIT/s/station | 100.0 % of 9,640 aircraft-s | 17 m / 58 m | 1.10 | 98.3 % | 0 |
| hard: 80 aircraft (½ on 7000), 2 radars, 50 %, +80 ns, 500 FRUIT/s/station | 94.2 % of 19,278 aircraft-s | 62 m / 250 m | 1.04 | 99.3 % | 1 |

Availability = share of aircraft-seconds (aircraft heard by ≥ 4 Mode A/C stations in the last 15 s) with a shown
target within 1 km. Single fixes (before tracking): P50 56 m / P95 491 m (acceptance), 180 m / 1,047 m (hard).
Altitude present on 79 % / 54 % of shown samples (the Mode C code needs its height gate to be identified).

Cost: `BenchmarkModeAC` (group) 196 ns per reply, 0 allocs; `BenchmarkACTrackerFix` 7.4 µs per fix, 0 allocs.
At 1,000 stations hearing 2,000 replies/s each (2 M replies/s) association is ≈ 0.4 cores; Mode A/C is limited to
anchors with anchor peers within 150 km, so the realistic load is far smaller.

Limits: a Mode A/C reply does not say which interrogation it answered; codes are classified by the tracker
(altitude = Gillham code consistent with the MLAT height). Targets with only 4-station fixes need 12 fixes in 4
bursts before they are shown. Two aircraft with the same squawk closer than 5 km are graded one class worse
(`crowded`). The Mode A/C payload layout of the feeding receivers must match `ModeACCodeFormat`.
