// Package mlat is AirView's own multilateration engine (DESIGN §9): the
// root that wires the reference tracks (mlat/ref), the joint clock filter
// (mlat/clock), the planner (mlat/plan), the cluster assembler
// (mlat/group), the solver (mlat/solve) and the tracker (mlat/track)
// behind the ingest grouper, routes the results to the global tracker and
// to the contributing feeders, and runs the hold-out self-validation and
// the quality statistics (mlat/quality).
//
// # Data flow
//
//	ingest grouper ──Observe(group)──► Engine ──► worker (by ICAO hash)
//	                                               │ decode once (modes)
//	                                               │ ref.Store.Observe      (ADS-B, transmit instant τ)
//	                                               │ planner.ObserveGroup   (hearers, ADS-B state)
//	                                               │ clock filter AddSync   (reference aircraft, see "Sync")
//	                                               │ ADS-B verification     (§9.6)
//	                                               └ group.Assembler ──Cluster──► solve ─► track ─► publish
//	Advance (100 ms): clock Step (1 s behind) · planner Replan (5 s) · 1 Hz tick:
//	                  track Sweep, coasting publications, results to feeders · expiries
//
// The engine implements ingest.MLATSink, ingest.LateObserver and
// ingest.ClockObserver; ClockView gives the ingest its ingest.ClockView.
//
// # Concurrency (DESIGN §20: 1,000+ feeders, 10k+ aircraft)
//
// Aircraft are partitioned over Workers goroutines by a multiplicative
// hash of the address. A worker owns its group.Assembler, solve.Solver and
// track.Tracker (none of them is safe for concurrent use) and its
// per-aircraft state, so nothing on the per-cluster path is shared or
// locked. Observe copies the group into a pooled job and queues it to the
// owning worker without blocking; a full queue drops the group and counts
// it (Stats.Dropped): backpressure never reaches the ingest's sweeper.
// Late deliveries go to the same worker as the original (same address),
// so the assembler's late-copy store stays consistent.
//
// Shared components are concurrency-safe by construction: the clock
// snapshot is read lock-free (atomic pointer; a worker pins one snapshot
// per group, so the assembler and the solver of a group see the same clock
// states), the reference store and the planner are sharded by address, the
// quality statistics lock briefly per sample. Advance drives everything
// time-based from one goroutine, so a test driver with a virtual clock
// reproduces production behaviour exactly (Workers < 0 processes
// synchronously inside Observe).
//
// # Time bases
//
// The clock filter works in network seconds since Options.OriginUnixNs
// (float64: a sub-picosecond resolution over the whole uptime). Network
// times are carried as group.Time (whole Unix ns + fraction), so no
// precision is lost however long the server runs. For one cluster the
// epoch is the reference site's whole nanosecond t_r; the solver and the
// tracker get
//
//	TOA_a = (T_a − t_r) seconds        (|TOA_a| < 10 ms: resolution ~1e-18 s)
//
// A component with GPS anchors has UTC network time. An anchor-less
// (relative-time) component's network time is offset from UTC by the
// clocks' common mode, initialised from host times (tens of ms for SDRs).
// Per time domain d the worker learns
//
//	o_d ← o_d + 0.02·(FirstNs − t_first − o_d)
//
// (server corrected time of the first copy minus the earliest network
// arrival time). Tracks, published times and reference look-ups of a
// relative domain are shifted by o_d, so an aircraft keeps one time base
// when its clusters change domain, and published times are approximately
// UTC; such fixes carry model.FixFlagRelativeTime. The solver's host-time
// check (DESIGN §9.4: τ within 50 ms of the first arrival) uses
// FirstNs − o_d for every domain: the ingest's corrected times carry the
// feeders' receive latency (≈ 50 ms for SDRs that deliver samples in
// blocks), which made every anchored fix fail the check before.
//
// # Covariance of the TOAs (DESIGN §9.4 weighting)
//
// A cluster's sites are single stations or co-located stations fused by
// the assembler (weights w_i, Σw = 1 per site). The clock snapshot gives
// the m×m covariance C of the members' clock-state errors
// (clock.Snapshot.Convert: θ, φ, ψ, β_short blocks and cross terms of the
// component, process noise since the epoch). The site covariance is
//
//	S_ab = Σ_{i∈a} Σ_{j∈b} w_i w_j C_ij      (a ≠ b; jitter is independent between stations)
//	S_aa = Var(site a)                         (clock, jitter, fusion geometry bound)
//
// with |S_ab| clipped to 0.98·sqrt(S_aa S_bb) to keep S positive
// definite. The solver uses S (s²). The tracker models the clock part as
// a correlated (Gauss-Markov) error and must not see it twice
// (mlat/track WIRING): it gets TOA.Sigma² = J_a = Σ w_i² jitter_i and
// TOA.SyncSigma² = S_aa − J_a. At most MaxSolveSites (16) sites are used,
// the lowest-variance ones.
//
// # Altitude row
//
// Mode S pressure altitude → height above the ellipsoid through the
// reference store (the aircraft's own GNSS−baro difference when it is an
// ADS-B aircraft, else the regional field, else ISA + EGM96), with the
// quantisation (25 ft or 100 ft Gillham, from the frame) and, for an
// altitude taken from an earlier frame (group.AltHint), the age × vertical
// rate inflation (|v_z| of the track + 1 m/s, 10 m/s without a track):
// solve.AltitudeFromHAE. ES GNSS heights (TC 20-22) are used directly with
// σ 15 m.
//
// # Solver and tracker routing
//
// A cluster with Use >= UseSolveAlt (the planner admitted the solve) is
// solved; the fix goes to the tracker only while the aircraft has no
// confirmed track. A confirmed track is updated with the raw TOA cluster
// (m−1 TDOA rows + altitude), never with both the cluster and the fix of
// the same transmission (that would count the measurement twice). The fix
// of a tracked aircraft still feeds the quality statistics and is an
// independent check of the track (below). A late re-emission of a cluster
// (group.Cluster.Late) is used only when the original was not applied.
//
// # Verification of tracks
//
// A minimal 3 + altitude solution (DOF 0) has two branches that the
// measurement cannot tell apart; on simnet aircraft 300 km outside a
// small network were confirmed on the near (wrong) branch and published
// with σ 20 m and 290 km of error. The engine therefore:
//
//   - publishes a confirmed track only once it has accepted redundant
//     position information: Σ DOF >= 2 over its solver fixes (unambiguous,
//     consistent with the track); minimal fixes count 0, and a DOF-1 fix
//     counts only when its station set differs from the DOF-1 fixes
//     already credited (worker.redundancy: the near/far branch ambiguity
//     of a 4-station or 4+alt problem belongs to its station geometry, so
//     repeating it verifies nothing);
//
//   - uses a track as solver prior and assembler hint only once verified;
//
//   - tests every redundant, unambiguous fix of a confirmed track against
//     the track predicted to the fix time,
//
//     q = dᵀ (P_fix + P_track)⁻¹ d,   d = p_fix − p_track
//
//     and when q > 21.1 (χ²₃ at 1e-4) with |d| > 1 km for fixes with
//     DOF >= 2 worth 2 DOF in a row, drops the track and restarts it from
//     the fix. A contradicting DOF-1 fix restarts nothing: far outside the
//     stations it is itself often the wrong branch;
//
//   - checks every redundant cluster (DOF >= 1) a confirmed track gated
//     away (worker.checkGated): it is solved even when the planner did not
//     admit it, its fix enters the contradiction test above, and when the
//     EWMA of gate failures among the track's redundant clusters exceeds
//     0.5 (after >= 6 of them) the track is restarted.
//
// The run-away hold-out tracks of the Wave 2B live run (hold-out track P95
// 4.5 km against a fix P95 of 188 m) had exactly these causes. Reproduced
// on the live-like netsim harness (TestHoldoutTrackNetsim with
// MLAT_HTRACK_LIVE: 12 AVF stations, 3 map-only Beast links, no GPS status
// so relative time throughout): far aircraft heard by three stations were
// solved as ambiguous 3+alt fixes; the far, true branch's tracker
// hypothesis got the near branch's covariance (ten times too small),
// gated its own fixes and was dropped for σ_h > 5 km at the 1 Hz sweep; two
// DOF-1 wrong-branch fixes minutes apart then "contradicted" and replaced a
// right track (0.1 km off) by a wrong one 133 km off, which redundant
// clusters kept contradicting unseen because the planner had not admitted
// them for solving. Fixed together with mlat/solve (alternates carry their
// own covariance) and mlat/track (tentative hypotheses are not dropped for
// their σ): hold-out track P95 vs truth 166 km → 392 m (fix P95 272 m).
//
// # Publication and results (DESIGN §9.5, §8.4)
//
// Confirmed, verified tracks are published as model.DecodedUpdate{MLAT:
// true, Src: mlat-av} at most every PublishInterval (500 ms) on update,
// and as their prediction once a second while coasting (no measurement for
// 1.5 s of server time) for up to 5 s; grade D (σ_h > 500 m) is never
// displayed. The two rate limits run on their own time bases (measurement
// updates on event time, the group's corrected antenna time; coasting on
// the tick's server time, which leads it by the network and grouping
// latency): with one shared stamp every coasting publication blocked the
// next second of measurement updates in the live system (hold-out tracks
// scored 4 times in 16 minutes, the map showing predictions). Every publication also goes
// to Fixes(). Feeders get an aircraft's latest result once a second, and
// only the stations that contributed to its track in the last 60 s and
// delivered >= 10 timed frames per minute while synced (the
// minimum-contribution rule); results are never re-forwarded (MLAT = true).
//
// # Hold-out self-validation (DESIGN §9.6)
//
// quality.HoldOut selects Holdout (10 %) of the ADS-B aircraft that
// announce NACp >= 9, by a keyed hash rotated every 10 minutes. A hold-out
// aircraft is hidden from the clock filter's reference source, is an MLAT
// candidate of the planner, and is solved as a Mode S-only aircraft: no
// ADS-B hint or prior, no TC 19 velocity, no ES GNSS height, and its
// pressure altitude goes through the regional field only. Its snapshot
// fixes and its track estimates (on measurement updates, at most 1 Hz;
// never published) are compared with the reference position at the same
// instant (ref.Store.At): horizontal error, along/cross-track split,
// σ-coverage and NEES against the sum of both covariances, per 1° cell ×
// altitude band. The reference still carries the airframe's unknown
// position latency (0-300 ms on simnet, σ 150 ms in the store) unless
// LatencyFeedback is on, which feeds hold-out fixes back into the latency
// estimate and makes the statistic optimistic; the cross-track error is
// insensitive to it. The reports therefore also carry the horizontal
// percentiles estimated from the cross-track errors alone
// (EstimateFromCross: the MLAT error's orientation follows the station
// geometry, not the heading, so it is isotropic relative to the track and
// the half-normal → Rayleigh quantile ratios apply): Quality.FixEst /
// TrackEst and PublicQuality.EstP50M / EstP95M, the honest headline
// figures; LatencyFeedback stays off.
//
// # Reproducibility
//
// With Workers < 0 (synchronous) a run is bit-reproducible: no map
// iteration order reaches the results (the tick, the tracker's Sweep, the
// result router and the planner's removals and idle-station deltas run in
// address / station order). TestReproducible publishes the same fixes in
// the same order twice (12 simulated minutes, bit for bit). Asynchronous
// workers keep per-aircraft order but not the interleaving across
// aircraft.
//
// # Survey and owner notification (phase 3 seam)
//
// SurveySink (implemented by *Engine) is what internal/mlat/survey and
// the owner API call: SetSurvey for reverse-MLAT results, OverrideSurvey
// for the owner's decision on their automatic use (a standing rejection
// of the station, lifted by OverrideSurvey(st, false)). Every change of a
// station's effective position source reaches OnPosition and the
// PositionNotifier (PositionChange: old/new source, position, σ,
// AutoSurvey, reason) for the owner notice of DESIGN §19.3.
//
// # Sync (DESIGN §9.2.2)
//
// Long frames (DF17/18) of reference aircraft go to the clock filter as
// they are. Short frames (DF0/4/5/11) repeat with identical payloads
// (interrogation pairs 0.74 ms apart share one ingest group), so for them
// the engine keeps only synced stations whose emission-time estimates
// e_i = t_net,i − ρ_i(p_ref)/c fall into the densest 20 µs window, and
// only in components with GPS anchors: in an anchor-less component the
// common mode of β_short is unobservable and short-frame sync drove
// SDR-only simnet networks into cascades of false steps. Sync observations
// are held back for SyncWarmup (60 s) after start, while the reference
// store confirms its first tracks.
//
// # Ingest interface
//
// ClockView switches off the ingest's network-time gate and its
// single-copy flush of sync candidates (see ClockView: both remove exactly
// the observations the clock filter needs, measured on simnet).
//
// # Quality learning
//
// Per station (single-station sites of fixes with DOF >= 2): the robust
// scale s of the Baarda w-tests (median |w| / 0.6745) gives the jitter
// that explains it, J' = s²(J + C) − C, clamped to ×0.5…×4 of the class
// prior; the residual bias (EWMA, ns); the RAIM exclusion and outlier
// (|w| > 4) shares; a bias flag after six consecutive 10-minute bins with
// a significant (|w̄|√n > 3) and relevant (|w̄| >= 0.5) mean, which asks
// for a survey (SurveyRequester). The learned jitter is fed back to the
// clock filter only with JitterFeedback (the filter learns its own from
// sync innovations). k_atm is regressed hourly from anchored fixes
// (quality.KAtm; advisory unless ApplyKAtm).
//
// # ADS-B verification (DESIGN §9.6)
//
// Every 5 s per aircraft an ADS-B position is checked against the TDOAs of
// >= 3 synced stations (quality.VerifyTDOA): contradicted when χ² exceeds
// its 1e-4 quantile and the largest TDOA error exceeds 300 m. Three
// contradictions within a minute make the aircraft suspect: the planner
// solves it, and — only on UTC (anchored) evidence — the reference store
// stops offering it for sync.
//
// # Mode A/C (DESIGN §9.9, phase 5, aggregator.mlat.modeAc, off)
//
// Mode A/C replies carry no address and never reach the Mode S MLAT path.
// The links hand them to ObserveModeAC / ObserveModeACFrames
// (ModeACReplyFromFrame: 2-byte payload in the pulse layout or as octal
// nibbles, Options.ModeACCodeFormat). Each reply is converted to network
// time with the current clock snapshot (unsynced stations dropped and
// counted), its variance is the clock's σ_θ² + jitter² + ModeACJitterNs²
// (50 ns: no preamble to correlate on, slow framing pulse), and it is held
// ModeACHoldNs (600 ms) for host latency. Advance then sorts the held
// replies and hands those older than the hold to group.ModeAC, which
// associates them by code and time among the planner's Mode A/C stations
// (anchors with anchor peers within 150 km; maximum cliques of the pairwise
// gate |Δt| ≤ d/c + slack + nσ). Clusters of ≥ 4 stations are solved by the
// ordinary solver, seeded by the prediction of a shown target with the same
// code (σ_h < 2 km); prior-fused fixes are rejected so that a seed cannot
// make a fix. Fixes and 2-3 station clusters go to track.ACTracker
// (synthetic identities, code bookkeeping, confirmation over several
// bursts, merging, crowding; track/doc.go §13). Once a second the shown
// targets of grade A-C are published as ModeACTarget records
// (ModeACTargets; ModeACAircraft gives model.Aircraft with ICAO "#xxxxxx",
// AddrType "modeac", Source mlat). Station identities never appear in
// them. Every accepted fix also goes to the EnableModeAC callback.
// ModeACStats has the admin counters. The planner tells Options.ModeACOut
// (plan.MLATCModeAC) which stations to ask for Mode A/C replies.
//
// Measured (TestModeACNetsim, 8 GPS anchors / 150 km, Mode A/C-only
// aircraft generated per radar sweep, truth comparison; WIRING.md §6):
// 40 aircraft, 4 radars, 75 % reception, +50 ns, 100 FRUIT/s: availability
// 100 %, P50 17 m, P95 58 m, no false targets. Hard variant (80 aircraft,
// half on 7000, 2 radars, 50 % reception, +80 ns, 500 FRUIT/s):
// availability 94.2 %, P50 62 m, P95 250 m, 1 false target.
//
// # Measured (simnet, real ingest, i9-13900K; tests gates2_test.go,
// throughput_test.go; see WIRING.md for the full tables)
//
// DESIGN §13.2 rows 1-3, published positions inside the network radius,
// last 4 of 16 simulated minutes:
//
//	8 GPS / 150 km:      P50 5.5 m  P95 17.9 m  within 1σ 63.1 %     (gate P50 < 20, P95 < 60, 60-75 %)
//	4 GPS + 20 SDR:      P50 11.6 m P95 107 m; Mode S-only in the
//	                     4-station footprint with fixes 61/62        (gate < 80, < 250, >= 90 %)
//	20 SDR:              P50 23-33 m on 6 of 8 seeds; 2 seeds hit a
//	                     clock-filter instability (WIRING.md)        (gate P50 < 200)
package mlat
