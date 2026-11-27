# internal/mlat/solve: wiring guide

`import "airview/internal/mlat/solve"` (stdlib + mlat/geo, mlat/linalg, geoid, model). Position solver of DESIGN §9.4.
Maths: `doc.go`. One `*solve.Solver` per worker goroutine (not concurrency-safe, ~0.4 MB of workspace, `Solve`
allocates nothing).

## Per cluster (engine, WP-M)

```go
s := solve.NewSolver(solve.Options{Atmosphere: atm})   // once per worker; s.SetAtmosphere(a) when k_atm/N0 change

// TOAs: clock.Snapshot.Convert(qs, t, covNs2, ok) -> seconds since the clock origin + full covariance (ns²,
// without receiver jitter). Keep only ok rows and one time domain per problem.
for i := range rows {
    st[i] = solve.Station{Pos: stationECEF /* HAE */, TOA: t[i] - epochS, Sigma: jitterS[i], ID: int32(idx)}
}
// Σ in s²: clock covariance + diag(jitter²)  (or leave Cov nil for diag(Sigma²))
solve.CovFromClock(cov, m, jitterS, func(i, j int) (float64, bool) { return covNs2[i*m+j] * 1e-18, true })

pr := solve.Problem{EpochNs: epochUnixNs, Stations: st, Cov: cov}
// altitude row (Mode S altitude): from mlat/ref, then quantisation / age inflation
hae, sig, _ := refStore.HAEFromBaro(key, lat, lon, baroFt, tNs)
pr.Alt = solve.AltitudeFromHAE(hae, sig, solve.AltitudeInput{QuantFt: 25 /*or 100*/, AgeS: age, VRateMps: vr})
// or without ref: solve.AltitudeFromBaro(in, lat, lon, field) (EGM96 + ISA fallback)
pr.Prior = solve.Prior{Valid: true, Pos: trackPredECEF, Cov: trackCovECEF}  // seeds + resolves 3+alt branches
pr.Last = solve.Motion{Valid: true, Pos: lastFix, SigmaM: lastSigma, DtS: dt, Mach: mach}
pr.HostFirstS, pr.HostValid = firstCorrectedHostS-epochS, true

var f solve.Fix
if err := s.Solve(&pr, &f); err == nil && f.OK() {
    out := f.MLATFix(icao, pr.EpochNs)   // model.MLATFix: ECEF, Cov, NBucket, Grade, flags
}
```

- **Minimum:** 4 TOA, or 3 TOA + altitude, or 2 TOA + altitude/prior (the prior is then fused: `FlagPriorFused`,
  the result is a posterior for the tracker, DESIGN §9.5 "2-station maintenance"). `ErrTooFew` otherwise.
- **Ambiguity:** `FlagAmbiguous` + `f.Alt[:f.NAlt]` (3+alt, 4-station or 4+alt mirror). Give the tracker both and
  let gating choose; with a prior the solver already prefers the branch near it.
- **Reject reasons** (`f.Reject`): sigma, altitude, horizon, speed, hosttime, implausible, notconverged, raim.
  Grade D fixes (σ_h ≤ 2 km) are for track maintenance only (`model.Displayable`).
- **Quality / σ learning (WP-M quality):** `f.Resid[i]` (m, model − measurement, also for excluded stations),
  `f.WTest[i]` (Baarda w), `f.Weight[i]` (Huber), `f.MDB[i]` (minimal detectable bias, m), `f.Excluded` mask,
  `f.Chi2/f.DOF`, `f.Scale`.
- **CRLB maps / siting:** `solve.PredictCRLB(stationsECEF, sigmaS, targetECEF, altSigmaM, &atm)`.

## Costs (i9-13900K, one core; `go test -bench . ./internal/mlat/solve`)

| Cluster | µs/solve |
|---|---|
| 3 + alt | 20 |
| 4 + alt | 16 |
| 6 + alt (GPS) | 16 |
| 6 + alt (SDR σ) | 20 |
| 6, no altitude | 29 |
| 10 + alt | 24 |
| 20 + alt | 52 |
| 4, no altitude (DOF 0, all starts) | 67 |
| 6 + alt with a 2 µs fault (RAIM) | 81 |
| 10 + alt with a 2 µs fault | 91 |

0 allocations in every case.
