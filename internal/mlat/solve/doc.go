// Package solve is the position solver of the AirView MLAT engine
// (docs/aggregator/DESIGN.md §9.4): it turns one cluster of network-time
// arrival times of a single Mode S transmission into a position, a
// transmit time, an honest covariance and a quality verdict.
//
// Everything here is implemented from the mathematics below (textbook
// least squares, Bancroft 1985, Baarda's reliability theory, Huber's
// M-estimation); no code of mlat-server, mlat-client, readsb or dump1090 was
// consulted.
//
// # Measurement model
//
// Station i at rᵢ (ECEF, height above the WGS-84 ellipsoid) stamps the
// transmission at network time tᵢ. With the transmit time τ and the
// aircraft position x (ECEF),
//
//	tᵢ = τ + ρᵢ(x)/c + nᵢ,   ρᵢ(x) = |x − rᵢ| + εᵢ(x)
//
// where εᵢ is the tropospheric excess path of mlat/geo (exponential
// refractivity, bent ray; tens of metres at low elevation). Working in
// metres, zᵢ = c·(tᵢ − t̄) (t̄ the mean TOA, for float64 precision) and
// b = c·(τ − t̄), the residual of TOA row i is
//
//	rᵢ = ρᵢ(x) + b − zᵢ.
//
// The noise n has the full covariance Σ (m×m): the clock filter's
// covariance of the stations' network-time conversions at the TOA (offset
// marginals and the cross-covariances of synced pairs, including any
// common-mode part) plus diag(jitter²); see CovFromClock. A common-mode
// clock error is absorbed by b; Σ must carry it so that the weights see
// the remaining (differential) information correctly.
//
// Optional rows:
//
//   - altitude: h(x) − h_meas with h the geodetic height of x, σ_h from
//     AltitudeFromBaro (Mode S pressure altitude + GNSS−baro field, else ISA
//   - EGM96 geoid). ∂h/∂x is the ellipsoid normal at x (exact for the
//     geodetic height).
//   - prior: x − x_p (3 rows, covariance P_p), a track prediction, fused
//     only when the other rows cannot fix the position (2 stations +
//     altitude, 3 stations without altitude) or on request.
//
// The rows are stacked into r(θ) with block-diagonal covariance
// R = blockdiag(Σ, σ_h², P_p).
//
// # Parameterisation and estimator
//
// θ = (e, n, u, b): an East-North-Up offset about the start point (well
// conditioned, metres throughout) and b. The estimate minimises the
// generalised least-squares cost
//
//	J(θ) = ½·r(θ)ᵀR⁻¹r(θ) = ½·‖L⁻¹r(θ)‖²,   R = LLᵀ (Cholesky),
//
// i.e. rows are whitened with L⁻¹ (linalg.Whiten) and then have unit
// weight. The Jacobian is analytic: a TOA row is ((uᵢ + ∇εᵢ)ᵀ·Rₑₙᵤ, 1) with
// uᵢ the unit vector from the station, the altitude row (nᵀ·Rₑₙᵤ, 0), a
// prior row the rotated unit axis.
//
// The excess εᵢ is expensive (a few hundred ns), so it is linearised,
// εᵢ(x) ≈ εᵢ(x₀) + ∇εᵢ(x₀)·(x − x₀), and re-linearised when the estimate
// moves more than Options.RelinM during the iterations and once more at
// the solution when it ended more than 100 m from x₀. The excess
// curvature is ~1e-7 /m, so the remaining model error is below a
// millimetre.
//
// Optimiser: Levenberg-Marquardt, (N + λ·diag N)·dx = −g with N = J̃ᵀJ̃ and
// g = J̃ᵀr̃ in whitened form, at most Options.MaxIter (10) iterations per
// start (twice that for coarse starts, plus a second round when a
// closed-form start has not converged). Targets outside the station
// polygon couple the vertical and b so strongly (condition numbers ~1e8)
// that λ must be able to fall far below diag N: a good step (gain ratio
// > 0.75) divides λ by 10, a poor one (< 0.25) doubles it, a rejected one
// multiplies it by ν (doubling). Convergence is declared on the Newton
// decrement gᵀN⁻¹g < 1e-10 (statistically nothing left to gain) or on a
// step below 0.1 mm. Nothing allocates: all workspace lives in the Solver.
//
// # Initialisation (multiple starts)
//
// Squaring |x − rᵢ| = zᵢ − b gives, with the Lorentz product
// ⟨a, b⟩ = a₁b₁ + a₂b₂ + a₃b₃ − a₄b₄, aᵢ = (rᵢ, zᵢ), y = (x, b),
//
//	⟨aᵢ, y⟩ = αᵢ + Λ,   αᵢ = ½⟨aᵢ, aᵢ⟩,   Λ = ½⟨y, y⟩.
//
// In order (DESIGN §9.4):
//
//  1. prior: the track prediction, when its horizontal σ < 2 km;
//  2. linear TOA (N ≥ 5): differencing against the best-timed station
//     removes Λ: (rᵢ − r₀)·x − (zᵢ − z₀)·b = αᵢ − α₀, solved by Householder
//     QR (αᵢ − α₀ is formed as ½((rᵢ−r₀)·(rᵢ+r₀) − (zᵢ−z₀)(zᵢ+z₀)) to avoid
//     cancellation);
//  3. Bancroft (N ≥ 4): By = α + Λ·1 with rows (rᵢ, −zᵢ) gives
//     y = q + Λp (q = B⁺α, p = B⁺1) and the quadratic
//     ⟨p,p⟩Λ² + 2(⟨p,q⟩ − 1)Λ + ⟨q,q⟩ = 0; both roots are candidates. ECEF
//     coordinates are used (a station-centred origin makes the four rows
//     linearly dependent);
//  4. 3 + altitude: the altitude surface is approximated by the sphere
//     |x| = R (geocentric radius at the candidate latitude plus the
//     height). With A the 3×3 matrix of rows rᵢ, x(b) = P + Qb + Sb²
//     (P = A⁻¹α + ½R²A⁻¹1, Q = A⁻¹z, S = −½A⁻¹1) and |x(b)|² = R² is a
//     quartic in b. Its real roots on the physical interval are found by a
//     sign-change scan, bisection and a golden-section search of sampled
//     extrema (two close roots or a near-tangent double root), each refined
//     with the radius at its own latitude. If that yields nothing
//     consistent, the grid of DESIGN §9.4 is searched: the TOA misfit with
//     b eliminated in closed form, rᵀAr − (1ᵀAr)²/(1ᵀA1) with A = Σ⁻¹, on a
//     5 km grid over ±400 km at the known height, and LM runs from the best
//     local minima;
//  5. the receiver centroid at the altitude (or 10 km);
//  6. fallback when no start gave a plausible solution: the same grid
//     (10 km) for any station count, at the altitude or at 9 and 3 km.
//
// The closed forms ignore the excess; they are therefore run twice, the
// second time with zᵢ corrected by the straight-path excess at the first
// answer (otherwise the DOP turns a 50 m excess into kilometres of seed
// error for far targets). A start that ends below ground is retried from
// its up/down mirror image (the stations are nearly coplanar, so the
// reflection through their plane fits almost as well).
//
// Hypotheses are deduplicated (20 m) and ranked by χ², the prior's
// Mahalanobis distance when a prior is known, and a weak reception prior
// (max range / 300 km)² that decides between equally good 3 + altitude or
// 4-station mirror solutions (an aircraft heard by all stations of a
// cluster is more likely near them). Other consistent hypotheses more
// than 1 km away are returned in Fix.Alt with FlagAmbiguous: the tracker
// resolves them by gating (DESIGN §9.5). Starts stop early when a
// consistent solution with DOF ≥ 2 is found.
//
// # Robust stage, RAIM and RANSAC
//
// Baarda's w-test of row i (the optimal single-outlier statistic for
// correlated observations), under the nominal R:
//
//	wᵢ = cᵢᵀR⁻¹r / sqrt(cᵢᵀR⁻¹Q_rR⁻¹cᵢ),   Q_r = R − J(JᵀR⁻¹J)⁻¹Jᵀ,
//
// computed in whitened form with mᵢ = L⁻¹cᵢ (column i of L⁻¹):
// wᵢ = mᵢᵀr̃ / sqrt(‖mᵢ‖² − (J̃ᵀmᵢ)ᵀN⁻¹(J̃ᵀmᵢ)). For a diagonal R it is the
// studentised residual rᵢ/(σᵢ·sqrt(1 − hᵢᵢ)). The denominator is the row's
// redundancy; Fix.MDB reports the minimal detectable bias 4.13/sqrt(den)
// (two-sided α = 1e-3, power 80 %).
//
//   - Huber IRLS (DOF ≥ 2): weights ωᵢ = min(1, k/|wᵢ|), k = 1.345, applied
//     by inflating row i's variance row-wise, R' = SRS with S = diag(1/√ωᵢ).
//     Since S is diagonal, its factor is simply S·L (no new Cholesky), and
//     correlations are kept. At most five passes.
//   - RAIM: the weighted SSE χ² = ‖L⁻¹r‖² is tested against χ²(DOF) at
//     PFA = 1e-3 (Chi2Quantile: regularised incomplete gamma, series and
//     continued fraction). χ² is χ²-distributed only at the minimiser of the
//     nominal cost, so the test is evaluated at the GLS solution (the Huber
//     M-estimate is restored afterwards when the test passes). On failure a
//     row is excluded while DOF ≥ 1 remains: the up to three rows with the
//     largest |w| are each tried (excluded and re-solved) and the one that
//     leaves the best-fitting remainder is excluded for good; with little
//     redundancy the largest |w| alone can point at an innocent row.
//   - RANSAC, when RAIM has failed twice and N ≥ 6: minimal subsets of 4
//     TOA rows (plus the altitude row), Bancroft + LM per subset, consensus
//     |residual| ≤ 3σ over all stations; then GLS on the inliers and back to
//     RAIM.
//
// # Covariance, grade and rejection
//
// The Cramér-Rao bound of θ is (J̃ᵀJ̃)⁻¹ = (JᵀR⁻¹J)⁻¹ with the nominal R at
// the solution; its position block is rotated from the start frame to
// ECEF and to the ENU frame at the fix (Fix.CRLBENU) and scaled by
// max(1, χ²/DOF) (Fix.CovENU, CovECEF). The reported σ_h is
// sqrt(σE² + σN²) and the grade model.GradeFor(σ_h, DOF). DOPs are the
// unit-weight geometry figures of the used stations. PredictCRLB gives the
// same bound for a hypothetical target (CRLB maps, siting advice).
//
// A fix is rejected (Fix.Reject) when σ_h > 2 km, its height disagrees with
// the altitude by more than 4σ, it is outside the 4/3-earth radio horizon
// of a used station, the implied speed from the last position exceeds
// max(600 kt, 1.25·Mach·a(h)), its transmit time disagrees with the
// latency-corrected host arrival by more than 50 ms, it is implausible
// (height, range), LM did not converge, or RAIM failed grossly.
//
// # Cost (H-track, §13.2 budget ≤ 20 µs per 6-station cluster)
//
// The exact excess model (geo.Atmosphere.RangeGrad, ~0.4 µs per station)
// was linearised about three times per solve (at the seed, on the way when
// LM moved > RelinM, at the solution), 40 % of a 6-station solve. Four
// changes, none of which moves an accepted fix by more than 0.06σ
// (TestCoarsePhaseMatchesExact, every benchmark shape against the exact-only
// path):
//
//   - coarse phase: while an over-determined problem (DOF >= 2) travels from
//     a seed, the excess is linearised with the closed-form straight-path
//     model (geo.Atmosphere.StraightExcess and its analytic gradient, two
//     exponentials per station); once LM converges on it the exact model is
//     linearised at that point with a fresh iteration budget, and a start
//     that does not converge finishes its second round on the exact model.
//     Minimal problems (DOF <= 1) stay exact throughout: their two branches
//     are close in cost and the coarse path could end in the other one;
//   - a closed-form seed below the ground starts from its up/down mirror
//     straight away (over-determined problems; it used to converge below
//     ground first and be mirrored after: one LM run saved in about one
//     case in eight);
//   - the fallback grid (0.4 ms) is skipped when a plausible, visible start
//     fits (χ² within the RAIM threshold) but did not formally converge in
//     the flat vertical/transmit-time valley of far-out targets: the grid
//     only found the same valley again;
//   - the w-tests are memoised: the Huber pass without a weight change, the
//     RAIM test at the GLS solution and finish asked for the same three
//     times.
//
// Measured (BenchmarkSolve*, 64 geometries each, median of 5, while other
// work packages loaded the machine): 6 stations 15.9 µs (was 28-30),
// 6 + alt 13.2 (17.5), 6 SDR + alt 14.5 (21), 4 stations 25 (70),
// 4 + alt 16.5, 10 + alt 19.5 (25), 20 + alt 38.6 (56), 3 + alt 20.0 (20),
// 6 + alt with a fault 68-70 (85), 10 + alt with a fault 69 (96). Zero
// allocations per Solve (TestNoAllocs).
//
// Fix.Alt carries each alternative's own CRLB covariance (Hypothesis
// CovECEF): the far branch of a 3+alt pair is often ten times less certain
// than the near one, and the tracker must not start or update it with the
// chosen branch's covariance.
package solve
