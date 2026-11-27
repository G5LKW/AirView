// Package ref builds the ADS-B reference tracks of the AirView MLAT engine
// (DESIGN §9.1 `mlat/ref`, §9.2.2, §9.4 altitude row, §9.6 GNSS−baro
// field): for every aircraft that broadcasts its own position it answers
// "where was this aircraft, with what uncertainty, at the instant it
// transmitted this message?" for ANY of its messages — DF17/18 positions,
// velocities and identities, and equally its DF11 squitters and DF0/4/5/
// 20/21 replies. Those answers are the beacons of the clock filter
// (mlat/clock), the truth of the hold-out (mlat/quality) and the targets of
// the survey (mlat/survey). It also learns the regional GNSS−baro height
// difference that turns Mode S pressure altitudes into heights above the
// ellipsoid.
//
// Everything here is our own derivation from the published message
// formats (DO-260/A/B field layouts as decoded by internal/modes) and
// textbook estimation theory; no third-party MLAT code was consulted.
//
// # Time base
//
// All instants are int64 Unix nanoseconds of the engine's network time,
// which is UTC when the clock component has GPS anchors and the server's
// NTP clock otherwise (DESIGN §9.2.1 gauge). The caller passes, with each
// message, its transmit instant τ (network time of the copy minus the
// propagation delay, or the grouper's corrected first-arrival time minus a
// typical delay) and the 1σ of τ. A millisecond is plenty: the aircraft
// moves 0.25 m per ms and the error enters the output covariance along the
// velocity. Inside a track, times are float64 seconds since the track's
// first message (sub-picosecond resolution over days).
//
// # Coordinates
//
// Each track has a kinematic frame anchored at (φ0, λ0):
//
//	x = Δλ · kx,  y = Δφ · ky,  z = h (height above the WGS-84 ellipsoid)
//	kx = N(φ0)·cos φ0,  ky = M(φ0)      (prime-vertical and meridian radii)
//
// Horizontal and vertical are exactly decoupled (lat/lon do not depend on
// h), so the three axes are filtered independently with 3×3 matrices. The
// map is not metric away from the origin; measurements are converted with
// the exact local scale factors
//
//	dE = sx·dx, dN = sy·dy,  sx = (N(φ)+h)·cos φ / kx,  sy = (M(φ)+h)/ky,
//
// applied to the velocities (v_x = v_E/sx), to the noise variances and to
// the output. Within one track the frame distortion is a slow, smooth
// deformation that the kinematic model absorbs (straight great-circle
// flight shows as an acceleration of order v²·tan φ/R ≈ 0.01 m/s²). When the
// aircraft is more than about 1° from the origin the frame is moved; the
// map is affine per axis (x' = s·x + o), so states and covariances
// transform exactly (TestRecenterExact).
//
// # Process model
//
// Per axis the state is (position, velocity, acceleration) with white jerk
// of power spectral density q (the Wiener-process-acceleration model).
// Integrating the continuous system ẋ = A x + B w, A the 3×3 shift, B = e₃,
// over a step T gives
//
//	F(T) = [[1, T, T²/2], [0, 1, T], [0, 0, 1]]
//	Q(T) = q·[[T⁵/20, T⁴/8, T³/6], [T⁴/8, T³/3, T²/2], [T³/6, T²/2, T]]
//
// (Q = ∫₀ᵀ F(s) B Bᵀ F(s)ᵀ ds). Defaults q = 4 m²/s⁵ horizontally and 1
// vertically: a 3°/s turn entered within 2 s is a jerk of ≈ 7 m/s³, which
// this q follows without large lag, while the 2 Hz velocity reports keep
// the straight-flight solution tight. On simnet the result is insensitive
// to q between 0.25 and 64 (P95 1.77–1.80 m), on the synthetic 3°/s turn
// the P95 error is 1.65 m (TestSyntheticTurn).
//
// # Measurements
//
// Airborne position (TC 9-18, 20-22). The first position of a track (and
// any position after StaleS without one) is decoded globally from an even
// and an odd frame no more than PairMaxS apart; every other frame is
// decoded locally against the track's prediction at its tag, and checked
// against a global decode when one is available (a disagreement > 1 km
// means the local decode aliased; the global one wins and the gate below
// decides). The decoded latitude/longitude are exact up to the CPR
// quantisation: the 2^17 cells of a zone, i.e. Dlat/2^17 (6° or 6.1°) in
// latitude and Dlon/2^17 = 360°/(NL−i)/2^17 in longitude, ≈ 5 m. For a
// moving aircraft the rounding is uniform and independent from report to
// report, so its variance cell²/12 (≈ 1.5 m 1σ) plus PosWhiteSigmaM² is
// the white measurement noise.
//
// Height. A GNSS height (TC 20-22) is used directly. A barometric altitude
// is converted with the aircraft's own GNSS−baro difference from its last
// velocity message (TC 19, ≤ 30 s old and taken within 1000 ft) or, failing
// that, with the field below. The white part of the noise is the altitude
// quantisation (25 ft or 100 ft Gillham, /√12); the conversion error is a
// bias for the duration of a track (it does not average out) and is kept
// separately and added to the output covariance only.
//
// Velocity (TC 19 subtypes 1/2). East and north from the decoded ground
// speed and track (the decoder reconstructs them from the integer
// components), noise from NACv (HFOM/2.45) and the 1 kt (4 kt) rounding,
// clamped to VelSigmaMinMps..VelSigmaMaxMps; vertical rate with 64 ft/min
// rounding and, for barometric rates, 5 % for the pressure/geometric ratio.
//
// # Time tags
//
// A T=1 position (UTC-coupled) applies to the 0.2 s UTC epoch whose parity
// is the CPR format bit, nearest to the transmit instant (DO-260A Appendix
// A §A.1.4.2.3.1; the rule of latency.NearestEpoch, TestNearestEpochMatches
// Latency). The tag is that epoch: exact, independent of latency. A
// transmitter that sends more than 150 ms (+3σ of τ) from its epochs three
// times is non-compliant and is demoted to T=0 tagging (FlagT1Untrusted).
//
// A T=0 position is tagged τ − L̂, L̂ the airframe's position latency
// (prior DefaultLatencyS ± LatencySigmaS = 0 ± 150 ms, the DO-260B
// −200..+400 ms budget). From the aircraft's own reports a constant latency
// is unobservable (it shifts the whole track in time), so L̂ is refined only
// by independent positions (ObserveFix). Velocities are tagged τ.
//
// # Window and filtering
//
// A track keeps its last Window measurement nodes (default 16 ≈ 4 s), in
// tag order, each with the filtered state after it. A node arriving out of
// order is inserted and the filter re-run from it (the result is order
// independent, TestOutOfOrderInsertion); a node older than the window is
// dropped (TooOld). The very first node starts from a vague prior (10 km,
// 20–316 m/s, 5 m/s²). The scalar updates use P⁺ = P − K Kᵀ S, which keeps
// P exactly symmetric.
//
// # Queries
//
// State(τ) is the fixed-interval smoothed estimate given every node in the
// window: the Rauch-Tung-Striebel recursion runs lazily from the newest
// node back to the node after τ,
//
//	C = P_f Fᵀ (F P_f Fᵀ + Q)⁻¹,  x_s = x_f + C (x_s⁺ − F x_f),
//	P_s = P_f + C (P_s⁺ − P⁻) Cᵀ,
//
// and τ itself is a pseudo-node (the filtered state of the node before τ
// predicted to τ) smoothed against it. After the newest node the answer is
// the forward prediction (FlagExtrapolated); before the oldest, a backward
// prediction (FlagBackward, unusable). TestSmootherMatchesBatch proves the
// recursion equal to the exact batch posterior of the same linear-Gaussian
// model (dense information-form solve over all states) to 1e-5 σ for
// interpolated instants and 1e-6 σ for forward ones.
//
// The output covariance (ENU, then rotated to ECEF) is
//
//	Σ = diag(sx²P_x, sy²P_y, P_z)          smoother (white noise only)
//	  + diag(b_h², b_h², b_v²)              report bias: b_h = EPU(NACp)/2.448,
//	                                        b_v = height-conversion bias
//	  + (σ_τ² + σ_L²) · v vᵀ                instant and latency uncertainty
//
// EPU is a 95 % horizontal radius; for a circular Gaussian r₉₅ = 2.448σ.
// The NACp error of GNSS is correlated over tens of seconds, so it is a
// bias to the clock filter, not noise the smoother can average. This is
// the Σ_pos of DESIGN §9.2.2; mlat/clock projects it on (u_i − u_ref).
//
// # Position latency (ObserveFix)
//
// With tags τ_k − L̂ and true latency L the track is p(t − δ), δ = L − L̂.
// An independent position p_fix at τ (a hold-out MLAT fix, or the TDOA
// solution of the same message from GPS-timed receivers) gives
// p_fix − track(τ) ≈ v·δ, i.e. the scalar measurement
// L_meas = L̂ + v̂·(p_fix − track(τ))/|v| with variance
// (v̂ᵀ(Σ_fix + Σ_track)v̂)/|v|². A scalar Kalman filter with a random walk
// of 0.5 ms/√s (it also absorbs the slowly varying along-track part of the
// GNSS error) estimates L; fixes slower than 50 m/s are skipped and
// outliers gated at 5σ. When L̂ moves by more than RetagS the T=0 position
// nodes are re-tagged and the window re-filtered.
//
// # Transmit instant
//
// TransmitInstant solves τ = Σ w_i (t_i − ρ_i(p(τ))/c) / Σ w_i for a
// message's arrivals (w_i = 1/σ_i², ρ the refraction-corrected range of
// internal/mlat/geo). The map τ ↦ RHS contracts by |v|/c ≈ 1e-6, so the
// fixed point is reached in two or three steps; the position moves along
// the velocity during the iterations and the excess path is evaluated once.
// σ_τ combines the arrival weights and the position uncertainty along the
// mean line of sight.
//
// # Quality gates (DESIGN §9.2.2)
//
// State.Usable requires: a confirmed track (ConfirmPositions accepted
// positions and a velocity), NACp ≥ MinNACp (8) and NIC ≥ MinNIC (7), the
// nearest position report within MaxPosAgeS (1 s) of τ, a height σ below
// MaxVertSigmaM, airborne, and none of the integrity flags below. NIC
// comes from the position type code with the NIC supplements of the
// aircraft's operational status (modes.NICRc). NACp comes from TC 31/29;
// until one is received it is derived from the type code's NUCp so that
// the EPU bound covers the NUCp accuracy bound (NUCp 9→10, 8→9, 7→8,
// 6→7, 5→6; FlagNUCp; Options.NoNUCp disables it). Only DF17 and DF18
// CF 0/1 with a clean parity are used: TIS-B and ADS-R are transmitted by
// ground stations, so their arrival times say nothing about the aircraft,
// and an error-corrected frame is not trusted as a reference.
//
// # Integrity (spoofing, jumps, duplicate addresses)
//
//   - Gate: a position whose innovation against the filtered prediction at
//     its tag exceeds both χ²₂ = GateChi2 (30, p ≈ 3e-7) and GateFloorM
//     (25 m), or that implies more than MaxSpeedMps from the previous
//     accepted position, is rejected. The height is gated separately (5σ
//     and 100 m) and only the height is dropped.
//   - Jump: JumpConfirm consecutive rejected positions that are consistent
//     with each other (≤ MaxSpeedMps·Δt + 2 km apart), at least one from a
//     global decode, re-initialise the track there with FlagProbation for
//     ProbationS.
//   - Unstable: UnstableJumps jumps, or ConflictRejects rejected positions,
//     within UnstableWindowS (two transmitters with one address, a spoofer
//     mixed with the real aircraft) suspend the address for SuspendS.
//   - Velocity consistency: velocity reports gated like positions
//     (VelGateMps floor); three rejections within 30 s set
//     FlagVelInconsistent for ProbationS (naive spoofers that do not make
//     their velocities match their positions).
//   - CheckHeard: a position more than HorizonMarginM below the radio
//     horizon (effective earth HorizonK = 2, generous for ducting) of a
//     receiver that heard the message is impossible (a transmitter near
//     the receivers announcing a far position); probation, and suspension
//     after three.
//   - Suspend: the clock filter's blacklist (an aircraft biased across
//     ≥ 3 station pairs, DESIGN §9.2.4) and failed TDOA verification
//     (§9.6) suspend an address from outside. A spoofer with
//     self-consistent kinematics can only be caught there.
//
// # GNSS−baro field (DESIGN §9.6)
//
// Δ = GNSS height − pressure altitude, per cell of CellKm × BandFt (50 km ×
// 2000 ft of pressure altitude; longitude steps widen with latitude to keep
// cells square). Observations are the TC 19 differences of ADS-B aircraft
// at their last position and barometric altitude, averaged per aircraft
// and cell over DecimateS (30 s), because the airframe's altimetry error
// and GNSS vertical error are common to all reports of one aircraft;
// ObsSigmaM (12 m) is that per-aircraft error. Each cell is a scalar
// Kalman filter whose prior is the fallback model and whose state follows
// a random walk of DriftMPerH (20 m/√h: the change of the pressure-height
// relation with the weather, ≈ 2 hPa/h). This realises the "1 h time
// constant" of the design as forgetting rather than as relaxation: a
// first-order Gauss-Markov cell with a 150 m stationary σ and τ = 1 h would
// lose 19 m of accuracy in 30 s, which is physically wrong. A cell that has
// drifted to the fallback's σ returns the fallback and is pruned.
//
// A query combines the eight cells around (lat, lon, alt) with weights
// bilinear/variance, each variance enlarged by a representativeness term
// (GradientMPerCell, GradientMPerBand times the distance in cells/bands),
// so learned neighbours dominate empty ones. The fallback is the ISA
// conversion with surface QNH and temperature from Weather (when known,
// σ 60 m),
//
//	p = 1013.25·(1 − L·h_p/T0)^5.25588,  h = T0/L·(1 − (p/QNH)^(1/5.25588)),
//	h_msl = h + ΔT·h/(T0 − L·h/2),
//
// plus the EGM96 undulation (internal/geoid), or the undulation alone
// (σ 150 m).
//
// # Mode S altitude replies and GNSS−baro pairing
//
// DF0/4/16/20 replies carry the pressure altitude (AC13) of an aircraft
// with the address overlaid on the parity. For an aircraft that has a
// reference track they are used in three ways (altreply.go): the latest
// pressure altitude for the pairings below; at most every AltReplyMinS a
// height-only node of the vertical channel (baro converted as for
// position reports, tagged with the transmit instant itself, never
// re-tagged with the position latency); and nothing else. A corrupted frame
// can carry any address, so a reply must agree with the aircraft's recent
// pressure altitude moved by its vertical rate (5σ and 100 m), else with
// the track height.
//
// An aircraft reporting GNSS heights (TC 20-22) gives no pressure altitude
// in its squitters; paired with a pressure altitude from a reply (or TC
// 9-18) within PairBaroS, the difference h_GNSS + v_z·Δt − h_baro is its own
// GNSS−baro value (used like TC 19) and one observation of the field.
//
// # Surface weather
//
// BundleWeather adapts internal/weather (Open-Meteo bundles): QNH =
// pressure_msl, the 2 m temperature, and the pressure-level geopotential
// heights. With a profile (WeatherProfile) the fallback takes the height
// of the aircraft's isobaric surface from the analysed column: the ISA
// pressure of the pressure altitude, quadratic interpolation in log
// pressure between levels, geopotential converted to geometric height
// (z = R·H/(R − H)), σ ProfileSigmaM (25 m) and ProfileSigmaTK (1.5 K).
//
// # Measured (Windows i9-13900K, go1.27.3; see the tests)
//
// Simnet (TestSimnetInterpolation: 200 aircraft, 6 min, 8 receivers,
// positions without latency/NACp error so the error is the
// interpolation): every message of every usable reference aircraft
// (351 513 instants, 0.4 s after the instant): horizontal P50 0.88 m,
// P95 1.78 m, P99 2.19 m, max 6.0 m; DF0/4/5/11 alone the same; vertical
// P50 1.74 m, P95 3.63 m (25 ft rounding of altitude and of the GNSS
// difference). Without the delay (forward prediction) P95 1.99 m.
// With simnet's 0..300 ms latency, uncorrected T=0 tracks are off by
// P95 66 m; ObserveFix with σ 30 m fixes every 2 s brings them to P50
// 1.9 m / P95 5.0 m. With all impairments the covariance holds 69 % of
// the errors in its 1σ ellipse and 96 % in 2σ. The field converts the
// pressure altitude of aircraft in learned cells to HAE with P95 3.4 m
// (geoid-only fallback: 77 m).
//
// Benchmarks: Observe 280 ns/message, 0 allocs; At 625 ns (0.4 s back),
// 160 ns (forward), 3.7 µs worst case (oldest node); 45 ns/op in parallel
// over 1000 aircraft on 32 threads; TransmitInstant (4 receivers) 3.7 µs;
// Field.Offset 445 ns; all 0 allocs. Memory ≈ 6.4 kB per aircraft with
// positions (64 MB for 10k), tracks with status messages only ≈ 1.1 kB.
package ref
