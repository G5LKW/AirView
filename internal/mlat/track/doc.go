// Package track is the MLAT tracker of the AirView aggregator (DESIGN.md
// §9.5): an interacting-multiple-model (IMM) filter that is updated directly
// with raw time-of-arrival (TOA) clusters, solver fixes, velocities and
// Mode S altitudes, keyed by ICAO address. It initiates, confirms, maintains
// (also from only two stations) and deletes tracks, resolves the two-branch
// ambiguity of 3-station + altitude fixes by gating and likelihood, and
// outputs position, velocity and covariance for the map and for MLATFix.
//
// Everything here is derived from textbook estimation theory (Kalman/IMM
// filtering, iterated EKF, Woodbury identity, Gauss-Markov noise); no code
// or text of other MLAT implementations was used.
//
// # 1. Use
//
//	tr := track.New(track.Params{})           // one goroutine, or
//	sh := track.NewSharded(track.Params{}, 0) // ICAO-sharded, concurrent
//	sh.Fix(&fix, &est)          // solver output (§9.4); starts tracks
//	sh.Cluster(&cluster, &est)  // every TOA cluster with m >= 2 stations
//	sh.Velocity(&vel, &est)     // DF17 TC19, BDS 5,0 / 6,0 (+ met wind)
//	sh.Altitude(&alt, &est)     // Mode S altitude as HAE
//	sh.Sweep(nowNs, emit)       // 1 Hz: deletes stale tracks, emits predictions
//	f := est.MLATFix()          // model.MLATFix with FixFlagTracked
//
// Times are network-time Unix nanoseconds (the clock snapshot's time base).
// A Cluster carries per-station TOAs in seconds relative to Cluster.T0Ns,
// the station ECEF positions, the per-transmission TOA σ (or a full
// covariance) and the slowly varying clock-sync σ (TOA.SyncSigma).
//
// # 2. State and frame
//
// Each hypothesis keeps, per motion model j ∈ {CV, CT}, a mean and
// covariance of
//
//	x = [e, n, u, vₑ, vₙ, vᵤ, ω]
//
// in a local east-north-up frame whose origin lies on the ellipsoid under
// the aircraft. The frame is moved under the aircraft when the horizontal
// offset exceeds RecentreM (5 km): with old/new rotations R₁, R₂ and
// origins O₁, O₂ (ENU = R(p − O)), M = R₂R₁ᵀ and d = R₂(O₁ − O₂), so
// p₂ = M·p₁ + d, v₂ = M·v₁, ω unchanged, P₂ = T·P₁·Tᵀ with
// T = diag(M, M, 1). Staying within 5 km of the tangent point keeps the
// flat-frame error of the dynamics below 2 m and 0.8 mrad.
//
// # 3. Motion models
//
// CV: p' = p + T·v, v' = v; the turn rate is carried passively.
//
// CT (coordinated turn, exact for constant ω): with s = sin ωT, c = cos ωT,
//
//	e' = e + (s/ω)·vₑ − ((1−c)/ω)·vₙ     vₑ' = c·vₑ − s·vₙ
//	n' = n + ((1−c)/ω)·vₑ + (s/ω)·vₙ     vₙ' = s·vₑ + c·vₙ
//
// whose Jacobian columns for ω are, with A = (Tcω − s)/ω² and
// B = (Tsω − (1−c))/ω²: ∂e'/∂ω = A·vₑ − B·vₙ, ∂n'/∂ω = B·vₑ + A·vₙ,
// ∂vₑ'/∂ω = −T(s·vₑ + c·vₙ), ∂vₙ'/∂ω = T(c·vₑ − s·vₙ). For |ωT| < 10⁻⁴ the
// series s/ω = T(1 − ω²T²/6), (1−c)/ω = ωT²/2·(1 − ω²T²/12),
// A = −ωT³/3, B = T²/2·(1 − ω²T²/4) are used (tested against the closed
// form and numeric differentiation).
//
// The vertical channel [u, vᵤ] is constant-velocity in both models with its
// own noise QVert. The turn rate is an Ornstein-Uhlenbeck process,
// ω' = e^(−T/τω)·ω, discrete variance QOmega·τω/2·(1 − e^(−2T/τω)): turns
// end, and a turn-rate that is not observed (2-station maintenance) stays
// bounded (stationary σ ≈ 1°/s by default).
//
// Process noise per horizontal axis is continuous white acceleration of
// PSD q: Q = q·[[|T|³/3, T|T|/2], [T|T|/2, |T|]] (q = QCV or QCT). The signed
// cross term makes the same formula valid backwards (§6).
//
// IMM interaction (Blom/Bar-Shalom): with Markov switching
// π_ij(T) = 1 − e^(−T/τᵢ) (i ≠ j; mean sojourn τ_CV = 300 s, τ_CT = 20 s),
// c̄ⱼ = Σᵢ π_ij μᵢ, μ_i|j = π_ij μᵢ/c̄ⱼ, x₀ⱼ = Σᵢ μ_i|j xᵢ,
// P₀ⱼ = Σᵢ μ_i|j (Pᵢ + (xᵢ − x₀ⱼ)(xᵢ − x₀ⱼ)ᵀ). After the update
// μⱼ ∝ c̄ⱼ Λⱼ (log-sum-exp, floored at 10⁻⁴) and the output is the moment-
// matched mixture x = Σ μⱼxⱼ, P = Σ μⱼ(Pⱼ + (xⱼ − x)(xⱼ − x)ᵀ).
//
// # 4. Measurement update
//
// TOA model. Station k at sₖ stamps tₖ = τ + ρₖ(p)/c + εₖ, where ρ is the
// optical range (geometric range plus tropospheric excess, mlat/geo). The
// unknown transmit time τ is eliminated by differencing against the
// reference member r (smallest σ): zₖ = c(tₖ − t_r) = ρₖ(p) − ρ_r(p) + c(εₖ −
// ε_r), m − 1 rows. With D the differencing matrix and Σ the TOA covariance,
// R = c²·DΣDᵀ, i.e. R_kl = c²(Σ_kl − Σ_kr − Σ_rl + Σ_rr); for diagonal Σ this
// is c²(diag σₖ² + σ_r²·11ᵀ). Differencing is exactly equivalent to
// estimating τ as a nuisance parameter with a flat prior, so nothing is
// lost. The Jacobian row is (∇ρₖ − ∇ρ_r) rotated into the frame
// (∂/∂e = R·∂/∂p_ECEF).
//
// Altitude row: z = HAE (Mode S altitude through the GNSS−baro field), h(x)
// = geodetic height of the frame point, ∂h/∂p = the ellipsoid normal at p
// (exact, so earth curvature in the frame never biases the vertical).
// Fixes (z = frame ENU of the solution, R = its covariance rotated into the
// frame) and velocities (z = vₑ, vₙ[, vᵤ]) are linear rows.
//
// Iterated EKF in information form. Let x̂, P be the predicted state. With
// D = diag(√Pᵢᵢ) the prior is scaled to unit diagonal (Pₛ = D⁻¹PD⁻¹) so
// the 7×7 Cholesky factorisations stay well conditioned with metres,
// metres per second and radians per second together. Each iteration
// linearises at xᵢ:
//
//	r̃ = L⁻¹(z − h(xᵢ) − H(x̂ − xᵢ)),  H̃ = L⁻¹H·D,   R = LLᵀ
//	A = Pₛ⁻¹ + H̃ᵀH̃,  b = H̃ᵀr̃,  δ = A⁻¹b,  xᵢ₊₁ = x̂ + D·δ
//
// (Gauss-Newton on the MAP cost; Iterations = 3, stopping at 5 cm). The
// cost is O(m²) for the whitening and O(7³) for the solve, independent of
// the cluster size, which is why the information form is used. The
// posterior is P⁺ = D·A⁻¹·D.
//
// Gate and likelihood without forming S = HPHᵀ + R (m×m): by Woodbury,
// NIS = νᵀS⁻¹ν = r̃ᵀr̃ − bᵀδ, and by the determinant lemma
// log det S = log det R + log det Pₛ + log det A (the D factors cancel).
// The model likelihood is log Λ = −½(NIS + log det S + m·log 2π).
// TestUpdateLinearMatchesKalman checks state, covariance, NIS and log Λ
// against the textbook covariance form.
//
// # 5. Measurement details
//
// 5.1 Refraction. The optical range costs ~380 ns per call; each
// hypothesis caches, per station, the excess ε₀ and its gradient g at the
// position p₀ where they were computed and uses ρ(p) = |p − s| + ε₀ +
// g·(p − p₀), refreshed after 2 km (error < 0.2 m at 2 km, tested). The
// cache holds MaxClusterMembers stations with LRU eviction.
//
// 5.2 Transmit instant. The update time is τ = T0 + t_r − ρ_r(p̂)/c with p̂
// the hypothesis' predicted position; a 1 km prediction error moves τ by
// 3 µs, i.e. millimetres of aircraft motion.
//
// 5.3 Correlated clock errors. The clock filter's residual error of a
// station (TOA.SyncSigma, σ_s) changes slowly (correlation time
// τ_s = SyncTauS, 15 s). Treated as white it would be averaged away by the
// 2-10 clusters per second of one aircraft, and the tracker would claim
// metres where tens of metres are true (seen on simnet: 2σ coverage 16 %).
// A first-order Gauss-Markov error of variance σ_s² sampled Δt apart has
// the same long-run information as white noise of variance σ_s²·2τ_s/Δt,
// so each station's sync variance enters R with that factor, Δt being the
// time since this hypothesis last used the station (factor 1 for a first
// or rare use). Cluster.Cov / TOA.Sigma carry only the per-transmission
// part (jitter, quantisation, instantaneous cross-covariance).
//
// # 6. Out-of-sequence measurements
//
// Late copies (§9.3: up to 2 s) arrive after the track has moved on. A
// measurement Δ < 0 seconds older than the state is applied by retrodiction
// at the current time: x_b = f(x̂, Δ), H_eff = H(x_b)·F(Δ),
// R_eff = R + H(x_b)·Q(Δ)·H(x_b)ᵀ, innovation z − h(x_b), one EKF step,
// state time unchanged. Without process noise this equals in-order
// processing exactly (TestOOSMExactWithoutProcessNoise); with it, it is
// the standard conservative one-step approximation. Older than MaxLagS
// (2.5 s): dropped.
//
// # 7. Gating and robustness
//
// A measurement is accepted when the smallest NIS of the two models is
// within the χ²(rows) quantile of GateProb (99.9 %, §9.5); χ² quantiles
// are computed from the regularised incomplete gamma function at New.
// When a cluster of a lone confirmed track fails the gate and keeps at
// least three members, the member whose TOA disagrees most with the
// prediction (residual against the median transmit time, in σ) is
// excluded and the update retried, up to MaxExclude times. Exclusion is
// never done while competing hypotheses exist: dropping the member that
// contradicts a wrong hypothesis would rescue it, and the likelihoods of
// hypotheses are only comparable on the same rows.
//
// # 8. Lifecycle and hypotheses
//
//   - Initiation: a fix for an unknown ICAO creates one tentative
//     hypothesis per branch (two for an ambiguous 3+alt fix) with the fix
//     covariance, zero velocity (σ 150 m/s horizontal, 15 m/s vertical) and
//     zero turn rate (σ 0.03 rad/s).
//   - Confirmation: ConfirmUpdates (2) position-determining updates — fixes
//     or clusters with ≥ 3 rows — within ConfirmWindowS (10 s) of creation
//     (§9.5 "2 consistent fixes within 10 s"; consistency is the gate).
//     Unconfirmed hypotheses die after the window.
//   - Maintenance: clusters with fewer than 3 rows (2 stations, ± altitude)
//     update confirmed hypotheses only.
//   - Ambiguity: every hypothesis of an ICAO processes the same
//     measurements; for a fix each picks the branch that gates best. Each
//     accumulates a score: log Λ of the IMM mixture when accepted, the
//     log-likelihood at the gate boundary when rejected, minus 20 for a
//     physically impossible state (horizontal speed above MaxSpeedMS by more
//     than 3σ, below −1000 m, or more than LOSMarginM below the 4/3-earth
//     radio horizon of a station that heard it). A hypothesis whose score
//     falls ln(HypLR) = ln 100 below the best is dropped (sequential
//     likelihood-ratio test). While several survive, the best is output with
//     Ambiguous (FixFlagAmbiguous) set.
//   - Kinematic prior (H-track): competing hypotheses are compared on
//     score + log-prior of their ground speed, −½z² with
//     z = max(0, v_min(h) − v, v − 300 m/s) / hypot(σ_v, 10 m/s), v_min
//     rising from 0 at 1500 m to 70 m/s at 4500 m HAE, σ_v the larger
//     horizontal velocity σ, capped at 12 nats. The far branch of a 3+alt
//     pair often moves at 10-60 m/s at FL150-FL350 (or above 350 m/s) while
//     its likelihood stays within 1-4 nats of the true branch for many
//     seconds. A lone track is never penalised. Same three stations only:
//     resolved within 10 s 36.9 % → 43.8 %, 30 s 60 %, 60 s 87 %; with a
//     fourth station on 20 % of the transmissions unchanged at 96.9 %.
//   - Re-acquisition: RespawnMisses (3) consecutive fixes that gate against
//     no hypothesis start competing hypotheses from the latest fix; they must
//     out-score the old track.
//   - Deletion: no accepted update for CoastS (60 s), or, for a confirmed
//     hypothesis, horizontal σ above MaxSigmaHM (5 km) at the Sweep time. A
//     tentative hypothesis is bounded by the confirmation window instead:
//     the far branch of a 3+alt pair (σ_h often several km) dropped for its
//     σ left the near, wrong branch to be confirmed by default (H-track,
//     live-like netsim).
//   - 2-station maintenance and manoeuvres: from 3 s after the last
//     position-determining update (fix, or cluster with >= 3 rows) the
//     horizontal acceleration PSD of both models gets a manoeuvre allowance
//     a²·τ (τ 10 s, ramped in over 5 s), a = 1.2 m/s² at or below 80 m/s
//     ground speed and 0 at or above 160 m/s (linear between): GA and
//     helicopters turn unseen while only one TDOA row and the altitude
//     constrain them. Simnet, two stations for 60 s: errors inside the
//     99.9 % region 93.8 % → 96.9 % (GPS) and 92.4 % → 97.9 % (SDR); the σ
//     grows accordingly (σ_h < 300 m for the whole 60 s on 45 % / 23 % of the
//     tracks instead of 71 % / 47 %, which was partly overconfidence). Fast
//     traffic is unchanged: the allowance degraded straight flight there
//     (fixed-pair P95 412 m → 4.2 km) because it lets the state drift along
//     the unobserved hyperbola.
//
// # 9. Output
//
// Estimate holds the combined state of the best hypothesis (highest score
// among confirmed ones): ECEF position and covariance, the velocity and its
// covariance rotated into the local ENU at the aircraft, horizontal σ
// (√(σₑ² + σₙ²)) and the 1σ ellipse, turn rate and P(CT), status,
// hypothesis count and the station count / DOF of the latest position
// update. MLATFix grades with model.GradeFor(σh, MaxDOF), MaxDOF being the
// largest DOF of a position update in the last 10 s (rows − 3 for a
// cluster, the solver's DOF for a fix).
//
// # 10. Concurrency and cost
//
// Tracker is single-goroutine with per-tracker scratch space; Sharded
// (power-of-two shards, Fibonacci-hashed ICAO, a mutex each) is safe for
// concurrent use. All update paths, Predict, Get and Sweep are
// allocation-free once a track exists (TestNoAllocs). Measured on the dev
// box (i9-13900K, Go 1.27, one core): a cluster update 4.5 µs (2-8
// stations) to 13 µs (24 stations), a fix update 3.7 µs, the 1 Hz Sweep
// 1.4 µs per track (14 ms for 10 000 tracks). A hypothesis is about 4.7 KB (with its 32-station refraction cache).
//
// # 11. Measured accuracy (go test -v; numbers of the current defaults)
//
// Synthetic (geo propagation, 7 stations on an 80 km ring, 50 ns, 2 Hz):
// straight flight P50 3.8 m, P95 8.5 m; 3°/s and 6°/s 180° turns P95 13 m
// and 18 m (CV-only 16 m and 19 m); shuffled delivery within 1.5 s changes
// nothing measurable; one 3 µs outlier per 10 clusters is excluded with no
// accuracy loss. 2-station maintenance with altitude: σh stays below 300 m
// for 92 s (GPS-grade 50 ns) / 74 s (SDR-grade 250 ns) on one fixed pair,
// indefinitely when the pair changes or a 5 m/s velocity arrives every 2 s;
// errors stay inside the 99.9 % region. 3+alt ambiguity (130 ambiguous
// starts): 96.9 % resolved to the true branch within 10 s when a fourth
// station hears 20 % of the transmissions, 92.3 % at 10 %, 36.9 % with
// strictly the same three stations; in no case was the wrong branch
// selected — the unresolved ones stay flagged Ambiguous.
//
// simnet truth (120 aircraft, 4 min, simnet's own propagation, multipath
// and outliers, the receivers' actual jitter and quantisation, a modelled
// clock-filter residual of 30 ns GPS / 120 ns SDR with 20 s correlation):
// 8 GPS anchors within 150 km: inside the footprint P50 12 m, P95 63 m,
// 2σ coverage 0.97; outside it (to 300 km) P50 49 m, P95 186 m. 20 SDRs:
// inside P50 28 m, P95 117 m, 2σ coverage 0.95; outside P50 110 m, P95
// 426 m. 2-station maintenance on the two nearest stations for 60 s
// (tracks inside the footprint with σh < 100 m): σh < 300 m for the whole
// 60 s on 71 % (GPS) / 47 % (SDR) of the tracks and for ≥ 30 s on 100 % /
// 96 %; with a BDS-like velocity every 5 s on 100 % / 98 %.
//
// End to end with the real solver (cmd/tracksim: simnet → mlat/solve fixes
// → this tracker, 4 min, ~140 aircraft): 8 GPS anchors inside the
// footprint P50 13 m, P95 50 m, 2σ coverage 0.98; 20 SDRs P50 27 m, P95
// 93 m, 2σ coverage 0.98; mean cost per update 5.8 µs / 9.3 µs including
// the solver calls before confirmation.
//
// # 12. Limits
//
//   - A turn that only two stations see is barely observable: with the
//     manoeuvre allowance (§8) 97-98 % of the 2-station errors of manoeuvring
//     helicopters and GA stay inside the 99.9 % region (92-94 % before); single
//     errors still reach 2.7-3.7 km. A velocity (BDS 5,0) makes the stretch
//     observable (99.3 % / 98.1 %).
//   - With strictly the same three stations and no velocity, the two 3+alt
//     branches often cannot be told apart within 10 s (56 % stay flagged
//     rather than guessed); after 60 s 87 % are resolved. Near-tangent pairs
//     whose branches stay within a few km of each other are occasionally
//     decided wrongly by chance (2-4 of 130 within 30-60 s, likelihood ratio
//     100).
//   - Score of one measurement: lls (accepted, or the gate-boundary value)
//     minus a separate violation penalty (settle's pen); a cap on the
//     per-step loss was tried and rejected (it slowed the 4th-station
//     resolution from 96.9 % to 92.3 % without saving the near-tangent
//     cases).
//   - Sweep emits in ICAO order (bit-reproducible engine runs).
//   - The per-station clock-error correlation is modelled per hypothesis;
//     correlation across aircraft (the same station clock error biasing
//     every track) is left to the clock filter.
//   - Sustained along-track accelerations (approach deceleration) are
//     covered by QCV = 0.3 m²/s³, which shortens fixed-pair maintenance;
//     an adaptive along-track model is a possible phase-4 refinement.
//
// # 13. Mode A/C targets (ACTracker, DESIGN §9.9, phase 5)
//
// Mode A/C replies carry no address, so ACTracker keys its targets by
// synthetic identities ACIDFlag | sequence (bit 25: above the 24-bit ICAO
// address and the non-ICAO flag; IsACID tells them apart) and runs an
// ordinary Tracker underneath for the kinematics.
//
// Codes (accode.go). The raw code is the 13 reply pulse positions in
// transmission order (C1 bit 12 … D4 bit 0, X bit 6, masked by
// ACCodeMask). ACSquawk/ACFromSquawk map it to the 4-digit octal identity;
// ACAltitudeFt/ACFromAltitudeFt to the Gillham Mode C pressure altitude
// (500 ft Gray count on D1 D2 D4 A1 A2 A4 B1 B2 B4, 100 ft five-state code on
// C1 C2 C4, reflected for odd 500 ft counts; −1200 … 126 700 ft). The reply
// does not say which interrogation it answered and most squawks are also
// valid Gillham codes, so a target keeps up to six codes with counts and
// last-seen times; the altitude code is the most recent code that decodes
// within max(AltGateM, 3σ_v) of the target's MLAT height, the squawk the
// most frequent other code. Codes unseen for CodeTTLS are forgotten.
//
// Association of a fix (≥ 4 stations, one or two hypotheses): candidates
// come from a 40 km spatial grid (27 cells) and a reachability pre-filter
// (speed limit × Δt + 5σ + 3 km); each candidate is predicted and gated on
// the squared Mahalanobis distance d² ≤ GateChi2 (25, 3 DOF) and chosen by
// d² + ln det S + code penalty (−1 for a shown target): the log-det term
// stops a tentative target with a wide prediction from stealing an
// established target's fixes. Code penalty: 0 for a code the target sent;
// CodePenalty for a second code (only within NewCodeWindowS = 0.5 s of the
// last update: the A and C replies of one dwell are 4-8 ms apart, later the
// gate of a one-code target cannot tell FRUIT from the aircraft); for a
// third or later code only a Gillham altitude within AltGateFt of an
// existing altitude code or within the height gate; otherwise +Inf. An
// unassociated unambiguous fix starts a target unless it lies within
// OutlierChi2 of a shown target (heavy-tailed fix errors would otherwise
// spawn duplicates).
//
// 2-3 station clusters never start targets: their TDOAs are tested against
// the predictions of the shown targets that have sent the code (χ² with an
// isotropic position σ = hypot(σ_h, ClusterSlackM); gate the 0.999 quantile
// of m − 1 DOF) and the best one is updated through Tracker.Cluster.
//
// Confirmation (shown): the kinematic track is confirmed, ConfirmFixes (3)
// fixes in ConfirmBursts (2) separate bursts (> BurstGapS apart) spanning
// ConfirmSpanS (2 s). A target that never had a fix with DOF ≥ StrongDOF
// (4-station fixes only: no redundancy, a mirror solution can move
// smoothly) needs WeakConfirmFixes (12) in WeakConfirmBursts (4). FRUIT and
// shared-code coincidences do not repeat at a consistent position over
// several sweeps.
//
// Merge (Sweep): two targets whose predictions agree within MergeChi2 and
// whose horizontal velocities agree (shared code: within max(MergeVelMS,
// 3σ); no shared code — the Mode A and Mode C halves of one aircraft —
// both velocities known to MergeVelMS and within it) for MergeSweeps
// consecutive sweeps are merged into the one with more fixes.
//
// Grade: model.GradeFor(σ_h, MaxDOF), one class worse when another shown
// target with the same squawk lies within CrowdM (5 km; association by
// position alone, e.g. two VFR 7000 targets). Sweep emits shown targets in
// identity order.
//
// Measured (engine simnet, internal/mlat TestModeACNetsim: 8 GPS anchors,
// 40 Mode A/C-only aircraft, a quarter on 7000, 4 radars, 75 % reception,
// +50 ns A/C timing noise, 100 FRUIT/s per station): availability 100 %,
// shown-target error P50 17 m, P95 58 m, 1.10 identities per aircraft,
// squawk correct 98.3 %, no false targets. BenchmarkACTrackerFix measures
// the per-fix cost (WIRING.md).
package track
