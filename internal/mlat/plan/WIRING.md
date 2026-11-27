# internal/mlat/plan: wiring notes (WP-GR, DESIGN §4.4, §9.1 mlat/plan, §9.3)

The MLAT planner: per-station syncSet / mlatSet (AVF INTEREST, mlat-client start/stop_sending),
per-aircraft station plans (greedy D-optimal), priorities for non-ADS-B aircraft, per-aircraft solve
rates and the global solve budget (`group.Selector`), Mode A/C participants. The maths is in `doc.go`.

## 1. Construction (WP-M)

```go
pl, err := plan.New(plan.Options{
    Config:    plan.DefaultConfig(),   // sync 15-30, mlatSet <= 400, 12 stations/aircraft (min 6), 4/2/1/0.5 Hz, 20k solves/s
    Stations:  stationAdapter,         // plan.Stations: Located+Pos (registry/survey), Synced/Anchor/SigmaNs (clock snapshot)
    Positions: trackAdapter,           // optional: MLAT track or ADS-B reference position + 1σ
    HoldOut:   quality.InHoldOut,      // optional: hold-out membership (DESIGN §9.6)
    Out:       plan.Fanout{plan.IngestOut{Ingest: ing}, plan.MLATCOut{C: mlatcServer}},
    Now:       ntp.Now,
})
go pl.Run(ctx)                         // Replan every ReplanEvery (5 s)
// per group, before the assembler (same goroutine): pl.ObserveGroup(g)
// group.Options.Selector = pl
// reference track verdicts: pl.NoteADSB(key, now, nacp >= 8 && nic >= 7)
// ADS-B verification failures: pl.MarkSuspect(key, now + 10 min)
// mlat-client seen/lost: pl.NoteHearer(key, st, now)
```

`Positions.Approx` is called with a planner shard lock held: it must not call back into the planner.
`Out.Interest` is called from the Replan goroutine; the `Delta` slices are reused (the adapters copy).

## 2. Outputs

- `IngestOut` → `ingest.SetInterest` (the ingest keeps the sets and sends INTEREST while the station is
  selective). The first message to a station is a reset with its whole sets.
- `MLATCOut` → `SendingController.StartSending/StopSending` with the union of both sets (to be
  implemented by `internal/ingest/mlatc`, WP-B3).
- `Summary()` (admin): last replan's counts, candidates per class, admitted/denied per class.
  `Sets(st)`, `AircraftPlan(key)` for the admin MLAT console. `ModeACEligible(st)` for group.ModeAC.

## 3. Tests and numbers

```
go test ./internal/mlat/plan
go test ./internal/mlat/plan -run '^$' -bench .
```

Measured on the dev machine (i9-13900K, Windows 11, Go 1.27.3; noisy, other agents running):

| Check | Result |
|---|---|
| Acceptance "plan sizes respected" (simnet geometry, 150 stations, 366 aircraft, 1/3 Mode S-only, 4 replans) | every syncSet ≤ 30 and ≥ 15 where ≥ 30 sync aircraft are heard (p10/p50/p90 = 30/30/30); mlatSet ≤ 400 (1/3/18); every plan ≤ 12 stations and ≥ 6 when ≥ 6 located hearers; non-reset deltas ≤ MaxChurn; replaying the deltas reproduces the sets and the mlat-client union exactly; an unchanged world gives no deltas; silent stations are emptied |
| D-optimal station choice | horizontal CRLB of the planned subset / all hearers: mean 1.31 (random subset of the same size: 16.9) |
| Admission | acquire 43 of 1,000 asks in 10 s (4/s + burst 3), tracked 23 (2/s); global 100/s saturated by 200 suspects: acquisitions unaffected, suspects capped above the 50 % reserve |
| `BenchmarkReplan1000x10k` (1,000 stations over a 1,500 km disc, 10,000 aircraft, 19 hearers/aircraft) | 70 ms per replan (every 5 s: 1.4 % of one core), 30 sync and 38 MLAT aircraft per station |
| `BenchmarkObserveGroup` / `Admit` / `Candidate` | 41 / 23 / 12 ns, 0 allocs |
| Live `groupsim` (300 stations, 1,200 aircraft, dense 400 km disc) | replan 80-150 ms (dense: many more hearers per aircraft than the 1,000-station benchmark), 6.8 k sync / 2.4 k MLAT memberships, 227 deltas in the last replan, 24 aircraft underserved (< 4 located hearers) |
