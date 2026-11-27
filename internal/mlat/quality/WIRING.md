# internal/mlat/quality — wiring and validated numbers

Maths and design decisions: `doc.go` and the file headers (`crlb.go`, `katm.go`,
`spoof.go`, `verify.go`, `latcal.go`). This file is the contract for the engine
(`internal/mlat`, root package) and the numbers the simnet validation measured.
Everything here is pure computation: no goroutines run after a call returns, no
I/O, no configuration reading (the engine maps its options onto the parameter
structs; zero fields always mean the documented defaults).

Phase-2 pieces (`HoldOut`, `ErrorStats`, `Cells`, `Stations`, `VerifyTDOA`,
`Verifier`, `KAtm.AddFix`) are wired already (`internal/mlat/WIRING.md`). Phase 4
(WP-Q2) adds the four features below. Wiring requests for the engine are in
`docs/aggregator/HOOKS.md` ("WP-Q2-quality -> internal/mlat").

## 1. CRLB admin map

```go
ix := quality.NewCRLBIndex(stations, quality.CRLBParams{Atmosphere: &atm, Workers: 4})
cells := ix.Map()                          // grid (0.25°, 3,000 ft / FL100 / FL300)
sum   := quality.SummarizeCRLB(cells)      // per altitude: P50/P95, cells <= 50/100/250/1000 m
s, n, ok := ix.SigmaAt(lat, lon, haeM)     // one point (siting advice, "expected here")
```

- `CRLBStation{Pos (ECEF HAE), SigmaS}`: SigmaS = √(σθ² + jitter²) of a synced
  station, else the class prior (the engine's `Engine.CRLBMap` already builds this).
- Station choice per target mirrors the planner (`internal/mlat/plan`): of the 64
  nearest stations inside the 4/3-earth radio horizon and `MaxRangeM` (400 km),
  `MaxStations` (12) by greedy D-optimal design. `Nearest: true` restores the old
  nearest-first choice (not recommended, see numbers).
- `CRLBIndex` is immutable and safe for concurrent use; build it on station
  changes (located / σ moved by > 20 %), cache `Map()`; never per request.
- `CRLBMap(sts, p)` is kept as the one-call form (`NewCRLBIndex(sts, p).Map()`).

**Validation** (`TestCRLBSimnet`, seed 21: 60 stations, simnet default device mix,
250 aircraft, 3 min, 100,658 fixes from up to 12 planner-chosen receiving stations,
altitude row σ 30 m):

| | value | expected |
|---|---|---|
| r ≤ σh of the receiving stations' CRLB | 66 % (gate 55-75 %) | 63.2 % (Rayleigh) |
| r ≤ 2σh | 94 % (gate ≥ 90 %) | 98.2 % |
| fixes without a map value | 0 % | |

Map value at the true position vs the real error (median r / σ_map; 0.67-0.83 is
the range for an elongated-to-circular Gaussian):

| σ_map band | fixes | median r | median r / σ_map |
|---|---|---|---|
| < 20 m | 42,840 | 6.2 m | 0.93 |
| 20-50 m | 14,831 | 22.7 m | 0.66 |
| 50-100 m | 16,825 | 48.2 m | 0.68 |
| 100-250 m | 8,790 | 117.6 m | 0.79 |
| > 250 m | 17,372 | 302.6 m | 0.62 |

The median error grows monotonically with the map band (the map ranks coverage
correctly). With the old nearest-12 choice the map was 1.5-2.5× pessimistic
outside the network (median r/σ_map 0.49 at 100-250 m, 0.39 above): the planner
reaches for distant stations that open the angles.

Cost (`BenchmarkCRLBMap`, 1,000 stations over a 1,000 km radius, defaults; i9-13900K):
0.37 s serial, 0.06 s with 8 workers (29,077 cells).

## 2. k_atm regression (projected estimator)

```go
// per accepted, anchored fix with >= 2 redundant rows (sample 1 in 4):
k.AddSolved(nowNs, &quality.KAtmFix{
    Target: f.ECEF, Stations: usedPos, ResidM: usedResidM, SigmaM: usedSigmaM,
    AltSigmaM: altSigma /*0 = no altitude row*/, AltResidM: f.AltResid, ModelK: atm.K,
}, atm)
k.Report()        // admin: method, k, σ, hours, χ²/hour, Ready, recent hourly fits
k.Estimate(model) // (k, σ, hours, ok) — projected when present, else demeaned
```

- Only the solver's *used* TOA rows (excluded rows carry nothing of the solution's
  projection); residuals model − measurement in metres, σ in metres.
- `ModelK` = the k the residuals were produced with: fixes solved with different k
  (after an apply mid-hour) mix correctly.
- Apply only when `Report().Ready` (σ ≤ 0.02, ≥ 2 hours, χ²/hour ≤ 4) and
  `ApplyKAtm` is on: `atm.K = Estimate(...)` into solver + clock filter.
- `AddFix` (demeaned, phase 2) stays for callers without geometry; it is attenuated
  toward the model value (numbers below), so the engine should move to `AddSolved`.

**Validation** (`TestKAtmSimnet`, seed 3: 40 GPS/Beast stations, 250 aircraft,
4 min, every 6th transmission heard by ≥ 6 stations, ~69,000 fixes per case).
`k_eff` is the k that makes geo's ray-traced model match simnet's own
(straight-chord, four-node) excess: simnet's excess is ≈ 3.5 % above geo's, so
k_eff ≈ 1.03·k*.

| k* (simnet) | model k | k_eff | projected | error vs k_eff | demeaned error vs k_eff |
|---|---|---|---|---|---|
| 1.00 | 1.00 | 1.0335 | 1.0356 ± 0.0020 | +0.0021 | −0.0122 |
| 1.15 | 1.00 | 1.1865 | 1.1836 ± 0.0020 | −0.0029 | −0.0825 |
| 0.88 | 1.00 | 0.9053 | 0.9084 ± 0.0020 | +0.0031 | +0.0405 |
| 1.10 | 1.20 | 1.1413 | 1.1462 ± 0.0020 | +0.0049 | +0.0272 |

Gates: |projected − k_eff| ≤ 0.01, |projected − k*| ≤ 0.05, projected better than
demeaned whenever the model is off by > 0.03. Synthetic exact-geometry check
(`TestKAtmProjectedSynthetic`): k* 1.120 → 1.1164 ± 0.0020.

Rows per hour are capped at 200,000 (6.4 MB); at 1,000 feeders the cap is reached
within minutes and the rest of the hour is dropped (the hour's fit then describes
its first minutes; the 6-hour combination still averages hours).

## 3. ADS-B position verification (spoof detection)

Two checks, one verdict stream per aircraft into a `Verifier`:

```go
// raw arrivals of one DF17 position message (>= 2 synced stations):
r := quality.Verify(repECEF, quality.RefCovECEF(rep, vel, nacp, 0.15, 30),
                    toas, clockCov /*or nil*/, sigmaS, rangeFn, quality.DefaultVerifyParams())
// an independent MLAT fix of the same message (or a track prediction at the instant):
if quality.MLATComparable(&f) {
    r2 := quality.CompareMLAT(repECEF, repCov, f.ECEF, f.CovECEF, quality.DefaultCompareParams())
}
v := quality.NewVerifierPhase4()   // OnlyConfirmedResets
if v.Record(icao, nowNs, r.Verdict) { planner.MarkSuspect(icao, ...); /* adsbSuspect flag */ }
suspect, verified := v.State(icao, nowNs)
```

- `Verify` (phase 4): χ² of the m−1 TDOAs with the full covariance, leave-one-out
  attribution (`Culprit`: a single bad station is blamed, not the aircraft), and the
  minimal detectable offset (`MDOM`): it confirms only when it could have seen a
  `MaxConfirmMDOM` (1 km) offset. `VerifyTDOA` (phase 2) remains for old callers.
- `Culprit >= 0` with `VerdictNone`: count it against that station (bias / timing),
  not the aircraft.
- `Verifier` phase 4: 3 contradictions within 60 s → suspect; clearing needs
  confirmations that outnumber contradictions in a fresh window (consistent checks
  do not clear).

**Validation** (`TestSpoofSimnet`, seed 11: 60 stations default mix, 250 aircraft,
3 min; offset spoofers = every real aircraft re-reported D metres off in a fixed
direction; 6 ground transmitters broadcasting 12 fake aircraft at FL350 30-150 km
away). Phase-4 defaults:

| offset | TDOA contradicted / msg | TDOA aircraft suspect | MLAT contradicted / msg | MLAT aircraft suspect | median time to suspect |
|---|---|---|---|---|---|
| 0 (honest) | 0.224 % | **0 of 191** | 0.146 % | **0 of 188** | — |
| 150 m | 18.7 % | 42.9 % | 0.4 % | 2.9 % | 5.4 / 65.9 s |
| 300 m | 60.1 % | 80.6 % | 60.3 % | 58.6 % | 2.2 / 2.6 s |
| 500 m | 72.8 % | 86.4 % | 79.4 % | 75.5 % | 1.9 / 1.6 s |
| 1 km | 83.7 % | 99.5 % | 97.9 % | 91.0 % | 1.6 / 1.1 s |
| 2 km | 97.9 % | 99.5 % | 99.8 % | 98.9 % | 1.0 / 1.0 s |
| 5 km | 99.1 % | 100 % | 99.99 % | 99.5 % | 1.0 / 1.0 s |
| ground spoofer | 100 % | **12 of 12** | 100 % | 9 of 11 | 1.0 / 2.5 s |

Phase-2 parameters for comparison: honest per-message contradiction rate 1.31 %
(TDOA), i.e. 6× higher, at the same detection power from 1 km. Gates: no honest
aircraft suspect, honest per-message contradictions < 0.5 %, ≥ 95 % caught by TDOA
from 1 km, ≥ 85 % (1 km) / ≥ 95 % (≥ 2 km) by MLAT, all ground spoofers by TDOA.
Offsets below ~300 m are inside what NACp-8/9 positions plus 0-300 ms latency
allow and are deliberately not flagged (`MinBiasM` 300 m).

## 4. Airframe-latency calibration behind a hold-out A/B test

```go
cal := quality.NewLatencyCal(quality.LatencyCalParams{Salt: holdoutSalt ^ 0x5a})
// for every accepted MLAT fix (MLATComparable) of an aircraft with a usable
// ADS-B reference at the fix instant:
in := hold.In(icao, now)
latS, sigS, _ := cal.ForReference(icao, in)       // what the reference applies
s := quality.LatencySample{Key: icao, NowNs: now,
    AlongM: (fix − ref)·v̂_h, SigmaAlongM: √(σ_fix,along² + σ_NACp²),
    SpeedMps: |v_h|, AppliedS: latS, NACp: nacp}
if in { cal.Score(s) } else { cal.Learn(s) }
// keep the reference store in step (only when the value changed):
refStore.SetLatency(icao, latS, sigS)
// once a minute:
rep := cal.Decide(now)            // admin: arms, gain, z, decision, enabled
```

- Replaces `LatencyFeedback` (which let hold-out fixes calibrate their own
  reference). Hold-out aircraft never teach; they score.
- `ForReference(key, inHoldOut)`: in the hold-out, treated airframes get their
  calibrated latency and control airframes the prior; outside it, calibrated
  latencies apply only after the A/B enabled them. When the result changes for an
  airframe, push it with `ref.Store.SetLatency` (it re-tags positions when the
  change exceeds `RetagS`).
- Learning and scoring use only NACp ≥ 8 and precise samples (σₛ ≤ 0.15 s to
  learn, σ_a ≤ 50 m to score; weighted 1/σ²).
- Enable: each arm ≥ 500 samples from ≥ 20 airframes, gain ≥ 5 %, z ≥ 3. Disable:
  gain < 0 and z ≤ −2. The arms keep running after the decision.

**Validation** (`TestLatencyCalSimnet`: 40 stations default mix, 400 aircraft,
15 min, every airborne position squitter heard by ≥ 5 stations solved as a
snapshot MLAT fix; hold-out 25 %, NACp ≥ 8, rotated every 3 min; Decide every
minute):

| | simnet latency 0-300 ms (seed 41) | no latency (seed 43, negative control) |
|---|---|---|
| A/B decision | **enabled at 600 s**; gain 0.91, z 5.1 | **never enabled**; gain −0.94, z −2.5 |
| MLAT-scored RMS along-track, control / treated | 41.7 / 12.8 m | 14.3 / 19.9 m |
| truth-scored hold-out reference error, control / treated | 41.5 / 14.7 m | 11.0 / 14.3 m |
| network references vs truth (last 2 min), prior / calibrated | 37.0 / **11.4 m** | 12.5 / 13.4 m |
| calibrated airframes; abs(learned − true latency) P50 / P95 | 136; 30 / 106 ms | 115; 30 / 102 ms |

The truth confirms what the MLAT-scored A/B measured (control 41.5 vs 41.7 m,
treated 14.7 vs 12.8 m). The learned latency also absorbs the airframe's slowly
wandering NACp error (Gauss-Markov, τ 60 s), which is why its distance to the
true latency (30 ms P50) is larger than the reference improvement suggests:
it is the *reference* that gets better. Without latency the frozen estimates go
stale and the treated arm is worse: the gate keeps calibration off, as intended.
Before the precision/NACp gates and weighting (first implementation) the run did
not reach a decision in 12 minutes (MLAT along-track RMS 215 m dominated both arms).

## Benchmarks (i9-13900K, `go test -bench . -run XXX`)

| operation | measured | budget (1,000 feeders, 10,000 aircraft) |
|---|---|---|
| `KAtm.AddSolved`, 8 rows | 5.3 µs, 0 allocs | ≤ 10 µs (1 fix in 4 sampled) |
| `CompareMLAT` | 0.21 µs, 0 allocs | ≤ 1 µs |
| `Verify`, 8 stations | 3.8 µs, 0 allocs | ≤ 25 µs (per aircraft every 5 s) |
| `LatencyCal.Learn` / `Score` | 33 / 41 ns, 0 allocs | ≤ 0.3 µs |
| `LatencyCal.ForReference` | 22 ns | ≤ 0.1 µs |
| `LatencyCal.Decide`, 10,000 airframes | 1.2 ms | once a minute |
| `CRLBMap`, 1,000 stations (29,077 cells), serial / 8 workers | 0.37 s / 0.06 s | off the request path (< 30 s) |

## Tests

`go test ./internal/mlat/quality` (≈ 4-5 min with the simnet runs; `-short` skips
them, < 2 s). Simnet tests: `TestCRLBSimnet`, `TestKAtmSimnet`, `TestSpoofSimnet`,
`TestLatencyCalSimnet`. Unit: `TestCRLBIndexMatchesMap`, `TestKAtmProjectedSynthetic`,
`TestKAtmCombineAndReady`, `TestCompareMLAT`, `TestVerifyPhase4`,
`TestLatencyCalLearn`, `TestLatencyCalArms`, `TestLatencyCalDecide` and the phase-2
tests.
