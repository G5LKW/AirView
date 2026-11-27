// Package quality holds the MLAT engine's self-validation and quality
// learning (DESIGN §9.6): the hold-out selection, the error statistics of
// hold-out positions (network percentiles, σ calibration, NEES, an error
// map per 1° cell × altitude band, the week-on-week P95 alarm), the
// per-station residual statistics (jitter learning, bias flags,
// contribution and outlier rates), the ADS-B position verification (TDOA
// and MLAT comparison, spoof detection), the k_atm regression, the CRLB
// map and the airframe-latency calibration with its hold-out A/B test. It
// starts no background goroutines (CRLBIndex.Map with Workers > 1 joins its
// own before returning); the engine (internal/mlat) feeds it. Engine
// contract and validated numbers: WIRING.md. Everything here is safe for
// concurrent use unless a type says otherwise.
//
// # Hold-out
//
// Selection is a pure function of (address, rotation index, salt):
//
//	selected ⇔ mix64(addr ⊕ salt ⊕ mix64(⌊t / Rotate⌋)) < fraction · 2⁶⁴
//
// (mix64 is the SplitMix64 finaliser), so all components agree without
// coordination and the set changes completely at every rotation. An
// aircraft is In the set while it is selected and announced NACp >= 9
// within the last minute.
//
// # Error statistics
//
// A Sample is MLAT minus reference in the local ENU frame at the
// reference. Distributions are kept in log-spaced histograms (0.5 m ·
// 1.05^k, 256 bins up to 125 km: quantiles within ±2.5 %), in one-minute
// slices for the last hour, hourly slices for 14 days and a total. For a
// consistent circular Gaussian error with per-axis σ the horizontal error
// r satisfies P(r <= σ_h) = 1 − e⁻¹ = 63.2 % and P(r <= 2σ_h) = 98.2 %
// with σ_h = sqrt(σE² + σN²) — the "1σ calibration" of DESIGN §13.2 (gate
// 60-75 %). NEES uses the MLAT and the reference covariance together:
// χ²(2) with 68.27 % below 2.279 and 95 % below 5.991. The cross-track
// error (perpendicular to the reference velocity) does not depend on the
// reference's unknown position latency.
//
// # Station learning
//
// For a used row the Baarda statistic wᵢ = rᵢ / σ(rᵢ) is N(0,1) when the
// row's variance V = J + C (jitter + clock) is right. With the robust
// scale s = median|w| / 0.6745 the variance that explains the residuals
// is s²V, so the jitter is J' = s²(J + C) − C, clamped to ×0.5…×4 of the
// class prior. A 10-minute bin is biased when its mean w̄ over n rows is
// significant (|w̄|·√n > 3) and relevant (|w̄| >= 0.5); BiasBins (6) in a
// row with the same sign flag the station for an hour (DESIGN: bias > 3σ
// for 1 h → survey).
//
// # ADS-B verification
//
// See VerifyTDOA: m arrivals give m−1 time differences against member 0,
// dᵢ = (tᵢ − t₀) − (ρᵢ(p) − ρ₀(p))/c, with covariance D·C·Dᵀ + G·Σp·Gᵀ/c²,
// G rows uᵢ − u₀ (unit vectors station → aircraft); χ² = dᵀS⁻¹d with m−1
// degrees of freedom.
// Phase 4 (Verify, CompareMLAT, Verifier with OnlyConfirmedResets) adds
// leave-one-out attribution (a single contradicting station is blamed, not
// the aircraft), the minimal detectable offset (a check confirms only when
// it could have seen a MaxConfirmMDOM offset) and the comparison with an
// independent MLAT fix of the same message, d² = Δᵀ(Σ_rep + Σ_mlat)⁻¹Δ ~
// χ²(2) on the horizontal difference Δ. Thresholds were validated on simnet
// with synthetic offset and ground spoofers (spoof_simnet_test.go; numbers
// in WIRING.md).
//
// # k_atm
//
// Projected estimator (KAtm.AddSolved, phase 4): the solution absorbs
// exactly the part of the signal in the column space of the fix's design
// matrix H, so the residual vector is r = P·((k − k*)·E + n) with
// P = I − H(HᵀWH)⁻¹HᵀW; regressing z = k·PE − r on x = PE gives k* itself
// without attenuation. Huber IRLS per hour with a per-fix cluster-robust
// σ; hours combined by inverse variance; Report.Ready gates an apply.
// The demeaned estimator (AddFix, phase 2) regresses on the per-fix
// demeaned excess and is attenuated toward the model value. Both are
// validated against simnet's true refractivity scale (katm_simnet_test.go).
//
// # CRLB map
//
// For a grid point and altitude, the stations whose 4/3-earth radio
// horizon (smooth earth) and range limit include the target (the 64
// nearest), MaxStations of them chosen by the planner's greedy D-optimal
// design, give the Fisher information Σ hᵢhᵢᵀ/σᵢ² (+ the altitude row) of
// the ENU position and the transmit time; σ_h = sqrt(P_EE + P_NN) of its
// inverse (solve.PredictCRLB). CRLBIndex answers point queries; the map is
// validated against the solver's real errors on simnet
// (crlb_simnet_test.go).
//
// # Airframe-latency calibration
//
// LatencyCal learns each airframe's ADS-B position latency from the
// along-track difference of MLAT fixes and the reference (aircraft outside
// the hold-out only) and applies it to the network's references only after
// a hold-out A/B test (keyed-hash arms, weighted along-track MSE,
// cluster-robust z) shows a significant gain; it switches itself off again
// on significant harm. See latcal.go and latcal_simnet_test.go.
package quality
