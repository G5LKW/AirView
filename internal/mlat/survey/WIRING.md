# internal/mlat/survey — wiring

Reverse-MLAT self-survey, declared-position check, MSL/HAE test and the
consent-gated job API (DESIGN §10.2-10.3, §19.3). Package doc: `doc.go`.
The engine wiring lives in `internal/engine` (owner WP-E / integrator); the
requests are filed in `docs/aggregator/HOOKS.md` ("WP-SU-survey -> ...").

## 1. Construction (aggregator mode, next to `mlat.New` in `internal/engine/mlatwire.go`)

```go
surv, err := survey.New(survey.Options{
    Config:     survey.ConfigFromModel(cfg.Aggregator.MLAT), // autoUseSurvey, autoUseMaxSigmaM, surveyIntervalH
    Clock:      eng.Filter(),     // *clock.Filter: Snapshot()
    Refs:       eng.Refs(),       // *ref.Store: TransmitInstant
    Atmosphere: eng.Atmosphere,   // same refraction at both ends
    Sink:       eng,              // mlat.SurveySink: SetSurvey (AutoUseSurvey is applied there)
    Declared:   <registry adapter>, // SetDeclaredSuspect -> LocStatus "suspect" (see §3)
    Notifier:   <owner notices>,  // SurveyFinished(Result): owner/admin data only
    Targets:    <planner adapter>,// SurveyTarget(st, active) (HOOKS: plan)
    Terrain:    <terrain.Source>, // optional DEM (cached tiles only); nil = declared/peer heights
    Now:        ntp.Now,
    Logger:     e.log.With("component", "mlat.survey"),
})
```

`mlat.Options.Survey = surv` makes the engine's residual-bias flag
(`RequestSurvey`) start a survey. `New` needs the engine first, and the
engine takes `Survey` at construction: use a small forwarding type
(`type surveyFwd struct{ s atomic.Pointer[survey.Surveyor] }`) or set it
after construction if the engine grows a setter.

## 2. The group feed

Put the survey **in front of the timing quarantine**: a station under
survey is usually one whose timing is not proven yet (it has no position
to prove it with).

```go
nc.trust = trust.New(eng, ...)
nc.mlatTap = newMLATTap(survey.Tee{Next: nc.trust, S: surv})
```

`Tee.Observe` hands every group to the survey and then to `Next`;
`ObserveLate` goes to `Next` only. `Surveyor.Observe` is lock-free and
costs ~40 ns per group while no target is in it (0 allocations). It uses
only `Timed` copies that are neither `MapOnly` (Beast 30004) nor
`SameStation`, of long Mode S frames; peers must be synced, located, not
under survey, not declared-suspect, within `MaxPeerSigmaNs`.

## 3. Stations (the registry sync loop, `syncMLATStations`)

For every approved station push `surv.SetStation(idx, survey.Station{...})`:

| field | from the registry |
|---|---|
| `Class` | `st.ClockCls` |
| `Timestamps` | the owner's consent: timestamps allowed (false for `--no-timestamps` feeders and owners who withheld timing, DESIGN §10.5) |
| `GNSS` | `st.GNSS.Valid` or `LocStatus == gnss` |
| `Confirmed` | `LocStatus == confirmed` |
| `Declared` | `st.Declared` with `DeclaredAltRef` (`msl`, `hae`, `ambiguous`) |
| `AGLM` | the declared antenna height above ground, if any |
| `Hint` | the Locate Me v2 estimate (owner view), if any |

and `surv.RemoveStation(idx)` for stations that are gone. Push on change
only (the struct is comparable apart from the pointers; compare the
pointed-to values). Withdrawing consent cancels a running job at once.

Results:

- `Sink.SetSurvey` (the engine) gets every valid result; the engine's
  `AutoUseSurvey` rule (stable, not suspect, σ_h ≤ 50 m, not rejected by
  the owner) decides, and its `PositionNotifier` tells the owner.
- `DeclaredFlagger.SetDeclaredSuspect(idx, true, d)`: set the registry's
  `LocStatus` to `suspect` and store the survey position as `Estimated`
  (`reg.SetEstimated`); `mlatStationInfo` already turns that into
  `mlat.StationInfo{Suspect, DiscrepancyM}`. `false`: back to `declared`.
- `Notifier.SurveyFinished(r)`: store `reg.AddEstimate(Estimate{Method:
  "survey", Lat/Lon/HAE: r.Solution.LLH, SigmaE/N/U: sqrt(CovENU[0/4/8]),
  N: r.Solution.Used, Status: "stable"|"unstable"|"suspect"})`, raise the
  owner notice (result, declared position suspect, MSL/HAE verdict
  `r.AltRef.Chosen`), and after a restart call `surv.Restore(idx, r)` with
  the persisted last result so the 6-hourly checks are not all run at once.

## 4. Driving

`go surv.Run(ctx)` (5 s ticks; evaluations on 2 background goroutines).
`surv.Close()` waits for running evaluations. Tests and replays use
`Options.Synchronous` and call `Tick()`.

## 5. Owner / admin API (internal/server, owner WP-H)

All routes owner- or admin-scoped (DESIGN §8.2: exact positions):

- `POST /api/me/stations/{sid}/survey` → `surv.Start(idx, survey.ReasonOwner)`;
  `ErrNoConsent` → 409 with the consent text C1, `ErrRateLimited` → 429,
  `ErrBusy` → 202 (queued).
- `POST /api/admin/stations/{sid}/survey` → `Start(idx, survey.ReasonAdmin)`;
  `DELETE` → `Cancel(idx)`.
- `GET .../survey` → `surv.Status(idx)`: running job (reason, started,
  observations), last results with `Solution.LLH`, σ_h, σ_v, 95 % ellipse
  (`Ellipse95A/B/AngleDeg`), stable/suspect (+ reasons), autoUse,
  declared distance and verdict, MSL/HAE verdict, next check.
- The owner's "do not use the survey" stays `eng.OverrideSurvey(idx, true)`.
- Admin stats: `surv.Stats()`.

## 6. Network bundle adjustment (phase 4, `Bundler`)

Not wired yet (HOOKS.md "WP-SU2-bundle -> internal/engine"). Beside the
surveyor in `internal/engine/surveywire.go`:

```go
bd, err := survey.NewBundler(survey.BundlerOptions{
    Config:     survey.BundleConfig{ /* from aggregator.mlat.survey.bundle; zero = default */ },
    Refs:       eng.Refs(),       // *ref.Store: RefPosition(key, df, t)
    Atmosphere: eng.Atmosphere,
    Declared:   <the same registry adapter as the survey>, // SetDeclaredSuspect
    Notifier:   <BundleFinished(BundleResult)>,             // admin view, owner notices
    Now:        ntp.Now,
    Logger:     e.log.With("component", "mlat.bundle"),
})
go bd.Run(ctx) // hourly (Config.Every), over the last Config.Span
```

- **Feed:** add it to the same tee as the survey, in front of the timing
  quarantine (`survey.Tee{Next: nc.trust, S: surv}` gains a second sink,
  or a small fan-out type in the engine). `Bundler.Observe` costs ~100 ns
  per group and 0 allocations while no subject is in focus; it keeps only
  consenting, timed, non-map-only copies of long frames with a tracker
  result and a reference position.
- **Stations:** in `syncSurveyStations`, next to `surv.SetStation`, call
  `bd.SetStation(idx, survey.MemberOf(in, anchor, sigma))` where `anchor`
  is the registry's GNSS or owner-confirmed position (ECEF, HAE; nil if
  none) and `sigma` its horizontal 1σ (0 = 5 m); `bd.RemoveStation(idx)`
  for stations that are gone. Stations with an anchor are never adjusted;
  they gauge the network and are tested (a contradicted anchor is
  flagged "anchor" in the result).
- **Results** (`BundleResult.Stations`, exact positions: owner/admin only):
  - `PriorReleased && Determined` on a declared station → the bundler
    calls `SetDeclaredSuspect(idx, true, PriorDistHM)` itself; a later run
    that confirms the declaration clears only its own flag.
  - `Determined` stations without an anchor: store as an estimate
    (`reg.AddEstimate(Estimate{Method: "bundle", Lat/Lon/HAE: LLH,
    SigmaE/N/U: sqrt(CovENU[0/4/8]), Status: "stable"|"outlier"})`). The
    bundle never sets a position for MLAT itself; auto-use stays the
    survey's rule (`AutoUseSurvey`, σ_h ≤ 50 m, not suspect), which an
    integrator may extend to bundle results with `Outlier == false`.
  - `Outlier` with `OutlierWhy` "noisy" / "rejects": an admin notice (a
    wrong clock class or a broken receiver).
- **Admin:** `bd.Stats()` (counters, last run) and `bd.Last()` (the last
  result) for `GET /api/admin/mlat/survey`.

Cost: one run at `MaxStations` 64 (30 min, 66k frames, 2.8M receptions)
takes ~27 s on 8 workers and allocates ~0.5 GB (transient); the frame
store holds at most `MaxFrames` 200k frames (~60 MB).

## 7. Configuration

`ConfigFromModel` reads `aggregator.mlat.autoUseSurvey`,
`autoUseMaxSigmaM`, `surveyIntervalH` (→ `CheckEvery`) and the
`aggregator.mlat.survey` block (`AIRVIEW_AGGREGATOR_MLAT_SURVEY_*`). The
solver internals keep their defaults (documented on `Config` and in
`doc.go`). `BundleConfig` has no file keys yet: HOOKS.md asks for an
`aggregator.mlat.survey.bundle` block (see "WP-SU2-bundle -> internal/model
+ internal/config"); every field's default and range is in
`BundleConfig.Normalize`.
