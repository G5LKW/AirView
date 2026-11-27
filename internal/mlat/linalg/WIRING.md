# internal/mlat/linalg: wiring guide

`import "airview/internal/mlat/linalg"` (stdlib only). Row-major `[]float64` with explicit sizes; callers pass
destination and workspace slices, so nothing allocates on hot paths (`TestNoAllocs`). Numerical failures are
errors (`ErrNotPositiveDefinite`, `ErrRankDeficient`, `ErrSingular`); dimension mistakes panic.

## Clock filter (§9.2.3): information update per epoch

```go
acc := linalg.NewAccum(n)                     // once per component size
acc.Reset()
acc.Add(idx, h, m, rinv, nu)                  // per group: J += HᵀR⁻¹H, g += HᵀR⁻¹ν on the touched states only
acc.AddScaled(s, idx, h, m, rinv, nu)         // influence cap (§9.2.4)
acc.AddDiagPrior(i, info, offset)             // gauges / soft constraints
// P⁺ = (P⁻⁻¹ + J)⁻¹ ; x⁺ = x⁻ + P⁺ g
linalg.SPDInverse(Pm, n, work)                // Pm := P⁻⁻¹ (in place)
for i := range Pm { Pm[i] += acc.J[i] }
linalg.SPDInverse(Pm, n, work)                // Pm := P⁺
linalg.MatVec(dx, Pm, acc.G, n, n)
```

Measured on the dev box (i9-13900K, one core, Go 1.27): `SPDInverse` 0.70 ms at n = 200, 16 ms at 600, 26 ms at
700; `Cholesky` 5.4 ms at 600. The two inverses above therefore cost ~51 ms at n = 700 here; a slower VPS core
may approach the 80 ms gate. Cheaper alternatives the clock WP can choose: keep the information matrix and
only factor it once per epoch (`Cholesky` + `CholSolve` for the state, `CholToInverse` only when a snapshot is
published), or use the covariance-form Kalman update when the epoch has fewer rows than states. Partitioning
(§20.2, `MaxComponentStations`) bounds n.

## Solver (§9.4) and tracker (§9.5)

- Full covariance weighting: factor Σ (`Cholesky`), then `Whiten(L, m, r, J, n)`; the whitened rows have unit
  weight. `MahalanobisSq(L, m, v, work)` for NIS/RAIM tests.
- LM: `var nrm linalg.Normal; nrm.Reset(4); nrm.AddRow(j, r, w)`; `pred, _ := nrm.Step(lm.Lambda, dmin, dx)`;
  `lm.Update(linalg.GainRatio(cost, newCost, pred))`; `linalg.Converged(dx, x, tol)`; CRLB via
  `nrm.Covariance(out)` (scale by max(1, χ²/DOF)). One 10-row, 4-parameter iteration: ~200 ns.
- Least squares without squaring the condition number (linear TOA init, Bancroft): `QR`, `QRSolve`,
  `QRCovariance`, `QRRank`. `LUSolve` for small general systems.
- Fixed sizes `Mat3/Mat4/Mat6/Mat9` + `Vec3/4/6/9`: value types with `Mul`, `MulVec`, `T`, `Quad`, `Congruence`
  (F·P·Fᵀ), `Chol`, `InvSPD`, `SolveSPD`, `Solve`. `Mat4.InvSPD` 78 ns, `Mat9.InvSPD` 330 ns.
- Covariance helpers: `Congruence`, `Symmetrize`, `Submatrix` (marginals), `ErrorEllipse(ee, en, nn)`
  (1σ axes + major-axis azimuth for map ellipses), `HorizontalSigma`.
