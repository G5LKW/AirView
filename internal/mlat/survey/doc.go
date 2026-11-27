// Package survey is the reverse-MLAT self-survey of a feeder's antenna
// position (docs/aggregator/DESIGN.md §10.2-10.3, decisions §19.3): the
// position and the clock of a station U are solved jointly from the
// transmissions of ADS-B reference aircraft that U and synchronised,
// located peers heard together. It also checks declared positions
// (suspect detection), decides ambiguous MSL/HAE altitude references,
// runs the consent-gated job API the owner and admin pages use, and runs
// the network bundle adjustment of all unanchored stations (phase 4,
// section "Network bundle adjustment" below).
//
// # Measurement model
//
// For one long Mode S frame of a reference aircraft the peers fix the
// emission time τ in network time (each peer: arrival − propagation from
// the reference position at τ, via ref.Store.TransmitInstant; peers that
// disagree by more than 5σ + 300 ns are dropped, at least MinPeers must
// remain). With U's raw receiver time s_U = ticks / f_nom:
//
//	ρ = c·(s_U − τ) = |p − u| + excess(p, u) + c·θ_U(τ) + n
//
// p is the aircraft at τ, u the unknown antenna position, θ_U U's clock
// offset against network time (no clock filter state of U is used: U may
// be unlocated, or its declared position may be wrong). The frame
// reference point of U's class is a constant and is absorbed by θ_U. The
// refraction excess uses the engine's geo.Atmosphere at both ends.
//
// The variance of n is σ_U² (U's timing noise, learned) + var(c·τ) (peer
// clock and jitter) + (Ē − e_U)ᵀ·Σ_p·(Ē − e_U): a position error δp of the
// aircraft moves the peers' τ by Ē·δp/c (Ē the weighted mean unit vector
// peers → aircraft) and the model by e_U·δp, so only the difference is
// left (Obs.Ebar).
//
// # Clock model (DESIGN §10.2 "clock knots")
//
// θ_U is continuous piecewise linear with knots every KnotS seconds and a
// Gaussian prior on its second differences (ClockModelFor: GPS 30 s / 5 ns,
// GPS emulated 20 s / 20 ns, hw12 10 s / 10 ns, sdr12 4 s / 200 ns, airspy
// 4 s / 80 ns, sbs20 4 s / 120 ns; peers in a relative-time domain get at
// least 4 s / 100 ns because their network time is the mean of free
// clocks). One clock segment exists per receiver epoch, peer time domain
// and anchoring state; a jump of the local-minus-network time between
// consecutive observations of more than 2 ms (a GPS day wrap, a gauge
// change of the peers' time) starts a new segment. A silent step inside a
// segment shows as one outsized normalised second difference and frees the
// prior rows around it (BreakZ, Solution.Breaks). The clock block of the
// normal equations is banded (half bandwidth 2): the position is solved by
// the Schur complement with banded Cholesky factorisations, O(n + K) per
// iteration.
//
// # Height prior and seed
//
// Height (DESIGN §10.2): DEM(lat, lon) + geoid + AGL (Options.Terrain,
// DefaultAGLM 10 m when none is declared), σ HeightSigmaM 20 m; without a
// DEM the declared height (its reference known), else the median height of
// the co-observing peers with σ 200 m. The seed grid (21×21 points inside
// SearchRadiusM 50 km of the declared position, the Locate Me hint, or
// 150 km of the peer centroid) scores each point by fitting the clock alone
// under a pseudo-Huber loss whose scale follows the grid step, then refines
// the three best by quarter steps down to 50 m; a best point on the
// boundary is searched again around itself.
//
// # Robust Gauss-Newton
//
// Rounds of iteratively reweighted least squares: each round learns U's
// jitter (median of the normalised squared residuals at the χ²₁ median,
// corrected for leverage), the per-aircraft error correlation ρ (method of
// moments on each aircraft's sum of normalised residuals; weights
// 1/(1 + (n_a − 1)ρ), the GLS weight of n_a equicorrelated observations)
// and Huber weights (HuberK 2σ, rejection beyond RejectK 5σ), then runs
// Gauss-Newton with them held, with a 5 km trust region. The elevation
// mask (MaskDeg 2.5°) applies to every observation; the tap drops
// observations far below it when an approximate position is known.
//
// # Covariance (honest σ)
//
// Three ingredients: the GLS formal covariance (FormalENU), the formal
// covariance of independent observations (IndepENU, a lower bound) and a
// delete-one-group jackknife over aircraft groups (about three aircraft
// per group, 10…JackknifeGroups groups), which measures what errors shared
// by the frames of one aircraft (position latency, NACp bias) really do.
// With at least 8 replicates the jackknife, inflated by
// (1 + 1/√(2(m−1)))², is used above the independent bound; with fewer the
// larger of formal and jackknife. A consider term adds a height error
// common to all reference aircraft (the GNSS−baro field), with the
// position's sensitivity to it computed from the normal equations; a
// systematic floor (3 m anchored, 10 m relative time) is added in
// quadrature. TestCalibrationSynth checks the result over 60 surveys.
//
// # Verdicts
//
//   - Stable (DESIGN §10.2): σ_h (semi-major axis) below 30 m for GPS
//     classes or 200 m otherwise, azimuth spread ≥ 180° (360° minus the
//     largest empty 10° sector), ≥ 10 aircraft, and the split-halves test
//     ran.
//   - Suspect (the survey's own checks): the solutions of the first and
//     second half of the data disagree (Mahalanobis² > 13.8, 99.9 % of
//     χ²₂), the best seed was outside the search area, more than 25 % of
//     the observations were rejected, or U's timing is noisier than its
//     class allows (> 4× the class prior and > 100 ns: a false clock
//     class). A suspect survey is never auto-used.
//   - AutoUse (DESIGN §19.3): stable, not suspect, σ_h ≤ AutoUseMaxSigmaM
//     (50 m) and AutoUse on. The decision itself is the engine's
//     (mlat.Engine.SetSurvey applies the same rule; the owner is notified
//     through mlat.PositionNotifier and may override with OverrideSurvey).
//   - Declared check (DESIGN §10.3): |declared − surveyed| horizontally >
//     max(SuspectMinM 100 m, SuspectK 3 × σ_h) marks the declaration
//     suspect (DeclaredFlagger; the station is then no longer a survey
//     peer). Any result may set the flag; only a stable one clears it, and
//     a changed declaration clears it and schedules a new check.
//   - MSL/HAE (DESIGN §10.2, declarations with reference "ambiguous" or
//     "msl"): a free-height solve gives h ± σ_v; the reading (alt as HAE,
//     alt + N as MSL) whose normalised distance is smaller by more than 3
//     and itself below 4 wins; otherwise the verdict stays undecided. An
//     "altref" job continues until it is decided or MaxDuration ends it.
//
// # Jobs, consent and scheduling
//
// A job collects observations for one station (a collector with per-
// aircraft thinning MinInterval and a memory bound MaxObs that decimates)
// and is evaluated after MinDuration (3 min) and then every EvalEvery
// (1 min) until it is stable and consistent (done), MaxDuration (30 min:
// unstable, best effort kept) or NoDataAfter (10 min with < 20
// observations: nodata).
//
// Consent (DESIGN §10.5): a station whose owner withholds timestamps
// (Station.Timestamps false) is never surveyed; withdrawing consent
// cancels its job at once. Untimed, map-only (Beast 30004) and second-
// channel copies are never used.
//
// The scheduler (Tick, driven by Run every 5 s) starts: requested jobs
// (Start by owner or admin, RequestSurvey from the engine's residual-bias
// flag), unlocated stations (no GNSS, confirmed or declared position;
// retried with a doubling pause from RetryAfter), ambiguous altitude
// references and the periodic check of declared positions every
// CheckEvery (6 h, aggregator.mlat.surveyIntervalH). A station under
// survey is not a peer of other surveys, so at most MaxCheckFrac (10 %) of
// the stations are checked at once; MaxJobs (64) bounds all jobs. Owners
// may start one job per OwnerMinInterval (1 h).
//
// # Privacy (DESIGN §8)
//
// Results contain exact positions: owner/admin data only. The package
// never logs positions (only σ and verdicts), Obs carry no peer
// identities, and Status/Result must only be served on owner- or admin-
// scoped routes.
//
// # Concurrency and cost
//
// Observe (the ingest's MLAT sink, usually through Tee) is lock-free on
// the hot path: one bitmap test per copy against an atomically published
// target set; 40 ns per group without targets, 0 allocations
// (BenchmarkObserveNoTarget). Groups with a target cost a clock
// conversion per peer and one TransmitInstant. Evaluations run on Workers
// (2) background goroutines; one evaluation of 15 000 observations takes
// about 0.3 s (BenchmarkSolve). Memory: ~140 B per observation, at most
// MaxObs per job. Sink, DeclaredFlagger, Notifier and Targets are called
// without the surveyor's lock.
//
// # Network bundle adjustment (DESIGN §10.3 phase 4)
//
// Bundle (bundle.go, bundle_solve.go, bundle_seed.go) refines the
// positions and clocks of many stations jointly from the raw arrival times
// of ADS-B reference frames they heard together; the Bundler (bundler.go)
// collects those frames live and runs it every BundleConfig.Every (1 h)
// over the last Span (1 h). Unlike a single survey, which needs synced,
// located peers, the bundle needs no clock state of any station: two
// unlocated stations that only hear each other's aircraft are observable,
// because the ADS-B positions fix the frame.
//
// Model. For frame f (aircraft at p_f, covariance Σ_f) heard by station s
// at raw receiver time t_fs:
//
//	c·t_fs = |p_f − u_s| + excess(p_f, u_s) + c·τ_f + c·θ_s,w(t) + n
//
// τ_f is the emission time (eliminated per frame in closed form: the
// frame's precision becomes Q = C⁻¹ − C⁻¹11ᵀC⁻¹/1ᵀC⁻¹1, with C = D + GΣ_fGᵀ
// by Woodbury, D the stations' noise and G the unit vectors, so aircraft
// position errors correlate the frame's residuals as they should). θ_s,w
// is a clock polynomial per station and window w (WindowS 10 s): offset +
// drift for GPS and hardware 12 MHz classes, plus a quadratic term for the
// free-running ones; the common clock of a window is the τ gauge and is
// fixed by a ridge. Per window the clock blocks are eliminated by a Schur
// complement, so the normal equations that remain are only 3·N position
// unknowns (N ≤ MaxStations 64): cost O(frames·m² + windows·(clock
// parameters)³ + N³) per Gauss-Newton pass, windows run on Workers
// goroutines.
//
// Gauge and priors. Anchors (GNSS-reported or owner-confirmed positions,
// AnchorSigmaHM/VM 5/10 m) and declared positions gauge the network;
// hints (Locate Me) are weak priors; stations without a position get a
// grid seed against the stations seeded so far (bundle_seed.go: robust
// misfit of the clock differences after a polynomial per pair and window,
// best three candidates refined). Every station has a height prior (DEM +
// AGL if given, else the declared height, else the median of the located
// stations with UnlocatedHeightSigmaM 300 m).
//
// Stages and outliers. Stage 1 holds every declared position loose
// (DeclaredLooseM 2 km) and tests it: the data-only position of a station
// (its own prior removed from the normal equations, every other prior
// kept) more than max(OutlierMinM 100 m, OutlierK 3 × σ_h) from the prior
// contradicts it; every contradicted declaration and the worst
// contradicted anchor are released and the bundle is solved again until
// nothing is contradicted. Stage 2 tightens the remaining declarations to
// DeclaredSigmaHM (30 m) and releases the single worst contradicted prior
// at a time (data snooping) up to MaxReleases. A station is flagged
// Outlier for a contradicted prior ("declared"/"anchor"), timing noise far
// above its class ("noisy": > NoisyFactor 4 × the class prior and >
// NoisyMinNs 150 ns, a false clock class) or more than MaxRejectFrac 25 %
// of its receptions rejected ("rejects").
//
// Robustness and covariance. Rounds of reweighting learn each station's
// timing noise (median of the leverage-corrected squared standardised
// residuals), apply Huber weights (HuberK 2σ) and reject beyond RejectK
// 5σ. The covariance is the larger of the formal one (scaled by the
// variance factor when it exceeds 1) and a delete-one-group jackknife over
// JackknifeGroups (10) contiguous time groups (inflated by
// (1 + 1/√(2(m−1)))²), plus a systematic floor (SysFloorM 3 m for GPS and
// hw12 classes; SysFloorFreeM 10 m for the others, 30 m vertically, where
// their level-dependent timing biases go; the jackknife is used with at
// least 8 groups when its trace is larger). A station with σ_h > MaxSigmaHM (1 km) is not Determined.
//
// The Bundler. Observe is the ingest's MLAT sink (lock-free bitmap test,
// ~100 ns per group, 0 allocations while nothing needs adjusting): it
// keeps long Mode S frames of reference aircraft (clock.SyncDF, a tracker
// result, a RefPositioner position) heard by a subject (a consenting timed
// station without an anchor) and at least one other member, at most one
// per aircraft per ThinInterval (8 s), bounded by MaxFrames (decimating
// when full). At most MaxStations/2 subjects are in focus at a time
// (unlocated first, then flagged declarations, then the least recently
// adjusted), with the members that co-observe most with them, so networks
// of any size are covered in turns. Declarations a run contradicts are
// reported to DeclaredFlagger (SetDeclaredSuspect true) and cleared by a
// later run that confirms them; every result goes to the BundleNotifier.
//
// Privacy and consent are as for the survey: only consenting, timed,
// non-map-only copies are used; results hold exact positions (owner/admin
// only); logs carry counts and σ only.
//
// Cost (i9-13900K, BenchmarkBundleMax: 64 stations, 30 min, 66k frames,
// 2.8M receptions): ~27 s and ~0.5 GB allocated per hourly run;
// BenchmarkBundle (17 stations, 10 min) 1.3 s.
//
// See WIRING.md for the engine wiring and docs/aggregator/MLAT-RESULTS.md
// for the simnet gate results.
package survey
