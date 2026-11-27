package analytics

// # Locate Me v2 (DESIGN §10.4)
//
// Locate Me v2 estimates where a station stands from what it receives,
// without peers: for standalone stations (the guess game, scoring against
// the receiver's own GNSS position) and for aggregator stations that have
// no location yet, before the reverse-MLAT survey converges.
//
// ## Files
//
//	locate2.go          Locate2: data preparation, grid search, likelihoods,
//	                    posterior moments, calibration, cost surface
//	locate2_skyline.go  per-sector skyline fit of the endpoint term
//	locate2_terrain.go  DEM grid of the search area and node panoramas
//	locate2_timing.go   L2TimingCollector: T=1 timing tracks from latency.Obs
//	locate2_store.go    Analyzer.LocateMe, LocateSources, GuessRecorder
//	locate2_hook.go     GuessRecorder.TrackerHook (tracker.SetGuessSampleHook)
//	guess.go            the classic four-method guess; GuessLocation merges v2
//	../store/guess_samples.go  the bounded table guess_samples
//
// ## Data
//
// guess_samples (store): one row per ADS-B position received off-air, at
// most one per aircraft per 10 s (the tracker throttles), with t, icao,
// lat, lon, baro altitude, signal level, RF channel and the surface flag.
// 7 days and 2 M rows per station by default (GuessRecorder prunes every
// 10 minutes). LocateMe reads the last 7 days of the Analyzer's station,
// thinned deterministically to 400 000 samples (whole 10 s buckets of an
// aircraft are kept or dropped). A standalone station without enough
// guess samples (fresh upgrade) falls back to its stored ADS-B trails
// (positions table: no surface flag, coarser session ends).
//
// ## Model
//
// The posterior over the station position is a product of tempered
// likelihoods (temperatures absorb the correlation of samples within one
// pass, one airway, one hour of weather), profiled over the antenna height
// (a few discrete heights with prior penalties: above ground with a DEM,
// MSL without one):
//
//   - horizon: every heard aircraft must be above the radio horizon of the
//     candidate (smooth earth with k = 4/3), or with a DEM above the terrain
//     panorama (120 rays to 30 km, the lowest of a ray and its neighbours)
//     and beyond 30 km above the smooth horizon over the lowest terrain at
//     the end of the ray. Violations beyond a free margin (0.2° plus
//     200 m of altitude error seen from the aircraft's distance) cost a
//     capped quadratic penalty (diffraction, ducting and odd altitudes must
//     not dominate). Surface positions (heights from the DEM, else from the
//     lowest nearby airborne altitude) are line-of-sight discs.
//   - endpoints: low aircraft (< 25 000 ft) that appear or vanish in mid-air
//     (not at the ground, not around receiver outages) do so at the skyline.
//     The likelihood ratio of "lost at the skyline, N(0, 0.35°)" against
//     "lost for another reason", whose density is the margin density of all
//     heard low aircraft at that candidate, makes ends informative only
//     where they cluster at the horizon more than the rest of the traffic
//     (seen from far away everything low is near the horizon). The skyline
//     of each 10° sector is profiled in [-0.5°, +3°] around the model
//     horizon, with a prior cost for leaving it and a penalty for heard
//     traffic below the raised skyline: hills, buildings and trees that the
//     model does not know then do not drag the estimate away
//     (locate2_skyline.go).
//   - signal: within each pass (aircraft session) the level follows
//     -20·log10(slant range) plus a low-order antenna gain
//     G(el, az) = a·10·log10(cos² el) + b·cos az + c·sin az; per-pass
//     transmitter offsets and the noise level are profiled out; samples
//     < 120 km, the dominant channel, levels between noise floor + 6 dB and
//     -2 dBFS (no saturation).
//   - timing (optional): T=1 airborne positions of a GPS-stamping receiver
//     (Radarcape). Arrival minus the 0.2 s UTC epoch is latency plus
//     propagation; tracks whose latency is stable to microseconds (tested
//     per track, l2UsableTrack) give range differences along the track.
//     Most transmitters jitter (internal/latency measures them) and are
//     ignored; standalone timing-only location is not attempted.
//
// Search: a 200 km circle at 8 km (terrain on a 1 km DEM), then boxes
// around the posterior mass at 2 km, 500 m and 200 m (250 m DEM). The
// estimate is the posterior mean; the ellipse comes from the second
// moments of the finest level that holds the mass. The 95 % circle is
// sqrt(5.991·λ_max) after calibration: the spread is widened where the
// model is known to be incomplete (l2Inflation: ×1.5 with signal and DEM,
// ×1.5 signal without DEM, ×3 DEM without signal, ×1.5 neither), times
// 1 + 14·SkylineRaised (the share of directions whose skyline the fit had
// to move: 0 on smooth earth, ~0.45 in hills without a DEM), plus a model
// floor of 2.0 km σ per axis (l2Calibrate, l2ModelFloorKm), so the 95 %
// circle is never smaller than ~4.9 km.
//
// r101 recalibration: until then the floor was 0.5 km with a DEM (1.6 km
// without) and the DEM+signal factor 1. A real station (DEM, signal levels)
// then reported σ 0.56 km / r95 1.3 km while 4.8 km off (8.6 σ): the
// synthetic worlds draw their terrain and link budget from the estimator's
// own models and cannot show what a real site adds (buildings, trees, DEM
// errors, antenna mounting, correlated samples of the same airways). The
// tables below predate the change; with it the error distribution is the
// same and the circles are wider.
//
// ## Evaluation (2026-11-14, development PC: 32 threads)
//
// Synthetic worlds (locate2_world_test.go; built from other code than the
// estimator: terrain.Panorama visibility, hourly refraction k 1.2..1.55,
// soft horizon, per-hour QNH errors of ±80 m (1 σ), link budget with
// per-aircraft power, elevation lobing, sector shadowing, AGC compression,
// fading, decoding per message; overflights, departures/arrivals with taxi,
// GA), TestLocate2Eval with AIRVIEW_LOCATE2_EVAL=20, seeds 1000..1019 (a 30-site run
// before the final calibration gave a similar error distribution):
//
//	configuration                 P50 km  P90 km  max km  inside r95
//	smooth earth, 24 h, 100 flights/h        0.70    1.48    1.83   20/20
//	smooth earth, 6 h                        0.68    1.58    2.40   20/20
//	smooth earth, 24 h, 30 flights/h         0.69    1.13    1.73   20/20
//	hills, DEM used                          0.42    1.34    2.89   19/20
//	hills, no DEM                            1.65    3.68    4.78   20/20
//	smooth earth, no signal levels          12.15   27.08   35.11   19/20
//	hills, no DEM, no signal levels         13.59   26.90   53.33   17/20
//	hills, DEM, no signal levels             0.68    5.22   45.20   19/20
//
// internal/sim/simnet (independent simulation, station 40-80 km off the
// world centre, 300 aircraft, 3 h, no DEM), TestLocate2Simnet with
// AIRVIEW_LOCATE2_SIMNET=10: errors 0.15, 0.32, 0.88, 1.01, 1.64, 1.81,
// 2.66, 3.65, 3.71, 3.75 km (P50 1.8 km, max 3.8 km), truth inside the 95 %
// circle (4.0-6.4 km) in 10 of 10.
//
// Typical error is well under the 10 km acceptance limit whenever signal
// levels are available (every Beast / Radarcape feed). Without signal
// levels (SBS-only feeds) the horizon and skyline alone give ~5-25 km and
// a correspondingly wide circle.
//
// Live check: AirView -demo on port 18722 (simulated traffic, no
// guess_samples hook wired yet, so the positions fallback), after about an
// hour of trails (13 589 samples of 106 aircraft): v2 2.6 km off the
// configured position with a 6.5 km accuracy circle, in 1.1 s; the classic
// methods 11.6 (floor), 20.3 (centroid), 25.7 (symmetry) and 43.3 km
// (signal). After 12 minutes (46 aircraft) v2 was still vague (σ 134 km,
// 46 km off); since then GuessLocation lets v2 replace the classic fusion
// only when its σ is the smaller one.
//
// Run time (BenchmarkLocate2, 32 threads): 1.8 s for a busy 24 h site,
// 1.9 s with a DEM; runs without signal levels take up to 5 s. A
// Raspberry Pi 4 (4 cores) should take 10-20 times longer (estimated, not
// measured). LocateMe caches the result for 15 minutes and gives up after
// 90 s.
//
// ## Wiring
//
// Standalone (engine): install the recorder and the sources once.
//
//	rec := analytics.NewGuessRecorder(st, analytics.GuessRecorderOptions{})
//	tr.SetGuessSampleHook(rec.TrackerHook())
//	defer rec.Close()
//	col := &analytics.L2TimingCollector{} // col.Observe(&latencyObs) per frame
//	analytics.SetLocateSources(analytics.LocateSources{
//	    Terrain: terrainProvider,            // cached tiles only
//	    GNSS:    receiverGNSS,               // scoring only, never an input
//	    Timing:  col.Tracks,
//	})
//
// GuessLocation (/api/guess) then merges the v2 estimate: an extra estimate
// "locate2", and when it succeeds the fused estimate is the v2 one with
// R95Km (the accuracy circle) and Guess.Locate2 (terms, levels, ErrorKm
// against the GNSS or configured position). The classic estimates keep
// their place in the list with weight 0, for comparison.
//
// Aggregator: one recorder on stations.db with Want = "station has no
// location yet", the same tracker hook; per station
// stationAnalyzer.LocateMe(ctx, analytics.LocateRequest{Ref: survey}).
// Station results are owner/admin data (§8.2) and never public; the host's
// GNSS and timing are never applied to a station.
//
// Configuration (§20.3, requested from WP-CF): locate.enabled,
// locate.retentionDays, locate.maxRowsPerStation, locate.maxRowsTotal and
// the AIRVIEW_LOCATE_* overrides map to GuessRecorderOptions.
