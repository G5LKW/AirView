// Package clock is the joint clock filter of the AirView MLAT engine
// (docs/aggregator/DESIGN.md §9.2): it estimates, for every timed station,
// the mapping from its receiver ticks to one network time scale, with a
// full covariance, from the reference aircraft every station hears.
//
// # Units and time
//
// Network time is float64 seconds since Config.OriginUnixNs ("network
// seconds"; 0.1 ns resolution over months). Inside the filter clock states
// are in nanoseconds (θ, β), ppb = ns/s (φ) and ppb/s (ψ), so all states
// are O(1…1e12) and diagonal scaling handles the rest.
//
// A station's local time for a frame of kind k is
//
//	y = (ticks − B)/f_nom + b − FrameRef(class, k)
//
// with B the first ticks seen in the current clock epoch and b their local
// time: for the GPS classes b = (B − origin)/1e9 exactly (the ticks are
// UTC or GPS-time nanoseconds), for counters b is the host time of that
// first frame (θ then starts near 0 with σ = HostSigmaS).
//
// # Model (DESIGN §9.2.1)
//
//	y_i = t_i + θ_i(t_i) + β_i·[short] + n_i,    t_i = τ + ρ_i/c
//
// τ is the transmit instant, ρ_i the optical path from the reference
// position p to station i (geo.Atmosphere: geometric range + refraction
// excess). A free clock has the block (θ, φ, [ψ], β):
//
//	θ(t_e + Δ) = θ + φ·Δ + ψ·Δ²/2,  continuous dynamics θ̇ = φ + w_θ,
//	φ̇ = ψ + w_φ, ψ̇ = w_ψ, β̇ = w_β
//
// with white noise densities q_θ (white FM), q_φ (random-walk FM), q_ψ
// (drift random walk) and q_β = 1 ns²/h. The exact discretisation over dt
// (component.go addQ, verified against numerical integration in
// TestPredictMatchesFPFtQ) is
//
//	Q_θθ = q_θ·dt + q_φ·dt³/3 + q_ψ·dt⁵/20   Q_θφ = q_φ·dt²/2 + q_ψ·dt⁴/8
//	Q_φφ = q_φ·dt + q_ψ·dt³/3                Q_θψ = q_ψ·dt³/6
//	Q_φψ = q_ψ·dt²/2                         Q_ψψ = q_ψ·dt
//
// An anchor has the block (δ, β) with φ ≡ 0 and δ a 1 ns/√h random walk.
// ψ exists for classes 5, 6 and 7 (thermal wander of SDR crystals; see
// "Deviations" for class 6). Priors and noise densities per class are in
// config.go (priorFor); every station's noise is scaled by an adaptive
// factor (below).
//
// # Sync observations and the transmit time (§9.2.2)
//
// One SyncObs is one transmission of a reference aircraft (DF17/18 and the
// DF0/4/5/11 replies of the same aircraft, so β_short is observable), with
// the reference position and its covariance Σ_p from mlat/ref. τ is
// eliminated, not estimated. With the innovations
//
//	e_i = (y_i − t̂_i)·1e9 − θ̂_i(t̂_i) − β̂_i·[short],   t̂_i = τ̂ + ρ_i/c
//
// (τ̂ from the most precisely known member) the linearised model is
//
//	e = H·δx + a·δτ + v,   a_i = 1 + φ̂_i,   v ~ N(0, R),
//	R = D + V·Vᵀ,  D_ii = σ_i² + Q_θθ,i(|Δ_i|) + σ²_τ̂·σ²_φ,i,  V = U·L_p/c
//
// where U has the unit vectors aircraft→station, L_p·L_pᵀ = Σ_p, σ_i is the
// station's jitter, Q_θθ,i(|Δ_i|) the process noise between the epoch
// time and the arrival, and σ²_τ̂·σ²_φ,i the bilinear time-argument term
// (the clock rate times the error of the arrival instant). Eliminating τ
// gives the weight
//
//	W_p = R⁻¹ − R⁻¹a·aᵀR⁻¹ / (aᵀR⁻¹a)
//
// which for a = 1 equals Dᵀ(D R Dᵀ)⁻¹D of the differenced form against a
// reference member (DESIGN's R = σ_ref²·11ᵀ + diag σ² + GᵀΣ_pG/c²) — proven
// numerically in TestNuisanceElimination — but needs no reference choice
// and no m×m inverse: with the Woodbury identity
//
//	R⁻¹ = D⁻¹ − Y·K·Yᵀ,  Y = D⁻¹V,  K = (I₃ + VᵀD⁻¹V)⁻¹
//
// W_p = D⁻¹ − Σ_{k=1..4} z_k z_kᵀ is diagonal plus rank 4 (three position
// columns Y·chol(K) and Wa/√(aᵀWa)), so a group of m members touching T
// states costs O(T²) to accumulate.
//
// Members of a GPS-class clock additionally get PPSCorrelatedNs² × (frames
// per second) in D: the 1PPS discipline error is common to all frames of a
// second, and counting it once per frame made the anchors' σ three times
// too small on simnet.
//
// # Estimation (§9.2.3)
//
// Components are the connected components of the co-observation graph of
// the last CoObsWindowS (union-find over consecutive members of each
// admitted group). Each component keeps x and a dense covariance P. Every
// epoch (0.5 s):
//
//  1. prediction with the block-diagonal F and Q, O(n²) in place;
//  2. every group is gated against P⁻ (below), and accepted groups add
//     J += Hᵀ·W_p·H, g += Hᵀ·W_p·e (innovations against the prediction at
//     each observation's own time: H rows are [1, Δ, Δ²/2, short]);
//  3. the gauge constraints are added as pseudo-observations;
//  4. P⁺ = (P⁻⁻¹ + J)⁻¹, x⁺ = x⁻ + P⁺·g, computed on the unit-diagonal
//     scaled matrices (S⁻¹P⁻S⁻¹, S = diag √P_ii) with two Cholesky-based
//     SPD inversions; a nearly singular case is retried with the variances
//     inflated by (1+ε), ε ≤ 1e-6 (conservative), else the epoch's
//     information is dropped (Stats.UpdateFailures);
//  5. an immutable Snapshot is published (atomic pointer).
//
// TestInformationUpdate checks steps 2-4 against dense textbook formulas.
//
// # Gauge
//
// TDOA observations leave the common mode of all clocks unobservable; the
// gauge defines network time:
//
//   - with anchors: mean(δ_anchor) = g, σ AnchorGaugeSigmaNs (10 ns). When
//     the anchor set changes, g is set to the current mean (no jump) and
//     then relaxes to 0 with GaugeRelaxS (1 h): network time is the mean
//     anchor clock, i.e. UTC plus the anchors' mean cable delay;
//   - without anchors ("relative time"): mean θ, mean φ and mean β_short =
//     const over the GPS-class free clocks when there are any (their tight
//     frequency prior is about UTC; tying the scale to the mean SDR crystal
//     would make that prior wrong by tens of ppm), else over the synced free
//     clocks, else over all (σ RelGaugeThetaSigmaNs, RelGaugeFreqSigmaPPB,
//     RelGaugeBetaSigmaNs; β only when no GPS-class clock is in the set,
//     whose ±50 ns β prior already fixes it). The β_short gauge makes short
//     frames usable in anchor-less networks: there the common mode of β is
//     unobservable (the transmit time absorbs it).
//   - one gauge per time domain: in a partitioned domain only the partition
//     with the most anchors (anchored domain) or the largest one (relative
//     domain) carries it; the others are pinned through their external
//     members.
//
// When the gauge set changes the targets are reset to the current means
// (continuity) and the old gauge's information is released: the gauge
// pseudo-observation accumulates information on wᵀx every epoch, and that
// information is a definition, not a measurement. Left in place it pinned
// a station that left and re-joined the synced set to its current θ and φ
// with about the gauge σ (TestGaugeSetChangeDoesNotPin: σ_φ 13.7 → 6.6 ppb
// in one change). That was the SDR-only instability of HOOKS
// WP-M-engine -> internal/mlat/clock: a wrong φ pinned to a few ppb drove
// runs of false steps (seed 17: 22-113 steps per minute). The release is
// the exact change of time scale x' = x − u·(wᵀx − g), A = I − u·wᵀ, applied
// as P' = A·P·Aᵀ + σ²·u·uᵀ (component.projectGauge): every difference
// x_i − x_j keeps its covariance, the common mode gets the gauge σ back.
//
// # Integrity (§9.2.4)
//
// Per group: S = H·P⁻·Hᵀ + R over the members, τ eliminated as above;
// NIS = eᵀS_p e against χ²(m−1, 0.999) and every studentised innovation
// w_i = (S_p e)_i/√(S_p)_ii against WTestMax (4.5). On failure the member
// with the largest |w| is dropped and the rest re-tested (down to two
// members; a failing pair is rejected without blame). ∇_i = (S_p e)_i/(S_p)_ii
// is the member's leave-one-out residual.
//
//   - Step detection: StepCount (5) consecutive same-sign |w| > 4 — accepted
//     or dropped — declare a step: θ is moved by the mean ∇ of the run (the
//     step estimate) and var(θ) set to max(ResetThetaSigmaNs, |step|/2)².
//     A run whose residuals grow linearly is a frequency error, not an
//     offset step: the least-squares line ∇ = a + b·(t − t̄) through the
//     run's members (station.stepEstimate) is tested, and with |b| > 4
//     standard errors and > 3σ_φ, φ is corrected by b (var φ ≥ b²/4 + se²)
//     and θ by the line's value at the epoch (Stats.FreqSteps). Otherwise
//     φ is kept; StepRunCount same-sign steps within StepRunWindowS reset
//     φ and ψ to their prior variance (Stats.FreqResets). A clock-epoch change (CLOCK_EVENT or a new epoch id in the
//     data) re-bases the station: θ from host time with σ = HostSigmaS,
//     φ and ψ reset only for a restart; members of the old epoch in the
//     same batch are dropped.
//   - Two or more consecutive outliers mark the station "suspect": it is
//     published as not synced until an observation fits again.
//   - Influence cap: a station whose effective observation count
//     (Σ W_p,ii·D_ii) exceeds InfluenceCap × the component median has its
//     measurement variance inflated by the excess in the next epoch.
//   - Per-aircraft blacklist: an aircraft whose groups (≥ 4 stations) keep
//     failing (rejected, or two or more members dropped) in more than half
//     of ≥ 10 groups is refused for BlacklistS (bad NACp, spoofing).
//   - Synced: σ_θ below the class limit (50/100/300 ns), ≥ 5 accepted
//     observations in 30 s, not suspect.
//
// # Anchors (§9.2.5)
//
// Classes 1 and 2 are free clocks (with the 1 ppb frequency prior) while on
// probation. (a) is ProbationS of continuous "locked + UTC (class 1) or
// locked (class 2)" status, skipped for StationInfo.NoStatus; (b) after
// ProbationS as a free clock |φ| < 2 ppb and |θ − median δ| < 200 ns; (c)
// not flagged. Promotion conditions φ (and ψ) to 0 in place (exact
// covariance-form conditioning); the block shrinks to (δ, β) at the next
// restructure.
//
// The first anchors of an anchor-less domain are admitted on (a) alone: the
// largest set of candidates that agree pairwise (|Δφ| < 2 ppb, |Δθ| <
// 400 ns), or the only candidate ("self_asserted"); several disagreeing
// candidates wait. The domain is first re-gauged to the new anchors' mean
// clock with the exact time-scale change u = t + Δs + r·(t − t_e):
//
//	θ' = θ − Δs·φ + ½Δs²·ψ − Δs·1e9 + r·Δs
//	φ' = (φ − Δs·ψ − r)/(1 + r·1e-9),  ψ' = ψ/(1 + r·1e-9)²
//
// applied to x and as P' = A·P·Aᵀ (TestRegauge). A self-asserted anchor is
// validated by a candidate that agrees with it and demoted when two or more
// mutually consistent candidates disagree with it in frequency (|φ| >
// 2 ppb + 3σ after two minutes of observation).
//
// Demotion (to a class-3 free clock, never re-upgraded by the server):
// status bits lost, a new clock epoch, a step, the admin flag, failed
// revalidation, or drift: the apparent offset δ̂ + mean ∇ in 60-s bins is
// regressed over AnchorDriftWindowS and the anchor is demoted when the
// slope exceeds max(100 ns/h, 3 standard errors). Class 2 with an offset of
// exactly k·1 s (±1 µs) against the anchors gets b −= k, θ −= k·1e9 and
// becomes class 1.
//
// # Adaptive noise
//
// For a consistent filter the state correction δx = x⁺ − x⁻ satisfies
// E[δx²] = P⁻ − P⁺ (δx = K·ν, Cov = K·S·Kᵀ). Over AdaptWindowS each station
// accumulates δφ² (δδ² for anchors) and P⁻ − P⁺; the ratio r scales its
// process noise by √r (clamped to ×0.5…×2 per window and ×0.25…×10 of the
// prior). The jitter scale follows the mean squared studentised innovation
// the same way (×0.5…×4, NoJitterLearning disables it; mlat/quality can
// set StationInfo.JitterNs instead).
//
// # Admission
//
// AddSync admits a group while any member has a token in its bucket
// (SyncRatePerStation per second, burst of two epochs); admitted members
// spend one token. Groups with more than MaxMembers stations keep up to four
// precise references (anchors, synced σ < 50 ns), then the members with
// tokens, so a large group does not use up the share of stations it cannot
// serve.
//
// # Partitioned filter (phase 5, §14, §20.2)
//
// A time domain larger than MaxComponentStations (200) is split into
// spatially compact, sticky partitions of at most PartitionStations (100)
// stations (recursive coordinate bisection; new stations join the nearest
// partition, an over-full one is re-bisected). Members in another partition
// of the same domain enter a group as "external", predicted from the
// previous snapshot. Up to ExternalStates (64) of them per partition and
// epoch are epoch-local states, shared by all of the epoch's groups and
// marginalised out before the dense update (external.go); the rest keep
// their variance in D (consider treatment). The cross-covariance between an
// external estimate and the partition's own state is unknown (both were
// informed by the groups that cross the boundary); the external variance is
// therefore inflated by ExternalInflate (2: the covariance-intersection
// bound with equal weights, consistent for any correlation). Only one
// partition per domain carries the gauge (above); every other partition's
// clock offsets get a common-mode random walk of PartitionDriftNs (1 ns/√s)
// per epoch, P += q·dt·u·uᵀ, which leaves every difference inside the
// partition unchanged and lets the information that circulates between
// partitions decay (partition.go "Common-mode drift"). Partitions are
// updated in parallel (Config.Workers).
//
// Scaling (TestSimnetScaleCurve, AIRVIEW_CLOCK_SCALE=1; default device
// mix at constant density; one i9-13900K core, Workers 1; second half of
// three simulated minutes):
//
//	stations  partitions  epoch median  p95      synced  RMS GPS  hw12   sdr12_hires  sdr12
//	   200        1          61 ms       62 ms    200      7 ns    8 ns     27 ns      73 ns
//	   500        5          51 ms       53 ms    500     10 ns   13 ns     29 ns      73 ns
//	  1000       10         104 ms      108 ms   1000     16 ns   18 ns     30 ns      73 ns
//
// The cost is linear above 200 stations (≈ 10 ms per partition of 100); a
// VPS core about 1.5× slower runs 1,000 stations in ≈ 160 ms of the 500 ms
// epoch on one core. The price of the partition boundaries is a slowly
// wandering common-mode offset per partition (±10-25 ns at 1,000
// stations, RMS over the GPS clocks 16 ns against 7 ns dense); the
// 12 MHz SDR clocks, whose own noise dominates, are unaffected. Two
// defects found on the way: partitions were filled in station order when a
// dense domain first crossed the limit (every partition spanned the whole
// network: GPS 40 ns RMS, epoch 137 ms), and without the common-mode drift
// the anchored partitions locked each other up to 40 ns off with σ of a
// few ns (GPS 30 ns RMS, e²/σ² 93).
//
// The GPS anchors are over-confident in every layout (e²/σ² 3.7 dense at
// 200 stations, 11 at 1,000 partitioned). TestSimnetPartitioned (10 GPS +
// 60 others, partitions of ≤ 30) shows the same in the dense filter
// (persistent 3-8 ns biases between anchors against σ ≈ 1.7 ns); it is
// left open.
//
// # Internal time origin
//
// Inside the filter every time is a float64 of seconds since an internal
// origin OriginUnixNs + shiftS. When the epoch time passes ReoriginS (one
// day) the origin moves forward by whole seconds and every stored time
// (station bases, epoch times, events, edges, quotas, blacklist) is shifted
// (Filter.reorigin), so filter-internal times keep 0.015 ns resolution at
// any uptime. The public API (Step, Snapshot.T, NetTime, Convert, Local,
// NetFromLocal, BaseS, event times) keeps "seconds since OriginUnixNs";
// NetTimeUnix adds the whole seconds back exactly
// (TestReoriginContinuity, TestNetTimeUnixShiftedOrigin).
//
// # Use
//
//	f, _ := clock.New(clock.Config{OriginUnixNs: start})
//	f.SetStation(idx, clock.StationInfo{Class: cls, Pos: ecef, Located: true})
//	ingest.Options{Clock: clock.View{F: f, Ref: ref}, ClockObserver: f, ...}
//	// or clock.GateView{F: f, Ref: ref}: frame kind + jitter for the gate
//	// MLATSink.Observe: f.ObserveGroup(g, ref) for sync candidates
//	go f.Run(ctx, ntp.NowUnixNs, time.Second)
//	snap := f.Snapshot(); snap.Convert(queries, t, cov, ok) // solver
//
// Snapshot.Convert returns network times and the full covariance (ns²) of a
// cluster's timestamps (marginal blocks and cross-covariances), with zero
// allocations; Snapshot.NetTime one timestamp; StationClock carries the
// owner/admin statistics (σ_θ, φ, β, anchor state and reason, outlier %,
// events, sync peers).
//
// # Deviations from DESIGN §9.2
//
//   - ψ also for class 6 (sdr12_hires): simnet's Airspy crystals ramp a few
//     ppb/s; without ψ the tracking lag was ~300 ns RMS, with ψ ~25 ns.
//   - Relative gauge on the mean with σ 10 ns / 1 ppb / 10 ns (θ, φ,
//     β_short) instead of 1 ms / 1 ppm on the sum: the gauge is a
//     definition, and the loose value left a slowly shrinking common-mode
//     variance of microseconds in every marginal σ_θ, so no clock ever met
//     the synced limit in an anchor-less component. Gauge changes release
//     the old gauge's information (above).
//   - Epoch cost (Stats.LastEpochNs / MeanEpochNs) is measured with the
//     precise system clock (ntp.Now), because Go's monotonic clock ticks in
//     about 0.5 ms steps on Windows and read 0 for most epochs.
//   - The influence cap inflates the over-contributing station's variance
//     instead of scaling whole groups (scaling groups that contain a precise
//     anchor starved every other station of information).
//   - Reset after a step moves θ by the estimated step (a silent USB sample
//     drop can be 100 ms; a bare (1 ms)² reset then never re-converges).
//   - GPS-class jitter prior: 10 ns white + the correlated 1PPS term
//     (PPSCorrelatedNs, 30 ns) instead of 30 ns white.
//   - The 80-ms epoch budget is met on the development machine (i9-13900K,
//     one core): see bench_test.go. A slower VPS core may need a smaller
//     MaxComponentStations (the cost is cubic in the component size);
//     above it, partitions of PartitionStations (100) cost about 10 ms
//     each (partitioned filter, above).
package clock
