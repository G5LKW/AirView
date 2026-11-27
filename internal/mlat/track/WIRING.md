# internal/mlat/track: wiring guide

`import "airview/internal/mlat/track"` (stdlib + `mlat/geo`, `mlat/linalg`, `model`). The IMM raw-TOA tracker of
DESIGN §9.5. Maths, lifecycle rules and measured numbers: `doc.go`.

## Construction

```go
tr := track.NewSharded(track.Params{Atmosphere: atm}, 0) // 0 = 4×GOMAXPROCS shards, concurrency-safe
// or track.New(p) for a single goroutine (not safe for concurrent use)
```

Zero `Params` fields take the defaults (`DefaultParams`, `Normalize` clamps). Replace the atmosphere when k_atm/N0
change by building a new tracker only at start-up; it is read on every refresh of the per-station refraction cache.

## Feeding it (engine, WP-M)

| Input | Call | Notes |
|---|---|---|
| solver fix (`solve.Fix` OK, or `FlagAmbiguous` with `Alt`) | `tr.Fix(&track.Fix{ICAO, TNs, Hyp: {{ECEF, Cov}, {alt ECEF, alt Cov}}, NHyp, NStations, DOF}, &est)` | starts tracks; `NHyp = 2` for the two 3+alt branches. **Give the alternative its own covariance** (`solve.Hypothesis.CovECEF`): the far branch is often ten times less certain, and with the chosen branch's covariance its hypothesis gates its own later fixes away. `track.FixFromMLAT` converts a `model.MLATFix`. |
| every cluster (incl. `UseTrack` 2-station ones and late re-emissions) | `tr.Cluster(&track.Cluster{ICAO, T0Ns, TOAs, Cov, AltHAE, AltSigma}, &est)` | TOAs in network time, seconds relative to `T0Ns` (pick T0 near the cluster so float64 keeps sub-ps resolution). |
| TOA σ | `TOA.Sigma` = per-transmission jitter/quantisation (learned station jitter); `TOA.SyncSigma` = the clock snapshot's marginal σ_θ of the station | **Do not fold σ_θ into `Sigma`/`Cov`**: the tracker treats it as correlated over `SyncTauS` (doc.go §5.3); folded in, the track σ is optimistic by ×3-5 on simnet. `Cluster.Cov` may carry the instantaneous cross-covariance (e.g. a common-mode term) of the per-transmission part. |
| altitude row | `Cluster.AltHAE/AltSigma` from `ref.Store.HAEFromBaro` (+ quantisation, + age × |v_z| when the altitude is older than the frame) | attach the latest Mode S altitude to frames without one (DF11): 2-station maintenance needs it. |
| DF17 TC19 / BDS 5,0 / BDS 6,0 + wind | `tr.Velocity(&track.Velocity{ICAO, TNs, East, North, SigmaH, Up, SigmaUp}, nil)` | local ENU ground velocity at the aircraft. A velocity every ≤ 5 s makes 2-station maintenance hold indefinitely. |
| Mode S altitude without a cluster | `tr.Altitude(&track.Altitude{ICAO, TNs, HAE, Sigma}, nil)` | optional |
| ADS-B takes over | `tr.Delete(icao)` | |

`Result` says what happened (`ResCreated`, `ResApplied`, `ResGated`, `ResNotConfirmed` for 2-station clusters of
unconfirmed tracks, `ResTooLate` beyond `MaxLagS` = 2.5 s, `ResNoTrack`, `ResIgnored`).

## Output

- On update: the `Estimate` passed to any call (Status `none` when the ICAO has no track). Publish only
  `StatusConfirmed`; `est.MLATFix()` gives `model.MLATFix` with `FixFlagTracked` (+ `FixFlagAmbiguous` while a second
  3+alt hypothesis is alive, + `FixFlagAltConstrained`), grade from σ_h and the recent DOF, NBucket from the last
  station count. `Vel` points into the Estimate: copy before reuse.
- 1 Hz: `tr.Sweep(nowNs, func(e *track.Estimate){ ... })` deletes coasted (60 s), unconfirmed (10 s) and σ_h > 5 km
  confirmed tracks (in ICAO order) and emits the prediction of every confirmed track to `nowNs` (1.4 µs per track). `emit` runs with the shard
  locked: do not call back into the tracker.
- Track prediction for the solver/grouper (`solve.Problem.Prior`, group hints): `tr.Predict(icao, tNs, &est)` →
  `est.ECEF`, `est.Cov` (§9.4 init step 1 when σ < 2 km).

## Behaviour notes (H-track)

- Competing hypotheses are ranked on likelihood × a ground-speed prior (doc.go §8); `Estimate.Ambiguous` stays set
  until the evidence decides.
- From 3 s after the last position-determining update, slow traffic (≤ 80 m/s, ramping to 0 at 160 m/s) gets a
  manoeuvre allowance in the process noise: 2-station σ is honest for turning GA and helicopters (doc.go §8).

## Mode A/C targets (phase 5, DESIGN §9.9; the engine does this, `internal/mlat/modeac.go`)

```go
ac := track.NewACTracker(track.ACParams{Track: track.Params{Atmosphere: atm}}) // zero = defaults; single goroutine
r := ac.Fix(code, &track.Fix{TNs, NHyp, Hyp, NStations, DOF})  // ≥ 4-station solver fix; may start a target (r.New)
r  = ac.Cluster(code, &track.Cluster{T0Ns, TOAs})            // 2-3 station TOAs; updates a shown target only
ok := ac.Hint(code, tNs, near, &est)                          // solver prior for a cluster of that code
ac.Sweep(nowNs, func(e *track.ACEstimate){ ... })             // 1 Hz: removal, code TTL, merge, crowding, emit shown
```

- `code` is the raw 13-pulse layout (`accode.go`; `ACFromSquawk`, `ACFromAltitudeFt`, `ACRawFromNibbles` convert).
- `ACEstimate`: `Estimate.ICAO` = synthetic identity (`ACIDFlag` set, `IsACID`), `Squawk/SquawkOK`, `AltFt/AltOK`,
  `Fixes`, `Clusters`, `FirstNs`, `Crowded`, `Grade` (σ_h + DOF, one class worse when crowded).
- `ACStats` counters for the admin view. Lifecycle defaults for SSR's bursty replies: confirm window 30 s, 3
  updates, coast 30 s. Every parameter of `ACParams` is documented in `modeac.go`; maths in `doc.go` §13.

## Cost (i9-13900K, one core)

Cluster update 4.5 µs (2-8 stations) … 13 µs (24), fix 3.7 µs, 0 allocs on every path once a track exists.
~4.7 KB per hypothesis (≤ 4 per ICAO, normally 1).
