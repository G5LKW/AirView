# internal/mlat/group: wiring notes (WP-GR, DESIGN §9.3, §9.9)

Turns the ingest's flushed timed groups into MLAT clusters (sets of network-time TOAs of one
transmission at distinct sites), splits groups that mix repeated identical replies, fuses co-located
stations, joins late copies, gives the adaptive flush delay and associates Mode A/C replies. The maths
is in `doc.go`.

## 1. Construction (WP-M, internal/mlat root)

```go
asm, err := group.New(group.Options{
    Config:   group.DefaultConfig(),  // NSigma 4, SlackM 1000, ColocatedM 1000, MinSites 4/3, LateHoldNs 2 s ...
    Clock:    clockAdapter,           // group.Clock (+ optional group.CrossCov, group.Domains), see §2
    Sites:    siteAdapter,            // group.Sites: MLAT antenna ECEF + 1σ (registry / survey, AutoUseSurvey)
    Hints:    trackAdapter,           // optional group.Hints: MLAT track prediction or ADS-B reference (ref.Store)
    Selector: planner,                // *plan.Planner (Candidate + Admit)
    Sink:     engine,                 // group.Sink: Cluster(*Cluster) -> solver (Use >= UseSolveAlt) and tracker
})
// ingest.MLATSink + ingest.LateObserver:
func (e *Engine) Observe(g *ingest.Group)     { e.planner.ObserveGroup(g); e.asm.Observe(g) }
func (e *Engine) ObserveLate(g *ingest.Group) { e.asm.ObserveLate(g) }
```

`Observe`/`ObserveLate` must be called from one goroutine (the ingest sweeper). `*Cluster` is valid only
during `Sink.Cluster`; `Clone()` to keep it. `asm.Stats()` is safe from any goroutine (admin view).

## 2. Clock adapter over `internal/mlat/clock` (WP-K)

`clock.Snapshot.NetTime(Query)` returns float64 *seconds since the snapshot origin* and a sigma that
excludes the receiver's own jitter. Adapt it as:

```go
func (a clockAdapter) NetTime(st model.StationIdx, ep uint32, ticks uint64, kind model.FrameKind) (group.Time, float64, float64, bool) {
    s := a.snap.Load()
    t, sig, ok := s.NetTime(clock.Query{Station: st, Epoch: ep, Ticks: ticks, Kind: kind})
    if !ok { return group.Time{}, 0, 0, false }
    sec := math.Floor(t); frac := (t - sec) * 1e9            // keep sub-ns precision
    return group.MakeTime(originNs+int64(sec)*1e9, 0).Add(frac), sig * sig, jitterNs(st) * jitterNs(st), true
}
func (a clockAdapter) Domain(st model.StationIdx) uint64        { /* StationClock.Domain */ }
func (a clockAdapter) CrossCov(x, y model.StationIdx) float64   { /* optional: snapshot cross-covariance */ }
```

`Domains` matters: stations in different clock components have incomparable times; without it they
could be clustered together.

## 3. Using a cluster

- `Members` grouped by `Site`; `Sites[i]` is the fused measurement (weighted centroid, weighted time,
  variance incl. cross-covariances and the second-order geometry bound). A solver may use `Sites`
  (counted once for DOF, DESIGN §9.3) or the raw `Members` with their own positions and the full
  covariance from the clock snapshot (`clock.Snapshot.Convert`).
- `Use`: `UseSolve` (≥ 4 sites), `UseSolveAlt` (3 sites + `AltFt`/`AltSrc`), `UseTrack` (2 sites, or
  refused by the planner's budget: TDOA update for a confirmed track only).
- `Parts > 1`: the group mixed several identical replies (burst); each part is its own transmission.
- `HintConflict`: the hint fragmented the cluster and was ignored (stale track or wrong 3+alt branch).
- `Late`: re-emission with late copies (`Member.Late`), same `Seq` as the original: a replacement
  measurement, not an independent one.

## 4. Adaptive flush (ingest side pending)

`group.FlushEstimator`: `ObserveDelay(st, serverRecvNs - copy.CorrNs, now)` per copy, `NoteGroup(c)`
per cluster, `Delay(key)` = max p99 latency of the aircraft's usual hearers clamped to 150-600 ms
(default 400 ms). The ingest still flushes every group at its fixed `FlushAfter`; see HOOKS.md
"WP-GR -> internal/ingest".

## 5. Mode A/C (phase 5, off by default)

`m, _ := group.NewModeAC(cfg, sites, planner.ModeACEligible)`; collect A/C replies (network time) over a
short window and call `m.Cluster(obs, emit)`; clusters have ≥ 4 anchors within 150 km. `m.ClusterMin(obs, 2, emit)`
also emits 2-3 station clusters (the engine uses them to update shown Mode A/C targets; `internal/mlat` WIRING.md §6).
`*ACCluster` (and its `Members`) is valid only during `emit`.

## 6. Tests and numbers

```
go test ./internal/mlat/group                     # unit, burst-split acceptance, simnet end-to-end
GROUP_LONG=1 go test ./internal/mlat/group -run Simnet -v   # 5 simulated minutes
go test ./internal/mlat/group -run '^$' -bench .  # per-copy cost
go run ./internal/mlat/group/cmd/groupsim -rx 300 -aircraft 1200 -dur 2m   # live pipeline, /stats on 127.0.0.1:18603
```

Measured on the dev machine (i9-13900K, Windows 11, Go 1.27.3; other agents were running, so timings
are noisy):

| Check | Result |
|---|---|
| Acceptance: identical DF4 replies mixed into one group, 40 stations / 250 km, σ 150 ns + 50 ns clock, 600 trials each | radar gaps 2.5 / 5 (×3 replies) / 50 ms: 0 mixed clusters, recall 1.0000 / 0.9999 / 1.0000, exact split 100 / 99.8 / 100 %; ACAS 0.74 ms with a 1 km or 3 km hint: 0 mixed, recall 1.0000, exact split 100 %; 0.74 ms without a hint: 6.7 % of clusters mixed, recall 0.904 (baselines > 222 km cannot separate 0.74 ms; the solver's RAIM must reject those) |
| Simnet end-to-end (60 stations, 250 aircraft, 30 s, real ingest grouper, truth clock σ 30 ns) | ingest groups mixing transmissions 0.149 %; clusters mixed 0.0094 % without hints, 0 with 1 km hints; 99.99 % of usable copies end up in a pure cluster |
| Same with every 6th station delivering 450-800 ms late | 49 k late deliveries, 189 k late copies joined to their retained cluster, 0 mixed clusters |
| `BenchmarkObserve` (per copy, 0 allocs) | 4 copies 97-118 ns; 6: 73-101 ns; 12: 85-137 ns; 30: 158-258 ns; 64: 258 ns (single), 404 ns (with hint), 450-530 ns (burst groups) |
| Live `groupsim` (300 stations / 400 km, 1,200 aircraft, 30 % Mode S-only, 2 simulated min, planner as Selector, port 18603) | 920 k groups, 750 k not candidates (good ADS-B), 170 k clusters (136 k solve, 0.9 k solve_alt, 34 k track), 44 mixed = 0.026 % (no hints), 603 groups split; assembler 124 ns per copy of all groups incl. filtered ones; 429 MLAT candidates, 17 k solves refused by the per-aircraft rate (downgraded to track) |
| `BenchmarkModeAC` | 260 ns per reply (1,000 replies, 10 codes, 20 anchors) |
| `BenchmarkFlushDelay` | 53 ns, 0 allocs |
